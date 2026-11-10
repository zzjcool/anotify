package store

// 孤儿 pending 任务（#270）的数据层验收（v4 迁移）。
//
// v4 起「孤儿」的判据与**派发路径复用同一份**判定（EligibleNodesForTask →
// nodeEligibility），不再有一张「节点申报了哪些能力」的近似表：能力在不在看
// `tasks.capability_id`（具体版本），能不能跑看「在线 + ns 申报∩授权 + 机器级配置已
// apply + 持有该 digest + 命令齐全 + requires 值可解析」。
//
// 这组用例要守住三件事：
//  1. 判据用 capability_id / 任务钉住的 digest（具体版本），不是能力名——
//     「节点持有 @2 的正文」不能救一条 @1 的任务
//  2. 区分「没有任何节点」与「有节点但都不可用」，并给出最具体的原因
//     （Reason=no_online_node + NodeReason=协议词表）
//  3. abandon 只动仍是 pending 的行（CAS），且能真正把背压信号解开

import (
	"context"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// seedOrphanEnv 建一个命名空间 + 一个能力 + 一条 pending 任务，并把 created_at 拨到过去。
//
// 任务用 v4Task 建（它会把能力行的 digest 钉在任务上）：孤儿判定读的就是这个 digest，
// 没有它任何节点都不能被证明持有正文（一律按 NotDeployed 报）。
func seedOrphanEnv(t *testing.T, db *DB, ageSec int64) (nsID string, cap *CapabilityVersion, taskID string) {
	t.Helper()
	ctx := context.Background()
	nsID, _ = newTestNS(t, db)
	cap = v4Cap(t, db, nsID, "orphan-cap", 1)
	task := v4Task(t, db, nsID, cap, "trace_orphan", "{}")
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET created_at = ? WHERE namespace_id = ? AND id = ?`,
		Now()-ageSec, nsID, task.ID); err != nil {
		t.Fatalf("age task: %v", err)
	}
	return nsID, cap, task.ID
}

// mustOrphanNode 建一个节点（状态由调用方给）。
func mustOrphanNode(t *testing.T, db *DB, status string) *Node {
	t.Helper()
	n := &Node{Name: "orphan-" + NewNodeID(), Status: status, DeclaredNamespaces: []string{"*"}}
	if _, err := db.CreateNode(context.Background(), n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	return n
}

// deployApplied 让节点**真的持有**某份正文（v4 的「能跑」事实 = deployments.state=applied）。
//
// pending / local 都不算持有：那是「中枢希望它持有」或「本地内容不一致」，节点上
// 未必有这份字节，派过去只会换来一次注定失败的执行。
func deployApplied(t *testing.T, db *DB, nsID, capabilityID, digest, nodeID string) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.CreateDeployments(ctx, nsID, capabilityID, digest, []string{nodeID})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	for _, r := range rows {
		if err := db.MarkDeployResult(ctx, nsID, r.ID, DeploymentApplied, ""); err != nil {
			t.Fatalf("mark deployment applied: %v", err)
		}
	}
}

// TestOrphanPendingNoNodeDeclares：一个节点都没有 → 列出来，原因=no_online_node
// 且具体原因是「没有任何匹配节点」（NoMatchingNode）。
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
	if got[0].Reason != OrphanNoOnlineNode {
		t.Fatalf("原因应为 %s，实际 %s", OrphanNoOnlineNode, got[0].Reason)
	}
	if got[0].NodeReason != protocol.ConditionReasonNoMatchingNode {
		t.Fatalf("具体原因应为 %s（一个节点都没申报），实际 %s",
			protocol.ConditionReasonNoMatchingNode, got[0].NodeReason)
	}
	if got[0].IdleSec < 7000 {
		t.Fatalf("空闲时长应≈7200s，实际 %d", got[0].IdleSec)
	}
}

// TestOrphanPendingNodeOffline：有节点但离线 → 原因=no_online_node / NodeOffline
// （不是「没有匹配节点」：运维要看到的是「机器不在」）。
func TestOrphanPendingNodeOffline(t *testing.T) {
	db := newTestDB(t)
	nsID, _, taskID := seedOrphanEnv(t, db, 7200)
	mustOrphanNode(t, db, NodeStatusOffline)

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
	if got[0].NodeReason != protocol.ConditionReasonNodeOffline {
		t.Fatalf("具体原因应为 %s，实际 %s", protocol.ConditionReasonNodeOffline, got[0].NodeReason)
	}
}

// TestOrphanPendingOnlineNodeNotListed：在线节点持有该 digest → 不是孤儿（不能被误列）。
func TestOrphanPendingOnlineNodeNotListed(t *testing.T) {
	db := newTestDB(t)
	nsID, capRow, taskID := seedOrphanEnv(t, db, 7200)
	node := v4Node(t, db, "orphan-online-"+NewNodeID(), 1)
	deployApplied(t, db, nsID, capRow.ID, capRow.Digest, node.ID)

	got, err := db.ListOrphanPendingTasks(context.Background(), nsID, 3600, 0)
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("有在线节点能跑时不该算孤儿，实际 %+v（task=%s）", got, taskID)
	}
}

// TestOrphanPendingOnlyPinnedDigestCounts 是上面第 1 条不变量的实测：
// 节点只持有 **@2 的正文** 不能救一条钉住 @1 的任务（判据是具体版本，不是能力名）。
func TestOrphanPendingOnlyPinnedDigestCounts(t *testing.T) {
	db := newTestDB(t)
	nsID, capV1, taskID := seedOrphanEnv(t, db, 7200)
	capV2 := v4Cap(t, db, nsID, "orphan-cap", 2, "other")
	if capV2.Digest == capV1.Digest {
		t.Fatalf("两个版本的 digest 相同（%s）：夹具没能造出「不同正文」", capV2.Digest)
	}
	node := v4Node(t, db, "orphan-v2-"+NewNodeID(), 1)
	deployApplied(t, db, nsID, capV2.ID, capV2.Digest, node.ID)

	got, err := db.ListOrphanPendingTasks(context.Background(), nsID, 3600, 0)
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	if len(got) != 1 || got[0].ID != taskID {
		t.Fatalf("节点只持有 @2 时 @1 的任务仍是孤儿，实际 %+v", got)
	}
	if got[0].NodeReason != protocol.ConditionReasonNotDeployed {
		t.Fatalf("具体原因应为 %s，实际 %s", protocol.ConditionReasonNotDeployed, got[0].NodeReason)
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
// 孤儿任务会把 `MIN(created_at) WHERE phase='pending'` 钉死；abandon 之后必须解开。
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
	if task.Phase != TaskAbandoned {
		t.Fatalf("相位应为 abandoned，实际 %s", task.Phase)
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
// 且**不得**改写相位（否则会把正在执行的任务打死，节点回报会被状态机拒绝）。
func TestAbandonPendingTaskOnlyTouchesPending(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _, taskID := seedOrphanEnv(t, db, 7200)

	// 让它先被取消（变成非 pending 的终态）。pending 上请求取消是就地终局：
	// 用户取消 → cancelled（abandon 走 abandoned）。
	if err := db.RequestCancel(ctx, taskID, protocol.ReasonCancelledByUser, Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	err := db.AbandonPendingTask(ctx, nsID, taskID, "too late")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("对非 pending 任务应返回 ErrInvalidTransition，实际 %v", err)
	}
	task, _ := db.GetTask(ctx, nsID, taskID)
	if task.Phase != TaskCancelled {
		t.Fatalf("相位不得被改写，实际 %s", task.Phase)
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
