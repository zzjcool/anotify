package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
)

// 能力领域（v4，capability.md §2/§3/§8）。
//
// 三条不变量（本文件是它们的唯一落点）：
//
//	① 两级内容身份：foldKey 判「是不是同一行」，digest 是部署身份（含 version）。
//	② 改 next / displayName **不产生新版本**：同一行原地更新（next 同时重建 edges
//	   并写一条 capability.routing_changed 事件），digest 不变，不重推节点。
//	③ 显式 version 的内容不可变：同 (ns,name,version) 的执行正文不同 → 409 语义的
//	   ErrVersionConflict（稳定码 version_content_immutable）。
//
// 执行正文 = spec.params/requires/lifecycle/concurrency/run；next 与 displayName
// 是既定例外，可原地变化（它们不进 digest）。

// ErrCapabilityInvalid 表示能力的正文（spec）不合法（客户端可修 → HTTP 400）。
//
// 形状规则只有一份（artifact.Validate）；这里只把它归成一个 store 层的哨兵错误。
var ErrCapabilityInvalid = errors.New("capability_invalid")

// ErrVersionConflict 表示 (namespace, name, version) 已经存在，但执行正文**不同**。
//
// 版本行不可变：同一个 name@version 不许换内容覆盖，也不会被静默改写成别的版本号。
// 判定用 artifact.SameContent（digest 相等）；next / displayName 是既定例外。
var ErrVersionConflict = errors.New("store: capability version already exists with different content")

// CodeVersionContentImmutable 是 ErrVersionConflict 的稳定机器码
// （capability.md §6.1 / §9.2：server 直接把它写进 Status.reason）。
const CodeVersionContentImmutable = artifact.CodeVersionContentImmutable

// VersionConflictError 给 server 一个稳定的 Code()（映射 version_content_immutable），
// 同时 errors.Is(err, ErrVersionConflict) 仍然成立。
type VersionConflictError struct {
	NamespaceID string
	Name        string
	Version     int
}

func (e *VersionConflictError) Error() string {
	if e == nil {
		return "store: capability version conflict"
	}
	return fmt.Sprintf("%s: %s@%d（版本行不可变；改内容请开新版本）",
		ErrVersionConflict.Error(), e.Name, e.Version)
}

// Unwrap 让 errors.Is(err, ErrVersionConflict) 成立。
func (e *VersionConflictError) Unwrap() error { return ErrVersionConflict }

// Code 返回稳定机器码（server 直接用作 Status.reason）。
func (e *VersionConflictError) Code() string { return CodeVersionContentImmutable }

// CapabilityVersion 是 capability_versions 表的一行：一个 name@version 的定义。
//
// Spec 是正文（params / next / run / requires / lifecycle / concurrency）。
// 其余字段是身份与记账：Digest（含 version 的部署身份）、FoldKey（不含 version
// 的折叠键）、InstalledFrom（首次安装来源）、ResourceVersion（写并发令牌）。
type CapabilityVersion struct {
	ID          string `json:"id"`
	NamespaceID string `json:"namespaceId"`
	Name        string `json:"name"`
	// DisplayName 是纯展示标签：不参与 digest / foldKey，改它不产生新版本。
	DisplayName string `json:"displayName"`
	Version     int    `json:"version"`
	// Digest 是部署身份（`sha256:<hex>`，含 version）。
	Digest string `json:"digest"`
	// FoldKey 是折叠键（`sha256:<hex>`，不含 version / next / displayName）。
	FoldKey string `json:"foldKey"`
	// InstalledFrom 是首次安装来源（安装回执 id；空 = 手建，复用不覆盖）。
	InstalledFrom   string `json:"installedFrom,omitempty"`
	ResourceVersion int64  `json:"resourceVersion"`
	CreatedAt       int64  `json:"createdAt"`
	// Spec 是正文。不进 JSON 响应（列表/详情形状由端点决定）。
	Spec artifact.Spec `json:"-"`
}

// Artifact 把行拼成一份完整产物：metadata 来自列，正文来自 Spec 字段。
//
// 「行 → 产物」只有这一处拼装：校验、digest、读取时拼产物都走它。多拼一份的后果是
// 某天有人少填一个字段，digest 与正文悄悄不一致（症状是部署永远被判成 stale）。
func (c *CapabilityVersion) Artifact() artifact.Artifact {
	if c == nil {
		return artifact.Artifact{}
	}
	return artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata: artifact.Metadata{
			Name:        c.Name,
			Version:     c.Version,
			DisplayName: c.DisplayName,
		},
		Spec: c.Spec,
	}
}

// capabilityColumns 是统一的列顺序，scanCapabilityRow 依赖它。
const capabilityColumns = `id, namespace_id, name, display_name, version, spec, digest,
	fold_key, installed_from, resource_version, created_at`

// normalizeCapability 校验并归一正文，返回归一后的产物（默认值已补齐）。
//
// 归一化是「省略 ≡ 显式默认」的实现：Normalize 补齐 maxAttempts / resultTtlSec /
// maxPerNode / params / next / requires 的空缺，所以同一份语义只有一份规范字节。
// 校验失败是客户端可修的错（ErrCapabilityInvalid → 400）。
func normalizeCapability(c *CapabilityVersion) (artifact.Artifact, error) {
	if c == nil {
		return artifact.Artifact{}, fmt.Errorf("%w: 能力为空", ErrCapabilityInvalid)
	}
	a := c.Artifact()
	norm, err := artifact.Normalize(&a)
	if err != nil {
		return artifact.Artifact{}, fmt.Errorf("%w: %s", ErrCapabilityInvalid, err.Error())
	}
	if verr := artifact.Validate(norm); verr != nil {
		return artifact.Artifact{}, fmt.Errorf("%w: %s", ErrCapabilityInvalid, verr.Error())
	}
	return *norm, nil
}

// marshalCapabilitySpec 把（已归一的）正文序列成落库字节。
//
// 规范字节只来自 json.Marshal(artifact.Spec)：struct 字段顺序固定、map 键有序、
// params 已归一。同一份语义因此永远得到同一串字节（foldKey 的字节相等比较依赖它）。
func marshalCapabilitySpec(spec artifact.Spec) ([]byte, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal capability spec: %w", err)
	}
	return raw, nil
}

// CreateCapability 写入一个能力版本（内容寻址或显式版本）。
//
//   - 省略 version（<=0）：先按 foldKey 在 (ns, name) 内找同内容行；有则复用
//     （创建复用不修改已存在资源），没有才分配 max+1。
//   - 显式 version：已存在且 SameContent → 只读复用；
//     执行正文不同 → *VersionConflictError（errors.Is(err, ErrVersionConflict)）。
func (d *DB) CreateCapability(ctx context.Context, c *CapabilityVersion) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create capability: begin tx: %w", err)
	}
	// 早退 / panic 都回滚：留下半条数据是不可撤的。
	defer func() { _ = tx.Rollback() }()
	if err := createCapabilityTx(ctx, txExec{tx: tx}, c); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create capability: commit: %w", err)
	}
	return nil
}

// createCapabilityTx 是建能力的**唯一**实现（CreateCapability 只是给它开事务）。
//
// ex 决定「读走哪里」：事务外传 *DB，事务内传 txExec。
func createCapabilityTx(ctx context.Context, ex queryExec, c *CapabilityVersion) error {
	if c == nil {
		return fmt.Errorf("create capability: nil")
	}
	if c.NamespaceID == "" || c.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("create capability: empty name")
	}

	// ① 显式版本先把版本号放进产物（digest 吃 version）；内容寻址的版本号在下面定。
	explicit := c.Version > 0
	if explicit {
		norm, err := normalizeCapability(c)
		if err != nil {
			return err
		}
		c.Spec = norm.Spec
		existing, gerr := getCapabilityOn(ctx, ex, c.NamespaceID, c.Name, c.Version)
		switch {
		case gerr == nil:
			return reuseCapabilityOn(ctx, ex, c, existing)
		case !errors.Is(gerr, ErrNotFound):
			return fmt.Errorf("lookup explicit capability version: %w", gerr)
		}
		return insertCapabilityOn(ctx, ex, c, &norm)
	}

	// ② 省略 version = 内容寻址：先算 foldKey（不含 version），再找同内容行。
	norm, err := normalizeCapability(c)
	if err != nil {
		return err
	}
	foldDoc := artifact.Artifact{Metadata: artifact.Metadata{Name: c.Name}, Spec: norm.Spec}
	foldKey, err := artifact.FoldKey(&foldDoc)
	if err != nil {
		return fmt.Errorf("fold key for %s: %w", c.Name, err)
	}
	existing, gerr := getCapabilityByFoldKeyOn(ctx, ex, c.NamespaceID, c.Name, foldKey)
	switch {
	case gerr == nil:
		return reuseCapabilityOn(ctx, ex, c, existing)
	case !errors.Is(gerr, ErrNotFound):
		return fmt.Errorf("lookup capability by fold key: %w", gerr)
	}

	// 没有同内容行 → 分配下一个版本号，再用含 version 的 digest 作为部署身份。
	next, nerr := nextCapabilityVersionOn(ctx, ex, c.NamespaceID, c.Name)
	if nerr != nil {
		return nerr
	}
	c.Version = next
	norm.Metadata.Version = next
	return insertCapabilityOn(ctx, ex, c, &norm)
}

// insertCapabilityOn 落一行能力版本（调用方已确定版本号与归一正文）。
//
// 唯一冲突只可能来自并发写同一 (name, version) 或同一 digest：
// 回查后按「同内容折叠 / 异内容冲突」判定，绝不静默覆盖。
func insertCapabilityOn(ctx context.Context, ex queryExec, c *CapabilityVersion, norm *artifact.Artifact) error {
	digest, err := artifact.Digest(norm)
	if err != nil {
		return fmt.Errorf("digest capability %s@%d: %w", c.Name, c.Version, err)
	}
	foldKey, err := artifact.FoldKey(norm)
	if err != nil {
		return fmt.Errorf("fold key for %s@%d: %w", c.Name, c.Version, err)
	}
	specJSON, err := marshalCapabilitySpec(norm.Spec)
	if err != nil {
		return err
	}
	c.Spec = norm.Spec
	c.Digest = digest
	c.FoldKey = foldKey
	if c.CreatedAt == 0 {
		c.CreatedAt = Now()
	}
	if c.ResourceVersion == 0 {
		c.ResourceVersion = 1
	}
	if c.ID == "" {
		c.ID = NewCapabilityID(strings.TrimPrefix(digest, artifact.DigestPrefix))
	}

	_, ierr := ex.execContext(ctx,
		`INSERT INTO capability_versions
		   (id, namespace_id, name, display_name, version, spec, digest, fold_key,
		    installed_from, resource_version, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.NamespaceID, c.Name, c.DisplayName, c.Version, string(specJSON),
		digest, foldKey, c.InstalledFrom, c.ResourceVersion, c.CreatedAt)
	if ierr == nil {
		// 名字级 edges 只属于 live latest；同事务插入历史低版本不改投影。
		latest, err := getCapabilityLatestOn(ctx, ex, c.NamespaceID, c.Name)
		if err != nil {
			return fmt.Errorf("lookup latest capability after insert: %w", err)
		}
		if latest.ID == c.ID {
			return rebuildCapabilityEdgesOn(ctx, ex, c.NamespaceID, c.Name, norm.Spec.Next)
		}
		return nil
	}
	if !isUniqueConstraintError(ierr) {
		return fmt.Errorf("insert capability: %w", ierr)
	}

	// 并发冲突：先按 (name, version) 回查，再按 digest 回查。
	existing, gerr := getCapabilityOn(ctx, ex, c.NamespaceID, c.Name, c.Version)
	if gerr == nil {
		return reuseCapabilityOn(ctx, ex, c, existing)
	}
	if !errors.Is(gerr, ErrNotFound) {
		return fmt.Errorf("lookup capability after conflict: %w", gerr)
	}
	same, serr := getCapabilityByDigestOn(ctx, ex, c.NamespaceID, digest)
	if serr == nil {
		return reuseCapabilityOn(ctx, ex, c, same)
	}
	if !errors.Is(serr, ErrNotFound) {
		return fmt.Errorf("lookup capability by digest after conflict: %w", serr)
	}
	return fmt.Errorf("insert capability: %w", ierr)
}

// reuseCapabilityOn is create-only idempotence: existing routing and metadata never change.
// Mutable exceptions require an explicit CAS update. Content remains immutable.
func reuseCapabilityOn(ctx context.Context, ex queryExec, c, existing *CapabilityVersion) error {
	if existing == nil {
		return nil
	}
	incoming := artifact.Artifact{Metadata: artifact.Metadata{Name: c.Name, Version: existing.Version}, Spec: c.Spec}
	stored := existing.Artifact()
	sameContent, err := artifact.SameContent(&incoming, &stored)
	if err != nil {
		return fmt.Errorf("compare capability content: %w", err)
	}
	if !sameContent {
		return &VersionConflictError{NamespaceID: existing.NamespaceID, Name: existing.Name, Version: existing.Version}
	}

	*c = *existing
	return nil
}

// sameStringList 判定两个列表是否逐元素相同（nil 与 [] 视为相同）。
func sameStringList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// UpdateNext 原地替换某个能力版本的 spec.next（不产生新版本）。
//
// 同一事务内做三件事（capability.md §3「改 next 到底发生了什么」）：
//
//  1. 该行 spec.next 原地更新（digest 不变）；
//  2. 该能力的 edges 整体重建（DELETE + INSERT，无端口概念）；
//  3. 落一条 capability.routing_changed 事件（旧 next → 新 next）。
//
// version<=0 表示 latest。返回更新后的行。
func (d *DB) UpdateNext(ctx context.Context, namespaceID, name string, version int, next []string, expectedVersion string) (*CapabilityVersion, error) {
	return d.UpdateCapability(ctx, namespaceID, name, version, CapabilityPatch{Next: &next}, expectedVersion)
}

// rebuildCapabilityEdgesOn 整体替换一个能力名的出边（无端口，to_ref 原样保存）。
//
// from_ref 是**能力名**（不是 id / 不是带版本的引用）：边是 next 的投影，
// 「这个能力有哪些出边」只有一份事实。
func rebuildCapabilityEdgesOn(ctx context.Context, ex queryExec, namespaceID, name string, next []string) error {
	if _, err := ex.execContext(ctx,
		`DELETE FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ?`,
		namespaceID, name); err != nil {
		return fmt.Errorf("clear capability edges: %w", err)
	}
	now := Now()
	for _, toRef := range next {
		ref := strings.TrimSpace(toRef)
		if ref == "" {
			continue
		}
		if _, err := ex.execContext(ctx,
			`INSERT INTO edges (namespace_id, from_is_source, from_ref, to_ref, created_at)
			 VALUES (?,0,?,?,?)
			 ON CONFLICT(namespace_id, from_is_source, from_ref, to_ref) DO NOTHING`,
			namespaceID, name, ref, now); err != nil {
			return fmt.Errorf("insert capability edge %s -> %s: %w", name, ref, err)
		}
	}
	return nil
}

// EventTypeCapabilityRoutingChanged 是「改线」事件类型（capability.md §3）。
const EventTypeCapabilityRoutingChanged = "capability.routing_changed"

// eventSourceHub 是中枢自身产生的事件来源名（不是用户建的源）。
const eventSourceHub = "hub"

// EventReasonRoutingChanged 是改线事件的稳定原因码。
const EventReasonRoutingChanged = "RoutingChanged"

// insertRoutingChangedEventOn 落一条 capability.routing_changed 事件（旧 next → 新 next）。
//
// 路由事件不属于任何 trace：trace_id 用能力 id 作关联键（同一能力的改线历史可查）。
func insertRoutingChangedEventOn(ctx context.Context, ex queryExec, c *CapabilityVersion, from, to []string) error {
	payload, err := json.Marshal(map[string]any{
		"capability": c.Name,
		"version":    c.Version,
		"from":       append(make([]string, 0, len(from)), from...),
		"to":         append(make([]string, 0, len(to)), to...),
	})
	if err != nil {
		return fmt.Errorf("marshal routing changed payload: %w", err)
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, reason, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		NewEventID(), c.NamespaceID, c.ID, eventSourceHub,
		EventTypeCapabilityRoutingChanged, string(payload), EventReasonRoutingChanged, Now()); err != nil {
		return fmt.Errorf("insert routing changed event: %w", err)
	}
	return nil
}

// getCapabilityByFoldKeyOn 按折叠键找同内容行（事务内外共用一份 SQL）。
func getCapabilityByFoldKeyOn(ctx context.Context, ex queryExec, namespaceID, name, foldKey string) (*CapabilityVersion, error) {
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions
		 WHERE namespace_id = ? AND name = ? AND fold_key = ? AND deleted_at IS NULL ORDER BY version DESC LIMIT 1`,
		namespaceID, name, foldKey)
}

// nextCapabilityVersionOn 返回某 name 的下一个版本号（无定义时返回 1）。
//
// 写入路径必须在事务内算，否则两次并发创建会拿到同一个 next，后一个直接撞 UNIQUE。
func nextCapabilityVersionOn(ctx context.Context, ex queryExec, namespaceID, name string) (int, error) {
	var maxV sql.NullInt64
	if err := ex.queryRowContext(ctx,
		`SELECT MAX(version) FROM capability_versions WHERE namespace_id = ? AND name = ? AND deleted_at IS NULL`,
		namespaceID, name).Scan(&maxV); err != nil {
		return 0, fmt.Errorf("next capability version: %w", err)
	}
	if !maxV.Valid {
		return 1, nil
	}
	return int(maxV.Int64) + 1, nil
}

// getCapabilityByDigestOn 按 (namespace_id, digest) 取已有版本（事务内外共用）。
func getCapabilityByDigestOn(ctx context.Context, ex queryExec, namespaceID, digest string) (*CapabilityVersion, error) {
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions WHERE namespace_id = ? AND digest = ? AND deleted_at IS NULL`,
		namespaceID, digest)
}

// scanCapability 执行查询并解析一行；未命中返回 ErrNotFound（事务外）。
func (d *DB) scanCapability(ctx context.Context, query string, args ...any) (*CapabilityVersion, error) {
	return scanCapabilityOn(ctx, d, query, args...)
}

// scanCapabilityOn 是 scanCapability 的 ex 版：所有读都经 ex，
// 事务内传 txExec 才能看到本事务刚写入的行（见 txexec.go）。
func scanCapabilityOn(ctx context.Context, ex queryExec, query string, args ...any) (*CapabilityVersion, error) {
	var c CapabilityVersion
	err := scanCapabilityRow(ex.queryRowContext(ctx, query, args...), &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// scanCapabilityRow 把一行 capability_versions 解进 c（列顺序 = capabilityColumns）。
func scanCapabilityRow(row scanner, c *CapabilityVersion) error {
	var spec string
	if err := row.Scan(
		&c.ID, &c.NamespaceID, &c.Name, &c.DisplayName, &c.Version, &spec, &c.Digest,
		&c.FoldKey, &c.InstalledFrom, &c.ResourceVersion, &c.CreatedAt,
	); err != nil {
		return err
	}
	if strings.TrimSpace(spec) != "" {
		if err := json.Unmarshal([]byte(spec), &c.Spec); err != nil {
			return fmt.Errorf("decode capability spec: %w", err)
		}
	}
	// 空列表纪律：库里存的是 []，读出来也必须是 [] 而不是 null。
	if c.Spec.Next == nil {
		c.Spec.Next = []string{}
	}
	if c.Spec.Requires.Commands == nil {
		c.Spec.Requires.Commands = []string{}
	}
	if c.Spec.Requires.Values == nil {
		c.Spec.Requires.Values = []string{}
	}
	if len(c.Spec.Params) == 0 {
		c.Spec.Params = json.RawMessage(`{}`)
	}
	return nil
}

// GetCapability 按 name + version 取能力定义；未命中返回 ErrNotFound。
func (d *DB) GetCapability(ctx context.Context, namespaceID, name string, version int) (*CapabilityVersion, error) {
	return getCapabilityOn(ctx, d, namespaceID, name, version)
}

// getCapabilityOn 是 GetCapability 的 ex 版（事务内读必须走它）。
func getCapabilityOn(ctx context.Context, ex queryExec, namespaceID, name string, version int) (*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions
		 WHERE namespace_id = ? AND name = ? AND version = ? AND deleted_at IS NULL`, namespaceID, name, version)
}

// GetCapabilityByID 按 ID 取能力定义；未命中返回 ErrNotFound。
// 必须同时校验 namespace_id（命名空间隔离）。
func (d *DB) GetCapabilityByID(ctx context.Context, namespaceID, id string) (*CapabilityVersion, error) {
	return getCapabilityByIDOn(ctx, d, namespaceID, id)
}

// getCapabilityByIDOn 是 GetCapabilityByID 的 ex 版。
func getCapabilityByIDOn(ctx context.Context, ex queryExec, namespaceID, id string) (*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions WHERE namespace_id = ? AND id = ? AND deleted_at IS NULL`, namespaceID, id)
}

// SetCapabilityDisplayName 只更新 display_name（与 resource_version 令牌）。
//
// **不碰 digest / foldKey**：displayName 不参与内容身份，改它不产生新版本。
// This immutable-ID CAS delegates to the same mutable-exception transaction as UpdateCapability.
func (d *DB) SetCapabilityDisplayName(ctx context.Context, namespaceID, id, displayName string, expectedRV int64) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if expectedRV <= 0 {
		return ErrResourceVersionRequired
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set capability display name: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	cur, err := getCapabilityByIDOn(ctx, ex, namespaceID, id)
	if err != nil {
		return err
	}
	if err := checkResourceRevision(expectedRV, cur.ID, cur.ResourceVersion); err != nil {
		return err
	}
	if _, err := updateCapabilityOn(ctx, ex, cur, CapabilityPatch{DisplayName: &displayName}); err != nil {
		return err
	}
	return tx.Commit()
}

// getPinnedCapabilityByIDOn retains execution facts, even after logical deletion.
// Callers must already own a real Task/attempt/deployment reference to this UID.
func getPinnedCapabilityByIDOn(ctx context.Context, ex queryExec, namespaceID, id string) (*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return scanCapabilityOn(ctx, ex, "SELECT "+capabilityColumns+" FROM capability_versions WHERE namespace_id = ? AND id = ?", namespaceID, id)
}

// ListCapabilities 列出命名空间内全部能力定义（按 name、version 升序；空列表返回 []）。
func (d *DB) ListCapabilities(ctx context.Context, namespaceID string) ([]*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT `+capabilityColumns+` FROM capability_versions
		 WHERE namespace_id = ? AND deleted_at IS NULL ORDER BY name, version`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list capabilities: %w", err)
	}
	defer rows.Close()
	out := []*CapabilityVersion{}
	for rows.Next() {
		var c CapabilityVersion
		if err := scanCapabilityRow(rows, &c); err != nil {
			return nil, fmt.Errorf("scan capability row: %w", err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ListCapabilitiesPage 是列表读取的 keyset 分页版（#377）：排序是**对外顺序**
// name ASC、同名内 version DESC（与 handler 的 orderedCapabilityVersions 一致）。
//
// 不能用 ListCapabilities 的 (name ASC, version ASC) 翻页再在 handler 里倒序：
// 那样每页窗口和最终顺序对不上，页边界会同时丢行和重复行。`afterName == ""`
// 表示第一页；(namespace_id, name, version) 唯一，所以不需要额外的 id 破平局。
func (d *DB) ListCapabilitiesPage(ctx context.Context, namespaceID, afterName string, afterVersion, limit int) ([]*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	query := `SELECT ` + capabilityColumns + ` FROM capability_versions
	 WHERE namespace_id = ? AND deleted_at IS NULL`
	args := []any{namespaceID}
	if afterName != "" {
		query += ` AND (name > ? OR (name = ? AND version < ?))`
		args = append(args, afterName, afterName, afterVersion)
	}
	query += ` ORDER BY name, version DESC LIMIT ?`
	args = append(args, normalizePageLimit(limit))
	rows, err := d.cachedQueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list capabilities page: %w", err)
	}
	defer rows.Close()
	out := []*CapabilityVersion{}
	for rows.Next() {
		var c CapabilityVersion
		if err := scanCapabilityRow(rows, &c); err != nil {
			return nil, fmt.Errorf("scan capability row: %w", err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NextCapabilityVersion 返回某 name 的下一个版本号（无定义时返回 1）。
//
// 写入路径不要用它来定版本（中间有窗口）；store 内部走 nextCapabilityVersionOn。
// 它保留给「只想看看下一个号是多少」的调用方与测试。
func (d *DB) NextCapabilityVersion(ctx context.Context, namespaceID, name string) (int, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return 0, ErrNamespaceRequired
	}
	return nextCapabilityVersionOn(ctx, d, namespaceID, name)
}

// CapabilityNamespace 返回能力定义所属的命名空间 id（隔离校验共用）。
func (d *DB) CapabilityNamespace(ctx context.Context, capabilityID string) (string, error) {
	if capabilityID == "" {
		return "", ErrNotFound
	}
	var ns string
	err := d.QueryRowContext(ctx,
		`SELECT namespace_id FROM capability_versions WHERE id = ? AND deleted_at IS NULL`, capabilityID).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup capability namespace: %w", err)
	}
	return ns, nil
}

// checkCapabilityInNamespace 是隔离的公共校验：能力定义必须属于 wantNS。
//
// what 用来说明违反的是哪条引用，让人一眼看出是哪一端写错了。
func (d *DB) checkCapabilityInNamespace(ctx context.Context, wantNS, capabilityID, what string) error {
	ns, err := d.CapabilityNamespace(ctx, capabilityID)
	if err != nil {
		return err
	}
	if ns != wantNS {
		return &CrossNamespaceError{What: what, Want: wantNS, Got: ns}
	}
	return nil
}
