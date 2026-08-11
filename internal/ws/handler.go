package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/broker"
	"github.com/zzjcool/anotify/internal/store"
)

// resumeTokenPrefix 把 seq 编码进 resume token / Last-Event-Id。
// 形式 "evt_<seq>"，解析时取数字部分。
const resumeTokenPrefix = "evt_"

// Handler 处理 GET /v1/stream 的 WebSocket 升级与连接生命周期。
type Handler struct {
	Broker broker.Broker
	Keys   authn.KeyValidator

	// HeartbeatSec 下发给客户端的心跳间隔（秒）；客户端应在此周期内 ping。
	HeartbeatSec int
	// AcceptOptions 可覆盖 WS 升级选项（如 OriginPatterns）；为 nil 用默认（含 InsecureSkipVerify，自托管场景）。
	AcceptOptions *websocket.AcceptOptions
}

// NewHandler 构造一个带合理默认值的 Handler。
func NewHandler(b broker.Broker, keys authn.KeyValidator) *Handler {
	return &Handler{
		Broker:       b,
		Keys:         keys,
		HeartbeatSec: 30,
		AcceptOptions: &websocket.AcceptOptions{
			// 自托管场景：前端可能从不同源（CDN 域名）连接，跳过 origin 校验。
			// 鉴权由 Bearer Key 保证。
			InsecureSkipVerify: true,
		},
	}
}

func (h *Handler) heartbeatSec() int {
	if h.HeartbeatSec > 0 {
		return h.HeartbeatSec
	}
	return 30
}

// ServeHTTP 升级并驱动一条 WS 连接。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID, err := authn.Authenticate(r, h.Keys, authn.ScopeNotifyReceive)
	if err != nil {
		code := http.StatusUnauthorized
		if errors.Is(err, authn.ErrForbidden) {
			code = http.StatusForbidden
		}
		slog.Warn("ws auth failed",
			"event", "ws.auth.failed",
			"ip", r.RemoteAddr,
			"status", code,
		)
		http.Error(w, `{"error":"`+err.Error()+`"}`, code)
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

	// 解析断线续传位置：优先 Last-Event-Id 头
	sinceSeq := parseResumeSeq(r.Header.Get("Last-Event-Id"))

	sess := &session{
		handler:  h,
		conn:     conn,
		userID:   userID,
		connID:   store.NewEventID(),
		sinceSeq: sinceSeq,
		tags:     nil, // nil = 全订阅
	}
	sess.run(r.Context())
}

// parseResumeSeq 解析 "evt_<seq>" 或纯数字，返回 seq；失败返回 0。
func parseResumeSeq(token string) int64 {
	if token == "" {
		return 0
	}
	s := token
	if len(s) > len(resumeTokenPrefix) && s[:len(resumeTokenPrefix)] == resumeTokenPrefix {
		s = s[len(resumeTokenPrefix):]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// session 表示一条活跃的 WS 连接。
type session struct {
	handler  *Handler
	conn     *websocket.Conn
	userID   string
	connID   string
	sinceSeq int64

	mu     sync.Mutex // 保护 tags
	tags   []string   // 订阅标签；nil/空 = 全订阅
	lastID string     // 最近下发的消息 id（resume token）

	writeMu sync.Mutex // 串行化写帧

	activityMu   sync.Mutex // 保护 lastActivity
	lastActivity time.Time  // 最近一次收到客户端帧的时间（心跳判活依据）
}

// touchActivity 记录一次客户端活动（收到任意上行帧）。
func (s *session) touchActivity() {
	s.activityMu.Lock()
	s.lastActivity = time.Now()
	s.activityMu.Unlock()
}

// lastActivityAt 返回最近一次客户端活动时间。
func (s *session) lastActivityAt() time.Time {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	return s.lastActivity
}

func (s *session) setTags(tags []string) {
	s.mu.Lock()
	s.tags = tags
	s.mu.Unlock()
}

func (s *session) getTags() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tags
}

// writeJSON 写一条下行帧。
func (s *session) writeJSON(ctx context.Context, f *Frame) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, raw)
}

// run 驱动连接：hello → (replay) → 实时流 + 读循环。
func (s *session) run(ctx context.Context) {
	defer s.conn.Close(websocket.StatusNormalClosure, "")

	// 心跳超时：2×heartbeatSec 内未收到任何帧则关闭。
	// 不用 conn.CloseRead：我们有自己的 readLoop 持续读帧并处理 ping/pong，
	// 对端失活会由读错误或应用级心跳超时暴露。
	hb := time.Duration(s.handler.heartbeatSec()) * time.Second
	s.conn.SetReadLimit(1 << 20)

	// 1) hello
	hello := &Frame{
		Type:         FrameHello,
		ConnID:       s.connID,
		Protocol:     1,
		HeartbeatSec: s.handler.heartbeatSec(),
		ResumeToken:  resumeTokenPrefix + strconv.FormatInt(s.sinceSeq, 10),
	}
	if err := s.writeJSON(ctx, hello); err != nil {
		return
	}

	// 2) 断线续传：先 Replay 补漏
	if s.sinceSeq > 0 {
		msgs, err := s.handler.Broker.Replay(ctx, s.userID, s.sinceSeq, 1000)
		if err == nil {
			for _, m := range msgs {
				if err := s.writeJSON(ctx, notificationFrame(m)); err != nil {
					return
				}
			}
		}
		if err := s.writeJSON(ctx, &Frame{Type: FrameReplayEnd}); err != nil {
			return
		}
	}

	// 3) 订阅实时流
	sub, err := s.handler.Broker.Subscribe(ctx, s.userID, nil)
	if err != nil {
		_ = s.writeJSON(ctx, errorFrame("subscribe_failed", err.Error()))
		return
	}
	defer sub.Close()

	// 读循环（goroutine）：处理上行帧
	s.touchActivity()
	readErr := make(chan error, 1)
	go s.readLoop(ctx, readErr)

	// 心跳巡检：周期性检查「距上次客户端活动」是否超过 2×heartbeatSec。
	// 注意：只有客户端活动（读帧/ping）能续命，服务端下行消息不重置计时，
	// 否则持续推送会掩盖一个不再 ping 的死连接。
	ticker := time.NewTicker(hb)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-readErr:
			_ = err
			return
		case <-ticker.C:
			if time.Since(s.lastActivityAt()) > 2*hb {
				_ = s.writeJSON(ctx, &Frame{Type: FrameBye, Reason: "heartbeat timeout"})
				s.conn.Close(websocket.StatusPolicyViolation, "heartbeat timeout")
				return
			}
		case msg, ok := <-sub.C():
			if !ok {
				return
			}
			// 按当前订阅标签过滤
			if !matchTags(s.getTags(), msg.DeviceTags) {
				continue
			}
			if err := s.writeJSON(ctx, notificationFrame(msg)); err != nil {
				return
			}
		}
	}
}

// readLoop 读上行帧并分发处理；任何活动都会通过 activity 通知主循环续命。
func (s *session) readLoop(ctx context.Context, done chan<- error) {
	for {
		_, raw, err := s.conn.Read(ctx)
		if err != nil {
			done <- err
			return
		}
		s.touchActivity()
		var f Frame
		if err := json.Unmarshal(raw, &f); err != nil {
			_ = s.writeJSON(ctx, errorFrame("bad_frame", "invalid JSON frame"))
			continue
		}
		s.handleFrame(ctx, &f)
	}
}

// handleFrame 处理一条上行帧。
func (s *session) handleFrame(ctx context.Context, f *Frame) {
	switch f.Type {
	case FramePing:
		_ = s.writeJSON(ctx, &Frame{Type: FramePong})
	case FrameSubscribe:
		s.setTags(normalizeTags(f.Tags))
		_ = s.writeJSON(ctx, &Frame{
			Type:           FrameSubscribed,
			SubscribedTags: s.getTags(),
			ResumeToken:    resumeTokenPrefix + strconv.FormatInt(s.lastSeq(), 10),
		})
	case FrameUnsubscribe:
		s.setTags(nil)
		_ = s.writeJSON(ctx, &Frame{Type: FrameSubscribed, SubscribedTags: nil})
	case FrameAck:
		// ack 一批 event_ids：把每个 id 对应的 seq 推进 high-water。
		for _, id := range f.EventIDs {
			if seq := parseResumeSeq(id); seq > 0 {
				_ = s.handler.Broker.Ack(ctx, s.connID, s.userID, seq)
			}
		}
	case FrameResume:
		// 客户端在首帧请求断线续传
		if seq := parseResumeSeq(f.ResumeToken); seq > 0 {
			s.sinceSeq = seq
		}
	default:
		_ = s.writeJSON(ctx, errorFrame("unknown_type", "unknown frame type: "+f.Type))
	}
}

func (s *session) lastSeq() int64 { return s.sinceSeq }

// matchTags WS 侧订阅过滤：sub（客户端订阅标签）为空 = 全订阅；
// 否则消息 DeviceTags 与 sub 有交集才下发。
func matchTags(sub, msgTags []string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(msgTags) == 0 {
		// 广播消息发给所有订阅者
		return true
	}
	set := make(map[string]struct{}, len(sub))
	for _, t := range sub {
		set[t] = struct{}{}
	}
	for _, t := range msgTags {
		if _, ok := set[t]; ok {
			return true
		}
	}
	return false
}

// normalizeTags WS 订阅标签归一化（trim/去空/去重）。
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
