package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClearCookieMatchesSetCookieAttributes(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "secure=false"
		if secure {
			name = "secure=true"
		}
		t.Run(name, func(t *testing.T) {
			sm := NewSessionManager(newTestDB(t), time.Hour, secure)

			setC := issueCookie(t, func(w http.ResponseWriter) { sm.SetCookie(w, "sess-abc123") })
			clearC := issueCookie(t, func(w http.ResponseWriter) { sm.ClearCookie(w) })

			if setC.Name != clearC.Name {
				t.Errorf("Name 不一致：SetCookie=%q ClearCookie=%q", setC.Name, clearC.Name)
			}
			if setC.Path != clearC.Path {
				t.Errorf("Path 不一致：SetCookie=%q ClearCookie=%q", setC.Path, clearC.Path)
			}
			if setC.Domain != clearC.Domain {
				t.Errorf("Domain 不一致：SetCookie=%q ClearCookie=%q", setC.Domain, clearC.Domain)
			}
			if setC.SameSite != clearC.SameSite {
				t.Errorf("SameSite 不一致：SetCookie=%v ClearCookie=%v", setC.SameSite, clearC.SameSite)
			}
			if setC.Secure != clearC.Secure {
				t.Errorf("Secure 不一致：SetCookie=%v ClearCookie=%v", setC.Secure, clearC.Secure)
			}
			if setC.HttpOnly != clearC.HttpOnly {
				t.Errorf("HttpOnly 不一致：SetCookie=%v ClearCookie=%v", setC.HttpOnly, clearC.HttpOnly)
			}
			if setC.Name != SessionCookieName {
				t.Errorf("Name = %q, want %q", setC.Name, SessionCookieName)
			}
			if setC.Path != "/" || !setC.HttpOnly || setC.SameSite != http.SameSiteLaxMode {
				t.Errorf("签发属性不符：Path=%q HttpOnly=%v SameSite=%v", setC.Path, setC.HttpOnly, setC.SameSite)
			}
			if setC.Secure != secure || clearC.Secure != secure {
				t.Errorf("Secure 必须跟随 SessionManager.secure=%v：SetCookie=%v ClearCookie=%v", secure, setC.Secure, clearC.Secure)
			}

			if clearC.Value != "" {
				t.Errorf("ClearCookie 的 Value 必须为空串（实际 %q）", clearC.Value)
			}
			if clearC.MaxAge >= 0 {
				t.Errorf("ClearCookie 的 MaxAge 必须 < 0（删除指令），实际 %d", clearC.MaxAge)
			}
			if setC.Value == "" {
				t.Error("SetCookie 的 Value 不应为空串")
			}
			if setC.MaxAge != 0 {
				t.Errorf("SetCookie 不应带 MaxAge（用 Expires 表达 TTL），实际 %d", setC.MaxAge)
			}
			if clearC.Expires.IsZero() {
				t.Error("ClearCookie 也必须带 Expires（属性集合与 SetCookie 一致），实际为零值")
			}
		})
	}
}

func issueCookie(t *testing.T, f func(http.ResponseWriter)) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	f(rec)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("必须恰好写一条 Set-Cookie，实际 %d 条（%v）", len(cookies), rec.Header().Values("Set-Cookie"))
	}
	return cookies[0]
}
