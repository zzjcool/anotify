package store

import (
	"database/sql"
	"testing"
)

// TestPasskey_RenameRoundtrip 验证 RenamePasskey 的存取往返一致性。
// 复现 bug：安全页「重命名 Passkey」走 PATCH /v1/auth/passkeys/:id，
// 后端没有 RenamePasskey 方法 → 编译失败 / 端点 404 → 前端 demo 模式。
func TestPasskey_RenameRoundtrip(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "ren", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	p := &Passkey{
		ID: "cred-ren-1", UserID: u.ID, PublicKey: []byte{1, 2}, Name: "旧名字",
		Transports: []string{"internal"}, BackupEligible: false, CreatedAt: Now(),
	}
	if err := db.CreatePasskey(p); err != nil {
		t.Fatalf("create passkey: %v", err)
	}

	// 重命名
	if err := db.RenamePasskey(p.ID, "新名字"); err != nil {
		t.Fatalf("rename passkey: %v", err)
	}

	// 往返：名字变了，其它字段（含 BackupEligible / transports）不变
	got, err := db.GetPasskeyByID(p.ID)
	if err != nil {
		t.Fatalf("get after rename: %v", err)
	}
	if got.Name != "新名字" {
		t.Errorf("name 往返不一致: got %q want %q", got.Name, "新名字")
	}
	if got.BackupEligible != false {
		t.Errorf("rename 不应改 BackupEligible: got %v", got.BackupEligible)
	}
	if len(got.Transports) != 1 || got.Transports[0] != "internal" {
		t.Errorf("rename 不应改 transports: got %v", got.Transports)
	}

	// 不存在的凭证 → ErrNotFound
	if err := db.RenamePasskey("cred-not-exist", "x"); err == nil {
		t.Errorf("重命名不存在的凭证应报错")
	}
}

// TestPasskey_DeleteRoundtrip 验证 DeletePasskey 删除后查不到。
// 复现 bug：安全页「删除 Passkey」走 DELETE /v1/auth/passkeys/:id，
// 后端没有 DeletePasskey → 404 → 前端 demo 模式。
func TestPasskey_DeleteRoundtrip(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "del", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	p1 := &Passkey{
		ID: "cred-del-1", UserID: u.ID, PublicKey: []byte{1}, Name: "a",
		Transports: []string{"internal"}, CreatedAt: Now(),
	}
	p2 := &Passkey{
		ID: "cred-del-2", UserID: u.ID, PublicKey: []byte{2}, Name: "b",
		Transports: []string{"hybrid"}, CreatedAt: Now(),
	}
	if err := db.CreatePasskey(p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}
	if err := db.CreatePasskey(p2); err != nil {
		t.Fatalf("create p2: %v", err)
	}

	// 删 p1
	if err := db.DeletePasskey(p1.ID); err != nil {
		t.Fatalf("delete passkey: %v", err)
	}

	// p1 查不到了
	if _, err := db.GetPasskeyByID(p1.ID); err != ErrNotFound {
		t.Errorf("删除后应 ErrNotFound, got %v", err)
	}
	// p2 还在
	list, err := db.ListPasskeysByUser(u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != p2.ID {
		t.Errorf("删除 p1 后应只剩 p2, got %v", list)
	}

	// 删不存在的 → ErrNotFound（幂等删除可也接受 nil，但这里要求明确报错以便上层转 404）
	if err := db.DeletePasskey("cred-not-exist"); err != nil && err != ErrNotFound {
		t.Errorf("删不存在的凭证应返回 nil(幂等) 或 ErrNotFound, got %v", err)
	}

	// 二次删除已删的 p1：要么 ErrNotFound，要么 nil（幂等），都不能 panic
	_ = db.DeletePasskey(p1.ID)
}

// TestPasskey_ListEmptyReturnsEmpty 验证空用户列表返回空切片而非 nil。
// 复现 bug：前端 fetchApi 拿到 null 会误判为「未连接」进 demo 模式，
// store 层必须保证空列表返回 [] 而非 nil（AGENTS.md 第 3 节）。
func TestPasskey_ListEmptyReturnsEmpty(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "empty", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	list, err := db.ListPasskeysByUser(u.ID)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if list == nil {
		t.Errorf("空列表应返回非 nil 切片，否则前端误判 demo 模式")
	}
	if len(list) != 0 {
		t.Errorf("空列表 len 应为 0, got %d", len(list))
	}
}

// TestPasskey_LastUsedAtNullable 验证 last_used_at 可空（新建凭证无 LastUsedAt）。
// 保护前端 fmtTime(null) 不崩。
func TestPasskey_LastUsedAtNullable(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "lu", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	p := &Passkey{
		ID: "cred-lu-1", UserID: u.ID, PublicKey: []byte{1}, Name: "x",
		Transports: []string{"internal"}, CreatedAt: Now(),
		LastUsedAt: sql.NullInt64{}, // 空
	}
	if err := db.CreatePasskey(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetPasskeyByID(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastUsedAt.Valid {
		t.Errorf("新建凭证 LastUsedAt 应为 NULL, got %v", got.LastUsedAt.Int64)
	}
}
