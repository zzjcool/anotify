package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseMigrations 校验 schema.sql 的分块序列：编号连续、无重复、名字非空。
//
// squash 之后只有一块（`1 baseline`）——这条同时守住「历史包袱不许借尸还魂」：
// 谁把老迁移块搬回来，首个断言就会红。
func TestParseMigrations(t *testing.T) {
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	if len(set) != 1 {
		t.Fatalf("squash 之后 schema.sql 应当只有 1 块（baseline），实际 %d 块", len(set))
	}
	if set[0].name != "baseline" {
		t.Fatalf("首块名字 = %q，期望 baseline", set[0].name)
	}
	seen := map[int]bool{}
	for _, m := range set {
		if seen[m.version] {
			t.Fatalf("duplicate migration version %d", m.version)
		}
		seen[m.version] = true
		if m.name == "" {
			t.Fatalf("migration %d has empty name", m.version)
		}
	}
	if set[0].version != 1 {
		t.Fatalf("first migration version must be 1, got %d", set[0].version)
	}
}

// TestParseMigrationsRejectsGap 断言跳号的迁移序列会被拒绝——
// 否则漏掉一个编号会导致「以为迁移过」。
func TestParseMigrationsRejectsGap(t *testing.T) {
	src := `
-- +migration 1 a
SELECT 1;
-- +migration 3 c
SELECT 3;
`
	if _, err := parseMigrations(src); err == nil {
		t.Fatal("expected error for gap in migration sequence, got nil")
	}
}

// TestSchemaVersionRecorded 断言迁移全部记账，且版本号 = schema.sql 里的最高编号。
func TestSchemaVersionRecorded(t *testing.T) {
	db := newTestDB(t)
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if want := set[len(set)-1].version; got != want {
		t.Fatalf("schema version = %d, want %d", got, want)
	}
	// 每条都已记账
	for _, m := range set {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM schema_version WHERE version=?`, m.version).Scan(&n); err != nil {
			t.Fatalf("count version %d: %v", m.version, err)
		}
		if n != 1 {
			t.Errorf("migration %d recorded %d times, want 1", m.version, n)
		}
	}
}

// TestMigrationIdempotent 重复 Open 同一库：不应重复应用，也不应报错。
func TestMigrationIdempotent(t *testing.T) {
	db, path := newTestDBFile(t)
	before, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("version before: %v", err)
	}
	// 先灌一点数据（注册会建用户 + 默认命名空间 + 内置能力），再重开：
	// 有数据的库重开也必须整体跳过 baseline（不重复执行、不重复记账）。
	newTestNS(t, db)

	// 关掉再打开
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	after, err := db2.SchemaVersion()
	if err != nil {
		t.Fatalf("version after: %v", err)
	}
	if before != after {
		t.Fatalf("schema version changed on reopen: %d → %d", before, after)
	}
	// 没有重复记账
	var dup int
	if err := db2.QueryRow(`SELECT count(*) - count(DISTINCT version) FROM schema_version`).Scan(&dup); err != nil {
		t.Fatalf("count duplicates: %v", err)
	}
	if dup != 0 {
		t.Fatalf("schema_version has %d duplicate rows", dup)
	}
}

// TestMigrationFailureRollsBack 注入一个写坏的迁移，断言：
//  1. Open 返回错误（不许吞错）
//  2. 该版本**未记账**
//  3. 该迁移里的建表**未生效**（事务回滚）
func TestMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	_, err := openWith(path, func(set *migrationSet) {
		// 追加一个必定失败的高编号迁移：前半段合法，后半段语法错误。
		*set = append(*set, migration{
			version: len(*set) + 1,
			name:    "broken",
			body: `CREATE TABLE half_done (id TEXT PRIMARY KEY);
			       CREATE TABLE ))) syntax error (((;`,
		})
	})
	if err == nil {
		t.Fatal("expected Open to fail on broken migration, got nil")
	}
	// 错误信息里要能看出是哪个迁移
	if !strings.Contains(err.Error(), "migration") {
		t.Fatalf("error should mention migration, got: %v", err)
	}

	// 用「不加坏迁移」的方式重开，检查上一个事务确实回滚了
	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after failure: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if tableExists(t, db, "half_done") {
		t.Fatal("half_done table exists: failed migration was NOT rolled back")
	}
	if got := mustMaxVersion(t, db); got != len(mustParse(t, schemaSQL)) {
		t.Fatalf("schema version = %d, want %d", got, len(mustParse(t, schemaSQL)))
	}
}

// TestMigrationHookFailureRollsBack hook 里失败同样要回滚且不记账。
func TestMigrationHookFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hookfail.db")
	errSentinel := errors.New("hook boom")
	_, err := openWith(path, func(set *migrationSet) {})
	if err != nil {
		t.Fatalf("baseline open: %v", err)
	}
	// 直接对已建好的库跑一次「带坏 hook 的迁移」
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	extra := migration{version: 999, name: "hookfail", body: `CREATE TABLE hook_half (id TEXT);`}
	if err := applyMigration(db.DB, extra, func(*sql.Tx) error { return errSentinel }); err == nil {
		t.Fatal("expected hook failure to propagate")
	}
	if tableExists(t, db, "hook_half") {
		t.Fatal("hook_half exists: hook failure was NOT rolled back")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_version WHERE version=999`).Scan(&n); err != nil {
		t.Fatalf("count 999: %v", err)
	}
	if n != 0 {
		t.Fatalf("failed migration recorded in schema_version (%d rows)", n)
	}
}

// TestMigrationNoSwallowedErrors 确认没有旧写法
// `_, _ = db.Exec("ALTER TABLE ...")` 那种吞错残留。
//
// 只检查真正的代码行（跳过纯注释：注释里提到旧写法是说明，不是实现）。
func TestMigrationNoSwallowedErrors(t *testing.T) {
	check := func(name, src string) {
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "_, _ = ") {
				t.Errorf("%s:%d swallowed error pattern: %s", name, i+1, trimmed)
			}
		}
	}
	check("schema.sql", schemaSQL)
	check("store.go", readSource(t, "store.go"))
	check("migrate.go", readSource(t, "migrate.go"))
}

// TestNoArgvOrKindInSchema 不变量 I1 / I3 的静态门禁（契约 A2 / A4）。
//
// I1：schema 里不得出现可执行概念（A2 的关键词）。
// I3：**推导量**不落库——capability / node / edge 都没有 kind 列（kind 由 outputs +
// 节点声明推导）。允许 kind 的只有两张表，且都不是推导量：sources.kind（cron /
// webhook / manual）是用户显式选择的**配置**，没有任何东西能推导出「这个源应该是
// 定时器还是 webhook」；cli_auth_sessions.kind（apikey / passkey）是**授权类型**，
// 与能力在链上的角色无关。
func TestNoArgvOrKindInSchema(t *testing.T) {
	lower := strings.ToLower(schemaSQL)
	for _, banned := range []string{"argv", "shell", "script"} {
		if strings.Contains(lower, banned) {
			t.Errorf("schema.sql contains banned token %q (invariant I1)", banned)
		}
	}

	kindAllowed := map[string]bool{"sources": true, "cli_auth_sessions": true}
	var curTable string
	for _, line := range strings.Split(schemaSQL, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue // 注释里的说明不算列定义
		}
		if strings.HasPrefix(strings.ToUpper(trimmed), "CREATE TABLE") {
			curTable = tableNameInCreate(trimmed)
			continue
		}
		if strings.HasPrefix(trimmed, ")") || strings.HasPrefix(trimmed, ";") {
			curTable = ""
			continue
		}
		if !strings.HasPrefix(trimmed, "kind ") || kindAllowed[curTable] {
			continue
		}
		t.Errorf("table %q declares banned column kind (invariant I3): %q", curTable, trimmed)
	}
}

// tableNameInCreate 从 `CREATE TABLE [IF NOT EXISTS] name (` 里取出表名
// （跳过 IF / NOT / EXISTS，并去掉 RENAME 重建留下的引号）。
func tableNameInCreate(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if !strings.EqualFold(f, "TABLE") {
			continue
		}
		j := i + 1
		for j < len(fields) && (strings.EqualFold(fields[j], "IF") ||
			strings.EqualFold(fields[j], "NOT") || strings.EqualFold(fields[j], "EXISTS")) {
			j++
		}
		if j >= len(fields) {
			return ""
		}
		return strings.Trim(strings.TrimSuffix(fields[j], "("), `"`)
	}
	return ""
}

// ---------- 小工具 ----------

func mustParse(t *testing.T, src string) migrationSet {
	t.Helper()
	set, err := parseMigrations(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return set
}

func mustMaxVersion(t *testing.T, db *DB) int {
	t.Helper()
	v, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	return v
}

// readSource 读取 store 包里的一个源文件（静态门禁用）。
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
