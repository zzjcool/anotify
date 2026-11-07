// Command agent 是 Anotify 的**最小节点执行器**：跑在用户侧的独立小进程。
//
// 它做且只做三件事：
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
//  3. 自升级（`anotify-agent upgrade`）：从**中枢**（不是一个 GitHub release）下载
//     当前版本的二进制、校验 sha256、原子替换自己。见 upgrade.go。
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
//	anotify-agent upgrade [--check] [--force] [--server URL] [--install-to PATH] [--restart]
//	anotify-agent runs [--trace ID | --capability NAME[@VER]] [--limit N]   # 看本机跑过的执行
//	anotify-agent show <trace>/<capability>@<ver>/<seq>-a<n>                # 看那一次的输入与输出
//	anotify-agent check ./capability.yaml                                 # 只校验一份能力 yaml（不执行）
//	anotify-agent schema [node|capability|emit]                            # 打印这三份形状的 JSON Schema
//	anotify-agent capabilities                                           # 本机能跑什么（读 index/）
//	anotify-agent status                                                 # 本机 daemon 现在怎么样（读 state.json）
//
//	-f          node.yaml 路径（默认 ./node.yaml）
//	-grace      优雅退出等待秒数（默认 20；<=0 表示退出时不等在跑任务）
//	-data-dir   数据目录（默认 ~/.anotify）：产物缓存、索引、任务工作目录、执行档案都在它下面
//
// -grace 的边界（运维必读）：它**只覆盖在跑任务的收尾**，**不包含关闭连接本身**
// （关连接是收尾之后的下一步），极端情况下进程总耗时会比 grace 略长一点。
// 所以 -grace 应当小于进程管理器的 stop 超时（如 docker stop 默认 10s），否则
// 来不及收尾就被 SIGKILL，等于白等。
//
// 退出码：0 = 正常（含 -h 看帮助）；1 = 参数/配置错误、令牌被拒、协议不兼容、
// 中枢在进程内退避重试之后仍被放弃（见下）。
// `run` 子命令是例外：它把**产物自己**的退出码原样透传（脚本怎么退，agent 就怎么退），
// 只有 agent 侧故障（产物解析失败、起不来）才用 1。
// `upgrade` 子命令也是例外：它的 0..5 是契约 §4 的升级结论（见 upgrade.go）。
//
// 节点模式的连接失败分两类（issue #323，判据表在 retry.go）：
//
//   - **瞬态**（拨号失败、5xx/530、网络/握手超时、连接断开）：**进程内**指数退避
//     重连（1s→2s→4s…封顶 30s，带 jitter），进程不退出；成功 handshake 后重置。
//     这一条是为了不让「中枢/隧道不可达」变成 systemd 的 3 秒一次崩溃循环。
//   - **永久**（令牌 401/403、协议版本不兼容、实例数超顶、node.yaml 坏、data-dir 不可用）：
//     立刻退出非 0，日志给一句人话 + `code=` 短码，交给 systemd/人处理。
//
// `-no-retry` 把节点模式退回「只试一次」：安装脚本的预检用它，因为预检要的是
// 「现在能不能连上」这个立刻的答案，不是「等中枢恢复」。
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

	// exitTimeout 是 **agent 自己判定超时**时的退出码（issue #320 §4）：
	// GNU timeout 的惯例（124），与 hc 一致。
	//
	// 它只在「ctx 到点、我们杀了进程组」时使用；**不改写**产物自己的退出码——
	// 脚本自己 `exit 124` 时无法区分，照样原样透传（这个歧义写进了 run -h）。
	exitTimeout = 124

	// runCommand 是 standalone 子命令名：anotify-agent run <path>
	runCommand = "run"

	// emitCommand 是中途产出子命令名：anotify-agent emit --data '…'（要钉住具体下游才写 --to）
	//
	// 它是**能力进程**调的（不是运维调的）：由 agent 注入的 ANOTIFY_EMIT_SOCK /
	// ANOTIFY_TASK_TOKEN 决定它去哪里、以谁的身份上报（见 emit_cmd.go）。
	emitCommand = "emit"

	// runsCommand / showCommand 是**执行档案回看**子命令名（见 runs_cmd.go）：
	// `runs` 列表、`show` 看一次执行的输入与完整输出。它们是纯粹的本地只读命令——
	// 不连中枢、不读 node.yaml、不落任何盘。
	runsCommand = "runs"
	showCommand = "show"

	// checkCommand / schemaCommand 是**离线书写面**子命令名（见 check_cmd.go /
	// schema_cmd.go，issue #319）：check 校验一份能力 yaml，schema 吐三份 JSON Schema。
	//
	// 它们与 runs / show 同属「不读 node.yaml、不连中枢、不落盘」的本地只读命令，
	// 但服务的是另一类人：**写**能力 yaml 的人（现在越来越多是 coding agent）。
	// check 与 run 的区别是硬红线：check 只 Parse + Validate，**绝不执行**入口文件。
	checkCommand  = "check"
	schemaCommand = "schema"

	// capabilitiesCommand / statusCommand 是**本机事实面**子命令名（issue #320）：
	//
	//	capabilities —— 本机能跑什么（读 <data-dir>/index，见 capabilities_cmd.go）
	//	status       —— 本机 daemon 现在怎么样（读 <data-dir>/state.json，见 status_cmd.go）
	//
	// 两条都只读本地文件：不连中枢、不开 socket、不落盘。
	capabilitiesCommand = "capabilities"
	statusCommand       = "status"
)

// options 是节点模式命令行解析出来的启动参数（parseFlags 的产物，便于单测比对）。
type options struct {
	configPath string        // -f：node.yaml 路径
	grace      time.Duration // -grace：已经归一，<=0 表示不等在跑任务
	dataDir    string        // -data-dir：缺省空串 = 用 defaultDataDir()
	// noRetry 是 -no-retry：只试一次，不在进程内退避重连（#323）。
	// 安装脚本的预检要的是「现在能不能连上」这个立刻的答案，所以它带这个 flag。
	noRetry bool
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
	// 自定义 usage：flag 包的默认输出只有下面三个 flag，子命令一个都不出现
	// （issue #317）。命令表在 usage.go，flag 说明仍由 PrintDefaults 出自这里。
	fs.Usage = func() { writeUsage(fs) }

	// 缺省空串 = 没显式指文件。真正的回落顺序在 loadConfigFrom：
	// 显式 -f > 环境变量（ANOTIFY_TOKEN 存在即生效）> ./node.yaml。
	// 用一个空缺省值而不是 defaultConfigPath，是为了能区分
	// 「用户点名了这个文件」和「用户什么都没说」——前者读不到必须报错，
	// 绝不能悄悄改成从环境变量读。
	configPath := fs.String("f", "", "node.yaml 路径（不给则先看 ANOTIFY_TOKEN 等环境变量，再退到 ./node.yaml）")
	graceSec := fs.Int("grace", defaultGraceSec,
		"优雅退出等待秒数（<=0 表示不等在跑任务）；只覆盖在跑任务的收尾、"+
			"不含关连接本身，应小于进程管理器的 stop 超时（如 docker stop 默认 10s）")
	dataDir := fs.String("data-dir", "",
		"数据目录（默认 ~/.anotify）：产物缓存、索引、任务工作目录都在它下面")
	noRetry := fs.Bool("no-retry", false,
		"连不上中枢时只试一次就退出（不在进程内退避重连）；预检/一次性探测用")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	return options{
		configPath: *configPath,
		grace:      graceDuration(*graceSec),
		dataDir:    *dataDir,
		noRetry:    *noRetry,
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

	cfg, err := loadConfigFrom(opts.configPath, os.Getenv)
	if err != nil {
		// loadConfig 的错误已经带文件路径前缀（config.go），这里不再套一层路径，
		// 免得同一条路径在报错里出现两次。
		//
		// 包成 permanentError：这是「不重试」的确定性事实（#323）——重试一千次
		// 也不会把 node.yaml 变合法，而静默挂着比早死更难查。
		fatal := &permanentError{code: failCodeConfigInvalid, reason: failReasonConfigInvalid, err: err}
		log.Error("配置加载失败",
			"event", "agent.config.failed",
			"code", fatal.code,
			"path", opts.configPath,
			"error", err.Error(),
		)
		return fatal
	}

	dataDir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		fatal := &permanentError{code: failCodeDataDirUnavailable, reason: failReasonDataDirUnavailable, err: err}
		log.Error("数据目录不可用", "event", "agent.datadir.failed", "code", fatal.code, "error", err.Error())
		return fatal
	}

	if opts.grace <= 0 {
		log.Warn("grace <= 0：退出时不再等在跑任务",
			"event", "agent.grace.disabled",
			"grace", opts.grace.String(),
		)
	}

	// 重连策略（#323）：瞬态退避重试、永久退出。`-no-retry` 退回「只试一次」。
	policy := defaultBackoffPolicy()
	policy.Disabled = opts.noRetry

	log.Info("agent starting",
		"event", "agent.start",
		"node", cfg.Name,
		"server", cfg.Server,
		"slots", cfg.slots(),
		"data_dir", dataDir,
		"cached_artifacts", len(newCache(dataDir).applied()),
		"grace", opts.grace.String(),
	)
	if policy.Disabled {
		log.Info("进程内重连已关闭（-no-retry）：连不上就退出",
			"event", "agent.retry.disabled",
		)
	}

	// 信号：SIGINT（Ctrl-C）与 SIGTERM（systemd、docker stop、进程管理器）走同一条
	// 优雅退出路径——runNode 内部先置 draining，再给在跑任务最多 grace 收尾，
	// 最后正常关闭连接（服务端据此区分「下线」与「掉线」）。grace 计的只是中间那步
	// 收尾，关连接不计入（所以总耗时可能比 grace 略长）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := reconnectLoop(ctx, log, policy, func(ctx context.Context, hooks sessionHooks) error {
		return runNode(ctx, cfg, dataDir, log, opts.grace, hooks)
	}); err != nil {
		// 走到这里的只有**不可重试**的失败（瞬态在 reconnectLoop 里退避重试，
		// 信号退出返回 nil）。code 是分类短码：运维按它检索，比 grep 中文稳。
		failure := classifyNodeFailure(err)
		log.Error("节点退出",
			"event", "agent.fatal",
			"code", failure.code,
			"reason", failure.reason,
			"error", err.Error(),
		)
		return err
	}
	log.Info("节点已正常退出", "event", "agent.stop")
	return nil
}

// runUsage / runExitRows 是 `run` 子命令的用法与退出码表。
//
// `run` 的退出码大部分不归我们管（透传产物自己），这一行表就是为了说清这件事：
// 拿到 2 不代表 agent 出了 2 类错，而是脚本自己 exit 2。
const runUsage = "anotify-agent run <path-to-capability.yaml> [-p key=value]... " +
	"[--data-dir DIR] [--timeout SEC]"

// runHelpNote 是 `run -h` 里关于「落档」与「124 歧义」的两段说明（issue #320 §3/§4）。
const runHelpNote = `给了 --data-dir 时，这次执行会落进 <data-dir>/runs（本地 trace 命名空间 local_<ts>），
事后可以用 anotify-agent runs / show <ref> 回看；没给则用系统临时目录且**不落档**（stderr 会明说一行）。
超时 124 只在 **agent 自己判定超时** 时使用；产物自己 exit 124 时无法区分，照样原样透传、不改写。`

var runExitRows = []exitRow{
	{code: "0..N", desc: "产物自己的退出码，原样透传（脚本怎么退，agent 就怎么退）"},
	{code: "124", desc: "agent 自己判定超时（--timeout / 产物的 timeoutSec 到点）；产物自己 exit 124 同样透传，无法区分"},
	{code: "1", desc: "agent 侧故障：产物不合法 / 起不来 / 参数错（kind 直接是 artifact 的短码）",
		kind: "invalid_yaml / unknown_field / invalid_name / unsupported_api_version …"},
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
	fs.Usage = func() { writeSubcommandHelp(fs, runUsage, runExitRows, runHelpNote) }

	params := paramFlags{}
	fs.Var(params, "p", "能力参数 key=value（可重复；值一律当字符串）")
	dataDir := fs.String("data-dir", "", "工作目录根（缺省用系统临时目录）")
	timeoutSec := fs.Int("timeout", 0, "超时秒数（0 = 用产物自己的 timeoutSec）")
	// 接受并忽略 --json：run 的 stdout 属于**产物自己**（issue #318 §1 的例外），
	// 但全局 --json 可能就写在子命令前面（`anotify-agent --json run x.yaml`），
	// 不认它就会变成「全局开关让子命令报错」这种莫名其妙的失败。
	_ = fs.Bool("json", false, "对 run 无效果：stdout 属于产物自己（--json 只影响错误那一路）")

	if err := fs.Parse(args); err != nil {
		return runOptions{}, err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return runOptions{}, fmt.Errorf("用法: %s", runUsage)
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
// 给了 `--data-dir` 时多一件事（issue #320 §3）：把这次执行落进
// `<data-dir>/runs/`（与中枢派单同一条 runs.Save），trace 用本地命名空间
// `local_<ts>`。于是超时/失败之后可以说「去 anotify-agent show <ref> 看」，
// 而不是只有一行 workdir。没给 `--data-dir` 时仍然不落档（临时目录会被清，
// 落一份找不到的档案比不落更坏），但 stderr 会**明说一行**——不能静默丢。
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
	// Decode = Parse（全部校验）+ 算 digest。手改过的产物一样能跑（只是 digest 变了，
	// 中枢握手时会发现并重新下发），但**不合法的产物一定跑不起来**：入口没有 shebang
	// 这类问题会在这里被拒。
	art, digest, err := artifact.Decode(data)
	if err != nil {
		return exitFailure, fmt.Errorf("%s: %w", opts.path, err)
	}

	// 数据目录（可选）：显式给了才落档，也才把工作目录建在它下面。
	var dataDir string
	if dir := strings.TrimSpace(opts.dataDir); dir != "" {
		abs, aerr := filepath.Abs(dir)
		if aerr != nil {
			return exitFailure, fmt.Errorf("数据目录 %q: %w", dir, aerr)
		}
		dataDir = abs
	}
	// 工作目录根：显式给了 --data-dir 就落在它下面（失败时保留现场便于复查），
	// 否则用系统临时目录——standalone 不该在用户家目录里凭空建 ~/.anotify。
	workRoot := ""
	if dataDir != "" {
		workRoot = filepath.Join(dataDir, workSubdirName)
	}
	// 档案库句柄：nil = 不落档（见文件头）。
	var store *runStore
	if dataDir != "" {
		store = newRunStore(dataDir, log)
	}

	log.Info("standalone run",
		"event", "agent.run.start",
		"path", opts.path,
		"capability", art.Metadata.Name,
		"version", art.Metadata.Version,
		"digest", digest,
		"params", len(opts.params),
		"archive", store != nil,
	)

	// 信号：Ctrl-C 要杀的是**脚本**（进程组），而不是让 agent 自己闷死。
	// 取消 ctx → newArtifactCmd 的 Cancel 会杀掉整个进程组（见 runner.go）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	j := job{
		// 本地执行没有中枢任务，但 ANOTIFY_TASK_ID 仍要给一个有意义的常量：
		// 空串会让脚本里 `"$ANOTIFY_TASK_ID"` 变成空路径片段，比报错更难查。
		TaskID:          localRunTaskID,
		Artifact:        art,
		Digest:          digest,
		Params:          map[string]any(opts.params),
		TimeoutOverride: opts.timeoutSec,
		WorkRoot:        workRoot,
		// 给了 --data-dir 才采完整输出（→ 档案）；否则一个字节也不多存（人就在终端前）。
		CaptureFull: store != nil,
		// 本地模式也给出口契约：能力自己声明了 outputs，脚本就可能真的调用
		// `emit`（然后拿到"本地模式无中枢"那条明确报错，而不是"读不到契约"）。
		// 真实派发时这份契约来自 assign 帧，这里来自能力定义本身——同一形状。
		Ports: append([]string(nil), outputPorts(art)...),
		// 本地模式也记「声明的工作目录 → 展开后的真实路径」：用户在终端前跑，
		// 更需要知道 ~/x 到底落在哪。
		Log: log,
	}

	started := time.Now()
	code, out, runErr := runLocal(ctx, j, stdout, stderr)
	finished := time.Now()

	// 落档在日志/提示之前：提示里的 ref 必须是**已经落盘**的那一个（指向一份
	// 不存在的档案比不给 ref 更坏）。写盘失败只 Warn，不改退出码（同 runs 的纪律）。
	ref := ""
	if store != nil {
		rec := localRunRecord(art, opts.params, localTraceID(started), out, started, finished)
		ref = archiveLocalRun(store, rec, log)
	} else {
		fmt.Fprintln(stderr, "anotify-agent: 未指定 --data-dir，本次执行不落档"+
			"（runs / show 看不到它；加 --data-dir ~/.anotify 就能回看）")
	}
	// 超时提示（issue #320 §4）：只有当 **agent 自己**判定超时才打，产物自己
	// exit 124 时 TimedOut 为 false——两件事不能混成一句话。
	if out.TimedOut {
		writeLocalTimeoutHint(stderr, dataDir, ref)
	}

	if runErr != nil {
		return code, runErr
	}
	log.Info("standalone finished",
		"event", "agent.run.stop", "exit_code", code, "ref", ref)
	return code, nil
}

// main 只做一件事：决定走哪条路径，把结果翻成退出码。
func main() {
	os.Exit(mainWith(os.Args[1:], os.Stdout, os.Stderr))
}

// subcommandAt 找出参数里真正的子命令位置，允许它出现在全局 flag **之后**
// （`anotify-agent -f node.yaml upgrade`）。
//
// 为什么必须处理：子命令分派原先只看 args[0]。于是
//
//	anotify-agent -f ~/.anotify/node.yaml upgrade --check
//
// 不是「检查更新」，而是**静默跑起一个常驻 agent**：它连上中枢、占用节点连接、
// 然后被 systemd 那份顶掉，退出码是 1 —— 用户看到的是「升级命令报错了」，
// 实际发生的是「起了第二个节点进程」。systemd 的 ExecStart 就是 `-f … ` 这个形状，
// 照着抄一次就会踩中。
//
// 实现：用全局 flag 集合解析（flag 包在第一个非 flag 处停下），剩下的第一个
// token 如果是已知子命令，就把它前面那些**已经解析过的**全局 flag 原样带上
// （子命令自己有同名 flag，必须还能读到 -f）。
//
// 返回 (子命令, 传给子命令的参数, 是否找到)。
func subcommandAt(args []string) (string, []string, bool) {
	if len(args) == 0 {
		return "", nil, false
	}
	if isSubcommand(args[0]) {
		return args[0], args[1:], true
	}
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerGlobalFlagNames(fs)
	// 任何解析错误都按「不是子命令调用」处理：交给主路径去报真正的错。
	if err := fs.Parse(args); err != nil {
		return "", nil, false
	}
	rest := fs.Args()
	if len(rest) == 0 || !isSubcommand(rest[0]) {
		return "", nil, false
	}
	prefix := args[:len(args)-len(rest)]
	return rest[0], append(append([]string{}, prefix...), rest[1:]...), true
}

// registerGlobalFlagNames 把**全局 flag 的名字与取值形状**注册进 fs，只用来定位
// 子命令（值本身不关心，所以全部给零值说明）。
//
// 形状必须与 parseFlags 一致，否则 `-grace 7 upgrade` 会被当成「解析不了 → 不是
// 子命令」而静默走节点模式——正是 subcommandAt 注释里那个「起了第二个节点进程」的坑。
// （-grace 是 int，注册成 Duration 会让 `-grace 7` 报 missing unit，于是不认子命令。）
func registerGlobalFlagNames(fs *flag.FlagSet) {
	fs.String("f", "", "")
	fs.String("data-dir", "", "")
	fs.Int("grace", 0, "")
	// 下面这些是历史/容器环境的 flag 名：现在只用于「flag 带值」时的定位，
	// 保证它们的**值**不会被误当成子命令（`-name upgrade` 是合法的节点名）。
	fs.String("name", "", "")
	fs.String("server", "", "")
	fs.String("token", "", "")
	fs.String("selector", "", "")
	fs.Int("slots", 0, "")
	fs.Bool("standalone", false, "")
	// --json 是 #318 的全局开关：它必须能出现在子命令**之前**（`anotify-agent --json
	// runs`），所以定位子命令时得先认得它。真正的语义在 cliout.go（mode 由 stdout
	// 是否终端与这个 flag 共同决定）。
	fs.Bool("json", false, "")
	// -no-retry 是 #323 的节点模式开关。它必须注册在这里，否则
	// `anotify-agent -no-retry upgrade` 会被当成「解析不了 → 不是子命令」而
	// **静默跑起一个常驻 agent**，正是 subcommandAt 注释里那个坑。
	fs.Bool("no-retry", false, "")
	fs.Bool("h", false, "")
}

// preludeMode 是「进程级开关」的取值：这些开关在任何子命令之前就被处理掉，
// 因为它们必须**不读 node.yaml、不连网、不落盘**（issue #317 §2/§3）。
type preludeMode int

const (
	preludeNone preludeMode = iota
	preludeVersion
	preludeSkill
)

// preludeCommand 报告 args 在**任何子命令之前**是否显式要了 --version / --skill。
//
// 用 FlagSet 定位而不是字符串扫描：flag 包在第一个非 flag 处停下，于是
// `run --version` 里那个 --version 属于 run 的 FlagSet（不是本进程的开关），
// 而 `-f x --version` 会被正确认出来。解析错误按「不是开关」处理，交给主路径报错。
func preludeCommand(args []string) preludeMode {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	wantVersion := fs.Bool("version", false, "")
	wantSkill := fs.Bool("skill", false, "")
	registerGlobalFlagNames(fs)
	if err := fs.Parse(args); err != nil {
		return preludeNone
	}
	switch {
	case *wantVersion:
		return preludeVersion
	case *wantSkill:
		return preludeSkill
	}
	return preludeNone
}

// isSubcommand 报告 token 是否是子命令名（不是的话就是节点模式的普通参数）。
func isSubcommand(tok string) bool {
	return tok == emitCommand || tok == upgradeCommand || tok == runCommand ||
		tok == runsCommand || tok == showCommand ||
		tok == checkCommand || tok == schemaCommand ||
		tok == capabilitiesCommand || tok == statusCommand
}

// mainWith 是 main 的可测版本（退出码是返回值，不是 os.Exit）。
func mainWith(args []string, stdout, stderr io.Writer) int {
	// --version / --skill 在**最前面**处理：它们不读 node.yaml、不连网、不落盘，
	// 所以在空目录、没有 ANOTIFY_TOKEN、甚至断网的机器上都照样可用（issue #317）。
	switch preludeCommand(args) {
	case preludeVersion:
		fmt.Fprintln(stdout, versionLine())
		return exitOK
	case preludeSkill:
		runSkill(stdout)
		return exitOK
	}
	// 子命令出现在任何位置都被规范化成 args[0]（见 subcommandAt 的注释：
	// `-f x upgrade` 曾经被静默解释成「跑常驻 agent」）。分派因此只认一种形状。
	if sub, rest, ok := subcommandAt(args); ok {
		args = append([]string{sub}, rest...)
	}
	// 输出形状先算好：解析参数失败时也要能立刻决定该说 JSON 还是中文
	// （flag 包已经把 usage 写到 stderr，但错误本身由这里报告）。
	mode := outputMode(stdout, args)

	if len(args) > 0 && args[0] == emitCommand {
		// emit 子命令：退出码**就是** emit-protocol.md §2 那张表（2..7），
		// 调用方（能力进程）据此决定重试还是停止，所以不能在这里改写成 1。
		code, err := runEmit(args[1:], os.Stdin, stdout, stderr, os.Getenv)
		// 只在这里打一次：runEmit 返回错误但不打印，避免同一句话出现两遍。
		reportFailure(stderr, mode, emitCommand, code, err)
		return code
	}
	if len(args) > 0 && args[0] == runCommand {
		code, err := runStandalone(args[1:], stdout, stderr)
		if err != nil {
			if mode == outJSON {
				// 管道里连错误也要是机器能读的（kind 复用 artifact 的短码词表）。
				reportFailure(stderr, mode, runCommand, exitFailure, err)
			} else {
				// 错误统一在这里落一条日志（runStandalone 只返回，不重复打）。
				newLogger(stderr).Error("standalone 执行失败",
					"event", "agent.run.failed", "error", err.Error())
			}
			return exitFailure
		}
		return code
	}
	if len(args) > 0 && args[0] == upgradeCommand {
		// 退出码**就是**契约 §4 那张表（0..5），脚本据它决定「去要 root / 别重试 / 重跑」，
		// 所以不能在这里改写成 1。
		return upgradeMain(args[1:], stdout, stderr, os.Getenv)
	}
	if len(args) > 0 && args[0] == runsCommand {
		// 回看子命令：退出码只有「找到 = 0 / 没找到 = 1」两档（见 runs_cmd.go 的文件头），
		// 错误说明只在这里打一次（runRuns 只返回、不打印）。
		code, err := runRuns(args[1:], stdout, stderr)
		reportFailure(stderr, mode, runsCommand, code, err)
		return code
	}
	if len(args) > 0 && args[0] == showCommand {
		code, err := runShow(args[1:], stdout, stderr)
		reportFailure(stderr, mode, showCommand, code, err)
		return code
	}
	if len(args) > 0 && args[0] == checkCommand {
		// 书写面子命令（#319）：退出码 0 全过 / 1 有问题或命令自身出错。
		// 问题列表打在 stdout（那是这条命令的答案），所以这里**通常**没有 error；
		// 只有文件读不到 / 参数形状不对时才有 stderr 那一行。
		code, err := runCheck(args[1:], stdout, stderr)
		reportFailure(stderr, mode, checkCommand, code, err)
		return code
	}
	if len(args) > 0 && args[0] == schemaCommand {
		code, err := runSchema(args[1:], stdout, stderr)
		reportFailure(stderr, mode, schemaCommand, code, err)
		return code
	}
	if len(args) > 0 && args[0] == capabilitiesCommand {
		// 本机事实面（#320 §1）：只读 index/artifacts。**空结果是 0**，不是错误。
		code, err := runCapabilities(args[1:], stdout, stderr)
		reportFailure(stderr, mode, capabilitiesCommand, code, err)
		return code
	}
	if len(args) > 0 && args[0] == statusCommand {
		// 本机事实面（#320 §2）：只读 state.json。stale = 1（最后一份已知状态）。
		code, err := runStatus(args[1:], stdout, stderr)
		reportFailure(stderr, mode, statusCommand, code, err)
		return code
	}
	// 节点模式的错误日志已经在 run 里记过（这里再记一条就成了重复）。
	if err := run(args, stderr); err != nil {
		return exitFailure
	}
	return exitOK
}

// outputPorts 取能力定义里声明的产出端口（本地模式给出口契约用）。
//
// 为什么从 artifact 里读而不是另开参数：本地模式的唯一事实源就是那份 yaml，
// 再传一个 --ports 只会让「定义」与「实际」有两处可写。
func outputPorts(art *artifact.Artifact) []string {
	if art == nil {
		return nil
	}
	return art.Spec.Outputs
}
