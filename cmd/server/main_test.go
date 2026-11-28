package main

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/server"
	"github.com/zzjcool/anotify/internal/version"
)

// withVersion 在测试期间替换构建期注入的版本元数据（包级 var，正是 -ldflags -X 的落点）。
func withVersion(t *testing.T, v, commit, builtAt string) {
	t.Helper()
	oldV, oldC, oldB := version.Version, version.Commit, version.BuiltAt
	t.Cleanup(func() { version.Version, version.Commit, version.BuiltAt = oldV, oldC, oldB })
	version.Version, version.Commit, version.BuiltAt = v, commit, builtAt
}

// --version 必须把注入值原样报出来（#334 的验收：version/commit 与构建注入一致）。
func TestVersionLineReportsInjectedMetadata(t *testing.T) {
	withVersion(t, "9.9.9-test", "deadbee", "2020-01-02T03:04:05Z")

	got := versionLine()
	want := "anotify 9.9.9-test+deadbee (2020-01-02T03:04:05Z)"
	if got != want {
		t.Fatalf("versionLine() = %q，want %q", got, want)
	}
}

// 没注入 BuiltAt 时不能打印空括号：那会被读成「构建时间是空字符串」而不是「没注入」。
func TestVersionLineWithoutBuiltAtSaysUnknown(t *testing.T) {
	withVersion(t, "", "", "")

	got := versionLine()
	want := "anotify dev (unknown)"
	if got != want {
		t.Fatalf("versionLine() = %q，want %q（空注入要回落 dev/unknown）", got, want)
	}
}

// 帮助必须能回答「这个二进制支持什么」：不带参数是起服务，--version / -h 是出口。
func TestUsageMentionsVersionAndHelp(t *testing.T) {
	var b strings.Builder
	usage(&b)
	out := b.String()
	for _, needle := range []string{"Usage: anotify", "--version", "-h", "环境变量"} {
		if !strings.Contains(out, needle) {
			t.Fatalf("usage() 缺 %q：\n%s", needle, out)
		}
	}
}

func TestStartPprofDisabledWhenOff(t *testing.T) {
	closer, addr := startPprof(server.Config{Pprof: false, PprofAddr: "127.0.0.1:0"})
	if closer != nil || addr != "" {
		t.Fatalf("Pprof=false 不应监听，得到 closer=%v addr=%q", closer, addr)
	}
}

func TestStartPprofServesOnConfiguredAddr(t *testing.T) {
	closer, addr := startPprof(server.Config{Pprof: true, PprofAddr: "127.0.0.1:0"})
	if closer == nil || addr == "" {
		t.Fatalf("Pprof=true 必须起监听，得到 closer=%v addr=%q", closer, addr)
	}
	t.Cleanup(func() { _ = closer.Close() })

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("监听地址不可解析：%q（%v）", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("显式给 127.0.0.1:0 时实际监听地址应仍是回环，得到 %q", addr)
	}

	resp, err := http.Get("http://" + addr + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET pprof 索引失败（%s）: %v", addr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Types of profiles available") {
		t.Fatalf("GET %s/debug/pprof/ = %d, body=%.120q", addr, resp.StatusCode, body)
	}
}

func TestStartPprofListenFailureUsesOwnEvent(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占一个端口：%v", err)
	}
	t.Cleanup(func() { _ = busy.Close() })

	closer, addr := startPprof(server.Config{Pprof: true, PprofAddr: busy.Addr().String()})
	if closer != nil || addr != "" {
		t.Fatalf("端口被占时不应返回可用监听，得到 closer=%v addr=%q", closer, addr)
	}
	logged := buf.String()
	if !strings.Contains(logged, `"event":"server.pprof.listen_failed"`) {
		t.Fatalf("端口冲突必须记 event=server.pprof.listen_failed，日志：%s", logged)
	}
	if strings.Contains(logged, `"event":"server.fatal"`) {
		t.Fatalf("pprof 端口冲突不得记 server.fatal（业务没挂，报警规则会误判）：%s", logged)
	}
}
