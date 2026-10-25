// Package lease 实现租约巡检的**调度**：周期性地把过期租约回收，并重派
// 退回 pending 的任务。
//
// 为什么单独一个包：`store.SweepExpiredLeases` 只负责「一条过期租约该怎么处理」
// （纯数据层，事务内 CAS），而「多久扫一次、扫完要不要重派、失败怎么退避」
// 是编排逻辑。混在 store 里会让 store 依赖 route（重派要查出口表），
// 循环依赖。
//
// 契约 §3.3 的四个参数：
//
//	租约 TTL    30s
//	心跳间隔    10s
//	巡检周期    5s
//	退避        10s / 60s / 300s
package lease

import (
	"context"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// Store 是巡检需要的数据层能力（*store.DB 满足）。
type Store interface {
	// SweepExpiredLeases 回收过期租约，返回真正被回收的任务 ID。
	SweepExpiredLeases(ctx context.Context, now int64) ([]string, error)
	// ListRedispatchable 列出「已到重试时间、可被重派」的 pending 任务。
	ListRedispatchable(ctx context.Context, now int64, limit int) ([]*store.Task, error)
}

// RedispatchFunc 重派一条 pending 任务。
//
// 返回 (true, nil) 表示已派出去（任务不再是 pending）；
// (false, nil) 表示没派成（没有可用节点等），任务仍 pending，下轮再试。
type RedispatchFunc func(ctx context.Context, task *store.Task) (bool, error)

// Sweeper 周期性执行租约巡检。
type Sweeper struct {
	Store      Store
	Redispatch RedispatchFunc

	// Interval 是巡检周期（默认 5s，契约 §3.3）。
	Interval time.Duration
	// Now 返回当前 unixepoch 秒（测试可注入）。
	Now func() int64
	// BatchLimit 是每轮最多重派多少条（防止一次积压把节点压垮）。
	BatchLimit int
}

// New 构造默认配置的巡检器。
func New(st Store) *Sweeper {
	return &Sweeper{
		Store:      st,
		Interval:   time.Duration(store.LeaseSweepIntervalSec) * time.Second,
		Now:        store.Now,
		BatchLimit: 100,
	}
}

func (s *Sweeper) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return time.Duration(store.LeaseSweepIntervalSec) * time.Second
}

func (s *Sweeper) now() int64 {
	if s.Now != nil {
		return s.Now()
	}
	return store.Now()
}

// Run 启动巡检循环，直到 ctx 取消。
//
// 单个周期失败只记日志并继续下一轮：巡检是**自愈机制**，
// 不能因为一次 DB 抖动就永久停摆（那会让所有租约永远不被回收）。
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval())
	defer t.Stop()

	slog.Info("lease sweeper started",
		"event", "lease.sweeper.start",
		"interval", s.interval().String(),
	)

	for {
		select {
		case <-ctx.Done():
			slog.Info("lease sweeper stopped", "event", "lease.sweeper.stop")
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick 执行一轮巡检（Run 内部调用；测试直接调它，不用等真实 ticker）。
//
// 两个阶段，顺序不能反：
//
//	① 回收过期租约（assigned/acked/running → lease_expired → pending|abandoned）
//	② 重派到期的 pending 任务
//
// 反过来的话，本轮刚被回收的任务要等下一轮（5s）才重派，白白多等一个周期。
func (s *Sweeper) Tick(ctx context.Context) {
	now := s.now()

	reclaimed, err := s.Store.SweepExpiredLeases(ctx, now)
	if err != nil {
		// 不 return：重派阶段仍应执行（上一轮可能留下了到期的 pending）
		slog.Error("sweep expired leases failed",
			"event", "lease.sweeper.sweep_failed",
			"error", err.Error(),
		)
	} else if len(reclaimed) > 0 {
		slog.Info("reclaimed expired leases",
			"event", "lease.sweeper.reclaimed",
			"count", len(reclaimed),
			"task_ids", reclaimed,
		)
	}

	if s.Redispatch == nil {
		return
	}

	pending, err := s.Store.ListRedispatchable(ctx, now, s.batchLimit())
	if err != nil {
		slog.Error("list redispatchable tasks failed",
			"event", "lease.sweeper.list_failed",
			"error", err.Error(),
		)
		return
	}

	var dispatched, skipped int
	for _, task := range pending {
		if ctx.Err() != nil {
			return // 进程在退出，别继续派
		}
		ok, err := s.Redispatch(ctx, task)
		if err != nil {
			slog.Warn("redispatch task failed",
				"event", "lease.sweeper.redispatch_failed",
				"task_id", task.ID,
				"attempts", task.Attempts,
				"error", err.Error(),
			)
			skipped++
			continue
		}
		if !ok {
			skipped++ // 没有可用节点，下轮再试（不动 not_before）
			continue
		}
		dispatched++
	}

	if dispatched > 0 || skipped > 0 {
		slog.Info("redispatch round finished",
			"event", "lease.sweeper.redispatch",
			"dispatched", dispatched,
			"skipped", skipped,
		)
	}
}

func (s *Sweeper) batchLimit() int {
	if s.BatchLimit > 0 {
		return s.BatchLimit
	}
	return 100
}
