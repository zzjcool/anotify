package artifact

// 本文件覆盖内置模板 agent.tui（issue #321）：参数契约、脚本形状，以及**真的跑一遍**
// 生成的脚本（fake hc + 真 git），把 worktree 三条路径、cleanup、哨兵、标签、
// blocked / 超时 / hc core 不可达这些分支逐条钉住。
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
	// P0 已定：一任务一会话。reuse / onBusy 这类字段**不许**混进来。
	keys := fieldKeys(tpl)
	for _, banned := range []string{"reuse", "on_busy", "onbusy"} {
		if _, ok := keys[banned]; ok {
			t.Fatalf("P0 不做会话复用，不该有字段 %q", banned)
		}
	}
}

func TestAgentTUIFields(t *testing.T) {
	tpl, _ := LookupTemplate("agent.tui")
	want := []struct {
		key   string
		typ   string
		def   string
		req   bool
		opts  []string
		label string
	}{
		{key: "agent", typ: "select", def: "pi",
			opts: []string{"pi", "claude", "codex", "gemini", "cursor", "opencode"}},
		{key: "agent_args", typ: "string"},
		{key: "ports", typ: "text"},
		{key: "space", typ: "string", def: "default"},
		{key: "worktree", typ: "select", def: "off", opts: []string{"off", "new", "existing"}},
		{key: "branch", typ: "string", def: "anotify/{{trace8}}-{{seq}}"},
		{key: "base", typ: "string", def: "HEAD"},
		{key: "worktree_path", typ: "string"},
		{key: "worktree_cleanup", typ: "select", def: "remove", opts: []string{"remove", "keep"}},
		{key: "prompt", typ: "text", req: true},
		{key: "timeout_sec", typ: "number", def: "3600"},
	}
	if len(tpl.Fields) != len(want) {
		t.Fatalf("字段数 = %d，期望 %d（issue #321 的参数表）", len(tpl.Fields), len(want))
	}
	for i, w := range want {
		got := tpl.Fields[i]
		if got.Key != w.key {
			t.Fatalf("第 %d 个字段是 %q，期望 %q（顺序也是契约：表单按这个顺序渲染）",
				i, got.Key, w.key)
		}
		if got.Type != w.typ {
			t.Fatalf("%s.type = %q，期望 %q", w.key, got.Type, w.typ)
		}
		if got.Default != w.def {
			t.Fatalf("%s.default = %q，期望 %q", w.key, got.Default, w.def)
		}
		if got.Required != w.req {
			t.Fatalf("%s.required = %v，期望 %v", w.key, got.Required, w.req)
		}
		if strings.Join(got.Options, ",") != strings.Join(w.opts, ",") {
			t.Fatalf("%s.options = %v，期望 %v", w.key, got.Options, w.opts)
		}
		if strings.TrimSpace(got.Help) == "" {
			t.Fatalf("%s 没有 help：控制台表单里这一项会是个哑输入框", w.key)
		}
	}
}

// TestAgentTUIParamsSchema：select 必须带 enum（agent 是枚举不是自由字符串），
// number 必须是 number（否则表单会当字符串发，派发期被 schema 拒）。
func TestAgentTUIParamsSchema(t *testing.T) {
	a, err := Render("agent.tui", RenderInput{Name: "demo.tui"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var schema struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(a.Spec.Params, &schema); err != nil {
		t.Fatalf("params 不是合法 JSON：%v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "prompt" {
		t.Fatalf("required = %v，期望只有 prompt", schema.Required)
	}
	for key, wantEnum := range map[string][]string{
		"agent":            {"pi", "claude", "codex", "gemini", "cursor", "opencode"},
		"worktree":         {"off", "new", "existing"},
		"worktree_cleanup": {"remove", "keep"},
	} {
		var prop struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		}
		if err := json.Unmarshal(schema.Properties[key], &prop); err != nil {
			t.Fatalf("%s 属性解析失败：%v", key, err)
		}
		if prop.Type != "string" || strings.Join(prop.Enum, ",") != strings.Join(wantEnum, ",") {
			t.Fatalf("%s = %+v，期望 string %v", key, prop, wantEnum)
		}
	}
	var timeout struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(schema.Properties["timeout_sec"], &timeout); err != nil {
		t.Fatalf("timeout_sec 属性解析失败：%v", err)
	}
	if timeout.Type != "number" {
		t.Fatalf("timeout_sec.type = %q，期望 number", timeout.Type)
	}
}

func fieldKeys(tpl Template) map[string]Field {
	out := make(map[string]Field, len(tpl.Fields))
	for _, f := range tpl.Fields {
		out[f.Key] = f
	}
	return out
}

// ---------- 脚本形状 ----------

// TestAgentTUIScriptShape：钉住脚本里**不许丢**的东西（丢了大分支就没了）。
func TestAgentTUIScriptShape(t *testing.T) {
	tpl, _ := LookupTemplate("agent.tui")
	s := tpl.Script
	must := map[string]string{
		"起会话":          "hc run --bg --json --outlive-parent --keep --no-notify",
		"标签 space":     `anotify.space=$SPACE`,
		"标签 cap":       `anotify.cap=$CAP_FULL`,
		"标签 trace":     `anotify.trace=$TRACE_ID`,
		"标签 worktree":  `anotify.worktree=`,
		"开分支 worktree": "git -C \"$REPO\" worktree add -b \"$BRANCH\" \"$WT_PATH\" \"$BASE\"",
		"detach":       "worktree add --detach",
		"清理只删目录":       "git -C \"$REPO\" worktree remove --force \"$WT_PATH\"",
		"等哨兵":          `--output "$SENTINEL"`,
		"哨兵前缀":         `SENTINEL="ANOTIFY-STOP-$TASK_ID"`,
		"屏幕归档":         `hc read "$SESSION" --lines 400`,
		"task.json":    "--task-file .anotify/task.json",
		"0600":         "chmod 600 \"$TASK_FILE\"",
		"core 不可达":     "hc_core_unreachable:",
		"blocked 前缀":   `die "blocked:`,
		"interrupt":    `hc interrupt "$SESSION"`,
		"hc 模板目录":      `.config/herdr-core/templates`,
	}
	for name, frag := range must {
		if !strings.Contains(s, frag) {
			t.Fatalf("脚本里少了「%s」这段：%q", name, frag)
		}
	}
	// 模板层零插值：占位符以**字面量**出现在脚本里，由节点任务期展开。
	for _, lit := range []string{"{{trace8}}", "{{seq}}"} {
		if !strings.Contains(s, lit) {
			t.Fatalf("脚本里没有 %s 这个字面占位符（模板层必须零插值）", lit)
		}
	}
	// token 只能出现在「写 task.json」这一条路径上：不许进提示词文案、不许进 argv。
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "ANOTIFY_TASK_TOKEN") && !strings.Contains(line, "TOKEN=") {
			t.Fatalf("token 变量出现在不该出现的地方：%q", line)
		}
	}
	if strings.Contains(s, "emit --port") || strings.Contains(s, "--port ") {
		t.Fatal("端口已从界面消失：提示词里不许教用户写 --port（单出口自动取）")
	}
}

func TestAgentTUIScriptSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("没有 bash，跳过语法检查")
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
// 不同分支的响应。它只模仿**我们这个脚本用到的那几个子命令**，多一个都不做。
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
      echo '{"terminals":[{"address":"t9","name":"anotify-demo-abc123de-1"},{"address":"t8","name":"anotify-demo-abc123de-2"}]}'
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
      slowwait) sleep 0.2; forever_loops=$(( ${forever_loops:-0} + 1 )); echo '{"ended":false,"reason":"timeout"}' ;;
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
	params map[string]string // 覆盖 ANOTIFY_PARAM_*
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
	tuiTrace = "1a2b3c4d5e6f7788"
	tuiToken = "tok_should_never_reach_argv"
)

func runAgentTUI(t *testing.T, sc tuiScenario) tuiRun {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git，跳过")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	bin := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")
	for _, d := range []string{repo, bin, home, filepath.Join(root, "work")} {
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
		"PATH":                 bin + ":" + os.Getenv("PATH"),
		"HOME":                 home,
		"FAKE_LOG":             filepath.Join(root, "hc.log"),
		"FAKE_MODE":            sc.mode,
		"ANOTIFY_WORKDIR":      repo,
		"ANOTIFY_TASKDIR":      filepath.Join(root, "work"),
		"ANOTIFY_TASK_ID":      "tsk_321",
		"ANOTIFY_TRACE_ID":     tuiTrace,
		"ANOTIFY_CAPABILITY":   "demo.tui@2",
		"ANOTIFY_DIGEST":       "sha256:deadbeef",
		"ANOTIFY_EMIT_SOCK":    filepath.Join(root, "emit.sock"),
		"ANOTIFY_TASK_TOKEN":   tuiToken,
		"ANOTIFY_EMIT_PORTS":   `[{"port":"out","schema":null,"targets":[{"capability":"script.echo","version":1,"priority":0}]}]`,
		"ANOTIFY_AGENT_BIN":    "/opt/anotify/bin/anotify-agent",
		"ANOTIFY_PARAM_AGENT":  "pi",
		"ANOTIFY_PARAM_PROMPT": "修好 #321",
	}
	// 其余参数用脚本自己的默认值（这样默认值也被真跑一遍）；map 里有这个 key
	// 就一定是显式（含显式空串 → 脚本的 `${X+x}` 分支）。
	for k, v := range sc.params {
		env[k] = strings.ReplaceAll(v, "PLACEHOLDER", root)
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = filepath.Join(root, "work")
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
		"ANOTIFY_PARAM_PORTS":            "修好后发 {\"summary\":\"done\"}",
		"ANOTIFY_PARAM_SPACE":            "work",
		"ANOTIFY_PARAM_WORKTREE_CLEANUP": "remove",
	}})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
	}
	// 会话名：能力名里的点换成短横线（hc 的 --name 只收 a-z0-9-_）。
	if got := firstLine(r.stdout); !strings.Contains(got, "agent.tui: demo.tui@2") ||
		!strings.Contains(got, "anotify-demo-tui-1a2b3c4d-1") {
		t.Fatalf("summary 不对：%q", got)
	}
	if _, err := os.Stat(r.wt); !os.IsNotExist(err) {
		t.Fatalf("cleanup=remove 应当删掉 worktree 目录 %s（err=%v）", r.wt, err)
	}
	if branches := gitExec(t, r.repo, "branch", "--list", "anotify/1a2b3c4d-1"); !strings.Contains(branches, "anotify/1a2b3c4d-1") {
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
		"--name\tanotify-demo-tui-1a2b3c4d-1",
		"--label\tanotify.space=work",
		"--label\tanotify.cap=demo.tui@2",
		"--label\tanotify.trace=" + tuiTrace,
		"--label\tanotify.worktree=anotify/1a2b3c4d-1",
		"--cwd\t" + r.wt,
		"--\tanotify-demo.tui-2",
		"\t修好 #321",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("hc run 少了 %q：\n%s", want, line)
		}
	}
	if strings.Contains(line, tuiToken) {
		t.Fatalf("token 进了 argv：\n%s", line)
	}

	// hc 模板文件：command 行来自 agent + agent_args，正文是 emit 契约。
	md, err := os.ReadFile(filepath.Join(templateDir(r.root), "anotify-demo.tui-2.md"))
	if err != nil {
		t.Fatalf("读不到 hc 模板：%v", err)
	}
	for _, want := range []string{
		"name: anotify-demo.tui-2",
		"command: pi --model gpt-6.1-sol --thinking medium",
		"--task-file .anotify/task.json",
		"修好后发 {\"summary\":\"done\"}",
		"ANOTIFY-STOP-<taskId>",
		"/opt/anotify/bin/anotify-agent emit",
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

	// 成功路径也要归档屏幕（runs/show 里能看到）。
	if !strings.Contains(r.stdout, "SCREEN-A") || !strings.Contains(r.stdout, "--- hc read ") {
		t.Fatalf("stdout 里应当有屏幕归档：\n%s", r.stdout)
	}
	// 收现场的结果进 DETAIL（stdout 第二行）：归档里要能看出「目录删了、分支还在」。
	if !strings.Contains(r.stdout, "已删除") || !strings.Contains(r.stdout, "分支 anotify/1a2b3c4d-1 保留") {
		t.Fatalf("stdout 应当说明 worktree 已清理：\n%s", r.stdout)
	}
}

// TestAgentTUIWorktreeNewKeep：keep 时目录留下，task.json 是 0600 且字段齐全。
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
	taskFile := filepath.Join(r.wt, ".anotify", "task.json")
	info, err := os.Stat(taskFile)
	if err != nil {
		t.Fatalf("读不到 task.json：%v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("task.json 权限 = %04o，必须 0600（里面有一次性 token）", perm)
	}
	body, err := os.ReadFile(taskFile)
	if err != nil {
		t.Fatalf("读 task.json：%v", err)
	}
	var got struct {
		Sock    string          `json:"sock"`
		Token   string          `json:"token"`
		Ports   json.RawMessage `json:"ports"`
		TaskID  string          `json:"taskId"`
		TraceID string          `json:"traceId"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("task.json 不是合法 JSON：%v\n%s", err, body)
	}
	if got.Sock == "" || got.Token != tuiToken || got.TaskID != "tsk_321" || got.TraceID != tuiTrace {
		t.Fatalf("task.json 字段不对：%+v", got)
	}
	// ports 必须是**同一份字节**（agent 侧照它校验，重编码会分叉）。
	if strings.Contains(string(got.Ports), " ") {
		t.Fatalf("ports 被重排版了（应当原样落盘）：%s", got.Ports)
	}
	if !strings.Contains(string(got.Ports), `"out"`) {
		t.Fatalf("ports 里应当有 out 出口：%s", got.Ports)
	}
	// .anotify/ 必须对整个 git 隐身：否则 agent 一句 `git add -A` 就把 token 追进提交，
	// 而分支是保留的（cleanup 只删目录），那份 token 会活在提交里。
	ignore := filepath.Join(r.wt, ".anotify", ".gitignore")
	if body, err := os.ReadFile(ignore); err != nil || strings.TrimSpace(string(body)) != "*" {
		t.Fatalf(".anotify/.gitignore 应当是 *（err=%v body=%q）", err, body)
	}
	if out, err := exec.Command("git", "-C", r.wt, "check-ignore", "-q", ".anotify/task.json").CombinedOutput(); err != nil {
		t.Fatalf("git 不该看见 .anotify/task.json：%v\n%s", err, out)
	}
	if out := gitExec(t, r.wt, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Fatalf("worktree 里不该有未跟踪的脏东西（.anotify 必须被忽略）：%q", out)
	}
}

// TestAgentTUIWorktreeExisting：用现成目录，不建分支、不删目录。
func TestAgentTUIWorktreeExisting(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{
		params: map[string]string{
			"ANOTIFY_PARAM_WORKTREE":      "existing",
			"ANOTIFY_PARAM_WORKTREE_PATH": filepath.Join("PLACEHOLDER", "wt"),
		},
		prep: func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "wt"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		},
	})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	run := r.hcCalls("run")
	if len(run) != 1 || !strings.Contains(run[0], "--cwd\t"+filepath.Join(r.root, "wt")) {
		t.Fatalf("cwd 应当是 existing 的那个目录：%v", run)
	}
	if _, err := os.Stat(filepath.Join(r.root, "wt")); err != nil {
		t.Fatalf("existing 目录不许被删：%v", err)
	}
	if branches := gitExec(t, r.repo, "branch", "--list"); strings.Contains(branches, "anotify/") {
		t.Fatalf("existing 不该开新分支：%q", branches)
	}
	if !strings.Contains(r.stdout, "existing") {
		t.Fatalf("stdout 应当说明这是 existing 目录：\n%s", r.stdout)
	}
}

// TestAgentTUIWorktreeOff：off 时 cwd 就是工作目录本身，不做任何 git 动作。
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
	// wait 的 timeout 不是失败：它必须至少等过（而不是第一次就放弃）。
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

// TestAgentTUISeqCycles：同一 trace 已有两条会话 → 这一次叫 -3（重试不撞名）。
func TestAgentTUISeqCycles(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{mode: "names"})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	run := r.hcCalls("run")
	if len(run) != 1 || !strings.Contains(run[0], "--name\tanotify-demo-tui-1a2b3c4d-3") {
		t.Fatalf("会话名应当带 -3：%v", run)
	}
	if !strings.Contains(run[0], "anotify.trace="+tuiTrace) {
		t.Fatalf("hc ls 应当按 trace 标签过滤：%v", r.hcCalls("ls"))
	}
}

// TestAgentTUISanitizesTraceID：cron 的 trace_id 带冒号，会话名/分支名不许带。
//
// cron 的 trace_id 形如 "cron:nightly:1730000000"（internal/trigger）。直接截 8 位会得到
// "cron:nig"，而 hc 的 --name 只收 [a-z0-9-_]、git ref 也不收冒号——症状是「cron 派的任务
// 一条也起不来」，而手工派的任务全好。
func TestAgentTUISanitizesTraceID(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{
		params: map[string]string{
			"ANOTIFY_TRACE_ID":       "cron:Nightly:1730000000",
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
	if !strings.Contains(run[0], "--name\tanotify-demo-tui-cron-nig-1") {
		t.Fatalf("会话名必须只含 [a-z0-9-_]：%s", run[0])
	}
	if !strings.Contains(run[0], "--label\tanotify.trace=cron:Nightly:1730000000") {
		t.Fatalf("标签里要留**原始** trace id（过滤/追溯用）：%s", run[0])
	}
	if branches := gitExec(t, r.repo, "branch", "--list"); !strings.Contains(branches, "anotify/cron-nig-1") {
		t.Fatalf("分支名也要 sanitize：%q", branches)
	}
}

// TestAgentTUIBranchEmptyDetaches：显式给空 branch = --detach（issue 已定）。
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
	if branches := gitExec(t, r.repo, "branch", "--list"); strings.Contains(branches, "anotify/") {
		t.Fatalf("空 branch 不该建分支：%q", branches)
	}
	// detach 时没有分支名：标签退回到路径（空值标签会让 hc ls --label 变成半个过滤器）。
	if label := r.hcCalls("run")[0]; strings.Contains(label, "anotify.worktree=\t") ||
		!strings.Contains(label, "anotify.worktree="+r.wt) {
		t.Fatalf("detach 时 worktree 标签应当是路径：%s", label)
	}
}

// TestAgentTUIBranchExpansion：{{trace8}} / {{seq}} 由节点展开（模板层零插值）。
func TestAgentTUIBranchExpansion(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{params: map[string]string{
		"ANOTIFY_PARAM_WORKTREE": "new",
		"ANOTIFY_PARAM_BRANCH":   "feature/{{trace8}}-{{seq}}",
	}})
	if r.code != 0 {
		t.Fatalf("应当成功，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	if branches := gitExec(t, r.repo, "branch", "--list", "feature/1a2b3c4d-1"); !strings.Contains(branches, "feature/1a2b3c4d-1") {
		t.Fatalf("占位符没展开：%q", branches)
	}
}

// TestAgentTUIParamValidation：坏参数是 exit 2（修，别重试），且不碰 hc run。
func TestAgentTUIParamValidation(t *testing.T) {
	cases := []struct {
		name  string
		param string
		value string
		want  string
	}{
		{"agent 不在枚举里", "ANOTIFY_PARAM_AGENT", "aider", "invalid_param"},
		{"agent_args 含换行", "ANOTIFY_PARAM_AGENT_ARGS", "--model x\n--evil", "换行"},
		{"引号不配对", "ANOTIFY_PARAM_AGENT_ARGS", `--model "x`, "引号不配对"},
		{"worktree 不认识", "ANOTIFY_PARAM_WORKTREE", "weird", "invalid_param"},
		{"cleanup 不认识", "ANOTIFY_PARAM_WORKTREE_CLEANUP", "maybe", "invalid_param"},
		{"timeout 不是数字", "ANOTIFY_PARAM_TIMEOUT_SEC", "soon", "invalid_param"},
		{"prompt 为空", "ANOTIFY_PARAM_PROMPT", "", "invalid_param"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runAgentTUI(t, tuiScenario{params: map[string]string{tc.param: tc.value}})
			if r.code != 2 {
				t.Fatalf("应当退 2（修参数），实际 %d\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
			}
			if !strings.Contains(firstLine(r.stdout), tc.want) {
				t.Fatalf("summary 里应当含 %q，实际 %q", tc.want, firstLine(r.stdout))
			}
			if len(r.hcCalls("run")) != 0 {
				t.Fatalf("参数错时不该起会话：%v", r.hcCalls("run"))
			}
		})
	}
}

// TestAgentTUIBalancedQuotesOK：引号配对就放行（不做安全过滤，只挡格式破坏）。
func TestAgentTUIBalancedQuotesOK(t *testing.T) {
	r := runAgentTUI(t, tuiScenario{params: map[string]string{
		"ANOTIFY_PARAM_AGENT":      "claude",
		"ANOTIFY_PARAM_AGENT_ARGS": `--model "gpt x" --thinking medium`,
	}})
	if r.code != 0 {
		t.Fatalf("引号配对应当放行，实际 %d\nstderr=%s", r.code, r.stderr)
	}
	md, err := os.ReadFile(filepath.Join(templateDir(r.root), "anotify-demo.tui-2.md"))
	if err != nil {
		t.Fatalf("读 hc 模板：%v", err)
	}
	if !strings.Contains(string(md), `command: claude --model "gpt x" --thinking medium`) {
		t.Fatalf("command 行必须是原文（引号属于命令本身）：\n%s", md)
	}
}

// TestAgentTUICancelInterrupts：节点 cancel（TERM）→ hc interrupt 当前会话，且不算成功。
//
// 为什么值得测：runner 的 cancel 是进程组 SIGKILL，脚本能拿到 TERM 是少数情况；
// 但把「还在 working 的终端」留在 hc 里，人会以为它还在干活。
func TestAgentTUICancelInterrupts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("没有 bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
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
	for name, body := range map[string]string{"hc": fakeHC, "id": fakeID, "getent": fakeGetent} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	tpl, _ := LookupTemplate("agent.tui")
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(tpl.Script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	hclog := filepath.Join(root, "hc.log")
	cmd := exec.Command("bash", script)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"HOME=" + home,
		"FAKE_LOG=" + hclog,
		"FAKE_MODE=slowwait",
		"ANOTIFY_WORKDIR=" + repo,
		"ANOTIFY_TASKDIR=" + filepath.Join(root, "work"),
		"ANOTIFY_TASK_ID=tsk_cancel",
		"ANOTIFY_TRACE_ID=" + tuiTrace,
		"ANOTIFY_CAPABILITY=demo.tui@2",
		"ANOTIFY_EMIT_SOCK=" + filepath.Join(root, "e.sock"),
		"ANOTIFY_TASK_TOKEN=" + tuiToken,
		"ANOTIFY_EMIT_PORTS=[]",
		"ANOTIFY_AGENT_BIN=/opt/anotify/bin/anotify-agent",
		"ANOTIFY_PARAM_AGENT=pi",
		"ANOTIFY_PARAM_PROMPT=修好 #321",
		"ANOTIFY_PARAM_TIMEOUT_SEC=60",
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 等到会话真的起来了（hc run 被调用）再发信号：否则 SESSION 还没赋值，
	// 用例会随机退化成「没东西可 interrupt」。
	deadline := time.Now().Add(10 * time.Second)
	for {
		body, _ := os.ReadFile(hclog)
		if strings.Contains(string(body), "run\t") {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("等会话起来超时，hc 日志：%s\nstderr=%s", body, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // 让脚本进到 hc wait
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatalf("cancel 不该算成功（退出码 143）\nstdout=%s", stdout.String())
	}
	if !strings.HasPrefix(firstLine(stdout.String()), "canceled:") {
		t.Fatalf("summary 应当以 canceled: 开头，实际 %q", firstLine(stdout.String()))
	}
	logBody, _ := os.ReadFile(hclog)
	if !strings.Contains(string(logBody), "interrupt") {
		t.Fatalf("cancel 必须 hc interrupt 当前会话：\n%s", logBody)
	}
}
