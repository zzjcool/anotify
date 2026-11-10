package store

// 事务合并（tx_merge）族的安全用例 —— v4 迁移。
//
// v3 这一族比的是「合并写」与「拆分写」的等价性（TestReportResultMatchesSplitWrites /
// …LeaseAndAssignMatchesSplitWrites / …ProgressTaskMatchesSplitWrites）：当时把四次独立提交
// 合成一个事务，所以必须证明合并后的结果与拆分逐字一致。
//
// v4 把这件事变成了**结构事实**：grant 与 result 各自只有一条写入路径（同一事务、
// 同一份 SQL 点），不存在「拆分版」可对照，所以等价性断言由「结果事实逐字段落库」
// 接走（v4test/result_facts_test.go 的 C3b 系列）。这里保留并迁移的是**两条安全断言**：
//
//	① 回报一条任务的结果，绝不能放掉**别人的**槽位（foreign slot）
//	② 对已经不持有当前绑定的终态任务重放结果，必须被拒且一行都不动（no slot touch）
//
// 其余 v3 用例逐条退役（映射见 TEST-MAPPING.md）：
//
//	TestProgressTaskMatchesSplitWrites / TestProgressTaskRejectsTerminalTask
//	  → 退役：v4 没有 ProgressTask（长任务的中间进度上报随 handler/central 执行器删除）。
//	TestLeaseAndAssignMatchesSplitWrites
//	  → 由 TestGrantMovesToRunning / TestAssignRecordsCandidateWithoutAttempt 接走：
//	    assign 只记候选（不耗 attempt、不绑槽），grant 才写租约与槽位。
//	TestLeaseAndAssignGuardFailureDoesNotLeakSlot
//	  → 由 TestGrantRejectsStaleAssignment（task_test.go）接走：被拒的 grant 之后
//	    `v4BusySlots == 0`，同一份不变量。

import (
	"context"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// txResult 是一次成功回报的最小事实集（exitCode 显式给 0，区别于「没报」）。
func txResult() TaskResult {
	start := timeAtUnixNano(Now())
	exit := 0
	return TaskResult{
		Phase:      TaskSucceeded,
		Summary:    "ok",
		Artifacts:  []protocol.TaskArtifact{},
		ExitCode:   &exit,
		StartedAt:  start,
		FinishedAt: start.Add(1_000_000_000),
	}
}

// txSlotOf 返回某条任务当前绑定的槽位号（没绑 → -1）。
func txSlotOf(t *testing.T, db *DB, nodeID, taskID string) int {
	t.Helper()
	insts, err := db.ListInstances(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range insts {
		if in.BusyTask != nil && *in.BusyTask == taskID {
			return in.Slot
		}
	}
	return -1
}

// TestReportResultDoesNotReleaseForeignSlot：A 的结果只能放掉 A 的槽位，
// 同节点上 B 的槽位必须原样保留（v3 的合并写里最危险的一条回归）。
func TestReportResultDoesNotReleaseForeignSlot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	a := v4Cap(t, db, nsID, "tx.a", 1)
	b := v4Cap(t, db, nsID, "tx.b", 1)
	node := v4Node(t, db, "tx-foreign", 2)

	taskA := v4Task(t, db, nsID, a, "tr_tx_a", "{}")
	leaseA, _, attemptA := v4Grant(t, db, taskA.ID, node.ID, "asg-tx-a")
	taskB := v4Task(t, db, nsID, b, "tr_tx_b", "{}")
	leaseB, _, attemptB := v4Grant(t, db, taskB.ID, node.ID, "asg-tx-b")

	slotA, slotB := txSlotOf(t, db, node.ID, taskA.ID), txSlotOf(t, db, node.ID, taskB.ID)
	if slotA < 0 || slotB < 0 || slotA == slotB {
		t.Fatalf("前置：两条任务应各占一个不同槽位（a=%d b=%d）", slotA, slotB)
	}

	disp, err := db.CompleteTaskClassifiedAuthorized(ctx, v4Auth(t, db, node.ID),
		taskA.ID, attemptA, leaseA, Now(), txResult())
	if err != nil {
		t.Fatalf("complete A: %v", err)
	}
	if disp != TaskResultRecorded {
		t.Fatalf("A 的回报分类 = %q, want recorded", disp)
	}
	v4MustPhase(t, db, nsID, taskA.ID, TaskSucceeded)

	if got := txSlotOf(t, db, node.ID, taskA.ID); got != -1 {
		t.Fatalf("A 的槽位应被释放，实际仍绑着 slot %d", got)
	}
	if got := txSlotOf(t, db, node.ID, taskB.ID); got != slotB {
		t.Fatalf("B 的槽位被别人的结果改动了：want slot %d, got %d", slotB, got)
	}
	// B 仍然持有自己的执行绑定（租约/attempt 不变）
	runningB := v4MustPhase(t, db, nsID, taskB.ID, TaskRunning)
	if runningB.LeaseID != leaseB || runningB.Attempt != attemptB {
		t.Fatalf("B 的执行绑定被改动: %+v", runningB)
	}
}

// TestReportResultRejectsTerminalTaskWithoutTouchingSlot：
// 一条任务被**新 attempt 重派到同一个槽位**之后，旧 attempt 的结果重放必须被拒，
// 且新绑定的槽位/租约一行都不动（旧结果不能跨 attempt 生效）。
//
// 「重派」用确定性库操作构造（v4 生产路径不会把终态任务重新派发；这里要的是
// 「同一 task 已换了当前绑定」这个精确状态，与 v4test/result_current_binding_test.go:18
// 的构造同源）。
func TestReportResultRejectsTerminalTaskWithoutTouchingSlot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := v4Cap(t, db, nsID, "tx.old", 1)
	node := v4Node(t, db, "tx-terminal", 1)

	// ① 旧 attempt 跑完（槽位释放，任务终态）
	task := v4Task(t, db, nsID, c, "tr_tx_old", "{}")
	oldLease, _, oldAttempt := v4Grant(t, db, task.ID, node.ID, "asg-tx-old")
	if _, err := db.CompleteTaskClassifiedAuthorized(ctx, v4Auth(t, db, node.ID),
		task.ID, oldAttempt, oldLease, Now(), txResult()); err != nil {
		t.Fatalf("complete old: %v", err)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskSucceeded)

	// ② 构造「同一任务换了当前绑定」：回到 pending，再用真实 grant 换 attempt=2 与租约，
	//    并占住同一个槽位（真实授权检查照常执行）。
	mustExec(t, db, `UPDATE tasks SET phase='pending', reported=0, lease_id='', lease_until=NULL,
		node_id=NULL, finished_at=NULL WHERE id=?`, task.ID)
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-tx-new", 2, Now()+1); err != nil {
		t.Fatalf("assign new attempt: %v", err)
	}
	newLease, _, newAttempt, _, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID),
		task.ID, "asg-tx-new", 2, 0, Now()+1)
	if err != nil {
		t.Fatalf("grant new attempt: %v", err)
	}
	if newAttempt != 2 || newLease == oldLease {
		t.Fatalf("前置：需要新的执行绑定（attempt=%d lease=%q）", newAttempt, newLease)
	}
	slot := txSlotOf(t, db, node.ID, task.ID)
	if slot < 0 {
		t.Fatal("新 attempt 应占住槽位")
	}

	// ③ 旧 attempt 的结果重放：跨 attempt 一律拒，且不许碰当前绑定
	_, err = db.CompleteTaskClassifiedAuthorized(ctx, v4Auth(t, db, node.ID),
		task.ID, oldAttempt, oldLease, Now()+2, txResult())
	if !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 的结果重放应 ErrLeaseMismatch, got %v", err)
	}
	if got := txSlotOf(t, db, node.ID, task.ID); got != slot {
		t.Fatalf("旧结果重放动了当前槽位: want %d, got %d", slot, got)
	}
	running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if running.LeaseID != newLease || running.Attempt != newAttempt {
		t.Fatalf("旧结果重放改了当前绑定: %+v", running)
	}
	insts, err := db.ListInstances(ctx, node.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range insts {
		if in.Slot == slot {
			if in.BusyLeaseID != newLease || in.BusyAttempt != newAttempt {
				t.Fatalf("槽位上的租约事实被改动: %+v", in)
			}
		}
	}
}
