package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
//     （同一个 ./out.json、同一个 git clone 目标）。现在 cwd = <data-dir>/work/<task_id>，
//     并以 ANOTIFY_WORKDIR 暴露给脚本。
//  3. **执行内容来自产物，不来自 node.yaml**。node.yaml 只剩 bootstrap（见 config.go）。
//
// # 出口名（out）的来源：一个文件
//
// v1 的 `run.out` 是 node.yaml 里一个固定字符串，任务成功就回报它。v2 的产物只有
// `spec.outputs[]`（**集合**：edges.from_port 的封闭集合），「这一次跑的是哪一个」
// 是**运行期**才知道的，所以必须由脚本告诉节点。
//
// 约定（CONTRACT-artifact §6.1）：
//   - 节点把 `$ANOTIFY_OUT_FILE`（= <workdir>/.anotify-out）交给脚本
//   - 脚本在**成功时**把出口名写进去（一行，最多 64 字节）
//   - 节点读它、trim、校验 ∈ spec.outputs，然后作为 result.out 回报
//   - 文件不存在 → 不回报（out 键缺席 = 「这次到头了，没选下一跳」）
//   - 写了但不在 spec.outputs 里 → 整次执行判 **failed**（出口是封闭集合，不变量 I2）
//
// 为什么不复用 `sh -c` 时代的 `run.out`：那个是「这个能力成功就等于这个出口」，
// 无法表达「同一个能力按参数走不同分支」，而分支恰恰是图的价值所在。
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
	// TimeoutOverride 是秒级的超时覆盖（只有 standalone 的 --timeout 会用）。
	// 人站在终端前跑一次时，「我想先给它 30 秒试试」应该压过产物里写死的 900。
	TimeoutOverride int
}

// outcome 是一次执行的结果。
type outcome struct {
	Status    string   // statusSucceeded | statusFailed
	Out       *string  // 出口名：v2 恒为 nil（来源是契约空白，见文件头注释）
	Summary   string   // 一行
	Artifacts []string // 恒非 nil（空返回 []）
	Err       string   // failed 时非空
	Output    string   // 合并后的子进程输出（只进日志，不进 result 帧）
	WorkDir   string   // 本次任务的独立工作目录（失败时保留，日志里指路）
}

const (
	// maxCapturedOutput 是 stdout+stderr 合并捕获的上限：任务输出只用于日志与
	// summary，不能让一个话痨命令把 agent 的内存吃光。
	maxCapturedOutput = 64 << 10
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
	workDir    string        // 本次任务的独立工作目录（既是 cwd 也是 ANOTIFY_WORKDIR）
	entrypoint string        // 入口文件的绝对路径（直接 exec，靠 shebang 选解释器）
	env        []string      // 完整环境变量（父环境剔除 ANOTIFY_* + 产物 env + 契约变量）
	timeout    time.Duration // 0 = 不设本地超时（交给中枢租约）
	// outFile 是脚本回报「这次走了哪个出口」的唯一通道（见文件头）。
	outFile string
	// allowedOutputs 是产物声明的出口集合（spec.outputs）。脚本写的出口名必须在里面，
	// 否则整次执行判失败 —— 出口是封闭集合（不变量 I2），不允许节点临时发明一个。
	allowedOutputs []string
}

// runJob 执行一次任务。**不 panic**：所有失败都写进 outcome.Err（client 侧照常
// 回 result 帧）。
//
// 上下文语义：ctx 被取消（cancel 帧 / 退出排空）或超时 → 杀整个进程组，outcome
// 记 failed（Err 分别是 canceled / timed out after ...）。
func runJob(ctx context.Context, j job) outcome {
	res := outcome{Status: statusFailed, Out: nil, Artifacts: []string{}}

	plan, err := prepareExecution(j)
	if err != nil {
		// 准备阶段的失败（没有产物 / 工作目录建不出来 / 文件落不下）都是**这个任务**
		// 的失败，不是 agent 的失败：照常回 result failed。
		res.Err = err.Error()
		return res
	}
	res.WorkDir = plan.workDir

	// 本地超时优先（产物自己声明怎么跑），中枢的 max_duration_sec 只在产物
	// 没写超时时兜底；都没有 → 0（不设本地超时，交给中枢租约）。
	if plan.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, plan.timeout)
		defer cancel()
	}

	out := &cappedBuffer{limit: maxCapturedOutput}
	cmd := newArtifactCmd(ctx, plan)
	// Stdin 不接（nil → /dev/null）：命令不可能因为等 stdin 而挂死。
	cmd.Stdout = out
	cmd.Stderr = out // 同一个 writer：exec 只用一条管道，stdout/stderr 按写序合并

	if err := startCmd(cmd); err != nil {
		// 明确区分「起不来」（入口文件不可执行、缺解释器…）与「跑完失败」。
		res.Err = "start entrypoint: " + err.Error()
		res.Output = out.String()
		res.Summary = oneLine(res.Output, summaryMaxBytes)
		return res
	}

	err = cmd.Wait()
	res.Output = out.String()
	res.Summary = oneLine(res.Output, summaryMaxBytes)

	switch {
	case err == nil && ctx.Err() == nil:
		// 成功判定要同时看 exit code 与 ctx：命令刚好在超时/取消那一刻 exit 0 时，
		// 它不是「跑完了」，而是「被我们掐断的」，不能当成功。
		res.Status = statusSucceeded
		// 出口名来自脚本写的那个文件（见文件头）。读失败/没写 = 不回报（分支终止）；
		// 写了但不在 spec.outputs 里 = 整次执行失败（封闭集合，I2）。
		out, outErr := readOutcomeOut(plan)
		switch {
		case outErr != nil:
			res.Status = statusFailed
			res.Err = outErr.Error()
		case out != nil:
			res.Out = out
		}
		if res.Status == statusSucceeded && res.Summary == "" {
			// 命令没有输出（echo -n、true …）：summary 不能是空串，
			// 否则服务端/前端看起来像「没回报」。
			res.Summary = "exit 0"
		}
		// 成功 → 删掉工作目录（契约 §5 的清理规则）。失败则保留，方便事后翻现场。
		// 删不掉不算任务失败（任务确实成功了），路径仍在 res.WorkDir 里可查。
		_ = os.RemoveAll(plan.workDir)
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
		} else {
			res.Err = "run entrypoint: " + err.Error()
		}
		if res.Summary != "" {
			// 只带首行：错误要能一眼看出是哪儿炸的，但别把整段日志塞进 error 字段。
			res.Err += ": " + res.Summary
		}
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
	env, err := buildEnv(os.Environ(), j, workDir)
	if err != nil {
		return execPlan{}, fmt.Errorf("build env: %w", err)
	}
	return execPlan{
		workDir:    workDir,
		entrypoint: entrypoint,
		env:        env,
		timeout:    effectiveTimeout(j),
		outFile:    filepath.Join(workDir, outFileName),
		// 出口集合来自产物：它是 edges.from_port 的封闭集合（不变量 I2）。
		allowedOutputs: append([]string(nil), j.Artifact.Spec.Outputs...),
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
	cmd.Dir = plan.workDir
	cmd.Env = plan.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel 只在 Start 成功后才会被调用，所以 cmd.Process 一定非 nil；
	// 组里已经没有进程时内核返回 ESRCH，那是正常情况，不当错误。
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid) }
	cmd.WaitDelay = waitDelay
	return cmd
}

// buildEnv 组织子进程环境：父环境（剔除 ANOTIFY_* 残留）+ 产物的 run.env +
// 契约变量（优先级最高），最后**按变量名排序**返回，便于测试直接比对。
//
// 纯函数：不读 os.Environ（base 由调用方给），不依赖时间/随机。
func buildEnv(base []string, j job, workDir string) ([]string, error) {
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
	// 而这个「谁」必须是真正在跑的那一份。
	capability, version := j.Capability, j.Version
	if j.Artifact != nil {
		capability, version = j.Artifact.Metadata.Name, j.Artifact.Metadata.Version
	}
	env["ANOTIFY_TASK_ID"] = j.TaskID
	env["ANOTIFY_CAPABILITY"] = fmt.Sprintf("%s@%d", capability, version)
	env["ANOTIFY_PARAMS"] = string(paramsJSON)
	// 三个新增变量**始终存在**（可能是空串）：脚本可以无条件引用，
	// 用 `${ANOTIFY_TRACE_ID:-}` 也照样成立；「键不存在」会让 set -u 的脚本直接炸。
	env["ANOTIFY_WORKDIR"] = workDir
	env["ANOTIFY_TRACE_ID"] = j.TraceID
	env["ANOTIFY_DIGEST"] = j.Digest
	// 出口回报通道（CONTRACT-artifact §6.1）：脚本把出口名写进这个文件。
	env[envOutFile] = filepath.Join(workDir, outFileName)

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

// cappedBuffer 是带上限的输出缓冲：写满之后只记住「被截断了」，不再增长，
// 但 Write 永远报告「全部写入」——短写会让 exec 把子进程判成写错误。
type cappedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

// Write 实现 io.Writer（stdout/stderr 共用一个实例）。
func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

// String 返回捕获到的输出；被截断时在末尾追加一句提示（说明「后面还有」）。
func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.buf)
	if b.truncated {
		s += fmt.Sprintf("\n[agent: output truncated at %d bytes]\n", b.limit)
	}
	return s
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

// 出口回报通道的常量（CONTRACT-artifact §6.1）。
const (
	// envOutFile 是交给脚本的出口文件路径。
	envOutFile = "ANOTIFY_OUT_FILE"
	// outFileName 是这个文件在工作目录里的名字。用点开头 + 固定名字：
	// 用户在脚本里 `ls` 时它不碍事，也不会跟常见的 out.json 撞。
	outFileName = ".anotify-out"
	// outcomeOutMaxBytes 是出口名的长度上限（与 artifact 包的 outputPattern 同量级）。
	outcomeOutMaxBytes = 64
)

// readOutcomeOut 读取脚本写下的出口名。
//
// 三种返回值语义（对应 result 帧 out 域的三态，不变量 I5）：
//
//	nil, nil        → 文件不存在/为空 = 这次不选下一跳（帧里 out 键缺席）
//	&name, nil      → 回报这个出口
//	nil, err        → 写了但不合法 → 整次执行判失败（不静默降级成「不回报」，
//	                  否则一个拼错的出口名会表现为「链路莫名断掉」）
func readOutcomeOut(plan execPlan) (*string, error) {
	if plan.outFile == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(plan.outFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 没写 = 不回报
		}
		return nil, fmt.Errorf("read %s: %w", outFileName, err)
	}
	name := strings.TrimSpace(string(raw))
	if name == "" {
		return nil, nil
	}
	if len(name) > outcomeOutMaxBytes {
		return nil, fmt.Errorf("out %q is longer than %d bytes", name, outcomeOutMaxBytes)
	}
	// 出口是**封闭集合**（不变量 I2）：必须是产物声明的 outputs 之一。
	for _, allowed := range plan.allowedOutputs {
		if allowed == name {
			return &name, nil
		}
	}
	return nil, fmt.Errorf("out %q is not declared in spec.outputs %v", name, plan.allowedOutputs)
}
