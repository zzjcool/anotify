// Package logging 提供结构化日志基础设施：slog 封装、请求级关联、字段命名约定。
//
// 使用方式：
//
//	logging.Init(level, format)         // 进程启动时调用一次
//	logging.WithRequestID(r.Context())  // handler 内取 request_id
//	slog.Info("message", "event", "auth.login.success", "user_id", uid, ...)
package logging

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"os"
	"strings"
)

// contextKey 是 logging 包私有的 context key 类型。
type contextKey int

const (
	keyRequestID contextKey = iota
	keyUserID
)

// Init 初始化全局 slog Logger。level/format 非法时回退默认值并打 WARN。
// 输出固定写 stderr（自托管惯例，systemd/Docker 接管）。
func Init(level, format string) {
	lvl := parseLevel(level)
	fmt_ := parseFormat(format)

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: lvl}
	if fmt_ == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)

	// 非法值回退提示
	if level != "" && lvl == slog.LevelInfo && !isValidLevel(level) {
		slog.Warn("invalid log level, falling back to info", "input", level, "default", "info")
	}
	if format != "" && fmt_ == "json" && !isValidFormat(format) {
		slog.Warn("invalid log format, falling back to json", "input", format, "default", "json")
	}
}

// NewRequestID 生成一个 8 字符 URL-safe 随机 request_id。
func NewRequestID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// WithRequestID 把 request_id 注入 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// RequestID 从 context 取 request_id（无则返回空串）。
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

// WithUserID 把 user_id 注入 context（供请求中间件结束时取用）。
func WithUserID(ctx context.Context, uid string) context.Context {
	return context.WithValue(ctx, keyUserID, uid)
}

// UserID 从 context 取 user_id（无则返回空串）。
func UserID(ctx context.Context) string {
	if v, ok := ctx.Value(keyUserID).(string); ok {
		return v
	}
	return ""
}

// Logger 返回带 request_id/user_id 字段的子 logger（从 context 取值）。
// handler 内调用 slog.With(logging.FromContext(r.Context())...) 即可关联请求。
func FromContext(ctx context.Context) []any {
	args := make([]any, 0, 4)
	if rid := RequestID(ctx); rid != "" {
		args = append(args, "request_id", rid)
	}
	if uid := UserID(ctx); uid != "" {
		args = append(args, "user_id", uid)
	}
	return args
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info", "":
		return slog.LevelInfo
	default:
		return slog.LevelInfo // 非法回退
	}
}

func parseFormat(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "text":
		return "text"
	case "json", "":
		return "json"
	default:
		return "json" // 非法回退
	}
}

func isValidLevel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "info", "warn", "warning", "error", "":
		return true
	default:
		return false
	}
}

func isValidFormat(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "json", "text", "":
		return true
	default:
		return false
	}
}
