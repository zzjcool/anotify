package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allStatuses 九态全集。
var allStatuses = []string{
	TaskPending, TaskAssigned, TaskAcked, TaskRunning,
	TaskSucceeded, TaskFailed, TaskCanceled, TaskAbandoned, TaskLeaseExpired,
}

// legalTransitions 是契约 §3.2 迁移表的**逐条**转写。
// 测试用它对照 ValidTransition：表内必须通过，表外必须拒绝。
var legalTransitions = map[string][]string{
	TaskPending:      {TaskAssigned, TaskCanceled},
	TaskAssigned:     {TaskAcked, TaskLeaseExpired, TaskCanceled},
	TaskAcked:        {TaskRunning, TaskLeaseExpired, TaskCanceled},
	TaskRunning:      {TaskSucceeded, TaskFailed, TaskLeaseExpired, TaskCanceled},
	TaskLeaseExpired: {TaskPending, TaskAbandoned},
	TaskSucceeded:    {},
	TaskFailed:       {},
	TaskCanceled:     {},
	TaskAbandoned:    {},
}

// TestValidTransitionTable 遍历 §3.2 表：表内全通过，表外全拒绝。
func TestValidTransitionTable(t *testing.T) {
	for _, from := range allStatuses {
		allowed := map[string]bool{}
		for _, to := range legalTransitions[from] {
			allowed[to] = true
			if !ValidTransition(from, to) {
				t.Errorf("ValidTransition(%s, %s) = false, want true (in §3.2 table)", from, to)
			}
		}
		for _, to := range allStatuses {
			if allowed[to] {
				continue
			}
			if ValidTransition(from, to) {
				t.Errorf("ValidTransition(%s, %s) = true, want false (not in §3.2 table)", from, to)
			}
		}
	}
	// 未知状态全部拒绝
	for _, bogus := range []string{"", "weird", "PENDING", "Pending"} {
		for _, to := range allStatuses {
			if ValidTransition(bogus, to) {
				t.Errorf("ValidTransition(%q, %s) = true, want false", bogus, to)
			}
		}
	}
}

// TestTransition 实际落库：每条合法迁移都能推进，表外全部 ErrInvalidTransition（#64 验收）。
func TestTransition(t *testing.T) {
	ctx := context.Background()

	// 为每条合法迁移单独建一个走到 from 的任务，再推 to。
	for from, targets := range legalTransitions {
		for _, to := range targets {
			from, to := from, to
			t.Run(from+"_to_"+to, func(t *testing.T) {
				db := newTestDB(t)
				orgID, _ := newTestOrg(t, db)
				cap := mkCap(orgID, "review", 1, nil)
				if err := db.CreateCapability(ctx, cap); err != nil {
					t.Fatalf("create cap: %v", err)
				}
				task := mkTask(orgID, "trace_"+from, cap)
				if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
					t.Fatalf("create task: %v", err)
				}
				// 把任务推到 from 状态（沿合法路径）
				pushToStatus(t, db, orgID, task, from)

				if err := db.Transition(ctx, orgID, task.ID, from, to); err != nil {
					t.Fatalf("legal transition %s → %s rejected: %v", from, to, err)
				}
				got, err := db.GetTask(ctx, orgID, task.ID)
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				if got.Status != to {
					t.Fatalf("status = %q, want %q", got.Status, to)
				}
			})
		}
	}

	// 表外组合：库里必须原样不动
	t.Run("illegal", func(t *testing.T) {
		db := newTestDB(t)
		orgID, _ := newTestOrg(t, db)
		cap := mkCap(orgID, "review", 1, nil)
		if err := db.CreateCapability(ctx, cap); err != nil {
			t.Fatalf("create cap: %v", err)
		}
		task := mkTask(orgID, "trace_illegal", cap)
		if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
			t.Fatalf("create task: %v", err)
		}
		for _, to := range []string{TaskAcked, TaskRunning, TaskSucceeded, TaskAbandoned, TaskLeaseExpired} {
			if to == TaskCanceled {
				continue
			}
			err := db.Transition(ctx, orgID, task.ID, TaskPending, to)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("pending → %s err = %v, want ErrInvalidTransition", to, err)
			}
		}
		got, _ := db.GetTask(ctx, orgID, task.ID)
		if got.Status != TaskPending {
			t.Fatalf("illegal transitions mutated status to %q", got.Status)
		}
	})
}

// pushToStatus 沿合法路径把任务推到目标状态（测试夹具）。
func pushToStatus(t *testing.T, db *DB, orgID string, task *Task, target string) {
	t.Helper()
	ctx := context.Background()
	path := map[string][]string{
		TaskPending:      {},
		TaskAssigned:     {TaskAssigned},
		TaskAcked:        {TaskAssigned, TaskAcked},
		TaskRunning:      {TaskAssigned, TaskAcked, TaskRunning},
		TaskSucceeded:    {TaskAssigned, TaskAcked, TaskRunning, TaskSucceeded},
		TaskFailed:       {TaskAssigned, TaskAcked, TaskRunning, TaskFailed},
		TaskCanceled:     {TaskCanceled},
		TaskAbandoned:    {TaskAssigned, TaskLeaseExpired, TaskAbandoned},
		TaskLeaseExpired: {TaskAssigned, TaskLeaseExpired},
	}
	steps, ok := path[target]
	if !ok {
		t.Fatalf("pushToStatus: no path to %q", target)
	}
	cur := TaskPending
	for _, next := range steps {
		if err := db.Transition(ctx, orgID, task.ID, cur, next); err != nil {
			t.Fatalf("push %s → %s: %v", cur, next, err)
		}
		cur = next
	}
	if cur != target {
		t.Fatalf("pushToStatus ended at %q, want %q", cur, target)
	}
}

// TestTerminalImmutable 终态不可再变（#64 验收）。
func TestTerminalImmutable(t *testing.T) {
	ctx := context.Background()
	for _, terminal := range []string{TaskSucceeded, TaskFailed, TaskCanceled, TaskAbandoned} {
		terminal := terminal
		t.Run(terminal, func(t *testing.T) {
			db := newTestDB(t)
			orgID, _ := newTestOrg(t, db)
			cap := mkCap(orgID, "review", 1, nil)
			if err := db.CreateCapability(ctx, cap); err != nil {
				t.Fatalf("create cap: %v", err)
			}
			task := mkTask(orgID, "trace_term", cap)
			if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
				t.Fatalf("create: %v", err)
			}
			pushToStatus(t, db, orgID, task, terminal)

			for _, to := range allStatuses {
				err := db.Transition(ctx, orgID, task.ID, terminal, to)
				if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("%s → %s err = %v, want ErrInvalidTransition", terminal, to, err)
				}
			}
			got, _ := db.GetTask(ctx, orgID, task.ID)
			if got.Status != terminal {
				t.Fatalf("terminal status mutated to %q", got.Status)
			}
		})
	}
}

// TestTransitionFinishedAt 进入终态时写 finished_at，中间态不写。
func TestTransitionFinishedAt(t *testing.T) {
	db, orgID, _, _, task := setupRunningTask(t)
	ctx := context.Background()

	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.FinishedAt != nil {
		t.Fatal("running task already has finished_at")
	}
	if err := db.Transition(ctx, orgID, task.ID, TaskRunning, TaskSucceeded); err != nil {
		t.Fatalf("succeed: %v", err)
	}
	got, _ = db.GetTask(ctx, orgID, task.ID)
	if got.FinishedAt == nil {
		t.Fatal("terminal status did not set finished_at")
	}
}

// TestTransitionCASAgainstConcurrency 状态被并发改走时，旧 from 的迁移必须失败。
func TestTransitionCASAgainstConcurrency(t *testing.T) {
	db, orgID, _, _, task := setupRunningTask(t)
	ctx := context.Background()

	// 第一次推进成功
	if err := db.Transition(ctx, orgID, task.ID, TaskRunning, TaskSucceeded); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	// 用旧的 from=running 再推一次 → CAS 失败（RowsAffected=0）
	err := db.Transition(ctx, orgID, task.ID, TaskRunning, TaskFailed)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second transition with stale from: err = %v, want ErrInvalidTransition", err)
	}
}

// TestTransitionNotFound 任务不存在返回 ErrNotFound。
func TestTransitionNotFound(t *testing.T) {
	db := newTestDB(t)
	orgID, _ := newTestOrg(t, db)
	err := db.Transition(context.Background(), orgID, "tsk_missing", TaskPending, TaskAssigned)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task err = %v, want ErrNotFound", err)
	}
}

// TestAssignTask 派发写归属 + attempts+1。
func TestAssignTask(t *testing.T) {
	db, orgID, nodeID, _, _ := setupRunningTask(t)
	ctx := context.Background()
	cap, err := db.GetCapability(ctx, orgID, "review", 1)
	if err != nil {
		t.Fatalf("get cap: %v", err)
	}
	task := mkTask(orgID, "trace_assign", cap)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create: %v", err)
	}
	expandCapacity(t, db, orgID, nodeID, 4)
	inst, err := db.LeaseInstance(ctx, nodeID, task.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	lease := Now() + LeaseTTLSec
	if err := db.AssignTask(ctx, orgID, task.ID, nodeID, inst, lease); err != nil {
		t.Fatalf("assign: %v", err)
	}
	got, err := db.GetTask(ctx, orgID, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != TaskAssigned {
		t.Fatalf("status = %q, want assigned", got.Status)
	}
	if got.NodeID == nil || *got.NodeID != nodeID {
		t.Fatalf("node_id = %v, want %s", got.NodeID, nodeID)
	}
	if got.InstanceID == nil || *got.InstanceID != inst {
		t.Fatalf("instance_id = %v, want %s", got.InstanceID, inst)
	}
	if got.LeaseUntil == nil || *got.LeaseUntil != lease {
		t.Fatalf("lease_until = %v, want %d", got.LeaseUntil, lease)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.Attempts)
	}
	// 已 assigned 的任务不能再 assign
	if err := db.AssignTask(ctx, orgID, task.ID, nodeID, inst, lease); err == nil {
		t.Fatal("re-assigning an assigned task was accepted")
	}
}

// TestExtendLease 心跳续租。
func TestExtendLease(t *testing.T) {
	db, orgID, nodeID, _, task := setupRunningTask(t)
	ctx := context.Background()

	newLease := Now() + 999
	if err := db.ExtendLease(ctx, orgID, task.ID, newLease); err != nil {
		t.Fatalf("extend running lease: %v", err)
	}
	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.LeaseUntil == nil || *got.LeaseUntil != newLease {
		t.Fatalf("lease_until = %v, want %d", got.LeaseUntil, newLease)
	}

	// 终态任务不再续租
	if err := db.Transition(ctx, orgID, task.ID, TaskRunning, TaskSucceeded); err != nil {
		t.Fatalf("succeed: %v", err)
	}
	if err := db.ExtendLease(ctx, orgID, task.ID, newLease+100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("extend terminal lease err = %v, want ErrNotFound", err)
	}
	_ = nodeID
}

// TestExtendLeasesForNode 心跳批量续期：节点上所有在跑任务一起前进。
func TestExtendLeasesForNode(t *testing.T) {
	db, orgID, nodeID, _, _ := setupRunningTask(t)
	ctx := context.Background()
	cap, err := db.GetCapability(ctx, orgID, "review", 1)
	if err != nil {
		t.Fatalf("get cap: %v", err)
	}
	// 再推两个任务到 running
	expandCapacity(t, db, orgID, nodeID, 4)
	for i := 0; i < 2; i++ {
		x := mkTask(orgID, "trace_multi", cap)
		if err := db.CreateTasks(ctx, []*Task{x}); err != nil {
			t.Fatalf("create: %v", err)
		}
		driveToRunning(t, db, orgID, nodeID, x)
	}
	newLease := Now() + 777
	n, err := db.ExtendLeasesForNode(ctx, orgID, nodeID, newLease)
	if err != nil {
		t.Fatalf("extend for node: %v", err)
	}
	if n != 3 {
		t.Fatalf("extended %d leases, want 3", n)
	}
	tasks, err := db.ListTasks(ctx, orgID, "", nodeID, "", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, x := range tasks {
		if x.LeaseUntil == nil || *x.LeaseUntil != newLease {
			t.Fatalf("task %s lease = %v, want %d", x.ID, x.LeaseUntil, newLease)
		}
	}
}

// ---------- #64：租约巡检 ----------

// TestSweepLeases 过期租约回收（#64 验收）。
//
// 场景：attempts=1 < maxAttempts(默认 3) → 回 pending。
func TestSweepLeases(t *testing.T) {
	db, orgID, nodeID, _, task := setupRunningTask(t)
	ctx := context.Background()

	// 让租约过期
	if err := db.ExtendLease(ctx, orgID, task.ID, Now()-10); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	reclaimed, err := db.SweepExpiredLeases(ctx, Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0] != task.ID {
		t.Fatalf("reclaimed = %v, want [%s]", reclaimed, task.ID)
	}
	got, err := db.GetTask(ctx, orgID, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != TaskPending {
		t.Fatalf("status = %q, want pending (attempts 未用尽要重派)", got.Status)
	}
	if got.NodeID != nil || got.InstanceID != nil || got.LeaseUntil != nil {
		t.Fatalf("requeued task kept stale ownership: node=%v inst=%v lease=%v",
			got.NodeID, got.InstanceID, got.LeaseUntil)
	}
	// 槽位被释放
	insts, err := db.ListInstances(ctx, nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range insts {
		if in.BusyTask != nil && *in.BusyTask == task.ID {
			t.Fatal("instance still holds the reclaimed task")
		}
	}
}

// TestSweepLeasesAttemptsExhausted attempts 用尽 → abandoned。
func TestSweepLeasesAttemptsExhausted(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	// maxAttempts=1
	cap := mkCap(orgID, "review", 1, nil)
	cap.Lifecycle = Lifecycle{MaxAttempts: 1}
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	node, _ := mkNode(t, db, orgID, "mbp")
	if _, err := db.ConfirmCapabilities(ctx, orgID, node.ID, []CapabilityRef{{Name: "review", Version: 1}}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := db.UpdateNodeStatus(ctx, orgID, node.ID, NodeStatusOnline); err != nil {
		t.Fatalf("online: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, orgID, node.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("instances: %v", err)
	}
	task := mkTask(orgID, "trace_exhaust", cap)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create: %v", err)
	}
	driveToRunning(t, db, orgID, node.ID, task) // attempts 变 1

	if err := db.ExtendLease(ctx, orgID, task.ID, Now()-1); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := db.SweepExpiredLeases(ctx, Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, err := db.GetTask(ctx, orgID, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != TaskAbandoned {
		t.Fatalf("status = %q, want abandoned (attempts 1 >= maxAttempts 1)", got.Status)
	}
	if got.FinishedAt == nil {
		t.Fatal("abandoned task has no finished_at")
	}
}

// TestSweepLeasesNonIdempotentAbandons idempotent:false 的能力租约过期后直接 abandoned。
func TestSweepLeasesNonIdempotentAbandons(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	no := false
	cap := mkCap(orgID, "deploy", 1, nil)
	cap.Lifecycle = Lifecycle{Idempotent: &no, MaxAttempts: 99}
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	node, _ := mkNode(t, db, orgID, "mbp")
	if _, err := db.ConfirmCapabilities(ctx, orgID, node.ID, []CapabilityRef{{Name: "deploy", Version: 1}}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := db.UpdateNodeStatus(ctx, orgID, node.ID, NodeStatusOnline); err != nil {
		t.Fatalf("online: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, orgID, node.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("instances: %v", err)
	}
	task := mkTask(orgID, "trace_nonidem", cap)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create: %v", err)
	}
	driveToRunning(t, db, orgID, node.ID, task)

	if err := db.ExtendLease(ctx, orgID, task.ID, Now()-1); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := db.SweepExpiredLeases(ctx, Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.Status != TaskAbandoned {
		t.Fatalf("status = %q, want abandoned (idempotent:false 不重放)", got.Status)
	}
}

// TestSweepLeasesIgnoresLiveLeases 未过期的不动。
func TestSweepLeasesIgnoresLiveLeases(t *testing.T) {
	db, orgID, _, _, task := setupRunningTask(t)
	ctx := context.Background()
	if err := db.ExtendLease(ctx, orgID, task.ID, Now()+3600); err != nil {
		t.Fatalf("extend: %v", err)
	}
	reclaimed, err := db.SweepExpiredLeases(ctx, Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("reclaimed live lease: %v", reclaimed)
	}
	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.Status != TaskRunning {
		t.Fatalf("live task status changed to %q", got.Status)
	}
}

// TestSweepLeasesSkipsCheckpoints assigned 超 ack 等待也算过期（契约 §3.3）。
func TestSweepLeasesAssignedExpires(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	cap := mkCap(orgID, "review", 1, nil)
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	node, _ := mkNode(t, db, orgID, "mbp")
	if _, err := db.UpsertInstances(ctx, orgID, node.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("instances: %v", err)
	}
	task := mkTask(orgID, "trace_assigned", cap)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create: %v", err)
	}
	inst, err := db.LeaseInstance(ctx, node.ID, task.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	// 派发但一直不 ack，租约到期
	if err := db.AssignTask(ctx, orgID, task.ID, node.ID, inst, Now()-1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	reclaimed, err := db.SweepExpiredLeases(ctx, Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("assigned task not reclaimed: %v", reclaimed)
	}
	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.Status != TaskPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
}

// TestSweepLeasesIdempotent 重复巡检不会把同一个任务回收两次。
func TestSweepLeasesIdempotent(t *testing.T) {
	db, orgID, _, _, task := setupRunningTask(t)
	ctx := context.Background()
	if err := db.ExtendLease(ctx, orgID, task.ID, Now()-1); err != nil {
		t.Fatalf("expire: %v", err)
	}
	first, err := db.SweepExpiredLeases(ctx, Now())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first sweep reclaimed %d, want 1", len(first))
	}
	second, err := db.SweepExpiredLeases(ctx, Now())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second sweep reclaimed %v again", second)
	}
}

// TestCancelTask 取消 + 释放槽位。
func TestCancelTask(t *testing.T) {
	db, orgID, nodeID, _, task := setupRunningTask(t)
	ctx := context.Background()

	if err := db.CancelTask(ctx, orgID, task.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.Status != TaskCanceled {
		t.Fatalf("status = %q, want canceled", got.Status)
	}
	insts, err := db.ListInstances(ctx, nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range insts {
		if in.BusyTask != nil && *in.BusyTask == task.ID {
			t.Fatal("canceled task still holds an instance slot")
		}
	}
	// 再取消一次 → 终态拒绝
	if err := db.CancelTask(ctx, orgID, task.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancel twice err = %v, want ErrInvalidTransition", err)
	}
}

// TestCancelPendingTask pending 也能取消（契约 §3.2 最后一行）。
func TestCancelPendingTask(t *testing.T) {
	db, orgID, _, task := setupTaskFixture(t)
	ctx := context.Background()
	if err := db.CancelTask(ctx, orgID, task.ID); err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.Status != TaskCanceled {
		t.Fatalf("status = %q, want canceled", got.Status)
	}
}

// TestReportResultFromAssignedImpliesRunning 节点没发 progress 帧直接 result 也能落。
func TestReportResultFromAssignedImpliesRunning(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	cap := mkCap(orgID, "review", 1, nil)
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	node, _ := mkNode(t, db, orgID, "mbp")
	if _, err := db.UpsertInstances(ctx, orgID, node.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("instances: %v", err)
	}
	task := mkTask(orgID, "trace_direct", cap)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create: %v", err)
	}
	inst, err := db.LeaseInstance(ctx, node.ID, task.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := db.AssignTask(ctx, orgID, task.ID, node.ID, inst, Now()+LeaseTTLSec); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := db.Transition(ctx, orgID, task.ID, TaskAssigned, TaskAcked); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// 不推 running，直接 result
	port := "fix"
	if err := db.ReportResult(ctx, orgID, task.ID, TaskSucceeded, &port, true, "done", nil, ""); err != nil {
		t.Fatalf("report from acked: %v", err)
	}
	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.Status != TaskSucceeded {
		t.Fatalf("status = %q, want succeeded", got.Status)
	}
	if got.StartedAt == nil {
		t.Fatal("started_at not set (running was implied)")
	}
}

// TestMarkRunningIdempotent 重复 progress 帧不报错。
func TestMarkRunningIdempotent(t *testing.T) {
	db, orgID, _, _, task := setupRunningTask(t)
	ctx := context.Background()
	if err := db.MarkRunning(ctx, orgID, task.ID); err != nil {
		t.Fatalf("MarkRunning on running task: %v", err)
	}
}

// TestCountTasksByStatus trace 视图的状态统计。
func TestCountTasksByStatus(t *testing.T) {
	db, orgID, _, _, task := setupRunningTask(t)
	ctx := context.Background()
	counts, err := db.CountTasksByStatus(ctx, orgID, "")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts[TaskRunning] != 1 {
		t.Fatalf("running count = %d, want 1 (task %s)", counts[TaskRunning], task.ID)
	}
}

// TestSweepLeasesNoExpiryIsNoop 没有过期任务时返回空切片（不是 nil）。
func TestSweepLeasesNoExpiryIsNoop(t *testing.T) {
	db := newTestDB(t)
	reclaimed, err := db.SweepExpiredLeases(context.Background(), Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if reclaimed == nil {
		t.Fatal("SweepExpiredLeases returned nil, want []")
	}
	if len(reclaimed) != 0 {
		t.Fatalf("want 0, got %d", len(reclaimed))
	}
}

// TestLeaseConstants 契约 §3.3 的参数值。
func TestLeaseConstants(t *testing.T) {
	if LeaseTTLSec != 30 {
		t.Errorf("LeaseTTLSec = %d, want 30", LeaseTTLSec)
	}
	if HeartbeatSec != 10 {
		t.Errorf("HeartbeatSec = %d, want 10", HeartbeatSec)
	}
	if AckWaitSec != 20 {
		t.Errorf("AckWaitSec = %d, want 20", AckWaitSec)
	}
	if DeadAfterHeartbeats != 2 {
		t.Errorf("DeadAfterHeartbeats = %d, want 2", DeadAfterHeartbeats)
	}
}

// TestLifecycleMaxAttemptsFromJSON 从库里读回 lifecycle 的 maxAttempts / idempotent。
func TestLifecycleMaxAttemptsFromJSON(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	no := false
	cap := mkCap(orgID, "deploy", 1, nil)
	cap.Lifecycle = Lifecycle{MaxAttempts: 5, Idempotent: &no, ResultTTLSec: 120}
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetCapability(ctx, orgID, "deploy", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Lifecycle.MaxAttempts != 5 {
		t.Fatalf("maxAttempts = %d, want 5", got.Lifecycle.MaxAttempts)
	}
	if got.Lifecycle.IsIdempotent() {
		t.Fatal("idempotent:false lost in round-trip")
	}
	if got.Lifecycle.ResultTTLSec != 120 {
		t.Fatalf("resultTtlSec = %d, want 120", got.Lifecycle.ResultTTLSec)
	}
	// 原始 JSON 里只出现声明了的字段（omitempty 生效）
	var raw map[string]any
	if err := json.Unmarshal(mustLifecycleRaw(t, db, cap.ID), &raw); err != nil {
		t.Fatalf("unmarshal raw lifecycle: %v", err)
	}
	if _, ok := raw["retryable"]; ok {
		t.Error("retryable:false should be omitted (omitempty)")
	}
}

// mustLifecycleRaw 直接读库里的 lifecycle 原文。
func mustLifecycleRaw(t *testing.T, db *DB, capID string) []byte {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT lifecycle FROM capability_versions WHERE id = ?`, capID).Scan(&raw); err != nil {
		t.Fatalf("read lifecycle: %v", err)
	}
	return []byte(raw)
}

// TestStatusWriterIsUnique 契约 §3「唯一改 status 的入口」的静态门禁。
//
// 全仓只允许 transitionGuarded 这一个函数体里出现 `UPDATE tasks SET status`；
// 其他任何位置（含其他包、其他函数）出现即失败。
// 只看真正的 SQL 字面量，注释里的说明不算。
func TestStatusWriterIsUnique(t *testing.T) {
	allowedFn := "func (d *DB) transitionGuarded("
	root := "../.."
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 跳过不可读目录（.git 等）
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", ".e2e-bin", "dist", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		inAllowedFn := false
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			// 函数边界：进入 transitionGuarded 后直到下一个顶层 func 结束
			if strings.HasPrefix(line, "func ") {
				inAllowedFn = strings.HasPrefix(line, allowedFn)
				continue
			}
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "UPDATE tasks SET status") && !inAllowedFn {
				offenders = append(offenders, path+": "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(offenders) != 0 {
		t.Fatalf("`UPDATE tasks SET status` found outside transitionGuarded:\n  %s",
			strings.Join(offenders, "\n  "))
	}

	// 同一文件里必须真的存在那一处（防止「全删了也算通过」）
	src := readSource(t, "task.go")
	if !strings.Contains(src, "UPDATE tasks SET status") {
		t.Fatal("task.go lost the single status writer (transitionGuarded)")
	}
}

// TestSweepLeasesGuardAgainstHeartbeatRace 守卫条件：租约刚被续期时不得回收。
//
// 构造：扫描时租约已过期，但在 transition 之前心跳把它推到未来。
// 这里通过「直接续期再巡检」模拟该时序——守卫应当让这次回收失败而不是误杀。
func TestSweepLeasesGuardAgainstHeartbeatRace(t *testing.T) {
	db, orgID, _, _, task := setupRunningTask(t)
	ctx := context.Background()

	// 租约已过期（巡检会看到它）
	if err := db.ExtendLease(ctx, orgID, task.ID, Now()-10); err != nil {
		t.Fatalf("expire: %v", err)
	}
	// 心跳在巡检之前把租约推到未来
	if err := db.ExtendLease(ctx, orgID, task.ID, Now()+3600); err != nil {
		t.Fatalf("heartbeat extend: %v", err)
	}
	reclaimed, err := db.SweepExpiredLeases(ctx, Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("sweep reclaimed a re-leased task: %v", reclaimed)
	}
	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.Status != TaskRunning {
		t.Fatalf("status = %q, want running (heartbeat won the race)", got.Status)
	}
	if got.LeaseUntil == nil || *got.LeaseUntil != Now()+3600 {
		t.Fatalf("lease was disturbed: %v", got.LeaseUntil)
	}
}

// TestTransitionGuardFailedSentinel 守卫失败用独立哨兵值，便于上层区分。
func TestTransitionGuardFailedSentinel(t *testing.T) {
	db, orgID, _, _, task := setupRunningTask(t)
	ctx := context.Background()
	// 正常续期，然后带一个不可能满足的守卫
	err := db.transitionGuarded(ctx, nil, orgID, task.ID, TaskRunning, TaskLeaseExpired,
		"lease_until IS NOT NULL AND lease_until < ?", 0)
	if !errors.Is(err, ErrTransitionGuardFailed) {
		t.Fatalf("guard failure err = %v, want ErrTransitionGuardFailed", err)
	}
	got, _ := db.GetTask(ctx, orgID, task.ID)
	if got.Status != TaskRunning {
		t.Fatalf("guarded transition mutated status despite guard failure: %q", got.Status)
	}
}
