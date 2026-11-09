package config

import (
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

const validYAML = `
name: node-a
server: wss://anotify.example.com
token: node-token
namespaces: [default, team-a]
listen: tcp://127.0.0.1:0
vars:
  SOURCE_ROOT: /srv/src
limits:
  total: 8
  perCapability:
    reviewer: 2
envPassthrough: [SSH_AUTH_SOCK]
`

func mustParse(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse() 失败: %v", err)
	}
	return cfg
}

func TestParseValidConfig(t *testing.T) {
	cfg := mustParse(t, validYAML)
	if cfg.Name != "node-a" || cfg.Server != "wss://anotify.example.com" || cfg.Token != "node-token" {
		t.Fatalf("身份字段解析错误: %+v", cfg)
	}
	if got, want := strings.Join(cfg.Namespaces, ","), "default,team-a"; got != want {
		t.Fatalf("namespaces = %s, want %s", got, want)
	}
	if cfg.Listen != "tcp://127.0.0.1:0" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
	if cfg.Vars["SOURCE_ROOT"] != "/srv/src" {
		t.Fatalf("vars = %v", cfg.Vars)
	}
	if cfg.Limits.Total == nil || *cfg.Limits.Total != 8 {
		t.Fatalf("limits.total = %v", cfg.Limits.Total)
	}
	if cfg.Limits.PerCapability["reviewer"] != 2 {
		t.Fatalf("perCapability = %v", cfg.Limits.PerCapability)
	}
	local := cfg.LocalLimits()
	if local.Total != 8 || local.PerCapability["reviewer"] != 2 {
		t.Fatalf("LocalLimits = %+v", local)
	}
	if len(cfg.EnvPassthrough) != 1 || cfg.EnvPassthrough[0] != "SSH_AUTH_SOCK" {
		t.Fatalf("envPassthrough = %v", cfg.EnvPassthrough)
	}
}

func TestStrictUnknownFields(t *testing.T) {
	cases := map[string]string{
		"top-level":         "name: a\nserver: wss://h\ntoken: t\nlisten_: tcp://127.0.0.1:0\n",
		"nested limits":     "name: a\nserver: wss://h\ntoken: t\nlimits:\n  total: 1\n  bogus: 2\n",
		"nested vars shape": "name: a\nserver: wss://h\ntoken: t\nvars:\n  - A\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Fatalf("期望未知字段失败，实际通过")
			}
		})
	}
}

func TestRequiredIdentityFields(t *testing.T) {
	_, err := Parse([]byte("namespaces: [default]\n"))
	if err == nil {
		t.Fatal("空配置应失败")
	}
	msg := err.Error()
	for _, want := range []string{"name 必填", "server 必填", "token 必填"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误 %q 缺少 %q", msg, want)
		}
	}
}

func TestNameAndServerValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"name uppercase", "name: Node-A\nserver: wss://h\ntoken: t\n", "name"},
		{"name 空", "name: \"\"\nserver: wss://h\ntoken: t\n", "name 必填"},
		{"server http", "name: a\nserver: http://h\ntoken: t\n", "ws:// 或 wss://"},
		{"server 缺 host", "name: a\nserver: wss://\ntoken: t\n", "缺少主机名"},
		{"server 带凭据", "name: a\nserver: wss://user@h\ntoken: t\n", "不允许在 URL 里带凭据"},
		{"token 空白", "name: a\nserver: wss://h\ntoken: \"  \"\n", "token 必填"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

func TestNamespacesDefaultExplicitEmptyAndDuplicate(t *testing.T) {
	cfg := mustParse(t, "name: a\nserver: wss://h\ntoken: t\n")
	if len(cfg.Namespaces) != 1 || cfg.Namespaces[0] != DefaultNamespace {
		t.Fatalf("缺省 namespaces = %v", cfg.Namespaces)
	}

	_, err := Parse([]byte("name: a\nserver: wss://h\ntoken: t\nnamespaces: []\n"))
	if err == nil || !strings.Contains(err.Error(), "显式为空") {
		t.Fatalf("显式空 namespaces 应失败: %v", err)
	}

	_, err = Parse([]byte("name: a\nserver: wss://h\ntoken: t\nnamespaces: [default, default]\n"))
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 namespaces 应失败: %v", err)
	}

	_, err = Parse([]byte("name: a\nserver: wss://h\ntoken: t\nnamespaces: [Default]\n"))
	if err == nil || !strings.Contains(err.Error(), "不合法") {
		t.Fatalf("非法 namespaces 元素应失败: %v", err)
	}

	cfg = mustParse(t, "name: a\nserver: wss://h\ntoken: t\nnamespaces: [\"*\"]\n")
	if len(cfg.Namespaces) != 1 || cfg.Namespaces[0] != Wildcard {
		t.Fatalf("通配 namespaces = %v", cfg.Namespaces)
	}
}

func TestVarsValidation(t *testing.T) {
	valid := map[string]string{"SOURCE_ROOT": "/srv"}
	if p := validateVars(valid); len(p) != 0 {
		t.Fatalf("合法 vars 报错: %v", p)
	}
	cases := []struct {
		name string
		vars map[string]string
		want string
	}{
		{"小写名", map[string]string{"source_root": "/x"}, "不合法"},
		{"空值", map[string]string{"A": ""}, "不能为空"},
		{"NUL", map[string]string{"A": "a\x00b"}, "NUL"},
		{"超长", map[string]string{"A": strings.Repeat("x", MaxVarsValueBytes+1)}, "超过上限"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validateVars(tc.vars)
			if len(p) == 0 || !strings.Contains(strings.Join(p, ";"), tc.want) {
				t.Fatalf("problems = %v, want contains %q", p, tc.want)
			}
		})
	}
}

func TestLimitsUnsetVsExplicitZero(t *testing.T) {
	cfg := mustParse(t, "name: a\nserver: wss://h\ntoken: t\n")
	if cfg.Limits.Total != nil {
		t.Fatalf("未写 total 应该是 nil，实际 %v", *cfg.Limits.Total)
	}
	if got := cfg.LocalLimits().Total; got != 0 {
		t.Fatalf("未写 total 的 wire 值应该是 0，实际 %d", got)
	}

	for _, yaml := range []string{
		"name: a\nserver: wss://h\ntoken: t\nlimits:\n  total: 0\n",
		"name: a\nserver: wss://h\ntoken: t\nlimits:\n  total: -1\n",
		"name: a\nserver: wss://h\ntoken: t\nlimits:\n  perCapability:\n    reviewer: 0\n",
		"name: a\nserver: wss://h\ntoken: t\nlimits:\n  perCapability:\n    Reviewer: 1\n",
	} {
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Fatalf("非法 limits 应失败:\n%s", yaml)
		}
	}
	mustParse(t, "name: a\nserver: wss://h\ntoken: t\nlimits:\n  perCapability:\n    \"*\": 3\n")
}

func TestEnvPassthroughValidation(t *testing.T) {
	mustParse(t, "name: a\nserver: wss://h\ntoken: t\nenvPassthrough: [SSH_AUTH_SOCK, GITHUB_TOKEN]\n")
	many := make([]string, MaxPassthroughNames+1)
	for i := range many {
		many[i] = "VAR" + strings.Repeat("A", 1) + string(rune('A'+i%26)) + strings.Repeat("X", i)
	}
	cases := []struct {
		name string
		list []string
		want string
	}{
		{"超过 32", many, "最多 32 项"},
		{"重复", []string{"A", "A"}, "重复"},
		{"非法名", []string{"1BAD"}, "不合法"},
		{"PATH", []string{"PATH"}, "不允许 PATH"},
		{"HOME", []string{"HOME"}, "不允许 HOME"},
		{"ANOTIFY_", []string{"ANOTIFY_TASK_TOKEN"}, "ANOTIFY_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := strings.Join(validatePassthrough(tc.list), ";")
			if !strings.Contains(p, tc.want) {
				t.Fatalf("problems = %q, want contains %q", p, tc.want)
			}
		})
	}
}

func TestListenValidation(t *testing.T) {
	cfg := mustParse(t, "name: a\nserver: wss://h\ntoken: t\n")
	if cfg.Listen != "tcp://127.0.0.1:0" {
		t.Fatalf("缺省 listen = %q", cfg.Listen)
	}
	mustParse(t, "name: a\nserver: wss://h\ntoken: t\nlisten: unix:///run/anode.sock\n")
	for _, spec := range []string{"http://127.0.0.1:1", "tcp://:1", "unix://relative.sock"} {
		if _, err := Parse([]byte("name: a\nserver: wss://h\ntoken: t\nlisten: " + spec + "\n")); err == nil {
			t.Fatalf("非法 listen %q 应失败", spec)
		}
	}
}

func TestMultiDocumentRejected(t *testing.T) {
	_, err := Parse([]byte("name: a\nserver: wss://h\ntoken: t\n---\nname: b\n"))
	if err == nil || !strings.Contains(err.Error(), "只允许一个 YAML 文档") {
		t.Fatalf("多文档应失败: %v", err)
	}
}

func TestMergeVarsLocalOverNode(t *testing.T) {
	got := MergeVars(map[string]string{"A": "local-a", "C": "local-c"}, map[string]string{"A": "node-a", "B": "node-b"})
	want := map[string]string{"A": "local-a", "B": "node-b", "C": "local-c"}
	if len(got) != len(want) {
		t.Fatalf("MergeVars = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("MergeVars[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestMergeLimitsLeaf(t *testing.T) {
	local := protocol.Limits{Total: 4, PerCapability: map[string]int{"reviewer": 2, "builder": 1}}
	node := protocol.Limits{Total: 8, PerCapability: map[string]int{"reviewer": 5, "tester": 3, "*": 2}}
	got := MergeLimits(local, node)
	if got.Total != 4 {
		t.Fatalf("total = %d, want local 4", got.Total)
	}
	want := map[string]int{"reviewer": 2, "builder": 1, "tester": 3, "*": 2}
	if len(got.PerCapability) != len(want) {
		t.Fatalf("perCapability = %v, want %v", got.PerCapability, want)
	}
	for k, v := range want {
		if got.PerCapability[k] != v {
			t.Fatalf("perCapability[%s] = %d, want %d", k, got.PerCapability[k], v)
		}
	}

	// local 没写 total → 继承 node；两层都没写 → 缺省 1。
	if got := MergeLimits(protocol.Limits{}, node); got.Total != 8 {
		t.Fatalf("继承 node total = %d", got.Total)
	}
	if got := MergeLimits(protocol.Limits{}, protocol.Limits{}); got.Total != protocol.DefaultLimitsTotal {
		t.Fatalf("缺省 total = %d", got.Total)
	}
}

func TestMachineHashUsesProtocolImplementation(t *testing.T) {
	local := mustParse(t, validYAML)
	vars, limits, hash, err := HashMachine(local, map[string]string{"SOURCE_ROOT": "/node/src", "NODE_ONLY": "n"}, protocol.Limits{Total: 6})
	if err != nil {
		t.Fatal(err)
	}
	if vars["SOURCE_ROOT"] != "/srv/src" || vars["NODE_ONLY"] != "n" {
		t.Fatalf("合并 vars = %v", vars)
	}
	// 冻结 golden（PROTOCOL-HANDOFF §2）：shared 实现的交叉验证。
	goldenVars := map[string]string{"A": "1", "B": "2"}
	goldenLimits := protocol.Limits{Total: 8, PerCapability: map[string]int{"*": 1, "review": 2}}
	golden, err := MachineHash(goldenVars, goldenLimits)
	if err != nil {
		t.Fatal(err)
	}
	const wantGolden = "sha256:80fa17cfdaa7c73612f7d349c7231d5acbe40b5a367fdb39cb91d23d248bee68"
	if golden != wantGolden {
		t.Fatalf("golden hash = %s, want %s", golden, wantGolden)
	}
	direct, err := protocol.MachineConfigHash(vars, limits)
	if err != nil {
		t.Fatal(err)
	}
	if direct != hash {
		t.Fatalf("hash 与 protocol 实现不一致: %s vs %s", hash, direct)
	}
}

func TestEffectiveNamespacesIntersection(t *testing.T) {
	cases := []struct {
		name       string
		declared   []string
		authorized []string
		want       string
	}{
		{"正常交集", []string{"default", "team-a"}, []string{"team-a", "team-b"}, "team-a"},
		{"本地申报全部", []string{"*"}, []string{"team-a", "team-b"}, "team-a,team-b"},
		{"中枢授权全部", []string{"default", "team-a"}, []string{"*"}, "default,team-a"},
		{"两侧全部", []string{"*"}, []string{"*"}, "*"},
		{"无交集", []string{"default"}, []string{"team-a"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(EffectiveNamespaces(tc.declared, tc.authorized), ",")
			if got != tc.want {
				t.Fatalf("EffectiveNamespaces = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAllowsNamespace(t *testing.T) {
	effective := EffectiveNamespaces([]string{"default", "team-a"}, []string{"team-a", "team-b"})
	if !AllowsNamespace(effective, "team-a") {
		t.Fatal("team-a 应被授权")
	}
	if AllowsNamespace(effective, "default") || AllowsNamespace(effective, "team-b") {
		t.Fatal("未交集的 ns 不应被授权（本地不能扩张）")
	}
	all := EffectiveNamespaces([]string{"*"}, []string{"*"})
	if !AllowsNamespace(all, "anything") {
		t.Fatal("通配应放行合法 ns")
	}
	if AllowsNamespace(all, "") {
		t.Fatal("空 ns 不应放行")
	}
}

func TestValidateNodeDefaults(t *testing.T) {
	if err := ValidateNodeDefaults(map[string]string{"OK": "v"}, protocol.Limits{Total: 2}); err != nil {
		t.Fatalf("合法节点默认值报错: %v", err)
	}
	if err := ValidateNodeDefaults(map[string]string{"bad": "v"}, protocol.Limits{}); err == nil {
		t.Fatal("小写 node var 应失败")
	}
	if err := ValidateNodeDefaults(nil, protocol.Limits{Total: -1}); err == nil {
		t.Fatal("负 total 应失败")
	}
	if err := ValidateNodeDefaults(nil, protocol.Limits{PerCapability: map[string]int{"x": 0}}); err == nil {
		t.Fatal("0 的 node perCapability 应失败")
	}
}
