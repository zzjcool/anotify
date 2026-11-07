package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/zzjcool/anotify/internal/artifact"
)

// 本文件是 agent 的**连接与调度层**：dial → hello → 等 welcome → 心跳 + 读循环
// （assign / deploy / cancel / error）→ 优雅退出。
//
// 边界（不变量 I1）：中枢不执行，也不告诉节点跑什么。assign 帧里只有
// capability / version / params / digest / trace_id，**没有任何命令文本**；节点自己
// 按 digest（或 name@version）去本地产物缓存（cache.go）里找到那份产物再执行。
// 命令的唯一来源是控制台编译出来的产物，而产物是内容寻址（digest）、不可变的。
//
// 并发模型（契约 §3.4）：
//   - 读：单 goroutine（serveNode 起的读循环），一帧一帧处理（deploy 也在里面，
//     同步做：见 apply.go 的注释）；
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
	// nodeReadLimit 是单帧读取上限：assign 帧只有 params/digest，足够；
	// deploy 帧带整份产物（服务端单产物上限 1 MiB + JSON 包装），所以放宽到 4 MiB。
	// 超了会被 coder/websocket 用 1009 关连接，节点只看到「连接断了」——
	// 那是排查成本最高的一类故障，宁给宽一点。
	nodeReadLimit = 4 << 20
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
// dataDir 是节点数据目录（产物缓存 + 工作目录的根，见 cache.go）。
// 返回 nil 表示「按信号正常退出」；连接断开返回 connection closed（v1 不做退避重连，
// 交给进程管理器重启）。
func runNode(ctx context.Context, cfg *nodeConfig, dataDir string, log *slog.Logger, grace time.Duration) error {
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
		"slots", cfg.slots(),
		"data_dir", dataDir,
		"cached", len(newCache(dataDir).applied()),
	)
	return serveNode(ctx, cfg, conn, dataDir, log, grace)
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
func serveNode(ctx context.Context, cfg *nodeConfig, conn nodeConn, dataDir string, log *slog.Logger, grace time.Duration) error {
	if cfg == nil {
		return errors.New("serveNode: nil node config")
	}
	if conn == nil {
		return errors.New("serveNode: nil connection")
	}
	if log == nil {
		log = slog.Default()
	}

	a := newNodeAgent(ctx, cfg, conn, dataDir, log)
	// 本机事实面（issue #320 §2）：先把「进程起来了、还没连上」落成一拍，之后每次
	// 心跳刷新 updatedAt。defer stop：连接断开、握手失败、信号退出都把 connected
	// 置回 false；**不**动 lastError（上次的真实故障仍然值得看）。
	a.state.start()
	defer a.state.stop()
	// 执行档案的后台 GC（启动时一次 + 每小时一次，见 runstore.go）：它挂在**连接自己的**
	// ctx（a.base）上，所以 drain/退出时随 a.cancel() 一起结束；stop 再兜一层，
	// 保证函数提前返回（握手失败…）时也不留下后台 goroutine（重复调用安全）。
	// 放在握手之前：开机就把上一次进程攒下的过期档案清掉，不等第一条任务。
	stopRunGC := startRunGC(a.base, a.runs, a.log)
	defer stopRunGC()
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
		a.state.fail(kindFailed, err.Error())
		return err
	}
	a.state.setConnected(true, welcome.HeartbeatSec)
	a.log.Info("handshake ok",
		"event", "agent.handshake.ok",
		"node_id", welcome.NodeID,
		"protocol", welcome.Protocol,
		"heartbeat_sec", welcome.HeartbeatSec,
		"lease_ttl_sec", welcome.LeaseTTLSec,
		"data_dir", a.dataDir,
		// 现扫一遍缓存：这一帧之后中枢对这台机器的认知就以此为据（D4），
		// 日志里留一份同样的数字，排障时不用去猜「当时报了几条」。
		"applied", len(a.cache.applied()),
	)

	// 中枢点名要剪掉的陈旧缓存（能力已在控制台删除）。
	//
	// 为什么由中枢下指令而不是节点自己猜：节点只知道「我缓存了什么」，不知道
	// 「中枢还认不认」。早先中枢对这种情况直接拒握手，agent 视为致命错误退出，
	// systemd 每 3 秒把它拉起来再拒一次 —— 删一个能力就能打死所有在线节点。
	for _, p := range welcome.Prune {
		digest, dirRemoved, perr := a.cache.prune(p.Capability, p.Version)
		if perr != nil {
			// 剪缓存失败不是致命错误：下一次握手还会收到同一条指令。
			a.log.Warn("prune stale artifact failed",
				"event", "agent.prune.failed",
				"capability", p.Capability, "version", p.Version, "error", perr.Error())
			continue
		}
		a.log.Info("pruned stale artifact",
			"event", "agent.prune.ok",
			"capability", p.Capability, "version", p.Version,
			"digest", digest, "dir_removed", dirRemoved)
	}

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
		// 连接断开是**节点级**错误：写进 state.json 的 lastError（status 里看得到），
		// 同时把 connected 置 false——只靠日志回答不了「它现在还行吗」。
		a.state.fail(kindCommFailed, readErr.Error())
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
	// cache 是本机的产物缓存（deploy 帧写、assign 帧读、hello.applied 扫它）。
	cache *cache
	// dataDir 是数据目录根（缓存与工作目录都在它下面）。
	dataDir string

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

	// 每能力（cap@ver）一个信号量：容量 = cfg.taskLimit()（= 槽位数）。
	semMu sync.Mutex
	sems  map[string]chan struct{}

	// 在跑任务表（task_id → 取消句柄），供 cancel 帧查找。
	taskMu sync.Mutex
	tasks  map[string]*taskCancel

	// emits 是本连接上在跑任务的 emit socket 服务（task_id → 服务），
	// 供 emit_ack 找回来路（中枢的确认只带 task_id + seq）。
	emitMu sync.Mutex
	emits  map[string]*emitServer

	// runs 是本机的**执行档案库**（<data-dir>/runs/，见 runstore.go）：每次执行的
	// 输入 + 完整输出落在跑它的这台机器上，事后用 `anotify-agent runs` 回看，
	// 也是中枢取日志（logs_request）的唯一数据来源。
	// 写盘失败只 Warn，绝不参与任务成败。
	runs *runStore

	// state 是本机事实面（<data-dir>/state.json，见 state.go）：每心跳刷新 updatedAt，
	// 任务进出时增减 busy，连接成败时改 connected/lastError。
	// `anotify-agent status` 只读这个文件（不连 socket），写失败也只 Warn。
	state *stateWriter

	// wg 追踪所有任务 goroutine：优雅退出要等它们（最多 grace）。
	//
	// wgMu 只在两件事上持有：reserveTask 做 Add、drain 置 draining。两件事
	// 互斥，就排除了 Add 与 Wait 并发（sync.WaitGroup 的硬要求）。
	wgMu sync.Mutex
	wg   sync.WaitGroup

	// draining 置位后不再接新任务（不 ack，让中枢的租约到期回收）。
	draining atomic.Bool

	// installToForTest / exitHook 只在测试里用：前者覆盖升级目标（测试不能替换
	// 自己），后者拦下 os.Exit（成功路径会退出，否则用例跑不到断言）。
	installToForTest string
	exitHook         func(int)
}

func newNodeAgent(parent context.Context, cfg *nodeConfig, conn nodeConn, dataDir string, log *slog.Logger) *nodeAgent {
	// 空数据目录退到系统临时目录：生产路径上 run() 一定给了绝对路径，
	// 这里只是不让「传空」变成「往当前目录写 artifacts/」（那是最难查的一类脏）。
	if strings.TrimSpace(dataDir) == "" {
		dataDir = filepath.Join(os.TempDir(), "anotify")
	}
	base, cancel := context.WithCancel(context.WithoutCancel(parent))
	a := &nodeAgent{
		cfg:     cfg,
		conn:    conn,
		log:     log,
		cache:   newCache(dataDir),
		dataDir: dataDir,
		parent:  parent,
		base:    base,
		cancel:  cancel,
		sems:    make(map[string]chan struct{}),
		tasks:   make(map[string]*taskCancel),
		emits:   make(map[string]*emitServer),
		// 档案库在这里只**构造句柄**（不建目录、不跑 GC）：目录由第一次 Save 建，
		// GC 由 serveNode 在握手前拉起（见 startRunGC）。
		runs: newRunStore(dataDir, log),
		// 状态快照句柄（#320 §2）：第一拍由 serveNode 的 start() 落，之后每次心跳 touch。
		state: newStateWriter(dataDir, cfg, log),
	}
	return a
}

// exit 结束进程。（升级成功后要退出让 supervisor 拉起新版本；测试用 exitHook 拦下。）
func (a *nodeAgent) exit(code int) {
	if a.exitHook != nil {
		a.exitHook(code)
		return
	}
	os.Exit(code)
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
// hello 里带 applied[]（本机缓存了哪些产物）：中枢靠它全量对账 node_capabilities
// （裁决 D4）——所以缓存状态是**每次握手现扫的**，不存内存副本。
//
// 期间收到 error 帧 = 中枢拒绝（令牌/节点名/实例数不合规，或 applied 里出现了
// 中枢不认识的能力），必须作为**失败**返回：契约 §3.4.1 的错误文案是运维唯一
// 能看懂的线索，逐字固定。
//
// 读的 ctx 用**父 ctx**（契约 §11.1 第 3 条）：握手阶段还没有要保全的 result，
// 父 ctx 取消时直接放弃握手是对的（调用方把 ctx.Err() 当「正常退出」处理）。
// 一旦握手成功，读就必须换成 a.base（见 readLoop）。
func (a *nodeAgent) handshake(ctx context.Context) (*welcomeFrame, error) {
	if err := a.sendHello(); err != nil {
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
		case *deployFrame:
			// 部署同步做（不扔 goroutine）：同一连接上 deploy 与 assign 的先后顺序
			// 才确定——中枢推完 deploy 立刻派单时，缓存一定已经可读。
			if err := a.handleDeploy(ctx, f); err != nil {
				// 只有「没能告诉中枢结果」（写失败 / 连接已断）会走到这里；
				// 部署本身的失败已经写进 apply 帧的 reason 里了（见 apply.go）。
				a.log.Warn("deploy handling failed",
					"event", "agent.deploy.failed",
					"deploy_id", f.DeployID, "error", err.Error())
			}
		case *upgradeFrame:
			// 升级是**同步**做的：下载 + 校验 + 原子替换，成功就退出让 supervisor
			// 拉起新版本（契约 §5）。扔 goroutine 的话，下一帧 assign 可能先到达，
			// 结果是「用旧二进制跑着跑着进程突然没了」。
			a.handleUpgrade(f)
		case *cancelFrame:
			a.handleCancel(f)
		case *emitAckFrame:
			// 中途产出的确认（emit-protocol.md §3.2）：唤醒 --wait-routed 的等待者、
			// 把成功的 seq 记进 outbox、把被拒的 seq 标成「不再重发」。
			a.handleEmitAck(f)
		case *logsRequestFrame:
			// 中枢按需取日志（CONTRACT-logs-on-node §1）。**必须**扔 goroutine：读循环
			// 停住的时间不止「读 1 MiB 盘」——同一条连接上的 heartbeat / assign / result
			// 全排在它后面，而中枢判活看的就是心跳间隔。一次慢日志读取能把自己显示成
			// 掉线，顺带让租约到期、任务被重派。日志是旁路，不配让主路等它。
			go a.handleLogsRequest(f)
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

	art, digest, ok := a.resolveArtifact(f)
	if !ok {
		// 已 ack 却跑不了：必须回 failed，否则中枢要等租约到期（契约 §3.4.3）。
		a.startUnrunnable(f)
		return
	}
	a.startTask(f, art, digest)
}

// resolveArtifact 找到本次任务要跑的那份产物。
//
// 优先按 assign.Digest（中枢指明了「就跑这一份」，能钉死在审计过的那版上）；
// 缺省按 capability@version 查索引（中枢没给 digest 的旧路径）。
//
// 第二个返回值是 cache 里**重新算出来**的 digest（而不是 assign 里那个字符串），
// 所以 ANOTIFY_DIGEST 永远是真正在跑的那份内容的哈希：用户手改过缓存里的产物时，
// 脚本里看到的就是改后的那版（同时中枢会在下次 hello 里发现不一致并重新下发）。
func (a *nodeAgent) resolveArtifact(f *assignFrame) (*artifact.Artifact, string, bool) {
	if f.Digest != "" {
		return a.cache.getByDigest(f.Digest)
	}
	return a.cache.get(f.Capability, f.Version)
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

// startUnrunnable 收回「本地没有这份产物」的派单：result failed。
//
// 文案固定为 `no artifact for <cap>@<ver>`：中枢/运维要能一眼区分「节点没接单」
// 与「节点接了但没有这份产物」（后者通常意味着部署还没生效、或缓存被清过）。
func (a *nodeAgent) startUnrunnable(f *assignFrame) {
	errText := fmt.Sprintf("no artifact for %s@%d", f.Capability, f.Version)
	a.log.Warn("assign without cached artifact",
		"event", "agent.assign.no_artifact",
		"task_id", f.TaskID, "capability", f.Capability, "version", f.Version,
		"digest", f.Digest)

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
func (a *nodeAgent) startTask(f *assignFrame, art *artifact.Artifact, digest string) {
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

	sem := a.semaphore(f.Capability, f.Version, a.cfg.taskLimit())
	j := job{
		TaskID:      f.TaskID,
		Capability:  f.Capability,
		Version:     f.Version,
		Params:      f.Params,
		Artifact:    art,
		Digest:      digest,
		TraceID:     f.TraceID,
		MaxDuration: f.MaxDuration,
		WorkRoot:    a.cache.workRoot(),
		// 落盘用的完整输出只在**中枢派单**这条路上采（见 job.CaptureFull）：
		// 这份输出不回传中枢，只写进本机的 runs/（契约 §3.1/§3.2）。
		CaptureFull: true,
		// 端口与下游契约都从 assign 帧原样带过来（**不重算**）：端口白名单与
		// ANOTIFY_EMIT_PORTS 必须同源，见 emitPortsOf。
		Ports:   emitPortsOf(f, art),
		Expects: f.Expects,
		// 注意：cwd 不从中枢拿（帧里的 work_dir 已作废）。唯一来源是产物里的
		// spec.run.workDir（见 runner.resolveChdir）；空 = 本次任务目录。
		// 日志口给执行层用来记一行「声明的工作目录 → 展开后的真实路径」（`~/x`）。
		Log: a.log,
	}

	// 名额已在 handleAssign 里占好（reserveTask）：这里只负责还。
	go func() {
		defer a.wg.Done()
		defer a.untrackTask(f.TaskID, handle)
		defer cancel()

		// 每个能力一个信号量：容量 = cfg.taskLimit()（= 槽位数，见 config.go）。
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

		// 槽位占用进本机快照（#320 §2）：status 的 slots.busy 是「正在执行」的数量，
		// 不含还在排队等信号量的（那些还没开始跑）。
		a.state.setBusy(1)
		defer a.state.setBusy(-1)

		startAt := time.Now()
		// emit socket 必须在**起能力进程之前**就监听好（emit-protocol.md §1）：
		// 进程里第一次 `anotify-agent emit` 不能撞上「socket 还没起来」。
		// 同时把 outbox 里上一次没 ack 的条目按 seq 补发（§7 的最后一条）。
		es := a.startEmitServer(jctx, f, art)
		if es != nil {
			j.Emit = es
		}
		out := runJob(jctx, j)
		// emit 次数必须在 Close **之前**取（见 emitServer.EmitCount）：它要进本次执行的
		// meta.json，而 Close 之后这个服务就不再接单了。
		emits := es.EmitCount()
		finishedAt := time.Now()
		// 能力进程已经退出（正常结束 / 超时 / 取消）：**先**关 socket 再回报 result。
		// 顺序有意义——关掉之后晚到的 emit 只会得到「socket 不存在」（exit 6），
		// 不可能在 result 之后还改动一个已终结的任务（§8）。
		if es != nil {
			a.stopEmitServer(f.TaskID, es)
			es.Close()
		}
		// 落盘**在 result 之前**（契约 §3.2）：result 一到中枢，这条 trace 就有人在看了，
		// 那时「输入与完整输出已经在盘上」必须是确定的事。失败只 Warn（见 saveRunArchive）。
		a.saveRunArchive(f, out, emits, startAt, finishedAt)
		// 日志不再上传（CONTRACT-logs-on-node §3）：日志只留在本机档案里，中枢要看时
		// 发一帧 logs_request 来取（见 logs_serve.go）。所以 result 之前的收尾只剩
		// 「档案落盘」这一件事——那条「先传日志再发 result」的顺序要求已经不存在了。
		a.log.Info("task finished",
			"event", "agent.task.finished",
			"task_id", f.TaskID,
			"capability", f.Capability,
			"version", f.Version,
			"digest", digest,
			"status", out.Status,
			"elapsed", time.Since(startAt),
			"workdir", out.WorkDir,
			// cwd 才是进程真实跑在的目录（= $ANOTIFY_WORKDIR）：能力声明了 ~/x 时
			// 它是展开 + 解链后的真实路径，跟上面的任务目录不是同一个。
			"cwd", out.Cwd,
			"summary", out.Summary,
			"error", out.Err,
		)
		a.log.Debug("task output",
			"event", "agent.task.output", "task_id", f.TaskID,
			"stdout", out.Stdout, "stderr", out.Stderr)
		a.sendResult(f.TaskID, canceledOutcome(jctx, out))
	}()
}

// runRecordFor 把一次执行组装成要落盘的全部内容（**纯函数**：不碰磁盘、不改 out）。
//
// 拆出来是为了可测：它是「档案里到底写了什么」的唯一实现，单测能拿真的 assign 帧与
// 真的 outcome 直接钉住字段映射，不必为了验一次映射而拉起整套 serveNode。
//
// 两条口径：
//   - params **原样**（不脱敏、不裁剪）：档案只落在跑它的这台机器上，用户要看的正是
//     「当时到底传了什么」（见 runstore.go 的文件头）。
//   - Attempt / SeqInTrace 来自**中枢的派发快照**（assign 帧），节点不自己算：
//     「这是同一 trace 同一能力的第几次执行」归中枢，节点重算一份就会与它分叉。
func runRecordFor(f *assignFrame, out outcome, emits int, started, finished time.Time) runRecord {
	return runRecord{
		Input: runInput{
			TraceID:    f.TraceID,
			TaskID:     f.TaskID,
			Capability: f.Capability,
			Version:    f.Version,
			Attempt:    f.Attempts,
			SeqInTrace: f.SeqInTrace,
			Params:     f.Params,
			WorkDir:    out.WorkDir,
		},
		Meta: runMeta{
			TraceID:    f.TraceID,
			TaskID:     f.TaskID,
			Capability: f.Capability,
			Version:    f.Version,
			Attempt:    f.Attempts,
			SeqInTrace: f.SeqInTrace,

			Status:   out.Status,
			ExitCode: out.ExitCode,
			Summary:  out.Summary,
			Error:    out.Err,

			Emits: emits,

			StdoutBytes:     out.ArchiveStdoutBytes,
			StderrBytes:     out.ArchiveStderrBytes,
			StdoutTruncated: out.ArchiveStdoutTruncated,
			StderrTruncated: out.ArchiveStderrTruncated,
		},
		// 两份**完整**输出（不是上报窗口）：落盘才是「当时到底打印了什么」的全部答案。
		Stdout: out.ArchiveStdout,
		Stderr: out.ArchiveStderr,

		Started:  started,
		Finished: finished,
	}
}

// saveRunArchive 把这次执行（输入 + 完整输出 + meta + 任务索引）落盘到 <data-dir>/runs/。
//
// 一条纪律：**失败只 Warn，绝不改 out.Status**。磁盘满、runs 目录被一个同名文件占着、
// params 里混进了编不出 JSON 的值——这些都不该把一个跑成的任务判成失败：本地档案是
// 「可惜丢了一份证据」，不是「任务没跑成」。
func (a *nodeAgent) saveRunArchive(f *assignFrame, out outcome, emits int, started, finished time.Time) {
	if a.runs == nil {
		return
	}
	if _, err := a.runs.Save(runRecordFor(f, out, emits, started, finished)); err != nil {
		a.log.Warn("run archive write failed",
			"event", "agent.runs.save_failed",
			"task_id", f.TaskID,
			"trace_id", f.TraceID,
			"capability", f.Capability,
			"seq_in_trace", f.SeqInTrace,
			"attempt", f.Attempts,
			"error", err.Error())
	}
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

// handleCancel 处理中枢的 cancel 帧。
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

// emitPortsOf 是本次任务的产出端口集合（**唯一**来源）。
//
// 它同时喂给两个地方：emit socket 的白名单（startEmitServer）与交给能力进程的
// ANOTIFY_EMIT_PORTS（buildEnv，经 job.Ports）。两处各算一次的症状就是「进程
// 看到的端口」与「emit 时被允许的端口」在某次改动后悄悄分叉：脚本照着契约 emit，
// 却被自己的节点以 exit 4 打回来。
//
// 优先级不变：assign 的 ports（中枢算的快照）优先；没下发时（旧中枢 / 空快照）
// 退到产物自己声明的 spec.outputs。它同样是**封闭集合**（I2 的源头），所以这不是
// 放宽，只是换了个同源的事实。
func emitPortsOf(f *assignFrame, art *artifact.Artifact) []string {
	if f != nil && len(f.Ports) > 0 {
		return f.Ports
	}
	if art == nil {
		return nil
	}
	return art.Spec.Outputs
}

// startEmitServer 为这次任务起本地 emit socket（emit-protocol.md §1）。
//
// ctx 是**任务自己的** ctx：它取消（cancel 帧 / 收尾超时）就等于「任务已非运行态」，
// socket 也随之收口（watchCtx）。
//
// 起不来时**不让任务失败**：能力可能根本不 emit（一次性返回也是合法的产出方式），
// 因为一个本地 IPC 就把它判失败，代价比「脚本里 emit 得 exit 6」大得多。所以这里
// 只记一条 warn，任务照跑（脚本真 emit 了会自己拿到 exit 6 和一句可读的话）。
func (a *nodeAgent) startEmitServer(ctx context.Context, f *assignFrame, art *artifact.Artifact) *emitServer {
	ports := emitPortsOf(f, art)
	if len(f.Ports) == 0 && art != nil {
		// 中枢没下发 ports（旧中枢 / 空快照）：退到产物自己声明的 spec.outputs。
		a.log.Debug("assign 没有 ports，退到产物的 spec.outputs",
			"event", "agent.emit.ports.fallback",
			"task_id", f.TaskID, "ports", ports)
	}
	if f.Token == "" {
		// 空令牌 = 不鉴权：同机其他进程能替这个任务 emit。这是中枢侧的退化，
		// 节点无法自己造一个令牌（中枢得认它），所以只能把风险记下来。
		a.log.Warn("assign 没有下发 emit 令牌：本地 socket 不鉴权",
			"event", "agent.emit.token.empty", "task_id", f.TaskID)
	}

	es, err := newEmitServer(emitServerOptions{
		DataDir: a.dataDir,
		TaskID:  f.TaskID,
		Token:   f.Token,
		Ports:   ports,
		Expects: f.Expects,
		Ctx:     ctx,
		Log:     a.log,
		Send:    func(frame *emitFrame) error { return a.send(frame) },
	})
	if err != nil {
		a.log.Warn("emit socket 起不来（任务照跑，但 emit 不可用）",
			"event", "agent.emit.socket.failed",
			"task_id", f.TaskID, "error", err.Error())
		return nil
	}

	a.emitMu.Lock()
	a.emits[f.TaskID] = es
	a.emitMu.Unlock()
	a.log.Info("emit socket ready",
		"event", "agent.emit.socket.ready",
		"task_id", f.TaskID, "path", es.Path(), "ports", es.portList())
	return es
}

// stopEmitServer 把服务从登记表里摘下（只在表里那一份还是自己时才删）。
func (a *nodeAgent) stopEmitServer(taskID string, es *emitServer) {
	a.emitMu.Lock()
	if a.emits[taskID] == es {
		delete(a.emits, taskID)
	}
	a.emitMu.Unlock()
}

// handleEmitAck 把中枢对一次 emit 的确认转给对应任务的 emit 服务。
func (a *nodeAgent) handleEmitAck(f *emitAckFrame) {
	a.emitMu.Lock()
	es := a.emits[f.TaskID]
	a.emitMu.Unlock()
	if es == nil {
		// 常见且无害：任务刚好结束（socket 已收），或是上一个连接残留的答复。
		a.log.Debug("emit_ack 对应不到在跑任务",
			"event", "agent.emit_ack.unknown", "task_id", f.TaskID, "seq", f.Seq)
		return
	}
	es.acknowledge(f)
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
// 出口**不在这条帧里**：out 已退役（P1.5），路由只看 emit 的 port。成败照实发——
// status 只做展示与重试策略，与「走了哪个出口」无关（I4 的形式不变，载体变了）。
func (a *nodeAgent) sendResult(taskID string, out outcome) {
	frame := &resultFrame{
		Type:      frameTypeResult,
		TaskID:    taskID,
		Status:    out.Status,
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
			// 每一拍都刷新 state.json 的 updatedAt：这是 `anotify-agent status` 判定
			// live/stale 的唯一证据（pid 在但心跳停了 = 卡死，也要能看出来）。
			a.state.touch()
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
