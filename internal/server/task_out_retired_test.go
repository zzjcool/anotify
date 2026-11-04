package server

// P1.5 收尾（迁移 28）在 **HTTP 层**的验收：任务对象的 JSON 里不再有 `out`。
//
// 为什么必须单独在 HTTP 层钉一遍：`store.Task` 上的 json tag 删掉了，不代表线上
// 响应就没这个键——handler 完全可能自己拼一个 map / 复用旧结构体 / 给客户端留
// 兼容字段。而前端与外部调用方读的正是**这个字节流**（旧版 deeplink 详情页就是按
// `out != null` 显示「→ 出口」的）。
//
// 断言口径：直接对响应体里的 task 对象做 `!("out" in task)`。这比「out 恒 NULL」
// 更强——键存在与值是否为 null 是两件事，前者会让读者以为它还有意义。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// assertTaskJSONHasNoOut 断言一个已解码的 task 对象里**没有** out 键。
func assertTaskJSONHasNoOut(t *testing.T, what string, task map[string]any) {
	t.Helper()
	if _, ok := task["out"]; ok {
		t.Fatalf("%s 的响应里仍然有 out 键（tasks.out 已在迁移 28 删掉）：%v", what, task)
	}
	// 同理，别的"路由结果"键也不该冒出来（防止有人换个名字把它加回来）
	for _, banned := range []string{"port", "exit", "nextHop"} {
		if _, ok := task[banned]; ok {
			t.Errorf("%s 的响应里有 %q 键：路由结果只存在于 emission，不在任务行上", what, banned)
		}
	}
}

// TestTaskEndpointsExposeNoOut GET /v1/tasks 与 /v1/tasks/{id} 的每个任务对象都没有 out。
func TestTaskEndpointsExposeNoOut(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	capDef := env.seedCapability(t, "review", 1, []string{"fix"})

	const traceID = "trace_no_out_api"
	parent := &store.Task{
		NamespaceID: env.nsID, TraceID: traceID, CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: 1, Params: json.RawMessage(`{"number":1}`),
		CapHash: capDef.ContentHash, GraphID: "grf_no_out",
	}
	if err := env.db.CreateTasks(ctx, []*store.Task{parent}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	// 子任务：详情端点会连整条 chain 一起返回，chain 里的每一行都要干净。
	parentID := parent.ID
	child := &store.Task{
		NamespaceID: env.nsID, TraceID: traceID, ParentTaskID: &parentID,
		CapabilityID: capDef.ID, Capability: capDef.Name, Version: 1,
		Params: json.RawMessage(`{}`), CapHash: capDef.ContentHash, GraphID: "grf_no_out",
	}
	if err := env.db.CreateTasks(ctx, []*store.Task{child}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	// 让父任务走一次真实的结果回报（历史实现的写入点）。HTTP 层不该冒出 out。
	// 中枢任务没有 node 归属，用 CreateTasks 后的 pending→running→终态不方便，
	// 所以这里只关心**读路径**的字段：库里怎么来的不影响 JSON 形状。

	// ① 列表端点
	rr := env.do(t, http.MethodGet, env.withNS("/v1/tasks"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	list, _ := m["tasks"].([]any)
	if len(list) != 2 {
		t.Fatalf("tasks = %d 条, want 2; body=%s", len(list), rr.Body.String())
	}
	for i, raw := range list {
		task, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tasks[%d] 不是对象: %v", i, raw)
		}
		assertTaskJSONHasNoOut(t, "GET /v1/tasks", task)
	}

	// ② 详情端点（task + chain）
	rr = env.do(t, http.MethodGet, env.withNS("/v1/tasks/"+parent.ID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks/{id} status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	m = decode(t, rr)
	single, ok := m["task"].(map[string]any)
	if !ok {
		t.Fatalf("task 不是对象: %v", m["task"])
	}
	assertTaskJSONHasNoOut(t, "GET /v1/tasks/{id}.task", single)
	chain, _ := m["chain"].([]any)
	if len(chain) != 2 {
		t.Fatalf("chain = %d 条, want 2（详情页一次给齐整条链）", len(chain))
	}
	for i, raw := range chain {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("chain[%d] 不是对象: %v", i, raw)
		}
		assertTaskJSONHasNoOut(t, "GET /v1/tasks/{id}.chain", row)
	}
}

// TestTraceEndpointExposesNoOut GET /v1/traces/{id} 的 tasks 里也没有 out。
func TestTraceEndpointExposesNoOut(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	capDef := env.seedCapability(t, "review", 1, []string{"fix"})

	const traceID = "trace_no_out_trace_view"
	if _, err := env.db.InsertEvent(ctx, &store.Event{
		NamespaceID: env.nsID, TraceID: traceID, Source: "github",
		Type: "pull_request.opened", Payload: json.RawMessage(`{"number":1}`),
	}); err != nil {
		t.Fatalf("insert root event: %v", err)
	}
	task := &store.Task{
		NamespaceID: env.nsID, TraceID: traceID, CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: 1, Params: json.RawMessage(`{"number":1}`),
		CapHash: capDef.ContentHash, GraphID: "grf_no_out",
	}
	if err := env.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	rr := env.do(t, http.MethodGet, env.withNS("/v1/traces/"+traceID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/traces/{id} status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	tasks, _ := m["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("trace.tasks = %d 条, want 1; body=%s", len(tasks), rr.Body.String())
	}
	for i, raw := range tasks {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tasks[%d] 不是对象: %v", i, raw)
		}
		assertTaskJSONHasNoOut(t, "GET /v1/traces/{id}.tasks", row)
	}
}
