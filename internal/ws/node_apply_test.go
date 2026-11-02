package ws

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件钉住「部署结果真的落到了中枢」这条端到端路径。
//
// 曾经的 P0（e2e 抓到，单测没抓到）：`node_session.handleTaskFrame` 没有
// `*ApplyFrame` 分支，节点回的 apply 被当成未知帧 → 中枢回 error{unknown_type}
// 并把结果丢弃。后果不是「少一条日志」，而是**部署失败永远停在 pending**：
// 「这台机器没装 claude」这种最有价值的诊断信息根本到不了控制台，
// 而成功路径只是靠节点重发 hello 对账侥幸变成 applied。
//
// 所以这里的断言必须落在**库里的 state** 上，而不是「回了什么帧」。

// createDeploymentFixture 造一套「能力定义 + 产物 + 部署行」，返回部署 id。
func createDeploymentFixture(t *testing.T, f *nodeFixture) (deployID string, digest string) {
	t.Helper()
	ctx := context.Background()

	art, err := f.db.CreateArtifact(ctx, &store.Artifact{
		NamespaceID:  f.nsID,
		CapabilityID: mustCapabilityID(t, f, "review", 1),
		Digest:       "sha256:" + repeatByte('a', 64),
		Spec:         []byte(`{"apiVersion":"anotify/v1","kind":"Capability"}`),
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	dp, err := f.db.CreateDeployment(ctx, f.nsID, f.node.ID, art.ID)
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if dp.State != store.DeploymentPending {
		t.Fatalf("新建部署 state = %q，期望 pending", dp.State)
	}
	return dp.ID, art.Digest
}

func repeatByte(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

func mustCapabilityID(t *testing.T, f *nodeFixture, name string, version int) string {
	t.Helper()
	c, err := f.db.GetCapability(context.Background(), f.nsID, name, version)
	if err != nil {
		t.Fatalf("get capability %s@%d: %v", name, version, err)
	}
	return c.ID
}

// deploymentState 读库里的权威状态（不是帧、不是内存）。
func deploymentState(t *testing.T, f *nodeFixture, id string) (string, string) {
	t.Helper()
	dp, err := f.db.GetDeployment(context.Background(), f.nsID, id)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return dp.State, dp.Reason
}

// TestApplyFrameLandsInStore 是 P0 的回归测试：apply 帧必须写进 deployments.state。
func TestApplyFrameLandsInStore(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	deployID, digest := createDeploymentFixture(t, f)

	send(t, conn, &ApplyFrame{
		Type:       NodeFrameApply,
		DeployID:   deployID,
		Capability: "review",
		Version:    1,
		Digest:     digest,
		State:      ApplyStateApplied,
	})

	// 帧处理是异步的：轮询库直到状态变化（不用固定 sleep）。
	waitForDeploymentState(t, f, deployID, store.DeploymentApplied)
}

// TestApplyFrameFailureIsVisible 部署失败必须能被看见 —— 这是 D5/产品门面
// 「这台机器缺 claude」的唯一通道。
func TestApplyFrameFailureIsVisible(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	deployID, digest := createDeploymentFixture(t, f)

	send(t, conn, &ApplyFrame{
		Type:     NodeFrameApply,
		DeployID: deployID,
		Digest:   digest,
		State:    ApplyStateFailed,
		Reason:   ApplyReasonMissingRequirement + ":claude",
	})

	state, reason := waitForDeploymentState(t, f, deployID, store.DeploymentFailed)
	if reason == "" {
		t.Fatal("failed 的部署必须带 reason（否则用户只知道失败，不知道该装什么）")
	}
	if state != store.DeploymentFailed {
		t.Fatalf("state = %q", state)
	}
}

// TestApplyFrameRejectsBadInput 协议层拒绝非法 apply，而不是静默成功。
func TestApplyFrameRejectsBadInput(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	// 缺 deploy_id
	send(t, conn, &ApplyFrame{Type: NodeFrameApply, State: ApplyStateApplied})
	frame := readErrorFrame(t, conn)
	if frame["code"] != ErrCodeBadFrame {
		t.Fatalf("缺 deploy_id 的 code = %v，期望 %q", frame["code"], ErrCodeBadFrame)
	}

	// 非法 state
	send(t, conn, &ApplyFrame{Type: NodeFrameApply, DeployID: "dp_x", State: "weird"})
	frame = readErrorFrame(t, conn)
	if frame["code"] != ErrCodeBadFrame {
		t.Fatalf("非法 state 的 code = %v，期望 %q", frame["code"], ErrCodeBadFrame)
	}
}

// TestApplyFrameUnknownDeploymentIgnored 撤销与回报天然会撞车：
// 行已经不在了不算协议错误（节点只是晚了一步），但要留日志、别 500。
func TestApplyFrameUnknownDeploymentIgnored(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	send(t, conn, &ApplyFrame{
		Type:     NodeFrameApply,
		DeployID: "dp_does_not_exist",
		State:    ApplyStateApplied,
	})

	// 连接必须还活着：紧接着发一个心跳，不报错。
	send(t, conn, &HeartbeatFrame{Type: NodeFrameHeartbeat})
	// 再发一个正常部署，确认会话没被打断。
	deployID, digest := createDeploymentFixture(t, f)
	send(t, conn, &ApplyFrame{
		Type: NodeFrameApply, DeployID: deployID, Digest: digest, State: ApplyStateApplied,
	})
	waitForDeploymentState(t, f, deployID, store.DeploymentApplied)
}

// waitForDeploymentState 轮询到期望状态，返回最终 (state, reason)。
//
// 为什么轮询而不是等信号：apply 帧在 readLoop 的 goroutine 里处理，
// 测试 goroutine 与它之间没有同步点；固定 sleep 要么慢要么 flaky。
func waitForDeploymentState(t *testing.T, f *nodeFixture, id, want string) (string, string) {
	t.Helper()
	const attempts = 200 // 200 × 5ms = 1s 上限
	for i := 0; i < attempts; i++ {
		state, reason := deploymentState(t, f, id)
		if state == want {
			return state, reason
		}
		time.Sleep(5 * time.Millisecond)
	}
	state, reason := deploymentState(t, f, id)
	t.Fatalf("deployment %s state = %q（reason=%q），期望 %q：apply 帧没有被落库",
		id, state, reason, want)
	return "", ""
}

// TestApplyFrameJSONShape 协议形状（tag 是协议的一部分，不能随手改）。
func TestApplyFrameJSONShape(t *testing.T) {
	b, err := Encode(&ApplyFrame{
		Type: NodeFrameApply, DeployID: "dp_1", Capability: "review", Version: 2,
		Digest: "sha256:aa", State: ApplyStateFailed, Reason: "missing_requirement:jq",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"type", "deploy_id", "capability", "version", "digest", "state", "reason"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("apply 帧缺字段 %q：%s", key, b)
		}
	}
	if m["type"] != NodeFrameApply {
		t.Fatalf("type = %v", m["type"])
	}
}
