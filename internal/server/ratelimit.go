package server

import (
	"log/slog"
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
)

var (
	rlEnrollLookup   = newFixedWindow(rlEnrollLookupPerMin, time.Minute)
	rlEnrollKnock    = newFixedWindow(rlEnrollKnockPerMin, time.Minute)
	rlEnrollComplete = newFixedWindow(rlEnrollCompletePerMin, time.Minute)
)

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
func (rl *fixedWindow) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok || now.Sub(b.windowAt) >= rl.window {
		// 新窗口
		rl.buckets[key] = &rlBucket{count: 1, windowAt: now}
		// 惰性清理：窗口数超过 1024 时清理过期项
		if len(rl.buckets) > 1024 {
			rl.pruneLocked(now)
		}
		return true
	}
	if b.count >= rl.limit {
		return false
	}
	b.count++
	return true
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

// 限速阈值常量（reply 端点）。
const rlReplyPerMin = 20 // 回复：20/min/user

var rlReply = newFixedWindow(rlReplyPerMin, time.Minute)

// allow wraps fixedWindow.allow to satisfy api.RateLimiter interface.
func (rl *fixedWindow) Allow(key string) bool {
	return rl.allow(key)
}

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

// rateLimited 检查限速，超限则写 429 并返回 false。
func rateLimited(w http.ResponseWriter, rl *fixedWindow, key string) bool {
	if !rl.allow(key) {
		slog.Warn("rate limited",
			"event", "ratelimit.hit",
			"key", key,
		)
		w.Header().Set("Retry-After", strconv.Itoa(60))
		writeErr(w, 429, "请求过于频繁，请稍后再试")
		return true
	}
	return false
}
