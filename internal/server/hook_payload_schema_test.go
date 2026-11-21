// TestHookPayloadSchemaRequiredOnlyGatesDelivery 是 #516 的 server 面：载荷契约只写 required（不写 properties）时必须真的闸住投递。
package server

import (
	"net/http"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

func TestHookPayloadSchemaRequiredOnlyGatesDelivery(t *testing.T) {
	env := newSourceResEnv(t)
	const token = "src_required_only"
	env.createSource(map[string]any{
		"kind": "webhook",
		"config": map[string]any{
			"token":         token,
			"payloadSchema": map[string]any{"required": []any{"action"}},
		},
		"targets": []any{map[string]any{"capability": "notify"}},
	}, map[string]any{"name": "required-only-hook"})

	before := hookSnapshot(t, env)
	rr := hookPost(t, env, token, []byte(`{"payload":{}}`), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("剪枝不是 4xx（事件已接收），期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if eventID := sourceResString(t, env.doc(rr)["eventId"]); eventID == "" {
		t.Fatalf("event_schema 剪枝仍必须返回 eventId：%s", rr.Body.String())
	}
	if tasks := sourceResAssertRawArray(t, rr, "tasks"); len(tasks) != 0 {
		t.Fatalf("载荷不合契约不得建任务，得到 %v", tasks)
	}
	pruned := sourceResAssertRawArray(t, rr, "pruned")
	if len(pruned) != 1 {
		t.Fatalf("应留 1 条剪枝，得到 %v", pruned)
	}
	item := sourceResObj(t, pruned[0])
	if reason := sourceResString(t, item["reason"]); reason != protocol.ReasonEventSchema {
		t.Fatalf("pruned[0].reason = %q，want event_schema", reason)
	}
	if detail := sourceResString(t, item["detail"]); detail != "载荷缺 action" {
		t.Fatalf("pruned[0].detail 应点名缺失的 action，得到 %q", detail)
	}
	if after := hookSnapshot(t, env); after.tasks != before.tasks {
		t.Fatalf("剪枝不得建任务：tasks %d → %d", before.tasks, after.tasks)
	}

	ok := hookPost(t, env, token, []byte(`{"payload":{"action":null}}`), "")
	if ok.Code != http.StatusOK {
		t.Fatalf("键存在应放行，得到 %d：%s", ok.Code, ok.Body.String())
	}
	if tasks := sourceResAssertRawArray(t, ok, "tasks"); len(tasks) != 1 {
		t.Fatalf("键存在（null 也算）必须放行——否则闸门就成了「一律剪」，得到 tasks %v", tasks)
	}
	if after := hookSnapshot(t, env); after.tasks != before.tasks+1 {
		t.Fatalf("放行应落 1 条任务：tasks %d → %d", before.tasks, after.tasks)
	}
}
