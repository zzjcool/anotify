package push

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// CentralNotifier 让中枢能力（notify.phone）能直接发推送。
//
// 为什么不复用 broker.Dispatcher：那条路径是「写一条 message → 消费者
// 拉取 → 过滤设备 → 推送」，message 是旧模型的核心实体（messages 表、
// seq 分配、deliveries 记录）。事件中心不再有 message，参数直接来自
// 任务 params（不变量 I6：不做模板替换）。
//
// 但**过滤规则**必须复用：ScopeMatch / TagMatch 是"哪台设备该收到这条"
// 的唯一定义，两套实现必然漂移。
type CentralNotifier struct {
	Store  *store.DB
	Sender Sender // 为 nil 时用 VAPID（由调用方注入 NewVAPIDSender）
}

// NotifyUser 给某用户的所有匹配设备发推送，返回成功的设备数。
func (n *CentralNotifier) NotifyUser(ctx context.Context, userID string, m NotifyMessage) (int, error) {
	if n.Sender == nil {
		return 0, fmt.Errorf("central notifier: no sender configured")
	}
	devices, err := n.Store.ListEnabledDevices(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("central notifier list devices: %w", err)
	}

	// 复用设备过滤：ShouldDeliver 的判定（enabled + scopeMatch + tagMatch）
	// 是"哪台设备该收到"的唯一定义，不复制第二份。
	//
	// #80 之后 Deliverable 已经收窄成最小结构（不再借 broker.Message），
	// 所以这里直接投影两个参与过滤的字段。
	matched := FilterDevices(devices, Deliverable{
		AgentState: m.AgentState,
		DeviceTags: m.DeviceTags,
	})

	if len(matched) == 0 {
		return 0, nil
	}

	payload, err := centralPayload(m)
	if err != nil {
		// 序列化失败不该静默：至少把标题发出去，并记日志
		slog.Warn("central notify payload marshal failed",
			"event", "push.central.payload_failed",
			"error", err.Error(),
		)
		payload = []byte(fmt.Sprintf(`{"title":%q}`, m.Title))
	}

	ttl := 86400
	urgency := urgencyFor(m.Priority)

	sent := 0
	var lastErr error
	for _, dev := range matched {
		statusCode, sendErr := n.Sender.Send(ctx, dev, payload, ttl, urgency)
		if sendErr != nil {
			lastErr = sendErr
			slog.Warn("central notify send failed",
				"event", "push.central.send_failed",
				"device_id", dev.ID,
				"status", statusCode,
				"error", sendErr.Error(),
			)
			// 404/410 = 订阅失效，禁用该设备（与旧路径行为一致）
			if statusCode == 404 || statusCode == 410 {
				if derr := n.Store.DisableDevice(ctx, dev.ID); derr != nil {
					slog.Warn("disable gone device failed",
						"event", "push.central.disable_failed",
						"device_id", dev.ID,
						"error", derr.Error(),
					)
				}
			}
			continue
		}
		sent++
		if terr := n.Store.TouchDelivered(ctx, dev.ID, store.Now()); terr != nil {
			slog.Warn("touch delivered failed",
				"event", "push.central.touch_failed",
				"device_id", dev.ID,
				"error", terr.Error(),
			)
		}
	}

	// 全部失败才算错误；部分成功是正常情况（某台设备订阅过期）
	if sent == 0 && lastErr != nil {
		return 0, fmt.Errorf("central notifier: all %d device(s) failed: %w", len(matched), lastErr)
	}
	return sent, nil
}

// NotifyMessage 是中枢推送的内容（来自 notify.phone 的 params）。
//
// 带 json tag 是因为它会被直接内嵌进 params 的解码结构
// （见 executor.notifyPhoneParams）——tag 必须与能力定义的
// params_schema 字段名一致，否则解码静默得到空值。
type NotifyMessage struct {
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Link       string   `json:"link"`
	Priority   string   `json:"priority"`
	AgentState string   `json:"agentState"`
	DeviceTags []string `json:"deviceTags"`
}

// centralPayload 构造推送给 Service Worker 的载荷。
//
// 字段名与旧路径保持一致（title/body/url/link/agentState），
// 因为 sw.js 已经按这套字段渲染通知——改字段名会让已安装的
// Service Worker 收不到内容。
func centralPayload(m NotifyMessage) ([]byte, error) {
	id := fmt.Sprintf("tsk_%d", time.Now().UnixNano())
	payload := map[string]any{
		"id":         id,
		"title":      m.Title,
		"body":       m.Body,
		"tag":        id,
		"link":       m.Link,
		"agentState": m.AgentState,
	}
	// 有 link 就带上（深链），没有就不带 url（避免跳到不存在的页面）
	if m.Link != "" {
		payload["url"] = m.Link
	}
	return json.Marshal(payload)
}
