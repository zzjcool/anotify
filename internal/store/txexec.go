package store

import (
	"context"
	"database/sql"
)

// 本文件是「同一个写操作既能在事务外跑、也能在事务内跑」的唯一底座。
//
// 为什么需要它：原子写入（能力行 + 它的 next 投影）要求「升级闸门 → 建能力 →
// 建边 → 产物」共享**一个**事务。而这些步骤的校验逻辑（引用解析、影响面计算）
// 全都建立在读之上——
// 一旦某一步的读走了 per-DB 的连接池（`d.QueryContext`），它就落在事务**之外**：
//
//	① 看不到本事务刚写进去的行 → 「刚建的能力」在边校验里报「目标不存在」；
//	② 也看不到别的写者尚未提交的中间态 → 校验与提交之间可能出现「检查时成立、
//	   提交后不成立」的窗口（读的是另一个快照）。
//
// 所以规则只有一条：**事务内的读必须走 tx**。实现方式是把这些读函数的
// 接收者从 *DB 放宽成一个最小接口，由 *DB（读走语句缓存）与 txExec（读走 *sql.Tx）
// 各实现一份。写同理。

// queryExec 是 *DB 与 *sql.Tx 共有的最小读写面。
//
// 方法名故意小写：这是包内接口，不是公开契约；外面要继续用 d.ExecContext /
// d.QueryContext（*DB 已经内嵌了 *sql.DB）。
type queryExec interface {
	queryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	execContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ---------- 事务外：*DB ----------

// queryRowContext 走 per-DB 语句缓存（#242，见 stmt.go）。
func (d *DB) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.cachedQueryRowContext(ctx, query, args...)
}

// queryContext 走 per-DB 语句缓存。
func (d *DB) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.cachedQueryContext(ctx, query, args...)
}

// execContext 是 d.ExecContext 的接口化名字（写路径不缓存语句，见 stmt.go 注释）。
func (d *DB) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.ExecContext(ctx, query, args...)
}

// ---------- 事务内：*sql.Tx ----------

// txExec 把一个 *sql.Tx 包成 queryExec。
//
// **绝不能**在事务内用 per-DB 的语句缓存：那些语句绑定的是池里某条连接，
// 与本事务不是同一个快照——用它们读，就是「事务里看不到自己刚写的行」。
type txExec struct{ tx *sql.Tx }

func (t txExec) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

func (t txExec) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

func (t txExec) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}
