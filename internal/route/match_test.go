package route

import (
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// TestPlanFromSourceMatchGate 钉住 match.types 的三条语义：命中才 ready、不命中只留
// no_match（不建任务、不报失败）、且闸门排在 resolve 之前（不命中连查库都不查）。
func TestPlanFromSourceMatchGate(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "issues", 1, capOpts{})
	src := &store.Source{
		NamespaceID: ns.ID, Name: "gh", Kind: sourcespec.KindWebhook,
		Targets: []sourcespec.Target{
			{Capability: "issues", Match: &sourcespec.Match{Types: []string{"issues.opened"}}},
			{Capability: "issues", Match: &sourcespec.Match{Types: []string{"pull_request.opened"}}},
			{Capability: "ghost", Match: &sourcespec.Match{Types: []string{"pull_request.opened"}}},
			{Capability: "issues"},
		},
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-match", "issues.opened", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if ready := plan.Ready(); len(ready) != 2 {
		t.Fatalf("命中的两条 target 应 ready（含无 match 的那条），得到 %+v", plan.Targets)
	}
	if len(plan.Pruned) != 2 {
		t.Fatalf("应恰两条剪枝（pull_request 那条 + 未解析的 ghost 被 match 挡住），得到 %+v", plan.Pruned)
	}
	for i, prune := range plan.Pruned {
		if prune.Reason != protocol.ReasonNoMatch || prune.Detail == "" || prune.Capability == "" {
			t.Fatalf("剪枝事实 %d 必须是带 detail/capability 的 no_match: %+v", i, prune)
		}
	}
	if !protocol.IsPruneReason(protocol.ReasonNoMatch) {
		t.Fatalf("no_match 必须在剪枝闭集里: %v", protocol.PruneReasons())
	}
	// 不命中不产生失败任务（downstream_missing 属于「该派但解析不到」）：
	// ghost 这条匹配的类型是 pull_request.opened，本次不命中 → 0 条失败事实。
	if failures := plan.FailureInputs(); len(failures) != 0 {
		t.Fatalf("被 match 挡掉的边不该报 downstream_missing: %+v", failures)
	}
}

func TestPlanFromSourceCombinatorGate(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	testCapability(t, db, ns.ID, "issues", 1, capOpts{})
	prGate := sourcespec.MatchLeaf{Field: "issue.pull_request", Op: sourcespec.MatchOpExists, Negate: true}
	src := &store.Source{
		NamespaceID: ns.ID, Name: "gh-combinator", Kind: sourcespec.KindWebhook,
		Targets: []sourcespec.Target{{
			Capability: "issues",
			Match: &sourcespec.Match{
				Types: []string{"issues.labeled"},
				All:   []sourcespec.MatchLeaf{prGate},
				Any: []sourcespec.MatchLeaf{
					{Field: "issue.labels.0.name", Op: sourcespec.MatchOpIn, Value: []any{"bug"}},
				},
			},
		}},
		Enabled: true,
	}
	prPayload := map[string]any{"issue": map[string]any{
		"pull_request": map[string]any{"url": "u"},
		"labels":       []any{map[string]any{"name": "bug"}},
	}}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-pr", "issues.labeled", prPayload)
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Ready()) != 0 || len(plan.FailureInputs()) != 0 {
		t.Fatalf("PR-as-issue 不该派发也不该报失败: %+v", plan.Targets)
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Reason != protocol.ReasonNoMatch {
		t.Fatalf("应恰一条 no_match 剪枝: %+v", plan.Pruned)
	}
	if !strings.Contains(plan.Pruned[0].Detail, "match.all[0]") {
		t.Fatalf("detail 应点名 all 的第几行: %q", plan.Pruned[0].Detail)
	}
	issuePayload := map[string]any{"issue": map[string]any{
		"labels": []any{map[string]any{"name": "bug"}},
	}}
	plan, err = New(db).PlanFromSource(testCtx(), src, "trace-issue", "issues.labeled", issuePayload)
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Ready()) != 1 {
		t.Fatalf("非 PR + any 命中应 ready: %+v %+v", plan.Targets, plan.Pruned)
	}
	blockedPayload := map[string]any{"issue": map[string]any{
		"pull_request": map[string]any{"url": "u"},
		"labels":       []any{map[string]any{"name": "bug"}},
	}}
	plan, err = New(db).PlanFromSource(testCtx(), src, "trace-and", "issues.labeled", blockedPayload)
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Ready()) != 0 {
		t.Fatalf("any 命中不能替代 all 的 AND: %+v", plan.Targets)
	}
}

// TestPlanFromSourceNoMatchDoesNotResolve 证明闸门位置：只声明了不命中的 ghost 目标时，
// 事件不命中 → 0 target/0 失败事实（若闸门排在 resolve 之后，会多出一条 downstream_missing）。
func TestPlanFromSourceNoMatchDoesNotResolve(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	src := &store.Source{
		NamespaceID: ns.ID, Name: "gh2", Kind: sourcespec.KindWebhook,
		Targets: []sourcespec.Target{{
			Capability: "ghost", Match: &sourcespec.Match{Types: []string{"issues.opened"}},
		}},
		Enabled: true,
	}
	plan, err := New(db).PlanFromSource(testCtx(), src, "trace-nomatch", "ping", map[string]any{})
	if err != nil {
		t.Fatalf("PlanFromSource: %v", err)
	}
	if len(plan.Ready()) != 0 || len(plan.FailureInputs()) != 0 {
		t.Fatalf("不命中的边既不该 ready 也不该报失败: %+v %+v", plan.Targets, plan.FailureInputs())
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Reason != protocol.ReasonNoMatch {
		t.Fatalf("应恰一条 no_match 剪枝: %+v", plan.Pruned)
	}
}
