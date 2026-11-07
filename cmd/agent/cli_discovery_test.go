package main

// 本文件是**可发现面**的单测（issue #317）：`-h` 的命令表、`--skill` 手册、`--version`。
//
// 为什么单开一个文件：它们钉的不是行为（连不连得上中枢、跑不跑得动产物），而是
// 「一个 agent 拿到这台机器上的二进制，能不能知道它有什么命令」——这条链路断了
// 不会编译失败，也不会让任何 e2e 变红，只会让人放弃使用它。
//
// 防止手册漂移的**机器守卫**在 scripts/check_agent_skill.py（`make check-agent-skill`）：
// 它拿真二进制的 `-h` / `--version` / `--skill` 去对账 skill.md。这里只钉那些
// 不需要构建二进制的部分（文本内容与主命令分派）。

import (
	"bytes"
	"flag"
	"regexp"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/version"
)

// TestUsageListsEverySubcommand：`-h` 的全文必须列出 5 个子命令，每条带一句人话
// 和一个可抄示例，末尾有 `--skill` 发现钩子，并且**第一屏**就说清「不带子命令 =
// 常驻进程」（否则 agent 会把它当查询命令跑，占掉一个节点连接）。
func TestUsageListsEverySubcommand(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(&buf)
	writeUsage(fs)
	out := buf.String()

	// 五条子命令：名字、人话、示例三样都在。
	for _, name := range []string{"run", "emit", "upgrade", "runs", "show"} {
		if !strings.Contains(out, "\n  "+name) {
			t.Errorf("命令表里没有 %q：\n%s", name, out)
		}
	}
	for _, c := range agentCommands {
		if !strings.Contains(out, c.signature()) {
			t.Errorf("命令表里没有 %q 的签名（名字 + 位置参数）", c.signature())
		}
		if !strings.Contains(out, c.desc) {
			t.Errorf("%s 没有一句人话（desc）", c.name)
		}
		if !strings.Contains(out, c.example) {
			t.Errorf("%s 没有示例 %q（agent 要能直接抄）", c.name, c.example)
		}
	}

	// 发现钩子固定在最后一行。
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if got := lines[len(lines)-1]; got != "Agent? 运行: anotify-agent --skill" {
		t.Errorf("最后一行应当是发现钩子，实际 %q", got)
	}

	// 节点模式的语义必须明说，而不是靠读者猜。
	if !strings.Contains(out, "常驻进程") {
		t.Errorf("usage 必须说清「不带子命令 = 常驻进程」：\n%s", out)
	}
	// 原有断言（main_test.go）要求这两个 flag 仍在 usage 里：PrintDefaults 保证。
	for _, flagName := range []string{"-f", "-grace", "-data-dir"} {
		if !strings.Contains(out, flagName) {
			t.Errorf("usage 里没有 %s", flagName)
		}
	}
	// 进程级开关也要出现在 help 里，否则等于没有发现路径。
	for _, switchName := range []string{"--version", "--skill"} {
		if !strings.Contains(out, switchName) {
			t.Errorf("usage 里没有进程级开关 %s", switchName)
		}
	}
}

// TestAgentCommandsMatchDispatch：命令表与分派必须同名同数。
//
// 两边分开写（表在 usage.go、分派在 mainWith），加子命令时最容易只改一边：
// 结果要么是「帮助里有、跑不了」，要么是「跑得了、帮助里没有」。
func TestAgentCommandsMatchDispatch(t *testing.T) {
	t.Parallel()

	if len(agentCommands) != 5 {
		t.Fatalf("命令表有 %d 条，want 5（run / emit / upgrade / runs / show）", len(agentCommands))
	}
	seen := map[string]bool{}
	for _, c := range agentCommands {
		if !isSubcommand(c.name) {
			t.Errorf("命令表里的 %q 不是 isSubcommand 认的子命令", c.name)
		}
		if seen[c.name] {
			t.Errorf("命令表里 %q 重复", c.name)
		}
		seen[c.name] = true
	}
	// 反方向：分派认的每个名字都得在表里。
	for _, name := range []string{runCommand, emitCommand, upgradeCommand, runsCommand, showCommand} {
		if !seen[name] {
			t.Errorf("isSubcommand 认 %q，但命令表里没有它", name)
		}
	}
}

// TestHelpExitsZeroAndShowsCommandTable：`-h` 通过真实入口跑一遍——退出码 0，
// 命令表在 stderr（flag 包把 usage 写到 FlagSet 的 output）。
func TestHelpExitsZeroAndShowsCommandTable(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := mainWith([]string{"-h"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("mainWith(-h) = %d, want %d", code, exitOK)
	}
	for _, name := range []string{"run", "emit", "upgrade", "runs", "show"} {
		if !strings.Contains(stderr.String(), "\n  "+name) {
			t.Errorf("-h 的输出里没有子命令 %q：\n%s", name, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "Agent? 运行: anotify-agent --skill") {
		t.Errorf("-h 的输出末尾没有发现钩子：\n%s", stderr.String())
	}
}

// TestVersionIsOneLine：`--version` 一行、含版本与 commit，不读配置（空目录 +
// 一个故意写坏的 ANOTIFY_TOKEN 也照常退 0），不往 stderr 写任何东西。
func TestVersionIsOneLine(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ANOTIFY_TOKEN", "token-should-never-be-read")

	var stdout, stderr bytes.Buffer
	if code := mainWith([]string{"--version"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("mainWith(--version) = %d, want %d（stderr=%s）", code, exitOK, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("--version 必须只打一行，实际 %d 行：%q", len(lines), stdout.String())
	}
	if !regexp.MustCompile(`^anotify-agent \S+ \(\S+, \S+\)$`).MatchString(lines[0]) {
		t.Fatalf("--version 形状应当是 `anotify-agent <version> (<commit>, <builtAt>)`，实际 %q", lines[0])
	}
	if !strings.Contains(lines[0], buildVersion()) || !strings.Contains(lines[0], buildCommit()) {
		t.Errorf("--version 里没有当前版本 %q / commit %q：%q", buildVersion(), buildCommit(), lines[0])
	}
	if stderr.Len() != 0 {
		t.Errorf("--version 不该往 stderr 写东西（说明读了配置或连了网）：%q", stderr.String())
	}
}

// TestSkillInEmptyDir：`--skill` 在空目录、没有 node.yaml、没有令牌的条件下退 0，
// 打出完整手册（五节 + 首行版本 + agent/服务端/herdr 的消歧）。
func TestSkillInEmptyDir(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ANOTIFY_TOKEN", "")

	var stdout, stderr bytes.Buffer
	if code := mainWith([]string{"--skill"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("mainWith(--skill) = %d, want %d（stderr=%s）", code, exitOK, stderr.String())
	}
	out := stdout.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 60 {
		t.Fatalf("手册只有 %d 行，像是被截断了", len(lines))
	}
	if want := "# anotify-agent manual (" + versionLabel() + ")"; lines[0] != want {
		t.Errorf("首行 = %q, want %q（版本必须来自 -ldflags 注入，而不是文件里写死）", lines[0], want)
	}
	for _, heading := range []string{"## 1.", "## 2.", "## 3.", "## 4.", "## 5."} {
		if !strings.Contains(out, heading) {
			t.Errorf("手册缺小节 %q（起节点/写能力/排障/退出码/不要做）", heading)
		}
	}
	// 消歧：agent ≠ 服务端 anotify ≠ herdr（否则 agent 会去 anotify 上找子命令）。
	head := strings.Join(lines[:15], "\n")
	for _, name := range []string{"anotify-agent", "anotify", "herdr"} {
		if !strings.Contains(head, name) {
			t.Errorf("手册开头 15 行没有区分 %q（agent / 服务端 / herdr）", name)
		}
	}
	// 占位符必须真的被替换掉（漏替换会让 agent 读到字面的 {{VERSION}}）。
	if strings.Contains(out, skillVersionPlaceholder) {
		t.Errorf("手册里还留着占位符 %s", skillVersionPlaceholder)
	}
	if stderr.Len() != 0 {
		t.Errorf("--skill 不该往 stderr 写东西：%q", stderr.String())
	}
}

// TestVersionAndSkillShareVersionString：两处版本串必须一致——agent 可能拿着从别的
// 机器上缓存的旧手册，首行不给版本它就会拿旧 flag 调新二进制。
//
// 这里直接改包级变量（正是 -ldflags 改的那两个）：注入值、缺省 dev 两种都钉住。
// **不能 t.Parallel**：version.Version 是全局状态。
func TestVersionAndSkillShareVersionString(t *testing.T) {
	oldVersion, oldCommit, oldBuiltAt := version.Version, version.Commit, version.BuiltAt
	t.Cleanup(func() {
		version.Version, version.Commit, version.BuiltAt = oldVersion, oldCommit, oldBuiltAt
	})

	cases := []struct {
		name     string
		version  string
		commit   string
		builtAt  string
		wantLine string
	}{
		{
			name:     "注入构建",
			version:  "1.5.0",
			commit:   "3fd5a59",
			builtAt:  "2026-11-05T10:00:00Z",
			wantLine: "anotify-agent 1.5.0 (3fd5a59, 2026-11-05T10:00:00Z)",
		},
		{
			name:     "裸 build（没注入）",
			version:  "",
			commit:   "",
			builtAt:  "",
			wantLine: "anotify-agent dev (unknown, unknown)",
		},
	}
	for _, tc := range cases {
		version.Version, version.Commit, version.BuiltAt = tc.version, tc.commit, tc.builtAt
		if got := versionLine(); got != tc.wantLine {
			t.Errorf("%s: versionLine() = %q, want %q", tc.name, got, tc.wantLine)
		}
		skillLines := strings.SplitN(renderSkill(), "\n", 2)
		wantBanner := "# anotify-agent manual (" + versionLabel() + ")"
		if skillLines[0] != wantBanner {
			t.Errorf("%s: 手册首行 = %q, want %q", tc.name, skillLines[0], wantBanner)
		}
		// 同一个版本串必须同时出现在 --version 与手册首行里。
		if !strings.Contains(renderSkill(), buildVersion()) || !strings.Contains(renderSkill(), buildCommit()) {
			t.Errorf("%s: 手册里没有版本 %q / commit %q", tc.name, buildVersion(), buildCommit())
		}
	}
}

// TestPreludeCommandPosition：`--version` / `--skill` 只在**子命令之前**算进程级开关；
// 写在子命令后面属于那个子命令（`run --version` 不该变成打印版本）。
func TestPreludeCommandPosition(t *testing.T) {
	t.Parallel()

	cases := []struct {
		args []string
		want preludeMode
	}{
		{args: nil, want: preludeNone},
		{args: []string{"--version"}, want: preludeVersion},
		{args: []string{"--skill"}, want: preludeSkill},
		{args: []string{"-f", "/tmp/node.yaml", "--version"}, want: preludeVersion},
		{args: []string{"-data-dir", "/tmp/dd", "--skill"}, want: preludeSkill},
		// 子命令之后的同名 flag 归子命令，不能被这里截走。
		{args: []string{"run", "--version"}, want: preludeNone},
		{args: []string{"upgrade", "--skill"}, want: preludeNone},
		// 未知 flag 交给主路径报错，不在这里变成「要版本」。
		{args: []string{"-nope", "--version"}, want: preludeNone},
	}
	for _, tc := range cases {
		if got := preludeCommand(tc.args); got != tc.want {
			t.Errorf("preludeCommand(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestSubcommandAfterGraceFlag 是 subcommandAt 的一个真实陷阱：
// `-grace 7 upgrade` 曾经因为定位用的 FlagSet 把 -grace 注册成 Duration（而真实
// flag 是 int）解析失败 → 不认子命令 → **静默跑起一个常驻 agent**。形状必须一致。
func TestSubcommandAfterGraceFlag(t *testing.T) {
	t.Parallel()

	sub, rest, ok := subcommandAt([]string{"-grace", "7", "upgrade", "--check"})
	if !ok || sub != upgradeCommand {
		t.Fatalf("subcommandAt(-grace 7 upgrade) = (%q, %v, %v)，want upgrade", sub, rest, ok)
	}
	want := []string{"-grace", "7", "--check"}
	if strings.Join(rest, " ") != strings.Join(want, " ") {
		t.Fatalf("rest = %v, want %v（全局 flag 要原样带给子命令）", rest, want)
	}
}

// TestVersionAndSkillIgnoreBrokenConfig：`-f` 指向不存在的文件时，--version / --skill
// 仍然必须成功 —— 它们的价值就在于「任何环境可用」。
func TestVersionAndSkillIgnoreBrokenConfig(t *testing.T) {
	t.Parallel()

	missing := "/nonexistent/anotify/node.yaml"
	for _, args := range [][]string{
		{"-f", missing, "--version"},
		{"-f", missing, "--skill"},
	} {
		var stdout, stderr bytes.Buffer
		if code := mainWith(args, &stdout, &stderr); code != exitOK {
			t.Errorf("mainWith(%v) = %d, want %d（stderr=%q）", args, code, exitOK, stderr.String())
		}
		if stdout.Len() == 0 {
			t.Errorf("mainWith(%v) 没有输出", args)
		}
		if strings.Contains(stderr.String(), "agent.config.failed") {
			t.Errorf("mainWith(%v) 读了配置 —— 这一条路径必须离线：%q", args, stderr.String())
		}
	}
}
