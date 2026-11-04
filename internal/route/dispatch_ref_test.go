package route

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// P2.2 的**派发侧**验收：link 上的引用在**每次派发时**解析（设计 §3.2 / §7）。
//
// 为什么这条必须在 route 层再验一遍（store 层已有引用语义的单测）：
// 「升级生效」这句话的完整含义是「下一次派发解析到新版」——解析点必须落在
// plan 里，而且必须**每次**重新解析。store 只管存引用，谁去解析、什么时候解析
// 是 route 的事；两处任一走偏，用户看到的就是「升了版没生效」或「升了版偷偷改线」。
//
// 这条用例用**真 store**（不是 mock）：它要证明的正是「真库 + 真解析 + 真派发」这条路上
// 的行为，mock 出来的语义只能证明 mock 自己对。

// realStoreFixture 建一个真库 + 命名空间，返回 (db, nsID)。
func realStoreFixture(t *testing.T) (*store.DB, string) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	u := &store.User{ID: store.NewUserID(), Username: "u-" + store.NewUserID(), CreatedAt: store.Now()}
	ns, err := db.RegisterUser(ctx, u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	return db, ns.ID
}

// TestDispatchResolvesLinkRef 裸名字跟随 latest、`name@version` 钉死——
// 同一次派发里两种引用各自按自己的语义解析。
func TestDispatchResolvesLinkRef(t *testing.T) {
	ctx := context.Background()
	db, nsID := realStoreFixture(t)

	review := &store.CapabilityVersion{
		NamespaceID: nsID, Name: "review", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"}}}`),
		// 两个出口：一个接「跟随 latest」的 link，一个接「钉死 @1」的 link。
		Outputs: []string{"fix", "fix-pinned"},
		Source:  store.CapSourceCustom,
	}
	fixer1 := &store.CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"}}}`),
		Source:       store.CapSourceCustom,
	}
	for _, c := range []*store.CapabilityVersion{review, fixer1} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create %s@%d: %v", c.Name, c.Version, err)
		}
	}
	// 一条跟随 latest、一条钉死 @1
	if err := db.CreateEdge(ctx, &store.Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: "fixer",
	}); err != nil {
		t.Fatalf("create link (follow): %v", err)
	}
	if err := db.CreateEdge(ctx, &store.Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix-pinned", ToRef: "fixer@1",
	}); err != nil {
		t.Fatalf("create link (pinned): %v", err)
	}

	// 一台节点同时声明 @1 与 @2（否则「解析到新版」会因为找不到节点而被剪枝，
	// 那种失败看起来像「升级没生效」，会把这条用例的意义搞混）。
	node := &store.Node{NamespaceID: nsID, Name: "nd-fixer"}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.ConfirmCapabilities(ctx, nsID, node.ID, []store.CapabilityRef{
		{Name: "fixer", Version: 1},
	}); err != nil {
		t.Fatalf("declare fixer@1: %v", err)
	}

	disp := New(db)

	// 第一次派发：latest = @1
	out1, err := disp.Dispatch(ctx, DispatchInput{
		NamespaceID: nsID, TraceID: "trc_ref_1",
		FromCap: "review", Out: "fix",
		Params: map[string]any{"issue": "#1"},
		Path:   []string{"review"},
	})
	if err != nil {
		t.Fatalf("dispatch before upgrade: %v", err)
	}
	if len(out1.Tasks) != 1 {
		t.Fatalf("before upgrade: tasks = %d, want 1 (%+v)", len(out1.Tasks), out1.Pruned)
	}
	if out1.Tasks[0].Version != 1 {
		t.Fatalf("before upgrade: version = %d, want 1", out1.Tasks[0].Version)
	}
	graph1 := out1.Tasks[0].GraphID

	// 升级 fixer@2（加可选入参 → 升级闸门放行）+ 节点声明新版
	fixer2 := &store.CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"},"note":{"type":"string"}}}`),
		Source:       store.CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, fixer2); err != nil {
		t.Fatalf("create fixer@2: %v", err)
	}
	if _, err := db.ConfirmCapabilities(ctx, nsID, node.ID, []store.CapabilityRef{
		{Name: "fixer", Version: 1}, {Name: "fixer", Version: 2},
	}); err != nil {
		t.Fatalf("declare fixer@2: %v", err)
	}

	// 第二次派发（**边一行没改**）：裸名字的引用解析到 @2 —— 升级生效
	out2, err := disp.Dispatch(ctx, DispatchInput{
		NamespaceID: nsID, TraceID: "trc_ref_2",
		FromCap: "review", Out: "fix",
		Params: map[string]any{"issue": "#2"},
		Path:   []string{"review"},
	})
	if err != nil {
		t.Fatalf("dispatch after upgrade: %v", err)
	}
	if len(out2.Tasks) != 1 {
		t.Fatalf("after upgrade: tasks = %d, want 1 (%+v)", len(out2.Tasks), out2.Pruned)
	}
	task := out2.Tasks[0]
	if task.Version != 2 || task.CapabilityID != fixer2.ID {
		t.Fatalf("after upgrade: 派发到了 %s@%d, want fixer@2（裸名字必须跟随 latest）",
			task.Capability, task.Version)
	}
	// path 是**能力名**序列（环剪枝按名字判）
	if len(task.Path) != 2 || task.Path[0] != "review" || task.Path[1] != "fixer" {
		t.Fatalf("path = %v, want [review fixer]", task.Path)
	}
	// 图版本不变：边没改，升级不该凭空产生新图（P2.2 §7.4）
	if task.GraphID != graph1 {
		t.Fatalf("升级换了图版本 %s → %s（边没改，图就不该变）", graph1, task.GraphID)
	}

	// 第三次派发：走钉死 @1 的那条边 —— 即使 @2 存在也仍然是 @1
	out3, err := disp.Dispatch(ctx, DispatchInput{
		NamespaceID: nsID, TraceID: "trc_ref_3",
		FromCap: "review", Out: "fix-pinned",
		Params: map[string]any{"issue": "#3"},
		Path:   []string{"review"},
	})
	if err != nil {
		t.Fatalf("dispatch pinned: %v", err)
	}
	if len(out3.Tasks) != 1 {
		t.Fatalf("pinned: tasks = %d, want 1 (%+v)", len(out3.Tasks), out3.Pruned)
	}
	if out3.Tasks[0].Version != 1 {
		t.Fatalf("pinned: 派发到了 @%d, want @1（钉死的引用不受升级影响）", out3.Tasks[0].Version)
	}
}

// TestDispatchPrunesUnresolvableLinkRef 指向已删能力的 link 只剪自己那条，不拖垮整次派发。
//
// 为什么要有这条：引用是**字符串**，能力行删掉之后边还留着（没有外键兜底）。
// 那种 link 必须在派发时可见地失败（pruned，带 detail），而不是让整个扇出 500。
func TestDispatchPrunesUnresolvableLinkRef(t *testing.T) {
	ctx := context.Background()
	db, nsID := realStoreFixture(t)

	review := &store.CapabilityVersion{
		NamespaceID: nsID, Name: "review", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"fix"},
		Source:       store.CapSourceCustom,
	}
	good := &store.CapabilityVersion{
		NamespaceID: nsID, Name: "good", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Source:       store.CapSourceCustom,
	}
	for _, c := range []*store.CapabilityVersion{review, good} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create %s@%d: %v", c.Name, c.Version, err)
		}
	}
	node := &store.Node{NamespaceID: nsID, Name: "nd-good"}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.ConfirmCapabilities(ctx, nsID, node.ID, []store.CapabilityRef{{Name: "good", Version: 1}}); err != nil {
		t.Fatalf("declare good@1: %v", err)
	}

	if err := db.CreateEdge(ctx, &store.Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: "good",
	}); err != nil {
		t.Fatalf("create good link: %v", err)
	}
	// 手写一条指向不存在能力的 link（模拟「能力行被删、边还在」）。
	// CreateEdge 会拒掉它（配置期校验），所以这里直插——就是要造出那个坏形状。
	if _, err := db.Exec(`INSERT INTO edges
		(namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, created_at)
		VALUES (?, 0, 'review', 'fix', 'ghost@1', '{}', '{}', 1)`, nsID); err != nil {
		t.Fatalf("insert dangling link: %v", err)
	}

	out, err := New(db).Dispatch(ctx, DispatchInput{
		NamespaceID: nsID, TraceID: "trc_ghost",
		FromCap: "review", Out: "fix",
	})
	if err != nil {
		t.Fatalf("一条坏 link 不该让整次派发失败: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].Capability != "good" {
		t.Fatalf("tasks = %+v, want 恰好 1 条到 good（坏 link 只剪自己）", out.Tasks)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamSchema {
		t.Fatalf("pruned = %+v, want 一条 param_schema（引用解析不到）", out.Pruned)
	}
	// detail 必须点名那条坏引用：否则「这条边为什么没走」在 trace 里查不下去。
	if !strings.Contains(out.Pruned[0].Detail, "ghost@1") {
		t.Fatalf("剪枝 detail 里没点名坏引用: %q", out.Pruned[0].Detail)
	}
}
