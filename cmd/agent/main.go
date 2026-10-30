// Command agent 是 Anotify 的**最小节点执行器**：跑在用户侧的独立小进程。
//
// 它做且只做一件事——按本地 node.yaml 声明的能力接单并执行：
//
//	读 node.yaml → 用节点令牌连 GET /v1/node/stream（Authorization: Bearer <token>）
//	→ 发 hello（capabilities + manifest）→ 等 welcome → 心跳 + 收 assign
//	→ 立刻 ack → 执行 → 回 result
//
// 边界（不变量 I1：**中枢不执行**）：
//
//   - 命令只有一个来源：本地 node.yaml 的 run.command。assign 帧里**没有**命令，
//     agent 也从不向服务端索要执行方式；中枢只存 run 原文、只校验形状、从不执行。
//   - 参数只走环境变量（ANOTIFY_PARAMS / ANOTIFY_PARAM_<NAME>）。run.command 是
//     原文，**不做** {{param}} 插值、不拼接 shell 片段，原样交给 sh -c。
//
// 本文件只做「读参数 → 装日志 → 等信号 → 交给 runNode」这四件事：连接与调度在
// client.go，执行语义在 runner.go，配置解析在 config.go。入口薄一层，
// 跨层职责才不会糊到一起（也才能不联网地测参数/配置这段）。
//
// 用法：
//
//	go run ./cmd/agent -f ./node.yaml -grace 20
//
//	-f      node.yaml 路径（默认 ./node.yaml）
//	-grace  优雅退出等待秒数（默认 20；<=0 表示退出时不等在跑任务）
//
// 退出码：0 = 收到 SIGINT/SIGTERM 正常收尾（含 -h 看帮助）；1 = 参数/配置错误、
// 握手被拒、连接断开。v1 不做断线退避重连：连接断了就退出，重启交给进程管理器。
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	// defaultConfigPath 是 -f 的缺省值：约定在启动目录放一份 node.yaml。
	defaultConfigPath = "./node.yaml"
	// defaultGraceSec 是 -grace 的缺省值（秒）：留 20 秒给在跑任务收尾，
	// 够一般的通知脚本跑完，又不会让 docker stop / systemd 等太久。
	defaultGraceSec = 20
)

// options 是命令行解析出来的启动参数（parseFlags 的产物，便于单测直接比对）。
type options struct {
	configPath string        // -f：node.yaml 路径
	grace      time.Duration // -grace：已经归一，<=0 表示不等在跑任务
}

// parseFlags 解析命令行参数。
//
// args **不含程序名**（main 传 os.Args[1:]），测试可以直接喂字符串切片。
// 每个 run/测试各自 NewFlagSet，不碰全局 flag.CommandLine：解析互不干扰，
// 测试可以并行。
//
// 用 ContinueOnError 而不是 ExitOnError：退出码只由 main 决定，解析失败时
// flag 包自己打的 usage 留在 stderr，本函数只把错误交回去。
func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)

	configPath := fs.String("f", defaultConfigPath, "node.yaml 路径")
	graceSec := fs.Int("grace", defaultGraceSec, "优雅退出等待秒数（<=0 表示不等在跑任务）")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	return options{configPath: *configPath, grace: graceDuration(*graceSec)}, nil
}

// graceDuration 把 -grace 的秒数翻成 time.Duration。
//
// 负数按 0 处理：client 的 drain 把 grace <= 0 解释成「不等在跑任务，直接取消」
// （契约 §3.4.6），所以负值既不会 panic、也不会变成"永远等"。归一在这里一次
// 做完，下游只面对「0 = 不等 / >0 = 最多等这么久」两种语义。
func graceDuration(sec int) time.Duration {
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

// newLogger 是进程级日志：文本格式、INFO 级、写 stderr。
//
// 写 stderr 而不是 stdout：本程序没有输出到 stdout 的产物，日志混进调用方的
// 管道只会污染它（docker logs / systemd journal 两路都收，不影响排障）。
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// run 是 main 的可测内核：解析参数 → 读配置 → 装日志 → 等信号 → 交给 runNode。
//
// 返回 nil 表示正常退出（信号收尾完毕，或 -h 看帮助）；返回错误时**已经**用本
// 进程的 logger 记过一条 error，main 只负责把它翻成退出码 1。
//
// 参数错、node.yaml 不存在/不合法这三类失败都发生在 dial 之前，**完全离线**，
// 所以测试用坏参数/坏配置就能覆盖入口这一段（main_test.go 就停在这一层）。
func run(args []string, stderr io.Writer) error {
	log := newLogger(stderr)

	opts, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h/-help：用户主动要帮助。usage 已由 flag 包写到 stderr，退出码 0。
			return nil
		}
		log.Error("命令行参数不正确", "event", "agent.flags.failed", "error", err.Error())
		return err
	}

	cfg, err := loadConfig(opts.configPath)
	if err != nil {
		// loadConfig 的错误已经带文件路径前缀（config.go），这里不再套一层路径，
		// 免得同一条路径在报错里出现两次。
		log.Error("node.yaml 加载失败",
			"event", "agent.config.failed",
			"path", opts.configPath,
			"error", err.Error(),
		)
		return err
	}

	if opts.grace <= 0 {
		log.Warn("grace <= 0：退出时不再等在跑任务",
			"event", "agent.grace.disabled",
			"grace", opts.grace.String(),
		)
	}

	log.Info("agent starting",
		"event", "agent.start",
		"node", cfg.Name,
		"server", cfg.Server,
		"capabilities", len(cfg.Capabilities),
		"slots", cfg.slots(),
		"grace", opts.grace.String(),
	)

	// 信号：SIGINT（Ctrl-C）与 SIGTERM（systemd、docker stop、进程管理器）走同一条
	// 优雅退出路径——runNode 内部先置 draining，再给在跑任务最多 grace 收尾，
	// 最后正常关闭连接（服务端据此区分「下线」与「掉线」）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runNode(ctx, cfg, log, opts.grace); err != nil {
		log.Error("节点退出", "event", "agent.fatal", "error", err.Error())
		return err
	}
	log.Info("节点已正常退出", "event", "agent.stop")
	return nil
}

// main 只做一件事：把 run 的结果翻成退出码（错误日志已经在 run 里记过，
// 这里再记一条就成了重复）。
func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		os.Exit(1)
	}
}
