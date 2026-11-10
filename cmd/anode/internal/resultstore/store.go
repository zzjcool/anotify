// Package resultstore 是节点上的系统结果目录：双流日志、托管产物副本、attempt
// 元数据与有限保留（GC）。
//
// 三条硬边界（capability.md §8.4 / ARTIFACT-HANDOFF §3）：
//
//  1. **只删自己创建并记录的路径**：GC 只处理 result root 下的 attempt 目录、以及
//     meta 里登记的 system work dir（必须在 work root 下）；用户 workDir 永不删除。
//  2. **独立副本**：导出产物先安全复制到系统结果目录，result.artifacts 指向副本，
//     不是用户 workDir 里的原文件；复制拒绝符号链接与越界路径。
//  3. **活动任务不删**：调用方把当前在跑的 taskId 传进来，GC 跳过它们。
//
// 元数据（meta.json）是重启后安全 GC 的唯一依据：目录名只是 hex 编码的 taskId，
// 真实 taskId / attempt / TTL / 系统目录都在 meta 里；读不懂的 meta 一律不删。
package resultstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// 导出上限引用 A1 共享常量（protocol/artifact_transfer.go），不在这里再写一份。
const (
	// MaxArtifactBytes 是单个导出文件上限（16 MiB）。
	MaxArtifactBytes = protocol.MaxArtifactTransferBytes
	// MaxAttemptArtifactBytes 是单个 attempt 的导出合计上限（64 MiB）。
	MaxAttemptArtifactBytes = protocol.MaxAttemptArtifactBytes
	// MaxArtifacts 是单个 attempt 最多的导出条目数。
	MaxArtifacts = protocol.MaxArtifactTransferItems
	// ManifestPathPrefix 是托管 manifest 的逻辑名前缀。逻辑名不带宿主目录前缀
	// （用户 workDir / 系统目录都不出现），下载 basename 与 API 展示都稳定。
	ManifestPathPrefix = "artifacts"
	// ExportRootPrefix 是每 attempt 系统导出目录的名字前缀（A2）。
	// 目录名带唯一随机后缀：两个任务共享同一 workDir、同 task 重试、旧 attempt
	// 短时重叠都不会写进同一个导出目录，节点只枚举自己 attempt 的那个。
	ExportRootPrefix = "export-"
)

// MetaFileName 是 attempt 元数据文件名。
const MetaFileName = "meta.json"

// LogsDirName / StdoutFileName / StderrFileName 是双流日志布局。
const (
	LogsDirName    = "logs"
	StdoutFileName = "stdout.log"
	StderrFileName = "stderr.log"
)

// Manifest 是托管副本的一条：逻辑相对路径 + 内容摘要 + 字节数。
type Manifest struct {
	// Path 是相对结果目录的逻辑名（`artifacts/<相对路径>`），不含宿主目录前缀。
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Record 是 attempt 元数据（重启后 GC / ops 读取的唯一事实源）。
type Record struct {
	TaskID     string `json:"taskId"`
	Node       string `json:"node"`
	Attempt    int    `json:"attempt"`
	Capability string `json:"capability"`
	Digest     string `json:"digest"`
	// TTL 是本次 attempt 钉住的结果保留期（秒）；0 = 永不清理。
	TTL int `json:"ttlSec"`
	// Phase 是终态；运行中为 "running"（崩溃后按 TTL 过期，不当作活动任务）。
	Phase string `json:"phase"`
	// SystemWorkDir 非空 = 这个目录是系统创建并拥有的（可被 GC 删除）。
	SystemWorkDir string `json:"systemWorkDir,omitempty"`
	// UserWorkDir 是用户指定的目录（仅为诊断记录，GC 永不删除）。
	UserWorkDir string `json:"userWorkDir,omitempty"`
	// Slot 是本次执行占用的物理槽位（A3；presence：缺失 = 旧记录/未知）。
	Slot *int `json:"slot,omitempty"`
	// LeaseID / PGID 是重启后核查「进程组是否真的回收」的持久事实（A3）。
	// PGID=0 = 尚未 spawn 或未知；Reaped 只在节点确认回收后置 true。
	LeaseID string `json:"leaseId,omitempty"`
	PGID    int    `json:"pgid,omitempty"`
	Reaped  bool   `json:"reaped,omitempty"`

	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt time.Time  `json:"finishedAt,omitempty"`
	Manifest   []Manifest `json:"manifest"`
}

// Store 是结果根 + 系统 work 根。
type Store struct {
	// Root 是结果目录根（<dataDir>/results）。
	Root string
	// WorkRoot 是系统创建的任务目录根（status.workRoot）。
	WorkRoot string

	// 同进程活跃日志索引写入者的私有登记（R9-A）：key = logs 目录绝对路径。
	// 只用于读侧区分「正常活跃的零记录」与「崩溃留下的 header-only」，不是
	// wire 协议、不跨进程、不落盘。
	liveMu sync.Mutex
	live   map[string]*LogRecorder
}

// registerLive 登记一个本进程打开的日志索引写入者。
func (s *Store) registerLive(key string, r *LogRecorder) {
	if s == nil || key == "" || r == nil {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.live == nil {
		s.live = make(map[string]*LogRecorder)
	}
	s.live[key] = r
}

// unregisterLive 注销写入者；只有当前登记的还是同一个 recorder 才删（重开竞态安全）。
func (s *Store) unregisterLive(key string, r *LogRecorder) {
	if s == nil || key == "" {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.live[key] == r {
		delete(s.live, key)
	}
}

// liveLogRecorder 返回登记在该 logs 目录上的活跃写入者（没有 = nil）。
func (s *Store) liveLogRecorder(key string) *LogRecorder {
	if s == nil || key == "" {
		return nil
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.live[key]
}

// New 构造（目录可尚不存在）。
func New(root, workRoot string) *Store { return &Store{Root: root, WorkRoot: workRoot} }

// Attempt 是一次 attempt 的结果目录句柄。
type Attempt struct {
	store  *Store
	dir    string
	record Record
	// openIndexPublished 是索引打开器的**私有测试 seam**（生产恒为 nil）：
	// openLogIndexFS 在把 recorder 登记进 Store 私有 live 表、即对读侧发布
	// 的那个边界调用它一次。回归测试借此确定性地钉住「已登记、初始化尚未
	// 完成」的窗口，证明发布发生在 writer 初始化锁内，不靠并发碰撞碰运气。
	// 它不是 wire 协议、不是公开构造参数、不参与生产行为。
	openIndexPublished func(*LogRecorder)
}

// Dir 返回 attempt 结果目录（绝对路径）。
func (a *Attempt) Dir() string { return a.dir }

// Record 返回当前元数据拷贝。
func (a *Attempt) Record() Record {
	out := a.record
	out.Manifest = append([]Manifest(nil), a.record.Manifest...)
	return out
}

// Begin 创建 attempt 结果目录（0700）、日志目录与初始 meta（phase=running）。
func (s *Store) Begin(rec Record) (*Attempt, error) {
	if strings.TrimSpace(rec.TaskID) == "" {
		return nil, errors.New("resultstore: taskId 为空")
	}
	if rec.Attempt < 1 {
		return nil, errors.New("resultstore: attempt 必须 ≥1")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	if rec.Phase == "" {
		rec.Phase = "running"
	}
	if rec.Manifest == nil {
		rec.Manifest = []Manifest{}
	}
	dir := filepath.Join(s.Root, hexName(rec.TaskID), strconv.Itoa(rec.Attempt))
	if err := mkdirOwned(s.Root, dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, LogsDirName), 0o700); err != nil {
		return nil, fmt.Errorf("resultstore: 创建日志目录: %w", err)
	}
	a := &Attempt{store: s, dir: dir, record: rec}
	if err := a.writeMeta(); err != nil {
		return nil, err
	}
	return a, nil
}

// mkdirOwned 逐级创建 root 下的目录，拒绝把已存在的符号链接当目录用。
func mkdirOwned(root, dir string) error {
	if !under(root, dir) {
		return fmt.Errorf("resultstore: %s 不在结果根 %s 下", dir, root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("resultstore: 创建结果根: %w", err)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("resultstore: 结果根 %s 不是普通目录", root)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("resultstore: 创建 attempt 目录: %w", err)
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("resultstore: attempt 目录 %s 不是普通目录", dir)
	}
	return nil
}

// OpenLogs 打开（或创建）双流日志文件，返回 stdout / stderr 句柄。
func (a *Attempt) OpenLogs() (stdout, stderr *os.File, err error) {
	stdout, err = a.OpenLogFile(StdoutFileName)
	if err != nil {
		return nil, nil, err
	}
	stderr, err = a.OpenLogFile(StderrFileName)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, err
	}
	return stdout, stderr, nil
}

// OpenLogFile 打开一条日志流（0600，截断重建）。
func (a *Attempt) OpenLogFile(name string) (*os.File, error) {
	if name != StdoutFileName && name != StderrFileName {
		return nil, fmt.Errorf("resultstore: 未知日志流 %q", name)
	}
	f, err := os.OpenFile(filepath.Join(a.dir, LogsDirName, name),
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("resultstore: 打开 %s: %w", name, err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Finish 写入终态与（可选的）托管 manifest。
func (a *Attempt) Finish(phase string, manifest []Manifest, finishedAt time.Time) error {
	a.record.Phase = phase
	a.record.FinishedAt = finishedAt
	if manifest == nil {
		manifest = []Manifest{}
	}
	a.record.Manifest = manifest
	return a.writeMeta()
}

// writeMeta 原子写 meta（0600）。
func (a *Attempt) writeMeta() error {
	data, err := json.Marshal(a.record)
	if err != nil {
		return fmt.Errorf("resultstore: 编码 meta: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.dir), ".meta-*")
	if err != nil {
		return fmt.Errorf("resultstore: 创建 meta 临时文件: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	target := filepath.Join(a.dir, MetaFileName)
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("resultstore: 落盘 meta: %w", err)
	}
	return nil
}

// SetSlot 持久记录本次执行的物理槽位（A3）。
func (a *Attempt) SetSlot(slot *int) error {
	a.record.Slot = slot
	return a.writeMeta()
}

// SetLeaseID 持久记录本次执行的 leaseId（重启/历史回收用）。
func (a *Attempt) SetLeaseID(id string) error {
	a.record.LeaseID = id
	return a.writeMeta()
}

// SetPGID 在 spawn 后把进程组 id 写进 meta（重启核查用）。
func (a *Attempt) SetPGID(pgid int) error {
	a.record.PGID = pgid
	return a.writeMeta()
}

// SetReaped 在 Finish 前记录回收事实（Finish 会一起落盘）。
func (a *Attempt) SetReaped(reaped bool) { a.record.Reaped = reaped }

// MarkReaped 持久记录「进程组是否已确认回收」。
func (a *Attempt) MarkReaped(reaped bool) error {
	a.record.Reaped = reaped
	return a.writeMeta()
}

// RunningRecords returns records whose task phase is running.
func (s *Store) RunningRecords() ([]Record, error) {
	return s.scanRecords(func(rec Record) bool { return rec.Phase == "running" })
}

// UnreapedRecords returns all known records not physically reaped, regardless
// of task phase. A terminal task can still own a live process group.
func (s *Store) UnreapedRecords() ([]Record, error) {
	return s.scanRecords(func(rec Record) bool { return !rec.Reaped })
}

func (s *Store) scanRecords(include func(Record) bool) ([]Record, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, taskEntry := range entries {
		if !taskEntry.IsDir() {
			continue
		}
		taskDir := filepath.Join(s.Root, taskEntry.Name())
		attempts, err := os.ReadDir(taskDir)
		if err != nil {
			continue
		}
		for _, attEntry := range attempts {
			if !attEntry.IsDir() {
				continue
			}
			rec, err := readMeta(filepath.Join(taskDir, attEntry.Name()))
			if err != nil || hexName(rec.TaskID) != taskEntry.Name() {
				continue
			}
			if include(rec) {
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

// MarkReaped 是恢复扫描用的便捷入口（按 taskId/attempt 更新 meta 的回收事实）。
func (s *Store) MarkReaped(taskID string, attempt int, reaped bool) error {
	dir := filepath.Join(s.Root, hexName(taskID), strconv.Itoa(attempt))
	metaPath := filepath.Join(dir, MetaFileName)
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return err
	}
	rec.Reaped = reaped
	if reaped && rec.Phase == "running" {
		rec.Phase = "failed"
		rec.FinishedAt = time.Now().UTC()
	}
	data, err := json.Marshal(&rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".meta-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, metaPath)
}

// NewExportDir 创建本 attempt 专属的系统导出目录（A2）。
//
// 位置：`<attempt 结果目录>/export-<16 hex>`，0700、绝对路径、唯一随机后缀。
// 它由节点创建并拥有（随 attempt 结果目录一起按 resultTtlSec 清理），绝不放在
// 用户 workDir 里、也不创建/删除用户工作目录内的专用目录。
func (a *Attempt) NewExportDir() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("resultstore: 生成导出目录随机后缀: %w", err)
	}
	dir := filepath.Join(a.dir, ExportRootPrefix+hex.EncodeToString(buf[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("resultstore: 创建导出目录 %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("resultstore: 导出目录 %s 不是普通目录", dir)
	}
	return dir, nil
}

// ExportDir 安全枚举本 attempt 的导出目录并托管复制（A2 的正式入口语义）。
//
// 规则：
//   - 目录必须是本 attempt 结果目录下直接子目录且带 ExportRootPrefix（不信任外部路径）；
//   - 只接受普通文件：符号链接、目录以外的特殊文件、越界路径一律明确报错；
//   - 单文件 16MiB / 合计 64MiB / 最多 32 项（不静默截断）；
//   - 逻辑名 = `artifacts/<相对导出目录的路径>`（不含宿主路径前缀）；
//   - **空目录是真空**：返回 []（不是错误，也不是"未实现"占位）。
func (a *Attempt) ExportDir(dir string) ([]Manifest, error) {
	manifest := []Manifest{}
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("resultstore: 导出目录为空")
	}
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) != filepath.Clean(a.dir) ||
		!strings.HasPrefix(filepath.Base(clean), ExportRootPrefix) {
		return nil, fmt.Errorf("resultstore: 导出目录 %s 不是本 attempt 的系统导出目录", dir)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, fmt.Errorf("resultstore: 检查导出目录: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("resultstore: 导出目录 %s 是符号链接，拒绝读取", clean)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("resultstore: 导出目录 %s 不是目录", clean)
	}
	dstRoot := filepath.Join(a.dir, ManifestPathPrefix)
	var (
		total   int64
		walkErr error
	)
	err = filepath.WalkDir(clean, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(clean, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			walkErr = fmt.Errorf("resultstore: 导出目录里有符号链接 %q（拒绝跟随/越界）", rel)
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			walkErr = fmt.Errorf("resultstore: 导出条目 %q 不是普通文件", rel)
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size := info.Size()
		if size > MaxArtifactBytes {
			walkErr = fmt.Errorf("resultstore: 导出文件 %q 有 %d 字节，超过单文件上限 %d",
				rel, size, MaxArtifactBytes)
			return walkErr
		}
		total += size
		if total > MaxAttemptArtifactBytes {
			walkErr = fmt.Errorf("resultstore: 导出合计超过 attempt 上限 %d", MaxAttemptArtifactBytes)
			return walkErr
		}
		if len(manifest) >= MaxArtifacts {
			walkErr = fmt.Errorf("resultstore: 导出条目超过上限 %d", MaxArtifacts)
			return walkErr
		}
		logical := filepath.ToSlash(filepath.Join(ManifestPathPrefix, rel))
		sha, copied, err := copyOwned(path, filepath.Join(dstRoot, rel))
		if err != nil {
			return err
		}
		if copied != size {
			walkErr = fmt.Errorf("resultstore: 导出文件 %q 复制时大小变化（%d → %d）", rel, size, copied)
			return walkErr
		}
		manifest = append(manifest, Manifest{Path: logical, SHA256: sha, Size: copied})
		return nil
	})
	if err != nil {
		if walkErr != nil {
			return nil, walkErr
		}
		return nil, fmt.Errorf("resultstore: 遍历导出目录: %w", err)
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
	a.record.Manifest = manifest
	return manifest, nil
}

// copyOwned 复制单个普通文件到托管目录：拒绝目标符号链接/非普通文件，边写边算 sha。
func copyOwned(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, fmt.Errorf("resultstore: 打开 %s: %w", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, fmt.Errorf("resultstore: 创建托管子目录: %w", err)
	}
	if info, err := os.Lstat(dst); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", 0, fmt.Errorf("resultstore: 托管目标 %s 是符号链接，拒绝写穿", dst)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("resultstore: 创建托管副本 %s: %w", dst, err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, MaxArtifactBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		return "", 0, fmt.Errorf("resultstore: 复制 %s: %w", src, copyErr)
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("resultstore: 关闭 %s: %w", dst, closeErr)
	}
	if n > MaxArtifactBytes {
		_ = os.Remove(dst)
		return "", 0, fmt.Errorf("resultstore: %s 超过单文件上限 %d", src, MaxArtifactBytes)
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Logs 读取 (taskID, attempt) 的精确本机档案。
//
// attempt=0 表示最新一次有档案的 attempt（logs_request 省略 attempt 时的语义）。
func (s *Store) Logs(taskID string, attempt int) (got int, stdout, stderr []byte, found bool, err error) {
	if attempt > 0 {
		dir := filepath.Join(s.Root, hexName(taskID), strconv.Itoa(attempt))
		if _, statErr := os.Lstat(filepath.Join(dir, MetaFileName)); statErr != nil {
			return 0, nil, nil, false, nil
		}
		stdout, _, err = readLog(filepath.Join(dir, LogsDirName, StdoutFileName))
		if err != nil {
			return 0, nil, nil, false, err
		}
		stderr, _, err = readLog(filepath.Join(dir, LogsDirName, StderrFileName))
		if err != nil {
			return 0, nil, nil, false, err
		}
		return attempt, stdout, stderr, true, nil
	}
	return s.LatestLogs(taskID)
}

// LatestLogs 读取某逻辑 task 最近一次有档案的 attempt 的双流日志。
//
// 返回 attempt 号、两段内容与「是否还有更多」标记。
func (s *Store) LatestLogs(taskID string) (attempt int, stdout, stderr []byte, found bool, err error) {
	base := filepath.Join(s.Root, hexName(taskID))
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil, nil, false, nil
	}
	if err != nil {
		return 0, nil, nil, false, err
	}
	attempts := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 1 {
			continue
		}
		attempts = append(attempts, n)
	}
	if len(attempts) == 0 {
		return 0, nil, nil, false, nil
	}
	sort.Ints(attempts)
	attempt = attempts[len(attempts)-1]
	dir := filepath.Join(base, strconv.Itoa(attempt))
	stdout, _, err = readLog(filepath.Join(dir, LogsDirName, StdoutFileName))
	if err != nil {
		return 0, nil, nil, false, err
	}
	stderr, _, err = readLog(filepath.Join(dir, LogsDirName, StderrFileName))
	if err != nil {
		return 0, nil, nil, false, err
	}
	return attempt, stdout, stderr, true, nil
}

// maxLogRead 是单流读取上限（更大的档案在 ops 层按帧上限截断，档案本身完整）。
const maxLogRead = 8 << 20

func readLog(path string) ([]byte, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxLogRead+1))
	if err != nil {
		return nil, false, err
	}
	return data, len(data) > maxLogRead, nil
}

// Collect 执行一次安全 GC：只删记录在案、已过期、非活动任务的系统目录/副本/日志。
//
// 返回被删除的路径（诊断用）。读不懂的 meta、越界路径、符号链接一律跳过并计入错误。
func (s *Store) Collect(now time.Time, active map[string]bool) ([]string, []error) {
	var removed []string
	var errs []error
	baseEntries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	for _, be := range baseEntries {
		if !be.IsDir() {
			continue
		}
		taskDir := filepath.Join(s.Root, be.Name())
		if info, err := os.Lstat(taskDir); err != nil || info.Mode()&os.ModeSymlink != 0 {
			errs = append(errs, fmt.Errorf("resultstore: %s 不是可清理的普通目录", taskDir))
			continue
		}
		entries, err := os.ReadDir(taskDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(taskDir, e.Name())
			rec, err := readMeta(dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("resultstore: 跳过 %s: %w", dir, err))
				continue
			}
			if active[rec.TaskID] {
				continue
			}
			if rec.TTL <= 0 {
				continue
			}
			reference := rec.FinishedAt
			if reference.IsZero() {
				reference = rec.CreatedAt
			}
			if now.Before(reference.Add(time.Duration(rec.TTL) * time.Second)) {
				continue
			}
			// 先删系统创建的 work dir（必须在 work root 下），再删结果副本。
			if rec.SystemWorkDir != "" {
				if err := removeOwned(s.WorkRoot, rec.SystemWorkDir); err != nil {
					errs = append(errs, err)
				} else {
					removed = append(removed, rec.SystemWorkDir)
				}
			}
			if err := removeOwned(s.Root, dir); err != nil {
				errs = append(errs, err)
				continue
			}
			removed = append(removed, dir)
		}
	}
	return removed, errs
}

// readMeta 严格读取 attempt 元数据（未知字段 = 不可信，不删）。
func readMeta(dir string) (Record, error) {
	data, err := os.ReadFile(filepath.Join(dir, MetaFileName))
	if err != nil {
		return Record{}, err
	}
	var rec Record
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return Record{}, err
	}
	if dec.More() {
		return Record{}, errors.New("meta 之后还有多余内容")
	}
	if rec.TaskID == "" || rec.Attempt < 1 {
		return Record{}, errors.New("meta 缺少 taskId/attempt")
	}
	if got := filepath.Base(dir); got != strconv.Itoa(rec.Attempt) {
		return Record{}, fmt.Errorf("meta attempt=%d 与目录 %s 不一致", rec.Attempt, got)
	}
	return rec, nil
}

// removeOwned 只删除 root 下、非符号链接、真实路径仍在 root 下的目录。
func removeOwned(root, target string) error {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(target) == "" {
		return errors.New("resultstore: 拒绝删除空路径")
	}
	rootClean := filepath.Clean(root)
	targetClean := filepath.Clean(target)
	if targetClean == rootClean || !under(rootClean, targetClean) {
		return fmt.Errorf("resultstore: 拒绝删除 %s（不在 %s 之下）", targetClean, rootClean)
	}
	info, err := os.Lstat(targetClean)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("resultstore: 拒绝删除符号链接 %s", targetClean)
	}
	if !info.IsDir() {
		return fmt.Errorf("resultstore: 拒绝删除非目录 %s", targetClean)
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootClean)
	if err != nil {
		resolvedRoot = rootClean
	}
	resolvedTarget, err := filepath.EvalSymlinks(targetClean)
	if err != nil {
		return err
	}
	if !under(filepath.Clean(resolvedRoot), filepath.Clean(resolvedTarget)) {
		return fmt.Errorf("resultstore: %s 解软链后逃出 %s", targetClean, rootClean)
	}
	return os.RemoveAll(targetClean)
}

// under 报告 target 是否是 root 的后代（root 本身不算）。
func under(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TTLFor 返回产物生命周期里钉住的结果保留期（秒）。
func TTLFor(a *artifact.Artifact) int {
	if a == nil {
		return artifact.DefaultResultTTLSec
	}
	return a.Spec.Lifecycle.EffectiveResultTTLSec()
}

func hexName(s string) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) * 2)
	for i := 0; i < len(s); i++ {
		b.WriteByte(digits[s[i]>>4])
		b.WriteByte(digits[s[i]&0x0f])
	}
	return b.String()
}
