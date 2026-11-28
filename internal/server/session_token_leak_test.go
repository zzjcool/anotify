package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// #589 的回归：会话令牌（Cookie 原文）不得出现在**任何**对外输出里——
// 本人会话列表、管理后台全站会话列表、错误响应体、访问日志。
//
// 装配是最小版的生产形状：/v1/auth/{me,sessions} 走会话中间件，
// /v1/admin/sessions 走「会话 + 管理员」两层中间件，整棵 mux 套 accessLog
// （日志断言要用真实访问日志，而不是另拼一份 path）。

type sessionLeakEnv struct {
	mux          http.Handler
	db           *store.DB
	adminUserID  string
	memberUserID string
	adminCookie  *http.Cookie
	memberCookie *http.Cookie
	memberToken  string // 明文令牌：只应出现在 Set-Cookie 里
	adminToken   string
}

func newSessionLeakEnv(t *testing.T) *sessionLeakEnv {
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

	ctx := context.Background()
	admin := &store.User{
		ID: store.NewUserID(), Username: "leak-admin", DisplayName: "admin",
		Role: store.RoleAdmin, CreatedAt: store.Now(),
	}
	if _, err := db.RegisterUser(ctx, admin); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	member := &store.User{
		ID: store.NewUserID(), Username: "leak-member", DisplayName: "member",
		CreatedAt: store.Now(),
	}
	if _, err := db.RegisterUser(ctx, member); err != nil {
		t.Fatalf("register member: %v", err)
	}

	sm := svc.Sessions()
	adminSess, err := sm.Create(admin.ID, "admin · test")
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	memberSess, err := sm.Create(member.ID, "member · test")
	if err != nil {
		t.Fatalf("create member session: %v", err)
	}

	authH := &authHandler{svc: svc, db: db}
	adminH := &adminHandler{db: db, svc: svc}
	sessMW := sm.Middleware

	mux := http.NewServeMux()
	// 与 mux.go 的装配顺序/形状一致：显式路由在前，/v1/auth/ 兜底在后。
	mux.Handle("/v1/auth/me", noStore(sessMW(http.HandlerFunc(authH.me))))
	mux.Handle("/v1/auth/sessions", noStore(sessMW(http.HandlerFunc(authH.sessions))))
	mux.Handle("/v1/auth/", noStore(http.HandlerFunc(authH.ServeHTTP)))
	mux.Handle("/v1/admin/", noStore(sessMW(svc.AdminMiddleware(http.HandlerFunc(adminH.ServeHTTP)))))

	return &sessionLeakEnv{
		mux:          accessLog(mux),
		db:           db,
		adminUserID:  admin.ID,
		memberUserID: member.ID,
		adminCookie:  &http.Cookie{Name: auth.SessionCookieName, Value: adminSess.Token},
		memberCookie: &http.Cookie{Name: auth.SessionCookieName, Value: memberSess.Token},
		memberToken:  memberSess.Token,
		adminToken:   adminSess.Token,
	}
}

func (e *sessionLeakEnv) do(t *testing.T, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// TestSessionListNeverReturnsToken 钉住 #589 的三条对外输出。
//
// 去掉修复必须红：改前 sessions[].id 就是 Cookie 原文，所以第 1/2/4 步都会失败。
func TestSessionListNeverReturnsToken(t *testing.T) {
	env := newSessionLeakEnv(t)

	// ---- 1. 本人列表：不出现令牌原文 ----
	rr := env.do(t, http.MethodGet, "/v1/auth/sessions", env.memberCookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/auth/sessions = %d %s", rr.Code, rr.Body.String())
	}
	selfBody := rr.Body.String()
	if strings.Contains(selfBody, env.memberToken) {
		t.Fatalf("本人会话列表泄露了 Cookie 令牌原文: %s", selfBody)
	}
	if strings.Contains(selfBody, env.memberToken[:16]) {
		t.Fatalf("本人会话列表泄露了令牌片段: %s", selfBody)
	}

	var selfList struct {
		Sessions []map[string]any `json:"sessions"`
		Count    int              `json:"count"`
	}
	if err := json.Unmarshal([]byte(selfBody), &selfList); err != nil {
		t.Fatalf("decode self list: %v", err)
	}
	if selfList.Count != 1 || len(selfList.Sessions) != 1 {
		t.Fatalf("本人应只有 1 条会话, got count=%d len=%d", selfList.Count, len(selfList.Sessions))
	}
	publicID := fmt.Sprint(selfList.Sessions[0]["id"])
	if publicID == "" || publicID == env.memberToken {
		t.Fatalf("sessions[].id 必须是公开 id（不是令牌）, got %q", publicID)
	}
	if !strings.HasPrefix(publicID, "sess_") {
		t.Errorf("公开 id 形状应为 sess_<opaque>, got %q", publicID)
	}

	// ---- 2. 公开 id 当 Cookie 必须 401（它就是被拿去冒充的那条路径）----
	rr = env.do(t, http.MethodGet, "/v1/auth/me",
		&http.Cookie{Name: auth.SessionCookieName, Value: publicID})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("公开 id 不能当会话令牌用: /v1/auth/me = %d %s", rr.Code, rr.Body.String())
	}

	// ---- 3. 管理后台全站列表：只有公开 id，没有**任何**会话的令牌原文 ----
	rr = env.do(t, http.MethodGet, "/v1/admin/sessions", env.adminCookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/admin/sessions = %d %s", rr.Code, rr.Body.String())
	}
	adminBody := rr.Body.String()
	for name, token := range map[string]string{
		"member": env.memberToken,
		"admin":  env.adminToken,
	} {
		if strings.Contains(adminBody, token) {
			t.Fatalf("/v1/admin/sessions 泄露了 %s 的令牌原文（管理员可冒充任意用户）: %s", name, adminBody)
		}
	}
	var adminList struct {
		Sessions []struct {
			ID     string `json:"id"`
			UserID string `json:"userId"`
		} `json:"sessions"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(adminBody), &adminList); err != nil {
		t.Fatalf("decode admin list: %v", err)
	}
	if adminList.Count != 2 {
		t.Fatalf("全站活跃会话应 2 条, got %d", adminList.Count)
	}
	var memberRowID string
	for _, row := range adminList.Sessions {
		if row.UserID == env.memberUserID {
			memberRowID = row.ID
		}
	}
	if memberRowID == "" {
		t.Fatalf("全站列表缺少 member 的会话: %s", adminBody)
	}
	// ---- 4. 拿别人的公开 id 冒充 → 必须 401（改前这里是 200 且身份是对方）----
	rr = env.do(t, http.MethodGet, "/v1/auth/me",
		&http.Cookie{Name: auth.SessionCookieName, Value: memberRowID})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("管理员用公开 id 冒充成员必须 401, got %d %s", rr.Code, rr.Body.String())
	}
}

// TestSessionRevokePathNeverLeaksToken 钉住「吊销请求把令牌写进访问日志 / 错误文本」。
//
// 前端（security.html）把 sessions[].id 拼进 DELETE /v1/auth/sessions/{id}。
// 修复后拼的是公开 id；但这里刻意**模拟旧客户端**（把令牌拼进路径），因为
// 日志是长期留存的，不能赌客户端行为：路径尾段脱敏 + 404 不回显路径。
func TestSessionRevokePathNeverLeaksToken(t *testing.T) {
	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	env := newSessionLeakEnv(t)

	// 旧客户端：路径里带令牌原文。
	rr := env.do(t, http.MethodDelete, "/v1/auth/sessions/"+env.memberToken, env.memberCookie)
	if strings.Contains(rr.Body.String(), env.memberToken) {
		t.Fatalf("404 错误体回显了路径（令牌随错误文本再泄一次）: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unknown auth endpoint") {
		t.Errorf("404 文案应保持固定且可识别, got %s", rr.Body.String())
	}
	logs := logBuf.String()
	if strings.Contains(logs, env.memberToken) {
		t.Fatalf("访问日志泄露了会话令牌原文:\n%s", logs)
	}
	if !strings.Contains(logs, "/v1/auth/sessions/***") {
		t.Fatalf("访问日志应脱敏会话路径尾段，实际:\n%s", logs)
	}

	// 新客户端：路径里只有公开 id —— 同样整段脱敏（公开 id 也没必要进日志）。
	selfBody := env.do(t, http.MethodGet, "/v1/auth/sessions", env.memberCookie).Body.String()
	var list struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(selfBody), &list); err != nil || len(list.Sessions) == 0 {
		t.Fatalf("decode self list: %v (%s)", err, selfBody)
	}
	publicID := list.Sessions[0].ID
	logBuf.Reset()
	env.do(t, http.MethodDelete, "/v1/auth/sessions/"+publicID, env.memberCookie)
	if strings.Contains(logBuf.String(), publicID) {
		t.Fatalf("访问日志出现了会话标识（应为 ***）:\n%s", logBuf.String())
	}
}

// TestSessionTokenStoredHashed 钉住「数据库里没有可直接使用的令牌」
// （store 侧的进一步断言，见 internal/store/session_token_test.go）。
func TestSessionTokenStoredHashed(t *testing.T) {
	env := newSessionLeakEnv(t)

	var id, tokenHash string
	if err := env.db.QueryRow(
		`SELECT id, token_hash FROM sessions WHERE user_id = ?`, env.memberUserID,
	).Scan(&id, &tokenHash); err != nil {
		t.Fatalf("read session row: %v", err)
	}
	if strings.Contains(id, env.memberToken) || strings.Contains(tokenHash, env.memberToken) {
		t.Fatalf("sessions 行里出现了令牌原文: id=%q token_hash=%q", id, tokenHash)
	}
	if tokenHash != auth.HashSessionToken(env.memberToken) {
		t.Fatalf("token_hash 必须是令牌的 sha256（库里没有别的可校验材料）, got %q", tokenHash)
	}
}
