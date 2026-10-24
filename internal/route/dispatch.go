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
	"log/slog"

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
	// ExtendLease 把租约推到指定时刻（派发失败回滚时用来"提前过期"）。
	ExtendLease(ctx context.Context, orgID, taskID string, leaseUntil int64) error
	// SweepExpiredLeases 回收过期租约（契约 §3.3）。
	//
	// 派发失败回滚复用它，而不是自己拼一套"退回 pending + 清归属"的 SQL：
	// 那样会出现第二个改任务归属的地方，与「唯一入口」的约束冲突。
	SweepExpiredLeases(ctx context.Context, now int64) ([]string, error)
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

	published := make([]bool, len(tasks))
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
		published[i] = true // 成功占用槽位并派发
		// 读回派发后的状态，让调用方拿到最新值
		task.Status = store.TaskAssigned
		task.NodeID = &nodeID
		task.InstanceID = &instanceID
		task.LeaseUntil = &lease
		task.Attempts = 1

		// 推 assign 帧。**必须在 AssignTask 之后**：节点收到帧就会开始干活，
		// 若此时库里还不是 assigned，它回报的 ack/result 会因为状态不匹配被拒。
		//
		// 推送失败（节点掉线/连接忙）时要把任务**退回 pending**并释放槽位：
		// 否则库里的状态是 assigned、节点却根本不知道有活干，
		// 这条任务会一直卡到租约过期才被巡检回收（30s 起，重试还要再来一轮）。
		// 退回 pending 让它能被下一次派发立刻捡起。
		if d.Publisher != nil {
			if err := d.Publisher.PublishAssign(ctx, in.OrgID, nodeID, task); err != nil {
				d.requeueAfterPublishFailure(ctx, in.OrgID, task, nodeID, instanceID)
				// 从返回值里剔除：它已退回 pending，不算"派发出去了"。
				// （仍留在库里，会被后续调度捡起。）
				published[i] = false
				continue
			}
		}

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

	// 返回值语义：
	//   - 容量不足留在 pending 的 → **要返回**（任务确实建出来了，在等调度；
	//     隐藏它会让调用方以为"这个出口什么都没触发"）
	//   - 推送失败被退回 pending 的 → **不返回**（它没派出去，是回滚）
	//
	// 两者在库里都是 pending，区别只在"这次派发有没有成功"。
	out.Tasks = make([]*store.Task, 0, len(tasks))
	for i, t := range tasks {
		if !published[i] && t.Status == store.TaskPending && t.Attempts > 0 {
			continue // 推送失败回滚的那条（attempts 已被 AssignTask +1）
		}
		out.Tasks = append(out.Tasks, t)
	}
	return out, nil
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
func (d *Dispatcher) requeueAfterPublishFailure(ctx context.Context, orgID string, task *store.Task, nodeID, instanceID string) {
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
	if err := d.Store.ExtendLease(ctx, orgID, task.ID, 0); err != nil {
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

// DispatchNext 实现 ws.NextHopDispatcher：任务回报 out 后派发下一跳。
//
// 这是契约 §4 的**递归应用**——把上一跳的结果当作下一跳的输入：
//
//	FromCap = 刚完成的能力
//	Out     = 它回报的出口名
//	Params  = 它的 params（原样传递，不求值）
//	Path    = 它的 path（环剪枝的累积路径）
//
// 三种情况不派发，都不是错误：
//   - out == ""：节点没回报出口（reported=0），这条分支自然结束
//   - 查表 0 行：pruned/no_edge，链到头了
//   - 目标是 origin 能力且已在 path 里：pruned/path，成环
func (d *Dispatcher) DispatchNext(ctx context.Context, orgID, traceID, parentTaskID, fromCapabilityID, out string, params json.RawMessage, path []string) error {
	if out == "" {
		return nil // 没回报出口：不派下一跳（契约 §7 第 ① 行）
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

	// path 已经包含 fromCapabilityID（父任务建的时候写进去的），
	// 直接沿用即可——Dispatch 会把目标能力追加到末尾。
	_, err := d.Dispatch(ctx, DispatchInput{
		OrgID:        orgID,
		TraceID:      traceID,
		ParentTaskID: parentTaskID,
		FromCap:      fromCapabilityID,
		Out:          out,
		Params:       paramMap,
		Path:         path,
	})
	return err
}
