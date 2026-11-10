package resultstore

// L1-R9-A/R9-B：索引完整性的确定性故障回归。
//
// 这里注入的是**真实 I/O 的小接口故障**（0 字节写失败、Sync/Close 失败、标记/完成
// 记录写失败、rename 失败），不是 mock 掉被验证的判定逻辑：读取侧走真实
// ReadLogs/File 路径，只是让写侧在指定位置失败。覆盖父冻结的六个场景：
//  1. 一条真实记录后下一条 0+err、marker 同败 → 不得完整成功/truncated=false；
//  2. header-only 后首条记录 0+err、marker 同败 → 不可伪真空；
//  3. Close/Sync/完成记录失败、marker 同败 → 不可伪正常关闭；
//  4. 活跃 0 记录 / 正常关闭 0 记录都是合法空；崩溃 header-only 不可伪空；
//  5. 正常收口 = count/seq/size 一致的完成事实；损坏/不一致 footer 明确拒绝；
//     无完成事实时只回可证安全前缀 + truncated；
//  6. 旁路 I/O 故障不改变 raw 字节（SHA 对照）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

// faultFS 是 logIndexFS 的故障注入实现：正常路径直通真实文件系统，只在指定
// 位置返回错误（0 字节写、marker/完成记录写失败、rename/Sync/Close 失败）。
type faultFS struct {
	real osLogIndexFS
	// failWriteCall 是第 N 次索引文件 Write（1=header）返回 0+err；0=不失败。
	failWriteCall  int
	writeCalls     int
	markerFails    bool
	doneWriteFails bool
	renameFails    bool
	syncFails      bool
	closeFails     bool
}

func (f *faultFS) OpenIndex(name string) (logIndexFile, error) {
	file, err := f.real.OpenIndex(name)
	if err != nil {
		return nil, err
	}
	return &faultFile{fs: f, file: file}, nil
}

func (f *faultFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	switch {
	case strings.HasSuffix(name, string(filepath.Separator)+LogIndexErrorFileName):
		if f.markerFails {
			return errors.New("injected: index.error write failed")
		}
	case strings.HasSuffix(name, LogIndexDoneFileName+".tmp"):
		if f.doneWriteFails {
			return errors.New("injected: completion write failed")
		}
	}
	return f.real.WriteFile(name, data, perm)
}

func (f *faultFS) Rename(oldName, newName string) error {
	if f.renameFails {
		return errors.New("injected: completion rename failed")
	}
	return f.real.Rename(oldName, newName)
}

func (f *faultFS) Remove(name string) error { return f.real.Remove(name) }

type faultFile struct {
	fs   *faultFS
	file logIndexFile
}

func (f *faultFile) Write(p []byte) (int, error) {
	f.fs.writeCalls++
	if f.fs.failWriteCall > 0 && f.fs.writeCalls == f.fs.failWriteCall {
		return 0, errors.New("injected: index write returned 0 bytes + error")
	}
	return f.file.Write(p)
}

func (f *faultFile) Sync() error {
	if f.fs.syncFails {
		return errors.New("injected: index sync failed")
	}
	return f.file.Sync()
}

func (f *faultFile) Close() error {
	err := f.file.Close()
	if f.fs.closeFails {
		return errors.New("injected: index close failed")
	}
	return err
}

// crashRecorder 模拟进程在任意时刻消失：关 fd、不写完成事实、注销活跃状态。
func crashRecorder(t *testing.T, r *LogRecorder) {
	t.Helper()
	r.mu.Lock()
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	r.closed = true
	r.mu.Unlock()
	r.unregister()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeRaw(t *testing.T, rec *Attempt, name string, data []byte) {
	t.Helper()
	f, err := rec.OpenLogFile(name)
	if err != nil {
		t.Fatalf("OpenLogFile(%s): %v", name, err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("写 raw %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭 raw %s: %v", name, err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}

func failExists(t *testing.T, path string, what string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s 必须不存在（err=%v）", what, err)
	}
}

// TestLogIndexDoubleFaultAfterOneRecordNeverLooksComplete 是 review major 的精确
// 反例：第一条记录真实完整落盘，第二条 Write 返回 0+err，同目录 marker 也写失败。
// 旧实现会在 LF 边界干净 EOF，误报 complete/truncated=false；修复后必须二选一：
// 活跃时看到锁存错误 → logs_unreadable；关闭后无完成事实 → truncated 前缀。
func TestLogIndexDoubleFaultAfterOneRecordNeverLooksComplete(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "double-fault", 1, 0)
	fs := &faultFS{real: osLogIndexFS{}, failWriteCall: 3, markerFails: true}
	idx := openLogIndexFS(rec, nil, fs)
	if err := idx.Err(); err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	if n, err := idx.Writer(protocol.LogStreamStdout).Write([]byte("one\n")); n != len("one\n") || err != nil {
		t.Fatalf("writer 必须永远返回成功：n=%d err=%v", n, err)
	}
	// 第二条：索引 Write 返回 0+err，latch 里的 marker 写同样失败。
	if n, err := idx.Writer(protocol.LogStreamStdout).Write([]byte("two\n")); n != len("two\n") || err != nil {
		t.Fatalf("索引故障不能传给 raw tee：n=%d err=%v", n, err)
	}
	if idx.Err() == nil {
		t.Fatal("0 字节写失败必须锁存错误")
	}
	raw := []byte("one\ntwo\n")
	writeRaw(t, rec, StdoutFileName, raw)
	wantSHA := sha256Hex(raw)

	// 活跃但已锁存错误：即使 marker 写失败也不能完整成功。
	if _, found, err := s.ReadLogs("double-fault", 1, LogQuery{}); !found || !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("活跃写入者已锁存错误必须 logs_unreadable：found=%v err=%v", found, err)
	}

	if err := idx.Close(); err == nil {
		t.Fatal("Close 必须回锁存错误（不能伪正常关闭）")
	}
	dir := filepath.Join(rec.Dir(), LogsDirName)
	failExists(t, filepath.Join(dir, LogIndexErrorFileName), "index.error")
	failExists(t, filepath.Join(dir, LogIndexDoneFileName), "index.done")

	arch, found, err := s.ReadLogs("double-fault", 1, LogQuery{})
	if err != nil {
		t.Fatalf("无完成事实的完整前缀允许成功返回（但必须 truncated）：%v", err)
	}
	if !found {
		t.Fatal("档案必须存在")
	}
	if !arch.Partial {
		t.Fatalf("无完成事实的索引不得声称完整（truncated 必须为 true）：%+v", arch)
	}
	if len(arch.Lines) != 1 || arch.Lines[0].Text != "one" {
		t.Fatalf("只允许可证安全前缀（1 条真实记录）：%+v", arch.Lines)
	}

	after := readFile(t, filepath.Join(dir, StdoutFileName))
	if !bytes.Equal(after, raw) || sha256Hex(after) != wantSHA {
		t.Fatalf("索引双故障不能改变 raw 字节：%q sha=%s", after, sha256Hex(after))
	}
}

// TestLogIndexHeaderOnlyDoubleFaultStaysUnreadable 覆盖父冻结场景 2：header 之后
// 首条记录就 0+err、marker 同败。活跃/关闭两种读取都不能当“真空成功”。
func TestLogIndexHeaderOnlyDoubleFaultStaysUnreadable(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "header-only-fault", 1, 0)
	fs := &faultFS{real: osLogIndexFS{}, failWriteCall: 2, markerFails: true}
	idx := openLogIndexFS(rec, nil, fs)
	if err := idx.Err(); err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	if n, err := idx.Writer(protocol.LogStreamStdout).Write([]byte("x\n")); n != 2 || err != nil {
		t.Fatalf("writer 必须永远返回成功：n=%d err=%v", n, err)
	}
	if idx.Err() == nil {
		t.Fatal("首条记录 0 字节写失败必须锁存")
	}
	if _, found, err := s.ReadLogs("header-only-fault", 1, LogQuery{}); !found || !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("活跃写入者错误必须 logs_unreadable：found=%v err=%v", found, err)
	}
	if err := idx.Close(); err == nil {
		t.Fatal("Close 必须回锁存错误")
	}
	dir := filepath.Join(rec.Dir(), LogsDirName)
	failExists(t, filepath.Join(dir, LogIndexErrorFileName), "index.error")
	failExists(t, filepath.Join(dir, LogIndexDoneFileName), "index.done")
	if _, found, err := s.ReadLogs("header-only-fault", 1, LogQuery{}); !found || !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("header-only 且无完成事实/无活跃者必须 logs_unreadable（不可伪真空）：found=%v err=%v", found, err)
	}
}

// TestLogIndexFinishFaultsNeverFakeNormalClose 覆盖父冻结场景 3：Sync/Close/
// 完成记录写/rename 任一失败 + marker 同败，都不得出现有效完成事实，也不得
// 被读侧当作正常关闭。
func TestLogIndexFinishFaultsNeverFakeNormalClose(t *testing.T) {
	cases := []struct {
		name    string
		fs      *faultFS
		records bool
	}{
		{"sync-fail-with-record", &faultFS{real: osLogIndexFS{}, syncFails: true, markerFails: true}, true},
		{"close-fail-with-record", &faultFS{real: osLogIndexFS{}, closeFails: true, markerFails: true}, true},
		{"completion-write-fail", &faultFS{real: osLogIndexFS{}, doneWriteFails: true, markerFails: true}, true},
		{"completion-rename-fail", &faultFS{real: osLogIndexFS{}, renameFails: true, markerFails: true}, true},
		{"sync-fail-empty", &faultFS{real: osLogIndexFS{}, syncFails: true, markerFails: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			rec := beginAttempt(t, s, "finish-fault-"+tc.name, 1, 0)
			idx := openLogIndexFS(rec, nil, tc.fs)
			if err := idx.Err(); err != nil {
				t.Fatalf("OpenLogIndex: %v", err)
			}
			if tc.records {
				idx.Writer(protocol.LogStreamStdout).Write([]byte("one\n"))
				idx.CloseStream(protocol.LogStreamStdout)
			}
			if err := idx.Close(); err == nil {
				t.Fatal("收口故障必须回错误")
			}
			dir := filepath.Join(rec.Dir(), LogsDirName)
			failExists(t, filepath.Join(dir, LogIndexDoneFileName), "index.done")
			failExists(t, filepath.Join(dir, LogIndexErrorFileName), "index.error")

			arch, found, err := s.ReadLogs("finish-fault-"+tc.name, 1, LogQuery{})
			if !found {
				t.Fatal("档案必须存在")
			}
			if !tc.records {
				if !errors.Is(err, ErrLogUnreadable) {
					t.Fatalf("空索引 + 收口失败 + 无完成事实必须 logs_unreadable：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("有可证前缀时允许成功返回（必须 truncated）：%v", err)
			}
			if !arch.Partial {
				t.Fatalf("收口失败不得伪装正常关闭（truncated 必须为 true）：%+v", arch)
			}
			if len(arch.Lines) != 1 || arch.Lines[0].Text != "one" {
				t.Fatalf("必须回可证安全前缀：%+v", arch.Lines)
			}
		})
	}
}

// TestLogIndexActiveEmptyClosedEmptyAndCrashHeaderOnly 覆盖父冻结场景 4：
// 活跃 0 记录与正常关闭 0 记录都是合法真空；崩溃 header-only 不可伪空。
func TestLogIndexActiveEmptyClosedEmptyAndCrashHeaderOnly(t *testing.T) {
	// 1) 同进程活跃、健康、0 记录：真实 lines:[] 成功。
	active := newArchive(t, "active-empty", 1, nil)
	arch := readArchive(t, active.rec, LogQuery{})
	if arch.Lines == nil || len(arch.Lines) != 0 || arch.Partial || arch.Omitted {
		t.Fatalf("活跃 0 记录必须是合法空成功：%+v", arch)
	}
	// 同一条活跃档案继续写入：读到的是有界一致快照。
	active.rec2.Writer(protocol.LogStreamStdout).Write([]byte("live\n"))
	arch = readArchive(t, active.rec, LogQuery{})
	if len(arch.Lines) != 1 || arch.Partial || arch.Lines[0].Text != "live" {
		t.Fatalf("活跃快照必须看到已提交记录：%+v", arch)
	}
	active.close()

	// 2) 正常关闭的 0 记录：完成事实证明空档案。
	closed := newArchive(t, "closed-empty", 1, nil)
	closed.close()
	arch = readArchive(t, closed.rec, LogQuery{})
	if arch.Lines == nil || len(arch.Lines) != 0 || arch.Partial {
		t.Fatalf("正常关闭 0 记录必须是合法空成功：%+v", arch)
	}

	// 3) 崩溃 header-only（无活跃、无完成事实）：不可伪空。
	crashed := newArchive(t, "crashed-empty", 1, nil)
	crashRecorder(t, crashed.rec2)
	if _, found, err := crashed.rec.store.ReadLogs("crashed-empty", 1, LogQuery{}); !found ||
		!errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("崩溃 header-only 必须 logs_unreadable：found=%v err=%v", found, err)
	}

	// 4) 崩溃但有完整记录：只回可证安全前缀 + truncated。
	partial := newArchive(t, "crashed-one", 1, nil)
	partial.rec2.Writer(protocol.LogStreamStdout).Write([]byte("one\n"))
	crashRecorder(t, partial.rec2)
	arch, found, err := partial.rec.store.ReadLogs("crashed-one", 1, LogQuery{})
	if err != nil || !found {
		t.Fatalf("可证前缀允许成功：found=%v err=%v", found, err)
	}
	if !arch.Partial || len(arch.Lines) != 1 || arch.Lines[0].Text != "one" {
		t.Fatalf("崩溃后的完整前缀必须标 truncated：%+v", arch)
	}
}

// TestLogIndexCompletionIsRequiredForCompleteSuccess 覆盖父冻结场景 5：
// 正常收口的 count/seq/size 必须与内容一致；损坏/不一致的完成事实拒绝；
// 无完成事实只回真实前缀 + truncated；运行中半条 JSON 不当记录。
func TestLogIndexCompletionIsRequiredForCompleteSuccess(t *testing.T) {
	b := newArchive(t, "completion", 1, nil)
	b.rec2.Writer(protocol.LogStreamStdout).Write([]byte("one\n"))
	b.rec2.Writer(protocol.LogStreamStderr).Write([]byte("two\n"))
	b.close()

	dir := filepath.Join(b.rec.Dir(), LogsDirName)
	donePath := filepath.Join(dir, LogIndexDoneFileName)
	indexPath := filepath.Join(dir, LogIndexFileName)

	// 完成事实本身必须精确描述内容（format/version/records/lastSequence/size）。
	var completion logIndexCompletion
	if err := json.Unmarshal(readFile(t, donePath), &completion); err != nil {
		t.Fatalf("完成事实不是合法 JSON：%v", err)
	}
	indexBytes := readFile(t, indexPath)
	if completion.Format != LogIndexCompletionFormat || completion.Version != LogIndexCompletionVersion ||
		completion.Records != 2 || completion.LastSequence != 2 || completion.Size != int64(len(indexBytes)) {
		t.Fatalf("完成事实与内容不一致：%+v（index=%d bytes）", completion, len(indexBytes))
	}

	arch := readArchive(t, b.rec, LogQuery{})
	if arch.Partial || len(arch.Lines) != 2 {
		t.Fatalf("有效完成事实必须完整成功：%+v", arch)
	}

	originalDone := readFile(t, donePath)
	restore := func() { writeFile(t, donePath, originalDone) }

	// count 不一致 = 完成事实不可信。
	mismatch := completion
	mismatch.Records = 1
	mismatch.LastSequence = 1
	data, _ := json.Marshal(mismatch)
	writeFile(t, donePath, append(data, '\n'))
	if _, _, err := b.rec.store.ReadLogs("completion", 1, LogQuery{}); !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("count/seq 不一致必须 logs_unreadable：%v", err)
	}

	// 损坏的完成事实（半条 JSON）。
	writeFile(t, donePath, []byte(`{"format":"anotify.node.logs.index.complete",`))
	if _, _, err := b.rec.store.ReadLogs("completion", 1, LogQuery{}); !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("损坏的完成事实必须 logs_unreadable：%v", err)
	}

	// 未知完成事实版本。
	unsupported := completion
	unsupported.Version = 99
	data, _ = json.Marshal(unsupported)
	writeFile(t, donePath, append(data, '\n'))
	if _, _, err := b.rec.store.ReadLogs("completion", 1, LogQuery{}); !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("未知完成事实版本必须 logs_unreadable：%v", err)
	}

	// 完成事实之后多出完整记录：字节数对不上 = 不可信。
	restore()
	extra := `{"sequence":3,"stream":"stdout","text":"extra","at":"2026-11-10T10:00:00Z"}` + "\n"
	writeFile(t, indexPath, append(append([]byte(nil), indexBytes...), []byte(extra)...))
	if _, _, err := b.rec.store.ReadLogs("completion", 1, LogQuery{}); !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("完成事实后追加字节必须 logs_unreadable：%v", err)
	}

	// 无完成事实 + 尾部半条记录：只回完整前缀 + truncated。
	writeFile(t, indexPath, indexBytes)
	if err := os.Remove(donePath); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(indexPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"sequence":3,"stream":"stdout","text":"tw`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	arch, _, err = b.rec.store.ReadLogs("completion", 1, LogQuery{})
	if err != nil || !arch.Partial || len(arch.Lines) != 2 {
		t.Fatalf("无完成事实 + 半条尾部必须回完整前缀 + truncated：%+v err=%v", arch, err)
	}

	// 无完成事实 + 最后一条记录被截断：同样只回前缀 + truncated。
	if err := os.Truncate(indexPath, int64(len(indexBytes))-5); err != nil {
		t.Fatal(err)
	}
	arch, _, err = b.rec.store.ReadLogs("completion", 1, LogQuery{})
	if err != nil || !arch.Partial || len(arch.Lines) != 1 || arch.Lines[0].Text != "one" {
		t.Fatalf("截断记录必须回可证前缀 + truncated：%+v err=%v", arch, err)
	}
}

// TestLogIndexRestartReadsRequireCompletion 是 review major 的语义回归：用第二个
// Store 实例（无同进程活跃登记，等价于进程重启后读旧档案）读取一个在完整 LF 边界
// 停下、但 raw 还有后续记录、且没有 index.error 的索引。旧实现仅凭 LF 边界就报
// complete/truncated=false（静默丢掉最新记录）；修复后没有完成事实只能回可证前缀
// 并标 truncated。
func TestLogIndexRestartReadsRequireCompletion(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "restart", 1, 0)
	idx := rec.OpenLogIndex(nil)
	if err := idx.Err(); err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	idx.Writer(protocol.LogStreamStdout).Write([]byte("one\n"))
	// 模拟「记录写失败 + marker 写失败」后的可观察状态：索引停在一条完整记录的
	// LF 边界，没有 marker、没有完成事实；raw 里其实还有第二条真实输出。
	writeRaw(t, rec, StdoutFileName, []byte("one\ntwo\n"))
	// 不 Close：进程重启后旧 writer 不存在，新 Store 没有活跃登记。
	restarted := New(s.Root, s.WorkRoot)
	arch, found, err := restarted.ReadLogs("restart", 1, LogQuery{})
	if !found {
		t.Fatal("档案必须存在")
	}
	if err != nil {
		t.Fatalf("可证安全前缀允许成功返回（但必须 truncated）：%v", err)
	}
	if !arch.Partial {
		t.Fatalf("没有完成事实的 LF 边界不得声称完整（truncated 必须为 true）：%+v", arch)
	}
	if len(arch.Lines) != 1 || arch.Lines[0].Text != "one" {
		t.Fatalf("只能回真实已落盘的安全前缀：%+v", arch.Lines)
	}

	// 同一 writer 正常收口后，同一份档案在重启视角下必须完整成功。
	idx.Writer(protocol.LogStreamStdout).Write([]byte("two\n"))
	idx.CloseStream(protocol.LogStreamStdout)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	arch, found, err = restarted.ReadLogs("restart", 1, LogQuery{})
	if err != nil || !found || arch.Partial {
		t.Fatalf("有效完成事实 + 重启视角必须完整成功：found=%v err=%v arch=%+v", found, err, arch)
	}
	if len(arch.Lines) != 2 || arch.Lines[0].Text != "one" || arch.Lines[1].Text != "two" {
		t.Fatalf("完成后的记录必须是两条：%+v", arch.Lines)
	}
}

// TestReadLogsSinceSecondsSaturation 钉 R9-C：极大的 sinceSeconds 不能因
// time.Duration 乘法回绕成未来阈值而误返回空；边界值安全饱和成“不限”。
func TestReadLogsSinceSecondsSaturation(t *testing.T) {
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	b := newArchive(t, "since", 1, steppingClock(start, time.Second))
	b.rec2.Writer(protocol.LogStreamStdout).Write([]byte("a\n"))
	b.rec2.Writer(protocol.LogStreamStdout).Write([]byte("b\n"))
	b.rec2.Writer(protocol.LogStreamStdout).Write([]byte("c\n"))
	b.close()

	maxInt := int(^uint(0) >> 1)
	for _, since := range []int{maxInt, int(logIndexMaxSinceSeconds), int(logIndexMaxSinceSeconds) + 1} {
		arch := readArchive(t, b.rec, LogQuery{SinceSeconds: since, Now: start.Add(time.Hour), TailLines: 0})
		if arch.Partial || arch.Omitted {
			t.Fatalf("since=%d 饱和为不限时不应截断：%+v", since, arch)
		}
		if len(arch.Lines) != 3 {
			t.Fatalf("since=%d 极大值不能被误算成空：%d 条", since, len(arch.Lines))
		}
	}

	// 正常可表示范围仍要真实过滤：now=start+60s、since=35s → 阈值 start+25s，
	// 三条记录都在阈值前 → 合法空（证明饱和修复没有破坏普通语义）。
	arch := readArchive(t, b.rec, LogQuery{SinceSeconds: 35, Now: start.Add(60 * time.Second)})
	if len(arch.Lines) != 0 || arch.Omitted {
		t.Fatalf("普通 since 过滤必须仍然生效：%+v", arch)
	}
}

// --- R9-P1: live 登记发布的初始化窗口 ---

// logReadResult 是真实 ReadLogs 调用的完整结果（测试 goroutine 回传用）。
type logReadResult struct {
	arch  LogArchive
	found bool
	err   error
}

// publicationProbe 是 openIndexPublished seam 的确定性闸门：写侧在 live 登记
// 发布点把 recorder 交给测试并停住，直到测试放行（同步通道，不用 sleep/轮询）。
type publicationProbe struct {
	published chan *LogRecorder
	release   chan struct{}
}

func newPublicationProbe() *publicationProbe {
	return &publicationProbe{published: make(chan *LogRecorder), release: make(chan struct{})}
}

func (p *publicationProbe) hook(r *LogRecorder) {
	p.published <- r
	<-p.release
}

// waitReadBeforeRelease 断言初始化放行之前真实读取没有返回任何结果：它必须
// 阻塞在 writer 初始化锁上。GREEN 下这是结构保证（锁由写侧持有），不是超时
// 或轮询碰运气。
func waitReadBeforeRelease(t *testing.T, readDone <-chan logReadResult) {
	t.Helper()
	select {
	case got := <-readDone:
		t.Fatalf("初始化未完成时真实读取不得返回（应阻塞在 writer 初始化锁上）：found=%v err=%v arch=%+v",
			got.found, got.err, got.arch)
	default:
	}
}

// failOpenFS 让 OpenIndex 真实失败，其余文件操作走真实文件系统。
type failOpenFS struct{ osLogIndexFS }

func (failOpenFS) OpenIndex(string) (logIndexFile, error) {
	return nil, errors.New("injected: open index failed")
}

// TestLogIndexLivePublicationHoldsInitLock 钉 R9-P1 的初始化窗口：live 登记
// 必须发生在 writer 初始化锁内。旧实现先 registerLive 再 r.mu.Lock()，读侧
// 可以在两步之间拿到「已登记但 header 未建」的 committed=0 快照，把健康活跃
// 档案瞬时读成 logs_unreadable。测试用私有 seam 在发布边界停住：证明锁已持有、
// 真实读取被阻塞，并在初始化完成后拿到合法活跃真空快照。
func TestLogIndexLivePublicationHoldsInitLock(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "live-publication", 1, 0)
	probe := newPublicationProbe()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(probe.release) }) }
	defer release()
	rec.openIndexPublished = probe.hook

	indexPath := filepath.Join(rec.Dir(), LogsDirName, LogIndexFileName)
	opened := make(chan *LogRecorder, 1)
	go func() { opened <- openLogIndexFS(rec, nil, osLogIndexFS{}) }()

	live := <-probe.published
	// 发布边界必须已持有 writer 初始化锁；否则读侧能抢在 header 建立前快照。
	if live.mu.TryLock() {
		live.mu.Unlock()
		t.Fatal("live 登记发布时 writer 初始化锁未被持有：读侧可能拿到 header 未建的 committed=0 伪快照")
	}
	// 发布点初始化确实尚未完成：新索引文件还不存在（fresh attempt）。
	if _, err := os.Stat(indexPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("发布点不应已存在 %s（err=%v）", LogIndexFileName, err)
	}

	readDone := make(chan logReadResult, 1)
	go func() {
		arch, found, err := s.ReadLogs("live-publication", 1, LogQuery{})
		readDone <- logReadResult{arch: arch, found: found, err: err}
	}()
	waitReadBeforeRelease(t, readDone)

	release()
	idx := <-opened
	if err := idx.Err(); err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	got := <-readDone
	if got.err != nil || !got.found {
		t.Fatalf("初始化完成后的读取必须是健康快照：found=%v err=%v", got.found, got.err)
	}
	if got.arch.Attempt != 1 || len(got.arch.Lines) != 0 || got.arch.Partial || got.arch.Omitted {
		t.Fatalf("活跃健康 0 记录必须是真空而非 truncated/伪结果：%+v", got.arch)
	}

	// 正常收口后仍然是合法空档案（完成事实 records=0/lastSeq=0）。
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if still := s.liveLogRecorder(filepath.Join(rec.Dir(), LogsDirName)); still != nil {
		t.Fatal("Close 后必须注销 live 登记")
	}
	arch, found, err := s.ReadLogs("live-publication", 1, LogQuery{})
	if err != nil || !found {
		t.Fatalf("关闭后的空档案必须完整成功：found=%v err=%v", found, err)
	}
	if len(arch.Lines) != 0 || arch.Partial || arch.Omitted {
		t.Fatalf("正常关闭 0 记录必须是真空：%+v", arch)
	}
}

// TestLogIndexFailedInitializationStaysTruthfulUnreadable 钉失败侧的同一发布
// 规则：初始化真失败时，读侧必须等到错误锁存后拿到 logs_unreadable，而不是
// 在窗口内被伪快照误导，也绝不伪空成功。
func TestLogIndexFailedInitializationStaysTruthfulUnreadable(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "live-publication-fail", 1, 0)
	probe := newPublicationProbe()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(probe.release) }) }
	defer release()
	rec.openIndexPublished = probe.hook

	opened := make(chan *LogRecorder, 1)
	go func() { opened <- openLogIndexFS(rec, nil, failOpenFS{}) }()

	live := <-probe.published
	if live.mu.TryLock() {
		live.mu.Unlock()
		t.Fatal("live 登记发布时 writer 初始化锁未被持有：失败初始化也可能被读成伪快照")
	}
	readDone := make(chan logReadResult, 1)
	go func() {
		arch, found, err := s.ReadLogs("live-publication-fail", 1, LogQuery{})
		readDone <- logReadResult{arch: arch, found: found, err: err}
	}()
	waitReadBeforeRelease(t, readDone)

	release()
	idx := <-opened
	if idx.Err() == nil {
		t.Fatal("注入的 OpenIndex 失败必须锁存错误")
	}
	got := <-readDone
	if !got.found {
		t.Fatal("attempt 元数据存在，读取必须 found=true")
	}
	if !errors.Is(got.err, ErrLogUnreadable) {
		t.Fatalf("初始化失败必须 logs_unreadable，得到 err=%v arch=%+v", got.err, got.arch)
	}

	// 注销（Close）之后没有完成事实/没有索引文件，仍然只能 logs_unreadable。
	if err := idx.Close(); err == nil {
		t.Fatal("失败初始化的 Close 必须返回锁存错误")
	}
	_, found, err := s.ReadLogs("live-publication-fail", 1, LogQuery{})
	if !found || !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("失败初始化关闭后仍必须 truthful unreadable：found=%v err=%v", found, err)
	}
}
