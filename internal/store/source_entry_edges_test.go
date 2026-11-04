package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 源的出口 = 入口边（P2.1）。
//
// 库里只有 edges 一份真相：建源/改源就是在事务里重建「该源的全部入口边」，
// 读源就是从 edges 读回来。这组用例钉住那条唯一的往返路径——尤其是
// **改名**与**删源**：入口边的左端是来源名（没有外键），靠代码自己搬/清。

// entrySourceFixture 建好「一个声明了字段字典的目标能力 + 一个 manual 源」。
func entrySourceFixture(t *testing.T) (db *DB, nsID string, capA, capB *CapabilityVersion, src *Source) {
	t.Helper()
	ctx := context.Background()
	db = newTestDB(t)
	nsID, _ = newTestNS(t, db)

	capA = mkCap(nsID, "notify", 1, nil)
	capA.ParamsSchema = json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"title":{"type":"string"}}}`)
	capB = mkCap(nsID, "archive", 1, nil)
	capB.ParamsSchema = json.RawMessage(`{"type":"object","properties":{"env":{"type":"string"}}}`)
	for _, c := range []*CapabilityVersion{capA, capB} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create capability %s: %v", c.Name, err)
		}
	}

	src = &Source{
		NamespaceID: nsID, Name: "多出口", Kind: SourceKindManual, Enabled: true,
		Targets: []SourceTarget{
			{CapabilityID: capA.ID, Params: map[string]any{"to": "{{pull_request.user.login}}", "title": "PR 已合并"}},
			{CapabilityID: capB.ID, Params: map[string]any{"env": "prod"}},
		},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	return db, nsID, capA, capB, src
}

// TestSourceTargetsRoundTripFromEdges 建源 = 建入口边；读源 = 从入口边读回来。
//
// 关键断言是「参数按出口分别往返」：两个能力各拿自己那份模板（一份公共 params
// 必然对不上至少一边），以及**入口边的形状**（from_is_source=1、from_ref=来源名、
// from_port='*'、literals='{}'）。
func TestSourceTargetsRoundTripFromEdges(t *testing.T) {
	db, nsID, capA, capB, src := entrySourceFixture(t)
	ctx := context.Background()

	// ① 库里是入口边，不是另一张表
	if tableExists(t, db, "source_targets") {
		t.Fatal("source_targets 还存在（应该只剩 edges 这条真相）")
	}
	edges, err := db.ListEdges(ctx, nsID, src.Name)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("入口边 = %d 条, want 2: %+v", len(edges), edges)
	}
	for _, e := range edges {
		if !e.FromIsSource || e.FromPort != "*" || string(e.Literals) != EdgeLiteralsEmpty {
			t.Errorf("入口边形状不对: %+v（want from_is_source=1 / from_port='*' / literals='{}'）", e)
		}
	}

	// ② 读回来：每个出口的模板各自还原
	got, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if len(got.Targets) != 2 {
		t.Fatalf("targets = %d, want 2: %+v", len(got.Targets), got.Targets)
	}
	byID := map[string]SourceTarget{}
	for _, tg := range got.Targets {
		byID[tg.CapabilityID] = tg
	}
	if p := byID[capA.ID].Params; p["to"] != "{{pull_request.user.login}}" || p["title"] != "PR 已合并" {
		t.Errorf("capA 的模板往返不一致: %+v", p)
	}
	if p := byID[capB.ID].Params; p["env"] != "prod" {
		t.Errorf("capB 的模板往返不一致: %+v", p)
	}
	// 列表路径同样从入口边读
	list, err := db.ListSources(ctx, nsID)
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	if len(list) != 1 || len(list[0].Targets) != 2 {
		t.Fatalf("ListSources 的出口 = %+v, want 2 条", list)
	}
}

// TestUpdateSourceRebuildsEntryEdges 改源：删掉的目标不再有边，改了模板的边跟着变。
func TestUpdateSourceRebuildsEntryEdges(t *testing.T) {
	db, nsID, capA, capB, src := entrySourceFixture(t)
	ctx := context.Background()

	cur, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	// 只留 capA，并把模板换掉（capB 的边必须消失，不能留残留）
	cur.Targets = []SourceTarget{{CapabilityID: capA.ID, Params: map[string]any{"to": "u_direct"}}}
	if err := db.UpdateSource(ctx, cur); err != nil {
		t.Fatalf("update source: %v", err)
	}

	edges, err := db.ListEdges(ctx, nsID, cur.Name)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	// 右端是**引用**（P2.2）：源的目标钉在它选中的那一版上。
	if len(edges) != 1 || edges[0].ToRef != capRef(capA) {
		t.Fatalf("改源后入口边 = %+v, want 只剩 capA 一条", edges)
	}
	if !strings.Contains(string(edges[0].Mapping), "u_direct") {
		t.Errorf("模板没跟着改: %q", edges[0].Mapping)
	}
	if n := countRows(t, db, "edges", "to_ref = ?", capRef(capB)); n != 0 {
		t.Errorf("被移除的目标还留着 %d 条边", n)
	}
}

// TestRenameSourceMovesEntryEdges 改名：旧名字下的边必须搬走（from_ref 没有外键）。
//
// 不搬的后果非常难查：旧名下的边变成「谁也派发不到」的孤儿，而新名字下会凭空
// 少一条边（用户看到的是"改个名字链路就断了"）。同一个名字被下一个源复用时
// 还会继承上一个人的模板。
func TestRenameSourceMovesEntryEdges(t *testing.T) {
	db, nsID, capA, _, src := entrySourceFixture(t)
	ctx := context.Background()

	cur, err := db.GetSource(ctx, nsID, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	cur.Name = "改过的名字"
	if err := db.UpdateSource(ctx, cur); err != nil {
		t.Fatalf("rename source: %v", err)
	}

	if n := countRows(t, db, "edges", "from_ref = ?", src.Name); n != 0 {
		t.Fatalf("旧名字下还剩 %d 条边（改名没搬走）", n)
	}
	edges, err := db.ListEdges(ctx, nsID, cur.Name)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("新名字下入口边 = %d 条, want 2: %+v", len(edges), edges)
	}
	// 模板跟着搬（不是重建出空模板）
	var found bool
	for _, e := range edges {
		if e.ToRef == capRef(capA) && strings.Contains(string(e.Mapping), "pull_request.user.login") {
			found = true
		}
	}
	if !found {
		t.Errorf("改名后 capA 的模板丢了: %+v", edges)
	}
}

// TestDeleteSourceClearsEntryEdges 删源：它名下的入口边一起删（没有外键级联）。
func TestDeleteSourceClearsEntryEdges(t *testing.T) {
	db, nsID, _, _, src := entrySourceFixture(t)
	ctx := context.Background()

	if err := db.DeleteSource(ctx, nsID, src.ID); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	if n := countRows(t, db, "edges", "namespace_id = ? AND from_is_source = 1", nsID); n != 0 {
		t.Fatalf("删源后还剩 %d 条入口边", n)
	}
}

// TestCreateSourceRejectsBadMapping 建源的路也走同一套 mapping 硬拒。
//
// 否则「入口边是库里唯一真相」这条就破了：CreateEdge 拒绝的形状可以从建源写进去，
// 库里就会出现一条只会在触发时才以 param_missing 暴露的边。
func TestCreateSourceRejectsBadMapping(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	capDef := mkCap(nsID, "notify", 1, nil)
	capDef.ParamsSchema = json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"}}}`)
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{name: "键不在 schema 里", params: map[string]any{"titel": "{{x}}"}, want: "mapping.titel"},
		{name: "半截占位符", params: map[string]any{"to": "PR {{title"}, want: "占位符没有闭合"},
		{name: "常量类型不符", params: map[string]any{"to": 42}, want: "want type string"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := db.CreateSource(ctx, &Source{
				NamespaceID: nsID, Name: "bad-" + strings.Repeat("x", i+1), Kind: SourceKindManual,
				Enabled: true, Targets: []SourceTarget{{CapabilityID: capDef.ID, Params: c.params}},
			})
			if err == nil {
				t.Fatalf("应报错（含 %q），但建源成功了", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误 %q 不含 %q", err.Error(), c.want)
			}
		})
	}
	// 一次都不该写进库（事务回滚 / 压根没开始）
	if n := countRows(t, db, "sources", "1 = 1"); n != 0 {
		t.Fatalf("被拒的建源写进了 %d 行", n)
	}
	if n := countRows(t, db, "edges", "1 = 1"); n != 0 {
		t.Fatalf("被拒的建源写了 %d 条边", n)
	}
}
