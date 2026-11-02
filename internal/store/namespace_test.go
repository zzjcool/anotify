package store

// 命名空间（#162）的 store 层验收测试。
//
// 覆盖三组硬约束：
//  1. 命名空间生命周期（CRUD / 阻塞删除 / 最后一个不许删）——契约 §2.1；
//  2. 所有命名空间资源的作用域参数**必须显式**（空串与 "*" 一律
//     ErrNamespaceRequired，绝不回退默认命名空间）——契约 §2.3；
//  3. I11：跨命名空间引用四处一律拒绝——契约 §2.4。
//
// 每条断言都写清「为什么重要」，因为这一层的失败方式大多是**静默串数据**：
// 写错一处不会报错，只会在 prod 的库里看见 staging 的机器。

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// mkSiblingNamespace 在同一个部署里再建一个命名空间（默认命名空间之外的「第二套流程」）。
//
// 为什么测试要用**同一部署里的兄弟命名空间**：迁移 14 之后命名空间是**唯一**隔离维度，
// 用两个命名空间才能把隔离面钉死；组织这一层已经不存在了（#204 / #205）。
func mkSiblingNamespace(t *testing.T, db *DB, slug string) *Namespace {
	t.Helper()
	ns, err := db.CreateNamespace(context.Background(), slug, slug)
	if err != nil {
		t.Fatalf("create namespace %q: %v", slug, err)
	}
	return ns
}

// TestNamespaceCreateSeedsBuiltins 新建命名空间必须**同事务**播种内置能力。
//
// 为什么重要：新命名空间里没有 notify.phone，就连一个 end 节点都声明不出来
// （ConfirmCapabilities 报 unknown capability），推送在那边直接失效；
// 而「稍后补播种」失败时没有任何人会看到。
func TestNamespaceCreateSeedsBuiltins(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	sandbox, err := db.CreateNamespace(ctx, "sandbox", "沙箱")
	if err != nil {
		t.Fatalf("create sandbox namespace: %v", err)
	}

	got, err := db.GetCapability(ctx, sandbox.ID, "notify.phone", 1)
	if err != nil {
		t.Fatalf("新建命名空间里没有 notify.phone@1（没有同事务播种？）: %v", err)
	}
	// sandbox 是「空命名空间」，但内置能力不是「用户数据」，它必须在。
	if got.Source != CapSourceBuiltin {
		t.Fatalf("notify.phone 的 source = %q, want %q", got.Source, CapSourceBuiltin)
	}
	if got.NamespaceID != sandbox.ID {
		t.Fatalf("notify.phone 落到了命名空间 %q, want %q", got.NamespaceID, sandbox.ID)
	}

	// 每个命名空间一份：默认命名空间与 sandbox 是**两行**，不是共享同一行。
	def, err := db.GetCapability(ctx, nsID, "notify.phone", 1)
	if err != nil {
		t.Fatalf("默认命名空间里没有 notify.phone@1: %v", err)
	}
	if def.ID == got.ID {
		t.Fatal("两个命名空间共享同一个 capability id：播种没有按命名空间隔离")
	}

	// 播种后 sandbox 的能力列表里只有内置能力（不夹带默认命名空间的自定义能力）
	defCap := mkCap(nsID, "review.only-in-default", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, defCap); err != nil {
		t.Fatalf("create custom capability in default namespace: %v", err)
	}
	list, err := db.ListCapabilities(ctx, sandbox.ID)
	if err != nil {
		t.Fatalf("list sandbox capabilities: %v", err)
	}
	for _, c := range list {
		if c.Name == "review.only-in-default" {
			t.Fatal("sandbox 看到了默认命名空间的自定义能力（隔离破了）")
		}
	}
}

// TestNamespaceCRUD 命名空间生命周期：列表 / 建 / 取 / 改名 / 删。
func TestNamespaceCRUD(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// ---------- 列表：新库里还没有命名空间，返回 [] 不是 nil（不变量 I9）----------
	empty, err := db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces on empty db: %v", err)
	}
	if empty == nil {
		t.Fatal("ListNamespaces 返回 nil，需要 []（不变量 I9）")
	}
	if len(empty) != 0 {
		t.Fatalf("新库返回 %d 个命名空间，want 0", len(empty))
	}

	nsID, _ := newTestNS(t, db)

	// ---------- 列表：默认命名空间 + 计数 ----------
	list, err := db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("注册一个用户后应有 1 个命名空间（default），got %d: %+v", len(list), list)
	}
	def := list[0]
	if def.ID != nsID || def.Slug != DefaultNamespaceSlug {
		t.Fatalf("默认命名空间 = %+v, want id=%s slug=%s", def, nsID, DefaultNamespaceSlug)
	}
	// 计数直接给 UI 用：内置能力必须算进 CapabilityCount（否则列表显示 0 个能力、
	// 点进去却有 notify.phone，前端会当成两套数据）。
	if def.CapabilityCount != 1 {
		t.Fatalf("默认命名空间 CapabilityCount = %d, want 1（notify.phone@1）", def.CapabilityCount)
	}
	if def.NodeCount != 0 || def.SourceCount != 0 || def.EdgeCount != 0 {
		t.Fatalf("空命名空间计数应为 0/0/0，got %d/%d/%d",
			def.NodeCount, def.SourceCount, def.EdgeCount)
	}

	// ---------- 建：slug 校验 ----------
	for _, bad := range []string{"", "  ", "Upper", "-lead", "under_score", "中文", strings.Repeat("a", 40)} {
		if _, err := db.CreateNamespace(ctx, bad, "坏 slug"); err == nil {
			t.Errorf("CreateNamespace(slug=%q) 应该被拒绝", bad)
		}
		// 非法 slug 不是「重名」：两者混淆会让 handler 回错状态码（400 vs 409）
		if _, err := db.CreateNamespace(ctx, bad, "坏 slug"); errors.Is(err, ErrNamespaceExists) {
			t.Errorf("CreateNamespace(slug=%q) 被报成了 ErrNamespaceExists（应为非法 slug）", bad)
		}
	}

	staging, err := db.CreateNamespace(ctx, "staging", "预发布")
	if err != nil {
		t.Fatalf("create staging: %v", err)
	}
	if staging.Name != "预发布" || staging.Slug != "staging" || staging.CreatedAt == 0 {
		t.Fatalf("新建命名空间字段不对: %+v", staging)
	}

	// slug 全局唯一（迁移 14：UNIQUE(slug)）→ 重复一律 ErrNamespaceExists
	if _, err := db.CreateNamespace(ctx, "staging", "又一份 staging"); !errors.Is(err, ErrNamespaceExists) {
		t.Fatalf("重复 slug err = %v, want ErrNamespaceExists", err)
	}
	// 第二个注册用户的默认 slug 必须让位（否则撞 UNIQUE(slug)）
	otherNS, _ := newTestNS(t, db)
	other, err := db.GetNamespace(ctx, otherNS)
	if err != nil {
		t.Fatalf("get second default namespace: %v", err)
	}
	if other.Slug != "default-2" {
		t.Fatalf("第二个用户的默认命名空间 slug = %q, want default-2（slug 全局唯一）", other.Slug)
	}
	// 手动抢 default 一样被拒
	if _, err := db.CreateNamespace(ctx, DefaultNamespaceSlug, "抢默认"); !errors.Is(err, ErrNamespaceExists) {
		t.Fatalf("显式抢 default slug err = %v, want ErrNamespaceExists", err)
	}

	// ---------- 取：命中 / 不存在 ----------
	if got, err := db.GetNamespace(ctx, staging.ID); err != nil || got.Slug != "staging" {
		t.Fatalf("GetNamespace = %+v, %v", got, err)
	}
	if _, err := db.GetNamespace(ctx, "ns_does_not_exist"); !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("GetNamespace(missing) err = %v, want ErrNamespaceNotFound", err)
	}
	// 空 id 与 "*" 一律 ErrNamespaceRequired（绝不回退默认命名空间）
	if _, err := db.GetNamespace(ctx, ""); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("GetNamespace(\"\") err = %v, want ErrNamespaceRequired", err)
	}

	// ---------- 改名：只改 name，slug 不动 ----------
	if err := db.RenameNamespace(ctx, staging.ID, "预发布环境"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	renamed, err := db.GetNamespace(ctx, staging.ID)
	if err != nil {
		t.Fatalf("get renamed: %v", err)
	}
	if renamed.Name != "预发布环境" {
		t.Fatalf("rename 没生效: name=%q", renamed.Name)
	}
	// slug 是稳定标识（出现在 URL 与令牌作用域里），改名字不能动它
	if renamed.Slug != "staging" {
		t.Fatalf("rename 改了 slug: %q, want staging", renamed.Slug)
	}
	if err := db.RenameNamespace(ctx, staging.ID, "  "); err == nil {
		t.Error("RenameNamespace(空名) 应该被拒绝")
	}
	if err := db.RenameNamespace(ctx, "ns_does_not_exist", "偷改"); !errors.Is(err, ErrNamespaceNotFound) {
		t.Errorf("改名不存在的命名空间 err = %v, want ErrNamespaceNotFound", err)
	}

	// ---------- 删：非空且 !force → 409 + 列出阻塞对象 ----------
	// 在 staging 里造齐四类阻塞对象（源 / 边 / 任务 / 节点）
	stagingCap := mkCap(staging.ID, "review", 1, []string{"fix"})
	sinkCap := mkCap(staging.ID, "fix", 1, nil)
	for _, c := range []*CapabilityVersion{stagingCap, sinkCap} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create capability in staging: %v", err)
		}
	}
	if _, err := db.CreateNode(ctx, &Node{NamespaceID: staging.ID, Name: "staging-node"}); err != nil {
		t.Fatalf("create node in staging: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: staging.ID, FromCapability: stagingCap.ID, FromPort: "fix", ToCapability: sinkCap.ID,
	}); err != nil {
		t.Fatalf("create edge in staging: %v", err)
	}
	if err := db.CreateSource(ctx, &Source{
		NamespaceID: staging.ID, Name: "staging-src", Kind: SourceKindManual, Enabled: true,
		Targets: []SourceTarget{{CapabilityID: stagingCap.ID}},
	}); err != nil {
		t.Fatalf("create source in staging: %v", err)
	}
	if err := db.CreateTasks(ctx, []*Task{mkTask(staging.ID, "trace_staging", stagingCap)}); err != nil {
		t.Fatalf("create task in staging: %v", err)
	}

	err = db.DeleteNamespace(ctx, staging.ID, false)
	var blocked *NamespaceBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("非空命名空间删除 err = %v (%T), want *NamespaceBlockedError", err, err)
	}
	// 409 的正文必须说清「你在拆什么」：删命名空间 = 里面的机器全部掉线
	joined := strings.Join(blocked.Blocking, ",")
	for _, want := range []string{"sources=1", "edges=1", "tasks=1", "nodes=1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("阻塞清单缺 %q（实际 %q）", want, joined)
		}
	}
	// 被拒的删除不能动数据
	if n := countRows(t, db, "nodes", "namespace_id = ?", staging.ID); n != 1 {
		t.Fatalf("被拒的删除动了节点：剩 %d 行", n)
	}

	// ---------- 删：force=true → 级联删干净 ----------
	// 先塞一条事件，验证 events 也在级联范围里（它没有阻塞探针，但必须被删）
	if _, err := db.InsertEvent(ctx, &Event{
		NamespaceID: staging.ID, TraceID: "trace_staging", Source: "github", Type: "push",
	}); err != nil {
		t.Fatalf("insert event in staging: %v", err)
	}
	if _, err := db.EnsureGraphVersion(ctx, staging.ID); err != nil {
		t.Fatalf("ensure graph version in staging: %v", err)
	}

	if err := db.DeleteNamespace(ctx, staging.ID, true); err != nil {
		t.Fatalf("force delete: %v", err)
	}
	if _, err := db.GetNamespace(ctx, staging.ID); !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("删完还查得到命名空间: %v", err)
	}
	// 五张表 + 子表都得清空（用 namespace_id 直接数，避免依赖读路径先报错）
	for _, table := range []string{
		"capability_versions", "nodes", "node_capabilities", "node_instances", "node_pins",
		"edges", "sources", "source_targets", "events", "tasks", "graph_versions",
	} {
		if n := countRows(t, db, table, "namespace_id = ?", staging.ID); n != 0 {
			t.Errorf("force 删除后 %s 还剩 %d 行", table, n)
		}
	}
	// 默认命名空间不受影响（级联只清被删的那个）
	if n := countRows(t, db, "capability_versions", "namespace_id = ?", nsID); n == 0 {
		t.Fatal("删 staging 顺手清空了默认命名空间")
	}

	// 同名 slug 可以重建（UNIQUE(slug) 随行一起释放）
	if _, err := db.CreateNamespace(ctx, "staging", "预发布（重建）"); err != nil {
		t.Fatalf("删除后同名 slug 应可重建: %v", err)
	}
}

// TestNamespaceRequirementEnforced 空串与 "*" 一律 ErrNamespaceRequired。
//
// 为什么这条是整个 #162 最重要的一条：**绝不回退默认命名空间**。
// 回退会让「调用方忘了带 namespace」变成「悄悄写进了 default」，是最难查的串数据；
// "*" 只是 HTTP 读端点的汇总标记，store 层不认识它，必须原样报错。
func TestNamespaceRequirementEnforced(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, userID := newTestNS(t, db)
	// 造一套真实数据：若实现里存在「空 → 默认命名空间」的回退，下面每条都会
	// 变成「成功返回默认命名空间的数据」，而不是报错。
	capDef := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	if _, err := db.CreateNode(ctx, &Node{NamespaceID: nsID, Name: "mbp"}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: nsID, FromCapability: capDef.ID, FromPort: "fix", ToCapability: capDef.ID,
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	if err := db.CreateSource(ctx, &Source{
		NamespaceID: nsID, Name: "src", Kind: SourceKindManual, Enabled: true,
		Targets: []SourceTarget{{CapabilityID: capDef.ID}},
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := db.InsertEvent(ctx, &Event{NamespaceID: nsID, TraceID: "t1", Source: "github", Type: "push"}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := db.CreateTasks(ctx, []*Task{mkTask(nsID, "t1", capDef)}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	calls := []struct {
		name string
		call func(ns string) error
	}{
		{"ListCapabilities", func(ns string) error { _, err := db.ListCapabilities(ctx, ns); return err }},
		{"GetCapability", func(ns string) error { _, err := db.GetCapability(ctx, ns, "review", 1); return err }},
		{"GetCapabilityByID", func(ns string) error { _, err := db.GetCapabilityByID(ctx, ns, capDef.ID); return err }},
		{"NextCapabilityVersion", func(ns string) error { _, err := db.NextCapabilityVersion(ctx, ns, "review"); return err }},
		{"ListNodes", func(ns string) error { _, err := db.ListNodes(ctx, ns); return err }},
		{"ListEdges", func(ns string) error { _, err := db.ListEdges(ctx, ns, ""); return err }},
		{"EdgesFrom", func(ns string) error { _, err := db.EdgesFrom(ctx, ns, false, capDef.ID, "fix"); return err }},
		{"ListSources", func(ns string) error { _, err := db.ListSources(ctx, ns); return err }},
		{"ListEvents", func(ns string) error { _, err := db.ListEvents(ctx, ns, "", "", 10); return err }},
		{"ListTasks", func(ns string) error { _, err := db.ListTasks(ctx, ns, "", "", "", 10); return err }},
		{"GetEvent", func(ns string) error { _, err := db.GetEvent(ctx, ns, "evt_x"); return err }},
		{"GetTask", func(ns string) error { _, err := db.GetTask(ctx, ns, "tsk_x"); return err }},
		{"EnsureGraphVersion", func(ns string) error { _, err := db.EnsureGraphVersion(ctx, ns); return err }},
		{"MessageStats", func(ns string) error { _, err := db.MessageStats(ctx, ns, userID, 0); return err }},
		{"CreateEdge", func(ns string) error {
			return db.CreateEdge(ctx, &Edge{NamespaceID: ns, FromCapability: capDef.ID, FromPort: "fix", ToCapability: capDef.ID})
		}},
		{"CreateTasks", func(ns string) error {
			return db.CreateTasks(ctx, []*Task{mkTask(ns, "t2", capDef)})
		}},
		{"DeleteEdge", func(ns string) error { return db.DeleteEdge(ctx, ns, false, capDef.ID, "fix", capDef.ID) }},
		{"InsertEvent", func(ns string) error {
			_, err := db.InsertEvent(ctx, &Event{NamespaceID: ns, TraceID: "t3", Source: "github", Type: "push"})
			return err
		}},
	}
	for _, c := range calls {
		for _, ns := range []string{"", AllNamespaces} {
			err := c.call(ns)
			if !errors.Is(err, ErrNamespaceRequired) {
				t.Errorf("%s(ns=%q) err = %v, want ErrNamespaceRequired（绝不回退默认命名空间）",
					c.name, ns, err)
			}
		}
	}
}

// TestCrossNamespaceReferencesRejected I11 四处：跨命名空间引用一律拒绝。
//
// 为什么必须在 store 层拦：外键只保证「行存在」，不保证「同属一处」。
// 能力 id / 任务 id 全局唯一，把别处的 id 填进来在 SQL 层完全合法，
// 结构上防不住，只能显式校验。
func TestCrossNamespaceReferencesRejected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	staging := mkSiblingNamespace(t, db, "staging")
	nsB := staging.ID

	// 两边各一套定义（同名不同行）
	upA := mkCap(nsA, "review", 1, []string{"fix"})
	sinkA := mkCap(nsA, "fix", 1, nil)
	upB := mkCap(nsB, "review", 1, []string{"fix"})
	sinkB := mkCap(nsB, "fix", 1, nil)
	for _, c := range []*CapabilityVersion{upA, sinkA, upB, sinkB} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create %s@%d: %v", c.Name, c.Version, err)
		}
	}
	// 每行都带着自己的命名空间
	for _, c := range []*CapabilityVersion{upA, sinkA} {
		if c.NamespaceID != nsA {
			t.Fatalf("capability %s 落错命名空间: %+v", c.Name, c)
		}
	}
	for _, c := range []*CapabilityVersion{upB, sinkB} {
		if c.NamespaceID != nsB {
			t.Fatalf("capability %s 落错命名空间: %+v", c.Name, c)
		}
	}

	assertCross := func(label string, err error, want, got string) {
		t.Helper()
		var cross *CrossNamespaceError
		if !errors.As(err, &cross) {
			t.Fatalf("%s: err = %v (%T), want *CrossNamespaceError", label, err, err)
		}
		if cross.Want != want || cross.Got != got {
			t.Fatalf("%s: CrossNamespaceError = {want:%q got:%q}, 真实值 {want:%q got:%q}",
				label, cross.Want, cross.Got, want, got)
		}
	}

	// ===== ① CreateEdge：两端各一条 =====
	assertCross("edges.to_capability",
		db.CreateEdge(ctx, &Edge{NamespaceID: nsA, FromCapability: upA.ID, FromPort: "fix", ToCapability: sinkB.ID}),
		nsA, nsB)
	assertCross("edges.from_capability",
		db.CreateEdge(ctx, &Edge{NamespaceID: nsA, FromCapability: upB.ID, FromPort: "fix", ToCapability: sinkA.ID}),
		nsA, nsB)

	// ===== ② CreateSource / UpdateSource 的 targets =====
	assertCross("source_targets(create)",
		db.CreateSource(ctx, &Source{
			NamespaceID: nsA, Name: "cross-src", Kind: SourceKindManual, Enabled: true,
			Targets: []SourceTarget{{CapabilityID: sinkB.ID}},
		}), nsA, nsB)

	// 先建一个合法源，再把它改成指向别的命名空间的能力：建与改是两条独立路径，
	// 只在 CreateSource 拦 = 先建后改就能绕过这道水闸。
	okSrc := &Source{
		NamespaceID: nsA, Name: "ok-src", Kind: SourceKindManual, Enabled: true,
		Targets: []SourceTarget{{CapabilityID: sinkA.ID}},
	}
	if err := db.CreateSource(ctx, okSrc); err != nil {
		t.Fatalf("create valid source: %v", err)
	}
	cur, err := db.GetSource(ctx, nsA, okSrc.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	cur.Targets = []SourceTarget{{CapabilityID: sinkB.ID}}
	assertCross("source_targets(update)", db.UpdateSource(ctx, cur), nsA, nsB)
	// 被拒的更新不能半写：targets 保持原样
	after, err := db.GetSource(ctx, nsA, okSrc.ID)
	if err != nil {
		t.Fatalf("get source after reject: %v", err)
	}
	if len(after.Targets) != 1 || after.Targets[0].CapabilityID != sinkA.ID {
		t.Fatalf("被拒的更新改了 targets: %+v", after.Targets)
	}

	// ===== ③ ReconcileApplied：hello 里 applied[] 只在别的命名空间定义过的能力 =====
	// 语义是「按 name@version 在**本节点所在命名空间**解析」，所以别处的同名同版本
	// 定义在这里**根本不存在** → 归入 ErrUnknownCapability（ws 层翻 unknown_capability
	// 帧），而不是 CrossNamespaceError。
	// 为什么保留这个语义：节点 hello 帧里带的是 capability name@version，没有 id 维度，
	// 「这个能力属于别的命名空间」对节点来说与「这个能力不存在」不可区分。
	//
	// 这条断言在 v2 里换了入口（ConfirmCapabilityDecls → ReconcileApplied，旧的
	// manifest 形状整个消失），但**语义一个字没变**：隔离边界仍然是节点所在命名空间，
	// 越界的名字表现成「不存在」，而不是泄露「别处有这个能力」。
	nodeA, _ := mkNode(t, db, nsA, "node-a")
	// staging 里放一个默认命名空间没有的能力名
	onlyB := mkCap(nsB, "staging-only", 1, []string{"go"})
	if err := db.CreateCapability(ctx, onlyB); err != nil {
		t.Fatalf("create staging-only: %v", err)
	}
	_, err = db.ReconcileApplied(ctx, nsA, nodeA.ID,
		[]AppliedRef{{Name: "staging-only", Version: 1}})
	if !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("applied 里出现别处的能力 err = %v, want ErrUnknownCapability（语义：按本命名空间解析）", err)
	}
	var declErr *CapabilityDeclError
	if !errors.As(err, &declErr) || !declErr.UnknownCapability {
		t.Fatalf("err 不是 *CapabilityDeclError{UnknownCapability:true}: %T %+v", err, declErr)
	}
	var cross *CrossNamespaceError
	if errors.As(err, &cross) {
		t.Fatalf("「别处才有这个能力」被报成了 CrossNamespaceError（语义不对）: %v", err)
	}
	// 反证：同名能力在**本**命名空间定义后就能对账（说明上一条不是别的错）
	if err := db.CreateCapability(ctx, mkCap(nsA, "staging-only", 1, []string{"go"})); err != nil {
		t.Fatalf("create local staging-only: %v", err)
	}
	if _, err := db.ReconcileApplied(ctx, nsA, nodeA.ID,
		[]AppliedRef{{Name: "staging-only", Version: 1}}); err != nil {
		t.Fatalf("本命名空间里定义后仍对账失败: %v", err)
	}

	// ===== ④ CreateTasks：子任务继承父任务的命名空间 =====
	parent := mkTask(nsA, "trace_i11", upA)
	if err := db.CreateTasks(ctx, []*Task{parent}); err != nil {
		t.Fatalf("create parent task: %v", err)
	}
	parentID := parent.ID

	// 给了不同命名空间 → CrossNamespaceError（不静默改写）
	childCross := mkTask(nsB, "trace_i11", upB)
	childCross.ParentTaskID = &parentID
	assertCross("tasks.parent_task_id", db.CreateTasks(ctx, []*Task{childCross}), nsA, nsB)
	// 被拒的子任务不能落库
	if n := countRows(t, db, "tasks", "id = ?", childCross.ID); n != 0 {
		t.Fatalf("被拒的子任务还是落库了（id=%s）", childCross.ID)
	}

	// 不给命名空间 → 继承父任务的（调用方不必自己填，也不许填错）
	childInherit := mkTask("", "trace_i11", upA)
	childInherit.NamespaceID = ""
	childInherit.ParentTaskID = &parentID
	if err := db.CreateTasks(ctx, []*Task{childInherit}); err != nil {
		t.Fatalf("父任务同命名空间的子任务应被接受: %v", err)
	}
	got, err := db.GetTask(ctx, nsA, childInherit.ID)
	if err != nil {
		t.Fatalf("按父命名空间查子任务: %v", err)
	}
	if got.NamespaceID != nsA {
		t.Fatalf("子任务 namespace = %q, want 继承父任务的 %q", got.NamespaceID, nsA)
	}
}

// TestNamespaceRoundTrips 往返一致性（AGENTS.md §3）：七张表存什么读什么。
//
// 为什么专挑 namespace_id：它是 #162 的隔离维度，一旦读路径不 scan 它（或 scan 错列），
// 上层拿到的行会「看起来对」，但据此再做写操作就会跨命名空间——静默串数据。
// 手法：把行写进 **sandbox**（不是默认命名空间），读回来必须还是 sandbox。
func TestNamespaceRoundTrips(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	sandbox, err := db.CreateNamespace(ctx, "sandbox", "沙箱")
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	ns := sandbox.ID

	// capability
	capDef := mkCap(ns, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	gotCap, err := db.GetCapability(ctx, ns, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if gotCap.NamespaceID != ns {
		t.Fatalf("capability namespace = %q, want %q", gotCap.NamespaceID, ns)
	}

	// node
	node := &Node{NamespaceID: ns, Name: "sandbox-node", Tags: []string{"pool-b"}}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	gotNode, err := db.GetNode(ctx, ns, node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if gotNode.NamespaceID != ns {
		t.Fatalf("node namespace = %q, want %q", gotNode.NamespaceID, ns)
	}

	// source（+ 出口）
	src := &Source{
		NamespaceID: ns, Name: "sandbox-src", Kind: SourceKindManual, Enabled: true,
		Targets: []SourceTarget{{CapabilityID: capDef.ID}},
	}
	if err := db.CreateSource(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	gotSrc, err := db.GetSource(ctx, ns, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if gotSrc.NamespaceID != ns {
		t.Fatalf("source namespace = %q, want %q", gotSrc.NamespaceID, ns)
	}
	// source_targets 的 namespace_id 从父行（source）取
	if n := countRows(t, db, "source_targets", "source_id = ? AND namespace_id = ?", src.ID, ns); n != 1 {
		t.Fatalf("source_targets 的 namespace_id 不是从源行取的（匹配 %d 行）", n)
	}

	// event
	ev := &Event{NamespaceID: ns, TraceID: "trace_rt", Source: "github", Type: "push"}
	if _, err := db.InsertEvent(ctx, ev); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	gotEv, err := db.GetEvent(ctx, ns, ev.ID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if gotEv.NamespaceID != ns {
		t.Fatalf("event namespace = %q, want %q", gotEv.NamespaceID, ns)
	}

	// task
	task := mkTask(ns, "trace_rt", capDef)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	gotTask, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if gotTask.NamespaceID != ns {
		t.Fatalf("task namespace = %q, want %q", gotTask.NamespaceID, ns)
	}

	// edge
	if err := db.CreateEdge(ctx, &Edge{
		NamespaceID: ns, FromCapability: capDef.ID, FromPort: "fix", ToCapability: capDef.ID,
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	edges, err := db.ListEdges(ctx, ns, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 || edges[0].NamespaceID != ns {
		t.Fatalf("edge 往返不一致: %+v", edges)
	}

	// graph_version
	gv, err := db.EnsureGraphVersion(ctx, ns)
	if err != nil {
		t.Fatalf("ensure graph version: %v", err)
	}
	gotGV, err := db.GetGraphVersion(ctx, ns, gv.ID)
	if err != nil {
		t.Fatalf("get graph version: %v", err)
	}
	if gotGV.NamespaceID != ns {
		t.Fatalf("graph_version namespace = %q, want %q", gotGV.NamespaceID, ns)
	}
	// 对照组：默认命名空间里这些资源一个都看不到（写进 sandbox 的东西不能漏过来）
	if list, err := db.ListCapabilities(ctx, nsID); err != nil {
		t.Fatalf("list default capabilities: %v", err)
	} else {
		for _, c := range list {
			if c.NamespaceID != nsID {
				t.Fatalf("默认命名空间列出了 sandbox 的能力: %+v", c)
			}
		}
	}
}

// TestRegisterUserCreatesDefaultNamespace 注册 = 建用户 + default 命名空间 + 播种（契约 §2.5）。
//
// 代码里不存在「没有命名空间的分支」：用户建好了却没有命名空间，之后所有资源端点
// 都无处安放。所以 RegisterUser 必须同事务做完三件事；组织这一层已经删除（#205）。
func TestRegisterUserCreatesDefaultNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	u := &User{ID: NewUserID(), Username: "acme-" + NewUserID(), DisplayName: "注册用户", CreatedAt: Now()}
	ns, err := db.RegisterUser(ctx, u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	if ns.Slug != DefaultNamespaceSlug {
		t.Fatalf("默认命名空间 slug = %q, want %q", ns.Slug, DefaultNamespaceSlug)
	}
	if _, err := db.GetCapability(ctx, ns.ID, "notify.phone", 1); err != nil {
		t.Fatalf("默认命名空间没有播种内置能力: %v", err)
	}
	// 用户与命名空间都真的落库了
	if got, err := db.GetUserByUsername(u.Username); err != nil || got.ID != u.ID {
		t.Fatalf("注册后查不到用户: %+v, %v", got, err)
	}
	// 再注册一个 → 第二个 default 让位成 default-2（slug 全局唯一）
	u2 := &User{ID: NewUserID(), Username: "acme-" + NewUserID(), CreatedAt: Now()}
	ns2, err := db.RegisterUser(ctx, u2)
	if err != nil {
		t.Fatalf("register second user: %v", err)
	}
	if ns2.Slug != "default-2" {
		t.Fatalf("第二个默认命名空间 slug = %q, want default-2", ns2.Slug)
	}

	// DefaultNamespaceID 的两条分支：有命名空间 → 最早那个；空库 → ErrNamespaceNotFound
	first, err := db.DefaultNamespaceID(ctx)
	if err != nil {
		t.Fatalf("default namespace id: %v", err)
	}
	if first != ns.ID {
		t.Fatalf("DefaultNamespaceID = %q, want 最早注册的 %q", first, ns.ID)
	}
	emptyDB := newTestDB(t)
	if _, err := emptyDB.DefaultNamespaceID(ctx); !errors.Is(err, ErrNamespaceNotFound) {
		t.Errorf("空库 DefaultNamespaceID err = %v, want ErrNamespaceNotFound", err)
	}
}

// legacyPre12DB 造一个「迁移 11 时代」的老库并返回文件路径。
//
// 做法：用 openWith 注入一个**截断到迁移 11**的迁移集（现成的测试注入点，
// 不改生产代码），于是库的结构就是迁移 12 之前的样子（有 org_id、没有 namespace_id）。
// 然后按外键依赖顺序直插老形状的行——老库里的数据就是这样长出来的。
//
// withTask 决定要不要写一条任务行：tasks.capability_id 是**不带级联**的外键，
// 它的存在会让迁移 12 的「重建父表」路径暴露一个真实缺陷（见交付报告），
// 所以两条用例分开，便于定位。
func legacyPre12DB(t *testing.T, withTask bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy-ns.db")
	legacy, err := openWith(path, func(set *migrationSet) {
		var kept migrationSet
		for _, m := range *set {
			if m.version <= 11 {
				kept = append(kept, m)
			}
		}
		*set = kept
	})
	if err != nil {
		t.Fatalf("open legacy (pre-12) db: %v", err)
	}
	v, err := legacy.SchemaVersion()
	if err != nil || v != 11 {
		t.Fatalf("老库版本 = %d, %v；want 11", v, err)
	}
	if columnNames(t, legacy, "capability_versions")["namespace_id"] {
		t.Fatal("截断到迁移 11 的库不该有 namespace_id 列")
	}

	// 顺序按外键依赖：orgs → capabilities → nodes → 子表
	mustExec(t, legacy, `INSERT INTO orgs (id, name, created_at) VALUES (?, 'legacy', 1000)`, legacyOrgID)
	mustExec(t, legacy, `INSERT INTO capability_versions
		(id, org_id, name, display_name, version, params_schema, outputs, lifecycle, sticky, source, content_hash, created_at)
		VALUES (?, ?, 'review', '', 1, '{}', '["fix"]', '{}', '', 'custom', 'hash_legacy', 1000)`,
		legacyCapID, legacyOrgID)
	mustExec(t, legacy, `INSERT INTO nodes
		(id, org_id, name, tags, status, token_hash, max_instances, max_concurrency, created_at)
		VALUES (?, ?, 'legacy-node', '[]', 'offline', 'tokenhash', 20, 8, 1000)`, legacyNodeID, legacyOrgID)
	// 子表（迁移 12 里最容易丢的三张：它们的父表会被 DROP 重建）
	mustExec(t, legacy, `INSERT INTO node_capabilities (node_id, capability_id, run_spec)
		VALUES (?, ?, '{}')`, legacyNodeID, legacyCapID)
	mustExec(t, legacy, `INSERT INTO node_instances (id, node_id, slot, concurrency, alive, updated_at)
		VALUES (?, ?, 0, 1, 1, 1000)`, legacyInstID, legacyNodeID)
	mustExec(t, legacy, `INSERT INTO node_pins (sticky_key, org_id, node_id, selector, created_at, last_used_at)
		VALUES (?, ?, ?, 'pool-a', 1000, 1000)`, legacyPinID, legacyOrgID, legacyNodeID)
	mustExec(t, legacy, `INSERT INTO edges (org_id, from_is_source, from_key, from_port, to_capability, created_at)
		VALUES (?, 0, ?, 'fix', ?, 1000)`, legacyOrgID, legacyCapID, legacyCapID)
	mustExec(t, legacy, `INSERT INTO sources (id, org_id, name, kind, config, enabled, created_at)
		VALUES (?, ?, 'legacy-src', 'manual', '{}', 1, 1000)`, legacySrcID, legacyOrgID)
	mustExec(t, legacy, `INSERT INTO source_targets (source_id, capability_id, params, created_at)
		VALUES (?, ?, '{}', 1000)`, legacySrcID, legacyCapID)
	mustExec(t, legacy, `INSERT INTO events (id, org_id, trace_id, source, type, payload, created_at)
		VALUES (?, ?, 'trace_legacy', 'github', 'push', '{}', 1000)`, legacyEvtID, legacyOrgID)
	mustExec(t, legacy, `INSERT INTO graph_versions (id, org_id, content_hash, snapshot, created_at)
		VALUES (?, ?, 'graph_hash_legacy', '[]', 1000)`, legacyGrfID, legacyOrgID)
	if withTask {
		mustExec(t, legacy, `INSERT INTO tasks
			(id, org_id, trace_id, path, capability_id, capability, version, params, status, cap_hash, graph_id, created_at)
			VALUES (?, ?, 'trace_legacy', '[]', ?, 'review', 1, '{}', 'pending', 'hash_legacy', ?, 1000)`,
			legacyTaskID, legacyOrgID, legacyCapID, legacyGrfID)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}
	return path
}

// 老库夹具里固定的 ID（断言与插入共用同一份，避免打错字符串）。
const (
	legacyOrgID  = "org_legacy"
	legacyCapID  = "cap_legacy"
	legacyNodeID = "nd_legacy"
	legacyInstID = "nins_legacy"
	legacyPinID  = "pin_legacy"
	legacySrcID  = "src_legacy"
	legacyEvtID  = "evt_legacy"
	legacyGrfID  = "grf_legacy"
	legacyTaskID = "tsk_legacy"
)

// TestMigrationBackfillsDefaultNamespace 迁移 12：老数据回填 namespace_id 且一行都不丢。
//
// 这是升级路径的唯一门禁：e2e 全部跑在新库上，老库升级只有这条用例能拦住。
func TestMigrationBackfillsDefaultNamespace(t *testing.T) {
	t.Run("老数据回填默认命名空间且不丢行", func(t *testing.T) {
		path := legacyPre12DB(t, false)
		up, err := Open(path)
		if err != nil {
			// 迁移 12 当前实现有两条真实缺陷（已上报，见交付报告）：
			//  A. DROP 父表触发 ON DELETE CASCADE，把还没搬的子表行删掉（数据丢失）
			//  B. tasks.capability_id 是不带级联的外键 → 有任务历史的老库 COMMIT 直接失败
			// 这条断言就是它们的验收：修好后这里应当绿。
			t.Fatalf("迁移 12 升级老库失败（老库升级是 #162 的硬门禁）: %v", err)
		}
		defer up.Close()
		ctx := context.Background()

		nsID, err := up.DefaultNamespaceID(ctx)
		if err != nil {
			t.Fatalf("迁移 12 没给老组织建默认命名空间: %v", err)
		}
		ns, err := up.GetNamespace(ctx, nsID)
		if err != nil {
			t.Fatalf("get default namespace: %v", err)
		}
		if ns.Slug != DefaultNamespaceSlug {
			t.Fatalf("回填目标命名空间 slug = %q, want %q", ns.Slug, DefaultNamespaceSlug)
		}

		// ① 老数据必须还在（迁移只加列，不删行）
		dataIntact := true
		lost := func(format string, args ...any) {
			t.Helper()
			dataIntact = false
			t.Errorf(format, args...)
		}
		rows := []struct{ table, id string }{
			{"capability_versions", legacyCapID},
			{"nodes", legacyNodeID},
			{"node_instances", legacyInstID},
			{"sources", legacySrcID},
			{"events", legacyEvtID},
			{"graph_versions", legacyGrfID},
		}
		for _, r := range rows {
			if n := countRows(t, up, r.table, "id = ?", r.id); n != 1 {
				lost("迁移把老 %s 行 %s 弄丢了（剩 %d 行）", r.table, r.id, n)
			}
		}
		if n := countRows(t, up, "node_capabilities", "node_id = ? AND capability_id = ?", legacyNodeID, legacyCapID); n != 1 {
			lost("迁移把老 node_capabilities 行弄丢了（剩 %d 行）", n)
		}
		if n := countRows(t, up, "node_pins", "sticky_key = ?", legacyPinID); n != 1 {
			lost("迁移把老 node_pins 行弄丢了（剩 %d 行）", n)
		}
		if n := countRows(t, up, "edges", "from_key = ? AND to_capability = ?", legacyCapID, legacyCapID); n != 1 {
			lost("迁移把老 edges 行弄丢了（剩 %d 行）", n)
		}
		if n := countRows(t, up, "source_targets", "source_id = ? AND capability_id = ?", legacySrcID, legacyCapID); n != 1 {
			lost("迁移把老 source_targets 行弄丢了（剩 %d 行）", n)
		}
		if !dataIntact {
			// 行都没了，后面的「回填对不对」无从谈起——先让第一条失败说清问题
			// （否则一堆下游断言会把真正的原因埋掉）。
			return
		}

		// ② 每行的 namespace_id 都指向默认命名空间
		// （只挑一级资源表；子表的 namespace_id 从父行继承，见下面单独断言）
		topRows := []struct{ table, id string }{
			{"capability_versions", legacyCapID},
			{"nodes", legacyNodeID},
			{"sources", legacySrcID},
			{"events", legacyEvtID},
			{"graph_versions", legacyGrfID},
			{"edges", ""}, // edges 没有 id 列，按主键定位（见循环内的特殊处理）
		}
		for _, r := range topRows {
			var gotNS string
			q := `SELECT namespace_id FROM ` + r.table + ` WHERE id = ?`
			args := []any{r.id}
			if r.table == "edges" {
				q = `SELECT namespace_id FROM edges WHERE from_key = ? AND to_capability = ?`
				args = []any{legacyCapID, legacyCapID}
			}
			if err := up.QueryRow(q, args...).Scan(&gotNS); err != nil {
				t.Fatalf("read legacy %s: %v", r.table, err)
			}
			if gotNS != nsID {
				t.Errorf("老 %s 行的 namespace_id = %q, want %q（必须回填到默认命名空间）", r.table, gotNS, nsID)
			}
		}
		// 子表的 namespace_id 从父行取（不是按 org 猜）
		var instNS, pinNS, targetNS string
		if err := up.QueryRow(`SELECT namespace_id FROM node_instances WHERE id = ?`, legacyInstID).Scan(&instNS); err != nil {
			t.Fatalf("read instance ns: %v", err)
		}
		if err := up.QueryRow(`SELECT namespace_id FROM node_pins WHERE sticky_key = ?`, legacyPinID).Scan(&pinNS); err != nil {
			t.Fatalf("read pin ns: %v", err)
		}
		if err := up.QueryRow(`SELECT namespace_id FROM source_targets WHERE source_id = ?`, legacySrcID).Scan(&targetNS); err != nil {
			t.Fatalf("read target ns: %v", err)
		}
		for label, got := range map[string]string{
			"node_instances": instNS, "node_pins": pinNS, "source_targets": targetNS,
		} {
			if got != nsID {
				t.Errorf("老 %s 行的 namespace_id = %q, want %q（子表必须从父行继承）", label, got, nsID)
			}
		}
		// 没有任何行留着空 namespace_id（回填漏一列就是永久孤儿行）
		for _, table := range []string{
			"capability_versions", "nodes", "node_capabilities", "node_instances", "node_pins",
			"edges", "sources", "source_targets", "events", "tasks", "graph_versions",
		} {
			if n := countRows(t, up, table, "namespace_id IS NULL OR namespace_id = ''"); n != 0 {
				t.Errorf("%s 有 %d 行 namespace_id 为空（回填漏了）", table, n)
			}
		}

		// ③ 默认命名空间里播种了内置能力（否则迁移完连 end 节点都声明不出来）
		phone, err := up.GetCapability(ctx, nsID, "notify.phone", 1)
		if err != nil {
			t.Fatalf("默认命名空间没有播种 notify.phone（迁移 12 第三步漏了）: %v", err)
		}
		if phone.Source != CapSourceBuiltin || phone.NamespaceID != nsID {
			t.Fatalf("notify.phone = {%s, %s}, want {builtin, %s}", phone.Source, phone.NamespaceID, nsID)
		}
		// 老能力行也还在，且能被新读路径按命名空间读到
		oldCap, err := up.GetCapability(ctx, nsID, "review", 1)
		if err != nil {
			t.Fatalf("回填后老能力行查不到: %v", err)
		}
		if oldCap.ID != legacyCapID || oldCap.NamespaceID != nsID {
			t.Fatalf("老能力行 = %+v, want id=%s ns=%s", oldCap, legacyCapID, nsID)
		}
		// 快照内容不该被迁移改写
		var snap []any
		gv, err := up.GetGraphVersion(ctx, nsID, legacyGrfID)
		if err != nil {
			t.Fatalf("回填后老图快照查不到: %v", err)
		}
		if err := json.Unmarshal(gv.Snapshot, &snap); err != nil {
			t.Fatalf("老图快照被迁移改坏了: %v", err)
		}
	})

	t.Run("有任务历史的老库能升级", func(t *testing.T) {
		// tasks.capability_id 是不带级联的外键。迁移 12 重建父表
		// capability_versions 时，defer_foreign_keys 记下的「父行消失」在
		// 同名表重建后无法被清偿 → COMMIT 报 FOREIGN KEY constraint failed。
		// 任何**跑过任务**的老库都会踩到（也就是几乎所有真实部署）。
		path := legacyPre12DB(t, true)
		up, err := Open(path)
		if err != nil {
			t.Fatalf("有任务历史的老库升级失败（产品 bug，见交付报告）：%v", err)
		}
		defer up.Close()
		ctx := context.Background()
		nsID, err := up.DefaultNamespaceID(ctx)
		if err != nil {
			t.Fatalf("默认命名空间: %v", err)
		}
		// 老任务行必须还在，且按命名空间可读
		gotTask, err := up.GetTask(ctx, nsID, legacyTaskID)
		if err != nil {
			t.Fatalf("迁移后老任务行丢了: %v", err)
		}
		if gotTask.NamespaceID != nsID || gotTask.CapabilityID != legacyCapID {
			t.Fatalf("老任务读回 = %+v, want ns=%s cap=%s", gotTask, nsID, legacyCapID)
		}
	})
}
