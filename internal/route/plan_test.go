package route

import (
	"encoding/json"
	"errors"
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
	if plan.Targets[0].Reason != protocol.ReasonDownstreamMissing {
		t.Fatalf("target reason = %q，want downstream_missing", plan.Targets[0].Reason)
	}
	// downstream_missing 是失败 Task + 错误事件，不是剪枝。
	if len(plan.Pruned) != 0 {
		t.Fatalf("missing 不得进入 pruned（它是失败 Task），得到 %+v", plan.Pruned)
	}
	failures := plan.FailureInputs()
	if len(failures) != 1 || failures[0].Capability != "ghost" || failures[0].Version != 0 ||
		failures[0].Reason != protocol.ReasonDownstreamMissing {
		t.Fatalf("失败事实不对（裸请求不携带版本事实）: %+v", failures)
	}
	if got := plan.PruneInputs(); len(got) != 0 {
		t.Fatalf("PruneInputs 不得包含 downstream_missing: %+v", got)
	}
}

func TestPlanFromSourceMissingKeepsDeclaredVersion(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{{Capability: "ghost@2"}},
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-1", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	failures := plan.FailureInputs()
	if len(failures) != 1 || failures[0].Capability != "ghost@2" || failures[0].Version != 2 {
		t.Fatalf("明确的声明版本必须保留（这是真实请求事实）: %+v", failures)
	}
	if len(plan.Pruned) != 0 {
		t.Fatalf("missing 不得当剪枝: %+v", plan.Pruned)
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
	if plan.Targets[0].Reason != protocol.ReasonParamMissing {
		t.Fatalf("target 自身 reason 必须逐条给出，得到 %+v", plan.Targets[0])
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

// TestPlanFromEmitResolvesDeclaredRefs 钉住父裁决的 next 声明语义（R1-C6）：
// 裸 to 按唯一声明解析（裸名跟随 latest / name@v 用 v），显式 name@v 只认精确声明，
// 同名多钉版歧义时沿现 not_declared 拒绝（不偷偷 latest）。
func TestPlanFromEmitResolvesDeclaredRefs(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	parentCap := testCapability(t, db, ns.ID, "producer", 1, capOpts{Next: []string{"consumer@2"}})
	_ = testCapability(t, db, ns.ID, "consumer", 1, capOpts{})
	consumerV2 := testCapability(t, db, ns.ID, "consumer", 2, capOpts{})
	parent := testPendingTask(t, db, ns.ID, "trace-pin", parentCap)
	router := New(db)

	// 父声明 next=[consumer@2]：裸 to 解析到声明版本 2（不是 latest 的偶然结果）。
	plan, err := router.PlanFromEmit(testCtx(), parent, "consumer", nil)
	if err != nil {
		t.Fatalf("PlanFromEmit: %v", err)
	}
	if len(plan.Ready()) != 1 || plan.Ready()[0].Version != 2 || plan.Ready()[0].Digest != consumerV2.Digest {
		t.Fatalf("裸 to 必须按声明钉到 v2（真实 version/digest）: %+v", plan.Targets)
	}
	// 显式 name@v 只能匹配已声明的精确引用：v1 未声明 → 拒。
	if _, err := router.PlanFromEmit(testCtx(), parent, "consumer@1", nil); !errors.Is(err, ErrNotDeclared) {
		t.Fatalf("显式旧版不能被裸/钉版声明放行，得到 %v", err)
	}
	// 精确声明 v2 → 允许。
	if pinned, err := router.PlanFromEmit(testCtx(), parent, "consumer@2", nil); err != nil ||
		len(pinned.Ready()) != 1 || pinned.Ready()[0].Version != 2 {
		t.Fatalf("显式声明过的 v2 应可用: %v/%+v", err, pinned)
	}

	// 同一父版本改 next（可变图配置）：后续 emit 按真实当前声明解析。
	consumerV3 := testCapability(t, db, ns.ID, "consumer", 3, capOpts{})
	if _, err := db.UpdateNext(testCtx(), ns.ID, "producer", 1,
		[]string{"consumer@3"}, store.ResourceVersion(parentCap.ID, parentCap.ResourceVersion)); err != nil {
		t.Fatalf("改 next: %v", err)
	}
	updated, err := router.PlanFromEmit(testCtx(), parent, "consumer", nil)
	if err != nil {
		t.Fatalf("改 next 后 PlanFromEmit: %v", err)
	}
	if len(updated.Ready()) != 1 || updated.Ready()[0].Version != 3 || updated.Ready()[0].Digest != consumerV3.Digest {
		t.Fatalf("后续 emit 必须按当前声明 v3: %+v", updated.Targets)
	}
	// 已建子任务/旧计划不被改 next 影响（父运行中改 next 不改既往事实）。
	if len(plan.Ready()) != 1 || plan.Ready()[0].Version != 2 {
		t.Fatalf("旧计划不得被改 next 漂移: %+v", plan.Targets)
	}

	// 同名多钉版：裸 to 无法唯一选择 → ambiguous（沿 not_declared，不新增码）。
	loaded, err := db.GetCapability(testCtx(), ns.ID, "producer", 1)
	if err != nil {
		t.Fatalf("回读 producer: %v", err)
	}
	if _, err := db.UpdateNext(testCtx(), ns.ID, "producer", 1,
		[]string{"consumer@2", "consumer@3"}, store.ResourceVersion(loaded.ID, loaded.ResourceVersion)); err != nil {
		t.Fatalf("改 next 为多钉版: %v", err)
	}
	if _, err := router.PlanFromEmit(testCtx(), parent, "consumer", nil); !errors.Is(err, ErrNotDeclared) {
		t.Fatalf("同名多钉版裸 to 必须 not_declared，得到 %v", err)
	}
	if exact, err := router.PlanFromEmit(testCtx(), parent, "consumer@3", nil); err != nil ||
		len(exact.Ready()) != 1 || exact.Ready()[0].Version != 3 {
		t.Fatalf("显式精确钉版仍可用: %v/%+v", err, exact)
	}

	// 声明裸名时显式 @v 不再被授权（钉版语义不能被裸声明绕过）。
	loadedAgain, err := db.GetCapability(testCtx(), ns.ID, "producer", 1)
	if err != nil {
		t.Fatalf("回读 producer: %v", err)
	}
	if _, err := db.UpdateNext(testCtx(), ns.ID, "producer", 1,
		[]string{"consumer"}, store.ResourceVersion(loadedAgain.ID, loadedAgain.ResourceVersion)); err != nil {
		t.Fatalf("改 next 回裸名: %v", err)
	}
	if _, err := router.PlanFromEmit(testCtx(), parent, "consumer@1", nil); !errors.Is(err, ErrNotDeclared) {
		t.Fatalf("裸声明不得放行显式 @v，得到 %v", err)
	}
	if latest, err := router.PlanFromEmit(testCtx(), parent, "consumer", nil); err != nil ||
		len(latest.Ready()) != 1 || latest.Ready()[0].Version != 3 {
		t.Fatalf("裸声明应跟随 latest v3: %v/%+v", err, latest)
	}
}

// TestPlanFromSourceTemplatesSinglePassAndKeepTypes 消费现 sourcespec 的真实语义：
// 模板单趟求值（payload 里的 $(payload.x) 文本不被二次展开）、整值引用保类型、
// 混写拼字符串（R1-C7）。
func TestPlanFromSourceTemplatesSinglePassAndKeepTypes(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "analyze", 1, capOpts{
		Params: `{"type":"object","properties":{
			"env":{"type":"string"},
			"count":{"type":"integer"},
			"flag":{"type":"boolean"},
			"meta":{"type":"object"},
			"mixed":{"type":"string"}
		},"required":["env","count","flag","meta","mixed"]}`,
	})
	src := &store.Source{
		NamespaceID: ns.ID, Name: "manual-1", Kind: sourcespec.KindManual,
		Targets: []sourcespec.Target{{
			Capability: "analyze",
			Params: map[string]any{
				"env":   "$(payload.env)",
				"count": "$(payload.count)",
				"flag":  "$(payload.flag)",
				"meta":  "$(payload.meta)",
				"mixed": "n=$(payload.count)",
			},
		}},
		Enabled: true,
	}
	payload := map[string]any{
		"env":   "$(payload.other)", // 若递归展开会变成 leak
		"other": "leak",
		"count": 7,
		"flag":  true,
		"meta":  map[string]any{"a": "b"},
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-tpl", payload)
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Ready()) != 1 {
		t.Fatalf("模板应全部解析成功: %+v", plan.Targets)
	}
	var params map[string]any
	if err := json.Unmarshal(plan.Ready()[0].Params, &params); err != nil {
		t.Fatalf("解码 params: %v", err)
	}
	if params["env"] != "$(payload.other)" {
		t.Fatalf("模板必须单趟不递归：env=%v", params["env"])
	}
	if count, ok := params["count"].(float64); !ok || count != 7 {
		t.Fatalf("整值引用必须保数字类型: %#v", params["count"])
	}
	if flag, ok := params["flag"].(bool); !ok || !flag {
		t.Fatalf("整值引用必须保 bool 类型: %#v", params["flag"])
	}
	meta, ok := params["meta"].(map[string]any)
	if !ok || meta["a"] != "b" {
		t.Fatalf("整值引用必须保对象类型: %#v", params["meta"])
	}
	if params["mixed"] != "n=7" {
		t.Fatalf("混写必须拼成字符串: %#v", params["mixed"])
	}
}
