package server

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/logging"
)

// statusWriter 包装 http.ResponseWriter 捕获 status code。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Write 也触发 status（确保 200 默认值正确）。
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}

// Hijack 实现 http.Hijacker，委托给底层 ResponseWriter。
// WebSocket 升级需要 Hijack；若不透传，accessLog 中间件会破坏 WS 连接。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("ResponseWriter does not implement Hijacker")
	}
	return h.Hijack()
}

// Flush 实现 http.Flusher，委托给底层 ResponseWriter（SSE/WS 需要）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog 是请求级日志中间件，包在最外层覆盖所有路由。
// 记录 method/path(剥离 query)/status/latency_ms/ip/request_id/user_id。
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := logging.NewRequestID()
		r = r.WithContext(logging.WithRequestID(r.Context(), rid))

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 0}

		next.ServeHTTP(sw, r)

		if sw.status == 0 {
			sw.status = 200
		}

		latency := time.Since(start).Microseconds()
		// r.URL.Path 天然不含 query（AC-3: secret/registrationToken 走 query 传输，不会泄露）
		path := r.URL.Path

		args := []any{
			"request_id", rid,
			"method", r.Method,
			"path", path,
			"status", sw.status,
			"latency_ms", latency,
			"ip", clientIP(r),
		}
		if uid := auth.UserIDFromContext(r.Context()); uid != "" {
			args = append(args, "user_id", uid)
		}

		// 健康检查降级为 DEBUG（避免探针刷屏）
		if path == "/health" {
			slog.Debug("request", args...)
			return
		}

		switch {
		case sw.status >= 500:
			slog.Error("request", args...)
		case sw.status == 429 || sw.status == 401 || sw.status == 403:
			slog.Warn("request", args...)
		default:
			slog.Info("request", args...)
		}
	})
}
