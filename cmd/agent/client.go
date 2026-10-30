package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// 本文件是 agent 的**连接与调度层**：dial → hello → 等 welcome → 心跳 + 读循环
// （assign / cancel / error）→ 优雅退出。
//
// 边界（不变量 I1）：中枢不执行，也不告诉节点跑什么。这里只把 assign 帧里的
// capability/version/params 翻成 job 交给 runner；**命令原文**只有本地 node.yaml
// 一处来源（run.command），参数只走环境变量（见 runner.go）。
//
// 并发模型（契约 §3.4）：
//   - 读：单 goroutine（serveNode 起的读循环），一帧一帧处理；
//   - 写：全部经 send 串行化（coder/websocket 不允许并发写）；
//   - 执行：每任务一个 goroutine，按能力用信号量限流（容量 cfg.taskLimit）；
//   - 任务表（task_id → cancel）由 mutex 保护，供 cancel 帧查找。
//
// # 两个 ctx 的分工（契约 §11.1 / F1，别改回去）
//
// 父 ctx（runNode 的入参，SIGINT/SIGTERM 取消它）只表达「本进程要退出」；连接自己
// 的 ctx 是 a.base = context.WithCancel(context.WithoutCancel(parent))，读写与在跑任务
// 都用它。原因在真库里是致命的：coder/websocket 的 Conn.Read(ctx) 会挂
// context.AfterFunc(ctx, func() { c.close() })（conn.go 的 setupReadTimeout），
// **取消读的 ctx 等于立刻关掉底层连接**。若拿父 ctx 去读，SIGINT 一到连接就没了，
// grace 期间跑完的任务发不出 result（use of closed network connection）→ 结果全丢 →
// 服务端只能等租约到期重派 → 同一任务重复执行。
//
// 可测性：连接被抽象成 nodeConn（read/write/close 三方法），runNode 只是
// dialNode + serveNode。测试两条腿走路：内存 fake 覆盖调度细节（快、确定），
// httptest + coder/websocket 起真连接覆盖「库的真实语义」（F1 的回归锚——
// 这条用 fake 是测不出来的，fake 的读语义是我们自己写的）。

const (
	// handshakeTimeout 是「发出 hello 之后等 welcome」的上限（契约 §3.4.1）。
	handshakeTimeout = 30 * time.Second
	// frameWriteTimeout 是单帧写超时：写是串行的，一帧卡死会拖住全部上行帧。
	frameWriteTimeout = 10 * time.Second
	// defaultHeartbeatSec 是 welcome 没给 heartbeat_sec 时的心跳周期（秒，契约 §3.4.2）。
	defaultHeartbeatSec = 10
	// minHeartbeatInterval 是心跳间隔下限：契约 §3.4.2 的 max(1s, heartbeatSec/2)
	// 保证服务端 idle 判定里「至少两拍」不会因为配置过小而抖动。
	minHeartbeatInterval = time.Second
	// nodeReadLimit 是单帧读取上限：assign 帧只有 params，1 MiB 足够。
	nodeReadLimit = 1 << 20
)

// errNodeConnClosed 是连接已关闭后继续写帧的返回值（只在内部使用）。
var errNodeConnClosed = errors.New("connection closed")

// ---------- 连接表面 ----------

// nodeConn 是连接的最小表面（便于用内存 fake 单测，不需要真 WebSocket）。
type nodeConn interface {
	read(ctx context.Context) ([]byte, error)
	write(ctx context.Context, data []byte) error
	close() error
}

// wsConn 是 nodeConn 的真实现：coder/websocket 的文本帧。
type wsConn struct{ c *websocket.Conn }

func (w *wsConn) read(ctx context.Context) ([]byte, error) {
	// 协议只走文本帧（JSON）：帧类型不在这里判，坏帧由读循环解码时记 warn，
	// 一条坏帧不该掀掉整条连接。
	_, data, err := w.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (w *wsConn) write(ctx context.Context, data []byte) error {
	return w.c.Write(ctx, websocket.MessageText, data)
}

func (w *wsConn) close() error {
	// 正常关闭码：服务端能把它和「掉线」区分开（node_handler.isNormalClose）。
	return w.c.Close(websocket.StatusNormalClosure, "agent shutting down")
}

// dialNode 连上 GET /v1/node/stream，用节点令牌做 Bearer 鉴权。
//
// 不用 query 参数传令牌：令牌进 URL 会落进反代日志。
func dialNode(ctx context.Context, url, token string) (nodeConn, error) {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		// 握手阶段的错误（含 401/403）由 coder/websocket 带回来；补上 URL 才看得出
		// 是「连错了地址」还是「令牌不对」。
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	conn.SetReadLimit(nodeReadLimit)
	return &wsConn{c: conn}, nil
}

// ---------- 入口 ----------

// runNode 是进程级入口：连上中枢并跑到连接结束或进程退出。
//
// 返回 nil 表示「按信号正常退出」；连接断开返回 connection closed（v1 不做退避重连，
// 交给进程管理器重启）。
func runNode(ctx context.Context, cfg *nodeConfig, log *slog.Logger, grace time.Duration) error {
	if cfg == nil {
		return errors.New("runNode: nil node config")
	}
	if log == nil {
		log = slog.Default()
	}

	url, err := cfg.streamURL()
	if err != nil {
		return err
	}
	conn, err := dialNode(ctx, url, cfg.Token)
	if err != nil {
		return err
	}
	// 此时只完成了建连，hello/welcome 还没走：握手结果由 serveNode 记。
	log.Info("websocket connected",
		"event", "agent.dialed",
		"node", cfg.Name,
		"url", url,
		"capabilities", len(cfg.Capabilities),
		"slots", cfg.slots(),
	)
	return serveNode(ctx, cfg, conn, log, grace)
}

// serveNode 是 runNode 的可测内核：给定一条已建立的连接，跑完整个生命周期。
//
// 返回：
//   - nil：ctx 被取消（SIGINT/SIGTERM）→ 优雅退出（先等在跑任务，最多 grace）；
//   - fmt.Errorf("connection closed: %w", err)：连接断开（契约 §3.4.7）；
//   - 其他：握手失败（含 handshake rejected）。
//
// 结构（契约 §11.1）：读循环在 goroutine 里，主 goroutine 只裁决「退出信号 vs
// 连接断开」。读循环用 a.base 而不是父 ctx，所以父 ctx 取消**不会**把连接关掉，
// drain 期间在跑任务仍能把 result 发出去。
func serveNode(ctx context.Context, cfg *nodeConfig, conn nodeConn, log *slog.Logger, grace time.Duration) error {
	if cfg == nil {
		return errors.New("serveNode: nil node config")
	}
	if conn == nil {
		return errors.New("serveNode: nil connection")
	}
	if log == nil {
		log = slog.Default()
	}

	a := newNodeAgent(ctx, cfg, conn, log)
	// 收尾兜底（幂等，两个分支里已做过的事不会重复做）：先关连接——send 看到
	// closed 就不会再碰连接，残留写被 writeMu 挡在外面；再断「连接自己的」ctx，
	// 兜住残留任务。
	defer a.cancel()
	defer a.closeConn()

	welcome, err := a.handshake(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// 握手中途收到 SIGINT/SIGTERM：不是故障，按正常退出处理。
			a.log.Info("shutdown during handshake", "event", "agent.shutdown.handshake")
			return nil
		}
		return err
	}
	a.log.Info("handshake ok",
		"event", "agent.handshake.ok",
		"node_id", welcome.NodeID,
		"protocol", welcome.Protocol,
		"heartbeat_sec", welcome.HeartbeatSec,
		"lease_ttl_sec", welcome.LeaseTTLSec,
	)

	// 心跳挂在**连接** ctx（a.base）上，不是父 ctx：drain 期间在跑任务还要把 result
	// 发出去，连接必须活着，心跳也跟着活着（服务端 idle 判定才不会把这段算成静默）。
	hbCtx, hbCancel := context.WithCancel(a.base)
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		a.heartbeatLoop(hbCtx, heartbeatInterval(welcome.HeartbeatSec))
	}()
	// 心跳必须在关连接**之前**停：否则 Close 之后再写一帧，只是白记一条 warn。
	stopHeartbeat := func() { hbCancel(); hbWG.Wait() }

	// 读循环放 goroutine：主 goroutine 只做「谁先来」的裁决，不再自己阻塞在读上。
	// 缓冲 1：主 goroutine 走了退出分支后，读 goroutine 也能落地不泄漏。
	readErrCh := make(chan error, 1)
	go func() { readErrCh <- a.readLoop(a.base) }()

	select {
	case <-ctx.Done():
		// SIGINT/SIGTERM：先 draining（不再接新任务），再给在跑任务最多 grace 收尾。
		// 这一步有意义的前提就是读循环用的是 a.base——父 ctx 取消不会关连接，
		// 所以这里跑完的任务的 result 还发得出去（F1）。
		a.log.Info("shutdown signal: draining",
			"event", "agent.shutdown.drain", "grace", grace)
		a.drain(grace)
		stopHeartbeat()
		a.closeConn()
		// 收尾期的读错误是「我们自己把连接关了」，不是故障：忽略（记 debug 备查）。
		if err := <-readErrCh; err != nil {
			a.log.Debug("read loop ended during shutdown",
				"event", "agent.read.shutdown", "error", err.Error())
		}
		return nil
	case readErr := <-readErrCh:
		// 连接断了：result 反正发不出去。直接收掉在跑任务（别留孤儿进程组），
		// v1 不重连（契约 §3.4.7 + §10 缺口 3）。
		a.cancelAll()
		stopHeartbeat()
		if readErr == nil {
			// readLoop 返回 nil 只可能因为它自己的 ctx 被取消；走到这条分支说明
			// 连接在退出流程之外断了，仍按断开处理（宁可让进程管理器重启，
			// 也不静默退出——静默退出等于节点凭空消失）。
			// 文案不能再套 "connection closed: " 前缀——errNodeConnClosed 自己就是
			// "connection closed"，套上去会打出「connection closed: connection closed」。
			readErr = errors.New("connection closed without error")
		}
		a.log.Warn("connection closed",
			"event", "agent.connection.closed",
			"error", readErr.Error(),
		)
		return readErr
	}
}

// heartbeatInterval 是契约 §3.4.2 的 max(1s, heartbeatSec/2)；缺省/非正数按 10s 算。
func heartbeatInterval(sec int) time.Duration {
	if sec <= 0 {
		sec = defaultHeartbeatSec
	}
	interval := time.Duration(sec) * time.Second / 2
	if interval < minHeartbeatInterval {
		interval = minHeartbeatInterval
	}
	return interval
}

// ---------- 调度状态 ----------

// nodeAgent 是一条节点连接的调度状态。
type nodeAgent struct {
	cfg  *nodeConfig
	conn nodeConn
	log  *slog.Logger

	// parent 是进程级 ctx（SIGINT/SIGTERM 取消它）；base 是**连接自己的** ctx，
	// 由 WithoutCancel 派生：父 ctx 取消只代表「要退出」，在跑任务还得收尾——
	// grace 的全部意义就在这里（否则优雅退出就退化成「立刻杀进程」）。
	// 收尾结束或连接断了才 cancel() base。
	parent context.Context
	base   context.Context
	cancel context.CancelFunc

	// writeMu 串行化所有写（coder/websocket 不允许并发写；fake 也不保证并发安全）。
	writeMu sync.Mutex
	closed  bool

	// 每能力（cap@ver）一个信号量：容量 = cfg.taskLimit(spec)。
	semMu sync.Mutex
	sems  map[string]chan struct{}

	// 在跑任务表（task_id → 取消句柄），供 cancel 帧查找。
	taskMu sync.Mutex
	tasks  map[string]*taskCancel

	// wg 追踪所有任务 goroutine：优雅退出要等它们（最多 grace）。
	//
	// wgMu 只在两件事上持有：reserveTask 做 Add、drain 置 draining。两件事
	// 互斥，就排除了 Add 与 Wait 并发（sync.WaitGroup 的硬要求）。
	wgMu sync.Mutex
	wg   sync.WaitGroup

	// draining 置位后不再接新任务（不 ack，让中枢的租约到期回收）。
	draining atomic.Bool
}

func newNodeAgent(parent context.Context, cfg *nodeConfig, conn nodeConn, log *slog.Logger) *nodeAgent {
	base, cancel := context.WithCancel(context.WithoutCancel(parent))
	return &nodeAgent{
		cfg:    cfg,
		conn:   conn,
		log:    log,
		parent: parent,
		base:   base,
		cancel: cancel,
		sems:   make(map[string]chan struct{}),
		tasks:  make(map[string]*taskCancel),
	}
}

// taskCancel 是一个在跑任务的取消句柄。
//
// 包一层指针是为了「同一 task_id 被重派」时能认出「表里的那一份还是不是我自己」：
// 否则旧 goroutine 退出时会把新任务的登记抹掉，cancel 帧就找不到人了。
type taskCancel struct {
	cancel context.CancelFunc
}

// isDraining 判定是否已经进入退出流程。
//
// 不能只看 draining 标志：ctx 取消与「读循环发现取消」之间还有一个窗口，
// 这个窗口里收到的 assign 同样不该 ack（ack 了就等于承诺执行）。
func (a *nodeAgent) isDraining() bool {
	return a.draining.Load() || a.parent.Err() != nil
}

// send 串行化地写一帧。
//
// 用的 ctx 是 base（连接自己的），不是父 ctx：优雅退出时在跑任务的 result 必须
// 还发得出去。frameWriteTimeout 兜住「对端不读」的僵死，但代价要说清楚：真库的
// setupWriteTimeout 在写 ctx 取消时同样走 c.close()，**写超时 = 关掉整条连接**
// （不是跳过这一帧继续写），随后由读循环读到错误收尸——连接本来就僵了，关掉是对的。
func (a *nodeAgent) send(f any) error {
	data, err := encodeFrame(f)
	if err != nil {
		return err
	}

	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.closed {
		return errNodeConnClosed
	}
	ctx, cancel := context.WithTimeout(a.base, frameWriteTimeout)
	defer cancel()
	return a.conn.write(ctx, data)
}

// closeConn 关连接（幂等）。
//
// 持 writeMu 关：保证没有并发写正在跑（fake 与真连接都不必处理这种竞争）。
func (a *nodeAgent) closeConn() {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	if err := a.conn.close(); err != nil {
		a.log.Warn("close connection", "event", "agent.close.failed", "error", err.Error())
	}
}

// ---------- 握手 ----------

// handshake 发 hello 并等 welcome。
//
// 期间收到 error 帧 = 中枢拒绝（令牌/能力/manifest 不合规），必须作为**失败**返回：
// 契约 §3.4.1 的错误文案是运维唯一能看懂的线索，逐字固定。
//
// 读的 ctx 用**父 ctx**（契约 §11.1 第 3 条）：握手阶段还没有要保全的 result，
// 父 ctx 取消时直接放弃握手是对的（调用方把 ctx.Err() 当「正常退出」处理）。
// 一旦握手成功，读就必须换成 a.base（见 readLoop）。
func (a *nodeAgent) handshake(ctx context.Context) (*welcomeFrame, error) {
	if err := a.send(a.cfg.hello()); err != nil {
		return nil, fmt.Errorf("send hello: %w", err)
	}

	wctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	for {
		data, err := a.conn.read(wctx)
		if err != nil {
			if ctx.Err() != nil {
				// 父 ctx 取消：交回调用方当「正常退出」处理。
				return nil, ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("wait welcome: timeout after %s", handshakeTimeout)
			}
			return nil, fmt.Errorf("wait welcome: %w", err)
		}

		frame, derr := decodeDownstream(data)
		if derr != nil {
			return nil, fmt.Errorf("wait welcome: %w", derr)
		}
		switch f := frame.(type) {
		case *welcomeFrame:
			return f, nil
		case *errorFrame:
			return nil, fmt.Errorf("handshake rejected: %s: %s", f.Code, f.Message)
		case *unknownFrame:
			a.log.Warn("unknown frame before welcome",
				"event", "agent.hello.unknown_frame", "type", f.Type)
		default:
			// welcome 之前的 assign/cancel 都不该出现：记下来，继续等 welcome。
			a.log.Warn("unexpected frame before welcome",
				"event", "agent.hello.unexpected_frame", "frame", fmt.Sprintf("%T", f))
		}
	}
}

// ---------- 读循环 ----------

// readLoop 读下行帧直到连接断开。
//
// ctx 必须是**连接自己的** ctx（a.base），不能是父进程 ctx：真库里取消读的 ctx
// 会触发 c.close()，等于把连接连根拔掉（F1）。父 ctx 的取消由 serveNode 裁决。
//
// 返回 nil = 连接 ctx 被取消（收尾期自己关连接，不是故障）；
// 其他 err = 连接断了（契约 §3.4.7 的错误文案）。
func (a *nodeAgent) readLoop(ctx context.Context) error {
	for {
		data, err := a.conn.read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// 连接 ctx 被取消：只可能是收尾（drain 超时后的 cancelAll / 进程收尾）。
				// 这时读到的错是「我们自己关的」，不算故障（契约 §11.1 第 4 条）。
				return nil
			}
			return fmt.Errorf("connection closed: %w", err)
		}

		frame, derr := decodeDownstream(data)
		if derr != nil {
			// 一条坏帧不断连接：v1 没有「回 error 帧」的通道（那是中枢→节点的方向）。
			a.log.Warn("bad downstream frame",
				"event", "agent.frame.bad", "error", derr.Error())
			continue
		}

		switch f := frame.(type) {
		case *assignFrame:
			a.handleAssign(f)
		case *cancelFrame:
			a.handleCancel(f)
		case *errorFrame:
			// 契约 §3.4.5：error 帧只记 warn，不断连接（握手阶段除外）。
			a.log.Warn("control plane error",
				"event", "agent.error_frame",
				"code", f.Code, "message", f.Message, "task_id", f.TaskID)
		case *welcomeFrame:
			a.log.Warn("duplicate welcome", "event", "agent.welcome.duplicate", "node_id", f.NodeID)
		case *unknownFrame:
			a.log.Debug("unknown frame type", "event", "agent.frame.unknown", "type", f.Type)
		default:
			a.log.Debug("unhandled frame", "event", "agent.frame.unhandled",
				"frame", fmt.Sprintf("%T", f))
		}
	}
}

// handleAssign 处理派单：**立刻 ack**，然后在信号量里跑。
//
// ack 必须在读循环里同步发（不能等排到信号量）：ack 是租约开始计时的信号，
// 拖住它会让中枢以为节点没收到。执行本身放 goroutine，读循环不被长任务卡住。
//
// 名额（wg 计数）在 ack **之前**就占好，理由两条：
//  1. ack = 承诺执行；占不到名额就不该 ack（不 ack → 租约到期回收，契约 §3.4.6）。
//  2. 读循环现在是独立 goroutine，drain 的 wg.Wait 与新任务的 wg.Add 会真的并发；
//     reserveTask 用一把锁把 Add 挡在 draining 置位之前（见 reserveTask）。
func (a *nodeAgent) handleAssign(f *assignFrame) {
	if a.isDraining() {
		// 契约 §3.4.6：draining 期间**不 ack**——ack 等于承诺执行，中枢会等一个
		// 永远不会来的 result；不 ack 则租约到期后自然回收重派。
		a.ignoreAssign(f)
		return
	}
	if !a.reserveTask() {
		// isDraining 与 reserveTask 之间退出流程刚好开始：与「draining 期间收到」同待遇。
		a.ignoreAssign(f)
		return
	}

	if err := a.send(&ackFrame{Type: frameTypeAck, TaskID: f.TaskID}); err != nil {
		// ack 都发不出去，连接已经在坏了；result 也发不出去，只能记日志。
		// 已占的名额没有 goroutine 会跑，还回去，否则 drain 会白等到 grace 到点。
		a.releaseTask()
		a.log.Warn("ack failed",
			"event", "agent.ack.failed", "task_id", f.TaskID, "error", err.Error())
		return
	}

	spec, ok := a.cfg.lookup(f.Capability, f.Version)
	if !ok {
		// 已 ack 却跑不了：必须回 failed，否则中枢要等租约到期（契约 §3.4.3）。
		a.startUnrunnable(f)
		return
	}
	a.startTask(f, spec)
}

// ignoreAssign 记下「这单没接」：没 ack → 中枢靠租约到期回收重派。
func (a *nodeAgent) ignoreAssign(f *assignFrame) {
	a.log.Warn("draining: assign ignored",
		"event", "agent.assign.draining",
		"task_id", f.TaskID, "capability", f.Capability, "version", f.Version)
}

// reserveTask 占一个任务 goroutine 名额（wg 计数 +1）。
//
// 为何要一把锁：读循环是独立 goroutine 之后，drain 的 wg.Wait 与新任务的
// wg.Add 会真的并发，而 sync.WaitGroup 的 Add 与 Wait 并发是 data race
// （计数已归零时还会 panic）。drain 在同一把锁里置位 draining 再 Wait，
// 于是「能 Add」一定发生在 Wait 之前。
func (a *nodeAgent) reserveTask() bool {
	a.wgMu.Lock()
	defer a.wgMu.Unlock()
	if a.draining.Load() {
		return false
	}
	a.wg.Add(1)
	return true
}

// releaseTask 归还 reserveTask 占下的名额（任务没起来时用）。
func (a *nodeAgent) releaseTask() { a.wg.Done() }

// startUnrunnable 收回「没有 run 映射」的派单：result failed。
func (a *nodeAgent) startUnrunnable(f *assignFrame) {
	errText := fmt.Sprintf("no run spec for %s@%d", f.Capability, f.Version)
	a.log.Warn("assign without run spec",
		"event", "agent.assign.no_run_spec",
		"task_id", f.TaskID, "capability", f.Capability, "version", f.Version)

	// 名额已在 handleAssign 里占好（reserveTask）：这里只负责还。
	go func() {
		defer a.wg.Done()
		a.sendResult(f.TaskID, outcome{
			Status:    statusFailed,
			Artifacts: []string{},
			Err:       errText,
		})
	}()
}

// startTask 起一个任务 goroutine：信号量 → runJob → result。
func (a *nodeAgent) startTask(f *assignFrame, spec *runSpec) {
	jctx, cancel := context.WithCancel(a.base)
	handle := &taskCancel{cancel: cancel}

	// 先登记再起 goroutine：cancel 帧可能在 goroutine 还没跑到 runJob 时到达。
	a.taskMu.Lock()
	old := a.tasks[f.TaskID]
	a.tasks[f.TaskID] = handle
	a.taskMu.Unlock()
	if old != nil {
		// 同一 task_id 重复派发（中枢租约到期后重派）：先让旧的那份死掉，
		// 避免两个进程写同一份输出、两条 result 打架。
		old.cancel()
	}

	sem := a.semaphore(f.Capability, f.Version, a.cfg.taskLimit(spec))
	j := job{
		TaskID:      f.TaskID,
		Capability:  f.Capability,
		Version:     f.Version,
		Params:      f.Params,
		Spec:        spec,
		MaxDuration: f.MaxDuration,
	}

	// 名额已在 handleAssign 里占好（reserveTask）：这里只负责还。
	go func() {
		defer a.wg.Done()
		defer a.untrackTask(f.TaskID, handle)
		defer cancel()

		// 每个能力一个信号量：容量 = cfg.taskLimit(spec)（slots × concurrency）。
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-jctx.Done():
			// 还没排到就被取消（cancel 帧 / 退出超时）：也要回一条 result。
			a.log.Info("task canceled before start",
				"event", "agent.task.canceled_pending", "task_id", f.TaskID)
			a.sendResult(f.TaskID, outcome{
				Status:    statusFailed,
				Artifacts: []string{},
				Err:       "canceled before start",
			})
			return
		}

		startAt := time.Now()
		out := runJob(jctx, j)
		a.log.Info("task finished",
			"event", "agent.task.finished",
			"task_id", f.TaskID,
			"capability", f.Capability,
			"version", f.Version,
			"status", out.Status,
			"elapsed", time.Since(startAt),
			"summary", out.Summary,
			"error", out.Err,
		)
		a.log.Debug("task output",
			"event", "agent.task.output", "task_id", f.TaskID, "output", out.Output)
		a.sendResult(f.TaskID, canceledOutcome(jctx, out))
	}()
}

// canceledOutcome 保证「被取消」这个事实一定能从中枢/用户看得到。
//
// 契约 §3.4.4 要求 cancel 后的 result.error 含 canceled：runner 在 ctx 取消时
// 本来就会写 canceled（§3.3），这里只在它没写时补一个前缀——否则一次取消会
// 长得像普通的「退出码非 0」，排障时看不出是中枢叫停的。
func canceledOutcome(jctx context.Context, out outcome) outcome {
	if jctx.Err() == nil || out.Status != statusFailed {
		return out
	}
	if strings.Contains(strings.ToLower(out.Err), "canceled") {
		return out
	}
	if out.Err == "" {
		out.Err = "canceled"
	} else {
		out.Err = "canceled: " + out.Err
	}
	return out
}

func (a *nodeAgent) handleCancel(f *cancelFrame) {
	a.taskMu.Lock()
	handle, ok := a.tasks[f.TaskID]
	a.taskMu.Unlock()
	if !ok {
		// 常见且无害：任务刚好跑完、或它是在本连接之前派下去的。
		a.log.Info("cancel for unknown task",
			"event", "agent.cancel.unknown", "task_id", f.TaskID, "reason", f.Reason)
		return
	}
	a.log.Info("canceling task",
		"event", "agent.cancel", "task_id", f.TaskID, "reason", f.Reason)
	handle.cancel()
}

// untrackTask 摘掉任务登记。
//
// 只在表里那一份还是自己时才删：同一 task_id 被重派后，旧 goroutine 退出不能把
// 新任务的登记抹掉（否则后续 cancel 帧就找不到人）。
func (a *nodeAgent) untrackTask(taskID string, handle *taskCancel) {
	a.taskMu.Lock()
	if a.tasks[taskID] == handle {
		delete(a.tasks, taskID)
	}
	a.taskMu.Unlock()
}

// cancelAll 取消所有在跑任务并断掉 base（兜住残留写与残留任务）。
func (a *nodeAgent) cancelAll() {
	a.taskMu.Lock()
	handles := make([]*taskCancel, 0, len(a.tasks))
	for _, handle := range a.tasks {
		handles = append(handles, handle)
	}
	a.taskMu.Unlock()

	for _, handle := range handles {
		handle.cancel()
	}
	a.cancel()
}

// semaphore 取某个能力（cap@ver）的信号量，不存在就按 limit 建一个。
//
// 用 cap@ver 当键而不是 cap：同一能力不同版本可以声明不同的 concurrency。
func (a *nodeAgent) semaphore(capability string, version, limit int) chan struct{} {
	if limit < 1 {
		limit = 1
	}
	key := fmt.Sprintf("%s@%d", capability, version)

	a.semMu.Lock()
	defer a.semMu.Unlock()
	if sem, ok := a.sems[key]; ok {
		return sem
	}
	sem := make(chan struct{}, limit)
	a.sems[key] = sem
	return sem
}

// sendResult 发 result 帧。
//
// out 透传 outcome.Out（契约 I5 三态）：runner 只在「任务成功且 node.yaml 声明了 run.out」
// 时给它设值 —— 非 nil → 帧里带 "out":"<出口名>"（服务端 Reported()==true，按边
// from_port 派下一跳）；nil → 帧里**没有** out 键（没回报出口：没声明 run.out，
// 或任务没成功/超时/取消）。
func (a *nodeAgent) sendResult(taskID string, out outcome) {
	frame := &resultFrame{
		Type:      frameTypeResult,
		TaskID:    taskID,
		Status:    out.Status,
		Out:       out.Out,
		Summary:   out.Summary,
		Artifacts: out.Artifacts,
		Error:     out.Err,
	}
	// 兜一层：runner 的 outcome 只有两种合法 status，出现第三种就是 bug，
	// 发出去会让中枢的状态机卡住；宁可自己回一条 failed。
	if frame.Status != statusSucceeded && frame.Status != statusFailed {
		frame.Status = statusFailed
		if frame.Error == "" {
			frame.Error = fmt.Sprintf("runner returned invalid status %q", out.Status)
		}
	}
	if err := a.send(frame); err != nil {
		a.log.Warn("send result failed",
			"event", "agent.result.failed",
			"task_id", taskID, "status", frame.Status, "error", err.Error())
	}
}

// heartbeatLoop 每 interval 发一帧心跳（契约 §3.4.2）。
func (a *nodeAgent) heartbeatLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-ticker.C:
			if err := a.send(&heartbeatFrame{Type: frameTypeHeartbeat, TS: tick.Unix()}); err != nil {
				// 心跳发不出去说明连接已经在坏；读循环会先发现并返回错误，
				// 这里退出即可（不在这里关连接，避免和读循环抢所有权）。
				a.log.Warn("heartbeat failed", "event", "agent.heartbeat.failed", "error", err.Error())
				return
			}
		}
	}
}

// drain 优雅退出：等在跑任务收尾，**最多 grace**，然后由调用方关连接。
//
// 超时不强求结果：到点就取消剩余任务（杀进程组），契约 §3.4.6 只要求
// 「等在跑任务最多 grace」——进程退出时不该被一个卡死的任务永远拖住。
func (a *nodeAgent) drain(grace time.Duration) {
	// 与 reserveTask 互斥地置位 draining：置位之后不再有 wg.Add，
	// 下面的 wg.Wait 才是安全的（见 reserveTask 的注释）。
	a.wgMu.Lock()
	a.draining.Store(true)
	a.wgMu.Unlock()

	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()

	if grace <= 0 {
		a.log.Warn("no grace: canceling running tasks", "event", "agent.drain.nograce")
		a.cancelAll()
		return
	}

	startAt := time.Now()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		a.log.Info("drained running tasks",
			"event", "agent.drain.done", "elapsed", time.Since(startAt))
	case <-timer.C:
		a.log.Warn("grace expired: canceling running tasks",
			"event", "agent.drain.expired", "grace", grace)
		a.cancelAll()
	}
}
