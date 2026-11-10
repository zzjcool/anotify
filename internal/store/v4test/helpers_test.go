// Package v4test 是 store 的 **v4 外部测试包**（package v4test）。
//
// 为什么在包外：v4 baseline 与能力领域是一次整体重建，而 `internal/store` 内部
// 的旧测试（v3 语义：端口 / 状态机 / handler / 图版本）要到 G1-6 才统一迁移。
// 在包内新增 v4 测试会与那些夹具抢同一个 package 命名空间、也会把「测试包能不能
// 编译」变成 G1-2 的阻塞项。放到外部测试包后：
//
//   - 只允许用**导出 API**（这本身就是一条约束：v4 的领域语义必须能通过公开
//     接口表达，不能靠读私有字段证明）；
//   - `go test ./internal/store/v4test/ -count=1` 独立可跑；
//   - G1-6 做内部测试迁移闭包时不会与这里冲突。
//
// 断言纪律：不 skip、不弱化。每条用例都在验一条 v4 契约（capability.md §2/§3、§8，
// playbook.md §0，config.md 的保留期形状），没有「先跑通再说」的空壳。
package v4test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

// openTestDB 打开一个**文件**后备的测试库（:memory: 会按连接分库，事务里读不到
// 别的连接刚写的行，不适合当迁移/事务测试的夹具）。
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "v4test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newNS 建一个命名空间（不走注册，避免依赖用户/密码形态）。
func newNS(t *testing.T, db *store.DB, slug string) string {
	t.Helper()
	ns, err := db.CreateNamespace(context.Background(), slug, slug)
	if err != nil {
		t.Fatalf("create namespace %q: %v", slug, err)
	}
	return ns.ID
}

// createCap 建能力，失败即 Fatal（CreateCapability 会回填 id/version/digest）。
func createCap(t *testing.T, db *store.DB, c *store.CapabilityVersion) *store.CapabilityVersion {
	t.Helper()
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability %s: %v", c.Name, err)
	}
	return c
}

// countCapRows 数某 (ns, name) 的能力行数。
func countCapRows(t *testing.T, db *store.DB, nsID, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM capability_versions WHERE namespace_id = ? AND name = ?`,
		nsID, name).Scan(&n); err != nil {
		t.Fatalf("count capability rows: %v", err)
	}
	return n
}

// tableExists 判断表是否存在。
func tableExists(t *testing.T, db *store.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatalf("probe table %s: %v", name, err)
	}
	return n > 0
}

// columnNames 返回表的列名集合。
func columnNames(t *testing.T, db *store.DB, table string) map[string]bool {
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
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns of %s: %v", table, err)
	}
	return out
}

// indexColumnsOn 返回某个索引的列（按序）；索引不存在时返回 nil。
func indexColumnsOn(t *testing.T, db *store.DB, table, index string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		t.Fatalf("index_info %s: %v", index, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index column: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index_info %s: %v", index, err)
	}
	_ = table
	return out
}

// rawSQLite 开一个**不跑迁移**的裸 SQLite 库（造旧库夹具用）。
func rawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	return db
}

// testCap 造一个能力行：params 是最小 object schema，正文是「一个能过校验的最小脚本」。
//
// v4 只有脚本一种实现形式，所以夹具必须给 run.files + entrypoint（#! 开头）。
// next 的第 4 参沿用「这个能力交给谁」的语义（v3 的 outputs 已不存在）。
func testCap(nsID, name string, version int, next []string) *store.CapabilityVersion {
	return &store.CapabilityVersion{
		NamespaceID: nsID,
		Name:        name,
		Version:     version,
		Spec: artifact.Spec{
			Params: []byte(`{"type":"object"}`),
			Next:   append([]string{}, next...),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}
}

// capSpec 造一份只给 params/next、正文用默认最小脚本的 spec。
func capSpec(params string, next []string) artifact.Spec {
	spec := testCap("", "x", 0, next).Spec
	if params != "" {
		spec.Params = []byte(params)
	}
	return spec
}

// formatRef 给错误消息拼 name@version。
func formatRef(name string, version int) string { return fmt.Sprintf("%s@%d", name, version) }

// openTestDBWithPath 打开测试库并返回路径（重开幂等类用例需要路径）。
func openTestDBWithPath(t *testing.T) (*store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v4test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}
