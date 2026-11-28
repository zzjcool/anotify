// Anotify 服务端单二进制入口。
//
// 路径分离：
//
//	/v1/*     动态 API（no-store），含 /v1/auth/*、/v1/keys、/v1/namespaces/*、/v1/node/stream
//	/*        静态资源（CDN 缓存分级：哈希文件 immutable，入口短缓存+ETag）
//
// 命令行面只有三个：不带参数起服务（配置全部来自环境变量）、`--version`、`-h`。
// `--version` 由 #334 补上：`make build` 通过 -ldflags 往 internal/version 注入
// version/commit/builtAt（中枢升级源也用它当目标版本），没有这个开关的话，
// 构建注入的事实就没有任何可核对的出口。
package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

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
		slog.Error("配置非法，拒绝启动",
			"event", "config.invalid",
			"error", err.Error(),
		)
		os.Exit(1)
	}

	mux := server.NewMux(cfg)

	// pprof 走**独立监听**（#624）：不与 ANOTIFY_ADDR 共用端口。
	//
	// 旧实现挂在业务 mux 上，而 ANOTIFY_ADDR 默认 `:8080`（全接口）——开关一开就把
	// goroutine 栈与堆分布暴露到业务监听面，pprof 又没有任何鉴权。现在暴露面只由
	// ANOTIFY_PPROF_ADDR 决定（默认 127.0.0.1:6060，仅回环）。
	if cfg.Pprof {
		slog.Warn("pprof 已开启（性能评估用，独立监听）",
			"event", "server.pprof.enabled",
			"addr", cfg.PprofAddr,
		)
		pprofSrv := &http.Server{
			Addr:              cfg.PprofAddr,
			Handler:           server.NewPprofMux(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			// 监听失败不改业务服务的命运（pprof 是可选能力）：记一条 ERROR 继续。
			// ErrServerClosed 是正常关闭，不算失败。
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("pprof 监听失败（业务服务继续运行）",
					"event", "server.fatal",
					"component", "pprof",
					"addr", cfg.PprofAddr,
					"error", err.Error(),
				)
			}
		}()
	}

	slog.Info("server started",
		"event", "server.start",
		"addr", cfg.Addr,
		"static_dir", cfg.StaticDir,
		"cdn_prefix", cfg.CDNPrefix,
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
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
