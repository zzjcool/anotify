package store

// resolveLostOne 的**重试安全判据**回归用例（审查 M1）。
//
// M1 的病根：ResolveLostTasks（连接断了 → 主动释放）与 SweepExpiredLeases（租约过期 →
// 巡检回收）是同一个问题的两条腿，却各拿一套判据。修复前 resolveLostOne 只看 policy：
//
//	to := TaskPending
//	if policy == LostAbandon && !(cur == TaskAssigned && !startedAt.Valid) {
//	    to = TaskAbandoned
//	}
//
// 于是同一个状态在两条路径上得到**相反**的结论：
//
//   · 非幂等能力（lifecycle.idempotent:false）+ 已开始（running/started_at 非空）
//     + clean 结束（LostRequeue）→ 回 pending 且 not_before=NULL 立刻可重派 =
//     重放一个有副作用的任务（sweepOne 同状态会 abandoned，理由是
//     "capability is not idempotent"）；
//   · attempts 已到 lifecycle.maxAttempts（默认 3）→ 同样回 pending → 再派发
//     attempts 就变 4，重试预算被打穿、永不收敛（sweepOne 会 abandoned，
//     理由是 "attempts exhausted"）。
//
// 修复后两条路径共用一套判据，**顺序即优先级**：
//
//	① attempts >= maxAttempts            → abandoned（attempts exhausted）
//	② status==assigned && started_at IS NULL → pending（安全例外，与幂等性/policy 无关）
//	③ !lifecycle.idempotent              → abandoned（capability is not idempotent）
//	④ 其余 → policy==LostAbandon ? abandoned : pending
//
// 本文件四条用例的角色：
//
//	· T1 / T2 是**红转绿**：修复前必红（回 pending 而不是 abandoned），
//	  分别是 M1 的两个病因（非幂等重放、预算打穿）。
//	· T3 / T4 是**保正**（绿转红护栏）：修复前就绿，钉住「不许矫枉过正」——
//	  T3 钉住顺序（安全例外必须排在幂等性判断之前），T4 钉住正常重排没被误关。
//	  把判据顺序调换或把 default 改成 abandoned，这两条立刻变红。
//
// 全部断言只依赖库里的机器事实（GetTask / ListInstances / FreeSlotsByNode /
// ListRedispatchable），不看日志、不看内存状态。

import (
	"context"
	"strings"
	"testing"
)

// ---------- 夹具（slt 前缀，避免与同包其它测试文件的辅助函数撞名） ----------

// sltNonIdempotent 是「重放不安全」能力的 lifecycle，等价于 YAML 里写 idempotent:false。
func sltNonIdempotent() Lifecycle {
	no := false
	return Lifecycle{Idempotent: &no}
}

// sltFixture 建库 + 命名空间 + 一条带指定 lifecycle 的能力定义。
func sltFixture(t *testing.T, lc Lifecycle) (db *DB, nsID string, capv *CapabilityVersion) {
	t.Helper()
	db = newTestDB(t)
	nsID, _ = newTestNS(t, db)
	capv = mkCap(nsID, "deploy", 1, nil)
	capv.Lifecycle = lc
	if err := db.CreateCapability(context.Background(), capv); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	return db, nsID, capv
}

// sltNode 建一台在线、申报了 slots 个并发槽位的节点，返回 node_id。
func sltNode(t *testing.T, db *DB, nsID, name string, slots int) string {
	t.Helper()
	ctx := context.Background()
	node, _ := mkNode(t, db, nsID, name)
	if err := db.UpdateNodeStatus(ctx, nsID, node.ID, NodeStatusOnline); err != nil {
		t.Fatalf("online %s: %v", name, err)
	}
	specs := make([]InstanceSpec, slots)
	for i := range specs {
		specs[i] = InstanceSpec{Slot: i, Concurrency: 1}
	}
	if _, err := db.UpsertInstances(ctx, nsID, node.ID, specs); err != nil {
		t.Fatalf("instances %s: %v", name, err)
	}
	return node.ID
}

// sltTask 落库一条 pending 任务。
func sltTask(t *testing.T, db *DB, nsID, traceID string, capv *CapabilityVersion) *Task {
	t.Helper()
	task := mkTask(nsID, traceID, capv)
	if err := db.CreateTasks(context.Background(), []*Task{task}); err != nil {
		t.Fatalf("create task %s: %v", traceID, err)
	}
	return task
}

// sltDispatch 走生产派发路径（LeaseAndAssign：占槽位 + pending→assigned + attempts+1）。
func sltDispatch(t *testing.T, db *DB, nsID, nodeID, taskID string, leaseUntil int64) {
	t.Helper()
	if _, err := db.LeaseAndAssign(context.Background(), nsID, taskID, nodeID, leaseUntil); err != nil {
		t.Fatalf("lease and assign %s: %v", taskID, err)
	}
}

// sltStart 模拟真实执行时序的尾部：ack 帧（assigned→acked）+ progress 帧
// （acked→running + started_at + 续租）。这是唯一能同时得到「running」与
// 「started_at 非空」的路径——判据 ②/③ 的分界就在 started_at 上。
func sltStart(t *testing.T, db *DB, nsID, taskID string, leaseUntil int64) {
	t.Helper()
	ctx := context.Background()
	if err := db.Transition(ctx, nsID, taskID, TaskAssigned, TaskAcked); err != nil {
		t.Fatalf("ack %s: %v", taskID, err)
	}
	if err := db.ProgressTask(ctx, nsID, taskID, leaseUntil); err != nil {
		t.Fatalf("progress %s: %v", taskID, err)
	}
}

// sltBusySlots 返回节点上 busy_task 非空的槽位（slot → taskID）。
func sltBusySlots(t *testing.T, db *DB, nodeID string) map[int]string {
	t.Helper()
	insts, err := db.ListInstances(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list instances of %s: %v", nodeID, err)
	}
	out := map[int]string{}
	for _, in := range insts {
		if in.BusyTask != nil && *in.BusyTask != "" {
			out[in.Slot] = *in.BusyTask
		}
	}
	return out
}

// sltFreeSlots 返回节点空闲槽位数（alive 且 busy_task IS NULL）。
func sltFreeSlots(t *testing.T, db *DB, nodeID string) int {
	t.Helper()
	free, err := db.FreeSlotsByNode(context.Background(), []string{nodeID})
	if err != nil {
		t.Fatalf("free slots of %s: %v", nodeID, err)
	}
	return free[nodeID]
}

// sltRedispatchableIDs 返回「此刻可立刻重派」的任务 ID 集合。
func sltRedispatchableIDs(t *testing.T, db *DB, now int64) map[string]bool {
	t.Helper()
	tasks, err := db.ListRedispatchable(context.Background(), now, 100)
	if err != nil {
		t.Fatalf("list redispatchable: %v", err)
	}
	out := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		out[task.ID] = true
	}
	return out
}

// sltGet 读回任务（失败即 Fatal）。
func sltGet(t *testing.T, db *DB, nsID, taskID string) *Task {
	t.Helper()
	task, err := db.GetTask(context.Background(), nsID, taskID)
	if err != nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	return task
}

// sltAssertSlotReleased 断言槽位真的放掉了：这是 #218 的另一半
// （任务状态对了但 busy_task 还钉着 = 这台机器少一个并发额度，直到巡检兜底）。
func sltAssertSlotReleased(t *testing.T, db *DB, nodeID string, slots int) {
	t.Helper()
	if busy := sltBusySlots(t, db, nodeID); len(busy) != 0 {
		t.Fatalf("槽位仍被占：%v（释放路径必须同时清 busy_task）", busy)
	}
	if free := sltFreeSlots(t, db, nodeID); free != slots {
		t.Fatalf("free slots = %d, want %d", free, slots)
	}
}

// ---------- T1：非幂等 + 已开始 + clean 结束 → abandoned ----------

// TestResolveLostTasksAbandonsNonIdempotentStarted 钉住 M1 的病因之一：**重放非幂等任务**。
//
// 场景：能力声明 lifecycle.idempotent:false；任务已经真跑过（running + started_at 非空，
// 说明副作用可能已经发生）；连接是「干净关闭」（policy=LostRequeue，调用方认为执行者
// 确定已消失，可以重排）。修复前 clean 结束一律回 pending 且 not_before=NULL，
// 下一秒就被 ListRedispatchable 捞去重派 = 同一个副作用跑第二遍（正是 #222 要拦的）。
// 正确结论与巡检同口径：宁可 abandoned，也不重放。
func TestResolveLostTasksAbandonsNonIdempotentStarted(t *testing.T) {
	db, nsID, capv := sltFixture(t, sltNonIdempotent())
	ctx := context.Background()
	nodeID := sltNode(t, db, nsID, "mbp", 1)
	lease0 := Now() + 60
	const reason = "peer closed normally"

	task := sltTask(t, db, nsID, "trace_m1_nonidem", capv)
	sltDispatch(t, db, nsID, nodeID, task.ID, lease0)
	sltStart(t, db, nsID, task.ID, lease0) // running + started_at 非空

	before := sltGet(t, db, nsID, task.ID)
	if before.Status != TaskRunning || before.StartedAt == nil || before.Attempts != 1 {
		t.Fatalf("前置不成立：status=%q started_at=%v attempts=%d, want running/非空/1",
			before.Status, before.StartedAt, before.Attempts)
	}
	if len(sltBusySlots(t, db, nodeID)) != 1 {
		t.Fatalf("前置不成立：槽位没被占，%v", sltBusySlots(t, db, nodeID))
	}

	requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID,
		[]TaskOwner{{TaskID: task.ID, Attempts: before.Attempts}},
		LostRequeue, reason, Now())
	if err != nil {
		t.Fatalf("ResolveLostTasks: %v", err)
	}
	if requeued != 0 || abandoned != 1 {
		t.Fatalf("requeued/abandoned = %d/%d, want 0/1（非幂等能力执行过后不许重排）",
			requeued, abandoned)
	}

	after := sltGet(t, db, nsID, task.ID)
	if after.Status != TaskAbandoned {
		t.Fatalf("status = %q, want abandoned（回 pending 就是重放非幂等任务，M1）", after.Status)
	}
	if !strings.HasPrefix(after.Error, "executor_lost:") {
		t.Errorf("error = %q, want 前缀 executor_lost:", after.Error)
	}
	if !strings.Contains(after.Error, reason) {
		t.Errorf("error = %q, 丢了结束原因 %q（运维排不下去）", after.Error, reason)
	}
	if !strings.Contains(after.Error, "not idempotent") {
		t.Errorf("error = %q, want 提到 not idempotent"+
			"（病因要写清：这不是「连接怎么断的」而是「重放不安全」）", after.Error)
	}
	if after.FinishedAt == nil {
		t.Error("finished_at = NULL（abandoned 是终态，必须落时间戳）")
	}
	// 终态行不该再持有任何「在飞」证据；node_id 保留作溯源（与 succeeded/failed 同口径）。
	if after.InstanceID != nil {
		t.Errorf("instance_id = %q, want NULL（终态不能留着槽位引用）", *after.InstanceID)
	}
	if after.LeaseUntil != nil {
		t.Errorf("lease_until = %d, want NULL（否则 UI 会显示它还活着）", *after.LeaseUntil)
	}
	if after.Attempts != 1 {
		t.Errorf("attempts = %d, want 1（释放路径不许动重试预算）", after.Attempts)
	}
	sltAssertSlotReleased(t, db, nodeID, 1)
	// at-most-once 的全部意义：绝不出现在重派清单里
	if ids := sltRedispatchableIDs(t, db, Now()); ids[task.ID] {
		t.Fatalf("abandoned 的任务出现在 ListRedispatchable（会被二次执行）")
	}
}

// ---------- T2：attempts 用尽 + clean 结束 → abandoned ----------

// TestResolveLostTasksAbandonsWhenAttemptsExhausted 钉住 M1 的病因之二：**打穿重试预算**。
//
// 场景：能力是默认（幂等，maxAttempts 未声明 → DefaultMaxAttempts=3）；任务已被派发到
// 第 3 次（attempts=3）且真的跑过；连接 clean 关闭。修复前回 pending → 再派发一次
// attempts 就变 4，预算形同虚设（一个反复把连接搞死的任务会无限重投）。
// 判据必须排在安全例外之前：attempts 在派发成功时就 +1，否则这条永远不会收敛。
func TestResolveLostTasksAbandonsWhenAttemptsExhausted(t *testing.T) {
	db, nsID, capv := sltFixture(t, Lifecycle{}) // 幂等 + maxAttempts 取默认值
	ctx := context.Background()
	nodeID := sltNode(t, db, nsID, "mbp", 1)
	lease0 := Now() + 60
	const reason = "peer closed normally"

	if capv.Lifecycle.MaxAttemptsOrDefault() != DefaultMaxAttempts {
		t.Fatalf("前置不成立：maxAttempts = %d, want 默认 %d",
			capv.Lifecycle.MaxAttemptsOrDefault(), DefaultMaxAttempts)
	}

	task := sltTask(t, db, nsID, "trace_m1_budget", capv)
	sltDispatch(t, db, nsID, nodeID, task.ID, lease0)
	sltStart(t, db, nsID, task.ID, lease0)
	// 派发只会把 attempts 加到 1；直接推到预算边界（模拟「已被巡检/重排回收过两次」）。
	mustExec(t, db, `UPDATE tasks SET attempts = ? WHERE id = ?`, DefaultMaxAttempts, task.ID)

	before := sltGet(t, db, nsID, task.ID)
	if before.Attempts != DefaultMaxAttempts {
		t.Fatalf("前置不成立：attempts = %d, want %d", before.Attempts, DefaultMaxAttempts)
	}
	if before.Status != TaskRunning || before.StartedAt == nil {
		t.Fatalf("前置不成立：status=%q started_at=%v, want running/非空",
			before.Status, before.StartedAt)
	}

	requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID,
		[]TaskOwner{{TaskID: task.ID, Attempts: before.Attempts}},
		LostRequeue, reason, Now())
	if err != nil {
		t.Fatalf("ResolveLostTasks: %v", err)
	}
	if requeued != 0 || abandoned != 1 {
		t.Fatalf("requeued/abandoned = %d/%d, want 0/1（预算用尽必须收敛）", requeued, abandoned)
	}

	after := sltGet(t, db, nsID, task.ID)
	if after.Status != TaskAbandoned {
		t.Fatalf("status = %q, want abandoned（回 pending 会被再派发一次 → attempts=%d，M1）",
			after.Status, after.Attempts+1)
	}
	if !strings.HasPrefix(after.Error, "executor_lost:") {
		t.Errorf("error = %q, want 前缀 executor_lost:", after.Error)
	}
	if !strings.Contains(after.Error, reason) {
		t.Errorf("error = %q, 丢了结束原因 %q", after.Error, reason)
	}
	if !strings.Contains(after.Error, "attempts exhausted") {
		t.Errorf("error = %q, want 提到 attempts exhausted"+
			"（与「非幂等」是两种病因，不能混写）", after.Error)
	}
	if strings.Contains(after.Error, "not idempotent") {
		t.Errorf("error = %q, 这条能力是幂等的，不该提到 not idempotent", after.Error)
	}
	if after.FinishedAt == nil {
		t.Error("finished_at = NULL（abandoned 是终态，必须落时间戳）")
	}
	if after.Attempts != DefaultMaxAttempts {
		t.Errorf("attempts = %d, want %d（预算不能被打穿）", after.Attempts, DefaultMaxAttempts)
	}
	sltAssertSlotReleased(t, db, nodeID, 1)
	if ids := sltRedispatchableIDs(t, db, Now()); ids[task.ID] {
		t.Fatalf("预算用尽的任务出现在 ListRedispatchable（会被派到 attempts=%d）",
			after.Attempts+1)
	}
}

// ---------- T3：安全例外优先于幂等性与 policy ----------

// TestResolveLostTasksAssignedNotStartedBeatsNonIdempotent 是**保正**用例：
// 修 M1 时最容易犯的错是把「非幂等 → abandoned」提到安全例外之前，
// 那样「领了单但一行代码都没跑」的任务会被白丢（#144 的回归）。
//
// 场景：非幂等能力 + assigned + started_at IS NULL（派发成功、ack 还没发出来，
// 正是 #221 「1MiB payload 把连接打死」那个窗口）+ policy=LostAbandon。
// 两个「看起来该 abandon」的条件同时在场，结论仍必须是 pending：没开始过 = 没有副作用
// 可重复，这条证据比能力声明和 policy 都硬。
func TestResolveLostTasksAssignedNotStartedBeatsNonIdempotent(t *testing.T) {
	db, nsID, capv := sltFixture(t, sltNonIdempotent())
	ctx := context.Background()
	nodeID := sltNode(t, db, nsID, "mbp", 1)
	lease0 := Now() + 60

	task := sltTask(t, db, nsID, "trace_m1_safe_exception", capv)
	sltDispatch(t, db, nsID, nodeID, task.ID, lease0) // 只派发：assigned，无 ack/progress

	before := sltGet(t, db, nsID, task.ID)
	if before.Status != TaskAssigned || before.StartedAt != nil {
		t.Fatalf("前置不成立：status=%q started_at=%v, want assigned/NULL",
			before.Status, before.StartedAt)
	}

	// policy 故意选 LostAbandon：安全例外必须压过它，也必须压过 idempotent:false。
	requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID,
		[]TaskOwner{{TaskID: task.ID, Attempts: before.Attempts}},
		LostAbandon, "heartbeat timeout after 3 missed pings", Now())
	if err != nil {
		t.Fatalf("ResolveLostTasks: %v", err)
	}
	if requeued != 1 || abandoned != 0 {
		t.Fatalf("requeued/abandoned = %d/%d, want 1/0（未开始的安全例外无条件优先）",
			requeued, abandoned)
	}

	after := sltGet(t, db, nsID, task.ID)
	if after.Status != TaskPending {
		t.Fatalf("status = %q, want pending（从没执行过的任务不该按幂等性废弃）", after.Status)
	}
	if after.NodeID != nil || after.InstanceID != nil {
		t.Errorf("归属没清：node_id=%v instance_id=%v, want 全 NULL", after.NodeID, after.InstanceID)
	}
	if after.LeaseUntil != nil {
		t.Errorf("lease_until = %d, want NULL", *after.LeaseUntil)
	}
	if after.NotBefore != nil {
		t.Errorf("not_before = %d, want NULL（执行者已经不在了，没有退避的理由）", *after.NotBefore)
	}
	if after.Error != "" {
		t.Errorf("error = %q, want \"\"（这是重排不是放弃，不该写 executor_lost 诊断）", after.Error)
	}
	if after.FinishedAt != nil {
		t.Errorf("finished_at = %d, want NULL（pending 不是终态）", *after.FinishedAt)
	}
	if after.Attempts != 1 {
		t.Errorf("attempts = %d, want 1（重试预算不能被释放路径清零）", after.Attempts)
	}
	sltAssertSlotReleased(t, db, nodeID, 1)
	if !sltRedispatchableIDs(t, db, Now())[task.ID] {
		t.Fatal("ListRedispatchable 捞不到刚释放的任务")
	}
}

// ---------- T4：正常重排不能被误关 ----------

// TestResolveLostTasksCleanEndOfIdempotentStartedStillRequeues 是**保正**用的对照组：
// 修 M1 时的另一种过度纠正，是把「已开始」直接等同于「不许重排」，那样
// §5.2 S2 的正常路径（幂等能力 + 执行者真的没了 → 立刻重排，不等 30s 租约）就断了，
// #218 会原地复活。
//
// 场景：默认幂等能力 + running（started_at 非空）+ attempts=1（远未用尽）+ LostRequeue
// → 必须回 pending，且**立刻**可重派（not_before=NULL）：执行者已经不在了，
// 没有「打坏节点要不要退避」的问题，重试预算由 attempts/maxAttempts 兜住。
func TestResolveLostTasksCleanEndOfIdempotentStartedStillRequeues(t *testing.T) {
	db, nsID, capv := sltFixture(t, Lifecycle{}) // 幂等 + 默认 maxAttempts
	ctx := context.Background()
	nodeID := sltNode(t, db, nsID, "mbp", 1)
	lease0 := Now() + 60
	const reason = "peer closed normally"

	task := sltTask(t, db, nsID, "trace_m1_positive_control", capv)
	sltDispatch(t, db, nsID, nodeID, task.ID, lease0)
	sltStart(t, db, nsID, task.ID, lease0)

	before := sltGet(t, db, nsID, task.ID)
	if before.Status != TaskRunning || before.StartedAt == nil || before.Attempts != 1 {
		t.Fatalf("前置不成立：status=%q started_at=%v attempts=%d, want running/非空/1",
			before.Status, before.StartedAt, before.Attempts)
	}
	if before.Attempts >= capv.Lifecycle.MaxAttemptsOrDefault() {
		t.Fatalf("前置不成立：attempts=%d 已经撞到预算上界，测不到「正常重排」这条腿",
			before.Attempts)
	}

	requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID,
		[]TaskOwner{{TaskID: task.ID, Attempts: before.Attempts}},
		LostRequeue, reason, Now())
	if err != nil {
		t.Fatalf("ResolveLostTasks: %v", err)
	}
	if requeued != 1 || abandoned != 0 {
		t.Fatalf("requeued/abandoned = %d/%d, want 1/0（幂等能力 + clean 结束就是该重排）",
			requeued, abandoned)
	}

	after := sltGet(t, db, nsID, task.ID)
	if after.Status != TaskPending {
		t.Fatalf("status = %q, want pending（修 M1 不能把正常重排也关掉）", after.Status)
	}
	if after.NodeID != nil || after.InstanceID != nil {
		t.Errorf("归属没清：node_id=%v instance_id=%v, want 全 NULL", after.NodeID, after.InstanceID)
	}
	if after.LeaseUntil != nil {
		t.Errorf("lease_until = %d, want NULL", *after.LeaseUntil)
	}
	if after.NotBefore != nil {
		t.Errorf("not_before = %d, want NULL（执行者已经不在了，退避会把「立刻可重派」偷换成 10s 后）",
			*after.NotBefore)
	}
	if after.Error != "" {
		t.Errorf("error = %q, want \"\"（回 pending 不是放弃）", after.Error)
	}
	if after.FinishedAt != nil {
		t.Errorf("finished_at = %d, want NULL（pending 不是终态）", *after.FinishedAt)
	}
	if after.Attempts != 1 {
		t.Errorf("attempts = %d, want 1（释放路径不许清零预算，否则会无限重投）", after.Attempts)
	}
	if after.StartedAt == nil {
		t.Error("started_at 被清空了：它是「这条任务真跑过」的唯一留痕，重排不该擦掉")
	}
	sltAssertSlotReleased(t, db, nodeID, 1)
	if !sltRedispatchableIDs(t, db, Now())[task.ID] {
		t.Fatal("ListRedispatchable 捞不到刚释放的任务（巡检这条腿也得认它）")
	}
}
