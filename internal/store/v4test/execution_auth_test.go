package v4test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 C3a（认证执行入口 + 三层预算 + 快照）的验收集：
//
//   - 执行入口只认当前 NodeSession（nodeId + token sha256）：令牌轮换后旧会话对
//     grant / renew / result / 历史回收全部失效，同 UID 也不例外；当前身份成功；
//   - namespace 撤销后 renew / result 也一并失效（不只在 grant 时检查）；
//   - stale 的 proposedAttempt / assignment / 过期窗口只被拒绝，不破坏新候选；
//   - 三层预算：机器总量（local+node 合并）、跨 ns 同名能力（perCapability / "*"）、
//     同 ns 同能力跨版本的 pinned maxPerNode；
//   - 占用 = 未回收执行 + 真实 busy reservation（节点 Busy=true 落进槽账本），
//     自己的 reservation 只算一次；phase/rehello/空事实都不能假释放；
//   - 并发窄预算 grant 恰一个成功，拒绝不计 attempt、不占槽；
//   - 到期/取消后 renew/result 不得复活；旧 attempt 的 ACK 只按精确历史绑定回收；
//   - task 创建后改 ns 配置，首次 grant 仍用原 pin 与 revision；
//   - 同一 attempt 中途改机器配置，旧快照不变；新 attempt 用新 limits。

// observeReservation 让节点像真机一样回报「slot 上有一条 Busy=true 的 reservation」。
//
// 只有能对上中枢当前候选（task/assignment/proposed/窗口）的事实才会进槽账本；
// 这就是「真实 busy reservation」的来源（纯中枢侧 assignment 不是物理占用）。
func observeReservation(t *testing.T, db *store.DB, nsID, nodeID, taskID string, slot int) {
	t.Helper()
	ctx := context.Background()
	task, err := db.GetTask(ctx, nsID, taskID)
	if err != nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	if task.AssignmentID == "" || task.ProposedAttempt <= 0 || task.NodeID == nil || *task.NodeID != nodeID {
		t.Fatalf("观测 reservation 需要当前候选：%+v", task)
	}
	busy := true
	n, rerr := db.ReclaimStaleSlots(ctx, nodeID, []store.SlotObservation{{
		Slot: slot, Busy: &busy,
		TaskID: taskID, AssignmentID: task.AssignmentID, ProposedAttempt: task.ProposedAttempt,
	}}, store.Now())
	if rerr != nil || n != 0 {
		t.Fatalf("记录 busy reservation 不该是回收：n=%d err=%v", n, rerr)
	}
}

// instanceLedger 读一个申报槽的完整 busy 账本：reservation 与执行的判别看 lease
// （空 = reservation，非空 = 执行），reservation 还必须带 assignment 与 recorded 窗口。
func instanceLedger(t *testing.T, db *store.DB, nodeID string, slot int) (taskID string, attempt int, leaseID, assignmentID string, expiresAt int64) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT COALESCE(busy_task,''), busy_attempt, busy_lease_id, busy_assignment_id, busy_expires_at
		   FROM node_instances WHERE node_id = ? AND slot = ?`, nodeID, slot).
		Scan(&taskID, &attempt, &leaseID, &assignmentID, &expiresAt); err != nil {
		t.Fatalf("read instance ledger slot %d: %v", slot, err)
	}
	return taskID, attempt, leaseID, assignmentID, expiresAt
}

// capWithPinnedMaxPerNode 造一个显式 pinned concurrency.maxPerNode 的幂等能力。
func capWithPinnedMaxPerNode(t *testing.T, db *store.DB, nsID, name string, perNode, maxAttempts int) *store.CapabilityVersion {
	t.Helper()
	c := testCap(nsID, name, 1, nil)
	c.Spec.Lifecycle.Idempotent = true
	c.Spec.Lifecycle.MaxAttempts = &maxAttempts
	c.Spec.Concurrency.MaxPerNode = &perNode
	return createCap(t, db, c)
}

// TestAuthorizedEntriesRequireCurrentNodeCredential 验令牌轮换后的会话失效面：
// 同一个 nodes.id（UID 不变）也不能用旧 token hash 做 grant / renew / result /
// 历史回收；当前 hash 全部成功。
func TestAuthorizedEntriesRequireCurrentNodeCredential(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "auth-rotate")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-rotate", "")

	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-1", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	oldAuth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash} // 发放时捕获的会话身份
	newHash := store.HashNodeToken("ant_node_c3a_rotated")
	if err := db.SetNodeTokenHash(ctx, node.ID, newHash); err != nil {
		t.Fatalf("rotate node token: %v", err)
	}
	currentAuth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: newHash}

	// 旧会话 grant：同 UID 仍被拒（UID 不能替代 token hash），且不动候选。
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, oldAuth, task.ID, "asg-1", 1, 0, store.Now()); !errors.Is(err, store.ErrNodeIdentityMismatch) {
		t.Fatalf("旧 token 的 grant 应 ErrNodeIdentityMismatch，得到 %v", err)
	}
	still := mustPhase(t, db, ns, task.ID, store.TaskPending)
	if still.AssignmentID != "asg-1" || still.Attempt != 0 {
		t.Fatalf("被拒的旧会话 grant 不得消耗/清掉候选：%+v", still)
	}

	leaseID, token, attempt, _, err := db.GrantLeaseAuthorized(ctx, currentAuth, task.ID, "asg-1", 1, 0, store.Now())
	if err != nil {
		t.Fatalf("当前身份 grant 应成功：%v", err)
	}
	if attempt != 1 || leaseID == "" || token == "" {
		t.Fatalf("grant 事实不完整：attempt=%d lease=%q token=%q", attempt, leaseID, token)
	}

	// 旧会话 renew / result：同 UID 仍被拒。
	if _, err := db.RenewLeaseAuthorized(ctx, oldAuth, task.ID, attempt, leaseID, store.Now()); !errors.Is(err, store.ErrNodeIdentityMismatch) {
		t.Fatalf("旧 token 的 renew 应 ErrNodeIdentityMismatch，得到 %v", err)
	}
	if err := db.CompleteTaskAuthorized(ctx, oldAuth, task.ID, attempt, leaseID, store.Now(),
		store.TaskResult{Phase: store.TaskSucceeded}); !errors.Is(err, store.ErrNodeIdentityMismatch) {
		t.Fatalf("旧 token 的 result 应 ErrNodeIdentityMismatch，得到 %v", err)
	}
	if _, err := db.RenewLeaseAuthorized(ctx, currentAuth, task.ID, attempt, leaseID, store.Now()); err != nil {
		t.Fatalf("当前 token 的 renew 应成功：%v", err)
	}
	if err := db.CompleteTaskAuthorized(ctx, currentAuth, task.ID, attempt, leaseID, store.Now(),
		store.TaskResult{Phase: store.TaskSucceeded}); err != nil {
		t.Fatalf("当前 token 的 result 应成功：%v", err)
	}
	mustPhase(t, db, ns, task.ID, store.TaskSucceeded)
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("正常结束后槽位应释放")
	}

	// ReapTaskAttempt 也保留当前身份：旧 hash 被拒，当前 hash 幂等接受（已 reaped）。
	reapIn := store.ReapedInputs{
		TaskID: task.ID, Attempt: attempt,
		NodeID: node.ID, NodeTokenHash: oldAuth.NodeTokenHash,
		LeaseID: leaseID, Slot: 0,
	}
	if _, err := db.ReapTaskAttempt(ctx, reapIn); !errors.Is(err, store.ErrNodeIdentityMismatch) {
		t.Fatalf("旧 token 的历史回收应 ErrNodeIdentityMismatch，得到 %v", err)
	}
	reapIn.NodeTokenHash = newHash
	if reaped, err := db.ReapTaskAttempt(ctx, reapIn); err != nil || reaped {
		t.Fatalf("当前 token 的重复回收应幂等：reaped=%v err=%v", reaped, err)
	}
}

// TestAuthorizedRenewAndResultRequireCurrentNamespace 验 ns 撤销在续租/结果上也生效
// （不能只在 grant 时检查一次——assign 后撤销授权，旧执行不得继续续命/写终态）。
func TestAuthorizedRenewAndResultRequireCurrentNamespace(t *testing.T) {
	for _, revoke := range []string{"authorized", "declared"} {
		t.Run(revoke, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			ns := newNS(t, db, "auth-ns-"+revoke)
			c := lifeCycleCap(t, db, ns, "idem", true, 3)
			node := newNode(t, db, "node-a", 1)
			task := newTask(t, db, ns, c, "trace-ns", "")
			leaseID, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")
			auth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}

			switch revoke {
			case "authorized":
				putNodeCfg(t, db, node.ID, []string{"other"}, nil, map[string]any{"total": 1})
				recordAppliedForNode(t, db, node.ID)
			case "declared":
				if err := db.RecordHello(ctx, node.ID, store.NodeFacts{Hostname: node.Name}, nil,
					[]string{"other"}, nil, nil, "test-agent", "test-commit"); err != nil {
					t.Fatalf("record hello: %v", err)
				}
				recordAppliedForNode(t, db, node.ID)
			}

			if _, err := db.RenewLeaseAuthorized(ctx, auth, task.ID, attempt, leaseID, store.Now()); !errors.Is(err, store.ErrEmitNamespaceDenied) {
				t.Fatalf("ns 撤销后 renew 应 ErrEmitNamespaceDenied，得到 %v", err)
			}
			if err := db.CompleteTaskAuthorized(ctx, auth, task.ID, attempt, leaseID, store.Now(),
				store.TaskResult{Phase: store.TaskSucceeded}); !errors.Is(err, store.ErrEmitNamespaceDenied) {
				t.Fatalf("ns 撤销后 result 应 ErrEmitNamespaceDenied，得到 %v", err)
			}
			still := mustPhase(t, db, ns, task.ID, store.TaskRunning)
			if still.LeaseID != leaseID || still.FinishedAt != nil {
				t.Fatalf("被拒的 renew/result 不得改执行事实：%+v", still)
			}
			if busySlots(t, db, node.ID) != 1 {
				t.Fatalf("被拒的 result 不得释放槽位")
			}
		})
	}
}

// TestGrantStaleProposedSlotAndExpiredBindingPreserveNewAssignment 验 stale 绑定
// （旧 assignment / 错 proposed / 过期窗口）只被拒绝：新候选的身份/窗口原样保留，
// 且新候选随后仍可正常 grant。
func TestGrantStaleProposedSlotAndExpiredBindingPreserveNewAssignment(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "stale-binding")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-stale", "")
	now := store.Now()

	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-old", 1, now); err != nil {
		t.Fatalf("assign old: %v", err)
	}
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-new", 2, now+1); err != nil {
		t.Fatalf("assign new: %v", err)
	}
	before, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	auth := nodeAuthByID(t, db, node.ID)

	// 旧 assignment + 旧 proposed + 旧 accepted slot：拒绝，且不改新候选。
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task.ID, "asg-old", 1, 0, now+2); !errors.Is(err, store.ErrNoAssignment) {
		t.Fatalf("旧 assignment 的 grant 应 ErrNoAssignment，得到 %v", err)
	}
	// 新 assignment 但 proposed 对不上：同样拒绝（proposed 是候选身份的一部分）。
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task.ID, "asg-new", 1, 0, now+2); !errors.Is(err, store.ErrNoAssignment) {
		t.Fatalf("错 proposedAttempt 的 grant 应 ErrNoAssignment，得到 %v", err)
	}
	after, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if after.AssignmentID != "asg-new" || after.ProposedAttempt != 2 ||
		after.NodeID == nil || *after.NodeID != node.ID ||
		after.AssignmentExpiresAt == nil || *after.AssignmentExpiresAt != *before.AssignmentExpiresAt {
		t.Fatalf("stale grant 破坏了新候选：before=%+v after=%+v", before, after)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, auth, task.ID, "asg-new", 2, 0, now+2); err != nil || got != 1 {
		t.Fatalf("新候选应可正常 grant：attempt=%d err=%v", got, err)
	}

	// 过期 assignment：只在窗口到点时拒绝（expiry==now 也算过期），不耗 attempt/不占槽，
	// 清掉候选后可以重新派发。
	task2 := newTask(t, db, ns, c, "trace-expired", "")
	if err := db.AssignTaskAt(ctx, task2.ID, node.ID, "asg-expired", 1, now); err != nil {
		t.Fatalf("assign expired: %v", err)
	}
	expired, err := db.GetTask(ctx, ns, task2.ID)
	if err != nil {
		t.Fatalf("get task2: %v", err)
	}
	// 过期路径在容量检查之前拒绝：不需要空闲槽，也不会消耗 attempt/占槽。
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task2.ID, "asg-expired", 1, 0, *expired.AssignmentExpiresAt); !errors.Is(err, store.ErrNoAssignment) {
		t.Fatalf("expiry==now 的 assignment 应 ErrNoAssignment，得到 %v", err)
	}
	staleTask := mustPhase(t, db, ns, task2.ID, store.TaskPending)
	if staleTask.Attempt != 0 || staleTask.AssignmentID != "" || staleTask.AssignmentExpiresAt != nil {
		t.Fatalf("过期 assignment 应被清掉且不耗 attempt：%+v", staleTask)
	}
}

// TestGrantPerCapabilityAcrossNamespaces 验 perCapability 的口径是「跨 namespace 的
// 同名能力」：ns1/review 占满 review=1 后，ns2/review 不能再授权；不同能力名不受影响。
func TestGrantPerCapabilityAcrossNamespaces(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns1 := newNS(t, db, "cap-ns-1")
	ns2 := newNS(t, db, "cap-ns-2")
	node := newNode(t, db, "node-a", 2)
	putNodeCfg(t, db, node.ID, []string{"*"}, nil,
		map[string]any{"total": 2, "perCapability": map[string]any{"review": 1}})
	recordAppliedForNode(t, db, node.ID)

	review1 := createCap(t, db, testCap(ns1, "review", 1, nil))
	review2 := createCap(t, db, testCap(ns2, "review", 1, nil))
	other := createCap(t, db, testCap(ns2, "other", 1, nil))

	task1 := newTask(t, db, ns1, review1, "trace-1", "")
	grant(t, db, task1.ID, node.ID, "asg-1")

	task2 := newTask(t, db, ns2, review2, "trace-2", "")
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-2", 1); err != nil {
		t.Fatalf("assign task2: %v", err)
	}
	auth := nodeAuthByID(t, db, node.ID)
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task2.ID, "asg-2", 1, freeSlot(t, db, node.ID), store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("跨 ns 同名能力预算已满应 ErrSlotUnavailable，得到 %v", err)
	}
	rejected := mustPhase(t, db, ns2, task2.ID, store.TaskPending)
	if rejected.Attempt != 0 || rejected.WaitingReason != store.WaitingForSlot {
		t.Fatalf("按能力拒绝不得消耗 attempt，且应回 waiting_for_slot：%+v", rejected)
	}

	// 不同能力名不受 review 预算影响。
	task3 := newTask(t, db, ns2, other, "trace-3", "")
	if err := db.AssignTask(ctx, task3.ID, node.ID, "asg-3", 1); err != nil {
		t.Fatalf("assign task3: %v", err)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, auth, task3.ID, "asg-3", 1,
		freeSlot(t, db, node.ID), store.Now()); err != nil || got != 1 {
		t.Fatalf("不同能力名应可授权：attempt=%d err=%v", got, err)
	}
}

// TestGrantWildcardPerCapabilityAcrossNames 验 perCapability["*"] 是「未单列能力」的
// 默认**单能力**预算：口径仍是同能力名（跨 ns）的并发数，不是所有能力的合计。
func TestGrantWildcardPerCapabilityAcrossNames(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cap-wildcard")
	node := newNode(t, db, "node-a", 3)
	putNodeCfg(t, db, node.ID, []string{"*"}, nil,
		map[string]any{"total": 3, "perCapability": map[string]any{"*": 1}})
	recordAppliedForNode(t, db, node.ID)
	auth := nodeAuthByID(t, db, node.ID)

	perNode := 2 // 让 maxPerNode 不成为判别项：本用例只验 perCapability["*"]
	alphaSpec := testCap(ns, "alpha", 1, nil)
	alphaSpec.Spec.Concurrency.MaxPerNode = &perNode
	alpha := createCap(t, db, alphaSpec)
	betaSpec := testCap(ns, "beta", 1, nil)
	betaSpec.Spec.Concurrency.MaxPerNode = &perNode
	beta := createCap(t, db, betaSpec)

	// alpha 用掉自己的通配额度；beta 是**另一个**能力名，仍可用自己的 1。
	taskA := newTask(t, db, ns, alpha, "trace-a", "")
	grant(t, db, taskA.ID, node.ID, "asg-a")
	beta1 := newTask(t, db, ns, beta, "trace-b1", "")
	if err := db.AssignTask(ctx, beta1.ID, node.ID, "asg-b1", 1); err != nil {
		t.Fatalf("assign beta1: %v", err)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, auth, beta1.ID, "asg-b1", 1,
		freeSlot(t, db, node.ID), store.Now()); err != nil || got != 1 {
		t.Fatalf("未单列能力应使用自己的通配额度：attempt=%d err=%v", got, err)
	}

	// 第二个 beta 并发：同能力名已 1，通配额度满 → 拒绝且不耗 attempt。
	beta2 := newTask(t, db, ns, beta, "trace-b2", "")
	if err := db.AssignTask(ctx, beta2.ID, node.ID, "asg-b2", 1); err != nil {
		t.Fatalf("assign beta2: %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, beta2.ID, "asg-b2", 1,
		freeSlot(t, db, node.ID), store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("通配 perCapability=1 应拦住同能力的第二个并发，得到 %v", err)
	}
	pending := mustPhase(t, db, ns, beta2.ID, store.TaskPending)
	if pending.Attempt != 0 {
		t.Fatalf("通配拒绝不得消耗 attempt：%+v", pending)
	}
}

// TestGrantPinnedMaxPerNodeAcrossVersions 验 pinned maxPerNode 的口径：
// 同 namespace + 同能力名、**跨版本**算一份并发；不同能力名互不影响。
func TestGrantPinnedMaxPerNodeAcrossVersions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "cap-maxnode")
	node := newNode(t, db, "node-a", 2)

	perNode := 1
	v1 := testCap(ns, "deploy", 1, nil)
	v1.Spec.Concurrency.MaxPerNode = &perNode
	c1 := createCap(t, db, v1)
	v2 := testCap(ns, "deploy", 2, nil)
	v2.Spec.Concurrency.MaxPerNode = &perNode
	c2 := createCap(t, db, v2)
	other := testCap(ns, "probe", 1, nil)
	other.Spec.Concurrency.MaxPerNode = &perNode
	c3 := createCap(t, db, other)

	task1 := newTask(t, db, ns, c1, "trace-v1", "")
	grant(t, db, task1.ID, node.ID, "asg-1")

	task2 := newTask(t, db, ns, c2, "trace-v2", "")
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-2", 1); err != nil {
		t.Fatalf("assign v2: %v", err)
	}
	auth := nodeAuthByID(t, db, node.ID)
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task2.ID, "asg-2", 1,
		freeSlot(t, db, node.ID), store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("同 ns 同名跨版本 maxPerNode=1 应拒绝，得到 %v", err)
	}
	if got := mustPhase(t, db, ns, task2.ID, store.TaskPending); got.Attempt != 0 {
		t.Fatalf("pinned 拒绝不得消耗 attempt：%+v", got)
	}

	// 不同能力名：不受 deploy 的 maxPerNode 限制（总量 2 仍有余量）。
	task3 := newTask(t, db, ns, c3, "trace-other", "")
	if err := db.AssignTask(ctx, task3.ID, node.ID, "asg-3", 1); err != nil {
		t.Fatalf("assign other: %v", err)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, auth, task3.ID, "asg-3", 1,
		freeSlot(t, db, node.ID), store.Now()); err != nil || got != 1 {
		t.Fatalf("不同能力名应可授权：attempt=%d err=%v", got, err)
	}
}

// TestGrantCountsUnreapedExecutionsAndRealReservations 验占用口径：
//   - 未回收执行（即使任务已回 pending）仍占预算；
//   - 节点 Busy=true 的 reservation 落进真实槽账本并占预算；
//   - 自己的 accepted reservation 只计一次（grant 成功时原地变成 attempt）；
//   - 空 observation / rehello 空申报不能假释放。
func TestGrantCountsUnreapedExecutionsAndRealReservations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "budget-ledger")
	c := lifeCycleCap(t, db, ns, "idem", true, 3) // maxPerNode=2
	// total=3（3 个申报槽）、perCap idem=2：拒绝来自「未回收执行 + 真实 reservation」
	// 的同名能力预算，而不是 slot 编号越界。
	node := newNode(t, db, "node-a", 3)
	putNodeCfg(t, db, node.ID, []string{"*"}, nil,
		map[string]any{"total": 3, "perCapability": map[string]any{"idem": 2}})
	recordAppliedForNode(t, db, node.ID)

	task1 := newTask(t, db, ns, c, "trace-1", "")
	grant(t, db, task1.ID, node.ID, "asg-1")

	// 空 observation / 缺字段：不是进程回收事实。
	if n, err := db.ReclaimStaleSlots(ctx, node.ID, nil, store.Now()); err != nil || n != 0 {
		t.Fatalf("空 observations 不能回收：n=%d err=%v", n, err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("空事实后 busy 必须仍是 1，得到 %d", busySlots(t, db, node.ID))
	}

	// task1 租约过期 → 幂等能力回 pending，但旧 attempt 未回收：仍占预算（不看 phase）。
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task1.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	mustPhase(t, db, ns, task1.ID, store.TaskPending)

	// task2 取得节点 accept 的 reservation（Busy=true 事实），task3 只拿到中枢候选。
	task2 := newTask(t, db, ns, c, "trace-2", "")
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-2", 1); err != nil {
		t.Fatalf("assign task2: %v", err)
	}
	slot2 := freeSlot(t, db, node.ID)
	observeReservation(t, db, ns, node.ID, task2.ID, slot2)
	task3 := newTask(t, db, ns, c, "trace-3", "")
	if err := db.AssignTask(ctx, task3.ID, node.ID, "asg-3", 1); err != nil {
		t.Fatalf("assign task3: %v", err)
	}
	auth := nodeAuthByID(t, db, node.ID)

	// 预算：1 未回收 + 1 真实 reservation + 本次自己 = 3 > perCap idem 2 → 拒绝。
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task3.ID, "asg-3", 1,
		freeSlot(t, db, node.ID), store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("未回收执行 + 真实 reservation 应占满预算，得到 %v", err)
	}
	rejected := mustPhase(t, db, ns, task3.ID, store.TaskPending)
	if rejected.Attempt != 0 || rejected.WaitingReason != store.WaitingForSlot {
		t.Fatalf("预算拒绝不得消耗 attempt / 应回 waiting_for_slot：%+v", rejected)
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("拒绝不得改槽账本：busy=%d（1 执行 + 1 reservation）", busySlots(t, db, node.ID))
	}

	// 自己的 accepted reservation 只计一次：base=1（旧执行）+ 1（新执行）=2 ≤ perCap 2。
	lease2, token2, got2, _, err := db.GrantLeaseAuthorized(ctx, auth, task2.ID, "asg-2", 1, slot2, store.Now())
	if err != nil || got2 != 1 {
		t.Fatalf("自己的 reservation 只该算一次，grant 应成功：attempt=%d err=%v", got2, err)
	}
	if lease2 == "" || token2 == "" {
		t.Fatalf("grant 事实不完整")
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("grant 后槽数不变（reservation 原地变成执行）：busy=%d", busySlots(t, db, node.ID))
	}
	att2, err := db.GetTaskAttempt(ctx, task2.ID, got2)
	if err != nil || att2.Slot != slot2 || att2.LeaseID != lease2 {
		t.Fatalf("reservation 必须原地变成该 attempt 的执行绑定：%+v err=%v", att2, err)
	}

	// 节点重连只申报空实例集（rehello 空）不能释放任何占用。
	if _, err := db.UpsertInstances(ctx, node.ID, []store.InstanceSpec{}); err != nil {
		t.Fatalf("empty rehello: %v", err)
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("rehello 空申报不得假释放：busy=%d", busySlots(t, db, node.ID))
	}
	// 旧 attempt 仍未回收 + task2 正在跑：预算已满，task3 重新派发也会被拒。
	if err := db.AssignTask(ctx, task3.ID, node.ID, "asg-3b", 1); err != nil {
		t.Fatalf("re-assign task3: %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task3.ID, "asg-3b", 1,
		freeSlot(t, db, node.ID), store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("未回收执行 + 新执行占满预算，task3 应 ErrSlotUnavailable，得到 %v", err)
	}
}

// TestConcurrentNarrowBudgetGrantsExactlyOne 验同预算的并发 grant 恰一个成功：
// 拒绝方不计 attempt、不占槽、不改成功方的绑定。
//
// 判别项必须是**预算本身**：两个任务同能力名、各自拿到一个**真正合法的空闲槽**
// （slot 0/1，机器总量 2，两个槽都 < total），pinned maxPerNode=2 对两个并发仍有
// 余量；唯一能拦住第二个的是 perCapability["idem"]=1。这样在预算检查被移除时，
// 两个 grant 都能各自绑上自己的合法槽（success=2 → 红），不会被 slot>=total 的
// 范围规则顺手拒绝掉。
func TestConcurrentNarrowBudgetGrantsExactlyOne(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "budget-race")
	node := newNode(t, db, "node-a", 2)
	putNodeCfg(t, db, node.ID, []string{"*"}, nil,
		map[string]any{"total": 2, "perCapability": map[string]any{"idem": 1}})
	recordAppliedForNode(t, db, node.ID)

	// 同一能力名（跨 ns 同名能力的 perCapability 口径）；pinned maxPerNode=2 不参与判别。
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	taskA := newTask(t, db, ns, c, "trace-a", "")
	taskB := newTask(t, db, ns, c, "trace-b", "")
	if err := db.AssignTask(ctx, taskA.ID, node.ID, "asg-a", 1); err != nil {
		t.Fatalf("assign A: %v", err)
	}
	if err := db.AssignTask(ctx, taskB.ID, node.ID, "asg-b", 1); err != nil {
		t.Fatalf("assign B: %v", err)
	}

	auth := nodeAuthByID(t, db, node.ID)
	slots := []int{0, 1} // 两个都是合法空闲槽（slot < total=2）
	results := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			taskID, assignmentID := taskA.ID, "asg-a"
			if i == 1 {
				taskID, assignmentID = taskB.ID, "asg-b"
			}
			_, _, _, _, results[i] = db.GrantLeaseAuthorized(ctx, auth, taskID, assignmentID, 1, slots[i], store.Now())
		}(i)
	}
	close(start)
	wg.Wait()

	success, rejected, loser := 0, 0, -1
	for i, rerr := range results {
		if rerr == nil {
			success++
			continue
		}
		if !errors.Is(rerr, store.ErrSlotUnavailable) {
			t.Fatalf("并发窄预算的失败方应是 ErrSlotUnavailable，得到 %v", rerr)
		}
		rejected, loser = rejected+1, i
	}
	if success != 1 || rejected != 1 {
		t.Fatalf("同预算并发 grant 应恰一个成功：success=%d rejected=%d", success, rejected)
	}
	// 被拒那一方的合法槽必须仍然空闲（预算拒绝不占槽）。
	if busyTask, _, _, _, _ := instanceLedger(t, db, node.ID, slots[loser]); busyTask != "" {
		t.Fatalf("被预算拒绝的 slot %d 不得被占：%q", slots[loser], busyTask)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("并发后只该有一个槽被占用，busy=%d", busySlots(t, db, node.ID))
	}
	taskIDs := []string{taskA.ID, taskB.ID}
	for i, taskID := range taskIDs {
		task, err := db.GetTask(ctx, ns, taskID)
		if err != nil {
			t.Fatalf("get task %s: %v", taskID, err)
		}
		if results[i] != nil {
			if task.Attempt != 0 || task.Phase != store.TaskPending || task.WaitingReason != store.WaitingForSlot {
				t.Fatalf("被拒任务应回 pending/waiting_for_slot 且 attempt=0：%+v", task)
			}
			continue
		}
		if task.Attempt != 1 || task.Phase != store.TaskRunning || task.LeaseID == "" {
			t.Fatalf("成功任务应 running 且带租约：%+v", task)
		}
		att, aerr := db.GetTaskAttempt(ctx, taskID, 1)
		if aerr != nil || att.Slot != slots[i] {
			t.Fatalf("成功方必须绑在自己那个合法 slot %d 上：%+v err=%v", slots[i], att, aerr)
		}
	}
}

// TestGrantOwnReservationCountedOnceAtItsOwnSlot 是「自己的 reservation 只算一次」
// 的正向面：node+task+assignment+proposed+**同 slot** 精确匹配的那一条被排除，
// grant 成功后它原地变成 attempt（槽数与预算都不重复计）。
func TestGrantOwnReservationCountedOnceAtItsOwnSlot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "own-slot-once")
	c := lifeCycleCap(t, db, ns, "idem", true, 3) // maxPerNode=2
	node := newNode(t, db, "node-a", 2)
	putNodeCfg(t, db, node.ID, []string{"*"}, nil,
		map[string]any{"total": 2, "perCapability": map[string]any{"idem": 2}})
	recordAppliedForNode(t, db, node.ID)

	// 1 条未回收执行已占掉一个名额。
	task1 := newTask(t, db, ns, c, "trace-1", "")
	grant(t, db, task1.ID, node.ID, "asg-1")

	// task2 的 reservation 落在 slot 1；grant 要绑的正是这个槽。
	task2 := newTask(t, db, ns, c, "trace-2", "")
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-2", 1); err != nil {
		t.Fatalf("assign task2: %v", err)
	}
	observeReservation(t, db, ns, node.ID, task2.ID, 1)
	auth := nodeAuthByID(t, db, node.ID)

	lease, _, got, _, err := db.GrantLeaseAuthorized(ctx, auth, task2.ID, "asg-2", 1, 1, store.Now())
	if err != nil || got != 1 {
		t.Fatalf("自己的同槽 reservation 只该算一次，grant 应成功：attempt=%d err=%v", got, err)
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("reservation 原地变成执行，槽数不变：busy=%d", busySlots(t, db, node.ID))
	}
	taskID, _, leaseID, assignmentID, expiresAt := instanceLedger(t, db, node.ID, 1)
	if taskID != task2.ID || leaseID != lease || assignmentID != "" || expiresAt != 0 {
		t.Fatalf("grant 后槽账本必须是执行绑定（无 assignment/窗口）：task=%q lease=%q assignment=%q expires=%d",
			taskID, leaseID, assignmentID, expiresAt)
	}
}

// TestGrantCountsOwnReservationOutsideItsSlot 是「只排除同槽那一条」的反向面：
// 同一 task 在**别的槽**上的 reservation（哪怕是它自己的候选）仍是真实物理占用，
// 必须计入预算；按 taskID 整包排除会少算并越过 perCapability。
func TestGrantCountsOwnReservationOutsideItsSlot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "own-slot-other")
	c := lifeCycleCap(t, db, ns, "idem", true, 3) // maxPerNode=2
	node := newNode(t, db, "node-a", 4)
	putNodeCfg(t, db, node.ID, []string{"*"}, nil,
		map[string]any{"total": 4, "perCapability": map[string]any{"idem": 2}})
	recordAppliedForNode(t, db, node.ID)

	// 预算 2：一条未回收执行已占掉一个名额。
	task1 := newTask(t, db, ns, c, "trace-1", "")
	grant(t, db, task1.ID, node.ID, "asg-1")

	// task2 在 slot 2、slot 3 各有一条节点已接受的 reservation（两个真实物理事实）。
	task2 := newTask(t, db, ns, c, "trace-2", "")
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-2", 1); err != nil {
		t.Fatalf("assign task2: %v", err)
	}
	observeReservation(t, db, ns, node.ID, task2.ID, 2)
	observeReservation(t, db, ns, node.ID, task2.ID, 3)
	auth := nodeAuthByID(t, db, node.ID)

	// grant 绑 slot 1（合法空闲）：1 执行 + 2 条真实 reservation + 本次 > 2 → 必须拒。
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task2.ID, "asg-2", 1, 1, store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("同 task 在别的槽上的 reservation 必须计入预算，得到 %v", err)
	}
	rejected := mustPhase(t, db, ns, task2.ID, store.TaskPending)
	if rejected.Attempt != 0 || rejected.WaitingReason != store.WaitingForSlot {
		t.Fatalf("预算拒绝不得消耗 attempt / 应回 waiting_for_slot：%+v", rejected)
	}
	// 被拒后所有真实预约原样保留（不因拒绝被清），且拒绝没有偷偷占 slot 1。
	if busySlots(t, db, node.ID) != 3 {
		t.Fatalf("1 执行 + 2 reservation 的账本必须原样保留：busy=%d", busySlots(t, db, node.ID))
	}
	taskID, attempt, lease, assignment, _ := instanceLedger(t, db, node.ID, 2)
	if taskID != task2.ID || attempt != 1 || lease != "" || assignment != "asg-2" {
		t.Fatalf("slot 2 的 reservation 账本被改了：task=%q attempt=%d lease=%q assignment=%q",
			taskID, attempt, lease, assignment)
	}
	if taskID, _, _, _, _ := instanceLedger(t, db, node.ID, 1); taskID != "" {
		t.Fatalf("被预算拒绝不得占 slot 1：%q", taskID)
	}
}

// TestReservationLedgerBindsAssignmentIdentity 验 reservation 账本绑定 assignment 身份：
// 旧 assignment 的 record / idle 都不能清掉新 assignment 的预约，旧 assignment 的预约
// 也不能被同 task、同 proposed 的新候选 grant 复用。
func TestReservationLedgerBindsAssignmentIdentity(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "reservation-assignment")
	// maxPerNode=3：本用例的判别点是 assignment 身份，不是预算。
	c := capWithPinnedMaxPerNode(t, db, ns, "idem", 3, 3)
	node := newNode(t, db, "node-a", 3)
	auth := nodeAuthByID(t, db, node.ID)

	task := newTask(t, db, ns, c, "trace-1", "")
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-old", 1); err != nil {
		t.Fatalf("assign old: %v", err)
	}
	observeReservation(t, db, ns, node.ID, task.ID, 0)
	// 同 task、同 proposed，但换新 assignment：新候选在新槽上留下自己的预约。
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-new", 1); err != nil {
		t.Fatalf("assign new: %v", err)
	}
	observeReservation(t, db, ns, node.ID, task.ID, 1)
	if _, _, lease, assignment, _ := instanceLedger(t, db, node.ID, 1); lease != "" || assignment != "asg-new" {
		t.Fatalf("新 reservation 应绑定新 assignment：lease=%q assignment=%q", lease, assignment)
	}

	// 旧 assignment 的候选本身已经不可授权。
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task.ID, "asg-old", 1, 0, store.Now()); !errors.Is(err, store.ErrNoAssignment) {
		t.Fatalf("旧 assignment 的 grant 应 ErrNoAssignment，得到 %v", err)
	}

	busyTrue := true
	if n, err := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{{
		Slot: 1, Busy: &busyTrue, TaskID: task.ID, AssignmentID: "asg-old", ProposedAttempt: 1,
	}}, store.Now()); err != nil || n != 0 {
		t.Fatalf("旧 assignment 的 Busy=true 观测不该回收：n=%d err=%v", n, err)
	}
	if _, _, lease, assignment, _ := instanceLedger(t, db, node.ID, 1); lease != "" || assignment != "asg-new" {
		t.Fatalf("旧 assignment 的观测不得覆盖新 reservation：lease=%q assignment=%q", lease, assignment)
	}

	busyFalse := false
	if n, err := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{{
		Slot: 1, Busy: &busyFalse, TaskID: task.ID, AssignmentID: "asg-old", ProposedAttempt: 1,
	}}, store.Now()); err != nil || n != 0 {
		t.Fatalf("旧 assignment 的 idle 不该清预约：n=%d err=%v", n, err)
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("旧 assignment 的 idle 不得清新 reservation：busy=%d", busySlots(t, db, node.ID))
	}

	// 旧 assignment 留下的 slot 0 预约不能被新候选（同 task、同 proposed）复用。
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task.ID, "asg-new", 1, 0, store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("旧 assignment 的预约不得被新候选复用，得到 %v", err)
	}
	if taskID, attempt, lease, assignment, _ := instanceLedger(t, db, node.ID, 0); taskID != task.ID ||
		attempt != 1 || lease != "" || assignment != "asg-old" {
		t.Fatalf("旧预约账本必须原样保留：task=%q attempt=%d lease=%q assignment=%q",
			taskID, attempt, lease, assignment)
	}

	// 精确 assignment 的 idle 才能清掉它自己的 reservation。
	if n, err := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{{
		Slot: 0, Busy: &busyFalse, TaskID: task.ID, AssignmentID: "asg-old", ProposedAttempt: 1,
	}}, store.Now()); err != nil || n != 1 {
		t.Fatalf("精确 assignment 的 idle 应清掉旧预约：n=%d err=%v", n, err)
	}
	// 清掉后新候选可以正常 grant 到 slot 0。
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-new", 1); err != nil {
		t.Fatalf("re-assign new: %v", err)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, auth, task.ID, "asg-new", 1, 0, store.Now()); err != nil || got != 1 {
		t.Fatalf("旧预约清除后新候选应可授权：attempt=%d err=%v", got, err)
	}
}

// TestReservationExpiryOnlyClearsLeaseFreeFacts 验 reservation 的 recorded 窗口
// 到期只能清**从未 grant 的 reservation**（busy_lease_id 为空 + 窗口已过）；真实执行
// （有 lease）即使租约过期/任务相位变化也永远不被到期规则清掉。
func TestReservationExpiryOnlyClearsLeaseFreeFacts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "reservation-expiry")
	c := lifeCycleCap(t, db, ns, "idem", true, 3) // maxPerNode=2
	node := newNode(t, db, "node-a", 3)

	// 旧 assignment 的预约：节点已接住，但一直没 grant。
	task := newTask(t, db, ns, c, "trace-1", "")
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-old", 1); err != nil {
		t.Fatalf("assign old: %v", err)
	}
	observeReservation(t, db, ns, node.ID, task.ID, 0)
	old, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || old.AssignmentExpiresAt == nil {
		t.Fatalf("读旧候选窗口：%+v err=%v", old, err)
	}
	oldExpiry := *old.AssignmentExpiresAt
	if _, _, lease, assignment, expiresAt := instanceLedger(t, db, node.ID, 0); lease != "" ||
		assignment != "asg-old" || expiresAt != oldExpiry {
		t.Fatalf("reservation 必须记录 assignment 与有界窗口：lease=%q assignment=%q expires=%d want=%d",
			lease, assignment, expiresAt, oldExpiry)
	}

	// 新候选（同 task、同 proposed，新 assignment）在旧预约窗口过期后接管同一物理槽。
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-new", 1, oldExpiry+10); err != nil {
		t.Fatalf("assign new: %v", err)
	}
	busyTrue := true
	if n, err := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{{
		Slot: 0, Busy: &busyTrue, TaskID: task.ID, AssignmentID: "asg-new", ProposedAttempt: 1,
	}}, oldExpiry+11); err != nil || n != 0 {
		t.Fatalf("记录新 reservation 不是回收：n=%d err=%v", n, err)
	}
	if _, _, lease, assignment, expiresAt := instanceLedger(t, db, node.ID, 0); lease != "" ||
		assignment != "asg-new" || expiresAt != oldExpiry+40 {
		t.Fatalf("过期 lease-free reservation 应被新候选替换：lease=%q assignment=%q expires=%d",
			lease, assignment, expiresAt)
	}

	// 真实执行（有 lease）不受到期规则影响：换候选观测/相位变化都不能清它。
	running := newTask(t, db, ns, c, "trace-2", "")
	runLease, _, runAttempt := grant(t, db, running.ID, node.ID, "asg-run")
	spare := newTask(t, db, ns, c, "trace-3", "")
	if err := db.AssignTask(ctx, spare.ID, node.ID, "asg-spare", 1); err != nil {
		t.Fatalf("assign spare: %v", err)
	}
	if n, err := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{{
		Slot: 1, Busy: &busyTrue, TaskID: spare.ID, AssignmentID: "asg-spare", ProposedAttempt: 1,
	}}, store.Now()); err != nil || n != 0 {
		t.Fatalf("reservation 观测不得覆盖执行绑定：n=%d err=%v", n, err)
	}
	if taskID, attempt, lease, assignment, expiresAt := instanceLedger(t, db, node.ID, 1); taskID != running.ID ||
		attempt != runAttempt || lease != runLease || assignment != "" || expiresAt != 0 {
		t.Fatalf("执行绑定被改写了：task=%q attempt=%d lease=%q assignment=%q expires=%d",
			taskID, attempt, lease, assignment, expiresAt)
	}
	// 租约过期 + 相位回收也不能清槽/清 reaped 事实（unreaped 只能由精确绑定回收）。
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, running.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	mustPhase(t, db, ns, running.ID, store.TaskPending)
	if taskID, _, lease, _, _ := instanceLedger(t, db, node.ID, 1); taskID != running.ID || lease != runLease {
		t.Fatalf("相位回收不得清执行绑定：task=%q lease=%q", taskID, lease)
	}
	if att, err := db.GetTaskAttempt(ctx, running.ID, runAttempt); err != nil || att.ReapedAt != nil {
		t.Fatalf("相位变化不得假写 reaped：%+v err=%v", att, err)
	}
}

// TestReservationCountsPerCapabilityAndPinnedMaxPerNode 把「真实 reservation 进入
// 预算」按维度隔离开：perCapability 用例的 total/pinned 都有余量，只有 perCap 能拒；
// maxPerNode 用例的 total/perCap 都有余量，只有 pinned 维度能拒。
func TestReservationCountsPerCapabilityAndPinnedMaxPerNode(t *testing.T) {
	setup := func(t *testing.T, slug string, perNode int, limits map[string]any) (*store.DB, context.Context, string, *store.Node, *store.CapabilityVersion) {
		t.Helper()
		db := openTestDB(t)
		ctx := context.Background()
		ns := newNS(t, db, slug)
		c := capWithPinnedMaxPerNode(t, db, ns, "idem", perNode, 3)
		node := newNode(t, db, "node-a", 3)
		putNodeCfg(t, db, node.ID, []string{"*"}, nil, limits)
		recordAppliedForNode(t, db, node.ID)
		return db, ctx, ns, node, c
	}

	t.Run("perCapability", func(t *testing.T) {
		// total=3 / maxPerNode=3：两者对 3 个占用都还有余量，只有 perCap idem=2 能拒。
		db, ctx, ns, node, c := setup(t, "reservation-percap", 3,
			map[string]any{"total": 3, "perCapability": map[string]any{"idem": 2}})
		assertReservationRejectedOnDimension(t, db, ctx, ns, node.ID, c)
	})
	t.Run("maxPerNode", func(t *testing.T) {
		// total=3 / perCap idem=3：两者对 3 个占用都还有余量，只有 pinned=2 能拒。
		db, ctx, ns, node, c := setup(t, "reservation-maxnode", 2,
			map[string]any{"total": 3, "perCapability": map[string]any{"idem": 3}})
		assertReservationRejectedOnDimension(t, db, ctx, ns, node.ID, c)
	})
}

// assertReservationRejectedOnDimension 造「1 条未回收执行 + 1 条真实 reservation +
// 本次授权」= 3 个占用，并断言 grant 在合法空闲 slot 上被拒。调用方负责让 total 与
// 另一个维度都有余量（>=3），因此拒绝只能来自剩下的那个维度。
func assertReservationRejectedOnDimension(t *testing.T, db *store.DB, ctx context.Context,
	nsID, nodeID string, c *store.CapabilityVersion) {
	t.Helper()
	task1 := newTask(t, db, nsID, c, "trace-1", "")
	grant(t, db, task1.ID, nodeID, "asg-1")
	task2 := newTask(t, db, nsID, c, "trace-2", "")
	if err := db.AssignTask(ctx, task2.ID, nodeID, "asg-2", 1); err != nil {
		t.Fatalf("assign task2: %v", err)
	}
	observeReservation(t, db, nsID, nodeID, task2.ID, 2)
	task3 := newTask(t, db, nsID, c, "trace-3", "")
	if err := db.AssignTask(ctx, task3.ID, nodeID, "asg-3", 1); err != nil {
		t.Fatalf("assign task3: %v", err)
	}
	auth := nodeAuthByID(t, db, nodeID)
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task3.ID, "asg-3", 1, 1, store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("1 执行 + 1 reservation + 本次应被唯一有余量的维度拒绝，得到 %v", err)
	}
	if got := mustPhase(t, db, nsID, task3.ID, store.TaskPending); got.Attempt != 0 {
		t.Fatalf("预算拒绝不得消耗 attempt：%+v", got)
	}
}

// TestGrantTotalUsesLocalOverNodeLimits 验机器总量预算是 local > node 的**合并**值，
// 并且这个合并值真的在 grant 的预算事务里拒绝：node limits.total=3、local
// limits.total=2 → 生效 2。第三个合法槽（slot 1 < 2）被拒；若合并退化成只看 node，
// grant 会以 total=3 通过并绑上 slot 1。
func TestGrantTotalUsesLocalOverNodeLimits(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "total-merge")
	c := capWithPinnedMaxPerNode(t, db, ns, "idem", 3, 3)
	node := newNode(t, db, "node-a", 3)
	// 节点默认值 total=3 来自 newNode；hello 上报的本机快照 local_limits.total=2
	// 是更外层生效值（local > node）。
	if err := db.RecordHello(ctx, node.ID, store.NodeFacts{}, nil, []string{"*"}, nil,
		map[string]any{"total": 2}, "", ""); err != nil {
		t.Fatalf("record hello (local limits): %v", err)
	}
	recordAppliedForNode(t, db, node.ID)

	// 1 条未回收执行（slot 0）+ 1 条真实 reservation（slot 2，申报槽）→ 生效 total=2 已满。
	task1 := newTask(t, db, ns, c, "trace-1", "")
	grant(t, db, task1.ID, node.ID, "asg-1")
	task2 := newTask(t, db, ns, c, "trace-2", "")
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-2", 1); err != nil {
		t.Fatalf("assign task2: %v", err)
	}
	observeReservation(t, db, ns, node.ID, task2.ID, 2)

	// slot 1 是合法空闲槽（1 < 2），因此拒绝只能来自合并后的 total=2。
	task3 := newTask(t, db, ns, c, "trace-3", "")
	if err := db.AssignTask(ctx, task3.ID, node.ID, "asg-3", 1); err != nil {
		t.Fatalf("assign task3: %v", err)
	}
	auth := nodeAuthByID(t, db, node.ID)
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, task3.ID, "asg-3", 1, 1, store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("local total=2 必须覆盖 node total=3 并拒绝第三次授权，得到 %v", err)
	}
	rejected := mustPhase(t, db, ns, task3.ID, store.TaskPending)
	if rejected.Attempt != 0 || rejected.WaitingReason != store.WaitingForSlot {
		t.Fatalf("总量拒绝不得消耗 attempt / 应回 waiting_for_slot：%+v", rejected)
	}
	if taskID, _, _, _, _ := instanceLedger(t, db, node.ID, 1); taskID != "" {
		t.Fatalf("总量拒绝不得占 slot 1：%q", taskID)
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("1 执行 + 1 reservation 账本必须原样保留：busy=%d", busySlots(t, db, node.ID))
	}
}

// TestRenewAndResultCannotReviveExpiredOrCancelled 验到期与取消后授权不可复活：
// expiry==now 的 renew / result 一律拒；取消后 renew 也拒、result 由 cancel.ack 裁定。
func TestRenewAndResultCannotReviveExpiredOrCancelled(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "no-revive")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	auth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}

	task := newTask(t, db, ns, c, "trace-1", "")
	leaseID, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")
	before, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || before.LeaseUntil == nil {
		t.Fatalf("get task: %+v err=%v", before, err)
	}
	expiry := *before.LeaseUntil
	// expiry==now 不得续租（不复活），授权事实原样保留。
	if until, err := db.RenewLeaseAuthorized(ctx, auth, task.ID, attempt, leaseID, expiry); !errors.Is(err, store.ErrLeaseMismatch) || until != 0 {
		t.Fatalf("expiry==now 的 renew 应 ErrLeaseMismatch，得到 until=%d err=%v", until, err)
	}
	after, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || after.LeaseUntil == nil || *after.LeaseUntil != expiry {
		t.Fatalf("被拒的 renew 不得改到期时刻：%+v err=%v", after, err)
	}
	// 到期前一刻仍可续租（证明不是一律拒绝）。
	expires2, err := db.RenewLeaseAuthorized(ctx, auth, task.ID, attempt, leaseID, expiry-1)
	if err != nil || expires2 != expiry-1+int64(protocol.LeaseTTL.Seconds()) {
		t.Fatalf("到期前续租应成功：until=%d err=%v", expires2, err)
	}
	// expiry==now 的 result 也必须拒（旧执行不得写终态/复活）。
	if err := db.CompleteTaskAuthorized(ctx, auth, task.ID, attempt, leaseID, expires2,
		store.TaskResult{Phase: store.TaskSucceeded}); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("expiry==now 的 result 应 ErrLeaseMismatch，得到 %v", err)
	}
	// 有效窗口内的 result 正常。
	if err := db.CompleteTaskAuthorized(ctx, auth, task.ID, attempt, leaseID, expires2-1,
		store.TaskResult{Phase: store.TaskSucceeded}); err != nil {
		t.Fatalf("有效窗口内的 result 应成功：%v", err)
	}

	// 取消：renew 立刻拒、result 由 cancel.ack 裁定；租约不被清理/复活。
	task2 := newTask(t, db, ns, c, "trace-2", "")
	lease2, _, attempt2 := grant(t, db, task2.ID, node.ID, "asg-2")
	if err := db.RequestCancel(ctx, task2.ID, "cancelled_by_user", store.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	cancelled, err := db.GetTask(ctx, ns, task2.ID)
	if err != nil || cancelled.LeaseUntil == nil {
		t.Fatalf("get cancelled task: %+v err=%v", cancelled, err)
	}
	leaseUntil := *cancelled.LeaseUntil
	if _, err := db.RenewLeaseAuthorized(ctx, auth, task2.ID, attempt2, lease2, store.Now()); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("取消后 renew 应 ErrLeaseMismatch，得到 %v", err)
	}
	if err := db.CompleteTaskAuthorized(ctx, auth, task2.ID, attempt2, lease2, store.Now(),
		store.TaskResult{Phase: store.TaskSucceeded}); !errors.Is(err, store.ErrTaskNotRunning) {
		t.Fatalf("取消后 result 应 ErrTaskNotRunning，得到 %v", err)
	}
	still := mustPhase(t, db, ns, task2.ID, store.TaskRunning)
	if still.LeaseUntil == nil || *still.LeaseUntil != leaseUntil {
		t.Fatalf("取消后的拒绝不得清租约（等可信回收事实）：%+v", still)
	}
}

// TestAckCancelAuthorizedHistoricalBinding 验认证历史 ACK：
// 错 lease/slot 拒（fail closed）；正确的旧绑定只回收旧槽（不改新 attempt）；
// 重复幂等；false 不释放；当前取消 + true 原子终结。
func TestAckCancelAuthorizedHistoricalBinding(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ack-history")
	c := lifeCycleCap(t, db, ns, "idem", true, 3) // maxPerNode=2
	node := newNode(t, db, "node-a", 2)

	ledger := func() [][5]any {
		rows := make([][5]any, 2)
		for slot := range rows {
			taskID, attempt, leaseID, assignmentID, expiresAt := instanceLedger(t, db, node.ID, slot)
			rows[slot] = [5]any{taskID, attempt, leaseID, assignmentID, expiresAt}
		}
		return rows
	}
	auth := nodeAuthByID(t, db, node.ID)

	task := newTask(t, db, ns, c, "trace-1", "")
	lease1, _, attempt1 := grant(t, db, task.ID, node.ID, "asg-1")
	att1, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get attempt1: %v", err)
	}
	// 租约过期 → 重排 → 第二次授权（旧槽仍被旧 attempt 占着）。
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET next_attempt_at = NULL WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if _, _, attempt2 := regrantOnFreeSlot(t, db, task.ID, node.ID, "asg-2"); attempt2 != 2 {
		t.Fatalf("第二次 attempt = %d", attempt2)
	}
	current, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("两个 attempt 应各占一槽，busy=%d", busySlots(t, db, node.ID))
	}

	// 错 lease / 错 slot：拒绝且一个槽都不放。
	for _, bad := range []store.ReapedInputs{
		{TaskID: task.ID, Attempt: attempt1, NodeID: node.ID, LeaseID: "lse_bogus", Slot: att1.Slot, InstanceID: att1.InstanceID},
		{TaskID: task.ID, Attempt: attempt1, NodeID: node.ID, LeaseID: lease1, Slot: att1.Slot + 1, InstanceID: att1.InstanceID},
		{TaskID: task.ID, Attempt: attempt1, NodeID: node.ID, LeaseID: lease1, Slot: att1.Slot, InstanceID: "inst_bogus"},
	} {
		bad.Now = store.Now()
		if err := db.AckCancelAuthorized(ctx, auth, bad, true); !errors.Is(err, store.ErrLeaseMismatch) {
			t.Fatalf("错绑定 ack 应 ErrLeaseMismatch，得到 %v（%+v）", err, bad)
		}
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("被拒的 ack 不得释放槽位：busy=%d", busySlots(t, db, node.ID))
	}

	// 正确旧绑定：只回收旧槽，新 attempt 的相位/lease/token 原样保留。
	oldBefore, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get historical attempt: %v", err)
	}
	newBefore, err := db.GetTaskAttempt(ctx, task.ID, 2)
	if err != nil {
		t.Fatalf("get new attempt: %v", err)
	}
	ledgerBefore := ledger()
	oldBinding := ackReapInput(t, db, node.ID, task.ID, attempt1)
	oldBinding.Now = store.Now()
	if err := db.AckCancelAuthorized(ctx, auth, oldBinding, true); err != nil {
		t.Fatalf("正确的历史 ack 应成功：%v", err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("历史 ack 只该释放旧槽，busy=%d", busySlots(t, db, node.ID))
	}
	running := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	if running.Attempt != 2 || running.LeaseID != current.LeaseID || running.AttemptTokenHash != current.AttemptTokenHash {
		t.Fatalf("历史 ack 不得改新 attempt 的相位/token：%+v", running)
	}
	att2, err := db.GetTaskAttempt(ctx, task.ID, 2)
	if err != nil || att2.ReapedAt != nil {
		t.Fatalf("新 attempt 不该被回收：%+v err=%v", att2, err)
	}
	oldReaped, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil || oldReaped.ReapedAt == nil || *oldReaped.ReapedAt != oldBinding.Now {
		t.Fatalf("首历史 ACK 必须写精确 ReapedAt=%d：%+v err=%v", oldBinding.Now, oldReaped, err)
	}
	expectedOld := *oldBefore
	expectedOld.ReapedAt = &oldBinding.Now
	expectedOld.ReclaimReason = store.ReclaimReasonCancelAck
	if !reflect.DeepEqual(&expectedOld, oldReaped) || !reflect.DeepEqual(current, running) ||
		!reflect.DeepEqual(newBefore, att2) {
		t.Fatalf("首历史 ACK 只能补旧回收事实，不得改旧失败/新任务/新 attempt：old=%+v new=%+v task=%+v", oldReaped, att2, running)
	}
	ledgerAfter := ledger()
	ledgerBefore[oldBinding.Slot] = [5]any{"", 0, "", "", int64(0)}
	if !reflect.DeepEqual(ledgerBefore, ledgerAfter) {
		t.Fatalf("首历史 ACK 只能清旧槽，新槽账本须完整保持：before=%+v after=%+v", ledgerBefore, ledgerAfter)
	}
	// 重复回收幂等（不同 now/reason，不再写任何事实、不报错）。
	replayOld := oldBinding
	replayOld.Now += 1000
	replayOld.Reason = "duplicate_ack"
	if err := db.AckCancelAuthorized(ctx, auth, replayOld, true); err != nil {
		t.Fatalf("重复历史 ack 应幂等：%v", err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("重复 ack 不得再动槽位，busy=%d", busySlots(t, db, node.ID))
	}

	oldAgain, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil || !reflect.DeepEqual(oldReaped, oldAgain) {
		t.Fatalf("重复历史 ACK 不得改整旧 attempt：before=%+v after=%+v err=%v", oldReaped, oldAgain, err)
	}
	newAgain, err := db.GetTaskAttempt(ctx, task.ID, 2)
	if err != nil || !reflect.DeepEqual(att2, newAgain) {
		t.Fatalf("重复历史 ACK 不得改整新 attempt：before=%+v after=%+v err=%v", att2, newAgain, err)
	}
	if taskAgain := mustPhase(t, db, ns, task.ID, store.TaskRunning); !reflect.DeepEqual(running, taskAgain) {
		t.Fatalf("重复历史 ACK 不得改整任务：before=%+v after=%+v", running, taskAgain)
	}
	if ledgerAgain := ledger(); !reflect.DeepEqual(ledgerAfter, ledgerAgain) {
		t.Fatalf("重复历史 ACK 不得改任一槽的完整账本：before=%+v after=%+v", ledgerAfter, ledgerAgain)
	}

	// false：只是「收到了」，不改相位、不释放槽、不清租约。
	if err := db.RequestCancel(ctx, task.ID, "cancelled_by_user", store.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	curBinding := ackReapInput(t, db, node.ID, task.ID, 2)
	curBinding.Now = store.Now()
	if err := db.AckCancelAuthorized(ctx, auth, curBinding, false); err != nil {
		t.Fatalf("reaped=false 的 ack 应被接受但不处理：%v", err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("reaped=false 不得释放槽位")
	}
	if got := mustPhase(t, db, ns, task.ID, store.TaskRunning); got.CancelRequestedAt == nil {
		t.Fatalf("reaped=false 后仍应是 running + 取消意图：%+v", got)
	}
	// 当前取消 + true：同事务原子终结并释放它自己的槽。
	if err := db.AckCancelAuthorized(ctx, auth, curBinding, true); err != nil {
		t.Fatalf("当前取消的 ack 应成功：%v", err)
	}
	done := mustPhase(t, db, ns, task.ID, store.TaskCancelled)
	if done.Error != "cancelled_by_user" {
		t.Fatalf("取消终态应带用户原因，得到 %q", done.Error)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("当前取消的 ack 应释放最后槽位")
	}

	// 当前取消的首 ACK 已经原子 finalize；第二次 ACK（不同 now/reason）必须逐字节幂等：
	// attempt 整行（含 phase/error/finished_at/reaped_at/reclaim_reason）、任务终态与槽都不变。
	curReaped, err := db.GetTaskAttempt(ctx, task.ID, 2)
	if err != nil || curReaped.ReapedAt == nil || *curReaped.ReapedAt != curBinding.Now {
		t.Fatalf("当前取消的首 ACK 必须写 reaped_at：%+v err=%v", curReaped, err)
	}
	terminalLedger := ledger()
	replayCurrent := curBinding
	replayCurrent.Now = curBinding.Now + 1000
	replayCurrent.Reason = "duplicate_ack"
	if err := db.AckCancelAuthorized(ctx, auth, replayCurrent, true); err != nil {
		t.Fatalf("重复当前取消 ack 应幂等：%v", err)
	}
	curReapedAgain, err := db.GetTaskAttempt(ctx, task.ID, 2)
	if err != nil || !reflect.DeepEqual(curReaped, curReapedAgain) {
		t.Fatalf("重复当前取消 ACK 不得改 attempt 行：\n before %+v\n after  %+v", curReaped, curReapedAgain)
	}
	stillDone := mustPhase(t, db, ns, task.ID, store.TaskCancelled)
	if !reflect.DeepEqual(done, stillDone) {
		t.Fatalf("重复当前取消 ACK 不得改任务终态事实：before %+v after %+v", done, stillDone)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("重复当前取消 ACK 不得再动槽位")
	}
	if ledgerAgain := ledger(); !reflect.DeepEqual(terminalLedger, ledgerAgain) {
		t.Fatalf("重复当前取消 ACK 不得改完整槽账本：before=%+v after=%+v", terminalLedger, ledgerAgain)
	}
}

// TestAckCancelHistoricalReapIsByteIdempotent 验历史 ACK 的**精确回收 + 逐字节幂等**：
// 第一次 ACK 写 exact ReapedAt 并真的解除预算；重复 ACK（不同 now/reason）不重写
// attempt 行（含 ReapedAt/reclaim_reason/phase/error/finished_at），不碰任务/新槽。
func TestAckCancelHistoricalReapIsByteIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ack-hist-idem")
	c := lifeCycleCap(t, db, ns, "idem", true, 3) // maxPerNode=2
	node := newNode(t, db, "node-a", 3)

	ledger := func() [][5]any {
		rows := make([][5]any, 3)
		for slot := range rows {
			taskID, attempt, leaseID, assignmentID, expiresAt := instanceLedger(t, db, node.ID, slot)
			rows[slot] = [5]any{taskID, attempt, leaseID, assignmentID, expiresAt}
		}
		return rows
	}

	task := newTask(t, db, ns, c, "trace-1", "")
	_, _, attempt1 := grant(t, db, task.ID, node.ID, "asg-1")
	// attempt1 变成历史：租约过期 → 重排 → 第二次授权在另一个合法槽上跑。
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET next_attempt_at = NULL WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if _, _, regrantAttempt := regrantOnFreeSlot(t, db, task.ID, node.ID, "asg-2"); regrantAttempt != 2 {
		t.Fatalf("第二次 attempt = %d", regrantAttempt)
	}

	// 预算：2 条未回收执行已经占满 pinned maxPerNode=2；第三个任务的槽是合法的。
	third := newTask(t, db, ns, c, "trace-3", "")
	if err := db.AssignTask(ctx, third.ID, node.ID, "asg-3", 1); err != nil {
		t.Fatalf("assign third: %v", err)
	}
	auth := nodeAuthByID(t, db, node.ID)
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, third.ID, "asg-3", 1,
		freeSlot(t, db, node.ID), store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("2 条未回收执行应占满 maxPerNode=2，得到 %v", err)
	}

	// 历史 ACK：第一次必须写 exact ReapedAt 并释放旧槽。
	oldBefore, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get historical attempt: %v", err)
	}
	taskBefore := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	newBefore, err := db.GetTaskAttempt(ctx, task.ID, 2)
	if err != nil {
		t.Fatalf("get new attempt: %v", err)
	}
	ledgerBefore := ledger()
	oldBinding := ackReapInput(t, db, node.ID, task.ID, attempt1)
	oldBinding.Now = store.Now()
	if err := db.AckCancelAuthorized(ctx, auth, oldBinding, true); err != nil {
		t.Fatalf("正确的历史 ack 应成功：%v", err)
	}
	att1, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil || att1.ReapedAt == nil || *att1.ReapedAt != oldBinding.Now {
		t.Fatalf("第一次历史 ACK 必须写精确 ReapedAt=%d：%+v err=%v", oldBinding.Now, att1, err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("历史 ack 只该释放旧槽，busy=%d", busySlots(t, db, node.ID))
	}

	expectedOld := *oldBefore
	expectedOld.ReapedAt = &oldBinding.Now
	expectedOld.ReclaimReason = store.ReclaimReasonCancelAck
	if !reflect.DeepEqual(&expectedOld, att1) {
		t.Fatalf("首历史 ACK 不得改旧失败/结果事实：before=%+v after=%+v", oldBefore, att1)
	}
	taskAfter := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	newAfter, err := db.GetTaskAttempt(ctx, task.ID, 2)
	if err != nil || !reflect.DeepEqual(newBefore, newAfter) || !reflect.DeepEqual(taskBefore, taskAfter) {
		t.Fatalf("首历史 ACK 不得改整任务/新 attempt：task=%+v attempt=%+v err=%v", taskAfter, newAfter, err)
	}
	ledgerAfter := ledger()
	ledgerBefore[oldBinding.Slot] = [5]any{"", 0, "", "", int64(0)}
	if !reflect.DeepEqual(ledgerBefore, ledgerAfter) {
		t.Fatalf("首历史 ACK 只能清旧槽，所有新槽须保持：before=%+v after=%+v", ledgerBefore, ledgerAfter)
	}

	// 重复 ACK（不同 now/reason）：attempt 整行逐字节不变，任务/新 attempt/槽都不动。
	replay := oldBinding
	replay.Now = oldBinding.Now + 1000
	replay.Reason = "duplicate_ack"
	if err := db.AckCancelAuthorized(ctx, auth, replay, true); err != nil {
		t.Fatalf("重复历史 ack 应幂等：%v", err)
	}
	att1b, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil || !reflect.DeepEqual(att1, att1b) {
		t.Fatalf("重复历史 ACK 不得改 attempt 行：\n before %+v\n after  %+v", att1, att1b)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("重复 ack 不得再动槽位，busy=%d", busySlots(t, db, node.ID))
	}
	running := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	if running.Attempt != 2 {
		t.Fatalf("历史 ack 不得推进/改当前的 attempt：%+v", running)
	}
	att2, err := db.GetTaskAttempt(ctx, task.ID, 2)
	if err != nil || att2.ReapedAt != nil || att2.LeaseID != running.LeaseID {
		t.Fatalf("新 attempt 不该被回收：%+v err=%v", att2, err)
	}

	if !reflect.DeepEqual(taskAfter, running) || !reflect.DeepEqual(newAfter, att2) {
		t.Fatalf("重复历史 ACK 不得改整任务/整新 attempt：task before=%+v after=%+v attempt before=%+v after=%+v", taskAfter, running, newAfter, att2)
	}
	if ledgerAgain := ledger(); !reflect.DeepEqual(ledgerAfter, ledgerAgain) {
		t.Fatalf("重复历史 ACK 不得改任一槽完整账本：before=%+v after=%+v", ledgerAfter, ledgerAgain)
	}

	// 预算真的解除了：第三个任务现在可以授权（1 未回收 + 本次 = 2 <= maxPerNode 2）。
	if err := db.AssignTask(ctx, third.ID, node.ID, "asg-3b", 1); err != nil {
		t.Fatalf("re-assign third: %v", err)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, auth, third.ID, "asg-3b", 1,
		freeSlot(t, db, node.ID), store.Now()); err != nil || got != 1 {
		t.Fatalf("历史 ACK 回收后预算应解除：attempt=%d err=%v", got, err)
	}
}

// TestAckCancelPreservesHistoricalFailureFacts 验历史 ACK 不能把已经写下的失败/终态
// 事实（phase/error/finished_at）用空 error 重写：只补 exact ReapedAt + 释放旧槽。
// 场景是真实产品路径：取消在飞时租约过期，RecoverLostLeases 先把任务推进取消终态、
// 把原因写进 attempt.error，但进程组回收事实（reaped_at）仍等节点 ACK。
func TestAckCancelPreservesHistoricalFailureFacts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ack-hist-failure")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)

	ledger := func() [][5]any {
		rows := make([][5]any, 2)
		for slot := range rows {
			taskID, attempt, leaseID, assignmentID, expiresAt := instanceLedger(t, db, node.ID, slot)
			rows[slot] = [5]any{taskID, attempt, leaseID, assignmentID, expiresAt}
		}
		return rows
	}

	task := newTask(t, db, ns, c, "trace-1", "")
	_, _, attempt1 := grant(t, db, task.ID, node.ID, "asg-1")
	if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	mustPhase(t, db, ns, task.ID, store.TaskCancelled)

	before, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if before.ReapedAt != nil {
		t.Fatalf("回收事实还没到，reaped_at 必须仍为空：%+v", before)
	}
	if before.Phase != store.TaskCancelled || before.Error != protocol.ReasonCancelledByUser {
		t.Fatalf("回收前的失败/终态事实不符：%+v", before)
	}
	taskBefore, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("未确认回收前槽必须保持占用，busy=%d", busySlots(t, db, node.ID))
	}

	auth := nodeAuthByID(t, db, node.ID)
	ledgerBefore := ledger()
	binding := ackReapInput(t, db, node.ID, task.ID, attempt1)
	binding.Now = store.Now()
	if err := db.AckCancelAuthorized(ctx, auth, binding, true); err != nil {
		t.Fatalf("历史 ack 应成功：%v", err)
	}
	after, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("re-get attempt: %v", err)
	}
	if after.ReapedAt == nil || *after.ReapedAt != binding.Now {
		t.Fatalf("第一次历史 ACK 必须写精确 ReapedAt=%d：%+v", binding.Now, after)
	}
	expected := *before
	expected.ReapedAt = &binding.Now
	expected.ReclaimReason = store.ReclaimReasonCancelAck
	if !reflect.DeepEqual(&expected, after) {
		t.Fatalf("首历史 ACK 只能补回收事实：before=%+v after=%+v", before, after)
	}
	if after.Phase != before.Phase || after.Error != before.Error ||
		!reflect.DeepEqual(after.FinishedAt, before.FinishedAt) {
		t.Fatalf("历史 ACK 不得重写旧失败事实：\n before %+v\n after  %+v", before, after)
	}
	taskAfter, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("re-get task: %v", err)
	}
	if !reflect.DeepEqual(taskBefore, taskAfter) {
		t.Fatalf("历史 ACK 不得改任务结果事实：\n before %+v\n after  %+v", taskBefore, taskAfter)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("历史 ACK 应释放旧槽")
	}

	ledgerAfter := ledger()
	ledgerBefore[binding.Slot] = [5]any{"", 0, "", "", int64(0)}
	if !reflect.DeepEqual(ledgerBefore, ledgerAfter) {
		t.Fatalf("首历史 ACK 只能清旧槽，其余账本须保持：before=%+v after=%+v", ledgerBefore, ledgerAfter)
	}

	// 重复 ACK（不同 now/reason）：整任务、attempt、完整账本逐字节不变。
	replay := binding
	replay.Now = binding.Now + 777
	replay.Reason = "duplicate_ack"
	if err := db.AckCancelAuthorized(ctx, auth, replay, true); err != nil {
		t.Fatalf("重复历史 ack 应幂等：%v", err)
	}
	again, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil || !reflect.DeepEqual(after, again) {
		t.Fatalf("重复 ACK 不得改 attempt 行：\n before %+v\n after  %+v", after, again)
	}
	taskAgain, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || !reflect.DeepEqual(taskAfter, taskAgain) {
		t.Fatalf("重复历史失败 ACK 不得改整任务：before=%+v after=%+v err=%v", taskAfter, taskAgain, err)
	}
	if ledgerAgain := ledger(); !reflect.DeepEqual(ledgerAfter, ledgerAgain) {
		t.Fatalf("重复历史失败 ACK 不得改完整槽账本：before=%+v after=%+v", ledgerAfter, ledgerAgain)
	}
}

// TestFirstGrantAfterNamespaceChangeKeepsPinnedRevision 验 D2 反例：
// task 创建后改 ns 配置，首次 grant 仍用创建时 pin 的 ns 值与 revision。
func TestFirstGrantAfterNamespaceChangeKeepsPinnedRevision(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pin-first-grant")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)

	pinned := setNamespaceVars(t, db, ns, map[string]string{"SOURCE_ROOT": "ns-A"})
	task := newTask(t, db, ns, c, "trace-pin", "")
	updated := setNamespaceVars(t, db, ns, map[string]string{"SOURCE_ROOT": "ns-B"})
	if updated.Revision == pinned.Revision {
		t.Fatalf("改 ns 配置必须推进 revision")
	}
	_, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")
	att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if att.NamespaceVarsRevision != pinned.Revision {
		t.Fatalf("attempt 快照的 namespace revision = %d，want 创建时 pin 的 %d",
			att.NamespaceVarsRevision, pinned.Revision)
	}
	inputs, err := db.TaskInputs(ctx, task.ID, []string{"vars.SOURCE_ROOT"})
	if err != nil {
		t.Fatalf("task inputs: %v", err)
	}
	if got := inputs["SOURCE_ROOT"]; got.Value != "ns-A" || got.Source != store.VarSourceNamespace {
		t.Fatalf("首次 grant 必须用创建时 pin 的 ns 值：%+v", got)
	}
	reread := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	if reread.NamespaceVarsRevision != pinned.Revision {
		t.Fatalf("任务行 revision 漂了：%d != %d", reread.NamespaceVarsRevision, pinned.Revision)
	}
}

// TestMachineLimitsSnapshotPinnedPerAttempt 验机器 limits 快照按 attempt 钉住：
// 同一 attempt 中途改配置不变；重排后的新 attempt 用新 limits。
func TestMachineLimitsSnapshotPinnedPerAttempt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "limits-snapshot")
	c := lifeCycleCap(t, db, ns, "idem", true, 3) // maxPerNode=2
	node := newNode(t, db, "node-a", 2)

	task := newTask(t, db, ns, c, "trace-limits", "")
	_, _, attempt1 := grant(t, db, task.ID, node.ID, "asg-1")
	att1, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get attempt1: %v", err)
	}
	typed1, terr := store.TypedMachineLimits(att1.MachineLimits)
	if terr != nil || typed1.Total != 2 {
		t.Fatalf("attempt1 limits = %+v err=%v（want total=2）", typed1, terr)
	}

	// attempt 进行中：把机器预算改成 3 并 apply。
	putNodeCfg(t, db, node.ID, []string{"*"}, nil, map[string]any{"total": 3})
	recordAppliedForNode(t, db, node.ID)
	att1b, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("reread attempt1: %v", err)
	}
	typed1b, terr := store.TypedMachineLimits(att1b.MachineLimits)
	if terr != nil || typed1b.Total != 2 {
		t.Fatalf("同一 attempt 的 limits 不得随配置更新漂移：%+v err=%v", typed1b, terr)
	}

	// 重排 + 回收旧槽 → 新 attempt 用新 limits。
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET next_attempt_at = NULL WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, idleObservations(t, db, node.ID, att1.Slot), store.Now()); rerr != nil || n != 1 {
		t.Fatalf("回收旧 attempt 的槽：n=%d err=%v", n, rerr)
	}
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-2", 2); err != nil {
		t.Fatalf("re-assign: %v", err)
	}
	_, _, attempt2, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID),
		task.ID, "asg-2", 2, freeSlot(t, db, node.ID), store.Now())
	if err != nil || attempt2 != 2 {
		t.Fatalf("新 attempt 应可授权：attempt=%d err=%v", attempt2, err)
	}
	att2, err := db.GetTaskAttempt(ctx, task.ID, attempt2)
	if err != nil {
		t.Fatalf("get attempt2: %v", err)
	}
	typed2, terr := store.TypedMachineLimits(att2.MachineLimits)
	if terr != nil || typed2.Total != 3 {
		t.Fatalf("新 attempt 必须用新 limits：%+v err=%v", typed2, terr)
	}
}
