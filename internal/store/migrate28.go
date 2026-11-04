package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// 迁移 28：删掉 `tasks.out` 整列（P1.5「out 退役」的收尾）。
//
// 为什么删列而不是留着当 deprecated：`.anotify-out` / `result.out` 这条产出通道在
// P1.5 已经硬切掉，出口的唯一载体是 emission（`emissions.port`）。剩下这一列恒为
// NULL——它没有任何信息量，却仍然出现在 Task 结构体、API 响应和前端渲染里，于是
// 每个读它的人都会以为「路由结果还在任务行上」。schema 里没有它，这个误会才不可能
// 再发生。
//
// 两条路径（都不改数据、只去掉一列）：
//
//	① 快路：`ALTER TABLE tasks DROP COLUMN out`。现代引擎（SQLite ≥3.35）直接支持；
//	   `out` 不在任何索引/主键/CHECK/生成列里，所以正常情况下走这条。
//	② 兜底：万一引擎拒绝（老引擎、或列被视图/触发器引用），退化成
//	   「改名 → 建新表 → 拷贝 → drop 旧表 → 恢复索引」，与迁移 12/14 的表重建同形。
//
// 幂等：入口先查 `hasColumn(tasks, out)`，列不在就直接返回 nil（重跑 no-op）。
// 那一层外还有迁移框架：同一版本已经记账就不会再执行。
//
// 外键：`emissions.task_id REFERENCES tasks(id) ON DELETE CASCADE` **必须继续生效**
// （任务被 retention 清掉时它的 emissions 一起走）。两条路径都不动 emissions：
// ① 列删除不涉及别的表；② 兜底路径靠 `PRAGMA legacy_alter_table` 阻止 SQLite 把
// 其它表里的 `REFERENCES tasks` 改写成 `REFERENCES tasks_drop_out_old`
// （迁移 12 的注释里有这个坑的完整说明），所以 `emissions` 始终指向 `tasks`。
func migrateDropTaskOut(tx *sql.Tx) error {
	has, err := hasColumn(tx, "tasks", "out")
	if err != nil {
		return err
	}
	if !has {
		return nil // 已是「删过」的形状（新库 / 重跑）
	}

	dropped, err := tryDropColumnInTx(tx, "tasks", "out")
	if err != nil {
		return err
	}
	if dropped {
		return nil
	}
	// 引擎不肯 DROP COLUMN：走表重建。
	return rebuildTasksWithoutOut(tx)
}

// tryDropColumnInTx 在 savepoint 里试一次 `ALTER TABLE ... DROP COLUMN`。
//
// 为什么要 savepoint：失败时要把「这条语句可能造成的任何部分状态」撤干净再走
// 重建路径，否则两条路径叠加会得到第三种形状。SQLite 的 DDL 失败通常只回滚该语句，
// 但显式 savepoint 不依赖这个实现细节。
//
// 返回值 dropped=false 且 err==nil 表示「引擎拒绝」——这是兜底路径的触发条件，
// 不是错误。
func tryDropColumnInTx(tx *sql.Tx, table, column string) (dropped bool, err error) {
	const sp = "anotify_drop_column_probe"
	if _, err := tx.Exec("SAVEPOINT " + sp); err != nil {
		return false, fmt.Errorf("savepoint %s: %w", sp, err)
	}
	// 表名/列名是**本文件里的常量**（tasks / out），不是外部输入，不能也不必绑参——
	// DDL 里的标识符本来就不能用占位符。
	if _, execErr := tx.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, table, column)); execErr != nil {
		if _, rerr := tx.Exec("ROLLBACK TO " + sp); rerr != nil {
			return false, fmt.Errorf("rollback drop column %s.%s: %w (原错误: %v)", table, column, rerr, execErr)
		}
		if _, rerr := tx.Exec("RELEASE " + sp); rerr != nil {
			return false, fmt.Errorf("release drop column %s.%s: %w", table, column, rerr)
		}
		return false, nil
	}
	if _, err := tx.Exec("RELEASE " + sp); err != nil {
		return false, fmt.Errorf("release drop column %s.%s: %w", table, column, err)
	}
	return true, nil
}

// tasksWithoutOutDDL 是重建路径用的新表 DDL：列的**类型/NOT NULL/默认值/外键**
// 与 schema.sql 里 tasks 的建表语句逐字一致，只少了 `out`。
//
// 为什么手写而不是从 pragma 拼：pragma 给不出 REFERENCES / 默认值的原始写法，
// 拼出来的表会**静默丢掉外键**（tasks.namespace_id → namespaces、capability_id →
// capability_versions 的级联全没了）。宁可抄一遍，也要保住这些约束。
// 锁死「抄得对」的是一条测试：升级后的 tasks 列集合必须与全新库逐列一致
// （TestMigration28DropTaskOut）。
const tasksWithoutOutDDL = `CREATE TABLE tasks (
	id             TEXT PRIMARY KEY,
	namespace_id   TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
	trace_id       TEXT NOT NULL,
	parent_task_id TEXT,
	path           TEXT NOT NULL DEFAULT '[]',
	capability_id  TEXT NOT NULL REFERENCES capability_versions(id),
	capability     TEXT NOT NULL,
	version        INTEGER NOT NULL,
	params         TEXT NOT NULL DEFAULT '{}',
	status         TEXT NOT NULL DEFAULT 'pending',
	node_id        TEXT,
	instance_id    TEXT,
	lease_until    INTEGER,
	attempts       INTEGER NOT NULL DEFAULT 0,
	reported       INTEGER NOT NULL DEFAULT 0,
	summary        TEXT NOT NULL DEFAULT '',
	artifacts      TEXT NOT NULL DEFAULT '[]',
	error          TEXT NOT NULL DEFAULT '',
	cap_hash       TEXT NOT NULL,
	graph_id       TEXT NOT NULL,
	created_at     INTEGER NOT NULL,
	started_at     INTEGER,
	finished_at    INTEGER,
	not_before     INTEGER,
	digest         TEXT NOT NULL DEFAULT '',
	token          TEXT NOT NULL DEFAULT ''
)`

// tasksCopyColumns 是重建时拷贝的列（= 上表除 `out` 外的全部列，顺序与 DDL 一致）。
//
// 注意 digest / token 排在最后：它们是迁移 20 / 23 用 `ALTER TABLE ADD COLUMN`
// 补的，在 v27 库里就落在末尾。重建后保持同样的形状，「升级过的库」与「全新库」
// 的 `pragma_table_info` 才是逐列一致的（有用例钉住）。
var tasksCopyColumns = []string{
	"id", "namespace_id", "trace_id", "parent_task_id", "path", "capability_id", "capability",
	"version", "params", "status", "node_id", "instance_id", "lease_until", "attempts",
	"reported", "summary", "artifacts", "error", "cap_hash", "graph_id",
	"created_at", "started_at", "finished_at", "not_before", "digest", "token",
}

// rebuildTasksWithoutOut 是迁移 28 的兜底路径：重建 tasks 表去掉 `out`。
//
// 难点只有一个：**`emissions.task_id REFERENCES tasks(id) ON DELETE CASCADE`**。
// `foreign_keys=ON` 时 `DROP TABLE` 会先隐式 DELETE 全部行，那条 DELETE 会级联
// 清空 emissions——数据静默消失。而 SQLite ≥3.25 的 `ALTER TABLE ... RENAME` 又会
// 自动改写其它表里指向它的外键子句（emissions 会变成指向中间表）。
//
// 两个 PRAGMA 都救不了：`PRAGMA foreign_keys=OFF` 在事务里是 no-op（迁移框架要求
// 每条迁移在一个事务里），`legacy_alter_table` 对「谁引用了本表」这层改写并不生效
// （实测：RENAME 之后 emissions 的外键仍被改写成指向中间表，等于把外键永久弄丢）。
//
// 所以这里改成**按依赖顺序重建这一对表**，不依赖任何 pragma 的细节：
//
//	① 把 emissions 整表复制成一张无约束的副本（CREATE TABLE ... AS SELECT）
//	② DROP 掉 emissions ——它是 tasks 唯一的子表，删掉它之后就没有东西会被级联误删
//	③ 三步重建 tasks（改名 → 建新表 → 拷贝 → 删旧表），此时改名的副作用无处可写
//	④ 按 sqlite_master 里存的原 DDL 把 emissions 建回来 + 回灌数据
//	⑤ 按原 DDL 恢复 tasks / emissions 的索引
//
// 索引与 emissions 的 DDL 都是**从 sqlite_master 里读出来原样执行**的，不在这里
// 手抄一份：迁移 15/16/17 陆续给 tasks 加过索引，抄一份就会随下次加索引悄悄漏掉
// （那是最难发现的一类性能回归）。
//
// 外键检查用 `PRAGMA defer_foreign_keys=TRUE` 推迟到提交时刻：中途必然出现
// 「emissions 已删、tasks 还是旧的」的瞬间，提交时终态一致即可。
func rebuildTasksWithoutOut(tx *sql.Tx) error {
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = TRUE`); err != nil {
		return fmt.Errorf("drop task out: defer foreign keys: %w", err)
	}

	taskIndexes, err := tableIndexDDL(tx, "tasks")
	if err != nil {
		return err
	}
	emissionDDL, err := tableDDL(tx, "emissions")
	if err != nil {
		return err
	}
	emissionIndexes, err := tableIndexDDL(tx, "emissions")
	if err != nil {
		return err
	}
	emissionCols, err := tableColumns(tx, "emissions")
	if err != nil {
		return err
	}

	const (
		oldTasks    = "tasks_drop_out_old"
		emissionsKp = "emissions_drop_out_keep"
	)
	// 半途重试的残留（理论上不该有：整个迁移在一个事务里）：先清掉再说。
	for _, stale := range []string{oldTasks, emissionsKp} {
		if _, err := tx.Exec(`DROP TABLE IF EXISTS ` + stale); err != nil {
			return fmt.Errorf("drop task out: clear stale %s: %w", stale, err)
		}
	}

	// ①② emissions 先落地成无约束副本，再整表删掉。
	if e := emissionDDL; e == "" {
		// 表不存在（迁移 22 之后它必然存在；防御性分支）。
	} else {
		if _, err := tx.Exec(fmt.Sprintf(
			`CREATE TABLE %s AS SELECT * FROM emissions`, emissionsKp)); err != nil {
			return fmt.Errorf("drop task out: stash emissions: %w", err)
		}
		if _, err := tx.Exec(`DROP TABLE emissions`); err != nil {
			return fmt.Errorf("drop task out: drop emissions: %w", err)
		}
	}

	// ③ 三步重建 tasks。
	if _, err := tx.Exec(`ALTER TABLE tasks RENAME TO ` + oldTasks); err != nil {
		return fmt.Errorf("drop task out: rename old table: %w", err)
	}
	if _, err := tx.Exec(tasksWithoutOutDDL); err != nil {
		return fmt.Errorf("drop task out: create new table: %w", err)
	}
	cols := strings.Join(tasksCopyColumns, ", ")
	if _, err := tx.Exec(fmt.Sprintf(
		`INSERT INTO tasks (%s) SELECT %s FROM %s`, cols, cols, oldTasks)); err != nil {
		return fmt.Errorf("drop task out: copy rows: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE ` + oldTasks); err != nil {
		return fmt.Errorf("drop task out: drop old table: %w", err)
	}

	// ④ emissions 按原 DDL 建回来 + 回灌。
	if emissionDDL != "" {
		if _, err := tx.Exec(emissionDDL); err != nil {
			return fmt.Errorf("drop task out: recreate emissions: %w", err)
		}
		ecols := strings.Join(emissionCols, ", ")
		if _, err := tx.Exec(fmt.Sprintf(
			`INSERT INTO emissions (%s) SELECT %s FROM %s`, ecols, ecols, emissionsKp)); err != nil {
			return fmt.Errorf("drop task out: restore emissions: %w", err)
		}
		if _, err := tx.Exec(`DROP TABLE ` + emissionsKp); err != nil {
			return fmt.Errorf("drop task out: drop emissions stash: %w", err)
		}
	}

	// ⑤ 恢复索引（DROP TABLE 连带删掉了旧索引）。
	for _, ddl := range append(taskIndexes, emissionIndexes...) {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("drop task out: recreate index %q: %w", ddl, err)
		}
	}
	return nil
}

// tableDDL 读出某张表的原始建表语句（不存在时返回空串）。
func tableDDL(tx *sql.Tx, table string) (string, error) {
	var ddl sql.NullString
	err := tx.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&ddl)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("drop task out: read ddl of %s: %w", table, err)
	}
	return ddl.String, nil
}

// tableColumns 读出某张表的列名（按 cid 顺序；回灌数据时显式列出，不依赖 * 的顺序）。
func tableColumns(tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.Query(
		`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, fmt.Errorf("drop task out: read columns of %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("drop task out: scan column of %s: %w", table, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("drop task out: read columns of %s: %w", table, err)
	}
	return out, nil
}

// tableIndexDDL 读出某张表上**显式创建**的索引 DDL（自动索引 sql 为 NULL，跳过）。
func tableIndexDDL(tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.Query(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL ORDER BY name`,
		table)
	if err != nil {
		return nil, fmt.Errorf("drop task out: read indexes of %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			return nil, fmt.Errorf("drop task out: scan index of %s: %w", table, err)
		}
		out = append(out, ddl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("drop task out: read indexes of %s: %w", table, err)
	}
	return out, nil
}
