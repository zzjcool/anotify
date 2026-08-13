// Package authn 定义 notify/ws 侧对 API Key 鉴权的最小依赖接口。
//
// T11（internal/auth）将提供真实实现；本包只定义接口与 scope 常量，
// 让 notify 上报与 WebSocket 接收在集成前可独立开发、测试时注入 stub。
// 集成期由协调者把 internal/auth 的实现接到这些接口上。
package authn

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// API Key scopes。
const (
	ScopeNotifySend    = "notify:send"
	ScopeNotifyReceive = "notify:receive"
	ScopeNotifyReply   = "notify:reply"
	ScopeDevicesRead   = "devices:read"
)

// ErrUnauthorized 表示 Key 缺失/无效。
var ErrUnauthorized = errors.New("unauthorized: invalid or missing API key")

// ErrForbidden 表示 Key 有效但 scope 不足。
var ErrForbidden = errors.New("forbidden: insufficient scope")

// KeyValidator 校验一个明文 API Key，返回其所属用户与 scope 集合。
type KeyValidator interface {
	ValidateKey(ctx context.Context, key string) (userID string, scopes []string, err error)
}

// KeyValidatorFunc 让普通函数适配 KeyValidator。
type KeyValidatorFunc func(ctx context.Context, key string) (string, []string, error)

// ValidateKey 实现 KeyValidator。
func (f KeyValidatorFunc) ValidateKey(ctx context.Context, key string) (string, []string, error) {
	return f(ctx, key)
}

// HasScope 判断 scopes 是否包含目标 scope。
func HasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// BearerToken 从请求头提取 Bearer token；优先 Authorization 头，
// 其次回退到 ?access_token= 查询参数（供受限的 WS 客户端使用）。
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h != "" {
		if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(tok)
		}
		if tok, ok := strings.CutPrefix(h, "bearer "); ok {
			return strings.TrimSpace(tok)
		}
		return strings.TrimSpace(h)
	}
	return strings.TrimSpace(r.URL.Query().Get("access_token"))
}

// Authenticate 提取并校验 Bearer Key，要求具备 wantScope。
// 成功返回 userID；失败返回 ErrUnauthorized 或 ErrForbidden。
func Authenticate(r *http.Request, v KeyValidator, wantScope string) (string, error) {
	tok := BearerToken(r)
	if tok == "" {
		return "", ErrUnauthorized
	}
	userID, scopes, err := v.ValidateKey(r.Context(), tok)
	if err != nil {
		return "", ErrUnauthorized
	}
	if wantScope != "" && !HasScope(scopes, wantScope) {
		return "", ErrForbidden
	}
	return userID, nil
}
