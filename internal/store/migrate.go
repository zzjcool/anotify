package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersionTable 记录已应用的迁移版本。
//
// 每个迁移是编号版本：已记账则跳过，未记账则在一个事务里执行 + 记账，
// 任何一步失败都返回错误（不吞错）。
const schemaVersionTable = `CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`

// baselineVersion 是 v4 baseline 的版本号（schema.sql 唯一的一块）。
const baselineVersion = 1

// headVersion 是本版本代码认识的**最高**迁移版本（= schema.sql 的最后一块）。
//
// v4 开发期只有一块 baseline：schema_version 里出现任何 >1 的版本，
// 都说明这个数据目录是 v3（或更早）留下的，结构完全不同 → 明确拒绝
// （见 legacyVersionError）。每追加一块迁移都要同步 +1。
const headVersion = 1

// legacyBusinessTables 是「没有 schema_version 但已经有业务表」的判定清单。
//
// 这种库只可能来自 v3 或更早：v4 的 Open 一定先建 schema_version 再建业务表，
// 所以「业务表在、记账表不在」= 旧库。命中任何一张就拒绝，绝不尝试兼容。
var legacyBusinessTables = []string{
	"users",
	"passkeys",
	"sessions",
	"api_keys",
	"devices",
	"namespaces",
	"capability_versions",
	"nodes",
	"tasks",
	"events",
	"edges",
	"sources",
	"playbooks",
	"deployments",
}

// migrationHeader 匹配分块注释，例如：`-- +migration 2 add_column`
var migrationHeader = regexp.MustCompile(`^--\s*\+migration\s+(\d+)\s+(\w+)\s*$`)

// migration 是 schema.sql 里的一块（一个编号版本）。
type migration struct {
	version int
	name    string
	body    string // 去掉分块头之后的 SQL；不含任何 statement
}

// migrations 是解析 schema.sql 得到的迁移列表（按 version 升序）。
type migrationSet []migration

// parseMigrations 把 schema.sql 按 `-- +migration N name` 切块。
//
// 约束（parseMigrations 会强制检查）：
//   - 版本号从 1 开始连续递增，不跳号、不重复（漏一个编号会导致「以为迁移过」）
//   - 每块必须有非空的名字
func parseMigrations(src string) (migrationSet, error) {
	var out migrationSet
	var cur *migration
	var buf strings.Builder
	flush := func() {
		if cur == nil {
			return
		}
		cur.body = strings.TrimSpace(buf.String())
		out = append(out, *cur)
		buf.Reset()
	}
	for _, line := range strings.Split(src, "\n") {
		if m := migrationHeader.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			flush()
			v, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, fmt.Errorf("parse migration version %q: %w", m[1], err)
			}
			cur = &migration{version: v, name: m[2]}
			continue
		}
		if cur == nil {
			continue // 文件头的说明文字
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()

	for i, mg := range out {
		if want := i + 1; mg.version != want {
			return nil, fmt.Errorf("migration sequence broken: at position %d got version %d (want %d)", i, mg.version, want)
		}
	}
	return out, nil
}

// migrate 应用所有未记账的迁移，并返回本次实际应用的版本数。
//
// modify（测试注入用）在校验完版本序列后、执行前被调用，可修改待执行的迁移
// ——用于断言「迁移失败必须回滚且不记账」。
//
// 第三个参数是「某版本需要用 Go 逻辑实现」的钩子表：SQL 表达不了的变更
// （条件 ALTER TABLE、数据搬迁、表重建）写在钩子里，SQL 块留空。
// 约定：hook 返回 error 时整个迁移回滚；hook 必须**幂等**。
//
// v4 baseline 全是 DDL，没有钩子；机制保留给后续 `-- +migration 2` 起的新块。
func migrate(db *sql.DB, modify func(*migrationSet)) (int, error) {
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		return 0, fmt.Errorf("parse schema.sql: %w", err)
	}
	if modify != nil {
		modify(&set)
	}
	return migrateWith(db, set, migrationHooks)
}

// migrationHooks 是「SQL 表达不了」的变更（数据搬迁）的钩子表。
//
// 约定见 migrateWith：hook 返回 error 时整个迁移回滚；hook 必须**幂等**。
// v4 baseline 不需要钩子（它建的是一个空库的全部结构，没有要搬的数据）。
var migrationHooks = map[int]func(*sql.Tx) error{}

// migrateWith 执行迁移：每个版本一个事务，成功才记账。
func migrateWith(db *sql.DB, set migrationSet, hooks map[int]func(*sql.Tx) error) (applied int, err error) {
	// 旧库判定必须在创建 schema_version **之前**：没有记账表但有业务表，
	// 本身就是「这是旧库」的证据；先建记账表会把这条证据抹掉。
	if err := legacyDatabaseError(db); err != nil {
		return 0, err
	}
	if _, err := db.Exec(schemaVersionTable); err != nil {
		return 0, fmt.Errorf("create schema_version: %w", err)
	}
	done, err := appliedVersions(db)
	if err != nil {
		return 0, err
	}
	if err := legacyVersionError(done); err != nil {
		return 0, err
	}
	for _, m := range set {
		if done[m.version] {
			continue
		}
		if err := applyMigration(db, m, hooks[m.version]); err != nil {
			return applied, fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		applied++
	}
	return applied, nil
}

// legacyDatabaseError 拦住「没有 schema_version、却已经有业务表」的旧库。
//
// 判据是 legacyBusinessTables 里的任何一张表存在。v4 的 Open 永远先建
// schema_version，所以这种库不可能是 v4 建的——它只可能来自 v3 或更早。
//
// 为什么必须**明说**而不是尽力兼容：v4 baseline 是全新结构（列、表、语义都变了），
// 升级路径根本不存在。就这么打开的话，缺列/缺表会在第一次查询时以
// `no such column` / `no such table` 炸开，而且是在业务路径上炸——半死不活，
// 还看不出根因。
//
// 为什么**不**静默删数据重建：这是用户的数据目录。删不删由人决定，程序只负责说清。
func legacyDatabaseError(db *sql.DB) error {
	hasVersion, err := tableExistsDB(db, "schema_version")
	if err != nil {
		return fmt.Errorf("probe schema_version: %w", err)
	}
	if hasVersion {
		return nil
	}
	var found []string
	for _, name := range legacyBusinessTables {
		ok, err := tableExistsDB(db, name)
		if err != nil {
			return fmt.Errorf("probe table %s: %w", name, err)
		}
		if ok {
			found = append(found, name)
		}
	}
	if len(found) == 0 {
		return nil // 空库：正常新建
	}
	return fmt.Errorf(
		"store: 数据目录里已有旧版业务表（%s）但没有 schema_version；"+
			"v4 不支持旧库升级，请删除数据目录后重启（数据可先用 cmd/devseed 重建）。程序不会自动删除数据",
		strings.Join(found, ", "))
}

// legacyVersionError 拦住「squash 之前 / v3 建的库」。
//
// 判据：schema_version 里出现过比 headVersion（v4 = 1）更高的版本。
// v3 的库记着 1..11，v4 只认 1，所以「高了」一定是旧库。
func legacyVersionError(done map[int]bool) error {
	maxDone := 0
	for v := range done {
		if v > maxDone {
			maxDone = v
		}
	}
	if maxDone <= headVersion {
		return nil
	}
	return fmt.Errorf(
		"store: 这个数据目录是 v4 之前的版本（schema_version=%d）；v4 只支持全新 baseline，"+
			"不支持旧库升级，请删除数据目录后重启（数据可先用 cmd/devseed 重建）。程序不会自动删除数据",
		maxDone)
}

// tableExistsDB 判断表是否存在（旧库判定用；迁移自己的探测走 tableExistsTx）。
func tableExistsDB(db *sql.DB, name string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// applyMigration 在单个事务内执行一个迁移并记账。
func applyMigration(db *sql.DB, m migration, hook func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if m.body != "" {
		if _, err := tx.Exec(m.body); err != nil {
			return fmt.Errorf("exec sql: %w", err)
		}
	}
	if hook != nil {
		if err := hook(tx); err != nil {
			return fmt.Errorf("hook: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, m.version, Now(),
	); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// appliedVersions 读出已记账的版本。
func appliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_version`)
	if err != nil {
		return nil, fmt.Errorf("query schema_version: %w", err)
	}
	defer rows.Close()
	done := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_version: %w", err)
		}
		done[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_version: %w", err)
	}
	return done, nil
}

// SchemaVersion 返回当前库的最高已应用版本（0 = 空库）。供健康检查/测试使用。
func (d *DB) SchemaVersion() (int, error) {
	var v sql.NullInt64
	if err := d.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}
