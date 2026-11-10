// Package lease 是 v4 的巡检层：租约过期回收、保留期清理、统计与槽位占用投影。
//
// 它只消费 **store 的真实入口**（RecoverLostLeases / RunRetention / MaintainStats /
// FreeSlotsByNode / InstanceStatsByNode / TaskStats），没有旧 v3 的别名层：
//
//   - 租约过期只吊销授权，**不释放物理槽**（reaped 事实由 cancel.ack 或明确的
//     实例观测给出）；非幂等能力的过期执行直接 abandoned，不自动重跑；
//   - 保留期只删 store 里的事件/任务/投递去重记录（TTL=0 永久），不碰用户目录；
//   - 统计/占用是读投影：Total/Busy/Free 都来自真实账本，不按 phase 推断空闲。
package lease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// DefaultInterval 是巡检循环的默认间隔。
const DefaultInterval = 30 * time.Second

// Report 是一轮巡检的结果（部分成功也如实返回：删了就要有人知道）。
type Report struct {
	// LeasesRequeued 是因短暂基础设施失败/幂等重试而回到 pending 的条数。
	LeasesRequeued int `json:"leasesRequeued"`
	// LeasesAbandoned 是直接进入终态的条数（非幂等或取消在飞）。
	LeasesAbandoned int `json:"leasesAbandoned"`
	// Retention 是本轮保留期清理的计数（含各 TTL 与是否被跳过/截断）。
	Retention store.RetentionStats `json:"retention"`
	// StatsRefreshed 是本轮重建了 SQLite 统计的表名（空 = 统计新鲜）。
	StatsRefreshed []string `json:"statsRefreshed"`
}

// Sweeper 是巡检器。
type Sweeper struct {
	db  *store.DB
	log *slog.Logger
}

// NewSweeper 构造巡检器。
func NewSweeper(db *store.DB, logger *slog.Logger) *Sweeper {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sweeper{db: db, log: logger}
}

// RunOnce 跑一轮：过期租约回收 → 保留期清理 → 统计维护。
//
// 三个阶段互不连坐：租约阶段失败仍会尝试保留期（错误用 errors.Join 汇总返回），
// 保留期失败也不吞掉已完成的计数。
func (s *Sweeper) RunOnce(ctx context.Context, now int64) (Report, error) {
	if now == 0 {
		now = store.Now()
	}
	var report Report
	var runErr error

	requeued, abandoned, err := s.db.RecoverLostLeases(ctx, now)
	report.LeasesRequeued = requeued
	report.LeasesAbandoned = abandoned
	if err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("lease: 回收过期租约: %w", err))
	}
	if requeued > 0 || abandoned > 0 {
		s.log.Info("lease sweep recovered lost leases",
			"requeued", requeued, "abandoned", abandoned)
	}

	retention, err := s.db.RunRetention(ctx, now)
	report.Retention = retention
	if err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("lease: 保留期清理: %w", err))
	}
	if retention.EventsDeleted > 0 || retention.TasksDeleted > 0 || retention.DeliveriesDeleted > 0 {
		s.log.Info("retention sweep deleted rows",
			"events", retention.EventsDeleted, "tasks", retention.TasksDeleted,
			"deliveries", retention.DeliveriesDeleted)
	}

	refreshed, err := s.db.MaintainStats(ctx)
	report.StatsRefreshed = refreshed
	if err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("lease: 统计维护: %w", err))
	}
	return report, runErr
}

// Run 按 interval 周期巡检，直到 ctx 结束。
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := s.RunOnce(ctx, store.Now()); err != nil {
				s.log.Warn("lease sweep failed", "err", err)
			}
		}
	}
}

// MarkDisconnected 是「会话死亡」的入口：节点转 offline 并把槽位标成不可派发。
//
// 它**不释放 busy 账本**（断连 != 进程组已回收）：容量在节点回来给出明确实例事实
// 之前保持占用，避免同一槽被派第二条任务。
func (s *Sweeper) MarkDisconnected(ctx context.Context, nodeID string, now int64) error {
	if now == 0 {
		now = store.Now()
	}
	return s.db.MarkNodeOffline(ctx, nodeID, now)
}

// Occupancy 是单节点的真实占用投影。
type Occupancy struct {
	// Total 是申报的槽位数（不看 alive：账本行存在就算一格）。
	Total int `json:"total"`
	// Busy 是仍压着未回收绑定的槽位数。
	Busy int `json:"busy"`
	// Free 是「alive 且没有 busy」的槽位数（派发容量提示）。
	Free int `json:"free"`
}

// OccupancyByNode 读真实槽位账本得到逐节点占用（nodeIDs 为空 → 空 map）。
func (s *Sweeper) OccupancyByNode(ctx context.Context, nodeIDs []string) (map[string]Occupancy, error) {
	out := map[string]Occupancy{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	stats, err := s.db.InstanceStatsByNode(ctx, nodeIDs)
	if err != nil {
		return nil, err
	}
	free, err := s.db.FreeSlotsByNode(ctx, nodeIDs)
	if err != nil {
		return nil, err
	}
	for _, nodeID := range nodeIDs {
		stat := stats[nodeID]
		out[nodeID] = Occupancy{Total: stat.Total, Busy: stat.Busy, Free: free[nodeID]}
	}
	return out, nil
}

// TaskPhaseStats 是一个命名空间的相位/能力/节点计数（读时派生，不落库）。
func (s *Sweeper) TaskPhaseStats(ctx context.Context, namespaceID string) (*store.TaskStats, error) {
	return s.db.TaskStats(ctx, namespaceID)
}

// RetentionConfig 返回当前生效的保留期配置（TTL=0 = 永久）。
func (s *Sweeper) RetentionConfig(ctx context.Context) (*store.SystemConfig, error) {
	return s.db.GetSystemConfig(ctx)
}
