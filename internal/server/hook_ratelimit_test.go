package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// newHookRateLimitEnv 建一个带 webhook 源的环境，并把入口限流器调成小窗口。
//
// 为什么必须换掉包级限流器：它按分钟计数，同一个进程里跑两次测试就会互相污染
// （第一次用完 60 额度，第二次一上来就 429）。用 t.Cleanup 还原是硬要求。
func newHookRateLimitEnv(t *testing.T, perMin int) *nsTestEnv {
	t.Helper()
	env := newNSTestEnv(t)
	src := &store.Source{
		NamespaceID: env.nsID, Name: "rl-src", Kind: store.SourceKindWebhook,
		Config:  json.RawMessage(`{"token":"tok-rl"}`),
		Enabled: true,
		Targets: []store.SourceTarget{{CapabilityID: env.builtinCapID(t, env.nsID, "notify.phone")}},
	}
	if err := env.db.CreateSource(context.Background(), src); err != nil {
		t.Fatalf("create webhook source: %v", err)
	}
	old := rlHookIP
	rlHookIP = newFixedWindow(perMin, time.Minute)
	t.Cleanup(func() { rlHookIP = old })
	return env
}

// TestHookRateLimited 公开 webhook 入口必须限流：它是唯一不要登录的写端点。
//
// 断言三件事，缺一不可：
//  1. 额度内不被拒（不能把正常调试也挡了）
//  2. 超出立刻 429（不是"慢慢变慢"）
//  3. 响应带 Retry-After（调用方要能知道等多久，而不是盲目重试把额度烧光）
func TestHookRateLimited(t *testing.T) {
	env := newHookRateLimitEnv(t, 2)

	for i := 1; i <= 2; i++ {
		rr := env.doAnon(t, http.MethodPost, "/v1/hooks/tok-rl", map[string]any{"type": "ping"})
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次就被限流了，额度 2 不该这么快用完", i)
		}
	}

	rr := env.doAnon(t, http.MethodPost, "/v1/hooks/tok-rl", map[string]any{"type": "ping"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("超额度后 status = %d, want 429（body=%s）", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want \"60\"", got)
	}
	if !strings.Contains(rr.Body.String(), "请求过于频繁") {
		t.Fatalf("429 要给人话，got %s", rr.Body.String())
	}
}

// TestHookRateLimitedNotByToken 限流键是 IP，不是 token。
//
// 这是刻意的选择：token 是攻击者手里**可以换**的东西（多个源各有一个），
// 按 token 限流等于换个 token 就绕过。这里用一个新 token 证明"换 token 无效"。
func TestHookRateLimitedNotByToken(t *testing.T) {
	env := newHookRateLimitEnv(t, 1)

	// 第二个源：token 不同，但 IP 相同
	src := &store.Source{
		NamespaceID: env.nsID, Name: "rl-src-2", Kind: store.SourceKindWebhook,
		Config:  json.RawMessage(`{"token":"tok-rl-2"}`),
		Enabled: true,
		Targets: []store.SourceTarget{{CapabilityID: env.builtinCapID(t, env.nsID, "notify.phone")}},
	}
	if err := env.db.CreateSource(context.Background(), src); err != nil {
		t.Fatalf("create webhook source 2: %v", err)
	}

	env.doAnon(t, http.MethodPost, "/v1/hooks/tok-rl", map[string]any{"type": "ping"}) // 用掉额度
	rr := env.doAnon(t, http.MethodPost, "/v1/hooks/tok-rl-2", map[string]any{"type": "ping"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("换 token 就绕过了限流（status=%d）—— 限流键必须是 IP", rr.Code)
	}
}

// TestHookRateLimitPerIP 不同 IP 各自计额度（否则一个人能拖垮所有人的 webhook）。
func TestHookRateLimitPerIP(t *testing.T) {
	env := newHookRateLimitEnv(t, 1)

	old := trustProxyHeaders
	trustProxyHeaders = true
	t.Cleanup(func() { trustProxyHeaders = old })

	hit := func(ip string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/tok-rl", strings.NewReader(`{"type":"ping"}`))
		req.Header.Set("X-Forwarded-For", ip)
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := hit("203.0.113.1"); code == http.StatusTooManyRequests {
		t.Fatalf("第一个 IP 第一次就 429")
	}
	if code := hit("203.0.113.1"); code != http.StatusTooManyRequests {
		t.Fatalf("同 IP 第二次 status = %d, want 429", code)
	}
	if code := hit("203.0.113.2"); code == http.StatusTooManyRequests {
		t.Fatalf("另一个 IP 被前一 IP 的额度连累了（status=429）")
	}
}
