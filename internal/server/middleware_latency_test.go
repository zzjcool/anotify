package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAccessLogLatencyIsMilliseconds 锁住 `latency_ms` 的**单位**（#249）。
//
// 原实现取的是 Microseconds()，于是 25ms 的处理写成 latency_ms≈25000：
// 任何按秒设阈值的 P95/超时告警都会永远误报；反过来按"25 看着正常"调阈值，
// 又会把真实的 25 秒慢请求当成正常值藏起来。
//
// 断言用**区间**而不是精确值：执行时间不可控，但数量级是确定的。
//   - 正确（ms）：25ms 的 handler → 20..1000 之间
//   - 退回微秒：≈25000 → 直接超出上界，红
func TestAccessLogLatencyIsMilliseconds(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	const want = 25 * time.Millisecond
	h := accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(want)
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/whatever", nil))

	// 找到那条 request 日志（可能还有别的行）
	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志不是合法 JSON: %v (%s)", err, line)
		}
		if m["msg"] == "request" {
			entry = m
			break
		}
	}
	if entry == nil {
		t.Fatalf("没找到 request 日志：%s", buf.String())
	}

	raw, ok := entry["latency_ms"]
	if !ok {
		t.Fatalf("日志缺 latency_ms 字段：%v", entry)
	}
	ms, ok := raw.(float64)
	if !ok {
		t.Fatalf("latency_ms 不是数字：%T %v", raw, raw)
	}
	if ms < 20 || ms > 1000 {
		t.Fatalf("latency_ms 必须接近 25（毫秒），实际 %v——若约等于 25000 说明又把微秒写进来了（#249）", ms)
	}
}
