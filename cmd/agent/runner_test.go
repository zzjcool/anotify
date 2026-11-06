package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件覆盖执行层（runner.go）的 v2 语义：
//
//   - **直接 exec 入口文件**（靠 shebang），不再经 sh -c；
//   - 每个任务一个**独立工作目录**（<data-dir>/work/<task_id>，cwd 就是它）；
//   - 契约变量 ANOTIFY_WORKDIR / ANOTIFY_TRACE_ID / ANOTIFY_DIGEST；
//   - 进程组 + WaitDelay + 超时/取消（v1 的成果，全部保留）。
//
// 测试只依赖 bash 与最基本的 coreutils，没有网络、没有 fake 时钟。

// ---------- 夹具 ----------

// bashScript 造一个带 shebang 的入口脚本正文（产物里的 run.sh）。
//
// 用 bash 而不是 sh：契约 §5 明确「直接 exec 入口文件，靠 shebang」，而模板里
// 生成的都是 #!/bin/bash。用 bash 才能证明「shebang 真的生效了」——见
// TestRunJobShebangPicksInterpreter。
func bashScript(body string) string {
	return "#!/bin/bash\n" + body
}

// newRunnerJob 造一个「跑一段 bash」的最小 job：产物只有一个 run.sh 入口。
//
// WorkRoot 用 t.TempDir()：工作目录是「每任务独立」的，测试也要互相独立。
func newRunnerJob(t *testing.T, body string) job {
	t.Helper()
	art := testScriptArtifact("demo.prep", 1, bashScript(body))
	return job{
		TaskID:     "tsk_test",
		Capability: "demo.prep",
		Version:    1,
		Artifact:   art,
		Digest:     mustDigest(t, art),
		WorkRoot:   filepath.Join(t.TempDir(), "work"),
	}
}

// workDirOf 返回这个 job 的工作目录（与 runner 拼路径的规则一致）。
func workDirOf(j job) string {
	root := j.WorkRoot
	if strings.TrimSpace(root) == "" {
		root = filepath.Join(os.TempDir(), "anotify-work")
	}
	return filepath.Join(root, j.TaskID)
}

// ---------- 环境变量 ----------

// TestRunJobParamEnv：params → ANOTIFY_PARAM_*（只走环境变量，零插值）。
//
// 覆盖：嵌套 object 的紧凑 JSON、名字归一（my-param → MY_PARAM）、
// number/bool 字面量、null → 空串（但仍存在）。
func TestRunJobParamEnv(t *testing.T) {
	j := newRunnerJob(t, `printf '%s|%s|%s|%s|%s|%s\n' `+
		`"$ANOTIFY_PARAM_TITLE" "$ANOTIFY_PARAM_MY_PARAM" "$ANOTIFY_PARAM_NESTED" `+
		`"$ANOTIFY_PARAM_COUNT" "$ANOTIFY_PARAM_FLAG" "${ANOTIFY_PARAM_NOTHING+set}"`)
	j.Params = map[string]any{
		"title":    "hello world",
		"my-param": "dash-value",
		"nested":   map[string]any{"a": 1, "b": []any{"x"}},
		"count":    float64(3),
		"flag":     true,
		"nothing":  nil,
	}

	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q stdout=%q stderr=%q)", res.Status, statusSucceeded, res.Err, res.Stdout, res.Stderr)
	}
	// "set" 来自 ${VAR+set}：证明 null 参数**存在**且为空串，而不是没设。
	want := `hello world|dash-value|{"a":1,"b":["x"]}|3|true|set`
	if got := strings.TrimSpace(res.Stdout); got != want {
		t.Fatalf("params env:\n got %q\nwant %q", got, want)
	}
}

// TestRunJobEnvIdentity：契约 §6 的全部变量都对（含 v2 新增的三个）。
func TestRunJobEnvIdentity(t *testing.T) {
	j := newRunnerJob(t, `printf '%s|%s|%s|%s|%s|%s' `+
		`"$ANOTIFY_TASK_ID" "$ANOTIFY_CAPABILITY" "$ANOTIFY_PARAMS" `+
		`"$ANOTIFY_TRACE_ID" "$ANOTIFY_DIGEST" "$ANOTIFY_WORKDIR"`)
	j.Params = map[string]any{"title": "hello world", "n": float64(1)}
	j.TraceID = "trc_abc123"

	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q stdout=%q stderr=%q)", res.Status, statusSucceeded, res.Err, res.Stdout, res.Stderr)
	}
	want := strings.Join([]string{
		"tsk_test",
		"demo.prep@1",
		`{"n":1,"title":"hello world"}`,
		"trc_abc123",
		j.Digest,
		workDirOf(j),
	}, "|")
	if got := strings.TrimSpace(res.Stdout); got != want {
		t.Fatalf("identity env:\n got %q\nwant %q", got, want)
	}
}

// TestRunJobEnvEmptyTraceID：没有 trace_id 时变量**存在但为空**（不是不存在）。
//
// 契约 §6 把它列为契约变量；`set -u` 的脚本引用一个不存在的变量会直接炸，
// 所以宁可是空串。
func TestRunJobEnvEmptyTraceID(t *testing.T) {
	j := newRunnerJob(t, `printf '[%s][%s]' "${ANOTIFY_TRACE_ID+set}" "${ANOTIFY_TRACE_ID}"`)

	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "[set][]" {
		t.Fatalf("stdout = %q, want [set][]（变量存在且为空串）", got)
	}
}

// ---------- 工作目录 ----------

// TestRunJobWorkDirIsolation 是本期最重要的一条：两个**并发**任务各自在自己的
// 工作目录里跑，互相看不见对方的文件。
//
// v1 的 agent 所有任务共享进程 cwd：8 个并发任务会抢同一个 ./out.json、
// 同一个 git clone 目标。这条用例的形状就是那个故障：两个脚本各自先落一个
// 自己独有的文件，等 0.3s（足够对方写完）再检查对方的文件**不存在**。
// 共享 cwd 时两边都会看到 LEAK；独立目录时各自只看得到自己。
func TestRunJobWorkDirIsolation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")

	// 两个 job 用不同的 task_id（= 不同工作目录）与不同的标记文件名。
	mkJob := func(taskID, mine, theirs string) job {
		body := fmt.Sprintf("set -u\ncd \"$ANOTIFY_WORKDIR\"\ntouch %q\nsleep 0.3\n"+
			"if [ -e %q ]; then echo LEAK-%s; exit 1; fi\n"+
			"printf 'cwd=%%s' \"$(pwd)\"\n", mine, theirs, mine)
		art := testScriptArtifact("demo.prep", 1, bashScript(body))
		return job{
			TaskID:     taskID,
			Capability: "demo.prep",
			Version:    1,
			Artifact:   art,
			Digest:     mustDigest(t, art),
			WorkRoot:   root,
		}
	}
	ja := mkJob("tsk_iso_a", "a.txt", "b.txt")
	jb := mkJob("tsk_iso_b", "b.txt", "a.txt")

	var wg sync.WaitGroup
	results := make([]outcome, 2)
	for i, j := range []job{ja, jb} {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			results[i] = runJob(context.Background(), j)
		}(i, j)
	}
	wg.Wait()

	for i, res := range results {
		if res.Status != statusSucceeded {
			t.Fatalf("job#%d status = %q, want succeeded（err=%q stdout=%q stderr=%q）", i, res.Status, res.Err, res.Stdout, res.Stderr)
		}
		if strings.Contains(res.Stdout, "LEAK-") {
			t.Fatalf("job#%d 看到了另一个任务的文件：%s（工作目录没隔离）", i, res.Stdout)
		}
	}

	// cwd 必须是各自的工作目录，而且两者不同。
	cwdA := strings.TrimPrefix(strings.TrimSpace(results[0].Stdout), "cwd=")
	cwdB := strings.TrimPrefix(strings.TrimSpace(results[1].Stdout), "cwd=")
	if cwdA != workDirOf(ja) {
		t.Errorf("job A cwd = %q, want %q", cwdA, workDirOf(ja))
	}
	if cwdB != workDirOf(jb) {
		t.Errorf("job B cwd = %q, want %q", cwdB, workDirOf(jb))
	}
	if cwdA == cwdB {
		t.Fatalf("两个任务的 cwd 相同（%q）：工作目录没隔离", cwdA)
	}
}

// TestRunJobCleansWorkDirOnSuccess：成功后工作目录被删（契约 §5 的清理规则）。
func TestRunJobCleansWorkDirOnSuccess(t *testing.T) {
	j := newRunnerJob(t, "echo ok")
	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want succeeded（err=%q）", res.Status, res.Err)
	}
	if _, err := os.Stat(workDirOf(j)); !os.IsNotExist(err) {
		t.Fatalf("成功后工作目录仍然存在（%v）：契约要求成功后删除", err)
	}
	// 日志里仍要能指出这次跑在哪儿（res.WorkDir 不清空）。
	if res.WorkDir != workDirOf(j) {
		t.Errorf("res.WorkDir = %q, want %q", res.WorkDir, workDirOf(j))
	}
}

// TestRunJobKeepsWorkDirOnFailure：失败保留工作目录（现场要能翻）。
func TestRunJobKeepsWorkDirOnFailure(t *testing.T) {
	j := newRunnerJob(t, "echo boom >&2; exit 7")
	res := runJob(context.Background(), j)
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if _, err := os.Stat(workDirOf(j)); err != nil {
		t.Fatalf("失败后工作目录不在了（%v）：失败要保留现场", err)
	}
	// 展开的文件也在（人可以直接 cd 进去重跑）。
	if _, err := os.Stat(filepath.Join(workDirOf(j), "run.sh")); err != nil {
		t.Errorf("工作目录里没有入口文件: %v", err)
	}
}

// TestRunJobWorkDirRejectsBadTaskID：task_id 要当目录名用，所以必须校验。
//
// 它是**对端给的字符串**：不校验就等于把路径拼接交给中枢（../.. 会写到 data-dir 外面）。
func TestRunJobWorkDirRejectsBadTaskID(t *testing.T) {
	for _, id := range []string{"", "..", "../escape", "a/b", `a\b`, strings.Repeat("x", maxTaskIDLen+1)} {
		j := newRunnerJob(t, "echo hi")
		j.TaskID = id
		res := runJob(context.Background(), j)
		if res.Status != statusFailed {
			t.Errorf("task_id %q: status = %q, want failed", id, res.Status)
			continue
		}
		if !strings.Contains(res.Err, "任务 ID") {
			t.Errorf("task_id %q: err = %q, want 提到任务 ID 不合法", id, res.Err)
		}
	}
}

// ---------- 执行方式 ----------

// TestRunJobShebangPicksInterpreter：执行方式是**直接 exec 入口文件**，解释器由
// shebang 决定，不再有 sh -c。
//
// 两个方向一起证：
//  1. 正例：`#!/bin/bash` + bash 独有语法（[[ ]]、数组）必须成功——如果实现还是
//     `sh -c ./run.sh`，脚本照样会走 shebang，但 v1 那种「命令字符串」路线根本
//     不存在了（这里能跑通说明入口文件被执行了）；
//  2. 反例：shebang 指向不存在的解释器 → **起不来**（start 失败）。
//     如果实现是 `sh -c <command>`，sh 会跑起来再报 "not found"（退出码 127），
//     属于「跑完失败」而不是「起不来」——这条断言刚好把两种实现分开。
func TestRunJobShebangPicksInterpreter(t *testing.T) {
	t.Run("bash 独有语法", func(t *testing.T) {
		j := newRunnerJob(t, `
set -u
tools=(alpha beta)
[[ -n "${tools[0]}" ]] && echo "bash:${tools[0]}:${BASH_VERSION:+ok}" || exit 9
`)
		res := runJob(context.Background(), j)
		if res.Status != statusSucceeded {
			t.Fatalf("status = %q, want succeeded（err=%q stdout=%q stderr=%q）", res.Status, res.Err, res.Stdout, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "bash:alpha:ok") {
			t.Fatalf("stdout = %q, want 含 bash:alpha:ok", res.Stdout)
		}
	})

	t.Run("shebang 指向不存在的解释器 → 起不来", func(t *testing.T) {
		art := testScriptArtifact("demo.prep", 1, "#!/nonexistent/interp\necho hi\n")
		j := job{
			TaskID: "tsk_badshebang", Capability: "demo.prep", Version: 1,
			Artifact: art, Digest: mustDigest(t, art),
			WorkRoot: filepath.Join(t.TempDir(), "work"),
		}
		res := runJob(context.Background(), j)
		if res.Status != statusFailed {
			t.Fatalf("status = %q, want failed", res.Status)
		}
		if !strings.Contains(res.Err, "start entrypoint") {
			t.Fatalf("err = %q, want 含 start entrypoint（直接 exec 才有的失败形态）", res.Err)
		}
	})
}

// TestRunJobExitCodePropagated：非 0 退出 → failed，Err 含 "exit status <code>"，
// 而后半段是 **stderr 的首行**（只有它说得出「为什么挂了」）。
//
// 历史：两个流合并捕获的时期，summary（stdout 首行）会被拼进 Err；拆流之后
// Err 一度只看 stdout，于是一个只往 stderr 写东西的失败只剩下 "exit status 7"——
// 开发环境 trace_1793986152223364273 就是这么把唯一有用的诊断丢掉的
// （trace 页只有 "exit status 4: capability: haha@1"，真正的原因在 stderr 里）。
func TestRunJobExitCodePropagated(t *testing.T) {
	j := newRunnerJob(t, "echo boom >&2; exit 7")
	res := runJob(context.Background(), j)

	if res.Status != statusFailed {
		t.Fatalf("status = %q, want %q", res.Status, statusFailed)
	}
	if !strings.Contains(res.Err, "exit status 7") {
		t.Fatalf("Err = %q, want 含 %q", res.Err, "exit status 7")
	}
	if !strings.Contains(res.Err, "boom") {
		t.Fatalf("Err = %q, want 含 stderr 首行 %q（失败原因不在 stdout 里）", res.Err, "boom")
	}
	if res.Summary != "" {
		t.Fatalf("Summary = %q, want 空（summary 的口径仍是 stdout 首行，不因 stderr 而变）", res.Summary)
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Fatalf("Stderr = %q, want 含 %q（不合并捕获的意义就在这里）", res.Stderr, "boom")
	}
	if res.Stdout != "" {
		t.Fatalf("Stdout = %q, want 空：脚本没往 stdout 写东西，两个流不得串味", res.Stdout)
	}
	if res.Artifacts == nil || len(res.Artifacts) != 0 {
		t.Fatalf("Artifacts = %v, want 非 nil 空列表", res.Artifacts)
	}
}

// TestRunJobFailureErrorUsesStderrFirstLine：两个流都有内容时，Err 的后半段是
// **stderr 首行**，Summary 仍是 **stdout 首行** —— 两个字段各答各的问题：
//
//	summary = 这个能力说了什么（stdout）
//	error   = 为什么挂了（stderr）
//
// 两者混用的代价是真实的：开发环境那条 trace 的 error 是 "exit status 4:
// capability: haha@1"（脚本第一行 echo），真正的原因在 stderr。
func TestRunJobFailureErrorUsesStderrFirstLine(t *testing.T) {
	j := newRunnerJob(t, `echo "capability: demo@1"
	echo "data 不满足 expects[out]：missing required field waoo" >&2
	echo second >&2
	exit 4`)
	res := runJob(context.Background(), j)

	if res.Status != statusFailed || res.ExitCode != 4 {
		t.Fatalf("status=%q exit=%d, want failed/4", res.Status, res.ExitCode)
	}
	if want := "exit status 4: data 不满足 expects[out]：missing required field waoo"; res.Err != want {
		t.Fatalf("Err = %q, want %q（只带 stderr 首行，不续接后面的行）", res.Err, want)
	}
	if want := "capability: demo@1"; res.Summary != want {
		t.Fatalf("Summary = %q, want %q（summary 仍走 stdout，不受 error 换源影响）", res.Summary, want)
	}
	// 两个流本身一个字都不能少（诊断靠它们，error 只是摘要）。
	if !strings.Contains(res.Stdout, "capability: demo@1") {
		t.Fatalf("Stdout = %q, want 含脚本写的首行", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "second") {
		t.Fatalf("Stderr = %q, want 含第二行（截断只发生在窗口边界）", res.Stderr)
	}
}

// TestRunJobSuccess：成功判定的全部字段。
func TestRunJobSuccess(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob(t, "echo first line; echo second line"))

	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Err != "" {
		t.Fatalf("Err = %q, want empty", res.Err)
	}
	if res.Artifacts == nil {
		t.Fatal("Artifacts = nil, want 非 nil（空列表必须是 []，不是 null）")
	}
	if len(res.Artifacts) != 0 {
		t.Fatalf("Artifacts = %v, want 空", res.Artifacts)
	}
	if res.Summary != "first line" {
		t.Fatalf("Summary = %q, want %q", res.Summary, "first line")
	}
	if !strings.Contains(res.Stdout, "second line") {
		t.Fatalf("Stdout = %q, want 含完整输出", res.Stdout)
	}
}

// TestRunJobSuccessEmptyOutput：没有输出的成功命令，summary 不能是空串。
func TestRunJobSuccessEmptyOutput(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob(t, "true"))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Summary != "exit 0" {
		t.Fatalf("Summary = %q, want %q", res.Summary, "exit 0")
	}
}

// TestRunJobNoOutPlumbing 守住「out 这条腿真的没了」：
//
// 旧实现里 outcome.Out 是「脚本退出时写了哪个出口」的载体（TestRunJobOutNeverReported
// 当年只能断言它为 nil，因为那时还没有载体）。P1.5 硬切之后**结构体里没有这个
// 字段了**——断言从「值是 nil」变成「编译期就不存在」，这是收紧，不是放宽。
//
// 真正替代它的验收在 runner_test.go 的 TestRunJobIgnoresLegacyOutFile（写文件不再
// 产生任何效果）与 TestRunJobExampleCapability（产出搬到 emission）。
func TestRunJobNoOutPlumbing(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob(t, "echo done"))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want succeeded（err=%q）", res.Status, res.Err)
	}
	// 结果帧里也不允许出现 out 键：帧结构体已经没有该字段（见 protocol.go 的
	// resultFrame），所以这里只需确保编码出来没有它。
	frame, err := encodeFrame(&resultFrame{
		Type: frameTypeResult, TaskID: "tsk_x", Status: res.Status,
		Summary: res.Summary, Artifacts: res.Artifacts, Error: res.Err,
	})
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	if strings.Contains(string(frame), `"out"`) {
		t.Fatalf("result 帧里出现了 out 键（out 已退役）: %s", frame)
	}
}

// ---------- 超时 / 取消 / WaitDelay（v1 成果，全部保留）----------

// TestRunJobTimeout：TimeoutSec=1，命令里有一个会活过超时的孙进程 →
//   - runJob 必须在 3s 内返回（真的杀了，没等 `sleep 5` 自己结束）；
//   - Err 含 "timed out"/"after"；
//   - 孙进程也必须死（标记文件永远不出现 = 杀的是整个进程组）。
func TestRunJobTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived.txt")
	j := newRunnerJob(t, fmt.Sprintf("( sleep 2; echo survived > %q ) & sleep 5", marker))
	j.Artifact.Spec.Run.TimeoutSec = 1

	start := time.Now()
	res := runJob(context.Background(), j)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("runJob 用了 %s，超时没有及时杀掉子进程树", elapsed)
	}
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want %q", res.Status, statusFailed)
	}
	if !strings.Contains(res.Err, "timed out") {
		t.Fatalf("Err = %q, want 含 %q", res.Err, "timed out")
	}
	if !strings.Contains(res.Err, "after") {
		t.Fatalf("Err = %q, want 含超时时长（timed out after ...）", res.Err)
	}

	// 后台子 shell 本该在 t≈2s 写文件；runJob 在 t≈1s 返回，再观察到 t≈2.5s。
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("后台孙进程在超时后仍然活着并写了标记文件：进程组没被彻底杀掉")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunJobTimeoutFromMaxDuration：产物没写 timeoutSec 时用中枢的 max_duration_sec 兜底。
func TestRunJobTimeoutFromMaxDuration(t *testing.T) {
	j := newRunnerJob(t, "sleep 5")
	j.MaxDuration = 1 // 秒

	start := time.Now()
	res := runJob(context.Background(), j)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("runJob 用了 %s，max_duration_sec 没生效", elapsed)
	}
	if res.Status != statusFailed || !strings.Contains(res.Err, "timed out") {
		t.Fatalf("status=%q err=%q, want failed 且含 timed out", res.Status, res.Err)
	}
}

// TestRunJobCanceled：ctx 取消（cancel 帧/排空）→ failed + Err 含 "canceled"，
// 且整个进程组被杀。
func TestRunJobCanceled(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived.txt")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	j := newRunnerJob(t, fmt.Sprintf("( sleep 2; echo survived > %q ) & sleep 5", marker))

	start := time.Now()
	res := runJob(ctx, j)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("runJob 用了 %s，取消没有及时杀掉子进程树", elapsed)
	}
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want %q", res.Status, statusFailed)
	}
	if !strings.Contains(res.Err, "canceled") {
		t.Fatalf("Err = %q, want 含 %q", res.Err, "canceled")
	}

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("取消后后台孙进程仍然活着并写了标记文件：进程组没被彻底杀掉")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunJobWaitDelay 对应契约 §11.2（F2）：命令把自己**脱离进程组**（setsid 建
// 新会话），却继承了 stdout/stderr 管道。杀进程组管不到这个孤儿，而 exec 的 Wait
// 默认要等到**管道 EOF**（= 孤儿自己退出），于是 timeoutSec=1 的任务被拖到 ≈3s。
func TestRunJobWaitDelay(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("系统没有 setsid（util-linux）：造不出「脱离进程组但仍占着管道」的场景；Linux CI 上必须有该命令")
	}

	j := newRunnerJob(t, "setsid sleep 3 & sleep 10")
	j.Artifact.Spec.Run.TimeoutSec = 1

	start := time.Now()
	res := runJob(context.Background(), j)
	elapsed := time.Since(start)

	// 没有 WaitDelay 时 ≈3.0s（Wait 在等 setsid 出来的 sleep 3 关管道）。
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("runJob 用了 %s，want < 2.5s：Wait 还在等脱离进程组的孤儿关管道（缺 cmd.WaitDelay）", elapsed)
	}
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusFailed, res.Err)
	}
	if !strings.Contains(res.Err, "timed out") {
		t.Fatalf("Err = %q, want 含 %q", res.Err, "timed out")
	}
}

// TestRunJobWaitDelayKeepsTailOutput：WaitDelay 不能变成「第二个超时」——命令自己
// 退出后，同进程组里的后台子 shell 还会继续写，这点尾巴必须仍然被捕获。
func TestRunJobWaitDelayKeepsTailOutput(t *testing.T) {
	j := newRunnerJob(t, `( sleep 0.3; printf 'tail-ok\n' ) & printf 'head-ok\n'`)

	start := time.Now()
	res := runJob(context.Background(), j)
	elapsed := time.Since(start)

	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q stdout=%q stderr=%q)", res.Status, statusSucceeded, res.Err, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "head-ok") || !strings.Contains(res.Stdout, "tail-ok") {
		t.Fatalf("Stdout = %q, want 含 head-ok 与 0.3s 后才写的 tail-ok（WaitDelay 不该吞掉正常输出）", res.Stdout)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("runJob 用了 %s，正常命令不该被 WaitDelay 拖慢", elapsed)
	}
}

// ---------- 没有产物（防御性）----------

// TestRunJobNoArtifact：执行层自己也不能 panic，错误文案要与 client 一致。
func TestRunJobNoArtifact(t *testing.T) {
	res := runJob(context.Background(), job{TaskID: "tsk_x", Capability: "demo.prep", Version: 2})
	if res.Status != statusFailed || !strings.Contains(res.Err, "no artifact for demo.prep@2") {
		t.Fatalf("nil Artifact → status=%q err=%q, want failed 且含 %q",
			res.Status, res.Err, "no artifact for demo.prep@2")
	}
	if res.Artifacts == nil {
		t.Fatal("Artifacts = nil, want 非 nil")
	}

	// 入口为空：同样是「不能跑」而不是「跑了个空命令」。
	j := newRunnerJob(t, "echo hi")
	j.Artifact.Spec.Run.Entrypoint = ""
	if res := runJob(context.Background(), j); res.Status != statusFailed ||
		!strings.Contains(res.Err, "run.entrypoint 为空") {
		t.Fatalf("空 entrypoint → status=%q err=%q, want failed 且提到 run.entrypoint", res.Status, res.Err)
	}
}

// ---------- stdout / stderr 分别捕获（契约 §1）----------

// TestRunJobSeparatesStdoutStderr 是本期拆掉「合并捕获」的直接证据：
// 两个流各一条管道，stdout 的东西不得到 stderr 里、反之亦然。
//
// 合并捕获的现场代价：`exit status 2` 的真正原因（旧二进制的
// "flag provided but not defined: -to"）在 stderr 里，混进 stdout 后就分不清
// 「脚本说的」和「工具报的」了。
func TestRunJobSeparatesStdoutStderr(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob(t, "printf 'out\\n'; printf 'err\\n' >&2"))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Stdout != "out\n" {
		t.Fatalf("Stdout = %q, want %q（不得混入 stderr）", res.Stdout, "out\n")
	}
	if res.Stderr != "err\n" {
		t.Fatalf("Stderr = %q, want %q（不得混入 stdout）", res.Stderr, "err\n")
	}
	if res.Truncated {
		t.Fatalf("Truncated = true, want false（这么短的输出不该被截断）")
	}
	if want := int64(len("out\n") + len("err\n")); res.Bytes != want {
		t.Fatalf("Bytes = %d, want %d（两个流的真实字节数之和）", res.Bytes, want)
	}
}

// TestRunJobLogsTruncatedPerStream：每个流各自 32 KiB 上限、**保留尾部**、
// 截断时在末尾追加一行中文标记，另一条没超的流完整保留。
func TestRunJobLogsTruncatedPerStream(t *testing.T) {
	// 纯 bash 造正好 100000 字节的 stdout（不依赖 yes/head）：
	// 1000 × 99 = 99000，再 989 个，最后是 11 字节的尾部标记 → 100000。
	body := `s=; i=0; while [ $i -lt 1000 ]; do s="${s}x"; i=$((i+1)); done; ` +
		`i=0; while [ $i -lt 99 ]; do printf '%s' "$s"; i=$((i+1)); done; ` +
		`i=0; while [ $i -lt 989 ]; do printf 'x'; i=$((i+1)); done; ` +
		`printf 'TAIL-MARKER'; printf 'err-small' >&2`

	res := runJob(context.Background(), newRunnerJob(t, body))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q stderr=%q)", res.Status, statusSucceeded, res.Err, res.Stderr)
	}

	marker := fmt.Sprintf("\n…（日志已截断：共 %d 字节，只保留最后 %d 字节）", 100000, maxLogStreamBytes)
	if !strings.HasSuffix(res.Stdout, marker) {
		t.Fatalf("Stdout 末尾没有截断标记，want 逐字以 %q 结尾（len=%d）", marker, len(res.Stdout))
	}
	for _, want := range []string{"共 100000 字节", "只保留最后 32768 字节"} {
		if !strings.Contains(marker, want) {
			t.Fatalf("截断标记里缺 %q：%q", want, marker)
		}
	}
	tail := strings.TrimSuffix(res.Stdout, marker)
	if len(tail) != maxLogStreamBytes {
		t.Fatalf("保留的尾部 = %d 字节，want %d（上限就是每个流的窗口大小）", len(tail), maxLogStreamBytes)
	}
	if !strings.HasSuffix(tail, "TAIL-MARKER") {
		// 保留头部而不是尾部的实现会在这里红：标记串早被扔了。
		t.Fatalf("尾部内容丢了：保留的最后 %d 字节 = %q，want 以 TAIL-MARKER 结尾",
			len("TAIL-MARKER"), tail[max(0, len(tail)-len("TAIL-MARKER")):])
	}

	// 另一条流没超：完整保留、不加标记。
	if res.Stderr != "err-small" {
		t.Fatalf("Stderr = %q, want %q（没超上限的流不得被截断/加标记）", res.Stderr, "err-small")
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true（stdout 被截断了，这个事实必须传出去）")
	}
	if want := int64(100000 + len("err-small")); res.Bytes != want {
		t.Fatalf("Bytes = %d, want %d（两流真实字节数之和，不是留下的长度）", res.Bytes, want)
	}
	if len(res.Summary) != summaryMaxBytes {
		t.Fatalf("Summary len = %d, want %d（按字节截断）", len(res.Summary), summaryMaxBytes)
	}
}

// TestRunJobInvalidUTF8Replaced：子进程吐的字节不一定是 UTF-8，而日志要能过 JSON。
//
// 不处理的后果很贵：json.Marshal 会报 "invalid character"，整条上传在最后一步失败，
// 于是「任务为什么炸」还是没留下来（只不过换了一个地方丢）。
//
// 替换用的是 strings.ToValidUTF8，它的口径是「**一段连续**非法字节 → 一个 ?」
// （不是每个字节一个），这条用例把这个口径钉下来。
func TestRunJobInvalidUTF8Replaced(t *testing.T) {
	// stdout：合法 + 连续两个非法字节（一段）+ 合法 + 一个非法；stderr：单个非法字节。
	res := runJob(context.Background(), newRunnerJob(t, `printf 'ok\xff\xfe!\x80'; printf '\xc3' >&2`))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Stdout != "ok?!?" {
		t.Fatalf("Stdout = %q, want %q（一段连续非法字节 → 一个 ?）", res.Stdout, "ok?!?")
	}
	if res.Stderr != "?" {
		t.Fatalf("Stderr = %q, want %q", res.Stderr, "?")
	}
	for name, s := range map[string]string{"stdout": res.Stdout, "stderr": res.Stderr} {
		if !utf8.ValidString(s) {
			t.Fatalf("%s 不是合法 UTF-8：%q", name, s)
		}
		if _, err := json.Marshal(s); err != nil {
			t.Fatalf("json.Marshal(%s) 报错（上传会在最后一步失败）: %v", name, err)
		}
	}
}

// TestRunJobSummaryIsFirstStdoutLine：summary 的口径**不变**（stdout 第一行），
// 不因为 stderr 先写就换源，也不 fallback 到 stderr。
func TestRunJobSummaryIsFirstStdoutLine(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob(t, "printf 'err-first\\n' >&2; printf 'out-first\\n'"))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Summary != "out-first" {
		t.Fatalf("Summary = %q, want %q（summary 只认 stdout 第一行）", res.Summary, "out-first")
	}
	if res.Stderr != "err-first\n" {
		t.Fatalf("Stderr = %q, want %q", res.Stderr, "err-first\n")
	}
}

// TestStreamLogWindow 钉住滑动窗口本身的边界（不经过进程，直接打 Write）。
func TestStreamLogWindow(t *testing.T) {
	marker := func(total int64, limit int) string {
		return fmt.Sprintf("\n…（日志已截断：共 %d 字节，只保留最后 %d 字节）", total, limit)
	}

	l := newStreamLog(8)
	// 单笔超过窗口：只留它的尾部。
	if n, err := l.Write([]byte("0123456789AB")); n != 12 || err != nil {
		t.Fatalf("Write = (%d, %v), want (12, nil)（短写会让 exec 把子进程判成写错误）", n, err)
	}
	if got, want := l.String(), "456789AB"+marker(12, 8); got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	if l.Total() != 12 {
		t.Fatalf("Total = %d, want 12（真实写入量，不是 len(buf)）", l.Total())
	}
	// 多笔追加同样只留尾部（窗口边界是跨 Write 的，不是每笔一个窗口）。
	l = newStreamLog(8)
	l.Write([]byte("abcdef"))
	l.Write([]byte("ghij"))
	if got, want := l.String(), "cdefghij"+marker(10, 8); got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	// 没超窗口：不加任何标记。
	small := newStreamLog(8)
	small.Write([]byte("abc"))
	if got := small.String(); got != "abc" {
		t.Fatalf("String = %q, want %q（没截断就不该有标记）", got, "abc")
	}
}

// ---------- buildEnv ----------

// TestBuildEnv：base 里的 ANOTIFY_* 残留被剔除、产物的 run.env 不能覆盖
// ANOTIFY_*、契约变量齐全、输出按变量名排序。
func TestBuildEnv(t *testing.T) {
	base := []string{
		"HOME=/home/worker",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"ZZZ=last",
		"ANOTIFY_TASK_ID=stale-from-parent",
		"ANOTIFY_PARAM_STALE=1",
		"ANOTIFY_CANCEL_TOKEN=leak",
		"malformed-entry", // 没有 "="：丢掉，别污染子进程环境
	}
	art := testScriptArtifact("demo.prep", 7, bashScript("true"))
	art.Spec.Run.Env = map[string]string{
		"LOG_LEVEL": "info",
		"MODE":      "strict",
		// run.env 里的 ANOTIFY_* 一律忽略（契约变量优先级最高）。
		"ANOTIFY_TASK_ID":   "hijack",
		"ANOTIFY_PARAM_N":   "999",
		"ANOTIFY_UNDECLARE": "nope",
	}
	j := job{
		TaskID: "tsk_build", Capability: "demo.prep", Version: 7,
		Params:   map[string]any{"title": "t", "n": float64(2)},
		Artifact: art, Digest: "sha256:" + strings.Repeat("a", 64), TraceID: "trc_build",
	}

	// buildEnv 会按契约 §5 把出口契约写进 $ANOTIFY_TASKDIR/emit.json，所以这里
	// 必须给一个**真实存在**的目录（假路径会让 buildEnv 直接报错，那就没在测环境了）。
	workDir := t.TempDir()
	got, err := buildEnv(base, j, workDir, workDir, "/opt/anotify/bin/anotify-agent")
	if err != nil {
		t.Fatalf("buildEnv: unexpected error: %v", err)
	}
	want := []string{
		// 能力脚本靠它调 `anotify-agent emit`（out 已退役，emit 是唯一产出通道）。
		"ANOTIFY_AGENT_BIN=/opt/anotify/bin/anotify-agent",
		"ANOTIFY_CAPABILITY=demo.prep@7",
		"ANOTIFY_DIGEST=sha256:" + strings.Repeat("a", 64),
		// 出口契约（契约 §5）：没端口时是 []（不是空串、不是 null），且**始终存在**。
		"ANOTIFY_EMIT_PORTS=[]",
		`ANOTIFY_PARAMS={"n":2,"title":"t"}`,
		"ANOTIFY_PARAM_N=2",
		"ANOTIFY_PARAM_TITLE=t",
		"ANOTIFY_TASKDIR=" + workDir,
		"ANOTIFY_TASK_ID=tsk_build",
		"ANOTIFY_TRACE_ID=trc_build",
		"ANOTIFY_WORKDIR=" + workDir,
		"HOME=/home/worker",
		"LOG_LEVEL=info",
		"MODE=strict",
		// PATH 打头：agent 自己所在目录排最前（见 TestBuildEnvPathPrefix*）。
		"PATH=/opt/anotify/bin:/usr/local/bin:/usr/bin:/bin",
		"ZZZ=last",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildEnv:\n got %v\nwant %v", got, want)
	}

	// ANOTIFY_OUT_FILE **不能再出现**：它是已退役的出口回报文件（P1.5）。
	// 断言「不存在」而不是不写它——否则哪天有人“顺手兼容一下”就又长回来了。
	for _, kv := range got {
		if strings.HasPrefix(kv, "ANOTIFY_OUT_FILE=") {
			t.Fatalf("buildEnv 仍在注入已退役的 ANOTIFY_OUT_FILE: %q", kv)
		}
	}

	// 排序断言（不依赖上面那份期望值，也算说明「按变量名排序」的口径）。
	names := make([]string, 0, len(got))
	for _, kv := range got {
		names = append(names, strings.SplitN(kv, "=", 2)[0])
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("变量名未排序：%q 在 %q 之前", names[i-1], names[i])
		}
	}

	// 子进程真的看不到那些残留。
	j2 := newRunnerJob(t, `printf '%s' "${ANOTIFY_PARAM_STALE-unset}"`)
	j2.Artifact.Spec.Run.Env = map[string]string{"ANOTIFY_PARAM_STALE": "1"}
	res := runJob(context.Background(), j2)
	if got := strings.TrimSpace(res.Stdout); got != "unset" {
		t.Fatalf("子进程看到 ANOTIFY_PARAM_STALE=%q, want 完全没设（unset）", got)
	}
}

// TestBuildEnvPathPrefix 是**现场故障的根本修复**：脚本里的 `anotify-agent`
// 必须是「跑这个任务的那个 agent」，而不是 PATH 上碰巧先出现的那份。
//
// 现场：节点上 /usr/local/bin/anotify-agent 是旧副本（不认 emit 的 --to），
// 脚本里 `anotify-agent emit --to …` 被它拒绝（exit 2，
// "flag provided but not defined: -to"），而模块的 PATH 里本来就把它排在了真二进制前面。
func TestBuildEnvPathPrefix(t *testing.T) {
	const agentBin = "/opt/anotify/agent/anotify-agent"
	wantDir := filepath.Dir(agentBin)

	cases := []struct {
		name     string
		base     []string
		runEnv   map[string]string
		agentBin string
		wantPath string
	}{
		{
			name:     "父环境已有 PATH：自己目录排最前",
			base:     []string{"PATH=/usr/local/bin:/usr/bin:/bin"},
			agentBin: agentBin,
			wantPath: wantDir + ":/usr/local/bin:/usr/bin:/bin",
		},
		{
			name:     "父环境没有 PATH：就是自己目录",
			base:     []string{"HOME=/root"},
			agentBin: agentBin,
			wantPath: wantDir,
		},
		{
			// 产物 run.env 里写 PATH 也压不过它（顺序：父环境 → run.env → 打头）。
			name:     "产物 run.env 设了 PATH 也不影响打头",
			base:     []string{"PATH=/usr/bin"},
			runEnv:   map[string]string{"PATH": "/custom/bin"},
			agentBin: agentBin,
			wantPath: wantDir + ":/custom/bin",
		},
		{
			// 取不到自己的路径（os.Executable 失败）→ PATH 原样不动：
			// 宁可缺这一条，也不要塞进 "." 或空段（空段等义于当前目录）。
			name:     "agentBin 为空：PATH 不动",
			base:     []string{"PATH=/usr/local/bin:/usr/bin:/bin"},
			agentBin: "",
			wantPath: "/usr/local/bin:/usr/bin:/bin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			art := testScriptArtifact("demo.prep", 1, bashScript("true"))
			art.Spec.Run.Env = tc.runEnv
			j := job{TaskID: "tsk_path", Capability: "demo.prep", Version: 1, Artifact: art}

			workDir := t.TempDir()
			env, err := buildEnv(tc.base, j, workDir, workDir, tc.agentBin)
			if err != nil {
				t.Fatalf("buildEnv: %v", err)
			}
			got, _ := envLookup(env, "PATH")
			if got != tc.wantPath {
				t.Fatalf("PATH = %q, want %q", got, tc.wantPath)
			}
			if tc.agentBin != "" {
				first := strings.Split(got, string(os.PathListSeparator))[0]
				if first != filepath.Dir(tc.agentBin) {
					t.Fatalf("PATH 第一段 = %q, want %q（agent 自己所在目录）", first, filepath.Dir(tc.agentBin))
				}
			}
		})
	}
}

// TestBuildEnvPathPrefixEndToEnd：真起一个子进程，它看到的 `anotify-agent`
// 必须解析到**本 agent 的目录**（而不是 PATH 上那个旧的）。
func TestBuildEnvPathPrefixEndToEnd(t *testing.T) {
	// 真二进制（整个包共用一次编译），放在它自己的目录里。
	bin := buildAgentBinary(t)
	old := executablePath
	executablePath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executablePath = old })

	// 脚本打印 `command -v anotify-agent` 的结果：修复前它会指向 PATH 上那个；
	// 修复后必须是 bin 自己。
	res := runJob(context.Background(), newRunnerJob(t, `command -v anotify-agent`))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want succeeded（err=%q stderr=%q）", res.Status, res.Err, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != bin {
		t.Fatalf("command -v anotify-agent = %q, want %q（PATH 打头没生效）", got, bin)
	}
}

// TestBuildEnvInjectsWorkDir：ANOTIFY_WORKDIR 必须等于**进程 cwd**（脚本靠它
// cd / 存中间产物）；ANOTIFY_TASKDIR 则是产物文件所在的任务目录——能力声明了
// run.workDir 之后两者会分开（见 workdir_test.go 的端到端用例）。
func TestBuildEnvInjectsWorkDir(t *testing.T) {
	j := job{TaskID: "t", Capability: "c", Version: 1}
	// 真目录：buildEnv 要写 emit.json（契约 §5）。
	workDir := t.TempDir()
	got, err := buildEnv(nil, j, workDir, workDir, "")
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	// agentBin 为空 → 不注入 ANOTIFY_AGENT_BIN（宁可缺键也不给空串，
	// 否则脚本里的 `"$ANOTIFY_AGENT_BIN" emit` 会变成执行 `emit ...`）。
	want := []string{
		"ANOTIFY_CAPABILITY=c@1",
		"ANOTIFY_DIGEST=",
		"ANOTIFY_EMIT_PORTS=[]",
		"ANOTIFY_PARAMS={}",
		"ANOTIFY_TASKDIR=" + workDir,
		"ANOTIFY_TASK_ID=t",
		"ANOTIFY_TRACE_ID=",
		"ANOTIFY_WORKDIR=" + workDir,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildEnv:\n got %v\nwant %v", got, want)
	}
}

// TestBuildEnvEmptyParams：params 为空 → ANOTIFY_PARAMS 是 {}（不是 null）。
func TestBuildEnvEmptyParams(t *testing.T) {
	// 真目录：buildEnv 要写 emit.json（契约 §5）。
	workDir := t.TempDir()
	got, err := buildEnv(nil, job{TaskID: "t", Capability: "c", Version: 1}, workDir, workDir, "")
	if err != nil {
		t.Fatalf("buildEnv: unexpected error: %v", err)
	}
	if !slicesContain(got, "ANOTIFY_PARAMS={}") {
		t.Fatalf("buildEnv = %v, want 含 ANOTIFY_PARAMS={}", got)
	}

	// Artifact 为 nil 也不能 panic（防御：client 侧正常不会走到这儿）。
	nilWorkDir := t.TempDir()
	if _, err := buildEnv(nil, job{}, nilWorkDir, nilWorkDir, ""); err != nil {
		t.Fatalf("buildEnv(nil artifact): unexpected error: %v", err)
	}
}

// TestBuildEnvParamError：参数值无法序列化 → buildEnv 返回错误（而不是静默丢参数）。
func TestBuildEnvParamError(t *testing.T) {
	j := job{
		TaskID: "t", Capability: "c", Version: 1,
		Params: map[string]any{"bad": make(chan int)},
	}
	// 真目录（buildEnv 会写 emit.json）：这样错误只可能来自参数本身。
	workDir := t.TempDir()
	if _, err := buildEnv(nil, j, workDir, workDir, ""); err == nil {
		t.Fatal("buildEnv: want error for 不可序列化的参数值, got nil")
	}
}

// TestEffectiveTimeout：命令行覆盖 > 产物 timeoutSec > 中枢 max_duration_sec > 0。
func TestEffectiveTimeout(t *testing.T) {
	withTimeout := func(sec int) *artifact.Artifact {
		a := testScriptArtifact("c", 1, bashScript("true"))
		a.Spec.Run.TimeoutSec = sec
		return a
	}
	cases := []struct {
		name string
		j    job
		want time.Duration
	}{
		{"产物优先", job{Artifact: withTimeout(5), MaxDuration: 9}, 5 * time.Second},
		{"兜底 max_duration", job{Artifact: withTimeout(0), MaxDuration: 9}, 9 * time.Second},
		{"都没有", job{Artifact: withTimeout(0)}, 0},
		{"无产物但有 max_duration", job{MaxDuration: 3}, 3 * time.Second},
		{"无产物无 max_duration", job{}, 0},
		{"命令行覆盖压过产物", job{Artifact: withTimeout(900), TimeoutOverride: 30}, 30 * time.Second},
	}
	for _, tc := range cases {
		if got := effectiveTimeout(tc.j); got != tc.want {
			t.Errorf("%s: effectiveTimeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ---------- 小工具 ----------

// TestParamEnvName：大写 + 非 [A-Z0-9_] → '_'。
func TestParamEnvName(t *testing.T) {
	cases := map[string]string{
		"title":        "TITLE",
		"my-param":     "MY_PARAM",
		"a.b/c":        "A_B_C",
		"Mixed_Case 1": "MIXED_CASE_1",
		"already_OK":   "ALREADY_OK",
		"中文":           "__", // 按 rune 归一：两个字符 → 两个 '_'
		"":             "",
	}
	for in, want := range cases {
		if got := paramEnvName(in); got != want {
			t.Errorf("paramEnvName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOneLine：取首个非空行、按字节截断且不破 UTF-8。
func TestOneLine(t *testing.T) {
	if got := oneLine("first\nsecond\nthird", summaryMaxBytes); got != "first" {
		t.Fatalf("oneLine 多行 = %q, want %q", got, "first")
	}
	if got := oneLine("\n\n   spaced line  \nnext", summaryMaxBytes); got != "spaced line" {
		t.Fatalf("oneLine 跳过空行/去空白 = %q, want %q", got, "spaced line")
	}
	if got := oneLine("  \n\t\n", summaryMaxBytes); got != "" {
		t.Fatalf("oneLine 全空白 = %q, want 空串", got)
	}
	if got := oneLine("ok\r\nnext", summaryMaxBytes); got != "ok" {
		t.Fatalf("oneLine CRLF = %q, want %q", got, "ok")
	}

	// é 是 2 字节：maxBytes=7 落在第 4 个 é 的中间，必须回退到 6 字节处。
	long := strings.Repeat("é", 10)
	got := oneLine(long, 7)
	if got != "ééé" {
		t.Fatalf("oneLine 截断 = %q, want %q", got, "ééé")
	}
	if len(got) > 7 {
		t.Fatalf("oneLine 截断后 %d 字节, want <= 7", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("oneLine 截出了半个字符：%q", got)
	}
}

// TestOneLineSummaryThroughRunJob：命令多行输出时，summary 只取首行（不含换行）。
func TestOneLineSummaryThroughRunJob(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob(t, "printf 'alpha\\nbeta\\n'"))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Summary != "alpha" {
		t.Fatalf("Summary = %q, want %q", res.Summary, "alpha")
	}
	if strings.ContainsAny(res.Summary, "\n\r") {
		t.Fatalf("Summary 含换行：%q", res.Summary)
	}
}

// slicesContain 是断言用的小工具（避免为一处判断引入额外的 import 语义分歧）。
func slicesContain(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestRunJobIgnoresLegacyOutFile 是「退出时写文件回报出口」这条腿**真的断了**的证据。
//
// 改动前这里是 TestOutcomeOutFromFile + TestRunJobReportsOut：脚本写
// `$ANOTIFY_OUT_FILE`，runner 读它、校验 ∈ spec.outputs、填进 outcome.Out。P1.5 把
// 那条腿整条删了（emit 是唯一的产出通道），所以正确的新语义是**写了一点用都没有**：
//
//   - 环境里根本没有 ANOTIFY_OUT_FILE（老脚本在 `set -u` 下会当场炸，比静默断链好）；
//   - 就算脚本硬编码把 `.anotify-out` 写进工作目录，也没人读它（下面就是这条）；
//   - 于是任务照常成功（退出码是唯一成败依据），但没有产出、没有 out。
//
// 这不是「放宽断言」：原来断言的是「out == needs-fix」，现在断言的是「同一个动作
// 不再产生 out，也不再产生任何输出」。产出搬到了 emission，见
// TestRunJobExampleCapability 与 emit_cmd_test.go 的真进程用例。
func TestRunJobIgnoresLegacyOutFile(t *testing.T) {
	// 脚本干三件事：① 把工作目录里的 .anotify-out 写上（老约定）；② 打印环境里
	// 到底有没有 ANOTIFY_OUT_FILE；③ 正常退出。
	body := `printf 'needs-fix\n' > .anotify-out
printf 'env=%s\n' "${ANOTIFY_OUT_FILE-unset}"`
	art := testScriptArtifact("demo.out", 1, bashScript(body))
	art.Spec.Outputs = []string{"clean", "needs-fix"}

	j := job{
		TaskID: "tsk_legacy_out", Capability: "demo.out", Version: 1,
		Artifact: art, WorkRoot: filepath.Join(t.TempDir(), "work"),
	}
	res := runJob(context.Background(), j)

	// ① 退出码是唯一成败依据：文件写了也不影响成功。
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want succeeded（err=%q）", res.Status, res.Err)
	}
	// ② 老脚本看不到那个变量（没被注入）。
	if !strings.Contains(res.Stdout, "env=unset") {
		t.Fatalf("stdout = %q，期望含 env=unset（ANOTIFY_OUT_FILE 不得再被注入）", res.Stdout)
	}
}

// TestRunJobLegacyOutFileNoEffectOnFailure 失败路径下同一个文件同样没人读：
// 任务还是 failed，原因来自进程（exit status），而且**不会**因为出口名非法而
// 多出一条「out not declared」的错误（那是已经删掉的那条腿的产物）。
func TestRunJobLegacyOutFileNoEffectOnFailure(t *testing.T) {
	art := testScriptArtifact("demo.failout", 1,
		bashScript(`printf 'failed\n' > .anotify-out; echo "编译失败"; exit 3`))
	art.Spec.Outputs = []string{"failed", "retry"}

	j := job{
		TaskID: "tsk_legacy_failout", Capability: "demo.failout", Version: 1,
		Artifact: art, WorkRoot: filepath.Join(t.TempDir(), "work"),
	}
	res := runJob(context.Background(), j)

	if res.Status != statusFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if !strings.Contains(res.Err, "exit status 3") {
		t.Fatalf("Err = %q，期望含 exit status 3（成败只看进程事实）", res.Err)
	}
	if strings.Contains(res.Err, "not declared") || strings.Contains(res.Err, "anotify-out") {
		t.Fatalf("Err = %q，不该再出现出口文件相关的错误（那条腿已删）", res.Err)
	}
	// 失败会保留工作目录：文件真的在那儿，而且确实不被读。
	if _, err := os.Stat(filepath.Join(res.WorkDir, ".anotify-out")); err != nil {
		t.Fatalf("前置条件不成立：脚本没写出 .anotify-out（%v）", err)
	}
}

// TestRunJobFailureKeepsWorkDir 失败保留工作目录（事后翻现场）；成功才删。
func TestRunJobFailureKeepsWorkDir(t *testing.T) {
	art := testScriptArtifact("demo.keepdir", 1, bashScript(`exit 2`))
	res := runJob(context.Background(), job{
		TaskID: "tsk_keepdir", Capability: "demo.keepdir", Version: 1,
		Artifact: art, WorkRoot: t.TempDir(),
	})
	if res.Status != statusFailed {
		t.Fatalf("status = %q，期望 failed", res.Status)
	}
	if res.WorkDir == "" {
		t.Fatal("WorkDir 为空，无法翻现场")
	}
	if _, err := os.Stat(res.WorkDir); err != nil {
		t.Fatalf("失败后工作目录应保留: %v", err)
	}
}

// TestRunJobUndeclaredOutNoLongerFails 旧语义：脚本写一个**没声明**的出口名 → 整次
// 执行判 failed（出口是封闭集合，I2）。
//
// 新语义（P1.5）：那条腿删了，所以「写了没声明的出口」这件事**不再存在**——文件没
// 人读，任务成败只看进程事实。封闭集合的判定搬到了 emit 当时（节点当场拒这一次
// emit，CLI 退出码 3），比事后判整个任务失败更早、更准（见 emit_test.go 的
// TestEmitRejectPort）。这里钉住「不会因为一个没人读的文件把任务弄失败」。
func TestRunJobUndeclaredOutNoLongerFails(t *testing.T) {
	art := testScriptArtifact("demo.out", 1, bashScript(`printf 'bogus\n' > .anotify-out; echo ok`))
	art.Spec.Outputs = []string{"clean"}

	res := runJob(context.Background(), job{
		TaskID: "tsk_out_bad", Capability: "demo.out", Version: 1,
		Artifact: art, WorkRoot: t.TempDir(),
	})
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q（err=%q），期望 succeeded：出口文件已退役，不该再影响任务成败",
			res.Status, res.Err)
	}
	if strings.Contains(res.Err, "not declared") {
		t.Fatalf("err = %q，不该再出现 out 校验错误（那条腿已删）", res.Err)
	}
}

// TestRunJobExampleCapability 把**真的内置示例能力**喂给真的 runner 跑一遍，并断言
// 它的产出真的落到了 **emission** 上。
//
// 这一条防的是「声明与脚本对不上」：能力声明 outputs=["echoed"]，脚本就必须真的
// emit 一个**声明过的**端口。改动前这里断言的是 `out == echoed`（脚本写出口文件）；
// P1.5 之后同样的证据在 emission 里：port 必须是 echoed，而且 data 就是入参原样
// （脚本用 `--data "$ANOTIFY_PARAMS"` 把参数交给下游）。
//
// 为什么用真二进制：ANOTIFY_AGENT_BIN 指向的真 `anotify-agent emit` 会走
// 「unix socket → 本地落盘 → ack」整条链，替掉它这一段就只剩一个字符串 mock 了。
func TestRunJobExampleCapability(t *testing.T) {
	bin := buildAgentBinary(t)
	old := executablePath
	executablePath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executablePath = old })

	opt := defaultEmitOptions(t, t.TempDir())
	opt.TaskID = "tsk_example"
	opt.Ports = append([]string(nil), store.ExampleCapability.Outputs...)
	srv, rec := startEmitServer(t, opt)

	res := runJob(context.Background(), job{
		TaskID:     opt.TaskID,
		Capability: store.ExampleCapability.Name,
		Version:    store.ExampleCapability.Version,
		Params:     map[string]any{"message": "hello"},
		Artifact:   store.ExampleCapability.Artifact,
		WorkRoot:   t.TempDir(),
		Emit:       srv,
		// 真实派发的 assign 帧一定带 ports（那是"本任务能往哪些出口 emit"的白名单），
		// 单出口能力的脚本靠它自动定位出口（脚本不再写 --port）。
		Ports: append([]string(nil), store.ExampleCapability.Outputs...),
	})
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q（err=%q）—— 示例能力必须开箱即能跑通", res.Status, res.Err)
	}
	if !strings.Contains(res.Summary, "hello") {
		t.Fatalf("summary = %q，期望包含参数消息（summary 取 stdout 首行）", res.Summary)
	}

	// 产出搬到了 emission：这里就是原来 `out == echoed` 的那条断言的等价物。
	frames := rec.all()
	if len(frames) != 1 {
		t.Fatalf("emit 帧数 = %d，期望 1（示例能力只 emit 一次）", len(frames))
	}
	if frames[0].Port != "echoed" {
		t.Fatalf("emit.port = %q，期望 echoed（声明与脚本对不上就是最初那个 bug）", frames[0].Port)
	}
	if got := string(frames[0].Data); got != `{"message":"hello"}` {
		t.Fatalf("emit.data = %s，期望入参原样透传 {\"message\":\"hello\"}", got)
	}
}

// TestRunJobSummarySurvivesTailTruncation：summary 的语义是 stdout 的**第一行**，
// 而日志窗口只保留最后 32 KiB——第一行会被挤出去。
//
// 这就是头部旁路窗口（streamLog.head）存在的理由：日志留尾部（错误通常在最后），
// 但 summary（进 result 帧、进 tasks.summary、失败时还进 error）必须仍是第一行。
// 只拿 String() 去 oneLine 的实现会在这里红：它取到的是尾部窗口的第一行。
func TestRunJobSummarySurvivesTailTruncation(t *testing.T) {
	// 第一行很短，后面再灌 100000 字节（> 32 KiB 窗口）。
	body := `printf 'first-line\n'; ` +
		`s=; i=0; while [ $i -lt 1000 ]; do s="${s}x"; i=$((i+1)); done; ` +
		`i=0; while [ $i -lt 100 ]; do printf '%s' "$s"; i=$((i+1)); done`

	res := runJob(context.Background(), newRunnerJob(t, body))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q stderr=%q)", res.Status, statusSucceeded, res.Err, res.Stderr)
	}
	if res.Summary != "first-line" {
		t.Fatalf("Summary = %q, want %q（summary 必须是 stdout 第一行，不是尾部窗口的第一行）",
			res.Summary, "first-line")
	}
	// 反向证据：尾部窗口里确实没有第一行（否则这条用例什么都没证明）。
	if strings.Contains(res.Stdout, "first-line") {
		t.Fatalf("尾部窗口里还有第一行，夹具失效（窗口 = 最后 %d 字节）", maxLogStreamBytes)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true（100000 字节的 stdout 必然被截断）")
	}
}
