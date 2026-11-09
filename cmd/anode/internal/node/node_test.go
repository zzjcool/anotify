package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/cmd/anode/internal/applied"
	"github.com/zzjcool/anotify/cmd/anode/internal/capability"
	"github.com/zzjcool/anotify/cmd/anode/internal/config"
	"github.com/zzjcool/anotify/cmd/anode/internal/hostenv"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

type fixture struct {
	node   *Node
	cfg    *config.Config
	dir    string
	binDir string
	home   string
	env    map[string]string
}

// newFixture 用给定 server 与额外 YAML 片段装配节点（默认申报 [default, team-a]）。
func newFixture(t *testing.T, serverURL, extraYAML string) *fixture {
	t.Helper()
	yaml := "name: node-a\nserver: " + serverURL + "\ntoken: node-secret-token\n" +
		"namespaces: [default, team-a]\n" + extraYAML
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	dir := t.TempDir()
	binDir := t.TempDir()
	home := t.TempDir()
	env := map[string]string{"GITHUB_TOKEN": "ghp-very-secret"}
	baseline := hostenv.CaptureWith(cfg.EnvPassthrough, func(key string) (string, bool) {
		switch key {
		case "PATH":
			return binDir, true
		case "HOME":
			return home, true
		default:
			v, ok := env[key]
			return v, ok
		}
	})
	n, err := New(Options{
		Config:       cfg,
		Store:        applied.NewStore(dir),
		Cache:        capability.NewCache(filepath.Join(dir, "capabilities")),
		Baseline:     baseline,
		Facts:        protocol.NodeFacts{HostIP: "10.0.0.9", Hostname: "host-a", OS: "linux", Arch: "amd64", CPU: 4, WorkRoot: filepath.Join(dir, "work"), HomeDir: home},
		AgentVersion: "v9.9.9",
		AgentCommit:  "deadbeef",
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return &fixture{node: n, cfg: cfg, dir: dir, binDir: binDir, home: home, env: env}
}

func welcomeFor(t *testing.T, revision string, nodeVars map[string]string, nodeLimits protocol.Limits, namespaces []string) protocol.WelcomeFrame {
	t.Helper()
	return protocol.WelcomeFrame{
		Type:           protocol.FrameWelcome,
		ConfigRevision: revision,
		NodeVars:       nodeVars,
		NodeLimits:     nodeLimits,
		Namespaces:     namespaces,
	}
}

func TestColdStartHelloHasNoAppliedAndEmptyInstances(t *testing.T) {
	f := newFixture(t, "wss://hub.example", "vars:\n  SOURCE_ROOT: /local/src\nlimits:\n  total: 2\n")
	hello := f.node.Hello()
	if hello.Type != protocol.FrameHello || hello.Name != "node-a" || hello.Token != "node-secret-token" {
		t.Fatalf("hello 身份 = %+v", hello)
	}
	if hello.AppliedRevision != "" || hello.AppliedHash != "" {
		t.Fatalf("冷启动不应有 applied 事实: %q/%q", hello.AppliedRevision, hello.AppliedHash)
	}
	if len(hello.Instances) != 0 {
		t.Fatalf("冷启动不应伪造槽位: %v", hello.Instances)
	}
	if hello.LocalVars["SOURCE_ROOT"] != "/local/src" {
		t.Fatalf("LocalVars = %v", hello.LocalVars)
	}
	if hello.LocalLimits.Total != 2 {
		t.Fatalf("LocalLimits = %+v", hello.LocalLimits)
	}
	if strings.Join(hello.Namespaces, ",") != "default,team-a" {
		t.Fatalf("Namespaces = %v", hello.Namespaces)
	}
	if hello.Facts.Hostname != "host-a" || hello.Facts.CPU != 4 {
		t.Fatalf("Facts = %+v", hello.Facts)
	}
	if hello.AgentVersion != "v9.9.9" || hello.AgentCommit != "deadbeef" {
		t.Fatalf("AgentVersion = %q/%q", hello.AgentVersion, hello.AgentCommit)
	}
	if len(hello.Capabilities) != 0 {
		t.Fatalf("冷启动缓存为空，Capabilities 应为 []: %v", hello.Capabilities)
	}
}

func TestHelloDoesNotLeakPassthroughOrTokenToAppliedAndSnapshot(t *testing.T) {
	f := newFixture(t, "wss://hub.example", "envPassthrough: [GITHUB_TOKEN]\nvars:\n  SOURCE_ROOT: /local/src\n")
	hello := f.node.Hello()
	helloJSON, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	// 身份字段允许出现 token；凭证透传的**名字与值**都不允许出现在快照里。
	if !strings.Contains(string(helloJSON), "node-secret-token") {
		t.Fatal("hello 必须具备身份 token（唯一允许的凭据字段）")
	}
	for _, forbidden := range []string{"ghp-very-secret", "GITHUB_TOKEN"} {
		if strings.Contains(string(helloJSON), forbidden) {
			t.Fatalf("hello 泄露了 %q: %s", forbidden, helloJSON)
		}
	}
	frame, err := f.node.ApplyWelcome(welcomeFor(t, "rev-1", nil, protocol.Limits{Total: 2}, []string{"*"}))
	if err != nil || frame == nil {
		t.Fatalf("ApplyWelcome = %v, %v", frame, err)
	}
	frameJSON, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	appliedJSON, err := os.ReadFile(filepath.Join(f.dir, protocol.AppliedFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"node-secret-token", "ghp-very-secret", "GITHUB_TOKEN"} {
		if strings.Contains(string(frameJSON), forbidden) {
			t.Fatalf("config.applied 帧泄露 %q: %s", forbidden, frameJSON)
		}
		if strings.Contains(string(appliedJSON), forbidden) {
			t.Fatalf("applied 文件泄露 %q: %s", forbidden, appliedJSON)
		}
	}
}

func TestWelcomeApplyWritesAppliedThenFrame(t *testing.T) {
	f := newFixture(t, "wss://hub.example", "vars:\n  SOURCE_ROOT: /local/src\nlimits:\n  total: 4\n  perCapability:\n    reviewer: 2\n")
	welcome := welcomeFor(t, "rev-7", map[string]string{"SOURCE_ROOT": "/node/src", "NODE_ONLY": "n"},
		protocol.Limits{Total: 8, PerCapability: map[string]int{"reviewer": 5, "tester": 3}}, []string{"*"})
	frame, err := f.node.ApplyWelcome(welcome)
	if err != nil {
		t.Fatalf("ApplyWelcome: %v", err)
	}
	if frame == nil {
		t.Fatal("首次 apply 必须回报 config.applied")
	}
	if frame.Type != protocol.FrameConfigApplied || frame.Revision != "rev-7" {
		t.Fatalf("frame = %+v", frame)
	}
	if frame.MachineVars["SOURCE_ROOT"] != "/local/src" || frame.MachineVars["NODE_ONLY"] != "n" {
		t.Fatalf("machineVars 合并错误: %v", frame.MachineVars)
	}
	if !f.node.AppliedPresent() {
		t.Fatal("成功落盘后 AppliedPresent 应为 true")
	}
	rev, hash := f.node.AppliedState()
	if rev != "rev-7" || hash != frame.Hash || hash == "" {
		t.Fatalf("AppliedState = %q/%q, frame = %+v", rev, hash, frame)
	}
	loaded, present, err := applied.NewStore(f.dir).Load()
	if err != nil || !present {
		t.Fatalf("落盘 Load = %v, %v", present, err)
	}
	if loaded.Hash != frame.Hash || loaded.Revision != "rev-7" {
		t.Fatalf("落盘内容 = %+v", loaded)
	}
	if loaded.Limits.Total != 4 || loaded.Limits.PerCapability["reviewer"] != 2 || loaded.Limits.PerCapability["tester"] != 3 {
		t.Fatalf("limits 逐叶合并错误: %+v", loaded.Limits)
	}
	// 槽位来自机器级生效 limits。
	hello := f.node.Hello()
	if len(hello.Instances) != 4 {
		t.Fatalf("instances = %v", hello.Instances)
	}
	for i, inst := range hello.Instances {
		if inst.Slot != i || inst.Concurrency != 1 || inst.Busy {
			t.Fatalf("instance[%d] = %+v", i, inst)
		}
	}
}

func TestWelcomeIdempotentAndRestartRemergesLocal(t *testing.T) {
	f := newFixture(t, "wss://hub.example", "vars:\n  SOURCE_ROOT: /local/src\nlimits:\n  total: 2\n")
	welcome := welcomeFor(t, "rev-1", nil, protocol.Limits{Total: 8}, []string{"*"})
	first, err := f.node.ApplyWelcome(welcome)
	if err != nil || first == nil {
		t.Fatalf("first apply = %v, %v", first, err)
	}
	before, err := os.ReadFile(filepath.Join(f.dir, protocol.AppliedFileName))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.node.ApplyWelcome(welcome)
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Fatal("同一 revision + 同内容应幂等：不重复写盘/不重复回报")
	}
	after, err := os.ReadFile(filepath.Join(f.dir, protocol.AppliedFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("幂等 apply 不应改写 applied 文件")
	}

	// 「本地配置重启后重 merge」：同 revision、同 node 默认值，但 local 改了。
	yaml := "name: node-a\nserver: wss://hub.example\ntoken: node-secret-token\n" +
		"namespaces: [default, team-a]\nvars:\n  SOURCE_ROOT: /local/v2\nlimits:\n  total: 2\n"
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Options{
		Config:   cfg,
		Store:    applied.NewStore(f.dir),
		Cache:    capability.NewCache(filepath.Join(f.dir, "capabilities")),
		Baseline: hostenv.Capture(nil),
		Facts:    protocol.NodeFacts{Hostname: "host-a", OS: "linux", Arch: "amd64", CPU: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.AppliedPresent() {
		t.Fatal("重启应加载到已有 applied")
	}
	rev, hash := restarted.AppliedState()
	if rev != "rev-1" || hash != first.Hash {
		t.Fatalf("重启 AppliedState = %q/%q, want rev-1/%s", rev, hash, first.Hash)
	}
	remerged, err := restarted.ApplyWelcome(welcome)
	if err != nil {
		t.Fatal(err)
	}
	if remerged == nil || remerged.Hash == first.Hash {
		t.Fatalf("本地值变化后应重新 merge 并回报: %+v", remerged)
	}
	loaded, _, err := applied.NewStore(f.dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Vars["SOURCE_ROOT"] != "/local/v2" {
		t.Fatalf("重启后 applied 未采用新 local 值: %v", loaded.Vars)
	}
}

func TestWelcomeAuthorizationIntersection(t *testing.T) {
	f := newFixture(t, "wss://hub.example", "")
	if !f.node.AllowsNamespace("default") || !f.node.AllowsNamespace("team-a") {
		t.Fatal("welcome 前按申报值（授权只能收紧，不能扩张）")
	}
	if _, err := f.node.ApplyWelcome(welcomeFor(t, "rev-1", nil, protocol.Limits{Total: 1}, []string{"team-a", "team-b"})); err != nil {
		t.Fatal(err)
	}
	effective := f.node.EffectiveNamespaces()
	if strings.Join(effective, ",") != "team-a" {
		t.Fatalf("effective = %v", effective)
	}
	if f.node.AllowsNamespace("default") || f.node.AllowsNamespace("team-b") {
		t.Fatal("本地不能扩张到中枢未授权的 ns")
	}
	if !f.node.AllowsNamespace("team-a") {
		t.Fatal("交集内 ns 应允许")
	}
}

func TestApplySaveFailureKeepsState(t *testing.T) {
	f := newFixture(t, "wss://hub.example", "")
	// 用目录占住 applied 路径，让 Save 必然失败。
	if err := os.Mkdir(filepath.Join(f.dir, protocol.AppliedFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.ApplyWelcome(welcomeFor(t, "rev-1", nil, protocol.Limits{Total: 1}, []string{"*"})); err == nil {
		t.Fatal("落盘失败应返回错误")
	}
	if f.node.AppliedPresent() {
		t.Fatal("写盘失败不得切生效值")
	}
	if rev, hash := f.node.AppliedState(); rev != "" || hash != "" {
		t.Fatalf("写盘失败后 AppliedState = %q/%q", rev, hash)
	}
	// 释放路径后可以恢复（不需要重启）。
	if err := os.Remove(filepath.Join(f.dir, protocol.AppliedFileName)); err != nil {
		t.Fatal(err)
	}
	frame, err := f.node.ApplyWelcome(welcomeFor(t, "rev-1", nil, protocol.Limits{Total: 1}, []string{"*"}))
	if err != nil || frame == nil {
		t.Fatalf("恢复后 apply = %v, %v", frame, err)
	}
}

func TestApplyRejectsInvalidNodeDefaults(t *testing.T) {
	f := newFixture(t, "wss://hub.example", "")
	welcome := welcomeFor(t, "rev-1", map[string]string{"lowercase": "x"}, protocol.Limits{Total: 1}, []string{"*"})
	if _, err := f.node.ApplyWelcome(welcome); err == nil {
		t.Fatal("非法 nodeVars 应被拒绝")
	}
	if f.node.AppliedPresent() {
		t.Fatal("非法默认值不应落盘")
	}
}

func TestHelloCapabilitiesProbeUsesStartupPATH(t *testing.T) {
	f := newFixture(t, "wss://hub.example", "")
	tool := filepath.Join(f.binDir, "probe-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata:   artifact.Metadata{Name: "probe", Version: 2},
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Next:   []string{},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\ntrue\n"},
			},
			Requires: artifact.Requires{Commands: []string{"probe-tool", "missing-tool"}, Values: []string{}},
		},
	}
	data, err := artifact.MarshalYAML(a)
	if err != nil {
		t.Fatal(err)
	}
	parsed, digest, err := artifact.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.node.cache.Store(parsed, digest); err != nil {
		t.Fatalf("缓存产物: %v", err)
	}
	hello := f.node.Hello()
	if len(hello.Capabilities) != 1 {
		t.Fatalf("Capabilities = %+v", hello.Capabilities)
	}
	cap := hello.Capabilities[0]
	if cap.Name != "probe" || cap.Version != 2 || cap.Digest != digest {
		t.Fatalf("cap = %+v", cap)
	}
	if len(cap.Commands) != 2 {
		t.Fatalf("commands = %+v", cap.Commands)
	}
	if !cap.Commands[0].Found || cap.Commands[0].Path != tool {
		t.Fatalf("probe-tool = %+v, want path %s", cap.Commands[0], tool)
	}
	if cap.Commands[1].Found || cap.Commands[1].Path != "" {
		t.Fatalf("missing-tool = %+v", cap.Commands[1])
	}
}

func TestDeployThroughNode(t *testing.T) {
	a := &artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata:   artifact.Metadata{Name: "greeter", Version: 3},
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Next:   []string{},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\ntrue\n"},
			},
			Requires: artifact.Requires{Commands: []string{}, Values: []string{}},
		},
	}
	data, err := artifact.MarshalYAML(a)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := artifact.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(protocol.EmitAuthHeader)
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	f := newFixture(t, "ws"+strings.TrimPrefix(srv.URL, "http"), "")
	if _, err := f.node.ApplyWelcome(welcomeFor(t, "rev-1", nil, protocol.Limits{Total: 1}, []string{"*"})); err != nil {
		t.Fatal(err)
	}
	frame := &protocol.CapabilityDeployFrame{
		Type:      protocol.FrameCapabilityDeploy,
		RequestID: "req-9",
		DeployRefs: protocol.DeployRefs{
			Namespace: "default", Name: "greeter", Version: 3, Digest: digest,
		},
	}
	ack := f.node.HandleDeploy(context.Background(), frame)
	if ack.Status != protocol.DeployStatusApplied || ack.Error != "" {
		t.Fatalf("ack = %+v", ack)
	}
	if ack.RequestID != frame.RequestID || ack.DeployRefs != frame.DeployRefs {
		t.Fatalf("ack refs = %+v", ack)
	}
	if gotAuth != "Bearer node-secret-token" {
		t.Fatalf("GET Authorization = %q", gotAuth)
	}
	if !f.node.cache.Has(digest) {
		t.Fatal("部署成功后应已缓存")
	}
}

func TestCorruptAppliedIsNotTrustedOnRestart(t *testing.T) {
	f := newFixture(t, "wss://hub.example", "")
	if _, err := f.node.ApplyWelcome(welcomeFor(t, "rev-1", nil, protocol.Limits{Total: 2}, []string{"*"})); err != nil {
		t.Fatal(err)
	}
	// 手改 applied 的 vars 但不改 hash：Load 必须拒绝，节点不得把被改过的值当生效事实。
	path := filepath.Join(f.dir, protocol.AppliedFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["vars"].(map[string]any)["SOURCE_ROOT"] = "/tmp/tampered"
	tampered, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte("name: node-a\nserver: wss://hub.example\ntoken: node-secret-token\n"))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Options{
		Config:   cfg,
		Store:    applied.NewStore(f.dir),
		Cache:    capability.NewCache(filepath.Join(f.dir, "capabilities")),
		Baseline: hostenv.Capture(nil),
		Facts:    protocol.NodeFacts{Hostname: "host-a", OS: "linux", Arch: "amd64", CPU: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.AppliedPresent() {
		t.Fatal("坏 hash 的 applied 不应被信任")
	}
	if restarted.AppliedError() == nil {
		t.Fatal("应保留加载错误用于诊断")
	}
	hello := restarted.Hello()
	if hello.AppliedRevision != "" || hello.AppliedHash != "" || len(hello.Instances) != 0 {
		t.Fatalf("坏 applied 不应伪造在线事实: %+v", hello)
	}
	// 中枢 welcome 重新对齐后覆盖坏文件。
	if _, err := restarted.ApplyWelcome(welcomeFor(t, "rev-2", nil, protocol.Limits{Total: 1}, []string{"*"})); err != nil {
		t.Fatalf("重新 apply: %v", err)
	}
	loaded, present, err := applied.NewStore(f.dir).Load()
	if err != nil || !present {
		t.Fatalf("重新落盘后 Load = %v, %v", present, err)
	}
	if loaded.Revision != "rev-2" {
		t.Fatalf("revision = %q", loaded.Revision)
	}
}
