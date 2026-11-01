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

// 本文件覆盖入口段：
//
//  1. 节点模式（`-f`）——参数/配置的失败全部发生在 dial 之前，不联网就能测；
//  2. standalone 模式（`run` 子命令）——**端到端**：临时产物文件 + `-p` +
//     断言 stdout 与退出码。这条路径是「产物拷到任意机器直接能跑」的落地，
//     所以必须真的执行一个脚本，而不是只测参数解析。

// ---------- 节点模式：参数与配置 ----------

// TestRunMissingConfigFile：-f 指向不存在的文件 → 返回错误且错误串含该路径。
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

// TestRunBadConfig：坏配置（缺 token / 缺 server / 未知字段 / 老配置 / 坏 YAML）
// → 返回非 nil 错误且不 panic。
func TestRunBadConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
		want string // "" = 只要求返回错误，不校验子串
	}{
		{
			name: "token 为空",
			yaml: "server: http://localhost:8080\ntoken: \"\"\n",
			want: "token is required",
		},
		{
			name: "缺 server",
			yaml: "token: node-token\n",
			want: "server is required",
		},
		{
			name: "YAML 语法坏",
			yaml: "server: [unclosed\n",
			want: "",
		},
		{
			name: "未知字段",
			yaml: "server: http://localhost:8080\ntoken: node-token\ntypoField: 1\n",
			want: "typoField",
		},
		{
			// v1 的老 node.yaml：能力不再由机器声明，必须在启动时就明确拒绝。
			name: "v1 的 capabilities",
			yaml: `server: http://localhost:8080
token: node-token
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo hi
`,
			want: "capabilities",
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

// TestGraceDuration 覆盖 -grace 的语义：秒 → Duration，<=0 归一为 0。
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

// TestParseFlagsDefaults 锁定缺省值：-f=./node.yaml、-grace=20、-data-dir 为空
// （空 = 用 ~/.anotify，见 resolveDataDir）。
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
	if opts.dataDir != "" {
		t.Errorf("缺省 -data-dir = %q, want 空串（用 defaultDataDir）", opts.dataDir)
	}

	dir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		t.Fatalf("resolveDataDir: %v", err)
	}
	if filepath.Base(dir) != ".anotify" {
		t.Errorf("resolveDataDir(\"\") = %q, want .../.anotify", dir)
	}
	if got, _ := resolveDataDir("/var/lib/anotify"); got != "/var/lib/anotify" {
		t.Errorf("resolveDataDir(显式) = %q, want /var/lib/anotify", got)
	}
}

// TestParseFlagsExplicit 覆盖显式传参（三个 flag 一起），以及 -grace 0 表示不等。
func TestParseFlagsExplicit(t *testing.T) {
	t.Parallel()

	opts, err := parseFlags([]string{
		"-f", "/tmp/anotify-agent/node.yaml", "-grace", "7", "-data-dir", "/var/lib/anotify",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags 意外失败: %v", err)
	}
	if opts.configPath != "/tmp/anotify-agent/node.yaml" {
		t.Errorf("-f = %q, want /tmp/anotify-agent/node.yaml", opts.configPath)
	}
	if opts.grace != 7*time.Second {
		t.Errorf("-grace 7 = %v, want 7s", opts.grace)
	}
	if opts.dataDir != "/var/lib/anotify" {
		t.Errorf("-data-dir = %q, want /var/lib/anotify", opts.dataDir)
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

// TestRunHelp：-h 是用户主动要帮助 → 不当失败（run 返回 nil → 退出码 0）。
func TestRunHelp(t *testing.T) {
	t.Parallel()

	if _, err := parseFlags([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseFlags(-h) = %v, want flag.ErrHelp", err)
	}

	var stderr bytes.Buffer
	if err := run([]string{"-h"}, &stderr); err != nil {
		t.Fatalf("run(-h) 应当返回 nil（退出码 0），实际 %v", err)
	}
	if !strings.Contains(stderr.String(), "-grace") || !strings.Contains(stderr.String(), "-data-dir") {
		t.Errorf("-h 的 usage 应当列出 -grace 与 -data-dir，实际 %q", stderr.String())
	}
}

// TestNewLoggerLevel：INFO 级、文本格式、写 stderr。
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

// ---------- standalone：run 子命令 ----------

// writeCapabilityFile 把产物写成 YAML 落到临时目录，返回路径（模拟「拷到别的机器」）。
func writeCapabilityFile(t *testing.T, name string, version int, body string) string {
	t.Helper()
	art := testScriptArtifact(name, version, bashScript(body))
	path := filepath.Join(t.TempDir(), "capability.yaml")
	if err := os.WriteFile(path, mustArtifactYAML(t, art), 0o600); err != nil {
		t.Fatalf("写产物文件: %v", err)
	}
	return path
}

// TestRunStandaloneEndToEnd 是验收标准 4 的单测版：把一份产物 YAML 拷到一个临时
// 文件，用 `run` 跑起来，参数走 -p，stdout 与退出码都要对。
func TestRunStandaloneEndToEnd(t *testing.T) {
	path := writeCapabilityFile(t, "demo.greet", 2, `
set -u
cd "$ANOTIFY_WORKDIR"
printf 'param=%s\n' "$ANOTIFY_PARAM_NAME"
printf 'num=%s\n' "$ANOTIFY_PARAM_N"
printf 'cap=%s\n' "$ANOTIFY_CAPABILITY"
printf 'workdir-ok=%s\n' "$([ -d "$ANOTIFY_WORKDIR" ] && echo yes || echo no)"
printf 'entrypoint=%s\n' "$([ -f ./run.sh ] && echo yes || echo no)"
printf 'digest=%s\n' "$([ -n "$ANOTIFY_DIGEST" ] && echo yes || echo no)"
`)

	var stdout, stderr bytes.Buffer
	code, err := runStandalone([]string{path, "-p", "name=world", "-p", "n=42"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runStandalone: %v（stderr=%s）", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("退出码 = %d, want 0（stderr=%s）", code, stderr.String())
	}

	got := stdout.String()
	for _, want := range []string{
		"param=world",
		"num=42", // -p 的值一律当字符串，但打印出来就是 42
		"cap=demo.greet@2",
		"workdir-ok=yes",
		"entrypoint=yes",
		"digest=yes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout 没有 %q：\n%s", want, got)
		}
	}

	// 日志走 stderr，绝不污染脚本的 stdout。
	if !strings.Contains(stderr.String(), "agent.run.start") {
		t.Errorf("stderr 应当有 agent.run.start 日志，实际 %q", stderr.String())
	}
	if strings.Contains(got, "agent.run.start") {
		t.Errorf("日志混进了 stdout：%q", got)
	}
}

// TestRunStandaloneFlagsAfterPath：位置参数写在前面时，后面的 -p 仍然生效。
func TestRunStandaloneFlagsAfterPath(t *testing.T) {
	path := writeCapabilityFile(t, "demo.echo", 1, "printf 'v=%s' \"$ANOTIFY_PARAM_V\"\n")

	var stdout, stderr bytes.Buffer
	code, err := runStandalone([]string{path, "-p", "v=late"}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("runStandalone = (%d, %v)，want (0, nil)（stderr=%s）", code, err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "v=late" {
		t.Fatalf("stdout = %q, want v=late", got)
	}

	// flags 写在前面也应该行。
	stdout.Reset()
	code, err = runStandalone([]string{"-p", "v=early", path}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("runStandalone(-p 在前) = (%d, %v)，want (0, nil)", code, err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "v=early" {
		t.Fatalf("stdout = %q, want v=early", got)
	}
}

// TestRunStandaloneExitCodePassThrough：退出码原样透传（脚本怎么退，agent 就怎么退）。
func TestRunStandaloneExitCodePassThrough(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{"exit 0\n", 0},
		{"exit 3\n", 3},
		{"echo boom >&2; exit 42\n", 42},
	}
	for _, tc := range cases {
		path := writeCapabilityFile(t, "demo.exit", 1, tc.body)
		var stdout, stderr bytes.Buffer
		code, err := runStandalone([]string{path}, &stdout, &stderr)
		if err != nil {
			t.Fatalf("body=%q: runStandalone 返回 error %v（退出码本身就是结果，不是 agent 故障）", tc.body, err)
		}
		if code != tc.want {
			t.Errorf("body=%q: 退出码 = %d, want %d", tc.body, code, tc.want)
		}
	}
}

// TestRunStandaloneTimeout：--timeout 压过产物自己的 timeoutSec，超时按失败退出。
func TestRunStandaloneTimeout(t *testing.T) {
	art := testScriptArtifact("demo.slow", 1, bashScript("sleep 5"))
	art.Spec.Run.TimeoutSec = 600 // 产物自己写的是 10 分钟
	path := filepath.Join(t.TempDir(), "capability.yaml")
	if err := os.WriteFile(path, mustArtifactYAML(t, art), 0o600); err != nil {
		t.Fatalf("写产物文件: %v", err)
	}

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code, err := runStandalone([]string{path, "--timeout", "1"}, &stdout, &stderr)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("runStandalone 用了 %s，--timeout 没生效", elapsed)
	}
	if code != exitFailure {
		t.Errorf("退出码 = %d, want %d", code, exitFailure)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want 含 timed out", err)
	}
	if !strings.Contains(stderr.String(), "工作目录已保留") {
		t.Errorf("超时后应当提示工作目录保留位置，实际 stderr = %q", stderr.String())
	}
}

// TestRunStandaloneBadInput：产物不合法 / 文件不存在 / 缺路径 → 失败但不 panic。
func TestRunStandaloneBadInput(t *testing.T) {
	t.Run("缺路径", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, err := runStandalone(nil, &stdout, &stderr)
		if err == nil || code != exitFailure {
			t.Fatalf("runStandalone(nil) = (%d, %v), want (%d, error)", code, err, exitFailure)
		}
		if !strings.Contains(err.Error(), "用法") {
			t.Errorf("error = %v, want 含用法", err)
		}
	})

	t.Run("文件不存在", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope.yaml")
		var stdout, stderr bytes.Buffer
		if _, err := runStandalone([]string{missing}, &stdout, &stderr); err == nil {
			t.Fatal("want error for missing file, got nil")
		} else if !strings.Contains(err.Error(), missing) {
			t.Errorf("error = %v, want 含路径 %q", err, missing)
		}
	})

	t.Run("产物不合法", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.yaml")
		// 入口没有 shebang：不合法产物**一定跑不起来**（直接 exec 会直接失败），
		// 所以要在解析阶段就明确报错，而不是让用户看到一个莫名奇妙的 exec 错误。
		if err := os.WriteFile(path, []byte(`apiVersion: anotify/v1
kind: Capability
metadata:
  name: demo.bad
  version: 1
spec:
  outputs: []
  run:
    entrypoint: run.sh
    files:
      run.sh: "echo hi\n"
`), 0o600); err != nil {
			t.Fatalf("写产物文件: %v", err)
		}
		var stdout, stderr bytes.Buffer
		_, err := runStandalone([]string{path}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "entrypoint_no_shebang") {
			t.Fatalf("error = %v, want 含 entrypoint_no_shebang", err)
		}
	})

	t.Run("-p 形状坏", func(t *testing.T) {
		path := writeCapabilityFile(t, "demo.p", 1, "true\n")
		var stdout, stderr bytes.Buffer
		if _, err := runStandalone([]string{path, "-p", "noequals"}, &stdout, &stderr); err == nil {
			t.Fatal("want error for -p without '=', got nil")
		}
	})

	t.Run("多余的位置参数", func(t *testing.T) {
		path := writeCapabilityFile(t, "demo.p", 1, "true\n")
		var stdout, stderr bytes.Buffer
		if _, err := runStandalone([]string{path, "extra.yaml"}, &stdout, &stderr); err == nil {
			t.Fatal("want error for extra positional args, got nil")
		}
	})
}

// TestMainWithDispatch：`run` 子命令走 standalone，其余走节点模式；退出码由 mainWith 决定。
func TestMainWithDispatch(t *testing.T) {
	t.Run("run 子命令透传退出码", func(t *testing.T) {
		path := writeCapabilityFile(t, "demo.exit", 1, "exit 5\n")
		var stdout, stderr bytes.Buffer
		if code := mainWith([]string{"run", path}, &stdout, &stderr); code != 5 {
			t.Fatalf("mainWith(run ...) = %d, want 5（透传产物退出码）", code)
		}
	})

	t.Run("run 子命令 agent 侧故障 = 1", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := mainWith([]string{"run"}, &stdout, &stderr); code != exitFailure {
			t.Fatalf("mainWith(run) = %d, want %d", code, exitFailure)
		}
		if !strings.Contains(stderr.String(), "agent.run.failed") {
			t.Errorf("stderr 应当有 agent.run.failed 日志，实际 %q", stderr.String())
		}
	})

	t.Run("节点模式坏配置 = 1", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "node.yaml")
		if err := os.WriteFile(path, []byte("server: \"\"\n"), 0o600); err != nil {
			t.Fatalf("写 node.yaml: %v", err)
		}
		var stdout, stderr bytes.Buffer
		if code := mainWith([]string{"-f", path}, &stdout, &stderr); code != exitFailure {
			t.Fatalf("mainWith(-f 坏配置) = %d, want %d", code, exitFailure)
		}
	})

	t.Run("-h = 0", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := mainWith([]string{"-h"}, &stdout, &stderr); code != exitOK {
			t.Fatalf("mainWith(-h) = %d, want %d", code, exitOK)
		}
	})
}
