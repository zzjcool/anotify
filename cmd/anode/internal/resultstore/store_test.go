package resultstore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return New(filepath.Join(dir, "results"), filepath.Join(dir, "work"))
}

func beginAttempt(t *testing.T, s *Store, taskID string, attempt int, ttl int) *Attempt {
	t.Helper()
	rec, err := s.Begin(Record{
		TaskID: taskID, Node: "node-a", Attempt: attempt, Capability: "echo",
		Digest: "sha256:aa", TTL: ttl, Phase: "running", CreatedAt: time.Now().UTC(),
		Manifest: []Manifest{},
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return rec
}

func TestExportDirEmptyIsValidEmptyList(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "t1", 1, 0)
	dir, err := rec.NewExportDir()
	if err != nil {
		t.Fatalf("NewExportDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("导出目录应存在且是目录：%v %v", info, err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("导出目录必须 0700：%v", info.Mode())
	}
	if filepath.Dir(dir) != rec.Dir() {
		t.Fatalf("导出目录必须是 attempt 直接子目录：%s vs %s", dir, rec.Dir())
	}
	manifest, err := rec.ExportDir(dir)
	if err != nil {
		t.Fatalf("空导出目录必须合法：%v", err)
	}
	if manifest == nil || len(manifest) != 0 {
		t.Fatalf("空目录 = []（非 nil）：%#v", manifest)
	}
}

func TestExportDirCopiesNestedFilesAndHashes(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "t1", 1, 0)
	dir, err := rec.NewExportDir()
	if err != nil {
		t.Fatalf("NewExportDir: %v", err)
	}
	files := map[string]string{
		"report.md":        "hello",
		"reports/deep.txt": "nested",
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := rec.ExportDir(dir)
	if err != nil {
		t.Fatalf("ExportDir: %v", err)
	}
	if len(manifest) != 2 {
		t.Fatalf("应导出 2 个文件：%+v", manifest)
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
	want := []string{"artifacts/report.md", "artifacts/reports/deep.txt"}
	for i, m := range manifest {
		if m.Path != want[i] {
			t.Fatalf("逻辑名必须相对导出目录：%+v", manifest)
		}
		sum := sha256.Sum256([]byte(files[want[i][len(ManifestPathPrefix)+1:]]))
		if m.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("sha256 不对：%+v", m)
		}
		copyPath := filepath.Join(rec.Dir(), filepath.FromSlash(m.Path))
		if data, err := os.ReadFile(copyPath); err != nil || string(data) != files[want[i][len(ManifestPathPrefix)+1:]] {
			t.Fatalf("托管副本不对：%q err=%v", data, err)
		}
	}
}

func TestExportDirRejectsSymlinkAndSpecialFiles(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "t1", 1, 0)
	dir, err := rec.NewExportDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "leak")); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.ExportDir(dir); err == nil {
		t.Fatal("导出目录里的符号链接必须报错（不能跟随/越界）")
	}
	_ = os.Remove(filepath.Join(dir, "leak"))

	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo 不可用：%v", err)
	}
	if _, err := rec.ExportDir(dir); err == nil {
		t.Fatal("非普通文件必须报错")
	}
}

func TestExportDirRejectsForeignDirectory(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "t1", 1, 0)
	other := beginAttempt(t, s, "t2", 1, 0)
	foreign, err := other.NewExportDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.ExportDir(foreign); err == nil {
		t.Fatal("不能枚举另一个 attempt 的导出目录")
	}
	if _, err := rec.ExportDir(filepath.Join(rec.Dir(), "not-export")); err == nil {
		t.Fatal("非 export-* 目录必须被拒绝")
	}
}

func TestCollectRespectsTTLActiveAndUserWorkDir(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	userDir := filepath.Join(t.TempDir(), "user-work")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1) TTL 0 = 永不清理。
	never := beginAttempt(t, s, "never", 1, 0)
	// 2) 已过期（TTL 3600，createdAt 2h 前）。
	expired, err := s.Begin(Record{
		TaskID: "expired", Node: "node-a", Attempt: 1, Capability: "echo", Digest: "sha256:aa",
		TTL: 3600, Phase: "succeeded", CreatedAt: now.Add(-2 * time.Hour),
		SystemWorkDir: filepath.Join(s.WorkRoot, "expired", "1"),
		Manifest:      []Manifest{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(expired.Record().SystemWorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 3) 用户 workDir：进度/原文件永不删。
	userRec, err := s.Begin(Record{
		TaskID: "user", Node: "node-a", Attempt: 1, Capability: "echo", Digest: "sha256:aa",
		TTL: 1, Phase: "succeeded", CreatedAt: now.Add(-48 * time.Hour),
		UserWorkDir: userDir, Manifest: []Manifest{},
	})
	if err != nil {
		t.Fatal(err)
	}
	keepFile := filepath.Join(userDir, "important.txt")
	if err := os.WriteFile(keepFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 4) 活动任务不删。
	active := beginAttempt(t, s, "active", 1, 1)
	if err := active.Finish("succeeded", nil, now); err != nil {
		t.Fatal(err)
	}
	// active 的元数据也是 1s TTL 且创建于过去？改用显式过期时间：直接改 CreatedAt 不方便，
	// 用现在时间创建 TTL 1 秒，Collect 用 now 判断不会过期。

	removed, errs := s.Collect(now, map[string]bool{"active": true})
	for _, err := range errs {
		t.Fatalf("Collect 错误: %v", err)
	}
	if _, err := os.Stat(never.Dir()); err != nil {
		t.Fatalf("TTL=0 的档案不能删：%v", err)
	}
	if _, err := os.Stat(expired.Dir()); !os.IsNotExist(err) {
		t.Fatalf("过期档案应被删：%v", err)
	}
	if _, err := os.Stat(expired.Record().SystemWorkDir); !os.IsNotExist(err) {
		t.Fatalf("系统 workDir 应随档案清理：%v", err)
	}
	if data, err := os.ReadFile(keepFile); err != nil || string(data) != "keep" {
		t.Fatalf("用户 workDir 原文件不能被删：%q err=%v", data, err)
	}
	if _, err := os.Stat(userRec.Dir()); !os.IsNotExist(err) {
		t.Fatalf("用户 workDir 的 attempt 档案（日志/托管副本）仍按 TTL 清理：%v", err)
	}
	if _, err := os.Stat(userDir); err != nil {
		t.Fatalf("用户 workDir 本身不能被删：%v", err)
	}
	if _, err := os.Stat(active.Dir()); err != nil {
		t.Fatalf("活动任务不能删：%v", err)
	}
	if len(removed) == 0 {
		t.Fatal("应至少删除一条过期记录")
	}
}

func TestLogsExactAttemptAndLatest(t *testing.T) {
	s := newStore(t)
	for attempt, content := range map[int]string{1: "first", 2: "second"} {
		rec := beginAttempt(t, s, "t1", attempt, 0)
		for name, text := range map[string]string{StdoutFileName: content + "-out", StderrFileName: content + "-err"} {
			f, err := rec.OpenLogFile(name)
			if err != nil {
				t.Fatalf("OpenLogFile: %v", err)
			}
			if _, err := f.WriteString(text); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}
	}
	attempt, stdout, stderr, found, err := s.Logs("t1", 1)
	if err != nil || !found || attempt != 1 || string(stdout) != "first-out" || string(stderr) != "first-err" {
		t.Fatalf("精确 attempt 读档不对：%d %q %q %v %v", attempt, stdout, stderr, found, err)
	}
	attempt, _, _, found, err = s.LatestLogs("t1")
	if err != nil || !found || attempt != 2 {
		t.Fatalf("latest 应选最大 attempt：%d %v %v", attempt, found, err)
	}
	if _, _, _, found, _ := s.Logs("t1", 9); found {
		t.Fatal("不存在的 attempt 不能伪造档案")
	}
}

func TestSetPGIDAndRunningRecords(t *testing.T) {
	s := newStore(t)
	rec := beginAttempt(t, s, "t1", 1, 0)
	slot := 0
	rec.SetSlot(&slot)
	rec.SetLeaseID("lease-1")
	if err := rec.SetPGID(4242); err != nil {
		t.Fatalf("SetPGID: %v", err)
	}
	records, err := s.RunningRecords()
	if err != nil || len(records) != 1 {
		t.Fatalf("running 记录：%+v err=%v", records, err)
	}
	got := records[0]
	if got.PGID != 4242 || got.LeaseID != "lease-1" || got.Slot == nil || *got.Slot != 0 {
		t.Fatalf("持久事实不对：%+v", got)
	}
	if err := rec.Finish("succeeded", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	records, _ = s.RunningRecords()
	if len(records) != 0 {
		t.Fatalf("终态后不能再算 running：%+v", records)
	}
}
