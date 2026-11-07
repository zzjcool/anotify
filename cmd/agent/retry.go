package main

// 本文件是节点模式的**失败分类 + 进程内重连退避**（issue #323）。
//
// 为什么要有它：改动之前，runNode 返回的任何错误都走同一条路 ——
// `log.Error("节点退出", event=agent.fatal)` + 退出码 1，而单元文件是
// `Restart=always` / `RestartSec=3` / `StartLimitIntervalSec=0`。于是「中枢/隧道
// 不可达」被翻译成 **3 秒一次的无退避崩溃循环**（实测：本地 dev 栈重建的 ~55 秒里
// 打进 6 次重启；中枢侧较长时间 530 时 restart counter 累计到 404）。代价不只是
// 日志噪声：
//
//   - 每次重启都走完整 hello 对账（全量 applied），中枢恢复的瞬间会被同一台节点
//     以 3 秒一次的频率握手，多节点时是乘法效应；
//   - 404 条 agent.fatal 把同一时间窗里的真故障（比如这次的缺 PATH）埋掉，
//     journal 检索基本失效。
//
// 但也不能一刀切「永不退出」：令牌被删 / 协议版本不兼容 / node.yaml 坏 必须**快速
// 失败并让人看见**，否则节点会静默挂在那里永远连不上。所以失败分两类：
//
//	类别   例子                                        行为
//	瞬态   dial 失败、5xx/530、网络超时、握手超时、      进程内指数退避重试（1s→2s→4s…
//	       握手 5xx、连接断开                            封顶 30s，带 ±20% jitter），不退出
//	永久   401/403、protocol_mismatch、                 立刻返回错误，由 run() 记一条带 code 的
//	       too_many_instances、node.yaml 坏、           agent.fatal，退出码 1（交给 systemd / 人）
//	       data-dir 不可用
//
// 判据一律是**结构化**的（HTTP 状态码 / 错误类型 / 协议短码），不靠字符串 contains：
// 分类表在 classifyNodeFailure，逐条用例在 retry_test.go。判不出来时**默认瞬态**
// ——宁可重试，也不要因为一次抖动就打死节点。
//
// 退避状态在「握手成功」那一刻重置（serveNode 通过 sessionHooks 汇报），所以
// 「连上就跑、断了立刻重来」与「连不上就一直退避」是两条不同的曲线。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/zzjcool/anotify/internal/ws"
)

// 退避参数。选值的依据是 #323 的验收「切断中枢 60 秒：agent 不退，日志 ≤5 条」：
// 1s→2s→4s→8s→16s→30s 在 60 秒里只有 6 次尝试，前 3 次逐条打日志，
// 一共 3 条重试日志（+1 条恢复日志）。
const (
	// backoffBase 是第一次失败后的等待（之后每次翻倍）。
	backoffBase = time.Second
	// backoffMax 是退避上限。取 30s 而不是 60s：中枢恢复后最坏也多等 30 秒，
	// 而 30s 已经足够把「3 秒一次」的握手风暴压下去（差 10 倍量级）。
	backoffMax = 30 * time.Second
	// backoffFactor 是每次失败后的放大倍数（指数退避）。
	backoffFactor = 2.0
	// backoffJitter 是抖动比例（±20%）：多节点同时掉线时不要让它们对齐重连。
	backoffJitter = 0.2

	// retryLogFirst 是「逐条打日志」的连续失败次数上限。
	retryLogFirst = 3
	// retryLogEvery 是降噪后的打点周期：之后每 M 次一条 WARN（其余降到 DEBUG）。
	retryLogEvery = 10
)

// 失败短码。它们进日志的 code= 字段，也是「这条错误该重试还是该退出」的唯一判据
// 的标签（运维按它检索：`journalctl -u anotify-agent | grep code=token_rejected`）。
const (
	// failCodeDialFailed：没拿到任何 HTTP 响应（DNS / TLS / 拒连 / 超时）。
	failCodeDialFailed = "dial_failed"
	// failCodeServerUnavailable：中枢侧 5xx（含 Cloudflare 隧道的 530）。
	failCodeServerUnavailable = "server_unavailable"
	// failCodeServerThrottled：408 / 429（过载或限流，等一下就好）。
	failCodeServerThrottled = "server_throttled"
	// failCodeTokenRejected：401 / 403。令牌被删、属于别的中枢/命名空间，或节点在 draining。
	failCodeTokenRejected = "token_rejected"
	// failCodeEndpointRejected：其余 4xx —— 地址/路径写错（node.yaml 的 server）。
	failCodeEndpointRejected = "endpoint_rejected"
	// failCodeProtocolMismatch：中枢明说协议版本不兼容（要去升级二进制）。
	failCodeProtocolMismatch = "protocol_mismatch"
	// failCodeTooManyInstances：节点申报的槽位/实例数超过中枢允许的上限（改 node.yaml）。
	failCodeTooManyInstances = "too_many_instances"
	// failCodeHandshakeRejected：握手被拒，但码不在上面那几条里（默认按瞬态重试）。
	failCodeHandshakeRejected = "handshake_rejected"
	// failCodeHandshakeTimeout：welcome 没在 handshakeTimeout 内到（中枢很慢/半死）。
	failCodeHandshakeTimeout = "handshake_timeout"
	// failCodeConnectionLost：握手成功之后连接断了（中枢重启、隧道重建）。
	failCodeConnectionLost = "connection_lost"
	// failCodeUnclassified：判不出来的错误 —— 按瞬态处理（见文件头最后一段）。
	failCodeUnclassified = "unclassified"
	// failCodeConfigInvalid / failCodeDataDirUnavailable：启动前就失败的本地错误。
	failCodeConfigInvalid        = "config_invalid"
	failCodeDataDirUnavailable   = "datadir_unavailable"
	failReasonConfigInvalid      = "node.yaml 不合法或不存在：按报错改好后重启（这条不会自愈，重启也没用）"
	failReasonDataDirUnavailable = "数据目录不可用：目录不存在/无权限/路径算不出来，改 -data-dir 后重启"
)

// failureClass 决定「进程内重试」还是「退出让 systemd / 人接手」。
type failureClass int

const (
	// classTransient：中枢或网络这一侧的问题，进程内退避重试，进程**不退出**。
	classTransient failureClass = iota
	// classPermanent：要人改配置 / 升级二进制的错误，立刻退出非 0。
	classPermanent
)

// nodeFailure 是一次失败的**分类结论**：类别 + 短码 + 一句人话。
//
// 它是纯数据（分类是纯函数），所以判据表可以逐条单测，不需要真去拨号。
type nodeFailure struct {
	class  failureClass
	code   string // 短码（日志 code= / state.json 的 kind）
	reason string // 给人看的一句话；不含机器判据
	err    error
}

// permanent 报告这条失败是否必须立刻退出。
func (f nodeFailure) permanent() bool { return f.class == classPermanent }

// ---------- 分类载体（typed errors）----------
//
// 三种错误类型各自保留**原来的文案**（预检脚本、已有测试、运维的 grep 都依赖它），
// 只是多带一份机器可判的字段。Error() 一律委托给原始 error，避免文案漂移。

// dialError 是建连失败的分类载体。
//
// status 是握手响应的 HTTP 状态码：**0 表示根本没拿到响应**（DNS/TLS/拒连/超时），
// 非 0 是中枢真的回了一个状态码（530 是 Cloudflare 隧道打不通时回的 5xx）。
// coder/websocket 的 Dial 在拿不到 101 时会把 *http.Response 带回来，所以这里
// 不需要从错误字符串里抠数字（那正是 #323 要求避免的判据形态）。
type dialError struct {
	status int
	url    string
	err    error
}

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// handshakeRejectError 是中枢在 welcome 之前下发 error 帧的拒绝（契约 §3.4.1）。
//
// 文案逐字保留成 `handshake rejected: <code>: <message>`：中枢给的 message 是运维
// 唯一能看懂的线索，不能因为分类而不打出来。
type handshakeRejectError struct {
	code    string
	message string
}

func (e *handshakeRejectError) Error() string {
	return "handshake rejected: " + e.code + ": " + e.message
}

// sessionDropError 标记「**握手成功之后**连接断了」。
//
// 它与「从来没连上」的区别正是退避重置的判据：握手成功说明中枢可达、令牌有效，
// 这次的断线是网络/中枢重启造成的，下一次重连应当从最短间隔开始（见 retryState）。
type sessionDropError struct{ cause error }

func (e *sessionDropError) Error() string { return e.cause.Error() }
func (e *sessionDropError) Unwrap() error { return e.cause }

// permanentError 把「压根没走到网络」的本地错误标记成永久（配置 / 数据目录）。
//
// 它在 dial 之前就返回，所以本不需要进重试循环；给它类型是为了让分类表**完整**
// ——「node.yaml 坏 → 永久」这条验收有可判的、不靠字符串的载体。
type permanentError struct {
	code   string
	reason string
	err    error
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// ---------- 分类 ----------

// classifyNodeFailure 是**唯一**的分类入口（判据表）。
//
// 顺序即优先级：本地永久错误 → 拨号（按 HTTP 状态码）→ 握手拒绝（按协议短码）
// → 握手超时 → 连接断开 → 兜底瞬态。
func classifyNodeFailure(err error) nodeFailure {
	if err == nil {
		return nodeFailure{code: failCodeUnclassified}
	}

	var pe *permanentError
	if errors.As(err, &pe) {
		return nodeFailure{class: classPermanent, code: pe.code, reason: pe.reason, err: err}
	}

	var de *dialError
	if errors.As(err, &de) {
		return classifyDialFailure(de)
	}

	var hr *handshakeRejectError
	if errors.As(err, &hr) {
		return classifyHandshakeReject(hr)
	}

	// 握手超时：handshake 里包的是 context.DeadlineExceeded（见 client.go）。
	if errors.Is(err, context.DeadlineExceeded) {
		return nodeFailure{
			class:  classTransient,
			code:   failCodeHandshakeTimeout,
			reason: "中枢没有在握手超时内回应（可能半死/被隧道吞了）",
			err:    err,
		}
	}

	var sd *sessionDropError
	if errors.As(err, &sd) {
		return nodeFailure{
			class:  classTransient,
			code:   failCodeConnectionLost,
			reason: "握手成功之后连接断了（中枢重启/隧道重建）",
			err:    err,
		}
	}

	// 兜底：判不出来就按瞬态。**这是刻意的** —— 宁可多试几次，也不要因为一次
	// 我们还没想到的抖动把节点打死（#323 §「判不出来时默认当瞬态」）。
	return nodeFailure{
		class:  classTransient,
		code:   failCodeUnclassified,
		reason: "未归类的连接错误（按瞬态重试）",
		err:    err,
	}
}

// classifyDialFailure 按 **HTTP 状态码** 分类拨号失败。
//
// 判据是状态码的**类别**，不是文案：
//   - 没有响应（0）→ 瞬态（DNS/TLS/拒连/超时都是网络这一侧的事）；
//   - 401/403 → 永久（令牌被删、属于别的中枢/命名空间，重试一万次也一样）；
//   - 408/429 → 瞬态（超时/限流，等一下就好 —— 它们虽然也是 4xx）；
//   - 5xx（含隧道的 530）→ 瞬态（中枢自己不可用）；
//   - 其余 4xx → 永久（地址/路径写错：node.yaml 的 server 指错了地方）。
func classifyDialFailure(e *dialError) nodeFailure {
	switch {
	case e.status == 0:
		return nodeFailure{
			class:  classTransient,
			code:   failCodeDialFailed,
			reason: "连不上中枢（DNS/证书/拒连/超时）",
			err:    e,
		}
	case e.status == http.StatusUnauthorized || e.status == http.StatusForbidden:
		return nodeFailure{
			class: classPermanent,
			code:  failCodeTokenRejected,
			reason: fmt.Sprintf(
				"节点令牌被中枢拒绝（HTTP %d）：令牌可能已被删除，或它属于另一个中枢/命名空间",
				e.status),
			err: e,
		}
	case e.status == http.StatusRequestTimeout || e.status == http.StatusTooManyRequests:
		return nodeFailure{
			class:  classTransient,
			code:   failCodeServerThrottled,
			reason: fmt.Sprintf("中枢暂时不可用（HTTP %d，超时/限流）", e.status),
			err:    e,
		}
	case e.status >= 500:
		return nodeFailure{
			class:  classTransient,
			code:   failCodeServerUnavailable,
			reason: fmt.Sprintf("中枢侧不可用（HTTP %d）", e.status),
			err:    e,
		}
	default:
		return nodeFailure{
			class: classPermanent,
			code:  failCodeEndpointRejected,
			reason: fmt.Sprintf(
				"中枢拒绝了这个地址（HTTP %d）：核对 node.yaml 里的 server（路径/端口/反代）",
				e.status),
			err: e,
		}
	}
}

// classifyHandshakeReject 按**协议短码**分类握手拒绝（契约 §6.1 的错误码表）。
//
// 只有「重试改变不了结果」的码才算永久：版本不兼容（要换二进制）、令牌无效、
// 实例数超顶（要改 node.yaml）。其余（bad_frame / invalid_transition / 中枢侧
// 临时故障被 failHello 兜成 bad_frame）默认瞬态 —— 判不出来就重试。
func classifyHandshakeReject(e *handshakeRejectError) nodeFailure {
	switch e.code {
	case ws.ErrCodeProtocolMismatch:
		return nodeFailure{
			class:  classPermanent,
			code:   failCodeProtocolMismatch,
			reason: "中枢与本机 agent 的协议版本不兼容：升级 agent（anotify-agent upgrade）后再启动",
			err:    e,
		}
	case ws.ErrCodeUnauthorized:
		return nodeFailure{
			class:  classPermanent,
			code:   failCodeTokenRejected,
			reason: "节点令牌无效：去控制台「节点」页重新注册，把新的 --token 带上来",
			err:    e,
		}
	case ws.ErrCodeTooManyInstances:
		return nodeFailure{
			class:  classPermanent,
			code:   failCodeTooManyInstances,
			reason: "本机申报的槽位/实例数超过中枢允许的上限：改 node.yaml 的 instances 后重启",
			err:    e,
		}
	default:
		return nodeFailure{
			class:  classTransient,
			code:   failCodeHandshakeRejected,
			reason: "握手被拒（码不在永久表里，按瞬态重试）",
			err:    e,
		}
	}
}

// ---------- 退避 ----------

// backoffPolicy 是重连退避的参数（字段全部可注入，单测才能跑到毫秒级、不真睡）。
type backoffPolicy struct {
	Base   time.Duration
	Max    time.Duration
	Factor float64
	Jitter float64
	// Rand 返回 [0,1)（抖动用）；nil → math/rand/v2。
	Rand func() float64
	// Wait 退避等待，返回 false 表示 ctx 在等待期间被取消（信号退出）。
	// nil → 真睡（可被 ctx 打断）。
	Wait func(ctx context.Context, d time.Duration) bool
	// Disabled：只试一次（`-no-retry`：安装脚本的预检、一次性探测）。
	// 瞬态错误也立刻返回，行为与 #323 之前一致。
	Disabled bool
}

// defaultBackoffPolicy 是生产用的退避参数（1s 起、封顶 30s、±20% 抖动）。
func defaultBackoffPolicy() backoffPolicy {
	return backoffPolicy{
		Base:   backoffBase,
		Max:    backoffMax,
		Factor: backoffFactor,
		Jitter: backoffJitter,
		Rand:   rand.Float64,
		Wait:   waitBackoff,
	}
}

// waitBackoff 是真睡：定时器与 ctx 竞速，ctx 先到就返回 false。
//
// 用 time.NewTimer 而不是 time.Sleep：SIGTERM 必须能**立刻**打断退避
// （`systemctl stop` 的 TimeoutStopSec 只有 30s，不能被一次退避拖住）。
func waitBackoff(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// delay 返回第 attempt 次（1-based）**连续失败**之后的等待时长。
//
// 形状：base × factor^(attempt-1)，封顶 Max，再叠加 ±Jitter 的抖动；
// 抖动之后再夹一次 [Base/2, Max]（抖动不该把间隔打到 0，也不该突破上限）。
// 循环在触顶时立刻停（attempt 很大时既不会溢出，也不会白转）。
func (p backoffPolicy) delay(attempt int) time.Duration {
	base := p.Base
	if base <= 0 {
		base = backoffBase
	}
	maxDelay := p.Max
	if maxDelay <= 0 {
		maxDelay = backoffMax
	}
	factor := p.Factor
	if factor < 1 {
		factor = backoffFactor
	}
	if attempt < 1 {
		attempt = 1
	}

	d := float64(base)
	for i := 1; i < attempt; i++ {
		if d >= float64(maxDelay) {
			break
		}
		d *= factor
	}
	if d > float64(maxDelay) {
		d = float64(maxDelay)
	}
	if p.Jitter > 0 {
		r := 0.5
		if p.Rand != nil {
			r = p.Rand()
		}
		d *= 1 + (r*2-1)*p.Jitter
	}
	if d > float64(maxDelay) {
		d = float64(maxDelay)
	}
	if d < float64(base)/2 {
		d = float64(base) / 2
	}
	return time.Duration(d)
}

// wait 退避（可注入；nil 策略字段退回真睡）。
func (p backoffPolicy) wait(ctx context.Context, d time.Duration) bool {
	if p.Wait != nil {
		return p.Wait(ctx, d)
	}
	return waitBackoff(ctx, d)
}

// retryState 是「连续失败」的累计状态：退避阶梯 + 日志降噪 + 恢复汇报。
//
// 它只被 reconnectLoop 的一个 goroutine 使用，所以不需要锁。
type retryState struct {
	log    *slog.Logger
	policy backoffPolicy
	now    func() time.Time

	consecutive int       // 连续失败次数（握手成功即清零 → 退避重置）
	total       int64     // 本进程累计失败次数（不清零：恢复日志要报它）
	firstFailAt time.Time // 本轮连续失败从什么时候开始（恢复日志报「断了多久」）
}

// newRetryState 构造退避状态。
func newRetryState(log *slog.Logger, policy backoffPolicy) *retryState {
	if log == nil {
		log = slog.Default()
	}
	return &retryState{log: log, policy: policy, now: time.Now}
}

// onFailure 记一次连续失败，返回**下一次尝试之前**要等的时长。
//
// 日志降噪（#323 要求「连续失败前 N 次逐条，之后每 M 次一条」）：
//   - 第 1..retryLogFirst 次：每条一条 WARN（带 attempt / delay / code / error）；
//   - 之后每 retryLogEvery 次（第 10、20…次）：一条 WARN，额外带「中间抑制了几条」；
//   - 其余：降到 DEBUG（默认 INFO 级看不到，需要时 `-- 调级` 才看得到）。
//
// 为什么不干脆只留一条：前几次是运维「刚断」时最需要看的现场；再往后每 10 次一条
// 既证明进程还活着、在退避，又不会把 journal 刷掉。
func (s *retryState) onFailure(f nodeFailure) time.Duration {
	s.consecutive++
	s.total++
	if s.firstFailAt.IsZero() {
		s.firstFailAt = s.now()
	}
	delay := s.policy.delay(s.consecutive)

	attrs := []any{
		"event", "agent.retry",
		"code", f.code,
		"attempt", s.consecutive,
		"delay", delay.String(),
	}
	if f.err != nil {
		attrs = append(attrs, "error", f.err.Error())
	}

	switch {
	case s.consecutive <= retryLogFirst:
		s.log.Warn("中枢不可用，退避后重试", attrs...)
	case s.consecutive%retryLogEvery == 0:
		s.log.Warn("中枢仍不可用（中间日志已抑制）", append(attrs,
			"suppressed", s.consecutive-retryLogFirst)...,
		)
	default:
		s.log.Debug("中枢仍不可用（日志已抑制）", attrs...)
	}
	return delay
}

// onHandshake 记一次「握手成功」：打一条带累计次数的恢复日志，并**重置退避**。
//
// 只在「这一轮确实断过」时打（首次就握手成功不打，否则每次正常启动都多一条噪声）。
func (s *retryState) onHandshake() {
	if s.consecutive == 0 {
		return
	}
	var down string
	if !s.firstFailAt.IsZero() {
		down = s.now().Sub(s.firstFailAt).Round(time.Second).String()
	}
	s.log.Info("中枢已恢复：握手成功",
		"event", "agent.retry.recovered",
		"failed_attempts", s.consecutive,
		"failures_total", s.total,
		"down_for", down,
	)
	// 退避重置：下一次断线从 1s 重新开始，而不是接着上次的 30s。
	s.consecutive = 0
	s.firstFailAt = time.Time{}
}

// ---------- 重连循环 ----------

// attemptFunc 是一次「连上并跑到断」的尝试。
//
// hooks 里的事件由 serveNode 在**真实事件点**触发（目前只有握手成功），
// 循环据此区分「连上过又断了」与「从来没连上」。
type attemptFunc func(ctx context.Context, hooks sessionHooks) error

// reconnectLoop 是节点模式的**进程内重连**：瞬态退避重试，永久立刻返回。
//
// 返回 nil 表示「按信号正常退出」（SIGINT/SIGTERM 收尾完毕，或退避期间收到信号）；
// 返回非 nil 表示**不可重试**的失败 —— 调用方（run）负责记一条带 code 的 fatal
// 并把它翻成退出码 1。
//
// 为什么不在这里打 fatal：日志的「一次事件一条」原则。重试日志在这里，
// 「节点退出」在 run（那里才知道退出码）；分类是纯函数，两处算出同一个 code。
func reconnectLoop(ctx context.Context, log *slog.Logger, policy backoffPolicy, attempt attemptFunc) error {
	state := newRetryState(log, policy)
	for {
		err := attempt(ctx, sessionHooks{onHandshake: state.onHandshake})
		if err == nil {
			// 信号收尾完毕（serveNode 的正常退出路径）。
			return nil
		}
		if ctx.Err() != nil {
			// 信号把这次尝试打断了（拨号中/握手中收到 SIGTERM）：按正常退出处理。
			// 与 serveNode 里「握手中途收到信号 → 返回 nil」同一口径。
			return nil
		}
		f := classifyNodeFailure(err)
		if f.permanent() {
			return err
		}
		if policy.Disabled {
			// `-no-retry`：单次尝试，把错误原样交回去（安装脚本的预检要的是
			// 「能不能连上」这个立刻的答案，不是「等中枢恢复」）。
			return err
		}
		delay := state.onFailure(f)
		if !policy.wait(ctx, delay) {
			// 退避期间收到信号：正常退出（不再尝试）。
			return nil
		}
	}
}
