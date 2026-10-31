package store

// #242 · 只读路径的 per-DB 语句缓存。
//
// 结论分**两层**（本 worktree 上实测 + 直接读驱动源码，不是推测）：
//
//	① 驱动 v1.34.5 时代：*sql.Stmt 复用只省**对象分配**，不省 parse。
//	   `(*conn).query`（SQL 文本直查）**每次调用**都 `c.prepare(...)` 再 `s.Close()`
//	   （sqlite.go:1766），而 `(*stmt).query` / `(*stmt).exec` 同样**每次执行**都重新
//	   `prepareV2(&psql)` 再 finalize（sqlite.go:647 / :530）——该版本
//	   `type stmt struct { c *conn; psql uintptr }` 里的 psql 只是 libc.CString 出来的
//	   **SQL 文本**，**没有编译结果**。`database/sql` 对文本直查也不缓存（它只为
//	   `*sql.Stmt` 按连接懒 prepare 一份副本）。
//	   ⇒ 在 v1.34.5 上，本缓存省掉的是 `newStmt`（libc.CString + 驱动 stmt 对象）的
//	     分配、以及 per-request 的 `PrepareContext` 调用次数，但 `prepareV2` 仍是
//	     每条 SQL 一次：缓存版 pprof 里 prepareV2 cum 40.5%、直查 50.5%，
//	     ns/op 在噪声内持平。**parse 并没有减少。**
//
//	② 驱动 ≥ v1.45.0：parse 收益才真正到手（本仓现在的状态）。
//	   v1.45.0 的 `type stmt struct { c *conn; psql, pstmt uintptr }` 在 `newStmt` 里就
//	   `prepareV2` 一次，把编译好的 handle 存进 `pstmt // The cached SQLite statement
//	   handle`；`(*stmt).query/exec` 只在 `pstmt == 0`（多语句脚本 / 纯注释）时才走
//	   fallback 分支重新 parse。**但那需要复用的 *sql.Stmt**：走 `db.QueryContext` 直查
//	   每次都造一个新 stmt 对象，`newStmt` 还是要 parse 一遍，驱动的 pstmt 缓存根本没
//	   机会命中。
//	   ⇒ 只有「复用 *sql.Stmt」+「驱动 ≥ v1.45.0」两者叠加，per-request 的 parse 才
//	     真的消失（实测 BenchmarkGetSession 18.2–20.3µs → 9.5–10.3µs）。
//
// ⇒ 因此本缓存（#242）的定位是：**驱动升级的必要前置** + 让 per-request 的
//   `PrepareContext` 归零。它单独存在只能省对象分配，驱动升级单独存在也省不了
//   parse（每次都新建 stmt），两者叠加才消掉 per-request 的 parse。
//   落地顺序按这个依赖关系分两个独立 commit/PR：缓存与门禁先进仓库，驱动版本另改。
//   （版本门槛：v1.39.0 还没有 `pstmt` 缓存，v1.45.0 有；v1.45.0 要求 go ≥ 1.24，
//   本仓 go 1.25 满足。）
//
// 为什么缓存键是**最终 SQL 文本**而不是「函数名 + 过滤条件组合」：
//   - 语句的身份就是 SQL 文本本身：同一段文本 → 同一个已编译语句，不用再发明一套
//     映射到 SQL 的中间表示（那种映射一旦与拼串逻辑漂移，就会悄悄把不同查询当成同一条）；
//   - 文本天然有界：每个调用点的可选过滤都在**列名**上做拼接（值一律走占位符 `?`），
//     ListTasks 最多 8 种组合、ListEvents 最多 4 种，全包加起来的种类数是一个小常数。
//   - 反过来说：如果哪天有人把**值**拼进 SQL 文本（那本身就是 SQL 注入洞），键会随值
//     无限膨胀——这正是下面 maxCachedStmts 存在的目的：超上限就退回直查，宁可丢缓存
//     也不让缓存自己变成内存泄漏 + 无界 prepare。
//
// 为什么**只**给只读路径用（不缓存 Exec / 不缓存事务内查询）：
//   - 写路径的语义挂在「这一次执行」上（`RowsAffected`、`LastInsertId`、`INSERT OR IGNORE`
//     的冲突路径），共用一条 *sql.Stmt 会让「这条语句刚刚影响了什么」的边界变模糊，
//     而写路径的成本从来不在 prepare 上（在锁与 fsync 上）；
//   - 事务内语句的生命周期绑定那个 `*sql.Tx`：把 tx 语句塞进 per-DB 缓存，
//     事务一结束（Rollback/Commit）就是一条死语句，之后所有命中都会失败回退——
//     缓存变成纯粹的负担，还多一类「偶发失效」的失败模式。
//
// 生命周期：`sql.DB.Close` 会关掉它创建的全部 stmt，所以缓存本身不需要 Close 钩子；
// 缓存里保留的只是句柄，`*sql.Stmt` 的连接级副本由 database/sql 自己管理
// （每条连接懒 prepare，连接被回收时随之释放）。
//
// ⚠️ 换代注意事项：驱动内部的 pstmt 缓存是**进程内**的，跨驱动版本升级写出来的库文件
// 与本缓存无关（本缓存只影响运行期 parse 次数，不改变任何 SQL 文本或结果）。
// 跨引擎（驱动版本）读老库的行为由 internal/store/migrate_engine_compat_test.go 守住。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
)

// maxCachedStmts 是单个 *DB 的语句缓存上限：超过就**不再缓存新的 SQL 文本**，
// 该条查询退回直查（prepare + execute + close，即改造前的形状）。
//
// 256 这个数远大于任何现有调用点的文本种类数（ListTasks 8 种、ListEvents 4 种，
// 其余全是常量 SQL），所以正常运行时永远不会触顶；它的作用只是一道**兜底闸门**：
// 假设将来有人把值拼进 SQL 文本，缓存也不会无界增长。
const maxCachedStmts = 256

// stmtCache 是 per-DB 的「最终 SQL 文本 → 已 Prepare 语句」表。
//
// 零值可用（map 在第一次写入时才分配）：这样 `&DB{DB: sqlDB}` 这种包内直造的
// DB（测试夹具）天然就带一个能用的缓存，不会因为「忘了初始化」而静默退化成直查。
// 含互斥量，**只能由指针访问**（DB 全程都是 *DB）。
type stmtCache struct {
	mu sync.RWMutex
	m  map[string]*sql.Stmt
}

// newStmtCache 建一个带预分配 map 的缓存（openWithOptions 用）。
func newStmtCache() stmtCache {
	return stmtCache{m: make(map[string]*sql.Stmt, 32)}
}

// get 取已缓存语句；nil 表示未命中。
func (c *stmtCache) get(q string) *sql.Stmt {
	c.mu.RLock()
	st := c.m[q]
	c.mu.RUnlock()
	return st
}

// full 报告是否已达上限（超限后新 SQL 一律走直查，不再 prepare 缓存）。
//
// 它是 cachedStmt 里的**预判**，不是唯一的闸门：为的是「明知存不进去就别去
// prepare 了」（prepare 要借连接、要 parse，做了白准备）。真正的兜底在 store()：
// 它在写锁里又查了一次 len，所以并发下 full() 返回 false、等真到 store() 时
// 已经被别人塞满，也依旧只是「关掉自己这条 + 返回 nil → 调用方退回直查」，
// 绝不会突破 maxCachedStmts。两处检查保留，是因为它们防的是两件事：
// 这一处省白做的 prepare，store() 那一处守住不变式。
func (c *stmtCache) full() bool {
	c.mu.RLock()
	n := len(c.m)
	c.mu.RUnlock()
	return n >= maxCachedStmts
}

// len 返回当前缓存的语句条数（测试/诊断用）。
func (c *stmtCache) len() int {
	c.mu.RLock()
	n := len(c.m)
	c.mu.RUnlock()
	return n
}

// store 把 q → st 放进缓存，返回**最终生效**的那条语句。
//
// 三种结局：
//   - 已有人抢先存了同一个键（并发双 prepare）→ 关掉自己这条，返回先到的（保序、不泄漏）；
//   - 缓存已满 → 关掉自己这条并返回 nil（调用方退回直查，绝不无界缓存 SQL 文本）；
//   - 否则存下并返回自己。
//
// 注意：本函数**不**在 prepare 期间持锁（prepare 由调用方在锁外完成），只在这里短暂持写锁，
// 详见 cachedStmt 的注释。
func (c *stmtCache) store(q string, st *sql.Stmt) *sql.Stmt {
	c.mu.Lock()
	if existing := c.m[q]; existing != nil {
		c.mu.Unlock()
		_ = st.Close()
		return existing
	}
	if len(c.m) >= maxCachedStmts {
		c.mu.Unlock()
		_ = st.Close()
		return nil
	}
	if c.m == nil {
		c.m = make(map[string]*sql.Stmt, 32)
	}
	c.m[q] = st
	c.mu.Unlock()
	return st
}

// evict 踢掉一条已失效的语句。
//
// 只在「命中缓存但语句本身用不了」时调用（连接/库被关）。比较指针再删，
// 避免把并发期间别人刚换上去的新语句误删。
//
// 它的定位是**清表 + 防御**，不是正确性必需项（审查 3A 在 go1.25.0 +
// modernc.org/sqlite v1.45.0 上实测）：库被 Close 之后，命中缓存语句拿到的
// 错误与走直查**逐字相同**（`sql: database is closed`），错误面因此不需要 evict
// 来「修」。evict 真正换来的东西是两件：① 死句柄不会长期占着缓存条目
// （maxCachedStmts 是有限的）；② 只死了语句、库还活着的形状（连接被池回收/
// 超时淘汰时 database/sql 会关掉该连接上的语句副本）能自愈——那次查询退化成
// 直查并成功，下一次再重新 prepare 一条新句柄。
func (c *stmtCache) evict(q string, st *sql.Stmt) {
	c.mu.Lock()
	if c.m[q] == st {
		delete(c.m, q)
	}
	c.mu.Unlock()
	// 已经失效了：Close 的错误没有信息量（通常是 "sql: statement is closed"）。
	_ = st.Close()
}

// cachedQueryContext 是 `d.QueryContext` 的语句缓存版，语义与之一致：
// 返回的 *sql.Rows 由调用方负责 Close，错误原样返回（**不在本层加包装**，
// 各调用点原有的 `fmt.Errorf("list tasks: %w", err)` 保持不变）。
func (d *DB) cachedQueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	st := d.cachedStmt(ctx, q)
	if st == nil {
		// 未命中且没能缓存（prepare 失败 / 缓存已满）：退回直查。
		// 好处是错误文本与「没有缓存时」逐字一致，调用点的包装方式无需任何调整。
		return d.QueryContext(ctx, q, args...)
	}
	rows, err := st.QueryContext(ctx, args...)
	if err == nil {
		return rows, nil
	}
	if !stmtUnusable(err) {
		// 真实的 SQL/参数错误：与直查是同一条错误，原样返回。
		// 这里**不**回退重跑——重跑只会把同一条 SQL 再执行一遍（读路径无害但纯浪费），
		// 而且会让「错误来自哪一次执行」变得不可追。
		return nil, err
	}
	// 缓存里的语句已不可用（连接/库被关掉）：踢掉它，退回直查拿「标准」错误。
	d.stmts.evict(q, st)
	return d.QueryContext(ctx, q, args...)
}

// cachedQueryRowContext 是 `d.QueryRowContext` 的语句缓存版。
//
// 与 QueryContext 的差别：*sql.Row 的错误是**延迟到 Scan 才暴露**的，本层拿不到，
// 所以「语句已失效」这种情况在这里表现为 Scan 返回
// `sql: statement is closed`（而不是直查时的 `sql: database is closed`）——
// 两者都发生在「库已经关了」这种不该再查询的时刻，调用方一律当错误处理，行为不变，
// 也不会 panic。
func (d *DB) cachedQueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	st := d.cachedStmt(ctx, q)
	if st == nil {
		return d.QueryRowContext(ctx, q, args...)
	}
	return st.QueryRowContext(ctx, args...)
}

// cachedStmt 取（必要时 prepare 并缓存）q 对应的语句；nil 表示「这次别用缓存，直查」。
//
// 为什么 prepare 必须发生在**锁外**：连接池是有界（默认 8）的，
// `db.PrepareContext` 要先借一条连接。若持着缓存的写锁去借连接，
// 就会出现这种死锁形状：8 条连接全被「持有 rows 的 goroutine」占着，
// 而它们下一步读新的 SQL 需要这把锁 → 借连接的人等连接、持连接的等锁。
// 所以流程是：读锁查表 → 锁外 prepare → 写锁落表（表里已有就先到的生效，自己这条关掉）。
//
// 代价是**冷启动时**同一条 SQL 会被多 prepare 几次：多出来的份数 = 并发首命中者的数量
// （审查 3A 在本 worktree 上用计数驱动实测，N 个 goroutine 同时首命中同一条 SQL）：
//
//	并发首命中者  8 → driver.Conn.PrepareContext 15 次
//	并发首命中者 32 → driver.Conn.PrepareContext 38 次
//	并发首命中者 64 → driver.Conn.PrepareContext 71 次
//
// 也就是「池上限 + 少量并发重复」，一次冷启动之后就收敛，与请求数无关。
// 缓存**条数恒为 1、不会抖动**：后到的 prepare 被 store() 直接关掉，
// 而 store() 对已存在的键只「忽略」、从不「替换」——不存在互相踢掉、
// 下一轮又要重新 prepare 的活锁形状，也不会泄漏句柄。
// 与「每条 SQL 每个请求 prepare 一次」相比，这仍是数量级的差异。
func (d *DB) cachedStmt(ctx context.Context, q string) *sql.Stmt {
	if st := d.stmts.get(q); st != nil {
		return st
	}
	if d.stmts.full() {
		return nil
	}
	st, err := d.DB.PrepareContext(ctx, q)
	if err != nil {
		// **Prepare 失败不缓存失败结果**：坏 SQL 是调用方的 bug（改代码就修好），
		// 把失败记进缓存只会让「修好之后仍然失败」变成一次重启才能恢复的故障。
		// 这里直接交给调用方退回直查，由直查产生与改造前完全一致的错误。
		return nil
	}
	return d.stmts.store(q, st)
}

// stmtUnusable 判定错误属于「缓存里这条语句本身不能用了」，而不是 SQL/参数/context 的问题。
//
// 只有这一类才值得「踢掉缓存 + 退回直查」：SQL 错误重跑一遍不会变好，
// context 取消/超时重跑只会立刻再失败一次。
//
// "statement is closed" / "database is closed" 只能按文本匹配：database/sql 把它们定义为
// 包内私有错误（errStmtClosed / errDBClosed，都是 `errors.New(...)` 每次新建的普通错误），
// 没有导出哨兵，errors.Is 匹配不到。
func stmtUnusable(err error) bool {
	if err == nil {
		return false
	}
	// 这三条有导出哨兵：errors.Is 精确匹配，不受文本变化影响。
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return true
	}
	msg := err.Error()
	for _, prefix := range stmtUnusablePrefixes {
		// **前缀**匹配而不是子串匹配：见 stmtUnusablePrefixes 的注释（子串有一个
		// 真实的假阳性，用例 TestStmtCacheUnusableDoesNotMatchSqlLogicErrors 钉住它）。
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// stmtUnusablePrefixes 是「缓存里这条语句本身不能用了」的**精确前缀**表。
//
// 两条都是 database/sql 的原文（$GOROOT/src/database/sql/sql.go 里 errStmtClosed /
// errDBClosed 的 errors.New 文本），本 worktree 的用例会打印实测值：
//
//	已关闭的 *sql.Stmt.QueryContext → `sql: statement is closed`
//	已关闭的 *sql.DB 上的语句命中 → `sql: database is closed`
//
// 为什么必须按前缀而不是子串（审查 3A 构造出的真实假阳性）：SQLite 的错误文本里
// 也会出现被引号包住的**表名**，于是「表不存在」这类普通 SQL 错误会命中子串——
//
//	SELECT * FROM "database is closed"
//	→ SQL logic error: no such table: database is closed (1)
//
// 子串匹配会把这条稳定的 SQL bug 误判成「语句句柄失效」，于是每次执行都白 evict
// 一条缓存、再退回直查重跑一次；更糟的是「缓存里那条语句被当成坏句柄」这件事
// 完全不可观测。前缀匹配把判定收回到 database/sql 的标准文本上。
var stmtUnusablePrefixes = [...]string{
	"sql: statement is closed", // errStmtClosed：命中的 *sql.Stmt 已被 Close
	"sql: database is closed",  // errDBClosed：库已 Close，语句跟着失效
}
