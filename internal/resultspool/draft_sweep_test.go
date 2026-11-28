package resultspool

import (
	"context"
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

	// 50 分钟前 = issue 的字面验收项（旧护栏 1h 时这一档清不掉）。
	old := time.Now().Add(-50 * time.Minute)
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
		if err := os.Chtimes(d.path, old, old); err != nil {
			t.Fatalf("回拨草稿 mtime: %v", err)
		}
		return d.path
	}
	leaked := []string{leakedDraft(), leakedDraft()}

	fresh, err := s.CreateDraft(16 << 20) // 仍在窗口内 = 可能是另一进程正在写的，必须留着
	if err != nil {
		t.Fatalf("CreateDraft (fresh) 失败: %v", err)
	}
	defer fresh.Discard()

	// 干扰项：形状不对的普通文件、大写十六进制（正则只认小写）、以及不是普通文件的目录与符号链接。
	keep := map[string]string{}
	writeFile := func(name, content string) {
		path := filepath.Join(tmp, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("写干扰文件 %s: %v", name, err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("回拨 %s 的 mtime: %v", name, err)
		}
		keep[name] = path
	}
	writeFile("keep-me.txt", "not a draft")
	writeFile("draft-"+strings.Repeat("B", 32), "uppercase hex")
	dirPath := filepath.Join(tmp, "draft-"+strings.Repeat("a", 32))
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatalf("建同名目录: %v", err)
	}
	// 目录 mtime 也回拨：否则时间护栏先挡住「不是普通文件」，IsRegular 那段判断没人钉（空目录能被 os.Remove 删掉）。
	if err := os.Chtimes(dirPath, old, old); err != nil {
		t.Fatalf("回拨目录 mtime: %v", err)
	}
	keep["dir"] = dirPath
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside tmp"), 0o600); err != nil {
		t.Fatalf("写 tmp 外文件: %v", err)
	}
	// 符号链接自身的 mtime 回拨不了（os.Chtimes 跟随目标），所以它只证明「窗口内不动」，IsRegular 见下一条用例；文件名用 c，避开大小写不敏感文件系统上与上面大写 B 撞名。
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

// TestSweepLeftoverDrafts_NonRegularEntriesNeverRemoved 单独钉 IsRegular 那段判断：真符号链接回拨不了自身
// mtime（标准库没有 lchtimes），所以注入 DirEntry（它本来就是参数）。三条只差「是不是普通文件」。
func TestSweepLeftoverDrafts_NonRegularEntriesNeverRemoved(t *testing.T) {
	s := newSpool(t)
	old := time.Now().Add(-2 * draftSweepGrace)
	regular := "draft-" + strings.Repeat("f", 32)
	entries := []os.DirEntry{
		fakeEntry{name: "draft-" + strings.Repeat("d", 32), mode: os.ModeSymlink | 0o777, mod: old},
		fakeEntry{name: "draft-" + strings.Repeat("e", 32), mode: os.ModeDir | 0o700, mod: old},
		fakeEntry{name: regular, mode: 0o600, mod: old},
	}
	var removed []string
	recorder := func(path string) error { removed = append(removed, filepath.Base(path)); return nil }
	if err := s.sweepLeftoverDrafts(entries, recorder); err != nil {
		t.Fatalf("注入条目清理报错: %v", err)
	}
	if len(removed) != 1 || removed[0] != regular {
		t.Fatalf("remove 调用 = %v, want 只有 %s（守卫没了就会去删目录/符号链接）", removed, regular)
	}
}

// fakeEntry / fakeInfo 是 os.DirEntry 的最小替身，只喂名字、模式与 mtime。
type fakeEntry struct {
	name string
	mode os.FileMode
	mod  time.Time
}

func (f fakeEntry) Name() string               { return f.name }
func (f fakeEntry) IsDir() bool                { return f.mode.IsDir() }
func (f fakeEntry) Type() os.FileMode          { return f.mode.Type() }
func (f fakeEntry) Info() (os.FileInfo, error) { return fakeInfo(f), nil }

type fakeInfo fakeEntry

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return f.mod }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

// TestGCRunOnce_SweepsCrashLeftoverDrafts 是 #629 第 2 项的第二个验收面：#629 的复现是「CreateDraft →
// 写 4 MiB → 只关句柄不 Discard → 重启 → 一轮 GC」，而清理不能只挂在重启上（不重启就一直不扫）。
func TestGCRunOnce_SweepsCrashLeftoverDrafts(t *testing.T) {
	// 护栏必须是上传窗口（30s）量级：回到小时级等于把清理时机绑在「重启延迟 ≥ 1h」上。
	if draftSweepGrace > 2*time.Minute {
		t.Fatalf("draftSweepGrace = %s，超出上传窗口（30s）的 2 倍量级", draftSweepGrace)
	}
	s := newSpool(t)
	writeObject(t, s, "obj_keep", []byte("published bytes"))

	crashed := func(size int) string {
		d, err := s.CreateDraft(16 << 20)
		if err != nil {
			t.Fatalf("CreateDraft 失败: %v", err)
		}
		if _, err := d.Write(make([]byte, size)); err != nil {
			t.Fatalf("草稿 Write 失败: %v", err)
		}
		if err := d.file.Close(); err != nil {
			t.Fatalf("关闭草稿句柄失败: %v", err)
		}
		return d.path
	}
	stale := crashed(4 << 20)
	fresh := crashed(4 << 20)
	old := time.Now().Add(-2 * draftSweepGrace)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("回拨草稿 mtime: %v", err)
	}

	g, err := NewGC(&fakePurgeStore{}, s, GCOptions{Interval: time.Minute})
	if err != nil {
		t.Fatalf("NewGC 失败: %v", err)
	}
	rep, err := g.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("没有过期对象也没有删不掉的文件，RunOnce 不该报错: %v", err)
	}
	if rep != (GCReport{}) {
		t.Errorf("GCReport = %+v, want 零值（没有过期对象）", rep)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("一轮 GC 后崩溃遗留草稿仍在（清理时机还是绑在重启上）: %s (err=%v)", filepath.Base(stale), err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("护栏内的草稿被删了（另一进程可能正在写）: %v", err)
	}
	f, _, err := s.Open("obj_keep")
	if err != nil {
		t.Fatalf("GC 扫 tmp/ 动了已发布对象: %v", err)
	}
	f.Close()

	// 元数据不可用也照扫：遗留草稿与元数据无关，不该被一起卡住。
	if err := os.Chtimes(fresh, old, old); err != nil {
		t.Fatalf("回拨草稿 mtime: %v", err)
	}
	broken, err := NewGC(&fakePurgeStore{expireErr: errors.New("元数据不可用")}, s, GCOptions{Interval: time.Minute})
	if err != nil {
		t.Fatalf("NewGC 失败: %v", err)
	}
	if _, err := broken.RunOnce(context.Background()); err == nil {
		t.Fatal("ExpireObjects 报错时本轮应返回非 nil error")
	}
	if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("元数据不可用就没扫草稿（清理被绑在元数据健康上）: %v", err)
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
