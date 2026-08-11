package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// passkeysTestEnv 装配一个最小的 /v1/auth/* 测试环境（带登录会话）。
//
// 复现 bug：安全页加载时请求 GET /v1/auth/passkeys，后端 ServeHTTP 没有该分支
// → 404 → 前端 fetchApi 返回 null → demoMode.passkeys=true →
// 点「添加 Passkey」直接弹「演示模式」提示，根本走不到 WebAuthn 流程。
type passkeysTestEnv struct {
	mux    http.Handler
	db     *store.DB
	svc    *auth.Service
	userID string
	cookie *http.Cookie
}

func newPasskeysTestEnv(t *testing.T) *passkeysTestEnv {
	t.Helper()
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

	uid, err := db.InsertUser(context.Background(), "", "pkuser", "PK User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	sess, err := svc.Sessions().Create(uid, "Test · macOS")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	authH := &authHandler{svc: svc}
	sessMW := svc.Sessions().Middleware
	mux := http.NewServeMux()
	// 与生产 mux.go 路由一致：me/sessions/passkeys 需登录优先匹配，/v1/auth/ 兜底。
	mux.Handle("/v1/auth/me", noStore(sessMW(http.HandlerFunc(authH.me))))
	mux.Handle("/v1/auth/sessions", noStore(sessMW(http.HandlerFunc(authH.sessions))))
	mux.Handle("/v1/auth/passkeys", noStore(sessMW(http.HandlerFunc(authH.passkeysRoot))))
	mux.Handle("/v1/auth/passkeys/", noStore(sessMW(http.HandlerFunc(authH.passkeysItem))))
	mux.Handle("/v1/auth/", noStore(http.HandlerFunc(authH.ServeHTTP)))

	return &passkeysTestEnv{
		mux:    mux,
		db:     db,
		svc:    svc,
		userID: uid,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}
}

func pkReq(t *testing.T, env *passkeysTestEnv, method, path string, body any, withCookie bool) *httptest.ResponseRecorder {
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

// TestPasskeys_List_404_Reproduce 复现：已登录用户 GET /v1/auth/passkeys 当前返回 404。
// 期望：200 + 空列表（首用户刚注册，还没有凭证）。
func TestPasskeys_List_404_Reproduce(t *testing.T) {
	env := newPasskeysTestEnv(t)

	// 未登录 → 401（守卫先于端点存在性）
	rr := pkReq(t, env, "GET", "/v1/auth/passkeys", nil, false)
	if rr.Code != 401 {
		t.Errorf("未登录应 401, got %d", rr.Code)
	}

	// 已登录 → 当前 bug 是 404，修复后应为 200 + 空列表
	rr2 := pkReq(t, env, "GET", "/v1/auth/passkeys", nil, true)
	if rr2.Code != 200 {
		t.Fatalf("已登录 GET /v1/auth/passkeys 应 200, got %d (body=%s) —— 复现 demo 模式根因", rr2.Code, rr2.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rr2.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", rr2.Body.String(), err)
	}
	// passkeys 必须是 [] 而非 null，否则前端误判 demo
	pk, ok := m["passkeys"].([]any)
	if !ok || pk == nil {
		t.Errorf("passkeys 应为非 null 数组, got %T (%v)", m["passkeys"], m["passkeys"])
	}
	if len(pk) != 0 {
		t.Errorf("首用户应无凭证, got %d", len(pk))
	}
}

// TestPasskeys_ListReturnsRealCred 验证列表返回真实凭证（非 demo 数据）。
// 先在 DB 直接插一条凭证，列表应返回它。
func TestPasskeys_ListReturnsRealCred(t *testing.T) {
	env := newPasskeysTestEnv(t)
	cred := &store.Passkey{
		ID: "cred-real-1", UserID: env.userID, PublicKey: []byte{1, 2, 3},
		Name: "我的手机", Transports: []string{"internal"}, BackupEligible: true,
		CreatedAt: store.Now(),
	}
	if err := env.db.CreatePasskey(cred); err != nil {
		t.Fatalf("create passkey: %v", err)
	}

	rr := pkReq(t, env, "GET", "/v1/auth/passkeys", nil, true)
	if rr.Code != 200 {
		t.Fatalf("GET 应 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	var m map[string]any
	_ = json.Unmarshal(rr2Body(rr), &m)
	list, _ := m["passkeys"].([]any)
	if len(list) != 1 {
		t.Fatalf("应返回 1 条凭证, got %d (%v)", len(list), list)
	}
	item, _ := list[0].(map[string]any)
	if item["id"] != "cred-real-1" {
		t.Errorf("id got %v want cred-real-1", item["id"])
	}
	if item["name"] != "我的手机" {
		t.Errorf("name got %v want 我的手机", item["name"])
	}
	// 不应泄露 publicKey
	if _, exists := item["publicKey"]; exists {
		t.Errorf("列表不应返回 publicKey 原始字节")
	}
}

// TestPasskeys_Delete_404_Reproduce 复现：DELETE /v1/auth/passkeys/:id 当前 404。
// 注：至少保留一个 Passkey 的限制不在此测试范围（见 TestPasskeys_DeleteLastOneRejected），
// 所以这里插 2 个凭证，删一个应成功。
func TestPasskeys_Delete_404_Reproduce(t *testing.T) {
	env := newPasskeysTestEnv(t)
	cred := &store.Passkey{
		ID: "cred-del-1", UserID: env.userID, PublicKey: []byte{1}, Name: "x",
		Transports: []string{"internal"}, CreatedAt: store.Now(),
	}
	keep := &store.Passkey{
		ID: "cred-keep-1", UserID: env.userID, PublicKey: []byte{2}, Name: "keep",
		Transports: []string{"internal"}, CreatedAt: store.Now(),
	}
	if err := env.db.CreatePasskey(cred); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := env.db.CreatePasskey(keep); err != nil {
		t.Fatalf("create keep: %v", err)
	}

	rr := pkReq(t, env, "DELETE", "/v1/auth/passkeys/cred-del-1", nil, true)
	if rr.Code != 200 {
		t.Errorf("DELETE 应 200, got %d (body=%s) —— 复现 demo 模式根因", rr.Code, rr.Body.String())
	}
	// 删完再 list 应只剩 keep
	rr2 := pkReq(t, env, "GET", "/v1/auth/passkeys", nil, true)
	var m map[string]any
	_ = json.Unmarshal(rr2Body(rr2), &m)
	list, _ := m["passkeys"].([]any)
	if len(list) != 1 {
		t.Errorf("删除后列表应剩 1 条, got %d", len(list))
	}
}

// TestPasskeys_Rename_404_Reproduce 复现：PATCH /v1/auth/passkeys/:id 当前 404。
func TestPasskeys_Rename_404_Reproduce(t *testing.T) {
	env := newPasskeysTestEnv(t)
	cred := &store.Passkey{
		ID: "cred-ren-1", UserID: env.userID, PublicKey: []byte{1}, Name: "旧",
		Transports: []string{"internal"}, CreatedAt: store.Now(),
	}
	if err := env.db.CreatePasskey(cred); err != nil {
		t.Fatalf("create: %v", err)
	}

	rr := pkReq(t, env, "PATCH", "/v1/auth/passkeys/cred-ren-1",
		map[string]any{"name": "新名字"}, true)
	if rr.Code != 200 {
		t.Fatalf("PATCH 应 200, got %d (body=%s) —— 复现 demo 模式根因", rr.Code, rr.Body.String())
	}
	// 往返：list 里名字应是新的
	rr2 := pkReq(t, env, "GET", "/v1/auth/passkeys", nil, true)
	var m map[string]any
	_ = json.Unmarshal(rr2Body(rr2), &m)
	list, _ := m["passkeys"].([]any)
	if len(list) != 1 {
		t.Fatalf("应 1 条, got %d", len(list))
	}
	item, _ := list[0].(map[string]any)
	if item["name"] != "新名字" {
		t.Errorf("rename 往返不一致: got %v want 新名字", item["name"])
	}
}

// TestPasskeys_DeleteOthersCred 防越权：删别人的凭证应 404/403，不能删成功。
func TestPasskeys_DeleteOthersCred(t *testing.T) {
	env := newPasskeysTestEnv(t)
	// 另一个用户的凭证
	otherUID, _ := env.db.InsertUser(context.Background(), "", "other", "Other")
	other := &store.Passkey{
		ID: "cred-other-1", UserID: otherUID, PublicKey: []byte{9}, Name: "别人的",
		Transports: []string{"internal"}, CreatedAt: store.Now(),
	}
	if err := env.db.CreatePasskey(other); err != nil {
		t.Fatalf("create: %v", err)
	}

	rr := pkReq(t, env, "DELETE", "/v1/auth/passkeys/cred-other-1", nil, true)
	if rr.Code == 200 {
		t.Errorf("删别人的凭证不应 200, got %d（越权）", rr.Code)
	}
	// 凭证应仍存在
	if _, err := env.db.GetPasskeyByID("cred-other-1"); err != nil {
		t.Errorf("别人的凭证被误删: %v", err)
	}
}

// TestPasskeys_Unauthorized_NoCookie 所有写操作无 cookie 应 401。
func TestPasskeys_Unauthorized_NoCookie(t *testing.T) {
	env := newPasskeysTestEnv(t)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/auth/passkeys", nil},
		{"DELETE", "/v1/auth/passkeys/x", nil},
		{"PATCH", "/v1/auth/passkeys/x", map[string]any{"name": "y"}},
	} {
		rr := pkReq(t, env, c.method, c.path, c.body, false)
		if rr.Code != 401 {
			t.Errorf("%s %s 无 cookie 应 401, got %d", c.method, c.path, rr.Code)
		}
	}
}

func rr2Body(rr *httptest.ResponseRecorder) []byte {
	return rr.Body.Bytes()
}

// TestPasskeys_AddCredentialEndpointsExist 验证补建凭证端点存在 + 鉴权。
// 真实 WebAuthn attestation 需浏览器/认证器，此处只测：未登录→401，端点存在（非 404）。
func TestPasskeys_AddCredentialEndpointsExist(t *testing.T) {
	env := newPasskeysTestEnv(t)

	// 未登录 → 401（而非 404）
	rr := pkReq(t, env, "POST", "/v1/auth/passkeys/register/options", nil, false)
	if rr.Code != 401 {
		t.Errorf("未登录 options 应 401, got %d", rr.Code)
	}
	rr2 := pkReq(t, env, "POST", "/v1/auth/passkeys/register", nil, false)
	if rr2.Code != 401 {
		t.Errorf("未登录 register 应 401, got %d", rr2.Code)
	}

	// 已登录 + 无 challenge → options 应 200（返回 publicKey creation options）
	rr3 := pkReq(t, env, "POST", "/v1/auth/passkeys/register/options", map[string]any{}, true)
	if rr3.Code != 200 {
		t.Errorf("已登录 options 应 200, got %d (body=%s)", rr3.Code, rr3.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rr3.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// WebAuthn creation options 含 publicKey 字段
	if m["publicKey"] == nil {
		t.Errorf("options 应含 publicKey 字段, got keys=%v", keysOf(m))
	}

	// 已登录 + finish 但没走 options（无 challenge）→ 400
	rr4 := pkReq(t, env, "POST", "/v1/auth/passkeys/register", map[string]any{"name": "x"}, true)
	if rr4.Code != 400 {
		t.Errorf("无 challenge finish 应 400, got %d", rr4.Code)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPasskeys_DeleteLastOneRejected 至少保留一个：删唯一凭证应 409。
func TestPasskeys_DeleteLastOneRejected(t *testing.T) {
	env := newPasskeysTestEnv(t)
	// 只有一个凭证
	cred := &store.Passkey{
		ID: "cred-only-1", UserID: env.userID, PublicKey: []byte{1}, Name: "唯一",
		Transports: []string{"internal"}, CreatedAt: store.Now(),
	}
	if err := env.db.CreatePasskey(cred); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 删唯一的 → 409
	rr := pkReq(t, env, "DELETE", "/v1/auth/passkeys/cred-only-1", nil, true)
	if rr.Code != 409 {
		t.Errorf("删唯一凭证应 409, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	// 凭证应仍存在
	if _, err := env.db.GetPasskeyByID("cred-only-1"); err != nil {
		t.Errorf("唯一凭证被误删: %v", err)
	}

	// 再加一个 → 现在有两个，删第一个应成功（200）
	cred2 := &store.Passkey{
		ID: "cred-only-2", UserID: env.userID, PublicKey: []byte{2}, Name: "第二个",
		Transports: []string{"internal"}, CreatedAt: store.Now(),
	}
	if err := env.db.CreatePasskey(cred2); err != nil {
		t.Fatalf("create cred2: %v", err)
	}
	rr2 := pkReq(t, env, "DELETE", "/v1/auth/passkeys/cred-only-1", nil, true)
	if rr2.Code != 200 {
		t.Errorf("有两个凭证时删一个应 200, got %d (body=%s)", rr2.Code, rr2.Body.String())
	}
	// 删剩最后一个时再删 → 409
	rr3 := pkReq(t, env, "DELETE", "/v1/auth/passkeys/cred-only-2", nil, true)
	if rr3.Code != 409 {
		t.Errorf("删剩最后一个应 409, got %d", rr3.Code)
	}
}
