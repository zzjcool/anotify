// Package auth 提供 API Key 与 CLI 设备授权的签发、校验与状态机管理。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anotify/anotify/internal/store"
)

// CLI 授权会话相关常量。
const (
	cliAuthTTL       = 10 * time.Minute // 会话有效期
	userCodeLen      = 8                // 短码长度
	userCodeCharset  = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	secretRandomLen  = 32 // secret 原始字节数
	deviceNameMaxLen = 64 // 设备名最大长度
)

// ErrInvalidParam 参数校验错误（建会话/批准时输入不合法）。
var ErrInvalidParam = errors.New("auth: 参数无效")

// ErrUnauthorized secret 校验失败。
var ErrUnauthorized = errors.New("auth: 未授权")

// ErrAlreadyTerminal 会话已处于终态（无法迁移）。
var ErrAlreadyTerminal = errors.New("auth: 会话已处于终态")

// ErrAlreadyConsumed 会话已被消费（明文 Key 已领取过）。
var ErrAlreadyConsumed = errors.New("auth: 会话已被消费")

// CreatedCliAuthSession 是建会话后返回给调用方的信息。
type CreatedCliAuthSession struct {
	SessionID    string
	Secret       string // 明文 secret，仅此一次返回（调用方负责传递给脚本）
	UserCode     string // 归一化后的 8 字符大写码
	DeviceName   string
	Scopes       []string // 请求的 scopes
	ExpiresAt    int64
	PollInterval int
}

// CliAuthManager 管理 CLI 设备授权会话的完整生命周期。
type CliAuthManager struct {
	db      *store.DB
	ttl     time.Duration
	timeNow func() time.Time       // 可注入时钟，便于测试 TTL 边界
	codeGen func() (string, error) // 可注入短码生成器，便于测试重试
}

// NewCliAuthManager 构造 CliAuthManager。ttl≤0 时用默认 10 分钟。
func NewCliAuthManager(db *store.DB, ttl time.Duration) *CliAuthManager {
	if ttl <= 0 {
		ttl = cliAuthTTL
	}
	return &CliAuthManager{db: db, ttl: ttl, timeNow: time.Now, codeGen: generateUserCode}
}

// SetClock 注入时钟函数（测试用）。
func (m *CliAuthManager) SetClock(f func() time.Time) {
	m.timeNow = f
}

// SetCodeGenerator 注入短码生成器（测试用，用于模拟 UNIQUE 冲突重试）。
func (m *CliAuthManager) SetCodeGenerator(f func() (string, error)) {
	m.codeGen = f
}

// CreateSession 建立一个授权会话。
// deviceName 非空且 ≤64 字符；scopes 非空且全部合法。
// 返回的 Secret 仅此一次可见。
func (m *CliAuthManager) CreateSession(deviceName string, scopes []string) (*CreatedCliAuthSession, error) {
	deviceName = strings.TrimSpace(deviceName)
	if deviceName == "" || len(deviceName) > deviceNameMaxLen {
		return nil, fmt.Errorf("%w: deviceName 必须非空且不超过 %d 字符", ErrInvalidParam, deviceNameMaxLen)
	}
	if err := validateScopes(scopes); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidParam, err.Error())
	}

	now := m.timeNow()
	nowUnix := now.Unix()
	expiresAt := nowUnix + int64(m.ttl.Seconds())

	// 生成 secret（明文只返回，库中存 sha256 hex）。
	secret, secretHash, err := generateSecret()
	if err != nil {
		return nil, err
	}

	sessionID := store.NewCliAuthID()

	// 生成 userCode 并插入（遇 UNIQUE 冲突重试 ≤5 次，采用插入时捕获冲突而非先查后插，消除 TOCTOU）。
	var userCode string
	for attempt := 0; attempt < 5; attempt++ {
		code, err := m.codeGen()
		if err != nil {
			return nil, err
		}
		sess := &store.CliAuthSession{
			ID:              sessionID,
			SecretHash:      secretHash,
			UserCode:        code,
			DeviceName:      deviceName,
			ScopesRequested: scopes,
			Status:          store.CliAuthPending,
			CreatedAt:       nowUnix,
			ExpiresAt:       expiresAt,
		}
		err = m.db.CreateCliAuthSession(sess)
		if err == nil {
			userCode = code
			break
		}
		if errors.Is(err, store.ErrDuplicateUserCode) {
			continue // 短码冲突，重新生成
		}
		return nil, fmt.Errorf("auth: 建会话失败: %w", err)
	}
	if userCode == "" {
		return nil, errors.New("auth: userCode 生成冲突超过重试上限")
	}

	// 惰性清理过期会话（删除 1 小时前过期的记录）。
	_ = m.db.DeleteExpiredCliAuthSessions(nowUnix - 3600)

	return &CreatedCliAuthSession{
		SessionID:    sessionID,
		Secret:       secret,
		UserCode:     userCode,
		DeviceName:   deviceName,
		Scopes:       scopes,
		ExpiresAt:    expiresAt,
		PollInterval: 2,
	}, nil
}

// GetByID 按 sessionId 查会话。读取时执行惰性过期迁移。
func (m *CliAuthManager) GetByID(id string) (*store.CliAuthSession, error) {
	s, err := m.db.GetCliAuthSession(id)
	if err != nil {
		return nil, err
	}
	m.lazyExpire(s)
	return s, nil
}

// GetByCode 按短码查会话。code 归一化（去连字符、大写）。读取时执行惰性过期迁移。
func (m *CliAuthManager) GetByCode(code string) (*store.CliAuthSession, error) {
	normalized := NormalizeUserCode(code)
	s, err := m.db.GetCliAuthSessionByCode(normalized)
	if err != nil {
		return nil, err
	}
	m.lazyExpire(s)
	return s, nil
}

// Approve 批准授权会话。grantedScopes 必须 ⊆ requestedScopes 且非空。
// 原子迁移 pending→approved，0 行受影响返回 ErrAlreadyTerminal。
func (m *CliAuthManager) Approve(id, userID string, grantedScopes []string) error {
	s, err := m.db.GetCliAuthSession(id)
	if err != nil {
		return err
	}
	// 过期惰性迁移。
	if m.lazyExpire(s) {
		return fmt.Errorf("%w: 会话已过期", ErrAlreadyTerminal)
	}

	if s.Status != store.CliAuthPending {
		return fmt.Errorf("%w: 当前状态 %s 不可批准", ErrAlreadyTerminal, s.Status)
	}

	// 校验 grantedScopes 非空且 ⊆ requestedScopes。
	if len(grantedScopes) == 0 {
		return fmt.Errorf("%w: grantedScopes 不能为空", ErrInvalidParam)
	}
	requestedSet := make(map[string]bool, len(s.ScopesRequested))
	for _, sc := range s.ScopesRequested {
		requestedSet[sc] = true
	}
	for _, sc := range grantedScopes {
		if !validScope(sc) {
			return fmt.Errorf("%w: 未知 scope %q", ErrInvalidParam, sc)
		}
		if !requestedSet[sc] {
			return fmt.Errorf("%w: scope %q 未在申请范围内", ErrInvalidParam, sc)
		}
	}

	ok, err := m.db.UpdateCliAuthSessionApproved(id, userID, grantedScopes, m.timeNow().Unix())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: 会话已不在 pending 状态", ErrAlreadyTerminal)
	}
	return nil
}

// Deny 拒绝授权会话。原子迁移 pending→denied。
func (m *CliAuthManager) Deny(id string) error {
	s, err := m.db.GetCliAuthSession(id)
	if err != nil {
		return err
	}
	if m.lazyExpire(s) {
		return fmt.Errorf("%w: 会话已过期", ErrAlreadyTerminal)
	}
	if s.Status != store.CliAuthPending {
		return fmt.Errorf("%w: 当前状态 %s 不可拒绝", ErrAlreadyTerminal, s.Status)
	}

	ok, err := m.db.UpdateCliAuthSessionDenied(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: 会话已不在 pending 状态", ErrAlreadyTerminal)
	}
	return nil
}

// PollResult 是轮询结果。
type PollResult struct {
	Status  string   // pending/approved/consumed/denied/expired
	APIKey  string   // 仅 approved→consumed 时有值
	KeyID   string   // 仅 approved→consumed 时有值
	KeyName string   // 仅 approved→consumed 时有值
	Scopes  []string // granted scopes（approved/consumed 时有值）
}

// Poll 用 sessionId + secret 轮询会话状态。
// secret 校验失败返回 ErrUnauthorized 且不消费会话。
// approved 状态首次 poll 时事务内建 Key、返回明文（一次性），会话置 consumed。
func (m *CliAuthManager) Poll(id, secret string) (*PollResult, error) {
	s, err := m.db.GetCliAuthSession(id)
	if err != nil {
		return nil, err
	}

	// secret 校验（常量时间比对，先惰性过期也不影响校验顺序安全性）。
	if !verifySecret(secret, s.SecretHash) {
		return nil, ErrUnauthorized
	}

	// 惰性过期迁移。
	if m.lazyExpire(s) {
		return &PollResult{Status: store.CliAuthExpired}, nil
	}

	switch s.Status {
	case store.CliAuthPending:
		return &PollResult{Status: store.CliAuthPending}, nil
	case store.CliAuthDenied:
		return &PollResult{Status: store.CliAuthDenied}, nil
	case store.CliAuthExpired:
		return &PollResult{Status: store.CliAuthExpired}, nil
	case store.CliAuthConsumed:
		// 已消费，返回终态但不附带明文 Key。
		return &PollResult{Status: store.CliAuthConsumed, Scopes: s.ScopesGranted}, nil
	case store.CliAuthApproved:
		// kind guard（D-C-6）：apikey poll 端点只消费 apikey-kind 会话。
		// passkey-kind 会话必须走 /v1/passkey-enroll/ 端点族，不得在此签发 API Key。
		if s.Kind != store.CliAuthKindAPIKey {
			return nil, ErrInvalidParam
		}
		return m.consumeAndMintKey(s)
	default:
		return nil, fmt.Errorf("auth: 未知会话状态 %q", s.Status)
	}
}

// consumeAndMintKey 在事务内完成 approved→consumed + 建 Key。
func (m *CliAuthManager) consumeAndMintKey(s *store.CliAuthSession) (*PollResult, error) {
	// 构建 Key 记录（复用 hashKey/scopeLabel，同包可见）。
	raw := make([]byte, keyRandomLen)
	if _, err := rand.Read(raw); err != nil {
		return nil, errors.New("auth: 随机数生成失败")
	}
	label := scopeLabel(s.ScopesGranted)
	secret := base64.RawURLEncoding.EncodeToString(raw)
	plaintext := "ant_" + label + "_" + secret

	prefix := plaintext
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}

	hash, err := hashKey(plaintext)
	if err != nil {
		return nil, err
	}

	keyName := "agent:" + s.DeviceName
	keyID := store.NewEventID()
	nowUnix := m.timeNow().Unix()

	keyRec := &store.APIKey{
		ID:        keyID,
		UserID:    s.UserID,
		Name:      keyName,
		KeyHash:   hash,
		Prefix:    prefix,
		Scopes:    s.ScopesGranted,
		Enabled:   true,
		CreatedAt: nowUnix,
	}

	alreadyConsumed, err := m.db.ConsumeCliAuthSessionAndCreateKey(s.ID, keyRec, nowUnix)
	if err != nil {
		return nil, err
	}
	if alreadyConsumed {
		// 被并发消费，重新读取返回 consumed。
		return &PollResult{Status: store.CliAuthConsumed, Scopes: s.ScopesGranted}, nil
	}

	return &PollResult{
		Status:  store.CliAuthApproved, // approved→consumed 的成功响应，status 用 approved 表示「已领证成功」
		APIKey:  plaintext,
		KeyID:   keyID,
		KeyName: keyName,
		Scopes:  s.ScopesGranted,
	}, nil
}

// lazyExpire 检查会话是否已过期，若是则惰性迁移到 expired 并返回 true。
// 已是终态（consumed/denied/expired）的不动。
func (m *CliAuthManager) lazyExpire(s *store.CliAuthSession) bool {
	if s.Status != store.CliAuthPending && s.Status != store.CliAuthRequested && s.Status != store.CliAuthApproved {
		return false
	}
	if m.timeNow().Unix() <= s.ExpiresAt {
		return false
	}
	_ = m.db.MarkCliAuthSessionExpired(s.ID)
	s.Status = store.CliAuthExpired
	return true
}

// generateSecret 生成 32B 随机 secret，返回明文与 sha256 hex。
func generateSecret() (plaintext, hashHex string, err error) {
	raw := make([]byte, secretRandomLen)
	if _, err := rand.Read(raw); err != nil {
		return "", "", errors.New("auth: secret 随机数生成失败")
	}
	plaintext = base64.RawURLEncoding.EncodeToString(raw)
	h := sha256.Sum256([]byte(plaintext))
	hashHex = hex.EncodeToString(h[:])
	return plaintext, hashHex, nil
}

// generateUserCode 生成一个 8 字符短码（去歧义字符集）。
func generateUserCode() (string, error) {
	raw := make([]byte, userCodeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("auth: userCode 随机数生成失败")
	}
	out := make([]byte, userCodeLen)
	for i, b := range raw {
		out[i] = userCodeCharset[int(b)%len(userCodeCharset)]
	}
	return string(out), nil
}

// verifySecret 常量时间校验 secret（sha256 后比对 hex）。
func verifySecret(plaintext, storedHashHex string) bool {
	if plaintext == "" || storedHashHex == "" {
		return false
	}
	h := sha256.Sum256([]byte(plaintext))
	got := hex.EncodeToString(h[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHashHex)) == 1
}

// NormalizeUserCode 归一化短码：去连字符/空格、大写。
func NormalizeUserCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	return code
}

// FormatUserCode 格式化短码为 XXXX-XXXX 显示形式。
func FormatUserCode(code string) string {
	normalized := NormalizeUserCode(code)
	if len(normalized) <= 4 {
		return normalized
	}
	return normalized[:4] + "-" + normalized[4:]
}
