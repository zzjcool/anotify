package server

import (
	"net/http"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func traceEventRow(t *testing.T, doc map[string]any, uid string) map[string]any {
	t.Helper()
	for _, item := range trArray(t, doc, "events") {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if trString(t, row, "metadata", "uid") == uid {
			return row
		}
	}
	t.Fatalf("trace.events 里找不到 %s", uid)
	return nil
}

// TestTaskResourceFailureFactIsNotPrune 钉住 #700 review 的口径：失败终态事实不是剪枝。
func TestTaskResourceFailureFactIsNotPrune(t *testing.T) {
	env := newTaskResEnv(t)
	base := store.Now() - 60

	failedTrace := "trace_failure_fact"
	deliveryUID := env.seedEvent(env.ns, failedTrace, "github", "push", `{"issue":700}`, "", base)
	taskID := env.insertTask(seededTask{trace: failedTrace, phase: store.TaskFailed, errText: protocol.ReasonTimeout})
	factUID := env.seedEvent(env.ns, failedTrace, store.EventPruneSource, protocol.ReasonTaskFailed,
		`{"taskId":"`+taskID+`","capability":"review","phase":"failed","failureReason":"timeout","error":"超时"}`,
		store.EventReasonPruned, base+1)

	routerTrace := "trace_failure_fact_router"
	routerDeliveryUID := env.seedEvent(env.ns, routerTrace, "github", "push", `{"issue":701}`, "", base+2)
	routerTaskID := env.insertTask(seededTask{trace: routerTrace, phase: store.TaskFailed})
	routerFactUID := env.seedEvent(env.ns, routerTrace, store.EventPruneSource, protocol.ReasonRouterFailed,
		`{"taskId":"`+routerTaskID+`","capability":"review","phase":"failed","error":"boom"}`,
		store.EventReasonPruned, base+3)

	trace := trWant(t, env.do(http.MethodGet, "/v1/namespaces/"+env.ns+"/traces/"+failedTrace, nil), http.StatusOK)
	if got := len(trArray(t, trace, "pruned")); got != 0 {
		t.Fatalf("失败事实行不许进 pruned[]，实际 %d 条", got)
	}
	fact := traceEventRow(t, trace, factUID)
	if got := trString(t, fact, "spec", "eventType"); got != protocol.ReasonTaskFailed {
		t.Fatalf("失败事实行的 spec.eventType = %q, want %q", got, protocol.ReasonTaskFailed)
	}
	if reason, has := trObject(t, fact, "spec")["reason"]; has {
		t.Fatalf("失败事实行不许把 store 的 'Pruned' 当 reason 回给客户端，实际 %v", reason)
	}
	if got := trNumber(t, fact, "status", "prunedCount"); got != 0 {
		t.Fatalf("失败事实行的 status.prunedCount = %v, want 0", got)
	}
	if pruned := trObject(t, fact, "status")["pruned"]; pruned != nil {
		t.Fatalf("失败事实行的 status.pruned 必须是 null，得到 %v", pruned)
	}
	if got := trNumber(t, fact, "status", "dispatched"); got != 1 {
		t.Fatalf("失败事实行的 status.dispatched = %v, want 1（任务真的建过）", got)
	}
	// 任务失败码留在 payload 的 failureReason 里：不冒充剪枝原因码，也不丢事实。
	if got := trString(t, fact, "spec", "payload", "failureReason"); got != protocol.ReasonTimeout {
		t.Fatalf("payload.failureReason = %q, want %q", got, protocol.ReasonTimeout)
	}

	events := trWant(t, env.do(http.MethodGet, "/v1/namespaces/"+env.ns+"/events?trace="+failedTrace, nil), http.StatusOK)
	if got := trNumber(t, findEventRow(t, events, deliveryUID), "status", "prunedCount"); got != 0 {
		t.Fatalf("投递行的 status.prunedCount = %v, want 0（失败事实不是剪枝）", got)
	}

	routerTraceDoc := trWant(t, env.do(http.MethodGet, "/v1/namespaces/"+env.ns+"/traces/"+routerTrace, nil), http.StatusOK)
	if got := len(trArray(t, routerTraceDoc, "pruned")); got != 0 {
		t.Fatalf("router 失败事实行同样不许进 pruned[]，实际 %d 条", got)
	}
	if pruned := trObject(t, traceEventRow(t, routerTraceDoc, routerFactUID), "status")["pruned"]; pruned != nil {
		t.Fatalf("router 失败事实行的 status.pruned 必须是 null，得到 %v", pruned)
	}
	if got := trNumber(t, findEventRow(t, trWant(t, env.do(http.MethodGet,
		"/v1/namespaces/"+env.ns+"/events?trace="+routerTrace, nil), http.StatusOK), routerDeliveryUID),
		"status", "prunedCount"); got != 0 {
		t.Fatalf("router 失败 trace 的投递行 prunedCount = %v, want 0", got)
	}
}
