package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 本文件覆盖契约 §5 的 W3 必测矩阵（runner）。
//
// 测试只依赖 POSIX sh 与最基本的 coreutils（sleep/printf/echo 都是 shell 内建或
// 必备命令），所以能跑在 Linux CI 上；没有任何网络、没有 fake 时钟。

// newRunnerJob 造一个「跑 command」的最小 job：node.yaml 的 run 里只有 command。
// 名字带 runner 前缀：同一个包里还有 client/main 的测试文件，helper 名别撞。
func newRunnerJob(command string) job {
	return job{
		TaskID:     "tsk_test",
		Capability: "demo.prep",
		Version:    1,
		Spec:       &runSpec{Runner: "bash", Command: command},
	}
}

// TestRunJobParamEnv 对应 W3-1：params → ANOTIFY_PARAM_*（只走环境变量）。
//
// 命令里一个 {{param}} 都没有，全是 $ANOTIFY_PARAM_* —— 这就是「参数只走环境
// 变量、零插值」在测试里的形状。同时覆盖：嵌套 object 的紧凑 JSON、名字归一
// （my-param → MY_PARAM）、number/bool 字面量、null → 空串（但仍存在）。
func TestRunJobParamEnv(t *testing.T) {
	j := job{
		TaskID:     "tsk_params",
		Capability: "demo.prep",
		Version:    1,
		Params: map[string]any{
			"title":    "hello world",
			"my-param": "dash-value",
			"nested":   map[string]any{"a": 1, "b": []any{"x"}},
			"count":    float64(3),
			"flag":     true,
			"nothing":  nil,
		},
		Spec: &runSpec{
			Runner: "bash",
			Command: `printf '%s|%s|%s|%s|%s|%s\n' ` +
				`"$ANOTIFY_PARAM_TITLE" "$ANOTIFY_PARAM_MY_PARAM" "$ANOTIFY_PARAM_NESTED" ` +
				`"$ANOTIFY_PARAM_COUNT" "$ANOTIFY_PARAM_FLAG" "${ANOTIFY_PARAM_NOTHING+set}"`,
		},
	}

	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q output=%q)", res.Status, statusSucceeded, res.Err, res.Output)
	}
	// "set" 来自 ${VAR+set}：证明 null 参数**存在**且为空串，而不是没设。
	want := `hello world|dash-value|{"a":1,"b":["x"]}|3|true|set`
	if got := strings.TrimSpace(res.Output); got != want {
		t.Fatalf("params env:\n got %q\nwant %q", got, want)
	}
}

// TestRunJobEnvIdentity 对应 W3-2：任务身份变量与整份 params JSON；
// 同时断言 v1 **不导出** ANOTIFY_TRACE_ID（契约 §10 缺口 1）。
func TestRunJobEnvIdentity(t *testing.T) {
	j := job{
		TaskID:     "tsk_identity",
		Capability: "demo.prep",
		Version:    1,
		Params:     map[string]any{"title": "hello world", "n": float64(1)},
		Spec: &runSpec{
			Runner: "bash",
			Command: `printf '%s|%s|%s|%s' "$ANOTIFY_TASK_ID" "$ANOTIFY_CAPABILITY" ` +
				`"$ANOTIFY_PARAMS" "${ANOTIFY_TRACE_ID+set}"`,
		},
	}

	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q output=%q)", res.Status, statusSucceeded, res.Err, res.Output)
	}
	// 末尾的 "|" 后面什么都没有 = ANOTIFY_TRACE_ID 没设（assign 帧没有 trace_id）。
	want := "tsk_identity|demo.prep@1|" + `{"n":1,"title":"hello world"}` + "|"
	if got := strings.TrimSpace(res.Output); got != want {
		t.Fatalf("identity env:\n got %q\nwant %q", got, want)
	}
}

// TestRunJobSuccess 对应 W3-3：成功判定的全部字段（未声明 run.out → Out nil、Artifacts 非 nil）。
func TestRunJobSuccess(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob("echo first line; echo second line"))

	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Err != "" {
		t.Fatalf("Err = %q, want empty", res.Err)
	}
	if res.Out != nil {
		t.Fatalf("Out = %q, want nil（未声明 run.out 就不回报出口）", *res.Out)
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
	if !strings.Contains(res.Output, "second line") {
		t.Fatalf("Output = %q, want 含完整输出", res.Output)
	}
}

// TestRunJobSuccessEmptyOutput：没有输出的成功命令，summary 不能是空串。
func TestRunJobSuccessEmptyOutput(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob("true"))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Summary != "exit 0" {
		t.Fatalf("Summary = %q, want %q", res.Summary, "exit 0")
	}
}

// TestRunJobFailure 对应 W3-4：非 0 退出 → failed，Err 含 "exit status <code>"
// 与 stderr 首行。
func TestRunJobFailure(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob("echo boom >&2; exit 7"))

	if res.Status != statusFailed {
		t.Fatalf("status = %q, want %q", res.Status, statusFailed)
	}
	if !strings.Contains(res.Err, "exit status 7") {
		t.Fatalf("Err = %q, want 含 %q", res.Err, "exit status 7")
	}
	if !strings.Contains(res.Err, "boom") {
		t.Fatalf("Err = %q, want 含 stderr 首行 %q", res.Err, "boom")
	}
	if res.Out != nil {
		t.Fatalf("Out = %q, want nil", *res.Out)
	}
	if res.Artifacts == nil || len(res.Artifacts) != 0 {
		t.Fatalf("Artifacts = %v, want 非 nil 空列表", res.Artifacts)
	}
}

// TestRunJobOutSucceededDeclared 对应契约 §4 #1：成功 + 声明 run.out → 回报该出口名。
func TestRunJobOutSucceededDeclared(t *testing.T) {
	j := newRunnerJob("echo done")
	j.Spec.Out = "ok"

	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Out == nil {
		t.Fatal("Out = nil, want 指向 \"ok\"（成功且声明了 run.out）")
	}
	if *res.Out != "ok" {
		t.Fatalf("Out = %q, want %q", *res.Out, "ok")
	}
}

// TestRunJobOutSucceededUndeclared 对应契约 §4 #2：成功但没声明 run.out → Out 仍为
// nil（帧里 out 键缺席 = 分支终止）。
func TestRunJobOutSucceededUndeclared(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob("echo done"))

	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.Out != nil {
		t.Fatalf("Out = %q, want nil（没声明 run.out）", *res.Out)
	}
}

// TestRunJobOutFailedNotReported 对应契约 §4 #3：声明了 out 但命令失败 → 绝不回报
// 出口（不能把失败伪装成走通了某条路）。
func TestRunJobOutFailedNotReported(t *testing.T) {
	j := newRunnerJob("echo boom >&2; exit 1")
	j.Spec.Out = "ok"

	res := runJob(context.Background(), j)
	if res.Status != statusFailed {
		t.Fatalf("status = %q, want %q", res.Status, statusFailed)
	}
	if !strings.Contains(res.Err, "exit status 1") {
		t.Fatalf("Err = %q, want 含 %q", res.Err, "exit status 1")
	}
	if res.Out != nil {
		t.Fatalf("Out = %q, want nil（失败不回报出口）", *res.Out)
	}
}

// TestRunJobOutWhitespaceTreatedAsUndeclared 对应契约 §4 #4：out 去空白后为空 → 按
// 未声明处理（绝不把空格当出口名发出去）；非空时回报的是**去空白后**的值。
func TestRunJobOutWhitespaceTreatedAsUndeclared(t *testing.T) {
	for _, raw := range []string{"", "  ", "\t\n"} {
		j := newRunnerJob("true")
		j.Spec.Out = raw

		res := runJob(context.Background(), j)
		if res.Status != statusSucceeded {
			t.Fatalf("out=%q: status = %q, want %q (err=%q)", raw, res.Status, statusSucceeded, res.Err)
		}
		if res.Out != nil {
			t.Fatalf("out=%q: Out = %q, want nil（纯空白按未声明处理）", raw, *res.Out)
		}
	}

	// 前后空白被 trim：回报的是干净出口名。
	j := newRunnerJob("true")
	j.Spec.Out = "  ok  "
	res := runJob(context.Background(), j)
	if res.Out == nil {
		t.Fatal("Out = nil, want 去空白后的 \"ok\"")
	}
	if *res.Out != "ok" {
		t.Fatalf("Out = %q, want %q（去首尾空白后回报）", *res.Out, "ok")
	}
}

// TestRunJobTimeout 对应 W3-5：TimeoutSec=1，命令里有一个会活过超时的孙进程 →
//   - runJob 必须在 3s 内返回（真的杀了，没等 `sleep 5` 自己结束）；
//   - Err 含 "timed out"/"after"；
//   - 孙进程也必须死。命令特意把「写标记文件」放在**后台子 shell** 里：
//     `( sleep 2; echo survived > marker ) & sleep 5`。
//     只杀直接子进程（sh）的时候，后台子 shell 会活到 t≈2s 把文件写出来，
//     而且它还占着输出管道，Wait 要等到 t≈5s 才返回——两个信号都能抓住。
//     杀**整组**时两个进程在 t≈1s 一起死，标记文件永远不出现（因此这里断言的是
//     「杀进程组」，不是「sh 死了」）。
func TestRunJobTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived.txt")
	j := newRunnerJob(fmt.Sprintf("( sleep 2; echo survived > %q ) & sleep 5", marker))
	j.Spec.TimeoutSec = 1
	j.Spec.Out = "ok" // 加固（契约 §4 #5）：超时即使声明了 out 也绝不回报出口

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
	// 加固（契约 §4 #5）：超时不回报出口。
	if res.Out != nil {
		t.Fatalf("Out = %q, want nil（超时不回报出口）", *res.Out)
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

// TestRunJobCanceled：ctx 取消（cancel 帧/排空）→ failed + Err 含 "canceled"，
// 且整个进程组被杀（不需要等 `sleep 5` 自己结束，后台孙进程也不许活下来）。
func TestRunJobCanceled(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived.txt")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	j := newRunnerJob(fmt.Sprintf("( sleep 2; echo survived > %q ) & sleep 5", marker))
	j.Spec.Out = "ok" // 加固（契约 §4 #5）：取消即使声明了 out 也绝不回报出口

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
	// 加固（契约 §4 #5）：取消不回报出口。
	if res.Out != nil {
		t.Fatalf("Out = %q, want nil（取消不回报出口）", *res.Out)
	}

	// 取消时后台孙进程（t≈2s 写文件）也必须一起死。
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("取消后后台孙进程仍然活着并写了标记文件：进程组没被彻底杀掉")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunJobNoSpec：执行层自己也不能 panic 或把空命令当成功（client 侧正常会
// 提前挡掉，这里是防御性行为）。
func TestRunJobNoSpec(t *testing.T) {
	noSpec := runJob(context.Background(), job{TaskID: "tsk_x", Capability: "demo.prep", Version: 2})
	if noSpec.Status != statusFailed || !strings.Contains(noSpec.Err, "no run spec") {
		t.Fatalf("nil Spec → status=%q err=%q, want failed 且含 %q", noSpec.Status, noSpec.Err, "no run spec")
	}
	if noSpec.Artifacts == nil {
		t.Fatal("Artifacts = nil, want 非 nil")
	}

	emptyCmd := runJob(context.Background(), newRunnerJob("   "))
	if emptyCmd.Status != statusFailed || !strings.Contains(emptyCmd.Err, "run.command is empty") {
		t.Fatalf("空 command → status=%q err=%q, want failed 且含 %q", emptyCmd.Status, emptyCmd.Err, "run.command is empty")
	}
}

// TestRunJobOutputTruncated：合并捕获有上限，超出部分丢弃并留下提示。
func TestRunJobOutputTruncated(t *testing.T) {
	// 纯 POSIX sh 造 100000 字节（> maxCapturedOutput），不依赖 yes/head。
	const cmd = `s=; i=0; while [ $i -lt 1000 ]; do s="${s}x"; i=$((i+1)); done; ` +
		`i=0; while [ $i -lt 100 ]; do printf '%s' "$s"; i=$((i+1)); done`

	res := runJob(context.Background(), newRunnerJob(cmd))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if !strings.Contains(res.Output, "output truncated") {
		t.Fatalf("Output 没有截断提示（len=%d）", len(res.Output))
	}
	if len(res.Output) > maxCapturedOutput+200 {
		t.Fatalf("Output len = %d，超过上限 %d 太多", len(res.Output), maxCapturedOutput)
	}
	if len(res.Summary) != summaryMaxBytes {
		t.Fatalf("Summary len = %d, want %d（按字节截断）", len(res.Summary), summaryMaxBytes)
	}
}

// TestBuildEnv 对应 W3-6：base 里的 ANOTIFY_* 残留被剔除、run.env 不能覆盖
// ANOTIFY_*、输出按变量名排序。
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
	j := job{
		TaskID:     "tsk_build",
		Capability: "demo.prep",
		Version:    7,
		Params:     map[string]any{"title": "t", "n": float64(2)},
		Spec: &runSpec{
			Runner:  "bash",
			Command: "true",
			Env: map[string]string{
				"LOG_LEVEL": "info",
				"MODE":      "strict",
				// run.env 里的 ANOTIFY_* 一律忽略（契约变量优先级最高）。
				"ANOTIFY_TASK_ID":   "hijack",
				"ANOTIFY_PARAM_N":   "999",
				"ANOTIFY_UNDECLARE": "nope",
			},
		},
	}

	got, err := buildEnv(base, j)
	if err != nil {
		t.Fatalf("buildEnv: unexpected error: %v", err)
	}
	want := []string{
		"ANOTIFY_CAPABILITY=demo.prep@7",
		`ANOTIFY_PARAMS={"n":2,"title":"t"}`,
		"ANOTIFY_PARAM_N=2",
		"ANOTIFY_PARAM_TITLE=t",
		"ANOTIFY_TASK_ID=tsk_build",
		"HOME=/home/worker",
		"LOG_LEVEL=info",
		"MODE=strict",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"ZZZ=last",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildEnv:\n got %v\nwant %v", got, want)
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
	res := runJob(context.Background(), job{
		TaskID: "tsk_run", Capability: "demo.prep", Version: 1,
		Spec: &runSpec{
			Runner:  "bash",
			Command: `printf '%s' "${ANOTIFY_PARAM_STALE-unset}"`,
			Env:     map[string]string{"ANOTIFY_PARAM_STALE": "1"},
		},
	})
	if got := strings.TrimSpace(res.Output); got != "unset" {
		t.Fatalf("子进程看到 ANOTIFY_PARAM_STALE=%q, want 完全没设（unset）", got)
	}
}

// TestBuildEnvEmptyParams：params 为空 → ANOTIFY_PARAMS 是 {}（不是 null）。
func TestBuildEnvEmptyParams(t *testing.T) {
	got, err := buildEnv(nil, job{TaskID: "t", Capability: "c", Version: 1, Spec: &runSpec{Command: "true"}})
	if err != nil {
		t.Fatalf("buildEnv: unexpected error: %v", err)
	}
	want := []string{"ANOTIFY_CAPABILITY=c@1", "ANOTIFY_PARAMS={}", "ANOTIFY_TASK_ID=t"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildEnv:\n got %v\nwant %v", got, want)
	}

	// Spec 为 nil 也不能 panic（防御：client 侧正常不会走到这儿）。
	if _, err := buildEnv(nil, job{}); err != nil {
		t.Fatalf("buildEnv(nil spec): unexpected error: %v", err)
	}
}

// TestBuildEnvParamError：参数值无法序列化 → buildEnv 返回错误（而不是静默丢参数）。
func TestBuildEnvParamError(t *testing.T) {
	j := job{
		TaskID: "t", Capability: "c", Version: 1,
		Params: map[string]any{"bad": make(chan int)},
		Spec:   &runSpec{Command: "true"},
	}
	if _, err := buildEnv(nil, j); err == nil {
		t.Fatal("buildEnv: want error for 不可序列化的参数值, got nil")
	}
}

// TestEffectiveTimeout：node.yaml 的 timeoutSec 优先，中枢 max_duration_sec 兜底。
func TestEffectiveTimeout(t *testing.T) {
	cases := []struct {
		name string
		j    job
		want time.Duration
	}{
		{"spec 优先", job{Spec: &runSpec{TimeoutSec: 5}, MaxDuration: 9}, 5 * time.Second},
		{"兜底 max_duration", job{Spec: &runSpec{}, MaxDuration: 9}, 9 * time.Second},
		{"都没有", job{Spec: &runSpec{}}, 0},
		{"无 spec 但有 max_duration", job{MaxDuration: 3}, 3 * time.Second},
		{"无 spec 无 max_duration", job{}, 0},
	}
	for _, tc := range cases {
		if got := effectiveTimeout(tc.j); got != tc.want {
			t.Errorf("%s: effectiveTimeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}

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

// TestOneLine 对应 W3-7：取首个非空行、按字节截断且不破 UTF-8。
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
	res := runJob(context.Background(), newRunnerJob("printf 'alpha\\nbeta\\n'"))
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
