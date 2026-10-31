package store

// 跨引擎（驱动版本）老库升级用例 —— #242 驱动升版的回归门禁。
//
// 为什么需要它：本批把 modernc.org/sqlite 从 v1.34.5 升到 v1.45.0，这**同时**换了
// 内嵌的 SQLite 引擎版本。对 DDL 最敏感的两步迁移恰好就在这条路径上：
//
//	迁移 14（#205）13 张表逐个重建（去掉 org 层，唯一键前缀换 namespace_id）；
//	迁移 15（#238）按 pragma 判断后 DROP + 重建 idx_edges_lookup。
//
// 「新库能建、老库能升、数据不丢」的既有用例（migrate14_test.go / migrate15_test.go /
// migrate16_test.go）全部是**同一个驱动版本造库、同一个驱动版本升级**，
// 它们结构上不可能发现「旧引擎写出来的库文件在新引擎上行为不同」这类问题
// （页格式 / 索引 b-tree 布局 / schema 文本细节都可能随引擎版本变化）。
// 所以这里补一条真·跨引擎用例：库由 v1.34.5 写，升级与读取由当前驱动做。
//
// ── fixture 是怎么来的（可复现，别手改这个文件）─────────────────────────────
//
//	fixture: internal/store/testdata/legacy_engine1345_v13.db.gz
//	内容： 一个停在 **迁移 13** 的库，用提交 3433eef 时代的 schema（迁移 12 之后
//	        所有业务表还带 org_id，迁移 14 才摘掉），带一小组业务行：
//	        1 users / 1 namespaces / 2 capability_versions / 3 events /
//	        20 tasks（15 done + 5 pending）/ 1 edges，外加 orgs 与 org_members 各 1 行
//	        （迁移 14 会把这两张表整体删掉，正是要被覆盖的路径）。
//	生成引擎：modernc.org/sqlite **v1.34.5**（本批升级前的版本）
//
// 生成步骤（在 `git archive 3433eef` 解出来的旧快照里做，**不产在本仓库**）：
//
//	本次实际用的是 /tmp/perf-3a/base-src（那份快照的副本，拷到 /tmp/d242/base-src 后操作，
//	避免与并行跑压测的 worker 抢同一份源码树）；go.mod 里 modernc.org/sqlite v1.34.5，
//	内嵌 SQLite **3.46.0**；当前驱动 v1.45.0 内嵌 SQLite **3.51.2**（版本差异由
//	TestEngineCompatFixtureWrittenByOlderEngine 从文件头 96..100 字节直接验证）。
//
//	$ cd <旧驱动快照>            # go.mod 里 modernc.org/sqlite v1.34.5
//	$ cat > internal/store/zz_genfixture_test.go <<'EOF'
//	  package store
//	  func TestGenLegacyFixture(t *testing.T) {
//	      // 停在迁移 13：14/15/16 都留给新引擎跑
//	      db := openAtVersion(t, "/tmp/legacy_engine1345_v13.db", 13)
//	      defer db.Close()
//	      // v13 时代的列（PRAGMA table_info 核对过）：
//	      db.Exec(`INSERT INTO orgs (id, name, created_at) VALUES (?,?,?)`, "org_fixture", "fixture-org", 1700000000)
//	      db.Exec(`INSERT INTO users (id, username, display_name, role, disabled, created_at) VALUES (?,?,?,?,?,?)`,
//	          "usr_fixture", "legacy-fixture", "Legacy Fixture", "admin", 0, 1700000000)
//	      db.Exec(`INSERT INTO org_members (org_id, user_id, role, joined_at) VALUES (?,?,?,?)`,
//	          "org_fixture", "usr_fixture", "owner", 1700000000)
//	      db.Exec(`INSERT INTO namespaces (id, org_id, slug, name, created_at) VALUES (?,?,?,?,?)`,
//	          "ns_fixture", "org_fixture", "prod", "Prod", 1700000001)
//	      db.Exec(`INSERT INTO capability_versions (id, namespace_id, org_id, name, display_name,
//	          version, params_schema, outputs, lifecycle, sticky, source, content_hash, created_at)
//	          VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
//	          "cap_fixture_1", "ns_fixture", "org_fixture", "review", "审查", 1,
//	          `{"type":"object"}`, `["fix","clean"]`, `{}`, "", "builtin", "hash_review_v1", 1700000002)
//	      // …cap_fixture_2 = notify.phone；3 条 events；20 条 tasks（capability_id 指 cap_fixture_1）；
//	      // 1 条 edges(cap_fixture_1.fix → cap_fixture_2)
//	      db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); db.Exec(`VACUUM`)
//	      db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
//	      // 自检：各表行数 + PRAGMA integrity_check == ok + max(schema_version) == 13
//	  }
//	EOF
//	$ go test ./internal/store/ -run TestGenLegacyFixture -count=1 -v
//	$ gzip -9 -c /tmp/legacy_engine1345_v13.db > internal/store/testdata/legacy_engine1345_v13.db.gz
//
// 体积：VACUUM + checkpoint 后的库文件 356352 B（348 KiB）；gzip -9 后 10577 B。
// 为什么用 .gz 而不是裸 .db：① 仓库 .gitignore 里有 `*.db`（那条规则针对运行期库文件，
// 本轮不动它），裸 .db 需 `git add -f` 才进得去；② 二进制 fixture 进 git 历史是永久的，
// 压缩形态小 34 倍。测试侧解压到 t.TempDir() 再打开，行为与裸 .db 完全一样。
//
// ── 为什么只能「提交 fixture」，不能在测试里现造 ────────────────────────────
//
// 同一个进程里无法同时加载两个版本的 modernc.org/sqlite：两个包都 `sql.Register("sqlite", ...)`，
// 第二次注册直接 panic（`sql: Register called twice for driver sqlite`）。而这是**包级**
// 副作用，import 时就发生——连「测完就卸」都做不到。所以要测「旧引擎写的库」，
// 唯一可行的形态就是把旧引擎已经写好的库文件当成二进制 fixture 提交进来。
// （另一条路是把整个旧驱动 fork 进 testdata 并改驱动名/包名，等于把 30MB cgo-free 移植代码
//  塞进仓库，成本与收益完全不匹配。）

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// legacyEngineFixtureGz 是旧引擎（modernc.org/sqlite v1.34.5）写出的 v13 库。
const legacyEngineFixtureGz = "testdata/legacy_engine1345_v13.db.gz"

// legacyEngineFixtureBytes 把 fixture 读进内存（gzip 解压后）。
func legacyEngineFixtureBytes(t *testing.T) []byte {
	t.Helper()
	f, err := os.Open(legacyEngineFixtureGz)
	if err != nil {
		t.Fatalf("打开旧引擎 fixture %s: %v（fixture 缺失就见本文件头的生成步骤）", legacyEngineFixtureGz, err)
	}
	defer func() { _ = f.Close() }()

	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("解压 fixture: %v", err)
	}
	defer func() { _ = zr.Close() }()

	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("读 fixture 内容: %v", err)
	}
	return raw
}

// legacyEngineFixturePath 把 fixture 解压到 t.TempDir() 并返回路径。
//
// 每次调用给调用方一份**全新副本**：升级会写迁移 14/15/16，绝不能让上一个用例
// （或下一个用例）读到已升级的库。
func legacyEngineFixturePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy_engine1345_v13.db")
	if err := os.WriteFile(path, legacyEngineFixtureBytes(t), 0o600); err != nil {
		t.Fatalf("写 fixture 副本: %v", err)
	}
	return path
}

// TestEngineCompatFixtureWrittenByOlderEngine 钉住 fixture 的**血统**：
// 它必须是比当前引擎更旧的 SQLite 写出来的，否则整个跨引擎用例就名不副实
// （文件头 96..100 字节就是写库引擎的 SQLITE_VERSION_NUMBER）。
//
// 顺带的副作用：这条会打印出「写库引擎 vs 当前引擎」的真实版本号，报告直接引用。
func TestEngineCompatFixtureWrittenByOlderEngine(t *testing.T) {
	raw := legacyEngineFixtureBytes(t)
	const sqliteMagic = "SQLite format 3\x00"
	if len(raw) < 100 || string(raw[:16]) != sqliteMagic {
		t.Fatalf("fixture 不是 SQLite 3 数据库文件（头部魔数不对）")
	}
	writerVersion := binary.BigEndian.Uint32(raw[96:100])
	currentVersion := currentSQLiteVersionNumber(t)
	if writerVersion == 0 || currentVersion == 0 {
		t.Fatalf("版本号解析失败：fixture=%d current=%d", writerVersion, currentVersion)
	}
	if writerVersion >= currentVersion {
		t.Fatalf("fixture 的写库引擎 SQLite %s 不低于当前引擎 SQLite %s —— 这条用例不再是「跨引擎」，"+
			"fixture 是被新驱动重写过了吗？", formatSQLiteVersion(writerVersion), formatSQLiteVersion(currentVersion))
	}
	t.Logf("跨引擎成立：fixture 由 SQLite %s（modernc.org/sqlite v1.34.5）写出，当前引擎 SQLite %s（v1.45.0）",
		formatSQLiteVersion(writerVersion), formatSQLiteVersion(currentVersion))
}

// currentSQLiteVersionNumber 读当前驱动内嵌引擎的版本号（SQLITE_VERSION_NUMBER 编码）。
func currentSQLiteVersionNumber(t *testing.T) uint32 {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open :memory: 读引擎版本: %v", err)
	}
	defer func() { _ = db.Close() }()
	var s string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&s); err != nil {
		t.Fatalf("读 sqlite_version(): %v", err)
	}
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) < 3 {
		t.Fatalf("sqlite_version() 返回 %q，解析不出 major.minor.patch", s)
	}
	nums := make([]int, 3)
	for i := 0; i < 3; i++ {
		v, err := strconv.Atoi(parts[i])
		if err != nil {
			t.Fatalf("sqlite_version() 返回 %q，第 %d 段不是数字: %v", s, i+1, err)
		}
		nums[i] = v
	}
	return uint32(nums[0])*1000000 + uint32(nums[1])*1000 + uint32(nums[2])
}

// formatSQLiteVersion 把 SQLITE_VERSION_NUMBER 编码格式化成 major.minor.patch。
func formatSQLiteVersion(v uint32) string {
	return fmt.Sprintf("%d.%d.%d", v/1000000, (v/1000)%1000, v%1000)
}

// TestEngineCompatLegacyEngineUpgradesToTip 是跨引擎用例的主体：
// 旧引擎写的老库在新引擎上 Open，必须升到 tip、结构与新库逐行一致、数据一行不丢。
func TestEngineCompatLegacyEngineUpgradesToTip(t *testing.T) {
	path := legacyEngineFixturePath(t)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("新引擎打开旧引擎(v1.34.5)写的 v13 库失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	// ① 版本升到 tip（不写死数字：tip 是 schema.sql 的函数）。
	tip := testTipVersion(t)
	got, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if got != tip {
		t.Fatalf("升级后 schema_version = %d, want %d（tip）", got, tip)
	}
	if got < 14 {
		t.Fatalf("升级后版本 = %d，连迁移 14 都没跑（fixture 是不是已经被升级过了？）", got)
	}

	// ② 引擎自检：跨版本读写的硬门禁（页格式/索引损坏都会在这里冒出来）。
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("升级后 integrity_check = %q, want ok", integrity)
	}

	// ③ 形状与新库逐行一致（含 DDL 文本）。迁移 14/15 都是「重建」，
	//    引擎换代后重建出来的 DDL 若与新库分叉，这里会逐条列出。
	fresh, _ := newTestDBFile(t)
	assertShapeEqual(t, "旧引擎(v1.34.5) v13 库升级后", schemaShape(t, fresh), schemaShape(t, db))

	// ④ 老数据一行不丢：逐表 count。
	for _, tc := range []struct {
		table string
		want  int
	}{
		{"users", 1},
		{"namespaces", 1},
		{"capability_versions", 2},
		{"events", 3},
		{"tasks", 20},
		{"edges", 1},
	} {
		if got := countRows(t, db, tc.table, "1 = 1"); got != tc.want {
			t.Errorf("%s 行数 = %d, want %d（迁移 14/15/16 不该动业务数据）", tc.table, got, tc.want)
		}
	}
	// 迁移 14 删掉的两张表必须真的消失（不是空壳、也不是 count() 报 no such table 的意外）。
	for _, gone := range []string{"orgs", "org_members"} {
		var exists int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, gone).
			Scan(&exists); err != nil {
			t.Fatalf("查 %s 是否存在: %v", gone, err)
		}
		if exists != 0 {
			t.Errorf("迁移 14 之后 %s 仍存在（%d 条 sqlite_master 记录）", gone, exists)
		}
	}

	// ⑤ 内容不变：抽样关键字段（外键、冗余列、时间戳都是重建时最容易掉的东西）。
	assertLegacyRowValues(t, db)

	// ⑥ 外键在重建后仍然自洽（迁移 14 是「建新表 + INSERT SELECT 回灌」，
	//    外键指向若有偏差，foreign_key_check 会逐行报出来）。
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("foreign_key_check 列名: %v", err)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("foreign_key_check scan: %v", err)
		}
		violations = append(violations, fmt.Sprint(vals...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("升级后外键不自洽（%d 条）: %v", len(violations), violations)
	}

	// ⑦ 读路径真的能用：老库升级后 ListTasks 必须拿到全部 20 行且按
	//    created_at DESC, id DESC 严格有序（迁移 16 的索引就是给这条查询用的）。
	tasks, err := db.ListTasks(context.Background(), "ns_fixture", "", "", "", 200)
	if err != nil {
		t.Fatalf("升级后 ListTasks: %v", err)
	}
	if len(tasks) != 20 {
		t.Fatalf("升级后 ListTasks 返回 %d 行, want 20", len(tasks))
	}
	for i := 1; i < len(tasks); i++ {
		prev, cur := tasks[i-1], tasks[i]
		if cur.CreatedAt > prev.CreatedAt || (cur.CreatedAt == prev.CreatedAt && cur.ID > prev.ID) {
			t.Fatalf("ListTasks 第 %d/%d 行顺序错: %v(%d) 在 %v(%d) 之后",
				i, i+1, prev.ID, prev.CreatedAt, cur.ID, cur.CreatedAt)
		}
	}
}

// assertLegacyRowValues 抽读关键字段，确认迁移没有改写内容。
func assertLegacyRowValues(t *testing.T, db *DB) {
	t.Helper()

	// users：迁移 14 不重建这张表，但身份行必须还在。
	var username, displayName, role string
	if err := db.QueryRow(`SELECT username, display_name, role FROM users WHERE id = ?`, "usr_fixture").
		Scan(&username, &displayName, &role); err != nil {
		t.Fatalf("读 users: %v", err)
	}
	if username != "legacy-fixture" || displayName != "Legacy Fixture" || role != "admin" {
		t.Fatalf("users 内容变了: (%q,%q,%q)", username, displayName, role)
	}

	// namespaces：迁移 14 会把唯一键从 (org_id, slug) 换成 slug，行本身不变。
	var slug, nsName string
	var nsCreatedAt int64
	if err := db.QueryRow(`SELECT slug, name, created_at FROM namespaces WHERE id = ?`, "ns_fixture").
		Scan(&slug, &nsName, &nsCreatedAt); err != nil {
		t.Fatalf("读 namespaces: %v", err)
	}
	if slug != "prod" || nsName != "Prod" || nsCreatedAt != 1700000001 {
		t.Fatalf("namespaces 内容变了: (%q,%q,%d)", slug, nsName, nsCreatedAt)
	}

	// events：trace_id / payload 全查（events 是迁移 14 重建表之一）。
	for _, want := range []struct {
		id, trace, payload string
	}{
		{"evt_fixture_1", "tr_fixture_1", `{"number":1}`},
		{"evt_fixture_2", "tr_fixture_2", `{"number":2}`},
		{"evt_fixture_3", "tr_fixture_3", `{"number":3}`},
	} {
		var trace, source, typ, payload string
		if err := db.QueryRow(`SELECT trace_id, source, type, payload FROM events WHERE id = ?`, want.id).
			Scan(&trace, &source, &typ, &payload); err != nil {
			t.Fatalf("读 events %s: %v", want.id, err)
		}
		if trace != want.trace || source != "github" || typ != "pull_request.opened" || payload != want.payload {
			t.Fatalf("events %s 内容变了: trace=%q source=%q type=%q payload=%q",
				want.id, trace, source, typ, payload)
		}
	}

	// tasks：状态分布 + 抽样一行的 created_at / capability 冗余列。
	var done, pending int
	if err := db.QueryRow(`SELECT
	        sum(CASE WHEN status = 'done' THEN 1 ELSE 0 END),
	        sum(CASE WHEN status = 'pending' THEN 1 ELSE 0 END)
	      FROM tasks`).Scan(&done, &pending); err != nil {
		t.Fatalf("读 tasks 状态分布: %v", err)
	}
	if done != 15 || pending != 5 {
		t.Fatalf("tasks 状态分布 = (done %d, pending %d), want (15, 5)", done, pending)
	}
	var status, capability, capHash, graphID string
	var createdAt int64
	if err := db.QueryRow(`SELECT status, capability, cap_hash, graph_id, created_at
	                         FROM tasks WHERE id = ?`, "tsk_fixture_07").
		Scan(&status, &capability, &capHash, &graphID, &createdAt); err != nil {
		t.Fatalf("读 tasks tsk_fixture_07: %v", err)
	}
	if status != "pending" || capability != "review" || capHash != "hash_review_v1" ||
		graphID != "grf_fixture" || createdAt != 1700000107 {
		t.Fatalf("tasks tsk_fixture_07 内容变了: (%q,%q,%q,%q,%d)",
			status, capability, capHash, graphID, createdAt)
	}

	// tasks → capability_versions 的外键指向仍是旧的那一行（外键不随重建丢失）。
	if n := countRows(t, db, "tasks", "capability_id = ?", "cap_fixture_1"); n != 20 {
		t.Fatalf("tasks.capability_id = cap_fixture_1 的行数 = %d, want 20", n)
	}

	// edges：主键在迁移 14 里换成了 namespace_id 前缀，行内容不变。
	var fromKey, fromPort, toCapability string
	var fromIsSource int
	if err := db.QueryRow(`SELECT from_is_source, from_key, from_port, to_capability
	                         FROM edges`).Scan(&fromIsSource, &fromKey, &fromPort, &toCapability); err != nil {
		t.Fatalf("读 edges: %v", err)
	}
	if fromIsSource != 0 || fromKey != "cap_fixture_1" || fromPort != "fix" || toCapability != "cap_fixture_2" {
		t.Fatalf("edges 内容变了: (%d,%q,%q,%q)", fromIsSource, fromKey, fromPort, toCapability)
	}

	// capability_versions：迁移 14 重建后 outputs / params_schema 的 JSON 文本必须原样。
	var outputs, paramsSchema string
	if err := db.QueryRow(`SELECT outputs, params_schema FROM capability_versions WHERE id = ?`,
		"cap_fixture_1").Scan(&outputs, &paramsSchema); err != nil {
		t.Fatalf("读 capability_versions: %v", err)
	}
	if outputs != `["fix","clean"]` {
		t.Fatalf("capability_versions.outputs = %q, want %q", outputs, `["fix","clean"]`)
	}
	if paramsSchema != `{"type":"object","properties":{"repo":{"type":"string"}}}` {
		t.Fatalf("capability_versions.params_schema = %q（JSON 文本被改写了？）", paramsSchema)
	}
}

// TestEngineCompatLegacyEngineGetsTasksListIndex 断言：旧引擎写的老库升级后，
// 迁移 16 的新索引真的建出来了，且列序是冻结形状。
//
// 为什么要单独一条：这条索引是本批新造的，老库里**不存在**——它只能由新引擎
// 在升级时建出来。这也是「引擎换代之后 DDL 仍然照常生效」的直接证据。
func TestEngineCompatLegacyEngineGetsTasksListIndex(t *testing.T) {
	path := legacyEngineFixturePath(t)

	// 升级前（旧库本身）不应该有这条索引：fixture 若被误升级/误提交就立刻暴露。
	// 这里直接用驱动打开旧库（不跑迁移），只看 sqlite_master。
	pre, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开 fixture（不跑迁移）: %v", err)
	}
	var n int
	if err := pre.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, tasksListIndexName).
		Scan(&n); err != nil {
		_ = pre.Close()
		t.Fatalf("fixture 里数 %s: %v", tasksListIndexName, err)
	}
	_ = pre.Close()
	if n != 0 {
		t.Fatalf("fixture 里已经有 %s（%d 条），说明它不是停在 v13 的库", tasksListIndexName, n)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("新引擎升级旧库: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := countIndexRows(t, db, "tasks", tasksListIndexName); got != 1 {
		t.Fatalf("升级后 tasks 上名为 %s 的索引有 %d 条, want 1", tasksListIndexName, got)
	}
	if got := indexColumnsOn(t, db, "tasks", tasksListIndexName); !sameColumnOrder(got, tasksListIndexColumns) {
		t.Fatalf("升级后 %s 列序 = %v, want %v", tasksListIndexName, got, tasksListIndexColumns)
	}
	t.Logf("升级后 %s 列序 = %v", tasksListIndexName, indexColumnsOn(t, db, "tasks", tasksListIndexName))
}
