package store

// 迁移 16（#237）的验收测试：tasks 上要有 (namespace_id, created_at, id) 这条
// 「过滤 + 排序」索引，并且它真的被 ListTasks 用上。
//
// #237 的症状是**静默**的：没有索引时查询照样返回正确结果，只是每页都要先扫完
// 整个命名空间再落临时 B-TREE 排序，p95 随行数线性增长。所以门禁不能只看
// 「结果对不对」，必须看**计划**（EXPLAIN QUERY PLAN）与**形状**（新库/老库一致）。
//
// 五条验收（契约 §1）：
//  1. 新库索引存在且列序 == [namespace_id, created_at, id]；
//  2. 停在 v15 的老库升级后 = tip 版本，且 schemaShape 与新库逐行一致；
//  3. 幂等：连续 Open 两次，索引只有一个、schema_version 里 16 只记一次；
//  4. 2000 行数据上 ListTasks 的计划里有该索引、没有 TEMP B-TREE、没有 SCAN tasks；
//  5. ListTasks 返回严格按 created_at DESC, id DESC（含同秒多行靠 id 破平）。

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// tasksListIndexName / tasksListIndexColumns 是迁移 16 冻结的索引名与列序。
//
// 与迁移 15 的 edgeLookupIndexName 一样，名字与列序都写在这里当**唯一**断言来源：
// 迁移 16 没有 Go hook（理由见 schema.sql 里那一块的注释），所以没有第二处
// 需要同步的常量。
const tasksListIndexName = "idx_tasks_ns_created"

var tasksListIndexColumns = []string{"namespace_id", "created_at", "id"}

// listTasksProbeSQL 是 ListTasks 在「无 trace/node/status 过滤」时的 SQL 文本，
// 逐字照抄（列清单复用同一个 taskColumns 常量）。
//
// 为什么不直接调 ListTasks 再想办法截获它的 SQL：ListTasks 走的是 *sql.DB 直查，
// 测试侧没有 hook 点；而给这个用例单独包一层计数驱动，成本远大于收益。
// 代价是「探针 SQL 与真实 SQL 脱钩」的风险，由 TestMigration16ListTasksSQLShape
// 这条静态断言兜住：查询形状一变，它会红。
const listTasksProbeSQL = `SELECT ` + taskColumns + ` FROM tasks WHERE namespace_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`

// m16InsertTaskSQL 直插一行 pending 任务。
//
// 走 SQL 而不是 CreateTasks：排序与破平断言要**精确控制** id 与 created_at，
// 而 CreateTasks 会自己生成 id、自己填 Now()（只到秒，无法造出「跨秒」样本）。
// 列清单与 CreateTasks 保持一致，只省掉那些有默认值的列。
const m16InsertTaskSQL = `INSERT INTO tasks
	(id, namespace_id, trace_id, path, capability_id, capability, version, params,
	 status, cap_hash, graph_id, created_at)
	VALUES (?, ?, ?, '[]', ?, 'm16.probe', 1, '{}', 'pending', 'hash_m16', 'grf_m16', ?)`

// TestMigration16FreshIndexShape 验收 1：全新库上索引存在，且列序就是冻结的三列。
func TestMigration16FreshIndexShape(t *testing.T) {
	db, _ := newTestDBFile(t)

	if v, want := mustMaxVersion(t, db), testTipVersion(t); v != want {
		t.Fatalf("新库版本 = %d, want %d（tip 版本取自 schema.sql）", v, want)
	}

	got := indexColumnsOn(t, db, "tasks", tasksListIndexName)
	// 实测列序原样打进日志：报告里要贴「索引列序实测输出」。
	t.Logf("pragma_index_info(%s) = %v", tasksListIndexName, got)
	if !sameColumnOrder(got, tasksListIndexColumns) {
		t.Fatalf("%s 列序 = %v, want %v", tasksListIndexName, got, tasksListIndexColumns)
	}

	// 列序对但方向不对的组合在 SQLite 里是存在的（DESC 列会体现在 DDL 文本上），
	// 所以再钉一下 DDL 原文：必须是三列全 ASC（降序查询靠反向扫，理由见 schema.sql）。
	ddl := indexDDL(t, db, tasksListIndexName)
	t.Logf("sqlite_master.sql = %q", ddl)
	if want := "ON tasks(namespace_id, created_at, id)"; !strings.Contains(ddl, want) {
		t.Fatalf("%s 的 DDL 不含 %q，实际 = %q（是否误加 DESC？）", tasksListIndexName, want, ddl)
	}
	if strings.Contains(strings.ToUpper(ddl), "DESC") {
		t.Fatalf("%s 的 DDL 里出现了 DESC：降序查询该靠反向扫 ASC 索引，不要建降序索引（见 schema.sql 迁移 16 注释）: %q",
			tasksListIndexName, ddl)
	}
}

// TestMigration16UpgradeFromV15 验收 2：停在 v15 的老库升级后 = tip，且形状与新库逐行一致。
//
// 这是 #237 在线上真正会走的路径：现有部署全是 v15 库，索引只能由迁移建出来。
// 「新库有索引」证明不了这一点——新库可能是在建表时顺带建的（那老库永远不会有）。
func TestMigration16UpgradeFromV15(t *testing.T) {
	fresh, _ := newTestDBFile(t)

	path := filepath.Join(t.TempDir(), "v15.db")
	db15 := openAtVersion(t, path, 15)
	if v := mustMaxVersion(t, db15); v != 15 {
		t.Fatalf("夹具版本 = %d, want 15", v)
	}
	// 先证明夹具真的停在 v15：索引还不存在，否则「升级把它建出来」没有证据。
	if n := countIndexRows(t, db15, "tasks", tasksListIndexName); n != 0 {
		t.Fatalf("v15 库里已经有 %s（%d 条），夹具没停在该停的位置", tasksListIndexName, n)
	}
	// 库里有真实业务行：迁移 16 只该加索引，一行数据都不许动。
	nsID, capID := m16TaskFixture(t, db15)
	m16InsertTask(t, db15, nsID, capID, "tsk_m16_keep", "trc_m16_keep", 1700000000)
	if err := db15.Close(); err != nil {
		t.Fatalf("close v15 db: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("升级 v15 库: %v", err)
	}
	defer func() { _ = upgraded.Close() }()

	if v, want := mustMaxVersion(t, upgraded), testTipVersion(t); v != want {
		t.Fatalf("升级后版本 = %d, want %d", v, want)
	}
	if got := indexColumnsOn(t, upgraded, "tasks", tasksListIndexName); !sameColumnOrder(got, tasksListIndexColumns) {
		t.Fatalf("升级后 %s 列序 = %v, want %v", tasksListIndexName, got, tasksListIndexColumns)
	}
	// 形状一致是新库/老库不分叉的硬门禁（含 sqlite_master 里的 DDL 原文）。
	assertShapeEqual(t, "v15 升级库", schemaShape(t, fresh), schemaShape(t, upgraded))
	// 数据一行不丢。
	if got := countRows(t, upgraded, "tasks", "1 = 1"); got != 1 {
		t.Fatalf("tasks 行数 = %d, want 1（迁移 16 只加索引，不该动数据）", got)
	}
}

// TestMigration16IdempotentReopen 验收 3：同一库连续 Open 两次，索引只一个、16 只记一次账。
//
// 为什么要测「Open 两次」而不是「同一事务里跑两遍 hook」：迁移 16 没有 hook，
// 幂等的唯一实现就是 `CREATE INDEX IF NOT EXISTS`，而重试路径正是「整个 Open 再来一次」。
func TestMigration16IdempotentReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")

	for i := 1; i <= 2; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次 Open: %v", i, err)
		}
		if v, want := mustMaxVersion(t, db), testTipVersion(t); v != want {
			t.Fatalf("第 %d 次 Open 后版本 = %d, want %d", i, v, want)
		}
		if n := countIndexRows(t, db, "tasks", tasksListIndexName); n != 1 {
			t.Fatalf("第 %d 次 Open 后 %s 有 %d 条，want 1", i, tasksListIndexName, n)
		}
		var recorded int
		if err := db.QueryRow(`SELECT count(*) FROM schema_version WHERE version = 16`).Scan(&recorded); err != nil {
			t.Fatalf("count version 16: %v", err)
		}
		if recorded != 1 {
			t.Fatalf("第 %d 次 Open 后 schema_version 里 16 记了 %d 次, want 1", i, recorded)
		}
		if got := indexColumnsOn(t, db, "tasks", tasksListIndexName); !sameColumnOrder(got, tasksListIndexColumns) {
			t.Fatalf("第 %d 次 Open 后 %s 列序 = %v, want %v", i, tasksListIndexName, got, tasksListIndexColumns)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("第 %d 次 Close: %v", i, err)
		}
	}
}

// TestMigration16ListTasksQueryPlan 验收 4：2000 行数据上，ListTasks 的查询计划走新索引、
// 不做临时排序、不整表扫。
//
// 2000 行不是随意取的：行太少时 SQLite 可能直接全表扫（两行数据上任何计划都「够快」），
// 门禁就抓不到回归。这个数量足以让 planner 走索引，又让用例保持在毫秒级。
func TestMigration16ListTasksQueryPlan(t *testing.T) {
	db, _ := newTestDBFile(t)
	nsID, capID := m16TaskFixture(t, db)

	const taskRows = 2000
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < taskRows; i++ {
		// created_at 每 3 行共享一秒：既贴近真实（派发是突发的），也让「只靠前两列」
		// 分不出先后——正是第三列 id 要破平的那种数据。
		if _, err := tx.Exec(m16InsertTaskSQL,
			fmt.Sprintf("tsk_m16_plan_%05d", i), nsID,
			fmt.Sprintf("trc_m16_plan_%05d", i), capID, 1700000000+i/3); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert task %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := countRows(t, db, "tasks", "1 = 1"); got != taskRows {
		t.Fatalf("tasks 行数 = %d, want %d", got, taskRows)
	}

	// 与 ListTasks 一致的形状：一个命名空间过滤 + LIMIT（200 是线上分页的常见上限档位）。
	plan := explainQueryPlan(t, db, listTasksProbeSQL, nsID, 200)
	joined := strings.Join(plan, "\n")
	t.Logf("EXPLAIN QUERY PLAN（无过滤 ListTasks，%d 行，LIMIT 200）:\n%s", taskRows, joined)

	if !strings.Contains(joined, tasksListIndexName) {
		t.Errorf("计划里没有 %s（#237 复发：又回到扫 idx_tasks_trace + 排序了）:\n%s", tasksListIndexName, joined)
	}
	if strings.Contains(strings.ToUpper(joined), "TEMP B-TREE") {
		t.Errorf("计划里有 TEMP B-TREE：ORDER BY created_at DESC, id DESC 没被索引满足（第三列 id 是否漏了？）:\n%s", joined)
	}
	if strings.Contains(strings.ToUpper(joined), "SCAN TASKS") {
		t.Errorf("计划里有 SCAN tasks：整表扫，命名空间过滤没走上索引前缀:\n%s", joined)
	}
}

// TestMigration16ListTasksOrdering 验收 5：返回顺序严格是 created_at DESC, id DESC。
//
// 索引能不能被用上是计划的事，这一条只管**语义**：同秒多行必须靠 id 破平，
// 否则翻页会出现「同一页里顺序抖动、跨页重复/漏行」——那是最难复现的一类线上问题。
func TestMigration16ListTasksOrdering(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, capID := m16TaskFixture(t, db)

	// 同秒三行（a/b/c 故意乱序插入）+ 跨秒三行，覆盖「秒相同靠 id」「秒不同靠时间」两档。
	for _, r := range []struct {
		id        string
		createdAt int64
	}{
		{"tsk_m16_a", 1700000000},
		{"tsk_m16_c", 1700000000},
		{"tsk_m16_b", 1700000000},
		{"tsk_m16_oldest", 1699999999},
		{"tsk_m16_newest", 1700000002},
		{"tsk_m16_mid", 1700000001},
	} {
		m16InsertTask(t, db, nsID, capID, r.id, "trc_m16_order", r.createdAt)
	}

	got, err := db.ListTasks(ctx, nsID, "", "", "", 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}

	// created_at DESC, id DESC 的期望顺序（同秒 1700000000 三行按 id 降序 = c, b, a）。
	want := []string{
		"tsk_m16_newest", // 1700000002
		"tsk_m16_mid",    // 1700000001
		"tsk_m16_c",      // 1700000000
		"tsk_m16_b",
		"tsk_m16_a",
		"tsk_m16_oldest", // 1699999999
	}
	if len(got) != len(want) {
		t.Fatalf("ListTasks 返回 %d 行, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			ids := make([]string, len(got))
			for j, task := range got {
				ids[j] = fmt.Sprintf("%s@%d", task.ID, task.CreatedAt)
			}
			t.Fatalf("第 %d 行 = %s, want %s（完整顺序: %v）", i, got[i].ID, want[i], ids)
		}
	}
	// 时间列本身也必须非增（防止「id 顺序对了但秒序被忽略」这种只对一半的实现）。
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt > got[i-1].CreatedAt {
			t.Fatalf("第 %d 行 created_at = %d 大于上一行 %d：不是 created_at DESC",
				i, got[i].CreatedAt, got[i-1].CreatedAt)
		}
	}
}

// TestMigration16ListTasksSQLShape 是 EXPLAIN 用例的护栏：探针 SQL 是照抄的，
// 一旦 ListTasks 的查询形状变了而探针没跟着变，上面那条 EXPLAIN 就变成
// 「在测一条没人跑的 SQL」——计划很漂亮，线上照样慢。这条把这个脱钩变成红灯。
//
// 扫整个 task.go 而不是只扫 ListTasks 函数体：实现可能把拼 SQL 的部分抽成同文件的
// helper（那是等价重构），片段只要还在这个文件里就算数。
func TestMigration16ListTasksSQLShape(t *testing.T) {
	// 把换行/连续空格压平，避免「只是换行位置变了」这种假红。
	src := strings.Join(strings.Fields(readSource(t, "task.go")), " ")
	for _, frag := range []string{
		"FROM tasks WHERE namespace_id = ?",
		"ORDER BY created_at DESC, id DESC LIMIT ?",
	} {
		if !strings.Contains(src, frag) {
			t.Errorf("task.go 里找不到任务列表 SQL 片段 %q：查询形状变了？"+
				"请同步更新 migrate16_test.go 的 listTasksProbeSQL（否则 EXPLAIN 门禁在测一条没人跑的 SQL）", frag)
		}
	}
}

// ---------- 小工具（迁移 16 专用，避免动 migrate15_test.go 里的既有 helper） ----------

// testTipVersion 返回 schema.sql 里的最高迁移版本 = 全新库 Open 之后的版本。
//
// 为什么不让调用方写死数字：每加一个迁移就要改一圈测试，是低价值噪音，
// 而且「写死 N」证明不了「升到最新」。tip 版本从迁移集里算才是唯一事实源
// （migrate15_test.go 里那条原先写死 15 的断言就是这么被改成调它的）。
func testTipVersion(t *testing.T) int {
	t.Helper()
	set := mustMigrations(t)
	if len(set) == 0 {
		t.Fatal("schema.sql 里没有解析出任何迁移")
	}
	return set[len(set)-1].version
}

// indexColumnsOn 返回 table 上名为 index 的索引的列序（按 seqno 升序）；索引不存在即 Fatal。
//
// 与 migrate15.go 里的 indexColumns 的区别：那个函数把 tbl_name 写死成 edges
// （迁移 15 只关心那一条索引），这里要读 tasks 上的索引，所以按表名参数化。
// 纯测试侧 helper，不改生产代码。
func indexColumnsOn(t *testing.T, db *DB, table, index string) []string {
	t.Helper()
	if n := countIndexRows(t, db, table, index); n != 1 {
		t.Fatalf("表 %s 上名为 %s 的索引有 %d 条, want 1", table, index, n)
	}
	rows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		t.Fatalf("read columns of index %s: %v", index, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column of index %s: %v", index, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns of index %s: %v", index, err)
	}
	return cols
}

// countIndexRows 数 sqlite_master 里「某表上某名字的索引」有几条（幂等断言用）。
func countIndexRows(t *testing.T, db *DB, table, index string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ? AND tbl_name = ?`,
		index, table).Scan(&n); err != nil {
		t.Fatalf("count index %s on %s: %v", index, table, err)
	}
	return n
}

// explainQueryPlan 跑 `EXPLAIN QUERY PLAN <query>`，把每行的 detail 列按行返回。
//
// 不写死列数：modernc 驱动目前返回 (id, parent, notused, detail)，但那属于驱动细节，
// 这里按 rows.Columns() 动态取值再拼成一行字符串，只在断言与日志里用。
func explainQueryPlan(t *testing.T, db *DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of explain result: %v", err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				parts[i] = string(b)
				continue
			}
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate explain rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN 没有任何输出")
	}
	return out
}

// m16TaskFixture 铺好 tasks 需要的 FK 父行（命名空间 + 一个能力定义），
// 返回 (namespaceID, capabilityID)。
//
// 为什么必须铺：store.Open 的 DSN 显式开了 foreign_keys(ON)，tasks.namespace_id /
// tasks.capability_id 都带外键，伪造父行会在插入时直接 FK 失败。
func m16TaskFixture(t *testing.T, db *DB) (namespaceID, capabilityID string) {
	t.Helper()
	namespaceID, _ = newTestNS(t, db)
	// 名字避开内置能力（内置名是保留的，CreateCapability 会拒绝）。
	def := &CapabilityVersion{
		NamespaceID:  namespaceID,
		Name:         "m16.probe",
		Version:      1,
		ParamsSchema: json.RawMessage(`{}`),
		Outputs:      []string{"ok"},
		Source:       CapSourceCustom,
	}
	if err := db.CreateCapability(context.Background(), def); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	return namespaceID, def.ID
}

// m16InsertTask 直插一行 pending 任务（id / trace_id / created_at 由调用方指定）。
func m16InsertTask(t *testing.T, db *DB, namespaceID, capabilityID, id, traceID string, createdAt int64) {
	t.Helper()
	mustExec(t, db, m16InsertTaskSQL, id, namespaceID, traceID, capabilityID, createdAt)
}
