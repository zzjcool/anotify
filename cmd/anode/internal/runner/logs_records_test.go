package runner

// L1-R7 的真实双 pipe 采集：记录来自 runner 的 os.Pipe（不是 mock），raw 逐字节
// 不变，序号跨 stdout/stderr 连续单调，at 是节点真实采集时间而不是 finishedAt。
// 旁路索引故障只让档案不可结构化读取，不截 raw、不改变进程退出事实。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

func runLogsScript(t *testing.T, h *harness, script string) *protocol.TaskResultFrame {
	t.Helper()
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = script
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	return h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1" && r.Attempt == 1
	}, 5*time.Second).(*protocol.TaskResultFrame)
}

func shaOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestLogRecordsFromRealPipesMatchRawSHA 覆盖：真实双 pipe、raw SHA 逐字节一致、
// 两流共享序号连续单调、无效 UTF-8 只做 JSON 文字投影、at 是真实采集时间。
func TestLogRecordsFromRealPipesMatchRawSHA(t *testing.T) {
	h := newHarness(t)
	script := "#!/bin/sh\n" +
		"printf 'out-1\\n'\n" +
		"printf 'err-1\\n' >&2\n" +
		"printf 'out-2\\n'\n" +
		"printf 'err-2\\n' >&2\n" +
		"printf 'bad-\\377\\n'\n"
	before := time.Now().Add(-time.Minute)
	res := runLogsScript(t, h, script)
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("脚本应成功：%+v", res)
	}
	after := time.Now().Add(time.Minute)

	dir := filepath.Join(h.results.Root, hexNameForTest("t1"), "1", "logs")
	wantOut := append([]byte("out-1\nout-2\nbad-"), 0xff)
	wantOut = append(wantOut, '\n')
	wantErr := []byte("err-1\nerr-2\n")
	if got := shaOf(t, filepath.Join(dir, resultstore.StdoutFileName)); got != shaOfBytes(wantOut) {
		t.Fatalf("raw stdout sha 不符：%s", got)
	}
	if got := shaOf(t, filepath.Join(dir, resultstore.StderrFileName)); got != shaOfBytes(wantErr) {
		t.Fatalf("raw stderr sha 不符：%s", got)
	}
	rawOut, err := os.ReadFile(filepath.Join(dir, resultstore.StdoutFileName))
	if err != nil || !bytes.Equal(rawOut, wantOut) {
		t.Fatalf("raw stdout 必须逐字节保留（含 0xff）：% x err=%v", rawOut, err)
	}
	rawErr, err := os.ReadFile(filepath.Join(dir, resultstore.StderrFileName))
	if err != nil || !bytes.Equal(rawErr, wantErr) {
		t.Fatalf("raw stderr 必须逐字节保留：% q err=%v", rawErr, err)
	}

	arch, found, err := h.results.ReadLogs("t1", 1, resultstore.LogQuery{})
	if err != nil || !found {
		t.Fatalf("ReadLogs: found=%v err=%v", found, err)
	}
	if arch.Partial || arch.Omitted || arch.StdoutMore || arch.StderrMore {
		t.Fatalf("完整档案不该带截断标记：%+v", arch)
	}
	if len(arch.Lines) != 5 {
		t.Fatalf("两个 pipe 共 5 行 → 5 条记录，实际 %d：%+v", len(arch.Lines), arch.Lines)
	}
	finishedAt := res.FinishedAt.Format(time.RFC3339Nano)
	seen := map[string]string{}
	for i, line := range arch.Lines {
		if line.Sequence != i+1 {
			t.Fatalf("序号必须跨两流连续单调：第 %d 条是 %d", i, line.Sequence)
		}
		at, err := time.Parse(time.RFC3339Nano, line.At)
		if err != nil {
			t.Fatalf("at 不是 RFC3339Nano：%q", line.At)
		}
		if at.Before(before) || at.After(after) {
			t.Fatalf("at 必须是真实采集时间（%v..%v），实际 %v", before, after, at)
		}
		if line.At == finishedAt {
			t.Fatalf("at 不能回填成 finishedAt：%q", line.At)
		}
		seen[line.Stream+"|"+line.Text] = line.At
	}
	for _, want := range []string{"stdout|out-1", "stdout|out-2", "stderr|err-1", "stderr|err-2"} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("缺少真实记录 %q：%+v", want, arch.Lines)
		}
	}
	if _, ok := seen["stdout|bad-\uFFFD"]; !ok {
		t.Fatalf("无效 UTF-8 必须按标准 JSON 投影成替换字符：%+v", arch.Lines)
	}
	if _, err := time.Parse(time.RFC3339Nano, res.FinishedAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("finishedAt 形状意外：%v", err)
	}
}

func shaOfBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestLogsSidecarFailureKeepsRawAndExit 覆盖：旁路索引写失败（路径被占成目录）
// 时 raw 完整、进程退出事实不变，档案明确 logs_unreadable，不伪空。
func TestLogsSidecarFailureKeepsRawAndExit(t *testing.T) {
	h := newHarness(t)
	// 预先占住索引路径：Begin/OpenLogs 都能成功，OpenLogIndex 必然失败。
	indexPath := filepath.Join(h.results.Root, hexNameForTest("t1"), "1", "logs", resultstore.LogIndexFileName)
	if err := os.MkdirAll(indexPath, 0o700); err != nil {
		t.Fatal(err)
	}
	res := runLogsScript(t, h, "#!/bin/sh\nprintf 'raw-survives\\n'\n")
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("旁路索引故障不能改变进程退出事实：%+v", res)
	}
	dir := filepath.Join(h.results.Root, hexNameForTest("t1"), "1", "logs")
	rawOut, err := os.ReadFile(filepath.Join(dir, resultstore.StdoutFileName))
	if err != nil || string(rawOut) != "raw-survives\n" {
		t.Fatalf("索引故障不能截断 raw：%q err=%v", rawOut, err)
	}
	if _, err := os.Stat(filepath.Join(dir, resultstore.LogIndexErrorFileName)); err != nil {
		t.Fatalf("必须留下可检查的 index.error：%v", err)
	}
	arch, found, err := h.results.ReadLogs("t1", 1, resultstore.LogQuery{})
	if !found || !errors.Is(err, resultstore.ErrLogUnreadable) {
		t.Fatalf("坏索引必须 logs_unreadable：found=%v err=%v arch=%+v", found, err, arch)
	}
	if strings.TrimSpace(res.Result.Summary) == "" {
		t.Fatal("结果事实必须仍然完整上报")
	}
}
