// Anotify 服务端单二进制入口。
//
// 路径分离：
//
//	/v1/*     动态 API（no-store），含 /v1/notify、/v1/auth/*、/v1/devices、/v1/keys、/v1/stream
//	/*        静态资源（CDN 缓存分级：哈希文件 immutable，入口短缓存+ETag）
//
// 命令行面只有三个：不带参数起服务（配置全部来自环境变量）、`--version`、`-h`。
// `--version` 由 #334 补上：`make build` 通过 -ldflags 往 internal/version 注入
// version/commit/builtAt（中枢升级源也用它当目标版本），没有这个开关的话，
// 构建注入的事实就没有任何可核对的出口。
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/zzjcool/anotify/internal/logging"
	"github.com/zzjcool/anotify/internal/server"
	"github.com/zzjcool/anotify/internal/version"
)

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: anotify")
	fmt.Fprintln(w, "  anotify            起服务（配置全部来自环境变量，见 README「环境变量」）")
	fmt.Fprintln(w, "  anotify --version  打印版本：anotify <version>+<commit> (<builtAt>)")
	fmt.Fprintln(w, "  anotify -h         本帮助")
}

// versionLine 是 --version 的唯一一行输出：version.String() = Version+Commit，
// 与节点 anode 的 main.version 注入值同形（中枢拿它当升级目标版本）。
func versionLine() string {
	built := version.BuiltAt
	if built == "" {
		built = "unknown"
	}
	return fmt.Sprintf("anotify %s (%s)", version.String(), built)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version":
			fmt.Println(versionLine())
			return
		case "-h", "--help", "help":
			usage(os.Stdout)
			return
		}
	}

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
		"version", version.String(),
	)
	if err := http.ListenAndServe(cfg.Addr, mux); err != nil {
		slog.Error("server listen failed",
			"event", "server.fatal",
			"error", err.Error(),
		)
		os.Exit(1)
	}
}
