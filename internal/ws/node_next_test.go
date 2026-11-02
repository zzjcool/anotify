package ws

// #140 的验收测试：接力派发不再丢任务。
//
// 旧实现：handleResult 先 ReportResult 提交终态，下一跳整段在后台 goroutine 里
// （查出口表 + 建行 + 推帧）。进程在「终态已提交」与「子任务行已建」之间退出，
// 就留下父任务 succeeded + out=fix、子任务行不存在的**永久空洞**：
// ListRedispatchable 只捞已存在的 pending 行，捞不到一条不存在的行 → 链永久停，
// trace 看起来一切正常。
//
// 新实现的三条硬性质，本文件的用例逐条钉住：
//  1. 子任务行与父任务终态**同一个事务**（要么都在，要么都不在）；
//  2. 事务提交后即使 publish 失败/丢失，行仍是 pending + node_id NULL + attempts=0，
//     ListRedispatchable 能捞到它（巡检兜底重派的机器事实）；
//  3. publish 失败**不回给节点**（父任务已成功），NextHop == nil 的老路径零漂移。

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 测试双：并发安全的 NextHopDispatcher ----------

// fakeNextHop 是 NextHopDispatcher 的测试双。
//
// 并发安全是硬要求：PublishTasks 由 handleResult 里的后台 goroutine 调用，
// 与测试主 goroutine 的断言并发；所有字段都过 mu（含 db，测试在建连前注入）。
type fakeNextHop struct {
	mu sync.Mutex

	// db 可选：PublishTasks 用它证明「行已先落库，publish 是后手」。
	db *store.DB

	planTasks  []*store.Task
	planErr    error
	publishErr error

	planCalls   int
	planTrace   string
	planParent  string
	planFromCap string
	planOut     string
	// planNamespace 是 PlanNextHop 收到的作用域参数：必须是**父任务的命名空间**
	// （#162 之前这里传的是 org，出口表会恒 0 行）。
	planNamespace string
	publishCall   []fakePublish
}

// fakePublish 是一次 PublishTasks 调用的快照（在 goroutine 里填，断言侧读）。
type fakePublish struct {
	// namespaceID 是 PublishTasks 收到的作用域参数（#162：派发作用域是命名空间）。
	namespaceID string
	taskIDs     []string
	// rowsInDB 是「publish 被调用的那一刻，这些 ID 已在库里」的条数。
	// 它是「先 commit 后 publish」的直接证据（不是靠调用顺序猜的）。
	rowsInDB int
}

func (f *fakeNextHop) setDB(db *store.DB) {
	f.mu.Lock()
	f.db = db
	f.mu.Unlock()
}

func (f *fakeNextHop) setPlan(tasks ...*store.Task) {
	f.mu.Lock()
	f.planTasks = tasks
	f.mu.Unlock()
}

func (f *fakeNextHop) PlanNextHop(_ context.Context, namespaceID, traceID, parentTaskID,
	fromCapabilityID, out string, _ json.RawMessage, _ []string) ([]*store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planCalls++
	f.planNamespace = namespaceID
	f.planTrace, f.planParent, f.planFromCap, f.planOut = traceID, parentTaskID, fromCapabilityID, out
	if f.planErr != nil {
		return nil, f.planErr
	}
	return f.planTasks, nil
}

func (f *fakeNextHop) PublishTasks(ctx context.Context, namespaceID string, tasks []*store.Task) error {
	f.mu.Lock()
	db, pubErr := f.db, f.publishErr
	f.mu.Unlock()

	// DB 查询放在锁外：它是被测性质本身（行是否已提交），不是 fake 的状态。
	call := fakePublish{namespaceID: namespaceID}
	for _, t := range tasks {
		call.taskIDs = append(call.taskIDs, t.ID)
		if db != nil {
			if _, err := db.GetTask(ctx, namespaceID, t.ID); err == nil {
				call.rowsInDB++
			}
		}
	}

	f.mu.Lock()
	f.publishCall = append(f.publishCall, call)
	f.mu.Unlock()
	return pubErr
}

// snapshot 返回 (plan 调用次数, publish 调用快照)。
func (f *fakeNextHop) snapshot() (int, []fakePublish) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.planCalls, append([]fakePublish(nil), f.publishCall...)
}

// waitPublishes 等 publish 被调用到 want 次（异步，需轮询）。
func (f *fakeNextHop) waitPublishes(t *testing.T, want int) []fakePublish {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, pubs := f.snapshot(); len(pubs) >= want {
			return pubs
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, pubs := f.snapshot()
	t.Fatalf("PublishTasks calls = %d, want %d (timeout)", len(pubs), want)
	return nil
}

// ---------- 夹具补充：造一条「plan 出来的下一跳子任务」 ----------

// nextHopChild 造一条与 route.PlanNextHop 产出一致形状的子任务：
// pending、ID 已生成、parent/trace/path 都接上父任务（尚未落库）。
func (f *nodeFixture) nextHopChild(t *testing.T, parent *store.Task, out string) *store.Task {
	t.Helper()
	cap1, err := f.db.GetCapability(context.Background(), f.nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	parentID := parent.ID
	return &store.Task{
		ID:           store.NewTaskID(),
		NamespaceID:  f.nsID,
		TraceID:      parent.TraceID,
		ParentTaskID: &parentID,
		Path:         append(append([]string{}, parent.Path...), out),
		CapabilityID: cap1.ID,
		Capability:   cap1.Name,
		Version:      1,
		Params:       json.RawMessage(`{"next":true}`),
		Status:       store.TaskPending,
		CapHash:      cap1.ContentHash,
		GraphID:      "grf_x",
	}
}

// sentResult 发一帧「成功 + 出口 out」的 result。
func sentResult(t *testing.T, conn *websocket.Conn, taskID, out string) {
	t.Helper()
	send(t, conn, &ResultFrame{
		Type: NodeFrameResult, TaskID: taskID, Status: store.TaskSucceeded,
		Summary: "ok", Out: strPtr(out),
	})
}

// ---------- 必测 1：崩溃点回归（issue #140 的验收核心） ----------

// TestResultPersistsNextHopRowBeforePublish 事务提交后 publish 失败/丢失
// （模拟「行已建、publish 丢失/进程退出」）→ 子任务行必须已经在库里、
// 形状可被巡检捞回，父任务仍是 succeeded（不因子任务派发失败而回退）。
func TestResultPersistsNextHopRowBeforePublish(t *testing.T) {
	ctx := context.Background()
	fake := &fakeNextHop{publishErr: errors.New("publish lost (simulated: process exited right after commit)")}
	f := newNodeFixture(t, func(h *NodeHandler) { h.NextHop = fake })
	fake.setDB(f.db)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	parent := f.makeAssignedTask(t, "trace_140_crash")
	child := f.nextHopChild(t, parent, "fix")
	fake.setPlan(child)

	sentResult(t, conn, parent.ID, "fix")

	// ① 父任务照常成功：下一跳 publish 失败不能让它回退/变失败。
	waitStatus(t, f.db, f.nsID, parent.ID, store.TaskSucceeded)

	// ② 子任务行存在（旧实现在这里就是 #140 的空洞：行根本不存在）。
	got, err := f.db.GetTask(ctx, f.nsID, child.ID)
	if err != nil {
		t.Fatalf("next-hop row missing after result — this is the #140 permanent hole: %v", err)
	}
	if got.Status != store.TaskPending {
		t.Errorf("child status = %q, want %q", got.Status, store.TaskPending)
	}
	if got.NodeID != nil {
		t.Errorf("child node_id = %v, want NULL", *got.NodeID)
	}
	if got.Attempts != 0 {
		t.Errorf("child attempts = %d, want 0 (publish never leased it)", got.Attempts)
	}
	if got.ParentTaskID == nil || *got.ParentTaskID != parent.ID {
		t.Errorf("child parent_task_id = %v, want %s", got.ParentTaskID, parent.ID)
	}
	if got.TraceID != parent.TraceID {
		t.Errorf("child trace_id = %q, want %q", got.TraceID, parent.TraceID)
	}
	// 子任务必须继承父任务的命名空间（I11④）；组织层已删除，没有更粗的归属可比。
	if got.NamespaceID != parent.NamespaceID {
		t.Errorf("child namespace_id = %q, want %q（必须继承父任务）", got.NamespaceID, parent.NamespaceID)
	}

	// ③ 补偿路径的机器事实：巡检（store.ListRedispatchable → lease sweeper →
	// Dispatcher.RedispatchTask）真的能捞到它，不是口头承诺。
	rows, err := f.db.ListRedispatchable(ctx, store.Now(), 100)
	if err != nil {
		t.Fatalf("list redispatchable: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == child.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("child %s not in ListRedispatchable (%d rows) — sweeper could never recover it",
			child.ID, len(rows))
	}

	// ④ publish 确实被调过，且拿到的是那条**已落库**的行；失败只 warn。
	pubs := fake.waitPublishes(t, 1)
	if len(pubs[0].taskIDs) != 1 || pubs[0].taskIDs[0] != child.ID {
		t.Fatalf("published task IDs = %v, want [%s]", pubs[0].taskIDs, child.ID)
	}
	if pubs[0].namespaceID != f.nsID {
		t.Errorf("publish namespace = %q, want %q", pubs[0].namespaceID, f.nsID)
	}
	if pubs[0].rowsInDB != 1 {
		t.Errorf("row was not committed before publish: %d/%d visible", pubs[0].rowsInDB, len(pubs[0].taskIDs))
	}

	// ⑤ 节点收不到任何帧（原意：下一跳的失败不该回给节点 ⇒ 不能有 error 帧；子任务既然
	// 没派出去 ⇒ 也不能有 assign 帧）。主语与改前一致 = 任何帧。
	// 放在最后：coder/websocket 的读超时会关掉这条客户端连接（conn.go:188-199），
	// 之后的断言都不能再用 conn。
	// （若这一条偶发不稳，把它去掉也不削弱 ②③ 的结论——那三条才是 #140 的验收。）
	assertNoFrame(t, conn, 500*time.Millisecond, "下一跳 publish 失败不该回给节点")
}

// ---------- 必测 2：正常路径「同生共死」 ----------

// TestResultNextHopChildIsInSameTransaction publish 用的必须是**已落库**的行：
// 断言 publish 被调时行已在库里，且收到的 ID 与库里新出现的子任务行完全一致。
func TestResultNextHopChildIsInSameTransaction(t *testing.T) {
	ctx := context.Background()
	fake := &fakeNextHop{}
	f := newNodeFixture(t, func(h *NodeHandler) { h.NextHop = fake })
	fake.setDB(f.db)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	parent := f.makeAssignedTask(t, "trace_140_same_tx")
	child := f.nextHopChild(t, parent, "fix")
	fake.setPlan(child)

	sentResult(t, conn, parent.ID, "fix")
	waitStatus(t, f.db, f.nsID, parent.ID, store.TaskSucceeded)

	pubs := fake.waitPublishes(t, 1)

	// ① 先 commit 后 publish：publish 跑的时候行已经在库里。
	if len(pubs[0].taskIDs) != 1 {
		t.Fatalf("published %d tasks, want 1: %v", len(pubs[0].taskIDs), pubs[0].taskIDs)
	}
	if pubs[0].rowsInDB != 1 {
		t.Fatalf("publish saw %d/%d rows in DB — the child row was not committed before publish",
			pubs[0].rowsInDB, len(pubs[0].taskIDs))
	}

	// ② 库里这条 trace 现在正好 2 行（父 + 1 个子），子任务行 ID == publish 收到的 ID。
	all, err := f.db.ListTasks(ctx, f.nsID, parent.TraceID, "", "", 50)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	var childIDs []string
	for _, tk := range all {
		if tk.ID != parent.ID {
			childIDs = append(childIDs, tk.ID)
		}
	}
	if len(childIDs) != 1 {
		t.Fatalf("trace has %d next-hop rows (%v), want exactly 1", len(childIDs), childIDs)
	}
	if childIDs[0] != pubs[0].taskIDs[0] {
		t.Fatalf("publish used task ID %s but the committed row is %s — publish is not operating on the tx row",
			pubs[0].taskIDs[0], childIDs[0])
	}

	// ③ plan 拿到的上下文是父任务那一份（trace/parent/from-cap/out 都接对了）。
	fake.mu.Lock()
	gotTrace, gotParent, gotFromCap, gotOut := fake.planTrace, fake.planParent, fake.planFromCap, fake.planOut
	gotPlanNS := fake.planNamespace
	fake.mu.Unlock()
	if gotTrace != parent.TraceID || gotParent != parent.ID || gotFromCap != parent.CapabilityID || gotOut != "fix" {
		t.Fatalf("plan args = (trace=%q, parent=%q, fromCap=%q, out=%q), want (%q, %q, %q, %q)",
			gotTrace, gotParent, gotFromCap, gotOut, parent.TraceID, parent.ID, parent.CapabilityID, "fix")
	}
	// plan 的作用域是**父任务的命名空间**（#162）：传 org 的话 EdgesFrom 恒 0 行，
	// 下一跳永远不会被建出来。
	if gotPlanNS != parent.NamespaceID || gotPlanNS != f.nsID {
		t.Fatalf("plan namespace = %q, want the parent task's namespace %q (fixture ns %q)",
			gotPlanNS, parent.NamespaceID, f.nsID)
	}
}

// ---------- 必测 3：子任务插入失败 → 父任务不终态 ----------

// TestResultRollsBackWhenNextHopRowRejected 子任务行被 store 拒绝（建任务必须落在
// pending）→ 整个事务回滚：父任务不在终态、trace 里仍只有 1 行、
// 绝不能 publish 一条库里不存在的任务。
func TestResultRollsBackWhenNextHopRowRejected(t *testing.T) {
	ctx := context.Background()
	fake := &fakeNextHop{}
	f := newNodeFixture(t, func(h *NodeHandler) { h.NextHop = fake })
	fake.setDB(f.db)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	parent := f.makeAssignedTask(t, "trace_140_rollback")
	bad := f.nextHopChild(t, parent, "fix")
	bad.Status = store.TaskAssigned // 非法：建任务只能落在 pending
	fake.setPlan(bad)

	sentResult(t, conn, parent.ID, "fix")

	// ① 节点收到一帧 error（这次结果没能落库）。
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeInvalidTransition {
		t.Errorf("error code = %v, want %q", frame["code"], ErrCodeInvalidTransition)
	}

	// ② 父任务不在终态（回滚；事务里的 assigned→running 也一起回滚了）。
	got, err := f.db.GetTask(ctx, f.nsID, parent.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if store.IsTerminalStatus(got.Status) {
		t.Fatalf("parent status = %q after a rejected next-hop row — the terminal write was not rolled back", got.Status)
	}

	// ③ 库里这条 trace 仍只有 1 行（子任务没建出来）。
	all, err := f.db.ListTasks(ctx, f.nsID, parent.TraceID, "", "", 50)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("trace has %d rows after rollback, want 1 (only the parent)", len(all))
	}

	// ④ PublishTasks 从未被调用：不能推一条库里不存在的任务。
	// 不需要 sleep：上游已经 readUntil 到 error 帧 ⇒ 服务端这一帧的 handleResult
	// 已经走完 rolled-back 分支并返回（error 帧就是它里面写出来的），而 publishNext
	// 只在 ReportResultAndCreateTasks **提交成功之后**才被调用（node_session.go:260）。
	// 原先的 time.Sleep(200ms) 是空窗口：goroutine 慢了就是假绿，删掉是严格增强。
	if planCalls, pubs := fake.snapshot(); len(pubs) != 0 {
		t.Fatalf("PublishTasks was called %d time(s) (%v) for a row that never committed", len(pubs), pubs[0].taskIDs)
	} else if planCalls != 1 {
		t.Errorf("PlanNextHop calls = %d, want 1", planCalls)
	}
}

// ---------- 必测 4：NextHop == nil 的老路径零漂移 ----------

// TestResultWithoutNextHopUnchanged 不设 NextHop：result 帧照旧推进终态、
// 不建子任务、不报错（防止 #140 的接线把默认路径弄坏）。
func TestResultWithoutNextHopUnchanged(t *testing.T) {
	ctx := context.Background()
	f := newNodeFixture(t) // 故意不设 NextHop

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	parent := f.makeAssignedTask(t, "trace_140_no_next_hop")
	sentResult(t, conn, parent.ID, "fix")
	waitStatus(t, f.db, f.nsID, parent.ID, store.TaskSucceeded)

	// ① 结果字段照旧落库（reported + out）。
	got, err := f.db.GetTask(ctx, f.nsID, parent.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !got.Reported || got.Out == nil || *got.Out != "fix" {
		t.Fatalf("result not recorded: reported=%v out=%v", got.Reported, got.Out)
	}

	// ② 不建子任务：这条 trace 只有父任务一行。
	all, err := f.db.ListTasks(ctx, f.nsID, parent.TraceID, "", "", 50)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("trace has %d rows without NextHop, want 1", len(all))
	}

	// ③ 一个帧都没有（原意：老路径既不报错、也不再派活）。主语与改前一致 = 任何帧。
	// 放最后：读超时会关掉这条客户端连接（conn.go:188-199）。
	assertNoFrame(t, conn, 500*time.Millisecond, "没设 NextHop 时不许再有下行帧")
}

// ---------- 补充：plan 失败 → 不提交终态（契约 §1.3 第 3 步） ----------

// TestResultPlanFailureLeavesNoTerminalState 计划阶段就失败（DB 故障）→ 不提交
// 终态、回错误帧、不 publish。这是「可恢复」的那一侧：父任务仍 running，
// 租约到期后巡检退回 pending 重派；绝不能变成「succeeded 且没有子任务行」。
func TestResultPlanFailureLeavesNoTerminalState(t *testing.T) {
	ctx := context.Background()
	fake := &fakeNextHop{planErr: errors.New("db is gone")}
	f := newNodeFixture(t, func(h *NodeHandler) { h.NextHop = fake })
	fake.setDB(f.db)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	parent := f.makeAssignedTask(t, "trace_140_plan_fail")
	sentResult(t, conn, parent.ID, "fix")

	// ① 收到 error 帧。
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeInvalidTransition {
		t.Errorf("error code = %v, want %q", frame["code"], ErrCodeInvalidTransition)
	}

	// ② 父任务不在终态（plan 失败 → 连结果都没落库）。
	got, err := f.db.GetTask(ctx, f.nsID, parent.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if store.IsTerminalStatus(got.Status) {
		t.Fatalf("parent status = %q, want a non-terminal state (plan failed, nothing may be committed)", got.Status)
	}

	// ③ 一条 publish 都没有（没计划就没有行，也就没有可推的东西）。
	// 不需要 sleep：① 已 readUntil 到 error 帧 ⇒ handleResult 的 plan 失败分支
	// 已经返回（没有提交，也就没有行），而 publishNext 只在提交成功后调用
	// （node_session.go:260）。原先的 time.Sleep(200ms) 只能假绿。
	if _, pubs := fake.snapshot(); len(pubs) != 0 {
		t.Fatalf("PublishTasks called %d time(s) despite a failed plan", len(pubs))
	}
}
