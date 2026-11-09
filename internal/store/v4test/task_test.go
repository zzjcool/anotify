package v4test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// TestTaskGrantAttemptOnlyOnGrant 验「attempt 只在 grant 时 +1、排队不计」。
//
// assign 只是记候选（phase 仍 pending、没有租约）；grant 才把 attempt 推到 1 并授租约。
func TestTaskGrantAttemptOnlyOnGrant(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "grant-attempt")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 2)
	task := newTask(t, db, ns, c, "trace-1", "")

	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-1", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	queued, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get queued task: %v", err)
	}
	if queued.Attempt != 0 || queued.Phase != store.TaskPending {
		t.Fatalf("assign 不该计 attempt / 改相位：attempt=%d phase=%s", queued.Attempt, queued.Phase)
	}
	if queued.AssignmentID != "asg-1" || queued.NodeID == nil || *queued.NodeID != node.ID {
		t.Fatalf("assign 该落候选事实：assignment=%q node=%v", queued.AssignmentID, queued.NodeID)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("assign 不该占槽位")
	}

	leaseID, token, attempt, expiresAt, err := db.GrantLease(ctx, task.ID, "asg-1", node.ID, store.Now())
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if attempt != 1 || leaseID == "" || token == "" {
		t.Fatalf("grant 应授第一次租约：attempt=%d lease=%q token=%q", attempt, leaseID, token)
	}
	if expiresAt-store.Now() < int64(protocol.LeaseTTL.Seconds())-5 {
		t.Fatalf("租约时长应约 60s，得到 %d", expiresAt-store.Now())
	}
	granted := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	if granted.Attempt != 1 || granted.LeaseID != leaseID || granted.LeaseUntil == nil {
		t.Fatalf("grant 后应带执行授权事实：%+v", granted)
	}
	if granted.AssignmentID != "" {
		t.Fatalf("grant 应清掉候选：assignment=%q", granted.AssignmentID)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("grant 必须占槽位")
	}
	// 同一任务不能被授权两次（唯一执行权）。
	if _, _, _, _, err := db.GrantLease(ctx, task.ID, "", node.ID, store.Now()); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("第二次 grant 应被拒（ErrInvalidTransition），得到 %v", err)
	}
}

// TestTaskGrantRejectsStaleAssignment 验旧派发的 grant 被拒。
func TestTaskGrantRejectsStaleAssignment(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "grant-stale")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")

	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-new", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	_, _, _, _, err := db.GrantLease(ctx, task.ID, "asg-old", node.ID, store.Now())
	if !errors.Is(err, store.ErrNoAssignment) {
		t.Fatalf("旧 assignment 的 grant 应被拒（ErrNoAssignment），得到 %v", err)
	}
	mustPhase(t, db, ns, task.ID, store.TaskPending)
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("被拒的 grant 不该留下占用的槽位")
	}
}

// TestTaskMaxAttemptsTerminal 验 attempt 超预算时转终态（不再授租约）。
func TestTaskMaxAttemptsTerminal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "max-attempts")
	c := lifeCycleCap(t, db, ns, "flaky", false, 1)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")

	leaseID, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")
	if attempt != 1 {
		t.Fatalf("第一次 grant attempt = %d", attempt)
	}
	// 非幂等能力：租约过期直接 abandoned（不重跑）。
	if err := db.RequestCancel(ctx, task.ID, protocol.ReasonAbandonedByUser, store.Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	if err := db.AckCancel(ctx, task.ID, attempt, true, store.Now()); err != nil {
		t.Fatalf("ack cancel: %v", err)
	}
	got := mustPhase(t, db, ns, task.ID, store.TaskAbandoned)
	if got.Error != protocol.ReasonAbandonedByUser {
		t.Fatalf("终态应带原因码，得到 %q", got.Error)
	}
	_ = leaseID

	// 新任务把 attempt 预算（1）耗光：grant 一次 → 租约过期 → 终态。
	task2 := newTask(t, db, ns, c, "trace-2", "")
	grant(t, db, task2.ID, node.ID, "asg-2")
	// 手工把租约改成已过期，再让巡检回收。
	if _, err := db.Exec(
		`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task2.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	requeued, abandoned, err := db.RecoverLostLeases(ctx, store.Now())
	if err != nil {
		t.Fatalf("recover lost leases: %v", err)
	}
	if requeued != 0 || abandoned != 1 {
		t.Fatalf("非幂等能力过期租约应 abandoned：requeued=%d abandoned=%d", requeued, abandoned)
	}
	dead := mustPhase(t, db, ns, task2.ID, store.TaskAbandoned)
	if dead.Attempt != 1 || dead.MaxAttempts != 1 {
		t.Fatalf("终态应保留 attempt/maxAttempts 事实：%d/%d", dead.Attempt, dead.MaxAttempts)
	}
}

// TestTaskStaleConfigReject 验 stale_config：不计 attempt、不占租约、回 pending+退避。
func TestTaskStaleConfigReject(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "stale-config")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-1", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	now := store.Now()
	if err := db.RejectAssignment(ctx, task.ID, "asg-1", protocol.ReasonStaleConfig, now); err != nil {
		t.Fatalf("reject assignment: %v", err)
	}
	got := mustPhase(t, db, ns, task.ID, store.TaskPending)
	if got.Attempt != 0 {
		t.Fatalf("stale_config 不计 attempt，得到 %d", got.Attempt)
	}
	if got.WaitingReason != store.WaitingForConfig {
		t.Fatalf("waiting_reason = %q，want %q", got.WaitingReason, store.WaitingForConfig)
	}
	if got.NextAttemptAt == nil || *got.NextAttemptAt <= now {
		t.Fatalf("该有退避 next_attempt_at（> now=%d），得到 %v", now, got.NextAttemptAt)
	}
	if got.NodeID != nil || got.LeaseID != "" || got.LeaseUntil != nil {
		t.Fatalf("stale_config 应清掉节点/租约：node=%v lease=%q", got.NodeID, got.LeaseID)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("stale_config 不占槽位")
	}
	// 只有 stale_config 是合法理由。
	if err := db.RejectAssignment(ctx, task.ID, "asg-1", "boom", now); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("非 stale_config 应被拒，得到 %v", err)
	}
}

// TestTaskRenewLease 验续租与 ACK deadline。
func TestTaskRenewLease(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "renew")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	leaseID, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")

	base := store.Now()
	expiresAt, err := db.RenewLease(ctx, task.ID, attempt, leaseID, base)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if expiresAt != base+int64(protocol.LeaseTTL.Seconds()) {
		t.Fatalf("续租到期 = %d，want %d", expiresAt, base+int64(protocol.LeaseTTL.Seconds()))
	}
	if _, err := db.RenewLease(ctx, task.ID, attempt+1, leaseID, base); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 续租应被拒（ErrLeaseMismatch），得到 %v", err)
	}
	if _, err := db.RenewLease(ctx, task.ID, attempt, "lse_bogus", base); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("错 lease 续租应被拒（ErrLeaseMismatch），得到 %v", err)
	}
}

// TestTaskAttemptToken 验 attempt token 的权威校验与旧 token 拒绝。
func TestTaskAttemptToken(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "token")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)
	task := newTask(t, db, ns, c, "trace-1", "")
	leaseID, token, attempt := grant(t, db, task.ID, node.ID, "asg-1")

	got, err := db.VerifyAttemptToken(ctx, task.ID, token)
	if err != nil {
		t.Fatalf("verify current token: %v", err)
	}
	if got.ID != task.ID || got.Attempt != attempt {
		t.Fatalf("校验应返回权威行：%+v", got)
	}
	for _, bad := range []string{"", "tok_wrong", token + "x"} {
		if _, err := db.VerifyAttemptToken(ctx, task.ID, bad); !errors.Is(err, store.ErrAttemptTokenInvalid) {
			t.Fatalf("token %q 应被拒（ErrAttemptTokenInvalid），得到 %v", bad, err)
		}
	}
	// 结束当前 attempt → 下一轮：旧 token 必须被拒（换 attempt 就换 token）。
	if err := db.CompleteTask(ctx, task.ID, attempt, leaseID, store.TaskResult{Phase: store.TaskFailed, Reason: protocol.ReasonScriptFailed, Error: "boom"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := db.VerifyAttemptToken(ctx, task.ID, token); !errors.Is(err, store.ErrTaskNotRunning) {
		t.Fatalf("终态任务的 token 应报 ErrTaskNotRunning，得到 %v", err)
	}
}

// TestTaskCancelWaitsForReap 验取消：running 要等 processGroupReaped=true 才终态+释放槽位。
func TestTaskCancelWaitsForReap(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cancel")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	leaseID, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("grant 后应占一个槽")
	}

	if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	running, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get running task: %v", err)
	}
	if running.Phase != store.TaskRunning || running.CancelReason != protocol.ReasonCancelledByUser ||
		running.CancelRequestedAt == nil {
		t.Fatalf("running 上的取消只记意图、不改相位：%+v", running)
	}

	// reaped=false：不改相位、不释放槽位。
	if err := db.AckCancel(ctx, task.ID, attempt, false, store.Now()); err != nil {
		t.Fatalf("ack cancel (not reaped): %v", err)
	}
	if got := mustPhase(t, db, ns, task.ID, store.TaskRunning); got.FinishedAt != nil {
		t.Fatalf("未回收进程组不该置终态时间：%v", got.FinishedAt)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("未回收进程组不该释放槽位")
	}
	// 旧 attempt 的 ack 被拒。
	if err := db.AckCancel(ctx, task.ID, attempt+1, true, store.Now()); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 的 ack 应被拒，得到 %v", err)
	}

	if err := db.AckCancel(ctx, task.ID, attempt, true, store.Now()); err != nil {
		t.Fatalf("ack cancel (reaped): %v", err)
	}
	done := mustPhase(t, db, ns, task.ID, store.TaskCancelled)
	if done.Error != protocol.ReasonCancelledByUser || done.FinishedAt == nil {
		t.Fatalf("取消终态应带原因与时间：%+v", done)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("reaped=true 必须释放槽位")
	}
	_ = leaseID
}

// TestTaskCancelPendingIsTerminal 验 pending 取消直接终态（没有执行者，不用等 ack）。
func TestTaskCancelPendingIsTerminal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cancel-pending")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	task := newTask(t, db, ns, c, "trace-1", "")

	if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	got := mustPhase(t, db, ns, task.ID, store.TaskCancelled)
	if got.Error != protocol.ReasonCancelledByUser {
		t.Fatalf("取消终态该带原因，得到 %q", got.Error)
	}
	if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, store.Now()); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("二次取消应被拒，得到 %v", err)
	}
}

// TestTaskRecoverIdempotentRequeues 验幂等 + infra 失败可重试；非幂等绝不重跑。
func TestTaskRecoverIdempotentRequeues(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "recover")
	idem := lifeCycleCap(t, db, ns, "idem", true, 3)
	plain := lifeCycleCap(t, db, ns, "plain", false, 3)
	node := newNode(t, db, "node-a", 2)

	idemTask := newTask(t, db, ns, idem, "trace-1", "")
	grant(t, db, idemTask.ID, node.ID, "asg-1")
	plainTask := newTask(t, db, ns, plain, "trace-2", "")
	grant(t, db, plainTask.ID, node.ID, "asg-2")

	now := store.Now()
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id IN (?,?)`,
		now-1, idemTask.ID, plainTask.ID); err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	requeued, abandoned, err := db.RecoverLostLeases(ctx, now)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if requeued != 1 || abandoned != 1 {
		t.Fatalf("幂等应重排、非幂等应放弃：requeued=%d abandoned=%d", requeued, abandoned)
	}
	back := mustPhase(t, db, ns, idemTask.ID, store.TaskPending)
	if back.WaitingReason != store.WaitingRetryBackoff || back.NextAttemptAt == nil {
		t.Fatalf("重排应带 retry_backoff + next_attempt_at：%+v", back)
	}
	if back.Attempt != 1 {
		t.Fatalf("重排不该重置 attempt，得到 %d", back.Attempt)
	}
	if back.NodeID != nil || back.LeaseID != "" {
		t.Fatalf("重排应清掉上一代执行事实：node=%v lease=%q", back.NodeID, back.LeaseID)
	}
	dead := mustPhase(t, db, ns, plainTask.ID, store.TaskAbandoned)
	if dead.Error != protocol.ReasonLeaseExpired {
		t.Fatalf("非幂等过期应 abandoned + lease_expired，得到 %q", dead.Error)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("两条路径都必须释放槽位")
	}
	// 重排后的任务可以再次被派发（attempt 变 2）。
	if _, _, attempt, _, err := func() (string, string, int, int64, error) {
		if err := db.AssignTask(ctx, idemTask.ID, node.ID, "asg-3", 2); err != nil {
			t.Fatalf("re-assign: %v", err)
		}
		return db.GrantLease(ctx, idemTask.ID, "asg-3", node.ID, store.Now())
	}(); err != nil {
		t.Fatalf("re-grant: %v", err)
	} else if attempt != 2 {
		t.Fatalf("重派后 attempt 应为 2，得到 %d", attempt)
	}
}

// TestTaskCompleteReleasesSlot 验正常结束释放槽位，并写入结果字段。
func TestTaskCompleteReleasesSlot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "complete")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	leaseID, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")

	if err := db.CompleteTask(ctx, task.ID, attempt, leaseID, store.TaskResult{
		Phase: store.TaskSucceeded, Summary: "ok", Artifacts: []string{"out/report.json"},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got := mustPhase(t, db, ns, task.ID, store.TaskSucceeded)
	if got.Summary != "ok" || len(got.Artifacts) != 1 || !got.Reported {
		t.Fatalf("结果字段没写全：%+v", got)
	}
	if got.LeaseID != "" || got.LeaseUntil != nil {
		t.Fatalf("终态该清掉租约：%+v", got)
	}
	// attempt_token_hash 刻意保留（= 最后一次发出的令牌），让「令牌错」与「任务已结束」
	// 可判别；下一次 grant 会覆盖它。
	if got.AttemptTokenHash == "" {
		t.Fatalf("终态应保留最后一次令牌摘要（用于 401/409 判别）")
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("正常结束必须释放槽位")
	}
	// 旧 attempt 的 result 被拒。
	if err := db.CompleteTask(ctx, task.ID, attempt, leaseID, store.TaskResult{Phase: store.TaskSucceeded}); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("终态任务再收 result 应被拒，得到 %v", err)
	}
}

// TestTaskCancelRevokesAuthorization 是取消语义的**对抗用例**（centurion 裁决）：
//
//   - RequestCancel 一发起就立刻收回执行授权：VerifyAttemptToken / RenewLease / CompleteTask
//     全部被拒——不能出现「等待 reap 期间还能 emit、续租、写终态」的窗口；
//   - reaped=false 的 ack 只表示「收到了」，**槽位仍被占用**；
//   - 只有 reaped=true 才释放槽位并置终态。
func TestTaskCancelRevokesAuthorization(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cancel-adversarial")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	leaseID, token, attempt := grant(t, db, task.ID, node.ID, "asg-1")

	// 取消前：token/续租/结果都可用（基线）。
	if _, err := db.VerifyAttemptToken(ctx, task.ID, token); err != nil {
		t.Fatalf("取消前 token 应有效：%v", err)
	}
	if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	// 取消后：授权立刻收回。
	if _, err := db.VerifyAttemptToken(ctx, task.ID, token); !errors.Is(err, store.ErrTaskNotRunning) {
		t.Fatalf("取消后 token 必须被收回（ErrTaskNotRunning），得到 %v", err)
	}
	if _, err := db.RenewLease(ctx, task.ID, attempt, leaseID, store.Now()); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("取消后不该允许续租（ErrLeaseMismatch），得到 %v", err)
	}
	if err := db.CompleteTask(ctx, task.ID, attempt, leaseID, store.TaskResult{
		Phase: store.TaskSucceeded, Summary: "late",
	}); !errors.Is(err, store.ErrTaskNotRunning) {
		t.Fatalf("取消后 result 必须被拒（终态由 cancel.ack 裁定），得到 %v", err)
	}
	// 这些拒绝都不该改变任务状态或槽位。
	still := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	if still.FinishedAt != nil {
		t.Fatalf("等待 reap 期间不该有终态时间")
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("等待 reap 期间槽位必须仍被占用")
	}

	// reaped=false：仍然只是「收到了」。
	if err := db.AckCancel(ctx, task.ID, attempt, false, store.Now()); err != nil {
		t.Fatalf("ack (not reaped): %v", err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("reaped=false 不该释放槽位")
	}
	if err := db.AckCancel(ctx, task.ID, attempt+1, true, store.Now()); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 的 ack 应被拒，得到 %v", err)
	}

	// reaped=true：终态 + 释放槽位。
	if err := db.AckCancel(ctx, task.ID, attempt, true, store.Now()); err != nil {
		t.Fatalf("ack (reaped): %v", err)
	}
	done := mustPhase(t, db, ns, task.ID, store.TaskCancelled)
	if done.Error != protocol.ReasonCancelledByUser {
		t.Fatalf("取消终态该带原因，得到 %q", done.Error)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("reaped=true 必须释放槽位")
	}
}

// TestTaskTimerBackoff 验两档退避的形状（5s 起、上限分别 5m / 60s）。
func TestTaskBackoffCurves(t *testing.T) {
	cases := []struct {
		attempt int
		retry   int64
		stale   int64
	}{
		{1, 5, 5},
		{2, 10, 10},
		{3, 20, 20},
		{10, 300, 60},
	}
	for _, c := range cases {
		if got := store.RetryBackoff(c.attempt); got != c.retry {
			t.Fatalf("RetryBackoff(%d) = %d, want %d", c.attempt, got, c.retry)
		}
		if got := store.StaleConfigBackoff(c.attempt); got != c.stale {
			t.Fatalf("StaleConfigBackoff(%d) = %d, want %d", c.attempt, got, c.stale)
		}
	}
}

// TestTaskListRoundTrip 验字段往返与过滤/空列表。
func TestTaskListRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "list")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	task := newTask(t, db, ns, c, "trace-1", `{"a":1}`)

	if string(task.Params) != `{"a":1}` {
		t.Fatalf("params 往返失真：%s", task.Params)
	}
	if task.Artifacts == nil || len(task.Artifacts) != 0 {
		t.Fatalf("空列表必须是 []：%v", task.Artifacts)
	}
	all, err := db.ListTasks(ctx, ns, store.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("list tasks 应有一条，得到 %d", len(all))
	}
	byTrace, err := db.ListTasksByTrace(ctx, ns, "trace-1")
	if err != nil || len(byTrace) != 1 {
		t.Fatalf("list by trace: %v (%d)", err, len(byTrace))
	}
	if n, err := db.CountTasksByTrace(ctx, ns, "trace-1"); err != nil || n != 1 {
		t.Fatalf("count by trace: %v (%d)", err, n)
	}
	empty, err := db.ListTasks(ctx, ns, store.TaskFilter{Phase: store.TaskFailed})
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空列表必须是 []，得到 %v", empty)
	}
	// params 超限整体拒（一条不合格一条都不建）。
	big := make([]byte, store.MaxTaskParamsBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	err = db.CreateTasks(ctx, []*store.Task{{
		NamespaceID: ns, TraceID: "trace-big", CapabilityID: c.ID, Capability: c.Name,
		Version: c.Version, Params: append(append([]byte(`{"p":"`), big...), '"', '}'),
	}})
	if !errors.Is(err, store.ErrParamsTooLarge) {
		t.Fatalf("超限 params 应被拒（ErrParamsTooLarge），得到 %v", err)
	}
	// 建任务只能落 pending。
	err = db.CreateTasks(ctx, []*store.Task{{
		NamespaceID: ns, TraceID: "trace-bad", CapabilityID: c.ID, Capability: c.Name,
		Version: c.Version, Phase: store.TaskRunning,
	}})
	if err == nil {
		t.Fatalf("非 pending 建任务应被拒")
	}
}

// TestTaskMaxAttemptsThreeTotal 验「总尝试次数 = maxAttempts」：幂等能力也只在预算内重跑。
//
// 3 次 grant 是上限：第 4 次 grant 直接转终态（abandoned + max_attempts_exceeded），
// 不再授予租约。这保证「重试」不会变成无限循环。
func TestTaskMaxAttemptsThreeTotal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "attempt-budget")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")

	for attempt := 1; attempt <= 3; attempt++ {
		leaseID, _, got := grant(t, db, task.ID, node.ID, fmt.Sprintf("asg-%d", attempt))
		if got != attempt {
			t.Fatalf("第 %d 次 grant 的 attempt = %d", attempt, got)
		}
		// 用「租约过期」制造一次 infra 失败；非最后一次应当重排。
		if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
			t.Fatalf("expire lease: %v", err)
		}
		_, _, err := db.RecoverLostLeases(ctx, store.Now())
		if err != nil {
			t.Fatalf("recover after attempt %d: %v", attempt, err)
		}
		if attempt < 3 {
			back := mustPhase(t, db, ns, task.ID, store.TaskPending)
			if back.Attempt != attempt || back.WaitingReason != store.WaitingRetryBackoff {
				t.Fatalf("第 %d 次失败后应重排（attempt=%d）：%+v", attempt, attempt, back)
			}
			// 清掉退避，让下一次能立刻被派发。
			if err := db.AssignTask(ctx, task.ID, node.ID, fmt.Sprintf("asg-next-%d", attempt), attempt+1); err != nil {
				t.Fatalf("re-assign after attempt %d: %v", attempt, err)
			}
			if _, err := db.Exec(`UPDATE tasks SET next_attempt_at = NULL WHERE id = ?`, task.ID); err != nil {
				t.Fatalf("clear backoff: %v", err)
			}
		} else {
			dead := mustPhase(t, db, ns, task.ID, store.TaskAbandoned)
			if dead.Error != protocol.ReasonMaxAttemptsExceeded {
				t.Fatalf("预算用尽应 abandoned + max_attempts_exceeded，得到 %q", dead.Error)
			}
		}
		_ = leaseID
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("预算用尽后不该占槽位")
	}
}
