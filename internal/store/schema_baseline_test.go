package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是 **squash 之后的 baseline 结构门禁**。
//
// squash 把 28 块增量迁移合并成了一份终态 DDL，升级路径（逐块 ALTER / 重建表 /
// 数据搬迁）整条删掉了。原来守护那些路径的用例（migrate14/15/16/18/26/27/28_test.go、
// migrate_engine_compat_test.go 等）测的是「已经不存在的代码」，但它们守护的性质
// 要靠**全新库的终态结构**来表达。
//
// 本文件实际守的是下面三条（注释只能声称真的跑过的断言，见 docs/lint-guards.md §3.1）：
//
//   · PRAGMA foreign_key_check / integrity_check 干净（TestBaselineIntegrity）
//   · 重开同一个库幂等：版本不重复记账、sqlite_master 形状一字不差
//     （TestBaselineReopenIsIdempotent）
//   · squash 之前建的库被明确拦截、且不动数据（TestLegacyDatabaseRejected）
//
// 曾经还在这里写着一份「表集合 / 索引形状 / 外键 / 唯一约束 / 主键」的期望清单
// （baselineTables / baselineIndexes，配 masterTables / indexShape / hasUniqueOn /
// primaryKeyColumns / foreignKeys 五件探针），但**从来没有任何用例调用过它们**：清单写了
// 却没人用，等于断言从没跑过（同 maskShapesSecrets 一族，是负债不是资产）。已在
// chore/u1000-cleanup 里按死代码删除；要拿回这项覆盖，必须重新接成真断言。
// 见 docs/lint-guards.md backlog #14。

// TestBaselineIntegrity 全新库必须自洽：外键检查干净、integrity_check = ok。
func TestBaselineIntegrity(t *testing.T) {
	db := newTestDB(t)

	var fkBad int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&fkBad); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if fkBad != 0 {
		t.Errorf("PRAGMA foreign_key_check 有 %d 条违规", fkBad)
	}
	var integ string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integ); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integ != "ok" {
		t.Errorf("PRAGMA integrity_check = %q, want ok", integ)
	}
}

// TestBaselineReopenIsIdempotent 重开同一个库：不重复执行、不报错、结构一字不差。
//
// 这是「版本记账」这条机制的门禁：baseline 只该应用一次，第二次 Open 必须整体跳过。
func TestBaselineReopenIsIdempotent(t *testing.T) {
	db, path := newTestDBFile(t)

	v1, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if v1 != headVersion {
		t.Fatalf("全新库版本 = %d，期望 headVersion %d（baseline + 追加块全部记账）", v1, headVersion)
	}
	before := masterDDL(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	v2, err := db2.SchemaVersion()
	if err != nil {
		t.Fatalf("schema version after reopen: %v", err)
	}
	if v2 != v1 {
		t.Fatalf("重开后版本 %d → %d，baseline 被重复执行了", v1, v2)
	}
	var rows int
	if err := db2.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&rows); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	// 每块迁移记一行（baseline + 追加块），重开**不重复**记账。
	if want := len(mustParse(t, schemaSQL)); rows != want {
		t.Fatalf("schema_version 有 %d 行，期望 %d 行（每块迁移只记账一次）", rows, want)
	}
	if got := masterDDL(t, db2); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("重开后结构变了：\n--- 首次 Open ---\n%s\n--- 重开 ---\n%s",
			strings.Join(before, "\n"), strings.Join(got, "\n"))
	}
}

// TestLegacyDatabaseRejected squash 之前建的库必须被**明确**拦截。
//
// 做法：手工造一个「v28 老库」——schema_version 里记着 1..28（squash 之前的最高版本），
// 外加一张带数据的老表。期望 Open 直接返回那句可操作的错误，而且**不动数据**
// （绝不能"静默删库重建"：那是用户的目录）。
func TestLegacyDatabaseRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v28.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	mustExec(t, raw, `CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`)
	for v := 1; v <= 28; v++ {
		mustExec(t, raw, `INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, v, 1700000000)
	}
	mustExec(t, raw, `CREATE TABLE legacy_marker (id TEXT PRIMARY KEY)`)
	mustExec(t, raw, `INSERT INTO legacy_marker (id) VALUES ('keep-me')`)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := Open(path)
	if err == nil {
		_ = db.Close()
		t.Fatal("squash 之前的老库被接受了：必须返回可操作的错误")
	}
	// 文案必须可操作：说清"是什么版本 / 不再支持增量升级 / 删数据目录 / 怎么重建数据"。
	for _, frag := range []string{
		"不是 v4 新建的库",
		"不支持旧库升级",
		"最高版本 28",
		"没有 v4 布局标记",
		"不会自动删除数据",
	} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("错误文案缺 %q：%v", frag, err)
		}
	}

	// 数据不许被静默删掉：老表还在、行还在、schema_version 一条不少。
	raw, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	var markers int
	if err := raw.QueryRow(`SELECT count(*) FROM legacy_marker WHERE id = 'keep-me'`).Scan(&markers); err != nil {
		t.Fatalf("老数据不见了（Open 把它删了？）: %v", err)
	}
	if markers != 1 {
		t.Fatalf("legacy_marker 行数 = %d，期望 1（拦截不许删数据）", markers)
	}
	var versions int
	if err := raw.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&versions); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if versions != 28 {
		t.Fatalf("schema_version 行数 = %d，期望 28（拦截不许改账）", versions)
	}
	// 也不许"先建一半再说"：新模型的表一张都不该出现。
	var fresh int
	if err := raw.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'tasks'`).Scan(&fresh); err != nil {
		t.Fatalf("probe tasks: %v", err)
	}
	if fresh != 0 {
		t.Error("拦截前已经把 baseline 建起来了：应该在执行任何迁移之前就返回错误")
	}
}

// ---------- 小工具 ----------

// masterDDL 把 sqlite_master 拉平成 `type|name|tbl_name|sql` 的排序切片（形状比对用）。
//
// 排除 sqlite_ 内部表：sqlite_stat1 / sqlite_stat4 / sqlite_sequence 是**运行期数据**，
// 两个库本来就可能不同（行数不同、AUTOINCREMENT 用没用过），把它们算进形状比对是错的。
func masterDDL(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT type, name, tbl_name, sql FROM sqlite_master
		  WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		  ORDER BY type DESC, name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, name, tbl, ddl string
		if err := rows.Scan(&typ, &name, &tbl, &ddl); err != nil {
			t.Fatalf("scan sqlite_master row: %v", err)
		}
		out = append(out, typ+"|"+name+"|"+tbl+"|"+strings.Join(strings.Fields(ddl), " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return out
}
