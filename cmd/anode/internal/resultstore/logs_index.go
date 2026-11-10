package resultstore

// 结构化日志记录索引（R1/R2/R9 的真实旁路档案）。
//
// 事实来源分工：
//   - logs/stdout.log / logs/stderr.log 是**逐字节**原始档案，行为不变（打开即截断、
//     0600、随 attempt 目录一起被 D6 保留/回收）；
//   - logs/index.jsonl 是节点在采集时**真实观察并串行记录**的旁路索引：首行是格式
//     头，之后每行一条 {sequence,stream,text,at} JSON 记录。序号跨两流严格递增、
//     从 1 开始；at 是采集该记录时的节点当前时间，不是 finishedAt。
//   - logs/index.done 是**完成事实**（R9-A）：只有记录全部写完、索引 Sync 与 Close
//     都成功之后才 temp+rename 原子落盘，内容带记录条数、最后序号与索引字节数。
//     读取侧必须拿它与实际内容逐项核对，一致才算完整成功；没有完成事实的已关闭
//     索引最多只回「可证安全前缀」并标 truncated，header-only 且无活跃写入者
//     （崩溃）明确 logs_unreadable，绝不靠「尾部恰好落在 LF 边界」冒充完整。
//
// 完成事实不依赖「index.error 标记一定可写」：记录/刷写/关闭/完成记录任一步失败
// 都不会留下有效 index.done；index.done 只在索引文件关闭成功后原子落盘，读取侧
// 看到它就意味着写侧全部步骤都成功过。错误同时锁存在内存（同进程活跃 writer 的
// 健康快照）与 index.error（诊断/重启后的持久证据）：标记写失败也不会误报成功。
//
// 索引不是 raw 的替代品，也不能从 raw 回推：raw 只有字节，没有逐条采集时间/序号。
// 因此旧开发期的 raw-only 档案不会被迁移或补时间，读取时明确报 logs_unreadable；
// 成功空输出必须来自一个真实存在、格式合法、带完成事实（或确有同进程活跃写入者）
// 的零记录索引。
//
// 写入失败（打开/写头/写记录）只锁存错误并留下 index.error 标记：raw 的 tee 分支
// 永远先写文件、索引 writer 永不对 io.MultiWriter 返回错误，所以旁路故障不会截断
// raw、也不会改变进程退出事实。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

const (
	// LogIndexFileName 是结构化记录索引的文件名（在 logs/ 下，与 raw 双流并列）。
	LogIndexFileName = "index.jsonl"
	// LogIndexErrorFileName 是写入失败的锁存标记；存在即表示无法证明索引完整性。
	LogIndexErrorFileName = "index.error"
	// LogIndexDoneFileName 是**完成事实**文件名（R9-A）：正常 Close 成功后才出现，
	// 内容见 logIndexCompletion。它和索引文件分开正是为了让「完成事实的写入」本身
	// 成为最后一步原子操作：rename 失败 = 没有完成事实（安全方向），而不是把半条
	// footer 留在索引里让读者猜。
	LogIndexDoneFileName = "index.done"
	// LogIndexFormat / LogIndexVersion 是索引头的封闭标识。版本不认识 = 不支持，
	// 明确报不可读而不是猜着解析。
	//
	// v2（R9）起，完整性以 logs/index.done 的完成事实为准；v1 是 e3 开发期的
	// 「只有 header+记录、没有完成事实」格式，无法证明完整，按 unsupported 明确
	// 不可读——不补假完结、不迁移、不删除原档案。
	LogIndexFormat  = "anotify.node.logs.index"
	LogIndexVersion = 2
	// LogIndexCompletionFormat / LogIndexCompletionVersion 是完成事实的封闭标识。
	LogIndexCompletionFormat  = "anotify.node.logs.index.complete"
	LogIndexCompletionVersion = 1

	// logIndexMaxLineBytes 是单行（头或记录）的读取上限。text 最多 64 KiB 采集
	// 字节，标准 JSON 转义最坏约 6 倍，再留出元数据余量。
	logIndexMaxLineBytes = 512 << 10
	// logIndexMaxDecodedTextBytes 是解码后 text 的合法上限。LogLineTextMaxBytes
	// 限制的是**采集字节**：无效 UTF-8 每个字节按标准 JSON 投影成一个 U+FFFD
	// （3 字节），所以解码后的合法长度最坏是 3 倍。超过它才是损坏记录。
	logIndexMaxDecodedTextBytes = 3 * protocol.LogLineTextMaxBytes
	// logIndexMaxCandidateTextBytes 是有界尾部缓存的 text 总字节上限：一帧
	// ≤1 MiB，缓存超过这个量的更早记录不可能被完整传输，丢弃只影响截断标记。
	logIndexMaxCandidateTextBytes = protocol.LogsResponseMaxBytes
	// logIndexMaxCandidateRecords 是条数上限（tailLines=0 无限条时的内存上界）。
	// 一帧 1 MiB 最多装约 1 万条最小记录，缓存再多的更早记录只会被帧上限丢掉。
	logIndexMaxCandidateRecords = 16384
	// logIndexMaxSinceSeconds 是能安全放进 time.Duration（纳秒）的秒数上限。
	// 更大的 sinceSeconds 按「起点早于全部可表示的正常记录」= 不限处理，绝不
	// 让 int64 乘法回绕成未来阈值而误返回空。
	logIndexMaxSinceSeconds = int64(1<<63-1) / int64(time.Second)
)

// ErrLogUnreadable 表示档案存在但结构化记录不可读：索引缺失/损坏/版本不支持/
// 写入失败锁存/没有完成事实且无活跃写入者。调用方必须映射为 logs_unreadable，
// 不能返回空成功。
var ErrLogUnreadable = errors.New("resultstore: 日志记录索引不可读")

// errIndexPartialLine 是「尾部有半条记录」的内部信号（不是损坏：正在追加，
// 或崩溃在一条记录中间）。它只允许把已完整前缀作为 truncated 结果返回。
var errIndexPartialLine = errors.New("resultstore: 索引尾部记录未写完")

// errIndexLineTooLong 是「一行超出上限」的损坏/不支持信号。
var errIndexLineTooLong = errors.New("resultstore: 索引行超过读取上限")

// logIndexHeader 是索引首行。未知 format/version 一律不可读。
type logIndexHeader struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
}

// logIndexCompletion 是 index.done 的完成事实（R9-A）。
//
// Records 与 LastSequence 必须同时与实际内容一致：记录序号从 1 连续递增，所以
// 两者在健康档案里相等，读侧显式核对两者（不拿 attempt.finishedAt 代替）。
// Size 是索引文件在完成时刻的字节数（header+全部记录行），用来证明「完成事实之后
// 没有多出/缺少任何字节」。
type logIndexCompletion struct {
	Format       string `json:"format"`
	Version      int    `json:"version"`
	Records      int    `json:"records"`
	LastSequence int    `json:"lastSequence"`
	Size         int64  `json:"size"`
}

// logIndexFile 是索引写入用到的最小文件操作（真实实现是 *os.File；测试注入小 I/O
// 故障，不新增公开 protocol/构造参数）。
type logIndexFile interface {
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

// logIndexFS 是索引/标记/完成事实用到的最小文件系统操作。
type logIndexFS interface {
	OpenIndex(name string) (logIndexFile, error)
	WriteFile(name string, data []byte, perm os.FileMode) error
	Rename(oldName, newName string) error
	Remove(name string) error
}

// osLogIndexFS 是 logIndexFS 的真实实现。
type osLogIndexFS struct{}

func (osLogIndexFS) OpenIndex(name string) (logIndexFile, error) {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (osLogIndexFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm)
}

func (osLogIndexFS) Rename(oldName, newName string) error { return os.Rename(oldName, newName) }

func (osLogIndexFS) Remove(name string) error { return os.Remove(name) }

// LogRecorder 是 attempt 级、两流共享的串行索引写入者。
//
// 它不拥有 raw 文件：runner 在同一个 io.MultiWriter 里先写 raw，再喂这里。
// 所有对外的写入口都**不返回错误**（Writer 返回的 io.Writer 永远 len(p), nil），
// 故障只锁存在 Err()/index.error，绝不通过 tee 的返回错误截断 raw。
//
// 同进程的活跃状态登记在 Store 的私有 live 表里（key = logs 目录绝对路径）：
// 读侧据此区分「正常活跃的零记录」与「崩溃留下的 header-only」，并能在标记写失败
// 时仍然看到锁存错误。正常/异常关闭都会注销。
type LogRecorder struct {
	mu      sync.Mutex
	dir     string
	key     string
	store   *Store
	fs      logIndexFS
	f       logIndexFile
	now     func() time.Time
	seq     int
	written int64
	err     error
	buffers map[string][]byte
	closed  bool
}

// OpenLogIndex 打开（截断重建）本 attempt 的结构化记录索引并写入格式头。
//
// 打开失败不会让调用失败：返回的 recorder 会锁存错误，Writer 是安全空写，
// 读取侧看到 index.error（或缺失索引）报 logs_unreadable。now=nil 用真实时钟。
func (a *Attempt) OpenLogIndex(now func() time.Time) *LogRecorder {
	return openLogIndexFS(a, now, osLogIndexFS{})
}

// openLogIndexFS 是可注入 I/O 的真实实现入口（测试专用；不新增公开构造参数）。
func openLogIndexFS(a *Attempt, now func() time.Time, fs logIndexFS) *LogRecorder {
	if now == nil {
		now = time.Now
	}
	dir := filepath.Join(a.dir, LogsDirName)
	r := &LogRecorder{
		dir:     dir,
		key:     dir,
		store:   a.store,
		fs:      fs,
		now:     now,
		buffers: make(map[string][]byte, 2),
	}
	// 打开期间持有 writer 锁，并且**在锁内登记 live**：读侧的活跃快照要么还
	// 看不到这次打开（未发布），要么阻塞在同一把锁上等到本次有界初始化完成，
	// 只可能看到头已写好/错误已锁存的稳定状态。若先登记再取锁，读侧就能在两步
	// 之间拿到「已登记但 header 未建」的 committed=0 快照，把健康活跃档案瞬时
	// 读成 logs_unreadable。
	//
	// 锁序：这里持 r.mu 再取 store.liveMu；读侧 liveLogRecorder 取 liveMu 后
	// 立即释放、snapshot 才取 r.mu；Close 先释放 r.mu 再注销。没有任何路径
	// 持有 liveMu 去等 r.mu，因此不构成锁序反转。
	r.mu.Lock()
	defer r.mu.Unlock()
	// 打开失败也留下「不健康活跃者」的内存证据，读侧即使写不出 index.error
	// 也不会把它误判成完整档案。Close 时注销。
	if a.store != nil {
		a.store.registerLive(r.key, r)
	}
	if a.openIndexPublished != nil {
		a.openIndexPublished(r)
	}
	path := filepath.Join(dir, LogIndexFileName)
	f, err := fs.OpenIndex(path)
	if err != nil {
		r.latchLocked(fmt.Errorf("打开 %s: %w", path, err))
		return r
	}
	r.f = f
	// 同一 attempt 目录可能被重新 Begin（重启后复用）：旧标记/旧完成事实必须清掉，
	// 否则新索引会被旧结论污染。索引已 O_TRUNC；即便清理失败，旧 index.done 的
	// size/count 也和新内容对不上，读侧只会 logs_unreadable，不会假完整。
	for _, name := range []string{LogIndexErrorFileName, LogIndexDoneFileName, LogIndexDoneFileName + ".tmp"} {
		if err := fs.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.latchLocked(fmt.Errorf("清理旧索引标记 %s: %w", name, err))
			return r
		}
	}
	header, err := json.Marshal(logIndexHeader{Format: LogIndexFormat, Version: LogIndexVersion})
	if err != nil {
		r.latchLocked(err)
		return r
	}
	r.writeLineLocked(header)
	return r
}

// Err 返回锁存的写入故障（nil = 正常）。
func (r *LogRecorder) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// snapshot 在写入者锁内原子取「是否仍活跃 / 已提交字节数 / 锁存错误」。
//
// 读侧用它在有界一致前缀上扫描：written 永远停在完整行边界（写一行期间不释放锁），
// 所以 committed 以内的字节可以被当作稳定快照。
func (r *LogRecorder) snapshot() (active bool, committed int64, err error) {
	if r == nil {
		return false, 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && r.err == nil, r.written, r.err
}

// Writer 返回往某一流喂原始采集字节的 io.Writer。
//
// 返回值永远 (len(p), nil)：索引故障不能让 io.Copy 提前结束，否则 raw 会被截断。
func (r *LogRecorder) Writer(stream string) io.Writer {
	return logRecorderWriter{rec: r, stream: stream}
}

type logRecorderWriter struct {
	rec    *LogRecorder
	stream string
}

func (w logRecorderWriter) Write(p []byte) (int, error) {
	if w.rec != nil {
		w.rec.WriteStream(w.stream, p)
	}
	return len(p), nil
}

// WriteStream 把一条流新采集到的字节切成记录：LF 分行、EOF/超长按实际片段成条。
//
// 单条 text 最多 LogLineTextMaxBytes；超长行的后续片段仍是同一条脚本行的真实
// 观察片段（不伪造额外换行），raw 档案保留完整字节。
func (r *LogRecorder) WriteStream(stream string, p []byte) {
	if r == nil || len(p) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil {
		return
	}
	buf, ok := r.buffers[stream]
	if !ok && stream != protocol.LogStreamStdout && stream != protocol.LogStreamStderr {
		r.latchLocked(fmt.Errorf("未知日志流 %q", stream))
		return
	}
	buf = append(buf, p...)
	for {
		if idx := bytes.IndexByte(buf, '\n'); idx >= 0 && idx <= protocol.LogLineTextMaxBytes {
			r.emitLocked(stream, buf[:idx])
			buf = buf[idx+1:]
			continue
		}
		if len(buf) > protocol.LogLineTextMaxBytes {
			r.emitLocked(stream, buf[:protocol.LogLineTextMaxBytes])
			buf = buf[protocol.LogLineTextMaxBytes:]
			continue
		}
		break
	}
	r.buffers[stream] = buf
}

// CloseStream 在一条流 EOF 时把残片作为真实观察记录落盘。
//
// EOF 处没有 LF 也不补：text 就是实际采集到的最后一段字节。
func (r *LogRecorder) CloseStream(stream string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil {
		return
	}
	buf := r.buffers[stream]
	if len(buf) > 0 {
		r.emitLocked(stream, buf)
	}
	delete(r.buffers, stream)
}

// Close 正常收口索引：先冲掉两流残片，再 Sync/Close 索引文件，只有全部成功才
// 原子落盘完成事实 index.done。任何一步失败都只锁存错误（+ index.error），
// 绝不制造成功完成记录，也不改变 raw/进程退出事实。
//
// 无论成功/失败都会注销同进程活跃状态（R9-A：异常关闭同族纳入）。
func (r *LogRecorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		err := r.err
		r.mu.Unlock()
		return err
	}
	for stream, buf := range r.buffers {
		if len(buf) > 0 {
			r.emitLocked(stream, buf)
		}
	}
	r.buffers = map[string][]byte{}
	if r.f != nil {
		if r.err == nil {
			if err := r.f.Sync(); err != nil {
				r.latchErrorOnlyLocked(fmt.Errorf("刷写索引: %w", err))
			}
		}
		err := r.f.Close()
		r.f = nil
		if err != nil {
			r.latchErrorOnlyLocked(fmt.Errorf("关闭索引: %w", err))
		}
	}
	if r.err == nil {
		r.writeCompletionLocked()
	}
	r.closed = true
	err := r.err
	r.mu.Unlock()
	// 先释放 writer 锁再拿 Store 的 live 表锁，避免与读侧形成锁序反转。
	r.unregister()
	return err
}

// writeCompletionLocked 写完成事实：tmp 写完再 rename，rename 失败 = 没有完成事实。
func (r *LogRecorder) writeCompletionLocked() {
	completion := logIndexCompletion{
		Format:       LogIndexCompletionFormat,
		Version:      LogIndexCompletionVersion,
		Records:      r.seq,
		LastSequence: r.seq,
		Size:         r.written,
	}
	data, err := json.Marshal(completion)
	if err != nil {
		r.latchErrorOnlyLocked(fmt.Errorf("编码索引完成记录: %w", err))
		return
	}
	data = append(data, '\n')
	tmp := filepath.Join(r.dir, LogIndexDoneFileName+".tmp")
	if err := r.fs.WriteFile(tmp, data, 0o600); err != nil {
		_ = r.fs.Remove(tmp)
		r.latchErrorOnlyLocked(fmt.Errorf("写索引完成记录: %w", err))
		return
	}
	if err := r.fs.Rename(tmp, filepath.Join(r.dir, LogIndexDoneFileName)); err != nil {
		_ = r.fs.Remove(tmp)
		r.latchErrorOnlyLocked(fmt.Errorf("落盘索引完成记录: %w", err))
	}
}

// emitLocked 追加一条完整记录（序号在锁内分配，保证两流共享的严格递增）。
func (r *LogRecorder) emitLocked(stream string, text []byte) {
	if r.f == nil || r.err != nil {
		return
	}
	r.seq++
	line, err := json.Marshal(protocol.LogLine{
		Sequence: r.seq,
		Stream:   stream,
		Text:     string(text),
		At:       r.now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		r.latchLocked(fmt.Errorf("编码索引记录: %w", err))
		return
	}
	r.writeLineLocked(line)
}

// writeLineLocked 写一条完整 JSON 行（单行一次写，读取侧靠结尾 LF 判定完整）。
//
// written 记录**实际落盘**的字节数（含失败前已写出的部分），供活跃快照给出稳定
// 前缀边界；正常路径永远停在完整行边界。
func (r *LogRecorder) writeLineLocked(line []byte) {
	if r.f == nil || r.err != nil {
		return
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	for len(buf) > 0 {
		n, err := r.f.Write(buf)
		if n > 0 {
			r.written += int64(n)
			buf = buf[n:]
		}
		if err != nil {
			r.latchLocked(fmt.Errorf("写索引记录: %w", err))
			return
		}
		if n == 0 {
			r.latchLocked(fmt.Errorf("写索引记录: %w", io.ErrShortWrite))
			return
		}
	}
}

// latchLocked 锁存首个故障、关掉索引文件并留下可检查的 index.error 标记。
func (r *LogRecorder) latchLocked(err error) {
	r.latchErrorOnlyLocked(err)
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
}

// latchErrorOnlyLocked 只锁存错误 + 写标记（Close 路径已自己关文件）。
func (r *LogRecorder) latchErrorOnlyLocked(err error) {
	if err == nil {
		return
	}
	if r.err == nil {
		r.err = err
	}
	// 标记只是诊断/重启后的证据，不是完成性的唯一来源：即使这里写失败，
	// 本次运行也不会写出 index.done（r.err != nil），同进程读侧还会看到活跃
	// writer 的锁存错误。两个方向都不会误报成功。
	_ = r.fs.WriteFile(filepath.Join(r.dir, LogIndexErrorFileName), []byte(r.err.Error()+"\n"), 0o600)
}

// unregister 注销同进程活跃状态（Close 之后必须不可再被当作活跃档案）。
func (r *LogRecorder) unregister() {
	if r == nil || r.store == nil {
		return
	}
	r.store.unregisterLive(r.key, r)
}

// LogQuery 是一次结构化日志查询的节点侧参数。
type LogQuery struct {
	// SinceSeconds 是时间窗（0 = 不限）；Now 是节点当前时间（零值取 time.Now）。
	SinceSeconds int
	Now          time.Time
	// TailLines 是条数上限（0 = 不限，上限 protocol.LogsTailLinesMax）。
	TailLines int
}

// LogArchive 是一次档案读取的真实结果（raw 尾部窗口 + 结构化记录）。
type LogArchive struct {
	// Attempt 是实际读到的 attempt（调用方必须原样回带）。
	Attempt int
	// Stdout / Stderr 是 raw 档案的尾部窗口（最多 maxLogRead 字节）；
	// More=true 表示档案还有更早内容未读入，回包必须传播 truncated。
	Stdout, Stderr         []byte
	StdoutMore, StderrMore bool
	// Lines 是按 sequence 升序的匹配记录（先 since 过滤、再取最新 tail 条）。
	Lines []protocol.LogLine
	// Omitted 表示有匹配记录因 tailLines 或读取缓存上界没有进入 Lines。
	Omitted bool
	// Partial 表示只返回了可证安全前缀：索引尾部半条、活跃快照之外的追加、
	// 或已关闭但没有有效完成事实的完整前缀。回包必须传播 truncated。
	Partial bool
}

// ReadLogs 读取 (taskID, attempt) 的真实档案：raw 尾部窗口 + 结构化记录。
//
// attempt=0 表示该逻辑 task 最近一次有档案的 attempt（与 LatestLogs 的选择一致）。
// found=false 且 err=nil = 这台节点没有那次执行的档案（no_local_archive）；
// err 匹配 ErrLogUnreadable = 档案在但记录不可读（logs_unreadable）。
func (s *Store) ReadLogs(taskID string, attempt int, q LogQuery) (LogArchive, bool, error) {
	dir, got, found, err := s.attemptDir(taskID, attempt)
	if err != nil {
		return LogArchive{}, found, err
	}
	if !found {
		return LogArchive{}, false, nil
	}
	arch := LogArchive{Attempt: got}
	arch.Stdout, arch.StdoutMore, err = readLog(filepath.Join(dir, LogsDirName, StdoutFileName))
	if err != nil {
		return LogArchive{}, true, fmt.Errorf("%w: 读取 stdout.log: %v", ErrLogUnreadable, err)
	}
	arch.Stderr, arch.StderrMore, err = readLog(filepath.Join(dir, LogsDirName, StderrFileName))
	if err != nil {
		return LogArchive{}, true, fmt.Errorf("%w: 读取 stderr.log: %v", ErrLogUnreadable, err)
	}
	lines, omitted, partial, err := s.readLogIndex(filepath.Join(dir, LogsDirName), q)
	if err != nil {
		return LogArchive{}, true, err
	}
	arch.Lines = lines
	arch.Omitted = omitted
	arch.Partial = partial
	return arch, true, nil
}

// attemptDir 解析要读的 attempt 目录（显式 attempt 必须有 meta；latest 取最大编号）。
func (s *Store) attemptDir(taskID string, attempt int) (dir string, got int, found bool, err error) {
	if attempt > 0 {
		dir = filepath.Join(s.Root, hexName(taskID), strconv.Itoa(attempt))
		if _, statErr := os.Lstat(filepath.Join(dir, MetaFileName)); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return "", 0, false, nil
			}
			return "", 0, true, fmt.Errorf("%w: 检查 attempt 元数据: %v", ErrLogUnreadable, statErr)
		}
		return dir, attempt, true, nil
	}
	base := filepath.Join(s.Root, hexName(taskID))
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, true, fmt.Errorf("%w: 枚举 attempt 目录: %v", ErrLogUnreadable, err)
	}
	latest := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 1 {
			continue
		}
		if n > latest {
			latest = n
		}
	}
	if latest == 0 {
		return "", 0, false, nil
	}
	return filepath.Join(base, strconv.Itoa(latest)), latest, true, nil
}

// readLogIndex 读取结构化索引（R2/R9-A）。
//
// 完整性判定：
//   - index.error 存在 = 写侧锁存过故障 → logs_unreadable；
//   - 同进程活跃 writer：先取健康/提交字节的原子快照。锁存错误 → unreadable；
//     健康时只扫描 committed 以内的一致前缀，快照之后多出的字节只令 truncated；
//     零记录是合法的「活跃真空」；
//   - 没有活跃 writer：必须有 index.done 且与实际内容（字节数/条数/最后序号）
//     逐项一致才算完整成功；没有完成事实时只允许把**可证安全前缀**（≥1 条完整
//     记录）作为 Partial 返回；header-only 无完成事实 = 崩溃残留，不可伪真空。
//
// 任何一条完整记录都必须通过序号连续、stream 闭集、text 上限、at=RFC3339Nano
// 校验；损坏/跳号/未知字段一律 unreadable，绝不静默跳过造「成功空日志」。
func (s *Store) readLogIndex(dir string, q LogQuery) ([]protocol.LogLine, bool, bool, error) {
	marker := filepath.Join(dir, LogIndexErrorFileName)
	if data, mErr := os.ReadFile(marker); mErr == nil {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = "写入者未提供原因"
		}
		return nil, false, false, fmt.Errorf("%w: 写入失败已锁存: %s", ErrLogUnreadable, msg)
	} else if !errors.Is(mErr, os.ErrNotExist) {
		return nil, false, false, fmt.Errorf("%w: 读取错误标记: %v", ErrLogUnreadable, mErr)
	}

	// 活跃写入者必须按同一 attempt 身份（logs 目录）匹配，且状态在 writer 锁内
	// 原子取得；committed=-1 表示没有可用的活跃快照。
	var (
		committed     int64 = -1
		liveUnhealthy bool
		liveErr       error
	)
	if s != nil {
		if live := s.liveLogRecorder(dir); live != nil {
			active, written, lErr := live.snapshot()
			if lErr != nil {
				liveUnhealthy, liveErr = true, lErr
			} else if active {
				committed = written
			}
		}
	}
	if liveUnhealthy {
		return nil, false, false, fmt.Errorf("%w: 活跃写入者已锁存错误（index.error 可能写失败）: %v",
			ErrLogUnreadable, liveErr)
	}

	path := filepath.Join(dir, LogIndexFileName)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, false, fmt.Errorf("%w: 缺少 %s（raw-only 档案不可结构化读取）",
			ErrLogUnreadable, LogIndexFileName)
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("%w: 打开 %s: %v", ErrLogUnreadable, LogIndexFileName, err)
	}
	defer f.Close()

	var src io.Reader = f
	if committed >= 0 {
		src = io.LimitReader(f, committed)
	}
	br := bufio.NewReaderSize(src, logIndexMaxLineBytes)
	headerLine, err := readIndexLine(br)
	if err != nil {
		return nil, false, false, fmt.Errorf("%w: 索引头不完整或超长: %v", ErrLogUnreadable, err)
	}
	var header logIndexHeader
	if err := decodeIndexJSON(headerLine, &header); err != nil {
		return nil, false, false, fmt.Errorf("%w: 索引头不是合法 JSON: %v", ErrLogUnreadable, err)
	}
	if header.Format != LogIndexFormat {
		return nil, false, false, fmt.Errorf("%w: 未知索引格式 %q", ErrLogUnreadable, header.Format)
	}
	if header.Version != LogIndexVersion {
		return nil, false, false, fmt.Errorf("%w: 不支持的索引版本 %d（支持 %d）",
			ErrLogUnreadable, header.Version, LogIndexVersion)
	}
	// offset 是已消费的完整行字节数（含每行结尾 LF），用于核对完成事实的 size。
	offset := int64(len(headerLine) + 1)

	var cutoff time.Time
	if q.SinceSeconds > 0 {
		now := q.Now
		if now.IsZero() {
			now = time.Now()
		}
		if sec := int64(q.SinceSeconds); sec <= logIndexMaxSinceSeconds {
			cutoff = now.Add(-time.Duration(sec) * time.Second)
		}
		// sec > logIndexMaxSinceSeconds：阈值早于 time.Duration 能表示的任何
		// 正常记录（约 292 年以前），等价于「不限时间」，保持非负输入语义，
		// 绝不因乘法回绕把阈值推到未来误返回空。
	}
	tail := q.TailLines
	if tail < 0 || tail > protocol.LogsTailLinesMax {
		return nil, false, false, fmt.Errorf("%w: 查询 tailLines 越界（%d）", ErrLogUnreadable, tail)
	}

	var (
		prev      int
		kept      []protocol.LogLine
		keptBytes int
		omitted   bool
		partial   bool
	)
	for {
		line, err := readIndexLine(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, errIndexPartialLine) {
			// 正在追加 / 崩溃在记录中间的末尾：只回完整前缀，并标 truncated。
			partial = true
			break
		}
		if err != nil {
			return nil, false, false, fmt.Errorf("%w: 索引行不可读: %v", ErrLogUnreadable, err)
		}
		offset += int64(len(line) + 1)
		var rec protocol.LogLine
		if err := decodeIndexJSON(line, &rec); err != nil {
			return nil, false, false, fmt.Errorf("%w: 记录不是合法 JSON: %v", ErrLogUnreadable, err)
		}
		if rec.Sequence != prev+1 {
			return nil, false, false, fmt.Errorf("%w: 记录序号不连续（上一条 %d，本条 %d）",
				ErrLogUnreadable, prev, rec.Sequence)
		}
		prev = rec.Sequence
		if rec.Stream != protocol.LogStreamStdout && rec.Stream != protocol.LogStreamStderr {
			return nil, false, false, fmt.Errorf("%w: 未知 stream %q", ErrLogUnreadable, rec.Stream)
		}
		if len(rec.Text) > logIndexMaxDecodedTextBytes {
			return nil, false, false, fmt.Errorf("%w: 记录 %d 的 text 解码后超过 %d 字节",
				ErrLogUnreadable, rec.Sequence, logIndexMaxDecodedTextBytes)
		}
		at, err := time.Parse(time.RFC3339Nano, rec.At)
		if err != nil {
			return nil, false, false, fmt.Errorf("%w: 记录 %d 的 at=%q 不是 RFC3339Nano: %v",
				ErrLogUnreadable, rec.Sequence, rec.At, err)
		}
		if !cutoff.IsZero() && at.Before(cutoff) {
			continue
		}
		kept = append(kept, rec)
		keptBytes += len(rec.Text)
		for len(kept) > 0 {
			overTail := tail > 0 && len(kept) > tail
			overCount := len(kept) > logIndexMaxCandidateRecords
			overBytes := keptBytes > logIndexMaxCandidateTextBytes && len(kept) > 1
			if !overTail && !overCount && !overBytes {
				break
			}
			keptBytes -= len(kept[0].Text)
			kept = kept[1:]
			omitted = true
		}
	}

	if committed >= 0 {
		// 活跃快照：committed 必须是完整行边界；快照之后追加的字节不算记录，
		// 只通过 truncated 表达「还有内容没进这次快照」。
		if offset != committed {
			return nil, false, false, fmt.Errorf("%w: 活跃索引长度与已提交字节不一致（读 %d / 提交 %d）",
				ErrLogUnreadable, offset, committed)
		}
		info, statErr := f.Stat()
		if statErr != nil {
			return nil, false, false, fmt.Errorf("%w: 检查活跃索引长度: %v", ErrLogUnreadable, statErr)
		}
		if info.Size() > committed {
			partial = true
		}
	} else {
		// 没有活跃写入者：完整成功只能由完成事实证明。
		completion, cErr := readLogIndexCompletion(dir)
		if cErr != nil {
			return nil, false, false, cErr
		}
		if completion != nil {
			if partial || offset != completion.Size ||
				prev != completion.Records || prev != completion.LastSequence {
				return nil, false, false, fmt.Errorf(
					"%w: 完成记录与索引内容不一致（size %d/%d records %d/%d lastSeq %d/%d）",
					ErrLogUnreadable, offset, completion.Size, prev, completion.Records,
					prev, completion.LastSequence)
			}
		} else if prev == 0 {
			return nil, false, false, fmt.Errorf(
				"%w: 索引只有头、没有完成记录且无活跃写入者（崩溃残留，不可当作空档案）",
				ErrLogUnreadable)
		} else {
			// 没有完成事实的完整前缀：可证安全（行完整/序号连续/字段合法），
			// 但不能冒充完整档案，必须让调用方标 truncated。
			partial = true
		}
	}

	if kept == nil {
		kept = []protocol.LogLine{}
	}
	return kept, omitted, partial, nil
}

// readLogIndexCompletion 读取并校验完成事实（不存在 = nil, nil；损坏 = unreadable）。
func readLogIndexCompletion(dir string) (*logIndexCompletion, error) {
	path := filepath.Join(dir, LogIndexDoneFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: 读取完成记录 %s: %v", ErrLogUnreadable, LogIndexDoneFileName, err)
	}
	var completion logIndexCompletion
	if err := decodeIndexJSON(bytes.TrimSuffix(data, []byte("\n")), &completion); err != nil {
		return nil, fmt.Errorf("%w: 完成记录不是合法 JSON: %v", ErrLogUnreadable, err)
	}
	if completion.Format != LogIndexCompletionFormat {
		return nil, fmt.Errorf("%w: 未知完成记录格式 %q", ErrLogUnreadable, completion.Format)
	}
	if completion.Version != LogIndexCompletionVersion {
		return nil, fmt.Errorf("%w: 不支持的完成记录版本 %d（支持 %d）",
			ErrLogUnreadable, completion.Version, LogIndexCompletionVersion)
	}
	if completion.Records < 0 || completion.LastSequence < 0 || completion.Size < 0 ||
		completion.Records != completion.LastSequence {
		return nil, fmt.Errorf("%w: 完成记录自身不自洽（records=%d lastSeq=%d size=%d）",
			ErrLogUnreadable, completion.Records, completion.LastSequence, completion.Size)
	}
	return &completion, nil
}

// readIndexLine 读一行（含结尾 LF）；EOF 处残留字节是「正在追加」而不是记录。
func readIndexLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err == nil {
		return line[:len(line)-1], nil
	}
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errIndexLineTooLong
	}
	if errors.Is(err, io.EOF) {
		if len(line) == 0 {
			return nil, io.EOF
		}
		return nil, errIndexPartialLine
	}
	return nil, err
}

// decodeIndexJSON 严格解析一行：未知字段 = 不认识的索引形态，明确不可读。
func decodeIndexJSON(line []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("一行里有多余内容")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("一行里有多余 JSON 值")
	}
	return nil
}
