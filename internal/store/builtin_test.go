package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestEnsureBuiltinCapabilitiesIsIdempotent 验证内置能力播种幂等：
// 重复调用不会开新版本（capability_versions 是 append-only，不变量 I8）。
func TestEnsureBuiltinCapabilitiesIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	for i := 0; i < 3; i++ {
		if err := db.EnsureBuiltinCapabilities(ctx, orgID); err != nil {
			t.Fatalf("第 %d 次播种失败: %v", i+1, err)
		}
	}

	c, err := db.GetCapability(ctx, orgID, "notify.phone", 1)
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
	list, err := db.ListCapabilities(ctx, orgID)
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
		t.Error("params_schema 必须 required: [to]（否则跨组织校验无从下手）")
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
	orgID, _ := newTestOrg(t, db)

	if err := db.EnsureBuiltinCapabilities(ctx, orgID); err != nil {
		t.Fatalf("seed builtins: %v", err)
	}
	capDef, err := db.GetCapability(ctx, orgID, "notify.phone", 1)
	if err != nil {
		t.Fatalf("get cap: %v", err)
	}

	task := &Task{
		ID:           NewTaskID(),
		OrgID:        orgID,
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
	got, err := db.GetTask(ctx, orgID, task.ID)
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
	orgID, _ := newTestOrg(t, db)
	if err := db.EnsureBuiltinCapabilities(ctx, orgID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	capDef, err := db.GetCapability(ctx, orgID, "notify.phone", 1)
	if err != nil {
		t.Fatalf("get cap: %v", err)
	}

	mkRunning := func() string {
		task := &Task{
			ID: NewTaskID(), OrgID: orgID, TraceID: "tr",
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
	if err := db.FinishTask(ctx, orgID, id, TaskSucceeded, &port, true, "done", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ := db.GetTask(ctx, orgID, id)
	if !got.Reported || got.Out == nil || *got.Out != "fix" {
		t.Errorf("态3 落库错误: reported=%v out=%v", got.Reported, got.Out)
	}

	// 态 2：reported=1, out=nil（到头了）
	id = mkRunning()
	if err := db.FinishTask(ctx, orgID, id, TaskSucceeded, nil, true, "end", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ = db.GetTask(ctx, orgID, id)
	if !got.Reported || got.Out != nil {
		t.Errorf("态2 落库错误: reported=%v out=%v", got.Reported, got.Out)
	}

	// 态 1：reported=0, out=nil（没回报）
	id = mkRunning()
	if err := db.FinishTask(ctx, orgID, id, TaskFailed, nil, false, "", "boom"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ = db.GetTask(ctx, orgID, id)
	if got.Reported || got.Out != nil {
		t.Errorf("态1 落库错误: reported=%v out=%v", got.Reported, got.Out)
	}
}

// TestFinishTaskRejectsInvalidStatus 验证只接受 succeeded|failed。
func TestFinishTaskRejectsInvalidStatus(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	err := db.FinishTask(ctx, orgID, "tsk_x", TaskCanceled, nil, false, "", "")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
}

// TestOriginCapabilityNameReserved 内置起点的名字是保留的。
//
// 为什么重要：findOrigin 取该 name 的**最高版本**。若允许用户建 github@2，
// source=github 会解析到 @2，而 @2 是自定义能力、不享受起点的
// 「任意事件类型」出口——于是用户已经建好的边全废，事件派不出去，
// 且没有任何提示。这个失败极其隐蔽，所以在写入处直接拒绝。
func TestOriginCapabilityNameReserved(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	if err := db.EnsureBuiltinCapabilities(ctx, orgID); err != nil {
		t.Fatalf("seed builtins: %v", err)
	}

	// 试图用内置起点的名字开新版本 → 拒绝
	for _, name := range []string{"github", "cron", "http", "manual"} {
		err := db.CreateCapability(ctx, &CapabilityVersion{
			OrgID:        orgID,
			Name:         name,
			Version:      2,
			ParamsSchema: json.RawMessage(`{"type":"object"}`),
			Outputs:      []string{"hijacked"},
			Source:       CapSourceCustom,
		})
		if !errors.Is(err, ErrCapabilityReserved) {
			t.Fatalf("%s@2 应被拒绝（ErrCapabilityReserved），got %v", name, err)
		}
	}

	// 但内置能力自己的播种必须放行（否则 ensureOneBuiltin 会拦死自己）
	if _, err := db.GetCapability(ctx, orgID, "github", 1); err != nil {
		t.Fatalf("内置 github@1 应已播种: %v", err)
	}

	// 非保留名不受影响
	if err := db.CreateCapability(ctx, &CapabilityVersion{
		OrgID:        orgID,
		Name:         "jira",
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{},
		Source:       CapSourceCustom,
	}); err != nil {
		t.Fatalf("自定义 jira@1 应允许: %v", err)
	}
}

// TestOriginEdgeAcceptsAnyPort 内置起点的出口是任意事件类型（不变量 I2 的唯一例外）。
func TestOriginEdgeAcceptsAnyPort(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	if err := db.EnsureBuiltinCapabilities(ctx, orgID); err != nil {
		t.Fatalf("seed builtins: %v", err)
	}

	github, err := db.GetCapability(ctx, orgID, "github", 1)
	if err != nil {
		t.Fatalf("get github: %v", err)
	}
	sink := &CapabilityVersion{
		OrgID: orgID, Name: "sink", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{}, Source: CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, sink); err != nil {
		t.Fatalf("create sink: %v", err)
	}

	// 任意未声明过的事件类型都应能建边
	if err := db.CreateEdge(ctx, &Edge{
		OrgID: orgID, FromCapability: github.ID,
		FromPort: "never.declared.before", ToCapability: sink.ID,
	}); err != nil {
		t.Fatalf("内置起点的任意出口应被接受: %v", err)
	}

	// 自定义能力仍然严格（I2 没有放松）
	custom := &CapabilityVersion{
		OrgID: orgID, Name: "custom", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"only"}, Source: CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, custom); err != nil {
		t.Fatalf("create custom: %v", err)
	}
	err = db.CreateEdge(ctx, &Edge{
		OrgID: orgID, FromCapability: custom.ID,
		FromPort: "not_declared", ToCapability: sink.ID,
	})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("自定义能力的非法出口应被拒绝，got %v", err)
	}
}

// TestBuiltinDisplayNames 内置能力必须**自带**显示名。
//
// 用户的原话是「默认的内置的我们给他取好了」——内置能力随项目发布，
// 名字该由我们定，不该让用户对着 github@1 猜这是什么。
func TestBuiltinDisplayNames(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	if err := db.EnsureBuiltinCapabilities(ctx, orgID); err != nil {
		t.Fatalf("ensure builtin: %v", err)
	}
	for _, bc := range BuiltinCapabilities {
		got, err := db.GetCapability(ctx, orgID, bc.Name, bc.Version)
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
	orgID, _ := newTestOrg(t, db)

	// 先播种，然后模拟"老库"：把 github 的显示名清空（就像这列刚加时那样）
	if err := db.EnsureBuiltinCapabilities(ctx, orgID); err != nil {
		t.Fatalf("ensure builtin: %v", err)
	}
	gh, err := db.GetCapability(ctx, orgID, "github", 1)
	if err != nil {
		t.Fatalf("get github: %v", err)
	}
	if err := db.SetCapabilityDisplayName(ctx, orgID, gh.ID, ""); err != nil {
		t.Fatalf("clear display name: %v", err)
	}
	// 再跑一次 Ensure（启动时会对所有组织跑一遍）→ 应该补回来
	if err := db.EnsureBuiltinCapabilities(ctx, orgID); err != nil {
		t.Fatalf("re-ensure builtin: %v", err)
	}
	gh2, _ := db.GetCapability(ctx, orgID, "github", 1)
	if gh2.DisplayName != "GitHub webhook" {
		t.Fatalf("backfill failed: %q", gh2.DisplayName)
	}

	// 用户改成自己的名字 → 不能被启动流程改回去
	if err := db.SetCapabilityDisplayName(ctx, orgID, gh.ID, "我们的 PR 入口"); err != nil {
		t.Fatalf("user rename: %v", err)
	}
	if err := db.EnsureBuiltinCapabilities(ctx, orgID); err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	gh3, _ := db.GetCapability(ctx, orgID, "github", 1)
	if gh3.DisplayName != "我们的 PR 入口" {
		t.Fatalf("startup overwrote user's display name: %q", gh3.DisplayName)
	}
}
