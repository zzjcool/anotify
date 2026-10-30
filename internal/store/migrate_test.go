package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseMigrations 校验 schema.sql 的分块序列：编号连续、无重复、名字非空。
func TestParseMigrations(t *testing.T) {
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	if len(set) < 4 {
		t.Fatalf("expected at least 4 migrations, got %d", len(set))
	}
	seen := map[int]bool{}
	for _, m := range set {
		if seen[m.version] {
			t.Fatalf("duplicate migration version %d", m.version)
		}
		seen[m.version] = true
		if m.name == "" {
			t.Fatalf("migration %d has empty name", m.version)
		}
	}
	if set[0].version != 1 {
		t.Fatalf("first migration version must be 1, got %d", set[0].version)
	}
}

// TestParseMigrationsRejectsGap 断言跳号的迁移序列会被拒绝——
// 否则漏掉一个编号会导致「以为迁移过」。
func TestParseMigrationsRejectsGap(t *testing.T) {
	src := `
-- +migration 1 a
SELECT 1;
-- +migration 3 c
SELECT 3;
`
	if _, err := parseMigrations(src); err == nil {
		t.Fatal("expected error for gap in migration sequence, got nil")
	}
}

// TestOpenCreatesAllTables 新库应建出 11 张新表 + 保留表。
func TestOpenCreatesAllTables(t *testing.T) {
	db := newTestDB(t)
	want := []string{
		// 契约 §2 的 11 张
		"orgs", "org_members", "capability_versions", "nodes", "node_capabilities",
		"node_instances", "node_pins", "edges", "events", "tasks", "graph_versions",
		// 命名空间（#162）：组织之下的隔离边界
		"namespaces",
		// 源（#110）：事件入口
		"sources", "source_targets",
		// 沿用：属于人的 4 张 + CLI/恢复码 2 张
		"users", "passkeys", "sessions", "api_keys",
		"cli_auth_sessions", "recovery_codes",
		// 设备（推送目标，属于用户）
		"devices",
		// 迁移机制自身
		"schema_version",
	}
	for _, name := range want {
		if !tableExists(t, db, name) {
			t.Errorf("missing table %s", name)
		}
	}
}

// TestSchemaVersionRecorded 断言迁移全部记账，且版本号 = schema.sql 里的最高编号。
func TestSchemaVersionRecorded(t *testing.T) {
	db := newTestDB(t)
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if want := set[len(set)-1].version; got != want {
		t.Fatalf("schema version = %d, want %d", got, want)
	}
	// 每条都已记账
	for _, m := range set {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM schema_version WHERE version=?`, m.version).Scan(&n); err != nil {
			t.Fatalf("count version %d: %v", m.version, err)
		}
		if n != 1 {
			t.Errorf("migration %d recorded %d times, want 1", m.version, n)
		}
	}
}

// TestMigrationIdempotent 重复 Open 同一库：不应重复应用，也不应报错。
func TestMigrationIdempotent(t *testing.T) {
	db, path := newTestDBFile(t)
	before, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("version before: %v", err)
	}
	ctx := context.Background()
	orgID, userID := newTestNS(t, db)
	_ = ctx
	_ = orgID
	_ = userID

	// 关掉再打开
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	after, err := db2.SchemaVersion()
	if err != nil {
		t.Fatalf("version after: %v", err)
	}
	if before != after {
		t.Fatalf("schema version changed on reopen: %d → %d", before, after)
	}
	// 没有重复记账
	var dup int
	if err := db2.QueryRow(`SELECT count(*) - count(DISTINCT version) FROM schema_version`).Scan(&dup); err != nil {
		t.Fatalf("count duplicates: %v", err)
	}
	if dup != 0 {
		t.Fatalf("schema_version has %d duplicate rows", dup)
	}
}

// TestMigrationFailureRollsBack 注入一个写坏的迁移，断言：
//  1. Open 返回错误（不许吞错）
//  2. 该版本**未记账**
//  3. 该迁移里的建表**未生效**（事务回滚）
func TestMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	_, err := openWith(path, func(set *migrationSet) {
		// 追加一个必定失败的高编号迁移：前半段合法，后半段语法错误。
		*set = append(*set, migration{
			version: len(*set) + 1,
			name:    "broken",
			body: `CREATE TABLE half_done (id TEXT PRIMARY KEY);
			       CREATE TABLE ))) syntax error (((;`,
		})
	})
	if err == nil {
		t.Fatal("expected Open to fail on broken migration, got nil")
	}
	// 错误信息里要能看出是哪个迁移
	if !strings.Contains(err.Error(), "migration") {
		t.Fatalf("error should mention migration, got: %v", err)
	}

	// 用「不加坏迁移」的方式重开，检查上一个事务确实回滚了
	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after failure: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if tableExists(t, db, "half_done") {
		t.Fatal("half_done table exists: failed migration was NOT rolled back")
	}
	if got := mustMaxVersion(t, db); got != len(mustParse(t, schemaSQL)) {
		t.Fatalf("schema version = %d, want %d", got, len(mustParse(t, schemaSQL)))
	}
}

// TestMigrationHookFailureRollsBack hook 里失败同样要回滚且不记账。
func TestMigrationHookFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hookfail.db")
	errSentinel := errors.New("hook boom")
	_, err := openWith(path, func(set *migrationSet) {})
	if err != nil {
		t.Fatalf("baseline open: %v", err)
	}
	// 直接对已建好的库跑一次「带坏 hook 的迁移」
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	extra := migration{version: 999, name: "hookfail", body: `CREATE TABLE hook_half (id TEXT);`}
	if err := applyMigration(db.DB, extra, func(*sql.Tx) error { return errSentinel }); err == nil {
		t.Fatal("expected hook failure to propagate")
	}
	if tableExists(t, db, "hook_half") {
		t.Fatal("hook_half exists: hook failure was NOT rolled back")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_version WHERE version=999`).Scan(&n); err != nil {
		t.Fatalf("count 999: %v", err)
	}
	if n != 0 {
		t.Fatalf("failed migration recorded in schema_version (%d rows)", n)
	}
}

// TestLegacyDBUpgrade 老库（只有 users 表，且缺新列）→ Open 后迁移到位。
func TestLegacyDBUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	db.SetMaxOpenConns(1)
	// 老库：users 没有 role/disabled；另有老 messages 表（带 kind/reply_to）
	mustExec(t, db, `CREATE TABLE users (
		id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE,
		display_name TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`)
	mustExec(t, db, `INSERT INTO users (id, username, display_name, created_at) VALUES ('usr_old','bob','Bob',1)`)
	mustExec(t, db, `CREATE TABLE messages (
		id TEXT PRIMARY KEY, user_id TEXT NOT NULL, seq INTEGER NOT NULL,
		title TEXT NOT NULL, kind TEXT NOT NULL DEFAULT '', reply_to TEXT,
		agent_state TEXT NOT NULL DEFAULT 'working', body TEXT NOT NULL DEFAULT '',
		link TEXT NOT NULL DEFAULT '', device_tags TEXT NOT NULL DEFAULT '[]',
		priority TEXT NOT NULL DEFAULT 'normal', ttl_seconds INTEGER NOT NULL DEFAULT 86400,
		payload TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
		UNIQUE (user_id, seq))`)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	up, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade legacy db: %v", err)
	}
	t.Cleanup(func() { _ = up.Close() })

	// 1. 版本已记账
	if _, err := up.SchemaVersion(); err != nil {
		t.Fatalf("schema version: %v", err)
	}
	// 2. 新表建好
	for _, name := range []string{"orgs", "tasks", "events", "edges", "capability_versions"} {
		if !tableExists(t, up, name) {
			t.Errorf("legacy upgrade missing table %s", name)
		}
	}
	// 3. 老库新列补上
	users := columnNames(t, up, "users")
	for _, col := range []string{"role", "disabled"} {
		if !users[col] {
			t.Errorf("users missing column %s after upgrade", col)
		}
	}
	// 4. 老数据还在
	var username string
	if err := up.QueryRow(`SELECT username FROM users WHERE id='usr_old'`).Scan(&username); err != nil {
		t.Fatalf("legacy user lost: %v", err)
	}
	if username != "bob" {
		t.Fatalf("legacy username = %q, want bob", username)
	}
	// 5. 老 messages 表的 kind / reply_to 被清掉（迁移 3 的重建表）
	msgs := columnNames(t, up, "messages")
	for _, col := range []string{"kind", "reply_to"} {
		if msgs[col] {
			t.Errorf("messages still has legacy column %s", col)
		}
	}
}

// TestMigrationNoSwallowedErrors 确认没有旧写法
// `_, _ = db.Exec("ALTER TABLE ...")` 那种吞错残留。
//
// 只检查真正的代码行（跳过纯注释：注释里提到旧写法是说明，不是实现）。
func TestMigrationNoSwallowedErrors(t *testing.T) {
	check := func(name, src string) {
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "_, _ = ") {
				t.Errorf("%s:%d swallowed error pattern: %s", name, i+1, trimmed)
			}
		}
	}
	check("schema.sql", schemaSQL)
	check("store.go", readSource(t, "store.go"))
	check("migrate.go", readSource(t, "migrate.go"))
}

// TestNoArgvOrKindInSchema 不变量 I1 / I3 的静态门禁（契约 A2 / A4）。
//
// kind 的判定只看**新建模型的表**：`cli_auth_sessions.kind` 是现有的认证字段
// （apikey / passkey），与任务 kind 无关，不在 I3 的管辖范围内。
func TestNoArgvOrKindInSchema(t *testing.T) {
	lower := strings.ToLower(schemaSQL)
	for _, banned := range []string{"argv", "shell", "script"} {
		if strings.Contains(lower, banned) {
			t.Errorf("schema.sql contains banned token %q (invariant I1)", banned)
		}
	}

	// 按迁移块解析：I3 只约束新模型（migration 4 起）的表定义。
	// 保留的旧表 cli_auth_sessions 有 kind 列（授权类型），不受影响。
	//
	// **例外（sources.kind）**：I3 的本意是「**推导量**不落库」——
	// capability 的 kind 由 outputs + 节点声明推导，存下来就可能与推导结果
	// 自相矛盾。但源的 kind（cron/webhook/manual）不是推导量：它是用户显式
	// 选择的**配置**，没有任何东西能推导出"这个源应该是定时器还是 webhook"，
	// 不存就没法知道该跑哪套逻辑。
	//
	// 所以这里按表限定，而不是按列名一刀切：守卫要精确表达不变量，
	// 否则只能靠改名绕过，那不变量就形同虚设了。
	set := mustParse(t, schemaSQL)
	for _, m := range set {
		if m.version < 4 {
			continue
		}
		var curTable string
		for _, line := range strings.Split(m.body, "\n") {
			trimmed := strings.TrimSpace(line)
			// 记录当前处于哪张表的定义里
			if strings.HasPrefix(strings.ToUpper(trimmed), "CREATE TABLE") {
				fields := strings.Fields(trimmed)
				for i, f := range fields {
					if strings.EqualFold(f, "EXISTS") && i+1 < len(fields) {
						curTable = strings.TrimSuffix(fields[i+1], "(")
						break
					}
					if strings.EqualFold(f, "TABLE") && i+1 < len(fields) {
						name := fields[i+1]
						if strings.EqualFold(name, "IF") {
							continue
						}
						curTable = strings.TrimSuffix(name, "(")
						break
					}
				}
				continue
			}
			if strings.HasPrefix(trimmed, ")") || strings.HasPrefix(trimmed, ";") {
				curTable = ""
				continue
			}
			if !strings.HasPrefix(trimmed, "kind ") {
				continue
			}
			// sources.kind 是用户配置，不是推导量（见上方注释）
			if curTable == "sources" {
				continue
			}
			t.Errorf("migration %d declares banned column kind on table %q (invariant I3): %q",
				m.version, curTable, trimmed)
		}
	}
}

// ---------- 小工具 ----------

func mustParse(t *testing.T, src string) migrationSet {
	t.Helper()
	set, err := parseMigrations(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return set
}

func mustMaxVersion(t *testing.T, db *DB) int {
	t.Helper()
	v, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	return v
}

// readSource 读取 store 包里的一个源文件（静态门禁用）。
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
