package main

import (
	"strings"
	"testing"

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
