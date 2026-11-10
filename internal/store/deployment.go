package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
)

// 部署状态（deployments.state）。
//
// 写者纪律（CONTRACT-protocol §7）：
//   - 建行/删行只有控制台 API 会做；
//   - state / reason / updated_at 只有节点帧（apply）与 hello 对账会写。
const (
	// DeploymentPending 中枢想部署，还没收到节点确认。
	DeploymentPending = "pending"
	// DeploymentApplied 节点确认应用成功。
	DeploymentApplied = "applied"
	// DeploymentFailed 节点拒绝（reason 里是短码 + 说明）。
	DeploymentFailed = "failed"
	// DeploymentLocal 是**旧的**「本地变体」态：节点上的 digest 与中枢期望不一致，
	// 且中枢不敢覆盖。
	//
	// 现在不再产生这个态：中枢是部署的唯一写者，发现不一致就重新下发（覆盖），
	// 对账会把行打成 pending。保留它只为了让旧库里已有的行能被读出来（下一次 hello
	// 对账就会把它们改成 applied/pending）。UI 上的黄色“本地变体”也因此只会是历史残留。
	DeploymentLocal = "local"
)

// ErrArtifactNotFound 表示「这个能力此刻没有可部署/可引用的产物」。
//
// v4 只有 run 一种实现形式，所以它只有两种来源：
//   - 能力不存在，或不属于该命名空间；
//   - 能力没有 run（spec 过不了 artifact.Validate 的行不该出现在库里，防御性判一次）。
//
// 名字沿用旧哨兵：handler 层靠它翻 404 artifact_not_found，改名字换不来任何好处。
var ErrArtifactNotFound = errors.New("store: artifact not found")

// ErrDeploymentExists 同一 (node, capability, digest) 已经部署过。
var ErrDeploymentExists = errors.New("store: deployment already exists")

// Artifact 是一份能力产物的**派生值**（Capability Spec v3 §11.2）。
//
// P2 之后它不再是表里的一行，而是「能力行的正文 + metadata」拼出来的整份产物：
// digest 是能力行的 content_hash（≡ 产物 digest），Spec 是整份 canonical JSON。
// 之所以还留着一个结构体而不是返回 (digest, spec)：deploy 帧、/v1/artifacts、撤销帧
// 三处都要同一份形状，拼三遍迟早会拼出三种 digest。
//
// 字段取舍（契约 §11.2 / P2a）：
//   - NamespaceID / CapabilityID / Digest / Spec 是内容的全部身份，都留着；
//   - CreatedAt 删掉：没有行了，就没有落库时间（它唯一的作用是「新的在前」排序，
//     而按能力取只有一份）。
type Artifact struct {
	// P2 起产物不再是一个行身份：它是能力行正文的**派生**（Capability Spec v3 §11）。
	// 曾经留过一个空的 ID 字段做兼容，P2b 已删：没有行身份就没有 ID，
	// 任何调用方读到它只会得到一个永远为空的假标识。
	NamespaceID  string `json:"namespaceId"`
	CapabilityID string `json:"capabilityId"`
	Digest       string `json:"digest"`
	// Spec 是整份产物的 canonical JSON（CanonicalJSON 的结果，含 metadata 与 spec）。
	Spec []byte `json:"spec"`
}

// Deployment 是「某台节点应当拥有某份产物」+「节点回报的状态」。
//
// 引用的是**能力行**（capability_id），digest 是那次期望的内容标识。没有 artifact_id：
// 产物不再是一个可以单独引用的物体。
type Deployment struct {
	ID           string `json:"id"`
	NamespaceID  string `json:"namespaceId"`
	NodeID       string `json:"nodeId"`
	CapabilityID string `json:"capabilityId"`
	Capability   string `json:"capability"`
	Version      int    `json:"version"`
	Digest       string `json:"digest"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

// CurrentArtifact 返回某能力**当前产物**：digest = 能力行的 content_hash，
// Spec = 由 capability_versions.spec 拼出的整份 canonical JSON。
//
// 能力不存在、或它没有 run 时返回 ErrArtifactNotFound —— 「还没有产物」与
// 「这个能力不该有产物」在**部署**这条路上是同一个回答。
func (d *DB) CurrentArtifact(ctx context.Context, namespaceID, capabilityID string) (*Artifact, error) {
	return currentArtifactOn(ctx, d, namespaceID, capabilityID)
}

// CurrentArtifact 的 ex 版（事务内读必须走它，否则看不到本事务刚写的行）。
func currentArtifactOn(ctx context.Context, ex queryExec, namespaceID, capabilityID string) (*Artifact, error) {
	c, err := getCapabilityByIDOn(ctx, ex, namespaceID, capabilityID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read capability for artifact: %w", err)
	}
	return artifactOf(c)
}

// deployable 判「这个能力有没有一份可推给节点的产物」。
//
// 可部署 ⟺ **有 run**（entrypoint + files）。v4 只有脚本一种实现形式，
// 「纯定义」不在合法形状里（写入期 artifact.Validate 就拒），这里只是防御性判一次：
// 放过去的后果是它出现在部署面板里，推下去节点只会拒。
func deployable(c *CapabilityVersion) error {
	if c.Spec.Run.Entrypoint == "" {
		return ErrArtifactNotFound
	}
	return nil
}

// artifactOf 把一行能力拼成整份产物（**唯一**的拼装点）。
//
// 两件事必须一起做对，否则症状极难查：
//   - digest 取能力行上的 digest（含 version 的部署身份，capability.md §3）。
//     它必须与节点从收到的字节重算出来的一致，否则部署永远收敛不了；
//   - Spec 取 `artifact.CanonicalJSON(c.Artifact())`（与 digest 同一次归一化）。
func artifactOf(c *CapabilityVersion) (*Artifact, error) {
	if err := deployable(c); err != nil {
		return nil, err
	}
	a := c.Artifact()
	spec, err := artifact.CanonicalJSON(&a)
	if err != nil {
		return nil, fmt.Errorf("canonical json for %s@%d: %w", c.Name, c.Version, err)
	}
	return &Artifact{
		NamespaceID:  c.NamespaceID,
		CapabilityID: c.ID,
		Digest:       c.Digest,
		Spec:         spec,
	}, nil
}

// GetArtifactByDigest 按内容寻址标识取产物：digest 就是 capability_versions.digest。
//
// 内容寻址：同一个 digest 在同一命名空间里最多命中一行能力（UNIQUE(namespace_id, digest)）。
// 没有 run 的行命中时返回 ErrArtifactNotFound ——它没有可推的产物。
func (d *DB) GetArtifactByDigest(ctx context.Context, namespaceID, digest string) (*Artifact, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return nil, ErrArtifactNotFound
	}
	c, err := getCapabilityByDigestOn(ctx, d, namespaceID, digest)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get artifact by digest: %w", err)
	}
	return artifactOf(c)
}

// ListArtifacts 返回某个能力的产物列表。
//
// **最多一条**（P2 起的语义，契约 §11.2）：产物不再是可累加的历史行，一个能力行
// = 一份内容 = 一个 digest。保留列表形状只为不动 `GET /v1/artifacts` 的响应契约
// （{artifacts:[…]}，e2e 与外部调用方都钉着它）。
//
// 能力不存在 / handler 形式 → 空列表（不是错误）：列表端点回答的是「有哪些」。
func (d *DB) ListArtifacts(ctx context.Context, namespaceID, capabilityID string) ([]*Artifact, error) {
	got, err := d.CurrentArtifact(ctx, namespaceID, capabilityID)
	if errors.Is(err, ErrArtifactNotFound) {
		return []*Artifact{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []*Artifact{got}, nil
}

// CreateDeployments 人工部署：为每个节点写一行 pending（「中枢希望这台机器持有这份内容」）。
//
// 语义（capability.md §2.4 Deployed condition / 契约 §11.2）：
//   - 只引用 capability_id + digest，没有 artifact 行可指（产物不再是可引用物体）；
//   - digest 的唯一来源是能力行：传空 = 用能力行的 digest；传了就必须与它相等
//     （部署别的能力的 digest 是自相矛盾的请求，按「找不到这份产物」拒绝）；
//   - 同一 (node, capability) **只保留一行**：部署是期望状态，新期望直接取代旧的。
//     留着旧行会让同一能力同时存在两条互斥期望，节点每次重连都在两份内容之间来回被推。
//
// nodeIDs 为空是 no-op（不是错误）：控制台要部署的机器集合可能被筛空了。
// 已存在的 (node, capability, digest) 视为幂等 no-op：重复点部署不该把绿标打回灰。
func (d *DB) CreateDeployments(ctx context.Context, namespaceID, capabilityID, digest string, nodeIDs []string) ([]*Deployment, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if capabilityID == "" {
		return nil, fmt.Errorf("create deployments: empty capability_id")
	}
	if len(nodeIDs) == 0 {
		return []*Deployment{}, nil
	}
	now := Now()

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin create deployments: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	capRow, err := getCapabilityByIDOn(ctx, txExec{tx: tx}, namespaceID, capabilityID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve deployment capability: %w", err)
	}
	if derr := deployable(capRow); derr != nil {
		return nil, derr
	}
	digest = strings.TrimSpace(digest)
	if digest == "" {
		digest = capRow.Digest
	}
	if digest != capRow.Digest {
		return nil, ErrArtifactNotFound
	}

	seen := make(map[string]bool, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID == "" || seen[nodeID] {
			continue
		}
		seen[nodeID] = true
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE id = ?`, nodeID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("create deployments: probe node %s: %w", nodeID, err)
		}
		if exists == 0 {
			return nil, fmt.Errorf("create deployments: node %s: %w", nodeID, ErrNotFound)
		}
		// 取代范围只能是同一能力的旧行（别的能力不碰）。
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM deployments
			  WHERE namespace_id = ? AND node_id = ? AND capability_id = ? AND digest <> ?`,
			namespaceID, nodeID, capabilityID, digest); err != nil {
			return nil, fmt.Errorf("supersede previous deployment: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO deployments (id, namespace_id, node_id, capability_id, digest, state, reason, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,'',?,?)
			 ON CONFLICT(node_id, capability_id, digest) DO NOTHING`,
			NewDeploymentID(), namespaceID, nodeID, capabilityID, digest, DeploymentPending, now, now); err != nil {
			return nil, fmt.Errorf("create deployment for %s: %w", nodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create deployments: %w", err)
	}

	out := make([]*Deployment, 0, len(seen))
	rows, err := d.QueryContext(ctx,
		deploymentSelect+` WHERE d.namespace_id = ? AND d.capability_id = ? ORDER BY d.node_id`, namespaceID, capabilityID)
	if err != nil {
		return nil, fmt.Errorf("read created deployments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		dp, serr := scanDeploymentRow(rows)
		if serr != nil {
			return nil, serr
		}
		if seen[dp.NodeID] {
			out = append(out, dp)
		}
	}
	return out, rows.Err()
}

// GetDeploymentByNodeCapability 取一条部署（同一 (node, capability) 至多一行）。
func (d *DB) GetDeploymentByNodeCapability(ctx context.Context, namespaceID, nodeID, capabilityID string) (*Deployment, error) {
	return d.scanDeployment(ctx,
		deploymentSelect+` WHERE c.deleted_at IS NULL AND d.namespace_id = ? AND d.node_id = ? AND d.capability_id = ?`,
		namespaceID, nodeID, capabilityID)
}

// GetDeployment 按 id 取一条部署。
func (d *DB) GetDeployment(ctx context.Context, namespaceID, id string) (*Deployment, error) {
	return d.scanDeployment(ctx, deploymentSelect+` WHERE c.deleted_at IS NULL AND d.namespace_id = ? AND d.id = ?`, namespaceID, id)
}

// deploymentDigestSQL 是「中枢期望的 digest」的**唯一**口径：能力行的 digest 列
// （含 `sha256:` 前缀的部署身份）。不读 d.digest 列——列里那份是建行那一刻的副本
// （审计用），而期望永远是「这个能力此刻的内容」。
//
// 为什么要统一：节点算 digest 用的是它收到的产物字节，中枢若拿别的值去比，
// 症状是「每次连上来都判不一致、每次都重推一遍」——不影响正确性，但永远收敛不了。
const deploymentDigestSQL = `c.digest`

const deploymentSelect = `
SELECT d.id, d.namespace_id, d.node_id, d.capability_id, ` + deploymentDigestSQL + ` AS digest,
       d.state, d.reason, d.created_at, d.updated_at, c.name, c.version
  FROM deployments d
  JOIN capability_versions c ON c.id = d.capability_id`

func (d *DB) scanDeployment(ctx context.Context, query string, args ...any) (*Deployment, error) {
	dp, err := scanDeploymentOne(d.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	return dp, nil
}

// deploymentScanner 是 *sql.Rows / *sql.Row 的最小公共面（都只有 Scan）。
type deploymentScanner interface{ Scan(dest ...any) error }

// scanDeploymentOne 从任意 Scan 面解析一条部署。
func scanDeploymentOne(row deploymentScanner) (*Deployment, error) {
	var dp Deployment
	if err := row.Scan(
		&dp.ID, &dp.NamespaceID, &dp.NodeID, &dp.CapabilityID, &dp.Digest,
		&dp.State, &dp.Reason, &dp.CreatedAt, &dp.UpdatedAt,
		&dp.Capability, &dp.Version); err != nil {
		return nil, err
	}
	return &dp, nil
}

// scanDeploymentRow 从结果集解析一条部署（列表路径）。
func scanDeploymentRow(rows *sql.Rows) (*Deployment, error) {
	dp, err := scanDeploymentOne(rows)
	if err != nil {
		return nil, fmt.Errorf("scan deployment: %w", err)
	}
	return dp, nil
}

// DeploymentFilter 是部署列表的过滤条件（零值 = 不过滤）。
type DeploymentFilter struct {
	NodeID       string
	CapabilityID string
}

// ListDeployments 列出部署。
func (d *DB) ListDeployments(ctx context.Context, namespaceID string, f DeploymentFilter) ([]*Deployment, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	var where []string
	args := []any{namespaceID}
	where = append(where, "d.namespace_id = ?", "c.deleted_at IS NULL")
	if f.NodeID != "" {
		where = append(where, "d.node_id = ?")
		args = append(args, f.NodeID)
	}
	if f.CapabilityID != "" {
		where = append(where, "d.capability_id = ?")
		args = append(args, f.CapabilityID)
	}

	rows, err := d.QueryContext(ctx,
		deploymentSelect+` WHERE `+strings.Join(where, " AND ")+
			` ORDER BY d.created_at DESC, d.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()

	out := []*Deployment{}
	for rows.Next() {
		dp, serr := scanDeploymentRow(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, dp)
	}
	return out, rows.Err()
}

// DeleteDeployment 删一条部署，返回被删的行（供调用方推 remove 帧）。
func (d *DB) DeleteDeployment(ctx context.Context, namespaceID, id string) (*Deployment, error) {
	dp, err := d.GetDeployment(ctx, namespaceID, id)
	if err != nil {
		return nil, err
	}
	if _, err := d.ExecContext(ctx,
		`DELETE FROM deployments WHERE namespace_id = ? AND id = ?`, namespaceID, id); err != nil {
		return nil, fmt.Errorf("delete deployment: %w", err)
	}
	return dp, nil
}

// MarkDeployResult 落一条来自节点 apply 帧的结果（只碰 observed 三列）。
func (d *DB) MarkDeployResult(ctx context.Context, namespaceID, deployID, state, reason string) error {
	// pending 也是合法目标：中枢重连时把“要重新下发”的行标回 pending（中枢覆盖语义）。
	if state != DeploymentApplied && state != DeploymentFailed &&
		state != DeploymentLocal && state != DeploymentPending {
		return fmt.Errorf("mark deploy result: invalid state %q", state)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE deployments SET state = ?, reason = ?, updated_at = ?
		  WHERE namespace_id = ? AND id = ?`,
		state, reason, Now(), namespaceID, deployID)
	if err != nil {
		return fmt.Errorf("mark deploy result: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AppliedCapability 是节点 hello 自报的「我持有这份内容」的一条事实。
//
// 三者一起才构成部署身份：能力 id 说「是哪个能力」，version 说「哪一版」，
// digest 说「哪份字节」。只对 digest 会让「同内容不同版本」被误判成已部署。
type AppliedCapability struct {
	CapabilityID string
	Capability   string
	Version      int
	Digest       string
}

// ReconcileStats 汇总一次节点对账的结果。
type ReconcileStats struct {
	// Applied 是与中枢期望一致的行数。
	Applied int
	// Stale 是节点上的内容与期望不一致（已被打回 pending，等着被重推）的行数。
	Stale int
	// Missing 是节点上根本没有上报这条能力（同样打回 pending）的行数。
	Missing int
	// Extra 是节点上有、中枢没有要求的内容（「本地变体」）。
	//
	// v4 的处置：**不**替它建部署行。部署是中枢下发的意图，节点自报不能变成
	// 「中枢曾经要求过」；把它作为漂移事实返回给调用方记日志/展示即可。
	// 这正是 v3 `state=local` 想表达的信息，但不再需要一个永不收敛的颜色。
	Extra []AppliedCapability
}

// ReconcileNode 用节点自报的 applied 列表对账它名下的部署行。
//
// 判据（中枢覆盖语义，capability.md §2.4）：
//   - digest 与期望一致 → applied（绿勾）
//   - digest 不一致       → pending + reason 里写明两端 digest（stale，等重推）
//   - 节点没上报这条能力  → pending（缺失，等重推）
//
// 期望值取**能力行**的 digest（deploymentDigestSQL），不是 deployments.digest 列的副本：
// 列里那份是建行那一刻的快照，而「这个能力现在的产物」只有能力行说了算。
// 老库里指向历史内容的行因此会自然收敛到当前内容。
//
// 只动该节点名下的行；一个事务内完成（对账是「读一遍自报 + 写一批状态」，中间被插入
// 新部署时不会产生半新半旧的观测）。
func (d *DB) ReconcileNode(ctx context.Context, nodeID string, applied []AppliedCapability, now int64) (ReconcileStats, error) {
	var stats ReconcileStats
	if nodeID == "" {
		return stats, fmt.Errorf("reconcile node: empty node_id")
	}
	if now == 0 {
		now = Now()
	}
	byCapability := make(map[string]AppliedCapability, len(applied))
	for _, a := range applied {
		if a.CapabilityID == "" {
			continue
		}
		byCapability[a.CapabilityID] = a
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("reconcile node: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT d.id, d.capability_id, `+deploymentDigestSQL+`
		   FROM deployments d
		   JOIN capability_versions c ON c.id = d.capability_id
		  WHERE d.node_id = ?`, nodeID)
	if err != nil {
		return stats, fmt.Errorf("reconcile node: read deployments: %w", err)
	}
	type row struct {
		id, capabilityID, digest string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.capabilityID, &r.digest); err != nil {
			rows.Close()
			return stats, fmt.Errorf("reconcile node: scan deployment: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("reconcile node: iterate deployments: %w", err)
	}
	rows.Close()

	known := make(map[string]bool, len(all))
	for _, r := range all {
		known[r.capabilityID] = true
		state, reason := DeploymentPending, ""
		got, reported := byCapability[r.capabilityID]
		switch {
		case reported && got.Digest == r.digest:
			state = DeploymentApplied
			stats.Applied++
		case reported:
			reason = fmt.Sprintf("stale: node has %s, console expects %s — 重新下发", got.Digest, r.digest)
			stats.Stale++
		default:
			reason = "missing: node did not report this capability"
			stats.Missing++
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE deployments SET state = ?, reason = ?, updated_at = ? WHERE id = ?`,
			state, reason, now, r.id); err != nil {
			return stats, fmt.Errorf("reconcile node: mark deployment: %w", err)
		}
	}
	for capabilityID, a := range byCapability {
		if !known[capabilityID] {
			stats.Extra = append(stats.Extra, a)
		}
	}
	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("reconcile node: commit: %w", err)
	}
	return stats, nil
}

// DeploymentDelivery 是「把一份产物推给某台节点」所需的全部信息（行 + 产物原文）。
//
// 为什么单独开一个形状：deploy 帧要的是 canonical JSON（整份产物），而 Deployment
// 是给控制台看的（不带产物正文）。两者混在一个结构里会让列表接口无端背上几 KB 正文。
//
// 两个字段都从**能力行**派生（P2 起）：Digest = content_hash（期望），Spec = 由
// capability_versions.spec 拼出的整份 canonical JSON。所以「推下去的字节」与
// 「中枢说的期望 digest」永远同源——这正是节点算出来的 digest 能与中枢对上的前提。
type DeploymentDelivery struct {
	DeploymentID string
	CapabilityID string
	Capability   string
	Version      int
	Digest       string
	Spec         []byte
}

// DeploymentsForNode 返回该节点当前的部署（每个能力最多一行，由 CreateDeployment 保证）。
//
// 调用方是握手结束后的补推：节点连上来时，它缓存的 digest 如果不是中枢期望的那份，
// 就把期望的那份推下去——部署是中枢下发的，这里不做“本地变体”协商，直接覆盖。
// 同时补上了「部署时节点离线」那条路径（原来只有注释说会补推，代码里没有）。
func (d *DB) DeploymentsForNode(ctx context.Context, namespaceID, nodeID string) ([]*DeploymentDelivery, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT d.id, c.id, c.name, c.version, `+deploymentDigestSQL+`, c.spec
		   FROM deployments d
		   JOIN capability_versions c ON c.id = d.capability_id
		  WHERE d.namespace_id = ? AND d.node_id = ?
		  ORDER BY d.created_at DESC, d.id`, namespaceID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list deployments for node: %w", err)
	}
	defer rows.Close()

	out := []*DeploymentDelivery{}
	for rows.Next() {
		var it DeploymentDelivery
		var spec string
		if err := rows.Scan(&it.DeploymentID, &it.CapabilityID, &it.Capability, &it.Version, &it.Digest, &spec); err != nil {
			return nil, fmt.Errorf("scan deployment for node: %w", err)
		}
		c := &CapabilityVersion{
			ID:          it.CapabilityID,
			NamespaceID: namespaceID,
			Name:        it.Capability,
			Version:     it.Version,
		}
		if trimmed := strings.TrimSpace(spec); trimmed != "" {
			if err := json.Unmarshal([]byte(trimmed), &c.Spec); err != nil {
				return nil, fmt.Errorf("decode capability spec for deployment %s: %w", it.DeploymentID, err)
			}
		}
		// 没有 run 的行不可部署（写入期就拒了）：直接跳过而不是推一份空产物下去
		// （节点只会拒，日志还会骗人）。
		full, aerr := artifactOf(c)
		if errors.Is(aerr, ErrArtifactNotFound) {
			continue
		}
		if aerr != nil {
			return nil, aerr
		}
		it.Spec = full.Spec
		out = append(out, &it)
	}
	return out, rows.Err()
}

// deploymentRetryAfter 是节点离线时部署重推的退避（目前不落库，留给 sweeper）。
const deploymentRetryAfter = 30 * time.Second

var _ = deploymentRetryAfter // 保留给后续 sweeper 使用，避免误删常量
