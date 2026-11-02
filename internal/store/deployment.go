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
	// DeploymentLocal 节点上这个能力的 digest 与中枢期望不一致 = 本地变体。
	// 中枢永不覆盖它（契约 §7），UI 必须标黄而不是打绿勾。
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
// 同一 (node, artifact) 已存在时是**幂等 no-op**：控制台重复点部署不该报错，
// 但也不该重置已经 applied 的状态（否则一次误点会把绿标打回灰）。
//
// 同一节点对同一能力部署一个新 digest 时，旧 digest 的行保留（它是历史），
// 但只有新行参与 node_capabilities 派生 —— 由 ReconcileApplied 按节点实际
// 缓存的 digest 决定哪一行是 applied。
func (d *DB) CreateDeployment(ctx context.Context, namespaceID, nodeID, artifactID string) (*Deployment, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	now := Now()
	id := NewDeploymentID()

	if _, err := d.ExecContext(ctx,
		`INSERT INTO deployments (id, namespace_id, node_id, artifact_id, digest, state, reason, created_at, updated_at)
		 SELECT ?, ?, ?, a.id, a.digest, ?, '', ?, ?
		   FROM artifacts a WHERE a.id = ? AND a.namespace_id = ?
		 ON CONFLICT(node_id, artifact_id) DO NOTHING`,
		id, namespaceID, nodeID, DeploymentPending, now, now, artifactID, namespaceID); err != nil {
		return nil, fmt.Errorf("create deployment: %w", err)
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
	if state != DeploymentApplied && state != DeploymentFailed && state != DeploymentLocal {
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

// MarkDeployAppliedByDigest 是 hello 对账用的便捷路径：按 (node, capability, digest)
// 把匹配的部署标成 applied、不匹配的标成 local。
//
// 返回被标记的行数。
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
		state, reason := DeploymentLocal, ""
		if r.digest == digest {
			state = DeploymentApplied
		} else {
			reason = fmt.Sprintf("digest mismatch: node has %s, console expects %s", digest, r.digest)
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

// PendingDeploymentsForNode 返回某节点上所有 pending 的行（节点上线时补推用）。
func (d *DB) PendingDeploymentsForNode(ctx context.Context, namespaceID, nodeID string) ([]*Deployment, error) {
	return d.ListDeployments(ctx, namespaceID, DeploymentFilter{NodeID: nodeID})
}

// deploymentRetryAfter 是节点离线时部署重推的退避（目前不落库，留给 sweeper）。
const deploymentRetryAfter = 30 * time.Second

var _ = deploymentRetryAfter // 保留给后续 sweeper 使用，避免误删常量
