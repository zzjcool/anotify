package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// newCliAuthTestDB 打开内存 DB 并建用户。
func newCliAuthTestDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	u := &store.User{ID: store.NewUserID(), Username: "testuser", CreatedAt: store.Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return db, u.ID
}

// fakeClock 返回固定时间，可手动推进。
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func newCliAuthManagerWithClock(db *store.DB, clock *fakeClock) *CliAuthManager {
	m := NewCliAuthManager(db, 10*time.Minute)
	m.SetClock(clock.now)
	return m
}

func TestCliAuth_CreateSession_Success(t *testing.T) {
	db, _ := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Unix(1000000, 0)}
	m := newCliAuthManagerWithClock(db, clock)

	created, err := m.CreateSession("my-macbook", []string{ScopeNotifySend})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 验证返回字段
	if !strings.HasPrefix(created.SessionID, "cas_") {
		t.Errorf("SessionID 前缀错误: %q", created.SessionID)
	}
	if created.Secret == "" {
		t.Errorf("Secret 不应为空")
	}
	if len(created.Secret) < 32 {
		t.Errorf("Secret 熵不足: len=%d", len(created.Secret))
	}
	if len(created.UserCode) != 8 {
		t.Errorf("UserCode 长度 got %d want 8", len(created.UserCode))
	}
	// 短码只用去歧义字符集
	for _, c := range created.UserCode {
		if !strings.ContainsRune(userCodeCharset, c) {
			t.Errorf("UserCode 含非法字符 %q", c)
		}
	}
	if created.DeviceName != "my-macbook" {
		t.Errorf("DeviceName got %q", created.DeviceName)
	}
	if created.ExpiresAt != 1000000+600 {
		t.Errorf("ExpiresAt got %d want %d", created.ExpiresAt, 1000000+600)
	}
	if created.PollInterval != 2 {
		t.Errorf("PollInterval got %d want 2", created.PollInterval)
	}

	// 库中不存明文 secret
	got, _ := db.GetCliAuthSession(created.SessionID)
	if got.SecretHash == created.Secret {
		t.Errorf("库中不应存明文 secret")
	}
	if got.Status != store.CliAuthPending {
		t.Errorf("status got %q want pending", got.Status)
	}
}

func TestCliAuth_CreateSession_InvalidParams(t *testing.T) {
	db, _ := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	cases := []struct {
		name       string
		deviceName string
		scopes     []string
	}{
		{"空设备名", "", []string{ScopeNotifySend}},
		{"超长设备名", strings.Repeat("x", 65), []string{ScopeNotifySend}},
		{"空 scopes", "dev", []string{}},
		{"未知 scope", "dev", []string{"admin:*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.CreateSession(tc.deviceName, tc.scopes)
			if !errors.Is(err, ErrInvalidParam) {
				t.Errorf("应返回 ErrInvalidParam, got %v", err)
			}
		})
	}
}

func TestCliAuth_StateMachine_AllTransitions(t *testing.T) {
	db, userID := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	// pending → approved → consumed
	created, _ := m.CreateSession("dev1", []string{ScopeNotifySend, ScopeNotifyReceive})
	if err := m.Approve(created.SessionID, userID, []string{ScopeNotifySend}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	res, err := m.Poll(created.SessionID, created.Secret)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if res.APIKey == "" {
		t.Fatalf("应返回 Key 明文")
	}
	if !strings.HasPrefix(res.APIKey, "ant_") {
		t.Errorf("Key 前缀错误: %q", res.APIKey)
	}
	if res.KeyName != "agent:dev1" {
		t.Errorf("KeyName got %q want agent:dev1", res.KeyName)
	}
	if len(res.Scopes) != 1 || res.Scopes[0] != ScopeNotifySend {
		t.Errorf("Scopes got %v want [notify:send]", res.Scopes)
	}

	// 验证 consumed
	got, _ := db.GetCliAuthSession(created.SessionID)
	if got.Status != store.CliAuthConsumed {
		t.Errorf("status got %q want consumed", got.Status)
	}

	// consumed 后再 approve → ErrAlreadyTerminal
	if err := m.Approve(created.SessionID, userID, []string{ScopeNotifySend}); !errors.Is(err, ErrAlreadyTerminal) {
		t.Errorf("consumed 后 approve 应 ErrAlreadyTerminal, got %v", err)
	}
	// consumed 后再 deny → ErrAlreadyTerminal
	if err := m.Deny(created.SessionID); !errors.Is(err, ErrAlreadyTerminal) {
		t.Errorf("consumed 后 deny 应 ErrAlreadyTerminal, got %v", err)
	}
}

func TestCliAuth_DenyTransition(t *testing.T) {
	db, userID := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	// pending → denied
	created, _ := m.CreateSession("dev2", []string{ScopeNotifySend})
	if err := m.Deny(created.SessionID); err != nil {
		t.Fatalf("deny: %v", err)
	}

	// poll 收到 denied
	res, err := m.Poll(created.SessionID, created.Secret)
	if err != nil {
		t.Fatalf("poll denied: %v", err)
	}
	if res.Status != store.CliAuthDenied {
		t.Errorf("poll status got %q want denied", res.Status)
	}
	if res.APIKey != "" {
		t.Errorf("denied 不应返回 Key")
	}

	// denied 后再 approve → ErrAlreadyTerminal
	if err := m.Approve(created.SessionID, userID, []string{ScopeNotifySend}); !errors.Is(err, ErrAlreadyTerminal) {
		t.Errorf("denied 后 approve 应 ErrAlreadyTerminal, got %v", err)
	}
}

func TestCliAuth_OneTimeConsumption(t *testing.T) {
	db, userID := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	created, _ := m.CreateSession("dev3", []string{ScopeNotifySend})
	m.Approve(created.SessionID, userID, []string{ScopeNotifySend})

	// 第一次 poll 拿到 Key
	res1, _ := m.Poll(created.SessionID, created.Secret)
	if res1.APIKey == "" {
		t.Fatalf("首次 poll 应返回 Key")
	}

	// 第二次 poll 不再返回 Key
	res2, err := m.Poll(created.SessionID, created.Secret)
	if err != nil {
		t.Fatalf("二次 poll: %v", err)
	}
	if res2.APIKey != "" {
		t.Errorf("二次 poll 不应返回 Key, got %q", res2.APIKey)
	}
	if res2.Status != store.CliAuthConsumed {
		t.Errorf("二次 poll status got %q want consumed", res2.Status)
	}

	// DB 中 Key 记录数不增加
	keys, _ := db.ListAPIKeysByUser(userID)
	if len(keys) != 1 {
		t.Errorf("Key 记录数 got %d want 1", len(keys))
	}
}

func TestCliAuth_WrongSecretDoesNotConsume(t *testing.T) {
	db, userID := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	created, _ := m.CreateSession("dev4", []string{ScopeNotifySend})
	m.Approve(created.SessionID, userID, []string{ScopeNotifySend})

	// 错误 secret → ErrUnauthorized，不消费
	_, err := m.Poll(created.SessionID, "wrong-secret-value")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("错 secret 应 ErrUnauthorized, got %v", err)
	}

	// 会话仍 approved（未被消费）
	got, _ := db.GetCliAuthSession(created.SessionID)
	if got.Status != store.CliAuthApproved {
		t.Errorf("错 secret 不应消费会话, status got %q want approved", got.Status)
	}

	// 正确 secret 仍能拿到 Key
	res, err := m.Poll(created.SessionID, created.Secret)
	if err != nil || res.APIKey == "" {
		t.Fatalf("正确 secret 应能拿到 Key: err=%v res=%+v", err, res)
	}
}

func TestCliAuth_CodeNormalization(t *testing.T) {
	db, userID := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	created, _ := m.CreateSession("dev5", []string{ScopeNotifySend})

	// 用带连字符、小写的形式查
	got, err := m.GetByCode(strings.ToLower(FormatUserCode(created.UserCode)))
	if err != nil {
		t.Fatalf("GetByCode with formatted lowercase: %v", err)
	}
	if got.ID != created.SessionID {
		t.Errorf("code lookup 不匹配: got %q want %q", got.ID, created.SessionID)
	}

	// 直接用原始码也能查
	got2, _ := m.GetByCode(created.UserCode)
	if got2.ID != created.SessionID {
		t.Errorf("原始码 lookup 失败")
	}

	// 授权后验证 GetByCode 也能看到状态
	m.Approve(created.SessionID, userID, []string{ScopeNotifySend})
	got3, _ := m.GetByCode(created.UserCode)
	if got3.Status != store.CliAuthApproved {
		t.Errorf("GetByCode status got %q want approved", got3.Status)
	}
}

func TestCliAuth_TTLExpiry(t *testing.T) {
	db, userID := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Unix(1000000, 0)}
	m := newCliAuthManagerWithClock(db, clock)

	created, _ := m.CreateSession("dev6", []string{ScopeNotifySend})

	// 未过期时 approve 成功
	if err := m.Approve(created.SessionID, userID, []string{ScopeNotifySend}); err != nil {
		t.Fatalf("approve before expiry: %v", err)
	}

	// 推进时钟到过期后
	clock.advance(11 * time.Minute)

	// poll 应收到 expired（approved 但过期）
	res, err := m.Poll(created.SessionID, created.Secret)
	if err != nil {
		t.Fatalf("poll expired: %v", err)
	}
	if res.Status != store.CliAuthExpired {
		t.Errorf("poll status got %q want expired", res.Status)
	}
	if res.APIKey != "" {
		t.Errorf("过期会话不应返回 Key")
	}

	// 过期后 approve 被拒
	created2, _ := m.CreateSession("dev7", []string{ScopeNotifySend})
	clock.advance(11 * time.Minute)
	if err := m.Approve(created2.SessionID, userID, []string{ScopeNotifySend}); !errors.Is(err, ErrAlreadyTerminal) {
		t.Errorf("过期后 approve 应 ErrAlreadyTerminal, got %v", err)
	}

	// GetByID 返回 expired
	got, _ := m.GetByID(created2.SessionID)
	if got.Status != store.CliAuthExpired {
		t.Errorf("GetByID status got %q want expired", got.Status)
	}
}

func TestCliAuth_ScopeSubsetValidation(t *testing.T) {
	db, userID := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	// 申请 send + receive
	created, _ := m.CreateSession("dev8", []string{ScopeNotifySend, ScopeNotifyReceive})

	// 批准时减量（只 send）→ 成功
	if err := m.Approve(created.SessionID, userID, []string{ScopeNotifySend}); err != nil {
		t.Fatalf("scope 减量应成功: %v", err)
	}

	// 领证的 Key scope 只有 send
	res, _ := m.Poll(created.SessionID, created.Secret)
	if len(res.Scopes) != 1 || res.Scopes[0] != ScopeNotifySend {
		t.Errorf("Key scope got %v want [notify:send]", res.Scopes)
	}

	// 未申请的 scope 被拒
	created2, _ := m.CreateSession("dev9", []string{ScopeNotifySend})
	if err := m.Approve(created2.SessionID, userID, []string{ScopeDevicesRead}); !errors.Is(err, ErrInvalidParam) {
		t.Errorf("未申请 scope 应 ErrInvalidParam, got %v", err)
	}

	// 空 grantedScopes 被拒
	created3, _ := m.CreateSession("dev10", []string{ScopeNotifySend})
	if err := m.Approve(created3.SessionID, userID, []string{}); !errors.Is(err, ErrInvalidParam) {
		t.Errorf("空 grantedScopes 应 ErrInvalidParam, got %v", err)
	}
}

func TestCliAuth_KeyUsableAfterMint(t *testing.T) {
	// 端到端：建会话→批准→领证→Key 可用（用 KeyManager.ValidateKey 验证）
	db, userID := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	created, _ := m.CreateSession("dev11", []string{ScopeNotifySend})
	m.Approve(created.SessionID, userID, []string{ScopeNotifySend})
	res, _ := m.Poll(created.SessionID, created.Secret)

	// 用 KeyManager 校验领到的 Key
	km := NewKeyManager(db)
	gotUser, gotScopes, err := km.ValidateKey(res.APIKey)
	if err != nil {
		t.Fatalf("validate key: %v", err)
	}
	if gotUser != userID {
		t.Errorf("userID got %q want %q", gotUser, userID)
	}
	if !HasScope(gotScopes, ScopeNotifySend) {
		t.Errorf("scope 应含 notify:send, got %v", gotScopes)
	}
}

func TestCliAuth_PollNotFound(t *testing.T) {
	db, _ := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	_, err := m.Poll("cas_ghost", "any-secret")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("poll 不存在应 ErrNotFound, got %v", err)
	}
}

func TestNormalizeUserCode(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"k7qx3f9m", "K7QX3F9M"},
		{"K7QX-3F9M", "K7QX3F9M"},
		{" k7qx 3f9m ", "K7QX3F9M"},
		{"K7QX 3F9M", "K7QX3F9M"},
	}
	for _, tc := range cases {
		got := NormalizeUserCode(tc.in)
		if got != tc.want {
			t.Errorf("NormalizeUserCode(%q) got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatUserCode(t *testing.T) {
	if got := FormatUserCode("K7QX3F9M"); got != "K7QX-3F9M" {
		t.Errorf("FormatUserCode got %q want K7QX-3F9M", got)
	}
	if got := FormatUserCode("k7qx-3f9m"); got != "K7QX-3F9M" {
		t.Errorf("FormatUserCode lowercase got %q want K7QX-3F9M", got)
	}
}

// TestCliAuth_UserCodeUniqueRetry 验证短码 UNIQUE 冲突时重试成功。
// 通过注入 codeGen 让前两次返回已占用的码，第三次返回新码，验证重试逻辑生效。
func TestCliAuth_UserCodeUniqueRetry(t *testing.T) {
	db, _ := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	// 先插入一个会话占据码 "COLLIDE1"
	pre := &store.CliAuthSession{
		ID:              "cas_preexist",
		SecretHash:      "hash",
		UserCode:        "COLLIDE1",
		DeviceName:      "pre",
		ScopesRequested: []string{ScopeNotifySend},
		Status:          store.CliAuthPending,
		CreatedAt:       1000,
		ExpiresAt:       1600,
	}
	if err := db.CreateCliAuthSession(pre); err != nil {
		t.Fatalf("seed pre-existing session: %v", err)
	}

	// 注入 codeGen：前两次返回已占用的码，第三次返回唯一码
	calls := 0
	m.SetCodeGenerator(func() (string, error) {
		calls++
		if calls <= 2 {
			return "COLLIDE1", nil // 已被占用
		}
		return "UNIQUE000", nil
	})

	created, err := m.CreateSession("retry-test", []string{ScopeNotifySend})
	if err != nil {
		t.Fatalf("CreateSession with collision retry: %v", err)
	}
	if created.UserCode != "UNIQUE000" {
		t.Errorf("UserCode got %q want UNIQUE000（第三次重试成功）", created.UserCode)
	}
	if calls != 3 {
		t.Errorf("codeGen 调用次数 got %d want 3（前两次冲突，第三次成功）", calls)
	}
}

// TestCliAuth_UserCodeRetryExhausted 验证连续 5 次冲突后报错。
func TestCliAuth_UserCodeRetryExhausted(t *testing.T) {
	db, _ := newCliAuthTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newCliAuthManagerWithClock(db, clock)

	// 注入永远冲突的 codeGen
	m.SetCodeGenerator(func() (string, error) {
		return "ALWAYSDUP", nil
	})

	// 先插入一个占据 ALWAYSDUP 的会话
	pre := &store.CliAuthSession{
		ID: "cas_block", SecretHash: "h", UserCode: "ALWAYSDUP",
		DeviceName: "block", ScopesRequested: []string{ScopeNotifySend},
		Status: store.CliAuthPending, CreatedAt: 1000, ExpiresAt: 1600,
	}
	db.CreateCliAuthSession(pre)

	_, err := m.CreateSession("exhaust-test", []string{ScopeNotifySend})
	if err == nil {
		t.Fatal("连续冲突应报错，但 CreateSession 成功了")
	}
	if !strings.Contains(err.Error(), "冲突超过重试上限") {
		t.Errorf("错误信息应含「冲突超过重试上限」, got %v", err)
	}
}
