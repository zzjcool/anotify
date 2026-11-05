package route

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 P1.4「assign.expects 推导」的用例（设计 §3.4）。
//
// 要守住的三件事：
//   - 并集正确（两个下游分别要 a / b → required 同时有 a 和 b）
//   - **被字面量覆盖的键不进 required**（否则实现会去提供它，而它会被常量盖掉）
//   - 没有 link 的 port 不出现在 map 里（= 没约束，不是「要求空对象」）

// expectsFixture 在真库上建一组 link，供 BuildExpects 推导。
func newExpectsFixture(t *testing.T) (*emitFixture, *store.CapabilityVersion) {
	t.Helper()
	f := newEmitFixture(t)
	root := f.addCap(t, "analyze", 1, []string{"fix", "push", "quiet"}, `{"type":"object"}`)
	return f, root
}

// decodeSchema 把 asserts 里频繁要看的三个部分解出来。
func decodeSchema(t *testing.T, raw json.RawMessage) (props map[string]any, required []string) {
	t.Helper()
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var root struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decode expects schema %s: %v", raw, err)
	}
	if root.Type != "object" {
		t.Errorf("expects schema type = %q, want object（下游 params 永远是字典）", root.Type)
	}
	return root.Properties, root.Required
}

// TestBuildExpectsTargetsCarryPriority 候选清单带 priority，并按 (priority, capability) 升序。
//
// 为什么必须下发 priority：节点看到的是「这个端口能去哪」的**候选清单**，实际只有一个
// 会被选中。没有 priority，实现会把备选当成「要挨个调一遍」；顺序不对，它会把
// 兜底目标当成主选。
func TestBuildExpectsTargetsCarryPriority(t *testing.T) {
	f, root := newExpectsFixture(t)
	ctx := context.Background()
	// 名字顺序与优先级**反向**：zzz 的 priority 更小，必须排在前面。
	zzz := f.addCap(t, "zzz.primary", 1, nil, `{"type":"object"}`)
	aaa := f.addCap(t, "aaa.fallback", 1, nil, `{"type":"object"}`)
	for _, e := range []*store.Edge{
		{NamespaceID: f.ns, FromRef: root.Name, FromPort: "fix", ToRef: zzz.Name, Priority: 10},
		{NamespaceID: f.ns, FromRef: root.Name, FromPort: "fix", ToRef: aaa.Name, Priority: 20},
	} {
		if err := f.db.CreateEdge(ctx, e); err != nil {
			t.Fatalf("create edge %s: %v", e.ToRef, err)
		}
	}

	expects, err := f.d.BuildExpects(ctx, f.ns, root.ID, []string{"fix"})
	if err != nil {
		t.Fatalf("BuildExpects: %v", err)
	}
	targets := expects["fix"].Targets
	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want 2 条", targets)
	}
	if targets[0].Capability != "zzz.primary" || targets[0].Priority != 10 {
		t.Fatalf("targets[0] = %+v, want zzz.primary(p=10)（按 priority 升序）", targets[0])
	}
	if targets[1].Capability != "aaa.fallback" || targets[1].Priority != 20 {
		t.Fatalf("targets[1] = %+v, want aaa.fallback(p=20)", targets[1])
	}
}

// TestBuildExpectsUnionWithLiterals 两个下游分别要 a / b，各自带字面量覆盖 →
// 并集正确，且被覆盖的键不进 required（验收 ⑧）。
func TestBuildExpectsUnionWithLiterals(t *testing.T) {
	f, root := newExpectsFixture(t)

	// 下游 A：要 issue + to，但这条 link 把 to 钉成常量。
	notifier := f.addCap(t, "notifier", 3, nil,
		`{"type":"object","properties":{"issue":{"type":"string"},"to":{"type":"string"}},"required":["issue","to"]}`)
	// 下游 B：要 issue + patch + repo，这条 link 把 repo 钉成常量。
	fixer := f.addCap(t, "fixer", 1, nil,
		`{"type":"object","properties":{"issue":{"type":"string"},"patch":{"type":"string"},"repo":{"type":"string"}},"required":["issue","patch","repo"]}`)
	f.addEdge(t, root, "fix", notifier, `{"to":"u_1"}`)
	f.addEdge(t, root, "fix", fixer, `{"repo":"acme/x"}`)

	expects, err := f.d.BuildExpects(context.Background(), f.ns, root.ID, []string{"fix"})
	if err != nil {
		t.Fatalf("BuildExpects: %v", err)
	}
	exp, ok := expects["fix"]
	if !ok {
		t.Fatalf("fix 没有出现在 expects 里：%+v", expects)
	}

	props, required := decodeSchema(t, exp.Schema)
	wantRequired := []string{"issue", "patch"} // to / repo 被各自的字面量覆盖 → 不在 required 里
	sort.Strings(required)
	if len(required) != len(wantRequired) {
		t.Fatalf("required = %v, want %v", required, wantRequired)
	}
	for i, w := range wantRequired {
		if required[i] != w {
			t.Fatalf("required = %v, want %v", required, wantRequired)
		}
	}
	for _, k := range wantRequired {
		if _, ok := props[k]; !ok {
			t.Errorf("properties 缺字段 %q：%v", k, props)
		}
	}
	for _, k := range []string{"to", "repo"} {
		if _, ok := props[k]; ok {
			t.Errorf("properties 不该含被字面量覆盖的 %q：%v", k, props)
		}
	}

	// targets 逐条列出下游（给人/LLM 看）：能力名 + 版本 + 它自己的 needs。
	if len(exp.Targets) != 2 {
		t.Fatalf("targets = %+v, want 2 条", exp.Targets)
	}
	byName := map[string]ExpectTarget{}
	for _, tg := range exp.Targets {
		byName[tg.Capability] = tg
	}
	tn, ok := byName["notifier"]
	if !ok {
		t.Fatalf("targets 里没有 notifier：%+v", exp.Targets)
	}
	if tn.Version != 3 {
		t.Errorf("notifier 版本 = %d, want 3", tn.Version)
	}
	nProps, nReq := decodeSchema(t, tn.Needs)
	if _, ok := nProps["to"]; ok {
		t.Errorf("notifier.needs 不该含被字面量覆盖的 to：%v", nProps)
	}
	if len(nReq) != 1 || nReq[0] != "issue" {
		t.Errorf("notifier.needs.required = %v, want [issue]", nReq)
	}
	tf, ok := byName["fixer"]
	if !ok {
		t.Fatalf("targets 里没有 fixer：%+v", exp.Targets)
	}
	fProps, fReq := decodeSchema(t, tf.Needs)
	if len(fReq) != 2 {
		t.Errorf("fixer.needs.required = %v, want issue+patch", fReq)
	}
	if _, ok := fProps["repo"]; ok {
		t.Errorf("fixer.needs 不该含被字面量覆盖的 repo：%v", fProps)
	}
}

// TestBuildExpectsNoLinksAbsent 没有 link 的 port 不出现在 map 里（= 没约束）。
func TestBuildExpectsNoLinksAbsent(t *testing.T) {
	f, root := newExpectsFixture(t)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`)
	f.addEdge(t, root, "fix", fixer, "")

	expects, err := f.d.BuildExpects(context.Background(), f.ns, root.ID, []string{"fix", "push", "quiet"})
	if err != nil {
		t.Fatalf("BuildExpects: %v", err)
	}
	if _, ok := expects["fix"]; !ok {
		t.Fatalf("有 link 的 fix 没有出现：%+v", expects)
	}
	for _, port := range []string{"push", "quiet"} {
		if _, ok := expects[port]; ok {
			t.Errorf("没接下游的 port %q 出现在 expects 里了（会被误读成「必须传空对象」）", port)
		}
	}
}

// TestBuildExpectsEmptyNeedsSchema 下游什么都没要（或全被字面量覆盖）→ schema 省略，targets 仍在。
func TestBuildExpectsEmptyNeedsSchema(t *testing.T) {
	f, root := newExpectsFixture(t)
	// 下游只要 to，而 link 把 to 钉死 → 推导出的并集为空。
	notifier := f.addCap(t, "notifier", 1, nil,
		`{"type":"object","properties":{"to":{"type":"string"}},"required":["to"]}`)
	f.addEdge(t, root, "push", notifier, `{"to":"u_1"}`)

	expects, err := f.d.BuildExpects(context.Background(), f.ns, root.ID, []string{"push"})
	if err != nil {
		t.Fatalf("BuildExpects: %v", err)
	}
	exp, ok := expects["push"]
	if !ok {
		t.Fatalf("push 没有出现：%+v", expects)
	}
	if len(exp.Schema) != 0 {
		t.Fatalf("schema = %s, want 省略（下游什么都不要）", exp.Schema)
	}
	if len(exp.Targets) != 1 || exp.Targets[0].Capability != "notifier" {
		t.Fatalf("targets = %+v, want 单条 notifier", exp.Targets)
	}
	if len(exp.Targets[0].Needs) != 0 {
		t.Fatalf("notifier.needs = %s, want 省略", exp.Targets[0].Needs)
	}
}

// TestBuildExpectsEmptyPortList 空 port 列表 → 空 map（不是 nil、不报错、不查库）。
func TestBuildExpectsEmptyPortList(t *testing.T) {
	f, root := newExpectsFixture(t)
	expects, err := f.d.BuildExpects(context.Background(), f.ns, root.ID, nil)
	if err != nil {
		t.Fatalf("BuildExpects: %v", err)
	}
	if expects == nil || len(expects) != 0 {
		t.Fatalf("expects = %v, want 空 map（前端/帧都按 map 处理）", expects)
	}
}

// TestBuildExpectsNamespaceRequired 缺命名空间直接拒（不能回退默认命名空间）。
func TestBuildExpectsNamespaceRequired(t *testing.T) {
	f, root := newExpectsFixture(t)
	if _, err := f.d.BuildExpects(context.Background(), "", root.ID, []string{"fix"}); err == nil {
		t.Fatal("空命名空间被接受了")
	}
	// 未知能力：GetCapabilityByID 失败 → 向上抛（expects 缺失会让节点把参数拼错）。
	if _, err := f.d.BuildExpects(context.Background(), f.ns, "cap_nope", []string{"fix"}); err == nil {
		t.Fatal("未知能力被接受了")
	}
}

// TestBuildExpectsDedupesRequired 同一个字段被多个下游要求 → required 里只出现一次。
func TestBuildExpectsDedupesRequired(t *testing.T) {
	f, root := newExpectsFixture(t)
	a := f.addCap(t, "a", 1, nil, `{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`)
	b := f.addCap(t, "b", 1, nil, `{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`)
	f.addEdge(t, root, "fix", a, "")
	f.addEdge(t, root, "fix", b, "")

	expects, err := f.d.BuildExpects(context.Background(), f.ns, root.ID, []string{"fix"})
	if err != nil {
		t.Fatalf("BuildExpects: %v", err)
	}
	_, required := decodeSchema(t, expects["fix"].Schema)
	if len(required) != 1 || required[0] != "issue" {
		t.Fatalf("required = %v, want 单个 issue（同一字段被两个下游要求也只算一次）", required)
	}
}
