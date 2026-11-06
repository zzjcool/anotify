package store

import (
	"context"
	"reflect"
	"testing"
)

// 本文件覆盖 CapabilityEdgeShape —— 「一个能力在 edges 里怎么接的」这个推导量的读法。
//
// 它有两个消费者，两者都**以边为准**（#383：outputs 不再是拓扑的守门人）：
//
//	assign.ports = 产物声明的 outputs ∪ OutPorts（ws.enrichAssign）
//	kind         = DeriveKind(HasOut, HasIn)（route.KindForCapability）
//
// 所以这里要钉住三件事：
//   - 左端 `name` 与钉死的 `name@version` 都算同一个能力的出边（与 EdgesFrom 同口径）；
//   - 入边既包括能力边（别人 --> 它），也包括入口边（来源 --事件--> 它）；
//   - 命名空间隔离：同名能力在别的命名空间里的边一条都不能读到。

// seedShapeCap 建一个能力定义（params_schema 留最简形状，本文件不关心它）。
func seedShapeCap(t *testing.T, db *DB, nsID, name string, outputs []string) *CapabilityVersion {
	t.Helper()
	c := mkCap(nsID, name, 1, outputs)
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability %s@1: %v", name, err)
	}
	return c
}

// TestCapabilityEdgeShapeReadsPortsAndInbound 出边端口（含钉死左端）+ 入边（能力边/入口边）。
func TestCapabilityEdgeShapeReadsPortsAndInbound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// flow 声明两个出口；down 是终端能力（outputs 为空，正是 #383 的场景）。
	seedShapeCap(t, db, nsID, "flow", []string{"before", "after"})
	seedShapeCap(t, db, nsID, "down", nil)
	seedShapeCap(t, db, nsID, "other", []string{ImplicitPort})

	// ① 没有任何边：空端口集合是 [] 而不是 nil（I9），也没有入边。
	shape, err := db.CapabilityEdgeShape(ctx, nsID, "flow")
	if err != nil {
		t.Fatalf("shape of an unwired capability: %v", err)
	}
	if shape.OutPorts == nil {
		t.Fatal("OutPorts = nil, want []（空列表纪律：调用方不该区分 nil / 空）")
	}
	if len(shape.OutPorts) != 0 || shape.HasOut() || shape.HasIn {
		t.Fatalf("unwired capability shape = %+v, want empty", shape)
	}

	// ② 两条出边：一条左端是裸名字，一条钉死 flow@1（同一能力的两种引用形态）。
	for _, e := range []*Edge{
		{NamespaceID: nsID, FromRef: "flow", FromPort: "before", ToRef: "down"},
		{NamespaceID: nsID, FromRef: "flow@1", FromPort: "after", ToRef: "other"},
		// 别人的边（other 是左端）：不该出现在 flow 的端口集合里。
		{NamespaceID: nsID, FromRef: "other", FromPort: ImplicitPort, ToRef: "down"},
	} {
		if err := db.CreateEdge(ctx, e); err != nil {
			t.Fatalf("seed edge %s--%s-->%s: %v", e.FromRef, e.FromPort, e.ToRef, err)
		}
	}
	shape, err = db.CapabilityEdgeShape(ctx, nsID, "flow")
	if err != nil {
		t.Fatalf("shape of flow: %v", err)
	}
	if want := []string{"after", "before"}; !reflect.DeepEqual(shape.OutPorts, want) {
		t.Fatalf("flow OutPorts = %v, want %v（去重升序；左端 name 与 name@version 都算）",
			shape.OutPorts, want)
	}
	if !shape.HasOut() {
		t.Fatal("flow HasOut() = false, want true")
	}
	if shape.HasIn {
		// 目前只有指向 down / other 的边；flow 自己不接收任何东西。
		t.Fatal("flow HasIn = true, want false（入边只有指向它的那些）")
	}

	// ③ 入边：down 被能力边指向 → HasIn。
	shape, err = db.CapabilityEdgeShape(ctx, nsID, "down")
	if err != nil {
		t.Fatalf("shape of down: %v", err)
	}
	if !shape.HasIn {
		t.Fatal("down HasIn = false, want true（flow --before--> down）")
	}
	if shape.HasOut() || len(shape.OutPorts) != 0 {
		t.Fatalf("down OutPorts = %v, want empty（它是终端）", shape.OutPorts)
	}

	// ④ 入边也包括**入口边**（来源 --任意事件--> 能力）：它同样是「有上游」。
	seedShapeCap(t, db, nsID, "entry", nil)
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromIsSource: true, FromRef: "github",
		FromPort: "*", ToRef: "entry",
	}); err != nil {
		t.Fatalf("seed entry edge: %v", err)
	}
	shape, err = db.CapabilityEdgeShape(ctx, nsID, "entry")
	if err != nil {
		t.Fatalf("shape of entry: %v", err)
	}
	if !shape.HasIn {
		t.Fatal("entry HasIn = false, want true（入口边也是入边）")
	}

	// ⑤ 右端钉死（down@1）同样算入边：右端是引用，不是 id。
	seedShapeCap(t, db, nsID, "pinned-target", nil)
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromIsSource: true, FromRef: "cron",
		FromPort: "*", ToRef: "pinned-target@1",
	}); err != nil {
		t.Fatalf("seed pinned entry edge: %v", err)
	}
	shape, err = db.CapabilityEdgeShape(ctx, nsID, "pinned-target")
	if err != nil {
		t.Fatalf("shape of pinned-target: %v", err)
	}
	if !shape.HasIn {
		t.Fatal("pinned-target HasIn = false, want true（to_ref 可以是 name@version）")
	}
}

// TestCapabilityEdgeShapeIsolatesNamespaces 同名能力在别的命名空间里的边不许串进来。
func TestCapabilityEdgeShapeIsolatesNamespaces(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	otherNS := newTestExtraNS(t, db, "shape-other")

	seedShapeCap(t, db, nsID, "flow", []string{"before"})
	seedShapeCap(t, db, otherNS, "flow", []string{"before"})
	seedShapeCap(t, db, otherNS, "down", nil)

	// 只在**另一个**命名空间里接一条 flow -> down 的边。
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: otherNS, FromRef: "flow", FromPort: "before", ToRef: "down",
	}); err != nil {
		t.Fatalf("seed edge in other namespace: %v", err)
	}

	shape, err := db.CapabilityEdgeShape(ctx, nsID, "flow")
	if err != nil {
		t.Fatalf("shape: %v", err)
	}
	if len(shape.OutPorts) != 0 {
		t.Fatalf("本命名空间的 flow OutPorts = %v, want empty（别的命名空间的边不许串进来）",
			shape.OutPorts)
	}
	// 反过来：另一个命名空间看得到自己的边。
	shape, err = db.CapabilityEdgeShape(ctx, otherNS, "flow")
	if err != nil {
		t.Fatalf("shape in other namespace: %v", err)
	}
	if want := []string{"before"}; !reflect.DeepEqual(shape.OutPorts, want) {
		t.Fatalf("other namespace flow OutPorts = %v, want %v", shape.OutPorts, want)
	}
}

// TestCapabilityEdgeShapeRejectsWildcardNamespace 汇总视图（`*`）不是可读的隔离维度。
func TestCapabilityEdgeShapeRejectsWildcardNamespace(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.CapabilityEdgeShape(context.Background(), AllNamespaces, "flow"); err == nil {
		t.Fatal("CapabilityEdgeShape(*) 应当报 ErrNamespaceRequired，而不是把全部命名空间的边混成一份")
	}
}
