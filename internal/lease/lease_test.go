package lease

import (
	"context"
	"encoding/json"
	"testing"

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
	if _, _, _, _, err := f.db.GrantLeaseAuthorized(ctx, store.NodeAuthorization{NodeID: f.node.ID, NodeTokenHash: hash},
		task.ID, "asg_lease_1", 1, 0, now); err != nil {
		t.Fatalf("授予租约: %v", err)
	}
	return task
}

func TestRunOnceRecoversExpiredNonIdempotentLease(t *testing.T) {
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
	if report.LeasesAbandoned != 1 || report.LeasesRequeued != 0 {
		t.Fatalf("非幂等过期租约应直接 abandoned: %+v", report)
	}
	stored, err := f.db.GetTask(ctx, f.ns.ID, task.ID)
	if err != nil {
		t.Fatalf("读任务: %v", err)
	}
	if stored.Phase != store.TaskAbandoned {
		t.Fatalf("phase = %q，want abandoned", stored.Phase)
	}
	// 物理槽不在这里释放：进程可能还在跑。
	instances, err := f.db.ListInstances(ctx, f.node.ID)
	if err != nil {
		t.Fatalf("列槽位: %v", err)
	}
	if len(instances) != 1 || instances[0].BusyTask == nil {
		t.Fatalf("租约过期不得释放物理槽: %+v", instances)
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
