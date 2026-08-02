package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/anotify/anotify/internal/broker"
)

// InsertMessage 把一条消息写入 messages 表（不含进程内广播）。
//
// 注意：生产路径上消息由 SQLiteBroker.Publish 在事务内生成 seq 并插入；
// 本方法是较低层的写入，主要用于测试为 deliveries 提供外键父行，
// 以及未来 broker 之外的工具。seq 由调用方传入（msg.Seq）。
func (d *DB) InsertMessage(ctx context.Context, msg *broker.Message) error {
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

// InsertTestMessage 是 InsertMessage 的便捷包装：给定 id/userID/seq/status，
// 其余字段填合理默认，仅供测试为 deliveries 提供外键父行。
func (d *DB) InsertTestMessage(ctx context.Context, id, userID string, seq int64, status string) error {
	now := time.Now().UTC()
	return d.InsertMessage(ctx, &broker.Message{
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
