package ws

// 第二轮（独立审查回单）的 ws 侧回归测试。
//
// 每条用例都钉一个**具体缺陷**，命名里带审查编号，便于回溯：
//
//	M2  旧连接的 cleanup 用 ① 的缓存值写 node 级状态 → 把新连接刚写好的 online/alive 改回去
//	M3  PublishAssign 记的世代来自 route 硬编码的 Attempts=1 → attempts≥2 的任务在 cleanup 里被 CAS 跳过
//	M4  classifyReadError 的 1005/1006 判据与 coder/websocket 实际行为相反（本轮已重裁：
//	    clean 只认「对端发了 close 帧」；EOF/RST/EPIPE 回 unclean，见 node_lease_fix3_test.go）
//	m5  next-hop 子任务 params 超限 → 节点只看到 invalid_transition（病因被埋掉）
//	m10 覆盖缺口：applyHello 的 ReclaimStaleSlots / inflight 释放后的可派性 / 续租失败时连接关闭
//
// 断言一律看**库里的机器事实**（GetTask / ListInstances / FreeSlotsByNode / hub 的在飞表）
// 或协议帧，不看日志文案。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- M4：结束原因判据表（classifyReadError 的唯一事实源） ----------

// TestClassifyReadError 表驱动钉住 clean/unclean 判据。
//
// 为什么每条都要写理由：这张表决定「执行者失联后重排还是放弃」，判错的后果是
// **同一个任务被两个进程并发执行**（#222）或**任务永久 abandoned**（#218 主场景）。
//
// 本轮（第三轮收口）的裁决（owner 已定，依据见 classifyReadError 的注释）：
// **clean 只认「对端发了 close 帧」（1000/1001/1005）**——因为 agent 用 Setpgid 起
// 子进程、而且 grace 窗口里先 drain（杀进程组）再 conn.Close(1000)，所以只有 close 帧
// 能证明子进程组已经死了。没有 close 帧（EOF / RST / EPIPE）一律 unclean：那个进程组
// 可能还在跑（kill -9 没有机会杀它），重排就是双执行。
func TestClassifyReadError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// wantClean 是这条错误该判 clean 吗。
		wantClean bool
		// wantReason 是 reason 里必须出现的片段（reason 要能进日志/executor_lost 文案，
		// 空 reason 会让排障断线）。
		wantReason string
		// why 是判定理由（契约 §3.2 的取舍）。
		why string
	}{
		{
			name: "nil", err: nil, wantClean: true, wantReason: "connection closed",
			why: "读循环没有报错就结束了（上层收尾）：连接已经结束，重排安全",
		},
		{
			name:      "1000 normal closure",
			err:       websocket.CloseError{Code: websocket.StatusNormalClosure, Reason: "bye"},
			wantClean: true, wantReason: "peer closed (1000)",
			why: "对端主动正常关闭（agent 收到 SIGTERM 的路径）：那个进程收了工，重排安全",
		},
		{
			name:      "1001 going away",
			err:       websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "restart"},
			wantClean: true, wantReason: "peer closed (1001)",
			why: "对端要走了（重启/部署）：同样是主动收工",
		},
		{
			name:      "1005 no status received",
			err:       fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: websocket.StatusNoStatusRcvd}),
			wantClean: true, wantReason: "peer closed (1005)",
			why: "1005 只在**收到一帧空 body 的 close 帧**时出现：对端确实发了 close（主动收工）",
		},
		{
			name:      "1009 message too big",
			err:       websocket.CloseError{Code: websocket.StatusMessageTooBig, Reason: "read limited"},
			wantClean: false, wantReason: "1009",
			why: "对端（agent）自己的读上限炸了：它**活着**，只是在拒绝我们的帧（#221）；重派只会让它再炸一次，还可能并发执行",
		},
		{
			name:      "io.EOF without close frame",
			err:       fmt.Errorf("failed to get reader: failed to read frame header: %w", io.EOF),
			wantClean: false, wantReason: "peer vanished without a close frame: process may still be running (EOF)",
			why: "对端不发 close 帧直接断流 = kill -9 / OOM / 进程退出。**不能**当 clean：" +
				"agent 用 Setpgid 起子进程且没有 Pdeathsig（cmd/agent/runner.go:129），" +
				"被 SIGKILL 的 agent 没机会杀进程组 → 那条命令可能还在跑并把副作用做完（#222）",
		},
		{
			name:      "ECONNRESET",
			err:       &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)},
			wantClean: false, wantReason: "ECONNRESET/EPIPE",
			why: "RST 只证明 socket 没了（可能是 kill -9，也可能是中间盒/NAT/LB 摘除），" +
				"不证明对端进程组死了 → unclean（abandoned + executor_lost）",
		},
		{
			name:      "EPIPE",
			err:       &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)},
			wantClean: false, wantReason: "ECONNRESET/EPIPE",
			why: "对端已关闭写侧：与 ECONNRESET 同义——同样不能证明子进程组已死",
		},
		{
			name:      "1006 abnormal closure",
			err:       websocket.CloseError{Code: websocket.StatusAbnormalClosure},
			wantClean: false, wantReason: "unclean disconnect",
			why: "1006 **根本不会由库的 Read 返回**（只导出给 wasm，且被 validWireCloseCode 拒收）；" +
				"就算伪造出来也只走保守兜底 —— 「对端异常断」由上面的 EOF/RST 两条判定负责，不靠它",
		},
		{
			name: "net.ErrClosed (我们自己关的)", err: fmt.Errorf("failed to get reader: %w", net.ErrClosed),
			wantClean: false, wantReason: "closed locally",
			why: "本地已关闭：读线程看到的「已关闭」不证明对端消失（kill/watchdog 会先 markEnd，这里是兜底）",
		},
		{
			name: "context.Canceled (中枢退出)", err: fmt.Errorf("connection closed: %w", context.Canceled),
			wantClean: true, wantReason: "hub shutting down",
			why: "中枢自己在退出：重启后本就应该重排（进程无论如何都跑不完手上的活）",
		},
		{
			name: "unclassifiable error", err: errors.New("failed to get reader: some unknown transport failure"),
			wantClean: false, wantReason: "unclean disconnect",
			why: "无法归类 → 保守 unclean：#222 宁可 abandoned 也不要静默并发执行两遍",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, reason := classifyReadError(tc.err)
			if clean != tc.wantClean {
				t.Errorf("classifyReadError(%v) = clean %v, want %v (%s)", tc.err, clean, tc.wantClean, tc.why)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q (reason 进日志与 executor_lost 文案，不能是空泛串)", reason, tc.wantReason)
			}
			if reason == "" {
				t.Errorf("empty reason for %v: 排障会断线", tc.err)
			}
		})
	}
}

// ---------- m10-1：applyHello 必须调 ReclaimStaleSlots（#218 兜底） ----------

// TestHelloReclaimsStaleSlots hello 会把「槽位指向的任务已经不在这台机器上跑了」的钉子清掉。
//
// 缺口的形状：把 applyHello 里那一行 ReclaimStaleSlots 删掉，改动前的全套测试仍然全绿
// ——store 侧 S7 覆盖的是方法本身，没人证明 hello 真的会调它。
//
// 两个槽位对照，证明既不是「什么都不做」也不是「全清」：
//   - slot 0 钉在一条 **pending** 任务上（中枢被 kill -9 留下的历史钉子）→ hello 后必须变空闲；
//   - slot 1 钉在一条**仍归本节点在飞**（assigned）的任务上 → 一个字节都不许动。
func TestHelloReclaimsStaleSlots(t *testing.T) {
	ctx := context.Background()
	f := newNodeFixture(t)

	// 先用 store API 造出两个槽位（等价于「上一代进程申报过」），不需要先连一条连接。
	if _, err := f.db.UpsertInstances(ctx, f.nsID, f.node.ID,
		[]store.InstanceSpec{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}}); err != nil {
		t.Fatalf("upsert instances: %v", err)
	}

	// 历史钉子：一条 pending 任务被钉在 slot 0 上（没有任何连接名下有它）。
	stale := f.makePendingTask(t, "trace_stale_pin")
	if _, err := f.db.Exec(`UPDATE node_instances SET busy_task = ? WHERE node_id = ? AND slot = 0`,
		stale.ID, f.node.ID); err != nil {
		t.Fatalf("pin stale slot: %v", err)
	}
	// 对照组：一条真的派给本节点的任务占着 slot 1（LeaseInstance 会挑剩下的空闲槽）。
	live := f.makeAssignedTask(t, "trace_live_pin")
	if got := instanceSlotOf(t, f, live.ID); got != 1 {
		t.Fatalf("precondition broken: live task sits on slot %d, want 1 (slot 0 是钉子)", got)
	}

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}})

	insts, err := f.db.ListInstances(ctx, f.node.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("instances = %d, want 2", len(insts))
	}
	bySlot := map[int]*store.NodeInstance{}
	for _, in := range insts {
		bySlot[in.Slot] = in
	}
	if in := bySlot[0]; in.BusyTask != nil {
		t.Fatalf("slot 0 is still pinned to %q after hello: applyHello 没有调 ReclaimStaleSlots "+
			"（#218 的兜底就靠这一行）", *in.BusyTask)
	}
	if in := bySlot[1]; in.BusyTask == nil || *in.BusyTask != live.ID {
		t.Fatalf("slot 1 busy_task = %v, want %q untouched: 仍在 assigned 且属于本节点的任务不许被抢跑",
			in.BusyTask, live.ID)
	}
	// 空闲槽位从「0 个」变成「1 个」——这才是节点能重新接活的可观测口径。
	free, err := f.db.FreeSlotsByNode(ctx, []string{f.node.ID})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	if free[f.node.ID] != 1 {
		t.Fatalf("free slots = %d, want 1 (只清 stale 的那一个)", free[f.node.ID])
	}
}

// ---------- m10-2a：result 落库后同一 taskID 能立刻改派给另一台节点 ----------

// TestInflightReleasedAfterResult result 落库后 forget 必须**同时**摘掉会话归属与
// hub 的在飞位；只摘一个（或都不摘）会让同一 taskID 永远派不出去。
func TestInflightReleasedAfterResult(t *testing.T) {
	ctx := context.Background()
	f := newNodeFixture(t)
	connA := f.mustDial(t)
	hello(t, connA, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})
	nodeB, connB := f.secondNode(t)

	task := f.makeAssignedTask(t, "trace_inflight_free")
	publishTo(t, f, f.node.ID, task.ID)
	send(t, connA, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked)

	// 跑完：result 落库 → handleResult 里 forget(taskID)。
	send(t, connA, &ResultFrame{Type: NodeFrameResult, TaskID: task.ID,
		Status: store.TaskSucceeded, Summary: "ok", Out: strPtr("fix")})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskSucceeded)

	// ① 在飞位必须被摘掉（forget 的一半：hub.release）。漏了它 → 这条 taskID 永远 ErrTaskInflight。
	if cur := f.handler.hub().owner(task.ID); cur != nil {
		t.Fatalf("task %s is still registered as in-flight after its result landed: "+
			"forget 只做了 disown 没做 release，同一任务将永远派不出去（#222 的护栏变成永久锁）", task.ID)
	}

	// ② 端到端：库里把这条任务重新派给了 B（模拟巡检重派后的行形状），PublishAssign 必须成功。
	//
	// 这里必须把 status 一起写回 assigned：真实重派是 pending → assigned（LeaseAndAssign 的 CAS），
	// 而 PublishAssign 现在会在写帧前回读行状态，**终态行不派发**（审查 n3：给终态任务
	// 登记在飞位/归属是永不清除的内存泄漏）。旧写法只改 node_id/attempts、把行留在
	// succeeded 上——它之所以能跑通，靠的正是“不看行状态”那个缺陷。
	if _, err := f.db.Exec(
		`UPDATE tasks SET node_id = ?, status = ?, attempts = attempts + 1, lease_until = ? WHERE id = ?`,
		nodeB.ID, store.TaskAssigned, store.Now()+store.LeaseTTLSec, task.ID); err != nil {
		t.Fatalf("simulate redispatch to B: %v", err)
	}
	assigned := mustGetTask(t, f, task.ID)
	if err := f.handler.hub().PublishAssign(ctx, f.nsID, nodeB.ID, assigned); err != nil {
		t.Fatalf("PublishAssign to another node after the result landed = %v, want nil "+
			"(在飞位没摘干净 → 这条任务再也派不出去)", err)
	}
	frame := readUntil(t, connB, NodeFrameAssign)
	if frame["task_id"] != task.ID {
		t.Fatalf("B received assign for %v, want %q", frame["task_id"], task.ID)
	}
}

// ---------- m10-2b：连接 clean 结束后同一任务能被改派成功 ----------

// TestCleanEndAllowsRedispatch 连接 end（clean）→ cleanup 把任务交还调度 → 同一条任务
// 必须能被**真实**派发链（LeaseAndAssign + PublishAssign）改派给另一台节点。
//
// 这条钉住 cleanup 里 ④ releaseAll 的必要性：漏了它，任务在库里已 pending、hub 却还
// 记着旧连接，下一次派发撞 ErrTaskInflight 并被退回 pending（抖动到租约过期）。
func TestCleanEndAllowsRedispatch(t *testing.T) {
	ctx := context.Background()
	f := newNodeFixture(t)
	nodeB, connB := f.secondNode(t)

	connA := f.mustDial(t)
	hello(t, connA, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_clean_redispatch")
	publishTo(t, f, f.node.ID, task.ID)
	send(t, connA, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked)

	// 对端正常关闭（1000）：clean → cleanup 回 pending（W3 已覆盖）+ releaseAll。
	if err := connA.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close connection: %v", err)
	}
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskPending)

	// 真实派发链：占槽 + pending→assigned（attempts+1）。
	if _, err := f.db.LeaseAndAssign(ctx, f.nsID, task.ID, nodeB.ID, store.Now()+store.LeaseTTLSec); err != nil {
		t.Fatalf("lease+assign to another node: %v (clean 结束后任务应当立刻可派)", err)
	}
	assigned := mustGetTask(t, f, task.ID)
	if assigned.Attempts < 2 {
		t.Fatalf("attempts = %d after redispatch, want >= 2 (每次派发 +1)", assigned.Attempts)
	}
	if err := f.handler.hub().PublishAssign(ctx, f.nsID, nodeB.ID, assigned); err != nil {
		t.Fatalf("PublishAssign after a clean end = %v, want nil (cleanup 的 releaseAll 没清干净)", err)
	}
	frame := readUntil(t, connB, NodeFrameAssign)
	if frame["task_id"] != task.ID {
		t.Fatalf("B received assign for %v, want %q", frame["task_id"], task.ID)
	}
}

// ---------- m10-3：续租失败 → 连接关闭（且不 panic） ----------

// failingExtendStore 让 ExtendLeasesForTasks 恒失败，其余方法透传给真 store。
type failingExtendStore struct {
	NodeStore
	err error
}

func (s *failingExtendStore) ExtendLeasesForTasks(context.Context, string, string, []string, int64) (int, error) {
	return 0, s.err
}

// TestHeartbeatLeaseFailureClosesConnection 心跳里的续租失败必须终止连接。
//
// 为什么：续租失败意味着「中枢不再认为我在跑这些任务」，继续留着连接会让节点
// 一直跑到租约过期后被巡检当过期任务回收并重派 → 双执行。所以 handleHeartbeat
// 把错误返回给读循环 → 连接关闭 → cleanup 按 unclean 放弃名下任务。
//
// 这条路径此前完全没有用例：注入失败的 store 从没被写出来过。
func TestHeartbeatLeaseFailureClosesConnection(t *testing.T) {
	f := newNodeFixture(t, func(h *NodeHandler) {
		h.Store = &failingExtendStore{NodeStore: h.Store, err: errors.New("simulated store outage")}
	})
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	// 必须有一条「本连接名下」的任务，否则心跳根本不调续租（#218 的口径）。
	task := f.makeAssignedTask(t, "trace_lease_fail")
	publishTo(t, f, f.node.ID, task.ID)
	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked)

	send(t, conn, &HeartbeatFrame{Type: NodeFrameHeartbeat, TS: time.Now().Unix()})

	// 连接最终关闭（服务端 readLoop 因 handleHeartbeat 返回错误而退出）。
	// 若实现里把续租失败当「只 warn」，这里会等到超时 → 红。
	readUntilClosed(t, conn, 5*time.Second)

	// cleanup 真的跑完了：unclean → 名下任务放弃（不重派），节点下线。
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAbandoned)
	waitNodeStatus(t, f, store.NodeStatusOffline)
}

// ---------- M2：旧连接的 cleanup 不许把新连接的 online/alive 改回去 ----------

// gateResolveStore 把「A 的 cleanup 正卡在 ResolveLostTasks（③）里」变成一个**确定事件**。
//
// 为什么需要它：M2 的缺陷是**时序窗口**（① 取 stillCurrent、⑤ 才用，中间隔着 ③ 的
// 每条任务一个事务）。靠「拨号 + hello 恰好在窗口内完成」是概率事件（审查用 18 个
// 任务做到 6/6，2 个任务时 0/12）。把 ③ 钉住再让新连接 hello，就把「窗口」变成
// 测试里的一个同步点：改动前后都能**确定地**跑出各自的结论。
//
// 只有第一次调用会被钉住（其余直接透传），避免测试收尾时别的连接卡在这里。
type gateResolveStore struct {
	NodeStore
	entered chan struct{} // 关：cleanup 已进入 ResolveLostTasks
	release chan struct{} // 关：测试放行
	once    sync.Once
}

func (s *gateResolveStore) ResolveLostTasks(ctx context.Context, namespaceID, nodeID string,
	owners []store.TaskOwner, policy store.LostPolicy, reason string, now int64) (int, int, error) {
	blocked := false
	s.once.Do(func() {
		blocked = true
		close(s.entered)
	})
	if blocked {
		<-s.release
	}
	return s.NodeStore.ResolveLostTasks(ctx, namespaceID, nodeID, owners, policy, reason, now)
}

// TestCleanupInsideWindowKeepsNewConnectionOnline 是 M2 的**主回归用例**：
// 旧连接背着 20 个在飞任务的 cleanup 还没跑完时，新连接（同 token）完成了 hello，
// 旧连接随后的收尾**绝不能**把新连接写好的 online / alive=1 改回去。
//
// 旧实现的后果不是「下次 hello 会自愈」，而是永久卡死：TouchNode 只写 last_seen_at，
// alive=1 全仓只有 hello 的 UpsertInstances 写，FreeSlotsByNode 要 alive=1、
// NodesForCapability 要 online → 节点「显示在线却永远接不到活」= #218 的症状。
func TestCleanupInsideWindowKeepsNewConnectionOnline(t *testing.T) {
	ctx := context.Background()
	const inflight = 20 // 审查给出 18+ 就能稳定复现；20 也是默认 max_instances
	gate := &gateResolveStore{entered: make(chan struct{}), release: make(chan struct{})}
	f := newNodeFixture(t, func(h *NodeHandler) {
		gate.NodeStore = h.Store
		h.Store = gate
	})
	insts, caps := m2Slots(inflight)
	f.handler.LeaseTTLSec = 5

	connA := f.mustDial(t)
	hello(t, connA, f.node.ID, caps, insts)
	tasks := f.inflightTasks(t, connA, inflight)

	// A 的连接自己断了（不是被 B 顶掉）：这才是 M2 的窗口形状——
	// ① Unregister 取到的 true 是真的「我是当前连接」，随后 ③ 才开始。
	_ = connA.CloseNow()

	// ③ 被钉住 = 旧连接的 cleanup 已经越过了「取 stillCurrent」的那一刻。
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup 没能在 5s 内走到 ResolveLostTasks")
	}

	// 窗口内：新连接用同一 token 完成 hello（写 alive=1 + online）。
	connB := f.mustDial(t)
	hello(t, connB, f.node.ID, caps, insts)

	// 放行旧连接的 cleanup 尾声（④ releaseAll；改动前它还会在这里写 node 级状态，
	// 改动后那些写已经在 ③ 之前做完了）。
	close(gate.release)

	// 旧连接名下 20 条任务必须被交还调度。这里只断言「不再在飞」
	// （assigned/acked/running 之外），不断言具体落到 pending 还是 abandoned：
	// 那是**结束原因判据**（M4 的 classifyReadError）管的事，本用例只管 node 级状态，
	// 不为别人的判据背书（否则 M4 的偏差会伪装成 M2 的失败）。
	for _, task := range tasks {
		waitNotInflight(t, f, task.ID)
	}

	// node 级事实必须是**新连接写的那份**。
	// 用有界轮询而不是立即断言：旧实现里 ⑤ 的两次写紧跟在 ④ 之后，需要给它一个
	// 「可能作恶」的窗口（这里 300ms 足够——它只是两条 UPDATE）。
	if !nodeOnlineWithin(f, 300*time.Millisecond) {
		node, _ := f.db.GetNode(ctx, f.nsID, f.node.ID)
		t.Fatalf("node status = %q after the old connection's cleanup, want %q: "+
			"旧连接用 ① 的缓存值把新连接刚写好的 online/alive=1 改回去了（M2 = #218 的「在线却接不到活」且不自愈）",
			node.Status, store.NodeStatusOnline)
	}
	assertNodeServing(t, f, inflight)
}

// TestSupersededCleanupKeepsNewConnectionOnline 是同一缺陷的另一条路径：B **顶掉** A
// （A 还没断，就被 Register 踢掉）。
//
// 这条路径上 ① 本来就返回 false（A 已不在 conns 里），所以改动前后都应当是绿的——
// 它的价值是防止「修 M2 修过头」：如果谁把 node 级写入改成无条件执行，新连接会被
// 旧连接的 cleanup 打成 offline，这里立刻红。
func TestSupersededCleanupKeepsNewConnectionOnline(t *testing.T) {
	ctx := context.Background()
	const inflight = 20
	f := newNodeFixture(t)
	insts, caps := m2Slots(inflight)
	f.handler.LeaseTTLSec = 5

	connA := f.mustDial(t)
	hello(t, connA, f.node.ID, caps, insts)
	tasks := f.inflightTasks(t, connA, inflight)

	// B 顶掉 A。这里不能直接用 hello()：coder/websocket 的 Close 是**握手**
	// （写 close 帧后等对端回帧），而 A 的客户端此刻没人读——Register 里的 kill 会一直
	// 等到握手超时，把 B 的 hello 卡住。真实的 agent 读循环当然在收帧，所以测试也把 A
	// 读完（客户端收到 close 帧会自动回帧），再看 B 的 welcome。
	connB := f.mustDial(t)
	send(t, connB, &HelloFrame{
		Type: NodeFrameHello, NodeID: f.node.ID, Protocol: ProtocolVersion,
		Capabilities: caps, Instances: insts,
	})
	if code := websocket.CloseStatus(readUntilClosed(t, connA, 8*time.Second)); code != websocket.StatusNormalClosure {
		t.Fatalf("superseded connection closed with status %v, want 1000 (换连接不是协议违规)", code)
	}
	readUntil(t, connB, NodeFrameWelcome)

	// 被顶掉 = unclean → 名下任务 abandoned（fencing：旧进程可能还在跑手上的命令）。
	for _, task := range tasks {
		waitStatus(t, f.db, f.nsID, task.ID, store.TaskAbandoned)
	}
	if !nodeOnlineWithin(f, 300*time.Millisecond) {
		node, _ := f.db.GetNode(ctx, f.nsID, f.node.ID)
		t.Fatalf("node status = %q after the superseded connection's cleanup, want %q",
			node.Status, store.NodeStatusOnline)
	}
	assertNodeServing(t, f, inflight)
}

// ---------- M3：PublishAssign 记的世代必须是库里的 attempts ----------

// TestPublishAssignUsesAuthoritativeAttempts attempts≥2 的任务（重试路径）派发后不 ack，
// 看门狗判死 → cleanup 必须能主动释放它（回 pending + 解槽位）。
//
// 旧的失败形状：route 在派发成功后把 task.Attempts 写死成 1
// （internal/route/dispatch.go 的 `task.Attempts = 1`），PublishAssign 拿它当世代登记；
// cleanup 的 ResolveLostTasks 用**库里的** attempts（这里是 2）做 CAS → 永远不匹配
// → 整条主动释放被静默跳过 → 任务停在 assigned、槽位钉到租约过期（20s ack 窗口），
// 也就是「#218 的根修 + #221 的槽位解药」对重试任务退化成旧行为。
func TestPublishAssignUsesAuthoritativeAttempts(t *testing.T) {
	ctx := context.Background()
	f := newNodeFixture(t)
	f.handler.HeartbeatSec = 1 // 判死窗口 = 2s
	f.handler.LeaseTTLSec = 5

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_m3_gen")
	// 推到第 2 代（模拟「第一次派发后执行者死掉，巡检重派」的那一行）。
	if _, err := f.db.Exec(`UPDATE tasks SET attempts = 2 WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("bump attempts: %v", err)
	}
	row := mustGetTask(t, f, task.ID)
	if row.Attempts != 2 {
		t.Fatalf("precondition broken: attempts = %d, want 2", row.Attempts)
	}
	// 正是 route 的做法：派发后用本地变量（Attempts 被硬编码成 1）去 PublishAssign。
	row.Attempts = 1
	if err := f.handler.hub().PublishAssign(ctx, f.nsID, f.node.ID, row); err != nil {
		t.Fatalf("publish assign: %v", err)
	}

	// 不 ack：看门狗判死（unclean）→ cleanup。
	if code := websocket.CloseStatus(readUntilClosed(t, conn, 8*time.Second)); code != websocket.StatusPolicyViolation {
		t.Fatalf("connection closed with status %v, want 1008 (heartbeat timeout)", code)
	}

	// assigned 且 started_at IS NULL（一行没跑）→ 无条件回 pending，与 policy 无关。
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskPending)
	if !isRedispatchable(t, f, task.ID) {
		t.Fatal("released task is not in ListRedispatchable: 它还在 assigned 或带着退避")
	}
	assertSlotFree(t, f, task.ID)
}

// ---------- m5：params 超限要有自己的错误码 ----------

// TestResultOversizedNextHopParamsIsDiagnosable 下一跳子任务的 params 超限 →
// 节点收到 code=params_too_large（而不是 invalid_transition），文案带体积/上限。
//
// 病因（审查 m5）：store 的闸门在 createTasksTx 里，而子任务插入与父任务终态是
// **同一个事务** → 子任务超限会让父任务终态整体回滚。父任务留在 running →
// 租约过期重派 → 节点把同一份昂贵工作再跑一遍 → 返回同样的超限子任务，直到 attempts 用尽。
// 把这种错误伪装成 invalid_transition 就把「你的 params 太大」这个唯一可行动的病因埋了。
func TestResultOversizedNextHopParamsIsDiagnosable(t *testing.T) {
	fake := &fakeNextHop{}
	f := newNodeFixture(t, func(h *NodeHandler) { h.NextHop = fake })
	fake.setDB(f.db)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	parent := f.makeAssignedTask(t, "trace_m5_params")
	child := f.nextHopChild(t, parent, "fix")
	child.Params = bigParams(t, store.MaxTaskParamsBytes+1024) // 刚过闸门 1KiB
	fake.setPlan(child)

	sentResult(t, conn, parent.ID, "fix")

	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeParamsTooLarge {
		t.Fatalf("error code = %v, want %q: params 超限不该伪装成状态机拒绝（病因是体积，不是状态）",
			frame["code"], ErrCodeParamsTooLarge)
	}
	msg, _ := frame["message"].(string)
	for _, want := range []string{
		"params too large",
		fmt.Sprint(len(child.Params)),
		fmt.Sprint(store.MaxTaskParamsBytes),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q (体积与上限都要给，节点才知道该怎么改)", msg, want)
		}
	}
	// 父任务不在终态：整条结果确实被回滚了（这与错误码无关，但它是「节点会重跑」的前提）。
	if got := mustGetTask(t, f, parent.ID); store.IsTerminalStatus(got.Status) {
		t.Fatalf("parent status = %q after a rejected next-hop row, want a non-terminal state", got.Status)
	}
}

// ---------- 测试辅助 ----------

// makePendingTask 建一条无归属的 pending 任务（造「历史钉子」用）。
func (f *nodeFixture) makePendingTask(t *testing.T, traceID string) *store.Task {
	t.Helper()
	ctx := context.Background()
	cap1, err := f.db.GetCapability(ctx, f.nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	task := &store.Task{
		NamespaceID: f.nsID, TraceID: traceID, CapabilityID: cap1.ID,
		Capability: cap1.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: cap1.ContentHash, GraphID: "grf_x",
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create pending task: %v", err)
	}
	return task
}

// instanceSlotOf 返回哪个槽位钉着这条任务（找不到直接失败）。
func instanceSlotOf(t *testing.T, f *nodeFixture, taskID string) int {
	t.Helper()
	insts, err := f.db.ListInstances(context.Background(), f.node.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range insts {
		if in.BusyTask != nil && *in.BusyTask == taskID {
			return in.Slot
		}
	}
	t.Fatalf("no slot is pinned to task %s", taskID)
	return -1
}

// waitNodeStatus 等节点到达期望状态（cleanup 是异步的）。
func waitNodeStatus(t *testing.T, f *nodeFixture, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		got, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
		if err == nil {
			last = got.Status
			if last == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node status = %q, want %q (timeout)", last, want)
}

// m2Slots 造 inflight 个单并发槽位 + 一份 review@1 能力声明（M2 的两个用例共用）。
func m2Slots(inflight int) ([]InstanceDecl, []CapabilityDecl) {
	insts := make([]InstanceDecl, 0, inflight)
	for i := 0; i < inflight; i++ {
		insts = append(insts, InstanceDecl{Slot: i, Concurrency: 1})
	}
	return insts, []CapabilityDecl{{Capability: "review", Version: 1}}
}

// inflightTasks 建 n 条任务、全部派给这条连接并 ack 完（= n 个在飞任务）。
func (f *nodeFixture) inflightTasks(t *testing.T, conn *websocket.Conn, n int) []*store.Task {
	t.Helper()
	tasks := make([]*store.Task, 0, n)
	for i := 0; i < n; i++ {
		tasks = append(tasks, f.makeAssignedTask(t, fmt.Sprintf("trace_m2_%02d", i)))
	}
	for _, task := range tasks {
		publishTo(t, f, f.node.ID, task.ID)
	}
	for _, task := range tasks {
		send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	}
	for _, task := range tasks {
		waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked)
	}
	return tasks
}

// waitNotInflight 等一条任务「不再在飞」：状态离开 assigned/acked/running。
func waitNotInflight(t *testing.T, f *nodeFixture, taskID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		got, err := f.db.GetTask(context.Background(), f.nsID, taskID)
		if err == nil {
			last = got.Status
			switch last {
			case store.TaskAssigned, store.TaskAcked, store.TaskRunning:
			default:
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s is still in flight (status = %q): cleanup 没能把它交还调度", taskID, last)
}

// nodeOnlineWithin 在 d 内轮询「节点是否 online」，返回是否达成。
//
// 用有界轮询而不是立即断言：被测的旧实现会在 cleanup 末尾补写 offline，
// 给它一个可能作恶的窗口才能稳定区分红绿（绿侧是立即成立，不会引入固定等待）。
func nodeOnlineWithin(f *nodeFixture, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		node, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
		if err == nil && node.Status == store.NodeStatusOnline {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertNodeServing 断言节点「真的能接活」的三个机器事实：
// status=online、alive 槽位数=申报数、至少一个空闲槽位。
//
// 单看 status 会漏掉「online 但 alive=0 / 槽位钉死」——那正是 #218 的观测口径。
func assertNodeServing(t *testing.T, f *nodeFixture, wantAlive int) {
	t.Helper()
	ctx := context.Background()
	node, err := f.db.GetNode(ctx, f.nsID, f.node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if node.Status != store.NodeStatusOnline {
		t.Fatalf("node status = %q, want %q", node.Status, store.NodeStatusOnline)
	}
	insts, err := f.db.ListInstances(ctx, f.node.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	alive := 0
	for _, in := range insts {
		if in.Alive {
			alive++
		}
	}
	if alive != wantAlive {
		t.Fatalf("alive instances = %d, want %d (申报数): MarkInstancesDead 把新连接的槽位标掉了", alive, wantAlive)
	}
	free, err := f.db.FreeSlotsByNode(ctx, []string{f.node.ID})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	if free[f.node.ID] <= 0 {
		t.Fatalf("free slots = %d, want > 0: 节点在线但一个槽位都接不了活（alive=0 或 busy_task 钉死）", free[f.node.ID])
	}
	if !f.handler.hub().Online(f.node.ID) {
		t.Fatal("hub 里没有当前连接了：新连接应当还在")
	}
}
