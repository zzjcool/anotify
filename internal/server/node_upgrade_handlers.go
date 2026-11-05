package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件是「控制台下发 agent 升级」端点（CONTRACT-agent-upgrade §5 / §5.1）。
//
//	POST /v1/nodes/{id}/upgrade
//
// 语义只有一句话：把「去哪个路径拿什么版本的二进制」推给那台节点，节点自己下载、
// 校验、原子替换、重启。中枢不碰节点磁盘，也不在帧里传二进制（15 MB 会撑爆心跳）。
//
// 响应形状（§5.1 冻结）：
//
//	200 {"status":"sent"|"up_to_date","targetVersion":"1.5.0+3fd5a59"}
//	409 {"error":"node_offline"}                节点没有活跃连接
//	501 {"error":"agent_binary_unavailable"}    服务端没带 agent 二进制
//	404                                          节点不存在 / 不在这个命名空间
//
// 为什么把 targetVersion 放进响应：控制台点完要立刻能在弹窗里写「正在升级到 X」，
// 不必再猜一次中枢版本。

// NodeUpgradePublisher 是中枢 → 节点的升级通道（*ws.NodeHub 满足它）。
//
// 抽成接口的理由与 DeployPublisher 相同：handler 的行为要能在不起 WebSocket 的
// 情况下测试（离线 / 501 / up_to_date 这些分支与连接层无关）。
type NodeUpgradePublisher interface {
	// Online 报告节点此刻是否有活跃连接（离线 → 409，判定在推帧之前）。
	Online(nodeID string) bool
	// PublishUpgrade 向节点推一帧 upgrade。
	PublishUpgrade(namespaceID, nodeID string, f *ws.UpgradeFrame) error
}

// nodeUpgradeHandler 持有升级端点需要的依赖。
type nodeUpgradeHandler struct {
	db      *store.DB
	console *consoleHandler // 复用命名空间解析（错误码映射只有一处真相）
	hub     NodeUpgradePublisher
	bin     *agentBinary
}

// upgradeRequest 是 POST /v1/nodes/{id}/upgrade 的请求体（全部可选）。
//
// force 既支持 JSON body 也支持 `?force=1|true`：控制台用 body，curl 用 query
// 更顺手。两种写法表达同一件事，不必为「怎么表达 force」再定一次协议。
type upgradeRequest struct {
	NamespaceID string `json:"namespaceId"`
	Force       bool   `json:"force"`
}

// targetVersion 是本次下发的目标版本（没带二进制时为空串）。
func (h *nodeUpgradeHandler) targetVersion() string {
	if h.bin == nil {
		return ""
	}
	return h.bin.targetVersion()
}

// upgrade 处理 POST /v1/nodes/{id}/upgrade。
func (h *nodeUpgradeHandler) upgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	nodeID := strings.TrimSpace(r.PathValue("id"))
	if nodeID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "node id required"})
		return
	}

	var req upgradeRequest
	if r.Body != nil && r.Body != http.NoBody {
		// body 是**可选**的：坏 JSON 不 400，交给命名空间解析给出更有用的错误
		// （「namespaceId is required」比「invalid JSON body」更接近调用方要改的东西）。
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	}
	// 写端点：`*` → 400（跨命名空间的下发没有定义）。
	ns, ok := h.console.resolveNamespace(w, r, req.NamespaceID, false)
	if !ok {
		return
	}
	force := req.Force || forceFromQuery(r)

	node, err := h.db.GetNode(r.Context(), ns.ID, nodeID)
	if errors.Is(err, store.ErrNotFound) {
		// 命名空间隔离在这里收口：别的命名空间的节点 id 同样得到 404，
		// 不区分「不存在」与「不属于你」（不回显资源是否存在）。
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
		return
	}
	if err != nil {
		slog.Error("get node failed", "event", "console.nodes.upgrade.get_failed",
			"node_id", nodeID, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// ① 离线优先（契约 §5 的顺序）：没有连接就没有下发对象，与「有没有二进制」无关。
	if h.hub == nil || !h.hub.Online(nodeID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "node_offline"})
		return
	}

	// ② 服务端没带二进制 → 501（明确说「这里没有升级源」，不是节点的问题）。
	target := h.targetVersion()
	if target == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "agent_binary_unavailable"})
		return
	}

	// ③ 已是最新且没强制 → 什么都不发。
	// 空 agent_version（老 agent）不等于目标版本，所以老 agent 永远走下发路径。
	if !force && node.AgentVersion == target {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "up_to_date",
			"targetVersion": target,
		})
		return
	}

	// ④ 推帧。节点在「查库」与「推帧」之间掉线是正常竞态 → 仍报 409。
	frame := &ws.UpgradeFrame{
		Type:          ws.NodeFrameUpgrade,
		TargetVersion: target,
		SHA256:        h.bin.sha256,
		URLPath:       ws.NodeBinaryPath,
	}
	if err := h.hub.PublishUpgrade(ns.ID, nodeID, frame); err != nil {
		if errors.Is(err, ws.ErrNodeOffline) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "node_offline"})
			return
		}
		slog.Error("publish upgrade failed", "event", "console.nodes.upgrade.publish_failed",
			"node_id", nodeID, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// ⑤ 记 sent（CONTRACT-agent-upgrade §5.1）：控制台刷新后要能看到「上次下发过、
	// 还没 ack」。写库失败只 WARN —— 帧已经推出去了，把它变成 500 会让调用方重试
	// 并推第二遍，节点会白下载一次。
	if err := h.db.MarkAgentUpgradeSent(r.Context(), ns.ID, nodeID, target, store.Now()); err != nil {
		slog.Warn("record agent upgrade sent failed",
			"event", "console.nodes.upgrade.record_failed",
			"node_id", nodeID, "error", err.Error())
	}

	slog.Info("agent upgrade dispatched",
		"event", "console.nodes.upgrade.sent",
		"node_id", nodeID,
		"namespace_id", ns.ID,
		"target_version", target,
		"sha256", h.bin.sha256,
		"force", force,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "sent",
		"targetVersion": target,
	})
}

// forceFromQuery 解析 `?force=1|true|yes`（大小写不敏感）。
//
// 只认这三个肯定值：`?force=0` / 垃圾值都当作「没强制」，不猜意图。
func forceFromQuery(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
