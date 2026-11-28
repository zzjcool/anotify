package resultspool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNew_SweepsLeftoverDrafts 是 #629 第 2 项的验收：进程在上传中途退出（kill -9 / OOM /
// 容器重启）时，handler 的 defer Discard 不会运行，tmp/draft-* 就永久留在盘上。
// 重启建 Store（New）必须把它收掉，且只收自己的形状。
func TestNew_SweepsLeftoverDrafts(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New(root) 失败: %v", err)
	}
	// 已发布对象不属于 tmp/ 清理范围，重启后必须还在。
	writeObject(t, s, "obj_keepme", []byte("published bytes"))

	// 两个「崩溃遗留」的草稿：只关句柄，不 Discard（模拟进程被杀）。
	var leaked []string
	for i := 0; i < 2; i++ {
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
		leaked = append(leaked, d.path)
	}

	// 干扰项：形状不对的普通文件、形状对但**不是普通文件**的目录与符号链接。
	// 清理只处置 `draft-<32 hex>` 的普通文件，其它一律不碰。
	tmp := tmpDir(s)
	notADraft := filepath.Join(tmp, "keep-me.txt")
	if err := os.WriteFile(notADraft, []byte("not a draft"), 0o600); err != nil {
		t.Fatalf("写干扰文件: %v", err)
	}
	draftDir := filepath.Join(tmp, "draft-"+strings.Repeat("a", 32))
	if err := os.Mkdir(draftDir, 0o700); err != nil {
		t.Fatalf("建同名目录: %v", err)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside tmp"), 0o600); err != nil {
		t.Fatalf("写 tmp 外文件: %v", err)
	}
	draftLink := filepath.Join(tmp, "draft-"+strings.Repeat("b", 32))
	if err := os.Symlink(outside, draftLink); err != nil {
		t.Fatalf("建同名符号链接: %v", err)
	}

	// 「重启」：同一个 root 再建一次 Store。
	s2, err := New(root)
	if err != nil {
		t.Fatalf("重启 New(root) 失败: %v", err)
	}
	for _, path := range leaked {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("重启后遗留草稿仍在: %s (err=%v)", filepath.Base(path), err)
		}
	}
	for _, path := range []string{notADraft, draftDir, draftLink, outside} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("清理误删了 %s: %v", filepath.Base(path), err)
		}
	}

	// 已发布对象一个字节都不许动，且新 Store 仍能正常读回它。
	f, size, err := s2.Open("obj_keepme")
	if err != nil {
		t.Fatalf("重启后读已发布对象失败: %v", err)
	}
	defer f.Close()
	if size != int64(len("published bytes")) {
		t.Fatalf("重启后对象大小 = %d, want %d", size, len("published bytes"))
	}

	// 新 Store 仍能正常开新草稿（清理没有把 tmp/ 弄坏）。
	d, err := s2.CreateDraft(1 << 20)
	if err != nil {
		t.Fatalf("重启后 CreateDraft 失败: %v", err)
	}
	if err := d.Discard(); err != nil {
		t.Fatalf("重启后 Discard 失败: %v", err)
	}
}
