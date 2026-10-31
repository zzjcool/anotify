package store

// #242 · 语句缓存「失效路径」的补测（reviewer 3A）。
//
// stmt.go 的 cachedQueryContext 里有一条分支**只写在设计说明里、没有用例覆盖**：
//
//	st.QueryContext(...) 返回「语句已关闭」   —— database/sql 的标准错误
//	  → stmtUnusable(err) == true             —— 判定它属于「句柄失效」，不是 SQL/参数/context 问题
//	  → d.stmts.evict(q, st)                  —— 比较指针再删，顺带把死句柄关掉
//	  → 退回 d.QueryContext 直查               —— 拿标准错误 / 正常结果
//
// 本文件逐环钉住这条链路（四段断言见 TestStmtCacheEvictsUnusableStmt 的分步注释）。
//
// # 造失效的方式：只关 *sql.Stmt，不关 *sql.DB
//
// 包内测试可以直接从缓存里取到那条语句（`d.stmts.get(q)`），于是能精确地把
// 「缓存里的句柄」单独弄死，而库、连接池、数据全都还活着。
//
// 为什么选这一种，而不是「关库」（TestStmtCacheUnusableStmtFallsBack 已经那样做了）：
//   - evict 分支真正要证明的是「踢掉死句柄之后**查询恢复了**」，而恢复只有在库还活着时
//     才可观测；库一关，直查也失败，「恢复了」和「一起坏了」在断言上分不开。
//   - 只关语句也确实是生产里唯一能触达 evict 的形状：database/sql 在连接被回收时
//     （maxLifetime / 池内驱逐）会关掉它在该连接上的语句副本，缓存里保留的 *sql.Stmt
//     因此可能变成死句柄，而库本身完好。
//
// # 实测（go1.25.0 + modernc.org/sqlite v1.45.0，本 worktree 上跑出来的，不是推测）
//
//	已关闭 *sql.Stmt.QueryContext 的错误原文 == `sql: statement is closed`
//
// 出处是 $GOROOT/src/database/sql/sql.go:2725（`(*Stmt).connStmt` 里 `s.closed` 一支），
// 注意它是 **`errors.New(...)` 每次新建**、不是导出哨兵 ⇒ `errors.Is` 根本匹配不到，
// 只能按 database/sql 的标准文本判定 —— stmtUnusable 就是干这件事的，本文件断言它判对了。

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// evictSQL / evictWant：带占位符的单行查询，形状与生产读路径一致（每次传参），
// 且返回一个明确**非零**的值 —— 这样「退回直查拿到正常结果」不会被空结果误判成成功。
const (
	evictSQL  = `SELECT ?`
	evictWant = 4242
)

// TestStmtCacheEvictsUnusableStmt 是失效路径的本体。
//
// 四段断言（按链路顺序，逐步 Fatalf，失败处一眼能看出断在哪一环）：
//
//	① 第二次调用不 panic；
//	② 链路第一环的错误是**标准错误**（`sql: statement is closed`，不是自定义文本），
//	   且 stmtUnusable 判定为 true、这一层没有把它吞成成功；
//	③ 缓存里那条被清掉了（evict 生效）；
//	④ 退回的是**直查**且结果正确，之后再查能拿到**新的**可用语句（没有留在失败态）。
func TestStmtCacheEvictsUnusableStmt(t *testing.T) {
	ctx := context.Background()
	path, _ := newCountingGateDB(t) // 正常 Open 建库（迁移 + 一个命名空间），再用计数驱动重开
	counters := &stmtCounters{}
	d := &DB{DB: openCountingDB(t, path, counters), stmts: newStmtCache()}

	// ---------- 预热：命中一次，把这条 *sql.Stmt 存进缓存 ----------
	rows, err := d.cachedQueryContext(ctx, evictSQL, evictWant)
	if got := scanCount(t, rows, err); got != evictWant {
		t.Fatalf("预热查询返回 %d, want %d", got, evictWant)
	}
	if got := d.stmts.len(); got != 1 {
		t.Fatalf("预热后缓存条数 = %d, want 1", got)
	}
	st := d.stmts.get(evictSQL)
	if st == nil {
		t.Fatalf("预热后缓存里应有 %q 这条语句", evictSQL)
	}
	t.Logf("预热后：缓存 1 条；驱动计数 [%s]", counters)

	// ---------- 造失效：只关语句，库/连接池/数据全部还活着 ----------
	if err := st.Close(); err != nil {
		t.Fatalf("关掉缓存里那条语句: %v", err)
	}

	// ---------- 链路第 1、2 环：实测错误原文 + stmtUnusable 判定（断言 ② 的前半） ----------
	_, closedErr := st.QueryContext(ctx, evictWant)
	if closedErr == nil {
		t.Fatalf("已关闭的 *sql.Stmt.QueryContext 必须返回错误，实际 nil")
	}
	t.Logf("实测：已关闭 *sql.Stmt.QueryContext 的错误原文 = %q", closedErr.Error())
	const wantClosedErr = "sql: statement is closed"
	if closedErr.Error() != wantClosedErr {
		t.Fatalf("实测错误原文 = %q, want %q（database/sql 的标准错误；若 Go/驱动换了文案，以实测为准更新这里，别改 stmtUnusable 的语义）",
			closedErr.Error(), wantClosedErr)
	}
	// 证据：stmtUnusable 对**实测错误**判定为 true，evict 分支才进得去。
	if !stmtUnusable(closedErr) {
		t.Fatalf("stmtUnusable(%q) = false —— evict 分支永远进不去（缓存会被死句柄长期占住），这是真 bug",
			closedErr.Error())
	}
	t.Logf("stmtUnusable(%q) = true（evict 分支的入口判定对实测错误成立）", closedErr.Error())

	// ---------- 第二次调用：必须走 evict + 退回直查 ----------
	// 断言 ①：不 panic。这里**故意不套 defer/recover** —— 真 panic 就该让这条用例红，
	// 而不是被兜住之后看起来"没 panic"。
	connBefore := counters.connQueries.Load()
	stmtBefore := counters.stmtQueries.Load()
	rows, err = d.cachedQueryContext(ctx, evictSQL, evictWant)
	if err != nil {
		// 断言 ② 的后半：这一层不允许把失效包装成自定义错误，也不允许乱回退。
		// 只关了语句、库还开着 ⇒ 设计上就该退回直查**成功**；拿到错误说明回退没生效。
		t.Fatalf("只关了语句、库还开着：退回直查应成功，实际返回错误 %q", err.Error())
	}
	got := scanCount(t, rows, err)
	if got != evictWant {
		t.Fatalf("退回直查的结果 = %d, want %d（不是空、不是错误，必须是真数据）", got, evictWant)
	}

	// 断言 ④ 前半：计数驱动证明这一次真的是**直查**（不是缓存命中、也不是把失败当成功返回）。
	//   - 驱动层连接级 QueryContext +1：文本直查（driver.Conn.QueryContext，每次新建驱动 stmt）；
	//   - 语句级 QueryContext +0：死语句根本没执行成功。
	if delta := counters.connQueries.Load() - connBefore; delta != 1 {
		t.Fatalf("退回直查的驱动层连接级 QueryContext 增量 = %d, want 1", delta)
	}
	if delta := counters.stmtQueries.Load() - stmtBefore; delta != 0 {
		t.Fatalf("已关闭的语句不该执行成功，语句级 QueryContext 增量 = %d, want 0", delta)
	}

	// 断言 ③：缓存里那条被清掉了。
	if still := d.stmts.get(evictSQL); still != nil {
		t.Fatalf("evict 没删掉 %q 的句柄（缓存里还留着死语句）", evictSQL)
	}
	if got := d.stmts.len(); got != 0 {
		t.Fatalf("失效语句应被踢出缓存，实际剩 %d 条", got)
	}
	t.Logf("evict 生效：缓存 0 条，第二次调用走了直查；驱动计数 [%s]", counters)

	// ---------- 断言 ④ 后半：没有留在失败态 ----------
	// 第三次调用：缓存已空，会重新 prepare 一条**新**语句（len 未满）。
	connBefore3 := counters.connQueries.Load()
	stmtBefore3 := counters.stmtQueries.Load()
	rows, err = d.cachedQueryContext(ctx, evictSQL, evictWant)
	if got = scanCount(t, rows, err); got != evictWant {
		t.Fatalf("恢复后的查询返回 %d, want %d", got, evictWant)
	}
	fresh := d.stmts.get(evictSQL)
	if fresh == nil {
		t.Fatalf("第三次调用应重新缓存一条可用语句，实际缓存里没有 %q", evictSQL)
	}
	if fresh == st {
		t.Fatalf("缓存里还是那条已关闭的坏句柄（evict 后又被人塞回去了？）")
	}
	if delta := counters.connQueries.Load() - connBefore3; delta != 0 {
		t.Fatalf("新语句已可用，不该再走文本直查，连接级 QueryContext 增量 = %d, want 0", delta)
	}
	if delta := counters.stmtQueries.Load() - stmtBefore3; delta != 1 {
		t.Fatalf("恢复后应命中新语句，语句级 QueryContext 增量 = %d, want 1", delta)
	}
	t.Logf("恢复后：缓存 1 条（新句柄），第三次调用命中新语句；驱动计数 [%s]", counters)

	// ---------- 对照：库也一起关掉时，链路终点必须是标准错误（断言 ② 的兜底） ----------
	// 「不把语句失效吞成成功」在库还活着时无法直接观测（正确行为就是恢复并成功），
	// 所以用一组库也关掉的对照把它钉死：错误必须原样冒出来，且是 database/sql 的标准文本。
	t.Run("对照：库也关掉时必须报标准错误，不吞成成功", func(t *testing.T) {
		path, _ := newCountingGateDB(t)
		dc := &DB{DB: openCountingDB(t, path, &stmtCounters{}), stmts: newStmtCache()}
		rows, err := dc.cachedQueryContext(ctx, evictSQL, evictWant)
		if got := scanCount(t, rows, err); got != evictWant {
			t.Fatalf("预热查询返回 %d, want %d", got, evictWant)
		}
		if got := dc.stmts.len(); got != 1 {
			t.Fatalf("预热后缓存条数 = %d, want 1", got)
		}
		if err := dc.Close(); err != nil {
			t.Fatalf("关库: %v", err)
		}
		rows, err = dc.cachedQueryContext(ctx, evictSQL, evictWant)
		if rows != nil {
			_ = rows.Close()
		}
		if err == nil {
			t.Fatalf("库已关闭，cachedQueryContext 必须返回错误（不得吞成成功）")
		}
		t.Logf("库关闭后缓存版返回的标准错误 = %q", err.Error())
		if got, want := err.Error(), "sql: database is closed"; got != want {
			t.Fatalf("错误原文 = %q, want %q（database/sql 标准文本；本层不加包装）", got, want)
		}
		if got := dc.stmts.len(); got != 0 {
			t.Fatalf("失效语句应被踢出缓存，实际剩 %d 条", got)
		}
	})
}

// TestStmtCacheFullFallsBackToDirectQuery 钉住上限路径的另一半。
//
// TestStmtCacheBoundedByMaxCachedStmts 已经证明「超上限后 len 不再增长、结果仍正确」，
// 但它是用 :memory: + QueryRow 测的，**没有**证明第 maxCachedStmts+1 条 SQL 走的到底
// 是「直查」还是偷偷又 prepare 了一次、只是没落表。这里用计数驱动把两条性质钉死：
//
//   - 第 maxCachedStmts+1 条：驱动层连接级 QueryContext +1、语句级 +0、
//     **驱动边界 PrepareContext 增量为 0**（超上限连 prepare 都不做，直接直查）；
//   - 缓存条数恒为 maxCachedStmts，且该条 SQL 不在表里。
//
// 构造取舍：值必须是 *sql.Stmt，所以只能真去 prepare maxCachedStmts 条唯一 SQL
// （不能往 map 里塞假键）。实测 256 条 prepare 在文件库上是毫秒级（耗时打到日志里，
// 若哪天 >2s 就该改成更小的构造）；**不**为了测试去改 maxCachedStmts 的常量值。
func TestStmtCacheFullFallsBackToDirectQuery(t *testing.T) {
	ctx := context.Background()
	path, _ := newCountingGateDB(t)
	counters := &stmtCounters{}
	d := &DB{DB: openCountingDB(t, path, counters), stmts: newStmtCache()}

	// ---------- 塞满上限：每条 SQL 文本都不同（缓存键就是文本本身） ----------
	start := time.Now()
	for i := 0; i < maxCachedStmts; i++ {
		rows, err := d.cachedQueryContext(ctx, fmt.Sprintf(`SELECT %d`, i))
		if got := scanCount(t, rows, err); got != i {
			t.Fatalf("第 %d 条 SQL 返回 %d, want %d", i, got, i)
		}
	}
	fillTook := time.Since(start)
	t.Logf("塞满 %d 条缓存耗时 %v", maxCachedStmts, fillTook)
	if fillTook > 2*time.Second {
		t.Fatalf("塞满 %d 条缓存耗时 %v（>2s：该把上限用例改成更小的构造）", maxCachedStmts, fillTook)
	}
	if got := d.stmts.len(); got != maxCachedStmts {
		t.Fatalf("缓存条数 = %d, want %d", got, maxCachedStmts)
	}

	// ---------- 第 maxCachedStmts+1 条：不缓存、仍正确、且是直查 ----------
	over := fmt.Sprintf(`SELECT %d`, maxCachedStmts)
	want := maxCachedStmts
	connBefore := counters.connQueries.Load()
	stmtBefore := counters.stmtQueries.Load()
	prepareBefore := counters.prepares.Load()
	rows, err := d.cachedQueryContext(ctx, over)
	if got := scanCount(t, rows, err); got != want {
		t.Fatalf("超上限的第 %d 条 SQL 返回 %d, want %d（退回直查也必须给正确结果）",
			maxCachedStmts+1, got, want)
	}
	if got := d.stmts.len(); got != maxCachedStmts {
		t.Fatalf("超上限后缓存条数 = %d, want %d（不得继续增长）", got, maxCachedStmts)
	}
	if d.stmts.get(over) != nil {
		t.Fatalf("超上限的 SQL %q 不得进缓存", over)
	}
	if delta := counters.connQueries.Load() - connBefore; delta != 1 {
		t.Fatalf("超上限应退回直查：驱动层连接级 QueryContext 增量 = %d, want 1", delta)
	}
	if delta := counters.stmtQueries.Load() - stmtBefore; delta != 0 {
		t.Fatalf("超上限不该走语句级查询，增量 = %d, want 0", delta)
	}
	if delta := counters.prepares.Load() - prepareBefore; delta != 0 {
		t.Fatalf("超上限不该再 prepare 新语句，驱动边界 PrepareContext 增量 = %d, want 0", delta)
	}
	t.Logf("上限路径：缓存仍 %d 条，第 %d 条走直查；驱动计数 [%s]",
		d.stmts.len(), maxCachedStmts+1, counters)
}
