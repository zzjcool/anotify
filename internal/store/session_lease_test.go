package store

// 会话归属 / 失联释放 / 槽位兜底 / params 闸门 的回归测试（契约 §5.2 的 S1..S7）。
//
// 这四条新能力钉住的是 #218 / #222 / #221 的**同一个根因**：续租与释放的粒度
// 错绑在 node 上，而执行体的生命周期绑在「连接（进程）」上。修复前的代码：
//
//   · 续租只有 ExtendLeasesForNode（按 node 续）→ 重启后的新连接会替上一代进程
//     遗留的孤儿任务一直续 lease_until → 巡检永远认为它活着 → 永久卡 acked、
//     槽位永久被占、pending 一条都派不出去（#218）。
//   · 连接结束只 MarkInstancesDead + offline，**不回收任务**：任务是死是活没人知道，
//     要么等 30s 租约后被当成「可重试」重派（同一个任务被两个进程并发执行 → #222），
//     要么（旧代码还被按 node 续租顶着）永远不回收。
//   · 建任务不看 params 体积：>=131072B 的任务必然 E2BIG，而且大帧会把连接打死，
//     剩下的槽位又是 #218 的一条复现路径（#221）。
//   · 中枢在「已派发、未回收」的窗口里被 kill -9 后，没人再清 busy_task（#218 兜底）。
//
// 因此 S1..S7 在修复前的代码上是**编译期红**（ExtendLeasesForTasks /
// ResolveLostTasks / ReclaimStaleSlots / CheckTaskParamsSize 均不存在），
// S6 在只加「新类型」而没加建行闸门的中间态是**运行期红**（超大 params 照建行）。
// 每条用例头部另写「这一条钉住的旧行为」。
//
// 全部断言只依赖库里的机器事实（GetTask / ListInstances / FreeSlotsByNode /
// ListRedispatchable），不看日志、不看内存状态。

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// ---------- 夹具 ----------

// slFixture 是本文件所有用例的公共夹具：一个命名空间 + 一个能力定义。
func slFixture(t *testing.T) (db *DB, nsID string, capv *CapabilityVersion) {
	t.Helper()
	db = newTestDB(t)
	nsID, _ = newTestNS(t, db)
	capv = mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(context.Background(), capv); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	return db, nsID, capv
}

// slNode 建一台「在线 + 申报了 slots 个并发槽位」的节点，返回 node_id。
func slNode(t *testing.T, db *DB, nsID, name string, slots int) string {
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

// slTask 落库一条 pending 任务。
func slTask(t *testing.T, db *DB, nsID, traceID string, capv *CapabilityVersion) *Task {
	t.Helper()
	task := mkTask(nsID, traceID, capv)
	if err := db.CreateTasks(context.Background(), []*Task{task}); err != nil {
		t.Fatalf("create task %s: %v", traceID, err)
	}
	return task
}

// slDispatch 走**生产派发路径**：占空闲槽位 + pending→assigned + attempts+1 +
// 写 node_id/instance_id/lease_until（LeaseAndAssign 一个事务）。
func slDispatch(t *testing.T, db *DB, nsID, nodeID, taskID string, leaseUntil int64) {
	t.Helper()
	if _, err := db.LeaseAndAssign(context.Background(), nsID, taskID, nodeID, leaseUntil); err != nil {
		t.Fatalf("lease and assign %s: %v", taskID, err)
	}
}

// slAck 模拟 ack 帧：assigned → acked（不写 started_at）。
func slAck(t *testing.T, db *DB, nsID, taskID string) {
	t.Helper()
	if err := db.Transition(context.Background(), nsID, taskID, TaskAssigned, TaskAcked); err != nil {
		t.Fatalf("ack %s: %v", taskID, err)
	}
}

// slStart 模拟 progress 帧：acked → running + started_at + 续租。
// 这是唯一能同时得到「running」和「started_at 非空」的真实时序。
func slStart(t *testing.T, db *DB, nsID, taskID string, leaseUntil int64) {
	t.Helper()
	if err := db.ProgressTask(context.Background(), nsID, taskID, leaseUntil); err != nil {
		t.Fatalf("progress %s: %v", taskID, err)
	}
}

// slGet 读回任务（失败即 Fatal）。
func slGet(t *testing.T, db *DB, nsID, taskID string) *Task {
	t.Helper()
	task, err := db.GetTask(context.Background(), nsID, taskID)
	if err != nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	return task
}

// slBusySlots 返回节点上 busy_task 非空的槽位（slot → taskID）。
// 槽位归属是「任务是否还在这台机器上跑」的唯一机器事实。
func slBusySlots(t *testing.T, db *DB, nodeID string) map[int]string {
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

// slFreeSlots 返回节点空闲槽位数（FreeSlotsByNode 的口径：alive 且 busy_task IS NULL）。
func slFreeSlots(t *testing.T, db *DB, nodeID string) int {
	t.Helper()
	free, err := db.FreeSlotsByNode(context.Background(), []string{nodeID})
	if err != nil {
		t.Fatalf("free slots of %s: %v", nodeID, err)
	}
	return free[nodeID]
}

// slTasksIn 返回某个 trace（"" = 不过滤）下的任务（id → task）。
func slTasksIn(t *testing.T, db *DB, nsID, traceID string) map[string]*Task {
	t.Helper()
	tasks, err := db.ListTasks(context.Background(), nsID, traceID, "", "", 0)
	if err != nil {
		t.Fatalf("list tasks (trace=%q): %v", traceID, err)
	}
	out := make(map[string]*Task, len(tasks))
	for _, task := range tasks {
		out[task.ID] = task
	}
	return out
}

// slRedispatchableIDs 返回当前「可立刻重派」的 pending 任务 ID 集合。
func slRedispatchableIDs(t *testing.T, db *DB, now int64) map[string]bool {
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

// slJSONOfSize 造一段**合法 JSON**、且长度恰好为 size 字节的 params。
func slJSONOfSize(t *testing.T, size int) json.RawMessage {
	t.Helper()
	const overhead = len(`{"k":""}`) // {"k":" + "} = 8 字节
	if size < overhead {
		t.Fatalf("slJSONOfSize(%d): size 小于最小骨架 %d", size, overhead)
	}
	raw := json.RawMessage(`{"k":"` + strings.Repeat("x", size-overhead) + `"}`)
	if len(raw) != size {
		t.Fatalf("slJSONOfSize: 造出 %d 字节，want %d", len(raw), size)
	}
	return raw
}

// slWantLease 断言租约值（消息里带上是谁，失败时不用猜）。
func slWantLease(t *testing.T, what string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: lease_until = NULL, want %d", what, want)
		return
	}
	if *got != want {
		t.Errorf("%s: lease_until = %d, want %d", what, *got, want)
	}
}

// slWantNilLease 断言租约已被清空。
func slWantNilLease(t *testing.T, what string, got *int64) {
	t.Helper()
	if got != nil {
		t.Errorf("%s: lease_until = %d, want NULL", what, *got)
	}
}

// ---------- S1 ----------

// TestExtendLeasesForTasksOnlyListedIDs：续租必须**只**覆盖「本连接名下的这批任务」。
//
// 钉住的旧行为：旧代码只有 ExtendLeasesForNode，它续的是「这台机器上所有
// assigned/acked/running 的任务」。重启/重连后新连接替上一代进程遗留的孤儿一直续命，
// 巡检永远认为它活着 → 任务永久卡 acked、槽位永久被占（#218 的病根）。
// 本用例第 ①~④ 组断言在旧代码上无法表达（ExtendLeasesForTasks 不存在 = 编译期红），
// 第 ⑤ 组则**直接演示**旧 API 会把名单外的孤儿一起续上。
func TestExtendLeasesForTasksOnlyListedIDs(t *testing.T) {
	db, nsID, capv := slFixture(t)
	ctx := context.Background()
	nodeA := slNode(t, db, nsID, "mbp-a", 5)
	nodeB := slNode(t, db, nsID, "mbp-b", 1)

	lease0 := Now() + 60    // 派发时写进去的租约（"上一轮"的值）
	leaseNew := Now() + 600 // 本连接心跳续租用的新值
	leaseDup := Now() + 900 // 重复 ID 去重用例的值
	leaseOldAPI := Now() + 1800

	// 三条「本连接名下的在飞任务」，状态各不相同：assigned / acked / running 都必须能续。
	// 契约 §2 的 WHERE 里 status IN (assigned,acked,running)，缺任一状态就会出现
	// 「ack 之前 / progress 之后租约不续 → 巡检提前回收」的窗口。
	tAcked := slTask(t, db, nsID, "trace_s1_acked", capv)
	slDispatch(t, db, nsID, nodeA, tAcked.ID, lease0)
	slAck(t, db, nsID, tAcked.ID)

	tRunning := slTask(t, db, nsID, "trace_s1_running", capv)
	slDispatch(t, db, nsID, nodeA, tRunning.ID, lease0)
	slAck(t, db, nsID, tRunning.ID)
	slStart(t, db, nsID, tRunning.ID, lease0)

	tAssigned := slTask(t, db, nsID, "trace_s1_assigned", capv)
	slDispatch(t, db, nsID, nodeA, tAssigned.ID, lease0)

	// 上一代连接的孤儿：仍在 node A、仍是 acked，但**不在**本连接的名单里。
	// 这就是 #218 的病根现场。
	tOrphan := slTask(t, db, nsID, "trace_s1_orphan", capv)
	slDispatch(t, db, nsID, nodeA, tOrphan.ID, lease0)
	slAck(t, db, nsID, tOrphan.ID)

	// 终态任务：续租只认 assigned/acked/running，终态一个都不碰。
	tDone := slTask(t, db, nsID, "trace_s1_done", capv)
	slDispatch(t, db, nsID, nodeA, tDone.ID, lease0)
	slAck(t, db, nsID, tDone.ID)
	slStart(t, db, nsID, tDone.ID, lease0)
	if err := db.Transition(ctx, nsID, tDone.ID, TaskRunning, TaskSucceeded); err != nil {
		t.Fatalf("finish %s: %v", tDone.ID, err)
	}

	// 别的节点的在飞任务：即使 ID 被误列进名单，node_id 条件也必须挡住它
	// （同一 taskID 可能已被重派到别的节点，替它续租就是替别人续命）。
	tOtherNode := slTask(t, db, nsID, "trace_s1_other_node", capv)
	slDispatch(t, db, nsID, nodeB, tOtherNode.ID, lease0)
	slAck(t, db, nsID, tOtherNode.ID)

	t.Run("空名单是 no-op", func(t *testing.T) {
		for _, empty := range [][]string{nil, {}} {
			n, err := db.ExtendLeasesForTasks(ctx, nsID, nodeA, empty, leaseNew)
			if err != nil {
				t.Fatalf("empty taskIDs: unexpected err %v", err)
			}
			if n != 0 {
				t.Fatalf("empty taskIDs: renewed %d rows, want 0", n)
			}
		}
		// 空名单不许有任何副作用
		for _, id := range []string{tAcked.ID, tRunning.ID, tAssigned.ID, tOrphan.ID} {
			slWantLease(t, "空名单后 "+id, slGet(t, db, nsID, id).LeaseUntil, lease0)
		}
	})

	t.Run("三条在飞只续两条，第三条不动", func(t *testing.T) {
		// 契约 §5.2 S1 的原话：三条在飞任务，只续其中两条 → 只有那两条变。
		// 这一条钉住「名单是唯一权威」：实现不许顺手把节点上其余在飞任务一起续了
		// （那正是按 node 续租的语义）。
		partial := Now() + 400
		n, err := db.ExtendLeasesForTasks(ctx, nsID, nodeA, []string{tAcked.ID, tRunning.ID}, partial)
		if err != nil {
			t.Fatalf("ExtendLeasesForTasks(2/3): %v", err)
		}
		if n != 2 {
			t.Fatalf("renewed %d rows, want 2", n)
		}
		slWantLease(t, "名单里的 acked", slGet(t, db, nsID, tAcked.ID).LeaseUntil, partial)
		slWantLease(t, "名单里的 running", slGet(t, db, nsID, tRunning.ID).LeaseUntil, partial)
		for _, u := range []struct{ what, id string }{
			{"同在名单节点的第三条在飞任务(assigned，未列入)", tAssigned.ID},
			{"名单外的孤儿任务(acked, 同节点)", tOrphan.ID},
			{"别的节点的任务(acked)", tOtherNode.ID},
			{"终态任务(succeeded)", tDone.ID},
		} {
			slWantLease(t, u.what, slGet(t, db, nsID, u.id).LeaseUntil, lease0)
		}
	})

	t.Run("只续名单里的三条", func(t *testing.T) {
		listed := []string{tAcked.ID, tRunning.ID, tAssigned.ID}
		n, err := db.ExtendLeasesForTasks(ctx, nsID, nodeA, listed, leaseNew)
		if err != nil {
			t.Fatalf("ExtendLeasesForTasks: %v", err)
		}
		if n != len(listed) {
			t.Fatalf("renewed %d rows, want %d", n, len(listed))
		}
		for _, id := range listed {
			slWantLease(t, "名单里的 "+id, slGet(t, db, nsID, id).LeaseUntil, leaseNew)
		}
		// 名单外的：孤儿（同节点同状态）/ 别的节点 / 终态 —— 一个都不许动
		untouched := []struct{ what, id string }{
			{"名单外的孤儿任务(acked, 同节点)", tOrphan.ID},
			{"别的节点的任务(acked)", tOtherNode.ID},
			{"终态任务(succeeded)", tDone.ID},
		}
		for _, u := range untouched {
			slWantLease(t, u.what, slGet(t, db, nsID, u.id).LeaseUntil, lease0)
		}
	})

	t.Run("不属于本节点或已终态的 ID 不算错误", func(t *testing.T) {
		n, err := db.ExtendLeasesForTasks(ctx, nsID, nodeA, []string{tOtherNode.ID, tDone.ID, ""}, leaseNew)
		if err != nil {
			t.Fatalf("ExtendLeasesForTasks: %v", err)
		}
		if n != 0 {
			t.Fatalf("renewed %d rows for foreign/terminal ids, want 0", n)
		}
		slWantLease(t, "别的节点的任务", slGet(t, db, nsID, tOtherNode.ID).LeaseUntil, lease0)
		slWantLease(t, "终态任务", slGet(t, db, nsID, tDone.ID).LeaseUntil, lease0)
	})

	t.Run("重复 ID 只算一行", func(t *testing.T) {
		dup := []string{tAcked.ID, tAcked.ID, tAcked.ID, tRunning.ID, tAssigned.ID}
		n, err := db.ExtendLeasesForTasks(ctx, nsID, nodeA, dup, leaseDup)
		if err != nil {
			t.Fatalf("ExtendLeasesForTasks(dup): %v", err)
		}
		if n != 3 {
			t.Fatalf("renewed %d rows for 5 ids (3 distinct), want 3: 去重没生效会把"+
				"「本连接名下几条任务」算错", n)
		}
		for _, id := range []string{tAcked.ID, tRunning.ID, tAssigned.ID} {
			slWantLease(t, "去重用例的 "+id, slGet(t, db, nsID, id).LeaseUntil, leaseDup)
		}
	})

	t.Run("对照：旧 API 按 node 续会替孤儿续命", func(t *testing.T) {
		// 同一个节点、同一批任务，用**旧的** ExtendLeasesForNode 走一遍：
		// 它把名单之外的孤儿也续上了 → 这正是 #218 里「巡检永远不回收」的机制。
		// 这条断言是「旧代码为什么红」的现场证据（新生产路径已不再调它）。
		n, err := db.ExtendLeasesForNode(ctx, nsID, nodeA, leaseOldAPI)
		if err != nil {
			t.Fatalf("ExtendLeasesForNode: %v", err)
		}
		if n != 4 {
			t.Fatalf("ExtendLeasesForNode renewed %d rows, want 4 "+
				"(acked/running/assigned + 孤儿；终态不算)", n)
		}
		slWantLease(t, "旧 API 下的孤儿任务", slGet(t, db, nsID, tOrphan.ID).LeaseUntil, leaseOldAPI)
		slWantLease(t, "旧 API 下的终态任务", slGet(t, db, nsID, tDone.ID).LeaseUntil, lease0)
	})
}

// ---------- S2 ----------

// TestResolveLostTasksRequeue：执行者**确定已消失**（对端正常关闭）→ 立刻回 pending，
// 不等 30s 租约、不留槽位钉子。
//
// 钉住的旧行为：旧 cleanup 只 MarkInstancesDead + offline，任务停在 acked、
// 槽位继续被占 → 重启后这台机器再也接不到活（#218）。
func TestResolveLostTasksRequeue(t *testing.T) {
	db, nsID, capv := slFixture(t)
	ctx := context.Background()
	nodeID := slNode(t, db, nsID, "mbp", 1)
	lease0 := Now() + 60

	task := slTask(t, db, nsID, "trace_s2", capv)
	slDispatch(t, db, nsID, nodeID, task.ID, lease0)
	slAck(t, db, nsID, task.ID)
	slStart(t, db, nsID, task.ID, lease0) // running + started_at

	before := slGet(t, db, nsID, task.ID)
	if before.Status != TaskRunning {
		t.Fatalf("前置不成立：status = %q, want running", before.Status)
	}
	if before.StartedAt == nil {
		t.Fatal("前置不成立：started_at 为 NULL（这条任务没真跑过，走不到 policy 分支）")
	}
	if before.Attempts != 1 {
		t.Fatalf("前置不成立：attempts = %d, want 1", before.Attempts)
	}
	if busy := slBusySlots(t, db, nodeID); len(busy) != 1 {
		t.Fatalf("前置不成立：占用的槽位数 = %d, want 1", len(busy))
	}

	// 契约 §5.2 S2 写的是「acked+started」；这里用真实时序的最接近等价态
	// （progress 帧后 running + started_at 非空）。agent 的分支只区分
	// 「started_at 是否为空」与「是否 assigned」，running/acked 走同一条 policy 分支。
	owners := []TaskOwner{{TaskID: task.ID, Attempts: before.Attempts}}
	requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID, owners,
		LostRequeue, "peer closed normally", Now())
	if err != nil {
		t.Fatalf("ResolveLostTasks: %v", err)
	}
	if requeued != 1 || abandoned != 0 {
		t.Fatalf("Requoted/abandoned = %d/%d, want 1/0", requeued, abandoned)
	}

	after := slGet(t, db, nsID, task.ID)
	if after.Status != TaskPending {
		t.Fatalf("status = %q, want pending（执行者已消失，必须立刻重排）", after.Status)
	}
	if after.NodeID != nil {
		t.Errorf("node_id = %q, want NULL", *after.NodeID)
	}
	if after.InstanceID != nil {
		t.Errorf("instance_id = %q, want NULL", *after.InstanceID)
	}
	slWantNilLease(t, "重排后的任务", after.LeaseUntil)
	if after.NotBefore != nil {
		t.Errorf("not_before = %d, want NULL（执行者已经不在了，没有退避的理由；"+
			"退避会把「立刻可重派」偷换成 10s 后）", *after.NotBefore)
	}
	if after.Attempts != 1 {
		t.Errorf("attempts = %d, want 1（重试预算不能被释放路径清零，否则会无限重投）", after.Attempts)
	}
	if after.StartedAt == nil {
		t.Error("started_at 被清空了：它是「这条任务真跑过」的唯一留痕，重排不该擦掉")
	}
	if after.Error != "" {
		t.Errorf("error = %q, want \"\"（回 pending 不是放弃，不该写诊断）", after.Error)
	}

	// 槽位必须同时放掉：只改任务状态不清槽位，等于把 #218 的钉子留在原地。
	if busy := slBusySlots(t, db, nodeID); len(busy) != 0 {
		t.Fatalf("槽位仍被占：%v", busy)
	}
	if free := slFreeSlots(t, db, nodeID); free != 1 {
		t.Fatalf("free slots = %d, want 1", free)
	}
	// 巡检这条腿必须也认它（否则要等下一轮 30s 才会重派）
	if !slRedispatchableIDs(t, db, Now())[task.ID] {
		t.Fatal("ListRedispatchable 捞不到刚释放的任务")
	}
}

// ---------- S3 ----------

// TestResolveLostTasksAbandon：执行者**可能还活着**（心跳超时/被顶掉/帧超限）→
// abandoned + `executor_lost:` 诊断，**绝不重派**（#222 at-most-once）。
//
// 钉住的旧行为：旧代码把这类任务留在 acked，等 30s 租约一过就被巡检当「可重试」
// 重派 → 同一个任务被两个进程并发执行两次（#222 的直接复现）。
func TestResolveLostTasksAbandon(t *testing.T) {
	db, nsID, capv := slFixture(t)
	ctx := context.Background()
	nodeID := slNode(t, db, nsID, "mbp", 2)
	const reason = "heartbeat timeout after 3 missed pings"

	// ① 已经真的跑过（running + started_at 非空）：重放不安全，必须 abandoned。
	tRunning := slTask(t, db, nsID, "trace_s3_running", capv)
	slDispatch(t, db, nsID, nodeID, tRunning.ID, Now()+60)
	slAck(t, db, nsID, tRunning.ID)
	slStart(t, db, nsID, tRunning.ID, Now()+60)

	// ② acked 但 started_at IS NULL：安全例外只覆盖 `assigned && started_at IS NULL`，
	//    所以这条也必须 abandoned（把安全例外放宽到 acked，就等于允许「接单后假死」被重派）。
	tAcked := slTask(t, db, nsID, "trace_s3_acked", capv)
	slDispatch(t, db, nsID, nodeID, tAcked.ID, Now()+60)
	slAck(t, db, nsID, tAcked.ID)

	for _, id := range []string{tRunning.ID, tAcked.ID} {
		if got := slGet(t, db, nsID, id); got.Attempts != 1 {
			t.Fatalf("前置不成立：%s attempts = %d, want 1", id, got.Attempts)
		}
	}
	if slGet(t, db, nsID, tAcked.ID).StartedAt != nil {
		t.Fatal("前置不成立：acked 那条不该有 started_at")
	}

	owners := []TaskOwner{
		{TaskID: tRunning.ID, Attempts: 1},
		{TaskID: tAcked.ID, Attempts: 1},
	}
	requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID, owners,
		LostAbandon, reason, Now())
	if err != nil {
		t.Fatalf("ResolveLostTasks: %v", err)
	}
	if requeued != 0 || abandoned != len(owners) {
		t.Fatalf("requeued/abandoned = %d/%d, want 0/%d", requeued, abandoned, len(owners))
	}

	for what, id := range map[string]string{"running": tRunning.ID, "acked": tAcked.ID} {
		got := slGet(t, db, nsID, id)
		if got.Status != TaskAbandoned {
			t.Errorf("%s: status = %q, want abandoned", what, got.Status)
		}
		if !strings.HasPrefix(got.Error, "executor_lost:") {
			t.Errorf("%s: error = %q, want 前缀 executor_lost:", what, got.Error)
		}
		if !strings.Contains(got.Error, reason) {
			t.Errorf("%s: error = %q, 丢了结束原因 %q（运维没法诊断）", what, got.Error, reason)
		}
		if got.FinishedAt == nil {
			t.Errorf("%s: finished_at = NULL（abandoned 是终态，必须落时间戳）", what)
		}
	}

	// ① 槽位必须放掉：这是 #218 的另一半（任务放弃了但槽位还钉着 = 机器少一个槽）
	if busy := slBusySlots(t, db, nodeID); len(busy) != 0 {
		t.Fatalf("槽位仍被占：%v", busy)
	}
	if free := slFreeSlots(t, db, nodeID); free != 2 {
		t.Fatalf("free slots = %d, want 2", free)
	}
	// ② 绝不能出现在重派清单里：at-most-once 的全部意义就在这里
	ids := slRedispatchableIDs(t, db, Now())
	for _, id := range []string{tRunning.ID, tAcked.ID} {
		if ids[id] {
			t.Fatalf("abandoned 的任务 %s 出现在 ListRedispatchable（会被二次执行）", id)
		}
	}
}

// ---------- S4 ----------

// TestResolveLostTasksSkipsStaleGeneration：owners 里的世代（attempts）与行里不一致时，
// 一个字段都不许动。
//
// 钉住的旧行为：旧代码没有「连接名下的任务」这个概念，任何清理动作都只能按
// node 或按过期时间做，于是会**误伤已被重派的任务**（把新执行者手上的活踢回 pending，
// 造成同一任务两个执行者）。
func TestResolveLostTasksSkipsStaleGeneration(t *testing.T) {
	db, nsID, capv := slFixture(t)
	ctx := context.Background()
	nodeID := slNode(t, db, nsID, "mbp", 2)
	lease0 := Now() + 60

	task := slTask(t, db, nsID, "trace_s4", capv)
	slDispatch(t, db, nsID, nodeID, task.ID, lease0)
	slAck(t, db, nsID, task.ID)

	assertUntouched := func(t *testing.T, why string) {
		t.Helper()
		got := slGet(t, db, nsID, task.ID)
		if got.Status != TaskAcked {
			t.Errorf("%s: status = %q, want acked（一行都不该动）", why, got.Status)
		}
		if got.Attempts != 1 {
			t.Errorf("%s: attempts = %d, want 1", why, got.Attempts)
		}
		if got.NodeID == nil || *got.NodeID != nodeID {
			t.Errorf("%s: node_id = %v, want %s", why, got.NodeID, nodeID)
		}
		slWantLease(t, why+" 的租约", got.LeaseUntil, lease0)
		if got.FinishedAt != nil {
			t.Errorf("%s: finished_at 被写了 %d", why, *got.FinishedAt)
		}
		if got.Error != "" {
			t.Errorf("%s: error = %q, want \"\"", why, got.Error)
		}
		busy := slBusySlots(t, db, nodeID)
		if busy[0] != task.ID {
			t.Errorf("%s: 槽位 0 = %v, want 仍挂着 %s", why, busy[0], task.ID)
		}
	}

	// ① 世代过期：库里 attempts=1，我手里还记着 0 或 2 → 说明这条任务在我"不在"的
	//    时候被巡检回收并重派过，它的现状不归我改（否则会踢掉新执行者）。
	for _, staleGen := range []int{0, 2} {
		requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID,
			[]TaskOwner{{TaskID: task.ID, Attempts: staleGen}}, LostAbandon, "stale generation", Now())
		if err != nil {
			t.Fatalf("attempts=%d: unexpected err %v", staleGen, err)
		}
		if requeued != 0 || abandoned != 0 {
			t.Fatalf("attempts=%d: requeued/abandoned = %d/%d, want 0/0（世代不符必须整体跳过）",
				staleGen, requeued, abandoned)
		}
		assertUntouched(t, "世代过期(attempts 记成 "+strconv.Itoa(staleGen)+")")
	}

	// ② 世代对、但它不在我名下（node 不同）→ 同样一行不动。
	//    同一 taskID 已被重派到别的节点时，这条路径绝不能把它改回去。
	{
		requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, "nd_ghost",
			[]TaskOwner{{TaskID: task.ID, Attempts: 1}}, LostRequeue, "wrong node", Now())
		if err != nil {
			t.Fatalf("wrong node: unexpected err %v", err)
		}
		if requeued != 0 || abandoned != 0 {
			t.Fatalf("wrong node: requeued/abandoned = %d/%d, want 0/0", requeued, abandoned)
		}
		assertUntouched(t, "不属于本节点")
	}

	// ③ 对照：世代与归属都对 → 真的处理。证明上面的 0/0 是「按条件跳过」而不是
	//    「函数压根不干活」。
	requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID,
		[]TaskOwner{{TaskID: task.ID, Attempts: 1}}, LostAbandon, "superseded", Now())
	if err != nil {
		t.Fatalf("control: unexpected err %v", err)
	}
	if requeued != 0 || abandoned != 1 {
		t.Fatalf("control: requeued/abandoned = %d/%d, want 0/1", requeued, abandoned)
	}
	if got := slGet(t, db, nsID, task.ID); got.Status != TaskAbandoned {
		t.Fatalf("control: status = %q, want abandoned", got.Status)
	}

	// ④ 幂等：同一条连接被 cleanup 两次（或旧连接＋新连接的 cleanup 撞车）时，
	//    已经是终态的任务不该再被改——「已处理过」必须由状态机自己挡住。
	{
		requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID,
			[]TaskOwner{{TaskID: task.ID, Attempts: 1}}, LostRequeue, "second cleanup", Now())
		if err != nil {
			t.Fatalf("repeat: unexpected err %v", err)
		}
		if requeued != 0 || abandoned != 0 {
			t.Fatalf("repeat: requeued/abandoned = %d/%d, want 0/0（终态不归本路径管）", requeued, abandoned)
		}
		if got := slGet(t, db, nsID, task.ID); got.Status != TaskAbandoned ||
			!strings.HasPrefix(got.Error, "executor_lost:") || strings.Contains(got.Error, "second cleanup") {
			t.Fatalf("repeat: 终态任务被二次改写了 status=%q error=%q", got.Status, got.Error)
		}
	}
}

// ---------- S5 ----------

// TestResolveLostTasksAssignedNotStartedRequeuesEvenWhenAbandon：领了单但一行没跑
// （assigned 且 started_at IS NULL）→ **不看 policy** 直接回 pending。
//
// 钉住的旧行为：旧代码只把这类任务留在 assigned 等租约过；而 #221 里
// 「1MiB payload 把连接打死」正好落在这个窗口（派发成功、ack 没发出来），
// 于是任务卡 assigned、槽位钉死、pending 一条派不出去。
// 安全例外的依据：没开始执行 = 没有副作用可重复，重排绝对安全。
func TestResolveLostTasksAssignedNotStartedRequeuesEvenWhenAbandon(t *testing.T) {
	db, nsID, capv := slFixture(t)
	ctx := context.Background()
	nodeID := slNode(t, db, nsID, "mbp", 1)
	lease0 := Now() + 60

	task := slTask(t, db, nsID, "trace_s5", capv)
	slDispatch(t, db, nsID, nodeID, task.ID, lease0) // 只派发：assigned，没有 ack/progress

	before := slGet(t, db, nsID, task.ID)
	if before.Status != TaskAssigned || before.StartedAt != nil {
		t.Fatalf("前置不成立：status = %q, started_at = %v, want assigned / NULL",
			before.Status, before.StartedAt)
	}

	// policy 故意选 LostAbandon：例外必须压过它。
	requeued, abandoned, err := db.ResolveLostTasks(ctx, nsID, nodeID,
		[]TaskOwner{{TaskID: task.ID, Attempts: before.Attempts}},
		LostAbandon, "frame too large: 1048577 > 1048576 bytes", Now())
	if err != nil {
		t.Fatalf("ResolveLostTasks: %v", err)
	}
	if requeued != 1 || abandoned != 0 {
		t.Fatalf("requeued/abandoned = %d/%d, want 1/0（assigned+未开始 必须无条件重排）",
			requeued, abandoned)
	}

	after := slGet(t, db, nsID, task.ID)
	if after.Status != TaskPending {
		t.Fatalf("status = %q, want pending", after.Status)
	}
	if after.NodeID != nil || after.InstanceID != nil {
		t.Errorf("归属没清：node_id=%v instance_id=%v, want 全 NULL", after.NodeID, after.InstanceID)
	}
	slWantNilLease(t, "重排后的任务", after.LeaseUntil)
	if after.NotBefore != nil {
		t.Errorf("not_before = %d, want NULL", *after.NotBefore)
	}
	if after.Error != "" {
		t.Errorf("error = %q, want \"\"（这是重排不是放弃，不该带 executor_lost 诊断）", after.Error)
	}
	if busy := slBusySlots(t, db, nodeID); len(busy) != 0 {
		t.Fatalf("槽位仍被占：%v", busy)
	}
	if !slRedispatchableIDs(t, db, Now())[task.ID] {
		t.Fatal("ListRedispatchable 捞不到刚释放的任务")
	}
}

// ---------- S6 ----------

// TestCreateTasksRejectsOversizedParams：params 超过 120KiB 的任务在建行时就被拒，
// 且是「整批一条都不建」。
//
// 钉住的旧行为：旧 createTasksTx 不看体积，>=131072B 的任务照建，
// 然后节点 fork/exec 必然 E2BIG、或大帧把连接打死（连接死 → 槽位钉死 = #218 复现路径）。
// 也就是说：旧代码里这条用例的第 ① 组断言（CreateTasks 报错）拿到的是 nil。
func TestCreateTasksRejectsOversizedParams(t *testing.T) {
	db, nsID, capv := slFixture(t)
	ctx := context.Background()

	t.Run("超限 1 字节被拒", func(t *testing.T) {
		over := mkTask(nsID, "trace_s6_over", capv)
		over.ID = NewTaskID()
		over.Params = slJSONOfSize(t, MaxTaskParamsBytes+1)

		err := db.CreateTasks(ctx, []*Task{over})
		if err == nil {
			t.Fatalf("CreateTasks 接受了 %d 字节的 params（limit %d）——建行闸门没生效",
				len(over.Params), MaxTaskParamsBytes)
		}
		if !errors.Is(err, ErrParamsTooLarge) {
			t.Fatalf("errors.Is(err, ErrParamsTooLarge) = false, err = %v", err)
		}
		var ple *ParamsTooLargeError
		if !errors.As(err, &ple) {
			t.Fatalf("errors.As 到 *ParamsTooLargeError 失败, err = %v", err)
		}
		if ple.Size != MaxTaskParamsBytes+1 {
			t.Errorf("Size = %d, want %d", ple.Size, MaxTaskParamsBytes+1)
		}
		if ple.Limit != MaxTaskParamsBytes {
			t.Errorf("Limit = %d, want %d", ple.Limit, MaxTaskParamsBytes)
		}
		if ple.TaskID != over.ID {
			t.Errorf("TaskID = %q, want %q（诊断要指得出是哪条任务）", ple.TaskID, over.ID)
		}
		// 文案要能被运维读懂（体积 + 上限 + 为什么）
		if !strings.Contains(ple.Error(), "131072") && !strings.Contains(ple.Error(), "limit") {
			t.Errorf("Error() 没有可诊断信息: %q", ple.Error())
		}
		// 一条行都不许落库
		if ids := slTasksIn(t, db, nsID, ""); len(ids) != 0 {
			t.Fatalf("超限任务仍然落库了：%v", ids)
		}
	})

	t.Run("整批原子回滚", func(t *testing.T) {
		// 一条合格 + 一条超限混在同一批：整批一条都不建（createTasksTx 一个事务）。
		// 只回滚超限那条，会让调用方以为扇出成功、实际少了边。
		good := mkTask(nsID, "trace_s6_batch_good", capv)
		bad := mkTask(nsID, "trace_s6_batch_bad", capv)
		bad.Params = slJSONOfSize(t, MaxTaskParamsBytes+1)

		err := db.CreateTasks(ctx, []*Task{good, bad})
		if !errors.Is(err, ErrParamsTooLarge) {
			t.Fatalf("errors.Is(err, ErrParamsTooLarge) = false, err = %v", err)
		}
		if ids := slTasksIn(t, db, nsID, ""); len(ids) != 0 {
			t.Fatalf("事务没有整体回滚，落库了：%v", ids)
		}
	})

	t.Run("正好到上限通过并往返一致", func(t *testing.T) {
		exact := mkTask(nsID, "trace_s6_exact", capv)
		exact.Params = slJSONOfSize(t, MaxTaskParamsBytes)
		if err := db.CreateTasks(ctx, []*Task{exact}); err != nil {
			t.Fatalf("CreateTasks(%d 字节) 被拒: %v", MaxTaskParamsBytes, err)
		}
		got := slGet(t, db, nsID, exact.ID)
		if len(got.Params) != MaxTaskParamsBytes {
			t.Fatalf("往返后 params 长度 = %d, want %d", len(got.Params), MaxTaskParamsBytes)
		}
		if ids := slTasksIn(t, db, nsID, "trace_s6_exact"); len(ids) != 1 || ids[exact.ID] == nil {
			t.Fatalf("上限内的任务没落库：%v", ids)
		}
	})
}

// ---------- S7 ----------

// TestReclaimStaleSlots：只清「槽位指向的任务已经不在这台机器上跑了」的钉子。
//
// 钉住的旧行为：旧代码在「已派发、未回收」的窗口里被 kill -9（或从老版本升级上来）后，
// 没人再清 busy_task → 节点显示 online 却永远接不到活（#218 的兜底面）。
// 危险方向同样要钉住：仍在 assigned/acked/running 且属于本节点的槽位**一个都不许清**，
// 否则会把正在跑的任务的槽位让给别人 → 双执行。
func TestReclaimStaleSlots(t *testing.T) {
	db, nsID, capv := slFixture(t)
	ctx := context.Background()
	nodeID := slNode(t, db, nsID, "mbp", 3)
	lease0 := Now() + 60

	// (a) 槽位 0：指着**终态**任务。真实来源：旧 ws handler 把「结果落库」和
	//     「释放槽位」分成两步，中间被 kill -9 就留下这个钉子。
	tTerm := slTask(t, db, nsID, "trace_s7_terminal", capv)
	slDispatch(t, db, nsID, nodeID, tTerm.ID, lease0) // → 槽位 0
	slAck(t, db, nsID, tTerm.ID)
	slStart(t, db, nsID, tTerm.ID, lease0)
	if err := db.Transition(ctx, nsID, tTerm.ID, TaskRunning, TaskSucceeded); err != nil {
		t.Fatalf("finish %s: %v", tTerm.ID, err)
	}

	// (b) 槽位 1：指着**已回 pending** 的任务（等重派，却还占着槽位）。
	//     正常路径里「改状态 + 放槽位」同事务，这种半成品只能来自崩溃/老版本升级，
	//     所以夹具直接改库造它——这正是 ReclaimStaleSlots 存在的唯一理由。
	tRequeued := slTask(t, db, nsID, "trace_s7_requeued", capv)
	slDispatch(t, db, nsID, nodeID, tRequeued.ID, lease0) // → 槽位 1
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, node_id = NULL, instance_id = NULL, lease_until = NULL
		  WHERE namespace_id = ? AND id = ?`, TaskPending, nsID, tRequeued.ID); err != nil {
		t.Fatalf("构造「已回 pending 但槽位未放」的遗留状态: %v", err)
	}

	// (c) 槽位 2：本节点仍在 acked 的**在飞**任务 → 必须原样保留。
	tLive := slTask(t, db, nsID, "trace_s7_live", capv)
	slDispatch(t, db, nsID, nodeID, tLive.ID, lease0) // → 槽位 2
	slAck(t, db, nsID, tLive.ID)

	// 前置：三个槽位分别挂三条任务，一条都不空
	before := slBusySlots(t, db, nodeID)
	want := map[int]string{0: tTerm.ID, 1: tRequeued.ID, 2: tLive.ID}
	for slot, id := range want {
		if before[slot] != id {
			t.Fatalf("前置不成立：槽位 %d = %q, want %q（全 busy=%v）", slot, before[slot], id, before)
		}
	}
	if free := slFreeSlots(t, db, nodeID); free != 0 {
		t.Fatalf("前置不成立：free slots = %d, want 0", free)
	}

	n, err := db.ReclaimStaleSlots(ctx, nsID, nodeID, Now())
	if err != nil {
		t.Fatalf("ReclaimStaleSlots: %v", err)
	}
	if n != 2 {
		t.Fatalf("cleared %d slots, want 2（终态 + 已回 pending；在飞的那条不算）", n)
	}

	after := slBusySlots(t, db, nodeID)
	if _, ok := after[0]; ok {
		t.Errorf("槽位 0 仍指着终态任务 %s，没清掉", tTerm.ID)
	}
	if _, ok := after[1]; ok {
		t.Errorf("槽位 1 仍指着已回 pending 的任务 %s，没清掉", tRequeued.ID)
	}
	if after[2] != tLive.ID {
		t.Fatalf("槽位 2 = %q, want %q（仍在 acked 的在飞任务被误清 → 会让别人抢同一任务）",
			after[2], tLive.ID)
	}
	if free := slFreeSlots(t, db, nodeID); free != 2 {
		t.Fatalf("free slots = %d, want 2", free)
	}

	// 这条修复只能动槽位：任务行本身一个字段都不许改。
	if got := slGet(t, db, nsID, tLive.ID); got.Status != TaskAcked ||
		got.NodeID == nil || *got.NodeID != nodeID ||
		got.LeaseUntil == nil || *got.LeaseUntil != lease0 {
		t.Errorf("在飞任务被动了：status=%q node_id=%v lease=%v", got.Status, got.NodeID, got.LeaseUntil)
	}
	if got := slGet(t, db, nsID, tTerm.ID); got.Status != TaskSucceeded {
		t.Errorf("终态任务状态被改了：%q", got.Status)
	}
	if got := slGet(t, db, nsID, tRequeued.ID); got.Status != TaskPending {
		t.Errorf("已回 pending 的任务状态被改了：%q", got.Status)
	}

	// 幂等：每次 hello 都会调它，第二次必须报 0 并就手。
	n, err = db.ReclaimStaleSlots(ctx, nsID, nodeID, Now())
	if err != nil {
		t.Fatalf("ReclaimStaleSlots(2nd): %v", err)
	}
	if n != 0 {
		t.Fatalf("第二次 cleared %d slots, want 0（不幂等的话每次 hello 都会多算）", n)
	}
	if again := slBusySlots(t, db, nodeID); again[2] != tLive.ID || len(again) != 1 {
		t.Fatalf("第二次调用后槽位 = %v, want 只有槽位 2 → %s", again, tLive.ID)
	}
}
