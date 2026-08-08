package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrateLegacyDB 验证从旧 schema（无 role/disabled 列）迁移到新 schema。
// 复现 dev.openaaas.org 升级场景：老库 users 表无 role/disabled 列，
// store.Open 必须能幂等迁移成功（ALTER 加列 + 建索引），且已有用户默认 member。
// 防回归：曾因 schema.sql 的 CREATE INDEX idx_users_role 在 ALTER 之前执行而失败。
func TestMigrateLegacyDB(t *testing.T) {
	// 1. 用旧 schema（无 role/disabled）建一个临时库
	tmp := t.TempDir()
	dbPath := tmp + "/legacy.db"
	legacyDB, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	// 旧 users 表（升级前的样子：无 role/disabled 列）
	if _, err := legacyDB.Exec(`CREATE TABLE users (
		id          TEXT PRIMARY KEY,
		username    TEXT NOT NULL UNIQUE,
		display_name TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy users: %v", err)
	}
	// 插入一个老用户（无 role）
	if _, err := legacyDB.Exec(`INSERT INTO users (id, username, display_name, created_at) VALUES (?,?,?,?)`,
		"usr_legacy", "olduser", "Old", 1000); err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	legacyDB.Close()

	// 2. 用 store.Open 打开（触发迁移）
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open legacy db 迁移失败: %v", err)
	}
	defer db.Close()

	// 3. 迁移后老用户应能读出，role 默认 member，disabled 默认 false
	u, err := db.GetUserByID("usr_legacy")
	if err != nil {
		t.Fatalf("get legacy user: %v", err)
	}
	if u.Username != "olduser" {
		t.Errorf("username: got %q want olduser", u.Username)
	}
	if u.Role != RoleMember {
		t.Errorf("迁移后老用户 role 应默认 member, got %q", u.Role)
	}
	if u.Disabled {
		t.Errorf("迁移后老用户 disabled 应默认 false")
	}

	// 4. role 列可写入（验证索引 idx_users_role 已建且列可更新）
	if n, err := db.UpdateUserRole(t.Context(), "usr_legacy", RoleAdmin); err != nil || n != 1 {
		t.Fatalf("update role: n=%d err=%v", n, err)
	}
	u2, _ := db.GetUserByID("usr_legacy")
	if u2.Role != RoleAdmin {
		t.Errorf("更新后 role 应 admin, got %q", u2.Role)
	}

	// 5. 新用户注册（验证新库路径也正常）
	u3 := &User{ID: NewUserID(), Username: "newuser", CreatedAt: Now()}
	if err := db.CreateUser(u3); err != nil {
		t.Fatalf("create new user: %v", err)
	}

	// 6. 再次 Open 幂等（重复迁移不报错）
	db.Close()
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("二次 Open（幂等迁移）失败: %v", err)
	}
	defer db2.Close()
}

// TestMigrateLegacyDB_UserCount 验证老库迁移后 UserCount 正常工作（首用户判定依赖它）。
func TestMigrateLegacyDB_UserCount(t *testing.T) {
	tmp := t.TempDir()
	dbPath := tmp + "/legacy2.db"
	legacyDB, _ := sql.Open("sqlite", "file:"+dbPath)
	legacyDB.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`)
	legacyDB.Exec(`INSERT INTO users (id, username, display_name, created_at) VALUES ('usr_a','a','',1)`)
	legacyDB.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	n, err := db.UserCount(t.Context())
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	if n != 1 {
		t.Errorf("UserCount 应 1（老库已有1用户）, got %d", n)
	}
}
