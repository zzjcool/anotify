package ws

// #162 命名空间隔离在**节点协议**这一侧的验收测试。
//
// 背景：store 层把命名空间资源的作用域参数从 orgID 换成了 namespaceID。
// 两者都是 string，所以 `Store.Xxx(ctx, s.node.OrgID, …)` 类型上照旧编译通过，
// 但语义全错——`WHERE namespace_id = 'org_xxx'` 恒 0 行，线上表现为：
// 心跳续不上租约、ack/progress/result 全部「unknown task」、握手写不进实例。
//
// 本文件钉住两条最容易静默失效的性质：
//  1. 任务帧的作用域是**节点的命名空间**：节点拿不到别的命名空间里的任务（哪怕 taskID 正确）；
//  2. hub 的派发路由按**命名空间**比较（不是 org）：同 org 不同命名空间一律不推。
//
// 为什么要用「同一个 org 里的两个命名空间」而不是两个 org：迁移 12 之前隔离维度
// 就是 org_id，用两个 org 去验，即使实现完全按 org 过滤也会通过。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// siblingNamespace 在夹具的组织下再建一个命名空间（同 org，第二套流程）。
func (f *nodeFixture) siblingNamespace(t *testing.T, slug string) *store.Namespace {
	t.Helper()
	ns, err := f.db.CreateNamespace(context.Background(), f.orgID, slug, slug)
	if err != nil {
		t.Fatalf("create namespace %q: %v", slug, err)
	}
	return ns
}

// TestCrossNamespaceTaskNotVisible 节点属于 ns_a 时，对 ns_b 的 taskID 做 ack/result
// → 任务**找不到**（store.ErrNotFound）→ 回 bad_frame "unknown task"。
//
// 判别点：错误码必须是 bad_frame（查不到任务），而不是 invalid_transition
// （任务存在但没派给本节点）。如果作用域传错成 org 或写死默认命名空间，
// GetTask 会命中 ns_b 的那一行，错误码就变成 invalid_transition——测试即失败。
func TestCrossNamespaceTaskNotVisible(t *testing.T) {
	ctx := context.Background()
	f := newNodeFixture(t) // 节点属于 organ 的 default 命名空间（f.nsID）
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	// 同一个组织里的第二套流程
	nsOther := f.siblingNamespace(t, "staging")
	if nsOther.ID == f.nsID {
		t.Fatal("sibling namespace reused the default namespace id")
	}

	// 隔离之后 review@1 在两个命名空间里是**两个不同定义**，所以各建一份。
	capOther := &store.CapabilityVersion{
		NamespaceID: nsOther.ID, Name: "review", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"fix"}, Source: store.CapSourceBuiltin,
	}
	if err := f.db.CreateCapability(ctx, capOther); err != nil {
		t.Fatalf("create capability in %s: %v", nsOther.ID, err)
	}
	if capOther.ID == "" || capOther.NamespaceID != nsOther.ID {
		t.Fatalf("capability namespace = %q, want %q", capOther.NamespaceID, nsOther.ID)
	}

	// ns_b 里的一条任务（未派给任何人：节点就算猜到 taskID 也不该看见它）
	task := &store.Task{
		NamespaceID: nsOther.ID, TraceID: "trace_other_ns",
		CapabilityID: capOther.ID, Capability: capOther.Name, Version: 1,
		Params: json.RawMessage(`{}`), CapHash: capOther.ContentHash, GraphID: "grf_x",
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task in %s: %v", nsOther.ID, err)
	}

	// store 层面的两端对照：行确实存在（ns_b 查得到），但节点所在命名空间查不到。
	if _, err := f.db.GetTask(ctx, nsOther.ID, task.ID); err != nil {
		t.Fatalf("task missing in its own namespace %s: %v", nsOther.ID, err)
	}
	if _, err := f.db.GetTask(ctx, f.nsID, task.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTask in the node's namespace = %v, want store.ErrNotFound "+
			"(跨命名空间拿到了一条属于 %s 的任务)", err, nsOther.ID)
	}

	// ack / result 两条路径都必须按节点的命名空间查任务。
	cases := []struct {
		name  string
		frame any
	}{
		{"ack", &AckFrame{Type: NodeFrameAck, TaskID: task.ID}},
		{"result", &ResultFrame{Type: NodeFrameResult, TaskID: task.ID,
			Status: store.TaskSucceeded, Summary: "ok", Out: strPtr("fix")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			send(t, conn, c.frame)
			frame := readUntil(t, conn, NodeFrameError)
			// bad_frame = 「本命名空间里没有这个任务」；invalid_transition =
			// 「任务存在但不是你的」——后者说明作用域泄漏了。
			if frame["code"] != ErrCodeBadFrame {
				t.Fatalf("code = %v, want %q (任务在别的命名空间，必须表现为「找不到」而不是「不是你的」)",
					frame["code"], ErrCodeBadFrame)
			}
			msg, _ := frame["message"].(string)
			if !strings.Contains(msg, "unknown task") {
				t.Fatalf("message = %q, want it to mention %q", msg, "unknown task")
			}
		})
	}

	// 别处的任务一个字节都没被改动（既没 ack 也没 result）。
	got, err := f.db.GetTask(ctx, nsOther.ID, task.ID)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if got.Status != store.TaskPending {
		t.Fatalf("task in %s status = %q after cross-namespace ack/result, want %q",
			nsOther.ID, got.Status, store.TaskPending)
	}
	if got.Reported || got.Out != nil {
		t.Fatalf("cross-namespace result leaked into the row: reported=%v out=%v", got.Reported, got.Out)
	}
}

// TestPublishAssignRoutesByNamespace hub 的派发路由按**命名空间**比较（不是 org）：
// 同命名空间推得出去，同 org 的另一个命名空间一律拒绝。
//
// 为什么按命名空间：一个连接 = 一个节点 = 一个命名空间（令牌绑命名空间），
// 所以「任务与节点同命名空间」是唯一正确的约束——同 org 的两个命名空间之间
// 派发会把一台机器的执行能力泄漏到另一个隔离域。
func TestPublishAssignRoutesByNamespace(t *testing.T) {
	ctx := context.Background()
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_hub_route")

	// 正向：同命名空间 → 推得出去，节点收到 assign 帧（证明比较字段没写死成别的东西）。
	if err := f.handler.hub().PublishAssign(ctx, f.nsID, f.node.ID, task); err != nil {
		t.Fatalf("PublishAssign in the node's own namespace: %v", err)
	}
	frame := readUntil(t, conn, NodeFrameAssign)
	if frame["task_id"] != task.ID {
		t.Fatalf("assign frame task_id = %v, want %q", frame["task_id"], task.ID)
	}

	// 反向：同 org 的另一个命名空间 → 拒绝，且节点收不到任何帧。
	nsOther := f.siblingNamespace(t, "staging")
	err := f.handler.hub().PublishAssign(ctx, nsOther.ID, f.node.ID, task)
	if err == nil {
		t.Fatalf("PublishAssign accepted a task from namespace %s on a node in %s", nsOther.ID, f.nsID)
	}
	if !strings.Contains(err.Error(), "another namespace") {
		t.Fatalf("error = %v, want it to say the node belongs to another namespace", err)
	}
	// 读超时表达「没有帧」。放在最后：读超时会关掉这条客户端连接。
	if m, ok := tryReadFrame(conn, 300*time.Millisecond); ok {
		t.Fatalf("node received a frame for a cross-namespace publish: %v", m)
	}
}
