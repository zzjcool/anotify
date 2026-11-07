package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
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

// TestFinishTaskWritesOutcome 验证 FinishTask 的结果落库。
//
// 中枢执行的结果也走同一套规则：reported 恒为 1（写只发生在任务收尾时），
// 失败必须有一句原因（空时兜底 "failed"）。**没有出口维度**：`tasks.out` 列
// 已删掉（baseline 里没有这一列），出口只存在于 emission。
func TestFinishTaskWritesOutcome(t *testing.T) {
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

	// 成功：reported 恒 1，失败语义位为空
	id := mkRunning()
	if err := db.FinishTask(ctx, nsID, id, TaskSucceeded, "done", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ := db.GetTask(ctx, nsID, id)
	if !got.Reported || got.Status != TaskSucceeded {
		t.Errorf("成功落库错误: reported=%v status=%v", got.Reported, got.Status)
	}
	if got.Summary != "done" {
		t.Errorf("summary = %q, want done", got.Summary)
	}
	if got.Error != "" {
		t.Errorf("成功不该带 error，实际 %q", got.Error)
	}

	// 失败且一句原因都没有 → 兜底 "failed"（库里不允许失败没有说明）
	id = mkRunning()
	if err := db.FinishTask(ctx, nsID, id, TaskFailed, "", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ = db.GetTask(ctx, nsID, id)
	if got.Summary != "failed" || got.Error != "failed" {
		t.Errorf("失败原因兜底错误: summary=%q error=%q", got.Summary, got.Error)
	}
}

// TestFinishTaskRejectsInvalidStatus 验证只接受 succeeded|failed。
func TestFinishTaskRejectsInvalidStatus(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	err := db.FinishTask(ctx, nsID, "tsk_x", TaskCanceled, "", "")
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
	if err := db.CreateCapability(ctx, withSpec(&CapabilityVersion{
		NamespaceID:  nsID,
		Name:         "github",
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"done"},
		Source:       CapSourceCustom,
	})); err != nil {
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
	if err := db.CreateCapability(ctx, withSpec(sink)); err != nil {
		t.Fatalf("create sink: %v", err)
	}

	// 入口边：左端是来源名，任意事件类型都能建
	for _, port := range []string{"pull_request.opened", "never.declared.before", "x"} {
		if err := db.CreateEdge(ctx, &Edge{
			NamespaceID:  nsID,
			FromIsSource: true,
			FromRef:      "github",
			FromPort:     port,
			ToRef:        sink.ID,
		}); err != nil {
			t.Fatalf("入口边的任意出口应被接受（port=%s）: %v", port, err)
		}
	}

	// 下游能查到（派发第 ① 步的查表）
	got, err := db.EdgesFrom(ctx, nsID, true, "github", "pull_request.opened")
	if err != nil {
		t.Fatalf("edges from: %v", err)
	}
	if len(got) != 1 || got[0].ToRef != capRef(sink) {
		t.Fatalf("入口边查表 = %v, want [%s]", got, capRef(sink))
	}

	// 自定义能力的边仍然严格（I2 没有放松）
	custom := &CapabilityVersion{
		NamespaceID: nsID, Name: "custom", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"only"}, Source: CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, withSpec(custom)); err != nil {
		t.Fatalf("create custom: %v", err)
	}
	err = db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: custom.ID,
		FromPort: "not_declared", ToRef: sink.ID,
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

// TestBackfillBuiltinCapabilitiesCoversEveryNamespace 启动时的补种必须覆盖**每一个**命名空间。
//
// 为什么单独钉这条：#205 之后播种的作用域从「每个 org 一份」变成「每个命名空间一份」，
// 而 BackfillBuiltinCapabilities 是唯一的「启动时把漏掉的补上」的路径（server 启动时调用）。
// 漏掉一个命名空间不会有任何报错——那个命名空间里就再也声明不出 end 节点，
// 推送在那片区域静默失效。旧用例只覆盖单命名空间（SeedBuiltinCapabilities），
// 遍历路径没有测试。
func TestBackfillBuiltinCapabilitiesCoversEveryNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)              // 注册时已播种
	nsB := newTestExtraNS(t, db, "sandbox") // 建命名空间时也播种
	// 造出「半升级」状态：某个命名空间的内置能力被删了（老库 / 人工清理都可能）
	mustExec(t, db, `DELETE FROM capability_versions WHERE namespace_id = ? AND source = ?`, nsB, CapSourceBuiltin)
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND source = ?", nsB, CapSourceBuiltin); n != 0 {
		t.Fatalf("fixture 没清干净: %d 行内置能力", n)
	}

	n, err := db.BackfillBuiltinCapabilities(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n < 2 {
		t.Fatalf("backfill 只扫了 %d 个命名空间, want >= 2（漏扫 = 那边没有内置能力）", n)
	}
	// 两个命名空间都必须有内置能力，而且是各自一份（不是共享同一行）
	var idA, idB string
	for _, c := range []struct {
		ns string
		id *string
	}{{nsA, &idA}, {nsB, &idB}} {
		got, err := db.GetCapability(ctx, c.ns, "notify.phone", 1)
		if err != nil {
			t.Fatalf("命名空间 %s 补种后仍没有 notify.phone: %v", c.ns, err)
		}
		if got.Source != CapSourceBuiltin || got.NamespaceID != c.ns {
			t.Fatalf("notify.phone = {%s, %s}, want {builtin, %s}", got.Source, got.NamespaceID, c.ns)
		}
		*c.id = got.ID
	}
	if idA == idB {
		t.Fatal("两个命名空间共享同一个内置能力行（播种没按命名空间隔离）")
	}

	// 幂等：再跑一次不报错、不重复插、计数不变
	n2, err := db.BackfillBuiltinCapabilities(ctx)
	if err != nil {
		t.Fatalf("第二次 backfill: %v", err)
	}
	if n2 != n {
		t.Fatalf("第二次 backfill 扫了 %d 个命名空间, want %d", n2, n)
	}
	for _, ns := range []string{nsA, nsB} {
		rows := countRows(t, db, "capability_versions", "namespace_id = ? AND name = ? AND version = ?", ns, "notify.phone", 1)
		if rows != 1 {
			t.Fatalf("命名空间 %s 有 %d 行 notify.phone@1, want 1（重复播种）", ns, rows)
		}
	}
}

// TestBackfillBuiltinCapabilitiesEmptyDeployment 空库（还没注册用户）不该报错。
func TestBackfillBuiltinCapabilitiesEmptyDeployment(t *testing.T) {
	db := newTestDB(t)
	n, err := db.BackfillBuiltinCapabilities(context.Background())
	if err != nil {
		t.Fatalf("空库 backfill: %v", err)
	}
	if n != 0 {
		t.Fatalf("空库扫了 %d 个命名空间, want 0", n)
	}
}

// TestExampleBuiltinScriptEmitsDeclaredPort 真跑一遍示例能力的 run.sh，并断言它的
// 产出真的**走 emit**、端口**在声明里**。
//
// 改动前它叫 ...ProducesDeclaredOutput，断言的是「脚本把 echoed 写进了
// $ANOTIFY_OUT_FILE」。P1.5 把那条腿删了（emit 是唯一的产出通道），所以同样的证据
// 换了载体：脚本必须调 `anotify-agent emit --port <声明过的端口>`。
//
// 为什么必须真跑：script.echo 曾经 declare outputs=["echoed"]，而脚本把**参数值**
// 写进了出口文件（那是出口名，不是结果），于是任何 message 都让任务在最后一步判
// failed。TestExampleBuiltinIsUsable 只比"产物 outputs == 能力 outputs"，两边一起
// 错就测不出来。
//
// 这里用一个**假 anotify-agent**（只记录 argv 就退 0）代替真二进制：store 包不该
//
//	① 依赖 cmd/agent 能编出来（那是另一个包的验收：cmd/agent 的
//	   TestRunJobExampleCapability 用真二进制跑同一条脚本）；
//	② 把 socket 协议搬进 store 的单测里（那属于 emit 协议自己的用例）。
func TestExampleBuiltinScriptEmitsDeclaredPort(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("没有 bash，跳过（示例能力只用 bash 内建）")
	}
	script := ExampleCapability.Artifact.Spec.Run.Files["run.sh"]
	if script == "" {
		t.Fatal("示例能力的产物里没有 run.sh")
	}
	// 静态门禁：已退役的出口文件**不能再出现在脚本的代码里**。断言「没有」而不是
	// 「不用」——否则哪天有人“顺手兼容一下”就又长回来了。
	// 只看非注释行：注释里解释「它已退役」是应该的（也正因为那段注释，这里不能
	// 简单地 strings.Contains 整个脚本文本）。
	for _, line := range strings.Split(script, "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		for _, banned := range []string{"ANOTIFY_OUT_FILE", ".anotify-out"} {
			if strings.Contains(code, banned) {
				t.Fatalf("示例脚本的代码仍然引用已退役的出口文件 %q（产出只走 emit）: %s", banned, code)
			}
		}
	}

	cases := []struct {
		name string
		env  string // 空 = 不传参数，走脚本里的默认值
		want string // 期望在 stdout 里看到的文本
	}{
		{name: "指定消息", env: "hello 世界", want: "hello 世界"},
		{name: "不传参数走默认值", env: "", want: "hello from Anotify"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			entry := filepath.Join(dir, "run.sh")
			if err := os.WriteFile(entry, []byte(script), 0o755); err != nil {
				t.Fatalf("写 run.sh: %v", err)
			}
			record := filepath.Join(dir, "emit-args.txt")
			// 假 agent：把 argv **一个一行**写进记录文件就成功退出（真 CLI 的成功退出码是 0）。
			// 一个一行而不是 `"$*"`：data 里会有空格（比如 message="hello 世界"），
			// 拼成一行之后就没法无歧义地拆回来了。
			fakeBin := filepath.Join(dir, "anotify-agent")
			fake := "#!/bin/bash\nprintf '%s\\n' \"$@\" > " + record + "\nexit 0\n"
			if err := os.WriteFile(fakeBin, []byte(fake), 0o755); err != nil {
				t.Fatalf("写假 agent: %v", err)
			}

			cmd := exec.Command(bash, entry)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				// 有 socket = 脚本会真的去 emit（没有它时脚本按「standalone 无中枢」跳过，
				// 那是另一条分支，见 cmd/agent 的 standalone 用例）。
				"ANOTIFY_EMIT_SOCK="+filepath.Join(dir, "task.sock"),
				"ANOTIFY_AGENT_BIN="+fakeBin,
				// agent 一定会注入它（buildEnv 的契约变量），这里给一份合法 JSON 对象。
				// 用 strconv/fmt 拼：Go 的原始字符串里带反引号会很别扭，
				// 而 JSON 的引号必须是真的双引号字符。
				"ANOTIFY_PARAMS="+fmt.Sprintf(`{"message":%q}`, tc.env),
			)
			if tc.env != "" {
				cmd.Env = append(cmd.Env, "ANOTIFY_PARAM_MESSAGE="+tc.env)
			}
			stdout, err := cmd.Output()
			if err != nil {
				t.Fatalf("run.sh 执行失败：%v", err)
			}
			if got := string(stdout); !strings.Contains(got, tc.want) {
				t.Fatalf("stdout = %q，期望包含 %q", got, tc.want)
			}

			raw, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("脚本没有调 emit（任务会「静默没有下游」，这正是要消灭的失败）: %v", err)
			}
			argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			if len(argv) == 0 || argv[0] != "emit" {
				t.Fatalf("记录到的 argv = %q，期望 [emit ...]", argv)
			}
			// 示例脚本**不该提到端口**：端口是内部路由键，单出口能力由 agent 从
			// 「出口契约」自动取（CONTRACT-hide-ports.md §2）。这条断言防的是
			// "顺手把端口写回来"——那样每个照抄示例的人都会被逼着去抄一个内部名字。
			if port := flagValue(argv, "--port"); port != "" {
				t.Fatalf("示例脚本不该写 --port（端口是内部路由键），实际 %q", port)
			}
			if to := flagValue(argv, "--to"); to != "" && !slices.ContainsFunc(
				[]string{"script.echo", "notify.phone"}, func(n string) bool { return to == n }) {
				t.Fatalf("示例脚本的 --to %q 不是候选能力名", to)
			}
			// data 必须是合法 JSON 对象（emit 的结构门槛）。
			data := flagValue(argv, "--data")
			var obj map[string]any
			if err := json.Unmarshal([]byte(data), &obj); err != nil {
				t.Fatalf("emit --data %q 不是 JSON 对象：%v", data, err)
			}
			if tc.env != "" {
				if got, _ := obj["message"].(string); got != tc.env {
					t.Fatalf("emit.data.message = %v，期望参数原样透传 %q", obj["message"], tc.env)
				}
			}
		})
	}
}

// TestExampleBuiltinIsUsable 守三件事（缺一条，「新账号第一次就能跑通」就是空话）：
//
//	① 产物本身合法（artifact.Validate 通过）—— 不合法的话部署时才会报错
//	② 播种后 artifacts 表里真有它，且 digest 与本地重算一致（内容寻址）
//	③ 重复播种幂等（老库升级会再跑一遍，不能插出第二行）
func TestExampleBuiltinIsUsable(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)

	// ① 产物合法 + 与能力定义的 outputs 一致
	if err := artifact.Validate(ExampleCapability.Artifact); err != nil {
		t.Fatalf("示例能力的产物不合法：%v", err)
	}
	if got, want := ExampleCapability.Artifact.Spec.Outputs, ExampleCapability.Outputs; len(got) != len(want) {
		t.Fatalf("产物 outputs=%v 与能力定义 outputs=%v 不一致", got, want)
	}

	// ② 播种后有 artifact 行，digest 一致
	wantDigest, err := artifact.Digest(ExampleCapability.Artifact)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	got, err := db.GetArtifactByDigest(ctx, nsID, wantDigest)
	if err != nil {
		t.Fatalf("新命名空间里应该已经有示例能力的产物（否则部署不了）：%v", err)
	}
	if got.Digest != wantDigest {
		t.Fatalf("artifact digest = %q, want %q", got.Digest, wantDigest)
	}

	// ③ 重复播种幂等：行数不变
	before := countRows(t, db, "artifacts", "namespace_id = ?", nsID)
	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	after := countRows(t, db, "artifacts", "namespace_id = ?", nsID)
	if after != before {
		t.Fatalf("重复播种把 artifact 行数从 %d 变成了 %d（幂等破了）", before, after)
	}
}

// TestExampleBuiltinArtifactUpdateRepointsDeployment 内置产物更新后（比如修了脚本的 bug），
// 已有部署会被自动指到新产物上并回到 pending，等节点上线时 ws.resendDeployments 补推——
// 不需要人去控制台逐台重部署。
//
// 这是“中枢覆盖”在播种侧的对应物：播种幂等键是 name@version，所以修脚本不会
// 换能力行，只能靠这里把已有的部署行指过去（否则旧 digest 会永远指着旧内容）。
func TestExampleBuiltinArtifactUpdateRepointsDeployment(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)

	cap1, err := db.GetCapability(ctx, nsID, ExampleCapability.Name, ExampleCapability.Version)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	latest, err := db.LatestArtifact(ctx, nsID, cap1.ID)
	if err != nil {
		t.Fatalf("latest artifact: %v", err)
	}

	// 造一份“旧产物”的已应用部署（模拟上一版播种出来的那份）
	node, _ := mkNode(t, db, nsID, "box")
	old := dpArtifact(t, db, nsID, cap1.ID, dpDigest("dead"), `{"old":true}`)
	dep, err := db.CreateDeployment(ctx, nsID, node.ID, old.ID)
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if err := db.MarkDeployResult(ctx, nsID, dep.ID, DeploymentApplied, ""); err != nil {
		t.Fatalf("mark applied: %v", err)
	}

	// 重启（= 重新播种）
	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("reseed: %v", err)
	}

	rows, err := db.ListDeployments(ctx, nsID, DeploymentFilter{NodeID: node.ID, CapabilityID: cap1.ID})
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("deployments 行数 = %d, want 1（一个能力一条期望）", len(rows))
	}
	got := rows[0]
	if got.ArtifactID != latest.ID || got.Digest != latest.Digest {
		t.Fatalf("部署没被指到新产物：artifact=%s digest=%s，want %s / %s",
			got.ArtifactID, got.Digest, latest.ID, latest.Digest)
	}
	if got.State != DeploymentPending {
		t.Fatalf("重指后 state = %q, want %q（等节点上线补推）", got.State, DeploymentPending)
	}
	if !strings.Contains(got.Reason, "builtin artifact updated") {
		t.Fatalf("reason = %q，应该说明是内置产物更新导致的", got.Reason)
	}

	// 幂等：再一次播种不会把 pending 打回别处、也不会变出第二行
	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("reseed twice: %v", err)
	}
	again, err := db.ListDeployments(ctx, nsID, DeploymentFilter{NodeID: node.ID, CapabilityID: cap1.ID})
	if err != nil {
		t.Fatalf("list deployments again: %v", err)
	}
	if len(again) != 1 || again[0].ArtifactID != latest.ID {
		t.Fatalf("重复播种把部署弄乱了: %+v", again)
	}

	// 旧库形状：同一个能力上两行（旧版允许并存）。播种要收敛成一行，
	// 而且**保留最新那条**（它才是用户最后一次部署的意图）。
	// 先清掉当前那条，才能造出“两行指向不同产物”的旧库形状。
	if _, err := db.ExecContext(ctx, `DELETE FROM deployments WHERE namespace_id = ? AND node_id = ?`, nsID, node.ID); err != nil {
		t.Fatalf("clear deployments: %v", err)
	}
	newerID := NewDeploymentID()
	now := Now() + 1000
	if _, err := db.ExecContext(ctx,
		`INSERT INTO deployments (id, namespace_id, node_id, artifact_id, digest, state, reason, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		newerID, nsID, node.ID, latest.ID, latest.Digest, DeploymentApplied, "", now, now); err != nil {
		t.Fatalf("insert legacy duplicate: %v", err)
	}
	// 再加一条更早的旧行（模拟旧版本留下的“历史”）
	if _, err := db.ExecContext(ctx,
		`INSERT INTO deployments (id, namespace_id, node_id, artifact_id, digest, state, reason, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		NewDeploymentID(), nsID, node.ID, old.ID, old.Digest, DeploymentApplied, "", now-2000, now-2000); err != nil {
		t.Fatalf("insert legacy stale row: %v", err)
	}

	if err := db.SeedBuiltinCapabilities(ctx, nsID); err != nil {
		t.Fatalf("reseed legacy: %v", err)
	}
	legacy, err := db.ListDeployments(ctx, nsID, DeploymentFilter{NodeID: node.ID, CapabilityID: cap1.ID})
	if err != nil {
		t.Fatalf("list deployments legacy: %v", err)
	}
	if len(legacy) != 1 {
		t.Fatalf("旧库多行没被收敛: %d 行", len(legacy))
	}
	if legacy[0].ID != newerID {
		t.Fatalf("收敛时保留的不是最新那条：id=%s, want %s", legacy[0].ID, newerID)
	}
	if legacy[0].ArtifactID != latest.ID {
		t.Fatalf("收敛后没指到当前产物: %s", legacy[0].ArtifactID)
	}
}

// TestExampleBuiltinBackfillsExistingNamespace：老命名空间（能力已存在、产物没有）
// 在 Backfill 之后也要拿到产物 —— 这是升级路径，光靠"新建时播种"覆盖不到。
func TestExampleBuiltinBackfillsExistingNamespace(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)

	var capID string
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM capability_versions WHERE namespace_id = ? AND name = ?`,
		nsID, ExampleCapability.Name).Scan(&capID); err != nil {
		t.Fatalf("capability 应该已被播种：%v", err)
	}
	// 模拟"老库"：能力在，产物不在
	if _, err := db.ExecContext(ctx, `DELETE FROM artifacts WHERE namespace_id = ?`, nsID); err != nil {
		t.Fatalf("清掉产物: %v", err)
	}
	if n := countRows(t, db, "artifacts", "namespace_id = ?", nsID); n != 0 {
		t.Fatalf("前置条件：产物应为 0 行，实际 %d", n)
	}

	if _, err := db.BackfillBuiltinCapabilities(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n := countRows(t, db, "artifacts", "namespace_id = ? AND capability_id = ?", nsID, capID); n != 1 {
		t.Fatalf("backfill 后示例能力的产物应有 1 行，实际 %d", n)
	}
}

// flagValue 从 argv 里取 `--flag value` 形式的值（没有就返回空串）。
func flagValue(argv []string, flag string) string {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}
