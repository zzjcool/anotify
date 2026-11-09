package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 能力领域 v4 的测试（capability.md §2/§3/§8）。
//
// 只测 store 的事实：foldKey 复用、显式同版不可变、next/displayName 原地更新
// （digest 不变、edges 重建、事件落库）、字段往返。执行/派发是 route 的事。

// TestCapabilityCreateAndGet 基本往返：存什么读什么。
func TestCapabilityCreateAndGet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	c := testCap(nsID, "review", 0, []string{"fixer"})
	c.Spec.Params = []byte(`{"type":"object","properties":{"diff":{"type":"string"}}}`)
	c.DisplayName = "代码审查"
	createCap(t, db, c)

	// 省略 version = 内容寻址：store 分配第一个版本。
	if c.Version != 1 || c.ID == "" || c.Digest == "" || c.FoldKey == "" {
		t.Fatalf("CreateCapability 没有补齐身份字段: %+v", c)
	}

	got, err := db.GetCapability(ctx, nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if got.ID != c.ID || got.Name != "review" || got.DisplayName != "代码审查" {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if len(got.Spec.Next) != 1 || got.Spec.Next[0] != "fixer" {
		t.Fatalf("next 往返 mismatch: %v", got.Spec.Next)
	}
	if got.Spec.Run.Entrypoint != "run.sh" {
		t.Fatalf("run 往返 mismatch: %+v", got.Spec.Run)
	}
	if got.ResourceVersion != 1 {
		t.Fatalf("resourceVersion = %d, want 1", got.ResourceVersion)
	}
	if got.Digest != c.Digest || got.FoldKey != c.FoldKey {
		t.Fatalf("digest/foldKey 往返 mismatch: %+v", got)
	}
}

// TestCapabilityIdempotentCreate 同内容（省略 version）不产生第二行。
func TestCapabilityIdempotentCreate(t *testing.T) {
	db := openTestDB(t)
	nsID := newNS(t, db, "main")

	first := createCap(t, db, testCap(nsID, "review", 0, nil))
	second := createCap(t, db, testCap(nsID, "review", 0, nil))

	if second.ID != first.ID || second.Version != first.Version || second.Digest != first.Digest {
		t.Fatalf("同内容被建成了第二行: first=%+v second=%+v", first, second)
	}
	if n := countCapRows(t, db, nsID, "review"); n != 1 {
		t.Fatalf("capability_versions 行数 = %d, want 1", n)
	}
}

// TestCapabilitySameVersionDifferentContentRejected 显式同版内容不可变。
func TestCapabilitySameVersionDifferentContentRejected(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	createCap(t, db, testCap(nsID, "review", 1, nil))

	// 同 (ns,name,version)、执行正文（run.files）不同 → version_content_immutable。
	dup := testCap(nsID, "review", 1, nil)
	dup.Spec.Run.Files = map[string]string{"run.sh": "#!/bin/sh\necho changed\n"}
	err := db.CreateCapability(ctx, dup)
	if !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("同版换内容应返回 ErrVersionConflict，实际 %v", err)
	}
	var conflict *store.VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("错误应是 *VersionConflictError，实际 %T", err)
	}
	if conflict.Code() != store.CodeVersionContentImmutable {
		t.Fatalf("稳定码 = %q，want %q", conflict.Code(), store.CodeVersionContentImmutable)
	}
	if store.CodeVersionContentImmutable != "version_content_immutable" {
		t.Fatalf("version_content_immutable 码漂了: %q", store.CodeVersionContentImmutable)
	}
	if n := countCapRows(t, db, nsID, "review"); n != 1 {
		t.Fatalf("冲突后行数 = %d, want 1", n)
	}
}

// TestCapabilityExplicitSameContentReuses 显式 version 但内容相同 → 复用已有行。
func TestCapabilityExplicitSameContentReuses(t *testing.T) {
	db := openTestDB(t)
	nsID := newNS(t, db, "main")

	first := createCap(t, db, testCap(nsID, "review", 2, nil))
	again := createCap(t, db, testCap(nsID, "review", 2, nil))

	if again.ID != first.ID || again.Digest != first.Digest {
		t.Fatalf("同版同内容没有复用: first=%+v again=%+v", first, again)
	}
	if n := countCapRows(t, db, nsID, "review"); n != 1 {
		t.Fatalf("行数 = %d, want 1", n)
	}
}

// TestCapabilityFoldKeyIgnoresNextAndDisplayName 改 next / displayName 不产生新版本。
//
// 一次请求同时改两者：同一行原地更新，digest 不变；edges 整体重建；事件落一条。
func TestCapabilityFoldKeyIgnoresNextAndDisplayName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	first := createCap(t, db, testCap(nsID, "review", 0, []string{"fixer"}))
	before := *first

	update := testCap(nsID, "review", 0, []string{"notify@2"})
	update.DisplayName = "新名字"
	createCap(t, db, update)

	if update.ID != before.ID || update.Version != before.Version {
		t.Fatalf("改 next 产生了新版本: before=%+v update=%+v", before, update)
	}
	if update.Digest != before.Digest {
		t.Fatalf("改 next 改了 digest: %q → %q", before.Digest, update.Digest)
	}
	if update.DisplayName != "新名字" {
		t.Fatalf("displayName 没有原地更新: %q", update.DisplayName)
	}
	got, err := db.GetCapability(ctx, nsID, "review", before.Version)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if len(got.Spec.Next) != 1 || got.Spec.Next[0] != "notify@2" {
		t.Fatalf("next 没有原地更新: %v", got.Spec.Next)
	}
	if n := countCapRows(t, db, nsID, "review"); n != 1 {
		t.Fatalf("行数 = %d, want 1", n)
	}

	// edges 是该能力 next 的投影（整体替换，不留旧边）。
	rows, err := db.Query(
		`SELECT to_ref FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ?`,
		nsID, "review")
	if err != nil {
		t.Fatalf("query edges: %v", err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		refs = append(refs, ref)
	}
	if len(refs) != 1 || refs[0] != "notify@2" {
		t.Fatalf("edges = %v, want [notify@2]", refs)
	}

	// 事件：capability.routing_changed，payload 带旧/新 next。
	var payload string
	if err := db.QueryRow(
		`SELECT payload FROM events
		  WHERE namespace_id = ? AND type = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		nsID, store.EventTypeCapabilityRoutingChanged).Scan(&payload); err != nil {
		t.Fatalf("routing event 没有落库: %v", err)
	}
	var doc struct {
		From []string `json:"from"`
		To   []string `json:"to"`
	}
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		t.Fatalf("decode routing payload: %v", err)
	}
	if len(doc.From) != 1 || doc.From[0] != "fixer" || len(doc.To) != 1 || doc.To[0] != "notify@2" {
		t.Fatalf("routing payload = %s", payload)
	}
}

// TestCapabilitySchemaKeyOrderInsensitive 同一份 params 的书写顺序不影响身份。
func TestCapabilitySchemaKeyOrderInsensitive(t *testing.T) {
	db := openTestDB(t)
	nsID := newNS(t, db, "main")

	a := testCap(nsID, "review", 0, nil)
	a.Spec.Params = []byte(`{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"number"}}}`)
	createCap(t, db, a)

	b := testCap(nsID, "review", 0, nil)
	b.Spec.Params = []byte(`{"properties":{"a":{"type":"number"},"b":{"type":"string"}},"type":"object"}`)
	createCap(t, db, b)

	if b.Digest != a.Digest || b.FoldKey != a.FoldKey {
		t.Fatalf("key 顺序改变了身份: a=%+v b=%+v", a, b)
	}
	if n := countCapRows(t, db, nsID, "review"); n != 1 {
		t.Fatalf("行数 = %d, want 1", n)
	}
}

// TestCapabilityVersionAppendOnly 不同内容 + 省略 version → 追加新版本。
func TestCapabilityVersionAppendOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	v1 := createCap(t, db, testCap(nsID, "review", 0, nil))

	v2 := testCap(nsID, "review", 0, nil)
	v2.Spec.Run.Files = map[string]string{"run.sh": "#!/bin/sh\necho v2\n"}
	createCap(t, db, v2)
	if v2.Version != v1.Version+1 {
		t.Fatalf("新内容版本 = %d, want %d", v2.Version, v1.Version+1)
	}
	if v2.Digest == v1.Digest {
		t.Fatalf("不同内容算出了同一个 digest")
	}
	if v2.FoldKey == v1.FoldKey {
		t.Fatalf("不同内容算出了同一个 foldKey")
	}
	if n := countCapRows(t, db, nsID, "review"); n != 2 {
		t.Fatalf("行数 = %d, want 2", n)
	}

	latest, err := db.GetCapabilityLatest(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest.Version != v2.Version {
		t.Fatalf("latest = %d, want %d", latest.Version, v2.Version)
	}
}

// TestCapabilityNamespaceIsolation 同一份定义在不同命名空间是两行。
func TestCapabilityNamespaceIsolation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsA := newNS(t, db, "main")
	nsB := newNS(t, db, "team-b")

	a := createCap(t, db, testCap(nsA, "review", 0, nil))
	b := createCap(t, db, testCap(nsB, "review", 0, nil))
	if a.ID == b.ID {
		t.Fatalf("跨命名空间复用了同一行: %+v", a)
	}
	if n := countCapRows(t, db, nsA, "review"); n != 1 {
		t.Fatalf("nsA 行数 = %d", n)
	}
	if n := countCapRows(t, db, nsB, "review"); n != 1 {
		t.Fatalf("nsB 行数 = %d", n)
	}
	// 跨命名空间按 id 读不到（隔离）。
	if _, err := db.GetCapabilityByID(ctx, nsA, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("跨命名空间按 id 读取应返回 ErrNotFound，实际 %v", err)
	}
}

// TestCapabilityEmptyListIsArray 空 next / requires 读出来是 [] 而不是 null。
func TestCapabilityEmptyListIsArray(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	createCap(t, db, testCap(nsID, "terminal", 0, nil))
	got, err := db.GetCapability(ctx, nsID, "terminal", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if got.Spec.Next == nil {
		t.Fatal("next 是 nil，应该是空列表 []")
	}
	if got.Spec.Requires.Commands == nil || got.Spec.Requires.Values == nil {
		t.Fatalf("requires 列表是 nil: %+v", got.Spec.Requires)
	}
	list, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if len(list) != 1 || list[0].Spec.Next == nil {
		t.Fatalf("list = %+v", list)
	}
}

// TestCapabilityValidation 正文不合法 → ErrCapabilityInvalid（客户端可修的错）。
func TestCapabilityValidation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	// 没有 run（v4 唯一实现形式是脚本）。
	noRun := &store.CapabilityVersion{NamespaceID: nsID, Name: "broken"}
	if err := db.CreateCapability(ctx, noRun); !errors.Is(err, store.ErrCapabilityInvalid) {
		t.Fatalf("缺 run 应返回 ErrCapabilityInvalid，实际 %v", err)
	}

	// next 元素不合法。
	badNext := testCap(nsID, "broken", 0, []string{"Bad Name!"})
	if err := db.CreateCapability(ctx, badNext); !errors.Is(err, store.ErrCapabilityInvalid) {
		t.Fatalf("非法 next 应返回 ErrCapabilityInvalid，实际 %v", err)
	}

	// entrypoint 不以 #! 开头。
	badEntry := testCap(nsID, "broken", 0, nil)
	badEntry.Spec.Run.Files = map[string]string{"run.sh": "echo no shebang\n"}
	if err := db.CreateCapability(ctx, badEntry); !errors.Is(err, store.ErrCapabilityInvalid) {
		t.Fatalf("坏 entrypoint 应返回 ErrCapabilityInvalid，实际 %v", err)
	}
	if n := countCapRows(t, db, nsID, "broken"); n != 0 {
		t.Fatalf("非法能力被写进了库: %d 行", n)
	}
}

// TestCapabilityDisplayName displayName 不出版本、不改 digest。
func TestCapabilityDisplayName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	c := createCap(t, db, testCap(nsID, "review", 0, nil))
	if err := db.SetCapabilityDisplayName(ctx, nsID, c.ID, "代码审查"); err != nil {
		t.Fatalf("set display name: %v", err)
	}
	got, err := db.GetCapability(ctx, nsID, "review", c.Version)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if got.DisplayName != "代码审查" {
		t.Fatalf("displayName = %q", got.DisplayName)
	}
	if got.Digest != c.Digest {
		t.Fatalf("改 displayName 改了 digest: %q → %q", c.Digest, got.Digest)
	}
	if got.Version != c.Version {
		t.Fatalf("改 displayName 产生了新版本: %d → %d", c.Version, got.Version)
	}
	if got.ResourceVersion != c.ResourceVersion+1 {
		t.Fatalf("resourceVersion = %d, want %d", got.ResourceVersion, c.ResourceVersion+1)
	}
}

// TestCapabilityNormalizeIncludesName 名字参与身份：不同名字 → 不同 digest/foldKey。
func TestCapabilityNormalizeIncludesName(t *testing.T) {
	db := openTestDB(t)
	nsID := newNS(t, db, "main")

	a := createCap(t, db, testCap(nsID, "alpha", 0, nil))
	b := createCap(t, db, testCap(nsID, "beta", 0, nil))
	if a.FoldKey == b.FoldKey || a.Digest == b.Digest {
		t.Fatalf("不同名字算出了同一身份: %+v %+v", a, b)
	}
}

// TestCapabilityDigestIncludesVersion foldKey 不含 version，digest 含 version。
func TestCapabilityDigestIncludesVersion(t *testing.T) {
	db := openTestDB(t)
	nsID := newNS(t, db, "main")

	v1 := createCap(t, db, testCap(nsID, "review", 1, nil))
	v2 := createCap(t, db, testCap(nsID, "review", 2, nil))
	if v1.FoldKey != v2.FoldKey {
		t.Fatalf("同内容的 foldKey 应该相同: %q vs %q", v1.FoldKey, v2.FoldKey)
	}
	if v1.Digest == v2.Digest {
		t.Fatalf("digest 必须含 version：%q", v1.Digest)
	}
}

// TestCapabilityInstalledFromRoundTrip 安装来源往返。
func TestCapabilityInstalledFromRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	c := testCap(nsID, "review", 0, nil)
	c.InstalledFrom = "ins_abc"
	createCap(t, db, c)

	got, err := db.GetCapability(ctx, nsID, "review", c.Version)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if got.InstalledFrom != "ins_abc" {
		t.Fatalf("installedFrom = %q, want ins_abc", got.InstalledFrom)
	}
}

// TestCapabilityCreateProjectsEdges 建能力时 next 同事务投影成 edges。
func TestCapabilityCreateProjectsEdges(t *testing.T) {
	db := openTestDB(t)
	nsID := newNS(t, db, "main")

	createCap(t, db, testCap(nsID, "review", 0, []string{"fixer", "notify@2"}))

	rows, err := db.Query(
		`SELECT to_ref FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ?
		  ORDER BY to_ref`, nsID, "review")
	if err != nil {
		t.Fatalf("query edges: %v", err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		refs = append(refs, ref)
	}
	if len(refs) != 2 || refs[0] != "fixer" || refs[1] != "notify@2" {
		t.Fatalf("edges = %v, want [fixer notify@2]", refs)
	}
	// 终端能力（next=[]）不产生边。
	createCap(t, db, testCap(nsID, "terminal", 0, nil))
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM edges WHERE namespace_id = ? AND from_ref = ?`, nsID, "terminal").Scan(&n); err != nil {
		t.Fatalf("count terminal edges: %v", err)
	}
	if n != 0 {
		t.Fatalf("终端能力产生了 %d 条边", n)
	}
}

// TestCapabilityUpdateNext 显式改线：digest 不变、edges 重建、事件落库、版本不变。
func TestCapabilityUpdateNext(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	c := createCap(t, db, testCap(nsID, "review", 0, []string{"fixer"}))
	updated, err := db.UpdateNext(ctx, nsID, "review", 0, []string{"fixer", "notify@2"})
	if err != nil {
		t.Fatalf("UpdateNext: %v", err)
	}
	if updated.Digest != c.Digest {
		t.Fatalf("改线改了 digest: %q → %q", c.Digest, updated.Digest)
	}
	if updated.Version != c.Version {
		t.Fatalf("改线产生了新版本: %d → %d", c.Version, updated.Version)
	}
	if len(updated.Spec.Next) != 2 {
		t.Fatalf("next = %v", updated.Spec.Next)
	}

	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ?`,
		nsID, "review").Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 2 {
		t.Fatalf("edges 行数 = %d, want 2", n)
	}

	// version 钉死也支持。
	if _, err := db.UpdateNext(ctx, nsID, "review", c.Version, []string{}); err != nil {
		t.Fatalf("UpdateNext (pinned version): %v", err)
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM edges WHERE namespace_id = ? AND from_ref = ?`,
		nsID, "review").Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 0 {
		t.Fatalf("清空 next 后 edges 行数 = %d", n)
	}

	// 未注册的能力 → ErrNotFound。
	if _, err := db.UpdateNext(ctx, nsID, "ghost", 0, []string{"x"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateNext 未知能力应返回 ErrNotFound，实际 %v", err)
	}
}

// TestCapabilityNextCapabilityVersion 下一个版本号的只读查询。
func TestCapabilityNextCapabilityVersion(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	if n, err := db.NextCapabilityVersion(ctx, nsID, "review"); err != nil || n != 1 {
		t.Fatalf("空库 next = %d, %v", n, err)
	}
	createCap(t, db, testCap(nsID, "review", 0, nil))
	if n, err := db.NextCapabilityVersion(ctx, nsID, "review"); err != nil || n != 2 {
		t.Fatalf("有 v1 后 next = %d, %v", n, err)
	}
}

// TestCapabilityCapabilityNamespace 能力归属查询（隔离校验共用）。
func TestCapabilityCapabilityNamespace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	c := createCap(t, db, testCap(nsID, "review", 0, nil))
	got, err := db.CapabilityNamespace(ctx, c.ID)
	if err != nil || got != nsID {
		t.Fatalf("CapabilityNamespace = %q, %v", got, err)
	}
	if _, err := db.CapabilityNamespace(ctx, "cap_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知 id 应返回 ErrNotFound，实际 %v", err)
	}
}
