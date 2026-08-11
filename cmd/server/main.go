// Anotify 服务端单二进制入口。
//
// 路径分离：
//
//	/v1/*     动态 API（no-store），含 /v1/notify、/v1/auth/*、/v1/devices、/v1/keys、/v1/stream
//	/*        静态资源（CDN 缓存分级：哈希文件 immutable，入口短缓存+ETag）
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/zzjcool/anotify/internal/logging"
	"github.com/zzjcool/anotify/internal/server"
)

func main() {
	cfg := server.FromEnv()

	// 初始化结构化日志（在所有其他操作之前）
	logging.Init(cfg.LogLevel, cfg.LogFormat)

	if err := cfg.Validate(); err != nil {
		slog.Warn("VAPID 未配置，Web Push 不可用（其余可运行）",
			"event", "push.vapid.missing",
			"error", err.Error(),
		)
	}

	mux := server.NewMux(cfg)

	slog.Info("server started",
		"event", "server.start",
		"addr", cfg.Addr,
		"static_dir", cfg.StaticDir,
		"cdn_prefix", cfg.CDNPrefix,
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
		"vapid_configured", cfg.VAPIDPublic != "",
	)
	if err := http.ListenAndServe(cfg.Addr, mux); err != nil {
		slog.Error("server listen failed",
			"event", "server.fatal",
			"error", err.Error(),
		)
		os.Exit(1)
	}
}
