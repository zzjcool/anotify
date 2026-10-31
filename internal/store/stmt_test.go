package store

// #242 · 语句缓存的**机器门禁** + 改前/改后基准。
//
// 门禁要钉住的事实（本 worktree 上已实测，见 .pi-legion/contract.md §0）：
//
//	只要调用点写的是 `db.QueryContext(ctx, SQL 文本, ...)`（改造前的形状），
//	**每次**都会新建一个驱动 stmt 对象（modernc.org/sqlite 的 `(*conn).query`
//	内部 `c.prepare` + `s.Close`，sqlite.go:1766），`database/sql` 对文本直查不缓存；
//	⇒ 直查 N 次 = 驱动层 N 次「查询」+ N 个驱动 stmt 对象。
//	而走复用的 *sql.Stmt 后，连接级 PrepareContext 只发生在「这条连接第一次见到
//	这条 SQL」时，总数 ≤ 连接池上限，与请求数无关；N 次执行全落在
//	driver.Stmt.QueryContext 上（驱动因此才有机会复用它的预编译 handle）。
//
// 怎么观测：把一个**计数驱动**包在真驱动外面（driver.Conn 的 PrepareContext /
// QueryContext、driver.Stmt 的 QueryContext 各记一笔），再用
// `sql.Open(计数驱动名, ...)` 打开一个由**正常 Open** 建好的库文件
// —— 生产代码里的驱动名/DSN/池参数一行都不用改，测的就是真实的驱动行为。
//
// 这个文件同时保留**直查对照组**（`*Baseline` 基准 + 直查子测试）：改造前的数字
// 不是从记忆里抄来的，而是同一份代码里一键就能复现的。
//
// ⚠️ 门禁到底在钉什么（别误读成本文件没权断言的东西）：
//
//	本文件断言的是**应用/驱动边界上**的事实——「没有 per-request 的 prepare 调用」：
//	同一条 SQL 跑 N 次时，`driver.Conn.PrepareContext` 只被调「每条连接一次」，
//	其余 N 次都落在已复用的 `driver.Stmt.QueryContext` 上。
//
//	它**不**断言「驱动内部是否又 parseV2 了一次」——那是**驱动版本行为**，会随版本变，
//	在边界上根本观测不到（v1.34.5 的 `(*stmt).query` 每次重新 prepareV2；v1.45.0 在
//	`newStmt` 里预编译并缓存 `pstmt`，命中时不 parse）。把驱动内部行为写进门禁，
//	等于把测试钉死在某一个驱动版本上：升驱动就假红，而升驱动恰恰是本仓库既定的路线。
//	⇒ 「parse 到底省没省」用**基准与 pprof** 给出证据（*Baseline 对照组 + 交付报告），
//	  不用计数驱动去猜驱动内部实现。两层结论见 stmt.go 文件头（#242）。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	sqlite "modernc.org/sqlite"
)

// ---------- 计数驱动：包一层 modernc.org/sqlite ----------

// stmtCounters 是计数驱动的四个计数器。
//
// 区分「连接级 QueryContext」与「语句级 QueryContext」是关键：
// 前者是 `db.QueryContext`（SQL 文本直查，驱动内部每次重新造一个 stmt 对象），
// 后者是 `(*sql.Stmt).QueryContext`（复用已 prepare 的语句）。
//
// prepares 数的是**驱动边界上**的 prepare 次数（database/sql 为每条连接看到新 SQL 时
// 调用一次）——它不等于驱动内部的 parseV2 次数（那是驱动版本行为，边界上不可观测）。
type stmtCounters struct {
	prepares    atomic.Int64 // driver.Conn.PrepareContext 调用次数（驱动边界上的 prepare）
	connQueries atomic.Int64 // driver.Conn.QueryContext 调用次数（= 文本直查，每次都新建驱动 stmt）
	stmtQueries atomic.Int64 // driver.Stmt.QueryContext 调用次数（= 复用语句的执行次数）
	connExecs   atomic.Int64 // driver.Conn.ExecContext 调用次数（本文件不涉及，留作诊断）
}

func (c *stmtCounters) String() string {
	return fmt.Sprintf("PrepareContext=%d connQueryContext=%d stmtQueryContext=%d connExecContext=%d",
		c.prepares.Load(), c.connQueries.Load(), c.stmtQueries.Load(), c.connExecs.Load())
}

// countingDriver 包装真驱动，统计每次调用后在 Open 出来的连接/语句上计数。
//
// 注意：包装层**必须**实现 driver.QueryerContext / ConnPrepareContext / StmtQueryContext
// —— 少了任何一个，database/sql 就会改走「prepare + 旧式 Query」的旁路，
// 那时测到的就不是生产路径了。
type countingDriver struct {
	inner driver.Driver
	c     *stmtCounters
}

func (cd *countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := cd.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, cd: cd}, nil
}

type countingConn struct {
	driver.Conn
	cd *countingDriver
}

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.cd.c.prepares.Add(1)
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		st, err := c.Conn.Prepare(query)
		if err != nil {
			return nil, err
		}
		return &countingStmt{Stmt: st, cd: c.cd}, nil
	}
	st, err := pc.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &countingStmt{Stmt: st, cd: c.cd}, nil
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.cd.c.connQueries.Add(1)
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, fmt.Errorf("计数夹具失效：底层驱动没有实现 driver.QueryerContext（测的就不是生产路径了）")
	}
	return qc.QueryContext(ctx, query, args)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.cd.c.connExecs.Add(1)
	ec, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, fmt.Errorf("计数夹具失效：底层驱动没有实现 driver.ExecerContext")
	}
	return ec.ExecContext(ctx, query, args)
}

type countingStmt struct {
	driver.Stmt
	cd *countingDriver
}

func (s *countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.cd.c.stmtQueries.Add(1)
	sc, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, fmt.Errorf("计数夹具失效：底层语句没有实现 driver.StmtQueryContext")
	}
	return sc.QueryContext(ctx, args)
}

// countingDriverSeq 给每次注册的驱动起唯一名字（sql.Register 对同名会 panic）。
var countingDriverSeq atomic.Int64

// openCountingDB 用一个全新的计数驱动打开 path 指向的库。
//
// DSN 只写 `file:`+path：本文件只跑只读查询，pragma/事务模式无关紧要；
// 池大小对齐生产（defaultMaxOpenConns），这样「Prepare ≤ 池上限」才是有意义的断言。
func openCountingDB(tb testing.TB, path string, counters *stmtCounters) *sql.DB {
	tb.Helper()
	name := fmt.Sprintf("sqlite-counting-%d", countingDriverSeq.Add(1))
	sql.Register(name, &countingDriver{inner: &sqlite.Driver{}, c: counters})
	sqlDB, err := sql.Open(name, "file:"+path+gateDSNPragmas)
	if err != nil {
		tb.Fatalf("open counting db: %v", err)
	}
	tb.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(defaultMaxOpenConns)
	sqlDB.SetMaxIdleConns(defaultMaxOpenConns)
	return sqlDB
}

// newCountingGateDB 用**正常 Open** 建好库（迁移 + 一个命名空间），再关掉，
// 供计数驱动重新打开 —— 生产代码的驱动名/DSN/池参数一律不动。
func newCountingGateDB(t *testing.T) (path, namespaceID string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "gate.db")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	namespaceID, _ = newTestNS(t, seed)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	return path, namespaceID
}

// gateSQL 是门禁用的 SQL：带一个占位符，形状与生产读路径一致（每次都传参）。
const gateSQL = `SELECT count(*) FROM events WHERE namespace_id = ?`

// gateDSNPragmas 是 openWithOptions 里 DSN 后缀的原样拷贝（只读测试用）。
//
// 为什么必须带上 pragma：并发子测试会同时开多条连接，WAL 库首次并发打开时会短暂
// 撞上 SQLITE_BUSY_RECOVERY —— 生产 DSN 靠 busy_timeout(5000) 兜住，这里不抄就会假红。
const gateDSNPragmas = "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"

// scanCount 执行一次查询并把 count 读出来（顺手证明缓存版结果没变）。
func scanCount(t *testing.T, rows *sql.Rows, err error) int {
	t.Helper()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		t.Fatalf("查询没有返回行（门禁 SQL 写错了？）")
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return n
}

// TestStmtCachePrepareGate 是 #242 的门禁本体。
//
// 同一条 SQL 跑 N=200 次：
//   - 直查（改造前的形状）：驱动层 QueryContext == 200（每条 SQL 都过一遍驱动，每次新建 stmt）；
//   - cachedQueryContext：连接级 PrepareContext ≤ 池上限（8），且语句级 QueryContext == 200。
//
// 边界说明：#242 的门禁只能证明「没有 per-request 的 prepare 调用」（语句确实被复用、
// 执行全部落在 driver.Stmt.QueryContext 上）；**不**断言驱动内部还会不会 parseV2——
// 那是驱动版本行为（v1.34.5 会、≥ v1.45.0 命中 pstmt 缓存就不回），边界上观测不到。
// 两层结论与版本门槛见 stmt.go 文件头；CPU 收益由下面 *Baseline 基准与 pprof 报告给证据。
func TestStmtCachePrepareGate(t *testing.T) {
	ctx := context.Background()
	path, nsID := newCountingGateDB(t)
	const n = 200

	t.Run("直查：驱动层查询次数 == N（改造前的现状）", func(t *testing.T) {
		counters := &stmtCounters{}
		d := &DB{DB: openCountingDB(t, path, counters), stmts: newStmtCache()}
		for i := 0; i < n; i++ {
			rows, err := d.QueryContext(ctx, gateSQL, nsID)
			got := scanCount(t, rows, err)
			if got != 0 {
				t.Fatalf("第 %d 次：空库 count = %d, want 0", i+1, got)
			}
		}
		if got := counters.connQueries.Load(); got != n {
			t.Fatalf("驱动层 QueryContext = %d, want %d（直查应每条 SQL 都过一遍驱动）", got, n)
		}
		if got := counters.stmtQueries.Load(); got != 0 {
			t.Fatalf("直查不该走语句级查询，实际 %d 次", got)
		}
		// 连接级 PrepareContext 在这里是 0：sqlite 驱动实现了 driver.QueryerContext，
		// 那条 *内部* prepare 发生在驱动内部（sqlite.go:1766），驱动之外看不见 ——
		// 这正是「必须换成 *sql.Stmt 才省得掉」的原因。
		t.Logf("直查 200 次：%s", counters)
	})

	t.Run("cachedQueryContext：PrepareContext ≤ 池上限，语句级查询 == N", func(t *testing.T) {
		counters := &stmtCounters{}
		d := &DB{DB: openCountingDB(t, path, counters), stmts: newStmtCache()}
		for i := 0; i < n; i++ {
			rows, err := d.cachedQueryContext(ctx, gateSQL, nsID)
			got := scanCount(t, rows, err)
			if got != 0 {
				t.Fatalf("第 %d 次：空库 count = %d, want 0", i+1, got)
			}
		}
		if got := counters.prepares.Load(); got > int64(defaultMaxOpenConns) {
			t.Fatalf("PrepareContext = %d, want <= %d（池上限：语句缓存没命中？）",
				got, defaultMaxOpenConns)
		}
		if got := counters.stmtQueries.Load(); got != n {
			t.Fatalf("语句级 QueryContext = %d, want %d", got, n)
		}
		if got := counters.connQueries.Load(); got != 0 {
			t.Fatalf("缓存命中的查询不该再走文本直查，实际 %d 次", got)
		}
		t.Logf("cachedQueryContext 200 次：%s", counters)
	})

	t.Run("cachedQueryRowContext：PrepareContext ≤ 池上限，语句级查询 == N", func(t *testing.T) {
		counters := &stmtCounters{}
		d := &DB{DB: openCountingDB(t, path, counters), stmts: newStmtCache()}
		for i := 0; i < n; i++ {
			var got int
			if err := d.cachedQueryRowContext(ctx, gateSQL, nsID).Scan(&got); err != nil {
				t.Fatalf("第 %d 次: %v", i+1, err)
			}
			if got != 0 {
				t.Fatalf("第 %d 次：空库 count = %d, want 0", i+1, got)
			}
		}
		if got := counters.prepares.Load(); got > int64(defaultMaxOpenConns) {
			t.Fatalf("PrepareContext = %d, want <= %d", got, defaultMaxOpenConns)
		}
		if got := counters.stmtQueries.Load(); got != n {
			t.Fatalf("语句级 QueryContext = %d, want %d", got, n)
		}
		t.Logf("cachedQueryRowContext 200 次：%s", counters)
	})

	// 并发版：8 条连接各用一次，prepare 依然 ≤ 池上限（与请求数无关的关键性质）。
	t.Run("并发下 PrepareContext 仍 ≤ 池上限", func(t *testing.T) {
		counters := &stmtCounters{}
		d := &DB{DB: openCountingDB(t, path, counters), stmts: newStmtCache()}
		const workers = 8
		const perWorker = n / workers
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					rows, err := d.cachedQueryContext(ctx, gateSQL, nsID)
					if err != nil {
						errs <- err
						return
					}
					var got int
					if !rows.Next() {
						err := rows.Err()
						_ = rows.Close()
						if err == nil {
							err = fmt.Errorf("查询没有返回行")
						}
						errs <- err
						return
					}
					if err := rows.Scan(&got); err != nil {
						_ = rows.Close()
						errs <- err
						return
					}
					_ = rows.Close()
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("并发查询失败: %v", err)
		}
		if got := counters.stmtQueries.Load(); got != workers*perWorker {
			t.Fatalf("语句级 QueryContext = %d, want %d", got, workers*perWorker)
		}
		// 上界取 4×池：并发首轮会出现「同一条 SQL 被多个 goroutine 同时 prepare」
		// （锁外 prepare 的代价，见 stmt.go cachedStmt 注释），多出来的那些会立刻被关掉；
		// 真正要钉住的性质是「prepare 次数 ≪ 查询次数」（这里是 200 次查询、上界 32）。
		if got := counters.prepares.Load(); got > 4*int64(defaultMaxOpenConns) {
			t.Fatalf("PrepareContext = %d, want <= %d（并发下也只是每连接一次）",
				got, 4*defaultMaxOpenConns)
		}
		t.Logf("并发 %d×%d 次：%s", workers, perWorker, counters)
	})
}

// TestStmtCacheBoundedByMaxCachedStmts 钉住上限：超过 maxCachedStmts 的新 SQL 不再缓存
// （退回直查），缓存条数恒定 —— 假设将来有人把值拼进 SQL 文本，也不会无界增长。
func TestStmtCacheBoundedByMaxCachedStmts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	total := maxCachedStmts + 20
	for i := 0; i < total; i++ {
		var got int
		// 每条 SQL 文本都不同（缓存键就是文本本身）。
		if err := db.cachedQueryRowContext(ctx, fmt.Sprintf(`SELECT %d`, i)).Scan(&got); err != nil {
			t.Fatalf("第 %d 条 SQL: %v", i, err)
		}
		if got != i {
			t.Fatalf("第 %d 条 SQL 返回 %d", i, got)
		}
	}
	if got := db.stmts.len(); got != maxCachedStmts {
		t.Fatalf("缓存条数 = %d, want %d（超上限应退回直查，不再增长）", got, maxCachedStmts)
	}
}

// ---------- 让「Prepare 失败」这条分支可测 ----------
//
// 事实（本 worktree 实测，v1.34.5）：旧驱动的 PrepareContext 只分配一个「SQL 文本包装
// 对象」（newStmt → stmt{c, psql}），真正的 parse 发生在执行时的 prepareV2 ——
// 所以这条驱动下坏 SQL 能 prepare 成功、在执行时报错，拿坏 SQL 是造不出 prepare 失败的。
// （v1.45.0 的 newStmt 会预编译，坏 SQL 会直接 prepare 失败——错误文本因此不同，
// 本文件的错误文本断言只比较「直查 vs 缓存」两条路径，不断言跨驱动版本的具体文案。）
// 但生产代码必须扛住 prepare 失败（换驱动/换版本就可能失败），
// 所以用一层故障注入驱动把这条分支钉住。

type prepareFailDriver struct {
	inner  driver.Driver
	failOn string // SQL 文本等于它：PrepareContext 与 QueryContext 都直接失败
}

func (d *prepareFailDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &prepareFailConn{Conn: c, failOn: d.failOn}, nil
}

type prepareFailConn struct {
	driver.Conn
	failOn string
}

var (
	errInjectedPrepare = errors.New("injected: prepare failed")
	errInjectedQuery   = errors.New("injected: query failed")
)

func (c *prepareFailConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if query == c.failOn {
		return nil, errInjectedPrepare
	}
	if pc, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *prepareFailConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if query == c.failOn {
		return nil, errInjectedQuery
	}
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, fmt.Errorf("故障注入夹具失效：底层驱动没有实现 driver.QueryerContext")
	}
	return qc.QueryContext(ctx, query, args)
}

// TestStmtCacheDoesNotCachePrepareFailure 钉住「Prepare 失败不进缓存」：
// 把失败记进缓存，会让「改对了仍然失败」变成一次重启才能恢复的故障。
func TestStmtCacheDoesNotCachePrepareFailure(t *testing.T) {
	ctx := context.Background()
	path, nsID := newCountingGateDB(t)
	const bad = `SELECT count(*) FROM events WHERE namespace_id = 'injected'`

	name := fmt.Sprintf("sqlite-preparefail-%d", countingDriverSeq.Add(1))
	sql.Register(name, &prepareFailDriver{inner: &sqlite.Driver{}, failOn: bad})
	sqlDB, err := sql.Open(name, "file:"+path+gateDSNPragmas)
	if err != nil {
		t.Fatalf("open fault-injecting db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	d := &DB{DB: sqlDB} // 零值缓存也顺便一起跑

	for i := 0; i < 3; i++ {
		if _, err := d.cachedQueryContext(ctx, bad); err == nil {
			t.Fatalf("第 %d 次：prepare 失败必须降级成错误（不得 panic/丢错误）", i+1)
		}
		var n int
		if err := d.cachedQueryRowContext(ctx, bad).Scan(&n); err == nil {
			t.Fatalf("第 %d 次：缓存版 QueryRow 也必须报错，实际 nil", i+1)
		}
	}
	if got := d.stmts.len(); got != 0 {
		t.Fatalf("Prepare 失败的结果不得进缓存，实际缓存 %d 条", got)
	}

	// 失败只影响那一条：同一个 DB 上的正常 SQL 依旧能缓存。
	for i := 0; i < 3; i++ {
		var n int
		if err := d.cachedQueryRowContext(ctx, gateSQL, nsID).Scan(&n); err != nil {
			t.Fatalf("第 %d 次：正常 SQL 应能查询: %v", i+1, err)
		}
	}
	if got := d.stmts.len(); got != 1 {
		t.Fatalf("正常 SQL 应被缓存 1 条，实际 %d 条", got)
	}
}

// TestStmtCacheErrorTextMatchesDirectQuery 钉住「缓存版遇到坏 SQL 时，错误文本与直查逐字相同」。
//
// 覆盖面要说清楚（别读成「两条路径在驱动这一层等价」）：当前驱动（v1.45.0 的 newStmt 会
// 预编译）上，坏 SQL 在 cachedStmt 的 PrepareContext 就失败 → cachedStmt 返回 nil →
// **退回直查**。所以「文本逐字相同」是**回退路径**的产物，不是「缓存里那条语句抛出了
// 同一条错误」（后者根本执行不到）。
// 这条用例真正的价值就是把那条回退钉住：一旦有人把「prepare 失败 → 退直查」改成
// 「prepare 失败 → 立刻返回自定义错误」，文本就会漂、调用点上已有的
// `fmt.Errorf("...: %w")` 包装也会叠成双层。
// 「缓存语句在执行时抛错」那一面不在本用例覆盖范围内，见
// TestStmtCacheUnusableDoesNotMatchSqlLogicErrors（用注入驱动把错误造在语句执行层）。
func TestStmtCacheErrorTextMatchesDirectQuery(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const bad = `SELECT * FROM table_that_does_not_exist`

	_, directErr := db.QueryContext(ctx, bad)
	_, cachedErr := db.cachedQueryContext(ctx, bad)
	if directErr == nil || cachedErr == nil {
		t.Fatalf("坏 SQL 必须报错：直查 %v / 缓存 %v", directErr, cachedErr)
	}
	if directErr.Error() != cachedErr.Error() {
		t.Fatalf("错误文本不一致：\n直查 %q\n缓存 %q", directErr.Error(), cachedErr.Error())
	}

	var n int
	directRowErr := db.QueryRowContext(ctx, bad).Scan(&n)
	cachedRowErr := db.cachedQueryRowContext(ctx, bad).Scan(&n)
	if directRowErr == nil || cachedRowErr == nil {
		t.Fatalf("坏 SQL 的 QueryRow 必须在 Scan 时报错：直查 %v / 缓存 %v", directRowErr, cachedRowErr)
	}
	if directRowErr.Error() != cachedRowErr.Error() {
		t.Fatalf("QueryRow 错误文本不一致：\n直查 %q\n缓存 %q", directRowErr.Error(), cachedRowErr.Error())
	}
}

// TestStmtCacheZeroValueWorks 钉住「零值缓存可用」：
// 包内直造的 `&DB{DB: sqlDB}`（不填缓存字段）也必须能查询，不能因为 map 为 nil 而 panic。
func TestStmtCacheZeroValueWorks(t *testing.T) {
	real, _ := newTestDBFile(t)
	z := &DB{DB: real.DB} // 故意不初始化 stmts：零值 map 为 nil
	var n int
	if err := z.cachedQueryRowContext(context.Background(),
		`SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("零值缓存的 DB 应能查询: %v", err)
	}
	if got := z.stmts.len(); got != 1 {
		t.Fatalf("缓存条数 = %d, want 1", got)
	}
}

// TestStmtCacheUnusableStmtFallsBack 钉住「库关掉之后缓存版不会 panic、不会吞错误」：
// 缓存里的语句全部失效，两次调用都必须返回错误（第二次连 prepare 都不成）。
//
// ⚠️ 别把下面那句 `len() == 0` 读成「Row 版也会 evict」（很容易误读，所以写明）：
// 缓存被清空**全部来自 Query 版**。cachedQueryRowContext 拿到的是 *sql.Row，错误延迟到
// Scan 才暴露，本层看不见 —— 那一分支的结构上**没有** evict 的机会（它连判定都拿不到）。
// 同一个循环体里先跑的 cachedQueryContext 拿到 `sql: database is closed`（前缀匹配命中）
// → evict → 退回直查拿标准错误，这才是 len 归零的原因。
// 「Row 版不会 evict」本身也被约束住了：库关后第二次调用依旧报错（不是成功），
// 因为它的 prepare 也不可能成功。
func TestStmtCacheUnusableStmtFallsBack(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	var n int
	if err := db.cachedQueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("首次查询: %v", err)
	}
	if got := db.stmts.len(); got != 1 {
		t.Fatalf("缓存条数 = %d, want 1", got)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for i := 0; i < 2; i++ {
		// 行内顺序是有意义的：先 Query 版（看得到错误 → 判定 → evict → 退直查），
		// 再 Row 版（延迟错误，不 evict；此轮它的 prepare 已经不可能成功）。
		if _, err := db.cachedQueryContext(ctx, `SELECT count(*) FROM events`); err == nil {
			t.Fatalf("第 %d 次：库已关闭，cachedQueryContext 应返回错误", i+1)
		}
		var m int
		if err := db.cachedQueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&m); err == nil {
			t.Fatalf("第 %d 次：库已关闭，cachedQueryRowContext.Scan 应返回错误", i+1)
		}
	}
	if got := db.stmts.len(); got != 0 {
		t.Fatalf("失效语句应被踢出缓存（清空来自上面循环里的 Query 版），实际还剩 %d 条", got)
	}
}

// ---------- stmtUnusable 的边界：SQL 逻辑错误不得被误判成「句柄失效」 ----------

// TestStmtCacheUnusableDoesNotMatchSqlLogicErrors 钉住 stmtUnusable 的**判定边界**。
//
// 背景（审查 3A 构造出的真实假阳性）：旧实现用 strings.Contains 做**子串**匹配，
// 而 SQLite 的错误文本里会原样带上被引号包住的表名，于是——
//
//	SELECT * FROM "database is closed"
//	→ SQL logic error: no such table: database is closed (1)
//	                            ^^^^^^^^^^^^^^^^^^^ 子串命中 → 误判「语句失效」
//
// 后果是两条都不好察觉的：
//   - 缓存里那条**好语句**被当成坏句柄踢掉（下一次要重新 prepare）；
//   - 查询被退回直查**重跑一遍**，把「一条稳定的 SQL bug」变成「每次多跑一条查询」。
//
// 现在改成对 database/sql 的两条标准文本做**前缀**匹配（见 stmt.go stmtUnusablePrefixes）。
//
// 三段断言，分别对应那条判定的三个可观测面：
//
//	① 真 SQLite 造出的带引号表名错误：stmtUnusable == false，
//	   且缓存版返回的错误文本与直查逐字相同（调用点的 `fmt.Errorf("...: %w")` 不用改）；
//	② 注入驱动（错误造在**语句执行层**，才是判定点）：SQL 逻辑错误文本 →
//	   错误原样冒出来、缓存条目**还在**（没 evict、没回退重跑）；
//	③ 对照组：真正的 `sql: statement is closed` → evict + 退回直查并成功。
//	   这两条一起把「前缀匹配」的边界钉死：多认一个字或退回子串匹配，② 立刻变红。
func TestStmtCacheUnusableDoesNotMatchSqlLogicErrors(t *testing.T) {
	ctx := context.Background()

	// ---------- ① 真 SQLite 的错误文本 ----------
	db := newTestDB(t)
	for _, q := range []string{
		`SELECT * FROM "database is closed"`,
		`SELECT * FROM "statement is closed"`,
	} {
		_, directErr := db.QueryContext(ctx, q)
		_, cachedErr := db.cachedQueryContext(ctx, q)
		if directErr == nil || cachedErr == nil {
			t.Fatalf("%s：应报「表不存在」：直查 %v / 缓存 %v", q, directErr, cachedErr)
		}
		if directErr.Error() != cachedErr.Error() {
			t.Fatalf("%s：错误文本不一致：\n直查 %q\n缓存 %q", q, directErr.Error(), cachedErr.Error())
		}
		if stmtUnusable(directErr) {
			t.Fatalf("%s 的实测错误 %q 被判成「句柄失效」——这是子串匹配的假阳性："+
				"表名恰好是那句话，不是 database/sql 的 errStmtClosed/errDBClosed", q, directErr.Error())
		}
		t.Logf("① %s → stmtUnusable=false；错误原文 = %q", q, directErr.Error())
	}
	if got := db.stmts.len(); got != 0 {
		t.Fatalf("坏 SQL 不该进缓存，实际 %d 条", got)
	}

	// ---------- ②③ 判定点：错误真的从「已缓存的语句」抛出来 ----------
	// 为什么需要注入：v1.45.0 的 newStmt 会预编译，所以上面的坏 SQL 在 PrepareContext
	// 就失败、根本进不了缓存 —— 「命中缓存语句 → 执行报错 → 判定」这条路径上
	// 看不到 SQL 逻辑错误。注入驱动把错误造在 driver.Stmt.QueryContext，
	// 即与生产完全同一个判定点（见 stmt.go cachedQueryContext 的 stmtUnusable 分支）。
	t.Run("②SQL 逻辑错误：不判失效、不 evict、错误原样返回", func(t *testing.T) {
		path, nsID := newCountingGateDB(t)
		const logicErr = `SQL logic error: no such table: database is closed (1)`
		d := openInjectedStmtErrDB(t, path, logicErr)

		for i := 0; i < 2; i++ {
			rows, err := d.cachedQueryContext(ctx, gateSQL, nsID)
			if rows != nil {
				_ = rows.Close()
				t.Fatalf("第 %d 次：SQL 逻辑错误必须原样返回，不该有 rows", i+1)
			}
			if err == nil {
				t.Fatalf("第 %d 次：注入的 SQL 逻辑错误必须冒出来（判断成失效时它会被回退直查吞掉）", i+1)
			}
			if err.Error() != logicErr {
				t.Fatalf("第 %d 次：错误被包装了：%q, want %q（本层不加包装，调用点自己的 fmt.Errorf 才加）",
					i+1, err.Error(), logicErr)
			}
			if got := d.stmts.len(); got != 1 {
				t.Fatalf("第 %d 次：SQL 逻辑错误被当成句柄失效了（缓存条数 = %d, want 1）——"+
					"误判会白 evict 一条好语句并多直查一次", i+1, got)
			}
		}
		t.Logf("② 注入 %q → 两次调用都原样返回错误、缓存条目未被动（旧子串实现会在这里变成无错 + 缓存 0 条）", logicErr)
	})

	t.Run("③对照：真 statement is closed → evict + 退回直查并成功", func(t *testing.T) {
		path, nsID := newCountingGateDB(t)
		const closedErrText = "sql: statement is closed"
		d := openInjectedStmtErrDB(t, path, closedErrText)

		// 第一次：语句级报「已关闭」→ stmtUnusable 判真 → evict → 退回直查 → 0 行的库拿到真结果。
		rows, err := d.cachedQueryContext(ctx, gateSQL, nsID)
		if got := scanCount(t, rows, err); got != 0 {
			t.Fatalf("退回直查的结果 = %d, want 0（空的 events 表）", got)
		}
		if n := d.stmts.len(); n != 0 {
			t.Fatalf("命中「%s」应 evict 掉那条语句，缓存条数 = %d, want 0", closedErrText, n)
		}

		// 第二次：缓存已空 → 重新 prepare（这条新语句又被注入成「已关闭」）→ 再次 evict + 退直查。
		// 关键性质：仍然**成功**返回，没把失效锁成持久失败。
		rows, err = d.cachedQueryContext(ctx, gateSQL, nsID)
		if got := scanCount(t, rows, err); got != 0 {
			t.Fatalf("第二次退回直查的结果 = %d, want 0", got)
		}
		t.Logf("③ 注入 %q → 判失效、evict、退回直查成功（两次都不 panic、不吞成错）", closedErrText)
	})
}

// stmtErrDriver 在**语句级** QueryContext 上注入一条固定错误（prepare 与连接级查询全部放行）。
//
// 它只服务上面 ②③ 两个子测试：把错误造在「已缓存的语句执行时」——那是 stmtUnusable
// 唯一会被调用的地方，而真 SQLite 的坏 SQL 在 v1.45.0 上根本到不了那里（预编译阶段就失败）。
type stmtErrDriver struct {
	inner driver.Driver
	msg   string
}

func (d *stmtErrDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &stmtErrConn{Conn: c, msg: d.msg}, nil
}

type stmtErrConn struct {
	driver.Conn
	msg string
}

func (c *stmtErrConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		st, err := c.Conn.Prepare(query)
		if err != nil {
			return nil, err
		}
		return &stmtErrStmt{Stmt: st, msg: c.msg}, nil
	}
	st, err := pc.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &stmtErrStmt{Stmt: st, msg: c.msg}, nil
}

type stmtErrStmt struct {
	driver.Stmt
	msg string
}

// QueryContext 一律返回注入的错误：这就是「缓存里那条语句执行时报错」的形状。
func (s *stmtErrStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return nil, errors.New(s.msg)
}

// QueryContext 是**连接级**查询（= 文本直查），必须放行给真驱动。
//
// 少了它，database/sql 对直查会走「prepare + 语句级 Query」旁路，而那条旁路也被
// 注入成失败——于是就分不清「回退真的发生了」和「回退也被注入挡住了」，
// ③ 对照组要的「退回直查就成功」也就无从观测（本文件第一版就是这么红的）。
func (c *stmtErrConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, fmt.Errorf("注入夹具失效：底层驱动没有实现 driver.QueryerContext（直查就测不到了）")
	}
	return qc.QueryContext(ctx, query, args)
}

// openInjectedStmtErrDB 用注入驱动打开 path（驱动名每次唯一：sql.Register 对同名会 panic）。
func openInjectedStmtErrDB(t *testing.T, path, msg string) *DB {
	t.Helper()
	name := fmt.Sprintf("sqlite-stmterr-%d", countingDriverSeq.Add(1))
	sql.Register(name, &stmtErrDriver{inner: &sqlite.Driver{}, msg: msg})
	sqlDB, err := sql.Open(name, "file:"+path+gateDSNPragmas)
	if err != nil {
		t.Fatalf("open stmt-error db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(defaultMaxOpenConns)
	sqlDB.SetMaxIdleConns(defaultMaxOpenConns)
	return &DB{DB: sqlDB, stmts: newStmtCache()}
}

// ---------- 基准：改前/改后在同一份代码里可复现 ----------
//
// `BenchmarkListTasks` / `BenchmarkGetSession` 走缓存版（本批改造后）；
// `*Baseline` 后缀的两条走**直查**（改造前的形状：SQL 文本与 scan 完全一致，
// 唯一差别是「语句复不复用」）。
//
// 跑法：
//
//	go test ./internal/store/ -run '^$' -bench 'ListTasks|GetSession' -benchmem -count=3

// listTasksBenchSQL 与 ListTasks 在**无过滤**参数下拼出来的文本逐字一致。
const listTasksBenchSQL = `SELECT ` + taskColumns + ` FROM tasks WHERE namespace_id = ?` +
	` ORDER BY created_at DESC, id DESC LIMIT ?`

// getSessionBenchSQL 与 GetSession 的 SQL 文本逐字一致。
const getSessionBenchSQL = `SELECT id, user_id, device_name, created_at, expires_at, last_seen FROM sessions WHERE id = ?`

// stmtBenchEnv 是两条读路径基准共用的夹具：内存库 + N 行任务 + 1 个会话。
type stmtBenchEnv struct {
	db        *DB
	namespace string
	sessionID string
}

// benchTaskRows 是 ListTasks 基准的数据量下限（契约 §2 要求 ≥2000 行）。
const benchTaskRows = 2000

func newStmtBenchEnv(tb testing.TB, tasks int) *stmtBenchEnv {
	tb.Helper()
	ctx := context.Background()
	// 内存库：基准要的是 store 层自身成本，不该混进磁盘 fsync 噪声。
	db, err := Open(":memory:")
	if err != nil {
		tb.Fatalf("open bench db: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	u := &User{ID: NewUserID(), Username: "bench-" + NewUserID(), DisplayName: "bench", CreatedAt: Now()}
	ns, err := db.RegisterUser(ctx, u)
	if err != nil {
		tb.Fatalf("register bench user: %v", err)
	}
	// tasks.capability_id 有外键（Open 的 DSN 开着 foreign_keys），必须先造父行。
	cap := &CapabilityVersion{
		NamespaceID: ns.ID, Name: "bench.taskcache", Version: 1,
		ParamsSchema: json.RawMessage(`{}`), Outputs: []string{"ok"}, CreatedAt: Now(),
	}
	if err := db.CreateCapability(ctx, cap); err != nil {
		tb.Fatalf("create bench capability: %v", err)
	}

	// 分批（每批一个事务）写入；created_at 递减，让 ORDER BY 有真实的选择性。
	const batchSize = 100
	base := Now()
	for start := 0; start < tasks; start += batchSize {
		end := start + batchSize
		if end > tasks {
			end = tasks
		}
		batch := make([]*Task, 0, end-start)
		for i := start; i < end; i++ {
			batch = append(batch, &Task{
				NamespaceID:  ns.ID,
				TraceID:      fmt.Sprintf("tr_bench_%06d", i),
				CapabilityID: cap.ID,
				Capability:   cap.Name,
				Version:      1,
				CreatedAt:    base - int64(i),
			})
		}
		if err := db.CreateTasks(ctx, batch); err != nil {
			tb.Fatalf("create bench tasks: %v", err)
		}
	}

	sess := &Session{
		ID: NewSessionID(), UserID: u.ID, DeviceName: "bench",
		CreatedAt: Now(), ExpiresAt: Now() + 3600, LastSeen: Now(),
	}
	if err := db.CreateSession(sess); err != nil {
		tb.Fatalf("create bench session: %v", err)
	}
	return &stmtBenchEnv{db: db, namespace: ns.ID, sessionID: sess.ID}
}

// BenchmarkListTasks 是改造后的 /v1/tasks 读路径（LIMIT 200，2000 行数据）。
func BenchmarkListTasks(b *testing.B) {
	env := newStmtBenchEnv(b, benchTaskRows)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts, err := env.db.ListTasks(ctx, env.namespace, "", "", "", 200)
		if err != nil {
			b.Fatalf("ListTasks: %v", err)
		}
		if len(ts) != 200 {
			b.Fatalf("ListTasks 返回 %d 行，want 200（基准跑空了？）", len(ts))
		}
	}
}

// BenchmarkListTasksBaseline 是**改造前**的形状：同一条 SQL 文本直查 + 同样的 scanTask。
// 与 BenchmarkListTasks 的差值就是语句缓存省下的 prepare/parse。
func BenchmarkListTasksBaseline(b *testing.B) {
	env := newStmtBenchEnv(b, benchTaskRows)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := env.db.QueryContext(ctx, listTasksBenchSQL, env.namespace, 200)
		if err != nil {
			b.Fatalf("直查 ListTasks SQL: %v", err)
		}
		n := 0
		for rows.Next() {
			if _, err := scanTask(rows); err != nil {
				_ = rows.Close()
				b.Fatalf("scan task: %v", err)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			b.Fatalf("rows: %v", err)
		}
		_ = rows.Close()
		if n != 200 {
			b.Fatalf("直查返回 %d 行，want 200", n)
		}
	}
}

// BenchmarkGetSession 是改造后的会话校验读路径（每个已登录请求一次）。
func BenchmarkGetSession(b *testing.B) {
	env := newStmtBenchEnv(b, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := env.db.GetSession(env.sessionID)
		if err != nil {
			b.Fatalf("GetSession: %v", err)
		}
		if s.ID != env.sessionID {
			b.Fatalf("GetSession 拿到 %q，want %q", s.ID, env.sessionID)
		}
	}
}

// BenchmarkGetSessionBaseline 是**改造前**的形状：同一条 SQL 文本直查 + 同样的 Scan。
func BenchmarkGetSessionBaseline(b *testing.B) {
	env := newStmtBenchEnv(b, 1)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s Session
		err := env.db.QueryRowContext(ctx, getSessionBenchSQL, env.sessionID).Scan(
			&s.ID, &s.UserID, &s.DeviceName, &s.CreatedAt, &s.ExpiresAt, &s.LastSeen)
		if err != nil {
			b.Fatalf("直查 GetSession SQL: %v", err)
		}
		if s.ID != env.sessionID {
			b.Fatalf("直查拿到 %q，want %q", s.ID, env.sessionID)
		}
	}
}
