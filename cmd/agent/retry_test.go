package main

// retry_test.go 钉住 #323 的三件事，判据全部是**结构化**的（不靠文案）：
//
//  1. 退避序列：单调递增、封顶、成功握手后重置；
//  2. 分类表：530/超时 → 瞬态重试；401/403、协议不兼容、node.yaml 坏 → 永久退出非 0；
//  3. 日志降噪：前 N 次逐条，之后每 M 次一条；恢复时一条带累计次数的 INFO。
//
// 另外两条是「真跑一遍」的锚：
//   - 真 WebSocket 拨号的**状态码**真的能拿到（不然分类表就只是一厢情愿的假设）；
//   - serveNode 在 welcome 落地那一刻真的回调 sessionHooks.onHandshake。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/ws"
)

// ---------- 1. 退避序列 ----------

// TestBackoffDelaySequence：1s→2s→4s… 单调递增、封顶 30s、不越界。
func TestBackoffDelaySequence(t *testing.T) {
	t.Parallel()

	p := defaultBackoffPolicy()
	p.Jitter = 0 // 纯序列，不带抖动
	p.Rand = nil

	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second, // 32 触顶 → 30
		30 * time.Second,
		30 * time.Second,
	}
	prev := time.Duration(0)
	for i, wantDelay := range want {
		got := p.delay(i + 1) // 1-based：第 i+1 次连续失败
		if got != wantDelay {
			t.Errorf("delay(%d) = %v, want %v", i+1, got, wantDelay)
		}
		if got < prev {
			t.Errorf("delay(%d) = %v 比上一次 %v 小：退避必须单调不减", i+1, got, prev)
		}
		prev = got
	}
	// 封顶：再往后（包括很大数）都不许超过 Max。
	for _, attempt := range []int{50, 1000, 1 << 20} {
		if got := p.delay(attempt); got != backoffMax {
			t.Errorf("delay(%d) = %v, want 封顶 %v", attempt, got, backoffMax)
		}
	}
	// 下限：attempt < 1 按 1 算，不许出现 0 等待（0 会退化成 busy loop）。
	if got := p.delay(0); got != time.Second {
		t.Errorf("delay(0) = %v, want %v（按第一次算）", got, time.Second)
	}
}

// TestBackoffDelayJitterBounds：抖动只在 ±20% 内，且**不会突破上限**。
//
// 抖动存在的理由：多节点同时掉线时不要把重连对齐成脉冲。
func TestBackoffDelayJitterBounds(t *testing.T) {
	t.Parallel()

	p := defaultBackoffPolicy()
	p.Jitter = 0.2

	cases := []struct {
		name string
		r    float64
	}{
		{"最小", 0},
		{"中间", 0.5},
		{"最大", 0.999999},
	}
	for _, tc := range cases {
		p.Rand = func() float64 { return tc.r }
		for attempt := 1; attempt <= 8; attempt++ {
			bare := p
			bare.Jitter = 0
			want := bare.delay(attempt) // 不带抖动的基准值
			got := p.delay(attempt)
			lo := time.Duration(float64(want) * 0.8)
			hi := time.Duration(float64(want) * 1.2)
			if hi > backoffMax {
				hi = backoffMax
			}
			if got < lo || got > hi {
				t.Errorf("%s抖动: delay(%d) = %v，want ∈ [%v, %v]", tc.name, attempt, got, lo, hi)
			}
			if got > backoffMax {
				t.Errorf("%s抖动: delay(%d) = %v 突破了上限 %v", tc.name, attempt, got, backoffMax)
			}
			if got <= 0 {
				t.Errorf("%s抖动: delay(%d) = %v，退避不能是 0", tc.name, attempt, got)
			}
		}
	}
}

// ---------- 2. 分类表 ----------

// TestClassifyNodeFailure：判据表逐条对账（#323 的验收：530/超时 → 瞬态；
// 401/403、协议不兼容、node.yaml 坏 → 永久）。
func TestClassifyNodeFailure(t *testing.T) {
	t.Parallel()

	netErr := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")

	cases := []struct {
		name       string
		err        error
		wantClass  failureClass
		wantCode   string
		wantReason string // 只看关键词（reason 是给人看的整句）
	}{
		{
			name:      "530（Cloudflare 隧道打不通）→ 瞬态",
			err:       &dialError{status: 530, url: "wss://x/v1/node/stream", err: netErr},
			wantClass: classTransient,
			wantCode:  failCodeServerUnavailable,
		},
		{
			name:      "500 → 瞬态",
			err:       &dialError{status: http.StatusInternalServerError, err: netErr},
			wantClass: classTransient,
			wantCode:  failCodeServerUnavailable,
		},
		{
			name:      "取不到响应（拒连/DNS/TLS 超时）→ 瞬态",
			err:       &dialError{status: 0, err: netErr},
			wantClass: classTransient,
			wantCode:  failCodeDialFailed,
		},
		{
			name:      "429 限流 → 瞬态",
			err:       &dialError{status: http.StatusTooManyRequests, err: netErr},
			wantClass: classTransient,
			wantCode:  failCodeServerThrottled,
		},
		{
			name:      "408 → 瞬态",
			err:       &dialError{status: http.StatusRequestTimeout, err: netErr},
			wantClass: classTransient,
			wantCode:  failCodeServerThrottled,
		},
		{
			name:       "401 令牌无效 → 永久",
			err:        &dialError{status: http.StatusUnauthorized, err: netErr},
			wantClass:  classPermanent,
			wantCode:   failCodeTokenRejected,
			wantReason: "令牌",
		},
		{
			name:       "403 → 永久",
			err:        &dialError{status: http.StatusForbidden, err: netErr},
			wantClass:  classPermanent,
			wantCode:   failCodeTokenRejected,
			wantReason: "令牌",
		},
		{
			name:       "404（server 路径写错）→ 永久",
			err:        &dialError{status: http.StatusNotFound, err: netErr},
			wantClass:  classPermanent,
			wantCode:   failCodeEndpointRejected,
			wantReason: "server",
		},
		{
			name:       "协议版本不兼容 → 永久",
			err:        &handshakeRejectError{code: ws.ErrCodeProtocolMismatch, message: "unsupported protocol 1 (server speaks 2)"},
			wantClass:  classPermanent,
			wantCode:   failCodeProtocolMismatch,
			wantReason: "协议版本",
		},
		{
			name:       "实例数超顶 → 永久",
			err:        &handshakeRejectError{code: ws.ErrCodeTooManyInstances, message: "too many instances"},
			wantClass:  classPermanent,
			wantCode:   failCodeTooManyInstances,
			wantReason: "instances",
		},
		{
			name:      "握手被拒但码不认识（bad_frame）→ 瞬态（判定不出来就别打死）",
			err:       &handshakeRejectError{code: ws.ErrCodeBadFrame, message: "hello failed: database is locked"},
			wantClass: classTransient,
			wantCode:  failCodeHandshakeRejected,
		},
		{
			name:      "welcome 超时（context.DeadlineExceeded）→ 瞬态",
			err:       &dialError{status: 0, err: context.DeadlineExceeded},
			wantClass: classTransient,
			wantCode:  failCodeDialFailed,
		},
		{
			name:      "握手成功之后连接断开 → 瞬态",
			err:       &sessionDropError{cause: errors.New("connection closed: EOF")},
			wantClass: classTransient,
			wantCode:  failCodeConnectionLost,
		},
		{
			name:       "node.yaml 坏 → 永久",
			err:        &permanentError{code: failCodeConfigInvalid, reason: failReasonConfigInvalid, err: errors.New("/x/node.yaml: token is required")},
			wantClass:  classPermanent,
			wantCode:   failCodeConfigInvalid,
			wantReason: "node.yaml",
		},
		{
			name:       "data-dir 不可用 → 永久",
			err:        &permanentError{code: failCodeDataDirUnavailable, reason: failReasonDataDirUnavailable, err: errors.New("perm denied")},
			wantClass:  classPermanent,
			wantCode:   failCodeDataDirUnavailable,
			wantReason: "数据目录",
		},
		{
			name:      "判不出来的错误 → 瞬态（默认重试）",
			err:       errors.New("某种还没归类的意外"),
			wantClass: classTransient,
			wantCode:  failCodeUnclassified,
		},
	}

	for _, tc := range cases {
		got := classifyNodeFailure(tc.err)
		if got.class != tc.wantClass {
			verb := map[failureClass]string{classTransient: "瞬态", classPermanent: "永久"}[tc.wantClass]
			t.Errorf("%s: class = %v，want %s", tc.name, got.class, verb)
		}
		if got.code != tc.wantCode {
			t.Errorf("%s: code = %q，want %q", tc.name, got.code, tc.wantCode)
		}
		if tc.wantReason != "" && !strings.Contains(got.reason, tc.wantReason) {
			t.Errorf("%s: reason = %q，应当含 %q（给运维的人话）", tc.name, got.reason, tc.wantReason)
		}
		if got.reason == "" {
			t.Errorf("%s: reason 不能是空串（日志里要给人看得懂的原因）", tc.name)
		}
		if got.permanent() != (tc.wantClass == classPermanent) {
			t.Errorf("%s: permanent() 与 class 不一致", tc.name)
		}
	}
}

// TestClassifyHandshakeTimeout：握手超时走的是 context.DeadlineExceeded，
// 这条单独钉一次（它最容易在重构中被 `%w` 的丢失悄悄改成「判不出来」）。
func TestClassifyHandshakeTimeout(t *testing.T) {
	t.Parallel()

	err := &wrapErr{msg: "wait welcome: timeout after 30s", cause: context.DeadlineExceeded}
	got := classifyNodeFailure(err)
	if got.class != classTransient || got.code != failCodeHandshakeTimeout {
		t.Fatalf("握手超时分类 = (%v, %q)，want (瞬态, %q)", got.class, got.code, failCodeHandshakeTimeout)
	}
}

// wrapErr 是一个最小的 %w 包装器（模拟 handshake 里的 fmt.Errorf("%w")）。
type wrapErr struct {
	msg   string
	cause error
}

func (e *wrapErr) Error() string { return e.msg + ": " + e.cause.Error() }
func (e *wrapErr) Unwrap() error { return e.cause }

// TestDialErrorCarriesRealStatus：真拨号一次，确认**状态码是真的能从响应里拿到**。
//
// 为什么必须真跑：分类表的判据是 `resp.StatusCode`，一旦 coder/websocket 的
// Dial 不再带回 *http.Response（或我们漏接了第二个返回值），所有 5xx/401
// 都会退化成「没响应 → 瞬态」，于是令牌写错的节点永远重试 —— 这正是 #323 要防的。
func TestDialErrorCarriesRealStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status    int
		wantClass failureClass
		wantCode  string
	}{
		{http.StatusServiceUnavailable, classTransient, failCodeServerUnavailable},
		{530, classTransient, failCodeServerUnavailable}, // Cloudflare 的 5xx
		{http.StatusUnauthorized, classPermanent, failCodeTokenRejected},
		{http.StatusForbidden, classPermanent, failCodeTokenRejected},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		url := "ws" + strings.TrimPrefix(srv.URL, "http")
		_, err := dialNode(context.Background(), url, "test-token")
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d 时拨号应当失败", tc.status)
		}
		var de *dialError
		if !errors.As(err, &de) {
			t.Fatalf("HTTP %d 的错误应当是 *dialError，实际 %T: %v", tc.status, err, err)
		}
		if de.status != tc.status {
			t.Errorf("dialError.status = %d，want %d（状态码必须原样带回来）", de.status, tc.status)
		}
		// 文案保持原样：安装脚本的预检与运维的 grep 都依赖它。
		if !strings.Contains(err.Error(), "dial "+url) {
			t.Errorf("错误文案丢了 `dial <url>` 前缀：%q", err.Error())
		}
		got := classifyNodeFailure(err)
		if got.class != tc.wantClass || got.code != tc.wantCode {
			t.Errorf("HTTP %d 分类 = (%v, %q)，want (%v, %q)",
				tc.status, got.class, got.code, tc.wantClass, tc.wantCode)
		}
	}
}

// TestDialErrorNoResponse：连一个必然拒连的端口 → status 0（没拿到响应）。
func TestDialErrorNoResponse(t *testing.T) {
	t.Parallel()

	// 127.0.0.1:1 上不会有服务在听（特权端口、几乎不可能被占）。
	_, err := dialNode(context.Background(), "ws://127.0.0.1:1/v1/node/stream", "t")
	if err == nil {
		t.Fatal("拨号到 127.0.0.1:1 应当失败")
	}
	var de *dialError
	if !errors.As(err, &de) {
		t.Fatalf("错误应当是 *dialError，实际 %T: %v", err, err)
	}
	if de.status != 0 {
		t.Errorf("没有响应时 status = %d，want 0", de.status)
	}
	if got := classifyNodeFailure(err); got.class != classTransient {
		t.Errorf("拒连应当按瞬态处理，实际 %v", got.class)
	}
}

// TestHandshakeRejectKeepsMessage：分类不能改变握手拒绝的文案（契约 §3.4.1 的
// message 是运维唯一能看懂的线索，已有测试按这个前缀断言）。
func TestHandshakeRejectKeepsMessage(t *testing.T) {
	t.Parallel()

	e := &handshakeRejectError{code: ws.ErrCodeTooManyInstances, message: "too many instances"}
	want := "handshake rejected: " + ws.ErrCodeTooManyInstances + ": too many instances"
	if got := e.Error(); got != want {
		t.Fatalf("握手拒绝文案 = %q，want %q", got, want)
	}
}

// ---------- 3. 日志降噪 ----------

// TestRetryStateLogNoise：前 retryLogFirst 次逐条、之后每 retryLogEvery 次一条，
// 恢复时一条带累计次数的 INFO。
func TestRetryStateLogNoise(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := defaultBackoffPolicy()
	p.Jitter = 0
	p.Wait = func(context.Context, time.Duration) bool { return true }
	state := newRetryState(log, p)

	// 连续失败 12 次：应当只有 1、2、3、10 这四条 WARN。
	for i := 0; i < 12; i++ {
		state.onFailure(nodeFailure{class: classTransient, code: failCodeDialFailed, err: errors.New("boom")})
	}
	warn := countLogLines(t, buf.String(), "level=WARN", "event=agent.retry")
	if warn != retryLogFirst+1 {
		t.Errorf("12 次连续失败的 WARN 条数 = %d，want %d（前 %d 次逐条 + 每 %d 次一条）",
			warn, retryLogFirst+1, retryLogFirst, retryLogEvery)
	}
	if !strings.Contains(buf.String(), "suppressed=") {
		t.Errorf("降噪后的那条应当带 suppressed= 计数：\n%s", buf.String())
	}
	// 中间那些必须真的降到了 DEBUG（默认 INFO 级看不到），而不是只在计数上省。
	debug := countLogLines(t, buf.String(), "level=DEBUG", "event=agent.retry")
	if debug == 0 {
		t.Error("被抑制的尝试应当降级到 DEBUG（否则 journal 照样被刷）")
	}

	// 恢复：一条 INFO，带累计失败次数与断了多久。
	before := buf.Len()
	state.onHandshake()
	if !strings.Contains(buf.String()[before:], "event=agent.retry.recovered") {
		t.Fatalf("恢复时应当打一条 event=agent.retry.recovered：\n%s", buf.String()[before:])
	}
	line := buf.String()[before:]
	for _, needle := range []string{"failed_attempts=12", "failures_total=12", "down_for="} {
		if !strings.Contains(line, needle) {
			t.Errorf("恢复日志缺 %q：%s", needle, line)
		}
	}

	// 重置：下一次失败从 attempt=1、1s 重新开始（而不是接着 30s）。
	before = buf.Len()
	delay := state.onFailure(nodeFailure{class: classTransient, code: failCodeDialFailed, err: errors.New("boom")})
	if delay != time.Second {
		t.Errorf("握手成功后下一次失败的退避 = %v，want %v（必须重置）", delay, time.Second)
	}
	if !strings.Contains(buf.String()[before:], "attempt=1") {
		t.Errorf("重置后应当重新从 attempt=1 打起：\n%s", buf.String()[before:])
	}

	// 首次就连上（没断过）不该打恢复日志：正常启动不该多一条噪声。
	quiet := newRetryState(log, p)
	before = buf.Len()
	quiet.onHandshake()
	if buf.Len() != before {
		t.Errorf("没断过时 onHandshake 不该打日志，实际多了：\n%s", buf.String()[before:])
	}
}

// countLogLines 数同时含所有 needle 的行数（slog TextHandler 一行一条）。
func countLogLines(t *testing.T, out string, needles ...string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(out, "\n") {
		all := true
		for _, needle := range needles {
			if !strings.Contains(line, needle) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// ---------- 4. 重连循环 ----------

// recordPolicy 是「不真睡、只记账」的退避策略。
func recordPolicy(delays *[]time.Duration) backoffPolicy {
	p := defaultBackoffPolicy()
	p.Jitter = 0
	p.Rand = func() float64 { return 0.5 }
	p.Wait = func(_ context.Context, d time.Duration) bool {
		*delays = append(*delays, d)
		return true
	}
	return p
}

// TestReconnectLoopRetriesTransientUntilSuccess：瞬态失败重试到成功，返回 nil。
func TestReconnectLoopRetriesTransientUntilSuccess(t *testing.T) {
	t.Parallel()

	var delays []time.Duration
	p := recordPolicy(&delays)
	log := testLogger()

	attempts := 0
	err := reconnectLoop(context.Background(), log, p, func(context.Context, sessionHooks) error {
		attempts++
		if attempts <= 3 {
			return &dialError{status: 530, err: errors.New("bad gateway")}
		}
		return nil // 第 4 次「正常退出」（信号收尾）
	})
	if err != nil {
		t.Fatalf("瞬态错误不该让循环返回错误，实际 %v", err)
	}
	if attempts != 4 {
		t.Fatalf("尝试次数 = %d，want 4（3 次失败 + 1 次成功）", attempts)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("退避次数 = %d，want %d：%v", len(delays), len(want), delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("第 %d 次退避 = %v，want %v", i+1, delays[i], want[i])
		}
	}
}

// TestReconnectLoopResetsAfterHandshake：握手成功过 → 退避重置回 1s。
func TestReconnectLoopResetsAfterHandshake(t *testing.T) {
	t.Parallel()

	var delays []time.Duration
	p := recordPolicy(&delays)
	log := testLogger()

	attempts := 0
	err := reconnectLoop(context.Background(), log, p, func(_ context.Context, hooks sessionHooks) error {
		attempts++
		switch attempts {
		case 1, 2:
			return &dialError{status: 503, err: errors.New("unavailable")}
		case 3:
			// 握手成功之后连接断了（中枢重启）→ 退避必须重置。
			hooks.onHandshake()
			return &sessionDropError{cause: errors.New("connection closed: EOF")}
		case 4:
			return &dialError{status: 503, err: errors.New("unavailable")}
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatalf("循环应当以 nil 结束，实际 %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, time.Second, 2 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("退避序列 = %v，want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("第 %d 次退避 = %v，want %v（握手成功必须重置阶梯）", i+1, delays[i], want[i])
		}
	}
}

// TestReconnectLoopPermanentStopsImmediately：永久错误第一次就返回，不重试。
func TestReconnectLoopPermanentStopsImmediately(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		code string
	}{
		{"令牌 401", &dialError{status: http.StatusUnauthorized, err: errors.New("unauthorized")}, failCodeTokenRejected},
		{"令牌 403", &dialError{status: http.StatusForbidden, err: errors.New("forbidden")}, failCodeTokenRejected},
		{"协议不兼容", &handshakeRejectError{code: ws.ErrCodeProtocolMismatch, message: "nope"}, failCodeProtocolMismatch},
		{"node.yaml 坏", &permanentError{code: failCodeConfigInvalid, err: errors.New("token is required")}, failCodeConfigInvalid},
	}
	for _, tc := range cases {
		var delays []time.Duration
		p := recordPolicy(&delays)
		attempts := 0
		err := reconnectLoop(context.Background(), testLogger(), p, func(context.Context, sessionHooks) error {
			attempts++
			return tc.err
		})
		if err == nil {
			t.Fatalf("%s：永久错误必须返回错误（→ 退出码非 0），却返回 nil", tc.name)
		}
		if attempts != 1 {
			t.Errorf("%s：尝试次数 = %d，want 1（永久错误不能变成无限重试）", tc.name, attempts)
		}
		if len(delays) != 0 {
			t.Errorf("%s：不该有任何退避等待，实际 %v", tc.name, delays)
		}
		if got := classifyNodeFailure(err); got.code != tc.code {
			t.Errorf("%s：返回的错误分类 code = %q，want %q", tc.name, got.code, tc.code)
		}
	}
}

// TestReconnectLoopNoRetry：-no-retry（预检）下瞬态错误立刻返回，不重试。
func TestReconnectLoopNoRetry(t *testing.T) {
	t.Parallel()

	var delays []time.Duration
	p := recordPolicy(&delays)
	p.Disabled = true

	attempts := 0
	boom := &dialError{status: 0, err: errors.New("connection refused")}
	err := reconnectLoop(context.Background(), testLogger(), p, func(context.Context, sessionHooks) error {
		attempts++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("-no-retry 下应当把原错误交回（errors.Is 能认），实际 %v", err)
	}
	if attempts != 1 {
		t.Errorf("尝试次数 = %d，want 1（-no-retry = 只试一次）", attempts)
	}
	if len(delays) != 0 {
		t.Errorf("-no-retry 下不该退避，实际 %v", delays)
	}
}

// TestReconnectLoopSignalDuringBackoff：退避期间收到 SIGTERM → 正常退出（nil），
// 且不再尝试一次（#323：退出语义不变）。
func TestReconnectLoopSignalDuringBackoff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	p := defaultBackoffPolicy()
	p.Wait = func(context.Context, time.Duration) bool {
		cancel() // 模拟退避期间收到 SIGTERM
		return false
	}
	err := reconnectLoop(ctx, testLogger(), p, func(context.Context, sessionHooks) error {
		attempts++
		return &dialError{status: 0, err: errors.New("connection refused")}
	})
	if err != nil {
		t.Fatalf("退避期间收到信号应当按正常退出（nil），实际 %v", err)
	}
	if attempts != 1 {
		t.Errorf("尝试次数 = %d，want 1（信号到达后不再重试）", attempts)
	}
}

// TestReconnectLoopSignalDuringAttempt：拨号中收到信号 → 正常退出（nil）。
func TestReconnectLoopSignalDuringAttempt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	err := reconnectLoop(ctx, testLogger(), defaultBackoffPolicy(), func(context.Context, sessionHooks) error {
		cancel()
		return &dialError{status: 0, err: context.Canceled}
	})
	if err != nil {
		t.Fatalf("信号打断的尝试应当按正常退出（nil），实际 %v", err)
	}
}

// ---------- 5. 真链路的两个锚 ----------

// TestServeNodeCallsHandshakeHook：welcome 落地那一刻必须回调 onHandshake 一次。
//
// 这是退避重置的**唯一**信号源：hook 不响的话「成功握手后重置」就只是纸上规则。
func TestServeNodeCallsHandshakeHook(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc := newW4Conn()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))

	handshakes := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveNode(ctx, w4Config(), fc, t.TempDir(), logger, 0,
			sessionHooks{onHandshake: func() { handshakes <- struct{}{} }})
	}()

	// hello 必须是第一帧；之后发 welcome 完成握手。
	fc.waitFor(t, 3*time.Second, "hello 帧", func(frames []map[string]any) bool { return len(frames) > 0 })
	fc.feed(t, &welcomeFrame{
		Type:         frameTypeWelcome,
		NodeID:       "nod_test",
		Protocol:     ws.ProtocolVersion,
		HeartbeatSec: 1,
		LeaseTTLSec:  30,
	})

	select {
	case <-handshakes:
	case <-time.After(3 * time.Second):
		t.Fatal("握手成功后没有回调 sessionHooks.onHandshake（退避无法重置）")
	}

	// 收尾：取消 ctx（SIGTERM）→ serveNode 返回 nil。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("正常退出必须返回 nil，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveNode 没在 5s 内返回")
	}

	// 再多也不能响两次（一次握手一次回调，否则计数会漂）。
	select {
	case <-handshakes:
		t.Fatal("onHandshake 被调用了不止一次")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPostHandshakeDropIsTransient：握手成功后的连接断开必须被判成瞬态，
// 并且能被 errors.As 认成 sessionDropError（退避重置的载体）。
func TestPostHandshakeDropIsTransient(t *testing.T) {
	t.Parallel()

	h := w4Start(t, w4Config(), 1, 0)
	// 中枢侧把连接掐掉（模拟隧道重建 / 中枢重启）。
	if err := h.fc.close(); err != nil {
		t.Fatalf("关 fake 连接: %v", err)
	}
	err := h.wait(5 * time.Second)
	if err == nil {
		t.Fatal("连接断开必须返回错误（否则节点会静默消失）")
	}
	var sd *sessionDropError
	if !errors.As(err, &sd) {
		t.Fatalf("断开错误应当是 *sessionDropError（退避重置要靠它），实际 %T: %v", err, err)
	}
	if !strings.HasPrefix(err.Error(), "connection closed: ") {
		t.Errorf("文案必须以 connection closed 开头（契约 §3.4.7），实际 %q", err.Error())
	}
	if got := classifyNodeFailure(err); got.class != classTransient {
		t.Errorf("连接断开应当按瞬态重试，实际 %v", got.class)
	}
}

// ---------- 6. 入口：永久错误 → 非 0 退出码 + 可读日志 ----------

// TestNodeConfigErrorExitsNonZero：node.yaml 坏 → 退出码 1，日志里有 code= 短码
// 与一句人话（#323 验收：不能变成无限重试，要让人看见）。
func TestNodeConfigErrorExitsNonZero(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/node.yaml"
	var stderr bytes.Buffer
	code := mainWith([]string{"-f", path}, io.Discard, &stderr)
	if code == 0 {
		t.Fatalf("node.yaml 不存在时退出码应当非 0，实际 %d", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "event=agent.config.failed") {
		t.Errorf("日志缺 event=agent.config.failed：\n%s", out)
	}
	if !strings.Contains(out, "code="+failCodeConfigInvalid) {
		t.Errorf("日志缺 code=%s（短码是运维的判据）：\n%s", failCodeConfigInvalid, out)
	}
}

// TestParseFlagsNoRetry：-no-retry 解析成 options.noRetry，而且**能被
// subcommandAt 认出**（否则 `-no-retry upgrade` 会静默跑起常驻 agent）。
func TestParseFlagsNoRetry(t *testing.T) {
	t.Parallel()

	opts, err := parseFlags([]string{"-no-retry"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags(-no-retry): %v", err)
	}
	if !opts.noRetry {
		t.Error("-no-retry 没有解析成 true")
	}

	sub, rest, ok := subcommandAt([]string{"-no-retry", "upgrade", "--check"})
	if !ok || sub != upgradeCommand {
		t.Fatalf("subcommandAt(-no-retry upgrade) = (%q, %v, %v)，want (upgrade, …, true)", sub, rest, ok)
	}
}

// testLogger 是给「只关心行为、不关心日志」的用例用的静音 logger。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
