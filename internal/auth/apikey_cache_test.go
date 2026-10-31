package auth

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// openKeyTestDB 打开一个**真 store**（文件型 SQLite，落在临时目录）。
// 用真实 DB 而不是桩：缓存的正确性论证依赖「DB 里到底被查了几次」，
// 桩会把这个证据变成对桩的假设。
func openKeyTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// sql.DB.Close 幂等（二次调用返回 nil），所以测试体内提前 Close 也不会与
	// 这里的 cleanup 冲突。
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// mustUser 建一个用户（api_keys.user_id 有外键，必须存在）。
func mustUser(t *testing.T, db *store.DB, username string) string {
	t.Helper()
	id := store.NewUserID()
	if err := db.CreateUser(&store.User{ID: id, Username: username, CreatedAt: store.Now()}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

// mustKey 签发一把 API Key，返回明文与记录。
func mustKey(t *testing.T, km *KeyManager, userID string, scopes []string) (string, *store.APIKey) {
	t.Helper()
	plaintext, rec, err := km.CreateKey(userID, "test-key", scopes)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return plaintext, rec
}

// TestAPIKeyCacheHitSkipsDB 证明命中路径是 0 次 DB、0 次 argon2：
// 把 DB 关掉再校验仍然成功（这是最强的「零 DB 往返」证据），
// 同时断言命中计数增长、返回的 scopes 是副本。
func TestAPIKeyCacheHitSkipsDB(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db) // 默认 TTL → 缓存开启
	uid := mustUser(t, db, "cache-hit")
	plaintext, _ := mustKey(t, km, uid, []string{ScopeNotifySend})

	// 第一次：未命中 → 完整校验（DB + argon2）并写缓存。
	got, scopes, err := km.ValidateKey(plaintext)
	if err != nil {
		t.Fatalf("首次校验应成功: %v", err)
	}
	if got != uid {
		t.Fatalf("userID 不匹配: got %q want %q", got, uid)
	}
	if !HasScope(scopes, ScopeNotifySend) {
		t.Fatalf("scopes 应含 %s, got %v", ScopeNotifySend, scopes)
	}

	entries, hits, misses := km.CacheStats()
	if entries != 1 {
		t.Fatalf("首次校验后应有 1 条缓存, got %d", entries)
	}
	if hits != 0 || misses != 1 {
		t.Fatalf("首次校验应记 1 次未命中 0 次命中, got hits=%d misses=%d", hits, misses)
	}

	// 关掉 DB：后续若还有任何 DB 往返都会失败。
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	for i := 0; i < 3; i++ {
		got, scopes, err := km.ValidateKey(plaintext)
		if err != nil {
			t.Fatalf("第 %d 次命中校验应成功（DB 已关闭，说明命中路径没碰 DB）: %v", i+1, err)
		}
		if got != uid {
			t.Fatalf("第 %d 次命中校验 userID 不匹配: got %q want %q", i+1, got, uid)
		}
		if !HasScope(scopes, ScopeNotifySend) {
			t.Fatalf("第 %d 次命中校验 scopes 应含 %s, got %v", i+1, ScopeNotifySend, scopes)
		}
		// 调用方改动返回值不得污染缓存：返回的必须是副本。
		scopes[0] = "polluted:scope"
	}

	entries2, hits2, misses2 := km.CacheStats()
	if hits2-hits != 3 {
		t.Fatalf("命中次数应增长 3, got %d → %d", hits, hits2)
	}
	if misses2 != misses {
		t.Fatalf("命中不该产生新的未命中, got %d → %d", misses, misses2)
	}
	if entries2 != entries {
		t.Fatalf("命中不该改条目数, got %d → %d", entries, entries2)
	}
}

// TestAPIKeyCacheRejectsWrongKey 同前缀、不同随机段（篡改末位）必须被拒绝，
// 且负结果不得进缓存。
func TestAPIKeyCacheRejectsWrongKey(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "wrong-key")
	plaintext, _ := mustKey(t, km, uid, []string{ScopeNotifySend})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("正确 key 应校验成功: %v", err)
	}
	// 篡改末位，确保与原值不同（避免 1/64 概率 flake）。
	last := plaintext[len(plaintext)-1]
	replacement := byte('X')
	if last == 'X' {
		replacement = 'Y'
	}
	wrong := plaintext[:len(plaintext)-1] + string(replacement)
	if strings.HasPrefix(wrong, plaintext) {
		t.Fatalf("构造的错 key 不应还是正确 key 的前缀: %q", wrong)
	}
	if _, _, err := km.ValidateKey(wrong); err == nil {
		t.Fatalf("同前缀不同随机段的 key 必须被拒绝")
	}

	entries, hits, misses := km.CacheStats()
	if entries != 1 {
		t.Fatalf("错 key 不得进缓存（应仍只有 1 条正确 key）, got %d", entries)
	}
	if hits != 0 {
		t.Fatalf("错 key 不该命中缓存, got hits=%d", hits)
	}
	if misses != 2 {
		t.Fatalf("应有 2 次未命中（正确 key 首次 + 错 key）, got %d", misses)
	}

	// 错 key 之后正确 key 仍然可用。
	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("错 key 之后正确 key 应仍可用: %v", err)
	}
}

// TestAPIKeyCacheExpiredEntryFallsBack 过期条目必须回落到完整校验（不是继续用旧结论）。
func TestAPIKeyCacheExpiredEntryFallsBack(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManagerTuned(db, time.Millisecond)
	uid := mustUser(t, db, "expired")
	plaintext, _ := mustKey(t, km, uid, []string{ScopeNotifySend})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("首次校验应成功: %v", err)
	}
	_, hits, misses := km.CacheStats()

	time.Sleep(5 * time.Millisecond)

	got, scopes, err := km.ValidateKey(plaintext)
	if err != nil {
		t.Fatalf("条目过期后应回落完整校验并仍然成功: %v", err)
	}
	if got != uid || !HasScope(scopes, ScopeNotifySend) {
		t.Fatalf("回落结果不正确: uid=%q scopes=%v", got, scopes)
	}
	_, hits2, misses2 := km.CacheStats()
	if hits2 != hits {
		t.Fatalf("过期条目不应算命中, got hits %d → %d", hits, hits2)
	}
	if misses2 != misses+1 {
		t.Fatalf("过期条目应记一次未命中, got misses %d → %d", misses, misses2)
	}
}

// TestAPIKeyRevokeInvalidatesCache revoke 后必须**立即**拒绝，不允许等 TTL 过期。
func TestAPIKeyRevokeInvalidatesCache(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db) // 默认 TTL 60s：若靠 TTL 兜底，本测试必然失败
	uid := mustUser(t, db, "revoke")
	plaintext, rec := mustKey(t, km, uid, []string{ScopeNotifySend})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("revoke 前校验应成功: %v", err)
	}
	if entries, _, _ := km.CacheStats(); entries != 1 {
		t.Fatalf("revoke 前缓存应为热（1 条）, got %d", entries)
	}

	if err := km.RevokeKey(rec.ID); err != nil {
		t.Fatalf("revoke 应成功: %v", err)
	}
	if _, _, err := km.ValidateKey(plaintext); err == nil {
		t.Fatalf("revoke 后必须立即拒绝，不允许等 TTL 过期")
	}
	if entries, _, _ := km.CacheStats(); entries != 0 {
		t.Fatalf("revoke 后缓存应被清空, got %d 条", entries)
	}
}

// TestAPIKeyRejectsNonAntPrefixUncached 没有 ant_ 前缀的输入必须直接拒，
// 且不产生任何缓存动作（连摘要都不算，防垃圾输入打表）。
func TestAPIKeyRejectsNonAntPrefixUncached(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)

	if _, _, err := km.ValidateKey("sk_live_x"); err == nil {
		t.Fatalf("无 ant_ 前缀的 key 必须被拒绝")
	}
	entries, hits, misses := km.CacheStats()
	if entries != 0 || hits != 0 || misses != 0 {
		t.Fatalf("无前缀输入不得进缓存也不应记账, got entries=%d hits=%d misses=%d", entries, hits, misses)
	}
}

// TestAPIKeyCacheHoldsNoPlaintext 缓存内部结构里不得出现明文或半明文（随机段）。
// 同包可以直接摸内部 map：这是「有没有把密钥材料存进长期对象」的唯一硬证据。
func TestAPIKeyCacheHoldsNoPlaintext(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "no-plaintext")
	plaintext, rec := mustKey(t, km, uid, []string{ScopeNotifySend, ScopeNotifyReceive})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("校验应成功: %v", err)
	}

	km.cache.mu.RLock()
	defer km.cache.mu.RUnlock()
	if n := len(km.cache.entries); n != 1 {
		t.Fatalf("应有 1 条缓存, got %d", n)
	}
	// 键必须是 32 字节 HMAC 摘要（域分隔 + 进程密钥），且**不得**等于裸 sha256(明文)。
	want := km.cache.digestOf(rec.Prefix, plaintext)
	e, ok := km.cache.entries[want]
	if !ok {
		t.Fatalf("缓存键必须等于 HMAC(secret, 前缀‖明文) 摘要")
	}
	if want == sha256.Sum256([]byte(plaintext)) {
		t.Fatalf("缓存键不能是裸 sha256(明文)：那是一个可离线验证的验证子")
	}
	for d := range km.cache.entries {
		if len(d) != sha256.Size {
			t.Fatalf("缓存键必须是 %d 字节摘要, got %d", sha256.Size, len(d))
		}
	}

	// 随机段（明文里真正的秘密部分）取尾部 16 字符：即使 keyID/userID 恰好包含
	// 前缀部分也不算泄漏，泄漏一定意味着随机段进了结构体。
	secret := plaintext[len(plaintext)-16:]
	joined := e.userID + e.keyID + strings.Join(e.scopes, ",")
	if strings.Contains(joined, plaintext) {
		t.Fatalf("缓存条目含明文 key")
	}
	if strings.Contains(joined, secret) {
		t.Fatalf("缓存条目含明文 key 的随机段")
	}
	// 反射扫全部 string 字段，防止将来加字段时漏掉这条断言。
	v := reflect.ValueOf(e).Elem()
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() != reflect.String {
			continue
		}
		s := v.Field(i).String()
		if strings.Contains(s, plaintext) || strings.Contains(s, secret) {
			t.Fatalf("缓存条目字段 %s 含明文/半明文", v.Type().Field(i).Name)
		}
	}
	// keyID 必须是真实记录 id，否则 RevokeKey 失效就失效不掉。
	if e.keyID != rec.ID {
		t.Fatalf("缓存条目 keyID 应为 %q, got %q", rec.ID, e.keyID)
	}
}

// TestAPIKeyCacheConcurrent 并发命中/过期/回源混跑：配 -race 跑，要求无 panic、
// 结果一致、账目闭合（每次调用恰好记一次命中或未命中）。
func TestAPIKeyCacheConcurrent(t *testing.T) {
	db := openKeyTestDB(t)
	// 50ms TTL + argon2 约 30ms → 跑的过程中会反复经历「命中→过期→回源」。
	km := NewKeyManagerTuned(db, 50*time.Millisecond)
	uid := mustUser(t, db, "concurrent")
	plaintext, _ := mustKey(t, km, uid, []string{ScopeNotifySend})

	const goroutines, perGoroutine = 8, 50
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				got, scopes, err := km.ValidateKey(plaintext)
				if err != nil {
					errCh <- fmt.Errorf("goroutine %d 第 %d 次校验失败: %w", g, i, err)
					return
				}
				if got != uid {
					errCh <- fmt.Errorf("goroutine %d 第 %d 次 userID 不匹配: %q", g, i, got)
					return
				}
				if !HasScope(scopes, ScopeNotifySend) {
					errCh <- fmt.Errorf("goroutine %d 第 %d 次 scopes 不匹配: %v", g, i, scopes)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("%v", err)
	}

	entries, hits, misses := km.CacheStats()
	if hits+misses != goroutines*perGoroutine {
		t.Fatalf("账目应闭合（每次调用一次命中或未命中）: hits=%d misses=%d total=%d want=%d",
			hits, misses, hits+misses, goroutines*perGoroutine)
	}
	// 命中数**不**作为断言：50ms TTL 与 8 路并发 argon2(64MB) 争抢内存带宽时，
	// 单次回源本身就可能超过 TTL，命中次数取决于机器负载（本机实测常为 0）。
	// 这里只记录观察值，命中路径的正确性由 TestAPIKeyCacheConcurrentHits 覆盖。
	t.Logf("并发混跑: hits=%d misses=%d entries=%d", hits, misses, entries)
	// 同一把 key 只会有一条条目；容量上限不该被触碰。
	if entries > 1 {
		t.Fatalf("同一把 key 只应有 1 条缓存, got %d", entries)
	}
}

// TestAPIKeyCacheConcurrentHits 并发**命中**路径（TTL 足够长，argon2 只跑一次）：
// 覆盖读锁共享、scopes 副本隔离、lastTouched 的 CAS 节流，成本是纯内存操作，
// 配合 -race 跑才有意义且几乎不占时间。
func TestAPIKeyCacheConcurrentHits(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db) // 默认 TTL 60s：整个测试期间条目都在有效期内
	uid := mustUser(t, db, "concurrent-hits")
	plaintext, _ := mustKey(t, km, uid, []string{ScopeNotifySend, ScopeNotifyReceive})
	// 暖缓存：只在这里跑一次 argon2。
	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("暖缓存校验失败: %v", err)
	}
	_, hits0, misses0 := km.CacheStats()

	const goroutines, perGoroutine = 8, 50
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				got, scopes, err := km.ValidateKey(plaintext)
				if err != nil {
					errCh <- fmt.Errorf("goroutine %d 第 %d 次: %w", g, i, err)
					return
				}
				// 断言完整内容而不只是长度：污染型 bug（返回内部切片）
				// 会让 len 仍然为 2，只有值校验才能抓住。
				if got != uid || len(scopes) != 2 ||
					!HasScope(scopes, ScopeNotifySend) || !HasScope(scopes, ScopeNotifyReceive) {
					errCh <- fmt.Errorf("goroutine %d 第 %d 次结果不一致: uid=%q scopes=%v", g, i, got, scopes)
					return
				}
				// 污染自己的副本，下一次调用必须仍然是干净的两项。
				scopes[0] = "polluted:scope"
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("%v", err)
	}

	_, hits, misses := km.CacheStats()
	if misses != misses0 {
		t.Fatalf("暖缓存后不应再有未命中（说明命中路径没碰 DB）, misses %d → %d", misses0, misses)
	}
	if hits-hits0 != goroutines*perGoroutine {
		t.Fatalf("命中次数应增长 %d, got %d → %d", goroutines*perGoroutine, hits0, hits)
	}
}

// TestAPIKeyRevokeAfterDBErrorKeepsCache DB 写失败时**不得**清缓存：
// 此时 key 在库里仍然有效，清缓存只是白丢性能，还会让缓存与 DB 分叉。
func TestAPIKeyRevokeAfterDBErrorKeepsCache(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "revoke-dberr")
	plaintext, rec := mustKey(t, km, uid, []string{ScopeNotifySend})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("校验应成功: %v", err)
	}
	entries, _, _ := km.CacheStats()
	if entries != 1 {
		t.Fatalf("校验后应有 1 条缓存, got %d", entries)
	}

	// 关掉 DB 模拟写失败。
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := km.RevokeKey(rec.ID); err == nil {
		t.Fatalf("DB 已关闭时 RevokeKey 必须返回错误")
	}
	if entries2, _, _ := km.CacheStats(); entries2 != entries {
		t.Fatalf("DB 失败不得清缓存, got %d → %d", entries, entries2)
	}
}

// TestAPIKeyCacheTouchThrottled 验证 last_used_at 的 60s 节流：
// 命中路径常态下**一次 DB 写都不做**，只有节流窗口到期的那一个请求写。
//
// 观测手法：直接把库里的 last_used_at 改成哨兵值 1，再校验——
// 值没变就说明这次请求真的没写库（比「数调用次数」更直接，也不需要打桩）。
func TestAPIKeyCacheTouchThrottled(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "touch-throttle")
	plaintext, rec := mustKey(t, km, uid, []string{ScopeNotifySend})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("首次校验应成功: %v", err)
	}
	// 哨兵：把 last_used_at 改成 1 秒（1970），便于分辨「有没有被写过」。
	if _, err := db.Exec(`UPDATE api_keys SET last_used_at = 1 WHERE id = ?`, rec.ID); err != nil {
		t.Fatalf("改写 last_used_at 失败: %v", err)
	}
	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("命中校验应成功: %v", err)
	}
	got, err := db.GetAPIKeyByPrefix(rec.Prefix)
	if err != nil {
		t.Fatalf("回读 key: %v", err)
	}
	if !got.LastUsedAt.Valid || got.LastUsedAt.Int64 != 1 {
		t.Fatalf("命中路径不应写 last_used_at（节流窗口内零 DB 写）, got %v", got.LastUsedAt)
	}

	// 把条目的 lastTouched 拨回 2 分钟前，模拟节流窗口到期：此时应当写一次。
	digest := km.cache.digestOf(rec.Prefix, plaintext)
	km.cache.mu.Lock()
	e := km.cache.entries[digest]
	km.cache.mu.Unlock()
	if e == nil {
		t.Fatalf("缓存条目应存在")
	}
	e.lastTouched.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("命中校验应成功: %v", err)
	}
	got, err = db.GetAPIKeyByPrefix(rec.Prefix)
	if err != nil {
		t.Fatalf("回读 key: %v", err)
	}
	if !got.LastUsedAt.Valid || got.LastUsedAt.Int64 == 1 {
		t.Fatalf("节流窗口到期后应写一次 last_used_at, got %v", got.LastUsedAt)
	}
}

// TestAPIKeyCacheDisabledAlwaysHitsDB cacheTTL < 0 必须完全回到旧行为：
// 不缓存、每次请求都查库（把 DB 关掉再校验就应该失败，证明没有走缓存）。
func TestAPIKeyCacheDisabledAlwaysHitsDB(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManagerTuned(db, -1)
	uid := mustUser(t, db, "cache-off")
	plaintext, rec := mustKey(t, km, uid, []string{ScopeNotifySend})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("首次校验应成功: %v", err)
	}
	if entries, hits, misses := km.CacheStats(); entries != 0 || hits != 0 || misses != 0 {
		t.Fatalf("缓存关闭时不应有任何缓存动作, got entries=%d hits=%d misses=%d", entries, hits, misses)
	}
	// 旧行为：每次校验都写 last_used_at。
	got, err := db.GetAPIKeyByPrefix(rec.Prefix)
	if err != nil {
		t.Fatalf("回读 key: %v", err)
	}
	if !got.LastUsedAt.Valid {
		t.Fatalf("缓存关闭时应每次校验都写 last_used_at")
	}

	// 关掉 DB：缓存关闭时每次请求都必须查库，所以这里必然失败。
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, _, err := km.ValidateKey(plaintext); err == nil {
		t.Fatalf("缓存关闭时每次校验都必须查库（DB 已关闭 → 应失败）")
	}
}

// TestKeyCacheCapacityLimit 容量上限的三条性质（#227，root 裁决口径）：
//
//  1. 上限是硬的：条目数永远不超过 maxKeyCacheEntries（内存有界，无界增长不可接受）；
//  2. 满且无过期项 → **整表清空**后写入（而不是「拒绝写入」——那会让新 key 永远进不来）；
//  3. 有可清项时先走「清过期」，不是每次都整表清空（避免无谓的全量重算）。
//
// 外加一条不属于容量但同属表结构的性质：同摘要覆盖是刷新，不增长条目数。
func TestKeyCacheCapacityLimit(t *testing.T) {
	now := time.Now()
	c, err := newKeyCache()
	if err != nil {
		t.Fatalf("newKeyCache: %v", err)
	}
	// seed 决定这一批摘要的形状；只动前 4 字节，与 extra（动末字节）不撞车。
	fill := func(n int, ttl time.Duration, seed int) {
		for i := 0; i < n; i++ {
			var d [32]byte
			d[0], d[1], d[2], d[3] = byte(i+seed), byte(i>>8), byte(i>>16), byte(i>>24)
			e := &keyEntry{userID: "u", keyID: "k", expiresAt: now.Add(ttl)}
			e.lastTouched.Store(now.UnixNano())
			c.put(d, e, now)
		}
	}
	newEntry := func(uid string) *keyEntry {
		e := &keyEntry{userID: uid, keyID: "k-" + uid, expiresAt: now.Add(time.Hour)}
		e.lastTouched.Store(now.UnixNano())
		return e
	}

	// ---- 性质 1 + 2：填满 → 无过期项 → 整表清空后写入 ----
	fill(maxKeyCacheEntries, time.Hour, 0)
	if n := len(c.entries); n != maxKeyCacheEntries {
		t.Fatalf("应填满到上限 %d, got %d", maxKeyCacheEntries, n)
	}
	var extra [32]byte
	extra[31] = 0xFF
	e := newEntry("u2")
	if ok := c.put(extra, e, now); !ok {
		t.Fatalf("整表清空后应当写入成功")
	}
	if n := len(c.entries); n != 1 {
		t.Fatalf("整表清空后应只剩刚写入的 1 条, got %d", n)
	}
	if _, ok := c.entries[extra]; !ok {
		t.Fatalf("清空后新条目必须写入")
	}
	if got := c.clears.Load(); got != 1 {
		t.Fatalf("整表清空次数应为 1, got %d", got)
	}

	// ---- 同摘要覆盖 = 刷新，不增长 ----
	c.invalidateAll()
	fill(maxKeyCacheEntries, time.Hour, 0)
	var existing [32]byte
	for k := range c.entries {
		existing = k
		break
	}
	refreshed := newEntry("u3")
	if ok := c.put(existing, refreshed, now); !ok {
		t.Fatalf("满容量下覆盖已有摘要（刷新）应成功")
	}
	if n := len(c.entries); n != maxKeyCacheEntries {
		t.Fatalf("覆盖不应改变条目数, got %d", n)
	}
	if got := c.entries[existing]; got != refreshed {
		t.Fatalf("覆盖后应指向新条目")
	}
	if got := c.clears.Load(); got != 1 {
		t.Fatalf("覆盖刷新不该触发整表清空, clears=%d", got)
	}

	// ---- 性质 3：有可清项时先清过期，不整表清空 ----
	c.mu.Lock()
	half := 0
	for _, v := range c.entries {
		if half >= maxKeyCacheEntries/2 {
			break
		}
		v.expiresAt = now.Add(-time.Second)
		half++
	}
	c.mu.Unlock()
	if ok := c.put(extra, e, now); !ok {
		t.Fatalf("清掉过期项后应能写入")
	}
	if n := len(c.entries); n > maxKeyCacheEntries {
		t.Fatalf("条目数不得超过上限, got %d", n)
	}
	if got := c.clears.Load(); got != 1 {
		t.Fatalf("有可清项时不该走整表清空, clears=%d", got)
	}

	// ---- 换代条目也算「可清项」（否则换代后整表等于永久占位） ----
	c.invalidateAll()
	fill(maxKeyCacheEntries, time.Hour, 0)
	c.bumpGeneration()
	if ok := c.put(extra, e, now); !ok {
		t.Fatalf("换代后应能写入")
	}
	if got := c.clears.Load(); got != 1 {
		t.Fatalf("换代条目应被 sweep 清掉而不是整表清空, clears=%d", got)
	}
}

// TestAPIKeyCacheHitStillConstantTimeCompares 锁住「命中 ≠ 直接放行」。
//
// 为什么必须有这条：缓存命中最容易写错的样子是「map 查到了就 return userID」。
// 那样一来，任何能写坏/撞中 map 的路径（哈希碰撞实现变更、将来引入异步刷新、
// 指针复用 bug）都会直接变成越权。这里把条目的 verifier 改坏一个字节——模拟
// 「拿到的条目不是这次请求的那一条」——然后要求 ValidateKey **拒绝**它。
//
// 断言强度：不仅要求报错，还要求那个坏条目被丢掉（下一次正常 key 仍能校验通过），
// 否则一个坏条目会让这把 key 永久不可用（拒绝服务而非越权，但同样是 bug）。
func TestAPIKeyCacheHitStillConstantTimeCompares(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "ct-compare")
	plaintext, rec := mustKey(t, km, uid, []string{ScopeNotifySend})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("首次校验应成功: %v", err)
	}
	digest := km.cache.digestOf(rec.Prefix, plaintext)

	// 破坏 verifier（等价于「map 里拿到的是别人的条目」）。
	km.cache.mu.Lock()
	e := km.cache.entries[digest]
	if e == nil {
		km.cache.mu.Unlock()
		t.Fatalf("缓存条目应存在")
	}
	e.verifier[0] ^= 0xFF
	km.cache.mu.Unlock()

	// 关掉 DB 后校验：如果实现「map 命中就放行」，这里会返回成功；
	// 正确实现必须丢弃坏条目并回源 → 回源失败 → 报错。
	// 注意「不匹配」的语义是「这条缓存不可信」，不是「这个 key 无效」，
	// 所以正确动作是丢弃 + 完整校验，而不是直接判死（那会把一把好 key 打成永久 401）。
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, _, err := km.ValidateKey(plaintext); err == nil {
		t.Fatalf("条目 verifier 与请求不匹配时不得放行（连回源都没做 = map 命中就直接放行了）")
	}
	km.cache.mu.RLock()
	_, stillThere := km.cache.entries[digest]
	km.cache.mu.RUnlock()
	if stillThere {
		t.Fatalf("不匹配的条目必须被丢弃，否则这把 key 会一直撞在坏条目上")
	}
}

// TestAPIKeyCacheGenerationBumpInvalidates 锁住「revoke / 改 scopes / 删 key
// 必须**立刻**失效，不等 TTL」。
//
// 用 BumpGeneration 而不是 RevokeKey 来测，是因为它覆盖的是「写路径只改库、
// 或改的不是 enabled 字段（例如 scopes）」这一类：那些路径拿不到（也不需要）
// 具体 key 的摘要，只能整表作废。把 DB 关掉再校验是「真的回源了」的硬证据。
func TestAPIKeyCacheGenerationBumpInvalidates(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "gen-bump")
	plaintext, _ := mustKey(t, km, uid, []string{ScopeNotifySend})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("首次校验应成功: %v", err)
	}
	// 缓存热：此时关掉 DB 仍应放行。
	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("命中校验应成功: %v", err)
	}

	km.BumpGeneration()

	// 换代之后必须回源。DB 已被关掉，所以「回源」的直接后果就是失败——
	// 这正好证明它**没有**继续用旧条目。
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, _, err := km.ValidateKey(plaintext); err == nil {
		t.Fatalf("generation bump 后必须立刻失效（不得继续吃旧条目直到 TTL）")
	}
}

// TestAPIKeyRevokeKeyBumpsGeneration 锁住 RevokeKey 的两件事都做了：
// 定向删条目 + bump 代数。只做定向删的话，「读库之后、写缓存之前」发生的 revoke
// 会被并发请求写回的旧条目盖掉（见 ValidateKey 的 genAtRead 注释）。
func TestAPIKeyRevokeKeyBumpsGeneration(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "revoke-gen")
	plaintext, rec := mustKey(t, km, uid, []string{ScopeNotifySend})

	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("首次校验应成功: %v", err)
	}
	genBefore := km.cache.generation()
	if err := km.RevokeKey(rec.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := km.cache.generation(); got <= genBefore {
		t.Fatalf("RevokeKey 必须 bump generation, before=%d after=%d", genBefore, got)
	}
	if _, _, err := km.ValidateKey(plaintext); err == nil {
		t.Fatalf("revoke 后必须立刻拒绝")
	}
}

// TestAPIKeyNegativeResultNeverCachedNoDoS 锁住「**只有验证成功的结果才进缓存**」
// （#227，root 裁决 ①）。
//
// 为什么这条是安全项而不是性能项：如果失败结果也进缓存，攻击者用随机 key 刷请求
// 就变成「每次失败写一条缓存」——内存无界增长，不需要任何凭据就能把进程打死
// （一种很便宜的 DoS）。而且这些垃圾条目会顺带把真 key 挤出去。
//
// 判据用三种不同的失败形状各刷 100 次（同一前缀但随机段不同 / 前缀不存在 /
// 完全不是 ant_ 开头），断言缓存条目数**始终为 0**、命中数为 0。
func TestAPIKeyNegativeResultNeverCachedNoDoS(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "neg-no-dos")
	plaintext, rec := mustKey(t, km, uid, []string{ScopeNotifySend})

	// 次数不必一样：只有 ① 会真的跑 argon2（31ms/次），刷 30 次已经足够证明
	// 「失败不入表」；②③ 在第一道校验就返回，刷 100 次几乎不花时间。
	cases := []struct {
		name  string
		iters int
		build func(i int) string
	}{
		// ① 同前缀、不同随机段：最像「猜 key」的攻击形状（能过前缀查库，只是 argon2 比对不过）。
		{"same-prefix", 30, func(i int) string { return rec.Prefix + "AAAAAAAAAAAAAAAAAAAAAAAA"[:16] + string(rune('a'+i%26)) }},
		// ② 前缀在库里查不到：连 argon2 都不会跑。
		{"no-such-prefix", 100, func(i int) string { return "ant_send_" + strings.Repeat("Z", 8) + string(rune('a'+i%26)) }},
		// ③ 完全不像 key：第一道 HasPrefix 就拒。
		{"junk", 100, func(i int) string { return "sk_live_" + strings.Repeat("Q", 12) + string(rune('a'+i%26)) }},
	}
	for _, c := range cases {
		for i := 0; i < c.iters; i++ {
			if _, _, err := km.ValidateKey(c.build(i)); err == nil {
				t.Fatalf("%s: 伪造 key 必须被拒", c.name)
			}
		}
	}

	entries, hits, _ := km.CacheStats()
	if entries != 0 {
		t.Fatalf("失败结果绝不能进缓存：%s 刷了 300 次后条目数应为 0, got %d", "negative", entries)
	}
	if hits != 0 {
		t.Fatalf("不该有任何命中, got %d", hits)
	}

	// 真 key 仍然工作（失败的喷洒没有把表撑满/污染）。
	if _, _, err := km.ValidateKey(plaintext); err != nil {
		t.Fatalf("真 key 应仍可校验: %v", err)
	}
	if entries, _, _ := km.CacheStats(); entries != 1 {
		t.Fatalf("只有真 key 该占 1 条, got %d", entries)
	}
}

// TestAPIKeyGenerationBumpInvalidatesAllEntries 锁住「一次 bump 让**所有**旧条目
// 立刻不再命中」，而不只是「新条目 gen 更高」（#227，root 裁决补充）。
//
// 为什么要单独一条：只看单条 key 的用例（TestAPIKeyCacheGenerationBumpInvalidates）
// 无法区分「全表失效」和「刚好那条失效」。这里同时缓存 5 把不同的 key，
// bump 之后逐条断言 lookup 返回 nil —— 一条都不许漏。
func TestAPIKeyGenerationBumpInvalidatesAllEntries(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "gen-all")

	const n = 5
	plaintexts := make([]string, 0, n)
	digests := make([][32]byte, 0, n)
	for i := 0; i < n; i++ {
		p, rec := mustKey(t, km, uid, []string{ScopeNotifySend})
		if _, _, err := km.ValidateKey(p); err != nil {
			t.Fatalf("第 %d 把 key 首次校验应成功: %v", i, err)
		}
		plaintexts = append(plaintexts, p)
		digests = append(digests, km.cache.digestOf(rec.Prefix, p))
	}
	if entries, _, _ := km.CacheStats(); entries != n {
		t.Fatalf("应有 %d 条缓存, got %d", n, entries)
	}
	// bump 之前全部命中。
	now := time.Now()
	for i, d := range digests {
		if e := km.cache.lookup(d, now); e == nil {
			t.Fatalf("bump 前第 %d 条应命中", i)
		}
	}

	km.BumpGeneration()

	// bump 之后**一条都不许命中**（这是本条用例的核心断言）。
	for i, d := range digests {
		if e := km.cache.lookup(d, now); e != nil {
			t.Fatalf("bump 后第 %d 条仍然命中（generation 失效没覆盖全表）", i)
		}
	}
	// 而且能重新回源、重新入缓存（失效不是「永久不可用」）。
	for i, p := range plaintexts {
		if _, _, err := km.ValidateKey(p); err != nil {
			t.Fatalf("bump 后第 %d 把 key 回源应成功: %v", i, err)
		}
	}
	for i, d := range digests {
		if e := km.cache.lookup(d, now); e == nil {
			t.Fatalf("回源后第 %d 条应重新入缓存（新的 gen）", i)
		}
	}
}

// TestAPIKeyRevokeDuringCacheMissNotResurrected 锁住「缓存未命中路径的 gen 快照
// 必须在**读 DB 之前**」（#227，审查发现的测试缺口）。
//
// 这条 bug 的形状很隐蔽：ValidateKey 未命中的流程是
//
//	(1) 读 DB（此时 key 还是 enabled）→ (2) argon2 31ms → (3) 写缓存
//
// 如果 gen 在 (3) 才取，那么「(1) 之后、(3) 之前」发生的 revoke 就会被这条条目
// 盖过去：条目带着**revoke 之后**的 gen，看起来是新鲜的，于是后续请求全部命中 →
// 一把已吊销的 key 还能继续用满一个 TTL（60s）。安全论证原来只活在代码注释里。
//
// 本用例在 (2) 的 31ms 窗口内触发 RevokeKey，然后断言「RevokeKey 返回之后的
// 任何一次 ValidateKey 都必须失败」。
//
// 覆盖边界（诚实说明）：它能咬住「gen 在 argon2 之后/写缓存时才取」这个现实会
// 发生的写法（窗口 31ms）；对「gen 紧跟读 DB 之后就取」的写法窗口只有微秒级，
// 本用例大概率抓不到 —— 那种写法由代码里的 genAtRead 注释与这条用例共同守（
// 评审已记录为已知残留，不假装它被测住了）。
func TestAPIKeyRevokeDuringCacheMissNotResurrected(t *testing.T) {
	db := openKeyTestDB(t)
	km := NewKeyManager(db)
	uid := mustUser(t, db, "revoke-race")

	const attempts = 8
	for attempt := 0; attempt < attempts; attempt++ {
		plaintext, rec := mustKey(t, km, uid, []string{ScopeNotifySend})
		// 清空缓存，强制下一次校验走「读 DB + argon2」的未命中路径。
		km.InvalidateAll()

		inflight := make(chan error, 1)
		go func() { _, _, err := km.ValidateKey(plaintext); inflight <- err }()

		// 等它读完 DB、进到 argon2（此时 key 在库里仍是 enabled）。
		time.Sleep(5 * time.Millisecond)
		if err := km.RevokeKey(rec.ID); err != nil {
			t.Fatalf("第 %d 轮 revoke: %v", attempt, err)
		}

		// in-flight 那一发允许成功：它读库时 key 确实还有效（只是它稍后写回的
		// 缓存条目必须**不可用**）。
		<-inflight

		// 核心断言：revoke 之后，任何后续校验都必须被拒。
		if _, _, err := km.ValidateKey(plaintext); err == nil {
			t.Fatalf("第 %d 轮：RevokeKey 返回后仍能通过 —— in-flight 的未命中请求把「吊销前」"+
				"的结论用「吊销后」的 gen 写回了缓存（gen 快照时机错）", attempt)
		}
	}
}
