package main

// 本文件覆盖 **standalone 落档 + 超时 124**（issue #320 §3/§4）。
//
// 钉住的验收点：
//
//  1. `run --data-dir X ./cap.yaml` 之后 `runs --data-dir X` 看得到它，
//     `show` 能读到 stdout；trace 用本地命名空间 `local_<ts>`，`<cap>@<ver>/1-a1` 可寻址；
//  2. 不带 `--data-dir` 时不落档，且 stderr **明说一行**（不能静默丢）；
//  3. `--timeout 1` + `sleep 5` → 退 124，stderr 里有指向 runs/show 的一行；
//     产物自己 `exit 124` 不被改写成「agent 超时」（两条用例分开，见 main_test.go 的
//     TestRunStandaloneOwnExit124）。
//
// 这里只依赖真实路径（跑真的进程、写真的 ATOMIC 档案），不 mock 文件系统：
// 落档的形状就是排障时看到的那份，mock 掉反而验不出「runs 能不能读到」。

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archiveFindRun 从 `runs --json` 的结果里取唯一一条（本地 trace）。
func archiveFindRun(t *testing.T, dataDir string) runJSONEntry {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := runRuns([]string{"--data-dir", dataDir, "--json"}, &stdout, &stderr)
	if err != nil || code != exitOK {
		t.Fatalf("runs = (%d, %v)（stderr=%s, stdout=%s）", code, err, stderr.String(), stdout.String())
	}
	var got runsJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("解析 runs JSON: %v（stdout=%q）", err, stdout.String())
	}
	if len(got.Runs) != 1 {
		t.Fatalf("runs 列出 %d 条，want 1：%+v", len(got.Runs), got.Runs)
	}
	return got.Runs[0]
}

// TestRunStandaloneArchivesToRuns 是验收第 4 条的主用例：
// run --data-dir 之后 runs 能看到、show 能读到 stdout（输入参数也在）。
func TestRunStandaloneArchivesToRuns(t *testing.T) {
	dataDir := t.TempDir()
	path := writeCapabilityFile(t, "demo.archive", 3,
		"printf 'hello-archive\\n'\nprintf 'warn-archive\\n' >&2\n")

	var stdout, stderr bytes.Buffer
	code, err := runStandalone([]string{
		path, "--data-dir", dataDir, "-p", "name=world", "-p", "n=42",
	}, &stdout, &stderr)
	if err != nil || code != exitOK {
		t.Fatalf("run = (%d, %v)（stderr=%s）", code, err, stderr.String())
	}
	// 终端输出照旧属于产物自己（落档不改这一条）。
	if !strings.Contains(stdout.String(), "hello-archive") {
		t.Errorf("stdout 里没有脚本输出：%q", stdout.String())
	}

	entry := archiveFindRun(t, dataDir)
	if !strings.HasPrefix(entry.TraceID, localTracePrefix) {
		t.Errorf("traceId = %q，want %q 前缀（本地命名空间）", entry.TraceID, localTracePrefix)
	}
	if entry.Capability != "demo.archive" || entry.Version != 3 {
		t.Errorf("档案身份 = %s@%d，want demo.archive@3", entry.Capability, entry.Version)
	}
	if entry.SeqInTrace != 1 || entry.Attempt != 1 {
		t.Errorf("seq/attempt = %d/%d，want 1/1（standalone 一次执行）", entry.SeqInTrace, entry.Attempt)
	}
	if entry.Status != statusSucceeded || entry.ExitCode != 0 {
		t.Errorf("status/exitCode = %s/%d，want succeeded/0", entry.Status, entry.ExitCode)
	}
	// ref 是三段：<trace>/<capability>@<version>/<seq>-a<attempt>（show 直接可用）。
	if strings.Count(entry.Ref, "/") != 2 {
		t.Errorf("ref = %q，want 三段", entry.Ref)
	}

	// show 能读到 stdout/stderr（含截断标记字段；这里的输出没超限，所以都是 false）。
	var showOut, showErr bytes.Buffer
	code, err = runShow([]string{entry.Ref, "--data-dir", dataDir, "--json"}, &showOut, &showErr)
	if err != nil || code != exitOK {
		t.Fatalf("show = (%d, %v)（stderr=%s）", code, err, showErr.String())
	}
	var detail showJSON
	if uerr := json.Unmarshal(showOut.Bytes(), &detail); uerr != nil {
		t.Fatalf("解析 show JSON: %v（stdout=%q）", uerr, showOut.String())
	}
	if detail.Stdout != "hello-archive\n" {
		t.Errorf("show.stdout = %q，want %q", detail.Stdout, "hello-archive\n")
	}
	if detail.Stderr != "warn-archive\n" {
		t.Errorf("show.stderr = %q，want %q", detail.Stderr, "warn-archive\n")
	}
	if detail.Meta.StdoutTruncated || detail.Meta.StderrTruncated {
		t.Error("输出没有超过 8 MiB，不该有截断标记")
	}
	if detail.Meta.StdoutBytes != int64(len("hello-archive\n")) {
		t.Errorf("meta.stdoutBytes = %d，want %d", detail.Meta.StdoutBytes, len("hello-archive\n"))
	}
	// params 原样落盘（不脱敏、不裁剪）：复核「当时到底传了什么」。
	if got := detail.Input.Params["name"]; got != "world" {
		t.Errorf("input.params.name = %v，want world", got)
	}
	if got := detail.Input.Params["n"]; got != "42" {
		t.Errorf("input.params.n = %v，want \"42\"（-p 值一律当字符串）", got)
	}
	if detail.Input.WorkDir == "" {
		t.Error("input.workdir 是空的：档案里要留下这次执行的现场路径")
	}
}

// TestRunStandaloneNoDataDirDoesNotArchive：不给 --data-dir 时不落档，stderr 必须
// 明说一行（不能静默丢——「最需要回看的那一次恰恰不在档案里」正是这个 issue 的起点）。
func TestRunStandaloneNoDataDirDoesNotArchive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := writeCapabilityFile(t, "demo.noarchive", 1, "echo hi\n")
	var stdout, stderr bytes.Buffer
	code, err := runStandalone([]string{path}, &stdout, &stderr)
	if err != nil || code != exitOK {
		t.Fatalf("run = (%d, %v)（stderr=%s）", code, err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "未指定 --data-dir") ||
		!strings.Contains(stderr.String(), "不落档") {
		t.Errorf("stderr 必须明说「未指定 --data-dir，本次不落档」，实际 %q", stderr.String())
	}
	// 默认数据目录（~/.anotify）下不该凭空出现 runs/。
	if _, err := os.Stat(filepath.Join(home, ".anotify", runsSubdirName)); !os.IsNotExist(err) {
		t.Errorf("没给 --data-dir 却写了 %s（err=%v）", filepath.Join(home, ".anotify", runsSubdirName), err)
	}
}

// TestRunStandaloneTimeoutArchivesWithRef：超时退 124，并且提示里的 ref **真的**
// 指向一份已落盘的档案（runs 能看到这次失败，error 是 timed out）。
func TestRunStandaloneTimeoutArchivesWithRef(t *testing.T) {
	dataDir := t.TempDir()
	path := writeCapabilityFile(t, "demo.slow", 1, "sleep 5\n")

	var stdout, stderr bytes.Buffer
	code, err := runStandalone([]string{path, "--data-dir", dataDir, "--timeout", "1"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run 返回 error %v（超时是结果，不是 agent 故障）", err)
	}
	if code != exitTimeout {
		t.Fatalf("退出码 = %d，want %d", code, exitTimeout)
	}
	if !strings.Contains(stderr.String(), "anotify-agent show ") ||
		!strings.Contains(stderr.String(), "runs --data-dir") {
		t.Errorf("超时提示必须给出 runs/show 两条命令，实际 %q", stderr.String())
	}

	entry := archiveFindRun(t, dataDir)
	if entry.Status != statusFailed {
		t.Errorf("档案 status = %q，want failed", entry.Status)
	}
	if !strings.Contains(entry.Error, "timed out") {
		t.Errorf("档案 error = %q，want 含 timed out", entry.Error)
	}
	if entry.ExitCode != -1 {
		t.Errorf("档案 exitCode = %d，want -1（拿不到退出码：超时被杀）", entry.ExitCode)
	}
	// stderr 里给出的 ref 必须就是盘上这一条（不是编出来的路径）。
	if !strings.Contains(stderr.String(), entry.Ref) {
		t.Errorf("超时提示里的 ref 与档案不一致：stderr=%q ref=%q", stderr.String(), entry.Ref)
	}
}

// TestRunStandaloneArchiveTruncatesHugeStream：standalone 落档与节点模式共用同一个
// 8 MiB/流的头部窗口——超大输出要截断、要留标记、meta 要记**真实**字节数。
//
// 这条用例存在的理由：CaptureFull 以前只在中枢派单那条路上被打开过，standalone
// 这条路（runLocal）的采集是新接的；不钉一次「真的大到要截」，接线接漏了也看不出来。
func TestRunStandaloneArchiveTruncatesHugeStream(t *testing.T) {
	dataDir := t.TempDir()
	path := writeCapabilityFile(t, "demo.flood", 1,
		"head -c 9000000 /dev/zero | tr '\\0' x\n")

	var stderr bytes.Buffer
	// stdout 丢掉：这里要验的是档案，不是终端输出（9 MiB 没必要留在内存里看）。
	code, err := runStandalone([]string{path, "--data-dir", dataDir}, io.Discard, &stderr)
	if err != nil || code != exitOK {
		t.Fatalf("run = (%d, %v)（stderr=%s）", code, err, stderr.String())
	}

	entry := archiveFindRun(t, dataDir)
	if !entry.StdoutTruncated {
		t.Error("meta.stdoutTruncated = false，want true（9 MB > 8 MiB 单流上限）")
	}
	if entry.StdoutBytes != 9000000 {
		t.Errorf("meta.stdoutBytes = %d，want 9000000（真实字节数，截断前）", entry.StdoutBytes)
	}

	var showOut, showErr bytes.Buffer
	if code, err := runShow([]string{entry.Ref, "--data-dir", dataDir, "--json"}, &showOut, &showErr); code != exitOK || err != nil {
		t.Fatalf("show = (%d, %v)（stderr=%s）", code, err, showErr.String())
	}
	var detail showJSON
	if uerr := json.Unmarshal(showOut.Bytes(), &detail); uerr != nil {
		t.Fatalf("解析 show JSON: %v", uerr)
	}
	if !strings.Contains(detail.Stdout, "输出已截断") {
		t.Error("show.stdout 里没有截断标记：读的人会把「只剩前 8 MiB」当成「脚本就输出这么多」")
	}
	if len(detail.Stdout) > 8<<20+1024 {
		t.Errorf("落盘内容 = %d 字节，want 前 8 MiB + 一行标记", len(detail.Stdout))
	}
}
