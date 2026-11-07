package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// P2.2 的引用语义验收（设计 typed-pipeline §3.2 / §7「P2.2 施工计划」）。
//
// 这一组用例只钉一件事：**边里存的是引用，不是 id**，而引用有两种形态：
//
//	analyze      跟随 latest —— 升级自动生效
//	analyze@1    钉死某一版 —— 渐进发布 / 隔离 / 回滚
//
// 两者必须同时成立。这正是 P2.2 的存在理由：旧模型（存 id）只能表达「钉死」，
// 于是升级永远不生效；而如果全都改成跟随 latest，就没人能隔离与回滚。

// TestLinkRefFollowsLatest `to_ref` 是裸名字时，升级之后解析到**新版**。
func TestLinkRefFollowsLatest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	review := mkCap(nsID, "review", 1, []string{"fix"})
	fixer := mkCap(nsID, "fixer", 1, nil)
	mustCreateCap(t, db, review, fixer)

	// 裸名字 = 跟随 latest（设计 §3.2 的默认写法）
	e := &Edge{NamespaceID: nsID, FromRef: review.Name, FromPort: "fix", ToRef: "fixer"}
	if err := db.CreateEdge(ctx, e); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	// 库里存的就是**引用原样**，不是 id、也不是 name@version
	if e.ToRef != "fixer" {
		t.Fatalf("to_ref = %q, want 裸名字 fixer（跟随 latest）", e.ToRef)
	}
	var stored string
	if err := db.QueryRow(`SELECT to_ref FROM edges WHERE namespace_id = ?`, nsID).Scan(&stored); err != nil {
		t.Fatalf("read to_ref: %v", err)
	}
	if stored != "fixer" {
		t.Fatalf("库里的 to_ref = %q, want fixer", stored)
	}

	// 派发第 ① 步：查到的是引用，原样给出
	targets, err := db.EdgesFrom(ctx, nsID, false, review.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(targets) != 1 || targets[0].ToRef != "fixer" {
		t.Fatalf("EdgesFrom = %+v, want 一条 to_ref=fixer", targets)
	}
	// 解析：此刻指向 v1
	got, err := db.ResolveRef(ctx, nsID, targets[0].ToRef)
	if err != nil {
		t.Fatalf("resolve ref: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("resolve(fixer) = @%d, want @1", got.Version)
	}

	// 发新版（加一个**可选**入参：不触发升级闸门，见 §3.6）
	fixer2 := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}}}`),
		Source:       CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, withSpec(fixer2)); err != nil {
		t.Fatalf("create fixer@2: %v", err)
	}

	// 边**一行没改**，但下一次派发解析到 v2 —— 这就是「升级生效」
	targets, err = db.EdgesFrom(ctx, nsID, false, review.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom after upgrade: %v", err)
	}
	if len(targets) != 1 || targets[0].ToRef != "fixer" {
		t.Fatalf("升级后 link 的引用被改动了: %+v（升级不该改配置）", targets)
	}
	got, err = db.ResolveRef(ctx, nsID, targets[0].ToRef)
	if err != nil {
		t.Fatalf("resolve ref after upgrade: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("resolve(fixer) 升级后 = @%d, want @2（裸名字必须跟随 latest）", got.Version)
	}
}

// TestLinkRefPinned `to_ref` 是 `name@version` 时，升级**不影响**它。
func TestLinkRefPinned(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	review := mkCap(nsID, "review", 1, []string{"fix"})
	fixer := mkCap(nsID, "fixer", 1, nil)
	mustCreateCap(t, db, review, fixer)

	e := &Edge{NamespaceID: nsID, FromRef: review.Name, FromPort: "fix", ToRef: "fixer@1"}
	if err := db.CreateEdge(ctx, e); err != nil {
		t.Fatalf("create edge: %v", err)
	}

	fixer2 := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}}}`),
		Source:       CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, withSpec(fixer2)); err != nil {
		t.Fatalf("create fixer@2: %v", err)
	}

	targets, err := db.EdgesFrom(ctx, nsID, false, review.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(targets) != 1 || targets[0].ToRef != "fixer@1" {
		t.Fatalf("EdgesFrom = %+v, want 一条钉死的 fixer@1", targets)
	}
	got, err := db.ResolveRef(ctx, nsID, targets[0].ToRef)
	if err != nil {
		t.Fatalf("resolve pinned: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("resolve(fixer@1) = @%d, want @1（钉死的引用不受升级影响）", got.Version)
	}
}

// TestLinkRefAcceptsLegacyID 输入兼容：传 id 也能建，但落库的是 `name@version`（绝不是 id）。
func TestLinkRefAcceptsLegacyID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	review := mkCap(nsID, "review", 1, []string{"fix"})
	fixer := mkCap(nsID, "fixer", 1, nil)
	mustCreateCap(t, db, review, fixer)

	// 走废弃字段名 + 值给 id：P2.2 之前的客户端就是这么传的
	e := &Edge{NamespaceID: nsID, FromCapability: review.ID, FromPort: "fix", ToCapability: fixer.ID}
	if err := db.CreateEdge(ctx, e); err != nil {
		t.Fatalf("create edge by id: %v", err)
	}
	if e.FromRef != capRef(review) || e.ToRef != capRef(fixer) {
		t.Fatalf("归一后 = (%q → %q), want (%q → %q)：id 必须被换成钉死引用",
			e.FromRef, e.ToRef, capRef(review), capRef(fixer))
	}
	// 库里没有 id 的痕迹
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM edges WHERE to_ref = ? OR from_ref = ?`,
		fixer.ID, review.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("库里出现了 id 形状的引用（%d 行）——P2.2 之后边里只许存引用", n)
	}
}

// TestCreateEdgeRejectsUnresolvableRef 解析不到的引用当场拒（配置期硬拒，不等到派发）。
func TestCreateEdgeRejectsUnresolvableRef(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	review := mkCap(nsID, "review", 1, []string{"fix"})
	mustCreateCap(t, db, review)

	cases := []struct {
		name string
		e    *Edge
		want string
	}{
		{"目标名字不存在", &Edge{NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: "nobody"}, "nobody"},
		{"目标版本不存在", &Edge{NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: "review@9"}, "review@9"},
		{"上游名字不存在", &Edge{NamespaceID: nsID, FromRef: "ghost", FromPort: "fix", ToRef: "review"}, "ghost"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := db.CreateEdge(ctx, c.e)
			if !errors.Is(err, ErrEdgeRejected) {
				t.Fatalf("err = %v (%T), want ErrEdgeRejected", err, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误里没说清是哪个引用: %v", err)
			}
		})
	}
}

// TestEdgeRefCycleByNameAllowed 环按**名字**看是合法的（契约 §2）：`a@1 → b` 与
// `b → a@2` 是同一条链上的两跳，存下来、两个方向都读得回。
//
// 这条用例原本钉的是「静态环检测按名字判」（a@1 与 a@2 是两个 id、同一个名字，
// 所以环只能按名字判）。检测删掉之后，它改钉**放行**这件事：名字层面的闭环
// 必须能落库，否则 `review → fix → review` 这种编排根本配不出来。
// 兜底在运行期 hop 预算（depth = len(path)），不在这里。
func TestEdgeRefCycleByNameAllowed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	a1 := mkCap(nsID, "a", 1, []string{"next"})
	// v2 必须与 v1 **内容不同**：同内容会被幂等折叠成同一行（I8 / content_hash 幂等），
	// 那样就没有「两个版本、同一个名字」这个场景可测了。
	a2 := &CapabilityVersion{
		NamespaceID: nsID, Name: "a", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		Outputs:      []string{"next"},
		Source:       CapSourceCustom,
	}
	b := mkCap(nsID, "b", 1, []string{"next"})
	mustCreateCap(t, db, a1, a2, b)

	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: "a@1", FromPort: "next", ToRef: "b"}); err != nil {
		t.Fatalf("create a@1 → b: %v", err)
	}
	// b → a@2：名字上构成环（a → b → a），契约 §2 之后**必须**存得下。
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: "b", FromPort: "next", ToRef: "a@2"}); err != nil {
		t.Fatalf("b → a@2 应当可以保存（环是合法编排）：%v", err)
	}
	// 两个方向都读得回：a 的出边到 b，b 的出边到 a@2（钉死引用原样保留）。
	fromA, err := db.EdgesFrom(ctx, nsID, false, "a", "next")
	if err != nil {
		t.Fatalf("edges from a: %v", err)
	}
	if len(fromA) != 1 || RefName(fromA[0].ToRef) != "b" {
		t.Fatalf("a 的出边 = %+v, want a → b", fromA)
	}
	fromB, err := db.EdgesFrom(ctx, nsID, false, "b", "next")
	if err != nil {
		t.Fatalf("edges from b: %v", err)
	}
	if len(fromB) != 1 || fromB[0].ToRef != "a@2" {
		t.Fatalf("b 的出边 = %+v, want b → a@2（钉死引用原样保留）", fromB)
	}

	// 自连也是合法环（长度 1，由 hop 预算兜底）。
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: "a@1", FromPort: "next", ToRef: "a@2"}); err != nil {
		t.Fatalf("a@1 → a@2 应当可以保存（自连）：%v", err)
	}

	// 菱形/扇出仍然合法：a → c 与 a → b 并存（**不能**把「多个下游」当环）。
	c := mkCap(nsID, "c", 1, nil)
	mustCreateCap(t, db, c)
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: "a", FromPort: "next", ToRef: "c"}); err != nil {
		t.Fatalf("a → c 不该被拒（扇出合法）: %v", err)
	}
}

// TestEdgeRefValidationUsesResolvedVersion literals / mapping 的配置期校验
// 用的是**解析到的那个版本**的 params_schema（不是名字的并集）。
func TestEdgeRefValidationUsesResolvedVersion(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	review := mkCap(nsID, "review", 1, []string{"fix"})
	// v1 只有 issue，v2 多了 note
	fixer1 := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"}}}`),
		Source:       CapSourceCustom,
	}
	fixer2 := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"},"note":{"type":"string"}}}`),
		Source:       CapSourceCustom,
	}
	mustCreateCap(t, db, review, fixer1, fixer2)

	// 钉死 fixer@1 → note 不在它的契约里 → 拒（拼错的键在派发时会被静默忽略）
	err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix",
		ToRef: "fixer@1", Literals: json.RawMessage(`{"note":"x"}`),
	})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("fixer@1 + literals.note err = %v, want ErrEdgeRejected（v1 没有 note）", err)
	}

	// 跟随 latest（此刻是 v2）→ note 合法
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix",
		ToRef: "fixer", Literals: json.RawMessage(`{"note":"x"}`),
	}); err != nil {
		t.Fatalf("fixer（latest=@2）+ literals.note 应该通过: %v", err)
	}

	// 同一条边再钉到 v1 也合法（不带 note）
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: "fixer@1",
	}); err != nil {
		t.Fatalf("fixer@1 无 literals 应该通过: %v", err)
	}
	// 两条不同的 link（跟随 vs 钉死）**可以并存**（主键是引用，不是 id）
	edges, err := db.ListEdges(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("edges = %d 条, want 2（fixer 与 fixer@1 是两条 link）: %+v", len(edges), edges)
	}

	// 入口边的 mapping 校验同样按解析到的版本：目标钉到 v1 时，模板里写 note 要拒
	err = db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromIsSource: true, FromRef: "github", FromPort: "*",
		ToRef: "fixer@1", Mapping: json.RawMessage(`{"note":"{{payload.note}}"}`),
	})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("入口边模板里的未知字段 err = %v, want ErrEdgeRejected", err)
	}
	// 指向 v2 则合法（note 在契约里）
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromIsSource: true, FromRef: "github", FromPort: "*",
		ToRef: "fixer@2", Mapping: json.RawMessage(`{"note":"{{payload.note}}"}`),
	}); err != nil {
		t.Fatalf("入口边模板指向 fixer@2 应该通过: %v", err)
	}
}

// TestGraphHashStableAcrossUpgrade 只要边没改，升级**不会**产生新的图版本（P2.2 §7.4）。
//
// 为什么这条是硬要求：graph_versions 是事后追责的底图。用 id 算 hash 时，
// 发一个新版本会让「同一张图」算出另一个 hash，于是每升级一次就多一版垃圾，
// 「当时那张图长什么样」再也对不上。
func TestGraphHashStableAcrossUpgrade(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	review := mkCap(nsID, "review", 1, []string{"fix"})
	fixer := mkCap(nsID, "fixer", 1, nil)
	mustCreateCap(t, db, review, fixer)
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: "fixer"}); err != nil {
		t.Fatalf("create edge: %v", err)
	}

	g1, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph: %v", err)
	}

	// 升级 fixer（加可选入参，不触发闸门）
	fixer2 := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}}}`),
		Source:       CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, withSpec(fixer2)); err != nil {
		t.Fatalf("create fixer@2: %v", err)
	}

	g2, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph after upgrade: %v", err)
	}
	if g1.ContentHash != g2.ContentHash || g1.ID != g2.ID {
		t.Fatalf("升级产生了新图版本：%s → %s（边没改，hash 就不该变）", g1.ID, g2.ID)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM graph_versions WHERE namespace_id = ?`, nsID).Scan(&n); err != nil {
		t.Fatalf("count graph versions: %v", err)
	}
	if n != 1 {
		t.Fatalf("graph_versions = %d 行, want 1", n)
	}

	// 快照里存的是引用（不是 id），所以升级前后逐字一致
	if !g2.GraphHasEdge("review", "fix", "fixer") {
		t.Fatalf("快照里没有那条边（应该按引用记录）: %s", g2.Snapshot)
	}
}

// mustCreateCap 建一批能力，任一步失败就 Fatal（测试夹具用）。
func mustCreateCap(t *testing.T, db *DB, caps ...*CapabilityVersion) {
	t.Helper()
	for _, c := range caps {
		if err := db.CreateCapability(context.Background(), withSpec(c)); err != nil {
			t.Fatalf("create capability %s@%d: %v", c.Name, c.Version, err)
		}
	}
}
