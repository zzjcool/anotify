package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// 迁移 27 的验收（P2.2：link 改成按名字引用）。
//
// 为什么必须有一条「v26 老库升级」用例：e2e 全跑在新库上，而这次改动**改了主键**、
// 还重写了 `tasks.path` 里已存的历史。这类形状在新库的建表语句里根本造不出来
// （新库一路迁移到 27 时，edges 里还没有任何行）。
//
// 三条硬要求：① 存量 id 型边被重写成 `name@version`（钉死迁移前的行为，升级不会
// 偷偷改线）；② 入口边（from_is_source=1）的左端仍是**来源名**、一字不差；
// ③ 幂等——同一个 hook 重跑不炸、不改出第二份结果。

// legacyV26DB 造一个「迁移 26 时代」的库：edges 还是 (from_key, to_capability) 的 id 形状。
//
// 做法与 legacyV24DB 相同：用 openWith 注入截断到 26 的迁移集，于是库结构恰好是
// v26 的样子。能力/命名空间走真 store API（v26 的表都齐）；**edges 与 tasks 用直插
// SQL**——仓库里现在的 CreateEdge 写的是引用（编译期代码不分版本），只有直插才能
// 造出「老形状」的那两行。
func legacyV26DB(t *testing.T) (path, nsID string, from, to *CapabilityVersion) {
	t.Helper()
	ctx := context.Background()
	path = filepath.Join(t.TempDir(), "legacy-v26.db")
	old := openAtVersion(t, path, 26)
	if v, err := old.SchemaVersion(); err != nil || v != 26 {
		t.Fatalf("老库版本 = %d, %v; want 26", v, err)
	}
	// 前置条件：夹具必须真的是「迁移 27 之前」的形状，否则这条用例测的就不是老库升级。
	cols := columnNames(t, old, "edges")
	if !cols["from_key"] || !cols["to_capability"] {
		t.Fatal("v26 库里 edges 没有 (from_key, to_capability)，夹具不对")
	}
	if cols["from_ref"] || cols["to_ref"] {
		t.Fatal("v26 库里 edges 已经有 from_ref/to_ref 列，夹具不对（迁移 27 没东西可测）")
	}

	ns, err := old.RegisterUser(ctx, &User{
		ID: NewUserID(), Username: "u-" + NewUserID(), DisplayName: "老库用户", CreatedAt: Now(),
	})
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	nsID = ns.ID

	from = &CapabilityVersion{
		NamespaceID: nsID, Name: "review", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"fix"},
		Source:       CapSourceCustom,
	}
	to = &CapabilityVersion{
		NamespaceID: nsID, Name: "notify", Version: 2,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Source:       CapSourceCustom,
	}
	for _, c := range []*CapabilityVersion{from, to} {
		if err := old.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create capability: %v", err)
		}
	}

	// ① 一条能力边：两端都是 id（P2.2 之前的唯一形状）
	mustExec(t, old, `INSERT INTO edges
		(namespace_id, from_is_source, from_key, from_port, to_capability, literals, mapping, created_at)
		VALUES (?, 0, ?, 'fix', ?, '{"to":"u_1"}', '{}', 1000)`,
		nsID, from.ID, to.ID)
	// ② 一条入口边：左端本来就是来源名（必须原样留下）
	mustExec(t, old, `INSERT INTO edges
		(namespace_id, from_is_source, from_key, from_port, to_capability, literals, mapping, created_at)
		VALUES (?, 1, 'github', '*', ?, '{}', '{"title":"{{pull_request.title}}"}', 1001)`,
		nsID, to.ID)
	// ③ 一条老任务：path 里存的是**能力 id**（迁移要换成名字）
	mustExec(t, old, `INSERT INTO tasks
		(id, namespace_id, trace_id, parent_task_id, path, capability_id, capability, version,
		 params, status, attempts, reported, summary, artifacts, error, cap_hash, graph_id, created_at)
		VALUES ('tsk_legacy_path', ?, 'trc_legacy', NULL, ?, ?, 'review', 1,
		 '{}', 'done', 1, 1, '', '[]', '', 'hash_review', 'grf_legacy', 1002)`,
		nsID, `["`+from.ID+`"]`, from.ID)

	if err := old.Close(); err != nil {
		t.Fatalf("close v26 db: %v", err)
	}
	return path, nsID, from, to
}

// TestMigration27EdgesBecomeRefs 迁移 27：id → `name@version`，入口边不动，path 换成名字，幂等。
func TestMigration27EdgesBecomeRefs(t *testing.T) {
	path, nsID, from, _ := legacyV26DB(t)

	up, err := Open(path)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	defer func() { _ = up.Close() }()
	ctx := context.Background()

	// 列换掉了（from_key/to_capability 不在，from_ref/to_ref 在）
	cols := columnNames(t, up, "edges")
	if cols["from_key"] || cols["to_capability"] {
		t.Fatalf("迁移 27 之后 edges 还留着 id 列：%v", cols)
	}
	if !cols["from_ref"] || !cols["to_ref"] {
		t.Fatalf("迁移 27 之后 edges 没有 from_ref/to_ref：%v", cols)
	}

	// ① 能力边：两端重写成 `name@version`（钉死迁移前指向的那一版）
	var fromRef, fromPort, toRef, literals string
	if err := up.QueryRow(`SELECT from_ref, from_port, to_ref, literals FROM edges
	                         WHERE namespace_id = ? AND from_is_source = 0`, nsID).
		Scan(&fromRef, &fromPort, &toRef, &literals); err != nil {
		t.Fatalf("读能力边: %v", err)
	}
	if fromRef != FormatRef("review", 1) || fromPort != "fix" || toRef != FormatRef("notify", 2) {
		t.Fatalf("能力边 = (%q --%s--> %q), want (%q --fix--> %q)",
			fromRef, fromPort, toRef, FormatRef("review", 1), FormatRef("notify", 2))
	}
	// literals 一字不差（迁移只动引用列）
	if literals != `{"to":"u_1"}` {
		t.Fatalf("literals = %q，被迁移改写了", literals)
	}
	// 引用能解析（解析不出来 = 迁移把边变成了死行）
	if _, err := up.ResolveRef(ctx, nsID, fromRef); err != nil {
		t.Fatalf("迁移后的 from_ref 解析不到: %v", err)
	}
	if _, err := up.ResolveRef(ctx, nsID, toRef); err != nil {
		t.Fatalf("迁移后的 to_ref 解析不到: %v", err)
	}

	// ② 入口边：左端是来源名（不变），右端同样是引用
	var entryFrom, entryTo, entryMapping string
	if err := up.QueryRow(`SELECT from_ref, to_ref, mapping FROM edges
	                         WHERE namespace_id = ? AND from_is_source = 1`, nsID).
		Scan(&entryFrom, &entryTo, &entryMapping); err != nil {
		t.Fatalf("读入口边: %v", err)
	}
	if entryFrom != "github" {
		t.Fatalf("入口边左端 = %q, want github（来源名不变）", entryFrom)
	}
	if entryTo != FormatRef("notify", 2) {
		t.Fatalf("入口边右端 = %q, want %q", entryTo, FormatRef("notify", 2))
	}
	if entryMapping != `{"title":"{{pull_request.title}}"}` {
		t.Fatalf("入口边 mapping = %q，被迁移改写了", entryMapping)
	}

	// ③ tasks.path：能力 id → 能力名（环剪枝按名字判）
	var pathJSON string
	if err := up.QueryRow(`SELECT path FROM tasks WHERE id = 'tsk_legacy_path'`).Scan(&pathJSON); err != nil {
		t.Fatalf("读老任务 path: %v", err)
	}
	var elems []string
	if err := json.Unmarshal([]byte(pathJSON), &elems); err != nil {
		t.Fatalf("老任务 path 不是 JSON 数组: %q", pathJSON)
	}
	if len(elems) != 1 || elems[0] != "review" {
		t.Fatalf("老任务 path = %v, want [review]", elems)
	}

	// ④ 幂等：同一个 hook 重跑（迁移框架可能因重试重复执行）→ 不炸、不改结果
	tx, err := up.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if err := migrateEdgeRefs(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("第 %d 次 migrateEdgeRefs: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var n int
	if err := up.QueryRow(`SELECT count(*) FROM edges WHERE namespace_id = ?`, nsID).Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 2 {
		t.Fatalf("重跑后 edges = %d 行, want 2（幂等：不该重复建行）", n)
	}
	if err := up.QueryRow(`SELECT from_ref, to_ref FROM edges
	                         WHERE namespace_id = ? AND from_is_source = 0`, nsID).
		Scan(&fromRef, &toRef); err != nil {
		t.Fatalf("重跑后读能力边: %v", err)
	}
	if fromRef != FormatRef("review", 1) || toRef != FormatRef("notify", 2) {
		t.Fatalf("重跑后能力边 = (%q → %q)，被改坏了", fromRef, toRef)
	}
	// path 也没被二次改写
	if err := up.QueryRow(`SELECT path FROM tasks WHERE id = 'tsk_legacy_path'`).Scan(&pathJSON); err != nil {
		t.Fatalf("重跑后读 path: %v", err)
	}
	if pathJSON != `["review"]` {
		t.Fatalf("重跑后 path = %q, want [\"review\"]", pathJSON)
	}

	// ⑤ 升级后的边仍然能被派发查到（迁移没把线弄断）
	targets, err := up.EdgesFrom(ctx, nsID, false, from.Name, "fix")
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(targets) != 1 || targets[0].ToRef != FormatRef("notify", 2) {
		t.Fatalf("迁移后 EdgesFrom = %+v, want 一条到 %q", targets, FormatRef("notify", 2))
	}
}
