// Package trigger 是 v4 的 cron 调度器：到点的源触发一次标准接收（与手动/webhook 同一条 Ingest
// 路径），然后把下一次触发**对齐到 now 之后的第一个计划时刻**。四条纪律：
//
//   - 补发合并：停机/晚醒错过的 N 格只补一次再对齐（逐格追赶的速率恒为「1 格 / tick」，`* * * * *` 永远追不平）；
//   - 投递幂等：触发带 `cron:<sourceID>:<计划时刻>` 作 deliveryID，同一格重复触发只落一条事件；
//   - 错误不吞：每条源的失败带源身份返回并记录，互不连坐；
//   - 不热循环：表达式不可用/永不成立时把 next_fire_at 推到远处并记 error，而不是每 tick 都到点。
package trigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

const (
	// DefaultTick 是调度循环的节拍。
	DefaultTick  = time.Second
	DefaultBatch = 100
	// neverFireHorizon 是「表达式永不成立」时的下一次触发距离（避免每 tick 重试）。
	neverFireHorizon = 366 * 24 * time.Hour
	// maxMissedScan 是「合并了几格」的统计上限（数字只进 FireResult/日志，行为上总是一次触发）。
	maxMissedScan = 1000
)

type Scheduler struct {
	db      *store.DB
	sources *source.Service
	log     *slog.Logger
	now     func() int64
}

func New(db *store.DB, sources *source.Service, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{db: db, sources: sources, log: logger, now: func() int64 { return store.Now() }}
}

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
	// ScheduledAt 是本次触发代表的计划时刻（Unix 秒）：正常节拍下就是到点的 next_fire_at，补发时是被合并掉的最早一格。
	ScheduledAt int64
	// Missed 是合并的格数（含本次，1 = 没错过）；上限 maxMissedScan。
	Missed  int
	Receipt *source.Receipt
	Err     error
}

// RunDue 触发一轮到点的 cron 源：每条到点源本轮最多触发一次（停机错过的 N 格合并成 1 次，归到
// 最早的错过计划时刻），随后 next_fire_at 对齐到 now 之后的第一格。失败也推进——cron 是时间驱动，
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
		scheduledAt, missed, nextFire := s.plan(src, now)
		result.ScheduledAt, result.Missed = scheduledAt, missed
		receipt, ierr := s.sources.Ingest(ctx, source.Input{
			Source:     src,
			Type:       src.Name,
			DeliveryID: deliveryID(src, scheduledAt),
		})
		if ierr != nil {
			result.Err = ierr
			s.log.Warn("cron source fire failed", "source_id", src.ID, "source", src.Name, "err", ierr)
		} else {
			result.Receipt = receipt
		}
		if missed > 1 {
			s.log.Warn("cron catch-up coalesced", "source_id", src.ID, "source", src.Name,
				"scheduled_at", scheduledAt, "missed", missed, "next_fire_at", nextFire)
		}
		if merr := s.db.MarkSourceFired(ctx, src.ID, now, nextFire); merr != nil {
			result.Err = errors.Join(result.Err, fmt.Errorf("trigger: 推进 %s 的下次触发: %w", src.Name, merr))
			sweepErr = errors.Join(sweepErr, result.Err)
		}
		results = append(results, result)
	}
	return results, sweepErr
}

// plan 算出本次触发代表哪个计划时刻、合并了多少格、下一次在哪（严格晚于 now 的第一格，
// 错过的格子不累积成追赶队列）。
func (s *Scheduler) plan(src *store.Source, now int64) (scheduledAt int64, missed int, nextFire int64) {
	scheduledAt = now
	if src.NextFireAt != nil && *src.NextFireAt > 0 {
		scheduledAt = *src.NextFireAt
	}
	missed = s.missedSlots(src, scheduledAt, now)
	next, err := sourcespec.NextCronTime(src.Config, time.Unix(now, 0))
	if err != nil {
		s.log.Error("cron expression unusable", "source_id", src.ID, "source", src.Name, "err", err)
		return scheduledAt, missed, now + int64(neverFireHorizon/time.Second)
	}
	if next.IsZero() {
		s.log.Error("cron expression never fires", "source_id", src.ID, "source", src.Name)
		return scheduledAt, missed, now + int64(neverFireHorizon/time.Second)
	}
	return scheduledAt, missed, next.Unix()
}

// missedSlots 数 [scheduledAt, now] 里有几个计划时刻被合并进这一次触发（≥1，只用于可观测）。
func (s *Scheduler) missedSlots(src *store.Source, scheduledAt, now int64) int {
	if scheduledAt >= now {
		return 1
	}
	count := 1 // scheduledAt 本身就是一格（ListDueSources 只取 next_fire_at <= now 的源）
	at := time.Unix(scheduledAt, 0)
	for count < maxMissedScan {
		next, err := sourcespec.NextCronTime(src.Config, at)
		if err != nil || next.IsZero() || next.Unix() > now {
			break
		}
		at = next
		count++
	}
	return count
}

// deliveryID 是 cron 的投递幂等键 `cron:<sourceID>:<计划时刻 Unix 秒>`：同一格重复触发会命中
// event_deliveries 的保留窗口，回同一条事件、不再建任务（比靠 next_fire_at 单点防重可靠）。
func deliveryID(src *store.Source, scheduledAt int64) string {
	return "cron:" + src.ID + ":" + strconv.FormatInt(scheduledAt, 10)
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
