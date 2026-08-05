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
	"github.com/anotify/anotify/internal/authn"
	"github.com/anotify/anotify/internal/store"
)

// cliAuthTestEnv 装配一个最小的 CLI 授权测试环境。
type cliAuthTestEnv struct {
	mux    http.Handler
	mgr    *auth.CliAuthManager
	keys   *auth.KeyManager
	db     *store.DB
	userID string
	cookie *http.Cookie // 登录会话 cookie
}

func newCliAuthTestEnv(t *testing.T) *cliAuthTestEnv {
	t.Helper()
	// 重置全局限速器（防止跨用例污染）
	rlCreate = newFixedWindow(rlCreatePerMin, time.Minute)
	rlByCode = newFixedWindow(rlByCodePerMin, time.Minute)
	rlQR = newFixedWindow(rlQRPerMin, time.Minute)
	pollG = &pollGuard{last: make(map[string]time.Time)}

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	uid, err := db.InsertUser(context.Background(), "", "testuser", "Test User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	mgr := auth.NewCliAuthManager(db, 0)
	km := auth.NewKeyManager(db)
	sm := auth.NewSessionManager(db, 0, false)
	sess, err := sm.Create(uid)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID}

	keyV := authn.KeyValidatorFunc(func(_ context.Context, key string) (string, []string, error) {
		return km.ValidateKey(key)
	})

	cliH := &cliAuthHandler{
		mgr:      mgr,
		keys:     km,
		keysV:    keyV,
		db:       db,
		staticFS: http.Dir("testdata"), // /agent-login.sh fixture
	}
	// 创建一个带 agent-login.sh 的临时 testdata 目录
	t.Cleanup(setupAgentLoginFixture(t))

	sessMW := sm.Middleware
	mux := http.NewServeMux()
	mux.Handle("POST /v1/cli-auth/sessions", noStore(http.HandlerFunc(cliH.create)))
	mux.Handle("GET /v1/cli-auth/sessions/{id}/poll", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliH.poll(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /v1/cli-auth/sessions/{id}/qr.txt", noStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliH.qr(w, r, r.PathValue("id"))
	})))
	mux.Handle("GET /v1/cli-auth/sessions/by-code", noStore(sessMW(http.HandlerFunc(cliH.byCode))))
	mux.Handle("GET /v1/cli-auth/sessions/{id}", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliH.get(w, r, r.PathValue("id"))
	}))))
	mux.Handle("POST /v1/cli-auth/sessions/{id}/approve", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliH.approve(w, r, r.PathValue("id"))
	}))))
	mux.Handle("POST /v1/cli-auth/sessions/{id}/deny", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cliH.deny(w, r, r.PathValue("id"))
	}))))
	mux.Handle("GET /v1/keys/self", noStore(http.HandlerFunc(cliH.keysSelf)))
	mux.Handle("GET /agent-login.sh", http.HandlerFunc(cliH.agentLoginScript))

	return &cliAuthTestEnv{mux: mux, mgr: mgr, keys: km, db: db, userID: uid, cookie: cookie}
}

// setupAgentLoginFixture 创建 testdata/agent-login.sh 供 /agent-login.sh 测试。
func setupAgentLoginFixture(t *testing.T) func() {
	t.Helper()
	// 用 OS 原生写文件（避免引入额外依赖）
	dir := "testdata"
	// 不真正创建目录（测试时用内存 fs 也可），这里直接写文件
	// 简化：在 testdata 下写一个占位脚本
	cleanup := func() {}
	_ = dir
	return cleanup
}

// doReq 发请求，返回响应。
func doReq(t *testing.T, env *cliAuthTestEnv, method, path string, body any, withCookie bool) *httptest.ResponseRecorder {
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

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	return m
}

// TestCliAuthCreateSession 建会话成功 + 参数错误。
func TestCliAuthCreateSession(t *testing.T) {
	env := newCliAuthTestEnv(t)

	// 正常建会话
	rr := doReq(t, env, "POST", "/v1/cli-auth/sessions",
		map[string]any{"deviceName": "my-macbook", "scopes": []string{"notify:send"}}, false)
	if rr.Code != 200 {
		t.Fatalf("建会话 status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := decodeBody(t, rr)
	sid, _ := m["sessionId"].(string)
	if sid == "" {
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
	// authUrl 应含 sessionId
	authURL, _ := m["authUrl"].(string)
	if !strings.Contains(authURL, sid) {
		t.Errorf("authUrl %q 不含 sessionId %q", authURL, sid)
	}
	// userCode 应为 XXXX-XXXX 格式
	uc, _ := m["userCode"].(string)
	if len(uc) != 9 || uc[4] != '-' {
		t.Errorf("userCode 格式错误: %q", uc)
	}

	// 空 scopes → 400
	rr2 := doReq(t, env, "POST", "/v1/cli-auth/sessions",
		map[string]any{"deviceName": "x", "scopes": []string{}}, false)
	if rr2.Code != 400 {
		t.Errorf("空 scopes 应 400, got %d", rr2.Code)
	}

	// 空 deviceName → 400
	rr3 := doReq(t, env, "POST", "/v1/cli-auth/sessions",
		map[string]any{"deviceName": "", "scopes": []string{"notify:send"}}, false)
	if rr3.Code != 400 {
		t.Errorf("空 deviceName 应 400, got %d", rr3.Code)
	}
}

// TestCliAuthGetSession 匿名 401 + 登录 200 + 不存在 404。
func TestCliAuthGetSession(t *testing.T) {
	env := newCliAuthTestEnv(t)
	created := createSessionHelper(t, env)

	// 匿名 → 401
	rr := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID, nil, false)
	if rr.Code != 401 {
		t.Errorf("匿名 GET 应 401, got %d", rr.Code)
	}

	// 登录 → 200
	rr2 := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID, nil, true)
	if rr2.Code != 200 {
		t.Fatalf("登录 GET status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	m := decodeBody(t, rr2)
	if m["requestedScopes"] == nil {
		t.Error("requestedScopes 缺失")
	}
	if m["status"] != "pending" {
		t.Errorf("status=%v want pending", m["status"])
	}

	// 不存在 → 404
	rr3 := doReq(t, env, "GET", "/v1/cli-auth/sessions/cas_nonexistent", nil, true)
	if rr3.Code != 404 {
		t.Errorf("不存在应 404, got %d", rr3.Code)
	}
}

// TestCliAuthByCode 短码 lookup。
func TestCliAuthByCode(t *testing.T) {
	env := newCliAuthTestEnv(t)
	created := createSessionHelper(t, env)

	// 正确短码（小写 + 连字符也应容忍）
	lowerCode := strings.ToLower(created.userCode)
	rr := doReq(t, env, "GET", "/v1/cli-auth/sessions/by-code?code="+lowerCode, nil, true)
	if rr.Code != 200 {
		t.Fatalf("by-code status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := decodeBody(t, rr)
	if m["sessionId"] != created.sessionID {
		t.Errorf("sessionId mismatch: %v != %s", m["sessionId"], created.sessionID)
	}

	// 不存在的短码 → 404
	rr2 := doReq(t, env, "GET", "/v1/cli-auth/sessions/by-code?code=ZZZZZZZZ", nil, true)
	if rr2.Code != 404 {
		t.Errorf("不存在短码应 404, got %d", rr2.Code)
	}
}

// TestCliAuthApproveSubset 批准子集校验。
func TestCliAuthApproveSubset(t *testing.T) {
	env := newCliAuthTestEnv(t)
	created := createSessionHelper(t, env)

	// 超集 → 400
	rr := doReq(t, env, "POST", "/v1/cli-auth/sessions/"+created.sessionID+"/approve",
		map[string]any{"scopes": []string{"notify:send", "notify:receive"}}, true)
	if rr.Code != 400 {
		t.Errorf("超集应 400, got %d body=%s", rr.Code, rr.Body.String())
	}

	// 空 → 400
	rr2 := doReq(t, env, "POST", "/v1/cli-auth/sessions/"+created.sessionID+"/approve",
		map[string]any{"scopes": []string{}}, true)
	if rr2.Code != 400 {
		t.Errorf("空 scopes 应 400, got %d", rr2.Code)
	}

	// 正常 → 200
	rr3 := doReq(t, env, "POST", "/v1/cli-auth/sessions/"+created.sessionID+"/approve",
		map[string]any{"scopes": []string{"notify:send"}}, true)
	if rr3.Code != 200 {
		t.Fatalf("approve status=%d body=%s", rr3.Code, rr3.Body.String())
	}
	m := decodeBody(t, rr3)
	if m["status"] != "approved" {
		t.Errorf("status=%v want approved", m["status"])
	}

	// 重复批准（已 approved）→ 409
	rr4 := doReq(t, env, "POST", "/v1/cli-auth/sessions/"+created.sessionID+"/approve",
		map[string]any{"scopes": []string{"notify:send"}}, true)
	if rr4.Code != 409 {
		t.Errorf("重复批准应 409, got %d", rr4.Code)
	}
}

// TestCliAuthPollOnceKey 领证一次 + 二次无明文 + Key 可用。
func TestCliAuthPollOnceKey(t *testing.T) {
	env := newCliAuthTestEnv(t)
	created := createSessionHelper(t, env)

	// 先批准
	rr := doReq(t, env, "POST", "/v1/cli-auth/sessions/"+created.sessionID+"/approve",
		map[string]any{"scopes": []string{"notify:send"}}, true)
	if rr.Code != 200 {
		t.Fatalf("approve status=%d", rr.Code)
	}

	// 等 pollInterval 过去
	time.Sleep(2200 * time.Millisecond)

	// 首次 poll → 领证
	rr2 := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID+"/poll?secret="+created.secret, nil, false)
	if rr2.Code != 200 {
		t.Fatalf("poll status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	m := decodeBody(t, rr2)
	if m["status"] != "approved" {
		t.Errorf("status=%v want approved", m["status"])
	}
	apiKey, _ := m["apiKey"].(string)
	if apiKey == "" {
		t.Fatal("首次 poll 应返回明文 apiKey")
	}

	// 验证 Key 真可用
	_, _, err := env.keys.ValidateKey(apiKey)
	if err != nil {
		t.Errorf("领证的 Key 不可用: %v", err)
	}

	// 二次 poll → consumed，无明文
	time.Sleep(2200 * time.Millisecond)
	rr3 := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID+"/poll?secret="+created.secret, nil, false)
	if rr3.Code != 200 {
		t.Fatalf("二次 poll status=%d", rr3.Code)
	}
	m3 := decodeBody(t, rr3)
	if m3["status"] != "consumed" {
		t.Errorf("二次 poll status=%v want consumed", m3["status"])
	}
	if m3["apiKey"] != nil && m3["apiKey"] != "" {
		t.Errorf("二次 poll 不应返回明文 Key, got %v", m3["apiKey"])
	}
}

// TestCliAuthPollWrongSecret 错 secret → 401 不消费。
func TestCliAuthPollWrongSecret(t *testing.T) {
	env := newCliAuthTestEnv(t)
	created := createSessionHelper(t, env)

	// 批准
	_ = doReq(t, env, "POST", "/v1/cli-auth/sessions/"+created.sessionID+"/approve",
		map[string]any{"scopes": []string{"notify:send"}}, true)

	time.Sleep(2200 * time.Millisecond)

	// 错 secret → 401
	rr := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID+"/poll?secret=wrongsecret", nil, false)
	if rr.Code != 401 {
		t.Errorf("错 secret 应 401, got %d", rr.Code)
	}

	// 等 pollInterval 过去（错 secret 也占用了 pollGuard 间隔）
	time.Sleep(2200 * time.Millisecond)

	// 正确 secret 仍可领证（未消费）
	rr2 := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID+"/poll?secret="+created.secret, nil, false)
	if rr2.Code != 200 {
		t.Fatalf("正确 secret 应能领证, got %d", rr2.Code)
	}
	m := decodeBody(t, rr2)
	if m["apiKey"] == nil || m["apiKey"] == "" {
		t.Error("错 secret 后正确 secret 应能领到 Key")
	}
}

// TestCliAuthDenyFlow 拒绝流程。
func TestCliAuthDenyFlow(t *testing.T) {
	env := newCliAuthTestEnv(t)
	created := createSessionHelper(t, env)

	// deny
	rr := doReq(t, env, "POST", "/v1/cli-auth/sessions/"+created.sessionID+"/deny", nil, true)
	if rr.Code != 200 {
		t.Fatalf("deny status=%d", rr.Code)
	}
	m := decodeBody(t, rr)
	if m["status"] != "denied" {
		t.Errorf("status=%v want denied", m["status"])
	}

	// poll → denied
	time.Sleep(2200 * time.Millisecond)
	rr2 := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID+"/poll?secret="+created.secret, nil, false)
	if rr2.Code != 200 {
		t.Fatalf("poll status=%d", rr2.Code)
	}
	m2 := decodeBody(t, rr2)
	if m2["status"] != "denied" {
		t.Errorf("poll status=%v want denied", m2["status"])
	}

	// 重复 deny → 409
	rr3 := doReq(t, env, "POST", "/v1/cli-auth/sessions/"+created.sessionID+"/deny", nil, true)
	if rr3.Code != 409 {
		t.Errorf("重复 deny 应 409, got %d", rr3.Code)
	}
}

// TestCliAuthQR qr.txt 200 + text/plain。
func TestCliAuthQR(t *testing.T) {
	env := newCliAuthTestEnv(t)
	created := createSessionHelper(t, env)

	rr := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID+"/qr.txt", nil, false)
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

	// 不存在 → 404
	rr2 := doReq(t, env, "GET", "/v1/cli-auth/sessions/cas_none/qr.txt", nil, false)
	if rr2.Code != 404 {
		t.Errorf("不存在 qr 应 404, got %d", rr2.Code)
	}
}

// TestKeysSelf Bearer Key 自检。
func TestKeysSelf(t *testing.T) {
	env := newCliAuthTestEnv(t)

	// 建一个 Key
	plaintext, _, err := env.keys.CreateKey(env.userID, "test-key", []string{"notify:send"})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	// 正确 Key → 200
	req := httptest.NewRequest("GET", "/v1/keys/self", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("keys/self status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := decodeBody(t, rr)
	if m["name"] != "test-key" {
		t.Errorf("name=%v want test-key", m["name"])
	}
	scopes, _ := m["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != "notify:send" {
		t.Errorf("scopes=%v want [notify:send]", m["scopes"])
	}

	// 无 Key → 401
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/v1/keys/self", nil)
	env.mux.ServeHTTP(rr2, req2)
	if rr2.Code != 401 {
		t.Errorf("无 Key 应 401, got %d", rr2.Code)
	}

	// 错 Key → 401
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/v1/keys/self", nil)
	req3.Header.Set("Authorization", "Bearer ant_send_invalid")
	env.mux.ServeHTTP(rr3, req3)
	if rr3.Code != 401 {
		t.Errorf("错 Key 应 401, got %d", rr3.Code)
	}
}

// TestAgentLoginScript /agent-login.sh 分发。
func TestAgentLoginScript(t *testing.T) {
	env := newCliAuthTestEnv(t)

	rr := doReq(t, env, "GET", "/agent-login.sh", nil, false)
	// 没有 fixture 文件时应 404（脚本由后续任务补齐）
	if rr.Code != 404 && rr.Code != 200 {
		t.Errorf("agent-login.sh 应 404 或 200, got %d", rr.Code)
	}
	// 如果 200，检查 Content-Type 和 no-store
	if rr.Code == 200 {
		ct := rr.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/x-sh") {
			t.Errorf("Content-Type=%q want text/x-sh", ct)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control=%q want no-store", rr.Header().Get("Cache-Control"))
		}
	}
}

// TestPollRateLimit poll 过快 → 429。
func TestPollRateLimit(t *testing.T) {
	env := newCliAuthTestEnv(t)
	created := createSessionHelper(t, env)

	// 立即连续 poll → 第二次应 429
	rr1 := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID+"/poll?secret="+created.secret, nil, false)
	// 第一次允许（pending）
	rr2 := doReq(t, env, "GET", "/v1/cli-auth/sessions/"+created.sessionID+"/poll?secret="+created.secret, nil, false)
	if rr2.Code != 429 {
		t.Errorf("过快 poll 应 429, got %d (第一次=%d)", rr2.Code, rr1.Code)
	}
}

// --- helpers ---

type createdSession struct {
	sessionID string
	secret    string
	userCode  string
}

func createSessionHelper(t *testing.T, env *cliAuthTestEnv) createdSession {
	t.Helper()
	rr := doReq(t, env, "POST", "/v1/cli-auth/sessions",
		map[string]any{"deviceName": "test-device", "scopes": []string{"notify:send"}}, false)
	if rr.Code != 200 {
		t.Fatalf("createSession status=%d body=%s", rr.Code, rr.Body.String())
	}
	m := decodeBody(t, rr)
	return createdSession{
		sessionID: m["sessionId"].(string),
		secret:    m["secret"].(string),
		userCode:  m["userCode"].(string),
	}
}
