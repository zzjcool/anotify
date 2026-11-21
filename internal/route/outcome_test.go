package route

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

type routerFixture struct {
	db     *store.DB
	ns     *store.Namespace
	router *store.CapabilityVersion
	closeC *store.CapabilityVersion
	notify *store.CapabilityVersion
	triage *store.CapabilityVersion
	node   *store.Node
	hash   string
	source *store.Source
}

func newRouterFixture(t *testing.T) *routerFixture {
	t.Helper()
	db := testDB(t)
	ns := testNamespace(t, db)
	router := testCapability(t, db, ns.ID, "issue-router", 1, capOpts{
		Next: []string{"issue-close", "issue-notify", "issue-triage"},
	})
	closeC := testCapability(t, db, ns.ID, "issue-close", 1, capOpts{
		Params: `{"type":"object","required":["reason"],"properties":{"reason":{"type":"string"},"title":{"type":"string"}}}`,
	})
	notify := testCapability(t, db, ns.ID, "issue-notify", 1, capOpts{
		Params: `{"type":"object","properties":{"channel":{"type":"string"}}}`,
	})
	triage := testCapability(t, db, ns.ID, "issue-triage", 1, capOpts{
		Params: `{"type":"object","properties":{"title":{"type":"string"}}}`,
	})
	node, token := testNode(t, db)
	return &routerFixture{
		db: db, ns: ns, router: router, closeC: closeC, notify: notify, triage: triage,
		node: node, hash: store.HashNodeToken(token),
		source: &store.Source{
			NamespaceID: ns.ID, Name: "gh", Kind: "webhook", Enabled: true,
			Config: json.RawMessage(`{}`),
			Targets: []sourcespec.Target{{
				Capability: "issue-router",
				Params:     map[string]any{"event": "$(payload)"},
				Outcomes: map[string][]sourcespec.OutcomeAction{
					"close": {
						{Capability: "issue-close", Params: map[string]any{
							"reason": "$(decision.reason)",
							"title":  "$(payload.issue.title)",
						}},
						{Capability: "issue-notify", Params: map[string]any{"channel": "#mod"}},
					},
					"triage": {{Capability: "issue-triage", Params: map[string]any{
						"title": "$(payload.issue.title)",
					}}},
					"drop": {},
				},
			}},
		},
	}
}

// seedRouterTask 走真实入口造一个 running 的 router 任务，返回任务行与决策凭据。
func (f *routerFixture) seedRouterTask(t *testing.T, payload map[string]any) (*store.Task, string) {
	t.Helper()
	ctx := testCtx()
	plan, err := New(f.db).PlanFromSource(ctx, f.source, "trace-router", "issues.opened", payload)
	if err != nil {
		t.Fatalf("计划: %v", err)
	}
	tasks := plan.Tasks("")
	if len(tasks) != 1 {
		t.Fatalf("应建 1 个 router 任务，得到 %d", len(tasks))
	}
	if err := f.db.CreateTasks(ctx, tasks); err != nil {
		t.Fatalf("建任务: %v", err)
	}
	task := tasks[0]
	if _, err := f.db.UpsertInstances(ctx, f.node.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("申报槽位: %v", err)
	}
	now := store.Now()
	if err := f.db.AssignTaskAt(ctx, task.ID, f.node.ID, "asg_router", 1, now); err != nil {
		t.Fatalf("记候选: %v", err)
	}
	_, _, decisionToken, _, _, err := f.db.GrantLeaseAuthorized(ctx,
		store.NodeAuthorization{NodeID: f.node.ID, NodeTokenHash: f.hash},
		task.ID, "asg_router", 1, 0, now)
	if err != nil {
		t.Fatalf("授予租约: %v", err)
	}
	return task, decisionToken
}

// finish 把任务置成指定终态（结局执行只读相位）。
func finish(t *testing.T, db *store.DB, taskID, phase string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE tasks SET phase = ?, finished_at = ? WHERE id = ?`, phase, store.Now(), taskID); err != nil {
		t.Fatalf("置终态 %s: %v", phase, err)
	}
}

func traceTasks(t *testing.T, db *store.DB, nsID, traceID string) []*store.Task {
	t.Helper()
	tasks, err := db.ListTasksByTrace(testCtx(), nsID, traceID)
	if err != nil {
		t.Fatalf("读 trace 任务: %v", err)
	}
	return tasks
}

func traceEventTypes(t *testing.T, db *store.DB, nsID, traceID string) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT type FROM events WHERE namespace_id = ? AND trace_id = ? AND source = ?`,
		nsID, traceID, store.EventPruneSource)
	if err != nil {
		t.Fatalf("读 route 事件: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[typ]++
	}
	return out
}

func TestPlanFromSourceRendersOutcomesOnce(t *testing.T) {
	f := newRouterFixture(t)
	payload := map[string]any{"issue": map[string]any{"title": "spam"}}
	plan, err := New(f.db).PlanFromSource(testCtx(), f.source, "trace-render", "issues.opened", payload)
	if err != nil {
		t.Fatalf("计划: %v", err)
	}
	target := plan.Targets[0]
	if target.State != StateReady {
		t.Fatalf("target 应 ready：%+v", target)
	}
	var outcomes map[string][]sourcespec.OutcomeAction
	if err := json.Unmarshal(target.Outcomes, &outcomes); err != nil {
		t.Fatalf("outcomes 不是 JSON：%v（%s）", err, target.Outcomes)
	}
	closeActions := outcomes["close"]
	if len(closeActions) != 2 {
		t.Fatalf("close 应有 2 个动作：%+v", closeActions)
	}
	if closeActions[0].Params["title"] != "spam" {
		t.Fatalf("payload 引用应在建任务时求值：%+v", closeActions[0].Params)
	}
	if closeActions[0].Params["reason"] != "$(decision.reason)" {
		t.Fatalf("decision 引用必须留到判决执行：%+v", closeActions[0].Params)
	}
	if actions, ok := outcomes["drop"]; !ok || len(actions) != 0 {
		t.Fatalf("drop: [] 必须保留成「已声明但空动作」：%+v", outcomes)
	}
	tasks := plan.Tasks("")
	if string(tasks[0].Outcomes) != string(target.Outcomes) {
		t.Fatalf("任务行的 outcomes 与计划不一致：\n%s\n%s", tasks[0].Outcomes, target.Outcomes)
	}
	plan, err = New(f.db).PlanFromSource(testCtx(), f.source, "trace-missing", "issues.opened", map[string]any{})
	if err != nil {
		t.Fatalf("计划: %v", err)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("缺 payload 引用应剪枝成 param_missing：%+v", plan.Targets)
	}
	if len(plan.Tasks("")) != 0 {
		t.Fatal("剪枝不该建任务")
	}
}

func TestDispatchOutcomeRunsOnlyChosenOutcome(t *testing.T) {
	f := newRouterFixture(t)
	ctx := testCtx()
	payload := map[string]any{"issue": map[string]any{"title": "spam"}}
	task, decisionToken := f.seedRouterTask(t, payload)
	if _, err := f.db.RecordDecision(ctx, store.DecisionInput{
		TaskID: task.ID, Token: decisionToken, Outcome: "close",
		Params: map[string]any{"reason": "广告"},
	}); err != nil {
		t.Fatalf("判决应被接受: %v", err)
	}
	finish(t, f.db, task.ID, store.TaskSucceeded)
	if err := New(f.db).DispatchOutcome(ctx, mustTask(t, f.db, task.ID)); err != nil {
		t.Fatalf("结局执行: %v", err)
	}

	tasks := traceTasks(t, f.db, f.ns.ID, task.TraceID)
	if len(tasks) != 3 { // router + close 的 2 个动作
		t.Fatalf("trace 任务数 = %d，want 3（router + 2 个动作，triage 不许派）", len(tasks))
	}
	byCap := map[string]*store.Task{}
	for _, got := range tasks {
		byCap[got.Capability] = got
	}
	if _, ok := byCap["issue-triage"]; ok {
		t.Fatal("没被选中的结局不能派发（群发就是这里出的错）")
	}
	closeTask, ok := byCap["issue-close"]
	if !ok {
		t.Fatalf("close 的第一个动作没派：%v", keys(byCap))
	}
	if closeTask.ParentTaskID == nil || *closeTask.ParentTaskID != task.ID {
		t.Fatalf("子任务必须挂到 router 任务上：%v", closeTask.ParentTaskID)
	}
	var params map[string]any
	if err := json.Unmarshal(closeTask.Params, &params); err != nil {
		t.Fatalf("子任务参数不是 JSON：%v", err)
	}
	if params["reason"] != "广告" || params["title"] != "spam" {
		t.Fatalf("动作参数两段求值不对：%v", params)
	}
	notifyTask, ok := byCap["issue-notify"]
	if !ok || string(notifyTask.Params) != `{"channel":"#mod"}` {
		t.Fatalf("常量动作参数不对：%v %s", ok, notifyTask.Params)
	}
}

func TestDispatchOutcomeNoDecisionRecordsTrail(t *testing.T) {
	f := newRouterFixture(t)
	task, _ := f.seedRouterTask(t, map[string]any{"issue": map[string]any{"title": "x"}})
	finish(t, f.db, task.ID, store.TaskSucceeded)
	if err := New(f.db).DispatchOutcome(testCtx(), mustTask(t, f.db, task.ID)); err != nil {
		t.Fatalf("结局执行: %v", err)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, task.TraceID)); got != 1 {
		t.Fatalf("无判决不该建任务：trace 任务数 %d", got)
	}
	if got := traceEventTypes(t, f.db, f.ns.ID, task.TraceID)[protocol.ReasonNoDecision]; got != 1 {
		t.Fatalf("必须留一条 no_decision：%v", traceEventTypes(t, f.db, f.ns.ID, task.TraceID))
	}
}

func TestDispatchOutcomeFailedRecordsRouterFailed(t *testing.T) {
	f := newRouterFixture(t)
	task, decisionToken := f.seedRouterTask(t, map[string]any{"issue": map[string]any{"title": "x"}})
	if _, err := f.db.RecordDecision(testCtx(), store.DecisionInput{
		TaskID: task.ID, Token: decisionToken, Outcome: "close", Params: map[string]any{"reason": "r"},
	}); err != nil {
		t.Fatalf("判决应被接受: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE tasks SET phase = ?, error = ? WHERE id = ?`,
		store.TaskFailed, protocol.ReasonScriptFailed, task.ID); err != nil {
		t.Fatalf("置失败: %v", err)
	}
	if err := New(f.db).DispatchOutcome(testCtx(), mustTask(t, f.db, task.ID)); err != nil {
		t.Fatalf("结局执行: %v", err)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, task.TraceID)); got != 1 {
		t.Fatalf("失败任务不该派发结局：trace 任务数 %d", got)
	}
	events := traceEventTypes(t, f.db, f.ns.ID, task.TraceID)
	if events[protocol.ReasonRouterFailed] != 1 {
		t.Fatalf("必须留一条 router_failed：%v", events)
	}
}

func TestDispatchOutcomeDropRecordsTrail(t *testing.T) {
	f := newRouterFixture(t)
	task, decisionToken := f.seedRouterTask(t, map[string]any{"issue": map[string]any{"title": "x"}})
	if _, err := f.db.RecordDecision(testCtx(), store.DecisionInput{
		TaskID: task.ID, Token: decisionToken, Outcome: "drop", Params: map[string]any{"reason": "非 issue"},
	}); err != nil {
		t.Fatalf("判决应被接受: %v", err)
	}
	finish(t, f.db, task.ID, store.TaskSucceeded)
	if err := New(f.db).DispatchOutcome(testCtx(), mustTask(t, f.db, task.ID)); err != nil {
		t.Fatalf("结局执行: %v", err)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, task.TraceID)); got != 1 {
		t.Fatalf("空动作结局不该建任务：trace 任务数 %d", got)
	}
	if got := traceEventTypes(t, f.db, f.ns.ID, task.TraceID)[protocol.ReasonOutcomeDropped]; got != 1 {
		t.Fatalf("必须留一条 outcome_dropped：%v", traceEventTypes(t, f.db, f.ns.ID, task.TraceID))
	}
	var payload string
	if err := f.db.QueryRow(`SELECT payload FROM events WHERE namespace_id = ? AND trace_id = ? AND type = ?`,
		f.ns.ID, task.TraceID, protocol.ReasonOutcomeDropped).Scan(&payload); err != nil {
		t.Fatalf("读留痕: %v", err)
	}
	var fact map[string]any
	if err := json.Unmarshal([]byte(payload), &fact); err != nil {
		t.Fatalf("留痕 payload: %v", err)
	}
	if fact["outcome"] != "drop" {
		t.Fatalf("留痕必须点名结局（reason 可读）：%v", fact)
	}
}

func TestDispatchOutcomeMissingDownstreamKeepsSemantics(t *testing.T) {
	f := newRouterFixture(t)
	task, decisionToken := f.seedRouterTask(t, map[string]any{"issue": map[string]any{"title": "x"}})
	if _, err := f.db.Exec(`UPDATE tasks SET outcomes = ? WHERE id = ?`,
		`{"close":[{"capability":"ghost"},{"capability":"issue-notify"}]}`, task.ID); err != nil {
		t.Fatalf("改 outcomes: %v", err)
	}
	if _, err := f.db.RecordDecision(testCtx(), store.DecisionInput{
		TaskID: task.ID, Token: decisionToken, Outcome: "close",
	}); err != nil {
		t.Fatalf("判决应被接受: %v", err)
	}
	finish(t, f.db, task.ID, store.TaskSucceeded)
	if err := New(f.db).DispatchOutcome(testCtx(), mustTask(t, f.db, task.ID)); err != nil {
		t.Fatalf("结局执行: %v", err)
	}
	tasks := traceTasks(t, f.db, f.ns.ID, task.TraceID)
	var failed, ok int
	for _, got := range tasks {
		switch {
		case got.Phase == store.TaskFailed && got.Capability == "ghost":
			if got.Error != protocol.ReasonDownstreamMissing {
				t.Fatalf("缺失下游的失败原因 = %q，want downstream_missing", got.Error)
			}
			failed++
		case got.Capability == "issue-notify":
			ok++
		}
	}
	if failed != 1 || ok != 1 {
		t.Fatalf("应 1 条失败 + 1 条正常，得到 failed=%d ok=%d（%v）", failed, ok, tasks)
	}
}

func TestDispatchOutcomePrunesMissingDecisionParam(t *testing.T) {
	f := newRouterFixture(t)
	task, decisionToken := f.seedRouterTask(t, map[string]any{"issue": map[string]any{"title": "x"}})
	if _, err := f.db.RecordDecision(testCtx(), store.DecisionInput{
		TaskID: task.ID, Token: decisionToken, Outcome: "close", // 没给 reason
	}); err != nil {
		t.Fatalf("判决应被接受: %v", err)
	}
	finish(t, f.db, task.ID, store.TaskSucceeded)
	if err := New(f.db).DispatchOutcome(testCtx(), mustTask(t, f.db, task.ID)); err != nil {
		t.Fatalf("结局执行: %v", err)
	}
	tasks := traceTasks(t, f.db, f.ns.ID, task.TraceID)
	if len(tasks) != 2 { // router + issue-notify（第一个动作被剪枝）
		t.Fatalf("缺 decision 参数的动作应剪枝、另一个照派：%v", tasks)
	}
	events := traceEventTypes(t, f.db, f.ns.ID, task.TraceID)
	if events[protocol.ReasonParamMissing] != 1 {
		t.Fatalf("必须留一条 param_missing 剪枝痕：%v", events)
	}
}

func TestDispatchOutcomeUndeclaredOutcomeFailsClosed(t *testing.T) {
	f := newRouterFixture(t)
	task, _ := f.seedRouterTask(t, map[string]any{"issue": map[string]any{"title": "x"}})
	if _, err := f.db.Exec(`UPDATE tasks SET decision = ? WHERE id = ?`,
		`{"outcome":"bogus","at":1}`, task.ID); err != nil {
		t.Fatalf("写坏判决: %v", err)
	}
	finish(t, f.db, task.ID, store.TaskSucceeded)
	err := New(f.db).DispatchOutcome(testCtx(), mustTask(t, f.db, task.ID))
	if !errors.Is(err, ErrOutcomeNotDeclaredExec) {
		t.Fatalf("应 fail closed（ErrOutcomeNotDeclaredExec），得到 %v", err)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, task.TraceID)); got != 1 {
		t.Fatalf("fail closed 不该建任务：%d", got)
	}
}

func TestDispatchOutcomeIgnoresNonRouterTask(t *testing.T) {
	f := newRouterFixture(t)
	plain := testPendingTask(t, f.db, f.ns.ID, "trace-plain", f.closeC)
	finish(t, f.db, plain.ID, store.TaskSucceeded)
	if err := New(f.db).DispatchOutcome(testCtx(), mustTask(t, f.db, plain.ID)); err != nil {
		t.Fatalf("普通任务应 no-op: %v", err)
	}
	if got := len(traceTasks(t, f.db, f.ns.ID, "trace-plain")); got != 1 {
		t.Fatalf("普通任务不该被派发任何东西：%d", got)
	}
}

func mustTask(t *testing.T, db *store.DB, taskID string) *store.Task {
	t.Helper()
	task, err := db.GetTaskByID(context.Background(), taskID)
	if err != nil {
		t.Fatalf("读任务 %s: %v", taskID, err)
	}
	return task
}

func keys(m map[string]*store.Task) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
