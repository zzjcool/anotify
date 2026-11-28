package resultspool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNew_SweepsLeftoverDrafts：#629 第 2 项验收——上传中途退出时 defer Discard 不运行，
// tmp/draft-* 留在盘上；重启建 Store 只收**过了时间护栏**且形状是自己的那些。
func TestNew_SweepsLeftoverDrafts(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New(root) 失败: %v", err)
	}
	writeObject(t, s, "obj_keepme", []byte("published bytes"))
	tmp := tmpDir(s)

	// 崩溃遗留：只关句柄不 Discard，并把 mtime 拨到护栏之前。
	leakedDraft := func() string {
		d, err := s.CreateDraft(16 << 20)
		if err != nil {
			t.Fatalf("CreateDraft 失败: %v", err)
		}
		if _, err := d.Write([]byte("half-uploaded")); err != nil {
			t.Fatalf("草稿 Write 失败: %v", err)
		}
		if err := d.file.Close(); err != nil {
			t.Fatalf("关闭草稿句柄失败: %v", err)
		}
		old := time.Now().Add(-2 * draftSweepGrace)
		if err := os.Chtimes(d.path, old, old); err != nil {
			t.Fatalf("回拨草稿 mtime: %v", err)
		}
		return d.path
	}
	leaked := []string{leakedDraft(), leakedDraft()}

	// 仍在窗口内的草稿 = 可能是另一个进程正在写的（滚动重启/共用目录），必须留着。
	fresh, err := s.CreateDraft(16 << 20)
	if err != nil {
		t.Fatalf("CreateDraft (fresh) 失败: %v", err)
	}
	defer fresh.Discard()

	// 干扰项：形状不对的普通文件、形状对但**不是普通文件**的目录与符号链接、以及大写十六进制
	// （正则只认小写：宁可漏删也不猜）。
	keep := map[string]string{}
	writeFile := func(name, content string) {
		path := filepath.Join(tmp, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("写干扰文件 %s: %v", name, err)
		}
		old := time.Now().Add(-2 * draftSweepGrace)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("回拨 %s 的 mtime: %v", name, err)
		}
		keep[name] = path
	}
	writeFile("keep-me.txt", "not a draft")
	writeFile("draft-"+strings.Repeat("B", 32), "uppercase hex")
	if err := os.Mkdir(filepath.Join(tmp, "draft-"+strings.Repeat("a", 32)), 0o700); err != nil {
		t.Fatalf("建同名目录: %v", err)
	}
	keep["dir"] = filepath.Join(tmp, "draft-"+strings.Repeat("a", 32))
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside tmp"), 0o600); err != nil {
		t.Fatalf("写 tmp 外文件: %v", err)
	}
	// 用 c 而不是 b：文件名大小写不敏感的文件系统（macOS 默认）上 b 与上面的大写 B 是同一个名字。
	if err := os.Symlink(outside, filepath.Join(tmp, "draft-"+strings.Repeat("c", 32))); err != nil {
		t.Fatalf("建同名符号链接: %v", err)
	}
	keep["link"] = filepath.Join(tmp, "draft-"+strings.Repeat("c", 32))

	s2, err := New(root)
	if err != nil {
		t.Fatalf("重启 New(root) 失败: %v", err)
	}
	if warn := s2.SweepWarning(); warn != nil {
		t.Fatalf("启动清理报错（这些草稿本该删掉）: %v", warn)
	}
	for _, path := range leaked {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("重启后遗留草稿仍在: %s (err=%v)", filepath.Base(path), err)
		}
	}
	for name, path := range keep {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("清理误删了 %s: %v", name, err)
		}
	}
	if _, err := os.Lstat(fresh.path); err != nil {
		t.Errorf("窗口内的草稿被删了（另一进程正在写的那份）: %v", err)
	}

	f, size, err := s2.Open("obj_keepme")
	if err != nil {
		t.Fatalf("重启后读已发布对象失败: %v", err)
	}
	defer f.Close()
	if size != int64(len("published bytes")) {
		t.Fatalf("重启后对象大小 = %d, want %d", size, len("published bytes"))
	}
	d, err := s2.CreateDraft(1 << 20)
	if err != nil {
		t.Fatalf("重启后 CreateDraft 失败: %v", err)
	}
	if err := d.Discard(); err != nil {
		t.Fatalf("重启后 Discard 失败: %v", err)
	}
}

// TestSweepLeftoverDrafts_UnremovableIsWarning 钉住错误分级：单文件删不掉只是 sweepWarn
// （调用方 Warn），不许让 New 失败——否则一份删不掉的旧草稿会让产物上传/下载全部 503。
func TestSweepLeftoverDrafts_UnremovableIsWarning(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New(root) 失败: %v", err)
	}
	stale := filepath.Join(tmpDir(s), "draft-"+strings.Repeat("c", 32))
	if err := os.WriteFile(stale, []byte("stuck"), 0o600); err != nil {
		t.Fatalf("写卡住的草稿: %v", err)
	}
	old := time.Now().Add(-2 * draftSweepGrace)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("回拨 mtime: %v", err)
	}
	entries, err := os.ReadDir(tmpDir(s))
	if err != nil {
		t.Fatalf("read tmp: %v", err)
	}
	denied := func(string) error { return os.ErrPermission }
	warn := s.sweepLeftoverDrafts(entries, denied)
	if warn == nil || !errors.Is(warn, os.ErrPermission) {
		t.Fatalf("删不掉应汇总成可诊断的 warning, got %v", warn)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("注入失败后文件应原样留着: %v", err)
	}
}
