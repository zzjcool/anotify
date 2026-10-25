package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Edge 是 edges 表的一行：出口表是路由的唯一依据（契约 §2.4）。
//
// isOriginCapability 判断某个能力定义是不是内置起点。
//
// 起点没有节点声明（kind=origin），出口是「任意事件类型」。
// 注意这里按 name+version 查内置表，而不是看 outputs——
// 用户自定义的、恰好没有 outputs 的能力**不应该**获得通配出口。
func isOriginCapability(c *CapabilityVersion) bool {
	if c == nil {
		return false
	}
	bc, ok := IsBuiltinCapability(c.Name, c.Version)
	return ok && bc.Origin
}

// 一行 = 一条边；同一 (FromCapability, FromPort) 可有多行 → 扇出（不变量 I7：
// 两端都是 capability_id，图里没有机器）。
type Edge struct {
	OrgID          string `json:"orgId"`
	FromCapability string `json:"fromCapability"`
	FromPort       string `json:"fromPort"`
	ToCapability   string `json:"toCapability"`
	CreatedAt      int64  `json:"createdAt"`
}

// ErrEdgeRejected 表示边被配置期校验拒绝（出口未定义 / 跨组织）。
var ErrEdgeRejected = errors.New("edge_rejected")

// GraphVersion 是 graph_versions 表的一行：按需落版的图快照（契约 §10.2）。
type GraphVersion struct {
	ID          string          `json:"id"`
	OrgID       string          `json:"orgId"`
	ContentHash string          `json:"contentHash"`
	Snapshot    json.RawMessage `json:"snapshot"`
	CreatedAt   int64           `json:"createdAt"`
}

// CreateEdge 新增一条边。
//
// 配置期校验（契约 §6.2 / 不变量 I2 / A5）：
//   - from_port 必须 ∈ from 能力的 outputs（封闭集合，不能凭空发明出口）
//   - 两端能力必须同组织（不放跨组织边，契约 §1.2）
//
// 违反返回 ErrEdgeRejected。重复边幂等（PK 冲突视为已存在）。
func (d *DB) CreateEdge(ctx context.Context, e *Edge) error {
	if e == nil {
		return fmt.Errorf("create edge: nil")
	}
	if e.OrgID == "" || e.FromCapability == "" || e.FromPort == "" || e.ToCapability == "" {
		return fmt.Errorf("%w: org/from/port/to all required", ErrEdgeRejected)
	}
	from, err := d.GetCapabilityByID(ctx, e.OrgID, e.FromCapability)
	if err != nil {
		return fmt.Errorf("%w: from capability %q: %v", ErrEdgeRejected, e.FromCapability, err)
	}
	to, err := d.GetCapabilityByID(ctx, e.OrgID, e.ToCapability)
	if err != nil {
		return fmt.Errorf("%w: to capability %q: %v", ErrEdgeRejected, e.ToCapability, err)
	}
	// 双保险：GetCapabilityByID 已经带了 org 过滤，这里再断言一次
	// （防止将来有人改成不带 org 的查询而不自知）
	if from.OrgID != e.OrgID || to.OrgID != e.OrgID {
		return fmt.Errorf("%w: cross-org edge not allowed", ErrEdgeRejected)
	}
	// 出口封闭集合（不变量 I2）：from_port 必须属于该能力的 outputs。
	//
	// 唯一例外是**内置起点**（github/cron/http/manual）：它的出口就是
	// "任意事件类型"，不可能预先枚举。所以对起点只要求非空。
	//
	// 这没有放松 I2 的本意——I2 要防的是「凭空发明下游口子」，
	// 而起点的事件类型本来就由外部系统决定（GitHub 发什么 type 就是什么）。
	if !isOriginCapability(from) && !from.HasOutput(e.FromPort) {
		return fmt.Errorf("%w: port %q not in %s@%d outputs %v",
			ErrEdgeRejected, e.FromPort, from.Name, from.Version, from.Outputs)
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = Now()
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO edges (org_id, from_capability, from_port, to_capability, created_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(org_id, from_capability, from_port, to_capability) DO NOTHING`,
		e.OrgID, e.FromCapability, e.FromPort, e.ToCapability, e.CreatedAt); err != nil {
		return fmt.Errorf("insert edge: %w", err)
	}
	return nil
}

// DeleteEdge 删除一条边。边不存在返回 ErrNotFound。
func (d *DB) DeleteEdge(ctx context.Context, orgID, fromCapability, fromPort, toCapability string) error {
	res, err := d.ExecContext(ctx,
		`DELETE FROM edges
		 WHERE org_id = ? AND from_capability = ? AND from_port = ? AND to_capability = ?`,
		orgID, fromCapability, fromPort, toCapability)
	if err != nil {
		return fmt.Errorf("delete edge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete edge rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListEdges 列出组织的边（fromCapability 非空时按它过滤）；空列表返回 []。
func (d *DB) ListEdges(ctx context.Context, orgID, fromCapability string) ([]*Edge, error) {
	q := `SELECT org_id, from_capability, from_port, to_capability, created_at FROM edges WHERE org_id = ?`
	args := []any{orgID}
	if fromCapability != "" {
		q += ` AND from_capability = ?`
		args = append(args, fromCapability)
	}
	q += ` ORDER BY from_capability, from_port, to_capability`
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list edges: %w", err)
	}
	defer rows.Close()
	out := []*Edge{}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.OrgID, &e.FromCapability, &e.FromPort, &e.ToCapability, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// EdgesFrom 是派发第 ① 步的**唯一**查表入口（契约 §4）：
// 返回 (orgID, fromCapability, fromPort) 的全部下游 capability_id。
//
// 除了 route 包，其他包不得再写 `FROM edges`（见 #56 的验收：全仓只允许 1 处）。
func (d *DB) EdgesFrom(ctx context.Context, orgID, fromCapability, fromPort string) ([]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT to_capability FROM edges
		 WHERE org_id = ? AND from_capability = ? AND from_port = ?
		 ORDER BY to_capability`, orgID, fromCapability, fromPort)
	if err != nil {
		return nil, fmt.Errorf("lookup edges: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			return nil, fmt.Errorf("scan edge target: %w", err)
		}
		out = append(out, to)
	}
	return out, rows.Err()
}

// graphSnapshot 是 graph_versions.snapshot 的规范化结构。
type graphSnapshot struct {
	Edges []graphEdge `json:"edges"`
}

type graphEdge struct {
	From string `json:"from"`
	Port string `json:"port"`
	To   string `json:"to"`
}

// graphContentHash 计算当前图的 content_hash：全部边排序后取 sha256。
//
// 排序是关键：边的插入顺序不改变图的语义，同一份图必须得同一个 hash，
// 否则只会在 graph_versions 里堆垃圾（契约 §10.2 的「按需落版」就失效了）。
func graphContentHash(edges []*Edge) (string, error) {
	snap := graphSnapshot{Edges: make([]graphEdge, 0, len(edges))}
	for _, e := range edges {
		snap.Edges = append(snap.Edges, graphEdge{From: e.FromCapability, Port: e.FromPort, To: e.ToCapability})
	}
	sort.Slice(snap.Edges, func(i, j int) bool {
		a, b := snap.Edges[i], snap.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.To < b.To
	})
	payload, err := json.Marshal(snap)
	if err != nil {
		return "", fmt.Errorf("marshal graph snapshot: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// EnsureGraphVersion 按需落版（契约 §10.2）：当前图 hash 不存在才写，返回图版本。
//
// 只在「被派发用到过」时留版——从没被任何一次派发用过的图，事后没人会去查它。
// 用 INSERT ... ON CONFLICT DO NOTHING 后回查，保证同一份图只有一行。
func (d *DB) EnsureGraphVersion(ctx context.Context, orgID string) (*GraphVersion, error) {
	edges, err := d.ListEdges(ctx, orgID, "")
	if err != nil {
		return nil, err
	}
	hash, err := graphContentHash(edges)
	if err != nil {
		return nil, err
	}
	snap := graphSnapshot{Edges: make([]graphEdge, 0, len(edges))}
	for _, e := range edges {
		snap.Edges = append(snap.Edges, graphEdge{From: e.FromCapability, Port: e.FromPort, To: e.ToCapability})
	}
	sort.Slice(snap.Edges, func(i, j int) bool {
		a, b := snap.Edges[i], snap.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.To < b.To
	})
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("marshal graph snapshot: %w", err)
	}

	if _, err := d.ExecContext(ctx,
		`INSERT INTO graph_versions (id, org_id, content_hash, snapshot, created_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(org_id, content_hash) DO NOTHING`,
		NewGraphVersionID(hash), orgID, hash, string(snapJSON), Now()); err != nil {
		return nil, fmt.Errorf("insert graph version: %w", err)
	}
	// 回查：可能是刚插的，也可能是并发下别人先插的（此时用已有的 id）
	gv, err := d.getGraphVersionByHash(ctx, orgID, hash)
	if err != nil {
		return nil, fmt.Errorf("read graph version after upsert: %w", err)
	}
	return gv, nil
}

// GetGraphVersion 按 ID 取图快照。
func (d *DB) GetGraphVersion(ctx context.Context, orgID, id string) (*GraphVersion, error) {
	return d.scanGraphVersion(ctx,
		`SELECT id, org_id, content_hash, snapshot, created_at FROM graph_versions
		 WHERE org_id = ? AND id = ?`, orgID, id)
}

// getGraphVersionByHash 按 (org_id, content_hash) 取图版本。
func (d *DB) getGraphVersionByHash(ctx context.Context, orgID, hash string) (*GraphVersion, error) {
	return d.scanGraphVersion(ctx,
		`SELECT id, org_id, content_hash, snapshot, created_at FROM graph_versions
		 WHERE org_id = ? AND content_hash = ?`, orgID, hash)
}

// scanGraphVersion 执行查询并解析一行；未命中返回 ErrNotFound。
func (d *DB) scanGraphVersion(ctx context.Context, query string, args ...any) (*GraphVersion, error) {
	var g GraphVersion
	var snapshot string
	err := d.QueryRowContext(ctx, query, args...).Scan(&g.ID, &g.OrgID, &g.ContentHash, &snapshot, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan graph version: %w", err)
	}
	g.Snapshot = json.RawMessage(snapshot)
	return &g, nil
}

// GraphHasEdge 判断快照里是否含某条边（便于测试与调试）。
func (g *GraphVersion) GraphHasEdge(from, port, to string) bool {
	var snap graphSnapshot
	if err := json.Unmarshal(g.Snapshot, &snap); err != nil {
		return false
	}
	for _, e := range snap.Edges {
		if e.From == from && e.Port == port && e.To == to {
			return true
		}
	}
	return false
}
