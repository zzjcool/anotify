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

// newTestNS 注册一个用户（= 建用户 + 它的默认命名空间 + 内置能力），
// 返回 (默认命名空间 id, userID)。
//
// 为什么测试基本都用它：#205 之后「组织」这一层不存在了，命名空间是唯一隔离维度，
// 而注册仍然保证「用户 + 默认命名空间」原子（代码里不存在没有命名空间的用户）。
// 绝大多数用例只关心"这一套资源"，拿默认命名空间就够了。
func newTestNS(t *testing.T, db *DB) (namespaceID, userID string) {
	t.Helper()
	u := &User{ID: NewUserID(), Username: "u-" + NewUserID(), DisplayName: "测试用户", CreatedAt: Now()}
	ns, err := db.RegisterUser(context.Background(), u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	return ns.ID, u.ID
}

// newTestNSNamed 注册一个用户，并额外建一个指定 slug 的命名空间，
// 返回 (该 slug 的命名空间 id, userID)。用于跨命名空间（数据隔离）用例。
func newTestNSNamed(t *testing.T, db *DB, slug string) (namespaceID, userID string) {
	t.Helper()
	_, userID = newTestNS(t, db)
	ns, err := db.CreateNamespace(context.Background(), slug, slug)
	if err != nil {
		t.Fatalf("create namespace %q: %v", slug, err)
	}
	return ns.ID, userID
}

// newTestExtraNS 在**同一个部署**里再建一个命名空间（不建用户），
// 用于断言"两个命名空间的数据互不影响"。
func newTestExtraNS(t *testing.T, db *DB, slug string) (namespaceID string) {
	t.Helper()
	ns, err := db.CreateNamespace(context.Background(), slug, slug)
	if err != nil {
		t.Fatalf("create extra namespace %q: %v", slug, err)
	}
	return ns.ID
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
