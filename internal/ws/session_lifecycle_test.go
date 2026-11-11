package ws

// 本文件钉住**会话写生命周期**的两条边界（#340 第四轮复审 m-1 / m-2）。
//
//  1. 调用方的 ctx 只结束**它自己的等待**，不拥有拆掉节点会话的权力：
//     session.go 的写超时上下文用 context.WithoutCancel，不继承调用方可取消性。
//     这条修复原先只有 internal/server 里**统计性**的护栏（空闲机 -count=200 不复现、
//     带负载约 1%/次）。这里的用例把它变成**确定性**的：真实 WS 连接 + 客户端故意不读
//     造成的阻塞写，一步一步只在事件（写计数 / ctx.Done）上推进，没有 sleep 断言。
//  2. 「进入即取消」不得被当成写失败：Send 返回可判别的 ErrSendCanceled，
//     detachSessionOnWriteFailure 据此放过会话；真实写失败仍按原逻辑关会话。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
)

const (
	// bigFrameBytes 是阻塞写的载荷大小：必须明显大于「客户端不读时内核能缓冲的量」
	// （loopback 的 sndbuf + rcvbuf 合计约 10MiB 量级），否则写会在调用方 deadline
	// 之前就完成，用例前提不成立——下面「Send 不能先返回」的前提断言会直接报出来，
	// 不会静默通过。
	bigFrameBytes = 32 << 20
	// callerDeadline 是调用方 ctx 的到期时间（真实触发路径是 HTTP 请求的 r.Context()
	// 到期、或客户端读到帧后立刻断开）。
	//
	// 它的计时起点锚在「服务端已经写出这条帧的第一笔」这个**事件**上（见下面的 arm 顺序），
	// 而不是锚在 Send 调用上：后者还要先跑 32MiB 的 json 编码，deadline 会被编码吃光，
	// 取消就落到写开始之前了（那是另一条路径，不是本用例要复现的窗口）。
	callerDeadline = 40 * time.Millisecond
	// sessionSettleLimit 等「会话退干净（detach 的库写已落）」的上限：单条会话退出是
	// 毫秒级，超时就是确证失败，不是 flake。
	sessionSettleLimit = 5 * time.Second
	// writeStartLimit 等「服务端开始写这条大帧」的上限。比其它界限宽得多，因为它后面
	// 还带着 Send 内部对 32MiB 帧的 json 编码（高负载下可能到秒级）——这只是一个上界，
	// 不是等待时长。
	writeStartLimit = 30 * time.Second
)

// bigLogsFrame 造一条 > 32MiB 的 logs_response。用共享协议的**真实帧类型**
// （不手写 wire 字节）；载荷是重复字符，但 hub 的 websocket.Accept 没有开压缩
// （AcceptOptions.CompressionMode 默认 Disabled），所以线上就是实打实的 32MiB。
func bigLogsFrame() *protocol.LogsResponseFrame {
	lines := []protocol.LogLine{{
		Sequence: 1,
		Stream:   protocol.LogStreamStdout,
		Text:     strings.Repeat("x", bigFrameBytes),
		At:       time.Now().UTC().Format(time.RFC3339Nano),
	}}
	return &protocol.LogsResponseFrame{
		Type:      protocol.FrameLogsResponse,
		TaskID:    "task-1",
		RequestID: "lreq-1",
		Attempt:   1,
		Lines:     &lines,
	}
}

// ---------- 服务端写字节计数：确定性前提的观测点 ----------
//
// 要断言「调用方取消时写还在飞行中」，就必须**看见**服务端真的开始写这条帧，而不是
// 靠「Send 应该已经在写了」这种猜测。hub 与传输无关，可以在它前面再挂一层包了计数的
// server（newEnv 自带的那个 server 本用例不用，它的 cleanup 照常关）。
type writeCounter struct {
	mu    sync.Mutex
	total int64
	armed chan struct{}
}

// onWrite 记一笔服务端写；武装之后的第一笔非空写关闭 armed（只关一次）。
func (c *writeCounter) onWrite(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(n)
	if c.armed != nil {
		close(c.armed)
		c.armed = nil
	}
}

// arm 把「下一笔服务端写」变成一个事件。握手与 welcome 的字节都发生在武装之前，
// 所以武装后的第一笔写只可能来自用例要发的那条大帧。
func (c *writeCounter) arm() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = make(chan struct{})
	return c.armed
}

// written 报告累计已写出的字节数（失败诊断用）。
func (c *writeCounter) written() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// countingConn 在**内核收下**字节之后立刻记账（返回的 n 是真实写出的字节数）。
type countingConn struct {
	net.Conn
	counter *writeCounter
}

func (c countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.counter.onWrite(n)
	}
	return n, err
}

type countingListener struct {
	net.Listener
	counter *writeCounter
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: conn, counter: l.counter}, nil
}

// newCountingServer 在同一个 hub 前面再挂一个带写计数的 server，并把 fixture 的
// `e.server` 指过去（`e.dial`/`e.dialNode` 都读它）。`newEnv` 自带的那个 server 本用例
// 不用，它的 cleanup（注册时就绑定了旧值）照常关。
//
// 不去改 fixture_test.go 的 newEnv：本轮改动边界不动共享夹具。
func newCountingServer(t *testing.T, e *env) *writeCounter {
	t.Helper()
	counter := &writeCounter{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(e.hub.ServeHTTP))
	srv.Listener = countingListener{Listener: srv.Listener, counter: counter}
	srv.Start()
	t.Cleanup(srv.Close)
	e.server = srv
	return counter
}

// TestSessionSendWriteSurvivesCallerCancel 是 #340 m-1 的确定性回归。
//
// 复现的正是 context.WithoutCancel 存在的理由：coder/websocket 的写超时是
// `context.AfterFunc(ctx, …c.close())` 实现的，而那个 AfterFunc 要到 writeFrame 的
// defer 里才解除。所以只要写 ctx 继承调用方的可取消性，**帧已经写到 socket 之后、
// defer 之前**的调用方取消就会关掉整条节点会话（真实表现：节点在线却没回包 →
// 503 node_offline，而不是调用方自己的 504）。
//
// 时序全部由事件驱动（无 sleep 断言）：
//
//	服务的计数事件 → 证明写已经开始（setupWriteTimeout 已注册）
//	ctx.Done()     → 调用方期限在**写阻塞中**到期（这就是那个窗口）
//	客户端读完整帧 → 写得以完成（客户端不读时它永远完不成）
//
// 断言：帧写完后会话仍在线、后续帧照常收发、调用方返回后不拆会话。
func TestSessionSendWriteSurvivesCallerCancel(t *testing.T) {
	e := newEnv(t)
	// 写自身的上限不该成为本用例的边界（默认 2s 会让「收干净 32MiB」与它竞争）。
	// 此刻还没有任何会话，而 writeTimeout 只在 Session.Send 里读，赋值无并发读者。
	e.hub.writeTimeout = 60 * time.Second
	counter := newCountingServer(t, e)

	conn := e.dial(t)
	// Dial 的默认读上限是 32KiB：32MiB 的帧必须自己放开读上限。
	conn.SetReadLimit(2 * bigFrameBytes)
	sess := e.hub.session(e.node.ID)
	if sess == nil {
		t.Fatal("节点未注册到 hub")
	}

	// 编码一次：① 预热（把 32MiB 的 json 编码成本从后面的时序里摘出去）；② 拿到这条
	// 帧应有的字节数，用来断言客户端收到的是**完整**一帧。
	frame := bigLogsFrame()
	want, err := protocol.Encode(frame)
	if err != nil {
		t.Fatalf("编码 32MiB 帧: %v", err)
	}
	if len(want) <= bigFrameBytes {
		t.Fatalf("前提不成立：造的帧只有 %d 字节（不够堵住 socket 缓冲）", len(want))
	}

	started := counter.arm()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan error, 1)
	go func() { sent <- sess.Send(ctx, frame) }()

	select {
	case <-started:
	case err := <-sent:
		t.Fatalf("Send 在服务端开始写之前就返回了（写路径没走到 socket）: %v", err)
	case <-time.After(writeStartLimit):
		t.Fatalf("服务端在 %s 内没有开始写这条 32MiB 帧（前提不成立；已记账 %d 字节）",
			writeStartLimit, counter.written())
	}

	// 写已经进了 socket（客户端一字节未读 ⇒ 它现在必然阻塞在剩下的 32MiB 上）。
	// 现在才武装调用方的到期：确定性地把「取消」落在**写飞行期间**。
	expired := time.AfterFunc(callerDeadline, cancel)
	defer expired.Stop()

	<-ctx.Done()
	select {
	case err := <-sent:
		if err == nil {
			t.Fatalf("前提被破坏：客户端零读取时 32MiB 仍在取消之前写完了（socket 缓冲比载荷还大？）")
		}
		// 写已经失败：这就是 buggy 版本的形状（会话十有八九已经被拆了）。
		// 不当成前提问题，让后面的读断言报出真正的病状（RED 路径）。
		t.Logf("Send 在调用方取消后立即返回: %v", err)
	default:
	}

	// 客户端开始读：把 32MiB 收干净，写因此才可能完成。
	readCtx, readCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer readCancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("客户端没读到 32MiB 帧（会话被调用方的取消拆了？hub.Online=%v）: %v",
			e.hub.Online(e.node.ID), err)
	}
	if len(data) != len(want) {
		t.Fatalf("客户端收到的帧长 = %d, want %d（帧不完整）", len(data), len(want))
	}
	frameType, err := protocol.FrameTypeOf(data)
	if err != nil {
		t.Fatalf("收到的不是合法帧: %v", err)
	}
	if frameType != protocol.FrameLogsResponse {
		t.Fatalf("帧类型 = %q, want %q", frameType, protocol.FrameLogsResponse)
	}

	// 调用方返回：写必须成功——会话的生命周期不归调用方的 ctx 管。
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("调用方 ctx 到期后 Send 返回错误（会话被调用方拆掉）: %v", err)
		}
	case <-time.After(sessionSettleLimit):
		t.Fatalf("客户端已收完 32MiB，Send 仍在 %s 内没有返回", sessionSettleLimit)
	}
	// 会话仍在线，后续帧照常收发。
	if !e.hub.Online(e.node.ID) {
		t.Fatal("调用方 ctx 到期把整条会话拆了（#340 的回归）")
	}
	e.handled(t, conn)

	// 收尾：优雅关连接，并等 detach 的**库写**落完（MarkInstancesDead 是 detach 的
	// 最后一笔写）。否则 db.Close 会与它撞上——#259 / #340 r3 的同一条不变量。
	closeConnAndWaitSettled(t, e, conn)
}

// TestSessionSendWriteTimeoutStillClosesSession 钉住 WithoutCancel 修复的**另一半**：
// 调用方的取消不再拥有拆会话的权力，但写的边界仍由 hub.writeTimeout 给——写超时是
// 「这条连接已经不可用」，必须按真实写失败处理（关会话），而不是被当成调用方取消。
//
// 为什么值得单独一条：把 `context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)`
// 里的超时也去掉（即彻底没有写边界）时，其余用例仍全绿：它们全都不依赖「写会被
// 超时打断」。本用例用同一条 32MiB 阻塞写（客户端始终不读）+ 200ms 写上限把它钉住。
func TestSessionSendWriteTimeoutStillClosesSession(t *testing.T) {
	e := newEnv(t)
	// 本用例的主角就是这个上界：客户端不读 ⇒ 32MiB 写必然阻塞到它到期。
	e.hub.writeTimeout = 200 * time.Millisecond
	counter := newCountingServer(t, e)

	conn := e.dial(t)
	conn.SetReadLimit(2 * bigFrameBytes)
	sess := e.hub.session(e.node.ID)
	if sess == nil {
		t.Fatal("节点未注册到 hub")
	}

	frame := bigLogsFrame()
	if _, err := protocol.Encode(frame); err != nil {
		t.Fatalf("编码 32MiB 帧: %v", err)
	}
	started := counter.arm()
	sent := make(chan error, 1)
	go func() { sent <- sess.Send(context.Background(), frame) }()

	select {
	case <-started:
	case err := <-sent:
		t.Fatalf("Send 在服务端开始写之前就返回了: %v", err)
	case <-time.After(writeStartLimit):
		t.Fatalf("服务端在 %s 内没有开始写这条 32MiB 帧（前提不成立；已记账 %d 字节）",
			writeStartLimit, counter.written())
	}

	var err error
	select {
	case err = <-sent:
	case <-time.After(sessionSettleLimit):
		// 200ms 的写上限 + 这个上界留了 25 倍余量；超时就是「写没有被边界中断」。
		t.Fatal("客户端始终不读，写却没有在 writeTimeout 内被中断（写边界丢了？）")
	}
	if err == nil {
		t.Fatal("32MiB 在客户端零读取下写成功了（socket 缓冲比载荷还大？）")
	}
	if errors.Is(err, ErrSendCanceled) {
		t.Fatalf("写超时被当成了调用方取消（分类错了）: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want 包住 context.DeadlineExceeded（写超时的形状）", err)
	}

	// 生产调用点的形状：写失败 → detach 关会话。
	// 这条路径上连接已经被 coder/websocket 的写超时（AfterFunc → c.close）关掉、读循环
	// 也会自己 detach，所以下面的 Online 断言**不是**判别点；本用例的判别点全在上面三条
	// （包住 DeadlineExceeded / 不命中 ErrSendCanceled / 写确实被边界中断）。
	e.hub.detachSessionOnWriteFailure(sess, err)
	waitDetachSettled(t, e)
	if e.hub.Online(e.node.ID) {
		t.Fatal("写超时后会话仍在线（连接已经不可用）")
	}
}

// closeConnAndWaitSettled 优雅关掉节点连接，并等到会话退干净、detach 的库写已落。
func closeConnAndWaitSettled(t *testing.T, e *env, conn *websocket.Conn) {
	t.Helper()
	before := e.instances(t)
	if len(before) == 0 {
		t.Fatal("前提不成立：hello 申报过 slot 0，槽位行不该为空")
	}
	alive := 0
	for _, row := range before {
		if row.Alive {
			alive++
		}
	}
	if alive == 0 {
		t.Fatal("前提不成立：hello 之后槽位应是 alive，否则「退干净」的观测点没有意义")
	}

	_ = conn.Close(websocket.StatusNormalClosure, "")
	waitDetachSettled(t, e)
	if e.hub.Online(e.node.ID) {
		t.Fatal("连接已关，会话仍留在注册表里")
	}
}

// waitDetachSettled 等 detach 的**库写**落完（MarkInstancesDead 是最后一笔）。
//
// 只看 hub.Online==false 不够：detach 是**先**摘注册表**再**写库（#340 r3 的注释），
// 所以这里等的是库事实：槽位 alive=0。没等到就直接让 db.Close 与它撞上（#259）。
func waitDetachSettled(t *testing.T, e *env) {
	t.Helper()
	settled := waitFor(t, sessionSettleLimit, func() bool {
		rows, err := e.db.ListInstances(context.Background(), e.node.ID)
		if err != nil || len(rows) == 0 {
			return false
		}
		for _, row := range rows {
			if row.Alive {
				return false
			}
		}
		return true
	})
	if !settled {
		t.Fatal("槽位没有全部标成 dead：detach 的库写没落完（db.Close 会与它撞上）")
	}
}

// TestSessionSendPreCanceledCtxWritesNothing 是 #340 m-2 的判别性用例。
//
// 走的是**生产调用点的形状**（hub.go 的 DispatchOnce / deploy / RequestLogs 都是
// `if err := sess.Send(...); err != nil { detachSessionOnWriteFailure(sess, err) }`）：
// 预取消的 ctx 调 Send → 一帧不写、错误可判别为 ErrSendCanceled → detach 放过会话。
// 判别点在 detach 那一步：哨兵之前这里返回的是裸 ctx.Err()，detach 会把它当写失败
// 关掉整条会话（“好端端的节点被标 offline”）。
func TestSessionSendPreCanceledCtxWritesNothing(t *testing.T) {
	e := newEnv(t)
	conn := e.dial(t)
	sess := e.hub.session(e.node.ID)
	if sess == nil {
		t.Fatal("节点未注册到 hub")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进入即已取消

	err := sess.Send(ctx, &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet})
	if err == nil {
		t.Fatal("预取消的 ctx 不该写出任何帧")
	}
	if !errors.Is(err, ErrSendCanceled) {
		t.Fatalf("err = %v, want ErrSendCanceled（detach 要靠它区分「调用方取消」与「写失败」）", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v 丢掉了 ctx 原因（HTTP 层的 504 映射靠 errors.Is(err, context.Canceled)）", err)
	}

	// 生产调用点的形状：把 Send 的错误交给 detachSessionOnWriteFailure，由它决定
	// 要不要拆会话（这一步才是本用例的判别点，不是 Send 自己能判的）。
	e.hub.detachSessionOnWriteFailure(sess, err)
	if !e.hub.Online(e.node.ID) {
		t.Fatal("预取消把会话下线了（#340 m-2 的回归）")
	}

	// 不写帧：客户端下一条读到的是自己 config.get 的 welcome —— 若上面真写了一帧，
	// 它会排在 welcome 前面，有序屏障会看见它。
	e.handled(t, conn)

	closeConnAndWaitSettled(t, e, conn)
}

// TestDetachSessionOnWriteFailureClassification 钉住分类表：哪些错误允许拆会话、
// 哪些必须放过。判别点直接看会话的 closed 标记——「放过」这一支只可能由本函数
// 改写（它不会去关连接，读循环也还活着）。
func TestDetachSessionOnWriteFailureClassification(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantClose bool
	}{
		{
			name: "调用方在写帧前取消（ErrSendCanceled）",
			// 与 session.go 入口 guard 的形状一致：哨兵 + ctx 原因。
			err:       fmt.Errorf("%w: %w", ErrSendCanceled, context.Canceled),
			wantClose: false,
		},
		{
			name:      "会话本来就关了（ErrSessionClosed）",
			err:       ErrSessionClosed,
			wantClose: false,
		},
		{
			name:      "真实写失败（EPIPE）",
			err:       errors.New("failed to write frame: broken pipe"),
			wantClose: true,
		},
		{
			name:      "写超时（写 ctx 到期 = 连接不可用）",
			err:       fmt.Errorf("failed to write frame: %w", context.DeadlineExceeded),
			wantClose: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			conn := e.dial(t)
			// 客户端持续读：coder/websocket 的 Conn.Close 要等对端回 close 帧（最多 5s），
			// 没有读者时每个「关会话」的子用例都要付满 5s；有读者就是毫秒级。
			go func() {
				for {
					if _, _, err := conn.Read(context.Background()); err != nil {
						return
					}
				}
			}()
			sess := e.hub.session(e.node.ID)
			if sess == nil {
				t.Fatal("节点未注册到 hub")
			}

			e.hub.detachSessionOnWriteFailure(sess, tc.err)

			// 判别点：会话的 closed 标记。放过的两支里读循环还活着、连接也没被关，
			// 所以 true 只可能来自本函数。
			if got := sess.closed(); got != tc.wantClose {
				t.Fatalf("会话 closed() = %v, want %v（err=%v）", got, tc.wantClose, tc.err)
			}
			if !tc.wantClose && !e.hub.Online(e.node.ID) {
				t.Fatal("放过的那一类错误不得下线会话")
			}

			if tc.wantClose {
				// 会话已经关了：只需等 detach 的库写落完，别让 db.Close 与它撞上。
				waitDetachSettled(t, e)
				return
			}
			closeConnAndWaitSettled(t, e, conn)
		})
	}
}
