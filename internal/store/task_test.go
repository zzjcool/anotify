package store

// task_test.go 覆盖 v4 任务执行链（task.md A3、capability.md §8）：
//
//	建任务（pending，钉 ns 输入快照）→ assign（只记候选）→ grant（认证身份 + 真实 slot
//	换租约，attempt+1，写 attempt 机器快照）→ renew / complete / cancel / 回收。
//
// v3 的状态机（Transition/StartTask/AckTask/status 九态、按逻辑任务一把释放、
// 独立 status writer）已整体删除：v4 只有六个相位，派发步骤是事实不是状态。
// 这些退役用例的逐条映射见 TEST-MAPPING.md。

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// mkTask 造一条未落库的 pending 任务（trace/capability 必填，params 默认 {}）。
func mkTask(nsID, traceID string, cap *CapabilityVersion) *Task {
	return &Task{
		NamespaceID:  nsID,
		TraceID:      traceID,
		CapabilityID: cap.ID,
		Capability:   cap.Name,
		Version:      cap.Version,
		Digest:       cap.Digest,
		Params:       json.RawMessage(`{}`),
	}
}

// pushToPhase 直接改相位：**只用于造种子行**（保留期/统计/积压类用例需要「已经终态」
// 的历史行）。正常路径必须走 grant/complete/cancel——那才是被验的状态机。
func pushToPhase(t *testing.T, db *DB, taskID, phase string) {
	t.Helper()
	if !IsTaskPhase(phase) {
		t.Fatalf("pushToPhase: %q 不在相位集合里", phase)
	}
	mustExec(t, db, `UPDATE tasks SET phase=? WHERE id=?`, phase, taskID)
}

// v4ReapInput 从 attempt 行构造**精确历史绑定**（node/task/attempt/lease/slot/instance）。
func v4ReapInput(t *testing.T, db *DB, nodeID, taskID string, attempt int) ReapedInputs {
	t.Helper()
	att, err := db.GetTaskAttempt(context.Background(), taskID, attempt)
	if err != nil {
		t.Fatalf("get attempt %s/%d: %v", taskID, attempt, err)
	}
	return ReapedInputs{
		TaskID: taskID, Attempt: attempt,
		NodeID: nodeID, NodeTokenHash: v4Auth(t, db, nodeID).NodeTokenHash,
		LeaseID: att.LeaseID, Slot: att.Slot, InstanceID: att.InstanceID,
	}
}

// v4Complete 以当前认证身份 + 当前 attempt/lease 提交一份给定的结果事实。
func v4Complete(t *testing.T, db *DB, nodeID, taskID, leaseID string, attempt int, result TaskResult) TaskResultDisposition {
	t.Helper()
	disp, err := db.CompleteTaskClassifiedAuthorized(context.Background(), v4Auth(t, db, nodeID),
		taskID, attempt, leaseID, Now(), result)
	if err != nil {
		t.Fatalf("complete task %s: %v", taskID, err)
	}
	return disp
}

// TestTaskPhaseSet 相位是封闭的六个值，终态四类。
func TestTaskPhaseSet(t *testing.T) {
	want := []string{TaskPending, TaskRunning, TaskSucceeded, TaskFailed, TaskCancelled, TaskAbandoned}
	got := TaskPhases()
	if len(got) != len(want) {
		t.Fatalf("TaskPhases() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TaskPhases()[%d] = %q, want %q", i, got[i], want[i])
		}
		if !IsTaskPhase(want[i]) {
			t.Fatalf("IsTaskPhase(%q) = false", want[i])
		}
	}
	for _, terminal := range []string{TaskSucceeded, TaskFailed, TaskCancelled, TaskAbandoned} {
		if !IsTerminalPhase(terminal) {
			t.Fatalf("IsTerminalPhase(%q) = false, want true", terminal)
		}
	}
	for _, open := range []string{TaskPending, TaskRunning} {
		if IsTerminalPhase(open) {
			t.Fatalf("IsTerminalPhase(%q) = true, want false", open)
		}
	}
	for _, bogus := range []string{"assigned", "acked", "lease_expired", "status"} {
		if IsTaskPhase(bogus) {
			t.Fatalf("IsTaskPhase(%q) = true：v3 派发步骤不是 v4 相位", bogus)
		}
	}
}

// TestCreateTasksAtomic 一次扇出里有一条非法 → 整批 0 写。
func TestCreateTasksAtomic(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "fanout", 1)

	good := mkTask(nsID, "tr_atomic", c)
	bad := &Task{NamespaceID: nsID, TraceID: "tr_atomic", CapabilityID: "cap_missing", Capability: "missing"}
	if err := db.CreateTasks(ctx, []*Task{good, bad}); err == nil {
		t.Fatal("含不存在能力 ID 的批被接受（FK 未生效）")
	}
	list, err := db.ListTasks(ctx, nsID, TaskFilter{TraceID: "tr_atomic"})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("失败批次留下了 %d 行（应 0 写）", len(list))
	}
}

// TestCreateTasksFanout 同一次扇出共享 trace，子任务继承父任务的命名空间。
func TestCreateTasksFanout(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "fanout", 1)

	root := mkTask(nsID, "tr_fanout", c)
	if err := db.CreateTasks(ctx, []*Task{root}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	child := mkTask(nsID, "tr_fanout", c)
	child.NamespaceID = "" // 子任务不写 ns → 继承父任务
	child.ParentTaskID = &root.ID
	if err := db.CreateTasks(ctx, []*Task{child}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	got, err := db.GetTask(ctx, nsID, child.ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if got.ParentTaskID == nil || *got.ParentTaskID != root.ID {
		t.Fatalf("child parent = %v, want %s", got.ParentTaskID, root.ID)
	}
	if got.NamespaceID != nsID {
		t.Fatalf("child namespace = %q, want 继承 %q", got.NamespaceID, nsID)
	}
}

// TestCreateTasksRejectsNonPending 建任务只能落 pending。
func TestCreateTasksRejectsNonPending(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "direct", 1)

	task := mkTask(nsID, "tr_direct", c)
	task.Phase = TaskRunning
	if err := db.CreateTasks(ctx, []*Task{task}); err == nil {
		t.Fatal("直接以 running 建任务被接受（必须经 grant 推进）")
	}
}

// TestCreateTasksEmptyIsNoop 空批是 no-op（不是错误）。
func TestCreateTasksEmptyIsNoop(t *testing.T) {
	db := newTestDB(t)
	if err := db.CreateTasks(context.Background(), nil); err != nil {
		t.Fatalf("empty create tasks: %v", err)
	}
}

// TestTaskRoundTrip 存什么读什么（含 params / digest / configRevision 诊断字段）。
func TestTaskRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)

	task := mkTask(nsID, "tr_roundtrip", c)
	task.Params = json.RawMessage(`{"diff":"a","n":2}`)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.TraceID != "tr_roundtrip" || got.CapabilityID != c.ID ||
		got.Capability != c.Name || got.Version != c.Version || got.Digest != c.Digest {
		t.Fatalf("任务身份字段往返不一致: %+v", got)
	}
	if string(got.Params) != `{"diff":"a","n":2}` {
		t.Fatalf("params 往返 = %s", got.Params)
	}
	if got.Phase != TaskPending || got.Attempt != 0 {
		t.Fatalf("新任务应 pending/attempt=0，得到 %s/%d", got.Phase, got.Attempt)
	}
	if got.MaxAttempts != artifact.DefaultMaxAttempts {
		t.Fatalf("maxAttempts 默认 = %d, want %d", got.MaxAttempts, artifact.DefaultMaxAttempts)
	}
}

// TestListTasksEmptyIsArray 空列表是 []（非 nil）。
func TestListTasksEmptyIsArray(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	list, err := db.ListTasks(ctx, nsID, TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if list == nil {
		t.Fatal("空任务列表返回 nil，want 空切片")
	}
	if len(list) != 0 {
		t.Fatalf("新命名空间有 %d 条任务，want 0", len(list))
	}
}

// TestListTasksFilters 过滤条件（phase / trace / capability）都要真的生效。
func TestListTasksFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	review := v4Cap(t, db, nsID, "review", 1)
	fix := v4Cap(t, db, nsID, "fix", 1)

	t1 := mkTask(nsID, "tr_a", review)
	t2 := mkTask(nsID, "tr_b", fix)
	if err := db.CreateTasks(ctx, []*Task{t1, t2}); err != nil {
		t.Fatalf("create tasks: %v", err)
	}
	pushToPhase(t, db, t2.ID, TaskSucceeded)

	byTrace, err := db.ListTasks(ctx, nsID, TaskFilter{TraceID: "tr_a"})
	if err != nil {
		t.Fatalf("list by trace: %v", err)
	}
	if len(byTrace) != 1 || byTrace[0].ID != t1.ID {
		t.Fatalf("trace 过滤 = %+v", byTrace)
	}
	byPhase, err := db.ListTasks(ctx, nsID, TaskFilter{Phase: TaskSucceeded})
	if err != nil {
		t.Fatalf("list by phase: %v", err)
	}
	if len(byPhase) != 1 || byPhase[0].ID != t2.ID {
		t.Fatalf("phase 过滤 = %+v", byPhase)
	}
	byCap, err := db.ListTasks(ctx, nsID, TaskFilter{Capability: "review"})
	if err != nil {
		t.Fatalf("list by capability: %v", err)
	}
	if len(byCap) != 1 || byCap[0].ID != t1.ID {
		t.Fatalf("capability 过滤 = %+v", byCap)
	}
}

// TestTaskNamespaceIsolation 任务只在所属命名空间可见。
func TestTaskNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB := newTestExtraNS(t, db, "task-other")
	ca := v4Cap(t, db, nsA, "review", 1)
	cb := v4Cap(t, db, nsB, "review", 1)

	ta := mkTask(nsA, "tr_iso", ca)
	tb := mkTask(nsB, "tr_iso", cb)
	if err := db.CreateTasks(ctx, []*Task{ta, tb}); err != nil {
		t.Fatalf("create tasks: %v", err)
	}
	listA, err := db.ListTasks(ctx, nsA, TaskFilter{})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != ta.ID {
		t.Fatalf("A 看到 %+v, want 只有 %s", listA, ta.ID)
	}
	if _, err := db.GetTask(ctx, nsA, tb.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A 里读到 B 的任务: %v", err)
	}
}

// TestAssignRecordsCandidateWithoutAttempt assign 只记候选：不计 attempt、不占槽、相位仍 pending。
func TestAssignRecordsCandidateWithoutAttempt(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-assign", 2)
	task := v4Task(t, db, nsID, c, "tr_assign", "")

	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-1", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	got := v4MustPhase(t, db, nsID, task.ID, TaskPending)
	if got.Attempt != 0 {
		t.Fatalf("assign 计了 attempt: %d", got.Attempt)
	}
	if got.AssignmentID != "asg-1" || got.NodeID == nil || *got.NodeID != node.ID {
		t.Fatalf("候选事实未落库: %+v", got)
	}
	if got.AssignmentExpiresAt == nil {
		t.Fatal("assign 必须带统一有界窗口 assignmentExpiresAt")
	}
	if v4BusySlots(t, db, node.ID) != 0 {
		t.Fatal("assign 不该占物理槽位")
	}
}

// TestGrantMovesToRunning 认证身份 + 真实 slot 换租约：attempt=1、槽位被占、token 只存 hash。
func TestGrantMovesToRunning(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-grant", 1)
	task := v4Task(t, db, nsID, c, "tr_grant", "")

	slot := v4FreeSlot(t, db, node.ID)
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-grant", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	leaseID, token, attempt, expiresAt, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID),
		task.ID, "asg-grant", 1, slot, Now())
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if attempt != 1 || leaseID == "" || token == "" {
		t.Fatalf("grant 返回 attempt=%d lease=%q token=%q", attempt, leaseID, token)
	}
	if expiresAt <= Now() {
		t.Fatalf("expiresAt=%d 不在未来", expiresAt)
	}
	got := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if got.Attempt != 1 || got.LeaseID != leaseID || got.LeaseUntil == nil {
		t.Fatalf("授权事实未落库: %+v", got)
	}
	if got.AttemptTokenHash == "" || got.AttemptTokenHash == token {
		t.Fatalf("attempt token 必须以 hash 存储（明文只在 grant 返回一次），得到 %q", got.AttemptTokenHash)
	}
	if v4BusySlots(t, db, node.ID) != 1 {
		t.Fatal("grant 必须占住真实槽位")
	}
	// token 能验回同一个任务；乱码 token 必须拒。
	if byTok, err := db.VerifyAttemptToken(ctx, task.ID, token); err != nil || byTok.ID != task.ID {
		t.Fatalf("VerifyAttemptToken = %+v, %v", byTok, err)
	}
	if _, err := db.VerifyAttemptToken(ctx, task.ID, "not-the-token"); !errors.Is(err, ErrAttemptTokenInvalid) {
		t.Fatalf("错误 token 应报 ErrAttemptTokenInvalid，得到 %v", err)
	}
}

// TestGrantRejectsStaleAssignment 旧 assignment 的 grant 被拒且不占槽。
func TestGrantRejectsStaleAssignment(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-stale", 1)
	task := v4Task(t, db, nsID, c, "tr_stale", "")

	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-new", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	_, _, _, _, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID), task.ID, "asg-old", 1, 0, Now())
	if !errors.Is(err, ErrNoAssignment) {
		t.Fatalf("旧 assignment 应报 ErrNoAssignment，得到 %v", err)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskPending)
	if v4BusySlots(t, db, node.ID) != 0 {
		t.Fatal("被拒的 grant 留下了占用槽位")
	}
}

// TestRenewLeaseAuthorized 续租只延长当前 attempt 的租约；旧 attempt 被拒。
func TestRenewLeaseAuthorized(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-renew", 1)
	task := v4Task(t, db, nsID, c, "tr_renew", "")
	leaseID, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-renew")

	before := v4MustPhase(t, db, nsID, task.ID, TaskRunning).LeaseUntil
	if before == nil {
		t.Fatal("grant 后应有 leaseUntil")
	}
	until, err := db.RenewLeaseAuthorized(ctx, v4Auth(t, db, node.ID), task.ID, attempt, leaseID, Now()+int64(protocol.LeaseTTL.Seconds())/2)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if until <= *before {
		t.Fatalf("续租没有延长租约: before=%d after=%d", *before, until)
	}
	if _, err := db.RenewLeaseAuthorized(ctx, v4Auth(t, db, node.ID), task.ID, attempt+1, leaseID, Now()); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("错 attempt 续租应报 ErrLeaseMismatch，得到 %v", err)
	}
	if _, err := db.RenewLeaseAuthorized(ctx, v4Auth(t, db, node.ID), task.ID, attempt, "lease_bogus", Now()); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("错 leaseId 续租应报 ErrLeaseMismatch，得到 %v", err)
	}
}

// TestCompleteRecordsResultAndFinishedAt 结果事实落库：相位、exitCode、finishedAt、acceptedAt。
func TestCompleteRecordsResultAndFinishedAt(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-complete", 1)
	task := v4Task(t, db, nsID, c, "tr_complete", "")
	leaseID, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-complete")

	exit := 0
	// v4 不替节点编造执行时间：结果是节点进程的事实，必须显式带上。
	started := Now() * 1e9
	finished := started + int64(2*1e9)
	result := TaskResult{
		Phase:      TaskSucceeded,
		Summary:    "review 通过",
		Artifacts:  []protocol.TaskArtifact{},
		ExitCode:   &exit,
		StartedAt:  timeAtUnixNano(started),
		FinishedAt: timeAtUnixNano(finished),
	}
	disp := v4Complete(t, db, node.ID, task.ID, leaseID, attempt, result)
	if disp != TaskResultRecorded {
		t.Fatalf("首次结果应 recorded，得到 %q", disp)
	}
	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Phase != TaskSucceeded || got.Summary != "review 通过" {
		t.Fatalf("结果未落库: %+v", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("exitCode 丢失: %v", got.ExitCode)
	}
	if got.FinishedAt == nil || got.ResultAcceptedAt == nil {
		t.Fatalf("finishedAt/resultAcceptedAt 未写: %+v", got)
	}
	if v4BusySlots(t, db, node.ID) != 0 {
		t.Fatal("任务终态后应释放物理槽位")
	}
}

// TestCompleteRequiresGrant 没 grant 的任务不能直接报结果（v3 的 implied-running 已退役）。
func TestCompleteRequiresGrant(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-nogrant", 1)
	task := v4Task(t, db, nsID, c, "tr_nogrant", "")

	_, err := db.CompleteTaskClassifiedAuthorized(context.Background(), v4Auth(t, db, node.ID),
		task.ID, 1, "lease_missing", Now(), TaskResult{Phase: TaskSucceeded})
	// v4 没有「未 grant 但处于 running」的隐含状态：attempt 1 没有保留的历史绑定，
	// 结果提交在绑定核对这一层就被拒（ErrLeaseMismatch），比相位判断更早也更严。
	if !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("未 grant 的任务报结果应报 ErrLeaseMismatch，得到 %v", err)
	}
}

// TestCompleteDuplicateIsIdempotent 完全相同的结果事实重复提交 = duplicate（不写第二份）。
func TestCompleteDuplicateIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-dup", 1)
	task := v4Task(t, db, nsID, c, "tr_dup", "")
	leaseID, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-dup")

	exit := 2
	started := Now() * 1e9
	finished := started + int64(3*1e9)
	result := TaskResult{
		Phase:      TaskFailed,
		Reason:     "exit_code",
		Summary:    "失败了",
		Error:      "boom",
		Artifacts:  []protocol.TaskArtifact{},
		ExitCode:   &exit,
		StartedAt:  timeAtUnixNano(started),
		FinishedAt: timeAtUnixNano(finished),
	}
	if disp := v4Complete(t, db, node.ID, task.ID, leaseID, attempt, result); disp != TaskResultRecorded {
		t.Fatalf("首次结果应 recorded，得到 %q", disp)
	}
	if disp := v4Complete(t, db, node.ID, task.ID, leaseID, attempt, result); disp != TaskResultDuplicate {
		t.Fatalf("同事实重复提交应 duplicate，得到 %q", disp)
	}
	changed := result
	changed.Summary = "换了个说法"
	if _, err := db.CompleteTaskClassifiedAuthorized(context.Background(), v4Auth(t, db, node.ID),
		task.ID, attempt, leaseID, Now(), changed); err == nil {
		t.Fatal("同一 attempt 提交不同结果事实被接受（结果不可改写）")
	}
}

// TestTerminalPhaseIsImmutable 终态后不能再 grant / complete / 改相位。
func TestTerminalPhaseIsImmutable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-terminal", 1)
	task := v4Task(t, db, nsID, c, "tr_terminal", "")

	if err := db.RequestCancel(ctx, task.ID, "", Now()); err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	got := v4MustPhase(t, db, nsID, task.ID, TaskCancelled)
	if got.FinishedAt == nil {
		t.Fatal("取消后应写 finishedAt")
	}
	slot := v4FreeSlot(t, db, node.ID)
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-after", 1); err == nil {
		t.Fatal("终态任务还能 assign")
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID), task.ID, "asg-after", 1, slot, Now()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态任务 grant 应报 ErrInvalidTransition，得到 %v", err)
	}
	if err := db.RequestCancel(ctx, task.ID, "again", Now()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态任务再次取消应报 ErrInvalidTransition，得到 %v", err)
	}
}

// TestCancelRunningAwaitsAck running 任务的取消要等 cancel.ack 回收进程组才落终态。
func TestCancelRunningAwaitsAck(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-cancel", 1)
	task := v4Task(t, db, nsID, c, "tr_cancel", "")
	leaseID, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-cancel")

	if err := db.RequestCancel(ctx, task.ID, "", Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	// 取消在飞：相位仍是 running，但 cancel_requested_at 有值；结果不得被接受。
	got := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if got.CancelRequestedAt == nil {
		t.Fatal("取消意图未落库")
	}
	exit := 0
	errResult := TaskResult{Phase: TaskSucceeded, Artifacts: []protocol.TaskArtifact{}, ExitCode: &exit}
	if _, err := db.CompleteTaskClassifiedAuthorized(ctx, v4Auth(t, db, node.ID),
		task.ID, attempt, leaseID, Now(), errResult); !errors.Is(err, ErrTaskNotRunning) {
		t.Fatalf("取消在飞时 result 应拒（ErrTaskNotRunning），得到 %v", err)
	}
	in := v4ReapInput(t, db, node.ID, task.ID, attempt)
	in.Now = Now()
	if err := db.AckCancelAuthorized(ctx, v4Auth(t, db, node.ID), in, true); err != nil {
		t.Fatalf("ack cancel: %v", err)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskCancelled)
}

// TestRecoverLostLeasesRequeues 过期租约按幂等声明分派：可重试 → 回 pending，不可重试 → abandoned。
func TestRecoverLostLeasesRequeues(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	idem := v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: "idem", Version: 1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}},
			Lifecycle: artifact.Lifecycle{
				Idempotent: true,
			},
		},
	})
	nonIdem := v4Cap(t, db, nsID, "nonidem", 1)

	for _, tc := range []struct {
		name string
		cap  *CapabilityVersion
		want string
	}{
		{"idempotent-requeues", idem, TaskPending},
		{"non-idempotent-abandons", nonIdem, TaskAbandoned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := v4Node(t, db, "node-"+tc.name, 1)
			task := v4Task(t, db, nsID, tc.cap, "tr_"+tc.name, "")
			slot := v4FreeSlot(t, db, node.ID)
			if err := db.AssignTask(ctx, task.ID, node.ID, "asg-"+tc.name, 1); err != nil {
				t.Fatalf("assign: %v", err)
			}
			_, _, _, expiresAt, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID),
				task.ID, "asg-"+tc.name, 1, slot, Now())
			if err != nil {
				t.Fatalf("grant: %v", err)
			}
			requeued, abandoned, err := db.RecoverLostLeases(ctx, expiresAt+1)
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if requeued+abandoned != 1 {
				t.Fatalf("回收数 requeued=%d abandoned=%d, want 合计 1", requeued, abandoned)
			}
			v4MustPhase(t, db, nsID, task.ID, tc.want)
		})
	}
}

// TestMaxAttemptsExceededAbandons 尝试预算用尽后不再派发（任务转 abandoned）。
func TestMaxAttemptsExceededAbandons(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	one := 1
	c := v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: "once", Version: 1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}},
			Lifecycle: artifact.Lifecycle{
				Idempotent:  true,
				MaxAttempts: &one,
			},
		},
	})
	node := v4Node(t, db, "node-budget", 1)
	task := v4Task(t, db, nsID, c, "tr_budget", "")
	// 建行时是默认预算（3）；真正的值在**派发时**从钉住的能力正文里取。
	if task.MaxAttempts != artifact.DefaultMaxAttempts {
		t.Fatalf("建任务时默认 maxAttempts = %d, want %d", task.MaxAttempts, artifact.DefaultMaxAttempts)
	}
	_, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-budget-1")
	dispatched := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if dispatched.MaxAttempts != 1 {
		t.Fatalf("派发时应钉住 spec 的 maxAttempts=1，得到 %d", dispatched.MaxAttempts)
	}
	if attempt != 1 {
		t.Fatalf("首次 grant attempt = %d, want 1", attempt)
	}
	// 租约过期并被回收：这次 attempt 已经用尽预算（attempt 1 / 预算 1）→
	// 回收直接把任务置终局 abandoned，原因码 = max_attempts_exceeded
	// （不再排一次注定被拒的 grant：预算是钉住的正文事实）。
	granted := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if granted.LeaseUntil == nil {
		t.Fatal("grant 后应有 leaseUntil")
	}
	if _, _, err := db.RecoverLostLeases(ctx, *granted.LeaseUntil+1); err != nil {
		t.Fatalf("recover: %v", err)
	}
	dead := v4MustPhase(t, db, nsID, task.ID, TaskAbandoned)
	if dead.Error != protocol.ReasonMaxAttemptsExceeded {
		t.Fatalf("abandoned 原因 = %q, want %q", dead.Error, protocol.ReasonMaxAttemptsExceeded)
	}
	// 终局后不可再派发（预算用尽的事实已经落库）。
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-budget-2", 2); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("abandoned 任务再 assign 应报 ErrInvalidTransition，得到 %v", err)
	}
}

// TestTaskAttemptSnapshotRoundTrip attempt 快照（machine vars/limits/slot/lease）可回读。
func TestTaskAttemptSnapshotRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-attempt", 1)
	putV4NodeCfg(t, db, node.ID, []string{"default"}, map[string]string{"REGION": "cn"}, map[string]any{"total": 1})
	v4RecordApplied(t, db, node.ID)
	task := v4Task(t, db, nsID, c, "tr_attempt", "")
	leaseID, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-attempt")

	att, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if att.LeaseID != leaseID || att.NodeID != node.ID || att.Phase != AttemptPhaseRunning {
		t.Fatalf("attempt 事实不一致: %+v", att)
	}
	if att.MachineVars["REGION"] != "cn" {
		t.Fatalf("attempt 机器 vars 快照缺失: %v", att.MachineVars)
	}
	all, err := db.ListTaskAttempts(ctx, task.ID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(all) != 1 || all[0].Attempt != attempt {
		t.Fatalf("attempt 列表 = %+v", all)
	}
	inputs, err := db.TaskInputs(ctx, task.ID, []string{"REGION"})
	if err != nil {
		t.Fatalf("task inputs: %v", err)
	}
	if inputs["REGION"].Value != "cn" || inputs["REGION"].Source != "node" {
		t.Fatalf("TaskInputs REGION = %+v, want node 层的 cn", inputs["REGION"])
	}
}

// TestLeaseConstants 派发/租约/退避的边界常量都是显式值（不许靠魔法数）。
func TestLeaseConstants(t *testing.T) {
	if AssignmentWindowSec != 30 {
		t.Fatalf("AssignmentWindowSec = %d, want 30", AssignmentWindowSec)
	}
	if protocol.LeaseTTL <= 0 {
		t.Fatalf("protocol.LeaseTTL = %v, want > 0", protocol.LeaseTTL)
	}
	if TaskRejectSlotUnavailable != "slot_unavailable" {
		t.Fatalf("TaskRejectSlotUnavailable = %q", TaskRejectSlotUnavailable)
	}
	if RetryBackoff(1) <= 0 || RetryBackoff(2) <= RetryBackoff(1) {
		t.Fatalf("RetryBackoff 应随 attempt 增长: %d/%d", RetryBackoff(1), RetryBackoff(2))
	}
	if SlotUnavailableBackoff(1) <= 0 || StaleConfigBackoff(1) <= 0 {
		t.Fatal("槽位/配置退避必须为正")
	}
}

// TestTaskTokenNeverSerializedToJSON attempt token 的 hash 与非公开快照不得进 JSON。
func TestTaskTokenNeverSerializedToJSON(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "node-json", 1)
	task := v4Task(t, db, nsID, c, "tr_json", "")
	v4Grant(t, db, task.ID, node.ID, "asg-json")

	got, err := db.GetTask(context.Background(), nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode task JSON: %v", err)
	}
	for _, forbidden := range []string{"attemptTokenHash", "namespaceVars", "leaseInstanceId", "resultJson"} {
		if _, ok := decoded[forbidden]; ok {
			t.Fatalf("任务 JSON 泄漏了内部字段 %q: %s", forbidden, raw)
		}
	}
}

// TestCreateTasksMixedBatchOversizedParamsWritesNothing 父裁定（limited review）：
// 超限 params 是**整批**拒绝——混在合法任务里也一样，一条都不建；
// 恰好等于上限则必须接受并逐字节往返。错误类型保留现有 detail 字段（谁/多大/上限）。
func TestCreateTasksMixedBatchOversizedParamsWritesNothing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "params-size", 1)

	good := mkTask(nsID, "tr_size_good", c)
	big := make([]byte, MaxTaskParamsBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	bad := mkTask(nsID, "tr_size_bad", c)
	bad.Params = append(append([]byte(`{"p":"`), big...), '"', '}')
	if err := db.CreateTasks(ctx, []*Task{good, bad}); err == nil {
		t.Fatal("混批里的超限 params 被接受")
	} else {
		var tooLarge *ParamsTooLargeError
		if !errors.As(err, &tooLarge) {
			t.Fatalf("应报 *ParamsTooLargeError，得到 %T: %v", err, err)
		}
		if tooLarge.TaskID != bad.ID || tooLarge.Size != len(bad.Params) || tooLarge.Limit != MaxTaskParamsBytes {
			t.Fatalf("错误 detail = %+v, want task=%q size=%d limit=%d",
				tooLarge, bad.ID, len(bad.Params), MaxTaskParamsBytes)
		}
	}
	// 整批 0 行：合法的 good 也不许留下。
	for _, trace := range []string{"tr_size_good", "tr_size_bad"} {
		if n, err := db.CountTasksByTrace(ctx, nsID, trace); err != nil {
			t.Fatalf("count %s: %v", trace, err)
		} else if n != 0 {
			t.Fatalf("超限混批留下了 %d 行（trace=%s）", n, trace)
		}
	}

	// 恰好等于上限：接受 + 逐字节往返。
	atLimit := make([]byte, 0, MaxTaskParamsBytes)
	atLimit = append(atLimit, []byte(`{"p":"`)...)
	filler := MaxTaskParamsBytes - len(atLimit) - 2 // 留 `"}`
	for i := 0; i < filler; i++ {
		atLimit = append(atLimit, 'y')
	}
	atLimit = append(atLimit, '"', '}')
	if len(atLimit) != MaxTaskParamsBytes {
		t.Fatalf("夹具长度 = %d, want %d", len(atLimit), MaxTaskParamsBytes)
	}
	exact := mkTask(nsID, "tr_size_exact", c)
	exact.Params = atLimit
	if err := db.CreateTasks(ctx, []*Task{exact}); err != nil {
		t.Fatalf("恰好等于上限的 params 应接受: %v", err)
	}
	got, err := db.GetTask(ctx, nsID, exact.ID)
	if err != nil {
		t.Fatalf("get exact task: %v", err)
	}
	if string(got.Params) != string(atLimit) {
		t.Fatalf("上限 params 往返失真：%d → %d 字节", len(atLimit), len(got.Params))
	}
}

// TestCompleteRejectsInvalidResultPhaseWithZeroWrites invalid result.phase 必须在
// **相位闸门**（execution_auth 的终态集合检查）被拒——用当前有效认证身份 + 正确
// lease/attempt 走到那一步，而不是靠坏 token/slot 在更早的绑定核对里被挡掉。
// 拒绝后整行任务、attempt 账本、结果事实全部零写。
func TestCompleteRejectsInvalidResultPhaseWithZeroWrites(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "phase-guard", 1)
	node := v4Node(t, db, "node-phase-guard", 1)
	task := v4Task(t, db, nsID, c, "tr_phase_guard", "")
	leaseID, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-phase-guard")

	beforeTask, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task before: %v", err)
	}
	beforeAttempt, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("get attempt before: %v", err)
	}
	beforeBusy := v4BusySlots(t, db, node.ID)

	exit := 0
	invalid := TaskResult{Phase: TaskRunning, ExitCode: &exit}
	_, err = db.CompleteTaskClassifiedAuthorized(ctx, v4Auth(t, db, node.ID),
		task.ID, attempt, leaseID, Now(), invalid)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("非法 result.phase 应报 ErrInvalidTransition，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "result phase") {
		t.Fatalf("错误应点名 result phase（证明走到相位闸门而不是绑定核对）: %v", err)
	}

	afterTask, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task after: %v", err)
	}
	if !reflect.DeepEqual(beforeTask, afterTask) {
		t.Fatalf("被拒结果改写了任务行:\nbefore %+v\nafter  %+v", beforeTask, afterTask)
	}
	afterAttempt, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("get attempt after: %v", err)
	}
	if !reflect.DeepEqual(beforeAttempt, afterAttempt) {
		t.Fatalf("被拒结果改写了 attempt 账本:\nbefore %+v\nafter  %+v", beforeAttempt, afterAttempt)
	}
	if n := len(v4Attempts(t, db, task.ID)); n != 1 {
		t.Fatalf("被拒结果新增了 attempt 行，现 %d", n)
	}
	if got := v4BusySlots(t, db, node.ID); got != beforeBusy {
		t.Fatalf("被拒结果改动了槽位占用: %d → %d", beforeBusy, got)
	}
}

// TestCompleteErrorProjectionKeepsRawResultFacts 旧投影规则（normalizeTaskError）仍在：
// 失败结果 Error 为空 → Task.error 落到 Reason；但完整 ResultJSON **从不**被投影改写，
// 原始 error 仍是空串，reason/phase/summary 原样保留。
func TestCompleteErrorProjectionKeepsRawResultFacts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "projection", 1)
	node := v4Node(t, db, "node-projection", 1)
	task := v4Task(t, db, nsID, c, "tr_projection", "")
	leaseID, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-projection")

	exit := 7
	started := Now() * 1e9
	finished := started + int64(1e9)
	result := TaskResult{
		Phase: TaskFailed, Reason: "exit_code", Summary: "失败了",
		Error: "", ExitCode: &exit,
		StartedAt: timeAtUnixNano(started), FinishedAt: timeAtUnixNano(finished),
	}
	if disp := v4Complete(t, db, node.ID, task.ID, leaseID, attempt, result); disp != TaskResultRecorded {
		t.Fatalf("complete: %q", disp)
	}
	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Error != "exit_code" {
		t.Fatalf("Task.error 投影 = %q, want Reason %q", got.Error, "exit_code")
	}
	raw := decodeRawResult(t, got.ResultJSON)
	if raw["error"] != "" {
		t.Fatalf("原始 ResultJSON.error 被投影改写了: %v", raw["error"])
	}
	if raw["reason"] != "exit_code" || raw["phase"] != TaskFailed || raw["summary"] != "失败了" {
		t.Fatalf("原始 ResultJSON 事实丢失: %+v", raw)
	}
	if raw["exitCode"] != float64(7) {
		t.Fatalf("原始 ResultJSON.exitCode = %v, want 7", raw["exitCode"])
	}
}

// TestCompleteSuccessorAttemptClearsProjectionAndKeepsSubmittedError：
//   - 后继 attempt 成功且 incoming Error/Reason 为空 → Task.error 写空（不残留上一代投影）；
//   - 成功结果带**非空** submitted Error → 按现实现保留（不施加 v3“成功必清 error”的旧措辞）。
//
// 「上一代投影残留」在 v4 只有一条真实可达路径：幂等能力租约过期 → attempt 重排
// （旧 attempt 的进程可能还在跑，槽位保持占用）。v4 的失败 attempt 不会把 error 写到
// 任务行（那是终态才有的事实），所以这里用一行显式种子模拟「先前投影」，再走真实的
// 重排 + 第二代 grant/complete 验证写路径会把它清掉。
func TestCompleteSuccessorAttemptClearsProjectionAndKeepsSubmittedError(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	// 重排时旧 attempt 的槽位保持占用（进程可能还在跑），所以这一代的并发预算
	// 必须允许 2：node 也申报 2 个真实槽位。
	two := 2
	c := v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: "projection-retry", Version: 1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}},
			Lifecycle: artifact.Lifecycle{
				Idempotent: true,
			},
			Concurrency: artifact.Concurrency{MaxPerNode: &two},
		},
	})
	node := v4Node(t, db, "node-projection-retry", 2)
	task := v4Task(t, db, nsID, c, "tr_projection_retry", "")

	// 第一代：grant 后租约过期 → RecoverLostLeases 把它重排回 pending
	// （幂等 + attempt 1 < 预算 3；旧 attempt 不写 reaped、槽位保持占用）。
	_, _, attempt1 := v4Grant(t, db, task.ID, node.ID, "asg-projection-1")
	running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if running.LeaseUntil == nil {
		t.Fatal("grant 后应有 leaseUntil")
	}
	if _, _, err := db.RecoverLostLeases(ctx, *running.LeaseUntil+1); err != nil {
		t.Fatalf("recover: %v", err)
	}
	v4MustPhase(t, db, nsID, task.ID, TaskPending)
	// 显式种子：模拟上一代遗留在任务行上的 error 投影。
	mustExec(t, db, `UPDATE tasks SET error = ? WHERE id = ?`, "stale_projection", task.ID)

	// 第二代：真实 assign + grant（另一台真实空闲槽），然后成功且不带 error/reason。
	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-projection-2", 2); err != nil {
		t.Fatalf("assign second attempt: %v", err)
	}
	slot2 := v4FreeSlot(t, db, node.ID)
	lease2, _, attempt2, _, gerr := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID),
		task.ID, "asg-projection-2", 2, slot2, Now())
	if gerr != nil {
		t.Fatalf("grant second attempt: %v", gerr)
	}
	if attempt2 <= attempt1 {
		t.Fatalf("第二代 attempt = %d, want > %d", attempt2, attempt1)
	}
	if disp := v4Complete(t, db, node.ID, task.ID, lease2, attempt2, TaskResult{
		Phase: TaskSucceeded, StartedAt: timeAtUnixNano(Now() * 1e9), FinishedAt: timeAtUnixNano(Now() * 1e9),
	}); disp != TaskResultRecorded {
		t.Fatalf("complete second attempt: %q", disp)
	}
	done, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get done task: %v", err)
	}
	if done.Phase != TaskSucceeded || done.Error != "" {
		t.Fatalf("后继成功后 phase/error = %q/%q, want succeeded/空", done.Phase, done.Error)
	}
	rawSuccess := decodeRawResult(t, done.ResultJSON)
	if rawSuccess["error"] != "" || rawSuccess["reason"] != "" {
		t.Fatalf("成功结果的原始 error/reason 应为空: %+v", rawSuccess)
	}

	// 非空 submitted Error 在成功结果里按现实现保留（不偷偷清掉）。
	task2 := v4Task(t, db, nsID, c, "tr_projection_keep", "")
	lease3, _, attempt3 := v4Grant(t, db, task2.ID, node.ID, "asg-projection-3")
	if disp := v4Complete(t, db, node.ID, task2.ID, lease3, attempt3, TaskResult{
		Phase: TaskSucceeded, Error: "warn: partial",
		StartedAt: timeAtUnixNano(Now() * 1e9), FinishedAt: timeAtUnixNano(Now() * 1e9),
	}); disp != TaskResultRecorded {
		t.Fatalf("complete keep-error: %q", disp)
	}
	kept, err := db.GetTask(ctx, nsID, task2.ID)
	if err != nil {
		t.Fatalf("get kept task: %v", err)
	}
	if kept.Error != "warn: partial" {
		t.Fatalf("成功结果里的 submitted Error 被改写: %q", kept.Error)
	}
	if rawKept := decodeRawResult(t, kept.ResultJSON); rawKept["error"] != "warn: partial" {
		t.Fatalf("原始 ResultJSON.error = %v, want submitted 值", rawKept["error"])
	}
}

// v4Attempts 列出任务的 attempt 账本（数量断言用）。
func v4Attempts(t *testing.T, db *DB, taskID string) []*TaskAttempt {
	t.Helper()
	all, err := db.ListTaskAttempts(context.Background(), taskID)
	if err != nil {
		t.Fatalf("list attempts %s: %v", taskID, err)
	}
	return all
}

// decodeRawResult 解原始 ResultJSON（投影绝不能反写它）。
func decodeRawResult(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("ResultJSON 为空")
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode ResultJSON %s: %v", raw, err)
	}
	return out
}
