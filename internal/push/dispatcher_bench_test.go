package push

import (
	"context"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/zzjcool/anotify/internal/broker"
	"github.com/zzjcool/anotify/internal/store"
)

// BenchmarkDispatch 度量一条消息完整派发管线的成本：
// 过期检查 → 查 enabled 设备 → 路由过滤 → 逐设备构造 payload → 发送 → 写投递记录。
// 用内存 Sender 注入，屏蔽真实网络，聚焦调度/路由/落库开销。
func BenchmarkDispatch(b *testing.B) {
	db := setupStore(b)
	ctx := context.Background()
	// 预置 50 台命中设备 + 50 台不命中（路由过滤 + 落库是热点）。
	for i := 0; i < 100; i++ {
		filter := "error"
		if i%2 == 0 {
			filter = "all"
		}
		insertDevice(b, db, "d_bench_"+string(rune('a'+i%26))+string(rune('0'+i/26%10)), "usr_1", true, filter, "ops", "build")
	}

	d := &Dispatcher{
		Broker: &fakeBroker{},
		Store:  db,
		Sender: SenderFunc(func(ctx context.Context, dev *store.Device, payload []byte, ttl int, u webpush.Urgency) (int, error) {
			return 201, nil
		}),
	}
	msg := &broker.Message{
		ID:         "ntf_bench",
		UserID:     "usr_1",
		Title:      "构建完成",
		Status:     broker.StatusSuccess,
		Priority:   "normal",
		TTLSeconds: 3600,
		ExpiresAt:  time.Now().Add(time.Hour),
		Payload:    []byte(`{"agentId":"a1"}`),
	}
	if err := db.InsertTestMessage(ctx, msg.ID, msg.UserID, 1, broker.StatusSuccess); err != nil {
		b.Fatalf("insert message: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := d.Dispatch(ctx, msg); err != nil {
			b.Fatalf("dispatch: %v", err)
		}
	}
}

// BenchmarkPushPayload 度量 push 载荷 JSON 构造成本。
func BenchmarkPushPayload(b *testing.B) {
	m := &broker.Message{
		ID:    "ntf_bench_01J8XA",
		Title: "构建完成：47 个文件变更",
		Body:  "全部通过，部署到生产环境",
		Link:  "pi://session/sess_8f3a",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pushPayload(m)
	}
}

// BenchmarkUrgencyFor 度量优先级 → Web Push urgency 映射成本（每次发送调用）。
func BenchmarkUrgencyFor(b *testing.B) {
	priorities := []string{"high", "normal", "low", "veryLow", "unknown"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = urgencyFor(priorities[i%len(priorities)])
	}
}
