package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// newEnrollTestDB 打开内存 DB 并建用户。
func newEnrollTestDB(t *testing.T) (*store.DB, *Service, string) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc, err := NewService(db, Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	u := &store.User{ID: store.NewUserID(), Username: "enrolluser", DisplayName: "Enroll User", CreatedAt: store.Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return db, svc, u.ID
}

func newEnrollMgrWithClock(db *store.DB, svc *Service, clock *fakeClock) *PasskeyEnrollManager {
	m := NewPasskeyEnrollManager(db, svc, 10*time.Minute)
	m.SetClock(clock.now)
	return m
}

// TestEnroll_CreateSession_Success 建会话成功。
func TestEnroll_CreateSession_Success(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Unix(1000000, 0)}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, err := m.CreateSession(uid, "my-iphone")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !startsWith(created.SessionID, "cas_") {
		t.Errorf("SessionID 前缀错误: %q", created.SessionID)
	}
	if created.Secret == "" {
		t.Error("Secret 不应为空")
	}
	if len(created.UserCode) != 8 {
		t.Errorf("UserCode 长度 got %d want 8", len(created.UserCode))
	}
	if created.ExpiresAt != 1000000+600 {
		t.Errorf("ExpiresAt got %d want %d", created.ExpiresAt, 1000000+600)
	}

	// 库中 kind=passkey
	got, _ := db.GetCliAuthSession(created.SessionID)
	if got.Kind != store.CliAuthKindPasskey {
		t.Errorf("Kind got %q want %q", got.Kind, store.CliAuthKindPasskey)
	}
	if got.Status != store.CliAuthPending {
		t.Errorf("status got %q want pending", got.Status)
	}
	// 库中不存明文 secret
	if got.SecretHash == created.Secret {
		t.Error("库中不应存明文 secret")
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// TestEnroll_CreateSession_InvalidParams 参数校验。
func TestEnroll_CreateSession_InvalidParams(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	cases := []struct {
		name       string
		deviceName string
	}{
		{"空设备名", ""},
		{"超长设备名", string(make([]byte, 65))},
	}
	for _, c := range cases {
		_, err := m.CreateSession(uid, c.deviceName)
		if err == nil {
			t.Errorf("%s: 应报错", c.name)
		}
	}
}

// TestEnroll_RequestKnock_Success 敲门 pending→requested。
func TestEnroll_RequestKnock_Success(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")

	secret, err := m.RequestKnock(created.SessionID, "Chrome · macOS")
	if err != nil {
		t.Fatalf("request knock: %v", err)
	}
	if secret == "" {
		t.Error("敲门应返回 secret")
	}
	// 敲门后的 secret 应与建会话时的不同
	if secret == created.Secret {
		t.Error("敲门 secret 不应与建会话 secret 相同")
	}

	// 验证状态
	got, _ := db.GetCliAuthSession(created.SessionID)
	if got.Status != store.CliAuthRequested {
		t.Errorf("status got %q want requested", got.Status)
	}
	if got.DeviceHint != "Chrome · macOS" {
		t.Errorf("deviceHint got %q want %q", got.DeviceHint, "Chrome · macOS")
	}
}

// TestEnroll_RequestKnock_WrongState 非 pending 态敲门失败。
func TestEnroll_RequestKnock_WrongState(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")
	// 先敲门
	_, _ = m.RequestKnock(created.SessionID, "Chrome")
	// 再敲门 → 应失败
	_, err := m.RequestKnock(created.SessionID, "Chrome")
	if err == nil {
		t.Error("requested 态敲门应失败")
	}
}

// TestEnroll_Approve_Success 批准 requested→approved。
func TestEnroll_Approve_Success(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")
	_, _ = m.RequestKnock(created.SessionID, "Chrome")

	err := m.Approve(created.SessionID, uid)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	got, _ := db.GetCliAuthSession(created.SessionID)
	if got.Status != store.CliAuthApproved {
		t.Errorf("status got %q want approved", got.Status)
	}
	if got.UserID != uid {
		t.Errorf("UserID got %q want %q", got.UserID, uid)
	}
}

// TestEnroll_Approve_WrongState 非 requested 态批准失败。
func TestEnroll_Approve_WrongState(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")
	// 未敲门直接批准 → 应失败
	err := m.Approve(created.SessionID, uid)
	if err == nil {
		t.Error("pending 态批准应失败")
	}
}

// TestEnroll_Deny_Success 拒绝 requested→denied。
func TestEnroll_Deny_Success(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")
	_, _ = m.RequestKnock(created.SessionID, "Chrome")

	err := m.Deny(created.SessionID)
	if err != nil {
		t.Fatalf("deny: %v", err)
	}

	got, _ := db.GetCliAuthSession(created.SessionID)
	if got.Status != store.CliAuthDenied {
		t.Errorf("status got %q want denied", got.Status)
	}
}

// TestEnroll_Poll_StatusFlow 轮询状态流转。
func TestEnroll_Poll_StatusFlow(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")

	// 敲门前 poll → 用建会话的 secret（敲门前 secret_hash 还是建会话时的）
	// 注意：敲门前 poll 用的是建会话时返回的 secret
	res, err := m.Poll(created.SessionID, created.Secret)
	if err != nil {
		t.Fatalf("poll pending: %v", err)
	}
	if res.Status != store.CliAuthPending {
		t.Errorf("poll pending: status got %q want pending", res.Status)
	}

	// 敲门后用新 secret poll
	knockSecret, _ := m.RequestKnock(created.SessionID, "Chrome")
	res2, err := m.Poll(created.SessionID, knockSecret)
	if err != nil {
		t.Fatalf("poll requested: %v", err)
	}
	if res2.Status != store.CliAuthRequested {
		t.Errorf("poll requested: status got %q want requested", res2.Status)
	}
}

// TestEnroll_Poll_WrongSecret 错 secret → 401。
func TestEnroll_Poll_WrongSecret(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")

	_, err := m.Poll(created.SessionID, "wrong-secret")
	if err != ErrUnauthorized {
		t.Errorf("错 secret 应返回 ErrUnauthorized, got %v", err)
	}
}

// TestEnroll_Expired 过期迁移。
func TestEnroll_Expired(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Unix(1000000, 0)}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")
	// 推进时间超过 TTL
	clock.advance(11 * time.Minute)

	// GetByID 应触发惰性过期
	s, err := m.GetByID(created.SessionID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if s.Status != store.CliAuthExpired {
		t.Errorf("status got %q want expired", s.Status)
	}
}

// TestEnroll_KindGuard 非 passkey kind 会话操作被拒。
func TestEnroll_KindGuard(t *testing.T) {
	db, svc, _ := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	// 直接插入一个 apikey-kind 的会话
	sess := &store.CliAuthSession{
		ID:         store.NewCliAuthID(),
		SecretHash: "fakehash",
		UserCode:   "TESTCODE",
		DeviceName: "test",
		Kind:       store.CliAuthKindAPIKey,
		Status:     store.CliAuthPending,
		CreatedAt:  store.Now(),
		ExpiresAt:  store.Now() + 600,
	}
	if err := db.CreateCliAuthSession(sess); err != nil {
		t.Fatalf("create apikey session: %v", err)
	}

	// 敲门应失败
	_, err := m.RequestKnock(sess.ID, "Chrome")
	if err == nil {
		t.Error("apikey-kind 会话敲门应失败")
	}
}

// TestEnroll_Delete 删除会话。
func TestEnroll_Delete(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")
	if err := m.Delete(created.SessionID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// 确认已删除
	_, err := db.GetCliAuthSession(created.SessionID)
	if err != store.ErrNotFound {
		t.Errorf("删除后应返回 ErrNotFound, got %v", err)
	}
}

// TestEnroll_Poll_DeniedStatus 拒绝后 poll 返回 denied。
func TestEnroll_Poll_DeniedStatus(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Now()}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, _ := m.CreateSession(uid, "old-mac")
	knockSecret, _ := m.RequestKnock(created.SessionID, "Chrome")
	_ = m.Deny(created.SessionID)

	res, err := m.Poll(created.SessionID, knockSecret)
	if err != nil {
		t.Fatalf("poll denied: %v", err)
	}
	if res.Status != store.CliAuthDenied {
		t.Errorf("status got %q want denied", res.Status)
	}
}

// ---- issue #338 / F7：会话与 registrationToken 的**一次性**语义 ----
//
// 语义确认（enroll.go:330-370 + webauthn.go storeEnrollToken）：
//   - approved 态 poll **不** consume 会话，只下发 attestationOptions + registrationToken，
//     且**每轮 poll 重新生成** token 并覆盖库里那份 hash（旧 token 立刻失效）；
//   - consume（approved→consumed）发生在 Complete 成功路径的最后一步，是原子的；
//   - 已 consumed 的会话再 Complete → ErrAlreadyConsumed（不会建第二个凭证）。
// 下面两条把这两层一次性都钉死，不再靠「名字声称重放被拒、实际断言仍 approved」的空壳用例。

// TestEnroll_CompleteRejectedAfterConsumed 钉死会话一次性：approved→consumed 只成功一次，
// 同一个 registrationToken + 已 consumed 的会话再调 Complete 必须 ErrAlreadyConsumed。
func TestEnroll_CompleteRejectedAfterConsumed(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Unix(1000000, 0)}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, err := m.CreateSession(uid, "old-mac")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := m.RequestKnock(created.SessionID, "Chrome"); err != nil {
		t.Fatalf("request knock: %v", err)
	}
	if err := m.Approve(created.SessionID, uid); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// 与 poll 下发同形的 registrationToken（明文只给调用方，库里只放 sha256）。
	token, tokenHash, err := generateSecret()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	svc.storeEnrollToken(created.SessionID, tokenHash)

	// 第一次消费成功（complete 成功路径的最后一步就是这句）。
	already, err := db.ConsumeCliAuthSessionPasskey(created.SessionID, clock.now().Unix())
	if err != nil {
		t.Fatalf("第一次消费报错: %v", err)
	}
	if already {
		t.Fatalf("第一次消费不该是 alreadyConsumed（会话应在 approved 态）")
	}

	// 第二次：同一个 token 打同一个已 consumed 的会话 → ErrAlreadyConsumed。
	if _, err := m.Complete(created.SessionID, token, "new-device",
		httptest.NewRequest(http.MethodPost, "/", nil)); !errors.Is(err, ErrAlreadyConsumed) {
		t.Fatalf("已 consumed 的会话再 complete 应 ErrAlreadyConsumed, got %v", err)
	}

	// 终态保持 consumed，consumed_at 不被二次写入覆盖。
	got, err := db.GetCliAuthSession(created.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != store.CliAuthConsumed {
		t.Errorf("status got %q want consumed", got.Status)
	}
	if !got.ConsumedAt.Valid || got.ConsumedAt.Int64 != clock.now().Unix() {
		t.Errorf("consumed_at got %+v want %d", got.ConsumedAt, clock.now().Unix())
	}
}

// TestEnroll_RegistrationTokenSupersededByNextPoll 钉死 registrationToken 一次性：每轮
// approved 态 poll 都重新生成 token（覆盖旧 hash），旧 token 立刻失效（Complete → 401），
// 且这个失败不消耗会话（仍 approved，可用最新 token 重试）。
func TestEnroll_RegistrationTokenSupersededByNextPoll(t *testing.T) {
	db, svc, uid := newEnrollTestDB(t)
	clock := &fakeClock{t: time.Unix(1000000, 0)}
	m := newEnrollMgrWithClock(db, svc, clock)

	created, err := m.CreateSession(uid, "old-mac")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	knockSecret, err := m.RequestKnock(created.SessionID, "Chrome")
	if err != nil {
		t.Fatalf("request knock: %v", err)
	}
	if err := m.Approve(created.SessionID, uid); err != nil {
		t.Fatalf("approve: %v", err)
	}

	first, err := m.Poll(created.SessionID, knockSecret)
	if err != nil {
		t.Fatalf("第一次 poll: %v", err)
	}
	if first.Status != store.CliAuthApproved || first.RegistrationToken == "" {
		t.Fatalf("第一次 poll 应 approved + 带 registrationToken: %+v", first)
	}

	second, err := m.Poll(created.SessionID, knockSecret)
	if err != nil {
		t.Fatalf("第二次 poll: %v", err)
	}
	if second.RegistrationToken == "" {
		t.Fatal("第二次 poll 也应下发 registrationToken")
	}
	if second.RegistrationToken == first.RegistrationToken {
		t.Fatal("第二次 poll 应重新生成 registrationToken（否则 token 永远可重放）")
	}

	// 旧 token 失效：Complete 在 token 校验这一步就拒（ErrUnauthorized）。
	if _, err := m.Complete(created.SessionID, first.RegistrationToken, "new-device",
		httptest.NewRequest(http.MethodPost, "/", nil)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("被覆盖的旧 registrationToken 应 ErrUnauthorized, got %v", err)
	}
	// 失败不消耗会话。
	if got, err := db.GetCliAuthSession(created.SessionID); err != nil || got.Status != store.CliAuthApproved {
		t.Fatalf("token 校验失败不应消耗会话: status=%v err=%v", got, err)
	}

	// 最新 token 能过 token 校验（继续走到 WebAuthn attestation 校验才失败）：
	// 空 body 必然解析失败，但错误**不再是** ErrUnauthorized——一次性只对旧 token 生效。
	_, err = m.Complete(created.SessionID, second.RegistrationToken, "new-device",
		httptest.NewRequest(http.MethodPost, "/", nil))
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("最新 registrationToken 不该被判 unauthorized: %v", err)
	}
	if err == nil {
		t.Fatal("伪造/缺失 attestation 不应 complete 成功")
	}
}
