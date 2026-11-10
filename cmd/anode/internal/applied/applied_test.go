package applied

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

func testVars() map[string]string { return map[string]string{"SOURCE_ROOT": "/srv/src"} }

func testLimits() protocol.Limits {
	return protocol.Limits{Total: 4, PerCapability: map[string]int{"reviewer": 2}}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	cfg, err := store.Save("rev-7", testVars(), testLimits())
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if cfg.Revision != "rev-7" {
		t.Fatalf("revision = %q", cfg.Revision)
	}
	wantHash, err := protocol.MachineConfigHash(testVars(), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hash != wantHash {
		t.Fatalf("hash = %s, want protocol 实现 %s", cfg.Hash, wantHash)
	}
	loaded, present, err := store.Load()
	if err != nil || !present {
		t.Fatalf("Load() = present=%v err=%v", present, err)
	}
	if loaded.Revision != cfg.Revision || loaded.Hash != cfg.Hash || loaded.Vars["SOURCE_ROOT"] != "/srv/src" {
		t.Fatalf("Load = %+v, want %+v", loaded, cfg)
	}
	if loaded.Limits.Total != 4 || loaded.Limits.PerCapability["reviewer"] != 2 {
		t.Fatalf("loaded limits = %+v", loaded.Limits)
	}
}

func TestSaveAtomicMode0600NoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if _, err := store.Save("r1", testVars(), protocol.Limits{Total: 1}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("applied 文件权限 = %o, want 600", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != protocol.AppliedFileName {
			t.Fatalf("残留临时文件: %s", entry.Name())
		}
	}
	// 覆盖写仍保持 0600。
	if _, err := store.Save("r2", testVars(), protocol.Limits{Total: 2}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("覆盖写后权限 = %o", info.Mode().Perm())
	}
}

func TestLoadColdStart(t *testing.T) {
	store := NewStore(t.TempDir())
	_, present, err := store.Load()
	if err != nil {
		t.Fatalf("冷启动不应报错: %v", err)
	}
	if present {
		t.Fatal("冷启动 Present 必须 false（不造旧在线事实）")
	}
}

func TestLoadStrictUnknownField(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if _, err := store.Save("r1", testVars(), protocol.Limits{Total: 1}); err != nil {
		t.Fatal(err)
	}
	mutateApplied(t, store.Path(), func(doc map[string]any) {
		doc["bogus"] = true
	})
	if _, _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("未知字段应失败: %v", err)
	}
}

func TestLoadHashMismatch(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if _, err := store.Save("r1", testVars(), protocol.Limits{Total: 1}); err != nil {
		t.Fatal(err)
	}
	mutateApplied(t, store.Path(), func(doc map[string]any) {
		doc["vars"].(map[string]any)["SOURCE_ROOT"] = "/tmp/evil"
	})
	_, _, err := store.Load()
	if err == nil || !strings.Contains(err.Error(), "hash 校验失败") {
		t.Fatalf("坏 hash 应失败: %v", err)
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	victim := filepath.Join(t.TempDir(), "victim.json")
	if err := os.WriteFile(victim, []byte(`{"revision":"x","hash":"","vars":{},"limits":{"total":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, store.Path()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("符号链接 applied 应拒绝: %v", err)
	}
}

func TestSaveReplacesSymlinkSafely(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	victim := filepath.Join(t.TempDir(), "victim.json")
	original := []byte("keep-me")
	if err := os.WriteFile(victim, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, store.Path()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save("r1", testVars(), protocol.Limits{Total: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// rename 替换符号链接本身，受害者文件内容不变。
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("符号链接目标被改写: %q", got)
	}
	info, err := os.Lstat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("applied 路径应变成普通文件，实际 %v", info.Mode())
	}
}

func TestSaveRejectsDirectoryPath(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := os.Mkdir(store.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save("r1", testVars(), protocol.Limits{Total: 1}); err == nil {
		t.Fatal("applied 路径是目录时应失败")
	}
}

func TestNsDefaultsNotInAppliedFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if _, err := store.Save("machine-rev", testVars(), testLimits()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 4 {
		t.Fatalf("applied 顶层键 = %v, 只允许 revision/hash/vars/limits", keys(doc))
	}
	for _, forbidden := range []string{"namespace", "namespaceVars", "namespaceVarsRevision"} {
		if _, ok := doc[forbidden]; ok {
			t.Fatalf("applied 不应包含 %q", forbidden)
		}
	}
	// 结构上也没有入口能写 ns：Save 只接受 vars/limits。
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mutateApplied(t *testing.T, path string, mutate func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	mutate(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}
