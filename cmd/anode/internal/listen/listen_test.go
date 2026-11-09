package listen

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseDefaultAndForms(t *testing.T) {
	if spec, err := Parse(""); err != nil || spec.Network != "tcp" || spec.Address != "127.0.0.1:0" {
		t.Fatalf("空值缺省 = %+v, %v", spec, err)
	}
	if spec, err := Parse("tcp://127.0.0.1:7070"); err != nil || spec.Address != "127.0.0.1:7070" {
		t.Fatalf("tcp 解析 = %+v, %v", spec, err)
	}
	if spec, err := Parse("unix:///run/anode.sock"); err != nil || spec.Network != "unix" || spec.Address != "/run/anode.sock" {
		t.Fatalf("unix 解析 = %+v, %v", spec, err)
	}
	for _, bad := range []string{"http://127.0.0.1:1", "tcp://:1", "tcp://127.0.0.1", "unix://relative.sock", "unix://host/path", "tcp://127.0.0.1:70000"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("%q 应失败", bad)
		}
	}
}

func TestOpenTCPActualEndpoint(t *testing.T) {
	l, err := Open(DefaultSpec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	if l.Spec().Network != "tcp" {
		t.Fatalf("network = %s", l.Spec().Network)
	}
	pattern := regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+$`)
	if endpoint := l.Endpoint(); !pattern.MatchString(endpoint) {
		t.Fatalf("Endpoint = %q", endpoint)
	}
	addr := l.Addr().String()
	if strings.HasSuffix(addr, ":0") {
		t.Fatalf("实际端点不应还是 :0: %s", addr)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("实际端点不可连: %v", err)
	}
	_ = conn.Close()
}

func TestOpenUnixAndCloseRemovesSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "emit.sock")
	l, err := Open("unix://" + path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if l.Endpoint() != "unix://"+path {
		t.Fatalf("Endpoint = %q", l.Endpoint())
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s 不是 socket", path)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket 权限 = %o", info.Mode().Perm())
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("Close 后 socket 应被删除: %v", err)
	}
}

func TestOpenUnixRefusesRegularFileAndSymlink(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open("unix://" + regular); err == nil || !strings.Contains(err.Error(), "不是 socket") {
		t.Fatalf("普通文件应拒绝: %v", err)
	}
	if data, err := os.ReadFile(regular); err != nil || string(data) != "keep" {
		t.Fatalf("普通文件被改动: %q, %v", data, err)
	}

	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open("unix://" + link); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("符号链接应拒绝: %v", err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "keep" {
		t.Fatalf("符号链接目标被改动: %q, %v", data, err)
	}

	if _, err := Open("unix://" + dir); err == nil {
		t.Fatal("目录应拒绝")
	}
}

func TestOpenUnixReclaimsStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.sock")
	raw, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	unix, ok := raw.(*net.UnixListener)
	if !ok {
		t.Fatalf("不是 *net.UnixListener: %T", raw)
	}
	unix.SetUnlinkOnClose(false)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("残留 socket 应还在: %v", err)
	}
	l, err := Open("unix://" + path)
	if err != nil {
		t.Fatalf("残留 socket 应可回收: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenUnixRefusesActiveSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "active.sock")
	first, err := Open("unix://" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Open("unix://" + path); err == nil || !strings.Contains(err.Error(), "已被占用") {
		t.Fatalf("活跃 socket 应拒绝: %v", err)
	}
}
