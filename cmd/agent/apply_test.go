package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/ws"
)

// 本文件覆盖**部署应用**（apply.go）：deploy 帧 → 校验 / 探依赖 / 落盘 → apply 帧
// → 重发 hello。
//
// 三条硬约束各有一组用例：
//   - 部署失败走 apply.reason（不是 error 帧）；
//   - 成功之后一定要重发 hello（中枢靠它对账 node_capabilities）；
//   - 撤销只摘索引、且幂等。

// ---------- 夹具 ----------

// deployConn 是 apply 用例的最小连接表面：记录上行帧，读一直阻塞到 ctx 取消。
type deployConn struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (c *deployConn) read(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *deployConn) write(_ context.Context, data []byte) error {
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		return err
	}
	c.mu.Lock()
	c.sent = append(c.sent, frame)
	c.mu.Unlock()
	return nil
}

func (c *deployConn) close() error { return nil }

func (c *deployConn) frames() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, len(c.sent))
	copy(out, c.sent)
	return out
}

// only 取唯一一帧某类型的帧（多于/少于一条都 Fatal）。
func (c *deployConn) only(t *testing.T, typ string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, f := range c.frames() {
		if f["type"] == typ {
			found = append(found, f)
		}
	}
	if len(found) != 1 {
		t.Fatalf("期望恰好 1 帧 %q，实际 %d 帧（全部帧: %v）", typ, len(found), c.describe())
	}
	return found[0]
}

func (c *deployConn) count(typ string) int {
	n := 0
	for _, f := range c.frames() {
		if f["type"] == typ {
			n++
		}
	}
	return n
}

func (c *deployConn) describe() string {
	parts := make([]string, 0, len(c.sent))
	for _, f := range c.frames() {
		part, _ := f["type"].(string)
		if state, ok := f["state"].(string); ok {
			part += "=" + state
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

// newDeployAgent 造一个只用来处理 deploy 帧的 agent（数据目录是临时目录）。
func newDeployAgent(t *testing.T) (*nodeAgent, *deployConn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	conn := &deployConn{}
	cfg := &nodeConfig{Name: "n", Server: "http://localhost:8080", Token: "tok", Instances: 1}
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return newNodeAgent(ctx, cfg, conn, t.TempDir(), log), conn
}

// deployFrameFor 把产物包成一帧 deploy（Artifact 用 canonical JSON，与中枢一致）。
func deployFrameFor(t *testing.T, id string, action string, a *artifact.Artifact) *deployFrame {
	t.Helper()
	f := &deployFrame{Type: frameTypeDeploy, DeployID: id, Action: action}
	if a != nil {
		raw, err := artifact.CanonicalJSON(a)
		if err != nil {
			t.Fatalf("artifact.CanonicalJSON: %v", err)
		}
		f.Artifact = raw
	}
	return f
}

// ---------- apply 成功 ----------

// TestHandleDeployApply：合法产物 → 回 applied、进缓存、applied 列表里出现、重发 hello。
func TestHandleDeployApply(t *testing.T) {
	a, conn := newDeployAgent(t)
	art := testScriptArtifact("demo.prep", 3, "#!/bin/bash\necho prep\n")
	digest := mustDigest(t, art)

	if err := a.handleDeploy(context.Background(), deployFrameFor(t, "dep_1", ws.DeployActionApply, art)); err != nil {
		t.Fatalf("handleDeploy: %v", err)
	}

	apply := conn.only(t, frameTypeApply)
	if apply["state"] != ws.ApplyStateApplied {
		t.Fatalf("apply.state = %v, want %q（reason=%v）", apply["state"], ws.ApplyStateApplied, apply["reason"])
	}
	if apply["deploy_id"] != "dep_1" {
		t.Errorf("apply.deploy_id = %v, want dep_1", apply["deploy_id"])
	}
	if apply["capability"] != "demo.prep" || apply["version"] != float64(3) {
		t.Errorf("apply 的 capability/version = %v@%v, want demo.prep@3", apply["capability"], apply["version"])
	}
	if apply["digest"] != digest {
		t.Errorf("apply.digest = %v, want %v", apply["digest"], digest)
	}
	if _, present := apply["reason"]; present {
		t.Errorf("成功的 apply 不该带 reason，实际 %v", apply["reason"])
	}

	// 缓存里真有它。
	cached, gotDigest, ok := a.cache.get("demo.prep", 3)
	if !ok || gotDigest != digest {
		t.Fatalf("cache.get = (%v, %q, %v), want 命中 %q", cached != nil, gotDigest, ok, digest)
	}

	// 重发 hello（中枢靠 applied 对账），且 applied 里就是这一条。
	hello := conn.only(t, frameTypeHello)
	applied, ok := hello["applied"].([]any)
	if !ok || len(applied) != 1 {
		t.Fatalf("重发的 hello.applied = %#v, want 1 条", hello["applied"])
	}
	entry, _ := applied[0].(map[string]any)
	if entry["capability"] != "demo.prep" || entry["version"] != float64(3) || entry["digest"] != digest {
		t.Errorf("hello.applied[0] = %#v, want {demo.prep 3 %s}", entry, digest)
	}

	// 先 apply 后 hello：中枢收到 applied 之前不该先看到对账帧（顺序换了会让
	// 控制台短暂显示「已缓存但还没应用」这种不存在的状态）。
	frames := conn.frames()
	if len(frames) != 2 || frames[0]["type"] != frameTypeApply || frames[1]["type"] != frameTypeHello {
		t.Errorf("帧顺序 = %v, want [apply hello]", conn.describe())
	}
}

// ---------- 依赖探测 ----------

// TestHandleDeployMissingRequirement：产物声明的命令这台机器没有 → failed +
// missing_requirement，且**不落盘**、不重发 hello。
func TestHandleDeployMissingRequirement(t *testing.T) {
	a, conn := newDeployAgent(t)
	art := testScriptArtifact("agent.claude", 1, "#!/bin/bash\nclaude --print hi\n")
	art.Spec.Requires = []string{"anotify-no-such-cmd-alpha", "anotify-no-such-cmd-beta"}

	if err := a.handleDeploy(context.Background(), deployFrameFor(t, "dep_missing", ws.DeployActionApply, art)); err != nil {
		t.Fatalf("handleDeploy 不该把部署失败当传输错误: %v", err)
	}

	apply := conn.only(t, frameTypeApply)
	if apply["state"] != ws.ApplyStateFailed {
		t.Fatalf("apply.state = %v, want failed", apply["state"])
	}
	reason, _ := apply["reason"].(string)
	if !strings.Contains(reason, "missing_requirement") {
		t.Fatalf("apply.reason = %q, want 含 missing_requirement", reason)
	}
	// 多个缺失用 ";" 连起来，一次说全。
	if !strings.Contains(reason, "missing_requirement:anotify-no-such-cmd-alpha") ||
		!strings.Contains(reason, "missing_requirement:anotify-no-such-cmd-beta") {
		t.Errorf("apply.reason = %q, want 两个缺失命令都列出来（; 分隔）", reason)
	}
	if _, _, ok := a.cache.get("agent.claude", 1); ok {
		t.Error("缺依赖的产物被落盘了：接下来派单会在运行时炸，而不是在部署时拦住")
	}
	if got := conn.count(frameTypeHello); got != 0 {
		t.Errorf("失败时不该重发 hello（缓存没变），实际发了 %d 次", got)
	}
}

// TestHandleDeployRequirementPresent：依赖真的装了 → applied（探的是真命令）。
func TestHandleDeployRequirementPresent(t *testing.T) {
	a, conn := newDeployAgent(t)
	art := testScriptArtifact("demo.sh", 1, "#!/bin/bash\nsh -c true\n")
	art.Spec.Requires = []string{"sh"}

	if err := a.handleDeploy(context.Background(), deployFrameFor(t, "dep_sh", ws.DeployActionApply, art)); err != nil {
		t.Fatalf("handleDeploy: %v", err)
	}
	apply := conn.only(t, frameTypeApply)
	if apply["state"] != ws.ApplyStateApplied {
		t.Fatalf("apply.state = %v, want applied（reason=%v）", apply["state"], apply["reason"])
	}
}

// ---------- 非法产物 ----------

// TestHandleDeployInvalidArtifact：产物不合法 → invalid_artifact + 具体原因，不落盘。
func TestHandleDeployInvalidArtifact(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		expect string // reason 里必须出现的短码
	}{
		{
			name:   "空 artifact",
			raw:    ``,
			expect: "invalid_artifact",
		},
		{
			name:   "不是合法 JSON/YAML",
			raw:    `{{{`,
			expect: "invalid_artifact",
		},
		{
			name:   "缺 apiVersion",
			raw:    `{"kind":"Capability","metadata":{"name":"x","version":1},"spec":{"run":{"entrypoint":"run.sh","files":{"run.sh":"#!/bin/bash\n"}}}}`,
			expect: "unsupported_api_version",
		},
		{
			name:   "入口没有 shebang",
			raw:    `{"apiVersion":"anotify/v1","kind":"Capability","metadata":{"name":"x","version":1},"spec":{"run":{"entrypoint":"run.sh","files":{"run.sh":"echo hi\n"}}}}`,
			expect: "entrypoint_no_shebang",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, conn := newDeployAgent(t)
			f := &deployFrame{Type: frameTypeDeploy, DeployID: "dep_bad", Action: ws.DeployActionApply,
				Artifact: json.RawMessage(tc.raw)}

			if err := a.handleDeploy(context.Background(), f); err != nil {
				t.Fatalf("非法产物是业务结果，不该返回 error: %v", err)
			}
			apply := conn.only(t, frameTypeApply)
			if apply["state"] != ws.ApplyStateFailed {
				t.Fatalf("apply.state = %v, want failed", apply["state"])
			}
			reason, _ := apply["reason"].(string)
			if !strings.HasPrefix(reason, ws.ApplyReasonInvalidArtifact) {
				t.Errorf("apply.reason = %q, want 以 invalid_artifact 开头（机器可读短码）", reason)
			}
			if !strings.Contains(reason, tc.expect) {
				t.Errorf("apply.reason = %q, want 含 %q（具体哪条校验没过）", reason, tc.expect)
			}
			if got := len(a.cache.applied()); got != 0 {
				t.Errorf("非法产物进了缓存（%d 条）", got)
			}
		})
	}
}

// ---------- remove ----------

// TestHandleDeployRemove：撤销 → 索引消失、回 applied、重发 hello（applied 为空）。
func TestHandleDeployRemove(t *testing.T) {
	a, conn := newDeployAgent(t)
	art := testScriptArtifact("demo.prep", 1, "#!/bin/bash\necho hi\n")
	digest := mustDigest(t, art)
	if err := a.handleDeploy(context.Background(), deployFrameFor(t, "dep_apply", ws.DeployActionApply, art)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	before := conn.count(frameTypeHello)

	if err := a.handleDeploy(context.Background(), deployFrameFor(t, "dep_remove", ws.DeployActionRemove, art)); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// 最后一帧是 remove 的 apply，倒数第二是重发的 hello。
	frames := conn.frames()
	apply := frames[len(frames)-2]
	if apply["type"] != frameTypeApply || apply["deploy_id"] != "dep_remove" {
		t.Fatalf("倒数第二帧 = %v, want dep_remove 的 apply", apply)
	}
	if apply["state"] != ws.ApplyStateApplied {
		t.Fatalf("remove 的 apply.state = %v, want applied（撤销成功也是 applied）", apply["state"])
	}
	if apply["digest"] != digest {
		t.Errorf("remove 的 apply.digest = %v, want 被撤掉的那份 %v", apply["digest"], digest)
	}

	hello := frames[len(frames)-1]
	if hello["type"] != frameTypeHello {
		t.Fatalf("最后一帧 = %v, want hello（撤销之后必须重发 hello）", hello["type"])
	}
	applied, ok := hello["applied"].([]any)
	if !ok || len(applied) != 0 {
		t.Errorf("撤销后的 hello.applied = %#v, want 空", hello["applied"])
	}
	if conn.count(frameTypeHello) != before+1 {
		t.Errorf("hello 帧数 = %d, want %d", conn.count(frameTypeHello), before+1)
	}

	// 索引没了、产物目录还在。
	if _, _, ok := a.cache.get("demo.prep", 1); ok {
		t.Error("撤销之后缓存里仍然能取到它")
	}
	// 再撤一次也成功（幂等：中枢重推不该变成 write_failed）。
	if err := a.handleDeploy(context.Background(), deployFrameFor(t, "dep_remove2", ws.DeployActionRemove, art)); err != nil {
		t.Fatalf("重复 remove: %v", err)
	}
	if f := conn.frames()[len(conn.frames())-2]; f["state"] != ws.ApplyStateApplied {
		t.Errorf("重复 remove 的 apply.state = %v, want applied", f["state"])
	}
}

// TestHandleDeployRemoveWithoutArtifact：remove 帧没带 artifact 就无法定位能力。
//
// 这是 DeployFrame（协议已冻结）没有 capability/version 字段的直接后果：中枢必须
// 把产物一起带下来。没有它只能回 invalid_artifact——猜一个名字去删是更坏的选择。
func TestHandleDeployRemoveWithoutArtifact(t *testing.T) {
	a, conn := newDeployAgent(t)
	f := &deployFrame{Type: frameTypeDeploy, DeployID: "dep_rm", Action: ws.DeployActionRemove}

	if err := a.handleDeploy(context.Background(), f); err != nil {
		t.Fatalf("handleDeploy: %v", err)
	}
	apply := conn.only(t, frameTypeApply)
	if apply["state"] != ws.ApplyStateFailed {
		t.Fatalf("apply.state = %v, want failed", apply["state"])
	}
	reason, _ := apply["reason"].(string)
	if !strings.HasPrefix(reason, ws.ApplyReasonInvalidArtifact) {
		t.Errorf("apply.reason = %q, want 以 invalid_artifact 开头", reason)
	}
}

// ---------- 未知 action / 连接收尾 ----------

// TestHandleDeployUnsupportedAction：未知 action 明确拒绝，绝不猜。
func TestHandleDeployUnsupportedAction(t *testing.T) {
	a, conn := newDeployAgent(t)
	art := testScriptArtifact("demo.prep", 1, "#!/bin/bash\necho hi\n")
	f := deployFrameFor(t, "dep_x", "explode", art)

	if err := a.handleDeploy(context.Background(), f); err != nil {
		t.Fatalf("handleDeploy: %v", err)
	}
	apply := conn.only(t, frameTypeApply)
	if apply["state"] != ws.ApplyStateFailed {
		t.Fatalf("apply.state = %v, want failed", apply["state"])
	}
	if reason, _ := apply["reason"].(string); !strings.HasPrefix(reason, ws.ApplyReasonUnsupportedAction) {
		t.Errorf("apply.reason = %q, want 以 unsupported_action 开头", reason)
	}
	if _, _, ok := a.cache.get("demo.prep", 1); ok {
		t.Error("未知 action 竟然落了盘")
	}
}

// TestHandleDeployDrainingCtx：连接收尾（ctx 取消）时不再改本地缓存、不发帧。
func TestHandleDeployDrainingCtx(t *testing.T) {
	a, conn := newDeployAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	art := testScriptArtifact("demo.prep", 1, "#!/bin/bash\necho hi\n")
	if err := a.handleDeploy(ctx, deployFrameFor(t, "dep_late", ws.DeployActionApply, art)); err == nil {
		t.Fatal("ctx 已取消时 handleDeploy 应返回错误（这一帧等重连后重推）")
	}
	if len(conn.frames()) != 0 {
		t.Errorf("收尾期不该发帧，实际 %v", conn.describe())
	}
	if len(a.cache.applied()) != 0 {
		t.Error("收尾期不该改本地缓存")
	}
}
