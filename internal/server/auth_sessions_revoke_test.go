package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

func sessionListDoc(t *testing.T, env *passkeysTestEnv, target string) (ids []string, current []string, body string) {
	t.Helper()
	rr := pkReq(t, env, http.MethodGet, target, nil, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, rr.Code, rr.Body.String())
	}
	var doc struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode %s: %v", target, err)
	}
	for _, s := range doc.Sessions {
		id, _ := s["id"].(string)
		ids = append(ids, id)
		if cur, ok := s["current"].(bool); ok && cur {
			current = append(current, id)
		}
	}
	return ids, current, rr.Body.String()
}

func TestAuthSessions_ListNeverLeaksToken(t *testing.T) {
	env := newPasskeysTestEnv(t)

	others := []*store.Session{}
	for i := 0; i < 2; i++ {
		s, err := env.svc.Sessions().Create(env.userID, "Chrome · Linux")
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		others = append(others, s)
	}

	target := "/v1/auth/sessions?limit=1"
	var allIDs, allCurrent []string
	for page := 0; page < 10; page++ {
		ids, current, body := sessionListDoc(t, env, target)
		allIDs = append(allIDs, ids...)
		allCurrent = append(allCurrent, current...)

		if strings.Contains(body, env.cookie.Value) {
			t.Fatalf("响应体包含会话令牌原文（#587 回归）：%s", body)
		}
		for _, s := range others {
			if strings.Contains(body, s.ID) {
				t.Fatalf("响应体包含别的会话令牌原文（#587 回归）：%s", body)
			}
		}

		var doc map[string]any
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		cont, _ := doc["continue"].(string)
		if cont == "" {
			break
		}
		keys, err := decodePageCursor(cont, "sessions|"+env.userID, 2)
		if err != nil {
			t.Fatalf("游标解不开：%v", err)
		}
		for _, k := range keys {
			if k == env.cookie.Value {
				t.Fatalf("游标里带着会话令牌原文（#587 回归）：%v", keys)
			}
		}
		target = "/v1/auth/sessions?limit=1&continue=" + cont
	}

	if len(allIDs) != 3 {
		t.Fatalf("应列出 3 条会话，got %d: %v", len(allIDs), allIDs)
	}
	if len(allCurrent) != 1 {
		t.Fatalf("current 会话应恰好 1 条，got %d: %v", len(allCurrent), allCurrent)
	}
	if allCurrent[0] != auth.SessionPublicID(env.cookie.Value) {
		t.Fatalf("current 应是登录会话的公开 id %q，got %q",
			auth.SessionPublicID(env.cookie.Value), allCurrent[0])
	}
	for _, id := range allIDs {
		if id == env.cookie.Value {
			t.Fatal("id 不能是 Cookie 原文")
		}
		for _, s := range others {
			if id == s.ID {
				t.Fatal("id 不能是会话令牌原文")
			}
		}
	}
}

func TestAuthSessions_RevokeOwnSession(t *testing.T) {
	env := newPasskeysTestEnv(t)
	victim, err := env.svc.Sessions().Create(env.userID, "Chrome · Linux")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	rr := pkReq(t, env, http.MethodDelete, "/v1/auth/sessions/"+auth.SessionPublicID(victim.ID), nil, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("吊销本人会话应 200，got %d %s", rr.Code, rr.Body.String())
	}

	meReq := httptest.NewRequest(http.MethodGet, "/v1/auth/me", nil)
	meReq.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: victim.ID})
	meRec := httptest.NewRecorder()
	env.mux.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusUnauthorized {
		t.Fatalf("被吊销的会话访问 /v1/auth/me 应 401，got %d %s", meRec.Code, meRec.Body.String())
	}

	ids, current, _ := sessionListDoc(t, env, "/v1/auth/sessions")
	for _, id := range ids {
		if id == auth.SessionPublicID(victim.ID) {
			t.Fatal("已吊销的会话仍在列表里")
		}
	}
	if len(current) != 1 || current[0] != auth.SessionPublicID(env.cookie.Value) {
		t.Fatalf("当前会话应仍然有效且标记 current，got %v", current)
	}
}

func TestAuthSessions_RevokeOthersIs404(t *testing.T) {
	env := newPasskeysTestEnv(t)
	otherUID, err := env.db.InsertUser(context.Background(), "", "otheruser", "Other")
	if err != nil {
		t.Fatalf("insert other user: %v", err)
	}
	other, err := env.svc.Sessions().Create(otherUID, "Firefox · macOS")
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}

	rr := pkReq(t, env, http.MethodDelete, "/v1/auth/sessions/"+auth.SessionPublicID(other.ID), nil, true)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("吊销别人的会话应 404，got %d %s", rr.Code, rr.Body.String())
	}
	if _, err := env.svc.Sessions().Validate(other.ID); err != nil {
		t.Fatalf("别人的会话被误删了（404 却真的删了）：%v", err)
	}

	rr = pkReq(t, env, http.MethodDelete, "/v1/auth/sessions/"+other.ID, nil, true)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("拿令牌原文当 id 应 404，got %d %s", rr.Code, rr.Body.String())
	}
}

func TestAuthSessions_RevokeAnonymousIs401(t *testing.T) {
	env := newPasskeysTestEnv(t)
	target, err := env.svc.Sessions().Create(env.userID, "Chrome · Linux")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rr := pkReq(t, env, http.MethodDelete, "/v1/auth/sessions/"+auth.SessionPublicID(target.ID), nil, false)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("未登录吊销应 401，got %d %s", rr.Code, rr.Body.String())
	}
	if _, err := env.svc.Sessions().Validate(target.ID); err != nil {
		t.Fatalf("匿名请求不该删掉会话：%v", err)
	}
}

func TestAuthSessions_RevokeCurrentIsLogout(t *testing.T) {
	env := newPasskeysTestEnv(t)
	rr := pkReq(t, env, http.MethodDelete, "/v1/auth/sessions/"+auth.SessionPublicID(env.cookie.Value), nil, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("吊销当前会话应 200，got %d %s", rr.Code, rr.Body.String())
	}
	cleared := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName && (c.MaxAge < 0 || c.Value == "") {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("吊销当前会话必须清 anotify_session，got %q", rr.Header().Get("Set-Cookie"))
	}
	if _, err := env.svc.Sessions().Validate(env.cookie.Value); err == nil {
		t.Fatal("当前会话删了库里还留着")
	}
}

func TestAuthSessions_RevokeMethodNotAllowed(t *testing.T) {
	env := newPasskeysTestEnv(t)
	rr := pkReq(t, env, http.MethodPost, "/v1/auth/sessions/"+auth.SessionPublicID(env.cookie.Value), nil, true)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/auth/sessions/{id} 应 405，got %d %s", rr.Code, rr.Body.String())
	}
}
