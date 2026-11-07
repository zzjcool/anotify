package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// withSpec 给测试夹具兜底：把行上的派生字段折叠成一份 `handler: noop` 正文。
//
// **只给测试用**。生产路径明令禁止从派生字段反推正文（契约 §4：正文是输入、派生列
// 是输出，反推会让「只改脚本不产生新版本」那个 bug 换个马甲回来）。但测试夹具的意图
// 恰恰是「给我一个 outputs=[a,b] 的能力」——在两百个用例里重复手写 spec 只会增加
// 写错的机会，也让用例真正想验的东西被噪音淹没。
//
// 已经带了实现的正文（handler 或 run）原样返回：显式写的 spec 优先，
// 于是「这条用例在验正文本身」的时候不会被夹具覆盖。
func withSpec(c *CapabilityVersion) *CapabilityVersion {
	if c == nil {
		return nil
	}
	if c.Spec.Handler != "" || c.Spec.Run.Entrypoint != "" || len(c.Spec.Run.Files) > 0 {
		return c
	}
	var lc json.RawMessage
	if c.Lifecycle != (Lifecycle{}) {
		b, err := json.Marshal(c.Lifecycle)
		if err != nil {
			panic(err) // Lifecycle 全是标量/指针，序列化不可能失败
		}
		lc = b
	}
	c.Spec = HandlerSpec(artifact.HandlerNoop, c.ParamsSchema, c.Outputs, lc)
	c.Spec.Sticky = c.Sticky
	return c
}

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

// newRawNamespace 直接插一个命名空间行（**不走注册、不播种内置能力**）。
//
// 「老库升级」类用例必须在**尚未迁到当前 schema** 的库上造数据：注册路径会顺带
// 播种内置能力，而播种要写新列（spec），老 schema 上根本没有那一列。用老库真实
// 的书写方式（裸 INSERT）造数据，才不会把「旧版本代码从没写过这一列」这个事实
// 掩盖成一次运行期报错。
func newRawNamespace(t *testing.T, db *DB) string {
	t.Helper()
	id := NewNamespaceID()
	if _, err := db.Exec(
		`INSERT INTO namespaces (id, slug, name, created_at) VALUES (?,?,?,?)`,
		id, "raw-"+id, "raw", Now()); err != nil {
		t.Fatalf("insert raw namespace: %v", err)
	}
	return id
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

// capRef 返回一个能力的**钉死引用**（`name@version`），当测试里 link 的一端用。
//
// P2.2 之后边里存的是**引用**，不是 `capability_versions.id`。所以测试也要写引用：
// `X.ID` 那种写法构造的是「P2.2 之前的形状」，它只能走输入兼容层
// （CreateEdge 会把 id 翻成 name@version），测的就不是路由语义了。
func capRef(c *CapabilityVersion) string { return FormatRef(c.Name, c.Version) }

// ---------- schema 形状 / 查询计划 helper（baseline 结构门禁与计划门禁用）----------

// indexColumnsOn 返回 table 上名为 index 的索引的列序（按 seqno 升序）；索引不存在即 Fatal。
//
// 按表名参数化（同一个索引名可能在不同表上都被断言），读不到唯一一条就 Fatal。
// 纯测试侧 helper，不改生产代码。
func indexColumnsOn(t *testing.T, db *DB, table, index string) []string {
	t.Helper()
	if n := countIndexRows(t, db, table, index); n != 1 {
		t.Fatalf("表 %s 上名为 %s 的索引有 %d 条, want 1", table, index, n)
	}
	rows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		t.Fatalf("read columns of index %s: %v", index, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column of index %s: %v", index, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns of index %s: %v", index, err)
	}
	return cols
}

// countIndexRows 数 sqlite_master 里「某表上某名字的索引」有几条（幂等断言用）。
func countIndexRows(t *testing.T, db *DB, table, index string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ? AND tbl_name = ?`,
		index, table).Scan(&n); err != nil {
		t.Fatalf("count index %s on %s: %v", index, table, err)
	}
	return n
}

// explainQueryPlan 跑 `EXPLAIN QUERY PLAN <query>`，把每行的 detail 列按行返回。
//
// 不写死列数：modernc 驱动目前返回 (id, parent, notused, detail)，但那属于驱动细节，
// 这里按 rows.Columns() 动态取值再拼成一行字符串，只在断言与日志里用。
func explainQueryPlan(t *testing.T, db *DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of explain result: %v", err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				parts[i] = string(b)
				continue
			}
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate explain rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN 没有任何输出")
	}
	return out
}
