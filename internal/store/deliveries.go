package store

import (
	"context"
	"fmt"
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
