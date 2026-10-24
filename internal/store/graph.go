package store

import (
	"context"
	"encoding/json"
)

// Edge 是 edges 表的一行：出口表是路由的唯一依据（契约 §2.4）。
//
// 一行 = 一条边；同一 (FromCapability, FromPort) 可有多行 → 扇出（不变量 I7：
// 两端都是 capability_id，图里没有机器）。
type Edge struct {
	OrgID          string `json:"orgId"`
	FromCapability string `json:"fromCapability"`
	FromPort       string `json:"fromPort"`
	ToCapability   string `json:"toCapability"`
	CreatedAt      int64  `json:"createdAt"`
}

// GraphVersion 是 graph_versions 表的一行：按需落版的图快照（契约 §10.2）。
type GraphVersion struct {
	ID          string          `json:"id"`
	OrgID       string          `json:"orgId"`
	ContentHash string          `json:"contentHash"`
	Snapshot    json.RawMessage `json:"snapshot"`
	CreatedAt   int64           `json:"createdAt"`
}

// CreateEdge 新增一条边。from_port 必须 ∈ from 能力的 outputs（不变量 I2 / A5），
// 两端能力必须同组织；违反返回 ErrEdgeRejected。
func (d *DB) CreateEdge(ctx context.Context, e *Edge) error {
	return ErrNotImplemented
}

// DeleteEdge 删除一条边。
func (d *DB) DeleteEdge(ctx context.Context, orgID, fromCapability, fromPort, toCapability string) error {
	return ErrNotImplemented
}

// ListEdges 列出组织的边（可选按 from 过滤）；空列表返回 []。
func (d *DB) ListEdges(ctx context.Context, orgID, fromCapability string) ([]*Edge, error) {
	return nil, ErrNotImplemented
}

// EdgesFrom 是派发第 ① 步的唯一查表入口：
// 返回 (orgID, fromCapability, fromPort) 的全部下游 capability_id。
func (d *DB) EdgesFrom(ctx context.Context, orgID, fromCapability, fromPort string) ([]string, error) {
	return nil, ErrNotImplemented
}

// EnsureGraphVersion 按需落版：当前图 hash 不存在才写，返回图版本。
func (d *DB) EnsureGraphVersion(ctx context.Context, orgID string) (*GraphVersion, error) {
	return nil, ErrNotImplemented
}

// GetGraphVersion 按 ID 取图快照。
func (d *DB) GetGraphVersion(ctx context.Context, orgID, id string) (*GraphVersion, error) {
	return nil, ErrNotImplemented
}
