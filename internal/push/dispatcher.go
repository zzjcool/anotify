package push

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/zzjcool/anotify/internal/broker"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// Sender 抽象一次 Web Push 发送，便于测试注入。
type Sender interface {
	Send(ctx context.Context, dev *store.Device, payload []byte, ttl int, urgency webpush.Urgency) (statusCode int, err error)
}

// SenderFunc 适配普通函数到 Sender。
type SenderFunc func(ctx context.Context, dev *store.Device, payload []byte, ttl int, urgency webpush.Urgency) (int, error)

// Send 实现 Sender。
func (f SenderFunc) Send(ctx context.Context, dev *store.Device, payload []byte, ttl int, urgency webpush.Urgency) (int, error) {
	return f(ctx, dev, payload, ttl, urgency)
}

// Dispatcher 是 Web Push 派发器：作为 Broker 的消费者2，
// 订阅某用户的通知流，按路由规则过滤设备后逐设备 Web Push 推送，
// 并把每次投递写入 deliveries 表。
type Dispatcher struct {
	Broker broker.Broker
	Store  *store.DB
	Sender Sender // 真实实现为 webpushSender（用 VAPID）

	// OnGone 在设备 endpoint 失效（404/410）时调用；默认禁用设备。
	OnGone func(ctx context.Context, deviceID string)
}

// webpushSender 用 webpush-go + VAPID 真实发送。
type webpushSender struct {
	cfg *VAPIDConfig
}

// NewVAPIDSender 构造一个基于 VAPID 的真实 Sender。
func NewVAPIDSender(cfg *VAPIDConfig) Sender {
	return &webpushSender{cfg: cfg}
}

func (s *webpushSender) Send(ctx context.Context, dev *store.Device, payload []byte, ttl int, urgency webpush.Urgency) (int, error) {
	sub := &webpush.Subscription{
		Endpoint: dev.Endpoint,
		Keys: webpush.Keys{
			Auth:   dev.Auth,
			P256dh: dev.P256dh,
		},
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, sub, s.cfg.options(ttl, urgency))
	if err != nil {
		return 0, fmt.Errorf("webpush send: %w", err)
	}
	defer resp.Body.Close()
	// 读取 body 以便连接复用 & 获取错误细节
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("webpush status %d: %s", resp.StatusCode, string(body))
	}
	return resp.StatusCode, nil
}

// urgencyFor 按消息优先级映射 Web Push urgency。
func urgencyFor(priority string) webpush.Urgency {
	switch priority {
	case "high":
		return webpush.UrgencyHigh
	case "low":
		return webpush.UrgencyLow
	case "veryLow":
		return webpush.UrgencyVeryLow
	default:
		return webpush.UrgencyNormal
	}
}

// Run 启动派发循环，直到 ctx 取消。为 userID 订阅全部消息（tags=nil）。
// 这是个常驻消费者；通常在 goroutine 里为每个活跃用户启动，或由上层编排。
func (d *Dispatcher) Run(ctx context.Context, userID string) error {
	sub, err := d.Broker.Subscribe(ctx, userID, nil)
	if err != nil {
		return fmt.Errorf("push dispatcher subscribe: %w", err)
	}
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-sub.C():
			if !ok {
				return nil
			}
			if err := d.Dispatch(ctx, msg); err != nil {
				slog.Error("push dispatch failed",
					"event", "push.dispatch.fail",
					"message_id", msg.ID,
					"user_id", msg.UserID,
					"error", err.Error(),
				)
			}
		}
	}
}

// Dispatch 处理一条消息：过滤设备 → 逐设备发送 → 记录投递。可独立调用（测试/同步路径）。
func (d *Dispatcher) Dispatch(ctx context.Context, msg *broker.Message) error {
	// 过期检查：超过可投递截止则跳过
	if !msg.ExpiresAt.IsZero() && time.Now().After(msg.ExpiresAt) {
		return nil
	}

	devices, err := d.Store.ListEnabledDevices(ctx, msg.UserID)
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}
	matched := route.FilterDevices(devices, msg)

	payload := pushPayload(msg)

	for _, dev := range matched {
		ttl := msg.TTLSeconds
		if ttl <= 0 {
			ttl = 86400
		}
		statusCode, sendErr := d.Sender.Send(ctx, dev, payload, ttl, urgencyFor(msg.Priority))
		d.recordResult(ctx, msg, dev, statusCode, sendErr)
	}
	return nil
}

// recordResult 把一次投递结果写进 deliveries，并处理失效设备。
func (d *Dispatcher) recordResult(ctx context.Context, msg *broker.Message, dev *store.Device, statusCode int, sendErr error) {
	now := store.Now()
	if sendErr == nil {
		_ = d.Store.RecordDelivery(ctx, msg.ID, dev.ID, store.ChannelWebPush, store.DeliverySent, "")
		_ = d.Store.TouchDelivered(ctx, dev.ID, now)
		slog.Info("push dispatched",
			"event", "push.dispatch.sent",
			"message_id", msg.ID,
			"device_id", dev.ID,
			"user_id", msg.UserID,
			"status", statusCode,
		)
		return
	}

	errMsg := sendErr.Error()
	_ = d.Store.RecordDelivery(ctx, msg.ID, dev.ID, store.ChannelWebPush, store.DeliveryFailed, errMsg)
	slog.Error("push dispatch failed",
		"event", "push.dispatch.fail",
		"message_id", msg.ID,
		"device_id", dev.ID,
		"user_id", msg.UserID,
		"status", statusCode,
		"error", errMsg,
	)

	// 404 Not Found / 410 Gone → endpoint 失效，禁用设备
	if statusCode == 404 || statusCode == 410 {
		slog.Warn("push device gone",
			"event", "push.device.gone",
			"device_id", dev.ID,
			"user_id", msg.UserID,
			"message_id", msg.ID,
		)
		if d.OnGone != nil {
			d.OnGone(ctx, dev.ID)
		} else {
			_ = d.Store.DisableDevice(ctx, dev.ID)
		}
	}
}

// pushPayload 构造发给设备的 JSON 载荷（Service Worker showNotification 用）。
// url 固定指向控制台消息详情页（message.html?id=<id>）：点击通知 → 展示该消息全部信息；
// 消息自带的外部 link 仅作 link 字段透传，由详情页内「打开 Agent 会话」按钮承接。
func pushPayload(msg *broker.Message) []byte {
	p := map[string]any{
		"id":    msg.ID,
		"title": msg.Title,
		"body":  msg.Body,
		"tag":   msg.ID,
		"url":   "message.html?id=" + url.QueryEscape(msg.ID),
		"link":  msg.Link,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		// 兜底：至少发标题
		return []byte(fmt.Sprintf(`{"title":%q,"body":%q}`, msg.Title, msg.Body))
	}
	return raw
}
