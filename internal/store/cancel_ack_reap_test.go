package store

// #567：节点补报的 cancel.ack{processGroupReaped} 是「这个物理进程组没了」的唯一事实，
// 不是中枢要求的取消的回执（节点只发一次，拒掉=槽位永久占用）。只写回收事实并释放它自己那一格。

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func ackReaped(t *testing.T, db *DB, nodeID, taskID string, attempt int, now int64) error {
	t.Helper()
	in := v4ReapInput(t, db, nodeID, taskID, attempt)
	in.Now = now
	return db.AckCancelAuthorized(context.Background(), v4Auth(t, db, nodeID), in, true)
}

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

// TestAckCancelReapedAfterLeaseSweepAbandonFreesSlot：巡检已判 abandoned（V4-D4）后节点补报的 reaped 回执必须被接受，且只补回收事实。
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

	// 遗留行形态（#660 之前落下）：相位已终态、执行账 reaped 仍为 NULL，槽位按当时占用还原。
	if _, err := db.Exec(`UPDATE task_attempts SET reaped_at = NULL, reclaim_reason = '' WHERE task_id = ? AND attempt = ?`,
		task.ID, attempt); err != nil {
		t.Fatalf("造遗留执行账: %v", err)
	}
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
	done := v4MustPhase(t, db, nsID, task.ID, TaskAbandoned)
	if done.Error != reasonBefore {
		t.Fatalf("reaped 回执不得改写终局原因：before %q after %q", reasonBefore, done.Error)
	}
	if done.Attempt != attempt {
		t.Fatalf("reaped 回执不得推进 attempt：%d != %d", attempt, done.Attempt)
	}
	_ = leaseID
	next := v4Task(t, db, nsID, c, "tr_leak_1b", "")
	if _, _, gerr := mustGrantErr(t, db, node.ID, next.ID, "asg-2", Now()+10); gerr != nil {
		t.Fatalf("BUG(#567): 旧执行已回收，新任务仍无法 grant: %v", gerr)
	}
}

// TestAckCancelReapedAfterSweepKeepsSweepReclaimReason：巡检已收敛过（reaped_at=lease_abandon）时迟到回执是幂等 no-op。
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

// TestAckCancelReapedWhileRunningByNodeStopFreesSlot：回执先于巡检到达时任务还 running 且无取消意图（中枢停机 >TTL 后先补发 / 升级 Drain）。旧实现 fail closed。
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
	still := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if still.LeaseID != leaseID || still.AttemptTokenHash != running.AttemptTokenHash ||
		still.CancelRequestedAt != nil || still.LeaseUntil == nil || *still.LeaseUntil != *running.LeaseUntil {
		t.Fatalf("reaped 回执不得改写 running 的任务事实：before=%+v after=%+v", running, still)
	}

	if rq, ab, serr := db.RecoverLostLeases(ctx, expiresAt+1); serr != nil || rq != 1 || ab != 0 {
		t.Fatalf("巡检应重排 1 条，得到 requeued=%d abandoned=%d err=%v", rq, ab, serr)
	}
	pending := v4MustPhase(t, db, nsID, task.ID, TaskPending)
	if pending.WaitingReason != WaitingRetryBackoff {
		t.Fatalf("重排必须带退避窗口，得到 %q", pending.WaitingReason)
	}

	next := v4Task(t, db, nsID, c, "tr_leak_2b", "")
	if _, _, gerr := mustGrantErr(t, db, node.ID, next.ID, "asg-2", Now()+10); gerr != nil {
		t.Fatalf("BUG(#567): 旧执行已回收，新任务仍无法 grant: %v", gerr)
	}
	v4MustPhase(t, db, nsID, next.ID, TaskRunning)
}

// TestAckCancelReapedIdempotentAndStillFailClosed 保住 A3 的两条不变式：重复回执幂等，错绑定 fail closed 且不释槽。
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

// TestAckCancelReapedAfterSuccessLeavesNewerBinding 钉住放宽后的边界：已 succeeded 的旧 attempt 补报回收只回收自己那一格。
func TestAckCancelReapedAfterSuccessLeavesNewerBinding(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "slow", 1)
	node := v4Node(t, db, "n1", 1)
	done := v4Task(t, db, nsID, c, "tr_leak_6", "")
	leaseID, doneAttempt, _ := mustGrant(t, db, node.ID, done.ID, "asg-6", Now())
	if err := db.CompleteTaskAuthorized(ctx, v4Auth(t, db, node.ID), done.ID, doneAttempt,
		leaseID, Now()+1, TaskResult{Phase: TaskSucceeded}); err != nil {
		t.Fatalf("完成 %s: %v", done.ID, err)
	}
	if _, err := db.Exec(`UPDATE task_attempts SET reaped_at = NULL, reclaim_reason = '' WHERE task_id = ? AND attempt = ?`,
		done.ID, doneAttempt); err != nil {
		t.Fatalf("造遗留执行账: %v", err)
	}
	next := v4Task(t, db, nsID, c, "tr_leak_6b", "")
	_, nextAttempt, _ := mustGrant(t, db, node.ID, next.ID, "asg-6b", Now()+10)
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("前置：新执行应占住那一格，busy=%d", busy)
	}

	if err := ackReaped(t, db, node.ID, done.ID, doneAttempt, Now()+20); err != nil {
		t.Fatalf("已 succeeded 的旧 attempt 的 reaped 回执应被接受（只补回收事实）: %v", err)
	}
	old, err := db.GetTaskAttempt(ctx, done.ID, doneAttempt)
	if err != nil || old.ReapedAt == nil || old.ReclaimReason != ReclaimReasonCancelAck {
		t.Fatalf("旧 attempt 必须补上回收事实：%+v err=%v", old, err)
	}
	v4MustPhase(t, db, nsID, done.ID, TaskSucceeded)
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("回执不得释放别人占着的槽（busy 应仍为 1，得到 %d）", busy)
	}
	if cur, cerr := db.GetTaskAttempt(ctx, next.ID, nextAttempt); cerr != nil || cur.ReapedAt != nil {
		t.Fatalf("新执行不得被旧回执回收：%+v err=%v", cur, cerr)
	}
}

// TestAckCancelNotReapedIsNoOpWithoutCancelRequest 钉住 reaped=false 的新语义：没有取消意图时不再 fail closed，而是一个事实都不写。
func TestAckCancelNotReapedIsNoOpWithoutCancelRequest(t *testing.T) {
	for _, phase := range []string{TaskRunning, TaskSucceeded} {
		t.Run(phase, func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			nsID, _ := newTestNS(t, db)
			c := v4Cap(t, db, nsID, "slow", 1)
			node := v4Node(t, db, "n1", 1)
			task := v4Task(t, db, nsID, c, "tr_leak_7", "")
			leaseID, attempt, _ := mustGrant(t, db, node.ID, task.ID, "asg-7", Now())
			if phase == TaskSucceeded {
				if err := db.CompleteTaskAuthorized(ctx, v4Auth(t, db, node.ID), task.ID, attempt,
					leaseID, Now()+1, TaskResult{Phase: TaskSucceeded}); err != nil {
					t.Fatalf("完成 %s: %v", task.ID, err)
				}
				if _, err := db.Exec(`UPDATE task_attempts SET reaped_at = NULL, reclaim_reason = '' WHERE task_id = ? AND attempt = ?`,
					task.ID, attempt); err != nil {
					t.Fatalf("造遗留执行账: %v", err)
				}
			}
			before := ackFacts(t, db)
			in := v4ReapInput(t, db, node.ID, task.ID, attempt)
			in.Now = Now() + 10
			if err := db.AckCancelAuthorized(ctx, v4Auth(t, db, node.ID), in, false); err != nil {
				t.Fatalf("reaped=false 不得因「没有取消意图」被拒（#567）: %v", err)
			}
			if after := ackFacts(t, db); !reflect.DeepEqual(before, after) {
				t.Fatalf("reaped=false 必须零写：\nbefore %v\nafter %v", before, after)
			}
		})
	}
}

// ackFacts 是「一个事实都不许写」的全量快照（SELECT * 保留 NULL）。
func ackFacts(t *testing.T, db *DB) map[string][][]any {
	t.Helper()
	out := map[string][][]any{}
	for name, query := range map[string]string{
		"tasks":    `SELECT * FROM tasks ORDER BY id`,
		"attempts": `SELECT * FROM task_attempts ORDER BY task_id, attempt`,
		"slots":    `SELECT * FROM node_instances ORDER BY id`,
	} {
		rows, err := db.QueryContext(context.Background(), query)
		if err != nil {
			t.Fatalf("快照 %s: %v", name, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatalf("快照 %s 列: %v", name, err)
		}
		out[name] = [][]any{}
		for rows.Next() {
			values, dest := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				_ = rows.Close()
				t.Fatalf("快照 %s 行: %v", name, err)
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					values[i] = string(b)
				}
			}
			out[name] = append(out[name], values)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("快照 %s 迭代: %v", name, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("快照 %s 关闭: %v", name, err)
		}
	}
	return out
}

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
