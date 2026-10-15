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

type recoveryTestEnv struct {
	mux      http.Handler
	db       *store.DB
	svc      *auth.Service
	userID   string
	username string
	cookie   *http.Cookie
}

func newRecoveryTestEnv(t *testing.T) *recoveryTestEnv {
	t.Helper()
	// 重置全局限速器（防止跨用例污染）
	rlRecoveryLoginIP = newFixedWindow(rlRecoveryLoginIPPerMin, time.Minute)
	rlRecoveryLoginUser = newFixedWindow(rlRecoveryLoginUserPerMin, time.Minute)

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

	uid, err := db.InsertUser(context.Background(), "", "recuser", "Rec User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	sess, err := svc.Sessions().Create(uid, "Test · Linux")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	authH := &authHandler{svc: svc}
	sessMW := svc.Sessions().Middleware
	mux := http.NewServeMux()
	mux.Handle("/v1/auth/me", noStore(sessMW(http.HandlerFunc(authH.me))))
	mux.Handle("/v1/auth/passkeys", noStore(sessMW(http.HandlerFunc(authH.passkeysRoot))))
	mux.Handle("/v1/auth/passkeys/", noStore(sessMW(http.HandlerFunc(authH.passkeysItem))))
	mux.Handle("/v1/auth/recovery/regenerate", noStore(sessMW(http.HandlerFunc(authH.recoveryRegenerate))))
	mux.Handle("/v1/auth/recovery/status", noStore(sessMW(http.HandlerFunc(authH.recoveryStatus))))
	mux.Handle("/v1/auth/", noStore(http.HandlerFunc(authH.ServeHTTP)))

	return &recoveryTestEnv{
		mux: mux, db: db, svc: svc, userID: uid, username: "recuser",
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}
}

func recReq(t *testing.T, env *recoveryTestEnv, method, path string, body any, withCookie bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withCookie {
		req.AddCookie(env.cookie)
	}
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, req)
	return rr
}

func TestRecovery_HTTP_RegenerateLoginStatus(t *testing.T) {
	env := newRecoveryTestEnv(t)

	// 未登录 regenerate → 401
	rr := recReq(t, env, "POST", "/v1/auth/recovery/regenerate", nil, false)
	if rr.Code != 401 {
		t.Fatalf("regenerate without cookie: %d %s", rr.Code, rr.Body.String())
	}

	// 登录后 regenerate
	rr = recReq(t, env, "POST", "/v1/auth/recovery/regenerate", nil, true)
	if rr.Code != 200 {
		t.Fatalf("regenerate: %d %s", rr.Code, rr.Body.String())
	}
	var regen map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &regen); err != nil {
		t.Fatalf("decode: %v", err)
	}
	codesAny, ok := regen["recoveryCodes"].([]any)
	if !ok || len(codesAny) == 0 {
		t.Fatalf("missing recoveryCodes: %v", regen)
	}
	code := codesAny[0].(string)

	// status
	rr = recReq(t, env, "GET", "/v1/auth/recovery/status", nil, true)
	if rr.Code != 200 {
		t.Fatalf("status: %d %s", rr.Code, rr.Body.String())
	}
	var st map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &st)
	if int(st["unusedCount"].(float64)) != len(codesAny) {
		t.Errorf("unusedCount=%v want %d", st["unusedCount"], len(codesAny))
	}

	// login with recovery code (no cookie)
	rr = recReq(t, env, "POST", "/v1/auth/recovery/login", map[string]string{
		"username": env.username, "code": code,
	}, false)
	if rr.Code != 200 {
		t.Fatalf("login: %d %s", rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	var gotSess string
	for _, c := range cookies {
		if c.Name == auth.SessionCookieName {
			gotSess = c.Value
		}
	}
	if gotSess == "" {
		t.Fatal("login should Set-Cookie session")
	}

	// 一次性：再兑失败
	rr = recReq(t, env, "POST", "/v1/auth/recovery/login", map[string]string{
		"username": env.username, "code": code,
	}, false)
	if rr.Code != 401 {
		t.Fatalf("reuse should 401, got %d %s", rr.Code, rr.Body.String())
	}

	// 新会话应能访问需登录的 status（证明会话中间件一致，可再走 passkeys 补建）
	env.cookie = &http.Cookie{Name: auth.SessionCookieName, Value: gotSess}
	rr = recReq(t, env, "GET", "/v1/auth/recovery/status", nil, true)
	if rr.Code != 200 {
		t.Fatalf("status with recovery session: %d", rr.Code)
	}
	rr = recReq(t, env, "GET", "/v1/auth/passkeys", nil, true)
	if rr.Code != 200 {
		t.Fatalf("passkeys with recovery session should work (re-enroll path), got %d %s", rr.Code, rr.Body.String())
	}
}

func TestRecovery_HTTP_BadLogin(t *testing.T) {
	env := newRecoveryTestEnv(t)
	rr := recReq(t, env, "POST", "/v1/auth/recovery/login", map[string]string{
		"username": env.username, "code": "AAAA-BBBB-CCCC-DDDD",
	}, false)
	if rr.Code != 401 {
		t.Fatalf("bad code: %d", rr.Code)
	}
	rr = recReq(t, env, "POST", "/v1/auth/recovery/login", map[string]string{
		"username": "", "code": "x",
	}, false)
	if rr.Code != 400 {
		t.Fatalf("missing fields: %d", rr.Code)
	}
}

// TestRecovery_HTTP_LoginRateLimitIP 同一 IP 超限 → 429。
func TestRecovery_HTTP_LoginRateLimitIP(t *testing.T) {
	env := newRecoveryTestEnv(t)
	rlRecoveryLoginIP = newFixedWindow(3, time.Minute)
	rlRecoveryLoginUser = newFixedWindow(100, time.Minute) // 放宽 user 维，只测 IP

	body := map[string]string{"username": env.username, "code": "AAAA-BBBB-CCCC-DDDD"}
	for i := 0; i < 3; i++ {
		rr := recReq(t, env, "POST", "/v1/auth/recovery/login", body, false)
		if rr.Code == 429 {
			t.Fatalf("attempt %d should not be rate-limited yet, got 429", i+1)
		}
		if rr.Code != 401 {
			t.Fatalf("attempt %d: want 401 (bad code), got %d %s", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := recReq(t, env, "POST", "/v1/auth/recovery/login", body, false)
	if rr.Code != 429 {
		t.Fatalf("4th attempt should 429, got %d %s", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra != "60" {
		t.Errorf("Retry-After=%q want 60", ra)
	}
}

// TestRecovery_HTTP_LoginRateLimitUser 同一 username（大小写不敏感）超限 → 429。
func TestRecovery_HTTP_LoginRateLimitUser(t *testing.T) {
	env := newRecoveryTestEnv(t)
	rlRecoveryLoginIP = newFixedWindow(100, time.Minute) // 放宽 IP
	rlRecoveryLoginUser = newFixedWindow(3, time.Minute)

	for i, user := range []string{env.username, "RecUser", "RECUSER"} {
		rr := recReq(t, env, "POST", "/v1/auth/recovery/login", map[string]string{
			"username": user, "code": "AAAA-BBBB-CCCC-DDDD",
		}, false)
		if rr.Code == 429 {
			t.Fatalf("attempt %d (%q) should not be rate-limited yet, got 429", i+1, user)
		}
		if rr.Code != 401 {
			t.Fatalf("attempt %d: want 401, got %d %s", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := recReq(t, env, "POST", "/v1/auth/recovery/login", map[string]string{
		"username": env.username, "code": "AAAA-BBBB-CCCC-DDDD",
	}, false)
	if rr.Code != 429 {
		t.Fatalf("4th attempt should 429, got %d %s", rr.Code, rr.Body.String())
	}
}
