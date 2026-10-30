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

// newTestNS 建组织（含 default 命名空间 + 内置能力）+ 一个 owner 用户，
// 返回 (默认命名空间 id, userID)。
//
// 为什么测试基本都用它而不是 newTestOrg：#162 之后命名空间资源的所有 store 调用
// 作用域参数都是 namespaceID（不是 orgID）。绝大多数用例只关心"这一套资源"，
// 拿默认命名空间就够了；真要断言组织级（成员/角色）时用 newTestOrg 拿 orgID。
func newTestNS(t *testing.T, db *DB) (namespaceID, userID string) {
	t.Helper()
	orgID, userID := newTestOrg(t, db)
	nsID, err := db.DefaultNamespaceID(context.Background(), orgID)
	if err != nil {
		t.Fatalf("default namespace for org %s: %v", orgID, err)
	}
	return nsID, userID
}

// newTestOrgNS 同时返回 orgID 与默认命名空间 id。
//
// 给那些**两层都需要**的用例：#162 之后资源写入用 nsID，而 orgID 还出现在
// 组织级字段（如 Source.OrgID）与跨命名空间断言里。
func newTestOrgNS(t *testing.T, db *DB) (orgID, namespaceID string) {
	t.Helper()
	orgID, _ = newTestOrg(t, db)
	nsID, err := db.DefaultNamespaceID(context.Background(), orgID)
	if err != nil {
		t.Fatalf("default namespace for org %s: %v", orgID, err)
	}
	return orgID, nsID
}

// orgIDOfNamespace 从命名空间回查组织（组织级断言用）。
func orgIDOfNamespace(t *testing.T, db *DB, namespaceID string) string {
	t.Helper()
	orgID, err := db.NamespaceOrgID(context.Background(), namespaceID)
	if err != nil {
		t.Fatalf("org of namespace %s: %v", namespaceID, err)
	}
	return orgID
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
