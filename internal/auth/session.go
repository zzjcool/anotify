package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/anotify/anotify/internal/store"
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

// Create 为某用户签发一个新会话。
func (m *SessionManager) Create(userID string) (*store.Session, error) {
	tok, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s := &store.Session{
		ID:        store.NewSessionID() + "." + tok, // 加随机后缀，避免 ID 可枚举
		UserID:    userID,
		CreatedAt: now.Unix(),
		ExpiresAt: now.Add(m.ttl).Unix(),
		LastSeen:  now.Unix(),
	}
	if err := m.db.CreateSession(s); err != nil {
		return nil, err
	}
	return s, nil
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
			http.Error(w, `{"error":"未登录"}`, http.StatusUnauthorized)
			return
		}
		sess, err := m.Validate(c.Value)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
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
