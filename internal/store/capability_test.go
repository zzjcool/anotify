package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// mkCap 构造一个测试用能力定义（未落库）。
func mkCap(orgID, name string, version int, outputs []string) *CapabilityVersion {
	return &CapabilityVersion{
		OrgID:        orgID,
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
	orgID, _ := newTestOrg(t, db)

	c := mkCap(orgID, "review", 1, []string{"fix", "clean"})
	c.ParamsSchema = json.RawMessage(`{"type":"object","properties":{"diff":{"type":"string"}}}`)
	c.Lifecycle = Lifecycle{ExpiresInSec: 3600, Retryable: true, MaxAttempts: 2, ResultTTLSec: 600}
	c.Sticky = "worktree"
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	if c.ID == "" || c.ContentHash == "" {
		t.Fatalf("CreateCapability did not fill id/hash: %+v", c)
	}

	got, err := db.GetCapability(ctx, orgID, "review", 1)
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
	byID, err := db.GetCapabilityByID(ctx, orgID, c.ID)
	if err != nil {
		t.Fatalf("get capability by id: %v", err)
	}
	if byID.ContentHash != c.ContentHash {
		t.Fatalf("content hash mismatch via id lookup")
	}
}

// TestCapabilityIdempotentCreate 同内容两次创建 → 返回同一行，库里只有 1 条（#61 核心验收）。
func TestCapabilityIdempotentCreate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	first := mkCap(orgID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// 同内容、**不同 name/version**：content_hash 仍应相同 → 幂等返回已有行
	second := mkCap(orgID, "review", 2, []string{"fix"})
	if err := db.CreateCapability(ctx, second); err != nil {
		t.Fatalf("second create (same content): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("same content produced different rows: %s vs %s", first.ID, second.ID)
	}
	if second.Name != first.Name || second.Version != first.Version {
		t.Fatalf("second create should be filled with existing row, got %s@%d", second.Name, second.Version)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM capability_versions WHERE org_id=?`, orgID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("capability_versions has %d rows, want 1 (same content must not duplicate)", n)
	}

	// 完全相同的调用再来一次也应幂等
	third := mkCap(orgID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, third); err != nil {
		t.Fatalf("third create: %v", err)
	}
	if third.ID != first.ID {
		t.Fatalf("third create returned different id")
	}
}

// TestCapabilitySameVersionDifferentContentRejected 同 name@version 换内容是改定义，
// 违反 append-only（不变量 I8），必须拒绝。
func TestCapabilitySameVersionDifferentContentRejected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	first := mkCap(orgID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	changed := mkCap(orgID, "review", 1, []string{"fix", "clean"}) // 出口变了
	err := db.CreateCapability(ctx, changed)
	if err == nil {
		t.Fatal("same name@version with different content was accepted (append-only violated)")
	}
}

// TestCapabilityOutputsOrderInsensitive outputs 是集合：顺序不影响 hash。
func TestCapabilityOutputsOrderInsensitive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	a := mkCap(orgID, "a", 1, []string{"fix", "clean"})
	b := mkCap(orgID, "a", 2, []string{"clean", "fix"})
	if err := db.CreateCapability(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := db.CreateCapability(ctx, b); err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.ContentHash != b.ContentHash {
		t.Fatalf("outputs order changed the hash: %s vs %s", a.ContentHash, b.ContentHash)
	}
}

// TestCapabilitySchemaKeyOrderInsensitive params_schema 的键顺序不影响 hash。
func TestCapabilitySchemaKeyOrderInsensitive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	a := mkCap(orgID, "x", 1, nil)
	a.ParamsSchema = json.RawMessage(`{"type":"object","required":["a"]}`)
	b := mkCap(orgID, "x", 2, nil)
	b.ParamsSchema = json.RawMessage(`{ "required": ["a"], "type": "object" }`)
	if err := db.CreateCapability(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := db.CreateCapability(ctx, b); err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.ContentHash != b.ContentHash {
		t.Fatalf("schema key order changed the hash: %s vs %s", a.ContentHash, b.ContentHash)
	}
}

// TestCapabilityDifferentContentDifferentHash 不同内容必须不同 hash。
func TestCapabilityDifferentContentDifferentHash(t *testing.T) {
	base := mkCap("org_x", "review", 1, []string{"fix"})
	h1, err := NormalizeCapability(base)
	if err != nil {
		t.Fatalf("normalize base: %v", err)
	}

	variants := []*CapabilityVersion{
		{OrgID: "org_x", Name: "review", Version: 1, ParamsSchema: json.RawMessage(`{"type":"object"}`), Outputs: []string{"clean"}},
		{OrgID: "org_x", Name: "review", Version: 1, ParamsSchema: json.RawMessage(`{"type":"object"}`), Outputs: []string{"fix"}, Sticky: "worktree"},
		{OrgID: "org_x", Name: "review", Version: 1, ParamsSchema: json.RawMessage(`{"type":"object"}`), Outputs: []string{"fix"}, Lifecycle: Lifecycle{MaxAttempts: 5}},
		{OrgID: "org_x", Name: "review", Version: 1, ParamsSchema: json.RawMessage(`{"type":"string"}`), Outputs: []string{"fix"}},
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
// 这是全组织内 UNIQUE(org_id, content_hash) 的直接后果：若 name 不参与，
// `fix1` 与 `fix2`（schema/outputs 恰好相同）会被当成同一份内容，
// 第二个 CreateCapability 会被幂等逻辑改写成第一个——两个能力默默合并。
func TestCapabilityNormalizeIncludesName(t *testing.T) {
	a := mkCap("org_x", "fix1", 1, []string{"done"})
	b := mkCap("org_x", "fix2", 1, []string{"done"})
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

	// 同 name 不同 version 仍然共享同一个 hash（幂等创建的基础）
	v2 := mkCap("org_x", "fix1", 2, []string{"done"})
	hv2, err := NormalizeCapability(v2)
	if err != nil {
		t.Fatalf("normalize v2: %v", err)
	}
	if ha != hv2 {
		t.Fatal("same name+content got different hash across versions; idempotent create would never hit")
	}
}

// TestCapabilityVersionAppendOnly 版本共存（A11）：review@1 与 review@2 同时存在，互不影响。
func TestCapabilityVersionAppendOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	v1 := mkCap(orgID, "review", 1, []string{"fix"})
	v2 := mkCap(orgID, "review", 2, []string{"fix", "clean"})
	if err := db.CreateCapability(ctx, v1); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if err := db.CreateCapability(ctx, v2); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v1.ID == v2.ID {
		t.Fatal("v1 and v2 share the same id")
	}

	g1, err := db.GetCapability(ctx, orgID, "review", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	g2, err := db.GetCapability(ctx, orgID, "review", 2)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if len(g1.Outputs) != 1 || len(g2.Outputs) != 2 {
		t.Fatalf("versions not independent: v1=%v v2=%v", g1.Outputs, g2.Outputs)
	}

	next, err := db.NextCapabilityVersion(ctx, orgID, "review")
	if err != nil {
		t.Fatalf("next version: %v", err)
	}
	if next != 3 {
		t.Fatalf("NextCapabilityVersion = %d, want 3", next)
	}
	// 新名字从 1 开始
	next, err = db.NextCapabilityVersion(ctx, orgID, "brand-new")
	if err != nil {
		t.Fatalf("next version new: %v", err)
	}
	if next != 1 {
		t.Fatalf("NextCapabilityVersion(new) = %d, want 1", next)
	}
}

// TestCapabilityOrgIsolation 不变量 I10：跨组织查不到。
func TestCapabilityOrgIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgA, _ := newTestOrg(t, db)
	orgB, _ := newTestOrg(t, db)

	c := mkCap(orgA, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.GetCapabilityByID(ctx, orgB, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org GetCapabilityByID err = %v, want ErrNotFound", err)
	}
	if _, err := db.GetCapability(ctx, orgB, "review", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org GetCapability err = %v, want ErrNotFound", err)
	}
	listB, err := db.ListCapabilities(ctx, orgB)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("org B sees %d capabilities from org A", len(listB))
	}
}

// TestCapabilityEmptyListIsArray 不变量 I9。
func TestCapabilityEmptyListIsArray(t *testing.T) {
	db := newTestDB(t)
	orgID, _ := newTestOrg(t, db)
	list, err := db.ListCapabilities(context.Background(), orgID)
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
func TestCapabilityEmptyOutputs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	c := mkCap(orgID, "notify.phone", 1, nil)
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetCapability(ctx, orgID, "notify.phone", 1)
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
	orgID, _ := newTestOrg(t, db)

	if err := db.CreateCapability(ctx, nil); err == nil {
		t.Error("CreateCapability(nil) accepted")
	}
	if err := db.CreateCapability(ctx, &CapabilityVersion{Name: "x", Version: 1}); err == nil {
		t.Error("CreateCapability without org accepted")
	}
	if err := db.CreateCapability(ctx, &CapabilityVersion{OrgID: orgID, Version: 1}); err == nil {
		t.Error("CreateCapability without name accepted")
	}
	if err := db.CreateCapability(ctx, &CapabilityVersion{OrgID: orgID, Name: "x", Version: 0}); err == nil {
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
	orgID, _ := newTestOrg(t, db)

	up := mkCap(orgID, "review", 1, []string{"fix"})
	down := mkCap(orgID, "fix", 1, nil)
	if err := db.CreateCapability(ctx, up); err != nil {
		t.Fatalf("create up: %v", err)
	}
	if err := db.CreateCapability(ctx, down); err != nil {
		t.Fatalf("create down: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{
		OrgID: orgID, FromCapability: up.ID, FromPort: "fix", ToCapability: down.ID,
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}

	g1, err := db.EnsureGraphVersion(ctx, orgID)
	if err != nil {
		t.Fatalf("ensure graph 1: %v", err)
	}
	g2, err := db.EnsureGraphVersion(ctx, orgID)
	if err != nil {
		t.Fatalf("ensure graph 2: %v", err)
	}
	if g1.ID != g2.ID {
		t.Fatalf("same graph produced two versions: %s vs %s", g1.ID, g2.ID)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM graph_versions WHERE org_id=?`, orgID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("graph_versions has %d rows, want 1", n)
	}
	if !g1.GraphHasEdge(up.ID, "fix", down.ID) {
		t.Fatalf("snapshot missing the edge: %s", g1.Snapshot)
	}
}

// TestGraphVersionChangesWithEdges 改边 → 新 hash → 新版本行。
func TestGraphVersionChangesWithEdges(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	up := mkCap(orgID, "review", 1, []string{"fix", "clean"})
	down1 := mkCap(orgID, "fix", 1, nil)
	down2 := mkCap(orgID, "clean", 1, nil)
	for _, c := range []*CapabilityVersion{up, down1, down2} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create cap: %v", err)
		}
	}
	if err := db.CreateEdge(ctx, &Edge{OrgID: orgID, FromCapability: up.ID, FromPort: "fix", ToCapability: down1.ID}); err != nil {
		t.Fatalf("edge 1: %v", err)
	}
	g1, err := db.EnsureGraphVersion(ctx, orgID)
	if err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	// 加一条边 → 图变了
	if err := db.CreateEdge(ctx, &Edge{OrgID: orgID, FromCapability: up.ID, FromPort: "clean", ToCapability: down2.ID}); err != nil {
		t.Fatalf("edge 2: %v", err)
	}
	g2, err := db.EnsureGraphVersion(ctx, orgID)
	if err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if g1.ID == g2.ID {
		t.Fatal("graph did not change after adding an edge")
	}
	// 删回原状 → hash 回到 g1 的（同一份图只有一行，复用旧版本）
	if err := db.DeleteEdge(ctx, orgID, up.ID, "clean", down2.ID); err != nil {
		t.Fatalf("delete edge: %v", err)
	}
	g3, err := db.EnsureGraphVersion(ctx, orgID)
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
	orgID, _ := newTestOrg(t, db)
	g1, err := db.EnsureGraphVersion(ctx, orgID)
	if err != nil {
		t.Fatalf("ensure empty: %v", err)
	}
	if g1.ContentHash == "" || g1.ID == "" {
		t.Fatalf("empty graph version incomplete: %+v", g1)
	}
	g2, err := db.EnsureGraphVersion(ctx, orgID)
	if err != nil {
		t.Fatalf("ensure empty twice: %v", err)
	}
	if g1.ID != g2.ID {
		t.Fatal("empty graph produced two versions")
	}
	if _, err := db.GetGraphVersion(ctx, orgID, g1.ID); err != nil {
		t.Fatalf("get graph version: %v", err)
	}
	if _, err := db.GetGraphVersion(ctx, orgID, "grf_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGraphVersion missing err = %v, want ErrNotFound", err)
	}
}

// TestEdgeRejectsPortNotInOutputs 不变量 I2 / A5：出口封闭。
func TestEdgeRejectsPortNotInOutputs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	up := mkCap(orgID, "review", 1, []string{"fix"})
	down := mkCap(orgID, "fix", 1, nil)
	for _, c := range []*CapabilityVersion{up, down} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	err := db.CreateEdge(ctx, &Edge{OrgID: orgID, FromCapability: up.ID, FromPort: "not-an-output", ToCapability: down.ID})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("edge with undefined port: err = %v, want ErrEdgeRejected", err)
	}
}

// TestEdgeRejectsCrossOrg 不放跨组织边（契约 §1.2）。
func TestEdgeRejectsCrossOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgA, _ := newTestOrg(t, db)
	orgB, _ := newTestOrg(t, db)

	up := mkCap(orgA, "review", 1, []string{"fix"})
	foreign := mkCap(orgB, "fix", 1, nil)
	for _, c := range []*CapabilityVersion{up, foreign} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	err := db.CreateEdge(ctx, &Edge{OrgID: orgA, FromCapability: up.ID, FromPort: "fix", ToCapability: foreign.ID})
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("cross-org edge: err = %v, want ErrEdgeRejected", err)
	}
}

// TestEdgesFromLookup 派发第 ① 步的查表入口。
func TestEdgesFromLookup(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	up := mkCap(orgID, "review", 1, []string{"fix"})
	d1 := mkCap(orgID, "fix1", 1, nil)
	d2 := mkCap(orgID, "fix2", 1, nil)
	for _, c := range []*CapabilityVersion{up, d1, d2} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	// 扇出：一个 (cap, port) 两条边
	for _, to := range []*CapabilityVersion{d1, d2} {
		if err := db.CreateEdge(ctx, &Edge{OrgID: orgID, FromCapability: up.ID, FromPort: "fix", ToCapability: to.ID}); err != nil {
			t.Fatalf("create edge: %v", err)
		}
	}
	got, err := db.EdgesFrom(ctx, orgID, up.ID, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("EdgesFrom returned %d targets, want 2 (fanout)", len(got))
	}

	// 不存在的出口 → 空列表（不是 nil）
	none, err := db.EdgesFrom(ctx, orgID, up.ID, "clean")
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
	if err := db.CreateEdge(ctx, &Edge{OrgID: orgID, FromCapability: up.ID, FromPort: "fix", ToCapability: d1.ID}); err != nil {
		t.Fatalf("idempotent create edge: %v", err)
	}
	got, err = db.EdgesFrom(ctx, orgID, up.ID, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom after dup: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("duplicate edge created a row: got %d targets", len(got))
	}

	// 删除
	if err := db.DeleteEdge(ctx, orgID, up.ID, "fix", d1.ID); err != nil {
		t.Fatalf("delete edge: %v", err)
	}
	if err := db.DeleteEdge(ctx, orgID, up.ID, "fix", d1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing edge err = %v, want ErrNotFound", err)
	}
	got, err = db.EdgesFrom(ctx, orgID, up.ID, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom after delete: %v", err)
	}
	if len(got) != 1 || got[0] != d2.ID {
		t.Fatalf("after delete got %v, want [%s]", got, d2.ID)
	}
}

// TestListEdgesEmptyIsArray 不变量 I9。
func TestListEdgesEmptyIsArray(t *testing.T) {
	db := newTestDB(t)
	orgID, _ := newTestOrg(t, db)
	edges, err := db.ListEdges(context.Background(), orgID, "")
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
	orgID, _ := newTestOrg(t, db)

	// ① 往返：存什么读什么
	c := mkCap(orgID, "analyze-pr", 1, []string{"needs-fix"})
	c.DisplayName = "分析 PR"
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetCapability(ctx, orgID, "analyze-pr", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DisplayName != "分析 PR" {
		t.Fatalf("display name roundtrip: got %q want %q", got.DisplayName, "分析 PR")
	}

	// ② 显示名不参与 content_hash：同一份定义、不同显示名 → 同一行
	hashBefore := got.ContentHash
	c2 := mkCap(orgID, "analyze-pr", 1, []string{"needs-fix"})
	c2.DisplayName = "完全不同的名字"
	if err := db.CreateCapability(ctx, c2); err != nil {
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
	if err := db.SetCapabilityDisplayName(ctx, orgID, c.ID, "改过的名字"); err != nil {
		t.Fatalf("set display name: %v", err)
	}
	after, err := db.GetCapabilityByID(ctx, orgID, c.ID)
	if err != nil {
		t.Fatalf("get after rename: %v", err)
	}
	if after.DisplayName != "改过的名字" {
		t.Fatalf("rename did not stick: %q", after.DisplayName)
	}
	if after.ContentHash != hashBefore {
		t.Fatalf("rename changed content_hash: %q vs %q", after.ContentHash, hashBefore)
	}
	// 版本数没变（还是 1）
	list, err := db.ListCapabilities(ctx, orgID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("rename created a new version: %d rows", len(list))
	}

	// ④ 跨组织改不动（不变量 I10）
	otherOrg, _ := newTestOrg(t, db)
	if err := db.SetCapabilityDisplayName(ctx, otherOrg, c.ID, "偷改"); err == nil {
		t.Fatal("cross-org display name update should fail")
	}
}
