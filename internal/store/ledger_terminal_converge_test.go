package store

import (
	"context"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

func capWithLimits(t *testing.T, db *DB, nsID, name string, maxAttempts, maxPerNode int) *CapabilityVersion {
	t.Helper()
	spec := v4Spec("", nil, "")
	spec.Lifecycle = artifact.Lifecycle{MaxAttempts: &maxAttempts}
	spec.Concurrency = artifact.Concurrency{MaxPerNode: &maxPerNode}
	return v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: name, Version: 1,
		Spec: spec,
	})
}

func TestConvergeTerminalLedgersReleasesTerminalSlots(t *testing.T) {
	cases := []struct {
		name         string
		phase        string
		cancelAgeSec int64 // >0 = 取消在飞但 cancel.ack 永不到来
	}{
		{name: "succeeded", phase: TaskSucceeded},
		{name: "failed", phase: TaskFailed},
		{name: "cancelled_ack_lost", phase: TaskCancelled, cancelAgeSec: terminalSlotGrace + 60},
		{name: "abandoned", phase: TaskAbandoned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			nsID, _ := newTestNS(t, db)
			c := capWithLimits(t, db, nsID, "terminal-"+tc.name, 1, 1)
			node := v4Node(t, db, "terminal-"+tc.name+"-node", 1)
			task := v4Task(t, db, nsID, c, "tr_terminal_"+tc.name, "")
			_, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-"+tc.name)
			if busy := v4BusySlots(t, db, node.ID); busy != 1 {
				t.Fatalf("grant 之后应恰好占 1 个槽，得到 %d", busy)
			}
			now := Now()
			var cancelAt any
			if tc.cancelAgeSec > 0 {
				cancelAt = now - tc.cancelAgeSec
			}
			if _, err := db.Exec( // 终态是事实，占槽是没收回去的旧绑定
				`UPDATE tasks SET phase = ?, finished_at = ?, cancel_requested_at = ? WHERE id = ?`,
				tc.phase, now, cancelAt, task.ID); err != nil {
				t.Fatalf("造终局仍占槽的行: %v", err)
			}

			first, err := db.ConvergeTerminalLedgers(ctx, now)
			if err != nil {
				t.Fatalf("首次收敛: %v", err)
			}
			if first.AttemptsReaped != 1 || first.SlotsReleased != 1 {
				t.Fatalf("首次收敛应各补 1 行，得到 %+v", first)
			}
			att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
			if err != nil {
				t.Fatalf("读 attempt: %v", err)
			}
			if att.ReapedAt == nil {
				t.Fatalf("终态任务遗留的 attempt 必须补 reaped 事实（#682）：%+v", att)
			}
			if att.ReclaimReason != ReclaimReasonSweepConverge {
				t.Fatalf("reaped 事实必须记录来源 %q，得到 %q", ReclaimReasonSweepConverge, att.ReclaimReason)
			}
			if busy := v4BusySlots(t, db, node.ID); busy != 0 {
				t.Fatalf("终态任务必须清槽账 busy_*（#682：否则这一格永久消失），仍有 %d 个槽被占", busy)
			}

			second, err := db.ConvergeTerminalLedgers(ctx, now+1)
			if err != nil {
				t.Fatalf("二次收敛: %v", err)
			}
			if second.AttemptsReaped != 0 || second.SlotsReleased != 0 {
				t.Fatalf("收敛必须幂等，第二轮得到 %+v", second)
			}
			again, err := db.GetTaskAttempt(ctx, task.ID, attempt)
			if err != nil {
				t.Fatalf("二次读 attempt: %v", err)
			}
			if *again.ReapedAt != *att.ReapedAt || again.ReclaimReason != att.ReclaimReason {
				t.Fatalf("二次收敛不得改行：before %+v after %+v", att, again)
			}
		})
	}
}

func TestConvergeTerminalLedgersHonoursCancelAckGrace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := capWithLimits(t, db, nsID, "terminal-grace", 1, 1)
	node := v4Node(t, db, "terminal-grace-node", 1)
	task := v4Task(t, db, nsID, c, "tr_terminal_grace", "")
	_, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-grace")
	now := Now()
	if _, err := db.Exec(
		`UPDATE tasks SET phase = ?, finished_at = ?, cancel_requested_at = ? WHERE id = ?`,
		TaskCancelled, now, now, task.ID); err != nil {
		t.Fatalf("造取消在飞的终局行: %v", err)
	}

	fresh, err := db.ConvergeTerminalLedgers(ctx, now)
	if err != nil {
		t.Fatalf("宽限内收敛: %v", err)
	}
	if fresh.AttemptsReaped != 0 || fresh.SlotsReleased != 0 {
		t.Fatalf("宽限内的取消在飞行不得被收敛，得到 %+v", fresh)
	}
	att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("读 attempt: %v", err)
	}
	if att.ReapedAt != nil {
		t.Fatalf("宽限内的取消在飞 attempt 不该写 reaped：%+v", att)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("宽限内槽位必须留给 cancel.ack，得到 busy=%d", busy)
	}

	stale, err := db.ConvergeTerminalLedgers(ctx, now+terminalSlotGrace+1)
	if err != nil {
		t.Fatalf("宽限后收敛: %v", err)
	}
	if stale.AttemptsReaped != 1 || stale.SlotsReleased != 1 {
		t.Fatalf("宽限过后必须收敛（ack 不会再来），得到 %+v", stale)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 0 {
		t.Fatalf("宽限过后槽位必须收回，得到 busy=%d", busy)
	}
}

// #682 现场同款（见 docs/reports/issue-682-terminal-ledger-converge.md）：attempt 1 重排后旧槽仍压着、reaped NULL。
func TestConvergeTerminalLedgersRecoversSlotLeakedByRetry(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := capWithLimits(t, db, nsID, "terminal-retry", 2, 2)
	node := v4Node(t, db, "terminal-retry-node", 2)
	task := v4Task(t, db, nsID, c, "tr_terminal_retry", "")

	_, _, firstAttempt := v4Grant(t, db, task.ID, node.ID, "asg-retry-1")
	running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if _, _, err := db.RecoverLostLeases(ctx, *running.LeaseUntil+1); err != nil {
		t.Fatalf("recover 首次租约: %v", err)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskPending)

	lease2, _, secondAttempt := v4Grant(t, db, task.ID, node.ID, "asg-retry-2")
	v4MustPhase(t, db, nsID, task.ID, TaskRunning)

	if err := db.CompleteTaskAuthorized(ctx, v4Auth(t, db, node.ID), task.ID, secondAttempt,
		lease2, Now(), TaskResult{Phase: TaskSucceeded}); err != nil {
		t.Fatalf("完成 attempt 2: %v", err)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskSucceeded)

	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("收敛前旧 attempt 的槽必须仍被压着（#682 现场），得到 busy=%d", busy)
	}
	leaked, err := db.GetTaskAttempt(ctx, task.ID, firstAttempt)
	if err != nil {
		t.Fatalf("读 attempt 1: %v", err)
	}
	if leaked.ReapedAt != nil {
		t.Fatalf("重排不写 reaped（旧进程可能还在跑，等节点事实），此处应仍为 NULL：%+v", leaked)
	}

	got, err := db.ConvergeTerminalLedgers(ctx, Now())
	if err != nil {
		t.Fatalf("收敛: %v", err)
	}
	if got.AttemptsReaped != 1 || got.SlotsReleased != 1 {
		t.Fatalf("重排泄露的那一格应被收敛：期望 1/1，得到 %+v", got)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 0 {
		t.Fatalf("收敛后不得残留占用（真机验收③：不需要手工清库），得到 busy=%d", busy)
	}
	reaped, err := db.GetTaskAttempt(ctx, task.ID, firstAttempt)
	if err != nil {
		t.Fatalf("二次读 attempt 1: %v", err)
	}
	if reaped.ReapedAt == nil || reaped.ReclaimReason != ReclaimReasonSweepConverge {
		t.Fatalf("旧 attempt 必须补 sweep_converge 的 reaped 事实：%+v", reaped)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskSucceeded)

	third := v4Task(t, db, nsID, c, "tr_terminal_retry_third", "")
	v4Grant(t, db, third.ID, node.ID, "asg-retry-3") // 收敛回来的容量必须真的可用
	v4MustPhase(t, db, nsID, third.ID, TaskRunning)
}

func TestConvergeTerminalLedgersLeavesNonTerminalPhasesAlone(t *testing.T) {
	cases := []struct{ name, phase string }{
		{name: "pending_reservation", phase: TaskPending},
		{name: "running", phase: TaskRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			nsID, _ := newTestNS(t, db)
			c := capWithLimits(t, db, nsID, "nonterminal-"+tc.name, 2, 2)
			node := v4Node(t, db, "nonterminal-"+tc.name+"-node", 2)
			task := v4Task(t, db, nsID, c, "tr_nonterminal_"+tc.name, "")
			_, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-nonterminal-"+tc.name)
			now := Now()
			if tc.phase == TaskPending {
				running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
				if _, _, err := db.RecoverLostLeases(ctx, *running.LeaseUntil+1); err != nil {
					t.Fatalf("recover 首次租约: %v", err)
				}
				v4MustPhase(t, db, nsID, task.ID, TaskPending)
			}
			if busy := v4BusySlots(t, db, node.ID); busy != 1 {
				t.Fatalf("%s 仍占着槽，收敛前应 busy=1，得到 %d", tc.phase, busy)
			}

			got, err := db.ConvergeTerminalLedgers(ctx, now)
			if err != nil {
				t.Fatalf("收敛: %v", err)
			}
			if got.AttemptsReaped != 0 || got.SlotsReleased != 0 {
				t.Fatalf("非终态（%s）的执行不得被收敛，得到 %+v", tc.phase, got)
			}
			att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
			if err != nil {
				t.Fatalf("读 attempt: %v", err)
			}
			if att.ReapedAt != nil {
				t.Fatalf("非终态（%s）的 attempt 不该写 reaped：%+v", tc.phase, att)
			}
			if busy := v4BusySlots(t, db, node.ID); busy != 1 {
				t.Fatalf("非终态（%s）的槽位不得被释放，得到 busy=%d", tc.phase, busy)
			}
			v4MustPhase(t, db, nsID, task.ID, tc.phase)
		})
	}
}

func TestConvergeTerminalLedgersReclaimsAfterAckReportsNotReaped(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := capWithLimits(t, db, nsID, "terminal-ack-not-reaped", 1, 1)
	node := v4Node(t, db, "terminal-ack-not-reaped-node", 1)
	task := v4Task(t, db, nsID, c, "tr_terminal_ack_not_reaped", "")
	_, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-ack-not-reaped")
	running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if err := db.RequestCancel(ctx, task.ID, "(取消)", Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, *running.LeaseUntil+1); err != nil {
		t.Fatalf("recover 租约: %v", err)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskCancelled)

	in := v4ReapInput(t, db, node.ID, task.ID, attempt)
	in.Now = Now()
	if err := db.AckCancelAuthorized(ctx, v4Auth(t, db, node.ID), in, false); err != nil {
		t.Fatalf("ack cancel（processGroupReaped=false）: %v", err)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskCancelled)
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("ack 明说没回收时不得释槽，得到 busy=%d", busy)
	}
	kept, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("读 attempt: %v", err)
	}
	if kept.ReapedAt != nil {
		t.Fatalf("ack 明说没回收时不得写 reaped：%+v", kept)
	}

	now := Now()
	fresh, err := db.ConvergeTerminalLedgers(ctx, now)
	if err != nil {
		t.Fatalf("宽限内收敛: %v", err)
	}
	if fresh.AttemptsReaped != 0 || fresh.SlotsReleased != 0 {
		t.Fatalf("宽限内必须继续等节点回收事实，得到 %+v", fresh)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 1 {
		t.Fatalf("宽限内槽位不得被释放，得到 busy=%d", busy)
	}

	stale, err := db.ConvergeTerminalLedgers(ctx, now+terminalSlotGrace+1)
	if err != nil {
		t.Fatalf("宽限后收敛: %v", err)
	}
	if stale.AttemptsReaped != 1 || stale.SlotsReleased != 1 {
		t.Fatalf("宽限后仍占槽 = 那一格再也回不来，必须收敛，得到 %+v", stale)
	}
	reaped, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("二次读 attempt: %v", err)
	}
	if reaped.ReapedAt == nil || reaped.ReclaimReason != ReclaimReasonSweepConverge {
		t.Fatalf("宽限后必须补 sweep_converge 的 reaped 事实（明确的契约放宽）：%+v", reaped)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 0 {
		t.Fatalf("宽限后槽位必须收回，得到 busy=%d", busy)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskCancelled)
}
