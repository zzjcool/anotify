package ws

// config_notify.go 是 `config.changed` 的中枢侧生产者（#569）：配置写库后必须主动叫节点
// 来 `config.get`，否则在线节点永远停在旧 revision（派发判定当 ConfigPending 排除）。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

const configNudgeInterval = 10 * time.Second

// NotifyConfigChanged 把「这台的默认值变了」推给它当前的会话：已对齐/离线都是 no-op。
func (h *Hub) NotifyConfigChanged(ctx context.Context, nodeID string) error {
	return h.pushConfigChanged(ctx, nodeID)
}

func (h *Hub) pushConfigChanged(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return nil
	}
	cfg, err := h.db.GetNodeConfig(ctx, nodeID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // 节点已被删除：没有可应用的事实
	}
	if err != nil {
		return fmt.Errorf("notify config changed: read node config: %w", err)
	}
	if cfg.Revision == cfg.AppliedRevision {
		return nil // 没有待应用的变更（含「从来没有配置行」）
	}
	sess := h.session(nodeID)
	if sess == nil {
		return nil // 离线：下次 hello 会对齐
	}
	frame := &protocol.ConfigChangedFrame{
		Type:     protocol.FrameConfigChanged,
		Revision: strconv.FormatInt(cfg.Revision, 10),
	}
	if err := sess.Send(ctx, frame); err != nil {
		h.detachSessionOnWriteFailure(sess, err)
		return fmt.Errorf("notify config changed: node %s: %w", nodeID, err)
	}
	return nil
}

func (h *Hub) nudgeStaleConfigs(ctx context.Context) {
	now := time.Now()
	h.cfgNudgeMu.Lock()
	if !h.lastCfgNudge.IsZero() && now.Sub(h.lastCfgNudge) < configNudgeInterval {
		h.cfgNudgeMu.Unlock()
		return
	}
	h.lastCfgNudge = now
	h.cfgNudgeMu.Unlock()

	for _, nodeID := range h.onlineNodeIDs() {
		if err := h.pushConfigChanged(ctx, nodeID); err != nil {
			h.log.Warn("config.changed nudge failed", "node_id", nodeID, "err", err)
		}
	}
}

func (h *Hub) onlineNodeIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.sessions))
	for id := range h.sessions {
		ids = append(ids, id)
	}
	return ids
}
