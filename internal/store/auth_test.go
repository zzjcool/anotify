package store

import (
	"errors"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUser_CreateGet(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "alice", DisplayName: "Alice", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetUserByUsername("alice")
	if err != nil {
		t.Fatalf("get by username: %v", err)
	}
	if got.ID != u.ID || got.DisplayName != "Alice" {
		t.Errorf("got %+v", got)
	}
	got2, err := db.GetUserByID(u.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got2.Username != "alice" {
		t.Errorf("got %+v", got2)
	}
	// 不存在。
	if _, err := db.GetUserByUsername("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("应返回 ErrNotFound, got %v", err)
	}
	// 用户名唯一约束。
	dup := &User{ID: NewUserID(), Username: "alice", CreatedAt: Now()}
	if err := db.CreateUser(dup); err == nil {
		t.Errorf("重复用户名应报错")
	}
}

func TestPasskey_CRUD(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "bob", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	p := &Passkey{
		ID: "cred-1", UserID: u.ID, PublicKey: []byte{1, 2, 3},
		SignCount: 0, Name: "手机", Transports: []string{"internal", "hybrid"},
		CreatedAt: Now(),
	}
	if err := db.CreatePasskey(p); err != nil {
		t.Fatalf("create passkey: %v", err)
	}
	got, err := db.GetPasskeyByID("cred-1")
	if err != nil {
		t.Fatalf("get passkey: %v", err)
	}
	if got.Name != "手机" || len(got.Transports) != 2 || got.Transports[0] != "internal" {
		t.Errorf("got %+v transports=%v", got, got.Transports)
	}
	// 列表。
	list, err := db.ListPasskeysByUser(u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("list len got %d want 1", len(list))
	}
	// 更新签名计数。
	if err := db.UpdatePasskeySignCount("cred-1", 5, Now()); err != nil {
		t.Fatalf("update sign count: %v", err)
	}
	got2, _ := db.GetPasskeyByID("cred-1")
	if got2.SignCount != 5 || !got2.LastUsedAt.Valid {
		t.Errorf("sign count got %d lastUsed valid=%v", got2.SignCount, got2.LastUsedAt.Valid)
	}
}

func TestSession_CRUD(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "carol", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	s := &Session{ID: NewSessionID(), UserID: u.ID, CreatedAt: Now(), ExpiresAt: Now() + 3600, LastSeen: Now()}
	if err := db.CreateSession(s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err := db.GetSession(s.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.UserID != u.ID {
		t.Errorf("got %+v", got)
	}
	// touch。
	if err := db.TouchSession(s.ID, Now()+10); err != nil {
		t.Fatalf("touch: %v", err)
	}
	// list。
	list, err := db.ListSessionsByUser(u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("list len got %d want 1", len(list))
	}
	// delete。
	if err := db.DeleteSession(s.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetSession(s.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后应 ErrNotFound, got %v", err)
	}
}

func TestAPIKey_CRUD(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "dave", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	k := &APIKey{
		ID: NewEventID(), UserID: u.ID, Name: "ci", KeyHash: "$argon2id$fake",
		Prefix: "ant_send_abcdef", Scopes: []string{"notify:send"}, Enabled: true, CreatedAt: Now(),
	}
	if err := db.CreateAPIKey(k); err != nil {
		t.Fatalf("create key: %v", err)
	}
	got, err := db.GetAPIKeyByPrefix("ant_send_abcdef")
	if err != nil {
		t.Fatalf("get by prefix: %v", err)
	}
	if !got.Enabled || got.Name != "ci" || len(got.Scopes) != 1 || got.Scopes[0] != "notify:send" {
		t.Errorf("got %+v scopes=%v", got, got.Scopes)
	}
	// touch。
	if err := db.TouchAPIKey(k.ID, Now()); err != nil {
		t.Fatalf("touch: %v", err)
	}
	// list。
	list, err := db.ListAPIKeysByUser(u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("list len got %d want 1", len(list))
	}
	// revoke。
	if err := db.RevokeAPIKey(k.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got2, _ := db.GetAPIKeyByPrefix("ant_send_abcdef")
	if got2.Enabled {
		t.Errorf("revoke 后应 enabled=false")
	}
	// 不存在前缀。
	if _, err := db.GetAPIKeyByPrefix("ant_send_zzzzzz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("应 ErrNotFound, got %v", err)
	}
}
