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
	// sessions.device_name：登录设备名（UA 推断，如「Chrome · macOS」）
	_, _ = db.Exec(`ALTER TABLE sessions ADD COLUMN device_name TEXT NOT NULL DEFAULT ''`)
	// users.role：角色 admin | member（首个注册用户自动 admin）
	_, _ = db.Exec(`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member'`)
	// users.disabled：超管禁用某用户（1=禁用，禁止登录）
	_, _ = db.Exec(`ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`)
	// idx_users_role：必须在 ALTER 加 role 列之后创建（老库迁移顺序），幂等。
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_users_role ON users(role)`)
	// messages 新列：agent_state/severity（接收端能力模型改造）
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN agent_state TEXT NOT NULL DEFAULT 'working'`)
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN severity TEXT NOT NULL DEFAULT ''`)
	// devices 新列：event_scope 替代旧 status_filter（push 设备默认 final）
	_, _ = db.Exec(`ALTER TABLE devices ADD COLUMN event_scope TEXT NOT NULL DEFAULT 'final'`)

	// messages 旧列清理：kind/reply_to 列在删除回复能力后不再需要。
	// SQLite 不支持 ALTER TABLE DROP COLUMN，需用标准重建表法丢弃这两列。
	if err := migrateMessagesDropKindReply(db); err != nil {
		return err
	}
	return nil
}

// Now 返回当前 unixepoch 秒（供各写入点统一使用）。
func Now() int64 { return time.Now().Unix() }

// migrateMessagesDropKindReply 重建 messages 表去掉 kind 和 reply_to 列（幂等）。
//
// 旧库 messages 表有 kind/reply_to 列（回复能力用），删除回复能力后不再需要。
// SQLite 不支持 ALTER TABLE DROP COLUMN，需用标准重建表法：
//
//	1. 检测 messages 表是否有 reply_to 列（无则 no-op）
//	2. PRAGMA foreign_keys=OFF（事务外执行，防止 DROP TABLE 触发级联删 deliveries）
//	3. BEGIN TRANSACTION
//	4. CREATE TABLE messages_new （按 schema.sql 定义，无 kind/reply_to）
//	5. INSERT INTO messages_new SELECT ... FROM messages（复制数据，丢弃 kind/reply_to）
//	6. DROP TABLE messages
//	7. ALTER TABLE messages_new RENAME TO messages
//	8. 重建索引
//	9. COMMIT
//	10. PRAGMA foreign_keys=ON（恢复）
func migrateMessagesDropKindReply(db *sql.DB) error {
	// 检测 messages 表是否有 reply_to 列（kind 和 reply_to 总是一起加的，检测一个即可）
	var colCount int
	err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('messages') WHERE name='reply_to'`).Scan(&colCount)
	if err != nil {
		return fmt.Errorf("check messages.reply_to column: %w", err)
	}
	if colCount == 0 {
		return nil // 新库或已迁移，no-op
	}

	// 外键安全：Open 的 DSN 设了 foreign_keys=ON，DROP TABLE messages 会触发
	// deliveries.message_id REFERENCES messages(id) ON DELETE CASCADE，
	// 静默清空 deliveries 全表历史投递记录。
	// PRAGMA foreign_keys 在事务内是 no-op，必须在 BEGIN 之前执行。
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign_keys for migration: %w", err)
	}
	defer func() {
		_, _ = db.Exec(`PRAGMA foreign_keys=ON`)
	}()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migrate messages drop kind/reply_to: %w", err)
	}
	defer tx.Rollback()

	// 建新表（与 schema.sql 的 messages 定义一致，无 kind/reply_to 列）
	if _, err := tx.Exec(`CREATE TABLE messages_new (
		id          TEXT PRIMARY KEY,
		user_id     TEXT NOT NULL,
		seq         INTEGER NOT NULL,
		title       TEXT NOT NULL,
		agent_state TEXT NOT NULL DEFAULT 'working',
		severity    TEXT NOT NULL DEFAULT '',
		body        TEXT NOT NULL DEFAULT '',
		link        TEXT NOT NULL DEFAULT '',
		device_tags TEXT NOT NULL DEFAULT '[]',
		priority    TEXT NOT NULL DEFAULT 'normal',
		ttl_seconds INTEGER NOT NULL DEFAULT 86400,
		payload     TEXT NOT NULL,
		created_at  INTEGER NOT NULL,
		expires_at  INTEGER NOT NULL,
		UNIQUE (user_id, seq)
	)`); err != nil {
		return fmt.Errorf("create messages_new: %w", err)
	}

	// 复制数据（丢弃 kind/reply_to 列）
	if _, err := tx.Exec(`INSERT INTO messages_new
		(id, user_id, seq, title, agent_state, severity, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at)
		SELECT id, user_id, seq, title, agent_state, severity, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at
		FROM messages`); err != nil {
		return fmt.Errorf("copy messages data: %w", err)
	}

	// 删旧表、重命名
	if _, err := tx.Exec(`DROP TABLE messages`); err != nil {
		return fmt.Errorf("drop old messages: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE messages_new RENAME TO messages`); err != nil {
		return fmt.Errorf("rename messages_new: %w", err)
	}

	// 重建索引
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_user_seq ON messages(user_id, seq)`); err != nil {
		return fmt.Errorf("recreate idx_messages_user_seq: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_user_created ON messages(user_id, created_at)`); err != nil {
		return fmt.Errorf("recreate idx_messages_user_created: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_expires ON messages(expires_at)`); err != nil {
		return fmt.Errorf("recreate idx_messages_expires: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrate messages drop kind/reply_to: %w", err)
	}
	return nil
}
