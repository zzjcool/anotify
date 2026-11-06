package lease

// 运行期统计维护（#265）的**调度**：把 `store.MaintainStats` 挂到现有的 5s 巡检 ticker 上，
// 按低频门（默认 6 小时）决定要不要真正执行。
//
// 为什么需要它：`PRAGMA optimize=0x2` 只在 `Open` 时跑一次，而且它**只补「缺失」不治「过期」**
// （表上有索引但没有 stat1 条目）。某个命名空间的 tasks 从 100 行涨到 100 万行时，
// `sqlite_stat1` 里可能仍记着 100，规划器据此选错计划——表现是"索引明明建好了但慢查询
// 神秘地出现"。实测（`store.TestMaintainStatsRefreshesDriftedStat1`）：100 → 2500 行（25×）
// 之后调 `PRAGMA optimize=0x2` 返回空建议集、stat1 仍是 100，所以**光调 optimize 是不够的**。
//
// 真正干活的是 `store.MaintainStats` 里的确定性漂移判定（stat1 行数估计 vs 真实行数，
// 差 ≥10 倍就 `ANALYZE "tasks"`）；这一层只负责**频率**：默认 6h 一次，不要变成每 5s 一次。
//
// 与保留清理的关系：两者都挂在同一条 ticker 上、都用低频门，但**互不阻塞**
// （各自异步、各自的重叠守卫），慢清理不会拖慢统计维护，反之亦然。

import (
	"context"
	"log/slog"
	"time"
)

// StatsStore 是统计维护需要的数据层能力（*store.DB 满足）。
type StatsStore interface {
	// MaintainStats 按需刷新统计（含 tasks 的行数漂移检查），返回实际执行的语句。
	MaintainStats(ctx context.Context) ([]string, error)
}

// DefaultStatsInterval 是统计维护的常态周期（6 小时）。
//
// 取值理由：统计漂移是**缓慢**的（行数翻倍量级），而 ANALYZE 的成本随表增长。
// 6 小时意味着一天最多 4 次；对"数据量翻倍后规划器选错索引"这类问题足够及时，
// 又不会把 ANALYZE 变成常态负载。
const DefaultStatsInterval = 6 * time.Hour

// maybeMaintainStats 是 Tick 里的低频门：每 5s 便宜地看一眼是否到点。
//
// 禁用方式：`StatsInterval < 0`（或 `Stats` 为 nil）。刻意用**负数**表示禁用而不是 0：
// 0 在这里的自然含义是"没设置，用默认值"，把它当禁用会让"忘了赋值"变成静默关闭。
func (s *Sweeper) maybeMaintainStats(ctx context.Context) {
	if s.Stats == nil || s.StatsInterval < 0 {
		return
	}
	interval := s.StatsInterval
	if interval == 0 {
		interval = DefaultStatsInterval
	}
	now := s.now()
	if s.lastStats != 0 && now-s.lastStats < int64(interval.Seconds()) {
		return
	}
	// 重叠守卫：上一轮还在跑就跳过本轮（统计维护慢时不该排队堆积）。
	if !s.statsBusy.CompareAndSwap(false, true) {
		slog.Debug("stats maintenance still running, skip this tick",
			"event", "lease.stats.skip_busy")
		return
	}
	s.lastStats = now

	go func() {
		defer s.statsBusy.Store(false)
		// 用独立的 context：Tick 的 ctx 可能随一次请求/一轮巡检结束而取消，
		// 但统计维护一旦开始就该做完（它很短，代价是几条 PRAGMA/ANALYZE）。
		ran, err := s.Stats.MaintainStats(context.WithoutCancel(ctx))
		if err != nil {
			slog.Warn("stats maintenance failed (不影响正确性，规划器可能暂时选不到最优索引)",
				"event", "lease.stats.failed",
				"error", err.Error(),
			)
			return
		}
		if len(ran) > 0 {
			// 真刷了才吭一声（常态是空）。**这里是 Debug**：`store.MaintainStats` 对
			// 「漂移触发」那一路已经用同一个事件名（`lease.stats.refreshed`）打过一条
			// **Info**，带 prev_rows/cur_rows——同一件事不该在日志里出现两条 Info。
			slog.Debug("stats maintenance finished",
				"event", "lease.stats.refreshed",
				"statements", ran,
			)
		}
	}()
}
