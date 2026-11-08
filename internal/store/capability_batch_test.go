package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 本文件钉住 CreateCapabilitiesBatch（契约 §4）里**只有 store 层才能验证**的东西：
//
//	① 两个哨兵（空批 / 批内重名）—— 调用方要能据此回 400，而不是 409
//	② builders 与 capabilities 的对齐（错位会把 A 的产物挂到 B 上）
//	③ 三个面的整批回滚（能力 / 产物 / 边）
//	④ 合并影响面：整批一个对象，但 Changes/Links 是全部破坏性能力的并集
//
// 环不在这里验：契约 §2 把配置期静态环检测删掉（A 组的竖切），
// 「批量与单建在成环上同进同退」由 HTTP 层的等价性用例覆盖。

// capsWithSpec 给一批夹具补正文（见 withSpec）。
//
// 批量创建走的是**另一条入口**（CreateCapabilitiesBatch），per-call 的 withSpec 包裹
// 覆盖不到它，所以这里显式折一次。
func capsWithSpec(caps []*CapabilityVersion) []*CapabilityVersion {
	for _, c := range caps {
		withSpec(c)
	}
	return caps
}

// TestCreateCapabilitiesBatchRejectsEmptyAndDuplicate：两个哨兵错误。
func TestCreateCapabilitiesBatchRejectsEmptyAndDuplicate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if _, err := db.CreateCapabilitiesBatch(ctx, nil, nil, nil, false); !errors.Is(err, ErrBatchEmpty) {
		t.Fatalf("空批: err = %v, want ErrBatchEmpty", err)
	}

	// 批内重名：links 用名字引用，重名让「这条边指向哪一个」无法表达 → 400。
	_, err := db.CreateCapabilitiesBatch(ctx,
		capsWithSpec([]*CapabilityVersion{mkCap(nsID, "dup", 1, nil), mkCap(nsID, "dup", 2, nil)}),
		nil, nil, false)
	if !errors.Is(err, ErrBatchDuplicateName) {
		t.Fatalf("重名: err = %v, want ErrBatchDuplicateName", err)
	}
	// 判重按 trim 后的名字：`"dup "` 与 `"dup"` 算同一个——靠空白绕过只会把
	// 冲突推到「两条边分别指向哪一行」这种更难查的地方。
	if !strings.Contains(err.Error(), "capabilities[1]") {
		t.Fatalf("重名的错误必须指出是第几条: %v", err)
	}
	// 命名空间建行时播种了内置能力，所以按**名字**判「什么都没留下」。
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND name = ?", nsID, "dup"); n != 0 {
		t.Fatalf("重名被拒却写入了能力行: %d", n)
	}
}

// TestCreateCapabilitiesBatchRejectsMisalignedBuilders：builders 与能力必须一一对齐。
func TestCreateCapabilitiesBatchRejectsMisalignedBuilders(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	caps := []*CapabilityVersion{mkCap(nsID, "a", 1, nil), mkCap(nsID, "b", 1, nil)}
	one := []ArtifactBuilder{func(*CapabilityVersion) (*Artifact, error) { return nil, nil }}
	if _, err := db.CreateCapabilitiesBatch(ctx, capsWithSpec(caps), nil, one, false); err == nil {
		t.Fatal("builders 长度不对必须报错（否则会把 A 的产物挂到 B 上）")
	}
	if n := countRows(t, db, "capability_versions",
		"namespace_id = ? AND name IN (?,?)", nsID, "a", "b"); n != 0 {
		t.Fatalf("对齐错误却写入了能力行: %d", n)
	}
}

// TestCreateCapabilitiesBatchLinksAcrossBatch：先插全部能力，边可以引用本批新建的名字。
func TestCreateCapabilitiesBatchLinksAcrossBatch(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// 注意顺序：数组里 b 在前、a 在后，而边是 a → b（前向引用）。
	caps := []*CapabilityVersion{
		mkCap(nsID, "b", 1, []string{"done"}),
		mkCap(nsID, "a", 1, []string{"fix"}),
	}
	res, err := db.CreateCapabilitiesBatch(ctx, capsWithSpec(caps),
		[]BatchLinkSpec{{FromRef: "a", FromPort: "fix", ToRef: "b"}}, nil, false)
	if err != nil {
		t.Fatalf("批量创建失败: %v", err)
	}
	if len(res.Capabilities) != 2 || len(res.Edges) != 1 {
		t.Fatalf("回执不对: %d 能力 / %d 边", len(res.Capabilities), len(res.Edges))
	}
	// 归一后的引用（两端都是名字：跟随 latest）
	if e := res.Edges[0]; e.FromRef != "a" || e.FromPort != "fix" || e.ToRef != "b" {
		t.Fatalf("边没归一: %+v", e)
	}
	if n := countRows(t, db, "edges", "namespace_id = ?", nsID); n != 1 {
		t.Fatalf("边行数 = %d, want 1", n)
	}
	// 影响面恒非 nil、恒非 null 数组
	if res.Impact == nil || res.Impact.Changes == nil || res.Impact.Links == nil {
		t.Fatalf("影响面必须恒在且是 [] 而不是 nil: %+v", res.Impact)
	}
	if res.Impact.Capability != "b" {
		t.Fatalf("没有破坏性变更时身份字段取批内第一个能力，got %q（want b）", res.Impact.Capability)
	}
}

// TestCreateCapabilitiesBatchRollsBackOnBadLink：一条坏 link → 能力/产物/边全部不留。
func TestCreateCapabilitiesBatchRollsBackOnBadLink(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	linksTarget(t, db, nsID, "notify.send", "")

	caps := []*CapabilityVersion{
		mkCap(nsID, "batch.ok", 1, []string{"done"}),
		mkCap(nsID, "batch.ok2", 1, []string{"done"}),
	}
	build := realBuild(t)
	// 第二条 link 指向不存在的名字：此时两个能力 + 两份产物已经在事务里了。
	_, err := db.CreateCapabilitiesBatch(ctx, capsWithSpec(caps),
		[]BatchLinkSpec{
			{FromRef: "batch.ok", FromPort: "done", ToRef: "notify.send"},
			{FromRef: "batch.ok2", FromPort: "done", ToRef: "ghost"},
		},
		[]ArtifactBuilder{build, build}, false)
	if err == nil {
		t.Fatal("坏 link 必须返回错误")
	}
	if !errors.Is(err, ErrEdgeRejected) {
		t.Fatalf("err = %v, want ErrEdgeRejected（handler 据此回 400）", err)
	}
	if !strings.Contains(err.Error(), "links[1]") {
		t.Fatalf("错误必须指出是第几条 link: %v", err)
	}
	// 三个面逐项为 0
	for _, name := range []string{"batch.ok", "batch.ok2"} {
		assertNothingPersisted(t, db, nsID, name)
	}
	if n := countRows(t, db, "edges", "namespace_id = ?", nsID); n != 0 {
		t.Fatalf("回滚失败：edges 里留下了 %d 条", n)
	}
}

// TestCreateCapabilitiesBatchArtifactBuildFailureRollsBack：事务内构建产物失败 → 整体回滚。
func TestCreateCapabilitiesBatchArtifactBuildFailureRollsBack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	boom := errors.New("entrypoint: 入口文件必须以 #! 开头")
	okBuild := realBuild(t)
	// 第二个能力的产物构建失败（第一个已经建成、产物已落库）
	_, err := db.CreateCapabilitiesBatch(ctx,
		capsWithSpec([]*CapabilityVersion{mkCap(nsID, "batch.one", 1, nil), mkCap(nsID, "batch.two", 1, nil)}),
		nil,
		[]ArtifactBuilder{okBuild, func(*CapabilityVersion) (*Artifact, error) { return nil, boom }}, false)
	if err == nil {
		t.Fatal("产物构建失败必须返回错误")
	}
	var buildErr *ArtifactBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("err = %v, want *ArtifactBuildError（handler 据此回 400 invalid_artifact）", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("原始错误必须可 unwrap: %v", err)
	}
	assertNothingPersisted(t, db, nsID, "batch.one")
	assertNothingPersisted(t, db, nsID, "batch.two")
}

// TestCreateCapabilitiesBatchMergesBreakingImpact：整批 409 必须报**全部**破坏性能力。
//
// createCapabilityTx 的闸门只会报「第一个撞上的」，所以批量在写入前自己合并一遍：
// 两个能力各自删掉一个出口 → 变更条目 2 条、受影响 link 2 条，身份字段取第一个。
func TestCreateCapabilitiesBatchMergesBreakingImpact(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// 两个已有能力（各两个出口）+ 各自一条**从 failed 出发**的边：
	// 「删出口」影响的正是这种边（左端跟随 latest、端口就是被删的那个）。
	for _, name := range []string{"batch.x", "batch.y"} {
		c := mkCap(nsID, name, 1, []string{"done", "failed"})
		if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
			t.Fatalf("seed %s@1: %v", name, err)
		}
		down := mkCap(nsID, "down."+name, 1, nil)
		if err := db.CreateCapability(ctx, withSpec(down)); err != nil {
			t.Fatalf("seed down.%s: %v", name, err)
		}
		if err := db.CreateEdge(ctx, &Edge{
			NamespaceID: nsID, FromRef: name, FromPort: "failed", ToRef: down.Name,
		}); err != nil {
			t.Fatalf("seed edge %s --failed--> %s: %v", name, down.Name, err)
		}
	}

	// v2：都删掉一个出口 → 两个都是破坏性变更（outputs 与 v1 不同，所以是真新版本，
	// 不会被 content-hash 幂等折叠回 v1）。
	v2x := mkCap(nsID, "batch.x", 2, []string{"done"})
	v2y := mkCap(nsID, "batch.y", 2, []string{"done"})

	_, err := db.CreateCapabilitiesBatch(ctx,
		capsWithSpec([]*CapabilityVersion{v2x, v2y}), nil, nil, false)
	if err == nil {
		t.Fatal("破坏性批量必须被拒")
	}
	if !errors.Is(err, ErrUpgradeBreaking) {
		t.Fatalf("err = %v, want ErrUpgradeBreaking", err)
	}
	var breaking *UpgradeBreakingError
	if !errors.As(err, &breaking) || breaking.Impact == nil {
		t.Fatalf("err = %v, want *UpgradeBreakingError（handler 要拿 Impact 回 409 body）", err)
	}
	impact := breaking.Impact
	if !impact.Breaking {
		t.Fatal("Impact.Breaking 必须是 true")
	}
	if impact.Capability != "batch.x" {
		t.Fatalf("身份字段取第一个破坏性能力，got %q（want batch.x）", impact.Capability)
	}
	// 两个能力的变化与两条受影响的 link 都要在（这是「整批」而不是「第一个」的证据）
	if len(impact.Changes) != 2 {
		t.Fatalf("Changes = %d 条，want 2（两个能力各删了一个出口）: %v", len(impact.Changes), impact.Changes)
	}
	if len(impact.Links) != 2 {
		t.Fatalf("Links = %d 条，want 2: %v", len(impact.Links), impact.Links)
	}
	// 什么都没写
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND version = 2", nsID); n != 0 {
		t.Fatalf("闸门拒了却写入了 v2 行: %d", n)
	}

	// force 越过 → 两个 v2 都写进去（同一个事务）
	if _, err := db.CreateCapabilitiesBatch(ctx,
		capsWithSpec([]*CapabilityVersion{v2x, v2y}), nil, nil, true); err != nil {
		t.Fatalf("force 越过失败: %v", err)
	}
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND version = 2", nsID); n != 2 {
		t.Fatalf("force 之后 v2 行数 = %d, want 2", n)
	}
}

// TestMergeUpgradeImpactDedupAndSort：并集去重 + 稳定顺序 + 能力名前缀（纯函数）。
func TestMergeUpgradeImpactDedupAndSort(t *testing.T) {
	dst := &UpgradeImpact{Changes: []UpgradeChange{}, Links: []UpgradeLink{}}
	mergeUpgradeImpact(dst, &UpgradeImpact{
		Capability: "a", FromVersion: 1, ToVersion: 2, Breaking: true,
		Changes: []UpgradeChange{{Kind: ChangeRemovedPort, Detail: `port "p" 被删`}},
		Links:   []UpgradeLink{{FromRef: "b", Port: "p", ToRef: "a", Reason: "r1"}},
	})
	mergeUpgradeImpact(dst, &UpgradeImpact{
		Capability: "a", FromVersion: 1, ToVersion: 2, Breaking: true,
		Changes: []UpgradeChange{
			{Kind: ChangeRemovedPort, Detail: `port "p" 被删`}, // 同能力同文案 → 去重
			{Kind: ChangeParamAddedRequired, Detail: "params.n 必填"},
		},
		// 同一条 link、不同 reason → 合并成一行
		Links: []UpgradeLink{
			{FromRef: "b", Port: "p", ToRef: "a", Reason: "r2"},
			{FromRef: "a", Port: "q", ToRef: "b", Reason: "r3"},
		},
	})
	if dst.Capability != "a" || dst.FromVersion != 1 || dst.ToVersion != 2 {
		t.Fatalf("身份字段应取**第一个破坏性能力**: %+v", dst)
	}
	if len(dst.Changes) != 2 {
		t.Fatalf("Changes = %d, want 2（去重后）: %v", len(dst.Changes), dst.Changes)
	}
	// Detail 带能力名前缀：整批里必须看得出是哪条能力变的（也是去重的前提）。
	for _, c := range dst.Changes {
		if !strings.HasPrefix(c.Detail, "a: ") {
			t.Fatalf("Change.Detail 缺能力名前缀: %q", c.Detail)
		}
	}
	// 稳定顺序：先按 kind，再按 detail（change_param_added_required < removed_port）
	if dst.Changes[0].Kind != ChangeParamAddedRequired {
		t.Fatalf("Changes 未按 kind 排序: %v", dst.Changes)
	}
	if len(dst.Links) != 2 {
		t.Fatalf("Links = %d, want 2（同一条合并、另一条新增）: %v", len(dst.Links), dst.Links)
	}
	if dst.Links[0].FromRef != "a" || dst.Links[1].FromRef != "b" {
		t.Fatalf("Links 未按 (fromRef, port, toRef) 排序: %v", dst.Links)
	}
	if !strings.Contains(dst.Links[1].Reason, "r1") || !strings.Contains(dst.Links[1].Reason, "r2") {
		t.Fatalf("同一条 link 的两个 reason 应被拼接: %q", dst.Links[1].Reason)
	}
}
