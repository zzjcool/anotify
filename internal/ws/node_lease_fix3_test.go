package ws

// 第三轮收口（第二轮独立复审回单）的 ws 侧回归测试。
//
// 每条用例都钉一个**具体缺陷/决定**，命名里带审查编号，便于回溯：
//
//	M4   （重裁）clean 只认「对端发了 close 帧」：1000/1001/1005 → LostRequeue；
//	     EOF / ECONNRESET / EPIPE（没有 close 帧）→ LostAbandon（子进程组可能还在跑）
//	n5-1 生产真实形状：acked + started_at IS NULL + **非幂等**能力 + 对端发 close 帧
//	     → 仍然 abandoned（clean 只证明子进程被杀，能不能重放是另一回事）
//	n1   hello 的 node 级写必须在 hub.Register **之后**再写一次（M2 残留的 TOCTOU）
//	n2   PublishAssign 的权威世代回读挪到写帧**之前**：读失败就不发帧、返回错误
//	n4   hub.release 带 owner 校验：只删「在飞登记 == 我」的条目
//
// 断言一律看库里的机器事实（GetTask / ListInstances / ListRedispatchable / hub 的在飞表）
// 或协议帧，不看日志文案。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- n5-1 / M4：非幂等 + acked + 对端发 close 帧 → 仍然 abandoned ----------

// TestAckedNonIdempotentCleanEndAbandons 是覆盖缺口的**最危险那格**（审查 n5-1）。
//
// 生产形状：agent 的逻辑是「先 ack 再跑」（cmd/agent/client.go 先 ack 后 startTask），
// 而全仓**没有任何地方发 progress 帧** ⇒ 节点执行的任务在库里只会到 `acked`，
// `started_at` 永远是 NULL。所以「acked + started_at IS NULL」就是「正在干活」。
//
// 对端发 close 帧（1000）= 走的是 client.go 的优雅退出（先 drain 掐掉进程组再关连接）
// ⇒ 子进程确定已死 ⇒ clean。但 clean 只回答「子进程死没死」，**没有**回答
// 「这件事能不能重放」：非幂等能力重放就是 #222（同一个副作用做两遍）。
// 所以这里必须由 M1 的 `!idempotent → abandoned` 判据收敛，而不是 clean → pending。
func TestAckedNonIdempotentCleanEndAbandons(t *testing.T) {
	f := newNodeFixture(t)
	// 非幂等能力（契约 §3.2 的 idempotent:false）：重放不安全。
	f.defineCapabilityWithLifecycle(t, "notify", 1, store.Lifecycle{Idempotent: boolPtr(false)})

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID,
		[]CapabilityDecl{{Capability: "review", Version: 1}, {Capability: "notify", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTaskWithCapability(t, "trace_n5_nonidem", "notify", 1)
	publishTo(t, f, f.node.ID, task.ID)
	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked)
	// 前提：生产形状里 started_at 是 NULL（没人发 progress 帧）。
	if got := mustGetTask(t, f, task.ID); got.StartedAt != nil {
		t.Fatalf("precondition broken: started_at = %d, want NULL（生产里没人发 progress 帧）", *got.StartedAt)
	}

	// 对端**发了** close 帧：按新裁决这是 clean（子进程组已被 drain 掐死）。
	if err := conn.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close connection: %v", err)
	}

	// 但 clean 不是「可以重放」的同义词：非幂等任务必须 abandoned。
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAbandoned)
	got := mustGetTask(t, f, task.ID)
	if !strings.Contains(got.Error, "executor_lost") {
		t.Fatalf("abandoned task error = %q, want it to start with executor_lost: <reason>", got.Error)
	}
	if !strings.Contains(got.Error, "not idempotent") {
		t.Fatalf("abandoned task error = %q, want it to name the real cause (capability is not idempotent): "+
			"clean 结束只证明子进程被杀，不证明重放安全", got.Error)
	}
	if isRedispatchable(t, f, task.ID) {
		t.Fatal("non-idempotent task is in ListRedispatchable after a clean end: 重放就是 #222 的并发双执行")
	}
	assertSlotFree(t, f, task.ID)
}

// ---------- M4 行为契约：kill -9（没有 close 帧）→ abandoned，不重派 ----------

// TestKilledAgentAbandonsInflightTask 钉住 M4 重裁后的行为契约。
//
// 裁决依据（代码事实）：cmd/agent/runner.go 用 `SysProcAttr{Setpgid: true}` 起子进程，
// cmd.Cancel = killProcessGroup（杀整组），但**没有 Pdeathsig**。agent 被 kill -9 时
// 它的 socket 直接消失（服务端读到 EOF/RST，CloseStatus == -1），而那个独立进程组
// 可能还在跑并把副作用做完——被 SIGKILL 的 agent 没有机会 cancelAll()。
//
// 所以「没有 close 帧」一律 unclean → abandoned + executor_lost，**绝不重派**。
// 代价（有意）：agent 被 kill -9 时需要人工重放/等运维介入（#222 at-most-once）。
func TestKilledAgentAbandonsInflightTask(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_kill9")
	publishTo(t, f, f.node.ID, task.ID)
	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked)

	// kill -9 形状：对端**不发** close 帧，socket 直接消失。
	_ = conn.CloseNow()

	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAbandoned)
	got := mustGetTask(t, f, task.ID)
	if !strings.Contains(got.Error, "executor_lost") {
		t.Fatalf("abandoned task error = %q, want it to start with executor_lost: <reason>", got.Error)
	}
	if !strings.Contains(got.Error, "without a close frame") {
		t.Fatalf("abandoned task error = %q, want it to name the real cause (peer vanished without a close frame): "+
			"排障时必须能区分「优雅退出」与「被强杀」", got.Error)
	}
	if isRedispatchable(t, f, task.ID) {
		t.Fatal("task is in ListRedispatchable after a kill -9 shaped disconnect: " +
			"子进程组可能还在跑，重派就是 #222 的并发双执行")
	}
	assertSlotFree(t, f, task.ID)
}

// ---------- n1：hello 必须在 Register 之后再写一次 node 级状态 ----------

// stepGate 是一个「进入即阻塞、只阻塞第一次」的同步点。
type stepGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStepGate() *stepGate {
	return &stepGate{entered: make(chan struct{}), release: make(chan struct{})}
}

// pass 报告「第一次到达」并阻塞到放行（后续调用直接返回）。
func (g *stepGate) pass() {
	blocked := false
	g.once.Do(func() {
		blocked = true
		close(g.entered)
	})
	if blocked {
		<-g.release
	}
}

// waitGate 等同步点被进入（超时即失败）。
func waitGate(t *testing.T, g *stepGate, what string) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("5s 内没有走到同步点：%s", what)
	}
}

// gateNodeStateStore 把「旧连接 cleanup 的 node 级写」与「新连接 hello 的注册时机」
// 变成确定的同步点（审查 n1 是纯时序窗口，只能这样稳定复现）。
type gateNodeStateStore struct {
	NodeStore
	// markDead 卡住第一次 MarkInstancesDead：旧连接的 ② 已经问过 current()（读到 nil），
	// 但两条 UPDATE 还没落库——这正是 n1 的窗口。
	markDead *stepGate
	// statusAfterWrite 在 UpdateNodeStatus(online) **写完**之后卡住调用方：
	// 这样旧连接的 dead/offline 一定落在「新连接已写 alive/online、但还没 Register」的窗口里。
	statusAfterWrite *stepGate
	statusArmed      atomic.Bool
}

func (s *gateNodeStateStore) MarkInstancesDead(ctx context.Context, nodeID string) error {
	s.markDead.pass()
	return s.NodeStore.MarkInstancesDead(ctx, nodeID)
}

func (s *gateNodeStateStore) UpdateNodeStatus(ctx context.Context, namespaceID, nodeID, status string) error {
	err := s.NodeStore.UpdateNodeStatus(ctx, namespaceID, nodeID, status)
	if status == store.NodeStatusOnline && s.statusArmed.Load() {
		s.statusAfterWrite.pass()
	}
	return err
}

// TestHelloReassertKeepsNodeOnline 钉住 n1：旧连接的 cleanup 在「新连接写完 alive/online、
// 还没 Register」的窗口里把它覆盖成 dead/offline 后，hello 必须自己纠正回来。
//
// 没有这次重写时的后果（就是 #218 的症状，且**不自愈**）：TouchNode 只写 last_seen_at，
// alive=1 全仓只有 hello 的 UpsertInstances 写，FreeSlotsByNode 要求 alive=1、
// NodesForCapability 要求 online ⇒ 节点「显示在线却永远接不到活」，直到下一次 hello。
//
// 用例把两件事变成确定的同步点（否则要靠毫秒级运气）：
//  1. 旧连接的 MarkInstancesDead 入口卡住 ⇒ 它的 current() 判据已经放行（读到 nil）；
//  2. 新连接 applyHello 的 UpdateNodeStatus(online) 在**写完之后**卡住 ⇒ 还没 Register。
//
// 放行顺序保证旧连接的 dead/offline 落在「新连接写完、还没 Register」之间；
// 之后的断言只看库里的机器事实：status=online 且 alive 实例数 == 申报数。
func TestHelloReassertKeepsNodeOnline(t *testing.T) {
	const slots = 2
	gate := &gateNodeStateStore{markDead: newStepGate(), statusAfterWrite: newStepGate()}
	f := newNodeFixture(t, func(h *NodeHandler) {
		gate.NodeStore = h.Store
		h.Store = gate
	})
	insts, caps := m2Slots(slots)

	connA := f.mustDial(t)
	hello(t, connA, f.node.ID, caps, insts)
	tasks := f.inflightTasks(t, connA, slots)

	// A 断（对端不发 close 帧）。它名下有一批在飞任务，cleanup 会走完整的 ①→④。
	_ = connA.CloseNow()

	// ① 旧连接的 cleanup 越过 ② 的 current() 判据（此刻 B 还没 Register → nil），卡在写之前。
	waitGate(t, gate.markDead, "旧连接 cleanup 的 MarkInstancesDead")

	// ② 窗口内：B 用同一 token 完成 applyHello 的**写**，卡在「写完 online、还没 Register」。
	gate.statusArmed.Store(true)
	connB := f.mustDial(t)
	send(t, connB, &HelloFrame{
		Type: NodeFrameHello, NodeID: f.node.ID, Protocol: ProtocolVersion,
		Capabilities: caps, Instances: insts,
	})
	waitGate(t, gate.statusAfterWrite, "新连接 applyHello 的 UpdateNodeStatus(online)")

	// ③ 放行旧连接：dead/offline 落库，把 B 刚写的 alive/online 覆盖掉。
	close(gate.markDead.release)
	waitNodeOfflineAndDead(t, f)

	// ④ 放行 B：Register → **再写一次** node 级状态（n1 的修）→ welcome。
	gate.statusArmed.Store(false)
	close(gate.statusAfterWrite.release)
	readUntil(t, connB, NodeFrameWelcome)

	// 旧连接名下那批任务照常落库（没有 close 帧 → unclean → abandoned），槽位释放。
	for _, task := range tasks {
		waitNotInflight(t, f, task.ID)
	}

	// hello 完成后必须是**新连接写的那份**：online + alive == 申报数 + 有空闲槽位。
	assertNodeServing(t, f, slots)
}

// waitNodeOfflineAndDead 等「旧连接的 node 级写已经落库」——即 n1 的覆盖真的发生了。
func waitNodeOfflineAndDead(t *testing.T, f *nodeFixture) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	var lastStatus string
	var lastAlive int
	for time.Now().Before(deadline) {
		node, err := f.db.GetNode(ctx, f.nsID, f.node.ID)
		if err == nil {
			lastStatus = node.Status
			insts, ierr := f.db.ListInstances(ctx, f.node.ID)
			if ierr == nil {
				lastAlive = 0
				for _, in := range insts {
					if in.Alive {
						lastAlive++
					}
				}
				if lastStatus == store.NodeStatusOffline && lastAlive == 0 {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("旧连接的 node 级写没有落库（status=%q alive=%d, want offline/0）：同步点没卡住窗口",
		lastStatus, lastAlive)
}

// ---------- n2：世代读不到 → 不发帧、返回错误 ----------

// failingGetTaskStore 让 GetTask 恒失败（其余方法透传）。
type failingGetTaskStore struct {
	NodeStore
	err error
}

func (s *failingGetTaskStore) GetTask(context.Context, string, string) (*store.Task, error) {
	return nil, s.err
}

// TestPublishAssignFailsWhenGenerationUnreadable 钉住 n2：回读权威世代失败时
// **绝不能**带着调用方传的 attempts 继续派发。
//
// 旧实现的回退路径把 M3 原样带回来：路由层传的 Attempts 是硬编码的 1，而 cleanup 的
// ResolveLostTasks 用库里的 attempts 做 CAS ⇒ 整条主动释放被静默跳过 ⇒ 任务停在
// assigned、槽位被 busy_task 钉住，直到 20s ack 租约过期才由巡检兜底。
//
// 现在读失败就是一次**干净的失败**：不发帧、不登记在飞/归属，route 会把任务退回
// pending 重试（requeueAfterPublishFailure）。
func TestPublishAssignFailsWhenGenerationUnreadable(t *testing.T) {
	f := newNodeFixture(t, func(h *NodeHandler) {
		h.Store = &failingGetTaskStore{NodeStore: h.Store, err: errors.New("simulated read outage")}
	})
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_n2_readfail")
	err := f.handler.hub().PublishAssign(context.Background(), f.nsID, f.node.ID, task)
	if err == nil {
		t.Fatal("PublishAssign = nil, want an error: 世代读不到时不许带着可能过期的值继续派发（M3 的形状会回来）")
	}
	if !strings.Contains(err.Error(), "authoritative attempts") {
		t.Fatalf("PublishAssign error = %v, want it to name the failed read (authoritative attempts)", err)
	}

	// ① 对端收不到 assign 帧：帧一旦发出去，这条连接就会去执行它。
	if frame, ok := tryReadFrame(conn, 500*time.Millisecond); ok {
		t.Fatalf("peer received %v after a failed generation read, want no frame at all", frame)
	}

	// ② 没有登记在飞位（否则这条 taskID 会被永久挡住）。
	if cur := f.handler.hub().owner(task.ID); cur != nil {
		t.Fatalf("task %s is registered as in-flight even though no frame was sent", task.ID)
	}
	// ③ 也没有登记归属（否则这条连接会替一条没派出去的任务续租）。
	sess := f.handler.hub().current(f.node.ID)
	if sess == nil {
		t.Fatal("node has no current connection any more")
	}
	if owners := sess.ownedOwners(); len(owners) != 0 {
		t.Fatalf("session owns %v after a failed generation read, want none", owners)
	}
}

// TestPublishAssignSkipsTerminalTask 钉住 n3 的终态判断：给一条**已经终态**的任务派发
// 不发帧、不 claim、不 own，直接返回 nil。
//
// 病态竞态的形状：任务在我回读世代之前就跑完了（同一连接上的重复派发 / 巡检与 result
// 撞车）。旧实现会照发帧并给一条终态任务登记在飞位 + 归属，而它再也不会被 forget
// （节点不会再报这条任务）→ 登记泄漏到连接结束，还挡着同 taskID 的后续派发。
func TestPublishAssignSkipsTerminalTask(t *testing.T) {
	ctx := context.Background()
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_n3_terminal")
	// 把行推到终态（assigned → acked → succeeded）：刻意**不**发帧，
	// 这样会话归属表从一开始就是空的——PublishAssign 有没有乱登记一眼就看得出来。
	if err := f.db.Transition(ctx, f.nsID, task.ID, store.TaskAssigned, store.TaskAcked); err != nil {
		t.Fatalf("ack task: %v", err)
	}
	if err := f.db.ReportResultAndCreateTasks(ctx, f.nsID, task.ID, store.TaskSucceeded,
		nil, true, "ok", nil, "", nil); err != nil {
		t.Fatalf("report result: %v", err)
	}
	row := mustGetTask(t, f, task.ID)
	if !store.IsTerminalStatus(row.Status) {
		t.Fatalf("precondition broken: status = %q, want a terminal state", row.Status)
	}

	if err := f.handler.hub().PublishAssign(ctx, f.nsID, f.node.ID, row); err != nil {
		t.Fatalf("PublishAssign on a terminal task = %v, want nil（任务本身已有结论，不该制造错误）", err)
	}
	// ① 不发帧：给终态任务派发等于让节点白跑一遍。
	if frame, ok := tryReadFrame(conn, 500*time.Millisecond); ok {
		t.Fatalf("peer received %v for a task that is already %q, want no frame at all", frame, row.Status)
	}
	// ② 不登记在飞位 / 归属（否则永远清不掉）。
	if cur := f.handler.hub().owner(task.ID); cur != nil {
		t.Fatalf("terminal task %s is registered as in-flight", task.ID)
	}
	sess := f.handler.hub().current(f.node.ID)
	if sess == nil {
		t.Fatal("node has no current connection any more")
	}
	if owners := sess.ownedOwners(); len(owners) != 0 {
		t.Fatalf("session owns %v after publishing a terminal task, want none", owners)
	}
}

// ---------- n4：release 必须校验 owner ----------

// TestReleaseIgnoresForeignOwner 钉住 n4：无条件的 `delete(inflight, taskID)` 会让
// 「A 迟到的 release」删掉「B 刚 claim 的条目」——在飞表是 #222 唯一的内存护栏，
// 护栏缺口 = 同一任务可能被派给两条连接（并发双执行）。
//
// 当前只有 forget → release 一条路径，靠 handleResult 的早退顺序隐式挡住；
// 这个用例把「靠签名保证」的那一半钉住（以后多一个 publish/forget 入口不会变成误删）。
func TestReleaseIgnoresForeignOwner(t *testing.T) {
	h := NewNodeHub()
	a := &nodeSession{}
	b := &nodeSession{}

	h.claim("tsk_n4", b)
	h.release("tsk_n4", a) // A 不认识这条任务：一个字节都不该动
	if cur := h.owner("tsk_n4"); cur != b {
		t.Fatalf("owner = %v after a foreign release, want the real owner (B): "+
			"无条件 delete 会让两条连接都以为自己能跑这条任务", cur)
	}
	h.release("tsk_n4", b)
	if cur := h.owner("tsk_n4"); cur != nil {
		t.Fatalf("owner = %v after the owner's release, want nil", cur)
	}
}

// ---------- 测试辅助 ----------

// defineCapabilityWithLifecycle 定义一个带 lifecycle 的能力（n5-1 需要 idempotent:false）。
func (f *nodeFixture) defineCapabilityWithLifecycle(t *testing.T, name string, version int, lc store.Lifecycle) {
	t.Helper()
	capVer := &store.CapabilityVersion{
		NamespaceID: f.nsID, Name: name, Version: version,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"fix"}, Source: store.CapSourceBuiltin,
		Lifecycle: lc,
	}
	if err := f.db.CreateCapability(context.Background(), capVer); err != nil {
		t.Fatalf("create capability %s@%d: %v", name, version, err)
	}
}

// makeAssignedTaskWithCapability 建一条派给本节点的任务，能力由调用方指定
// （makeAssignedTask 写死 review@1，n5-1 需要非幂等能力）。
func (f *nodeFixture) makeAssignedTaskWithCapability(t *testing.T, traceID, capName string, version int) *store.Task {
	t.Helper()
	ctx := context.Background()
	cv, err := f.db.GetCapability(ctx, f.nsID, capName, version)
	if err != nil {
		t.Fatalf("get capability %s@%d: %v", capName, version, err)
	}
	task := &store.Task{
		NamespaceID: f.nsID, TraceID: traceID, CapabilityID: cv.ID,
		Capability: cv.Name, Version: version, Params: json.RawMessage(`{}`),
		CapHash: cv.ContentHash, GraphID: "grf_x",
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	instID, err := f.db.LeaseInstance(ctx, f.node.ID, task.ID)
	if err != nil {
		t.Fatalf("lease instance: %v", err)
	}
	if err := f.db.AssignTask(ctx, f.nsID, task.ID, f.node.ID, instID, store.Now()+store.AckWaitSec); err != nil {
		t.Fatalf("assign: %v", err)
	}
	return task
}

func boolPtr(b bool) *bool { return &b }
