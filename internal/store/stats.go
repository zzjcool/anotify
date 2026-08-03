package store

import (
	"context"
	"fmt"
)

// DayCount 是某一天的通知数量（热力图用）。
type DayCount struct {
	Day   string `json:"day"`   // YYYY-MM-DD（本地时区由调用方解释，这里用 UTC 日期）
	Count int    `json:"count"` // 当天通知数
}

// Stats 是用户工作台的真实统计。
type Stats struct {
	Total         int        `json:"total"`         // 通知总数
	Today         int        `json:"today"`         // 今日通知数
	Delivered     int        `json:"delivered"`     // 已送达次数（deliveries status in sent/delivered/acked）
	DeliveryTotal int        `json:"deliveryTotal"` // 投递尝试总数（非 failed 之外的也算，分母用全部）
	DeviceCount   int        `json:"deviceCount"`   // 已启用接收设备数
	Daily         []DayCount `json:"daily"`         // 近 N 天每天通知数（热力图）
}

// MessageStats 计算某用户的真实统计。sinceSec 为热力图起始（unixepoch 秒）。
func (d *DB) MessageStats(ctx context.Context, userID string, sinceSec int64) (*Stats, error) {
	s := &Stats{Daily: []DayCount{}}

	// 总数
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE user_id=?`, userID).Scan(&s.Total); err != nil {
		return nil, fmt.Errorf("stats total: %w", err)
	}

	// 今日（UTC 0 点起）
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE user_id=? AND created_at >=
		   (strftime('%s','now','start of day'))`, userID).Scan(&s.Today); err != nil {
		return nil, fmt.Errorf("stats today: %w", err)
	}

	// 送达统计（deliveries 关联该用户的消息）
	if err := d.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN dl.status IN ('sent','delivered','acked') THEN 1 ELSE 0 END),0),
		   COUNT(*)
		 FROM deliveries dl
		 JOIN messages m ON m.id = dl.message_id
		 WHERE m.user_id=?`, userID).Scan(&s.Delivered, &s.DeliveryTotal); err != nil {
		return nil, fmt.Errorf("stats delivered: %w", err)
	}

	// 启用设备数
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices WHERE user_id=? AND enabled=1`, userID).Scan(&s.DeviceCount); err != nil {
		return nil, fmt.Errorf("stats devices: %w", err)
	}

	// 近 N 天每天数量（热力图）
	rows, err := d.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', created_at, 'unixepoch') AS day, COUNT(*)
		 FROM messages
		 WHERE user_id=? AND created_at >= ?
		 GROUP BY day ORDER BY day`, userID, sinceSec)
	if err != nil {
		return nil, fmt.Errorf("stats daily: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dc DayCount
		if err := rows.Scan(&dc.Day, &dc.Count); err != nil {
			return nil, fmt.Errorf("stats daily scan: %w", err)
		}
		s.Daily = append(s.Daily, dc)
	}
	return s, rows.Err()
}
