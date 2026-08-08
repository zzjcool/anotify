// Package auth 提供 Anotify 的认证能力：
// Passkey(WebAuthn) 注册/登录、会话管理、API Key 签发与校验（argon2id + scope）。
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/anotify/anotify/internal/store"
)

// challengeTTL 是 WebAuthn challenge 的有效期。
const challengeTTL = 5 * time.Minute

// webAuthnUser 适配 store.User + 其凭证到 webauthn.User 接口。
type webAuthnUser struct {
	user        *store.User
	credentials []webauthn.Credential
}

func (u *webAuthnUser) WebAuthnID() []byte   { return []byte(u.user.ID) }
func (u *webAuthnUser) WebAuthnName() string { return u.user.Username }
func (u *webAuthnUser) WebAuthnDisplayName() string {
	if u.user.DisplayName != "" {
		return u.user.DisplayName
	}
	return u.user.Username
}
func (u *webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// sessionEntry 暂存一次注册/登录的 challenge（内存实现，单进程够用）。
type sessionEntry struct {
	data      webauthn.SessionData
	expiresAt time.Time
}

// Service 是认证服务，聚合 WebAuthn、会话、API Key。
type Service struct {
	wa   *webauthn.WebAuthn
	db   *store.DB
	sess *SessionManager
	keys *KeyManager

	mu         sync.Mutex
	challenges map[string]*sessionEntry // key: challenge 标识（注册用 username，登录用 username 或 discoverable token）
}

// Config 是 Relying Party 配置。
type Config struct {
	RPDisplayName string   // 如 "Anotify"
	RPID          string   // 如 "localhost" 或 "api.anotify.dev"
	RPOrigins     []string // 如 ["http://localhost:5699"]
	SessionTTL    time.Duration
	SecureCookie  bool
}

// NewService 构造认证服务。
func NewService(db *store.DB, cfg Config) (*Service, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName: cfg.RPDisplayName,
		RPID:          cfg.RPID,
		RPOrigins:     cfg.RPOrigins,
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn new: %w", err)
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	s := &Service{
		wa:         wa,
		db:         db,
		sess:       NewSessionManager(db, cfg.SessionTTL, cfg.SecureCookie),
		keys:       NewKeyManager(db),
		challenges: make(map[string]*sessionEntry),
	}
	return s, nil
}

// Sessions 暴露会话管理器。
func (s *Service) Sessions() *SessionManager { return s.sess }

// Keys 暴露 API Key 管理器。
func (s *Service) Keys() *KeyManager { return s.keys }

// GetUser 按 ID 取用户（/v1/auth/me 用）。
func (s *Service) GetUser(id string) (*store.User, error) { return s.db.GetUserByID(id) }

// storeChallenge 暂存 challenge。
func (s *Service) storeChallenge(key string, data webauthn.SessionData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.challenges[key] = &sessionEntry{data: data, expiresAt: time.Now().Add(challengeTTL)}
}

// takeChallenge 取出并删除 challenge（一次性）。
func (s *Service) takeChallenge(key string) (webauthn.SessionData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.challenges[key]
	if !ok {
		return webauthn.SessionData{}, errors.New("auth: challenge 不存在或已使用")
	}
	delete(s.challenges, key)
	if time.Now().After(e.expiresAt) {
		return webauthn.SessionData{}, errors.New("auth: challenge 已过期")
	}
	return e.data, nil
}

// loadWebAuthnUser 加载用户及其所有凭证，组装成 webauthn.User。
func (s *Service) loadWebAuthnUser(u *store.User) (*webAuthnUser, error) {
	pks, err := s.db.ListPasskeysByUser(u.ID)
	if err != nil {
		return nil, fmt.Errorf("load passkeys: %w", err)
	}
	creds := make([]webauthn.Credential, 0, len(pks))
	for _, p := range pks {
		cred, err := credentialFromStore(p)
		if err != nil {
			return nil, err
		}
		creds = append(creds, cred)
	}
	return &webAuthnUser{user: u, credentials: creds}, nil
}

// credentialFromStore 把 store.Passkey 还原成 webauthn.Credential。
// 我们只持久化 PublicKey / SignCount / Transports，其余按 webauthn 库要求最小填充。
func credentialFromStore(p *store.Passkey) (webauthn.Credential, error) {
	idBytes, err := decodeCredID(p.ID)
	if err != nil {
		return webauthn.Credential{}, fmt.Errorf("decode credential id: %w", err)
	}
	transports := make([]protocol.AuthenticatorTransport, 0, len(p.Transports))
	for _, t := range p.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(t))
	}
	return webauthn.Credential{
		ID:        idBytes,
		PublicKey: p.PublicKey,
		Transport: transports,
		Flags:     webauthn.CredentialFlags{BackupEligible: p.BackupEligible},
		Authenticator: webauthn.Authenticator{
			SignCount: uint32(p.SignCount),
		},
	}, nil
}

// ---------- 注册 ----------

// BeginRegister 开始 Passkey 注册，返回 creation options（直接 JSON 给前端）。
// 若用户名已存在则报错（注册新用户）。
func (s *Service) BeginRegister(username, displayName string) (*protocol.CredentialCreation, error) {
	if _, err := s.db.GetUserByUsername(username); err == nil {
		return nil, errors.New("auth: 用户名已存在")
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	// 用一个临时 user（尚未入库）生成 options；WebAuthnID 用即将创建的用户 ID。
	tmp := &store.User{ID: store.NewUserID(), Username: username, DisplayName: displayName}
	waUser := &webAuthnUser{user: tmp, credentials: nil}
	// 强制 resident key（可发现凭据）：免用户名登录（discoverable login）依赖它——
	// 认证器里存有 resident 凭据，登录时才能不输入用户名直接选择。
	creation, session, err := s.wa.BeginRegistration(waUser, webauthn.WithAuthenticatorSelection(
		protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		},
	))
	if err != nil {
		return nil, fmt.Errorf("begin registration: %w", err)
	}
	// 暂存 challenge，key 用 username；tmp user id 也存进 session 的 UserID（库已做）。
	s.storeChallenge(regKey(username), *session)
	return creation, nil
}

// FinishRegister 完成注册：校验 attestation → 建 user + passkey + 会话。
// 返回新会话 ID（调用方负责种 Cookie）。
func (s *Service) FinishRegister(username string, r *http.Request) (sessionID string, err error) {
	session, err := s.takeChallenge(regKey(username))
	if err != nil {
		return "", err
	}
	// 重建与 BeginRegister 一致的临时 user（ID 取自 session.UserID）。
	tmp := &store.User{ID: string(session.UserID), Username: username}
	waUser := &webAuthnUser{user: tmp, credentials: nil}
	cred, err := s.wa.FinishRegistration(waUser, session, r)
	if err != nil {
		return "", fmt.Errorf("finish registration: %w", err)
	}
	// 建用户（若并发下已被创建则复用）。
	user, err := s.db.GetUserByID(tmp.ID)
	if errors.Is(err, store.ErrNotFound) {
		user = tmp
		user.CreatedAt = store.Now()
		// 首个注册的用户自动成为超级管理员：建用户前查 count，为 0 则 role=admin。
		// 注意：SQLite 单写（SetMaxOpenConns(1)）使该查询与随后的 INSERT 串行化，
		// 不会出现两个并发注册都看到 count=0 的情况。
		if n, cerr := s.db.UserCount(r.Context()); cerr == nil && n == 0 {
			user.Role = store.RoleAdmin
		} else if cerr != nil {
			return "", fmt.Errorf("auth: 首用户判定失败: %w", cerr)
		}
		if err := s.db.CreateUser(user); err != nil {
			return "", err
		}
		if user.Role == store.RoleAdmin {
			slog.Info("first user promoted to admin",
				"event", "auth.register.first_admin",
				"user_id", user.ID,
				"username", user.Username,
			)
		}
	} else if err != nil {
		return "", err
	}
	// 存凭证。
	if err := s.saveCredential(user.ID, cred, "新设备"); err != nil {
		return "", err
	}
	// 建会话。
	sess, err := s.sess.Create(user.ID, DeviceNameFromUA(r.UserAgent()))
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// ---------- 已登录用户补建凭证 ----------

// addCredKey 是「已登录用户补建凭证」的 challenge 暂存 key 命名空间，
// 与 reg:/login:/disc: 隔离，避免与注册新用户流程混淆。
func addCredKey(userID string) string { return "addcred:" + userID }

// BeginAddCredential 已登录用户补建一个 Passkey（不创建新用户，不新建会话）。
// userID 取自会话上下文。返回 creation options（直接 JSON 给前端）。
func (s *Service) BeginAddCredential(userID string) (*protocol.CredentialCreation, error) {
	user, err := s.db.GetUserByID(userID)
	if err != nil {
		return nil, fmt.Errorf("auth: 用户不存在: %w", err)
	}
	waUser, err := s.loadWebAuthnUser(user)
	if err != nil {
		return nil, err
	}
	creation, session, err := s.wa.BeginRegistration(waUser, webauthn.WithAuthenticatorSelection(
		protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		},
	))
	if err != nil {
		return nil, fmt.Errorf("begin add credential: %w", err)
	}
	s.storeChallenge(addCredKey(userID), *session)
	return creation, nil
}

// FinishAddCredential 完成补建凭证：校验 attestation → 存凭证。不建会话（已登录）。
func (s *Service) FinishAddCredential(userID, name string, r *http.Request) error {
	session, err := s.takeChallenge(addCredKey(userID))
	if err != nil {
		return err
	}
	user, err := s.db.GetUserByID(userID)
	if err != nil {
		return fmt.Errorf("auth: 用户不存在: %w", err)
	}
	waUser, err := s.loadWebAuthnUser(user)
	if err != nil {
		return err
	}
	cred, err := s.wa.FinishRegistration(waUser, session, r)
	if err != nil {
		return fmt.Errorf("finish add credential: %w", err)
	}
	if name == "" {
		name = "新设备"
	}
	return s.saveCredential(userID, cred, name)
}

// ---------- 登录 ----------

// BeginLogin 开始 Passkey 登录（指定用户名）。返回 assertion options。
func (s *Service) BeginLogin(username string) (*protocol.CredentialAssertion, error) {
	user, err := s.db.GetUserByUsername(username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errors.New("auth: 用户不存在")
		}
		return nil, fmt.Errorf("auth: 登录失败，请稍后重试: %w", err)
	}
	if user.Disabled {
		return nil, errors.New("auth: 账户已被禁用")
	}
	waUser, err := s.loadWebAuthnUser(user)
	if err != nil {
		return nil, err
	}
	assertion, session, err := s.wa.BeginLogin(waUser)
	if err != nil {
		return nil, fmt.Errorf("begin login: %w", err)
	}
	s.storeChallenge(loginKey(username), *session)
	return assertion, nil
}

// FinishLogin 完成指定用户名的登录：校验 assertion → 建会话。
func (s *Service) FinishLogin(username string, r *http.Request) (sessionID string, err error) {
	user, err := s.db.GetUserByUsername(username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", errors.New("auth: 用户不存在")
		}
		return "", fmt.Errorf("auth: 登录失败，请稍后重试: %w", err)
	}
	session, err := s.takeChallenge(loginKey(username))
	if err != nil {
		return "", err
	}
	waUser, err := s.loadWebAuthnUser(user)
	if err != nil {
		return "", err
	}
	cred, err := s.wa.FinishLogin(waUser, session, r)
	if err != nil {
		return "", fmt.Errorf("finish login: %w", err)
	}
	// 更新签名计数（防重放）。
	_ = s.db.UpdatePasskeySignCount(encodeCredID(cred.ID), int64(cred.Authenticator.SignCount), store.Now())
	sess, err := s.sess.Create(user.ID, DeviceNameFromUA(r.UserAgent()))
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// BeginDiscoverableLogin 开始可发现（conditional / passkey）登录，无需用户名。
func (s *Service) BeginDiscoverableLogin(token string) (*protocol.CredentialAssertion, error) {
	assertion, session, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		return nil, fmt.Errorf("begin discoverable login: %w", err)
	}
	s.storeChallenge(discKey(token), *session)
	return assertion, nil
}

// lookupUserByHandle 是 FinishDiscoverableLogin 的 userHandle 定位逻辑，
// 抽出为独立方法以便单测（构造真实 WebAuthn assertion 成本高）。
// userHandle 失配（=认证器里残留过期/孤儿 Passkey）时打诊断日志并返回中性文案。
func (s *Service) lookupUserByHandle(userHandle []byte) (webauthn.User, error) {
	user, err := s.db.GetUserByID(string(userHandle))
	if err != nil {
		slog.Warn("discoverable login: stale user handle (orphan passkey)",
			"event", "auth.login.stale_user_handle",
			"user_handle_prefix", userHandlePrefix(userHandle),
		)
		return nil, errors.New("auth: 该 Passkey 未关联到任何账户，可能已失效，请尝试其他 Passkey 或重新注册")
	}
	return s.loadWebAuthnUser(user)
}

// FinishDiscoverableLogin 完成可发现登录：按 userHandle 定位用户并校验。
func (s *Service) FinishDiscoverableLogin(token string, r *http.Request) (sessionID string, err error) {
	session, err := s.takeChallenge(discKey(token))
	if err != nil {
		return "", err
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		return s.lookupUserByHandle(userHandle)
	}
	waUser, cred, err := s.wa.FinishPasskeyLogin(handler, session, r)
	if err != nil {
		return "", fmt.Errorf("finish discoverable login: %w", err)
	}
	_ = s.db.UpdatePasskeySignCount(encodeCredID(cred.ID), int64(cred.Authenticator.SignCount), store.Now())
	sess, err := s.sess.Create(string(waUser.WebAuthnID()), DeviceNameFromUA(r.UserAgent()))
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// ---------- Passkey 设备授权补建凭证（匿名） ----------

// enrollCredKey 是「Passkey 设备授权补建凭证」的 challenge 暂存 key 命名空间，
// 以 sessionId 为维度（不撞 addcred:<userID>），支持同一 user 并发 enroll。
func enrollCredKey(sessionID string) string { return "enroll:" + sessionID }

// BeginEnrollCredential 为已批准的 passkey-enroll 会话生成 WebAuthn creation options。
// target userID 取自 session.user_id（approve 时回填）。challenge key 绑 sessionId。
// 调用方负责校验 session 状态为 approved 且 kind=passkey。
func (s *Service) BeginEnrollCredential(sessionID, userID string) (*protocol.CredentialCreation, error) {
	user, err := s.db.GetUserByID(userID)
	if err != nil {
		return nil, fmt.Errorf("auth: 用户不存在: %w", err)
	}
	waUser, err := s.loadWebAuthnUser(user)
	if err != nil {
		return nil, err
	}
	creation, session, err := s.wa.BeginRegistration(waUser, webauthn.WithAuthenticatorSelection(
		protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		},
	))
	if err != nil {
		return nil, fmt.Errorf("begin enroll credential: %w", err)
	}
	s.storeChallenge(enrollCredKey(sessionID), *session)
	return creation, nil
}

// FinishEnrollCredential 完成 Passkey 设备授权补建凭证：校验 attestation → 存凭证。
// challenge 从 enrollCredKey(sessionID) 取（一次性）。不建会话（匿名流程）。
// 调用方负责校验 registrationToken 并执行原子 consume。
func (s *Service) FinishEnrollCredential(sessionID, userID, name string, r *http.Request) error {
	session, err := s.takeChallenge(enrollCredKey(sessionID))
	if err != nil {
		return err
	}
	user, err := s.db.GetUserByID(userID)
	if err != nil {
		return fmt.Errorf("auth: 用户不存在: %w", err)
	}
	waUser, err := s.loadWebAuthnUser(user)
	if err != nil {
		return err
	}
	cred, err := s.wa.FinishRegistration(waUser, session, r)
	if err != nil {
		return fmt.Errorf("finish enroll credential: %w", err)
	}
	if name == "" {
		name = "新设备"
	}
	return s.saveCredential(userID, cred, name)
}

// ---------- Passkey 管理（已登录用户） ----------

// ListPasskeys 列出某用户的所有凭证。
func (s *Service) ListPasskeys(userID string) ([]*store.Passkey, error) {
	return s.db.ListPasskeysByUser(userID)
}

// GetPasskey 按 credential id 查凭证。
func (s *Service) GetPasskey(id string) (*store.Passkey, error) {
	return s.db.GetPasskeyByID(id)
}

// RenamePasskey 重命名凭证。
func (s *Service) RenamePasskey(id, name string) error {
	return s.db.RenamePasskey(id, name)
}

// DeletePasskey 删除凭证。
func (s *Service) DeletePasskey(id string) error {
	return s.db.DeletePasskey(id)
}

// saveCredential 持久化一个 webauthn.Credential。
func (s *Service) saveCredential(userID string, cred *webauthn.Credential, name string) error {
	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	p := &store.Passkey{
		ID:             encodeCredID(cred.ID),
		UserID:         userID,
		PublicKey:      cred.PublicKey,
		SignCount:      int64(cred.Authenticator.SignCount),
		Name:           name,
		Transports:     transports,
		BackupEligible: cred.Flags.BackupEligible,
		CreatedAt:      store.Now(),
	}
	if err := s.db.CreatePasskey(p); err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	return nil
}

// challenge 暂存 key 前缀，避免不同类型冲突。
func regKey(username string) string   { return "reg:" + username }
func loginKey(username string) string { return "login:" + username }
func discKey(token string) string     { return "disc:" + token }

// ---------- Passkey 设备授权 registrationToken 暂存 ----------
//
// registrationToken 是 complete 端点的凭证（替代登录 Cookie）。
// poll 在 approved 首次返回时生成，存储其 sha256 hash（明文不落库）。
// complete 时常量时间校验后删除（一次性）。

// storeEnrollToken 暂存 registrationToken 的 hash（key=sessionId）。
func (s *Service) storeEnrollToken(sessionID, tokenHash string) {
	s.storeChallenge("enrolltoken:"+sessionID, webauthn.SessionData{
		Challenge: tokenHash,
	})
}

// verifyEnrollToken 常量时间校验 registrationToken。校验后不删除（complete 成功时调 deleteEnrollToken）。
func (s *Service) verifyEnrollToken(sessionID, token string) bool {
	sess, err := s.takeChallenge("enrolltoken:" + sessionID)
	if err != nil {
		return false
	}
	// takeChallenge 已删除 entry，需重新存入供 complete 重试（registrationToken 在 complete 成功前可多次用）
	s.storeChallenge("enrolltoken:"+sessionID, sess)
	return subtle.ConstantTimeCompare([]byte(sess.Challenge), []byte(sha256Hex(token))) == 1
}

// deleteEnrollToken 删除 registrationToken（complete 成功后调用）。
func (s *Service) deleteEnrollToken(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.challenges, "enrolltoken:"+sessionID)
}

// sha256Hex 计算字符串的 sha256 hex 摘要。
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// userHandlePrefix 返回 userHandle 的 hex 前 8 字符，用于诊断日志。
// 仅含前缀（不含完整 handle、不含 username），避免日志泄露用户标识。
// 空输入返回空串。
func userHandlePrefix(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	full := hex.EncodeToString(b)
	if len(full) <= 8 {
		return full
	}
	return full[:8]
}
