package store

// capability_test.go 覆盖 v4 能力行（capability.md §2/§3）：
//
//   - 正文是 artifact.Spec（脚本），落库时由 store 归一化并算 foldKey/digest；
//   - 显式同版内容不可变（ErrVersionConflict），省略 version = 内容寻址复用/追加；
//   - displayName / next 不进 digest（改它们不产生新版本），改 run.files 一定产生。
//
// v3 的 outputs / paramsSchema / sticky / handler / graph-version / edge-port 断言
// 随这些字段一起退役（映射见 TEST-MAPPING.md）：v4 没有端口与中枢执行器，
// 路由是 spec.next，边是派生投影。边投影本身由 v4test/edges_test.go 覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// mkCap 造一个未落库的能力行：正文是真实脚本（run.sh），不是 handler 镜像。
//
// next 是 v4 的路由声明（v3 的 outputs 端口已删除，同位置参数保留成 next）。
func mkCap(nsID, name string, version int, next []string) *CapabilityVersion {
	return &CapabilityVersion{
		NamespaceID: nsID,
		Name:        name,
		Version:     version,
		Spec:        v4Spec("", next, ""),
	}
}

// TestCapabilityCreateAndGet 基本往返：存什么读什么。
func TestCapabilityCreateAndGet(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := mkCap(nsID, "review", 1, []string{"fix", "clean"})
	c.DisplayName = "代码审阅"
	c.Spec.Params = json.RawMessage(`{"type":"object","properties":{"diff":{"type":"string"}}}`)
	c.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho review\n"
	maxAttempts := 2
	c.Spec.Lifecycle.MaxAttempts = &maxAttempts
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	if c.ID == "" || c.Digest == "" || c.FoldKey == "" || c.ResourceVersion == 0 {
		t.Fatalf("CreateCapability 未回填身份字段: %+v", c)
	}

	got, err := db.GetCapability(ctx, nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if got.ID != c.ID || got.Name != "review" || got.Version != 1 {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if got.Digest != c.Digest || got.FoldKey != c.FoldKey {
		t.Fatalf("内容身份不一致: digest %q/%q fold %q/%q", got.Digest, c.Digest, got.FoldKey, c.FoldKey)
	}
	if got.DisplayName != "代码审阅" {
		t.Fatalf("displayName 往返丢失: %q", got.DisplayName)
	}
	if len(got.Spec.Next) != 2 || got.Spec.Next[0] != "fix" || got.Spec.Next[1] != "clean" {
		t.Fatalf("next 往返不一致: %v", got.Spec.Next)
	}
	if got.Spec.Run.Files["run.sh"] != "#!/bin/sh\necho review\n" {
		t.Fatalf("run.files 往返丢失: %v", got.Spec.Run.Files)
	}
	if got.Spec.Lifecycle.EffectiveMaxAttempts() != 2 {
		t.Fatalf("lifecycle 往返不一致: %+v", got.Spec.Lifecycle)
	}

	// by id：正文是同一份（digest 是内容身份，行 id 只是物理键）。
	byID, err := db.GetCapabilityByID(ctx, nsID, c.ID)
	if err != nil {
		t.Fatalf("get capability by id: %v", err)
	}
	if byID.Digest != got.Digest {
		t.Fatalf("by-id digest mismatch: %q vs %q", byID.Digest, got.Digest)
	}
}

// TestCapabilityIdempotentCreate 同 name@version 同内容重复提交 → 折叠到同一行；
// 换版本号是另一份内容（digest 吃 metadata.version）。
func TestCapabilityIdempotentCreate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	first := mkCap(nsID, "review", 1, nil)
	if err := db.CreateCapability(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	again := mkCap(nsID, "review", 1, nil)
	if err := db.CreateCapability(ctx, again); err != nil {
		t.Fatalf("second create (same content): %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("同内容重复提交没有折叠到已有行: %s vs %s", first.ID, again.ID)
	}

	next := mkCap(nsID, "review", 2, nil)
	if err := db.CreateCapability(ctx, next); err != nil {
		t.Fatalf("create review@2: %v", err)
	}
	if next.ID == first.ID || next.Version != 2 {
		t.Fatalf("review@2 不该折叠成 review@1（got %s@%d）", next.Name, next.Version)
	}
}

// TestCapabilitySameVersionDifferentContentRejected 同 name@version 改内容必须拒。
func TestCapabilitySameVersionDifferentContentRejected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if err := db.CreateCapability(ctx, mkCap(nsID, "review", 1, nil)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	changed := mkCap(nsID, "review", 1, nil)
	changed.Spec.Run.Files["run.sh"] = "#!/bin/sh\nexit 1\n"
	err := db.CreateCapability(ctx, changed)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("same name@version 换内容应报 ErrVersionConflict，得到 %v", err)
	}
}

// TestCapabilitySchemaKeyOrderInsensitive params 的键顺序不参与 digest。
func TestCapabilitySchemaKeyOrderInsensitive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	a := mkCap(nsID, "same", 1, nil)
	a.Spec.Params = json.RawMessage(`{"type":"object","required":["a"]}`)
	if err := db.CreateCapability(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	b := mkCap(nsID, "same", 1, nil)
	b.Spec.Params = json.RawMessage(`{ "required": ["a"], "type": "object" }`)
	if err := db.CreateCapability(ctx, b); err != nil {
		t.Fatalf("create b (同内容不同排版): %v", err)
	}
	if a.Digest != b.Digest {
		t.Fatalf("params 键序不同算出两个 digest: %s vs %s", a.Digest, b.Digest)
	}
}

// TestCapabilityDifferentContentDifferentHash 换脚本正文 = 换内容身份。
func TestCapabilityDifferentContentDifferentHash(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	a := mkCap(nsID, "same", 1, nil)
	if err := db.CreateCapability(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	b := mkCap(nsID, "same", 2, nil)
	b.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho different\n"
	if err := db.CreateCapability(ctx, b); err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.Digest == b.Digest || a.FoldKey == b.FoldKey {
		t.Fatalf("正文不同却算出同一身份: digest %q vs %q", a.Digest, b.Digest)
	}
}

// TestCapabilityNormalizeIncludesName 同样的正文、不同能力名 → 不同 foldKey。
func TestCapabilityNormalizeIncludesName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	a := mkCap(nsID, "alpha", 1, nil)
	b := mkCap(nsID, "beta", 1, nil)
	if err := db.CreateCapability(ctx, a); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if err := db.CreateCapability(ctx, b); err != nil {
		t.Fatalf("create beta: %v", err)
	}
	if a.FoldKey == b.FoldKey {
		t.Fatalf("不同能力名算出同一 foldKey（名字没进内容身份）: %q", a.FoldKey)
	}
}

// TestCapabilityVersionAppendOnly 版本只追加：内容寻址分配下一个版本，显式版本可跳号。
func TestCapabilityVersionAppendOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	v1 := mkCap(nsID, "review", 0, nil)
	if err := db.CreateCapability(ctx, v1); err != nil {
		t.Fatalf("create content-addressed v1: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("内容寻址首版应为 1，得到 %d", v1.Version)
	}
	next, err := db.NextCapabilityVersion(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("next version: %v", err)
	}
	if next != 2 {
		t.Fatalf("NextCapabilityVersion = %d, want 2", next)
	}
	v2 := mkCap(nsID, "review", 0, nil)
	v2.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho v2\n"
	if err := db.CreateCapability(ctx, v2); err != nil {
		t.Fatalf("create content-addressed v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("第二个内容版本应为 2，得到 %d", v2.Version)
	}
	latest, err := db.GetCapabilityLatest(ctx, nsID, "review")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.Version != 2 || latest.ID != v2.ID {
		t.Fatalf("latest 指向 %s@%d, want %s@2", latest.ID, latest.Version, v2.ID)
	}
}

// TestCapabilityNamespaceIsolation 同名同内容在两个命名空间是两行，列表互不可见。
func TestCapabilityNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB := newTestExtraNS(t, db, "other-ns")

	a := mkCap(nsA, "review", 1, nil)
	if err := db.CreateCapability(ctx, a); err != nil {
		t.Fatalf("create in A: %v", err)
	}
	b := mkCap(nsB, "review", 1, nil)
	if err := db.CreateCapability(ctx, b); err != nil {
		t.Fatalf("create in B: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("跨命名空间同名能力共用了行 id %s", a.ID)
	}
	listA, err := db.ListCapabilities(ctx, nsA)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != a.ID {
		t.Fatalf("A 的列表 = %+v, want 只有 %s", listA, a.ID)
	}
	if _, err := db.GetCapability(ctx, nsB, "review", 1); err != nil {
		t.Fatalf("B 里读不到自己的 review: %v", err)
	}
}

// TestCapabilityEmptyListIsArray 空列表必须是 []（前端据此判「没有能力」，不是「没连上」）。
func TestCapabilityEmptyListIsArray(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	list, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if list == nil {
		t.Fatal("空能力列表返回 nil，want 非 nil 空切片（JSON 才是 []）")
	}
	if len(list) != 0 {
		t.Fatalf("新命名空间应有 0 个能力，得到 %d", len(list))
	}
}

// TestCapabilityValidation 正文非法（entrypoint 不在 files / 缺 shebang）必须拒。
func TestCapabilityValidation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	cases := []struct {
		name string
		spec artifact.Spec
	}{
		{"entrypoint-missing", artifact.Spec{
			Params: json.RawMessage(`{}`),
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"other.sh": "#!/bin/sh\n"}},
		}},
		{"no-shebang", artifact.Spec{
			Params: json.RawMessage(`{}`),
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "echo hi\n"}},
		}},
		{"params-invalid", artifact.Spec{
			Params: json.RawMessage(`"not-an-object"`),
			Run:    artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\n"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &CapabilityVersion{NamespaceID: nsID, Name: "bad-" + tc.name, Version: 1, Spec: tc.spec}
			if err := db.CreateCapability(ctx, c); err == nil {
				t.Fatalf("非法正文被接受: %+v", tc.spec)
			}
		})
	}
	// 合法的最小正文必须能落库，证明上面的拒绝不是「全都拒」。
	if err := db.CreateCapability(ctx, mkCap(nsID, "ok", 1, nil)); err != nil {
		t.Fatalf("最小合法正文被拒: %v", err)
	}
}

// TestLifecycleIdempotentDefault 省略 = 显式默认（归一化在落库前完成）。
func TestLifecycleIdempotentDefault(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := mkCap(nsID, "defaults", 1, nil)
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetCapability(ctx, nsID, "defaults", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Lifecycle.Idempotent {
		t.Fatalf("省略 idempotent 应为 false，得到 true")
	}
	if got.Spec.Lifecycle.EffectiveMaxAttempts() != artifact.DefaultMaxAttempts {
		t.Fatalf("省略 maxAttempts 应取默认 %d，得到 %d",
			artifact.DefaultMaxAttempts, got.Spec.Lifecycle.EffectiveMaxAttempts())
	}
	if got.Spec.Lifecycle.EffectiveResultTTLSec() != artifact.DefaultResultTTLSec {
		t.Fatalf("省略 resultTtlSec 应取默认 %d，得到 %d",
			artifact.DefaultResultTTLSec, got.Spec.Lifecycle.EffectiveResultTTLSec())
	}
	if got.Spec.Concurrency.EffectiveMaxPerNode() != artifact.DefaultMaxPerNode {
		t.Fatalf("省略 maxPerNode 应取默认 %d，得到 %d",
			artifact.DefaultMaxPerNode, got.Spec.Concurrency.EffectiveMaxPerNode())
	}
}

// TestCapabilityDisplayName displayName 不进 digest：改它不产生新版本，走 CAS 更新。
func TestCapabilityDisplayName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := mkCap(nsID, "review", 1, nil)
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := c.Digest
	// 同内容 + 新 displayName 再提交一次：内容身份不变，displayName 不被隐式改写。
	again := mkCap(nsID, "review", 1, nil)
	again.DisplayName = "新名字"
	if err := db.CreateCapability(ctx, again); err != nil {
		t.Fatalf("re-create with displayName: %v", err)
	}
	if again.Digest != before {
		t.Fatalf("displayName 影响了 digest: %s vs %s", before, again.Digest)
	}
	if again.ID != c.ID {
		t.Fatalf("同内容重复提交没折叠: %s vs %s", again.ID, c.ID)
	}
	stored, err := db.GetCapability(ctx, nsID, "review", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.DisplayName != "" {
		t.Fatalf("create-only 幂等不得隐式改 displayName，得到 %q", stored.DisplayName)
	}
	if err := db.SetCapabilityDisplayName(ctx, nsID, c.ID, "新名字", stored.ResourceVersion); err != nil {
		t.Fatalf("set display name: %v", err)
	}
	after, err := db.GetCapability(ctx, nsID, "review", 1)
	if err != nil {
		t.Fatalf("get after rename: %v", err)
	}
	if after.DisplayName != "新名字" {
		t.Fatalf("displayName = %q, want 新名字", after.DisplayName)
	}
	if after.Digest != before {
		t.Fatalf("改 displayName 改了 digest: %s -> %s", before, after.Digest)
	}
	// 旧 RV 的 CAS 必须冲突。
	if err := db.SetCapabilityDisplayName(ctx, nsID, c.ID, "又改", stored.ResourceVersion); !errors.Is(err, ErrResourceVersionConflict) {
		t.Fatalf("旧 resourceVersion 应冲突，得到 %v", err)
	}
}

// TestCapabilityDigestMatchesArtifactContent digest 是正文规范字节的摘要
// （v3 的 content_hash ≡ digest 口径在 v4 由 artifact.Digest 唯一实现）。
func TestCapabilityDigestMatchesArtifactContent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := mkCap(nsID, "review", 1, nil)
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	art := c.Artifact()
	want, err := artifact.Digest(&art)
	if err != nil {
		t.Fatalf("artifact digest: %v", err)
	}
	if c.Digest != want {
		t.Fatalf("行 digest %q != 正文 digest %q", c.Digest, want)
	}
	fold, err := artifact.FoldKey(&art)
	if err != nil {
		t.Fatalf("artifact fold key: %v", err)
	}
	if c.FoldKey != fold {
		t.Fatalf("行 foldKey %q != 正文 foldKey %q", c.FoldKey, fold)
	}
}

// TestCapabilitySameSpecExplicitNewVersionCreatesNewRow 父裁定 #3（B 轮）：
// 同一份正文 + **显式新版本号** = 合法的新行（新 UID、两行并存）。
//
// 两个输入都是「新对象、ID 为空」，只有 version 不同——这正是 v3
// `TestCapabilityVersionAppendOnly` 的原意，v4 用真实输入/DDL/错误区分：
//   - 同版不同内容 → ErrVersionConflict（TestCapabilitySameVersionDifferentContentRejected）；
//   - 同版同内容 → 复用（TestCapabilityIdempotentCreate）；
//   - 新版同内容 → 建新行（本用例）。
//
// fold_key 是普通索引、不是唯一键：两行共享同一 foldKey 是契约内的事实
// （deployment digest 因含 version 而不同；foldKey 不含 version）。
func TestCapabilitySameSpecExplicitNewVersionCreatesNewRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	spec := v4Spec(`{"type":"object"}`, []string{"notify@1"}, "#!/bin/sh\nexit 0\n")
	v1 := &CapabilityVersion{NamespaceID: nsID, Name: "same-spec", Version: 1, Spec: spec}
	if v1.ID != "" {
		t.Fatalf("前置：新对象 ID 应为空，得到 %q", v1.ID)
	}
	if err := db.CreateCapability(ctx, v1); err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if v1.ID == "" {
		t.Fatal("create v1 后没有分配 ID")
	}
	v2 := &CapabilityVersion{NamespaceID: nsID, Name: "same-spec", Version: 2, Spec: spec}
	if err := db.CreateCapability(ctx, v2); err != nil {
		t.Fatalf("同正文显式 v2 应允许建新行，得到 %v", err)
	}
	if v2.ID == "" || v2.ID == v1.ID {
		t.Fatalf("v2 应有新 UID：v1=%q v2=%q", v1.ID, v2.ID)
	}
	got1, err := db.GetCapability(ctx, nsID, "same-spec", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	got2, err := db.GetCapability(ctx, nsID, "same-spec", 2)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if got1.ID != v1.ID || got2.ID != v2.ID {
		t.Fatalf("两行 UID 往返不一致: v1 %q/%q v2 %q/%q", got1.ID, v1.ID, got2.ID, v2.ID)
	}
	if got1.FoldKey == "" || got1.FoldKey != got2.FoldKey {
		t.Fatalf("同正文两行的 foldKey 应相同：v1=%q v2=%q", got1.FoldKey, got2.FoldKey)
	}
	if got1.Digest == got2.Digest {
		t.Fatalf("digest 含 version，v1/v2 不应相同：%q", got1.Digest)
	}
	all, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("应有 2 行（同 name 两个版本），得到 %d: %+v", len(all), all)
	}
}

// TestCapabilityCreateEdgeWriteFailureRollsBack 拒绝真实 edge INSERT 时，已写下的
// 能力正文/digest 与第一条边必须整体回滚。两个 target 都存在：这不是输入校验，
// 也不把 store 允许保存未解析 next 的行为假扮成 v3 的 ErrEdgeRejected guard。
func TestCapabilityCreateEdgeWriteFailureRollsBack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	v4Cap(t, db, nsID, "a-target", 1)
	v4Cap(t, db, nsID, "z-target", 1)
	if err := db.CreateCapability(ctx, mkCap(nsID, "seed", 1, []string{"a-target"})); err != nil {
		t.Fatal(err)
	}
	beforeCaps, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEdges, err := db.ListEdges(ctx, nsID)
	if err != nil {
		t.Fatal(err)
	}
	// sentinel 仅在新能力与第一条边已在当前事务内可见时返回，证明失败点在写入之后。
	mustExec(t, db, `CREATE TRIGGER reject_create_edge BEFORE INSERT ON edges
		WHEN NEW.from_ref = 'edge-failure' AND NEW.to_ref = 'z-target'
		BEGIN
			SELECT CASE WHEN
				EXISTS(SELECT 1 FROM capability_versions WHERE namespace_id=NEW.namespace_id AND name=NEW.from_ref)
				AND EXISTS(SELECT 1 FROM edges WHERE namespace_id=NEW.namespace_id AND from_ref=NEW.from_ref AND to_ref='a-target')
			THEN RAISE(ABORT, 'injected edge write failure after capability and first edge')
			ELSE RAISE(ABORT, 'failure fixture did not reach prior writes') END;
		END`)
	c := mkCap(nsID, "edge-failure", 1, []string{"a-target", "z-target"})
	err = db.CreateCapability(ctx, c)
	if err == nil || !strings.Contains(err.Error(), "insert capability edge") ||
		!strings.Contains(err.Error(), "injected edge write failure after capability and first edge") {
		t.Fatalf("expected actual edge INSERT failure after prior writes, got %v", err)
	}
	afterCaps, err := db.ListCapabilities(ctx, nsID)
	if err != nil {
		t.Fatal(err)
	}
	afterEdges, err := db.ListEdges(ctx, nsID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeCaps, afterCaps) || !reflect.DeepEqual(beforeEdges, afterEdges) {
		t.Fatalf("failed edge write left partial capability/edges: caps %v -> %v; edges %v -> %v", beforeCaps, afterCaps, beforeEdges, afterEdges)
	}
	if _, err := db.GetCapability(ctx, nsID, c.Name, c.Version); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed capability remained readable: %v", err)
	}
}

// TestCapabilityGhostNextIsPreservedAndResolvedMissing 未存在目标允许声明并落边；
// 缺失是 ResolveRef 的数据事实（ErrNotFound），不是 CREATE 错误或坏引用格式。
func TestCapabilityGhostNextIsPreservedAndResolvedMissing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	const missingRef = "ghost-target@7"
	c := mkCap(nsID, "ghost-next", 1, []string{missingRef})
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("legal ghost next CREATE: %v", err)
	}
	got, err := db.GetCapability(ctx, nsID, c.Name, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != c.ID || !reflect.DeepEqual(got.Spec.Next, []string{missingRef}) {
		t.Fatalf("ghost next declaration lost on roundtrip: %+v", got)
	}
	edges, err := db.EdgesFrom(ctx, nsID, false, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(edges, []EdgeTarget{{ToRef: missingRef}}) {
		t.Fatalf("ghost next edge ref not preserved: %+v", edges)
	}
	if target, err := db.ResolveRef(ctx, nsID, missingRef); target != nil || !errors.Is(err, ErrNotFound) || errors.Is(err, ErrRefUnresolved) {
		t.Fatalf("ghost next must resolve as missing data: target=%+v err=%v", target, err)
	}
	if owner, err := db.ResolveRef(ctx, nsID, c.Name); err != nil || owner == nil || owner.ID != c.ID {
		t.Fatalf("existing capability must still resolve: owner=%+v err=%v", owner, err)
	}
}
