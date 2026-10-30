// Package route 实现事件链的路由判定：给定上游「能力 + 出口名」，算出要建哪些任务。
//
// 它是本仓库唯一一个知道「怎么从出口表走到任务」的地方：
//
//	① edges 查表 → N 行
//	② 校参数（各自 params_schema）+ 环剪枝 + 跨组织用户校验
//	③ node_capabilities 查节点
//	④ 按负载（或粘性钉住）挑一台
//	⑤ 建任务
//
// 前三步都在建任务**之前**，所以剪枝不产生任务行（契约 §4）。
//
// 实现上分成两段：①–③（含构造 pending 任务）是 **plan**——只读，不落库、
// 不占槽位、不推帧；④–⑤ 是 **launch**——占槽位 + 推 assign 帧。拆开是 #140 的要求：
// 下一跳的「父任务终态 + 子任务建行」必须在同一个事务里，而 publish 是网络操作、
// 不能进事务，所以 plan 必须能在事务之前单独跑（见 PlanNextHop / PublishTasks）。
//
// 但剪枝**一律留痕**：每一条剪枝都写一行 events（source='route'，#142），
// 否则「这条边为什么没走」在触发那一刻之后就永远查不到了。
// 本包不写 SQL：所有查表/落库都走 Store 接口，便于用 mock 覆盖全部分支。
package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 派发 ----------

// DispatchInput 是一次派发的输入。
type DispatchInput struct {
	OrgID string
	// NamespaceID 是这次派发的**隔离维度**（契约 §3 / 设计文档 §5）。
	//
	// 派发四步里 ① 查出口表、③ 找节点、⑤ 建任务都按它过滤/落库；② 校参数、
	// ④ 挑机器与命名空间无关，完全不变。
	//
	// 谁来给：根任务由 handler 给、来源触发由调用方从 src.NamespaceID 传进来、
	// 下一跳由父任务的 NamespaceID 继承（见 PlanNextHop）。
	//
	// 空值是**错误**，不是「回退默认命名空间」（见 checkInput）：回退会让
	// 「调用方忘了带 namespace」变成「悄悄写进了 default」，是最难查的那类串数据。
	NamespaceID  string `json:"namespaceId"`
	TraceID      string
	ParentTaskID string
	// FromCap 是上游标识：默认是上游能力定义 ID；
	// FromIsSource 为 true 时它是**来源名**（入口边，如 "github"）。
	FromCap      string
	FromIsSource bool
	// Out 是上游回报的出口名。"" 表示上游没回报——此时不派发、也不记剪枝。
	Out    string
	Params map[string]any
	// TargetParams 按目标能力 ID 指定该目标的参数（源用）。
	//
	// 为什么必须**按目标**分别给，而不是一份公共 Params：
	// 每个能力有自己的 params_schema（字段名、必填项都不同）。一个源同时
	// 喂 analyze-pr（要 changedLines）和 deploy-preview（要 env）时，
	// 一份公共 params 必然对不上其中至少一边，触发时被 param_schema 剪枝，
	// 而用户配完以为好了——那正是要消灭的静默失败。
	//
	// 某目标没有条目时回退到 Params（保持对 events / 普通派发的兼容）。
	TargetParams map[string]map[string]any

	// DryRun 为 true 时只算"会派发什么、会剪枝什么"，**不落库、不推送**。
	//
	// 给「试运行」用：用户改完参数，保存前想看到"这条配置到底能不能跑"。
	// 复用 Dispatch 的同一套判定，所以试运行和真实触发不会出现分歧
	// （那种"试运行说能跑、真点却剪枝"的差异最伤信任）。
	//
	// DryRun 下 Tasks 里的任务只有能力/参数/节点信息，ID 等字段为空。
	DryRun bool

	// Targets 非空时**跳过 edges 查表**，直接把事件派给这些能力。
	//
	// 这是「源」（source）用的路径：源不建边，它自己就是起点，出口是
	// 它明确配置好的能力列表（source_targets）。
	//
	// 为什么不给源也建边：
	//   - 边是"能力 A 的某个出口 → 能力 B"，而源不是能力，没有 outputs 可枚举；
	//   - 用户为了配一个定时器去理解 kind=origin、去手建一条边，是多余的心智负担。
	// 所以源走这条直连路径，其余（参数校验 / 找节点 / 占槽位 / 建任务）完全复用。
	Targets []string
	// Path 是从根到本任务的能力序列（含本任务的能力），用于环剪枝。
	Path []string
}

// DispatchOutput 是一次派发的结果：建成 N 个任务，或 N 条剪枝记录。
//
// 两者可以同时非空：扇出里某条边参数不过、另一条正常建任务，是合法结果（契约 §4 ②）。
type DispatchOutput struct {
	// json tag 必须写：这个结构会直接出现在 HTTP 响应里
	// （/v1/events、/v1/sources/{id}/fire、/v1/hooks/{token}）。
	// 不写的话 Go 默认用字段名 "Tasks"/"Pruned"，前端按 camelCase 读会得到
	// undefined——曾经就是这样：派发明明成功、前端却显示"0 个任务"。
	Tasks  []*store.Task  `json:"tasks"`
	Pruned []PrunedRecord `json:"pruned"`
}

// Store 是派发需要的最小 store 表面（便于 mock 测试，也便于将来换实现）。
//
// *store.DB 满足这个接口。
type Store interface {
	// EdgesFrom 是派发第 ① 步的唯一查表入口。
	//
	// fromIsSource 决定 fromKey 的解释（见 store.Edge.FromIsSource）：
	// false → 上游能力 id；true → 来源名（入口边）。
	//
	// 命名空间维度是必须的：同名能力可以在两个命名空间里各有一条**不同的**出口边，
	// 少这个参数就会把别人的边当自己的（契约 §3 第 ① 步）。
	EdgesFrom(ctx context.Context, namespaceID string, fromIsSource bool, fromKey, fromPort string) ([]string, error)
	GetCapabilityByID(ctx context.Context, namespaceID, id string) (*store.CapabilityVersion, error)
	// NodesForCapability 只返回**本命名空间**里声明了该能力的节点（契约 §3 第 ③ 步）。
	//
	// 能力 id 全局唯一，所以这里天然只会匹配到一个命名空间的声明；仍然按
	// namespace_id 过滤一遍，防的是**数据被写坏**（见 store I11）。
	NodesForCapability(ctx context.Context, namespaceID, capabilityID string) ([]string, error)
	ListInstances(ctx context.Context, nodeID string) ([]*store.NodeInstance, error)
	// FreeSlotsByNode 一次拿到多个节点的空闲槽位数（#145：派发不能被 N+1 拖慢）
	FreeSlotsByNode(ctx context.Context, nodeIDs []string) (map[string]int, error)
	// LeaseAndAssign 在一个事务里完成「占槽位 + pending→assigned + 写归属/attempts」（#141）。
	//
	// 没有空闲槽位返回 store.ErrNoCapacity 且**什么都没写**（事务回滚）；
	// 其他错误同样带事务回滚，调用方不需要再补一次 ReleaseInstance。
	LeaseAndAssign(ctx context.Context, namespaceID, taskID, nodeID string, leaseUntil int64) (string, error)
	ReleaseInstance(ctx context.Context, nodeID, instanceID string) error
	ReleaseInstanceByTask(ctx context.Context, nodeID, taskID string) error
	GetPin(ctx context.Context, stickyKey string) (*store.NodePin, error)
	PutPin(ctx context.Context, p *store.NodePin) error
	CreateTasks(ctx context.Context, ts []*store.Task) error
	// ExtendLease 把租约推到指定时刻（派发失败回滚时用来"提前过期"）。
	ExtendLease(ctx context.Context, namespaceID, taskID string, leaseUntil int64) error
	// SweepExpiredLeases 回收过期租约（契约 §3.3）。
	//
	// 派发失败回滚复用它，而不是自己拼一套"退回 pending + 清归属"的 SQL：
	// 那样会出现第二个改任务归属的地方，与「唯一入口」的约束冲突。
	SweepExpiredLeases(ctx context.Context, now int64) ([]string, error)
	EnsureGraphVersion(ctx context.Context, namespaceID string) (*store.GraphVersion, error)
	// NamespaceOrgID 把命名空间反推回组织。
	//
	// 派发里唯一还剩的组织级规则是「params.to 必须是本组织成员」（契约 §10.1.1），
	// 以及 task.OrgID / 日志。下一跳拿不到 org，但只要知道父任务的 namespace
	// 就能把它推出来（I10：namespace.org_id 是唯一事实源）。
	NamespaceOrgID(ctx context.Context, namespaceID string) (string, error)
	IsOrgMember(ctx context.Context, orgID, userID string) (bool, error)

	// InsertPruneEvent 落一条剪枝事件（#142）。
	//
	// 它是 d.prune() 的唯一落库出口：剪枝的「产生」与「留痕」不能分叉，
	// 否则总会有某个分支只进了 HTTP 响应、没进库——那就又是一个查不到的剪枝。
	// 语义（幂等、idem_key 恒 NULL）由 store 实现，本包不关心细节。
	InsertPruneEvent(ctx context.Context, e *store.Event) (bool, error)
}

// Dispatcher 按派发四步算出要建的任务。
type Dispatcher struct {
	Store Store

	// UserScoped 是「params.to 必须属于本组织」的能力名集合（契约 §10.1.1）。
	//
	// 这是 params_schema 管不到的业务规则：schema 只能说「to 是非空字符串」，
	// 「这个人属于本组织」要单独查 org_members。
	// 每加一个跨组织（推送/私信）能力，都要在这里补一条。
	UserScoped map[string]bool

	// Now 返回当前 unixepoch 秒（测试可注入）。
	Now func() int64

	// Publisher 把刚派发的任务推给节点（assign 帧）。
	//
	// 为 nil 时只落库不推送（测试、以及"仅配置不跑"的场景）。
	// **非 nil 时推送失败会让 Dispatch 返回错误**：任务已经 assigned 在库里、
	// 但节点不知道有活干，这种状态必须让调用方知道（否则任务会静默卡到租约过期）。
	Publisher Publisher
}

// Publisher 把一条已派发的任务推给目标节点。
//
// 由 ws 层实现（NodeHub）。抽成接口是为了让 route 包不依赖 ws 包。
type Publisher interface {
	// PublishAssign 向 nodeID 推送 assign 帧；节点不在线返回 error。
	//
	// 返回 error 时派发器会把任务退回 pending（并释放槽位），
	// 让下一次派发能立刻重试——而不是让它卡到租约过期。
	PublishAssign(ctx context.Context, orgID, nodeID string, task *store.Task) error
}

// New 构造一个 Dispatcher，默认把 notify.phone 视为用户维度能力。
func New(st Store) *Dispatcher {
	return &Dispatcher{
		Store:      st,
		UserScoped: DefaultUserScoped(),
		Now:        store.Now,
	}
}

// DefaultUserScoped 返回默认的用户维度能力集合。
func DefaultUserScoped() map[string]bool {
	return map[string]bool{
		// notify.phone 的 params.to 是用户 ID（契约 Q1：推送目标是用户，不是组织）
		"notify.phone": true,
	}
}

// candidate 是一条「已通过全部判定、等待建任务/派发」的目标。
//
// 它是 plan 与 launch 之间的唯一交接物：plan 只算候选（纯读），launch 只负责
// 占槽位/推帧。#140 要求「终态 + 建行」同事务、publish 在事务外，
// 所以这两半必须能分开调用（事务之前只能跑 plan 那一半）。
type candidate struct {
	cap    *store.CapabilityVersion
	params map[string]any
	nodes  []string
	// central 表示这个能力由**中枢自己执行**，不找节点。
	// 典型是 notify.phone：没人会写一个"发推送节点"。
	central bool
}

// Dispatch 执行派发四步（契约 §4），返回要建的任务或剪枝记录。
//
// 顺序（前三步都在建任务之前）：
//
//	① edges 查表 → N 行；0 行 → pruned/no_edge，结束
//	② 逐条边校验：环剪枝(path) → 参数(param_schema) → 跨组织用户(foreign_user)
//	③ node_capabilities 查节点；0 行 → pruned/no_node
//	④ 按负载或粘性钉住挑一台；挑不到容量就先把任务留 pending（不剪枝）
//	⑤ 事务内批量建任务 + 逐条派发
//
// 实现是 plan（①②③ + 试运行早退 + 构造 pending 任务，只读）→ CreateTasks →
// launch（④⑤ 占槽位/推帧）三段。对外签名、返回值语义、剪枝留痕、日志与拆分前一致。
func (d *Dispatcher) Dispatch(ctx context.Context, in DispatchInput) (*DispatchOutput, error) {
	if err := d.checkInput(in); err != nil {
		return nil, err
	}

	out := &DispatchOutput{Tasks: []*store.Task{}, Pruned: []PrunedRecord{}}

	tasks, cands, err := d.plan(ctx, in, out)
	if err != nil {
		return nil, err
	}
	// 没有候选（链到头 / 全被剪枝）或试运行：plan 已把结果
	//（剪枝记录 / 试运行任务）填进 out，这里直接返回。
	if len(cands) == 0 || in.DryRun {
		return out, nil
	}

	// 先落库（原子），再派发。落库失败什么都没发生；
	// 派发失败的任务留在 pending，等后续调度（不会丢任务）。
	if err := d.Store.CreateTasks(ctx, tasks); err != nil {
		return nil, fmt.Errorf("route: create tasks: %w", err)
	}

	if err := d.launch(ctx, in, tasks, cands, out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkInput 是 Dispatch 与 PlanNextHop 共用的前置校验。
//
// 抽出来的理由：PlanNextHop 绕过了 Dispatch 直接调 plan，若不校验，
// Store 为 nil 时会在 plan 里 panic（旧 DispatchNext 走 Dispatch，本来就有这层校验）。
//
// 为什么这里不再要求 OrgID：org_id 现在是**派生量**（I10，由 namespaces 行决定），
// 派发四步的每一次 store 调用都只吃 namespaceID。而下一跳根本拿不到 org
// （见 PlanNextHop：子任务只继承父任务的 namespace）。OrgID 仍有用的地方
// （跨组织用户校验、task.OrgID、日志）在 plan 里按需从命名空间反查，见 plan。
func (d *Dispatcher) checkInput(in DispatchInput) error {
	if d.Store == nil {
		return fmt.Errorf("route: dispatcher has no store")
	}
	if d.Now == nil {
		d.Now = store.Now
	}
	// 命名空间必填：这里**不**回退默认命名空间（契约 §3 / §4）。
	//
	// 复用 store 的哨兵错误（ErrNamespaceRequired）是有意的：handler 只要
	// errors.Is(err, store.ErrNamespaceRequired) 就能把这个失败翻成 400，
	// 不必为「路由层少传了 namespace」再造一个只有它自己认识的错误类型。
	if in.NamespaceID == "" {
		return fmt.Errorf("route: empty namespace_id: %w", store.ErrNamespaceRequired)
	}
	if in.TraceID == "" {
		return fmt.Errorf("route: empty trace_id")
	}
	return nil
}

// plan 是派发的**只读**前半段：① 出口早退 + ② 查出口表 + ③ 逐条边判定
// （环/参数/跨组织用户/找节点）+ DryRun 早退 + 构造 pending 任务。
//
// 它**不落库、不占槽位、不推帧**（任务只有 ID/字段，尚未写进 tasks 表）。
// 唯一的写入是两处既有副作用，都不属于「派发结果」：
//   - EnsureGraphVersion：按需落一次图版本（契约 §10.2）；
//   - InsertPruneEvent：剪枝留痕（#142，best-effort、事务外，见 d.prune）。
//
// 为什么 plan 能同步跑（#140）：plan 全是只读查询（出口表/能力/节点），
// 把真正的网络操作（publish）留在后台就够了。而「必须先有计划才能进
// 『父任务终态 + 子任务建行』的事务」，所以 plan 挪到了节点的读循环里同步执行。
//
// 返回的 tasks 与 cands **一一对应、顺序一致**（launch 依赖这个不变量）。
// DryRun 时把试运行任务填进 out.Tasks，并返回 (nil, nil, nil)。
func (d *Dispatcher) plan(ctx context.Context, in DispatchInput, out *DispatchOutput) (tasks []*store.Task, cands []candidate, err error) {
	// ① 上游没回报出口：正常结束的分支，不查表、不剪枝（契约 §7 第 ① 行）。
	if in.Out == "" {
		return nil, nil, nil
	}

	// org_id 是派生量（I10）：根任务由调用方带进来；下一跳只拿得到父任务的
	// namespace（见 PlanNextHop），这里就反查一次——绝不“猜”默认 org，也不
	// 拿空 org 去查 org_members（那会把本组织用户的推送静默剪成 foreign_user）。
	// 只在真的缺了才查：根派发（handler/源/触发器）这条路一次查询也不多付。
	if in.OrgID == "" {
		orgID, err := d.Store.NamespaceOrgID(ctx, in.NamespaceID)
		if err != nil {
			return nil, nil, fmt.Errorf("route: resolve org for namespace %s: %w", in.NamespaceID, err)
		}
		in.OrgID = orgID
	}

	// ② 查出口表；源（Targets 非空）跳过这一步，直接用配置好的目标
	var targets []string
	if len(in.Targets) > 0 {
		targets = in.Targets
	} else {
		var err error
		// 只查**本命名空间**的出口表（契约 §3 第 ① 步）：别的命名空间里
		// 同名能力的边不参与派发。
		targets, err = d.Store.EdgesFrom(ctx, in.NamespaceID, in.FromIsSource, in.FromCap, in.Out)
		if err != nil {
			return nil, nil, fmt.Errorf("route: lookup edges: %w", err)
		}
		if len(targets) == 0 {
			d.prune(ctx, in, out, PrunedRecord{
				Reason: ReasonNoEdge,
				Port:   in.Out,
				Detail: fmt.Sprintf("no edge from %s port %q", in.FromCap, in.Out),
			})
			return nil, nil, nil
		}
	}

	// 图版本只落一次（按需落版，契约 §10.2）。图版本属于命名空间（edges 按 namespace 重建）。
	graph, err := d.Store.EnsureGraphVersion(ctx, in.NamespaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("route: pin graph version: %w", err)
	}

	// ③ 逐条边：环剪枝 → 参数 → 跨组织用户 → 找节点
	for _, targetID := range targets {
		capDef, err := d.Store.GetCapabilityByID(ctx, in.NamespaceID, targetID)
		if err != nil {
			return nil, nil, fmt.Errorf("route: load target capability %s: %w", targetID, err)
		}

		// 环剪枝：目标已在路径里 → 静默剪枝，但必须留痕
		if pathContains(in.Path, targetID) {
			d.prune(ctx, in, out, PrunedRecord{
				Reason:       ReasonPath,
				CapabilityID: targetID,
				Port:         in.Out,
				Detail:       fmt.Sprintf("%s@%d already in path %v (cycle pruned)", capDef.Name, capDef.Version, in.Path),
			})
			continue
		}

		// 该目标实际使用的参数：优先 TargetParams，回退到公共 Params
		targetParams := in.Params
		if tp, ok := in.TargetParams[targetID]; ok && tp != nil {
			targetParams = tp
		}

		// 参数校验：不过只跳过这一条边，不影响其他边（扇出可部分成功）
		if msg := validateParams(capDef.ParamsSchema, targetParams); msg != "" {
			d.prune(ctx, in, out, PrunedRecord{
				Reason:       ReasonParamSchema,
				CapabilityID: targetID,
				Port:         in.Out,
				Detail:       fmt.Sprintf("%s@%d params_schema: %s", capDef.Name, capDef.Version, msg),
			})
			continue
		}

		// 跨组织用户校验（params_schema 管不到的业务规则）
		if pruned, rec := d.checkUserScoped(ctx, in.OrgID, capDef, targetParams); pruned {
			rec.Port = in.Out // 留痕要有完整证据：哪条边、哪个出口
			d.prune(ctx, in, out, rec)
			continue
		}

		// 内置中枢能力（如 notify.phone）：**跳过找节点**。
		//
		// 这类能力没有节点执行者——推送是中枢自己的能力。若照常走
		// node_capabilities 查询必然是 0 行，于是 pruned/no_node，
		// 推送永远发不出去。这是「内置能力」与「自定义能力」唯一的分岔点。
		if bc, ok := store.IsBuiltinCapability(capDef.Name, capDef.Version); ok && bc.Central {
			cands = append(cands, candidate{
				cap: capDef, params: targetParams, central: true,
			})
			continue
		}

		// 找节点（只在本命名空间的在线节点里找，契约 §3 第 ③ 步）
		nodes, err := d.Store.NodesForCapability(ctx, in.NamespaceID, targetID)
		if err != nil {
			return nil, nil, fmt.Errorf("route: nodes for capability %s: %w", targetID, err)
		}
		if len(nodes) == 0 {
			d.prune(ctx, in, out, PrunedRecord{
				Reason:       ReasonNoNode,
				CapabilityID: targetID,
				Port:         in.Out,
				Detail:       fmt.Sprintf("no node declares %s@%d", capDef.Name, capDef.Version),
			})
			continue
		}

		cands = append(cands, candidate{cap: capDef, params: targetParams, nodes: nodes})
	}

	if len(cands) == 0 {
		return nil, nil, nil
	}

	// DryRun 在这里返回：判定已经做完（目标存在、参数过、节点在线），
	// 但**不占槽位、不建任务、不推送**。这正好是"试运行"要的信息。
	if in.DryRun {
		for _, c := range cands {
			out.Tasks = append(out.Tasks, &store.Task{
				CapabilityID: c.cap.ID,
				Capability:   c.cap.Name,
				Version:      c.cap.Version,
				Status:       store.TaskPending,
			})
		}
		return nil, nil, nil
	}

	// 构造 pending 任务：只算字段（ID 在这里生成），**不落库**。
	tasks = make([]*store.Task, 0, len(cands))
	for _, c := range cands {
		paramsJSON, err := json.Marshal(c.params)
		if err != nil {
			return nil, nil, fmt.Errorf("route: marshal params: %w", err)
		}
		if len(paramsJSON) == 0 || string(paramsJSON) == "null" {
			paramsJSON = []byte("{}")
		}
		path := append(append([]string{}, in.Path...), c.cap.ID)
		task := &store.Task{
			ID:           store.NewTaskID(),
			NamespaceID:  in.NamespaceID, // ⑤ 根任务用入参；子任务由 store I11④ 强制继承父任务
			OrgID:        in.OrgID,
			TraceID:      in.TraceID,
			ParentTaskID: parentPtr(in.ParentTaskID),
			Path:         path,
			CapabilityID: c.cap.ID,
			Capability:   c.cap.Name,
			Version:      c.cap.Version,
			Params:       json.RawMessage(paramsJSON),
			Status:       store.TaskPending,
			CapHash:      c.cap.ContentHash,
			GraphID:      graph.ID,
		}
		tasks = append(tasks, task)
	}

	return tasks, cands, nil
}

// launch 是派发的后半段：④ 挑机器/占槽位 + ⑤ 推 assign 帧，最后算出 out.Tasks。
//
// 入参 tasks 必须**已落库**（Dispatch 里由 CreateTasks 完成），且与 cands 一一对应。
// 返回值语义与拆分前完全一致：
//   - 容量不足留在 pending 的 → **出现在 out.Tasks 里**（任务确实建出来了，在等调度）；
//   - 推帧失败被退回 pending 的 → **不出现在 out.Tasks 里**（它没派出去，是回滚）。
func (d *Dispatcher) launch(ctx context.Context, in DispatchInput, tasks []*store.Task, cands []candidate, out *DispatchOutput) error {
	published := make([]bool, len(tasks))
	centralTasks := make([]*store.Task, 0)
	for i, task := range tasks {
		c := cands[i]

		// 中枢执行的候选：不挑节点、不占槽位、不设租约。
		// 任务保持 pending，由中枢执行器（internal/executor）领取并推进状态机。
		// 用 pending 而不是新状态：九态是契约定的，不能为内部实现加状态。
		if c.central {
			centralTasks = append(centralTasks, task)
			continue
		}

		// 挑一台：粘性优先，否则负载最小
		nodeID, stickySelector, err := d.pickNode(ctx, in, c.cap, c.params, c.nodes)
		if err != nil {
			return err
		}
		// 租约必须在 LeaseAndAssign **之前**算好：占槽位 / 改状态 / 写归属是一个事务，
		// lease_until 是它的入参（#141）。
		lease := d.Now() + store.LeaseTTLSec
		// 派发后 20s 内必须 ack，所以初始租约取 min(TTL, AckWait)——
		// 等 ack 的是 20s 窗口，ack 之后才开始按心跳续 30s（契约 §3.3）。
		if ackLease := d.Now() + store.AckWaitSec; ackLease < lease {
			lease = ackLease
		}
		// 占槽位 + pending→assigned + 写归属/attempts = **一次事务**（#141）。
		// 任何一步失败都回滚，槽位不会泄漏——所以这里**没有**（也不需要）
		// 失败后补一次 ReleaseInstance 的语句。
		instanceID, err := d.Store.LeaseAndAssign(ctx, in.NamespaceID, task.ID, nodeID, lease)
		if errors.Is(err, store.ErrNoCapacity) {
			// 有节点声明但暂时没空：不是永久剪枝，任务留 pending 等调度。
			// （剪枝语义是「这条链在这里断了」，容量不够不是断链。）
			continue
		}
		if err != nil {
			return fmt.Errorf("route: lease+assign on %s: %w", nodeID, err)
		}
		published[i] = true // 成功占用槽位并派发
		// 读回派发后的状态，让调用方拿到最新值
		task.Status = store.TaskAssigned
		task.NodeID = &nodeID
		task.InstanceID = &instanceID
		task.LeaseUntil = &lease
		task.Attempts = 1

		// 推 assign 帧。**必须在 LeaseAndAssign 之后**：节点收到帧就会开始干活，
		// 若此时库里还不是 assigned，它回报的 ack/result 会因为状态不匹配被拒。
		//
		// 推送失败（节点掉线/连接忙）时要把任务**退回 pending**并释放槽位：
		// 否则库里的状态是 assigned、节点却根本不知道有活干，
		// 这条任务会一直卡到租约过期才被巡检回收（30s 起，重试还要再来一轮）。
		// 退回 pending 让它能被下一次派发立刻捡起。
		if d.Publisher != nil {
			if err := d.Publisher.PublishAssign(ctx, in.OrgID, nodeID, task); err != nil {
				d.requeueAfterPublishFailure(ctx, in.NamespaceID, task, nodeID, instanceID)
				// 从返回值里剔除：它已退回 pending，不算"派发出去了"。
				// （仍留在库里，会被后续调度捡起。）
				published[i] = false
				continue
			}
		}

		// 粘性钉住：能力开了 sticky 且是第一次落到这台机器时写 pin
		// （pin 钉的是「哪个命名空间里的哪台机器」，所以带 NamespaceID）。
		if stickySelector != nil {
			key := store.StickyKey(in.TraceID, c.cap.ID, *stickySelector)
			if err := d.Store.PutPin(ctx, &store.NodePin{
				StickyKey:   key,
				NamespaceID: in.NamespaceID,
				OrgID:       in.OrgID,
				NodeID:      nodeID,
				Selector:    *stickySelector,
			}); err != nil {
				return fmt.Errorf("route: put pin: %w", err)
			}
		}
	}

	// 返回值语义：
	//   - 容量不足留在 pending 的 → **要返回**（任务确实建出来了，在等调度；
	//     隐藏它会让调用方以为"这个出口什么都没触发"）
	//   - 推送失败被退回 pending 的 → **不返回**（它没派出去，是回滚）
	//
	// 两者在库里都是 pending，区别只在"这次派发有没有成功"。
	out.Tasks = make([]*store.Task, 0, len(tasks))
	for i, t := range tasks {
		if !published[i] && t.Status == store.TaskPending && t.Attempts > 0 {
			continue // 推送失败回滚的那条（attempts 已被 LeaseAndAssign +1）
		}
		out.Tasks = append(out.Tasks, t)
	}
	return nil
}

// prune 记录一条剪枝：先进返回值，再留痕落库（#142）。
//
// 唯一入口——「产生」与「留痕」不允许分叉（漏一处就是又一个查不到的剪枝），
// 所以 plan 里 5 个剪枝产生点（no_edge / path / param_schema / foreign_user /
// no_node）全部走这里，不再直接 append out.Pruned。
//
// 三条规则（契约 §4.3）：
//
//  1. 返回值语义不变：out.Pruned 照旧给 HTTP 响应（调用方当场就要看到）；
//  2. DryRun 不落库：试运行不能污染 events（否则用户每试一次就多一条假历史）；
//  3. 落库失败只记日志，**不向上抛错**——剪枝留痕是诊断，
//     不能反过来打断派发/建任务。
func (d *Dispatcher) prune(ctx context.Context, in DispatchInput, out *DispatchOutput, p PrunedRecord) {
	out.Pruned = append(out.Pruned, p)

	if in.DryRun {
		return
	}

	payload, err := json.Marshal(PruneEventPayload{
		FromCapability:   in.FromCap,
		Port:             p.Port,
		TargetCapability: p.CapabilityID,
		Detail:           p.Detail,
	})
	if err != nil {
		// 四个字段全是 string，理论上不可达；保底不 panic、不中断派发。
		slog.Warn("marshal prune event payload failed",
			"event", "route.prune.record_failed",
			"reason", string(p.Reason),
			"trace_id", in.TraceID,
			"error", err.Error())
		return
	}

	// 幂等/去重由 store 层负责（同 namespace + 同 trace + 同原因 + 同 payload 只落一行）。
	// NamespaceID 必填：events 是命名空间资源，缺它 store 会直接报 ErrNamespaceRequired，
	// 于是剪枝会「静默地写失败」——那就又把 #142 要消灭的「查不到的剪枝」还回来了。
	if _, err := d.Store.InsertPruneEvent(ctx, &store.Event{
		NamespaceID: in.NamespaceID,
		OrgID:       in.OrgID,
		TraceID:     in.TraceID,
		Source:      PruneEventSource,
		Type:        PruneEventType(p.Reason),
		Payload:     payload,
		CreatedAt:   d.now(),
	}); err != nil {
		slog.Warn("record prune event failed",
			"event", "route.prune.record_failed",
			"reason", string(p.Reason),
			"trace_id", in.TraceID,
			"capability_id", p.CapabilityID,
			"error", err.Error())
	}
}

// requeueAfterPublishFailure 把「已 assigned 但推不出去」的任务退回 pending。
//
// 关键是**复用租约巡检**而不是自己拼一套回滚：
//
//  1. ReleaseInstance 释放派发时占住的槽位
//  2. ExtendLease(…, 0) 把租约推到"已过期"
//  3. SweepExpiredLeases 走同一条回收路径（改状态 + 清归属 + 释放槽位）
//
// 这样"清理任务归属"的逻辑只存在一处（sweepOne），不会出现第二份实现。
// 失败只记日志：最坏情况是等 30s 后巡检自然兜底，结果一样。
func (d *Dispatcher) requeueAfterPublishFailure(ctx context.Context, namespaceID string, task *store.Task, nodeID, instanceID string) {
	if err := d.Store.ReleaseInstance(ctx, nodeID, instanceID); err != nil {
		slog.Warn("release instance after publish failure failed",
			"event", "route.publish.requeue",
			"task_id", task.ID, "node_id", nodeID, "error", err.Error())
	}
	// 把租约推到过去，然后走**同一条**巡检路径回收。
	//
	// 为什么不直接 Transition：租约巡检除了改状态，还要清空
	// node_id/instance_id/lease_until 并释放实例槽位。自己再写一遍
	// 就等于出现第二个改归属的地方，且容易漏步骤（漏了 node_id 会让
	// 下次派发以为任务还挂在旧节点上）。
	//
	// 代价是这中间有极短的时间窗任务仍是 assigned（租约已过期），
	// 但巡检的 CAS 保证只有一个执行者能改它。
	if err := d.Store.ExtendLease(ctx, namespaceID, task.ID, 0); err != nil {
		slog.Warn("expire lease after publish failure failed",
			"event", "route.publish.requeue",
			"task_id", task.ID, "error", err.Error())
		return
	}
	if _, err := d.Store.SweepExpiredLeases(ctx, d.now()); err != nil {
		slog.Warn("sweep after publish failure failed",
			"event", "route.publish.requeue",
			"task_id", task.ID, "error", err.Error())
		return
	}
	task.Status = store.TaskPending
	task.NodeID = nil
	task.InstanceID = nil
	task.LeaseUntil = nil
}

// now 返回当前 unixepoch 秒。
func (d *Dispatcher) now() int64 {
	if d.Now != nil {
		return d.Now()
	}
	return store.Now()
}

// checkUserScoped 校验 params.to 是否属于本组织（契约 §10.1.1）。
func (d *Dispatcher) checkUserScoped(ctx context.Context, orgID string, capDef *store.CapabilityVersion, params map[string]any) (bool, PrunedRecord) {
	if !d.UserScoped[capDef.Name] {
		return false, PrunedRecord{}
	}
	to, _ := params["to"].(string)
	if to == "" {
		// schema 已经要求 to 非空；走到这里说明 schema 没声明要求，
		// 保守处理：没有目标用户就不能推（不然会变成广播）。
		return true, PrunedRecord{
			Reason:       ReasonForeignUser,
			CapabilityID: capDef.ID,
			Detail:       fmt.Sprintf("%s requires a non-empty params.to", capDef.Name),
		}
	}
	member, err := d.Store.IsOrgMember(ctx, orgID, to)
	if err != nil {
		// 查库失败不能当成剪枝（会静默丢消息），向上抛
		return false, PrunedRecord{}
	}
	if !member {
		return true, PrunedRecord{
			Reason:       ReasonForeignUser,
			CapabilityID: capDef.ID,
			Detail:       fmt.Sprintf("params.to=%q is not a member of this org", to),
		}
	}
	return false, PrunedRecord{}
}

// pickNode 挑一台节点：粘性钉住优先，否则按负载（空闲槽位最多）挑。
//
// 返回 (nodeID, stickySelector, error)。stickySelector 非 nil 表示该能力开了 sticky，
// 调用方应写 pin。粘性命中时沿用钉住的那台（哪怕负载更高）。
func (d *Dispatcher) pickNode(ctx context.Context, in DispatchInput, capDef *store.CapabilityVersion, params map[string]any, candidates []string) (string, *string, error) {
	var selector *string
	if capDef.Sticky != "" {
		// 用**该目标实际的参数**算粘性选择值：多目标各有各的 params，
		// 拿公共 Params 会算出错的 key，钉到不该钉的节点上。
		sel := stickySelectorValue(capDef, params)
		selector = &sel
		key := store.StickyKey(in.TraceID, capDef.ID, sel)
		pin, err := d.Store.GetPin(ctx, key)
		if err == nil && pin != nil && containsStr(candidates, pin.NodeID) {
			if err := d.Store.PutPin(ctx, &store.NodePin{
				StickyKey:   key,
				NamespaceID: pin.NamespaceID, // 刷新沿用库里那条 pin 的命名空间
				OrgID:       pin.OrgID,
				NodeID:      pin.NodeID,
				Selector:    pin.Selector,
				CreatedAt:   pin.CreatedAt,
				LastUsedAt:  d.Now(),
			}); err != nil {
				return "", nil, fmt.Errorf("route: refresh pin: %w", err)
			}
			return pin.NodeID, nil, nil // 已钉住，不再重复写
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", nil, fmt.Errorf("route: read pin: %w", err)
		}
	}

	// 负载最小：空闲（alive 且 busy_task IS NULL）槽位最多的那台
	//
	// 一次聚合查询查完所有候选，而不是每个节点查一次 ListInstances：
	// 这段在**派发热路径**上，每个任务都要付一次（接力派发还会再乘 T 次）。
	// 节点数一多，N+1 就是吞吐的主要瓶颈，也是当前 node 数量上限的真正原因（#145）。
	freeSlots, err := d.Store.FreeSlotsByNode(ctx, candidates)
	if err != nil {
		return "", nil, fmt.Errorf("route: free slots by node: %w", err)
	}
	best := ""
	bestFree := -1
	for _, nodeID := range candidates {
		// 完全没有槽位申报的节点也算候选（它可能会在下一次 hello 申报），
		// 但优先级最低（free=0 已经体现了这一点）。
		if free := freeSlots[nodeID]; free > bestFree {
			best, bestFree = nodeID, free
		}
	}
	if best == "" {
		best = candidates[0]
	}
	return best, selector, nil
}

// stickySelectorValue 取粘性选择器的值：sticky 字段通常就是 params 里那个键名
// （如 sticky="worktree" → params["worktree"]），没有则用空串（单一桶）。
func stickySelectorValue(capDef *store.CapabilityVersion, params map[string]any) string {
	if v, ok := params[capDef.Sticky]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// ValidateParams 用 params_schema 校验参数，返回不合法原因（"" = 通过）。
//
// 导出供控制台在建源/改源时**立即**校验源配置的参数：
// 用户填完表单就该知道填错了，而不是等触发时被剪枝才发现。
// 与派发期用的是同一个函数——两边口径必须一致，否则会出现
// "建源说没问题、触发被剪枝"这种最让人困惑的情况。
func ValidateParams(schema json.RawMessage, params map[string]any) string {
	return validateParams(schema, params)
}

// validateParams 用 params_schema 校验参数，返回不合法原因（"" = 通过）。
func validateParams(schema json.RawMessage, params map[string]any) string {
	if len(schema) == 0 {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		// schema 本身坏了：拒绝派发（不能拿坏 schema 放行任何参数）
		return "invalid params_schema in capability definition"
	}
	if len(root) == 0 {
		return ""
	}
	if params == nil {
		params = map[string]any{}
	}
	return validateValue(root, params, "params")
}

// ---------- 小工具 ----------

func pathContains(path []string, id string) bool {
	for _, p := range path {
		if p == id {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func parentPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// PlanNextHop 只算「下一跳要建哪些任务」：**只读**，不落库、不占槽位、不推帧。
//
// 这是契约 §4 的**递归应用**——把上一跳的结果当作下一跳的输入：
//
//	namespaceID = 父任务的 NamespaceID（store.Task.NamespaceID，必须原样传入）
//	FromCap     = 刚完成的能力
//	Out         = 它回报的出口名
//	Params      = 它的 params（原样传递，不求值）
//	Path        = 它的 path（环剪枝的累积路径）
//
// 为什么第二个参数是 namespaceID 而不是 orgID（契约 §3 第 ⑤ 步）：
// 下一跳的子任务**必须继承父任务的命名空间**（I11④ 也由 store.CreateTasks
// 兜一遍：子任务显式给了不同命名空间就直接报 CrossNamespaceError）。
// 调用方只能把父任务行上的那个值原样递进来，不能自己挑一个。
//
// 三种情况不派发，都不是错误（返回空列表 + nil）：
//   - out == ""：节点没回报出口（reported=0），这条分支自然结束
//   - 查表 0 行：pruned/no_edge，链到头了
//   - 目标是 origin 能力且已在 path 里：pruned/path，成环
//
// 返回的任务是 pending、ID 已生成，但**尚未落库**：调用方要把它们交给
// 「写父任务终态 + 建子任务行」的同一个事务（store.ReportResultAndCreateTasks），
// 事务提交后再调 PublishTasks 推帧。这也是 #140 的形状：
// 行先以 pending + node_id NULL 落库，publish 在事务外（网络操作不能进事务）。
// 为什么 plan 可以同步跑在读循环里：它全是只读查询（出口表/能力/节点）。
//
// 剪枝留痕（InsertPruneEvent）在 plan 内、事务外，best-effort：若随后的事务
// 回滚，库里可能留下一条「这条边没走成」的痕迹——那是诊断信息，
// 比丢掉证据好，可接受（契约 §4）。
func (d *Dispatcher) PlanNextHop(ctx context.Context, namespaceID, traceID, parentTaskID, fromCapabilityID, out string, params json.RawMessage, path []string) ([]*store.Task, error) {
	if out == "" {
		return nil, nil // 没回报出口：不派下一跳（契约 §7 第 ① 行）
	}

	var paramMap map[string]any
	if len(params) > 0 {
		if err := json.Unmarshal(params, &paramMap); err != nil {
			paramMap = map[string]any{}
		}
	}
	if paramMap == nil {
		paramMap = map[string]any{}
	}

	in := DispatchInput{
		// org_id 不在这里传：子任务的 OrgID 由 store 从 namespace 派生（I10）。
		// NamespaceID 在这里只能是父任务的那一个（I11④：不接受调用方另指定）。
		NamespaceID:  namespaceID,
		TraceID:      traceID,
		ParentTaskID: parentTaskID,
		FromCap:      fromCapabilityID,
		Out:          out,
		Params:       paramMap,
		Path:         path,
	}
	// 旧 DispatchNext 走 Dispatch，本来就有这层校验；绕过 Dispatch 时要补上。
	if err := d.checkInput(in); err != nil {
		return nil, err
	}

	// planOut 只用来承接 plan 的剪枝留痕（plan 的签名要求有个 out）；
	// 剪枝已由 plan 包留痕落库，这里不回给调用方。
	planOut := &DispatchOutput{Tasks: []*store.Task{}, Pruned: []PrunedRecord{}}
	tasks, _, err := d.plan(ctx, in, planOut)
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// PublishTasks 对**已落库**的 pending 任务执行「挑机器 + 占槽位 + 推 assign 帧」。
//
// 它绝不建行、绝不新增建任务路径：任务行的唯一出口是 Dispatch 或
// PlanNextHop + 调用方的事务。这里只做「让已存在的 pending 行跑起来」，
// 状态推进也只能走 LeaseAndAssign（pending→assigned 的既有入口）。
//
// 为什么直接复用 RedispatchTask：它已经是「NodesForCapability + pickNode(粘性/负载)
// + LeaseAndAssign + PublishAssign + 推失败退回 pending」的既有实现，语义与派发
// 第 ④ 步完全一致。另写一份只会让「派发」与「重派」两条路径慢慢走偏。
//
// 为什么 publish 不能进事务：它是网络操作（写 WS 帧），握着 SQLite 写事务等
// 一个可能掉线的节点，会把整库的写入卡住；而且帧已经发出去了、事务再回滚，
// 两边就永久不一致。
//
// 为什么 publish 丢了也不丢任务（#140 的补偿路径）：任务行已经以
// pending + node_id NULL 落库。推帧失败时 RedispatchTask 会走
// requeueAfterPublishFailure 退回 pending；即使本 goroutine 随进程消失、
// 或进程在「事务提交」与「publish」之间退出，store.ListRedispatchable
// （internal/lease 的 sweeper 每 5s tick，Redispatch = Dispatcher.RedispatchTask）
// 也会把它捞回来重派。「建行进事务 + publish 在事务外」正是依赖这条兜底才成立。
//
// 中枢能力（store.IsBuiltinCapability(...).Central，如 notify.phone）**跳过**：
// 它们没有节点执行者，由 internal/executor 从 pending 里领取（ClaimCentralTasks）。
// 任务留 pending 是正确终态，不是失败。
//
// 逐条独立：一条失败不拦住其它条；返回第一个错误仅供调用方记日志。
//
// 关于「推帧失败」要不要算错误：RedispatchTask 把推不出去当成「这次没派成」
// 而**不**返回 error（任务已退回 pending，sweeper 会兜底重派）。但 #140 的
// 调用方需要知道「行建出来了、帧没发出去」，所以这里用「attempts 涨了却仍回
// pending」识别出推送回滚（判定规则与 launch 筛 out.Tasks 的规则一致），
// 造一个错误交给调用方记日志——它不是硬失败，不进任何状态机。
func (d *Dispatcher) PublishTasks(ctx context.Context, orgID string, tasks []*store.Task) error {
	var firstErr error
	// record 只做两件事：warn + 记住第一个错误，绝不中断后续任务。
	record := func(task *store.Task, err error) {
		slog.Warn("publish next-hop task failed",
			"event", "route.next_hop.publish_failed",
			"org_id", orgID,
			"task_id", task.ID,
			"capability", task.Capability,
			"error", err.Error())
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, t := range tasks {
		if t == nil {
			continue
		}

		// 中枢执行的能力：不找节点、不占槽位，任务留 pending 给执行器领取。
		if bc, ok := store.IsBuiltinCapability(t.Capability, t.Version); ok && bc.Central {
			continue
		}

		// plan 出来的任务自带 OrgID；只在缺失时用入参补上，
		// 避免拿空 org_id 去查库（那样什么都查不到，任务会默默留 pending）。
		if t.OrgID == "" {
			t.OrgID = orgID
		}

		beforeAttempts := t.Attempts
		ok, err := d.RedispatchTask(ctx, t)
		if err != nil {
			record(t, err)
			continue
		}
		if ok {
			continue
		}
		// 占过槽位（attempts 涨了）却又回到 pending：只有「推帧失败被退回」
		// 会留下这个形状（容量不足/没节点都不会碰 attempts）。
		if t.Attempts > beforeAttempts && t.Status == store.TaskPending {
			record(t, fmt.Errorf("route: publish assign for task %s failed, requeued to pending", t.ID))
		}
	}
	return firstErr
}

// RedispatchTask 尝试把一条已存在的 pending 任务重新派给节点。
//
// 与 Dispatch 的区别：Dispatch 是「从出口表算出要建哪些任务」，
// 本方法是「任务已经建好了，只是还没找到机器」——典型场景是租约过期后
// 被巡检退回 pending，或派发时容量不足。
//
// 不重新走 ①②（出口表/参数校验）：那些在建模时就过了，重走一遍只会
// 因为图变了而产生不一致（任务已存在，却不该存在——那是另一个问题）。
//
// 返回 (true, nil) 表示派发出去了；false 表示没派成（没有可用节点、
// 容量不足），调用方应保持 pending 并在退避后重试。
func (d *Dispatcher) RedispatchTask(ctx context.Context, task *store.Task) (bool, error) {
	if task == nil {
		return false, fmt.Errorf("route: redispatch nil task")
	}
	if task.Status != store.TaskPending {
		// 不是 pending 说明已被别人处理（派发/取消/回收），不是错误。
		return false, nil
	}
	// 重派也必须知道隔离维度（它决定节点列表与租约落在哪个命名空间）。
	// 库里不存在这种行（tasks.namespace_id 是必填）；真出现只能是调用方自己拼了一个，
	// 那宁可报错也不要拿空 namespace 去查：查不到节点会让任务默默卡在 pending。
	if task.NamespaceID == "" {
		return false, fmt.Errorf("route: redispatch task %s: %w", task.ID, store.ErrNamespaceRequired)
	}

	nodes, err := d.Store.NodesForCapability(ctx, task.NamespaceID, task.CapabilityID)
	if err != nil {
		return false, fmt.Errorf("route: redispatch nodes for %s: %w", task.CapabilityID, err)
	}
	if len(nodes) == 0 {
		// 没有节点声明这个能力。不是错误——节点可能临时全下线，
		// 保持 pending，等节点回来再派。
		return false, nil
	}

	// 复用派发第 ④ 步的挑机器逻辑（粘性 + 负载）。
	capDef, err := d.Store.GetCapabilityByID(ctx, task.NamespaceID, task.CapabilityID)
	if err != nil {
		return false, fmt.Errorf("route: redispatch load capability %s: %w", task.CapabilityID, err)
	}
	// 重派时用**任务自己存的 params**算粘性（不能空着，否则粘性形同虚设）
	var taskParams map[string]any
	if len(task.Params) > 0 {
		_ = json.Unmarshal(task.Params, &taskParams)
	}
	// stickySelector 非 nil 表示「该能力开了 sticky 且库里还没钉住」，派发成功后**必须**写 pin。
	// 不能像以前那样用 `_` 丢掉：接力派发（#140）不再经过 launch，这里是它唯一的
	// 写 pin 的地方（见下方 PutPin 段）。
	// 粘性/负载与命名空间无关（派发第 ④ 步不变），但 pin 要能追溯归属，
	// 所以把任务自己的 namespace 带进去。
	nodeID, stickySelector, err := d.pickNode(ctx, DispatchInput{OrgID: task.OrgID, NamespaceID: task.NamespaceID, TraceID: task.TraceID}, capDef, taskParams, nodes)
	if err != nil {
		return false, err
	}

	// 租约在调用前算好（LeaseAndAssign 的入参），算法与 Dispatch 一致。
	lease := d.now() + store.LeaseTTLSec
	if ackLease := d.now() + store.AckWaitSec; ackLease < lease {
		lease = ackLease
	}
	// 占槽位 + pending→assigned + 写归属/attempts = 一次事务（#141）。
	// 失败即回滚，槽位不会泄漏，所以不需要再调 ReleaseInstance。
	instanceID, err := d.Store.LeaseAndAssign(ctx, task.NamespaceID, task.ID, nodeID, lease)
	if errors.Is(err, store.ErrNoCapacity) {
		// 有节点但满了：不是永久失败，下轮再试。
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("route: redispatch lease+assign: %w", err)
	}

	task.Status = store.TaskAssigned
	task.NodeID = &nodeID
	task.InstanceID = &instanceID
	task.LeaseUntil = &lease
	task.Attempts++
	// 派出去了，清掉退避时间（下次若再失败会重新设）
	task.NotBefore = nil

	if d.Publisher != nil {
		if err := d.Publisher.PublishAssign(ctx, task.OrgID, nodeID, task); err != nil {
			d.requeueAfterPublishFailure(ctx, task.NamespaceID, task, nodeID, instanceID)
			return false, nil // 推不出去 = 这次没派成，下轮重试
		}
	}

	// 粘性钉住：这是 launch 第 ④⑤ 步里 PutPin 那一段的**同构副本**。
	//
	// 为什么必须在这里再写一遍：接力派发（#140）改走
	// PlanNextHop + PublishTasks → RedispatchTask 之后**不再经过 launch**，
	// 若不在这里接住 pickNode 返回的 selector，接力链上就永远不会有 pin——
	// 而 pickNode 只在「库里还没有 pin」时才返回 selector（已钉住返回 nil），
	// 于是 hop≥2 的 sticky 静默失效：第二次派发/巡检重派会按负载换机器，
	// 同一 trace 的同一 worktree 落到另一台节点上（对 sticky="worktree" 这类
	// 能力是结果可能不正确，不只是慢）。
	//
	// 位置与 launch 一致，两条都是硬要求：
	//   - 必须在 PublishAssign **成功之后**：上面那个 return false 的分支
	//     （推帧失败退回 pending）绝不能写 pin，否则重派会被钉死在一台
	//     刚刚推不出去的机器上；
	//   - Publisher == nil（只落库不推送）时也要写，与 launch 同口径。
	//
	// 写失败只记日志、不影响返回值：帧已经发出去了，这里报错也无法反悔；
	// 后果仅是这条 trace 的粘性退化为「按负载挑」（下一跳可能换机器），
	// 不影响任务本身已经派发成功的事实。
	if stickySelector != nil {
		key := store.StickyKey(task.TraceID, capDef.ID, *stickySelector)
		if err := d.Store.PutPin(ctx, &store.NodePin{
			StickyKey:   key,
			NamespaceID: task.NamespaceID,
			OrgID:       task.OrgID,
			NodeID:      nodeID,
			Selector:    *stickySelector,
		}); err != nil {
			slog.Warn("put sticky pin after redispatch failed",
				"event", "route.redispatch.put_pin_failed",
				"org_id", task.OrgID,
				"task_id", task.ID,
				"trace_id", task.TraceID,
				"capability_id", task.CapabilityID,
				"node_id", nodeID,
				"error", err.Error())
		}
	}
	return true, nil
}
