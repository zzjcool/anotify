package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 本文件是 `-- +migration 2`（edges.priority + capability_versions.balance）的**往返门禁**。
//
// 「存什么读什么」是 AGENTS.md §3 对 store 层的硬要求：两列都在保存路径上做了归一
// （priority 默认 0、balance 缺省 {}），归一一旦和读取路径不一致，症状是
// 「配了优先级却没生效」或「balance 读出来是 null」——两种都只能靠往返用例发现。

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

// TestCapabilityBalanceIsDeadColumn balance 从 P1-b 起是**死字段**（契约 §4）：
// 保存路径仍然校验调用方给的值（非法 mode 仍然 400，见下一条用例），但落库恒为 '{}'，
// 读回来恒是空 map（不是 nil，不变量 I9）。P3 会删掉这一列。
//
// 为什么要单独钉住「不再落库」：如果它看起来还生效、实际被丢弃，运维会以为
// 「我配了 all、跑起来却是 1:1」是路由 bug，而真相是这个字段已经不属于能力了。
func TestCapabilityBalanceIsDeadColumn(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// ① 配了合法 balance：保存成功，但读回来是空（不再落库）。
	c := mkCap(nsID, "review", 1, []string{"fix", "clean"})
	c.Balance = Balance{
		"fix":   {Mode: BalanceModePriority},
		"clean": {}, // 空 mode = 缺省 = priority
	}
	if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	got, err := db.GetCapability(ctx, nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if len(got.Balance) != 0 {
		t.Fatalf("balance 列应是空（死字段，P1-b 起恒写 '{}'），got %+v", got.Balance)
	}
	if got.Balance == nil {
		t.Fatal("读回来是 nil：JSON 会变成 null（不变量 I9）")
	}
	if bytes, err := json.Marshal(got.Balance); err != nil || string(bytes) != `{}` {
		t.Fatalf("balance JSON = %s (err=%v)，want {}", bytes, err)
	}
	// 端口仍然按缺省 priority 走（BalanceModeOf 的缺省语义不变）。
	for _, port := range []string{"fix", "clean"} {
		if mode := got.BalanceModeOf(port); mode != BalanceModePriority {
			t.Errorf("balance[%s].mode = %q, want %q", port, mode, BalanceModePriority)
		}
	}

	// ② 第二条读路径（ListCapabilities）与第一条一致：空 map，不是 nil、不是 null。
	list, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	for _, item := range list {
		if item.Name != "review" {
			continue
		}
		if item.Balance == nil {
			t.Fatal("ListCapabilities 读出的 balance 是 nil（JSON 会变 null）")
		}
		if len(item.Balance) != 0 {
			t.Fatalf("ListCapabilities 读出的 balance 应为空，got %+v", item.Balance)
		}
	}

	// ③ balance 不参与 hash（它是死字段）：同 name@version 重放仍需要幂等折叠，
	// 换 version 则是另一份内容（digest 吃 version）。
	same := mkCap(nsID, "review", 1, []string{"fix", "clean"})
	if err := db.CreateCapability(ctx, withSpec(same)); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if same.Version != 1 || same.ID != got.ID {
		t.Fatalf("同 name@version 的重放应折叠回已有行，got %s@%d", same.Name, same.Version)
	}
}

// TestCapabilityBalanceRejectsUnimplementedModes 预留模式保存时必须 400（不许静默降级）。
//
// 判据是 errors.Is(err, ErrBalanceRejected)：handler 按它翻 400。文案要带「尚未实现」，
// 否则用户看到 400 也不知道自己是「配错了」还是「本版本还没有」。
func TestCapabilityBalanceRejectsUnimplementedModes(t *testing.T) {
	for _, mode := range []string{"all", "random", "least_loaded", "hash"} {
		t.Run(mode, func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			nsID, _ := newTestNS(t, db)

			c := mkCap(nsID, "review", 1, []string{"fix"})
			c.Balance = Balance{"fix": {Mode: mode}}
			err := db.CreateCapability(ctx, withSpec(c))
			if !errors.Is(err, ErrBalanceRejected) {
				t.Fatalf("err = %v (%T), want ErrBalanceRejected", err, err)
			}
			if !strings.Contains(err.Error(), "尚未实现") {
				t.Fatalf("错误文案没有「尚未实现」: %v", err)
			}
			// 拒了就是什么都没写（append-only 表里留下半条数据撤不回来）。
			if _, gerr := db.GetCapability(ctx, nsID, "review", 1); !errors.Is(gerr, ErrNotFound) {
				t.Fatalf("被拒的能力仍然存在（err=%v）", gerr)
			}
		})
	}
	// 未知拼写同样拒（不认识的 mode 不能当 priority 放过）。
	t.Run("拼错的模式", func(t *testing.T) {
		db := newTestDB(t)
		ctx := context.Background()
		nsID, _ := newTestNS(t, db)
		c := mkCap(nsID, "review", 1, []string{"fix"})
		c.Balance = Balance{"fix": {Mode: "prioroty"}}
		if err := db.CreateCapability(ctx, withSpec(c)); !errors.Is(err, ErrBalanceRejected) {
			t.Fatalf("err = %v, want ErrBalanceRejected", err)
		}
	})
}

// TestCapabilityBalanceRejectsUnknownPort 端口名不在 outputs 里 = 拼错，保存时当场拒。
//
// 放它过的话，派发侧读到的永远是「这个端口没配平衡策略」——一个拼错的键
// 静默不生效，正是契约 §1 要消灭的那种失败模式。
func TestCapabilityBalanceRejectsUnknownPort(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := mkCap(nsID, "review", 1, []string{"fix"})
	c.Balance = Balance{"fxi": {Mode: BalanceModePriority}}
	err := db.CreateCapability(ctx, withSpec(c))
	if !errors.Is(err, ErrBalanceRejected) {
		t.Fatalf("err = %v, want ErrBalanceRejected", err)
	}
	if !strings.Contains(err.Error(), "fxi") {
		t.Fatalf("错误文案没说是哪个端口: %v", err)
	}
}

// TestMigrationTwoColumns 全新库必须带上这两列（`-- +migration 2` 真的跑了）。
//
// baseline 没有改（契约 §1）：新列只能由追加块加出来，所以这条同时守住
// 「别顺手把列加进 baseline」——那样老库永远拿不到它。
func TestMigrationTwoColumns(t *testing.T) {
	db := newTestDB(t)

	for _, tc := range []struct{ table, col string }{
		{"edges", "priority"},
		{"capability_versions", "balance"},
	} {
		if !columnNames(t, db, tc.table)[tc.col] {
			t.Errorf("%s 缺列 %s（migration 2 没生效？）", tc.table, tc.col)
		}
	}
	// 默认值必须是 0 / '{}'（老行的行为与加列之前一致）。
	var prio int
	if err := db.QueryRow(`SELECT priority FROM edges LIMIT 1`).Scan(&prio); err == nil {
		t.Fatalf("空表竟然读出了 priority=%d", prio)
	}
}
