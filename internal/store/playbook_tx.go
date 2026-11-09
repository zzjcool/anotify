package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// 本文件是 Playbook 安装的**事务内实现细节**（playbook.go 是公开面）。
//
// 为什么单独一个文件：安装路径上所有读都必须走当前事务（txExec），而 store 里
// 「事务外读」的既有方法是 `*DB` 方法（走 per-DB 语句缓存）。混用会读到**另一个快照**：
//
//	① 看不到本事务刚写的行（新源 targets 指向同包刚建的能力）；
//	② 与提交之间出现「检查时成立、提交后不成立」的窗口（把读错当空库 → 多建一批行）。
//
// 所以这里的每个查询都只经 ex，且把 error 交给调用方裁决（installSnapshot）。

// getPlaybookOn 是事务内按 (namespace, slug) 取包（GetPlaybook 的 ex 版）。
func getPlaybookOn(ctx context.Context, ex queryExec, namespaceID, slug string) (*PlaybookRow, error) {
	row := ex.queryRowContext(ctx,
		`SELECT `+playbookColumns+` FROM playbooks WHERE namespace_id = ? AND slug = ?`,
		namespaceID, slug)
	p, err := scanPlaybookRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get playbook: %w", err)
	}
	return p, nil
}

// getNamespaceSlugOn 取命名空间 slug（回执的 namespace 快照写它，不是内部 id）。
func getNamespaceSlugOn(ctx context.Context, ex queryExec, namespaceID string) (string, error) {
	var slug string
	err := ex.queryRowContext(ctx, `SELECT slug FROM namespaces WHERE id = ?`, namespaceID).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNamespaceNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get namespace slug: %w", err)
	}
	return slug, nil
}

// getSourceByNameOn 是事务内按名字取源（GetSourceByName 的只读 ex 版）。
//
// 不能调用 GetSourceByName：它走 per-DB 语句缓存，在本事务里读的是另一条连接。
func getSourceByNameOn(ctx context.Context, ex queryExec, namespaceID, name string) (*Source, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT `+sourceColumns+` FROM sources WHERE namespace_id = ? AND name = ?`,
		namespaceID, name)
	if err != nil {
		return nil, fmt.Errorf("get source by name: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get source by name: %w", err)
		}
		return nil, ErrSourceNotFound
	}
	return scanSource(rows)
}

// sourceTargetRow 是「源名 + targets 声明」的轻量读模型（引用计数 / 上游判断用）。
type sourceTargetRow struct {
	id      string
	targets []sourcespec.Target
}

// listSourceTargetsOn 读出命名空间内所有源的 targets（只读两列，不碰调度字段）。
//
// 坏 JSON 是**读错误**（不是空列表）：调用方（installSnapshot）必须让安装失败，
// 而不是把一条损坏的源当成「没有 targets」继续装。
func listSourceTargetsOn(ctx context.Context, ex queryExec, namespaceID string) ([]sourceTargetRow, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT id, targets FROM sources WHERE namespace_id = ?`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list source targets: %w", err)
	}
	defer rows.Close()
	out := []sourceTargetRow{}
	for rows.Next() {
		var (
			id     string
			target string
		)
		if err := rows.Scan(&id, &target); err != nil {
			return nil, fmt.Errorf("scan source targets: %w", err)
		}
		parsed := []sourcespec.Target{}
		if trimmed := strings.TrimSpace(target); trimmed != "" {
			if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
				return nil, fmt.Errorf("decode source %s targets: %w", id, err)
			}
		}
		if parsed == nil {
			parsed = []sourcespec.Target{}
		}
		out = append(out, sourceTargetRow{id: id, targets: parsed})
	}
	return out, rows.Err()
}

// installSnapshot 是 `playbook.Existing` 的**事务内**实现：六个只读查询 + 错误账本。
//
// 接口本身没有 error（纯包要保持纯），所以这里把每个查询的错误记下来；Plan 结束后
// 调用方先看 snap.err：读失败时 Plan 得到的「空库」判断不可信，整次安装必须回滚失败。
type installSnapshot struct {
	ctx         context.Context
	ex          queryExec
	namespaceID string
	// err 是第一个读错误（后续查询直接短路，不再产生误导性的 false）。
	err error
}

func newInstallSnapshot(ctx context.Context, ex queryExec, namespaceID string) *installSnapshot {
	return &installSnapshot{ctx: ctx, ex: ex, namespaceID: namespaceID}
}

// fail 记下第一个错误（后续错误不覆盖：根因比后缀有用）。
func (s *installSnapshot) fail(err error) {
	if err != nil && s.err == nil {
		s.err = err
	}
}

// getCapabilityByFoldKeyLatestOn 按 foldKey 取**最高版本**那一行（安装快照专用）。
//
// 为什么必须 ORDER BY version DESC：同一个 foldKey 可以有多行（显式 v1 与 v3 正文相同）。
// 省略 version 的安装（12 格第 2 行）必须确定性地复用最高版本——否则「复用哪一行」
// 取决于 SQLite 的返回顺序，skipped 的版本号与 unapplied 的比较对象都会漂移。
// 直写路径的 getCapabilityByFoldKeyOn 保持原样（本函数只服务安装快照）。
func getCapabilityByFoldKeyLatestOn(ctx context.Context, ex queryExec, namespaceID, name, foldKey string) (*CapabilityVersion, error) {
	return scanCapabilityOn(ctx, ex,
		`SELECT `+capabilityColumns+` FROM capability_versions
		  WHERE namespace_id = ? AND name = ? AND fold_key = ?
		  ORDER BY version DESC LIMIT 1`,
		namespaceID, name, foldKey)
}

// capabilitySnapshot 把库里的能力行折成纯包的快照形状。
func capabilitySnapshot(c *CapabilityVersion) *playbook.CapabilitySnapshot {
	if c == nil {
		return nil
	}
	return &playbook.CapabilitySnapshot{
		Name:        c.Name,
		Version:     c.Version,
		Digest:      c.Digest,
		FoldKey:     c.FoldKey,
		DisplayName: c.DisplayName,
		Next:        append([]string{}, c.Spec.Next...),
	}
}

func (s *installSnapshot) CapabilityByFoldKey(name, foldKey string) (*playbook.CapabilitySnapshot, bool) {
	if s.err != nil {
		return nil, false
	}
	c, err := getCapabilityByFoldKeyLatestOn(s.ctx, s.ex, s.namespaceID, name, foldKey)
	if errors.Is(err, ErrNotFound) {
		return nil, false
	}
	if err != nil {
		s.fail(fmt.Errorf("capability %q by fold key: %w", name, err))
		return nil, false
	}
	return capabilitySnapshot(c), true
}

func (s *installSnapshot) CapabilityByNameVersion(name string, version int) (*playbook.CapabilitySnapshot, bool) {
	if s.err != nil {
		return nil, false
	}
	c, err := getCapabilityOn(s.ctx, s.ex, s.namespaceID, name, version)
	if errors.Is(err, ErrNotFound) {
		return nil, false
	}
	if err != nil {
		s.fail(fmt.Errorf("capability %q@%d: %w", name, version, err))
		return nil, false
	}
	return capabilitySnapshot(c), true
}

func (s *installSnapshot) CapabilityMaxVersion(name string) (int, bool) {
	if s.err != nil {
		return 0, false
	}
	next, err := nextCapabilityVersionOn(s.ctx, s.ex, s.namespaceID, name)
	if err != nil {
		s.fail(fmt.Errorf("capability %q max version: %w", name, err))
		return 0, false
	}
	if next <= 1 {
		return 0, false
	}
	return next - 1, true
}

func (s *installSnapshot) SourceByName(name string) (*playbook.SourceSnapshot, bool) {
	if s.err != nil {
		return nil, false
	}
	src, err := getSourceByNameOn(s.ctx, s.ex, s.namespaceID, name)
	if errors.Is(err, ErrSourceNotFound) {
		return nil, false
	}
	if err != nil {
		s.fail(fmt.Errorf("source %q: %w", name, err))
		return nil, false
	}
	return &playbook.SourceSnapshot{
		Name:    src.Name,
		Kind:    src.Kind,
		Enabled: src.Enabled,
		Config:  append(json.RawMessage(nil), src.Config...),
		Targets: append([]sourcespec.Target{}, src.Targets...),
	}, true
}

// BareNameRefs 返回库里**裸名**引用 name 的边数与源数（upgraded[].affects 的两个计数）。
func (s *installSnapshot) BareNameRefs(name string) (edges, sources int) {
	if s.err != nil {
		return 0, 0
	}
	var edgeCount int
	if err := s.ex.queryRowContext(s.ctx,
		`SELECT count(*) FROM edges
		  WHERE namespace_id = ? AND from_is_source = 0 AND to_ref = ?`,
		s.namespaceID, name).Scan(&edgeCount); err != nil {
		s.fail(fmt.Errorf("count bare edge refs of %q: %w", name, err))
		return 0, 0
	}
	targets, err := listSourceTargetsOn(s.ctx, s.ex, s.namespaceID)
	if err != nil {
		s.fail(err)
		return 0, 0
	}
	sourceCount := 0
	for _, row := range targets {
		for _, t := range row.targets {
			ref, _, pinned := SplitRef(strings.TrimSpace(t.Capability))
			if ref != name || pinned {
				continue
			}
			sourceCount++
			break
		}
	}
	return edgeCount, sourceCount
}

// HasUpstream 报告库里是否已有指向 name 的引用（裸名，或 name@任意版本）。
func (s *installSnapshot) HasUpstream(name string) bool {
	if s.err != nil {
		return false
	}
	prefix := name + string(RefVersionSep)
	rows, err := s.ex.queryContext(s.ctx,
		`SELECT to_ref FROM edges
		  WHERE namespace_id = ? AND (to_ref = ? OR substr(to_ref, 1, ?) = ?)`,
		s.namespaceID, name, len(prefix), prefix)
	if err != nil {
		s.fail(fmt.Errorf("edges upstream of %q: %w", name, err))
		return false
	}
	found := false
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			s.fail(fmt.Errorf("scan edge ref: %w", err))
			return false
		}
		if got, _, _ := SplitRef(ref); got == name {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.fail(fmt.Errorf("iterate edge refs: %w", err))
		return false
	}
	rows.Close()
	if found {
		return true
	}
	targets, err := listSourceTargetsOn(s.ctx, s.ex, s.namespaceID)
	if err != nil {
		s.fail(err)
		return false
	}
	for _, row := range targets {
		for _, t := range row.targets {
			if got, _, _ := SplitRef(strings.TrimSpace(t.Capability)); got == name {
				return true
			}
		}
	}
	return false
}

// insertInstalledCapabilityOn 落一条**本次新建**的能力行（计划已定 version / digest）。
//
// 只处理 Action=create：reuse（skipped）条目一行都不写，也不调用任何「折叠更新」
// 入口（CreateCapability / UpdateNext / SetCapabilityDisplayName）——那些会改已存行的
// next / displayName，违反安装只做加法（I-P1）。所以这里不复用 insertCapabilityOn
// （它的唯一冲突分支会回查复用），而是严格 INSERT：撞唯一约束就是错误。
//
// 边投影只属于**该名字的 latest**：显式低版本新建（已有 v3，再装 v1）不改变裸名解析
// 到的版本，因此也**不能**重建这个名字的边（否则边指向 v1 的 next、派发却落到 v3）。
func insertInstalledCapabilityOn(ctx context.Context, ex queryExec, namespaceID, installID string, pc playbook.PlannedCapability) error {
	if pc.Artifact == nil {
		return fmt.Errorf("planned capability is nil")
	}
	a := *pc.Artifact
	norm, err := artifact.Normalize(&a)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrCapabilityInvalid, err.Error())
	}
	if verr := artifact.Validate(norm); verr != nil {
		return fmt.Errorf("%w: %s", ErrCapabilityInvalid, verr.Error())
	}
	name, version := norm.Metadata.Name, norm.Metadata.Version
	if version < 1 {
		return fmt.Errorf("planned capability %q 没有版本号（Plan 必须先定 version）", name)
	}
	digest, err := artifact.Digest(norm)
	if err != nil {
		return fmt.Errorf("digest capability %s@%d: %w", name, version, err)
	}
	foldKey, err := artifact.FoldKey(norm)
	if err != nil {
		return fmt.Errorf("fold key for %s@%d: %w", name, version, err)
	}
	specJSON, err := marshalCapabilitySpec(norm.Spec)
	if err != nil {
		return err
	}
	// nextCapabilityVersionOn 返回 max+1：本次写入的版本 >= 它，才成为该名字的新 latest。
	nextVersion, err := nextCapabilityVersionOn(ctx, ex, namespaceID, name)
	if err != nil {
		return err
	}
	becomesLatest := version >= nextVersion
	now := Now()
	id := NewCapabilityID(strings.TrimPrefix(digest, artifact.DigestPrefix))
	if _, ierr := ex.execContext(ctx,
		`INSERT INTO capability_versions
		   (id, namespace_id, name, display_name, version, spec, digest, fold_key,
		    installed_from, resource_version, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		id, namespaceID, name, norm.Metadata.DisplayName, version, string(specJSON),
		digest, foldKey, installID, 1, now); ierr != nil {
		if isUniqueConstraintError(ierr) || isUniqueViolation(ierr) {
			// 安装快照已经判过「这一行不存在」；撞约束说明计划与库不一致。
			// **不回查折叠、不改已有行**：只加不改的边界在这里是最硬的。
			return fmt.Errorf("capability %s@%d 已存在（Plan 与库快照不一致）：%w", name, version, ierr)
		}
		return fmt.Errorf("insert capability %s@%d: %w", name, version, ierr)
	}
	if !becomesLatest {
		// 显式低版本：该名字的 max 版本与边投影都不变（新行只是多一个可钉死的版本）。
		return nil
	}
	return rebuildCapabilityEdgesOn(ctx, ex, namespaceID, name, norm.Spec.Next)
}

// createInstalledSourceOn 落一条**本次新建**的源行 + 入口边投影（只对新源写）。
//
// config 走与直写 CreateSource 完全相同的归一化（含「webhook token 省略则服务端生成」）：
// token 的生成是创建期一次性副作用，生成的值只进这一行（dryRun 随事务回滚）。
// **不校验 targets 可解析**：安装允许 pending（§3 第三行），那是 Plan 的判定，不是写路径的。
func (d *DB) createInstalledSourceOn(ctx context.Context, ex queryExec, namespaceID, installID string, ps playbook.PlannedSource, now int64) error {
	cfg, err := d.prepareSourceConfig(ps.Spec.Kind, ps.Spec.Config)
	if err != nil {
		return fmt.Errorf("source %q config: %w", ps.Name, err)
	}
	src := &Source{
		ID:            NewSourceID(),
		NamespaceID:   namespaceID,
		Name:          ps.Name,
		Kind:          ps.Spec.Kind,
		Config:        cfg,
		Targets:       append([]sourcespec.Target{}, ps.Spec.Targets...),
		Enabled:       ps.Spec.Enabled,
		InstalledFrom: installID,
		// ResourceVersion 是写并发令牌：新行从 1 开始（更新时 +1）。
		ResourceVersion: 1,
		CreatedAt:       now,
	}
	nextFire, err := sourceNextFire(src, now)
	if err != nil {
		return fmt.Errorf("source %q next fire: %w", ps.Name, err)
	}
	src.NextFireAt = nextFire
	if err := insertSourceOn(ctx, ex, src); err != nil {
		return fmt.Errorf("source %q: %w", ps.Name, err)
	}
	return nil
}

// insertPlaybookInstallOn 写一行安装回执（dryRun 也会走到这里，然后随事务回滚）。
//
// 落库内容（§4.5 / D21）：
//   - `values` 是**打码后**的有效 values（writeOnly → "***"），digest 对打码前算；
//   - `receipt` 是回执原文（响应文档，不是资源信封）；
//   - `namespace_slug` / `playbook_slug` / `playbook_id` 是快照：删包之后回执仍可读，
//     同 slug 重建的新包也不会把旧回执认成自己的实例。
func insertPlaybookInstallOn(ctx context.Context, ex queryExec, namespaceID string, row *PlaybookRow, receipt *playbook.Receipt, now int64) error {
	valuesJSON, err := json.Marshal(receipt.Values)
	if err != nil {
		return fmt.Errorf("marshal install values: %w", err)
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("marshal install receipt: %w", err)
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO playbook_installs
		   (id, namespace_id, namespace_slug, playbook_id, playbook_slug,
		    "values", values_digest, receipt, source, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		receipt.Install.ID, namespaceID, receipt.Namespace, row.ID, row.Slug,
		string(valuesJSON), receipt.ValuesDigest, string(receiptJSON), receipt.Source, now); err != nil {
		return fmt.Errorf("insert install receipt: %w", err)
	}
	return nil
}

// keptPlaybookObjectsOn 反查删包后留下的对象（DELETE 响应的 kept）。
//
// 能力写 `name@version`、源写名字；两个列表都按 installed_from ∈ 本包历史回执 过滤。
// **不需要再用 values 渲染一次**：installed_from 记录的就是「这一行是谁建的」（§7）。
func keptPlaybookObjectsOn(ctx context.Context, ex queryExec, namespaceID, playbookID string) (PlaybookKept, error) {
	kept := PlaybookKept{Capabilities: []string{}, Sources: []string{}}
	capRows, err := ex.queryContext(ctx,
		`SELECT name, version FROM capability_versions
		  WHERE namespace_id = ?
		    AND installed_from IN (SELECT id FROM playbook_installs
		                            WHERE namespace_id = ? AND playbook_id = ?)
		  ORDER BY name, version`, namespaceID, namespaceID, playbookID)
	if err != nil {
		return kept, fmt.Errorf("kept playbook capabilities: %w", err)
	}
	for capRows.Next() {
		var name string
		var version int
		if err := capRows.Scan(&name, &version); err != nil {
			capRows.Close()
			return kept, fmt.Errorf("scan kept capability: %w", err)
		}
		kept.Capabilities = append(kept.Capabilities, FormatRef(name, version))
	}
	if err := capRows.Err(); err != nil {
		capRows.Close()
		return kept, fmt.Errorf("iterate kept capabilities: %w", err)
	}
	capRows.Close()

	srcRows, err := ex.queryContext(ctx,
		`SELECT name FROM sources
		  WHERE namespace_id = ?
		    AND installed_from IN (SELECT id FROM playbook_installs
		                            WHERE namespace_id = ? AND playbook_id = ?)
		  ORDER BY name`, namespaceID, namespaceID, playbookID)
	if err != nil {
		return kept, fmt.Errorf("kept playbook sources: %w", err)
	}
	for srcRows.Next() {
		var name string
		if err := srcRows.Scan(&name); err != nil {
			srcRows.Close()
			return kept, fmt.Errorf("scan kept source: %w", err)
		}
		kept.Sources = append(kept.Sources, name)
	}
	if err := srcRows.Err(); err != nil {
		srcRows.Close()
		return kept, fmt.Errorf("iterate kept sources: %w", err)
	}
	srcRows.Close()
	return kept, nil
}

// playbookInstallValuesOn 汇总一组「装过哪些 values」（DELETE 响应的 values）。
//
// 按 valuesDigest 去重（同一组 values 装 N 次只报一次），保留首次出现的顺序；
// values 本身就是回执行里的**打码后**形态，这里不再二次处理。
func playbookInstallValuesOn(ctx context.Context, ex queryExec, namespaceID, playbookID string) ([]map[string]any, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT "values", values_digest FROM playbook_installs
		  WHERE namespace_id = ? AND playbook_id = ?
		  ORDER BY created_at, rowid`, namespaceID, playbookID)
	if err != nil {
		return nil, fmt.Errorf("playbook install values: %w", err)
	}
	defer rows.Close()
	out := []map[string]any{}
	seen := map[string]bool{}
	for rows.Next() {
		var raw, digest string
		if err := rows.Scan(&raw, &digest); err != nil {
			return nil, fmt.Errorf("scan install values: %w", err)
		}
		if seen[digest] {
			continue
		}
		parsed := map[string]any{}
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
				return nil, fmt.Errorf("decode install values %s: %w", digest, err)
			}
			if parsed == nil {
				parsed = map[string]any{}
			}
		}
		seen[digest] = true
		out = append(out, parsed)
	}
	return out, rows.Err()
}
