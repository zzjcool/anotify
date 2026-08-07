// Package auth 提供 Anotify 的认证能力：
// Passkey(WebAuthn) 注册/登录、会话管理、API Key 签发与校验（argon2id + scope）。
package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anotify/anotify/internal/store"
)

// PasskeyEnrollManager 管理 Passkey 设备授权会话的完整生命周期。
// 复用 cli_auth_sessions 表（kind=passkey），但状态机和端点独立于 CliAuthManager。
// 状态流：pending → requested → approved → consumed（+denied/expired 旁路终态）。
type PasskeyEnrollManager struct {
	db      *store.DB
	svc     *Service // WebAuthn Service（Begin/FinishEnrollCredential）
	ttl     time.Duration
	timeNow func() time.Time
	codeGen func() (string, error)
}

// NewPasskeyEnrollManager 构造 PasskeyEnrollManager。ttl≤0 时用默认 10 分钟。
func NewPasskeyEnrollManager(db *store.DB, svc *Service, ttl time.Duration) *PasskeyEnrollManager {
	if ttl <= 0 {
		ttl = cliAuthTTL
	}
	return &PasskeyEnrollManager{
		db:      db,
		svc:     svc,
		ttl:     ttl,
		timeNow: time.Now,
		codeGen: generateUserCode,
	}
}

// SetClock 注入时钟函数（测试用）。
func (m *PasskeyEnrollManager) SetClock(f func() time.Time) {
	m.timeNow = f
}

// SetCodeGenerator 注入短码生成器（测试用）。
func (m *PasskeyEnrollManager) SetCodeGenerator(f func() (string, error)) {
	m.codeGen = f
}

// CreatedEnrollSession 是建会话后返回给调用方的信息。
type CreatedEnrollSession struct {
	SessionID    string
	Secret       string // 明文 secret，仅此一次返回
	UserCode     string // 归一化后的 8 字符大写码
	DeviceName   string
	ExpiresAt    int64
	PollInterval int
}

// CreateSession 建立一个 Passkey 设备授权会话（旧设备发起，需登录）。
// deviceName 非空且 ≤64 字符。不需要 scopes（Passkey 授权无 scope 概念）。
// 返回的 Secret 仅此一次可见，由调用方传递给新设备。
func (m *PasskeyEnrollManager) CreateSession(deviceName string) (*CreatedEnrollSession, error) {
	deviceName = strings.TrimSpace(deviceName)
	if deviceName == "" || len(deviceName) > deviceNameMaxLen {
		return nil, fmt.Errorf("%w: deviceName 必须非空且不超过 %d 字符", ErrInvalidParam, deviceNameMaxLen)
	}

	now := m.timeNow()
	nowUnix := now.Unix()
	expiresAt := nowUnix + int64(m.ttl.Seconds())

	// 生成 secret（明文只返回，库中存 sha256 hex）
	secret, secretHash, err := generateSecret()
	if err != nil {
		return nil, err
	}

	sessionID := store.NewCliAuthID()

	// 生成 userCode 并插入（遇 UNIQUE 冲突重试 ≤5 次）
	var userCode string
	for attempt := 0; attempt < 5; attempt++ {
		code, err := m.codeGen()
		if err != nil {
			return nil, err
		}
		sess := &store.CliAuthSession{
			ID:         sessionID,
			SecretHash: secretHash,
			UserCode:   code,
			DeviceName: deviceName,
			Kind:       store.CliAuthKindPasskey,
			Status:     store.CliAuthPending,
			CreatedAt:  nowUnix,
			ExpiresAt:  expiresAt,
		}
		err = m.db.CreateCliAuthSession(sess)
		if err == nil {
			userCode = code
			break
		}
		if errors.Is(err, store.ErrDuplicateUserCode) {
			continue
		}
		return nil, fmt.Errorf("auth: 建会话失败: %w", err)
	}
	if userCode == "" {
		return nil, errors.New("auth: userCode 生成冲突超过重试上限")
	}

	// 惰性清理过期会话
	_ = m.db.DeleteExpiredCliAuthSessions(nowUnix - 3600)

	return &CreatedEnrollSession{
		SessionID:    sessionID,
		Secret:       secret,
		UserCode:     userCode,
		DeviceName:   deviceName,
		ExpiresAt:    expiresAt,
		PollInterval: 2,
	}, nil
}

// GetByID 按 sessionId 查会话。读取时执行惰性过期迁移。
func (m *PasskeyEnrollManager) GetByID(id string) (*store.CliAuthSession, error) {
	s, err := m.db.GetCliAuthSession(id)
	if err != nil {
		return nil, err
	}
	m.lazyExpire(s)
	return s, nil
}

// GetByCode 按短码查会话。code 归一化（去连字符、大写）。读取时执行惰性过期迁移。
func (m *PasskeyEnrollManager) GetByCode(code string) (*store.CliAuthSession, error) {
	normalized := NormalizeUserCode(code)
	s, err := m.db.GetCliAuthSessionByCode(normalized)
	if err != nil {
		return nil, err
	}
	m.lazyExpire(s)
	return s, nil
}

// RequestKnock 新设备敲门：pending→requested，设置 deviceHint。
// 校验 kind=passkey。返回 secret（新设备轮询凭证，仅 pending 态可用）。
func (m *PasskeyEnrollManager) RequestKnock(sessionID, deviceHint string) (secret string, err error) {
	s, err := m.db.GetCliAuthSession(sessionID)
	if err != nil {
		return "", err
	}
	if m.lazyExpire(s) {
		return "", fmt.Errorf("%w: 会话已过期", ErrAlreadyTerminal)
	}
	if s.Kind != store.CliAuthKindPasskey {
		return "", fmt.Errorf("%w: 非 passkey 类型会话", ErrInvalidParam)
	}
	if s.Status != store.CliAuthPending {
		return "", fmt.Errorf("%w: 当前状态 %s 不可敲门", ErrAlreadyTerminal, s.Status)
	}

	// 重新生成 secret（建会话时的 secret 仅供旧设备持有，
	// 敲门时重新生成，使新设备获得独立的轮询凭证）
	secret, secretHash, err := generateSecret()
	if err != nil {
		return "", err
	}

	// 更新 secret_hash + 状态迁移 pending→requested
	ok, err := m.db.UpdateCliAuthSessionRequestedWithSecret(sessionID, deviceHint, secretHash)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: 会话已不在 pending 状态", ErrAlreadyTerminal)
	}
	return secret, nil
}

// Approve 旧设备批准：requested→approved，回填 user_id。
func (m *PasskeyEnrollManager) Approve(id, userID string) error {
	s, err := m.db.GetCliAuthSession(id)
	if err != nil {
		return err
	}
	if m.lazyExpire(s) {
		return fmt.Errorf("%w: 会话已过期", ErrAlreadyTerminal)
	}
	if s.Kind != store.CliAuthKindPasskey {
		return fmt.Errorf("%w: 非 passkey 类型会话", ErrInvalidParam)
	}
	if s.Status != store.CliAuthRequested {
		return fmt.Errorf("%w: 当前状态 %s 不可批准", ErrAlreadyTerminal, s.Status)
	}

	ok, err := m.db.UpdateCliAuthSessionApprovedFromRequested(id, userID, m.timeNow().Unix())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: 会话已不在 requested 状态", ErrAlreadyTerminal)
	}
	return nil
}

// Deny 旧设备拒绝：requested→denied。
func (m *PasskeyEnrollManager) Deny(id string) error {
	s, err := m.db.GetCliAuthSession(id)
	if err != nil {
		return err
	}
	if m.lazyExpire(s) {
		return fmt.Errorf("%w: 会话已过期", ErrAlreadyTerminal)
	}
	if s.Kind != store.CliAuthKindPasskey {
		return fmt.Errorf("%w: 非 passkey 类型会话", ErrInvalidParam)
	}
	if s.Status != store.CliAuthRequested {
		return fmt.Errorf("%w: 当前状态 %s 不可拒绝", ErrAlreadyTerminal, s.Status)
	}

	ok, err := m.db.UpdateCliAuthSessionDeniedFromRequested(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: 会话已不在 requested 状态", ErrAlreadyTerminal)
	}
	return nil
}

// EnrollPollResult 是新设备轮询结果。
type EnrollPollResult struct {
	Status             string // pending/requested/approved/consumed/denied/expired
	AttestationOptions any    // 仅 approved 首次返回（protocol.CredentialCreation 序列化后）
	RegistrationToken  string // 仅 approved 首次返回（一次性，用于 complete 端点）
	InitiatorName      string // 批准者显示名（approved 时有值）
}

// Poll 新设备轮询：secret 门控。
// approved 首次 poll 时生成 attestationOptions + registrationToken（一次性）。
func (m *PasskeyEnrollManager) Poll(id, secret string) (*EnrollPollResult, error) {
	s, err := m.db.GetCliAuthSession(id)
	if err != nil {
		return nil, err
	}

	// secret 校验（常量时间比对）
	if !verifySecret(secret, s.SecretHash) {
		return nil, ErrUnauthorized
	}

	if m.lazyExpire(s) {
		return &EnrollPollResult{Status: store.CliAuthExpired}, nil
	}

	switch s.Status {
	case store.CliAuthPending:
		return &EnrollPollResult{Status: store.CliAuthPending}, nil
	case store.CliAuthRequested:
		return &EnrollPollResult{Status: store.CliAuthRequested}, nil
	case store.CliAuthDenied:
		return &EnrollPollResult{Status: store.CliAuthDenied}, nil
	case store.CliAuthExpired:
		return &EnrollPollResult{Status: store.CliAuthExpired}, nil
	case store.CliAuthConsumed:
		return &EnrollPollResult{Status: store.CliAuthConsumed}, nil
	case store.CliAuthApproved:
		return m.generateAttestation(s)
	default:
		return nil, fmt.Errorf("auth: 未知会话状态 %q", s.Status)
	}
}

// generateAttestation 在 approved 态首次 poll 时：
// 生成 attestationOptions + registrationToken。**不 consume 会话**——consume 留给 Complete 做
// （approved→consumed + 建凭证，见 D-C-4 防重放）。二次 poll 重新生成 token（覆盖旧 token hash）。
// 会话状态保持 approved，直至 Complete 成功迁移到 consumed。
func (m *PasskeyEnrollManager) generateAttestation(s *store.CliAuthSession) (*EnrollPollResult, error) {
	// 生成 attestationOptions（WebAuthn creation options）
	creation, err := m.svc.BeginEnrollCredential(s.ID, s.UserID)
	if err != nil {
		return nil, err
	}

	// 生成 registrationToken（一次性，用于 complete 端点校验）
	regToken, regTokenHash, err := generateSecret()
	if err != nil {
		return nil, err
	}
	// 暂存 registrationToken hash（用 challenge key 空间存）
	m.svc.storeEnrollToken(s.ID, regTokenHash)

	// 获取批准者显示名
	initiatorName := ""
	if u, err := m.db.GetUserByID(s.UserID); err == nil {
		if u.DisplayName != "" {
			initiatorName = u.DisplayName
		} else {
			initiatorName = u.Username
		}
	}

	return &EnrollPollResult{
		Status:             store.CliAuthApproved,
		AttestationOptions: creation,
		RegistrationToken:  regToken,
		InitiatorName:      initiatorName,
	}, nil
}

// CompleteResult 是 complete 端点的结果。
type CompleteResult struct {
	Ok        bool
	PasskeyID string
}

// Complete 新设备提交 attestation 完成凭证创建。
// 校验：registrationToken 匹配 ∧ kind=passkey ∧ status=approved ∧ 未过期。
// 本方法执行原子 consume + 建凭证。
func (m *PasskeyEnrollManager) Complete(sessionID, registrationToken, name string, r *http.Request) (*CompleteResult, error) {
	s, err := m.db.GetCliAuthSession(sessionID)
	if err != nil {
		return nil, err
	}

	// 校验 registrationToken
	if !m.svc.verifyEnrollToken(sessionID, registrationToken) {
		return nil, ErrUnauthorized
	}

	if s.Kind != store.CliAuthKindPasskey {
		return nil, fmt.Errorf("%w: 非 passkey 类型会话", ErrInvalidParam)
	}

	if m.lazyExpire(s) {
		return nil, fmt.Errorf("%w: 会话已过期", ErrAlreadyTerminal)
	}

	// 已 consumed 的会话不允许 complete（registrationToken 一次性）
	if s.Status == store.CliAuthConsumed {
		return nil, ErrAlreadyConsumed
	}

	if s.Status != store.CliAuthApproved {
		return nil, fmt.Errorf("%w: 当前状态 %s 不可完成", ErrAlreadyTerminal, s.Status)
	}

	// 先完成 WebAuthn 凭证创建（challenge 从 enrollCredKey(sessionID) 取）。
	// challenge 是一次性的（takeChallenge 取走即删）：并发场景下第二个 complete 会因
	// challenge 缺失而失败，自然防重放。attestation 无效时返回错误，会话保持 approved 可重试。
	if err := m.svc.FinishEnrollCredential(sessionID, s.UserID, name, r); err != nil {
		return nil, err
	}

	// 凭证建立成功后，原子迁移 approved→consumed（防重放，D-C-4）。
	// 若 consume 失败（并发下已被抢用）凭证已建但会话状态不一致——可接受（凭证无害，会话终态）。
	_, err = m.db.ConsumeCliAuthSessionPasskey(s.ID, m.timeNow().Unix())
	if err != nil {
		return nil, err
	}

	// 清理 registrationToken
	m.svc.deleteEnrollToken(sessionID)

	return &CompleteResult{Ok: true}, nil
}

// Delete 旧设备取消会话（删除记录）。
func (m *PasskeyEnrollManager) Delete(id string) error {
	return m.db.DeleteCliAuthSession(id)
}

// lazyExpire 检查会话是否已过期，若是则惰性迁移到 expired 并返回 true。
func (m *PasskeyEnrollManager) lazyExpire(s *store.CliAuthSession) bool {
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

// verifySecret 常量时间校验 secret（sha256 后比对 hex）。
// 复用 cliauth.go 中的同名函数（包内可见）。
var _ = verifySecret
