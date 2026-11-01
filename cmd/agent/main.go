// Command agent 是 Anotify 的**最小节点执行器**：跑在用户侧的独立小进程。
//
// 它做且只做两件事：
//
//  1. 节点模式（默认）：按 node.yaml 连中枢，接单、执行、回报。
//     读 node.yaml → 用节点令牌连 GET /v1/node/stream（Authorization: Bearer <token>）
//     → 发 hello（槽位 + 本机缓存了哪些产物）→ 等 welcome → 心跳 + 收 assign/deploy
//     → 立刻 ack → 在独立工作目录里直接 exec 产物入口 → 回 result
//
//  2. 本地模式（`anotify-agent run ./capability.yaml -p k=v`）：不连中枢，
//     把一份产物 YAML 拷到任意机器上直接跑一次。这是「产物可移植」的落地证明——
//     同一份文件，中枢推下来能跑，scp 到另一台机器上也能跑。
//
// 边界（不变量 I1：**中枢不执行**，节点纯执行）：
//
//   - assign 帧里没有任何命令文本，只有 capability / version / params / digest。
//     执行内容来自本地产物缓存，产物是控制台编译出来的、内容寻址（digest）、
//     不可变、可审计的（见 cache.go）。
//   - 执行方式是直接 exec 入口文件（靠 shebang），**不经 shell**：没有 shell 就没有
//     插值/拼接/引号问题（见 runner.go）。
//   - 参数只走环境变量（ANOTIFY_PARAMS / ANOTIFY_PARAM_<NAME>）。
//   - 资源上限（并发槽位数）由节点自己的启动参数决定，中枢推不动（裁决 D5）。
//
// 本文件只做「读参数 → 装日志 → 等信号 → 交给 runNode / runLocal」这几件事：
// 连接与调度在 client.go，部署应用在 apply.go，缓存落盘在 cache.go，执行语义在
// runner.go，配置解析在 config.go。入口薄一层，跨层职责才不会糊到一起
// （也才能不联网地测参数/配置这段）。
//
// 用法：
//
//	anotify-agent -f ./node.yaml -grace 20 --data-dir ~/.anotify
//	anotify-agent run ./capability.yaml -p repo=acme/x -p pr=42 --timeout 600
//
//	-f          node.yaml 路径（默认 ./node.yaml）
//	-grace      优雅退出等待秒数（默认 20；<=0 表示退出时不等在跑任务）
//	-data-dir   数据目录（默认 ~/.anotify）：产物缓存、索引、任务工作目录都在它下面
//
// -grace 的边界（运维必读）：它**只覆盖在跑任务的收尾**，**不包含关闭连接本身**
// （关连接是收尾之后的下一步），极端情况下进程总耗时会比 grace 略长一点。
// 所以 -grace 应当小于进程管理器的 stop 超时（如 docker stop 默认 10s），否则
// 来不及收尾就被 SIGKILL，等于白等。
//
// 退出码：0 = 正常（含 -h 看帮助）；1 = 参数/配置错误、握手被拒、连接断开。
// `run` 子命令是例外：它把**产物自己**的退出码原样透传（脚本怎么退，agent 就怎么退），
// 只有 agent 侧故障（产物解析失败、起不来）才用 1。
// 节点模式不做断线退避重连：连接断了就退出，重启交给进程管理器。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
)

const (
	// defaultConfigPath 是 -f 的缺省值：约定在启动目录放一份 node.yaml。
	defaultConfigPath = "./node.yaml"
	// defaultGraceSec 是 -grace 的缺省值（秒）：留 20 秒给在跑任务收尾，
	// 够一般的通知脚本跑完，又不会让 docker stop / systemd 等太久。
	// 注意 20 只算在跑任务的收尾、不含关连接本身（见上面包的注释），且 -grace
	// 应小于进程管理器的 stop 超时（docker stop 默认只有 10s，那种环境要调小）。
	defaultGraceSec = 20

	// exitOK / exitFailure 是本进程自己的两个退出码。
	// `run` 子命令透传产物的退出码，所以失败码只用于「agent 自己出问题」。
	exitOK      = 0
	exitFailure = 1

	// runCommand 是 standalone 子命令名：anotify-agent run <path>
	runCommand = "run"
)

// options 是节点模式命令行解析出来的启动参数（parseFlags 的产物，便于单测比对）。
type options struct {
	configPath string        // -f：node.yaml 路径
	grace      time.Duration // -grace：已经归一，<=0 表示不等在跑任务
	dataDir    string        // -data-dir：缺省空串 = 用 defaultDataDir()
}

// runOptions 是 standalone 模式（run 子命令）的参数。
type runOptions struct {
	path       string     // 产物 YAML 路径（位置参数）
	params     paramFlags // -p：key=value，可重复
	dataDir    string     // -data-dir：工作目录根（空 = 系统临时目录）
	timeoutSec int        // -timeout：超时秒数，>0 时压过产物自己的 timeoutSec
}

// paramFlags 是 -p 的累加器。
//
// 值**一律当字符串**：`-p pr=42` 得到 "42" 而不是 42。简单优先——参数最终都通过
// 环境变量进脚本（ANOTIFY_PARAM_*），环境变量本来就是文本；提前猜类型只会让
// `-p code=007` 变成 7 这种惊吓。
type paramFlags map[string]any

// String 实现 flag.Value（调试用）。
func (p paramFlags) String() string {
	parts := make([]string, 0, len(p))
	for k, v := range p {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, ",")
}

// Set 解析一条 key=value。
func (p paramFlags) Set(raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("参数 %q 不是 key=value 形状", raw)
	}
	p[key] = value
	return nil
}

// parseFlags 解析节点模式的命令行参数。
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
	graceSec := fs.Int("grace", defaultGraceSec,
		"优雅退出等待秒数（<=0 表示不等在跑任务）；只覆盖在跑任务的收尾、"+
			"不含关连接本身，应小于进程管理器的 stop 超时（如 docker stop 默认 10s）")
	dataDir := fs.String("data-dir", "",
		"数据目录（默认 ~/.anotify）：产物缓存、索引、任务工作目录都在它下面")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	return options{
		configPath: *configPath,
		grace:      graceDuration(*graceSec),
		dataDir:    *dataDir,
	}, nil
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
// 写 stderr 而不是 stdout：节点的 stdout 没有产物，日志混进调用方的管道只会污染它
// （docker logs / systemd journal 两路都收，不影响排障）。standalone 模式尤其要紧：
// 那条路径的 stdout 属于**脚本**。
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// resolveDataDir 归一 -data-dir：空串 → ~/.anotify，其余转成绝对路径。
//
// 转绝对路径是因为缓存与工作目录都要拼在它下面：相对路径会随 cwd 漂移
// （systemd 的 WorkingDirectory、docker 的 -w 都能改），那种「同一个 flag 两处
// 解析出两个目录」的问题极难查。
func resolveDataDir(flagValue string) (string, error) {
	dir := strings.TrimSpace(flagValue)
	if dir == "" {
		d, err := defaultDataDir()
		if err != nil {
			return "", err
		}
		dir = d
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("数据目录 %q: %w", dir, err)
	}
	return abs, nil
}

// run 是节点模式的可测内核：解析参数 → 读配置 → 装日志 → 等信号 → 交给 runNode。
//
// 返回 nil 表示正常退出（信号收尾完毕，或 -h 看帮助）；返回错误时**已经**用本
// 进程的 logger 记过一条 error，main 只负责把它翻成退出码 1。
//
// 参数错、node.yaml 不存在/不合法、数据目录算不出来这几类失败都发生在 dial 之前，
// **完全离线**，所以测试用坏参数/坏配置就能覆盖入口这一段。
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

	dataDir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		log.Error("数据目录不可用", "event", "agent.datadir.failed", "error", err.Error())
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
		"slots", cfg.slots(),
		"data_dir", dataDir,
		"cached_artifacts", len(newCache(dataDir).applied()),
		"grace", opts.grace.String(),
	)

	// 信号：SIGINT（Ctrl-C）与 SIGTERM（systemd、docker stop、进程管理器）走同一条
	// 优雅退出路径——runNode 内部先置 draining，再给在跑任务最多 grace 收尾，
	// 最后正常关闭连接（服务端据此区分「下线」与「掉线」）。grace 计的只是中间那步
	// 收尾，关连接不计入（所以总耗时可能比 grace 略长）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runNode(ctx, cfg, dataDir, log, opts.grace); err != nil {
		log.Error("节点退出", "event", "agent.fatal", "error", err.Error())
		return err
	}
	log.Info("节点已正常退出", "event", "agent.stop")
	return nil
}

// parseRunFlags 解析 `run` 子命令的参数。
//
// 位置参数（产物路径）允许出现在 flag 前后：
//
//	anotify-agent run ./x.yaml -p a=1
//	anotify-agent run -p a=1 ./x.yaml
//
// 做法是两次 Parse：第一次停在首个位置参数，把它取出来之后再解析剩下的。
// 自己手写「哪个裸参数是 flag 的值」容易踩错（-p 就吃一个值），交给 flag 包更稳。
func parseRunFlags(args []string, stderr io.Writer) (runOptions, error) {
	fs := flag.NewFlagSet("agent run", flag.ContinueOnError)
	fs.SetOutput(stderr)

	params := paramFlags{}
	fs.Var(params, "p", "能力参数 key=value（可重复；值一律当字符串）")
	dataDir := fs.String("data-dir", "", "工作目录根（缺省用系统临时目录）")
	timeoutSec := fs.Int("timeout", 0, "超时秒数（0 = 用产物自己的 timeoutSec）")

	if err := fs.Parse(args); err != nil {
		return runOptions{}, err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return runOptions{}, errors.New(
			"用法: anotify-agent run <path-to-capability.yaml> [-p key=value]... [--data-dir DIR] [--timeout SEC]")
	}
	path := rest[0]
	if len(rest) > 1 {
		// 路径写在前面：剩下的 flag 再解析一遍（同一个 FlagSet 可重复 Parse）。
		if err := fs.Parse(rest[1:]); err != nil {
			return runOptions{}, err
		}
		if extra := fs.Args(); len(extra) > 0 {
			return runOptions{}, fmt.Errorf("多余的参数: %s", strings.Join(extra, " "))
		}
	}
	return runOptions{
		path:       path,
		params:     params,
		dataDir:    *dataDir,
		timeoutSec: *timeoutSec,
	}, nil
}

// runStandalone 是 `anotify-agent run <yaml>` 的实现：不连中枢，跑一次，走人。
//
// 这条路径是「产物可移植」的落地（验收标准 4）：不读 node.yaml、不建索引、
// 不碰缓存——产物只在一个**临时工作目录**里展开并执行，退出码原样透传。
//
// 这也是排查线上问题最快的方式：把中枢上那版产物另存成文件，`-p` 填同样的参数，
// 在任意一台机器上复现，不用碰中枢也不用连网。
func runStandalone(args []string, stdout, stderr io.Writer) (int, error) {
	log := newLogger(stderr)

	opts, err := parseRunFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil // -h：usage 已由 flag 包写到 stderr，退出码 0
		}
		return exitFailure, err
	}

	data, err := os.ReadFile(opts.path)
	if err != nil {
		return exitFailure, fmt.Errorf("%s: %w", opts.path, err)
	}
	// Decode = Parse（全部校验）+ 算 digest。手改过的产物一样能跑（它就是本地变体），
	// 但**不合法的产物一定跑不起来**：入口没有 shebang 这类问题会在这里被拒。
	art, digest, err := artifact.Decode(data)
	if err != nil {
		return exitFailure, fmt.Errorf("%s: %w", opts.path, err)
	}

	// 工作目录根：显式给了 --data-dir 就落在它下面（失败时保留现场便于复查），
	// 否则用系统临时目录——standalone 不该在用户家目录里凭空建 ~/.anotify。
	workRoot := ""
	if dir := strings.TrimSpace(opts.dataDir); dir != "" {
		abs, aerr := filepath.Abs(dir)
		if aerr != nil {
			return exitFailure, fmt.Errorf("数据目录 %q: %w", dir, aerr)
		}
		workRoot = filepath.Join(abs, workSubdirName)
	}

	log.Info("standalone run",
		"event", "agent.run.start",
		"path", opts.path,
		"capability", art.Metadata.Name,
		"version", art.Metadata.Version,
		"digest", digest,
		"params", len(opts.params),
	)

	// 信号：Ctrl-C 要杀的是**脚本**（进程组），而不是让 agent 自己闷死。
	// 取消 ctx → newArtifactCmd 的 Cancel 会杀掉整个进程组（见 runner.go）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	j := job{
		// 本地执行没有中枢任务，但 ANOTIFY_TASK_ID 仍要给一个有意义的常量：
		// 空串会让脚本里 `"$ANOTIFY_TASK_ID"` 变成空路径片段，比报错更难查。
		TaskID:          "local",
		Artifact:        art,
		Digest:          digest,
		Params:          map[string]any(opts.params),
		TimeoutOverride: opts.timeoutSec,
		WorkRoot:        workRoot,
	}

	code, err := runLocal(ctx, j, stdout, stderr)
	if err != nil {
		return code, err
	}
	log.Info("standalone finished", "event", "agent.run.stop", "exit_code", code)
	return code, nil
}

// main 只做一件事：决定走哪条路径，把结果翻成退出码。
func main() {
	os.Exit(mainWith(os.Args[1:], os.Stdout, os.Stderr))
}

// mainWith 是 main 的可测版本（退出码是返回值，不是 os.Exit）。
func mainWith(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == runCommand {
		code, err := runStandalone(args[1:], stdout, stderr)
		if err != nil {
			// 错误统一在这里落一条日志（runStandalone 只返回，不重复打）。
			newLogger(stderr).Error("standalone 执行失败",
				"event", "agent.run.failed", "error", err.Error())
			return exitFailure
		}
		return code
	}
	// 节点模式的错误日志已经在 run 里记过（这里再记一条就成了重复）。
	if err := run(args, stderr); err != nil {
		return exitFailure
	}
	return exitOK
}
