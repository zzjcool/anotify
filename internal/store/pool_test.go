package store

// #240 连接池的验收测试。
//
// 修复前：`SetMaxOpenConns(1)` 让**只读查询**也排同一条连接（读并发度 ≈ 1，
// 实测读端点吞吐只有 /health 的 2.5%–11%，混合读写下写 -93%）。
// 修复后：池 = N（默认 8 + `ANOTIFY_DB_MAX_CONNS` 可调），写事务一律
// `BEGIN IMMEDIATE`（DSN `_txlock=immediate`）串行化，锁冲突交给 busy_timeout。
//
// 这组用例要钉住四件事：
//  1. 池大小可配置，且 `:memory:` 恒为 1（SQLite 内存库是每连接一份，>1 就读空库）；
//  2. 读真的能并发（不是「池还是 1」的假绿）；
//  3. 写事务开局即持写锁、写与写排队，但**不挡读者**；
//  4. 高并发混合读写下 `database is locked` 不冒到调用方。

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPoolMaxOpenConnsConfig 池大小：默认值、环境变量覆盖、非法值回落、:memory: 恒 1。
func TestPoolMaxOpenConnsConfig(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"默认", "", defaultMaxOpenConns},
		{"环境变量覆盖", "3", 3},
		{"非法值回落默认", "abc", defaultMaxOpenConns},
		{"0 回落默认", "0", defaultMaxOpenConns},
		{"负数回落默认", "-4", defaultMaxOpenConns},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(maxConnsEnv, c.env)
			db, err := Open(filepath.Join(t.TempDir(), "pool.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()
			if got := db.Stats().MaxOpenConnections; got != c.want {
				t.Fatalf("MaxOpenConnections = %d, want %d（env=%q）", got, c.want, c.env)
			}
		})
	}

	// OpenWithConns 显式指定，优先级高于环境变量；<=0 视作默认。
	t.Run("OpenWithConns 显式指定", func(t *testing.T) {
		t.Setenv(maxConnsEnv, "5")
		db, err := OpenWithConns(filepath.Join(t.TempDir(), "explicit.db"), 2)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()
		if got := db.Stats().MaxOpenConnections; got != 2 {
			t.Fatalf("MaxOpenConnections = %d, want 2", got)
		}
	})
	t.Run("OpenWithConns 非正数视作默认", func(t *testing.T) {
		t.Setenv(maxConnsEnv, "")
		db, err := OpenWithConns(filepath.Join(t.TempDir(), "explicit0.db"), 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()
		if got := db.Stats().MaxOpenConnections; got != defaultMaxOpenConns {
			t.Fatalf("MaxOpenConnections = %d, want %d", got, defaultMaxOpenConns)
		}
	})

	// `:memory:` 必须恒为单连接：SQLite 的内存库是**每连接一份**，
	// 池 >1 时新连接看到的是空库（迁移建的表全部「消失」）。
	t.Run("内存库恒为单连接", func(t *testing.T) {
		t.Setenv(maxConnsEnv, "8")
		db := newTestDB(t) // Open(":memory:")
		if got := db.Stats().MaxOpenConnections; got != 1 {
			t.Fatalf("MaxOpenConnections = %d, want 1（内存库每连接一份）", got)
		}
		// 功能侧证据：迁移建的表在池里取到的连接上必须存在。
		if !tableExists(t, db, "events") {
			t.Fatal("内存库取到的连接看不到迁移建的表（说明拿到了另一条连接 = 另一个空库）")
		}
		mustExec(t, db, `CREATE TABLE pool_mem_probe (a INTEGER)`)
		mustExec(t, db, `INSERT INTO pool_mem_probe (a) VALUES (1)`)
		if got := countRows(t, db, "pool_mem_probe", "1 = 1"); got != 1 {
			t.Fatalf("内存库行数 = %d, want 1", got)
		}
	})
}

// TestPoolReadsRunInParallel 读并发：默认池下能同时握住 4 条连接并同时查询。
//
// 反例子测试（池 = 1）证明上面那条断言不是空转：MaxOpenConns(1) 时第二条连接
// 根本拿不到——这正是修复前读端点「排队」的根因。
func TestPoolReadsRunInParallel(t *testing.T) {
	t.Run("默认池可以同时持有多条读连接", func(t *testing.T) {
		t.Setenv(maxConnsEnv, "")
		db, _ := newTestDBFile(t)
		ctx := context.Background()

		const conns = 4
		held := make([]*sql.Conn, 0, conns)
		for i := 0; i < conns; i++ {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			c, err := db.Conn(cctx)
			cancel()
			if err != nil {
				t.Fatalf("第 %d 条连接拿不到（读仍被串行化？）: %v", i+1, err)
			}
			held = append(held, c)
		}
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()

		var wg sync.WaitGroup
		for _, c := range held {
			wg.Add(1)
			go func(c *sql.Conn) {
				defer wg.Done()
				var n int
				if err := c.QueryRowContext(ctx, `SELECT count(*) FROM schema_version`).Scan(&n); err != nil {
					t.Errorf("并发读失败: %v", err)
				}
			}(c)
		}
		wg.Wait()
	})

	t.Run("池=1 时第二条连接会阻塞（旧病理）", func(t *testing.T) {
		db, err := OpenWithConns(filepath.Join(t.TempDir(), "one.db"), 1)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = db.Close() }()

		ctx := context.Background()
		c1, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("第一条连接: %v", err)
		}
		defer func() { _ = c1.Close() }()

		cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if c2, err := db.Conn(cctx); err == nil {
			_ = c2.Close()
			t.Fatal("池=1 时不该能拿到第二条连接（该用例失去了区分能力）")
		}
	})
}

// TestPoolWriteTxIsImmediateAndDoesNotBlockReaders 写串行化 + 读不被写挡。
//
// `_txlock=immediate` 的两条可观测后果：
//   - 写事务**开局**就持写锁 → 第二个写事务必须排队；
//   - 写者持锁期间读者照常（WAL：读写不互斥）。
//
// 若 DSN 丢了 `_txlock=immediate`（回到 deferred），第二个 `db.Begin()` 会立刻成功，
// 本用例会在「不该拿到写锁」那条断言上红。
func TestPoolWriteTxIsImmediateAndDoesNotBlockReaders(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin write tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// ① 读者不受写锁影响。
	readCtx, cancelRead := context.WithTimeout(ctx, 3*time.Second)
	defer cancelRead()
	var n int
	if err := db.QueryRowContext(readCtx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("写事务持锁期间只读查询应照常成功，实际: %v", err)
	}

	// ② 第二个写事务必须等锁。
	type beginResult struct {
		tx  *sql.Tx
		err error
	}
	done := make(chan beginResult, 1)
	go func() {
		tx2, err := db.Begin()
		done <- beginResult{tx: tx2, err: err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			_ = r.tx.Rollback()
			t.Fatal("第一个写事务还没结束时第二个就拿到了写锁（_txlock=immediate 没生效）")
		}
		t.Fatalf("第二个写事务不该直接失败（应等锁）: %v", r.err)
	case <-time.After(400 * time.Millisecond):
		// 正常：还在等锁。
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback first tx: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("第一个写事务已结束，第二个应立刻成功，实际: %v", r.err)
		}
		if err := r.tx.Rollback(); err != nil {
			t.Fatalf("rollback second tx: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("第一个写事务已结束，第二个仍在等锁")
	}
}

// TestPoolConcurrentWritesNoLockErrors 高并发写 + 并发读：
// 全部写入必须成功，`database is locked` / `SQLITE_BUSY` 不得冒到调用方。
//
// 这是 #240 修复的**安全边界**：池放宽到 8 之后，如果写路径还靠 deferred 事务，
// 这里会稳定出现 `database is locked`（读后升级写无法等待）。
func TestPoolConcurrentWritesNoLockErrors(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	const (
		writers   = 16
		perWriter = 20
	)
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)

	// 并发读者：混合读写下写更容易撞锁。
	stopRead := make(chan struct{})
	var readWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		readWG.Add(1)
		go func() {
			defer readWG.Done()
			for {
				select {
				case <-stopRead:
					return
				default:
				}
				var n int
				if err := db.QueryRowContext(ctx,
					`SELECT count(*) FROM events WHERE namespace_id = ?`, nsID).Scan(&n); err != nil {
					errs <- fmt.Errorf("并发读: %w", err)
					return
				}
			}
		}()
	}

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("evt_pool_%02d_%02d", w, i)
				tx, err := db.Begin()
				if err != nil {
					errs <- fmt.Errorf("writer %d begin: %w", w, err)
					return
				}
				// 事务内**先读后写**：这是生产代码的常见形状（读状态 → 改状态），
				// 也是 deferred 事务在 WAL 下必炸的形状（读到的快照过期 → 升级写时
				// 立刻 SQLITE_BUSY_SNAPSHOT，busy_timeout 帮不上忙）。
				var before int
				if err := tx.QueryRowContext(ctx,
					`SELECT count(*) FROM events WHERE namespace_id = ?`, nsID).Scan(&before); err != nil {
					_ = tx.Rollback()
					errs <- fmt.Errorf("writer %d read-before-write: %w", w, err)
					return
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, created_at)
					 VALUES (?, ?, ?, 'pooltest', 'pool.event', '{}', ?)`,
					id, nsID, "tr_"+id, Now()); err != nil {
					_ = tx.Rollback()
					errs <- fmt.Errorf("writer %d insert %s: %w", w, id, err)
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- fmt.Errorf("writer %d commit %s: %w", w, id, err)
					return
				}
				// 自提交单语句写也一起压：它和显式事务抢同一把写锁。
				if _, err := db.ExecContext(ctx,
					`UPDATE namespaces SET name = name WHERE id = ?`, nsID); err != nil {
					errs <- fmt.Errorf("writer %d autocommit update: %w", w, err)
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(stopRead)
	readWG.Wait()
	close(errs)

	lockErrs := 0
	for err := range errs {
		if strings.Contains(err.Error(), "database is locked") || strings.Contains(err.Error(), "SQLITE_BUSY") {
			lockErrs++
			t.Errorf("写冲突冒到调用方: %v", err)
			continue
		}
		t.Errorf("写/读失败: %v", err)
	}
	if lockErrs > 0 {
		t.Fatalf("%d 个 database is locked / SQLITE_BUSY 错误（busy_timeout 没能兜住）", lockErrs)
	}
	if got, want := countRows(t, db, "events", "source = 'pooltest'"), writers*perWriter; got != want {
		t.Fatalf("落库行数 = %d, want %d（有写入丢失）", got, want)
	}
}

// TestPoolOpenConnsEnvReadByMaxConnsFromEnv 只钉住解析函数本身（不建库）：
// 空/非法/0/负数回落默认，正整数原样。
func TestMaxConnsFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", defaultMaxOpenConns},
		{"1", 1},
		{"32", 32},
		{"0", defaultMaxOpenConns},
		{"-1", defaultMaxOpenConns},
		{"8 ", defaultMaxOpenConns}, // 带空格不是合法整数：宁可回落默认，也不要静默变成「无上限」
		{"x", defaultMaxOpenConns},
	}
	for _, c := range cases {
		t.Setenv(maxConnsEnv, c.env)
		if got := maxConnsFromEnv(); got != c.want {
			t.Errorf("maxConnsFromEnv() with %q = %d, want %d", c.env, got, c.want)
		}
	}
	// 兜底：确保 defaultMaxOpenConns 不会被误改成 0（database/sql 里 0 = 不限制）。
	if defaultMaxOpenConns < 1 {
		t.Fatalf("defaultMaxOpenConns = %d，0 会让 database/sql 变成「不限制连接数」", defaultMaxOpenConns)
	}
}
