package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// mkCap 构造一个测试用能力定义（未落库）。
//
// 只填**派生字段**（params/outputs/…），正文由 withSpec 在落库前折叠出来
// （见 store_testutil_test.go 的 withSpec 注释）。
func mkCap(nsID, name string, version int, outputs []string) *CapabilityVersion {
	return &CapabilityVersion{
		NamespaceID:  nsID,
		Name:         name,
		Version:      version,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      outputs,
		Source:       CapSourceCustom,
	}
}

// TestCapabilityCreateAndGet 基本往返：存什么读什么（AGENTS.md §3 要求）。
func TestCapabilityCreateAndGet(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := mkCap(nsID, "review", 1, []string{"fix", "clean"})
	c.ParamsSchema = json.RawMessage(`{"type":"object","properties":{"diff":{"type":"string"}}}`)
	c.Lifecycle = Lifecycle{ExpiresInSec: 3600, Retryable: true, MaxAttempts: 2, ResultTTLSec: 600}
	c.Sticky = "worktree"
	if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	if c.ID == "" || c.ContentHash == "" {
		t.Fatalf("CreateCapability did not fill id/hash: %+v", c)
	}

	got, err := db.GetCapability(ctx, nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if got.ID != c.ID || got.Name != "review" || got.Version != 1 {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if len(got.Outputs) != 2 || got.Outputs[0] != "fix" || got.Outputs[1] != "clean" {
		t.Fatalf("outputs mismatch: %v", got.Outputs)
	}
	if got.Sticky != "worktree" || got.Source != CapSourceCustom {
		t.Fatalf("sticky/source mismatch: %q %q", got.Sticky, got.Source)
	}
	if got.Lifecycle.MaxAttempts != 2 || got.Lifecycle.ExpiresInSec != 3600 || !got.Lifecycle.Retryable {
		t.Fatalf("lifecycle mismatch: %+v", got.Lifecycle)
	}
	if !got.HasOutput("fix") || got.HasOutput("nope") {
		t.Fatalf("HasOutput wrong: %v", got.Outputs)
	}

	// by id
	byID, err := db.GetCapabilityByID(ctx, nsID, c.ID)
	if err != nil {
		t.Fatalf("get capability by id: %v", err)
	}
	if byID.ContentHash != c.ContentHash {
		t.Fatalf("content hash mismatch via id lookup")
	}
}

// TestCapabilityIdempotentCreate 同一个 name@version 的同内容重复提交 → 返回同一行；
// 而**换版本号**是另一份内容（v3 起 digest 吃 metadata.version，跨版本折叠已取消）。
func TestCapabilityIdempotentCreate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	first := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(first)); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// 同 name@version、同内容 → 幂等返回已有行（#61 的核心验收）。
	again := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(again)); err != nil {
		t.Fatalf("second create (same content): %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("同内容重复提交没有折叠到已有行: %s vs %s", first.ID, again.ID)
	}

	// 换版本号 = 换内容：digest 吃 metadata.version，所以 review@2 是另一行。
	// （旧行为会把「同内容不同版本」折叠回 @1 —— 那正是 v3 取消的跨版本折叠。）
	next := mkCap(nsID, "review", 2, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(next)); err != nil {
		t.Fatalf("create review@2: %v", err)
	}
	if next.ID == first.ID || next.Version != 2 {
		t.Fatalf("v3 已取消跨版本折叠：review@2 不该折叠成 review@1（got %s@%d）", next.Name, next.Version)
	}

	// 只数 review 的行：默认命名空间自带内置能力（notify.phone@1），
	// 「整表 = 1」会把内置能力算进去，断言就变成验播种而不是验幂等。
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM capability_versions WHERE namespace_id=? AND name='review'`, nsID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("review 有 %d 行，want 2（@1 幂等 + @2 新版本）", n)
	}
}

// TestCapabilitySameVersionDifferentContentRejected 同 name@version 换内容是改定义，
// 违反 append-only（不变量 I8），必须拒绝。
func TestCapabilitySameVersionDifferentContentRejected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	first := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(first)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	changed := mkCap(nsID, "review", 1, []string{"fix", "clean"}) // 出口变了
	err := db.CreateCapability(ctx, withSpec(changed))
	if err == nil {
		t.Fatal("same name@version with different content was accepted (append-only violated)")
	}
}

// TestCapabilityOutputsOrderChangesHash outputs 的**顺序**在 v3 起参与 digest。
//
// 为什么口径变了：content_hash ≡ digest，而 digest 是「规范字节」的摘要（契约 §3.2）。
// 出口是封闭集合（语义上无序），但正文是节点会拿去跑的那份内容 —— 规范字节不同
// 就是不同内容；排序是写入方（前端提交）的责任，不在摘要里偷偷做。
func TestCapabilityOutputsOrderChangesHash(t *testing.T) {
	a := withSpec(mkCap("ns_x", "same", 1, []string{"fix", "clean"}))
	b := withSpec(mkCap("ns_x", "same", 1, []string{"clean", "fix"}))
	ha, err := NormalizeCapability(a)
	if err != nil {
		t.Fatalf("normalize a: %v", err)
	}
	hb, err := NormalizeCapability(b)
	if err != nil {
		t.Fatalf("normalize b: %v", err)
	}
	if ha == hb {
		t.Fatal("outputs 顺序不同却算出同一个 hash：规范字节不同就是不同内容")
	}
	// 同一份顺序（不同对象）必须同 hash —— 否则幂等永远命不中。
	c := withSpec(mkCap("ns_x", "same", 1, []string{"fix", "clean"}))
	hc, err := NormalizeCapability(c)
	if err != nil {
		t.Fatalf("normalize c: %v", err)
	}
	if ha != hc {
		t.Fatalf("同一份内容算出两个 hash: %s vs %s", ha, hc)
	}
}

// TestCapabilitySchemaKeyOrderInsensitive params_schema 的**键顺序**不影响 hash。
//
// params 是 JSON Schema，键顺序没有语义：digest 之前会把它归一成 key 有序的紧凑
// 形式（artifact 的 canonicalCopy），所以同样的 schema 换一种写法仍是同一份内容。
func TestCapabilitySchemaKeyOrderInsensitive(t *testing.T) {
	a := mkCap("ns_x", "same", 1, nil)
	a.ParamsSchema = json.RawMessage(`{"type":"object","required":["a"]}`)
	a = withSpec(a)
	b := mkCap("ns_x", "same", 1, nil)
	b.ParamsSchema = json.RawMessage(`{ "required": ["a"], "type": "object" }`)
	b = withSpec(b)

	ha, err := NormalizeCapability(a)
	if err != nil {
		t.Fatalf("normalize a: %v", err)
	}
	hb, err := NormalizeCapability(b)
	if err != nil {
		t.Fatalf("normalize b: %v", err)
	}
	if ha != hb {
		t.Fatalf("schema 键顺序改变了 hash: %s vs %s", ha, hb)
	}
}

// TestCapabilityDifferentContentDifferentHash 不同内容必须不同 hash。
func TestCapabilityDifferentContentDifferentHash(t *testing.T) {
	base := withSpec(mkCap("ns_x", "review", 1, []string{"fix"}))
	h1, err := NormalizeCapability(base)
	if err != nil {
		t.Fatalf("normalize base: %v", err)
	}

	variants := []*CapabilityVersion{
		withSpec(&CapabilityVersion{NamespaceID: "ns_x", Name: "review", Version: 1, ParamsSchema: json.RawMessage(`{"type":"object"}`), Outputs: []string{"clean"}}),
		withSpec(&CapabilityVersion{NamespaceID: "ns_x", Name: "review", Version: 1, ParamsSchema: json.RawMessage(`{"type":"object"}`), Outputs: []string{"fix"}, Sticky: "worktree"}),
		withSpec(&CapabilityVersion{NamespaceID: "ns_x", Name: "review", Version: 1, ParamsSchema: json.RawMessage(`{"type":"object"}`), Outputs: []string{"fix"}, Lifecycle: Lifecycle{MaxAttempts: 5}}),
		withSpec(&CapabilityVersion{NamespaceID: "ns_x", Name: "review", Version: 1, ParamsSchema: json.RawMessage(`{"type":"string"}`), Outputs: []string{"fix"}}),
	}
	for i, v := range variants {
		h, err := NormalizeCapability(v)
		if err != nil {
			t.Fatalf("normalize variant %d: %v", i, err)
		}
		if h == h1 {
			t.Errorf("variant %d got same hash as base (content hash is not discriminating)", i)
		}
	}
}

// TestCapabilityNormalizeIncludesName name 必须参与 hash：不同名字的能力不能被合并成同一行。
//
// 这是命名空间内 UNIQUE(namespace_id, content_hash) 的直接后果：若 name 不参与，
// `fix1` 与 `fix2`（schema/outputs 恰好相同）会被当成同一份内容，
// 第二个 CreateCapability 会被幂等逻辑改写成第一个——两个能力默默合并。
func TestCapabilityNormalizeIncludesName(t *testing.T) {
	a := mkCap("ns_x", "fix1", 1, []string{"done"})
	b := mkCap("ns_x", "fix2", 1, []string{"done"})
	ha, err := NormalizeCapability(a)
	if err != nil {
		t.Fatalf("normalize a: %v", err)
	}
	hb, err := NormalizeCapability(b)
	if err != nil {
		t.Fatalf("normalize b: %v", err)
	}
	if ha == hb {
		t.Fatal("different names got same content_hash; capabilities would be silently merged")
	}

	// 同 name 不同 version **必须**不同 hash：digest 吃 metadata.version（契约 §3.2）。
	// 旧口径（version 不进 hash）会让「同内容换个版本号」折叠回旧行 —— 那正是 v3
	// 取消的形态；现在换版本号就是换内容，撞 (name,version) 才是 ErrVersionConflict。
	v2 := withSpec(mkCap("ns_x", "fix1", 2, []string{"done"}))
	hv2, err := NormalizeCapability(v2)
	if err != nil {
		t.Fatalf("normalize v2: %v", err)
	}
	if ha == hv2 {
		t.Fatal("同 name 不同 version 得到同一个 hash：跨版本折叠会借尸还魂")
	}
}

// TestCapabilityVersionAppendOnly 版本共存（A11）：review@1 与 review@2 同时存在，互不影响。
func TestCapabilityVersionAppendOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	v1 := mkCap(nsID, "review", 1, []string{"fix"})
	v2 := mkCap(nsID, "review", 2, []string{"fix", "clean"})
	if err := db.CreateCapability(ctx, withSpec(v1)); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if err := db.CreateCapability(ctx, withSpec(v2)); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v1.ID == v2.ID {
		t.Fatal("v1 and v2 share the same id")
	}

	g1, err := db.GetCapability(ctx, nsID, "review", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	g2, err := db.GetCapability(ctx, nsID, "review", 2)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if len(g1.Outputs) != 1 || len(g2.Outputs) != 2 {
		t.Fatalf("versions not independent: v1=%v v2=%v", g1.Outputs, g2.Outputs)
	}

	next, err := db.NextCapabilityVersion(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("next version: %v", err)
	}
	if next != 3 {
		t.Fatalf("NextCapabilityVersion = %d, want 3", next)
	}
	// 新名字从 1 开始
	next, err = db.NextCapabilityVersion(ctx, nsID, "brand-new")
	if err != nil {
		t.Fatalf("next version new: %v", err)
	}
	if next != 1 {
		t.Fatalf("NextCapabilityVersion(new) = %d, want 1", next)
	}
}

// TestCapabilityNamespaceIsolation 不变量 I11：跨命名空间查不到能力定义。
//
// 默认命名空间里不再是「空」，而是自带内置能力（notify.phone@1）——所以隔离的正确断言
// 不是「B 一条都没有」，而是「B 里一行都不属于 A 写的那份定义」。这里同时验两套独立资源
// （两个 newTestNS = 两个 default 命名空间）与**同一个库里另建的命名空间**（#162 真正
// 新增的隔离面）。
func TestCapabilityNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, _ := newTestNS(t, db)

	c := mkCap(nsA, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.GetCapabilityByID(ctx, nsB, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-namespace GetCapabilityByID err = %v, want ErrNotFound", err)
	}
	if _, err := db.GetCapability(ctx, nsB, "review", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-namespace GetCapability err = %v, want ErrNotFound", err)
	}

	// 同一个库里另建的命名空间：同名能力各自一份，那边的那份不能被这边的看见（反之亦然）
	sibling := newTestExtraNS(t, db, "staging")
	sibCap := mkCap(sibling, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(sibCap)); err != nil {
		t.Fatalf("create sibling capability: %v", err)
	}
	if sibCap.ID == c.ID {
		t.Fatal("两个命名空间写出了同一个 capability id（namespace 没进唯一键）")
	}
	if got, err := db.GetCapability(ctx, sibling, "review", 1); err != nil {
		t.Fatalf("sibling GetCapability: %v", err)
	} else if got.ID != sibCap.ID || got.NamespaceID != sibling {
		t.Fatalf("sibling GetCapability 返回了错的行: %+v", got)
	}

	for _, ns := range []string{nsA, nsB, sibling} {
		list, err := db.ListCapabilities(ctx, ns)
		if err != nil {
			t.Fatalf("list %s: %v", ns, err)
		}
		for _, got := range list {
			// 每一行都必须自带本命名空间 id（I11：命名空间是唯一隔离维度）
			if got.NamespaceID != ns {
				t.Fatalf("ListCapabilities(%s) 返回了别的命名空间的行: %s@%d (%s)",
					ns, got.Name, got.Version, got.NamespaceID)
			}
		}
		if len(list) == 0 {
			t.Fatalf("ListCapabilities(%s) 为空：内置能力应已播种", ns)
		}
	}
}

// TestCapabilityEmptyListIsArray 不变量 I9：真·空列表必须返回 [] 而不是 nil。
//
// 为什么先把能力行删干净：#162 之后每个命名空间都自带内置能力（notify.phone@1），
// 「新建命名空间 → 能力列表为空」这个前提已经不存在了。要验 I9 就必须先造出空表，
// 否则这条用例会变成「验播种」（len==0 vs len==1）而不是「验 nil vs []」。
func TestCapabilityEmptyListIsArray(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	// 直删能力行（本用例不涉及任务/边/节点声明，无外键阻挡）
	mustExec(t, db, `DELETE FROM capability_versions WHERE namespace_id = ?`, nsID)

	list, err := db.ListCapabilities(context.Background(), nsID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if list == nil {
		t.Fatal("ListCapabilities returned nil for empty result (need [])")
	}
	if len(list) != 0 {
		t.Fatalf("want 0, got %d", len(list))
	}
}

// TestCapabilityEmptyOutputs 没有出口的能力也允许（end 能力的形态），
// 且读回来是 [] 不是 null。
//
// 名字故意不用 notify.phone：内置能力已经占了这个 name@1（且内容不同），
// 再建同名能力会被 append-only 拦住（I8）——那是另一条不变量，不该混进来。
func TestCapabilityEmptyOutputs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	const name = "digest.daily"
	c := mkCap(nsID, name, 1, nil)
	if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetCapability(ctx, nsID, name, 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Outputs == nil {
		t.Fatal("empty outputs decoded as nil, want []")
	}
	if len(got.Outputs) != 0 {
		t.Fatalf("want 0 outputs, got %v", got.Outputs)
	}
}

// TestCapabilityValidation 入参校验。
func TestCapabilityValidation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if err := db.CreateCapability(ctx, withSpec(nil)); err == nil {
		t.Error("CreateCapability(nil) accepted")
	}
	// 没带 namespace：必须是 ErrNamespaceRequired（绝不回退默认命名空间）。
	// 注意不能只验「报错了」：以前的实现会因缺 org 报错，那不是同一条不变量。
	if err := db.CreateCapability(ctx, &CapabilityVersion{Name: "x", Version: 1}); !errors.Is(err, ErrNamespaceRequired) {
		t.Errorf("CreateCapability without namespace err = %v, want ErrNamespaceRequired", err)
	}
	// 通配符不是写入作用域：store 层不接受 "*"（它只在 HTTP 读端点解释）。
	if err := db.CreateCapability(ctx, &CapabilityVersion{NamespaceID: AllNamespaces, Name: "x", Version: 1}); !errors.Is(err, ErrNamespaceRequired) {
		t.Errorf("CreateCapability on %q err = %v, want ErrNamespaceRequired", AllNamespaces, err)
	}
	if err := db.CreateCapability(ctx, &CapabilityVersion{NamespaceID: nsID, Version: 1}); err == nil {
		t.Error("CreateCapability without name accepted")
	}
	if err := db.CreateCapability(ctx, &CapabilityVersion{NamespaceID: nsID, Name: "x", Version: 0}); err == nil {
		t.Error("CreateCapability with version 0 accepted")
	}
	if _, err := NormalizeCapability(nil); err == nil {
		t.Error("NormalizeCapability(nil) accepted")
	}
}

// TestLifecycleIdempotentDefault 默认 idempotent=true，显式 false 生效。
func TestLifecycleIdempotentDefault(t *testing.T) {
	if !(Lifecycle{}).IsIdempotent() {
		t.Error("default lifecycle should be idempotent")
	}
	f := false
	if (Lifecycle{Idempotent: &f}).IsIdempotent() {
		t.Error("explicit idempotent:false ignored")
	}
	tr := true
	if !(Lifecycle{Idempotent: &tr}).IsIdempotent() {
		t.Error("explicit idempotent:true ignored")
	}
	if (Lifecycle{}).MaxAttemptsOrDefault() != DefaultMaxAttempts {
		t.Errorf("default maxAttempts = %d, want %d", (Lifecycle{}).MaxAttemptsOrDefault(), DefaultMaxAttempts)
	}
	if (Lifecycle{MaxAttempts: 7}).MaxAttemptsOrDefault() != 7 {
		t.Error("explicit maxAttempts ignored")
	}
}

// ---------- graph_versions（#61 下半） ----------

// TestGraphVersionOnDemand 同 hash 两次 → 只有 1 行（#61 验收）。
func TestGraphVersionOnDemand(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	up := mkCap(nsID, "review", 1, []string{"fix"})
	down := mkCap(nsID, "fix", 1, nil)
	if err := db.CreateCapability(ctx, withSpec(up)); err != nil {
		t.Fatalf("create up: %v", err)
	}
	if err := db.CreateCapability(ctx, withSpec(down)); err != nil {
		t.Fatalf("create down: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down.ID,
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}

	g1, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph 1: %v", err)
	}
	g2, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure graph 2: %v", err)
	}
	if g1.ID != g2.ID {
		t.Fatalf("same graph produced two versions: %s vs %s", g1.ID, g2.ID)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM graph_versions WHERE namespace_id=?`, nsID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("graph_versions has %d rows, want 1", n)
	}
	// 快照里存的是**引用**（P2.2），不是 id：否则能力发新版会让同一张图算出另一个 hash。
	if !g1.GraphHasEdge(capRef(up), "fix", capRef(down)) {
		t.Fatalf("snapshot missing the edge: %s", g1.Snapshot)
	}
}

// TestGraphVersionChangesWithEdges 改边 → 新 hash → 新版本行。
func TestGraphVersionChangesWithEdges(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	up := mkCap(nsID, "review", 1, []string{"fix", "clean"})
	down1 := mkCap(nsID, "fix", 1, nil)
	down2 := mkCap(nsID, "clean", 1, nil)
	for _, c := range []*CapabilityVersion{up, down1, down2} {
		if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
			t.Fatalf("create cap: %v", err)
		}
	}
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: down1.ID}); err != nil {
		t.Fatalf("edge 1: %v", err)
	}
	g1, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	// 加一条边 → 图变了
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: up.ID, FromPort: "clean", ToRef: down2.ID}); err != nil {
		t.Fatalf("edge 2: %v", err)
	}
	g2, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if g1.ID == g2.ID {
		t.Fatal("graph did not change after adding an edge")
	}
	// 删回原状 → hash 回到 g1 的（同一份图只有一行，复用旧版本）
	if err := db.DeleteEdge(ctx, nsID, false, up.ID, "clean", down2.ID); err != nil {
		t.Fatalf("delete edge: %v", err)
	}
	g3, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure 3: %v", err)
	}
	if g3.ID != g1.ID {
		t.Fatalf("reverting to the same graph should reuse version %s, got %s", g1.ID, g3.ID)
	}
}

// TestGraphVersionEmptyGraph 空图也能落版（hash 稳定）。
func TestGraphVersionEmptyGraph(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	g1, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure empty: %v", err)
	}
	if g1.ContentHash == "" || g1.ID == "" {
		t.Fatalf("empty graph version incomplete: %+v", g1)
	}
	g2, err := db.EnsureGraphVersion(ctx, nsID)
	if err != nil {
		t.Fatalf("ensure empty twice: %v", err)
	}
	if g1.ID != g2.ID {
		t.Fatal("empty graph produced two versions")
	}
	if _, err := db.GetGraphVersion(ctx, nsID, g1.ID); err != nil {
		t.Fatalf("get graph version: %v", err)
	}
	if _, err := db.GetGraphVersion(ctx, nsID, "grf_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGraphVersion missing err = %v, want ErrNotFound", err)
	}
}

// TestEdgeRejectsPortNotInOutputs 不变量 I2 / A5：出口封闭。
func TestEdgeRejectsPortNotInOutputs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	up := mkCap(nsID, "review", 1, []string{"fix"})
	down := mkCap(nsID, "fix", 1, nil)
	for _, c := range []*CapabilityVersion{up, down} {
		if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: up.ID, FromPort: "not-an-output", ToRef: down.ID})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("edge with undefined port: err = %v, want ErrEdgeRejected", err)
	}
}

// TestEdgeRejectsCrossNamespace 不变量 I11 第 ① 条：不放跨命名空间的边。
//
// 语义在 #162 之后从「跨组织被拒」升级成「跨命名空间被拒」：能力 id 全局唯一，
// 把另一个命名空间的 id 填进来在 SQL 层是合法的（外键只保证行存在，不保证同属一处），
// 所以必须显式校验 → *CrossNamespaceError（handler 一律 400，不静默忽略）。
//
// 为什么用**同一个库里另建的命名空间**而不是两个 newTestNS：后者在「两套独立资源」
// 这个维度上也能拦住，拦不住「同一个库里串数据」这个真实错误。
func TestEdgeRejectsCrossNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB := newTestExtraNS(t, db, "staging")

	upA := mkCap(nsA, "review", 1, []string{"fix"})
	sinkA := mkCap(nsA, "fix", 1, nil)
	upB := mkCap(nsB, "review", 1, []string{"fix"})
	foreign := mkCap(nsB, "fix", 1, nil)
	for _, c := range []*CapabilityVersion{upA, sinkA, upB, foreign} {
		if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
			t.Fatalf("create %s@%d: %v", c.Name, c.Version, err)
		}
	}

	assertCross := func(label string, err error, want, got string) {
		t.Helper()
		var cross *CrossNamespaceError
		if !errors.As(err, &cross) {
			t.Fatalf("%s: err = %v (%T), want *CrossNamespaceError", label, err, err)
		}
		if cross.Want != want || cross.Got != got {
			t.Fatalf("%s: CrossNamespaceError = want %q got %q, real want %q got %q",
				label, cross.Want, cross.Got, want, got)
		}
	}

	// ① 右端在别的命名空间
	err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsA, FromRef: upA.ID, FromPort: "fix", ToRef: foreign.ID,
	})
	assertCross("to_capability", err, nsA, nsB)

	// ② 左端在别的命名空间（右端合法，所以不会先被右端拦下）
	err = db.CreateEdge(ctx, &Edge{
		NamespaceID: nsA, FromRef: upB.ID, FromPort: "fix", ToRef: sinkA.ID,
	})
	assertCross("from_capability", err, nsA, nsB)

	// ③ 同类配置在 nsB 里完全合法：隔离不等于禁用（否则 staging 里就没法建图了）
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsB, FromRef: upB.ID, FromPort: "fix", ToRef: foreign.ID,
	}); err != nil {
		t.Fatalf("nsB 内部的合法边被拒: %v", err)
	}

	// ④ 能力 id 根本不存在 → ErrEdgeRejected（配置错误，不是隔离违反，两个错误不能互相串）
	err = db.CreateEdge(ctx, &Edge{
		NamespaceID: nsA, FromRef: "cap_does_not_exist", FromPort: "fix", ToRef: sinkA.ID,
	})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("missing capability: err = %v, want ErrEdgeRejected", err)
	}
	var cross *CrossNamespaceError
	if errors.As(err, &cross) {
		t.Fatalf("不存在的 capability 被报成了跨命名空间: %v", err)
	}

	// 被拒的三次都不能留下半条边（nsA 里只能有 0 条）
	edges, err := db.ListEdges(ctx, nsA, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("被拒的跨命名空间边还是落库了: %+v", edges)
	}
}

// TestEdgesFromLookup 派发第 ① 步的查表入口。
func TestEdgesFromLookup(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	up := mkCap(nsID, "review", 1, []string{"fix"})
	d1 := mkCap(nsID, "fix1", 1, nil)
	d2 := mkCap(nsID, "fix2", 1, nil)
	for _, c := range []*CapabilityVersion{up, d1, d2} {
		if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	// 扇出：一个 (cap, port) 两条边
	for _, to := range []*CapabilityVersion{d1, d2} {
		if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: to.ID}); err != nil {
			t.Fatalf("create edge: %v", err)
		}
	}
	got, err := db.EdgesFrom(ctx, nsID, false, up.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("EdgesFrom returned %d targets, want 2 (fanout)", len(got))
	}

	// 不存在的出口 → 空列表（不是 nil）
	none, err := db.EdgesFrom(ctx, nsID, false, up.Name, "clean")
	if err != nil {
		t.Fatalf("EdgesFrom missing port: %v", err)
	}
	if none == nil {
		t.Fatal("EdgesFrom returned nil for no rows (need [])")
	}
	if len(none) != 0 {
		t.Fatalf("want 0, got %d", len(none))
	}

	// 幂等：重复建同一条边不报错、不重复
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: up.ID, FromPort: "fix", ToRef: d1.ID}); err != nil {
		t.Fatalf("idempotent create edge: %v", err)
	}
	got, err = db.EdgesFrom(ctx, nsID, false, up.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom after dup: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("duplicate edge created a row: got %d targets", len(got))
	}

	// 删除
	if err := db.DeleteEdge(ctx, nsID, false, up.ID, "fix", d1.ID); err != nil {
		t.Fatalf("delete edge: %v", err)
	}
	if err := db.DeleteEdge(ctx, nsID, false, up.ID, "fix", d1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing edge err = %v, want ErrNotFound", err)
	}
	got, err = db.EdgesFrom(ctx, nsID, false, up.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom after delete: %v", err)
	}
	if len(got) != 1 || got[0].ToRef != capRef(d2) {
		t.Fatalf("after delete got %v, want [%s]", got, capRef(d2))
	}
}

// TestListEdgesEmptyIsArray 不变量 I9。
func TestListEdgesEmptyIsArray(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	edges, err := db.ListEdges(context.Background(), nsID, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if edges == nil {
		t.Fatal("ListEdges returned nil (need [])")
	}
}

// TestCapabilityDisplayName 显示名的往返 + 三个必须成立的边界。
//
// 这个测试盯的是 display_name 的"装饰字段"性质 —— 它一旦被误算进
// content_hash，或者改它产生新版本，I8（能力定义 append-only）就被破坏了：
// 用户改个中文名，旧 trace 会因为 capHash 对不上而无法解释。
func TestCapabilityDisplayName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// ① 往返：存什么读什么
	c := mkCap(nsID, "analyze-pr", 1, []string{"needs-fix"})
	c.DisplayName = "分析 PR"
	if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetCapability(ctx, nsID, "analyze-pr", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DisplayName != "分析 PR" {
		t.Fatalf("display name roundtrip: got %q want %q", got.DisplayName, "分析 PR")
	}

	// ② 显示名不参与 content_hash：同一份定义、不同显示名 → 同一行
	hashBefore := got.ContentHash
	c2 := mkCap(nsID, "analyze-pr", 1, []string{"needs-fix"})
	c2.DisplayName = "完全不同的名字"
	if err := db.CreateCapability(ctx, withSpec(c2)); err != nil {
		t.Fatalf("create same content with different display name: %v", err)
	}
	if c2.ContentHash != hashBefore {
		t.Fatalf("display name leaked into content_hash: %q vs %q", c2.ContentHash, hashBefore)
	}
	if c2.ID != c.ID {
		t.Fatalf("same content produced a second row: %q vs %q", c2.ID, c.ID)
	}
	// 幂等创建时带了新显示名 → 应该更新那一列（而不是静默丢弃）
	if c2.DisplayName != "完全不同的名字" {
		t.Fatalf("idempotent create dropped new display name: %q", c2.DisplayName)
	}

	// ③ 单独改名：只动 display_name，不动 hash / 不产生新版本
	if err := db.SetCapabilityDisplayName(ctx, nsID, c.ID, "改过的名字"); err != nil {
		t.Fatalf("set display name: %v", err)
	}
	after, err := db.GetCapabilityByID(ctx, nsID, c.ID)
	if err != nil {
		t.Fatalf("get after rename: %v", err)
	}
	if after.DisplayName != "改过的名字" {
		t.Fatalf("rename did not stick: %q", after.DisplayName)
	}
	if after.ContentHash != hashBefore {
		t.Fatalf("rename changed content_hash: %q vs %q", after.ContentHash, hashBefore)
	}
	// 版本数没变（还是 1个 analyze-pr）：注意命名空间里还有内置能力（notify.phone@1），
	// 所以只能按 name 过滤计数；「整表 = 1」会把内置能力算进去。
	list, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	versions := 0
	for _, x := range list {
		if x.Name == "analyze-pr" {
			versions++
		}
	}
	if versions != 1 {
		t.Fatalf("rename created a new version: %d rows of analyze-pr", versions)
	}

	// ④ 别的命名空间改不动（不变量 I11）
	otherNS, _ := newTestNS(t, db)
	if err := db.SetCapabilityDisplayName(ctx, otherNS, c.ID, "偷改"); err == nil {
		t.Fatal("cross-namespace display name update should fail")
	}
}

// TestContentHashEqualsDigest content_hash 就是 digest 的裸 hex（契约 §4）。
//
// 判据是「从落库的那一行拼出产物再算一次 digest」——两边必须逐字节一致，否则
// 部署永远判 stale、幂等永远命不中。顺带钉住两条 v3 推论：
//   - balance 是死字段（P1-b 起恒写 '{}'），**不参与** hash；
//   - displayName 不在 digest 里，改它不算换内容。
func TestContentHashEqualsDigest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := mkCap(nsID, "digest.probe", 1, []string{"fix"})
	c.DisplayName = "展示名不参与摘要"
	if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
		t.Fatalf("create: %v", err)
	}
	digest := c.Digest()
	if !strings.HasPrefix(digest, artifact.DigestPrefix) {
		t.Fatalf("Digest() = %q，缺少 %q 前缀", digest, artifact.DigestPrefix)
	}
	if strings.TrimPrefix(digest, artifact.DigestPrefix) != c.ContentHash {
		t.Fatalf("Digest() 与 content_hash 不是同一个值: %q vs %q", digest, c.ContentHash)
	}
	// 从**落库的那一行**重算：必须得回同一个 hash（行 ↔ 产物只有一处拼装）。
	stored, err := db.GetCapability(ctx, nsID, "digest.probe", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	a := stored.Artifact()
	want, err := artifact.Digest(&a)
	if err != nil {
		t.Fatalf("digest row: %v", err)
	}
	if want != digest {
		t.Fatalf("落库行重算的 digest = %q，want %q（content_hash 与正文不同源）", want, digest)
	}

	// displayName 只影响 metadata，不进 digest：改完 hash 不变。
	before := stored.ContentHash
	if err := db.SetCapabilityDisplayName(ctx, nsID, stored.ID, "改个名"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	renamed, err := db.GetCapability(ctx, nsID, "digest.probe", 1)
	if err != nil {
		t.Fatalf("get renamed: %v", err)
	}
	if renamed.ContentHash != before {
		t.Fatalf("改 displayName 让 content_hash 变了: %s → %s", before, renamed.ContentHash)
	}
}
