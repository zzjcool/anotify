package route

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- emit：唯一的产出通道 ----------
//
// 这个文件是 docs/design/emit-protocol.md §3/§4/§7 的中枢侧实现。
//
// 它和 Dispatch（事件/接力的派发）的关系，一句话：**判定完全复用，入口和事务不同**。
//
//   - 复用：emit 的路由判定就是 plan（出口表 → 逐 link 叠字面量 → ValidateParams →
//     path/foreign_user/no_node 剪枝 → 建 pending 行的形状）。所以「同一个 port
//     经 emit 派发」与「同一个 port 经接力派发」不可能给出两套结论。
//   - 不同：① 上游不是「刚完成的任务」，而是「一个还在跑的任务的某一次 emit」——
//     所以要校验任务状态与 seq；② 落库多一行 emissions，且它必须与子任务行
//     同事务（§7：进程在中间退出 = 静默丢一条 emit）。
//
// 幂等是这里的核心不变量，不是可选项：
//
//	节点侧有本地 outbox，未 ack 的 emit 会在重连后按 seq 重发；中枢按
//	UNIQUE(task_id, seq) 把它折成一次派发。少了这一步，「agent 在 emit 与 ack
//	之间崩溃」就变成「下游被派发 N 遍」。

// EmitInput 是一次 emit 的输入（由 emit 帧原样转来，emit-protocol §3.1）。
type EmitInput struct {
	// NamespaceID 是隔离维度。节点连接绑定了命名空间，帧里不带它（令牌即作用域）。
	NamespaceID string
	// TaskID 是上报者所属的任务（必须仍在运行态）。
	TaskID string
	// Seq 是任务内单调递增序号，从 1 开始（§4）。0 / 负数是非法值。
	Seq int64
	// Port 是出口名。它是**唯一**的路由输入：中枢按 (capability, port) 查 link，
	// emit 自己不能指定「调用哪个能力」（§9）。
	Port string
	// Data 是这次产出的 JSON 对象，直接就是下游的 params（不做字段搬运）。
	// 缺省 = {}。
	Data json.RawMessage
	// Note 是给人看的备注，**不参与路由**（§3.1）。
	Note string
}

// EmitOutput 是一次 emit 的处理结果（对应 emit_ack，§3.2）。
type EmitOutput struct {
	TaskID string `json:"taskId"`
	Seq    int64  `json:"seq"`
	// Routed 是派发出去的下游条数（= 建出的子任务行数）。
	Routed int `json:"routed"`
	// Pruned 是被剪掉的 link 条数（no_node / foreign_user / path / param_*）。
	Pruned int `json:"pruned"`
	// Reasons 是逐条剪枝原因，给日志/trace 用（emit_ack 只带计数）。
	Reasons []PrunedRecord `json:"prunedReasons,omitempty"`
	// Duplicate 表示这一条是**重发**：计数取首次结果，本次没有建任何任务（§3.2）。
	Duplicate bool `json:"duplicate,omitempty"`
}

// emit 的拒收错误（全部由调用方翻译成 emit_ack.error，见 ws.handleEmit）。
//
// 为什么用哨兵值而不是字符串：节点据此决定「重发还是停止」（emit-protocol §2 的退出码），
// 而字符串匹配在第一次改文案时就会静默失效。文案本身仍带上下文（哪个 seq、缺哪段）。
var (
	// ErrEmitTaskNotFound 任务行不存在（被 retention 清掉，或帧里的 task_id 是编的）。
	ErrEmitTaskNotFound = errors.New("emit: unknown task")
	// ErrEmitTaskClosed 任务已非运行态（终态 / 租约已过期）。
	//
	// 「已终态」必须拒：允许晚到的 emit 改动已终结的任务，等于允许它凭空产生下游
	// （emit-protocol §8）。节点侧看到它应当停止执行（exit 5）。
	ErrEmitTaskClosed = errors.New("emit: task is not running")
	// ErrEmitSeqGap seq 缺口：1..seq-1 没有全部落库（§4 缺口策略）。
	//
	// 缺口是**丢消息的证据**，所以明确报错让节点重发缺失项，而不是猜/跳过/等超时。
	ErrEmitSeqGap = errors.New("emit: seq gap")
	// ErrEmitSeqReused 同一 seq 被用于不同内容（§4）。
	//
	// 这是节点 bug，不能静默覆盖：覆盖会让「库里那一次派发」变成无法解释的孤岛。
	ErrEmitSeqReused = errors.New("emit: seq reused with different payload")
	// ErrEmitBadData data 不是 JSON 对象（§6 第 4 步；节点本该先拦住，中枢兜第二道）。
	ErrEmitBadData = errors.New("emit: data must be a JSON object")
)

// emitAcceptableStatus 判定任务是否还能收 emit。
//
// 只有四个「运行中」的状态可以：pending（还没派到节点）、assigned、acked、running。
// 其余一律拒：
//   - succeeded / failed / canceled / abandoned —— 已终结，不能再产生下游；
//   - lease_expired —— 租约已被回收，执行权不在上报者手上（它以为还在跑，
//     但中枢已经把任务交还给调度，此时接受 emit 会与重派产生两条并行的下游链）。
//
// 判据放在这里而不是用 store.IsTerminalStatus：后者只覆盖四个终态，
// 而我们要的是**白名单**（新增状态时默认拒绝，而不是默认接受）。
func emitAcceptableStatus(status string) bool {
	switch status {
	case store.TaskPending, store.TaskAssigned, store.TaskAcked, store.TaskRunning:
		return true
	}
	return false
}

// Emit 处理一次 emit：查任务 → 校验 seq → 幂等 → 按 link 派发（与 emission 同事务）。
//
// 顺序即语义（每一步失败都是「这次 emit 不收」，且都**不建任何子任务行**）：
//
//	① 任务行必须存在且可 emit（白名单状态）
//	② data 必须是 JSON 对象（缺省 = {}）
//	③ 幂等：同 (task_id, seq) 已存在 → 回首次结果；内容不同 → 拒
//	④ 缺口：1..seq-1 必须齐全（§4）
//	⑤ 路由判定走 plan（复用派发四步的 ②③ 与建行形状）
//	⑥ 一个事务：写 emission + 建子任务行（§7）
//	⑦ 事务提交后 publish（网络操作不能进事务；失败只记日志，任务留在 pending 由巡检重派）
//
// ③ 在 ④ 之前是必须的：重发的那一条 seq 天然「不连续」（它自己就是 n，而库里
// 已有 n），先判缺口会把每一次正常重发都当成缺口拒掉。
//
// 并发：两个并发到达的同 (task_id, seq) 帧各自跑完 ⑤，插入时被唯一键折成一个
// 赢家；输的那一个看到 first=false，读到首次结果后原样返回——**不会重复派发**。
// 乱序到达（seq 2 先于 seq 1）由 ④ 挡住（回 seq gap，节点据此重发 1）。
func (d *Dispatcher) Emit(ctx context.Context, in EmitInput) (*EmitOutput, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("route: emit: dispatcher has no store")
	}
	if d.Now == nil {
		d.Now = store.Now
	}
	if in.NamespaceID == "" {
		return nil, fmt.Errorf("route: emit: %w", store.ErrNamespaceRequired)
	}
	if in.TaskID == "" {
		return nil, fmt.Errorf("route: emit: empty task_id")
	}
	if in.Seq <= 0 {
		return nil, fmt.Errorf("route: emit: seq must be >= 1 (got %d)", in.Seq)
	}
	if in.Port == "" {
		return nil, fmt.Errorf("route: emit: empty port")
	}

	// ① 任务行必须存在，且仍在运行态。
	task, err := d.Store.GetTask(ctx, in.NamespaceID, in.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrEmitTaskNotFound, in.TaskID)
	}
	if err != nil {
		return nil, fmt.Errorf("route: emit: load task %s: %w", in.TaskID, err)
	}
	if !emitAcceptableStatus(task.Status) {
		return nil, fmt.Errorf("%w: task %s is %s", ErrEmitTaskClosed, task.ID, task.Status)
	}

	// ② data 必须是 JSON 对象。空 = {}（§3.1：可缺省）。
	params, err := decodeEmitData(in.Data)
	if err != nil {
		return nil, err
	}

	// ③ 幂等：这条 seq 已经处理过 → 回首次结果，**不重复派发**。
	//
	// 先读一次（快速路径）+ ⑥ 的 first=false 兜住并发窗口：两次都能覆盖
	// 「内容冲突要拒」这件事，只是第一次不浪费一次 plan。
	prev, err := d.Store.GetEmission(ctx, in.TaskID, in.Seq)
	if err == nil {
		return duplicateOutput(prev, in)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("route: emit: read emission %s#%d: %w", in.TaskID, in.Seq, err)
	}

	// ④ 缺口：1..seq-1 必须齐全（emit-protocol §4）。
	if err := d.checkEmitSeqGap(ctx, in); err != nil {
		return nil, err
	}

	// ⑤ 路由判定。与接力派发**同一套** plan（出口表 / 字面量覆盖 / 逐 link 校验 /
	// 环/用户/节点剪枝），所以两条入口不可能给出两套结论。
	//
	// ParentTaskID = 本任务、Path = 本任务的 path（含本能力）：子任务是「这次 emit
	// 的一次接力」，环检测因此仍然有效。
	planIn := DispatchInput{
		NamespaceID:  in.NamespaceID,
		TraceID:      task.TraceID,
		ParentTaskID: task.ID,
		FromCap:      task.CapabilityID,
		Out:          in.Port,
		Params:       params,
		Path:         task.Path,
	}
	planOut := &DispatchOutput{Tasks: []*store.Task{}, Pruned: []PrunedRecord{}}
	var tasks []*store.Task
	var cands []candidate
	// 一个 port 没接下游**不是错误**（emit-protocol §8：routed=0）：这里先探一次
	// 出口表，就是为了不把 plan 的 no_edge 剪枝当成「有什么失败了」——
	// 事件流里也不该出现一条「这个出口没有边」的剪枝，那在 emit 语义下是正常终止。
	targets, err := d.Store.EdgesFrom(ctx, in.NamespaceID, false, task.CapabilityID, in.Port)
	if err != nil {
		return nil, fmt.Errorf("route: emit: lookup edges: %w", err)
	}
	if len(targets) > 0 {
		tasks, cands, err = d.plan(ctx, planIn, planOut)
		if err != nil {
			return nil, err
		}
	}

	// ⑥ emission + 子任务行同一个事务（§7）。
	// Routed 取**建出的行数**（不是 publish 成功数）：它在事务里就确定了，
	// 而重复 seq 要回同一份计数——publish 的结果不进这个数。
	emission := &store.Emission{
		TaskID:    in.TaskID,
		Seq:       in.Seq,
		Port:      in.Port,
		Data:      in.Data,
		Routed:    len(tasks),
		Pruned:    len(planOut.Pruned),
		CreatedAt: d.now(),
	}
	first, err := d.Store.InsertEmissionAndCreateTasks(ctx, emission, tasks)
	if err != nil {
		return nil, fmt.Errorf("route: emit: record emission %s#%d: %w", in.TaskID, in.Seq, err)
	}
	if !first {
		// 并发下的败者：别人先写了同一个 (task_id, seq)。读首次结果、不重复派发。
		prev, err := d.Store.GetEmission(ctx, in.TaskID, in.Seq)
		if err != nil {
			return nil, fmt.Errorf("route: emit: read first emission %s#%d: %w", in.TaskID, in.Seq, err)
		}
		return duplicateOutput(prev, in)
	}

	// ⑦ 事务已提交 → 推送（网络操作在事务外，§7）。
	//
	// 失败**不算 emit 失败**：行已经落库（pending + node_id NULL），
	// internal/lease 的巡检会把它捡回来重派。把它当错误回给节点反而有害——
	// 节点会重发同一个 seq，而那条 seq 已经存在，于是只会命中幂等。
	if len(tasks) > 0 {
		if err := d.launch(ctx, planIn, tasks, cands, planOut); err != nil {
			slog.Warn("emit: publish downstream failed, tasks left pending",
				"event", "route.emit.publish_failed",
				"namespace_id", in.NamespaceID,
				"task_id", in.TaskID,
				"seq", in.Seq,
				"port", in.Port,
				"tasks", len(tasks),
				"error", err.Error())
		}
	}

	return &EmitOutput{
		TaskID:  in.TaskID,
		Seq:     in.Seq,
		Routed:  len(tasks),
		Pruned:  len(planOut.Pruned),
		Reasons: planOut.Pruned,
	}, nil
}

// checkEmitSeqGap 校验 1..seq-1 全部已落库（emit-protocol §4）。
//
// 判据：条数 == seq-1 **且** 最大 seq == seq-1。
// 两个条件都要：只有条数会放过「中间有洞、尾部很长」的坏数据（那种数据下面
// 每一个新 seq 都该被拒），只有最大值会把「1,3」误判成连续。
func (d *Dispatcher) checkEmitSeqGap(ctx context.Context, in EmitInput) error {
	count, maxSeq, err := d.Store.EmissionSeqStatus(ctx, in.TaskID)
	if err != nil {
		return fmt.Errorf("route: emit: emission seq status %s: %w", in.TaskID, err)
	}
	if count == int(in.Seq-1) && maxSeq == in.Seq-1 {
		return nil
	}
	// 缺口区间：从已连续到的位置 +1 到 seq-1。丢了哪几条要写在错误里——
	// 节点据此重发缺的那几条（不是从头重发）。
	return fmt.Errorf("%w: seq %d but task %s has %d emission(s) (max seq %d); missing seq %d..%d",
		ErrEmitSeqGap, in.Seq, in.TaskID, count, maxSeq, maxSeq+1, in.Seq-1)
}

// decodeEmitData 把帧里的 data 解成 params（必须是 JSON 对象；缺省 = {}）。
//
// 为什么在中枢再判一次「是不是对象」：节点已经做过（§6 第 4 步），但那一道在
// 不可信的一侧。这里拒掉的代价只是一条 emit_ack.error，放进去的代价是
// 「params 不是字典」这种形状漏进派发（applyLiterals / ValidateParams 都以字典为前提）。
func decodeEmitData(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON: %v", ErrEmitBadData, err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: got %s", ErrEmitBadData, store.JSONTypeName(v))
	}
	return obj, nil
}

// duplicateOutput 把「库里已有的那条 emission」翻成幂等输出。
//
// 内容不同 → 拒（§4：同一 seq 不同内容是节点 bug，不能静默覆盖）。
// 内容相同 → 回首次的计数，Duplicate=true（节点据此知道这是重发，不必再等）。
//
// 内容比较用**语义相等**（解成 any 再比）而不是字节相等：节点重发的是它 outbox
// 里那份原始字节，正常情况下完全相同；但「{"a":1}」与「{ "a": 1 }」是同一个
// data，把它们判成冲突只会制造假故障。键序不同同理。
func duplicateOutput(prev *store.Emission, in EmitInput) (*EmitOutput, error) {
	if prev.Port != in.Port || !sameJSON(prev.Data, in.Data) {
		return nil, fmt.Errorf("%w: seq %d of task %s was port %q with different data",
			ErrEmitSeqReused, in.Seq, in.TaskID, prev.Port)
	}
	return &EmitOutput{
		TaskID:    prev.TaskID,
		Seq:       prev.Seq,
		Routed:    prev.Routed,
		Pruned:    prev.Pruned,
		Duplicate: true,
	}, nil
}

// sameJSON 判定两份 JSON 是否语义相同（空值与 "{}" / "null" 都算没有 data）。
func sameJSON(a, b json.RawMessage) bool {
	na := normalizeJSON(a)
	nb := normalizeJSON(b)
	return reflect.DeepEqual(na, nb)
}

// normalizeJSON 把原始字节解成可比较的形状；空/坏数据一律折成空对象。
func normalizeJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// 坏 JSON：退回字节比较（调用方会用 reflect.DeepEqual 比这两个 any，
		// 这里给一个稳定的可比较值——同一份坏字节得到同一个字符串）。
		return string(raw)
	}
	if v == nil {
		return map[string]any{}
	}
	return v
}
