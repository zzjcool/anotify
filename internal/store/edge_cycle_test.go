package store

import (
	"context"
	"testing"
)

// ---------- 环是合法编排（契约 §2） ----------
//
// 历史：这里原本是**静态环检测**（P2.3）——保存时按名字做一次 DFS，能走回自己就拒。
// 现在反过来：`review → fix → review` 是用户拍板的合法编排，配置期一律放行。
//
// 兜底在运行期，而且**不是**「名字判重」（那会把只走一圈的合法链也拒掉），
// 而是 hop 预算：depth = len(path)，depth >= max_hops → 剪枝
// （internal/route/dispatch.go 的 ReasonMaxHops / ReasonBudget）。
//
// 所以本文件现在守护的是：**成环的边必须存得下、读得回**（两个方向都是）。
// 菱形与 1:N 扇出的断言一条都没有放宽——它们是无环图的合法形状，与环的放行无关。

func TestCreateEdgeAllowsSelfLoop(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns, _ := newTestNS(t, db)

	capA := mkCap(ns, "a", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	// 自连 = 长度 1 的环：合法（每绕一圈 path 长 1，hop 预算会兜住）。
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: ns, FromRef: capA.ID, FromPort: "ok", ToRef: capA.ID}); err != nil {
		t.Fatalf("自连应当可以保存（契约 §2）：%v", err)
	}
	got, err := db.EdgesFrom(ctx, ns, false, "a", "ok")
	if err != nil {
		t.Fatalf("edges from a: %v", err)
	}
	if len(got) != 1 || got[0].ToRef != capRef(capA) {
		t.Fatalf("自连读不回来: %+v", got)
	}
}

func TestCreateEdgeAllowsCycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns, _ := newTestNS(t, db)

	// a --ok--> b --ok--> c --ok--> a
	a := mkCap(ns, "a", 1, []string{"ok"})
	b := mkCap(ns, "b", 1, []string{"ok"})
	c := mkCap(ns, "c", 1, []string{"ok"})
	for _, cap := range []*CapabilityVersion{a, b, c} {
		if err := db.CreateCapability(ctx, cap); err != nil {
			t.Fatalf("create capability %s: %v", cap.Name, err)
		}
	}
	cycle := []*Edge{
		{NamespaceID: ns, FromRef: a.ID, FromPort: "ok", ToRef: b.ID},
		{NamespaceID: ns, FromRef: b.ID, FromPort: "ok", ToRef: c.ID},
		{NamespaceID: ns, FromRef: c.ID, FromPort: "ok", ToRef: a.ID}, // 闭环：以前这里被拒
	}
	for _, e := range cycle {
		if err := db.CreateEdge(ctx, e); err != nil {
			t.Fatalf("成环的边应当可以保存（契约 §2）：%v", err)
		}
	}

	// 「两个方向都能读到」：环上每一跳都是真实可查的边（派发才走得动）。
	for _, tc := range []struct{ from, to string }{
		{"a", "b"}, {"b", "c"}, {"c", "a"},
	} {
		got, err := db.EdgesFrom(ctx, ns, false, tc.from, "ok")
		if err != nil {
			t.Fatalf("edges from %s: %v", tc.from, err)
		}
		if len(got) != 1 || RefName(got[0].ToRef) != tc.to {
			t.Fatalf("%s → %s 读不回来: %+v", tc.from, tc.to, got)
		}
	}
	// ListEdges 也要能看到闭环那一条（控制台的「出边」视图靠它）。
	edges, err := db.ListEdges(ctx, ns, "c")
	if err != nil {
		t.Fatalf("list edges of c: %v", err)
	}
	if len(edges) != 1 || RefName(edges[0].ToRef) != "a" {
		t.Fatalf("c 的出边应当只有 c → a: %+v", edges)
	}

	// 反例仍然成立：一条指向不存在能力的边必须被拒（放行环不等于放行一切）。
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: ns, FromRef: c.ID, FromPort: "ok", ToRef: "ghost"}); err == nil {
		t.Fatal("指向不存在能力的边被接受了：引用解析不该被这次改动放松")
	}
}

// TestCreateEdgeAllowsDiamond 菱形（a→b, a→c, b→d, c→d）是无环的：共享下游不是环。
//
// 这条是防止「环检测写成了『重复到达同一个节点就拒』」——那样 1:N 扇出会被误杀，
// 而扇出是这个产品的核心能力（I7）。契约 §2 放行环之后它依然必须绿：
// 放行环**不是**把「共享下游」当成环。
func TestCreateEdgeAllowsDiamond(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns, _ := newTestNS(t, db)

	a := mkCap(ns, "a", 1, []string{"ok"})
	b := mkCap(ns, "b", 1, []string{"ok"})
	c := mkCap(ns, "c", 1, []string{"ok"})
	d := mkCap(ns, "d", 1, nil)
	for _, cap := range []*CapabilityVersion{a, b, c, d} {
		if err := db.CreateCapability(ctx, cap); err != nil {
			t.Fatalf("create capability %s: %v", cap.Name, err)
		}
	}
	edges := []*Edge{
		{NamespaceID: ns, FromRef: a.ID, FromPort: "ok", ToRef: b.ID},
		{NamespaceID: ns, FromRef: a.ID, FromPort: "ok", ToRef: c.ID},
		{NamespaceID: ns, FromRef: b.ID, FromPort: "ok", ToRef: d.ID},
		{NamespaceID: ns, FromRef: c.ID, FromPort: "ok", ToRef: d.ID},
	}
	for _, e := range edges {
		if err := db.CreateEdge(ctx, e); err != nil {
			t.Fatalf("菱形结构应当合法，却被拒：%v", err)
		}
	}
}

// TestCreateEdgeEntryEdgeNeverCycles 入口边没有上游能力，接不出环；
// 而且它和「能力边成环」是两件事：入口边照旧永远合法。
func TestCreateEdgeEntryEdgeNeverCycles(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns, _ := newTestNS(t, db)

	a := mkCap(ns, "a", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, a); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: ns, FromIsSource: true, FromRef: "github", FromPort: "push", ToRef: a.ID,
	}); err != nil {
		t.Fatalf("入口边应当永远合法：%v", err)
	}
	// 自连（a → a）也合法——它是长度 1 的环，由 hop 预算兜底，不是配置错误。
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: ns, FromRef: a.ID, FromPort: "ok", ToRef: a.ID}); err != nil {
		t.Fatalf("自连应当可以保存：%v", err)
	}
	// 入口边不进环：它没有上游能力，所以「来源 → a」不会让 a 的出边成环。
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: ns, FromRef: a.ID, FromPort: "ok", ToRef: a.ID}); err != nil {
		t.Fatalf("重复的自连是幂等的（ON CONFLICT DO NOTHING），不该报错：%v", err)
	}
}
