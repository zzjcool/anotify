package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/store"
)

// enrollTestEnv 装配一个 Passkey 设备授权测试环境。
type enrollTestEnv struct {
	mux    http.Handler
	mgr    *auth.PasskeyEnrollManager
	db     *store.DB
	svc    *auth.Service
	userID string
	cookie *http.Cookie
}

func newEnrollTestEnv(t *testing.T) *enrollTestEnv {
	t.Helper()
	// 重置全局限速器
	rlEnrollLookup = newFixedWindow(rlEnrollLookupPerMin, time.Minute)
	rlEnrollKnock = newFixedWindow(rlEnrollKnockPerMin, time.Minute)
	rlEnrollComplete = newFixedWindow(rlEnrollCompletePerMin, time.Minute)
	pollG = &pollGuard{last: make(map[string]time.Time)}

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	uid, err := db.InsertUser(context.Background(), "", "enrolluser", "Enroll User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	mgr := auth.NewPasskeyEnrollManager(db, svc, 0)
	sess, err := svc.Sessions().Create(uid, "Test · macOS")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID}

	enrollH := &passkeyEnrollHandler{mgr: mgr, db: db}
	sessMW := svc.Sessions().Middleware

	mux := http.NewServeMux()
	mux.Handle("POST /v1/passkey-enroll/sessions", noStore(sessMW(http.HandlerFunc(enrollH.create))))
	mux.Handle("GET /v1/passkey-enroll/sessions/by-code", noStore(http.HandlerFunc(enrollH.byCode)))
	mux.Handle("GET /v1/passkey-enroll/sessions/{id}", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.lookup(w, r, r.PathValue("id"))
	})))
	mux.Handle("POST /v1/passkey-enroll/sessions/{id}/request", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.knock(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /v1/passkey-enroll/sessions/{id}/poll", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.poll(w, r, r.PathValue("id"))
	})))
	mux.Handle("POST /v1/passkey-enroll/sessions/{id}/complete", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.complete(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /v1/passkey-enroll/sessions/{id}/qr.txt", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.qr(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /v1/passkey-enroll/sessions/{id}/watch", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.watch(w, r, r.PathValue("id"))
	}))))
	mux.Handle("POST /v1/passkey-enroll/sessions/{id}/approve", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.approve(w, r, r.PathValue("id"))
	}))))
	mux.Handle("POST /v1/passkey-enroll/sessions/{id}/deny", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.deny(w, r, r.PathValue("id"))
	}))))
	mux.Handle("DELETE /v1/passkey-enroll/sessions/{id}", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrollH.cancel(w, r, r.PathValue("id"))
	}))))

	return &enrollTestEnv{mux: mux, mgr: mgr, db: db, svc: svc, userID: uid, cookie: cookie}
}

func enrollDoReq(t *testing.T, env *enrollTestEnv, method, path string, body any, withCookie bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if withCookie {
		req.AddCookie(env.cookie)
	}
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	return rr
}

func enrollDecodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	return m
}

type createdEnrollSession struct {
	sessionID string
	secret    string
	userCode  string
	authUrl   string
}

func enrollCreateHelper(t *testing.T, env *enrollTestEnv) createdEnrollSession {
	t.Helper()
	rr := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions",
		map[string]any{"deviceName": "old-macbook"}, true)
	if rr.Code != 200 {
		t.Fatalf("create enroll session status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := enrollDecodeBody(t, rr)
	return createdEnrollSession{
		sessionID: m["sessionId"].(string),
		secret:    m["secret"].(string),
		userCode:  m["userCode"].(string),
		authUrl:   m["authUrl"].(string),
	}
}

// TestEnroll_CreateSession 建会话成功 + 参数校验。
func TestEnroll_CreateSession(t *testing.T) {
	env := newEnrollTestEnv(t)

	// 正常建会话
	rr := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions",
		map[string]any{"deviceName": "old-macbook"}, true)
	if rr.Code != 200 {
		t.Fatalf("建会话 status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := enrollDecodeBody(t, rr)
	if m["sessionId"] == nil || m["sessionId"] == "" {
		t.Fatal("sessionId 为空")
	}
	if m["secret"] == nil || m["secret"] == "" {
		t.Fatal("secret 为空")
	}
	if m["userCode"] == nil {
		t.Fatal("userCode 为空")
	}
	if m["authUrl"] == nil {
		t.Fatal("authUrl 为空")
	}
	if m["kind"] != "passkey" {
		t.Errorf("kind got %v want passkey", m["kind"])
	}
	// authUrl 应含 sessionId
	authURL, _ := m["authUrl"].(string)
	sid, _ := m["sessionId"].(string)
	if !strings.Contains(authURL, sid) {
		t.Errorf("authUrl %q 不含 sessionId %q", authURL, sid)
	}
	// authUrl 应指向 passkey-enroll.html
	if !strings.Contains(authURL, "passkey-enroll.html") {
		t.Errorf("authUrl %q 应含 passkey-enroll.html", authURL)
	}

	// 空 deviceName → 400
	rr2 := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions",
		map[string]any{"deviceName": ""}, true)
	if rr2.Code != 400 {
		t.Errorf("空 deviceName 应 400, got %d", rr2.Code)
	}

	// 未登录 → 401
	rr3 := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions",
		map[string]any{"deviceName": "test"}, false)
	if rr3.Code != 401 {
		t.Errorf("未登录建会话应 401, got %d", rr3.Code)
	}
}

// TestEnroll_AnonLookup 匿名 lookup + by-code。
func TestEnroll_AnonLookup(t *testing.T) {
	env := newEnrollTestEnv(t)
	created := enrollCreateHelper(t, env)

	// 匿名 lookup by ID → 200
	rr := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+created.sessionID, nil, false)
	if rr.Code != 200 {
		t.Fatalf("匿名 lookup status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := enrollDecodeBody(t, rr)
	if m["status"] != "pending" {
		t.Errorf("status=%v want pending", m["status"])
	}
	// 不应含密钥
	if m["secret"] != nil {
		t.Error("匿名 lookup 不应返回 secret")
	}
	if m["userId"] != nil {
		t.Error("匿名 lookup 不应返回 userId")
	}

	// 匿名 by-code → 200
	rr2 := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/by-code?code="+strings.ToLower(created.userCode), nil, false)
	if rr2.Code != 200 {
		t.Fatalf("by-code status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	m2 := enrollDecodeBody(t, rr2)
	if m2["sessionId"] != created.sessionID {
		t.Errorf("sessionId mismatch: %v != %s", m2["sessionId"], created.sessionID)
	}

	// 不存在短码 → 404
	rr3 := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/by-code?code=ZZZZZZZZ", nil, false)
	if rr3.Code != 404 {
		t.Errorf("不存在短码应 404, got %d", rr3.Code)
	}
}

// TestEnroll_Knock 敲门 pending→requested。
func TestEnroll_Knock(t *testing.T) {
	env := newEnrollTestEnv(t)
	created := enrollCreateHelper(t, env)

	// 敲门 → 200 + secret
	rr := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+created.sessionID+"/request",
		map[string]any{"deviceHint": "Chrome · macOS"}, false)
	if rr.Code != 200 {
		t.Fatalf("knock status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := enrollDecodeBody(t, rr)
	knockSecret, _ := m["secret"].(string)
	if knockSecret == "" {
		t.Fatal("敲门应返回 secret")
	}
	// 敲门 secret 不应等于建会话 secret
	if knockSecret == created.secret {
		t.Error("敲门 secret 不应等于建会话 secret")
	}

	// 重复敲门 → 409（已 requested）
	rr2 := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+created.sessionID+"/request",
		map[string]any{"deviceHint": "Chrome"}, false)
	if rr2.Code != 409 {
		t.Errorf("重复敲门应 409, got %d", rr2.Code)
	}
}

// TestEnroll_Watch 旧设备轮询。
func TestEnroll_Watch(t *testing.T) {
	env := newEnrollTestEnv(t)
	created := enrollCreateHelper(t, env)

	// 敲门
	rr := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+created.sessionID+"/request",
		map[string]any{"deviceHint": "Chrome · macOS"}, false)
	if rr.Code != 200 {
		t.Fatalf("knock status=%d", rr.Code)
	}

	// 旧设备 watch → requested + deviceHint
	rr2 := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+created.sessionID+"/watch", nil, true)
	if rr2.Code != 200 {
		t.Fatalf("watch status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	m := enrollDecodeBody(t, rr2)
	if m["status"] != "requested" {
		t.Errorf("status=%v want requested", m["status"])
	}
	if m["deviceHint"] != "Chrome · macOS" {
		t.Errorf("deviceHint=%v want %q", m["deviceHint"], "Chrome · macOS")
	}

	// 未登录 watch → 401
	rr3 := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+created.sessionID+"/watch", nil, false)
	if rr3.Code != 401 {
		t.Errorf("未登录 watch 应 401, got %d", rr3.Code)
	}
}

// TestEnroll_ApproveDeny 批准/拒绝。
func TestEnroll_ApproveDeny(t *testing.T) {
	env := newEnrollTestEnv(t)
	created := enrollCreateHelper(t, env)

	// 先敲门
	_ = enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+created.sessionID+"/request",
		map[string]any{"deviceHint": "Chrome"}, false)

	// 批准 → 200
	rr := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+created.sessionID+"/approve", nil, true)
	if rr.Code != 200 {
		t.Fatalf("approve status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := enrollDecodeBody(t, rr)
	if m["status"] != "approved" {
		t.Errorf("status=%v want approved", m["status"])
	}

	// 重复批准 → 409
	rr2 := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+created.sessionID+"/approve", nil, true)
	if rr2.Code != 409 {
		t.Errorf("重复批准应 409, got %d", rr2.Code)
	}
}

// TestEnroll_ApprovePending 未敲门直接批准失败。
func TestEnroll_ApprovePending(t *testing.T) {
	env := newEnrollTestEnv(t)
	created := enrollCreateHelper(t, env)

	// 未敲门直接批准 → 409
	rr := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+created.sessionID+"/approve", nil, true)
	if rr.Code != 409 {
		t.Errorf("pending 态批准应 409, got %d", rr.Code)
	}
}

// TestEnroll_Poll_StatusPoll 轮询状态。
func TestEnroll_Poll_StatusPoll(t *testing.T) {
	env := newEnrollTestEnv(t)
	created := enrollCreateHelper(t, env)

	// 敲门前 poll（用建会话 secret）
	time.Sleep(100 * time.Millisecond) // 避免 pollGuard
	rr := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+created.sessionID+"/poll?secret="+created.secret, nil, false)
	if rr.Code != 200 {
		t.Fatalf("poll pending status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := enrollDecodeBody(t, rr)
	if m["status"] != "pending" {
		t.Errorf("status=%v want pending", m["status"])
	}

	// 敲门
	time.Sleep(100 * time.Millisecond)
	rr2 := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+created.sessionID+"/request",
		map[string]any{"deviceHint": "Chrome"}, false)
	knockSecret := enrollDecodeBody(t, rr2)["secret"].(string)

	// 敲门后 poll → requested
	time.Sleep(2200 * time.Millisecond)
	rr3 := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+created.sessionID+"/poll?secret="+knockSecret, nil, false)
	if rr3.Code != 200 {
		t.Fatalf("poll requested status=%d body=%s", rr3.Code, rr3.Body.String())
	}
	m3 := enrollDecodeBody(t, rr3)
	if m3["status"] != "requested" {
		t.Errorf("status=%v want requested", m3["status"])
	}
}

// TestEnroll_Poll_WrongSecret 错 secret → 401。
func TestEnroll_Poll_WrongSecret(t *testing.T) {
	env := newEnrollTestEnv(t)
	created := enrollCreateHelper(t, env)

	rr := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+created.sessionID+"/poll?secret=wrong", nil, false)
	if rr.Code != 401 {
		t.Errorf("错 secret 应 401, got %d", rr.Code)
	}
}

// TestEnroll_QR qr.txt。
func TestEnroll_QR(t *testing.T) {
	env := newEnrollTestEnv(t)
	created := enrollCreateHelper(t, env)

	rr := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+created.sessionID+"/qr.txt", nil, false)
	if rr.Code != 200 {
		t.Fatalf("qr.txt status=%d", rr.Code)
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type=%q want text/plain", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "▀") && !strings.Contains(body, "█") && !strings.Contains(body, "▄") {
		t.Error("qr.txt 不含二维码字符")
	}
}

// TestEnroll_Cancel 旧设备取消会话。
func TestEnroll_Cancel(t *testing.T) {
	env := newEnrollTestEnv(t)
	created := enrollCreateHelper(t, env)

	req := httptest.NewRequest("DELETE", "/v1/passkey-enroll/sessions/"+created.sessionID, nil)
	req.AddCookie(env.cookie)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("cancel status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := enrollDecodeBody(t, rr)
	if m["status"] != "deleted" {
		t.Errorf("status=%v want deleted", m["status"])
	}

	// 取消后 lookup → 404
	rr2 := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+created.sessionID, nil, false)
	if rr2.Code != 404 {
		t.Errorf("取消后应 404, got %d", rr2.Code)
	}
}

// TestEnroll_ApiKeyKindGuard apikey-kind 会话调 enroll 端点 → 404。
func TestEnroll_ApiKeyKindGuard(t *testing.T) {
	env := newEnrollTestEnv(t)

	// 创建一个 apikey-kind 的 cli-auth 会话
	cliMgr := auth.NewCliAuthManager(env.db, 0)
	created, err := cliMgr.CreateSession("test-device", []string{"notify:send"})
	if err != nil {
		t.Fatalf("create cli-auth session: %v", err)
	}

	// 用 enroll 端点 lookup → 404（kind 不匹配）
	rr := enrollDoReq(t, env, "GET", "/v1/passkey-enroll/sessions/"+created.SessionID, nil, false)
	if rr.Code != 404 {
		t.Errorf("apikey-kind 会话调 enroll lookup 应 404, got %d", rr.Code)
	}

	// 敲门 → 应失败（404 或 409）
	rr2 := enrollDoReq(t, env, "POST", "/v1/passkey-enroll/sessions/"+created.SessionID+"/request",
		map[string]any{"deviceHint": "Chrome"}, false)
	if rr2.Code != 404 && rr2.Code != 400 {
		t.Errorf("apikey-kind 会话敲门应 404/400, got %d", rr2.Code)
	}
}

// TestEnroll_CliAuthZeroRegression CLI 授权零回归（apikey 路径不受影响）。
func TestEnroll_CliAuthZeroRegression(t *testing.T) {
	env := newEnrollTestEnv(t)
	// 复用 enrollTestEnv 的 db + svc，但不经过 enroll handler
	cliMgr := auth.NewCliAuthManager(env.db, 0)

	// 建 apikey 会话
	created, err := cliMgr.CreateSession("test", []string{"notify:send"})
	if err != nil {
		t.Fatalf("create cli-auth session: %v", err)
	}

	// GetByID → kind=apikey
	s, err := cliMgr.GetByID(created.SessionID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if s.Kind != store.CliAuthKindAPIKey {
		t.Errorf("kind got %q want apikey", s.Kind)
	}
	if s.Status != store.CliAuthPending {
		t.Errorf("status got %q want pending", s.Status)
	}

	// approve → approved
	if err := cliMgr.Approve(created.SessionID, env.userID, []string{"notify:send"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	s2, _ := cliMgr.GetByID(created.SessionID)
	if s2.Status != store.CliAuthApproved {
		t.Errorf("status got %q want approved", s2.Status)
	}

	// poll → 领证（apiKey 非空）
	res, err := cliMgr.Poll(created.SessionID, created.Secret)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if res.APIKey == "" {
		t.Error("apikey-kind poll 应返回明文 apiKey")
	}
}
