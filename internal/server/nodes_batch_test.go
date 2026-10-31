package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 #223（/v1/nodes 的 N+1 查询）的验收测试。
//
// 自建最小环境（**不复用 console_test.go 的 newConsoleEnv**）：那套夹具同时建
// 用户/会话/路由，被别的用例改的时候会把这里的断言一起带崩；而这里只需要
// "一个库里几台节点 + 能力 + 槽位"，越少依赖越能钉住语义。
//
// 核心不变量：批量版（viewsFor）与单台版（viewFor）对同一台节点必须产出
// **逐字节相同**的 JSON —— 优化只该改 SQL 条数，不该改响应形状。

// openBatchDB 开一个临时库（每个用例独立文件，避免 :memory: 的连接池语义差异）。
func openBatchDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/nodes-batch.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// batchNamespace 建一个命名空间（创建时会播种内置能力），返回它的 id。
func batchNamespace(t *testing.T, ctx context.Context, db *store.DB) string {
	t.Helper()
	ns, err := db.CreateNamespace(ctx, "batch", "批量视图")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.ID
}

// batchNode 建一台节点。
func batchNode(t *testing.T, ctx context.Context, db *store.DB, nsID, name string) *store.Node {
	t.Helper()
	n := &store.Node{NamespaceID: nsID, Name: name, Tags: []string{"pool-batch"}}
	if _, err := db.CreateNode(ctx, n); err != nil {
		t.Fatalf("create node %q: %v", name, err)
	}
	return n
}

// batchCap 在命名空间里定义一个自定义能力。
//
// outputs 里带上 version：version 不参与 content_hash（「同内容不同版本」是合法
// 概念），若两个版本的内容一字不差，CreateCapability 会当成幂等重放返回旧行，
// 第二个版本根本建不出来 —— 那样「同名多版本排序」的断言就测不到东西了。
func batchCap(t *testing.T, ctx context.Context, db *store.DB, nsID, name string, version int) {
	t.Helper()
	c := &store.CapabilityVersion{
		NamespaceID: nsID,
		Name:        name,
		Version:     version,
		Outputs:     []string{fmt.Sprintf("%s-v%d-out", name, version)},
	}
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create capability %s@%d: %v", name, version, err)
	}
}

// batchDeclareCaps 让节点声明一组能力（老路径：不带 run → run_spec 落 '{}'）。
//
// 声明顺序刻意与 (name, version) 升序不同，用来钉住"组内排序来自查询层"。
func batchDeclareCaps(t *testing.T, ctx context.Context, db *store.DB, nsID, nodeID string, refs ...store.CapabilityRef) {
	t.Helper()
	if _, err := db.ConfirmCapabilities(ctx, nsID, nodeID, refs); err != nil {
		t.Fatalf("confirm capabilities for %s: %v", nodeID, err)
	}
}

// mustJSON 把视图序列化成 JSON（等价性断言按字节比，不按结构比：
// 结构比会漏掉"nil 切片 vs 空切片"这类只有在编码后才看得见的差异）。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// TestViewsForBatchEquivalence 批量视图与单台视图必须逐字节一致，且顺序/空值语义不变。
func TestViewsForBatchEquivalence(t *testing.T) {
	ctx := context.Background()
	db := openBatchDB(t)
	nsID := batchNamespace(t, ctx, db)
	h := &consoleHandler{db: db}

	batchCap(t, ctx, db, nsID, "review", 1)
	batchCap(t, ctx, db, nsID, "analyze-pr", 1)

	a := batchNode(t, ctx, db, nsID, "node-a")
	b := batchNode(t, ctx, db, nsID, "node-b")
	c := batchNode(t, ctx, db, nsID, "node-c")

	// A：2 个能力（声明顺序 review → analyze-pr，与 name 升序**相反**）+ 2 个槽位（1 个被占）
	batchDeclareCaps(t, ctx, db, nsID, a.ID,
		store.CapabilityRef{Name: "review", Version: 1},
		store.CapabilityRef{Name: "analyze-pr", Version: 1},
	)
	if _, err := db.UpsertInstances(ctx, nsID, a.ID, []store.InstanceSpec{
		{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1},
	}); err != nil {
		t.Fatalf("declare instances for a: %v", err)
	}
	if _, err := db.LeaseInstance(ctx, a.ID, "tsk_batch_busy"); err != nil {
		t.Fatalf("lease instance for a: %v", err)
	}

	// B：1 个能力 + 0 槽位
	batchDeclareCaps(t, ctx, db, nsID, b.ID, store.CapabilityRef{Name: "review", Version: 1})

	// C：什么都没有（能力/槽位都为空）

	nodes := []*store.Node{a, b, c}
	views := h.viewsFor(ctx, nodes)
	if len(views) != len(nodes) {
		t.Fatalf("viewsFor returned %d views, want %d", len(views), len(nodes))
	}

	// ① 顺序与入参一致（按节点身份比，不只比数量）
	for i, n := range nodes {
		if views[i].Node != n {
			t.Fatalf("views[%d].Node = %p, want %p (返回顺序必须与入参一致)", i, views[i].Node, n)
		}
	}

	// ② 每台节点的批量结果 == 单台结果（逐字节）
	for i, n := range nodes {
		got := mustJSON(t, views[i])
		want := mustJSON(t, h.viewFor(ctx, n))
		if got != want {
			t.Fatalf("node %s 批量/单台 JSON 不一致:\n got=%s\nwant=%s", n.Name, got, want)
		}
	}

	// ③ A 的数量语义：2 个槽位、1 个 busy
	if views[0].Instances != 2 || views[0].Busy != 1 {
		t.Fatalf("A instances/busy = %d/%d, want 2/1", views[0].Instances, views[0].Busy)
	}
	// ④ A 的能力按 (name, version) 升序（声明顺序相反，所以这条能抓到"漏了 ORDER BY"）
	caps := views[0].Capabilities
	if len(caps) != 2 {
		t.Fatalf("A capabilities = %d, want 2 (%+v)", len(caps), caps)
	}
	if caps[0].Name != "analyze-pr" || caps[0].Version != 1 ||
		caps[1].Name != "review" || caps[1].Version != 1 {
		t.Fatalf("A capabilities 顺序 = [%s@%d, %s@%d], want [analyze-pr@1, review@1]",
			caps[0].Name, caps[0].Version, caps[1].Name, caps[1].Version)
	}
	// ⑤ 没声明 run 的能力：runSpec 必须是 `{}`（I9），不能是 null/空串
	for _, cv := range caps {
		if strings.TrimSpace(string(cv.RunSpec)) != "{}" {
			t.Fatalf("cap %s@%d runSpec = %q, want {}", cv.Name, cv.Version, string(cv.RunSpec))
		}
	}
	// ⑥ B 的 1 个能力 + 0 槽位
	if len(views[1].Capabilities) != 1 || views[1].Instances != 0 || views[1].Busy != 0 {
		t.Fatalf("B caps/instances/busy = %d/%d/%d, want 1/0/0",
			len(views[1].Capabilities), views[1].Instances, views[1].Busy)
	}

	// ⑦ C 的 JSON 必须给 [] 和 0（不是 null）—— 前端靠 "capabilities":[] 判断"没能力"，
	//    拿到 null 会直接崩在 .map 上。
	rawC := mustJSON(t, views[2])
	for _, want := range []string{`"capabilities":[]`, `"instances":0`, `"busy":0`} {
		if !strings.Contains(rawC, want) {
			t.Fatalf("C 的 JSON 缺 %s: %s", want, rawC)
		}
	}
	if strings.Contains(rawC, `"capabilities":null`) {
		t.Fatalf("C 的 capabilities 是 null（I9 要求 []）: %s", rawC)
	}
}

// TestViewsForBatchEmptyInput 空输入不能查库报错，也不能返回 nil（调用方直接 range）。
func TestViewsForBatchEmptyInput(t *testing.T) {
	ctx := context.Background()
	db := openBatchDB(t)
	h := &consoleHandler{db: db}

	views := h.viewsFor(ctx, nil)
	if views == nil {
		t.Fatal("viewsFor(nil) = nil, want 空切片")
	}
	if len(views) != 0 {
		t.Fatalf("viewsFor(nil) len = %d, want 0", len(views))
	}
	// 序列化后必须是 [] 而不是 null（列表端点把它放进 {"nodes": ...}）
	if got := mustJSON(t, views); got != "[]" {
		t.Fatalf("viewsFor(nil) JSON = %s, want []", got)
	}
}

// TestBatchStoreQueriesEdgeCases 两个批量查的空输入 / 未知节点 / 重复 id / 掉线槽位语义。
//
// 这些边界决定调用方能不能"无脑"把整页 id 传进来：空 map 而不报错、
// 重复 id 不放大结果、掉线槽位仍然计入总数（与 ListInstances 同口径）。
func TestBatchStoreQueriesEdgeCases(t *testing.T) {
	ctx := context.Background()
	db := openBatchDB(t)
	nsID := batchNamespace(t, ctx, db)

	// 空输入：空 map + nil error（不是 nil map，调用方直接下标取值）
	capsEmpty, err := db.ListCapabilitiesByNode(ctx, nil)
	if err != nil {
		t.Fatalf("ListCapabilitiesByNode(nil): %v", err)
	}
	if capsEmpty == nil || len(capsEmpty) != 0 {
		t.Fatalf("ListCapabilitiesByNode(nil) = %v, want 空 map", capsEmpty)
	}
	statsEmpty, err := db.InstanceStatsByNode(ctx, nil)
	if err != nil {
		t.Fatalf("InstanceStatsByNode(nil): %v", err)
	}
	if statsEmpty == nil || len(statsEmpty) != 0 {
		t.Fatalf("InstanceStatsByNode(nil) = %v, want 空 map", statsEmpty)
	}

	// 库里不存在的节点 id：同样空 map + nil error（列表页可能并发删节点）
	capsGhost, err := db.ListCapabilitiesByNode(ctx, []string{"node_ghost"})
	if err != nil {
		t.Fatalf("ListCapabilitiesByNode(ghost): %v", err)
	}
	if len(capsGhost) != 0 {
		t.Fatalf("ListCapabilitiesByNode(ghost) = %v, want 空 map", capsGhost)
	}
	statsGhost, err := db.InstanceStatsByNode(ctx, []string{"node_ghost"})
	if err != nil {
		t.Fatalf("InstanceStatsByNode(ghost): %v", err)
	}
	if len(statsGhost) != 0 {
		t.Fatalf("InstanceStatsByNode(ghost) = %v, want 空 map", statsGhost)
	}

	// 真实数据的边界：重复 id 不放大结果；掉线槽位仍计入 Total（与 ListInstances 同口径）
	batchCap(t, ctx, db, nsID, "review", 1)
	n := batchNode(t, ctx, db, nsID, "node-dedup")
	batchDeclareCaps(t, ctx, db, nsID, n.ID, store.CapabilityRef{Name: "review", Version: 1})
	if _, err := db.UpsertInstances(ctx, nsID, n.ID, []store.InstanceSpec{
		{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1},
	}); err != nil {
		t.Fatalf("declare instances: %v", err)
	}
	if _, err := db.LeaseInstance(ctx, n.ID, "tsk_dedup_busy"); err != nil {
		t.Fatalf("lease: %v", err)
	}
	// 掉线：alive=0，但 busy_task 与行都还在
	if err := db.MarkInstancesDead(ctx, n.ID); err != nil {
		t.Fatalf("mark instances dead: %v", err)
	}

	caps, err := db.ListCapabilitiesByNode(ctx, []string{n.ID, n.ID, n.ID})
	if err != nil {
		t.Fatalf("ListCapabilitiesByNode: %v", err)
	}
	if len(caps) != 1 {
		t.Fatalf("caps map 有 %d 个 key, want 1 (%v)", len(caps), caps)
	}
	if len(caps[n.ID]) != 1 {
		t.Fatalf("重复 id 把能力放大了：%d 条, want 1", len(caps[n.ID]))
	}
	stats, err := db.InstanceStatsByNode(ctx, []string{n.ID, n.ID})
	if err != nil {
		t.Fatalf("InstanceStatsByNode: %v", err)
	}
	if got := stats[n.ID]; got.Total != 2 || got.Busy != 1 {
		t.Fatalf("重复 id 的统计 = %+v, want {Total:2 Busy:1}（Total 不看 alive）", got)
	}
	// 反向确认口径与 ListInstances 一致（掉线槽位仍在列表里）
	insts, err := db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(insts) != stats[n.ID].Total {
		t.Fatalf("InstanceStatsByNode.Total = %d, ListInstances = %d（口径必须一致）",
			stats[n.ID].Total, len(insts))
	}
}

// TestBatchListCapabilitiesByNodeOrdering 组内排序（同名多版本）+ run_spec 空串兜底。
//
// 两条都容易被"批量版"写漏：ORDER BY 少了 c.version 时同名多版本的顺序变成
// 插入顺序；空串兜底漏了则页面显示 runSpec:""（前端 JSON.parse 直接抛错）。
func TestBatchListCapabilitiesByNodeOrdering(t *testing.T) {
	ctx := context.Background()
	db := openBatchDB(t)
	nsID := batchNamespace(t, ctx, db)

	batchCap(t, ctx, db, nsID, "review", 1)
	batchCap(t, ctx, db, nsID, "review", 2)

	n := batchNode(t, ctx, db, nsID, "node-multi-version")
	// 声明顺序：先 @2 后 @1（若查询层只按插入顺序返回，断言会失败）
	batchDeclareCaps(t, ctx, db, nsID, n.ID,
		store.CapabilityRef{Name: "review", Version: 2},
		store.CapabilityRef{Name: "review", Version: 1},
	)

	got, err := db.ListCapabilitiesByNode(ctx, []string{n.ID})
	if err != nil {
		t.Fatalf("ListCapabilitiesByNode: %v", err)
	}
	caps := got[n.ID]
	if len(caps) != 2 {
		t.Fatalf("caps[%s] = %d 条, want 2", n.ID, len(caps))
	}
	if caps[0].Version != 1 || caps[1].Version != 2 {
		t.Fatalf("review 版本顺序 = [%d, %d], want [1, 2]", caps[0].Version, caps[1].Version)
	}
	// 与单节点实现的排序必须完全一致
	single, err := db.ListNodeCapabilities(ctx, n.ID)
	if err != nil {
		t.Fatalf("ListNodeCapabilities: %v", err)
	}
	for i := range single {
		if single[i].Name != caps[i].Name || single[i].Version != caps[i].Version {
			t.Fatalf("第 %d 条与 ListNodeCapabilities 不一致: %s@%d vs %s@%d",
				i, caps[i].Name, caps[i].Version, single[i].Name, single[i].Version)
		}
	}

	// run_spec 空串（手工改库/老数据）→ 必须兜底成 '{}'（I9）
	if _, err := db.ExecContext(ctx, `UPDATE node_capabilities SET run_spec = '' WHERE node_id = ?`, n.ID); err != nil {
		t.Fatalf("blank run_spec: %v", err)
	}
	got, err = db.ListCapabilitiesByNode(ctx, []string{n.ID})
	if err != nil {
		t.Fatalf("ListCapabilitiesByNode after blank: %v", err)
	}
	for _, c := range got[n.ID] {
		if strings.TrimSpace(string(c.RunSpec)) != "{}" {
			t.Fatalf("空 run_spec 未兜底成 {}: %q", string(c.RunSpec))
		}
	}

	// 没有能力的节点不出现在 map 里（调用方按 nil → [] 处理）
	empty := batchNode(t, ctx, db, nsID, "node-no-caps")
	got, err = db.ListCapabilitiesByNode(ctx, []string{empty.ID})
	if err != nil {
		t.Fatalf("ListCapabilitiesByNode(no caps): %v", err)
	}
	if _, ok := got[empty.ID]; ok {
		t.Fatalf("没能力的节点出现在 map 里: %v", got)
	}
}
