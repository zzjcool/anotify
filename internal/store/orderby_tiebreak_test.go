package store

// ============================================================================
// L4 · 排序破平局守卫（docs/lint-guards.md 的 L4 条目）
//
// 事故来源（commit 1461f7e4）：`ListUsersWithStatsPage` 原来只 `ORDER BY created_at`，
// 同一毫秒创建的两个用户在游标翻页里谁是「前一行」由 SQLite 的排序实现决定。
// 破平局改成 `id` 之后变成了掷硬币的另一面：`users.id` 是 `usr_` + crypto/rand，
// 谁在前由随机低位决定 —— `TestAdmin_ListUsers` 改成 `-count=40` 实测 17 次红（42.5%）。
// 终态修法是 `ORDER BY created_at, rowid`（rowid = 插入序，稳定且语义正确）。
//
// 那条 commit 之后，全仓 store 还有 70 条 ORDER BY，没有任何机械约束防止复发：
// 下一个人写 `ORDER BY created_at` 时，review 看不出来，测试要么不覆盖并列、
// 要么靠运气（并列概率 = 同一时间粒度内落两行）。本文件把这条纪律变成静态门禁。
//
// 判据（四档，逐条都必须落在某一档里）：
//
//  1. **末键自身唯一** —— 自动通过，不用登记：
//     末键是 `rowid`（表不是 WITHOUT ROWID 时 rowid 天然唯一），
//     或者是单列主键 / 单列 UNIQUE 约束（`id TEXT PRIMARY KEY`、`UNIQUE (slug)` 等）。
//  2. **证明安全** —— 必须登记（orderByAllowlist），且证据是**机械可核验**的：
//     · WHERE 用等值把唯一索引的其余列收窄成常量（`task_id=? AND attempt=? ORDER BY path`；
//       索引是 `PRIMARY KEY (task_id, attempt, path)`）；
//     · 聚合：末键就是 `GROUP BY` 的分组键（每组一行，天然唯一）；
//     · GC 批量删除：`DELETE ... WHERE id IN (SELECT ... ORDER BY x LIMIT n)` 的并列
//       只影响「这一批删哪几行」，不影响不变式（下一轮继续删），也没有游标可漂；
//     · 目录表（sqlite_master / pragma_*）：行来源是 SQLite 内省，天然每行唯一。
//     「证不了」的不许塞进这一档 —— 见第 4 档。
//  3. **已知缺口** —— 末键确实不唯一、且证不了安全（orderByKnownGaps）：每条必须写
//     为什么现在还没出事 + 风险等级与具体影响 + 建议修法。条数受 orderByKnownGapBudget
//     棘轮约束（修好一条必须下调常量，新增一条必须上调 —— 两个方向都红）。
//  4. 剩下的任何一条 = **红**：失败信息指名道姓给 文件:行 + 函数 + 表 + 子句原文 +
//     为什么末键不唯一 + 修法建议。
//
// 纪律（照 internal/server/openapi_parity_test.go 的例外表范式）：
//
//   · **过期即红**：登记项（白名单/缺口）必须仍然命中一条真实存在的子句；
//     子句被改写或删掉 ⇒ 登记项过期 ⇒ 测试失败并要求删除该条。
//   · **反假绿**：扫描到的子句条数有下限（orderByClauseFloor，冻结时实测 70），
//     扫到 0 条直接失败；条数、自动通过、白名单命中、缺口命中全部打印出来。
//     「绿」与「没检查」必须长得不一样。
//   · **证据不许口说无凭**：白名单的每条证据都由 orderByEvidenceProblems 机械复核
//     （声明的唯一索引必须真的在 schema 里、声明的收窄列必须真的出现在 WHERE 里、
//     部分唯一索引的谓词必须真的在子句窗口里）—— 写一个「我觉得安全」登不进去。
//   · **解析器自己也有守卫**：sqlite 侧结构（主键/唯一索引/WITHOUT ROWID）与
//     schema.sql 的解析结果逐表比对（见「schema 解析与真实库一致」子测试）。
//     解析器看漏一个 UNIQUE 索引，就等于给自己开了一个口子。
//
// 为什么扫源码而不是扫运行期日志：ORDER BY 有一半是 `query := ...` + `query += ...`
// 拼出来的，只有把它们在**函数作用域**内拼回去才能拿到完整语句（表名与排序键在同一
// 条语句里）。所以这里用 go/ast 取字面量、按函数拼成 blob，再逐个字面量算括号深度
// （字面量是独立片段，跨片段累计深度会被一个未闭合的 `(` 带偏）。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// orderByClauseFloor 是「非测试源码里能扫到的 ORDER BY 子句条数」的下限。
//
// 冻结时实测值：70（internal/store 的 40 个非测试 .go 文件；全仓 74 条里其余 4 条在
// 测试文件里，本守卫按契约只扫非测试源码）。条数掉了只有两种可能：有人删了查询
// （那就同步下调这个常量，并说明为什么），或者**扫描器自己失效了**（字面量拼接、
// 常量引用、注释掉整段 SQL……）—— 后者正是「绿与没检查同形」的事故形态。
const orderByClauseFloor = 70

// orderByKnownGapBudget 是已知缺口（末键不唯一且证不了安全）的条数棘轮。
//
// 当前 12 条，全部是「展示型列表只按时间列排序、没有游标翻页」的同一个形态，
// 逐条列在 orderByKnownGaps 里（带风险等级与修法）。修好一条就把它从表里删掉并把
// 这个常量减一；新增一条必须同时上调 —— 两个方向都红，不许静默膨胀也不许静默消失。
const orderByKnownGapBudget = 12

// TestStoreOrderByTiebreak 是全仓 store 的 ORDER BY 破平局门禁（L4）。
//
// 五个子测试分别守：扫描器真的扫到了东西（下限）、每条子句都有归属（判据四档）、
// 缺口预算的棘轮、schema 解析与真实库一致、以及守卫自己的双侧自证（变异必红）。
func TestStoreOrderByTiebreak(t *testing.T) {
	t.Run("扫到足够的 ORDER BY（反假绿下限）", func(t *testing.T) {
		clauses := orderByScanPackage(t)
		if len(clauses) == 0 {
			t.Fatalf("一条 ORDER BY 都没扫到：扫描器失效了（不是「没有 ORDER BY」——schema.sql 与 admin.go/auth.go 里必然有）")
		}
		if len(clauses) < orderByClauseFloor {
			var got []string
			for _, c := range clauses {
				got = append(got, fmt.Sprintf("%s:%d", c.File, c.Line))
			}
			t.Fatalf("只扫到 %d 条 ORDER BY，低于下限 %d。\n扫到的：%s\n"+
				"要么有人删了查询（同步下调 orderByClauseFloor 并写明原因），要么扫描器失效（常量拼接/多行字面量/注释掉整段 SQL）",
				len(clauses), orderByClauseFloor, strings.Join(got, " "))
		}
		// 契约点名的 10 个文件必须都出现在扫描结果里：缺一个说明某个文件的字面量
		// 抽取路径挂了（例如整文件都在常量里、或 SQL 换成非常量拼接）。
		perFile := map[string]int{}
		for _, c := range clauses {
			perFile[c.File]++
		}
		for _, want := range []string{
			"admin.go", "auth.go", "devices.go", "event_page.go", "node.go",
			"capability.go", "deployment.go", "artifact_manifest.go", "graph.go", "retention.go",
		} {
			if perFile[want] == 0 {
				t.Fatalf("%s 里一条 ORDER BY 都没扫到（契约点名的文件必须被覆盖；扫描器漏了这个文件的字面量）", want)
			}
		}
		t.Logf("扫描到 %d 条 ORDER BY（下限 %d），覆盖 %d 个文件", len(clauses), orderByClauseFloor, len(perFile))
	})

	t.Run("每条 ORDER BY 都属于判据四档之一", func(t *testing.T) {
		clauses := orderByScanPackage(t)
		tables := orderByParseSchema(t, schemaSQL)
		got := orderByAudit(clauses, tables, orderByAllowlist, orderByKnownGaps)
		if len(got.Problems) > 0 {
			t.Fatalf("ORDER BY 破平局判据不满足（%d 条问题）：\n%s",
				len(got.Problems), strings.Join(got.Problems, "\n"))
		}
		t.Logf("ORDER BY %d 条：末键自身唯一 %d 条、证明安全登记 %d 条、已知缺口 %d 条",
			got.Clauses, got.Auto, got.Allowed, got.Gaps)
	})

	t.Run("已知缺口条数受棘轮约束", func(t *testing.T) {
		if len(orderByKnownGaps) != orderByKnownGapBudget {
			t.Fatalf("已知缺口 %d 条，预算常量 orderByKnownGapBudget = %d。\n"+
				"修好一条 ⇒ 从 orderByKnownGaps 里删掉它并把常量减一；新增一条 ⇒ 常量加一并在表里写清为什么。",
				len(orderByKnownGaps), orderByKnownGapBudget)
		}
	})

	t.Run("schema 解析与真实库一致", func(t *testing.T) {
		// 解析器看漏一个 UNIQUE 索引/把 WITHOUT ROWID 当普通表，都会让上面的判据
		// 误放（或误杀）—— 所以拿真实建库结果逐表对账。
		db := newTestDB(t)
		tables := orderByParseSchema(t, schemaSQL)
		if len(tables) < 20 {
			t.Fatalf("schema.sql 只解析出 %d 张表，明显不对（v4 baseline 是 27 张）", len(tables))
		}
		names := make([]string, 0, len(tables))
		for name := range tables {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			shape := tables[name]
			// 主键：与 sqlite 的 pragma_table_info(pk>0) 完全一致（顺序也算）。
			wantPK := primaryKeyColumns(t, db, name)
			if strings.Join(shape.PK, ",") != strings.Join(wantPK, ",") {
				t.Errorf("表 %s 的主键解析 = %v，真实库 = %v", name, shape.PK, wantPK)
			}
			// 唯一键集合：主键 + UNIQUE 约束 + CREATE UNIQUE INDEX，逐集合比对。
			wantUnique := orderByDBUniqueSets(t, db, name)
			var gotUnique []string
			for _, u := range shape.Uniques {
				gotUnique = append(gotUnique, strings.Join(u.Cols, ","))
			}
			sort.Strings(gotUnique)
			if strings.Join(gotUnique, " | ") != strings.Join(wantUnique, " | ") {
				t.Errorf("表 %s 的唯一键集合解析 = %v，真实库 = %v", name, gotUnique, wantUnique)
			}
			// rowid：sqlite_master 的 DDL 里有没有 WITHOUT ROWID。
			if want := orderByDBWithoutRowID(t, db, name); shape.HasRowID == want {
				t.Errorf("表 %s 的 WITHOUT ROWID 判定不一致：解析 HasRowID=%v，真实库 WITHOUT ROWID=%v",
					name, shape.HasRowID, want)
			}
		}
	})

	t.Run("守卫双侧自证（变异必红）", func(t *testing.T) {
		orderBySelfTest(t)
	})
}

// ============================================================================
// 白名单：末键不唯一，但**能机械证明**不会因并列抖动
// ============================================================================
//
// 每条的 Clause 必须与 SQL 里 ORDER BY 之后的原文一致（归一化：小写 + 空白折叠）。
// 改 SQL 就会让登记项过期（红），改完必须回来同步这张表 —— 这正是它的作用：
// 让人没法「顺手改一句 ORDER BY」而不说清楚并列怎么破。
var orderByAllowlist = []orderByEntry{
	// ---- 证据①：WHERE 等值收窄 + 唯一索引覆盖排序键 ----
	{
		File: "artifact_manifest.go", Func: "uploadedManifestOn", Table: "task_artifact_manifests",
		Clause:   "order by path",
		Narrowed: []string{"task_id", "attempt"},
		Unique:   []string{"task_id", "attempt", "path"},
		Why: "PRIMARY KEY (task_id, attempt, path)：WHERE 已把前两列钉成常量，" +
			"同一 (task, attempt) 下 path 是唯一的 —— 末键并列不可能发生。",
	},
	{
		File: "artifact_manifest.go", Func: "ListArtifactManifest", Table: "task_artifact_manifests",
		Clause:   "order by path",
		Narrowed: []string{"namespace_id", "task_id", "attempt"},
		Unique:   []string{"task_id", "attempt", "path"},
		Why:      "同上：WHERE 钉住 task_id + attempt，path 在同一个 PK 里唯一。",
	},
	{
		File: "capability.go", Func: "getCapabilityByFoldKeyOn", Table: "capability_versions",
		Clause:   "order by version desc",
		Narrowed: []string{"namespace_id", "name", "fold_key"},
		Unique:   []string{"namespace_id", "name", "version"},
		Why: "idx_cap_live_name_ver 是**部分唯一索引**（WHERE deleted_at IS NULL），" +
			"而本查询的 WHERE 也带 deleted_at IS NULL、并把 namespace_id + name 钉成常量 " +
			"⇒ 活行里 (namespace_id, name, version) 唯一，version 就是末键。",
	},
	{
		File: "capability.go", Func: "ListCapabilities", Table: "capability_versions",
		Clause:   "order by name, version",
		Narrowed: []string{"namespace_id"},
		Unique:   []string{"namespace_id", "name", "version"},
		Why: "同一部分唯一索引：WHERE namespace_id=? + deleted_at IS NULL，" +
			"排序键 (name, version) 补齐了索引的剩余两列 ⇒ 活行里 (name, version) 唯一。",
	},
	{
		File: "capability.go", Func: "ListCapabilitiesPage", Table: "capability_versions",
		Clause:   "order by name, version desc",
		Narrowed: []string{"namespace_id"},
		Unique:   []string{"namespace_id", "name", "version"},
		Why: "keyset 分页版，同一句 WHERE（namespace_id + deleted_at IS NULL）⇒ " +
			"(name, version) 在活行里唯一，页边界不会因为并列重复/丢行。",
	},
	{
		File: "graph.go", Func: "EdgesFrom", Table: "edges",
		Clause:   "order by to_ref",
		Narrowed: []string{"namespace_id", "from_is_source", "from_ref"},
		Unique:   []string{"namespace_id", "from_is_source", "from_ref", "to_ref"},
		Why: "PRIMARY KEY (namespace_id, from_is_source, from_ref, to_ref)：" +
			"WHERE 把前三列钉成常量，to_ref 在同一个上游下唯一。",
	},
	{
		File: "graph.go", Func: "listEdgesOn", Table: "edges",
		Clause:   "order by from_is_source, from_ref, to_ref",
		Narrowed: []string{"namespace_id"},
		Unique:   []string{"namespace_id", "from_is_source", "from_ref", "to_ref"},
		Why:      "排序键就是主键去掉 namespace_id（WHERE 已钉常量）⇒ 整行身份唯一。",
	},
	{
		File: "node.go", Func: "ListInstances", Table: "node_instances",
		Clause:   "order by slot",
		Narrowed: []string{"node_id"},
		Unique:   []string{"node_id", "slot"},
		Why: "UNIQUE (node_id, slot)：WHERE node_id=? 之后 slot 在节点内唯一" +
			"（槽位语义本来就是「每槽最多一个」）。",
	},
	{
		File: "playbook.go", Func: "ListPlaybookInstalls", Table: "sources",
		Clause:   "order by name",
		Narrowed: []string{"namespace_id"},
		Unique:   []string{"namespace_id", "name"},
		Why:      "UNIQUE (namespace_id, name)：源名在命名空间内唯一，WHERE 已钉 namespace_id。",
	},
	{
		File: "playbook_tx.go", Func: "getCapabilityByFoldKeyLatestOn", Table: "capability_versions",
		Clause:   "order by version desc",
		Narrowed: []string{"namespace_id", "name", "fold_key"},
		Unique:   []string{"namespace_id", "name", "version"},
		Why: "安装快照的 foldKey 取最高版本：WHERE 钉住 namespace_id + name + fold_key " +
			"且带 deleted_at IS NULL ⇒ 活行里 (namespace_id, name, version) 唯一，" +
			"所以「取哪一行」是确定的（这正是该函数注释里承诺的确定性）。",
	},
	{
		File: "playbook_tx.go", Func: "keptPlaybookObjectsOn", Table: "capability_versions",
		Clause:   "order by name, version",
		Narrowed: []string{"namespace_id"},
		Unique:   []string{"namespace_id", "name", "version"},
		Why: "同一部分唯一索引（WHERE 里有 namespace_id=? 与 deleted_at IS NULL）⇒ " +
			"(name, version) 在活行里唯一，保留集快照的顺序是确定的。",
	},
	{
		File: "playbook_tx.go", Func: "keptPlaybookObjectsOn", Table: "sources",
		Clause:   "order by name",
		Narrowed: []string{"namespace_id"},
		Unique:   []string{"namespace_id", "name"},
		Why:      "UNIQUE (namespace_id, name)：WHERE 钉 namespace_id，源名唯一。",
	},
	{
		File: "ref.go", Func: "getCapabilityLatestOn", Table: "capability_versions",
		Clause:   "order by version desc",
		Narrowed: []string{"namespace_id", "name"},
		Unique:   []string{"namespace_id", "name", "version"},
		Why: "「解析裸名 = latest 版本」的唯一口径：WHERE namespace_id + name 钉常量、" +
			"deleted_at IS NULL 命中部分唯一索引 ⇒ version 在返回集里唯一。",
	},
	{
		File: "task_attempt.go", Func: "ListTaskAttempts", Table: "task_attempts",
		Clause:   "order by attempt",
		Narrowed: []string{"task_id"},
		Unique:   []string{"task_id", "attempt"},
		Why: "PRIMARY KEY (task_id, attempt)：WHERE task_id=? 之后 attempt 唯一" +
			"（attempt 本来就是任务内的执行序号）。",
	},
	{
		File: "task_attempt.go", Func: "PreviousTaskAttempt", Table: "task_attempts",
		Clause:   "order by attempt desc",
		Narrowed: []string{"task_id"},
		Unique:   []string{"task_id", "attempt"},
		Why:      "同上（attempt 的范围谓词不只影响唯一性：task_id 已钉常量 ⇒ attempt 唯一）。",
	},

	// ---- 证据②：聚合后分组键天然唯一 ----
	{
		File: "admin.go", Func: "SystemStats", Table: "events",
		Clause:   "order by day",
		GroupKey: "day",
		Why: "按 strftime('%Y-%m-%d') GROUP BY day 之后再 ORDER BY day：每天只有一行结果，" +
			"末键就是分组键本身 ⇒ 并列不可能发生。",
	},
	{
		File: "stats.go", Func: "messageStats", Table: "events",
		Clause:   "order by day",
		GroupKey: "day",
		Why:      "同上：GROUP BY day 的每组一行，ORDER BY day 的末键就是分组键。",
	},

	// ---- 证据③：GC 批量删除（并列只影响本批成员，不影响不变式）----
	{
		File: "retention.go", Func: "DeleteExpiredTasks", Table: "tasks",
		Clause:  "order by coalesce(t.result_accepted_at, case when t.reported=0 then t.finished_at end)",
		GCBatch: true,
		Why: "DELETE ... WHERE id IN (SELECT ... ORDER BY <过期时刻> LIMIT n)：这条 ORDER BY 只决定" +
			"「这一批先删哪 n 行」。并列 ⇒ 换一批同样过期的行，下一轮继续删，不变式（过期行终将删光）" +
			"不受影响，也没有游标会漂。",
	},
	{
		File: "retention.go", Func: "DeleteExpiredEvents", Table: "events",
		Clause:  "order by e.created_at",
		GCBatch: true,
		Why:     "同上的 GC 批量删除：ORDER BY created_at 只影响本批成员，并列无害。",
	},
	{
		File: "retention.go", Func: "DeleteExpiredEventDeliveries", Table: "event_deliveries",
		Clause:  "order by expires_at",
		GCBatch: true,
		Why:     "同上的 GC 批量删除：去重窗过期的行迟早都会被删，并列只影响批次构成。",
	},
	{
		File: "retention.go", Func: "DeleteExpiredPlaybookInstalls", Table: "playbook_installs",
		Clause:  "order by pi.created_at",
		GCBatch: true,
		Why:     "同上的 GC 批量删除：并列只影响本批删哪几行回执。",
	},
	{
		File: "retention.go", Func: "PurgeDeletedCapabilities", Table: "capability_versions",
		Clause:  "order by c.deleted_at,c.id",
		GCBatch: true,
		Why: "同上的 GC 批量删除（末键 c.id 其实是主键，只是别名在子查询里、自动路径解析不到）；" +
			"按 deleted_at 排序只决定先清哪一批软删行。",
	},

	// ---- 证据④：目录表（SQLite 内省），行来源天然唯一 ----
	{
		File: "migrate.go", Func: "userSchemaObjects", Table: "sqlite_master",
		Clause:     "order by type, name",
		MetaUnique: []string{"name"},
		Why: "sqlite_master 的 name 在库内唯一（对象名不许重名）⇒ ORDER BY type, name 的末键唯一；" +
			"这条排序只是让 schema 指纹列表可比对。",
	},
	{
		File: "migrate.go", Func: "userTables", Table: "sqlite_master",
		Clause:     "order by name",
		MetaUnique: []string{"name"},
		Why:        "同上：sqlite_master.name 唯一，按它排序即全序。",
	},
	{
		File: "migrate.go", Func: "foreignKeysDB", Table: "pragma_foreign_key_list",
		Clause:     "order by id, seq",
		MetaUnique: []string{"id", "seq"},
		Why: "pragma_foreign_key_list 一行 = 一条外键的某一列：(id, seq) 是这张虚拟表的行身份" +
			"（id = 约束序号，seq = 约束内列序）⇒ 排序键自身唯一。",
	},
	{
		File: "migrate.go", Func: "uniqueIndexesDB", Table: "pragma_index_list",
		Clause:     "order by seq",
		MetaUnique: []string{"seq"},
		Why:        "pragma_index_list 的 seq 是表内索引序号（pk 为 0，其余 1..n），单表结果里唯一。",
	},
	{
		File: "migrate.go", Func: "indexKeyColumnsDB", Table: "pragma_index_xinfo",
		Clause:     "order by seqno",
		MetaUnique: []string{"seqno"},
		Why:        "pragma_index_xinfo 的 seqno 是索引内列序，单索引结果里唯一。",
	},
}

// ============================================================================
// 已知缺口：末键确实不唯一，而且证不了安全
// ============================================================================
//
// 这一档是**待裁决清单**，不是「允许」清单：每条都是真违规（判据要求末键唯一），
// 只是它的并列抖动目前只影响展示顺序、不影响数据正确性。修法都是同一招：
// 在末键后面补一个唯一键（`, rowid` / `, id`）。条数受 orderByKnownGapBudget 棘轮约束。
var orderByKnownGaps = []orderByGap{
	{
		File: "admin.go", Func: "ListUsersWithStats", Clause: "order by u.created_at asc",
		Why: "admin 用户列表一次取全量（admin_handlers.go:168 拿 all 再在内存里筛），没有游标翻页",
		Risk: "中 —— 同一秒注册的两个用户谁在前不确定；#377 只修了分页版（ListUsersWithStatsPage 用 rowid），" +
			"这一版没修，任何断言「首注册用户排最前」的用例仍可能随机红（正是 1461f7e4 的原始形态）",
		Fix: "ORDER BY u.created_at ASC, u.rowid ASC（与分页版同一破平局键，语义 = 注册先后）",
	},
	{
		File: "admin.go", Func: "SystemStats", Clause: "order by coalesce((select max(s2.last_seen) from sessions s2 where s2.user_id = u.id), 0) desc, u.created_at asc",
		Why:  "「最近活跃的前 10 个用户」展示榜，LIMIT 10，无游标",
		Risk: "低 —— 并列发生在 last_seen + created_at 都相同的两个用户之间，只影响榜单边缘谁上榜，且每次刷新可能换人",
		Fix:  "末键补 `, u.rowid`",
	},
	{
		File: "admin.go", Func: "ListGlobalEvents", Clause: "order by created_at desc",
		Why:  "admin 全局事件流，单次 LIMIT 取前 N 条，无游标（事件分页走 event_page.go 的 created_at+id）",
		Risk: "低-中 —— 同一毫秒/同秒落库的事件在前端先后不确定；若日后在这个接口上加游标翻页会立刻变成丢行/重复行（event_page 已经因为同一原因必须带 id）",
		Fix:  "ORDER BY created_at DESC, id DESC",
	},
	{
		File: "admin.go", Func: "ListAllSessions", Clause: "order by s.last_seen desc",
		Why:  "admin 会话总览，LIMIT 500 一次取全，无游标",
		Risk: "低 —— 并列只影响同一 last_seen 的会话在列表里的先后",
		Fix:  "末键补 `, s.id`",
	},
	{
		File: "auth.go", Func: "ListPasskeysByUser", Clause: "order by created_at asc",
		Why:  "某用户的 passkey 列表，一次取全（登录时的候选断言列表也用这份），无游标",
		Risk: "低 —— 同秒注册的多个 passkey 顺序不确定；登录侧按「全部候选都试一遍」消费，不依赖顺序",
		Fix:  "末键补 `, id`",
	},
	{
		File: "auth.go", Func: "ListSessionsByUser", Clause: "order by created_at desc",
		Why:  "「我的设备/会话」列表，一次取全，无游标",
		Risk: "低 —— 同秒创建的两个会话先后不确定（分页版 ListSessionsByUserPage 已带 id 破平局）",
		Fix:  "ORDER BY created_at DESC, id DESC",
	},
	{
		File: "auth.go", Func: "ListAPIKeysByUser", Clause: "order by created_at desc",
		Why:  "某用户的 API key 列表，一次取全，无游标",
		Risk: "低 —— 同秒创建的 key 先后不确定（分页版 ListAPIKeysByUserPage 已带 id）",
		Fix:  "ORDER BY created_at DESC, id DESC",
	},
	{
		File: "auth.go", Func: "ListUnusedRecoveryCodesByUser", Clause: "order by created_at asc",
		Why:  "未使用恢复码列表；恢复码是**批量签发**的，同一批必然同秒",
		Risk: "低 —— 同一批签发的恢复码之间顺序随机；目前没有调用方依赖这个顺序",
		Fix:  "末键补 `, id`",
	},
	{
		File: "deployment.go", Func: "CreateDeployments", Clause: "order by d.node_id",
		Why:  "写事务提交后回读刚建的部署行，调用方（ws/hub.go:598）只做集合消费，不取首行",
		Risk: "低 —— 同一节点上同一能力的多行（不同 digest）先后不确定；若日后有人改成「取第一行」会读到随机 digest",
		Fix:  "末键补 `, d.id`",
	},
	{
		File: "devices.go", Func: "ListDevices", Clause: "order by created_at asc",
		Why:  "用户的推送设备列表，一次取全（handlers.go:51/116），无游标",
		Risk: "低 —— 同秒添加的两个设备先后不确定",
		Fix:  "末键补 `, id`",
	},
	{
		File: "devices.go", Func: "ListEnabledDevices", Clause: "order by created_at asc",
		Why:  "推送扇出的设备集（push/central.go:32），顺序不参与语义",
		Risk: "低 —— 同上；扇出顺序会影响 push 的投递先后，但对正确性无影响",
		Fix:  "末键补 `, id`",
	},
	{
		File: "playbook.go", Func: "ListPlaybookInstalls", Clause: "order by name, version",
		Why: "安装回执里「这个安装装过哪些能力」的展示集，一次取全",
		Risk: "低 —— WHERE 没有 deleted_at IS NULL，部分唯一索引 idx_cap_live_name_ver 不适用，" +
			"软删行可能与活行撞 (name, version) ⇒ 展示顺序不确定（不会丢行，因为这是全量而不是翻页）",
		Fix: "先补 `AND deleted_at IS NULL`（配合部分唯一索引即可证明唯一），或末键补 `, id`",
	},
}

// ============================================================================
// 判据实现
// ============================================================================

// orderByClause 是一条被扫到的 ORDER BY 子句（连同解析出的上下文）。
type orderByClause struct {
	File    string            // 文件名（不含目录）
	Line    int               // ORDER BY 关键字所在行（跨字面量时为该字面量的起始行 + 偏移行数）
	Func    string            // 所在函数名（登记表的键之一）
	Clause  string            // 归一化子句：小写、空白折叠、以 "order by " 开头
	Keys    []string          // 全部排序键（去掉 ASC/DESC/COLLATE 尾巴，保留原文大小写前的归一化形态）
	LastKey string            // 末键（已去掉 ASC/DESC）
	Refs    []orderByTableRef // 语句窗口里顶层（括号深度 0）出现的表引用
	Window  string            // 归一化的语句窗口（从语句起始关键字到 ORDER BY 之前）
}

// orderByTableRef 是语句窗口里的一张表（及其别名）。
type orderByTableRef struct {
	Table string
	Alias string
}

// orderByEntry 是一条「末键不唯一但可以证明安全」的登记项。
//
// 证据四选一（至少一条，且会被机械复核，见 orderByEvidenceProblems）。
type orderByEntry struct {
	File   string // 文件名
	Func   string // 函数名
	Clause string // 归一化 ORDER BY 子句（改 SQL 即过期）
	Table  string // 该子句作用的表（必须是语句里真实出现的顶层表）
	Why    string // 人话理由

	Narrowed   []string // 证据①：WHERE 里用 `= ?` 收窄成常量的列
	Unique     []string // 证据①：覆盖 (Narrowed ∪ 排序键) 的唯一索引列（前缀/全匹配都算）
	GroupKey   string   // 证据②：聚合分组键（必须等于末键）
	GCBatch    bool     // 证据③：GC 批量删除
	MetaUnique []string // 证据④：目录表里天然唯一的列（必须被排序键覆盖）
}

// orderByGap 是一条「末键确实不唯一、而且证不了安全」的已知缺口（待裁决清单）。
type orderByGap struct {
	File   string
	Func   string
	Clause string
	Why    string // 为什么现在还没出事
	Risk   string // 风险等级 + 具体影响
	Fix    string // 建议修法
}

// orderByTableShape 是一张表的唯一性事实（从 schema.sql 解析）。
type orderByTableShape struct {
	Name     string
	HasRowID bool // 不是 WITHOUT ROWID ⇒ rowid 唯一可用
	PK       []string
	Uniques  []orderByUniqueKey // 主键 + UNIQUE 约束 + CREATE UNIQUE INDEX
}

// orderByUniqueKey 是一条唯一约束（或部分唯一索引）。
type orderByUniqueKey struct {
	Cols    []string
	Partial string // 归一化的 WHERE 谓词；非空 = 部分唯一索引，只在谓词内唯一
	Source  string // 出处（主键 / UNIQUE 约束 / 索引名），只用于失败信息
}

// orderByAuditResult 是一次判据跑批的结果（条数用于打印与反假绿）。
type orderByAuditResult struct {
	Clauses  int
	Auto     int // 末键自身唯一
	Allowed  int // 白名单命中
	Gaps     int // 已知缺口命中
	Problems []string
}

// orderByAudit 是这条门禁的**纯函数判据**：子句集合 + schema + 两张登记表 → 问题清单。
//
// 拆成纯函数是为了「双侧自证」：变异用例（orderBySelfTest）用同一份逻辑跑合成输入，
// 证明它真的会红，而不是恰好对所有真实输入都放行。
func orderByAudit(clauses []orderByClause, tables map[string]*orderByTableShape,
	allow []orderByEntry, gaps []orderByGap) orderByAuditResult {
	var res orderByAuditResult
	res.Clauses = len(clauses)
	if len(clauses) == 0 {
		res.Problems = append(res.Problems,
			"一条 ORDER BY 都没扫到：扫描器失效（0 条不等于「没有 ORDER BY」）")
		return res
	}

	entryHit := make([]int, len(allow))
	gapHit := make([]int, len(gaps))

	for _, c := range clauses {
		if orderByAutoUnique(c, tables) {
			res.Auto++
			continue
		}

		// 表必须是人认识的：schema.sql 里的表，或显式登记的目录表。
		for _, ref := range c.Refs {
			if _, ok := tables[ref.Table]; ok {
				continue
			}
			if _, ok := orderByMetaTables[ref.Table]; ok {
				continue
			}
			res.Problems = append(res.Problems, fmt.Sprintf(
				"%s:%d %s(): 表 %q 既不在 schema.sql 里，也不在目录表白名单里 —— "+
					"新增表（可能在 Go 代码里建）必须先在 orderByMetaTables 登记，否则唯一性无从判断",
				c.File, c.Line, c.Func, ref.Table))
		}

		var allowIdx, gapIdx = -1, -1
		for i, e := range allow {
			if e.File == c.File && e.Func == c.Func && e.Clause == c.Clause {
				allowIdx = i
				break
			}
		}
		for i, g := range gaps {
			if g.File == c.File && g.Func == c.Func && g.Clause == c.Clause {
				gapIdx = i
				break
			}
		}
		switch {
		case allowIdx >= 0 && gapIdx >= 0:
			res.Problems = append(res.Problems, fmt.Sprintf(
				"%s:%d %s(): 同一条子句同时登记为「证明安全」和「已知缺口」—— 二者只能选一个",
				c.File, c.Line, c.Func))
		case allowIdx >= 0:
			entryHit[allowIdx]++
			res.Allowed++
			res.Problems = append(res.Problems, orderByEvidenceProblems(c, allow[allowIdx], tables)...)
		case gapIdx >= 0:
			gapHit[gapIdx]++
			res.Gaps++
			if strings.TrimSpace(gaps[gapIdx].Why) == "" ||
				strings.TrimSpace(gaps[gapIdx].Risk) == "" ||
				strings.TrimSpace(gaps[gapIdx].Fix) == "" {
				res.Problems = append(res.Problems, fmt.Sprintf(
					"%s:%d %s(): 已知缺口登记缺字段（Why/Risk/Fix 都必须写清），壳子登记不许过",
					c.File, c.Line, c.Func))
			}
		default:
			res.Problems = append(res.Problems, orderByViolation(c, tables))
		}
	}

	// 过期即红：登记项必须仍然命中真实子句。
	for i, e := range allow {
		if entryHit[i] == 0 {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"白名单已过期：%s %s() 的 %q 不再命中任何 ORDER BY（SQL 改过或函数改名）—— "+
					"如果这条已经满足判据就删掉登记项；只是改了写法就同步更新 Clause",
				e.File, e.Func, e.Clause))
		}
	}
	for i, g := range gaps {
		if gapHit[i] == 0 {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"已知缺口已过期：%s %s() 的 %q 不再命中任何 ORDER BY —— 修好就把它删掉，"+
					"并同步下调 orderByKnownGapBudget", g.File, g.Func, g.Clause))
		}
	}
	return res
}

// orderByMetaTables 是 SQLite 目录虚表的登记（只用来判定「这不是 schema.sql 里的表」）。
//
// 这些表的行来自 SQLite 内省，权威定义在 SQLite 文档里（见各条白名单理由），
// 因此不进 schema.sql 的解析结果，但必须显式点名 —— 不能让未知表名静默通过。
var orderByMetaTables = map[string]string{
	"sqlite_master":           "库对象目录（type/name/tbl_name/rootpage/sql）",
	"pragma_foreign_key_list": "外键列表（id=约束序号, seq=约束内列序）",
	"pragma_index_list":       "索引列表（seq=表内索引序号, unique, origin, partial）",
	"pragma_index_xinfo":      "索引列（seqno=索引内列序）",
}

// orderByAutoUnique 判断「末键自身就是唯一键」：rowid / 单列主键 / 单列 UNIQUE。
//
// 多表（join）时按表限定符定位；键不带限定符时看全部顶层表里有没有任何一张在该列上
// 单列唯一 —— 不带限定符而两表同名的列，SQLite 自己会报 ambiguous，走不到这里。
func orderByAutoUnique(c orderByClause, tables map[string]*orderByTableShape) bool {
	col := orderByStripQualifier(c.LastKey)
	alias := orderByQualifier(c.LastKey)

	// rowid：表不是 WITHOUT ROWID 时唯一（WITHOUT ROWID 表根本没有 rowid 列）。
	if col == "rowid" || col == "_rowid_" || col == "oid" {
		for _, ref := range c.Refs {
			if alias != "" && ref.Alias != alias {
				continue
			}
			if t, ok := tables[ref.Table]; ok && t.HasRowID {
				return true
			}
		}
		return false
	}

	for _, ref := range c.Refs {
		if alias != "" && ref.Alias != alias {
			continue
		}
		t, ok := tables[ref.Table]
		if !ok {
			continue
		}
		for _, u := range t.Uniques {
			if u.Partial == "" && len(u.Cols) == 1 && u.Cols[0] == col {
				return true
			}
		}
	}
	return false
}

// orderByViolation 生成一条「没有任何归属」的失败信息。
//
// 契约要求：文件:行 + 表名 + 子句原文 + 为什么末键不唯一 + 修法建议。
func orderByViolation(c orderByClause, tables map[string]*orderByTableShape) string {
	var b strings.Builder

	// 先按表限定符定位表（join 语句里 `d.id` 指的是哪张表必须看别名），
	// 定位不到就退回第一张顶层表 —— 失败信息宁可粗一点，也要指名道姓。
	tbl := "?"
	var shape *orderByTableShape
	alias := orderByQualifier(c.LastKey)
	for _, ref := range c.Refs {
		if alias != "" && ref.Alias != alias {
			continue
		}
		if s, ok := tables[ref.Table]; ok {
			tbl, shape = ref.Table, s
			break
		}
	}
	if shape == nil && len(c.Refs) > 0 {
		tbl = c.Refs[0].Table
		shape = tables[tbl]
	}

	// 为什么末键不唯一
	reason := ""
	col := orderByStripQualifier(c.LastKey)
	switch {
	case shape != nil && orderByIsExpression(c.LastKey):
		reason = fmt.Sprintf("末键 %q 是表达式（而非列），表上没有任何唯一键能覆盖它", c.LastKey)
	case shape != nil:
		var singles []string
		for _, u := range shape.Uniques {
			if u.Partial == "" && len(u.Cols) == 1 {
				singles = append(singles, u.Cols[0])
			}
		}
		if col == "rowid" || col == "_rowid_" || col == "oid" {
			reason = fmt.Sprintf("末键是 %q，但表 %s 是 WITHOUT ROWID（没有 rowid 列）", col, tbl)
		} else if len(singles) == 0 {
			reason = fmt.Sprintf("末键 %q 所在的表 %s 没有单列唯一键（实际唯一键：%s）", col, tbl,
				strings.Join(orderByUniqueBrief(shape), "，"))
		} else {
			reason = fmt.Sprintf("末键 %q 不是唯一键（表 %s 的单列唯一键只有 %s）",
				col, tbl, strings.Join(singles, " / "))
		}
	case col == "rowid" || col == "_rowid_" || col == "oid":
		reason = fmt.Sprintf("末键是 %q，但没有解析到它属于哪张表 —— 无法确认 rowid 可用", col)
	default:
		reason = "没有解析到这条子句作用的表，无法判断末键唯一性"
	}

	fmt.Fprintf(&b, "%s:%d %s(): 表 %s 的 ORDER BY 末键 %q 不唯一（子句原文：%q）\n"+
		"    —— %s。\n"+
		"    —— 修法：末键后面补一个唯一键（`, rowid` 即插入序，或 `, id` 即主键列）；"+
		"若确实不需要破平局（聚合/GC 批量/目录表），登记进 orderByAllowlist 并写清可核验的证据。\n"+
		"    —— 事故形态参考 commit 1461f7e4：只按时间列排序 ⇒ 同毫秒两行谁在前是掷硬币，翻页丢行/重复行、断言随机红。",
		c.File, c.Line, c.Func, tbl, c.LastKey, c.Clause, reason)
	return b.String()
}

// orderByIsExpression 判断排序键是不是表达式（函数/运算符/CASE）而不是普通列。
func orderByIsExpression(key string) bool {
	return strings.ContainsAny(key, "( ,+'*/") || strings.Contains(strings.ToUpper(key), "CASE")
}

// orderByUniqueBrief 把一张表的唯一键简述出来（失败信息用）。
func orderByUniqueBrief(shape *orderByTableShape) []string {
	var out []string
	for _, u := range shape.Uniques {
		s := "(" + strings.Join(u.Cols, ",") + ")"
		if u.Partial != "" {
			s += " WHERE " + u.Partial
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{"无"}
	}
	return out
}

// orderByEvidenceProblems 机械复核一条白名单的证据（口说无凭一律红）。
func orderByEvidenceProblems(c orderByClause, e orderByEntry, tables map[string]*orderByTableShape) []string {
	var probs []string
	bad := func(format string, args ...any) {
		probs = append(probs, fmt.Sprintf("%s:%d %s(): 白名单证据不成立 —— ", c.File, c.Line, c.Func)+
			fmt.Sprintf(format, args...))
	}
	if strings.TrimSpace(e.Why) == "" {
		bad("Why 不能为空（这条为什么不会因为并列抖动）")
		return probs
	}
	if len(e.Unique) == 0 && e.GroupKey == "" && !e.GCBatch && len(e.MetaUnique) == 0 {
		bad("没有给任何证据（Unique+Narrowed / GroupKey / GCBatch / MetaUnique 至少给一条）")
		return probs
	}

	keys := map[string]bool{}
	for _, k := range c.Keys {
		keys[orderByStripQualifier(k)] = true
	}

	if len(e.Unique) > 0 {
		if e.Table == "" {
			bad("声明了 Unique 证据却没写作用于哪张表（Table）")
			return probs
		}
		inRefs := false
		for _, ref := range c.Refs {
			if ref.Table == e.Table {
				inRefs = true
			}
		}
		if !inRefs {
			bad("声明的表 %q 不在这条语句的顶层表里（%s）", e.Table, strings.Join(orderByRefNames(c.Refs), " "))
			return probs
		}
		shape := tables[e.Table]
		if shape == nil {
			bad("表 %q 不在 schema.sql 里", e.Table)
			return probs
		}
		// 收窄列必须真的以 `col = ?` 出现在窗口里（等值 = 常量，才能当索引前缀用）。
		covered := map[string]bool{}
		for _, col := range e.Narrowed {
			if !orderByHasEquality(c.Window, col) {
				bad("声明的收窄列 %q 并没有以 `= ?` 出现在这条语句的 WHERE 里", col)
				return probs
			}
			covered[col] = true
		}
		for k := range keys {
			covered[k] = true
		}
		matched := false
		for _, u := range shape.Uniques {
			if strings.Join(u.Cols, ",") != strings.Join(e.Unique, ",") {
				continue
			}
			ok := true
			for _, col := range u.Cols {
				if !covered[col] {
					ok = false
				}
			}
			if !ok {
				continue
			}
			// 部分唯一索引只在谓词成立的行上唯一：查询的 WHERE 必须蕴含那个谓词。
			if u.Partial != "" && !strings.Contains(c.Window, u.Partial) {
				bad("唯一键 %v 是部分唯一索引（WHERE %s），但这条语句的 WHERE 里没有该谓词 ⇒ 不唯一", u.Cols, u.Partial)
				return probs
			}
			matched = true
			break
		}
		if !matched {
			bad("表 %s 上找不到列组合 %v 的唯一键（真实唯一键：%s），或者收窄列 + 排序键覆盖不了它",
				e.Table, e.Unique, strings.Join(orderByUniqueBrief(shape), "，"))
			return probs
		}
	}

	if e.GroupKey != "" {
		if orderByStripQualifier(c.LastKey) != orderByStripQualifier(e.GroupKey) {
			bad("GroupKey %q 与末键 %q 不一致（聚合证据要求末键就是分组键）", e.GroupKey, c.LastKey)
			return probs
		}
		groupKeys, ok := orderByGroupKeys(c.Window)
		if !ok {
			bad("这条语句里没有 GROUP BY，却声明了聚合证据")
			return probs
		}
		if !groupKeys[orderByStripQualifier(e.GroupKey)] {
			bad("GROUP BY 里没有 %q（实际分组键：%v）", e.GroupKey, orderBySortedKeys(groupKeys))
			return probs
		}
	}

	if e.GCBatch {
		if !strings.Contains(c.Window, "delete from") {
			bad("GC 批量删除证据要求语句是 DELETE ... WHERE ... IN (SELECT ... ORDER BY ... LIMIT n)，这里没看到 delete from")
			return probs
		}
	}

	if len(e.MetaUnique) > 0 {
		if _, ok := orderByMetaTables[e.Table]; !ok {
			bad("MetaUnique 证据只能用在 orderByMetaTables 登记的目录表上（Table=%q）", e.Table)
			return probs
		}
		inRefs := false
		for _, ref := range c.Refs {
			if ref.Table == e.Table {
				inRefs = true
			}
		}
		if !inRefs {
			bad("声明的目录表 %q 不在这条语句的顶层表里", e.Table)
			return probs
		}
		for _, col := range e.MetaUnique {
			if !keys[col] {
				bad("声明的目录表唯一列 %q 不在排序键里（排序键：%v）", col, orderBySortedKeys(keys))
				return probs
			}
		}
	}
	return probs
}

// orderByHasEquality 判断归一化窗口里是否有 `col = ?`（允许表限定与空格）。
func orderByHasEquality(window, col string) bool {
	re := regexp.MustCompile(`(?:^|[\s(,])(?:[a-z_][\w]*\.)?` + regexp.QuoteMeta(col) + `\s*=\s*\?`)
	return re.MatchString(window)
}

// orderByGroupKeys 取归一化窗口里 GROUP BY 的分组键集合。
func orderByGroupKeys(window string) (map[string]bool, bool) {
	idx := strings.Index(window, "group by")
	if idx < 0 {
		return nil, false
	}
	rest := window[idx+len("group by"):]
	// 分组键到 ORDER BY / LIMIT / HAVING 之前为止（窗口本身已截到 ORDER BY 之前）。
	for _, stop := range []string{" having ", " limit ", " window "} {
		if i := strings.Index(rest, stop); i >= 0 {
			rest = rest[:i]
		}
	}
	out := map[string]bool{}
	for _, k := range orderBySplitTop(rest) {
		out[orderByStripQualifier(orderByCleanKey(k))] = true
	}
	return out, true
}

func orderByRefNames(refs []orderByTableRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Alias != "" {
			out = append(out, r.Table+"("+r.Alias+")")
			continue
		}
		out = append(out, r.Table)
	}
	return out
}

func orderBySortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ============================================================================
// 扫描器：从 Go 源码里把 ORDER BY 子句捞出来
// ============================================================================

// orderByScanPackage 扫本包全部非测试源码。
func orderByScanPackage(t *testing.T) []orderByClause {
	t.Helper()
	sources := orderByReadPackageSources(t)
	return orderByScanSources(t, sources)
}

// orderByReadPackageSources 读取本包非测试 .go 源码（文件名 → 内容）。
func orderByReadPackageSources(t *testing.T) map[string]string {
	t.Helper()
	patterns := []string{"*.go", filepath.Join("internal", "store", "*.go")}
	var paths []string
	for _, pat := range patterns {
		got, err := filepath.Glob(pat)
		if err != nil {
			t.Fatalf("glob %s: %v", pat, err)
		}
		if len(got) > 10 { // 找对了包目录
			paths = got
			break
		}
	}
	if len(paths) == 0 {
		t.Fatalf("没找到本包的源码（工作目录 = %q）：显然不是从包目录跑的，扫描器无法工作", ".")
	}
	out := map[string]string{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		buf, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		out[filepath.Base(p)] = string(buf)
	}
	if len(out) < 30 {
		t.Fatalf("只读到 %d 个非测试源文件，太少了（扫描面不完整会假绿）", len(out))
	}
	return out
}

// orderByScanSources 扫一组 Go 源（文件名 → 源码），返回其中 SQL 字面量里的 ORDER BY 子句。
//
// 两步：先把**同一函数**里的字符串字面量按源码顺序拼成 blob（变量拼接 `q += ...`
// 因此能接上前面那句 FROM），再在 blob 里找 ORDER BY 并按字面量边界/顶层关键字
// （LIMIT/UNION/OFFSET/结束括号）截出子句。包级字符串常量（nodeColumns 之类）按
// 引用展开 —— deploymentSelect 这种常量里就有 FROM。
func orderByScanSources(t *testing.T, sources map[string]string) []orderByClause {
	t.Helper()
	fset := token.NewFileSet()
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)

	var files []*ast.File
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, sources[name], parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	consts := orderByConstExprs(files)

	var out []orderByClause
	for _, file := range files {
		fname := filepath.Base(fset.Position(file.Pos()).Filename)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			blob := orderByFuncBlob(fset, consts, fn)
			out = append(out, orderByClausesInBlob(fname, fn.Name.Name, blob)...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	for i := range out {
		out[i].Refs = orderByTopTableRefs(out[i])
	}
	return out
}

// orderBySpan 是 blob 里一段字面量的位置（用于把偏移映射回文件行号）。
type orderBySpan struct {
	start, end int
	line       int
	file       string
}

// orderByBlob 是一个函数里拼起来的 SQL 文本。
type orderByBlob struct {
	text  string
	spans []orderBySpan
}

// orderByConstExprs 收集包级 const/var 的初值表达式（初值本身可以是 + 拼接或引用别的常量）。
func orderByConstExprs(files []*ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if _, dup := out[name.Name]; dup {
						// 同名多份（不该发生）：当作不可解析，宁可少拼也不要拼错。
						out[name.Name] = nil
						continue
					}
					out[name.Name] = vs.Values[i]
				}
			}
		}
	}
	return out
}

// orderByFuncBlob 把一个函数体里的字符串字面量按源码顺序拼成 blob。
func orderByFuncBlob(fset *token.FileSet, consts map[string]ast.Expr, fn *ast.FuncDecl) orderByBlob {
	var sb strings.Builder
	var spans []orderBySpan
	var walk func(expr ast.Expr, seen map[string]bool)
	walk = func(expr ast.Expr, seen map[string]bool) {
		switch e := expr.(type) {
		case *ast.BasicLit:
			if e.Kind != token.STRING {
				return
			}
			raw, err := strconv.Unquote(e.Value)
			if err != nil {
				return
			}
			pos := fset.Position(e.Pos())
			spans = append(spans, orderBySpan{start: sb.Len(), end: sb.Len() + len(raw), line: pos.Line, file: pos.Filename})
			sb.WriteString(raw)
		case *ast.Ident:
			init, ok := consts[e.Name]
			if !ok || init == nil || seen[e.Name] {
				return
			}
			seen[e.Name] = true
			walk(init, seen)
		case *ast.BinaryExpr:
			if e.Op != token.ADD {
				return
			}
			walk(e.X, seen)
			walk(e.Y, seen)
		case *ast.ParenExpr:
			walk(e.X, seen)
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		// 遇到 + 拼接就整体展开（并剪掉子节点，避免重复计入）；否则遇到字符串字面量就收。
		if be, ok := n.(*ast.BinaryExpr); ok && be.Op == token.ADD {
			walk(be, map[string]bool{})
			sb.WriteString("\n")
			return false
		}
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			walk(lit, map[string]bool{})
			sb.WriteString("\n")
			return false
		}
		return true
	})
	return orderByBlob{text: sb.String(), spans: spans}
}

var (
	orderByReKeyword = regexp.MustCompile(`(?i)\border\s+by\b`)
	orderByReTail    = regexp.MustCompile(`(?i)\s+(asc|desc)\s*$`)
	orderByStops     = []string{"limit", "offset", "union", "except", "intersect", "returning", "window", "from", "where", "join", "select"}
	orderByStmtStart = []string{"select", "insert", "update", "delete", "replace", "with", "pragma"}
)

// orderByClausesInBlob 在 blob 里找出全部 ORDER BY 子句。
func orderByClausesInBlob(file, fnName string, blob orderByBlob) []orderByClause {
	depth := orderBySpanDepths(blob)
	var out []orderByClause
	for _, loc := range orderByReKeyword.FindAllStringIndex(blob.text, -1) {
		limit := orderBySpanEnd(blob, loc[0])
		end := orderByClauseEnd(blob.text, depth, loc[1], limit)
		raw := strings.TrimSpace(blob.text[loc[1]:end])
		keys := orderBySplitTop(raw)
		for i := range keys {
			keys[i] = orderByCleanKey(keys[i])
		}
		// 语句窗口：往前找最近一个括号深度 0 的语句关键字（拼出来的 SQL 才有 FROM 可看）。
		winStart := 0
		for i := loc[0] - 1; i >= 0; i-- {
			if depth[i] != 0 {
				continue
			}
			hit := false
			for _, w := range orderByStmtStart {
				if orderByWordAt(blob.text, i, w) {
					winStart, hit = i, true
					break
				}
			}
			if hit {
				break
			}
		}
		line, spanFile := orderByLocate(blob, loc[0])
		if spanFile != "" {
			file = filepath.Base(spanFile)
		}
		out = append(out, orderByClause{
			File:    file,
			Line:    line,
			Func:    fnName,
			Clause:  orderByNorm("order by " + raw),
			Keys:    keys,
			LastKey: keys[len(keys)-1],
			Window:  orderByNorm(blob.text[winStart:loc[0]]),
		})
	}
	return out
}

// orderBySpanDepths 逐字面量算括号深度（每个字面量从 0 起算）。
//
// 为什么要按字面量重置：字面量是独立片段，拼起来后一个未闭合的 `(` 会把后面所有
// 片段都算成「子查询里」，顶层表就被漏掉（表解析不到 ⇒ 判据失效）。
func orderBySpanDepths(blob orderByBlob) []int {
	d := make([]int, len(blob.text)+1)
	for _, sp := range blob.spans {
		cur := 0
		inStr := byte(0)
		for i := sp.start; i < sp.end && i < len(blob.text); i++ {
			c := blob.text[i]
			d[i] = cur
			if inStr != 0 {
				if c == inStr {
					inStr = 0
				}
				continue
			}
			switch c {
			case '\'', '"', '`':
				inStr = c
			case '(':
				cur++
			case ')':
				if cur > 0 {
					cur--
				}
			}
		}
	}
	return d
}

// orderByClauseEnd 找子句终点：顶层关键字（LIMIT/UNION/...）、结束括号、或字面量结束。
func orderByClauseEnd(text string, depth []int, start, limit int) int {
	base := depth[start]
	i := start
	inStr := byte(0)
	for i < limit {
		c := text[i]
		if inStr != 0 {
			if c == inStr {
				inStr = 0
			}
			i++
			continue
		}
		switch c {
		case '\'', '"', '`':
			inStr = c
			i++
			continue
		case ')':
			if depth[i] < base {
				return i
			}
		}
		if depth[i] == base {
			for _, w := range orderByStops {
				if orderByWordAt(text, i, w) {
					return i
				}
			}
		}
		i++
	}
	return i
}

// orderBySpanEnd 返回包含 off 的字面量结束偏移。
func orderBySpanEnd(blob orderByBlob, off int) int {
	for _, sp := range blob.spans {
		if sp.start <= off && off <= sp.end {
			return sp.end
		}
	}
	return len(blob.text)
}

// orderByLocate 把 blob 偏移映射回（文件行号, 文件名）。
func orderByLocate(blob orderByBlob, off int) (int, string) {
	line, file := 0, ""
	for _, sp := range blob.spans {
		if sp.start > off {
			break
		}
		line = sp.line
		file = sp.file
		if off <= sp.end {
			line = sp.line + strings.Count(blob.text[sp.start:off], "\n")
			break
		}
	}
	return line, file
}

// orderByTopTableRefs 取语句窗口里**括号深度 0**的表引用（子查询里的表不算）。
func orderByTopTableRefs(c orderByClause) []orderByTableRef {
	// 窗口是归一化文本，深度按归一化文本重算即可（子查询括号在归一化后仍在）。
	depth := orderByPlainDepths(c.Window)
	var out []orderByTableRef
	for _, m := range orderByReTableRef.FindAllStringSubmatchIndex(c.Window, -1) {
		if depth[m[0]] != 0 {
			continue
		}
		ref := orderByTableRef{Table: strings.ToLower(c.Window[m[4]:m[5]])}
		rest := strings.TrimLeft(c.Window[m[1]:], " ")
		if len(rest) > 2 && strings.EqualFold(rest[:2], "as") {
			rest = strings.TrimLeft(rest[2:], " ")
		}
		if len(rest) > 0 && orderByIsIdentByte(rest[0]) && !orderByIsDigit(rest[0]) {
			j := 0
			for j < len(rest) && orderByIsIdentByte(rest[j]) {
				j++
			}
			if cand := strings.ToLower(rest[:j]); !orderBySQLKeywords[cand] {
				ref.Alias = cand
			}
		}
		out = append(out, ref)
	}
	return out
}

var orderByReTableRef = regexp.MustCompile(`(?i)\b(from|join|update|into)\s+([a-z_][a-z0-9_]*)`)

var orderBySQLKeywords = map[string]bool{
	"where": true, "join": true, "left": true, "right": true, "inner": true, "outer": true,
	"on": true, "group": true, "order": true, "limit": true, "offset": true, "set": true,
	"using": true, "union": true, "values": true, "select": true, "and": true, "or": true,
	"natural": true, "cross": true, "full": true, "returning": true, "as": true,
}

// orderByPlainDepths 对一段普通文本算括号深度（忽略引号内的括号）。
func orderByPlainDepths(s string) []int {
	d := make([]int, len(s)+1)
	cur := 0
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		d[i] = cur
		c := s[i]
		if inStr != 0 {
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inStr = c
		case '(':
			cur++
		case ')':
			if cur > 0 {
				cur--
			}
		}
	}
	d[len(s)] = cur
	return d
}

func orderByWordAt(text string, i int, word string) bool {
	if i+len(word) > len(text) || !strings.EqualFold(text[i:i+len(word)], word) {
		return false
	}
	if i > 0 && orderByIsIdentByte(text[i-1]) {
		return false
	}
	if i+len(word) < len(text) && orderByIsIdentByte(text[i+len(word)]) {
		return false
	}
	return true
}

func orderByIsIdentByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func orderByIsDigit(c byte) bool { return c >= '0' && c <= '9' }

// orderBySplitTop 按顶层逗号切分（忽略括号/引号内的逗号）。
func orderBySplitTop(s string) []string {
	var out []string
	depth := 0
	inStr := byte(0)
	last := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inStr = c
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	return append(out, s[last:])
}

// orderByCleanKey 去掉排序键尾巴上的 ASC/DESC 与空白。
func orderByCleanKey(key string) string {
	key = strings.TrimSpace(key)
	for {
		trimmed := strings.TrimSpace(orderByReTail.ReplaceAllString(key, ""))
		if trimmed == key {
			return key
		}
		key = trimmed
	}
}

// orderByStripQualifier 去掉表限定符（`u.created_at` → `created_at`），小写化。
func orderByStripQualifier(key string) string {
	key = strings.TrimSpace(key)
	if i := strings.LastIndex(key, "."); i >= 0 {
		key = key[i+1:]
	}
	return orderByUnquoteIdent(key)
}

// orderByQualifier 取表限定符（`d.node_id` → `d`），没有则空串。
func orderByQualifier(key string) string {
	key = strings.TrimSpace(key)
	if i := strings.LastIndex(key, "."); i >= 0 {
		return strings.ToLower(strings.TrimSpace(key[:i]))
	}
	return ""
}

// orderByUnquoteIdent 去掉标识符引号并小写化。
func orderByUnquoteIdent(s string) string {
	s = strings.TrimSpace(s)
	for _, pair := range [][2]string{{`"`, `"`}, {"`", "`"}, {"[", "]"}} {
		if strings.HasPrefix(s, pair[0]) && strings.HasSuffix(s, pair[1]) && len(s) >= 2 {
			s = s[1 : len(s)-1]
		}
	}
	return strings.ToLower(s)
}

// orderByNorm 归一化：小写 + 空白折叠。
func orderByNorm(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// ============================================================================
// schema.sql 解析
// ============================================================================

// orderByParseSchema 解析 schema.sql，产出每张表的主键 / 唯一键 / rowid 可用性。
//
// 为什么直接解析 SQL 文本而不是查 pragma：判据要在**静态**层面成立（不建库也能查
// 一条新写的 ORDER BY 是否安全），而且 schema.sql 是这张库的单一事实源。
// 解析器本身由「schema 解析与真实库一致」子测试兜住。
func orderByParseSchema(t *testing.T, ddl string) map[string]*orderByTableShape {
	t.Helper()
	if strings.TrimSpace(ddl) == "" {
		t.Fatalf("schema.sql 为空：嵌入的 schemaSQL 没读到（判定依据缺失）")
	}
	body := orderByStripLineComments(ddl)
	tables := map[string]*orderByTableShape{}
	for _, stmt := range orderBySplitStatements(body) {
		s := strings.TrimSpace(stmt)
		if s == "" {
			continue
		}
		if m := orderByReCreateTable.FindStringSubmatch(s); m != nil {
			name := orderByUnquoteIdent(m[1])
			shape := &orderByTableShape{Name: name, HasRowID: strings.TrimSpace(m[3]) == ""}
			for _, def := range orderBySplitTop(m[2]) {
				d := strings.TrimSpace(def)
				low := strings.ToLower(d)
				switch {
				case strings.HasPrefix(low, "primary key"):
					cols := orderByParenCols(d)
					shape.PK = cols
					shape.Uniques = append(shape.Uniques, orderByUniqueKey{Cols: cols, Source: "PRIMARY KEY"})
				case strings.HasPrefix(low, "unique"):
					shape.Uniques = append(shape.Uniques, orderByUniqueKey{
						Cols: orderByParenCols(d), Source: "UNIQUE 约束"})
				case strings.HasPrefix(low, "constraint"), strings.HasPrefix(low, "foreign key"),
					strings.HasPrefix(low, "check"):
					// 与唯一性无关
				default:
					// 列级：`x TEXT PRIMARY KEY` / `endpoint TEXT NOT NULL UNIQUE`
					if m2 := orderByReColPK.FindStringSubmatch(d); m2 != nil {
						col := orderByUnquoteIdent(m2[1])
						shape.PK = []string{col}
						shape.Uniques = append(shape.Uniques, orderByUniqueKey{Cols: []string{col}, Source: "PRIMARY KEY（列内）"})
					}
					if m2 := orderByReColUnique.FindStringSubmatch(d); m2 != nil {
						col := orderByUnquoteIdent(m2[1])
						if !orderByIsReservedWord(col) {
							shape.Uniques = append(shape.Uniques, orderByUniqueKey{
								Cols: []string{col}, Source: "UNIQUE（列内）"})
						}
					}
				}
			}
			tables[name] = shape
			continue
		}
		if m := orderByReCreateIndex.FindStringSubmatch(s); m != nil {
			if strings.TrimSpace(m[1]) == "" { // 普通索引不影响唯一性
				continue
			}
			shape, ok := tables[orderByUnquoteIdent(m[3])]
			if !ok {
				continue
			}
			shape.Uniques = append(shape.Uniques, orderByUniqueKey{
				Cols:    orderByColumnList(m[4]),
				Partial: orderByNorm(m[5]),
				Source:  "UNIQUE INDEX " + orderByUnquoteIdent(m[2]),
			})
		}
	}
	return tables
}

var (
	orderByReCreateTable = regexp.MustCompile(`(?is)^create\s+table\s+(?:if\s+not\s+exists\s+)?([a-z_][\w]*)\s*\((.*)\)\s*(without\s+rowid)?\s*$`)
	orderByReCreateIndex = regexp.MustCompile(`(?is)^create\s+(unique\s+)?index\s+(?:if\s+not\s+exists\s+)?([a-z_][\w]*)\s+on\s+([a-z_][\w]*)\s*\(([^)]*)\)\s*(?:where\s+(.*))?$`)
	orderByReColPK       = regexp.MustCompile(`(?is)^([a-z_][\w]*)\s+.*\bprimary\s+key\b`)
	orderByReColUnique   = regexp.MustCompile(`(?is)^([a-z_][\w]*)\s+.*\bunique\b`)
)

// orderByIsReservedWord 拦住把表级约束（`UNIQUE (a, b)`）误当列级 UNIQUE 的形态。
func orderByIsReservedWord(word string) bool {
	switch word {
	case "unique", "primary", "key", "constraint", "foreign", "check":
		return true
	}
	return false
}

// orderByParenCols 取 `PRIMARY KEY (a, b)` / `UNIQUE (a, b)` 括号里的列。
func orderByParenCols(def string) []string {
	open := strings.Index(def, "(")
	close := strings.LastIndex(def, ")")
	if open < 0 || close <= open {
		return nil
	}
	return orderByColumnList(def[open+1 : close])
}

// orderByColumnList 把列清单切成小写列名（吃掉 ASC/COLLATE 之类的尾巴）。
func orderByColumnList(list string) []string {
	var out []string
	for _, part := range orderBySplitTop(list) {
		part = orderByStripQualifier(orderByCleanKey(part))
		if i := strings.Index(part, " collate"); i >= 0 {
			part = part[:i]
		}
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// orderByStripLineComments 去掉 SQL 的 `--` 行注释（引号内的 `--` 不动）。
func orderByStripLineComments(s string) string {
	var sb strings.Builder
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			sb.WriteByte(c)
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inStr = c
			sb.WriteByte(c)
		case '-':
			if i+1 < len(s) && s[i+1] == '-' {
				for i < len(s) && s[i] != '\n' {
					i++
				}
				sb.WriteByte('\n')
				continue
			}
			sb.WriteByte(c)
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// orderBySplitStatements 按顶层 `;` 切语句（忽略括号与引号内的分号）。
func orderBySplitStatements(s string) []string {
	var out []string
	depth := 0
	inStr := byte(0)
	last := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inStr = c
		case '(':
			depth++
		case ')':
			depth--
		case ';':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	return append(out, s[last:])
}

// ============================================================================
// 与真实库对账（schema 解析器的守卫）
// ============================================================================

// orderByDBUniqueSets 返回真实库里某张表的唯一键集合（主键 + UNIQUE 约束 + UNIQUE 索引）。
func orderByDBUniqueSets(t *testing.T, db *DB, table string) []string {
	t.Helper()
	sets := map[string]bool{}
	// 主键：pragma_table_info 的 pk 序号（INTEGER PRIMARY KEY 是 rowid 别名，索引列表里
	// 可能没有它，所以主键要单独取）。
	if pk := primaryKeyColumns(t, db, table); len(pk) > 0 {
		sets[strings.Join(pk, ",")] = true
	}
	rows, err := db.Query(`SELECT name FROM pragma_index_list(?) WHERE "unique" = 1`, table)
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
		cols, unique, _, ok := indexShape(t, db, name)
		if !ok || !unique {
			continue
		}
		sets[strings.Join(cols, ",")] = true
	}
	out := make([]string, 0, len(sets))
	for s := range sets {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// orderByDBWithoutRowID 报告真实库里某张表是不是 WITHOUT ROWID。
func orderByDBWithoutRowID(t *testing.T, db *DB, table string) bool {
	t.Helper()
	var ddl string
	if err := db.QueryRow(`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'table' AND name = ?`,
		table).Scan(&ddl); err != nil {
		t.Fatalf("read DDL of %s: %v", table, err)
	}
	return strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID")
}

// ============================================================================
// 双侧自证：变异必须变红（证「这套判据真的在承重」）
// ============================================================================

// orderBySelfTest 用合成输入跑同一份判据，红/绿两侧都要被证明。
func orderBySelfTest(t *testing.T) {
	t.Helper()
	schema := `
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS capability_versions (
    id TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL,
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    deleted_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_cap_live ON capability_versions(namespace_id, name, version) WHERE deleted_at IS NULL;
CREATE TABLE IF NOT EXISTS kv (
    k TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL
) WITHOUT ROWID;
`
	tables := orderByParseSchema(t, schema)

	// 把源码片段变成子句集合（走的是真实扫描路径，不是手搓结构体）。
	mutate := func(body string) []orderByClause {
		src := "package store\n\nfunc mutant() {\n\t_ = `" + body + "`\n}\n"
		return orderByScanSources(t, map[string]string{"mutant.go": src})
	}

	t.Run("只按时间列排序（1461f7e4 的形态）必须红", func(t *testing.T) {
		clauses := mutate("SELECT id FROM users ORDER BY created_at")
		if len(clauses) != 1 {
			t.Fatalf("扫描器没抓到预期的 1 条子句，实际 %d 条", len(clauses))
		}
		got := orderByAudit(clauses, tables, nil, nil)
		if len(got.Problems) == 0 {
			t.Fatalf("ORDER BY created_at（末键不唯一）被判据放行了 —— 这正是 1461f7e4 的形态")
		}
		msg := strings.Join(got.Problems, "\n")
		for _, want := range []string{"users", "order by created_at", "created_at"} {
			if !strings.Contains(msg, want) {
				t.Errorf("失败信息里缺 %q（要求：文件:行 + 表 + 子句原文 + 为什么 + 修法）：\n%s", want, msg)
			}
		}
	})

	t.Run("rowid / 主键破平局必须绿", func(t *testing.T) {
		for _, body := range []string{
			"SELECT id FROM users ORDER BY created_at, rowid",
			"SELECT id FROM users ORDER BY created_at, id",
			"SELECT k FROM kv ORDER BY created_at, k",
		} {
			clauses := mutate(body)
			got := orderByAudit(clauses, tables, nil, nil)
			if len(got.Problems) > 0 {
				t.Errorf("%q 被判成违规（应绿）：%v", body, got.Problems)
			}
			if got.Auto != 1 {
				t.Errorf("%q 没有走「末键自身唯一」自动路径（Auto=%d）", body, got.Auto)
			}
		}
	})

	t.Run("WITHOUT ROWID 表上 rowid 破平局必须红", func(t *testing.T) {
		clauses := mutate("SELECT k FROM kv ORDER BY created_at, rowid")
		got := orderByAudit(clauses, tables, nil, nil)
		if len(got.Problems) == 0 {
			t.Fatalf("kv 是 WITHOUT ROWID，`ORDER BY ..., rowid` 不可能给出全序（rowid 列根本不存在），却被放行")
		}
	})

	t.Run("部分唯一索引必须要求谓词在 WHERE 里", func(t *testing.T) {
		entry := orderByEntry{
			File: "mutant.go", Func: "mutant", Table: "capability_versions",
			Clause: "order by name, version", Narrowed: []string{"namespace_id"},
			Unique: []string{"namespace_id", "name", "version"}, Why: "合成用例",
		}
		// 有 deleted_at IS NULL：命中部分唯一索引 ⇒ 绿。
		withPred := mutate("SELECT id FROM capability_versions WHERE namespace_id = ? AND deleted_at IS NULL ORDER BY name, version")
		if got := orderByAudit(withPred, tables, []orderByEntry{entry}, nil); len(got.Problems) > 0 {
			t.Errorf("带谓词的证据被拒（应绿）：%v", got.Problems)
		}
		// 去掉谓词：行集里可能有软删重复行 ⇒ 必须红。
		noPred := mutate("SELECT id FROM capability_versions WHERE namespace_id = ? ORDER BY name, version")
		if got := orderByAudit(noPred, tables, []orderByEntry{entry}, nil); len(got.Problems) == 0 {
			t.Errorf("缺 deleted_at IS NULL 时部分唯一索引不成立，却被放行")
		}
	})

	t.Run("证据口说无凭必须红", func(t *testing.T) {
		clauses := mutate("SELECT id FROM users ORDER BY created_at")
		cases := []struct {
			name  string
			entry orderByEntry
		}{
			{"没有证据", orderByEntry{File: "mutant.go", Func: "mutant", Table: "users", Clause: "order by created_at", Why: "我觉得安全"}},
			{"声明的唯一键不存在", orderByEntry{File: "mutant.go", Func: "mutant", Table: "users", Clause: "order by created_at", Why: "x",
				Narrowed: []string{"id"}, Unique: []string{"id", "created_at"}}},
			{"声明的收窄列不在 WHERE 里", orderByEntry{File: "mutant.go", Func: "mutant", Table: "users", Clause: "order by created_at", Why: "x",
				Narrowed: []string{"username"}, Unique: []string{"username", "created_at"}}},
			{"GC 证据但语句不是 DELETE", orderByEntry{File: "mutant.go", Func: "mutant", Table: "users", Clause: "order by created_at", Why: "x", GCBatch: true}},
		}
		for _, tc := range cases {
			got := orderByAudit(clauses, tables, []orderByEntry{tc.entry}, nil)
			if len(got.Problems) == 0 {
				t.Errorf("白名单「%s」没写可核验证据，却被放行", tc.name)
			}
		}
	})

	t.Run("登记项过期必须红", func(t *testing.T) {
		clauses := mutate("SELECT id FROM users ORDER BY created_at")
		// 过期白名单（子句已不存在）
		stale := orderByEntry{File: "mutant.go", Func: "mutant", Clause: "order by created_at, rowid",
			Table: "users", Why: "过期", Unique: []string{"id"}, Narrowed: []string{"created_at"}}
		got := orderByAudit(clauses, tables, []orderByEntry{stale}, nil)
		if len(got.Problems) == 0 || !strings.Contains(strings.Join(got.Problems, "\n"), "过期") {
			t.Errorf("不命中的白名单没有报过期：%v", got.Problems)
		}
		// 过期缺口
		staleGap := orderByGap{File: "mutant.go", Func: "mutant", Clause: "order by x", Why: "w", Risk: "r", Fix: "f"}
		got = orderByAudit(clauses, tables, nil, []orderByGap{staleGap})
		if !strings.Contains(strings.Join(got.Problems, "\n"), "过期") {
			t.Errorf("不命中的已知缺口没有报过期：%v", got.Problems)
		}
	})

	t.Run("扫到 0 条必须红", func(t *testing.T) {
		got := orderByAudit(nil, tables, nil, nil)
		if len(got.Problems) == 0 {
			t.Fatalf("0 条子句被判据放行 —— 「绿」与「没检查」同形")
		}
	})

	t.Run("未知表必须红（新表不许静默通过）", func(t *testing.T) {
		clauses := mutate("SELECT id FROM mystery_table ORDER BY created_at, id")
		got := orderByAudit(clauses, tables, nil, nil)
		if !strings.Contains(strings.Join(got.Problems, "\n"), "mystery_table") {
			t.Fatalf("不在 schema.sql 也不在目录表白名单的表被静默放过：%v", got.Problems)
		}
	})
}
