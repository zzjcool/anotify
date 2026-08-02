package server

import (
	"net/http"
	"strings"
)

// Deps 装配依赖（集成期由协调者注入真实实现；此处为接口占位，便于先编译）。
type Deps struct {
	// 以下在集成期接线（来自 broker/auth/api/ws/push 包）：
	// NotifyHandler  http.Handler
	// StreamHandler  http.Handler
	// AuthHandler    http.Handler
	// DevicesHandler http.Handler
	// KeysHandler    http.Handler
}

// NewMux 构造路由：/v1/* 走动态 API（no-store），其余走静态资源（CDN 缓存分级）。
// 集成期会注入真实 API handler；当前返回健康检查 + 静态服务，便于冒烟。
func NewMux(cfg Config) http.Handler {
	mux := http.NewServeMux()

	// 动态 API（no-store）
	mux.Handle("/v1/", noStore(apiPlaceholder()))

	// 健康检查
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	// 静态资源（CDN 缓存分级）
	mux.Handle("/", staticHandler(cfg.StaticDir))

	return mux
}

// apiPlaceholder 在集成前返回 501，保证 /v1/* 不落到静态处理器。
func apiPlaceholder() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 集成期替换为真实路由
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"error":"api not wired yet"}`))
			return
		}
		http.NotFound(w, r)
	})
}
