// Package resultspool 实现中枢自有的托管字节池：节点上传的产物字节在中枢侧的**唯一**落地副本。
//
// 这个包只做一件事——把「一次上传的字节」安全落到中枢自己的 root 下，再交给元数据登记去引用。
// 它不认识任务、attempt、数据库，也不读用户的 workDir、节点上的原文件或任何宿主路径：
// 托管副本是**中枢自己的字节**，只能从这里进出（下载只按对象 id 读回，绝不 fallback 到宿主文件）。
//
// 硬不变量（违反即安全事件，不是普通 bug）：
//   - root、root/objects、root/tmp 一律 0700；所有文件一律 0600（显式 Chmod 修 umask）；
//   - 写入顺序是「临时写（root/tmp，0600）→ 校验 → 原子 rename 到 objects/<id>」，
//     中途任何失败只清自己 root 内的临时文件，绝不留下半成品；
//   - 文件名只来自 store 铸造的对象 id（obj_<base62>）；id 先过白名单校验，
//     违规立即 ErrInvalidObjectID，绝不把外部字符串拼进路径落盘；
//   - 错误路径只关句柄、只删 root 内 own temp；**不读、不删** root 之外的任何路径
//     （节点原文件、用户 workDir、导出目录都不在处置范围内）；
//   - Open 只读且防符号链接（O_NOFOLLOW + Stat 复核 IsRegular），绝不因为「托管副本不在」
//     而回退去读元数据里的逻辑路径。
package resultspool

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// 对象池的目录布局（都在 root 之下，保证 Finalize/Move 是同文件系统内的 rename）。
const (
	objectsDirName = "objects"
	tmpDirName     = "tmp"
)

// 导出错误（调用方用 errors.Is 判定，不比较错误字符串）。
var (
	// ErrInvalidObjectID 表示 id 不合白名单规则（长度 3..64、首字符字母、只含 [A-Za-z0-9_]）。
	//
	// 这是**安全边界**：id 是唯一会拼进文件名的外部输入，先校验再落盘，
	// 任何 `..`、`/`、`\`、`.`、空白、控制字符、NUL 都在这里被拦住。
	ErrInvalidObjectID = errors.New("resultspool: invalid object id")
	// ErrTooLarge 表示写入超过本次草稿的字节上限（不写超限的部分）。
	ErrTooLarge = errors.New("resultspool: write exceeds limit")
	// ErrNotCommitted 表示草稿已经发布（或已清理）却又被要求发布：二次 Finalize 一律拒绝。
	ErrNotCommitted = errors.New("resultspool: draft not committed")
	// ErrNoSuchObject 表示托管副本不可用（不存在，或不是可读的普通文件）。
	//
	// 符号链接 / 非普通文件也归入这个哨兵：它们的后果相同——托管字节不可用，
	// 下载端统一当 410 artifact_expired 处理（真实原因留在错误消息里，由调用方 log.Warn 告警）。
	ErrNoSuchObject = errors.New("resultspool: object missing")
)

// Store 是一个已初始化好的托管字节池。
//
// 零值不可用：必须用 New 建（root/objects/tmp 的 0700 与存在性由 New 保证）。
type Store struct {
	root    string
	objects string
	tmp     string
}

// New 校验并建出 root/objects/tmp 三级目录（都是 0700），返回可用的 Store。
//
// root 为空 → 报错（**不猜默认路径**：托管池的位置必须是显式配置的事实，
// 猜一个默认值会在部署时把产物写到意料之外的盘上）。
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("resultspool: root 为空（不猜默认路径）")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resultspool: 解析 root %q: %w", root, err)
	}
	s := &Store{root: filepath.Clean(abs)}
	s.objects = filepath.Join(s.root, objectsDirName)
	s.tmp = filepath.Join(s.root, tmpDirName)
	for _, dir := range []string{s.root, s.objects, s.tmp} {
		if err := ensurePrivateDir(dir); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ensurePrivateDir 建目录并把权限显式收紧到 0700。
//
// MkdirAll 的 mode 会被进程 umask 削（只会更严，但也可能因为既有目录而完全不生效），
// 所以建完再 Chmod 一次，把「目录 0700」变成与 umask 无关的硬事实。
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("resultspool: 创建目录 %s: %w", dir, err)
	}
	// 先 Lstat **再** Chmod：Chmod 会跟随符号链接，若顺序反过来，把 root 配成一个
	// 指向别处的符号链接会先去改**链接目标**的权限、然后再拒绝使用这个目录
	// （「改了权限却不用它」）。先拒绝，就永远不会碰 root 之外的 inode。
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("resultspool: 检查目录 %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("resultspool: %s 不是普通目录（拒绝符号链接/特殊文件）", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("resultspool: 收紧目录权限 %s: %w", dir, err)
	}
	return nil
}

// Root 返回初始化后的绝对 root（不含尾部分隔符）。
func (s *Store) Root() string { return s.root }

// ObjectPath 把对象 id 映射成 objects/<id> 的绝对路径（**不检查存在性**）。
//
// id 非法 → ErrInvalidObjectID。这个函数是所有落盘路径的唯一入口：
// 只要它先校验，后面的 Open/Remove/Move 就不可能被拼出 root 外的路径。
func (s *Store) ObjectPath(id string) (string, error) {
	if err := validateObjectID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.objects, id), nil
}

// CreateDraft 在 root/tmp 下开一个 0600 临时文件，准备接收一次上传的字节。
//
// limit 是本次草稿的字节上限（<=0 视为配置错误：没有上限的草稿等于可以写满磁盘）。
// 草稿要么 Finalize 成为 objects/<id>，要么 Discard 只删自己的临时文件。
func (s *Store) CreateDraft(limit int64) (*Draft, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("resultspool: 草稿上限 %d 非法（必须为正）", limit)
	}
	f, path, err := s.newTempFile()
	if err != nil {
		return nil, err
	}
	return &Draft{store: s, file: f, path: path, limit: limit, digest: sha256.New()}, nil
}

// newTempFile 用 O_CREATE|O_EXCL|O_WRONLY + 0600 建一个名字随机的临时文件。
//
// 名字只由本包生成（不取自任何外部输入），O_EXCL 保证不会踩已有文件；
// 建完再显式 Chmod 0600，让 umask 无法影响「所有文件 0600」这条不变量。
func (s *Store) newTempFile() (*os.File, string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var buf [16]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, "", fmt.Errorf("resultspool: 生成临时文件名: %w", err)
		}
		path := filepath.Join(s.tmp, "draft-"+hex.EncodeToString(buf[:]))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("resultspool: 创建临时文件: %w", err)
		}
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			os.Remove(path)
			return nil, "", fmt.Errorf("resultspool: 收紧临时文件权限: %w", err)
		}
		return f, path, nil
	}
	return nil, "", errors.New("resultspool: 临时文件名随机冲突重试耗尽")
}

// Draft 是一次未发布的写入：root/tmp 下的 0600 临时文件 + 已写字节的 sha256 状态。
//
// 它不是并发安全的写入目标（一次上传一个 goroutine 顺序写），方法内部加锁只防
// 「defer Discard 与写循环」这类同 goroutine 的交叉调用与误用。
type Draft struct {
	mu        sync.Mutex
	store     *Store
	file      *os.File
	path      string
	limit     int64
	size      int64
	digest    hash.Hash
	finalized bool
}

// Write 追加字节；超过 limit 时只写剩余额度并返回 ErrTooLarge（**不写超限字节**）。
//
// 返回值遵循 io.Writer 契约：n 是真正写下去的字节数，ErrTooLarge 时 n < len(p)。
func (d *Draft) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return 0, fmt.Errorf("resultspool: 草稿不可写（已发布或已清理）")
	}
	allowed := d.limit - d.size
	if int64(len(p)) > allowed {
		var n int
		if allowed > 0 {
			written, err := d.file.Write(p[:allowed])
			n = written
			d.size += int64(written)
			if written > 0 {
				d.digest.Write(p[:written])
			}
			if err != nil {
				return n, fmt.Errorf("resultspool: 写入草稿: %w", err)
			}
		}
		return n, fmt.Errorf("%w: 已写 %d + %d 字节 > 上限 %d", ErrTooLarge, d.size, int64(len(p))-int64(n), d.limit)
	}
	n, err := d.file.Write(p)
	if n > 0 {
		d.size += int64(n)
		d.digest.Write(p[:n])
	}
	if err != nil {
		return n, fmt.Errorf("resultspool: 写入草稿: %w", err)
	}
	return n, nil
}

// Size 返回已成功写下的字节数。
func (d *Draft) Size() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.size
}

// SHA256 返回已写字节的 sha256（小写 hex，64 位）。
func (d *Draft) SHA256() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return hex.EncodeToString(d.digest.Sum(nil))
}

// Finalize 把草稿原子发布成 objects/<id>（同文件系统内的 rename，不复制字节）。
//
// 顺序：Sync（把写错误与落盘失败挡在发布之前）→ Close → rename。
// 二次调用 → ErrNotCommitted；id 非法 → ErrInvalidObjectID（草稿保持未发布，调用方仍可 Discard）。
// rename 之前的一切失败都不动已发布对象，只留一个仍可 Discard 的临时文件。
func (d *Draft) Finalize(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finalized {
		return fmt.Errorf("%w: 草稿已发布，拒绝二次发布", ErrNotCommitted)
	}
	if d.file == nil {
		return fmt.Errorf("%w: 草稿已清理", ErrNotCommitted)
	}
	target, err := d.store.ObjectPath(id)
	if err != nil {
		return err
	}
	if err := d.file.Sync(); err != nil {
		return fmt.Errorf("resultspool: 同步草稿: %w", err)
	}
	if err := d.file.Close(); err != nil {
		d.file = nil
		return fmt.Errorf("resultspool: 关闭草稿: %w", err)
	}
	d.file = nil
	if err := os.Rename(d.path, target); err != nil {
		return fmt.Errorf("resultspool: 原子发布对象 %s: %w", id, err)
	}
	d.path = ""
	d.finalized = true
	return nil
}

// Discard 丢掉草稿：关闭句柄并删掉**自己的**临时文件。
//
// 幂等，且已 Finalize 之后一律返回 nil——**绝不删已发布对象**（发布后的字节归元数据引用）。
// 删除前再复核一次临时文件确实在 root/tmp 之下：错误路径只处置自己 root 内的东西。
func (d *Draft) Discard() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finalized {
		return nil
	}
	var errs []error
	if d.file != nil {
		if err := d.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, fmt.Errorf("resultspool: 关闭草稿: %w", err))
		}
		d.file = nil
	}
	if d.path != "" {
		if under(d.store.tmp, d.path) {
			if err := os.Remove(d.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("resultspool: 删除临时文件: %w", err))
			}
		} else {
			errs = append(errs, fmt.Errorf("resultspool: 拒绝删除 %s（不在 %s 之下）", d.path, d.store.tmp))
		}
		d.path = ""
	}
	return errors.Join(errs...)
}

// Move 在 objects 里原子改名（objects/<from> → objects/<to>），用于「旧 ticket 过期后换新 id」。
//
// from 不存在/不是普通文件 → ErrNoSuchObject；任一 id 非法 → ErrInvalidObjectID；
// 目标已存在 → 报错（宁可让调用方走补偿，也不静默覆盖一个已被登记的托管对象）。
func (s *Store) Move(from, to string) error {
	src, err := s.ObjectPath(from)
	if err != nil {
		return err
	}
	dst, err := s.ObjectPath(to)
	if err != nil {
		return err
	}
	info, err := os.Lstat(src)
	if errors.Is(err, os.ErrNotExist) {
		return objectUnavailable(from, "源对象不存在")
	}
	if err != nil {
		return fmt.Errorf("resultspool: 检查源对象 %s: %w", from, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return objectUnavailable(from, "源对象不是普通文件")
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("resultspool: 目标对象 %s 已存在，拒绝覆盖", to)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resultspool: 检查目标对象 %s: %w", to, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("resultspool: 移动对象 %s → %s: %w", from, to, err)
	}
	return nil
}

// Open 只读打开 objects/<id>，返回文件句柄与大小。
//
// 防符号链接：O_NOFOLLOW 让「对象位置被换成符号链接」直接失败（不会顺着链接读到 root 外的
// decoy），再 Stat 复核 IsRegular 挡掉目录/设备/FIFO。不存在 → ErrNoSuchObject；
// 符号链接与非普通文件也包装 ErrNoSuchObject（托管字节不可用，调用方统一按已清理处理）。
func (s *Store) Open(id string) (*os.File, int64, error) {
	path, err := s.ObjectPath(id)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, objectUnavailable(id, "对象不存在")
		}
		return nil, 0, objectUnavailable(id, "打开失败: "+err.Error())
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("resultspool: 检查对象 %s: %w", id, err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, 0, objectUnavailable(id, "不是普通文件")
	}
	return f, info.Size(), nil
}

// Remove 删除 objects/<id>；不存在算成功（**幂等**，GC 会重复调）。
//
// id 非法 → ErrInvalidObjectID（绝不因为一个坏 id 去删 root 外的东西）；
// 目录对象 → 报错（不做递归删除）；符号链接只删链接本身，不碰链接目标。
func (s *Store) Remove(id string) error {
	path, err := s.ObjectPath(id)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resultspool: 检查对象 %s: %w", id, err)
	}
	if info.IsDir() {
		return fmt.Errorf("resultspool: 拒绝删除目录对象 %s", id)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resultspool: 删除对象 %s: %w", id, err)
	}
	return nil
}

// objectUnavailable 把「托管副本不可用」的各种原因统一包装到 ErrNoSuchObject 上。
//
// 契约只把「不存在」钉成 ErrNoSuchObject；符号链接/非普通文件是同一后果（托管字节读不到），
// 所以同样包装它，让下载端能用 errors.Is 统一映射成 410 artifact_expired，
// 同时把真实原因留在消息里（由调用方 log.Warn 告警，不静默吞掉）。
func objectUnavailable(id, reason string) error {
	return fmt.Errorf("%w: %s（%s）", ErrNoSuchObject, id, reason)
}

// validateObjectID 校验对象 id 白名单：长度 3..64、首字符是字母、其余只允许 [A-Za-z0-9_]。
//
// 字符白名单天然排除了 `/`、`\`、`.`、空白、控制字符与 NUL，所以通过校验的 id
// 拼到 objects/ 之后不可能逃出 root（`..` 需要 `.`，绝对路径需要首字符 `/`）。
func validateObjectID(id string) error {
	if len(id) < 3 || len(id) > 64 {
		return fmt.Errorf("%w: 长度 %d 不在 3..64", ErrInvalidObjectID, len(id))
	}
	if !isASCIILetter(id[0]) {
		return fmt.Errorf("%w: 首字符必须是字母", ErrInvalidObjectID)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if isASCIILetter(c) || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return fmt.Errorf("%w: 第 %d 个字符 %q 不在 [A-Za-z0-9_]", ErrInvalidObjectID, i, string(c))
	}
	return nil
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// under 报告 target 是否是 root 的后代（root 本身不算）。
func under(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
