package route

import (
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 派发预算（CONTRACT.md A7）：环合法，兜底是 depth >= max_hops 与 trace 任务数 >= budget。
func chain(t *testing.T, db *store.DB, ns *store.Namespace, traceID string, capability *store.CapabilityVersion, depth int) *store.Task {
	t.Helper()
	parent := ""
	var leaf *store.Task
	for i := 1; i <= depth; i++ {
		task := &store.Task{
			NamespaceID: ns.ID, TraceID: traceID,
			CapabilityID: capability.ID, Capability: capability.Name,
			Version: capability.Version, Digest: capability.Digest,
			Params: json.RawMessage(`{}`),
		}
		if parent != "" {
			p := parent
			task.ParentTaskID = &p
		}
		if err := db.CreateTasks(testCtx(), []*store.Task{task}); err != nil {
			t.Fatalf("建链第 %d 层: %v", i, err)
		}
		parent = task.ID
		leaf = task
	}
	return leaf
}

func declareSlots(t *testing.T, db *store.DB, node *store.Node, count int) {
	t.Helper()
	cfg, err := db.GetNodeConfig(testCtx(), node.ID)
	if err != nil {
		t.Fatalf("读节点配置: %v", err)
	}
	cfg.Limits = map[string]any{"total": count, "perCapability": map[string]any{"*": count}}
	if err := db.PutNodeConfig(testCtx(), cfg); err != nil {
		t.Fatalf("抬机器总量预算: %v", err)
	}
	typed, err := store.TypedMachineLimits(cfg.Limits)
	if err != nil {
		t.Fatalf("typed limits: %v", err)
	}
	hash, err := protocol.MachineConfigHash(cfg.Vars, typed)
	if err != nil {
		t.Fatalf("machine config hash: %v", err)
	}
	if err := db.RecordApplied(testCtx(), node.ID, cfg.Revision, hash); err != nil {
		t.Fatalf("record applied: %v", err)
	}
	specs := make([]store.InstanceSpec, 0, count)
	for i := 0; i < count; i++ {
		specs = append(specs, store.InstanceSpec{Slot: i})
	}
	if _, err := db.UpsertInstances(testCtx(), node.ID, specs); err != nil {
		t.Fatalf("申报槽位: %v", err)
	}
}

func runTask(t *testing.T, db *store.DB, task *store.Task, node *store.Node, tokenHash string, slot int) store.EmitAuth {
	t.Helper()
	assignmentID := "asg_" + task.ID
	if err := db.AssignTaskAt(testCtx(), task.ID, node.ID, assignmentID, 1, store.Now()); err != nil {
		t.Fatalf("记派发候选: %v", err)
	}
	leaseID, attemptToken, _, _, _, err := db.GrantLeaseAuthorized(testCtx(),
		store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: tokenHash},
		task.ID, assignmentID, 1, slot, store.Now())
	if err != nil {
		t.Fatalf("授予租约: %v", err)
	}
	return store.EmitAuth{
		TaskID: task.ID, NodeID: node.ID, NodeTokenHash: tokenHash,
		Attempt: 1, LeaseID: leaseID, AttemptToken: attemptToken, Now: store.Now(),
	}
}

// TestEmitCycleSecondLapWithinBudget ①：a → b → a 能建，第二圈真的跑（预算内）。
func TestEmitCycleSecondLapWithinBudget(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "a-cap", 1, capOpts{Next: []string{"b-cap"}, MaxPerNode: 4})
	testCapability(t, db, ns.ID, "b-cap", 1, capOpts{Next: []string{"a-cap"}, MaxPerNode: 4})
	node, token := testNode(t, db)
	tokenHash := store.HashNodeToken(token)
	declareSlots(t, db, node, 2)
	router := New(db)

	root := chain(t, db, ns, "trace-cycle", mustCapability(t, db, ns.ID, "a-cap"), 1)
	first, err := router.Emit(testCtx(), EmitInput{
		Auth: runTask(t, db, root, node, tokenHash, 0), To: "b-cap", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("第一圈 emit: %v", err)
	}
	if first.Response.TaskID == nil || first.Response.Pruned != nil {
		t.Fatalf("第一圈必须建任务: %+v", first.Response)
	}
	child, err := db.GetTask(testCtx(), ns.ID, *first.Response.TaskID)
	if err != nil {
		t.Fatalf("读子任务: %v", err)
	}

	second, err := router.Emit(testCtx(), EmitInput{
		Auth: runTask(t, db, child, node, tokenHash, 1), To: "a-cap", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("第二圈 emit（回到 a-cap）: %v", err)
	}
	if second.Response.TaskID == nil || second.Response.Pruned != nil {
		t.Fatalf("第二圈必须也建任务（环合法）: %+v", second.Response)
	}
	if second.Response.To != "a-cap" {
		t.Fatalf("第二圈目标 = %q，want a-cap", second.Response.To)
	}
	depth, err := db.AncestorDepth(testCtx(), ns.ID, *second.Response.TaskID)
	if err != nil || depth != 3 {
		t.Fatalf("第二圈任务的深度 = %d（err=%v），want 3", depth, err)
	}
}

// TestEmitMaxHopsPrunesAtBoundary ②：depth >= max_hops 剪 max_hops，且差一格仍放行。
func TestEmitMaxHopsPrunesAtBoundary(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "loop", 1, capOpts{Next: []string{"loop"}, MaxPerNode: 4})
	node, token := testNode(t, db)
	tokenHash := store.HashNodeToken(token)
	declareSlots(t, db, node, 2)
	router := New(db).WithBudget(Budget{MaxHops: 3, TraceTaskBudget: 100})
	loop := mustCapability(t, db, ns.ID, "loop")

	// depth 1 的父任务 emit 出来的是 depth 2（2 < 3）→ 放行。
	shallow := chain(t, db, ns, "trace-hop-ok", loop, 1)
	ok, err := router.Emit(testCtx(), EmitInput{
		Auth: runTask(t, db, shallow, node, tokenHash, 0), To: "loop", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if ok.Response.TaskID == nil || ok.Response.Pruned != nil {
		t.Fatalf("depth 2 < max_hops 3 必须放行: %+v", ok.Response)
	}

	// depth 2 的父任务 emit 出来的是 depth 3 == max_hops → 剪，且不建任务。
	deep := chain(t, db, ns, "trace-hop-prune", loop, 2)
	before, err := db.CountTasksByTrace(testCtx(), ns.ID, "trace-hop-prune")
	if err != nil || before != 2 {
		t.Fatalf("剪枝前 trace 任务数 = %d（err=%v），want 2", before, err)
	}
	pruned, err := router.Emit(testCtx(), EmitInput{
		Auth: runTask(t, db, deep, node, tokenHash, 1), To: "loop", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("超预算的 emit 不是 4xx: %v", err)
	}
	if pruned.Response.Pruned == nil || pruned.Response.Pruned.Reason != protocol.ReasonMaxHops {
		t.Fatalf("剪枝原因 = %+v，want max_hops", pruned.Response.Pruned)
	}
	if pruned.Response.TaskID != nil {
		t.Fatalf("剪枝回执不许带 taskId: %q", *pruned.Response.TaskID)
	}
	if pruned.Response.EventID == "" {
		t.Fatal("剪枝回执必须带真实 eventId")
	}
	after, err := db.CountTasksByTrace(testCtx(), ns.ID, "trace-hop-prune")
	if err != nil || after != before {
		t.Fatalf("剪枝后 trace 任务数 = %d（err=%v），want 仍 %d", after, err, before)
	}
	var traced int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE trace_id = ? AND source = 'route' AND type = ?`,
		"trace-hop-prune", protocol.ReasonMaxHops).Scan(&traced); err != nil {
		t.Fatalf("统计剪枝事件: %v", err)
	}
	if traced != 1 {
		t.Fatalf("max_hops 留痕行数 = %d，want 1", traced)
	}
}

// TestEmitTraceBudgetPrunes ③：同 trace 任务数 >= budget 剪 budget。
func TestEmitTraceBudgetPrunes(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "loop", 1, capOpts{Next: []string{"loop"}, MaxPerNode: 4})
	node, token := testNode(t, db)
	tokenHash := store.HashNodeToken(token)
	declareSlots(t, db, node, 2)
	router := New(db).WithBudget(Budget{MaxHops: 100, TraceTaskBudget: 3})
	loop := mustCapability(t, db, ns.ID, "loop")

	room := chain(t, db, ns, "trace-budget-ok", loop, 2)
	ok, err := router.Emit(testCtx(), EmitInput{
		Auth: runTask(t, db, room, node, tokenHash, 0), To: "loop", Data: json.RawMessage(`{}`),
	})
	if err != nil || ok.Response.TaskID == nil {
		t.Fatalf("2 < budget 3 必须放行: %+v err=%v", ok.Response, err)
	}

	full := chain(t, db, ns, "trace-budget-prune", loop, 3)
	pruned, err := router.Emit(testCtx(), EmitInput{
		Auth: runTask(t, db, full, node, tokenHash, 1), To: "loop", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("超预算的 emit 不是 4xx: %v", err)
	}
	if pruned.Response.Pruned == nil || pruned.Response.Pruned.Reason != protocol.ReasonBudget {
		t.Fatalf("剪枝原因 = %+v，want budget", pruned.Response.Pruned)
	}
	if pruned.Response.TaskID != nil {
		t.Fatalf("剪枝回执不许带 taskId: %q", *pruned.Response.TaskID)
	}
}

// TestBudgetHopWinsAndOnlyOneReason 两者都命中时只留 hop（一次派发一条原因）。
func TestBudgetHopWinsAndOnlyOneReason(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "loop", 1, capOpts{Next: []string{"loop"}, MaxPerNode: 4})
	loop := mustCapability(t, db, ns.ID, "loop")
	parent := chain(t, db, ns, "trace-both", loop, 2)
	router := New(db).WithBudget(Budget{MaxHops: 2, TraceTaskBudget: 1})

	plan, err := router.PlanFromEmit(testCtx(), parent, "loop", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("PlanFromEmit: %v", err)
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Reason != protocol.ReasonMaxHops {
		t.Fatalf("两个预算都命中时只留 hop，得到 %+v", plan.Pruned)
	}
	if len(plan.Ready()) != 0 || len(plan.Targets) != 0 {
		t.Fatalf("剪枝计划不许留 target: ready=%d targets=%d", len(plan.Ready()), len(plan.Targets))
	}
}

// TestBudgetDoesNotPruneLegalChain ④ 反例：只走一圈、深度远小于预算的链不许被剪（防「同名就剪」复活）。
func TestBudgetDoesNotPruneLegalChain(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "review", 1, capOpts{Next: []string{"fix"}, MaxPerNode: 4})
	testCapability(t, db, ns.ID, "fix", 1, capOpts{Next: []string{"review"}, MaxPerNode: 4})
	node, token := testNode(t, db)
	tokenHash := store.HashNodeToken(token)
	declareSlots(t, db, node, 2)
	router := New(db)

	root := chain(t, db, ns, "trace-once", mustCapability(t, db, ns.ID, "review"), 1)
	first, err := router.Emit(testCtx(), EmitInput{
		Auth: runTask(t, db, root, node, tokenHash, 0), To: "fix", Data: json.RawMessage(`{}`),
	})
	if err != nil || first.Response.TaskID == nil {
		t.Fatalf("第一跳必须建任务: %+v err=%v", first.Response, err)
	}
	child, err := db.GetTask(testCtx(), ns.ID, *first.Response.TaskID)
	if err != nil {
		t.Fatalf("读子任务: %v", err)
	}
	// 环回到 review（第二圈）默认预算下必放行。
	second, err := router.Emit(testCtx(), EmitInput{
		Auth: runTask(t, db, child, node, tokenHash, 1), To: "review", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("回到 review 的 emit: %v", err)
	}
	if second.Response.Pruned != nil || second.Response.TaskID == nil {
		t.Fatalf("合法环不许被剪: %+v", second.Response)
	}
}

// TestPlanFromOutcomeUsesSameBudget 判决结局是第三条建任务路径，也走同一份判定。
func TestPlanFromOutcomeUsesSameBudget(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "loop", 1, capOpts{Next: []string{"loop"}, MaxPerNode: 4})
	loop := mustCapability(t, db, ns.ID, "loop")
	parent := chain(t, db, ns, "trace-outcome-budget", loop, 2)
	parent.Outcomes = json.RawMessage(`{"done":[{"capability":"loop"}]}`)
	router := New(db).WithBudget(Budget{MaxHops: 2, TraceTaskBudget: 100})

	plan, err := router.PlanFromOutcome(testCtx(), parent, &store.Decision{Outcome: "done"})
	if err != nil {
		t.Fatalf("PlanFromOutcome: %v", err)
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Reason != protocol.ReasonMaxHops {
		t.Fatalf("结局派发也要判预算，得到 %+v", plan.Pruned)
	}
}

func mustCapability(t *testing.T, db *store.DB, namespaceID, name string) *store.CapabilityVersion {
	t.Helper()
	c, err := db.GetCapability(testCtx(), namespaceID, name, 1)
	if err != nil {
		t.Fatalf("读能力 %s: %v", name, err)
	}
	return c
}
