package ws

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// Session 是一条已认证的节点连接。
//
// 身份三元组（NodeID / NodeName / TokenHash）在 hello 那一刻固定，之后**只读**：
// 授权写入口每次都会把这份 auth 交给 store 事务与库里的当前哈希重新比对，所以
// 令牌轮换/ns 撤销在下一个写入口立刻生效（不需要也不允许会话自己缓存「已校验」）。
type Session struct {
	hub  *Hub
	conn *websocket.Conn

	nodeID    string
	nodeName  string
	tokenHash string
	auth      store.NodeAuthorization

	writeMu  sync.Mutex
	stateMu  sync.Mutex
	isClosed bool
}

// NodeID 返回认证后的 nodes.id。
func (s *Session) NodeID() string { return s.nodeID }

// NodeName 返回已登记节点名（wire 上的 node 字段必须与它一致）。
func (s *Session) NodeName() string { return s.nodeName }

// Auth 返回固定的 NodeAuthorization（grant/renew/result/cancel/emit 共用）。
func (s *Session) Auth() store.NodeAuthorization { return s.auth }

// Send 编码并写出一帧（写串行化 + 有界写超时）。
//
// 写超时上下文**不继承调用方的可取消性**（context.WithoutCancel）。
//
// 为什么：coder/websocket 的 write timeout 是 `context.AfterFunc(ctx, …c.close())`
// 实现的（conn.go 的 setupWriteTimeout）——那个 AfterFunc 要到 writeFrame 的 defer
// 里才被解除。调用方（HTTP 请求的 r.Context()）如果在帧已经写到 socket 之后、解除之前
// 取消，AfterFunc 就会把**整条节点会话**关掉。真实触发路径：客户端读到帧立刻 cancel
// 请求上下文 → 服务端还差一条 defer 没跑 → 会话被拆 → detach 唤醒在飞的 logs waiter
// 返回 ErrNodeOffline（表现为「节点在线却没回包」的请求拿到 503 node_offline，而不是
// 调用方自己的 504）。调用方取消只该结束**它自己的等待**，不该拥有拆会话的权力；
// 写的边界由 s.hub.writeTimeout 给（写自己也会在超时后关连接，这是 coder/websocket 的语义）。
//
// 「进入即已取消」不算写失败：返回可判别的 ErrSendCanceled，会话毫发无损
// （hub 里会转给 hub.detachSessionOnWriteFailure 的三个发送口——dispatch / deploy /
// logs——都靠这个哨兵放过会话；handler 回包那几处只把错误返回给帧处理器）。
func (s *Session) Send(ctx context.Context, frame any) error {
	// 进入时就已取消：尊重调用方，不再往线上写一帧没人等的请求。
	//
	// 不用裸 ctx.Err()：这个错误会经 detachSessionOnWriteFailure 决定**要不要拆掉整条
	// 会话**（hub 的 dispatch / deploy / logs 三个发送口都会转给它），而调用方取消只该
	// 结束它自己的等待（窗口窄但真实：HTTP 请求在调 Send 之前就被撤掉时，裸 ctx 错误会被
	// 当成写失败 → 好端端的节点会话被关）。
	// 保留 %w 链是有意的：errors.Is(err, context.Canceled/DeadlineExceeded) 仍然成立，
	// HTTP 层对这两者的 504 映射（resource_task.go 的 logs 分支）语义不变。
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrSendCanceled, err)
	}
	data, err := protocol.Encode(frame)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed() {
		return ErrSessionClosed
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.hub.writeTimeout)
	defer cancel()
	if err := s.conn.Write(writeCtx, websocket.MessageText, data); err != nil {
		return err
	}
	return nil
}

// close 关闭底层连接（幂等）。
func (s *Session) close(code websocket.StatusCode, reason string) {
	s.stateMu.Lock()
	if s.isClosed {
		s.stateMu.Unlock()
		return
	}
	s.isClosed = true
	s.stateMu.Unlock()
	_ = s.conn.Close(code, reason)
}

// closedFlag 把会话标记成关闭（不重复 Close：读循环退出时连接已经不可用）。
func (s *Session) closedFlag() {
	s.stateMu.Lock()
	s.isClosed = true
	s.stateMu.Unlock()
}

// closed 报告会话是否已经关闭。
func (s *Session) closed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.isClosed
}

// serveSession 跑会话读循环，直到连接断开/协议违例。
//
// 边界：
//   - 逐帧软上限来自共享协议（FrameSoftLimit），超限由传输层以 1009 断开；
//   - 未知帧类型**不断连接**（Decode 的既定语义：一条老/新帧不该打断整条连接）；
//   - 方向错误的帧（node 发中枢下行帧）是协议违例，直接断开；
//   - 没有 T3 定义过的 error 帧可回，所以错误不做「发明一帧」的兜底。
func (h *Hub) serveSession(ctx context.Context, sess *Session) {
	defer h.detach(sess)
	sess.conn.SetReadLimit(protocol.FrameSoftLimit)
	for {
		_, data, err := sess.conn.Read(ctx)
		if err != nil {
			return
		}
		frameType, err := protocol.FrameTypeOf(data)
		if err != nil {
			h.log.Warn("bad frame from node", "node_id", sess.nodeID, "err", err)
			sess.close(websocket.StatusUnsupportedData, "bad_frame")
			return
		}
		if protocol.IsDownstream(frameType) {
			h.log.Warn("node sent a downstream frame", "node_id", sess.nodeID, "frame", frameType)
			sess.close(websocket.StatusPolicyViolation, "unexpected frame direction")
			return
		}
		if !protocol.IsUpstream(frameType) {
			// 未知帧：记录并继续（不打断整条连接）。
			h.log.Debug("unknown frame from node", "node_id", sess.nodeID, "frame", frameType)
			continue
		}
		frame, err := protocol.Decode(data)
		if err != nil {
			h.log.Warn("decode frame failed", "node_id", sess.nodeID, "frame", frameType, "err", err)
			sess.close(websocket.StatusUnsupportedData, "bad_frame")
			return
		}
		if err := h.dispatchFrame(ctx, sess, frame); err != nil {
			h.log.Warn("frame handler failed", "node_id", sess.nodeID, "frame", frameType, "err", err)
			if errors.Is(err, errProtocolViolation) {
				sess.close(websocket.StatusPolicyViolation, "protocol violation")
				return
			}
		}
		if err := h.db.TouchNode(ctx, sess.nodeID, h.Now()); err != nil {
			h.log.Debug("touch node failed", "node_id", sess.nodeID, "err", err)
		}
	}
}

// dispatchFrame 按帧类型分流（方向已在上游检查）。
func (h *Hub) dispatchFrame(ctx context.Context, sess *Session, frame any) error {
	switch f := frame.(type) {
	case *protocol.HelloFrame:
		// rehello：只更新事实并回 welcome；**不**释放任何 busy（重连 != 回收）。
		if err := h.authenticate(ctx, sess, f); err != nil {
			return errProtocolViolation
		}
		if err := h.applyHello(ctx, sess, f); err != nil {
			return err
		}
		if err := h.sendWelcome(ctx, sess); err != nil {
			return err
		}
		return h.replayDeployments(ctx, sess)
	case *protocol.ConfigGetFrame:
		return h.sendWelcome(ctx, sess)
	case *protocol.ConfigAppliedFrame:
		return h.handleConfigApplied(ctx, sess, f)
	case *protocol.TaskAcceptFrame:
		return h.handleAccept(ctx, sess, f)
	case *protocol.TaskRejectFrame:
		return h.handleReject(ctx, sess, f)
	case *protocol.LeaseRenewFrame:
		return h.handleRenew(ctx, sess, f)
	case *protocol.TaskResultFrame:
		return h.handleResult(ctx, sess, f)
	case *protocol.CancelAckFrame:
		return h.handleCancelAck(ctx, sess, f)
	case *protocol.LogsResponseFrame:
		h.deliverLogs(sess, f)
		return nil
	case *protocol.CapabilityDeployedFrame:
		return h.handleDeployed(ctx, sess, f)
	case *protocol.TaskEmitFrame:
		return h.handleEmit(ctx, sess, f)
	case *protocol.UpgradeAckFrame:
		return h.handleUpgradeAck(ctx, sess, f)
	default:
		return nil
	}
}
