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

	"github.com/zzjcool/anotify/internal/route"
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

// 帧体积上限（#221）。两个上限解决两个不同的问题，都需要。
const (
	// NodeReadLimitHard 是服务端单帧**硬**读上限（4 MiB）：交给 coder/websocket，
	// 超过它由库直接 1009 关连接。
	//
	// 为什么必须显式设：库的默认值是 32768B。不设时一个带 40KB manifest 的 hello
	// 会在服务端读取阶段就被 1009 关掉，节点**永远**上不了线（且节点侧只看到断连，
	// 没有任何可诊断信息）。agent 侧对下行帧也设了 1MiB 读上限，方向对称。
	NodeReadLimitHard = 4 << 20
	// NodeFrameSoftLimit 是服务端**逐帧**软上限（1 MiB，与 agent 侧 nodeReadLimit 对齐）。
	// 超过它：先回 error 帧 `frame_too_large`（可诊断），再用 1009 带文案关连接，
	// 而不是让库在硬上限处静默 1009。
	NodeFrameSoftLimit = 1 << 20
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

	// OnCapacityFreed 在「任务终态落库、槽位释放」之后被调用一次（#219）。
	//
	// 为什么需要这个钩子：pending 任务原本只能靠 5s 巡检排空，槽位空出来的那一刻
	// 没有任何东西去拉下一条。接上 sweeper.Nudge 之后，一次完成就会触发一次即时重派。
	//
	// nil 是允许的（测试/嵌入式用法）：没有钩子时行为退回「只有 5s 巡检」。
	OnCapacityFreed func()

	// MaxFrameBytes 覆盖逐帧软上限（0 → NodeFrameSoftLimit）。测试里会调到很小值。
	MaxFrameBytes int
	// ReadLimit 覆盖硬读上限（0 → NodeReadLimitHard）。
	ReadLimit int

	// Now 返回当前 unixepoch 秒（测试可注入）。
	Now func() int64

	// Hub 维护「节点 → 活跃连接」，派发器通过它推送 assign 帧。
	// 为 nil 时自动创建一个（handler 自己持有，不共享）。
	Hub *NodeHub

	// Emit 处理 emit 帧（P1.1 / P1.3，docs/design/emit-protocol.md §3.1 / §7）。
	//
	// **它是下一跳的唯一入口**：`result` 帧不再路由（P1.5 删掉了 `result.out`，
	// 迁移 28 又删掉了 `tasks.out` 整列），「建子任务行 + 写 emission + 推 assign」
	// 全部在 route.Emit 里一次事务完成。
	//
	// 为 nil 时收到 emit 帧一律回 emit_ack{error}（**不断连接**）：
	// 「未装配」不该表现为断连，否则节点会进入「重连 → 重发 → 又断」的风暴。
	Emit EmitDispatcher

	// Expects 推导 assign.expects（P1.4，设计 §3.4）。
	//
	// 为 nil 时帧里不下发这个字段（老装配/测试），行为与加它之前完全一致。
	Expects ExpectsBuilder
}

// EmitDispatcher 处理一次 emit（由 route.Dispatcher 实现）。
//
// 本包只负责「把帧转成一次路由调用，再把结果写成 emit_ack」：状态机、seq 连续性、
// 幂等、建子任务行全在 route/store。抽成接口是为了让本包可以用假实现在不起真库的
// 情况下验证帧层（拒收不断连、错误走 error 字段）。
//
// 返回值 error 非空 = 这次 emit 被拒收。**一律**翻译成 emit_ack.error，
// 不能用 error 帧（那是协议错误，节点会重连），也不能断连。
type EmitDispatcher interface {
	Emit(ctx context.Context, in route.EmitInput) (*route.EmitOutput, error)
}

// ExpectsBuilder 推导「这个端口的 data 要被怎样消费」（由 route.Dispatcher 实现）。
//
// 参数是能力**定义 id**（edges.from_key 现在的口径），ports 是该能力的 outputs。
type ExpectsBuilder interface {
	BuildExpects(ctx context.Context, namespaceID, capabilityID string, ports []string) (map[string]route.PortExpect, error)
}

// hub 返回 Hub，必要时惰性创建（供直接写 `&NodeHandler{Store: …}` 的调用方用）。
func (h *NodeHandler) hub() *NodeHub {
	if h.Hub == nil {
		h.Hub = NewNodeHub()
	}
	return h.Hub
}

// NewNodeHandler 构造默认配置的节点协议 handler。
//
// Hub 在这里就建好（而不是留给 hub() 惰性创建）：hub() 会被每条连接的读循环、看门狗和
// cleanup goroutine 并发调用，惰性赋值 `h.Hub = NewNodeHub()` 是一个真实的 data race
// （`go test -race` 会报）。生产装配（internal/server/mux.go）本来就会用共享的 hub
// 覆盖这个字段，所以提前建一个不带来任何行为差异。
func NewNodeHandler(st NodeStore) *NodeHandler {
	return &NodeHandler{
		Store: st,
		Hub:   NewNodeHub(),
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

// maxFrameBytes 返回生效的逐帧软上限。
func (h *NodeHandler) maxFrameBytes() int {
	if h.MaxFrameBytes > 0 {
		return h.MaxFrameBytes
	}
	return NodeFrameSoftLimit
}

// readLimit 返回生效的单帧硬读上限（0 → NodeReadLimitHard），交给 coder/websocket.SetReadLimit。
func (h *NodeHandler) readLimit() int {
	if h.ReadLimit > 0 {
		return h.ReadLimit
	}
	return NodeReadLimitHard
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

	// #221 握手闸门：Accept 之后**立刻**设读上限，赶在任何一帧之前。
	//
	// 不设的后果（issue 实测）：coder/websocket 默认 32768B，一条 40KB 的 hello
	// （manifest 里带长 command）在库的读取层就炸成 1009，节点永远上不了线；
	// 而服务端日志只有一句「连接关了」，没有任何指向体积的线索。
	conn.SetReadLimit(int64(h.readLimit()))

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

	// leaseMu 保护「本连接名下的任务」与「连接为什么结束」（#218 / #222）。
	// 与上面那把 mu 分开：那把是「活着吗」的热路径，这把是策略状态的写少读少路径。
	// 具体语义见 node_session_lease.go。
	leaseMu   sync.Mutex
	owned     map[string]int // taskID → 库里的 attempts（派发世代）
	endMarked bool
	endClean  bool
	endReason string
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
	return s.sendBytes(ctx, data)
}

// sendBytes 写一帧已经编码好的载荷。
//
// 单独出来是为了让 PublishAssign 能「先量体积再决定发不发」（#221）：
// 超限的 assign 帧绝不能进连接，而量体积必须在编码之后、写之前。
func (s *nodeSession) sendBytes(ctx context.Context, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, defaultWriteTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, data)
}

// closeWith 带写锁地关连接（Close 会写 close 帧，必须与 send 串行化）。
func (s *nodeSession) closeWith(code websocket.StatusCode, reason string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.Close(code, reason)
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
	// 读循环结束后补齐「为什么结束」：看门狗/被顶掉/帧超限三条路径已经**先标**过，
	// 这里只补空（first wins）——不得把看门狗标的 unclean 改回 clean。
	s.endFromReadError(err)
	// **clean 与 unclean 都打这条日志**：perf/e2e 脚本按 `"node.ws.closed"` 计数断连
	// （scripts/perf/b/s4-payload.sh、s5-lease-dup.sh、s6-reconnect.sh）。
	// 只在 unclean 时打会让「对端正常收尾」（1000）变成一条日志都没有；
	// 相反，kill -9 / RST（无 close 帧）现在是 unclean（见 classifyReadError 的裁决依据），
	// 统计「意外掉线」时要按 clean=false 过滤。
	// 判据仍只有一处：classifyReadError（node_session_lease.go）。
	_, clean, reason := s.endState()
	slog.Info("node connection closed",
		"event", "node.ws.closed",
		"node_id", s.node.ID,
		"namespace_id", s.node.NamespaceID,
		"clean", clean,
		"reason", reason,
		"error", errString(err),
	)
	s.cleanup(context.WithoutCancel(ctx))
}

// readLoop 读上行帧并分发。
func (s *nodeSession) readLoop(ctx context.Context) error {
	for {
		typ, raw, err := s.conn.Read(ctx)
		if err != nil {
			return err
		}
		// #221 入帧闸门：逐帧软上限。比硬上限低，为的是能**先回一帧可诊断的 error**
		// 再用 1009 带文案关连接（硬上限处是库直接静默 1009，节点侧看不到原因）。
		// 这条路径按 unclean 处理：对端活着，只是它的帧炸了。
		if limit := s.h.maxFrameBytes(); limit > 0 && len(raw) > limit {
			msg := fmt.Sprintf("frame too large: %d bytes (limit %d bytes)", len(raw), limit)
			s.sendError(ctx, ErrCodeFrameTooLarge, msg)
			s.markEnd(false, msg)
			s.closeWith(websocket.StatusMessageTooBig, msg)
			return fmt.Errorf("frame too large: %d > %d bytes", len(raw), limit)
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
		if err := s.handle(ctx, frame, raw); err != nil {
			return err
		}
	}
}

// handle 分发一帧上行消息。
//
// raw 是解码前的原始载荷：hello 里有一个字段（agent_version）由 A 组加在
// HelloFrame 上、B 组不得改那个 struct（契约 §7），所以 B 组从原始 JSON 里取它
// （见 parseHelloVersion）。其余帧忽略 raw。
func (s *nodeSession) handle(ctx context.Context, frame any, raw []byte) error {
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
		return s.handleHello(ctx, f, parseHelloVersion(raw))

	case *HeartbeatFrame:
		return s.handleHeartbeat(ctx, f)

	default:
		// 其余帧（ack/progress/result/apply/upgrade_ack）要求先 hello
		if !s.hasHello() {
			s.sendError(ctx, ErrCodeBadFrame, "send hello before other frames")
			return nil
		}
		// apply / upgrade_ack 是配置帧，其余是任务帧：分开走，别让「任务帧」的语义含糊。
		switch frame.(type) {
		case *ApplyFrame, *UpgradeAckFrame:
			return s.handleConfigFrame(ctx, frame)
		}
		return s.handleTaskFrame(ctx, frame)
	}
}

// handleHello 处理握手后的第一帧。
func (s *nodeSession) handleHello(ctx context.Context, f *HelloFrame, ver helloVersion) error {
	applied, stale, err := applyHello(ctx, s.h.Store, s.node, f)
	if err != nil {
		return s.failHello(ctx, err)
	}
	// 记录节点自报的 agent 版本（CONTRACT-agent-upgrade §3）。
	//
	// 写在 welcome 之前：控制台在节点上线后立刻读 /v1/nodes，版本列不该比
	// 「节点已上线」晚一拍。失败只 WARN —— 版本是信息性字段，写不进去也必须让节点上线
	// （否则一个存储抖动能把整台机器挡在门外）。
	if err := s.h.Store.UpdateNodeAgentVersion(ctx, s.node.NamespaceID, s.node.ID, ver.AgentVersion); err != nil {
		slog.Warn("record agent version failed",
			"event", "node.ws.hello.agent_version_failed",
			"node_id", s.node.ID,
			"agent_version", ver.AgentVersion,
			"error", err.Error(),
		)
	}
	s.markHello()
	s.touch()
	// 握手成功 → 成为该节点的当前连接（后连的顶掉旧的）。
	// 必须在 welcome 之前注册：节点收到 welcome 后可能立刻开始等 assign。
	s.h.hub().Register(s.node.ID, s)
	// 注册之后**再写一次** node 级状态（审查 n1：M2 修好后的残留 TOCTOU）。
	//
	// 为什么必须写在 Register 之后：Register 之后 current() 才返回本连接，旧连接
	// cleanup 的判据（cur == nil || cur == s）就会跳过它的 node 级写；而 Register
	// **之前**写的话，旧连接完全可能在我写完、还没 Register 的窗口里（此时
	// current() 还是 nil）把 alive/online 覆盖成 0/offline，而我之后再也不会重写
	// → 节点「在线却接不到活」且不自愈（#218 的症状）。
	// 能力校验仍留在 applyHello 里（必须在 Register 之前，否则一个能力非法的节点
	// 会先顶掉在线的旧连接）；这里只重写两份幂等的 node 级状态。
	if err := reassertNodeState(ctx, s.h.Store, s.node, f); err != nil {
		return s.failHello(ctx, err)
	}
	// welcome 是握手成功的信号：节点据此配置心跳节奏（间隔/TTL 以服务端为准）。
	if err := s.send(ctx, welcomeFrameFor(s.node, stale)); err != nil {
		return err
	}
	slog.Info("node connected",
		"event", "node.ws.hello",
		"node_id", s.node.ID,
		"namespace_id", s.node.NamespaceID,
		"applied", len(f.Applied),
		"instances", len(f.Instances),
	)
	// 中枢覆盖：节点上缺的 / 旧的产物，握手一结束就补发（见 resendDeployments）。
	s.resendDeployments(ctx, applied)
	// 节点上线 = **新增了执行能力**（#219）。队列里可能正压着一批
	// 「之前没人能跑」的 pending，它们原本也要等下一轮 5s 巡检才被捡起来。
	// 钩子是可合并的非阻塞投递，这里同步调用不会拖慢握手。
	if s.h.OnCapacityFreed != nil {
		s.h.OnCapacityFreed()
	}
	return nil
}

// resendDeployments 把「中枢期望、节点上还没有（或还是旧的那份）」的产物补发下去。
//
// 中枢是部署的唯一写者，所以这里不做“本地变体”协商：不一致就覆盖。它同时覆盖两个场景：
//   - 重连后节点缓存的 digest 不是中枢期望的那份（换了部署目标 / 产物在中枢更新过）；
//   - 部署时节点离线：行建好了、帧没发出去，等它上线补推
//     （原来只有一句“节点上线时会补推”的注释，代码里没有）。
//
// 失败只记日志、不断连接：补推不是握手的前置条件，能接活比“产物一致”更要紧，
// 下一次连上来还会再试。
func (s *nodeSession) resendDeployments(ctx context.Context, applied map[string]string) {
	rows, err := s.h.Store.DeploymentsForNode(ctx, s.node.NamespaceID, s.node.ID)
	if err != nil {
		slog.Warn("resend deployments: list failed",
			"event", "node.ws.deploy.resend_failed",
			"node_id", s.node.ID,
			"error", err.Error(),
		)
		return
	}

	pushed := 0
	for _, r := range rows {
		if r.Digest == applied[r.CapabilityID] {
			continue // 已经是这一份
		}
		// 先落回 pending：控制台上看到的应该是“正在重新下发”，而不是一个假的绿勾。
		// reason 里保留两端 digest：对账日志会被下一次推送盖掉，排障时得看得到差在哪。
		have := applied[r.CapabilityID]
		reason := fmt.Sprintf("resend on connect: node has %q, console expects %q", have, r.Digest)
		if have == "" {
			reason = fmt.Sprintf("resend on connect: node has nothing, console expects %q", r.Digest)
		}
		if err := s.h.Store.MarkDeployResult(ctx, s.node.NamespaceID, r.DeploymentID, store.DeploymentPending, reason); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("resend deployments: mark pending failed",
				"event", "node.ws.deploy.resend_mark_failed",
				"node_id", s.node.ID,
				"deploy_id", r.DeploymentID,
				"error", err.Error(),
			)
		}
		if err := s.send(ctx, &DeployFrame{
			Type:     NodeFrameDeploy,
			DeployID: r.DeploymentID,
			Action:   DeployActionApply,
			Artifact: json.RawMessage(r.Spec),
		}); err != nil {
			slog.Warn("resend deployments: send failed",
				"event", "node.ws.deploy.resend_send_failed",
				"node_id", s.node.ID,
				"deploy_id", r.DeploymentID,
				"error", err.Error(),
			)
			return
		}
		pushed++
	}

	if pushed > 0 {
		slog.Info("deployments resent on connect",
			"event", "node.ws.deploy.resend",
			"node_id", s.node.ID,
			"count", pushed,
		)
	}
}

// failHello 把 hello 失败翻译成协议错误帧，并返回「结束连接」的错误。
//
// hello 失败 = 这个节点不该继续连着（能力不合法 / 实例超顶 / 状态写不进去），
// 返回错误会让读循环结束 → cleanup 跑正常的关连接收尾。
func (s *nodeSession) failHello(ctx context.Context, err error) error {
	var he *helloError
	if errors.As(err, &he) {
		s.sendError(ctx, he.code, he.message)
		return fmt.Errorf("hello rejected: %s", he.message)
	}
	s.sendError(ctx, ErrCodeBadFrame, err.Error())
	return fmt.Errorf("hello failed: %w", err)
}

// handleHeartbeat 处理心跳：刷新 last_seen_at，并**只续本连接名下**那些在飞任务的租约。
//
// #218 的根修在这里（改动前的写法是 ExtendLeasesForNode，按 **node** 续）：
// 一条连接只能替它自己派出去、还没结束的任务续命。按节点批续会把**上一代进程遗留的
// 孤儿任务**（重启后仍 assigned/acked 的行）也一直续着 → 巡检永远认为它们还活着 →
// 任务永久卡 acked、槽位永久被占、pending 一条都派不出去。
//
// 名下没任务就不调续租（不能退化成按节点续）。
func (s *nodeSession) handleHeartbeat(ctx context.Context, f *HeartbeatFrame) error {
	now := s.h.now()
	// 作用域参数是**命名空间**（#162）：节点属于且只属于一个命名空间，
	// 传别的 id 会让 `WHERE namespace_id = ?` 恒 0 行——心跳续不上租约。
	if err := s.h.Store.TouchNode(ctx, s.node.NamespaceID, s.node.ID, now); err != nil {
		return fmt.Errorf("touch node: %w", err)
	}
	owners := s.ownedOwners()
	if len(owners) == 0 {
		return nil
	}
	ids := make([]string, 0, len(owners))
	for _, o := range owners {
		ids = append(ids, o.TaskID)
	}
	if _, err := s.h.Store.ExtendLeasesForTasks(ctx, s.node.NamespaceID, s.node.ID, ids,
		now+int64(s.h.leaseTTLSec())); err != nil {
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
				// 先标后关（first wins）：我们主动掐的连接**不证明对端进程死了**
				// （SIGSTOP / 半开 TCP 都长这模样）→ unclean → 名下任务 abandoned，绝不重派（#222）。
				s.markEnd(false, fmt.Sprintf("heartbeat timeout (idle %s)", s.idleFor()))
				// 主动关闭：策略违规（协议要求按心跳周期上报）
				s.closeWith(websocket.StatusPolicyViolation, "heartbeat timeout")
				cancel()
				return
			}
		}
	}
}

// cleanup 连接结束时的收尾（#218 + #222，**顺序是硬要求**）。
//
// ① 先从 hub 摘掉（Unregister）：否则派发器会把 assign 推给一条已断的连接。
// ② 只有**此刻仍是**当前连接时才动 node 级状态（MarkInstancesDead + offline）。
// 它不依赖 ③（纯 node 级写），所以紧跟 ① 做——中间的窗口越短越好，理由见下。
// ③ 把「执行者不在了」落库（ResolveLostTasks）：clean → 立刻回 pending（不等 30s 租约）；
// unclean → abandoned + executor_lost 诊断，**绝不重派**（#222 at-most-once）。
// ④ 才放开 hub 的在飞登记（releaseAll）：先放开的话，正在巡检的派发器可以抢在
// ③ 的 DB 释放之前把同一任务派出去 → 双执行。
// ② → ③ → ④ 的先后不能换：② 是 node 级、③④ 是任务级，改顺序只会重现窗口或双执行。
func (s *nodeSession) cleanup(ctx context.Context) {
	hub := s.h.hub()
	// ① 先摘连接：派发器不能再往这条死连接上推帧。
	// 返回值**故意不用**（见 ②）：它回答的是「摘的那一刻是不是当前连接」，
	// 而我们真正要回答的是「现在是不是」。
	hub.Unregister(s.node.ID, s)

	// ② node 级状态（alive / online）只在「没有**更新的连接**接替了我」时写。
	//
	// 为什么必须重新问一次 hub，而不是用 ① 的返回值（审查 M2）：
	// 改动前 ② 排在 ③ 之后，而 ③ 是**每条任务一个事务**——于是 ① 与 ② 之间有一个
	// 长度正比于在飞任务数的窗口。新连接完全可能在这段时间完成 hello
	// （UpsertInstances alive=1 + online）并 Register，而旧连接手里仍是 ① 的 true，
	// 于是 ② 把新连接刚写好的 alive/online 改回 dead/offline。而且**不会自愈**：
	// TouchNode 只写 last_seen_at；alive=1 全仓只有 hello 的 UpsertInstances 写；
	// FreeSlotsByNode 要求 alive=1、NodesForCapability 要求 online
	// → 节点「显示在线却永远接不到活」，就是 #218 的症状（测得 18 个在飞任务时 6/6 复现）。
	//
	// 把 ② 提到 ③ 之前把窗口从「正比于在飞任务数」压到几条指令，但**仍然**要复查：
	// 并发下「摘的时候是当前」与「现在还是当前」是两件事。
	//
	// 判据是 `cur == nil || cur == s`，不是字面的 `cur == s`：
	// ① 已经把自己从 conns 里摘掉了，所以此刻 current 只可能是
	//   - nil（没有人接替我）→ 这一次断开是真的，应该标 dead/offline；
	//   - 另一条连接（新连接顶掉了我）→ 绝不能动，否则就是把它的 alive/online 改回去。
	// 写成 `cur == s` 会把 nil 也算成「不是当前连接」→ 节点永远不下线（TestDisconnectMarksNodeOffline 红）。
	if cur := hub.current(s.node.ID); cur == nil || cur == s {
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

	owners := s.ownedOwners() // ③ 之前快照：之后 own/disown 属于别的帧
	if len(owners) > 0 {
		_, clean, reason := s.endState()
		if reason == "" {
			reason = "connection closed"
		}
		policy := s.lostPolicy()
		if requeued, abandoned, err := s.h.Store.ResolveLostTasks(ctx, s.node.NamespaceID, s.node.ID,
			owners, policy, reason, s.h.now()); err != nil {
			slog.Warn("resolve lost tasks failed",
				"event", "node.ws.cleanup.tasks_failed",
				"node_id", s.node.ID,
				"tasks", len(owners),
				"clean", clean,
				"error", err.Error(),
			)
		} else if requeued > 0 || abandoned > 0 {
			slog.Info("released tasks of a closed connection",
				"event", "node.ws.cleanup.tasks",
				"node_id", s.node.ID,
				"clean", clean,
				"requeued", requeued,
				"abandoned", abandoned,
				"reason", reason,
			)
		}
	}
	// ④ 落库释放之后才放开在飞登记（反了就是双执行，见函数头）
	hub.releaseAll(s)
}

// kill 主动断开这条连接（被新连接顶掉时调用）。
//
// 用 NormalClosure 而不是 PolicyViolation：这是正常的「换连接」，
// 不是协议违规。节点看到 1000 会直接重连，看到 1008 可能会退避更久。
//
// 但**对我们自己而言**这是 unclean（先标后关）：旧进程很可能还在跑它手上的命令，
// 把它的任务重派出去就是并发执行两遍（#222）。
func (s *nodeSession) kill(reason string) {
	s.markEnd(false, "superseded by a newer connection: "+reason)
	s.closeWith(websocket.StatusNormalClosure, reason)
}

// errString 把可能为 nil 的错误转成日志字段（nil → ""）。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
