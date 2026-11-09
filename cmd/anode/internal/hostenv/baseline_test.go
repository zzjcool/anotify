package hostenv

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeExecutable 在 dir 下写一个可执行文件（Unix 语义；本机是 linux）。
func writeExecutable(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLookPathUsesGivenPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix 执行位语义")
	}
	dir := t.TempDir()
	want := writeExecutable(t, dir, "probe-tool", 0o755)
	got, err := LookPath(dir, "probe-tool")
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	if got != want {
		t.Fatalf("LookPath = %q, want %q", got, want)
	}
	// 名字含 '/' 直接按路径找。
	if got, err := LookPath("/nonexistent", want); err != nil || got != want {
		t.Fatalf("绝对路径 LookPath = %q, %v", got, err)
	}
}

func TestLookPathNotFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix 执行位语义")
	}
	dir := t.TempDir()
	writeExecutable(t, dir, "no-exec", 0o644)
	writeExecutable(t, dir, "is-dir", 0o755)
	if err := os.Remove(filepath.Join(dir, "is-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "is-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "no-exec", "is-dir"} {
		if _, err := LookPath(dir, name); err == nil {
			t.Fatalf("%s 不应被探测到", name)
		}
	}
}

func TestBaselineSnapshotStableAcrossEnvChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix 执行位语义")
	}
	dir := t.TempDir()
	want := writeExecutable(t, dir, "tool", 0o755)
	base := CaptureWith(nil, func(key string) (string, bool) {
		if key == "PATH" {
			return dir, true
		}
		return "", false
	})
	// 进程环境随后变化不影响启动快照：探测与执行同源。
	t.Setenv("PATH", t.TempDir())
	got, err := base.LookPath("tool")
	if err != nil || got != want {
		t.Fatalf("Baseline.LookPath = %q, %v; want %q", got, err, want)
	}
	if base.PATH() != dir {
		t.Fatalf("Baseline.PATH = %q", base.PATH())
	}
}

func TestPassthroughSnapshotOnlyNames(t *testing.T) {
	env := map[string]string{"GITHUB_TOKEN": "ghp-secret", "SSH_AUTH_SOCK": "/tmp/agent.sock"}
	base := CaptureWith([]string{"GITHUB_TOKEN", "MISSING"}, func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	})
	if v, ok := base.Passthrough("GITHUB_TOKEN"); !ok || v != "ghp-secret" {
		t.Fatalf("GITHUB_TOKEN = %q,%v", v, ok)
	}
	if _, ok := base.Passthrough("SSH_AUTH_SOCK"); ok {
		t.Fatal("不在白名单的名字不应被快照")
	}
	if _, ok := base.Passthrough("MISSING"); ok {
		t.Fatal("启动时缺失的名字不应被快照")
	}
	// 环境后续变化不影响快照值。
	env["GITHUB_TOKEN"] = "changed"
	if v, _ := base.Passthrough("GITHUB_TOKEN"); strings.Contains(v, "changed") {
		t.Fatalf("passthrough 值漂移: %q", v)
	}
}
