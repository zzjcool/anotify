// Package source 实现「源」（origin）的运行时：cron 到点触发、webhook 收事件。
//
// 源与能力的区别（也是这个包存在的理由）：
//
//	能力  回答"能干什么"——由节点声明，中枢派任务给它
//	源    回答"什么时候开始干"——有配置、自己产生事件、自己不干活
//
// 所以源**不执行**任何东西：它只产生事件。它的出口就是 edges 里的**入口边**
// （from_is_source=1、from_key=来源名、from_port='*'），参数模板存在边的 mapping 上。
// 触发时只把「来源名 + 事件类型」递给 Dispatcher，取边/求值/扇出全在 route 一处
// （P2.1 已删除原来那条跳过 edges 的旁路）。
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
	GetSource(ctx context.Context, namespaceID, id string) (*store.Source, error)
	InsertEvent(ctx context.Context, e *store.Event) (bool, error)
	// InsertPruneEvent 落一条剪枝事件（#142）——入口校验失败时的留痕出口。
	//
	// 为什么要它：resultSchema 不合规的事件**照常落库、不派发**（设计 §4.2：
	// 外部系统不重试，丢了就真没了），于是一次命中只能从事件流里看到。
	// 它与派发期的剪枝用**同一张表、同一套 source/type 约定**
	// （source='route'、type='pruned.<reason>'），trace 视图因此不用分两套口径读。
	InsertPruneEvent(ctx context.Context, e *store.Event) (bool, error)
}

// Dispatcher 是派发器（route.Dispatcher 满足它）。
type Dispatcher interface {
	Dispatch(ctx context.Context, in route.DispatchInput) (*route.DispatchOutput, error)
}

// Trigger 把一次"源触发"变成事件 + 首轮派发。
//
// 它和 trigger.Service.Ingest 现在是**同一条路**（P2.1）：两者都只给
// 「来源名 + 事件类型」，目标能力与参数模板全从入口边（edges）里取。
// 区别只在事件行由谁写：Ingest 是外部系统 POST 进来，Trigger 是源自己在内部发起。
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
// payload 的合并规则（打底 + 覆盖，且打底只对 cron/manual 生效，见
// eventPayload）与变量求值同样与 Fire 一致：不一致的话，试运行看到的
// `{{x}}` 取值就与真触发不同，试运行也就失去了意义。
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
		// 两种来源都可能为空：已保存的源（GetSource 从入口边读，空 = 边被删了）
		// 或**还没保存的草稿**（用户在请求体里没选出口）。文案要同时说得通——
		// 试运行是「保存前先验一遍」的主路径，不能假设它一定在库里。
		return nil, fmt.Errorf("source: %s 没有配置出口（入口边）", src.Name)
	}
	// payloadMap = config.payload 打底 + 本次事件 payload 覆盖（契约 §2.5）；
	// 打底只对 cron/manual 生效，webhook/github 的 payload 就是投递体（A13）。
	payloadMap, _ := eventPayload(src, payload)
	// 入口校验与 Fire 同一判据：试运行必须能提前看到"这版 payload 会被
	// event_schema 剪枝"，否则用户只会在真触发之后从事件流里发现。
	// 试运行不落库：只把判定回给用户（不写事件、不写剪枝）。
	if p, bad := resultSchemaPrune(src, eventType, payloadMap); bad {
		return prunedOnly(p), nil
	}
	return t.Dispatcher.Dispatch(ctx, route.DispatchInput{
		NamespaceID: src.NamespaceID, // 派发（查边/找节点/建任务）的隔离维度
		TraceID:     "dryrun",        // 不落库，trace 只是占位（Dispatch 要求非空）
		// 入口边：来源名 + 事件类型。目标与参数模板都从这条边上取（P2.1）。
		FromIsSource:  true,
		EntryFromKey:  src.Name,
		EntryFromPort: eventType,
		// Payload / VarValues 是边上模板（{{路径}} / {from} / {var}）的取值来源。
		// 试运行必须真解析一遍，用户才能看到 param_missing / param_type。
		Payload:   payloadMap,
		VarValues: route.ResolveVars(src.VarDefs(), payloadMap),
		Path:      nil,
		DryRun:    true,
	})
}

// Fire 触发一个源：写事件 + 派给它的入口边的目标。
//
// eventType 是这次事件的类型（写进 events.type）。它同时就是入口边的**端口**
// （`from_port = ? OR from_port = '*'`），也是剪枝记录里的 port——
// 排查"为什么没派出去"时两个地方用的是同一个值。
//
// payload 先与 config.payload 合并（打底 + 覆盖；打底只对 cron/manual 生效），
// 再作为边上模板的取值来源：`[Payload]` 给 `{{x}}` / `{from}` 查路径，`[VarValues]` 是
// `config.vars` 求出的别名表，给 `{{name}}` / `{var}` 用。两个都传下去，
// Dispatcher 才能把 edges.mapping 里的模板解析成真正的参数（#114）。
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

	// 事件 payload = config.payload 打底 + 本次 payload 覆盖同名键（契约 §2.5）；
	// 打底只对 cron/manual 生效，webhook/github 直接用投递体（A13）。
	// 写进 events 的字节也用合并后的 map 重新 marshal：否则"剪枝说取不到 x"
	// 与"事件体里明明有 x"会互相矛盾，事后无法复盘。
	payloadMap, evPayload := eventPayload(src, payload)

	ev := &store.Event{
		NamespaceID: src.NamespaceID, // 事件属于源所在的命名空间（I10）
		TraceID:     t.newTraceID(),
		Source:      src.Name, // 事件来源记源的名字，而不是能力的名字
		Type:        eventType,
		Payload:     evPayload,
		CreatedAt:   t.now(),
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
		// 建源时就要求至少一个目标，走到这里说明数据被外部改坏了。
		// 不静默吞掉：这属于不一致状态，要能报警。
		return res, fmt.Errorf("source: %s 没有入口边（edges 里没有 from_key=%s 的行）", src.Name, src.Name)
	}

	// 入口校验（设计 §4.2 / §7 P0.2）：事件**已经落库**（外部系统不重试，丢了就
	// 真的没了），这里只决定派不派。不合规 → 一条都不派 + prune(event_schema)，
	// trace 里看得到"收到了，但这版 payload 不合契约"。
	//
	// 为什么不在 handler 层直接 400：400 会让外部系统把事件丢在地上，而这整
	// 套系统一直在消灭的正是静默失败。钳住派发、留住事件，才是可复盘的处理。
	if p, bad := resultSchemaPrune(src, eventType, payloadMap); bad {
		t.recordEventSchemaPrune(ctx, ev, p)
		res.Dispatch = prunedOnly(p)
		return res, nil
	}

	out, err := t.Dispatcher.Dispatch(ctx, route.DispatchInput{
		NamespaceID: src.NamespaceID, // 源触发：命名空间随源走（契约 §3）
		TraceID:     ev.TraceID,
		// 入口边：来源名 + 事件类型。目标能力与参数模板都从这条边上取（P2.1）——
		// 不再有 Targets / TargetParams 旁路，所以源触发与 POST /v1/events
		// 走的是同一条路、同一套判定。
		FromIsSource:  true,
		EntryFromKey:  src.Name,
		EntryFromPort: eventType,
		// Payload / VarValues 是边上模板（{{路径}} / {from} / {var}）的取值来源。
		// 注意 Params 不传：入口边的参数来自边上的模板，不是整份 payload——
		// 否则「参数从哪来」又会有两条口径。
		Payload:   payloadMap,
		VarValues: route.ResolveVars(src.VarDefs(), payloadMap),
		// 源没有"能力 path"：它自己不是能力，所以环检测从空开始
		Path: nil,
	})
	if err != nil {
		return res, fmt.Errorf("source: dispatch: %w", err)
	}
	res.Dispatch = out
	return res, nil
}

// ---------- 入口校验：resultSchema（设计 §4.2 / §7 P0.2）----------

// resultSchemaPrune 用源的 resultSchema 判本次事件 payload。
//
// 返回 (记录, true) = 不合规：调用方**不派发**，但要留痕（事件本身已经落库）；
// 返回 (_, false) = 通过，或者源根本没声明契约。
//
// **没声明 = 不校验**：加这个字段之前的所有源（含已经被手改成非对象的脏数据）
// 行为与今天完全一致，不需要任何迁移。
//
// 判据不另写一套：用 route.ValidateEventPayload，与边上的 params_schema 同一个
// 受控子集引擎（required/type/…）。同一个 payload 在入口和在边上不能得出不同结论。
//
// detail 必须能回答「凭什么说这次事件不合规」：引擎给的消息就是
// `payload.title: want type string, got number` / `payload: missing required field "title"`
// 这种形式——哪个字段、期望什么、实得什么全在里面。
func resultSchemaPrune(src *store.Source, eventType string, payload map[string]any) (route.PrunedRecord, bool) {
	schema := src.ResultSchema()
	if len(schema) == 0 {
		return route.PrunedRecord{}, false // 未声明 = 不校验（老源行为不变）
	}
	msg := route.ValidateEventPayload(schema, payload)
	if msg == "" {
		return route.PrunedRecord{}, false
	}
	return route.PrunedRecord{
		Reason: route.ReasonEventSchema,
		// Port 是事件类型：它与派发期剪枝里的 port 是同一个位置
		//（"是哪一次触发派不出去"），trace 里能直接对上。
		Port:   eventType,
		Detail: fmt.Sprintf("payload 不符合源「%s」的 resultSchema：%s", src.Name, msg),
	}, true
}

// recordEventSchemaPrune 把一条入口剪枝写进 events。
//
// 与派发期的剪枝（route.Dispatcher.prune）共用**同一套约定**而不共用那个函数：
// source='route'、type='pruned.<reason>'、payload=PruneEventPayload。区别只在
// 产生点——这里发生在派发**之前**（压根没走到 plan），所以那边那个函数用不上。
//
// 留痕是 best-effort：写失败只记日志，绝不反过来让这次投递失败。事件已经落库，
// 一条诊断记录写不进去不能把"可观测的剪枝"变成"不可观测的错误"。
func (t *Trigger) recordEventSchemaPrune(ctx context.Context, ev *store.Event, p route.PrunedRecord) {
	payload, err := json.Marshal(route.PruneEventPayload{
		// 入口剪枝的上游标识是**源的名字**（与入口边的 fromCapability 同一口径）。
		FromCapability: ev.Source,
		Port:           p.Port,
		Detail:         p.Detail,
	})
	if err != nil {
		// 三个字段全是 string，理论上不可达；保底不 panic。
		slog.Warn("marshal event_schema prune payload failed",
			"event", "source.schema_prune.record_failed",
			"trace_id", ev.TraceID, "error", err.Error())
		return
	}
	if _, err := t.Store.InsertPruneEvent(ctx, &store.Event{
		NamespaceID: ev.NamespaceID, // 与事件同一个命名空间（I10）
		TraceID:     ev.TraceID,     // 同一条 trace：这就是"这次事件为什么没派"
		Source:      route.PruneEventSource,
		Type:        route.PruneEventType(p.Reason),
		Payload:     payload,
		CreatedAt:   t.now(),
	}); err != nil {
		slog.Warn("record event_schema prune failed",
			"event", "source.schema_prune.record_failed",
			"trace_id", ev.TraceID, "source", ev.Source, "error", err.Error())
	}
}

// prunedOnly 是一次「入口剪枝」的派发结果：0 个任务 + 1 条剪枝。
//
// 形状必须与派发期一致（Tasks 是 [] 而不是 null）：调用方（handler / 前端）只
// 有一套读法，空列表返回 [] 也是仓库硬约束。
func prunedOnly(p route.PrunedRecord) *route.DispatchOutput {
	return &route.DispatchOutput{Tasks: []*store.Task{}, Pruned: []route.PrunedRecord{p}}
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

// eventPayload 算出本次事件的 payload：config.payload 打底，事件 payload
// 覆盖同名键（契约 §2.5）。
//
// **打底只对 cron / manual 生效**（A13）：这两种源的事件体是源自己造的，
// config.payload 就是它的默认值。webhook / github 的事件体来自外部投递，
// 投递体本身就是 payload —— 历史行里残留的 `payload` 键（早期
// ValidateSourceConfig 各 kind 都保留）绝不能被复活成事件字段：那样同一次
// 投递在两版实现下会产出不同的 payload，事后复盘无从对齐。
// 未知 kind 同样按"不合并"处理：宁可少合并，也不要凭空造字段。
//
// 两个返回值一定要成对使用：map 供参数求值 / 任务 params，字节供写进
// events。两者必须同源，否则「剪枝说取不到 x」和「事件体里明明有 x」
// 会互相矛盾，事后根本查不清到底发生了什么。
//
// 没有打底数据（不是 cron/manual，或 ConfigPayload 为 nil，即没配 / 不是
// JSON 对象）时直接返回事件 payload 本身：不重新 marshal，保留调用方给的
// 原始字节。
func eventPayload(src *store.Source, payload json.RawMessage) (map[string]any, json.RawMessage) {
	m := extractParams(payload)
	if !usesConfigPayload(src.Kind) {
		return m, payload
	}
	base := src.ConfigPayload()
	if len(base) == 0 {
		return m, payload
	}
	merged := make(map[string]any, len(base)+len(m))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range m {
		merged[k] = v // 事件 payload 覆盖同名键
	}
	// 值都来自 JSON 反序列化，marshal 不会失败；失败时退回原始字节。
	if b, err := json.Marshal(merged); err == nil {
		return merged, b
	}
	return merged, payload
}

// usesConfigPayload 判断这种源是否用 config.payload 给事件体打底。
//
// 只有 cron / manual 才算（A13）：它们的 config.payload 就是源自己的默认
// 事件体。webhook / github 的 payload 来自外部投递；未知 kind 也按保守
// 处理（不合并）。
func usesConfigPayload(kind string) bool {
	switch kind {
	case store.SourceKindCron, store.SourceKindManual:
		return true
	default:
		return false
	}
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

		// src.NamespaceID 是 DueCronSources 行自带的命名空间（store 扫出来），
		// GetSource 按它定位：令牌/名称只在命名空间内唯一。
		full, err := s.Store.GetSource(ctx, src.NamespaceID, src.ID)
		if err != nil {
			slog.Error("load source failed",
				"event", "source.scheduler.load_failed", "source_id", src.ID, "error", err.Error())
			continue
		}

		// cron 的事件体：三个系统字段打底，config.payload 的用户字段覆盖
		// 同名键（契约 §2.5）——用户声明的字段得能盖住系统默认值，否则
		// "config.payload 里写了 source" 这件事会静默不生效。
		payloadMap := map[string]any{
			"source":  src.Name,
			"kind":    store.SourceKindCron,
			"firedAt": fireAt,
		}
		for k, v := range full.ConfigPayload() {
			payloadMap[k] = v
		}
		payload, _ := json.Marshal(payloadMap)
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
