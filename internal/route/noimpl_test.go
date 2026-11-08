package route

// 本文件是 P2b「中枢认领改由 spec.handler 决定」的派发侧验收
// （Capability Spec v3 §3.1 / §11.3，契约 R1/R2）。
//
// 两件事必须同时成立，否则「名字表死了、判据只剩一处」只是口头承诺：
//
//  1. **可执行 handler**（notify-phone）→ 中枢执行，**不找节点**。
//     判据是目标能力自己的 spec.handler —— 一个用户自定义名字、从没进过
//     BuiltinCapabilities 的能力，也必须被中枢认领（旧名字表会把它剪成 no_node）。
//  2. **占位 handler**（noop）→ 剪枝 pruned/no_impl，**不建任务**。
//     占位能力"能跑但什么都不做"，路由到它等于静默丢掉这条链；留痕比成功重要。

import (
	"context"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

// TestDispatchNoopPrunedNoImpl 一条边指向 noop 能力 → 不建任务、写一行 pruned.no_impl。
//
// 关键细节：这个占位能力**没有任何节点声明**。若判据还停在「找节点」，
// 症状会是 pruned/no_node（"装个节点就好了"），而真实病因是「它根本没有实现」。
// 两种病因的修法完全不同，所以 reason 必须能分开。
func TestDispatchNoopPrunedNoImpl(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"done"}, `{"type":"object"}`)
	placeholder := f.addCap(testNS, "placeholder", 1, nil, `{"type":"object"}`)
	placeholder.Spec.Handler = artifact.HandlerNoop
	f.addEdge(testNS, review.ID, "done", placeholder.ID)
	d := New(f)

	const traceID = "trace_no_impl"
	out, err := d.Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: traceID,
		FromCap: review.ID, Out: "done", Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// ① 不建任务：造不出实现的边不能变成"一条绿任务"。
	if len(out.Tasks) != 0 {
		t.Fatalf("建出了 %d 个任务, want 0（noop 不可执行，不能默默成功）", len(out.Tasks))
	}
	if len(f.createdTasks) != 0 {
		t.Fatalf("CreateTasks 收到 %d 行, want 0", len(f.createdTasks))
	}

	// ② 返回值里看得到理由（HTTP 响应当场就要能回答"为什么没跑"）
	if len(out.Pruned) != 1 {
		t.Fatalf("out.Pruned = %+v, want 恰好一条", out.Pruned)
	}
	if out.Pruned[0].Reason != ReasonNoImpl {
		t.Fatalf("reason = %q, want %q（不是 no_node：病因是「没有实现」，不是「没装节点」）",
			out.Pruned[0].Reason, ReasonNoImpl)
	}
	if out.Pruned[0].CapabilityID != placeholder.ID {
		t.Errorf("capabilityId = %q, want %q", out.Pruned[0].CapabilityID, placeholder.ID)
	}

	// ③ 留痕：events 里一行 type=pruned.no_impl。
	//
	// type 断**字面量**（不是 PruneEventType(ReasonNoImpl) 的回算）：trace 视图与
	// 前端按这个字符串筛剪枝行，常量改值改是本条测试唯一能拦住的地方。
	events := pruneEventsFor(f, traceID)
	if len(events) != 1 {
		t.Fatalf("剪枝事件 %d 行, want 恰好 1 行", len(events))
	}
	e := events[0]
	if got := string(ReasonNoImpl); got != "no_impl" {
		t.Fatalf("ReasonNoImpl wire 值 = %q, want %q", got, "no_impl")
	}
	if e.Type != "pruned.no_impl" {
		t.Errorf("events.type = %q, want %q", e.Type, "pruned.no_impl")
	}
	if e.Source != PruneEventSource {
		t.Errorf("events.source = %q, want %q", e.Source, PruneEventSource)
	}
	p := decodePrunePayload(t, e)
	if p.TargetCapability != placeholder.ID {
		t.Errorf("payload.targetCapability = %q, want %q", p.TargetCapability, placeholder.ID)
	}
	if p.Port != "done" {
		t.Errorf("payload.port = %q, want done", p.Port)
	}
}

// TestDispatchExecutableHandlerWithoutNodeIsCentral 可执行 handler → 中枢执行，**不查节点**。
//
// 「名字表真的死了」的派发侧证明：这个能力叫 phone.custom，**不在** BuiltinCapabilities
// 里，也没有任何节点声明它。旧实现（`IsBuiltinCapability(...).Central`）会把这条边剪成
// no_node，推送永远发不出去；新判据只看 spec.handler。
func TestDispatchExecutableHandlerWithoutNodeIsCentral(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"done"}, `{"type":"object"}`)
	custom := f.addCap(testNS, "phone.custom", 1, nil, `{"type":"object"}`)
	custom.Spec.Handler = artifact.HandlerNotifyPhone
	f.addEdge(testNS, review.ID, "done", custom.ID)
	d := New(f)

	out, err := d.Dispatch(ctx, DispatchInput{
		NamespaceID: testNS, TraceID: "trace_custom_central",
		FromCap: review.ID, Out: "done", Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("out.Pruned = %+v, want 空（机器没有节点声明，但中枢会执行它）", out.Pruned)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("建出 %d 个任务, want 1", len(out.Tasks))
	}
	task := out.Tasks[0]
	// 中枢任务的形状：pending + 无归属（由 internal/executor 认领并推进状态机）。
	if task.Status != store.TaskPending || task.NodeID != nil {
		t.Fatalf("task = {status:%s nodeID:%v}, want {pending, nil}（中枢执行不挑机器）", task.Status, task.NodeID)
	}
	if task.CapabilityID != custom.ID {
		t.Errorf("task.capabilityId = %q, want %q", task.CapabilityID, custom.ID)
	}
}
