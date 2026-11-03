package store

import (
	"context"
	"errors"
	"testing"
)

// ---------- 静态环检测（P2.3） ----------
//
// 为什么在保存时拒，而不是只靠运行期 path 剪枝：
// 环是**配置错误**。放它进去，每条任务都会继续扇出，直到深度/环剪枝兜住——
// 可见，但查起来很痛（一堆互为父子的任务）。运行期那层仍然保留：图会被改，
// 旧任务还在跑，它是最后一道防线。

func TestCreateEdgeRejectsSelfLoop(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns, _ := newTestNS(t, db)

	capA := mkCap(ns, "a", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	err := db.CreateEdge(ctx, &Edge{NamespaceID: ns, FromCapability: capA.ID, FromPort: "ok", ToCapability: capA.ID})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("自连 err = %v, want ErrEdgeRejected", err)
	}
}

func TestCreateEdgeRejectsCycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns, _ := newTestNS(t, db)

	// a --ok--> b --ok--> c
	a := mkCap(ns, "a", 1, []string{"ok"})
	b := mkCap(ns, "b", 1, []string{"ok"})
	c := mkCap(ns, "c", 1, []string{"ok"})
	for _, cap := range []*CapabilityVersion{a, b, c} {
		if err := db.CreateCapability(ctx, cap); err != nil {
			t.Fatalf("create capability %s: %v", cap.Name, err)
		}
	}
	for _, e := range []*Edge{
		{NamespaceID: ns, FromCapability: a.ID, FromPort: "ok", ToCapability: b.ID},
		{NamespaceID: ns, FromCapability: b.ID, FromPort: "ok", ToCapability: c.ID},
	} {
		if err := db.CreateEdge(ctx, e); err != nil {
			t.Fatalf("create edge: %v", err)
		}
	}

	// 2 环：c → a 会闭环 a→b→c→a
	err := db.CreateEdge(ctx, &Edge{NamespaceID: ns, FromCapability: c.ID, FromPort: "ok", ToCapability: a.ID})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("三元环 err = %v, want ErrEdgeRejected", err)
	}
	// 反例：c → 一个新的 d 不成环（不能因为「有过环」就一律拒）
	d := mkCap(ns, "d", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, d); err != nil {
		t.Fatalf("create d: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: ns, FromCapability: c.ID, FromPort: "ok", ToCapability: d.ID}); err != nil {
		t.Fatalf("不成环的边被拒：%v", err)
	}
}

// TestCreateEdgeAllowsDiamond 菱形（a→b, a→c, b→d, c→d）是无环的：共享下游不是环。
//
// 这条是防止「环检测写成了『重复到达同一个节点就拒』」——那样 1:N 扇出会被误杀，
// 而扇出是这个产品的核心能力（I7）。
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
		{NamespaceID: ns, FromCapability: a.ID, FromPort: "ok", ToCapability: b.ID},
		{NamespaceID: ns, FromCapability: a.ID, FromPort: "ok", ToCapability: c.ID},
		{NamespaceID: ns, FromCapability: b.ID, FromPort: "ok", ToCapability: d.ID},
		{NamespaceID: ns, FromCapability: c.ID, FromPort: "ok", ToCapability: d.ID},
	}
	for _, e := range edges {
		if err := db.CreateEdge(ctx, e); err != nil {
			t.Fatalf("菱形结构应当合法，却被拒：%v", err)
		}
	}
}

// TestCreateEdgeEntryEdgeNeverCycles 入口边没有上游能力，永远不可能成环。
func TestCreateEdgeEntryEdgeNeverCycles(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns, _ := newTestNS(t, db)

	a := mkCap(ns, "a", 1, []string{"ok"})
	if err := db.CreateCapability(ctx, a); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	// 先有一条 a → a 会环路线的反例：入口边接 a，再接 a 的回边仍然要拒。
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: ns, FromIsSource: true, FromCapability: "github", FromPort: "push", ToCapability: a.ID,
	}); err != nil {
		t.Fatalf("入口边应当永远合法：%v", err)
	}
	// 入口边不构成环：a 没有回边，所以 a → a 只是自连
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: ns, FromCapability: a.ID, FromPort: "ok", ToCapability: a.ID}); !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("自连应当被拒，err = %v", err)
	}
}
