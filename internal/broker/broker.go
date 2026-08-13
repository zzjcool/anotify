// Package broker 定义 Anotify 的消息队列抽象。
//
// 抽象边界划在 Publish/Subscribe/Ack/Replay 上，不在 HTTP 或 WS 处理器上，
// 这样 WS 派发器与 Web Push 派发器都只是 Broker 的两个消费者。
// Phase 1 用 SQLiteBroker（进程内广播负责实时，DB 表负责回放/历史/崩溃恢复）；
// 未来可平滑替换为 NATS / Redis Streams / Kafka，业务代码零改动。
package broker

import (
	"context"
	"time"
)

// Agent 生命周期状态（投递给设备时按设备 event_scope 过滤）
const (
	AgentStateWorking     = "working"
	AgentStateBlocked     = "blocked"
	AgentStateDone        = "done"
	AgentStateInterrupted = "interrupted"
	AgentStateError       = "error"
)

// IsTerminal 判定某 agentState 是否为终态（done/interrupted/error）。
// 终态事件会被 event_scope=final 的接收端放行；非终态只被 all 放行。
func IsTerminal(state string) bool {
	return state == AgentStateDone || state == AgentStateInterrupted || state == AgentStateError
}

// Message 是一条通知消息。
// JSON tag 统一 camelCase，与 api/openapi.yaml 契约一致（/v1/notifications、WS notification 帧）。
type Message struct {
	ID         string    `json:"id"`                 // 消息 ID（KSUID，如 "ntf_01J8XA…"）
	UserID     string    `json:"userId"`             // 所属用户（分区键）
	Seq        int64     `json:"seq"`                // 每用户单调递增序号（= replay 的 offset）
	Title      string    `json:"title"`              // 标题
	AgentState string    `json:"agentState"`         // working|blocked|done|interrupted|error
	Severity   string    `json:"severity,omitempty"` // 呈现语气 info|warning|error（缺省由 agentState 派生）
	Kind       string    `json:"kind,omitempty"`     // task|reply|steer（阶段二用，默认 task）
	ReplyTo    string    `json:"replyTo,omitempty"`  // kind=reply 时指目标消息 id
	Body       string    `json:"body"`               // 正文
	Link       string    `json:"link"`               // 深链
	DeviceTags []string  `json:"deviceTags"`         // 路由键（= topic）；空 = 广播
	Priority   string    `json:"priority"`           // 优先级
	TTLSeconds int       `json:"ttlSeconds"`         // 有效期（秒）
	Payload    []byte    `json:"payload"`            // 完整 JSON（含 agentId/sessionId/model 等未规范化字段）
	CreatedAt  time.Time `json:"createdAt"`          // 创建时间
	ExpiresAt  time.Time `json:"expiresAt"`          // 可投递截止
}

// Subscription 是 Subscribe 返回的句柄，用于接收某用户的实时消息流。
type Subscription interface {
	// C 返回消息通道；至少一次投递（可能重复，消费方需按 Seq 去重）。
	C() <-chan *Message
	// Close 取消订阅并释放资源。
	Close() error
}

// Broker 是消息队列抽象接口。
type Broker interface {
	// Publish 生产：Agent 上报 → 入队（seq 在事务内生成）。
	Publish(ctx context.Context, msg *Message) error

	// Subscribe 消费：订阅某用户的通知流；tags 为空 = 全订阅。
	Subscribe(ctx context.Context, userID string, tags []string) (Subscription, error)

	// Ack 确认：标记消息已被某消费者处理（at-least-once，high-water 只前进）。
	Ack(ctx context.Context, consumerID, userID string, seq int64) error

	// Replay 重放：断线续传，返回 sinceSeq 之后的消息（按 seq 升序）。
	Replay(ctx context.Context, userID string, sinceSeq int64, limit int) ([]*Message, error)

	// Close 关闭并释放资源。
	Close() error
}
