package main

// 本文件钉住**离线书写面**的第一条命令：`anotify-agent check <capability.yaml>`（issue #319 §1）。
//
// 断言口径一律是外部可见的东西（退出码 / stdout / stderr / 文件系统），因为这条命令的
// 全部价值就在这三处：
//
//	一次报全部问题    坏 yaml 的每一类问题都得出现（不是「改一条再跑一次」）；
//	不执行            入口文件写成 touch /tmp/… 也不能真的被跑起来；
//	不落盘            不同目录、不建 ~/.anotify、不写工作目录/执行档案。
//
// 最后两条是**它存在的理由**：`run` 也会做 Parse+Validate，但它会真的执行入口文件，
// 所以「我只是想看看 yaml 对不对」不能用 `run`。

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// checkWriteFile 把内容写进 t.TempDir() 下的一个文件并返回路径。
func checkWriteFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
	return path
}

// badCapabilityYAML 是一份**同时**踩了三类问题（且互不依赖）的产物：
//
//	metadata.version: 0      → invalid_version
//	run.entrypoint 缺失       → entrypoint_missing
//	spec.outputs 里有空格     → invalid_output
//
// 三处独立破坏是关键：只报第一条的实现会在这个用例上暴露（而「改一条再跑一次」正是
// issue 要消灭的体验）。
const badCapabilityYAML = `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: notify.phone
  version: 0
spec:
  outputs: ["good-out", "bad port"]
  run:
    files:
      main.sh: |
        #!/bin/sh
        echo hi
`

// goodCapabilityYAML 是合法产物；entrypoint 会去 touch 一个哨兵文件（若真的被执行）。
func goodCapabilityYAML(sentinel string) string {
	return `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: notify.phone
  version: 2
  displayName: 写文档
spec:
  params:
    type: object
    properties:
      repo: {type: string}
  outputs: [fix]
  run:
    entrypoint: main.sh
    timeoutSec: 60
    files:
      main.sh: |
        #!/bin/sh
        touch ` + sentinel + `
`
}

// TestCheckReportsAllProblemsAndExitsOne：坏 yaml 一次打印**全部**问题，退 1，且
// stderr 是空的（问题列表是这条命令的答案，不是命令自身失败）。
func TestCheckReportsAllProblemsAndExitsOne(t *testing.T) {
	t.Parallel()

	path := checkWriteFile(t, "bad.yaml", badCapabilityYAML)
	code, stdout, stderr := cliTextInvoke(t, checkCommand, path)
	if code != exitFailure {
		t.Fatalf("check(bad) 退出码 = %d, want %d（stdout:\n%s）", code, exitFailure, stdout)
	}
	if stderr != "" {
		t.Fatalf("坏 yaml 不该往 stderr 写东西（那是「命令自己出错」的通道）：%q", stderr)
	}

	// 三个问题各一行、都带短码——按行 grep 就能判。
	for _, wantCode := range []string{"invalid_version", "entrypoint_missing", "invalid_output"} {
		if !strings.Contains(stdout, wantCode) {
			t.Errorf("stdout 缺 %q 这一类问题（一次要报全部）：\n%s", wantCode, stdout)
		}
	}
	// 计数行也说了是 3 个（不是"第一个"）。
	if !strings.Contains(stdout, "3 个问题") {
		t.Errorf("stdout 没有「3 个问题」（说明只报了第一条？）：\n%s", stdout)
	}
	// 问题行是缩进两格的单行，且数量与计数一致。
	problemLines := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "  ") && strings.Contains(line, ":") {
			problemLines++
		}
	}
	if problemLines < 3 {
		t.Errorf("缩进的问题行只有 %d 条，want >= 3：\n%s", problemLines, stdout)
	}
}

// TestCheckJSONShapeAndCodes：`--json` 的形状是 {"ok":false,"problems":[{code,message}]}，
// code 取 artifact 的短码词表（与 run / apply 帧同一套）。
func TestCheckJSONShapeAndCodes(t *testing.T) {
	t.Parallel()

	path := checkWriteFile(t, "bad.yaml", badCapabilityYAML)
	code, stdout, stderr := cliJSONInvoke(t, checkCommand, path)
	if code != exitFailure {
		t.Fatalf("check --json(bad) 退出码 = %d, want 1", code)
	}
	if stderr != "" {
		t.Fatalf("坏 yaml 时 stderr 应为空（问题在 stdout），实际 %q", stderr)
	}

	var got checkJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout 不是合法 JSON: %v\n%s", err, stdout)
	}
	if got.OK {
		t.Fatalf("坏 yaml 的 ok 必须为 false：%s", stdout)
	}
	if len(got.Problems) < 3 {
		t.Fatalf("problems 只有 %d 条，want >= 3：%s", len(got.Problems), stdout)
	}
	for _, p := range got.Problems {
		if p.Code == "" || p.Message == "" {
			t.Fatalf("problems[] 的 code/message 都不许为空：%+v", p)
		}
		// code 必须来自 artifact 的词表（同一份问题文本用 ProblemCode 再切一次，必须一致）。
		if want := artifact.ProblemCode(p.Code + ": " + p.Message); p.Code != want {
			t.Fatalf("code = %q, 与 artifact.ProblemCode 分叉（%q）", p.Code, want)
		}
	}
	// 抽查两个具体短码（这两类最容易在重构里换个名字）。
	codes := map[string]bool{}
	for _, p := range got.Problems {
		codes[p.Code] = true
	}
	for _, want := range []string{"invalid_version", "entrypoint_missing", "invalid_output"} {
		if !codes[want] {
			t.Errorf("problems 里没有 %q：%s", want, stdout)
		}
	}
}

// TestCheckOKExitsZeroWithoutExecutingOrWriting：好 yaml 退 0，**不执行**入口文件，
// 也**不落盘**（不建 ~/.anotify、不在周围造任何文件）。
//
// 哨兵文件用 issue 里点名的那个路径：entrypoint 写成 touch /tmp/anotify-check-should-not-run，
// 只要 check 真的 exec 了入口文件，这个文件就会出现。
func TestCheckOKExitsZeroWithoutExecutingOrWriting(t *testing.T) {
	// 这条用例改了 HOME（钉「不落盘」），不能并行跑。
	home := t.TempDir()
	t.Setenv("HOME", home)

	const sentinel = "/tmp/anotify-check-should-not-run"
	_ = os.Remove(sentinel)
	t.Cleanup(func() { _ = os.Remove(sentinel) })

	dir := t.TempDir()
	path := filepath.Join(dir, "capability.yaml")
	if err := os.WriteFile(path, []byte(goodCapabilityYAML(sentinel)), 0o600); err != nil {
		t.Fatalf("写产物: %v", err)
	}

	code, stdout, stderr := cliTextInvoke(t, checkCommand, path)
	if code != exitOK {
		t.Fatalf("check(good) 退出码 = %d, want 0（stderr: %s）", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("好 yaml 不该往 stderr 写东西：%q", stderr)
	}
	// 人话那行要说清解析器理解到了什么（名字/版本/入口），而不只是"没报错"。
	for _, want := range []string{"通过", "notify.phone@2", "main.sh"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout 缺 %q：\n%s", want, stdout)
		}
	}

	// ① 不执行：入口文件没有被跑起来。
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("%s 被创建了：check 执行了入口文件（它只能 Parse + Validate）", sentinel)
	}
	// ② 不落盘：cwd 旁边与 ~/.anotify 都没有新东西。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读产物目录: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "capability.yaml" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("check 在产物目录里造了东西：%v（只能读、不能写）", names)
	}
	if _, err := os.Stat(filepath.Join(home, ".anotify")); err == nil {
		t.Fatalf("check 建了 ~/.anotify（数据目录是 run/节点模式的东西）")
	}
}

// TestCheckJSONOKShape：好 yaml 的 `--json` 是 {"ok":true,"problems":[]}——
// 空问题必须是 `[]` 而不是 null（jq 判形状时 null 会被读成「命令坏了」）。
func TestCheckJSONOKShape(t *testing.T) {
	t.Parallel()

	path := checkWriteFile(t, "good.yaml", goodCapabilityYAML("/tmp/never-runs"))
	code, stdout, stderr := cliJSONInvoke(t, checkCommand, path)
	if code != exitOK {
		t.Fatalf("check --json(good) 退出码 = %d, want 0（stderr: %s）", code, stderr)
	}
	if got := strings.TrimSpace(stdout); got != `{"ok":true,"problems":[]}` {
		t.Fatalf("stdout = %s, want {\"ok\":true,\"problems\":[]}", got)
	}
}

// TestCheckBrokenYAMLListsInvalidYAML：yaml 语法坏掉也要「一次说清」——短码 invalid_yaml，
// 后面的行号信息留在 message 里（那是 yaml 库给的最有用的东西）。
func TestCheckBrokenYAMLListsInvalidYAML(t *testing.T) {
	t.Parallel()

	path := checkWriteFile(t, "broken.yaml", "apiVersion: [unclosed\n")
	code, stdout, _ := cliJSONInvoke(t, checkCommand, path)
	if code != exitFailure {
		t.Fatalf("check(broken) 退出码 = %d, want 1", code)
	}
	var got checkJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout 不是合法 JSON: %v\n%s", err, stdout)
	}
	if len(got.Problems) == 0 || got.Problems[0].Code != "invalid_yaml" {
		t.Fatalf("problems = %+v, want 首条 invalid_yaml", got.Problems)
	}
}

// TestCheckMissingFileIsCommandError：文件读不到是**命令自身**的故障（stderr + kind），
// 与「产物有问题」（stdout + 没有 kind）分得开——否则 agent 会对着空 stdout 以为 yaml 没问题。
func TestCheckMissingFileIsCommandError(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "nope.yaml")
	code, stdout, stderr := cliJSONInvoke(t, checkCommand, missing)
	if code != exitFailure {
		t.Fatalf("check(缺失文件) 退出码 = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("文件读不到时 stdout 应为空（没有 problems 形状）：%q", stdout)
	}
	apiErr := cliDecodeError(t, stderr)
	if apiErr.Kind != kindConfigMissing {
		t.Fatalf("kind = %q, want %q（stderr: %s）", apiErr.Kind, kindConfigMissing, stderr)
	}
}

// TestCheckUsageErrors：位置参数缺失/多余都退 1，kind=usage（参数形状的问题）。
func TestCheckUsageErrors(t *testing.T) {
	t.Parallel()

	t.Run("没有位置参数", func(t *testing.T) {
		code, _, stderr := cliJSONInvoke(t, checkCommand)
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want 1", code)
		}
		if got := cliDecodeError(t, stderr).Kind; got != kindUsage {
			t.Fatalf("kind = %q, want %q", got, kindUsage)
		}
	})
	t.Run("两个位置参数", func(t *testing.T) {
		code, _, stderr := cliJSONInvoke(t, checkCommand, "a.yaml", "b.yaml")
		if code != exitFailure {
			t.Fatalf("退出码 = %d, want 1", code)
		}
		if got := cliDecodeError(t, stderr).Kind; got != kindUsage {
			t.Fatalf("kind = %q, want %q", got, kindUsage)
		}
	})
}

// TestCheckAcceptsFlagsAfterPath：`check x.yaml --json` 是最自然的写法（与 run / show 一致）。
func TestCheckAcceptsFlagsAfterPath(t *testing.T) {
	t.Parallel()

	path := checkWriteFile(t, "good.yaml", goodCapabilityYAML("/tmp/never-runs"))
	code, stdout, _ := cliJSONInvoke(t, checkCommand, path, "--json")
	if code != exitOK {
		t.Fatalf("退出码 = %d, want 0（stdout: %s）", code, stdout)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), `{"ok":true`) {
		t.Fatalf("--json 写在路径后面没生效：%s", stdout)
	}
}

// TestCheckHelpListsBoundaries：`check -h` 必须把三条边界（不执行 / 不连中枢 / 不落盘）
// 与退出码表都写出来——「agent 最容易把 run 当校验器用」正是这三行要挡的。
func TestCheckHelpListsBoundaries(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := mainWith([]string{checkCommand, "-h"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("check -h 退出码 = %d, want 0", code)
	}
	help := stderr.String()
	if help == "" {
		help = stdout.String()
	}
	for _, want := range []string{checkUsage, "不执行", "不连中枢", "不落盘", kindConfigMissing} {
		if !strings.Contains(help, want) {
			t.Errorf("check -h 缺 %q：\n%s", want, help)
		}
	}
}
