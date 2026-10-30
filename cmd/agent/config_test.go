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

// nodeYAMLFull 是 issue #184 里的完整 node.yaml 形状：name/server/token/instances，
// 两条能力各带 run（runner/command/timeoutSec/concurrency/env），第一条另带 out
// （issue #189：成功时回报的出口名）。
const nodeYAMLFull = `# node.yaml —— 节点侧唯一声明入口
name: laptop-worker
server: http://localhost:8080
token: ntk_local_dev
instances: 2
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
      timeoutSec: 30
      concurrency: 1
      env:
        LOG_LEVEL: info
      out: ok
  - capability: demo.review
    version: 2
    run:
      runner: bash
      command: ./runners/review.sh
      timeoutSec: 900
      concurrency: 2
      env:
        MODE: strict
        DRY_RUN: "1"
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

// TestConfigLoadIssueYAML：issue 原文 YAML 全字段解码正确（W1 必测 1）。
func TestConfigLoadIssueYAML(t *testing.T) {
	cfg := mustLoadNodeConfig(t, nodeYAMLFull)

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

	want := []capabilityConfig{
		{
			Capability: "demo.prep",
			Version:    1,
			Run: &runSpec{
				Runner:      "bash",
				Command:     "echo prep",
				TimeoutSec:  30,
				Concurrency: 1,
				Env:         map[string]string{"LOG_LEVEL": "info"},
				Out:         "ok",
			},
		},
		{
			Capability: "demo.review",
			Version:    2,
			Run: &runSpec{
				Runner:      "bash",
				Command:     "./runners/review.sh",
				TimeoutSec:  900,
				Concurrency: 2,
				Env:         map[string]string{"MODE": "strict", "DRY_RUN": "1"},
			},
		},
	}
	if !reflect.DeepEqual(cfg.Capabilities, want) {
		t.Errorf("Capabilities = %#v, want %#v", cfg.Capabilities, want)
	}
}

// TestConfigValidateErrors：各类非法输入都必须报错，且错误子串与契约 §3.1.1 一致
// （W1 必测 2）。错误串前面还要带文件路径。
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
			want: []string{
				"server is required",
				"token is required",
				"at least one capability is required",
			},
		},
		{
			name: "run 缺失",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
`,
			want: []string{"demo.prep@1: run is required"},
		},
		{
			name: "runner 缺失",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      command: echo prep
`,
			want: []string{"demo.prep@1: run.runner is required (only bash)"},
		},
		{
			name: "runner 非 bash",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: node
      command: echo prep
`,
			want: []string{`demo.prep@1: run.runner "node" is not supported (only bash)`},
		},
		{
			name: "command 空白",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: "   "
`,
			want: []string{"demo.prep@1: run.command is empty"},
		},
		{
			name: "timeoutSec 负数",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
      timeoutSec: -1
`,
			want: []string{"demo.prep@1: run.timeoutSec must be >= 0"},
		},
		{
			name: "concurrency 负数",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
      concurrency: -2
`,
			want: []string{"demo.prep@1: run.concurrency must be >= 0"},
		},
		{
			name: "token 空",
			yaml: `name: n
server: http://localhost:8080
token: ""
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
`,
			want: []string{"token is required"},
		},
		{
			name: "server 空",
			yaml: `name: n
server: ""
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
`,
			want: []string{"server is required"},
		},
		{
			name: "server 无 scheme",
			yaml: `name: n
server: localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
`,
			want: []string{"server", "is not a valid URL"},
		},
		{
			name: "server 非法 scheme",
			yaml: `name: n
server: ftp://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
`,
			want: []string{"server", "is not a valid URL"},
		},
		{
			name: "instances 负数",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
instances: -1
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
`,
			want: []string{"instances must be >= 0"},
		},
		{
			name: "capabilities 为空",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities: []
`,
			want: []string{"at least one capability is required"},
		},
		{
			name: "capability 名为空",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: ""
    version: 1
    run:
      runner: bash
      command: echo prep
`,
			want: []string{"capability is required"},
		},
		{
			name: "version 小于 1",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 0
    run:
      runner: bash
      command: echo prep
`,
			want: []string{"demo.prep: version must be >= 1"},
		},
		{
			name: "同名同版本重复",
			yaml: `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep-again
`,
			want: []string{"duplicate capability demo.prep@1"},
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
	cfg := mustLoadNodeConfig(t, `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
`)

	// 同一份合法配置上改坏三处：缺 token + 版本非法 + 命令空白。
	cfg.Token = ""
	cfg.Capabilities[0].Version = 0
	cfg.Capabilities[0].Run.Command = " "
	err := cfg.validate()
	if err == nil {
		t.Fatal("validate: want error, got nil")
	}
	const want = "token is required; demo.prep: version must be >= 1; demo.prep@0: run.command is empty"
	if err.Error() != want {
		t.Errorf("validate() = %q, want %q", err.Error(), want)
	}
}

// TestConfigUnknownField：未知 YAML 字段必须报错（KnownFields，防拼错静默失效）。
func TestConfigUnknownField(t *testing.T) {
	t.Run("顶层字段", func(t *testing.T) {
		path := writeNodeYAML(t, strings.Replace(nodeYAMLFull, "instances: 2", "instances: 2\nbogus_field: 1", 1))
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

	t.Run("run 块内字段", func(t *testing.T) {
		path := writeNodeYAML(t, strings.Replace(nodeYAMLFull, "      timeoutSec: 30",
			"      timeoutSec: 30\n      typoKey: 1", 1))
		_, err := loadConfig(path)
		if err == nil {
			t.Fatal("loadConfig: want error for unknown run field, got nil")
		}
		if !strings.Contains(err.Error(), "typoKey") {
			t.Errorf("error %q does not name the unknown field typoKey", err)
		}
	})
}

// TestConfigMissingFile：文件不存在时错误带路径（W5 的 -f 校验也靠这个）。
func TestConfigMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig: want error for missing file, got nil")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not contain the config path %q", err, path)
	}
}

// TestConfigStreamURL：streamURL 表格测试，契约 §3.1.2 六行全覆盖（W1 必测 4）。
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

// TestConfigHello：hello 帧的声明部分（W1 必测 5）。
//
// 断言：capabilities 顺序 = yaml 顺序；manifest.run 是 runSpec 的紧凑 JSON 且能被
// 回读成同一份 runSpec；instances 数量 = slots() 且每槽 concurrency = slotConcurrency()。
func TestConfigHello(t *testing.T) {
	cfg := mustLoadNodeConfig(t, nodeYAMLFull)
	h := cfg.hello()

	if h.Type != ws.NodeFrameHello {
		t.Errorf("Type = %q, want %q", h.Type, ws.NodeFrameHello)
	}
	if h.Protocol != ws.ProtocolVersion {
		t.Errorf("Protocol = %d, want %d", h.Protocol, ws.ProtocolVersion)
	}
	if h.NodeID != "" {
		t.Errorf("NodeID = %q, want empty (由服务端按令牌认)", h.NodeID)
	}
	if h.Labels != nil {
		t.Errorf("Labels = %#v, want nil", h.Labels)
	}

	wantCaps := []capabilityDecl{
		{Capability: "demo.prep", Version: 1},
		{Capability: "demo.review", Version: 2},
	}
	if !reflect.DeepEqual(h.Capabilities, wantCaps) {
		t.Errorf("Capabilities = %#v, want %#v", h.Capabilities, wantCaps)
	}

	if len(h.Manifest) != len(wantCaps) {
		t.Fatalf("len(Manifest) = %d, want %d", len(h.Manifest), len(wantCaps))
	}
	for i, want := range wantCaps {
		if h.Manifest[i].Capability != want.Capability || h.Manifest[i].Version != want.Version {
			t.Errorf("Manifest[%d] = %s@%d, want %s@%d",
				i, h.Manifest[i].Capability, h.Manifest[i].Version, want.Capability, want.Version)
		}
		var back runSpec
		if err := json.Unmarshal(h.Manifest[i].Run, &back); err != nil {
			t.Fatalf("manifest[%d].run is not valid JSON: %v (%s)", i, err, h.Manifest[i].Run)
		}
		if !reflect.DeepEqual(&back, cfg.Capabilities[i].Run) {
			t.Errorf("manifest[%d].run round-trip = %#v, want %#v", i, back, *cfg.Capabilities[i].Run)
		}
	}
	// run 原文是紧凑 JSON（没有空格），零值字段由 omitempty 省略。
	// 字段顺序 = runSpec 结构体顺序（out 在最后）。
	const wantRun0 = `{"runner":"bash","command":"echo prep","timeoutSec":30,"concurrency":1,"env":{"LOG_LEVEL":"info"},"out":"ok"}`
	if got := string(h.Manifest[0].Run); got != wantRun0 {
		t.Errorf("manifest[0].run = %s, want %s", got, wantRun0)
	}

	// instances：槽数 = slots()，每槽并发 = slotConcurrency()。
	if len(h.Instances) != cfg.slots() {
		t.Fatalf("len(Instances) = %d, want slots() = %d", len(h.Instances), cfg.slots())
	}
	for i, inst := range h.Instances {
		if inst.Slot != i {
			t.Errorf("Instances[%d].Slot = %d, want %d", i, inst.Slot, i)
		}
		if want := cfg.slotConcurrency(); inst.Concurrency != want {
			t.Errorf("Instances[%d].Concurrency = %d, want slotConcurrency() = %d",
				i, inst.Concurrency, want)
		}
	}
	if got := cfg.slotConcurrency(); got != 2 {
		t.Errorf("slotConcurrency() = %d, want 2 (max of run.concurrency)", got)
	}

	// 服务端必须能把这帧读回来（tag 漂移由 ws.DecodeUpstream 当场暴露）。
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("json.Marshal(hello): %v", err)
	}
	frame, err := ws.DecodeUpstream(data)
	if err != nil {
		t.Fatalf("ws.DecodeUpstream(hello) = %v (json=%s)", err, data)
	}
	back, ok := frame.(*ws.HelloFrame)
	if !ok {
		t.Fatalf("ws.DecodeUpstream(hello) = %T, want *ws.HelloFrame", frame)
	}
	if !reflect.DeepEqual(back.Capabilities, wantCaps) || len(back.Instances) != len(h.Instances) {
		t.Errorf("服务端回读 = %#v, want capabilities %#v / %d instances",
			back, wantCaps, len(h.Instances))
	}
}

// TestConfigDefaults：缺省归一（W1 必测 6）——instances 缺省 → 1，concurrency 缺省 → 1；
// 归一在**读取处**表达，不改写用户结构体。
func TestConfigDefaults(t *testing.T) {
	cfg := mustLoadNodeConfig(t, `name: n
server: http://localhost:8080
token: ntk_x
capabilities:
  - capability: demo.prep
    version: 1
    run:
      runner: bash
      command: echo prep
`)

	if cfg.Instances != 0 {
		t.Errorf("Instances = %d, want 0（缺省值不被改写进结构体）", cfg.Instances)
	}
	if got := cfg.Capabilities[0].Run.Concurrency; got != 0 {
		t.Errorf("Run.Concurrency = %d, want 0（缺省值不被改写进结构体）", got)
	}
	if got := cfg.slots(); got != 1 {
		t.Errorf("slots() = %d, want 1", got)
	}
	if got := cfg.slotConcurrency(); got != 1 {
		t.Errorf("slotConcurrency() = %d, want 1", got)
	}
	if got := cfg.taskLimit(cfg.Capabilities[0].Run); got != 1 {
		t.Errorf("taskLimit(run) = %d, want 1", got)
	}
	if got := cfg.taskLimit(nil); got != 1 {
		t.Errorf("taskLimit(nil) = %d, want 1", got)
	}

	h := cfg.hello()
	wantInstances := []instanceDecl{{Slot: 0, Concurrency: 1}}
	if !reflect.DeepEqual(h.Instances, wantInstances) {
		t.Errorf("Instances = %#v, want %#v", h.Instances, wantInstances)
	}
	// 零值字段（timeoutSec/concurrency/env/out）不出现在 run 原文里 ——
	// out 的 omitempty 是硬要求（未声明时 run 原文逐字节不变）。
	const wantRun = `{"runner":"bash","command":"echo prep"}`
	if got := string(h.Manifest[0].Run); got != wantRun {
		t.Errorf("manifest[0].run = %s, want %s", got, wantRun)
	}

	// instances 显式给了就按给的算（3 槽 × 缺省并发 1）。
	cfg.Instances = 3
	if got := cfg.slots(); got != 3 {
		t.Errorf("slots() = %d, want 3", got)
	}
	if got := len(cfg.hello().Instances); got != 3 {
		t.Errorf("len(hello().Instances) = %d, want 3", got)
	}
}

// TestConfigLookup：name@version → run 块（client 侧 ack 后执行要靠它）。
func TestConfigLookup(t *testing.T) {
	cfg := mustLoadNodeConfig(t, nodeYAMLFull)

	if rs, ok := cfg.lookup("demo.review", 2); !ok || rs == nil {
		t.Fatalf("lookup(demo.review, 2) = (%v, %v), want run, true", rs, ok)
	} else if !reflect.DeepEqual(rs, cfg.Capabilities[1].Run) {
		t.Errorf("lookup(demo.review, 2) = %#v, want %#v", rs, *cfg.Capabilities[1].Run)
	}
	if rs, ok := cfg.lookup("demo.prep", 2); ok || rs != nil {
		t.Errorf("lookup(demo.prep, 2) = (%v, %v), want (nil, false)（版本不同）", rs, ok)
	}
	if rs, ok := cfg.lookup("nope", 1); ok || rs != nil {
		t.Errorf("lookup(nope, 1) = (%v, %v), want (nil, false)", rs, ok)
	}

	// 声明了能力但没写 run → 不可执行，同样报 false。
	bare := &nodeConfig{Capabilities: []capabilityConfig{{Capability: "demo.bare", Version: 1}}}
	if rs, ok := bare.lookup("demo.bare", 1); ok || rs != nil {
		t.Errorf("lookup(没有 run 的能力) = (%v, %v), want (nil, false)", rs, ok)
	}
}

// TestConfigTaskLimit：每能力并发上限 = 槽位 × 该能力的 concurrency。
func TestConfigTaskLimit(t *testing.T) {
	cfg := mustLoadNodeConfig(t, nodeYAMLFull)

	if got := cfg.taskLimit(cfg.Capabilities[0].Run); got != 2 {
		t.Errorf("taskLimit(demo.prep run) = %d, want 2（2 槽 × 1）", got)
	}
	if got := cfg.taskLimit(cfg.Capabilities[1].Run); got != 4 {
		t.Errorf("taskLimit(demo.review run) = %d, want 4（2 槽 × 2）", got)
	}
	if got := cfg.taskLimit(nil); got != 2 {
		t.Errorf("taskLimit(nil) = %d, want 2（= slots()）", got)
	}

	zero := &runSpec{Runner: "bash", Command: "echo hi"}
	if got := cfg.taskLimit(zero); got != 2 {
		t.Errorf("taskLimit(concurrency 缺省) = %d, want 2（缺省记 1）", got)
	}
}
