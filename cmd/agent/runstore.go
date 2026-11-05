package main

// 本文件是节点的**执行档案库**（`<data-dir>/runs/`）：把每一次执行的输入与**完整**输出
// 落在跑它的那台机器上，事后用 `anotify-agent runs` / `show` 回看。
//
// # 为什么在节点上另存一份（而不是只有中枢的 task_logs）
//
// 中枢那张 `task_logs` 存的是**传输窗口**（每流最后 32 KiB），它的目的是「控制台上能看出
// 为什么炸了」。而「我这条 trace 到底收到了什么参数、脚本完整吐了什么、这是同一能力第几次
// 执行」是**排障与复核**问题，只有跑它的机器答得上来：
//
//   - params 里有用户的真实输入（网址、diff、密钥引用…）。上传到中枢等于把它们复制一份到
//     服务端；用户的原话是「这个只存在节点上」。
//   - 32 KiB 窗口是给网络与内存的让步，不是为了省钱。盘上留完整内容（单流 8 MiB）才能回答
//     「前面那一段到底打印了什么」。
//   - 同一 trace 同一能力可能执行多次（重试、扇出、循环）。每次的序号（seqInTrace）与派发
//     世代（attempt）只有配合**那次执行的输入输出**才有意义。
//
// # 落盘形状（路径即索引，不需要数据库）
//
//	<data-dir>/runs/<trace_id>/<capability>@<version>/<seqInTrace>-a<attempts>/
//	    input.json   这次执行收到了什么（params 原样，不脱敏）
//	    stdout.log   完整 stdout（单流上限 8 MiB，超限截尾 + 一行标记）
//	    stderr.log   完整 stderr（同上）
//	    meta.json    什么时候跑的、退出码、状态、summary、emit 次数
//
// 目录 0700 / 文件 0600（与 emit/、outbox/ 同一条纪律：这是用户自己的输入与输出）。
//
// # 纪律
//
//   - **写盘失败不得影响任务成败**：Save 只返回 error，调用方 Warn 一下就算完（与日志上传
//     同一条纪律，见 client.uploadLogs）。跑成功就是成功，磁盘满不该改结论。
//   - **不做脱敏**：能力跑在用户自己的机器上，params 本来就是用户自己给的；脱敏只会让
//     「当时到底传了什么」变成猜谜。
//   - **不碰 work/**：任务工作目录的保留策略是另一件事（成功即删），这里只写自己的目录。
//   - `meta.json` **最后写**：它的存在 = 这一次执行完整落了盘（中途崩掉只留半份日志，
//     读的人能一眼看出「这次没写完」）。
//
// # 保留策略
//
// 默认保留 7 天、最多 2000 个 run 目录（都可用环境变量覆盖），按时间 GC：
// `anotify-agent` 启动时跑一次，之后每小时一次（见 startRunGC）。GC 是**尽力而为**的
// 磁盘卫生工作，失败只 Warn，不阻塞任务。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// runsSubdirName 是数据目录下存放执行档案的子目录。
	runsSubdirName = "runs"

	// 一次执行落盘的四个文件名（见文件头）。
	runInputFile  = "input.json"
	runStdoutFile = "stdout.log"
	runStderrFile = "stderr.log"
	runMetaFile   = "meta.json"

	// runDirMode / runFileMode 是档案的权限：与 emit/、outbox/ 一致。
	// 输入里有用户参数、输出里有用户数据，只有本用户该看得到。
	runDirMode  os.FileMode = 0o700
	runFileMode os.FileMode = 0o600

	// runStreamMaxBytes 是**单流**落盘上限（8 MiB）。
	//
	// 与 maxLogStreamBytes（32 KiB，上传窗口）是两个量级、两个用途：那个是「网络上只传这么多」，
	// 这个是「盘上留全」。超限时保留**头部**（"截尾"）——档案的用途是复核「这次到底跑了什么」，
	// 开头那一段（版本信息、参数回显、日志前半）比尾部更有信息量；反过来 32 KiB 窗口保留尾部
	// 是因为那里写着错误（见 streamLog 的注释）。两个方向都有理由，不要互相统一。
	runStreamMaxBytes = 8 << 20

	// 保留策略的默认值与覆盖变量。
	defaultRunRetentionDays = 7
	defaultRunMaxCount      = 2000
	envRunsRetentionDays    = "ANOTIFY_RUNS_RETENTION_DAYS"
	envRunsMaxCount         = "ANOTIFY_RUNS_MAX_COUNT"

	// runGCInterval 是后台 GC 的周期（启动时另有一次）。
	runGCInterval = time.Hour

	// maxRunSegmentLen 是路径段（trace / capability）的长度上限：它们来自中枢，
	// 要当目录名用（见 runSegment）。
	maxRunSegmentLen = 128
	// runFallbackSegment 是「中枢给了个不能当目录名的值」时的替身（不丢记录）。
	runFallbackSegment = "_"
)

// ErrRunNotFound 表示要回看的那次执行不存在（CLI 据此退出码 1）。
var ErrRunNotFound = errors.New("run not found")

// runInput 是 input.json 的形状：这次执行**收到了什么**。
//
// params 是**原样**的（不脱敏、不裁剪）：它只落在这台机器上，用户要看的正是「当时到底传了什么」。
type runInput struct {
	TraceID    string         `json:"traceId"`
	TaskID     string         `json:"taskId"`
	Capability string         `json:"capability"`
	Version    int            `json:"version"`
	Attempt    int            `json:"attempt"`
	SeqInTrace int            `json:"seqInTrace"`
	Params     map[string]any `json:"params"`
	WorkDir    string         `json:"workdir"`
}

// runMeta 是 meta.json 的形状：这次执行**怎么了**。
//
// 时间用 RFC3339（毫秒精度）：它是给人看的（`anotify-agent show` 直接打印），
// 同一条记录在 meta.json 里就能读懂，不必再猜是秒还是毫秒。
type runMeta struct {
	TraceID    string `json:"traceId"`
	TaskID     string `json:"taskId"`
	Capability string `json:"capability"`
	Version    int    `json:"version"`
	Attempt    int    `json:"attempt"`
	SeqInTrace int    `json:"seqInTrace"`

	Status   string `json:"status"`   // succeeded / failed
	ExitCode int    `json:"exitCode"` // -1 = 拿不到（起不来 / 被信号杀 / 超时）
	Summary  string `json:"summary"`
	Error    string `json:"error,omitempty"`

	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	DurationMs int64  `json:"durationMs"`

	// Emits 是本任务累计 emit 次数（产出通道的用量，排查「没产出」时第一个要看数）。
	Emits int `json:"emits"`

	// 两个流的**真实**字节数（截断前）与截断标记：读的人必须能分清「就这么点」与「被截了」。
	StdoutBytes     int64 `json:"stdoutBytes"`
	StderrBytes     int64 `json:"stderrBytes"`
	StdoutTruncated bool  `json:"stdoutTruncated"`
	StderrTruncated bool  `json:"stderrTruncated"`
}

// runRecord 是一次执行要落盘的全部内容（由 client 侧在一次任务结束时组装）。
//
// Started / Finished 由 Save 格式化成 Meta.StartedAt / Meta.FinishedAt / Meta.DurationMs：
// 时间戳只有一个来源，调用方不必自己格式化（也就不会出现两种格式混在同一个目录里）。
type runRecord struct {
	Input  runInput
	Meta   runMeta
	Stdout string
	Stderr string

	Started  time.Time
	Finished time.Time
}

// runStore 是 `<data-dir>/runs` 的句柄。
//
// root 为空 = 不落盘（没有任何一条路径会成功），Save 直接返回错误由调用方 Warn。
type runStore struct {
	root      string
	retention time.Duration
	maxCount  int
	log       *slog.Logger
}

// newRunStore 建出档案库句柄：数据目录 + 环境变量覆盖的保留策略。
//
// **不**在这里建目录、也不在这里跑 GC：句柄可能在 standalone / 单测里构造（不需要磁盘副作用），
// 目录由第一次 Save 建（0700），GC 由 startRunGC 在节点启动时拉起。
//
// 环境变量的非法值一律回落默认并 Warn（而不是让 agent 起不来）：保留策略配错最多是多占点盘，
// 不值得让整台节点停摆。
func newRunStore(dataDir string, log *slog.Logger) *runStore {
	if log == nil {
		log = slog.Default()
	}
	root := ""
	if dir := strings.TrimSpace(dataDir); dir != "" {
		root = filepath.Join(dir, runsSubdirName)
	}
	return &runStore{
		root:      root,
		retention: time.Duration(envInt(envRunsRetentionDays, defaultRunRetentionDays, log)) * 24 * time.Hour,
		maxCount:  envInt(envRunsMaxCount, defaultRunMaxCount, log),
		log:       log,
	}
}

// envInt 读一个正整数环境变量；缺失/非法/非正 → 默认值（并在非法时 Warn 一次）。
//
// getenv 不注入（直接 os.Getenv）：这里只有进程级配置一条来源，与 emit 的目录一致；
// 单测用 t.Setenv 就能覆盖，比多一个参数更省事。
func envInt(name string, def int, log *slog.Logger) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Warn("环境变量取值非法，已回落默认值",
			"event", "agent.runs.env_invalid",
			"name", name, "value", raw, "default", def)
		return def
	}
	return n
}

// Root 是档案库根目录（空串 = 不落盘）。CLI 与排障从这里出发。
func (s *runStore) Root() string { return s.root }

// Save 把一次执行落盘，返回 run 目录的绝对路径。
//
// 三类错误都**不影响任务成败**（调用方只 Warn）：
//   - root 为空（没有数据目录）；
//   - 目录/文件建不出来（磁盘满、权限、runs 被一个同名文件占着）；
//   - 内容编不出来（params 里有无法 JSON 化的值——只可能是上游 bug）。
//
// 幂等：同一个 (trace, capability, version, seq, attempt) 重复落盘会整目录覆盖
// （重连/重放同一份结果时不会留下两份）。
func (s *runStore) Save(rec runRecord) (string, error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return "", errors.New("runs: 没有数据目录，跳过落盘")
	}
	rel, err := runDirRel(rec.Input)
	if err != nil {
		return "", err
	}
	dir, err := safeJoin(s.root, rel)
	if err != nil {
		return "", fmt.Errorf("runs: 路径 %q: %w", rel, err)
	}
	// MkdirAll 的 perm 只作用于**新建**的那几级；已存在的目录（root）保持原样。
	if err := os.MkdirAll(dir, runDirMode); err != nil {
		return "", fmt.Errorf("runs: 建目录 %s: %w", dir, err)
	}

	inputJSON, err := marshalIndent(rec.Input)
	if err != nil {
		return "", fmt.Errorf("runs: 编码 input.json: %w", err)
	}
	meta := rec.Meta
	// 身份字段兜底：调用方（client）会填全，但这里是落盘的最后一道——列表/回看要认得出
	// 「这是哪条 trace 的哪次执行」，缺了它就等于一条读不懂的记录。
	if meta.TraceID == "" {
		meta.TraceID = rec.Input.TraceID
	}
	if meta.TaskID == "" {
		meta.TaskID = rec.Input.TaskID
	}
	if meta.Capability == "" {
		meta.Capability = rec.Input.Capability
	}
	if meta.Version == 0 {
		meta.Version = rec.Input.Version
	}
	if meta.Attempt == 0 {
		meta.Attempt = rec.Input.Attempt
	}
	if meta.SeqInTrace == 0 {
		meta.SeqInTrace = rec.Input.SeqInTrace
	}
	// 时间戳只在 Save 里格式化一次（见 runRecord 的注释）。
	if !rec.Started.IsZero() {
		meta.StartedAt = rec.Started.UTC().Format(time.RFC3339Nano)
	}
	if !rec.Finished.IsZero() {
		meta.FinishedAt = rec.Finished.UTC().Format(time.RFC3339Nano)
	}
	if !rec.Started.IsZero() && !rec.Finished.IsZero() {
		meta.DurationMs = rec.Finished.Sub(rec.Started).Milliseconds()
	}
	metaJSON, err := marshalIndent(meta)
	if err != nil {
		return "", fmt.Errorf("runs: 编码 meta.json: %w", err)
	}

	// 顺序：两个流 → input → meta（**meta 最后** = 完整性标记，见文件头）。
	if err := writeRunFile(filepath.Join(dir, runStdoutFile), []byte(rec.Stdout)); err != nil {
		return "", err
	}
	if err := writeRunFile(filepath.Join(dir, runStderrFile), []byte(rec.Stderr)); err != nil {
		return "", err
	}
	if err := writeRunFile(filepath.Join(dir, runInputFile), inputJSON); err != nil {
		return "", err
	}
	if err := writeRunFile(filepath.Join(dir, runMetaFile), metaJSON); err != nil {
		return "", err
	}
	return dir, nil
}

// writeRunFile 写一个档案文件（0600、整份写、不做 rename）。
//
// 与 writeFileSync 的差别（**刻意不共用**）：那个函数在 forkLock 下写临时文件再 rename，
// 为的是「绝不出现半份内容」与「不踩 ETXTBSY」（产物文件要被执行）。档案文件两个都不需要：
//   - 它们从不被执行，不存在 ETXTBSY；
//   - 8 MiB 的日志在 forkLock 下写，会把别的任务的 startCmd（fork）一起卡住；
//   - 半份内容的判据是 meta.json 在不在（它最后写），不需要 rename 保序。
func writeRunFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, runFileMode)
	if err != nil {
		return fmt.Errorf("runs: 打开 %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("runs: 写 %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("runs: 关闭 %s: %w", path, err)
	}
	// 显式 chmod：O_CREATE 的 perm 会被 umask 削（022 → 0644），而 0600 是纪律不是建议。
	if err := os.Chmod(path, runFileMode); err != nil {
		return fmt.Errorf("runs: chmod %s: %w", path, err)
	}
	return nil
}

// marshalIndent 编一份给人看的 JSON（两空格缩进 + 末尾换行）。
//
// 不关 HTML 转义：档案是给人 `cat` 的，`<` 变成 \u003c 反而更难读；而这两份文件不进网络、
// 不进 HTML，不存在注入面。
func marshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// runDirRel 算一次执行的**相对**路径：<trace>/<capability>@<version>/<seq>-a<attempt>。
//
// 三段全部来自中枢（对端给的字符串），而它们要拼进路径：每一段都过 runSegment
// （拒绝分隔符 / ".." / 控制字符），与 workDirName 是同一条纪律。
//
// seqInTrace <= 0（旧中枢不带这个字段）时退到 attempt：每一次派发就是一次执行，
// 于是目录名仍然唯一且可排序。attempt 也没有（<=0）就退到 1。
func runDirRel(in runInput) (string, error) {
	trace := runSegment(in.TraceID)
	capability := runSegment(in.Capability)
	seq := in.SeqInTrace
	if seq <= 0 {
		seq = in.Attempt
	}
	if seq <= 0 {
		seq = 1
	}
	attempt := in.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	version := in.Version
	if version < 0 {
		version = 0
	}
	return fmt.Sprintf("%s/%s@%d/%d-a%d", trace, capability, version, seq, attempt), nil
}

// runSegment 把一个「要用作目录名的对端字符串」归一成安全的单段。
//
// 不合法的值**不丢记录**，换成一个固定的替身（`_`）：丢掉一条执行档案比名字难看严重得多
// （排障时最想看的那一次恰好没了）。真正会丢记录的错误只有「写不进去」。
func runSegment(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "." || s == ".." {
		return runFallbackSegment
	}
	if len(s) > maxRunSegmentLen {
		s = s[:maxRunSegmentLen]
	}
	bad := false
	for _, r := range s {
		if r == '/' || r == '\\' || r == 0 || r < 0x20 || r == 0x7f {
			bad = true
			break
		}
	}
	if bad {
		return runFallbackSegment
	}
	return s
}

// runEntry 是列表里的一条执行记录（列表只读 meta.json，不读两个流）。
type runEntry struct {
	// Ref 是给 `show` 用的三段相对路径：<trace>/<capability>@<version>/<seq>-a<attempt>。
	Ref        string
	TraceID    string
	Capability string
	Version    int
	Seq        int
	Attempt    int
	Dir        string
	// Started 是执行开始时间（meta.json 里的那个）；读不到就用目录 mtime。
	Started  time.Time
	Modified time.Time
	Meta     runMeta
}

// runFilter 是列表的过滤条件（零值 = 全部）。
type runFilter struct {
	TraceID    string // 精确匹配（空 = 不限）
	Capability string // 能力名（空 = 不限）
	// Version > 0 = 只留这个版本；<= 0 = 不限版本。
	//
	// 用 `> 0` 而不是 `>= 0`：零值 runFilter{} 必须是「什么都不过滤」——一个"空条件"
	// 把列表过滤成只剩 version=0 的行（而能力版本从 1 起，等于空列表）是最难查的那种坑。
	// 版本号本身从 1 起，所以「显式只要 0 版」不是一个需要表达的需求。
	Version int
	Limit   int // <= 0 = 不限条数
}

// completeRunDir 判断一个目录名是否是 run 目录（<seq>-a<attempt>）。
func parseRunDirName(name string) (seq, attempt int, ok bool) {
	seqPart, attemptPart, found := strings.Cut(name, "-a")
	if !found {
		return 0, 0, false
	}
	seq, err := strconv.Atoi(seqPart)
	if err != nil || seq < 0 {
		return 0, 0, false
	}
	attempt, err = strconv.Atoi(attemptPart)
	if err != nil || attempt < 0 {
		return 0, 0, false
	}
	return seq, attempt, true
}

// parseCapVersion 把 `<capability>@<version>` 拆成名字与版本。
func parseCapVersion(name string) (capability string, version int, ok bool) {
	capability, versionPart, found := strings.Cut(name, "@")
	if !found {
		return "", 0, false
	}
	version, err := strconv.Atoi(versionPart)
	if err != nil || version < 0 {
		return "", 0, false
	}
	return capability, version, true
}

// List 返回符合条件的执行记录，**按时间倒序**（最近的在前）。
//
// 遍历三层目录（trace → cap@ver → seq-aN）。读取是**容错**的：单个记录坏掉
// （缺 meta.json / JSON 坏了 / 名字不规则）只跳过它，不让整次列表失败——
// 磁盘上有一份坏记录不該让人看不到其余 999 条。
func (s *runStore) List(f runFilter) ([]runEntry, error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return nil, nil
	}
	traces, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 还没有任何执行 = 空列表，不是错误
		}
		return nil, fmt.Errorf("runs: 读 %s: %w", s.root, err)
	}

	entries := make([]runEntry, 0, 32)
	for _, traceDir := range traces {
		if !traceDir.IsDir() {
			continue
		}
		traceName := traceDir.Name()
		if f.TraceID != "" && traceName != f.TraceID {
			continue
		}
		caps, err := os.ReadDir(filepath.Join(s.root, traceName))
		if err != nil {
			continue
		}
		for _, capDir := range caps {
			if !capDir.IsDir() {
				continue
			}
			capability, version, ok := parseCapVersion(capDir.Name())
			if !ok {
				continue
			}
			if f.Capability != "" && capability != f.Capability {
				continue
			}
			if f.Version > 0 && version != f.Version {
				continue
			}
			runs, err := os.ReadDir(filepath.Join(s.root, traceName, capDir.Name()))
			if err != nil {
				continue
			}
			for _, runDir := range runs {
				if !runDir.IsDir() {
					continue
				}
				seq, attempt, ok := parseRunDirName(runDir.Name())
				if !ok {
					continue
				}
				info, err := runDir.Info()
				if err != nil {
					continue
				}
				e := runEntry{
					Ref:        traceName + "/" + capDir.Name() + "/" + runDir.Name(),
					TraceID:    traceName,
					Capability: capability,
					Version:    version,
					Seq:        seq,
					Attempt:    attempt,
					Dir:        filepath.Join(s.root, traceName, capDir.Name(), runDir.Name()),
					Modified:   info.ModTime(),
					Started:    info.ModTime(),
				}
				// meta.json 读得到就用它的时间/状态（列表要显示 STATUS）；读不到不是错误，
				// 只是这条记录在那两列上是未知（见 runEntry 的注释）。
				if meta, err := readRunMeta(e.Dir); err == nil {
					e.Meta = meta
					if ts, err := time.Parse(time.RFC3339Nano, meta.StartedAt); err == nil {
						e.Started = ts
					}
				}
				entries = append(entries, e)
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].Started.Equal(entries[j].Started) {
			return entries[i].Started.After(entries[j].Started)
		}
		// 同一时刻（秒级精度常见）按 ref 倒序：顺序确定，不会每次列出来都不一样。
		return entries[i].Ref > entries[j].Ref
	})
	if f.Limit > 0 && len(entries) > f.Limit {
		entries = entries[:f.Limit]
	}
	return entries, nil
}

// readRunMeta 读一个 run 目录里的 meta.json。
func readRunMeta(dir string) (runMeta, error) {
	var meta runMeta
	data, err := os.ReadFile(filepath.Join(dir, runMetaFile))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return runMeta{}, err
	}
	return meta, nil
}

// runDetail 是 `show` 的全部内容：一次执行的输入、meta 与两个流。
type runDetail struct {
	runEntry
	Input  runInput
	Stdout string
	Stderr string
	// StdoutTruncated / StderrTruncated 是**落盘时**的截断事实（来自 meta.json）：
	// 文件里本来就已经少了尾巴，读的人必须被告知（否则「输出就这么短」是错的结论）。
	StdoutTruncated bool
	StderrTruncated bool
}

// Load 读回一次执行的全部内容（ref = List 返回的那种三段相对路径）。
//
// ref 是**用户从命令行给的字符串**，所以逐段校验（safeJoin + 三段形状）：不做的话
// `anotify-agent show ../../etc/passwd` 就是一个任意文件读取。
func (s *runStore) Load(ref string) (*runDetail, error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return nil, ErrRunNotFound
	}
	clean := strings.Trim(strings.TrimSpace(ref), "/")
	parts := strings.Split(clean, "/")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: %q（形状应为 <trace>/<capability>@<version>/<seq>-a<n>）", ErrRunNotFound, ref)
	}
	for _, p := range parts {
		if err := checkRelPath(p); err != nil {
			return nil, fmt.Errorf("%w: %q（%v）", ErrRunNotFound, ref, err)
		}
	}
	dir, err := safeJoin(s.root, clean)
	if err != nil {
		return nil, fmt.Errorf("%w: %q（%v）", ErrRunNotFound, ref, err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, ref)
	}

	capability, version, ok := parseCapVersion(parts[1])
	if !ok {
		return nil, fmt.Errorf("%w: %q（第二段应为 <capability>@<version>）", ErrRunNotFound, ref)
	}
	seq, attempt, ok := parseRunDirName(parts[2])
	if !ok {
		return nil, fmt.Errorf("%w: %q（第三段应为 <seq>-a<attempt>）", ErrRunNotFound, ref)
	}

	d := &runDetail{runEntry: runEntry{
		Ref:        clean,
		TraceID:    parts[0],
		Capability: capability,
		Version:    version,
		Seq:        seq,
		Attempt:    attempt,
		Dir:        dir,
		Modified:   info.ModTime(),
		Started:    info.ModTime(),
	}}
	// meta 缺失/坏掉：**不算没找到**（记录确实在盘上，两个流也在），按零值展示。
	if meta, err := readRunMeta(dir); err == nil {
		d.Meta = meta
		d.StdoutTruncated = meta.StdoutTruncated
		d.StderrTruncated = meta.StderrTruncated
		if ts, err := time.Parse(time.RFC3339Nano, meta.StartedAt); err == nil {
			d.Started = ts
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, runInputFile)); err == nil {
		if err := json.Unmarshal(data, &d.Input); err != nil {
			// input.json 坏了：不拦着 show（流还是能看的），把身份字段留在 entry 上。
			d.Input = runInput{TraceID: parts[0], Capability: capability, Version: version, SeqInTrace: seq, Attempt: attempt}
		}
	} else {
		d.Input = runInput{TraceID: parts[0], Capability: capability, Version: version, SeqInTrace: seq, Attempt: attempt}
	}
	if data, err := os.ReadFile(filepath.Join(dir, runStdoutFile)); err == nil {
		d.Stdout = string(data)
	}
	if data, err := os.ReadFile(filepath.Join(dir, runStderrFile)); err == nil {
		d.Stderr = string(data)
	}
	return d, nil
}

// GC 按「保留天数 + 最大条数」清理档案，返回删掉的 run 目录数。
//
// 顺序是刻意的：
//  1. 先按**时间**删（超过 retention 的一律走，与条数无关）——这是用户能理解的语义
//     （"留 7 天"）；
//  2. 再按**条数**删（还剩太多就按最旧的继续删）——这是磁盘失控的兜底。
//
// 只用目录 mtime 判年龄（不读 meta.json）：mtime 恒存在、不会因为一条坏记录就漏删。
//
// 失败不中止：一个删不掉的目录（权限/占用）不该让其余 1999 个也留下。返回第一个错误，
// 让调用方 Warn 一次即可。
func (s *runStore) GC(now time.Time) (int, error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return 0, nil
	}
	dirs, err := s.scanRunDirs()
	if err != nil {
		return 0, err
	}
	// 最旧的在前：先删超期的，再从最旧的开始凑条数。
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].mod.Before(dirs[j].mod) })

	cutoff := now.Add(-s.retention)
	removed := 0
	deleted := make(map[string]struct{}, len(dirs))
	var firstErr error
	noteErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, d := range dirs {
		// retention <= 0 视为「不按时间删」（只按条数），不可能是「立刻全删」：
		// 后者一个人为的环境变量就能把档案库清空，代价太大。
		if s.retention > 0 && d.mod.Before(cutoff) {
			if err := os.RemoveAll(d.path); err != nil {
				noteErr(fmt.Errorf("runs: 删除过期档案 %s: %w", d.path, err))
				continue
			}
			deleted[d.path] = struct{}{}
			removed++
		}
	}

	// 条数兜底：dirs 是最旧的在前，从头部继续删到剩 maxCount 个为止。
	if s.maxCount > 0 {
		remaining := len(dirs) - removed
		for _, d := range dirs {
			if remaining <= s.maxCount {
				break
			}
			if _, gone := deleted[d.path]; gone {
				continue
			}
			if err := os.RemoveAll(d.path); err != nil {
				noteErr(fmt.Errorf("runs: 删除超量档案 %s: %w", d.path, err))
				continue
			}
			removed++
			remaining--
		}
	}

	// 收尾：删掉空掉的 cap@ver 与 trace 目录（否则 runs/ 下会留下成片的空壳）。
	pruneEmptyRunParents(s.root)
	return removed, firstErr
}

// runDirInfo 是 GC 扫描到的一个 run 目录。
type runDirInfo struct {
	path string
	mod  time.Time
}

// scanRunDirs 列出全部 run 目录（三层：trace → cap@ver → seq-aN）。
//
// 与 List 的差别：这里**只认形状**（不读 meta.json），因为 GC 要删的正是
// 「可能已经坏掉/读不出来」的那些。
func (s *runStore) scanRunDirs() ([]runDirInfo, error) {
	traces, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 还没有任何档案 = 没什么可清的
		}
		return nil, fmt.Errorf("runs: 读 %s: %w", s.root, err)
	}
	dirs := make([]runDirInfo, 0, 64)
	for _, traceDir := range traces {
		if !traceDir.IsDir() {
			continue
		}
		caps, err := os.ReadDir(filepath.Join(s.root, traceDir.Name()))
		if err != nil {
			continue
		}
		for _, capDir := range caps {
			if !capDir.IsDir() {
				continue
			}
			runs, err := os.ReadDir(filepath.Join(s.root, traceDir.Name(), capDir.Name()))
			if err != nil {
				continue
			}
			for _, runDir := range runs {
				if !runDir.IsDir() {
					continue
				}
				if _, _, ok := parseRunDirName(runDir.Name()); !ok {
					continue
				}
				info, err := runDir.Info()
				if err != nil {
					continue
				}
				dirs = append(dirs, runDirInfo{
					path: filepath.Join(s.root, traceDir.Name(), capDir.Name(), runDir.Name()),
					mod:  info.ModTime(),
				})
			}
		}
	}
	return dirs, nil
}

// pruneEmptyRunParents 删掉 runs/ 下已经空掉的两级目录（尽力而为，不报错）。
func pruneEmptyRunParents(root string) {
	traces, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, traceDir := range traces {
		if !traceDir.IsDir() {
			continue
		}
		tracePath := filepath.Join(root, traceDir.Name())
		caps, err := os.ReadDir(tracePath)
		if err != nil {
			continue
		}
		for _, capDir := range caps {
			if !capDir.IsDir() {
				continue
			}
			capPath := filepath.Join(tracePath, capDir.Name())
			if entries, err := os.ReadDir(capPath); err == nil && len(entries) == 0 {
				_ = os.Remove(capPath)
			}
		}
		if entries, err := os.ReadDir(tracePath); err == nil && len(entries) == 0 {
			_ = os.Remove(tracePath)
		}
	}
}

// startRunGC 在后台跑档案 GC：**启动时一次 + 每小时一次**，ctx 取消即退出。
//
// 为什么不是「每次任务结束后扫一遍」：GC 要 stat 整个 runs/ 树（2000 个目录），
// 挂在任务收尾路径上等于让每个任务替全库付一次代价；而档案的时效性以天计，
// 每小时一次完全够。
//
// 返回的 stop 用于测试与优雅退出（重复调用安全）。
func startRunGC(ctx context.Context, s *runStore, log *slog.Logger) (stop func()) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return func() {}
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOnce := func() {
			removed, err := s.GC(time.Now())
			if err != nil {
				log.Warn("runs GC 部分失败", "event", "agent.runs.gc_failed", "error", err.Error())
			}
			if removed > 0 {
				log.Info("runs GC 完成", "event", "agent.runs.gc", "removed", removed,
					"retention_days", int(s.retention.Hours()/24), "max_count", s.maxCount)
			}
		}
		runOnce() // 启动时一次：上一次进程可能是被 kill -9 的，攒了 8 天的档案得先清。
		ticker := time.NewTicker(runGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// runLog 是「保留**头部**、单流上限 limit 字节」的完整输出捕捉器（io.Writer）。
//
// 与 streamLog（保留尾部 32 KiB）刻意相反：档案的用途是复核「这次到底跑了什么」，
// 开头的版本信息/参数回显/日志前半比尾部更有信息量。超限时末尾追加一行标记，
// 说明「共多少字节、只留了前多少」——读的人必须能分清「就这么点」与「被截了」。
//
// Write 永远报告「全部写入」且不返回错误：短写/报错会让 os/exec 把子进程判成写失败
// （一次正常的任务被本地档案拖成 I/O 故障）。真实写入总量记在 total 里。
type runLog struct {
	mu    sync.Mutex
	buf   []byte
	limit int
	total int64
}

// newRunLog 造一个上限 limit 字节的头部窗口捕捉器。
func newRunLog(limit int) *runLog {
	if limit <= 0 {
		limit = runStreamMaxBytes
	}
	return &runLog{limit: limit}
}

// Write 实现 io.Writer：留头部，超出的字节直接丢弃（仍然计入 total）。
func (l *runLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total += int64(len(p))
	if room := l.limit - len(l.buf); room > 0 {
		if len(p) <= room {
			l.buf = append(l.buf, p...)
		} else {
			l.buf = append(l.buf, p[:room]...)
		}
	}
	return len(p), nil
}

// Total 是这个流**真实**写进来的字节数（截断前）。
func (l *runLog) Total() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// Truncated 表示窗口外还有内容被丢掉了。
func (l *runLog) Truncated() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total > int64(len(l.buf))
}

// String 返回落盘内容：窗口里的字节 + （截断时）末尾那一行标记。
//
// 不做 UTF-8 归一：档案是原样字节（二进制输出也照存），两个流本来就是「原始记录」，
// 在这里替用户改写字节只会让「我明明输出了这个」变成谜。
func (l *runLog) String() string {
	l.mu.Lock()
	buf, total := l.buf, l.total
	l.mu.Unlock()

	out := string(buf)
	if total > int64(len(buf)) {
		out += fmt.Sprintf("\n…（输出已截断：共 %d 字节，只保留前 %d 字节）\n", total, len(buf))
	}
	return out
}
