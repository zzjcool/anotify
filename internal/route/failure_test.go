package route

import (
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

type failureFixture struct {
	db     *store.DB
	ns     *store.Namespace
	impl   *store.CapabilityVersion
	record *store.CapabilityVersion
}

func newFailureFixture(t *testing.T, fallback *artifact.Fallback) *failureFixture {
	t.Helper()
	db := testDB(t)
	ns := testNamespace(t, db)
	record := testCapability(t, db, ns.ID, "issue-record", 1, capOpts{
		Params: `{"type":"object","required":["repo","issue"],
			"properties":{"repo":{"type":"string"},"issue":{"type":"integer"},
			"outcome":{"type":"string"},"findings":{"type":"string"}}}`,
	})
	impl := testCapability(t, db, ns.ID, "issue-implement", 1, capOpts{
		Params: `{"type":"object","required":["repo","issue"],
			"properties":{"repo":{"type":"string"},"issue":{"type":"integer"}}}`,
		Next:     []string{"issue-record"},
		Fallback: fallback,
	})
	return &failureFixture{db: db, ns: ns, impl: impl, record: record}
}

func (f *failureFixture) seedFailed(t *testing.T, reason string) *store.Task {
	t.Helper()
	task := &store.Task{
		ID:           store.NewTaskID(),
		NamespaceID:  f.ns.ID,
		TraceID:      store.NewEventID(),
		CapabilityID: f.impl.ID,
		Capability:   f.impl.Name,
		Version:      f.impl.Version,
		Digest:       f.impl.Digest,
		Params:       json.RawMessage(`{"repo":"zzjcool/anotify","issue":700}`),
		Phase:        store.TaskPending,
	}
	if err := f.db.CreateTasks(testCtx(), []*store.Task{task}); err != nil {
		t.Fatalf("建任务行: %v", err)
	}
	raw, err := json.Marshal(store.TaskResult{Phase: store.TaskFailed, Reason: reason, Error: "超时"})
	if err != nil {
		t.Fatalf("编码结果: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE tasks SET phase = ?, error = ?, result_json = ? WHERE id = ?`,
		store.TaskFailed, "超时", string(raw), task.ID); err != nil {
		t.Fatalf("置终态: %v", err)
	}
	return mustTask(t, f.db, task.ID)
}

func TestDispatchFailureRecordsFactAndRunsDeclaredFallback(t *testing.T) {
	f := newFailureFixture(t, &artifact.Fallback{
		To:     "issue-record",
		Params: map[string]any{"outcome": "timeout", "findings": "因超时终止"},
	})
	task := f.seedFailed(t, protocol.ReasonTimeout)

	if err := New(f.db).DispatchFailure(testCtx(), task); err != nil {
		t.Fatalf("失败兜底: %v", err)
	}

	events := traceEventTypes(t, f.db, f.ns.ID, task.TraceID)
	if events[protocol.ReasonTaskFailed] != 1 {
		t.Fatalf("必须留一条 %s：%v", protocol.ReasonTaskFailed, events)
	}
	var payload struct {
		TaskID        string `json:"taskId"`
		FailureReason string `json:"failureReason"`
		Reason        string `json:"reason"`
	}
	row := f.db.QueryRow(`SELECT payload FROM events WHERE namespace_id = ? AND trace_id = ? AND type = ?`,
		f.ns.ID, task.TraceID, protocol.ReasonTaskFailed)
	var raw string
	if err := row.Scan(&raw); err != nil {
		t.Fatalf("读观察行: %v", err)
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("观察行不是 JSON 对象: %v", err)
	}
	if payload.TaskID != task.ID || payload.FailureReason != protocol.ReasonTimeout {
		t.Fatalf("观察行要带 taskId 与 failureReason（= 任务失败码），实际 %+v", payload)
	}
	if payload.Reason != "" {
		t.Fatalf("payload.reason 属于剪枝原因码，失败事实行不得占用：%q", payload.Reason)
	}

	tasks := traceTasks(t, f.db, f.ns.ID, task.TraceID)
	if len(tasks) != 2 {
		t.Fatalf("兜底必须建一条下游任务，trace 任务数 %d", len(tasks))
	}
	var child *store.Task
	for _, it := range tasks {
		if it.ID != task.ID {
			child = it
		}
	}
	if child == nil || child.Capability != "issue-record" || child.ParentTaskID == nil || *child.ParentTaskID != task.ID {
		t.Fatalf("兜底下游形状不对：%+v", child)
	}
	var params map[string]any
	if err := json.Unmarshal(child.Params, &params); err != nil {
		t.Fatalf("兜底参数不是对象: %v", err)
	}
	if params["repo"] != "zzjcool/anotify" || params["issue"] != float64(700) {
		t.Fatalf("兜底要继承父任务上下文参数，实际 %v", params)
	}
	if params["outcome"] != "timeout" || params["findings"] != "因超时终止" {
		t.Fatalf("兜底要带上声明的字面参数，实际 %v", params)
	}
}

func TestDispatchFailureWithoutDeclarationOnlyRecordsFact(t *testing.T) {
	f := newFailureFixture(t, nil)
	task := f.seedFailed(t, protocol.ReasonTimeout)

	if err := New(f.db).DispatchFailure(testCtx(), task); err != nil {
		t.Fatalf("失败兜底: %v", err)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, task.TraceID)); got != 1 {
		t.Fatalf("没声明兜底就不该建任务，trace 任务数 %d", got)
	}
	if events := traceEventTypes(t, f.db, f.ns.ID, task.TraceID); events[protocol.ReasonTaskFailed] != 1 {
		t.Fatalf("没声明兜底也必须留终态事实：%v", events)
	}
}

func TestDispatchFailureFallbackHonorsReasonScope(t *testing.T) {
	f := newFailureFixture(t, &artifact.Fallback{
		To:      "issue-record",
		Reasons: []string{protocol.ReasonTimeout},
		Params:  map[string]any{"outcome": "timeout"},
	})
	task := f.seedFailed(t, protocol.ReasonScriptFailed)
	if err := New(f.db).DispatchFailure(testCtx(), task); err != nil {
		t.Fatalf("失败兜底: %v", err)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, task.TraceID)); got != 1 {
		t.Fatalf("script_failed 不在 reasons 作用域里，不该派兜底；trace 任务数 %d", got)
	}
	if events := traceEventTypes(t, f.db, f.ns.ID, task.TraceID); events[protocol.ReasonTaskFailed] != 1 {
		t.Fatalf("不在作用域也必须留终态事实：%v", events)
	}
}

func TestDispatchFailureRecordsAbandonedTask(t *testing.T) {
	f := newFailureFixture(t, &artifact.Fallback{
		To:      "issue-record",
		Reasons: []string{protocol.ReasonMaxAttemptsExceeded},
		Params:  map[string]any{"outcome": "timeout"},
	})
	task := f.seedFailed(t, protocol.ReasonTimeout)
	if _, err := f.db.Exec(`UPDATE tasks SET phase = ?, error = ?, result_json = '{}' WHERE id = ?`,
		store.TaskAbandoned, protocol.ReasonMaxAttemptsExceeded, task.ID); err != nil {
		t.Fatalf("置 abandoned: %v", err)
	}
	task = mustTask(t, f.db, task.ID)
	if err := New(f.db).DispatchFailure(testCtx(), task); err != nil {
		t.Fatalf("abandoned 兜底: %v", err)
	}
	var payload struct {
		FailureReason string `json:"failureReason"`
	}
	row := f.db.QueryRow(`SELECT payload FROM events WHERE namespace_id = ? AND trace_id = ? AND type = ?`,
		f.ns.ID, task.TraceID, protocol.ReasonTaskFailed)
	var raw string
	if err := row.Scan(&raw); err != nil {
		t.Fatalf("中枢推进的 abandoned 也必须有终态事实：%v", err)
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("观察行不是 JSON: %v", err)
	}
	if payload.FailureReason != protocol.ReasonMaxAttemptsExceeded {
		t.Fatalf("失败码应来自 tasks.error，实际 %q", payload.FailureReason)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, task.TraceID)); got != 2 {
		t.Fatalf("声明了 max_attempts_exceeded 的兜底应当派发，trace 任务数 %d", got)
	}
}

func TestFailureFallbackOutsideNextIsRefusedAtWriteTime(t *testing.T) {
	f := newFailureFixture(t, nil)
	testCapability(t, f.db, f.ns.ID, "issue-record-2", 1, capOpts{})
	spec := artifact.Spec{
		Params: json.RawMessage(`{"type":"object"}`),
		Next:   []string{"issue-record-2"},
		Lifecycle: artifact.Lifecycle{OnFailure: &artifact.Fallback{
			To: "issue-unrelated", Params: map[string]any{"outcome": "timeout"},
		}},
		Run: artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}},
	}
	bad := &store.CapabilityVersion{NamespaceID: f.ns.ID, Name: "issue-impl-bad", Version: 1, Spec: spec}
	if err := f.db.CreateCapability(testCtx(), bad); err == nil {
		t.Fatal("兜底指向 next 之外必须被写入期拒")
	}
	spec.Lifecycle.OnFailure = &artifact.Fallback{To: "issue-record-2"}
	ok := &store.CapabilityVersion{NamespaceID: f.ns.ID, Name: "issue-impl-ok", Version: 1, Spec: spec}
	if err := f.db.CreateCapability(testCtx(), ok); err != nil {
		t.Fatalf("next 里的兜底应当被接受: %v", err)
	}
}

func TestDispatchFailureRouterTaskKeepsRouterFailed(t *testing.T) {
	f := newRouterFixture(t)
	task, _ := f.seedRouterTask(t, map[string]any{"issue": map[string]any{"title": "x"}})
	finish(t, f.db, task.ID, store.TaskFailed)

	if err := New(f.db).DispatchFailure(testCtx(), mustTask(t, f.db, task.ID)); err != nil {
		t.Fatalf("失败兜底: %v", err)
	}
	events := traceEventTypes(t, f.db, f.ns.ID, task.TraceID)
	if events[protocol.ReasonRouterFailed] != 1 || events[protocol.ReasonTaskFailed] != 0 {
		t.Fatalf("router 任务失败仍记 router_failed：%v", events)
	}
}

func TestDispatchFailureIgnoresNonFailedTask(t *testing.T) {
	f := newFailureFixture(t, &artifact.Fallback{To: "issue-record"})
	task := f.seedFailed(t, protocol.ReasonTimeout)
	finish(t, f.db, task.ID, store.TaskSucceeded)

	if err := New(f.db).DispatchFailure(testCtx(), mustTask(t, f.db, task.ID)); err != nil {
		t.Fatalf("非失败终态不该报错: %v", err)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, task.TraceID)); got != 1 {
		t.Fatalf("非失败终态不该派发兜底，trace 任务数 %d", got)
	}
}

func TestTaskFailureReasonReadsResultBytes(t *testing.T) {
	f := newFailureFixture(t, nil)
	task := f.seedFailed(t, protocol.ReasonNoProgress)
	if got := TaskFailureReason(task); got != protocol.ReasonNoProgress {
		t.Fatalf("原因码必须取自 result 原字节，实际 %q", got)
	}
	if got := TaskFailureReason(&store.Task{}); got != "" {
		t.Fatalf("没有结果时应当是空串，实际 %q", got)
	}
}

func TestDispatchFailureIsIdempotentPerTask(t *testing.T) {
	f := newFailureFixture(t, &artifact.Fallback{
		To: "issue-record", Params: map[string]any{"outcome": "timeout"},
	})
	task := f.seedFailed(t, protocol.ReasonTimeout)
	for i := 0; i < 3; i++ {
		if err := New(f.db).DispatchFailure(testCtx(), mustTask(t, f.db, task.ID)); err != nil {
			t.Fatalf("第 %d 次失败兜底: %v", i+1, err)
		}
	}
	if events := traceEventTypes(t, f.db, f.ns.ID, task.TraceID); events[protocol.ReasonTaskFailed] != 1 {
		t.Fatalf("重复兜底不得落第二条终态事实：%v", events)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, task.TraceID)); got != 2 {
		t.Fatalf("重复兜底不得派第二个兜底任务：trace 任务数 %d", got)
	}
}

func TestDispatchFailureIsIdempotentForRouterTask(t *testing.T) {
	f := newRouterFixture(t)
	task, _ := f.seedRouterTask(t, map[string]any{"issue": map[string]any{"title": "x"}})
	finish(t, f.db, task.ID, store.TaskFailed)
	for i := 0; i < 2; i++ {
		if err := New(f.db).DispatchFailure(testCtx(), mustTask(t, f.db, task.ID)); err != nil {
			t.Fatalf("第 %d 次失败兜底: %v", i+1, err)
		}
	}
	if events := traceEventTypes(t, f.db, f.ns.ID, task.TraceID); events[protocol.ReasonRouterFailed] != 1 {
		t.Fatalf("router 任务的失败事实也必须只落一条：%v", events)
	}
}
