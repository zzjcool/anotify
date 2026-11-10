package resultspool

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// 本文件覆盖契约 §5 的 1..9 条（真实文件系统，t.TempDir()）。

// newSpool 建一个临时 root 下的可用 Store。
func newSpool(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New(root) 失败: %v", err)
	}
	return s
}

// objectsDir / tmpDir 按契约布局拼出子目录（刻意不用 Store 的私有字段，
// 让测试断言的是对外可观察的目录布局）。
func objectsDir(s *Store) string { return filepath.Join(s.Root(), "objects") }
func tmpDir(s *Store) string     { return filepath.Join(s.Root(), "tmp") }

// writeObject 用草稿把 content 发布成 id 的托管对象。
func writeObject(t *testing.T, s *Store, id string, content []byte) {
	t.Helper()
	d, err := s.CreateDraft(int64(len(content)) + 16)
	if err != nil {
		t.Fatalf("CreateDraft 失败: %v", err)
	}
	if _, err := d.Write(content); err != nil {
		t.Fatalf("草稿 Write 失败: %v", err)
	}
	if err := d.Finalize(id); err != nil {
		t.Fatalf("Finalize(%s) 失败: %v", id, err)
	}
}

// dirEntries 返回目录下的条目名（目录不存在时 t.Fatal）。
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录 %s 失败: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// snapshotDir 递归记录目录内容（相对路径 / 权限 / 内容摘要）。
// 用于断言「错误路径没在 root 之外新建/修改/删除任何东西」。
func snapshotDir(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum := "-"
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			sum = hex.EncodeToString(h[:])
		}
		lines = append(lines, fmt.Sprintf("%s|%o|%s", rel, info.Mode().Perm(), sum))
		return nil
	})
	if err != nil {
		t.Fatalf("快照目录 %s 失败: %v", dir, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestNewCreatesPrivateRootDirs 覆盖 §5.1：root/objects/tmp 都是 0700 目录，
// 且 root 为空时 New 报错（不猜默认路径）。
func TestNewCreatesPrivateRootDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pool")
	s, err := New(root)
	if err != nil {
		t.Fatalf("New(%q) 失败: %v", root, err)
	}
	for _, dir := range []string{s.Root(), objectsDir(s), tmpDir(s)} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s 失败: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s 不是目录", dir)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s 权限 = %04o, want 0700", dir, got)
		}
	}
	if s.Root() != root {
		t.Errorf("Root() = %q, want %q", s.Root(), root)
	}

	// 重复 New 同一个 root 必须幂等（服务重启会再走一次初始化）。
	again, err := New(root)
	if err != nil {
		t.Fatalf("重复 New(%q) 失败: %v", root, err)
	}
	if again.Root() != root {
		t.Errorf("重复 New 的 Root() = %q, want %q", again.Root(), root)
	}

	// 空 root 不猜默认路径。
	for _, bad := range []string{"", "   "} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) 应报错（不猜默认路径）", bad)
		}
	}
}

// TestDraftFinalizePublishesObject 覆盖 §5.2：草稿 0600，Finalize 后对象 0600、
// 字节/sha/size 一致，tmp 下无残留。
func TestDraftFinalizePublishesObject(t *testing.T) {
	s := newSpool(t)
	content := []byte("hello artifact bytes\x00\x01\x02 tail")
	d, err := s.CreateDraft(int64(len(content)) + 8)
	if err != nil {
		t.Fatalf("CreateDraft 失败: %v", err)
	}

	// 草稿文件在 root/tmp 下、权限 0600。
	if names := dirEntries(t, tmpDir(s)); len(names) != 1 {
		t.Fatalf("tmp 下应恰好 1 个草稿，得到 %v", names)
	}
	draftInfo, err := os.Stat(filepath.Join(tmpDir(s), dirEntries(t, tmpDir(s))[0]))
	if err != nil {
		t.Fatalf("stat 草稿失败: %v", err)
	}
	if got := draftInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("草稿权限 = %04o, want 0600", got)
	}

	n, err := d.Write(content)
	if err != nil {
		t.Fatalf("草稿 Write 失败: %v", err)
	}
	if n != len(content) {
		t.Fatalf("Write 返回 %d, want %d", n, len(content))
	}
	if got := d.Size(); got != int64(len(content)) {
		t.Errorf("Size() = %d, want %d", got, len(content))
	}
	wantSum := sha256.Sum256(content)
	if got := d.SHA256(); got != hex.EncodeToString(wantSum[:]) {
		t.Errorf("SHA256() = %q, want %q", got, hex.EncodeToString(wantSum[:]))
	}

	const id = "obj_Test123abc"
	if err := d.Finalize(id); err != nil {
		t.Fatalf("Finalize(%q) 失败: %v", id, err)
	}
	objPath, err := s.ObjectPath(id)
	if err != nil {
		t.Fatalf("ObjectPath(%q) 失败: %v", id, err)
	}
	objInfo, err := os.Stat(objPath)
	if err != nil {
		t.Fatalf("stat 对象 %s 失败: %v", objPath, err)
	}
	if got := objInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("对象权限 = %04o, want 0600", got)
	}
	if objInfo.Size() != int64(len(content)) {
		t.Errorf("对象大小 = %d, want %d", objInfo.Size(), len(content))
	}
	got, err := os.ReadFile(objPath)
	if err != nil {
		t.Fatalf("读对象失败: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("对象字节 = %q, want %q", got, content)
	}
	if names := dirEntries(t, tmpDir(s)); len(names) != 0 {
		t.Errorf("Finalize 后 tmp 下不应有残留，得到 %v", names)
	}

	// 已发布草稿的 Discard 是 no-op：绝不删已发布对象。
	if err := d.Discard(); err != nil {
		t.Errorf("已发布草稿的 Discard 返回错误: %v", err)
	}
	if _, err := os.Stat(objPath); err != nil {
		t.Errorf("Discard 之后已发布对象消失了: %v", err)
	}
}

// TestDraftWriteOverLimit 覆盖 §5.3：超限 Write → ErrTooLarge、只写剩余额度
// （Size()==limit）、Discard 后无残留。
func TestDraftWriteOverLimit(t *testing.T) {
	s := newSpool(t)
	const limit = 8
	d, err := s.CreateDraft(limit)
	if err != nil {
		t.Fatalf("CreateDraft(%d) 失败: %v", limit, err)
	}
	n, err := d.Write([]byte("0123456789abcdef"))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超限 Write 错误 = %v, want ErrTooLarge", err)
	}
	if n != limit {
		t.Errorf("超限 Write 返回 %d, want %d（只写剩余额度）", n, limit)
	}
	if got := d.Size(); got != limit {
		t.Errorf("超限后 Size() = %d, want %d", got, limit)
	}

	// 额度用尽后任何一点字节都写不下去。
	n, err = d.Write([]byte("x"))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("额度用尽后 Write 错误 = %v, want ErrTooLarge", err)
	}
	if n != 0 {
		t.Errorf("额度用尽后写入了 %d 字节, want 0", n)
	}
	if got := d.Size(); got != limit {
		t.Errorf("额度用尽后 Size() = %d, want %d", got, limit)
	}

	if err := d.Discard(); err != nil {
		t.Fatalf("Discard 失败: %v", err)
	}
	if names := dirEntries(t, tmpDir(s)); len(names) != 0 {
		t.Errorf("Discard 后 tmp 下不应有残留，得到 %v", names)
	}
}

// TestDiscardOnlyRemovesOwnTemp 覆盖 §5.4：Discard 只删自己的临时文件，
// root 外的 decoy 一个字节都不动。
func TestDiscardOnlyRemovesOwnTemp(t *testing.T) {
	outside := t.TempDir()
	decoy := filepath.Join(outside, "decoy.txt")
	if err := os.WriteFile(decoy, []byte("decoy-must-survive"), 0o600); err != nil {
		t.Fatalf("造 decoy 失败: %v", err)
	}

	s := newSpool(t)
	d, err := s.CreateDraft(64)
	if err != nil {
		t.Fatalf("CreateDraft 失败: %v", err)
	}
	if _, err := d.Write([]byte("temp bytes")); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if err := d.Discard(); err != nil {
		t.Fatalf("Discard 失败: %v", err)
	}
	if names := dirEntries(t, tmpDir(s)); len(names) != 0 {
		t.Errorf("Discard 后 tmp 下不应有残留，得到 %v", names)
	}
	if err := d.Discard(); err != nil {
		t.Errorf("二次 Discard 应幂等返回 nil，得到 %v", err)
	}

	got, err := os.ReadFile(decoy)
	if err != nil {
		t.Fatalf("root 外 decoy 被删/不可读: %v", err)
	}
	if string(got) != "decoy-must-survive" {
		t.Errorf("root 外 decoy 内容被改: %q", got)
	}
}

// TestRejectsInvalidObjectID 覆盖 §5.5：非法 id 全覆盖，一律 ErrInvalidObjectID，
// 且 root 外没有任何新文件（decoy 目录快照前后一致）。
func TestRejectsInvalidObjectID(t *testing.T) {
	outside := t.TempDir()
	decoyDir := filepath.Join(outside, "decoy")
	if err := os.Mkdir(decoyDir, 0o700); err != nil {
		t.Fatalf("造 decoy 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(decoyDir, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("造 decoy 文件失败: %v", err)
	}
	before := snapshotDir(t, outside)

	s := newSpool(t)
	ids := []string{
		"",
		"../x",
		"obj_x/../../y",
		"/abs",
		"obj x",
		"obj..x",
		"obj\x00x",
		strings.Repeat("a", 65),
	}
	for _, id := range ids {
		if _, err := s.ObjectPath(id); !errors.Is(err, ErrInvalidObjectID) {
			t.Errorf("ObjectPath(%q) 错误 = %v, want ErrInvalidObjectID", id, err)
		}
		if _, _, err := s.Open(id); !errors.Is(err, ErrInvalidObjectID) {
			t.Errorf("Open(%q) 错误 = %v, want ErrInvalidObjectID", id, err)
		}
		if err := s.Remove(id); !errors.Is(err, ErrInvalidObjectID) {
			t.Errorf("Remove(%q) 错误 = %v, want ErrInvalidObjectID", id, err)
		}
		if err := s.Move(id, "obj_source"); !errors.Is(err, ErrInvalidObjectID) {
			t.Errorf("Move(%q, …) 错误 = %v, want ErrInvalidObjectID", id, err)
		}
		if err := s.Move("obj_source", id); !errors.Is(err, ErrInvalidObjectID) {
			t.Errorf("Move(…, %q) 错误 = %v, want ErrInvalidObjectID", id, err)
		}

		d, err := s.CreateDraft(64)
		if err != nil {
			t.Fatalf("CreateDraft 失败: %v", err)
		}
		if _, err := d.Write([]byte("abc")); err != nil {
			t.Fatalf("Write 失败: %v", err)
		}
		if err := d.Finalize(id); !errors.Is(err, ErrInvalidObjectID) {
			t.Errorf("Finalize(%q) 错误 = %v, want ErrInvalidObjectID", id, err)
		}
		if err := d.Discard(); err != nil {
			t.Errorf("Finalize 失败后 Discard(%q) 返回错误: %v", id, err)
		}
	}

	if after := snapshotDir(t, outside); after != before {
		t.Errorf("非法 id 触及了 root 之外的目录:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if names := dirEntries(t, tmpDir(s)); len(names) != 0 {
		t.Errorf("tmp 下不应有残留，得到 %v", names)
	}
	if names := dirEntries(t, objectsDir(s)); len(names) != 0 {
		t.Errorf("objects 下不应有残留，得到 %v", names)
	}
}

// TestOpenMissingSymlinkAndDirectory 覆盖 §5.6：不存在 → ErrNoSuchObject；
// 符号链接 → 报错且不返回 decoy 字节；目录 → 报错；正常对象可读回。
func TestOpenMissingSymlinkAndDirectory(t *testing.T) {
	s := newSpool(t)
	if _, _, err := s.Open("obj_missing"); !errors.Is(err, ErrNoSuchObject) {
		t.Fatalf("Open(不存在) 错误 = %v, want ErrNoSuchObject", err)
	}

	// 符号链接：objects/<id> 指向 root 外的 decoy。
	outside := t.TempDir()
	decoy := filepath.Join(outside, "decoy.bin")
	const decoyContent = "root-outside-decoy-must-not-leak"
	if err := os.WriteFile(decoy, []byte(decoyContent), 0o600); err != nil {
		t.Fatalf("造 decoy 失败: %v", err)
	}
	linkPath, err := s.ObjectPath("obj_symlink")
	if err != nil {
		t.Fatalf("ObjectPath(符号链接) 失败: %v", err)
	}
	if err := os.Symlink(decoy, linkPath); err != nil {
		t.Fatalf("造符号链接失败: %v", err)
	}
	f, _, err := s.Open("obj_symlink")
	if err == nil {
		f.Close()
		t.Fatal("Open(符号链接) 应报错")
	}
	if f != nil {
		f.Close()
		t.Fatal("Open(符号链接) 不应返回文件句柄（否则可能读到 decoy 字节）")
	}
	if got, err := os.ReadFile(decoy); err != nil || string(got) != decoyContent {
		t.Fatalf("root 外 decoy 被动过: content=%q err=%v", got, err)
	}

	// 目录对象：不是普通文件。
	dirPath, err := s.ObjectPath("obj_dir")
	if err != nil {
		t.Fatalf("ObjectPath(目录) 失败: %v", err)
	}
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatalf("造目录对象失败: %v", err)
	}
	if f, _, err := s.Open("obj_dir"); err == nil {
		f.Close()
		t.Fatal("Open(目录) 应报错")
	}

	// 对照组：正常对象能读回同样字节。
	const content = "legit object bytes"
	writeObject(t, s, "obj_real", []byte(content))
	f, size, err := s.Open("obj_real")
	if err != nil {
		t.Fatalf("Open(正常对象) 失败: %v", err)
	}
	defer f.Close()
	if size != int64(len(content)) {
		t.Errorf("Open 返回 size = %d, want %d", size, len(content))
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("读正常对象失败: %v", err)
	}
	if string(b) != content {
		t.Errorf("正常对象字节 = %q, want %q", b, content)
	}
}

// TestRemoveIdempotentAndRejectsEscape 覆盖 §5.7：Remove 幂等；越界 id → ErrInvalidObjectID
// 且 root 外 decoy 未被删。
func TestRemoveIdempotentAndRejectsEscape(t *testing.T) {
	s := newSpool(t)
	if err := s.Remove("obj_absent"); err != nil {
		t.Fatalf("Remove(不存在) 应返回 nil（幂等），得到 %v", err)
	}

	writeObject(t, s, "obj_gone", []byte("to be removed"))
	objPath, err := s.ObjectPath("obj_gone")
	if err != nil {
		t.Fatalf("ObjectPath 失败: %v", err)
	}
	if err := s.Remove("obj_gone"); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	if _, err := os.Stat(objPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove 后对象仍在: %v", err)
	}
	if err := s.Remove("obj_gone"); err != nil {
		t.Errorf("二次 Remove 应幂等返回 nil，得到 %v", err)
	}

	outside := t.TempDir()
	decoy := filepath.Join(outside, "decoy")
	if err := os.WriteFile(decoy, []byte("decoy-must-survive"), 0o600); err != nil {
		t.Fatalf("造 decoy 失败: %v", err)
	}
	for _, id := range []string{"../../decoy", "../decoy", "./decoy"} {
		if err := s.Remove(id); !errors.Is(err, ErrInvalidObjectID) {
			t.Errorf("Remove(%q) 错误 = %v, want ErrInvalidObjectID", id, err)
		}
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("root 外 decoy 被删了: %v", err)
	}
}

// TestMoveRenamesObject 覆盖 §5.8：正常改名终态内容/权限不变；
// from 不存在 → ErrNoSuchObject；非法 id → ErrInvalidObjectID；目标已存在 → 拒绝覆盖。
func TestMoveRenamesObject(t *testing.T) {
	s := newSpool(t)
	const content = "move me atomically"
	writeObject(t, s, "obj_from", []byte(content))

	fromPath, err := s.ObjectPath("obj_from")
	if err != nil {
		t.Fatalf("ObjectPath(from) 失败: %v", err)
	}
	toPath, err := s.ObjectPath("obj_to")
	if err != nil {
		t.Fatalf("ObjectPath(to) 失败: %v", err)
	}
	if err := s.Move("obj_from", "obj_to"); err != nil {
		t.Fatalf("Move 失败: %v", err)
	}
	if _, err := os.Stat(fromPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Move 后源对象仍在: %v", err)
	}
	info, err := os.Stat(toPath)
	if err != nil {
		t.Fatalf("Move 后目标对象不存在: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("Move 后目标权限 = %04o, want 0600（rename 不改权限）", got)
	}
	if info.Size() != int64(len(content)) {
		t.Errorf("Move 后目标大小 = %d, want %d", info.Size(), len(content))
	}
	if b, err := os.ReadFile(toPath); err != nil || string(b) != content {
		t.Fatalf("Move 后目标内容被改: content=%q err=%v", b, err)
	}

	if err := s.Move("obj_from", "obj_elsewhere"); !errors.Is(err, ErrNoSuchObject) {
		t.Errorf("Move(不存在, …) 错误 = %v, want ErrNoSuchObject", err)
	}
	if err := s.Move("../x", "obj_elsewhere"); !errors.Is(err, ErrInvalidObjectID) {
		t.Errorf("Move(非法 from) 错误 = %v, want ErrInvalidObjectID", err)
	}
	if err := s.Move("obj_to", "../y"); !errors.Is(err, ErrInvalidObjectID) {
		t.Errorf("Move(非法 to) 错误 = %v, want ErrInvalidObjectID", err)
	}
	if err := s.Move("obj_to", "obj_to"); err == nil {
		t.Error("Move(自己到自己, 目标已存在) 应报错（拒绝覆盖）")
	}
	if b, err := os.ReadFile(toPath); err != nil || string(b) != content {
		t.Fatalf("Move(自己到自己) 后对象被动过: content=%q err=%v", b, err)
	}

	// 目标已存在 → 拒绝覆盖，双方都不受影响。
	writeObject(t, s, "obj_taken", []byte("already published"))
	if err := s.Move("obj_to", "obj_taken"); err == nil {
		t.Fatal("Move 到已存在的对象应报错（不允许静默覆盖已登记对象）")
	}
	if b, err := os.ReadFile(toPath); err != nil || string(b) != content {
		t.Fatalf("拒绝覆盖后源对象被动过: content=%q err=%v", b, err)
	}
	takenPath, err := s.ObjectPath("obj_taken")
	if err != nil {
		t.Fatalf("ObjectPath(taken) 失败: %v", err)
	}
	if b, err := os.ReadFile(takenPath); err != nil || string(b) != "already published" {
		t.Fatalf("拒绝覆盖后目标对象被动过: content=%q err=%v", b, err)
	}
}

// TestFinalizeTwiceRejected 覆盖 §5.9：二次 Finalize → ErrNotCommitted，
// 且不得删掉（也不得替换）已发布对象。
func TestFinalizeTwiceRejected(t *testing.T) {
	s := newSpool(t)
	const content = "published once"
	d, err := s.CreateDraft(64)
	if err != nil {
		t.Fatalf("CreateDraft 失败: %v", err)
	}
	if _, err := d.Write([]byte(content)); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if err := d.Finalize("obj_first"); err != nil {
		t.Fatalf("首次 Finalize 失败: %v", err)
	}

	if err := d.Finalize("obj_second"); !errors.Is(err, ErrNotCommitted) {
		t.Fatalf("二次 Finalize 错误 = %v, want ErrNotCommitted", err)
	}
	firstPath, err := s.ObjectPath("obj_first")
	if err != nil {
		t.Fatalf("ObjectPath(first) 失败: %v", err)
	}
	if b, err := os.ReadFile(firstPath); err != nil || string(b) != content {
		t.Fatalf("二次 Finalize 破坏了已发布对象: content=%q err=%v", b, err)
	}
	secondPath, err := s.ObjectPath("obj_second")
	if err != nil {
		t.Fatalf("ObjectPath(second) 失败: %v", err)
	}
	if _, err := os.Stat(secondPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("二次 Finalize 不应创建新对象: %v", err)
	}

	// 已发布的草稿不可再写；Discard 仍是 no-op。
	if _, err := d.Write([]byte("more")); err == nil {
		t.Error("已发布草稿的 Write 应报错")
	}
	if err := d.Discard(); err != nil {
		t.Errorf("已发布草稿的 Discard 返回错误: %v", err)
	}
	if b, err := os.ReadFile(firstPath); err != nil || string(b) != content {
		t.Fatalf("Discard 之后已发布对象被动过: content=%q err=%v", b, err)
	}
	if names := dirEntries(t, tmpDir(s)); len(names) != 0 {
		t.Errorf("tmp 下不应有残留，得到 %v", names)
	}
}
