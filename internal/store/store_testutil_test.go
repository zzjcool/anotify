package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// newTestDBFile 打开一个**文件**后备的测试库并返回路径。
//
// 与 auth_test.go 里 :memory: 的 newTestDB 的区别：这里的库可以关掉再打开，
// 迁移测试（老库升级 / 失败回滚后重开）需要同一份数据。
func newTestDBFile(t *testing.T) (db *DB, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

// newTestOrg 建一个组织 + 一个 owner 用户，返回 (orgID, userID)。
// 所有新表都带 org_id，测试必须从这里拿，不能随手造字符串。
func newTestOrg(t *testing.T, db *DB) (orgID, userID string) {
	t.Helper()
	ctx := context.Background()
	u := &User{ID: NewUserID(), Username: "u-" + NewUserID(), DisplayName: "测试用户", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := db.CreateOrg(ctx, "org-"+NewUserID())
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := db.AddOrgMember(ctx, o.ID, u.ID, OrgRoleOwner); err != nil {
		t.Fatalf("add org member: %v", err)
	}
	return o.ID, u.ID
}

// tableExists 判断表是否存在（迁移断言用）。
func tableExists(t *testing.T, db *DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatalf("probe table %s: %v", name, err)
	}
	return n > 0
}

// columnNames 返回表的列名集合。
func columnNames(t *testing.T, db *DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("probe columns of %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out[name] = true
	}
	return out
}

// mustExec 执行 SQL，失败即 Fatal。
func mustExec(t *testing.T, db interface {
	Exec(string, ...any) (sql.Result, error)
}, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
