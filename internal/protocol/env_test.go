package protocol

import (
	"strings"
	"testing"
)

// TestEnvContractVariables 钉死四个 ANOTIFY_ 契约变量的字面值（capability.md §4.0）。
//
// 这些名字是协议：能力脚本、节点注入、文档三方必须逐字一致；改一个字就是
// 「脚本读不到端点」。所以连值一起快照，不接受「反正 Go 常量两边都用」。
func TestEnvContractVariables(t *testing.T) {
	if EnvEmitEndpoint != "ANOTIFY_EMIT_ENDPOINT" {
		t.Fatalf("EnvEmitEndpoint = %q", EnvEmitEndpoint)
	}
	if EnvTaskToken != "ANOTIFY_TASK_TOKEN" {
		t.Fatalf("EnvTaskToken = %q", EnvTaskToken)
	}
	if EnvTaskID != "ANOTIFY_TASK_ID" {
		t.Fatalf("EnvTaskID = %q", EnvTaskID)
	}
	if EnvParamPrefix != "ANOTIFY_PARAM_" {
		t.Fatalf("EnvParamPrefix = %q", EnvParamPrefix)
	}
	// 全部以 ANOTIFY_ 开头；参数变量名必须真的挂在这个前缀上。
	for _, name := range []string{EnvEmitEndpoint, EnvTaskToken, EnvTaskID, EnvParamPrefix} {
		if !strings.HasPrefix(name, "ANOTIFY_") {
			t.Fatalf("%q 不是 ANOTIFY_ 契约变量", name)
		}
	}
	if got := ParamEnvName("x"); !strings.HasPrefix(got, EnvParamPrefix) {
		t.Fatalf("ParamEnvName 没用 EnvParamPrefix：%q", got)
	}
}

// TestParamEnvNameUppercase 大写规则：a-z 抬成 A-Z；已经是 A-Z 的原样保留。
func TestParamEnvNameUppercase(t *testing.T) {
	cases := map[string]string{
		"sha":  "ANOTIFY_PARAM_SHA",
		"SHA":  "ANOTIFY_PARAM_SHA",
		"Sha":  "ANOTIFY_PARAM_SHA",
		"repo": "ANOTIFY_PARAM_REPO",
	}
	for in, want := range cases {
		if got := ParamEnvName(in); got != want {
			t.Fatalf("ParamEnvName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestParamEnvNameDigits 数字原样保留，且不插任何分隔。
func TestParamEnvNameDigits(t *testing.T) {
	cases := map[string]string{
		"sha256": "ANOTIFY_PARAM_SHA256",
		"v2":     "ANOTIFY_PARAM_V2",
		"3d":     "ANOTIFY_PARAM_3D",
		"a1b2":   "ANOTIFY_PARAM_A1B2",
	}
	for in, want := range cases {
		if got := ParamEnvName(in); got != want {
			t.Fatalf("ParamEnvName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestParamEnvNameSymbols 符号规则：非 [A-Z0-9] 的字符替换成一个 '_'。
func TestParamEnvNameSymbols(t *testing.T) {
	cases := map[string]string{
		"repo-url": "ANOTIFY_PARAM_REPO_URL",
		"a.b c":    "ANOTIFY_PARAM_A_B_C",
		"a/b":      "ANOTIFY_PARAM_A_B",
		"x--y":     "ANOTIFY_PARAM_X__Y",
		"_lead":    "ANOTIFY_PARAM__LEAD",
		"":         "ANOTIFY_PARAM_",
		// 非 ASCII 逐个 rune 换一个 '_'（多字节 rune 不变成多个下划线）。
		"参数":    "ANOTIFY_PARAM___",
		"naïve": "ANOTIFY_PARAM_NA_VE",
	}
	for in, want := range cases {
		if got := ParamEnvName(in); got != want {
			t.Fatalf("ParamEnvName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestParamEnvNameNoCamelBoundary 不插 camel 大小写边界：
// repoUrl → REPOURL（不是 REPO_URL），workDirPath → WORKDIRPATH。
//
// 这条与 artifact 的私有实现逐字一致（artifact_test.go 的 TestParamEnvName
// 有同样的样例）：两处必须同一行为，否则「写入期算的 env 名」和「运行期注入的
// env 名」会分叉，脚本读不到参数。
func TestParamEnvNameNoCamelBoundary(t *testing.T) {
	cases := map[string]string{
		"repoUrl":     "ANOTIFY_PARAM_REPOURL",
		"workDirPath": "ANOTIFY_PARAM_WORKDIRPATH",
	}
	for in, want := range cases {
		if got := ParamEnvName(in); got != want {
			t.Fatalf("ParamEnvName(%q) = %q，期望 %q（不插 camel 边界）", in, got, want)
		}
	}
	// 反证：带符号的形式才会出现下划线，两者不能混为一谈。
	if ParamEnvName("repoUrl") == ParamEnvName("repo-url") {
		t.Fatal("repoUrl 与 repo-url 归一后不该相同（不插 camel 边界）")
	}
}

// TestParamEnvNameCollisions 归一撞名是**预期行为**：函数不报错、不消歧，
// 由 artifact 在写入期用同一个函数检测并拒绝（param_env_collision）。
//
// 共享函数只负责规则一致；「谁覆盖谁」这种隐式行为不允许进入运行期。
func TestParamEnvNameCollisions(t *testing.T) {
	groups := [][]string{
		{"sha", "SHA", "Sha"},
		{"repo-url", "repo.url", "repo_url", "repo url", "REPO-URL"},
		{"a-b", "a_b", "a b", "a.b"},
	}
	for _, group := range groups {
		want := ParamEnvName(group[0])
		for _, name := range group[1:] {
			if got := ParamEnvName(name); got != want {
				t.Fatalf("%q 与 %q 应当归一撞名：%q != %q", group[0], name, want, got)
			}
		}
	}
	// 不撞的名字不能被误并。
	for _, pair := range [][2]string{
		{"sha", "sha1"},
		{"repo-url", "repoUrl"},
		{"a", "A1"},
	} {
		if ParamEnvName(pair[0]) == ParamEnvName(pair[1]) {
			t.Fatalf("%q 与 %q 不该撞名", pair[0], pair[1])
		}
	}
}
