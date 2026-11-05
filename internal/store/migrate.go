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
// 为什么要有它：旧实现用一串 `_, _ = db.Exec("ALTER TABLE ...")` 补列，
// 所有错误被静默吞掉——真实迁移失败会被当成「已经迁移过」，库就坏在无声处。
// 现在每个迁移是编号版本：已记账则跳过，未记账则在一个事务里执行 + 记账，
// 任何一步失败都返回错误（不吞错）。
const schemaVersionTable = `CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`

// baselineVersion 是 squash 出来的那块终态 DDL 的版本号。
//
// schema.sql 从 `-- +migration 1 baseline`（squash 后的终态 DDL）开始，
// 往后的新变更按 `-- +migration 2 …` 追加块（见本文件头的说明）。
// baseline 本身**不再改动**：改它等于让「已发布的库」与「新建的库」分叉。
const baselineVersion = 1

// headVersion 是本版本代码认识的**最高**迁移版本（= schema.sql 的最后一块）。
//
// 它只用来识别「squash 之前建的库」：那种库的 schema_version 里记着 v2..v28，
// 比本代码认识的最高版本还高，且结构与本仓库的终态 DDL 完全不同 → 明确拒绝
// （见 legacyVersionError）。每追加一块迁移都要同步 +1，否则新建的库会被自己拒绝。
const headVersion = 3

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
// 第三个返回参数是「某版本需要用 Go 逻辑实现」的钩子表：SQL 表达不了的变更
// （条件 ALTER TABLE——SQLite 没有 ADD COLUMN IF NOT EXISTS、数据搬迁、表重建）
// 写在钩子里，SQL 块留空。约定：hook 返回 error 时整个迁移回滚；hook 必须**幂等**
// ——同一版本在同一个事务内可能因重试被执行多次。
//
// baseline 全是 DDL，没有钩子，所以这里传 nil。机制本身是保留的：从 `-- +migration 2`
// 往后追加块时，需要 Go 逻辑就把它加进 map。
func migrate(db *sql.DB, modify func(*migrationSet)) (int, error) {
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		return 0, fmt.Errorf("parse schema.sql: %w", err)
	}
	if modify != nil {
		modify(&set)
	}
	return migrateWith(db, set, nil)
}

// migrateWith 执行迁移：每个版本一个事务，成功才记账。
func migrateWith(db *sql.DB, set migrationSet, hooks map[int]func(*sql.Tx) error) (applied int, err error) {
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

// legacyVersionError 拦住「squash 之前建的库」。
//
// 判据：schema_version 里出现过比 baseline 更高的版本——baseline 只记 1，所以高了
// 一定是老库（v2..v28）。
//
// 为什么必须**明说**而不是尽力兼容：squash 把 28 块增量迁移合并成了一份终态 DDL，
// 升级路径（逐块 ALTER / 重建表 / 数据搬迁）已经整条删掉。老库就这么打开的话，
// 缺列会在第一次查询时以 `no such column` 炸开，而且是在业务路径上炸——半死不活，
// 还看不出根因。这里直接给出可操作的一句话。
//
// 为什么**不**静默删数据重建：这是用户的数据目录。删不删由人决定，程序只负责说清
// 「删掉就能起来，要留的数据先用 cmd/devseed 之类的工具重放」。
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
		"store: 这个库是 squash 之前的版本（v%d）；本版本不再支持增量升级，请删除数据目录后重启（数据可先用 cmd/devseed 重建）",
		maxDone)
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
