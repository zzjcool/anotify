package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zzjcool/anotify/internal/store"
)

// 恢复码格式：XXXX-XXXX-XXXX-XXXX（16 字符，去歧义字符集），一批 8 个。
const (
	recoveryCodeCount    = 8
	recoveryCodeRawLen   = 16 // 不含连字符
	recoveryCodeGroupLen = 4
	recoveryAlphabet     = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 无 0/O/1/I
	recoveryPrefixLen    = 8                                  // 规范化明文前缀长度（定位候选）
)

// IssueRecoveryCodes 为用户签发一批恢复码。
// 返回明文 codes（仅此一次）；库内只存 argon2id 哈希。
// 不自动作废旧码——调用方（Regenerate）应先 DeleteUnused。
func (s *Service) IssueRecoveryCodes(userID string) ([]string, error) {
	if userID == "" {
		return nil, errors.New("auth: 缺少 userID")
	}
	plaintexts := make([]string, 0, recoveryCodeCount)
	now := store.Now()
	for i := 0; i < recoveryCodeCount; i++ {
		raw, err := randomRecoveryRaw()
		if err != nil {
			return nil, err
		}
		display := formatRecoveryCode(raw)
		norm := normalizeRecoveryCode(display)
		hash, err := hashKey(norm)
		if err != nil {
			return nil, err
		}
		prefix := norm
		if len(prefix) > recoveryPrefixLen {
			prefix = prefix[:recoveryPrefixLen]
		}
		rec := &store.RecoveryCode{
			ID:        store.NewRecoveryCodeID(),
			UserID:    userID,
			CodeHash:  hash,
			Prefix:    prefix,
			CreatedAt: now,
		}
		if err := s.db.CreateRecoveryCode(rec); err != nil {
			return nil, err
		}
		plaintexts = append(plaintexts, display)
	}
	return plaintexts, nil
}

// RegenerateRecoveryCodes 作废该用户全部未用旧码，再签发新一批。
func (s *Service) RegenerateRecoveryCodes(userID string) ([]string, error) {
	if _, err := s.db.GetUserByID(userID); err != nil {
		return nil, errors.New("auth: 用户不存在")
	}
	if _, err := s.db.DeleteUnusedRecoveryCodesByUser(userID); err != nil {
		return nil, err
	}
	codes, err := s.IssueRecoveryCodes(userID)
	if err != nil {
		return nil, err
	}
	slog.Info("recovery codes regenerated",
		"event", "auth.recovery.regenerated",
		"user_id", userID,
		"count", len(codes),
	)
	return codes, nil
}

// LoginWithRecoveryCode 用 username + 恢复码登录：校验哈希 → 作废该码 → 建会话。
// 返回会话 ID（调用方种 Cookie）。错误信息不区分「用户不存在 / 码错误」以防枚举。
func (s *Service) LoginWithRecoveryCode(username, code, deviceName string) (sessionID string, err error) {
	if username == "" || code == "" {
		return "", errors.New("auth: 缺少 username 或 code")
	}
	if err := ValidateUsername(username); err != nil {
		return "", err
	}
	user, err := s.db.GetUserByUsername(username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", errors.New("auth: 恢复码无效")
		}
		return "", fmt.Errorf("auth: 登录失败，请稍后重试: %w", err)
	}
	if user.Disabled {
		return "", errors.New("auth: 账户已被禁用")
	}
	norm := normalizeRecoveryCode(code)
	if len(norm) != recoveryCodeRawLen {
		return "", errors.New("auth: 恢复码无效")
	}
	prefix := norm
	if len(prefix) > recoveryPrefixLen {
		prefix = prefix[:recoveryPrefixLen]
	}
	cands, err := s.db.ListUnusedRecoveryCodesByPrefix(prefix)
	if err != nil {
		return "", err
	}
	var matched *store.RecoveryCode
	for _, c := range cands {
		if c.UserID != user.ID {
			continue
		}
		ok, verr := verifyKey(norm, c.CodeHash)
		if verr != nil || !ok {
			continue
		}
		matched = c
		break
	}
	if matched == nil {
		slog.Warn("recovery login failed",
			"event", "auth.recovery.login_fail",
			"username", username,
		)
		return "", errors.New("auth: 恢复码无效")
	}
	// 一次性：先作废再发会话（防并发双兑）。
	ok, err := s.db.MarkRecoveryCodeUsed(matched.ID, store.Now())
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("auth: 恢复码无效")
	}
	if deviceName == "" {
		deviceName = "Recovery · Unknown"
	}
	sess, err := s.sess.Create(user.ID, deviceName)
	if err != nil {
		return "", err
	}
	slog.Info("recovery login success",
		"event", "auth.recovery.login_success",
		"user_id", user.ID,
		"recovery_code_id", matched.ID,
	)
	return sess.ID, nil
}

// RecoveryStatus 返回未用恢复码数量（不含明文）。
func (s *Service) RecoveryStatus(userID string) (unused int, err error) {
	return s.db.CountUnusedRecoveryCodes(userID)
}

// randomRecoveryRaw 生成 recoveryCodeRawLen 个字母表字符（不含连字符）。
func randomRecoveryRaw() (string, error) {
	out := make([]byte, recoveryCodeRawLen)
	alphabet := []byte(recoveryAlphabet)
	// 拒绝采样，避免模偏差。
	max := 256 - (256 % len(alphabet))
	for i := 0; i < recoveryCodeRawLen; {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", errors.New("auth: 随机数生成失败")
		}
		if int(b[0]) >= max {
			continue
		}
		out[i] = alphabet[int(b[0])%len(alphabet)]
		i++
	}
	return string(out), nil
}

// formatRecoveryCode 把 16 字符 raw 格式化为 XXXX-XXXX-XXXX-XXXX。
func formatRecoveryCode(raw string) string {
	var parts []string
	for i := 0; i < len(raw); i += recoveryCodeGroupLen {
		end := i + recoveryCodeGroupLen
		if end > len(raw) {
			end = len(raw)
		}
		parts = append(parts, raw[i:end])
	}
	return strings.Join(parts, "-")
}

// normalizeRecoveryCode 去空白/连字符并大写，便于哈希与比对。
func normalizeRecoveryCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range code {
		if r == '-' || r == ' ' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
