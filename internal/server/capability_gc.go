package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 已删能力（capability tombstone）的物理回收驱动（issue 003）。
//
// 它只是**驱动器**：删哪些行、什么算「还有引用」全部在 store 的
// PurgeDeletedCapabilities 里（tasks / task_attempts / task_artifact_manifests /
// deployments / playbook_installs 任一引用即不删）。这里负责三件事：
//
//  1. **限速**：单条 DELETE 最多 batch 行、单轮最多 maxBatches 批（约 20000 行），
//     因为 purge 仍在 `BEGIN IMMEDIATE` 里扫 `capability_versions` 的全部 tombstone、
//     按安装数扫回执的 `json_tree` 并临时 B 树排序（三个 capability_id 引用列已走索引，
//     #628；剩下的扫描归 #551）——不设上限就是让一次启动补扫把写锁攥到事件入口开始 503。
//  2. **不丢账**：单批失败返回「已删数量 + 包装错误」。吞掉已删数会让运维在
//     「这轮到底删了没有」上只能猜（同 lease.RunOnce / retention.drainRetention 的约定）。
//  3. **干净收尾**：Run 挂在 App.runBackgroundLoop 上，ctx 取消即返回 nil，
//     关库顺序（cancel → WaitBackground → db.Close）才有确定的 happens-before。
//
// 为什么不在 lease 的 30s 巡检里跑：全表扫 + 写锁不该与事件清理同频（issue 003 裁决 2）。
type capabilityGC struct {
	db         *store.DB
	log        *slog.Logger
	batch      int // 单条 DELETE 的行数上限（1000，与 store.DefaultRetentionBatch 同口径）
	maxBatches int // 单轮的批数上限（20 ≈ 20000 行/轮，避免长写锁）
}

// capabilityGCBatch / capabilityGCMaxBatches 是默认预算，只在 newCapabilityGC 里出现一次。
//
// 数字与 store.DefaultRetentionBatch / DefaultRetentionBatches 逐字相同：同一场库里
// 两个「每批多少、每轮几批」不该是两套口径，否则调了一处、另一处还在按旧值抢锁。
const (
	capabilityGCBatch      = 1000
	capabilityGCMaxBatches = 20
)

// newCapabilityGC 造一个回收驱动；log 为 nil 时用 slog.Default()。
func newCapabilityGC(db *store.DB, log *slog.Logger) *capabilityGC {
	if log == nil {
		log = slog.Default()
	}
	return &capabilityGC{
		db:         db,
		log:        log,
		batch:      capabilityGCBatch,
		maxBatches: capabilityGCMaxBatches,
	}
}

// RunOnce 跑一轮回收：多批直到某批返回 0（清干净）或预算用尽。
//
// 返回值为本轮**实际删掉的行数**，包括中途失败前已提交的批次；错误按批包装。
// ctx 已取消时立刻返回（一行都不删）——关停期间不该再开新的写事务。
func (g *capabilityGC) RunOnce(ctx context.Context) (int64, error) {
	var deleted int64
	// 删除总数 > 0 就打一条 Info：tombstone 回收是**破坏性**动作，运维必须能在
	// 日志里看到「什么时候删了多少行」，否则事后只能靠猜（同 artifact-gc 的记账口径）。
	// 失败轮次也打：此时已删数只在这里和 Run 的 Warn 里各出现一次，不会掩盖失败。
	defer func() {
		if deleted > 0 {
			g.log.Info("已删能力（tombstone）物理回收",
				"event", "server.capability.gc.deleted",
				"deleted", deleted,
			)
		}
	}()

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for i := 0; i < g.maxBatches; i++ {
		// 批间检查：一轮要跑 20 批，取消要能立刻生效（不能等整轮跑完）。
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		n, err := g.db.PurgeDeletedCapabilities(ctx, g.batch)
		deleted += n
		if err != nil {
			// 已删数量必须跟着错误一起回传（单批失败 ≠ 本轮没删）。
			return deleted, fmt.Errorf("capability gc: 第 %d 批物理回收失败（已删 %d 行）: %w", i+1, deleted, err)
		}
		if n == 0 {
			// 没有可回收的 tombstone：清干净了，不必再开写事务。
			return deleted, nil
		}
	}
	// 预算用尽：本轮最多删到 batch*maxBatches 行，剩下的下一轮继续（宁可少删，
	// 也不要在写锁里一次扫全表）。
	return deleted, nil
}

// Run 在 ticker 上重复 RunOnce，直到 ctx 结束（ctx.Done → 干净退出返回 nil）。
//
// 单 goroutine：Run 自己阻塞在 select 上，不额外起 goroutine。
// 单轮失败只记 Warn（下轮会重试），不停摆——除非是 ctx 取消。
// interval <= 0 直接返回：调用方（mux）已按 >0 判断过，这里是防线而不是策略。
func (g *capabilityGC) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n, err := g.RunOnce(ctx)
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				// 取消打断的轮次：按「ctx.Done → 干净退出」处理，不再等下一轮。
				return nil
			}
			g.log.Warn("已删能力物理回收轮次失败（下一轮重试）",
				"event", "server.capability.gc.failed",
				"deleted", n,
				"error", err.Error(),
			)
		}
	}
}
