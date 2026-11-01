package ws

// #218 / #222 的 ws 侧回归测试：会话归属（续租/释放的粒度是「连接名下的任务」）、
// 结束原因判定（clean → 重排 / unclean → 放弃）、以及「同一任务只许一条连接执行」的护栏。
//
// 断言尽量看**库里的机器事实**（GetTask / ListInstances / ListRedispatchable），
// 而不是日志——日志会随文案漂移，库里的行不会。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- W1（#218 根修）：心跳只续本连接名下的任务 ----------

// TestHeartbeatRenewsOnlyOwnedTasks 心跳只续「这条连接派出去、还没结束」的任务。
//
// 旧代码的心脏病就在这：handleHeartbeat 调的是 ExtendLeasesForNode（按 **node** 批续），
// 于是重启后的新连接会替上一代进程遗留的孤儿任务一直续 lease_until → 巡检永远认为它们
// 还活着 → 任务永久卡 acked、槽位永久被占、pending 一条都派不出去（#218）。
//
// 判别点：孤儿行的 lease_until **一个字节都不许动**。
func TestHeartbeatRenewsOnlyOwnedTasks(t *testing.T) {
	f := newNodeFixture(t)
	f.handler.HeartbeatSec = 1
	f.handler.LeaseTTLSec = 5 // 与默认的 30 区分开：泄漏到孤儿行上一眼可见
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}})

	// orphan：库里已 assigned 到本节点，但**不属于任何活着的连接**
	// （上一代进程遗留：连接没了，行还停在 assigned/acked，busy_task 也还钉着）。
	orphan := f.makeAssignedTask(t, "trace_orphan")
	// owned：真的派给这条连接并 ack 过（归属已经登记）。
	owned := f.makeAssignedTask(t, "trace_owned")
	publishTo(t, f, f.node.ID, owned.ID)
	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: owned.ID})
	waitStatus(t, f.db, f.nsID, owned.ID, store.TaskAcked)
	// 等 ack 自己的那次续租落库，再把两条的租约一起拨到同一个 stale 值：
	// 这样「谁被心跳续了」只可能来自心跳本身。
	waitLeaseAtLeast(t, f.db, f.nsID, owned.ID, store.Now()+int64(f.handler.LeaseTTLSec)-5)
	stale := store.Now() - 1000
	for _, id := range []string{orphan.ID, owned.ID} {
		if _, err := f.db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, stale, id); err != nil {
			t.Fatalf("prime lease of %s: %v", id, err)
		}
	}

	send(t, conn, &HeartbeatFrame{Type: NodeFrameHeartbeat, TS: time.Now().Unix()})

	// owned 被续到 now+TTL
	want := store.Now() + int64(f.handler.LeaseTTLSec)
	got := waitLeaseAtLeast(t, f.db, f.nsID, owned.ID, want-5)
	if got > want+5 {
		t.Fatalf("owned task lease_until = %d, want ≈ %d (now+TTL)", got, want)
	}

	// orphan 一动不动：心跳没有按节点扫描（旧行为会把这一行也推上去）
	orphanGot := mustGetTask(t, f, orphan.ID)
	if orphanGot.LeaseUntil == nil || *orphanGot.LeaseUntil != stale {
		t.Fatalf("orphan lease_until = %v, want unchanged %d: heartbeat must NOT renew tasks "+
			"this connection does not own (node-wide renewal is the #218 root cause)",
			orphanGot.LeaseUntil, stale)
	}
}

// ---------- W2（#222）：unclean 结束 → 已开始的任务放弃，绝不重派 ----------

// TestUncleanEndAbandonsStartedTask 心跳超时（= 我们主动掐、对端进程可能还活着）
// → 任务 abandoned + executor_lost，槽位释放，且**不可能被重派**。
//
// 旧代码：任务停在 acked、槽位永久占用（#218）；巡检过期后又会重派（#222 双执行）。
func TestUncleanEndAbandonsStartedTask(t *testing.T) {
	f := newNodeFixture(t)
	f.handler.HeartbeatSec = 1 // 判死窗口 = 2×1s
	f.handler.LeaseTTLSec = 5
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_unclean")
	publishTo(t, f, f.node.ID, task.ID)
	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked)
	send(t, conn, &ProgressFrame{Type: NodeFrameProgress, TaskID: task.ID, Note: "step 1/3"})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskRunning)

	// 停发心跳，等看门狗判死。服务端用 1008（政策违规）关连接，客户端读到闭环即证明判死已发生。
	if code := websocket.CloseStatus(readUntilClosed(t, conn, 8*time.Second)); code != websocket.StatusPolicyViolation {
		t.Fatalf("connection closed with status %v, want 1008 (heartbeat timeout)", code)
	}

	// 任务被执行者放弃：诊断可读、且不在调度池里。
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAbandoned)
	got := mustGetTask(t, f, task.ID)
	if !strings.Contains(got.Error, "executor_lost") {
		t.Fatalf("abandoned task error = %q, want it to start with executor_lost: <reason>", got.Error)
	}
	if isRedispatchable(t, f, task.ID) {
		t.Fatal("abandoned task is in ListRedispatchable: an unclean end must NEVER be redispatched (#222 at-most-once)")
	}
	assertSlotFree(t, f, task.ID)
}

// ---------- W3（#218/#222）：clean 结束 → 已开始的任务立刻重排 ----------

// TestCleanEndRequeuesStartedTask 对端正常关闭（1000）= 执行者确定消失
// → 任务立刻回 pending（不等 30s 租约），槽位释放，可被马上重派。
//
// 旧代码：停在 acked，槽位占死，只能等租约过期 + 巡检（最坏 35s），
// 而且若那个进程其实没死就是双执行。
func TestCleanEndRequeuesStartedTask(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_clean")
	publishTo(t, f, f.node.ID, task.ID)
	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked)
	send(t, conn, &ProgressFrame{Type: NodeFrameProgress, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskRunning)

	// 对端正常关闭（1000）：socket 没了 → 那个进程手上的命令也随进程消失。
	if err := conn.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close connection: %v", err)
	}

	waitStatus(t, f.db, f.nsID, task.ID, store.TaskPending)
	got := mustGetTask(t, f, task.ID)
	if got.NodeID != nil {
		t.Fatalf("requeued task still has node_id = %q, want NULL", *got.NodeID)
	}
	if got.LeaseUntil != nil {
		t.Fatalf("requeued task still has lease_until = %d, want NULL", *got.LeaseUntil)
	}
	if got.NotBefore != nil {
		t.Fatalf("requeued task has not_before = %d, want NULL (立刻可重派)", *got.NotBefore)
	}
	if !isRedispatchable(t, f, task.ID) {
		t.Fatal("requeued task is not in ListRedispatchable: a clean end must put it back in the pool")
	}
	assertSlotFree(t, f, task.ID)
}

// ---------- W4（#221 的槽位钉死路径）：assigned 且未开始 → 无条件重排 ----------

// TestAssignedNotStartedRequeuedOnUncleanEnd 只 assigned（没 ack、started_at IS NULL）
// 的「领了单但一行没跑」的任务，即使按 unclean 结束也回 pending。
//
// 这正是 #221 那条不需要 kill agent 的钉死路径：1MiB payload 把连接打死，
// 任务停在 assigned、槽位被 busy_task 钉住，而中枢再也不会碰它。
// 安全依据：没有副作用可重复执行。
func TestAssignedNotStartedRequeuedOnUncleanEnd(t *testing.T) {
	f := newNodeFixture(t)
	f.handler.HeartbeatSec = 1
	f.handler.LeaseTTLSec = 5
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_assigned_only")
	publishTo(t, f, f.node.ID, task.ID) // 连接名下有它，但客户端一行都没跑（不发 ack/progress）
	if got := mustGetTask(t, f, task.ID); got.StartedAt != nil {
		t.Fatalf("precondition broken: started_at = %d, want NULL", *got.StartedAt)
	}

	// 看门狗判死（unclean）
	if code := websocket.CloseStatus(readUntilClosed(t, conn, 8*time.Second)); code != websocket.StatusPolicyViolation {
		t.Fatalf("connection closed with status %v, want 1008 (heartbeat timeout)", code)
	}

	waitStatus(t, f.db, f.nsID, task.ID, store.TaskPending)
	got := mustGetTask(t, f, task.ID)
	if got.NodeID != nil || got.LeaseUntil != nil {
		t.Fatalf("requeued task still bound: node_id=%v lease_until=%v, want both NULL", got.NodeID, got.LeaseUntil)
	}
	if !isRedispatchable(t, f, task.ID) {
		t.Fatal("assigned-not-started task was not put back in the pool (#221 的槽位钉死路径)")
	}
	assertSlotFree(t, f, task.ID)
}

// ---------- W8（#222）：同一任务不会被两条连接执行 ----------

// TestSameTaskNotExecutedByTwoConnections 同一 taskID 只许一条活着的连接执行。
//
//   - 推给第二条连接 → ErrTaskInflight（旧实现：照推 → 两个进程各跑一遍，结果各自落库）
//   - 第二条连接的结果帧 → lease_expired（fencing：任务不归你），且任务状态不推进
//   - 真正执行者（A）的结果照常落库
func TestSameTaskNotExecutedByTwoConnections(t *testing.T) {
	f := newNodeFixture(t)
	connA := f.mustDial(t)
	hello(t, connA, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})
	nodeB, connB := f.secondNode(t)

	ctx := context.Background()
	task := f.makeAssignedTask(t, "trace_dup")
	publishTo(t, f, f.node.ID, task.ID)
	send(t, connA, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked) // A 确确实实在跑

	// ① 同一任务推给另一条连接 → 拒绝
	assigned := mustGetTask(t, f, task.ID)
	err := f.handler.hub().PublishAssign(ctx, f.nsID, nodeB.ID, assigned)
	if !errors.Is(err, ErrTaskInflight) {
		t.Fatalf("PublishAssign to a second connection = %v, want ErrTaskInflight "+
			"(同一任务只许一个执行者，否则就是 #222 的并发双执行)", err)
	}

	// ② 第二条连接想报结果 → lease_expired，且任务状态原地不动
	send(t, connB, &ResultFrame{Type: NodeFrameResult, TaskID: task.ID,
		Status: store.TaskSucceeded, Summary: "duplicate", Out: strPtr("fix")})
	frame := readUntil(t, connB, NodeFrameError)
	if frame["code"] != ErrCodeLeaseExpired {
		t.Fatalf("second connection's result got code = %v, want %q (fencing)", frame["code"], ErrCodeLeaseExpired)
	}
	if msg, _ := frame["message"].(string); !strings.Contains(msg, "another connection") {
		t.Fatalf("error message = %q, want it to say the task is being executed by another connection", msg)
	}
	if got := mustGetTask(t, f, task.ID); got.Status != store.TaskAcked {
		t.Fatalf("task status = %q after a foreign result, want %q (第二条连接不许推进状态)", got.Status, store.TaskAcked)
	}
	if got := mustGetTask(t, f, task.ID); got.Reported || got.Out != nil {
		t.Fatalf("foreign result leaked into the row: reported=%v out=%v", got.Reported, got.Out)
	}

	// ③ 真正的执行者照常收工
	send(t, connA, &ResultFrame{Type: NodeFrameResult, TaskID: task.ID,
		Status: store.TaskSucceeded, Summary: "ok", Out: strPtr("fix")})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskSucceeded)
	if got := mustGetTask(t, f, task.ID); got.Out == nil || *got.Out != "fix" {
		t.Fatalf("owner's result not recorded: out=%v", got.Out)
	}
}

// ---------- 测试辅助 ----------

// secondNode 在同一命名空间里再建一台节点并连上来（W8 需要「另一条连接」）。
func (f *nodeFixture) secondNode(t *testing.T) (*store.Node, *websocket.Conn) {
	t.Helper()
	ctx := context.Background()
	node := &store.Node{NamespaceID: f.nsID, Name: "mbp2-" + store.NewUserID()}
	token, err := f.db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("create second node: %v", err)
	}
	conn, resp, err := f.dial(t, token)
	if err != nil {
		t.Fatalf("dial second node: %v (status %d)", err, statusOf(resp))
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	hello(t, conn, node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})
	return node, conn
}

// publishTo 把库里的一条任务真的派给某条连接（#218 起这是心跳续租 / cleanup 释放的前提）。
func publishTo(t *testing.T, f *nodeFixture, nodeID, taskID string) *store.Task {
	t.Helper()
	task := mustGetTask(t, f, taskID)
	if err := f.handler.hub().PublishAssign(context.Background(), f.nsID, nodeID, task); err != nil {
		t.Fatalf("publish assign for %s: %v", taskID, err)
	}
	return task
}

// mustGetTask 读一条任务（失败直接 Fatal）。
func mustGetTask(t *testing.T, f *nodeFixture, taskID string) *store.Task {
	t.Helper()
	got, err := f.db.GetTask(context.Background(), f.nsID, taskID)
	if err != nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	return got
}

// waitLeaseAtLeast 等 lease_until 达到某个下界（状态写入是异步的，需要轮询），返回最终值。
func waitLeaseAtLeast(t *testing.T, db *store.DB, namespaceID, taskID string, want int64) int64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last *int64
	for {
		got, err := db.GetTask(context.Background(), namespaceID, taskID)
		if err == nil && got.LeaseUntil != nil {
			last = got.LeaseUntil
			if *got.LeaseUntil >= want {
				return *got.LeaseUntil
			}
		}
		if time.Now().After(deadline) {
			if last == nil {
				t.Fatalf("task %s lease_until is NULL, want >= %d", taskID, want)
			}
			t.Fatalf("task %s lease_until = %d, want >= %d", taskID, *last, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// isRedispatchable 报告任务是否在巡检的重派候选里（pending + node_id NULL + 没有退避）。
func isRedispatchable(t *testing.T, f *nodeFixture, taskID string) bool {
	t.Helper()
	tasks, err := f.db.ListRedispatchable(context.Background(), store.Now(), 100)
	if err != nil {
		t.Fatalf("list redispatchable: %v", err)
	}
	for _, x := range tasks {
		if x.ID == taskID {
			return true
		}
	}
	return false
}

// assertSlotFree 断言没有槽位还指着这条任务（busy_task 非空 = #218 的槽位钉死）。
func assertSlotFree(t *testing.T, f *nodeFixture, taskID string) {
	t.Helper()
	insts, err := f.db.ListInstances(context.Background(), f.node.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range insts {
		if in.BusyTask != nil && *in.BusyTask == taskID {
			t.Fatalf("slot %d is still pinned to task %s after the connection ended (#218 槽位钉死)",
				in.Slot, taskID)
		}
	}
}

// readUntilClosed 一直读到连接关闭为止，返回关闭错误（用来确认服务端已判死）。
func readUntilClosed(t *testing.T, conn *websocket.Conn, d time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatalf("connection was not closed within %s: %v", d, err)
			}
			return err
		}
	}
}
