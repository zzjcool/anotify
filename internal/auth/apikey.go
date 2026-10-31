package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
//
// 为什么要有缓存（issue #227）：ValidateKey 原始路径每请求一次 DB 前缀查询 + 一次
// argon2id(m=64MB,t=1,p=4) 重算 + 一次写事务，4 核上把 Bearer 路径压在 ~35 RPS、
// -c 8 时 RSS ≈1GB。已验证结论在 TTL 内不可能变（被 revoke 走 RevokeKey 会同步失效），
// 所以可以安全复用。
type KeyManager struct {
	db *store.DB
	// cacheTTL 是缓存条目存活时间；与 cache 一起由构造器决定是否启用缓存。
	cacheTTL time.Duration
	// cache 为 nil 表示缓存关闭（所有请求走完整校验）。用 nil 而不是空的 cache 对象，
	// 让「关缓存」在热路径上只是一次指针比较，不需要额外布尔字段，也不可能被误用。
	cache *keyCache
}

// NewKeyManager 构造 Key 管理器（默认 TTL，缓存开启）。
// 签名保持不变：调用点（webauthn.go / devseed）不需要改。
func NewKeyManager(db *store.DB) *KeyManager {
	return NewKeyManagerTuned(db, defaultKeyCacheTTL)
}

// NewKeyManagerTuned 构造一个可调缓存 TTL 的 Key 管理器。
//
// cacheTTL < 0 表示**关闭缓存**（所有请求走完整校验，含每请求写 last_used_at，
// 即完全保持旧行为）；cacheTTL == 0 视作「未指定」，回落到默认 TTL——这与 env
// 语义对齐（ANOTIFY_APIKEY_CACHE_TTL 的 0/空 = 用默认，-1/off = 关），
// 也让接线层漏做归一化时不会意外把性能优化关掉。
func NewKeyManagerTuned(db *store.DB, cacheTTL time.Duration) *KeyManager {
	if cacheTTL == 0 {
		cacheTTL = defaultKeyCacheTTL
	}
	m := &KeyManager{db: db, cacheTTL: cacheTTL}
	if cacheTTL > 0 {
		cache, err := newKeyCache()
		if err != nil {
			// 取不到 HMAC 密钥就**关缓存**，绝不退化到裸哈希：
			// 退化是静默的安全降级，关缓存只是少了优化。
			logCacheDisabled(err)
			m.cacheTTL = -1
		} else {
			m.cache = cache
		}
	}
	return m
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
//
// 判定顺序是安全边界，不允许调换：
//  1. 没有 ant_ 前缀 → 直接拒绝，且**不进缓存**（连 HMAC 都不算，避免拿垃圾输入打表）；
//  2. 缓存键 = HMAC(进程密钥, 前缀‖0x00‖明文) → 查表，命中且未过期、generation 匹配
//     → **再做一次常量时间比对**（verifier）通过才放行；
//  3. 未命中 → 原路径：前缀查记录 → !Enabled 拒绝 → verifyKey(argon2 常量时间比对)
//     → 成功才写缓存。
//
// **负结果一律不缓存**（查不到 / 校验失败 / 已停用）：否则攻击者拿随机 key 喷洒
// 就能把进程内存顶到上限，还顺手把热 key 挤出有效集合。
// 校验失败统一返回错误（不区分原因以防枚举）。
func (m *KeyManager) ValidateKey(plaintext string) (userID string, scopes []string, err error) {
	if !strings.HasPrefix(plaintext, "ant_") {
		return "", nil, errors.New("auth: key 无效")
	}
	now := time.Now()
	prefix := keyPrefixOf(plaintext)
	// digest 与 genAtRead 必须提到 cache 判断之外：未命中路径写缓存时要用它们。
	// **genAtRead 必须是读 DB 之前的快照**：如果读库之后才取 gen，则「读库 → revoke →
	// 写缓存」这个窗口里写进去的条目会带着 revoke 之前的状态活到 TTL，脏读窗口
	// 就重新出现了。
	var digest [32]byte
	var genAtRead uint64
	if m.cache != nil {
		digest = m.cache.digestOf(prefix, plaintext)
		genAtRead = m.cache.generation()
		if e := m.cache.lookup(digest, now); e != nil {
			// 命中不等于放行：必须再过一次定长常量时间比对。
			// 这一步把「map 里拿到一条记录」与「这条记录确实属于这次请求」分开验证，
			// 代价是一次 HMAC（~300ns），相对 argon2 的 31ms 可以忽略。
			got := m.cache.verifierOf(plaintext)
			if constTimeEqual(got[:], e.verifier[:]) {
				m.cache.hits.Add(1)
				m.touchThrottled(e, now)
				return e.userID, cloneScopes(e.scopes), nil
			}
			// 条目与请求不匹配（碰撞 / 写坏 / 竞态）：丢弃并按未命中处理，
			// 绝不能「反正 map 说命中就放行」。
			m.cache.forget(digest)
		}
		m.cache.misses.Add(1)
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
	// 刷新最近使用时间（忽略错误）。这里是完整校验路径，写一次是原行为。
	_ = m.db.TouchAPIKey(rec.ID, store.Now())
	if m.cache != nil {
		// gen 必须是**读 DB 之前**的快照（见函数开头 genAtRead），否则「读库之后、
		// 写缓存之前」发生的 revoke 会被这条条目盖过去，脏读窗口重新出现。
		e := &keyEntry{
			userID:    rec.UserID,
			scopes:    cloneScopes(rec.Scopes), // 缓存自己留一份副本
			keyID:     rec.ID,
			expiresAt: now.Add(m.cacheTTL),
			gen:       genAtRead,
			verifier:  m.cache.verifierOf(plaintext),
		}
		e.lastTouched.Store(now.UnixNano())
		m.cache.put(digest, e, now) // 容量满时内部放弃写入，不影响本次鉴权
	}
	// 未命中路径返回 DB 解码出的新切片（与缓存里的副本互不影响）。
	return rec.UserID, rec.Scopes, nil
}

// RevokeKey 停用一条 API Key，并在 DB 写成功之后**同步**清掉它的缓存条目。
//
// 顺序不能反：DB 失败却清了缓存，只是白丢一次性能；反之「清了缓存但 DB 没改」
// 会让缓存与 DB 分叉（当前进程里看起来失效、重启后又活了）。所以先写库，
// 成功才失效。失效做两件事：
//   - 按 keyID 定向删（把这条 key 的条目立刻拿掉）；
//   - bump 全局 generation（让**其它**可能读到旧状态的条目一起作废）。
//
// 第二件事是为了一个具体的坑：ValidateKey 是「先读 DB 后写缓存」，如果 revoke
// 恰好落在某个并发请求的两次操作之间，那条条目会带着 revoke 之前的状态活到 TTL 结束。
// generation 让这种条目在下一��校验时立刻变成未命中（见 ValidateKey 的 genAtRead）。
func (m *KeyManager) RevokeKey(id string) error {
	if err := m.db.RevokeAPIKey(id); err != nil {
		return err
	}
	if m.cache != nil {
		m.cache.bumpGeneration()
		m.cache.invalidateKeyID(id)
	}
	return nil
}

// BumpGeneration 让整表缓存立刻失效。
//
// 给「不经过 RevokeKey 的写路径」用：改 scopes、删 key、手工改库后想立刻生效，
// 调它比等 TTL 干净。它不碰 DB，只让本进程的缓存作废（下一次请求回源）。
func (m *KeyManager) BumpGeneration() {
	if m.cache != nil {
		m.cache.bumpGeneration()
	}
}

// InvalidateAll 清空已验证结果缓存（运维/测试用；例如批量改库后强制回源）。
func (m *KeyManager) InvalidateAll() {
	if m.cache != nil {
		m.cache.bumpGeneration()
		m.cache.invalidateAll()
	}
}

// CacheStats 返回当前缓存条目数（含尚未惰性清扫的过期项）与累计命中/未命中次数。
// 缓存关闭时全为 0。
func (m *KeyManager) CacheStats() (entries int, hits, misses uint64) {
	if m.cache == nil {
		return 0, 0, 0
	}
	return m.cache.stats()
}

// CacheClearCount 返回「因容量到顶而整表清空」的次数。
//
// 为什么要暴露这个数：它是「4096 条上限被打到」的唯一可观测信号。长期为 0 说明
// 上限够用；持续增长说明活 key 数远超预期，该调大上限（或换共享缓存）了。
func (m *KeyManager) CacheClearCount() uint64 {
	if m.cache == nil {
		return 0
	}
	return m.cache.clears.Load()
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
