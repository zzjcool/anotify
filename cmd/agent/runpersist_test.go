package main

// 本文件覆盖「执行档案」这条腿（契约 §3）：节点把每次执行的**输入 + 完整输出**落盘到
// <data-dir>/runs/，事后用 `anotify-agent runs` / `show` 回看。
//
// 三个层次各有用例，缺一条就有一段没人钉的路：
//
//  1. **runner**：CaptureFull 时采到的是**完整**输出（不是上报用的 32 KiB 尾部窗口），
//     并且带上每个流的真实字节数、截断标记与退出码；
//  2. **档案库**：同一份 outcome 经 runRecordFor + runStore.Save 落到盘上之后，
//     stdout.log 是脚本的**逐字节**输出、meta.json 不带 truncated、input.json 里是
//     params 原文；
//  3. **真的 serveNode**（fake 连接）：档案在 result **之前**就写好了（契约 §3.2 的
//     顺序要求），而 <data-dir>/runs 被一个同名文件占着时任务**仍然是 succeeded**。
//
// 夹具刻意只依赖 bash 与最基本的 coreutils（与 runner_test.go 同一条纪律），
// 帮助函数统一 runPersist 前缀避免与同包其它用例撞名。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// runPersistLog 是一个安静的 logger：断言失败时要看的是断言，不是 slog 输出。
func runPersistLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// runPersistScript 造一段**确定性**的大输出脚本：
//
//	HEAD-MARKER(换行) <chunkBytes 字节的 z> × repeat (换行)TAIL-MARKER(换行)
//	stderr 一行 warn-line
//
// 头尾两个标记是给「保留头部的完整捕获」与「保留尾部的 32 KiB 上报窗口」做区分的：
// 完整档案两头都有，上报窗口只可能有尾。
//
// 用 bash 循环拼而不是 head/tr/yes：与 runner_test.go 的夹具同一条纪律（只依赖 bash
// 与最基本的 coreutils），而且回来逐字节对得上打印出来的字符串。
func runPersistScript(chunkBytes, repeat int) (body, wantStdout, wantStderr string) {
	chunk := strings.Repeat("z", chunkBytes)
	body = fmt.Sprintf(
		`printf 'HEAD-MARKER\n'; `+
			`chunk=; i=0; while [ $i -lt %d ]; do chunk="${chunk}z"; i=$((i+1)); done; `+
			`i=0; while [ $i -lt %d ]; do printf '%%s' "$chunk"; i=$((i+1)); done; `+
			`printf '\nTAIL-MARKER\n'; printf 'warn-line\n' >&2`,
		chunkBytes, repeat)
	wantStdout = "HEAD-MARKER\n" + strings.Repeat(chunk, repeat) + "\nTAIL-MARKER\n"
	wantStderr = "warn-line\n"
	return body, wantStdout, wantStderr
}

// runPersistReadMeta 读一个 run 目录里的 meta.json（档案的完整性标记）。
func runPersistReadMeta(t *testing.T, dir string) runMeta {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, runMetaFile))
	if err != nil {
		t.Fatalf("读 %s: %v", runMetaFile, err)
	}
	var meta runMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("解 %s: %v", runMetaFile, err)
	}
	return meta
}

// runPersistReadInput 读一个 run 目录里的 input.json。
func runPersistReadInput(t *testing.T, dir string) (runInput, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, runInputFile))
	if err != nil {
		t.Fatalf("读 %s: %v", runInputFile, err)
	}
	var in runInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("解 %s: %v", runInputFile, err)
	}
	return in, string(raw)
}

// ---------- 1) runner：完整捕获 vs 上报窗口 ----------

// TestRunPersistArchiveKeepsFullOutputBeyondUploadWindow 是一次执行的两条腿的对照：
//
//	上报窗口（Stdout）      = 最后 32 KiB（约定），被截断
//	落盘档案（ArchiveStdout）= **完整**输出（单流 8 MiB 以内），逐字节等于脚本真输出
//
// 这条用例就是「为什么要在节点上另存一份」的证据：HEAD-MARKER 在上报窗口里**根本不存在**，
// 排障时「前面那一段到底打印了什么」只有档案答得上来。
func TestRunPersistArchiveKeepsFullOutputBeyondUploadWindow(t *testing.T) {
	body, wantStdout, wantStderr := runPersistScript(1024, 100) // 102400 字节 > 32 KiB
	j := newRunnerJob(t, body)
	j.TaskID = "tsk_persist"
	j.TraceID = "trc_persist"
	j.CaptureFull = true
	j.Params = map[string]any{
		"title":  "hello-persist",
		"nested": map[string]any{"n": float64(3)},
	}

	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q stderr=%q)", res.Status, statusSucceeded, res.Err, res.Stderr)
	}

	// ---- 落盘那条腿：完整、不截断、字节数是真的 ----
	if res.ArchiveStdout != wantStdout {
		t.Fatalf("ArchiveStdout 不是完整输出：len=%d，want len=%d（差 %d 字节）",
			len(res.ArchiveStdout), len(wantStdout), len(res.ArchiveStdout)-len(wantStdout))
	}
	if len(res.ArchiveStdout) <= maxLogStreamBytes {
		t.Fatalf("ArchiveStdout = %d 字节，期望 > %d（默认上限就是上报窗口的大小，两条腿不能一样）",
			len(res.ArchiveStdout), maxLogStreamBytes)
	}
	if res.ArchiveStdoutBytes != int64(len(wantStdout)) {
		t.Fatalf("ArchiveStdoutBytes = %d, want %d（真实字节数，不是留下多少）",
			res.ArchiveStdoutBytes, len(wantStdout))
	}
	if res.ArchiveStdoutTruncated {
		t.Fatalf("ArchiveStdoutTruncated = true, want false（%d 字节远低于单流上限 %d）",
			len(wantStdout), archiveStreamMaxBytes)
	}
	if res.ArchiveStderr != wantStderr {
		t.Fatalf("ArchiveStderr = %q, want %q", res.ArchiveStderr, wantStderr)
	}
	if res.ArchiveStderrTruncated {
		t.Fatalf("ArchiveStderrTruncated = true, want false")
	}

	// ---- 上报那条腿：仍然是尾部 32 KiB 窗口 ----
	// 逐字形状见 runner_test.go 的 TestRunJobLogsTruncatedPerStream（口径没改）。
	windowMarker := fmt.Sprintf("\n…（日志已截断：共 %d 字节，只保留最后 %d 字节）",
		len(wantStdout), maxLogStreamBytes)
	if res.Stdout != wantStdout[len(wantStdout)-maxLogStreamBytes:]+windowMarker {
		t.Fatalf("上报窗口不是「最后 %d 字节 + 截断标记」：len=%d（报的口径不能变）",
			maxLogStreamBytes, len(res.Stdout))
	}
	if strings.Contains(res.Stdout, "HEAD-MARKER") {
		t.Fatalf("上报窗口里还有 HEAD-MARKER：夹具失效（窗口 = 最后 %d 字节）", maxLogStreamBytes)
	}
	if !strings.Contains(res.Stdout, "TAIL-MARKER") {
		t.Fatalf("上报窗口里没有 TAIL-MARKER（窗口必须保留尾部）")
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true（上报窗口确实被截断了，这个事实仍要传出去）")
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Summary != "HEAD-MARKER" {
		t.Fatalf("Summary = %q, want HEAD-MARKER（summary 口径不变）", res.Summary)
	}
}

// TestRunPersistSaveKeepsFullOutputAndInput 把上一条的 outcome 真的落盘：
// stdout.log 是**完整内容**（不是 32 KiB 窗口）、meta.json 不带 truncated、
// input.json 里是 params 原文。
func TestRunPersistSaveKeepsFullOutputAndInput(t *testing.T) {
	body, wantStdout, wantStderr := runPersistScript(1024, 100)
	j := newRunnerJob(t, body)
	j.TaskID = "tsk_persist"
	j.TraceID = "trc_persist"
	j.CaptureFull = true
	j.Params = map[string]any{
		"title":  "hello-persist",
		"nested": map[string]any{"n": float64(3)},
	}

	res := runJob(context.Background(), j)
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q stderr=%q)", res.Status, statusSucceeded, res.Err, res.Stderr)
	}

	// 独立口径：把同一段脚本直接交给 bash 跑一遍，拿「脚本的真实输出」。
	// 不拿夹具里算出来的字符串当唯一事实源——那样脚本与期望值会一起漂。
	direct := exec.Command("bash", "-c", body)
	var directOut bytes.Buffer
	direct.Stdout = &directOut
	if err := direct.Run(); err != nil {
		t.Fatalf("直接跑脚本取真实输出: %v", err)
	}
	if directOut.String() != wantStdout {
		t.Fatalf("夹具与脚本真输出不一致：len(bash)=%d len(夹具)=%d", directOut.Len(), len(wantStdout))
	}

	dataDir := t.TempDir()
	store := newRunStore(dataDir, runPersistLog())
	// 中枢派下来的那份快照：attempt=2（第二次派发）、seq_in_trace=3（同一 trace 同一能力的
	// 第 3 次执行）。节点不自己算这两个数，只原样记。
	f := &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     j.TaskID,
		Capability: "demo.prep",
		Version:    1,
		TraceID:    "trc_persist",
		Attempts:   2,
		SeqInTrace: 3,
		Params:     j.Params,
	}
	started := time.Now().Add(-2 * time.Second)
	finished := started.Add(1500 * time.Millisecond)
	dir, err := store.Save(runRecordFor(f, res, 4, started, finished))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	wantDir := filepath.Join(dataDir, runsSubdirName, "trc_persist", "demo.prep@1", "3-a2")
	if dir != wantDir {
		t.Fatalf("run 目录 = %q, want %q（<trace>/<capability>@<version>/<seq>-a<attempt>）", dir, wantDir)
	}

	// ---- stdout.log：**完整**内容（这条用例的靶心） ----
	got, err := os.ReadFile(filepath.Join(dir, runStdoutFile))
	if err != nil {
		t.Fatalf("读 %s: %v", runStdoutFile, err)
	}
	if len(got) <= maxLogStreamBytes {
		t.Fatalf("%s = %d 字节，期望 > %d（盘上必须是完整输出，不是 32 KiB 窗口）",
			runStdoutFile, len(got), maxLogStreamBytes)
	}
	if string(got) != directOut.String() {
		t.Fatalf("%s 与脚本真实输出不一致：len(盘上)=%d len(脚本)=%d",
			runStdoutFile, len(got), directOut.Len())
	}
	if gotStderr, err := os.ReadFile(filepath.Join(dir, runStderrFile)); err != nil {
		t.Fatalf("读 %s: %v", runStderrFile, err)
	} else if string(gotStderr) != wantStderr {
		t.Fatalf("%s = %q, want %q", runStderrFile, gotStderr, wantStderr)
	}

	// ---- meta.json：没被截断、退出码 0、emit 次数、身份字段、时间 ----
	meta := runPersistReadMeta(t, dir)
	if meta.StdoutTruncated {
		t.Fatalf("%s: stdoutTruncated = true, want false（%d 字节没有被截断）",
			runMetaFile, len(got))
	}
	if meta.StderrTruncated {
		t.Fatalf("%s: stderrTruncated = true, want false", runMetaFile)
	}
	if meta.StdoutBytes != int64(len(got)) {
		t.Fatalf("%s: stdoutBytes = %d, want %d", runMetaFile, meta.StdoutBytes, len(got))
	}
	if meta.StderrBytes != int64(len(wantStderr)) {
		t.Fatalf("%s: stderrBytes = %d, want %d", runMetaFile, meta.StderrBytes, len(wantStderr))
	}
	if meta.Status != statusSucceeded || meta.ExitCode != 0 {
		t.Fatalf("%s: status=%q exitCode=%d, want %q/0", runMetaFile, meta.Status, meta.ExitCode, statusSucceeded)
	}
	if meta.Emits != 4 {
		t.Fatalf("%s: emits = %d, want 4（调用方给的 emit 次数要原样落盘）", runMetaFile, meta.Emits)
	}
	if meta.TraceID != "trc_persist" || meta.Capability != "demo.prep" || meta.Version != 1 {
		t.Fatalf("%s: 身份字段 = %+v", runMetaFile, meta)
	}
	if meta.Attempt != 2 || meta.SeqInTrace != 3 {
		t.Fatalf("%s: attempt=%d seqInTrace=%d, want 2/3（来自 assign 快照）",
			runMetaFile, meta.Attempt, meta.SeqInTrace)
	}
	if meta.Summary != "HEAD-MARKER" || meta.Error != "" {
		t.Fatalf("%s: summary=%q error=%q", runMetaFile, meta.Summary, meta.Error)
	}
	if meta.StartedAt == "" || meta.FinishedAt == "" {
		t.Fatalf("%s: 时间戳空着（startedAt=%q finishedAt=%q）", runMetaFile, meta.StartedAt, meta.FinishedAt)
	}
	if meta.DurationMs != 1500 {
		t.Fatalf("%s: durationMs = %d, want 1500", runMetaFile, meta.DurationMs)
	}

	// ---- input.json：params **原文** ----
	in, rawInput := runPersistReadInput(t, dir)
	if !reflect.DeepEqual(in.Params, j.Params) {
		t.Fatalf("%s 的 params = %#v, want %#v（原样，不脱敏不裁剪）", runInputFile, in.Params, j.Params)
	}
	for _, want := range []string{`"title": "hello-persist"`, `"n": 3`, `"seqInTrace": 3`, `"attempt": 2`} {
		if !strings.Contains(rawInput, want) {
			t.Fatalf("%s 里没有 %s：\n%s", runInputFile, want, rawInput)
		}
	}
	if in.WorkDir != res.WorkDir {
		t.Fatalf("%s: workdir = %q, want %q", runInputFile, in.WorkDir, res.WorkDir)
	}
	if in.TraceID != "trc_persist" || in.TaskID != "tsk_persist" || in.Capability != "demo.prep" {
		t.Fatalf("%s: 身份字段 = %+v", runInputFile, in)
	}
}

// TestRunPersistCaptureOffByDefault：没开 CaptureFull（standalone / 旧调用方）时
// outcome 不带档案——档案是中枢派单路径的**额外**采集，不该偷偷改变其它路径的行为。
func TestRunPersistCaptureOffByDefault(t *testing.T) {
	res := runJob(context.Background(), newRunnerJob(t, "echo plain\n"))
	if res.Status != statusSucceeded {
		t.Fatalf("status = %q, want %q (err=%q)", res.Status, statusSucceeded, res.Err)
	}
	if res.ArchiveStdout != "" || res.ArchiveStderr != "" {
		t.Fatalf("ArchiveStdout/Stderr = %q/%q, want 空串（CaptureFull=false）", res.ArchiveStdout, res.ArchiveStderr)
	}
	if res.ArchiveStdoutBytes != 0 || res.ArchiveStderrBytes != 0 {
		t.Fatalf("档案字节数 = %d/%d, want 0/0", res.ArchiveStdoutBytes, res.ArchiveStderrBytes)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestRunPersistExitCode：退出码照实进 outcome（档案里「为什么失败」的一半答案）。
// 起不来 / 没有产物 = -1（拿不到退出码），非 0 退出 = 那个码。
func TestRunPersistExitCode(t *testing.T) {
	cases := []struct {
		name string
		job  func(t *testing.T) job
		want int
		stat string
	}{
		{
			name: "成功=0",
			job:  func(t *testing.T) job { return newRunnerJob(t, "exit 0\n") },
			want: 0,
			stat: statusSucceeded,
		},
		{
			name: "非0退出=原码",
			job:  func(t *testing.T) job { return newRunnerJob(t, "exit 7\n") },
			want: 7,
			stat: statusFailed,
		},
		{
			name: "没有产物=-1",
			job: func(t *testing.T) job {
				j := newRunnerJob(t, "exit 0\n")
				j.Artifact = nil
				return j
			},
			want: -1,
			stat: statusFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runJob(context.Background(), tc.job(t))
			if res.Status != tc.stat {
				t.Fatalf("status = %q, want %q (err=%q)", res.Status, tc.stat, res.Err)
			}
			if res.ExitCode != tc.want {
				t.Fatalf("ExitCode = %d, want %d", res.ExitCode, tc.want)
			}
		})
	}
}

// ---------- 2) 真的 serveNode：档案在 result 之前落盘 ----------

// TestRunPersistClientArchivesBeforeResult 走真的 serveNode（fake 连接）：assign → 跑 →
// 落盘 → result。断言 result 到达时，盘上的档案已经是**完整**输出，且 emit 次数、
// attempt/seqInTrace 都对。
//
// 用真 `anotify-agent emit` 二进制（buildAgentBinary）而不是 mock：emit 次数（meta.emits）
// 的口径是「本任务累计 emit 了多少次」，只有真的走一遍 socket 才有意义。
func TestRunPersistClientArchivesBeforeResult(t *testing.T) {
	bin := buildAgentBinary(t)
	old := executablePath
	executablePath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executablePath = old })

	dataDir := t.TempDir()
	h := w4StartRawAt(t, w4Config(), dataDir, 200*time.Millisecond)
	h.welcome(10)

	body, wantStdout, _ := runPersistScript(1024, 100)
	// 大输出之后再 emit 一次：一次执行里「输出」与「产出」两件事都要进档案。
	// emit 的回复（一行 JSON）重定向到 /dev/null：那只是 CLI 的输出，不是能力的输出；
	// 不重定向的话它会被算进「脚本打印了什么」，档案里就多出一行 Node 自己的话。
	art := w4Artifact("demo.persist", 1,
		body+"; \"$ANOTIFY_AGENT_BIN\" emit --data '{\"n\":1}' >/dev/null\n")
	h.deploy(art)

	params := map[string]any{"title": "hello-archive"}
	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_arch",
		Capability: "demo.persist",
		Version:    1,
		TraceID:    "trc_arch",
		Attempts:   2,
		SeqInTrace: 1,
		Params:     params,
		Ports:      []string{"echoed"},
		Token:      "tok_arch",
	})

	frames := h.fc.waitFor(t, 15*time.Second, "tsk_arch 的 result", w4Cond(frameTypeResult, "tsk_arch"))
	res, _, ok := w4Find(frames, frameTypeResult, "tsk_arch")
	if !ok {
		t.Fatalf("没收到 result: %s", w4Describe(frames))
	}
	if res["status"] != statusSucceeded {
		t.Fatalf("status = %v, want %s（error=%v summary=%v）", res["status"], statusSucceeded, res["error"], res["summary"])
	}

	// result 到了 = 档案必须已经在盘上（契约 §3.2 的顺序要求）。
	dir := filepath.Join(dataDir, runsSubdirName, "trc_arch", "demo.persist@1", "1-a2")
	got, err := os.ReadFile(filepath.Join(dir, runStdoutFile))
	if err != nil {
		t.Fatalf("result 到了但档案还没落盘（读 %s: %v）", filepath.Join(dir, runStdoutFile), err)
	}
	if string(got) != wantStdout {
		t.Fatalf("盘上 stdout 不是完整输出：len=%d，want %d（HEAD-MARKER 在不在：%v）",
			len(got), len(wantStdout), strings.Contains(string(got), "HEAD-MARKER"))
	}
	if len(got) <= maxLogStreamBytes {
		t.Fatalf("盘上 stdout = %d 字节，期望 > %d（完整内容，不是上报窗口）", len(got), maxLogStreamBytes)
	}

	meta := runPersistReadMeta(t, dir)
	if meta.StdoutTruncated {
		t.Fatalf("meta.json: stdoutTruncated = true, want false")
	}
	if meta.Emits != 1 {
		t.Fatalf("meta.json: emits = %d, want 1（脚本 emit 了一次，且计数必须在 Close 之前取到）", meta.Emits)
	}
	if meta.Attempt != 2 || meta.SeqInTrace != 1 {
		t.Fatalf("meta.json: attempt=%d seqInTrace=%d, want 2/1（来自 assign 帧）",
			meta.Attempt, meta.SeqInTrace)
	}
	if meta.ExitCode != 0 || meta.Status != statusSucceeded {
		t.Fatalf("meta.json: status=%q exitCode=%d", meta.Status, meta.ExitCode)
	}

	in, rawInput := runPersistReadInput(t, dir)
	if !reflect.DeepEqual(in.Params, params) {
		t.Fatalf("input.json: params = %#v, want %#v", in.Params, params)
	}
	if !strings.Contains(rawInput, `"hello-archive"`) {
		t.Fatalf("input.json 里没有 params 原文：\n%s", rawInput)
	}

	h.stop()
}

// TestRunPersistBrokenRunsDirKeepsTaskSucceeded：把 <data-dir>/runs 建成一个**同名普通
// 文件**（建目录必然失败）→ 落盘报错，但任务仍然 succeeded、进程不 panic。
//
// 这是「本地档案永远不参与任务成败」的证据：磁盘满、权限不对、runs 被占着，
// 都不该让一个跑成功的任务变成 failed（索引写失败同理，只 WARN）。
func TestRunPersistBrokenRunsDirKeepsTaskSucceeded(t *testing.T) {
	dataDir := t.TempDir()
	runsPath := filepath.Join(dataDir, runsSubdirName)
	if err := os.WriteFile(runsPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("铺坏 runs: %v", err)
	}

	h := w4StartRawAt(t, w4Config(), dataDir, 200*time.Millisecond)
	h.welcome(10)
	h.deploy(w4Artifact("demo.broken", 1, "echo broken-runs-ok\n"))

	h.fc.feed(t, &assignFrame{
		Type:       frameTypeAssign,
		TaskID:     "tsk_broken",
		Capability: "demo.broken",
		Version:    1,
		TraceID:    "trc_broken",
		Attempts:   1,
		SeqInTrace: 1,
		Params:     map[string]any{"title": "still-fine"},
	})

	frames := h.fc.waitFor(t, 10*time.Second, "tsk_broken 的 result", w4Cond(frameTypeResult, "tsk_broken"))
	res, _, ok := w4Find(frames, frameTypeResult, "tsk_broken")
	if !ok {
		t.Fatalf("没收到 result: %s", w4Describe(frames))
	}
	if res["status"] != statusSucceeded {
		t.Fatalf("status = %v, want %s（落盘失败不得改变任务结论；error=%v）",
			res["status"], statusSucceeded, res["error"])
	}
	if res["summary"] != "broken-runs-ok" {
		t.Fatalf("summary = %v, want broken-runs-ok", res["summary"])
	}

	// 我们铺的那个文件必须原样还在（没有谁偷偷把它删掉/改名去"重试一次"）。
	st, err := os.Stat(runsPath)
	if err != nil {
		t.Fatalf("stat %s: %v", runsPath, err)
	}
	if !st.Mode().IsRegular() {
		t.Fatalf("%s 不再是普通文件（mode=%v）——落盘路径不该改动它", runsPath, st.Mode())
	}

	h.stop()
}

// TestRunPersistStoreRejectsFileAsRunsDir 是上一条的**单元层**对照：runs 被同名文件
// 占着时 Save 直接返回错误（调用方 Warn 了事），而不是 panic 或半途而废。
func TestRunPersistStoreRejectsFileAsRunsDir(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, runsSubdirName), []byte("x"), 0o600); err != nil {
		t.Fatalf("铺坏 runs: %v", err)
	}
	store := newRunStore(dataDir, runPersistLog())
	dir, err := store.Save(runRecord{
		Input: runInput{TraceID: "trc_bad", TaskID: "tsk_bad", Capability: "demo.prep", Version: 1, Attempt: 1, SeqInTrace: 1},
		Meta:  runMeta{Status: statusSucceeded},
	})
	if err == nil {
		t.Fatalf("Save 在 runs 是文件时应当返回错误，实际 dir=%q err=nil", dir)
	}
	if dir != "" {
		t.Fatalf("失败的 Save 不该给出目录：%q", dir)
	}
}
