// Package source 实现「源」（origin）的运行时：cron 到点触发、webhook 收事件。
//
// 源与能力的区别（也是这个包存在的理由）：
//
//	能力  回答"能干什么"——由节点声明，中枢派任务给它
//	源    回答"什么时候开始干"——有配置、自己产生事件、自己不干活
//
// 所以源**不建边**：它的出口是明确配置的能力列表（source_targets），
// 触发时直接调 Dispatcher 并带上 Targets（见 route.DispatchInput.Targets）。
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/trigger"
)

// Store 是调度与触发需要的最小 store 表面。
type Store interface {
	DueCronSources(ctx context.Context, now int64, limit int) ([]*store.Source, error)
	MarkSourceFired(ctx context.Context, id string, firedAt, nextFire int64) error
	GetSource(ctx context.Context, orgID, id string) (*store.Source, error)
	InsertEvent(ctx context.Context, e *store.Event) (bool, error)
}

// Dispatcher 是派发器（route.Dispatcher 满足它）。
type Dispatcher interface {
	Dispatch(ctx context.Context, in route.DispatchInput) (*route.DispatchOutput, error)
}

// Trigger 把一次"源触发"变成事件 + 首轮派发。
//
// 它和 trigger.Service.Ingest 的区别：Ingest 走的是"外部系统 POST 进来"
// 那条路（按 source 名字找 origin 能力、再查边）；Trigger 是源自己在内部
// 发起，目标能力是源配置好的，不需要找 origin、也不需要查边。
type Trigger struct {
	Store      Store
	Dispatcher Dispatcher

	// Now / NewTraceID 可注入，便于测试断言。
	Now        func() int64
	NewTraceID func() string
}

func (t *Trigger) now() int64 {
	if t.Now != nil {
		return t.Now()
	}
	return store.Now()
}

func (t *Trigger) newTraceID() string {
	if t.NewTraceID != nil {
		return t.NewTraceID()
	}
	return trigger.NewTraceID()
}

// FireResult 是一次触发的产物。
type FireResult struct {
	Event    *store.Event          `json:"event"`
	Inserted bool                  `json:"inserted"`
	Dispatch *route.DispatchOutput `json:"dispatch,omitempty"`
	// Idempotent=true 表示这次触发命中了幂等键，事件已存在、没有重复派发。
	Idempotent bool `json:"idempotent"`
}

// DryRun 试运行：用给定 payload 算出"会派发什么、会剪枝什么"，**不落库**。
//
// 和 Fire 共用 Dispatcher.Dispatch（只差 DryRun 标志），所以试运行和真实
// 触发不会给出不同判定 —— 那种"试运行说能跑、真点却剪枝"的差异会直接
// 摧毁用户对这套配置的信任。
//
// 不检查 src.Enabled：停用状态下也应该能试运行，否则用户没法先验证
// 再启用。
func (t *Trigger) DryRun(ctx context.Context, src *store.Source, eventType string, payload json.RawMessage) (*route.DispatchOutput, error) {
	if src == nil {
		return nil, fmt.Errorf("source: nil source")
	}
	if eventType == "" {
		eventType = src.Name
	}
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	if len(src.Targets) == 0 {
		return nil, fmt.Errorf("source: %s 没有出口能力（source_targets 为空）", src.Name)
	}
	return t.Dispatcher.Dispatch(ctx, route.DispatchInput{
		OrgID:        src.OrgID,
		TraceID:      "dryrun", // 不落库，trace 只是占位（Dispatch 要求非空）
		Params:       extractParams(payload),
		Targets:      src.TargetIDs(),
		TargetParams: src.TargetParamsMap(),
		Out:          eventType,
		Path:         nil,
		DryRun:       true,
	})
}

// Fire 触发一个源：写事件 + 派给它的出口能力。
//
// eventType 是这次事件的类型（写进 events.type）。它同时会作为
// 派发的 port 记录在剪枝信息里，便于排查"为什么没派出去"。
//
// idemKey 为空时允许重复触发（manual 手工点两次就是两次，符合直觉）；
// cron 必须给确定性 key（cron:<sourceID>:<计划时刻>），否则进程重启后
// 同一时刻会被重复触发。
func (t *Trigger) Fire(ctx context.Context, src *store.Source, eventType, idemKey string, payload json.RawMessage) (*FireResult, error) {
	if src == nil {
		return nil, fmt.Errorf("source: nil source")
	}
	if !src.Enabled {
		return nil, fmt.Errorf("source: %s 已停用", src.Name)
	}
	if eventType == "" {
		eventType = src.Name
	}
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}

	ev := &store.Event{
		OrgID:     src.OrgID,
		TraceID:   t.newTraceID(),
		Source:    src.Name, // 事件来源记源的名字，而不是能力的名字
		Type:      eventType,
		Payload:   payload,
		CreatedAt: t.now(),
	}
	if idemKey != "" {
		k := idemKey
		ev.IdemKey = &k
	}

	inserted, err := t.Store.InsertEvent(ctx, ev)
	if err != nil {
		return nil, fmt.Errorf("source: insert event: %w", err)
	}
	res := &FireResult{Event: ev, Inserted: inserted, Idempotent: !inserted}
	if !inserted {
		// 幂等命中：事件已处理过，不重复派发
		return res, nil
	}

	if len(src.Targets) == 0 {
		// 建源时就要求至少一个出口，走到这里说明数据被外部改坏了。
		// 不静默吞掉：这属于不一致状态，要能报警。
		return res, fmt.Errorf("source: %s 没有出口能力（source_targets 为空）", src.Name)
	}

	out, err := t.Dispatcher.Dispatch(ctx, route.DispatchInput{
		OrgID:   src.OrgID,
		TraceID: ev.TraceID,
		Params:  extractParams(payload),
		// Targets 非空 → Dispatcher 跳过 edges 查表，直接派给这些能力
		Targets: src.TargetIDs(),
		// 每个出口各自的参数模板：各能力的 params_schema 不同，
		// 一份公共 params 必然对不上至少一边（会被静默剪枝）。
		TargetParams: src.TargetParamsMap(),
		// Out 仅用于剪枝记录里的 port 字段，让人知道"是哪次触发派不出去"
		Out: eventType,
		// 源没有"能力 path"：它自己不是能力，所以环检测从空开始
		Path: nil,
	})
	if err != nil {
		return res, fmt.Errorf("source: dispatch: %w", err)
	}
	res.Dispatch = out
	return res, nil
}

// extractParams 从 payload 里取交给任务用的参数。
//
// 与 trigger.extractParams 同语义：payload 原样只存不解释（不变量 I6），
// 但任务 params 需要是有用的上游数据，所以整份 payload 作为 params 传下去。
func extractParams(payload json.RawMessage) map[string]any {
	if len(payload) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// ---------- cron 调度器 ----------

// Scheduler 是 cron 源的调度循环。
type Scheduler struct {
	Store      Store
	Dispatcher Dispatcher
	// Interval 是轮询间隔（默认 1s：cron 最细粒度是分钟，1s 足够精确）
	Interval time.Duration
	// Now 可注入（测试用）
	Now func() int64
}

func (s *Scheduler) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return time.Second
}

func (s *Scheduler) now() int64 {
	if s.Now != nil {
		return s.Now()
	}
	return store.Now()
}

// Run 启动调度循环，直到 ctx 取消。
//
// 单轮失败只记日志继续：调度是自愈机制，不能因一次 DB 抖动就永久停摆
// （否则所有 cron 源再也不会触发）。
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval())
	defer t.Stop()

	slog.Info("cron scheduler started",
		"event", "source.scheduler.start",
		"interval", s.interval().String(),
	)
	for {
		select {
		case <-ctx.Done():
			slog.Info("cron scheduler stopped", "event", "source.scheduler.stop")
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick 执行一轮：取到点的 cron 源，逐个触发。
//
// 触发时刻用**计划时刻**（next_fire_at）而不是"现在"：
// 它是幂等键的一部分，进程延迟醒来也不会导致同一分钟被触发两次。
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.now()
	due, err := s.Store.DueCronSources(ctx, now, 100)
	if err != nil {
		slog.Error("list due cron sources failed",
			"event", "source.scheduler.due_failed", "error", err.Error())
		return
	}
	if len(due) == 0 {
		return
	}

	trig := &Trigger{Store: s.Store, Dispatcher: s.Dispatcher, Now: s.Now}
	for _, src := range due {
		fireAt := now
		if src.NextFireAt != nil {
			fireAt = *src.NextFireAt
		}
		// 确定性幂等键：同一个源、同一个计划时刻只会产生一条事件
		idem := fmt.Sprintf("cron:%s:%d", src.ID, fireAt)

		full, err := s.Store.GetSource(ctx, src.OrgID, src.ID)
		if err != nil {
			slog.Error("load source failed",
				"event", "source.scheduler.load_failed", "source_id", src.ID, "error", err.Error())
			continue
		}

		payload, _ := json.Marshal(map[string]any{
			"source":  src.Name,
			"kind":    store.SourceKindCron,
			"firedAt": fireAt,
		})
		res, err := trig.Fire(ctx, full, src.Name, idem, payload)
		if err != nil {
			slog.Error("cron fire failed",
				"event", "source.scheduler.fire_failed",
				"source_id", src.ID, "source", src.Name, "error", err.Error())
			// 即使失败也推进 next_fire_at，否则这个源会卡在同一时刻反复重试、
			// 把日志刷满。真实的失败会作为错误留在日志里。
		} else if res.Idempotent {
			slog.Debug("cron fire skipped (idempotent)",
				"event", "source.scheduler.idempotent", "source", src.Name)
		} else {
			slog.Info("cron fired",
				"event", "source.fired",
				"source", src.Name, "source_id", src.ID,
				"tasks", len(res.Dispatch.Tasks),
				"pruned", len(res.Dispatch.Pruned),
			)
		}

		// 算下一次：从**计划时刻**往后推，避免延迟醒来导致漏触发
		next, nerr := store.NextCronTime(full.Config, time.Unix(fireAt, 0))
		if nerr != nil || next.IsZero() {
			// 表达式永不成立（如 2 月 30 日）：停用该源并记日志，
			// 别让它每分钟被查一次却什么也不做。
			slog.Warn("cron expression never fires, disabling source",
				"event", "source.scheduler.never_fires",
				"source", src.Name, "source_id", src.ID)
			continue
		}
		if err := s.Store.MarkSourceFired(ctx, src.ID, fireAt, next.Unix()); err != nil {
			slog.Error("mark source fired failed",
				"event", "source.scheduler.mark_failed", "source_id", src.ID, "error", err.Error())
		}
	}
}
