package push

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/anotify/anotify/internal/broker"
	"github.com/anotify/anotify/internal/store"
)

// fakeBroker 是一个内存 Broker，只为满足 Dispatcher 测试。
type fakeBroker struct {
	published []*broker.Message
}

func (f *fakeBroker) Publish(ctx context.Context, m *broker.Message) error {
	f.published = append(f.published, m)
	return nil
}
func (f *fakeBroker) Subscribe(ctx context.Context, userID string, tags []string) (broker.Subscription, error) {
	return &fakeSub{ch: make(chan *broker.Message)}, nil
}
func (f *fakeBroker) Ack(ctx context.Context, consumerID, userID string, seq int64) error {
	return nil
}
func (f *fakeBroker) Replay(ctx context.Context, userID string, sinceSeq int64, limit int) ([]*broker.Message, error) {
	return nil, nil
}
func (f *fakeBroker) Close() error { return nil }

type fakeSub struct{ ch chan *broker.Message }

func (s *fakeSub) C() <-chan *broker.Message { return s.ch }
func (s *fakeSub) Close() error              { return nil }

// setupStore 建一个内存库。
func setupStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// 插入外键父用户
	if _, err := db.InsertUser(context.Background(), "usr_1", "tester", "Tester"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return db
}

func insertDevice(t *testing.T, db *store.DB, id, userID string, enabled bool, filter string, tags ...string) {
	t.Helper()
	dev := &store.Device{
		ID:           id,
		UserID:       userID,
		Enabled:      enabled,
		StatusFilter: filter,
		Tags:         tags,
		Endpoint:     "https://push.example.com/" + id,
		P256dh:       "p256dh-" + id,
		Auth:         "auth-" + id,
	}
	if err := db.UpsertDevice(context.Background(), dev); err != nil {
		t.Fatalf("upsert device %s: %v", id, err)
	}
}

type sentRecord struct {
	deviceID string
	payload  []byte
}

// TestDispatchSendsToMatchedDevices 验证派发只命中路由匹配的设备。
func TestDispatchSendsToMatchedDevices(t *testing.T) {
	db := setupStore(t)
	ctx := context.Background()
	insertDevice(t, db, "d_phone", "usr_1", true, "all", "手机")
	insertDevice(t, db, "d_work", "usr_1", true, "error", "工作")
	insertDevice(t, db, "d_all", "usr_1", true, "all") // catch-all

	var sent []sentRecord
	d := &Dispatcher{
		Broker: &fakeBroker{},
		Store:  db,
		Sender: SenderFunc(func(ctx context.Context, dev *store.Device, payload []byte, ttl int, u webpush.Urgency) (int, error) {
			sent = append(sent, sentRecord{deviceID: dev.ID, payload: payload})
			return 201, nil
		}),
	}

	// 广播 success：d_phone✓ d_work✗(status) d_all✓
	msg := &broker.Message{
		ID:         "ntf_1",
		UserID:     "usr_1",
		Title:      "构建完成",
		Status:     broker.StatusSuccess,
		TTLSeconds: 3600,
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	// deliveries 外键父行（生产中由 broker.Publish 持久化）
	if err := db.InsertTestMessage(ctx, "ntf_1", "usr_1", 1, broker.StatusSuccess); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := d.Dispatch(ctx, msg); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	got := map[string]bool{}
	for _, s := range sent {
		got[s.deviceID] = true
	}
	if len(sent) != 2 || !got["d_phone"] || !got["d_all"] {
		t.Fatalf("sent to %v, want d_phone+d_all", got)
	}

	// 校验投递记录写库
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE message_id='ntf_1' AND channel='webpush' AND status='sent'`).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if n != 2 {
		t.Fatalf("deliveries rows=%d, want 2", n)
	}
}

// TestPushPayloadDeepLink 推送载荷的点击跳转 URL 必须是控制台消息详情深链
// （index.html?msg=<id>）；消息自带 link 仅作 link 字段透传，由详情页承接。
func TestPushPayloadDeepLink(t *testing.T) {
	msg := &broker.Message{
		ID:    "ntf_01J8XA",
		Title: "构建完成",
		Body:  "共 47 个文件变更",
		Link:  "pi://session/sess_8f3a",
	}
	var p map[string]string
	if err := json.Unmarshal(pushPayload(msg), &p); err != nil {
		t.Fatalf("payload 非 JSON: %v", err)
	}
	if want := "message.html?id=ntf_01J8XA"; p["url"] != want {
		t.Fatalf("url=%q, want %q（点击应跳控制台消息详情页）", p["url"], want)
	}
	if p["link"] != msg.Link {
		t.Fatalf("link=%q, want %q（外部深链透传）", p["link"], msg.Link)
	}
	if p["id"] != msg.ID || p["tag"] != msg.ID {
		t.Fatalf("id/tag 应等于消息 ID: id=%q tag=%q", p["id"], p["tag"])
	}
}

// TestDispatchExpiredMessageSkipped 过期消息不投递。
func TestDispatchExpiredMessageSkipped(t *testing.T) {
	db := setupStore(t)
	insertDevice(t, db, "d_all", "usr_1", true, "all")

	called := false
	d := &Dispatcher{
		Broker: &fakeBroker{},
		Store:  db,
		Sender: SenderFunc(func(ctx context.Context, dev *store.Device, p []byte, ttl int, u webpush.Urgency) (int, error) {
			called = true
			return 201, nil
		}),
	}
	msg := &broker.Message{
		ID:        "ntf_exp",
		UserID:    "usr_1",
		Title:     "过期",
		Status:    broker.StatusInfo,
		ExpiresAt: time.Now().Add(-time.Hour), // 已过期
	}
	if err := d.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if called {
		t.Fatal("expired message should not be sent")
	}
}

// TestDispatchGoneDeviceDisabled 410 Gone → 设备被禁用。
func TestDispatchGoneDeviceDisabled(t *testing.T) {
	db := setupStore(t)
	ctx := context.Background()
	insertDevice(t, db, "d_dead", "usr_1", true, "all")

	d := &Dispatcher{
		Broker: &fakeBroker{},
		Store:  db,
		Sender: SenderFunc(func(ctx context.Context, dev *store.Device, p []byte, ttl int, u webpush.Urgency) (int, error) {
			return 410, errors.New("webpush status 410: gone")
		}),
	}
	msg := &broker.Message{
		ID:        "ntf_gone",
		UserID:    "usr_1",
		Title:     "测试",
		Status:    broker.StatusSuccess,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.InsertTestMessage(ctx, "ntf_gone", "usr_1", 1, broker.StatusSuccess); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := d.Dispatch(ctx, msg); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// 设备应被禁用
	var enabled int
	if err := db.QueryRow(`SELECT enabled FROM devices WHERE id='d_dead'`).Scan(&enabled); err != nil {
		t.Fatalf("query device: %v", err)
	}
	if enabled != 0 {
		t.Fatalf("device should be disabled after 410, enabled=%d", enabled)
	}

	// 应有一条 failed 投递记录
	var status, derr string
	if err := db.QueryRow(`SELECT status, error FROM deliveries WHERE message_id='ntf_gone'`).Scan(&status, &derr); err != nil {
		t.Fatalf("query delivery: %v", err)
	}
	if status != store.DeliveryFailed {
		t.Fatalf("delivery status=%s, want failed", status)
	}
}
