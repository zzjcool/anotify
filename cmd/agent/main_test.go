package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖入口段（契约 §5 W5）：`-f` 指向不存在的文件、坏配置，以及
// `-grace`/缺省值的解析。全部**不联网、不连 WebSocket**——参数与配置的失败
// 都发生在 dial 之前，run() 会在这三处直接返回。
//
// 测试到哪一层为止：run() 里「信号 + runNode」那一段不测（要真连接才能推进），
// 它由 client_test.go 用 fake nodeConn 覆盖 serveNode 内核；入口这里只保证
// 「参数/配置错了就带着可读错误退出，而不是静默连上去」。

// TestRunMissingConfigFile 对应 W5-1：-f 指向不存在的文件 → 返回错误且错误串
// 含该路径（用户同时维护多份 node.yaml 时，报错必须自己说清是哪一份）。
func TestRunMissingConfigFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "no-such-node.yaml")
	err := run([]string{"-f", path}, io.Discard)
	if err == nil {
		t.Fatalf("run(-f %s) 应当失败，却返回 nil", path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("错误串应当包含配置路径 %q，实际 %q", path, err.Error())
	}
}

// TestRunBadConfig 对应 W5-2：坏配置（缺 token / 缺 capabilities / 缺 run /
// 坏 YAML / 未知字段）→ 返回非 nil 错误且不 panic。
//
// 断言子串与 config.go 的校验口径一致（契约 §3.1.1），这里只取几条有代表性的：
// 入口的职责是「把配置错误原样交出来」，具体措辞归 config_test.go 全覆盖。
func TestRunBadConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
		want string // "" = 只要求返回错误，不校验子串
	}{
		{
			name: "token 为空",
			yaml: `
server: http://localhost:8080
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo hi
`,
			want: "token is required",
		},
		{
			name: "缺 capabilities",
			yaml: `
server: http://localhost:8080
token: node-token
`,
			want: "at least one capability is required",
		},
		{
			name: "缺 run",
			yaml: `
server: http://localhost:8080
token: node-token
capabilities:
  - capability: demo.prep
    version: 1
`,
			want: "demo.prep@1: run is required",
		},
		{
			name: "缺 server",
			yaml: `
token: node-token
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo hi
`,
			want: "server is required",
		},
		{
			name: "YAML 语法坏",
			yaml: "server: [unclosed\n",
			want: "",
		},
		{
			name: "未知字段",
			yaml: `
server: http://localhost:8080
token: node-token
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo hi
      typoField: 1
`,
			want: "typoField",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "node.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("写临时 node.yaml 失败: %v", err)
			}

			var stderr bytes.Buffer
			err := run([]string{"-f", path}, &stderr)
			if err == nil {
				t.Fatalf("坏配置 %q 应当返回错误，却返回 nil（stderr=%s）", tc.name, stderr.String())
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误串应当包含 %q，实际 %q", tc.want, err.Error())
			}
			// 错误必须落到日志里（main 只翻退出码，没人再打错误详情）。
			if !strings.Contains(stderr.String(), "agent.config.failed") {
				t.Fatalf("stderr 应当有 agent.config.failed 日志，实际 %q", stderr.String())
			}
		})
	}
}

// TestGraceDuration 覆盖 -grace 的语义：秒 → Duration，<=0 归一为 0（= 不等在跑
// 任务），不用负数去「反向等待」，也不 panic。
func TestGraceDuration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sec  int
		want time.Duration
	}{
		{sec: 0, want: 0},
		{sec: -1, want: 0},
		{sec: -3600, want: 0},
		{sec: 7, want: 7 * time.Second},
		{sec: defaultGraceSec, want: 20 * time.Second},
	}
	for _, tc := range cases {
		if got := graceDuration(tc.sec); got != tc.want {
			t.Errorf("graceDuration(%d) = %v, want %v", tc.sec, got, tc.want)
		}
	}
}

// TestParseFlagsDefaults 锁定缺省值：-f=./node.yaml、-grace=20。
func TestParseFlagsDefaults(t *testing.T) {
	t.Parallel()

	opts, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags(nil) 意外失败: %v", err)
	}
	if opts.configPath != "./node.yaml" {
		t.Errorf("缺省 -f = %q, want ./node.yaml", opts.configPath)
	}
	if opts.grace != 20*time.Second {
		t.Errorf("缺省 -grace = %v, want 20s", opts.grace)
	}
}

// TestParseFlagsExplicit 覆盖显式传参（两个 flag 一起），以及 -grace 0 表示不等。
func TestParseFlagsExplicit(t *testing.T) {
	t.Parallel()

	opts, err := parseFlags([]string{"-f", "/tmp/anotify-agent/node.yaml", "-grace", "7"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags 意外失败: %v", err)
	}
	if opts.configPath != "/tmp/anotify-agent/node.yaml" {
		t.Errorf("-f = %q, want /tmp/anotify-agent/node.yaml", opts.configPath)
	}
	if opts.grace != 7*time.Second {
		t.Errorf("-grace 7 = %v, want 7s", opts.grace)
	}

	opts, err = parseFlags([]string{"-grace", "0"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags(-grace 0) 意外失败: %v", err)
	}
	if opts.grace != 0 {
		t.Errorf("-grace 0 = %v, want 0（无等待）", opts.grace)
	}
}

// TestParseFlagsUnknownFlag：未知 flag 是错误，且不能顺手 exit（退出码归 main）。
func TestParseFlagsUnknownFlag(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	if _, err := parseFlags([]string{"-nope"}, &stderr); err == nil {
		t.Fatal("未知 flag 应当返回错误，却返回 nil")
	}
	if !strings.Contains(stderr.String(), "-nope") {
		t.Errorf("stderr 应当提到 -nope，实际 %q", stderr.String())
	}
}

// TestRunHelp：-h 是用户主动要帮助 → 不当失败（run 返回 nil → 退出码 0），
// 也不能因为「没读配置」而报配置错误。
func TestRunHelp(t *testing.T) {
	t.Parallel()

	if _, err := parseFlags([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseFlags(-h) = %v, want flag.ErrHelp", err)
	}

	var stderr bytes.Buffer
	if err := run([]string{"-h"}, &stderr); err != nil {
		t.Fatalf("run(-h) 应当返回 nil（退出码 0），实际 %v", err)
	}
	if !strings.Contains(stderr.String(), "-grace") {
		t.Errorf("-h 的 usage 应当列出 -grace，实际 %q", stderr.String())
	}
}

// TestNewLoggerLevel：契约要求 INFO 级、文本格式、写 stderr —— 用 buffer 代 stderr
// 验证 handler 形状（DEBUG 被丢弃、INFO 带 key=value）。
func TestNewLoggerLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := newLogger(&buf)
	log.Debug("不应该出现")
	log.Info("出现了", "event", "agent.test")

	out := buf.String()
	if strings.Contains(out, "不应该出现") {
		t.Errorf("INFO 级 logger 不该输出 DEBUG，实际 %q", out)
	}
	if !strings.Contains(out, "出现了") || !strings.Contains(out, "event=agent.test") {
		t.Errorf("日志行不符合 TextHandler 形状，实际 %q", out)
	}
}
