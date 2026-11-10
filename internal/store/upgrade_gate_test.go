package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 升级闸门的验收（P2.2，设计 typed-pipeline §3.6）—— v4 迁移。
//
// v4 的契约面收窄成 **params（JSON Schema）**：
//   - 出口不再是契约：next 是**原地**可改的路由（UpdateNext / ReplaceCapabilityEdges），
//     不进版本、不改 digest，所以「删出口」不是破坏性变更，闸门也没有 removed_port 这一类。
//   - 剩下的破坏性变更只有两类：改入参类型（ChangeParamTypeChanged）、
//     新增必填入参（ChangeParamAddedRequired）。
//
// 闸门在 v4 是**预计算**接口：console 先拿 impact（前端要能先看到哪几条 link 会断）
// 再决定要不要提交。v3 的 `CreateCapabilityForce(force)` 逃生口随端口模型一起删除，
// 本文件因此只钉住 store 今天真实提供的事实（不再断言一个不存在的写入口）。
//
// 为什么闸门必须先于「跟随 latest」的引用语义——这条理由在 v4 里仍然成立：
// 把一个破坏性 params 版本推成 latest 等于**静默改线**，没人点过任何东西，
// 已有那条链却换了入参契约。

// gateFixture 造一个「review --> fixer」的图（review 的 next 是裸名字 fixer）。
//
// 关键点：`to_ref` 是**裸名字**（跟随 latest）——只有这种 link 会被闸门影响；
// 钉死的 `fixer@1` 不受任何升级影响（它钉的就是旧版）。
func gateFixture(t *testing.T) (db *DB, nsID string, fixer, review *CapabilityVersion) {
	t.Helper()
	ctx := context.Background()
	db = newTestDB(t)
	nsID, _ = newTestNS(t, db)

	// fixer 自己也有一条出线（fixer --> notify）：用来证明「改 next 只重建**这个能力**的边」。
	v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: "notify", Version: 1,
		Spec: v4Spec("", nil, ""),
	})
	fixer = v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 1,
		Spec: v4Spec(`{"type":"object","required":["issue"],"properties":{"issue":{"type":"string"}}}`, []string{"notify"}, ""),
	})
	// review 的 next 就是 v4 的「出线」：建行时投影成 edges（review --> fixer）。
	review = v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: "review", Version: 1,
		Spec: v4Spec("", []string{"fixer"}, ""),
	})
	edges, err := db.ListEdges(ctx, nsID)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	have := map[string]bool{}
	for _, e := range edges {
		have[e.FromRef+"-->"+e.ToRef] = true
	}
	if !have["review-->fixer"] || !have["fixer-->notify"] {
		t.Fatalf("夹具前置：edges = %+v, want review-->fixer 与 fixer-->notify", edges)
	}
	return db, nsID, fixer, review
}

// fixerParams 是 fixer 的入参契约（required issue: string）。
func fixerParams() string {
	return `{"type":"object","required":["issue"],"properties":{"issue":{"type":"string"}}}`
}

// TestUpgradeGateRemovedPort：删掉出口在 v4 **不是**破坏性变更（它是原地改线，不是升级）。
//
// v3 这条用例断言「删 port → 拒绝 + 列受影响 link + force 能越过」；v4 把出口从契约里
// 移走之后三件事都不再成立，改成钉住真正的新事实：
//   - CheckUpgrade 对「只少一个 next」的候选返回空影响面（不是 breaking）；
//   - UpdateNext 原地改线：版本号与 digest 不变，edges 投影跟着重建；
//   - 没有 force 逃生口（CreateCapabilityForce 已删除）——所以也不再断言它。
func TestUpgradeGateRemovedPort(t *testing.T) {
	db, nsID, fixer, _ := gateFixture(t)
	ctx := context.Background()

	// 候选版本：params 一模一样，只是不再有出口。
	candidate := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		Spec: v4Spec(fixerParams(), nil, ""),
	}
	impact, err := db.CheckUpgrade(ctx, nsID, candidate)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if impact.Breaking || len(impact.Changes) != 0 {
		t.Fatalf("删出口不该再算破坏性变更: %+v", impact)
	}
	if impact.Changes == nil || impact.Links == nil {
		t.Fatalf("空影响面也必须是 [] 而不是 null（I9）: %+v", impact)
	}

	// 真正删出口的路径是原地改线：不产生新版本、不改部署身份，只重建 edges。
	// name 路径是 CAS：必须带当前 resourceVersion 令牌
	token := ResourceVersion(fixer.ID, fixer.ResourceVersion)
	updated, err := db.UpdateNext(ctx, nsID, "fixer", 0, nil, token)
	if err != nil {
		t.Fatalf("update next: %v", err)
	}
	if updated.Version != fixer.Version {
		t.Fatalf("改 next 不该产生新版本：%d → %d", fixer.Version, updated.Version)
	}
	if updated.Digest != fixer.Digest {
		t.Fatalf("改 next 不该改 digest：%s → %s", fixer.Digest, updated.Digest)
	}
	if len(updated.Spec.Next) != 0 {
		t.Fatalf("next 应被清空，实际 %v", updated.Spec.Next)
	}
	if _, gerr := db.GetCapability(ctx, nsID, "fixer", 2); !errors.Is(gerr, ErrNotFound) {
		t.Fatalf("原地改线不该落出 fixer@2（err=%v）", gerr)
	}
	// 只重建 fixer 自己的出边：review --> fixer 是 **review 的** next 投影，必须原样留着。
	edges, eerr := db.ListEdges(ctx, nsID)
	if eerr != nil {
		t.Fatalf("list edges: %v", eerr)
	}
	for _, e := range edges {
		if e.FromRef == "fixer" {
			t.Fatalf("fixer 的出口清空后不该还有 %s --> %s", e.FromRef, e.ToRef)
		}
	}
	if len(edges) != 1 || edges[0].FromRef != "review" || edges[0].ToRef != "fixer" {
		t.Fatalf("清 fixer 出口误伤了别人的边: %+v", edges)
	}
}

// TestUpgradeGateListsAffectedLinks 受影响 link 必须按 `from_ref --> to_ref` 列出来。
//
// 判据落在「这条 link 会不会解析到新版」上：只有**裸名字**右端（跟随 latest）受影响，
// 钉死的 `fixer@1` 不受影响。
func TestUpgradeGateListsAffectedLinks(t *testing.T) {
	db, nsID, _, _ := gateFixture(t)
	ctx := context.Background()

	// 再挂一条钉死的边：cleanup --> fixer@1（两端都不跟随 latest）。
	v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: "cleanup", Version: 1,
		Spec: v4Spec("", []string{"fixer@1"}, ""),
	})

	// 候选：新增必填 note（破坏）——出口的增减与闸门无关，所以这里不放 next。
	candidate := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		Spec: v4Spec(`{"type":"object","required":["issue","note"],"properties":{
			"issue":{"type":"string"},"note":{"type":"string"}}}`, nil, ""),
	}
	impact, err := db.CheckUpgrade(ctx, nsID, candidate)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if !impact.Breaking || impact.FromVersion != 1 || impact.ToVersion != 2 {
		t.Fatalf("impact = %+v, want breaking (1 → 2)", impact)
	}
	if len(impact.Changes) != 1 || impact.Changes[0].Kind != ChangeParamAddedRequired {
		t.Fatalf("changes = %+v, want 一条 param_added_required", impact.Changes)
	}
	if len(impact.Links) != 1 {
		t.Fatalf("links = %+v, want 恰好 1 条（只有跟随 latest 的那条受影响）", impact.Links)
	}
	got := impact.Links[0]
	if got.FromRef != "review" || got.ToRef != "fixer" {
		t.Fatalf("link = %s, want `review --> fixer`", got)
	}
	if !strings.Contains(got.Reason, "跟随 latest") {
		t.Fatalf("link 的 reason 没说清为什么受影响: %q", got.Reason)
	}
	// 结构化拒绝的文案里带着这条 link（纯文本客户端也看得见）。
	breaking := &UpgradeBreakingError{Impact: impact}
	if !errors.Is(breaking, ErrUpgradeBreaking) {
		t.Fatalf("UpgradeBreakingError 必须能被 errors.Is 认出来")
	}
	if !strings.Contains(breaking.Error(), "review --> fixer") {
		t.Fatalf("错误文案里没有受影响 link: %v", breaking.Error())
	}
}

// TestUpgradeGateAllowsAdditiveChanges 加可选入参 / 加出口 → 直接通过（不需要任何逃生口）。
func TestUpgradeGateAllowsAdditiveChanges(t *testing.T) {
	db, nsID, _, _ := gateFixture(t)
	ctx := context.Background()

	candidate := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		Spec: v4Spec(`{"type":"object","required":["issue"],"properties":{
			"issue":{"type":"string"},"note":{"type":"string"}}}`, []string{"done"}, ""),
	}
	impact, err := db.CheckUpgrade(ctx, nsID, candidate)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if impact.Breaking || len(impact.Changes) != 0 {
		t.Fatalf("impact = %+v, want 空影响面（只增不减）", impact)
	}
	if err := db.CreateCapability(ctx, candidate); err != nil {
		t.Fatalf("加可选入参/加出口应该直接通过: %v", err)
	}
	if candidate.Version != 2 {
		t.Fatalf("落库版本 = %d, want 2", candidate.Version)
	}
}

// TestUpgradeGateParamTypeChanged 改入参类型 → 闸门报破坏，并指出右端跟随 latest 的 link。
func TestUpgradeGateParamTypeChanged(t *testing.T) {
	db, nsID, _, _ := gateFixture(t)
	ctx := context.Background()

	candidate := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		// issue: string → integer
		Spec: v4Spec(`{"type":"object","required":["issue"],"properties":{"issue":{"type":"integer"}}}`, nil, ""),
	}
	impact, err := db.CheckUpgrade(ctx, nsID, candidate)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if !impact.Breaking {
		t.Fatalf("改入参类型必须 breaking: %+v", impact)
	}
	if len(impact.Changes) != 1 || impact.Changes[0].Kind != ChangeParamTypeChanged {
		t.Fatalf("changes = %+v, want param_type_changed", impact.Changes)
	}
	// 受影响的是**右端**跟随 latest 的 link
	if len(impact.Links) != 1 || impact.Links[0].ToRef != "fixer" {
		t.Fatalf("links = %+v, want 一条 to_ref=fixer", impact.Links)
	}
}

// TestUpgradeGateOnlyForLatest 不会成为 latest 的版本不过闸门（它不是「升级」）。
//
// 场景：fixer@1 已在；候选 @2 加可选入参（放行）成为 latest；此后
//   - 拿一个**同样 @2** 的破坏性内容去问 → 它不会成为 latest，闸门给空影响面；
//   - 拿 @3 的破坏性内容去问 → 会顶掉 latest，闸门照常报破坏。
func TestUpgradeGateOnlyForLatest(t *testing.T) {
	db, nsID, _, _ := gateFixture(t)
	ctx := context.Background()

	optional := v4Spec(`{"type":"object","required":["issue"],"properties":{
		"issue":{"type":"string"},"note":{"type":"string"}}}`, nil, "")
	v2 := v4CapWith(t, db, &CapabilityVersion{NamespaceID: nsID, Name: "fixer", Version: 2, Spec: optional})
	if v2.Version != 2 {
		t.Fatalf("fixer@2 落库版本 = %d, want 2", v2.Version)
	}

	// 同一个版本号（2）+ 破坏性内容：不会成为 latest → 闸门不管。
	notLatest := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		Spec: v4Spec(`{"type":"object","required":["issue"],"properties":{"issue":{"type":"integer"}}}`, nil, ""),
	}
	impact, err := db.CheckUpgrade(ctx, nsID, notLatest)
	if err != nil {
		t.Fatalf("check upgrade (not latest): %v", err)
	}
	if impact.Breaking || len(impact.Changes) != 0 {
		t.Fatalf("不会成为 latest 的版本不该被闸门拦: %+v", impact)
	}

	// @3 的破坏性内容会顶掉 latest → 报破坏。
	willBeLatest := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 3,
		Spec: v4Spec(`{"type":"object","required":["issue"],"properties":{"issue":{"type":"integer"}}}`, nil, ""),
	}
	impact, err = db.CheckUpgrade(ctx, nsID, willBeLatest)
	if err != nil {
		t.Fatalf("check upgrade (latest): %v", err)
	}
	if !impact.Breaking || impact.FromVersion != 2 || impact.ToVersion != 3 {
		t.Fatalf("impact = %+v, want breaking (2 → 3)", impact)
	}
}

// TestUpgradeImpactEmptyOnFirstVersion 第一个版本没有「上一版」，谈不上破坏。
func TestUpgradeImpactEmptyOnFirstVersion(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := mkCap(nsID, "brand-new", 1, []string{"out"})
	impact, err := db.CheckUpgrade(ctx, nsID, c)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if impact.Breaking || impact.FromVersion != 0 {
		t.Fatalf("impact = %+v, want 空（首个版本）", impact)
	}
	if impact.Changes == nil || impact.Links == nil {
		t.Fatalf("空影响面也必须是 [] 而不是 null（I9）: %+v", impact)
	}
}
