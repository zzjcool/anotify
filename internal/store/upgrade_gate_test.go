package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 升级闸门的验收（P2.2，设计 typed-pipeline §3.6）。
//
// 判据（§3.6 那张表）：
//
//	加可选入参 / 加 port        → 直接通过
//	删 port / 改入参类型 / 加必填 → 拒绝，并列出受影响的 link；除非显式 force
//
// 为什么必须先有闸门再有引用语义（§7「P2.2 施工计划」第 1 条）：一旦 link 能跟随
// latest，把一个破坏性版本推上去就等于**静默改线**——没人点过任何东西，线上那条链
// 却换了契约。闸门把这一步变成一次显式决定。

// gateFixture 造一个「review --fix--> fixer（裸名字）」的图，返回三个能力。
//
// 关键点：`to_ref` 是**裸名字**（跟随 latest）——只有这种 link 会被闸门影响；
// 钉死的 `fixer@1` 不受任何升级影响（它钉的就是旧版）。
func gateFixture(t *testing.T) (db *DB, nsID string, review, fixer *CapabilityVersion) {
	t.Helper()
	ctx := context.Background()
	db = newTestDB(t)
	nsID, _ = newTestNS(t, db)

	review = mkCap(nsID, "review", 1, []string{"fix"})
	fixer = &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","required":["issue"],"properties":{"issue":{"type":"string"}}}`),
		Outputs:      []string{"done", "failed"},
		Source:       CapSourceCustom,
	}
	mustCreateCap(t, db, review, fixer)
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromRef: "review", FromPort: "fix", ToRef: "fixer",
	}); err != nil {
		t.Fatalf("create link review --fix--> fixer: %v", err)
	}
	return db, nsID, review, fixer
}

// TestUpgradeGateRemovedPort 删 port → 拒绝，且错误里列出受影响的 link。
func TestUpgradeGateRemovedPort(t *testing.T) {
	db, nsID, _, fixer := gateFixture(t)
	ctx := context.Background()

	// 新版删掉 failed 出口
	next := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","required":["issue"],"properties":{"issue":{"type":"string"}}}`),
		Outputs:      []string{"done"},
		Source:       CapSourceCustom,
	}

	// CheckUpgrade 先算影响面（handler 用它给前端展示）
	impact, err := db.CheckUpgrade(ctx, nsID, next)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if !impact.Breaking || impact.FromVersion != 1 || impact.ToVersion != 2 {
		t.Fatalf("impact = %+v, want breaking (1 → 2)", impact)
	}
	if len(impact.Changes) != 1 || impact.Changes[0].Kind != ChangeRemovedPort {
		t.Fatalf("changes = %+v, want 一条 removed_port", impact.Changes)
	}
	// 受影响的是「左端跟随 latest 且端口正好被删」的边（review --failed--> …）。
	// 夹具里没有这种边，所以 links 是**空数组**（不是 null）。
	if impact.Links == nil || len(impact.Links) != 0 {
		t.Fatalf("links = %v, want 空数组（没有 link 挂在这个出口上）", impact.Links)
	}

	// 写入被拒，且是**结构化拒绝**
	err = db.CreateCapability(ctx, next)
	if !errors.Is(err, ErrUpgradeBreaking) {
		t.Fatalf("err = %v (%T), want ErrUpgradeBreaking", err, err)
	}
	var breaking *UpgradeBreakingError
	if !errors.As(err, &breaking) {
		t.Fatalf("err 里没有结构化的 impact: %v (%T)", err, err)
	}
	if len(breaking.Impact.Changes) != 1 || breaking.Impact.Changes[0].Kind != ChangeRemovedPort {
		t.Fatalf("拒绝里没说清变了什么: %+v", breaking.Impact.Changes)
	}
	// 版本没被写进去（append-only 撤不回，所以必须拒在写入之前）
	if _, gerr := db.GetCapability(ctx, nsID, "fixer", 2); !errors.Is(gerr, ErrNotFound) {
		t.Fatalf("fixer@2 竟然写进去了（err=%v）", gerr)
	}
	_ = fixer

	// force 能越过闸门（用户的显式决定）
	if err := db.CreateCapabilityForce(ctx, next, true); err != nil {
		t.Fatalf("force create fixer@2: %v", err)
	}
	if _, err := db.GetCapability(ctx, nsID, "fixer", 2); err != nil {
		t.Fatalf("force 之后 fixer@2 应该存在: %v", err)
	}
}

// TestUpgradeGateListsAffectedLinks 受影响 link 必须按 `from_ref --port--> to_ref` 列出来。
func TestUpgradeGateListsAffectedLinks(t *testing.T) {
	db, nsID, _, _ := gateFixture(t)
	ctx := context.Background()

	// 再挂两条边到 failed 出口上（跟随 latest 的 fixer）与一条钉死的 fixer@1
	if err := db.CreateEdge(ctx, &Edge{NamespaceID: nsID, FromRef: "review@1", FromPort: "fix", ToRef: "fixer@1"}); err != nil {
		t.Fatalf("create pinned link: %v", err)
	}

	next := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","required":["issue","note"],"properties":{
			"issue":{"type":"string"},"note":{"type":"string"}}}`),
		Outputs: []string{"done"}, // 删 failed + 新增必填 note
		Source:  CapSourceCustom,
	}
	impact, err := db.CheckUpgrade(ctx, nsID, next)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if !impact.Breaking {
		t.Fatalf("impact 应该 breaking: %+v", impact)
	}
	// 两类变化都算出来了：removed_port + param_added_required
	kinds := map[UpgradeChangeKind]bool{}
	for _, c := range impact.Changes {
		kinds[c.Kind] = true
	}
	if !kinds[ChangeRemovedPort] || !kinds[ChangeParamAddedRequired] {
		t.Fatalf("changes = %+v, want removed_port + param_added_required", impact.Changes)
	}
	// 受影响 link：只有**裸名字**那条（review --fix--> fixer）。
	// 钉死的 review@1 --fix--> fixer@1 两端都钉死 → 不受影响。
	if len(impact.Links) != 1 {
		t.Fatalf("links = %+v, want 恰好 1 条（只有跟随 latest 的那条受影响）", impact.Links)
	}
	got := impact.Links[0]
	if got.FromRef != "review" || got.Port != "fix" || got.ToRef != "fixer" {
		t.Fatalf("link = %s, want `review --fix--> fixer`", got)
	}
	if !strings.Contains(got.Reason, "跟随 latest") && !strings.Contains(got.Reason, "删掉") {
		t.Fatalf("link 的 reason 没说清为什么受影响: %q", got.Reason)
	}
	// 错误文案里带着这条 link（纯文本客户端也看得见）
	err = db.CreateCapability(ctx, next)
	var breaking *UpgradeBreakingError
	if !errors.As(err, &breaking) {
		t.Fatalf("err = %v, want *UpgradeBreakingError", err)
	}
	if !strings.Contains(err.Error(), "review --fix--> fixer") {
		t.Fatalf("错误文案里没有受影响 link: %v", err)
	}
}

// TestUpgradeGateAllowsAdditiveChanges 加可选入参 / 加 port → 直接通过（不需要 force）。
func TestUpgradeGateAllowsAdditiveChanges(t *testing.T) {
	db, nsID, _, _ := gateFixture(t)
	ctx := context.Background()

	next := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		// 只加：一个可选入参 + 一个新 port
		ParamsSchema: json.RawMessage(`{"type":"object","required":["issue"],"properties":{
			"issue":{"type":"string"},"note":{"type":"string"}}}`),
		Outputs: []string{"done", "failed", "skipped"},
		Source:  CapSourceCustom,
	}
	impact, err := db.CheckUpgrade(ctx, nsID, next)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if impact.Breaking || len(impact.Changes) != 0 {
		t.Fatalf("impact = %+v, want 空影响面（只增不减）", impact)
	}
	if err := db.CreateCapability(ctx, next); err != nil {
		t.Fatalf("加可选入参/加 port 应该直接通过: %v", err)
	}
}

// TestUpgradeGateParamTypeChanged 改入参类型 → 拒绝。
func TestUpgradeGateParamTypeChanged(t *testing.T) {
	db, nsID, _, _ := gateFixture(t)
	ctx := context.Background()

	next := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","required":["issue"],"properties":{
			"issue":{"type":"integer"}}}`), // string → integer
		Outputs: []string{"done", "failed"},
		Source:  CapSourceCustom,
	}
	err := db.CreateCapability(ctx, next)
	if !errors.Is(err, ErrUpgradeBreaking) {
		t.Fatalf("err = %v (%T), want ErrUpgradeBreaking", err, err)
	}
	var breaking *UpgradeBreakingError
	if !errors.As(err, &breaking) {
		t.Fatalf("err 里没有结构化 impact: %T", err)
	}
	if len(breaking.Impact.Changes) != 1 || breaking.Impact.Changes[0].Kind != ChangeParamTypeChanged {
		t.Fatalf("changes = %+v, want param_type_changed", breaking.Impact.Changes)
	}
	// 受影响的是**右端**跟随 latest 的 link
	if len(breaking.Impact.Links) != 1 || breaking.Impact.Links[0].ToRef != "fixer" {
		t.Fatalf("links = %+v, want 一条 to_ref=fixer", breaking.Impact.Links)
	}
}

// TestUpgradeGateOnlyForLatest 不会成为 latest 的版本不过闸门（它不是「升级」）。
//
// 场景：先有 fixer@1、fixer@2；再创建 fixer@3（破坏性）会被拦；
// 但把 @3 内容改成与 @2 相同 → 幂等折叠，不会成为新版本，也不该被拦。
func TestUpgradeGateOnlyForLatest(t *testing.T) {
	db, nsID, _, _ := gateFixture(t)
	ctx := context.Background()

	v2 := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object","required":["issue"],"properties":{
			"issue":{"type":"string"},"extra":{"type":"string"}}}`),
		Outputs: []string{"done", "failed"},
		Source:  CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, v2); err != nil {
		t.Fatalf("create fixer@2（加可选入参）: %v", err)
	}
	// 再把 v2 的内容当 v3 提交：同内容 → 幂等折叠回 @2，不会成为 latest
	same := &CapabilityVersion{
		NamespaceID: nsID, Name: "fixer", Version: 3,
		ParamsSchema: v2.ParamsSchema, Outputs: v2.Outputs, Source: CapSourceCustom,
	}
	impact, err := db.CheckUpgrade(ctx, nsID, same)
	if err != nil {
		t.Fatalf("check upgrade: %v", err)
	}
	if impact.Breaking {
		t.Fatalf("同内容的不同版本号不该被闸门拦: %+v", impact)
	}
	if err := db.CreateCapability(ctx, same); err != nil {
		t.Fatalf("同内容创建应该幂等通过: %v", err)
	}
	if same.Version != 2 {
		t.Fatalf("幂等折叠后版本 = %d, want 2（回到已有那一行）", same.Version)
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
