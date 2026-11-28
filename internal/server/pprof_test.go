package server

// pprof 暴露面（#624）。
//
// 为什么需要这一条：旧实现把 5 条 /debug/pprof/* 挂在业务 mux 上，于是
// 「ANOTIFY_PPROF=1」= 「把 goroutine 栈与堆分布暴露到 ANOTIFY_ADDR 的监听面」，
// 而 ANOTIFY_ADDR 默认 `:8080`（全接口）、pprof 端点没有任何鉴权。
// 现在 pprof 走独立监听（NewPprofMux，默认 127.0.0.1:6060）。
//
// 两个断言缺一不可：
//  1. 业务 mux 上**即使开关打开**也采不到 pprof —— 只测「关着时 404」毫无意义；
//  2. 独立 mux 上必须真的能采到（200）—— 否则「隔离」只是把功能弄丢了。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPprof_NotOnPublicMux 钉住 pprof 与业务端口的隔离。
func TestPprof_NotOnPublicMux(t *testing.T) {
	// 用真实装配（newProductionAppForTest）而不是手拼小 mux：隔离是装配层的事实，
	// 只有真 NewApp 才能证明「开关打开时也没人把它挂回业务 mux」。
	app := newProductionAppForTest(t, func(cfg *Config) { cfg.Pprof = true })

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/profile", "/debug/pprof/heap"} {
		t.Run("业务 mux 不暴露 "+path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			app.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			// 业务 mux 上这个路径只能落到静态兜底，而兜底只有两种结局：
			//   - 内嵌产物在（`-tags embeddist` 构建）→ 找不到文件 → 404；
			//   - 没内嵌产物（普通 `go test`）→ resolveStatic 直接 503。
			// 两种都不该是 pprof 的 200。
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusServiceUnavailable {
				t.Errorf("pprof 开关打开时 %s 应落到静态兜底（404/503），得到 %d —— 说明它又被挂回业务 mux 了", path, rec.Code)
			}
			// 再加一条与状态码无关的判据：pprof 的索引页一定有这个标题，出现即暴露。
			if body := rec.Body.String(); strings.Contains(body, "/debug/pprof/") || strings.Contains(body, "Types of profiles available") {
				t.Errorf("pprof 开关打开时 %s 的响应体里出现了 pprof 索引内容 —— 已暴露到 ANOTIFY_ADDR", path)
			}
		})
	}

	// 独立 mux 必须真的能采到：pprof.Index 直接写 200 + 索引页。
	t.Run("独立 pprof mux 提供索引", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NewPprofMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("NewPprofMux 的 /debug/pprof/ 必须返回 200，得到 %d", rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "Types of profiles available") {
			t.Errorf("NewPprofMux 的 /debug/pprof/ 响应不是 pprof 索引页（缺标题），body: %.120q", body)
		}
	})
}
