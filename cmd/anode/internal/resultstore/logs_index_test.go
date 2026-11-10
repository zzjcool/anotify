package resultstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

// archiveBuilder 是一次真实旁路档案的最小装配器：raw 逐字节 + 索引记录。
type archiveBuilder struct {
	t    *testing.T
	rec  *Attempt
	rec2 *LogRecorder
	now  func() time.Time
}

func newArchive(t *testing.T, taskID string, attempt int, now func() time.Time) *archiveBuilder {
	t.Helper()
	s := newStore(t)
	rec := beginAttempt(t, s, taskID, attempt, 0)
	if now == nil {
		now = time.Now
	}
	r := rec.OpenLogIndex(now)
	if err := r.Err(); err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	return &archiveBuilder{t: t, rec: rec, rec2: r, now: now}
}

// feed 同时写 raw（逐字节）与索引（按 chunk 喂，模拟 pipe 读取粒度）。
func (b *archiveBuilder) feed(stream string, data []byte, chunk int) {
	b.t.Helper()
	name := StdoutFileName
	if stream == protocol.LogStreamStderr {
		name = StderrFileName
	}
	f, err := b.rec.OpenLogFile(name)
	if err != nil {
		b.t.Fatalf("OpenLogFile: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		b.t.Fatalf("写 raw %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		b.t.Fatalf("关闭 raw %s: %v", name, err)
	}
	w := b.rec2.Writer(stream)
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		if _, err := w.Write(data[off:end]); err != nil {
			b.t.Fatalf("索引 writer 不能返回错误（会截 raw）：%v", err)
		}
	}
}

// close 收尾：按流冲 EOF 残片，再关索引。
func (b *archiveBuilder) close() {
	b.t.Helper()
	b.rec2.CloseStream(protocol.LogStreamStdout)
	b.rec2.CloseStream(protocol.LogStreamStderr)
	if err := b.rec2.Close(); err != nil {
		b.t.Fatalf("关闭索引: %v", err)
	}
}

func steppingClock(start time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	cur := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		out := cur
		cur = cur.Add(step)
		return out
	}
}

// assertStreamMatchesRaw 逐条比对：text 后若 raw 的下一字节是 LF，则该 LF 是记录
// 分隔符（不属于 text）。这样能同时证明「记录覆盖全部 raw 字节」与「没伪造换行」。
func assertStreamMatchesRaw(t *testing.T, raw []byte, lines []protocol.LogLine, stream string) {
	t.Helper()
	pos := 0
	n := 0
	for _, line := range lines {
		if line.Stream != stream {
			continue
		}
		n++
		text := []byte(line.Text)
		if pos+len(text) > len(raw) || !bytes.Equal(raw[pos:pos+len(text)], text) {
			t.Fatalf("%s 第 %d 条记录文字与 raw 不符（pos=%d, text=%q）", stream, n, pos, line.Text)
		}
		pos += len(text)
		if pos < len(raw) && raw[pos] == '\n' {
			pos++
		}
	}
	if pos != len(raw) {
		t.Fatalf("%s 记录只覆盖 raw 的 %d/%d 字节", stream, pos, len(raw))
	}
}

func readArchive(t *testing.T, rec *Attempt, q LogQuery) LogArchive {
	t.Helper()
	arch, found, err := rec.store.ReadLogs(rec.record.TaskID, rec.record.Attempt, q)
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if !found {
		t.Fatal("ReadLogs 必须找到刚写入的档案")
	}
	return arch
}

// TestLogRecorderSplitsLinesFragmentsAndEOF 覆盖 LF 分行、64 KiB 超长行片段化与
// EOF 残片：展示记录语义只做有界分段，不伪造额外换行；raw 逐字节不变。
func TestLogRecorderSplitsLinesFragmentsAndEOF(t *testing.T) {
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	b := newArchive(t, "t1", 1, steppingClock(start, time.Second))
	longLine := bytes.Repeat([]byte("x"), 200<<10)
	stdout := append([]byte("a\n"), longLine...)
	stdout = append(stdout, []byte("\n")...)
	stdout = append(stdout, []byte("tail")...) // EOF 残片（没有结尾 LF）
	stderr := []byte("e1\ne2\n")
	b.feed(protocol.LogStreamStdout, stdout, 1000)
	b.feed(protocol.LogStreamStderr, stderr, 7)
	b.close()

	// raw 必须逐字节保留。
	rawOut, err := os.ReadFile(filepath.Join(b.rec.Dir(), LogsDirName, StdoutFileName))
	if err != nil || !bytes.Equal(rawOut, stdout) {
		t.Fatalf("raw stdout 不该被改动：%d bytes err=%v", len(rawOut), err)
	}
	rawErr, err := os.ReadFile(filepath.Join(b.rec.Dir(), LogsDirName, StderrFileName))
	if err != nil || !bytes.Equal(rawErr, stderr) {
		t.Fatalf("raw stderr 不该被改动：%q err=%v", rawErr, err)
	}

	arch := readArchive(t, b.rec, LogQuery{})
	if arch.Partial || arch.Omitted || arch.StdoutMore || arch.StderrMore {
		t.Fatalf("完整档案不该带截断标记：%+v", arch)
	}
	var lastAt time.Time
	fragments := 0
	for i, line := range arch.Lines {
		if line.Sequence != i+1 {
			t.Fatalf("序号必须从 1 连续递增：第 %d 条是 %d", i, line.Sequence)
		}
		at, err := time.Parse(time.RFC3339Nano, line.At)
		if err != nil {
			t.Fatalf("at 不是 RFC3339Nano：%q", line.At)
		}
		if i > 0 && at.Before(lastAt) {
			t.Fatalf("测试时钟单调，at 不该倒退：%v → %v", lastAt, at)
		}
		lastAt = at
		if len(line.Text) > protocol.LogLineTextMaxBytes {
			t.Fatalf("单条 text 超过 64 KiB：%d", len(line.Text))
		}
		switch line.Stream {
		case protocol.LogStreamStdout:
			if len(line.Text) == protocol.LogLineTextMaxBytes {
				fragments++
			}
		case protocol.LogStreamStderr:
		default:
			t.Fatalf("未知 stream %q", line.Stream)
		}
	}
	assertStreamMatchesRaw(t, stdout, arch.Lines, protocol.LogStreamStdout)
	assertStreamMatchesRaw(t, stderr, arch.Lines, protocol.LogStreamStderr)
	if fragments < 3 {
		t.Fatalf("200 KiB 单行必须按 64 KiB 实际片段拆成多条记录，实际满片 %d 条", fragments)
	}
}

// TestLogRecorderEmptyAndEOFOnlyArchives 钉两种边界：真实零记录索引是合法空成功，
// EOF 残片是真实记录（不补尾 LF）。
func TestLogRecorderEmptyAndEOFOnlyArchives(t *testing.T) {
	empty := newArchive(t, "empty", 1, nil)
	empty.close()
	arch := readArchive(t, empty.rec, LogQuery{})
	if arch.Lines == nil || len(arch.Lines) != 0 {
		t.Fatalf("零记录必须是合法空数组：%#v", arch.Lines)
	}
	if arch.Partial {
		t.Fatal("正常关闭的零记录索引不是 partial")
	}
	if len(arch.Stdout) != 0 || len(arch.Stderr) != 0 {
		t.Fatalf("零输出档案 raw 应为空：%+v", arch)
	}

	eof := newArchive(t, "eof", 1, nil)
	eof.feed(protocol.LogStreamStdout, []byte("no-newline-at-eof"), 4)
	eof.close()
	arch = readArchive(t, eof.rec, LogQuery{})
	if len(arch.Lines) != 1 || arch.Lines[0].Text != "no-newline-at-eof" {
		t.Fatalf("EOF 残片必须是真实记录：%+v", arch.Lines)
	}
	raw, err := os.ReadFile(filepath.Join(eof.rec.Dir(), LogsDirName, StdoutFileName))
	if err != nil || string(raw) != "no-newline-at-eof" {
		t.Fatalf("raw 不能被补尾 LF：%q err=%v", raw, err)
	}
}

// TestLogRecorderUnicodeAndInvalidUTF8Projection 钉实际文字语义：合法 Unicode 原样，
// 无效 UTF-8 只按标准 JSON 文字投影（替换字符），raw 字节不受损。
func TestLogRecorderUnicodeAndInvalidUTF8Projection(t *testing.T) {
	src := append([]byte("中文-ok\nbad-"), 0xff)
	src = append(src, []byte("-end\n")...)
	b := newArchive(t, "t1", 1, nil)
	b.feed(protocol.LogStreamStdout, src, 3)
	b.close()

	raw, err := os.ReadFile(filepath.Join(b.rec.Dir(), LogsDirName, StdoutFileName))
	if err != nil || !bytes.Equal(raw, src) {
		t.Fatalf("raw 必须保留无效字节：% x err=%v", raw, err)
	}
	sum := sha256.Sum256(src)
	if got := sha256.Sum256(raw); got != sum {
		t.Fatalf("raw sha 不符：%s", hex.EncodeToString(got[:]))
	}
	arch := readArchive(t, b.rec, LogQuery{})
	if len(arch.Lines) != 2 {
		t.Fatalf("应有两条记录：%+v", arch.Lines)
	}
	if arch.Lines[0].Text != "中文-ok" {
		t.Fatalf("合法 Unicode 必须原样：%q", arch.Lines[0].Text)
	}
	if arch.Lines[1].Text != "bad-\uFFFD-end" {
		t.Fatalf("无效 UTF-8 必须按标准 JSON 投影成替换字符：%q", arch.Lines[1].Text)
	}
	at, err := time.Parse(time.RFC3339Nano, arch.Lines[1].At)
	if err != nil || at.IsZero() {
		t.Fatalf("at 必须是真实可解析时间：%q", arch.Lines[1].At)
	}
}

// TestLogRecorderFailureLatchesUnreadableAndRawSurvives 钉 R2：旁路索引故障只锁存
// 可检查状态，raw 与进程事实不受影响；读取侧明确 logs_unreadable，不伪空。
func TestLogRecorderFailureLatchesUnreadableAndRawSurvives(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "t1", 1, 0)
	// 把索引路径占成目录：打开索引必然失败（真实 IO 故障，不是 mock）。
	if err := os.MkdirAll(filepath.Join(rec.Dir(), LogsDirName, LogIndexFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	r := rec.OpenLogIndex(nil)
	if r.Err() == nil {
		t.Fatal("索引打开失败必须锁存错误")
	}
	if n, err := r.Writer(protocol.LogStreamStdout).Write([]byte("still-raw\n")); n != len("still-raw\n") || err != nil {
		t.Fatalf("索引 writer 必须永远成功（否则截 raw）：n=%d err=%v", n, err)
	}
	// raw 由 runner 的 tee 先写，独立于索引故障。
	rawPath := filepath.Join(rec.Dir(), LogsDirName, StdoutFileName)
	if err := os.WriteFile(rawPath, []byte("still-raw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err == nil {
		t.Fatal("Close 必须回锁存错误")
	}
	marker := filepath.Join(rec.Dir(), LogsDirName, LogIndexErrorFileName)
	if data, err := os.ReadFile(marker); err != nil || len(strings.TrimSpace(string(data))) == 0 {
		t.Fatalf("必须留下可检查的 index.error 标记：%q err=%v", data, err)
	}
	if data, err := os.ReadFile(rawPath); err != nil || string(data) != "still-raw\n" {
		t.Fatalf("索引故障不能截断 raw：%q err=%v", data, err)
	}
	_, found, err := s.ReadLogs("t1", 1, LogQuery{})
	if !found || !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("索引故障必须 logs_unreadable（found=true）：found=%v err=%v", found, err)
	}
}

// TestReadLogsMissingIndexIsUnreadableNotNoArchive 区分三种事实：无档案（found=false）、
// raw-only 旧档案（存在但不可结构化读取）、正常档案。
func TestReadLogsMissingIndexIsUnreadableNotNoArchive(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "raw-only", 1, 0)
	f, err := rec.OpenLogFile(StdoutFileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("legacy raw\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	_, found, err := s.ReadLogs("raw-only", 1, LogQuery{})
	if !found || !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("raw-only 档案必须 logs_unreadable：found=%v err=%v", found, err)
	}
	if _, found, err := s.ReadLogs("missing", 1, LogQuery{}); found || err != nil {
		t.Fatalf("不存在的档案必须是 found=false：found=%v err=%v", found, err)
	}
	if _, found, err := s.ReadLogs("raw-only", 0, LogQuery{}); !found || !errors.Is(err, ErrLogUnreadable) {
		t.Fatalf("latest 也必须识别 raw-only 不可读：found=%v err=%v", found, err)
	}
}

// TestReadLogsPartialAppendUsesCompletePrefix 钉运行中追加语义：半条 JSON 不是记录，
// 只回完整前缀并标 Partial；写完整后同一次扫描能看到新记录。
func TestReadLogsPartialAppendUsesCompletePrefix(t *testing.T) {
	b := newArchive(t, "t1", 1, nil)
	b.rec2.Writer(protocol.LogStreamStdout).Write([]byte("one\n"))
	arch := readArchive(t, b.rec, LogQuery{})
	if len(arch.Lines) != 1 || arch.Partial {
		t.Fatalf("完整行快照：%+v partial=%v", arch.Lines, arch.Partial)
	}

	indexPath := filepath.Join(b.rec.Dir(), LogsDirName, LogIndexFileName)
	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(indexPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"sequence":2,"stream":"stdout","text":"tw`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	arch = readArchive(t, b.rec, LogQuery{})
	if len(arch.Lines) != 1 || !arch.Partial {
		t.Fatalf("半条记录只能回完整前缀 + Partial：%+v partial=%v", arch.Lines, arch.Partial)
	}
	// 恢复现场（删掉人工追加的半条），再让真实 writer 落完整记录。
	if err := os.Truncate(indexPath, before.Size()); err != nil {
		t.Fatal(err)
	}
	b.rec2.Writer(protocol.LogStreamStdout).Write([]byte("two\n"))
	arch = readArchive(t, b.rec, LogQuery{})
	if len(arch.Lines) != 2 || arch.Partial {
		t.Fatalf("运行中追加后应看到两条完整记录：%+v partial=%v", arch.Lines, arch.Partial)
	}
	if arch.Lines[1].Text != "two" {
		t.Fatalf("新记录不对：%+v", arch.Lines[1])
	}
	b.close()
}

// TestReadLogsCorruptIndexIsUnreadableNotEmpty 钉「不可证明完整性」的多种形态：
// 缺头/坏 JSON/序号跳号/未知字段/版本不支持，全部 logs_unreadable，绝不伪空。
func TestReadLogsCorruptIndexIsUnreadableNotEmpty(t *testing.T) {
	makeValid := func(t *testing.T, taskID string) *Attempt {
		t.Helper()
		b := newArchive(t, taskID, 1, nil)
		b.feed(protocol.LogStreamStdout, []byte("one\ntwo\n"), 3)
		b.close()
		return b.rec
	}
	lineOf := func(t *testing.T, rec *Attempt) []string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(rec.Dir(), LogsDirName, LogIndexFileName))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	writeLines := func(t *testing.T, rec *Attempt, lines []string) {
		t.Helper()
		body := strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(rec.Dir(), LogsDirName, LogIndexFileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("bad-json", func(t *testing.T) {
		rec := makeValid(t, "bad-json")
		lines := lineOf(t, rec)
		lines[1] = `{"sequence":1,"stream":"stdout","text":"one"`
		writeLines(t, rec, lines)
		if _, found, err := rec.store.ReadLogs("bad-json", 1, LogQuery{}); !found || !errors.Is(err, ErrLogUnreadable) {
			t.Fatalf("坏 JSON 必须 logs_unreadable：found=%v err=%v", found, err)
		}
	})
	t.Run("sequence-gap", func(t *testing.T) {
		rec := makeValid(t, "sequence-gap")
		lines := lineOf(t, rec)
		lines[2] = `{"sequence":7,"stream":"stdout","text":"two","at":"2026-11-10T10:00:00Z"}`
		writeLines(t, rec, lines)
		if _, _, err := rec.store.ReadLogs("sequence-gap", 1, LogQuery{}); !errors.Is(err, ErrLogUnreadable) {
			t.Fatalf("序号跳号必须 logs_unreadable：%v", err)
		}
	})
	t.Run("unknown-field", func(t *testing.T) {
		rec := makeValid(t, "unknown-field")
		lines := lineOf(t, rec)
		lines[1] = `{"sequence":1,"stream":"stdout","text":"one","at":"2026-11-10T10:00:00Z","future":true}`
		writeLines(t, rec, lines)
		if _, _, err := rec.store.ReadLogs("unknown-field", 1, LogQuery{}); !errors.Is(err, ErrLogUnreadable) {
			t.Fatalf("未知字段索引必须 logs_unreadable：%v", err)
		}
	})
	t.Run("unsupported-version", func(t *testing.T) {
		rec := makeValid(t, "unsupported-version")
		lines := lineOf(t, rec)
		lines[0] = `{"format":"anotify.node.logs.index","version":3}`
		writeLines(t, rec, lines)
		if _, _, err := rec.store.ReadLogs("unsupported-version", 1, LogQuery{}); !errors.Is(err, ErrLogUnreadable) {
			t.Fatalf("不支持的索引版本必须 logs_unreadable：%v", err)
		}
	})
	t.Run("v1-no-completion-is-unsupported", func(t *testing.T) {
		// e3 开发期格式（v1 只有 header+记录、没有完成事实）不能补假完结，
		// 也不能被当成完整档案；原文件保持不动（不迁移/不删除）。
		rec := makeValid(t, "v1-index")
		lines := lineOf(t, rec)
		lines[0] = `{"format":"anotify.node.logs.index","version":1}`
		writeLines(t, rec, lines)
		if err := os.Remove(filepath.Join(rec.Dir(), LogsDirName, LogIndexDoneFileName)); err != nil {
			t.Fatal(err)
		}
		indexPath := filepath.Join(rec.Dir(), LogsDirName, LogIndexFileName)
		before, err := os.ReadFile(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, found, err := rec.store.ReadLogs("v1-index", 1, LogQuery{}); !found || !errors.Is(err, ErrLogUnreadable) {
			t.Fatalf("v1（无完成事实）必须 logs_unreadable：found=%v err=%v", found, err)
		}
		after, err := os.ReadFile(indexPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("旧索引档案不能被迁移/删除：%d→%d err=%v", len(before), len(after), err)
		}
	})
	t.Run("empty-file", func(t *testing.T) {
		rec := makeValid(t, "empty-file")
		if err := os.WriteFile(filepath.Join(rec.Dir(), LogsDirName, LogIndexFileName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := rec.store.ReadLogs("empty-file", 1, LogQuery{}); !errors.Is(err, ErrLogUnreadable) {
			t.Fatalf("空索引文件（缺头）必须 logs_unreadable：%v", err)
		}
	})
}

// TestReadLogsSinceTailLatestAndIsolation 钉 R4 的查询顺序：since 过滤 → tail 取尾 →
// 保持 sequence 原序；显式 attempt 与 latest 不串档。
func TestReadLogsSinceTailLatestAndIsolation(t *testing.T) {
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	s := newStore(t)
	first := beginAttempt(t, s, "t1", 1, 0)
	firstIdx := first.OpenLogIndex(steppingClock(start, 10*time.Second))
	for i := 0; i < 6; i++ {
		firstIdx.Writer(protocol.LogStreamStdout).Write([]byte{byte('a' + i), '\n'})
	}
	firstIdx.CloseStream(protocol.LogStreamStdout)
	if err := firstIdx.Close(); err != nil {
		t.Fatalf("关闭 attempt1 索引: %v", err)
	}
	second := beginAttempt(t, s, "t1", 2, 0)
	secondIdx := second.OpenLogIndex(nil)
	secondIdx.Writer(protocol.LogStreamStdout).Write([]byte("latest-only\n"))
	secondIdx.CloseStream(protocol.LogStreamStdout)
	if err := secondIdx.Close(); err != nil {
		t.Fatalf("关闭 attempt2 索引: %v", err)
	}

	// since: now=start+60s, since=35s → 阈值 start+25s，匹配 +30/+40/+50 三条。
	q := LogQuery{SinceSeconds: 35, Now: start.Add(60 * time.Second), TailLines: 2}
	arch, found, err := s.ReadLogs("t1", 1, q)
	if err != nil || !found {
		t.Fatalf("ReadLogs: found=%v err=%v", found, err)
	}
	if len(arch.Lines) != 2 || arch.Lines[0].Text != "e" || arch.Lines[1].Text != "f" {
		t.Fatalf("since→tail 必须取最新两条匹配记录且保持原序：%+v", arch.Lines)
	}
	if !arch.Omitted {
		t.Fatal("被 tail 丢掉的匹配记录必须标 Omitted（回包 truncated）")
	}
	if arch.Lines[0].Sequence != 5 || arch.Lines[1].Sequence != 6 {
		t.Fatalf("序号不能被重编号：%+v", arch.Lines)
	}

	// 无匹配：合法空数组（不是不可读）。
	arch, _, err = s.ReadLogs("t1", 1, LogQuery{SinceSeconds: 1, Now: start.Add(100 * time.Second)})
	if err != nil || len(arch.Lines) != 0 || arch.Omitted {
		t.Fatalf("时间窗过滤掉全部记录必须是合法空：%+v err=%v", arch, err)
	}

	// 显式 attempt=1 不越档；latest=2 只回 attempt 2。
	arch, found, err = s.ReadLogs("t1", 0, LogQuery{})
	if err != nil || !found || arch.Attempt != 2 || len(arch.Lines) != 1 || arch.Lines[0].Text != "latest-only" {
		t.Fatalf("latest 必须是 attempt 2：%+v found=%v err=%v", arch, found, err)
	}
	if _, found, err := s.ReadLogs("t1", 9, LogQuery{}); found || err != nil {
		t.Fatalf("不存在的显式 attempt 不能串档：found=%v err=%v", found, err)
	}
}

// TestReadLogsCandidateCacheIsBounded 钉读取有界：条数与 text 字节都有上限，被丢掉的
// 更早匹配记录只通过 Omitted 表达（仍然完整前缀、不无限吃内存）。
func TestReadLogsCandidateCacheIsBounded(t *testing.T) {
	b := newArchive(t, "many", 1, nil)
	w := b.rec2.Writer(protocol.LogStreamStdout)
	for i := 0; i < logIndexMaxCandidateRecords+100; i++ {
		if _, err := w.Write([]byte("x\n")); err != nil {
			t.Fatal(err)
		}
	}
	b.close()
	arch := readArchive(t, b.rec, LogQuery{})
	if len(arch.Lines) != logIndexMaxCandidateRecords {
		t.Fatalf("tailLines=0 时缓存条数必须有界 %d，实际 %d", logIndexMaxCandidateRecords, len(arch.Lines))
	}
	if !arch.Omitted {
		t.Fatal("超出缓存上界的匹配记录必须标 Omitted")
	}

	big := newArchive(t, "bigtext", 1, nil)
	bw := big.rec2.Writer(protocol.LogStreamStdout)
	text := strings.Repeat("y", protocol.LogLineTextMaxBytes) + "\n"
	for i := 0; i < 20; i++ {
		if _, err := bw.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	big.close()
	arch = readArchive(t, big.rec, LogQuery{})
	total := 0
	for _, line := range arch.Lines {
		total += len(line.Text)
	}
	if total > logIndexMaxCandidateTextBytes {
		t.Fatalf("缓存 text 必须有界 ≤ %d，实际 %d", logIndexMaxCandidateTextBytes, total)
	}
	if !arch.Omitted || len(arch.Lines) >= 20 {
		t.Fatalf("超出字节上界必须截掉更早记录并标 Omitted：%d 条", len(arch.Lines))
	}
}

// TestLogArchiveRetentionActiveSevenDayForever 是 D6 保留接缝：结构化索引与 raw 同
// 生命周期——活动 attempt 不删、默认 7 天内不删、0=永不删，过期才随目录删。
func TestLogArchiveRetentionActiveSevenDayForever(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	sevenDay := TTLFor(nil)
	if sevenDay <= 0 {
		t.Fatalf("默认保留期必须 >0：%d", sevenDay)
	}
	makeRec := func(taskID string, ttl int, created time.Time) *Attempt {
		rec, err := s.Begin(Record{
			TaskID: taskID, Node: "node-a", Attempt: 1, Capability: "echo",
			Digest: "sha256:aa", TTL: ttl, Phase: "succeeded", CreatedAt: created,
			Manifest: []Manifest{},
		})
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		r := rec.OpenLogIndex(nil)
		r.Writer(protocol.LogStreamStdout).Write([]byte("keep\n"))
		r.CloseStream(protocol.LogStreamStdout)
		if err := r.Close(); err != nil {
			t.Fatalf("关闭索引: %v", err)
		}
		return rec
	}
	active := makeRec("active", 1, now.Add(-100*24*time.Hour))
	recent := makeRec("recent", sevenDay, now.Add(-24*time.Hour))
	expired := makeRec("expired", sevenDay, now.Add(-8*24*time.Hour))
	forever := makeRec("forever", 0, now.Add(-365*24*time.Hour))

	removed, errs := s.Collect(now, map[string]bool{"active": true})
	for _, err := range errs {
		t.Fatalf("Collect 错误: %v", err)
	}
	indexOf := func(rec *Attempt) string {
		return filepath.Join(rec.Dir(), LogsDirName, LogIndexFileName)
	}
	for _, keep := range []*Attempt{active, recent, forever} {
		if _, err := os.Stat(indexOf(keep)); err != nil {
			t.Fatalf("%s 的结构化索引不能被删：%v", keep.record.TaskID, err)
		}
	}
	if _, err := os.Stat(indexOf(expired)); !os.IsNotExist(err) {
		t.Fatalf("过期档案的索引必须随目录清理：%v", err)
	}
	if len(removed) == 0 {
		t.Fatal("应至少删除一条过期记录")
	}
}

// TestReadLogsRawMoreFlagSurvives 钉 R4：raw 读取超过窗口（8 MiB）的 more 标记不能
// 被丢弃——档案本身继续完整，回包必须据此传播 truncated。
func TestReadLogsRawMoreFlagSurvives(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "t1", 1, 0)
	idx := rec.OpenLogIndex(nil)
	if err := idx.Err(); err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	idx.CloseStream(protocol.LogStreamStdout)
	if err := idx.Close(); err != nil {
		t.Fatalf("关闭索引: %v", err)
	}
	raw := bytes.Repeat([]byte("r"), maxLogRead+1024)
	if err := os.WriteFile(filepath.Join(rec.Dir(), LogsDirName, StdoutFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	arch, found, err := s.ReadLogs("t1", 1, LogQuery{})
	if err != nil || !found {
		t.Fatalf("ReadLogs: found=%v err=%v", found, err)
	}
	if !arch.StdoutMore {
		t.Fatal("raw 超过读取窗口必须回 More=true（不能静默丢弃）")
	}
	if len(arch.Stdout) <= maxLogRead || len(arch.Stdout) > maxLogRead+1 {
		t.Fatalf("raw 窗口必须停在 %d 字节附近，实际 %d", maxLogRead, len(arch.Stdout))
	}
	if arch.Lines == nil || len(arch.Lines) != 0 {
		t.Fatalf("合法零记录索引必须回 []：%#v", arch.Lines)
	}
	// 档案本身没有被缩减。
	info, err := os.Stat(filepath.Join(rec.Dir(), LogsDirName, StdoutFileName))
	if err != nil || info.Size() != int64(len(raw)) {
		t.Fatalf("原始档案必须完整：%v size=%d", err, info.Size())
	}
}

// TestLogRecorderFullWidthInvalidUTF8FragmentStaysReadable 钉「单段≤64 KiB」是
// **采集字节**上限：整段无效 UTF-8 经标准 JSON 投影后每个字节变成 3 字节的替换
// 字符，这仍是合法记录，读取侧不能把它误判为损坏索引。
func TestLogRecorderFullWidthInvalidUTF8FragmentStaysReadable(t *testing.T) {
	src := append(bytes.Repeat([]byte{0xff}, protocol.LogLineTextMaxBytes), '\n')
	b := newArchive(t, "invalid-wide", 1, nil)
	b.feed(protocol.LogStreamStdout, src, 4096)
	b.close()

	arch := readArchive(t, b.rec, LogQuery{})
	if len(arch.Lines) != 1 {
		t.Fatalf("64 KiB 无效 UTF-8 行必须是一条可读记录：%+v", arch.Lines)
	}
	want := strings.Repeat("\uFFFD", protocol.LogLineTextMaxBytes)
	if arch.Lines[0].Text != want {
		t.Fatalf("投影长度不对：%d bytes（want %d）", len(arch.Lines[0].Text), len(want))
	}
	raw, err := os.ReadFile(filepath.Join(b.rec.Dir(), LogsDirName, StdoutFileName))
	if err != nil || !bytes.Equal(raw, src) {
		t.Fatalf("raw 必须逐字节保留：%d bytes err=%v", len(raw), err)
	}
}
