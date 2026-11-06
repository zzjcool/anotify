package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runStoreTestLogger 给档案库用例一个安静的 logger（它自己会 Warn，别把输出搞脏）。
func runStoreTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 本文件覆盖执行档案库（runstore.go）的**共享核心**：
//
//  1. Save → List → Load 的往返（路径形状、四份文件、权限）；
//  2. runLog 的「保留头部 + 截断标记」（与上传窗口的尾部窗口刻意相反）；
//  3. GC 的「先按时间、再按条数」与空目录收尾；
//  4. 对端给的脏字符串（`../..`、空、带分隔符）不得逃出 runs/。
//
// 落盘集成（runner/client 把 outcome 变成 runRecord）在 runpersist_test.go 里，
// CLI 的四种输出在 runs_cmd_test.go 里——这里只钉共享核心。

// runStoreTestRecord 造一条可落盘记录（helper 名字带前缀，避免与同包其它测试撞名）。
func runStoreTestRecord(traceID, capability string, version, seq, attempt int, stdout string) runRecord {
	started := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	return runRecord{
		Input: runInput{
			TraceID:    traceID,
			TaskID:     "tsk_" + traceID,
			Capability: capability,
			Version:    version,
			Attempt:    attempt,
			SeqInTrace: seq,
			Params:     map[string]any{"url": "https://example.test/x", "n": 2.0},
			WorkDir:    "/tmp/work/" + traceID,
		},
		Meta: runMeta{
			Status:          "succeeded",
			ExitCode:        0,
			Summary:         "exit 0",
			Emits:           2,
			StdoutBytes:     int64(len(stdout)),
			StderrBytes:     0,
			StdoutTruncated: false,
		},
		Stdout:   stdout,
		Stderr:   "",
		Started:  started,
		Finished: started.Add(1500 * time.Millisecond),
	}
}

// TestRunStoreSavePathAndFiles 钉住「路径即索引」的形状与四份文件的内容。
func TestRunStoreSavePathAndFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := newRunStore(dir, runStoreTestLogger())
	out := "hello 世界\n"
	got, err := s.Save(runStoreTestRecord("trc_a", "review", 2, 3, 1, out))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	want := filepath.Join(dir, runsSubdirName, "trc_a", "review@2", "3-a1")
	if got != want {
		t.Fatalf("run 目录 = %q, want %q", got, want)
	}

	// 目录 0700 / 文件 0600：输入与输出是用户自己的数据，不能被同机其他用户读到。
	if info, err := os.Stat(got); err != nil {
		t.Fatalf("stat run dir: %v", err)
	} else if perm := info.Mode().Perm(); perm != runDirMode {
		t.Errorf("run 目录权限 = %#o, want %#o", perm, runDirMode)
	}
	for _, name := range []string{runInputFile, runStdoutFile, runStderrFile, runMetaFile} {
		path := filepath.Join(got, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("缺少 %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != runFileMode {
			t.Errorf("%s 权限 = %#o, want %#o", name, perm, runFileMode)
		}
	}

	if data, err := os.ReadFile(filepath.Join(got, runStdoutFile)); err != nil {
		t.Fatalf("读 stdout.log: %v", err)
	} else if string(data) != out {
		t.Errorf("stdout.log = %q, want %q（必须是完整内容）", string(data), out)
	}

	// input.json：params **原样**（不脱敏）。
	var in runInput
	data, err := os.ReadFile(filepath.Join(got, runInputFile))
	if err != nil {
		t.Fatalf("读 input.json: %v", err)
	}
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatalf("input.json 不是合法 JSON: %v", err)
	}
	if in.TraceID != "trc_a" || in.Capability != "review" || in.Version != 2 || in.SeqInTrace != 3 || in.Attempt != 1 {
		t.Errorf("input.json 身份字段不对: %+v", in)
	}
	if in.Params["url"] != "https://example.test/x" {
		t.Errorf("input.json params 丢了原文: %+v", in.Params)
	}

	// meta.json：时间戳由 Save 格式化（唯一的格式化点）。
	var meta runMeta
	data, err = os.ReadFile(filepath.Join(got, runMetaFile))
	if err != nil {
		t.Fatalf("读 meta.json: %v", err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("meta.json 不是合法 JSON: %v", err)
	}
	if meta.StartedAt == "" || meta.FinishedAt == "" {
		t.Errorf("meta.json 缺时间戳: %+v", meta)
	}
	if meta.DurationMs != 1500 {
		t.Errorf("DurationMs = %d, want 1500", meta.DurationMs)
	}
	if meta.Status != "succeeded" || meta.Emits != 2 {
		t.Errorf("meta.json 内容不对: %+v", meta)
	}
}

// TestRunStoreListFiltersAndOrder List 的三个过滤维度 + 时间倒序。
func TestRunStoreListFiltersAndOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := newRunStore(dir, runStoreTestLogger())

	base := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	save := func(trace, cap string, version, seq, attempt int, started time.Time) {
		rec := runStoreTestRecord(trace, cap, version, seq, attempt, "out\n")
		rec.Started = started
		rec.Finished = started.Add(time.Second)
		if _, err := s.Save(rec); err != nil {
			t.Fatalf("Save(%s/%s@%d/%d-a%d): %v", trace, cap, version, seq, attempt, err)
		}
	}
	save("trc_1", "review", 1, 1, 1, base)
	save("trc_1", "review", 1, 2, 2, base.Add(time.Minute))
	save("trc_1", "build", 2, 1, 1, base.Add(2*time.Minute))
	save("trc_2", "review", 1, 1, 1, base.Add(3*time.Minute))

	all, err := s.List(runFilter{Version: -1})
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("List(all) = %d 条, want 4", len(all))
	}
	// 时间倒序：最近的在前。
	if all[0].TraceID != "trc_2" {
		t.Errorf("第一条 = %s, want trc_2（最近的在前）", all[0].TraceID)
	}
	if !all[0].Started.After(all[1].Started) {
		t.Errorf("List 没有按时间倒序: %v", []time.Time{all[0].Started, all[1].Started})
	}
	if all[0].Meta.Status != "succeeded" {
		t.Errorf("列表没读到 meta.json 的 status: %+v", all[0].Meta)
	}

	byTrace, err := s.List(runFilter{TraceID: "trc_1", Version: -1})
	if err != nil {
		t.Fatalf("List(trace): %v", err)
	}
	if len(byTrace) != 3 {
		t.Fatalf("List(trace=trc_1) = %d 条, want 3", len(byTrace))
	}
	for _, e := range byTrace {
		if e.TraceID != "trc_1" {
			t.Errorf("trace 过滤漏了: %+v", e)
		}
	}

	byCap, err := s.List(runFilter{Capability: "review", Version: -1})
	if err != nil {
		t.Fatalf("List(capability): %v", err)
	}
	if len(byCap) != 3 {
		t.Fatalf("List(capability=review) = %d 条, want 3", len(byCap))
	}

	byCapVer, err := s.List(runFilter{Capability: "review", Version: 1, Limit: 2})
	if err != nil {
		t.Fatalf("List(capability@version): %v", err)
	}
	if len(byCapVer) != 2 {
		t.Fatalf("List(capability=review,version=1,limit=2) = %d 条, want 2", len(byCapVer))
	}
	for _, e := range byCapVer {
		if e.Capability != "review" || e.Version != 1 {
			t.Errorf("capability@version 过滤漏了: %+v", e)
		}
	}

	// 空目录/不存在 = 空列表而不是错误。
	if empty, err := newRunStore(t.TempDir(), runStoreTestLogger()).List(runFilter{Version: -1}); err != nil || len(empty) != 0 {
		t.Errorf("空档案库 List = %v, %v; want 空、nil", empty, err)
	}
}

// TestRunStoreLoadRoundTrip show 的读回路径（含截断标记的传递）。
func TestRunStoreLoadRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := newRunStore(dir, runStoreTestLogger())

	rec := runStoreTestRecord("trc_b", "build.repo", 3, 7, 2, "完整输出 100%\n")
	rec.Meta.StdoutTruncated = true // 假设落盘时截过尾：读回必须告诉读的人
	rec.Stderr = "警告\n"
	if _, err := s.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := s.List(runFilter{Version: -1})
	if err != nil || len(entries) != 1 {
		t.Fatalf("List = %v, %v; want 1 条", entries, err)
	}

	d, err := s.Load(entries[0].Ref)
	if err != nil {
		t.Fatalf("Load(%q): %v", entries[0].Ref, err)
	}
	if d.Stdout != rec.Stdout || d.Stderr != rec.Stderr {
		t.Errorf("Load 的流内容不一致: stdout=%q stderr=%q", d.Stdout, d.Stderr)
	}
	if !d.StdoutTruncated || d.StderrTruncated {
		t.Errorf("截断事实没传出来: stdout=%v stderr=%v", d.StdoutTruncated, d.StderrTruncated)
	}
	if d.Input.Params["url"] != "https://example.test/x" {
		t.Errorf("Load 的 input 丢了 params: %+v", d.Input)
	}
	if d.Seq != 7 || d.Attempt != 2 || d.Capability != "build.repo" || d.Version != 3 {
		t.Errorf("Load 的路径解析不对: %+v", d.runEntry)
	}

	// 不存在的 ref → ErrRunNotFound（CLI 据此退出码 1）。
	if _, err := s.Load("trc_b/build.repo@3/9-a1"); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("Load(不存在) 的错误 = %v, want ErrRunNotFound", err)
	}
	// 形状不对/越界 → 也必须是 ErrRunNotFound，且**绝不能**读到 runs/ 之外的文件。
	for _, bad := range []string{
		"../../etc/passwd",
		"trc_b/build.repo@3/../../../etc/passwd",
		"trc_b/looks-like-a-cap/1-a1",
		"trc_b/build.repo@3/no-sep",
		"/etc/passwd",
	} {
		if _, err := s.Load(bad); err == nil {
			t.Errorf("Load(%q) 竟然成功了（路径必须被拒）", bad)
		}
	}
}

// TestRunStoreSaveIsIdempotent 同一 (trace, cap, ver, seq, attempt) 重复落盘 = 覆盖，不留两份。
func TestRunStoreSaveIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := newRunStore(dir, runStoreTestLogger())

	first := runStoreTestRecord("trc_c", "review", 1, 1, 1, "第一次\n")
	if _, err := s.Save(first); err != nil {
		t.Fatalf("Save#1: %v", err)
	}
	second := runStoreTestRecord("trc_c", "review", 1, 1, 1, "第二次（重放）\n")
	if _, err := s.Save(second); err != nil {
		t.Fatalf("Save#2: %v", err)
	}
	entries, err := s.List(runFilter{Version: -1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("重复落盘产生了 %d 条, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(entries[0].Dir, runStdoutFile))
	if err != nil {
		t.Fatalf("读 stdout.log: %v", err)
	}
	if string(data) != second.Stdout {
		t.Errorf("覆盖后的 stdout.log = %q, want %q", string(data), second.Stdout)
	}
}

// TestRunStoreSaveFailsGracefully 没有数据目录 / runs 被同名文件占着 → 只返回 error（调用方 Warn）。
func TestRunStoreSaveFailsGracefully(t *testing.T) {
	t.Parallel()

	if _, err := newRunStore("", runStoreTestLogger()).Save(runStoreTestRecord("trc_d", "x", 1, 1, 1, "")); err == nil {
		t.Error("没有数据目录时 Save 应当返回错误")
	}
	if _, err := (*runStore)(nil).Save(runStoreTestRecord("trc_d", "x", 1, 1, 1, "")); err == nil {
		t.Error("nil runStore 时 Save 应当返回错误（而不是 panic）")
	}

	dir := t.TempDir()
	// 把 runs 建成普通文件：建目录必然失败（模拟「盘满/权限错」那类写盘失败）。
	if err := os.WriteFile(filepath.Join(dir, runsSubdirName), []byte("x"), 0o600); err != nil {
		t.Fatalf("准备 runs 占位文件: %v", err)
	}
	if _, err := newRunStore(dir, runStoreTestLogger()).Save(runStoreTestRecord("trc_d", "x", 1, 1, 1, "")); err == nil {
		t.Error("runs 不可建时 Save 应当返回错误")
	}
	// nil logger 也不能 panic（newRunStore 自己兜底）。
	if _, err := newRunStore(dir, nil).Save(runStoreTestRecord("trc_d", "x", 1, 1, 1, "")); err == nil {
		t.Error("nil logger 时 Save 仍应返回错误而不是 panic")
	}
}

// TestRunStorePathSegmentsFromPeer 对端给的脏字符串不得逃出 runs/（也不丢记录）。
func TestRunStorePathSegmentsFromPeer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := newRunStore(dir, runStoreTestLogger())

	rec := runStoreTestRecord("../../etc", "a/b", 1, 1, 1, "out\n")
	got, err := s.Save(rec)
	if err != nil {
		t.Fatalf("Save(脏 trace/capability): %v", err)
	}
	root := filepath.Join(dir, runsSubdirName)
	if !strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Fatalf("落盘路径 %q 逃出了 %q", got, root)
	}
	if strings.Contains(got, "..") {
		t.Fatalf("落盘路径里还有 %q: %s", "..", got)
	}
	// 记录没有被丢掉：仍然能在列表里看到（名字是替身而已）。
	entries, err := s.List(runFilter{Version: -1})
	if err != nil || len(entries) != 1 {
		t.Fatalf("List = %v, %v; want 1 条（脏名字也要留记录）", entries, err)
	}

	// seq/attempt 缺省（旧中枢不带 seq_in_trace）→ 退到 attempt，再退到 1。
	for _, tc := range []struct {
		seq, attempt, wantSeq, wantAttempt int
	}{
		{0, 3, 3, 3},
		{0, 0, 1, 1},
		{5, 0, 5, 1},
	} {
		rel, err := runDirRel(runInput{TraceID: "trc_x", Capability: "c", Version: 1,
			SeqInTrace: tc.seq, Attempt: tc.attempt})
		if err != nil {
			t.Fatalf("runDirRel: %v", err)
		}
		want := "trc_x/c@1/" + strconv.Itoa(tc.wantSeq) + "-a" + strconv.Itoa(tc.wantAttempt)
		if rel != want {
			t.Errorf("seq=%d attempt=%d → %q, want %q", tc.seq, tc.attempt, rel, want)
		}
	}
}

// TestRunLogKeepsHead 完整输出捕捉器：留**头部**，超限时末尾加一行标记。
func TestRunLogKeepsHead(t *testing.T) {
	t.Parallel()

	l := newRunLog(10)
	if n, err := l.Write([]byte("0123456789ABC")); n != 13 || err != nil {
		t.Fatalf("Write = %d, %v; want 13, nil（永不短写/永不报错）", n, err)
	}
	if l.Total() != 13 {
		t.Errorf("Total = %d, want 13（真实字节数）", l.Total())
	}
	if !l.Truncated() {
		t.Error("Truncated 应当为 true")
	}
	got := l.String()
	if !strings.HasPrefix(got, "0123456789") {
		t.Errorf("没保留头部: %q", got)
	}
	if strings.Contains(got, "ABC") {
		t.Errorf("头部窗口外的内容不该出现: %q", got)
	}
	if !strings.Contains(got, "共 13 字节") {
		t.Errorf("缺截断标记（必须写明真实字节数）: %q", got)
	}

	// 未超限：逐字相同、没有标记。
	l2 := newRunLog(10)
	_, _ = l2.Write([]byte("short"))
	if l2.String() != "short" || l2.Truncated() {
		t.Errorf("未超限时 = %q, truncated=%v; want 逐字、false", l2.String(), l2.Truncated())
	}
}

// TestRunStoreGCByAgeAndCount GC 的两道闸：时间优先，条数兜底，并清掉空壳目录。
func TestRunStoreGCByAgeAndCount(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := newRunStore(dir, runStoreTestLogger())
	s.retention = 24 * time.Hour // 1 天
	s.maxCount = 3

	now := time.Now()
	mk := func(seq int, age time.Duration) string {
		rec := runStoreTestRecord("trc_gc", "review", 1, seq, 1, "x\n")
		rec.Started = now.Add(-age)
		rec.Finished = rec.Started.Add(time.Second)
		path, err := s.Save(rec)
		if err != nil {
			t.Fatalf("Save(seq=%d): %v", seq, err)
		}
		// GC 只看目录 mtime（不读 meta.json），所以要把年龄落在 mtime 上。
		stamp := now.Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		return path
	}

	expired := mk(1, 48*time.Hour) // 超过保留期
	mk(2, 2*time.Hour)
	mk(3, 1*time.Hour)
	mk(4, 30*time.Minute)
	keep := mk(5, 10*time.Minute)

	if _, err := os.Stat(expired); err != nil {
		t.Fatalf("准备阶段就丢了目录: %v", err)
	}
	removed, err := s.GC(now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	// 过期 1 条 + 条数兜底再删 1 条 = 2。
	if removed != 2 {
		t.Errorf("GC removed = %d, want 2（1 条超期 + 1 条超量）", removed)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Errorf("超期目录还在: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("最新的一条被误删了: %v", err)
	}
	entries, err := s.List(runFilter{Version: -1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("GC 之后剩 %d 条, want 3（maxCount）", len(entries))
	}

	// retention<=0 时**不按时间删**（只按条数）：「立刻全删」不该由一个环境变量误配造成。
	s.retention = 0
	s.maxCount = 0 // 0 = 不限条数
	removed, err = s.GC(now)
	if err != nil {
		t.Fatalf("GC(retention=0): %v", err)
	}
	if removed != 0 {
		t.Errorf("retention=0/maxCount=0 时 GC removed = %d, want 0（不该删任何东西）", removed)
	}

	// 全清 → 空掉的 trace/cap 目录也要收掉。
	s.maxCount = 1
	if _, err := s.GC(now); err != nil {
		t.Fatalf("GC(maxCount=1): %v", err)
	}
	left, err := os.ReadDir(filepath.Join(dir, runsSubdirName))
	if err != nil {
		t.Fatalf("读 runs: %v", err)
	}
	// .tasks 是任务索引目录（契约 §4），**不是**一条 trace：这里按名字把它排掉再数。
	// 它不是 GC 的对象，会一直陪着剩下的档案（带着指向 keep 那一条的索引）。
	traces := 0
	for _, entry := range left {
		if entry.Name() == runIndexDirName {
			continue
		}
		traces++
	}
	if traces != 1 {
		t.Errorf("runs/ 下的 trace 目录 = %d 个, want 1（runs/ 里共 %d 项，含索引目录 .tasks）",
			traces, len(left))
	}
}

// TestStartRunGCStopsOnCancel 后台 GC 的 ctx 取消必须能停下来（优雅退出/测试都要它）。
func TestStartRunGCStopsOnCancel(t *testing.T) {
	t.Parallel()

	s := newRunStore(t.TempDir(), runStoreTestLogger())
	s.maxCount = 1
	for i := 1; i <= 3; i++ {
		if _, err := s.Save(runStoreTestRecord("trc_t", "c", 1, i, 1, "x")); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	stop := startRunGC(t.Context(), s, runStoreTestLogger())
	// 启动时那一次是同步在 goroutine 里跑的：stop 会等它结束（<-done），
	// 所以 stop 返回之后条数上限一定已经生效。
	stop()

	entries, err := s.List(runFilter{Version: -1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("启动时的那次 GC 没把条数压到 1：剩 %d 条", len(entries))
	}

	// 没有数据目录时是空实现：不 panic、可调用。
	startRunGC(t.Context(), newRunStore("", runStoreTestLogger()), nil)()
}
