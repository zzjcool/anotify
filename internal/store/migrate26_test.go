package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// 迁移 25 / 26 的验收（P2.1：源收敛为入口边）。
//
// 为什么要单独一条「老库升级」用例：e2e 全跑在新库上，而这次改动**删掉了一张表**。
// 老库升级只有这类用例能拦住——表还在、列还没加、数据还在 source_targets 里，
// 这类形状在 tip 的建表语句里根本造不出来。

// legacyV24DB 造一个「迁移 24 时代」的库（有 source_targets、edges 还没有 mapping 列）。
//
// 做法与 legacyPre12DB 相同：用 openWith 注入一个截断到 24 的迁移集，于是库结构恰好
// 是 v24 的样子。用户 / 命名空间 / 能力走真 store API（v24 的表都齐）；**源与它的出口
// 用直插 SQL**——v24 的代码会走 source_targets，而仓库里现在的 CreateSource 写的是
// 入口边（编译期代码不分版本），只有直插才能造出"老形状"。
func legacyV24DB(t *testing.T) (path, nsID string, capA, capB *CapabilityVersion, srcs map[string]string) {
	t.Helper()
	ctx := context.Background()
	path = filepath.Join(t.TempDir(), "legacy-v24.db")
	old := openAtVersion(t, path, 24)
	if v, err := old.SchemaVersion(); err != nil || v != 24 {
		t.Fatalf("老库版本 = %d, %v; want 24", v, err)
	}
	// 前置条件必须成立，否则这条用例测的就不是"老库升级"了：
	if !tableExists(t, old, "source_targets") {
		t.Fatal("v24 库里没有 source_targets，夹具不对")
	}
	if columnNames(t, old, "edges")["mapping"] {
		t.Fatal("v24 库里 edges 已经有 mapping 列，夹具不对（迁移 25 就没东西可测了）")
	}

	ns, err := old.RegisterUser(ctx, &User{
		ID: NewUserID(), Username: "u-" + NewUserID(), DisplayName: "老库用户", CreatedAt: Now(),
	})
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	nsID = ns.ID

	// 目标能力带 params_schema：搬过来的模板必须有字段可落（迁移不校验，但读回要能对上）。
	capA = &CapabilityVersion{
		NamespaceID: nsID, Name: "notify", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"},"n":{"type":"integer"}}}`),
		Source:       CapSourceCustom,
	}
	capB = &CapabilityVersion{
		NamespaceID: nsID, Name: "archive", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Source:       CapSourceCustom,
	}
	for _, c := range []*CapabilityVersion{capA, capB} {
		if err := old.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create capability: %v", err)
		}
	}

	// 三个源：① 两个出口 + 模板；② 一个出口 + 坏 params（手改过的脏数据）；
	// ③ 一个出口（回归"没有模板"的默认 '{}'）。
	srcs = map[string]string{"hook": NewSourceID(), "dirty": NewSourceID(), "plain": NewSourceID()}
	for name, id := range srcs {
		mustExec(t, old, `INSERT INTO sources (id, namespace_id, name, kind, config, enabled, created_at)
			VALUES (?, ?, ?, 'manual', '{}', 1, 1000)`, id, nsID, name)
	}
	mustExec(t, old, `INSERT INTO source_targets (source_id, capability_id, namespace_id, params, created_at)
		VALUES (?, ?, ?, ?, 1000)`, srcs["hook"], capA.ID, nsID,
		`{"title":"{{pull_request.title}}","n":"{{pull_request.number}}"}`)
	mustExec(t, old, `INSERT INTO source_targets (source_id, capability_id, namespace_id, params, created_at)
		VALUES (?, ?, ?, '{}', 1001)`, srcs["hook"], capB.ID, nsID)
	mustExec(t, old, `INSERT INTO source_targets (source_id, capability_id, namespace_id, params, created_at)
		VALUES (?, ?, ?, 'not json', 1002)`, srcs["dirty"], capA.ID, nsID)
	mustExec(t, old, `INSERT INTO source_targets (source_id, capability_id, namespace_id, params, created_at)
		VALUES (?, ?, ?, '{}', 1003)`, srcs["plain"], capB.ID, nsID)

	// 一条普通能力边：搬完之后它的 mapping 必须是 '{}'（能力边不允许模板）。
	mustExec(t, old, `INSERT INTO edges (namespace_id, from_is_source, from_key, from_port, to_capability, literals, created_at)
		VALUES (?, 0, ?, 'ok', ?, '{}', 1000)`, nsID, capB.ID, capA.ID)

	if err := old.Close(); err != nil {
		t.Fatalf("close v24 db: %v", err)
	}
	return path, nsID, capA, capB, srcs
}

// TestMigration26SourceTargetsBecomeEntryEdges 迁移 25 + 26：老库升到 tip。
//
// 四件事，缺一条就是线上事故：
//  1. 每一行 source_targets 都搬成一条入口边（from_is_source=1、from_key=来源名、
//     from_port='*'），mapping 一字不差地跟着搬过来；
//  2. 坏 params 退化成 '{}'（不能让一行脏数据把整张图的 hash 打爆）；
//  3. source_targets 表**不存在**了（收敛成一份真相，不留双写）；
//  4. 幂等：迁移重跑（hook 再跑一次 / 再开一次库）不炸、不重复建边。
func TestMigration26SourceTargetsBecomeEntryEdges(t *testing.T) {
	path, nsID, capA, capB, srcs := legacyV24DB(t)

	up, err := Open(path)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	ctx := context.Background()

	// ③ 表被删了
	if tableExists(t, up, "source_targets") {
		t.Fatal("迁移 26 之后 source_targets 仍然存在（还留着旁路）")
	}
	// 迁移 25：列加上了
	if !columnNames(t, up, "edges")["mapping"] {
		t.Fatal("迁移 25 之后 edges 仍然没有 mapping 列")
	}

	// ① 每个出口一条入口边，mapping 原样搬过来
	//
	// 右端是**引用**（迁移 27 把 id 重写成 `name@version`，钉死迁移前的行为），
	// 所以这里按 (name, version) 找行，而不是按 id。
	checkEdge := func(srcName string, cap *CapabilityVersion, wantMapping string) {
		t.Helper()
		var got string
		if err := up.QueryRow(
			`SELECT mapping FROM edges
			  WHERE namespace_id = ? AND from_is_source = 1 AND from_ref = ? AND from_port = '*' AND to_ref = ?`,
			nsID, srcName, FormatRef(cap.Name, cap.Version)).Scan(&got); err != nil {
			t.Fatalf("源 %s → %s 的入口边没建出来: %v", srcName, capRef(cap), err)
		}
		if got != wantMapping {
			t.Errorf("源 %s → %s 的 mapping = %q, want %q", srcName, capRef(cap), got, wantMapping)
		}
	}
	checkEdge("hook", capA, `{"title":"{{pull_request.title}}","n":"{{pull_request.number}}"}`)
	checkEdge("hook", capB, "{}")
	checkEdge("plain", capB, "{}")
	// ② 坏 params → '{}'（不是把坏字节搬过去）
	checkEdge("dirty", capA, "{}")

	// 能力边的 mapping 必须是 '{}'（迁移不会给能力边塞模板）
	var capEdgeMapping string
	// P2.2（迁移 27）：能力边的两端是引用（`name@version`，钉死迁移前的行为）。
	if err := up.QueryRow(`SELECT mapping FROM edges WHERE from_is_source = 0 AND from_ref = ?`,
		FormatRef(capB.Name, capB.Version)).Scan(&capEdgeMapping); err != nil {
		t.Fatalf("能力边丢了: %v", err)
	}
	if capEdgeMapping != EdgeMappingEmpty {
		t.Errorf("能力边的 mapping = %q, want %q（能力边不允许模板）", capEdgeMapping, EdgeMappingEmpty)
	}

	// 读路径：GetSource 的 targets 从入口边读回来（参数 = mapping）
	got, err := up.GetSource(ctx, nsID, srcs["hook"])
	if err != nil {
		t.Fatalf("get source after migration: %v", err)
	}
	if len(got.Targets) != 2 {
		t.Fatalf("源的出口数 = %d, want 2: %+v", len(got.Targets), got.Targets)
	}
	byID := map[string]SourceTarget{}
	for _, tg := range got.Targets {
		byID[tg.CapabilityID] = tg
	}
	if p := byID[capA.ID].Params; p["title"] != "{{pull_request.title}}" || p["n"] != "{{pull_request.number}}" {
		t.Errorf("搬过来的模板读回不对: %+v", p)
	}
	if p := byID[capB.ID].Params; len(p) != 0 {
		t.Errorf("没有模板的出口读回来不该有参数: %+v", p)
	}

	// ④ 幂等
	//  (a) 同一版本重试：hook 直接再跑一次（表已不在 → no-op，不能报错）
	tx, err := up.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateSourceTargetsToEntryEdges(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("迁移 26 重跑报错（幂等破了）: %v", err)
	}
	if err := migrateEdgeMapping(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("迁移 25 重跑报错（幂等破了）: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	//  (b) 再开一次库：不重复应用、也不重复建边
	if err := up.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("第二次打开失败: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	// 4 条源目标全搬完 = 4 条入口边；重开后必须还是 4 条（不是 8 条）
	if n := countRows(t, reopened, "edges", "from_is_source = 1"); n != 4 {
		t.Fatalf("入口边行数 = %d, want 4（重跑不该重复建边）", n)
	}
}
