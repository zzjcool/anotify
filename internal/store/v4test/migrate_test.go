package v4test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 迁移机制的外部验收（v4：单块 baseline + 旧库拒绝）。
//
// 白盒部分（parseMigrations 的编号校验、注入坏迁移后的回滚）需要包内访问，
// 在 G1-6 内部测试迁移时补；这里用**公开面**验收用户真正会遇到的三件事：
// 新建库版本正确、重开幂等、旧库被明确拒绝且数据不被删。

// TestMigrationSchemaVersion 新建库只应用 baseline 一块，且版本号 = 1。
func TestMigrationSchemaVersion(t *testing.T) {
	db := openTestDB(t)
	v, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if v != 1 {
		t.Fatalf("v4 baseline 版本 = %d, want 1", v)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&n); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if n != 1 {
		t.Fatalf("schema_version 行数 = %d, want 1（baseline 不重复记账）", n)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 1 {
		t.Fatalf("记账版本 = %d, want 1", version)
	}
}

// TestMigrationIdempotent 重复 Open 同一库：不重复应用、不重复记账、结构不变。
func TestMigrationIdempotent(t *testing.T) {
	db, path := openTestDBWithPath(t)
	// 灌一点数据：有数据的库重开也必须整体跳过 baseline。
	newNS(t, db, "keep")
	before, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("version before: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	after, err := db2.SchemaVersion()
	if err != nil {
		t.Fatalf("version after: %v", err)
	}
	if before != after {
		t.Fatalf("schema version changed on reopen: %d → %d", before, after)
	}
	var n int
	if err := db2.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&n); err != nil {
		t.Fatalf("count after reopen: %v", err)
	}
	if n != 1 {
		t.Fatalf("重开后 schema_version 行数 = %d, want 1", n)
	}
	// 数据还在（重开不是重建）。
	var nsCount int
	if err := db2.QueryRow(`SELECT count(*) FROM namespaces WHERE slug = 'keep'`).Scan(&nsCount); err != nil {
		t.Fatalf("probe namespaces: %v", err)
	}
	if nsCount != 1 {
		t.Fatalf("重开后命名空间行数 = %d, want 1", nsCount)
	}
}

// TestLegacyDatabaseRejectedVersion 旧库（schema_version 记着 v3 的 11）被明确拒绝。
func TestLegacyDatabaseRejectedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-version.db")
	raw := rawSQLite(t, path)
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (11, 1)`); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	// 旧库里的业务表：拒绝必须发生在应用 baseline 之前，所以它不该被改。
	if _, err := raw.Exec(`CREATE TABLE legacy_keep (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	_, err := store.Open(path)
	if err == nil {
		t.Fatal("旧库应该被拒绝，实际 Open 成功")
	}
	msg := err.Error()
	if !strings.Contains(msg, "不支持旧库升级") || !strings.Contains(msg, "不会自动删除数据") {
		t.Fatalf("错误信息应说明「不支持旧库升级」且「不会自动删数据」: %v", err)
	}

	// 数据没被程序删掉：旧表仍在，且没有被建出 v4 的表。
	raw2 := rawSQLite(t, path)
	defer func() { _ = raw2.Close() }()
	var n int
	if err := raw2.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='legacy_keep'`).Scan(&n); err != nil {
		t.Fatalf("probe legacy table: %v", err)
	}
	if n != 1 {
		t.Fatal("旧库的业务表被删了（程序不许自动删数据）")
	}
	if err := raw2.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='capability_versions'`).Scan(&n); err != nil {
		t.Fatalf("probe capability_versions: %v", err)
	}
	if n != 0 {
		t.Fatal("拒绝之前已经把 baseline 应用了（顺序错）")
	}
}

// TestLegacyDatabaseRejectedTables 没有 schema_version、但已有业务表的旧库同样被拒绝。
func TestLegacyDatabaseRejectedTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-tables.db")
	raw := rawSQLite(t, path)
	if _, err := raw.Exec(`CREATE TABLE users (
		id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL DEFAULT 'member', disabled INTEGER NOT NULL DEFAULT 0,
		password_hash TEXT NOT NULL DEFAULT '', password_updated_at INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create users: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	_, err := store.Open(path)
	if err == nil {
		t.Fatal("旧库应该被拒绝，实际 Open 成功")
	}
	if !strings.Contains(err.Error(), "旧版业务表") || !strings.Contains(err.Error(), "users") {
		t.Fatalf("错误信息应点名旧库的业务表: %v", err)
	}

	// schema_version 是拒绝之后才该存在的东西：拒绝路径不能先建它。
	raw2 := rawSQLite(t, path)
	defer func() { _ = raw2.Close() }()
	var n int
	if err := raw2.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&n); err != nil {
		t.Fatalf("probe schema_version: %v", err)
	}
	if n != 0 {
		t.Fatal("拒绝旧库之前就先建了 schema_version（会抹掉判据）")
	}
}

// TestSchemaSQLStaticDiscipline 不变量 I1 的静态门禁（契约 A2）：
// schema.sql 全文（含注释）不得出现可执行命令类概念的那几个英文子串。
//
// 为什么在测试里再来一遍：Makefile 的 contract target 是最快的门，但它不在
// `go test` 的门禁里；schema.sql 是唯一事实源，这条纪律值得两道门。
func TestSchemaSQLStaticDiscipline(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	lower := strings.ToLower(string(src))
	for _, banned := range []string{"argv", "shell", "script"} {
		if strings.Contains(lower, banned) {
			t.Errorf("schema.sql 出现被禁子串 %q（不变量 I1）", banned)
		}
	}
	if !strings.Contains(string(src), "-- +migration 1 baseline") {
		t.Error("schema.sql 缺 `-- +migration 1 baseline` 分块头")
	}
}
