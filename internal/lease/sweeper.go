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
	"sync"
	"sync/atomic"
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

	// Retention 是保留策略清理能力（#143）。nil = 不清理。
	//
	// **事实：生产路径上这个调度是接上的。** New 里用类型断言自动接线
	// （Store 同时实现 RetentionStore 就启用），而 internal/server/mux.go 传进来的
	// 正是 *store.DB——所以 lease.New(*store.DB) 会让清理调度在进程里真的跑起来。
	// **本轮没有运维 kill switch**：没有 env 开关、没有保留期配置开关，关闭只能改代码
	// （不调用 New，或把本字段置 nil）。是否加开关待上级拍板。
	//
	// **但 events 保留期默认禁用**（store.DefaultEventRetentionSec ==
	// store.EventRetentionDisabled == 0，0 表示禁用、不是「保留 0 秒」）：本版没有开关、
	// 也没有 per-namespace 配置，lease 层**从不**给 store.RetentionOptions 填 EventRetentionSec，
	// store 见到 <=0 就跳过整个 events 阶段并置 EventsDisabled=true——所以本版升级后
	// **不会删任何 events**；等部署级保留期配置 retention_sec 落地后由部署配置开启。
	// tasks 侧则按能力声明生效：默认安装（库里只有内置能力）下不删——
	// 内置能力（store.BuiltinCapabilities）的 lifecycle 没声明 resultTtlSec（0 = 永不清理）；
	// 但一旦有自定义能力声明了 resultTtlSec > 0（经 POST /v1/capabilities 落库），
	// 该能力下的终态 tasks 就会被按 TTL 删（**首轮就可能删 tasks**）。
	//
	// 非 nil 时清理是**异步**的：Tick 只判断是否到点，真正的 DELETE 在独立 goroutine 里跑，
	// 慢清理不会拖住 5s 的租约回收/重派（见 retention.go 的 maybeSweepRetention）。
	Retention RetentionStore
	// RetentionInterval 是保留策略的执行周期；0 → DefaultRetentionInterval（1 小时）。
	// 巡检 tick 是 5s，两者不同频：每 5s 便宜地看一眼是否到点，到点才真删。
	RetentionInterval time.Duration
	// RetentionBatch 是单条 DELETE 的行数上限；0 → store.DefaultRetentionBatch。
	RetentionBatch int
	// RetentionBatches 是每个清理范围每轮最多跑几批；0 → store.DefaultRetentionBatches。
	RetentionBatches int

	// lastRetention 是上次执行保留策略的时刻（unixepoch 秒）；
	// 0 = 还没跑过 → 首轮 tick 立即执行（先排一次 tasks 侧积压；
	// events 阶段本版总是被跳过，见 Retention 字段注释）。
	// 只在 Tick 里读写（单 goroutine），故不加锁；清理 goroutine 不碰它。
	// 同包测试会直接置 0 来逼开低频门（如重叠守卫用例）。
	lastRetention int64

	// retentionBusy 是重叠守卫：true = 上一轮清理还在跑，本轮直接跳过
	// （不排队、不叠加；两轮并发只会互相抢 SQLite 的写锁）。
	// 用 atomic 而不是普通 bool：它在 Tick 的 goroutine 里被 CAS，
	// 在清理 goroutine 的 defer 里被清除。
	retentionBusy atomic.Bool

	// retentionWG 跟踪异步清理的 goroutine（Add 在启动前，Done 在 defer 里）。
	// 不 export：这是实现细节，Run 退出时不必等它（ctx 取消后 store 层自己会收手），
	// 只有同包测试需要 Wait 到清理结束再断言。
	retentionWG sync.WaitGroup
}

// New 构造默认配置的巡检器。
func New(st Store) *Sweeper {
	s := &Sweeper{
		Store:      st,
		Interval:   time.Duration(store.LeaseSweepIntervalSec) * time.Second,
		Now:        store.Now,
		BatchLimit: 100,
	}
	// 自动接线保留策略（#143）：*store.DB 同时实现 RetentionStore。
	// 用断言而不是显式赋值，是为了让**不需要**清理的调用方（不实现 RetentionStore 的
	// 测试 fake）什么都不做也能拿到 nil（no-op），且不必改 server 层的组装代码。
	// 注意：生产路径传的就是 *store.DB，所以线上这个调度总是接上的；但 events 保留期
	// 默认禁用（见 Retention 字段注释），本版不会删 events。
	if rs, ok := st.(RetentionStore); ok {
		s.Retention = rs
	}
	return s
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
// 三个阶段，顺序不能反：
//
//	① 回收过期租约（assigned/acked/running → lease_expired → pending|abandoned）
//	② 重派到期的 pending 任务
//	③ 保留清理（#143）——**异步**，低频门自己控频（默认 1 小时一轮）
//
// ①② 反过来的话，本轮刚被回收的任务要等下一轮（5s）才重派，白白多等一个周期。
//
// ③ 必须异步且放到 ①② 之后：无索引时单轮清理是「整库扫描 + TEMP B-TREE 排序」
// （契约 §6 实测计划），可能跑很久；同步执行会把同一条 goroutine 上的租约回收/重派
// 一起堵住（Tick 与 Run 共用 goroutine，ticker 还会丢 tick）。
func (s *Sweeper) Tick(ctx context.Context) {
	now := s.now()

	// 保留策略（#143）挂在同一条 ticker 上、但按低频门自己决定要不要执行；
	// 到点也只是**启动**一个清理 goroutine，本函数立刻返回。
	// 用 defer 而不是写在末尾：重派阶段有提前 return（没配 Redispatch、
	// 列 pending 失败），那两处都不该连带跳过与它们无关的保留清理。
	defer s.maybeSweepRetention(ctx, now)

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
