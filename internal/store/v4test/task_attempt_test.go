package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 G1-6S/R2（A3 物理槽位与历史回收接缝）的验收集：
//
//   - grant 只消费节点 accept 回报的真实 slot（不随机另挑）、同事务落 attempt 快照；
//   - assignment 是带 30s 窗口的 reservation：过期不授权、重复有效 assignment 幂等；
//   - slot 容量拒绝（slot_unavailable）不消耗 attempt；
//   - 同一 attempt 的机器输入固定，新 attempt 重新解析；namespace 仍是首次 pin；
//   - 槽位回收只看**明确实例事实 + 相符历史引用**，rehello / phase / 缺字段都不算；
//   - 历史 attempt 的回收要 node 身份 + task/attempt/lease/slot 匹配，且幂等；
//   - 未 reaped 的 attempt 是 GC 硬保留条件；attempt JSON 不回显机器值与凭证。

// ---------- 夹具 ----------

// regrantOnFreeSlot 清退避后重新派发并授权（挑节点当前真实空闲 slot）。
func regrantOnFreeSlot(t *testing.T, db *store.DB, taskID, nodeID, assignmentID string) (leaseID, token string, attempt int) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Exec(`UPDATE tasks SET next_attempt_at = NULL WHERE id = ?`, taskID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if err := db.AssignTask(ctx, taskID, nodeID, assignmentID, 1); err != nil {
		t.Fatalf("assign %s: %v", assignmentID, err)
	}
	slot := freeSlot(t, db, nodeID)
	leaseID, token, attempt, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, nodeID), taskID, assignmentID, 1, slot, store.Now())
	if err != nil {
		t.Fatalf("re-grant %s: %v", assignmentID, err)
	}
	return leaseID, token, attempt
}

// requeueByExpiry 用「租约过期」把 running 任务收回 pending（attempt 计数保留）。
func requeueByExpiry(t *testing.T, db *store.DB, taskID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, taskID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	requeued, _, err := db.RecoverLostLeases(ctx, store.Now())
	if err != nil {
		t.Fatalf("recover lost leases: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("应重排 1 条，实际 %d", requeued)
	}
}

// TestAttemptSnapshotPinnedPerAttempt 验：
//   - 同一 attempt 中途改 config / hello 上报本机值 → 该 attempt 的机器快照不变；
//   - 新 attempt 重新解析机器值（换 attempt 允许变）；
//   - namespace 值仍是任务接受时钉住的那一份，跨 attempt 不变。
func TestAttemptSnapshotPinnedPerAttempt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "attempt-pin")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)

	// rev2：节点层机器值 SRC=rev2，预算 total=2。
	putNodeCfg(t, db, node.ID, []string{"*"}, map[string]string{"SRC": "rev2"}, map[string]any{"total": 2})
	recordAppliedForNode(t, db, node.ID)
	cfg2, err := db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}

	setNamespaceVars(t, db, ns, map[string]string{"NS_VAR": "ns-A"})
	task := newTask(t, db, ns, c, "trace-pin", "")
	lease1, _, attempt1 := grant(t, db, task.ID, node.ID, "asg-1")
	if attempt1 != 1 {
		t.Fatalf("attempt = %d", attempt1)
	}

	att1, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get attempt 1: %v", err)
	}
	if att1.ConfigRevision != cfg2.Revision {
		t.Fatalf("attempt 快照的 configRevision = %d，want %d", att1.ConfigRevision, cfg2.Revision)
	}
	if att1.MachineVars["SRC"] != "rev2" || att1.MachineVarSources["SRC"] != store.VarSourceNode {
		t.Fatalf("attempt 1 机器值/来源 = %v/%v", att1.MachineVars, att1.MachineVarSources)
	}
	typed1, terr := store.TypedMachineLimits(att1.MachineLimits)
	if terr != nil || typed1.Total != 2 {
		t.Fatalf("attempt 1 limits = %+v err=%v", typed1, terr)
	}
	// hash 必须与共享实现完全一致（store 不复制 canonical 规则）。
	wantHash1, herr := protocol.MachineConfigHash(
		map[string]string{"SRC": "rev2"}, protocol.Limits{Total: 2})
	if herr != nil {
		t.Fatalf("protocol hash: %v", herr)
	}
	if att1.ConfigHash != wantHash1 {
		t.Fatalf("attempt 1 configHash = %q，want %q（共享 MachineConfigHash）", att1.ConfigHash, wantHash1)
	}
	inputs1, err := db.TaskInputs(ctx, task.ID, []string{"vars.SRC", "vars.NS_VAR"})
	if err != nil {
		t.Fatalf("task inputs attempt 1: %v", err)
	}
	if inputs1["SRC"].Value != "rev2" || inputs1["NS_VAR"].Value != "ns-A" {
		t.Fatalf("attempt 1 inputs = %+v", inputs1)
	}

	// 同一 attempt 期间：配置推到 rev3 + 本机上报 local SRC + ns 改 B。
	putNodeCfg(t, db, node.ID, []string{"*"}, map[string]string{"SRC": "rev3"}, map[string]any{"total": 2})
	helloNode(t, db, node, map[string]string{"SRC": "local3"}, nil)
	setNamespaceVars(t, db, ns, map[string]string{"NS_VAR": "ns-B"})

	inputs1b, err := db.TaskInputs(ctx, task.ID, []string{"vars.SRC", "vars.NS_VAR"})
	if err != nil {
		t.Fatalf("task inputs after config change: %v", err)
	}
	if inputs1b["SRC"].Value != "rev2" || inputs1b["SRC"].Source != store.VarSourceNode {
		t.Fatalf("同一 attempt 的机器值不得随配置更新漂移：%+v", inputs1b)
	}
	if inputs1b["NS_VAR"].Value != "ns-A" {
		t.Fatalf("namespace 必须是任务接受时 pin 的值：%+v", inputs1b)
	}
	att1b, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil || att1b.ConfigHash != att1.ConfigHash {
		t.Fatalf("同一 attempt 的快照 hash 不得漂移：%v err=%v", att1b.ConfigHash, err)
	}

	// 重排 + 回收旧槽 → 新 attempt 重新解析机器值（local 覆盖 node）。
	requeueByExpiry(t, db, task.ID)
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, idleObservations(t, db, node.ID, att1.Slot), store.Now()); rerr != nil || n != 1 {
		t.Fatalf("回收旧 attempt 的槽：n=%d err=%v", n, rerr)
	}
	_, _, attempt2 := regrantOnFreeSlot(t, db, task.ID, node.ID, "asg-2")
	if attempt2 != 2 {
		t.Fatalf("第二次 attempt = %d", attempt2)
	}
	att2, err := db.GetTaskAttempt(ctx, task.ID, attempt2)
	if err != nil {
		t.Fatalf("get attempt 2: %v", err)
	}
	if att2.MachineVars["SRC"] != "local3" || att2.MachineVarSources["SRC"] != store.VarSourceLocal {
		t.Fatalf("新 attempt 必须重新解析机器值（local 覆盖 node）：%v/%v",
			att2.MachineVars, att2.MachineVarSources)
	}
	if att2.NodeID != node.ID || att2.InstanceID == "" || att2.Slot < 0 {
		t.Fatalf("新 attempt 必须绑到自己的物理槽：%+v", att2)
	}
	// 旧槽回收后可以被新 attempt 复用：绑定的 instance 必须与它记录的 slot 一致。
	insts, ierr := db.ListInstances(ctx, node.ID)
	if ierr != nil {
		t.Fatalf("list instances: %v", ierr)
	}
	var slotInstance string
	for _, in := range insts {
		if in.Slot == att2.Slot {
			slotInstance = in.ID
		}
	}
	if slotInstance != att2.InstanceID {
		t.Fatalf("attempt 2 的 instance %s 与 slot %d 的实例 %s 不一致",
			att2.InstanceID, att2.Slot, slotInstance)
	}
	inputs2, err := db.TaskInputs(ctx, task.ID, []string{"vars.SRC", "vars.NS_VAR"})
	if err != nil {
		t.Fatalf("task inputs attempt 2: %v", err)
	}
	if inputs2["SRC"].Value != "local3" || inputs2["SRC"].Source != store.VarSourceLocal {
		t.Fatalf("attempt 2 机器值 = %+v", inputs2["SRC"])
	}
	if inputs2["NS_VAR"].Value != "ns-A" {
		t.Fatalf("attempt 2 的 ns 值必须仍是首次 pin 的 ns-A：%+v", inputs2["NS_VAR"])
	}

	// 历史定位能力：按 attempt 查、列出全部、取上一次。
	list, err := db.ListTaskAttempts(ctx, task.ID)
	if err != nil || len(list) != 2 || list[0].Attempt != 1 || list[1].Attempt != 2 {
		t.Fatalf("list attempts = %v err=%v", list, err)
	}
	prev, err := db.PreviousTaskAttempt(ctx, task.ID)
	if err != nil || prev.Attempt != 1 || prev.InstanceID != att1.InstanceID || prev.Slot != att1.Slot {
		t.Fatalf("previous attempt = %+v err=%v", prev, err)
	}
	if prev.ReapedAt == nil {
		t.Fatalf("旧 attempt 应已记 reaped 事实：%+v", prev)
	}
	_ = lease1
}

// TestGrantConsumesAcceptedSlot 验 grant 只消费 accept 回报的真实 slot：
// 不随机另挑、占用/未申报/缺 presence 一律 slot_unavailable，且不耗 attempt。
func TestGrantConsumesAcceptedSlot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "accept-slot")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)

	task1 := newTask(t, db, ns, c, "trace-1", "")
	if err := db.AssignTask(ctx, task1.ID, node.ID, "asg-1", 1); err != nil {
		t.Fatalf("assign task1: %v", err)
	}
	now := store.Now()
	lease1, _, attempt1, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task1.ID, "asg-1", 1, 1, now)
	if err != nil {
		t.Fatalf("grant slot 1: %v", err)
	}
	if attempt1 != 1 {
		t.Fatalf("attempt = %d", attempt1)
	}
	insts, err := db.ListInstances(ctx, node.ID)
	if err != nil || len(insts) != 2 {
		t.Fatalf("list instances: %v %d", err, len(insts))
	}
	if insts[0].BusyTask != nil {
		t.Fatalf("slot 0 不该被占（grant 必须消费 accept 的 slot 1）：%+v", insts[0])
	}
	if insts[1].BusyTask == nil || *insts[1].BusyTask != task1.ID ||
		insts[1].BusyAttempt != 1 || insts[1].BusyLeaseID != lease1 {
		t.Fatalf("slot 1 的绑定必须带具体执行身份：%+v", insts[1])
	}
	row1, err := db.GetTask(ctx, ns, task1.ID)
	if err != nil || row1.LeaseInstanceID != insts[1].ID {
		t.Fatalf("任务行 lease_instance_id = %q，want %q", row1.LeaseInstanceID, insts[1].ID)
	}
	att1, err := db.GetTaskAttempt(ctx, task1.ID, 1)
	if err != nil || att1.Slot != 1 || att1.InstanceID != insts[1].ID {
		t.Fatalf("attempt 1 的物理槽 = %+v err=%v", att1, err)
	}

	// 同一个 slot 再次授权：被占 → slot_unavailable（不另挑空槽）。
	task2 := newTask(t, db, ns, c, "trace-2", "")
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-2", 1); err != nil {
		t.Fatalf("assign task2: %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task2.ID, "asg-2", 1, 1, now); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("占用 slot 应 ErrSlotUnavailable，得到 %v", err)
	}
	pending2, err := db.GetTask(ctx, ns, task2.ID)
	if err != nil || pending2.Attempt != 0 {
		t.Fatalf("容量拒绝不得消耗 attempt：%+v err=%v", pending2, err)
	}
	if pending2.WaitingReason != store.WaitingForSlot || pending2.NextAttemptAt == nil {
		t.Fatalf("容量拒绝应回 waiting_for_slot + 有界退避：%+v", pending2)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("被拒的 grant 不得占槽，busy=%d", busySlots(t, db, node.ID))
	}
	// 缺 presence（负数）与未申报 slot 同样是 slot_unavailable。
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-3", 1); err != nil {
		t.Fatalf("re-assign task2: %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task2.ID, "asg-3", 1, -1, now); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("缺 slot presence 应 ErrSlotUnavailable，得到 %v", err)
	}
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-4", 1); err != nil {
		t.Fatalf("re-assign task2 again: %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task2.ID, "asg-4", 1, 5, now); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("未申报 slot 应 ErrSlotUnavailable，得到 %v", err)
	}
	// 真正空闲的 slot 0 才能授权。
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-5", 1); err != nil {
		t.Fatalf("re-assign task2: %v", err)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task2.ID, "asg-5", 1, 0, now); err != nil || got != 1 {
		t.Fatalf("slot 0 应可授权：attempt=%d err=%v", got, err)
	}
}

// TestAssignmentWindowBoundedAndIdempotent 验 assignment 统一 30s 窗口：
// 同一有效 assignment 重复 assign 幂等；过期不再 grant（不耗 attempt、不占槽）。
func TestAssignmentWindowBoundedAndIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "assignment-window")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")

	now := store.Now()
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-1", 1, now); err != nil {
		t.Fatalf("assign: %v", err)
	}
	first, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || first.AssignmentExpiresAt == nil {
		t.Fatalf("assignment 必须带窗口：%+v err=%v", first, err)
	}
	if got := *first.AssignmentExpiresAt; got != now+store.AssignmentWindowSec {
		t.Fatalf("assignmentExpiresAt = %d，want %d", got, now+store.AssignmentWindowSec)
	}
	// 未 grant：没有 attempt / lease / 槽位。
	if first.Attempt != 0 || first.LeaseID != "" {
		t.Fatalf("reservation 不造 lease/attempt：%+v", first)
	}
	attempts, err := db.ListTaskAttempts(ctx, task.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("未 grant 不该有 attempt 行：%v err=%v", attempts, err)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("未 grant 不该占槽")
	}

	// 同一 assignment/节点在窗口内重复 assign：幂等（不换窗口、不改 proposedAttempt）。
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-1", 2, now+10); err != nil {
		t.Fatalf("duplicate assign: %v", err)
	}
	again, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || again.ProposedAttempt != 1 || *again.AssignmentExpiresAt != *first.AssignmentExpiresAt {
		t.Fatalf("重复有效 assignment 必须幂等：%+v err=%v", again, err)
	}

	// 窗口过期：不得授权，清掉候选、不耗 attempt、不占槽。
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "asg-1", 1, 0, now+store.AssignmentWindowSec); !errors.Is(err, store.ErrNoAssignment) {
		t.Fatalf("过期 assignment 应 ErrNoAssignment，得到 %v", err)
	}
	stale, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || stale.AssignmentID != "" || stale.NodeID != nil || stale.Attempt != 0 {
		t.Fatalf("过期 assignment 应被清掉且不耗 attempt：%+v err=%v", stale, err)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("过期 grant 不得占槽")
	}

	// 新窗口可以正常授权。
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-2", 1, now+store.AssignmentWindowSec+1); err != nil {
		t.Fatalf("re-assign: %v", err)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task.ID, "asg-2", 1, 0, now+store.AssignmentWindowSec+2); err != nil || got != 1 {
		t.Fatalf("新窗口应可授权：attempt=%d err=%v", got, err)
	}
}

// TestRejectSlotUnavailableNoAttempt 验 task.reject 的第二个合法理由：
// slot_unavailable 回 pending + waiting_for_slot、不耗 attempt；未知理由 fail-closed。
func TestRejectSlotUnavailableNoAttempt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "reject-slot")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")

	now := store.Now()
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-1", 1, now); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := db.RejectAssignment(ctx, task.ID, "asg-1", store.TaskRejectSlotUnavailable, now); err != nil {
		t.Fatalf("reject slot_unavailable: %v", err)
	}
	got, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || got.Phase != store.TaskPending {
		t.Fatalf("reject 后应仍是 pending：%+v err=%v", got, err)
	}
	if got.WaitingReason != store.WaitingForSlot || got.NextAttemptAt == nil || *got.NextAttemptAt <= now {
		t.Fatalf("slot_unavailable 应回 waiting_for_slot + 有界退避：%+v", got)
	}
	if got.Attempt != 0 || got.LeaseID != "" || got.AssignmentID != "" {
		t.Fatalf("reject 不得消耗 attempt / 造 lease / 留候选：%+v", got)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("reject 不得占槽")
	}
	// 未知理由 fail-closed。
	if err := db.RejectAssignment(ctx, task.ID, "", "missing_requirement", now); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("未知 reject 理由应 ErrInvalidTransition，得到 %v", err)
	}
	// stale_config 仍是合法理由并回 waiting_for_config。
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-2", 1, now); err != nil {
		t.Fatalf("re-assign: %v", err)
	}
	if err := db.RejectAssignment(ctx, task.ID, "asg-2", "stale_config", now); err != nil {
		t.Fatalf("reject stale_config: %v", err)
	}
	cfgWait, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || cfgWait.WaitingReason != store.WaitingForConfig {
		t.Fatalf("stale_config 应回 waiting_for_config：%+v err=%v", cfgWait, err)
	}
	// 两次 reject 都不消耗 attempt：下一次 grant 仍是 attempt 1。
	if _, _, attempt := regrantOnFreeSlot(t, db, task.ID, node.ID, "asg-3"); attempt != 1 {
		t.Fatalf("reject 不该消耗 attempt，得到 %d", attempt)
	}
}

// TestHistoricalReapRequiresExactBinding 验历史 attempt 的回收：
// node 身份 + task/attempt/lease/slot 全对上才清**旧槽**；幂等；不碰新 attempt。
func TestHistoricalReapRequiresExactBinding(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "historical-reap")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)
	task := newTask(t, db, ns, c, "trace-1", "")

	lease1, token1, attempt1 := grant(t, db, task.ID, node.ID, "asg-1")
	att1, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get attempt 1: %v", err)
	}
	child := createCap(t, db, testCap(ns, "child", 1, nil))
	emitID := "old-attempt-emit"
	auth1 := store.EmitAuth{TaskID: task.ID, NodeID: node.ID, NodeTokenHash: node.TokenHash,
		Attempt: attempt1, LeaseID: lease1, AttemptToken: token1, Now: store.Now()}
	rec := &store.EmitRecord{TaskID: task.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)}
	oldResult, first, err := db.EmitAuthorized(ctx, auth1, rec, childTasks(ns, task.TraceID, child, 1))
	if err != nil || !first {
		t.Fatalf("old authorized emit must actually write: first=%v err=%v", first, err)
	}
	requeueByExpiry(t, db, task.ID)
	lease2, token2, attempt2 := regrantOnFreeSlot(t, db, task.ID, node.ID, "asg-2")
	if attempt2 != 2 {
		t.Fatalf("attempt 2 = %d", attempt2)
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("两个 attempt 应各占一槽，busy=%d", busySlots(t, db, node.ID))
	}
	base := store.ReapedInputs{
		TaskID: task.ID, Attempt: attempt1,
		NodeID: node.ID, NodeTokenHash: node.TokenHash,
		LeaseID: lease1, Slot: att1.Slot, InstanceID: att1.InstanceID,
	}

	beforeRejected, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatal(err)
	}
	// 身份/绑定不符：全部 fail-closed，且不清任何槽。
	wrongToken := base
	wrongToken.NodeTokenHash = "sha256:not-this-node"
	if _, err := db.ReapTaskAttempt(ctx, wrongToken); !errors.Is(err, store.ErrNodeIdentityMismatch) {
		t.Fatalf("错节点令牌应 ErrNodeIdentityMismatch，得到 %v", err)
	}
	unknownNode := base
	unknownNode.NodeID = "nd_missing"
	if _, err := db.ReapTaskAttempt(ctx, unknownNode); !errors.Is(err, store.ErrNodeIdentityMismatch) {
		t.Fatalf("不存在的节点应 ErrNodeIdentityMismatch，得到 %v", err)
	}
	wrongLease := base
	wrongLease.LeaseID = "lse_bogus"
	if _, err := db.ReapTaskAttempt(ctx, wrongLease); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("错 lease 应 ErrLeaseMismatch，得到 %v", err)
	}
	wrongSlot := base
	wrongSlot.Slot = att1.Slot + 1
	if _, err := db.ReapTaskAttempt(ctx, wrongSlot); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("错 slot 应 ErrLeaseMismatch，得到 %v", err)
	}
	missingSlot := base
	missingSlot.Slot = -1
	if _, err := db.ReapTaskAttempt(ctx, missingSlot); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("缺 slot presence 应 ErrLeaseMismatch，得到 %v", err)
	}
	missingLease := base
	missingLease.LeaseID = ""
	if _, err := db.ReapTaskAttempt(ctx, missingLease); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("缺 lease 应 ErrLeaseMismatch，得到 %v", err)
	}
	for _, bad := range []store.ReapedInputs{
		{TaskID: task.ID, Attempt: attempt1, NodeID: node.ID, NodeTokenHash: node.TokenHash, LeaseID: lease1, Slot: att1.Slot, InstanceID: "inst_wrong"},
		{TaskID: "tsk_missing", Attempt: attempt1, NodeID: node.ID, NodeTokenHash: node.TokenHash, LeaseID: lease1, Slot: att1.Slot},
		{TaskID: task.ID, Attempt: attempt1 + 10, NodeID: node.ID, NodeTokenHash: node.TokenHash, LeaseID: lease1, Slot: att1.Slot},
		{TaskID: task.ID, Attempt: attempt1, NodeID: node.ID, NodeTokenHash: "", LeaseID: lease1, Slot: att1.Slot},
		{TaskID: task.ID, Attempt: attempt1, NodeID: "", NodeTokenHash: node.TokenHash, LeaseID: lease1, Slot: att1.Slot},
	} {
		if _, err := db.ReapTaskAttempt(ctx, bad); err == nil {
			t.Fatalf("bad historical identity/binding must reject: %+v", bad)
		}
	}
	unchanged, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil || !reflect.DeepEqual(beforeRejected, unchanged) {
		t.Fatalf("rejected historical reap mutated attempt: before=%+v after=%+v err=%v", beforeRejected, unchanged, err)
	}
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("被拒的历史回收不得清槽，busy=%d", busySlots(t, db, node.ID))
	}

	currentBefore := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	// 正确绑定：只清旧槽，不动新 attempt / 任务相位。
	reaped, err := db.ReapTaskAttempt(ctx, base)
	if err != nil || !reaped {
		t.Fatalf("历史回收应成功：reaped=%v err=%v", reaped, err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("只该清旧槽，busy=%d", busySlots(t, db, node.ID))
	}
	nowRunning := mustPhase(t, db, ns, task.ID, store.TaskRunning)
	if !reflect.DeepEqual(currentBefore, nowRunning) {
		t.Fatalf("历史回收不得改新 attempt 的任何事实：before=%+v after=%+v", currentBefore, nowRunning)
	}
	att2, err := db.GetTaskAttempt(ctx, task.ID, attempt2)
	if err != nil || att2.ReapedAt != nil || att2.InstanceID == att1.InstanceID {
		t.Fatalf("新 attempt 不该被回收：%+v err=%v", att2, err)
	}
	// 重复回收幂等。
	if again, err := db.ReapTaskAttempt(ctx, base); err != nil || again {
		t.Fatalf("重复回收应幂等：reaped=%v err=%v", again, err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("重复回收不得再动槽位，busy=%d", busySlots(t, db, node.ID))
	}
	// 旧 result 仍被拒（历史回收不放行任何业务提交）。
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt1, lease1, 0, store.TaskResult{Phase: store.TaskSucceeded}); err == nil {
		t.Fatalf("旧 attempt 的 result 必须被拒")
	}
	// 旧 token 也已失效。
	if _, err := db.VerifyAttemptToken(ctx, task.ID, token1); err == nil {
		t.Fatalf("旧 attempt 的 token 必须被拒")
	}
	// Real authoritative emit writes/replays, not merely a token precheck.
	if _, _, err := db.EmitAuthorized(ctx, auth1, rec, nil); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("old emit must reject even for prior business id after reap: %v", err)
	}
	verified, err := db.VerifyAttemptToken(ctx, task.ID, token2)
	if err != nil || verified.Attempt != attempt2 || verified.LeaseID != lease2 {
		t.Fatalf("current exact token must remain valid: task=%+v err=%v", verified, err)
	}
	auth2 := auth1
	auth2.Attempt, auth2.LeaseID, auth2.AttemptToken = attempt2, lease2, token2
	auth2.Now = store.Now()
	if replay, first, err := db.EmitAuthorized(ctx, auth2, rec, nil); err != nil || first || string(replay) != string(oldResult) {
		t.Fatalf("current authorized replay after old reap: result=%s first=%v err=%v", replay, first, err)
	}
	newID := "new-attempt-emit"
	if _, first, err := db.EmitAuthorized(ctx, auth2,
		&store.EmitRecord{TaskID: task.ID, EmitID: &newID, ToRef: "child", Data: json.RawMessage(`{}`)},
		childTasks(ns, task.TraceID, child, 1)); err != nil || !first {
		t.Fatalf("current authorized emit must still write: first=%v err=%v", first, err)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatal("emit must not release the new attempt's slot")
	}
	// 新 attempt 仍可正常完成（只释放它自己的槽）。
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt2, lease2, 0, store.TaskResult{Phase: store.TaskSucceeded, Summary: "ok"}); err != nil {
		t.Fatalf("新 attempt 的 result 应可写：%v", err)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("两个 attempt 都回收后不该有 busy 槽")
	}
}

// TestRehelloIsNotProcessFact 验 rehello/缺字段/phase 都不是回收事实：
// 只有「明确 busy=false + 相符历史引用」才能清旧绑定。
func TestRehelloIsNotProcessFact(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "rehello")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	lease1, _, attempt1 := grant(t, db, task.ID, node.ID, "asg-1")
	att1, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}

	// 会话死亡 + 租约巡检重排；节点随后重连并重新申报实例。
	if err := db.MarkNodeOffline(ctx, node.ID, store.Now()); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	requeueByExpiry(t, db, task.ID)
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("offline/rehello 不是进程回收事实：busy 必须保留，得到 %d", busySlots(t, db, node.ID))
	}
	if _, err := db.UpsertInstances(ctx, node.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("re-declare instances: %v", err)
	}
	if err := db.UpdateNodeStatus(ctx, node.ID, store.NodeStatusOnline); err != nil {
		t.Fatalf("mark online: %v", err)
	}
	// 节点明确说「还在跑」：不回收。
	busyTrue := true
	stillRunning := store.SlotObservation{
		Slot: att1.Slot, Busy: &busyTrue,
		TaskID: task.ID, Attempt: attempt1, LeaseID: lease1,
	}
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{stillRunning}, store.Now()); rerr != nil || n != 0 {
		t.Fatalf("busy=true 不能回收：n=%d err=%v", n, rerr)
	}
	// nil 数组 / 缺 busy 字段 / 未提到的 slot：都未知，不回收。
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, nil, store.Now()); rerr != nil || n != 0 {
		t.Fatalf("缺 instances 不能回收：n=%d err=%v", n, rerr)
	}
	unknownBusy := store.SlotObservation{Slot: att1.Slot, TaskID: task.ID, Attempt: attempt1, LeaseID: lease1}
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{unknownBusy}, store.Now()); rerr != nil || n != 0 {
		t.Fatalf("缺 busy 字段不能回收：n=%d err=%v", n, rerr)
	}
	// 明确 idle 但引用不符（错 attempt / 错 lease / 无引用）不能清旧绑定。
	idle := false
	for _, bad := range []store.SlotObservation{
		{Slot: att1.Slot, Busy: &idle, TaskID: task.ID, Attempt: attempt1 + 1, LeaseID: lease1},
		{Slot: att1.Slot, Busy: &idle, TaskID: task.ID, Attempt: attempt1, LeaseID: "lse_other"},
		{Slot: att1.Slot, Busy: &idle},
	} {
		if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{bad}, store.Now()); rerr != nil || n != 0 {
			t.Fatalf("无匹配引用的 idle 不能回收：n=%d err=%v（%+v）", n, rerr, bad)
		}
	}
	// slot 0 是有效 presence：明确 idle + 相符引用 → 回收该旧绑定。
	match := store.SlotObservation{
		Slot: 0, Busy: &idle,
		TaskID: task.ID, Attempt: attempt1, LeaseID: lease1,
	}
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{match}, store.Now()); rerr != nil || n != 1 {
		t.Fatalf("slot 0 + 相符引用应回收：n=%d err=%v", n, rerr)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("回收后不该有 busy 槽")
	}
	if again, rerr := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{match}, store.Now()); rerr != nil || again != 0 {
		t.Fatalf("重复事实应幂等：n=%d err=%v", again, rerr)
	}
}

// TestTerminalPhaseIsNotProcessFact 验「数据库相位 = 终态」不能当进程已回收：
// 节点说还在跑就保持 busy；缺字段/重连都不算；只有明确 idle + 匹配引用才清。
func TestTerminalPhaseIsNotProcessFact(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "terminal-alive")
	c := lifeCycleCap(t, db, ns, "plain", false, 3) // 非幂等：巡检直接终态
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	lease1, _, attempt1 := grant(t, db, task.ID, node.ID, "asg-1")
	att1, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}

	if err := db.MarkNodeOffline(ctx, node.ID, store.Now()); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, task.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, terminal, err := db.RecoverLostLeases(ctx, store.Now()); err != nil || terminal != 1 {
		t.Fatalf("非幂等过期应终态：terminal=%d err=%v", terminal, err)
	}
	mustPhase(t, db, ns, task.ID, store.TaskAbandoned)
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("终态不等于进程已回收：busy 必须保留")
	}
	// 节点明确说仍在跑（终态 + 活进程）：不回收。
	busyTrue := true
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, []store.SlotObservation{{
		Slot: att1.Slot, Busy: &busyTrue, TaskID: task.ID, Attempt: attempt1, LeaseID: lease1,
	}}, store.Now()); rerr != nil || n != 0 {
		t.Fatalf("活进程不能被终态推断回收：n=%d err=%v", n, rerr)
	}
	// 明确停止事实才回收。
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, idleObservations(t, db, node.ID, att1.Slot), store.Now()); rerr != nil || n != 1 {
		t.Fatalf("明确停止事实应回收：n=%d err=%v", n, rerr)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("回收后不该有 busy 槽")
	}
}

// TestBudgetShrinkKeepsLiveOccupancy 验预算缩小不假释放存量：旧占用保持 busy，
// 新 grant 只能在**新预算**内发生。
func TestBudgetShrinkKeepsLiveOccupancy(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "budget-shrink")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)

	task1 := newTask(t, db, ns, c, "trace-1", "")
	_, _, a1 := grant(t, db, task1.ID, node.ID, "asg-1")
	task2 := newTask(t, db, ns, c, "trace-2", "")
	_, _, a2 := grant(t, db, task2.ID, node.ID, "asg-2")
	if a1 != 1 || a2 != 1 || busySlots(t, db, node.ID) != 2 {
		t.Fatalf("前置：两个 attempt 各占一槽")
	}

	// 预算 2 → 1（节点已 apply）。
	putNodeCfg(t, db, node.ID, []string{"*"}, nil, map[string]any{"total": 1})
	recordAppliedForNode(t, db, node.ID)
	if busySlots(t, db, node.ID) != 2 {
		t.Fatalf("缩小预算不得假释放存量：busy=%d", busySlots(t, db, node.ID))
	}
	// 新任务在旧占用回收前不能授权（容量只有 1，且已被占）。
	task3 := newTask(t, db, ns, c, "trace-3", "")
	if err := db.AssignTask(ctx, task3.ID, node.ID, "asg-3", 1); err != nil {
		t.Fatalf("assign task3: %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task3.ID, "asg-3", 1, 0, store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("预算已满应 ErrSlotUnavailable，得到 %v", err)
	}
	if got, _ := db.GetTask(ctx, ns, task3.ID); got.Attempt != 0 {
		t.Fatalf("预算拒绝不得消耗 attempt")
	}
	// 回收其中一个旧占用后，容量才回到 1。
	att1, err := db.GetTaskAttempt(ctx, task1.ID, a1)
	if err != nil {
		t.Fatalf("get attempt 1: %v", err)
	}
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, idleObservations(t, db, node.ID, att1.Slot), store.Now()); rerr != nil || n != 1 {
		t.Fatalf("回收一个旧占用：n=%d err=%v", n, rerr)
	}
	if busySlots(t, db, node.ID) != 1 {
		t.Fatalf("回收后 busy=1，实际 %d", busySlots(t, db, node.ID))
	}
	if err := db.AssignTask(ctx, task3.ID, node.ID, "asg-3b", 1); err != nil {
		t.Fatalf("assign task3: %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task3.ID, "asg-3b", 1, 0, store.Now()); !errors.Is(err, store.ErrSlotUnavailable) {
		t.Fatalf("仍有一个占用吃满预算，应 ErrSlotUnavailable，得到 %v", err)
	}
	// 再回收另一个 → 现在预算内可以授权。
	att2, err := db.GetTaskAttempt(ctx, task2.ID, a2)
	if err != nil {
		t.Fatalf("get attempt 2: %v", err)
	}
	if n, rerr := db.ReclaimStaleSlots(ctx, node.ID, idleObservations(t, db, node.ID, att2.Slot), store.Now()); rerr != nil || n != 1 {
		t.Fatalf("回收第二个旧占用：n=%d err=%v", n, rerr)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatalf("两个都回收后 busy=0")
	}
	if err := db.AssignTask(ctx, task3.ID, node.ID, "asg-3c", 1); err != nil {
		t.Fatalf("assign task3: %v", err)
	}
	if _, _, got, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, node.ID), task3.ID, "asg-3c", 1, 0, store.Now()); err != nil || got != 1 {
		t.Fatalf("预算内应可授权：attempt=%d err=%v", got, err)
	}
}

// TestMachineConfigHashSharedAndNormalized 验机器配置 hash 用共享实现：
// 合并规则、limits 缺省归一（total 未设置 → 1）、来源层都不在 store 复制。
func TestMachineConfigHashSharedAndNormalized(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "hash-shared")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)

	putNodeCfg(t, db, node.ID, []string{"*"},
		map[string]string{"A": "node-a", "B": "node-b"},
		map[string]any{"total": 3, "perCapability": map[string]any{"build": 2}})
	// 本机层覆盖 B 与 perCapability.build。
	err := db.RecordHello(ctx, node.ID, store.NodeFacts{Hostname: node.Name}, nil, []string{"*"},
		map[string]string{"B": "local-b"},
		map[string]any{"perCapability": map[string]any{"build": 1}},
		"test-agent", "test-commit")
	if err != nil {
		t.Fatalf("record hello: %v", err)
	}
	recordAppliedForNode(t, db, node.ID)

	task := newTask(t, db, ns, c, "trace-1", "")
	_, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")
	att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	wantVars := map[string]string{"A": "node-a", "B": "local-b"}
	wantLimits := protocol.Limits{Total: 3, PerCapability: map[string]int{"build": 1}}
	wantHash, herr := protocol.MachineConfigHash(wantVars, wantLimits)
	if herr != nil {
		t.Fatalf("protocol hash: %v", herr)
	}
	if att.ConfigHash != wantHash {
		t.Fatalf("configHash = %q，want %q", att.ConfigHash, wantHash)
	}
	if att.MachineVarSources["A"] != store.VarSourceNode || att.MachineVarSources["B"] != store.VarSourceLocal {
		t.Fatalf("来源层 = %v", att.MachineVarSources)
	}
	typed, terr := store.TypedMachineLimits(att.MachineLimits)
	if terr != nil || typed.Total != 3 || typed.PerCapability["build"] != 1 {
		t.Fatalf("limits 快照 = %+v err=%v", typed, terr)
	}
	// TaskInputs 只按名字返回；未点名的键不出现。
	inputs, err := db.TaskInputs(ctx, task.ID, []string{"vars.B"})
	if err != nil || len(inputs) != 1 || inputs["B"].Value != "local-b" || inputs["B"].Source != store.VarSourceLocal {
		t.Fatalf("task inputs = %+v err=%v", inputs, err)
	}

	// limits 只给 perCapability、不写 total：共享 hash 必须按默认 total=1 归一。
	node2 := newNode(t, db, "node-b", 1)
	putNodeCfg(t, db, node2.ID, []string{"*"}, nil,
		map[string]any{"perCapability": map[string]any{"build": 2}})
	recordAppliedForNode(t, db, node2.ID)
	task2 := newTask(t, db, ns, c, "trace-2", "")
	_, _, attempt2 := grant(t, db, task2.ID, node2.ID, "asg-2")
	att2, err := db.GetTaskAttempt(ctx, task2.ID, attempt2)
	if err != nil {
		t.Fatalf("get attempt node2: %v", err)
	}
	wantHash2, herr := protocol.MachineConfigHash(nil, protocol.Limits{PerCapability: map[string]int{"build": 2}})
	if herr != nil {
		t.Fatalf("protocol hash 2: %v", herr)
	}
	if att2.ConfigHash != wantHash2 {
		t.Fatalf("缺省 total 的 hash = %q，want %q（默认 1 归一）", att2.ConfigHash, wantHash2)
	}
	if att2.MachineVars == nil || len(att2.MachineVars) != 0 {
		t.Fatalf("空机器 vars 归一成空 map：%v", att2.MachineVars)
	}
}

// TestTaskAttemptJSONShape 验 attempt 的公开 JSON 只暴露身份/版本/来源，不回显机器值与凭证。
func TestTaskAttemptJSONShape(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "attempt-json")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	putNodeCfg(t, db, node.ID, []string{"*"}, map[string]string{"SECRET_TOKEN": "s3cr3t-value"}, map[string]any{"total": 1})
	recordAppliedForNode(t, db, node.ID)
	task := newTask(t, db, ns, c, "trace-1", "")
	_, token, attempt := grant(t, db, task.ID, node.ID, "asg-1")

	att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	raw, err := json.Marshal(att)
	if err != nil {
		t.Fatalf("marshal attempt: %v", err)
	}
	joined := string(raw)
	for _, forbidden := range []string{"s3cr3t-value", "machineVars", "machineLimits", "attemptTokenHash", "resultJSON", "ResultJSON", "capabilityId", token} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("attempt JSON 不得包含 %q：%s", forbidden, joined)
		}
	}
	// Inspect real keys, not substrings: removing a json tag leaks uppercase Go
	// keys such as InstanceID/MachineVars, which lowercase searches cannot catch.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"taskId": true, "attempt": true, "nodeId": true, "nodeName": true,
		"artifacts": true, "exitCode": true,
		"slot": true, "leaseId": true, "configRevision": true, "configHash": true,
		"machineVarSources": true, "namespaceVarsRevision": true, "phase": true,
		"error": true, "startedAt": true, "finishedAt": true, "reapedAt": true, "reclaimReason": true,
	}
	if len(keys) != len(allowed) {
		t.Fatalf("attempt JSON keys differ from exact public map: %s", raw)
	}
	for key := range keys {
		if !allowed[key] {
			t.Fatalf("unexpected attempt JSON key %q (including capital-key leaks): %s", key, raw)
		}
	}
	for key := range allowed {
		if _, ok := keys[key]; !ok {
			t.Fatalf("missing attempt JSON key %q: %s", key, raw)
		}
	}
	if string(keys["exitCode"]) != "null" {
		t.Fatalf("not-yet-reported attempt must not invent exit zero: %s", raw)
	}
	// 内部执行输入仍可通过 TaskInputs 按名字拿到（证明值没丢，只是不回显）。
	inputs, err := db.TaskInputs(ctx, task.ID, []string{"vars.SECRET_TOKEN"})
	if err != nil || inputs["SECRET_TOKEN"].Value != "s3cr3t-value" {
		t.Fatalf("task inputs = %+v err=%v", inputs, err)
	}
}

// TestRetentionKeepsUnreapedAttempts 验 GC 硬保留：还有未 reaped attempt 的终态任务不删；
// 回收后任务与其 attempt 才一起级联消失。
func TestRetentionKeepsUnreapedAttempts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "retention-attempt")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)

	// A：正常结束 → 自己的 attempt 已 reaped，可被 GC。
	taskA := newTask(t, db, ns, c, "trace-a", "")
	leaseA, _, attemptA := grant(t, db, taskA.ID, node.ID, "asg-a")
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, taskA.ID, attemptA, leaseA, 0, store.TaskResult{Phase: store.TaskSucceeded, Summary: "ok"}); err != nil {
		t.Fatalf("complete A: %v", err)
	}
	// B：取消在飞 + 租约巡检收终态（没有 reaped 事实）→ 不可被 GC。
	taskB := newTask(t, db, ns, c, "trace-b", "")
	leaseB, _, attemptB := grant(t, db, taskB.ID, node.ID, "asg-b")
	attB, err := db.GetTaskAttempt(ctx, taskB.ID, attemptB)
	if err != nil {
		t.Fatalf("get attempt B: %v", err)
	}
	if err := db.RequestCancel(ctx, taskB.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
		t.Fatalf("cancel B: %v", err)
	}
	if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, store.Now()-1, taskB.ID); err != nil {
		t.Fatalf("expire B: %v", err)
	}
	if _, terminal, err := db.RecoverLostLeases(ctx, store.Now()); err != nil || terminal != 1 {
		t.Fatalf("recover B: terminal=%d err=%v", terminal, err)
	}
	mustPhase(t, db, ns, taskB.ID, store.TaskCancelled)

	cutoff := store.Now() + 3600
	n, err := db.DeleteExpiredTasks(ctx, cutoff, 10)
	if err != nil || n != 1 {
		t.Fatalf("第一轮 GC 只该删 A：n=%d err=%v", n, err)
	}
	if _, err := db.GetTask(ctx, ns, taskA.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("A 应被删：%v", err)
	}
	if _, err := db.GetTask(ctx, ns, taskB.ID); err != nil {
		t.Fatalf("未 reaped 的终态任务必须保留：%v", err)
	}
	if kept, err := db.GetTaskAttempt(ctx, taskB.ID, attemptB); err != nil || kept.ReapedAt != nil {
		t.Fatalf("B 的 attempt 必须保留且未 reaped：%+v err=%v", kept, err)
	}
	// 回收旧 attempt 后才能 GC（级联删 attempt）。
	reaped, err := db.ReapTaskAttempt(ctx, store.ReapedInputs{
		TaskID: taskB.ID, Attempt: attemptB,
		NodeID: node.ID, NodeTokenHash: node.TokenHash,
		LeaseID: leaseB, Slot: attB.Slot, InstanceID: attB.InstanceID,
	})
	if err != nil || !reaped {
		t.Fatalf("reap B: reaped=%v err=%v", reaped, err)
	}
	n, err = db.DeleteExpiredTasks(ctx, cutoff, 10)
	if err != nil || n != 1 {
		t.Fatalf("回收后 GC 应删 B：n=%d err=%v", n, err)
	}
	if _, err := db.GetTaskAttempt(ctx, taskB.ID, attemptB); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("任务删除后 attempt 应级联消失：%v", err)
	}
}

// TestActiveTaskNeverGC 验 running（active）任务与其未 reaped attempt 永不被 GC 带走。
func TestActiveTaskNeverGC(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "retention-active")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 1)
	task := newTask(t, db, ns, c, "trace-1", "")
	lease, _, attempt := grant(t, db, task.ID, node.ID, "asg-1")

	cutoff := store.Now() + 3600
	if _, err := db.Exec(`UPDATE tasks SET created_at = ? WHERE id = ?`, store.Now()-7200, task.ID); err != nil {
		t.Fatalf("age task: %v", err)
	}
	n, err := db.DeleteExpiredTasks(ctx, cutoff, 10)
	if err != nil || n != 0 {
		t.Fatalf("active 任务不得被 GC：n=%d err=%v", n, err)
	}
	if att, err := db.GetTaskAttempt(ctx, task.ID, attempt); err != nil || att.ReapedAt != nil {
		t.Fatalf("active attempt 事实必须保留：%+v err=%v", att, err)
	}
	// 正常结束后（reaped）才可被 GC。
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, lease, 0, store.TaskResult{Phase: store.TaskSucceeded}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n, err := db.DeleteExpiredTasks(ctx, cutoff, 10); err != nil || n != 1 {
		t.Fatalf("reaped 后应可 GC：n=%d err=%v", n, err)
	}
}
