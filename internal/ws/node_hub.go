package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/zzjcool/anotify/internal/store"
)

// NodeHub 维护「节点 ID → 活跃连接」的映射，让派发器能把 assign 帧推给节点。
//
// 生命周期：
//
//	连接建立（hello 校验通过）→ Register
//	连接断开                   → Unregister
//	派发出新任务               → PublishAssign(nodeID, task)
//
// 一个节点同时只允许一条连接：后连的顶掉前一条（节点重启/换网络时常见）。
// 顶掉而不是拒绝，是因为旧连接往往已经是半死状态（TCP 还没超时），
// 拒绝会让节点永远连不上。
type NodeHub struct {
	mu    sync.RWMutex
	conns map[string]*nodeSession // nodeID → session
}

// NewNodeHub 构造一个空 hub。
func NewNodeHub() *NodeHub {
	return &NodeHub{conns: make(map[string]*nodeSession)}
}

// Register 让一条连接成为该节点的当前连接（顶掉旧的）。
func (h *NodeHub) Register(nodeID string, s *nodeSession) {
	h.mu.Lock()
	old := h.conns[nodeID]
	h.conns[nodeID] = s
	h.mu.Unlock()
	if old != nil && old != s {
		// 旧连接先踢掉：不踢的话两条连接会同时收到 assign，
		// 而同一任务只该有一个执行者。
		old.kill("superseded by a newer connection")
	}
}

// Unregister 移除连接（只在它仍是当前连接时移除，避免误删新连接）。
func (h *NodeHub) Unregister(nodeID string, s *nodeSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.conns[nodeID]; ok && cur == s {
		delete(h.conns, nodeID)
	}
}

// session 取节点的当前连接。
func (h *NodeHub) session(nodeID string) *nodeSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[nodeID]
}

// Online 报告节点是否有活跃连接（控制台/inspect 用）。
func (h *NodeHub) Online(nodeID string) bool {
	return h.session(nodeID) != nil
}

// OnlineCount 返回在线节点数。
func (h *NodeHub) OnlineCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// ErrNodeOffline 表示目标节点没有活跃连接。
var ErrNodeOffline = fmt.Errorf("node is offline")

// PublishAssign 向节点推送 assign 帧（实现 route.Publisher）。
//
// 任务已落库为 assigned，节点不在线时返回 ErrNodeOffline——
// 让派发方知道「这个任务推不出去了」。调用方可以选择：
//   - 保持 assigned，等节点重连后由租约巡检回收再派（当前行为）
//   - 立刻改回 pending（需要额外的事务编排，暂不处理）
func (h *NodeHub) PublishAssign(ctx context.Context, orgID, nodeID string, task *store.Task) error {
	s := h.session(nodeID)
	if s == nil {
		return ErrNodeOffline
	}
	if s.node.OrgID != orgID {
		// 绝不跨组织推任务（不变量 I10）
		return fmt.Errorf("node %s belongs to another org", nodeID)
	}
	return s.send(ctx, assignFrameFor(task))
}

// assignFrameFor 把任务转成 assign 帧。
//
// 载荷里**只有**能力名 + 版本 + params + 租约（不变量 I1）：
// 不含命令、不含脚本、不含 argv——节点自己知道怎么执行这个能力。
func assignFrameFor(task *store.Task) *AssignFrame {
	var params map[string]any
	if len(task.Params) > 0 {
		// 失败就发空 params：节点会按自己的 schema 校验并报错，
		// 比悄悄改写成别的值安全。
		if err := json.Unmarshal(task.Params, &params); err != nil {
			params = map[string]any{}
		}
	}
	if params == nil {
		params = map[string]any{}
	}

	var lease int64
	if task.LeaseUntil != nil {
		lease = *task.LeaseUntil
	}
	// max_duration_sec 是「这一份任务最多跑多久」的建议值。
	// 任务行没有携带能力 lifecycle（那是定义期的字段），所以这里给
	// 契约 §6.1 示例里的 3600s 作为上限预算；节点可以更早结束。
	// 真正的强制约束是租约（lease_until），不是这个值。
	const maxDuration = 3600

	return &AssignFrame{
		Type:        NodeFrameAssign,
		TaskID:      task.ID,
		Capability:  task.Capability,
		Version:     task.Version,
		Params:      params,
		LeaseUntil:  lease,
		MaxDuration: maxDuration,
	}
}
