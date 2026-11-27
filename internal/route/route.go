// Package route 是 v4 中枢运行期的**纯路由投影**：把「一次源事件」或「一次能力
// emit」解析成「要建哪些下游任务」，并给出每条被剪掉的边的封闭原因码。
//
// 三条边界（capability.md §1/§8.1/§8.3）：能力边来自 `spec.next`、入口边来自源的
// `spec.targets`，两者都是只读派生投影；任务在**建行那一刻**钉住 name/version/digest/params
// （上游之后改 next 不影响已建任务）；剪枝不是错误——事件已接收、下游没建 task 时回执
// `taskId:null` + `pruned`，并留一条 `source='route'` 的事件行（§6「剪枝永远留痕」）。
//
// 本包不做 WS、不执行代码、不写 edges：它是 store 之上的**读投影 + 参数对齐**，供
// internal/source（入口触发）、internal/ws（emit）与 HTTP 层复用；两份逻辑必然分叉。
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
	db     *store.DB
	now    Clock
	budget Budget
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
	// Outcomes 是 router 的**结局表快照**（#522，已按本次事件载荷求值过 payload 段，
	// decision 段留给判决执行时求值）。空 = 不是 router 任务；不进 wire。
	Outcomes json.RawMessage `json:"-"`
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
			Outcomes:     t.Outcomes,
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

// PruneInputs 把计划里的**纯剪枝**观察转成 store 的同事务事实（S1-D2/D3）。
//
// 只包含 param_* / no_target / event_schema 这一类「事件收到了但没派发」的原因；
// downstream_missing 是失败 Task（见 FailureInputs），不在这个集合里。
func (p *Plan) PruneInputs() []store.PruneInputs {
	out := make([]store.PruneInputs, 0, len(p.Pruned))
	for _, prune := range p.Pruned {
		out = append(out, store.PruneInputs{
			Capability: prune.Capability, Reason: prune.Reason, Detail: prune.Detail,
		})
	}
	return out
}

// FailureInputs 把未解析目标转成失败 Task 事实：真实任务（phase=failed、
// attempt=0）+ 错误事件，不是剪枝。Capability 保留**请求里写的引用**，不是解析结果。
func (p *Plan) FailureInputs() []store.DispatchFailureInputs {
	out := make([]store.DispatchFailureInputs, 0)
	for _, target := range p.Targets {
		if target.State != StateMissing {
			continue
		}
		out = append(out, store.DispatchFailureInputs{
			Capability: target.Capability,
			Version:    target.Version,
			Reason:     protocol.ReasonDownstreamMissing,
			Detail:     target.Detail,
		})
	}
	return out
}

// resolveCapability 解析一个能力引用（`name` / `name@version`）。
//
// 解析不到时返回 (nil, nil)：调用方把这条边记成 downstream_missing 的**失败 Task +
// 错误事件**（不是剪枝），而不是整次触发失败（source.md §4.2：一行失败不拖垮其它行）。
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

// PlanFromSource 按源声明的 targets 求值一次事件载荷：每一行**独立**求值、独立校验
// （source.md §4.2），顺序 `match → resolve → params → 开关 → ready`；源没有 targets
// → 单条 no_target（事件照常接收，只是不派发）。
func (r *Router) PlanFromSource(ctx context.Context, src *store.Source, traceID, eventType string, payload map[string]any) (*Plan, error) {
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
	if prune, err := r.checkBudget(ctx, src.NamespaceID, traceID, ""); err != nil {
		return nil, err
	} else if prune != nil {
		plan.Pruned = append(plan.Pruned, *prune)
		return plan, nil
	}
	for _, declared := range src.Targets {
		ref := strings.TrimSpace(declared.Capability)
		if ref == "" {
			continue
		}
		// ★ 条件派（#513/#519）：match 是“这条边要不要看这次事件”的闸，排在 resolve **之前**：
		// 不命中的边不该查库，也不该把 no_match 报成 downstream_missing。
		if hit, detail := declared.Match.Matches(eventType, payload); !hit {
			plan.pruneNoMatch(ref, detail)
			continue
		}
		capability, err := r.resolveCapability(ctx, src.NamespaceID, ref)
		if err != nil {
			return nil, err
		}
		if capability == nil {
			// 解析不到 = 未解析派发失败（failed Task + 错误事件），不是剪枝：
			// 只在 plan.Targets 里留 StateMissing，plan.Pruned 不带 downstream_missing。
			detail := fmt.Sprintf("下游 %q 解析不到", ref)
			plan.Targets = append(plan.Targets, Target{
				Ref: ref, Capability: ref, Version: declaredVersion(ref), State: StateMissing,
				Reason: protocol.ReasonDownstreamMissing, Detail: detail,
			})
			continue
		}
		// ★ #522：params 与 outcomes 一次求值（同一个载荷、同一套类型规则）。结局表里
		// 任何一个 payload 引用取不到都按 param_missing 剪枝整条 target：跑一个「结局表
		// 少了一半输入」的 router 只会派出一半参数的动作。
		rendered, outcomes, missing := sourcespec.RenderTarget(declared, payload)
		if len(missing) > 0 {
			detail := "需要 " + strings.Join(missing, ", ")
			plan.Targets = append(plan.Targets, prunedTarget(capability, ref, protocol.ReasonParamMissing, detail))
			plan.Pruned = append(plan.Pruned, Prune{
				Capability: capability.Name, Reason: protocol.ReasonParamMissing, Detail: detail,
			})
			continue
		}
		rawOutcomes, oerr := marshalOutcomes(outcomes)
		if oerr != nil {
			return nil, oerr
		}
		params, prune := PrepareParams(capability.Spec.Params, rendered)
		if prune != nil {
			prune.Capability = capability.Name
			plan.Targets = append(plan.Targets, prunedTarget(capability, ref, prune.Reason, prune.Detail))
			plan.Pruned = append(plan.Pruned, *prune)
			continue
		}
		// ★ 闸门（capability-lifecycle §6.1）：resolve → param_* → 开关 → ready。
		// 放在参数校验之后是设计图定的顺序：同时「参数不合法」与「已停用」时先报参数原因。
		accepting, aerr := r.capabilityAccepting(ctx, src.NamespaceID, capability.Name)
		if aerr != nil {
			return nil, aerr
		}
		if !accepting {
			plan.pruneDisabled(capability, ref, capability.Name)
			continue
		}
		plan.Targets = append(plan.Targets, Target{
			Ref: ref, Capability: capability.Name, Version: capability.Version,
			Digest: capability.Digest, CapabilityID: capability.ID,
			Params: params, Outcomes: rawOutcomes, State: StateReady,
		})
	}
	return plan, nil
}

// marshalOutcomes 把渲染好的结局表序列化成任务行的快照字节（空表 → nil）。
func marshalOutcomes(outcomes map[string][]sourcespec.OutcomeAction) (json.RawMessage, error) {
	if len(outcomes) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(outcomes)
	if err != nil {
		return nil, fmt.Errorf("route: 编码 outcomes: %w", err)
	}
	return raw, nil
}

// capabilityAccepting 是「能力能否接受新触发」的唯一判定点（capability-lifecycle §6.1）。
//
// 开关按**能力名**查（不是 `name@version`）：换版本不重置开关，一个名字一个闸门；
// 无开关行 = enabled，所以老库与未操作过的能力没有行为变化。判定在**计划期**、
// 与 sources.enabled 同一位置：这里只读，不写库、不改任何在途任务（§3）。
func (r *Router) capabilityAccepting(ctx context.Context, namespaceID, name string) (bool, error) {
	enabled, err := r.db.IsCapabilityEnabled(ctx, namespaceID, name)
	if err != nil {
		return false, fmt.Errorf("读能力开关 %q: %w", name, err)
	}
	return enabled, nil
}

// pruneNoMatch 把「match 不命中」记成一条剪枝（#513 的 types / #519 的字段级组合子）。
//
// capability 用声明里的名字（`name@version` 取 `name`）：此时还没解析，没有真身份可给；
// detail 由 `Match.Matches` 给（点名是 types 没命中、还是 all 的第几行、还是 any 全不成立）。
func (p *Plan) pruneNoMatch(ref, detail string) {
	name := refName(ref)
	p.Targets = append(p.Targets, Target{
		Ref: ref, Capability: name, State: StatePruned,
		Reason: protocol.ReasonNoMatch, Detail: detail,
	})
	p.Pruned = append(p.Pruned, Prune{Capability: name, Reason: protocol.ReasonNoMatch, Detail: detail})
}

// pruneDisabled 把「下游能力已停用」记成剪枝（target + 事实）：停用不是失败（§1/§6.1），
// 事件照收、父任务不失败，但「为什么没跑」要留证据。
func (p *Plan) pruneDisabled(capability *store.CapabilityVersion, ref, name string) {
	detail := fmt.Sprintf("下游 %q 已停用", name)
	p.Targets = append(p.Targets, prunedTarget(capability, ref, protocol.ReasonCapabilityDisabled, detail))
	p.Pruned = append(p.Pruned, Prune{
		Capability: capability.Name, Reason: protocol.ReasonCapabilityDisabled, Detail: detail,
	})
}

// prunedTarget 把一条已解析但被剪枝的目标写成 Target（保留解析出的身份便于预览）。
// reason 逐条给：目标是逐行求值的，拿 plan.Pruned[0] 兜给所有 target 会说错原因。
func prunedTarget(capability *store.CapabilityVersion, ref, reason, detail string) Target {
	return Target{
		Ref: ref, Capability: capability.Name, Version: capability.Version,
		Digest: capability.Digest, CapabilityID: capability.ID,
		State: StatePruned, Reason: reason, Detail: detail,
	}
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
	// 父 next 是**当前声明**（同 version 可变图配置）：每次 emit 重读声明，
	// 已建子任务/重试仍按建行时钉住的 version/digest 走。
	effective, declared := declaredRef(parentCap.Spec.Next, to)
	if !declared {
		return nil, fmt.Errorf("%w: %q 不在 %s 的 next (%v) 里（或同名钉版无法唯一选择）",
			ErrNotDeclared, to, parentCap.Name, parentCap.Spec.Next)
	}
	plan := &Plan{NamespaceID: parent.NamespaceID, TraceID: parent.TraceID}
	if prune, perr := r.checkBudget(ctx, parent.NamespaceID, parent.TraceID, parent.ID); perr != nil {
		return nil, perr
	} else if prune != nil {
		plan.Pruned = append(plan.Pruned, *prune)
		return plan, nil
	}
	// effective 是声明侧的引用（裸 name 跟随 latest / name@v 用 v）；
	// ResolveRef 的全局 latest 规则不改，选择在 route 本地完成。
	capability, rerr := r.resolveCapability(ctx, parent.NamespaceID, effective)
	if rerr != nil {
		return nil, rerr
	}
	if capability == nil {
		detail := fmt.Sprintf("下游 %q 解析不到", to)
		plan.Targets = append(plan.Targets, Target{
			// Ref 保留**真实请求引用**（回执 to 用它）；Capability 只存 basename，
			// 版本不进名字（失败 Task 的 capability + version 是两个独立列）。
			Ref: to, Capability: refName(to), Version: declaredVersion(effective), State: StateMissing,
			Reason: protocol.ReasonDownstreamMissing, Detail: detail,
		})
		return plan, nil
	}
	input, ok := DecodePayload(data)
	if !ok {
		detail := "emit data 必须是 JSON 对象"
		plan.Targets = append(plan.Targets, prunedTarget(capability, to, protocol.ReasonParamSchema, detail))
		plan.Pruned = append(plan.Pruned, Prune{
			Capability: capability.Name, Reason: protocol.ReasonParamSchema, Detail: detail,
		})
		return plan, nil
	}
	params, prune := PrepareParams(capability.Spec.Params, input)
	if prune != nil {
		prune.Capability = capability.Name
		plan.Targets = append(plan.Targets, prunedTarget(capability, to, prune.Reason, prune.Detail))
		plan.Pruned = append(plan.Pruned, *prune)
		return plan, nil
	}
	// ★ 与 PlanFromSource 同一个闸门、同一位置（参数之后、ready 之前）。
	// 开关按**能力名**查：effective 可能是 `name@v`，必须先取名字（refName）。
	accepting, aerr := r.capabilityAccepting(ctx, parent.NamespaceID, refName(effective))
	if aerr != nil {
		return nil, aerr
	}
	if !accepting {
		plan.pruneDisabled(capability, to, refName(effective))
		return plan, nil
	}
	plan.Targets = append(plan.Targets, Target{
		Ref: to, Capability: capability.Name, Version: capability.Version,
		Digest: capability.Digest, CapabilityID: capability.ID,
		Params: params, State: StateReady,
	})
	return plan, nil
}

// declaredRef 把一次 emit 的 `to` 解析到父 next 的**声明引用**上（R1-C6）：判据唯一
// 实现在 sourcespec.DeclaredRef（白名单不能有两套），这里只保留本包的名字。
func declaredRef(next []string, to string) (string, bool) {
	return sourcespec.DeclaredRef(next, to)
}

// declaredVersion 取引用里明确声明的版本（裸名 = 0 = 没有声明事实）。
func declaredVersion(ref string) int { return sourcespec.DeclaredVersion(ref) }

// refName 取引用的能力名部分（`name@version` → `name`）。
func refName(ref string) string { return sourcespec.RefName(ref) }
