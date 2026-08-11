package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
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

// SessionManager 管理登录会话（签发/校验/吊销）。
type SessionManager struct {
	db     *store.DB
	ttl    time.Duration
	secure bool
}

// NewSessionManager 构造会话管理器。
func NewSessionManager(db *store.DB, ttl time.Duration, secure bool) *SessionManager {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &SessionManager{db: db, ttl: ttl, secure: secure}
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
func (m *SessionManager) Validate(sessionID string) (*store.Session, error) {
	s, err := m.db.GetSession(sessionID)
	if err != nil {
		return nil, errors.New("auth: 会话无效")
	}
	if time.Now().Unix() > s.ExpiresAt {
		return nil, errors.New("auth: 会话已过期")
	}
	return s, nil
}

// Revoke 吊销一个会话。
func (m *SessionManager) Revoke(sessionID string) error {
	return m.db.DeleteSession(sessionID)
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
		sess, err := m.Validate(c.Value)
		if err != nil {
			slog.Warn("session invalid",
				"event", "auth.session.invalid",
				"reason", err.Error(),
				"path", r.URL.Path,
			)
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
			return
		}
		// 校验用户是否被禁用（超管后台禁用某用户后立即生效，所有端点拒绝）。
		if u, err := m.db.GetUserByID(sess.UserID); err == nil && u.Disabled {
			slog.Warn("session user disabled",
				"event", "auth.session.disabled",
				"user_id", sess.UserID,
				"path", r.URL.Path,
			)
			m.ClearCookie(w)
			http.Error(w, `{"error":"账户已被禁用"}`, http.StatusUnauthorized)
			return
		}
		// 异步刷新活跃时间（忽略错误）。
		_ = m.db.TouchSession(sess.ID, time.Now().Unix())
		next.ServeHTTP(w, r.WithContext(withUserID(r.Context(), sess.UserID)))
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
