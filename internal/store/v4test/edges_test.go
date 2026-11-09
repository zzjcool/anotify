package v4test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 边投影（capability.md §1/§3/§8.1）的外部验收：next / targets 整体替换、
// 字面引用原样保存、无 port/priority/literals/mapping、环合法。

// edgeRefs 把 EdgesFrom 的结果摊平成字符串列表。
func edgeRefs(t *testing.T, db *store.DB, nsID string, fromIsSource bool, fromRef string) []string {
	t.Helper()
	targets, err := db.EdgesFrom(context.Background(), nsID, fromIsSource, fromRef)
	if err != nil {
		t.Fatalf("edges from %q: %v", fromRef, err)
	}
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, target.ToRef)
	}
	return out
}

// TestCapabilityEdgesProjection 能力边 = spec.next 的投影；改 next 整体替换。
func TestCapabilityEdgesProjection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	c := createCap(t, db, testCap(nsID, "review", 0, []string{"fixer", "notify@2"}))
	if got := edgeRefs(t, db, nsID, false, "review"); !sameStrings(got, []string{"fixer", "notify@2"}) {
		t.Fatalf("初始出边 = %v, want [fixer notify@2]", got)
	}

	if _, err := db.UpdateNext(ctx, nsID, "review", c.Version, []string{"fixer", "audit"}); err != nil {
		t.Fatalf("update next: %v", err)
	}
	if got := edgeRefs(t, db, nsID, false, "review"); !sameStrings(got, []string{"audit", "fixer"}) {
		t.Fatalf("改线后出边 = %v, want [audit fixer]（旧 notify@2 必须消失）", got)
	}

	// ReplaceCapabilityEdges 是同一份投影实现的外部入口（G1-4 写路径可调）。
	changed := *c
	changed.Spec.Next = []string{"only"}
	if err := db.ReplaceCapabilityEdges(ctx, &changed); err != nil {
		t.Fatalf("replace capability edges: %v", err)
	}
	if got := edgeRefs(t, db, nsID, false, "review"); !sameStrings(got, []string{"only"}) {
		t.Fatalf("replace 后出边 = %v, want [only]", got)
	}

	// 终端能力（next=[]）不产生边。
	createCap(t, db, testCap(nsID, "terminal", 0, nil))
	if got := edgeRefs(t, db, nsID, false, "terminal"); len(got) != 0 {
		t.Fatalf("终端能力产生了边：%v", got)
	}

	if _, err := db.EdgesFrom(ctx, "", false, "review"); !errors.Is(err, store.ErrNamespaceRequired) {
		t.Fatalf("缺 ns 应 ErrNamespaceRequired，实际 %v", err)
	}
	if _, err := db.EdgesFrom(ctx, nsID, false, ""); !errors.Is(err, store.ErrRefUnresolved) {
		t.Fatalf("空 from ref 应 ErrRefUnresolved，实际 %v", err)
	}
}

// TestSourceEdgesProjection 入口边 = targets 的投影；整体替换；与能力边互不干扰。
func TestSourceEdgesProjection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	src := &store.Source{
		NamespaceID: nsID,
		Name:        "deploy-hook",
		Targets: []store.SourceTarget{
			{CapabilityID: "deploy"},
			{CapabilityID: "notify@2"},
		},
	}
	if err := db.ReplaceSourceEdges(ctx, src); err != nil {
		t.Fatalf("replace source edges: %v", err)
	}
	if got := edgeRefs(t, db, nsID, true, "deploy-hook"); !sameStrings(got, []string{"deploy", "notify@2"}) {
		t.Fatalf("入口边 = %v, want [deploy notify@2]", got)
	}

	src.Targets = []store.SourceTarget{{CapabilityID: "audit"}}
	if err := db.ReplaceSourceEdges(ctx, src); err != nil {
		t.Fatalf("replace source edges again: %v", err)
	}
	if got := edgeRefs(t, db, nsID, true, "deploy-hook"); !sameStrings(got, []string{"audit"}) {
		t.Fatalf("替换后入口边 = %v, want [audit]", got)
	}

	edges, err := db.ListEdges(ctx, nsID)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 || !edges[0].FromIsSource || edges[0].FromRef != "deploy-hook" ||
		edges[0].ToRef != "audit" || edges[0].NamespaceID != nsID {
		t.Fatalf("ListEdges = %+v", edges)
	}

	if err := db.ReplaceSourceEdges(ctx, nil); err == nil {
		t.Fatal("nil source 必须拒绝")
	}
	if err := db.ReplaceSourceEdges(ctx, &store.Source{Name: "x"}); !errors.Is(err, store.ErrNamespaceRequired) {
		t.Fatalf("缺 ns 应 ErrNamespaceRequired，实际 %v", err)
	}
}

// TestRefResolution name / name@version 解析 + 环合法。
func TestRefResolution(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	v1 := createCap(t, db, testCap(nsID, "fixer", 0, nil))
	v2 := testCap(nsID, "fixer", 0, nil)
	v2.Spec.Run.Files = map[string]string{"run.sh": "#!/bin/sh\necho v2\n"}
	createCap(t, db, v2)
	if v2.Version <= v1.Version {
		t.Fatalf("版本没有前进：%d → %d", v1.Version, v2.Version)
	}

	latest, err := db.ResolveRef(ctx, nsID, "fixer")
	if err != nil {
		t.Fatalf("resolve latest: %v", err)
	}
	if latest.Version != v2.Version {
		t.Fatalf("裸名解析到 v%d, want latest v%d", latest.Version, v2.Version)
	}
	pinned, err := db.ResolveRef(ctx, nsID, store.FormatRef("fixer", v1.Version))
	if err != nil {
		t.Fatalf("resolve pinned: %v", err)
	}
	if pinned.Version != v1.Version {
		t.Fatalf("钉死引用解析到 v%d, want v%d", pinned.Version, v1.Version)
	}
	if _, err := db.ResolveRef(ctx, nsID, "fixer@999"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的版本应 ErrNotFound，实际 %v", err)
	}
	if _, err := db.ResolveRef(ctx, nsID, ""); !errors.Is(err, store.ErrRefUnresolved) {
		t.Fatalf("空引用应 ErrRefUnresolved，实际 %v", err)
	}

	name, version, isPinned := store.SplitRef("notify@2")
	if name != "notify" || version != 2 || !isPinned {
		t.Fatalf("SplitRef(notify@2) = %q/%d/%v", name, version, isPinned)
	}
	if name, _, isPinned := store.SplitRef("notify"); name != "notify" || isPinned {
		t.Fatalf("SplitRef(notify) = %q/%v", name, isPinned)
	}

	// 环合法：a → b → a 是正常编排（运行期 hop 预算兜底，不在写入期拒绝）。
	createCap(t, db, testCap(nsID, "a", 0, []string{"b"}))
	createCap(t, db, testCap(nsID, "b", 0, []string{"a"}))
	if got := edgeRefs(t, db, nsID, false, "a"); !sameStrings(got, []string{"b"}) {
		t.Fatalf("环 a 出边 = %v", got)
	}
	if got := edgeRefs(t, db, nsID, false, "b"); !sameStrings(got, []string{"a"}) {
		t.Fatalf("环 b 出边 = %v", got)
	}
}
