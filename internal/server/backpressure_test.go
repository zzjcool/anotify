package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件钉住背压中间件的 9 条行为规格（契约 §1.2）。最容易在"优化"里被静默改坏
// 的是这两条，所以都是**显式**断言而不是"大概没出事"：
//
//	① 未超阈时请求路径上 **0 次 DB 往返** —— fake 一被调用就 t.Fatal；
//	② 采样出错**保持上一次状态** —— 既不解保也不加保（一次抖动不该改变全站行为）。

const bpMaxAge = 120 * time.Second

// syncBuffer 是并发安全的日志缓冲（Run 的采样在 goroutine 上写，测试在读）。
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureBackpressureLog 把默认 slog 换成写进 buffer 的 TextHandler。
func captureBackpressureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// countEvent 数某条事件在日志里出现了几次（event=server.backpressure.xxx）。
func countEvent(logs, event string) int {
	return strings.Count(logs, "event="+event)
}

// eventLine 返回第一条带该事件的日志行；找不到直接失败。
func eventLine(t *testing.T, logs, event string) string {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "event="+event) {
			return line
		}
	}
	t.Fatalf("日志里没有 event=%s：\n%s", event, logs)
	return ""
}

// mustContain 断言日志行里带着某个字段（TextHandler 的 `key=value`）。
func mustContain(t *testing.T, line, want string) {
	t.Helper()
	if !strings.Contains(line, want) {
		t.Fatalf("日志行缺 %q：%s", want, line)
	}
}

// testClock 是可手动推进的时钟：阈值边界（age > MaxAge 这条严格不等号）
// 必须能精确摆布，靠真实时间流逝只会得到"有时红有时绿"。
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// oldestFor 返回一个能让 age 恰好等于 d 的 created_at（unix 秒，即数据层的真实口径）。
func (c *testClock) oldestFor(d time.Duration) int64 {
	return c.now().Add(-d).Unix()
}

// fakeAgeSource 是可控的 pendingAgeSource：手动摆年龄、手动造错，
// 还能进入"一被调用就让测试失败"的模式 —— 这是证明请求路径零 DB 往返的硬证据。
type fakeAgeSource struct {
	tb testing.TB

	mu        sync.Mutex
	calls     int
	oldest    int64
	err       error
	forbidden string // 非空 = 一被调用就 t.Fatalf(forbidden)
}

func newFakeAgeSource(tb testing.TB) *fakeAgeSource { return &fakeAgeSource{tb: tb} }

func (f *fakeAgeSource) set(oldest int64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oldest, f.err = oldest, err
}

// forbid 之后任何一次采样调用都是 bug（用来把"计数为 0"升级成"被调用就炸"）。
func (f *fakeAgeSource) forbid(why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forbidden = why
}

func (f *fakeAgeSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAgeSource) OldestPendingCreatedAt(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.forbidden != "" {
		f.tb.Helper()
		f.tb.Fatalf("背压在不该查 DB 的地方查了 DB：%s", f.forbidden)
	}
	return f.oldest, f.err
}

// guardWithClock 构造一个把时钟钉在 clk 上的 guard（边界用例专用）。
func guardWithClock(tb testing.TB, src pendingAgeSource, opts backpressureOptions, clk *testClock) *backpressureGuard {
	g := newBackpressureGuard(src, opts)
	g.now = clk.now
	return g
}

func sampleOrFail(t *testing.T, g *backpressureGuard) {
	t.Helper()
	if err := g.sample(context.Background()); err != nil {
		t.Fatalf("sample 失败：%v", err)
	}
}

// okHandler 是"新事件入口"的最小替身：数调用次数并写 200。
func okHandler(calls *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = *calls + 1
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
}

// serveNewEvent 发一条"新事件"请求（POST /v1/events）。
func serveNewEvent(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(`{"sourceId":"s","idemKey":"k"}`))
	h.ServeHTTP(rec, req)
	return rec
}

// waitUntil 轮询等条件成立（只用于唯一那条真实 goroutine 的用例）。
func waitUntil(t *testing.T, what string, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待 %s 超时（%v）", what, timeout)
}

// ---------- 行为 1：未启用（nil / MaxAge<=0）→ 完全不介入 ----------

func TestBackpressureDisabledPassesThrough(t *testing.T) {
	cases := []struct {
		name  string
		guard func(src *fakeAgeSource) *backpressureGuard
	}{
		{"nil guard", func(*fakeAgeSource) *backpressureGuard { return nil }},
		{"MaxAge=0（运维显式关闭）", func(src *fakeAgeSource) *backpressureGuard {
			return newBackpressureGuard(src, backpressureOptions{MaxAge: 0})
		}},
		{"MaxAge<0", func(src *fakeAgeSource) *backpressureGuard {
			return newBackpressureGuard(src, backpressureOptions{MaxAge: -time.Second})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newFakeAgeSource(t)
			src.forbid("背压未启用时不该碰数据层")
			g := tc.guard(src)

			calls := 0
			rec := serveNewEvent(g.Wrap(okHandler(&calls)))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200（未启用时必须原样放行）", rec.Code)
			}
			if got := rec.Body.String(); got != `{"ok":true}` {
				t.Fatalf("body = %q, want next 的原样输出", got)
			}
			if calls != 1 {
				t.Fatalf("next 被调用 %d 次, want 1", calls)
			}
			if got := src.callCount(); got != 0 {
				t.Fatalf("未启用时仍查了 %d 次 DB", got)
			}
			if got := rec.Header().Get("Retry-After"); got != "" {
				t.Fatalf("未启用时写了 Retry-After=%q", got)
			}
		})
	}
}

// ---------- 行为 2 + B5：未超阈 → 每请求 0 次 DB 往返 ----------

func TestBackpressureUnderThresholdNoDBRoundTripOnRequestPath(t *testing.T) {
	clk := newTestClock()
	src := newFakeAgeSource(t)
	src.set(clk.oldestFor(30*time.Second), nil)
	g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge}, clk)

	sampleOrFail(t, g) // 一次显式采样：把状态推进到"已采样、未超阈"
	if got := src.callCount(); got != 1 {
		t.Fatalf("显式采样 %d 次, want 1", got)
	}
	if g.over.Load() {
		t.Fatal("30s < 120s 却判定超阈")
	}

	// 从这一刻起，请求路径上**任何**一次采样调用都是 bug。
	// 这不是"调用次数 == 0"的弱断言：真调了就会在这一行炸掉，并指出是哪个场景。
	src.forbid("未超阈时每请求必须是 0 次 DB 往返（契约 §1.2 行为 2 / B5）")

	// 请求数用 10000 而不是 10：这里要证的是「**每请求 0 次**」，样本太小的话
	// 一次意外采样在比例上看不出来（1/10 与 1/10000 的结论完全不同）。
	// 10000 条请求跑完只要几毫秒（Wrap 只做一次 atomic 读），换来的是硬结论。
	const n = 10000
	calls := 0
	h := g.Wrap(okHandler(&calls))
	for i := 0; i < n; i++ {
		if rec := serveNewEvent(h); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 条请求 status = %d, want 200", i, rec.Code)
		}
	}
	if calls != n {
		t.Fatalf("next 被调用 %d 次, want %d", calls, n)
	}
	if got := src.callCount(); got != 1 {
		t.Fatalf("%d 条请求后 DB 往返 = %d 次（只该有那 1 次显式采样，即每请求 0 次）", n, got)
	}
	if got := g.rejectedTotal.Load(); got != 0 {
		t.Fatalf("未超阈却计数了 %d 次拒绝", got)
	}
}

// ---------- 行为 3：超阈 → 503 + Retry-After + 固定字段 body，且不调用 next ----------

func TestBackpressureOverThresholdWrites503(t *testing.T) {
	clk := newTestClock()
	src := newFakeAgeSource(t)
	src.set(clk.oldestFor(213*time.Second), nil)
	g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge}, clk)
	sampleOrFail(t, g)

	calls := 0
	h := g.Wrap(okHandler(&calls))
	rec := serveNewEvent(h)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if calls != 0 {
		t.Fatalf("超阈时仍调用了 next %d 次（等于没拒）", calls)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store（这是即时结论，任何层都不许缓存）", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "15" {
		t.Fatalf("Retry-After = %q, want 15（默认 15s）", got)
	}

	// body 字段名与值都由契约冻结：用 map 比对，字段名改了/少了/多了都会红。
	var gotBody map[string]any
	raw := rec.Body.String()
	if err := json.Unmarshal([]byte(raw), &gotBody); err != nil {
		t.Fatalf("503 body 不是合法 JSON（%v）：%s", err, raw)
	}
	want := map[string]any{
		"error":               "backpressure",
		"pending_age_sec":     float64(213),
		"max_pending_age_sec": float64(120),
		"retry_after_sec":     float64(15),
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("503 body = %v, want %v（raw: %s）", gotBody, want, raw)
	}

	// 第二条也拒，计数按次累加（供 released 日志报"这轮挡了多少"）。
	if rec2 := serveNewEvent(h); rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("第二条请求 status = %d, want 503", rec2.Code)
	}
	if got := g.rejectedTotal.Load(); got != 2 {
		t.Fatalf("rejectedTotal = %d, want 2", got)
	}
}

// ---------- 行为 4：没有 pending（采样值 0）→ 放行 ----------

func TestBackpressureNoPendingAllowsThrough(t *testing.T) {
	clk := newTestClock()
	src := newFakeAgeSource(t)
	src.set(0, nil) // 一条 pending 都没有：数据层约定返回 0，不是"1970 年就在等"
	g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge}, clk)
	sampleOrFail(t, g)

	if g.over.Load() {
		t.Fatal("没有 pending 却判定超阈（0 被当成了很老的 created_at）")
	}
	if got := g.lastAgeSec.Load(); got != 0 {
		t.Fatalf("lastAgeSec = %d, want 0", got)
	}

	src.forbid("没有积压时请求路径不该查 DB")
	calls := 0
	if rec := serveNewEvent(g.Wrap(okHandler(&calls))); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("next 被调用 %d 次, want 1", calls)
	}
}

// ---------- 行为 5：还没采过样（进程刚起）→ 放行 ----------

func TestBackpressureBeforeFirstSampleAllowsThrough(t *testing.T) {
	src := newFakeAgeSource(t)
	src.forbid("还没采过样时 Wrap 也不该查 DB（判定只读 atomic）")
	g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge}, newTestClock())

	// 从未调用过 sample：over 的零值 false 就是"还不知道" → 放行。
	calls := 0
	if rec := serveNewEvent(g.Wrap(okHandler(&calls))); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（不因为「还不知道」就拒绝新事件）", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("next 被调用 %d 次, want 1", calls)
	}
	if got := src.callCount(); got != 0 {
		t.Fatalf("采样 %d 次, want 0", got)
	}
}

// ---------- 行为 6：采样出错 → 保持上一次状态 + sample_failed 日志 ----------

func TestBackpressureSampleErrorKeepsLastState(t *testing.T) {
	boom := errors.New("db: disk I/O error")

	t.Run("未超阈时出错 → 仍放行", func(t *testing.T) {
		logs := captureBackpressureLog(t)
		clk := newTestClock()
		src := newFakeAgeSource(t)
		src.set(clk.oldestFor(10*time.Second), nil)
		g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge}, clk)
		sampleOrFail(t, g)

		src.set(0, boom)
		if err := g.sample(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("sample err = %v, want %v", err, boom)
		}
		if g.over.Load() {
			t.Fatal("一次 DB 抖动就把新事件挡了")
		}
		calls := 0
		if rec := serveNewEvent(g.Wrap(okHandler(&calls))); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		line := eventLine(t, logs.String(), "server.backpressure.sample_failed")
		mustContain(t, line, "engaged=false")
		if n := countEvent(logs.String(), "server.backpressure.engaged"); n != 0 {
			t.Fatalf("采样失败却打了 %d 条 engaged", n)
		}
	})

	t.Run("超阈时出错 → 仍拒绝", func(t *testing.T) {
		logs := captureBackpressureLog(t)
		clk := newTestClock()
		src := newFakeAgeSource(t)
		src.set(clk.oldestFor(300*time.Second), nil)
		g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge}, clk)
		sampleOrFail(t, g)
		if !g.over.Load() {
			t.Fatal("300s > 120s 却没判定超阈")
		}

		src.set(0, boom)
		if err := g.sample(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("sample err = %v, want %v", err, boom)
		}
		if !g.over.Load() {
			t.Fatal("一次 DB 抖动就解除了保护（积压并没有被证明好转）")
		}
		calls := 0
		if rec := serveNewEvent(g.Wrap(okHandler(&calls))); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if calls != 0 {
			t.Fatalf("超阈时调用了 next %d 次", calls)
		}
		line := eventLine(t, logs.String(), "server.backpressure.sample_failed")
		mustContain(t, line, "engaged=true")
		if n := countEvent(logs.String(), "server.backpressure.released"); n != 0 {
			t.Fatalf("采样失败却打了 %d 条 released", n)
		}
	})
}

// ---------- 行为 7：状态跃迁才打日志（engaged / still_engaged 60s 节流 / released） ----------

func TestBackpressureLogTransitions(t *testing.T) {
	logs := captureBackpressureLog(t)
	clk := newTestClock()
	src := newFakeAgeSource(t)
	g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge}, clk)

	calls := 0
	h := g.Wrap(okHandler(&calls))

	// 1) 未超阈：一条背压日志都不该有（每采样一条就是刷屏）。
	src.set(clk.oldestFor(10*time.Second), nil)
	sampleOrFail(t, g)
	if out := logs.String(); strings.Contains(out, "server.backpressure.") {
		t.Fatalf("未超阈却打了背压日志：\n%s", out)
	}

	// 2) 进入超阈：一条 engaged，带 pending_age_sec / max_pending_age_sec。
	src.set(clk.oldestFor(200*time.Second), nil)
	sampleOrFail(t, g)
	if n := countEvent(logs.String(), "server.backpressure.engaged"); n != 1 {
		t.Fatalf("engaged 日志 %d 条, want 1", n)
	}
	line := eventLine(t, logs.String(), "server.backpressure.engaged")
	mustContain(t, line, "pending_age_sec=200")
	mustContain(t, line, "max_pending_age_sec=120")

	// 期间真拒两次（released 日志要报出这轮挡了多少）。
	for i := 0; i < 2; i++ {
		if rec := serveNewEvent(h); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
	}

	// 3) 持续超阈但没到 60s：不重复 engaged，也还没有 still_engaged。
	clk.advance(30 * time.Second)
	src.set(clk.oldestFor(230*time.Second), nil)
	sampleOrFail(t, g)
	if n := countEvent(logs.String(), "server.backpressure.engaged"); n != 1 {
		t.Fatalf("保持超阈时 engaged 日志涨到 %d 条（应当只在跃迁时打一条）", n)
	}
	if n := countEvent(logs.String(), "server.backpressure.still_engaged"); n != 0 {
		t.Fatalf("距进入才 30s 就打了 %d 条 still_engaged（节流窗是 60s）", n)
	}

	// 4) 距进入满 60s：补一条 still_engaged，带 engaged_secs / rejected_total。
	clk.advance(31 * time.Second) // 累计 61s
	src.set(clk.oldestFor(261*time.Second), nil)
	sampleOrFail(t, g)
	if n := countEvent(logs.String(), "server.backpressure.still_engaged"); n != 1 {
		t.Fatalf("still_engaged 日志 %d 条, want 1", n)
	}
	line = eventLine(t, logs.String(), "server.backpressure.still_engaged")
	mustContain(t, line, "pending_age_sec=261")
	mustContain(t, line, "engaged_secs=61")
	mustContain(t, line, "rejected_total=2")

	// 5) 再过 30s（距上一条 still 只有 30s）：不刷屏。
	clk.advance(30 * time.Second) // 累计 91s
	src.set(clk.oldestFor(291*time.Second), nil)
	sampleOrFail(t, g)
	if n := countEvent(logs.String(), "server.backpressure.still_engaged"); n != 1 {
		t.Fatalf("still_engaged 涨到 %d 条（60s 节流没生效）", n)
	}

	// 6) 解除：一条 released，带 pending_age_sec / rejected_total / engaged_secs。
	src.set(0, nil)
	sampleOrFail(t, g)
	if g.over.Load() {
		t.Fatal("积压清零后仍判定超阈")
	}
	if n := countEvent(logs.String(), "server.backpressure.released"); n != 1 {
		t.Fatalf("released 日志 %d 条, want 1", n)
	}
	line = eventLine(t, logs.String(), "server.backpressure.released")
	mustContain(t, line, "pending_age_sec=0")
	mustContain(t, line, "rejected_total=2")
	mustContain(t, line, "engaged_secs=91")
	if got := g.engagedSecs.Load(); got != 91 {
		t.Fatalf("engagedSecs = %d, want 91", got)
	}

	// 解除后新事件立刻恢复放行。
	if rec := serveNewEvent(h); rec.Code != http.StatusOK {
		t.Fatalf("解除后 status = %d, want 200", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("解除后 next 被调用 %d 次, want 1", calls)
	}
}

// ---------- 阈值边界：严格大于（age > MaxAge） ----------

func TestBackpressureThresholdIsStrictlyGreater(t *testing.T) {
	cases := []struct {
		name     string
		skew     time.Duration // 采样时刻相对整秒基准的偏移
		age      time.Duration
		wantOver bool
	}{
		{name: "恰好等于阈值（120s）→ 放行", age: bpMaxAge, wantOver: false},
		{name: "超过阈值 0.5s（120.5s）→ 拒绝", skew: 500 * time.Millisecond, age: bpMaxAge, wantOver: true},
		{name: "超过阈值 1s（121s）→ 拒绝", age: bpMaxAge + time.Second, wantOver: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := newTestClock()
			clk.advance(tc.skew)
			src := newFakeAgeSource(t)
			// oldest 是整秒，所以"真实年龄"= 整秒差 + 时钟的小数偏移。
			src.set(clk.now().Add(-tc.age).Unix(), nil)
			g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge}, clk)
			sampleOrFail(t, g)

			if got := g.over.Load(); got != tc.wantOver {
				t.Fatalf("over = %v, want %v（阈值判定必须是严格大于）", got, tc.wantOver)
			}
			calls := 0
			rec := serveNewEvent(g.Wrap(okHandler(&calls)))
			wantCode := http.StatusOK
			if tc.wantOver {
				wantCode = http.StatusServiceUnavailable
			}
			if rec.Code != wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, wantCode)
			}
		})
	}
}

// ---------- 行为 8：自适应降采样 ----------

func TestBackpressureAdaptiveSampling(t *testing.T) {
	const idle, fast = 5 * time.Second, time.Second
	cases := []struct {
		name string
		age  time.Duration
		want time.Duration
	}{
		{"没有 pending → IdleEvery", 0, idle},
		{"远低于阈值（30s：2×30 < 120）→ IdleEvery", 30 * time.Second, idle},
		{"不到一半（59s：2×59 = 118 < 120）→ IdleEvery", 59 * time.Second, idle},
		{"恰好一半（60s：2×60 = 120 不小于 120）→ FastEvery", 60 * time.Second, fast},
		{"超过阈值（200s）→ FastEvery", 200 * time.Second, fast},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := newTestClock()
			src := newFakeAgeSource(t)
			g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge, FastEvery: fast, IdleEvery: idle}, clk)
			if got := g.nextInterval(); got != fast {
				t.Fatalf("初始间隔 = %v, want FastEvery(%v)", got, fast)
			}
			if tc.age == 0 {
				src.set(0, nil)
			} else {
				src.set(clk.oldestFor(tc.age), nil)
			}
			sampleOrFail(t, g)
			if got := g.nextInterval(); got != tc.want {
				t.Fatalf("下一轮采样间隔 = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("采样出错不改采样间隔", func(t *testing.T) {
		clk := newTestClock()
		src := newFakeAgeSource(t)
		g := guardWithClock(t, src, backpressureOptions{MaxAge: bpMaxAge, FastEvery: fast, IdleEvery: idle}, clk)
		src.set(clk.oldestFor(10*time.Second), nil)
		sampleOrFail(t, g)
		if got := g.nextInterval(); got != idle {
			t.Fatalf("间隔 = %v, want IdleEvery(%v)", got, idle)
		}
		src.set(0, errors.New("boom"))
		if err := g.sample(context.Background()); err == nil {
			t.Fatal("sample 应当返回错误")
		}
		if got := g.nextInterval(); got != idle {
			t.Fatalf("采样出错后间隔 = %v, want 保持 %v", got, idle)
		}
	})
}

// ---------- Run：立刻采一次（startDelay=0）、可被 ctx 打断、不泄漏 ----------

func TestBackpressureRunSamplesImmediatelyAndStopsOnCancel(t *testing.T) {
	src := newFakeAgeSource(t)
	// 一起步就已经积压 10 分钟（远超 60s 阈值）。
	src.set(time.Now().Add(-10*time.Minute).Unix(), nil)

	// 采样间隔给到 1h：于是「启动后几十毫秒内 fake 就被调用过」只能是
	// "进入循环立刻同步采一次"（首个 timer 延迟 0）带来的，不是 ticker 碰巧到点。
	g := newBackpressureGuard(src, backpressureOptions{
		MaxAge:    time.Minute,
		FastEvery: time.Hour,
		IdleEvery: 2 * time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(ctx)
	}()

	// 启动后（几百毫秒预算内）必须已经采过样且 over 已按采样结果更新。
	waitUntil(t, "启动即采样", func() bool { return src.callCount() >= 1 && g.over.Load() }, 500*time.Millisecond)
	if got := g.lastAgeSec.Load(); got < 590 || got > 700 {
		t.Fatalf("lastAgeSec = %d, want ≈600（采样结果没写进状态）", got)
	}
	if n := src.callCount(); n != 1 {
		t.Fatalf("启动瞬间采样 %d 次, want 1（等 1h 的 ticker 不该触发）", n)
	}

	// 取消必须**立刻**生效：等待走的是 select{ctx.Done, timer.C}，
	// 裸 time.Sleep(1h) 会让这里超时（也就会被 goroutine 泄漏检测抓到）。
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("ctx 取消后 Run 没有退出（等待不可打断 → goroutine 泄漏）")
	}

	// 退出之后不许再采样。
	after := src.callCount()
	time.Sleep(20 * time.Millisecond)
	if got := src.callCount(); got != after {
		t.Fatalf("Run 退出后仍在采样（%d → %d）", after, got)
	}
}

// Run 在未启用时立刻返回：不该起采样循环，更不该碰 DB。
func TestBackpressureRunDisabledReturnsImmediately(t *testing.T) {
	src := newFakeAgeSource(t)
	src.forbid("MaxAge<=0 时 Run 不该启动采样")
	g := newBackpressureGuard(src, backpressureOptions{MaxAge: 0})

	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("未启用时 Run 挂住了（应当立刻返回）")
	}
	if got := src.callCount(); got != 0 {
		t.Fatalf("未启用时采样了 %d 次", got)
	}
}

// ---------- B5 的机械证据：未超阈时包一层 vs 裸 handler ----------

// BenchmarkBackpressureWrapUnderThreshold 对比裸 handler 与包一层背压的 ns/op。
//
// fake 在 bench 里同样是"被调用就失败"：请求路径上根本不该有 DB 往返，
// 所以哪怕 b.N 次里混进一次采样，bench 也会立刻炸掉而不是安静地变慢。
func BenchmarkBackpressureWrapUnderThreshold(b *testing.B) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	src := newFakeAgeSource(b)
	src.set(time.Now().Unix(), nil) // 刚入队 → age ≈ 0，未超阈
	g := newBackpressureGuard(src, backpressureOptions{MaxAge: bpMaxAge})
	if err := g.sample(context.Background()); err != nil {
		b.Fatalf("准备状态失败：%v", err)
	}
	src.forbid("bench 里请求路径上出现了 DB 往返（B5 要求 0 次）")

	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(`{"sourceId":"s","idemKey":"k"}`))

	run := func(b *testing.B, h http.Handler) {
		b.Helper()
		rec := httptest.NewRecorder()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rec.Body.Reset() // 复用 recorder，避免 body 无界增长
			h.ServeHTTP(rec, req)
		}
		b.StopTimer()
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d, want 200", rec.Code)
		}
	}

	b.Run("bare", func(b *testing.B) { run(b, next) })
	b.Run("guarded", func(b *testing.B) { run(b, g.Wrap(next)) })
}

// ---------- 行为 10：安全阀 —— 超阈过久必须 fail-open（#228 复审 MAJOR） ----------
//
// 为什么必须有这一条：判定信号是 `MIN(created_at) WHERE status='pending'`，
// 而库里一旦存在**永远派不出去**的 pending（能力升级后无人声明该能力 / 节点下线后
// 无人声明 / 积压来源消失但任务未收敛），这个值只增不减，`RedispatchTask` 对这些
// 只会一直返回 false，全仓没有 abandon 路径。若没有出口，一条孤儿任务就能把
// **整个部署**的新事件入口钉死在 503 上，永不恢复。
//
// 断言语义：
//  1. 超阈但未到安全阀窗口 → 仍拒绝（保护照常）；
//  2. 跨过窗口 → **停止拒绝**（放行），并打一条 server.backpressure.stuck；
//  3. 信号回落后解除，且**重新具备拒绝能力**（不是一次性烧毁的开关）。
func TestBackpressureStuckValveFailsOpen(t *testing.T) {
	logs := captureBackpressureLog(t)
	clk := newTestClock()
	fs := newFakeAgeSource(t)
	g := guardWithClock(t, fs, backpressureOptions{
		MaxAge:     bpMaxAge,
		StuckAfter: 10 * time.Minute,
	}, clk)
	var calls int
	h := g.Wrap(okHandler(&calls))

	// ① 刚开始超阈：照常拒绝。
	fs.set(clk.oldestFor(bpMaxAge+time.Second), nil)
	sampleOrFail(t, g)
	if rec := serveNewEvent(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("超阈初期应 503，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("拒绝时不该调用 next，实际 %d 次", calls)
	}

	// ② 恰好差一秒到窗口：仍然拒绝（边界是「>= StuckAfter」才放行）。
	clk.advance(10*time.Minute - time.Second)
	fs.set(clk.oldestFor(bpMaxAge+time.Minute), nil) // 年龄继续增长（孤儿任务的样子）
	sampleOrFail(t, g)
	if rec := serveNewEvent(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未到安全阀窗口应仍 503，实际 %d", rec.Code)
	}

	// ③ 跨过窗口：fail-open，必须放行并留痕。
	clk.advance(2 * time.Second)
	fs.set(clk.oldestFor(bpMaxAge+2*time.Minute), nil)
	sampleOrFail(t, g)
	if rec := serveNewEvent(h); rec.Code != http.StatusOK {
		t.Fatalf("越过安全阀后必须放行（fail-open），实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("fail-open 后应调用 next 一次，实际 %d 次", calls)
	}
	if n := countEvent(logs.String(), "server.backpressure.stuck"); n != 1 {
		t.Fatalf("应打恰好 1 条 server.backpressure.stuck，实际 %d：\n%s", n, logs.String())
	}
	line := eventLine(t, logs.String(), "server.backpressure.stuck")
	mustContain(t, line, "level=ERROR")
	mustContain(t, line, "stuck_after=10m0s")

	// ④ 解保：信号回落后 released，且 was_stuck=true 留痕。
	fs.set(clk.oldestFor(0), nil)
	sampleOrFail(t, g)
	if rec := serveNewEvent(h); rec.Code != http.StatusOK {
		t.Fatalf("解保后应放行，实际 %d", rec.Code)
	}
	mustContain(t, eventLine(t, logs.String(), "server.backpressure.released"), "was_stuck=true")

	// ⑤ 重新具备拒绝能力（安全阀不是一次性烧毁）：再积压一次照样 503。
	clk.advance(time.Minute)
	fs.set(clk.oldestFor(bpMaxAge+time.Second), nil)
	sampleOrFail(t, g)
	if rec := serveNewEvent(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("解除后再次超阈应重新 503，实际 %d", rec.Code)
	}
	if got := countEvent(logs.String(), "server.backpressure.engaged"); got != 2 {
		t.Fatalf("两轮超阈应各打一条 engaged（共 2），实际 %d", got)
	}
}

// 行为 11：采样失败日志必须节流（超阈时采样档是 1s，DB 一直坏就是 1 条 WARN/秒）。
func TestBackpressureSampleFailedLogThrottled(t *testing.T) {
	logs := captureBackpressureLog(t)
	clk := newTestClock()
	fs := newFakeAgeSource(t)
	g := guardWithClock(t, fs, backpressureOptions{MaxAge: bpMaxAge}, clk)

	boom := errors.New("db is gone")
	fs.set(0, boom)
	sampleOrFail2(t, g) // 第一次失败：必须打
	if n := countEvent(logs.String(), "server.backpressure.sample_failed"); n != 1 {
		t.Fatalf("第一次采样失败应打 1 条，实际 %d", n)
	}

	// 1s 一次连打 5 次：全部被节流（60s 窗口内）。
	for i := 0; i < 5; i++ {
		clk.advance(time.Second)
		sampleOrFail2(t, g)
	}
	if n := countEvent(logs.String(), "server.backpressure.sample_failed"); n != 1 {
		t.Fatalf("60s 窗口内应只有 1 条（节流失效会刷屏），实际 %d", n)
	}

	// 跨过窗口：再打一条。
	clk.advance(61 * time.Second)
	sampleOrFail2(t, g)
	if n := countEvent(logs.String(), "server.backpressure.sample_failed"); n != 2 {
		t.Fatalf("跨过 60s 窗口应再打 1 条（共 2），实际 %d", n)
	}
}

// sampleOrFail2 是「预期失败」的采样版本（上面的 sampleOrFail 反过来要求成功）。
func sampleOrFail2(t *testing.T, g *backpressureGuard) {
	t.Helper()
	if err := g.sample(context.Background()); err == nil {
		t.Fatal("预期 sample 返回错误，实际 nil")
	}
}
