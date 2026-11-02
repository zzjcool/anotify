package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件覆盖 node.yaml 的**新形状**（console-declares / 裁决 D4）：
// 只剩 bootstrap 四件 name/server/token/instances。
//
// v1 的 capabilities[]/run（runner/command/out/concurrency/env）全部删除——能力
// 不再由机器声明，而是控制台编译成产物、经 deploy 帧下发（cache.go / apply.go）。
// 因此 v1 那些「run 块校验 / lookup / 按能力算并发」的用例一并删除，
// 换成「bootstrap 校验 + 未知字段（含老配置）必须报错 + hello.applied 透传」。

// nodeYAMLBootstrap 是 T4 之后 node.yaml 的完整形状。
const nodeYAMLBootstrap = `# node.yaml —— 节点侧唯一的 bootstrap 声明
name: laptop-worker
server: http://localhost:8080
token: ntk_local_dev
instances: 2
`

// writeNodeYAML 把 YAML 落到 t.TempDir() 下的 node.yaml 并返回路径。
// 不依赖 testdata：每份坏配置都是本测试自己写出来的，读起来就是断言。
func writeNodeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write node.yaml: %v", err)
	}
	return path
}

// mustLoadNodeConfig 是「这份配置应当合法」的捷径。
func mustLoadNodeConfig(t *testing.T, body string) *nodeConfig {
	t.Helper()
	cfg, err := loadConfig(writeNodeYAML(t, body))
	if err != nil {
		t.Fatalf("loadConfig: unexpected error: %v", err)
	}
	return cfg
}

// TestConfigLoadBootstrap：四个字段全解码正确，且结构体里**没有**能力字段了。
func TestConfigLoadBootstrap(t *testing.T) {
	cfg := mustLoadNodeConfig(t, nodeYAMLBootstrap)

	if cfg.Name != "laptop-worker" {
		t.Errorf("Name = %q, want laptop-worker", cfg.Name)
	}
	if cfg.Server != "http://localhost:8080" {
		t.Errorf("Server = %q, want http://localhost:8080", cfg.Server)
	}
	if cfg.Token != "ntk_local_dev" {
		t.Errorf("Token = %q, want ntk_local_dev", cfg.Token)
	}
	if cfg.Instances != 2 {
		t.Errorf("Instances = %d, want 2", cfg.Instances)
	}
	// 结构体形状本身也是断言：nodeConfig 只有四件 bootstrap（D4：机器不自报能力）。
	want := nodeConfig{Name: "laptop-worker", Server: "http://localhost:8080",
		Token: "ntk_local_dev", Instances: 2}
	if *cfg != want {
		t.Errorf("nodeConfig = %#v, want %#v", *cfg, want)
	}
}

// TestConfigValidateErrors：缺 server / 缺 token / instances 负数都必须报错，
// 且错误串前面带文件路径。
func TestConfigValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{
			// 空文件：yaml 解码给 io.EOF，不该当成解码错误，而是让 validate
			// 把「缺什么」一次列全。
			name: "空文件",
			yaml: "",
			want: []string{"server is required", "token is required"},
		},
		{
			name: "token 空",
			yaml: `name: n
server: http://localhost:8080
token: ""
`,
			want: []string{"token is required"},
		},
		{
			name: "server 空",
			yaml: `name: n
server: ""
token: ntk_x
`,
			want: []string{"server is required"},
		},
		{
			name: "server 无 scheme",
			yaml: `name: n
server: localhost:8080
token: ntk_x
`,
			want: []string{"server", "is not a valid URL"},
		},
		{
			name: "server 非法 scheme",
			yaml: `name: n
server: ftp://localhost:8080
token: ntk_x
`,
			want: []string{"server", "is not a valid URL"},
		},
		{
			name: "instances 负数",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
instances: -1
`,
			want: []string{"instances must be >= 0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeNodeYAML(t, tc.yaml)
			cfg, err := loadConfig(path)
			if err == nil {
				t.Fatalf("loadConfig: want error, got nil (cfg=%#v)", cfg)
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, path) {
				t.Errorf("error %q is not prefixed with the config path %q", msg, path)
			}
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
		})
	}
}

// TestConfigValidateListsAllProblems：校验一次列全，用 "; " 连接（不是只报第一条）。
func TestConfigValidateListsAllProblems(t *testing.T) {
	cfg := mustLoadNodeConfig(t, nodeYAMLBootstrap)

	// 同一份合法配置上改坏三处：缺 token + 缺 server + 实例数负数。
	cfg.Token = ""
	cfg.Server = ""
	cfg.Instances = -2
	err := cfg.validate()
	if err == nil {
		t.Fatal("validate: want error, got nil")
	}
	const want = "server is required; token is required; instances must be >= 0"
	if err.Error() != want {
		t.Errorf("validate() = %q, want %q", err.Error(), want)
	}
}

// TestConfigUnknownField：未知 YAML 字段必须报错（KnownFields，防拼错静默失效）。
//
// 这里还钉住一条**迁移护栏**：v1 的 `capabilities:` 现在是未知字段。老 node.yaml
// 必须在启动时被明确拒绝——静默连上去、却永远接不到单，是比报错难查得多的故障。
func TestConfigUnknownField(t *testing.T) {
	t.Run("顶层字段", func(t *testing.T) {
		path := writeNodeYAML(t, strings.Replace(nodeYAMLBootstrap, "instances: 2",
			"instances: 2\nbogus_field: 1", 1))
		_, err := loadConfig(path)
		if err == nil {
			t.Fatal("loadConfig: want error for unknown field, got nil")
		}
		if !strings.Contains(err.Error(), "bogus_field") {
			t.Errorf("error %q does not name the unknown field bogus_field", err)
		}
		if !strings.HasPrefix(err.Error(), path) {
			t.Errorf("error %q is not prefixed with the config path", err)
		}
	})

	t.Run("v1 的 capabilities 现在是未知字段", func(t *testing.T) {
		path := writeNodeYAML(t, `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
`)
		_, err := loadConfig(path)
		if err == nil {
			t.Fatal("loadConfig: v1 的 capabilities 必须报错（能力不再由机器声明），却通过了")
		}
		if !strings.Contains(err.Error(), "capabilities") {
			t.Errorf("error %q does not name the removed field capabilities", err)
		}
	})
}

// TestConfigMissingFile：文件不存在时错误带路径（-f 的校验也靠这个）。
func TestConfigMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig: want error for missing file, got nil")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not contain the config path %q", err, path)
	}
}

// TestConfigStreamURL：streamURL 表格测试，契约 §3.1.2 六行全覆盖。
func TestConfigStreamURL(t *testing.T) {
	cases := []struct {
		server  string
		want    string
		wantErr bool
	}{
		{server: "http://localhost:8080", want: "ws://localhost:8080/v1/node/stream"},
		{server: "http://localhost:8080/", want: "ws://localhost:8080/v1/node/stream"},
		{server: "https://a.example", want: "wss://a.example/v1/node/stream"},
		{server: "ws://h:1", want: "ws://h:1/v1/node/stream"},
		{server: "wss://h:1", want: "wss://h:1/v1/node/stream"},
		{server: "ws://h:1/v1/node/stream", want: "ws://h:1/v1/node/stream"},
		{server: "localhost:8080", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.server, func(t *testing.T) {
			cfg := &nodeConfig{Server: tc.server}
			got, err := cfg.streamURL()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("streamURL(%q) = %q, want error", tc.server, got)
				}
				if !strings.Contains(err.Error(), "is not a valid URL") {
					t.Errorf("error %q does not contain %q", err, "is not a valid URL")
				}
				return
			}
			if err != nil {
				t.Fatalf("streamURL(%q): unexpected error: %v", tc.server, err)
			}
			if got != tc.want {
				t.Errorf("streamURL(%q) = %q, want %q", tc.server, got, tc.want)
			}
		})
	}
}

// TestConfigHello：hello 帧的**新形状**（契约 §3）。
//
// 断言：没有 capabilities/manifest 两键（v2 删了）；applied 原样透传；
// instances 数量 = slots()、每槽并发 = 1；protocol = 2。
func TestConfigHello(t *testing.T) {
	cfg := mustLoadNodeConfig(t, nodeYAMLBootstrap)
	applied := []appliedDecl{
		{Capability: "demo.prep", Version: 1, Digest: "sha256:" + strings.Repeat("a", 64)},
		{Capability: "demo.review", Version: 2, Digest: "sha256:" + strings.Repeat("b", 64)},
	}
	h := cfg.hello(applied)

	if h.Type != ws.NodeFrameHello {
		t.Errorf("Type = %q, want %q", h.Type, ws.NodeFrameHello)
	}
	if h.Protocol != ws.ProtocolVersion {
		t.Errorf("Protocol = %d, want %d", h.Protocol, ws.ProtocolVersion)
	}
	if ws.ProtocolVersion != 2 {
		t.Errorf("ws.ProtocolVersion = %d, want 2（console-declares 升版）", ws.ProtocolVersion)
	}
	if h.NodeID != "" {
		t.Errorf("NodeID = %q, want empty (由服务端按令牌认)", h.NodeID)
	}
	if h.Labels != nil {
		t.Errorf("Labels = %#v, want nil", h.Labels)
	}
	if !reflect.DeepEqual(h.Applied, applied) {
		t.Errorf("Applied = %#v, want %#v（缓存索引原样透传）", h.Applied, applied)
	}

	// instances：槽数 = slots()，每槽并发 = 1（v2 不再有 per-capability concurrency）。
	if len(h.Instances) != cfg.slots() {
		t.Fatalf("len(Instances) = %d, want slots() = %d", len(h.Instances), cfg.slots())
	}
	for i, inst := range h.Instances {
		if inst.Slot != i {
			t.Errorf("Instances[%d].Slot = %d, want %d", i, inst.Slot, i)
		}
		if inst.Concurrency != 1 {
			t.Errorf("Instances[%d].Concurrency = %d, want 1", i, inst.Concurrency)
		}
	}

	// 线上字节里不能出现 capabilities / manifest 两个键（v2 删除）。
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("json.Marshal(hello): %v", err)
	}
	for _, banned := range []string{`"capabilities"`, `"manifest"`} {
		if strings.Contains(string(data), banned) {
			t.Errorf("hello 帧里仍然有 %s 键（v2 已删除）：%s", banned, data)
		}
	}

	// 服务端必须能把这帧读回来（tag 漂移由 ws.DecodeUpstream 当场暴露）。
	frame, err := ws.DecodeUpstream(data)
	if err != nil {
		t.Fatalf("ws.DecodeUpstream(hello) = %v (json=%s)", err, data)
	}
	back, ok := frame.(*ws.HelloFrame)
	if !ok {
		t.Fatalf("ws.DecodeUpstream(hello) = %T, want *ws.HelloFrame", frame)
	}
	if !reflect.DeepEqual(back.Applied, applied) {
		t.Errorf("服务端回读 applied = %#v, want %#v", back.Applied, applied)
	}
	if len(back.Instances) != len(h.Instances) {
		t.Errorf("服务端回读 instances = %#v, want %#v", back.Instances, h.Instances)
	}
	if back.Protocol != 2 {
		t.Errorf("服务端回读 protocol = %d, want 2", back.Protocol)
	}
}

// TestConfigHelloEmptyApplied：没有任何缓存时 applied 必须是 []，不是 null
// （空列表纪律：服务端/前端把 null 当异常）。
func TestConfigHelloEmptyApplied(t *testing.T) {
	cfg := mustLoadNodeConfig(t, nodeYAMLBootstrap)
	for _, in := range [][]appliedDecl{nil, {}} {
		h := cfg.hello(in)
		if h.Applied == nil {
			t.Fatalf("Applied = nil, want 空切片（%v）", in)
		}
		data, err := json.Marshal(h)
		if err != nil {
			t.Fatalf("json.Marshal(hello): %v", err)
		}
		if !strings.Contains(string(data), `"applied":[]`) {
			t.Errorf("hello = %s, want \"applied\":[]", data)
		}
	}
}

// TestConfigDefaults：缺省归一——instances 缺省 → 1；归一在**读取处**表达，
// 不改写用户结构体。
func TestConfigDefaults(t *testing.T) {
	cfg := mustLoadNodeConfig(t, `name: n
server: http://localhost:8080
token: ntk_x
`)

	if cfg.Instances != 0 {
		t.Errorf("Instances = %d, want 0（缺省值不被改写进结构体）", cfg.Instances)
	}
	if got := cfg.slots(); got != 1 {
		t.Errorf("slots() = %d, want 1", got)
	}
	if got := cfg.slotConcurrency(); got != 1 {
		t.Errorf("slotConcurrency() = %d, want 1（v2 每个槽位一个任务）", got)
	}
	if got := cfg.taskLimit(); got != 1 {
		t.Errorf("taskLimit() = %d, want 1", got)
	}

	h := cfg.hello(nil)
	wantInstances := []instanceDecl{{Slot: 0, Concurrency: 1}}
	if !reflect.DeepEqual(h.Instances, wantInstances) {
		t.Errorf("Instances = %#v, want %#v", h.Instances, wantInstances)
	}

	// instances 显式给了就按给的算（3 槽 × 每槽 1）。
	cfg.Instances = 3
	if got := cfg.slots(); got != 3 {
		t.Errorf("slots() = %d, want 3", got)
	}
	if got := cfg.taskLimit(); got != 3 {
		t.Errorf("taskLimit() = %d, want 3", got)
	}
	if got := len(cfg.hello(nil).Instances); got != 3 {
		t.Errorf("len(hello().Instances) = %d, want 3", got)
	}
}

// TestDefaultDataDir：缺省数据目录是 ~/.anotify（产物要留着，不能落 /tmp）。
func TestDefaultDataDir(t *testing.T) {
	got, err := defaultDataDir()
	if err != nil {
		t.Fatalf("defaultDataDir: %v", err)
	}
	if filepath.Base(got) != ".anotify" {
		t.Errorf("defaultDataDir() = %q, want .../.anotify", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("defaultDataDir() = %q, want 绝对路径", got)
	}
}

// ---------- 环境变量 bootstrap（容器/编排路径） ----------
//
// 控制台「加机器」给的那行 docker 命令只带 -e，容器里没有 node.yaml 可指。
// 这组用例钉住：那条命令**真的能起来**，而不是一个好看的字符串。

// envMap 把 map 变成 getenv 函数（测试用，避免污染进程环境）。
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestConfigFromEnv 令牌在就生效，四件都能从环境变量来。
func TestConfigFromEnv(t *testing.T) {
	cfg, ok := configFromEnv(envMap(map[string]string{
		envServer:    "https://anotify.example.com",
		envToken:     "nt_abc",
		envName:      "buildbox-1",
		envInstances: "4",
	}))
	if !ok {
		t.Fatal("有 ANOTIFY_TOKEN 却没走环境变量路径")
	}
	if cfg.Server != "https://anotify.example.com" || cfg.Token != "nt_abc" ||
		cfg.Name != "buildbox-1" || cfg.Instances != 4 {
		t.Fatalf("解析结果不对: %+v", cfg)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("这份配置应该合法: %v", err)
	}
}

// TestConfigFromEnvNeedsToken 没有令牌 = 没打算走这条路（返回 false 让它回落）。
//
// 为什么不「报错说缺 token」：二进制装在机器上的人根本没有环境变量，
// 对他们来说「没有 ANOTIFY_TOKEN」是常态，报错会把 ./node.yaml 这条路堵死。
func TestConfigFromEnvNeedsToken(t *testing.T) {
	if _, ok := configFromEnv(envMap(map[string]string{envServer: "https://x"})); ok {
		t.Fatal("没有 ANOTIFY_TOKEN 却判定走环境变量")
	}
}

// TestConfigFromEnvBadInstances 非法槽位数必须变成**可诊断的错误**，不是静默当 1。
func TestConfigFromEnvBadInstances(t *testing.T) {
	cfg, ok := configFromEnv(envMap(map[string]string{envToken: "nt_x", envInstances: "abc"}))
	if !ok {
		t.Fatal("有令牌就走这条路")
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "instances") {
		t.Fatalf("期望 instances 相关错误，实际: %v", err)
	}
}

// TestConfigFromEnvMissingServer 缺 server 必须报错（猜一个会表现为「连不上」）。
func TestConfigFromEnvMissingServer(t *testing.T) {
	cfg, ok := configFromEnv(envMap(map[string]string{envToken: "nt_x"}))
	if !ok {
		t.Fatal("有令牌就走这条路")
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "server is required") {
		t.Fatalf("期望 server 必填报错，实际: %v", err)
	}
}

// TestLoadConfigFromPrecedence 显式 -f 永远压过环境变量，且读不到就报错。
//
// 这条是「我改的文件没生效」的唯一防线：如果显式 -f 失败时悄悄回落到环境变量，
// 用户会看到 agent 正常启动、连的却是另一次配置里的中枢。
func TestLoadConfigFromPrecedence(t *testing.T) {
	env := envMap(map[string]string{
		envServer: "https://from-env.example.com",
		envToken:  "nt_env",
	})

	// ① 显式 -f 指向不存在的文件 → 报错（不回落到环境变量）
	if _, err := loadConfigFrom("/nonexistent/node.yaml", env); err == nil {
		t.Fatal("显式 -f 指向不存在的文件却没报错（可能悄悄回落到了环境变量）")
	}

	// ② 显式 -f 指向真实文件 → 用文件，无视环境变量
	dir := t.TempDir()
	p := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(p, []byte("name: from-file\nserver: https://file.example.com\ntoken: nt_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFrom(p, env)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	if cfg.Token != "nt_file" || cfg.Server != "https://file.example.com" {
		t.Fatalf("显式 -f 应该压过环境变量，实际: %+v", cfg)
	}

	// ③ 没给 -f → 环境变量生效
	cfg, err = loadConfigFrom("", env)
	if err != nil {
		t.Fatalf("环境变量路径失败: %v", err)
	}
	if cfg.Token != "nt_env" {
		t.Fatalf("没给 -f 时应该走环境变量，实际 token=%q", cfg.Token)
	}
}
