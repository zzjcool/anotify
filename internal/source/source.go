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

// TaskRef 是回执里的一条已建任务（含未解析失败任务：真实请求名 + 明确声明的
// version；没有 digest 事实就不携带 digest 字段）。
type TaskRef struct {
	TaskID     string `json:"taskId"`
	Capability string `json:"capability"`
	// Version 只在真实解析/明确声明时出现（0 省略，不公开“没有事实”的 0）。
	Version int `json:"version,omitempty"`
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

// Ingest 接收一次事件：单次 store.AcceptSourceEvent 原子落「事件 + 投递窗 + 正常
// 任务 + 未解析失败 Task/错误事件 + 纯剪枝留痕 + 完整回执」，返回统一回执。
//
// 关键边界：
//   - 预编译的任务/失败/剪枝事实与「源 resource_version CAS + namespaceVarsRevision
//     pin」同事务校验：源被改/ns 输入被改 → 明确错误 + 0 写（不是重试同一批陈旧参数）。
//   - 窗口内重复投递返回第一次持久化的**同一份** receipt 字节（first=false），
//     不建任务、不重算可变资源。
//   - downstream_missing 是失败 Task + 错误事件（不是剪枝）；`taskId:null` 不用于它。
//   - 无 deliveryId 同样原子（只是不去重）；dry-run 不调本入口。
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

	// 计划先于接收：store 需要 typed 事实与事件同事务写；计划阶段的读失败不落任何行。
	var (
		tasks    []*store.Task
		failures []store.DispatchFailureInputs
		prunes   []store.PruneInputs
	)
	if prune := route.ValidatePayload(s.payloadSchema(src), payload); prune != nil {
		prunes = append(prunes, store.PruneInputs{
			Capability: prune.Capability, Reason: prune.Reason, Detail: prune.Detail,
		})
	} else {
		plan, err := s.router.PlanFromSource(ctx, src, traceID, payload)
		if err != nil {
			return nil, err
		}
		tasks = plan.Tasks("")
		failures = plan.FailureInputs()
		prunes = plan.PruneInputs()
	}

	nsCfg, err := s.db.GetNamespaceConfig(ctx, src.NamespaceID)
	if err != nil {
		return nil, fmt.Errorf("source: 读命名空间输入版本: %w", err)
	}
	receiptJSON, first, err := s.db.AcceptSourceEvent(ctx, store.SourceEventInputs{
		NamespaceID:                   src.NamespaceID,
		SourceID:                      src.ID,
		ExpectedSourceResourceVersion: src.ResourceVersion,
		ExpectedNamespaceVarsRevision: nsCfg.Revision,
		TraceID:                       traceID,
		Type:                          eventType,
		Payload:                       rawPayload,
		DeliveryID:                    in.DeliveryID,
		WindowSec:                     s.eventWindow(ctx),
		Now:                           s.Now(),
		Tasks:                         tasks,
		Failures:                      failures,
		Prunes:                        prunes,
	})
	if err != nil {
		return nil, fmt.Errorf("source: 接收事件: %w", err)
	}
	return receiptFromStore(receiptJSON, first)
}

// receiptFromStore 把 store 的持久 receipt 投影为公共回执（W6 原字段）。
//
// tasks 列**全部真实建立的 task**：正常任务 + 未解析失败任务（真实请求名/明确声明
// version；没有 digest 事实就不提 digest）。pruned 只含真实纯剪枝；store 内部的
// failure 事实不新增独立 Resource，也不被当成剪枝。
func receiptFromStore(raw json.RawMessage, first bool) (*Receipt, error) {
	var stored store.SourceEventReceipt
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("source: 解码接收回执: %w", err)
	}
	receipt := &Receipt{
		EventID:   stored.EventID,
		TraceID:   stored.TraceID,
		Duplicate: !first,
		Tasks:     []TaskRef{},
		Pruned:    []route.Prune{},
	}
	for _, task := range stored.Tasks {
		receipt.Tasks = append(receipt.Tasks, TaskRef{
			TaskID: task.TaskID, Capability: task.Capability, Version: task.Version,
		})
	}
	for _, failure := range stored.FailedTasks {
		receipt.Tasks = append(receipt.Tasks, TaskRef{
			TaskID: failure.TaskID, Capability: failure.Capability, Version: failure.Version,
		})
	}
	for _, prune := range stored.Pruned {
		receipt.Pruned = append(receipt.Pruned, route.Prune{
			Capability: prune.Capability, Reason: prune.Reason, Detail: prune.Detail,
		})
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
		preview.Targets = append(preview.Targets, PreviewTarget{
			Capability: target.Capability, Version: target.Version,
			Params: target.Params, State: target.State,
			Reason: target.Reason, Detail: target.Detail,
		})
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
