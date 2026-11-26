package lease

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/lease.db")
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fixture struct {
	db   *store.DB
	ns   *store.Namespace
	node *store.Node
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := testDB(t)
	ctx := context.Background()
	ns, err := db.CreateNamespace(ctx, "default", "Default")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}
	node := &store.Node{Name: "node-1", MaxInstances: 4, MaxConcurrency: 2, DeclaredNamespaces: []string{"default"}}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("建节点: %v", err)
	}
	stored, err := db.GetNodeByName(ctx, "node-1")
	if err != nil {
		t.Fatalf("回读节点: %v", err)
	}
	if err := db.UpdateNodeStatus(ctx, stored.ID, store.NodeStatusOnline); err != nil {
		t.Fatalf("节点上线: %v", err)
	}
	return &fixture{db: db, ns: ns, node: stored}
}

func (f *fixture) capability(t *testing.T, name string, idempotent bool, maxAttempts int) *store.CapabilityVersion {
	t.Helper()
	spec := artifact.Spec{
		Params: json.RawMessage(`{"type":"object"}`),
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
		},
		Lifecycle: artifact.Lifecycle{Idempotent: idempotent},
	}
	if maxAttempts > 0 {
		spec.Lifecycle.MaxAttempts = &maxAttempts
	}
	capability := &store.CapabilityVersion{NamespaceID: f.ns.ID, Name: name, Version: 1, Spec: spec}
	if err := f.db.CreateCapability(context.Background(), capability); err != nil {
		t.Fatalf("建能力 %s: %v", name, err)
	}
	got, err := f.db.GetCapability(context.Background(), f.ns.ID, name, capability.Version)
	if err != nil {
		t.Fatalf("回读能力: %v", err)
	}
	return got
}

func (f *fixture) runningTask(t *testing.T, capability *store.CapabilityVersion, now int64) *store.Task {
	t.Helper()
	ctx := context.Background()
	task := &store.Task{
		NamespaceID: f.ns.ID, TraceID: "trace-lease",
		CapabilityID: capability.ID, Capability: capability.Name,
		Version: capability.Version, Digest: capability.Digest,
		Params: json.RawMessage(`{}`),
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("建任务: %v", err)
	}
	if _, err := f.db.UpsertInstances(ctx, f.node.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("申报槽位: %v", err)
	}
	if err := f.db.AssignTaskAt(ctx, task.ID, f.node.ID, "asg_lease_1", 1, now); err != nil {
		t.Fatalf("派发候选: %v", err)
	}
	token, hash, err := store.NewNodeToken()
	if err != nil {
		t.Fatalf("生成节点令牌: %v", err)
	}
	if err := f.db.SetNodeTokenHash(ctx, f.node.ID, hash); err != nil {
		t.Fatalf("写节点令牌: %v", err)
	}
	_ = token
	if _, _, _, _, _, err := f.db.GrantLeaseAuthorized(ctx, store.NodeAuthorization{NodeID: f.node.ID, NodeTokenHash: hash},
		task.ID, "asg_lease_1", 1, 0, now); err != nil {
		t.Fatalf("授予租约: %v", err)
	}
	return task
}

// TestRunOnceRequeuesLostLeaseWithinBudget 租约失联按**重试预算**重排（#661）：
// 非幂等能力只要还有预算就回 pending，不再整单作废。
// newTask 建一条 pending 任务并回读（拿权威 id/attempt 等字段）。
func (f *fixture) newTask(t *testing.T, capability *store.CapabilityVersion) *store.Task {
	t.Helper()
	ctx := context.Background()
	task := &store.Task{
		NamespaceID: f.ns.ID, TraceID: "trace-" + capability.Name,
		CapabilityID: capability.ID, Capability: capability.Name,
		Version: capability.Version, Digest: capability.Digest,
		Params: json.RawMessage(`{}`),
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("建任务: %v", err)
	}
	got, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
	if err != nil {
		t.Fatalf("回读任务: %v", err)
	}
	return got
}

// startTask 走真实入口把任务推到 running（assign + grant），返回续租需要的节点身份。
// assignment 必须每轮不同：同一个候选不能被授予两次。
func (f *fixture) startTask(t *testing.T, task *store.Task, assignment string, proposed int, now int64) (store.NodeAuthorization, string, int) {
	t.Helper()
	ctx := context.Background()
	// 节点申报的并发槽位（首次调用时落库；后续轮次沿用同一格，除非测试显式归还）。
	if instances, err := f.db.ListInstances(ctx, f.node.ID); err != nil {
		t.Fatalf("列槽位: %v", err)
	} else if len(instances) == 0 {
		if _, err := f.db.UpsertInstances(ctx, f.node.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
			t.Fatalf("申报槽位: %v", err)
		}
	}
	if err := f.db.AssignTaskAt(ctx, task.ID, f.node.ID, assignment, proposed, now); err != nil {
		t.Fatalf("派发候选 %s: %v", assignment, err)
	}
	_, hash, err := store.NewNodeToken()
	if err != nil {
		t.Fatalf("生成节点令牌: %v", err)
	}
	if err := f.db.SetNodeTokenHash(ctx, f.node.ID, hash); err != nil {
		t.Fatalf("写节点令牌: %v", err)
	}
	auth := store.NodeAuthorization{NodeID: f.node.ID, NodeTokenHash: hash}
	leaseID, _, _, attempt, _, err := f.db.GrantLeaseAuthorized(ctx, auth, task.ID, assignment, proposed, 0, now)
	if err != nil {
		t.Fatalf("授予租约 %s: %v", assignment, err)
	}
	return auth, leaseID, attempt
}

// reclaim 模拟「节点重连并明确声明这一格空闲 + 那次执行已回收」：走真实入口
// ReclaimStaleSlots（带历史 task/attempt/lease 引用的 Busy=false 事实）。
// 重排的 attempt 不伪造 reaped，所以下一轮 grant 前必须真的拿到这条回收事实。
func (f *fixture) reclaim(t *testing.T, taskID string, attempt int, leaseID string) {
	t.Helper()
	ctx := context.Background()
	noProcess := false
	released, err := f.db.ReclaimStaleSlots(ctx, f.node.ID, []store.SlotObservation{{
		Slot: 0, Busy: &noProcess, TaskID: taskID, Attempt: attempt, LeaseID: leaseID,
	}}, store.Now())
	if err != nil {
		t.Fatalf("回收槽位: %v", err)
	}
	if released != 1 {
		t.Fatalf("回收槽位数 = %d，want 1（没拿到可信事实）", released)
	}
	if _, err := f.db.Exec(`UPDATE tasks SET next_attempt_at = NULL`); err != nil {
		t.Fatalf("清退避: %v", err)
	}
}

func TestRunOnceRequeuesLostLeaseWithinBudget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := store.Now()
	capability := f.capability(t, "script", false, 3)
	task := f.runningTask(t, capability, now)
	// 租约过期（进程可能还在跑）。
	if _, err := f.db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, now-1, task.ID); err != nil {
		t.Fatalf("过期租约: %v", err)
	}
	report, err := NewSweeper(f.db, nil).RunOnce(ctx, now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if report.LeasesRequeued != 1 || report.LeasesAbandoned != 0 {
		t.Fatalf("还有预算的过期租约应回 pending 重试: %+v", report)
	}
	stored, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
	if err != nil {
		t.Fatalf("读任务: %v", err)
	}
	if stored.Phase != store.TaskPending || stored.WaitingReason != store.WaitingRetryBackoff {
		t.Fatalf("应回 pending + retry_backoff，得到 %q/%q", stored.Phase, stored.WaitingReason)
	}
	if stored.Attempt != 1 {
		t.Fatalf("重排不动 attempt（新 attempt 由下次 grant 计）：%d", stored.Attempt)
	}
	// 重排**不伪造** reaped 事实：旧进程可能还在跑（#660 的反面纪律）。
	att, err := f.db.GetTaskAttempt(ctx, task.ID, stored.Attempt)
	if err != nil {
		t.Fatalf("读 attempt: %v", err)
	}
	if att.ReapedAt != nil || att.Phase != store.AttemptPhaseRequeued {
		t.Fatalf("重排的 attempt 应标 requeued 且 reaped 仍为 NULL：%+v", att)
	}
	instances, err := f.db.ListInstances(ctx, f.node.ID)
	if err != nil {
		t.Fatalf("列槽位: %v", err)
	}
	if len(instances) != 1 || instances[0].BusyTask == nil {
		t.Fatalf("重排必须保留物理槽（旧进程可能还在跑）: %+v", instances)
	}
}

func TestRunOnceRequeuesExpiredIdempotentLease(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := store.Now()
	capability := f.capability(t, "script-idem", true, 3)
	task := f.runningTask(t, capability, now)
	if _, err := f.db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, now-1, task.ID); err != nil {
		t.Fatalf("过期租约: %v", err)
	}
	report, err := NewSweeper(f.db, nil).RunOnce(ctx, now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if report.LeasesRequeued != 1 || report.LeasesAbandoned != 0 {
		t.Fatalf("幂等过期租约应回 pending 重试: %+v", report)
	}
	stored, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
	if err != nil {
		t.Fatalf("读任务: %v", err)
	}
	if stored.Phase != store.TaskPending || stored.WaitingReason != store.WaitingRetryBackoff {
		t.Fatalf("应回 pending + retry_backoff，得到 %q/%q", stored.Phase, stored.WaitingReason)
	}
	if stored.NextAttemptAt == nil || *stored.NextAttemptAt <= now {
		t.Fatalf("重试必须有退避窗口: %+v", stored.NextAttemptAt)
	}
}

func TestRunOnceRetentionHonorsTTLAndKeepsActive(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := store.Now()
	capability := f.capability(t, "cleanup", false, 3)
	// 终态旧任务 + 活动任务各一条。
	terminal := &store.Task{
		NamespaceID: f.ns.ID, TraceID: "trace-terminal",
		CapabilityID: capability.ID, Capability: capability.Name,
		Version: capability.Version, Digest: capability.Digest, Params: json.RawMessage(`{}`),
	}
	active := &store.Task{
		NamespaceID: f.ns.ID, TraceID: "trace-active",
		CapabilityID: capability.ID, Capability: capability.Name,
		Version: capability.Version, Digest: capability.Digest, Params: json.RawMessage(`{}`),
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{terminal, active}); err != nil {
		t.Fatalf("建任务: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE tasks SET phase = 'succeeded', finished_at = ? WHERE id = ?`, now-100, terminal.ID); err != nil {
		t.Fatalf("置终态: %v", err)
	}
	if err := f.db.PutSystemConfig(ctx, &store.SystemConfig{
		RetentionEventTTLSec: 10, RetentionTaskTTLSec: 10, RetentionResultTTLSec: 10,
	}, 1); err != nil {
		t.Fatalf("写系统配置: %v", err)
	}
	oldEventID, freshEventID := "evt_test_old", "evt_test_fresh"
	if _, err := f.db.InsertEvent(ctx, &store.Event{
		ID: oldEventID, NamespaceID: f.ns.ID, TraceID: "old-event", Source: "manual-1", Type: "manual-1",
		CreatedAt: now - 100,
	}, now, 0); err != nil {
		t.Fatalf("插旧事件: %v", err)
	}
	if _, err := f.db.InsertEvent(ctx, &store.Event{
		ID: freshEventID, NamespaceID: f.ns.ID, TraceID: "fresh-event", Source: "manual-1", Type: "manual-1",
	}, now, 0); err != nil {
		t.Fatalf("插新事件: %v", err)
	}
	report, err := NewSweeper(f.db, nil).RunOnce(ctx, now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if report.Retention.TasksDeleted != 1 || report.Retention.EventsDeleted < 1 {
		t.Fatalf("TTL 清理计数不对: %+v", report.Retention)
	}
	if _, err := f.db.GetTask(ctx, f.ns.ID, active.ID); err != nil {
		t.Fatalf("活动任务不得被清理: %v", err)
	}
	if _, err := f.db.GetTask(ctx, f.ns.ID, terminal.ID); err == nil {
		t.Fatal("过期终态任务应被清理")
	}
	if _, err := f.db.GetEvent(ctx, f.ns.ID, oldEventID); err == nil {
		t.Fatal("过期事件应被清理")
	}
	if _, err := f.db.GetEvent(ctx, f.ns.ID, freshEventID); err != nil {
		t.Fatalf("新事件不得被清理: %v", err)
	}
}

func TestRetentionZeroTTLIsPermanent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := store.Now()
	capability := f.capability(t, "keep", false, 3)
	task := &store.Task{
		NamespaceID: f.ns.ID, TraceID: "trace-keep",
		CapabilityID: capability.ID, Capability: capability.Name,
		Version: capability.Version, Digest: capability.Digest, Params: json.RawMessage(`{}`),
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("建任务: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE tasks SET phase = 'failed', finished_at = ? WHERE id = ?`, now-1000, task.ID); err != nil {
		t.Fatalf("置终态: %v", err)
	}
	if err := f.db.PutSystemConfig(ctx, &store.SystemConfig{
		RetentionEventTTLSec: 0, RetentionTaskTTLSec: 0, RetentionResultTTLSec: 0,
	}, 1); err != nil {
		t.Fatalf("写系统配置: %v", err)
	}
	report, err := NewSweeper(f.db, nil).RunOnce(ctx, now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !report.Retention.TasksSkipped || !report.Retention.EventsSkipped {
		t.Fatalf("TTL=0 应跳过清理（不是「扫了没东西删」）: %+v", report.Retention)
	}
	if _, err := f.db.GetTask(ctx, f.ns.ID, task.ID); err != nil {
		t.Fatalf("TTL=0 是永久保留: %v", err)
	}
}

func TestOccupancyByNodeReadsRealLedger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	capability := f.capability(t, "busy", false, 3)
	now := store.Now()
	task := f.runningTask(t, capability, now)
	if _, err := f.db.UpsertInstances(ctx, f.node.ID, []store.InstanceSpec{{Slot: 0}, {Slot: 1}}); err != nil {
		t.Fatalf("申报槽位: %v", err)
	}
	occupancy, err := NewSweeper(f.db, nil).OccupancyByNode(ctx, []string{f.node.ID})
	if err != nil {
		t.Fatalf("OccupancyByNode: %v", err)
	}
	got := occupancy[f.node.ID]
	if got.Total != 2 || got.Busy != 1 || got.Free != 1 {
		t.Fatalf("占用投影不对: %+v（task=%s）", got, task.ID)
	}
}

// TestRunOnceAbandonsExpiredLeaseWithoutBudget 声明「不可重跑」（maxAttempts=1）时，
// 租约失联仍然是终局，且 #660 的两本账照旧同事务收敛。
func TestRunOnceAbandonsExpiredLeaseWithoutBudget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := store.Now()
	capability := f.capability(t, "once", false, 1)
	task := f.runningTask(t, capability, now)
	running, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
	if err != nil || running.LeaseUntil == nil {
		t.Fatalf("读 running 任务: %+v err=%v", running, err)
	}
	report, err := NewSweeper(f.db, nil).RunOnce(ctx, *running.LeaseUntil+1)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if report.LeasesAbandoned != 1 || report.LeasesRequeued != 0 {
		t.Fatalf("预算用尽应直接 abandoned: %+v", report)
	}
	dead, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
	if err != nil {
		t.Fatalf("读任务: %v", err)
	}
	if dead.Phase != store.TaskAbandoned {
		t.Fatalf("phase = %q，want abandoned", dead.Phase)
	}
	att, err := f.db.GetTaskAttempt(ctx, task.ID, dead.Attempt)
	if err != nil || att.ReapedAt == nil {
		t.Fatalf("终局 abandon 必须补 reaped 事实（#660）：%+v err=%v", att, err)
	}
	instances, err := f.db.ListInstances(ctx, f.node.ID)
	if err != nil {
		t.Fatalf("列槽位: %v", err)
	}
	if len(instances) != 1 || instances[0].BusyTask != nil {
		t.Fatalf("终局 abandon 必须释放它自己的槽: %+v", instances)
	}
}

// TestLongTaskAcrossTwoTTLsIsNotAbandoned 是验收 ③：一次真实的长执行跨过 ≥2 个 TTL
// 仍然 running（不被 abandon、不被重排）。
//
// 口径与生产同构，只是把量级缩小：TTL=2s、续租 200ms（10 个续租周期/TTL，生产是
// 180s/20s = 9 个），巡检每 50ms 一轮，用真实定时器跑 2.25 个 TTL（lease_until 是
// 秒口径，TTL 不能再压到 1s：那样续租永远推不动同一秒内的 deadline）。
func TestLongTaskAcrossTwoTTLsIsNotAbandoned(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.db.SetLeaseTTL(2 * time.Second)
	capability := f.capability(t, "long-step", false, 3)
	task := f.newTask(t, capability)
	auth, leaseID, attempt := f.startTask(t, task, "asg-long-1", 1, store.Now())
	granted, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
	if err != nil || granted.LeaseUntil == nil {
		t.Fatalf("读 grant 后的任务: %+v err=%v", granted, err)
	}
	initialLease := *granted.LeaseUntil

	const (
		renewEvery = 200 * time.Millisecond
		sweepEvery = 50 * time.Millisecond
		total      = 4500 * time.Millisecond // 2.25 个 TTL
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var loopErr error
	var reports []Report
	renewals := 0
	sweeper := NewSweeper(f.db, nil)

	// 续租循环：节点侧 lease.renew 的服务端一半（真实事务 + 身份校验）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(renewEvery)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if _, err := f.db.RenewLeaseAuthorized(ctx, auth, task.ID, attempt, leaseID, store.Now()); err != nil {
					mu.Lock()
					if loopErr == nil {
						loopErr = err
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				renewals++
				mu.Unlock()
			}
		}
	}()
	// 巡检循环：与生产同一条入口（RunOnce = 回收过期租约 + 收敛 + 保留期 + 统计）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(sweepEvery)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				report, err := sweeper.RunOnce(ctx, store.Now())
				if err != nil {
					mu.Lock()
					if loopErr == nil {
						loopErr = err
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				reports = append(reports, report)
				mu.Unlock()
			}
		}
	}()
	// 跑满 2.25 个 TTL（等的是「跑够这么久」这个条件，不是等某个动画/超时）。
	timer := time.NewTimer(total)
	<-timer.C
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	abandoned, requeued := 0, 0
	for _, r := range reports {
		abandoned += r.LeasesAbandoned
		requeued += r.LeasesRequeued
	}
	if abandoned != 0 || requeued != 0 {
		t.Fatalf("租约一直在续，巡检却回收了 %d 次（abandoned=%d requeued=%d）", abandoned+requeued, abandoned, requeued)
	}
	if loopErr != nil {
		t.Fatalf("长任务期间续租/巡检出错: %v", loopErr)
	}
	if renewals < 15 {
		t.Fatalf("续租次数过少（%d）：这条用例没跨过 2 个 TTL", renewals)
	}
	got, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
	if err != nil {
		t.Fatalf("读任务: %v", err)
	}
	if got.Phase != store.TaskRunning || got.Attempt != 1 {
		t.Fatalf("长任务必须还在第 1 次执行上：phase=%q attempt=%d", got.Phase, got.Attempt)
	}
	if got.LeaseUntil == nil || *got.LeaseUntil <= initialLease {
		t.Fatalf("租约应随续租推进：lease_until=%v initial=%d", got.LeaseUntil, initialLease)
	}
}

// TestKilledNodeIsAbandonedAfterTTL 是验收 ④（反例）：节点进程真死了（不再续租）时，
// 回收逻辑必须仍然在 TTL 后把它从节点上摘下来，且重排次数被 maxAttempts 卡死——
// 新逻辑不能把「死」变成「永远不判死」。
func TestKilledNodeIsAbandonedAfterTTL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.db.SetLeaseTTL(time.Second)
	capability := f.capability(t, "killed", false, 3)
	task := f.newTask(t, capability)
	sweeper := NewSweeper(f.db, nil)

	auth, leaseID, attempt := f.startTask(t, task, "asg-killed-1", 1, store.Now())
	_ = auth
	for round := 1; round <= 3; round++ {
		running, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
		if err != nil || running.LeaseUntil == nil {
			t.Fatalf("第 %d 轮读 running 任务: %+v err=%v", round, running, err)
		}
		if running.Phase != store.TaskRunning || running.NodeID == nil {
			t.Fatalf("第 %d 轮应仍在节点上 running：phase=%q node=%v", round, running.Phase, running.NodeID)
		}
		report, err := sweeper.RunOnce(ctx, *running.LeaseUntil+1)
		if err != nil {
			t.Fatalf("第 %d 轮巡检: %v", round, err)
		}
		if round < 3 {
			if report.LeasesRequeued != 1 || report.LeasesAbandoned != 0 {
				t.Fatalf("第 %d 轮：还有预算应重排一次，得到 %+v", round, report)
			}
			back, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
			if err != nil {
				t.Fatalf("第 %d 轮读重排后的任务: %v", round, err)
			}
			if back.Phase != store.TaskPending || back.NodeID != nil {
				t.Fatalf("第 %d 轮应回 pending 且摘掉节点：phase=%q node=%v", round, back.Phase, back.NodeID)
			}
			// 死节点的进程组是内核收的；节点"回来"后给出这条回收事实，下一轮才能重派。
			f.reclaim(t, task.ID, attempt, leaseID)
			auth, leaseID, attempt = f.startTask(t, task, "asg-killed-"+string(rune('1'+round)), round+1, store.Now())
			_ = auth
			continue
		}
		// 第 3 轮：预算用尽 → 终局，两本账同事务收敛（不会无限重排）。
		if report.LeasesAbandoned != 1 || report.LeasesRequeued != 0 {
			t.Fatalf("预算用尽应 abandoned，得到 %+v", report)
		}
		dead, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
		if err != nil {
			t.Fatalf("读终态任务: %v", err)
		}
		if dead.Phase != store.TaskAbandoned {
			t.Fatalf("phase = %q，want abandoned", dead.Phase)
		}
		if dead.Attempt != 3 {
			t.Fatalf("终态 attempt = %d，want 3（重排次数被 maxAttempts 卡死）", dead.Attempt)
		}
		att, err := f.db.GetTaskAttempt(ctx, task.ID, dead.Attempt)
		if err != nil || att.ReapedAt == nil {
			t.Fatalf("终局 abandon 必须补 reaped 事实（#660）：%+v err=%v", att, err)
		}
		instances, err := f.db.ListInstances(ctx, f.node.ID)
		if err != nil {
			t.Fatalf("列槽位: %v", err)
		}
		if len(instances) != 1 || instances[0].BusyTask != nil {
			t.Fatalf("终局 abandon 必须释放槽位: %+v", instances)
		}
	}
}
