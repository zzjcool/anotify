package v4test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

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

	leaseID, token, attempt, expiresAt, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "asg-1", 1, 0, store.Now())
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
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "", 1, 0, store.Now()); !errors.Is(err, store.ErrInvalidTransition) {
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
	_, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "asg-old", 1, 0, store.Now())
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
	if err := db.RequestAbandon(ctx, task.ID, protocol.ReasonAbandonedByUser, store.Now()); err != nil {
		t.Fatalf("request abandon: %v", err)
	}
	if err := ackCancel(t, db, node.ID, task.ID, attempt, true); err != nil {
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
	expiresAt, err := db.RenewLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, attempt, leaseID, base)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if expiresAt != base+int64(protocol.LeaseTTL.Seconds()) {
		t.Fatalf("续租到期 = %d，want %d", expiresAt, base+int64(protocol.LeaseTTL.Seconds()))
	}
	if _, err := db.RenewLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, attempt+1, leaseID, base); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 续租应被拒（ErrLeaseMismatch），得到 %v", err)
	}
	if _, err := db.RenewLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, attempt, "lse_bogus", base); !errors.Is(err, store.ErrLeaseMismatch) {
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
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, leaseID, 0, store.TaskResult{Phase: store.TaskFailed, Reason: protocol.ReasonScriptFailed, Error: "boom"}); err != nil {
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
	if err := ackCancel(t, db, node.ID, task.ID, attempt, false); err != nil {
		t.Fatalf("ack cancel (not reaped): %v", err)
	}
	if got := mustPhase(t, db, ns, task.ID, store.TaskRunning); got.FinishedAt != nil {
		t.Fatalf("未回收进程组不该置终态时间：%v", got.FinishedAt)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("未回收进程组不该释放槽位")
	}
	// 旧 attempt 的 ack 被拒。
	if err := db.AckCancelAuthorized(ctx, nodeAuthByID(t, db, node.ID), store.ReapedInputs{TaskID: task.ID, Attempt: attempt + 1, NodeID: node.ID, LeaseID: leaseID, Slot: 0}, true); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 的 ack 应被拒，得到 %v", err)
	}

	if err := ackCancel(t, db, node.ID, task.ID, attempt, true); err != nil {
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
	// 显式模拟**真实 offline 事实**（会话死亡）：它只说明连接断了，不证明进程组已回收，
	// 所以 busy 账本必须保留（容量不可派发，但不造假 reaped）。
	if err := db.MarkNodeOffline(ctx, node.ID, now); err != nil {
		t.Fatalf("mark node offline: %v", err)
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
	// offline 不能当物理回收事实：两个 attempt 的槽必须都还记着。
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("offline 不是进程回收事实：busy 账本必须保留，得到 %d", busySlots(t, db, node.ID))
	}
	// 节点回来并明确声明两个槽都空闲：这才是可信回收事实。
	facts := idleObservations(t, db, node.ID)
	if len(facts) != 2 {
		t.Fatalf("应有 2 个空闲事实，得到 %d", len(facts))
	}
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, facts, now); rerr != nil || n != 2 {
		t.Fatalf("明确空闲事实应回收两个槽：n=%d err=%v", n, rerr)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("明确回收后仍有 %d 个 busy 槽位", busySlots(t, db, node.ID))
	}
	// 节点回来后再派发：重排后的任务可以再次被授权（attempt 变 2）。
	if err := db.UpdateNodeStatus(ctx, node.ID, store.NodeStatusOnline); err != nil {
		t.Fatalf("mark node online: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, node.ID, []store.InstanceSpec{{Slot: 0}, {Slot: 1}}); err != nil {
		t.Fatalf("re-declare instances: %v", err)
	}
	if _, _, attempt, _, err := func() (string, string, int, int64, error) {
		if err := db.AssignTask(ctx, idemTask.ID, node.ID, "asg-3", 2); err != nil {
			t.Fatalf("re-assign: %v", err)
		}
		return db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), idemTask.ID, "asg-3", 2, 0, store.Now())
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
	leaseID, uploadToken, attempt := grant(t, db, task.ID, node.ID, "asg-1")
	refs := registerTestArtifact(t, db, store.EmitAuth{TaskID: task.ID, NodeID: node.ID, NodeTokenHash: node.TokenHash, Attempt: attempt, LeaseID: leaseID, AttemptToken: uploadToken}, "out/report.json", 1)

	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, leaseID, 0, store.TaskResult{
		Phase: store.TaskSucceeded, Summary: "ok", Artifacts: refs,
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
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, leaseID, 0, store.TaskResult{Phase: store.TaskSucceeded}); !errors.Is(err, store.ErrInvalidTransition) {
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
	if _, err := db.RenewLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, attempt, leaseID, store.Now()); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("取消后不该允许续租（ErrLeaseMismatch），得到 %v", err)
	}
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, leaseID, 0, store.TaskResult{
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
	if err := ackCancel(t, db, node.ID, task.ID, attempt, false); err != nil {
		t.Fatalf("ack (not reaped): %v", err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("reaped=false 不该释放槽位")
	}
	if err := db.AckCancelAuthorized(ctx, nodeAuthByID(t, db, node.ID), store.ReapedInputs{TaskID: task.ID, Attempt: attempt + 1, NodeID: node.ID, LeaseID: leaseID, Slot: 0}, true); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 的 ack 应被拒，得到 %v", err)
	}

	// reaped=true：终态 + 释放槽位。
	if err := ackCancel(t, db, node.ID, task.ID, attempt, true); err != nil {
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
		// 用「租约过期 + 节点真的掉了」制造一次 infra 失败：非最后一次应当重排。
		// 物理槽位不再因 offline / phase 变化被回收（那只是会话死亡，不是进程事实）：
		// 只有节点重连后**明确**申报「槽位空闲」才允许清。
		if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
			t.Fatalf("expire lease: %v", err)
		}
		if uerr := db.MarkNodeOffline(ctx, node.ID, store.Now()); uerr != nil {
			t.Fatalf("mark node offline after attempt %d: %v", attempt, uerr)
		}
		if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
			t.Fatalf("recover after attempt %d: %v", attempt, err)
		}
		if busySlots(t, db, node.ID) != 1 {
			t.Fatalf("offline 只说明连接断了，不能证明进程组已回收：busy 必须保留（attempt %d），得到 %d",
				attempt, busySlots(t, db, node.ID))
		}
		// 重连并明确申报「槽 0 此刻空闲」才是物理回收事实。
		if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, idleObservations(t, db, node.ID, 0), store.Now()); rerr != nil || n != 1 {
			t.Fatalf("明确空闲事实应回收槽位（attempt %d）：n=%d err=%v", attempt, n, rerr)
		}
		if busySlots(t, db, node.ID) != 0 {
			t.Fatalf("明确回收后不该再有 busy 槽位（attempt %d）", attempt)
		}
		if uerr := db.UpdateNodeStatus(ctx, node.ID, store.NodeStatusOnline); uerr != nil {
			t.Fatalf("mark node online after attempt %d: %v", attempt, uerr)
		}
		if _, uerr := db.UpsertInstances(ctx, node.ID, []store.InstanceSpec{{Slot: 0}}); uerr != nil {
			t.Fatalf("re-declare instances after attempt %d: %v", attempt, uerr)
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

// TestTaskOnlineLeaseExpiryKeepsSlotUntilNodeFact 验**在线节点**的租约过期只吊销授权：
// 物理槽位不被巡检「假 reaped」清掉（phase 不是物理事实），只有节点明确申报实例空闲
// （ReclaimStaleSlots 的事实）才回收。
func TestTaskOnlineLeaseExpiryKeepsSlotUntilNodeFact(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "slot-online")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	grant(t, db, task.ID, node.ID, "asg-1")

	now := store.Now()
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, now-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	requeued, terminal, err := db.RecoverLostLeases(ctx, now)
	if err != nil || requeued != 1 || terminal != 0 {
		t.Fatalf("recover: requeued=%d terminal=%d err=%v", requeued, terminal, err)
	}
	mustPhase(t, db, ns, task.ID, store.TaskPending)
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("在线节点的 lease 过期不该释放物理槽位（上一代进程可能还在跑），得到 busy=%d",
			busySlots(t, db, node.ID))
	}
	// 节点重新上报（hello）本身**不是** reaped 事实：任务已不在 running 不得拿来推断进程已死。
	n, err := db.ReclaimStaleSlots(ctx, node.ID, nil, now)
	if err != nil || n != 0 {
		t.Fatalf("没有实例事实时不得回收（rehello != reaped）：n=%d err=%v", n, err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("无实例事实时槽位必须保持占用")
	}
	// 节点明确申报「槽 0 此刻没有进程」才是可信回收事实。
	n, err = db.ReclaimStaleSlots(ctx, node.ID, idleObservations(t, db, node.ID, 0), now)
	if err != nil || n != 1 {
		t.Fatalf("reclaim stale slots = %d, err=%v（want 1）", n, err)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("明确实例事实回收后不该再有 busy 槽位")
	}
}

// TestTaskCancelSlotHeldUntilReapOrOfflineFact 验取消后的槽位：
//   - 在线 + reaped 未确认 → 终局也不释放（reaped=false / 租约巡检都不放）；
//   - 节点 offline 只说明会话断了（busy 账本保留、容量不可派发），不自动回收；
//   - 确认 reap（reaped=true）或明确实例空闲事实 → 释放。
func TestTaskCancelSlotHeldUntilReapOrOfflineFact(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "slot-cancel")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)

	// ① 在线 + 未 reap：巡检把取消收成终局，但槽位保持占用。
	online := newNode(t, db, "node-online", 1)
	taskA := newTask(t, db, ns, c, "trace-a", "")
	_, _, attemptA := grant(t, db, taskA.ID, online.ID, "asg-a")
	if err := db.RequestCancel(ctx, taskA.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("cancel A: %v", err)
	}
	if err := ackCancel(t, db, online.ID, taskA.ID, attemptA, false); err != nil {
		t.Fatalf("ack A (not reaped): %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, taskA.ID); err != nil {
		t.Fatalf("expire A: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover A: %v", err)
	}
	doneA := mustPhase(t, db, ns, taskA.ID, store.TaskCancelled)
	if doneA.Error != protocol.ReasonCancelledByUser || doneA.Attempt != attemptA {
		t.Fatalf("取消终局应保留用户原因/attempt：%+v", doneA)
	}
	if busySlots(t, db, online.ID) != 1 {
		t.Fatalf("reaped 未确认时槽位必须保持占用，得到 busy=%d", busySlots(t, db, online.ID))
	}
	// 确认 reap 才释放。
	if err := ackCancel(t, db, online.ID, taskA.ID, attemptA, true); err != nil {
		t.Fatalf("ack A (reaped): %v", err)
	}
	if busySlots(t, db, online.ID) != 0 {
		t.Fatalf("reaped=true 后必须释放槽位")
	}

	// ② 真 offline（会话死亡）：不自动回收，busy 账本保留、容量不可派发。
	offline := newNode(t, db, "node-offline", 1)
	taskB := newTask(t, db, ns, c, "trace-b", "")
	_, _, attemptB := grant(t, db, taskB.ID, offline.ID, "asg-b")
	if err := db.RequestCancel(ctx, taskB.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("cancel B: %v", err)
	}
	if err := ackCancel(t, db, offline.ID, taskB.ID, attemptB, false); err != nil {
		t.Fatalf("ack B (not reaped): %v", err)
	}
	if err := db.MarkNodeOffline(ctx, offline.ID, store.Now()); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, taskB.ID); err != nil {
		t.Fatalf("expire B: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover B: %v", err)
	}
	mustPhase(t, db, ns, taskB.ID, store.TaskCancelled)
	if busySlots(t, db, offline.ID) != 1 {
		t.Fatalf("offline 不是进程回收事实：busy 账本必须保留，得到 %d", busySlots(t, db, offline.ID))
	}
	if _, err := db.LeaseSlot(ctx, offline.ID, "tsk_other", 1, "lse_other", 0); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("offline 节点不可派发（容量必须为 0），得到 %v", err)
	}
	// 节点回来并明确申报槽位空闲才释放。
	if n, rerr := db.ReclaimStaleSlots(ctx, offline.ID, idleObservations(t, db, offline.ID, 0), store.Now()); rerr != nil || n != 1 {
		t.Fatalf("明确空闲事实应回收（n=%d err=%v）", n, rerr)
	}
	if busySlots(t, db, offline.ID) != 0 {
		t.Fatalf("明确回收后不该再有 busy 槽位")
	}
}

// TestTaskOldAckCannotReleaseNewAttemptSlot 验**旧 attempt 的回收事实不能碰新 attempt 的槽**：
//
// 同一逻辑任务重排后可能同时占着旧 attempt 的槽（旧进程未回收）与新 attempt 的槽。
// 旧槽的回收必须走带节点身份 + 历史 lease/instance/slot 核对的历史入口：
// 只清它自己那一条绑定，不改新 attempt 的相位/token/槽，重复回收幂等。
func TestTaskOldAckCannotReleaseNewAttemptSlot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "slot-oldack")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)
	task := newTask(t, db, ns, c, "trace-1", "")

	lease1, _, attempt1 := grant(t, db, task.ID, node.ID, "asg-1")
	first, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	slot1 := first.LeaseInstanceID
	if slot1 == "" {
		t.Fatalf("grant 必须把这次 attempt 绑定的物理槽位记在任务行上（lease_instance_id）")
	}
	att1, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get attempt 1: %v", err)
	}
	if att1.LeaseID != lease1 || att1.InstanceID != slot1 || att1.NodeID != node.ID {
		t.Fatalf("attempt 行必须留下历史绑定：%+v", att1)
	}

	// 租约过期 → 重排（在线节点：旧槽位保持占用）→ 再授权第二个 attempt。
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET next_attempt_at = NULL WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-2", attempt1+1); err != nil {
		t.Fatalf("re-assign: %v", err)
	}
	lease2, _, attempt2, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "asg-2", 2, freeSlot(t, db, node.ID), store.Now())
	if err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	if attempt2 != 2 {
		t.Fatalf("第二次 attempt = %d", attempt2)
	}
	second, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task after re-grant: %v", err)
	}
	if second.LeaseInstanceID == slot1 {
		t.Fatalf("第二次 grant 应绑到另一个槽位（旧槽仍被 attempt1 占着）")
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("两个 attempt 各占一个槽，得到 busy=%d", busySlots(t, db, node.ID))
	}

	// 旧 attempt 的 ACK 必须带认证身份 + 精确历史绑定：错 lease 一律 fail closed，
	// 一个槽都不放（尤其不能碰新 attempt 的槽）。
	if err := db.AckCancelAuthorized(ctx, nodeAuthByID(t, db, node.ID), store.ReapedInputs{
		TaskID: task.ID, Attempt: attempt1, NodeID: node.ID, LeaseID: "lse_bogus",
		Slot: att1.Slot, InstanceID: slot1,
	}, true); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 的错绑定 reap ACK 应 ErrLeaseMismatch，得到 %v", err)
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("旧 ACK 不得释放新 attempt 的槽（也不得释放旧槽），得到 busy=%d", busySlots(t, db, node.ID))
	}

	// 带身份 + 历史绑定的回收：只清旧 attempt 自己那个槽。
	reapIn := store.ReapedInputs{
		TaskID: task.ID, Attempt: attempt1,
		NodeID: node.ID, NodeTokenHash: node.TokenHash,
		LeaseID: lease1, InstanceID: slot1, Slot: att1.Slot,
	}
	reaped, err := db.ReapTaskAttempt(ctx, reapIn)
	if err != nil || !reaped {
		t.Fatalf("历史回收应成功：reaped=%v err=%v", reaped, err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("旧 attempt 的回收只该释放它自己的槽，剩 busy=%d", busySlots(t, db, node.ID))
	}
	nowRunning := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	if nowRunning.Attempt != attempt2 || nowRunning.LeaseID != lease2 {
		t.Fatalf("旧回收不得改动新 attempt 的相位/token：%+v", nowRunning)
	}
	att2, err := db.GetTaskAttempt(ctx, task.ID, attempt2)
	if err != nil || att2.ReapedAt != nil {
		t.Fatalf("新 attempt 不应被标记 reaped：%+v err=%v", att2, err)
	}
	// 重复回收幂等：不再写事实、不报错。
	if again, err := db.ReapTaskAttempt(ctx, reapIn); err != nil || again {
		t.Fatalf("重复回收应幂等：reaped=%v err=%v", again, err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("重复回收不得再动槽位，busy=%d", busySlots(t, db, node.ID))
	}

	// 当前 attempt 取消 + 确认 reap：只释放它自己绑定的那个槽。
	if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := ackCancel(t, db, node.ID, task.ID, attempt2, true); err != nil {
		t.Fatalf("ack current attempt: %v", err)
	}
	mustPhase(t, db, ns, task.ID, store.TaskCancelled)
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("两个 attempt 都已回收，剩 busy=%d", busySlots(t, db, node.ID))
	}
}

// TestTaskCancelInFlightNeverRegranted 验取消在飞的任务**永不再授权**（F1 反例）：
// 租约过期/巡检不会把它重排成 pending 再 grant，且终局原因是取消（不是 lease_expired）。
func TestTaskCancelInFlightNeverRegranted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cancel-regrant")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	_, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")

	if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	requeued, terminal, err := db.RecoverLostLeases(ctx, store.Now())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if requeued != 0 || terminal != 1 {
		t.Fatalf("取消在飞的任务不得重排：requeued=%d terminal=%d", requeued, terminal)
	}
	done := mustPhase(t, db, ns, task.ID, store.TaskCancelled)
	if done.Error != protocol.ReasonCancelledByUser {
		t.Fatalf("终局原因必须是用户取消，得到 %q（lease_expired 是错的）", done.Error)
	}
	if done.Attempt != attempt {
		t.Fatalf("取消终局不得产生新 attempt：%d != %d", done.Attempt, attempt)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("reaped 未确认前槽位必须保持占用")
	}

	// 永不进派发候选、永不再授权。
	now := store.Now()
	dispatchable, err := db.ListDispatchableTasks(ctx, now+3600, 100)
	if err != nil {
		t.Fatalf("list dispatchable: %v", err)
	}
	if taskByID(dispatchable, task.ID) != nil {
		t.Fatalf("已取消的任务不能再进派发候选")
	}
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-2", 2); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("取消后 assign 应被拒，得到 %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "", 1, 0, now); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("取消后 grant 应被拒，得到 %v", err)
	}
	// 再巡检一轮也不会有新动作。
	if rq, term, err := db.RecoverLostLeases(ctx, now+60); err != nil || rq != 0 || term != 0 {
		t.Fatalf("第二轮巡检不该再处理它：rq=%d term=%d err=%v", rq, term, err)
	}
	// 迟到的 reap ACK 仍可补上槽位回收（终局不变）。
	if err := ackCancel(t, db, node.ID, task.ID, attempt, true); err != nil {
		t.Fatalf("reap ack after terminal: %v", err)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("确认 reap 后槽位应释放")
	}
}

// TestTaskCancelGrantRace 验取消与 grant 的竞争可线性化（不靠 sleep，用屏障并发）：
//
// 两种赢法都必须自洽——取消先赢 → 终局且永不授权、无槽位；
// grant 先赢 → running + 取消意图 + 槽位占用，等 reap。绝不出现「取消后还在跑且无取消标记」。
func TestTaskCancelGrantRace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cancel-race")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)

	for i := 0; i < 8; i++ {
		task := newTask(t, db, ns, c, fmt.Sprintf("trace-rc-%d", i), "")
		assignID := fmt.Sprintf("asg-rc-%d", i)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var (
			cancelErr error
			grantErr  error
			attempt   int
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			cancelErr = db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, store.Now())
		}()
		go func() {
			defer wg.Done()
			<-start
			if aerr := db.AssignTask(ctx, task.ID, node.ID, assignID, 1); aerr != nil {
				grantErr = aerr
				return
			}
			_, _, attempt, _, grantErr = db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, assignID, 1, 0, store.Now())
		}()
		close(start)
		wg.Wait()

		if cancelErr != nil {
			t.Fatalf("round %d: 取消不该失败（task 只可能被自己的取消终结）：%v", i, cancelErr)
		}
		got, err := db.GetTask(ctx, ns, task.ID)
		if err != nil {
			t.Fatalf("round %d: get task: %v", i, err)
		}
		switch got.Phase {
		case store.TaskCancelled:
			// 取消先赢：grant 必须失败，且没有占槽、没有 attempt。
			if grantErr == nil {
				t.Fatalf("round %d: 取消先赢时 grant 不该成功", i)
			}
			if got.Attempt != 0 {
				t.Fatalf("round %d: 取消先赢时不该有 attempt：%d", i, got.Attempt)
			}
			if busySlots(t, db, node.ID) != 0 {
				t.Fatalf("round %d: 取消先赢时不该占槽位", i)
			}
			if got.Error != protocol.ReasonCancelledByUser {
				t.Fatalf("round %d: 终局原因 = %q", i, got.Error)
			}
		case store.TaskRunning:
			// grant 先赢：必须带取消意图、槽位占用，结果/续租都已被收回。
			if grantErr != nil {
				t.Fatalf("round %d: grant 先赢却报错 %v", i, grantErr)
			}
			if got.CancelRequestedAt == nil {
				t.Fatalf("round %d: grant 先赢后取消必须留下意图（否则运行中的任务无法被停）", i)
			}
			if busySlots(t, db, node.ID) != 1 {
				t.Fatalf("round %d: running + 取消在飞时槽位必须仍占用", i)
			}
			if err := ackCancel(t, db, node.ID, task.ID, attempt, true); err != nil {
				t.Fatalf("round %d: ack reap: %v", i, err)
			}
			mustPhase(t, db, ns, task.ID, store.TaskCancelled)
			if busySlots(t, db, node.ID) != 0 {
				t.Fatalf("round %d: reap 后必须释放槽位", i)
			}
		default:
			t.Fatalf("round %d: 不合法终态 %q（取消后不得留下 pending）", i, got.Phase)
		}
		// 取消之后永不再授权（两种赢法都一样）。
		if err := db.AssignTask(ctx, task.ID, node.ID, "asg-late", 2); !errors.Is(err, store.ErrInvalidTransition) {
			t.Fatalf("round %d: 取消后 assign 应被拒，得到 %v", i, err)
		}
	}
}

// TestTaskGrantBudgetExhaustedReleasesSlot 验 grant 预算用尽分支**不泄漏槽位**（F3）：
//
// 占槽在事务外先发生；预算用尽直接终态，必须同事务把 busy_task 还回去，
// 否则那个槽位要等到节点下次 hello 才被兜底清掉。
func TestTaskGrantBudgetExhaustedReleasesSlot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "grant-budget")
	c := lifeCycleCap(t, db, ns, "idem", true, 1)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	// 制造「attempt 已用掉一次、任务又回到 pending」的真实形状（能力预算被改小后重排）。
	if _, err := db.Exec(`UPDATE tasks SET attempt = 1 WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-1", 2); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "asg-1", 2, 0, store.Now()); !errors.Is(err, store.ErrMaxAttemptsExceeded) {
		t.Fatalf("预算用尽应 ErrMaxAttemptsExceeded，得到 %v", err)
	}
	dead := mustPhase(t, db, ns, task.ID, store.TaskAbandoned)
	if dead.Error != protocol.ReasonMaxAttemptsExceeded {
		t.Fatalf("终态原因 = %q", dead.Error)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("预算用尽分支必须归还槽位，仍占 %d 个", busySlots(t, db, node.ID))
	}
}

// TestTaskDrainingNotLost 验 drain 语义：停止新派发，但 running 续租/结果不受影响（F2）。
func TestTaskDrainingNotLost(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "drain")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)
	running := newTask(t, db, ns, c, "trace-run", "")
	leaseID, _, attempt := grant(t, db, running.ID, node.ID, "asg-1")

	if err := db.UpdateNodeStatus(ctx, node.ID, store.NodeStatusDraining); err != nil {
		t.Fatalf("mark draining: %v", err)
	}
	// 巡检不把 draining 当失联。
	if rq, term, err := db.RecoverLostLeases(ctx, store.Now()); err != nil || rq != 0 || term != 0 {
		t.Fatalf("draining 不是 lost：requeued=%d terminal=%d err=%v", rq, term, err)
	}
	mustPhase(t, db, ns, running.ID, store.TaskRunning)
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("draining 不该动在跑任务的槽位")
	}
	// 在跑的任务照常续租、照常回报结果。
	if _, err := db.RenewLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), running.ID, attempt, leaseID, store.Now()); err != nil {
		t.Fatalf("draining 节点上的 running 任务应可续租：%v", err)
	}
	// 但不接新任务：选点（EligibleNodes）与 assign/grant 都不行。
	next := newTask(t, db, ns, c, "trace-next", "")
	eligible, reasons, err := db.EligibleNodesForTask(ctx, next)
	if err != nil {
		t.Fatalf("eligible for task: %v", err)
	}
	if len(eligible) != 0 {
		t.Fatalf("draining 节点不该是派发候选：%v", eligible)
	}
	if reason := store.NodeAvailabilityReason(reasons); reason != protocol.ConditionReasonNodeOffline {
		t.Fatalf("draining 节点的不可用原因 = %q，want NodeOffline", reason)
	}
	if err := db.AssignTask(ctx, next.ID, node.ID, "asg-2", 1); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("draining 节点不该接受新 assign，得到 %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), next.ID, "", 1, 0, store.Now()); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("draining 节点不该接受新 grant，得到 %v", err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("被拒的 grant 不该留下槽位")
	}
	// 结果仍然写得进去（drain 不影响存量）。
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, running.ID, attempt, leaseID, 0, store.TaskResult{
		Phase: store.TaskSucceeded, Summary: "drained-ok",
	}); err != nil {
		t.Fatalf("draining 节点上 running 任务的 result 应可写：%v", err)
	}
	mustPhase(t, db, ns, running.ID, store.TaskSucceeded)

	// 恢复 online 后可以接新任务。
	if err := db.UpdateNodeStatus(ctx, node.ID, store.NodeStatusOnline); err != nil {
		t.Fatalf("mark online: %v", err)
	}
	if err := db.AssignTask(ctx, next.ID, node.ID, "asg-3", 1); err != nil {
		t.Fatalf("online 后应可 assign：%v", err)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), next.ID, "asg-3", 1, freeSlot(t, db, node.ID), store.Now()); err != nil || got != 1 {
		t.Fatalf("online 后应可 grant：attempt=%d err=%v", got, err)
	}
}

// TestTaskCompleteRejectsOldAttemptLeaseAndExpiry 验 result 的权威校验（F5）：
// 旧 attempt / 错 lease / **过期租约**都必须拒，且不改任务事实、不释放槽位。
func TestTaskCompleteRejectsOldAttemptLeaseAndExpiry(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "result-auth")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	leaseID, token, attempt, expiresAt, err := func() (string, string, int, int64, error) {
		if aerr := db.AssignTask(ctx, task.ID, node.ID, "asg-1", 1); aerr != nil {
			t.Fatalf("assign: %v", aerr)
		}
		return db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "asg-1", 1, 0, store.Now())
	}()
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	ok := store.TaskResult{Phase: store.TaskSucceeded, Summary: "ok"}

	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt+1, leaseID, store.Now(), ok); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 的 result 应 ErrLeaseMismatch，得到 %v", err)
	}
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, "lse_bogus", store.Now(), ok); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("错 lease 的 result 应 ErrLeaseMismatch，得到 %v", err)
	}
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, leaseID, expiresAt+1, ok); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("过期租约的 result 应 ErrLeaseMismatch，得到 %v", err)
	}
	still := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	if still.FinishedAt != nil || still.Reported {
		t.Fatalf("被拒的 result 不该改终态事实：%+v", still)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("被拒的 result 不该释放槽位")
	}
	// 过期租约的 token 也不再被接受（VerifyAttemptToken 不得声称授权却漏到期）。
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, err := db.VerifyAttemptToken(ctx, task.ID, token); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("过期租约的 token 应 ErrLeaseMismatch，得到 %v", err)
	}
	// 恢复租约后，有效窗口内的 result 正常落库。
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, expiresAt, task.ID); err != nil {
		t.Fatalf("restore lease: %v", err)
	}
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, leaseID, expiresAt-1, ok); err != nil {
		t.Fatalf("有效 result 应成功：%v", err)
	}
	mustPhase(t, db, ns, task.ID, store.TaskSucceeded)
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("正常结束应释放槽位")
	}
}

// TestTaskParentNamespaceRoundTrip 验 parentTaskId 的往返与跨 ns 拒绝（不是 attempt 父子）。
func TestTaskParentNamespaceRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "parent-ns")
	other := newNS(t, db, "parent-other")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	parent := newTask(t, db, ns, c, "trace-parent", "")

	// 子任务不给 ns：继承父任务的命名空间（I11④）。
	child := &store.Task{
		TraceID:      "trace-child",
		CapabilityID: c.ID,
		Capability:   c.Name,
		Version:      c.Version,
		Digest:       c.Digest,
		ParentTaskID: &parent.ID,
	}
	if err := db.CreateTasks(ctx, []*store.Task{child}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	if child.NamespaceID != ns {
		t.Fatalf("子任务应继承父 ns：%q != %q", child.NamespaceID, ns)
	}
	got, err := db.GetTask(ctx, ns, child.ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if got.ParentTaskID == nil || *got.ParentTaskID != parent.ID {
		t.Fatalf("parentTaskId 往返失真：%v", got.ParentTaskID)
	}

	// 给了不同的 ns：拒绝（不静默修正），且一条都不建。
	cross := &store.Task{
		NamespaceID:  other,
		TraceID:      "trace-cross",
		CapabilityID: c.ID,
		Capability:   c.Name,
		Version:      c.Version,
		Digest:       c.Digest,
		ParentTaskID: &parent.ID,
	}
	err = db.CreateTasks(ctx, []*store.Task{cross})
	var crossErr *store.CrossNamespaceError
	if !errors.As(err, &crossErr) {
		t.Fatalf("跨 ns 的 parentTaskId 应 *CrossNamespaceError，得到 %v", err)
	}
	if crossErr.Want != ns {
		t.Fatalf("CrossNamespaceError.Want = %q, want %q", crossErr.Want, ns)
	}
	otherTasks, err := db.ListTasks(ctx, other, store.TaskFilter{})
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(otherTasks) != 0 {
		t.Fatalf("被拒的批次不该建行，other ns 有 %d 行", len(otherTasks))
	}
}

// TestTaskReapOfRequeuedAttemptReleasesOnlyItsSlot 验**重排后**旧 attempt 的确认 reap
// 只释放它自己绑定的那个槽（不按 task_id 清、不改相位、不动后续 attempt 的可能绑定）。
func TestTaskReapOfRequeuedAttemptReleasesOnlyItsSlot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "slot-requeue-reap")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")

	_, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	requeued := mustPhase(t, db, ns, task.ID, store.TaskPending)
	if requeued.NodeID != nil {
		t.Fatalf("重排应清掉 node 归属：%v", *requeued.NodeID)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("在线节点的槽位在 reap 前必须保持占用")
	}

	// 旧 attempt 自己的「进程组已回收」事实到达：只释放那个槽，任务仍是 pending。
	if err := ackCancel(t, db, node.ID, task.ID, attempt, true); err != nil {
		t.Fatalf("reap of requeued attempt: %v", err)
	}
	after := mustPhase(t, db, ns, task.ID, store.TaskPending)
	if after.Attempt != attempt || after.FinishedAt != nil {
		t.Fatalf("reap 不该改任务相位/attempt：%+v", after)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("确认 reap 后槽位应释放")
	}
}

// TestGrantRechecksNodeAuthorization closes assign -> grant revocation, including
// matching applied config: a valid machine hash is not namespace permission.
func TestGrantRechecksNodeAuthorization(t *testing.T) {
	for _, change := range []string{"authorized", "authorized-budget-spent", "declared", "offline", "wrong-node", "missing-node"} {
		t.Run(change, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			ns := newNS(t, db, "grant-auth")
			c := lifeCycleCap(t, db, ns, "idem", true, 3)
			node := newNode(t, db, "node-a", 1)
			task := newTask(t, db, ns, c, "trace-auth", "")
			if err := db.AssignTask(ctx, task.ID, node.ID, "asg-auth", 1); err != nil {
				t.Fatal(err)
			}
			grantNode := node.ID
			switch change {
			case "authorized", "authorized-budget-spent":
				if change == "authorized-budget-spent" {
					if _, err := db.Exec(`UPDATE tasks SET attempt = max_attempts WHERE id = ?`, task.ID); err != nil {
						t.Fatal(err)
					}
				}
				putNodeCfg(t, db, node.ID, []string{"other"}, nil, map[string]any{"total": 1})
				recordAppliedForNode(t, db, node.ID)
			case "declared":
				declareHello(t, db, node.ID, []string{"other"}, nil, nil)
				recordAppliedForNode(t, db, node.ID)
			case "offline":
				if err := db.MarkNodeOffline(ctx, node.ID, store.Now()); err != nil {
					t.Fatal(err)
				}
			case "wrong-node":
				grantNode = newNode(t, db, "node-b", 1).ID
			case "missing-node":
				grantNode = "nd_missing"
			}
			before, err := db.GetTask(ctx, ns, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			authForNode := store.NodeAuthorization{NodeID: grantNode, NodeTokenHash: "missing-node-token-hash"}
			if n, nerr := db.GetNode(ctx, grantNode); nerr == nil {
				authForNode.NodeTokenHash = n.TokenHash
			}
			if lease, token, attempt, until, err := db.GrantLeaseAuthorized(ctx, authForNode, task.ID, "asg-auth", 1, 0, store.Now()); err == nil || lease != "" || token != "" || attempt != 0 || until != 0 {
				t.Fatalf("revoked grant must fail without authorization: lease=%q attempt=%d until=%d err=%v", lease, attempt, until, err)
			}
			after, err := db.GetTask(ctx, ns, task.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected grant mutated task: before=%+v after=%+v err=%v", before, after, err)
			}
			attempts, err := db.ListTaskAttempts(ctx, task.ID)
			if err != nil || len(attempts) != 0 || busySlots(t, db, node.ID) != 0 {
				t.Fatalf("rejected grant wrote snapshot/occupancy: attempts=%v err=%v", attempts, err)
			}
		})
	}
}

func TestStaleGrantAndRejectPreserveNewAssignment(t *testing.T) {
	for _, operation := range []string{"grant-old", "grant-budget-spent", "grant-empty", "reject-old", "reject-empty", "reject-no-current"} {
		t.Run(operation, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			ns := newNS(t, db, "assignment-cas")
			c := lifeCycleCap(t, db, ns, "idem", true, 3)
			node := newNode(t, db, "node-a", 1)
			task := newTask(t, db, ns, c, "trace-cas", "")
			now := store.Now()
			if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-old", 1, now); err != nil {
				t.Fatal(err)
			}
			if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-new", 2, now+1); err != nil {
				t.Fatal(err)
			}
			if operation == "grant-budget-spent" {
				if _, err := db.Exec(`UPDATE tasks SET attempt = max_attempts WHERE id = ?`, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if operation == "reject-no-current" {
				if err := db.RejectAssignment(ctx, task.ID, "asg-new", "stale_config", now+2); err != nil {
					t.Fatal(err)
				}
			}
			before, err := db.GetTask(ctx, ns, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			assignment := "asg-old"
			if operation == "grant-empty" || operation == "reject-empty" {
				assignment = ""
			}
			if operation == "grant-old" || operation == "grant-empty" || operation == "grant-budget-spent" {
				_, _, _, _, err = db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, assignment, 1, 0, now+2)
			} else {
				err = db.RejectAssignment(ctx, task.ID, assignment, "stale_config", now+2)
			}
			if !errors.Is(err, store.ErrNoAssignment) {
				t.Fatalf("stale/empty identity must fail closed: %v", err)
			}
			after, err := db.GetTask(ctx, ns, task.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("stale request changed newer assignment: before=%+v after=%+v err=%v", before, after, err)
			}
			if operation != "grant-budget-spent" && operation != "reject-no-current" {
				if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "asg-new", 2, 0, now+3); err != nil {
					t.Fatalf("new assignment must remain grantable: %v", err)
				}
			}
		})
	}
}

func TestExpiredAssignmentCannotRefreshDeadline(t *testing.T) {
	for _, delta := range []int64{0, 1} {
		t.Run(fmt.Sprintf("expiry+%d", delta), func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			ns := newNS(t, db, "expired-assignment")
			c := lifeCycleCap(t, db, ns, "idem", true, 3)
			node := newNode(t, db, "node-a", 1)
			task := newTask(t, db, ns, c, "trace-expiry", "")
			now := store.Now()
			if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-expired", 1, now); err != nil {
				t.Fatal(err)
			}
			before, err := db.GetTask(ctx, ns, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-expired", 2, *before.AssignmentExpiresAt+delta); err == nil {
				t.Fatal("expired assignment must not be refreshed")
			}
			after, err := db.GetTask(ctx, ns, task.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("expired assign mutated deadline: before=%+v after=%+v err=%v", before, after, err)
			}
			if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "asg-expired", 1, 0, *before.AssignmentExpiresAt+delta); !errors.Is(err, store.ErrNoAssignment) {
				t.Fatalf("expired assignment was granted: %v", err)
			}
			if busySlots(t, db, node.ID) != 0 {
				t.Fatal("expired assignment occupied a slot")
			}
		})
	}
}

func TestRenewLeaseCannotReviveExpiredAuthorization(t *testing.T) {
	for _, delta := range []int64{0, 1} {
		t.Run(fmt.Sprintf("expiry+%d", delta), func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			ns := newNS(t, db, "renew-expired")
			c := lifeCycleCap(t, db, ns, "idem", true, 3)
			node := newNode(t, db, "node-a", 1)
			task := newTask(t, db, ns, c, "trace-renew", "")
			lease, _, attempt := grant(t, db, task.ID, node.ID, "asg-renew")
			before, err := db.GetTask(ctx, ns, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if until, err := db.RenewLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, attempt, lease, *before.LeaseUntil+delta); !errors.Is(err, store.ErrLeaseMismatch) || until != 0 {
				t.Fatalf("expired renewal must reject: until=%d err=%v", until, err)
			}
			after, err := db.GetTask(ctx, ns, task.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("expired renewal mutated authorization: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

// TestRejectAssignmentRacePreservesNewAssignment races two real writers without
// hooks/sleeps: whichever wins, old reject must leave the new identity, proposed
// attempt and deadline intact (it does not claim both orders were observed).
func TestRejectAssignmentRacePreservesNewAssignment(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ns := newNS(t, db, "reject-race")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	now := store.Now()
	for i := 0; i < 16; i++ {
		task := newTask(t, db, ns, c, fmt.Sprintf("trace-%d", i), "")
		if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-old", 1, now); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var assignErr, rejectErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			assignErr = db.AssignTaskAt(ctx, task.ID, node.ID, "asg-new", 2, now+1)
		}()
		go func() {
			defer wg.Done()
			<-start
			rejectErr = db.RejectAssignment(ctx, task.ID, "asg-old", "stale_config", now+1)
		}()
		close(start)
		wg.Wait()
		if assignErr != nil || (rejectErr != nil && !errors.Is(rejectErr, store.ErrNoAssignment)) {
			t.Fatalf("race writers failed: assign=%v reject=%v", assignErr, rejectErr)
		}
		got, err := db.GetTask(ctx, ns, task.ID)
		if err != nil || got.AssignmentID != "asg-new" || got.NodeID == nil || *got.NodeID != node.ID || got.ProposedAttempt != 2 || got.AssignmentExpiresAt == nil || *got.AssignmentExpiresAt != now+1+store.AssignmentWindowSec {
			t.Fatalf("racing old reject cleared/changed new assignment: task=%+v err=%v", got, err)
		}
	}
}
