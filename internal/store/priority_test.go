package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 本文件是 `-- +migration 2`（edges.priority）的**往返门禁**。
//
// 「存什么读什么」是 AGENTS.md §3 对 store 层的硬要求：priority 在保存路径上做了归一
// （缺省 0），归一一旦和读取路径不一致，症状是「配了优先级却没生效」——这种只能靠
// 往返用例发现。
//
// 同块的另一列（capability_versions.balance）在 P3 已整列删除，
// 它的门禁现在是 `TestMigrationNineDropsBalance`（migrate_test.go）。

// TestEdgePriorityRoundTrip edges.priority 存什么读什么（ListEdges / EdgesFrom 两条读路径）。
func TestEdgePriorityRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	up := mkCap(nsID, "review", 1, []string{"fix"})
	low := mkCap(nsID, "fix-later", 1, nil)
	high := mkCap(nsID, "fix-first", 1, nil)
	mustCreateCap(t, db, up, low, high)

	// 一条显式 p=20、一条显式 p=10、一条不传（默认 0）。
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: low.Name, Priority: 20,
	}); err != nil {
		t.Fatalf("create low edge: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: high.Name, Priority: 10,
	}); err != nil {
		t.Fatalf("create high edge: %v", err)
	}
	// 默认值：不传 priority 的边是 0。
	other := mkCap(nsID, "notify", 1, nil)
	mustCreateCap(t, db, other)
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: other.Name,
	}); err != nil {
		t.Fatalf("create default-priority edge: %v", err)
	}

	// 读路径 ①：ListEdges（控制台的出边视图）。
	edges, err := db.ListEdges(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	want := map[string]int{low.Name: 20, high.Name: 10, other.Name: 0}
	for _, e := range edges {
		if got, ok := want[RefName(e.ToRef)]; !ok {
			t.Fatalf("unexpected edge %s → %s", e.FromRef, e.ToRef)
		} else if e.Priority != got {
			t.Errorf("ListEdges: %s priority = %d, want %d", e.ToRef, e.Priority, got)
		}
	}
	if len(edges) != len(want) {
		t.Fatalf("ListEdges 返回 %d 条，want %d", len(edges), len(want))
	}

	// 读路径 ②：EdgesFrom（派发主路径，候选选择只认它带出来的 priority）。
	targets, err := db.EdgesFrom(ctx, nsID, false, "review", "fix")
	if err != nil {
		t.Fatalf("edges from: %v", err)
	}
	gotPrio := map[string]int{}
	for _, tg := range targets {
		gotPrio[RefName(tg.ToRef)] = tg.Priority
	}
	for name, prio := range want {
		if gotPrio[name] != prio {
			t.Errorf("EdgesFrom: %s priority = %d, want %d", name, gotPrio[name], prio)
		}
	}

	// 改优先级是**原地改一列**（不是新的一条边）：行数不变、值变了。
	if err := db.UpdateEdgePriority(ctx, nsID, false, "review", "fix", low.Name, 5); err != nil {
		t.Fatalf("update edge priority: %v", err)
	}
	edges, err = db.ListEdges(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("list edges after update: %v", err)
	}
	if len(edges) != len(want) {
		t.Fatalf("改优先级改变了行数：%d，want %d", len(edges), len(want))
	}
	for _, e := range edges {
		if RefName(e.ToRef) == low.Name && e.Priority != 5 {
			t.Fatalf("%s priority = %d after update, want 5", low.Name, e.Priority)
		}
	}
	if err := db.UpdateEdgePriority(ctx, nsID, false, "review", "fix", "ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("改不存在的边 err = %v, want ErrNotFound", err)
	}
}

// TestEdgePriorityInGraphSnapshot 改优先级 = 改了「同一个端口落到谁头上」，必须落新版。
//
// 按需落版（契约 §10.2）的依据是 content_hash：priority 不进快照的话，改完优先级
// 算出的还是旧 hash，事后再也答不出「当时那个下游为什么是它」。
func TestEdgePriorityInGraphSnapshot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	up := mkCap(nsID, "review", 1, []string{"fix"})
	down := mkCap(nsID, "fixer", 1, nil)
	mustCreateCap(t, db, up, down)
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: down.Name, Priority: 10,
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	g1, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph: %v", err)
	}
	if err := db.UpdateEdgePriority(ctx, nsID, false, "review", "fix", down.Name, 20); err != nil {
		t.Fatalf("update priority: %v", err)
	}
	g2, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph after update: %v", err)
	}
	if g1.ID == g2.ID {
		t.Fatal("改了 priority 却算回同一张图：快照里没有 priority")
	}
	// 快照里能直接看到它是一个 20 的候选（omitempty 只省掉 0）。
	if !strings.Contains(string(g2.Snapshot), `"priority":20`) {
		t.Fatalf("快照里没有 priority：%s", g2.Snapshot)
	}
}

// TestCreateEdgeRejectsOutOfRangePriority 钉住「两条写路径同一条口径」。
//
// 背景：`ReplaceDownstreams`（PUT …/downstreams）一直查 [0,1000]，而
// `createEdgeTx`（POST /capabilities 的 links[]、/batch 的 links[]）以前**完全不查**
// —— 同一个值走 A 路径 400、走 B 路径静默入库。priority 直接决定候选顺序
// （小的先走），越界值会让「为什么这条线永远不跑」变成一件靠猜的事。
func TestCreateEdgeRejectsOutOfRangePriority(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	for _, name := range []string{"prio.src", "prio.dst"} {
		if err := db.CreateCapability(ctx, withSpec(mkCap(nsID, name, 1, []string{"out"}))); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	edge := func(priority int) error {
		return db.CreateEdge(ctx, &Edge{
			NamespaceID: nsID, FromRef: "prio.src", FromPort: "out",
			ToRef: "prio.dst", Priority: priority,
		})
	}
	for _, bad := range []int{-1, DownstreamPriorityMax + 1} {
		err := edge(bad)
		if err == nil {
			t.Fatalf("priority %d 应该被拒（越界会静默影响候选顺序）", bad)
		}
		if !strings.Contains(err.Error(), "priority_out_of_range") {
			t.Fatalf("priority %d 的错误码应可机器识别，got %v", bad, err)
		}
	}
	// 边界值合法（含 0 —— 它就是默认值，omitempty 只影响序列化）。
	for _, ok := range []int{0, DownstreamPriorityMin, DownstreamPriorityMax} {
		if err := edge(ok); err != nil {
			t.Fatalf("priority %d 应该合法: %v", ok, err)
		}
	}
}
