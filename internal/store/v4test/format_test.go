package v4test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// v4 布局格式闭包的外部验收（G1-6C1）。
//
// 三条事实被分开钉住：
//   - **记账** schema_version：跑过哪些迁移块（v4 baseline = 1）；
//   - **布局格式** PRAGMA user_version：表结构是哪一代（v4 = 4）；
//   - 两者只有在一起、且结构真的完整时才是「v4 库」。
//
// 这里全部用真实 SQLite 文件造反例（旧库/坏库/半写库），并且每条拒绝都要验：
// 拒绝前后**用户数据、DDL、布局标记**一字不差——程序不许自愈、不许补标记、不许删数据。
// 唯一不比较的是 SQLite 自己的 journal/WAL 字节（父裁决明确排除）。

// TestFormatNewDatabaseWritesLayoutMarker 全新空库：baseline 记账 1 + 布局标记 4，
// 重开幂等，字段往返一致。
func TestFormatNewDatabaseWritesLayoutMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	// 记账仍是 1（历史迁移链没多、headVersion 没变）。
	if v, err := db.SchemaVersion(); err != nil || v != 1 {
		t.Fatalf("SchemaVersion = %d, %v; want 1", v, err)
	}
	// 布局标记是**独立**的另一件事。
	if got := rawUserVersion(t, path); got != 4 {
		t.Fatalf("新库 PRAGMA user_version = %d, want 4（v4 布局格式标记）", got)
	}
	nsID := newNS(t, db, "roundtrip")
	cap := createCap(t, db, testCap(nsID, "cap-roundtrip", 1, nil))
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen v4 db: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if got := rawUserVersion(t, path); got != 4 {
		t.Fatalf("重开后 PRAGMA user_version = %d, want 4", got)
	}
	if v, err := db2.SchemaVersion(); err != nil || v != 1 {
		t.Fatalf("重开后 SchemaVersion = %d, %v; want 1", v, err)
	}
	var name, digest, foldKey string
	if err := db2.QueryRow(
		`SELECT name, digest, fold_key FROM capability_versions WHERE id = ?`, cap.ID).
		Scan(&name, &digest, &foldKey); err != nil {
		t.Fatalf("read capability back: %v", err)
	}
	if name != "cap-roundtrip" || digest != cap.Digest || foldKey == "" {
		t.Fatalf("字段往返不一致: name=%q digest=%q(=%q) fold_key=%q",
			name, digest, cap.Digest, foldKey)
	}
}

// TestFormatBaselineLedgerWithoutLayoutMarkerRejected 是 G1-6C1 的核心反例：
// schema_version=1（看起来和 v4 baseline 一模一样）但**没有**布局标记的 v3 库。
//
// 旧实现就是这么放过去的：看见 schema_version 已有 1 就跳过 baseline，
// 然后在业务路径上炸 "no such column"。现在必须在任何写操作**之前**具名拒绝。
func TestFormatBaselineLedgerWithoutLayoutMarkerRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3-baseline1.db")
	raw := openRawFixture(t, path)
	mustExecSQL(t, raw,
		// v3 的记账：baseline 也是 1（这正是「记账 ≠ 布局身份」的来源）。
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version, applied_at) VALUES (1, 1)`,
		// v3 的 nodes：带 namespace_id（v4 的节点是集群级，没有这一列）。
		`CREATE TABLE nodes (
		    id TEXT PRIMARY KEY, name TEXT NOT NULL, namespace_id TEXT NOT NULL DEFAULT '',
		    outputs TEXT NOT NULL DEFAULT '[]')`,
		`INSERT INTO nodes (id, name, namespace_id) VALUES ('n1', 'node-1', 'ns-1')`,
		// v3 的 capability_versions：带 outputs / content_hash。
		`CREATE TABLE capability_versions (
		    id TEXT PRIMARY KEY, namespace_id TEXT NOT NULL, name TEXT NOT NULL,
		    version INTEGER NOT NULL, outputs TEXT NOT NULL DEFAULT '[]',
		    content_hash TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO capability_versions (id, namespace_id, name, version, outputs, content_hash)
		 VALUES ('c1', 'ns-1', 'cap', 1, '["x"]', 'h1')`,
	)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before := snapshotDatabase(t, path)

	_, err := store.Open(path)
	if err == nil {
		t.Fatal("schema_version=1 但没有 v4 布局标记的 v3 库应该被拒绝，实际 Open 成功")
	}
	if !errors.Is(err, store.ErrUnsupportedFormat) {
		t.Fatalf("应返回 ErrUnsupportedFormat（记账 ≠ 布局格式），实际: %v", err)
	}
	if errors.Is(err, store.ErrSchemaInvalid) {
		t.Fatalf("这不是「自称 v4 但结构坏」，不该报 ErrSchemaInvalid: %v", err)
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("错误信息应点名 schema_version 记账与布局标记的区别: %v", err)
	}
	t.Logf("v3·schema_version=1 但无布局标记的拒绝原因: %v", err)

	// 拒绝前后：数据/DDL/标记一字不差；而且没有补 marker、没有补 v4 表。
	after := snapshotDatabase(t, path)
	if after != before {
		t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
	}
	if got := rawUserVersion(t, path); got != 0 {
		t.Fatalf("拒绝时补写了布局标记（PRAGMA user_version=%d）：v3 库不许被标成 v4", got)
	}
	for _, v4Only := range []string{"task_attempts", "event_deliveries", "emits"} {
		if rawTableExists(t, path, v4Only) {
			t.Fatalf("拒绝之前就把 v4 表 %q 建出来了（顺序错）", v4Only)
		}
	}
	if got := rawColumnNames(t, path, "nodes")["namespace_id"]; !got {
		t.Fatal("v3 的 nodes.namespace_id 被删了：程序不许删用户数据")
	}
	if got := rawColumnNames(t, path, "capability_versions")["outputs"]; !got {
		t.Fatal("v3 的 capability_versions.outputs 被删了：程序不许删用户数据")
	}
}

// TestFormatVersionlessLegacyRejected 没有记账、没有任何布局标记、却已有业务表的旧库。
func TestFormatVersionlessLegacyRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-tables.db")
	raw := openRawFixture(t, path)
	mustExecSQL(t, raw,
		`CREATE TABLE users (
		    id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE,
		    display_name TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT 'member')`,
		`INSERT INTO users (id, username) VALUES ('u1', 'root')`,
		`CREATE TABLE node_capabilities (node_id TEXT NOT NULL, capability TEXT NOT NULL)`,
		`INSERT INTO node_capabilities (node_id, capability) VALUES ('n1', 'cap')`,
	)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before := snapshotDatabase(t, path)

	_, err := store.Open(path)
	if err == nil {
		t.Fatal("没有记账表但有业务表的旧库应该被拒绝，实际 Open 成功")
	}
	if !errors.Is(err, store.ErrUnsupportedFormat) {
		t.Fatalf("应返回 ErrUnsupportedFormat，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "旧版业务表") || !strings.Contains(err.Error(), "users") {
		t.Fatalf("错误信息应点名旧版业务表: %v", err)
	}
	if after := snapshotDatabase(t, path); after != before {
		t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
	}
	// schema_version 是拒绝之后才该存在的东西：拒绝路径不能先建它（那会抹掉判据）。
	if rawTableExists(t, path, "schema_version") {
		t.Fatal("拒绝旧库之前就先建了 schema_version（会抹掉「没有记账」这条证据）")
	}
}

// TestFormatLedgerAheadOfHeadRejected schema_version 记着比 headVersion 更高的版本（v3 的 1..11）。
func TestFormatLedgerAheadOfHeadRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-version.db")
	raw := openRawFixture(t, path)
	mustExecSQL(t, raw,
		`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version, applied_at) VALUES (1, 1), (2, 1), (11, 1)`,
		`CREATE TABLE legacy_keep (id TEXT PRIMARY KEY)`,
		`INSERT INTO legacy_keep (id) VALUES ('keep')`,
	)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before := snapshotDatabase(t, path)

	_, err := store.Open(path)
	if err == nil {
		t.Fatal("schema_version 高于 headVersion 的旧库应该被拒绝，实际 Open 成功")
	}
	if !errors.Is(err, store.ErrUnsupportedFormat) {
		t.Fatalf("应返回 ErrUnsupportedFormat，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "11") {
		t.Fatalf("错误信息应点名最高版本 11: %v", err)
	}
	if after := snapshotDatabase(t, path); after != before {
		t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
	}
}

// TestFormatUnknownLayoutMarkerRejected 未知布局标记（不是 0，也不是 4）。
func TestFormatUnknownLayoutMarkerRejected(t *testing.T) {
	for _, marker := range []int{1, 3, 5, 99} {
		t.Run(fmt.Sprintf("marker%d", marker), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fmt.Sprintf("marker%d.db", marker))
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw,
				fmt.Sprintf(`PRAGMA user_version = %d`, marker),
				`CREATE TABLE some_other_layout (id TEXT PRIMARY KEY)`,
				`INSERT INTO some_other_layout (id) VALUES ('x')`,
			)
			if err := raw.Close(); err != nil {
				t.Fatalf("close raw: %v", err)
			}
			before := snapshotDatabase(t, path)

			_, err := store.Open(path)
			if err == nil {
				t.Fatalf("未知布局标记 %d 应该被拒绝，实际 Open 成功", marker)
			}
			if !errors.Is(err, store.ErrUnsupportedFormat) {
				t.Fatalf("应返回 ErrUnsupportedFormat，实际: %v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("user_version=%d", marker)) {
				t.Fatalf("错误信息应点名未知的布局标记 %d: %v", marker, err)
			}
			if after := snapshotDatabase(t, path); after != before {
				t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// TestFormatMarkerWithoutLedgerRejected marker=4 但没有记账表：半写的 v4 库。
//
// 关键点：**不许**因为「标记是 4」就建 baseline 补齐——那等于把一个半写库
// 当成新库来用，而它的表可能缺一半。
func TestFormatMarkerWithoutLedgerRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker-no-ledger.db")
	raw := openRawFixture(t, path)
	mustExecSQL(t, raw, `PRAGMA user_version = 4`)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before := snapshotDatabase(t, path)

	_, err := store.Open(path)
	if err == nil {
		t.Fatal("marker=4 但没有 schema_version 记账的半写库应该被拒绝，实际 Open 成功")
	}
	if !errors.Is(err, store.ErrSchemaInvalid) {
		t.Fatalf("应返回 ErrSchemaInvalid，实际: %v", err)
	}
	if errors.Is(err, store.ErrUnsupportedFormat) {
		t.Fatalf("marker=4 是「自称 v4 但结构坏」，不该报 ErrUnsupportedFormat: %v", err)
	}
	if after := snapshotDatabase(t, path); after != before {
		t.Fatalf("拒绝改动了数据目录（不许补建 baseline）:\n before %s\n after  %s", before, after)
	}
	if len(rawUserTables(t, path)) != 0 {
		t.Fatalf("拒绝时建出了表: %v", rawUserTables(t, path))
	}
}

// TestFormatMarkerContradictoryLedgerRejected marker=4 与记账内容互相矛盾。
func TestFormatMarkerContradictoryLedgerRejected(t *testing.T) {
	cases := []struct {
		name     string
		stmts    []string
		wantHint string
	}{
		{
			name: "baseline_row_missing",
			stmts: []string{
				`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
			},
			wantHint: "baseline",
		},
		{
			name: "only_higher_version",
			stmts: []string{
				`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
				`INSERT INTO schema_version (version, applied_at) VALUES (2, 1)`,
			},
			wantHint: "baseline",
		},
		{
			name: "ledger_ahead_of_head",
			stmts: []string{
				`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
				`INSERT INTO schema_version (version, applied_at) VALUES (1, 1), (2, 1)`,
			},
			wantHint: "高于",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name+".db")
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, `PRAGMA user_version = 4`)
			mustExecSQL(t, raw, tc.stmts...)
			if err := raw.Close(); err != nil {
				t.Fatalf("close raw: %v", err)
			}
			before := snapshotDatabase(t, path)

			_, err := store.Open(path)
			if err == nil {
				t.Fatal("标记与记账矛盾的库应该被拒绝，实际 Open 成功")
			}
			if !errors.Is(err, store.ErrSchemaInvalid) {
				t.Fatalf("应返回 ErrSchemaInvalid，实际: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Fatalf("错误信息应包含 %q: %v", tc.wantHint, err)
			}
			if after := snapshotDatabase(t, path); after != before {
				t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
			}
		})
	}
}

// TestFormatMarkerMissingRequiredTableRejected marker=4 但缺一张 baseline 里的表。
func TestFormatMarkerMissingRequiredTableRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-table.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("create valid v4 db: %v", err)
	}
	newNS(t, db, "keep")
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 造坏库：删掉 emits（emit 幂等记录的落点）。
	raw := openRawFixture(t, path)
	mustExecSQL(t, raw, `DROP TABLE emits`)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before := snapshotDatabase(t, path)

	_, err = store.Open(path)
	if err == nil {
		t.Fatal("marker=4 但缺 emits 表的库应该被拒绝，实际 Open 成功")
	}
	if !errors.Is(err, store.ErrSchemaInvalid) {
		t.Fatalf("应返回 ErrSchemaInvalid，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "缺表 emits") {
		t.Fatalf("错误信息应点名缺表 emits: %v", err)
	}
	if after := snapshotDatabase(t, path); after != before {
		t.Fatalf("拒绝改动了数据目录（不许 CREATE IF NOT EXISTS 自愈）:\n before %s\n after  %s", before, after)
	}
	if rawTableExists(t, path, "emits") {
		t.Fatal("坏库被自愈了：emits 又被建出来了")
	}
	if got := rawRowCount(t, path, "namespaces"); got != 1 {
		t.Fatalf("拒绝后业务数据丢了：namespaces 行数 = %d, want 1", got)
	}
}

// TestFormatMarkerMissingRequiredColumnRejected marker=4 但关键表缺列（半写/结构漂移）。
func TestFormatMarkerMissingRequiredColumnRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-column.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("create valid v4 db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// nodes.facts 是节点 hello 快照的落点：没了它「命令缺失/命名空间未授权」都答不出来。
	raw := openRawFixture(t, path)
	mustExecSQL(t, raw, `ALTER TABLE nodes RENAME COLUMN facts TO node_facts_v3`)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before := snapshotDatabase(t, path)

	_, err = store.Open(path)
	if err == nil {
		t.Fatal("marker=4 但 nodes 缺 facts 列的库应该被拒绝，实际 Open 成功")
	}
	if !errors.Is(err, store.ErrSchemaInvalid) {
		t.Fatalf("应返回 ErrSchemaInvalid，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "缺列 nodes.facts") {
		t.Fatalf("错误信息应点名缺列 nodes.facts: %v", err)
	}
	if after := snapshotDatabase(t, path); after != before {
		t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
	}
	if rawColumnNames(t, path, "nodes")["facts"] {
		t.Fatal("坏库被自愈了：nodes.facts 又被加回来了")
	}
}

// TestFormatMarkerRetiredColumnRejected marker=4 却带着 v3 的退役字段。
//
// 这种库最危险：列在、v4 代码不读它 →「能写进去、读出来丢」。
func TestFormatMarkerRetiredColumnRejected(t *testing.T) {
	cases := []struct{ table, column string }{
		{"nodes", "namespace_id"},          // v4 节点是集群级
		{"node_instances", "namespace_id"}, // 同上
		{"capability_versions", "outputs"}, // v3 的 outputs 退役
		{"capability_versions", "content_hash"},
		{"tasks", "status"},    // v4 是 phase
		{"tasks", "attempts"},  // v4 是 attempt + max_attempts
		{"edges", "from_port"}, // v4 边没有端口
		{"edges", "priority"},  // v4 边没有优先级
	}
	for _, tc := range cases {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "retired.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatalf("create valid v4 db: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN "%s" TEXT`, tc.table, tc.column))
			if err := raw.Close(); err != nil {
				t.Fatalf("close raw: %v", err)
			}
			before := snapshotDatabase(t, path)

			_, err = store.Open(path)
			if err == nil {
				t.Fatalf("marker=4 但 %s.%s 残留的库应该被拒绝，实际 Open 成功", tc.table, tc.column)
			}
			if !errors.Is(err, store.ErrSchemaInvalid) {
				t.Fatalf("应返回 ErrSchemaInvalid，实际: %v", err)
			}
			want := "残留 v3 列 " + tc.table + "." + tc.column
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("错误信息应点名 %q: %v", want, err)
			}
			if after := snapshotDatabase(t, path); after != before {
				t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
			}
			if !rawColumnNames(t, path, tc.table)[tc.column] {
				t.Fatal("拒绝时删了列：程序不许改用户的数据结构")
			}
		})
	}
}

// TestFormatMarkerMissingUniqueIndexRejected marker=4 但缺关键唯一约束。
//
// emits 的 (task_id, emit_id) 唯一索引是 emit 幂等的唯一落点：没了它，
// 同一 emit_id 会被重复派发。
func TestFormatMarkerMissingUniqueIndexRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-unique.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("create valid v4 db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw := openRawFixture(t, path)
	mustExecSQL(t, raw, `DROP INDEX idx_emits_task_emit`)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before := snapshotDatabase(t, path)

	_, err = store.Open(path)
	if err == nil {
		t.Fatal("marker=4 但缺 emits 唯一索引的库应该被拒绝，实际 Open 成功")
	}
	if !errors.Is(err, store.ErrSchemaInvalid) {
		t.Fatalf("应返回 ErrSchemaInvalid，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "缺唯一约束 emits (task_id, emit_id)") {
		t.Fatalf("错误信息应点名缺的唯一约束: %v", err)
	}
	if after := snapshotDatabase(t, path); after != before {
		t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
	}
	if rawIndexExists(t, path, "idx_emits_task_emit") {
		t.Fatal("坏库被自愈了：唯一索引又被建出来了")
	}
}

// TestFormatCollidingBaselineViewRejectedBeforeInitialization 保留原碰撞夹具，但非空库必须在事务前拒绝。
// 这不是中途 DDL 回滚证明；真正的注入回滚测试留给旧 inpackage 测试闭包卡。
func TestFormatCollidingBaselineViewRejectedBeforeInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline-fail.db")
	raw := openRawFixture(t, path)
	// 与 baseline 末段的索引同名：CREATE INDEX IF NOT EXISTS 撞上视图会直接报错
	// （IF NOT EXISTS 只对同名**索引**生效，跨对象类型的重名仍然是错误）。
	mustExecSQL(t, raw, `CREATE VIEW idx_sources_due AS SELECT 1 AS enabled`)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before := snapshotDatabase(t, path)

	_, err := store.Open(path)
	if err == nil {
		t.Fatal("非空库必须在 baseline 前拒绝，实际 Open 成功")
	}
	if !errors.Is(err, store.ErrUnsupportedFormat) || !strings.Contains(err.Error(), "idx_sources_due") {
		t.Fatalf("应在初始化前具名拒绝已有用户视图: %v", err)
	}
	t.Logf("baseline 碰撞夹具现为事务前非空拒绝: %v", err)
	after := snapshotDatabase(t, path)
	if after != before {
		t.Fatalf("事务前拒绝改动了原视图:\n before %s\n after  %s", before, after)
	}
	if got := rawUserVersion(t, path); got != 0 {
		t.Fatalf("事务前拒绝却写下了布局标记（PRAGMA user_version=%d）：不能初始化非空库", got)
	}
	if tables := rawUserTables(t, path); len(tables) != 0 {
		t.Fatalf("事务前拒绝却留下了表: %v", tables)
	}
}

// TestFormatCollidingLedgerViewRejectedBeforeInitialization 保留原同名 ledger 视图夹具，具名非空拒绝。
// 不再声称已经运行到 INSERT；中途记账/marker 失败白盒覆盖仍待后续授权卡补齐。
func TestFormatCollidingLedgerViewRejectedBeforeInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger-fail.db")
	raw := openRawFixture(t, path)
	// 同名视图也属于已有用户 schema 对象，必须在任何 marker/DDL/INSERT 前拒绝。
	mustExecSQL(t, raw, `CREATE VIEW schema_version AS SELECT 1 AS version`)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	before := snapshotDatabase(t, path)

	_, err := store.Open(path)
	if err == nil {
		t.Fatal("已有 ledger 视图的非空库必须被拒绝，实际成功")
	}
	if !errors.Is(err, store.ErrUnsupportedFormat) || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("应在初始化前具名拒绝已有 ledger 视图: %v", err)
	}
	t.Logf("ledger 碰撞夹具现为事务前非空拒绝: %v", err)
	after := snapshotDatabase(t, path)
	if after != before {
		t.Fatalf("事务前拒绝改动了原 ledger 视图:\n before %s\n after  %s", before, after)
	}
	if got := rawUserVersion(t, path); got != 0 {
		t.Fatalf("事务前拒绝却写下了布局标记（PRAGMA user_version=%d）：不能初始化非空库", got)
	}
	if tables := rawUserTables(t, path); len(tables) != 0 {
		t.Fatalf("事务前拒绝却留下了表: %v", tables)
	}
}

// ---------- 夹具工具（全部直接读真实 SQLite 文件，不经过 store.Open） ----------

// openRawFixture 打开一个**不跑迁移**的裸 SQLite 连接（造旧库/坏库夹具用）。
//
// 不带 foreign_keys：造夹具时要能删被别的表引用的表（那正是「半写库」的样子）。
func openRawFixture(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw sqlite %s: %v", path, err)
	}
	return db
}

// mustExecSQL 依次执行夹具 SQL，任何一步失败立即 Fatal。
func mustExecSQL(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture exec %q: %v", stmt, err)
		}
	}
}

// rawUserVersion 读 PRAGMA user_version（布局格式标记）。
func rawUserVersion(t *testing.T, path string) int {
	t.Helper()
	db := openRawFixture(t, path)
	defer func() { _ = db.Close() }()
	var marker int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&marker); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return marker
}

// rawUserTables 列出用户表（不含 sqlite_ 内部表）。
func rawUserTables(t *testing.T, path string) []string {
	t.Helper()
	db := openRawFixture(t, path)
	defer func() { _ = db.Close() }()
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND substr(name, 1, 7) <> 'sqlite_' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	return out
}

// rawTableExists 判断表是否存在（视图不算）。
func rawTableExists(t *testing.T, path, table string) bool {
	t.Helper()
	db := openRawFixture(t, path)
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
		t.Fatalf("probe table %s: %v", table, err)
	}
	return n > 0
}

// rawIndexExists 判断索引/其它同名对象是否存在。
func rawIndexExists(t *testing.T, path, index string) bool {
	t.Helper()
	db := openRawFixture(t, path)
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE name=?`, index).Scan(&n); err != nil {
		t.Fatalf("probe index %s: %v", index, err)
	}
	return n > 0
}

// rawColumnNames 返回表的列名集合（表不存在时为空）。
func rawColumnNames(t *testing.T, path, table string) map[string]bool {
	t.Helper()
	db := openRawFixture(t, path)
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("probe columns of %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column of %s: %v", table, err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns of %s: %v", table, err)
	}
	return out
}

// rawRowCount 数一张表的行数。
func rawRowCount(t *testing.T, path, table string) int {
	t.Helper()
	db := openRawFixture(t, path)
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM "` + table + `"`).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// snapshotDatabase 把「这个数据目录的可观察状态」拍成一段稳定文本：
// 布局标记 + sqlite_master 全量（对象集合与 DDL）+ 每张用户表的完整行值。
//
// 为什么不用文件字节：SQLite 自己的 journal/WAL 字节不是契约目标（父裁决排除），
// 但**用户数据与结构、布局标记**必须原样保留——这三样正好覆盖在快照里。
// 同数量覆写、NULL/类型变化与重复行也必须被快照捕获。
func snapshotDatabase(t *testing.T, path string) string {
	t.Helper()
	db := openRawFixture(t, path)
	defer func() { _ = db.Close() }()

	var b strings.Builder
	fmt.Fprintf(&b, "user_version=%d\n", rawUserVersion(t, path))

	rows, err := db.Query(
		`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master
		  WHERE substr(name, 1, 7) <> 'sqlite_' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	for rows.Next() {
		var typ, name, tbl, ddl string
		if err := rows.Scan(&typ, &name, &tbl, &ddl); err != nil {
			_ = rows.Close()
			t.Fatalf("scan sqlite_master: %v", err)
		}
		fmt.Fprintf(&b, "master %s|%s|%s|%s\n", typ, name, tbl, ddl)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close sqlite_master: %v", err)
	}

	for _, table := range rawUserTables(t, path) {
		fmt.Fprintf(&b, "rows %s=%q\n", table, snapshotTableRows(t, db, table))
	}
	return b.String()
}

// snapshotTableRows 保留每一列的类型和值，排序只消除无 ORDER BY 的读取顺序。
func snapshotTableRows(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
	if err != nil {
		t.Fatalf("snapshot rows %s: %v", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("snapshot columns %s: %v", table, err)
	}
	var out []string
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("snapshot scan %s: %v", table, err)
		}
		var b strings.Builder
		for _, v := range values {
			fmt.Fprintf(&b, "%T:%#v|", v, v)
		}
		out = append(out, b.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot iterate %s: %v", table, err)
	}
	sort.Strings(out)
	return out
}

func TestFormatExistingUserObjectsRejected(t *testing.T) {
	cases := []struct {
		name string
		sql  []string
	}{
		{"view_only", []string{`CREATE VIEW keep_view AS SELECT 'sentinel' AS value`}},
		{"sqliteXkeep_data", []string{`CREATE TABLE sqliteXkeep (id INTEGER PRIMARY KEY, value TEXT, bytes BLOB)`, `INSERT INTO sqliteXkeep VALUES (1, 'keep-original', X'0001ff')`}},
		{"schema_only_table", []string{`CREATE TABLE keep_table (value TEXT)`}},
		{"view_trigger", []string{`CREATE VIEW keep_view AS SELECT 'sentinel' AS value`, `CREATE TRIGGER keep_trigger INSTEAD OF INSERT ON keep_view BEGIN SELECT 1; END`}},
		{"table_index", []string{`CREATE TABLE keep_table (value TEXT)`, `CREATE INDEX keep_index ON keep_table(value)`, `INSERT INTO keep_table VALUES ('sentinel')`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "objects.db")
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, tc.sql...)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			assertFormatRefusalUnchanged(t, path, store.ErrUnsupportedFormat, "")
		})
	}
}

func TestFormatEmitIdentityRequiresExactUniqueKey(t *testing.T) {
	for _, tc := range []struct{ name, definition string }{
		{"suffix", `(task_id, emit_id, to_ref)`},
		{"where_zero", `(task_id, emit_id) WHERE 0`},
		{"where_zero_comment_decoy", `(task_id, emit_id) /* gap */ WHERE 0 -- ) WHERE emit_id IS NOT NULL`},
		{"block_comment_decoy", `(task_id, emit_id) /* ) WHERE emit_id IS NOT NULL */ WHERE 0`},
		{"reverse_order", `(emit_id, task_id)`},
		{"expression_suffix", `(task_id, emit_id, lower(to_ref))`},
		{"expression_key", `(task_id, lower(emit_id))`},
		{"wrong_partial", `(task_id, emit_id) WHERE to_ref = 'protected'`},
		{"weaker_partial", `(task_id, emit_id) WHERE emit_id IS NOT NULL AND to_ref = 'protected'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := seededFormatFixture(t)
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, `DROP INDEX idx_emits_task_emit`, `CREATE UNIQUE INDEX replacement_emit ON emits`+tc.definition)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			assertFormatRefusalUnchanged(t, path, store.ErrSchemaInvalid, "emits")
		})
	}
}

// 内部操作数分组不能当作整个谓词的无害括号；NOT NULL 是值为 NULL 的表达式。
func TestFormatEmitIdentityGroupedOperandsRejected(t *testing.T) {
	for _, tc := range []struct {
		name, predicate string
		inverted        bool
	}{
		{"is_grouped_not_null", `emit_id IS (NOT NULL)`, true},
		{"wrapped_is_grouped_not_null", `(([emit_id] IS (/* unary NOT */ NOT NULL)))`, true},
		{"is_not_grouped_null", `emit_id IS NOT (NULL)`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := seededFormatFixture(t)
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, `DROP INDEX idx_emits_task_emit`,
				`CREATE UNIQUE INDEX replacement_emit ON emits(task_id, emit_id) WHERE `+tc.predicate)
			if tc.inverted {
				mustExecSQL(t, raw, `INSERT INTO emits (id, task_id, emit_id, to_ref, data, created_at)
					VALUES ('e-duplicate', 't-keep', 'emit-keep', 'duplicate-target', '{"input":"duplicate-value"}', 124)`)
				var duplicates int
				if err := raw.QueryRow(`SELECT count(*) FROM emits WHERE task_id='t-keep' AND emit_id='emit-keep' AND emit_id IS NOT NULL`).Scan(&duplicates); err != nil {
					t.Fatal(err)
				}
				if duplicates != 2 {
					t.Fatalf("inverted predicate fixture has %d non-NULL duplicate identities, want 2", duplicates)
				}
				t.Logf("SQLite admitted %d non-NULL rows with the same (task_id, emit_id) under %s", duplicates, tc.predicate)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			assertFormatRefusalUnchanged(t, path, store.ErrSchemaInvalid, "emits")
		})
	}
}

func TestFormatEmitIdentityEquivalentUniqueIndexAccepted(t *testing.T) {
	for _, definition := range []string{`(task_id, emit_id) WHERE emit_id IS NOT NULL`, `(task_id, emit_id) where ("emit_id" is not null)`, `(task_id, emit_id) WHERE /* only named emits */ (([emit_id]) IS NOT NULL)`, `(task_id, emit_id)`} {
		t.Run(definition, func(t *testing.T) {
			path := seededFormatFixture(t)
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, `DROP INDEX idx_emits_task_emit`, `CREATE UNIQUE INDEX renamed_emit_identity ON emits`+definition)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotDatabase(t, path)
			db, err := store.Open(path)
			if err != nil {
				t.Fatalf("exact identity index should be accepted: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if after := snapshotDatabase(t, path); after != before {
				t.Fatal("reopen changed business values/DDL/marker")
			}
		})
	}
}

// 对 baseline 每张表的每一列造 rename 反例，包含原门禁漏掉的两项机器/命名空间输入。
func TestFormatMarkerAllBaselineColumnsRequired(t *testing.T) {
	db := openTestDB(t)
	for _, table := range append(append([]string{}, baselineTables...), "schema_version") {
		for _, col := range sortedKeys(columnNames(t, db, table)) {
			t.Run(table+"."+col, func(t *testing.T) {
				path := seededFormatFixture(t)
				raw := openRawFixture(t, path)
				mustExecSQL(t, raw, `ALTER TABLE "`+table+`" RENAME COLUMN "`+col+`" TO c1r_missing`)
				if err := raw.Close(); err != nil {
					t.Fatal(err)
				}
				assertFormatRefusalUnchanged(t, path, store.ErrSchemaInvalid, table)
			})
		}
	}
}

func TestFormatMarkerIdentityConstraintsRequired(t *testing.T) {
	for _, tc := range []struct{ name, table, old, replacement string }{
		{"node_name_unique", "nodes", `UNIQUE (name)`, `UNIQUE (name, token_hash)`},
		{"attempt_pk_suffix", "task_attempts", `PRIMARY KEY (task_id, attempt)`, `PRIMARY KEY (task_id, attempt, node_id)`},
		{"attempt_pk_order", "task_attempts", `PRIMARY KEY (task_id, attempt)`, `PRIMARY KEY (attempt, task_id)`},
		{"attempt_unique_not_pk", "task_attempts", `PRIMARY KEY (task_id, attempt)`, `UNIQUE (task_id, attempt)`},
		{"node_unique_not_pk", "nodes", `id                    TEXT PRIMARY KEY`, `id                    TEXT UNIQUE`},
		{"ledger_rowid_identity", "schema_version", `version    INTEGER PRIMARY KEY`, `version    INTEGER PRIMARY KEY DESC`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := seededFormatFixture(t)
			raw := openRawFixture(t, path)
			rebuildFormatFixtureTable(t, raw, tc.table, tc.old, tc.replacement)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			assertFormatRefusalUnchanged(t, path, store.ErrSchemaInvalid, tc.table)
		})
	}
}

func TestFormatMarkerForeignKeysRequired(t *testing.T) {
	for _, tc := range []struct{ name, replacement string }{
		{"missing", `/* REFERENCES tasks(id) ON DELETE CASCADE */`},
		{"wrong_target", `REFERENCES users(id) ON DELETE CASCADE`},
		{"wrong_column", `REFERENCES tasks(trace_id) ON DELETE CASCADE`},
		{"wrong_action", `REFERENCES tasks(id) ON DELETE RESTRICT`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := seededFormatFixture(t)
			raw := openRawFixture(t, path)
			rebuildFormatFixtureTable(t, raw, "task_attempts", `REFERENCES tasks(id) ON DELETE CASCADE`, tc.replacement)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			assertFormatRefusalUnchanged(t, path, store.ErrSchemaInvalid, "task_attempts")
		})
	}
}

func TestFormatMarkerMalformedLedgerRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  []string
	}{
		{"missing_version", []string{`CREATE TABLE schema_version (old_version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`, `INSERT INTO schema_version VALUES (1, 123)`}},
		{"missing_applied_at", []string{`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, old_time INTEGER NOT NULL)`, `INSERT INTO schema_version VALUES (1, 123)`}},
		{"bad_version_value", []string{`CREATE TABLE schema_version (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`, `INSERT INTO schema_version VALUES ('bad-version', 123)`}},
		{"bad_timestamp_value", []string{`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`, `INSERT INTO schema_version VALUES (1, 'bad-time')`}},
		{"duplicate_versions", []string{`CREATE TABLE schema_version (version INTEGER, applied_at INTEGER NOT NULL)`, `INSERT INTO schema_version VALUES (1, 123), (1, 124)`}},
		{"extra_zero_version", []string{`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`, `INSERT INTO schema_version VALUES (1, 123), (0, 124)`}},
		{"extra_negative_version", []string{`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`, `INSERT INTO schema_version VALUES (1, 123), (-1, 124)`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := seededFormatFixture(t)
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, `DROP TABLE schema_version`)
			mustExecSQL(t, raw, tc.sql...)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			assertFormatRefusalUnchanged(t, path, store.ErrSchemaInvalid, "schema_version")
		})
	}
}

func TestFormatUnmarkedMalformedLedgerUnsupported(t *testing.T) {
	for _, marker := range []int{0, 99} {
		t.Run(fmt.Sprint(marker), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unrecognized.db")
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, `CREATE TABLE schema_version (old_version TEXT)`, `INSERT INTO schema_version VALUES ('sentinel')`, fmt.Sprintf(`PRAGMA user_version=%d`, marker))
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			assertFormatRefusalUnchanged(t, path, store.ErrUnsupportedFormat, "")
		})
	}
}

func TestFormatSnapshotDetectsSameCountOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.db")
	raw := openRawFixture(t, path)
	mustExecSQL(t, raw, `CREATE TABLE sqliteXkeep (value, bytes)`, `INSERT INTO sqliteXkeep VALUES ('original', X'00ff')`)
	before := snapshotDatabase(t, path)
	mustExecSQL(t, raw, `UPDATE sqliteXkeep SET value='overwritten', bytes=X'01ff'`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshotDatabase(t, path) == before {
		t.Fatal("same-count overwrite escaped snapshot")
	}
}

func assertFormatRefusalUnchanged(t *testing.T, path string, want error, hint string) {
	t.Helper()
	before := snapshotDatabase(t, path)
	db, err := store.Open(path)
	if db != nil {
		_ = db.Close()
	}
	// 即便错误分类不对也先比较全部行值，不能靠提前 Fatal 藏掉覆写。
	if after := snapshotDatabase(t, path); after != before {
		t.Fatalf("refusal wrote business values/DDL/marker:\nbefore %s\nafter %s", before, after)
	}
	if !errors.Is(err, want) {
		t.Fatalf("want %v, got %v", want, err)
	}
	if !strings.Contains(err.Error(), hint) {
		t.Fatalf("want named defect %q, got %v", hint, err)
	}
}

func seededFormatFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "marked.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw := openRawFixture(t, path)
	mustExecSQL(t, raw,
		`INSERT INTO users (id, username, display_name, created_at) VALUES ('u-keep', 'keep-user', 'keep-display', 123)`,
		`INSERT INTO namespaces (id, slug, name, created_at) VALUES ('ns-keep', 'keep-ns', 'keep-name', 123)`,
		`INSERT INTO namespace_configs (namespace_id, vars, updated_at) VALUES ('ns-keep', '{"input":"keep-ns-value"}', 123)`,
		`INSERT INTO nodes (id, name, token_hash, created_at) VALUES ('n-keep', 'keep-node', 'keep-hash', 123)`,
		// S1-D1：已解析任务必须钉住真实 version/digest（baseline CHECK），种子行补 digest 事实。
		`INSERT INTO tasks (id, namespace_id, trace_id, capability_id, capability, version, digest, created_at) VALUES ('t-keep', 'ns-keep', 'trace-keep', 'cap-keep', 'cap-keep', 1, 'sha256:keep', 123)`,
		`INSERT INTO task_attempts (task_id, attempt, node_id, machine_limits, machine_vars, machine_var_sources) VALUES ('t-keep', 1, 'n-keep', '{"cpu":2}', '{"input":"keep-node-value"}', '{"input":"local"}')`,
		`INSERT INTO emits (id, task_id, emit_id, to_ref, data, created_at) VALUES ('e-keep', 't-keep', 'emit-keep', 'target-keep', '{"input":"keep-emit-value"}', 123)`,
	)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// 不 rename 旧表（那会改写其他表的 FK），只换目标表的约束并保留全部实际数据。
func rebuildFormatFixtureTable(t *testing.T, db *sql.DB, table, old, replacement string) {
	t.Helper()
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if strings.Count(ddl, old) != 1 {
		t.Fatalf("fixture constraint %q missing/ambiguous in %s", old, ddl)
	}
	ddl = strings.Replace(ddl, old, replacement, 1)
	ddl = strings.Replace(ddl, table, "c1r_replacement", 1)
	mustExecSQL(t, db, ddl, `INSERT INTO c1r_replacement SELECT * FROM "`+table+`"`, `DROP TABLE "`+table+`"`, `ALTER TABLE c1r_replacement RENAME TO "`+table+`"`)
}

// TestFormatSourceDisplayNameRequired checks the newly required Source layout without repair.
func TestFormatSourceDisplayNameRequired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-source-display.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw := openRawFixture(t, path)
	cols := rawColumnNames(t, path, "sources")
	if !cols["display_name"] {
		t.Fatal("fresh v4 sources is missing required display_name")
	}
	mustExecSQL(t, raw, `ALTER TABLE sources DROP COLUMN display_name`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotDatabase(t, path)
	reopened, err := store.Open(path)
	if reopened != nil {
		_ = reopened.Close()
	}
	if !errors.Is(err, store.ErrSchemaInvalid) {
		t.Fatalf("marker4 missing Source display_name: %v", err)
	}
	if after := snapshotDatabase(t, path); after != before {
		t.Fatal("rejected old Source layout was repaired")
	}
}

func TestFormatCapabilityLiveUniquePredicate(t *testing.T) {
	cases := []struct {
		name, predicate string
		valid           bool
	}{
		{"baseline", "deleted_at IS NULL", true},
		{"whole", "((deleted_at IS NULL))", true},
		{"identifier", "(([deleted_at]) IS NULL)", true},
		{"opposite", "deleted_at IS NOT NULL", false},
		{"operandGroup", "deleted_at IS (NOT NULL)", false},
		{"operandNullGroup", "deleted_at IS (NULL)", false},
		{"expression", "(deleted_at + 0) IS NULL", false},
		{"extra", "deleted_at IS NULL OR 1", false},
		{"literal", "'deleted_at' IS NULL", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "live-index.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			ns := newNS(t, db, "live-layout")
			createCap(t, db, testCap(ns, "keep", 1, nil))
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, `DROP INDEX idx_cap_live_name_ver`, `CREATE UNIQUE INDEX idx_cap_live_name_ver ON capability_versions(namespace_id, name, version) WHERE `+tc.predicate)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotDatabase(t, path)
			reopened, err := store.Open(path)
			if reopened != nil {
				_ = reopened.Close()
			}
			if tc.valid && err != nil {
				t.Fatalf("safe baseline predicate rejected: %v", err)
			}
			if !tc.valid && !errors.Is(err, store.ErrSchemaInvalid) {
				t.Fatalf("unsafe predicate accepted: %v", err)
			}
			if after := snapshotDatabase(t, path); after != before {
				t.Fatal("layout validation changed data/schema")
			}
		})
	}
}

func TestFormatCapabilityMissingTombstoneAndFullUniqueRejected(t *testing.T) {
	for _, mode := range []string{"missing-column", "full-only", "active-plus-full", "active-plus-full-desc", "active-plus-tombstone-key", "extra-key"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old-cap.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			ns := newNS(t, db, "cap-layout")
			createCap(t, db, testCap(ns, "keep", 1, nil))
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			raw := openRawFixture(t, path)
			switch mode {
			case "missing-column":
				mustExecSQL(t, raw, `DROP INDEX idx_cap_live_name_ver`, `DROP INDEX idx_cap_live_digest`, `ALTER TABLE capability_versions DROP COLUMN deleted_at`)
			case "full-only":
				mustExecSQL(t, raw, `DROP INDEX idx_cap_live_name_ver`, `CREATE UNIQUE INDEX bad_full ON capability_versions(namespace_id,name,version)`)
			case "active-plus-full":
				mustExecSQL(t, raw, `CREATE UNIQUE INDEX bad_full ON capability_versions(namespace_id,name,version)`)
			case "active-plus-full-desc":
				mustExecSQL(t, raw, `CREATE UNIQUE INDEX bad_full_desc ON capability_versions(namespace_id,name,version DESC)`)
			case "active-plus-tombstone-key":
				mustExecSQL(t, raw, `CREATE UNIQUE INDEX bad_tombstone_key ON capability_versions(namespace_id,name,version) WHERE deleted_at IS NOT NULL`)
			case "extra-key":
				mustExecSQL(t, raw, `DROP INDEX idx_cap_live_name_ver`, `CREATE UNIQUE INDEX bad_extra ON capability_versions(namespace_id,name,version,deleted_at) WHERE deleted_at IS NULL`)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotDatabase(t, path)
			reopened, err := store.Open(path)
			if reopened != nil {
				_ = reopened.Close()
			}
			if !errors.Is(err, store.ErrSchemaInvalid) {
				t.Fatalf("bad capability layout accepted: %v", err)
			}
			if after := snapshotDatabase(t, path); after != before {
				t.Fatal("invalid capability layout repaired")
			}
		})
	}
}
