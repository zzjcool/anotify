// Package ws 实现 WebSocket 接收通道（Broker 的消费者1）。
//
// 帧协议（JSON over WebSocket）：
//
//	服务端→客户端: hello / notification / replay_end / pong / subscribed / error / bye
//	客户端→服务端: subscribe / unsubscribe / ack / ping / resume
//
// 断线重连：客户端用 Last-Event-Id 头或 resume 帧携带 resume_token，
// 服务端 broker.Replay 补漏 → replay_end → 进入实时流。
package ws

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
	EventID string   `json:"event_id,omitempty"`
	Seq     int64    `json:"seq,omitempty"`
	Title   string   `json:"title,omitempty"`
	Body    string   `json:"body,omitempty"`
	Status  string   `json:"status,omitempty"`
	URL     string   `json:"url,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	SentAt  string   `json:"sent_at,omitempty"`
	TTLSec  int      `json:"ttl_sec,omitempty"`

	// subscribe / unsubscribe / ack / resume
	EventIDs []string `json:"event_ids,omitempty"`

	// error
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`

	// bye
	Reason string `json:"reason,omitempty"`
}

// notificationFrame 把一条 broker.Message 转成下行 notification 帧。
func notificationFrame(msg *broker.Message) *Frame {
	return &Frame{
		Type:    FrameNotification,
		EventID: msg.ID,
		Seq:     msg.Seq,
		Title:   msg.Title,
		Body:    msg.Body,
		Status:  msg.Status,
		URL:     msg.Link,
		Tags:    msg.DeviceTags,
		SentAt:  msg.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		TTLSec:  msg.TTLSeconds,
	}
}

func errorFrame(code, message string) *Frame {
	return &Frame{Type: FrameError, Code: code, Message: message}
}
