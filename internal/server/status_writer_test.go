package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 statusWriter 的两条「观测/能力」语义（都不影响响应字节，只影响运维与
// 依赖 ResponseWriter 能力的功能，所以特别容易在重构里被静默改坏）：
//
//	① 访问日志的 status 必须与客户端真正看到的状态码一致 ——「第一次 WriteHeader 赢」；
//	② 中间件链必须让 http.ResponseController 穿到真连接（SetWriteDeadline 等能力）。

// captureAccessLog 把默认 slog 换成写进 buffer 的 JSON handler，返回读缓冲的闭包。
func captureAccessLog(t *testing.T) func() string {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string { return buf.String() }
}

// TestAccessLogRecordsFirstWriteHeader 201 之后再写 500：响应是 201，日志也必须是 201。
//
// 旧实现的 `statusWriter.WriteHeader` 无条件覆盖 status，于是「先 201 再被某段收尾
// 代码写成 500」的请求在访问日志里变成 500，而客户端拿到的是 201。排障时这种
// 「日志说的事实和真实行为不一致」比缺一条日志更难查（#249 的微秒当毫秒是同一类）。
func TestAccessLogRecordsFirstWriteHeader(t *testing.T) {
	logs := captureAccessLog(t)

	h := gzipJSON(accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
		// 收尾代码又写了一次头：net/http 会忽略它（记一行 superfluous），
		// 客户端看到的仍然是 201 —— 日志必须与之一致。
		w.WriteHeader(http.StatusInternalServerError)
	})))

	// 客户端声明接受 gzip，走的是生产同款链路（gzipJSON 在外、accessLog 在内）。
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("客户端收到的 status = %d, want 201（net/http 的「第一次赢」语义）", rec.Code)
	}
	out := logs()
	if !strings.Contains(out, `"status":201`) {
		t.Fatalf("访问日志没记 201（记成了别的状态码 = 日志与真实行为不一致）：%s", out)
	}
	if strings.Contains(out, `"status":500`) {
		t.Fatalf("访问日志把「201 之后又写了个 500」记成 500：%s", out)
	}
}

// TestResponseControllerReachesConn 中间件链必须支持 http.ResponseController。
//
// 为什么这是功能性问题而不是优化：handler 拿到的永远是 *statusWriter（链是
// gzipJSON(accessLog(mux))）。ResponseController 只在 writer 提供了 Unwrap 链时
// 才能一路走到 net/http 的真连接；没有 Unwrap 时 SetWriteDeadline 直接返回
// ErrNotSupported，想给长连接（流式/WS 之后的写）设写超时的代码就只能静默失败。
func TestResponseControllerReachesConn(t *testing.T) {
	type probe struct {
		deadlineErr error
		flushErr    error
	}
	got := make(chan probe, 1)

	h := gzipJSON(accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		var p probe
		// SetWriteDeadline 只由底层连接（*http.response）提供，是最干净的能力探针。
		p.deadlineErr = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
		p.flushErr = rc.Flush()
		got <- p
	})))

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/events")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("读响应体: %v", err)
	}

	p := <-got
	if p.deadlineErr != nil {
		t.Fatalf("ResponseController.SetWriteDeadline 穿透失败: %v（多半是某层 writer 缺 Unwrap）", p.deadlineErr)
	}
	if p.flushErr != nil {
		t.Fatalf("ResponseController.Flush 穿透失败: %v", p.flushErr)
	}
}
