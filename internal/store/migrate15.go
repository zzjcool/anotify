package store

import (
	"database/sql"
	"fmt"
)

// 迁移 15（#238）：把 idx_edges_lookup 的形状收口到唯一权威定义。
//
// 为什么需要这个迁移：`CREATE INDEX IF NOT EXISTS` 只认**名字**，不认列——
// 库里已经有一个叫 idx_edges_lookup 的索引时，后面那句"正确形状"的声明会被
// 静默跳过（这正是 #238 的成因：迁移 4 里同一索引名被声明了两次，第二条引用
// 一个早已不存在的列，被永久短路、从未生效）。
//
// 于是历史库可能出现「同名索引、列不同」的漂移：迁移 9 重建 edges、迁移 12 加
// namespace_id、迁移 14 摘掉 org_id，三次重建各自建过同名索引，任何一个停在
// 中途的历史库都可能带着非权威形状的索引。漂移索引对查询没有帮助，还会让
// 「新库」与「老库升上来的库」形状不一致（TestMigration15ShapeParity* 就是这条门禁）。
//
// 判据与动作：
//  1. 用 pragma_index_info 读出实际列序，与 edgeLookupIndexColumns 逐位比较；
//  2. **只有「索引存在且列序不一致」时才 DROP**，形状已正确时不动索引
//     （索引缺失时不必 DROP，下面的收口语句会建出来）；
//  3. 最后一律 CREATE INDEX IF NOT EXISTS 收口——形状正确时它是一句 no-op，
//     因此整个 hook 幂等（同一事务里跑两次结果相同，不会无脑重建索引）。
//
// 注意这里**不**做 `edges 表不存在就静默 return nil` 的守卫：#238 的教训就是
// 静默跳过，症状被藏起来。表不存在时让 CREATE INDEX 直接报错，错误一路向上抛。
const edgeLookupIndexName = "idx_edges_lookup"

// edgeLookupIndexColumns 是 idx_edges_lookup 唯一权威的列序。
//
// 与 edges 的主键前缀逐字对齐（namespace_id, from_is_source, from_key, from_port）。
// 需要说明的是：#238 的 EXPLAIN 证据显示派发查边实际走的是
// `COVERING INDEX sqlite_autoindex_edges_1`（PK 自动索引，前四列与 WHERE 对齐且覆盖
// to_capability），所以这条索引**本身是冗余的**——保留它是因为契约按「唯一权威形状」
// 冻结（删索引是另一条 issue 的事，删了会牵动迁移 12/14 的既有定义）。
// 本迁移只负责「同名索引不许带着别的列序存在」。
var edgeLookupIndexColumns = []string{"namespace_id", "from_is_source", "from_key", "from_port"}

// edgeLookupIndexDDL 是重建该索引时**唯一**使用的语句。
//
// 缩进刻意与迁移 14（`orgLayerRebuilds` 里 edges 的 indexes 项）逐字节一致：
// SQLite 会把 CREATE 语句原文存进 sqlite_master.sql（只把 IF NOT EXISTS 去掉），
// 所以只要这段文本不同，「漂移修复后的库」与「全新库」就会在 sqlite_master 上分叉——
// 语义一样、文本不一样，`TestMigration15ShapeParity*` 会把它判成形状不一致。
// 改这段文本前先看迁移 14 的同名字面量。
const edgeLookupIndexDDL = `CREATE INDEX IF NOT EXISTS idx_edges_lookup
			   ON edges(namespace_id, from_is_source, from_key, from_port)`

// migrateEdgeLookupIndex 是迁移 15 的 hook：把 idx_edges_lookup 拉回权威形状。
func migrateEdgeLookupIndex(tx *sql.Tx) error {
	cols, exists, err := indexColumns(tx, edgeLookupIndexName)
	if err != nil {
		return err
	}
	// 存在但列序不对 = 漂移，先删掉；不存在则直接建。
	// 形状已正确时**不**删：重建索引在大表上是没必要的代价，也破坏幂等的直观性。
	if exists && !sameColumnOrder(cols, edgeLookupIndexColumns) {
		if _, err := tx.Exec(`DROP INDEX ` + edgeLookupIndexName); err != nil {
			return fmt.Errorf("drop drifted index %s (%v): %w", edgeLookupIndexName, cols, err)
		}
	}
	// 收口语句：形状已正确时是 no-op；索引缺失或刚被删掉时建出权威形状。
	if _, err := tx.Exec(edgeLookupIndexDDL); err != nil {
		return fmt.Errorf("create index %s: %w", edgeLookupIndexName, err)
	}
	return nil
}

// indexColumns 返回某个索引的列名（按 seqno 顺序）以及它是否存在。
//
// 用表值函数 pragma_index_info(?) 而不是 `PRAGMA index_info(x)`：前者能绑参数，
// 索引名不需要拼进 SQL。seqno 就是索引里的列位置（0 起），必须显式排序——
// 不排序时行序没有保证，形状比较会变成 flaky 断言。
func indexColumns(tx *sql.Tx, index string) (cols []string, exists bool, err error) {
	var n int
	if err := tx.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ? AND tbl_name = 'edges'`, index,
	).Scan(&n); err != nil {
		return nil, false, fmt.Errorf("probe index %s: %w", index, err)
	}
	if n == 0 {
		return nil, false, nil
	}
	rows, err := tx.Query(
		`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		return nil, false, fmt.Errorf("read columns of index %s: %w", index, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, false, fmt.Errorf("scan column of index %s: %w", index, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate columns of index %s: %w", index, err)
	}
	return cols, true, nil
}

// sameColumnOrder 逐位比较两个列序（顺序敏感：索引前缀的顺序决定它能不能命中查询）。
func sameColumnOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
