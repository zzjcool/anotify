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

// TestMigration16ListTasksOrdering 验收 5：返回顺序**眼下**是 created_at DESC, id DESC。
//
// ⚠️ 覆盖面要说准（实测，别再把它写成「这条守着破平语义」）：
// 把 ListTasks 的 ORDER BY 里的 `, id DESC` 删掉，**这条用例仍然是绿的**。原因是：
// 同秒三行之所以还按 id 降序出来，不是 SQL 要了 id DESC，而是 `idx_tasks_ns_created`
// 的**第三列就是 id**——规划器选 `SEARCH tasks USING INDEX idx_tasks_ns_created
// (namespace_id=?)`（无 TEMP B-TREE），反向扫这条索引时同秒的行天然就是 id 降序。
// 换句话说：
//
//	这条用例断言的是「当前实现 + 当前索引形状下，返回顺序恰好符合预期」，
//	**不是**「破平语义没被删掉」。真正拦住「把 id DESC 删了」这类篡改的是
//	TestMigration16ListTasksSQLShape 的**静态**断言（扫 task.go 里
//	`ORDER BY created_at DESC, id DESC LIMIT ?` 这个字面片段）。
//
// 保留它仍然有价值：它把「同一页里顺序抖动 / 跨页重复或漏行」这类**用户可见**的后果
// 钉成可执行的断言；一旦索引形状或查询计划变了（索引不再带 id、或退化成 TEMP B-TREE
// 排序且顺序碰巧不同），它会真的红。只是别把它当成「删掉 id DESC 会红」的守卫。
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

// ---------- reviewer 3A · MAJOR-1 返工补的门禁 ----------
//
// 上面那组用例（契约 §1 的五条验收）只测了 ListTasks **不带任何可选过滤** 的那一种
// SQL 形状，而 ListTasks 会拼出 2^3 = 8 种形状（trace_id / node_id / status 各可有可无）。
// 回归正是从这条缝里漏出去的：加了 idx_tasks_ns_created 之后，**没有统计信息**时规划器
// 把「带等值过滤」的形状抢到了新索引上（= 按命名空间全扫），稀有 status 的查询在 2 万行
// 上从 0.15ms 退到 14~26ms，且随命名空间行数线性增长。
//
// 补的门禁分两类，各管一半：
//   · 「有统计之后规划器选哪个索引」——status 稀有值 / status 常见值 / trace_id / node_id
//     四条形状（前两条一条用例、后两条一条用例）。这几条**自己跑一次 ANALYZE**：它们要钉的是
//     规划器的选择，不能因为统计恰好缺失就把「选错索引」和「没统计」混成同一个红灯。
//   · 「统计到底有没有、是不是全量」——老库升级路径（迁移 16 里的 `ANALYZE;`）与新库灌满
//     数据后重开（Open 末尾的统计维护）各一条用例，**都不许测试自己补 ANALYZE**：
//     产品的统计维护一旦退化，它们必须红（MAJOR-1 就是从这两条路上漏出去的）。
//
// 数据规模统一取 2000 行以上（与验收 4 同因：行太少时任何计划都「够快」，门禁抓不到回归）。

// m16InsertStatusTaskSQL 直插一行任务，status / node_id 由调用方指定。
//
// 为什么另开一条 SQL 而不改 m16InsertTaskSQL：那条被验收 5（顺序性）复用，
// 它只关心 id/created_at，不该被这里的列清单变化波及。
const m16InsertStatusTaskSQL = `INSERT INTO tasks
	(id, namespace_id, trace_id, node_id, path, capability_id, capability, version, params,
	 status, cap_hash, graph_id, created_at)
	VALUES (?, ?, ?, ?, '[]', ?, 'm16.probe', 1, '{}', ?, 'hash_m16', 'grf_m16', ?)`

// listTasksProbeFilteredSQL 拼出「带任意一个可选过滤」的探针 SQL，与 ListTasks 的拼法
// 逐字对齐（列清单 / 过滤子句 / 排序 / LIMIT 的顺序都一样；脱钩由
// TestMigration16ListTasksSQLShape 兜住——那条扫的是 task.go 里的字面片段）。
func listTasksProbeFilteredSQL(where string) string {
	return `SELECT ` + taskColumns + ` FROM tasks WHERE namespace_id = ?` + where +
		` ORDER BY created_at DESC, id DESC LIMIT ?`
}

// TestMigration16ListTasksStatusFilterQueryPlan：status 过滤的两种形状（稀有值 / 常见值）。
//
// 这是 MAJOR-1 的**主门禁**：稀有 status 必须走 status 索引（idx_tasks_lease，第二列
// lease_until 让同一个 status 内部有序，但 ORDER BY 仍是 created_at 所以要临时排序——
// 排序几行 vs 扫两万行，前者便宜），常见 status 才该走 idx_tasks_ns_created。
//
// 数据按真实分布造：2000 行里稀有 status 只有个位数（派发失败/取消是少数派），
// 常见 status 占绝大多数。均分数据会让「稀有/常见」两种形状退化成同一种，门禁就失效了。
func TestMigration16ListTasksStatusFilterQueryPlan(t *testing.T) {
	db, _ := newTestDBFile(t)
	nsID, capID := m16TaskFixture(t, db)

	const (
		taskRows = 2400
		rareRows = 5 // 稀有 status 只占个位数，模拟真实（canceled 是少数派）
	)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < taskRows; i++ {
		status := TaskPending
		if i < rareRows {
			status = TaskCanceled
		}
		if _, err := tx.Exec(m16InsertStatusTaskSQL,
			fmt.Sprintf("tsk_m16_st_%05d", i), nsID,
			fmt.Sprintf("trc_m16_st_%05d", i), fmt.Sprintf("nod_m16_st_%02d", i%50),
			capID, status, 1700000000+i/3); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert task %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 复刻迁移 16 在老库升级时做的事（统计来自产品，不是测试替产品补课）。
	mustExec(t, db, `ANALYZE`)
	m16LogTasksStat1(t, db)

	rarePlan := strings.Join(explainQueryPlan(t, db,
		listTasksProbeFilteredSQL(` AND status = ?`), nsID, TaskCanceled, 200), "\n")
	t.Logf("EXPLAIN QUERY PLAN（status=%s 稀有值，%d 行里 %d 行，LIMIT 200）:\n%s",
		TaskCanceled, taskRows, rareRows, rarePlan)
	if !strings.Contains(rarePlan, "idx_tasks_lease") {
		t.Errorf("稀有 status 的计划没走 idx_tasks_lease（MAJOR-1 复发：规划器又把它交给"+
			" idx_tasks_ns_created 按命名空间全扫了——统计是不是没建/被采样降级了？）:\n%s", rarePlan)
	}
	if strings.Contains(rarePlan, tasksListIndexName) {
		t.Errorf("稀有 status 的计划里出现了 %s：等值过滤被「按命名空间全扫」的新索引抢走了"+
			"（这正是 MAJOR-1 的形状；实测 2 万行上代价 14~26ms，且随行数线性增长）:\n%s",
			tasksListIndexName, rarePlan)
	}
	if strings.Contains(strings.ToUpper(rarePlan), "SCAN TASKS") {
		t.Errorf("稀有 status 的计划里有 SCAN tasks：整表扫，status 等值条件没走上任何索引前缀:\n%s", rarePlan)
	}

	commonPlan := strings.Join(explainQueryPlan(t, db,
		listTasksProbeFilteredSQL(` AND status = ?`), nsID, TaskPending, 200), "\n")
	t.Logf("EXPLAIN QUERY PLAN（status=%s 常见值，%d 行里 %d 行，LIMIT 200）:\n%s",
		TaskPending, taskRows, taskRows-rareRows, commonPlan)
	if !strings.Contains(commonPlan, tasksListIndexName) {
		t.Errorf("常见 status 的计划里没有 %s：绝大多数行都命中，就该走「过滤 + 排序一把过」的索引"+
			"（#237 的收益不能为了稀有值那条形状被牺牲）:\n%s", tasksListIndexName, commonPlan)
	}
	if strings.Contains(strings.ToUpper(commonPlan), "TEMP B-TREE") {
		t.Errorf("常见 status 的计划里有 TEMP B-TREE：ORDER BY created_at DESC, id DESC 没被索引满足:\n%s", commonPlan)
	}

	noFilterPlan := strings.Join(explainQueryPlan(t, db, listTasksProbeSQL, nsID, 200), "\n")
	t.Logf("EXPLAIN QUERY PLAN（无过滤，%d 行，LIMIT 200）:\n%s", taskRows, noFilterPlan)
	if !strings.Contains(noFilterPlan, tasksListIndexName) {
		t.Errorf("无过滤的计划里没有 %s（#237 复发）:\n%s", tasksListIndexName, noFilterPlan)
	}
}

// TestMigration16ListTasksTraceFilterQueryPlan：trace_id / node_id 两种过滤形状。
//
// 审查实测这两种**没有**被新索引抢走（它们各自的索引选择性足够高：trace_id 每行唯一、
// node_id 每个节点占 1/50），这里把这条事实锚住——将来谁动了索引集合或统计策略，
// 这四行计划会立刻变成红灯，而不是等到线上 p95 报警。
//
// 注意 trace 的计划里带 TEMP B-TREE 是**合理**的：一个 trace 通常只有几行到几十行，
// 拿 idx_tasks_trace 精确定位再排几行，比重扫命名空间便宜得多。所以这里不断言没有排序。
func TestMigration16ListTasksTraceFilterQueryPlan(t *testing.T) {
	db, _ := newTestDBFile(t)
	nsID, capID := m16TaskFixture(t, db)

	const (
		taskRows = 2400
		nodeMod  = 50 // 50 个节点轮流接单：每个 node_id 约 48 行
	)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < taskRows; i++ {
		if _, err := tx.Exec(m16InsertStatusTaskSQL,
			fmt.Sprintf("tsk_m16_tr_%05d", i), nsID,
			fmt.Sprintf("trc_m16_tr_%05d", i), fmt.Sprintf("nod_m16_tr_%02d", i%nodeMod),
			capID, TaskPending, 1700000000+i/3); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert task %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mustExec(t, db, `ANALYZE`)
	m16LogTasksStat1(t, db)

	// 取中间的一个 trace / node（不取边界值，避免「撞上第一行」这种碰巧）。
	tracePlan := strings.Join(explainQueryPlan(t, db,
		listTasksProbeFilteredSQL(` AND trace_id = ?`), nsID, "trc_m16_tr_01200", 200), "\n")
	t.Logf("EXPLAIN QUERY PLAN（trace_id 过滤）:\n%s", tracePlan)
	if !strings.Contains(tracePlan, "idx_tasks_trace") {
		t.Errorf("trace_id 过滤没走 idx_tasks_trace（唯一的 trace 定位索引，选择性=1 行）:\n%s", tracePlan)
	}
	if strings.Contains(strings.ToUpper(tracePlan), "SCAN TASKS") {
		t.Errorf("trace_id 过滤的计划里有 SCAN tasks：整表扫:\n%s", tracePlan)
	}

	nodePlan := strings.Join(explainQueryPlan(t, db,
		listTasksProbeFilteredSQL(` AND node_id = ?`), nsID, "nod_m16_tr_07", 200), "\n")
	t.Logf("EXPLAIN QUERY PLAN（node_id 过滤）:\n%s", nodePlan)
	if !strings.Contains(nodePlan, "idx_tasks_node") {
		t.Errorf("node_id 过滤没走 idx_tasks_node（每个节点只占 1/%d 的行，比扫命名空间便宜）:\n%s",
			nodeMod, nodePlan)
	}
	if strings.Contains(strings.ToUpper(nodePlan), "SCAN TASKS") {
		t.Errorf("node_id 过滤的计划里有 SCAN tasks：整表扫:\n%s", nodePlan)
	}
}

// TestMigration16ListTasksCrossNamespaceSameSecondOrder：两个命名空间、同一批 created_at、
// id 交错插入时，各自命名空间内的顺序仍然是严格的 `created_at DESC, id DESC`，且不串号。
//
// 为什么单独一条：验收 5 只在一个命名空间内验证排序。跨命名空间才是索引「第一列是
// namespace_id」真正要挡的东西——如果索引/查询的命名空间条件写错（或破平键漏在
// 索引外），表现就是「别的命名空间的任务混进来」「同秒顺序错乱」，而单命名空间用例看不见。
func TestMigration16ListTasksCrossNamespaceSameSecondOrder(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()

	// 两个命名空间各自有一个能力定义（tasks.capability_id 是外键，不能借用别人的）。
	nsA, capA := m16TaskFixture(t, db)
	nsB := newTestExtraNS(t, db, "m16-order-b")
	capB := m16CapabilityIn(t, db, nsB, "m16.probe.b")

	// 同一批秒（跨 3 秒），id 在两个命名空间之间**交错**：写入顺序是 A,B,A,B…
	// 于是「按 rowid/插入顺序」和「按 (created_at, id)」会给出完全不同的答案。
	base := int64(1700000000)
	for i := 0; i < 6; i++ {
		createdAt := base + int64(i/2) // 0,0,1,1,2,2 → 每两行同秒
		m16InsertTask(t, db, nsA, capA,
			fmt.Sprintf("tsk_m16_xa_%d", i), "trc_m16_xa", createdAt)
		m16InsertTask(t, db, nsB, capB,
			fmt.Sprintf("tsk_m16_xb_%d", i), "trc_m16_xb", createdAt)
	}

	for _, tc := range []struct {
		name    string
		nsID    string
		prefix  string
		foreign string
	}{
		{"命名空间 A", nsA, "tsk_m16_xa_", "tsk_m16_xb_"},
		{"命名空间 B", nsB, "tsk_m16_xb_", "tsk_m16_xa_"},
	} {
		got, err := db.ListTasks(ctx, tc.nsID, "", "", "", 0)
		if err != nil {
			t.Fatalf("%s: ListTasks: %v", tc.name, err)
		}
		if len(got) != 6 {
			ids := make([]string, len(got))
			for i, task := range got {
				ids[i] = task.ID
			}
			t.Fatalf("%s: 返回 %d 行, want 6（完整: %v）", tc.name, len(got), ids)
		}
		// created_at DESC, id DESC：同秒内按 id 降序（i 大在前），秒大的在前。
		want := []string{
			tc.prefix + "5", // base+2
			tc.prefix + "4", // base+2
			tc.prefix + "3", // base+1
			tc.prefix + "2", // base+1
			tc.prefix + "1", // base+0
			tc.prefix + "0", // base+0
		}
		ids := make([]string, len(got))
		for i, task := range got {
			ids[i] = task.ID
		}
		for i := range want {
			if got[i].ID != want[i] {
				t.Fatalf("%s 第 %d 行 = %s, want %s（完整顺序: %v）", tc.name, i, got[i].ID, want[i], ids)
			}
			if strings.HasPrefix(got[i].ID, tc.foreign) {
				t.Fatalf("%s 的结果里混进了另一个命名空间的行 %s（命名空间隔离破了）", tc.name, got[i].ID)
			}
			if got[i].NamespaceID != tc.nsID {
				t.Fatalf("%s 第 %d 行的 namespace_id = %s, want %s", tc.name, i, got[i].NamespaceID, tc.nsID)
			}
		}
		// 时间列非增（防止「id 顺序对了但秒序被忽略」）。
		for i := 1; i < len(got); i++ {
			if got[i].CreatedAt > got[i-1].CreatedAt {
				t.Fatalf("%s 第 %d 行 created_at = %d 大于上一行 %d：不是 created_at DESC",
					tc.name, i, got[i].CreatedAt, got[i-1].CreatedAt)
			}
		}
	}
}

// TestMigration16ListTasksStatsFromProductOpen：产品路径（Open）必须自己把统计建对。
//
// 场景正是 MAJOR-1 最容易复发的那条：**新库空气 → 灌到几千行 → 重开**。
// 建库时迁移 16 的 ANALYZE 扫的是空表（等于没写统计），所以统计只能由 Open 末尾那次
// 统计维护补出来。补出来的必须是**全量**统计：裸 `PRAGMA optimize` 会带上 SQLite 的
// 0x10 位（analysis_limit=2000 的采样分析），只按样本算「每命名空间平均多少行」，
// 把 2000 行的命名空间记成 ~2000 行，稀有 status 的查询又会被抢回 idx_tasks_ns_created。
// 这条用例把「Open 之后的统计质量」也纳入门禁，而不是只看「有没有 sqlite_stat1 这行」。
func TestMigration16ListTasksStatsFromProductOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fill.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	nsID, capID := m16TaskFixture(t, first)
	const taskRows = 2400
	tx, err := first.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < taskRows; i++ {
		status := TaskPending
		if i < 5 {
			status = TaskCanceled
		}
		if _, err := tx.Exec(m16InsertStatusTaskSQL,
			fmt.Sprintf("tsk_m16_fill_%05d", i), nsID,
			fmt.Sprintf("trc_m16_fill_%05d", i), fmt.Sprintf("nod_m16_fill_%02d", i%50),
			capID, status, 1700000000+i/3); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert task %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()

	// ① 统计确实存在，而且是**全量**的（不是 2000 行采样的估算值）。
	//    用 Errorf 而不是 Fatalf：统计缺失时下面的计划断言也要跑出来，一次性把
	//    「没统计」和「计划选错」两条证据一起摆出来（改前跑这条用例正是这样红的）。
	stat := m16LogTasksStat1(t, second)
	if stat == "" {
		t.Errorf("重开后 sqlite_stat1 里没有 tasks 的统计：Open 末尾的统计维护没生效"+
			"（灌了 %d 行之后规划器仍然是盲的）", taskRows)
	}
	// ② 统计质量：稀有 status 的计划必须走 status 索引。
	rarePlan := strings.Join(explainQueryPlan(t, second,
		listTasksProbeFilteredSQL(` AND status = ?`), nsID, TaskCanceled, 200), "\n")
	t.Logf("EXPLAIN QUERY PLAN（重开后 status=%s 稀有值）:\n%s", TaskCanceled, rarePlan)
	if !strings.Contains(rarePlan, "idx_tasks_lease") {
		t.Errorf("重开后稀有 status 的计划没走 idx_tasks_lease：Open 补出来的统计质量不够"+
			"（裸 PRAGMA optimize 的 0x10 采样位会把每命名空间行数低估约 10 倍，规划器于是"+
			"又选 idx_tasks_ns_created 全扫；去掉 0x10 即 PRAGMA optimize=0x2）:\n%s", rarePlan)
	}
	if strings.Contains(rarePlan, tasksListIndexName) {
		t.Errorf("重开后稀有 status 的计划里出现了 %s（MAJOR-1 复发）:\n%s", tasksListIndexName, rarePlan)
	}
	// ③ #237 的收益不能因此丢：无过滤仍走新索引。
	noFilterPlan := strings.Join(explainQueryPlan(t, second, listTasksProbeSQL, nsID, 200), "\n")
	if !strings.Contains(noFilterPlan, tasksListIndexName) {
		t.Errorf("重开后无过滤的计划里没有 %s（#237 复发）:\n%s", tasksListIndexName, noFilterPlan)
	}
}

// TestMigration16UpgradeFromV15BackfillsStats：老库升级路径必须**自己**把统计补出来。
//
// 这是迁移 16 里那句 `ANALYZE;` 的**唯一**门禁。上面几条计划用例都显式跑过一次 ANALYZE
// （因为它们要断言的是「有统计之后规划器怎么选」），所以它们无法区分「统计是迁移建的」
// 还是「统计是测试建的」——把 schema.sql 里那句 ANALYZE 删掉，它们仍然全绿。
// 而线上走的就是这条路：v15 老库里已经躺着几万行任务，升级时建索引的同时必须补统计，
// 否则 MAJOR-1 立刻复发（实测 2 万行 14~26ms，随行数线性增长）。
//
// 所以这条用例**不跑任何 ANALYZE**，只做「造 v15 库 + 写数据 + Open」，
// 然后直接看：stat1 有没有、是不是全量的、稀有 status 有没有走 status 索引。
func TestMigration16UpgradeFromV15BackfillsStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v15_filled.db")
	db15 := openAtVersion(t, path, 15)
	if v := mustMaxVersion(t, db15); v != 15 {
		t.Fatalf("夹具版本 = %d, want 15", v)
	}
	// 升级前索引还不存在，所以下面的计划断言真正测的是「迁移 16 一次做完 DDL + 统计」。
	if n := countIndexRows(t, db15, "tasks", tasksListIndexName); n != 0 {
		t.Fatalf("v15 库里已经有 %s（%d 条），夹具没停在该停的位置", tasksListIndexName, n)
	}
	nsID, capID := m16TaskFixture(t, db15)
	const taskRows = 2400
	tx, err := db15.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < taskRows; i++ {
		status := TaskPending
		if i < 5 {
			status = TaskCanceled
		}
		if _, err := tx.Exec(m16InsertStatusTaskSQL,
			fmt.Sprintf("tsk_m16_up_%05d", i), nsID,
			fmt.Sprintf("trc_m16_up_%05d", i), fmt.Sprintf("nod_m16_up_%02d", i%50),
			capID, status, 1700000000+i/3); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert task %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := db15.Close(); err != nil {
		t.Fatalf("close v15 db: %v", err)
	}

	// 只调 Open：迁移 16 建索引 + ANALYZE 都在里面，测试不得再补 ANALYZE。
	db, err := Open(path)
	if err != nil {
		t.Fatalf("升级 v15 库: %v", err)
	}
	defer func() { _ = db.Close() }()
	if v, want := mustMaxVersion(t, db), testTipVersion(t); v != want {
		t.Fatalf("升级后版本 = %d, want %d", v, want)
	}

	stat := m16LogTasksStat1(t, db)
	if stat == "" {
		t.Fatalf("升级后 sqlite_stat1 里没有 tasks 的统计：schema.sql 迁移 16 的 ANALYZE 没生效/"+
			"被删了？没有统计时规划器会把 status= 稀有值的列表查询交给 %s 全扫（MAJOR-1）",
			tasksListIndexName)
	}
	// 全量统计的判据：stat1 里「每个命名空间平均多少行」应等于真实行数（采样分析会记成
	// ~2000 = analysis_limit，正好是这次回归被低估的那个数字）。
	wantPrefix := fmt.Sprintf("%d %d ", taskRows, taskRows)
	if !strings.HasPrefix(stat, wantPrefix) {
		t.Errorf("%s 的 stat1 = %q，want 前缀 %q：统计看起来是**采样**的而不是全量的",
			tasksListIndexName, stat, wantPrefix)
	}

	rarePlan := strings.Join(explainQueryPlan(t, db,
		listTasksProbeFilteredSQL(` AND status = ?`), nsID, TaskCanceled, 200), "\n")
	t.Logf("EXPLAIN QUERY PLAN（老库升级后 status=%s 稀有值）:\n%s", TaskCanceled, rarePlan)
	if !strings.Contains(rarePlan, "idx_tasks_lease") {
		t.Errorf("老库升级后稀有 status 的计划没走 idx_tasks_lease（MAJOR-1 复发：升级路径没补统计）:\n%s",
			rarePlan)
	}
	if strings.Contains(rarePlan, tasksListIndexName) {
		t.Errorf("老库升级后稀有 status 的计划里出现了 %s：等值过滤被按命名空间全扫的首选索引抢走"+
			"（实测 2 万行上 14~26ms）:\n%s", tasksListIndexName, rarePlan)
	}

	// #237 的收益照旧：无过滤仍走新索引、不落临时排序。
	noFilterPlan := strings.Join(explainQueryPlan(t, db, listTasksProbeSQL, nsID, 200), "\n")
	if !strings.Contains(noFilterPlan, tasksListIndexName) {
		t.Errorf("老库升级后无过滤的计划里没有 %s（#237 复发）:\n%s", tasksListIndexName, noFilterPlan)
	}
	if strings.Contains(strings.ToUpper(noFilterPlan), "TEMP B-TREE") {
		t.Errorf("老库升级后无过滤的计划里有 TEMP B-TREE：ORDER BY 没被索引满足:\n%s", noFilterPlan)
	}
}

// TestMigration16ListTasksFilteredSQLShape 是**新增那几条计划用例**的护栏。
//
// 既有的 TestMigration16ListTasksSQLShape 只守无过滤那条探针（namespace_id + ORDER BY + LIMIT）；
// 而 status / trace / node 那几条用例用的是 `AND <列> = ?` 的可选过滤片段，它们没被那条守住。
// 一旦 ListTasks 的拼法变了（列名加表别名、参数顺序调整、条件改写），新用例就会变成
// 「在测一条没人跑的 SQL」——计划很漂亮，线上照样慢，而且**不会红**。这条把这种脱钩变成红灯。
func TestMigration16ListTasksFilteredSQLShape(t *testing.T) {
	src := strings.Join(strings.Fields(readSource(t, "task.go")), " ")
	for _, frag := range []string{
		"AND trace_id = ?",
		"AND node_id = ?",
		"AND status = ?",
	} {
		if !strings.Contains(src, frag) {
			t.Errorf("task.go 里找不到可选过滤片段 %q：ListTasks 的拼法变了？"+
				"请同步更新 migrate16_test.go 的 listTasksProbeFilteredSQL（否则计划门禁在测一条没人跑的 SQL）", frag)
		}
	}
}

// m16CapabilityIn 在**指定**命名空间里建一个能力定义，返回它的 id。
//
// m16TaskFixture 只给「新注册用户 + 它的默认命名空间」铺了一个能力；
// 跨命名空间用例需要第二个命名空间也有自己的能力（tasks.capability_id 是外键）。
func m16CapabilityIn(t *testing.T, db *DB, namespaceID, name string) string {
	t.Helper()
	def := &CapabilityVersion{
		NamespaceID:  namespaceID,
		Name:         name,
		Version:      1,
		ParamsSchema: json.RawMessage(`{}`),
		Outputs:      []string{"ok"},
		Source:       CapSourceCustom,
	}
	if err := db.CreateCapability(context.Background(), def); err != nil {
		t.Fatalf("create capability %q in %s: %v", name, namespaceID, err)
	}
	return def.ID
}

// m16LogTasksStat1 把 tasks 在 sqlite_stat1 里的条目打进日志，并返回**行数那一条**
// （idx_tasks_ns_created 的 stat1，缺失则返回空串）。
//
// 断言用它判「统计到底有没有建对」：stat1 的第二个数字是「每个前缀值平均多少行」，
// 采样分析会把它记成 ~2000（=分析上限），全量分析记的是真实行数。
func m16LogTasksStat1(t *testing.T, db *DB) string {
	t.Helper()
	var hasTable int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE name = 'sqlite_stat1'`).Scan(&hasTable); err != nil {
		t.Fatalf("probe sqlite_stat1: %v", err)
	}
	if hasTable == 0 {
		t.Logf("sqlite_stat1 不存在（还没跑过 ANALYZE / optimize）")
		return ""
	}
	rows, err := db.Query(`SELECT idx, stat FROM sqlite_stat1 WHERE tbl = 'tasks' ORDER BY idx`)
	if err != nil {
		t.Fatalf("read sqlite_stat1: %v", err)
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		var idx, stat string
		if err := rows.Scan(&idx, &stat); err != nil {
			t.Fatalf("scan sqlite_stat1: %v", err)
		}
		t.Logf("sqlite_stat1[tasks.%s] = %q", idx, stat)
		if idx == tasksListIndexName {
			out = stat
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_stat1: %v", err)
	}
	return out
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
