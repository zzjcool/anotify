package auth

import (
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
