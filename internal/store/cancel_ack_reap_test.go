package store

// cancel_ack_reap_test.go 钉住 #567：节点在任务执行中掉线/重启（kill -9、systemd restart、
// 升级 Drain、中枢停机 >TTL 后自停）后补报的 `cancel.ack{processGroupReaped:true}` 不是
// 「中枢要求的取消」的回执，而是「这个物理进程组没了」的**唯一**事实（先于租约巡检到达，
// 且节点只发一次）。它曾按 `invalid_transition` 被拒 —— 槽位于是永久占用。
// 两条分支都只写 reaped 事实 + 释放它自己那一格，**不改相位/error/租约**（重排还是终局
// 仍归巡检按 stop_intent/maxAttempts 裁定，V4-D4 的「非幂等不重跑」不在这里）。

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// ackReaped 以精确历史绑定发送 processGroupReaped=true 的回执。
func ackReaped(t *testing.T, db *DB, nodeID, taskID string, attempt int, now int64) error {
	t.Helper()
	in := v4ReapInput(t, db, nodeID, taskID, attempt)
	in.Now = now
	return db.AckCancelAuthorized(context.Background(), v4Auth(t, db, nodeID), in, true)
}

// mustGrant 在节点的一个空闲槽上给任务授权（先 assign 再 grant，now 由调用方给）。
func mustGrant(t *testing.T, db *DB, nodeID, taskID, assignmentID string, now int64) (leaseID string, attempt int, expiresAt int64) {
	t.Helper()
	ctx := context.Background()
	if err := db.AssignTask(ctx, taskID, nodeID, assignmentID, 1); err != nil {
		t.Fatalf("assign %s: %v", taskID, err)
	}
	leaseID, _, _, attempt, expiresAt, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, nodeID),
		taskID, assignmentID, 1, v4FreeSlot(t, db, nodeID), now)
	if err != nil {
		t.Fatalf("grant %s: %v", taskID, err)
	}
	return leaseID, attempt, expiresAt
}

// TestAckCancelReapedAfterLeaseSweepAbandonFreesSlot 是验收用例 A：巡检已判 abandoned
// （V4-D4），节点补报的 reaped 回执必须被接受，且只补回收事实。
func TestAckCancelReapedAfterLeaseSweepAbandonFreesSlot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := onceCap(t, db, nsID, "slow") // maxAttempts=1：失联即终局
	node := v4Node(t, db, "n1", 1)

	task := v4Task(t, db, nsID, c, "tr_leak_1", "")
	leaseID, attempt, expiresAt := mustGrant(t, db, node.ID, task.ID, "asg-1", Now())
	if _, _, err := db.RecoverLostLeases(ctx, expiresAt+1); err != nil {
		t.Fatalf("巡检: %v", err)
	}
	abandoned := v4MustPhase(t, db, nsID, task.ID, TaskAbandoned)
	reasonBefore := abandoned.Error
	if reasonBefore == "" {
		t.Fatalf("前置：巡检终局必须带原因码：%+v", abandoned)
	}

	// 遗留行形态（#660 之前落下的终态行）：相位已终态、执行账 reaped 仍为 NULL。
	if _, err := db.Exec(`UPDATE task_attempts SET reaped_at = NULL, reclaim_reason = '' WHERE task_id = ? AND attempt = ?`,
		task.ID, attempt); err != nil {
		t.Fatalf("造遗留执行账: %v", err)
	}
	// 槽位在 #660 的巡检里已被释放，按遗留行还原占用（只造种子行）。
	if _, err := db.Exec(`UPDATE node_instances
	                         SET busy_task = ?, busy_attempt = ?, busy_lease_id = ?,
	                             busy_expires_at = ?, updated_at = ?
	                       WHERE node_id = ?`, task.ID, attempt, leaseID, expiresAt, Now(), node.ID); err != nil {
		t.Fatalf("还原遗留槽位: %v", err)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("前置：槽位必须仍被占（busy=%d）", busy)
	}
	legacy, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("读遗留 attempt: %v", err)
	}

	ackAt := expiresAt + 5
	if err := ackReaped(t, db, node.ID, task.ID, attempt, ackAt); err != nil {
		t.Fatalf("BUG(#567): 中枢拒收了 abandoned 任务的 reaped 回执: %v", err)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 0 {
		t.Fatalf("BUG(#567): 进程组已确认回收，中枢槽位仍被占用 busy=%d", busy)
	}
	att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("读 attempt: %v", err)
	}
	if att.ReapedAt == nil || *att.ReapedAt != ackAt {
		t.Fatalf("BUG(#567): task_attempts.reaped_at 必须是回执时间 %d，得到 %+v", ackAt, att.ReapedAt)
	}
	if att.ReclaimReason != ReclaimReasonCancelAck {
		t.Fatalf("回收事实的来源必须是 %q，得到 %q", ReclaimReasonCancelAck, att.ReclaimReason)
	}
	// 除了那条回收事实，其余字段（含错误/结果/起止）都必须原样：这不是结果提交。
	want := *legacy
	want.ReapedAt = &ackAt
	want.ReclaimReason = ReclaimReasonCancelAck
	if !reflect.DeepEqual(&want, att) {
		t.Fatalf("reaped 回执只该补回收事实，不得改 attempt 行：\n want %+v\n got  %+v", want, att)
	}
	// 相位/error 是巡检裁定的事实，回执不得改写。
	done := v4MustPhase(t, db, nsID, task.ID, TaskAbandoned)
	if done.Error != reasonBefore {
		t.Fatalf("reaped 回执不得改写终局原因：before %q after %q", reasonBefore, done.Error)
	}
	if done.Attempt != attempt {
		t.Fatalf("reaped 回执不得推进 attempt：%d != %d", done.Attempt, attempt)
	}
	_ = leaseID
	// 预算真的还回来了：下一条任务能在同一个槽上拿到授权。
	next := v4Task(t, db, nsID, c, "tr_leak_1b", "")
	if _, _, gerr := mustGrantErr(t, db, node.ID, next.ID, "asg-2", Now()+10); gerr != nil {
		t.Fatalf("BUG(#567): 旧执行已回收，新任务仍无法 grant: %v", gerr)
	}
}

// TestAckCancelReapedAfterSweepKeepsSweepReclaimReason 验巡检已收敛过（#660 写了
// reaped_at=lease_abandon）后，迟到的回执是逐字节幂等的 no-op，不报错。
func TestAckCancelReapedAfterSweepKeepsSweepReclaimReason(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := onceCap(t, db, nsID, "slow")
	node := v4Node(t, db, "n1", 1)
	task := v4Task(t, db, nsID, c, "tr_leak_1c", "")
	_, attempt, expiresAt := mustGrant(t, db, node.ID, task.ID, "asg-1", Now())
	if _, _, err := db.RecoverLostLeases(ctx, expiresAt+1); err != nil {
		t.Fatalf("巡检: %v", err)
	}
	before, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil || before.ReapedAt == nil || before.ReclaimReason != ReclaimReasonLeaseAbandon {
		t.Fatalf("前置：巡检必须已收敛执行账：%+v err=%v", before, err)
	}
	if err := ackReaped(t, db, node.ID, task.ID, attempt, expiresAt+5); err != nil {
		t.Fatalf("BUG(#567): 巡检已收敛后的迟到回执也应被接受（幂等 no-op），得到 %v", err)
	}
	after, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("二次读 attempt: %v", err)
	}
	if *after.ReapedAt != *before.ReapedAt || after.ReclaimReason != before.ReclaimReason {
		t.Fatalf("迟到回执不得改写巡检写下的回收事实：before=%+v after=%+v", before, after)
	}
}

// TestAckCancelReapedWhileRunningByNodeStopFreesSlot 是验收用例 B（中枢停机
// >TTL 后先补发回执 / 升级 Drain）：回执先于巡检到达时任务还 running 且无取消意图。
// 旧实现 fail closed，巡检随即重排，旧槽位与旧执行账永不收敛（limits.total=1 即报废）。
func TestAckCancelReapedWhileRunningByNodeStopFreesSlot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "slow", 1) // 默认 maxAttempts=3：巡检走重排分支
	node := v4Node(t, db, "n1", 1)
	task := v4Task(t, db, nsID, c, "tr_leak_2", "")
	leaseID, attempt, expiresAt := mustGrant(t, db, node.ID, task.ID, "asg-1", Now())
	running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)

	ackAt := Now() + 5
	if err := ackReaped(t, db, node.ID, task.ID, attempt, ackAt); err != nil {
		t.Fatalf("BUG(#567): 中枢拒收了「节点主动停掉进程组」的 reaped 回执: %v", err)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 0 {
		t.Fatalf("BUG(#567): 进程组已确认回收，中枢槽位仍被占用 busy=%d", busy)
	}
	att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil || att.ReapedAt == nil || att.ReclaimReason != ReclaimReasonCancelAck {
		t.Fatalf("必须写回收事实（reaped_at + %q）：%+v err=%v", ReclaimReasonCancelAck, att, err)
	}
	// 相位/租约/令牌都不动：终局与否归巡检，回执只证明物理事实。
	still := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if still.LeaseID != leaseID || still.AttemptTokenHash != running.AttemptTokenHash ||
		still.CancelRequestedAt != nil || still.LeaseUntil == nil || *still.LeaseUntil != *running.LeaseUntil {
		t.Fatalf("reaped 回执不得改写 running 的任务事实：before=%+v after=%+v", running, still)
	}

	// 巡检照旧按 maxAttempts 重排（attempt 预算还在），并给出退避窗口。
	if rq, ab, serr := db.RecoverLostLeases(ctx, expiresAt+1); serr != nil || rq != 1 || ab != 0 {
		t.Fatalf("巡检应重排 1 条，得到 requeued=%d abandoned=%d err=%v", rq, ab, serr)
	}
	pending := v4MustPhase(t, db, nsID, task.ID, TaskPending)
	if pending.WaitingReason != WaitingRetryBackoff {
		t.Fatalf("重排必须带退避窗口，得到 %q", pending.WaitingReason)
	}

	// 槽位与预算都已收敛：新任务能在这个唯一的槽上跑起来（旧实现必然「没有空闲 slot」）。
	next := v4Task(t, db, nsID, c, "tr_leak_2b", "")
	if _, _, gerr := mustGrantErr(t, db, node.ID, next.ID, "asg-2", Now()+10); gerr != nil {
		t.Fatalf("BUG(#567): 旧执行已回收，新任务仍无法 grant: %v", gerr)
	}
	v4MustPhase(t, db, nsID, next.ID, TaskRunning)
}

// TestAckCancelReapedIdempotentAndStillFailClosed 保住 A3 的两条不变式：重复回执逐字节
// 幂等（槽/attempt 行都不再动），错绑定的回执照旧 fail closed 且不释槽。
func TestAckCancelReapedIdempotentAndStillFailClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "slow", 1)
	node := v4Node(t, db, "n1", 2)
	task := v4Task(t, db, nsID, c, "tr_leak_3", "")
	_, attempt, _ := mustGrant(t, db, node.ID, task.ID, "asg-1", Now())

	first := Now() + 5
	if err := ackReaped(t, db, node.ID, task.ID, attempt, first); err != nil {
		t.Fatalf("首条 reaped 回执应被接受：%v", err)
	}
	before, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("读 attempt: %v", err)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 0 {
		t.Fatalf("首条回执应释放它绑定的那一格，busy=%d", busy)
	}

	// 第二条任务占住另一格：重复回执与错绑定回执都不得动它。
	other := v4Task(t, db, nsID, c, "tr_leak_4", "")
	_, otherAttempt, _ := mustGrant(t, db, node.ID, other.ID, "asg-4", first+1)
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("前置：第二条执行应占 1 格，busy=%d", busy)
	}

	in := v4ReapInput(t, db, node.ID, task.ID, attempt)
	in.Now = first + 1000
	in.Reason = "duplicate_ack"
	if err := db.AckCancelAuthorized(ctx, v4Auth(t, db, node.ID), in, true); err != nil {
		t.Fatalf("重复 reaped 回执应幂等：%v", err)
	}
	after, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("二次读 attempt: %v", err)
	}
	if *after.ReapedAt != *before.ReapedAt || after.ReclaimReason != before.ReclaimReason {
		t.Fatalf("重复回执不得改写回收事实：before=%+v after=%+v", before, after)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("重复回执不得再动槽位，busy=%d", busy)
	}

	// 错绑定（lease/slot/instance 任一不符）仍必须 fail closed：那是「哪次执行哪个槽」的唯一依据。
	for _, bad := range []ReapedInputs{
		func() ReapedInputs {
			b := v4ReapInput(t, db, node.ID, other.ID, otherAttempt)
			b.LeaseID = "lse_bogus"
			return b
		}(),
		func() ReapedInputs {
			b := v4ReapInput(t, db, node.ID, other.ID, otherAttempt)
			b.Slot = 1 - b.Slot
			return b
		}(),
		func() ReapedInputs {
			b := v4ReapInput(t, db, node.ID, other.ID, otherAttempt)
			b.InstanceID = "inst_bogus"
			return b
		}(),
	} {
		bad.Now = Now() + 10
		if err := db.AckCancelAuthorized(ctx, v4Auth(t, db, node.ID), bad, true); !errors.Is(err, ErrLeaseMismatch) {
			t.Fatalf("错绑定回执应 ErrLeaseMismatch，得到 %v（%+v）", err, bad)
		}
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("被拒的回执不得释放槽位，busy=%d", busy)
	}
	if att, aerr := db.GetTaskAttempt(ctx, other.ID, otherAttempt); aerr != nil || att.ReapedAt != nil {
		t.Fatalf("被拒的回执不得写回收事实：%+v err=%v", att, aerr)
	}
}

// mustGrantErr 与 mustGrant 同形，但把授权错误交回调用方判定（验收用例要断言「能授权」）。
func mustGrantErr(t *testing.T, db *DB, nodeID, taskID, assignmentID string, now int64) (string, int, error) {
	t.Helper()
	ctx := context.Background()
	if err := db.AssignTask(ctx, taskID, nodeID, assignmentID, 1); err != nil {
		t.Fatalf("assign %s: %v", taskID, err)
	}
	leaseID, _, _, attempt, _, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, nodeID),
		taskID, assignmentID, 1, v4FreeSlot(t, db, nodeID), now)
	return leaseID, attempt, err
}
