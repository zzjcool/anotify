package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// CLI 授权会话状态常量。
const (
	CliAuthPending  = "pending"
	CliAuthApproved = "approved"
	CliAuthConsumed = "consumed"
	CliAuthDenied   = "denied"
	CliAuthExpired  = "expired"
)

// CliAuthSession 是 cli_auth_sessions 表的一行。
type CliAuthSession struct {
	ID              string
	SecretHash      string
	UserCode        string
	DeviceName      string
	ScopesRequested []string
	ScopesGranted   []string // nil 表示未批准
	UserID          string   // 空串表示未批准
	KeyID           string   // 空串表示未领证
	Status          string
	CreatedAt       int64
	ExpiresAt       int64
	ConsumedAt      sql.NullInt64
}

// CreateCliAuthSession 插入一个授权会话。
func (d *DB) CreateCliAuthSession(s *CliAuthSession) error {
	sr, err := json.Marshal(s.ScopesRequested)
	if err != nil {
		return fmt.Errorf("marshal scopes_requested: %w", err)
	}
	var sg any
	if s.ScopesGranted != nil {
		b, err := json.Marshal(s.ScopesGranted)
		if err != nil {
			return fmt.Errorf("marshal scopes_granted: %w", err)
		}
		sg = string(b)
	}
	var userID any
	if s.UserID != "" {
		userID = s.UserID
	}
	var keyID any
	if s.KeyID != "" {
		keyID = s.KeyID
	}
	_, err = d.Exec(
		`INSERT INTO cli_auth_sessions
		   (id, secret_hash, user_code, device_name, scopes_requested, scopes_granted,
		    status, user_id, key_id, created_at, expires_at, consumed_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.SecretHash, s.UserCode, s.DeviceName, string(sr), sg,
		s.Status, userID, keyID, s.CreatedAt, s.ExpiresAt, s.ConsumedAt,
	)
	if err != nil {
		return fmt.Errorf("create cli_auth_session: %w", err)
	}
	return nil
}

// GetCliAuthSession 按 ID 查授权会话。
func (d *DB) GetCliAuthSession(id string) (*CliAuthSession, error) {
	var s CliAuthSession
	var sr, sg sql.NullString
	var userID, keyID sql.NullString
	err := d.QueryRow(
		`SELECT id, secret_hash, user_code, device_name, scopes_requested, scopes_granted,
		        status, user_id, key_id, created_at, expires_at, consumed_at
		 FROM cli_auth_sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.SecretHash, &s.UserCode, &s.DeviceName, &sr, &sg,
		&s.Status, &userID, &keyID, &s.CreatedAt, &s.ExpiresAt, &s.ConsumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get cli_auth_session: %w", err)
	}
	s.ScopesRequested = decodeScopes(sr.String)
	s.ScopesGranted = decodeScopesNullable(sg.String)
	s.UserID = userID.String
	s.KeyID = keyID.String
	return &s, nil
}

// GetCliAuthSessionByCode 按短码查授权会话（code 已归一化：大写无连字符）。
func (d *DB) GetCliAuthSessionByCode(code string) (*CliAuthSession, error) {
	var s CliAuthSession
	var sr, sg sql.NullString
	var userID, keyID sql.NullString
	err := d.QueryRow(
		`SELECT id, secret_hash, user_code, device_name, scopes_requested, scopes_granted,
		        status, user_id, key_id, created_at, expires_at, consumed_at
		 FROM cli_auth_sessions WHERE user_code = ?`, code,
	).Scan(&s.ID, &s.SecretHash, &s.UserCode, &s.DeviceName, &sr, &sg,
		&s.Status, &userID, &keyID, &s.CreatedAt, &s.ExpiresAt, &s.ConsumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get cli_auth_session by code: %w", err)
	}
	s.ScopesRequested = decodeScopes(sr.String)
	s.ScopesGranted = decodeScopesNullable(sg.String)
	s.UserID = userID.String
	s.KeyID = keyID.String
	return &s, nil
}

// UpdateCliAuthSessionApproved 原子迁移 pending→approved。
// grantedScopes 存为 JSON 数组。返回 ErrAlreadyTerminal 表示会话不在 pending 状态。
func (d *DB) UpdateCliAuthSessionApproved(id, userID string, grantedScopes []string, now int64) (bool, error) {
	sg, err := json.Marshal(grantedScopes)
	if err != nil {
		return false, fmt.Errorf("marshal scopes_granted: %w", err)
	}
	res, err := d.Exec(
		`UPDATE cli_auth_sessions
		   SET status = ?, user_id = ?, scopes_granted = ?
		 WHERE id = ? AND status = ?`,
		CliAuthApproved, userID, string(sg), id, CliAuthPending,
	)
	if err != nil {
		return false, fmt.Errorf("update cli_auth_session approved: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// UpdateCliAuthSessionDenied 原子迁移 pending→denied。
func (d *DB) UpdateCliAuthSessionDenied(id string) (bool, error) {
	res, err := d.Exec(
		`UPDATE cli_auth_sessions SET status = ? WHERE id = ? AND status = ?`,
		CliAuthDenied, id, CliAuthPending,
	)
	if err != nil {
		return false, fmt.Errorf("update cli_auth_session denied: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// MarkCliAuthSessionExpired 将 pending 或 approved 状态的会话标记为 expired。
// 用于惰性过期迁移（读路径上发现已过时时调用）。
func (d *DB) MarkCliAuthSessionExpired(id string) error {
	_, err := d.Exec(
		`UPDATE cli_auth_sessions SET status = ? WHERE id = ? AND status IN (?, ?)`,
		CliAuthExpired, id, CliAuthPending, CliAuthApproved,
	)
	if err != nil {
		return fmt.Errorf("mark cli_auth_session expired: %w", err)
	}
	return nil
}

// ConsumeCliAuthSessionAndCreateKey 单事务：approved→consumed + 插入 api_keys 行。
// 0 行受影响表示会话已被消费（返回 alreadyConsumed=true）。
// 明文 Key 任何表都不落。
func (d *DB) ConsumeCliAuthSessionAndCreateKey(sessionID string, key *APIKey, consumedAt int64) (alreadyConsumed bool, err error) {
	tx, err := d.Begin()
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`UPDATE cli_auth_sessions
		   SET status = ?, consumed_at = ?, key_id = ?
		 WHERE id = ? AND status = ?`,
		CliAuthConsumed, consumedAt, key.ID, sessionID, CliAuthApproved,
	)
	if err != nil {
		return false, fmt.Errorf("update consumed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return true, nil // 已被消费
	}

	sc, err := json.Marshal(key.Scopes)
	if err != nil {
		return false, fmt.Errorf("marshal scopes: %w", err)
	}
	enabled := 0
	if key.Enabled {
		enabled = 1
	}
	_, err = tx.Exec(
		`INSERT INTO api_keys (id, user_id, name, key_hash, prefix, scopes, enabled, created_at, last_used_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		key.ID, key.UserID, key.Name, key.KeyHash, key.Prefix, string(sc), enabled, key.CreatedAt, nullableInt64(key.LastUsedAt),
	)
	if err != nil {
		return false, fmt.Errorf("create api key in tx: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return false, nil
}

// DeleteExpiredCliAuthSessions 惰性清理过期会话（删除 expiresAt < before 的记录）。
func (d *DB) DeleteExpiredCliAuthSessions(before int64) error {
	_, err := d.Exec(`DELETE FROM cli_auth_sessions WHERE expires_at < ?`, before)
	if err != nil {
		return fmt.Errorf("delete expired cli_auth_sessions: %w", err)
	}
	return nil
}

// decodeScopes 解析 JSON scope 数组，失败返回空切片（而非 nil）。
func decodeScopes(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []string{}
	}
	return out
}

// decodeScopesNullable 解析可能为空的 JSON scope 数组，空串返回 nil。
func decodeScopesNullable(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
