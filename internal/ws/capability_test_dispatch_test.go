package ws

// capability_test_dispatch_test.go 钉住 #432 的一条硬要求：**试跑建的是真任务、走真派发**。
//
// 这里用「fake 节点」形态：真 store + 真 ws.Hub + 真 WS 帧（脚本化节点客户端），
// 不 mock 会话、不伪造执行事实。链路是：
//
//	db.AcceptTestEvent（HTTP handler 背后的同一条 store 入口）
//	  → 事件行 origin='test' + 任务行 pending
//	  → hub.DispatchOnce（生产调度入口）
//	  → 节点侧收到 task.dispatch（真帧）
//
// 「改回去即红」：把 AcceptTestEvent 换成只写事件不建任务 → 这里读不到任务行 / 收不到
// dispatch 帧；把 origin 写错 → 事件断言红；任务不钉 digest → DispatchOnce 选不到候选。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// TestCapabilityTestEventIsDispatched 断言试跑任务能被真实调度派发到节点。
func TestCapabilityTestEventIsDispatched(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	capability := e.capability(t, "test-run", "", false, nil)
	conn := e.dial(t)
	// 派发候选的硬条件：节点持有并 applied 了这个 digest。
	e.deployApplied(t, capability)

	params := json.RawMessage(`{"diff":"a b"}`)
	task := &store.Task{
		NamespaceID:  e.ns.ID,
		CapabilityID: capability.ID,
		Capability:   capability.Name,
		Version:      capability.Version,
		Digest:       capability.Digest,
		Params:       params,
		Phase:        store.TaskPending,
	}
	receipt, err := e.db.AcceptTestEvent(ctx, store.CapabilityTestInputs{
		NamespaceID: e.ns.ID,
		Capability:  capability.Name,
		Version:     capability.Version,
		Payload:     params,
		Tasks:       []*store.Task{task},
		ActorID:     "usr_fixture",
		Now:         e.now,
	})
	if err != nil {
		t.Fatalf("接收试跑: %v", err)
	}
	if len(receipt.Tasks) != 1 || receipt.Tasks[0].TaskID != task.ID {
		t.Fatalf("回执必须带真任务 id：%+v", receipt.Tasks)
	}
	if receipt.Origin != store.EventOriginTest {
		t.Fatalf("回执 origin = %q, want test", receipt.Origin)
	}

	// 事件行：origin=test、source 是固定文本（不是某个源名）、payload 原样。
	event, err := e.db.GetEvent(ctx, e.ns.ID, receipt.EventID)
	if err != nil {
		t.Fatalf("读试跑事件: %v", err)
	}
	if event.Origin != store.EventOriginTest {
		t.Fatalf("事件 origin = %q, want test", event.Origin)
	}
	if event.Source != store.EventTestSource {
		t.Fatalf("事件 source = %q, want %q", event.Source, store.EventTestSource)
	}
	if string(event.Payload) != string(params) {
		t.Fatalf("事件 payload 应原样保存：%s", event.Payload)
	}

	// 真任务：库里有行、pending、钉住版本与 digest。
	stored, err := e.db.GetTask(ctx, e.ns.ID, task.ID)
	if err != nil {
		t.Fatalf("读回试跑任务: %v", err)
	}
	if stored.Phase != store.TaskPending {
		t.Fatalf("试跑任务 phase = %q, want pending", stored.Phase)
	}
	if stored.CapabilityID != capability.ID || stored.Digest != capability.Digest {
		t.Fatalf("试跑任务没钉住能力身份：%+v", stored)
	}

	// 真派发：生产调度入口扫到这条 pending 任务，并给节点写出 task.dispatch 帧。
	dispatched, err := e.hub.DispatchOnce(ctx, 10)
	if err != nil || dispatched != 1 {
		t.Fatalf("DispatchOnce = %d, %v（试跑任务必须和普通任务一样是可派发的）", dispatched, err)
	}
	frame := e.read(t, conn)
	dispatch, ok := frame.(*protocol.TaskDispatchFrame)
	if !ok {
		t.Fatalf("节点应收到 task.dispatch，得到 %T", frame)
	}
	if dispatch.TaskID != task.ID {
		t.Fatalf("派发的任务 id = %q, want %q", dispatch.TaskID, task.ID)
	}
	if dispatch.Namespace != e.ns.Slug {
		t.Fatalf("派发帧 namespace = %q, want %q", dispatch.Namespace, e.ns.Slug)
	}
	if dispatch.Capability.Name != capability.Name || dispatch.Capability.Version != capability.Version {
		t.Fatalf("派发帧能力身份不对：%+v", dispatch.Capability)
	}
	if string(dispatch.Params) != string(params) {
		t.Fatalf("派发帧 params = %s, want %s", dispatch.Params, params)
	}
	// 派发只记录候选与**拟** attempt（真正的 attempt 计数在 lease.grant 生效时才 +1）。
	if dispatch.ProposedAttempt == 0 {
		t.Fatalf("派发帧必须带拟 attempt（proposedAttempt），得到 %d", dispatch.ProposedAttempt)
	}
}
