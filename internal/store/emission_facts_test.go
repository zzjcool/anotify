package store

// emission_facts_test.go 是 S1-D3 的白盒验收：
//
//   - EmitAuthorized 在同一授权事务里落**计划事实**（剪枝观察 + downstream_missing 失败 Task）；
//   - 授权先于 replay/prune/failure 写；同 id 同 body replay 返回原结果且 0 附带写；
//   - 撤凭证/取消后 0 附带写；任一 prune 写失败整事务回滚（emit 行/任务都不留）。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

func TestEmitAuthorizedPlannedFactsSameTransaction(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "facts-node", "tr_emit_facts")

	child := mkTask(nsID, parent.TraceID, c)
	emitID := "emit-facts"
	planned := func() *EmitRecord {
		return &EmitRecord{
			TaskID: parent.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{"a":1}`),
			PlannedPrunes: []EmitPruneFact{
				{Capability: "review", Reason: protocol.ReasonParamMissing, Detail: "需要 diff"},
			},
			PlannedFailures: []EmitFailureFact{
				{Capability: "ghost", Version: 2, Detail: "下游 ghost@2 解析不到"},
			},
		}
	}
	result, first, err := db.EmitAuthorized(ctx, auth, planned(), []*Task{child})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !first {
		t.Fatal("首次 emit 应 first=true")
	}
	var resultObj struct {
		TaskIDs []string `json:"taskIds"`
	}
	if err := json.Unmarshal(result, &resultObj); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(resultObj.TaskIDs) != 1 || resultObj.TaskIDs[0] != child.ID {
		t.Fatalf("emit result 只应含成功子任务: %+v", resultObj)
	}

	// 同 trace：父 + 正常子 + 失败子 = 3，失败子 NULL ref 且挂父任务。
	tasks, err := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	if err != nil {
		t.Fatalf("list trace tasks: %v", err)
	}
	var failure *Task
	for _, task := range tasks {
		if task.CapabilityID == "" {
			failure = task
		}
	}
	if failure == nil {
		t.Fatalf("失败 Task 没有落库: %d 行", len(tasks))
	}
	if failure.Phase != TaskFailed || failure.Attempt != 0 || failure.Version != 2 ||
		failure.Digest != "" || failure.Error != protocol.ReasonDownstreamMissing ||
		failure.ParentTaskID == nil || *failure.ParentTaskID != parent.ID {
		t.Fatalf("失败 Task facts = %+v", failure)
	}

	// 观察事件：剪枝 + downstream_missing。
	events, err := db.ListEvents(ctx, nsID, parent.TraceID, "", 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	seen := map[string]int{}
	for _, ev := range events {
		seen[ev.Type]++
	}
	if seen[protocol.ReasonParamMissing] != 1 || seen[protocol.ReasonDownstreamMissing] != 1 {
		t.Fatalf("计划观察事件 = %+v", seen)
	}

	// 同 id 同 body replay：原结果、0 附带写。
	replayed, dup, err := db.EmitAuthorized(ctx, auth, planned(), []*Task{mkTask(nsID, parent.TraceID, c)})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if dup {
		t.Fatal("replay 应 first=false")
	}
	if string(replayed) != string(result) {
		t.Fatalf("replay 结果不同: %s != %s", replayed, result)
	}
	tasksAfter, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	eventsAfter, _ := db.ListEvents(ctx, nsID, parent.TraceID, "", 50)
	if len(tasksAfter) != len(tasks) || len(eventsAfter) != len(events) {
		t.Fatalf("replay 产生了附带写: tasks %d→%d events %d→%d",
			len(tasks), len(tasksAfter), len(events), len(eventsAfter))
	}
}

func TestEmitAuthorizedRevokedAuthorityWritesNoFacts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "revoked-node", "tr_emit_revoked")

	before, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	beforeEvents, _ := db.ListEvents(ctx, nsID, parent.TraceID, "", 50)

	emitID := "emit-revoked"
	// 旧/错 credential：授权必须先于任何计划事实写。
	badAuth := auth
	badAuth.NodeTokenHash = "deadbeef"
	_, _, err := db.EmitAuthorized(ctx, badAuth, &EmitRecord{
		TaskID: parent.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{}`),
		PlannedPrunes:   []EmitPruneFact{{Capability: "review", Reason: protocol.ReasonParamMissing}},
		PlannedFailures: []EmitFailureFact{{Capability: "ghost"}},
	}, []*Task{mkTask(nsID, parent.TraceID, c)})
	if !errors.Is(err, ErrNodeIdentityMismatch) {
		t.Fatalf("错凭证应 ErrNodeIdentityMismatch，得到 %v", err)
	}
	if _, lerr := db.LookupEmit(ctx, parent.ID, emitID); !errors.Is(lerr, ErrNotFound) {
		t.Fatalf("拒绝路径写了 emit 行: %v", lerr)
	}
	after, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	afterEvents, _ := db.ListEvents(ctx, nsID, parent.TraceID, "", 50)
	if len(after) != len(before) || len(afterEvents) != len(beforeEvents) {
		t.Fatalf("拒绝路径产生了计划事实: tasks %d→%d events %d→%d",
			len(before), len(after), len(beforeEvents), len(afterEvents))
	}

	// 已请求取消：授权收回，同样 0 附带写。
	if err := db.RequestCancel(ctx, parent.ID, protocol.ReasonCancelledByUser, Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	emitID2 := "emit-cancelled"
	_, _, err = db.EmitAuthorized(ctx, auth, &EmitRecord{
		TaskID: parent.ID, EmitID: &emitID2, ToRef: "fix", Data: json.RawMessage(`{}`),
		PlannedPrunes: []EmitPruneFact{{Capability: "review", Reason: protocol.ReasonParamMissing}},
	}, []*Task{mkTask(nsID, parent.TraceID, c)})
	if !errors.Is(err, ErrTaskNotRunning) {
		t.Fatalf("取消后 emit 应 ErrTaskNotRunning，得到 %v", err)
	}
	afterCancel, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	if len(afterCancel) != len(before) {
		t.Fatalf("取消后 emit 产生了计划事实: %d→%d", len(before), len(afterCancel))
	}
}

func TestEmitAuthorizedPruneFailureRollsBackWholeTransaction(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, _, parent, auth, _ := emitFixture(t, db, nsID, "review", "prune-fail-node", "tr_emit_prunefail")

	mustExec(t, db, `CREATE TRIGGER s1_emit_prune_fail BEFORE INSERT ON events
		WHEN NEW.source = 'route' AND NEW.type = 'param_missing'
		BEGIN SELECT RAISE(ABORT, 'injected prune write failure'); END`)

	emitID := "emit-prune-fail"
	_, _, err := db.EmitAuthorized(ctx, auth, &EmitRecord{
		TaskID: parent.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{}`),
		PlannedPrunes:   []EmitPruneFact{{Capability: "review", Reason: protocol.ReasonParamMissing}},
		PlannedFailures: []EmitFailureFact{{Capability: "ghost"}},
	}, []*Task{mkTask(nsID, parent.TraceID, c)})
	if err == nil {
		t.Fatal("prune 写失败被吞了")
	}
	if _, lerr := db.LookupEmit(ctx, parent.ID, emitID); !errors.Is(lerr, ErrNotFound) {
		t.Fatalf("prune 写失败后 emit 行还在: %v", lerr)
	}
	tasks, _ := db.ListTasksByTrace(ctx, nsID, parent.TraceID)
	for _, task := range tasks {
		if task.ID != parent.ID {
			t.Fatalf("prune 写失败后子任务/失败任务还在: %+v", task)
		}
	}
}
