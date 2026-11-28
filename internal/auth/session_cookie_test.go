package auth

// 会话 Cookie 的属性一致性（#624）。
//
// 为什么需要这一条：SetCookie 与 ClearCookie 曾经各自内联一个 &http.Cookie{}，
// ClearCookie 少了 SameSite、少了 Expires。浏览器把「名字相同但属性不同」的 Cookie
// 当作两个不同的 cookie，于是否则会出现：
//
//	登出 → 响应里带着属性不完整的清除 Cookie → 浏览器保留了旧的那条
//	     → 服务端 session 已删 → 每个请求 401，但前端每次刷新还"登录着"。
//
// 全仓此前没有任何 cookie 属性单测（rg 'SameSite|HttpOnly|Max-Age' internal/**/*_test.go
// 零命中），所以这个不一致一直是静默的。

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClearCookieMatchesSetCookieAttributes 逐项比对 SetCookie 与 ClearCookie 的属性。
func TestClearCookieMatchesSetCookieAttributes(t *testing.T) {
	// 两轮：Secure 由 RP_ORIGIN 推导（#624），两条路径都必须自洽。
	for _, secure := range []bool{false, true} {
		name := "secure=false"
		if secure {
			name = "secure=true"
		}
		t.Run(name, func(t *testing.T) {
			sm := NewSessionManager(newTestDB(t), time.Hour, secure)

			setC := issueCookie(t, func(w http.ResponseWriter) { sm.SetCookie(w, "sess-abc123") })
			clearC := issueCookie(t, func(w http.ResponseWriter) { sm.ClearCookie(w) })

			// 属性逐项一致：任何一项不同，浏览器就可能不认这条清除指令。
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
			// 判据本身也要钉住：属性「一致地错」同样是缺陷。
			if setC.Name != SessionCookieName {
				t.Errorf("Name = %q, want %q", setC.Name, SessionCookieName)
			}
			if setC.Path != "/" || !setC.HttpOnly || setC.SameSite != http.SameSiteLaxMode {
				t.Errorf("签发属性不符：Path=%q HttpOnly=%v SameSite=%v", setC.Path, setC.HttpOnly, setC.SameSite)
			}
			if setC.Secure != secure || clearC.Secure != secure {
				t.Errorf("Secure 必须跟随 SessionManager.secure=%v：SetCookie=%v ClearCookie=%v", secure, setC.Secure, clearC.Secure)
			}

			// 清除语义：空值 + MaxAge<0（Max-Age 优先于 Expires，浏览器立刻删）。
			if clearC.Value != "" {
				t.Errorf("ClearCookie 的 Value 必须为空串（实际 %q）", clearC.Value)
			}
			if clearC.MaxAge >= 0 {
				t.Errorf("ClearCookie 的 MaxAge 必须 < 0（删除指令），实际 %d", clearC.MaxAge)
			}
			// 对照：签发那条不能带删除指令。
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

// issueCookie 让 f 往一个 Recorder 里写 Set-Cookie，再把那条 Cookie 解析回来。
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
