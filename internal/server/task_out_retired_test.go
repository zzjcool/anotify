package server

// P1.5 收尾（迁移 28）在 **HTTP 层**的验收：任务对象的 JSON 里不再有 `out`。
//
// 为什么必须单独在 HTTP 层钉一遍：`store.Task` 上的 json tag 删掉了，不代表线上
// 响应就没这个键——handler 完全可能自己拼一个 map / 复用旧结构体 / 给客户端留
// 兼容字段。而前端与外部调用方读的正是**这个字节流**（旧版 deeplink 详情页就是按
// `out != null` 显示「→ 出口」的）。
//
// 断言口径：对响应体里的**每一层**对象做 `!("out" in obj)`（递归遍历，不是只看顶层）。
// 这比「out 恒 NULL」更强——键存在与值是否为 null 是两件事，前者会让读者以为它还有
// 意义；也比「只看顶层」更强——v4 的 Task 正文在 spec / status 里，路由结果若被
// 塞进任何一层都该判红。
//
// v4 装配口径：路径是 `/v1/namespaces/{ns}/tasks…`（v3 的扁平 `/v1/tasks` 已具名退役，
// D0.1）。v3 详情端点的 `{task, chain}` 组合在 v4 拆成两处：单任务信封
// （`GET …/tasks/{id}`）与整条 trace 读模型（`GET …/traces/{traceId}` 的 `tasks[]`），
// 两处都在下面逐一钉住。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// retiredRouteResultKeys 是「路由结果」类键名：它们只存在于 emission，不该出现在
// 任务行上（out 是已删的旧列，其余是防止有人换个名字把它加回来）。
var retiredRouteResultKeys = []string{"out", "port", "exit", "nextHop"}

// assertTaskJSONHasNoOut 断言一个已解码的 JSON 值里**任何一层**都没有路由结果键。
func assertTaskJSONHasNoOut(t *testing.T, what string, v any) {
	t.Helper()
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch n := node.(type) {
		case map[string]any:
			for _, banned := range retiredRouteResultKeys {
				if _, ok := n[banned]; ok {
					t.Fatalf("%s%s 的响应里仍然有 %q 键：路由结果只存在于 emission，不在任务行上（node=%v）",
						what, path, banned, n)
				}
			}
			for k, child := range n {
				walk(path+"."+k, child)
			}
		case []any:
			for i, child := range n {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("", v)
}

// seedTaskOutTrace 建一条 trace 的父/子任务（子任务的父引用让「链」这条语义真实存在）。
func seedTaskOutTrace(t *testing.T, env *taskOutEnv, traceID string) *store.Task {
	t.Helper()
	ctx := context.Background()
	capDef := env.capability

	// v4 的已解析任务必须钉住真实 version/digest（未解析形状只允许由失败事实入口创建），
	// 所以这里用夹具建出的真能力行。
	parent := &store.Task{
		NamespaceID: env.nsID, TraceID: traceID, CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: capDef.Version, Digest: capDef.Digest,
		Params: json.RawMessage(`{"number":1}`),
	}
	if err := env.db.CreateTasks(ctx, []*store.Task{parent}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	parentID := parent.ID
	child := &store.Task{
		NamespaceID: env.nsID, TraceID: traceID, ParentTaskID: &parentID,
		CapabilityID: capDef.ID, Capability: capDef.Name, Version: capDef.Version, Digest: capDef.Digest,
		Params: json.RawMessage(`{}`),
	}
	if err := env.db.CreateTasks(ctx, []*store.Task{child}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	return parent
}

// TestTaskEndpointsExposeNoOut GET …/tasks 与 …/tasks/{id} 的每个任务对象都没有 out。
func TestTaskEndpointsExposeNoOut(t *testing.T) {
	env := newTaskOutEnv(t)
	const traceID = "trace_no_out_api"
	parent := seedTaskOutTrace(t, env, traceID)

	// ① 列表端点（List 信封：items 恒数组）
	rr := env.do(t, http.MethodGet, env.tasksPath())
	if rr.Code != http.StatusOK {
		t.Fatalf("GET …/tasks status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	list, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("items 不是数组: %v（body=%s）", m["items"], rr.Body.String())
	}
	if len(list) != 2 {
		t.Fatalf("items = %d 条, want 2; body=%s", len(list), rr.Body.String())
	}
	for i, raw := range list {
		task, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("items[%d] 不是对象: %v", i, raw)
		}
		assertTaskJSONHasNoOut(t, fmt.Sprintf("GET …/tasks.items[%d]", i), task)
	}

	// ② 单任务详情（v4 是四键信封，没有 v3 的 {task, chain} 包装）
	rr = env.do(t, http.MethodGet, env.tasksPath()+"/"+parent.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET …/tasks/{id} status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	detail := decode(t, rr)
	if got := detail["kind"]; got != "Task" {
		t.Fatalf("详情 kind = %v, want \"Task\"（body=%s）", got, rr.Body.String())
	}
	assertTaskJSONHasNoOut(t, "GET …/tasks/{id}", detail)

	// ③ 整条 exec chain：v3 详情端点的 chain 在 v4 由 trace 读模型给出，
	//    这里一并钉住「链上每一行都干净」这条原意。
	rr = env.do(t, http.MethodGet, env.tracePath(traceID))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET …/traces/{id} status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	chain, ok := decode(t, rr)["tasks"].([]any)
	if !ok || len(chain) != 2 {
		t.Fatalf("trace.tasks = %v 条, want 2（详情页一次给齐整条链）; body=%s", len(chain), rr.Body.String())
	}
	for i, raw := range chain {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("trace.tasks[%d] 不是对象: %v", i, raw)
		}
		assertTaskJSONHasNoOut(t, fmt.Sprintf("GET …/traces/{id}.tasks[%d]", i), row)
	}
}

// TestTraceEndpointExposesNoOut GET …/traces/{id} 的 tasks 里也没有 out。
func TestTraceEndpointExposesNoOut(t *testing.T) {
	env := newTaskOutEnv(t)
	ctx := context.Background()
	capDef := env.capability

	const traceID = "trace_no_out_trace_view"
	// v4 的 InsertEvent 显式收 now/窗口（v3 的 IdemKey 字段已被 DeliveryID 取代；
	// 这条事件不带投递键，窗口参数不影响结论）。
	if _, err := env.db.InsertEvent(ctx, &store.Event{
		NamespaceID: env.nsID, TraceID: traceID, Source: "github",
		Type: "pull_request.opened", Payload: json.RawMessage(`{"number":1}`),
	}, store.Now(), store.DefaultRetentionEventTTLSec); err != nil {
		t.Fatalf("insert root event: %v", err)
	}
	task := &store.Task{
		NamespaceID: env.nsID, TraceID: traceID, CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: capDef.Version, Digest: capDef.Digest,
		Params: json.RawMessage(`{"number":1}`),
	}
	if err := env.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	rr := env.do(t, http.MethodGet, env.tracePath(traceID))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET …/traces/{id} status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	tasks, ok := m["tasks"].([]any)
	if !ok {
		t.Fatalf("trace.tasks 不是数组: %v（body=%s）", m["tasks"], rr.Body.String())
	}
	if len(tasks) != 1 {
		t.Fatalf("trace.tasks = %d 条, want 1; body=%s", len(tasks), rr.Body.String())
	}
	for i, raw := range tasks {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tasks[%d] 不是对象: %v", i, raw)
		}
		assertTaskJSONHasNoOut(t, fmt.Sprintf("GET …/traces/{id}.tasks[%d]", i), row)
	}
	// events 也走同一份读模型：事件信封的 spec/status 同样是「只读事实」，不该冒出路由结果。
	events, ok := m["events"].([]any)
	if !ok {
		t.Fatalf("trace.events 不是数组: %v（body=%s）", m["events"], rr.Body.String())
	}
	if len(events) != 1 {
		t.Fatalf("trace.events = %d 条, want 1; body=%s", len(events), rr.Body.String())
	}
	for i, raw := range events {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("events[%d] 不是对象: %v", i, raw)
		}
		assertTaskJSONHasNoOut(t, fmt.Sprintf("GET …/traces/{id}.events[%d]", i), row)
	}
}
