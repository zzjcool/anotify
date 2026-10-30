package store

// 迁移 14（drop_org_layer）的验收测试：老库升级路径的唯一门禁。
//
// e2e 全部跑在新库上，只有这一组用例能拦住「老库升级丢数据 / 撞唯一键 / 不幂等」。
// 三条硬要求（#205）：
//  1. org 相关的东西消失（orgs / org_members 表、所有表上的 org_id 列）；
//  2. 业务行一行不少（按表逐个比行数，升级前 vs 升级后）；
//  3. 幂等：同一版本重试不报错、不变更数据。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// openAtVersion 用 openWith 的测试注入点把迁移集截断到 maxVersion，
// 于是库的结构就停在那一个版本（老库升级测试用）。
func openAtVersion(t *testing.T, path string, maxVersion int) *DB {
	t.Helper()
	db, err := openWith(path, func(set *migrationSet) {
		var kept migrationSet
		for _, m := range *set {
			if m.version <= maxVersion {
				kept = append(kept, m)
			}
		}
		*set = kept
	})
	if err != nil {
		t.Fatalf("open at version %d: %v", maxVersion, err)
	}
	return db
}

// orgLayerTables 是迁移 14 会重建的全部表（业务表 + 命名空间 + 预案）。
var orgLayerTables = []string{
	"namespaces", "capability_versions", "nodes", "node_capabilities", "node_instances",
	"node_pins", "edges", "sources", "source_targets", "events", "tasks", "graph_versions",
	"playbooks",
}

// tableRowCounts 数一遍全部业务表的行数（升级前后比对用）。
func tableRowCounts(t *testing.T, db *DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range orgLayerTables {
		out[table] = countRows(t, db, table, "1 = 1")
	}
	return out
}

// tableContentDigest 对若干列的全部行做 sha256（按列排序，结果稳定）。
//
// 用来断言「迁移只是去掉 org_id，行内容一个字节都没变」。
func tableContentDigest(t *testing.T, db *DB, table string, cols []string) string {
	t.Helper()
	order := strings.Join(cols, ", ")
	q := `SELECT ` + strings.Join(cols, ", ") + ` FROM ` + table + ` ORDER BY ` + order
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("digest %s: %v", table, err)
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("digest scan %s: %v", table, err)
		}
		fmt.Fprintf(h, "%v\n", vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("digest rows %s: %v", table, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestMigration14ColumnReconciliation 迁移 14 的列清单必须等于「v13 真实列 − org_id」。
//
// 为什么必须有这一条（实测得出的教训）：回灌 SQL 的列清单和 stage 的 SELECT 用的是
// **同一个** cols，漏一列不会报错 —— 新表那一列会安静地取 DEFAULT。
// 把 nodes.cols 里的 tags 删掉后，迁移照样成功、行数照样相等，只是 tags 全变成 []。
// 这类回归只能靠「拿老库的真实列清单逐表对账」拦住。
func TestMigration14ColumnReconciliation(t *testing.T) {
	path := legacyPre12DB(t, false)
	db13 := openAtVersion(t, path, 13)
	defer func() { _ = db13.Close() }()

	for _, rb := range orgLayerRebuilds {
		actual := columnNames(t, db13, rb.table) // 迁移 13 时点的真实列
		want := map[string]bool{}
		for name := range actual {
			if name == "org_id" {
				continue // 迁移 14 就是要摘掉它
			}
			want[name] = true
		}
		got := map[string]bool{}
		for _, c := range rb.cols {
			if got[c] {
				t.Errorf("%s: cols 里 %q 重复", rb.table, c)
			}
			got[c] = true
			if !want[c] {
				t.Errorf("%s: cols 里的 %q 在 v13 里不存在（拼错列名会让回灌失败）", rb.table, c)
			}
		}
		for name := range want {
			if !got[name] {
				t.Errorf("%s: 漏了列 %q —— 回灌会静默取 DEFAULT（丢数据）", rb.table, name)
			}
		}
		if len(rb.cols) != len(want) {
			t.Errorf("%s: cols 数量 %d, v13 去掉 org_id 后 %d", rb.table, len(rb.cols), len(want))
		}
		// 新表 DDL 里声明的列也必须与 cols 一致（防止 create 与 cols 各自漂移）
		for _, line := range strings.Split(rb.create, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "CREATE") || strings.HasPrefix(trimmed, ")") ||
				strings.HasPrefix(trimmed, "UNIQUE") || strings.HasPrefix(trimmed, "PRIMARY KEY") {
				continue
			}
			name := strings.Fields(trimmed)[0]
			if !got[name] {
				t.Errorf("%s: 新表 DDL 声明了 %q，但 cols 里没有（回灌不会填它）", rb.table, name)
			}
		}
	}
}

// TestMigration14RemovesOrgLayer 新库（= 迁移链跑完）里不该再有任何 org 痕迹。
func TestMigration14RemovesOrgLayer(t *testing.T) {
	db := newTestDB(t)

	// ① 两张表消失
	for _, table := range []string{"orgs", "org_members"} {
		if tableExists(t, db, table) {
			t.Errorf("迁移 14 之后仍存在表 %s", table)
		}
	}
	// ② 任何表都不能再有 org_id 列
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan table: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("没有读到任何表")
	}
	// 注意：DSN 是 MaxOpenConns(1)，上面那轮 rows 必须读完/关闭后才能再查
	// （否则会自己把唯一连接占住，测试直接挂死）。rows 已在上面的循环里读完。
	for _, table := range tables {
		if columnNames(t, db, table)["org_id"] {
			t.Errorf("表 %s 仍有 org_id 列（迁移 14 漏了）", table)
		}
	}

	// ③ 迁移 14 已记账（且只记一次）
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_version WHERE version = 14`).Scan(&n); err != nil {
		t.Fatalf("count version 14: %v", err)
	}
	if n != 1 {
		t.Fatalf("schema_version 里 14 记了 %d 次, want 1", n)
	}

	// ④ namespaces 的唯一域是全局 slug：显式插重复 slug 必须被拒
	//（先注册一个用户，让 `default` 这一个 slug 真的被占用）
	newTestNS(t, db)
	_, err = db.Exec(`INSERT INTO namespaces (id, slug, name, created_at) VALUES ('ns_dup','default','重复',1)`)
	if err == nil {
		t.Fatal("namespaces 仍然接受重复 slug（UNIQUE(slug) 没生效）")
	}
	if !isUniqueConstraintError(err) {
		t.Fatalf("重复 slug 的错误不是唯一约束: %v", err)
	}
}

// TestMigration14Idempotent 同一版本重试多次：不报错、不变更数据。
func TestMigration14Idempotent(t *testing.T) {
	db, path := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	capDef := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	if _, err := db.CreateNode(ctx, &Node{NamespaceID: nsID, Name: "mbp"}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.InsertEvent(ctx, &Event{NamespaceID: nsID, TraceID: "trace_14", Source: "github", Type: "push"}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	before := tableRowCounts(t, db)

	// ① 同一个事务内连续跑两次 hook：「同一版本可能因重试执行多次」。
	//
	// 注意这一条钉住的是**幂等守卫本身**（第二次调用在 hasTable(orgs)==false 处返回），
	// 不是「重建逻辑可重入」——真正的重放安全来自「整块迁移在一个事务里，失败即全量回滚」，
	// 那条由 migrate_test.go 的 TestMigrationFailureRollsBack 与手工验证（老库孤儿行
	// 导致 commit 失败 → 回滚 → 修数据后重放成功）覆盖。
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateDropOrgLayer(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("第一次 migrateDropOrgLayer: %v", err)
	}
	if err := migrateDropOrgLayer(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("重试 migrateDropOrgLayer（幂等性破了）: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// ② 关掉再打开两次：迁移不重复应用、行数不变
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for i := 0; i < 2; i++ {
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次重开: %v", i+1, err)
		}
		after := tableRowCounts(t, reopened)
		for table, want := range before {
			if after[table] != want {
				t.Errorf("重开后 %s 行数 %d → %d（迁移不该改数据）", table, want, after[table])
			}
		}
		var dup int
		if err := reopened.QueryRow(`SELECT count(*) - count(DISTINCT version) FROM schema_version`).Scan(&dup); err != nil {
			t.Fatalf("count duplicates: %v", err)
		}
		if dup != 0 {
			t.Errorf("schema_version 有 %d 行重复记账", dup)
		}
		if err := reopened.Close(); err != nil {
			t.Fatalf("close reopened: %v", err)
		}
	}
}

// TestMigration14PreservesRows 老库（迁移 11 时代的 schema）升级：
// 业务行一行不少，且 org 痕迹消失。
func TestMigration14PreservesRows(t *testing.T) {
	// 复用 namespace_test.go 的老库夹具（迁移 11 时代的形状，含 org_id 列）
	path := legacyPre12DB(t, true)

	// 阶段 A：先升到 13（迁移 12 跑完、迁移 14 还没跑），记下行数
	db13 := openAtVersion(t, path, 13)
	if v, err := db13.SchemaVersion(); err != nil || v != 13 {
		t.Fatalf("阶段 A 版本 = %d, %v; want 13", v, err)
	}
	// 此时 org 还在（对照：迁移 14 之后必须消失）
	if !tableExists(t, db13, "orgs") || !columnNames(t, db13, "capability_versions")["org_id"] {
		t.Fatal("阶段 A 就看不到 org 痕迹，夹具不对")
	}
	before := tableRowCounts(t, db13)
	// 内容摘要：行数相等不等于数据没坏（列漏了/顺序错了都可能行数不变）
	digestCols := map[string][]string{}
	for _, rb := range orgLayerRebuilds {
		digestCols[rb.table] = rb.cols
	}
	beforeDigest := map[string]string{}
	for _, table := range []string{"capability_versions", "nodes", "node_capabilities", "node_pins", "edges", "sources", "events", "tasks", "graph_versions", "playbooks"} {
		beforeDigest[table] = tableContentDigest(t, db13, table, digestCols[table])
	}
	if err := db13.Close(); err != nil {
		t.Fatalf("close stage A: %v", err)
	}

	// 阶段 B：完整迁移到 14
	up, err := Open(path)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	t.Cleanup(func() { _ = up.Close() })
	after := tableRowCounts(t, up)
	for table, want := range before {
		if after[table] != want {
			t.Errorf("升级后 %s 行数 %d → %d（迁移 14 丢了数据）", table, want, after[table])
		}
	}
	for table, want := range beforeDigest {
		if got := tableContentDigest(t, up, table, digestCols[table]); got != want {
			t.Errorf("升级后 %s 的行内容变了（摘要 %s → %s）：迁移 14 只该去掉 org_id，不该改数据",
				table, want[:12], got[:12])
		}
	}
	// org 痕迹消失
	if tableExists(t, up, "orgs") || tableExists(t, up, "org_members") {
		t.Fatal("升级后 orgs / org_members 还在")
	}
	for _, table := range orgLayerTables {
		if columnNames(t, up, table)["org_id"] {
			t.Errorf("升级后 %s 仍有 org_id 列", table)
		}
	}
	// 老数据仍按命名空间读得到
	nsID, err := up.DefaultNamespaceID(context.Background())
	if err != nil {
		t.Fatalf("default namespace: %v", err)
	}
	if _, err := up.GetCapability(context.Background(), nsID, "review", 1); err != nil {
		t.Fatalf("升级后老能力查不到: %v", err)
	}
}

// TestMigration14DedupesNamespaceSlugs 老库里**多个组织各有 `default`** 时，
// 迁移 14 必须把 slug 全局唯一化（否则 UNIQUE(slug) 撞车，升级直接失败）。
//
// 这是迁移里最容易出事的一段：迁移 12 的唯一键是 (org_id, slug)，所以老库
// 每个组织都能有一个 `default`；迁移 14 之后唯一域变成全局的 (slug)。
func TestMigration14DedupesNamespaceSlugs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-two-orgs.db")

	// 阶段 A：造「迁移 11 时代」的库 —— 两个组织，各自一套数据
	pre := openAtVersion(t, path, 11)
	if v, err := pre.SchemaVersion(); err != nil || v != 11 {
		t.Fatalf("老库版本 = %d, %v; want 11", v, err)
	}
	const (
		orgA, orgB = "org_a", "org_b"
		capA, capB = "cap_a", "cap_b"
		nodeA      = "nd_a"
	)
	// 名字唯一（迁移 4 里 orgs.name 是 UNIQUE）
	mustExec(t, pre, `INSERT INTO orgs (id, name, created_at) VALUES (?, 'org-a', 1000)`, orgA)
	mustExec(t, pre, `INSERT INTO orgs (id, name, created_at) VALUES (?, 'org-b', 2000)`, orgB)
	// 两个组织各一个同名能力（老唯一键是 (org_id, name, version) → 合法）
	for _, r := range []struct{ org, cap string }{{orgA, capA}, {orgB, capB}} {
		mustExec(t, pre, `INSERT INTO capability_versions
			(id, org_id, name, display_name, version, params_schema, outputs, lifecycle, sticky, source, content_hash, created_at)
			VALUES (?, ?, 'review', '', 1, '{}', '["fix"]', '{}', '', 'custom', ?, 1000)`,
			r.cap, r.org, "hash_"+r.cap)
	}
	mustExec(t, pre, `INSERT INTO nodes
		(id, org_id, name, tags, status, token_hash, max_instances, max_concurrency, created_at)
		VALUES (?, ?, 'node-a', '[]', 'offline', 'th', 20, 8, 1000)`, nodeA, orgA)
	mustExec(t, pre, `INSERT INTO edges (org_id, from_is_source, from_key, from_port, to_capability, created_at)
		VALUES (?, 0, ?, 'fix', ?, 1000)`, orgA, capA, capA)
	mustExec(t, pre, `INSERT INTO events (id, org_id, trace_id, source, type, payload, created_at)
		VALUES ('evt_b', ?, 'trace_b', 'github', 'push', '{}', 1000)`, orgB)
	if err := pre.Close(); err != nil {
		t.Fatalf("close pre: %v", err)
	}

	// 阶段 B：升到 13（迁移 12 给每个 org 建一个 `default`，此时 (org_id, slug) 合法）
	db13 := openAtVersion(t, path, 13)
	var dupCount int
	if err := db13.QueryRow(`SELECT count(*) FROM namespaces WHERE slug = ?`, DefaultNamespaceSlug).Scan(&dupCount); err != nil {
		t.Fatalf("count default slugs: %v", err)
	}
	if dupCount != 2 {
		t.Fatalf("阶段 B 期望 2 个 `default` 命名空间（迁移 12 按组织各建一个），got %d", dupCount)
	}
	var nsIDsBefore []string
	rows, err := db13.Query(`SELECT id FROM namespaces ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("scan namespace id: %v", err)
		}
		nsIDsBefore = append(nsIDsBefore, id)
	}
	rows.Close()
	counts13 := tableRowCounts(t, db13)
	if err := db13.Close(); err != nil {
		t.Fatalf("close stage B: %v", err)
	}

	// 阶段 C：完整升级到 14 —— ① 不报错 ② 两行都保留 ③ slug = default / default-2
	up, err := Open(path)
	if err != nil {
		t.Fatalf("含重复 slug 的老库升级失败（迁移 14 去重没生效）: %v", err)
	}
	t.Cleanup(func() { _ = up.Close() })

	nsList, err := up.ListNamespaces(context.Background())
	if err != nil {
		t.Fatalf("list namespaces after upgrade: %v", err)
	}
	if len(nsList) != 2 {
		t.Fatalf("升级后命名空间数 = %d, want 2（不能丢/合并）: %+v", len(nsList), nsList)
	}
	gotSlugs := []string{nsList[0].Slug, nsList[1].Slug}
	for _, want := range []string{DefaultNamespaceSlug, DefaultNamespaceSlug + "-2"} {
		found := false
		for _, got := range gotSlugs {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("升级后 slug 列表 %v 缺 %q（按 created_at,id 保留最早，其余 default-N）", gotSlugs, want)
		}
	}
	// 命名空间 id 不变（子表的 namespace_id 指着它们）
	gotIDs := map[string]bool{}
	for _, ns := range nsList {
		gotIDs[ns.ID] = true
	}
	for _, id := range nsIDsBefore {
		if !gotIDs[id] {
			t.Errorf("命名空间 id %s 在升级后消失了（改名可以，换 id 会把子表指针弄断）", id)
		}
	}
	// 业务行数不变
	after := tableRowCounts(t, up)
	for table, want := range counts13 {
		if after[table] != want {
			t.Errorf("升级后 %s 行数 %d → %d", table, want, after[table])
		}
	}
	// UNIQUE(slug) 真的生效
	if _, err := up.Exec(`INSERT INTO namespaces (id, slug, name, created_at) VALUES ('ns_x', ?, 'x', 1)`, DefaultNamespaceSlug); err == nil {
		t.Fatal("升级后仍能插入重复 slug（UNIQUE(slug) 没生效）")
	}
	// 两个 org 的数据分别落在各自的命名空间里（不能串）
	capRows, err := up.QueryContext(context.Background(), `SELECT id, namespace_id FROM capability_versions WHERE id IN (?, ?) ORDER BY id`, capA, capB)
	if err != nil {
		t.Fatalf("query legacy capabilities: %v", err)
	}
	defer capRows.Close()
	seen := map[string]string{}
	for capRows.Next() {
		var id, ns string
		if err := capRows.Scan(&id, &ns); err != nil {
			t.Fatalf("scan legacy capability: %v", err)
		}
		seen[id] = ns
	}
	if seen[capA] == "" || seen[capB] == "" {
		t.Fatalf("老能力行丢了: %+v", seen)
	}
	if seen[capA] == seen[capB] {
		t.Fatalf("两个组织的同名能力被合并到同一个命名空间 %s（串数据）", seen[capA])
	}
}

// TestMigration14NoSlugReference 静态门禁：**没有任何业务表通过 slug 引用命名空间**。
//
// 为什么重要：迁移 14 会给重复的 slug 改名（default → default-2）。
// 如果哪张表用 slug 当外键/指针，改名就会静默断引用。
func TestMigration14NoSlugReference(t *testing.T) {
	db := newTestDB(t)
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var ddl sql.NullString
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatalf("scan table ddl: %v", err)
		}
		if name == "namespaces" || !ddl.Valid {
			continue // 只有 namespaces 自己拥有 slug 列
		}
		if strings.Contains(ddl.String, "namespace_slug") ||
			strings.Contains(ddl.String, "REFERENCES namespaces(slug)") ||
			strings.Contains(ddl.String, "REFERENCES namespaces (slug)") {
			t.Errorf("表 %s 通过 slug 引用命名空间（改名迁移会断引用）: %s", name, ddl.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	// 反向确认：业务表引用命名空间只可能是 id 列
	for _, table := range []string{"capability_versions", "nodes", "events", "tasks", "playbooks"} {
		cols := columnNames(t, db, table)
		if !cols["namespace_id"] {
			t.Errorf("表 %s 没有 namespace_id 列", table)
		}
		if cols["namespace_slug"] || cols["ns_slug"] {
			t.Errorf("表 %s 有 slug 维度的列", table)
		}
	}
}

// TestMigration14HookCounts 迁移 14 的删表顺序必须是子表在前，否则 DROP 父表
// 会触发 ON DELETE CASCADE 把还没搬走的子行删掉（静默丢数据）。
func TestMigration14HookCounts(t *testing.T) {
	pos := map[string]int{}
	for i, name := range orgLayerDropOrder {
		pos[name] = i
	}
	// 被引用的表必须排在引用它的表之后
	for _, child := range []string{"node_capabilities", "node_instances", "node_pins", "source_targets", "edges", "tasks", "playbooks"} {
		ci, ok := pos[child]
		if !ok {
			t.Fatalf("删表顺序里没有 %s", child)
		}
		for _, parent := range []string{"nodes", "sources", "capability_versions", "namespaces"} {
			if pi, ok := pos[parent]; ok && pi < ci {
				t.Errorf("%s（引用者）排在 %s（被引用者）之后，DROP 会级联删掉子表行", child, parent)
			}
		}
	}
	if len(orgLayerRebuilds) != len(orgLayerTables) {
		t.Fatalf("重建表数量 = %d, want %d", len(orgLayerRebuilds), len(orgLayerTables))
	}
	// 重建表的列清单里不允许出现 org_id
	for _, rb := range orgLayerRebuilds {
		for _, c := range rb.cols {
			if c == "org_id" {
				t.Errorf("%s 的回灌列里还有 org_id", rb.table)
			}
		}
		if strings.Contains(rb.create, "org_id") {
			t.Errorf("%s 的新表定义里还有 org_id", rb.table)
		}
	}
}
