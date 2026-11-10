package capability

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
)

// artifactFileName 是缓存条目里的产物文件名。
const artifactFileName = "artifact.yaml"

// Entry 是一条缓存条目（一个 digest 一份不可变产物）。
type Entry struct {
	Digest   string
	Name     string
	Version  int
	Artifact *artifact.Artifact
	Path     string
}

// Cache 是 `<root>/<sha256-hex>/artifact.yaml` 的不可变产物缓存。
//
// 安全边界：
//   - 条目目录与文件都拒绝符号链接（写入时先 Lstat，临时文件 + rename 不跟随）；
//   - 读回时重算 digest，与目录名不一致 = 损坏/被替换，拒绝；
//   - 重启恢复只接受能通过完整校验的条目，坏条目进 error 列表而不是被信任。
type Cache struct {
	Root string
}

// NewCache 用缓存根目录构造（目录可尚不存在）。
func NewCache(root string) *Cache { return &Cache{Root: root} }

// Store 校验并原子写入一份产物，返回条目。
//
// 调用方必须已经验证过 digest 与 refs；这里再算一次 digest，保证
// 「目录名 / 文件内容 / artifact.Digest」三者一致。
func (c *Cache) Store(a *artifact.Artifact, digest string) (Entry, error) {
	if a == nil {
		return Entry{}, errors.New("capability: 产物为空")
	}
	if err := artifact.Validate(a); err != nil {
		return Entry{}, fmt.Errorf("capability: 产物校验失败: %w", err)
	}
	got, err := artifact.Digest(a)
	if err != nil {
		return Entry{}, fmt.Errorf("capability: 算 digest: %w", err)
	}
	if got != digest {
		return Entry{}, fmt.Errorf("capability: digest 不一致：参数 %s，内容 %s", digest, got)
	}
	root, err := c.resolvedRoot()
	if err != nil {
		return Entry{}, err
	}
	entryDir, err := entryDir(root, digest)
	if err != nil {
		return Entry{}, err
	}
	if info, err := os.Lstat(entryDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return Entry{}, fmt.Errorf("capability: 缓存条目 %s 是符号链接，拒绝写入", entryDir)
		}
		if !info.IsDir() {
			return Entry{}, fmt.Errorf("capability: 缓存条目 %s 不是目录", entryDir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Entry{}, fmt.Errorf("capability: 查看缓存条目 %s: %w", entryDir, err)
	}
	if err := os.MkdirAll(entryDir, 0o700); err != nil {
		return Entry{}, fmt.Errorf("capability: 创建缓存条目 %s: %w", entryDir, err)
	}
	data, err := artifact.MarshalYAML(a)
	if err != nil {
		return Entry{}, fmt.Errorf("capability: 编码产物: %w", err)
	}
	target := filepath.Join(entryDir, artifactFileName)
	tmp, err := os.CreateTemp(entryDir, "."+artifactFileName+".tmp-*")
	if err != nil {
		return Entry{}, fmt.Errorf("capability: 创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return Entry{}, fmt.Errorf("capability: 设置权限: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return Entry{}, fmt.Errorf("capability: 写入缓存: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return Entry{}, fmt.Errorf("capability: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Entry{}, fmt.Errorf("capability: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return Entry{}, fmt.Errorf("capability: rename: %w", err)
	}
	// 「部署后存在验证」：落盘内容必须能按同一个 digest 读回来。
	if _, err := c.Load(digest); err != nil {
		return Entry{}, fmt.Errorf("capability: 落盘后校验失败: %w", err)
	}
	return Entry{Digest: digest, Name: a.Metadata.Name, Version: a.Metadata.Version,
		Artifact: a, Path: target}, nil
}

// Load 读取并完整校验一个缓存条目（不存在/损坏/被替换都返回 error）。
func (c *Cache) Load(digest string) (Entry, error) {
	root, err := c.resolvedRoot()
	if err != nil {
		return Entry{}, err
	}
	dir, err := entryDir(root, digest)
	if err != nil {
		return Entry{}, err
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, fmt.Errorf("capability: 缓存条目不存在：%s", digest)
		}
		return Entry{}, fmt.Errorf("capability: 查看缓存条目: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return Entry{}, fmt.Errorf("capability: 缓存条目 %s 是符号链接，拒绝读取", dir)
	}
	if !dirInfo.IsDir() {
		return Entry{}, fmt.Errorf("capability: 缓存条目 %s 不是目录", dir)
	}
	target := filepath.Join(dir, artifactFileName)
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, fmt.Errorf("capability: 缓存条目 %s 缺 %s", digest, artifactFileName)
		}
		return Entry{}, fmt.Errorf("capability: 查看缓存文件: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Entry{}, fmt.Errorf("capability: 缓存文件 %s 是符号链接，拒绝读取", target)
	}
	if !info.Mode().IsRegular() {
		return Entry{}, fmt.Errorf("capability: 缓存文件 %s 不是普通文件", target)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return Entry{}, fmt.Errorf("capability: 读取缓存文件: %w", err)
	}
	a, got, err := artifact.Decode(data)
	if err != nil {
		return Entry{}, fmt.Errorf("capability: 缓存内容无效: %w", err)
	}
	if got != digest {
		return Entry{}, fmt.Errorf("capability: 缓存内容 digest %s 与请求 %s 不一致（损坏或被替换）", got, digest)
	}
	return Entry{Digest: digest, Name: a.Metadata.Name, Version: a.Metadata.Version,
		Artifact: a, Path: target}, nil
}

// Has 报告一个 digest 是否已经安全缓存（存在且校验通过）。
func (c *Cache) Has(digest string) bool {
	_, err := c.Load(digest)
	return err == nil
}

// Entries 扫描缓存根，返回全部**通过校验**的条目与坏条目错误。
//
// 重启恢复用：坏条目/符号链接/非 digest 目录都不被信任，放进 errors 供诊断，
// 不影响好条目上线。
func (c *Cache) Entries() ([]Entry, []error) {
	root, err := c.resolvedRoot()
	if err != nil {
		return nil, []error{err}
	}
	names, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("capability: 读取缓存根 %s: %w", root, err)}
	}
	var entries []Entry
	var problems []error
	for _, name := range names {
		if !hexDigestName(name.Name()) {
			continue
		}
		entry, err := c.Load(artifact.DigestPrefix + name.Name())
		if err != nil {
			problems = append(problems, err)
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Digest < entries[j].Digest })
	return entries, problems
}

// resolvedRoot 创建（如需要）并解析缓存根；根本身允许是符号链接（用户数据目录），
// 但所有条目路径都在解析后的根下面。
func (c *Cache) resolvedRoot() (string, error) {
	if strings.TrimSpace(c.Root) == "" {
		return "", errors.New("capability: 缓存根为空")
	}
	abs, err := filepath.Abs(c.Root)
	if err != nil {
		return "", fmt.Errorf("capability: 缓存根: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("capability: 创建缓存根 %s: %w", abs, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("capability: 解析缓存根 %s: %w", abs, err)
	}
	return resolved, nil
}

// entryDir 由 digest 推出条目目录；digest 形状非法直接拒绝（防路径注入）。
func entryDir(root, digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("capability: digest %q 不合法", digest)
	}
	hex := strings.TrimPrefix(digest, artifact.DigestPrefix)
	dir := filepath.Join(root, hex)
	// 双保险：结果必须仍在 root 下（digest 已限定十六进制，这里只是断言）。
	if filepath.Dir(dir) != root {
		return "", fmt.Errorf("capability: digest %q 推出越界路径", digest)
	}
	return dir, nil
}

func hexDigestName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
