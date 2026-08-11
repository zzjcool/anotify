package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/zzjcool/anotify/internal/store"
)

// argon2id 参数（按任务卡要求）。
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// 支持的 scope。
const (
	ScopeNotifySend    = "notify:send"
	ScopeNotifyReceive = "notify:receive"
	ScopeDevicesRead   = "devices:read"
)

// keyRandomLen 是 Key 随机部分的原始字节数。
const keyRandomLen = 24

// KeyManager 管理 API Key 的签发与校验。
type KeyManager struct {
	db *store.DB
}

// NewKeyManager 构造 Key 管理器。
func NewKeyManager(db *store.DB) *KeyManager {
	return &KeyManager{db: db}
}

// scopeLabel 从 scopes 推断 Key 的用途标签（用于生成 ant_<label>_ 前缀）。
func scopeLabel(scopes []string) string {
	has := func(s string) bool {
		for _, x := range scopes {
			if x == s {
				return true
			}
		}
		return false
	}
	send, recv := has(ScopeNotifySend), has(ScopeNotifyReceive)
	switch {
	case send && recv:
		return "full"
	case send:
		return "send"
	case recv:
		return "recv"
	default:
		return "key"
	}
}

// validScope 判断是否为已知合法 scope。
func validScope(s string) bool {
	switch s {
	case ScopeNotifySend, ScopeNotifyReceive, ScopeDevicesRead:
		return true
	default:
		return false
	}
}

// validateScopes 校验 scope 列表非空且全部合法，返回错误描述具体问题。
func validateScopes(scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("auth: 至少需要一个 scope")
	}
	for _, s := range scopes {
		if !validScope(s) {
			return fmt.Errorf("auth: 未知 scope %q", s)
		}
	}
	return nil
}

// CreateKey 签发一个 API Key。
// 返回完整明文 Key（仅此一次，调用方负责一次性展示）与持久化记录（不含明文）。
func (m *KeyManager) CreateKey(userID, name string, scopes []string) (plaintext string, record *store.APIKey, err error) {
	if err := validateScopes(scopes); err != nil {
		return "", nil, err
	}
	raw := make([]byte, keyRandomLen)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, errors.New("auth: 随机数生成失败")
	}
	label := scopeLabel(scopes)
	secret := base64.RawURLEncoding.EncodeToString(raw)
	plaintext = "ant_" + label + "_" + secret
	// 前缀用于识别与定位（取前 12 个字符，含 ant_label_ 开头部分）。
	prefix := plaintext
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	hash, err := hashKey(plaintext)
	if err != nil {
		return "", nil, err
	}
	rec := &store.APIKey{
		ID:        store.NewEventID(), // 复用 ID 生成器（唯一即可）
		UserID:    userID,
		Name:      name,
		KeyHash:   hash,
		Prefix:    prefix,
		Scopes:    scopes,
		Enabled:   true,
		CreatedAt: store.Now(),
	}
	if err := m.db.CreateAPIKey(rec); err != nil {
		return "", nil, err
	}
	return plaintext, rec, nil
}

// ValidateKey 校验一个明文 Key，返回所属 userID 与 scopes。
// 采用前缀定位 + argon2id 常量时间比对。校验失败统一返回错误（不区分原因以防枚举）。
func (m *KeyManager) ValidateKey(plaintext string) (userID string, scopes []string, err error) {
	if !strings.HasPrefix(plaintext, "ant_") {
		return "", nil, errors.New("auth: key 无效")
	}
	prefix := plaintext
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	rec, err := m.db.GetAPIKeyByPrefix(prefix)
	if err != nil {
		return "", nil, errors.New("auth: key 无效")
	}
	if !rec.Enabled {
		return "", nil, errors.New("auth: key 已停用")
	}
	ok, err := verifyKey(plaintext, rec.KeyHash)
	if err != nil || !ok {
		return "", nil, errors.New("auth: key 无效")
	}
	// 刷新最近使用时间（忽略错误）。
	_ = m.db.TouchAPIKey(rec.ID, store.Now())
	return rec.UserID, rec.Scopes, nil
}

// HasScope 判断 scopes 是否包含目标 scope。
func HasScope(scopes []string, target string) bool {
	for _, s := range scopes {
		if s == target {
			return true
		}
	}
	return false
}

// RequireScope 返回一个中间件：从 Authorization: Bearer 取 Key 校验，
// scope 不足返回 403，校验通过把 userID 注入 request context。
func (m *KeyManager) RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := bearerToken(r)
			if key == "" {
				http.Error(w, `{"error":"缺少 API Key"}`, http.StatusUnauthorized)
				return
			}
			userID, scopes, err := m.ValidateKey(key)
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
				return
			}
			if !HasScope(scopes, scope) {
				http.Error(w, `{"error":"权限不足（需要 `+scope+`）"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(withUserID(r.Context(), userID)))
		})
	}
}

// bearerToken 从 Authorization 头提取 Bearer token。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// hashKey 用 argon2id 计算 Key 的哈希（编码为标准 PHC 字符串）。
func hashKey(key string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("auth: 盐生成失败")
	}
	h := argon2.IDKey([]byte(key), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	// PHC 格式：$argon2id$v=19$m=...,t=...,p=...$salt$hash
	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(h),
	)
	return encoded, nil
}

// verifyKey 校验明文 Key 与 PHC 格式哈希是否匹配（常量时间比对）。
func verifyKey(key, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("auth: 哈希格式错误")
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, fmt.Errorf("auth: 解析哈希参数失败: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("auth: 解析盐失败: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("auth: 解析哈希失败: %w", err)
	}
	got := argon2.IDKey([]byte(key), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
