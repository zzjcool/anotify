package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// sha256Hex 是本文件用的令牌哈希（store 不能 import auth，所以在测试里就地算）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestSessionTokenStoredHashed 钉住 #589 的存储侧不变量：
// sessions 表里没有任何一列保存着 Cookie 令牌原文（改前 id 就是令牌）。
//
// 去掉修复必须红：改前 id == 令牌，本用例的两个 NOT CONTAINS 都会命中。
func TestSessionTokenStoredHashed(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "hashed", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	token := "tok_secret_" + NewSessionID()
	s := &Session{
		ID:         NewSessionID(),
		Token:      token,
		TokenHash:  sha256Hex(token),
		UserID:     u.ID,
		DeviceName: "Test · macOS",
		CreatedAt:  Now(),
		ExpiresAt:  Now() + 3600,
		LastSeen:   Now(),
	}
	if err := db.CreateSession(s); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 整表转储（列名按 PRAGMA 取，加列不会被这条用例漏掉）再找令牌原文。
	cols, err := sessionColumns(db)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	row := db.QueryRow(`SELECT `+strings.Join(cols, ", ")+` FROM sessions WHERE id = ?`, s.ID)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := row.Scan(ptrs...); err != nil {
		t.Fatalf("dump session row: %v", err)
	}
	for i, col := range cols {
		if strings.Contains(fmt.Sprint(vals[i]), token) {
			t.Fatalf("sessions.%s 保存了令牌原文（应只有哈希）: %v", col, vals[i])
		}
	}

	// 按哈希取行：往返一致性（存什么读什么）。
	got, err := db.GetSessionByTokenHash(sha256Hex(token))
	if err != nil {
		t.Fatalf("get session by token hash: %v", err)
	}
	if got.ID != s.ID || got.UserID != u.ID || got.DeviceName != s.DeviceName ||
		got.CreatedAt != s.CreatedAt || got.ExpiresAt != s.ExpiresAt || got.LastSeen != s.LastSeen {
		t.Fatalf("往返不一致:\n got=%+v\nwant=%+v", got, s)
	}
	if got.TokenHash != sha256Hex(token) {
		t.Errorf("token_hash 往返不一致: got %q", got.TokenHash)
	}
	if _, err := db.GetSessionByTokenHash(sha256Hex("tok_wrong")); !errors.Is(err, ErrNotFound) {
		t.Errorf("错的令牌应 ErrNotFound, got %v", err)
	}

	// 公开 id 那条读路径**不带**哈希出库（列表/详情永远拿不到凭据派生值）。
	byID, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("get session by id: %v", err)
	}
	if byID.TokenHash != "" {
		t.Errorf("GetSession 不该把 token_hash 读出来, got %q", byID.TokenHash)
	}

	// 节流写走哈希条件。
	if err := db.TouchSessionByTokenHash(sha256Hex(token), Now()+42); err != nil {
		t.Fatalf("touch by token hash: %v", err)
	}
	touched, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if touched.LastSeen != s.LastSeen+42 {
		t.Errorf("last_seen 未按哈希条件更新: got %d want %d", touched.LastSeen, s.LastSeen+42)
	}

	// 登出路径：按哈希删除。
	if err := db.DeleteSessionByTokenHash(sha256Hex(token)); err != nil {
		t.Fatalf("delete by token hash: %v", err)
	}
	if _, err := db.GetSessionByTokenHash(sha256Hex(token)); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后按哈希应 ErrNotFound, got %v", err)
	}
}

// TestSessionTokenHashColumn 断言迁移块 16 真的加上了 token_hash 与它的索引
// （只改 Go 不改 schema 时，上面那条用例会因为「列不存在」先红在 CreateSession 上）。
func TestSessionTokenHashColumn(t *testing.T) {
	db := newTestDB(t)
	cols, err := sessionColumns(db)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	found := false
	for _, c := range cols {
		if c == "token_hash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sessions 缺 token_hash 列: %v", cols)
	}
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_sessions_token_hash'`).Scan(&name)
	if err != nil {
		t.Fatalf("缺 idx_sessions_token_hash 索引（鉴权热路径会退化成全表扫）: %v", err)
	}
}

// sessionColumns 返回 sessions 表的列名（按 PRAGMA 顺序，用于整行转储/列集合断言）。
func sessionColumns(db *DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('sessions') ORDER BY cid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
