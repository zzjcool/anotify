package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 密码登录（契约 §3）的包级用例：校验规则、设置/修改/移除的状态机、登录的错误映射。
// 灯板：
//   - 明文永不落库（库里的 password_hash 必须是 argon2id PHC，且不等于明文）
//   - 失败路径的错误**不**泄露用户是否存在（除了「未设置密码 / 已禁用」这两类
//     必须给真人的提示）

// ---------- ValidatePassword ----------

func TestValidatePassword_Boundaries(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		ok   bool
		want string // 失败时要包含的文案片段
	}{
		{"7 太短", "1234567", false, "至少 8 个字符"},
		{"8 刚好", "12345678", true, ""},
		{"128 刚好", strings.Repeat("a", 128), true, ""},
		{"129 太长", strings.Repeat("a", 129), false, "最多 128 个字符"},
		{"全空白（8 空格）", "        ", false, "不能全为空白"},
		{"全空白（tab/换行混排）", "\t\t\n\n\r\n  ", false, "不能全为空白"},
		{"空串", "", false, "至少 8 个字符"},
		// 中文按 rune 计：8 个汉字合格（按字节算是 24，那会把中文用户挡在门外）。
		{"8 个汉字", "密码密码密码密码", true, ""},
		{"7 个汉字", "密码密码密码密", false, "至少 8 个字符"},
		// 前后空格不算空白：只有**全**空白才拒。
		{"首尾空格但中间有内容", "  correct horse  ", true, ""},
		{"表情符号按 rune 计", "🔐🔐🔐🔐🔐🔐🔐🔐", true, ""},
	}
	for _, tc := range cases {
		err := ValidatePassword(tc.pw)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: 期望通过, got %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 期望拒绝, got nil", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: 文案 = %q, 期望含 %q", tc.name, err.Error(), tc.want)
		}
	}
}

// ---------- 设置 / 状态 ----------

func TestPassword_SetThenStatusAndLogin(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "pwuser")

	// 初始：没有密码。
	st, err := svc.PasswordStatus(u.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.HasPassword || st.UpdatedAt != 0 {
		t.Fatalf("新用户状态 = %+v，期望 {false 0}", st)
	}
	// 未设置密码时登录 → ErrPasswordNotSet（文案要指向 Passkey）。
	if _, err := svc.LoginWithPassword("pwuser", "whatever-1", "UA"); !errors.Is(err, ErrPasswordNotSet) {
		t.Fatalf("未设置密码登录 err = %v, 期望 ErrPasswordNotSet", err)
	} else if !strings.Contains(err.Error(), "请使用 Passkey 登录") {
		t.Errorf("未设置密码的文案应指向 Passkey: %v", err)
	}

	// 设置（currentPassword 被忽略）。
	st, err = svc.SetPassword(u.ID, "", "correct horse battery")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !st.HasPassword || st.UpdatedAt == 0 {
		t.Fatalf("设置后状态 = %+v, 期望 {true, >0}", st)
	}

	// 库里是 argon2id PHC 哈希，且不等于明文。
	rec, err := svc.db.GetUserPassword(u.ID)
	if err != nil {
		t.Fatalf("db get: %v", err)
	}
	if !strings.HasPrefix(rec.Hash, "$argon2id$") {
		t.Errorf("存储应为 argon2id PHC: %q", rec.Hash)
	}
	if strings.Contains(rec.Hash, "correct horse battery") {
		t.Error("库里出现了明文密码")
	}
	if rec.UpdatedAt != st.UpdatedAt {
		t.Errorf("状态 updatedAt=%d 与库 %d 不一致", st.UpdatedAt, rec.UpdatedAt)
	}

	// 登录成功 → 会话可用。
	sessID, err := svc.LoginWithPassword("pwuser", "correct horse battery", "Test · Linux")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	sess, err := svc.Sessions().Validate(sessID)
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	if sess.UserID != u.ID {
		t.Errorf("会话 user = %q, 期望 %q", sess.UserID, u.ID)
	}
	if sess.DeviceName != "Test · Linux" {
		t.Errorf("会话 deviceName = %q", sess.DeviceName)
	}

	// 错误密码 → ErrInvalidCredentials。
	if _, err := svc.LoginWithPassword("pwuser", "correct horse batterY", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("错密码 err = %v, 期望 ErrInvalidCredentials", err)
	}
}

func TestPassword_LoginFailureMapping(t *testing.T) {
	svc := newTestService(t)

	// 1) 用户不存在 → 凭据错误（不泄露存在性），文案固定。
	if _, err := svc.LoginWithPassword("ghost", "some-password", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("不存在用户 err = %v, 期望 ErrInvalidCredentials", err)
	} else if !strings.Contains(err.Error(), "用户名或密码错误") {
		t.Errorf("文案 = %q", err.Error())
	}

	// 2) 用户名不合法 → 同样是凭据错误（不给"用户名格式不对"这种可以用来筛名字的提示）。
	if _, err := svc.LoginWithPassword("no spaces allowed", "some-password", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("非法用户名 err = %v, 期望 ErrInvalidCredentials", err)
	}

	// 3) 空字段 → 凭据错误。
	if _, err := svc.LoginWithPassword("", "some-password", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("空 username err = %v, 期望 ErrInvalidCredentials", err)
	}
	if _, err := svc.LoginWithPassword("pwuser2", "", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("空 password err = %v, 期望 ErrInvalidCredentials", err)
	}

	// 4) 禁用账号：即使密码对也不能登录。
	disabled := createUser(t, svc, "pwdisabled")
	if _, err := svc.SetPassword(disabled.ID, "", "correct horse battery"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := svc.db.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, disabled.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := svc.LoginWithPassword("pwdisabled", "correct horse battery", "UA"); err == nil {
		t.Fatal("禁用账号不应能登录")
	} else if !strings.Contains(err.Error(), "禁用") {
		t.Errorf("want disabled error, got %v", err)
	}

	// 5) 哈希坏掉（数据被外部改坏）→ 凭据错误，不能 500。
	broken := createUser(t, svc, "pwbroken")
	if err := svc.db.SetUserPassword(broken.ID, "not-a-phc-hash", store.Now()); err != nil {
		t.Fatalf("set broken hash: %v", err)
	}
	if _, err := svc.LoginWithPassword("pwbroken", "correct horse battery", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("坏哈希 err = %v, 期望 ErrInvalidCredentials", err)
	}
}

func TestPassword_ChangeRequiresCurrent(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "pwchange")
	if _, err := svc.SetPassword(u.ID, "", "old-password-1"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// 旧密码错 → 拒绝，且文案固定（前端直接展示）。
	if _, err := svc.SetPassword(u.ID, "wrong-password", "new-password-2"); err == nil {
		t.Fatal("旧密码错不应允许改密码")
	} else if err.Error() != "当前密码不正确" {
		t.Errorf("文案 = %q, 期望「当前密码不正确」", err.Error())
	}

	// 新密码不合规 → 拒绝（校验旧密码通过后才轮到新密码）。
	if _, err := svc.SetPassword(u.ID, "old-password-1", "short"); err == nil {
		t.Fatal("新密码太短不应通过")
	} else if !strings.Contains(err.Error(), "至少 8 个字符") {
		t.Errorf("文案 = %q", err.Error())
	}

	// 都对 → 成功，且旧密码立刻失效、新密码可用。
	st, err := svc.SetPassword(u.ID, "old-password-1", "new-password-2")
	if err != nil {
		t.Fatalf("change: %v", err)
	}
	if !st.HasPassword || st.UpdatedAt == 0 {
		t.Errorf("改后状态 = %+v", st)
	}
	if _, err := svc.LoginWithPassword("pwchange", "old-password-1", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("旧密码应失效, err = %v", err)
	}
	if _, err := svc.LoginWithPassword("pwchange", "new-password-2", "UA"); err != nil {
		t.Errorf("新密码应可用: %v", err)
	}
}

func TestPassword_SetForMissingUser(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.SetPassword("usr_nope", "", "correct horse battery"); err == nil {
		t.Fatal("不存在的用户不应能设置密码")
	} else if err.Error() != "用户不存在" {
		t.Errorf("文案 = %q, 期望「用户不存在」", err.Error())
	}
	if _, err := svc.PasswordStatus("usr_nope"); err == nil {
		t.Fatal("不存在的用户查询状态应报错")
	}
	if err := svc.RemovePassword("usr_nope", "correct horse battery"); err == nil {
		t.Fatal("不存在的用户不应能移除密码")
	}
}

// TestPassword_DummyVerifyRunsOnUnknownUser 「查无用户」路径必须真的跑过一次 argon2id。
//
// 这条不能用计时断言（CI 上抖动大），但可以用“dummy 哈希是否已生成”这一个可观测事实：
// 只要走的是惰性预计算那条分支，它就不是空串。故意在开头把包级状态清空，
// 避免其它用例已经预计算过、让这条断言变成永真。
func TestPassword_DummyVerifyRunsOnUnknownUser(t *testing.T) {
	svc := newTestService(t)
	dummyPasswordOnce = sync.Once{}
	dummyPasswordHash = ""

	if _, err := svc.LoginWithPassword("ghostuser", "some-password", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, 期望 ErrInvalidCredentials", err)
	}
	if dummyPasswordHash == "" {
		t.Fatal("查无用户路径没有生成 dummy 哈希：计时侧信道仍然存在")
	}
	if !strings.HasPrefix(dummyPasswordHash, "$argon2id$") {
		t.Errorf("dummy 也是 argon2id PHC: %q", dummyPasswordHash)
	}
}

// ---------- 移除 ----------

func TestPassword_RemoveWithoutPasskeyRejected(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "pwnopasskey")
	if _, err := svc.SetPassword(u.ID, "", "correct horse battery"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// 没有 Passkey + 移除密码 = 账号彻底进不去 → ErrLastAuthMethod。
	if err := svc.RemovePassword(u.ID, "correct horse battery"); !errors.Is(err, ErrLastAuthMethod) {
		t.Fatalf("err = %v, 期望 ErrLastAuthMethod", err)
	}
	// 密码必须还在（守卫在写入之前）。
	st, err := svc.PasswordStatus(u.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.HasPassword {
		t.Error("被拒绝的移除不应清掉密码")
	}
}

func TestPassword_RemoveWithPasskeySucceeds(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "pwwithpasskey")
	if _, err := svc.SetPassword(u.ID, "", "correct horse battery"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := svc.db.CreatePasskey(&store.Passkey{
		ID: "cred-pw-1", UserID: u.ID, PublicKey: []byte{1, 2, 3},
		Name: "手机", Transports: []string{"internal"}, CreatedAt: store.Now(),
	}); err != nil {
		t.Fatalf("create passkey: %v", err)
	}

	// 旧密码错 → 拒绝。
	if err := svc.RemovePassword(u.ID, "wrong-password"); err == nil {
		t.Fatal("旧密码错不应允许移除")
	} else if err.Error() != "当前密码不正确" {
		t.Errorf("文案 = %q", err.Error())
	}

	if err := svc.RemovePassword(u.ID, "correct horse battery"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	st, err := svc.PasswordStatus(u.ID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.HasPassword || st.UpdatedAt != 0 {
		t.Errorf("移除后状态 = %+v, 期望 {false 0}", st)
	}
	// 移除后密码登录失效（未设置密码），Passkey 仍在（最后一种登录方式保住了）。
	if _, err := svc.LoginWithPassword("pwwithpasskey", "correct horse battery", "UA"); !errors.Is(err, ErrPasswordNotSet) {
		t.Errorf("移除后密码登录 err = %v, 期望 ErrPasswordNotSet", err)
	}
	pks, err := svc.db.ListPasskeysByUser(u.ID)
	if err != nil {
		t.Fatalf("list passkeys: %v", err)
	}
	if len(pks) != 1 {
		t.Errorf("Passkey 数量 = %d, 期望 1（移除密码不该碰 Passkey）", len(pks))
	}

	// 幂等：再移除一次仍然成功。
	if err := svc.RemovePassword(u.ID, ""); err != nil {
		t.Errorf("重复移除应幂等成功, got %v", err)
	}
}

// TestPassword_ListPasskeysEmptyIsNotNil 兜住「空列表返回 []」这条不变量：
// 移除密码的守卫靠 len(list)==0 判断，如果 ListPasskeysByUser 哪天返回 nil 切片，
// 语义上没变，但这里顺手把「空列表不是错误」钉一下。
func TestPassword_ListPasskeysEmptyIsNotNil(t *testing.T) {
	svc := newTestService(t)
	u := createUser(t, svc, "pwpkempty")
	pks, err := svc.db.ListPasskeysByUser(u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if pks == nil {
		t.Fatal("空列表应为 [] 而非 nil")
	}
	if len(pks) != 0 {
		t.Fatalf("len = %d", len(pks))
	}
}

// ---------- 用密码注册（契约「追加：用密码注册」§A3 / §A6） ----------
//
// 灯板：
//   - 成功一次 = 能用密码立刻登录（会话可 Validate）+ 8 个恢复码 + 库里只有 PHC 哈希
//   - 账号没有任何 Passkey（密码就是它的登录方式）
//   - 重复用户名（含并发输家）统一「auth: 用户名已存在」——与 BeginRegister 同文案
//   - 校验失败**不留用户行**（§A1 的原子性：不能有「有账号没登录方式」的半成品）
//   - 首用户 admin、第二个 member（判定在 store 事务内，RoleAuto）

func TestRegisterWithPassword_Success(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	const pw = "correct horse battery"
	sessID, codes, err := svc.RegisterWithPassword(ctx, "pwreguser", "", pw, "Test · Linux")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if sessID == "" {
		t.Fatal("注册成功后应直接建会话（返回 sessionID）")
	}
	if len(codes) != recoveryCodeCount {
		t.Fatalf("恢复码数量 = %d, want %d（明文仅此一次）", len(codes), recoveryCodeCount)
	}
	for _, c := range codes {
		if c == "" {
			t.Error("恢复码里出现空串")
		}
	}

	u, err := svc.db.GetUserByUsername("pwreguser")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	// displayName 缺省用 username。
	if u.DisplayName != "pwreguser" {
		t.Errorf("displayName = %q, want username（缺省用 username）", u.DisplayName)
	}
	// 首个注册用户是 admin（判定在 store 事务内，不是这里先查后写）。
	if u.Role != store.RoleAdmin {
		t.Errorf("首个注册用户 role = %q, want %q", u.Role, store.RoleAdmin)
	}

	// 密码只存 PHC 哈希，明文不在库里。
	rec, err := svc.db.GetUserPassword(u.ID)
	if err != nil {
		t.Fatalf("get user password: %v", err)
	}
	if !strings.HasPrefix(rec.Hash, "$argon2id$") {
		t.Errorf("存储应为 argon2id PHC: %q", rec.Hash)
	}
	if strings.Contains(rec.Hash, pw) {
		t.Error("库里出现了明文密码")
	}
	if rec.UpdatedAt == 0 {
		t.Error("password_updated_at 应 > 0")
	}

	// 恢复码在库里是 8 条未使用。
	n, err := svc.db.CountUnusedRecoveryCodes(u.ID)
	if err != nil {
		t.Fatalf("count codes: %v", err)
	}
	if n != recoveryCodeCount {
		t.Errorf("未使用恢复码 = %d, want %d", n, recoveryCodeCount)
	}

	// 没有任何 Passkey：密码注册不走 WebAuthn。
	pks, err := svc.db.ListPasskeysByUser(u.ID)
	if err != nil {
		t.Fatalf("list passkeys: %v", err)
	}
	if len(pks) != 0 {
		t.Errorf("密码注册不应创建 Passkey, got %d", len(pks))
	}

	// 会话可用，设备名来自调用方。
	sess, err := svc.Sessions().Validate(sessID)
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	if sess.UserID != u.ID || sess.DeviceName != "Test · Linux" {
		t.Errorf("会话 = %+v, want user=%s device=Test · Linux", sess, u.ID)
	}

	// 注册完立刻能用密码登录（round-trip：哈希是可用且匹配的）。
	if _, err := svc.LoginWithPassword("pwreguser", pw, "UA"); err != nil {
		t.Errorf("注册后密码登录应成功: %v", err)
	}
	if _, err := svc.LoginWithPassword("pwreguser", pw+"x", "UA"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("错密码 err = %v, want ErrInvalidCredentials", err)
	}

	// 注册仍原子地建立默认命名空间；v4 不再播种中央执行的内置能力。
	nss, err := svc.db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(nss) != 1 {
		t.Fatalf("命名空间数 = %d, want 1（注册必须建默认命名空间）", len(nss))
	}
	if nss[0].Slug != store.DefaultNamespaceSlug || nss[0].Name != store.DefaultNamespaceName {
		t.Fatalf("默认命名空间 = %+v", nss[0])
	}
	if err := svc.db.AuthorizeNamespace(ctx, u.ID, nss[0].ID); err != nil {
		t.Fatalf("新用户不能使用注册建立的命名空间: %v", err)
	}
	// 用真实脚本正文建立批准的资源域，不把已退役的 builtin 播种搬回注册生产路径。
	capability := &store.CapabilityVersion{NamespaceID: nss[0].ID, Name: "registration-probe", Version: 1}
	capability.Spec.Params = []byte(`{"type":"object"}`)
	capability.Spec.Run.Entrypoint = "run.sh"
	capability.Spec.Run.Files = map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"}
	if err := svc.db.CreateCapability(ctx, capability); err != nil {
		t.Fatalf("新用户命名空间无法建立真实能力: %v", err)
	}
	stored, err := svc.db.GetCapability(ctx, nss[0].ID, capability.Name, capability.Version)
	if err != nil {
		t.Fatalf("读取真实注册资源: %v", err)
	}
	if stored.ID != capability.ID || stored.Digest == "" || stored.Spec.Run.Files["run.sh"] != capability.Spec.Run.Files["run.sh"] {
		t.Fatalf("真实注册资源未按不可变身份往返: %+v", stored)
	}
}

// TestRegisterWithPassword_FirstAdminSecondMember 角色规则与 Passkey 路径一致。
func TestRegisterWithPassword_FirstAdminSecondMember(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if _, _, err := svc.RegisterWithPassword(ctx, "pwadmin", "", "correct horse battery", "UA"); err != nil {
		t.Fatalf("register first: %v", err)
	}
	first, err := svc.db.GetUserByUsername("pwadmin")
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if first.Role != store.RoleAdmin {
		t.Errorf("first role = %q, want admin", first.Role)
	}

	if _, _, err := svc.RegisterWithPassword(ctx, "pwmember", "", "correct horse battery", "UA"); err != nil {
		t.Fatalf("register second: %v", err)
	}
	second, err := svc.db.GetUserByUsername("pwmember")
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if second.Role != store.RoleMember {
		t.Errorf("second role = %q, want member", second.Role)
	}
}

// TestRegisterWithPassword_DuplicateUsername 重复用户名：文案与 BeginRegister 一致，
// 且不会留下第二个用户行。
func TestRegisterWithPassword_DuplicateUsername(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if _, _, err := svc.RegisterWithPassword(ctx, "dupuser", "", "correct horse battery", "UA"); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, _, err := svc.RegisterWithPassword(ctx, "dupuser", "", "another good password", "UA")
	if err == nil {
		t.Fatal("重复用户名应报错")
	}
	if err.Error() != "auth: 用户名已存在" {
		t.Errorf("文案 = %q, want %q（与 BeginRegister 同文案）", err.Error(), "auth: 用户名已存在")
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM users WHERE username = ?`, "dupuser").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("用户行数 = %d, want 1", n)
	}
	// 失败不该多建命名空间（每个用户恰一个）。
	nss, err := svc.db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(nss) != 1 {
		t.Errorf("命名空间数 = %d, want 1（重复注册不能建出第二个）", len(nss))
	}
	// 原密码仍然可用（重复注册没有覆盖它）。
	if _, err := svc.LoginWithPassword("dupuser", "correct horse battery", "UA"); err != nil {
		t.Errorf("重复注册不应影响已有密码: %v", err)
	}
}

// TestRegisterWithPassword_ConcurrentDuplicate 并发注册同名：恰好一个赢家，
// 输家（无论输在预检还是输在 UNIQUE）都必须拿到同一句「用户名已存在」。
//
// 用文件库：":memory:" 会把池压到 1，两个注册被串行化，永远走不到 UNIQUE 那条分支。
func TestRegisterWithPassword_ConcurrentDuplicate(t *testing.T) {
	const rounds = 5
	for round := 0; round < rounds; round++ {
		svc, _ := newTestServiceFile(t)
		const racers = 2
		var (
			start  = make(chan struct{})
			wg     sync.WaitGroup
			errs   = make([]error, racers)
			sessOK = make([]bool, racers)
		)
		wg.Add(racers)
		for i := 0; i < racers; i++ {
			go func() {
				defer wg.Done()
				<-start
				sessID, _, err := svc.RegisterWithPassword(context.Background(), "samename", "", "correct horse battery", "UA")
				errs[i] = err
				sessOK[i] = sessID != ""
			}()
		}
		close(start)
		wg.Wait()

		wins, dups := 0, 0
		for i, err := range errs {
			switch {
			case err == nil:
				wins++
				if !sessOK[i] {
					t.Fatalf("round %d: 赢家没有会话", round)
				}
			case err.Error() == "auth: 用户名已存在":
				dups++
			default:
				t.Fatalf("round %d: 输家的错误不在契约内: %v", round, err)
			}
		}
		if wins != 1 || dups != racers-1 {
			t.Fatalf("round %d: wins=%d dups=%d, want 1/%d", round, wins, dups, racers-1)
		}
		var n int
		if err := svc.db.QueryRow(`SELECT COUNT(*) FROM users WHERE username = ?`, "samename").Scan(&n); err != nil {
			t.Fatalf("round %d: count: %v", round, err)
		}
		if n != 1 {
			t.Fatalf("round %d: 同名用户 = %d, want 1", round, n)
		}
	}
}

// TestRegisterWithPassword_RejectsInvalid 校验失败：原文案返回，且**不留用户行**
// （原子性：哈希都还没生成，绝不能先把用户建出来）。
func TestRegisterWithPassword_RejectsInvalid(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		username string
		password string
		want     string
	}{
		{"用户名太短", "a", "correct horse battery", "用户名至少 2 个字符"},
		{"用户名非法字符", "bad name", "correct horse battery", "用户名只能包含字母、数字"},
		{"空用户名", "", "correct horse battery", "用户名不能为空"},
		{"密码太短", "okname", "short", "密码至少 8 个字符"},
		{"密码太长", "okname", strings.Repeat("a", PasswordMaxLen+1), "密码最多 128 个字符"},
		{"密码全空白", "okname", strings.Repeat(" ", PasswordMinLen), "密码不能全为空白"},
	}
	for _, tc := range cases {
		_, _, err := svc.RegisterWithPassword(ctx, tc.username, "", tc.password, "UA")
		if err == nil {
			t.Errorf("%s: 期望拒绝, got nil", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: 文案 = %q, want 含 %q", tc.name, err.Error(), tc.want)
		}
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 0 {
		t.Fatalf("校验失败的注册留下了 %d 个用户行（应为 0：不能有「有账号没登录方式」的半成品）", n)
	}
	if _, err := svc.db.GetUserByUsername("okname"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("校验失败后用户名不该被占用: %v", err)
	}
}

// TestRegisterWithPassword_LongDisplayNameKept displayName 显式给出时按原值落库。
func TestRegisterWithPassword_LongDisplayNameKept(t *testing.T) {
	svc := newTestService(t)
	if _, _, err := svc.RegisterWithPassword(context.Background(), "nameduser", "显示 名字", "correct horse battery", "UA"); err != nil {
		t.Fatalf("register: %v", err)
	}
	u, err := svc.db.GetUserByUsername("nameduser")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u.DisplayName != "显示 名字" {
		t.Errorf("displayName = %q", u.DisplayName)
	}
}
