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
	// #80 后口径（messages → events/tasks）：
	// Total/Today/Daily 是**事件**数；Delivered/DeliveryTotal 是**任务**终态统计。
	// 字段名与 JSON key 保持不变，避免破坏前端契约；语义变了但形状没变。
	Total         int        `json:"total"`         // 事件总数
	Today         int        `json:"today"`         // 今日事件数
	Delivered     int        `json:"delivered"`     // 成功任务数
	DeliveryTotal int        `json:"deliveryTotal"` // 已结束任务数（分母）
	DeviceCount   int        `json:"deviceCount"`   // 已启用推送设备数
	Daily         []DayCount `json:"daily"`         // 近 N 天每天事件数（热力图）
}

// MessageStats 计算**某个命名空间**的真实统计。sinceSec 为热力图起始（unixepoch 秒）。
//
// #162：事件与任务都属于命名空间，所以统计的作用域是 namespaceID（不是 org）——
// `/v1/stats` 缺 namespaceId 一律 400，绝不回退默认命名空间（契约 §6.2）。
// userID 只用于「该用户启用的推送设备数」（设备仍按用户维度）。
//
// 口径（#80 事件中心化后的定义）：
//
//	Total     = 该命名空间的事件总数（events 表）
//	Today     = 今日事件数（UTC 0 点起）
//	Delivered/DeliveryTotal = 任务成功数 / 已结束任务数（tasks 的终态统计）
//	DeviceCount = 该用户启用的推送设备数（devices 仍按 user 维度）
//	Daily     = 近 N 天每天事件数（热力图）
//
// 为什么从 messages 改为 events/tasks：messages 是旧"通知历史"模型，
// 事件中心里"发生了什么"是 events、"做完没有"是 tasks。
// 数值会与旧版本不同（历史通知数 ≠ 事件数），这是口径变化不是 bug。
func (d *DB) MessageStats(ctx context.Context, namespaceID, userID string, sinceSec int64) (*Stats, error) {
	s := &Stats{Daily: []DayCount{}}
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}

	// 总数
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE namespace_id=?`, namespaceID).Scan(&s.Total); err != nil {
		return nil, fmt.Errorf("stats total: %w", err)
	}

	// 今日（UTC 0 点起）
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE namespace_id=? AND created_at >=
		   (strftime('%s','now','start of day'))`, namespaceID).Scan(&s.Today); err != nil {
		return nil, fmt.Errorf("stats today: %w", err)
	}

	// 成功/完成比（口径：终态任务里 succeeded 的比例）
	if err := d.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END),0),
		   COUNT(*)
		 FROM tasks
		 WHERE namespace_id=? AND status IN ('succeeded','failed','canceled','abandoned')`,
		namespaceID).Scan(&s.Delivered, &s.DeliveryTotal); err != nil {
		return nil, fmt.Errorf("stats tasks: %w", err)
	}

	// 启用设备数（推送订阅仍按用户维度）
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices WHERE user_id=? AND enabled=1`, userID).Scan(&s.DeviceCount); err != nil {
		return nil, fmt.Errorf("stats devices: %w", err)
	}

	// 近 N 天每天数量（热力图）
	rows, err := d.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', created_at, 'unixepoch') AS day, COUNT(*)
		 FROM events
		 WHERE namespace_id=? AND created_at >= ?
		 GROUP BY day ORDER BY day`, namespaceID, sinceSec)
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
