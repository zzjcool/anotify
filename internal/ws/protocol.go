// Package ws 实现 WebSocket 通道，有两条互不相干的通路：
//
//	节点协议（事件中心，本期主线）：GET /v1/node/stream → node_handler.go，
//	  九帧定义在 node_protocol.go（hello/heartbeat/ack/progress/result 上行，
//	  welcome/assign/cancel/error 下行）。
//
//	旧浏览器端协议（Deprecated，待 #7 下线）：GET /v1/stream → handler.go + 本文件，
//	  帧是 notification/ack/subscribe，面向「浏览器收通知」的旧模型。
package ws

// 本文件是**旧浏览器端**的 WS 帧定义（notification/ack/subscribe），
// 与事件中心的节点协议（node_protocol.go 的九帧）**没有关系**。
//
// Deprecated: 等 #7 下线 /v1/stream 与前端页面后，本文件一并删除。
import "github.com/zzjcool/anotify/internal/broker"

// 帧类型。
const (
	// 下行（server→client）
	FrameHello        = "hello"
	FrameNotification = "notification"
	FrameReplayEnd    = "replay_end"
	FramePong         = "pong"
	FrameSubscribed   = "subscribed"
	FrameError        = "error"
	FrameBye          = "bye"

	// 上行（client→server）
	FrameSubscribe   = "subscribe"
	FrameUnsubscribe = "unsubscribe"
	FrameAck         = "ack"
	FramePing        = "ping"
	FrameResume      = "resume"
)

// Frame 是一条通用帧。下行与上行共用此结构，按 Type 取用相关字段。
type Frame struct {
	Type string `json:"type"`

	// hello
	ConnID         string   `json:"conn_id,omitempty"`
	Protocol       int      `json:"protocol,omitempty"`
	HeartbeatSec   int      `json:"heartbeat_sec,omitempty"`
	ResumeToken    string   `json:"resume_token,omitempty"`
	SubscribedTags []string `json:"subscribed_tags,omitempty"`

	// notification
	EventID    string   `json:"event_id,omitempty"`
	Seq        int64    `json:"seq,omitempty"`
	Title      string   `json:"title,omitempty"`
	Body       string   `json:"body,omitempty"`
	AgentState string   `json:"agentState,omitempty"`
	URL        string   `json:"url,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	SentAt     string   `json:"sent_at,omitempty"`
	TTLSec     int      `json:"ttl_sec,omitempty"`

	// subscribe / unsubscribe / ack / resume
	EventIDs   []string `json:"event_ids,omitempty"`
	EventScope string   `json:"event_scope,omitempty"` // subscribe 帧携带；subscribed 帧回显

	// error
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`

	// bye
	Reason string `json:"reason,omitempty"`
}

// notificationFrame 把一条 broker.Message 转成下行 notification 帧。
func notificationFrame(msg *broker.Message) *Frame {
	return &Frame{
		Type:       FrameNotification,
		EventID:    msg.ID,
		Seq:        msg.Seq,
		Title:      msg.Title,
		Body:       msg.Body,
		AgentState: msg.AgentState,
		URL:        msg.Link,
		Tags:       msg.DeviceTags,
		SentAt:     msg.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		TTLSec:     msg.TTLSeconds,
	}
}

func errorFrame(code, message string) *Frame {
	return &Frame{Type: FrameError, Code: code, Message: message}
}
