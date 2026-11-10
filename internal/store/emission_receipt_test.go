package store

// emission_receipt_test.go 是 E1-D1/D2 的 store 侧白盒验收：
//
//   - 调用方预分配的失败 TaskID 就是落库行 id（回执身份不可能是假的）；
//   - 回执字节点名的 taskId 必须指向本事务真实创建的行（emitResult 不假定原字节一定对）；
//   - 非法形状 / 与既有任务碰撞 → 整个 emit 事务回滚（emit 行/任务/事件都不留，不覆盖既有行）；
//   - 省略 TaskID 仍生成真实 id（source 多 fanout 的内部行为不变）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// traceFailedTasks 列 trace 里的未解析失败任务（capability_id IS NULL）。
func traceFailedTasks(t *testing.T, db *DB, namespaceID, traceID string) []*Task {
	t.Helper()
	tasks, err := db.ListTasksByTrace(context.Background(), namespaceID, traceID)
	if err != nil {
		t.Fatalf("列 trace 任务: %v", err)
	}
	out := make([]*Task, 0, 1)
	for _, task := range tasks {
		if task.CapabilityID == "" {
			out = append(out, task)
		}
	}
	return out
}

// traceEvents 列 trace 的事件（升序）。
func traceEvents(t *testing.T, db *DB, namespaceID, traceID string) []*Event {
	t.Helper()
	events, err := db.ListEvents(context.Background(), namespaceID, traceID, "", 100)
	if err != nil {
		t.Fatalf("列 trace 事件: %v", err)
	}
	return events
}

// TestEmitAuthorizedFailureTaskSuppliedIDIsReceiptIdentity 钉住「回执 taskId ↔ 真实失败 Task」
// 是同一条事实：给定 ID → 行用它、事件绑定它、回执原字节点名它；同 id 回放不改字节、不新建行。
func TestEmitAuthorizedFailureTaskSuppliedIDIsReceiptIdentity(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "supl-node", "tr_emit_supplied")

	supplied := NewTaskID()
	emitID := "emit-supplied"
	body := json.RawMessage(`{"taskId":"` + supplied + `","eventId":"evt_receipt","to":"ghost@2","version":2}`)
	planned := func() *EmitRecord {
		return &EmitRecord{
			TaskID: parent.ID, EmitID: &emitID, ToRef: "ghost@2", Data: json.RawMessage(`{}`),
			Result:          body,
			PlannedFailures: []EmitFailureFact{{Capability: "ghost", Version: 2, TaskID: supplied}},
		}
	}

	result, first, err := db.EmitAuthorized(ctx, auth, planned(), nil)
	if err != nil || !first {
		t.Fatalf("emit: err=%v first=%v", err, first)
	}
	if string(result) != string(body) {
		t.Fatalf("落库 result 被改写: %s != %s", result, body)
	}

	task, err := db.GetTask(ctx, nsID, supplied)
	if err != nil {
		t.Fatalf("预分配 ID 的行不存在: %v", err)
	}
	if task.ID != supplied || task.Phase != TaskFailed || task.Attempt != 0 ||
		task.Capability != "ghost" || task.Version != 2 ||
		task.ParentTaskID == nil || *task.ParentTaskID != parent.ID ||
		task.Error != protocol.ReasonDownstreamMissing {
		t.Fatalf("失败 Task = %+v", task)
	}
	events := traceEvents(t, db, nsID, parent.TraceID)
	if len(events) != 1 {
		t.Fatalf("错误事件数 = %d，want 1", len(events))
	}
	var payload struct {
		TaskID string `json:"taskId"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("解析事件 payload: %v", err)
	}
	if events[0].Type != protocol.ReasonDownstreamMissing || payload.TaskID != supplied {
		t.Fatalf("错误事件没绑定同一个 TaskID: %+v / %+v", events[0], payload)
	}

	// 同 id 回放：原字节、无附带写。
	replay, dup, err := db.EmitAuthorized(ctx, auth, planned(), nil)
	if err != nil || dup {
		t.Fatalf("replay: err=%v first=%v", err, dup)
	}
	if string(replay) != string(body) {
		t.Fatalf("回放字节不同: %s != %s", replay, body)
	}
	if n := len(traceFailedTasks(t, db, nsID, parent.TraceID)); n != 1 {
		t.Fatalf("回放新建了失败 Task: %d", n)
	}
	if n := len(traceEvents(t, db, nsID, parent.TraceID)); n != 1 {
		t.Fatalf("回放新建了事件: %d", n)
	}
}

// TestEmitAuthorizedFailureTaskOmittedIDStillGenerated 钉住省略语义：不给 TaskID 仍生成真实 id
// （source 多 fanout 的内部行为不变，失败 Task 一条不少）。
func TestEmitAuthorizedFailureTaskOmittedIDStillGenerated(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "omitted-node", "tr_emit_omitted")

	emitID := "emit-omitted"
	_, first, err := db.EmitAuthorized(ctx, auth, &EmitRecord{
		TaskID: parent.ID, EmitID: &emitID, ToRef: "ghost", Data: json.RawMessage(`{}`),
		PlannedFailures: []EmitFailureFact{{Capability: "ghost"}},
	}, nil)
	if err != nil || !first {
		t.Fatalf("emit: err=%v first=%v", err, first)
	}
	failed := traceFailedTasks(t, db, nsID, parent.TraceID)
	if len(failed) != 1 {
		t.Fatalf("失败 Task 数 = %d，want 1", len(failed))
	}
	if !strings.HasPrefix(failed[0].ID, "tsk_") || failed[0].Version != 0 || failed[0].Phase != TaskFailed {
		t.Fatalf("省略 TaskID 时生成的行不对: %+v", failed[0])
	}
}

// TestEmitAuthorizedFailureTaskInvalidSuppliedIDRollsBack：形状非法 → 整事务回滚（0 写）。
func TestEmitAuthorizedFailureTaskInvalidSuppliedIDRollsBack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "badid-node", "tr_emit_badid")

	beforeTasks, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	beforeEvents := traceEvents(t, db, nsID, parent.TraceID)

	emitID := "emit-bad-id"
	_, _, err := db.EmitAuthorized(ctx, auth, &EmitRecord{
		TaskID: parent.ID, EmitID: &emitID, ToRef: "ghost", Data: json.RawMessage(`{}`),
		Result:          json.RawMessage(`{"taskId":"evt_not_a_task","to":"ghost"}`),
		PlannedFailures: []EmitFailureFact{{Capability: "ghost", TaskID: "evt_not_a_task"}},
	}, nil)
	if err == nil {
		t.Fatal("非法预分配 task id 被接受")
	}
	if _, lerr := db.LookupEmit(ctx, parent.ID, emitID); !errors.Is(lerr, ErrNotFound) {
		t.Fatalf("非法 id 留下了 emit 行: %v", lerr)
	}
	afterTasks, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	if len(afterTasks) != len(beforeTasks) {
		t.Fatalf("非法 id 留下了任务: %d → %d", len(beforeTasks), len(afterTasks))
	}
	if n := len(traceEvents(t, db, nsID, parent.TraceID)); n != len(beforeEvents) {
		t.Fatalf("非法 id 留下了事件: %d → %d", len(beforeEvents), n)
	}
}

// TestEmitAuthorizedFailureTaskSuppliedIDCollisionRollsBack：与既有任务碰撞 → 整事务回滚
// （不覆盖既有行，也不留下半截事实：先建的正常子任务同样回滚）。
func TestEmitAuthorizedFailureTaskSuppliedIDCollisionRollsBack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "collision-node", "tr_emit_collision")

	child := mkTask(nsID, parent.TraceID, c)
	child.ID = NewTaskID()
	beforeEvents := traceEvents(t, db, nsID, parent.TraceID)

	emitID := "emit-collision"
	_, _, err := db.EmitAuthorized(ctx, auth, &EmitRecord{
		TaskID: parent.ID, EmitID: &emitID, ToRef: "review", Data: json.RawMessage(`{}`),
		Result:          json.RawMessage(`{"taskId":"` + child.ID + `","to":"review"}`),
		PlannedFailures: []EmitFailureFact{{Capability: "ghost", TaskID: child.ID}},
	}, []*Task{child})
	if err == nil {
		t.Fatal("碰撞的预分配 task id 被接受（覆盖了既有任务）")
	}
	if _, lerr := db.LookupEmit(ctx, parent.ID, emitID); !errors.Is(lerr, ErrNotFound) {
		t.Fatalf("碰撞路径留下了 emit 行: %v", lerr)
	}
	tasks, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	if len(tasks) != 1 || tasks[0].ID != parent.ID {
		t.Fatalf("碰撞路径没有整事务回滚（先建的子任务还在）: %+v", tasks)
	}
	if n := len(traceEvents(t, db, nsID, parent.TraceID)); n != len(beforeEvents) {
		t.Fatalf("碰撞路径留下了事件: %d → %d", len(beforeEvents), n)
	}
}

// TestEmitResultRejectsPhantomTaskID 钉住 actual emitResult 的核对：回执点名的 taskId
// 若不是本事务真实创建的行，emit 直接失败（不把假身份落库成成功回执）。
func TestEmitResultRejectsPhantomTaskID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "phantom-node", "tr_emit_phantom")

	beforeTasks, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	emitID := "emit-phantom"
	_, _, err := db.EmitAuthorized(ctx, auth, &EmitRecord{
		TaskID: parent.ID, EmitID: &emitID, ToRef: "review", Data: json.RawMessage(`{}`),
		Result: json.RawMessage(`{"taskId":"tsk_phantom","to":"review"}`),
	}, nil)
	if err == nil {
		t.Fatal("回执点名的假 taskId 被落库成成功回执")
	}
	if _, lerr := db.LookupEmit(ctx, parent.ID, emitID); !errors.Is(lerr, ErrNotFound) {
		t.Fatalf("假回执路径留下了 emit 行: %v", lerr)
	}
	afterTasks, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	if len(afterTasks) != len(beforeTasks) {
		t.Fatalf("假回执路径留下了任务: %d → %d", len(beforeTasks), len(afterTasks))
	}
}
