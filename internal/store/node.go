package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 节点状态（契约 §2.3 nodes.status）。
const (
	NodeStatusOnline   = "online"
	NodeStatusOffline  = "offline"
	NodeStatusDraining = "draining"
)

// 节点默认硬顶（契约 §2.3）。
const (
	DefaultMaxInstances   = 20
	DefaultMaxConcurrency = 8
)

// Node 是 nodes 表的一行（一台机器）。
//
// TokenHash 是 sha256(token)，明文只在创建时返回一次（契约 §10.3）；
// 所以它带 `json:"-"`，任何 API 响应都不会泄漏哈希。
type Node struct {
	ID             string   `json:"id"`
	OrgID          string   `json:"orgId"`
	Name           string   `json:"name"`
	Tags           []string `json:"tags"`
	Status         string   `json:"status"`
	TokenHash      string   `json:"-"`
	MaxInstances   int      `json:"maxInstances"`
	MaxConcurrency int      `json:"maxConcurrency"`
	LastSeenAt     *int64   `json:"lastSeenAt"`
	CreatedAt      int64    `json:"createdAt"`
}

// NodeInstance 是 node_instances 表的一行（并发槽位，由节点申报）。
type NodeInstance struct {
	ID          string  `json:"id"`
	NodeID      string  `json:"nodeId"`
	Slot        int     `json:"slot"`
	Concurrency int     `json:"concurrency"`
	BusyTask    *string `json:"busyTask"`
	Alive       bool    `json:"alive"`
	UpdatedAt   int64   `json:"updatedAt"`
}

// NodePin 是 node_pins 表的一行（粘性路由）。
type NodePin struct {
	StickyKey  string `json:"stickyKey"`
	OrgID      string `json:"orgId"`
	NodeID     string `json:"nodeId"`
	Selector   string `json:"selector"`
	CreatedAt  int64  `json:"createdAt"`
	LastUsedAt int64  `json:"lastUsedAt"`
}

// 节点注册 / 派发相关的哨兵错误。
var (
	// ErrUnknownCapability 节点声明了中枢不存在的能力名@版本（不变量 I2 / A3）。
	ErrUnknownCapability = errors.New("unknown_capability")
	// ErrTooManyInstances 申报实例数超过 nodes.max_instances 硬顶（契约 Q5）。
	ErrTooManyInstances = errors.New("too_many_instances")
	// ErrNoCapacity 节点没有空闲并发槽位。
	ErrNoCapacity = errors.New("no_capacity")
	// ErrNodeDraining 节点在排空（不再接新任务）。
	ErrNodeDraining = errors.New("node_draining")
)

// CapabilityRef 是节点声明自己具备的能力（只有 name + version，没有命令）。
type CapabilityRef struct {
	Name    string `json:"capability"`
	Version int    `json:"version"`
}

// InstanceSpec 是节点申报的并发槽位（hello 帧的 instances 字段，契约 Q5）。
type InstanceSpec struct {
	Slot        int `json:"slot"`
	Concurrency int `json:"concurrency"`
}

// NewNodeToken 生成节点 opaque token：ant_node_<base64url(24B)>（契约 §10.3，
// 与既有 api_keys 形态一致）。返回 (明文, sha256 哈希)。
func NewNodeToken() (token, hash string, err error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("generate node token: %w", err)
	}
	token = "ant_node_" + base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashNodeToken(token), nil
}

// HashNodeToken 计算 token 的 sha256（DB 只存哈希，明文只在创建时返回一次）。
func HashNodeToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateNode 创建节点，返回节点与**只出现一次**的明文 token。
//
// n.TokenHash 可预先由调用方设置（用于测试注入已知 token）；为空时自动生成。
func (d *DB) CreateNode(ctx context.Context, n *Node) (token string, err error) {
	if n == nil {
		return "", fmt.Errorf("create node: nil")
	}
	if n.OrgID == "" {
		return "", fmt.Errorf("create node: empty org_id")
	}
	if n.Name == "" {
		return "", fmt.Errorf("create node: empty name")
	}
	if n.TokenHash == "" {
		token, n.TokenHash, err = NewNodeToken()
		if err != nil {
			return "", err
		}
	}
	if n.MaxInstances <= 0 {
		n.MaxInstances = DefaultMaxInstances
	}
	if n.MaxConcurrency <= 0 {
		n.MaxConcurrency = DefaultMaxConcurrency
	}
	if n.Status == "" {
		n.Status = NodeStatusOffline
	}
	if n.CreatedAt == 0 {
		n.CreatedAt = Now()
	}
	if n.Tags == nil {
		n.Tags = []string{}
	}
	tagsJSON, err := json.Marshal(n.Tags)
	if err != nil {
		return "", fmt.Errorf("marshal node tags: %w", err)
	}
	if n.ID == "" {
		n.ID = NewNodeID()
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO nodes
		   (id, org_id, name, tags, status, token_hash, max_instances, max_concurrency, last_seen_at, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		n.ID, n.OrgID, n.Name, string(tagsJSON), n.Status, n.TokenHash,
		n.MaxInstances, n.MaxConcurrency, n.LastSeenAt, n.CreatedAt); err != nil {
		return "", fmt.Errorf("insert node: %w", err)
	}
	return token, nil
}

// nodeColumns 是统一的列顺序，scanNode 依赖它。
const nodeColumns = `id, org_id, name, tags, status, token_hash,
	max_instances, max_concurrency, last_seen_at, created_at`

// scanNodeRow 从 rows 解析一行。
func scanNodeRow(rows *sql.Rows) (*Node, error) {
	var n Node
	var tags string
	if err := rows.Scan(&n.ID, &n.OrgID, &n.Name, &tags, &n.Status, &n.TokenHash,
		&n.MaxInstances, &n.MaxConcurrency, &n.LastSeenAt, &n.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan node: %w", err)
	}
	if err := json.Unmarshal([]byte(tags), &n.Tags); err != nil {
		return nil, fmt.Errorf("decode node tags: %w", err)
	}
	if n.Tags == nil {
		n.Tags = []string{} // 不变量 I9
	}
	return &n, nil
}

// GetNodeByTokenHash 按 token sha256 取节点（握手鉴权用，契约 §10.3）。
//
// 这是唯一一个「不知道 org 也能查」的节点查询：token 自带身份。
// 拿到 (node_id, org_id) 之后，该连接上所有操作都用这两个值，不再查库。
func (d *DB) GetNodeByTokenHash(ctx context.Context, tokenHash string) (*Node, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return nil, fmt.Errorf("get node by token hash: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get node by token hash: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanNodeRow(rows)
}

// GetNode 按 (orgID, id) 取节点（不变量 I10：必须带 org）。
func (d *DB) GetNode(ctx context.Context, orgID, nodeID string) (*Node, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE org_id = ? AND id = ?`, orgID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("get node: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get node: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanNodeRow(rows)
}

// GetNodeByName 按 (orgID, name) 取节点；未命中返回 ErrNotFound。
func (d *DB) GetNodeByName(ctx context.Context, orgID, name string) (*Node, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE org_id = ? AND name = ?`, orgID, name)
	if err != nil {
		return nil, fmt.Errorf("get node by name: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get node by name: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanNodeRow(rows)
}

// ListNodes 列出组织内节点（空列表返回 []）。
func (d *DB) ListNodes(ctx context.Context, orgID string) ([]*Node, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE org_id = ? ORDER BY name, id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	out := []*Node{}
	for rows.Next() {
		n, err := scanNodeRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UpdateNodeStatus 改节点状态（online/offline/draining），并刷新 last_seen_at。
func (d *DB) UpdateNodeStatus(ctx context.Context, orgID, nodeID, status string) error {
	switch status {
	case NodeStatusOnline, NodeStatusOffline, NodeStatusDraining:
	default:
		return fmt.Errorf("update node status: invalid status %q", status)
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET status = ?, last_seen_at = ? WHERE org_id = ? AND id = ?`,
		status, Now(), orgID, nodeID)
	if err != nil {
		return fmt.Errorf("update node status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update node status rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchNode 刷新 last_seen_at（任何上行帧都续命，契约 §3.3）。
func (d *DB) TouchNode(ctx context.Context, orgID, nodeID string, at int64) error {
	if at == 0 {
		at = Now()
	}
	if _, err := d.ExecContext(ctx,
		`UPDATE nodes SET last_seen_at = ? WHERE org_id = ? AND id = ?`,
		at, orgID, nodeID); err != nil {
		return fmt.Errorf("touch node: %w", err)
	}
	return nil
}

// ConfirmCapabilities 校验并写入节点声明的能力（契约 Q5 / 不变量 I2 / A3）。
//
// 声明的 name@version 必须在中枢已定义，否则返回 ErrUnknownCapability；
// 写入 node_capabilities 时只存 (node_id, capability_id)——没有命令、没有出口、没有 kind。
//
// 语义是**全量替换**：节点这次声明什么，就把它之前的声明全换掉。
// （节点升级后不再支持某能力，不该还留在表里被派发选中。）
// 返回解析出的 capability_id 列表（顺序与入参一致）。
func (d *DB) ConfirmCapabilities(ctx context.Context, orgID, nodeID string, caps []CapabilityRef) ([]string, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("confirm capabilities: empty node_id")
	}
	// 先在事务外解析全部能力，任何一个不认识就整体拒绝（不留半份声明）
	ids := make([]string, 0, len(caps))
	for _, ref := range caps {
		if ref.Name == "" {
			return nil, fmt.Errorf("%w: empty capability name", ErrUnknownCapability)
		}
		c, err := d.GetCapability(ctx, orgID, ref.Name, ref.Version)
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s@%d", ErrUnknownCapability, ref.Name, ref.Version)
		}
		if err != nil {
			return nil, err
		}
		ids = append(ids, c.ID)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin confirm capabilities: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM node_capabilities WHERE node_id = ?`, nodeID); err != nil {
		return nil, fmt.Errorf("clear node capabilities: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_capabilities (node_id, capability_id) VALUES (?,?)
			 ON CONFLICT(node_id, capability_id) DO NOTHING`, nodeID, id); err != nil {
			return nil, fmt.Errorf("insert node capability: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit confirm capabilities: %w", err)
	}
	return ids, nil
}

// ListNodeCapabilities 返回节点声明的能力定义（含 outputs，供 kind 推导与派发选点）。
func (d *DB) ListNodeCapabilities(ctx context.Context, nodeID string) ([]*CapabilityVersion, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+prefixedCapabilityColumns("c")+`
		 FROM node_capabilities nc
		 JOIN capability_versions c ON c.id = nc.capability_id
		 WHERE nc.node_id = ?
		 ORDER BY c.name, c.version`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list node capabilities: %w", err)
	}
	defer rows.Close()
	out := []*CapabilityVersion{}
	for rows.Next() {
		var c CapabilityVersion
		var paramsSchema, outputs, lifecycle string
		if err := rows.Scan(&c.ID, &c.OrgID, &c.Name, &c.Version, &paramsSchema, &outputs,
			&lifecycle, &c.Sticky, &c.Source, &c.ContentHash, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan node capability: %w", err)
		}
		if err := json.Unmarshal([]byte(paramsSchema), &c.ParamsSchema); err != nil {
			return nil, fmt.Errorf("decode params_schema: %w", err)
		}
		if err := json.Unmarshal([]byte(outputs), &c.Outputs); err != nil {
			return nil, fmt.Errorf("decode outputs: %w", err)
		}
		if c.Outputs == nil {
			c.Outputs = []string{}
		}
		if err := json.Unmarshal([]byte(lifecycle), &c.Lifecycle); err != nil {
			return nil, fmt.Errorf("decode lifecycle: %w", err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// prefixedCapabilityColumns 给 capabilityColumns 的每一列加表别名前缀，
// 避免与 join 表的同名列（id/org_id）混淆。
func prefixedCapabilityColumns(alias string) string {
	cols := []string{"id", "org_id", "name", "version", "params_schema", "outputs",
		"lifecycle", "sticky", "source", "content_hash", "created_at"}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = alias + "." + c
	}
	return strings.Join(parts, ", ")
}

// NodesForCapability 返回声明了该能力、且可接单的节点 ID（派发第 ③ 步的唯一查表入口）。
//
// 「可接单」= 节点状态不是 offline/draining。排序按 last_seen_at 新→旧，
// 让调用方（路由层）能在同分时偏向最近活跃的机器。
func (d *DB) NodesForCapability(ctx context.Context, orgID, capabilityID string) ([]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT n.id
		 FROM node_capabilities nc
		 JOIN nodes n ON n.id = nc.node_id
		 WHERE nc.capability_id = ? AND n.org_id = ?
		   AND n.status IN (?, ?)
		 ORDER BY COALESCE(n.last_seen_at, 0) DESC, n.id`,
		capabilityID, orgID, NodeStatusOnline, NodeStatusOffline)
	if err != nil {
		return nil, fmt.Errorf("nodes for capability: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan node id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// NodesDeclaringCapability 返回声明了该能力的**所有**节点（含 offline / draining）。
//
// 与 NodesForCapability 的区别是**用途**，不是实现细节：
//
//	NodesForCapability    派发入口 —— 只给现在能接单的机器
//	NodesDeclaringCapability  控制台展示 —— "谁声明了这个能力"是配置事实，
//	                          排空中的机器仍然声明着它，页面上必须看得到，
//	                          否则运维会以为这个能力没人管了
//
// 排序按状态（在线优先）+ 名字，让列表读起来稳定。
func (d *DB) NodesDeclaringCapability(ctx context.Context, orgID, capabilityID string) ([]*Node, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+nodeColumns+`
		 FROM node_capabilities nc
		 JOIN nodes n ON n.id = nc.node_id
		 WHERE nc.capability_id = ? AND n.org_id = ?
		 ORDER BY CASE n.status WHEN 'online' THEN 0 WHEN 'draining' THEN 1 ELSE 2 END,
		          n.name, n.id`,
		capabilityID, orgID)
	if err != nil {
		return nil, fmt.Errorf("nodes declaring capability: %w", err)
	}
	defer rows.Close()
	out := []*Node{}
	for rows.Next() {
		n, err := scanNodeRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ErrNoCapacity 上面的哨兵错误。

// UpsertInstances 落库节点申报的实例槽位；超过 nodes.max_instances 返回 ErrTooManyInstances。
//
// 语义：节点每次 hello 申报的是一份**完整清单**，多余的老槽位删除（节点缩容）。
// slot 必须唯一且 ≥ 0，concurrency 至少 1。
func (d *DB) UpsertInstances(ctx context.Context, orgID, nodeID string, specs []InstanceSpec) ([]*NodeInstance, error) {
	node, err := d.GetNode(ctx, orgID, nodeID)
	if err != nil {
		return nil, err
	}
	if len(specs) > node.MaxInstances {
		return nil, fmt.Errorf("%w: declared %d instances, node max is %d",
			ErrTooManyInstances, len(specs), node.MaxInstances)
	}
	seen := make(map[int]bool, len(specs))
	for _, s := range specs {
		if s.Slot < 0 {
			return nil, fmt.Errorf("upsert instances: negative slot %d", s.Slot)
		}
		if seen[s.Slot] {
			return nil, fmt.Errorf("upsert instances: duplicate slot %d", s.Slot)
		}
		seen[s.Slot] = true
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin upsert instances: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 删掉本次没申报的旧槽位（节点缩容）
	if len(specs) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM node_instances WHERE node_id = ?`, nodeID); err != nil {
			return nil, fmt.Errorf("clear instances: %w", err)
		}
	} else {
		placeholders := make([]string, len(specs))
		args := make([]any, 0, len(specs)+1)
		args = append(args, nodeID)
		for i, s := range specs {
			placeholders[i] = "?"
			args = append(args, s.Slot)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM node_instances WHERE node_id = ? AND slot NOT IN (`+
				strings.Join(placeholders, ",")+`)`, args...); err != nil {
			return nil, fmt.Errorf("remove stale instances: %w", err)
		}
	}

	now := Now()
	for _, s := range specs {
		conc := s.Concurrency
		if conc <= 0 {
			conc = 1
		}
		// 已有槽位保留 busy_task（重启不该把正在跑的任务丢掉），只更新并发与存活标记
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_instances (id, node_id, slot, concurrency, busy_task, alive, updated_at)
			 VALUES (?,?,?,?,NULL,1,?)
			 ON CONFLICT(node_id, slot) DO UPDATE SET
			   concurrency = excluded.concurrency,
			   alive = 1,
			   updated_at = excluded.updated_at`,
			NewInstanceID(), nodeID, s.Slot, conc, now); err != nil {
			return nil, fmt.Errorf("upsert instance slot %d: %w", s.Slot, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit upsert instances: %w", err)
	}
	return d.ListInstances(ctx, nodeID)
}

// ListInstances 列出节点实例（空列表返回 []）。
// FreeSlotsByNode 一次查出多个节点的空闲槽位数（#145：消掉派发热路径上的 N+1）。
//
// 语义必须与 ListInstances 的口径一致：**alive 且 busy_task IS NULL** 的槽位算空闲。
// 用 LEFT JOIN 而不是 INNER：没申报槽位的节点仍要出现在结果里（free=0），
// 否则调用方会把它当成"查不到"而丢掉——它只是优先级最低，仍然可以接派发。
func (d *DB) FreeSlotsByNode(ctx context.Context, nodeIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if _, seen := out[id]; seen {
			continue
		}
		out[id] = 0 // 先给 0：LEFT JOIN 没匹配到时不会出现在结果里
		args = append(args, id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := d.QueryContext(ctx,
		`SELECT n.id, COUNT(i.id)
		   FROM nodes n
		   LEFT JOIN node_instances i
		     ON i.node_id = n.id AND i.alive = 1 AND i.busy_task IS NULL
		  WHERE n.id IN (`+placeholders+`)
		  GROUP BY n.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("free slots by node: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var free int
		if err := rows.Scan(&id, &free); err != nil {
			return nil, fmt.Errorf("scan free slots: %w", err)
		}
		out[id] = free
	}
	return out, rows.Err()
}

func (d *DB) ListInstances(ctx context.Context, nodeID string) ([]*NodeInstance, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, node_id, slot, concurrency, busy_task, alive, updated_at
		 FROM node_instances WHERE node_id = ? ORDER BY slot`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer rows.Close()
	out := []*NodeInstance{}
	for rows.Next() {
		var in NodeInstance
		var alive int
		if err := rows.Scan(&in.ID, &in.NodeID, &in.Slot, &in.Concurrency, &in.BusyTask, &alive, &in.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan instance: %w", err)
		}
		in.Alive = alive != 0
		out = append(out, &in)
	}
	return out, rows.Err()
}

// MarkInstancesDead 把节点全部实例标记为掉线（连接断开时调用）。
//
// 只是标记 alive=0，不删行：槽位申报是节点的事，中枢不替它决定「少了一个槽」。
// 也不清 busy_task——任务归属由租约巡检（SweepExpiredLeases）回收，那是唯一入口。
func (d *DB) MarkInstancesDead(ctx context.Context, nodeID string) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE node_instances SET alive = 0, updated_at = ? WHERE node_id = ?`,
		Now(), nodeID); err != nil {
		return fmt.Errorf("mark instances dead: %w", err)
	}
	return nil
}

// LeaseInstance 把任务占到一个空闲实例上（CAS：busy_task IS NULL → taskID）。
//
// 「实际并发 = min(实例 concurrency, 实现级 concurrency, nodes.max_concurrency)」
// 由调用方（路由层）算，这里只做最后一道 CAS 抢占：并发 UPDATE 下只有一个能成功。
// 返回 instance_id；没有空闲槽位返回 ErrNoCapacity。
func (d *DB) LeaseInstance(ctx context.Context, nodeID, taskID string) (string, error) {
	if taskID == "" {
		return "", fmt.Errorf("lease instance: empty task_id")
	}
	// 先找最早的存活空闲槽位（ORDER BY slot 保证确定性），再由 CAS 更新抢占。
	// SQLite 单写 + 单连接使这两步不会与另一个写者交错；
	// 跨连接场景下 CAS 失败会重试下一轮，不会出现两个任务占同一槽。
	for attempt := 0; attempt < 3; attempt++ {
		var instanceID string
		err := d.QueryRowContext(ctx,
			`SELECT id FROM node_instances
			 WHERE node_id = ? AND alive = 1 AND busy_task IS NULL
			 ORDER BY slot LIMIT 1`, nodeID).Scan(&instanceID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNoCapacity
		}
		if err != nil {
			return "", fmt.Errorf("find free instance: %w", err)
		}
		res, err := d.ExecContext(ctx,
			`UPDATE node_instances SET busy_task = ?, updated_at = ?
			 WHERE id = ? AND busy_task IS NULL`, taskID, Now(), instanceID)
		if err != nil {
			return "", fmt.Errorf("lease instance: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return "", fmt.Errorf("lease instance rows: %w", err)
		}
		if n == 1 {
			return instanceID, nil
		}
		// CAS 失败：被另一个写者抢了，重试找下一个空闲槽
	}
	return "", ErrNoCapacity
}

// LeaseAndAssign 是派发的**一个事务**：占槽位 + pending→assigned + 写归属/attempts（#141）。
//
// 合并前派发要 3 次提交（LeaseInstance 的占槽 UPDATE 一次、AssignTask 里的
// Transition 一次、写归属/attempts 一次），中间失败会出现两种
// 脏状态：槽位占了任务却没派出去（槽位泄漏，要等巡检才收回），或任务已 assigned
// 而 node_id 还空着（调用方必须自己补 ReleaseInstance 才不留坑）。
//
// 顺序与语义都和拆分版一致，只是原子化：
//  1. 找最早的存活空闲槽位（口径与 LeaseInstance 完全一致）
//  2. CAS 占位（busy_task IS NULL → taskID）；RowsAffected==0 → ErrNoCapacity
//  3. transitionGuarded(pending → assigned)（状态写唯一入口）
//  4. 写 node/instance/lease_until 并 attempts+1
//
// 任一步失败都整体回滚——槽位不泄漏，attempts 也不会被提前 +1。
func (d *DB) LeaseAndAssign(ctx context.Context, orgID, taskID, nodeID string, leaseUntil int64) (instanceID string, err error) {
	if taskID == "" {
		// 与 LeaseInstance 的参数校验口径一致：先拒绝，连事务都不开（不占槽）
		return "", fmt.Errorf("lease and assign: empty task_id")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin lease and assign: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// ① 空闲槽位：ORDER BY slot 保证确定性，且必须走 tx
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM node_instances
		 WHERE node_id = ? AND alive = 1 AND busy_task IS NULL
		 ORDER BY slot LIMIT 1`, nodeID).Scan(&instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoCapacity
	}
	if err != nil {
		return "", fmt.Errorf("lease and assign: find free instance: %w", err)
	}

	// ② CAS 占位：并发 UPDATE 下只有一个能成功。
	// 本进程内 MaxOpenConns(1) + 事务内单写者让它不可达，但条件必须留着——
	// 它是放开连接数/跨进程场景下的最后一道闸。
	res, err := tx.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = ?, updated_at = ?
		 WHERE id = ? AND busy_task IS NULL`, taskID, Now(), instanceID)
	if err != nil {
		return "", fmt.Errorf("lease and assign: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("lease and assign rows: %w", err)
	}
	if n == 0 {
		return "", ErrNoCapacity
	}

	// ③ 改状态（唯一写入口）+ ④ 写归属/attempts
	if err := d.assignTaskTx(ctx, tx, orgID, taskID, nodeID, instanceID, leaseUntil); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit lease and assign: %w", err)
	}
	return instanceID, nil
}

// ReleaseInstance 释放实例占用（任务结束/回收时调用）。
func (d *DB) ReleaseInstance(ctx context.Context, nodeID, instanceID string) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = NULL, updated_at = ?
		 WHERE node_id = ? AND id = ?`, Now(), nodeID, instanceID); err != nil {
		return fmt.Errorf("release instance: %w", err)
	}
	return nil
}

// ReleaseInstanceByTask 按任务 ID 释放它占用的槽位（不知道 instance_id 时用）。
func (d *DB) ReleaseInstanceByTask(ctx context.Context, nodeID, taskID string) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE node_instances SET busy_task = NULL, updated_at = ?
		 WHERE node_id = ? AND busy_task = ?`, Now(), nodeID, taskID); err != nil {
		return fmt.Errorf("release instance by task: %w", err)
	}
	return nil
}

// ---------- 粘性路由 ----------

// StickyKey 计算粘性键：sha256(trace_id ∥ capability_id ∥ selector)。
//
// 同一 trace 里同能力+同选择器永远落到同一台机器（例如同一个 worktree）。
// 用 \x00 分隔，避免拼接歧义（("ab","c") 与 ("a","bc")）。
func StickyKey(traceID, capabilityID, selector string) string {
	sum := sha256.Sum256([]byte(traceID + "\x00" + capabilityID + "\x00" + selector))
	return hex.EncodeToString(sum[:])
}

// GetPin 按 sticky_key 取粘性钉住；未命中返回 ErrNotFound。
func (d *DB) GetPin(ctx context.Context, stickyKey string) (*NodePin, error) {
	var p NodePin
	err := d.QueryRowContext(ctx,
		`SELECT sticky_key, org_id, node_id, selector, created_at, last_used_at
		 FROM node_pins WHERE sticky_key = ?`, stickyKey).
		Scan(&p.StickyKey, &p.OrgID, &p.NodeID, &p.Selector, &p.CreatedAt, &p.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get pin: %w", err)
	}
	return &p, nil
}

// PutPin 写入（或刷新）粘性钉住。已存在时更新 last_used_at（钉住不换机器）。
func (d *DB) PutPin(ctx context.Context, p *NodePin) error {
	if p == nil || p.StickyKey == "" || p.OrgID == "" || p.NodeID == "" {
		return fmt.Errorf("put pin: sticky_key/org_id/node_id required")
	}
	now := Now()
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	if p.LastUsedAt == 0 {
		p.LastUsedAt = now
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO node_pins (sticky_key, org_id, node_id, selector, created_at, last_used_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(sticky_key) DO UPDATE SET last_used_at = excluded.last_used_at`,
		p.StickyKey, p.OrgID, p.NodeID, p.Selector, p.CreatedAt, p.LastUsedAt); err != nil {
		return fmt.Errorf("put pin: %w", err)
	}
	return nil
}

// DeletePin 删除粘性钉住（节点下线 / 换机器时用）。
func (d *DB) DeletePin(ctx context.Context, stickyKey string) error {
	if _, err := d.ExecContext(ctx, `DELETE FROM node_pins WHERE sticky_key = ?`, stickyKey); err != nil {
		return fmt.Errorf("delete pin: %w", err)
	}
	return nil
}

// DeletePinsForNode 清掉某节点的全部钉住（节点下线/排空时用）。
func (d *DB) DeletePinsForNode(ctx context.Context, nodeID string) error {
	if _, err := d.ExecContext(ctx, `DELETE FROM node_pins WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("delete pins for node: %w", err)
	}
	return nil
}

// SetNodeTokenHash 覆盖节点的 token 哈希（仅用于开发工具重建节点凭据）。
//
// 明文 token 只在 CreateNode 时返回一次，之后无法复原。开发场景（devseed
// 反复跑）需要一个"同一个节点、换一把钥匙"的入口，否则只能不断新建节点行，
// 留下大量永远离线的死节点。
func (d *DB) SetNodeTokenHash(ctx context.Context, nodeID, tokenHash string) error {
	if nodeID == "" || tokenHash == "" {
		return fmt.Errorf("set node token hash: empty node_id or hash")
	}
	res, err := d.ExecContext(ctx,
		`UPDATE nodes SET token_hash = ? WHERE id = ?`, tokenHash, nodeID)
	if err != nil {
		return fmt.Errorf("update node token hash: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("set node token hash: %w", ErrNotFound)
	}
	return nil
}
