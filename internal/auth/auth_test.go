package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/anotify/anotify/internal/store"
)

// newTestDB 打开一个内存 SQLite 并建表。
func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newTestService 构造一个内存后端的 Service。
func newTestService(t *testing.T) *Service {
	t.Helper()
	db := newTestDB(t)
	svc, err := NewService(db, Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// ---------- API Key ----------

func TestAPIKey_CreateValidate_RoundTrip(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "alice", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	plaintext, rec, err := svc.Keys().CreateKey(user.ID, "ci-key", []string{ScopeNotifySend})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if !strings.HasPrefix(plaintext, "ant_send_") {
		t.Errorf("明文 Key 前缀错误: %q", plaintext)
	}
	if rec.KeyHash == plaintext {
		t.Errorf("哈希不应等于明文")
	}
	if !strings.HasPrefix(rec.KeyHash, "$argon2id$") {
		t.Errorf("哈希应为 argon2id PHC 格式: %q", rec.KeyHash)
	}

	gotUser, gotScopes, err := svc.Keys().ValidateKey(plaintext)
	if err != nil {
		t.Fatalf("validate key: %v", err)
	}
	if gotUser != user.ID {
		t.Errorf("userID 不匹配: got %q want %q", gotUser, user.ID)
	}
	if !HasScope(gotScopes, ScopeNotifySend) {
		t.Errorf("应包含 notify:send scope, got %v", gotScopes)
	}
}

func TestAPIKey_ScopeLabels(t *testing.T) {
	cases := []struct {
		scopes []string
		want   string
	}{
		{[]string{ScopeNotifySend}, "ant_send_"},
		{[]string{ScopeNotifyReceive}, "ant_recv_"},
		{[]string{ScopeNotifySend, ScopeNotifyReceive}, "ant_full_"},
		{[]string{ScopeDevicesRead}, "ant_key_"},
	}
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "bob", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	for _, c := range cases {
		plaintext, _, err := svc.Keys().CreateKey(user.ID, "k", c.scopes)
		if err != nil {
			t.Fatalf("create key %v: %v", c.scopes, err)
		}
		if !strings.HasPrefix(plaintext, c.want) {
			t.Errorf("scopes %v: 前缀 got %q want %q", c.scopes, plaintext, c.want)
		}
	}
}

func TestAPIKey_WrongKeyRejected(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "carol", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	plaintext, _, err := svc.Keys().CreateKey(user.ID, "k", []string{ScopeNotifySend})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	// 篡改最后一位（确保与原值不同，避免 1/64 概率 flake）。
	last := plaintext[len(plaintext)-1]
	replacement := byte('X')
	if last == 'X' {
		replacement = 'Y'
	}
	tampered := plaintext[:len(plaintext)-1] + string(replacement)
	if _, _, err := svc.Keys().ValidateKey(tampered); err == nil {
		t.Errorf("篡改的 Key 应被拒绝")
	}
	// 完全错误的前缀。
	if _, _, err := svc.Keys().ValidateKey("not_a_key"); err == nil {
		t.Errorf("非法 Key 应被拒绝")
	}
	// 不存在的 Key。
	if _, _, err := svc.Keys().ValidateKey("ant_send_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Errorf("不存在的 Key 应被拒绝")
	}
}

func TestAPIKey_RevokedKeyRejected(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "dave", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	plaintext, rec, err := svc.Keys().CreateKey(user.ID, "k", []string{ScopeNotifySend})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := svc.db.RevokeAPIKey(rec.ID); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	if _, _, err := svc.Keys().ValidateKey(plaintext); err == nil {
		t.Errorf("已停用的 Key 应被拒绝")
	}
}

func TestAPIKey_UnknownScopeRejected(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "erin", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, _, err := svc.Keys().CreateKey(user.ID, "k", []string{"bogus:scope"}); err == nil {
		t.Errorf("未知 scope 应被拒绝")
	}
	if _, _, err := svc.Keys().CreateKey(user.ID, "k", nil); err == nil {
		t.Errorf("空 scope 应被拒绝")
	}
}

// ---------- RequireScope 中间件 ----------

// statusRecorder 记录 handler 是否被调用。
func scopeProtectedHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequireScope(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "frank", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sendKey, _, err := svc.Keys().CreateKey(user.ID, "sender", []string{ScopeNotifySend})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	cases := []struct {
		name       string
		authHeader string
		wantStatus int
		wantCalled bool
		wantUserID string // 仅在 wantCalled 时校验
	}{
		{"有效 send key", "Bearer " + sendKey, http.StatusOK, true, user.ID},
		{"缺少 Authorization", "", http.StatusUnauthorized, false, ""},
		{"非 Bearer 格式", "Basic abc", http.StatusUnauthorized, false, ""},
		{"错误 key", "Bearer ant_send_wrongwrongwrong", http.StatusUnauthorized, false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			called := false
			var gotUserID string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				gotUserID = UserIDFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			h := svc.Keys().RequireScope(ScopeNotifySend)(inner)
			req := httptest.NewRequest(http.MethodPost, "/v1/notify", nil)
			if c.authHeader != "" {
				req.Header.Set("Authorization", c.authHeader)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantStatus {
				t.Errorf("status got %d want %d (body=%s)", rec.Code, c.wantStatus, rec.Body.String())
			}
			if called != c.wantCalled {
				t.Errorf("handler called got %v want %v", called, c.wantCalled)
			}
			if c.wantCalled && gotUserID != c.wantUserID {
				t.Errorf("context userID got %q want %q", gotUserID, c.wantUserID)
			}
		})
	}
}

// scope 不足时应 403：用 send key 访问 receive 接口。
func TestRequireScope_InsufficientScopeForbidden(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "grace", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sendOnly, _, err := svc.Keys().CreateKey(user.ID, "sender", []string{ScopeNotifySend})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	h := svc.Keys().RequireScope(ScopeNotifyReceive)(scopeProtectedHandler(new(bool)))
	req := httptest.NewRequest(http.MethodGet, "/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+sendOnly)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("scope 不足应 403, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// ---------- 会话 ----------

func TestSession_CreateValidateRevoke(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "henry", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	sess, err := svc.Sessions().Create(user.ID, "Test · macOS")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err := svc.Sessions().Validate(sess.ID)
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	if got.UserID != user.ID {
		t.Errorf("session userID got %q want %q", got.UserID, user.ID)
	}

	// 吊销后失效。
	if err := svc.Sessions().Revoke(sess.ID); err != nil {
		t.Fatalf("revoke session: %v", err)
	}
	if _, err := svc.Sessions().Validate(sess.ID); err == nil {
		t.Errorf("已吊销会话应无效")
	}
}

func TestSession_Expiry(t *testing.T) {
	db := newTestDB(t)
	// TTL 极短，构造后立即过期。
	svc, err := NewService(db, Config{
		RPDisplayName: "T", RPID: "localhost", RPOrigins: []string{"http://localhost"},
		SessionTTL: -1 * time.Hour, // 负 TTL → 立即过期
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	user := &store.User{ID: store.NewUserID(), Username: "ivan", CreatedAt: store.Now()}
	if err := db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	// 负 TTL 会被 NewSessionManager 归一化为默认，改为手动构造一个过期会话直接入库。
	expired := &store.Session{
		ID:        store.NewSessionID() + ".tok",
		UserID:    user.ID,
		CreatedAt: store.Now() - 7200,
		ExpiresAt: store.Now() - 3600, // 1 小时前过期
		LastSeen:  store.Now() - 7200,
	}
	if err := db.CreateSession(expired); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if _, err := svc.Sessions().Validate(expired.ID); err == nil {
		t.Errorf("过期会话应无效")
	}
}

func TestSession_Middleware(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "judy", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sess, err := svc.Sessions().Create(user.ID, "Test · macOS")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	var gotUserID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserID = UserIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := svc.Sessions().Middleware(inner)

	// 无 Cookie → 401。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无 Cookie 应 401, got %d", rec.Code)
	}

	// 有效 Cookie → 200 且注入 userID。
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.ID})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("有效 Cookie 应 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if gotUserID != user.ID {
		t.Errorf("context userID got %q want %q", gotUserID, user.ID)
	}

	// 无效 Cookie → 401。
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "sess_bogus.tok"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无效 Cookie 应 401, got %d", rec.Code)
	}
}

// ---------- challenge 存取 ----------

func TestChallenge_StoreTakeOnce(t *testing.T) {
	svc := newTestService(t)
	data := fakeSessionData("user1")
	svc.storeChallenge(regKey("user1"), data)

	// 第一次取成功。
	if _, err := svc.takeChallenge(regKey("user1")); err != nil {
		t.Fatalf("take challenge: %v", err)
	}
	// 第二次取失败（一次性）。
	if _, err := svc.takeChallenge(regKey("user1")); err == nil {
		t.Errorf("challenge 应一次性，二次取用应失败")
	}
}

func TestChallenge_Expiry(t *testing.T) {
	svc := newTestService(t)
	data := fakeSessionData("user2")
	svc.storeChallenge(loginKey("user2"), data)
	// 手动把 challenge 置为已过期。
	svc.mu.Lock()
	svc.challenges[loginKey("user2")].expiresAt = time.Now().Add(-time.Minute)
	svc.mu.Unlock()
	if _, err := svc.takeChallenge(loginKey("user2")); err == nil {
		t.Errorf("过期 challenge 应失败")
	}
}

func TestChallenge_NamespaceIsolation(t *testing.T) {
	svc := newTestService(t)
	svc.storeChallenge(regKey("u"), fakeSessionData("u"))
	// 不同命名空间不应命中。
	if _, err := svc.takeChallenge(loginKey("u")); err == nil {
		t.Errorf("reg: 的 challenge 不应被 login: 取出")
	}
}

// ---------- 注册前置校验 ----------

func TestBeginRegister_DuplicateUsername(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "taken", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.BeginRegister("taken", "Taken"); err == nil {
		t.Errorf("已存在的用户名注册应失败")
	}
}

func TestBeginRegister_ReturnsOptions(t *testing.T) {
	svc := newTestService(t)
	creation, err := svc.BeginRegister("newuser", "New User")
	if err != nil {
		t.Fatalf("begin register: %v", err)
	}
	if creation == nil {
		t.Fatalf("creation 不应为 nil")
	}
	// challenge 应已暂存。
	if _, err := svc.takeChallenge(regKey("newuser")); err != nil {
		t.Errorf("注册后 challenge 应已暂存: %v", err)
	}
}

func TestBeginLogin_UnknownUser(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.BeginLogin("ghost")
	if err == nil {
		t.Fatalf("不存在的用户登录应失败")
	}
	if !strings.Contains(err.Error(), "用户不存在") {
		t.Errorf("未注册用户应提示「用户不存在」, got %q", err.Error())
	}
}

// ---------- credid 编解码 ----------

func TestCredID_RoundTrip(t *testing.T) {
	raw := []byte{0x01, 0x02, 0xff, 0x10, 0xAB}
	enc := encodeCredID(raw)
	dec, err := decodeCredID(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(dec) != string(raw) {
		t.Errorf("round trip 不匹配: got %v want %v", dec, raw)
	}
	if _, err := decodeCredID("!!!invalid!!!"); err == nil {
		t.Errorf("非法 base64url 应报错")
	}
}

// ---------- UserIDFromContext ----------

func TestUserIDFromContext(t *testing.T) {
	if got := UserIDFromContext(t.Context()); got != "" {
		t.Errorf("空 context 应返回空串, got %q", got)
	}
}

// 辅助：构造一个最小的 SessionData 用于 challenge 测试。
func fakeSessionData(userID string) webauthn.SessionData {
	return webauthn.SessionData{
		Challenge: "test-challenge-" + userID,
		UserID:    []byte(userID),
		Expires:   time.Now().Add(5 * time.Minute),
	}
}

// ---------- 可发现登录 userHandle 失配（孤儿 Passkey）----------

// TestLookupUserByHandle_StaleUserHandle 模拟认证器残留过期 Passkey：
// userHandle 指向一个已不存在的用户（被删/重建后 user ID 变了）。
// 应返回中性文案（「未关联到任何账户」），不报「用户不存在」。
func TestLookupUserByHandle_StaleUserHandle(t *testing.T) {
	svc := newTestService(t)

	// 用一个库里不存在的 userHandle（模拟孤儿凭证）
	staleHandle := []byte("deleted-user-id-12345")
	_, err := svc.lookupUserByHandle(staleHandle)
	if err == nil {
		t.Fatalf("孤儿 userHandle 应返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "未关联到任何账户") {
		t.Errorf("应提示「未关联到任何账户」, got %q", msg)
	}
	if strings.Contains(msg, "用户不存在") {
		t.Errorf("不应提示「用户不存在」（会误导用户），got %q", msg)
	}
}

// TestLookupUserByHandle_ValidUser 正常 userHandle 应返回非 nil 的 webauthn.User。
func TestLookupUserByHandle_ValidUser(t *testing.T) {
	svc := newTestService(t)
	user := &store.User{ID: store.NewUserID(), Username: "alice", CreatedAt: store.Now()}
	if err := svc.db.CreateUser(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	waUser, err := svc.lookupUserByHandle([]byte(user.ID))
	if err != nil {
		t.Fatalf("正常 userHandle 不应报错: %v", err)
	}
	if waUser == nil {
		t.Errorf("应返回非 nil 的 webauthn.User")
	}
	if string(waUser.WebAuthnID()) != user.ID {
		t.Errorf("WebAuthnID 不匹配: got %q want %q", waUser.WebAuthnID(), user.ID)
	}
}

// ---------- userHandlePrefix ----------

func TestUserHandlePrefix(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, ""},
		{"short", []byte{0xab}, "ab"},                          // 1 byte = 2 hex chars < 8
		{"exact4", []byte{0xde, 0xad, 0xbe, 0xef}, "deadbeef"}, // 4 bytes = 8 hex
		{"long", []byte("abcdefghij"), "61626364"},             // first 4 bytes hex
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := userHandlePrefix(tt.in)
			if got != tt.want {
				t.Errorf("userHandlePrefix(%v) = %q, want %q", tt.in, got, tt.want)
			}
			if len(got) > 8 {
				t.Errorf("前缀不应超过 8 字符, got %d", len(got))
			}
		})
	}
}
