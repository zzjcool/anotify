package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// 孤儿 pending 任务（#270）的数据层验收。
//
// 这组用例要守住三件事：
//  1. 判据用 capability_id（具体版本），不是能力名——@1 的节点对 @2 的任务无效
//  2. 区分「没人声明」和「声明了但都离线」，处置完全不同
//  3. abandon 只动仍是 pending 的行（CAS），且能真正把背压信号解开

// seedOrphanEnv 建一个命名空间 + 一个能力 + 一条 pending 任务，并把 created_at 拨到过去。
func seedOrphanEnv(t *testing.T, db *DB, ageSec int64) (nsID, capID, taskID string) {
	t.Helper()
	ctx := context.Background()
	// slug 只允许小写字母/数字/连字符（ns_xxx 里的下划线不行）
	ns, err := db.CreateNamespace(ctx, fmt.Sprintf("orphan-%d", time.Now().UnixNano()), "orphan test")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	capRec := &CapabilityVersion{
		ID:           NewCapabilityID("orphan-cap-1"),
		NamespaceID:  ns.ID,
		Name:         "orphan-cap",
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		CreatedAt:    Now(),
	}
	if err := db.CreateCapability(ctx, capRec); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	taskID = NewTaskID()
	task := &Task{
		ID:           taskID,
		NamespaceID:  ns.ID,
		TraceID:      "trace_orphan",
		Path:         []string{capRec.ID},
		CapabilityID: capRec.ID,
		Capability:   "orphan-cap",
		Version:      1,
		Params:       json.RawMessage(`{}`),
		Status:       TaskPending,
		CapHash:      "deadbeefdeadbeef",
		GraphID:      "grf_test",
		CreatedAt:    Now(),
	}
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 把创建时间拨到过去（只有测试会这么干；生产时间由派发决定）
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET created_at = ? WHERE namespace_id = ? AND id = ?`,
		Now()-ageSec, ns.ID, taskID); err != nil {
		t.Fatalf("age task: %v", err)
	}
	return ns.ID, capRec.ID, taskID
}

// mustNode 建一个节点（状态由调用方给）。
func mustOrphanNode(t *testing.T, db *DB, nsID, status string) string {
	t.Helper()
	id := NewNodeID()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO nodes (id, namespace_id, name, status, token_hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, nsID, "n-"+id, status, "hash-"+id, Now())
	if err != nil {
		t.Fatalf("insert node: %v", err)
	}
	return id
}

func declareNodeCap(t *testing.T, db *DB, nsID, nodeID, capID string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO node_capabilities (node_id, capability_id, namespace_id, run_spec)
		 VALUES (?, ?, ?, '{}')`,
		nodeID, capID, nsID); err != nil {
		t.Fatalf("declare node capability: %v", err)
	}
}

// TestOrphanPendingNoNodeDeclares：没有任何节点声明 → 列出来，原因=no_node_declares。
func TestOrphanPendingNoNodeDeclares(t *testing.T) {
	db := newTestDB(t)
	nsID, _, taskID := seedOrphanEnv(t, db, 7200)

	got, err := db.ListOrphanPendingTasks(context.Background(), nsID, 3600, 0)
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	if len(got) != 1 || got[0].ID != taskID {
		t.Fatalf("应列出那 1 条孤儿任务，实际 %+v", got)
	}
	if got[0].Reason != OrphanNoNodeDeclares {
		t.Fatalf("原因应为 %s，实际 %s", OrphanNoNodeDeclares, got[0].Reason)
	}
	if got[0].IdleSec < 7000 {
		t.Fatalf("空闲时长应≈7200s，实际 %d", got[0].IdleSec)
	}
}

// TestOrphanPendingNodeOffline：有节点声明但离线 → 原因=no_online_node（不是"没人声明"）。
func TestOrphanPendingNodeOffline(t *testing.T) {
	db := newTestDB(t)
	nsID, capID, taskID := seedOrphanEnv(t, db, 7200)
	node := mustOrphanNode(t, db, nsID, NodeStatusOffline)
	declareNodeCap(t, db, nsID, node, capID)

	got, err := db.ListOrphanPendingTasks(context.Background(), nsID, 3600, 0)
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	if len(got) != 1 || got[0].ID != taskID {
		t.Fatalf("离线节点的任务也算孤儿，实际 %+v", got)
	}
	if got[0].Reason != OrphanNoOnlineNode {
		t.Fatalf("原因应为 %s，实际 %s", OrphanNoOnlineNode, got[0].Reason)
	}
}

// TestOrphanPendingOnlineNodeNotListed：有在线节点声明 → 不是孤儿（不能被误列）。
func TestOrphanPendingOnlineNodeNotListed(t *testing.T) {
	db := newTestDB(t)
	nsID, capID, _ := seedOrphanEnv(t, db, 7200)
	node := mustOrphanNode(t, db, nsID, NodeStatusOnline)
	declareNodeCap(t, db, nsID, node, capID)

	got, err := db.ListOrphanPendingTasks(context.Background(), nsID, 3600, 0)
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("有在线节点能跑时不该算孤儿，实际 %+v", got)
	}
}

// TestOrphanPendingRespectsAgeFloor：刚建的任务（未超过 olderThan）不列出来。
//
// 这条是防误报的：派发瞬间"暂时没有在线节点"是正常现象。
func TestOrphanPendingRespectsAgeFloor(t *testing.T) {
	db := newTestDB(t)
	nsID, _, _ := seedOrphanEnv(t, db, 30) // 30 秒前建的

	got, err := db.ListOrphanPendingTasks(context.Background(), nsID, 3600, 0)
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("未超过 olderThan 的任务不该被算孤儿，实际 %+v", got)
	}
}

// TestAbandonPendingTaskReleasesBackpressure 是这条 issue 的核心：
// 孤儿任务会把 `MIN(created_at) WHERE status='pending'` 钉死；abandon 之后必须解开。
func TestAbandonPendingTaskReleasesBackpressure(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _, taskID := seedOrphanEnv(t, db, 7200)

	before, err := db.OldestPendingCreatedAt(ctx)
	if err != nil {
		t.Fatalf("oldest pending: %v", err)
	}
	if before == 0 {
		t.Fatal("前置：应有一条 2 小时前的 pending")
	}

	if err := db.AbandonPendingTask(ctx, nsID, taskID, "能力没有节点声明"); err != nil {
		t.Fatalf("abandon: %v", err)
	}

	task, err := db.GetTask(ctx, nsID, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != TaskAbandoned {
		t.Fatalf("状态应为 abandoned，实际 %s", task.Status)
	}
	if task.Error != "能力没有节点声明" {
		t.Fatalf("原因必须写进 error 列（排查时要看得见），实际 %q", task.Error)
	}
	if task.FinishedAt == nil {
		t.Fatal("终态必须写 finished_at")
	}

	after, err := db.OldestPendingCreatedAt(ctx)
	if err != nil {
		t.Fatalf("oldest pending after: %v", err)
	}
	if after >= before {
		t.Fatalf("abandon 后最老 pending 必须变新（背压信号解开）：before=%d after=%d", before, after)
	}
	if after != 0 {
		t.Fatalf("队列里已没有 pending，应返回 0，实际 %d", after)
	}
}

// TestAbandonPendingTaskOnlyTouchesPending：CAS 语义——已经不是 pending 的一律拒绝，
// 且**不得**改写状态（否则会把正在执行的任务打死，节点回报会被状态机拒绝）。
func TestAbandonPendingTaskOnlyTouchesPending(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _, taskID := seedOrphanEnv(t, db, 7200)

	// 让它先被取消（变成非 pending 的终态）
	if err := db.CancelTask(ctx, nsID, taskID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	err := db.AbandonPendingTask(ctx, nsID, taskID, "too late")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("对非 pending 任务应返回 ErrInvalidTransition，实际 %v", err)
	}
	task, _ := db.GetTask(ctx, nsID, taskID)
	if task.Status != TaskCanceled {
		t.Fatalf("状态不得被改写，实际 %s", task.Status)
	}
	if task.Error == "too late" {
		t.Fatal("被拒绝的 abandon 不该写原因")
	}

	// 不存在的任务 → ErrNotFound（与"状态不对"区分开，调用方要回 404 vs 409）
	if err := db.AbandonPendingTask(ctx, nsID, "tsk_does_not_exist", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的任务应返回 ErrNotFound，实际 %v", err)
	}
}

// TestAbandonPendingTaskDefaultReason：没给原因时也要留下可读的说明。
func TestAbandonPendingTaskDefaultReason(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _, taskID := seedOrphanEnv(t, db, 7200)

	if err := db.AbandonPendingTask(ctx, nsID, taskID, ""); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	task, _ := db.GetTask(ctx, nsID, taskID)
	if task.Error == "" {
		t.Fatal("默认原因也不能是空——否则事后没人知道这条任务为什么没跑")
	}
}
