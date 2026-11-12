package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
)

// ErrAuthRejected 是节点身份被中枢拒绝（握手 401/403）：**终态**，不重连。
var ErrAuthRejected = errors.New("hub: 节点身份被拒绝")

// ErrOffline 表示当前没有可用连接（帧没发出去；调用方保留事实等重连）。
var ErrOffline = errors.New("hub: 与中枢的连接不可用")

// ErrCheckTimeout 是一次性预检（Check）在调用方给的期限内没收到 welcome：**终态**，不重试。
//
// 预检不新增超时 flag：`anode serve --check` 用固定的 10s ctx deadline，到期即此错误。
var ErrCheckTimeout = errors.New("hub: 预检未在期限内收到 welcome")

// defaultReadLimit 是下行帧读上限（与协议 1 MiB 软上限留余量）。
const defaultReadLimit = 4 << 20

// writeTimeout 是单帧写超时（慢网络不阻塞其它发送方无限期）。
const writeTimeout = 10 * time.Second

// Handler 是节点对配置/部署帧的处理入口（由 internal/node 实现）。
//
// N2B 的执行/运维/emit 帧通过下面的可选接口接线：只实现 Handler 的节点（N2A
// 的配置/部署闭包）仍然可用，未接线的下行帧忽略而不是伪造。
type Handler interface {
	// Hello 返回本次连接的第一帧（每次重连都重新取当前事实）。
	Hello() protocol.HelloFrame
	// ApplyWelcome 合并并（必要时）落盘；nil 帧 = 幂等无需回报。
	ApplyWelcome(welcome protocol.WelcomeFrame) (*protocol.ConfigAppliedFrame, error)
	// AppliedState 返回当前生效的 (revision, hash)，config.get 用。
	AppliedState() (revision, hash string)
	// HandleDeploy 处理一条显式部署；返回 nil = 不回执（不应发生）。
	HandleDeploy(ctx context.Context, frame *protocol.CapabilityDeployFrame) *protocol.CapabilityDeployedFrame
}

// ExecHandler 是执行子协议入口（runner 实现）。
type ExecHandler interface {
	HandleDispatch(frame *protocol.TaskDispatchFrame)
	HandleLeaseGrant(frame *protocol.LeaseGrantFrame)
	HandleLeaseAck(frame *protocol.LeaseAckFrame)
	HandleResultAck(frame *protocol.ResultAckFrame)
	HandleCancel(frame *protocol.TaskCancelFrame)
}

// OpsHandler 是保留运维帧入口（ops 实现）。
//
// Upgrade 自己负责 ack（send 是并发安全的上行入口）：替换成功后先 ack 实际版本
// 再触发重启，顺序不能反（ack 丢了中枢就永远不知道升没升上去）。
type OpsHandler interface {
	HandleUpgrade(ctx context.Context, frame *protocol.UpgradeFrame, send func(any) error)
	HandleLogsRequest(ctx context.Context, frame *protocol.LogsRequestFrame) *protocol.LogsResponseFrame
}

// EmitAckHandler 是 task.emit.ack 的配对入口（emit 网关实现）。
type EmitAckHandler interface {
	HandleEmitAck(frame *protocol.TaskEmitAckFrame)
}

// SessionHandler 在每次连接建立（hello 已发）后收到通知：重发未确认的事实。
type SessionHandler interface {
	SessionUp()
}

// DialFunc 是 websocket.Dial 的可注入形状（测试用假中枢/假失败）。
type DialFunc func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)

// SleepFunc 是退避等待的可注入形状（测试不真等）。
type SleepFunc func(ctx context.Context, d time.Duration) error

// Client 是节点到中枢的 WS 通道（配置 + 执行 + 运维 + emit 共用一条连接）。
type Client struct {
	ServerURL string
	Token     string
	Handler   Handler

	Dial      DialFunc
	Sleep     SleepFunc
	Backoff   Backoff
	ReadLimit int64
	Logf      func(format string, args ...any)

	writeMu    sync.Mutex
	conn       *websocket.Conn
	generation atomic.Uint64 // changes invalidate sends queued for an earlier connection
}

// New 校验 server 形状并装配客户端。
func New(serverURL, token string, handler Handler) (*Client, error) {
	if handler == nil {
		return nil, errors.New("hub: 缺少 Handler")
	}
	if _, err := StreamURL(serverURL); err != nil {
		return nil, err
	}
	return &Client{ServerURL: serverURL, Token: token, Handler: handler}, nil
}

// Check 做一次性身份预检：连接 → hello → 等到 welcome → 返回。
//
// 它是 `anode serve --check` 的实现（v4.1 契约 §4）：只确认「中枢可达 + 身份被接受」，
// 不应用配置、不写盘、不上报本地 applied。**不重连、不重试、不 sleep、不 backoff**：
// 调用方给 ctx 一个 deadline（CLI 固定 10s），那就是整次预检的上限。
//
// 诊断约定（CLI 据此分 kind）：
//   - 401/403、或 welcome 之前的 close 1008 → 包装 ErrAuthRejected（身份被拒）；
//   - ctx 到期 → 包装 ErrCheckTimeout；ctx 取消 → 返回 ctx.Err()；
//   - 其它失败原样返回（CLI 归入 timeout 诊断，detail 带真实原因）。
func Check(ctx context.Context, serverURL, token string, hello protocol.HelloFrame) error {
	streamURL, err := StreamURL(serverURL)
	if err != nil {
		return err
	}
	data, err := protocol.Encode(hello)
	if err != nil {
		return err
	}
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if token != "" {
		opts.HTTPHeader.Set(protocol.EmitAuthHeader, protocol.EmitAuthScheme+" "+token)
	}
	conn, resp, err := websocket.Dial(ctx, streamURL, opts)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("%w: HTTP %d", ErrAuthRejected, resp.StatusCode)
		}
		return checkContextErr(ctx, fmt.Errorf("hub: 预检连接 %s: %w", streamURL, err))
	}
	// 一次性预检：不做优雅关闭（Close 会等对端回 close 帧，最多 5s；预检拿到答案就退出）。
	defer conn.CloseNow()
	conn.SetReadLimit(defaultReadLimit)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return checkContextErr(ctx, fmt.Errorf("hub: 预检写 hello: %w", err))
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// welcome 之前收到 close 1008 = 中枢拒绝这次握手（令牌/名字不被接受）。
			// ctx 已结束时不把 close 语义当结论：到期/取消优先。
			if ctx.Err() == nil {
				if status := websocket.CloseStatus(err); status == websocket.StatusPolicyViolation {
					return fmt.Errorf("%w: 中枢在 welcome 之前以 close %d 断开", ErrAuthRejected, status)
				}
			}
			return checkContextErr(ctx, fmt.Errorf("hub: 预检读帧: %w", err))
		}
		frame, err := protocol.Decode(data)
		if err != nil {
			return fmt.Errorf("hub: 预检收到坏帧: %w", err)
		}
		if _, ok := frame.(*protocol.WelcomeFrame); ok {
			return nil
		}
		// welcome 之前的其它帧与身份无关（未知帧/保留帧）：丢弃继续等。
	}
}

// checkContextErr 把「到期/取消」翻译成预检诊断；其它失败原样返回。
func checkContextErr(ctx context.Context, err error) error {
	switch ctxErr := ctx.Err(); {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", ErrCheckTimeout, ctxErr)
	case ctxErr != nil:
		return ctxErr
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", ErrCheckTimeout, err)
	default:
		return err
	}
}

// Run 连接中枢并保持配置同步，直到 ctx 结束或认证被拒。
//
// 断线/连接失败按有界指数退避重连（封顶 DefaultBackoffMax）；401/403 是终态
// （令牌错重试没用，只会制造登录风暴）。每次连接（含重连）都重发 hello 全量
// 对齐——中枢从不主动问节点。
func (c *Client) Run(ctx context.Context) error {
	if c.Handler == nil {
		return errors.New("hub: 缺少 Handler")
	}
	if _, err := StreamURL(c.ServerURL); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.runSession(ctx)
		if err == nil {
			err = errors.New("hub: 连接结束")
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrAuthRejected) {
			return err
		}
		delay := c.Backoff.Next()
		c.logf("hub: %v；%s 后重连", err, delay)
		if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
			return sleepErr
		}
	}
}

// Send 把一帧写到当前连接（并发安全）。
//
// 离线返回 ErrOffline（调用方保留事实等重连重发）；不阻塞、不排队、不重放。
func (c *Client) Send(frame any) error {
	data, err := protocol.Encode(frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return ErrOffline
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("hub: 写帧: %w", err)
	}
	return nil
}

// SendGuarded is the task.result write fence. guard must only inspect state:
// it runs under writeMu, after the connection check, immediately before Write.
// false/nil means locally rejected (no bytes), not a network failure. ctx must
// be cancelable by the lease owner and bounded by its authorization deadline.
// Cancellation fences admission, but cannot retract an admitted write: that
// write retains the original deadline/network cap, not attempt cleanup cancel.
// No runner/attempt lock is retained during the network write.
func (c *Client) SendGuarded(ctx context.Context, frame any, guard func() bool) (bool, error) {
	generation := c.generation.Load()
	data, err := protocol.Encode(frame)
	if err != nil {
		return false, err
	}
	// Capture the original deadline and network cap before queuing. Later lease
	// renewal cannot extend this snapshot, and queue time consumes the same cap.
	admissionCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if generation != c.generation.Load() {
		return false, nil
	}
	if c.conn == nil {
		return false, ErrOffline
	}
	if admissionCtx.Err() != nil || guard == nil || !guard() {
		return false, nil
	}
	if admissionCtx.Err() != nil { // Caller cancellation during guard still precedes admission.
		return false, nil
	}
	// The guard/caller check is the admission fence. In particular a trusted
	// result ACK may immediately cancel the attempt while Write is returning;
	// propagating it into coder/websocket would close the entire shared stream.
	// WithoutCancel strips deadlines too, so explicitly restore the captured
	// minimum of the caller's authorization deadline and the original 10s cap.
	// Connection Close still interrupts IO; the server finally validates bytes.
	deadline, _ := admissionCtx.Deadline() // WithTimeout always supplies one.
	writeCtx, stopWrite := context.WithDeadline(context.WithoutCancel(admissionCtx), deadline)
	defer stopWrite()
	if err := c.conn.Write(writeCtx, websocket.MessageText, data); err != nil {
		return false, fmt.Errorf("hub: 写 guarded 帧: %w", err)
	}
	return true, nil
}

// runSession 是一次「连上 → 收发 → 断开」的生命周期。
func (c *Client) runSession(ctx context.Context) error {
	streamURL, err := StreamURL(c.ServerURL)
	if err != nil {
		return err
	}
	conn, resp, err := c.dial(ctx, streamURL)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("%w: HTTP %d", ErrAuthRejected, resp.StatusCode)
		}
		return fmt.Errorf("hub: 连接 %s: %w", streamURL, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "anode session end")
	limit := c.ReadLimit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	conn.SetReadLimit(limit)
	c.setConn(conn)
	defer c.clearConn(conn)

	if err := c.Send(c.Handler.Hello()); err != nil {
		return err
	}
	if h, ok := c.Handler.(SessionHandler); ok {
		h.SessionUp()
	}
	// welcomed 之前收到 close 1008 = 中枢拒绝这次握手：身份拒绝是终态（重连只会制造
	// 登录风暴，与 401/403 同一取向）。welcome 之后（含被新连接顶掉的会话）保持
	// 可重连：那已经不是「身份不被接受」。
	welcomed := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if !welcomed && ctx.Err() == nil {
				if status := websocket.CloseStatus(err); status == websocket.StatusPolicyViolation {
					return fmt.Errorf("%w: welcome 之前被 close %d 断开", ErrAuthRejected, status)
				}
			}
			return fmt.Errorf("hub: 读取帧: %w", err)
		}
		frame, err := protocol.Decode(data)
		if err != nil {
			// 一条坏帧不掀整条连接（与协议包「unknown type 不断连」同一取向）。
			c.logf("hub: 忽略坏帧: %v", err)
			continue
		}
		if _, ok := frame.(*protocol.WelcomeFrame); ok {
			welcomed = true
		}
		c.dispatch(ctx, conn, frame)
	}
}

// dispatch 分派一条下行帧。
//
// 慢操作（部署 GET、日志读盘、升级下载、取消收尾）放到 goroutine：一个慢 HTTP
// 不能卡住同一条连接上的 lease.ack / result.ack / emit.ack 读取。
func (c *Client) dispatch(ctx context.Context, conn *websocket.Conn, frame any) {
	switch f := frame.(type) {
	case *protocol.WelcomeFrame:
		// 收到 welcome = 握手真正完成，退避从此刻重置。
		c.Backoff.Reset()
		applied, err := c.Handler.ApplyWelcome(*f)
		if err != nil {
			// 落盘失败不回报 config.applied（写成功前不切生效值）；连接保留，
			// 下一次 config.changed / hello 会再对齐。
			c.logf("hub: 应用 welcome 失败: %v", err)
			return
		}
		if applied != nil {
			c.sendAsync(applied)
		}
	case *protocol.ConfigChangedFrame:
		rev, hash := c.Handler.AppliedState()
		c.sendAsync(protocol.ConfigGetFrame{
			Type:            protocol.FrameConfigGet,
			AppliedRevision: rev,
			AppliedHash:     hash,
		})
	case *protocol.CapabilityDeployFrame:
		if h, ok := c.Handler.(interface {
			HandleDeploy(context.Context, *protocol.CapabilityDeployFrame) *protocol.CapabilityDeployedFrame
		}); ok {
			go func() {
				ack := h.HandleDeploy(ctx, f)
				if ack == nil {
					return
				}
				if err := c.Send(ack); err != nil {
					c.logf("hub: 发送 capability.deployed 失败: %v", err)
				}
			}()
		}
	case *protocol.TaskDispatchFrame:
		if h, ok := c.Handler.(ExecHandler); ok {
			h.HandleDispatch(f)
		}
	case *protocol.LeaseGrantFrame:
		if h, ok := c.Handler.(ExecHandler); ok {
			h.HandleLeaseGrant(f)
		}
	case *protocol.LeaseAckFrame:
		if h, ok := c.Handler.(ExecHandler); ok {
			h.HandleLeaseAck(f)
		}
	case *protocol.ResultAckFrame:
		if h, ok := c.Handler.(ExecHandler); ok {
			h.HandleResultAck(f)
		}
	case *protocol.TaskCancelFrame:
		if h, ok := c.Handler.(ExecHandler); ok {
			h.HandleCancel(f)
		}
	case *protocol.TaskEmitAckFrame:
		if h, ok := c.Handler.(EmitAckHandler); ok {
			h.HandleEmitAck(f)
		}
	case *protocol.UpgradeFrame:
		if h, ok := c.Handler.(OpsHandler); ok {
			go h.HandleUpgrade(ctx, f, c.Send)
		} else {
			c.logf("hub: 未接线 upgrade 帧（忽略）")
		}
	case *protocol.LogsRequestFrame:
		if h, ok := c.Handler.(OpsHandler); ok {
			go func() {
				resp := h.HandleLogsRequest(ctx, f)
				if resp == nil {
					return
				}
				if err := c.Send(resp); err != nil {
					c.logf("hub: 发送 logs_response 失败: %v", err)
				}
			}()
		}
	case *protocol.UnknownFrame:
		c.logf("hub: 忽略未知帧 %q", f.Type)
	default:
		// 其它下行帧（config.applied 等）不由节点接收。
	}
}

// sendAsync 在独立 goroutine 里发一帧（读循环不因写超时停摆）。
func (c *Client) sendAsync(frame any) {
	go func() {
		if err := c.Send(frame); err != nil {
			c.logf("hub: 发送 %T 失败: %v", frame, err)
		}
	}()
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.writeMu.Lock()
	c.conn = conn
	c.generation.Add(1)
	c.writeMu.Unlock()
}

func (c *Client) clearConn(conn *websocket.Conn) {
	c.writeMu.Lock()
	if c.conn == conn {
		c.conn = nil
		c.generation.Add(1)
	}
	c.writeMu.Unlock()
}

func (c *Client) dial(ctx context.Context, streamURL string) (*websocket.Conn, *http.Response, error) {
	dial := c.Dial
	if dial == nil {
		dial = websocket.Dial
	}
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if c.Token != "" {
		opts.HTTPHeader.Set(protocol.EmitAuthHeader, protocol.EmitAuthScheme+" "+c.Token)
	}
	return dial(ctx, streamURL, opts)
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
