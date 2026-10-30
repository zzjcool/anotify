package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestSeedBuiltinCapabilitiesIsIdempotent 验证内置能力播种幂等：
// 重复调用不会开新版本（capability_versions 是 append-only，不变量 I8）。
//
// #205 之后播种收口到 SeedBuiltinCapabilities(namespaceID)：能力属于命名空间，
// 组织那一层已经不存在了。
func TestSeedBuiltinCapabilitiesIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	for i := 0; i < 3; i++ {
		if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
			t.Fatalf("第 %d 次播种失败: %v", i+1, err)
		}
	}

	c, err := db.GetCapability(ctx, nsID, "notify.phone", 1)
	if err != nil {
		t.Fatalf("get notify.phone: %v", err)
	}
	if c.Source != CapSourceBuiltin {
		t.Errorf("source = %q, want builtin", c.Source)
	}
	if c.Outputs == nil {
		t.Fatal("outputs 是 nil，应为 []（不变量 I9）")
	}
	if len(c.Outputs) != 0 {
		t.Errorf("notify.phone 是终端能力，outputs 应为空，实际 %v", c.Outputs)
	}

	// 不该出现第二行
	list, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var n int
	for _, x := range list {
		if x.Name == "notify.phone" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("notify.phone 版本数 = %d, want 1", n)
	}
}

// TestBuiltinCapabilityIsCentral 验证 notify.phone 被标记为中枢执行。
func TestBuiltinCapabilityIsCentral(t *testing.T) {
	bc, ok := IsBuiltinCapability("notify.phone", 1)
	if !ok {
		t.Fatal("notify.phone@1 应被识别为内置能力")
	}
	if !bc.Central {
		t.Error("notify.phone 应由中枢执行（没有节点执行者）")
	}

	if _, ok := IsBuiltinCapability("review", 1); ok {
		t.Error("review 不是内置能力（要节点执行）")
	}
	if _, ok := IsBuiltinCapability("notify.phone", 99); ok {
		t.Error("版本不匹配不该命中")
	}
}

// TestCentralCapabilityNames 验证名单一致性。
func TestCentralCapabilityNames(t *testing.T) {
	db := newTestDB(t)
	names := db.CentralCapabilityNames()
	if len(names) != 1 || names[0] != "notify.phone" {
		t.Fatalf("CentralCapabilityNames = %v, want [notify.phone]", names)
	}
}

// TestNotifyPhoneParamsSchemaRequiresTo 验证 params_schema 声明了 to 必填。
//
// 这条很重要：没有它，params 缺 to 时派发期就会过校验，
// 直到执行期才报错——用户看到的是"任务失败"而不是"参数不对"。
func TestNotifyPhoneParamsSchemaRequiresTo(t *testing.T) {
	var schema struct {
		Required   []string       `json:"required"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(NotifyPhoneCapability.ParamsSchema), &schema); err != nil {
		t.Fatalf("params_schema 不是合法 JSON: %v", err)
	}
	var hasTo bool
	for _, r := range schema.Required {
		if r == "to" {
			hasTo = true
		}
	}
	if !hasTo {
		t.Error("params_schema 必须 required: [to]（否则跨命名空间校验无从下手）")
	}
	if _, ok := schema.Properties["title"]; !ok {
		t.Error("params_schema 应声明 title")
	}
}

// TestClaimCentralTasksRunsStateMachine 验证中枢任务被领取时走完整状态机。
//
// 关键：中枢任务没有节点，但仍必须 pending → assigned → running，
// 不能直接跳到 running——那样状态机就出现了旁路。
func TestClaimCentralTasksRunsStateMachine(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("seed builtins: %v", err)
	}
	capDef, err := db.GetCapability(ctx, nsID, "notify.phone", 1)
	if err != nil {
		t.Fatalf("get cap: %v", err)
	}

	task := &Task{
		ID:           NewTaskID(),
		NamespaceID:  nsID,
		TraceID:      "tr_central",
		Path:         []string{capDef.ID},
		CapabilityID: capDef.ID,
		Capability:   capDef.Name,
		Version:      capDef.Version,
		Params:       []byte(`{"to":"usr_x","title":"hi"}`),
		Status:       TaskPending,
		CapHash:      capDef.ContentHash,
		GraphID:      "grf_test",
	}
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	claimed, err := db.ClaimCentralTasks(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("应领到 1 条，实际 %d", len(claimed))
	}
	if claimed[0].ID != task.ID {
		t.Errorf("领到的任务 ID 不对")
	}

	// 落库状态必须是 running（且 started_at 已写）
	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != TaskRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.StartedAt == nil {
		t.Error("started_at 应已写入")
	}
	// 中枢任务没有节点归属
	if got.NodeID != nil {
		t.Errorf("中枢任务不该有 node_id，实际 %v", *got.NodeID)
	}

	// 再领一次：不该重复领到（status 已是 running）
	again, err := db.ClaimCentralTasks(ctx, 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("已被领过的任务不该再领到，实际 %d 条", len(again))
	}
}

// TestFinishTaskWritesTristate 验证 FinishTask 的 out 三态落库（不变量 I5）。
func TestFinishTaskWritesTristate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	capDef, err := db.GetCapability(ctx, nsID, "notify.phone", 1)
	if err != nil {
		t.Fatalf("get cap: %v", err)
	}

	mkRunning := func() string {
		task := &Task{
			ID: NewTaskID(), NamespaceID: nsID, TraceID: "tr",
			Path: []string{capDef.ID}, CapabilityID: capDef.ID,
			Capability: capDef.Name, Version: capDef.Version,
			Params: []byte(`{"to":"u","title":"t"}`), Status: TaskPending,
			CapHash: capDef.ContentHash, GraphID: "grf",
		}
		if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := db.ClaimCentralTasks(ctx, 10); err != nil {
			t.Fatalf("claim: %v", err)
		}
		return task.ID
	}

	// 态 3：reported=1, out="fix"（虽然 notify.phone 没有出口，但机制要通用）
	id := mkRunning()
	port := "fix"
	if err := db.FinishTask(ctx, nsID, id, TaskSucceeded, &port, true, "done", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ := db.GetTask(ctx, nsID, id)
	if !got.Reported || got.Out == nil || *got.Out != "fix" {
		t.Errorf("态3 落库错误: reported=%v out=%v", got.Reported, got.Out)
	}

	// 态 2：reported=1, out=nil（到头了）
	id = mkRunning()
	if err := db.FinishTask(ctx, nsID, id, TaskSucceeded, nil, true, "end", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ = db.GetTask(ctx, nsID, id)
	if !got.Reported || got.Out != nil {
		t.Errorf("态2 落库错误: reported=%v out=%v", got.Reported, got.Out)
	}

	// 态 1：reported=0, out=nil（没回报）
	id = mkRunning()
	if err := db.FinishTask(ctx, nsID, id, TaskFailed, nil, false, "", "boom"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ = db.GetTask(ctx, nsID, id)
	if got.Reported || got.Out != nil {
		t.Errorf("态1 落库错误: reported=%v out=%v", got.Reported, got.Out)
	}
}

// TestFinishTaskRejectsInvalidStatus 验证只接受 succeeded|failed。
func TestFinishTaskRejectsInvalidStatus(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	err := db.FinishTask(ctx, nsID, "tsk_x", TaskCanceled, nil, false, "", "")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
}

// TestNoOriginShellCapabilities #110：内置起点不再是能力。
//
// 它们原来以「内置起点能力」形式存在：不执行代码、不产生任务、也没地方
// 存配置，唯一作用是给入口边当左端锚点，代价是能力列表多出 4 个看得见
// 却用不上的条目。现在入口边左端直接是来源名，所以它们不该再被播种。
func TestNoOriginShellCapabilities(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("seed builtins: %v", err)
	}
	for _, name := range []string{"github", "cron", "http", "manual"} {
		if _, err := db.GetCapability(ctx, nsID, name, 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s@1 不该再作为能力存在（#110），got err=%v", name, err)
		}
	}
	// 中枢执行的终端能力仍然在（它是真能力）
	if _, err := db.GetCapability(ctx, nsID, "notify.phone", 1); err != nil {
		t.Fatalf("notify.phone@1 应仍存在: %v", err)
	}
	// 用户现在可以自由使用这些名字（不再保留给空壳）
	if err := db.CreateCapability(ctx, &CapabilityVersion{
		NamespaceID:  nsID,
		Name:         "github",
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"done"},
		Source:       CapSourceCustom,
	}); err != nil {
		t.Fatalf("用户现在可以定义叫 github 的能力: %v", err)
	}
}

// TestEntryEdgeAcceptsAnyPort 入口边（from_is_source）的出口是任意事件类型。
//
// 这是不变量 I2 的唯一例外，而且现在表达得更直接：入口边的左端是
// **来源名**而不是能力，来源没有 outputs 可枚举（事件类型由外部系统决定）。
func TestEntryEdgeAcceptsAnyPort(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	sink := &CapabilityVersion{
		NamespaceID: nsID, Name: "sink", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{}, Source: CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, sink); err != nil {
		t.Fatalf("create sink: %v", err)
	}

	// 入口边：左端是来源名，任意事件类型都能建
	for _, port := range []string{"pull_request.opened", "never.declared.before", "x"} {
		if err := db.CreateEdge(ctx, &Edge{
			NamespaceID:    nsID,
			FromIsSource:   true,
			FromCapability: "github",
			FromPort:       port,
			ToCapability:   sink.ID,
		}); err != nil {
			t.Fatalf("入口边的任意出口应被接受（port=%s）: %v", port, err)
		}
	}

	// 下游能查到（派发第 ① 步的查表）
	got, err := db.EdgesFrom(ctx, nsID, true, "github", "pull_request.opened")
	if err != nil {
		t.Fatalf("edges from: %v", err)
	}
	if len(got) != 1 || got[0] != sink.ID {
		t.Fatalf("入口边查表 = %v, want [%s]", got, sink.ID)
	}

	// 自定义能力的边仍然严格（I2 没有放松）
	custom := &CapabilityVersion{
		NamespaceID: nsID, Name: "custom", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"only"}, Source: CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, custom); err != nil {
		t.Fatalf("create custom: %v", err)
	}
	err = db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromCapability: custom.ID,
		FromPort: "not_declared", ToCapability: sink.ID,
	})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("自定义能力的非法出口应被拒绝，got %v", err)
	}

	// 两种边互不串味：来源名 "github" 与能力边不会互相命中
	if _, err := db.GetCapability(ctx, nsID, "github", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不该存在一个叫 github 的能力来混淆入口边")
	}
}

// TestBuiltinDisplayNames 内置能力必须**自带**显示名。
//
// 用户的原话是「默认的内置的我们给他取好了」——内置能力随项目发布，
// 名字该由我们定，不该让用户对着 github@1 猜这是什么。
func TestBuiltinDisplayNames(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("ensure builtin: %v", err)
	}
	for _, bc := range BuiltinCapabilities {
		got, err := db.GetCapability(ctx, nsID, bc.Name, bc.Version)
		if err != nil {
			t.Fatalf("get %s@%d: %v", bc.Name, bc.Version, err)
		}
		if bc.DisplayName == "" {
			t.Fatalf("builtin %s has no DisplayName in code", bc.Name)
		}
		if got.DisplayName != bc.DisplayName {
			t.Fatalf("builtin %s display name = %q, want %q",
				bc.Name, got.DisplayName, bc.DisplayName)
		}
	}
}

// TestBuiltinDisplayNameBackfill 老库补名：已存在的内置行 display_name 为空时要补上，
// 但**用户改过的名字不能被启动流程覆盖**。
func TestBuiltinDisplayNameBackfill(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// 先播种，然后模拟"老库"：把内置能力的显示名清空（就像这列刚加时那样）
	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("ensure builtin: %v", err)
	}
	// notify.phone 是现在唯一的内置能力（起点已不再是能力）
	gh, err := db.GetCapability(ctx, nsID, "notify.phone", 1)
	if err != nil {
		t.Fatalf("get notify.phone: %v", err)
	}
	if err := db.SetCapabilityDisplayName(ctx, nsID, gh.ID, ""); err != nil {
		t.Fatalf("clear display name: %v", err)
	}
	// 再跑一次播种（启动时会对所有命名空间跑一遍）→ 应该补回来
	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("re-ensure builtin: %v", err)
	}
	gh2, _ := db.GetCapability(ctx, nsID, "notify.phone", 1)
	if gh2.DisplayName != "推送通知" {
		t.Fatalf("backfill failed: %q", gh2.DisplayName)
	}

	// 用户改成自己的名字 → 不能被启动流程改回去
	if err := db.SetCapabilityDisplayName(ctx, nsID, gh.ID, "我们的 PR 入口"); err != nil {
		t.Fatalf("user rename: %v", err)
	}
	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	gh3, _ := db.GetCapability(ctx, nsID, "notify.phone", 1)
	if gh3.DisplayName != "我们的 PR 入口" {
		t.Fatalf("startup overwrote user's display name: %q", gh3.DisplayName)
	}
}
