package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// NodeHub 维护「节点 ID → 活跃连接」的映射，让派发器能把 assign 帧推给节点。
//
// 生命周期：
//
//	连接建立（hello 校验通过）→ Register
//	连接断开                   → Unregister
//	派发出新任务               → PublishAssign(nodeID, task)
//
// 一个节点同时只允许一条连接：后连的顶掉前一条（节点重启/换网络时常见）。
// 顶掉而不是拒绝，是因为旧连接往往已经是半死状态（TCP 还没超时），
// 拒绝会让节点永远连不上。
//
// 除此之外 hub 还持有**任务级**的在飞表（#222）：一个 taskID 同一时刻只许一条连接执行。
// 连接表按 nodeID 去重挡不住这个——节点重启会换连接，而任务行还是原来那一条。
type NodeHub struct {
	mu    sync.RWMutex
	conns map[string]*nodeSession // nodeID → session
	// inflight 是「正在被某条连接执行」的任务 → 那条连接（#222 幂等护栏）。
	// 写入时机是 assign 帧**写成功之后**（写失败的一帧没到对端，登记它反而会挡住
	// 真正该拿到它的连接）；清除时机是 result 落库之后 / 连接 cleanup 的 releaseAll。
	inflight map[string]*nodeSession

	// LogsTimeout 覆盖一次 logs_request 的等待上限（0 → LogsRequestTimeout，5s）。
	//
	// 它存在的唯一目的是**可测**：契约要求「不要写死 sleep」，而超时路径
	// （等不到回包 → 200 + available:false + reason=timeout）是这条链最关键的降级分支。
	// 生产装配不设它（用默认 5s），测试把它调到几十毫秒。
	LogsTimeout time.Duration
}

// NewNodeHub 构造一个空 hub。
func NewNodeHub() *NodeHub {
	return &NodeHub{
		conns:    make(map[string]*nodeSession),
		inflight: make(map[string]*nodeSession),
	}
}

// ErrTaskInflight 表示这条任务已经被**另一条活着的连接**执行（#222）。
//
// 派发方（route）看到它应当把任务退回 pending 等下一次巡检，而不是硬推——
// 硬推会让同一个任务被两个进程同时执行，结果各自落库，后者覆盖前者。
var ErrTaskInflight = errors.New("task is already in flight on another connection")

// Register 让一条连接成为该节点的当前连接（顶掉旧的）。
func (h *NodeHub) Register(nodeID string, s *nodeSession) {
	h.mu.Lock()
	old := h.conns[nodeID]
	h.conns[nodeID] = s
	h.mu.Unlock()
	if old != nil && old != s {
		// 旧连接先踢掉：不踢的话两条连接会同时收到 assign，
		// 而同一任务只该有一个执行者。
		old.kill("superseded by a newer connection")
	}
}

// Unregister 移除连接（只在它仍是当前连接时移除，避免误删新连接）。
//
// 返回值是「被移除的是不是当前连接」。它回答的是**摘的那一刻**的事实，
// 所以 cleanup **不用**它决定要不要写 node 级状态（alive/online）——
// 那会导致「旧连接把新连接刚写好的 online/alive=1 改回去」的窗口（审查 M2，
// 详见 nodeSession.cleanup 里 ② 的注释）。要问「现在是不是我」就用 current()。
// 保留返回值只是因为它能表达「这次移除有没有生效」，对诊断/测试有用。
func (h *NodeHub) Unregister(nodeID string, s *nodeSession) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.conns[nodeID]; ok && cur == s {
		delete(h.conns, nodeID)
		return true
	}
	return false
}

// claim 登记「taskID 正在被 s 执行」。
func (h *NodeHub) claim(taskID string, s *nodeSession) {
	if taskID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight == nil {
		h.inflight = make(map[string]*nodeSession, 4)
	}
	h.inflight[taskID] = s
}

// release 清掉一条任务的在飞登记（幂等），**但只清「登记的还是 s」的那条**。
//
// 为什么必须校验 owner（审查 n4）：`delete(inflight, taskID)` 无条件删除时，
// 「A 迟到的 release 删掉 B 刚 claim 的条目」就是一个静默的双执行入口——
// in-flight 是 #222 的唯一内存护栏，护栏缺口 = 同一任务可能被派给两条连接。
// 当前仅有 forget → release 一条路径，活性靠 handleResult 的早退顺序撑着
// （rejectForeignExecution / 「任务不在本节点」两条早退分支都**不** forget），
// 但那是**隐式**不变量：以后多一个 publish/forget 入口就会变成误删。
// 带上 owner 之后，这条不变路由签名自己保证。
func (h *NodeHub) release(taskID string, s *nodeSession) {
	if taskID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.inflight[taskID]; ok && cur == s {
		delete(h.inflight, taskID)
	}
}

// owner 返回正在执行 taskID 的连接（没有则 nil）。
func (h *NodeHub) owner(taskID string) *nodeSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.inflight[taskID]
}

// releaseAll 清掉一条连接名下的全部在飞登记。
//
// 调用时机是 cleanup 里、**落库释放（ResolveLostTasks）之后**：
// 反过来先放开在飞表，正在巡检的派发器可以抢在 DB 释放之前把同一个任务派给
// 另一条连接 → 双执行。保留到释放完成之后，最坏也只是这次派发失败、下轮重试。
func (h *NodeHub) releaseAll(s *nodeSession) {
	if s == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for taskID, cur := range h.inflight {
		if cur == s {
			delete(h.inflight, taskID)
		}
	}
}

// current 取节点**此刻**的当前连接（没有则 nil）。
//
// 名字里的 current 是重点：它只回答「现在是不是你」，不回答「刚才是不是你」。
// cleanup 判断「该不该写 node 级状态（alive/online）」时必须用它，而不是用
// Unregister 的返回值——那个值是**摘连接那一刻**的快照，两者在并发下并不等价
// （见 nodeSession.cleanup 里 ② 的注释，审查 M2）。
func (h *NodeHub) current(nodeID string) *nodeSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[nodeID]
}

// Online 报告节点是否有活跃连接（控制台/inspect 用）。
func (h *NodeHub) Online(nodeID string) bool {
	return h.current(nodeID) != nil
}

// OnlineCount 返回在线节点数。
func (h *NodeHub) OnlineCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// ErrNodeOffline 表示目标节点没有活跃连接。
var ErrNodeOffline = fmt.Errorf("node is offline")

// PublishAssign 向节点推送 assign 帧（实现 route.Publisher）。
//
// namespaceID 是任务的命名空间（#162）。路由比较走的也是**命名空间**：
// 一个连接 = 一个节点 = 一个命名空间（令牌绑命名空间），所以「任务与节点同命名空间」
// 是唯一正确的约束——两个命名空间的任务绝不能互推（组织层已在迁移 14 删除）。
//
// 任务已落库为 assigned，节点不在线时返回 ErrNodeOffline——
// 让派发方知道「这个任务推不出去了」。调用方可以选择：
//   - 保持 assigned，等节点重连后由租约巡检回收再派（当前行为）
//   - 立刻改回 pending（需要额外的事务编排，暂不处理）
//
// 六步，顺序即语义（#222 / #221 / 审查 n2-n4）：
//  1. 节点不在线 → ErrNodeOffline；跨命名空间 → 拒绝（都是既有行为）
//  2. 该 taskID 已被**另一条活着的连接**占为在飞 → ErrTaskInflight（同一连接重推幂等）
//  3. 回读**库里的**权威行（attempts / seq_in_trace / status）——读不到就返回错误、**不发帧**（n2）；
//     已经终态则不发帧、不占用任何登记，直接返回 nil（n3）
//  4. 编帧（attempts / seq_in_trace 一律用库里那份覆盖调用方内存里的值）+ 体积闸门
//     → 超限 *store.ParamsTooLargeError，**不发帧**
//  5. 写帧（失败不登记）
//  6. 写成功 → hub 登记在飞（claim 先，fencing 优先）+ 会话登记归属
//
// 回读必须排在编帧与写帧**之前**：attempts / seq_in_trace 是这一帧要发出去的字段，
// 而调用方传进来的 task 不可信（见下），所以只能先读库、再编帧、最后对**最终字节**量体积。
func (h *NodeHub) PublishAssign(ctx context.Context, namespaceID, nodeID string, task *store.Task) error {
	s := h.current(nodeID)
	if s == nil {
		return ErrNodeOffline
	}
	if s.node.NamespaceID != namespaceID {
		// 绝不跨命名空间推任务（不变量 I10/I11）：跨命名空间派发会把一台机器的
		// 执行能力泄漏给别的隔离域。
		return fmt.Errorf("node %s belongs to another namespace", nodeID)
	}
	// 护栏（#222）：同一 taskID 只许一条活着的连接执行。
	// 同一条连接重复推同一任务 = 幂等刷新，允许（覆盖旧登记）。
	if cur := h.owner(task.ID); cur != nil && cur != s {
		return fmt.Errorf("%w: task %s is already being executed by another connection (node %s)",
			ErrTaskInflight, task.ID, s.node.ID)
	}

	// ③ 回读**库里的**权威行，必须在编帧/写帧**之前**（审查 n2/n3）。
	//
	// 为什么不能信调用方传的 task.Attempts：route 在派发成功后把这一列写死成 1
	// （internal/route/dispatch.go 的 `task.Attempts = 1`，不管库里是 2、3…），
	// 而 cleanup 的 ResolveLostTasks 用库里的 attempts 做 CAS（`WHERE attempts = ?`）→
	// 世代记错的后果是**整条主动释放被静默跳过**：任务留在 assigned、槽位被 busy_task
	// 钉住，直到 20s ack 窗口的租约过期才由巡检兜底。
	// 同理 task.SeqInTrace 在内存里恒为 0（只有库里那份有意义），而节点要用它命名
	// 本地 runs/<trace>/<cap>@<ver>/<seq>-a<n> 目录——发错了就是两代执行写进同一个目录。
	//
	// 为什么读失败就**不发帧**：旧实现在写帧成功后才回读，读失败就退回入参继续——
	// 那正好把上面那个缺陷原样带回来（只是触发条件从「route 硬编码」变成「一次读失败」）。
	// 返回错误后 route 的 requeueAfterPublishFailure 会把任务退回 pending 重试，
	// 那条路径本来就是干净的；比「发了一帧却记着错世代」安全得多。
	fresh, err := s.h.Store.GetTask(ctx, namespaceID, task.ID)
	if err != nil {
		return fmt.Errorf("read authoritative attempts for task %s: %w", task.ID, err)
	}
	if fresh == nil {
		return fmt.Errorf("read authoritative attempts for task %s: not found", task.ID)
	}
	// 已经终态 = 病态竞态（这条任务在我读它之前就跑完了：同一条连接上的重复派发，
	// 或巡检/result 撞车）。此时**不发帧**，也不 claim/own（n3）：
	// 给一条终态任务登记在飞位 + 归属，它再也不会被 forget（节点根本不会再报这条任务）
	// → 内存登记泄漏到连接结束，还挡着同 taskID 的后续派发。
	// 返回 nil 而不是错误：任务本身已经有结论了，多一个错误只会让 route 把它退回 pending
	// 重跑一遍（那才是真错）。
	if store.IsTerminalStatus(fresh.Status) {
		return nil
	}

	// ④ 帧骨架由纯函数给出（好测）；P1 的三项（ports / expects / token）**在这里**补：
	// 它们要读库，而 assignFrameFor 不该因此变成一个需要 DB 的函数。
	//
	// 为什么补在发布路径而不是派发路径：PublishAssign 是唯一出口——
	// launch（首次派发）与 RedispatchTask（重派/巡检）都走它，所以这几项
	// 不会因为走了哪条路径而漏一份（这也是「不要改 assignFrameFor 签名」的原因：
	// 它会逼着每个调者拿一个 ctx+store）。
	frame := assignFrameFor(task)
	// attempts / seq_in_trace **只取库里那份**：帧是要发出去的机器事实，不是展示缓存。
	frame.Attempts = fresh.Attempts
	frame.SeqInTrace = fresh.SeqInTrace
	s.enrichAssign(ctx, task, frame)
	data, err := Encode(frame)
	if err != nil {
		return err
	}
	// 体积闸门（#221）：超限的 assign 帧**绝不写进连接**。
	//
	// 写进去的后果是把 agent 的帧读上限（1MiB）打炸 → 对端 1009 → 连接死 → 它手上的
	// 任务停在 assigned、槽位钉死（就是 #218 的另一条复现路径）。返回 typed error，
	// route 会把任务退回 pending（既有行为），连接与槽位都不受影响。
	if limit := store.MaxTaskParamsBytes + assignFrameOverheadBytes; len(data) > limit {
		size := len(task.Params)
		if size <= store.MaxTaskParamsBytes {
			// params 本身没超，但编码成帧（字段名 + JSON 包装）后超了：报实际帧体积。
			size = len(data)
		}
		return &store.ParamsTooLargeError{TaskID: task.ID, Size: size, Limit: store.MaxTaskParamsBytes}
	}
	// ⑤ 写帧：写失败不登记（没送到对端的一帧不该占住在飞位）。
	if err := s.sendBytes(ctx, data); err != nil {
		return err
	}
	// ⑥ claim 先、own 后（fencing 优先）：先立屏障再去登记归属，
	// 中间的极窄窗口里别人看到的只是“这条任务在飞、但还没登记归属”（挡住重派）；
	// 反过来（先 own 后 claim）则会短暂地“归属已记、护栏没立”。
	h.claim(task.ID, s)
	s.own(task.ID, fresh.Attempts)
	return nil
}

// assignFrameOverheadBytes 是 assign 帧除 params 之外的余量（字段名 + 租约/任务 ID 等）。
// 4KiB 远大于实际开销（约 120B），留它是为了让「params 刚好在上限内」的任务能正常派发。
const assignFrameOverheadBytes = 4 << 10

// enrichAssign 补全 assign 帧里那三项**推导出来的**字段（P1.1 / P1.4，设计 §3.4）。
//
//	ports   = 该能力定义的 outputs（I2 封闭集合的快照：节点据此当场拒掉写错的出口名）
//	expects = 按 port 推导的「下游要什么」（route.BuildExpects，不落库）
//	token   = 该任务的一次性令牌（本地 emit socket 鉴权）
//
// 全部是 **best-effort**（失败只记日志、**不阻断派发**）：
//   - 它们是降级信息：没有 ports 时节点仍能跑（只是 emit 时才报出口错），
//     没有 expects 时实现只能靠被拒后猜，没有 token 时只能等下次重发；
//   - 而派发失败会让这条任务退回 pending 重跑一次——代价大得多（可能是一次昂贵执行）。
//     这条取舍与 pinDigest（审计字段不拦派发）同一个口径。
//
// 幂等：token 走 store.EnsureTaskToken（CAS 只在为空时写）——断线重连重发 assign
// 时拿到的必须是同一个令牌，否则节点侧 outbox 的重发会被判成 token 无效。
func (s *nodeSession) enrichAssign(ctx context.Context, task *store.Task, f *AssignFrame) {
	capDef, err := s.h.Store.GetCapabilityByID(ctx, task.NamespaceID, task.CapabilityID)
	if err != nil {
		slog.Warn("assign enrichment: capability lookup failed",
			"event", "node.ws.assign.enrich",
			"node_id", s.node.ID,
			"task_id", task.ID,
			"capability_id", task.CapabilityID,
			"error", err.Error())
	} else {
		f.Ports = capDef.Outputs
		if s.h.Expects != nil && len(capDef.Outputs) > 0 {
			expects, err := s.h.Expects.BuildExpects(ctx, task.NamespaceID, task.CapabilityID, capDef.Outputs)
			if err != nil {
				slog.Warn("assign enrichment: build expects failed",
					"event", "node.ws.assign.enrich",
					"node_id", s.node.ID,
					"task_id", task.ID,
					"capability_id", task.CapabilityID,
					"error", err.Error())
			} else {
				f.Expects = expectsForFrame(expects)
			}
		}
	}

	token, err := s.h.Store.EnsureTaskToken(ctx, task.NamespaceID, task.ID, store.NewTaskToken())
	if err != nil {
		slog.Warn("assign enrichment: task token failed",
			"event", "node.ws.assign.enrich",
			"node_id", s.node.ID,
			"task_id", task.ID,
			"error", err.Error())
		return
	}
	f.Token = token
}

// expectsForFrame 把 route 推导出来的 expects 转成线上帧的形状。
//
// 为什么要转（而不是两边共用一个类型）：route 包不能 import ws（否则 route 依赖
// 传输层，而它现在只依赖 store）；而帧上的类型已冻结在 node_protocol.go 里。
// 两边形状必须一致，node_emit_test.go 的 TestExpectsFrameShapeMatches 用
// JSON 序列化结果钉死这一点（两边漂移会红）。
func expectsForFrame(in map[string]route.PortExpect) map[string]PortExpect {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]PortExpect, len(in))
	for port, e := range in {
		targets := make([]ExpectTarget, 0, len(e.Targets))
		for _, tg := range e.Targets {
			targets = append(targets, ExpectTarget{
				Capability: tg.Capability,
				Version:    tg.Version,
				Priority:   tg.Priority,
				Needs:      tg.Needs,
			})
		}
		out[port] = PortExpect{Schema: e.Schema, Targets: targets}
	}
	return out
}

// assignFrameFor 把任务转成 assign 帧。
//
// 载荷里**只有**能力名 + 版本 + params + 租约（不变量 I1）：
// 不含命令、不含脚本、不含 argv——节点自己知道怎么执行这个能力。
func assignFrameFor(task *store.Task) *AssignFrame {
	var params map[string]any
	if len(task.Params) > 0 {
		// 失败就发空 params：节点会按自己的 schema 校验并报错，
		// 比悄悄改写成别的值安全。
		if err := json.Unmarshal(task.Params, &params); err != nil {
			params = map[string]any{}
		}
	}
	if params == nil {
		params = map[string]any{}
	}

	var lease int64
	if task.LeaseUntil != nil {
		lease = *task.LeaseUntil
	}
	// max_duration_sec 是「这一份任务最多跑多久」的建议值。
	// 任务行没有携带能力 lifecycle（那是定义期的字段），所以这里给
	// 契约 §6.1 示例里的 3600s 作为上限预算；节点可以更早结束。
	// 真正的强制约束是租约（lease_until），不是这个值。
	const maxDuration = 3600

	return &AssignFrame{
		Type:        NodeFrameAssign,
		TaskID:      task.ID,
		Capability:  task.Capability,
		Version:     task.Version,
		Params:      params,
		LeaseUntil:  lease,
		MaxDuration: maxDuration,
		// Digest 是「就跑这一份字节」的钉子（契约 §6.1 / 设计 §7 P0.1）。
		//
		// 为什么必须在这里传：节点侧早就有按 digest 取产物的路径
		// （cmd/agent/client.go:549 → cache.getByDigest），但这条路以前**永远走不到**
		// —— 帧里没有 digest，节点只好回落到 index/<name>@<version>。
		// 后果是「中枢以为跑的是 A，节点跑的是被手改过的 A′」这种分叉无法被发现。
		// 空值（老任务行 / 没有产物）由 omitempty 变成「不出现」：老节点行为不变。
		Digest:  task.Digest,
		TraceID: task.TraceID,

		// Attempts / SeqInTrace 在这里**只取入参**（纯函数，不碰 DB）。
		//
		// 注意：入参上的这两个值在生产里是**不可信的**（route 把内存里的 Attempts
		// 写死成 1、SeqInTrace 恒为 0），所以 PublishAssign 发帧前会用库里回读的
		// fresh 覆盖掉它们（见 PublishAssign 的 ③ / ④）。这里只负责“若有人直接用它
		// 造帧，至少不会丢掉已知的字段”。
		Attempts:   task.Attempts,
		SeqInTrace: task.SeqInTrace,
	}
}

// PublishDeploy 向节点推送一帧 deploy（下发或撤销一份产物）。
//
// 与 PublishAssign 的区别是**不做任何任务级护栏**：部署是幂等的配置动作，
// 重复推同一份产物只是让节点把同样的内容再应用一遍（digest 内容寻址，
// 落盘也是幂等的）。这里只有两条拒绝：
//   - 节点没有活跃连接 → ErrNodeOffline（调用方据此标注「已排队」）
//   - 跨命名空间 → 拒绝（与任务派发同一条不变量 I10/I11）
//
// 与 assign 一样，**中枢绝不执行产物**：这里只是把一段 JSON 转发给节点。
func (h *NodeHub) PublishDeploy(namespaceID, nodeID string, f *DeployFrame) error {
	s := h.current(nodeID)
	if s == nil {
		return ErrNodeOffline
	}
	if s.node.NamespaceID != namespaceID {
		return fmt.Errorf("node %s belongs to another namespace", nodeID)
	}
	data, err := Encode(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), deploySendTimeout)
	defer cancel()
	return s.sendBytes(ctx, data)
}

// PublishUpgrade 向节点推送一帧 upgrade（CONTRACT-agent-upgrade §5）。
//
// 与 PublishDeploy 同形：不做任务级护栏，只有两条拒绝 —— 没有活跃连接
// （ErrNodeOffline，调用方据此回 409 node_offline）与跨命名空间。
//
// 帧里只有「目标版本 / sha256 / 下载路径」，二进制本身**不**走 WS：
// agent 是 15 MB 量级，塞进帧会阻塞同一条连接上的心跳与 assign（节点会掉线）。
func (h *NodeHub) PublishUpgrade(namespaceID, nodeID string, f *UpgradeFrame) error {
	s := h.current(nodeID)
	if s == nil {
		return ErrNodeOffline
	}
	if s.node.NamespaceID != namespaceID {
		return fmt.Errorf("node %s belongs to another namespace", nodeID)
	}
	data, err := Encode(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), upgradeSendTimeout)
	defer cancel()
	return s.sendBytes(ctx, data)
}

// upgradeSendTimeout 是单帧升级指令的写超时。
//
// 帧本身很小（几百字节），不需要部署那种 30s 宽限；10s 足够覆盖慢链路，
// 又不至于让控制台的一次点击挂太久。
const upgradeSendTimeout = 10 * time.Second

// deploySendTimeout 是单帧部署推送的写超时。
//
// 比 assign 宽松：产物最大 1 MiB（CONTRACT-artifact §4），在慢速链路上
// 写一帧可能需要几秒；而部署不像任务派发那样有租约在倒计时。
const deploySendTimeout = 30 * time.Second
