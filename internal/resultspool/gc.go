package resultspool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// 默认单轮 GC 处理上限（元数据一次最多返回多少个待清理 id）。
const defaultGCLimit = 256

// PurgeStore 是 GC 需要的元数据能力（由 internal/store 实现，本包不 import 它）。
//
// 两段式语义：
//   - ExpireObjects 只**列举**已到保留期终点的对象 id（并原子地把它们标成过期），不动字节；
//   - AckPurged 在字节确实删掉之后回执，元数据据此把对象行标成已清理。
//
// 中间失败就是「元数据说过期了，但字节还在」——下一轮 ExpireObjects 会再列出来重删，
// 所以 Remove 必须幂等，且 Remove 失败时**绝不** Ack（否则字节永远漏在盘上）。
type PurgeStore interface {
	// ExpireObjects 返回本轮待清理的对象 id（最多 limit 个）。
	ExpireObjects(ctx context.Context, now int64, limit int) ([]string, error)
	// AckPurged 确认某个 id 的字节已删除。
	AckPurged(ctx context.Context, id string, now int64) error
}

// GCOptions 是 GC 的显式配置。
type GCOptions struct {
	// Interval 是 Run 的 ticker 周期；<=0 → NewGC 报错（有界驱动必须有正周期，
	// 否则不是「定时清理」而是忙轮询）。
	Interval time.Duration
	// Limit 是单轮最多处理的 id 数；<=0 → 默认 256。
	Limit int
	// Now 返回当前 unix 秒；nil → time.Now().Unix()。
	Now func() int64
	// Log 是 GC 的日志出口；nil → slog.Default()。
	Log *slog.Logger
}

// GC 是按周期把「已过期」对象从托管池里删掉的驱动。
//
// 单 goroutine、单 ticker：RunOnce 是同步的一轮，Run 只是在 ticker 上重复 RunOnce。
type GC struct {
	store    PurgeStore
	spool    *Store
	interval time.Duration
	limit    int
	now      func() int64
	log      *slog.Logger
}

// GCReport 是一轮 GC 的计数（Expired = 本轮列举到的 id 数）。
type GCReport struct {
	Expired int
	Purged  int
	Failed  int
}

// NewGC 构造 GC 驱动并校验配置（不做任何 IO，也不启动 goroutine）。
//
// Interval <= 0 → 报错；Limit <= 0 → 256；Now/Log nil → 默认实现。
// store/spool 为 nil → 报错（缺任何一半都没法「删字节 + 回执」）。
func NewGC(st PurgeStore, spool *Store, opts GCOptions) (*GC, error) {
	if st == nil {
		return nil, errors.New("resultspool: NewGC 需要 PurgeStore")
	}
	if spool == nil {
		return nil, errors.New("resultspool: NewGC 需要 spool Store")
	}
	if opts.Interval <= 0 {
		return nil, fmt.Errorf("resultspool: GC 周期 %s 非法（必须为正）", opts.Interval)
	}
	g := &GC{
		store:    st,
		spool:    spool,
		interval: opts.Interval,
		limit:    opts.Limit,
		now:      opts.Now,
		log:      opts.Log,
	}
	if g.limit <= 0 {
		g.limit = defaultGCLimit
	}
	if g.now == nil {
		g.now = func() int64 { return time.Now().Unix() }
	}
	if g.log == nil {
		g.log = slog.Default()
	}
	return g, nil
}

// RunOnce 跑一轮清理，顺序固定（先列举、再删字节、最后回执）：
//
//  1. ExpireObjects 报错 → 直接返回，**一个文件都不动**（元数据不可信时不动字节）；
//  2. 逐个 id Remove 字节：失败 → Failed++ 且**不 Ack**（下一轮还能再试），继续下一个；
//  3. Remove 成功 → AckPurged；失败 → Failed++，继续；
//  4. 只有「删字节 + 回执」都成功才进 Purged；有任何失败 → errors.Join 返回非 nil；
//  5. Expired 恒等于本轮列举到的 id 数；
//  6. ctx 已取消 → 尽快返回 ctx 错误（已处理完的部分照实记账）。
func (g *GC) RunOnce(ctx context.Context) (GCReport, error) {
	var rep GCReport
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	now := g.now()
	ids, err := g.store.ExpireObjects(ctx, now, g.limit)
	if err != nil {
		return rep, fmt.Errorf("resultspool: 列举过期对象: %w", err)
	}
	rep.Expired = len(ids)
	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("resultspool: GC 被中断，剩余 %d 个对象未处理: %w", rep.Expired-rep.Purged-rep.Failed, err))
			break
		}
		if err := g.spool.Remove(id); err != nil {
			rep.Failed++
			errs = append(errs, fmt.Errorf("resultspool: 删除对象 %q: %w", id, err))
			// 不 Ack：字节还在，回执会让它永久漏在盘上。
			continue
		}
		if err := g.store.AckPurged(ctx, id, now); err != nil {
			rep.Failed++
			errs = append(errs, fmt.Errorf("resultspool: 回执对象 %q: %w", id, err))
			continue
		}
		rep.Purged++
	}
	return rep, errors.Join(errs...)
}

// Run 在 ticker 上重复 RunOnce，直到 ctx 结束（ctx.Done → 干净退出返回 nil）。
//
// 单 goroutine：Run 自己阻塞在 select 上，不额外起 goroutine；
// 单轮失败只记 Warn（下轮会重试），不终止驱动——除非是 ctx 取消。
func (g *GC) Run(ctx context.Context) error {
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			rep, err := g.RunOnce(ctx)
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				// 取消打断的轮次：按「ctx.Done → 干净退出」处理，不再等下一轮。
				return nil
			}
			g.log.Warn("resultspool: GC 轮次失败（下一轮重试）",
				"expired", rep.Expired, "purged", rep.Purged, "failed", rep.Failed, "err", err)
		}
	}
}
