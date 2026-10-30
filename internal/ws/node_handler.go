package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// 心跳与租约时序（契约 §3.3）。
const (
	// defaultHeartbeatSec 是心跳间隔：welcome 帧下发给节点。
	DefaultHeartbeatSec = store.HeartbeatSec // 10s
	// defaultLeaseTTLSec 是租约 TTL：心跳把 lease_until 推到 now + 30。
	DefaultLeaseTTLSec = store.LeaseTTLSec // 30s
	// defaultAckWaitSec 是派发后等 ack 的上限。
	DefaultAckWaitSec = store.AckWaitSec // 20s
	// deadAfterHeartbeats 是判死倍数：超过 2×心跳没有上行帧即判死。
	DeadAfterHeartbeats = store.DeadAfterHeartbeats // 2
	// defaultWriteTimeout 是单帧写超时（避免慢节点把连接 goroutine 卡死）。
	defaultWriteTimeout = 10 * time.Second
)

// NodeHandler 处理 GET /v1/node/stream：节点连接的生命周期驱动。
//
// 职责边界：
//   - 握手与鉴权在 Upgrade **之前**（node_handshake.go 的 AuthenticateNode）
//   - 连接上的一切帧都用握手拿到的 (node_id, namespace_id)，不再查库
//     （命名空间随令牌走：令牌 → node 行 → namespace_id，见设计文档 §2.1）
//   - 心跳与租约续期（本文件）
//   - 结果帧推进状态机（node_session.go）
type NodeHandler struct {
	Store NodeStore

	// HeartbeatSec / LeaseTTLSec 可覆盖默认值（测试用）。
	HeartbeatSec int
	LeaseTTLSec  int

	// AcceptOptions 可覆盖 WS 升级选项。
	AcceptOptions *websocket.AcceptOptions

	// Now 返回当前 unixepoch 秒（测试可注入）。
	Now func() int64

	// Hub 维护「节点 → 活跃连接」，派发器通过它推送 assign 帧。
	// 为 nil 时自动创建一个（handler 自己持有，不共享）。
	Hub *NodeHub

	// NextHop 在任务成功回报 out 后接力派发下一跳（契约 §4 的递归应用）。
	//
	// 为 nil 时**不派下一跳**——链路就停在当前任务（测试/仅配置场景）。
	//
	// #140 把一次「派发」拆成 plan 与 publish 两段，两段的失败处理**故意不同**：
	//
	//   - plan 是只读查询（出口表/能力/节点），在**读循环里同步**跑。它是
	//     「父任务终态 + 子任务行同一个事务」的前提——必须先知道要建哪些子任务行
	//     才能把它们一起交给 ReportResultAndCreateTasks。plan 失败 = 这次结果
	//     没能落库（DB 故障，与今日 ReportResult 失败等价），所以不提交终态、
	//     回错误帧；父任务仍 running，租约到期后巡检退回 pending 重派，可恢复。
	//   - publish 是网络操作（挑机器 + 占槽位 + 推 assign 帧），在**后台 goroutine**
	//     里跑（不能进事务：握着 SQLite 写事务等一个可能掉线的节点会把整库写入卡住，
	//     且帧发出去后事务再回滚就永久不一致）。publish 失败/丢失**不回给节点**：
	//     父任务已经成功完了，子任务行也已落库为 pending + node_id NULL，
	//     由 store.ListRedispatchable / lease sweeper 兜底重派。
	NextHop NextHopDispatcher
}

// NextHopDispatcher 接力派发下一跳（由 route.Dispatcher 实现）。
//
// 抽成接口是为了让 ws 包不依赖 route 包（避免循环依赖）。
// 两条方法是 #140 的核心切分（见 NodeHandler.NextHop 的说明）：
//   - PlanNextHop 只算不落库（同步，事务前）
//   - PublishTasks 只推已落库的行（异步，事务后，绝不建行）
type NextHopDispatcher interface {
	// PlanNextHop 只算「下一跳要建哪些任务」：只读，不落库、不占槽位、不推帧。
	// 返回的任务是 pending、ID 已生成但尚未落库，由调用方放进父任务终态的事务里。
	//
	// namespaceID 是**父任务的命名空间**（#162）：出口表/能力/节点都按命名空间隔离，
	// 传节点的命名空间也不会错，但**两者必须一致**：任务和节点都在同一个
	// 命名空间里（I11），这里用任务的命名空间更直接——传错会出路，但不会错作用域。
	PlanNextHop(ctx context.Context, namespaceID, traceID, parentTaskID, fromCapabilityID,
		out string, params json.RawMessage, path []string) ([]*store.Task, error)

	// PublishTasks 对**已落库**的 pending 任务执行「挑机器 + 占槽位 + 推 assign 帧」。
	// 绝不建行；逐条独立，返回第一个错误仅供调用方记日志。
	//
	// namespaceID 同样是任务的命名空间（挑机器只在该命名空间的节点里挑）。
	PublishTasks(ctx context.Context, namespaceID string, tasks []*store.Task) error
}

// hub 返回 Hub，必要时惰性创建。
func (h *NodeHandler) hub() *NodeHub {
	if h.Hub == nil {
		h.Hub = NewNodeHub()
	}
	return h.Hub
}

// NewNodeHandler 构造默认配置的节点协议 handler。
func NewNodeHandler(st NodeStore) *NodeHandler {
	return &NodeHandler{
		Store: st,
		AcceptOptions: &websocket.AcceptOptions{
			// 自托管场景：节点可能从不同源连接。鉴权由 opaque token 保证
			// （token 本身就是凭据，不依赖 Origin）。
			InsecureSkipVerify: true,
		},
		Now: store.Now,
	}
}

func (h *NodeHandler) heartbeatSec() int {
	if h.HeartbeatSec > 0 {
		return h.HeartbeatSec
	}
	return DefaultHeartbeatSec
}

func (h *NodeHandler) leaseTTLSec() int {
	if h.LeaseTTLSec > 0 {
		return h.LeaseTTLSec
	}
	return DefaultLeaseTTLSec
}

func (h *NodeHandler) now() int64 {
	if h.Now != nil {
		return h.Now()
	}
	return store.Now()
}

// deadAfter 是判死窗口：> 2×心跳。
func (h *NodeHandler) deadAfter() time.Duration {
	return time.Duration(h.heartbeatSec()*DeadAfterHeartbeats) * time.Second
}

// ServeHTTP 握手鉴权 → Upgrade → 驱动连接。
//
// 鉴权失败的响应**必须**在 Upgrade 之前写出（401/403），
// 否则客户端会先看到 101 再看到关闭，误以为握手成功。
func (h *NodeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	node, err := AuthenticateNode(r.Context(), h.Store, r)
	if err != nil {
		status := HTTPStatusForAuthError(err)
		slog.Warn("node ws auth failed",
			"event", "node.ws.auth.failed",
			"ip", r.RemoteAddr,
			"status", status,
			"error", err.Error(),
		)
		// 用普通 HTTP 响应返回：此时还没有 101
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"` + jsonSafe(err.Error()) + `"}`))
		return
	}

	opts := h.AcceptOptions
	if opts == nil {
		opts = &websocket.AcceptOptions{InsecureSkipVerify: true}
	}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		// Accept 内部已写错误响应
		return
	}
	defer conn.CloseNow() //nolint:errcheck // 关闭错误无处理价值

	ns := &nodeSession{h: h, conn: conn, node: node}
	ns.run(r.Context())
}

// jsonSafe 把字符串转义成可安全塞进 JSON 的形式（错误信息可能含引号）。
func jsonSafe(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if c < 0x20 {
				out = append(out, []byte(fmt.Sprintf(`\u%04x`, c))...)
				continue
			}
			out = append(out, c)
		}
	}
	return string(out)
}

// nodeSession 是一条活跃的节点连接。
//
// 并发模型：一个读 goroutine + 一个超时看门狗 goroutine，写操作由 mu 串行化
// （coder/websocket 不允许并发写）。
type nodeSession struct {
	h    *NodeHandler
	conn *websocket.Conn
	node *store.Node // 握手时确定；之后所有帧都用它的 ID/NamespaceID

	writeMu sync.Mutex

	// lastUpstream 是最近一次收到上行帧的时间（任何上行帧都续命）。
	// 用 mutex 保护：读 goroutine 写，看门狗读。
	mu           sync.Mutex
	lastUpstream time.Time
	helloDone    bool
}

// touch 记录一次上行活动（任何上行帧都续命，契约 §3.3）。
func (s *nodeSession) touch() {
	s.mu.Lock()
	s.lastUpstream = time.Now()
	s.mu.Unlock()
}

// idleFor 返回距上次上行活动的时间。
func (s *nodeSession) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastUpstream.IsZero() {
		return 0
	}
	return time.Since(s.lastUpstream)
}

// markHello 记录 hello 已完成。
func (s *nodeSession) markHello() {
	s.mu.Lock()
	s.helloDone = true
	s.mu.Unlock()
}

// hasHello 返回是否已收到 hello。
func (s *nodeSession) hasHello() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.helloDone
}

// send 串行化地写一帧。
func (s *nodeSession) send(ctx context.Context, f any) error {
	data, err := Encode(f)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, defaultWriteTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, data)
}

// sendError 下发一个 error 帧（不关连接）。
func (s *nodeSession) sendError(ctx context.Context, code, message string) {
	_ = s.send(ctx, &ErrorFrame{Type: NodeFrameError, Code: code, Message: message})
}

// run 驱动连接：读循环 + 看门狗，直到断开或判死。
func (s *nodeSession) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.touch() // 连接建立即视为活跃（避免看门狗立刻判死）

	// 看门狗：超过 2×心跳没有上行帧 → 判死，关连接。
	//
	// 关键：**只看上行**。下行 assign/cancel 不续命——否则一条持续推送的
	// 死连接（对端不读）会被误判为健康，掩盖故障（契约 §3.3 末条）。
	go s.watchdog(ctx, cancel)

	// 注意：**不在这里发 welcome**。
	// welcome 是「握手成功」的信号，必须等 hello 校验通过（能力/实例/身份都对）
	// 之后才发；否则节点收到 welcome 会以为已经接入成功，
	// 而实际上它的能力声明稍后可能被拒。
	err := s.readLoop(ctx)
	if err != nil && !isNormalClose(err) {
		slog.Info("node connection closed",
			"event", "node.ws.closed",
			"node_id", s.node.ID,
			"namespace_id", s.node.NamespaceID,
			"error", err.Error(),
		)
	}
	s.cleanup(context.WithoutCancel(ctx))
}

// readLoop 读上行帧并分发。
func (s *nodeSession) readLoop(ctx context.Context) error {
	for {
		typ, raw, err := s.conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			// 协议只走文本帧（JSON）
			s.sendError(ctx, ErrCodeBadFrame, "binary frames are not supported")
			continue
		}
		s.touch()

		frame, err := DecodeUpstream(raw)
		if err != nil {
			s.sendError(ctx, ErrCodeBadFrame, err.Error())
			continue
		}
		if err := s.handle(ctx, frame); err != nil {
			return err
		}
	}
}

// handle 分发一帧上行消息。
func (s *nodeSession) handle(ctx context.Context, frame any) error {
	switch f := frame.(type) {
	case *UnknownFrame:
		// 未知类型：回 error 帧但**保持连接**（不是致命错误）
		code := ErrCodeUnknownType
		if IsDownstream(f.Type) {
			code = ErrCodeUnknownType // 节点不该发下行帧；同样按未知类型处理
		}
		s.sendError(ctx, code, fmt.Sprintf("unknown frame type %q", f.Type))
		return nil

	case *HelloFrame:
		return s.handleHello(ctx, f)

	case *HeartbeatFrame:
		return s.handleHeartbeat(ctx, f)

	default:
		// 其余帧（ack/progress/result）要求先 hello
		if !s.hasHello() {
			s.sendError(ctx, ErrCodeBadFrame, "send hello before other frames")
			return nil
		}
		return s.handleTaskFrame(ctx, frame)
	}
}

// handleHello 处理握手后的第一帧。
func (s *nodeSession) handleHello(ctx context.Context, f *HelloFrame) error {
	if err := applyHello(ctx, s.h.Store, s.node, f); err != nil {
		var he *helloError
		if errors.As(err, &he) {
			s.sendError(ctx, he.code, he.message)
			// hello 失败 = 这个节点不该继续连着（能力不合法 / 实例超顶）
			return fmt.Errorf("hello rejected: %s", he.message)
		}
		s.sendError(ctx, ErrCodeBadFrame, err.Error())
		return fmt.Errorf("hello failed: %w", err)
	}
	s.markHello()
	s.touch()
	// 握手成功 → 成为该节点的当前连接（后连的顶掉旧的）。
	// 必须在 welcome 之前注册：节点收到 welcome 后可能立刻开始等 assign。
	s.h.hub().Register(s.node.ID, s)
	// welcome 是握手成功的信号：节点据此配置心跳节奏（间隔/TTL 以服务端为准）。
	if err := s.send(ctx, welcomeFrameFor(s.node)); err != nil {
		return err
	}
	slog.Info("node connected",
		"event", "node.ws.hello",
		"node_id", s.node.ID,
		"namespace_id", s.node.NamespaceID,
		"capabilities", len(f.Capabilities),
		"instances", len(f.Instances),
	)
	return nil
}

// handleHeartbeat 处理心跳：刷新 last_seen_at 并把该节点所有在跑任务的租约推到 now+TTL。
//
// 「续期」推的是**节点上全部**在跑任务，不是一条：心跳是一条连接的生命信号，
// 该连接上的任务一起续命（契约 §3.3「任何上行帧续命」）。
func (s *nodeSession) handleHeartbeat(ctx context.Context, f *HeartbeatFrame) error {
	now := s.h.now()
	// 作用域参数是**命名空间**（#162）：节点属于且只属于一个命名空间，
	// 传别的 id 会让 `WHERE namespace_id = ?` 恒 0 行——心跳续不上租约，
	// 节点上所有在跑任务会被巡检当过期回收。
	if err := s.h.Store.TouchNode(ctx, s.node.NamespaceID, s.node.ID, now); err != nil {
		return fmt.Errorf("touch node: %w", err)
	}
	if _, err := s.h.Store.ExtendLeasesForNode(ctx, s.node.NamespaceID, s.node.ID, now+int64(s.h.leaseTTLSec())); err != nil {
		return fmt.Errorf("extend leases: %w", err)
	}
	return nil
}

// watchdog 看门狗：超过判死窗口没有上行帧就关连接。
//
// 检查周期取心跳间隔的一半，保证判死不会晚太多（也不会太早）。
func (s *nodeSession) watchdog(ctx context.Context, cancel context.CancelFunc) {
	interval := time.Duration(s.h.heartbeatSec()) * time.Second / 2
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.idleFor() > s.h.deadAfter() {
				slog.Warn("node heartbeat timeout, closing",
					"event", "node.ws.timeout",
					"node_id", s.node.ID,
					"idle", s.idleFor().String(),
				)
				// 主动关闭：策略违规（协议要求按心跳周期上报）
				s.writeMu.Lock()
				_ = s.conn.Close(websocket.StatusPolicyViolation, "heartbeat timeout")
				s.writeMu.Unlock()
				cancel()
				return
			}
		}
	}
}

// cleanup 连接结束时的收尾：标记实例掉线、节点离线、清空钉住。
//
// 注意：**不回收任务**。任务归属由租约巡检（SweepExpiredLeases）统一处理，
// 那是唯一入口；这里只把「连接不在了」这个事实记下来。
func (s *nodeSession) cleanup(ctx context.Context) {
	// 先从 hub 摘掉：否则派发器会把 assign 推给一条已断的连接。
	s.h.hub().Unregister(s.node.ID, s)
	if err := s.h.Store.MarkInstancesDead(ctx, s.node.ID); err != nil {
		slog.Warn("mark instances dead failed",
			"event", "node.ws.cleanup.instances",
			"node_id", s.node.ID,
			"error", err.Error(),
		)
	}
	if err := s.h.Store.UpdateNodeStatus(ctx, s.node.NamespaceID, s.node.ID, store.NodeStatusOffline); err != nil {
		slog.Warn("mark node offline failed",
			"event", "node.ws.cleanup.status",
			"node_id", s.node.ID,
			"error", err.Error(),
		)
	}
}

// isNormalClose 判定读错误是否属于「正常断开」。
func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	status := websocket.CloseStatus(err)
	switch status {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusPolicyViolation:
		return true
	}
	return false
}

// kill 主动断开这条连接（被新连接顶掉时调用）。
//
// 用 NormalClosure 而不是 PolicyViolation：这是正常的「换连接」，
// 不是协议违规。节点看到 1000 会直接重连，看到 1008 可能会退避更久。
func (s *nodeSession) kill(reason string) {
	s.writeMu.Lock()
	_ = s.conn.Close(websocket.StatusNormalClosure, reason)
	s.writeMu.Unlock()
}
