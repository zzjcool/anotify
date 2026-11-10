// Package emitgw 是节点上的 emit HTTP 网关：一个监听、一条 HTTP 语义、一种令牌。
//
// 能力脚本带 `Authorization: Bearer <attemptToken>` POST `$ANOTIFY_EMIT_ENDPOINT/emit`；
// 节点只做**本地预检**（token 归属、任务运行态、目标 ∈ 该 attempt 钉住的 next），
// 然后按 requestId 把请求转发成一条 task.emit WS 帧，等中枢的 task.emit.ack：
// 下游建任务 / id 幂等 / 跨节点派发 / 旧 attempt 拒绝都由中枢权威判定。
//
// 节点不做的事（契约明确禁止）：本地建下游任务、按 cache.next 替代中枢路由、
// 造业务 seq/去重、自动重放无 id 请求。结果未知 = 5（CLI 层），不是 infra 失败。
package emitgw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/runner"
	"github.com/zzjcool/anotify/internal/protocol"
)

// 上限与超时（明确、不静默截断）。
const (
	// MaxBodyBytes 是 HTTP request body 的硬上限（与 WS 帧软上限同值）。
	MaxBodyBytes = protocol.FrameSoftLimit
	// WarnBodyBytes 是软上限：超过只在日志里告警，仍原样转发（不截断）。
	WarnBodyBytes = 256 << 10
	// DefaultTimeout 是一次 emit RPC 的有界等待（中枢 30s 级）。
	DefaultTimeout = 30 * time.Second
	// MaxInflight 是同时在途的 emit 请求上限（有界等待表）。
	MaxInflight = 128
)

// Sender 是上行帧发送入口（hub.Client 实现）。
type Sender interface {
	Send(frame any) error
}

// Lookup 是「令牌 → 当前运行 attempt」的本地预检入口（runner 实现）。
type Lookup interface {
	LookupEmit(token string) (runner.EmitBinding, bool)
}

// LocalErrorCode 是本地网关自己的错误码（超出契约封闭集合的两条）。
const (
	// CodePayloadTooLarge 是 body / 编码后帧超上限（413，不截断）。
	CodePayloadTooLarge = "payload_too_large"
)

// Gateway 是 HTTP handler + 在途请求表。
type Gateway struct {
	Sender  Sender
	Lookup  Lookup
	Timeout time.Duration
	Logf    func(format string, args ...any)

	mu       sync.Mutex
	inflight map[string]*waiter
	closed   bool
}

type waiter struct {
	taskID  string
	attempt int
	ch      chan *protocol.TaskEmitAckFrame
}

// New 构造网关。
func New(sender Sender, lookup Lookup) *Gateway {
	return &Gateway{Sender: sender, Lookup: lookup, inflight: map[string]*waiter{}}
}

// Handler 返回 HTTP handler（只有 POST /emit）。
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(protocol.EmitPath, g.handleEmit)
	return mux
}

// Close 取消所有在途等待（断开连接 / 退出）。
func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
	waiters := g.inflight
	g.inflight = map[string]*waiter{}
	g.mu.Unlock()
	for _, w := range waiters {
		select {
		case w.ch <- nil:
		default:
		}
	}
}

// HandleEmitAck 把中枢 ACK 交给等待中的 HTTP 请求。
//
// 只按 requestId 找 waiter，并**再次核对** taskId / attempt：迟到 ACK、错 attempt
// 的 ACK 一律丢弃，绝不会被送给另一条 HTTP 请求。
func (g *Gateway) HandleEmitAck(frame *protocol.TaskEmitAckFrame) {
	if frame == nil || frame.Type != protocol.FrameTaskEmitAck {
		return
	}
	g.mu.Lock()
	w, ok := g.inflight[frame.RequestID]
	if ok {
		delete(g.inflight, frame.RequestID)
	}
	g.mu.Unlock()
	if !ok {
		g.logf("emitgw: 丢弃未在等待中的 ACK（requestId=%s）", frame.RequestID)
		return
	}
	if w.taskID != frame.TaskID || w.attempt != frame.Attempt {
		g.logf("emitgw: ACK 关联身份不符（requestId=%s：等待 %s/%d，实际 %s/%d），丢弃",
			frame.RequestID, w.taskID, w.attempt, frame.TaskID, frame.Attempt)
		return
	}
	select {
	case w.ch <- frame:
	default:
	}
}

func (g *Gateway) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
	}
}

// Inflight 返回当前在途数量（诊断/测试）。
func (g *Gateway) Inflight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.inflight)
}

func (g *Gateway) handleEmit(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "missing_target", "emit 只接受 POST")
		return
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	// 1) 令牌本地预检：token → 当前运行 attempt（任务结束即失效 = I8）。
	token := bearerToken(req.Header.Get(protocol.EmitAuthHeader))
	if token == "" {
		writeError(w, http.StatusUnauthorized, protocol.ReasonTokenInvalid, "缺少 Bearer 令牌")
		return
	}
	binding, ok := g.Lookup.LookupEmit(token)
	if !ok {
		writeError(w, http.StatusUnauthorized, protocol.ReasonTokenInvalid,
			"令牌不属于任何运行中的任务（或任务已结束）")
		return
	}

	// 2) body：硬上限 + 不静默截断。
	body, err := io.ReadAll(io.LimitReader(req.Body, MaxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.ReasonParamType, "读取请求体: "+err.Error())
		return
	}
	if len(body) > MaxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			fmt.Sprintf("请求体超过 %d 字节上限（不截断）", MaxBodyBytes))
		return
	}
	if len(body) > WarnBodyBytes {
		g.logf("emitgw: task=%s attempt=%d 的 emit body %d 字节超过软上限 %d（仍原样转发）",
			binding.TaskID, binding.Attempt, len(body), WarnBodyBytes)
	}

	var emit protocol.EmitRequest
	if err := json.Unmarshal(body, &emit); err != nil {
		writeError(w, http.StatusBadRequest, protocol.ReasonParamType, "请求体不是合法 JSON: "+err.Error())
		return
	}

	// 3) 缺目标 = missing_target 400（不是剪枝）。
	if strings.TrimSpace(emit.To) == "" {
		writeError(w, http.StatusBadRequest, protocol.ReasonMissingTarget, "emit 需要 --to/目标名")
		return
	}
	// 4) 目标必须在本 attempt 钉住的 next 里（中枢仍会以权威路由复核）。
	if !targetDeclared(binding.Next, emit.To) {
		writeError(w, http.StatusBadRequest, protocol.ReasonTargetNotDeclared,
			fmt.Sprintf("目标 %q 不在本能力的 next 里", emit.To))
		return
	}
	// 5) data 必须是 JSON 对象（省略 = 空对象）。
	if len(emit.Data) > 0 && strings.TrimSpace(string(emit.Data)) != "null" {
		var probe any
		if err := json.Unmarshal(emit.Data, &probe); err != nil {
			writeError(w, http.StatusBadRequest, protocol.ReasonParamType, "data 不是合法 JSON")
			return
		}
		if _, isObject := probe.(map[string]any); !isObject {
			writeError(w, http.StatusBadRequest, protocol.ReasonParamType, "data 必须是 JSON 对象")
			return
		}
	}

	requestID, err := newRequestID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "transport", "生成 requestId: "+err.Error())
		return
	}
	frame := &protocol.TaskEmitFrame{
		Type:         protocol.FrameTaskEmit,
		RequestID:    requestID,
		TaskID:       binding.TaskID,
		Attempt:      binding.Attempt,
		LeaseID:      binding.LeaseID,
		AttemptToken: binding.Token,
		EmitRequest:  protocol.EmitRequest{To: emit.To, ID: emit.ID, Data: emit.Data},
	}
	if err := frame.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, protocol.ReasonParamType, err.Error())
		return
	}
	raw, err := protocol.Encode(frame)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "transport", err.Error())
		return
	}
	if len(raw) > protocol.FrameSoftLimit {
		writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			fmt.Sprintf("编码后的 task.emit 帧 %d 字节超过软上限 %d（不截断）", len(raw), protocol.FrameSoftLimit))
		return
	}

	// 6) 有界在途表。
	wait := make(chan *protocol.TaskEmitAckFrame, 1)
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, protocol.ReasonEmitUnreachable, "节点正在退出")
		return
	}
	if len(g.inflight) >= MaxInflight {
		g.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, protocol.ReasonEmitUnreachable,
			fmt.Sprintf("在途 emit 已达上限 %d", MaxInflight))
		return
	}
	g.inflight[requestID] = &waiter{taskID: binding.TaskID, attempt: binding.Attempt, ch: wait}
	g.mu.Unlock()

	removeWaiter := func() {
		g.mu.Lock()
		delete(g.inflight, requestID)
		g.mu.Unlock()
	}

	// 7) 转发中枢；发送失败 = 结果未知（可能已派发），不能报成功/确定失败。
	if err := g.Sender.Send(frame); err != nil {
		removeWaiter()
		writeError(w, http.StatusBadGateway, protocol.ReasonEmitUnreachable,
			"与中枢的连接不可用，emit 结果未知："+err.Error())
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ack := <-wait:
		if ack == nil {
			writeError(w, http.StatusServiceUnavailable, protocol.ReasonEmitUnreachable, "节点已关闭")
			return
		}
		writeAck(w, ack)
	case <-timer.C:
		removeWaiter()
		writeError(w, http.StatusGatewayTimeout, protocol.ReasonEmitUnreachable,
			fmt.Sprintf("%s 内没有收到中枢 ACK，结果未知", timeout))
	case <-req.Context().Done():
		removeWaiter()
	}
}

// writeAck 把中枢的权威回执翻成 HTTP：成功只 response、错误只 error。
func writeAck(w http.ResponseWriter, ack *protocol.TaskEmitAckFrame) {
	if ack.Error != nil {
		status := ack.HTTPStatus
		if status == 0 {
			status = protocol.EmitHTTPStatus(ack.Error.Error)
		}
		writeJSON(w, status, protocol.EmitError{Error: ack.Error.Error, Message: ack.Error.Message})
		return
	}
	if ack.Response == nil {
		writeError(w, http.StatusBadGateway, protocol.ReasonEmitUnreachable, "中枢 ACK 既没有 response 也没有 error")
		return
	}
	writeJSON(w, http.StatusOK, ack.Response)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, protocol.EmitError{Error: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", protocol.EmitContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// targetDeclared 判断 to 是否在 next 里（接受 `name` 或 `name@version`）。
func targetDeclared(next []string, to string) bool {
	for _, ref := range next {
		if ref == to {
			return true
		}
		if !strings.Contains(to, "@") {
			name := ref
			if i := strings.LastIndexByte(ref, '@'); i >= 0 {
				name = ref[:i]
			}
			if name == to {
				return true
			}
		}
	}
	return false
}

// bearerToken 从 Authorization 头取 Bearer 令牌。
func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], protocol.EmitAuthScheme) {
		return ""
	}
	return parts[1]
}

// newRequestID 生成 RPC 关联 id（crypto/rand；不是业务幂等键）。
func newRequestID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// ErrClosed 报告网关已关闭（测试/装配用）。
var ErrClosed = errors.New("emitgw: 网关已关闭")

// Wait 是给装配层用的有界等待（关闭时立刻返回 ErrClosed）。
func (g *Gateway) Wait(ctx context.Context) error {
	<-ctx.Done()
	g.Close()
	return nil
}
