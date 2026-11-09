package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
)

// ErrAuthRejected 是节点身份被中枢拒绝（握手 401/403）：**终态**，不重连。
var ErrAuthRejected = errors.New("hub: 节点身份被拒绝")

// defaultReadLimit 是下行帧读上限（与协议 1 MiB 软上限留余量）。
const defaultReadLimit = 4 << 20

// Handler 是节点对配置/部署帧的处理入口（由 internal/node 实现）。
//
// A 阶段只接线配置 5 帧与部署 2 帧；执行/运维帧留给 N2B 扩展，客户端不假装
// 处理它们（未知/未接线的下行帧忽略，不掀连接）。
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

// DialFunc 是 websocket.Dial 的可注入形状（测试用假中枢/假失败）。
type DialFunc func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)

// SleepFunc 是退避等待的可注入形状（测试不真等）。
type SleepFunc func(ctx context.Context, d time.Duration) error

// Client 是节点到中枢的 WS 配置通道。
type Client struct {
	ServerURL string
	Token     string
	Handler   Handler

	Dial      DialFunc
	Sleep     SleepFunc
	Backoff   Backoff
	ReadLimit int64
	Logf      func(format string, args ...any)
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

	if err := c.send(ctx, conn, c.Handler.Hello()); err != nil {
		return err
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("hub: 读取帧: %w", err)
		}
		frame, err := protocol.Decode(data)
		if err != nil {
			// 一条坏帧不掀整条连接（与协议包「unknown type 不断连」同一取向）。
			c.logf("hub: 忽略坏帧: %v", err)
			continue
		}
		switch f := frame.(type) {
		case *protocol.WelcomeFrame:
			// 收到 welcome = 握手真正完成，退避从此刻重置。
			c.Backoff.Reset()
			applied, applyErr := c.Handler.ApplyWelcome(*f)
			if applyErr != nil {
				// 落盘失败不回报 config.applied（写成功前不切生效值）；连接保留，
				// 下一次 config.changed / hello 会再对齐。
				c.logf("hub: 应用 welcome 失败: %v", applyErr)
				continue
			}
			if applied != nil {
				if err := c.send(ctx, conn, applied); err != nil {
					return err
				}
			}
		case *protocol.ConfigChangedFrame:
			rev, hash := c.Handler.AppliedState()
			get := protocol.ConfigGetFrame{
				Type:            protocol.FrameConfigGet,
				AppliedRevision: rev,
				AppliedHash:     hash,
			}
			if err := c.send(ctx, conn, get); err != nil {
				return err
			}
		case *protocol.CapabilityDeployFrame:
			ack := c.Handler.HandleDeploy(ctx, f)
			if ack != nil {
				if err := c.send(ctx, conn, ack); err != nil {
					return err
				}
			}
		case *protocol.UnknownFrame:
			c.logf("hub: 忽略未知帧 %q", f.Type)
		default:
			// 执行/运维帧由 N2B 接线；A 不处理也不伪造。
		}
	}
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

func (c *Client) send(ctx context.Context, conn *websocket.Conn, frame any) error {
	data, err := protocol.Encode(frame)
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("hub: 写帧: %w", err)
	}
	return nil
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
