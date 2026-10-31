package server

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 限速阈值常量（passkey-enroll 匿名端点）。
const (
	rlEnrollLookupPerMin   = 10 // 匿名 lookup/by-code：10/min/IP
	rlEnrollKnockPerMin    = 10 // 匿名敲门：10/min/IP
	rlEnrollCompletePerMin = 10 // 匿名 complete：10/min/IP

	// recovery-code 登录：高价值密钥，双维度限速（对齐 CLI create / enroll 量级）。
	rlRecoveryLoginIPPerMin   = 10 // 10/min/IP
	rlRecoveryLoginUserPerMin = 10 // 10/min/username（小写）
)

var (
	rlEnrollLookup   = newFixedWindow(rlEnrollLookupPerMin, time.Minute)
	rlEnrollKnock    = newFixedWindow(rlEnrollKnockPerMin, time.Minute)
	rlEnrollComplete = newFixedWindow(rlEnrollCompletePerMin, time.Minute)

	rlRecoveryLoginIP   = newFixedWindow(rlRecoveryLoginIPPerMin, time.Minute)
	rlRecoveryLoginUser = newFixedWindow(rlRecoveryLoginUserPerMin, time.Minute)

	// hooks 入口是**双维度**限流（见 rateLimitedHook 的注释）。
	// nil 表示该维度被显式关闭（配置为负数，见 configureHookRateLimit）。
	rlHookIP    = newFixedWindow(defaultHookRatePerIPPerMin, time.Minute)
	rlHookToken = newFixedWindow(defaultHookRatePerMin, time.Minute)
)

// /v1/hooks/{token} 的默认限流额度（每分钟），可被 env 覆盖（见 Config.hookRateLimits）。
//
// 为什么这个端点必须有：它是**唯一不需要登录的公开写端点**（token 即凭据）。
// 拿到（或猜到）一个 token 就能直接触发派发；没有限流时，一个 token 泄露
// 等于把中枢的派发能力无上限地交给外网。
//
// 为什么是两维（#245）：
//   - 只看 IP：一个出口 IP 后面挂多个源（或批量告警回放）时会被总量掐死 ——
//     老实现 60/min 让批量告警从第 61 条起全 429，这是 issue #245 的现场。
//   - 只看 token：token 是攻击者手里**可以换**的东西（多个源各一个），换个 token
//     就绕过，等于没限流。
//
// 所以两维都要放行才算放行：token 维管“单通道稳态速率”，IP 维管“单机总量”。
// 600/min（10 RPS）够一条告警通道批量回放几百条；1200/min（20 RPS）给同 IP 多源留一倍余量。
const (
	defaultHookRatePerMin      = 600  // 每 token 每分钟（10 RPS）
	defaultHookRatePerIPPerMin = 1200 // 每 IP 每分钟（20 RPS）
)

// configureHookRateLimit 按配置重建 hooks 入口的两个限速器。
//
// 由 NewApp（mux 接线）在启动时调用一次，测试直接调用来摆布局部额度。
// 传 <=0 表示**关闭该维度**（把对应包级变量置 nil，rateLimitedHook 会跳过它）——
// 这是显式运维选择，所以打一条 WARN 让它在日志里留痕。
//
// ⚠️ 接线约定：参数必须来自 Config.hookRateLimits()（它负责把 0/未填归宿成默认值），
// 不要直接传 Config.HookRatePerMin 原值 —— 未经 FromEnv 构造的 Config 字段是零值，
// 直接传会把两个维度一起关掉（把唯一的匿名写入口敞开）。
func configureHookRateLimit(perToken, perIP int) {
	rlHookToken = hookWindowOrNil(perToken, "token", "ANOTIFY_HOOK_RATE_PER_MIN")
	rlHookIP = hookWindowOrNil(perIP, "ip", "ANOTIFY_HOOK_RATE_PER_IP_PER_MIN")
}

// hookWindowOrNil 把额度换算成限速器；<=0 → nil（该维度关闭）。
func hookWindowOrNil(limit int, dimension, envKey string) *fixedWindow {
	if limit <= 0 {
		slog.Warn("hook rate limit disabled",
			"event", "ratelimit.disabled",
			"dimension", dimension,
			"env", envKey,
			"hint", "显式配置为非正数：该维度不再限流，只剩另一维兜底",
		)
		return nil
	}
	return newFixedWindow(limit, time.Minute)
}

// 限速阈值常量（见 cli-auth-plan §0）.
const (
	rlCreatePerMin = 10 // 建会话：10/min/IP
	rlByCodePerMin = 20 // by-code lookup：20/min/user
	rlQRPerMin     = 30 // qr.txt：30/min/IP
	pollInterval   = 2  // 轮询最小间隔（秒）
)

// fixedWindow 是一个内存固定窗口限速器。
// key→count，窗口=1 分钟，惰性清理过期窗口。
type fixedWindow struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	buckets map[string]*rlBucket
}

type rlBucket struct {
	count    int
	windowAt time.Time
}

func newFixedWindow(limit int, window time.Duration) *fixedWindow {
	return &fixedWindow{
		window:  window,
		limit:   limit,
		buckets: make(map[string]*rlBucket),
	}
}

// allow 返回是否允许通过；同时惰性清理过期 bucket。
//
// 保留这个方法：poll/其它调用点只关心 allow/deny，不需要 Retry-After。
func (rl *fixedWindow) allow(key string) bool {
	ok, _ := rl.allowWithRetry(key)
	return ok
}

// allowWithRetry 是 allow 的增强版：放行时 ok=true（retryAfter=0），
// 拒绝时 ok=false 并给出**本窗口剩余时间**。
//
// 为什么要真实剩余时间：老实现硬编码 Retry-After: 60，窗口刚开就被拒的调用方
// 白等一整分钟（还会在窗口边界整齐地一起重试，形成脉冲）。
func (rl *fixedWindow) allowWithRetry(key string) (ok bool, retryAfter time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, exists := rl.buckets[key]
	if !exists || now.Sub(b.windowAt) >= rl.window {
		// 新窗口
		rl.buckets[key] = &rlBucket{count: 1, windowAt: now}
		// 惰性清理：窗口数超过 1024 时清理过期项
		if len(rl.buckets) > 1024 {
			rl.pruneLocked(now)
		}
		return true, 0
	}
	if b.count >= rl.limit {
		return false, rl.window - now.Sub(b.windowAt)
	}
	b.count++
	return true, 0
}

func (rl *fixedWindow) pruneLocked(now time.Time) {
	for k, b := range rl.buckets {
		if now.Sub(b.windowAt) >= rl.window {
			delete(rl.buckets, k)
		}
	}
}

// 全局限速器实例（进程级）。
var (
	rlCreate = newFixedWindow(rlCreatePerMin, time.Minute)
	rlByCode = newFixedWindow(rlByCodePerMin, time.Minute)
	rlQR     = newFixedWindow(rlQRPerMin, time.Minute)
)

// pollGuard 按 sessionId 记录上次 poll 时间，保证最小间隔。
type pollGuard struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var pollG = &pollGuard{last: make(map[string]time.Time)}

// allow 返回是否允许 poll（距上次 < pollInterval*0.8 → false）。
func (g *pollGuard) allow(sessionID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if t, ok := g.last[sessionID]; ok {
		minInterval := time.Duration(float64(pollInterval)*0.8*float64(time.Second.Nanoseconds())) * time.Nanosecond
		if now.Sub(t) < minInterval {
			return false
		}
	}
	g.last[sessionID] = now
	// 惰性清理
	if len(g.last) > 4096 {
		for k, t := range g.last {
			if now.Sub(t) > time.Minute {
				delete(g.last, k)
			}
		}
	}
	return true
}

// trustProxyHeaders 控制是否信任 X-Forwarded-For。默认 false（直连源站不可信），
// 由 NewApp 根据 Config.TrustProxy 设置。仅在反代（cloudflared/nginx）后开启。
var trustProxyHeaders bool

// clientIP 从请求提取客户端 IP。
// 默认用 RemoteAddr（去端口）；仅当 trustProxyHeaders=true 时取 X-Forwarded-For 第一段。
func clientIP(r *http.Request) string {
	if trustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.Index(xff, ","); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host
}

// rateLimited 检查限速，超限则写 429 并返回 true。
//
// 签名保持不变：enroll / recovery / cli-auth 等 6 个调用点不该为了日志细节改动。
func rateLimited(w http.ResponseWriter, rl *fixedWindow, key string) bool {
	if rl == nil { // 该维度被显式关闭
		return false
	}
	ok, retryAfter := rl.allowWithRetry(key)
	if ok {
		return false
	}
	writeRateLimited(w, "key", retryAfter)
	return true
}

// rateLimitedHook 是 POST /v1/hooks/{token} 的双维度限流入口。
//
// 维度与顺序：先 IP 维，再 token 维。任一维拒绝即 429。
//
// 为什么先 IP：
//   - 这个端点的 token 是攻击者可以轮换的东西，「换 token 即绕过」是老实现的洞；
//   - 先判 IP 能在任何 DB 查询 / 派发动作之前，把「一台机器狂刷」这种最常见、
//     最便宜的打法挡掉，也让单机脚本无法靠轮换 token 把成本摊薄到各个 token 额度上；
//   - IP 额度（默认 1200/min）比 token 额度（默认 600/min）宽一倍，所以正常的多源
//     同 IP 场景不会因为先判 IP 而被误伤，拒的总是更极端的那一侧。
//
// 反过来（先 token）唯一的好处是 token 喷洒时能更早地对单个 token 生效，
// 但那类请求本来就会被 IP 维在下一步拒掉，日志与成本上没有差别。
func rateLimitedHook(w http.ResponseWriter, r *http.Request) bool {
	if rlHookIP != nil {
		if ok, retryAfter := rlHookIP.allowWithRetry(clientIP(r)); !ok {
			writeRateLimited(w, "hook:ip", retryAfter)
			return true
		}
	}
	if rlHookToken != nil {
		// token 为空时不占用 token 维额度（否则所有空 token 请求共用一个桶，
		// 会被一个乱扫把后面正常的 404 也变成 429）。
		if token := r.PathValue("token"); token != "" {
			if ok, retryAfter := rlHookToken.allowWithRetry(token); !ok {
				writeRateLimited(w, "hook:token", retryAfter)
				return true
			}
		}
	}
	return false
}

// writeRateLimited 写 429，并把「等多久 / 哪个维度」塞给最外层 accessLog。
//
// 为什么日志不在这里写：老实现限流器写一条 WARN、accessLog 再写一条 WARN，
// 每个 429 双写；c=16 打满时 10 秒能刷出 75MB 日志（issue #245）。
// 现在只由 accessLog 写**唯一一行**，代价是维度信息要靠 statusWriter 往外传。
// 故意**不**加响应头暴露维度名：那是内部实现细节，只进日志。
func writeRateLimited(w http.ResponseWriter, scope string, retryAfter time.Duration) {
	secs := retryAfterSeconds(retryAfter)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	if sw, ok := w.(*statusWriter); ok {
		sw.retryAfter = secs
		sw.rlScope = scope
	}
	writeErr(w, 429, "请求过于频繁，请稍后再试")
}

// retryAfterSeconds 把窗口剩余时长换算成 Retry-After 秒，并夹到 [1,60]。
//
// 向上取整：剩余 0.2s 也要让调用方至少等 1s，否则立刻重试只会再吃一次 429。
// 上限 60 对应固定窗口的长度（窗口是个配置常量，将来调长再改这里）。
func retryAfterSeconds(d time.Duration) int {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		return 1
	}
	if secs > 60 {
		return 60
	}
	return secs
}
