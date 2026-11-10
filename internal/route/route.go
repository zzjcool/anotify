// Package route 是 v4 中枢运行期的**纯路由投影**：把「一次源事件」或「一次能力
// emit」解析成「要建哪些下游任务」，并给出每条被剪掉的边的封闭原因码。
//
// 三条边界（capability.md §1/§8.1/§8.3）：
//
//   - 一个能力一个结果：能力边来自 `spec.next`，入口边来自源的 `spec.targets`；
//     两者都是派生投影（edges 表），本包只读不写；
//   - 任务钉住目标引用：解析出的 name/version/digest/params 在**建行那一刻**固定，
//     上游之后改 next 不影响已建任务；
//   - 剪枝不是错误：事件已接收、下游没建 task 时回执 `taskId:null` + `pruned`，
//     并留一条 `source='route'` 的事件行（source.md §6「剪枝永远留痕」）。
//
// 本包不做 WS、不执行代码、不写 edges：它是 store 之上的**读投影 + 参数对齐**，
// 供 internal/source（入口触发）、internal/ws（emit / 派发编排）与 G3 的 HTTP 层复用。
// 独立写一份参数/解析逻辑是错的——同一份语义在 emit 与源两条入口上必须一致。
package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// Clock 是中枢的秒级时钟（测试可注入；生产用 store.Now）。
type Clock func() int64

// Router 是路由投影的唯一入口。
type Router struct {
	db  *store.DB
	now Clock
}

// New 用真实时钟构造路由器。
func New(db *store.DB) *Router {
	return &Router{db: db, now: func() int64 { return store.Now() }}
}

// NewWithClock 是显式时钟版（测试/回放用）。
func NewWithClock(db *store.DB, now Clock) *Router {
	if now == nil {
		now = func() int64 { return store.Now() }
	}
	return &Router{db: db, now: now}
}

// DB 返回底层 store（编排层需要同一份连接）。
func (r *Router) DB() *store.DB { return r.db }

// Now 返回路由器当前时刻（秒）。
func (r *Router) Now() int64 {
	if r.now == nil {
		return store.Now()
	}
	return r.now()
}

// 计划里每条 target 的三种状态（source.md §5.3 dry-run 与派发共用）。
const (
	// StateReady 表示参数校验通过、可以建任务。
	StateReady = "ready"
	// StatePruned 表示目标解析到了，但参数/载荷被剪枝。
	StatePruned = "pruned"
	// StateMissing 表示目标引用在库里解析不到（downstream_missing）。
	StateMissing = "missing"
)

// Target 是一条已解析的下游目标（成功或带原因的失败）。
type Target struct {
	// Ref 是声明侧原样的引用（`name` / `name@version`）。
	Ref string `json:"ref"`
	// Capability / Version / Digest 是解析结果（missing 时为空）。
	Capability   string `json:"capability,omitempty"`
	Version      int    `json:"version,omitempty"`
	Digest       string `json:"digest,omitempty"`
	CapabilityID string `json:"-"`
	// Params 是校验/补默认值后的任务参数（ready 时有效）。
	Params json.RawMessage `json:"params,omitempty"`
	// State ∈ ready / pruned / missing。
	State string `json:"state"`
	// Reason / Detail 只在非 ready 时出现，reason 取封闭词表。
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Plan 是一次触发/emit 的完整解析结果。
type Plan struct {
	NamespaceID string
	TraceID     string
	// Targets 按声明顺序（源）或单条（emit）出现，包含被剪枝的行。
	Targets []Target
	// Pruned 是与 Targets 里非 ready 行一一对应的观察原因（含 no_target/event_schema）。
	Pruned []Prune
}

// Ready 返回可以建任务的 target。
func (p *Plan) Ready() []Target {
	out := make([]Target, 0, len(p.Targets))
	for _, t := range p.Targets {
		if t.State == StateReady {
			out = append(out, t)
		}
	}
	return out
}

// Tasks 把 ready target 变成待落库的任务行（一律 pending，无任何执行授权）。
//
// parentTaskID 为空表示入口事件（源）；非空表示业务 emit 的父子关系。
// 任务的 attempt/token/租约一律不在这里出现——那是 accept 之后 grant 事务的事。
func (p *Plan) Tasks(parentTaskID string) []*store.Task {
	ready := p.Ready()
	out := make([]*store.Task, 0, len(ready))
	for _, t := range ready {
		task := &store.Task{
			ID:           store.NewTaskID(),
			NamespaceID:  p.NamespaceID,
			TraceID:      p.TraceID,
			CapabilityID: t.CapabilityID,
			Capability:   t.Capability,
			Version:      t.Version,
			Digest:       t.Digest,
			Params:       t.Params,
			Phase:        store.TaskPending,
		}
		if parentTaskID != "" {
			parent := parentTaskID
			task.ParentTaskID = &parent
		}
		out = append(out, task)
	}
	return out
}

// resolveCapability 解析一个能力引用（`name` / `name@version`）。
//
// 解析不到时返回 (nil, nil)：调用方把这条边记成 `downstream_missing` 观察，
// 而不是整次触发失败（source.md §4.2：一行失败只剪那一条）。
func (r *Router) resolveCapability(ctx context.Context, namespaceID, ref string) (*store.CapabilityVersion, error) {
	capability, err := r.db.ResolveRef(ctx, namespaceID, ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("解析下游引用 %q: %w", ref, err)
	}
	return capability, nil
}

// PlanFromSource 按源声明的 targets 求值一次事件载荷。
//
// 每一行**独立**求值、独立校验（source.md §4.2）：模板取不到路径 → param_missing；
// 目标能力解析不到 → downstream_missing + state=missing；源没有 targets →
// 单条 no_target（事件照常接收，只是不派发）。
func (r *Router) PlanFromSource(ctx context.Context, src *store.Source, traceID string, payload map[string]any) (*Plan, error) {
	if src == nil {
		return nil, fmt.Errorf("route: nil source")
	}
	if traceID == "" {
		traceID = store.NewEventID()
	}
	plan := &Plan{NamespaceID: src.NamespaceID, TraceID: traceID}
	if len(src.Targets) == 0 {
		plan.Pruned = append(plan.Pruned, Prune{Reason: protocol.ReasonNoTarget, Detail: "源没有 targets"})
		return plan, nil
	}
	for _, declared := range src.Targets {
		ref := strings.TrimSpace(declared.Capability)
		if ref == "" {
			continue
		}
		capability, err := r.resolveCapability(ctx, src.NamespaceID, ref)
		if err != nil {
			return nil, err
		}
		if capability == nil {
			detail := fmt.Sprintf("下游 %q 解析不到", ref)
			plan.Targets = append(plan.Targets, Target{
				Ref: ref, Capability: ref, State: StateMissing,
				Reason: protocol.ReasonDownstreamMissing, Detail: detail,
			})
			plan.Pruned = append(plan.Pruned, Prune{
				Capability: ref, Reason: protocol.ReasonDownstreamMissing, Detail: detail,
			})
			continue
		}
		rendered, missing := sourcespec.RenderParams(declared, payload)
		if len(missing) > 0 {
			detail := "需要 " + strings.Join(missing, ", ")
			plan.Targets = append(plan.Targets, prunedTarget(capability, ref, detail))
			plan.Pruned = append(plan.Pruned, Prune{
				Capability: capability.Name, Reason: protocol.ReasonParamMissing, Detail: detail,
			})
			continue
		}
		params, prune := PrepareParams(capability.Spec.Params, rendered)
		if prune != nil {
			prune.Capability = capability.Name
			plan.Targets = append(plan.Targets, prunedTarget(capability, ref, prune.Detail))
			plan.Pruned = append(plan.Pruned, *prune)
			continue
		}
		plan.Targets = append(plan.Targets, Target{
			Ref: ref, Capability: capability.Name, Version: capability.Version,
			Digest: capability.Digest, CapabilityID: capability.ID,
			Params: params, State: StateReady,
		})
	}
	return plan, nil
}

// prunedTarget 把一条已解析但被剪枝的目标写成 Target（保留解析出的身份便于预览）。
func prunedTarget(capability *store.CapabilityVersion, ref, detail string) Target {
	t := Target{
		Ref: ref, Capability: capability.Name, Version: capability.Version,
		Digest: capability.Digest, CapabilityID: capability.ID,
		State: StatePruned, Detail: detail,
	}
	if capability != nil {
		t.Reason = ""
	}
	return t
}

// ErrNotDeclared 是 emit 目标不在父能力 `spec.next` 里的拒绝（400 target_not_declared）。
var ErrNotDeclared = errors.New("route: 目标不在 spec.next 里")

// PlanFromEmit 按父任务的 emit 目标解析一次下游派发。
//
// `to` 必须 ∈ **父任务钉住的那个能力版本**的 `spec.next`（capability.md §4.1）：
// 不在里面就是 400 target_not_declared，不静默吞掉；没给目标由调用方先按
// missing_target 拒绝。参数走「同名直取」：payload 里多余字段忽略，缺必填 → 剪枝。
func (r *Router) PlanFromEmit(ctx context.Context, parent *store.Task, to string, data json.RawMessage) (*Plan, error) {
	if parent == nil {
		return nil, fmt.Errorf("route: nil parent task")
	}
	if strings.TrimSpace(to) == "" {
		return nil, fmt.Errorf("%w: missing target", ErrNotDeclared)
	}
	parentCap, err := r.db.GetCapabilityByID(ctx, parent.NamespaceID, parent.CapabilityID)
	if errors.Is(err, store.ErrNotFound) {
		// 父能力行已被删除：无法证明 on spec.next 的声明 → 保守 fail closed。
		return nil, fmt.Errorf("%w: 父能力 %s 已不可读，无法校验 next", ErrNotDeclared, parent.Capability)
	}
	if err != nil {
		return nil, fmt.Errorf("读取父能力 %s: %w", parent.Capability, err)
	}
	if !declaresTarget(parentCap.Spec.Next, to) {
		return nil, fmt.Errorf("%w: %q 不在 %s 的 next (%v) 里",
			ErrNotDeclared, to, parentCap.Name, parentCap.Spec.Next)
	}
	plan := &Plan{NamespaceID: parent.NamespaceID, TraceID: parent.TraceID}
	capability, rerr := r.resolveCapability(ctx, parent.NamespaceID, to)
	if rerr != nil {
		return nil, rerr
	}
	if capability == nil {
		detail := fmt.Sprintf("下游 %q 解析不到", to)
		plan.Targets = append(plan.Targets, Target{
			Ref: to, Capability: to, State: StateMissing,
			Reason: protocol.ReasonDownstreamMissing, Detail: detail,
		})
		plan.Pruned = append(plan.Pruned, Prune{
			Capability: to, Reason: protocol.ReasonDownstreamMissing, Detail: detail,
		})
		return plan, nil
	}
	input, ok := DecodePayload(data)
	if !ok {
		detail := "emit data 必须是 JSON 对象"
		t := prunedTarget(capability, to, detail)
		t.Reason = protocol.ReasonParamSchema
		plan.Targets = append(plan.Targets, t)
		plan.Pruned = append(plan.Pruned, Prune{
			Capability: capability.Name, Reason: protocol.ReasonParamSchema, Detail: detail,
		})
		return plan, nil
	}
	params, prune := PrepareParams(capability.Spec.Params, input)
	if prune != nil {
		prune.Capability = capability.Name
		t := prunedTarget(capability, to, prune.Detail)
		t.Reason = prune.Reason
		plan.Targets = append(plan.Targets, t)
		plan.Pruned = append(plan.Pruned, *prune)
		return plan, nil
	}
	plan.Targets = append(plan.Targets, Target{
		Ref: to, Capability: capability.Name, Version: capability.Version,
		Digest: capability.Digest, CapabilityID: capability.ID,
		Params: params, State: StateReady,
	})
	return plan, nil
}

// declaresTarget 判断 emit 目标是否落在 next 的声明范围内。
//
// 规则（capability.md §2.1「next 的三种写法」）：
//   - next 里的裸名表示「跟随该名字」：任何同名的 to（含 name@version）都算声明；
//   - next 里的 `name@version` 是钉死某版：to 必须同名；to 带版本时必须同版本，
//     to 不带版本时仍算声明（由 ResolveRef 解析到 latest，钉版语义仍由父能力的
//     next 表达，不在这一层另造一套版本选择）。
func declaresTarget(next []string, to string) bool {
	toName, toVer, toPinned := splitRef(to)
	for _, ref := range next {
		name, ver, pinned := splitRef(ref)
		if name != toName {
			continue
		}
		if pinned && toPinned && ver != toVer {
			continue
		}
		return true
	}
	return false
}

// splitRef 把 `name` / `name@version` 拆成 (name, version, pinned)。
func splitRef(ref string) (string, int, bool) {
	ref = strings.TrimSpace(ref)
	name, verRaw, found := strings.Cut(ref, "@")
	if !found {
		return name, 0, false
	}
	var version int
	if _, err := fmt.Sscanf(verRaw, "%d", &version); err != nil || version <= 0 {
		// 坏的版本写法：当成钉版但永不可能匹配（fail closed）。
		return name, -1, true
	}
	return name, version, true
}
