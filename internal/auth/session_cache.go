package auth

import (
	"sync/atomic"
	"time"
)

// 会话缓存与活跃时间节流（#241）。
//
// 背景：改造前每个「带会话」的请求固定 4 次 DB 往返——
//  1. GetSession（校验会话是否存在/过期）
//  2. GetUserByID（只为读 Disabled；而且错误被静默吞掉，见下面 authSession 的说明）
//  3. TouchSession（同步 UPDATE 写事务，SQLite 单写，最贵的一条）
//  4. 下游 AuthorizeNamespace
//
// 本文件处理前三条：
//   - 会话缓存把 1+2 攒成「每 cacheTTL（默认 10s）一次」；
//   - 活跃时间节流把 3 从「每请求一次同步写」降到「每 touchInterval（默认 60s）一次异步写」。
//
// 安全取舍（为什么缓存不降级安全性）：
//   - 缓存只存 userID / expiresAt / 时间戳，不存 Cookie、不存任何凭据；
//   - 命中路径仍然校验 expiresAt（不止看缓存年龄），过期会话不会被缓存复活超过一个 cacheTTL；
//   - 会话失效必须显式失效：Revoke 成功后同步删条目，禁用用户由 InvalidateUser 删（见 session.go）；
//   - 任何 DB 读错误一律 fail-closed（401），不做「查不到就放行」。

const (
	// defaultSessionCacheTTL 是会话缓存默认 TTL。
	// 取值依据：10s 足以把同一页面连发的十几个请求压成 2 次 DB 往返，
	// 又足够短——即使某个失效路径漏了钩子，最坏也只多放行 10s。
	defaultSessionCacheTTL = 10 * time.Second
	// defaultTouchInterval 是 last_seen 的最小落库间隔。
	// last_seen 只用于「会话列表最近活跃」，不需要秒级精度，60s 完全够用。
	defaultTouchInterval = 60 * time.Second
	// maxSessionCacheEntries 是缓存条目硬上限。
	// 单进程 + 会话 Cookie 不可枚举，8192 条（≈ 8192 个活跃会话）已远超单机部署规模。
	maxSessionCacheEntries = 8192
)

// sessEntry 是一条会话缓存。
//
// 用指针存于 map：lastTouch 必须是同一个原子量实例，否则 CAS 抢写权失效。
type sessEntry struct {
	// userID 是会话归属用户；命中缓存时直接注入 context，跳过 GetSession+GetUserByID。
	userID string
	// expiresAt 是会话自身的过期时间（unix 秒，来自 sessions.expires_at）。
	// 命中路径必须再校验它：否则缓存会让已过期会话多活一个 cacheTTL。
	expiresAt int64
	// createdAt 是缓存写入时间。用时间点而非秒数，避免整秒截断导致 TTL 抖动。
	createdAt time.Time
	// lastTouch 是最近一次「已经发起落库」的 last_seen（unix 秒）。
	// 多个并发请求用它 CAS 抢写权：只有抢到的那个 goroutine 去写库。
	lastTouch atomic.Int64
}

// fresh 判断缓存条目是否仍然可用：既要在 TTL 内，也要会话本身没过期。
func (e *sessEntry) fresh(now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	if now.Sub(e.createdAt) >= ttl {
		return false
	}
	return now.Unix() <= e.expiresAt
}

// cacheEnabled 判断是否启用会话缓存。
func (m *SessionManager) cacheEnabled() bool {
	return m.cacheTTL > 0 && m.cache != nil
}

// cacheLookup 取一条缓存（不存在返回 nil）。
func (m *SessionManager) cacheLookup(sessionID string) *sessEntry {
	if !m.cacheEnabled() {
		return nil
	}
	m.mu.RLock()
	e := m.cache[sessionID]
	m.mu.RUnlock()
	return e
}

// cacheStore 写入缓存，返回条目；容量已满且清不出空间时返回 nil。
//
// 已存在的条目**原地刷新**（保留 lastTouch）：否则每次 cacheTTL 过期，
// 节流窗口都会被重置，活跃会话的 last_seen 写入会从「每 touchInterval 一次」
// 退化回「每 cacheTTL 一次」。
func (m *SessionManager) cacheStore(sessionID, userID string, expiresAt int64, now time.Time) *sessEntry {
	if !m.cacheEnabled() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.cache[sessionID]; ok {
		e.userID = userID
		e.expiresAt = expiresAt
		e.createdAt = now
		return e
	}
	if len(m.cache) >= maxSessionCacheEntries {
		m.pruneExpiredLocked(now)
		if len(m.cache) >= maxSessionCacheEntries {
			// 满了就先清过期项；仍满则这一次不写缓存。
			// 不驱逐有效条目：否则「把缓存灌满」就等于把热会话踢出去（降级成全部打 DB）。
			return nil
		}
	}
	e := &sessEntry{userID: userID, expiresAt: expiresAt, createdAt: now}
	m.cache[sessionID] = e
	return e
}

// pruneExpiredLocked 删除过期/超龄条目。调用方必须持写锁。
func (m *SessionManager) pruneExpiredLocked(now time.Time) {
	for id, e := range m.cache {
		if !e.fresh(now, m.cacheTTL) {
			delete(m.cache, id)
		}
	}
}

// InvalidateSession 删除单个会话的缓存条目。
// 用法：任何「让某个会话失效」的写路径，在 DB 写成功之后调用。
func (m *SessionManager) InvalidateSession(sessionID string) {
	if !m.cacheEnabled() {
		return
	}
	m.mu.Lock()
	delete(m.cache, sessionID)
	m.mu.Unlock()
}

// InvalidateUser 删除某用户所有会话的缓存条目（多设备一次清干净）。
// 用法：超管禁用/删除用户后调用，让最后禁用状态立即生效，而不是等 cacheTTL。
func (m *SessionManager) InvalidateUser(userID string) {
	if !m.cacheEnabled() {
		return
	}
	m.mu.Lock()
	for id, e := range m.cache {
		if e.userID == userID {
			delete(m.cache, id)
		}
	}
	m.mu.Unlock()
}

// InvalidateAll 清空缓存（测试/运维用）。
func (m *SessionManager) InvalidateAll() {
	if !m.cacheEnabled() {
		return
	}
	m.mu.Lock()
	clear(m.cache)
	m.mu.Unlock()
}

// CacheStats 返回缓存条目数与命中/未命中累计次数（只用于观测/测试）。
//
// misses 统计的是「真正做了完整校验（GetSession+GetUserByID）」的次数，
// 所以命中率可直接由 hits/(hits+misses) 得到。
func (m *SessionManager) CacheStats() (entries int, hits, misses uint64) {
	if m.cacheEnabled() {
		m.mu.RLock()
		entries = len(m.cache)
		m.mu.RUnlock()
	}
	return entries, m.hits.Load(), m.misses.Load()
}

// authSession 把会话 Cookie 值解析成 userID。
//
// 返回的 *sessEntry 交给调用方做 TouchSession 节流；缓存关闭或写满时为 nil。
// userID 在「账户被禁用」这个错误路径上也会返回（用于日志），其它错误路径返回空串。
func (m *SessionManager) authSession(sessionID string) (string, *sessEntry, error) {
	now := time.Now()

	// 命中路径：0 次 DB 往返（GetSession + GetUserByID 都省掉）。
	if e := m.cacheLookup(sessionID); e != nil && e.fresh(now, m.cacheTTL) {
		m.hits.Add(1)
		return e.userID, e, nil
	}
	m.misses.Add(1)

	s, err := m.db.GetSession(sessionID)
	if err != nil {
		// 不区分「会话不存在」和「查询失败」，统一按无效会话处理：
		// 既避免泄露会话是否存在，也保证 DB 故障时 fail-closed。
		return "", nil, errSessionInvalid
	}
	if now.Unix() > s.ExpiresAt {
		return s.UserID, nil, errSessionExpired
	}
	u, err := m.db.GetUserByID(s.UserID)
	if err != nil {
		// #241 §5 点名的顺带发现：旧实现是
		//	if u, err := m.db.GetUserByID(...); err == nil && u.Disabled {
		// —— DB 查询出错（或该用户已被删除）时会**静默放行**，属于安全 bug。
		// 现在必须 fail-closed：查不到用户 = 未登录（401），绝不进入业务逻辑。
		return "", nil, errSessionInvalid
	}
	if u.Disabled {
		return s.UserID, nil, errSessionDisabled
	}
	// 写缓存。注意「账户被禁用」不进缓存（负结果不缓存）：宁可让禁用账户每次都走一次 DB，
	// 也不要把「禁用」固化进缓存、或是让批量禁用账户把缓存冲满。
	entry := m.cacheStore(sessionID, s.UserID, s.ExpiresAt, now)
	return s.UserID, entry, nil
}

// touchSession 节流刷新 last_seen。
//
// touchWrites 只做计数（见 SessionManager.TouchWriteCount），不参与判定。
//
// 判据（#241）：节流只允许「少写」，不允许「写旧值」或「永久不写」。
//   - 只有「距上次落库 ≥ touchInterval」并且 CAS 抢到写权的那**一个** goroutine 才落库，
//     且写的是本次请求拿到的 now（不会写回更早的时间戳）；
//   - 每个请求都 spawn 一个 goroutine 是 goroutine 爆炸，所以窗口内的请求一个都不 spawn；
//   - 缓存条目在 cacheTTL 过期后原地刷新（保留 lastTouch），所以活跃会话不会因为
//     缓存刷新而反复重置窗口——节流窗口一过必然会有请求写进去，不存在「永久不写」。
//
// last_seen 只有秒精度，因此节流粒度天然是整秒：touchInterval < 1s 时按 1s 生效。
func (m *SessionManager) touchSession(sessionID string, e *sessEntry) {
	now := time.Now().Unix()
	if e == nil {
		// 缓存关闭（或条目写满被拒）→ 保持旧行为：每请求同步刷新。
		// 这里刻意同步而不是异步：不能让「缓存是否命中」改变 last_seen 的可见时机，
		// 否则运维排查「最近活跃」时会看到两种时延语义。
		m.touchWrites.Add(1)
		_ = m.db.TouchSession(sessionID, now)
		return
	}
	last := e.lastTouch.Load()
	if now <= last {
		// 同一秒内的重复请求直接跳过（写相同值也是白写一次事务）。
		return
	}
	if time.Duration(now-last)*time.Second < m.touchInterval {
		return
	}
	if !e.lastTouch.CompareAndSwap(last, now) {
		// 本窗口的写权已被别的 goroutine 抢走：什么都不做，避免每请求一个 goroutine。
		return
	}
	m.touchWrites.Add(1)
	go func() {
		// 写失败忽略：last_seen 只是活跃统计（会话列表的「最近使用」），不参与鉴权判定，
		// 丢了这次刷新只影响统计精度；反过来把统计写失败上抛成 5xx 才是真的降级。
		_ = m.db.TouchSession(sessionID, now)
	}()
}

// TouchWriteCount 返回「节流放行了多少次 last_seen 写库」的累计计数。
//
// 为什么需要它（而不是在测试/运维侧数 SQLite 的 total_changes()）：
// `SELECT total_changes()` 是**按连接**统计的。store 的连接池从 #240 起不再固定为 1，
// 于是「执行 UPDATE 的连接」与「查 total_changes() 的连接」可能不是同一条，
// 计数就会随机变成 0 —— 那是测量工具的问题，不是节流的问题，但会让用例变得
// 偶发假红（本仓库踩过：同一条用例 1 次 ok / 2 次 FAIL）。
// 在写入发起处自增计数，与连接池形状无关，也顺带把「发起但写失败」计入（更严格）。
//
// 「真的落库了」由另一条独立断言保证：测试会等异步写落地后从 DB 读回 last_seen
// 并断言它 >= 请求发生的那一秒。两个证据合起来才完整：
//   - 本计数 = 节流放行了几次（上界/下界）
//   - DB 里的 last_seen = 写确实生效、且写的是新时间（不是旧值、不是永久冻住）
func (m *SessionManager) TouchWriteCount() uint64 { return m.touchWrites.Load() }
