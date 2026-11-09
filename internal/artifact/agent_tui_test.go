package artifact

// 本文件覆盖内置模板 agent.tui：参数契约、脚本形状，以及**真的跑一遍**生成的脚本
// （fake hc + 真 git），把 worktree 三条路径、cleanup、哨兵、标签、blocked / 超时 /
// hc core 不可达这些分支逐条钉住。
//
// 为什么要跑脚本而不是只比字符串：这段脚本的价值全在「分支走对了没有」——
// worktree=off 却真的开了个 worktree、失败却把现场删了、timeout 当成失败直接
// 放弃而不是接着等。这些都不是字符串断言能看出来的。

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------- 参数契约 ----------

func TestAgentTUITemplateMeta(t *testing.T) {
	tpl, ok := LookupTemplate("agent.tui")
	if !ok {
		t.Fatal("agent.tui 模板不存在")
	}
	if tpl.Category != "agent" {
		t.Fatalf("category = %q，期望 agent", tpl.Category)
	}
	if tpl.TimeoutSec != 3600 {
		t.Fatalf("timeoutSec = %d，期望 3600", tpl.TimeoutSec)
	}
	if got, want := strings.Join(tpl.Requires, ","), "hc,git"; got != want {
		t.Fatalf("requires = %q，期望 %q（没装 hc 要在**部署期**失败）", got, want)
	}
	// 一任务一会话（不做 reuse）：reuse / onBusy 这类字段不许混进来。
	keys := fieldKeys(tpl)
	for _, banned := range []string{"reuse", "on_busy", "onbusy"} {
		if _, ok := keys[banned]; ok {
			t.Fatalf("不做会话复用，不该有字段 %q", banned)
		}
	}
	// 出口已从界面消失：不该再有 ports 字段。
	if _, ok := keys["ports"]; ok {
		t.Fatal("v4 没有端口概念：字段应当叫 emit_notes")
	}
	if _, ok := keys["emit_notes"]; !ok {
		t.Fatal("缺少 emit_notes 字段")
	}
}

func fieldKeys(tpl Template) map[string]Field {
	out := make(map[string]Field, len(tpl.Fields))
	for _, f := range tpl.Fields {
		out[f.Key] = f
	}
	return out
}

func TestAgentTUIFields(t *testing.T) {
	tpl, _ := LookupTemplate("agent.tui")
	keys := fieldKeys(tpl)
	want := map[string]string{
		"agent": "select", "agent_args": "string", "emit_notes": "text",
		"space": "string", "worktree": "select", "branch": "string",
		"base": "string", "worktree_path": "string", "worktree_cleanup": "select",
		"prompt": "text", "timeout_sec": "number",
	}
	if len(keys) != len(want) {
		t.Fatalf("字段数 %d，期望 %d：%v", len(keys), len(want), keys)
	}
	for key, typ := range want {
		f, ok := keys[key]
		if !ok {
			t.Fatalf("缺少字段 %q", key)
		}
		if f.Type != typ {
			t.Fatalf("字段 %q 的类型 = %q，期望 %q", key, f.Type, typ)
		}
	}
	if !keys["prompt"].Required {
		t.Fatal("prompt 必填")
	}
	if got := keys["agent"].Default; got != "pi" {
		t.Fatalf("agent 默认 %q", got)
	}
	if got := keys["worktree"].Default; got != "off" {
		t.Fatalf("worktree 默认 %q（默认不碰 git 现场）", got)
	}
	if got := keys["worktree_cleanup"].Default; got != "remove" {
		t.Fatalf("worktree_cleanup 默认 %q", got)
	}
	if got := keys["branch"].Default; got != "anotify/{{task8}}-{{seq}}" {
		t.Fatalf("branch 默认 %q", got)
	}
}

func TestAgentTUIParamsSchema(t *testing.T) {
	a, err := Render("agent.tui", RenderInput{Name: "acme.tui"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var schema struct {
		Required   []string                  `json:"required"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(a.Spec.Params, &schema); err != nil {
		t.Fatalf("params: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "prompt" {
		t.Fatalf("required = %v，期望 [prompt]", schema.Required)
	}
	for _, key := range []string{"agent", "worktree", "worktree_cleanup"} {
		if _, ok := schema.Properties[key]["enum"]; !ok {
			t.Fatalf("%q 应当有 enum（枚举而不是自由字符串）", key)
		}
	}
	if got := a.Spec.Lifecycle.EffectiveTimeoutSec(); got != 3600 {
		t.Fatalf("lifecycle.timeoutSec = %d，期望 3600", got)
	}
	if got := strings.Join(a.Spec.Requires.Commands, ","); got != "hc,git" {
		t.Fatalf("requires.commands = %q", got)
	}
}

func TestAgentTUIScriptShape(t *testing.T) {
	tpl, _ := LookupTemplate("agent.tui")
	s := tpl.Script
	must := map[string]string{
		"起会话":         "hc run --bg --json --outlive-parent --keep --no-notify",
		"标签 space":    `anotify.space=$SPACE`,
		"标签 task":     `anotify.task=$TASK_ID`,
		"标签 worktree": `anotify.worktree=`,
		"开分支":         `git -C "$REPO" worktree add -b "$BRANCH" "$WT_PATH" "$BASE"`,
		"detach":      "worktree add --detach",
		"清理只删目录":      `git -C "$REPO" worktree remove --force "$WT_PATH"`,
		"等哨兵":         `--output "$SENTINEL"`,
		"哨兵前缀":        `SENTINEL="ANOTIFY-STOP-$TASK_ID"`,
		"屏幕归档":        `hc read "$SESSION" --lines 400`,
		"身份文件":        `.anotify/task.env`,
		"0600":        `chmod 600 "$TASK_FILE"`,
		"emit 命令":     "anode emit",
		"幂等键":         "--id review-a.go",
		"core 不可达":    "hc_core_unreachable:",
		"blocked 前缀":  `die "blocked:`,
		"interrupt":   `hc interrupt "$SESSION"`,
		"hc 模板目录":     `.config/herdr-core/templates`,
		"任务上限":        "timeout_sec",
	}
	for name, frag := range must {
		if !strings.Contains(s, frag) {
			t.Fatalf("脚本里少了「%s」这段：%q", name, frag)
		}
	}
	// 模板层零插值：占位符以**字面量**出现在脚本里，由节点任务期展开。
	for _, lit := range []string{"{{task8}}", "{{seq}}"} {
		if !strings.Contains(s, lit) {
			t.Fatalf("脚本里没有 %s 这个字面占位符（模板层必须零插值）", lit)
		}
	}
	// token 只能出现在「写 task.env」这一条路径上：不许进提示词文案、不许进 argv。
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "ANOTIFY_TASK_TOKEN") &&
			!strings.Contains(line, "ANOTIFY_TASK_TOKEN=%s") {
			t.Fatalf("token 变量出现在不该出现的地方：%q", line)
		}
	}
	// v3 口径必须消失。
	for _, banned := range []string{"--port", "ANOTIFY_EMIT_SOCK", "--task-file", "ANOTIFY_WORKDIR"} {
		if strings.Contains(s, banned) {
			t.Fatalf("脚本还带着 v3 口径 %q", banned)
		}
	}
}

func TestAgentTUIScriptSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("没有 bash，无法校验脚本语法：%v", err)
	}
	tpl, _ := LookupTemplate("agent.tui")
	path := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(path, []byte(tpl.Script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out, err := exec.Command(bash, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n 失败：%v\n%s", err, out)
	}
}

// ---------- 真的跑一遍（fake hc + 真 git） ----------

// fakeHC 是「hc 那一侧」的替身：记录每一次调用（argv），并按 FAKE_MODE 给出
// 不同分支的响应。它只模仿我们这个脚本用到的那几个子命令，多一个都不做。
const fakeHC = `#!/bin/bash
{ printf '%s' "$1"; for a in "${@:2}"; do printf '\t%s' "$a"; done; printf '\n'; } >> "$FAKE_LOG"
cmd="$1"
mode="${FAKE_MODE:-ok}"
case "$cmd" in
  server)
    if [ "$mode" = core-down ]; then echo '{"error":{"kind":"unavailable","message":"no core"}}'; exit 1; fi
    echo '{"live":1}'; exit 0 ;;
  ls)
    if [ "$mode" = names ]; then
      echo '{"terminals":[{"address":"t9","name":"anotify-tsk-321-1"},{"address":"t8","name":"anotify-tsk-321-2"}]}'
    else
      echo '{"terminals":[]}'
    fi
    exit 0 ;;
  run)
    if [ "$mode" = run-fail ]; then echo '{"error":{"kind":"spawn_failed","message":"no pi"}}'; exit 1; fi
    if [ "$mode" = not-sent ]; then
      echo '{"address":"t1","first_message":{"reason":"it exited before it was ready","sent":false}}'
      exit 0
    fi
    echo '{"address":"t1","name":"x","first_message":{"reason":"ready","sent":true}}'
    exit 0 ;;
  wait)
    case "$mode" in
      blocked) echo '{"ended":true,"reason":"blocked","status":"blocked"}' ;;
      error)   echo '{"ended":true,"reason":"error","status":"error"}' ;;
      exit3)   echo '{"ended":true,"reason":"exit","exit_code":3}' ;;
      forever) echo '{"ended":false,"reason":"timeout"}' ;;
      *)       echo '{"ended":true,"reason":"output"}' ;;
    esac
    exit 0 ;;
  get)
    case "$mode" in
      exit3)   echo '{"state":"finished","status":null,"exit_code":3}' ;;
      blocked) echo '{"state":"live","status":"blocked","exit_code":null}' ;;
      *)       echo '{"state":"live","status":"idle","exit_code":null}' ;;
    esac
    exit 0 ;;
  read) printf 'SCREEN-A\nSCREEN-B\n'; exit 0 ;;
  interrupt) echo '{"outcome":"stopped"}'; exit 0 ;;
  *) exit 0 ;;
esac
`

// fakeID / fakeGetent：让脚本里的「OS 用户家目录」落到测试的临时目录，
// 而不是真的写进 ~/.config/herdr-core/templates。
const fakeID = `#!/bin/bash
case "$1" in
  -u) echo 1000 ;;
  -un) echo tester ;;
  *) echo 1000 ;;
esac
`

const fakeGetent = `#!/bin/bash
echo "tester:x:1000:1000::$(dirname "$FAKE_LOG"):/bin/bash"
`

type tuiScenario struct {
	mode   string            // FAKE_MODE
	params map[string]string // 覆盖环境变量
	prep   func(t *testing.T, root string)
}

type tuiRun struct {
	code   int
	stdout string
	stderr string
	root   string
	repo   string
	wt     string
	hclog  []string
}

func (r tuiRun) hcCalls(sub string) []string {
	var out []string
	for _, l := range r.hclog {
		if strings.HasPrefix(l, sub+"\t") {
			out = append(out, l)
		}
	}
	return out
}

const (
	tuiTaskID = "tsk_321"
	tuiToken  = "tok_should_never_reach_argv"
	tuiSlug   = "tsk-321"
)

func runAgentTUI(t *testing.T, sc tuiScenario) tuiRun {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("没有 git，未安装无法验证 worktree 分支：%v", err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	bin := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")
	for _, d := range []string{repo, bin, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for name, body := range map[string]string{"hc": fakeHC, "id": fakeID, "getent": fakeGetent} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	gitExec(t, repo, "init", "-q", ".")
	gitExec(t, repo, "config", "user.email", "t@example.com")
	gitExec(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	gitExec(t, repo, "add", "a.txt")
	gitExec(t, repo, "commit", "-qm", "init")

	wt := filepath.Join(repo, "wt")
	if sc.prep != nil {
		sc.prep(t, root)
	}

	tpl, _ := LookupTemplate("agent.tui")
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(tpl.Script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	env := map[string]string{
		"PATH":                  bin + ":" + os.Getenv("PATH"),
		"HOME":                  home,
		"FAKE_LOG":              filepath.Join(root, "hc.log"),
		"FAKE_MODE":             sc.mode,
		"ANOTIFY_TASK_ID":       tuiTaskID,
		"ANOTIFY_TASK_TOKEN":    tuiToken,
		"ANOTIFY_EMIT_ENDPOINT": "http://127.0.0.1:41237",
		"ANOTIFY_PARAM_AGENT":   "pi",
		"ANOTIFY_PARAM_PROMPT":  "修好 #321",
	}
	// 其余参数用脚本自己的默认值（这样默认值也被真跑一遍）；map 里有这个 key
	// 就一定是显式（含显式空串 → 脚本的 `${X+x}` 分支）。
	for k, v := range sc.params {
		env[k] = strings.ReplaceAll(v, "PLACEHOLDER", root)
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = repo // 工作目录就是任务目录（exec 的 cwd）
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("跑脚本失败：%v\nstderr=%s", err, stderr.String())
		}
		code = exitErr.ExitCode()
	}

	logBody, _ := os.ReadFile(filepath.Join(root, "hc.log"))
	var hclog []string
	for _, l := range strings.Split(strings.TrimSpace(string(logBody)), "\n") {
		if l != "" {
			hclog = append(hclog, l)
		}
	}
	return tuiRun{
		code: code, stdout: stdout.String(), stderr: stderr.String(),
		root: root, repo: repo, wt: wt, hclog: hclog,
	}
}

func gitExec(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func templateDir(root string) string {
	return filepath.Join(root, ".config", "herdr-core", "templates")
}

// firstLine 返回 stdout 的第一行（= runner 眼里的 summary）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestAgentTUIWorktreeNewRemove：开分支 → 成功 → 目录删掉、分支留下、标签与 cwd 正确。
func TestAgentTUIWorktreeNewRemove(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{params: map[string]string{
		"ANOTIFY_PARAM_WORKTREE":         "new",
		"ANOTIFY_PARAM_AGENT_ARGS":       "--model gpt-6.1-sol --thinking medium",
		"ANOTIFY_PARAM_EMIT_NOTES":       "修好后发 {\"summary\":\"done\"}",
		"ANOTIFY_PARAM_SPACE":            "work",
		"ANOTIFY_PARAM_WORKTREE_CLEANUP": "remove",
	}})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
	}
	// 会话名：任务 id 里的非 [a-z0-9-] 字符换成短横线。
	if got := firstLine(r.stdout); !strings.Contains(got, "任务 "+tuiTaskID) ||
		!strings.Contains(got, "anotify-"+tuiSlug+"-1") {
		t.Fatalf("summary 不对：%q", got)
	}
	if _, err := os.Stat(r.wt); !os.IsNotExist(err) {
		t.Fatalf("cleanup=remove 应当删掉 worktree 目录 %s（err=%v）", r.wt, err)
	}
	branch := "anotify/" + tuiSlug + "-1"
	if branches := gitExec(t, r.repo, "branch", "--list", branch); !strings.Contains(branches, branch) {
		t.Fatalf("分支应当保留（只删目录不删分支），实际 %q", branches)
	}
	if got := gitExec(t, r.repo, "worktree", "list"); strings.Contains(got, r.wt) {
		t.Fatalf("git worktree list 里不该还有它：\n%s", got)
	}

	run := r.hcCalls("run")
	if len(run) != 1 {
		t.Fatalf("应当只 hc run 一次，实际 %d：%v", len(run), run)
	}
	line := run[0]
	for _, want := range []string{
		"--bg", "--outlive-parent", "--keep", "--no-notify",
		"--name\tanotify-" + tuiSlug + "-1",
		"--label\tanotify.space=work",
		"--label\tanotify.task=" + tuiTaskID,
		"--label\tanotify.worktree=" + branch,
		"--cwd\t" + r.wt,
		"--\tanotify-" + tuiSlug,
		"\t修好 #321",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("hc run 少了 %q：\n%s", want, line)
		}
	}
	if strings.Contains(line, tuiToken) || strings.Contains(line, "tok_should") {
		t.Fatalf("token 进了 argv：\n%s", line)
	}

	// hc 模板文件：command 行来自 agent + agent_args，正文是 v4 的 emit 契约。
	md, err := os.ReadFile(filepath.Join(templateDir(r.root), "anotify-"+tuiSlug+".md"))
	if err != nil {
		t.Fatalf("读不到 hc 模板：%v", err)
	}
	for _, want := range []string{
		"name: anotify-" + tuiSlug,
		"command: pi --model gpt-6.1-sol --thinking medium",
		". .anotify/task.env",
		`修好后发 {"summary":"done"}`,
		"ANOTIFY-STOP-<taskId>",
		"anode emit <下游能力>",
	} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("hc 模板里少了 %q：\n%s", want, md)
		}
	}
	if strings.Contains(string(md), tuiToken) {
		t.Fatal("token 进了提示词正文（屏幕归档里会有明文）")
	}
	// hc 的 frontmatter 是原文、不剥引号：值一旦被引号包起来，command 就执行不了。
	if strings.Contains(string(md), `command: "`) {
		t.Fatalf("command 行不许加引号（hc 不解析 YAML 引号）：\n%s", md)
	}

	// 成功路径也要归档屏幕。
	if !strings.Contains(r.stdout, "SCREEN-A") || !strings.Contains(r.stdout, "--- hc read ") {
		t.Fatalf("stdout 里应当有屏幕归档：\n%s", r.stdout)
	}
	// 收现场的结果进 DETAIL（stdout 第二行）。
	if !strings.Contains(r.stdout, "已删除") || !strings.Contains(r.stdout, "分支 "+branch+" 保留") {
		t.Fatalf("stdout 应当说明 worktree 已清理：\n%s", r.stdout)
	}
}

// TestAgentTUIWorktreeNewKeep：keep 时目录留下，task.env 是 0600 且可被 source。
func TestAgentTUIWorktreeNewKeep(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{params: map[string]string{
		"ANOTIFY_PARAM_WORKTREE":         "new",
		"ANOTIFY_PARAM_WORKTREE_CLEANUP": "keep",
	}})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(r.wt); err != nil {
		t.Fatalf("cleanup=keep 应当保留 worktree 目录：%v", err)
	}
	taskFile := filepath.Join(r.wt, ".anotify", "task.env")
	info, err := os.Stat(taskFile)
	if err != nil {
		t.Fatalf("读不到 task.env：%v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("task.env 权限 = %04o，必须 0600（里面有一次性 token）", perm)
	}
	body, err := os.ReadFile(taskFile)
	if err != nil {
		t.Fatalf("读 task.env：%v", err)
	}
	for _, want := range []string{
		"ANOTIFY_EMIT_ENDPOINT='http://127.0.0.1:41237'",
		"ANOTIFY_TASK_TOKEN='" + tuiToken + "'",
		"ANOTIFY_TASK_ID='" + tuiTaskID + "'",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("task.env 少了 %q：\n%s", want, body)
		}
	}
	// 这个文件是给 agent **source** 的：能被 bash 读回同样的值才算契约成立。
	sourceCmd := exec.Command("bash", "-c", `. .anotify/task.env && printf '%s|%s|%s' "$ANOTIFY_TASK_ID" "$ANOTIFY_TASK_TOKEN" "$ANOTIFY_EMIT_ENDPOINT"`)
	sourceCmd.Dir = r.wt
	out, err := sourceCmd.Output()
	if err != nil {
		t.Fatalf("source task.env 失败：%v", err)
	}
	if strings.TrimSpace(string(out)) != tuiTaskID+"|"+tuiToken+"|http://127.0.0.1:41237" {
		t.Fatalf("source 出来的值不对：%q", out)
	}
	// 让错误的工作目录跑一次 source，确认它拒绝（文件只在任务目录里）。
	if err := exec.Command("bash", "-c", `cd / && . .anotify/task.env 2>/dev/null`).Run(); err == nil {
		t.Fatal("task.env 不该在任务目录外可用")
	}
	// .anotify/ 必须对整个 git 隐身：否则 agent 一句 git add -A 就把 token 追进提交。
	ignore := filepath.Join(r.wt, ".anotify", ".gitignore")
	if ignoreBody, err := os.ReadFile(ignore); err != nil || strings.TrimSpace(string(ignoreBody)) != "*" {
		t.Fatalf(".anotify/.gitignore 应当是 *（err=%v body=%q）", err, ignoreBody)
	}
	if out, err := exec.Command("git", "-C", r.wt, "check-ignore", "-q", ".anotify/task.env").CombinedOutput(); err != nil {
		t.Fatalf(".anotify/task.env 必须被 git 忽略：%v\n%s", err, out)
	}
}

// TestAgentTUIWorktreeExisting：existing 复用现成目录，且不自动删除。
func TestAgentTUIWorktreeExisting(t *testing.T) {
	var existing string
	r := runAgentTUI(t, tuiScenario{
		params: map[string]string{
			"ANOTIFY_PARAM_WORKTREE":      "existing",
			"ANOTIFY_PARAM_WORKTREE_PATH": "PLACEHOLDER/existing-wt",
		},
		prep: func(t *testing.T, root string) {
			existing = filepath.Join(root, "existing-wt")
			if err := os.MkdirAll(existing, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		},
	})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	run := r.hcCalls("run")
	if len(run) != 1 || !strings.Contains(run[0], "--cwd\t"+existing) {
		t.Fatalf("cwd 应当是 existing 目录：%v", run)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Fatalf("existing 目录不该被删：%v", err)
	}
	if !strings.Contains(r.stdout, "existing") {
		t.Fatalf("stdout 应当说明是复用目录：%s", r.stdout)
	}
	// existing 缺 worktree_path 必须拒绝。
	if r := runAgentTUI(t, tuiScenario{params: map[string]string{"ANOTIFY_PARAM_WORKTREE": "existing"}}); r.code == 0 {
		t.Fatal("existing 不给 worktree_path 必须失败")
	}
}

// TestAgentTUIWorktreeOff：off 直接用工作目录，不碰 git。
func TestAgentTUIWorktreeOff(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{params: map[string]string{"ANOTIFY_PARAM_WORKTREE": "off"}})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	run := r.hcCalls("run")
	if len(run) != 1 || !strings.Contains(run[0], "--cwd\t"+r.repo) {
		t.Fatalf("cwd 应当是工作目录本身：%v", run)
	}
	if strings.Contains(run[0], "anotify.worktree=") {
		t.Fatalf("worktree=off 不该打 worktree 标签：%s", run[0])
	}
	if _, err := os.Stat(r.wt); !os.IsNotExist(err) {
		t.Fatal("worktree=off 却建了 wt 目录")
	}
	if strings.Contains(r.stderr, "worktree:") {
		t.Fatalf("worktree=off 不该有 worktree 日志：%s", r.stderr)
	}
}

// TestAgentTUIBlocked：blocked = 失败 + summary 前缀 blocked:，现场保留。
func TestAgentTUIBlocked(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{mode: "blocked", params: map[string]string{
		"ANOTIFY_PARAM_WORKTREE":         "new",
		"ANOTIFY_PARAM_WORKTREE_CLEANUP": "remove", // 失败时取值无效：必须保留
	}})
	if r.code == 0 {
		t.Fatalf("blocked 必须是失败\nstdout=%s", r.stdout)
	}
	if got := firstLine(r.stdout); !strings.HasPrefix(got, "blocked:") {
		t.Fatalf("summary 必须以 blocked: 开头，实际 %q", got)
	}
	if _, err := os.Stat(r.wt); err != nil {
		t.Fatalf("失败必须保留现场（worktree 还在）：%v", err)
	}
}

// TestAgentTUIDeadline：wait 一直 timeout → 到 timeout_sec 就 interrupt 并失败。
func TestAgentTUIDeadline(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{mode: "forever", params: map[string]string{
		"ANOTIFY_PARAM_WORKTREE":    "new",
		"ANOTIFY_PARAM_TIMEOUT_SEC": "1",
	}})
	if r.code == 0 {
		t.Fatalf("超时必须是失败\nstdout=%s", r.stdout)
	}
	if got := firstLine(r.stdout); !strings.HasPrefix(got, "timeout:") {
		t.Fatalf("summary 必须以 timeout: 开头，实际 %q", got)
	}
	if len(r.hcCalls("interrupt")) != 1 {
		t.Fatalf("超时要 hc interrupt 一次：%v", r.hclog)
	}
	if _, err := os.Stat(r.wt); err != nil {
		t.Fatalf("超时必须保留现场：%v", err)
	}
	if len(r.hcCalls("wait")) < 1 {
		t.Fatal("根本没等过")
	}
}

// TestAgentTUICoreUnreachable：hc core 不可达 → summary 前缀 hc_core_unreachable:。
func TestAgentTUICoreUnreachable(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{mode: "core-down"})
	if r.code == 0 {
		t.Fatalf("core 不可达必须失败\nstdout=%s", r.stdout)
	}
	if got := firstLine(r.stdout); !strings.HasPrefix(got, "hc_core_unreachable:") {
		t.Fatalf("summary 必须以 hc_core_unreachable: 开头，实际 %q", got)
	}
	for _, bad := range r.hcCalls("run") {
		t.Fatalf("core 不可达时不该还去 hc run：%s", bad)
	}
}

// TestAgentTUIRunFailures：起不来 / 首条消息没送到，都要说清楚，不许当成「跑完了」。
func TestAgentTUIRunFailures(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"run-fail", "hc_run_failed:"},
		{"not-sent", "hc_run_failed:"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := runAgentTUI(t, tuiScenario{mode: tc.mode})
			if r.code == 0 {
				t.Fatalf("必须失败\nstdout=%s", r.stdout)
			}
			if got := firstLine(r.stdout); !strings.HasPrefix(got, tc.want) {
				t.Fatalf("summary 必须以 %s 开头，实际 %q", tc.want, got)
			}
		})
	}
}

// TestAgentTUIExitNonZero：agent 自己退出且没打哨兵 → 失败（不许猜成功）。
func TestAgentTUIExitNonZero(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{mode: "exit3"})
	if r.code == 0 {
		t.Fatalf("exit_code=3 必须失败\nstdout=%s", r.stdout)
	}
	if got := firstLine(r.stdout); !strings.HasPrefix(got, "agent_exit:") {
		t.Fatalf("summary 必须以 agent_exit: 开头，实际 %q", got)
	}
}

// TestAgentTUISeqCycles：同一 task 已有两条会话 → 这一次叫 -3（重试不撞名）。
func TestAgentTUISeqCycles(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{mode: "names"})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	run := r.hcCalls("run")
	if len(run) != 1 || !strings.Contains(run[0], "--name\tanotify-"+tuiSlug+"-3") {
		t.Fatalf("会话名应当带 -3：%v", run)
	}
	ls := r.hcCalls("ls")
	if len(ls) != 1 || !strings.Contains(ls[0], "anotify.task="+tuiTaskID) {
		t.Fatalf("hc ls 应当按 task 标签过滤：%v", ls)
	}
}

// TestAgentTUISanitizesTaskID：cron 的任务 id 带冒号，会话名/分支名不许带。
//
// cron 的任务 id 形如 "cron:nightly:1730000000"。直接截 8 位会得到 "cron:nig"，
// 而 hc 的 --name 只收 [a-z0-9-_]、git ref 也不收冒号——症状是「cron 派的任务
// 一条也起不来」，而手工派的任务全好。
func TestAgentTUISanitizesTaskID(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{
		params: map[string]string{
			"ANOTIFY_TASK_ID":        "cron:Nightly:1730000000",
			"ANOTIFY_PARAM_WORKTREE": "new",
		},
	})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	run := r.hcCalls("run")
	if len(run) != 1 {
		t.Fatalf("hc run 次数不对：%v", run)
	}
	if !strings.Contains(run[0], "--name\tanotify-cron-nig-1") {
		t.Fatalf("会话名必须只含 [a-z0-9-_]：%s", run[0])
	}
	if !strings.Contains(run[0], "--label\tanotify.task=cron:Nightly:1730000000") {
		t.Fatalf("标签里要留**原始**任务 id（过滤/追溯用）：%s", run[0])
	}
	if branches := gitExec(t, r.repo, "branch", "--list"); !strings.Contains(branches, "anotify/cron-nig-1") {
		t.Fatalf("分支名也要 sanitize：%q", branches)
	}
}

// TestAgentTUIBranchEmptyDetaches：显式给空 branch = --detach。
func TestAgentTUIBranchEmptyDetaches(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{params: map[string]string{
		"ANOTIFY_PARAM_WORKTREE": "new",
		"ANOTIFY_PARAM_BRANCH":   "", // 显式空串
	}})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	if !strings.Contains(r.stderr, "branch=<detached>") {
		t.Fatalf("stderr 应当显示用了 --detach：%s", r.stderr)
	}
	run := r.hcCalls("run")
	if len(run) != 1 || !strings.Contains(run[0], "--label\tanotify.worktree="+r.wt) {
		t.Fatalf("detach 时 worktree 标签应当回退到目录：%v", run)
	}
}

// TestAgentTUIBranchExpansion：{{task8}} / {{seq}} 由脚本展开。
func TestAgentTUIBranchExpansion(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{params: map[string]string{
		"ANOTIFY_PARAM_WORKTREE": "new",
		"ANOTIFY_PARAM_BRANCH":   "release/{{task8}}-{{seq}}",
	}})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	want := "release/" + tuiSlug + "-1"
	if branches := gitExec(t, r.repo, "branch", "--list", want); !strings.Contains(branches, want) {
		t.Fatalf("分支应当展开成 %q，实际 %q", want, branches)
	}
}

// TestAgentTUIParamValidation：非法参数值必须在起会话前就失败（exit 2）。
func TestAgentTUIParamValidation(t *testing.T) {
	cases := map[string]map[string]string{
		"没有 prompt":   {"ANOTIFY_PARAM_PROMPT": ""},
		"不认识的 agent":  {"ANOTIFY_PARAM_AGENT": "not-an-agent"},
		"timeout 非数字": {"ANOTIFY_PARAM_TIMEOUT_SEC": "abc"},
		"timeout 为 0": {"ANOTIFY_PARAM_TIMEOUT_SEC": "0"},
		"worktree 非法": {"ANOTIFY_PARAM_WORKTREE": "maybe"},
		"cleanup 非法":  {"ANOTIFY_PARAM_WORKTREE_CLEANUP": "delete"},
		"引号不配对":       {"ANOTIFY_PARAM_AGENT_ARGS": "--x 'unterminated"},
		"参数含换行":       {"ANOTIFY_PARAM_AGENT_ARGS": "--x\ny"},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			r := runAgentTUI(t, tuiScenario{params: params})
			if r.code != 2 {
				t.Fatalf("参数错误必须以 2 退出，实际 %d\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
			}
			if got := firstLine(r.stdout); !strings.HasPrefix(got, "invalid_param:") {
				t.Fatalf("summary 必须以 invalid_param: 开头，实际 %q", got)
			}
			if len(r.hcCalls("run")) != 0 {
				t.Fatal("参数错误时不该起会话")
			}
		})
	}
}

// TestAgentTUIBalancedQuotesOK：引号配对（偶数个）的 agent_args 允许通过。
func TestAgentTUIBalancedQuotesOK(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{params: map[string]string{
		"ANOTIFY_PARAM_AGENT_ARGS": `--prompt "two words" --x 'y'`,
	}})
	if r.code != 0 {
		t.Fatalf("配对引号应当通过，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	md, err := os.ReadFile(filepath.Join(templateDir(r.root), "anotify-"+tuiSlug+".md"))
	if err != nil {
		t.Fatalf("读模板：%v", err)
	}
	if !strings.Contains(string(md), `command: pi --prompt "two words" --x 'y'`) {
		t.Fatalf("agent_args 应当原样进 command 行：\n%s", md)
	}
}

// TestAgentTUICancelInterrupts：收到 TERM 就 interrupt 会话（别把 working 的终端留下）。
func TestAgentTUICancelInterrupts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("没有 git：%v", err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	bin := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")
	for _, d := range []string{repo, bin, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// 慢速 hc：让脚本停在 wait 循环里，好让我们发信号。
	slowHC := strings.Replace(fakeHC, `      forever) echo '{"ended":false,"reason":"timeout"}' ;;`,
		`      forever) sleep 30; echo '{"ended":false,"reason":"timeout"}' ;;`, 1)
	if err := os.WriteFile(filepath.Join(bin, "hc"), []byte(slowHC), 0o755); err != nil {
		t.Fatalf("write hc: %v", err)
	}
	for name, body := range map[string]string{"id": fakeID, "getent": fakeGetent} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	tpl, _ := LookupTemplate("agent.tui")
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(tpl.Script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = repo
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"HOME=" + home,
		"FAKE_LOG=" + filepath.Join(root, "hc.log"),
		"FAKE_MODE=forever",
		"ANOTIFY_TASK_ID=" + tuiTaskID,
		"ANOTIFY_TASK_TOKEN=" + tuiToken,
		"ANOTIFY_EMIT_ENDPOINT=http://127.0.0.1:41237",
		"ANOTIFY_PARAM_AGENT=pi",
		"ANOTIFY_PARAM_PROMPT=慢慢来",
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 等它起完会话再发 TERM（hc run 之后才 wait）。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if body, _ := os.ReadFile(filepath.Join(root, "hc.log")); strings.Contains(string(body), "wait\t") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("应当以非零退出：%v", err)
	}
	if got := exitErr.ExitCode(); got != 143 {
		t.Fatalf("cancel 退出码 = %d，期望 143（128+SIGTERM）\nstderr=%s", got, stderr.String())
	}
	logBody, _ := os.ReadFile(filepath.Join(root, "hc.log"))
	if !strings.Contains(string(logBody), "interrupt\t") {
		t.Fatalf("收到 TERM 必须 hc interrupt 会话：\n%s", logBody)
	}
	if !strings.Contains(stdout.String(), "canceled:") {
		t.Fatalf("summary 必须说明被取消：%s", stdout.String())
	}
}
