package main

// 本文件是 agent 的**本地 emit 服务**：让能力进程把中途产出带回来
// （规范：docs/design/emit-protocol.md，线格式：internal/ws/node_protocol.go）。
//
// # 进程树（§0）
//
//	中枢 ←──WS──── anotify-agent ←──unix socket──── anotify-agent emit ←──exec── 能力进程
//
// 能力进程**不持有中枢凭证**，也不直连网络：它只能通过这个本地 socket 说服 agent
// 代发一帧 emit。鉴权靠 assign 下发的一次性任务令牌（→ ANOTIFY_TASK_TOKEN）：
// 同一台机器上可能有别的任务的进程，没有令牌它就能替这个任务伪造产出。
//
// # 两级 ack（§5）——这是本文件最重要的设计点
//
//   - 默认：追加进本地 outbox 并 fsync 之后立刻回 acked:"durable"（毫秒级）。
//     能力进程（尤其 LLM）一次运行会 emit 很多次，每次都等中枢 RTT 会把长任务
//     拖慢一个数量级。
//   - --wait-routed：等中枢的 emit_ack 再回 acked:"routed"（一次 RTT）。
//
// 「不丢」由 **outbox + 中枢侧 (task_id, seq) 幂等**保证，不靠同步等待：agent 崩了
// 或断线了，下一次启动把 outbox 里没 ack 的条目按 seq 升序补发，中枢按唯一约束
// 去重。缺口是丢消息的证据，中枢会明确回 error——这里不猜、不跳过、不静默重排。
//
// # 校验顺序（§6，逐条照抄）
//
//	token → 任务运行态 → port ∈ ports[] → data 是 JSON 对象 →
//	data ⊨ expects[port].schema → 体积 ≤64KiB → 本任务累计 <1000 次
//
// 顺序不是随意排的：它是「越便宜、越可能是调用方搞错」的判定越靠前，于是被拒时
// 给出的原因总是**当下最具体**的那一条（缺哪个字段，而不是"反正你哪儿错了"）。
//
// # 边界
//
//   - socket 的**生命周期就是任务的执行生命周期**：起能力进程之前 Listen，进程
//     退出 / 超时 / 取消 / agent 关闭 → 关 socket 并删文件。晚到的 emit 会被
//     「socket 不存在」挡在外面（§8），不允许它改动已终结的任务。
//   - 目录 0700、socket 0600：只要目录能写，别人就能**替换** socket 文件，
//     所以文件权限单独设不够，目录必须收口。
//   - 这里不做 schema 解释：一律转发 internal/schema（零依赖，不把 sqlite 链进
//     节点二进制）。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/anotify/internal/schema"
	"github.com/zzjcool/anotify/internal/ws"
)

// emit 协议里出现的**字符串取值**与本地实现常量。
const (
	// emitSocketDirName / emitOutboxDirName 是 data-dir 下的两个子目录。
	emitSocketDirName = "emit"
	emitOutboxDirName = "outbox"
	emitSockSuffix    = ".sock"
	emitOutboxSuffix  = ".jsonl"

	// emitDirMode / emitFileMode 是 socket 与 outbox 的权限（见文件头）。
	emitDirMode  os.FileMode = 0o700
	emitFileMode os.FileMode = 0o600

	// emitMaxDataBytes 是一次 emit 的 data 体积上限（§6 第 6 条）。
	emitMaxDataBytes = 64 << 10
	// emitMaxCount 是单任务累计 emit 次数上限（§6 第 7 条）。
	emitMaxCount = 1000

	// maxUnixSocketPath 是 sun_path 的实际上限（Linux 108 / BSD 104）：超了
	// bind 会返回一句很难懂的 "invalid argument"，不如自己先报清楚。
	maxUnixSocketPath = 100

	// emitReadTimeout 是「一条请求必须在这段时间内写完」，防半开连接占着 goroutine。
	emitReadTimeout = 30 * time.Second
	// emitReplyTimeout 是回一条响应的上限。
	emitReplyTimeout = 10 * time.Second
	// emitMaxRequestBytes 是单行请求的硬上限（64KiB data + JSON 包装 + 备注，留足余量）。
	// 它挡的是「拿超长行把 agent 内存吃掉」，而 64KiB 那条业务上限在 emit() 里判。
	emitMaxRequestBytes = 1 << 20
	// emitWaitRoutedTimeout 是 --wait-routed 等中枢 emit_ack 的上限（§2 的 exit 7）。
	emitWaitRoutedTimeout = 60 * time.Second

	// emitOpName / emitWaitRoutedName 是 socket 协议里的两个取值（§1）。
	emitOpName         = "emit"
	emitWaitRoutedName = "routed"
	// emitNoteMaxBytes 是备注的长度上限：note 只给人看，超了就截断（不拒绝）。
	emitNoteMaxBytes = 1 << 10
)

// acked 的两个取值（§1）。
const (
	emitAckedDurable = "durable"
	emitAckedRouted  = "routed"
)

// rejected 的取值（§1 的枚举，§6 的判定顺序）。
const (
	emitRejectToken  = "token"
	emitRejectTask   = "task"
	emitRejectPort   = "port"
	emitRejectSchema = "schema"
	emitRejectSize   = "size"
	emitRejectRate   = "rate"
)

// 注入给能力进程的两个环境变量（§1 的「注入」一行）。
const (
	// envEmitSock 指向本次任务的本地 socket。
	envEmitSock = "ANOTIFY_EMIT_SOCK"
	// envTaskToken 是 socket 侧的鉴权令牌（每任务随机，任务结束即销毁）。
	envTaskToken = "ANOTIFY_TASK_TOKEN"
)

// emitRequest 是本地 socket 的**请求**线格式（§1）。
//
// seq 在请求里是可选且**只读**的：§4 规定 seq 由 agent 分配并持久化。让调用方
// 自己发 seq 会立刻分裂成两个写者（两个 emit 进程各发一个 seq 3 → 中枢报
// "seq reused with different payload"，那是节点 bug）。响应里回填真正用掉的那个。
type emitRequest struct {
	Op    string `json:"op"`
	Token string `json:"token"`
	Seq   int64  `json:"seq,omitempty"`
	Port  string `json:"port"`
	// To 是脚本点名的下游能力（`emit --to`）。空 = 由中枢按候选选。
	To   string          `json:"to,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	Note string          `json:"note,omitempty"`
	// Wait == "routed" 时等中枢 emit_ack 再回（对应 --wait-routed）。
	Wait string `json:"wait,omitempty"`
}

// emitResponse 是本地 socket 的**响应**线格式（§1）。字段与取值严格照文档。
type emitResponse struct {
	OK       bool   `json:"ok"`
	Seq      int64  `json:"seq"`
	Acked    string `json:"acked"`
	Error    string `json:"error,omitempty"`
	Rejected string `json:"rejected,omitempty"`
}

// outboxRecord 是 outbox 文件里的一行。
//
// 同一个文件里有两种行（append-only，两种都带 seq）：
//
//	{"seq":1,"port":"fix","data":{…},"note":"","ts":…}   一次 emit
//	{"seq":1,"ack":{"routed":2,"pruned":0,"error":""}}   中枢的答复
//
// 为什么追加而不是就地改：崩在写一半时最多丢最后那半行（读的时候跳过），
// 不会把整份 outbox 写坏；而「丢了一条」在中枢侧会表现为 seq 缺口（可见）。
type outboxRecord struct {
	Seq  int64           `json:"seq"`
	Port string          `json:"port,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	Note string          `json:"note,omitempty"`
	TS   int64           `json:"ts,omitempty"`
	Ack  *outboxAck      `json:"ack,omitempty"`
}

// outboxAck 是中枢对某个 seq 的答复（落盘只为一件事：重启后**不再重发被拒的条目**）。
type outboxAck struct {
	Routed int    `json:"routed"`
	Pruned int    `json:"pruned,omitempty"`
	Error  string `json:"error,omitempty"`
}

// pendingEmit 是一条「已经落盘、但中枢还没 ack」的 emit。
type pendingEmit struct {
	seq int64
	// frame 是重新发给中枢时用的原件（同 seq 重发 → 中枢幂等）。
	frame *emitFrame
	// sent 表示本轮连接里已经发过一次（补发只做一次，不在这里无限重试）。
	sent bool
	// failed 非空 = 中枢明确拒收过这个 seq → 永不再发（§2「不要无限重试」）。
	failed string
	// done 只给 --wait-routed 的等待者用（buffered，没人读也不会阻塞发送方）。
	done chan emitAckResult
}

// emitAckResult 是一条 emit 的中枢答复（内部表示）。
type emitAckResult struct {
	routed int
	pruned int
	err    string
}

// emitServerOptions 是 newEmitServer 的入参（便于测试直接构造，不依赖 nodeAgent）。
type emitServerOptions struct {
	DataDir string
	TaskID  string
	Token   string
	Ports   []string
	Expects map[string]ws.PortExpect
	Ctx     context.Context
	Log     *slog.Logger
	Send    func(f *emitFrame) error
}

// emitServer 是**一个任务**的本地 emit socket 服务。
type emitServer struct {
	dataDir string
	taskID  string
	token   string
	ports   map[string]struct{}
	expects map[string]ws.PortExpect
	log     *slog.Logger
	// send 把一帧 emit 交给中枢（测试注入 fake；生产是 nodeAgent.send）。
	send func(f *emitFrame) error
	// ctx 是**任务**的 ctx：取消 = 任务已非运行态（§6 第 2 条）。
	ctx context.Context

	sockPath   string
	outboxPath string
	ln         net.Listener

	// wake 叫醒 flusher（容量 1：多次叫醒合并成一次）。
	wake chan struct{}
	// done 在 Close 时关闭，让后台 goroutine 立刻退出。
	done chan struct{}

	mu      sync.Mutex
	closed  bool
	seq     int64 // 已分配的最大 seq（持久化在 outbox 里）
	count   int   // 本任务累计 emit 次数（跨重启累计）
	pending map[int64]*pendingEmit
	outbox  *os.File

	wg sync.WaitGroup
}

// newEmitServer 建出 socket 与 outbox，并立刻开始服务。
//
// 顺序是刻意的：**先 Listen 再返回**——调用方拿到 *emitServer 之后才去起能力进程，
// 所以能力进程里第一次 emit 一定连得上（没有「socket 还没起来」的竞态）。
func newEmitServer(opt emitServerOptions) (*emitServer, error) {
	if strings.TrimSpace(opt.DataDir) == "" {
		return nil, errors.New("emit: 数据目录为空")
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Ctx == nil {
		opt.Ctx = context.Background()
	}
	if opt.Send == nil {
		// 显式的空发送器：/dev/null。这样「没有中枢」是一种**显式**状态，
		// 而不是一个会 nil panic 的字段。
		opt.Send = func(*emitFrame) error { return errEmitNoUpstream }
	}
	taskID, err := workDirName(opt.TaskID)
	if err != nil {
		return nil, fmt.Errorf("emit: %w", err)
	}

	s := &emitServer{
		dataDir: opt.DataDir,
		taskID:  taskID,
		token:   opt.Token,
		ports:   make(map[string]struct{}, len(opt.Ports)),
		expects: opt.Expects,
		log:     opt.Log,
		send:    opt.Send,
		ctx:     opt.Ctx,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		pending: make(map[int64]*pendingEmit),
	}
	for _, p := range opt.Ports {
		if p = strings.TrimSpace(p); p != "" {
			s.ports[p] = struct{}{}
		}
	}

	// 目录 0700：socket 0600 只挡得住「连」，挡不住「把 socket 文件换成一个
	// 自己监听的」，所以目录必须只有本用户能进/写。
	dir := filepath.Join(opt.DataDir, emitSocketDirName)
	if err := os.MkdirAll(dir, emitDirMode); err != nil {
		return nil, fmt.Errorf("emit: 建 socket 目录 %s: %w", dir, err)
	}
	// MkdirAll 不改已存在目录的权限：显式收口（上一次可能建成了别的模式）。
	if err := os.Chmod(dir, emitDirMode); err != nil {
		return nil, fmt.Errorf("emit: 收口 socket 目录 %s: %w", dir, err)
	}
	s.sockPath = filepath.Join(dir, taskID+emitSockSuffix)
	if len(s.sockPath) > maxUnixSocketPath {
		return nil, fmt.Errorf("emit: socket 路径过长（%d > %d 字节）：%s",
			len(s.sockPath), maxUnixSocketPath, s.sockPath)
	}
	// 上一次运行崩溃时可能留下同名 socket 文件（unix socket 不会自动消失）：
	// 不删掉 keep，Listen 会 EADDRINUSE，而那个文件本来也已经没人监听了。
	if err := os.Remove(s.sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("emit: 清理旧 socket %s: %w", s.sockPath, err)
	}
	ln, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return nil, fmt.Errorf("emit: 监听 %s: %w", s.sockPath, err)
	}
	// socket 0600：默认 umask 下 unix socket 往往是 0755，同机其他用户能连上 → 拒。
	if err := os.Chmod(s.sockPath, emitFileMode); err != nil {
		_ = ln.Close()
		_ = os.Remove(s.sockPath)
		return nil, fmt.Errorf("emit: 收口 socket %s: %w", s.sockPath, err)
	}
	s.ln = ln

	if err := s.openOutbox(); err != nil {
		_ = ln.Close()
		_ = os.Remove(s.sockPath)
		return nil, err
	}

	s.wg.Add(3)
	go s.acceptLoop()
	go s.flushLoop()
	go s.watchCtx()
	// 先把上一次没 ack 的条目排进发送队列（断线重连/重启后的补发，§7 的最后一条）。
	s.wakeFlush()
	return s, nil
}

// Path 是交给能力进程的 socket 路径（→ ANOTIFY_EMIT_SOCK）。
func (s *emitServer) Path() string { return s.sockPath }

// Token 是交给能力进程的鉴权令牌（→ ANOTIFY_TASK_TOKEN）。
func (s *emitServer) Token() string { return s.token }

// OutboxPath 是本地 outbox 文件路径（测试与排障用）。
func (s *emitServer) OutboxPath() string { return s.outboxPath }

// errEmitNoUpstream 表示「没有中枢可发」（本地模式 / 连接已断）。
var errEmitNoUpstream = errors.New("no upstream (agent 没有连中枢)")

// openOutbox 打开 outbox 并从中恢复 seq / 计数 / 未 ack 的条目。
func (s *emitServer) openOutbox() error {
	dir := filepath.Join(s.dataDir, emitOutboxDirName)
	if err := os.MkdirAll(dir, emitDirMode); err != nil {
		return fmt.Errorf("emit: 建 outbox 目录 %s: %w", dir, err)
	}
	s.outboxPath = filepath.Join(dir, s.taskID+emitOutboxSuffix)

	state, err := loadOutbox(s.outboxPath)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.outboxPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, emitFileMode)
	if err != nil {
		return fmt.Errorf("emit: 打开 outbox %s: %w", s.outboxPath, err)
	}
	s.outbox = f
	s.seq = state.maxSeq
	s.count = state.count
	s.pending = state.pending
	if state.maxSeq > 0 {
		s.log.Info("emit outbox 恢复",
			"event", "agent.emit.outbox.loaded",
			"task_id", s.taskID,
			"path", s.outboxPath,
			"max_seq", state.maxSeq,
			"emits", state.count,
			"pending", len(state.pending),
		)
	}
	return nil
}

// outboxState 是从 outbox 文件恢复出来的现场。
type outboxState struct {
	maxSeq  int64
	count   int
	pending map[int64]*pendingEmit
}

// loadOutbox 读一遍 outbox：算出「用过的最大 seq」「累计次数」「还没 ack 的条目」。
//
// 半行（崩溃时截断的写入）直接跳过：宁可少一条（中枢侧会表现为 seq 缺口，可见），
// 也不要因为一行坏数据让整个任务永远起不来。
func loadOutbox(path string) (outboxState, error) {
	st := outboxState{pending: make(map[int64]*pendingEmit)}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, fmt.Errorf("emit: 读 outbox %s: %w", path, err)
	}

	acked := make(map[int64]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec outboxRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Seq <= 0 {
			continue
		}
		if rec.Ack != nil {
			acked[rec.Seq] = rec.Ack.Error
			continue
		}
		st.count++
		if rec.Seq > st.maxSeq {
			st.maxSeq = rec.Seq
		}
		if _, ok := st.pending[rec.Seq]; ok {
			continue // 同一 seq 重复落盘不该发生；真发生了只留第一条（内容以首条为准）
		}
		st.pending[rec.Seq] = &pendingEmit{
			seq: rec.Seq,
			frame: &emitFrame{
				Type:   frameTypeEmit,
				TaskID: "", // 由服务端在发送前补（loadOutbox 不关心 task_id）
				Seq:    rec.Seq,
				Port:   rec.Port,
				Data:   rec.Data,
				Note:   rec.Note,
			},
			done: make(chan emitAckResult, 1),
		}
	}
	for seq, errText := range acked {
		if p, ok := st.pending[seq]; ok {
			if errText != "" {
				// 中枢明确拒收过：留着但永不重发（「不要无限重试」）。
				p.failed = errText
			} else {
				delete(st.pending, seq)
			}
		}
	}
	return st, nil
}

// Close 关掉 socket 并删掉文件（幂等）。
//
// **不 wg.Wait**：watchCtx 这个 goroutine 自己就会调 Close，在里面等自己会死锁。
// 后台 goroutine 各自看 closed / done 退出，不需要在这里汇合。
func (s *emitServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ln := s.ln
	outbox := s.outbox
	s.outbox = nil
	s.mu.Unlock()

	close(s.done)
	if ln != nil {
		_ = ln.Close()
	}
	if outbox != nil {
		_ = outbox.Sync()
		_ = outbox.Close()
	}
	// 必须删文件：同一个任务被重派时要能重新 Listen，而且「socket 不在了」正是
	// §8 用来拒绝晚到 emit 的信号。
	if err := os.Remove(s.sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("删除 emit socket 失败",
			"event", "agent.emit.socket.remove_failed", "path", s.sockPath, "error", err.Error())
	}
	s.log.Debug("emit socket closed", "event", "agent.emit.socket.closed",
		"task_id", s.taskID, "path", s.sockPath)
}

// isClosed 判定是否已经收口。
func (s *emitServer) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// EmitCount 是本任务累计 emit 次数（跨重启累计，见 count 字段）。
//
// 调用方（client.startTask）必须在 Close **之前**取：Close 之后这个服务不再接单，
// 计数自然不会再变，但「这次执行里到底 emit 了几次」只在那之前是可解释的——
// 落盘的 meta.json 要的正是这个数。nil 接收者返回 0（没起到 socket 的任务）。
func (s *emitServer) EmitCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// watchCtx 在任务 ctx 取消（cancel 帧 / 收尾超时）时立刻收掉 socket（§1 生命周期）。
func (s *emitServer) watchCtx() {
	defer s.wg.Done()
	select {
	case <-s.ctx.Done():
		s.log.Debug("任务 ctx 已取消：关闭 emit socket", "event", "agent.emit.socket.cancel",
			"task_id", s.taskID)
		s.Close()
	case <-s.done:
	}
}

// ---------- 连接处理 ----------

// acceptLoop 一连接一 goroutine：一个连接只处理一条请求（§1 的「每连接一行」）。
func (s *emitServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.isClosed() || errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("emit accept 失败", "event", "agent.emit.accept.failed",
				"task_id", s.taskID, "error", err.Error())
			return
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn 读一行请求、回一行响应、关连接。
func (s *emitServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(emitReadTimeout))
	line, err := readLimitedLine(bufio.NewReader(conn), emitMaxRequestBytes)
	if err != nil {
		s.reply(conn, emitResponse{Error: "读取请求失败：" + err.Error(), Rejected: emitRejectSchema})
		return
	}

	var req emitRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.reply(conn, emitResponse{Error: "请求不是合法 JSON：" + err.Error(), Rejected: emitRejectSchema})
		return
	}
	if req.Op != "" && req.Op != emitOpName {
		s.reply(conn, emitResponse{
			Error:    fmt.Sprintf("未知 op %q（只支持 %q）", req.Op, emitOpName),
			Rejected: emitRejectSchema,
		})
		return
	}
	s.reply(conn, s.emit(req, strings.TrimSpace(req.Wait) == emitWaitRoutedName))
}

// reply 回一行 JSON（响应写不出去只可能是对端已经走了：不值得重试）。
func (s *emitServer) reply(conn net.Conn, resp emitResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(emitReplyTimeout))
	_, _ = conn.Write(append(data, '\n'))
}

// targetNeeds 取出 --to 点名的那一个候选的 needs（没有 needs 返回空切片）。
//
// 匹配口径与中枢的 narrowCandidatesTo 逐字一致：`name` 或 `name@version`，
// 名字相同时再比版本（ver == 0 表示任意版本）。两边判据不同会得到「agent 放行、
// 中枢说不是候选」这种两边都看不出自己错在哪的失败。
//
// 第二个返回值是**匹配到了没有**：没匹配到（老中枢不带 targets、或脚本直连
// socket 绕过 CLI 校验）时调用方退回端口并集，行为与改之前一致。
func targetNeeds(targets []ws.ExpectTarget, to string) (json.RawMessage, bool) {
	name, ver := to, 0
	if at := strings.LastIndex(to, "@"); at > 0 {
		name = to[:at]
		if n, err := strconv.Atoi(to[at+1:]); err == nil {
			ver = n
		}
	}
	for _, tgt := range targets {
		if tgt.Capability != name {
			continue
		}
		if ver != 0 && tgt.Version != ver {
			continue
		}
		return tgt.Needs, true
	}
	return nil, false
}

// readLimitedLine 读一行（以 \n 结尾），超过 limit 就报错而不是继续吃内存。
func readLimitedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > limit {
			return nil, fmt.Errorf("请求超过 %d 字节", limit)
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(buf) > 0 {
			break // 最后一行没带换行也算（宽容一点，不为此丢一条产出）
		}
		if errors.Is(err, io.EOF) {
			return nil, errors.New("对端没有发任何数据")
		}
		return nil, err
	}
	return bytes.TrimSpace(buf), nil
}

// ---------- 核心：一次 emit ----------

// emit 是校验 + 落盘 + 排队发送的全部逻辑，顺序严格照 §6。
func (s *emitServer) emit(req emitRequest, waitRouted bool) emitResponse {
	// 1) token：一次性任务令牌（constant-time 比较：令牌是秘密，别用 == 泄露时序）。
	//
	// rejected 用 §1 枚举里那个更具体的 token（§6 第 1 条的文字写的是 task，但
	// 退出码要求是 6，与第 2 条的 5 不同；若这里也回 task，任何按 rejected→退出码
	// 映射的调用方都会把「令牌无效」读成「任务已非运行态」而停掉本该重试的任务）。
	// 退出码仍然严格照 §6：token → 6，任务态 → 5。
	if !s.checkToken(req.Token) {
		return rejectEmit(req.Seq, emitRejectToken,
			"token 不匹配：本次任务的令牌对不上（能力进程应当原样使用 ANOTIFY_TASK_TOKEN）")
	}
	// 2) 任务仍在运行态（未 cancel / 租约未过期 / 未终结）。
	if s.ctx.Err() != nil || s.isClosed() {
		return rejectEmit(req.Seq, emitRejectTask,
			"任务已非运行态（已 cancel / 租约过期 / 已终结）：停止执行，别再 emit")
	}
	// 3) port ∈ 本次 assign.ports（I2 的封闭集合，提前到 emit 时判）。
	port := strings.TrimSpace(req.Port)
	if port == "" {
		return rejectEmit(req.Seq, emitRejectPort, "port 不能为空")
	}
	if _, ok := s.ports[port]; !ok {
		return rejectEmit(req.Seq, emitRejectPort, fmt.Sprintf(
			"port %q 不在本次任务的 ports %v 里（只能往这些出口 emit）", port, s.portList()))
	}
	// 4) data 是 JSON 对象（缺省 = {}）。
	data, dataJSON, err := normalizeEmitData(req.Data)
	if err != nil {
		return rejectEmit(req.Seq, emitRejectSchema, err.Error())
	}
	// 5) data ⊨ 这次**要去的那一个**下游的要求（没有下游 → 只做结构校验）。
	//
	// --to 点名了候选 → 用那一个候选的 needs；没点名 → 用端口的并集。
	//
	// 为什么不能一律用并集：同一端口的候选各有各的必填字段（haha 的 out 接
	// script.echo{message} 和 waoo{waoo}），并集会把「别人要的字段」也算成
	// 本次必填 —— 于是 `--to script.echo` 带 {"message":…} 被拒 exit 4，
	// 同一个端口的候选**谁都发不出去**（开发环境 trace_1793986152223364273
	// 就是这么挂的）。并集只在「谁去由中枢轮盘定」时有意义：那时确实得让每个
	// 候选都能吃下这份 data。
	if exp, ok := s.expects[port]; ok {
		// who 是给人看的（拒绝原因里说「谁的要求」），where 是 schema 的路径（坏 schema 时指出哪一份）。
		raw := exp.Schema
		who, where := fmt.Sprintf("expects[%s]", port), fmt.Sprintf("expects[%s]", port)
		if to := strings.TrimSpace(req.To); to != "" {
			if needs, ok := targetNeeds(exp.Targets, to); ok {
				raw = needs
				who = fmt.Sprintf("expects[%s] 的候选 %s", port, to)
				where = fmt.Sprintf("expects[%s].targets[%s]", port, to)
			}
		}
		if len(raw) > 0 {
			sch, serr := schema.FromRaw(raw)
			if serr != nil {
				// 坏 schema 不能放行（「拿坏契约放行」是最坏的降级方式）：拒绝并说清是谁的锅。
				return rejectEmit(req.Seq, emitRejectSchema, fmt.Sprintf(
					"中枢下发的 %s.schema 不可用：%v", where, serr))
			}
			if msg := schema.Value(sch, data, "data"); msg != "" {
				return rejectEmit(req.Seq, emitRejectSchema, fmt.Sprintf(
					"data 不满足 %s：%s", who, msg))
			}
		}
	}
	// 6) 体积 ≤ 64 KiB。
	if len(dataJSON) > emitMaxDataBytes {
		return rejectEmit(req.Seq, emitRejectSize, fmt.Sprintf(
			"data 体积 %d 字节超过上限 %d 字节（改用引用/分片，别把整份内容塞进 emit）",
			len(dataJSON), emitMaxDataBytes))
	}

	note := strings.TrimSpace(req.Note)
	if len(note) > emitNoteMaxBytes {
		note = oneLine(note, emitNoteMaxBytes)
	}

	// 7) 累计次数 < 1000，且分配 seq —— 三件事必须在同一把锁里做完，
	//    否则两个并发 emit 会拿到同一个 seq（中枢报 "seq reused"）。
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return rejectEmit(req.Seq, emitRejectTask, "任务已终结：emit socket 已关闭")
	}
	if s.count >= emitMaxCount {
		count := s.count
		s.mu.Unlock()
		return rejectEmit(req.Seq, emitRejectRate, fmt.Sprintf(
			"本任务已 emit %d 次，达到上限 %d 次", count, emitMaxCount))
	}
	seq := s.seq + 1
	frame := &emitFrame{
		Type:   frameTypeEmit,
		TaskID: s.taskID,
		Seq:    seq,
		Port:   port,
		// To 是脚本点名的那一个下游（`--to`）。不带下去的话，"钉住某个候选"
		// 就变成了静默的"还是按轮盘随便挑一个"——最坏的那种失败。
		To:   strings.TrimSpace(req.To),
		Data: dataJSON,
		Note: note,
	}
	// **先落盘再 ack**（两级 ack 的第一级）：fsync 成功之后这条 emit 就丢不了了，
	// 崩溃重启会从 outbox 里补发。
	if err := s.appendOutboxLocked(seq, port, dataJSON, note); err != nil {
		s.mu.Unlock()
		return emitResponse{OK: false, Seq: seq, Error: fmt.Sprintf("本地 outbox 落盘失败：%v", err)}
	}
	s.seq = seq
	s.count++
	p := &pendingEmit{seq: seq, frame: frame, done: make(chan emitAckResult, 1)}
	s.pending[seq] = p
	s.mu.Unlock()

	s.wakeFlush()

	if !waitRouted {
		return emitResponse{OK: true, Seq: seq, Acked: emitAckedDurable}
	}

	// --wait-routed：等中枢 emit_ack（一次 RTT）。超时/关闭都**不撒谎**：
	// acked 仍是 durable（它确实已经落盘），错误里说清是「没等到派发确认」。
	select {
	case res := <-p.done:
		if res.err != "" {
			return emitResponse{Seq: seq, Acked: emitAckedDurable,
				Error: "中枢拒收：" + res.err + "（同一 seq 不会再自动重发）"}
		}
		return emitResponse{OK: true, Seq: seq, Acked: emitAckedRouted}
	case <-time.After(emitWaitRoutedTimeout):
		return emitResponse{Seq: seq, Acked: emitAckedDurable, Error: fmt.Sprintf(
			"等待中枢 emit_ack 超时（%s）：emit 已落盘，agent 会在重连后按 seq 补发", emitWaitRoutedTimeout)}
	case <-s.ctx.Done():
		return emitResponse{Seq: seq, Acked: emitAckedDurable,
			Error: "任务已结束：emit 已落盘但没等到中枢确认"}
	case <-s.done:
		return emitResponse{Seq: seq, Acked: emitAckedDurable,
			Error: "agent 正在关闭：emit 已落盘但没等到中枢确认"}
	}
}

// checkToken 比对令牌。任务没有下发令牌时（旧中枢）不鉴权，但要记一条 warn（见 startEmitServer）。
func (s *emitServer) checkToken(got string) bool {
	if s.token == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// portList 返回白名单的稳定快照（错误消息用：顺序稳定，测试与排障都能直接看）。
func (s *emitServer) portList() []string {
	ports := make([]string, 0, len(s.ports))
	for p := range s.ports {
		ports = append(ports, p)
	}
	sort.Strings(ports)
	return ports
}

// rejectEmit 造一条拒绝响应（acked 留空，§1 的示例如此）。
func rejectEmit(seq int64, code, msg string) emitResponse {
	return emitResponse{OK: false, Seq: seq, Error: msg, Rejected: code}
}

// normalizeEmitData 把 data 归一成「JSON 对象 + 紧凑字节」。
//
// 缺省与 null 都当 {}（§3.1：data 可缺省 = {}）。非对象一律拒绝：data **直接就是
// 下游的 params**，数组/标量既无法 ⊨ schema，也没法拼成参数。
func normalizeEmitData(raw json.RawMessage) (map[string]any, json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]any{}, json.RawMessage("{}"), nil
	}
	var data map[string]any
	if err := json.Unmarshal(trimmed, &data); err != nil {
		return nil, nil, fmt.Errorf("data 必须是 JSON 对象（实际 %s）：%v",
			jsonTypeName(trimmed), err)
	}
	compact, err := json.Marshal(data)
	if err != nil {
		return nil, nil, fmt.Errorf("data 不是可编码的 JSON：%w", err)
	}
	return data, compact, nil
}

// jsonTypeName 给原始 JSON 一个人类可读的类型名（错误消息用）。
func jsonTypeName(raw []byte) string {
	switch {
	case len(raw) == 0:
		return "空"
	case raw[0] == '[':
		return "array"
	case raw[0] == '"':
		return "string"
	case raw[0] == '{':
		return "object"
	case string(raw) == "true" || string(raw) == "false":
		return "boolean"
	case string(raw) == "null":
		return "null"
	default:
		return "number"
	}
}

// appendOutboxLocked 追加一条 emit 并 fsync（调用方必须持有 s.mu）。
func (s *emitServer) appendOutboxLocked(seq int64, port string, data json.RawMessage, note string) error {
	if s.outbox == nil {
		return errors.New("outbox 已关闭")
	}
	line, err := json.Marshal(outboxRecord{
		Seq:  seq,
		Port: port,
		Data: data,
		Note: note,
		TS:   time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("编码 outbox 记录: %w", err)
	}
	if _, err := s.outbox.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("写 outbox %s: %w", s.outboxPath, err)
	}
	// fsync 是「durable」这个词的全部含义：不做它，回复 durable 就是撒谎。
	if err := s.outbox.Sync(); err != nil {
		return fmt.Errorf("fsync outbox %s: %w", s.outboxPath, err)
	}
	return nil
}

// ---------- 中枢答复 ----------

// acknowledge 处理一帧 emit_ack（由 client 的读循环调用）。
func (s *emitServer) acknowledge(f *emitAckFrame) {
	if f == nil {
		return
	}
	s.mu.Lock()
	p := s.pending[f.Seq]
	if p == nil {
		s.mu.Unlock()
		// 常见且无害：重复的 ack，或这条早就被确认过。
		s.log.Debug("emit_ack 对应不到未确认条目",
			"event", "agent.emit_ack.unknown", "task_id", f.TaskID, "seq", f.Seq)
		return
	}
	// 缺口：中枢收到 seq=n 而库里没有 1..n-1 → 它**什么都没写**（事务回滚，§4）。
	// 所以 n 本身和更小的未确认条目都要重发：漏掉的只可能是在上一轮连接里丢了。
	// 这是 §4 唯一允许的补救：不猜、不跳过，把缺的补上。
	gap := f.Error != "" && strings.Contains(strings.ToLower(f.Error), "gap")
	terminal := f.Error != "" && !gap
	switch {
	case terminal:
		// 中枢明确拒收（内容冲突 / 任务非运行态）：从 pending 里摘掉 = **永不再发**
		// （§2 不要无限重试）。重启后也不会重发——appendAckLocked 把这条 error
		// 也落进了 outbox，replay 时会认出「这条被拒过」。
		delete(s.pending, f.Seq)
	case gap:
		p.sent = false
		for seq, q := range s.pending {
			if seq < f.Seq {
				q.sent = false
			}
		}
	default:
		delete(s.pending, f.Seq)
	}
	s.appendAckLocked(f)
	s.mu.Unlock()

	// 两类情况都要叫醒 flusher：成功之后接着发后面的 seq；缺口之后把重排的条目补上。
	s.wakeFlush()

	if gap {
		// 缺口不是「这条被拒」，而是「前面缺了」：让 --wait-routed 继续等补发后的确认。
		s.log.Warn("中枢报 seq 缺口，补齐中",
			"event", "agent.emit.gap", "task_id", f.TaskID, "seq", f.Seq, "error", f.Error)
		return
	}
	if terminal {
		s.log.Warn("emit 被中枢拒收",
			"event", "agent.emit.rejected",
			"task_id", f.TaskID, "seq", f.Seq, "error", f.Error)
		select {
		case p.done <- emitAckResult{err: f.Error}:
		default:
		}
		return
	}
	s.log.Debug("emit 已由中枢确认",
		"event", "agent.emit.acked",
		"task_id", f.TaskID, "seq", f.Seq, "routed", f.Routed, "pruned", f.Pruned)
	select {
	case p.done <- emitAckResult{routed: f.Routed, pruned: f.Pruned}:
	default:
	}
}

// appendAckLocked 把中枢答复追加进 outbox（调用方持有 s.mu）。
//
// 这里**不 fsync**：丢了这条记录最多导致重启后重发一次同一 seq，而中枢对
// (task_id, seq) 幂等——重发的代价远小于每条 emit 多一次 fsync。
// 但它必须落盘：这是「被拒的条目不再重发」的唯一依据。
func (s *emitServer) appendAckLocked(f *emitAckFrame) {
	if s.outbox == nil {
		return
	}
	line, err := json.Marshal(outboxRecord{
		Seq: f.Seq,
		Ack: &outboxAck{Routed: f.Routed, Pruned: f.Pruned, Error: f.Error},
	})
	if err != nil {
		s.log.Warn("编码 outbox ack 记录失败", "event", "agent.emit.outbox.encode_failed",
			"task_id", s.taskID, "seq", f.Seq, "error", err.Error())
		return
	}
	if _, err := s.outbox.Write(append(line, '\n')); err != nil {
		s.log.Warn("写 outbox ack 记录失败", "event", "agent.emit.outbox.write_failed",
			"task_id", s.taskID, "seq", f.Seq, "error", err.Error())
	}
}

// ---------- 发送（补发） ----------

// wakeFlush 叫醒 flusher（容量 1，多次叫醒合并）。
func (s *emitServer) wakeFlush() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// flushLoop 是唯一的发送者：只有它能给中枢发 emit，于是「谁先谁后」是确定的。
func (s *emitServer) flushLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.done:
			return
		case <-s.wake:
		}
		if err := s.flushPending(); err != nil {
			// 发不出去（连接断 / 在关闭）：这一批剩下的先不发，但**不退出循环**——
			// 条目都还在 outbox 里（sent=false），下一次叫醒（新的 emit / 中枢的
			// 缺口答复）会重试。注意这是**有界**的：叫醒只来自这两个事件，
			// 所以不会变成忙等式无限重试。
			s.log.Warn("emit 发往中枢失败（留在 outbox，等下次叫醒或重启补发）",
				"event", "agent.emit.send.failed", "task_id", s.taskID, "error", err.Error())
		}
	}
}

// flushPending 按 **seq 升序** 把还没发过的条目送给中枢。
//
// 必须升序：中枢对 seq 缺口回 error（§4），而缺口只能靠「把缺的那些补上」修好；
// 乱序发送只会反复制造缺口。
func (s *emitServer) flushPending() error {
	s.mu.Lock()
	seqs := make([]int64, 0, len(s.pending))
	for seq, p := range s.pending {
		if p.sent || p.failed != "" {
			continue
		}
		seqs = append(seqs, seq)
	}
	s.mu.Unlock()
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	for _, seq := range seqs {
		s.mu.Lock()
		p := s.pending[seq]
		if p == nil || p.sent || p.failed != "" || s.closed {
			s.mu.Unlock()
			continue
		}
		// **先**标已发，再发：中枢的答复是另一个 goroutine 在读的，如果等发完再标，
		// 一条「seq 缺口」答复可能先把它重排成未发、又被这里盖回已发——那条就永远
		// 补不上了。发失败时再退回未发（见下面）。
		p.sent = true
		frame := *p.frame
		frame.TaskID = s.taskID
		s.mu.Unlock()

		if err := s.send(&frame); err != nil {
			s.mu.Lock()
			if q := s.pending[seq]; q == p {
				q.sent = false
			}
			s.mu.Unlock()
			return fmt.Errorf("seq %d: %w", seq, err)
		}
	}
	return nil
}
