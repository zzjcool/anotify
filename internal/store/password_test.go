package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// ---------- 用密码注册（契约「追加：用密码注册」§A4 / §A6） ----------
//
// 这两条盯的是同一个不变量：**注册出来的用户行怎么建，决定它有什么登录方式**。
// 密码注册必须在同一个事务里把「用户 + 默认命名空间 + 内置能力 + 密码列」一次写全；
// Passkey 注册则必须继续留空密码列（否则一个只能走 Passkey 的账号会凭空多出
// 「hasPassword=true」的对外状态）。

// phcHash 是一个形状正确的 argon2id PHC 假串：store 不解析哈希，只做往返。
const phcHash = "$argon2id$v=19$m=65536,t=1,p=4$c2FsdHNhbHQ$aGFzaGJ5dGVz"

// TestRegisterUserWithPassword_RoundTrip 往返一致性：用户 / 密码列 / 命名空间 / 内置能力。
func TestRegisterUserWithPassword_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const at = int64(1700000000)
	u := &User{ID: NewUserID(), Username: "pw-signup", DisplayName: "Pw Signup", CreatedAt: Now(), Role: RoleAuto}
	ns, err := db.RegisterUserWithPassword(ctx, u, phcHash, at)
	if err != nil {
		t.Fatalf("register with password: %v", err)
	}
	// 首用户判定仍在 store 的事务里（RoleAuto）；这里顺手钉住它对密码路径同样生效。
	if u.Role != RoleAdmin {
		t.Errorf("首个注册用户 role = %q, want %q", u.Role, RoleAdmin)
	}
	if ns == nil || ns.ID == "" {
		t.Fatalf("默认命名空间没建出来: %+v", ns)
	}

	// 1) 用户行
	got, err := db.GetUserByID(u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Username != "pw-signup" || got.DisplayName != "Pw Signup" || got.Role != RoleAdmin || got.Disabled {
		t.Errorf("用户行读回异常: %+v", got)
	}
	// 2) 密码列（与用户行同一事务写入）
	pw, err := db.GetUserPassword(u.ID)
	if err != nil {
		t.Fatalf("get password: %v", err)
	}
	if pw.Hash != phcHash {
		t.Errorf("password_hash 往返不一致:\n got %q\nwant %q", pw.Hash, phcHash)
	}
	if pw.UpdatedAt != at {
		t.Errorf("password_updated_at = %d, want %d", pw.UpdatedAt, at)
	}
	// 3) 命名空间
	if _, err := db.GetNamespace(ctx, ns.ID); err != nil {
		t.Errorf("默认命名空间查不到: %v", err)
	}
	nss, err := db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(nss) != 1 || nss[0].ID != ns.ID {
		t.Errorf("命名空间列表 = %+v, want 只有 %s", nss, ns.ID)
	}
	// 4) v4 不播种任何内置能力（identity.go registerUser 注释：链尾是普通脚本能力）。
	//    注册产生的命名空间必须是**空**的——这条断言取代了 v3 的 builtin 播种断言。
	caps, err := db.ListCapabilities(ctx, ns.ID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if len(caps) != 0 {
		t.Errorf("新注册命名空间里有 %d 个能力，v4 不该播种内置能力", len(caps))
	}

	// 5) 第二个用户不是 admin（RoleAuto 不是「永远是 admin」）
	u2 := &User{ID: NewUserID(), Username: "pw-signup-2", CreatedAt: Now(), Role: RoleAuto}
	if _, err := db.RegisterUserWithPassword(ctx, u2, phcHash, at+1); err != nil {
		t.Fatalf("register second: %v", err)
	}
	if u2.Role != RoleMember {
		t.Errorf("第二个注册用户 role = %q, want %q", u2.Role, RoleMember)
	}
}

// TestRegisterUser_PasskeyPathKeepsPasswordEmpty Passkey 注册路径必须仍然写空密码列，
// 且与「列默认值」完全一致（CreateUser 走的是不传密码列的 INSERT）。
func TestRegisterUser_PasskeyPathKeepsPasswordEmpty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	u := &User{ID: NewUserID(), Username: "passkey-only", CreatedAt: Now(), Role: RoleAuto}
	if _, err := db.RegisterUser(ctx, u); err != nil {
		t.Fatalf("register (passkey path): %v", err)
	}
	pw, err := db.GetUserPassword(u.ID)
	if err != nil {
		t.Fatalf("get password: %v", err)
	}
	if pw.Hash != "" || pw.UpdatedAt != 0 {
		t.Errorf("Passkey 路径密码列 = (%q,%d), want (\"\",0)", pw.Hash, pw.UpdatedAt)
	}

	// 与列默认值对齐：CreateUser 不写密码列，读回来必须一模一样。
	def := &User{ID: NewUserID(), Username: "column-default", CreatedAt: Now()}
	if err := db.CreateUser(def); err != nil {
		t.Fatalf("create user: %v", err)
	}
	defPw, err := db.GetUserPassword(def.ID)
	if err != nil {
		t.Fatalf("get default password: %v", err)
	}
	if defPw.Hash != pw.Hash || defPw.UpdatedAt != pw.UpdatedAt {
		t.Errorf("RegisterUser 与列默认值不一致: (%q,%d) vs (%q,%d)",
			pw.Hash, pw.UpdatedAt, defPw.Hash, defPw.UpdatedAt)
	}
}

// TestRegisterUserWithPassword_ConcurrentFirstAdmin 并发首注册恰好一个 admin（密码路径）。
//
// 用**文件**库：":memory:" 会把连接池压到 1，所有池操作被串行化，竞态永远不命中
// （见 auth/first_admin_test.go 的同款说明）。断言与 Passkey 路径那条一致：
// admin 恰好 1 个、用户数等于注册者数 —— 「首个用户」判定必须与 INSERT 同事务。
func TestRegisterUserWithPassword_ConcurrentFirstAdmin(t *testing.T) {
	const rounds = 5
	for round := 0; round < rounds; round++ {
		db, _ := newTestDBFile(t)
		ctx := context.Background()

		const registers = 2
		var (
			start = make(chan struct{})
			wg    sync.WaitGroup
			roles = make([]string, registers)
			errs  = make([]error, registers)
		)
		wg.Add(registers)
		for i := 0; i < registers; i++ {
			go func() {
				defer wg.Done()
				u := &User{
					ID:        NewUserID(),
					Username:  fmt.Sprintf("pw-racer-%d-%d", round, i),
					CreatedAt: Now(),
					Role:      RoleAuto,
				}
				<-start
				if _, err := db.RegisterUserWithPassword(ctx, u, phcHash, Now()); err != nil {
					errs[i] = err
					return
				}
				roles[i] = u.Role
			}()
		}
		close(start)
		wg.Wait()

		var admins int
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: register %d failed: %v", round, i, err)
			}
			switch roles[i] {
			case RoleAdmin:
				admins++
			case RoleMember:
			default:
				t.Fatalf("round %d: register %d 拿到非法角色 %q", round, i, roles[i])
			}
		}
		var userCount, adminCount int64
		if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&userCount); err != nil {
			t.Fatalf("round %d: count users: %v", round, err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&adminCount); err != nil {
			t.Fatalf("round %d: count admins: %v", round, err)
		}
		if admins != 1 || adminCount != 1 || userCount != registers {
			t.Fatalf("round %d: admins=%d dbAdmins=%d users=%d，want 1/1/%d —— "+
				"首用户判定必须与 INSERT 同一事务", round, admins, adminCount, userCount, registers)
		}
		// 并发下两个密码哈希都必须落库（没有「有账号没登录方式」的半成品）。
		var withPassword int
		if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE password_hash = ?`, phcHash).Scan(&withPassword); err != nil {
			t.Fatalf("round %d: count hashed users: %v", round, err)
		}
		if withPassword != registers {
			t.Fatalf("round %d: 带密码的用户数 = %d, want %d", round, withPassword, registers)
		}
	}
}
