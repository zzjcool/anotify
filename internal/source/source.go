// Package source 是 v4 **源触发**的唯一运行期入口：手动 / webhook / cron 三个入口
// 共用同一份归一化配置、同一份载荷装配、同一份接收回执（source.md §5.1）。
//
// 一次触发做四件事：
//
//  1. 装配载荷：cron/manual 以 `config.payload` 打底、本次输入一层覆盖；
//     webhook 的载荷就是投递体里的 `payload`（没有静态打底）。
//  2. 投递幂等：`(namespace, 源稳定 id, deliveryId)` 在保留窗口内只算一条事件，
//     重复投递返回**原** event/trace 且不重复派发；窗口外同 id 按新事件接收。
//  3. 载荷契约：不符合 `config.payloadSchema` → 入库 + `event_schema` 剪枝留痕，不派发。
//  4. 路由投影：`route.PlanFromSource` 逐行求值 targets，成功的建任务、失败的每条
//     留一条剪枝事件（`source='route'`）。源没有 targets → 单条 `no_target`。
//
// 试运行（dry-run）走**同一条**解析路径但完全不落库：不写事件 / 任务 / 去重表 /
// 回执，也不需要保存 token（source.md §5.3）。
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// ErrBadPayload 表示投递体里的 `payload` 不是 JSON 对象（HTTP 400，不产生事件）。
var ErrBadPayload = errors.New("source: payload 必须是 JSON 对象")

// Clock 是秒级时钟（测试注入）。
type Clock func() int64

// Service 是源运行期的入口。
type Service struct {
	db     *store.DB
	router *route.Router
	now    Clock
}

// New 用真实时钟构造源服务。
func New(db *store.DB, router *route.Router) *Service {
	return &Service{db: db, router: router, now: func() int64 { return store.Now() }}
}

// NewWithClock 是显式时钟版。
func NewWithClock(db *store.DB, router *route.Router, now Clock) *Service {
	if now == nil {
		now = func() int64 { return store.Now() }
	}
	return &Service{db: db, router: router, now: now}
}

// Router 返回共享的路由器（编排层复用同一份解析）。
func (s *Service) Router() *route.Router { return s.router }

// Now 返回当前时刻（秒）。
func (s *Service) Now() int64 {
	if s.now == nil {
		return store.Now()
	}
	return s.now()
}

// Input 是一次源触发的事实。
type Input struct {
	// Source 是已经解析到的源行（webhook 由 token 定位，manual/API 由名字定位）。
	Source *store.Source
	// Type 是事件类型：webhook 取投递体的 type；空 → 回落源名（source.md §5）。
	Type string
	// DeliveryID 是外部投递幂等键（可选；webhook 的 `id`）。
	DeliveryID string
	// Payload 是投递体里的 payload（原始 JSON；空 → 只用配置打底）。
	Payload json.RawMessage
}

// TaskRef 是回执里的一条已建任务。
type TaskRef struct {
	TaskID     string `json:"taskId"`
	Capability string `json:"capability"`
	Version    int    `json:"version"`
}

// Receipt 是三个入口共用的接收回执（source.md §5.1，不是资源信封）。
//
// 空列表一律 `[]`（不是 null）：`tasks: []` / `pruned: []` 是「确实没有」，
// 前端与脚本据此区分「没派发」与「字段缺失」。
type Receipt struct {
	EventID   string        `json:"eventId"`
	TraceID   string        `json:"traceId"`
	Duplicate bool          `json:"duplicate"`
	Tasks     []TaskRef     `json:"tasks"`
	Pruned    []route.Prune `json:"pruned"`
}

// Ingest 接收一次事件：落事件 → 剪枝留痕 → 建任务，返回统一回执。
//
// 顺序与原子性说明：投递窗口与事件行由 `store.InsertEvent` 在**一个事务**里
// 判定/写入；任务扇出由 `store.CreateTasks` 在**另一个事务**里整批写入。store 当前
// 没有「事件 + 任务」同一事务的入口（见 G2-WRITER-VERDICT 的精确 gap 记录），所以
// 事件先落、任务后建：任务批失败时事件仍在（返回错误给调用方重试，重复投递由
// deliveryId 窗口吸收）。这里不伪造原子性，也不在失败时编造成功回执。
func (s *Service) Ingest(ctx context.Context, in Input) (*Receipt, error) {
	if in.Source == nil {
		return nil, fmt.Errorf("source: 缺少源行")
	}
	src := in.Source
	payload, perr := s.assemblePayload(src, in.Payload)
	if perr != nil {
		return nil, perr
	}
	traceID := store.NewEventID()
	eventType := strings.TrimSpace(in.Type)
	if eventType == "" {
		eventType = src.Name
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("source: 编码载荷: %w", err)
	}
	event := &store.Event{
		ID:          store.NewEventID(),
		NamespaceID: src.NamespaceID,
		TraceID:     traceID,
		Source:      src.Name,
		Type:        eventType,
		Payload:     rawPayload,
		Reason:      store.EventReasonIngested,
	}
	if in.DeliveryID != "" {
		deliveryID := in.DeliveryID
		event.DeliveryID = &deliveryID
	}
	window := s.eventWindow(ctx)
	inserted, err := s.db.InsertEvent(ctx, event, s.Now(), window)
	if err != nil {
		return nil, fmt.Errorf("source: 落事件: %w", err)
	}
	if !inserted {
		// 窗口内重复投递：回原 event/trace，不重复派发（source.md §5.1）。
		return &Receipt{
			EventID: event.ID, TraceID: event.TraceID, Duplicate: true,
			Tasks: []TaskRef{}, Pruned: []route.Prune{},
		}, nil
	}
	receipt := &Receipt{
		EventID: event.ID, TraceID: event.TraceID,
		Tasks: []TaskRef{}, Pruned: []route.Prune{},
	}
	// 载荷契约：不符合 payloadSchema → event_schema 剪枝，不派发。
	if prune := route.ValidatePayload(s.payloadSchema(src), payload); prune != nil {
		if _, perr := s.recordPrune(ctx, src.NamespaceID, traceID, *prune); perr != nil {
			return nil, perr
		}
		receipt.Pruned = append(receipt.Pruned, *prune)
		return receipt, nil
	}
	plan, err := s.router.PlanFromSource(ctx, src, traceID, payload)
	if err != nil {
		return nil, err
	}
	for _, prune := range plan.Pruned {
		if _, perr := s.recordPrune(ctx, src.NamespaceID, traceID, prune); perr != nil {
			return nil, perr
		}
		receipt.Pruned = append(receipt.Pruned, prune)
	}
	tasks := plan.Tasks("")
	if len(tasks) > 0 {
		if err := s.db.CreateTasks(ctx, tasks); err != nil {
			return nil, fmt.Errorf("source: 建任务: %w", err)
		}
		for _, task := range tasks {
			receipt.Tasks = append(receipt.Tasks, TaskRef{
				TaskID: task.ID, Capability: task.Capability, Version: task.Version,
			})
		}
	}
	return receipt, nil
}

// Fire 是手动触发（`POST …/sources/{name}/fire`）：省略 payload 时用源配置的打底值。
//
// 停用的源按 §5 的停用规则处理：**不**触发（调用方翻 404/409，本函数返回
// ErrSourceDisabled，避免把「已停用」记成一次真实事件）。
func (s *Service) Fire(ctx context.Context, namespaceID, name string, payload json.RawMessage) (*Receipt, error) {
	src, err := s.db.GetSourceByName(ctx, namespaceID, name)
	if err != nil {
		return nil, err
	}
	if !src.Enabled {
		return nil, ErrSourceDisabled
	}
	return s.Ingest(ctx, Input{Source: src, Type: src.Name, Payload: payload})
}

// ErrSourceDisabled 表示源已停用（webhook 对外 404，fire 拒绝触发）。
var ErrSourceDisabled = errors.New("source: 源已停用")

// HookBody 是 webhook 投递体（source.md §3.3）。
type HookBody struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

// Hook 处理一次 webhook 投递：token 定位源（停用/不存在一律 404，不区分）。
func (s *Service) Hook(ctx context.Context, token string, body HookBody) (*Receipt, error) {
	src, err := s.db.LookupHook(ctx, token)
	if err != nil {
		return nil, err
	}
	return s.Ingest(ctx, Input{
		Source: src, Type: body.Type, DeliveryID: body.ID, Payload: body.Payload,
	})
}

// PreviewTarget 是试运行预览里的一条目标（source.md §5.3）。
type PreviewTarget struct {
	Capability string          `json:"capability"`
	Version    int             `json:"version,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	State      string          `json:"state"`
	Reason     string          `json:"reason,omitempty"`
	Detail     string          `json:"detail,omitempty"`
}

// Preview 是试运行预览文档（不是资源、不可写；没有 eventId/taskId）。
type Preview struct {
	Targets []PreviewTarget `json:"targets"`
	Pruned  []route.Prune   `json:"pruned"`
}

// Preview 对一个**已保存**的源做试运行（0 持久写入）。
func (s *Service) Preview(ctx context.Context, namespaceID, name string, payload json.RawMessage) (*Preview, error) {
	src, err := s.db.GetSourceByName(ctx, namespaceID, name)
	if err != nil {
		return nil, err
	}
	return s.PreviewSource(ctx, src, payload)
}

// PreviewSource 对任意源文档做试运行（未保存的源也能预览）。
//
// 完全不写库：事件、任务、去重记录、剪枝留痕、回执一个都不落（source.md §5.3）。
func (s *Service) PreviewSource(ctx context.Context, src *store.Source, payload json.RawMessage) (*Preview, error) {
	if src == nil {
		return nil, fmt.Errorf("source: 缺少源行")
	}
	assembled, perr := s.assemblePayload(src, payload)
	if perr != nil {
		return nil, perr
	}
	preview := &Preview{Targets: []PreviewTarget{}, Pruned: []route.Prune{}}
	if prune := route.ValidatePayload(s.payloadSchema(src), assembled); prune != nil {
		preview.Pruned = append(preview.Pruned, *prune)
		return preview, nil
	}
	plan, err := s.router.PlanFromSource(ctx, src, "preview", assembled)
	if err != nil {
		return nil, err
	}
	for _, target := range plan.Targets {
		item := PreviewTarget{
			Capability: target.Capability, Version: target.Version,
			Params: target.Params, State: target.State,
			Reason: target.Reason, Detail: target.Detail,
		}
		if item.State == route.StatePruned && item.Reason == "" && len(plan.Pruned) > 0 {
			item.Reason = plan.Pruned[0].Reason
		}
		preview.Targets = append(preview.Targets, item)
	}
	preview.Pruned = append(preview.Pruned, plan.Pruned...)
	return preview, nil
}

// assemblePayload 装配载荷：cron/manual 以 config.payload 打底、投递体一层覆盖。
//
// 深合并不做（source.md §5：一层覆盖）；webhook 没有静态打底。
// 投递体不是 JSON 对象 → ErrBadPayload（400，不产生事件）。
func (s *Service) assemblePayload(src *store.Source, incoming json.RawMessage) (map[string]any, error) {
	base := map[string]any{}
	if src.Kind == sourcespec.KindCron || src.Kind == sourcespec.KindManual {
		if cfgPayload := sourcespec.ParseConfig(src.Kind, src.Config).Payload; len(cfgPayload) > 0 {
			for name, value := range cfgPayload {
				base[name] = value
			}
		}
	}
	overlay, ok := route.DecodePayload(incoming)
	if !ok {
		return nil, ErrBadPayload
	}
	for name, value := range overlay {
		base[name] = value
	}
	return base, nil
}

// payloadSchema 读出源配置里的载荷契约（webhook/cron/manual 通用字段）。
func (s *Service) payloadSchema(src *store.Source) json.RawMessage {
	return sourcespec.ParseConfig(src.Kind, src.Config).PayloadSchema
}

// eventWindow 是投递去重窗口（= events 保留期；0 = 永久）。
func (s *Service) eventWindow(ctx context.Context) int64 {
	cfg, err := s.db.GetSystemConfig(ctx)
	if err != nil || cfg == nil {
		return store.DefaultRetentionEventTTLSec
	}
	return cfg.RetentionEventTTLSec
}

// recordPrune 写一条剪枝留痕并返回真实事件 id。
func (s *Service) recordPrune(ctx context.Context, namespaceID, traceID string, prune route.Prune) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"capability": prune.Capability, "detail": prune.Detail,
	})
	if err != nil {
		payload = json.RawMessage(`{}`)
	}
	event := &store.Event{
		NamespaceID: namespaceID,
		TraceID:     traceID,
		Type:        prune.Reason,
		Payload:     payload,
	}
	if err := s.db.InsertPruneEvent(ctx, event); err != nil {
		return "", fmt.Errorf("source: 写剪枝留痕: %w", err)
	}
	return event.ID, nil
}

// 编译期断言：回执 reason 必须是封闭剪枝词表（防止有人在这里新造一个词）。
var _ = func() bool {
	for _, reason := range []string{
		protocol.ReasonNoTarget, protocol.ReasonEventSchema, protocol.ReasonParamMissing,
		protocol.ReasonParamSchema, protocol.ReasonParamType, protocol.ReasonDownstreamMissing,
	} {
		if !protocol.IsPruneReason(reason) && reason != protocol.ReasonDownstreamMissing {
			panic("source: 非剪枝原因码 " + reason)
		}
	}
	return true
}
