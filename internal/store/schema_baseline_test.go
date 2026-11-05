package store

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// 本文件是 **squash 之后的 baseline 结构门禁**。
//
// squash 把 28 块增量迁移合并成了一份终态 DDL，升级路径（逐块 ALTER / 重建表 /
// 数据搬迁）整条删掉了。原来守护那些路径的用例（migrate14/15/16/18/26/27/28_test.go、
// migrate_engine_compat_test.go 等）测的是「已经不存在的代码」，但**它们守护的性质
// 一条都不能丢**——那些性质现在由「全新库的终态结构」来表达：
//
//   · 表集合（哪些表存在、哪些历史表不该复活）
//   · 关键外键（级联语义、哪一列刻意**不**带外键）
//   · 关键索引（名字 → 列序 → unique / partial）
//   · 唯一约束、主键
//   · PRAGMA foreign_key_check / integrity_check 干净
//   · 重开同一个库幂等（不重复执行、不报错、结构不变）
//   · squash 之前建的库被明确拦截（而不是半死不活）

// baselineTables 是全新库建完之后的**全部**表（含迁移机制自身的 schema_version）。
//
// 逐个点名而不是只数个数：少一张 / 多一张都要被抓出来。
// 历史包袱刻意不在这里：source_targets（并入入口边）、orgs / org_members（组织层已删）、
// messages / consumer_offsets / deliveries（旧通知模型已下线）。
var baselineTables = []string{
	// 身份与密钥（部署级，属于人）
	"users", "passkeys", "sessions", "api_keys",
	"cli_auth_sessions", "recovery_codes",
	// 推送设备（属于用户，不属于命名空间）
	"devices",
	// 命名空间：唯一隔离边界
	"namespaces",
	// 能力 / 节点 / 边（路由的三件套）
	"capability_versions", "nodes", "node_capabilities", "node_instances",
	"node_pins", "edges",
	// 事件 / 任务 / 图
	"events", "tasks", "graph_versions",
	// 产物与部署（内容寻址）
	"artifacts", "deployments",
	// emit 落库
	"emissions",
	// 任务日志（迁移 4：stdout/stderr 原文，随任务级联删除）
	"task_logs",
	// 预案与源
	"playbooks", "sources",
	// 迁移机制自身
	"schema_version",
}

// TestBaselineTableSet 全新库建完 = 恰好这批表（多一张少一张都红）。
func TestBaselineTableSet(t *testing.T) {
	db := newTestDB(t)

	got := masterTables(t, db)
	want := append([]string(nil), baselineTables...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("表集合不一致\n  实际: %v\n  期望: %v", got, want)
	}
	for _, name := range baselineTables {
		if !tableExists(t, db, name) {
			t.Errorf("缺表 %s", name)
		}
	}
}

// TestBaselineNoHistoricalBaggage 历史包袱不许复活。
//
// 这一条是 squash 的"验收条件"：迁移链里那些「建了又删」的东西，在终态里必须
// **根本没有出现过**，而不是"建了再被删"。
func TestBaselineNoHistoricalBaggage(t *testing.T) {
	db := newTestDB(t)

	gone := map[string]string{
		"source_targets":   "源的目标就是入口边（edges.from_is_source=1），不再有旁路表",
		"orgs":             "组织层已删，命名空间是唯一隔离边界",
		"org_members":      "组织层已删",
		"messages":         "旧通知模型已下线（事件在 events，结果在 tasks/emissions）",
		"consumer_offsets": "旧通知模型的 per-user 位点，broker 已删",
		"deliveries":       "旧通知模型的投递记录",
	}
	for name, why := range gone {
		if tableExists(t, db, name) {
			t.Errorf("表 %s 不该存在：%s", name, why)
		}
	}

	// tasks.out 整列删掉：产出通道只有 emission（emissions.port）。
	if columnNames(t, db, "tasks")["out"] {
		t.Error("tasks.out 不该存在：路由结果只在 emissions.port 上（恒 NULL 的可读列会被误用）")
	}
	// node_capabilities.run_spec 整列删掉：执行方式属于产物（artifacts.spec）。
	if columnNames(t, db, "node_capabilities")["run_spec"] {
		t.Error("node_capabilities.run_spec 不该存在：执行方式属于产物（artifacts），归属属于部署")
	}
	// edges 两端是**引用**，不是 capability id：旧的 from_key / to_capability 不该回来。
	edgeCols := columnNames(t, db, "edges")
	for _, col := range []string{"from_key", "to_capability", "org_id"} {
		if edgeCols[col] {
			t.Errorf("edges.%s 不该存在：两端是引用（from_ref / to_ref）", col)
		}
	}
	for _, col := range []string{"from_ref", "to_ref", "from_is_source", "literals", "mapping"} {
		if !edgeCols[col] {
			t.Errorf("edges 缺列 %s", col)
		}
	}

	// 组织层删掉之后，任何表都不该再带 org_id。
	for _, table := range baselineTables {
		if columnNames(t, db, table)["org_id"] {
			t.Errorf("%s.org_id 不该存在：组织层已删（隔离维度只有 namespace_id）", table)
		}
	}
}

// TestBaselineForeignKeys 外键全量形状：谁指向谁、级联语义是什么。
//
// 这里逐条列出**全部**外键（28 条），而不是只挑几条：
// 外键是「删命名空间 / 删节点 / 清任务」这些级联行为的唯一依据，
// 少一条就是静默留下孤儿行，多一条就是静默删掉不该删的数据。
func TestBaselineForeignKeys(t *testing.T) {
	db := newTestDB(t)

	want := []string{
		// 身份与密钥：删用户带走它的凭证 / 会话 / 密钥 / 恢复码
		"api_keys.user_id -> users(id) ON DELETE CASCADE",
		"devices.user_id -> users(id) ON DELETE CASCADE",
		"passkeys.user_id -> users(id) ON DELETE CASCADE",
		"recovery_codes.user_id -> users(id) ON DELETE CASCADE",
		"sessions.user_id -> users(id) ON DELETE CASCADE",
		// 命名空间是唯一隔离边界：删命名空间带走它的全部资源
		"artifacts.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"capability_versions.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"deployments.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"edges.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"events.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"graph_versions.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"node_capabilities.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"node_instances.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"node_pins.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"nodes.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"playbooks.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"sources.namespace_id -> namespaces(id) ON DELETE CASCADE",
		"tasks.namespace_id -> namespaces(id) ON DELETE CASCADE",
		// 能力归属（部署派生）与部署的父行
		"artifacts.capability_id -> capability_versions(id) ON DELETE CASCADE",
		"deployments.artifact_id -> artifacts(id) ON DELETE CASCADE",
		"deployments.node_id -> nodes(id) ON DELETE CASCADE",
		"node_capabilities.capability_id -> capability_versions(id) ON DELETE CASCADE",
		"node_capabilities.node_id -> nodes(id) ON DELETE CASCADE",
		"node_instances.node_id -> nodes(id) ON DELETE CASCADE",
		"node_pins.node_id -> nodes(id) ON DELETE CASCADE",
		// 关键级联：任务被 retention 清掉时，它的 emission 与日志一起走
		//（脱任务的 emission 既不能派发也不能解释；脱任务的日志是一张只涨不降的
		// 孤儿大表，而日志恰好是全库最大的 TEXT）
		"emissions.task_id -> tasks(id) ON DELETE CASCADE",
		"task_logs.task_id -> tasks(id) ON DELETE CASCADE",
		// 刻意**不带级联**：删一个能力不该顺手清空它的任务历史（审计靠它）
		"tasks.capability_id -> capability_versions(id) ON DELETE NO ACTION",
	}
	sort.Strings(want)

	got := foreignKeys(t, db)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("外键集合不一致\n  实际:\n%s\n\n  期望:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// 补充断言：edges 的两端是**引用**，刻意没有外键（"跟随 latest" 无法用外键表达）。
	// 这一条单独钉住，避免有人"顺手"给 from_ref / to_ref 加 REFERENCES 把
	// `fixer`（跟随）语义搞坏。
	var refFKs int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_foreign_key_list('edges') WHERE "from" IN ('from_ref','to_ref')`).
		Scan(&refFKs); err != nil {
		t.Fatalf("probe edges refs: %v", err)
	}
	if refFKs != 0 {
		t.Errorf("edges.from_ref / to_ref 不该有外键（它们是 name / name@version 引用）：%d 条", refFKs)
	}
}

// baselineIndexes 是全部 27 条索引的期望形状（列序 = 查询形状）。
var baselineIndexes = []struct {
	name    string
	table   string
	cols    []string
	unique  bool
	partial bool
}{
	{name: "idx_apikeys_user", table: "api_keys", cols: []string{"user_id"}},
	{name: "idx_artifacts_cap", table: "artifacts", cols: []string{"capability_id"}},
	{name: "idx_cap_name_ver", table: "capability_versions", cols: []string{"namespace_id", "name", "version"}},
	{name: "idx_cli_auth_expires", table: "cli_auth_sessions", cols: []string{"expires_at"}},
	{name: "idx_deploy_artifact", table: "deployments", cols: []string{"artifact_id"}},
	{name: "idx_deploy_node", table: "deployments", cols: []string{"node_id"}},
	{name: "idx_devices_user", table: "devices", cols: []string{"user_id"}},
	// 派发主路径：列序即查询形状（历史库里同名的漂移索引一律以本形状为准）
	{name: "idx_edges_lookup", table: "edges",
		cols: []string{"namespace_id", "from_is_source", "from_ref", "from_port"}},
	{name: "idx_events_idem", table: "events",
		cols: []string{"namespace_id", "source", "idem_key"}, unique: true, partial: true},
	{name: "idx_events_retention", table: "events", cols: []string{"namespace_id", "created_at"}},
	{name: "idx_events_trace", table: "events", cols: []string{"namespace_id", "trace_id"}},
	{name: "idx_namespaces_created", table: "namespaces", cols: []string{"created_at"}},
	{name: "idx_nodecap_cap", table: "node_capabilities", cols: []string{"capability_id"}},
	{name: "idx_playbooks_ns", table: "playbooks", cols: []string{"namespace_id", "created_at"}},
	{name: "idx_recovery_codes_prefix", table: "recovery_codes", cols: []string{"prefix"}},
	{name: "idx_recovery_codes_user", table: "recovery_codes", cols: []string{"user_id"}},
	{name: "idx_sessions_user", table: "sessions", cols: []string{"user_id"}},
	{name: "idx_sources_due", table: "sources", cols: []string{"enabled", "next_fire_at"}},
	{name: "idx_sources_ns", table: "sources", cols: []string{"namespace_id", "created_at"}},
	{name: "idx_tasks_lease", table: "tasks", cols: []string{"status", "lease_until"}},
	{name: "idx_tasks_node", table: "tasks", cols: []string{"node_id", "status"}},
	// 任务列表：前两列对齐过滤 + 排序，第三列 id 同秒破平
	{name: "idx_tasks_ns_created", table: "tasks", cols: []string{"namespace_id", "created_at", "id"}},
	{name: "idx_tasks_parent", table: "tasks", cols: []string{"namespace_id", "parent_task_id"}},
	// 背压信号：**部分**索引只收 pending 行（终态行不进索引）
	{name: "idx_tasks_pending_created_at", table: "tasks", cols: []string{"created_at"}, partial: true},
	{name: "idx_tasks_retention", table: "tasks",
		cols: []string{"namespace_id", "capability_id", "status", "finished_at"}},
	{name: "idx_tasks_trace", table: "tasks", cols: []string{"namespace_id", "trace_id"}},
	{name: "idx_users_role", table: "users", cols: []string{"role"}},
}

// TestBaselineIndexes 索引全量形状（名字 → 表 → 列序 → unique / partial）。
func TestBaselineIndexes(t *testing.T) {
	db := newTestDB(t)

	// ① 期望的每条索引都存在，且形状逐项相同。
	for _, want := range baselineIndexes {
		cols, unique, partial, ok := indexShape(t, db, want.name)
		if !ok {
			t.Errorf("缺索引 %s（表 %s）", want.name, want.table)
			continue
		}
		var tbl string
		if err := db.QueryRow(
			`SELECT tbl_name FROM sqlite_master WHERE type = 'index' AND name = ?`, want.name).Scan(&tbl); err != nil {
			t.Fatalf("read tbl_name of %s: %v", want.name, err)
		}
		if tbl != want.table {
			t.Errorf("%s 建在 %s 上，期望 %s", want.name, tbl, want.table)
		}
		if strings.Join(cols, ",") != strings.Join(want.cols, ",") {
			t.Errorf("%s 列序 = %v，期望 %v", want.name, cols, want.cols)
		}
		if unique != want.unique {
			t.Errorf("%s unique = %v，期望 %v", want.name, unique, want.unique)
		}
		if partial != want.partial {
			t.Errorf("%s partial = %v，期望 %v", want.name, partial, want.partial)
		}
	}

	// ② 没有多余的显式索引（sqlite_autoindex_* 是 UNIQUE / PK 的隐式索引，单独断言）。
	var explicit []string
	rows, err := db.Query(
		`SELECT name FROM sqlite_master
		  WHERE type = 'index' AND sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		  ORDER BY name`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan index name: %v", err)
		}
		explicit = append(explicit, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate indexes: %v", err)
	}
	rows.Close()
	wantNames := make([]string, 0, len(baselineIndexes))
	for _, ix := range baselineIndexes {
		wantNames = append(wantNames, ix.name)
	}
	sort.Strings(wantNames)
	if strings.Join(explicit, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("显式索引集合不一致\n  实际: %v\n  期望: %v", explicit, wantNames)
	}

	// ③ 背压索引的谓词必须真的是 status='pending'（列序对、谓词错就白建）。
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`,
		"idx_tasks_pending_created_at").Scan(&ddl); err != nil {
		t.Fatalf("read ddl of pending backlog index: %v", err)
	}
	if flat := strings.Join(strings.Fields(ddl), " "); !strings.Contains(flat, "WHERE status = 'pending'") {
		t.Errorf("背压索引的谓词不是 `WHERE status = 'pending'`:\n%s", flat)
	}
}

// TestBaselineUniqueConstraints UNIQUE 约束（隐式索引）也在门禁里。
//
// 为什么单独一条：`capability_versions` 的两个 UNIQUE 承载「能力身份」与
// 「同内容不产生第二行」，它们没有索引名（sqlite_autoindex_*），
// 只看显式索引会完全漏掉。
func TestBaselineUniqueConstraints(t *testing.T) {
	db := newTestDB(t)

	cases := []struct {
		table string
		cols  []string
	}{
		{"capability_versions", []string{"namespace_id", "name", "version"}},
		{"capability_versions", []string{"namespace_id", "content_hash"}},
		{"nodes", []string{"namespace_id", "name"}},
		{"namespaces", []string{"slug"}},
		{"node_instances", []string{"node_id", "slot"}},
		{"graph_versions", []string{"namespace_id", "content_hash"}},
		{"artifacts", []string{"namespace_id", "digest"}},
		{"deployments", []string{"node_id", "artifact_id"}},
		{"playbooks", []string{"namespace_id", "slug"}},
		{"sources", []string{"namespace_id", "name"}},
		{"users", []string{"username"}},
		{"devices", []string{"endpoint"}},
		{"cli_auth_sessions", []string{"user_code"}},
	}
	for _, tc := range cases {
		if !hasUniqueOn(t, db, tc.table, tc.cols) {
			t.Errorf("%s 上缺 UNIQUE(%s)", tc.table, strings.Join(tc.cols, ", "))
		}
	}
}

// TestBaselinePrimaryKeys 主键形状（复合主键的顺序就是幂等键的顺序）。
func TestBaselinePrimaryKeys(t *testing.T) {
	db := newTestDB(t)

	cases := []struct {
		table string
		cols  []string
	}{
		{"users", []string{"id"}},
		{"edges", []string{"namespace_id", "from_is_source", "from_ref", "from_port", "to_ref"}},
		{"node_capabilities", []string{"node_id", "capability_id"}},
		{"emissions", []string{"task_id", "seq"}},
		{"node_pins", []string{"sticky_key"}},
		{"schema_version", []string{"version"}},
	}
	for _, tc := range cases {
		got := primaryKeyColumns(t, db, tc.table)
		if strings.Join(got, ",") != strings.Join(tc.cols, ",") {
			t.Errorf("%s 主键 = %v，期望 %v", tc.table, got, tc.cols)
		}
	}
}

// TestBaselineIntegrity 全新库必须自洽：外键检查干净、integrity_check = ok。
func TestBaselineIntegrity(t *testing.T) {
	db := newTestDB(t)

	var fkBad int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&fkBad); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if fkBad != 0 {
		t.Errorf("PRAGMA foreign_key_check 有 %d 条违规", fkBad)
	}
	var integ string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integ); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integ != "ok" {
		t.Errorf("PRAGMA integrity_check = %q, want ok", integ)
	}
}

// TestBaselineReopenIsIdempotent 重开同一个库：不重复执行、不报错、结构一字不差。
//
// 这是「版本记账」这条机制的门禁：baseline 只该应用一次，第二次 Open 必须整体跳过。
func TestBaselineReopenIsIdempotent(t *testing.T) {
	db, path := newTestDBFile(t)

	v1, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if v1 != headVersion {
		t.Fatalf("全新库版本 = %d，期望 headVersion %d（baseline + 追加块全部记账）", v1, headVersion)
	}
	before := masterDDL(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	v2, err := db2.SchemaVersion()
	if err != nil {
		t.Fatalf("schema version after reopen: %v", err)
	}
	if v2 != v1 {
		t.Fatalf("重开后版本 %d → %d，baseline 被重复执行了", v1, v2)
	}
	var rows int
	if err := db2.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&rows); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	// 每块迁移记一行（baseline + 追加块），重开**不重复**记账。
	if want := len(mustParse(t, schemaSQL)); rows != want {
		t.Fatalf("schema_version 有 %d 行，期望 %d 行（每块迁移只记账一次）", rows, want)
	}
	if got := masterDDL(t, db2); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("重开后结构变了：\n--- 首次 Open ---\n%s\n--- 重开 ---\n%s",
			strings.Join(before, "\n"), strings.Join(got, "\n"))
	}
}

// TestLegacyDatabaseRejected squash 之前建的库必须被**明确**拦截。
//
// 做法：手工造一个「v28 老库」——schema_version 里记着 1..28（squash 之前的最高版本），
// 外加一张带数据的老表。期望 Open 直接返回那句可操作的错误，而且**不动数据**
// （绝不能"静默删库重建"：那是用户的目录）。
func TestLegacyDatabaseRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v28.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	mustExec(t, raw, `CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`)
	for v := 1; v <= 28; v++ {
		mustExec(t, raw, `INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, v, 1700000000)
	}
	mustExec(t, raw, `CREATE TABLE legacy_marker (id TEXT PRIMARY KEY)`)
	mustExec(t, raw, `INSERT INTO legacy_marker (id) VALUES ('keep-me')`)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := Open(path)
	if err == nil {
		_ = db.Close()
		t.Fatal("squash 之前的老库被接受了：必须返回可操作的错误")
	}
	// 文案必须可操作：说清"是什么版本 / 不再支持增量升级 / 删数据目录 / 怎么重建数据"。
	for _, frag := range []string{
		"squash 之前的版本（v28）",
		"本版本不再支持增量升级",
		"请删除数据目录后重启",
		"cmd/devseed",
	} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("错误文案缺 %q：%v", frag, err)
		}
	}

	// 数据不许被静默删掉：老表还在、行还在、schema_version 一条不少。
	raw, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	var markers int
	if err := raw.QueryRow(`SELECT count(*) FROM legacy_marker WHERE id = 'keep-me'`).Scan(&markers); err != nil {
		t.Fatalf("老数据不见了（Open 把它删了？）: %v", err)
	}
	if markers != 1 {
		t.Fatalf("legacy_marker 行数 = %d，期望 1（拦截不许删数据）", markers)
	}
	var versions int
	if err := raw.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&versions); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if versions != 28 {
		t.Fatalf("schema_version 行数 = %d，期望 28（拦截不许改账）", versions)
	}
	// 也不许"先建一半再说"：新模型的表一张都不该出现。
	var fresh int
	if err := raw.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'tasks'`).Scan(&fresh); err != nil {
		t.Fatalf("probe tasks: %v", err)
	}
	if fresh != 0 {
		t.Error("拦截前已经把 baseline 建起来了：应该在执行任何迁移之前就返回错误")
	}
}

// ---------- 小工具 ----------

// masterTables 返回库里全部用户表（排除 sqlite_ 内部表），已排序。
func masterTables(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT name FROM sqlite_master
		  WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		  ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	return out
}

// masterDDL 把 sqlite_master 拉平成 `type|name|tbl_name|sql` 的排序切片（形状比对用）。
//
// 排除 sqlite_ 内部表：sqlite_stat1 / sqlite_stat4 / sqlite_sequence 是**运行期数据**，
// 两个库本来就可能不同（行数不同、AUTOINCREMENT 用没用过），把它们算进形状比对是错的。
func masterDDL(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT type, name, tbl_name, sql FROM sqlite_master
		  WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		  ORDER BY type DESC, name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, name, tbl, ddl string
		if err := rows.Scan(&typ, &name, &tbl, &ddl); err != nil {
			t.Fatalf("scan sqlite_master row: %v", err)
		}
		out = append(out, typ+"|"+name+"|"+tbl+"|"+strings.Join(strings.Fields(ddl), " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return out
}

// indexShape 返回索引的 (列序, unique, partial, 是否存在)。
func indexShape(t *testing.T, db *DB, index string) (cols []string, unique, partial, ok bool) {
	t.Helper()
	rows, err := db.Query(`SELECT "unique", partial FROM pragma_index_list(
		(SELECT tbl_name FROM sqlite_master WHERE type = 'index' AND name = ?)) WHERE name = ?`,
		index, index)
	if err != nil {
		t.Fatalf("probe index %s: %v", index, err)
	}
	seen := false
	for rows.Next() {
		var u, p int
		if err := rows.Scan(&u, &p); err != nil {
			rows.Close()
			t.Fatalf("scan index list of %s: %v", index, err)
		}
		unique = u == 1
		partial = p == 1
		seen = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate index list of %s: %v", index, err)
	}
	rows.Close()
	if !seen {
		return nil, false, false, false
	}

	crows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		t.Fatalf("probe columns of index %s: %v", index, err)
	}
	defer crows.Close()
	for crows.Next() {
		var name string
		if err := crows.Scan(&name); err != nil {
			t.Fatalf("scan column of index %s: %v", index, err)
		}
		cols = append(cols, name)
	}
	if err := crows.Err(); err != nil {
		t.Fatalf("iterate columns of index %s: %v", index, err)
	}
	return cols, unique, partial, true
}

// hasUniqueOn 判断表上是否存在一条列序完全等于 cols 的 UNIQUE 索引。
func hasUniqueOn(t *testing.T, db *DB, table string, cols []string) bool {
	t.Helper()
	rows, err := db.Query(
		`SELECT name FROM pragma_index_list(?) WHERE "unique" = 1 AND origin IN ('u','pk')`, table)
	if err != nil {
		t.Fatalf("list unique indexes on %s: %v", table, err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan unique index of %s: %v", table, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate unique indexes of %s: %v", table, err)
	}
	rows.Close()

	for _, name := range names {
		crows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name)
		if err != nil {
			t.Fatalf("probe columns of index %s: %v", name, err)
		}
		var got []string
		for crows.Next() {
			var col string
			if err := crows.Scan(&col); err != nil {
				crows.Close()
				t.Fatalf("scan column of index %s: %v", name, err)
			}
			got = append(got, col)
		}
		if err := crows.Err(); err != nil {
			crows.Close()
			t.Fatalf("iterate columns of index %s: %v", name, err)
		}
		crows.Close()
		if strings.Join(got, ",") == strings.Join(cols, ",") {
			return true
		}
	}
	return false
}

// primaryKeyColumns 返回表的主键列（按 pk 序号升序）。
func primaryKeyColumns(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT name, pk FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		t.Fatalf("probe primary key of %s: %v", table, err)
	}
	defer rows.Close()
	type col struct {
		name string
		pk   int
	}
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.name, &c.pk); err != nil {
			t.Fatalf("scan primary key column of %s: %v", table, err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate primary key of %s: %v", table, err)
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].pk < cols[j].pk })
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.name)
	}
	return out
}

// foreignKeys 返回全库外键的描述行（`表.列 -> 引用表(引用列) ON DELETE X`），已排序。
func foreignKeys(t *testing.T, db *DB) []string {
	t.Helper()
	var out []string
	for _, table := range masterTables(t, db) {
		rows, err := db.Query(
			`SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?)`, table)
		if err != nil {
			t.Fatalf("list foreign keys of %s: %v", table, err)
		}
		type fk struct {
			refTable, from string
			to             sql.NullString
			onDelete       string
		}
		var fks []fk
		for rows.Next() {
			var f fk
			if err := rows.Scan(&f.refTable, &f.from, &f.to, &f.onDelete); err != nil {
				rows.Close()
				t.Fatalf("scan foreign key of %s: %v", table, err)
			}
			fks = append(fks, f)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate foreign keys of %s: %v", table, err)
		}
		rows.Close()
		for _, f := range fks {
			ref := f.refTable
			if f.to.Valid {
				ref += "(" + f.to.String + ")"
			}
			out = append(out, table+"."+f.from+" -> "+ref+" ON DELETE "+f.onDelete)
		}
	}
	sort.Strings(out)
	return out
}
