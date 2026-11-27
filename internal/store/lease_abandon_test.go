package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// onceCap 造一个「声明不可重试」的能力（maxAttempts=1）：只有它才会在租约失联时直接走
// 终局，从而验到 #660 的两本账收敛（有剩余预算的任务在 #661 之后改为重排）。
func onceCap(t *testing.T, db *DB, nsID, name string) *CapabilityVersion {
	t.Helper()
	one := 1
	return v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: name, Version: 1,
		Spec: artifact.Spec{Lifecycle: artifact.Lifecycle{MaxAttempts: &one}},
	})
}

func TestLeaseSweepAbandonSettlesBothLedgers(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := onceCap(t, db, nsID, "abandon-both-ledgers")
	node := v4Node(t, db, "abandon-node", 1)
	task := v4Task(t, db, nsID, c, "tr_abandon_both", "")
	_, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-1")

	running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("grant 之后应恰好占 1 个槽，得到 %d", busy)
	}
	if _, _, err := db.RecoverLostLeases(ctx, *running.LeaseUntil+1); err != nil {
		t.Fatalf("recover lost leases: %v", err)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskAbandoned)

	att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("读 attempt: %v", err)
	}
	if att.ReapedAt == nil {
		t.Fatalf("abandon 必须补执行账 reaped_at，否则节点预算恒满（#660）：%+v", att)
	}
	if att.ReclaimReason != ReclaimReasonLeaseAbandon {
		t.Fatalf("reaped 事实必须记录来源 %q，得到 %q", ReclaimReasonLeaseAbandon, att.ReclaimReason)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 0 {
		t.Fatalf("abandon 必须清槽账 busy_*（#660）：仍有 %d 个槽被占", busy)
	}
}

func TestConsecutiveLeaseAbandonsDoNotLeakCapacity(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := onceCap(t, db, nsID, "abandon-repeat")
	node := v4Node(t, db, "repeat-node", 1)
	auth := v4Auth(t, db, node.ID)

	for i := 1; i <= 2; i++ {
		task := v4Task(t, db, nsID, c, "tr_abandon_"+string(rune('0'+i)), "")
		_, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-"+string(rune('0'+i)))
		running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
		if _, _, err := db.RecoverLostLeases(ctx, *running.LeaseUntil+1); err != nil {
			t.Fatalf("第 %d 轮 recover: %v", i, err)
		}
		v4MustPhase(t, db, nsID, task.ID, TaskAbandoned)
		att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
		if err != nil {
			t.Fatalf("第 %d 轮读 attempt: %v", i, err)
		}
		if att.ReapedAt == nil {
			t.Fatalf("第 %d 轮 abandon 之后 reaped_at 仍为 NULL（执行账漏）：%+v", i, att)
		}
		if busy := v4BusySlots(t, db, node.ID); busy != 0 {
			t.Fatalf("第 %d 轮 abandon 之后仍占 %d 个槽（槽账漏）", i, busy)
		}
	}

	third := v4Task(t, db, nsID, c, "tr_abandon_third", "")
	slot := v4FreeSlot(t, db, node.ID)
	if err := db.AssignTask(ctx, third.ID, node.ID, "asg-third", 1); err != nil {
		t.Fatalf("assign 第三个任务: %v", err)
	}
	if _, _, _, got, _, err := db.GrantLeaseAuthorized(ctx, auth, third.ID, "asg-third", 1, slot, Now()); err != nil {
		t.Fatalf("连续两次 abandon 后第三个任务必须被接受（容量不得累积泄漏），得到 %v", err)
	} else if got != 1 {
		t.Fatalf("第三个任务应是第一次尝试，得到 attempt=%d", got)
	}
	v4MustPhase(t, db, nsID, third.ID, TaskRunning)
}

func TestRunningExecutionStillConsumesBudget(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "abandon-budget", 1)
	node := v4Node(t, db, "budget-node", 2)
	putV4NodeCfg(t, db, node.ID, []string{"*"}, nil, map[string]any{"total": 1})
	v4RecordApplied(t, db, node.ID)

	first := v4Task(t, db, nsID, c, "tr_budget_first", "")
	v4Grant(t, db, first.ID, node.ID, "asg-1")
	v4MustPhase(t, db, nsID, first.ID, TaskRunning)

	second := v4Task(t, db, nsID, c, "tr_budget_second", "")
	slot := v4FreeSlot(t, db, node.ID)
	if err := db.AssignTask(ctx, second.ID, node.ID, "asg-2", 1); err != nil {
		t.Fatalf("assign second: %v", err)
	}
	_, _, _, _, _, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID), second.ID, "asg-2", 1, slot, Now())
	if !errors.Is(err, ErrSlotUnavailable) || !strings.Contains(err.Error(), "机器总量预算已满") {
		t.Fatalf("running 执行必须继续计入预算（机器总量 1/1），得到 %v", err)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("预算拒绝不得改槽账本，得到 busy=%d", busy)
	}
}

func TestConvergeTerminalLedgersIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "abandon-converge", 1)
	node := v4Node(t, db, "converge-node", 2)

	legacy := v4Task(t, db, nsID, c, "tr_converge_legacy", "")
	_, _, legacyAttempt := v4Grant(t, db, legacy.ID, node.ID, "asg-legacy")
	if _, err := db.Exec(`UPDATE tasks SET phase = ?, finished_at = ? WHERE id = ?`,
		TaskAbandoned, Now(), legacy.ID); err != nil {
		t.Fatalf("造遗留行: %v", err)
	}
	stuck := v4Task(t, db, nsID, c, "tr_converge_stuck", "")
	_, _, stuckAttempt := v4Grant(t, db, stuck.ID, node.ID, "asg-stuck")
	stuckRunning := v4MustPhase(t, db, nsID, stuck.ID, TaskRunning)
	if err := db.RequestCancel(ctx, stuck.ID, "(取消)", Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, *stuckRunning.LeaseUntil+1); err != nil {
		t.Fatalf("recover: %v", err)
	}
	v4MustPhase(t, db, nsID, stuck.ID, TaskCancelled)

	first, err := db.ConvergeTerminalLedgers(ctx, Now())
	if err != nil {
		t.Fatalf("首次收敛: %v", err)
	}
	if first.AttemptsReaped != 1 || first.SlotsReleased != 1 {
		t.Fatalf("首次收敛应各补 1 行，得到 %+v", first)
	}
	reaped, err := db.GetTaskAttempt(ctx, legacy.ID, legacyAttempt)
	if err != nil {
		t.Fatalf("读遗留 attempt: %v", err)
	}
	if reaped.ReapedAt == nil || reaped.ReclaimReason != ReclaimReasonSweepConverge {
		t.Fatalf("遗留 attempt 必须被补上 reaped 事实：%+v", reaped)
	}
	kept, err := db.GetTaskAttempt(ctx, stuck.ID, stuckAttempt)
	if err != nil {
		t.Fatalf("读取消在飞的 attempt: %v", err)
	}
	if kept.ReapedAt != nil {
		t.Fatalf("取消在飞的 attempt 不该被收敛写 reaped：%+v", kept)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("收敛只清指向 abandoned 的槽，取消在飞那格必须留着，得到 busy=%d", busy)
	}

	second, err := db.ConvergeTerminalLedgers(ctx, Now())
	if err != nil {
		t.Fatalf("二次收敛: %v", err)
	}
	if second.AttemptsReaped != 0 || second.SlotsReleased != 0 {
		t.Fatalf("收敛必须幂等，第二轮得到 %+v", second)
	}
	reapedAgain, err := db.GetTaskAttempt(ctx, legacy.ID, legacyAttempt)
	if err != nil {
		t.Fatalf("二次读遗留 attempt: %v", err)
	}
	if *reapedAgain.ReapedAt != *reaped.ReapedAt || reapedAgain.ReclaimReason != reaped.ReclaimReason {
		t.Fatalf("二次收敛不得改行：before %+v after %+v", reaped, reapedAgain)
	}
}

// #660 读时兜底：终态任务的遗留 attempt（reaped NULL、槽行已删）不许再钉死预算。
func TestTerminalUnreapedAttemptDoesNotBlockBudget(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "abandon-legacy", 1)
	node := v4Node(t, db, "legacy-node", 1)

	stuck := v4Task(t, db, nsID, c, "tr_legacy_stuck", "")
	_, _, stuckAttempt := v4Grant(t, db, stuck.ID, node.ID, "asg-stuck")
	running := v4MustPhase(t, db, nsID, stuck.ID, TaskRunning)
	if err := db.RequestAbandon(ctx, stuck.ID, "e2e 放弃", Now()); err != nil {
		t.Fatalf("request abandon: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, *running.LeaseUntil+1); err != nil {
		t.Fatalf("recover: %v", err)
	}
	v4MustPhase(t, db, nsID, stuck.ID, TaskAbandoned)
	if att, err := db.GetTaskAttempt(ctx, stuck.ID, stuckAttempt); err != nil || att.ReapedAt != nil {
		t.Fatalf("取消在飞的 attempt 应仍未回收：%+v err=%v", att, err)
	}
	if _, err := db.Exec(`UPDATE node_instances SET busy_task = NULL, busy_attempt = 0,
		busy_lease_id = '', busy_assignment_id = '', busy_expires_at = 0 WHERE node_id = ?`, node.ID); err != nil {
		t.Fatalf("清槽行: %v", err)
	}

	next := v4Task(t, db, nsID, c, "tr_legacy_next", "")
	slot := v4FreeSlot(t, db, node.ID)
	if err := db.AssignTask(ctx, next.ID, node.ID, "asg-next", 1); err != nil {
		t.Fatalf("assign next: %v", err)
	}
	if _, _, _, _, _, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID),
		next.ID, "asg-next", 1, slot, Now()); err != nil {
		t.Fatalf("终态任务的遗留 attempt 不得再占预算（#660），得到 %v", err)
	}
	v4MustPhase(t, db, nsID, next.ID, TaskRunning)
}
