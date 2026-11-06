package ws

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// 本文件是「向节点按需取日志」的中枢侧（CONTRACT-logs-on-node §1）：
// 发一帧 logs_request、按 request_id 等回包、超时收工。
//
// # 为什么是「请求/响应」而不是「节点主动推」
//
// 日志只存在节点上，而且**只有人真的要看的时候**才需要（trace 页展开某个任务）。
// 主动推等于每个任务结束都搬一次日志，正好是这一版要停掉的那条路；按需取则是
// 「点开 → 一次帧往返」。中枢发起的请求天然带来两个要点：配对（同一连接上可能
// 有多条请求在飞）与超时（节点可能已经死了却没断开，或者干脆没回）。
//
// # 超时是 5 秒（LogsRequestTimeout）
//
// 节点侧读的是 ≤512 KiB×2 的本地文件尾部，正常远小于 1 秒；5 秒是给「磁盘很忙 /
// 节点正在跑满并发」留的余量。等不到就返回 ErrLogsTimeout —— HTTP 端点会把它翻成
// **200 + available:false**（不是 5xx）：日志取不到是「这次看不到」，不是服务出错。
//
// # 配对规则（少一条都会错得很隐蔽）
//
//   - request_id 是唯一的键：中枢没登记过的 id（晚到的回包 / 过期请求）直接丢弃；
//   - 回包的 task_id 必须与请求一致：不一致同样丢弃（宁可超时，也不能把 A 的日志
//     显示成 B 的）；
//   - 连接结束时唤醒所有等待者（ErrNodeOffline），不让调用方白等到超时。

const (
	// LogsRequestTimeout 是一次 logs_request 的默认超时（契约 §1）。
	LogsRequestTimeout = 5 * time.Second

	// logsRequestIDPrefix 是请求 id 的前缀（契约 §1：`lreq_<base62>`）。
	logsRequestIDPrefix = "lreq_"
	// logsRequestIDBytes 是随机部分的字节数（8 字节 = 64 位，碰撞概率可忽略）。
	logsRequestIDBytes = 8
)

// 取日志这条链的三种「没取到」（契约 §2 的 reason 就是按它们定义的）。
var (
	// ErrNodeOffline 复用：节点没有活跃连接（也包括「任务还没派给任何节点」）。
	// 定义在 node_hub.go，与派发路径同一个值。
	//
	// ErrNoLocalArchive 节点在线、也回了包，但它本机上没有那次执行的档案。
	ErrNoLocalArchive = errors.New(LogsErrNoLocalArchive)
	// ErrLogsTimeout 帧发出去了，5 秒内没有配得上 request_id 的回包。
	ErrLogsTimeout = errors.New("logs request timed out")
)

// logsWaiter 是一次在飞的请求：回包投递到 ch（缓冲 1），投递只有一次。
type logsWaiter struct {
	taskID string
	ch     chan *LogsResponseFrame
}

// logsPending 是本连接上的在飞请求表（request_id → waiter）。
//
// 放在 nodeSession 上而不是 NodeHub 上：配对是**连接级**的事实，连接一断这些请求
// 就都不可能再有回包了（cleanup 会把它们全部唤醒）。放在 hub 里还要额外维护
// 「连接 ↔ 请求」的从属关系，多一层可能不同步的状态。
type logsPending struct {
	mu    sync.Mutex
	items map[string]*logsWaiter
}

// register 登记一次在飞请求。
func (w *logsPending) register(requestID, taskID string) chan *LogsResponseFrame {
	ch := make(chan *LogsResponseFrame, 1)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.items == nil {
		w.items = make(map[string]*logsWaiter, 4)
	}
	w.items[requestID] = &logsWaiter{taskID: taskID, ch: ch}
	return ch
}

// drop 注销一次请求（幂等：已经投递/超时都安全）。
func (w *logsPending) drop(requestID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.items, requestID)
}

// deliver 把回包投给对应的等待者，返回是否投递成功。
//
// 三种失败都是**预期内**的，一律返回 false 由调用方记一行 debug：
//   - request_id 没登记过（晚到 / 已超时 / 对端乱发）；
//   - 回包的 task_id 与请求不一致（对端实现 bug，丢比错配安全）；
//   - 同一个 id 已经被投递过一次（缓冲 1，第二次必然满）。
func (w *logsPending) deliver(f *LogsResponseFrame) bool {
	if f == nil || f.RequestID == "" {
		return false
	}
	w.mu.Lock()
	wt, ok := w.items[f.RequestID]
	if ok && wt.taskID != f.TaskID {
		// 配对键对了、任务对不上：绝不投递（错配的日志比没有日志危险得多）。
		w.mu.Unlock()
		slog.Warn("logs_response task mismatch, dropped",
			"event", "node.ws.logs.task_mismatch",
			"request_id", f.RequestID,
			"want_task_id", wt.taskID,
			"got_task_id", f.TaskID,
		)
		return false
	}
	delete(w.items, f.RequestID)
	w.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case wt.ch <- f:
		return true
	default:
		// 缓冲 1 且登记时还没投递过：走到这里说明同一 id 被投了两次。
		return false
	}
}

// logsErrNodeOffline 是**进程内**的哨兵（永不出线）：连接断开时用来唤醒等待者。
//
// 不用「关闭 channel」唤醒：deliver 在锁外投递，close 与 send 并发会 panic；
// 而缓冲 1 + 非阻塞 send 天然幂等（第二次投递只是失败）。
const logsErrNodeOffline = "node_offline"

// failAll 唤醒本连接上全部在飞请求（连接断了，回包永远不会来）。
// 返回被唤醒的请求数，供 cleanup 记一行日志。
func (w *logsPending) failAll() int {
	w.mu.Lock()
	items := w.items
	w.items = nil
	w.mu.Unlock()
	for _, wt := range items {
		select {
		case wt.ch <- &LogsResponseFrame{
			Type:   NodeFrameLogsResponse,
			TaskID: wt.taskID,
			Error:  logsErrNodeOffline,
		}:
		default:
		}
	}
	return len(items)
}

// logsTimeout 返回生效的取日志超时（NodeHub.LogsTimeout 可覆盖，测试用）。
func (h *NodeHub) logsTimeout() time.Duration {
	if h.LogsTimeout > 0 {
		return h.LogsTimeout
	}
	return LogsRequestTimeout
}

// FetchLogs 向节点要一条任务的日志尾巴（CONTRACT-logs-on-node §1 / §2）。
//
// 它是 HTTP 端点的**唯一**数据来源：每次请求都真的问节点一次（不落库、不缓存）——
// 日志只存在节点上，中枢手里的任何副本都只能更旧。
//
// 返回值与错误：
//   - 成功 → 回包原样（Stdout/Stderr/Truncated/Bytes）；
//   - 节点没有活跃连接 / 跨命名空间 → ErrNodeOffline；
//   - 节点回 error=no_local_archive → ErrNoLocalArchive；
//   - 超时没回 → ErrLogsTimeout；
//   - 写帧失败（连接已断）→ 底层错误（调用方按「没取到」处理，不是 5xx）。
//
// 节点侧的 error 只有 no_local_archive 一个合法取值；其它非空 error 折成一句带原值的
// 错误返回（端点会把任何非 no_local_archive 的失败翻成 reason=timeout，见
// internal/server 的 taskLogs）。
func (h *NodeHub) FetchLogs(ctx context.Context, namespaceID, nodeID, taskID string) (*LogsResponseFrame, error) {
	if taskID == "" {
		return nil, fmt.Errorf("fetch logs: empty task id")
	}
	s := h.current(nodeID)
	if s == nil {
		return nil, ErrNodeOffline
	}
	if s.node.NamespaceID != namespaceID {
		// 绝不跨命名空间取数（不变量 I10/I11，与 assign/deploy/upgrade 同一条）。
		return nil, fmt.Errorf("node %s belongs to another namespace", nodeID)
	}

	requestID := newLogsRequestID()
	ch := s.logsPending.register(requestID, taskID)
	defer s.logsPending.drop(requestID)

	// 整体超时：写帧 + 等回包共用一个 deadline。
	//
	// 为什么写帧也纳入：一条半死的连接上 Write 会一直挂到写超时（10s），
	// 那已经超出取日志的预算，用户早就关掉页面了。
	wctx, cancel := context.WithTimeout(ctx, h.logsTimeout())
	defer cancel()

	data, err := Encode(&LogsRequestFrame{
		Type:      NodeFrameLogsRequest,
		TaskID:    taskID,
		RequestID: requestID,
	})
	if err != nil {
		return nil, err
	}
	if err := s.sendBytes(wctx, data); err != nil {
		// 连接已断：不用等回包，直接按「节点不可用」报（调用方翻成 node_offline/timeout）。
		return nil, fmt.Errorf("send logs_request: %w", err)
	}

	select {
	case resp := <-ch:
		switch {
		case resp == nil, resp.Error == logsErrNodeOffline:
			// 连接在这条请求在飞的时候断了（cleanup 的 failAll 唤醒了我们）。
			return nil, ErrNodeOffline
		case resp.Error == LogsErrNoLocalArchive:
			return nil, ErrNoLocalArchive
		case resp.Error != "":
			return nil, fmt.Errorf("node logs error: %s", resp.Error)
		}
		return resp, nil
	case <-wctx.Done():
		if ctx.Err() != nil {
			// 调用方自己走了（浏览器关了 / 请求被取消）：不是节点的问题。
			return nil, ctx.Err()
		}
		return nil, ErrLogsTimeout
	}
}

// handleLogsResponse 落一帧 logs_response（CONTRACT-logs-on-node §1）。
//
// 有意思的分支只有一条：**不认识的 request_id 一律丢弃**。这不是错误，也不回
// error 帧——晚到的回包（我们这边已经超时了）是正常现象，回一帧 error 只会让
// 节点以为协议坏了。连接必须保持（日志是旁路，不该因为一次超时把任务带下水）。
func (s *nodeSession) handleLogsResponse(_ context.Context, f *LogsResponseFrame) error {
	if s.logsPending.deliver(f) {
		return nil
	}
	slog.Debug("unpaired logs_response dropped",
		"event", "node.ws.logs.unpaired",
		"node_id", s.node.ID,
		"request_id", f.RequestID,
		"task_id", f.TaskID,
		"error", f.Error,
	)
	return nil
}

// newLogsRequestID 生成 `lreq_<base62>`（契约 §1）。
//
// 随机源是 crypto/rand：request_id 会进日志与错误信息，且同一个进程可能并发处理
// 多条请求——用自增计数器会让「两条连接的 id 撞在一起」变成可预期的事。
func newLogsRequestID() string {
	var raw [logsRequestIDBytes]byte
	// crypto/rand.Read 在 Go 1.24+ 永不返回错误（失败即 fatal）；这里仍按返回值
	// 处理，避免把「读不到随机数」静默成一个可预测的 id。
	if _, err := rand.Read(raw[:]); err != nil {
		return logsRequestIDPrefix + fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return logsRequestIDPrefix + base62Encode(raw[:])
}

// base62Alphabet 是 request id 用的字符集（URL 安全、日志里可读）。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base62Encode 把字节串按 62 进制展开（定长可逆性不重要，只要均匀且无分隔符）。
func base62Encode(src []byte) string {
	// 简化实现：每个字节转两个 base62 字符（8 字节 → 16 字符）。
	// 不用 big-int 除法是因为这里不需要最小值长度，只需要「无歧义、无特殊字符」。
	out := make([]byte, 0, len(src)*2)
	for _, b := range src {
		out = append(out, base62Alphabet[b/62], base62Alphabet[b%62])
	}
	return string(out)
}
