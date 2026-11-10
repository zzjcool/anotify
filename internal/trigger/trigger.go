// Package trigger 是 v4 的 cron 调度器：到点的源触发一次标准接收（与手动/webhook
// 完全同一条 Ingest 路径），然后按**上次计划时刻**推进下一次触发。
//
// 三条纪律：
//
//   - 触发就是 source.Service.Ingest：没有 cron 专属的派发/幂等/回执逻辑；
//   - 错误不吞：每条源的失败都带源身份返回并记录，互不连坐；
//   - 不热循环：配置无法解析/表达式永不成立时也把 next_fire_at 推到远处并记 error，
//     而不是让它每 tick 都「到点」。
package trigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

const (
	// DefaultTick 是调度循环的节拍。
	DefaultTick = time.Second
	// DefaultBatch 是单轮最多触发的源数量。
	DefaultBatch = 100
	// neverFireHorizon 是「表达式永不成立」时的下一次触发距离（避免每 tick 重试）。
	neverFireHorizon = 366 * 24 * time.Hour
)

// Scheduler 是 cron 源的调度器。
type Scheduler struct {
	db      *store.DB
	sources *source.Service
	log     *slog.Logger
	now     func() int64
}

// New 构造调度器（时钟为真实时钟）。
func New(db *store.DB, sources *source.Service, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{db: db, sources: sources, log: logger, now: func() int64 { return store.Now() }}
}

// NewWithClock 是显式时钟版（测试用）。
func NewWithClock(db *store.DB, sources *source.Service, logger *slog.Logger, now func() int64) *Scheduler {
	s := New(db, sources, logger)
	if now != nil {
		s.now = now
	}
	return s
}

// FireResult 是单条源的触发结果（Err 非空表示这条失败，其它源照常）。
type FireResult struct {
	SourceID    string
	SourceName  string
	NamespaceID string
	Receipt     *source.Receipt
	Err         error
}

// RunDue 触发一轮到点的 cron 源。
//
// 下一次触发从**上次计划时刻**推算（不是 now）：调度器晚醒时不会把错过的窗口叠成
// 一串立刻触发。无论 Ingest 成功与否都会推进 next_fire_at——cron 是时间驱动，
// 「失败就原地重试」会变成热循环；失败本身以 FireResult.Err 返回，不吞。
func (s *Scheduler) RunDue(ctx context.Context, now int64, limit int) ([]FireResult, error) {
	if limit <= 0 {
		limit = DefaultBatch
	}
	if now == 0 {
		now = s.now()
	}
	due, err := s.db.ListDueSources(ctx, now, limit)
	if err != nil {
		return nil, fmt.Errorf("trigger: 列到点源: %w", err)
	}
	results := make([]FireResult, 0, len(due))
	var sweepErr error
	for _, src := range due {
		result := FireResult{SourceID: src.ID, SourceName: src.Name, NamespaceID: src.NamespaceID}
		nextFire := s.nextFireAfter(src, now)
		receipt, ierr := s.sources.Ingest(ctx, source.Input{Source: src, Type: src.Name})
		if ierr != nil {
			result.Err = ierr
			s.log.Warn("cron source fire failed", "source_id", src.ID, "source", src.Name, "err", ierr)
		} else {
			result.Receipt = receipt
		}
		if merr := s.db.MarkSourceFired(ctx, src.ID, now, nextFire); merr != nil {
			result.Err = errors.Join(result.Err, fmt.Errorf("trigger: 推进 %s 的下次触发: %w", src.Name, merr))
			sweepErr = errors.Join(sweepErr, result.Err)
		}
		results = append(results, result)
	}
	return results, sweepErr
}

// nextFireAfter 计算下一次触发时刻（以源的上次计划时刻为基准）。
func (s *Scheduler) nextFireAfter(src *store.Source, now int64) int64 {
	base := time.Unix(now, 0)
	if src.NextFireAt != nil && *src.NextFireAt > 0 {
		base = time.Unix(*src.NextFireAt, 0)
	}
	next, err := sourcespec.NextCronTime(src.Config, base)
	if err != nil {
		s.log.Error("cron expression unusable", "source_id", src.ID, "source", src.Name, "err", err)
		return now + int64(neverFireHorizon/time.Second)
	}
	if next.IsZero() {
		s.log.Error("cron expression never fires", "source_id", src.ID, "source", src.Name)
		return now + int64(neverFireHorizon/time.Second)
	}
	return next.Unix()
}

// Run 按 tick 周期触发，直到 ctx 结束。
func (s *Scheduler) Run(ctx context.Context, tick time.Duration) error {
	if tick <= 0 {
		tick = DefaultTick
	}
	timer := time.NewTicker(tick)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if _, err := s.RunDue(ctx, s.now(), DefaultBatch); err != nil {
				s.log.Warn("cron sweep failed", "err", err)
			}
		}
	}
}
