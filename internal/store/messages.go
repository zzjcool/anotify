package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// MessageRow 是 messages 表的一行（store 本地类型，避免 store 依赖 broker 造成 import 循环）。
// 生产路径上消息由 broker 层在事务内生成 seq 后调用 InsertMessage 落库；
// broker.Message 与 MessageRow 字段一一对应，由 broker 层做适配转换。
type MessageRow struct {
	ID         string
	UserID     string
	Seq        int64
	Title      string
	Status     string
	Body       string
	Link       string
	DeviceTags []string
	Priority   string
	TTLSeconds int
	Payload    []byte
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// InsertMessage 把一条消息写入 messages 表（不含进程内广播）。
//
// 注意：生产路径上消息由 SQLiteBroker.Publish 在事务内生成 seq 并插入；
// 本方法是较低层的写入，主要用于测试为 deliveries 提供外键父行，
// 以及未来 broker 之外的工具。seq 由调用方传入（msg.Seq）。
func (d *DB) InsertMessage(ctx context.Context, msg *MessageRow) error {
	tags, err := json.Marshal(msg.DeviceTags)
	if err != nil {
		return fmt.Errorf("marshal device tags: %w", err)
	}
	payload := msg.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	created := msg.CreatedAt.Unix()
	if created == 0 {
		created = Now()
	}
	ttl := msg.TTLSeconds
	if ttl <= 0 {
		ttl = 86400
	}
	expires := msg.ExpiresAt.Unix()
	if expires == 0 {
		expires = created + int64(ttl)
	}
	priority := msg.Priority
	if priority == "" {
		priority = "normal"
	}
	id := msg.ID
	if id == "" {
		id = NewMessageID()
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO messages
		   (id, user_id, seq, title, status, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, msg.UserID, msg.Seq, msg.Title, msg.Status, msg.Body, msg.Link,
		string(tags), priority, ttl, string(payload), created, expires); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// GetMessage 按 ID 取一条消息（仅限属主 userID，防越权读他人消息）。
// 未命中返回 (nil, nil)，由调用方翻译成 404。
func (d *DB) GetMessage(ctx context.Context, userID, messageID string) (*MessageRow, error) {
	var m MessageRow
	var tags, payload string
	var createdAt, expiresAt int64
	err := d.QueryRowContext(ctx, `
		SELECT id, user_id, seq, title, status, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at
		FROM messages
		WHERE id=? AND user_id=?`, messageID, userID).Scan(
		&m.ID, &m.UserID, &m.Seq, &m.Title, &m.Status, &m.Body, &m.Link,
		&tags, &m.Priority, &m.TTLSeconds, &payload, &createdAt, &expiresAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get message: %w", err)
	}
	if err := json.Unmarshal([]byte(tags), &m.DeviceTags); err != nil {
		return nil, fmt.Errorf("get message: 解析 device_tags: %w", err)
	}
	m.Payload = []byte(payload)
	m.CreatedAt = time.Unix(createdAt, 0)
	m.ExpiresAt = time.Unix(expiresAt, 0)
	return &m, nil
}

// InsertTestMessage 是 InsertMessage 的便捷包装：给定 id/userID/seq/status，
// 其余字段填合理默认，仅供测试为 deliveries 提供外键父行。
func (d *DB) InsertTestMessage(ctx context.Context, id, userID string, seq int64, status string) error {
	now := time.Now().UTC()
	return d.InsertMessage(ctx, &MessageRow{
		ID:         id,
		UserID:     userID,
		Seq:        seq,
		Title:      id,
		Status:     status,
		Body:       "",
		DeviceTags: []string{},
		Priority:   "normal",
		TTLSeconds: 86400,
		Payload:    []byte("{}"),
		CreatedAt:  now,
		ExpiresAt:  now.Add(24 * time.Hour),
	})
}
