package route

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 #140 的 route 侧验收：把「接力派发」拆成 plan（只读）+ publish（事务外）。
//
// 为什么必须这么拆（而不是继续用一条 DispatchNext 全做完）：
// 下一跳子任务行要和父任务的终态写在**同一个事务**里，否则进程在
// 「终态已提交、子任务行还没建」之间退出，就是一条永远捞不回来的空洞
// （父任务 succeeded，子任务行不存在，ListRedispatchable 只捞 pending 行）。
// 而 publish 是网络操作，不能进事务。于是：
//
//	PlanNextHop  = 只读算计划（不落库/不占槽位/不推帧）
//	事务          = 父任务终态 + 子任务行（pending，node_id NULL）
//	PublishTasks = 挑机器/占槽位/推帧；失败退回 pending，由租约巡检兜底重派
//
// 这里钉住三件事：plan 真的只读；publish 真的派；publish 失败真的退回可重派。

// fakePublisher 记录 assign 帧推送，可按节点注入失败。
type fakePublisher struct {
	mu sync.Mutex
	// calls 按调用顺序记录，供「失败也没拦住其它条」的断言
	calls []publishCall
	// errByNode 让指定节点的推送失败（模拟节点掉线）
	errByNode map[string]error
	// errAll 让全部推送失败
	errAll error
}

type publishCall struct {
	namespaceID string
	nodeID      string
	taskID      string
}

func (p *fakePublisher) PublishAssign(ctx context.Context, namespaceID, nodeID string, task *store.Task) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, publishCall{namespaceID: namespaceID, nodeID: nodeID, taskID: task.ID})
	if p.errAll != nil {
		return p.errAll
	}
	if err, ok := p.errByNode[nodeID]; ok {
		return err
	}
	return nil
}

func (p *fakePublisher) called() []publishCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]publishCall, len(p.calls))
	copy(out, p.calls)
	return out
}

// setupNextHop 构造：review --fix--> fix，fix 由 nd_fix（2 槽位）声明。
func setupNextHop(t *testing.T) (*fakeStore, *Dispatcher, *store.CapabilityVersion, *store.CapabilityVersion) {
	t.Helper()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	target := f.addCap(testNS, "fix", 1, nil, `{"type":"object"}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 2)
	return f, New(f), review, target
}

// mustPlanNextHop 跑一次 PlanNextHop 并要求恰好 1 个任务。
func mustPlanNextHop(t *testing.T, d *Dispatcher, fromCapID, traceID string) []*store.Task {
	t.Helper()
	tasks, err := d.PlanNextHop(context.Background(), testNS, traceID, "tsk_parent",
		fromCapID, "fix", json.RawMessage(`{"k":"v"}`), []string{fromCapID})
	if err != nil {
		t.Fatalf("plan next hop: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("plan next hop returned %d tasks, want 1", len(tasks))
	}
	return tasks
}

// assertRedispatchable 断言「store.ListRedispatchable 能捞到它」。
//
// fake 没有 SQL，所以直接钉住 ListRedispatchable 的三个条件
// （status='pending' AND node_id IS NULL AND not_before 已到）：
// 任何一条不成立，任务就会永久卡住——那正是 #140 要消灭的状态。
func assertRedispatchable(t *testing.T, task *store.Task) {
	t.Helper()
	if task.Status != store.TaskPending {
		t.Errorf("task %s status = %q, want pending (ListRedispatchable 只捞 pending)", task.ID, task.Status)
	}
	if task.NodeID != nil {
		t.Errorf("task %s node_id = %q, want NULL (有归属的行不会被重派)", task.ID, *task.NodeID)
	}
	if task.NotBefore != nil {
		t.Errorf("task %s not_before = %d, want NULL (退避会让它晚些才被捞起)", task.ID, *task.NotBefore)
	}
}

// TestPlanNextHopIsReadOnly plan 不得落库、不得占槽位、不得推帧，但任务字段要齐全。
func TestPlanNextHopIsReadOnly(t *testing.T) {
	f, d, review, target := setupNextHop(t)
	pub := &fakePublisher{}
	d.Publisher = pub
	now := int64(1730000000)
	d.Now = func() int64 { return now }
	// 比「事后数行数」更硬：plan 若偷偷建行，这里立刻报错。
	f.errCreateTasks = errors.New("plan must not create task rows")

	tasks, err := d.PlanNextHop(context.Background(), testNS, "trace_plan", "tsk_parent",
		review.ID, "fix", json.RawMessage(`{"k":"v"}`), []string{review.ID})
	if err != nil {
		t.Fatalf("plan next hop: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("plan returned %d tasks, want 1", len(tasks))
	}
	task := tasks[0]

	// 只读：一行没建、一个槽位没占、一帧没推
	if len(f.createdTasks) != 0 {
		t.Errorf("plan created %d task rows, want 0", len(f.createdTasks))
	}
	if len(f.assignments) != 0 {
		t.Errorf("plan assigned %d tasks, want 0", len(f.assignments))
	}
	if len(f.leasedSlots) != 0 {
		t.Errorf("plan leased %d slots, want 0", len(f.leasedSlots))
	}
	if calls := pub.called(); len(calls) != 0 {
		t.Errorf("plan pushed %d assign frames, want 0", len(calls))
	}

	// 任务字段：pending、ID 已生成、parent/path/params/capability/graph 都对
	if task.Status != store.TaskPending {
		t.Errorf("task status = %q, want pending (还没落库派发)", task.Status)
	}
	if !strings.HasPrefix(task.ID, "tsk_") {
		t.Errorf("task id = %q, want tsk_ prefix (plan 里就该生成 ID)", task.ID)
	}
	if task.NamespaceID != testNS || task.TraceID != "trace_plan" {
		t.Errorf("task namespace/trace = %q/%q, want %q/trace_plan", task.NamespaceID, task.TraceID, testNS)
	}
	// ⑤ 下一跳子任务继承**父任务的命名空间**（调用方只能把父任务行上的值递进来）
	if task.NamespaceID != testNS {
		t.Errorf("task namespace_id = %q, want %q（子任务必须继承父任务的命名空间）",
			task.NamespaceID, testNS)
	}
	if task.ParentTaskID == nil || *task.ParentTaskID != "tsk_parent" {
		t.Errorf("parent_task_id = %v, want tsk_parent", task.ParentTaskID)
	}
	if len(task.Path) != 2 || task.Path[0] != review.ID || task.Path[1] != target.ID {
		t.Errorf("path = %v, want [%s %s]", task.Path, review.ID, target.ID)
	}
	if task.CapabilityID != target.ID || task.Capability != target.Name || task.Version != target.Version {
		t.Errorf("capability = %s/%s@%d, want %s/%s@%d",
			task.CapabilityID, task.Capability, task.Version,
			target.ID, target.Name, target.Version)
	}
	if task.CapHash != target.ContentHash {
		t.Errorf("cap_hash = %q, want %q (版本钉住)", task.CapHash, target.ContentHash)
	}
	if task.GraphID != "grf_fake" {
		t.Errorf("graph_id = %q, want grf_fake", task.GraphID)
	}
	var got map[string]any
	if err := json.Unmarshal(task.Params, &got); err != nil {
		t.Fatalf("params %s 不是合法 JSON: %v", task.Params, err)
	}
	if got["k"] != "v" {
		t.Errorf("params = %v, want {k:v}（上游 params 原样传递）", got)
	}
}

// TestPlanNextHopNotDispatched 三种「不派发」的情况：返回空列表 + 不报错。
//
// 三者都不是错误（契约 §4/§7）：链到头、没回报出口、成环都是正常终止，
// 若它们变成 error，ws 层会把它当成派发失败并反压节点。
func TestPlanNextHopNotDispatched(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, d *Dispatcher, review, target *store.CapabilityVersion) ([]*store.Task, error)
		// wantPrune 是期望留痕的剪枝原因（"" = 不该产生剪枝记录）
		wantPrune PruneReason
	}{
		{
			name: "out 为空：不派发、不剪枝、连出口表都不查",
			run: func(t *testing.T, d *Dispatcher, review, target *store.CapabilityVersion) ([]*store.Task, error) {
				return d.PlanNextHop(context.Background(), testNS, "trace_none", "tsk_parent",
					review.ID, "", json.RawMessage(`{}`), []string{review.ID})
			},
			wantPrune: "",
		},
		{
			name: "无出口边：pruned/no_edge",
			run: func(t *testing.T, d *Dispatcher, review, target *store.CapabilityVersion) ([]*store.Task, error) {
				return d.PlanNextHop(context.Background(), testNS, "trace_none", "tsk_parent",
					review.ID, "nobody-listens", json.RawMessage(`{}`), []string{review.ID})
			},
			wantPrune: ReasonNoEdge,
		},
		{
			name: "成环：目标已在 path 里 → pruned/path",
			run: func(t *testing.T, d *Dispatcher, review, target *store.CapabilityVersion) ([]*store.Task, error) {
				return d.PlanNextHop(context.Background(), testNS, "trace_none", "tsk_parent",
					review.ID, "fix", json.RawMessage(`{}`), []string{review.ID, target.ID})
			},
			wantPrune: ReasonPath,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, d, review, target := setupNextHop(t)
			tasks, err := c.run(t, d, review, target)
			if err != nil {
				t.Fatalf("不派发不是错误，却返回 %v", err)
			}
			if len(tasks) != 0 {
				t.Fatalf("returned %d tasks, want 0", len(tasks))
			}
			if len(f.createdTasks) != 0 {
				t.Fatalf("created %d rows, want 0", len(f.createdTasks))
			}
			events := pruneEventsFor(f, "trace_none")
			if c.wantPrune == "" {
				if len(events) != 0 {
					t.Fatalf("out 为空不该产生剪枝，got %d events", len(events))
				}
				if f.graphCalls != 0 {
					t.Errorf("out 为空还落了一次图版本（%d calls）", f.graphCalls)
				}
				return
			}
			if len(events) != 1 || events[0].Type != string(PruneEventType(c.wantPrune)) {
				t.Fatalf("want 1 prune event %q, got %v", PruneEventType(c.wantPrune), events)
			}
		})
	}
}

// TestPlanNextHopUserScopedNotPruned 下一跳只递父任务的命名空间时，
// 用户维度校验（checkUserScoped）也不能把真实用户的推送误剪成 foreign_user。
//
// 历史：这条用例原来叫 TestPlanNextHopResolvesOrg，守的是「下一跳拿不到 org，
// 必须从命名空间反查，否则 IsOrgMember 拿空 org 去查 → 静默剪成 foreign_user」。
// #204 删掉组织层后「反查 org」不存在了，但**同一类静默失败**仍然可能发生：
// checkUserScoped 若拿不到/查不到用户，就会把这条链在 notify 这步剪断
// （正是 #142 要消灭的那种「查不到的静默失败」）。所以这条用例保留，
// 断言改为：任务建出来、命名空间是父任务那个、且没有任何剪枝留痕。
func TestPlanNextHopUserScopedNotPruned(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"notify"}, `{"type":"object"}`)
	phone := f.addCap(testNS, store.NotifyPhoneCapability.Name, store.NotifyPhoneCapability.Version,
		nil, store.NotifyPhoneCapability.ParamsSchema)
	f.addEdge(testNS, review.ID, "notify", phone.ID)
	// 中枢能力没有节点执行者（notify.phone 由 executor 领取），不需要 addNode

	d := New(f)
	tasks, err := d.PlanNextHop(context.Background(), testNS, "trace_org", "tsk_parent",
		review.ID, "notify", json.RawMessage(`{"to":"member_1","title":"hi"}`), []string{review.ID})
	if err != nil {
		t.Fatalf("plan next hop: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("plan returned %d tasks, want 1（to 被误判成 foreign_user → 链路在 notify 断掉）", len(tasks))
	}
	if got := tasks[0]; got.NamespaceID != testNS {
		t.Fatalf("task namespace = %q, want %q（子任务必须继承父任务的命名空间）", got.NamespaceID, testNS)
	}
	if events := pruneEventsFor(f, "trace_org"); len(events) != 0 {
		t.Fatalf("不该有剪枝留痕: %+v", events)
	}
}

// TestPublishTasksAssignsPendingTask 正常路径：已落库的 pending 任务被占槽位并推出 assign 帧。
func TestPublishTasksAssignsPendingTask(t *testing.T) {
	f, d, review, _ := setupNextHop(t)
	pub := &fakePublisher{}
	d.Publisher = pub
	now := int64(1730000000)
	d.Now = func() int64 { return now }

	tasks := mustPlanNextHop(t, d, review.ID, "trace_pub")
	// 「已落库」由调用方的事务完成（ws 里是 ReportResultAndCreateTasks）；
	// 这里用 fake 的 CreateTasks 模拟那一行已经在库里。
	if err := f.CreateTasks(context.Background(), tasks); err != nil {
		t.Fatalf("seed task rows: %v", err)
	}

	if err := d.PublishTasks(context.Background(), testNS, tasks); err != nil {
		t.Fatalf("publish tasks: %v", err)
	}

	task := tasks[0]
	if task.Status != store.TaskAssigned {
		t.Fatalf("task status = %q, want assigned", task.Status)
	}
	if task.NodeID == nil || *task.NodeID != "nd_fix" {
		t.Fatalf("node_id = %v, want nd_fix", task.NodeID)
	}
	if task.InstanceID == nil || *task.InstanceID == "" {
		t.Fatal("instance_id 为空：槽位没占上")
	}
	if task.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (LeaseAndAssign 记一次派发)", task.Attempts)
	}
	if task.LeaseUntil == nil || *task.LeaseUntil != now+store.AckWaitSec {
		t.Fatalf("lease_until = %v, want %d (ack 窗口)", task.LeaseUntil, now+store.AckWaitSec)
	}

	calls := pub.called()
	if len(calls) != 1 {
		t.Fatalf("pushed %d assign frames, want 1", len(calls))
	}
	// #162：推帧按**命名空间**路由（hub 只推给同命名空间的连接）。
	if calls[0].taskID != task.ID || calls[0].nodeID != "nd_fix" || calls[0].namespaceID != testNS {
		t.Fatalf("assign frame 推给了 %+v, want task %s → nd_fix/%s", calls[0], task.ID, testNS)
	}

	// 绝不建行：库里仍然只有 plan 出来的那一行
	if len(f.createdTasks) != 1 {
		t.Fatalf("tasks 表里 %d 行, want 1（PublishTasks 不能建行）", len(f.createdTasks))
	}
	if len(f.assignments) != 1 {
		t.Fatalf("assignments = %d, want 1", len(f.assignments))
	}
}

// TestPublishTasksFailureRequeuesAndContinues
// publish 失败 → 该任务退回 pending 且不带归属（可被 ListRedispatchable 捞回），
// 返回错误但不中断其它任务。
func TestPublishTasksFailureRequeuesAndContinues(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	a := f.addCap(testNS, "fixA", 1, nil, `{"type":"object"}`)
	b := f.addCap(testNS, "fixB", 1, nil, `{"type":"object"}`)
	f.addEdge(testNS, review.ID, "fix", a.ID)
	f.addEdge(testNS, review.ID, "fix", b.ID)
	f.addNode("nd_a", a.ID, 1)
	f.addNode("nd_b", b.ID, 1)

	d := New(f)
	// 只有 nd_a 掉线：b 那条必须照常派出去（逐条独立）
	pub := &fakePublisher{errByNode: map[string]error{"nd_a": errors.New("node offline")}}
	d.Publisher = pub
	now := int64(1730000000)
	d.Now = func() int64 { return now }

	tasks, err := d.PlanNextHop(context.Background(), testNS, "trace_fail", "tsk_parent",
		review.ID, "fix", json.RawMessage(`{}`), []string{review.ID})
	if err != nil {
		t.Fatalf("plan next hop: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("plan returned %d tasks, want 2", len(tasks))
	}
	if err := f.CreateTasks(context.Background(), tasks); err != nil {
		t.Fatalf("seed task rows: %v", err)
	}

	// 不 panic、不中断，但要把第一个错误交回调用方记日志
	if err := d.PublishTasks(context.Background(), testNS, tasks); err == nil {
		t.Fatal("publish 失败却返回 nil（调用方就记不到日志了）")
	}
	if calls := pub.called(); len(calls) != 2 {
		t.Fatalf("pushed %d frames, want 2（一条失败不能拦住另一条）", len(calls))
	} else if calls[0].nodeID != "nd_a" || calls[1].nodeID != "nd_b" {
		t.Fatalf("assign 帧顺序 = %v, want [nd_a nd_b]（先失败后成功，说明没中断）", calls)
	}

	var failed, ok *store.Task
	for _, task := range tasks {
		switch task.Capability {
		case "fixA":
			failed = task
		case "fixB":
			ok = task
		}
	}
	if failed == nil || ok == nil {
		t.Fatalf("plan 没给出两条任务: %v", tasks)
	}

	// 失败那条：退回 pending + 没有归属 + 没有租约 → 租约巡检/sweeper 能重派
	assertRedispatchable(t, failed)
	if failed.InstanceID != nil || failed.LeaseUntil != nil {
		t.Errorf("失败的 task 还挂着 instance/lease: %v / %v", failed.InstanceID, failed.LeaseUntil)
	}
	// 槽位已释放（否则该节点永远缺一个槽）；成功那条的槽位还占着
	if len(f.leasedSlots) != 1 {
		t.Errorf("leased slots = %d, want 1（失败那条的槽位没释放？）: %v", len(f.leasedSlots), f.leasedSlots)
	}
	for _, in := range f.instances["nd_a"] {
		if in.BusyTask != nil {
			t.Errorf("nd_a 的槽位 %s 仍被 %s 占着（publish 失败没释放）", in.ID, *in.BusyTask)
		}
	}

	// 成功那条：assigned 且帧推出去了
	if ok.Status != store.TaskAssigned {
		t.Errorf("另一条 task status = %q, want assigned（不该被连累）", ok.Status)
	}
	if ok.NodeID == nil || *ok.NodeID != "nd_b" {
		t.Errorf("另一条 node_id = %v, want nd_b", ok.NodeID)
	}
}

// TestPublishTasksSkipsCentralCapability 中枢能力（notify.phone）不找节点、不推帧、不报错。
//
// 它的执行者是 internal/executor（从 pending 里 ClaimCentralTasks），
// 所以「跳过 publish、任务留 pending」是正确终态，不是失败。
func TestPublishTasksSkipsCentralCapability(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"notify"}, `{"type":"object"}`)
	phone := f.addCap(testNS, store.NotifyPhoneCapability.Name, store.NotifyPhoneCapability.Version,
		nil, store.NotifyPhoneCapability.ParamsSchema)
	f.addEdge(testNS, review.ID, "notify", phone.ID)
	// 刻意不给 notify.phone 加节点：中枢能力本来就没有节点执行者

	d := New(f)
	pub := &fakePublisher{}
	d.Publisher = pub

	tasks, err := d.PlanNextHop(context.Background(), testNS, "trace_central", "tsk_parent",
		review.ID, "notify", json.RawMessage(`{"to":"member_1","title":"hi"}`), []string{review.ID})
	if err != nil {
		t.Fatalf("plan next hop: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("plan returned %d tasks, want 1（中枢能力不需要节点）", len(tasks))
	}
	if tasks[0].Capability != store.NotifyPhoneCapability.Name {
		t.Fatalf("capability = %q, want %q", tasks[0].Capability, store.NotifyPhoneCapability.Name)
	}
	if err := f.CreateTasks(context.Background(), tasks); err != nil {
		t.Fatalf("seed task rows: %v", err)
	}

	if err := d.PublishTasks(context.Background(), testNS, tasks); err != nil {
		t.Fatalf("中枢能力不算失败，却返回 %v", err)
	}

	task := tasks[0]
	assertRedispatchable(t, task) // pending + 无归属：等 executor 领取
	if task.InstanceID != nil || task.LeaseUntil != nil {
		t.Errorf("中枢任务被占了槽位/租约: %v / %v", task.InstanceID, task.LeaseUntil)
	}
	if len(f.assignments) != 0 || len(f.leasedSlots) != 0 {
		t.Errorf("中枢任务不该占槽位: assignments=%d slots=%d", len(f.assignments), len(f.leasedSlots))
	}
	if calls := pub.called(); len(calls) != 0 {
		t.Errorf("中枢任务不该推 assign 帧，got %v", calls)
	}
}

// TestPublishTasksWritesStickyPin 接力派发必须写粘性 pin（#140 回归 M1）。
//
// 回归背景：接力派发从 `DispatchNext → Dispatch → launch` 改成
// `PlanNextHop + PublishTasks → RedispatchTask` 之后，pickNode 返回的
// stickySelector 被丢掉了 —— 于是**接力链上永远不写 pin**。
// 而 pickNode 只在「库里还没有 pin」时才返回 selector（已钉住返回 nil），
// 所以 hop≥2 的粘性再也不会被钉住：第二次派发（含巡检重派）按负载换机器，
// 同一 trace 的同一 worktree 落到另一台节点上 —— 对 sticky="worktree" 这类
// 能力是**结果可能不正确**，不只是慢。
//
// 这里钉住四件事：
//  1. 第一次 publish 后恰好 1 条 pin，key/selector/node 都对（node 是实际派发的那台）；
//  2. 巡检重派（租约过期 → sweep → 再 publish）后仍然只有 1 条 pin，且仍派到同一台；
//  3. 没有 pin 的话第二次本该换机器（用例刻意构造成这样，否则断言没意义）；
//  4. 推帧失败的路径不写 pin（见 TestPublishTasksPublishFailureWritesNoPin）。
func TestPublishTasksWritesStickyPin(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	target := f.addCap(testNS, "fix", 1, nil, `{"type":"object"}`)
	target.Sticky = "worktree" // sticky 字段是 params 里的键名
	f.addEdge(testNS, review.ID, "fix", target.ID)
	// 两台节点各 2 个槽位：首跳并列 → 取候选表第一台 nd_a。
	// nd_a 被占走一个槽后 free(nd_a)=1 < free(nd_b)=2，**没有 pin 的话**
	// 第二次必然挑 nd_b —— 所以「第二次还在 nd_a」是 pin 生效的硬证据。
	f.addNode("nd_a", target.ID, 2)
	f.addNode("nd_b", target.ID, 2)

	d := New(f)
	pub := &fakePublisher{}
	d.Publisher = pub
	now := int64(1730000000)
	d.Now = func() int64 { return now }

	const traceID = "trace_sticky_hop2"
	key := store.StickyKey(traceID, target.ID, "wt-1")

	tasks, err := d.PlanNextHop(context.Background(), testNS, traceID, "tsk_parent",
		review.ID, "fix", json.RawMessage(`{"worktree":"wt-1"}`), []string{review.ID})
	if err != nil {
		t.Fatalf("plan next hop: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("plan returned %d tasks, want 1", len(tasks))
	}
	if err := f.CreateTasks(context.Background(), tasks); err != nil {
		t.Fatalf("seed task rows: %v", err)
	}

	// ---- 首跳 ----------------
	if err := d.PublishTasks(context.Background(), testNS, tasks); err != nil {
		t.Fatalf("publish tasks: %v", err)
	}
	task := tasks[0]
	if task.NodeID == nil {
		t.Fatal("首跳没派出去（node_id 为空）")
	}
	dispatched := *task.NodeID
	if dispatched != "nd_a" {
		t.Fatalf("首跳派到 %q, want nd_a（空载并列取候选表第一台）", dispatched)
	}

	if len(f.pins) != 1 {
		t.Fatalf("pins = %v, want 恰好 1 条（接力派发必须写粘性 pin，M1 回归）", keysOf(f.pins))
	}
	pin, ok := f.pins[key]
	if !ok {
		t.Fatalf("pin key = StickyKey(trace, cap, wt-1) 缺失；have %v", keysOf(f.pins))
	}
	if pin.NodeID != dispatched {
		t.Errorf("pin.node_id = %q, want %q（钉住的必须是实际派发的那台）", pin.NodeID, dispatched)
	}
	if pin.Selector != "wt-1" {
		t.Errorf("pin.selector = %q, want wt-1", pin.Selector)
	}
	if pin.NamespaceID != testNS {
		t.Errorf("pin.namespace_id = %q, want %q（pin 必须记住它属于哪个命名空间）", pin.NamespaceID, testNS)
	}

	// ---- 巡检重派同一任务：租约过期 → sweep 退回 pending → 再 publish ----
	// 先钉住「下面那个断言有说服力」的前提：此刻 nd_b 比 nd_a 更空，
	// 所以如果 pin 写了但没被 pickNode 采用，重派就会挑 nd_b。
	free, err := f.FreeSlotsByNode(context.Background(), []string{"nd_a", "nd_b"})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	if free["nd_a"] >= free["nd_b"] {
		t.Fatalf("用例前提不成立: free(nd_a)=%d, free(nd_b)=%d（nd_a 必须更紧，否则换机器不会发生）",
			free["nd_a"], free["nd_b"])
	}
	if err := f.ExtendLease(context.Background(), testNS, task.ID, 0); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, err := f.SweepExpiredLeases(context.Background(), now+store.AckWaitSec+1); err != nil {
		t.Fatalf("sweep expired leases: %v", err)
	}
	assertRedispatchable(t, task) // 确认真的回到了「可被重派」的形状

	if err := d.PublishTasks(context.Background(), testNS, tasks); err != nil {
		t.Fatalf("republish task: %v", err)
	}

	// 仍派到同一台：这是 pin 起作用（否则 nd_b 更空，会被挑走）
	if task.NodeID == nil || *task.NodeID != dispatched {
		t.Fatalf("重派派到 %v, want %q（pin 没生效 → hop≥2 的 sticky 静默失效）", task.NodeID, dispatched)
	}
	// 仍然只有 1 条 pin（已钉住时 pickNode 返回 nil selector，只做 last_used_at 刷新）
	if len(f.pins) != 1 {
		t.Fatalf("重派后 pins = %v, want 仍只有 1 条", keysOf(f.pins))
	}
	if again, ok := f.pins[key]; !ok || again.NodeID != dispatched || again.Selector != "wt-1" {
		t.Errorf("重派后 pin = %+v，want 同一 key/selector/node %q", again, dispatched)
	}

	// 两次 assign 帧都推给同一台
	calls := pub.called()
	if len(calls) != 2 {
		t.Fatalf("pushed %d frames, want 2", len(calls))
	}
	for i, c := range calls {
		if c.nodeID != dispatched {
			t.Errorf("第 %d 帧推给 %q, want %q（粘性必须粘住）", i+1, c.nodeID, dispatched)
		}
	}
}

// TestPublishTasksPublishFailureWritesNoPin 推帧失败的任务**不**能留 pin。
//
// 失败那条会被 requeueAfterPublishFailure 退回 pending 等重派；此时若写了 pin，
// 下一轮 pickNode 会把它又钉回一台刚刚推不出去的机器上（粘性优先于负载）。
// 后半段是正向对照：修好推帧之后同一条任务**要**写 pin ——
// 证明上面「没 pin」确实是因为失败路径，而不是这段逻辑根本不写。
func TestPublishTasksPublishFailureWritesNoPin(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	target := f.addCap(testNS, "fix", 1, nil, `{"type":"object"}`)
	target.Sticky = "worktree"
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 2)

	d := New(f)
	pub := &fakePublisher{errByNode: map[string]error{"nd_fix": errors.New("node offline")}}
	d.Publisher = pub
	d.Now = func() int64 { return 1730000000 }

	const traceID = "trace_sticky_fail"
	tasks, err := d.PlanNextHop(context.Background(), testNS, traceID, "tsk_parent",
		review.ID, "fix", json.RawMessage(`{"worktree":"wt-2"}`), []string{review.ID})
	if err != nil {
		t.Fatalf("plan next hop: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("plan returned %d tasks, want 1", len(tasks))
	}
	if err := f.CreateTasks(context.Background(), tasks); err != nil {
		t.Fatalf("seed task rows: %v", err)
	}

	if err := d.PublishTasks(context.Background(), testNS, tasks); err == nil {
		t.Fatal("publish 失败却返回 nil（调用方就记不到日志了）")
	}
	if calls := pub.called(); len(calls) != 1 || calls[0].nodeID != "nd_fix" {
		t.Fatalf("assign 帧 = %v, want 一次推给 nd_fix（确认走的就是失败路径）", calls)
	}
	assertRedispatchable(t, tasks[0]) // 已退回 pending、无归属：等重派
	if len(f.pins) != 0 {
		t.Fatalf("推帧失败还写了 %d 条 pin: %v（它还在 pending，钉住会把重派锁死在同一台推不出去的机器上）",
			len(f.pins), keysOf(f.pins))
	}

	// 正向对照：推帧恢复后重派成功 → 这时才写 pin
	d.Publisher = &fakePublisher{}
	if err := d.PublishTasks(context.Background(), testNS, tasks); err != nil {
		t.Fatalf("republish task: %v", err)
	}
	key := store.StickyKey(traceID, target.ID, "wt-2")
	if len(f.pins) != 1 {
		t.Fatalf("pins = %v, want 恰好 1 条（成功派发后必须钉住）", keysOf(f.pins))
	}
	if pin, ok := f.pins[key]; !ok || pin.NodeID != "nd_fix" || pin.Selector != "wt-2" {
		t.Errorf("pin = %+v, want nd_fix/wt-2", pin)
	}
}
