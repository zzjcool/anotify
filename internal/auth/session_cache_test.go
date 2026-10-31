package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 会话缓存 / 节流（#241）的测试脚手架 ----------

// newSessionTestDB 打开一个文件型 SQLite（会话缓存测试需要能中途 db.Close()，
// 用文件库可以直接把「DB 不可用」当成「有没有真的查库」的探针）。
func newSessionTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // 幂等：中途 Close 过的库再 Close 返回 nil
	return db
}

// seedSession 建一个用户 + 一个会话。
// last_seen 故意写成「1 小时前」：节流测试要靠它确定性地等待异步写入落地
// （写进去的值必然 > 旧值，waitLastSeen(initial+1) 才能证明「真的写了一次」）。
func seedSession(t *testing.T, db *store.DB, username string, disabled bool, expiresAt int64) (*store.User, *store.Session) {
	t.Helper()
	u := &store.User{ID: store.NewUserID(), Username: username, Disabled: disabled, CreatedAt: store.Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	s := &store.Session{
		ID:         store.NewSessionID() + ".tok",
		UserID:     u.ID,
		DeviceName: "Test · macOS",
		CreatedAt:  store.Now(),
		ExpiresAt:  expiresAt,
		LastSeen:   store.Now() - 3600,
	}
	if err := db.CreateSession(s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return u, s
}

// sessionMWResult 是一次中间件调用的可观测结果。
type sessionMWResult struct {
	code      int
	body      string
	userID    string // 注入 context 的 userID（未放行时为空）
	setCookie string // 响应的 Set-Cookie（禁用路径要清 Cookie）
	reached   bool   // 是否真的进到了下游 handler
}

// runSessionMW 跑一次「带会话 Cookie 的请求」经过 SessionManager.Middleware。
func runSessionMW(m *SessionManager, sessionID string) sessionMWResult {
	var got sessionMWResult
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.reached = true
		got.userID = UserIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	if sessionID != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionID})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	got.code = rec.Code
	got.body = rec.Body.String()
	got.setCookie = rec.Header().Get("Set-Cookie")
	return got
}

// lastSeenOf 读 sessions.last_seen。
func lastSeenOf(t *testing.T, db *store.DB, sessionID string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow(`SELECT last_seen FROM sessions WHERE id = ?`, sessionID).Scan(&v); err != nil {
		t.Fatalf("read last_seen: %v", err)
	}
	return v
}

// setLastSeen 直接把 last_seen 改成指定值（制造「旧值」以便观察刷新）。
func setLastSeen(t *testing.T, db *store.DB, sessionID string, v int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE sessions SET last_seen = ? WHERE id = ?`, v, sessionID); err != nil {
		t.Fatalf("set last_seen: %v", err)
	}
}

// totalChanges 返回本连接累计修改行数。
// TouchSession 是唯一的写点，所以「前后差值」就是实际落库的 UPDATE 次数
// （SQLite 的 UPDATE 即使写入相同的值也会计入 total_changes，因此计数不会漏）。
func totalChanges(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT total_changes()`).Scan(&n); err != nil {
		t.Fatalf("total_changes: %v", err)
	}
	return n
}

// waitLastSeen 轮询等 last_seen 涨到 atLeast（触摸是异步写，测试必须等它落地）。
// 超时即视为「永久不写」——这正是节流不能犯的错，所以直接 t.Fatalf。
func waitLastSeen(t *testing.T, db *store.DB, sessionID string, atLeast int64) int64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v := lastSeenOf(t, db, sessionID); v >= atLeast {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("3s 内 last_seen 没有写到 >= %d（节流只允许少写，不允许永久不写）", atLeast)
	return 0
}

// sleepToNextSecond 睡到「下一个整秒边界 + 100ms」。
// last_seen 是整数秒，节流比较也按秒走，所以只有跨过整秒才可能观察到新的写入。
func sleepToNextSecond() {
	next := time.Now().Truncate(time.Second).Add(time.Second + 100*time.Millisecond)
	time.Sleep(time.Until(next))
}

// ---------- 1. 命中缓存时零 DB 往返 ----------

func TestSessionCacheHitSkipsDB(t *testing.T) {
	db := newSessionTestDB(t)
	u, s := seedSession(t, db, "cache-hit", false, store.Now()+3600)
	// TTL 故意设得很短（500ms），用来同时证明「TTL 边界之后必须重新查库」。
	// 不用 50ms 这种量级：CI/race 下有偶发卡顿，几百微秒的停顿就会让「命中」变成假故障。
	m := NewSessionManagerTuned(db, time.Hour, false, 500*time.Millisecond)

	// 第一次：未命中 → GetSession + GetUserByID 两次往返，然后写缓存。
	got := runSessionMW(m, s.ID)
	if got.code != http.StatusOK || got.userID != u.ID {
		t.Fatalf("首次请求应放行: code=%d uid=%q want %q", got.code, got.userID, u.ID)
	}
	if entries, hits, misses := m.CacheStats(); entries != 1 || hits != 0 || misses != 1 {
		t.Fatalf("首次请求后应 entries=1 hits=0 misses=1, got entries=%d hits=%d misses=%d", entries, hits, misses)
	}

	// 把 DB 关掉：之后任何一次真查库都必然失败，所以「仍然 200」== 命中缓存（0 次 DB 往返）。
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	got = runSessionMW(m, s.ID)
	if got.code != http.StatusOK || got.userID != u.ID {
		t.Fatalf("缓存热时 DB 关闭仍应放行（证明 0 次 DB 往返）: code=%d uid=%q body=%s", got.code, got.userID, got.body)
	}
	if _, hits, misses := m.CacheStats(); hits != 1 || misses != 1 {
		t.Fatalf("第二次应命中: hits=%d misses=%d (want 1/1)", hits, misses)
	}

	// TTL 边界：过期后必须重新查库，DB 已关 → fail-closed 401（缓存不是永久放行）。
	time.Sleep(600 * time.Millisecond)
	got = runSessionMW(m, s.ID)
	if got.code != http.StatusUnauthorized {
		t.Fatalf("缓存 TTL 过后应重新查库并因 DB 关闭而 401, got code=%d body=%s", got.code, got.body)
	}
	if got.reached {
		t.Fatalf("401 不应进入下游 handler")
	}
}

// ---------- 2. revoke 立即失效 ----------

func TestSessionRevokeInvalidatesCache(t *testing.T) {
	db := newSessionTestDB(t)
	_, s := seedSession(t, db, "revoke-me", false, store.Now()+3600)
	m := NewSessionManager(db, time.Hour, false) // 默认 10s 缓存

	if got := runSessionMW(m, s.ID); got.code != http.StatusOK {
		t.Fatalf("预热请求应放行, got %d", got.code)
	}
	if entries, _, _ := m.CacheStats(); entries != 1 {
		t.Fatalf("预热后应缓存 1 条, got %d", entries)
	}

	if err := m.Revoke(s.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if entries, _, _ := m.CacheStats(); entries != 0 {
		t.Fatalf("Revoke 后必须同步清缓存, got entries=%d", entries)
	}
	got := runSessionMW(m, s.ID)
	if got.code != http.StatusUnauthorized {
		t.Fatalf("Revoke 后应立即 401（不等 TTL）, got code=%d body=%s", got.code, got.body)
	}
	if !strings.Contains(got.body, "会话无效") {
		t.Fatalf("401 文案应保持「会话无效」: %s", got.body)
	}
}

// ---------- 3. 过期会话 ----------

func TestSessionExpiredRejected(t *testing.T) {
	db := newSessionTestDB(t)
	_, s := seedSession(t, db, "expired", false, store.Now()-3600) // 1 小时前过期
	m := NewSessionManager(db, time.Hour, false)

	got := runSessionMW(m, s.ID)
	if got.code != http.StatusUnauthorized {
		t.Fatalf("过期会话应 401, got %d", got.code)
	}
	if !strings.Contains(got.body, "会话已过期") {
		t.Fatalf("401 文案应保持「会话已过期」: %s", got.body)
	}
	if entries, _, _ := m.CacheStats(); entries != 0 {
		t.Fatalf("过期会话不能进缓存, got entries=%d", entries)
	}
}

// ---------- 4. 禁用用户 ----------

func TestSessionDisabledUserRejected(t *testing.T) {
	db := newSessionTestDB(t)
	_, s := seedSession(t, db, "disabled", true, store.Now()+3600)
	m := NewSessionManager(db, time.Hour, false)

	got := runSessionMW(m, s.ID)
	if got.code != http.StatusUnauthorized {
		t.Fatalf("禁用用户应 401, got %d", got.code)
	}
	if !strings.Contains(got.body, "账户已被禁用") {
		t.Fatalf("401 文案应保持「账户已被禁用」: %s", got.body)
	}
	if got.reached {
		t.Fatalf("禁用用户不能进入下游 handler")
	}
	if !strings.Contains(got.setCookie, SessionCookieName) {
		t.Fatalf("禁用用户应清会话 Cookie（沿用旧行为）, Set-Cookie=%q", got.setCookie)
	}
	if entries, _, _ := m.CacheStats(); entries != 0 {
		t.Fatalf("禁用用户不能进缓存（负结果不缓存）, got entries=%d", entries)
	}
}

// ---------- 5. DB 读错误必须 fail-closed ----------

// TestSessionUserLookupErrorFailsClosed 钉住 #241 §5 的顺带安全缺陷：
// 旧实现是 `if u, err := m.db.GetUserByID(...); err == nil && u.Disabled`，
// GetUserByID 出错时会**静默放行**，属于安全 bug。现在必须 401。
func TestSessionUserLookupErrorFailsClosed(t *testing.T) {
	t.Run("GetSessionError", func(t *testing.T) {
		// 库里根本没有这个会话 → GetSession 失败 → 401。
		db := newSessionTestDB(t)
		seedSession(t, db, "no-session", false, store.Now()+3600)
		m := NewSessionManager(db, time.Hour, false)
		m.InvalidateAll() // 防御：确保没有任何缓存条目（新管理器本来就是空）
		got := runSessionMW(m, "sess_does_not_exist.tok")
		if got.code != http.StatusUnauthorized || got.reached {
			t.Fatalf("不存在的会话应 401 且不进下游, got code=%d reached=%v", got.code, got.reached)
		}
		if !strings.Contains(got.body, "会话无效") {
			t.Fatalf("401 文案应保持「会话无效」: %s", got.body)
		}
	})

	t.Run("GetUserByIDError", func(t *testing.T) {
		// 构造「GetSession 成功但 GetUserByID 失败」：会话行存在、user 行不存在。
		// sessions.user_id 有外键，正常插不进去，所以临时关掉本连接的外键约束
		// （store.Open 的 DSN 是 foreign_keys(ON)，PRAGMA 可运行期切换）。
		db := newSessionTestDB(t)
		if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
			t.Fatalf("pragma off: %v", err)
		}
		orphan := &store.Session{
			ID:         store.NewSessionID() + ".orphan",
			UserID:     "usr_missing_" + store.NewUserID(),
			DeviceName: "Test · Orphan",
			CreatedAt:  store.Now(),
			ExpiresAt:  store.Now() + 3600,
			LastSeen:   store.Now(),
		}
		if err := db.CreateSession(orphan); err != nil {
			t.Fatalf("create orphan session: %v", err)
		}
		if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
			t.Fatalf("pragma on: %v", err)
		}

		m := NewSessionManager(db, time.Hour, false)
		got := runSessionMW(m, orphan.ID)
		if got.code != http.StatusUnauthorized {
			t.Fatalf("GetUserByID 失败必须 401（不许静默放行）, got code=%d body=%s", got.code, got.body)
		}
		if got.reached {
			t.Fatalf("GetUserByID 失败时不能进入下游 handler")
		}
	})
}

// ---------- 6. 节流 + 不丢更新（最重要） ----------

// TestTouchSessionThrottleAndNoLostUpdate 的判据（节流只允许「少写」）：
//
//	(a) 320 次并发请求实际落库次数必须远小于请求数，且 ≥ 1
//	    ——用 SQLite total_changes() 精确计数（UPDATE 即使写同值也计数，不会漏）；
//	(b) last_seen 单调不减，最终值 ≥ 请求发生的那一秒（写的是「新时间」而不是「旧值」）；
//	(c) goroutine 数不爆炸：每请求 spawn 一个异步写的实现会在这里看到 ~320 个；
//	(d) -race 下无数据竞争（由 `go test -race` 保证）。
//
// 不允许的两种退化：写旧值（last_seen 回退）、永久不写（窗口过后仍不落库）。
func TestTouchSessionThrottleAndNoLostUpdate(t *testing.T) {
	t.Run("concurrent", func(t *testing.T) {
		db := newSessionTestDB(t)
		_, s := seedSession(t, db, "touch-concurrent", false, store.Now()+3600)
		m := NewSessionManager(db, time.Hour, false)
		m.touchInterval = 60 * time.Millisecond // 小于 1s 时按整秒生效（last_seen 只有秒精度）

		initial := lastSeenOf(t, db, s.ID)
		// 预热：未命中请求负责写缓存并抢到第一次 touch 写权。
		if got := runSessionMW(m, s.ID); got.code != http.StatusOK {
			t.Fatalf("预热请求应放行, got %d", got.code)
		}
		// initial 是「1 小时前」的旧值，所以 initial+1 只有真的写进去才会满足。
		waitLastSeen(t, db, s.ID, initial+1)
		time.Sleep(50 * time.Millisecond) // 让预热 goroutine 收尾（goroutine 计数基线更稳）

		beforeGoroutines := runtime.NumGoroutine()
		beforeChanges := totalChanges(t, db)
		start := time.Now()

		// 采样器：并发期间反复读 last_seen，用于断言「单调不减」。
		var (
			samplesMu sync.Mutex
			samples   []int64
		)
		stopSampler := make(chan struct{})
		var samplerWG sync.WaitGroup
		samplerWG.Add(1)
		go func() {
			defer samplerWG.Done()
			for {
				select {
				case <-stopSampler:
					return
				default:
				}
				var v int64
				if err := db.QueryRow(`SELECT last_seen FROM sessions WHERE id = ?`, s.ID).Scan(&v); err == nil {
					samplesMu.Lock()
					samples = append(samples, v)
					samplesMu.Unlock()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()

		// 16 goroutine × 20 轮 × 1 请求 = 320 次请求，跨度 ~2s（确保跨过整秒边界）。
		var (
			failsMu  sync.Mutex
			failures []int
		)
		for round := 0; round < 20; round++ {
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if got := runSessionMW(m, s.ID); got.code != http.StatusOK {
						failsMu.Lock()
						failures = append(failures, got.code)
						failsMu.Unlock()
					}
				}()
			}
			wg.Wait()
			time.Sleep(100 * time.Millisecond)
		}
		elapsed := time.Since(start)
		close(stopSampler)
		samplerWG.Wait()
		time.Sleep(200 * time.Millisecond) // 等最后一次异步写入落地

		if len(failures) != 0 {
			t.Fatalf("并发请求里有 %d 个没放行（例如 %d）", len(failures), failures[0])
		}

		writes := totalChanges(t, db) - beforeChanges
		maxWrites := int64(elapsed.Seconds()) + 2 // 每个整秒窗口最多 1 次写
		if writes < 1 {
			t.Fatalf("节流不能变成「永久不写」: %d 次请求后实际写库 0 次", 320)
		}
		if writes > maxWrites {
			t.Fatalf("节流失效：%v 内写库 %d 次（上限 %d，请求数 320）", elapsed, writes, maxWrites)
		}
		if writes >= 320/4 {
			t.Fatalf("节流基本没生效：写库 %d 次 / 320 次请求", writes)
		}

		// 单调不减：采样窗口内的 last_seen 不允许回退（节流不许写旧值）。
		prev := initial
		for i, v := range samples {
			if v < prev {
				t.Fatalf("last_seen 出现回退：第 %d 次采样 %d < 前值 %d（节流不许写旧值）", i, v, prev)
			}
			prev = v
		}
		final := lastSeenOf(t, db, s.ID)
		if final < start.Unix() {
			t.Fatalf("最终 last_seen=%d 应 >= 请求起始秒 %d", final, start.Unix())
		}

		// goroutine 不爆炸：每请求 spawn 一个异步写的实现会多出几百个。
		if after := runtime.NumGoroutine(); after > beforeGoroutines+50 {
			t.Fatalf("goroutine 疑似爆炸：before=%d after=%d（320 请求）", beforeGoroutines, after)
		}
		t.Logf("320 次请求 / %v：落库 %d 次（上限 %d），last_seen %d → %d，采样 %d 次",
			elapsed, writes, maxWrites, initial, final, len(samples))
	})

	t.Run("serial_strict_increase", func(t *testing.T) {
		// 确定性版本：每轮等到下一个整秒再请求，节流窗口一过就必须写、
		// 且写的是新时间（last_seen 严格递增 3 次）。
		db := newSessionTestDB(t)
		_, s := seedSession(t, db, "touch-serial", false, store.Now()+3600)
		m := NewSessionManager(db, time.Hour, false)
		m.touchInterval = 60 * time.Millisecond

		prev := lastSeenOf(t, db, s.ID)
		if got := runSessionMW(m, s.ID); got.code != http.StatusOK {
			t.Fatalf("预热请求应放行, got %d", got.code)
		}
		prev = waitLastSeen(t, db, s.ID, prev+1)

		for round := 1; round <= 3; round++ {
			sleepToNextSecond()
			if got := runSessionMW(m, s.ID); got.code != http.StatusOK {
				t.Fatalf("第 %d 轮请求应放行, got %d", round, got.code)
			}
			cur := waitLastSeen(t, db, s.ID, prev+1)
			if cur <= prev {
				t.Fatalf("第 %d 轮 last_seen 没有严格递增：prev=%d cur=%d", round, prev, cur)
			}
			prev = cur
		}
	})

	t.Run("cache_refresh_keeps_throttle_window", func(t *testing.T) {
		// 回归守卫：缓存条目在 cacheTTL 过期后被**原地刷新**，不能重置节流窗口。
		// 否则活跃会话的 last_seen 写入会从「每 touchInterval 一次」退化成
		// 「每 cacheTTL 一次」——虽然不丢更新，但写放大会回到改造前的一半。
		db := newSessionTestDB(t)
		_, s := seedSession(t, db, "touch-refresh", false, store.Now()+3600)
		m := NewSessionManagerTuned(db, time.Hour, false, 100*time.Millisecond)
		m.touchInterval = time.Hour // 节流窗口远大于测试时长

		initial := lastSeenOf(t, db, s.ID)
		if got := runSessionMW(m, s.ID); got.code != http.StatusOK {
			t.Fatalf("预热请求应放行, got %d", got.code)
		}
		waitLastSeen(t, db, s.ID, initial+1) // 等第一次 touch 落地

		beforeChanges := totalChanges(t, db)
		time.Sleep(150 * time.Millisecond) // 缓存已过 TTL，但节流窗口远未到
		if got := runSessionMW(m, s.ID); got.code != http.StatusOK {
			t.Fatalf("TTL 过期后的请求应放行, got %d body=%s", got.code, got.body)
		}
		time.Sleep(100 * time.Millisecond) // 给「本不该 spawn 的异步写」一点时间暴露
		if writes := totalChanges(t, db) - beforeChanges; writes != 0 {
			t.Fatalf("缓存刷新不应重置节流窗口：这次 miss 竟然写库 %d 次", writes)
		}
		if entries, _, misses := m.CacheStats(); entries != 1 || misses != 2 {
			t.Fatalf("应已原地刷新回缓存: entries=%d misses=%d (want 1/2)", entries, misses)
		}
	})
}

// ---------- 7. 缓存关闭 ----------

func TestSessionCacheDisabled(t *testing.T) {
	db := newSessionTestDB(t)
	_, s := seedSession(t, db, "cache-off", false, store.Now()+3600)
	m := NewSessionManagerTuned(db, time.Hour, false, -1) // <0 → 关闭缓存

	// 让「旧的 last_seen」可观察，好证明缓存关闭时仍保持旧行为：每请求同步刷新。
	setLastSeen(t, db, s.ID, store.Now()-3600)
	if got := runSessionMW(m, s.ID); got.code != http.StatusOK {
		t.Fatalf("首个请求应放行, got %d", got.code)
	}
	if v := lastSeenOf(t, db, s.ID); v < store.Now()-1 {
		t.Fatalf("缓存关闭时应每请求同步刷新 last_seen（保持旧行为）, got %d", v)
	}
	if entries, hits, _ := m.CacheStats(); entries != 0 || hits != 0 {
		t.Fatalf("缓存关闭时不应有条目/命中, got entries=%d hits=%d", entries, hits)
	}

	// DB 关掉 → 立刻失败：证明每次请求真的都走 DB（没有缓存兜底）。
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	got := runSessionMW(m, s.ID)
	if got.code != http.StatusUnauthorized {
		t.Fatalf("缓存关闭时 DB 不可用必须 401, got code=%d body=%s", got.code, got.body)
	}
	if got.reached {
		t.Fatalf("401 不应进入下游 handler")
	}
}

// ---------- 8. 按用户失效（多设备） ----------

func TestSessionInvalidateUser(t *testing.T) {
	db := newSessionTestDB(t)
	u1, s1 := seedSession(t, db, "multi-device", false, store.Now()+3600)
	// 同一用户的第二个设备/会话。
	s2 := &store.Session{
		ID:         store.NewSessionID() + ".tok2",
		UserID:     u1.ID,
		DeviceName: "Test · iPhone",
		CreatedAt:  store.Now(),
		ExpiresAt:  store.Now() + 3600,
		LastSeen:   store.Now(),
	}
	if err := db.CreateSession(s2); err != nil {
		t.Fatalf("create second session: %v", err)
	}
	// 另一个用户，用来确认 InvalidateUser 不会误伤别人。
	_, other := seedSession(t, db, "bystander", false, store.Now()+3600)

	m := NewSessionManager(db, time.Hour, false)
	for _, id := range []string{s1.ID, s2.ID, other.ID} {
		if got := runSessionMW(m, id); got.code != http.StatusOK {
			t.Fatalf("预热请求应放行 (session %s), got %d", id, got.code)
		}
	}
	if entries, _, _ := m.CacheStats(); entries != 3 {
		t.Fatalf("预热后应缓存 3 条, got %d", entries)
	}

	m.InvalidateUser(u1.ID)
	if entries, _, _ := m.CacheStats(); entries != 1 {
		t.Fatalf("InvalidateUser 后应只剩别人的 1 条, got %d", entries)
	}

	// 关掉 DB：该用户的两个会话都失效了（缓存已删 → 必须查库 → fail-closed）。
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	for _, id := range []string{s1.ID, s2.ID} {
		if got := runSessionMW(m, id); got.code != http.StatusUnauthorized {
			t.Fatalf("InvalidateUser 后会话 %s 应失效, got code=%d", id, got.code)
		}
	}
	// 别人的缓存条目还在 → 仍然放行（证明失效范围正确）。
	if got := runSessionMW(m, other.ID); got.code != http.StatusOK {
		t.Fatalf("其它用户不该被误伤, got code=%d body=%s", got.code, got.body)
	}
}
