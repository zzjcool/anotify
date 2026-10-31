package store

// 迁移 15（#238）的验收测试：idx_edges_lookup 只能有**一个**权威形状。
//
// #238 的成因是静态的（schema.sql 里同一个索引名被声明两次，第二条引用一个
// 早已被删掉的列，于是被 IF NOT EXISTS 永久短路、从未生效），所以门禁也分两层：
//  1. 静态：schema.sql 里不许有同名索引的重复声明，可执行语句里不许再出现那个列；
//  2. 动态：历史库里「同名索引、列不同」的漂移要被迁移 15 拉正，且新库/老库形状一致。

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// schemaIndexDecl 匹配索引声明：CREATE [UNIQUE] INDEX [IF NOT EXISTS] <name>。
//
// 只用来收集**名字**（形状由动态用例负责）。加 (?i) 是因为 SQLite 关键字大小写不敏感，
// 库里现有语句混用了 `CREATE        INDEX` 这种对齐写法（中间多个空格）。
var schemaIndexDecl = regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)

// TestSchemaIndexDeclarationsUnique 静态门禁：同一个索引名在 schema.sql 里只能声明一次。
//
// 为什么必须有这一条：`CREATE INDEX IF NOT EXISTS` 只认名字不认列，**重复声明不会报错**，
// 只会让后一句静默失效（#238 就是这样潜伏了很久）。名字唯一 = 每句声明都真的执行。
func TestSchemaIndexDeclarationsUnique(t *testing.T) {
	// 去掉注释行再收集：注释里提到 CREATE INDEX 是说明文字，不会执行；
	// 把它算进来会让守卫对文档措辞敏感，最终只能靠改注释绕过。
	var executable strings.Builder
	for _, line := range strings.Split(schemaSQL, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		executable.WriteString(line)
		executable.WriteString("\n")
	}

	counts := map[string]int{}
	for _, m := range schemaIndexDecl.FindAllStringSubmatch(executable.String(), -1) {
		name := strings.ToLower(m[1])
		counts[name]++
		if counts[name] > 1 {
			t.Errorf("schema.sql 重复声明索引 %s（第 %d 次）：同名第二次会被 IF NOT EXISTS 静默跳过", name, counts[name])
		}
	}
	if counts[edgeLookupIndexName] != 1 {
		t.Errorf("%s 在 schema.sql 里被声明了 %d 次，want 1（#238 不能复发）",
			edgeLookupIndexName, counts[edgeLookupIndexName])
	}
	if len(counts) == 0 {
		t.Fatal("没有从 schema.sql 里收集到任何索引声明，正则可能失效了")
	}
}

// TestSchemaNoExecutableDroppedColumnReference 静态门禁：可执行语句里不许再引用已删除的列。
//
// from_capability 从迁移 9 起就不存在（edges 的左端改成了 from_is_source + from_key）。
// 注释里保留历史说明是允许的（那是文档），但任何会真的执行的语句引用它都是死句或错误。
func TestSchemaNoExecutableDroppedColumnReference(t *testing.T) {
	executablePrefixes := []string{"CREATE", "ALTER", "INSERT", "SELECT", "UPDATE", "DELETE"}
	for i, line := range strings.Split(schemaSQL, "\n") {
		trimmed := strings.TrimSpace(line)
		isExecutable := false
		upper := strings.ToUpper(trimmed)
		for _, p := range executablePrefixes {
			if strings.HasPrefix(upper, p) {
				isExecutable = true
				break
			}
		}
		if !isExecutable {
			continue
		}
		if strings.Contains(strings.ToLower(line), "from_capability") {
			t.Errorf("schema.sql:%d 可执行语句引用了已删除的列 from_capability（迁移 9 起不存在）: %s",
				i+1, trimmed)
		}
	}
}

// TestMigration15NormalizesEdgeLookupIndex 迁移 15 的三条硬要求：
// 新库形状正确、历史库漂移被拉正、hook 幂等。
func TestMigration15NormalizesEdgeLookupIndex(t *testing.T) {
	want := edgeLookupIndexColumns

	t.Run("新库形状正确", func(t *testing.T) {
		db, _ := newTestDBFile(t)
		if got := mustIndexColumns(t, db, edgeLookupIndexName); !sameColumnOrder(got, want) {
			t.Fatalf("%s 列序 = %v, want %v", edgeLookupIndexName, got, want)
		}
	})

	// driftedV14 造一个停在迁移 14、但索引形状被改坏的库，返回路径（库已关闭）。
	driftedV14 := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "drift.db")
		db14 := openAtVersion(t, path, 14)
		// 模拟历史漂移：同名索引、列不同（正是 IF NOT EXISTS 拦不住的那种）。
		mustExec(t, db14, `DROP INDEX `+edgeLookupIndexName)
		mustExec(t, db14, `CREATE INDEX idx_edges_lookup ON edges(namespace_id, from_port)`)
		if got := mustIndexColumns(t, db14, edgeLookupIndexName); sameColumnOrder(got, want) {
			t.Fatalf("漂移形状没造出来：列序仍是 %v", got)
		}
		if v := mustMaxVersion(t, db14); v != 14 {
			t.Fatalf("漂移库版本 = %d, want 14", v)
		}
		if err := db14.Close(); err != nil {
			t.Fatalf("close v14 db: %v", err)
		}
		return path
	}

	t.Run("历史库漂移被拉正", func(t *testing.T) {
		path := driftedV14(t)
		db, err := Open(path) // 升级 14 → 15
		if err != nil {
			t.Fatalf("升级漂移库: %v", err)
		}
		defer func() { _ = db.Close() }()

		if v := mustMaxVersion(t, db); v != 15 {
			t.Fatalf("升级后版本 = %d, want 15", v)
		}
		if got := mustIndexColumns(t, db, edgeLookupIndexName); !sameColumnOrder(got, want) {
			t.Fatalf("升级后 %s 列序 = %v, want %v（漂移没被拉正）", edgeLookupIndexName, got, want)
		}
	})

	t.Run("幂等", func(t *testing.T) {
		path := driftedV14(t)
		db, err := Open(path)
		if err != nil {
			t.Fatalf("升级漂移库: %v", err)
		}
		defer func() { _ = db.Close() }()

		// 同一事务里跑两次 hook：结果相同、不报错（迁移可能因重试被重复执行）。
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		for i := 1; i <= 2; i++ {
			if err := migrateEdgeLookupIndex(tx); err != nil {
				t.Fatalf("第 %d 次 migrateEdgeLookupIndex: %v", i, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}

		if got := mustIndexColumns(t, db, edgeLookupIndexName); !sameColumnOrder(got, want) {
			t.Fatalf("重复执行后 %s 列序 = %v, want %v", edgeLookupIndexName, got, want)
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM schema_version WHERE version = 15`).Scan(&n); err != nil {
			t.Fatalf("count version 15: %v", err)
		}
		if n != 1 {
			t.Fatalf("schema_version 里 15 记了 %d 次, want 1", n)
		}
	})
}

// TestMigration15ShapeParityFreshVsUpgraded 新库与「v14 截断库再升级」的形状必须逐行相等。
//
// 为什么单独一条：漂移索引不会让任何查询报错，只会让**两边形状不同**——
// 于是「测试跑新库、线上跑老库」时行为不一致。这条把 sqlite_master 的全部
// 表/索引（除 sqlite_ 内部对象）拉平成字符串逐条比对，是形状一致性的硬门禁。
func TestMigration15ShapeParityFreshVsUpgraded(t *testing.T) {
	fresh, _ := newTestDBFile(t)

	path := filepath.Join(t.TempDir(), "v14.db")
	db14 := openAtVersion(t, path, 14)
	if err := db14.Close(); err != nil {
		t.Fatalf("close v14 db: %v", err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("升级 v14 库: %v", err)
	}
	defer func() { _ = upgraded.Close() }()

	freshShape := schemaShape(t, fresh)
	upgradedShape := schemaShape(t, upgraded)

	if len(freshShape) != len(upgradedShape) {
		t.Fatalf("形状条数 新库 %d ≠ 升级库 %d", len(freshShape), len(upgradedShape))
	}
	diff := map[string]int{}
	for _, s := range upgradedShape {
		diff[s]++
	}
	for _, s := range freshShape {
		if diff[s] > 0 {
			diff[s]--
			continue
		}
		t.Errorf("升级库缺少形状（新库有）: %s", s)
	}
	for s, n := range diff {
		if n > 0 {
			t.Errorf("升级库多出形状（新库没有，%d 条）: %s", n, s)
		}
	}
}

// ---------- 小工具 ----------

// mustIndexColumns 读出一个索引的列序（索引必须存在）。
//
// indexColumns 收的是 *sql.Tx（迁移 hook 的签名），这里是测试侧的薄包装：
// 起一个事务读、读完回滚，不留下任何写入。
func mustIndexColumns(t *testing.T, db *DB, index string) []string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	cols, exists, err := indexColumns(tx, index)
	if err != nil {
		t.Fatalf("indexColumns(%s): %v", index, err)
	}
	if !exists {
		t.Fatalf("索引 %s 不存在", index)
	}
	return cols
}

// schemaShape 把库的结构拉平成 `type|name|tbl_name|ddl` 的排序切片
// （排除 sqlite_ 内部对象：sqlite_autoindex_* / sqlite_sequence 等由实现决定，不是 schema）。
func schemaShape(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, coalesce(sql, '')
	                         FROM sqlite_master
	                        WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list sqlite_master: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, name, tbl, ddl string
		if err := rows.Scan(&typ, &name, &tbl, &ddl); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s", typ, name, tbl, ddl))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("没有从 sqlite_master 读到任何对象")
	}
	sort.Strings(out)
	return out
}

// TestMigration15LegacyUpgradeKeepsIndex 真实老库升级路径：v11 时代 → 一路升到最新。
//
// 上面那条漂移用例是「手工造坏形状」；这条走的是**真实历史**：迁移 12 重建 edges
// （加 namespace_id）、迁移 14 摘掉 org_id，每一次重建都会重建同名索引。
// 要点是升级后索引仍然只有一个、且形状是权威形状——历史库不会停在中途的形状上。
func TestMigration15LegacyUpgradeKeepsIndex(t *testing.T) {
	path := legacyPre12DB(t, false)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("升级 v11 老库: %v", err)
	}
	defer func() { _ = db.Close() }()

	if v := mustMaxVersion(t, db); v != len(mustMigrations(t)) {
		t.Fatalf("升级后版本 = %d, want %d", v, len(mustMigrations(t)))
	}
	if got := mustIndexColumns(t, db, edgeLookupIndexName); !sameColumnOrder(got, edgeLookupIndexColumns) {
		t.Fatalf("v11 老库升级后 %s 列序 = %v, want %v", edgeLookupIndexName, got, edgeLookupIndexColumns)
	}
	// 同名索引只能有一个（名字在 edges 上唯一），且没有第二条死声明留下的残影。
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND tbl_name='edges' AND name=?`,
		edgeLookupIndexName).Scan(&n); err != nil {
		t.Fatalf("count index: %v", err)
	}
	if n != 1 {
		t.Fatalf("edges 上名为 %s 的索引有 %d 条, want 1", edgeLookupIndexName, n)
	}
	// 老库的业务行不能因为迁移 15 丢失（edges 一行不少）。
	if got := countRows(t, db, "edges", "1 = 1"); got != 1 {
		t.Fatalf("edges 行数 = %d, want 1（迁移 15 只动索引，不该动数据）", got)
	}
}

// mustMigrations 解析 schema.sql 得到迁移列表（测试侧取「最新版本号」用）。
func mustMigrations(t *testing.T) migrationSet {
	t.Helper()
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	return set
}
