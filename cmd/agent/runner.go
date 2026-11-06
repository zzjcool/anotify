package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件是 agent 的**执行层**：把中枢派来的任务（capability + version + params +
// digest）落到本地缓存里的那份产物上，在一个**独立工作目录**里直接 exec 它的入口
// 文件，返回一个 outcome；client 侧只负责把 outcome 编码成 result 帧。
//
// # 与 v1 的三处根本差异
//
//  1. **不再走 `sh -c`**。v1 的命令是 node.yaml 里的一行字符串，只能交给 shell 去
//     解释；v2 的产物自带入口文件（带 shebang），直接 `exec <entrypoint>` 即可。
//     没有 shell 就没有插值、没有拼接、没有引号转义——参数只走环境变量。
//  2. **每个任务一个工作目录**。v1 所有任务共享 agent 的 cwd，8 并发会互相踩
//     （同一个 ./out.json、同一个 git clone 目标）。现在产物文件总展开在
//     <data-dir>/work/<task_id>；cwd 默认就是它，除非能力定义里声明了 run.workDir
//     （那就用那个目录，见 resolveChdir）。
//  3. **执行内容来自产物，不来自 node.yaml**。node.yaml 只剩 bootstrap（见 config.go）。
//
// # 出口（out）：**已退役** —— emit 是唯一的产出通道
//
// 这里曾经有一条「退出时读文件回报出口」的腿：节点把 `$ANOTIFY_OUT_FILE`
// （= <workdir>/.anotify-out）交给脚本，脚本跑完写一行出口名，节点读它、校验 ∈
// `spec.outputs`，再合成 `result.out`。P1.5 把它**整条删掉**（没有兼容层）：
// 产出只有一个通道 —— 运行中 `anotify-agent emit --data …`（端口是内部路由键，见
// docs/design/typed-pipeline.md §2 与 docs/design/emit-protocol.md）。
//
// 为什么必须一刀切掉两条：
//   - 两套协议必然分叉。「一次性返回」只是「只 emit 一次」的特例；留着文件通道，
//     同一个能力就会出现「emit 出去的」与「退出时写的」两份产出，谁先谁后、
//     重复了算几次，全都没有定义。
//   - 端口白名单要能**当场**拒（emit 时），不必等进程退出才发现出口名写错；
//     而文件通道天然是「事后才发现」，失败信息还只剩一行 summary。
//   - 「失败也能继续」不再需要特殊照顾：emit 与 status 无关（I4 的形式不变，
//     载体从 out 变成 emit 的 port）。
//
// 所以本文件现在**不再读写任何出口文件**，也**不再注入** `ANOTIFY_OUT_FILE`：
// 脚本若还写着 `> "$ANOTIFY_OUT_FILE"`，`set -u` 会当场炸（比静默断链好），
// 而任何写死的 `.anotify-out` 文件都不会再被任何人读（见 runner_test.go 的
// TestRunJobIgnoresLegacyOutFile：那条用例就是「这条腿真的断了」的证据）。
//
// 任务的成功/失败仍然只看**进程事实**（退出码 / 信号 / 超时 / 取消），不来自端口。
//
// # 进程组与 WaitDelay（v1 的成果，全部保留）
//
// 杀掉的是**整个进程组**，而不是直接子进程：脚本派生出来的孙进程（`sleep 5 &`）
// 在只杀直接子进程时会活下来，还占着输出管道，Wait 会被拖到它们自己退出。
//
// 另一个坑是「自己脱离了进程组、却继承了管道」的孤儿（`setsid sleep 3 &`）：
// 杀组杀不到它，Wait 要等它关管道。cmd.WaitDelay 是那个兜底（不是超时本体）。
//
// 可测性：runJob / runLocal 都是纯函数式入口（发起、等待、把结果翻译成
// outcome / 退出码），不读全局状态；工作目录由 job.WorkRoot 决定，测试给临时目录。

// job 是一次执行请求：中枢能表达的全部（能力名、版本、参数、租约兜底、digest、
// trace_id）加上本地缓存里**找到的那份产物**。Artifact 为 nil 表示节点没有这份
// 产物的缓存（中枢派了一个它没部署过的能力 / 缓存被清掉了）。
type job struct {
	TaskID      string
	Capability  string
	Version     int
	Params      map[string]any
	Artifact    *artifact.Artifact // 本次要跑的产物（来自 cache）
	Digest      string             // 产物的内容 digest → ANOTIFY_DIGEST（排障：跑的到底是哪一版）
	TraceID     string             // assign.trace_id → ANOTIFY_TRACE_ID（旧协议没有这个字段，v2 补上）
	MaxDuration int                // assign.max_duration_sec：>=1 时作为兜底超时
	WorkRoot    string             // <data-dir>/work：每任务在其下建独立目录
	// 注意这里**没有** WorkDir 字段：cwd 的唯一来源是产物里的 spec.run.workDir
	// （见 resolveChdir）。中枢下发的 work_dir 与节点侧的 workDirs 白名单都已作废——
	// 两份事实必然分叉，而分叉的症状是「控制台改了却没生效」。
	// TimeoutOverride 是秒级的超时覆盖（只有 standalone 的 --timeout 会用）。
	// 人站在终端前跑一次时，「我想先给它 30 秒试试」应该压过产物里写死的 900。
	TimeoutOverride int
	// Log 是执行期的日志口（节点模式 = agent 的 slog，standalone = stderr 上那个）。
	// 只有一个用途：把「能力里声明的工作目录」与「实际展开/解链后的目录」各写一行，
	// 排障时能看出 `~/x` 到底落在了哪。nil = 不记（单测里的 job 大多不装日志）。
	Log *slog.Logger

	// CaptureFull 让本次执行额外采集一份**完整**输出（→ outcome.ArchiveStdout/ArchiveStderr）。
	//
	// 只有中枢派单路径（client.startTask）为 true：那份输出落在**跑它的这台机器**上
	// （<data-dir>/runs/，见 runstore.go），不回传中枢——网络那条路仍然只走 Stdout/Stderr
	// 的 32 KiB 尾部窗口。standalone（runLocal）不采集：人就在终端前，输出已经直接流到
	// 他屏幕上了，再存一份没有读者。
	CaptureFull bool

	// Ports 是本次任务的产出端口（封闭集合）。来源与 emit socket 白名单**同一处**：
	// assign 的 ports，缺省退到产物的 spec.outputs。
	Ports []string
	// Expects 是这些端口的下游契约（assign 帧的推导量，原样带过来，**不重算**）。
	Expects map[string]ws.PortExpect

	// Emit 是本次任务的本地 emit socket 服务（emit-protocol.md §1）。
	//
	// nil = 这条路径没有中枢（standalone），于是 **不写** ANOTIFY_EMIT_SOCK /
	// ANOTIFY_TASK_TOKEN：能力进程看到「没有 socket」就走「本地模式无中枢，emit
	// 不可用」的分支，而不是拿到一个空路径去 dial。
	Emit *emitServer
}

// outcome 是一次执行的结果。
type outcome struct {
	Status string // statusSucceeded | statusFailed
	// 这里**没有** Out（出口名）：P1.5 起 out 退役，路由只看 emit 的 port。
	Summary   string   // 一行（= Stdout 的第一行，见 runJob）
	Artifacts []string // 恒非 nil（空返回 []）
	Err       string   // failed 时非空
	// Stdout / Stderr 是**分别**捕获的两个流（不再合并）：现场那个
	// `exit status 2` 的真正原因只出现在 stderr，合并捕获之后就没人分得清
	// 「脚本说的」和「工具报的」。两者都是 UTF-8 安全、尾部截断、带截断标记的。
	Stdout string
	Stderr string
	// Truncated 表示任一流被截断过（契约 §1：截断要明说，别让人误以为看全了）。
	Truncated bool
	// Bytes 是两个流的**真实**字节数之和（截断前）——报给中枢的就是它。
	Bytes int64
	// WorkDir 是本次任务的独立工作目录（产物文件展开在这里，也是 ANOTIFY_TASKDIR；
	// 失败时保留，日志里指路）。注意它**不是** cwd：能力声明了 run.workDir 时，
	// cwd（= $ANOTIFY_WORKDIR）是那个目录，见 Cwd。
	WorkDir string
	// Cwd 是进程真正的工作目录（= $ANOTIFY_WORKDIR）。没声明 run.workDir 时等于
	// WorkDir；声明了就可能是另一台机器上的家目录（`~/x` 展开后的真实路径）。
	Cwd string

	// ArchiveStdout / ArchiveStderr 是**完整**输出（单流上限 8 MiB，超限截尾 + 标记行）。
	// 只用于节点本地落盘（runs/），**不**上传：网络那条路仍然走 Stdout/Stderr 的 32 KiB 窗口。
	ArchiveStdout string
	ArchiveStderr string
	// ExitCode 是子进程退出码（0 = 正常；-1 = 拿不到：起不来/被信号杀/超时）。
	ExitCode int

	// 下面四个是落盘用的**账目**（meta.json 的 stdoutBytes/stderrBytes/truncated）。
	// 不能从 ArchiveStdout/ArchiveStderr 反推：截断之后字符串里多了标记行，长度已经不是
	// 真实字节数，而「截了多少」只能由捕获器自己给出（见 runLog.Truncated/Total）。
	ArchiveStdoutBytes     int64
	ArchiveStderrBytes     int64
	ArchiveStdoutTruncated bool
	ArchiveStderrTruncated bool
}

const (
	// maxLogStreamBytes 是**每个流各自**的捕获上限（合计 64 KiB）：任务输出只用于
	// agent 自己的调试日志与 summary（日志不再上传，中枢要看时会发 logs_request 来取
	// 档案里的完整输出），不能让一个话痨命令把 agent 的内存吃光。保留的是**尾部**——
	// 错误几乎总在最后。
	maxLogStreamBytes = 32 << 10
	// archiveStreamMaxBytes 是**落盘**（runs/）时的单流上限：8 MiB。
	//
	// 与 maxLogStreamBytes（32 KiB 窗口）是两个量级、两个用途：那个是「内存里看一眼」，
	// 这个是「盘上留全，中枢取日志时就从这里读」。数值的唯一来源是 runstore.go 的
	// runStreamMaxBytes（档案的规格由档案库定义，执行层只是它的使用者）——在这里
	// 再写一个 8 MiB，两处迟早会分叉成「盘上留 8 MiB、执行层却只采 32 KiB」这种看不见的错。
	archiveStreamMaxBytes = runStreamMaxBytes
	// summaryHeadBytes 是头部旁路窗口的大小：主窗口只留尾部，而 summary 的语义是
	// **stdout 的第一行**（result 帧里那句，还进 tasks.summary / error）。
	// 4 KiB 足够容下「前面一串空行 + 第一行」，而 oneLine 只取前 200 字节。
	summaryHeadBytes = 4 << 10
	// summaryMaxBytes 是 summary 的字节上限（一行、UTF-8 安全截断）。
	summaryMaxBytes = 200
	// maxTaskIDLen 是任务 ID 的长度上限：它要当目录名用。
	maxTaskIDLen = 128
)

// waitDelay 是 cmd.WaitDelay 的取值：Wait 收尾的**兜底**，不是超时本体（超时本体
// 仍由 cmd.Cancel 杀进程组负责）。
//
// 它是什么：Go 1.20+ 的 exec.Cmd 字段。Wait 除了等子进程退出，还要等 stdout/stderr
// 的复制 goroutine 读到**管道 EOF**；如果命令把自己**脱离进程组**（`setsid sleep 3 &`）
// 而那个孤儿继承了管道，杀进程组杀不到它，Wait 就被它拖住（契约 §11.2 F2 实测：
// timeoutSec=1 的命令 3.0s 才返回）。设了 WaitDelay 后，只要「ctx 已 done / 子进程
// 已被 kill 过」而 I/O 管道仍未关闭，再过 waitDelay 就强行关管道、让 Wait 返回
// （返回 exec.ErrWaitDelay；下面的判定顺序仍然先看 ctx.Err()，所以 Err 还是
// "timed out after ..."）。
//
// 为什么是 1s 而不是契约建议的 3s：WaitDelay 的计时器在 **ctx done 或 Wait 观察到
// 子进程已退出**（两者**先到者**）那一刻启动（os/exec 的 watchCtx（exec.go:818）与
// awaitGoroutines（exec.go:985）里那两处 time.NewTimer(c.WaitDelay)），而不是更早，
// 所以最坏耗时为「超时时长 + waitDelay」。3s 兜底意味着
// 1s 超时的任务要 4s 才返回——而 `setsid sleep 3` 的孤儿 3s 就自己退出了，兜底永远
// 轮不到先触发（实测仍是 3.0s，等于没修）。1s 同时远大于「杀组后管道排空」所需的
// 毫秒级时间，不会误伤正常命令的输出。
const waitDelay = time.Second

// execPlan 是「已经准备好、可以开跑」的一次执行。
//
// 把准备（建目录、展开文件、算环境变量、算超时）与执行（起进程、等退出）分开，
// 是为了让 runJob（中枢派单）与 runLocal（standalone）共用**同一套**准备与执行
// 语义：同一份产物在两条路径上必须表现得一模一样，否则「拷到别的机器直接能跑」
// 就是句空话。
type execPlan struct {
	workDir    string        // 本次任务的独立工作目录（产物文件展开在这里，也是 ANOTIFY_TASKDIR）
	cwd        string        // 进程的工作目录；默认同 workDir，能力声明了 run.workDir 时是那个目录（已解链）
	entrypoint string        // 入口文件的绝对路径（直接 exec，靠 shebang 选解释器）
	env        []string      // 完整环境变量（父环境剔除 ANOTIFY_* + 产物 env + 契约变量）
	timeout    time.Duration // 0 = 不设本地超时（交给中枢租约）
}

// runJob 执行一次任务。**不 panic**：所有失败都写进 outcome.Err（client 侧照常
// 回 result 帧）。
//
// 上下文语义：ctx 被取消（cancel 帧 / 退出排空）或超时 → 杀整个进程组，outcome
// 记 failed（Err 分别是 canceled / timed out after ...）。
func runJob(ctx context.Context, j job) outcome {
	// ExitCode 的初值是 -1（「拿不到」）：准备失败、起不来、超时、被信号杀都走这条路，
	// 只有真的看到「进程自己退出了」才会被改成具体码（0 或 exitErr.ExitCode()）。
	res := outcome{Status: statusFailed, Artifacts: []string{}, ExitCode: -1}

	plan, err := prepareExecution(j)
	if err != nil {
		// 准备阶段的失败（没有产物 / 工作目录建不出来 / 文件落不下）都是**这个任务**
		// 的失败，不是 agent 的失败：照常回 result failed。
		res.Err = err.Error()
		return res
	}
	res.WorkDir = plan.workDir
	res.Cwd = plan.cwd

	// 本地超时优先（产物自己声明怎么跑），中枢的 max_duration_sec 只在产物
	// 没写超时时兜底；都没有 → 0（不设本地超时，交给中枢租约）。
	if plan.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, plan.timeout)
		defer cancel()
	}

	stdoutSink := newStreamLog(maxLogStreamBytes)
	stderrSink := newStreamLog(maxLogStreamBytes)
	// CaptureFull（只有中枢派单路径为 true）时，同一个流再挂一只**完整**捕获器：
	// 它只服务本地落盘（runs/），与上报窗口并存、互不影响。
	var stdoutFull, stderrFull *runLog
	if j.CaptureFull {
		stdoutFull = newRunLog(archiveStreamMaxBytes)
		stderrFull = newRunLog(archiveStreamMaxBytes)
	}
	cmd := newArtifactCmd(ctx, plan)
	// Stdin 不接（nil → /dev/null）：命令不可能因为等 stdin 而挂死。
	//
	// 两个流**各一条管道**（不再共用一个 writer）：合并之后 stderr 就再也找不回来了，
	// 而「为什么炸了」通常只在 stderr 里（契约 §0 的现场）。
	//
	// cmd.Stdout 只能设一次，所以「窗口 + 完整捕获」是 MultiWriter 而不是两次赋值；
	// 两个 writer 都不返回错误、都报告全部写入（streamLog/runLog 同一条纪律），
	// 不会把子进程写死。
	cmd.Stdout = multiSink(stdoutSink, stdoutFull)
	cmd.Stderr = multiSink(stderrSink, stderrFull)

	if err := startCmd(cmd); err != nil {
		// 明确区分「起不来」（入口文件不可执行、缺解释器…）与「跑完失败」。
		res.Err = "start entrypoint: " + err.Error()
		captureLogs(&res, stdoutSink, stderrSink)
		captureArchive(&res, stdoutFull, stderrFull)
		res.Summary = stdoutSink.FirstLine(summaryMaxBytes)
		return res
	}

	err = cmd.Wait()
	captureLogs(&res, stdoutSink, stderrSink)
	// 完整捕获在 Wait **之后**才取（String 是快照）：此时子进程已退出、复制 goroutine
	// 已收尾，读到的就是全部输出。
	captureArchive(&res, stdoutFull, stderrFull)
	// summary 的口径**不变**：仍然是 stdout 的第一行。刻意不 fallback 到 stderr——
	// 换源会让「同一个失败」在改版前后给出两种摘要，而摘要还进 error 字段。
	//
	// 用的是 stdoutSink.FirstLine（头部旁路窗口）而不是 oneLine(res.Stdout)：
	// res.Stdout 只留了**最后** 32 KiB，第一行早已被挤出去。
	res.Summary = stdoutSink.FirstLine(summaryMaxBytes)

	switch {
	case err == nil && ctx.Err() == nil:
		// 成功判定要同时看 exit code 与 ctx：命令刚好在超时/取消那一刻 exit 0 时，
		// 它不是「跑完了」，而是「被我们掐断的」，不能当成功。
		res.Status = statusSucceeded
		res.ExitCode = 0
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Err = "timed out after " + plan.timeout.String()
	case errors.Is(ctx.Err(), context.Canceled):
		res.Err = "canceled"
		if err != nil {
			res.Err += ": " + err.Error()
		}
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// "exit status 7" / "signal: killed"
			res.Err = exitErr.Error()
			// 被信号杀掉时 ExitCode() 自己就是 -1（与「拿不到退出码」同义），
			// 那就保持初值；拿到具体码就照实记（档案里「为什么失败」的一半答案）。
			if code := exitErr.ExitCode(); code >= 0 {
				res.ExitCode = code
			}
		} else {
			res.Err = "run entrypoint: " + err.Error()
		}
		if res.Summary != "" {
			// 只带首行：错误要能一眼看出是哪儿炸的，但别把整段日志塞进 error 字段。
			res.Err += ": " + res.Summary
		}
	}

	// 这里曾经读一次 `.anotify-out` 并合成 res.Out —— P1.5 起没有这一步：
	// 产出只有 emit 通道，成败只有进程事实（见文件头）。

	if res.Status == statusSucceeded {
		if res.Summary == "" {
			// 命令没有输出（echo -n、true …）：summary 不能是空串，
			// 否则服务端/前端看起来像「什么都没说」。
			res.Summary = "exit 0"
		}
		// 成功 → 删掉工作目录（契约 §5 的清理规则）。失败则保留，方便事后翻现场。
		// 删不掉不算任务失败（任务确实成功了），路径仍在 res.WorkDir 里可查。
		_ = os.RemoveAll(plan.workDir)
	}
	return res
}

// runLocal 是 standalone 模式（`anotify-agent run ./x.yaml`）的执行器。
//
// 与 runJob 的差异**只有**输出去处与回报方式：stdout/stderr 直连调用方给的 writer
// （人就在终端前，要看实时输出），退出码原样透传给进程（脚本怎么退，agent 就怎么退）。
// 准备（工作目录 / 展开文件 / 环境变量 / 超时）与执行（进程组 / WaitDelay）完全共用，
// 否则「拷到别的机器直接能跑」和「中枢派下来能跑」会是两种行为。
//
// 它**不**采集执行档案（job.CaptureFull 在这条路上无意义）：输出已经直连调用方的
// writer，而「这次执行的输入与完整输出」的读者是中枢侧的事后回看（见 runstore.go）。
//
// 返回 (退出码, error)：error 只表示「没能跑起来」这类 agent 侧故障（退出码由
// 调用方按失败处理）；子进程自己的非 0 退出码是**结果**，不是 error。
func runLocal(ctx context.Context, j job, stdout, stderr io.Writer) (int, error) {
	plan, err := prepareExecution(j)
	if err != nil {
		return exitFailure, err
	}
	if plan.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, plan.timeout)
		defer cancel()
	}

	cmd := newArtifactCmd(ctx, plan)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := startCmd(cmd); err != nil {
		return exitFailure, fmt.Errorf("start %s: %w", plan.entrypoint, err)
	}
	err = cmd.Wait()

	switch {
	case err == nil && ctx.Err() == nil:
		// standalone 是一次性执行：成功后不留工作目录。
		_ = os.RemoveAll(plan.workDir)
		return exitOK, nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		keepWorkDir(stderr, plan.workDir)
		return exitFailure, fmt.Errorf("timed out after %s", plan.timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		keepWorkDir(stderr, plan.workDir)
		return exitFailure, errors.New("canceled")
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		keepWorkDir(stderr, plan.workDir)
		code := exitErr.ExitCode()
		if code < 0 {
			// 被信号杀掉（-1 = 没有退出码）：按失败算，不假装成某个具体错误码。
			code = exitFailure
		}
		return code, nil
	}
	keepWorkDir(stderr, plan.workDir)
	return exitFailure, fmt.Errorf("run %s: %w", plan.entrypoint, err)
}

// keepWorkDir 在失败时告诉用户工作目录还在哪儿（standalone 的唯一现场）。
func keepWorkDir(stderr io.Writer, dir string) {
	if stderr == nil || dir == "" {
		return
	}
	fmt.Fprintf(stderr, "anotify-agent: 工作目录已保留：%s\n", dir)
}

// startCmd 在 forkLock 下启动子进程。
//
// 拦截 cmd.Start 的唯一理由见 cache.go 的 forkLock：另一个 goroutine 正在写的文件
// 的写 fd 会被 fork 出来的子进程继承，让「刚写好、马上要 exec」的入口文件拿到
// ETXTBSY。Start 返回时子进程已经 exec 完（CLOEXEC 的 fd 已关闭），锁就可以放了。
func startCmd(cmd *exec.Cmd) error {
	forkLock.Lock()
	defer forkLock.Unlock()
	return cmd.Start()
}

// prepareExecution 做一次执行的全部准备（runJob / runLocal 共用）。
func prepareExecution(j job) (execPlan, error) {
	if j.Artifact == nil {
		// 执行层自己也不能 panic：client 侧正常会在派发前挡掉「没有产物」的任务，
		// 但防御性错误文案要与 client 保持一致（排障时两条路径表述必须一样）。
		return execPlan{}, fmt.Errorf("no artifact for %s@%d", j.Capability, j.Version)
	}
	entryRel := j.Artifact.Spec.Run.Entrypoint
	if strings.TrimSpace(entryRel) == "" {
		return execPlan{}, fmt.Errorf("%s@%d: run.entrypoint 为空", j.Capability, j.Version)
	}

	workDir, err := ensureWorkDir(j)
	if err != nil {
		return execPlan{}, err
	}
	entrypoint, err := expandFiles(workDir, j.Artifact)
	if err != nil {
		return execPlan{}, err
	}
	// cwd 可能来自能力定义里的 run.workDir（见 resolveChdir）。
	cwd, err := resolveChdir(j, workDir)
	if err != nil {
		return execPlan{}, err
	}
	// 声明里带 `~` 时，把「写的是什么」和「实际跑在哪」一起记下来：只看展开后的
	// 路径会以为能力里写死了它，只看 `~` 又不知道落在哪。这条日志在进程启动之前，
	// 是排「为什么跑到别的目录」时最先要看的那一行。
	if declared := strings.TrimSpace(j.Artifact.Spec.Run.WorkDir); j.Log != nil && artifact.IsHomeRelative(declared) {
		j.Log.Info(fmt.Sprintf("workdir declared=%s resolved=%s", declared, cwd),
			"event", "agent.task.workdir",
			"task_id", j.TaskID,
			"declared", declared,
			"resolved", cwd,
		)
	}
	env, err := buildEnv(os.Environ(), j, workDir, cwd, selfExecutable())
	if err != nil {
		return execPlan{}, fmt.Errorf("build env: %w", err)
	}
	return execPlan{
		workDir:    workDir,
		cwd:        cwd,
		entrypoint: entrypoint,
		env:        env,
		timeout:    effectiveTimeout(j),
	}, nil
}

// ensureWorkDir 建出本次任务的独立工作目录（<work-root>/<task_id>）。
//
// 同一 task_id 被重派（中枢租约到期后再派）时先清空：上一次 attempt 可能留下
// 半成品文件，混进新一次的读取会让脚本行为变得不可预测。删它不会伤到「还该继续
// 跑的那个任务」——重派时旧的 goroutine 已经被 cancel（见 client.startTask）。
func ensureWorkDir(j job) (string, error) {
	name, err := workDirName(j.TaskID)
	if err != nil {
		return "", err
	}
	root, err := workDirRoot(j)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if _, err := os.Stat(dir); err == nil {
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("清理旧工作目录 %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建工作目录 %s: %w", dir, err)
	}
	return dir, nil
}

// workDirRoot 是工作目录的父目录：正常是 <data-dir>/work；
// 没给（standalone 的兜底 / 单测）时退到系统临时目录。
func workDirRoot(j job) (string, error) {
	root := strings.TrimSpace(j.WorkRoot)
	if root == "" {
		root = filepath.Join(os.TempDir(), "anotify-work")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("创建工作目录根 %s: %w", root, err)
	}
	return root, nil
}

// workDirName 校验任务 ID 能不能直接当目录名用。
//
// task_id 来自中枢（正常是 tsk_xxx），但它是**对端给的字符串**，而这里要拿它拼
// 路径：不校验就等于把路径拼接交给对端（`../..` 会写到 data-dir 外面）。
func workDirName(taskID string) (string, error) {
	id := strings.TrimSpace(taskID)
	if id == "" {
		return "", errors.New("任务 ID 为空，无法建独立工作目录")
	}
	if len(id) > maxTaskIDLen {
		return "", fmt.Errorf("任务 ID 过长（%d > %d）", len(id), maxTaskIDLen)
	}
	if id == "." || id == ".." || strings.ContainsAny(id, `/\`) || strings.ContainsRune(id, 0) {
		return "", fmt.Errorf("非法任务 ID %q", id)
	}
	return id, nil
}

// expandFiles 把产物的全部文件按相对路径展开进工作目录，返回入口文件的绝对路径。
//
// 全部 0755：执行方式是直接 exec 入口文件（靠 shebang），被入口调用的辅助脚本
// 也常常需要可执行位。产物内容本身就是能力作者写的代码——它本来就会以节点的
// 身份跑，给执行位不扩大任何权限面。
//
// 路径安全走 safeJoin（与缓存落盘同一个闸门）：产物在 artifact.Parse 时已经校验过
// 一次，这里是落盘前的第二道，因为工作目录是**可写**的，上游校验不该是唯一防线。
func expandFiles(workDir string, a *artifact.Artifact) (string, error) {
	names := make([]string, 0, len(a.Spec.Run.Files))
	for name := range a.Spec.Run.Files {
		names = append(names, name)
	}
	sort.Strings(names) // 落盘顺序确定，便于排障（内容不受顺序影响）

	for _, rel := range names {
		dst, err := safeJoin(workDir, rel)
		if err != nil {
			return "", fmt.Errorf("文件 %q: %w", rel, err)
		}
		if dir := filepath.Dir(dst); dir != workDir {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", fmt.Errorf("mkdir for %q: %w", rel, err)
			}
		}
		if err := writeFileSync(dst, []byte(a.Spec.Run.Files[rel]), 0o755); err != nil {
			return "", fmt.Errorf("展开文件 %q: %w", rel, err)
		}
	}

	entrypoint, err := safeJoin(workDir, a.Spec.Run.Entrypoint)
	if err != nil {
		return "", fmt.Errorf("entrypoint %q: %w", a.Spec.Run.Entrypoint, err)
	}
	if st, err := os.Stat(entrypoint); err != nil || st.IsDir() {
		return "", fmt.Errorf("入口文件 %q 没有落到工作目录里", a.Spec.Run.Entrypoint)
	}
	return entrypoint, nil
}

// newArtifactCmd 造一条执行产物的命令：**直接 exec 入口文件**，靠 shebang 选解释器。
//
// 这里就是 v1 的 `sh -c <command>` 的替代品。差别不只是「少一层」：
//
//   - 没有 shell，就没有变量插值/命令替换/引号转义，中枢给的数据不可能变成代码；
//   - 入口文件是一个**有内容寻址 digest** 的产物的组成文件，跑的是什么可审计；
//   - cwd 是本次任务的独立目录，脚本里所有相对路径都落在它里面。
//
// 进程组的处理与 v1 完全一致（那段是踩过坑的，别改）：
//   - Setpgid：脚本派生的孙进程都在同一个组里，才能一次性杀干净；
//   - Cancel 覆盖默认的「只 SIGKILL 直接子进程」：sh 死了但 `sleep` 之类还活着时，
//     它们仍占着输出管道，Wait 会一直等下去；
//   - WaitDelay 兜住「脱离进程组却还占着管道」的孤儿（见 waitDelay 的注释）。
func newArtifactCmd(ctx context.Context, plan execPlan) *exec.Cmd {
	cmd := exec.CommandContext(ctx, plan.entrypoint)
	cmd.Dir = plan.cwd
	cmd.Env = plan.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel 只在 Start 成功后才会被调用，所以 cmd.Process 一定非 nil；
	// 组里已经没有进程时内核返回 ESRCH，那是正常情况，不当错误。
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid) }
	cmd.WaitDelay = waitDelay
	return cmd
}

// selfExecutable 返回 agent 自己的可执行文件绝对路径（→ ANOTIFY_AGENT_BIN）。
//
// 取不到就返回空串：调用方按「不注入」处理（脚本回退到 PATH 上的 anotify-agent），
// 绝不拿一个空路径去充数（见 buildEnv 里那段注释）。
func selfExecutable() string {
	exe, err := executablePath()
	if err != nil {
		return ""
	}
	return exe
}

// executablePath 是 selfExecutable 的接缝：测试里会把它换成**真编出来的** agent
// 二进制，这样「示例能力调 $ANOTIFY_AGENT_BIN emit」才能在单测里真的跑通。
// 默认实现是 os.Executable（生产路径）。
var executablePath = os.Executable

// buildEnv 组织子进程环境：父环境（剔除 ANOTIFY_* 残留）+ 产物的 run.env +
// 契约变量（优先级最高），最后**按变量名排序**返回，便于测试直接比对。
//
// 它**不再是「几乎是纯函数」**：按契约 §5，本函数还要把出口契约写成
// $ANOTIFY_TASKDIR/emit.json（0600）——那是它**唯一**的副作用，而且必须在返回
// 环境变量之前完成（写不进去就让这次执行失败：脚本看到「有变量、没文件」和
// 「有文件、没变量」一样是坏状态，只是坏得更隐蔽）。除 workDir 之外它仍然不读
// 外部状态：base 由调用方给，不依赖时间/随机；agentBin 也由调用方取好传进来
// （这样单测能钉住具体值，不必去猜测试二进制的路径）。
func buildEnv(base []string, j job, workDir, cwd, agentBin string) ([]string, error) {
	env := make(map[string]string, len(base)+8)

	// 1) 父进程环境。先剔除所有 ANOTIFY_*：agent 自己可能被别的东西包着跑，
	//    残留的 ANOTIFY_TASK_ID 会让子进程看到「上一个任务」的身份。
	for _, kv := range base {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue // 形状坏的条目直接丢（"=x" 塞进子进程环境没意义）
		}
		if hasAnotifyPrefix(name) {
			continue
		}
		env[name] = value
	}

	// 2) 产物自带的 run.env（v1 这里是 node.yaml 的 run.env；v2 执行方式归产物）。
	//    ANOTIFY_* 一律忽略——这些是契约变量，由下面第 3 步独占（run.env 不得覆盖）。
	if j.Artifact != nil {
		for name, value := range j.Artifact.Spec.Run.Env {
			if name == "" || hasAnotifyPrefix(name) {
				continue
			}
			env[name] = value
		}
	}

	// 2.5) PATH 打头：**本 agent 二进制所在目录**必须排在最前面。
	//
	// 现场故障：节点上 /usr/local/bin 里躺着一份旧的 anotify-agent，它抢在 PATH
	// 前面，于是脚本里的 `anotify-agent emit --to …` 被旧二进制当成未知参数
	// （exit 2，stderr 只有 "flag provided but not defined: -to"）。
	//
	// 顺序是硬要求：必须在第 2 步**之后**。产物 run.env 里也可能写 PATH，但那压不过
	// 这一条——「我这一版 agent」永远比「PATH 上碰巧先出现的那份」更可信；反过来
	// 就等于把「跑起来的是谁」交给产物的环境变量去决定。
	//
	// agentBin 为空（os.Executable 失败）时**不动** PATH：宁可缺这条，也不要塞进一个
	// "." 或空段（空段在 PATH 里等义于「当前目录」，是更严重的注入面）。
	if dir := filepath.Dir(agentBin); agentBin != "" && dir != "" && dir != "." {
		if cur := env["PATH"]; cur != "" {
			env["PATH"] = dir + string(os.PathListSeparator) + cur
		} else {
			env["PATH"] = dir
		}
	}

	// 3) 契约变量（契约 §6 的表格，全部保留 + 新增三个）。
	paramsJSON := []byte("{}")
	if len(j.Params) > 0 {
		b, err := json.Marshal(j.Params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		paramsJSON = b
	}
	// 能力名与版本以**产物**为准（assign 可能只给了 digest）：脚本要知道自己是谁，
	// 而这个「谁」必须是真正在跑的那一份。同一个 "name@version" 也用于错误文案
	// （见 resolveChdir），两处必须同源（capabilityRef）。
	env["ANOTIFY_TASK_ID"] = j.TaskID
	env["ANOTIFY_CAPABILITY"] = capabilityRef(j)
	env["ANOTIFY_PARAMS"] = string(paramsJSON)
	// 三个新增变量**始终存在**（可能是空串）：脚本可以无条件引用，
	// 用 `${ANOTIFY_TRACE_ID:-}` 也照样成立；「键不存在」会让 set -u 的脚本直接炸。
	//
	// ANOTIFY_WORKDIR 历史上就**等于**进程的 cwd（脚本里普遍写着 cd "$ANOTIFY_WORKDIR"），
	// 能力声明了 run.workDir 之后这条不变式必须继续成立，否则那些脚本会跑回任务目录。
	// 产物文件所在的任务目录另给一个 ANOTIFY_TASKDIR（只增不改）。
	env["ANOTIFY_WORKDIR"] = cwd
	env[envTaskDir] = workDir
	env["ANOTIFY_TRACE_ID"] = j.TraceID
	env["ANOTIFY_DIGEST"] = j.Digest
	// 出口回报文件（ANOTIFY_OUT_FILE）**不再注入**：out 已退役，产出只走 emit。
	// 注入一个空路径会让老脚本带着空字符串去写（写到哪里全看 cwd），比让它
	// 在 `set -u` 下当场炸更糟。

	// ANOTIFY_AGENT_BIN：本 agent 二进制的绝对路径。
	//
	// 为什么要有它：能力进程调 `anotify-agent emit` 是**唯一**的产出通道
	// （emit-protocol §2），而节点上 agent 不一定在 PATH 里（可以是从任意
	// 目录起的静态二进制，也可能是容器里的 /app/agent）。靠 PATH 猜的后果是
	// 「emit 报 command not found → 脚本 exit 127 → 链断掉」，看起来像能力写错。
	// 脚本的正确写法是 `"${ANOTIFY_AGENT_BIN:-anotify-agent}" emit …`：
	// 取不到时回退到 PATH（standalone / 手动跑产物 的场景）。
	//
	// 取不到路径（极罕见，os.Executable 失败）就不注入：宁可缺键也不给空串——
	// 空串会让 `"$ANOTIFY_AGENT_BIN" emit` 变成执行 `emit …`，错误完全对不上号。
	if agentBin != "" {
		env[envAgentBin] = agentBin
	}

	// emit 通道（emit-protocol.md §1）：socket 路径 + 一次性任务令牌。
	// 只在「这次任务真的有 socket」时注入（见 job.Emit 的注释）。
	if j.Emit != nil {
		env[envEmitSock] = j.Emit.Path()
		env[envTaskToken] = j.Emit.Token()
	}

	// 出口契约（契约 §5）：**同一份字节**既进环境变量，也落盘到 emit.json。
	//
	// 为什么两处都要：环境变量是脚本/模型最容易读到的地方（一行就够），但长度
	// 有上限、还会被整棵进程树继承；文件让长得多的契约不必截断。两者必须逐字节
	// 相同——各算一次就会出现「emit 时按 A 校验、模型看到的是 B」这种最难查的分叉。
	// 没有端口时是 []（不是空串、不是 null）：变量与文件都**始终存在**。
	emitJSON, err := emitContractJSON(j)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(workDir) == "" {
		// 没有任务目录就等于没有「契约文件该落在哪」：宁可让任务失败，也不能
		// 拿空路径拼出一个相对路径写进 agent 自己的 cwd。
		return nil, fmt.Errorf("写出口契约 %s: 任务目录为空", emitContractFile)
	}
	env[envEmitPorts] = string(emitJSON)
	emitPath := filepath.Join(workDir, emitContractFile)
	if err := writeFileSync(emitPath, emitJSON, 0o600); err != nil {
		// **不得静默跳过**：报错要带路径，运维才知道去哪儿看（见契约 §5）。
		return nil, fmt.Errorf("写出口契约 %s: %w", emitPath, err)
	}

	// 每个顶层参数一份 ANOTIFY_PARAM_<NAME>。按 key 排序后再生成：归一化之后
	// 可能撞名（my-param / my_param → MY_PARAM），排序保证同一份 params 每次
	// 得到同一个结果（字典序靠后者的值赢）。
	keys := make([]string, 0, len(j.Params))
	for key := range j.Params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, err := paramEnvValue(j.Params[key])
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", key, err)
		}
		env["ANOTIFY_PARAM_"+paramEnvName(key)] = value
	}

	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+env[name])
	}
	return out, nil
}

// hasAnotifyPrefix 判断环境变量名是否在 agent 自己的命名空间里（大小写敏感：
// POSIX 环境变量名本身大小写敏感，这里只认全大写的契约前缀）。
func hasAnotifyPrefix(name string) bool {
	return strings.HasPrefix(name, "ANOTIFY_")
}

// emitPortContract 是 ANOTIFY_EMIT_PORTS / emit.json 里的一项（契约 §5 的逐字形状）：
//
//	[{"port":"fix","schema":{…},"targets":[{"capability":"b","version":2}]}]
//
// Schema 用 json.RawMessage 且**不加 omitempty**：没有下游时它必须是显式 null——
// 「这个端口没有约束」与「忘了下发」是两件事，缺键会把前者说成后者。
// Targets 同理：没有候选时是 []，不是 null。
type emitPortContract struct {
	Port    string               `json:"port"`
	Schema  json.RawMessage      `json:"schema"`
	Targets []emitTargetContract `json:"targets"`
}

// emitTargetContract 是一个候选下游。Version **总是输出**（不加 omitempty）：
// 结构稳定比省几个字节重要，消费方可以无条件读它。
//
// Priority 同样总是输出（0 也输出）：脚本/LLM 要据此理解「选谁」，缺键会被读成
// 「没有优先级概念」。同级候选的先后由中枢在派发时轮盘决定，这里的顺序只是**确定性**。
type emitTargetContract struct {
	Capability string `json:"capability"`
	Version    int    `json:"version"`
	Priority   int    `json:"priority"`
}

// emitContractJSON 把本次任务的出口契约序列化成**唯一**那份 JSON（契约 §5）。
//
// 输入只有 j.Ports 与 j.Expects，两者都是 assign 帧原样带过来的；这里**不第二次
// 推导**下游（推导归中枢：节点再算一遍就会与中枢的判据悄悄分叉，而且两边都看不出
// 自己错在哪）。由此得到三条确定的事：
//
//   - 顶层按 port 名升序（顺序确定，日志/测试可以直接逐字节比）；
//   - j.Expects 里不在 j.Ports 的端口**不输出**（端口是封闭集合，I2）；
//   - targets 按 (priority, capability, version) 升序（确定性优先；选中谁由中枢轮盘定）。
func emitContractJSON(j job) ([]byte, error) {
	ports := normalizeEmitPorts(j.Ports)
	contracts := make([]emitPortContract, 0, len(ports))
	for _, port := range ports {
		expect := j.Expects[port]
		// 非 nil 的空切片：json.Marshal 才会输出 []（nil 切片会变成 null）。
		targets := make([]emitTargetContract, 0, len(expect.Targets))
		for _, tgt := range expect.Targets {
			targets = append(targets, emitTargetContract{
				Capability: tgt.Capability,
				Version:    tgt.Version,
				Priority:   tgt.Priority,
			})
		}
		// 排序口径 = (priority, capability, version)：优先级是「谁先被选中」的语义，
		// 名称只是同级里的确定性次序（选中谁由中枢轮盘决定，不是这个顺序）。
		sort.SliceStable(targets, func(i, k int) bool {
			if targets[i].Priority != targets[k].Priority {
				return targets[i].Priority < targets[k].Priority
			}
			if targets[i].Capability != targets[k].Capability {
				return targets[i].Capability < targets[k].Capability
			}
			return targets[i].Version < targets[k].Version
		})
		contracts = append(contracts, emitPortContract{Port: port, Schema: expect.Schema, Targets: targets})
	}
	b, err := json.Marshal(contracts)
	if err != nil {
		return nil, fmt.Errorf("marshal emit ports: %w", err)
	}
	return b, nil
}

// normalizeEmitPorts 把端口白名单归一成「去空白、去重、按名字升序」的确定集合。
//
// 专门服务序列化：assign 里的 ports 可能带空白/重复（对端给的字符串），而输出
// 顺序必须与输入顺序无关（否则同一份快照在不同进程里生成两份不同的 JSON）。
func normalizeEmitPorts(ports []string) []string {
	seen := make(map[string]struct{}, len(ports))
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		name := strings.TrimSpace(port)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// paramEnvValue 把参数值渲染成环境变量里的文本（契约 §3.3 的表格）：
//
//	null            → ""（空串：变量存在但没值）
//	string          → 原值（不加引号）
//	number / bool   → JSON 字面量（42 / 1.5 / true）
//	object / array  → 紧凑 JSON（{"a":1} / [1,2]）
//
// 除 null 和 string 之外一律走 json.Marshal：它输出的就是紧凑 JSON，而对
// number/bool 恰好就是字面量，不必为每种类型分叉。
func paramEnvValue(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal param value: %w", err)
	}
	return string(b), nil
}

// effectiveTimeout 取本次执行的超时：命令行显式覆盖（standalone 的 --timeout）最优先，
// 其次是产物自己的 run.timeoutSec，再次是中枢给的 max_duration_sec；
// 都没有 → 0（不设本地超时，交给中枢租约）。
func effectiveTimeout(j job) time.Duration {
	if j.TimeoutOverride > 0 {
		return time.Duration(j.TimeoutOverride) * time.Second
	}
	if j.Artifact != nil && j.Artifact.Spec.Run.TimeoutSec > 0 {
		return time.Duration(j.Artifact.Spec.Run.TimeoutSec) * time.Second
	}
	if j.MaxDuration > 0 {
		return time.Duration(j.MaxDuration) * time.Second
	}
	return 0
}

// paramEnvName 把参数名归一成环境变量名片段：大写 + 非 [A-Z0-9_] 一律变 '_'。
//
// 例：title → TITLE，my-param → MY_PARAM，a.b/c → A_B_C。
func paramEnvName(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToUpper(key) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			// 按 rune 归一（不是按字节），一个非 ASCII 字符只变成一个 '_'。
			b.WriteByte('_')
		}
	}
	return b.String()
}

// oneLine 取一行摘要：去首尾空白、取**第一个非空行**，再按字节截断到 maxBytes
// 以内（截断点回退到 UTF-8 字符边界，绝不切出半个汉字）。
func oneLine(s string, maxBytes int) string {
	line := ""
	for _, candidate := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(candidate); t != "" {
			line = t
			break
		}
	}
	if maxBytes <= 0 || len(line) <= maxBytes {
		return line
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut]
}

// streamLog 是「只保留最后 limit 字节」的输出捕捉器（滑动窗口）。
//
// 为什么是**尾部**而不是前 limit 字节：错误几乎总是写在最后（编译错误的尾段、
// 栈顶之前的最后一句 stderr），砍掉尾部等于把最有用的部分扔掉。
//
// Write 永远报告「全部写入」——短写会让 exec 把子进程判成写错误（写失败会把一次
// 正常的任务变成 I/O 故障）。真实写入总量记在 total 里：截断提示与上报的 bytes
// 都要用它，不能拿 len(buf) 冒充（那是「留下了多少」，不是「产生了多少」）。
type streamLog struct {
	mu    sync.Mutex
	buf   []byte // 尾部窗口：真正上报的日志（最后 limit 字节）
	head  []byte // 头部窗口：只服务 FirstLine（summary）
	limit int
	total int64
}

func newStreamLog(limit int) *streamLog { return &streamLog{limit: limit} }

// Write 实现 io.Writer（每个流一个实例）。
func (s *streamLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total += int64(len(p))
	// 头部窗口（只为了 summary 不被尾部滑动挤掉）：环形之外的那几 KB 代价可忽略。
	if room := summaryHeadBytes - len(s.head); room > 0 {
		if len(p) <= room {
			s.head = append(s.head, p...)
		} else {
			s.head = append(s.head, p[:room]...)
		}
	}
	if len(p) >= s.limit {
		// 这一笔自己就超了窗口：窗口里只可能是它的尾部（前面的字节已被挤出去）。
		s.buf = append(s.buf[:0], p[len(p)-s.limit:]...)
		return len(p), nil
	}
	if over := len(s.buf) + len(p) - s.limit; over > 0 {
		s.buf = s.buf[over:] // 丢掉窗口最前面的 over 字节
	}
	s.buf = append(s.buf, p...)
	return len(p), nil
}

// Total 是这个流**真实**写进来的字节数（截断前）。
func (s *streamLog) Total() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// Truncated 表示窗口外还有内容被丢掉了。
func (s *streamLog) Truncated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total > int64(len(s.buf))
}

// FirstLine 是本流**第一行**（= summary 的唯一来源，契约 §1）。
//
// 不能拿 String() 的结果去算：那是最后 32 KiB，脚本的第一行可能已经被挤出去了。
// 头部窗口最多 4 KiB，再长的一行也只需要前 maxBytes 字节，所以结果与
// 「对完整输出做 oneLine」一致（除非前 4 KiB 全是空白行——那种输出本来就没有摘要）。
func (s *streamLog) FirstLine(maxBytes int) string {
	s.mu.Lock()
	head := s.head
	s.mu.Unlock()
	return oneLine(strings.ToValidUTF8(string(head), "?"), maxBytes)
}

// String 返回捕获到的输出：先按 UTF-8 校验（非法字节 → '?'，否则下游要把它塞进
// JSON 时编不出来），被截断时在**末尾**追加一行说明。
func (s *streamLog) String() string {
	s.mu.Lock()
	buf, total := s.buf, s.total
	s.mu.Unlock()

	out := strings.ToValidUTF8(string(buf), "?")
	if total > int64(len(buf)) {
		// 逐字形状见契约 §1：省略号说明前面还有内容，N 是真实字节数。
		out += fmt.Sprintf("\n…（日志已截断：共 %d 字节，只保留最后 %d 字节）", total, s.limit)
	}
	return out
}

// captureLogs 把两条流的采集结果填进 outcome（字符串、截断标记、真实字节数）。
//
// 起不来（start 失败）与跑完（Wait 之后）两条路径都必须调它：前者往往正是最需要
// stderr 的场景（缺解释器、权限不对），漏了就等于什么都没记。
func captureLogs(res *outcome, stdout, stderr *streamLog) {
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	res.Truncated = stdout.Truncated() || stderr.Truncated()
	res.Bytes = stdout.Total() + stderr.Total()
}

// multiSink 把「上报窗口」与可选的「完整捕获器」拼成 cmd 的输出 writer。
//
// full 为 nil（不落盘：standalone / 任何没开 CaptureFull 的路径）时直接返回窗口本身，
// 不额外套一层 MultiWriter——那条路径的行为与引入档案之前**逐字节相同**。
func multiSink(window io.Writer, full *runLog) io.Writer {
	if full == nil {
		return window
	}
	return io.MultiWriter(window, full)
}

// captureArchive 把两份**完整**输出（与它们的真实字节数/截断标记）填进 outcome。
//
// 只有 job.CaptureFull 为 true 时两个 *runLog 才非 nil；nil 时什么都不做——于是
// standalone 与旧调用方的 outcome 与改动前一致（ArchiveStdout 保持空串）。
func captureArchive(res *outcome, stdout, stderr *runLog) {
	if stdout != nil {
		res.ArchiveStdout = stdout.String()
		res.ArchiveStdoutBytes = stdout.Total()
		res.ArchiveStdoutTruncated = stdout.Truncated()
	}
	if stderr != nil {
		res.ArchiveStderr = stderr.String()
		res.ArchiveStderrBytes = stderr.Total()
		res.ArchiveStderrTruncated = stderr.Truncated()
	}
}

// killProcessGroup 给**整个进程组**发 SIGKILL（负 pid = 进程组）。
//
// 组里已经没有进程时内核返回 ESRCH：那是正常情况（子进程先自己退出，或者上一次
// 已经杀过），当成功处理，免得 Wait 那边报出莫名其妙的 "no such process"。
func killProcessGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// 契约环境变量的常量名。
const (
	// envAgentBin 是交给能力脚本的 agent 二进制路径（脚本用它调 `emit` 子命令）。
	envAgentBin = "ANOTIFY_AGENT_BIN"

	// envEmitPorts 是「出口契约」环境变量（契约 §5）：紧凑 JSON，始终存在。
	// 内容不是路径——**就是** JSON 文本本身；对应的文件是 emitContractFile。
	envEmitPorts = "ANOTIFY_EMIT_PORTS"

	// envTaskDir 是本次任务的工作目录（产物展开在这里，也放 emit.json）。
	envTaskDir = "ANOTIFY_TASKDIR"

	// emitContractFile 是同一份 JSON 落在 $ANOTIFY_TASKDIR 里的文件名（契约 §5）。
	// 权限 0600：它含下游的 schema，只有这个任务的进程需要看。
	emitContractFile = "emit.json"
)

// resolveChdir 决定这次执行的 cwd。**唯一的来源是能力定义里的 run.workDir**。
//
//	run.workDir 为空      → 本次任务目录（与这个能力出现之前逐字节一致）
//	run.workDir 是绝对路径 → 那个目录（解符号链接后用解链后的路径执行）
//	run.workDir 是 `~`/`~/…` → 先展开成 agent 用户的家目录，再同上
//
// `~` 只在**这里**展开（中枢/解析期不展开）：run.workDir 参与 digest，展开后的
// 路径是机器事实，混进 digest 会让同一份产物在不同节点算出不同摘要。后果是同一个
// 能力在不同节点上可能跑在不同目录——**这是有意为之**，`~/proj` 的含义就是
// 「这台机器上跑 agent 那个用户的项目目录」。
//
// 节点侧不再有任何白名单/映射（node.yaml 的 workDirs、ANOTIFY_WORK_DIRS、中枢下发
// 的 work_dir 全部作废）：「这个能力要在哪个目录里跑」是能力定义的一部分，跟入口
// 文件、超时一样。定义它的地方只能有一个，否则就是两份会分叉的事实，而分叉的症状
// 是「控制台改了却没生效」，极难查。
//
// 安全边界也随之换了位置：能力定义由控制台写、由中枢部署，节点只是执行者。所以
// 这里不再做「要不要放行」的判断，只做**可用性**判断——并且把「这台机器上没这个
// 目录」当成**任务失败**而不是降级：
//
//	静默退回任务目录会把「机器上没建这个目录」伪装成「脚本莫名报错」——脚本在一个
//	空目录里找不到工具链配置，报出来的错跟真实原因毫无关系。
//
// 失败文案必须写清三件事：哪个能力（X@Y）、哪个路径、下一步该干什么（在控制台改，
// 或先在机器上建好这个目录）。
func resolveChdir(j job, taskDir string) (string, error) {
	if j.Artifact == nil {
		// 没有产物时 prepareExecution 已经在前边挡掉了；这里防御性维持旧行为。
		return taskDir, nil
	}
	// 目录声明来自产物（能力定义）：节点只执行，不维护第二份目录事实。
	declared := strings.TrimSpace(j.Artifact.Spec.Run.WorkDir)
	if declared == "" {
		return taskDir, nil
	}
	// 第一道：对**字面量**做静态检查（与 artifact.validateWorkDir 同口径）。
	// `..` 必须在展开**之前**挡掉：`~/../etc` 一旦先展开就是「家目录的父目录」了。
	if reason := workDirLiteralProblem(declared, true); reason != "" {
		return "", workDirError(j, declared, reason, nil)
	}
	resolved, err := expandWorkDirOrFail(j, declared)
	if err != nil {
		return "", err
	}
	// 第二道：展开**之后**重跑全套检查。家目录不在我们手里（可能为空、可能是
	// 相对路径、可能带未预料的段），所以不能只信字面量那一遍。
	if reason := workDirLiteralProblem(resolved, false); reason != "" {
		return "", workDirError(j, resolved, reason, nil)
	}
	clean := filepath.Clean(resolved)

	// 解链后再 stat、也用解链后的路径当 cwd：判断的和使用的必须是同一个路径
	// （项目目录常常是软链，不解链会让日志/ANOTIFY_WORKDIR 与实际执行目录不是同一个字符串）。
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return "", workDirError(j, clean, "不存在", err)
		}
		return "", workDirError(j, clean, "无法解析", err)
	}
	st, err := os.Stat(real)
	if err != nil {
		if os.IsNotExist(err) {
			return "", workDirError(j, clean, "不存在", err)
		}
		return "", workDirError(j, clean, "无法访问", err)
	}
	if !st.IsDir() {
		return "", workDirError(j, clean, "不是目录", nil)
	}
	return real, nil
}

// workDirLiteralProblem 是执行前的静态检查，返回非空的中文原因（给 workDirError）。
//
// 它被调用两次：一次对声明里的**字面量**（allowHome=true，`~` / `~/…` 可以过，
// 随后才展开），一次对**展开后**的路径（allowHome=false，必须是绝对路径）。
// 解析期已经查过一遍，这里是第二道——产物也可能来自缓存里的文件（离线 / 手改），
// 防线不该只有上游一道。
//
// 顺序有意义：长度 / NUL 先查（它们是字节层面的事实），`..` 段跟着查，最后才是
// 形态（绝对路径 / 家目录前缀）——`~/../etc` 必须因为它含 `..` 被拒，而不是被当成
// 一个可以先展开的合法声明。
func workDirLiteralProblem(dir string, allowHome bool) string {
	if len(dir) > artifact.MaxWorkDirLen {
		return fmt.Sprintf("有 %d 字节，上限 %d", len(dir), artifact.MaxWorkDirLen)
	}
	if strings.ContainsRune(dir, 0) {
		return "含 NUL 字节"
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == ".." {
			return `含 ".." 路径段`
		}
	}
	if !filepath.IsAbs(dir) && !(allowHome && artifact.IsHomeRelative(dir)) {
		return "不是绝对路径，也不是 ~/… 形式（不展开 $VAR，也不支持 ~user；留空 = 每次任务一个独立目录）"
	}
	return ""
}

// expandWorkDirOrFail 把 `~` / `~/…` 展开成 agent 用户的家目录；不含 `~` 的原样返回。
//
// 家目录用 os.UserHomeDir()，**不**直接读 $HOME：后者在 systemd / 容器里可能被烧成
// 别的值甚至被清空，而 UserHomeDir 在同一种环境下会给出明确的错误。两种失败都意味着
// 「这台机器上无法解释 `~`」，此时任务直接 failed：
//
//	绝不能把空家目录拼成 `/proj` 就跑——那会把「家目录不可知」伪装成一个具体目录，
//	比报错难查一百倍（路径存在时还会真的在里面执行）。
func expandWorkDirOrFail(j job, declared string) (string, error) {
	if !artifact.IsHomeRelative(declared) {
		return declared, nil
	}
	home, err := os.UserHomeDir()
	if err == nil && (strings.TrimSpace(home) == "" || !filepath.IsAbs(home)) {
		// $HOME 被设成空串或相对路径：同样不可用（拼出来的路径会落在 agent 的 cwd 里）。
		err = fmt.Errorf("家目录不可用：%q", home)
	}
	if err != nil {
		return "", workDirError(j, declared,
			"以 ~ 开头，但 agent 进程查不到家目录（容器 / 系统用户常见：HOME 未设置或不是绝对路径）", err)
	}
	return artifact.ExpandWorkDir(declared, home), nil
}

// workDirError 统一「能力声明的工作目录不可用」的错误文案。
//
// 三个要素缺一不可，否则运维看到的就是一句无从下手的错：
//   - 能力 X@Y + 路径：一眼看出是谁、哪条路径（这不是脚本的锅）
//   - 原因（不存在 / 不是目录 / 无法解析 / 不是绝对路径）
//   - 这是**能力定义**里的 run.workDir：指到用户真正能改的那个地方（控制台），
//     而不是让他在 node.yaml 里翻半天（那里已经没有目录配置了）
func workDirError(j job, path, reason string, cause error) error {
	msg := fmt.Sprintf("能力 %s 声明的工作目录 %s %s"+
		"（这是能力定义里的 run.workDir；在控制台改或先在机器上建好这个目录）",
		capabilityRef(j), path, reason)
	if cause != nil {
		return fmt.Errorf("%s: %w", msg, cause)
	}
	return errors.New(msg)
}

// capabilityOf 是本次执行的**能力名**：以产物为准（assign 可能只给了 digest），
// 与 buildEnv 里 ANOTIFY_CAPABILITY 同源。
func capabilityOf(j job) string {
	if j.Artifact != nil {
		return strings.TrimSpace(j.Artifact.Metadata.Name)
	}
	return strings.TrimSpace(j.Capability)
}

// capabilityRef 是能力身份 "name@version"（= ANOTIFY_CAPABILITY / 错误文案里的 X@Y）。
//
// 取法集中在这里：跑在哪个目录（resolveChdir 的报错）与脚本看到的身份一旦分叉，
// 就会变成「日志说在跑 script.echo，出错的却是 build.repo」这种极贵的谜题。
func capabilityRef(j job) string {
	version := j.Version
	if j.Artifact != nil {
		version = j.Artifact.Metadata.Version
	}
	return fmt.Sprintf("%s@%d", capabilityOf(j), version)
}
