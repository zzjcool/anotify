package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// newHookRateLimitEnv 建一个带 webhook 源的环境，并把入口限流器调成小窗口。
//
// 为什么必须换掉包级限流器：它按分钟计数，同一个进程里跑两次测试就会互相污染
// （第一次用完额度，第二次一上来就 429）。用 t.Cleanup 还原是硬要求。
//
// 两个维度都要摆：token 维给一个足够大的额度，否则它会先于 IP 维把既有断言打乱
// （perMin 在这些老用例里只代表 IP 维的额度）。
func newHookRateLimitEnv(t *testing.T, perMin int) *nsTestEnv {
	t.Helper()
	return newHookRateLimitEnvTuned(t, perMin, testHookTokenPerMinUnlimited)
}

// testHookTokenPerMinUnlimited 是“token 维不参与”的大额度（既不是 nil，也不影响结论）。
const testHookTokenPerMinUnlimited = 1_000_000

// newHookRateLimitEnvTuned 分别指定 IP 维与 token 维的额度。
func newHookRateLimitEnvTuned(t *testing.T, ipPerMin, tokenPerMin int) *nsTestEnv {
	t.Helper()
	env := newNSTestEnv(t)
	createWebhookSource(t, env, "rl-src", "tok-rl")

	oldIP, oldToken := rlHookIP, rlHookToken
	rlHookIP = newFixedWindow(ipPerMin, time.Minute)
	rlHookToken = newFixedWindow(tokenPerMin, time.Minute)
	t.Cleanup(func() { rlHookIP, rlHookToken = oldIP, oldToken })
	return env
}

// createWebhookSource 直接走 store 建一个 webhook 源（token 就是它的 URL 段）。
func createWebhookSource(t *testing.T, env *nsTestEnv, name, token string) *store.Source {
	t.Helper()
	src := &store.Source{
		NamespaceID: env.nsID, Name: name, Kind: store.SourceKindWebhook,
		Config:  json.RawMessage(`{"token":"` + token + `"}`),
		Enabled: true,
		Targets: []store.SourceTarget{{CapabilityID: env.builtinCapID(t, env.nsID, "notify.phone")}},
	}
	if err := env.db.CreateSource(context.Background(), src); err != nil {
		t.Fatalf("create webhook source %s: %v", name, err)
	}
	return src
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

// TestHookRateLimitPerToken token 维独立生效：同 IP、不同 token，各有各的额度。
//
// 为什么必须有这条：只按 IP 限流会把「一个出口 IP 后面多个源」或「批量告警回放」
// 按总量掐死（#245 现场就是这个）。这里把 IP 维度给足，证明 token 维能单独拒人，
// 且一个 token 打满不会连累另一个 token。
func TestHookRateLimitPerToken(t *testing.T) {
	// IP 额度给足（避免 IP 维先拒），token 额度 2。
	env := newHookRateLimitEnvTuned(t, 1_000_000, 2)
	createWebhookSource(t, env, "rl-src-b", "tok-b")

	ping := func(token string) int {
		return env.doAnon(t, http.MethodPost, "/v1/hooks/"+token, map[string]any{"type": "ping"}).Code
	}

	// A token 用满额度
	for i := 1; i <= 2; i++ {
		if code := ping("tok-rl"); code != http.StatusOK {
			t.Fatalf("A token 第 %d 次 status = %d, want 200（额度 2 不该这么快用完）", i, code)
		}
	}

	// A 打满后，换成 B 仍然能过（token 维是独立的桶，且真实跑到了派发 200）
	if code := ping("tok-b"); code != http.StatusOK {
		t.Fatalf("B token 在 A 打满后被拒：status=%d, want 200 —— token 维必须分桶", code)
	}

	// 回头再打 A：仍应 429（A 的桶已经是满的）
	if code := ping("tok-rl"); code != http.StatusTooManyRequests {
		t.Fatalf("A token 超额后 status = %d, want 429", code)
	}
}

// TestHookRetryAfterRemaining 429 的 Retry-After 必须是真实剩余秒数，且落在 [1,60]。
//
// 老实现硬编码 60：窗口刚开就被拒的调用方白等一分钟，且所有客户端会在窗口边界
// 一起重试（脉冲）。这里只要断言“整数、在 [1,60] 内”，不绑定具体实现。
func TestHookRetryAfterRemaining(t *testing.T) {
	env := newHookRateLimitEnv(t, 1) // IP 维额度 1

	env.doAnon(t, http.MethodPost, "/v1/hooks/tok-rl", map[string]any{"type": "ping"}) // 用掉额度
	rr := env.doAnon(t, http.MethodPost, "/v1/hooks/tok-rl", map[string]any{"type": "ping"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}

	raw := rr.Header().Get("Retry-After")
	secs, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After = %q 不是整数秒：%v", raw, err)
	}
	if secs < 1 || secs > 60 {
		t.Fatalf("Retry-After = %d, want 落在 [1,60]", secs)
	}
}

// TestRateLimit429LogsOnce 一个 429 只产生**一条**日志。
//
// 这是 issue #245 的第二个投诉：老实现限流器写一条 WARN、accessLog 再写一条 WARN，
// c=16 打满时 10 秒刷 75MB。这里接管 slog 默认 logger 逐条计数。
func TestRateLimit429LogsOnce(t *testing.T) {
	env := newHookRateLimitEnv(t, 1) // IP 维额度 1

	old := slog.Default()
	rec := &recordingHandler{}
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(old) })

	// 生产链路的最外层就是 accessLog（mux.go: accessLog(mux)），这里原样包一层。
	h := accessLog(env.mux)
	hit := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/hooks/tok-rl", strings.NewReader(`{"type":"ping"}`))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	hit() // 用掉额度
	if code := hit(); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", code)
	}

	var rateLimitedLogs, req429Logs, loggedRetryAfter int
	var scope string
	for _, r := range rec.snapshot() {
		if r.msg == "rate limited" {
			rateLimitedLogs++
		}
		if r.msg != "request" {
			continue
		}
		if status, ok := logAttrInt(r.attrs, "status"); ok && status == http.StatusTooManyRequests {
			req429Logs++
			if s, ok := r.attrs["rate_limit"].(string); ok {
				scope = s
			}
			loggedRetryAfter, _ = logAttrInt(r.attrs, "retry_after")
		}
	}
	if rateLimitedLogs != 0 {
		t.Fatalf("限流器自己又写了一条 %q 日志（429 只允许 accessLog 写一份）", "rate limited")
	}
	if req429Logs != 1 {
		t.Fatalf("429 相关日志 %d 条，want 恰好 1 条", req429Logs)
	}
	// 唯一那一条要能看出是哪个维度拒的、让对方等多久（内部细节只进日志，不进响应头）。
	if scope != "hook:ip" {
		t.Fatalf("429 日志的 rate_limit = %q, want \"hook:ip\"", scope)
	}
	if loggedRetryAfter < 1 || loggedRetryAfter > 60 {
		t.Fatalf("429 日志的 retry_after = %d, want 落在 [1,60]", loggedRetryAfter)
	}
}

// TestHookRateLimitDisabled 配置负数 = 显式关闭该维度（运维选择，不能报错也不能拒人）。
func TestHookRateLimitDisabled(t *testing.T) {
	env := newHookRateLimitEnv(t, 1) // IP 额度 1：不关的话第二次就 429

	prevIP, prevToken := rlHookIP, rlHookToken
	configureHookRateLimit(-1, -1)
	t.Cleanup(func() { rlHookIP, rlHookToken = prevIP, prevToken })

	if rlHookIP != nil || rlHookToken != nil {
		t.Fatalf("configureHookRateLimit(-1,-1) 之后两个限速器都该是 nil，got ip=%v token=%v", rlHookIP, rlHookToken)
	}
	for i := 0; i < 5; i++ {
		rr := env.doAnon(t, http.MethodPost, "/v1/hooks/tok-rl", map[string]any{"type": "ping"})
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("维度已关闭，第 %d 次仍然 429", i+1)
		}
	}
}

// TestHookRateEnvParsing 环境变量解析语义（契约 §2.2）：
// 空 / 0 / 非法 → 默认；负数 / off → 关闭（<0）；正数 → 原值。
func TestHookRateEnvParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", defaultHookRatePerMin},
		{"0", defaultHookRatePerMin},
		{"abc", defaultHookRatePerMin},
		{"-5", -5},
		{"off", -1},
		{"90", 90},
	}
	for _, c := range cases {
		t.Setenv("ANOTIFY_HOOK_RATE_PER_MIN", c.raw)
		if got := FromEnv().HookRatePerMin; got != c.want {
			t.Errorf("ANOTIFY_HOOK_RATE_PER_MIN=%q → %d, want %d", c.raw, got, c.want)
		}
	}

	// 结构体零值（不经 FromEnv 构造）也必须等于默认，不能等于“关闭”。
	zero := Config{}
	perToken, perIP := zero.hookRateLimits()
	if perToken != defaultHookRatePerMin || perIP != defaultHookRatePerIPPerMin {
		t.Fatalf("Config{} 的 hookRateLimits() = (%d,%d), want (%d,%d)",
			perToken, perIP, defaultHookRatePerMin, defaultHookRatePerIPPerMin)
	}
}

// recordingHandler 是最小的 slog.Handler：逐条记下 msg/level/attrs，供测试计数。
type recordingHandler struct {
	mu      sync.Mutex
	records []recordedLog
}

type recordedLog struct {
	msg   string
	level slog.Level
	attrs map[string]any
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	rec := recordedLog{msg: r.Message, level: r.Level, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) snapshot() []recordedLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]recordedLog, len(h.records))
	copy(out, h.records)
	return out
}

// logAttrInt 把 record 里的数值 attr 取成 int（slog 会保留调用方传的 int 类型）。
func logAttrInt(attrs map[string]any, key string) (int, bool) {
	v, ok := attrs[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}
