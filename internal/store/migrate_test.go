package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestParseMigrations 校验 schema.sql 的分块序列：编号连续、无重复、名字非空。
//
// squash 之后从 `1 baseline` 重新起算：baseline 不动，新变更按 `2 ...` 往后追加。
// 这里钉住「块数 == 本代码认识的最高版本」（headVersion）——追加了块却忘了 +1
// 会让新建的库被 legacyVersionError 自己拒掉；反过来改了常量没加块则直接红。
func TestParseMigrations(t *testing.T) {
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	if len(set) != headVersion {
		t.Fatalf("schema.sql 有 %d 块，headVersion = %d（追加块必须同步 +1）", len(set), headVersion)
	}
	if set[0].name != "baseline" {
		t.Fatalf("首块名字 = %q，期望 baseline", set[0].name)
	}
	if set[len(set)-1].version != headVersion {
		t.Fatalf("最高版本 = %d，headVersion = %d", set[len(set)-1].version, headVersion)
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

// TestMigrationTwoColumns 全新库必须带上 migration 2 加的列（`-- +migration 2` 真的跑了）。
//
// baseline 没有改（契约 §1）：新列只能由追加块加出来，所以这条同时守住
// 「别顺手把列加进 baseline」——那样老库永远拿不到它。
func TestMigrationTwoColumns(t *testing.T) {
	db := newTestDB(t)

	if !columnNames(t, db, "edges")["priority"] {
		t.Error("edges 缺列 priority（migration 2 没生效？）")
	}
	// 默认值必须是 0（老行的行为与加列之前一致）：空表当然读不出行。
	var prio int
	if err := db.QueryRow(`SELECT priority FROM edges LIMIT 1`).Scan(&prio); err == nil {
		t.Fatalf("空表竟然读出了 priority=%d", prio)
	}
}

// TestMigrationTenDropsBalance 钉住 P3：`balance` 整列不存在了。
//
// 这条不是「删了一个用例」的附属物——它守的是两件事：
//   - migration 2 的 ALTER（历史块，不再改）与 migration 10 的 DROP 在**新建库**上
//     真的按顺序跑完了（新建库是先加后删，结尾不能有这列）；
//   - 升级库（v9 → v10）也真的没这列了（见 TestMigration9Backfill 的 v7 升级路径）。
//
// 为什么必须为「删列」单独留一条：列还在而代码不读它，症状是「能写进库、读出来丢」，
// 谁也不知道该信哪一个；列真被删掉才只有一份真相。
func TestMigrationTenDropsBalance(t *testing.T) {
	db := newTestDB(t)

	if columnNames(t, db, "capability_versions")["balance"] {
		t.Fatal("capability_versions.balance 还在：migration 10 没生效（P3 已删该字段）")
	}
	// 相关的老列不能跟着一起没：migration 2 的 priority、migration 9 的 spec 都在。
	for _, tc := range []struct{ table, col string }{
		{"edges", "priority"},
		{"capability_versions", "spec"},
	} {
		if !columnNames(t, db, tc.table)[tc.col] {
			t.Errorf("%s 缺列 %s（迁移跑过头了？）", tc.table, tc.col)
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
	// 先灌一点数据（注册会建用户 + 默认命名空间 + 内置能力），再重开：
	// 有数据的库重开也必须整体跳过 baseline（不重复执行、不重复记账）。
	newTestNS(t, db)

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
// I1：schema 里不得出现可执行概念（A2 的关键词）。
// I3：**推导量**不落库——capability / node / edge 都没有 kind 列（kind 由 outputs +
// 节点声明推导）。允许 kind 的只有两张表，且都不是推导量：sources.kind（cron /
// webhook / manual）是用户显式选择的**配置**，没有任何东西能推导出「这个源应该是
// 定时器还是 webhook」；cli_auth_sessions.kind（apikey / passkey）是**授权类型**，
// 与能力在链上的角色无关。
func TestNoArgvOrKindInSchema(t *testing.T) {
	lower := strings.ToLower(schemaSQL)
	for _, banned := range []string{"argv", "shell", "script"} {
		if strings.Contains(lower, banned) {
			t.Errorf("schema.sql contains banned token %q (invariant I1)", banned)
		}
	}

	kindAllowed := map[string]bool{"sources": true, "cli_auth_sessions": true}
	var curTable string
	for _, line := range strings.Split(schemaSQL, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue // 注释里的说明不算列定义
		}
		if strings.HasPrefix(strings.ToUpper(trimmed), "CREATE TABLE") {
			curTable = tableNameInCreate(trimmed)
			continue
		}
		if strings.HasPrefix(trimmed, ")") || strings.HasPrefix(trimmed, ";") {
			curTable = ""
			continue
		}
		if !strings.HasPrefix(trimmed, "kind ") || kindAllowed[curTable] {
			continue
		}
		t.Errorf("table %q declares banned column kind (invariant I3): %q", curTable, trimmed)
	}
}

// tableNameInCreate 从 `CREATE TABLE [IF NOT EXISTS] name (` 里取出表名
// （跳过 IF / NOT / EXISTS，并去掉 RENAME 重建留下的引号）。
func tableNameInCreate(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if !strings.EqualFold(f, "TABLE") {
			continue
		}
		j := i + 1
		for j < len(fields) && (strings.EqualFold(fields[j], "IF") ||
			strings.EqualFold(fields[j], "NOT") || strings.EqualFold(fields[j], "EXISTS")) {
			j++
		}
		if j >= len(fields) {
			return ""
		}
		return strings.Trim(strings.TrimSuffix(fields[j], "("), `"`)
	}
	return ""
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

// ---------- migration 11：deployments 改引用能力行（Capability Spec v3 §11.1）----------

// mig10Legacy 是一份「停在 migration 10 的库」里的产物夹具：
//   - capA 有两份产物（当前 + 历史），capB 有一份；
//   - 三条部署：A 的当前产物（applied）、B 的产物（pending）、A 的**历史**产物（applied）。
//
// 为什么要有「历史 digest」那一条：迁移必须逐行照抄 artifact 的 (capability_id, digest)，
// 而不是把 digest 换成能力行当前的 content_hash——那会让一次「部署了旧版本」的历史
// 事实凭空变成「部署了当前版本」，而且抹掉以后无法恢复。
type mig10Legacy struct {
	nsID, node1, node2     string
	capA, capB             string
	artACurrent, artAOld   string
	artB                   string
	digestACurrent         string
	digestAOld             string
	digestB                string
	deployIDs              []string
	deploymentArtifactByID map[string]string // deployment id → artifact id
	artifactCapability     map[string]string // artifact id → capability id
	artifactDigest         map[string]string // artifact id → digest
}

// seedMig10Legacy 用**老库真实的书写方式**（裸 INSERT）造数据。
func seedMig10Legacy(t *testing.T, v9 *DB) mig10Legacy {
	t.Helper()
	f := mig10Legacy{
		nsID:                   newRawNamespace(t, v9),
		node1:                  "nd_legacy_1",
		node2:                  "nd_legacy_2",
		capA:                   "cap_legacy_a",
		capB:                   "cap_legacy_b",
		artACurrent:            "af_legacy_a_cur",
		artAOld:                "af_legacy_a_old",
		artB:                   "af_legacy_b",
		digestACurrent:         "sha256:" + strings.Repeat("a1", 32),
		digestAOld:             "sha256:" + strings.Repeat("a0", 32),
		digestB:                "sha256:" + strings.Repeat("b1", 32),
		deploymentArtifactByID: map[string]string{},
		artifactCapability:     map[string]string{},
		artifactDigest:         map[string]string{},
	}
	for _, id := range []string{f.node1, f.node2} {
		if _, err := v9.Exec(
			`INSERT INTO nodes (id, namespace_id, name, token_hash, created_at) VALUES (?,?,?,?,?)`,
			id, f.nsID, id, "hash-"+id, Now()); err != nil {
			t.Fatalf("insert legacy node %s: %v", id, err)
		}
	}
	for i, c := range []struct{ id, name string }{{f.capA, "legacy.a"}, {f.capB, "legacy.b"}} {
		// v9 的样子：有 spec 列（migration 9），没有 balance（migration 10 删了）。
		// content_hash 各给一个（UNIQUE(namespace_id, content_hash) 在 v9 就已生效）。
		if _, err := v9.Exec(
			`INSERT INTO capability_versions
			   (id, namespace_id, name, display_name, version, params_schema, outputs,
			    lifecycle, sticky, source, content_hash, created_at, spec)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			c.id, f.nsID, c.name, "", 1, `{"type":"object"}`, `["done"]`,
			`{}`, "", CapSourceCustom, strings.Repeat("f0", 30)+strconv.Itoa(i)+"0", Now(),
			// 正文写成**脚本形式**：迁移后的补推路径（DeploymentsForNode）只认有 run 的
			// 能力（handler 形式与纯定义都不可部署），夹具必须长得像真库里的行。
			`{"outputs":["done"],"run":{"entrypoint":"run.sh","files":{"run.sh":"#!/bin/bash\nexit 0\n"}}}`); err != nil {
			t.Fatalf("insert legacy capability %s: %v", c.id, err)
		}
	}
	artifacts := []struct{ id, capID, digest string }{
		{f.artACurrent, f.capA, f.digestACurrent},
		{f.artAOld, f.capA, f.digestAOld},
		{f.artB, f.capB, f.digestB},
	}
	for _, a := range artifacts {
		if _, err := v9.Exec(
			`INSERT INTO artifacts (id, namespace_id, capability_id, digest, spec, created_at)
			 VALUES (?,?,?,?,?,?)`,
			a.id, f.nsID, a.capID, a.digest, `{"apiVersion":"anotify/v1","kind":"Capability"}`, Now()); err != nil {
			t.Fatalf("insert legacy artifact %s: %v", a.id, err)
		}
		f.artifactCapability[a.id] = a.capID
		f.artifactDigest[a.id] = a.digest
	}

	deployments := []struct {
		id, node, artifactID, state string
		createdAt                   int64
	}{
		{"dp_legacy_1", f.node1, f.artACurrent, DeploymentApplied, 1000},
		{"dp_legacy_2", f.node1, f.artB, DeploymentPending, 2000},
		{"dp_legacy_3", f.node2, f.artAOld, DeploymentApplied, 3000},
	}
	for _, d := range deployments {
		if _, err := v9.Exec(
			`INSERT INTO deployments (id, namespace_id, node_id, artifact_id, digest, state, reason, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			d.id, f.nsID, d.node, d.artifactID, f.artifactDigest[d.artifactID], d.state, "", d.createdAt, d.createdAt); err != nil {
			t.Fatalf("insert legacy deployment %s: %v", d.id, err)
		}
		f.deployIDs = append(f.deployIDs, d.id)
		f.deploymentArtifactByID[d.id] = d.artifactID
	}
	return f
}

// TestMigration11DeploymentsReferenceCapability 是 P2a 的**迁移正确性证明**。
//
// 造一个停在 migration 10 的库（能力行 + 产物行 + 部署行，含 applied/pending 两种 state，
// 且其中一条部署指向**历史** digest），跑到 10，断言：
//
//	① deployments 行数不变（一条都没丢）；
//	② 每行的 (capability_id, digest) 与原 artifact_id 指向的产物逐字一致（逐行对照）；
//	③ artifacts 表不存在了（镜像表拆掉）；
//	④ 外键自洽（PRAGMA foreign_key_check 干净）+ 两个新索引都在；
//	⑤ 重跑迁移不炸、也不改任何一行（幂等）。
func TestMigration11DeploymentsReferenceCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")

	v9, err := openWith(path, func(set *migrationSet) { *set = (*set)[:9] })
	if err != nil {
		t.Fatalf("open v9 db: %v", err)
	}
	if got, verr := v9.SchemaVersion(); verr != nil || got != 9 {
		t.Fatalf("v9 schema version = %d (err %v), want 9", got, verr)
	}
	f := seedMig10Legacy(t, v9)
	if err := v9.Close(); err != nil {
		t.Fatalf("close v9: %v", err)
	}

	// 用本版本代码重开 → migration 11 的 hook 重建 deployments 并删掉 artifacts。
	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (migrate) db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got, verr := db.SchemaVersion(); verr != nil || got != headVersion {
		t.Fatalf("升级后 schema version = %d (err %v), want %d", got, verr, headVersion)
	}

	// ① 行数不变
	if n := countRows(t, db, "deployments", "namespace_id = ?", f.nsID); n != len(f.deployIDs) {
		t.Fatalf("deployments 行数 = %d, want %d（迁移丢行了）", n, len(f.deployIDs))
	}

	// ② 逐行核对映射：capability_id 来自 artifacts.capability_id，digest 原样照抄
	for _, id := range f.deployIDs {
		artifactID := f.deploymentArtifactByID[id]
		var capID, digest, state string
		if err := db.QueryRow(
			`SELECT capability_id, digest, state FROM deployments WHERE id = ?`, id).
			Scan(&capID, &digest, &state); err != nil {
			t.Fatalf("read migrated deployment %s: %v", id, err)
		}
		if want := f.artifactCapability[artifactID]; capID != want {
			t.Fatalf("%s 的 capability_id = %q, want %q（= 原 artifact %s 的能力）", id, capID, want, artifactID)
		}
		if want := f.artifactDigest[artifactID]; digest != want {
			t.Fatalf("%s 的 digest = %q, want %q（逐字照抄，不能换成能力行当前值）", id, digest, want)
		}
		if state == "" {
			t.Fatalf("%s 的 state 丢了", id)
		}
	}
	// 历史 digest 那一条必须**还在**且仍是历史值（不被“修正”成当前内容）
	var histCap, histDigest string
	if err := db.QueryRow(
		`SELECT capability_id, digest FROM deployments WHERE id = ?`, "dp_legacy_3").
		Scan(&histCap, &histDigest); err != nil {
		t.Fatalf("read historical deployment: %v", err)
	}
	if histCap != f.capA || histDigest != f.digestAOld {
		t.Fatalf("历史部署被改写成 %s/%s，want %s/%s", histCap, histDigest, f.capA, f.digestAOld)
	}
	// 旧状态机列不许被动过
	if n := countRows(t, db, "deployments", "state = ?", DeploymentApplied); n != 2 {
		t.Fatalf("applied 行数 = %d, want 2（state 不该被迁移改写）", n)
	}
	if n := countRows(t, db, "deployments", "state = ?", DeploymentPending); n != 1 {
		t.Fatalf("pending 行数 = %d, want 1", n)
	}

	// ③ 镜像表不存在了
	if tableExists(t, db, "artifacts") {
		t.Fatal("artifacts 表还在：migration 11 没生效")
	}
	if columnNames(t, db, "deployments")["artifact_id"] {
		t.Fatal("deployments.artifact_id 还在：外键没改成 capability_id")
	}

	// ④ 外键自洽 + 索引重建
	var fkViolations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&fkViolations); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if fkViolations != 0 {
		t.Fatalf("外键不自洽：%d 条违规（重建表时漏了引用）", fkViolations)
	}
	for _, idx := range []struct{ table, name string }{
		{"deployments", "idx_deploy_node"},
		{"deployments", "idx_deploy_cap"},
	} {
		if n := countIndexRows(t, db, idx.table, idx.name); n != 1 {
			t.Fatalf("索引 %s 有 %d 条，want 1（重建表之后必须补回来）", idx.name, n)
		}
	}
	if n := countIndexRows(t, db, "deployments", "idx_deploy_artifact"); n != 0 {
		t.Fatalf("旧索引 idx_deploy_artifact 还在（%d 条）", n)
	}

	// ⑤ 幂等：hook 直接重跑一次不许炸、也不许改任何行
	before := map[string][2]string{}
	for _, id := range f.deployIDs {
		var capID, digest string
		if err := db.QueryRow(`SELECT capability_id, digest FROM deployments WHERE id = ?`, id).
			Scan(&capID, &digest); err != nil {
			t.Fatalf("read before rerun %s: %v", id, err)
		}
		before[id] = [2]string{capID, digest}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := referenceCapabilityFromDeployments(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("重跑 migration 11 的 hook 炸了（幂等要求）：%v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rerun: %v", err)
	}
	for _, id := range f.deployIDs {
		var capID, digest string
		if err := db.QueryRow(`SELECT capability_id, digest FROM deployments WHERE id = ?`, id).
			Scan(&capID, &digest); err != nil {
			t.Fatalf("read after rerun %s: %v", id, err)
		}
		if before[id] != [2]string{capID, digest} {
			t.Fatalf("%s 重跑后变了: %v → %s/%s", id, before[id], capID, digest)
		}
	}
	if n := countRows(t, db, "deployments", "namespace_id = ?", f.nsID); n != len(f.deployIDs) {
		t.Fatalf("重跑后行数 = %d, want %d", n, len(f.deployIDs))
	}

	// 读路径也真的按能力行工作了：期望 digest 取 content_hash，不再读 digest 列的副本。
	rows, derr := db.DeploymentsForNode(context.Background(), f.nsID, f.node1)
	if derr != nil {
		t.Fatalf("deployments for node: %v", derr)
	}
	if len(rows) != 2 {
		t.Fatalf("node1 的部署行数 = %d, want 2", len(rows))
	}
}

// TestMigration11RefusesToDropUnmappedDeployment 旧行映射不到能力行时**绝不静默丢**。
//
// 判据在 LEFT JOIN + 指名道姓的报错：INNER JOIN 会把这种行悄悄筛掉，而「丢一行部署」
// = 那台机器上的能力从此没有期望，谁都不会发现。
//
// 造这行需要一个**没有外键约束**的连接（正常连接上 artifacts 外键会拦住它）——
// 模拟的是「库被外部工具写坏」这种真实存在的场景。
func TestMigration11RefusesToDropUnmappedDeployment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9orphan.db")

	v9, err := openWith(path, func(set *migrationSet) { *set = (*set)[:9] })
	if err != nil {
		t.Fatalf("open v9 db: %v", err)
	}
	f := seedMig10Legacy(t, v9)

	// 裸连接：DSN 上不带 _pragma=foreign_keys(ON)，于是能写进一条指向不存在产物的行。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO deployments (id, namespace_id, node_id, artifact_id, digest, state, reason, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		"dp_legacy_ghost", f.nsID, f.node1, "af_ghost", "sha256:"+strings.Repeat("ee", 32),
		DeploymentPending, "", Now(), Now()); err != nil {
		raw.Close()
		_ = v9.Close()
		t.Fatalf("insert orphan deployment: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	if err := v9.Close(); err != nil {
		t.Fatalf("close v9: %v", err)
	}

	_, err = Open(path)
	if err == nil {
		t.Fatal("映射不到能力行的部署行应该让迁移失败，而不是被静默丢掉")
	}
	if !strings.Contains(err.Error(), "af_ghost") {
		t.Fatalf("迁移错误必须点名那条行（artifact_id=af_ghost），got: %v", err)
	}
}
