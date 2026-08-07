package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anotify/anotify/internal/authn"
	"github.com/anotify/anotify/internal/broker"
	"github.com/anotify/anotify/internal/store"
)

// BenchmarkNormalizeTags 度量 deviceTags 归一化（trim + 去重 + 限量限长）成本。
func BenchmarkNormalizeTags(b *testing.B) {
	tags := []string{" ops ", "build", "release", "  ", "ops", "BUILD", "ci", "prod", "手机", "工作"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(normalizeTags(tags)) == 0 {
			b.Fatal("normalizeTags 不应返回空")
		}
	}
}

// BenchmarkNotifyServeHTTP 度量完整 POST /v1/notify 处理器成本：
// 鉴权 + 解码 + 校验 + 归一化 + 发布到 broker + 投递预览。
// 用内存 broker 与内存 store，屏蔽真实 DB 与网络。
func BenchmarkNotifyServeHTTP(b *testing.B) {
	db, err := store.Open(":memory:")
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := db.InsertUser(ctx, "usr_bench", "tester", "Tester"); err != nil {
		b.Fatalf("insert user: %v", err)
	}
	// 预置 20 台 enabled 设备，让投递预览有真实命中路径。
	for i := 0; i < 20; i++ {
		dev := &store.Device{
			ID: "dev_bench" + string(rune('a'+i)), UserID: "usr_bench",
			Enabled: true, StatusFilter: "all",
			Endpoint: "https://push.example.com/" + string(rune('a'+i)),
			P256dh:   "p", Auth: "a",
		}
		if err := db.UpsertDevice(ctx, dev); err != nil {
			b.Fatalf("upsert device: %v", err)
		}
	}

	kv := authn.KeyValidatorFunc(func(ctx context.Context, key string) (string, []string, error) {
		return "usr_bench", []string{authn.ScopeNotifySend}, nil
	})
	broker := newNotifyBenchBroker()
	h := newNotifyHandler(broker, kv, db)

	body := `{"title":"构建完成","status":"success","body":"47 个文件","deviceTags":["ops","build"]}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 每次迭代重新构造请求体（body 首次读取即被消费，不能复用 req）。
		req := httptest.NewRequest(http.MethodPost, "/v1/notify", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer ant_live_bench")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("status=%d, want 200, body=%s", rr.Code, rr.Body)
		}
	}
}

// newNotifyBenchBroker 返回一个满足 broker.Broker 的内存桩，只记录发布。
func newNotifyBenchBroker() *fakeBroker {
	return &fakeBroker{published: make([]*broker.Message, 0, 1)}
}
