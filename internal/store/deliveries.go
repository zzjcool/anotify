package store

import (
	"context"
	"fmt"
	"time"
)

// 投递通道
const (
	ChannelWebPush   = "webpush"
	ChannelWebSocket = "websocket"
)

// 投递状态
const (
	DeliveryPending   = "pending"
	DeliverySent      = "sent"
	DeliveryDelivered = "delivered"
	DeliveryAcked     = "acked"
	DeliveryFailed    = "failed"
)

// RecordDelivery 写入一条投递记录（观测性数据源）。
func (d *DB) RecordDelivery(ctx context.Context, messageID, consumerID, channel, status, errMsg string) error {
	now := Now()
	if _, err := d.ExecContext(ctx,
		`INSERT INTO deliveries (message_id, consumer_id, channel, status, error, attempts, created_at, updated_at)
		 VALUES (?,?,?,?,?,1,?,?)`,
		messageID, consumerID, channel, status, errMsg, now, now); err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	return nil
}

// DeliveryRow 是一条投递记录（详情页展示用）。
type DeliveryRow struct {
	MessageID string    `json:"-"`
	DeviceID  string    `json:"deviceId"`  // consumer_id（= 设备 ID / 连接 ID）
	Channel   string    `json:"channel"`   // webpush | websocket
	Status    string    `json:"status"`    // pending|sent|delivered|acked|failed
	Error     string    `json:"error"`     // 失败原因（空 = 无）
	Attempts  int       `json:"attempts"`  // 尝试次数
	CreatedAt time.Time `json:"createdAt"` // 首次尝试时间
	UpdatedAt time.Time `json:"updatedAt"` // 最近更新时间
}

// ListDeliveriesByMessage 列出一条消息的全部投递记录（按时间升序）。
// 不做用户过滤：消息本身已按属主校验过（调用方先 GetMessage）。
func (d *DB) ListDeliveriesByMessage(ctx context.Context, messageID string) ([]*DeliveryRow, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT message_id, consumer_id, channel, status, error, attempts, created_at, updated_at
		FROM deliveries
		WHERE message_id=?
		ORDER BY id ASC`, messageID)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()
	out := []*DeliveryRow{}
	for rows.Next() {
		var r DeliveryRow
		var createdAt, updatedAt int64
		if err := rows.Scan(&r.MessageID, &r.DeviceID, &r.Channel, &r.Status, &r.Error, &r.Attempts, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("list deliveries scan: %w", err)
		}
		r.CreatedAt = time.Unix(createdAt, 0)
		r.UpdatedAt = time.Unix(updatedAt, 0)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	return out, nil
}
