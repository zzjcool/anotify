package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// SessionCookieName 是会话 Cookie 名。
const SessionCookieName = "anotify_session"

// ctxKey 是 request context 的私有键类型。
type ctxKey string

// ctxUserID 是注入 context 的 userID 键。
const ctxUserID ctxKey = "anotify.userID"

// UserIDFromContext 从 context 取 userID（未登录返回空串）。
func UserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxUserID).(string); ok {
		return v
	}
	return ""
}

// WithUserID 把 userID 注入 context 并返回新 context。
// 供会话中间件注入登录身份；也可在测试中直接构造已鉴权 context。
func WithUserID(ctx context.Context, userID string) context.Context {
	return withUserID(ctx, userID)
}

// withUserID 把 userID 注入 context。
func withUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxUserID, userID)
}

// 会话鉴权链的三种失败（文案与改造前逐字一致，e2e 可能锁着它们）。
var (
	// errSessionInvalid：会话不存在 / DB 查询失败 / 用户查不到（一律按未登录处理，不泄露细节）。
	errSessionInvalid = errors.New("auth: 会话无效")
	// errSessionExpired：会话本身已过期。
	errSessionExpired = errors.New("auth: 会话已过期")
	// errSessionDisabled：会话有效但账户已被禁用（需额外清 Cookie，文案与其它 401 不同）。
	errSessionDisabled = errors.New("账户已被禁用")
)

// SessionManager 管理登录会话（签发/校验/吊销）。
//
// 鉴权热路径上还带一层进程内会话缓存 + last_seen 写入节流（#241），
// 设计与取舍见 session_cache.go。
type SessionManager struct {
	db     *store.DB
	ttl    time.Duration
	secure bool

	// cacheTTL 是会话缓存有效期。<= 0 表示缓存关闭（每个请求都查库）。
	cacheTTL time.Duration
	// touchInterval 是 last_seen 的最小落库间隔。
	touchInterval time.Duration

	mu     sync.RWMutex
	cache  map[string]*sessEntry // key: sessionID；nil 表示缓存关闭
	hits   atomic.Uint64
	misses atomic.Uint64
}

// NewSessionManager 构造会话管理器（缓存 TTL / 节流间隔用默认值）。
func NewSessionManager(db *store.DB, ttl time.Duration, secure bool) *SessionManager {
	return NewSessionManagerTuned(db, ttl, secure, defaultSessionCacheTTL)
}

// NewSessionManagerTuned 构造会话管理器并显式指定会话缓存 TTL（env 接线的注入点）。
//
// cacheTTL < 0 表示关闭缓存：所有请求走完整 DB 校验（排障/对齐旧行为用）。
// cacheTTL == 0 按契约「0 = 用默认」处理，等价于 NewSessionManager。
func NewSessionManagerTuned(db *store.DB, ttl time.Duration, secure bool, cacheTTL time.Duration) *SessionManager {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	if cacheTTL == 0 {
		cacheTTL = defaultSessionCacheTTL
	}
	m := &SessionManager{
		db:            db,
		ttl:           ttl,
		secure:        secure,
		cacheTTL:      cacheTTL,
		touchInterval: defaultTouchInterval,
	}
	if cacheTTL > 0 {
		m.cache = make(map[string]*sessEntry)
	}
	return m
}

// Create 为某用户签发一个新会话。deviceName 从 UA 推断（如「Chrome · macOS」）。
func (m *SessionManager) Create(userID, deviceName string) (*store.Session, error) {
	tok, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s := &store.Session{
		ID:         store.NewSessionID() + "." + tok, // 加随机后缀，避免 ID 可枚举
		UserID:     userID,
		DeviceName: deviceName,
		CreatedAt:  now.Unix(),
		ExpiresAt:  now.Add(m.ttl).Unix(),
		LastSeen:   now.Unix(),
	}
	if err := m.db.CreateSession(s); err != nil {
		return nil, err
	}
	return s, nil
}

// ListByUser 列出某用户的全部会话。
func (m *SessionManager) ListByUser(userID string) ([]*store.Session, error) {
	return m.db.ListSessionsByUser(userID)
}

// Validate 校验会话 ID，返回会话（过期视为无效）。
//
// 注意：Validate 直接查库、不走会话缓存——它要返回完整 Session，而缓存只存 userID/expiresAt。
// 需要缓存加速的是中间件热路径（见 Middleware/authSession）。
func (m *SessionManager) Validate(sessionID string) (*store.Session, error) {
	s, err := m.db.GetSession(sessionID)
	if err != nil {
		return nil, errSessionInvalid
	}
	if time.Now().Unix() > s.ExpiresAt {
		return nil, errSessionExpired
	}
	return s, nil
}

// Revoke 吊销一个会话，并同步失效其缓存条目。
//
// 顺序有讲究：必须 DB 删除成功之后才删缓存。DB 删除失败时会话仍然有效，
// 此时清缓存只会白丢性能（下一次请求照样能查回这个会话）。
func (m *SessionManager) Revoke(sessionID string) error {
	if err := m.db.DeleteSession(sessionID); err != nil {
		return err
	}
	m.InvalidateSession(sessionID)
	return nil
}

// SetCookie 把会话 ID 写入 httpOnly Cookie。
func (m *SessionManager) SetCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(m.ttl),
	})
}

// ClearCookie 清除会话 Cookie。
func (m *SessionManager) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		MaxAge:   -1,
	})
}

// Middleware 校验会话 Cookie，把 userID 注入 request context。
// 未登录/会话无效时返回 401。
func (m *SessionManager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(SessionCookieName)
		if err != nil || c.Value == "" {
			slog.Warn("session cookie missing",
				"event", "auth.session.invalid",
				"reason", "missing_cookie",
				"path", r.URL.Path,
			)
			http.Error(w, `{"error":"未登录"}`, http.StatusUnauthorized)
			return
		}
		// 会话校验走缓存热路径：命中时 0 次 DB 往返，未命中才 GetSession+GetUserByID。
		// 用户禁用判定在 authSession 里（DB 读错误必须 fail-closed，不再静默放行）。
		userID, entry, err := m.authSession(c.Value)
		if err != nil {
			if errors.Is(err, errSessionDisabled) {
				slog.Warn("session user disabled",
					"event", "auth.session.disabled",
					"user_id", userID,
					"path", r.URL.Path,
				)
				m.ClearCookie(w)
				http.Error(w, `{"error":"账户已被禁用"}`, http.StatusUnauthorized)
				return
			}
			slog.Warn("session invalid",
				"event", "auth.session.invalid",
				"reason", err.Error(),
				"path", r.URL.Path,
			)
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
			return
		}
		// 刷新活跃时间：touchInterval 内（默认 60s 窗口）最多落库一次，且是异步写，
		// 不再让每个请求都背一个同步 UPDATE 写事务。
		m.touchSession(c.Value, entry)
		next.ServeHTTP(w, r.WithContext(withUserID(r.Context(), userID)))
	})
}

// randomToken 生成 URL 安全的随机串。
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("auth: 随机数生成失败")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DeviceNameFromUA 从 User-Agent 推断设备名（如「Chrome · macOS」）。
// 用于登录会话、Passkey 凭证命名等，统一设备名格式。
func DeviceNameFromUA(ua string) string {
	ua = strings.ToLower(ua)

	// 浏览器
	browser := "Browser"
	switch {
	case strings.Contains(ua, "edg/"):
		browser = "Edge"
	case strings.Contains(ua, "chrome/") || strings.Contains(ua, "crios"):
		browser = "Chrome"
	case strings.Contains(ua, "firefox/") || strings.Contains(ua, "fxios"):
		browser = "Firefox"
	case strings.Contains(ua, "safari/") && !strings.Contains(ua, "chrome"):
		browser = "Safari"
	}

	// 操作系统
	osName := "Unknown OS"
	switch {
	case strings.Contains(ua, "iphone") || strings.Contains(ua, "ipad"):
		osName = "iOS"
	case strings.Contains(ua, "mac os") || strings.Contains(ua, "macintosh"):
		osName = "macOS"
	case strings.Contains(ua, "windows"):
		osName = "Windows"
	case strings.Contains(ua, "android"):
		osName = "Android"
	case strings.Contains(ua, "linux"):
		osName = "Linux"
	}

	return browser + " · " + osName
}
