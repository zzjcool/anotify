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
// 本包不写 SQL：所有查表都走 Store 接口，便于用 mock 覆盖全部分支。
package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 派发 ----------

// DispatchInput 是一次派发的输入。
type DispatchInput struct {
	OrgID        string
	TraceID      string
	ParentTaskID string
	// FromCap 是上游能力定义 ID（events 起的链用 origin 能力）。
	FromCap string
	// Out 是上游回报的出口名。"" 表示上游没回报——此时不派发、也不记剪枝。
	Out    string
	Params map[string]any
	// Path 是从根到本任务的能力序列（含本任务的能力），用于环剪枝。
	Path []string
}

// DispatchOutput 是一次派发的结果：建成 N 个任务，或 N 条剪枝记录。
//
// 两者可以同时非空：扇出里某条边参数不过、另一条正常建任务，是合法结果（契约 §4 ②）。
type DispatchOutput struct {
	Tasks  []*store.Task
	Pruned []PrunedRecord
}

// Store 是派发需要的最小 store 表面（便于 mock 测试，也便于将来换实现）。
//
// *store.DB 满足这个接口。
type Store interface {
	// EdgesFrom 是派发第 ① 步的唯一查表入口。
	EdgesFrom(ctx context.Context, orgID, fromCapability, fromPort string) ([]string, error)
	GetCapabilityByID(ctx context.Context, orgID, id string) (*store.CapabilityVersion, error)
	NodesForCapability(ctx context.Context, orgID, capabilityID string) ([]string, error)
	ListInstances(ctx context.Context, nodeID string) ([]*store.NodeInstance, error)
	LeaseInstance(ctx context.Context, nodeID, taskID string) (string, error)
	ReleaseInstance(ctx context.Context, nodeID, instanceID string) error
	ReleaseInstanceByTask(ctx context.Context, nodeID, taskID string) error
	GetPin(ctx context.Context, stickyKey string) (*store.NodePin, error)
	PutPin(ctx context.Context, p *store.NodePin) error
	CreateTasks(ctx context.Context, ts []*store.Task) error
	AssignTask(ctx context.Context, orgID, taskID, nodeID, instanceID string, leaseUntil int64) error
	EnsureGraphVersion(ctx context.Context, orgID string) (*store.GraphVersion, error)
	IsOrgMember(ctx context.Context, orgID, userID string) (bool, error)
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

// Dispatch 执行派发四步（契约 §4），返回要建的任务或剪枝记录。
//
// 顺序（前三步都在建任务之前）：
//
//	① edges 查表 → N 行；0 行 → pruned/no_edge，结束
//	② 逐条边校验：环剪枝(path) → 参数(param_schema) → 跨组织用户(foreign_user)
//	③ node_capabilities 查节点；0 行 → pruned/no_node
//	④ 按负载或粘性钉住挑一台；挑不到容量就先把任务留 pending（不剪枝）
//	⑤ 事务内批量建任务 + 逐条派发
func (d *Dispatcher) Dispatch(ctx context.Context, in DispatchInput) (*DispatchOutput, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("route: dispatcher has no store")
	}
	if d.Now == nil {
		d.Now = store.Now
	}
	if in.OrgID == "" {
		return nil, fmt.Errorf("route: empty org_id")
	}
	if in.TraceID == "" {
		return nil, fmt.Errorf("route: empty trace_id")
	}

	out := &DispatchOutput{Tasks: []*store.Task{}, Pruned: []PrunedRecord{}}

	// ① 上游没回报出口：正常结束的分支，不查表、不剪枝（契约 §7 第 ① 行）。
	if in.Out == "" {
		return out, nil
	}

	// ② 查出口表
	targets, err := d.Store.EdgesFrom(ctx, in.OrgID, in.FromCap, in.Out)
	if err != nil {
		return nil, fmt.Errorf("route: lookup edges: %w", err)
	}
	if len(targets) == 0 {
		out.Pruned = append(out.Pruned, PrunedRecord{
			Reason: ReasonNoEdge,
			Port:   in.Out,
			Detail: fmt.Sprintf("no edge from %s port %q", in.FromCap, in.Out),
		})
		return out, nil
	}

	// 图版本只落一次（按需落版，契约 §10.2）
	graph, err := d.Store.EnsureGraphVersion(ctx, in.OrgID)
	if err != nil {
		return nil, fmt.Errorf("route: pin graph version: %w", err)
	}

	// ③ 逐条边：环剪枝 → 参数 → 跨组织用户 → 找节点
	type candidate struct {
		cap    *store.CapabilityVersion
		params map[string]any
		nodes  []string
	}
	var candidates []candidate

	for _, targetID := range targets {
		capDef, err := d.Store.GetCapabilityByID(ctx, in.OrgID, targetID)
		if err != nil {
			return nil, fmt.Errorf("route: load target capability %s: %w", targetID, err)
		}

		// 环剪枝：目标已在路径里 → 静默剪枝，但必须留痕
		if pathContains(in.Path, targetID) {
			out.Pruned = append(out.Pruned, PrunedRecord{
				Reason:       ReasonPath,
				CapabilityID: targetID,
				Port:         in.Out,
				Detail:       fmt.Sprintf("%s@%d already in path %v (cycle pruned)", capDef.Name, capDef.Version, in.Path),
			})
			continue
		}

		// 参数校验：不过只跳过这一条边，不影响其他边（扇出可部分成功）
		if msg := validateParams(capDef.ParamsSchema, in.Params); msg != "" {
			out.Pruned = append(out.Pruned, PrunedRecord{
				Reason:       ReasonParamSchema,
				CapabilityID: targetID,
				Port:         in.Out,
				Detail:       fmt.Sprintf("%s@%d params_schema: %s", capDef.Name, capDef.Version, msg),
			})
			continue
		}

		// 跨组织用户校验（params_schema 管不到的业务规则）
		if pruned, rec := d.checkUserScoped(ctx, in.OrgID, capDef, in.Params); pruned {
			rec.Port = in.Out // 留痕要有完整证据：哪条边、哪个出口
			out.Pruned = append(out.Pruned, rec)
			continue
		}

		// 找节点
		nodes, err := d.Store.NodesForCapability(ctx, in.OrgID, targetID)
		if err != nil {
			return nil, fmt.Errorf("route: nodes for capability %s: %w", targetID, err)
		}
		if len(nodes) == 0 {
			out.Pruned = append(out.Pruned, PrunedRecord{
				Reason:       ReasonNoNode,
				CapabilityID: targetID,
				Port:         in.Out,
				Detail:       fmt.Sprintf("no node declares %s@%d", capDef.Name, capDef.Version),
			})
			continue
		}

		candidates = append(candidates, candidate{cap: capDef, params: in.Params, nodes: nodes})
	}

	if len(candidates) == 0 {
		return out, nil
	}

	// ④ 挑机器 + ⑤ 建任务
	// 先把 task 全建出来（拿到 ID），再逐条挑机器占槽位，最后一次性落库。
	// 顺序理由：占槽位需要 task_id，而落库失败要能回滚已占的槽位。
	tasks := make([]*store.Task, 0, len(candidates))
	for _, c := range candidates {
		paramsJSON, err := json.Marshal(c.params)
		if err != nil {
			return nil, fmt.Errorf("route: marshal params: %w", err)
		}
		if len(paramsJSON) == 0 || string(paramsJSON) == "null" {
			paramsJSON = []byte("{}")
		}
		path := append(append([]string{}, in.Path...), c.cap.ID)
		task := &store.Task{
			ID:           store.NewTaskID(),
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

	// 先落库（原子），再派发。落库失败什么都没发生；
	// 派发失败的任务留在 pending，等后续调度（不会丢任务）。
	if err := d.Store.CreateTasks(ctx, tasks); err != nil {
		return nil, fmt.Errorf("route: create tasks: %w", err)
	}

	for i, task := range tasks {
		c := candidates[i]
		// 挑一台：粘性优先，否则负载最小
		nodeID, stickySelector, err := d.pickNode(ctx, in, c.cap, c.nodes)
		if err != nil {
			return nil, err
		}
		instanceID, err := d.Store.LeaseInstance(ctx, nodeID, task.ID)
		if errors.Is(err, store.ErrNoCapacity) {
			// 有节点声明但暂时没空：不是永久剪枝，任务留 pending 等调度。
			// （剪枝语义是「这条链在这里断了」，容量不够不是断链。）
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("route: lease instance on %s: %w", nodeID, err)
		}
		lease := d.Now() + store.LeaseTTLSec
		// 派发后 20s 内必须 ack，所以初始租约取 min(TTL, AckWait)——
		// 等 ack 的是 20s 窗口，ack 之后才开始按心跳续 30s（契约 §3.3）。
		if ackLease := d.Now() + store.AckWaitSec; ackLease < lease {
			lease = ackLease
		}
		if err := d.Store.AssignTask(ctx, in.OrgID, task.ID, nodeID, instanceID, lease); err != nil {
			// 释放刚占的槽位，任务留 pending
			_ = d.Store.ReleaseInstance(ctx, nodeID, instanceID)
			return nil, fmt.Errorf("route: assign task %s: %w", task.ID, err)
		}
		// 读回派发后的状态，让调用方拿到最新值
		task.Status = store.TaskAssigned
		task.NodeID = &nodeID
		task.InstanceID = &instanceID
		task.LeaseUntil = &lease
		task.Attempts = 1

		// 粘性钉住：能力开了 sticky 且是第一次落到这台机器时写 pin
		if stickySelector != nil {
			key := store.StickyKey(in.TraceID, c.cap.ID, *stickySelector)
			if err := d.Store.PutPin(ctx, &store.NodePin{
				StickyKey: key,
				OrgID:     in.OrgID,
				NodeID:    nodeID,
				Selector:  *stickySelector,
			}); err != nil {
				return nil, fmt.Errorf("route: put pin: %w", err)
			}
		}
	}

	out.Tasks = tasks
	return out, nil
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
func (d *Dispatcher) pickNode(ctx context.Context, in DispatchInput, capDef *store.CapabilityVersion, candidates []string) (string, *string, error) {
	var selector *string
	if capDef.Sticky != "" {
		sel := stickySelectorValue(capDef, in.Params)
		selector = &sel
		key := store.StickyKey(in.TraceID, capDef.ID, sel)
		pin, err := d.Store.GetPin(ctx, key)
		if err == nil && pin != nil && containsStr(candidates, pin.NodeID) {
			if err := d.Store.PutPin(ctx, &store.NodePin{
				StickyKey:  key,
				OrgID:      pin.OrgID,
				NodeID:     pin.NodeID,
				Selector:   pin.Selector,
				CreatedAt:  pin.CreatedAt,
				LastUsedAt: d.Now(),
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
	best := ""
	bestFree := -1
	for _, nodeID := range candidates {
		insts, err := d.Store.ListInstances(ctx, nodeID)
		if err != nil {
			return "", nil, fmt.Errorf("route: list instances of %s: %w", nodeID, err)
		}
		free := 0
		for _, in := range insts {
			if in.Alive && in.BusyTask == nil {
				free++
			}
		}
		// 完全没有槽位申报的节点也算候选（它可能会在下一次 hello 申报），
		// 但优先级最低（free=0 已经体现了这一点）。
		if free > bestFree {
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
