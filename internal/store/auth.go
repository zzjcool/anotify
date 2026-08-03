package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("store: record not found")

// User 是 users 表的一行。
type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	CreatedAt   int64  `json:"createdAt"`
}

// Passkey 是 passkeys 表的一行（public_key 为原始字节）。
type Passkey struct {
	ID         string // credential id（base64url 字符串，作表主键）
	UserID     string
	PublicKey  []byte
	SignCount  int64
	Name       string
	Transports []string
	CreatedAt  int64
	LastUsedAt sql.NullInt64
}

// Session 是 sessions 表的一行。
type Session struct {
	ID        string `json:"id"`
	UserID    string `json:"userId"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	LastSeen  int64  `json:"lastSeen"`
}

// APIKey 是 api_keys 表的一行（不含明文）。
type APIKey struct {
	ID         string
	UserID     string
	Name       string
	KeyHash    string
	Prefix     string
	Scopes     []string
	Enabled    bool
	CreatedAt  int64
	LastUsedAt sql.NullInt64
}

// ---------- User ----------

// CreateUser 插入一个用户。
func (d *DB) CreateUser(u *User) error {
	_, err := d.Exec(
		`INSERT INTO users (id, username, display_name, created_at) VALUES (?,?,?,?)`,
		u.ID, u.Username, u.DisplayName, u.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// GetUserByUsername 按用户名查用户。
func (d *DB) GetUserByUsername(username string) (*User, error) {
	var u User
	err := d.QueryRow(
		`SELECT id, username, display_name, created_at FROM users WHERE username = ?`, username,
	).Scan(&u.ID, &u.Username, &u.DisplayName, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	return &u, nil
}

// GetUserByID 按 ID 查用户。
func (d *DB) GetUserByID(id string) (*User, error) {
	var u User
	err := d.QueryRow(
		`SELECT id, username, display_name, created_at FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Username, &u.DisplayName, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return &u, nil
}

// ---------- Passkey ----------

// CreatePasskey 插入一个 Passkey 凭证。
func (d *DB) CreatePasskey(p *Passkey) error {
	tr, err := json.Marshal(p.Transports)
	if err != nil {
		return fmt.Errorf("marshal transports: %w", err)
	}
	_, err = d.Exec(
		`INSERT INTO passkeys (id, user_id, public_key, sign_count, name, transports, created_at, last_used_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		p.ID, p.UserID, p.PublicKey, p.SignCount, p.Name, string(tr), p.CreatedAt, nullableInt64(p.LastUsedAt),
	)
	if err != nil {
		return fmt.Errorf("create passkey: %w", err)
	}
	return nil
}

// ListPasskeysByUser 列出某用户的所有凭证。
func (d *DB) ListPasskeysByUser(userID string) ([]*Passkey, error) {
	rows, err := d.Query(
		`SELECT id, user_id, public_key, sign_count, name, transports, created_at, last_used_at
		 FROM passkeys WHERE user_id = ? ORDER BY created_at ASC`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	defer rows.Close()
	var out []*Passkey
	for rows.Next() {
		var p Passkey
		var tr string
		if err := rows.Scan(&p.ID, &p.UserID, &p.PublicKey, &p.SignCount, &p.Name, &tr, &p.CreatedAt, &p.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan passkey: %w", err)
		}
		if err := json.Unmarshal([]byte(tr), &p.Transports); err != nil {
			p.Transports = []string{}
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// GetPasskeyByID 按 credential id 查凭证。
func (d *DB) GetPasskeyByID(id string) (*Passkey, error) {
	var p Passkey
	var tr string
	err := d.QueryRow(
		`SELECT id, user_id, public_key, sign_count, name, transports, created_at, last_used_at
		 FROM passkeys WHERE id = ?`, id,
	).Scan(&p.ID, &p.UserID, &p.PublicKey, &p.SignCount, &p.Name, &tr, &p.CreatedAt, &p.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get passkey: %w", err)
	}
	if err := json.Unmarshal([]byte(tr), &p.Transports); err != nil {
		p.Transports = []string{}
	}
	return &p, nil
}

// UpdatePasskeySignCount 更新签名计数与最近使用时间（防重放）。
func (d *DB) UpdatePasskeySignCount(id string, signCount int64, lastUsedAt int64) error {
	_, err := d.Exec(
		`UPDATE passkeys SET sign_count = ?, last_used_at = ? WHERE id = ?`,
		signCount, lastUsedAt, id,
	)
	if err != nil {
		return fmt.Errorf("update passkey sign count: %w", err)
	}
	return nil
}

// ---------- Session ----------

// CreateSession 插入一个会话。
func (d *DB) CreateSession(s *Session) error {
	_, err := d.Exec(
		`INSERT INTO sessions (id, user_id, created_at, expires_at, last_seen) VALUES (?,?,?,?,?)`,
		s.ID, s.UserID, s.CreatedAt, s.ExpiresAt, s.LastSeen,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession 按 ID 查会话。
func (d *DB) GetSession(id string) (*Session, error) {
	var s Session
	err := d.QueryRow(
		`SELECT id, user_id, created_at, expires_at, last_seen FROM sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.LastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return &s, nil
}

// TouchSession 刷新 last_seen（可选，用于活跃统计）。
func (d *DB) TouchSession(id string, lastSeen int64) error {
	_, err := d.Exec(`UPDATE sessions SET last_seen = ? WHERE id = ?`, lastSeen, id)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// DeleteSession 吊销一个会话。
func (d *DB) DeleteSession(id string) error {
	_, err := d.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// ListSessionsByUser 列出某用户的所有会话。
func (d *DB) ListSessionsByUser(userID string) ([]*Session, error) {
	rows, err := d.Query(
		`SELECT id, user_id, created_at, expires_at, last_seen FROM sessions WHERE user_id = ? ORDER BY created_at DESC`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.UserID, &s.CreatedAt, &s.ExpiresAt, &s.LastSeen); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

// ---------- API Key ----------

// CreateAPIKey 插入一个 API Key（仅存哈希）。
func (d *DB) CreateAPIKey(k *APIKey) error {
	sc, err := json.Marshal(k.Scopes)
	if err != nil {
		return fmt.Errorf("marshal scopes: %w", err)
	}
	enabled := 0
	if k.Enabled {
		enabled = 1
	}
	_, err = d.Exec(
		`INSERT INTO api_keys (id, user_id, name, key_hash, prefix, scopes, enabled, created_at, last_used_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		k.ID, k.UserID, k.Name, k.KeyHash, k.Prefix, string(sc), enabled, k.CreatedAt, nullableInt64(k.LastUsedAt),
	)
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	return nil
}

// GetAPIKeyByPrefix 按前缀查 Key（用于校验时定位记录）。
func (d *DB) GetAPIKeyByPrefix(prefix string) (*APIKey, error) {
	var k APIKey
	var sc string
	var enabled int
	err := d.QueryRow(
		`SELECT id, user_id, name, key_hash, prefix, scopes, enabled, created_at, last_used_at
		 FROM api_keys WHERE prefix = ?`, prefix,
	).Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.Prefix, &sc, &enabled, &k.CreatedAt, &k.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get api key by prefix: %w", err)
	}
	k.Enabled = enabled == 1
	if err := json.Unmarshal([]byte(sc), &k.Scopes); err != nil {
		k.Scopes = []string{}
	}
	return &k, nil
}

// TouchAPIKey 更新最近使用时间。
func (d *DB) TouchAPIKey(id string, lastUsedAt int64) error {
	_, err := d.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, lastUsedAt, id)
	if err != nil {
		return fmt.Errorf("touch api key: %w", err)
	}
	return nil
}

// ListAPIKeysByUser 列出某用户的所有 Key（不含哈希字段对外，但此处返回完整记录供内部使用）。
func (d *DB) ListAPIKeysByUser(userID string) ([]*APIKey, error) {
	rows, err := d.Query(
		`SELECT id, user_id, name, key_hash, prefix, scopes, enabled, created_at, last_used_at
		 FROM api_keys WHERE user_id = ? ORDER BY created_at DESC`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		var k APIKey
		var sc string
		var enabled int
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.Prefix, &sc, &enabled, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		k.Enabled = enabled == 1
		if err := json.Unmarshal([]byte(sc), &k.Scopes); err != nil {
			k.Scopes = []string{}
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

// RevokeAPIKey 停用一个 Key（enabled=0）。
func (d *DB) RevokeAPIKey(id string) error {
	_, err := d.Exec(`UPDATE api_keys SET enabled = 0 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	return nil
}

// nullableInt64 把 sql.NullInt64 转为可存 NULL 的 driver 值。
func nullableInt64(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}
