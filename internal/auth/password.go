package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/zzjcool/anotify/internal/store"
)

// 密码登录（第二套登录模型，契约 §3）：与 Passkey 并存，不替代它。
//
// 不变量：**每个账号至少保留一种登录方式**（≥1 Passkey 或已设置密码）。
// 删除最后一个 Passkey 与移除密码这两条路径各自守一半，守卫在 store 写入之前
// ——宁可拒绝，也不让账号变成"谁也进不去"。

const (
	// PasswordMinLen / PasswordMaxLen 按 **rune** 计（中文密码 8 个汉字即合格）。
	PasswordMinLen = 8
	PasswordMaxLen = 128
)

var (
	// ErrPasswordNotSet：账号存在但没有设置密码（只能走 Passkey）。
	ErrPasswordNotSet = errors.New("auth: 该账户未设置密码")
	// ErrInvalidCredentials：登录失败的唯一对外说法——不区分「用户不存在 / 密码错」，避免枚举。
	ErrInvalidCredentials = errors.New("auth: 用户名或密码错误")
	// ErrLastAuthMethod：会抹掉最后一种登录方式的操作被拒绝。
	ErrLastAuthMethod = errors.New("auth: 至少保留一种登录方式：请先添加 Passkey 再移除密码")

	// errUserNotFound / errCurrentPasswordWrong 是密码管理端点直接展示给用户的文案
	// （前端把服务端 error 原样贴进弹层），所以措辞固定、不带包前缀。
	errUserNotFound         = errors.New("用户不存在")
	errCurrentPasswordWrong = errors.New("当前密码不正确")
)

// PasswordStatus 是「这个账号有没有密码」的对外视图（不含哈希）。
type PasswordStatus struct {
	HasPassword bool
	UpdatedAt   int64
}

// ValidatePassword 校验新密码是否合规。
//
// 顺序：先长度、后全空白——长度给的是「还差几个字」这类可操作反馈，
// 8 个空格先报"至少 8 个字符"没有意义（它已经够长，问题是内容）。
func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < PasswordMinLen {
		return fmt.Errorf("密码至少 %d 个字符", PasswordMinLen)
	}
	if n > PasswordMaxLen {
		return fmt.Errorf("密码最多 %d 个字符", PasswordMaxLen)
	}
	if strings.TrimSpace(pw) == "" {
		return errors.New("密码不能全为空白")
	}
	return nil
}

// dummyPasswordOnce / dummyPasswordHash 是「查无用户」路径用的惰性 argon2id 哈希。
//
// 为什么要它：找到用户 → 校验哈希（~31ms）；找不到用户 → 立刻返回，
// 两者耗时可差两个数量级，攻击者拿秒表就能枚举出哪些用户名存在。
// 所以失败路径也要跑一次同参数的 argon2id。哈希在进程内只算一次（sync.Once）。
var (
	dummyPasswordOnce sync.Once
	dummyPasswordHash string
)

// verifyDummyPassword 对所有明文跑一次 argon2id，只为消耗同样的时间，结果丢弃。
//
// 降级：dummy 哈希生成失败（熵源异常）时**不做校验也不 panic**——
// 登录本来就要失败了，这里再炸一次只会把 401 变成 500。
func verifyDummyPassword(password string) {
	dummyPasswordOnce.Do(func() {
		h, err := hashKey("anotify-dummy-password")
		if err != nil {
			slog.Warn("password: dummy hash init failed",
				"event", "auth.password.dummy_hash_fail",
				"error", err.Error(),
			)
			return
		}
		dummyPasswordHash = h
	})
	if dummyPasswordHash == "" {
		return
	}
	_, _ = verifyKey(password, dummyPasswordHash) // 结果不重要，耗时才是目的
}

// PasswordStatus 返回该用户的密码状态（不含哈希）。
func (s *Service) PasswordStatus(userID string) (*PasswordStatus, error) {
	rec, err := s.db.GetUserPassword(userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errUserNotFound
		}
		return nil, fmt.Errorf("auth: 读取密码状态失败: %w", err)
	}
	return &PasswordStatus{HasPassword: rec.Hash != "", UpdatedAt: rec.UpdatedAt}, nil
}

// SetPassword 设置或修改密码：设置（本来没有）忽略 currentPassword，
// 修改（已有密码）**必须**校验 currentPassword。
//
// 不吊销已有会话（本轮契约）：改密码 ≠ 踢人下线，这一点与"忘记密码"流程不同。
// 返回最新状态，调用方直接回给前端（省一次 GET）。
func (s *Service) SetPassword(userID, currentPassword, newPassword string) (*PasswordStatus, error) {
	rec, err := s.db.GetUserPassword(userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errUserNotFound
		}
		return nil, fmt.Errorf("auth: 读取密码状态失败: %w", err)
	}
	// 已有密码 = 这是「修改」，先验旧密码再验新密码：先验旧的能让"我只是打错旧密码"
	// 与"新密码不合规"分开报，用户知道该改哪一格。
	if rec.Hash != "" {
		ok, verr := verifyKey(currentPassword, rec.Hash)
		if verr != nil || !ok {
			return nil, errCurrentPasswordWrong
		}
	}
	if verr := ValidatePassword(newPassword); verr != nil {
		return nil, verr
	}
	hash, err := hashKey(newPassword)
	if err != nil {
		return nil, err
	}
	now := store.Now()
	if err := s.db.SetUserPassword(userID, hash, now); err != nil {
		return nil, err
	}
	// 只记"设置了"，绝不记明文/哈希（日志常年被复制到别处）。
	slog.Info("password set",
		"event", "auth.password.set",
		"user_id", userID,
		"created", rec.Hash == "",
	)
	return &PasswordStatus{HasPassword: true, UpdatedAt: now}, nil
}

// RemovePassword 移除密码。
//
// 守卫顺序：用户存在 → 已有密码（未设置即幂等成功）→ 旧密码正确 →
// **至少还有一个 Passkey**（否则账号就彻底进不去了）→ 清空。
func (s *Service) RemovePassword(userID, currentPassword string) error {
	rec, err := s.db.GetUserPassword(userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errUserNotFound
		}
		return fmt.Errorf("auth: 读取密码状态失败: %w", err)
	}
	if rec.Hash == "" {
		// 幂等：本来就没密码，移除"成功"（hasPassword=false）。
		return nil
	}
	ok, verr := verifyKey(currentPassword, rec.Hash)
	if verr != nil || !ok {
		return errCurrentPasswordWrong
	}
	passkeys, err := s.db.ListPasskeysByUser(userID)
	if err != nil {
		return fmt.Errorf("auth: 读取 Passkey 失败: %w", err)
	}
	if len(passkeys) == 0 {
		// 最后一种登录方式：拒绝。前端按 409 展示 ErrLastAuthMethod 的文案。
		return ErrLastAuthMethod
	}
	if err := s.db.ClearUserPassword(userID); err != nil {
		return err
	}
	slog.Info("password removed",
		"event", "auth.password.removed",
		"user_id", userID,
	)
	return nil
}

// LoginWithPassword 用 username + password 登录，返回会话 ID（调用方种 Cookie）。
//
// 错误纪律（防枚举 + 防计时侧信道）：
//   - 「用户不存在 / 查库失败 / 验证器异常 / 密码错」一律 ErrInvalidCredentials；
//   - 失败路径都跑过一次 argon2id（真哈希或 dummy 哈希），耗时与成功路径同量级；
//   - 「未设置密码」「账户已禁用」是**另一类**错误：它们只有拿到正确用户名才看得到，
//     且用户需要据此换一条路（Passkey / 找管理员），不给提示等于制造死胡同。
func (s *Service) LoginWithPassword(username, password, deviceName string) (sessionID string, err error) {
	if username == "" || password == "" {
		return "", ErrInvalidCredentials
	}
	if verr := ValidateUsername(username); verr != nil {
		// 用户名不合法的耗时与"查无用户"一致：同样跑一次 dummy 校验。
		verifyDummyPassword(password)
		return "", ErrInvalidCredentials
	}
	user, err := s.db.GetUserByUsername(username)
	if err != nil {
		// 查无用户 / 查库出错：抹平计时差后统一回凭据错误。
		// （查库出错也不泄露"库坏了"：那是运维信息，不是登录方的信息。）
		verifyDummyPassword(password)
		return "", ErrInvalidCredentials
	}
	if user.Disabled {
		return "", errors.New("auth: 账户已被禁用")
	}
	rec, err := s.db.GetUserPassword(user.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 用户行与密码列理论上同一个事务写；走到这里说明数据被外部改了，
			// 按"凭据无效"处理（fail-closed），耗时同样抹平。
			verifyDummyPassword(password)
			return "", ErrInvalidCredentials
		}
		verifyDummyPassword(password)
		return "", fmt.Errorf("auth: 登录失败，请稍后重试: %w", err)
	}
	if rec.Hash == "" {
		// 文案带"请使用 Passkey 登录"：这条错误是给真人看的，必须告诉他还剩哪条路。
		return "", fmt.Errorf("%w，请使用 Passkey 登录", ErrPasswordNotSet)
	}
	ok, verr := verifyKey(password, rec.Hash)
	if verr != nil || !ok {
		return "", ErrInvalidCredentials
	}
	sess, err := s.sess.Create(user.ID, deviceName)
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// RegisterWithPassword 用用户名 + 密码注册（不创建 Passkey）。
// 返回会话 ID 与一次性恢复码（明文仅此一次）。
//
// 这是**第二条注册方式**：Passkey ceremony 仍是主路径（register/options + register），
// 本条只服务「没有/不想用认证器」的用户。两条路径落库后的用户行必须长得一样
// （同样的默认命名空间、同样的内置能力、同样的首用户 admin 判定），差别只有
// 「有没有 Passkey 凭证」与「有没有密码列」。
//
// 原子性（契约 §A1）：用户行 + 默认命名空间 + 内置能力 + 密码哈希由
// store.RegisterUserWithPassword 在**同一个事务**里写入。任何一步失败都不留用户行
// ——不允许出现「有账号、但没有任何登录方式」的半成品。
//
// 错误文案纪律：用户名/密码不合规原样返回 ValidateUsername / ValidatePassword 的文案
// （前端把服务端 error 直接贴到表单下方）；重复用户名与 Passkey 路径的
// BeginRegister 同一句话（前端按文案分支，不能有两套说法）。
func (s *Service) RegisterWithPassword(
	ctx context.Context, username, displayName, password, deviceName string,
) (sessionID string, recoveryCodes []string, err error) {
	// 1) 规则校验：原样返回，不做包装（文案就是给用户看的）。
	if verr := ValidateUsername(username); verr != nil {
		return "", nil, verr
	}
	if verr := ValidatePassword(password); verr != nil {
		return "", nil, verr
	}
	// 2) 显示名缺省用用户名（与 WebAuthn 的 WebAuthnDisplayName 同口径）。
	if displayName == "" {
		displayName = username
	}
	// 3) 预检重复用户名。真正的唯一性由 users.username UNIQUE 保证——
	//    这一步只是为了在**最常见的顺序路径**上给出干净文案、不白跑一次 argon2。
	if _, gerr := s.db.GetUserByUsername(username); gerr == nil {
		return "", nil, errors.New("auth: 用户名已存在")
	} else if !errors.Is(gerr, store.ErrNotFound) {
		return "", nil, fmt.Errorf("auth: 查询用户名失败: %w", gerr)
	}
	// 4) 先生成哈希、再建用户：hashKey 失败（熵源异常）绝不能留下一个没有密码的账号
	//    （那正是 §A1 禁止的半成品）。密码明文只活在这个局部变量里，不落库、不落日志。
	hash, err := hashKey(password)
	if err != nil {
		return "", nil, err
	}
	u := &store.User{
		ID:          store.NewUserID(),
		Username:    username,
		DisplayName: displayName,
		CreatedAt:   store.Now(),
	}
	// 5) 首个注册用户 → admin 的判定交给 store，在它建用户的**同一个事务**里完成
	//    （store.RoleAuto → 事务内 SELECT COUNT(*)）。绝不在这里先查后写：
	//    两次独立池操作之间没有事务，并发首注册会双双拿到 admin（见 store.RoleAuto）。
	u.Role = store.RoleAuto
	if _, rerr := s.db.RegisterUserWithPassword(ctx, u, hash, store.Now()); rerr != nil {
		// 并发输家：预检时还没这行，INSERT 时撞了 users.username 的唯一约束。
		// 复查一次用户名把「数据库约束失败」统一翻成「用户名已存在」——
		// 既与 BeginRegister 文案一致，也不把 sqlite 的约束名漏给前端。
		if _, cerr := s.db.GetUserByUsername(username); cerr == nil {
			return "", nil, errors.New("auth: 用户名已存在")
		}
		slog.Error("register: register user with password failed",
			"event", "auth.register.namespace_failed",
			"user_id", u.ID,
			"error", rerr.Error(),
		)
		return "", nil, fmt.Errorf("auth: 注册失败（已回滚，未建用户）: %w", rerr)
	}
	// 首个用户升 admin 的日志：字段与 ensureRegisteredUser 的 auth.register.first_admin
	// 完全一致（两条注册路径的运维视图必须长得一样，否则「谁成了超管」要分两处查）。
	// 绝不记密码或哈希。
	if u.Role == store.RoleAdmin {
		slog.Info("first user promoted to admin",
			"event", "auth.register.first_admin",
			"user_id", u.ID,
			"username", u.Username,
		)
	}
	// 6) 恢复码：明文仅此一次返回。失败时用户已经建好（与 Passkey 注册的既有语义一致：
	//    账号可用，只是拿不到这批码，可稍后 regenerate）。
	codes, cerr := s.IssueRecoveryCodes(u.ID)
	if cerr != nil {
		return "", nil, fmt.Errorf("auth: 签发恢复码失败: %w", cerr)
	}
	// 7) 建会话（设备名语义同 Passkey 注册：调用方传 DeviceNameFromUA）。
	sess, serr := s.sess.Create(u.ID, deviceName)
	if serr != nil {
		return "", nil, serr
	}
	return sess.ID, codes, nil
}
