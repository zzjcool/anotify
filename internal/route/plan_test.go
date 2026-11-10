package route

import (
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

func TestPlanFromSourceMapsTemplatesAndKeepsOrder(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	capability := testCapability(t, db, ns.ID, "analyze", 1, capOpts{
		Params: `{"type":"object","properties":{"env":{"type":"string"},"note":{"type":"string","default":"none"}},"required":["env"]}`,
	})
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Config: json.RawMessage(`{"payload":{"env":"prod"}}`),
		Targets: []sourcespec.Target{{
			Capability: "analyze",
			Params:     map[string]any{"env": "$(payload.env)"},
		}},
		Enabled: true,
	}
	router := New(db)
	plan, err := router.PlanFromSource(testCtx(), src, "trace-1", map[string]any{"env": "prod"})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].State != StateReady {
		t.Fatalf("应有一条 ready target，得到 %+v", plan.Targets)
	}
	if plan.Targets[0].Capability != "analyze" || plan.Targets[0].CapabilityID != capability.ID {
		t.Fatalf("target 未解析到真实能力行: %+v", plan.Targets[0])
	}
	var params map[string]any
	if err := json.Unmarshal(plan.Targets[0].Params, &params); err != nil {
		t.Fatalf("params 不是 JSON: %v", err)
	}
	if params["env"] != "prod" || params["note"] != "none" {
		t.Fatalf("模板/default 未生效: %v", params)
	}
	if len(plan.Pruned) != 0 {
		t.Fatalf("不该有剪枝: %+v", plan.Pruned)
	}
}

func TestPlanFromSourceNoTargets(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-1", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Reason != protocol.ReasonNoTarget {
		t.Fatalf("空 targets 必须留一条 no_target，得到 %+v", plan.Pruned)
	}
	if len(plan.Ready()) != 0 {
		t.Fatalf("no_target 不该建任务: %+v", plan.Targets)
	}
}

func TestPlanFromSourceMissingCapability(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{{Capability: "ghost"}},
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-1", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].State != StateMissing {
		t.Fatalf("解析不到的能力应记 missing，得到 %+v", plan.Targets)
	}
	if plan.Pruned[0].Reason != protocol.ReasonDownstreamMissing {
		t.Fatalf("reason = %q，want downstream_missing", plan.Pruned[0].Reason)
	}
}

func TestPlanFromSourceParamMissing(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "analyze", 1, capOpts{
		Params: `{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`,
	})
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{{Capability: "analyze", Params: map[string]any{"env": "$(payload.env)"}}},
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-1", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("缺模板路径应 param_missing，得到 %+v", plan.Pruned)
	}
	if plan.Targets[0].State != StatePruned {
		t.Fatalf("target 应记 pruned，得到 %+v", plan.Targets[0])
	}
}

func TestPlanTasksArePendingWithoutAuthority(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "analyze", 2, capOpts{Params: `{"type":"object"}`})
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{{Capability: "analyze"}},
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-42", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	tasks := plan.Tasks("")
	if len(tasks) != 1 {
		t.Fatalf("应生成一条任务，得到 %d", len(tasks))
	}
	task := tasks[0]
	if task.Phase != store.TaskPending || task.Attempt != 0 || task.LeaseID != "" || task.AttemptTokenHash != "" {
		t.Fatalf("建任务不得携带执行授权: %+v", task)
	}
	if task.TraceID != "trace-42" || task.Version != 2 || task.Capability != "analyze" {
		t.Fatalf("任务未钉住解析结果: %+v", task)
	}
}

func TestPlanFromEmitSameNameAndNotDeclared(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	parentCap := testCapability(t, db, ns.ID, "producer", 1, capOpts{Next: []string{"consumer"}})
	testCapability(t, db, ns.ID, "consumer", 1, capOpts{
		Params: `{"type":"object","properties":{"file":{"type":"string"}},"required":["file"]}`,
	})
	parent := testPendingTask(t, db, ns.ID, "trace-emit", parentCap)
	router := New(db)

	plan, err := router.PlanFromEmit(testCtx(), parent, "consumer", json.RawMessage(`{"file":"a.go","extra":1}`))
	if err != nil {
		t.Fatalf("PlanFromEmit: %v", err)
	}
	if len(plan.Ready()) != 1 {
		t.Fatalf("应有一条 ready target，得到 %+v", plan.Targets)
	}
	var params map[string]any
	_ = json.Unmarshal(plan.Targets[0].Params, &params)
	if params["file"] != "a.go" {
		t.Fatalf("同名直取未生效: %v", params)
	}
	if _, ok := params["extra"]; ok {
		t.Fatalf("多余字段必须忽略: %v", params)
	}

	if _, err := router.PlanFromEmit(testCtx(), parent, "other", nil); err == nil {
		t.Fatal("未声明的 emit 目标必须被拒绝")
	}

	pruned, err := router.PlanFromEmit(testCtx(), parent, "consumer", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("缺参不应报错而应剪枝: %v", err)
	}
	if pruned.Pruned[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("缺参应 param_missing，得到 %+v", pruned.Pruned)
	}

	bad, err := router.PlanFromEmit(testCtx(), parent, "consumer", json.RawMessage(`[1]`))
	if err != nil {
		t.Fatalf("非对象 data 应剪枝: %v", err)
	}
	if bad.Pruned[0].Reason != protocol.ReasonParamSchema {
		t.Fatalf("非对象 data 应 param_schema，得到 %+v", bad.Pruned)
	}
}
