package store

import (
	"context"
	"errors"
)

// 节点状态（契约 §2.3 nodes.status）。
const (
	NodeStatusOnline   = "online"
	NodeStatusOffline  = "offline"
	NodeStatusDraining = "draining"
)

// Node 是 nodes 表的一行（一台机器）。
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

// ErrUnknownCapability 表示节点声明了中枢不存在的能力名@版本（不变量 I2）。
var ErrUnknownCapability = errors.New("unknown_capability")

// ErrTooManyInstances 表示节点申报的实例数超过 nodes.max_instances 硬顶。
var ErrTooManyInstances = errors.New("too_many_instances")

// CapabilityRef 是节点声明自己具备的能力（只有 name + version，没有命令）。
type CapabilityRef struct {
	Name    string `json:"capability"`
	Version int    `json:"version"`
}

// InstanceSpec 是节点申报的并发槽位（hello 帧的 instances 字段）。
type InstanceSpec struct {
	Slot        int `json:"slot"`
	Concurrency int `json:"concurrency"`
}

// CreateNode 创建节点，返回节点与**只出现一次**的明文 token。
func (d *DB) CreateNode(ctx context.Context, n *Node) (token string, err error) {
	return "", ErrNotImplemented
}

// GetNodeByTokenHash 按 token sha256 取节点（握手鉴权用，契约 §10.3）。
func (d *DB) GetNodeByTokenHash(ctx context.Context, tokenHash string) (*Node, error) {
	return nil, ErrNotImplemented
}

// GetNode 按 (orgID, id) 取节点。
func (d *DB) GetNode(ctx context.Context, orgID, nodeID string) (*Node, error) {
	return nil, ErrNotImplemented
}

// ListNodes 列出组织内节点（空列表返回 []）。
func (d *DB) ListNodes(ctx context.Context, orgID string) ([]*Node, error) {
	return nil, ErrNotImplemented
}

// UpdateNodeStatus 改节点状态（online/offline/draining），并刷新 last_seen_at。
func (d *DB) UpdateNodeStatus(ctx context.Context, orgID, nodeID, status string) error {
	return ErrNotImplemented
}

// TouchNode 刷新 last_seen_at（任何上行帧都续命，契约 §3.3）。
func (d *DB) TouchNode(ctx context.Context, orgID, nodeID string, at int64) error {
	return ErrNotImplemented
}

// ConfirmCapabilities 校验并写入节点声明的能力。
//
// 声明的 name@version 必须在中枢已定义，否则返回 ErrUnknownCapability；
// 写入 node_capabilities 时只存 (node_id, capability_id)——没有命令、没有出口、没有 kind。
// 返回解析出的 capability_id 列表（顺序与入参一致）。
func (d *DB) ConfirmCapabilities(ctx context.Context, orgID, nodeID string, caps []CapabilityRef) ([]string, error) {
	return nil, ErrNotImplemented
}

// ListNodeCapabilities 返回节点声明的能力定义（含 outputs，供 kind 推导与派发选点）。
func (d *DB) ListNodeCapabilities(ctx context.Context, nodeID string) ([]*CapabilityVersion, error) {
	return nil, ErrNotImplemented
}

// NodesForCapability 返回声明了该能力、且可接单的节点 ID（派发第 ③ 步的唯一查表入口）。
func (d *DB) NodesForCapability(ctx context.Context, orgID, capabilityID string) ([]string, error) {
	return nil, ErrNotImplemented
}

// UpsertInstances 落库节点申报的实例槽位；超过 nodes.max_instances 返回 ErrTooManyInstances。
func (d *DB) UpsertInstances(ctx context.Context, orgID, nodeID string, specs []InstanceSpec) ([]*NodeInstance, error) {
	return nil, ErrNotImplemented
}

// ListInstances 列出节点实例（空列表返回 []）。
func (d *DB) ListInstances(ctx context.Context, nodeID string) ([]*NodeInstance, error) {
	return nil, ErrNotImplemented
}

// MarkInstancesDead 把节点全部实例标记为掉线（连接断开时调用）。
func (d *DB) MarkInstancesDead(ctx context.Context, nodeID string) error {
	return ErrNotImplemented
}

// LeaseInstance 把任务占到一个空闲实例上（CAS：busy_task IS NULL → taskID）。
// 返回 instance_id；没有空闲槽位返回 ErrNoCapacity。
func (d *DB) LeaseInstance(ctx context.Context, nodeID, taskID string) (string, error) {
	return "", ErrNotImplemented
}

// ReleaseInstance 释放实例占用（任务结束/回收时调用）。
func (d *DB) ReleaseInstance(ctx context.Context, nodeID, instanceID string) error {
	return ErrNotImplemented
}

// ErrNoCapacity 表示节点没有空闲并发槽位。
var ErrNoCapacity = errors.New("no_capacity")

// GetPin 按 sticky_key 取粘性钉住；未命中返回 ErrNotFound。
func (d *DB) GetPin(ctx context.Context, stickyKey string) (*NodePin, error) {
	return nil, ErrNotImplemented
}

// PutPin 写入（或刷新）粘性钉住。
func (d *DB) PutPin(ctx context.Context, p *NodePin) error {
	return ErrNotImplemented
}

// StickyKey 计算粘性键：sha256(trace_id ∥ capability_id ∥ selector)。
func StickyKey(traceID, capabilityID, selector string) string {
	return ""
}
