package ws

import (
	"context"
	"sync"
)

// steer.go 是「这台节点刚说过这个能力没槽」的调度建议表（#681）：只影响试点顺序，不阻止派发。
const slotSteerWindowSec int64 = 30

type slotSteerKey struct {
	nodeID      string
	namespaceID string
	capability  string
}

type slotSteer struct {
	mu    sync.Mutex
	until map[slotSteerKey]int64
}

func (h *Hub) markSlotFull(nodeID, namespaceID, capability string, now int64) {
	if nodeID == "" || namespaceID == "" || capability == "" {
		return
	}
	h.steer.mu.Lock()
	defer h.steer.mu.Unlock()
	if h.steer.until == nil {
		h.steer.until = map[slotSteerKey]int64{}
	}
	h.steer.until[slotSteerKey{nodeID: nodeID, namespaceID: namespaceID, capability: capability}] =
		now + slotSteerWindowSec
}

func (h *Hub) slotFullRecently(nodeID, namespaceID, capability string, now int64) bool {
	h.steer.mu.Lock()
	defer h.steer.mu.Unlock()
	key := slotSteerKey{nodeID: nodeID, namespaceID: namespaceID, capability: capability}
	until, ok := h.steer.until[key]
	if !ok {
		return false
	}
	if now >= until {
		delete(h.steer.until, key) // 顺手回收：窗口过后这条建议不再有意义
		return false
	}
	return true
}

func (h *Hub) clearSlotFull(nodeID, namespaceID, capability string) {
	h.steer.mu.Lock()
	defer h.steer.mu.Unlock()
	delete(h.steer.until, slotSteerKey{nodeID: nodeID, namespaceID: namespaceID, capability: capability})
}

func (h *Hub) noteSlotFull(ctx context.Context, nodeID, taskID string, now int64) {
	task, err := h.db.GetTaskByID(ctx, taskID)
	if err != nil || task == nil {
		return
	}
	h.markSlotFull(nodeID, task.NamespaceID, task.Capability, now)
}

func (h *Hub) clearSlotFullSteer(ctx context.Context, nodeID, taskID string) {
	task, err := h.db.GetTaskByID(ctx, taskID)
	if err != nil || task == nil {
		return
	}
	h.clearSlotFull(nodeID, task.NamespaceID, task.Capability)
}
