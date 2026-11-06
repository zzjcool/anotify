package main

// 本文件覆盖「中枢按需取日志」的节点侧（CONTRACT-logs-on-node §1 / §4）：
//
//  1. 命中：真档案（真 runStore.Save）→ logs_request → 回包里的两个流、bytes、配对字段；
//  2. 未命中：没有档案 → error=no_local_archive（不是空 error + 空串）；
//  3. 1 MiB 截断：两路 >2 MiB → 编码后 ≤ LogsResponseMaxBytes、truncated、只留尾部；
//  4. 索引兜底与 GC 清索引：索引没了/脏了仍能取到，GC 删目录时顺手删索引；
//  5. 端到端（契约 §7 最要紧的那条）：真 WS + 真任务，中枢发一帧 logs_request 就要得到
//     本次执行的 stdout——这就是「停掉上传后中枢仍能拿到日志」的线上证据。
//
// 夹具复用 client_test.go 的 w4Conn / w4Hub（真连接那套），帮助函数统一 logsServe 前缀。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/anotify/internal/ws"
)

// logsServeTestLog 是安静的 logger（回包路径自己会 Warn，别把断言输出搞脏）。
func logsServeTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logsServeTestAgent 造一个只用来回答 logs_request 的 agent：
// fake 连接（能拿到它发出去的帧）+ 真档案库（dataDir 下的 runs/）。
func logsServeTestAgent(t *testing.T, dataDir string) (*nodeAgent, *w4Conn) {
	t.Helper()
	conn := newW4Conn()
	a := newNodeAgent(context.Background(), w4Config(), conn, dataDir, logsServeTestLog())
	t.Cleanup(a.cancel)
	return a, conn
}

// logsServeFindResponse 返回 fake 连接上收到的唯一一帧 logs_response（解成帧结构体）。
//
// 用 ws.DecodeUpstream 而不是自己 Unmarshal：tag 属于 internal/ws，漂了就要在这里红
// （回包是**出线**的东西，测试不该有第二份解码规则）。
func logsServeFindResponse(t *testing.T, conn *w4Conn, taskID string) *ws.LogsResponseFrame {
	t.Helper()
	frames := conn.waitFor(t, 5*time.Second, "logs_response", w4Cond(frameTypeLogsResponse, taskID))
	resp, _, ok := w4Find(frames, frameTypeLogsResponse, taskID)
	if !ok {
		t.Fatalf("没有 logs_response: %s", w4Describe(frames))
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("重新编码回包: %v", err)
	}
	got, err := ws.DecodeUpstream(raw)
	if err != nil {
		t.Fatalf("ws.DecodeUpstream(logs_response): %v", err)
	}
	f, ok := got.(*ws.LogsResponseFrame)
	if !ok {
		t.Fatalf("ws.DecodeUpstream 解出 %T, want *ws.LogsResponseFrame（tag 漂了？）", got)
	}
	return f
}

// logsServeBigStream 造一路 >2 MiB 的输出：第一行与最后一行都是标记，中间是填充。
//
// 头尾两个标记用来钉住「回包是**文件的结尾**」：尾部窗口里必须有 LAST，不能有 FIRST。
func logsServeBigStream(first, last, fill string, lines int) string {
	chunk := strings.Repeat(fill, 1024)
	var b strings.Builder
	b.WriteString(first)
	b.WriteString("\n")
	for i := 0; i < lines; i++ {
		b.WriteString(chunk)
		b.WriteString("\n")
	}
	b.WriteString(last)
	b.WriteString("\n")
	return b.String()
}

// ---------- 1) 命中 ----------

// TestLogsServeHitReturnsArchiveTail 是一条完整的好路径：真档案 → logs_request → 回包。
func TestLogsServeHitReturnsArchiveTail(t *testing.T) {
	dataDir := t.TempDir()
	store := newRunStore(dataDir, logsServeTestLog())

	rec := runStoreTestRecord("trc_logs", "review", 1, 2, 1, "第一行\n第二行\n")
	rec.Stderr = "一句警告\n"
	dir, err := store.Save(rec)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Save 必须顺手写下任务索引（契约 §4）：一行相对路径，相对 runs/。
	idxPath := filepath.Join(dataDir, runsSubdirName, runIndexDirName, rec.Input.TaskID)
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("Save 之后没有任务索引 %s: %v", idxPath, err)
	}
	wantRef := "trc_logs/review@1/2-a1"
	if got := string(raw); got != wantRef+"\n" {
		t.Fatalf("索引内容 = %q, want %q（一行相对路径 + 换行）", got, wantRef+"\n")
	}
	if !strings.HasSuffix(dir, filepath.FromSlash(wantRef)) {
		t.Fatalf("档案目录 %q 与索引 %q 对不上", dir, wantRef)
	}

	a, conn := logsServeTestAgent(t, dataDir)
	a.handleLogsRequest(&logsRequestFrame{
		Type:      frameTypeLogsRequest,
		TaskID:    rec.Input.TaskID,
		RequestID: "lreq_hit",
	})

	resp := logsServeFindResponse(t, conn, rec.Input.TaskID)
	if resp.RequestID != "lreq_hit" {
		t.Errorf("request_id = %q, want lreq_hit（中枢按它配对，改了就是超时）", resp.RequestID)
	}
	if resp.TaskID != rec.Input.TaskID {
		t.Errorf("task_id = %q, want %q（对不上会被中枢丢弃）", resp.TaskID, rec.Input.TaskID)
	}
	if resp.Type != frameTypeLogsResponse {
		t.Errorf("type = %q, want %q", resp.Type, frameTypeLogsResponse)
	}
	if resp.Stdout != rec.Stdout {
		t.Errorf("stdout = %q, want %q", resp.Stdout, rec.Stdout)
	}
	if resp.Stderr != rec.Stderr {
		t.Errorf("stderr = %q, want %q", resp.Stderr, rec.Stderr)
	}
	if resp.Truncated {
		t.Errorf("truncated = true, want false（两百来字节远低于窗口）")
	}
	if want := int64(len(rec.Stdout) + len(rec.Stderr)); resp.Bytes != want {
		t.Errorf("bytes = %d, want %d（档案两个流的真实字节数之和）", resp.Bytes, want)
	}
	if resp.Error != "" {
		t.Errorf("error = %q, want 空（这次确实取到了）", resp.Error)
	}
}

// ---------- 2) 未命中 ----------

// TestLogsServeMissReturnsNoLocalArchive 没有档案时必须回 no_local_archive：
// 空 error + 两个空串的意思是「这次执行什么都没打印」，两件事不能混（契约 §1）。
func TestLogsServeMissReturnsNoLocalArchive(t *testing.T) {
	t.Run("档案库里没有这条任务", func(t *testing.T) {
		a, conn := logsServeTestAgent(t, t.TempDir())
		a.handleLogsRequest(&logsRequestFrame{
			Type: frameTypeLogsRequest, TaskID: "tsk_nope", RequestID: "lreq_miss",
		})
		resp := logsServeFindResponse(t, conn, "tsk_nope")
		if resp.Error != ws.LogsErrNoLocalArchive {
			t.Fatalf("error = %q, want %q", resp.Error, ws.LogsErrNoLocalArchive)
		}
		if resp.Stdout != "" || resp.Stderr != "" || resp.Bytes != 0 {
			t.Fatalf("没取到时回包必须是空的：stdout=%q stderr=%q bytes=%d",
				resp.Stdout, resp.Stderr, resp.Bytes)
		}
		if resp.RequestID != "lreq_miss" {
			t.Errorf("request_id = %q, want lreq_miss（失败回包也要能配对）", resp.RequestID)
		}
	})

	t.Run("没有数据目录", func(t *testing.T) {
		// runs 为 nil = 这台节点根本不落档案（dataDir 为空的老构造路径）。
		a := &nodeAgent{base: context.Background(), log: logsServeTestLog(), conn: newW4Conn()}
		req := &logsRequestFrame{Type: frameTypeLogsRequest, TaskID: "tsk_x", RequestID: "lreq_x"}
		resp := a.logsResponseFor(req)
		if resp.Error != ws.LogsErrNoLocalArchive {
			t.Fatalf("error = %q, want %q", resp.Error, ws.LogsErrNoLocalArchive)
		}
		if resp.Bytes != 0 {
			t.Fatalf("bytes = %d, want 0", resp.Bytes)
		}
	})

	t.Run("索引命中但目录里缺流文件", func(t *testing.T) {
		dataDir := t.TempDir()
		store := newRunStore(dataDir, logsServeTestLog())
		rec := runStoreTestRecord("trc_half", "halfcap", 1, 1, 1, "x\n")
		dir, err := store.Save(rec)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		// 只删一个流：剩下的是半份档案，不能当「这次什么都没打印」回出去。
		if err := os.Remove(filepath.Join(dir, runStderrFile)); err != nil {
			t.Fatalf("删 stderr.log: %v", err)
		}
		a, conn := logsServeTestAgent(t, dataDir)
		a.handleLogsRequest(&logsRequestFrame{
			Type: frameTypeLogsRequest, TaskID: rec.Input.TaskID, RequestID: "lreq_half",
		})
		resp := logsServeFindResponse(t, conn, rec.Input.TaskID)
		if resp.Error != ws.LogsErrNoLocalArchive {
			t.Fatalf("error = %q, want %q", resp.Error, ws.LogsErrNoLocalArchive)
		}
		if resp.Stdout != "" || resp.Bytes != 0 {
			t.Fatalf("缺文件时不该带回半份内容：stdout=%q bytes=%d", resp.Stdout, resp.Bytes)
		}
	})
}

// ---------- 3) 1 MiB 截断 ----------

// TestLogsServeTruncatesToOneMiB 两路各 >2 MiB 时：编码后 ≤ 1 MiB、truncated、
// 只保留**文件结尾**（最后一行在、第一行不在），bytes 仍是真实字节数。
func TestLogsServeTruncatesToOneMiB(t *testing.T) {
	const (
		firstStdout = "FIRST-STDOUT-LINE-SHOULD-NOT-SURVIVE"
		lastStdout  = "LAST-STDOUT-LINE-MUST-SURVIVE"
		firstStderr = "FIRST-STDERR-LINE-SHOULD-NOT-SURVIVE"
		lastStderr  = "LAST-STDERR-LINE-MUST-SURVIVE"
	)
	stdout := logsServeBigStream(firstStdout, lastStdout, "s", 2400) // ≈2.4 MiB
	stderr := logsServeBigStream(firstStderr, lastStderr, "e", 2400)

	dataDir := t.TempDir()
	store := newRunStore(dataDir, logsServeTestLog())
	rec := runStoreTestRecord("trc_big", "bigcap", 1, 1, 1, stdout)
	rec.Stderr = stderr
	if _, err := store.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	a, _ := logsServeTestAgent(t, dataDir)
	frame := a.logsResponseFor(&logsRequestFrame{
		Type: frameTypeLogsRequest, TaskID: rec.Input.TaskID, RequestID: "lreq_big",
	})

	// 硬约束：**编码后**不能超过 1 MiB（服务端对超限上行帧是 1009 关连接）。
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("json.Marshal(logs_response): %v", err)
	}
	if len(data) > ws.LogsResponseMaxBytes {
		t.Fatalf("回包编码后 = %d 字节，超过上限 %d（会被 1009 关连接）",
			len(data), ws.LogsResponseMaxBytes)
	}
	if !frame.Truncated {
		t.Fatalf("truncated = false, want true（两路各 >2 MiB，1 MiB 装不下）")
	}
	if want := int64(len(stdout) + len(stderr)); frame.Bytes != want {
		t.Fatalf("bytes = %d, want %d（档案里的真实字节数，不是回包长度）", frame.Bytes, want)
	}

	// 尾部语义：回包里必须是文件的**结尾**。下面两行 t.Log 是这条结论的证据
	// （-v 时能直接看到「窗口多大、含不含第一行/最后一行」）。
	t.Logf("两路各 %d 字节 → 编码后回包 %d 字节（上限 %d）；窗口 stdout=%d stderr=%d",
		frame.Bytes, len(data), ws.LogsResponseMaxBytes, len(frame.Stdout), len(frame.Stderr))
	t.Logf("stdout 含最后一行 %q = %v；含第一行 %q = %v",
		lastStdout, strings.Contains(frame.Stdout, lastStdout),
		firstStdout, strings.Contains(frame.Stdout, firstStdout))
	if !strings.Contains(frame.Stdout, lastStdout) {
		t.Fatalf("stdout 里没有文件最后一行（尾部窗口必须从文件末尾往前取）")
	}
	if strings.Contains(frame.Stdout, firstStdout) {
		t.Fatalf("stdout 里出现了文件第一行（说明取的是头部，不是尾部）")
	}
	if !strings.Contains(frame.Stderr, lastStderr) {
		t.Fatalf("stderr 里没有文件最后一行")
	}
	if strings.Contains(frame.Stderr, firstStderr) {
		t.Fatalf("stderr 里出现了文件第一行")
	}

	// tag 漂移守卫：这一帧必须能被服务端的解码器解成 *ws.LogsResponseFrame。
	decoded, err := ws.DecodeUpstream(data)
	if err != nil {
		t.Fatalf("ws.DecodeUpstream: %v", err)
	}
	got, ok := decoded.(*ws.LogsResponseFrame)
	if !ok {
		t.Fatalf("ws.DecodeUpstream 解出 %T, want *ws.LogsResponseFrame", decoded)
	}
	if got.RequestID != "lreq_big" || got.TaskID != rec.Input.TaskID {
		t.Fatalf("解码后的配对字段 = %q/%q", got.RequestID, got.TaskID)
	}
	if !got.Truncated || got.Bytes != frame.Bytes {
		t.Fatalf("解码后的 truncated/bytes = %v/%d, want true/%d", got.Truncated, got.Bytes, frame.Bytes)
	}
	if !strings.Contains(got.Stdout, lastStdout) {
		t.Fatalf("解码后的 stdout 丢了最后一行")
	}
}

// TestLogsServeDropsControlBytesAndInvalidUTF8 二进制输出（控制字节 + 坏 UTF-8）也要压得住：
// 归一之后编码膨胀被压到常数级，回包仍然 ≤1 MiB，且内容是合法 UTF-8。
func TestLogsServeDropsControlBytesAndInvalidUTF8(t *testing.T) {
	dataDir := t.TempDir()
	store := newRunStore(dataDir, logsServeTestLog())
	// 每行都是 ESC/CR/NUL + 坏字节（0x80 单独出现不是合法 UTF-8）：JSON 转义会把它撑大。
	line := "\x1b[31mboom\x00\x80\xfe tail\n"
	body := strings.Repeat(line, 140000) // ≈3.5 MiB
	rec := runStoreTestRecord("trc_bin", "bincap", 1, 1, 1, body)
	if _, err := store.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	a, _ := logsServeTestAgent(t, dataDir)
	frame := a.logsResponseFor(&logsRequestFrame{
		Type: frameTypeLogsRequest, TaskID: rec.Input.TaskID, RequestID: "lreq_bin",
	})
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if len(data) > ws.LogsResponseMaxBytes {
		t.Fatalf("回包编码后 = %d 字节，超过上限 %d", len(data), ws.LogsResponseMaxBytes)
	}
	if strings.ContainsRune(frame.Stdout, 0) || strings.Contains(frame.Stdout, "\x1b") {
		t.Fatalf("stdout 里还有 NUL/ESC（控制字节没折掉，JSON 会给它们 6 倍膨胀）")
	}
	if !strings.Contains(frame.Stdout, "tail") {
		t.Fatalf("stdout 尾部内容丢了：归一不该把可读内容一起吃掉")
	}
	if !utf8.ValidString(frame.Stdout) {
		t.Fatalf("stdout 不是合法 UTF-8（非法字节会被 JSON 换成 3 字节的 U+FFFD）")
	}
}

// ---------- 4) 索引兜底与 GC 清索引 ----------

// TestRunStoreTaskRefFallsBackAndRepairs 索引缺失/脏掉时仍要以档案为准：扫一遍
// meta.json 找回来，并把索引修好。
func TestRunStoreTaskRefFallsBackAndRepairs(t *testing.T) {
	dataDir := t.TempDir()
	store := newRunStore(dataDir, logsServeTestLog())
	rec := runStoreTestRecord("trc_ref", "refcap", 2, 3, 2, "out\n")
	if _, err := store.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	want := "trc_ref/refcap@2/3-a2"
	idxPath := filepath.Join(dataDir, runsSubdirName, runIndexDirName, rec.Input.TaskID)

	if got, ok := store.taskRef(rec.Input.TaskID); !ok || got != want {
		t.Fatalf("taskRef = %q, %v; want %q, true", got, ok, want)
	}

	// (a) 索引被删了 → 兜底扫描仍能取到，并顺手把索引写回来。
	if err := os.Remove(idxPath); err != nil {
		t.Fatalf("删索引: %v", err)
	}
	if got, ok := store.taskRef(rec.Input.TaskID); !ok || got != want {
		t.Fatalf("索引缺失时 taskRef = %q, %v; want %q, true（必须落到兜底扫描）", got, ok, want)
	}
	if raw, err := os.ReadFile(idxPath); err != nil {
		t.Fatalf("兜底命中后应该把索引修好: %v", err)
	} else if string(raw) != want+"\n" {
		t.Fatalf("修好的索引 = %q, want %q", string(raw), want+"\n")
	}

	// (b) 索引被写成指向一个不存在的目录 → 不能把它当答案（契约 §4）。
	if err := os.WriteFile(idxPath, []byte("trc_ref/refcap@2/9-a9\n"), runFileMode); err != nil {
		t.Fatalf("写脏索引: %v", err)
	}
	if got, ok := store.taskRef(rec.Input.TaskID); !ok || got != want {
		t.Fatalf("脏索引时 taskRef = %q, %v; want %q, true", got, ok, want)
	}
	if raw, err := os.ReadFile(idxPath); err != nil {
		t.Fatalf("读修好的索引: %v", err)
	} else if string(raw) != want+"\n" {
		t.Fatalf("脏索引没有被修正：%q", string(raw))
	}

	// (c) 索引里塞路径穿越 → 绝不采纳（跳过索引，走兜底扫描）。
	if err := os.WriteFile(idxPath, []byte("../../etc/passwd\n"), runFileMode); err != nil {
		t.Fatalf("写穿越索引: %v", err)
	}
	if got, ok := store.taskRef(rec.Input.TaskID); !ok || got != want {
		t.Fatalf("穿越索引时 taskRef = %q, %v; want %q, true", got, ok, want)
	}

	// (d) 内容层面：索引没了，agent 那条路径（logs_request → 回包）也要真的取到输出。
	if err := os.Remove(idxPath); err != nil {
		t.Fatalf("删索引: %v", err)
	}
	a, _ := logsServeTestAgent(t, dataDir)
	resp := a.logsResponseFor(&logsRequestFrame{
		Type: frameTypeLogsRequest, TaskID: rec.Input.TaskID, RequestID: "lreq_fallback",
	})
	if resp.Error != "" || resp.Stdout != rec.Stdout {
		t.Fatalf("索引缺失时 agent 取到 error=%q stdout=%q, want 空/%q", resp.Error, resp.Stdout, rec.Stdout)
	}
	// 而且它顺手把索引写回去了（下一次不用再扫全树）。
	if raw, err := os.ReadFile(idxPath); err != nil {
		t.Fatalf("agent 路径兜底命中后应修好索引: %v", err)
	} else if string(raw) != want+"\n" {
		t.Fatalf("agent 修好的索引 = %q, want %q", string(raw), want+"\n")
	}
}

// TestRunStoreGCDeletesOnlyMatchingIndex GC 删目录时清索引，但**只清正指向它的那条**。
func TestRunStoreGCDeletesOnlyMatchingIndex(t *testing.T) {
	dataDir := t.TempDir()
	store := newRunStore(dataDir, logsServeTestLog())
	store.retention = time.Hour
	store.maxCount = 0 // 只按时间删，不按条数

	// 同一个 task 重派两次：两条档案、一条索引（指向后写的那次）。
	first := runStoreTestRecord("trc_gc", "gccap", 1, 1, 1, "第一次\n")
	dir1, err := store.Save(first)
	if err != nil {
		t.Fatalf("Save#1: %v", err)
	}
	second := runStoreTestRecord("trc_gc", "gccap", 1, 1, 2, "第二次\n")
	dir2, err := store.Save(second)
	if err != nil {
		t.Fatalf("Save#2: %v", err)
	}
	idxPath := filepath.Join(dataDir, runsSubdirName, runIndexDirName, first.Input.TaskID)
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("读索引: %v", err)
	}
	if !strings.Contains(string(raw), "1-a2") {
		t.Fatalf("索引应指最新那次（attempt=2）：%q", string(raw))
	}

	// 只让老的那条超期。
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(dir1, old, old); err != nil {
		t.Fatalf("Chtimes(dir1): %v", err)
	}
	removed, err := store.GC(time.Now())
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if removed != 1 {
		t.Fatalf("GC removed = %d, want 1", removed)
	}
	if _, err := os.Stat(dir1); !os.IsNotExist(err) {
		t.Fatalf("过期目录还在: %v", err)
	}
	// 索引指向的是 dir2（活的那条）→ **不许**被删掉。
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("GC 删掉了不该删的索引（它指向的是另一条还在的档案）: %v", err)
	}
	if got, ok := store.taskRef(first.Input.TaskID); !ok || !strings.Contains(got, "1-a2") {
		t.Fatalf("GC 之后 taskRef = %q, %v; want 指向 1-a2", got, ok)
	}

	// 再让 dir2 超期 → GC 删它，索引必须跟着消失（否则会一直指向不存在的档案）。
	if err := os.Chtimes(dir2, old, old); err != nil {
		t.Fatalf("Chtimes(dir2): %v", err)
	}
	if _, err := store.GC(time.Now()); err != nil {
		t.Fatalf("GC#2: %v", err)
	}
	if _, err := os.Stat(idxPath); !os.IsNotExist(err) {
		t.Fatalf("档案删了索引还在（会让中枢拿到 no_local_archive 之外的错觉）: %v", err)
	}
}

// TestRunStoreIndexSkipsUnsafeTaskID task_id 要当文件名用：不能用的值跳过索引写入，
// 档案照存（靠兜底扫描仍然找得到）。
func TestRunStoreIndexSkipsUnsafeTaskID(t *testing.T) {
	for _, bad := range []string{
		"",
		".",
		"..",
		"a/b",
		`a\b`,
		"a\x00b",
		"a\x01b",
		strings.Repeat("t", maxTaskIDLen+1),
	} {
		if validTaskIDFile(bad) {
			t.Errorf("validTaskIDFile(%q) = true, want false（不能当文件名用）", bad)
		}
	}

	dataDir := t.TempDir()
	store := newRunStore(dataDir, logsServeTestLog())
	rec := runStoreTestRecord("trc_unsafe", "unsafecap", 1, 1, 1, "out\n")
	rec.Input.TaskID = "a/b" // 会写到 .tasks 外面去的那种
	if _, err := store.Save(rec); err != nil {
		t.Fatalf("Save（task_id 不能当文件名）：%v", err)
	}
	// 索引目录下不该出现任何东西（写失败只 WARN，不影响 Save）。
	entries, err := os.ReadDir(filepath.Join(dataDir, runsSubdirName, runIndexDirName))
	if err == nil && len(entries) != 0 {
		t.Fatalf(".tasks 下出现了 %d 项，want 0（非法 task_id 必须跳过索引）", len(entries))
	}
	// 档案本身仍在，List 能看到。
	list, err := store.List(runFilter{Version: -1})
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v; want 1 条", list, err)
	}
}

// ---------- 5) 端到端：真 WS + 真任务 ----------

// requestLogs 像真中枢那样在连接上下发一帧 logs_request（= NodeHub.FetchLogs 的对手方）。
func (h *w4Hub) requestLogs(t *testing.T, taskID, requestID string) {
	t.Helper()
	h.mu.Lock()
	c := h.conn
	h.mu.Unlock()
	if c == nil {
		t.Fatalf("假中枢还没拿到连接，不能发 logs_request")
	}
	// welcome/assign 早在 result 之前就写完了，这里是测试 goroutine 的串行写。
	if err := w4HubWrite(context.Background(), c, &logsRequestFrame{
		Type:      frameTypeLogsRequest,
		TaskID:    taskID,
		RequestID: requestID,
	}); err != nil {
		t.Fatalf("写 logs_request 失败: %v", err)
	}
}

// TestLogsServeRealWSRequestAfterTask 是这一版最要紧的一条证据（契约 §7 最后一条）：
//
//	真连接 + 真 assign + 真执行 → 档案落盘 → 中枢发 logs_request → 拿回本次执行的 stdout。
//
// 上传路径已经删掉（logs.go 不在了），所以「中枢还能拿到 stdout」只能靠这条帧路径成立。
func TestLogsServeRealWSRequestAfterTask(t *testing.T) {
	const marker = "logs-on-node-stdout-marker"

	hub := w4StartHub(t, 1, &assignFrame{
		Type:        frameTypeAssign,
		TaskID:      "tsk_live",
		Capability:  "demo.prep",
		Version:     1,
		TraceID:     "trc_live",
		Attempts:    1,
		SeqInTrace:  1,
		Params:      map[string]any{"title": "live"},
		LeaseUntil:  time.Now().Add(time.Minute).Unix(),
		MaxDuration: 10,
	})
	cfg := w4RealWSConfig(t, hub.srv.URL)
	dataDir := w4PreloadedDataDir(t, w4Artifact("demo.prep", 1, "echo "+marker+"\necho live-stderr-marker >&2\n"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := w4ServeRealWS(t, ctx, cfg, dataDir, 2*time.Second)

	hub.waitFrame(t, 5*time.Second, "hello", w4Cond(frameTypeHello, ""))
	// 档案在 result **之前**落盘（契约 §3.2），所以「收到 result」之后档案一定已经在了。
	hub.waitFrame(t, 15*time.Second, "tsk_live 的 result", w4Cond(frameTypeResult, "tsk_live"))

	// 中枢按需取日志。
	hub.requestLogs(t, "tsk_live", "lreq_live_1")
	frames := hub.waitFrame(t, 10*time.Second, "logs_response", w4Cond(frameTypeLogsResponse, "tsk_live"))
	raw, _, ok := w4Find(frames, frameTypeLogsResponse, "tsk_live")
	if !ok {
		t.Fatalf("没收到 logs_response: %s", w4Describe(frames))
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("重新编码 logs_response: %v", err)
	}
	if len(encoded) > ws.LogsResponseMaxBytes {
		t.Fatalf("线上回包 = %d 字节，超过 1 MiB 硬上限", len(encoded))
	}
	decoded, err := ws.DecodeUpstream(encoded)
	if err != nil {
		t.Fatalf("ws.DecodeUpstream(logs_response): %v", err)
	}
	resp, ok := decoded.(*ws.LogsResponseFrame)
	if !ok {
		t.Fatalf("解出 %T, want *ws.LogsResponseFrame", decoded)
	}
	if resp.RequestID != "lreq_live_1" {
		t.Errorf("request_id = %q, want lreq_live_1", resp.RequestID)
	}
	if resp.TaskID != "tsk_live" {
		t.Errorf("task_id = %q, want tsk_live", resp.TaskID)
	}
	if resp.Error != "" {
		t.Fatalf("error = %q, want 空（档案就在这台机器上）", resp.Error)
	}
	if !strings.Contains(resp.Stdout, marker) {
		t.Fatalf("stdout 里没有本次执行的输出标记 %q：stdout=%q", marker, resp.Stdout)
	}
	if !strings.Contains(resp.Stderr, "live-stderr-marker") {
		t.Fatalf("stderr 里没有本次执行的输出标记：stderr=%q", resp.Stderr)
	}
	if resp.Bytes <= 0 {
		t.Errorf("bytes = %d, want > 0（真实字节数）", resp.Bytes)
	}
	if resp.Truncated {
		t.Errorf("truncated = true, want false（几十字节的输出）")
	}

	// 上传路径确实不在了（契约 §3）：这条监听上只有那一次 WS 握手。
	if got := hub.allRequestsSeen(); len(got) != 1 || got[0] != "GET /v1/node/stream" {
		t.Errorf("不该再有日志上传请求：这条监听收到过 %v", got)
	}

	cancel()
	w4WaitServe(t, done, 15*time.Second)
}
