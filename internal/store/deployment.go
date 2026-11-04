package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
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

// ErrArtifactNotFound 产物不存在（或不属于该命名空间）。
var ErrArtifactNotFound = errors.New("store: artifact not found")

// ErrDeploymentExists 同一 (node, artifact) 已经部署过。
var ErrDeploymentExists = errors.New("store: deployment already exists")

// Artifact 是一份能力产物的不可变快照。
type Artifact struct {
	ID           string `json:"id"`
	NamespaceID  string `json:"namespaceId"`
	CapabilityID string `json:"capabilityId"`
	Digest       string `json:"digest"`
	// Spec 是 canonical JSON（CONTRACT-artifact §2）。
	Spec      []byte `json:"spec"`
	CreatedAt int64  `json:"createdAt"`
}

// Deployment 是「某台节点应当拥有某份产物」+「节点回报的状态」。
type Deployment struct {
	ID           string `json:"id"`
	NamespaceID  string `json:"namespaceId"`
	NodeID       string `json:"nodeId"`
	ArtifactID   string `json:"artifactId"`
	CapabilityID string `json:"capabilityId"`
	Capability   string `json:"capability"`
	Version      int    `json:"version"`
	Digest       string `json:"digest"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

// CreateArtifact 登记一份产物，按 (namespace, digest) 幂等。
//
// digest 是内容寻址标识：同一 digest 永远对应同一份内容，所以重复登记是 no-op
// 而不是冲突。返回已有行（第二次调用返回第一次的行）。
func (d *DB) CreateArtifact(ctx context.Context, a *Artifact) (*Artifact, error) {
	if a.NamespaceID == "" || a.NamespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if a.CapabilityID == "" {
		return nil, fmt.Errorf("create artifact: empty capability_id")
	}
	if a.Digest == "" {
		return nil, fmt.Errorf("create artifact: empty digest")
	}
	if a.ID == "" {
		a.ID = NewArtifactID()
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = Now()
	}

	if _, err := d.ExecContext(ctx,
		`INSERT INTO artifacts (id, namespace_id, capability_id, digest, spec, created_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(namespace_id, digest) DO NOTHING`,
		a.ID, a.NamespaceID, a.CapabilityID, a.Digest, string(a.Spec), a.CreatedAt); err != nil {
		return nil, fmt.Errorf("create artifact: %w", err)
	}

	// 回读权威行：ON CONFLICT DO NOTHING 时插入的 id 是丢弃的，
	// 返回刚编的那个 id 会让调用方拿到一个不存在的行。
	return d.GetArtifactByDigest(ctx, a.NamespaceID, a.Digest)
}

// GetArtifactByDigest 按内容寻址标识取产物。
func (d *DB) GetArtifactByDigest(ctx context.Context, namespaceID, digest string) (*Artifact, error) {
	return d.scanArtifact(ctx,
		`SELECT id, namespace_id, capability_id, digest, spec, created_at
		   FROM artifacts WHERE namespace_id = ? AND digest = ?`, namespaceID, digest)
}

// GetArtifact 按 id 取产物。
func (d *DB) GetArtifact(ctx context.Context, namespaceID, id string) (*Artifact, error) {
	return d.scanArtifact(ctx,
		`SELECT id, namespace_id, capability_id, digest, spec, created_at
		   FROM artifacts WHERE namespace_id = ? AND id = ?`, namespaceID, id)
}

func (d *DB) scanArtifact(ctx context.Context, query string, args ...any) (*Artifact, error) {
	var a Artifact
	var spec string
	err := d.QueryRowContext(ctx, query, args...).Scan(
		&a.ID, &a.NamespaceID, &a.CapabilityID, &a.Digest, &spec, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get artifact: %w", err)
	}
	a.Spec = []byte(spec)
	return &a, nil
}

// ListArtifacts 返回某个能力的全部产物（新的在前）。
func (d *DB) ListArtifacts(ctx context.Context, namespaceID, capabilityID string) ([]*Artifact, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, namespace_id, capability_id, digest, spec, created_at
		   FROM artifacts WHERE namespace_id = ? AND capability_id = ?
		  ORDER BY created_at DESC, id`, namespaceID, capabilityID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()
	out := []*Artifact{}
	for rows.Next() {
		var a Artifact
		var spec string
		if err := rows.Scan(&a.ID, &a.NamespaceID, &a.CapabilityID, &a.Digest, &spec, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		a.Spec = []byte(spec)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// LatestArtifact 返回某能力最新登记的产物；没有产物时返回 ErrArtifactNotFound。
func (d *DB) LatestArtifact(ctx context.Context, namespaceID, capabilityID string) (*Artifact, error) {
	return d.scanArtifact(ctx,
		`SELECT id, namespace_id, capability_id, digest, spec, created_at
		   FROM artifacts WHERE namespace_id = ? AND capability_id = ?
		  ORDER BY created_at DESC, id LIMIT 1`, namespaceID, capabilityID)
}

// CreateDeployment 建一条部署（state=pending）。
//
// 同一 (node, capability) **只保留一行**：部署是中枢对「这台机器该有哪一份产物」的
// 期望状态，新期望直接取代旧的。
//
// 为什么不再把旧 digest 的行当“历史”留着：留着会让同一能力同时存在两条互斥的期望。
// hello 对账按行逐个比对 digest，于是旧行总是“与节点上那份一致”、新行总是“不一致”，
// 两者每次重连都互相推翻——节点会在两份产物之间来回被推。收敛的前提是
// 「一个 (机器, 能力) 只有一个期望」（见 markDeployedDigest）。
//
// 同一 (node, artifact) 已存在时是幂等 no-op：控制台重复点部署不该报错，
// 也不该重置已经 applied 的状态（否则一次误点会把绿标打回灰）。
func (d *DB) CreateDeployment(ctx context.Context, namespaceID, nodeID, artifactID string) (*Deployment, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	now := Now()
	id := NewDeploymentID()

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin create deployment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 目标产物的能力：取代范围只能是**同一能力**的旧行（别的能力不碰）。
	var capabilityID string
	err = tx.QueryRowContext(ctx,
		`SELECT capability_id FROM artifacts WHERE id = ? AND namespace_id = ?`,
		artifactID, namespaceID).Scan(&capabilityID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrArtifactNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve artifact capability: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM deployments
		  WHERE namespace_id = ? AND node_id = ? AND artifact_id <> ?
		    AND artifact_id IN (SELECT id FROM artifacts WHERE capability_id = ?)`,
		namespaceID, nodeID, artifactID, capabilityID); err != nil {
		return nil, fmt.Errorf("supersede previous deployment: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO deployments (id, namespace_id, node_id, artifact_id, digest, state, reason, created_at, updated_at)
		 SELECT ?, ?, ?, a.id, a.digest, ?, '', ?, ?
		   FROM artifacts a WHERE a.id = ? AND a.namespace_id = ?
		 ON CONFLICT(node_id, artifact_id) DO NOTHING`,
		id, namespaceID, nodeID, DeploymentPending, now, now, artifactID, namespaceID); err != nil {
		return nil, fmt.Errorf("create deployment: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create deployment: %w", err)
	}
	return d.GetDeploymentByNodeArtifact(ctx, namespaceID, nodeID, artifactID)
}

// GetDeploymentByNodeArtifact 取一条部署。
func (d *DB) GetDeploymentByNodeArtifact(ctx context.Context, namespaceID, nodeID, artifactID string) (*Deployment, error) {
	return d.scanDeployment(ctx,
		deploymentSelect+` WHERE d.namespace_id = ? AND d.node_id = ? AND d.artifact_id = ?`,
		namespaceID, nodeID, artifactID)
}

// GetDeployment 按 id 取一条部署。
func (d *DB) GetDeployment(ctx context.Context, namespaceID, id string) (*Deployment, error) {
	return d.scanDeployment(ctx, deploymentSelect+` WHERE d.namespace_id = ? AND d.id = ?`, namespaceID, id)
}

const deploymentSelect = `
SELECT d.id, d.namespace_id, d.node_id, d.artifact_id, d.digest, d.state, d.reason,
       d.created_at, d.updated_at, a.capability_id, c.name, c.version
  FROM deployments d
  JOIN artifacts a ON a.id = d.artifact_id
  JOIN capability_versions c ON c.id = a.capability_id`

func (d *DB) scanDeployment(ctx context.Context, query string, args ...any) (*Deployment, error) {
	var dp Deployment
	err := d.QueryRowContext(ctx, query, args...).Scan(
		&dp.ID, &dp.NamespaceID, &dp.NodeID, &dp.ArtifactID, &dp.Digest,
		&dp.State, &dp.Reason, &dp.CreatedAt, &dp.UpdatedAt,
		&dp.CapabilityID, &dp.Capability, &dp.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	return &dp, nil
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
	where = append(where, "d.namespace_id = ?")
	if f.NodeID != "" {
		where = append(where, "d.node_id = ?")
		args = append(args, f.NodeID)
	}
	if f.CapabilityID != "" {
		where = append(where, "a.capability_id = ?")
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
		var dp Deployment
		if err := rows.Scan(
			&dp.ID, &dp.NamespaceID, &dp.NodeID, &dp.ArtifactID, &dp.Digest,
			&dp.State, &dp.Reason, &dp.CreatedAt, &dp.UpdatedAt,
			&dp.CapabilityID, &dp.Capability, &dp.Version); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		out = append(out, &dp)
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

// markDeployedDigest 是 hello 对账的入口：拿「节点实际缓存的 digest」和中枢的期望
// 逐能力比对。
//
// 语义（中枢覆盖）：
//   - digest 一致 → applied（绿勾）
//   - 不一致   → pending，reason 里两端 digest 都写上，回来后由 handleHello
//     把中枢期望的那份推下去（节点不需要知道自己错在哪里，它只要拿到对的东西）
//
// 早先这里标 `local`（“本地变体，中枢永不覆盖”，契约 §7 的旧口径），实践中行不通：
// 部署本来就是中枢下发的，节点上那份不一致是“还没收到”而不再是“用户改过”，
// 标黄只会让一条正常的部署永远停在黄标上，还得人去点一次。
func (d *DB) markDeployedDigest(ctx context.Context, tx *sql.Tx, nodeID, capabilityID, digest string) (int, error) {
	now := Now()
	rows, err := tx.QueryContext(ctx,
		`SELECT d.id, d.digest FROM deployments d
		   JOIN artifacts a ON a.id = d.artifact_id
		  WHERE d.node_id = ? AND a.capability_id = ?`, nodeID, capabilityID)
	if err != nil {
		return 0, fmt.Errorf("query deployments for reconcile: %w", err)
	}
	defer rows.Close()

	type row struct{ id, digest string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.digest); err != nil {
			return 0, fmt.Errorf("scan deployment for reconcile: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, r := range all {
		state, reason := DeploymentPending, ""
		if r.digest == digest {
			state = DeploymentApplied
		} else {
			reason = fmt.Sprintf("digest mismatch: node has %s, console expects %s — 重新下发",
				digest, r.digest)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE deployments SET state = ?, reason = ?, updated_at = ? WHERE id = ?`,
			state, reason, now, r.id); err != nil {
			return 0, fmt.Errorf("mark deployment: %w", err)
		}
		n++
	}
	return n, nil
}

// DeploymentDelivery 是「把一份产物推给某台节点」所需的全部信息（行 + 产物原文）。
//
// 为什么单独开一个形状：deploy 帧要的是 canonical JSON（artifact.Spec），
// 而 Deployment 是给控制台看的（不带产物正文）。两者混在一个结构里会让
// 列表接口无端背上几 KB 的产物正文。
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
		`SELECT d.id, a.capability_id, c.name, c.version, d.digest, a.spec
		   FROM deployments d
		   JOIN artifacts a ON a.id = d.artifact_id
		   JOIN capability_versions c ON c.id = a.capability_id
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
		it.Spec = []byte(spec)
		out = append(out, &it)
	}
	return out, rows.Err()
}

// deploymentRetryAfter 是节点离线时部署重推的退避（目前不落库，留给 sweeper）。
const deploymentRetryAfter = 30 * time.Second

var _ = deploymentRetryAfter // 保留给后续 sweeper 使用，避免误删常量
