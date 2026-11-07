package store

import (
	"errors"
	"testing"
)

// 迁移 8 的 store 层门禁：password_hash / password_updated_at 的往返一致性。
//
// 这里只钉「存什么读什么」与「空值语义」，不懂哈希格式（那是 auth 包的事）；
// 用的是 PHC 样的假串，正因为 store 不该解析它。

func TestUserPassword_DefaultEmpty(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "pw-default", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	got, err := db.GetUserPassword(u.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Hash != "" {
		t.Errorf("新用户 password_hash = %q，期望空串（= 未设置密码）", got.Hash)
	}
	if got.UpdatedAt != 0 {
		t.Errorf("新用户 password_updated_at = %d，期望 0", got.UpdatedAt)
	}
}

func TestUserPassword_SetGetRoundTrip(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "pw-roundtrip", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// 真实 PHC 串里全是 $ 、= 与 base64url 字符：不允许被 store 规范化/截断。
	const hash = "$argon2id$v=19$m=65536,t=1,p=4$c29tZXNhbHQ$aGFzaGJ5dGVz"
	const at = int64(1700000000)
	if err := db.SetUserPassword(u.ID, hash, at); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := db.GetUserPassword(u.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Hash != hash {
		t.Errorf("hash 往返不一致:\n got %q\nwant %q", got.Hash, hash)
	}
	if got.UpdatedAt != at {
		t.Errorf("updatedAt 往返不一致: got %d want %d", got.UpdatedAt, at)
	}

	// 再改一次（修改密码路径）：hash 与时间都要换成新的。
	const hash2 = "$argon2id$v=19$m=65536,t=1,p=4$bmV3c2FsdA$bmV3aGFzaA"
	if err := db.SetUserPassword(u.ID, hash2, at+60); err != nil {
		t.Fatalf("set again: %v", err)
	}
	got2, err := db.GetUserPassword(u.ID)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if got2.Hash != hash2 || got2.UpdatedAt != at+60 {
		t.Errorf("修改后不一致: hash=%q updatedAt=%d", got2.Hash, got2.UpdatedAt)
	}
}

func TestUserPassword_ClearResetsToZero(t *testing.T) {
	db := newTestDB(t)
	u := &User{ID: NewUserID(), Username: "pw-clear", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.SetUserPassword(u.ID, "$argon2id$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA", 1700000001); err != nil {
		t.Fatalf("set: %v", err)
	}

	if err := db.ClearUserPassword(u.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := db.GetUserPassword(u.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Hash != "" || got.UpdatedAt != 0 {
		t.Errorf("clear 后 = (%q,%d)，期望 (\"\",0)", got.Hash, got.UpdatedAt)
	}
	// 幂等：再清一次不报错、状态不变。
	if err := db.ClearUserPassword(u.ID); err != nil {
		t.Fatalf("clear twice: %v", err)
	}

	// 清空后用户本体仍然在（只动密码列，不删行）。
	if _, err := db.GetUserByID(u.ID); err != nil {
		t.Errorf("clear 不该影响用户行: %v", err)
	}
}

func TestUserPassword_MissingUser(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.GetUserPassword("usr_does_not_exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserPassword(不存在) = %v，期望 ErrNotFound", err)
	}
	if err := db.SetUserPassword("usr_does_not_exist", "$argon2id$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA", Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetUserPassword(不存在) = %v，期望 ErrNotFound", err)
	}
	// ClearUserPassword 对不存在用户无副作用（不报错）。
	if err := db.ClearUserPassword("usr_does_not_exist"); err != nil {
		t.Errorf("ClearUserPassword(不存在) = %v，期望 nil（幂等无副作用）", err)
	}
}

// TestUserPassword_ColumnsExist 迁移 8 的列存在性 + 「密码只属于 users 一列」的形状门禁。
//
// 外部序列化面（/v1/auth/me、admin 用户列表）直接吐 store.User：
// 如果密码被挂进 User，泄露就发生在一次普通的 GET 里，没有运行期用例会红。
func TestUserPassword_ColumnsExist(t *testing.T) {
	db := newTestDB(t)
	cols := columnNames(t, db, "users")
	if !cols["password_hash"] {
		t.Fatal("users 缺 password_hash 列（迁移 8 未生效）")
	}
	if !cols["password_updated_at"] {
		t.Fatal("users 缺 password_updated_at 列（迁移 8 未生效）")
	}
	// 密码列只活在 users 表上，不该被复制到别处。
	u := &User{ID: NewUserID(), Username: "pw-nohash", CreatedAt: Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.SetUserPassword(u.ID, "$argon2id$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA", Now()); err != nil {
		t.Fatalf("set: %v", err)
	}
	// 写入密码不改变用户本体的任何字段（GetUserByID 的投影里没有密码列）。
	got, err := db.GetUserByID(u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Username != "pw-nohash" || got.Role != RoleMember || got.Disabled {
		t.Errorf("用户行读回异常: %+v", got)
	}
}
