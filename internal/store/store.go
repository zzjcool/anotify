// Package store 提供 SQLite 数据访问层。
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// DB 包装 *sql.DB。
type DB struct {
	*sql.DB
}

// Open 打开（并迁移）一个 SQLite 数据库。path 可为 ":memory:"。
func Open(path string) (*DB, error) {
	// modernc.org/sqlite 驱动名 "sqlite"
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + path
	}
	dsn += "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite 单写：限制连接数避免 "database is locked"
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// 幂等列迁移：CREATE TABLE IF NOT EXISTS 不会给已存在的表加新列，
	// 需在此显式补（SQLite ALTER TABLE ADD COLUMN 幂等性靠 try/ignore）。
	if err := migrateColumns(db); err != nil {
		return nil, fmt.Errorf("migrate columns: %w", err)
	}
	return &DB{db}, nil
}

// migrateColumns 幂等补充新列（老库升级用）。重复执行不报错。
func migrateColumns(db *sql.DB) error {
	// passkeys.backup_eligible（BackupEligible flag 持久化）
	_, _ = db.Exec(`ALTER TABLE passkeys ADD COLUMN backup_eligible INTEGER NOT NULL DEFAULT 0`)
	// cli_auth_sessions.kind：区分 apikey / passkey 授权类型（老行默认 apikey）
	_, _ = db.Exec(`ALTER TABLE cli_auth_sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'apikey'`)
	// cli_auth_sessions.device_hint：Passkey 授权时新设备敲门自报的设备信息（apikey 流程不用）
	_, _ = db.Exec(`ALTER TABLE cli_auth_sessions ADD COLUMN device_hint TEXT NOT NULL DEFAULT ''`)
	return nil
}

// Now 返回当前 unixepoch 秒（供各写入点统一使用）。
func Now() int64 { return time.Now().Unix() }
