package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 已验证 API Key 结果缓存的硬上限。
//
// # 内存上界
//
// 4096 条 × 约 150B/条（结构体 + map 桶开销）≈ **0.6 MB** 上限。相对单次 argon2
// 的 64 MB 分配可以忽略，但**必须有**：没有上限时，「合法的 key 很多」或「多轮换」
// 也能把进程内存顶上去，而这本身就是一个不需要攻击的 OOM 通道。
//
// # 满了怎么办：先清过期，还满就**整表清空**
//
// 为什么不做「满了就不写」：那会让「后来才出现的 key」永远进不了缓存——一旦缓存
// 被一批冷 key 占满，新 key 的每个请求都要重算 31ms 的 argon2，而且**永远不会自愈**
// （没有淘汰就腾不出位置）。
// 为什么不做 LRU：LRU 要多一份链表/计数并按访问序重排，复杂度全落在最热的读路径上；
// 而这里的衰减代价只是「一次全量重算」（最坏 4096×31ms，摊在后续请求上），
// 一次性、可预测、且不会重复发生。整表清空腾出空间后缓存立刻重新长回热集。
//
// 注意：**负结果一律不缓存**（见 ValidateKey），所以攻击者用随机 key 喷洒**灌不满**
// 这张表——只有真正校验通过的 key 才有资格占位。这是「容量上限」之外的第一道保险。
const maxKeyCacheEntries = 4096

// defaultKeyCacheTTL 是缓存条目的默认存活时间。
//
// 为什么是 60s：缓存的是「这把 key 此刻有效 + 它属于谁/有什么 scope」。revoke /
// 改 scopes / 删 key 由 generation bump **同步**整表失效（安全边界不靠 TTL 兜），
// TTL 兜的只是**绕过本进程**的写路径（运维直改库、同库被别的进程写）。
// 60s 落在要求的 30–60s 区间上沿：命中率更高，而失效已经不依赖它了。
const defaultKeyCacheTTL = 60 * time.Second

// keyTouchInterval 是缓存命中路径刷新 last_used_at 的最小间隔。
//
// 为什么节流：原始实现每个请求都写一次 last_used_at（一次写事务，SQLite 单写者，
// 直接把并发打死，且让「命中路径零 DB 往返」失效）。last_used_at 是给人看的面板
// 数据，60s 粒度完全够；真需要精确值就走缓存失效后的完整校验路径。
const keyTouchInterval = 60 * time.Second

// keyEntry 是一条已验证结果。
//
// 刻意**只**存判定结论（谁、什么 scope、哪条记录、什么时候失效）：不存明文 key，
// 也不存 argon2 的 salt/hash。存下哈希材料等于把「离线爆破的靶子」放进进程内存
// 的长期对象里，而且没有任何收益——命中判定用的是 HMAC 摘要，salt/hash 用不上。
//
// 除 lastTouched 外，条目**发布后不再被原地修改**（刷新一律换新指针），
// 所以读路径可以只持读锁、甚至在释放读锁后继续读 userID/scopes/expiresAt。
type keyEntry struct {
	userID    string
	scopes    []string
	keyID     string
	expiresAt time.Time
	// gen 是条目写入时看到的缓存 generation。
	// 任何「可能改变 key 有效性」的写路径（revoke / 改 scopes / 删除 / 手工清缓存）
	// 都会把 keyCache.gen +1，本字段与当前 generation 不等即为失效。
	gen uint64
	// verifier 是**独立的** HMAC(secret, "verify" ‖ 明文)。
	//
	// 为什么要多存一份而不是直接信 map 命中：命中后必须再做一次
	// subtle.ConstantTimeCompare 才放行（见 ValidateKey）。这条比对不是装饰——
	// 条目与请求对不上（哈希碰撞、map 被写坏、将来引入异步刷新时的竞态）时它真的会失败；
	// 而比较本身是定长常量时间的，不会泄露「前几位对上了」这种信息。
	verifier [32]byte
	// lastTouched 是上次真正写库的 unixnano 时间，用原子变量是为了让并发的
	// 命中请求能无锁 CAS 出「唯一一个」去写 DB 的请求（见 touchThrottled）。
	lastTouched atomic.Int64
}

// keyCache 是进程内的「已验证 key → 判定结论」表。
//
// 缓存键**不是**明文、不是明文哈希、也不是可直接鉴权的东西，而是
//
//	HMAC-SHA256(进程随机密钥, key 前缀 ‖ 0x00 ‖ 完整明文)
//
// 为什么不用裸 sha256(明文)：
//   - HMAC 的密钥只存在于本进程内存（启动时 crypto/rand 生成，不落盘、不落日志、
//     不随 DB 持久化）。即使有人拿到整个缓存表，也既不能反推明文，**也不能离线
//     验证候选 key**（裸哈希在这一点上是个可用的驗證子：拿到 sha256 就能离线跑字典，
//     只是本项目的 key 有 192 bit 随机性让这条路不现实——但"不现实"不是"不用防"）。
//   - 前缀参与 MAC 输入，等于把「身份标签」一并绑定，跨前缀的同明文撞不进同一个桶。
//
// 用固定 32 字节数组当 map key（而不是 hex 字符串）：省一次编码分配，
// 比较成本固定，且不会被误当成可读的凭据材料打印出去。
type keyCache struct {
	mu      sync.RWMutex
	entries map[[32]byte]*keyEntry
	// gen 是全局失效代数。命中要求「条目 gen == 当前 gen」，bump 一次就整表失效。
	gen    atomic.Uint64
	hits   atomic.Uint64
	misses atomic.Uint64
	// clears 是「因容量到顶而整表清空」的次数（观测用：正常部署应长期为 0）。
	clears atomic.Uint64
	secret [32]byte // HMAC 密钥（仅本进程内存）
}

// 缓存键与验证子的域分隔前缀（避免两类 HMAC 输入落在同一条轨道上）。
const (
	cacheKeyDomain = "anotify/apikey/cache/v1\x00"
	cacheVerDomain = "anotify/apikey/verify/v1\x00"
)

// newKeyCache 创建一个空缓存。
//
// 随机密钥取不到时**返回错误而不是退化**：退化（比如退回裸 sha256）会静默降低
// 安全属性，那是更坏的失败模式。调用方（NewKeyManagerTuned）在出错时直接**关闭缓存**，
// 于是所有请求走完整的 argon2 校验——功能与安全都不受影响，只是少了优化。
func newKeyCache() (*keyCache, error) {
	c := &keyCache{entries: make(map[[32]byte]*keyEntry)}
	if _, err := rand.Read(c.secret[:]); err != nil {
		return nil, err
	}
	return c, nil
}

// macOf 用进程密钥算 HMAC-SHA256。domain 做域分隔。
func (c *keyCache) macOf(domain, prefix, plaintext string) [32]byte {
	m := hmac.New(sha256.New, c.secret[:])
	_, _ = m.Write([]byte(domain))
	if prefix != "" {
		_, _ = m.Write([]byte(prefix))
		_, _ = m.Write([]byte{0})
	}
	_, _ = m.Write([]byte(plaintext))
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

// digestOf 计算缓存键。
func (c *keyCache) digestOf(prefix, plaintext string) [32]byte {
	return c.macOf(cacheKeyDomain, prefix, plaintext)
}

// verifierOf 计算命中后用于常量时间比对的独立验证子。
func (c *keyCache) verifierOf(plaintext string) [32]byte {
	return c.macOf(cacheVerDomain, "", plaintext)
}

// generation 返回当前代数（写路径在**读 DB 之前**快照它，见 ValidateKey）。
func (c *keyCache) generation() uint64 { return c.gen.Load() }

// bumpGeneration 让整表立刻失效，返回新代数。
//
// 这是「revoke / 改 scopes / 删 key 必须立刻失效」的最简实现：失效方不需要知道
// 具体是哪把 key（很多写路径只拿得到 keyID，甚至只有用户 id），bump 一次全表作废，
// 下一批请求自然回落 DB 重新校验。代价是那次失效会清空所有 key 的命中率——
// 但 key 的写操作（签发/吊销）本来就很稀有。
func (c *keyCache) bumpGeneration() uint64 { return c.gen.Add(1) }

// lookup 查一条**当前可用**的条目：未过期且 generation 匹配。
//
// 不做命中计数（计数在 ValidateKey 里做，因为「常量时间比对没过」也算未命中）。
// 过期/换代条目在这里只算查不到、不删除：调用方紧接着会走完整校验并 put 同一摘要
// （覆盖掉它），而容量吃紧时由 sweepExpiredLocked 惰性清扫。这样高并发命中路径
// 全程只需要读锁，不会因为「顺手删个过期项」升级成写锁互相排队。
// 读出指针后立即释放读锁：条目发布后不可变（见 keyEntry 注释），所以后续读
// expiresAt / verifier 不需要持锁。
func (c *keyCache) lookup(digest [32]byte, now time.Time) *keyEntry {
	c.mu.RLock()
	e := c.entries[digest]
	c.mu.RUnlock()
	if e == nil || !now.Before(e.expiresAt) || e.gen != c.gen.Load() {
		return nil
	}
	return e
}

// forget 丢掉一个条目（命中后发现验证子不匹配时用）。
func (c *keyCache) forget(digest [32]byte) {
	c.mu.Lock()
	delete(c.entries, digest)
	c.mu.Unlock()
}

// put 写入（或覆盖）一条条目，返回是否真的写进去了。
//
// 容量到顶时两级处理（见 maxKeyCacheEntries）：
//  1. 先清「已过期 / 已换代」的条目 —— 大多数情况下这就够了；
//  2. 仍然满 → **整表清空**（一次性重算代价，换内存有界）。
//
// 同摘要的覆盖（刷新）不受容量限制——那是更新既有条目，不增长。
// 返回值只表示「这次有没有写进去」，调用方不依赖它做正确性判断。
func (c *keyCache) put(digest [32]byte, e *keyEntry, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[digest]; !exists && len(c.entries) >= maxKeyCacheEntries {
		c.sweepExpiredLocked(now)
		if len(c.entries) >= maxKeyCacheEntries {
			// 整表清空：比「不写入」更公平（新 key 也能进来），
			// 也比 LRU 简单（读路径上不引入任何额外记账）。
			clear(c.entries)
			c.clears.Add(1)
		}
	}
	c.entries[digest] = e
	return true
}

// sweepExpiredLocked 删除所有已过期或已换代的条目。调用方必须持有写锁。
func (c *keyCache) sweepExpiredLocked(now time.Time) {
	gen := c.gen.Load()
	for k, e := range c.entries {
		if !now.Before(e.expiresAt) || e.gen != gen {
			delete(c.entries, k) // 遍历中删除当前元素在 Go 里是安全的
		}
	}
}

// invalidateKeyID 删掉某条 DB 记录对应的全部条目（同一 key 只有一条，但按 keyID
// 删而不是按摘要删，是因为失效调用方只拿得到 keyID，拿不到明文）。
func (c *keyCache) invalidateKeyID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if e.keyID == id {
			delete(c.entries, k)
		}
	}
}

// invalidateAll 清空缓存。用 clear 而不是重新 make：保留已分配的桶，
// 避免「清空 → 立刻又填」时反复扩容。
func (c *keyCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
}

// stats 返回当前条目数（含尚未被清扫的过期项）与累计命中/未命中次数。
func (c *keyCache) stats() (entries int, hits, misses uint64) {
	c.mu.RLock()
	n := len(c.entries)
	c.mu.RUnlock()
	return n, c.hits.Load(), c.misses.Load()
}

// touchThrottled 在缓存命中路径刷新 last_used_at，每个 key 每 keyTouchInterval
// 最多写一次库。
//
// 为什么要 CAS：并发命中同一个 key 时，多个 goroutine 会同时看到「已经超过 60s」，
// 只有 CompareAndSwap 成功的那一个写库，其余直接返回——这样命中路径常态下是
// 零 DB 往返，而不是「每个请求都写一次」或「每请求都加一把锁」。
// 写库失败故意忽略：last_used_at 只是观测数据，不能因为它让一次合法鉴权失败。
func (m *KeyManager) touchThrottled(e *keyEntry, now time.Time) {
	prev := e.lastTouched.Load()
	if now.UnixNano()-prev < int64(keyTouchInterval) {
		return
	}
	if !e.lastTouched.CompareAndSwap(prev, now.UnixNano()) {
		return
	}
	_ = m.db.TouchAPIKey(e.keyID, store.Now())
}

// cloneScopes 复制一份 scopes 给调用方。
//
// 为什么必须复制：缓存条目会被后续请求复用，若把内部切片直接给出去，调用方
// （中间件、handler）任何一次 append/改写都会静默污染缓存，等于让一个请求的
// bug 变成全局越权。复制是每次命中 1 次小分配，相对 argon2 的 64MB 可以忽略。
func cloneScopes(scopes []string) []string {
	if scopes == nil {
		return nil
	}
	return slices.Clone(scopes)
}

// cacheDigestPrefixLen 是参与缓存键的 key 前缀长度（与 CreateKey 的 prefix 口径一致）。
const cacheDigestPrefixLen = 16

// keyPrefixOf 取 key 的前缀（含 ant_<label>_ 开头部分）。
func keyPrefixOf(plaintext string) string {
	if len(plaintext) > cacheDigestPrefixLen {
		return plaintext[:cacheDigestPrefixLen]
	}
	return plaintext
}

// constTimeEqual 是 subtle.ConstantTimeCompare 的薄封装（只为了让调用点读起来
// 一眼就知道这里是安全比较，而不是 bytes.Equal 那种短路比较）。
func constTimeEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// logCacheDisabled 在缓存因随机数失败而关闭时留一条证据（静默降级是最难查的）。
func logCacheDisabled(err error) {
	slog.Warn("api key 缓存关闭（随机数不可用），所有校验走完整 argon2 路径",
		"event", "auth.apikey_cache.disabled",
		"error", err.Error(),
	)
}
