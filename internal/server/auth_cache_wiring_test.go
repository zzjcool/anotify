package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件锁的是**接线**（wiring），不是缓存本身。
//
// 为什么单开一个文件、单建两个小 mux：三个缓存（API Key / 会话 / hooks 限流）
// 各自在 internal/auth 与 internal/server 里有自己的单测，但「谁调用了失效钩子」
// 这件事只发生在装配层——`mux.go` / `handlers.go` / `admin_handlers.go` 的那几行。
// 那几行是**最容易在后续重构里被悄悄丢掉**的地方，而丢掉它们的后果是静默的安全回归
// （被吊销的 key 还能用一个 TTL；被禁用的用户还能用一个 TTL），不会有任何报错。
//
// 所以这里不复用 nsTestEnv（它没挂 /v1/keys 与 /v1/admin 路由），而是按真实装配
// 形状搭最小 mux，走完整的 HTTP → 中间件 → handler → store 链路。

// wiringEnv 是最小装配环境：一个真 store、一个真 auth.Service、两种中间件。
type wiringEnv struct {
	db     *store.DB
	svc    *auth.Service
	mux    *http.ServeMux
	userID string
	cookie *http.Cookie
}

// newWiringEnv 建一个用户 + 一个会话，并挂上「受保护端点」与 /v1/keys 路由。
func newWiringEnv(t *testing.T) *wiringEnv {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(t.TempDir() + "/wiring.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify Wiring Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	uid := store.NewUserID()
	if _, err := db.RegisterUser(ctx, &store.User{
		ID: uid, Username: "u-" + uid, DisplayName: "Wiring", CreatedAt: store.Now(),
	}); err != nil {
		t.Fatalf("register user: %v", err)
	}
	sess, err := svc.Sessions().Create(uid, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	sessMW := svc.Sessions().Middleware
	keysH := &keysHandler{keys: svc.Keys(), db: db}

	mux := http.NewServeMux()
	// 受保护端点：只要过了会话中间件就 200。用来观察「禁用是否立即生效」。
	mux.Handle("GET /protected", noStore(sessMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"ok": "yes"})
	}))))
	// Key 管理（签发 / 吊销），走会话。
	mux.Handle("/v1/keys", noStore(sessMW(keysH)))
	mux.Handle("/v1/keys/", noStore(sessMW(keysH)))

	return &wiringEnv{
		db: db, svc: svc, mux: mux, userID: uid,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}
}

func (e *wiringEnv) do(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(e.cookie)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// TestKeyRevokeViaHTTPInvalidatesCache 锁住 `keysHandler.revoke` 走的是
// `KeyManager.RevokeKey`（带失效），而不是 `db.RevokeAPIKey`（绕过失效）。
//
// 判据：先把缓存打热（ValidateKey 一次 → 命中路径 0 次 DB），再走 HTTP 吊销，
// 然后**把 DB 关掉**再校验——必须失败。如果 revoke 绕过了缓存失效，这一步会命中
// 旧条目直接放行（因为命中路径压根不碰 DB），测试立刻变红。
func TestKeyRevokeViaHTTPInvalidatesCache(t *testing.T) {
	env := newWiringEnv(t)
	km := env.svc.Keys()

	plaintext, rec, err := km.CreateKey(env.userID, "wiring-key", []string{auth.ScopeNotifySend})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	// 打热缓存。
	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("首次校验应成功: %v", err)
	}

	rr := env.do(t, http.MethodPost, "/v1/keys/"+rec.ID+"/revoke")
	if rr.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body=%s", rr.Code, rr.Body.String())
	}

	// 关掉 DB：此时唯一的放行来源只能是缓存里的旧条目。
	if err := env.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, _, err := km.ValidateKey(plaintext); err == nil {
		t.Fatalf("HTTP 吊销后必须立刻失效（不得继续吃缓存条目直到 TTL）")
	}
}

// TestAdminDisableInvalidatesSessionCache 锁住 `adminHandler.applyDisabledChange`
// 在写库成功后调了 `Sessions().InvalidateUser`。
//
// 判据：先让被禁用用户请求一次 `/protected`（会话缓存打热），超管禁用后再请求——
// 必须立刻 401。没有失效钩子的话这里会是 200，直到缓存 TTL（默认 10s）过去。
func TestAdminDisableInvalidatesSessionCache(t *testing.T) {
	env := newWiringEnv(t)
	ctx := context.Background()

	// 超管（admin）+ 被管用户（member，与 env.userID 同一个，简化：直接把他提成 member、另建 admin）
	if _, err := env.db.UpdateUserRole(ctx, env.userID, store.RoleMember); err != nil {
		t.Fatalf("set member role: %v", err)
	}
	adminID := store.NewUserID()
	if _, err := env.db.RegisterUser(ctx, &store.User{
		ID: adminID, Username: "adm-" + adminID, DisplayName: "Admin", CreatedAt: store.Now(),
	}); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := env.db.UpdateUserRole(ctx, adminID, store.RoleAdmin); err != nil {
		t.Fatalf("set admin role: %v", err)
	}
	adminSess, err := env.svc.Sessions().Create(adminID, "admin")
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}

	// 把 admin 端点挂上（与 mux.go 同形状：sessMW → AdminMiddleware → adminHandler）。
	adminH := &adminHandler{db: env.db, svc: env.svc}
	env.mux.Handle("/v1/admin/", noStore(env.svc.Sessions().Middleware(
		env.svc.AdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			adminH.ServeHTTP(w, r)
		})),
	)))

	// ① 打热被禁用用户的会话缓存。
	if rr := env.do(t, http.MethodGet, "/protected"); rr.Code != http.StatusOK {
		t.Fatalf("预热请求应 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	// ② 超管禁用他。
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+env.userID+"/disable", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: adminSess.ID})
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("禁用 status = %d, body=%s", rr.Code, rr.Body.String())
	}

	// ③ 必须立刻 401（不是「等一个 cacheTTL」）。
	if rr := env.do(t, http.MethodGet, "/protected"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("禁用后必须立刻 401，got %d（说明会话缓存没有失效）", rr.Code)
	}
}
