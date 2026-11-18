package route

import (
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// 能力停用的路由闸门（capability-lifecycle §6.1）：★ 判定在 PrepareParams 成功之后、
// target 标成 StateReady 之前，PlanFromSource 与 PlanFromEmit 共用同一个私有方法。
//
// 夹具全部走真实临时库（fixture_test.go），开关用真实的 SetCapabilitySwitch 写：
// 「无行 = enabled」这条默认值是 store 的语义，不能在测试里用 fake 假装。

// disableCapability 用真实 store 写一条 enabled=false 的开关行（含审计，同事务）。
func disableCapability(t *testing.T, db *store.DB, namespaceID, name string) {
	t.Helper()
	if _, err := db.SetCapabilitySwitch(testCtx(), store.CapabilitySwitchInput{
		NamespaceID: namespaceID, Name: name, Enabled: false,
		Reason: "测试停用", ActorID: "usr_test", Now: store.Now(),
	}); err != nil {
		t.Fatalf("停用能力 %s: %v", name, err)
	}
}

// enableCapability 显式写一条 enabled=true 的开关行（与「无行」对照用）。
func enableCapability(t *testing.T, db *store.DB, namespaceID, name string) {
	t.Helper()
	if _, err := db.SetCapabilitySwitch(testCtx(), store.CapabilitySwitchInput{
		NamespaceID: namespaceID, Name: name, Enabled: true,
		Reason: "测试启用", ActorID: "usr_test", Now: store.Now(),
	}); err != nil {
		t.Fatalf("启用能力 %s: %v", name, err)
	}
}

// TestPlanFromSourceDisabledTargetPruned 钉住 disabled 的唯一后果：不建任务 + 一条剪枝事实。
func TestPlanFromSourceDisabledTargetPruned(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "review", 1, capOpts{})
	disableCapability(t, db, ns.ID, "review")
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{{Capability: "review"}},
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-disabled", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if ready := plan.Ready(); len(ready) != 0 {
		t.Fatalf("停用的能力不得是 ready（那会建任务）: %+v", ready)
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Reason != protocol.ReasonCapabilityDisabled {
		t.Fatalf("剪枝事实必须恰一条 capability_disabled，得到 %+v", plan.Pruned)
	}
	if plan.Pruned[0].Capability != "review" {
		t.Fatalf("剪枝事实要带能力名: %+v", plan.Pruned[0])
	}
	if plan.Pruned[0].Detail == "" {
		t.Fatalf("剪枝事实要带给人看的 detail: %+v", plan.Pruned[0])
	}
	if !protocol.IsPruneReason(protocol.ReasonCapabilityDisabled) {
		t.Fatalf("capability_disabled 必须在剪枝闭集里: %v", protocol.PruneReasons())
	}
	if len(plan.Targets) != 1 || plan.Targets[0].State != StatePruned ||
		plan.Targets[0].Reason != protocol.ReasonCapabilityDisabled ||
		plan.Targets[0].Capability != "review" {
		t.Fatalf("target 必须逐条记 pruned + capability_disabled: %+v", plan.Targets)
	}
	if tasks := plan.Tasks(""); len(tasks) != 0 {
		t.Fatalf("停用的能力不得建任务，得到 %d 条: %+v", len(tasks), tasks)
	}
	// 剪枝事实要能原样落进 store（同事务留痕）；停用不是失败 Task。
	if inputs := plan.PruneInputs(); len(inputs) != 1 ||
		inputs[0].Capability != "review" || inputs[0].Reason != protocol.ReasonCapabilityDisabled {
		t.Fatalf("PruneInputs 应带 capability_disabled: %+v", inputs)
	}
	if failures := plan.FailureInputs(); len(failures) != 0 {
		t.Fatalf("停用不是失败 Task（不留 downstream_missing）: %+v", failures)
	}
}

// TestPlanFromSourceDisabledOnlyNamedTarget 钉住闸门只作用于被停的那个名字：
// 同一次扇出里其它 target 照常建任务。
func TestPlanFromSourceDisabledOnlyNamedTarget(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "review", 1, capOpts{})
	alert := testCapability(t, db, ns.ID, "alert", 1, capOpts{})
	disableCapability(t, db, ns.ID, "review")
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{{Capability: "review"}, {Capability: "alert"}},
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-fanout", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Targets) != 2 {
		t.Fatalf("两个声明 target 都要留行，得到 %+v", plan.Targets)
	}
	if plan.Targets[0].State != StatePruned || plan.Targets[0].Reason != protocol.ReasonCapabilityDisabled {
		t.Fatalf("review 必须被剪: %+v", plan.Targets[0])
	}
	if plan.Targets[1].State != StateReady || plan.Targets[1].CapabilityID != alert.ID {
		t.Fatalf("alert 必须照常 ready: %+v", plan.Targets[1])
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Capability != "review" ||
		plan.Pruned[0].Reason != protocol.ReasonCapabilityDisabled {
		t.Fatalf("只该有 review 一条剪枝，得到 %+v", plan.Pruned)
	}
	tasks := plan.Tasks("")
	if len(tasks) != 1 || tasks[0].Capability != "alert" || tasks[0].Version != 1 {
		t.Fatalf("恰好一条 alert 任务，得到 %+v", tasks)
	}
}

// TestPlanFromSourceEnabledRegression 钉住默认启用：无开关行（与显式 enabled=true）
// 必须与改动前逐字同形状 —— ready + 建任务，且不留任何剪枝。
func TestPlanFromSourceEnabledRegression(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	capability := testCapability(t, db, ns.ID, "analyze", 2, capOpts{
		Params: `{"type":"object","properties":{"env":{"type":"string"}},"required":["env"]}`,
	})
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{{Capability: "analyze", Params: map[string]any{"env": "$(payload.env)"}}},
		Enabled: true,
	}
	check := func(when string) {
		t.Helper()
		plan, err := New(db).PlanFromSource(testCtx(), src, "trace-enabled", map[string]any{"env": "prod"})
		if err != nil {
			t.Fatalf("%s: PlanFromSource: %v", when, err)
		}
		if len(plan.Pruned) != 0 {
			t.Fatalf("%s: 启用的能力不得有剪枝，得到 %+v", when, plan.Pruned)
		}
		if len(plan.Targets) != 1 || plan.Targets[0].State != StateReady ||
			plan.Targets[0].Capability != "analyze" || plan.Targets[0].CapabilityID != capability.ID ||
			plan.Targets[0].Digest != capability.Digest {
			t.Fatalf("%s: 应为一条 ready target，得到 %+v", when, plan.Targets)
		}
		tasks := plan.Tasks("")
		if len(tasks) != 1 || tasks[0].Capability != "analyze" || tasks[0].Version != 2 ||
			tasks[0].Phase != store.TaskPending {
			t.Fatalf("%s: 应建一条 pinned pending 任务，得到 %+v", when, tasks)
		}
	}
	check("无开关行（默认启用）")
	enableCapability(t, db, ns.ID, "analyze")
	check("显式 enabled=true")
}

// TestPlanFromSourceDisabledStillReportsParamError 钉住设计图定的优先级：
// 同时「参数不合法」与「已停用」时先报参数原因（闸门在 PrepareParams 之后）。
func TestPlanFromSourceDisabledStillReportsParamError(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "review", 1, capOpts{
		Params: `{"type":"object","properties":{"diff":{"type":"string"}},"required":["diff"]}`,
	})
	disableCapability(t, db, ns.ID, "review")
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{{Capability: "review", Params: map[string]any{"diff": "$(payload.diff)"}}},
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-order", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Pruned) != 1 {
		t.Fatalf("应恰一条剪枝，得到 %+v", plan.Pruned)
	}
	if plan.Pruned[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("参数原因优先于开关：reason = %q，want param_missing（不是 capability_disabled）",
			plan.Pruned[0].Reason)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("target 的 reason 也必须是 param_missing: %+v", plan.Targets)
	}
	if tasks := plan.Tasks(""); len(tasks) != 0 {
		t.Fatalf("两条原因都不建任务，得到 %d 条", len(tasks))
	}
}

// TestPlanFromEmitDisabledPinnedRefLooksUpByName 钉住「开关按能力名查，不按引用查」：
// 父 next 声明 `consumer@2`（effective=name@v）时，停用 consumer 仍必须命中闸门。
func TestPlanFromEmitDisabledPinnedRefLooksUpByName(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	producer := testCapability(t, db, ns.ID, "producer", 1, capOpts{Next: []string{"consumer@2"}})
	testCapability(t, db, ns.ID, "consumer", 1, capOpts{})
	consumerV2 := testCapability(t, db, ns.ID, "consumer", 2, capOpts{})
	parent := testPendingTask(t, db, ns.ID, "trace-pinned", producer)
	disableCapability(t, db, ns.ID, "consumer")

	plan, err := New(db).PlanFromEmit(testCtx(), parent, "consumer", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("PlanFromEmit: %v", err)
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Reason != protocol.ReasonCapabilityDisabled ||
		plan.Pruned[0].Capability != "consumer" {
		t.Fatalf("钉版引用必须按名字命中开关（得到 %+v）", plan.Pruned)
	}
	if len(plan.Targets) != 1 || plan.Targets[0].State != StatePruned ||
		plan.Targets[0].Version != 2 || plan.Targets[0].Digest != consumerV2.Digest {
		t.Fatalf("剪枝 target 也要保留解析出的身份（v2/digest）: %+v", plan.Targets)
	}
	if tasks := plan.Tasks(parent.ID); len(tasks) != 0 {
		t.Fatalf("停用下游不得建任务，得到 %d 条", len(tasks))
	}
}
