package store

import (
	"context"
	"fmt"
)

// ---------- Admin 查询（超管后台用） ----------

// UserCount 返回用户总数（用于「首用户判定」与仪表盘）。
func (d *DB) UserCount(ctx context.Context) (int64, error) {
	var n int64
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("user count: %w", err)
	}
	return n, nil
}

// AdminCount 返回 admin 角色用户数（用于防误删最后一个 admin）。
func (d *DB) AdminCount(ctx context.Context) (int64, error) {
	var n int64
	if err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&n); err != nil {
		return 0, fmt.Errorf("admin count: %w", err)
	}
	return n, nil
}

// UserWithStats 是管理后台用户列表的一行：用户基础信息 + 该用户的实体计数。
type UserWithStats struct {
	User
	MessageCount int64 `json:"messageCount"`
	DeviceCount  int64 `json:"deviceCount"`
	KeyCount     int64 `json:"keyCount"`
	SessionCount int64 `json:"sessionCount"`
}

// ListUsersWithStats 列出全部用户并附各用户的实体计数（管理后台用户页用）。
// 按 created_at ASC 排序（首注册用户排最前，便于识别超管来源）。
func (d *DB) ListUsersWithStats(ctx context.Context) ([]*UserWithStats, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT u.id, u.username, u.display_name, u.role, u.disabled, u.created_at,
		        (SELECT COUNT(*) FROM messages  m WHERE m.user_id = u.id),
		        (SELECT COUNT(*) FROM devices   d WHERE d.user_id = u.id),
		        (SELECT COUNT(*) FROM api_keys  k WHERE k.user_id = u.id),
		        (SELECT COUNT(*) FROM sessions  s WHERE s.user_id = u.id)
		 FROM users u
		 ORDER BY u.created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list users with stats: %w", err)
	}
	defer rows.Close()
	var out []*UserWithStats
	for rows.Next() {
		var u UserWithStats
		var disabled int
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &disabled, &u.CreatedAt,
			&u.MessageCount, &u.DeviceCount, &u.KeyCount, &u.SessionCount); err != nil {
			return nil, fmt.Errorf("scan user with stats: %w", err)
		}
		u.Disabled = disabled != 0
		out = append(out, &u)
	}
	return out, rows.Err()
}

// SystemStats 是管理后台的系统级总览统计。
type SystemStats struct {
	UserCount       int64        `json:"userCount"`       // 用户总数
	AdminCount      int64        `json:"adminCount"`      // 超管数
	ActiveUserCount int64        `json:"activeUserCount"` // 近 7 天有消息上报的用户数
	MessageCount    int64        `json:"messageCount"`    // 消息总数
	TodayMessages   int64        `json:"todayMessages"`   // 今日消息数
	DeviceCount     int64        `json:"deviceCount"`     // 接收设备总数
	EnabledDevices  int64        `json:"enabledDevices"`  // 启用设备数
	KeyCount        int64        `json:"keyCount"`        // API Key 总数
	SessionCount    int64        `json:"sessionCount"`    // 活跃会话数
	DeliveredCount  int64        `json:"deliveredCount"`  // 已送达投递数（sent/delivered/acked）
	DeliveryTotal   int64        `json:"deliveryTotal"`   // 投递尝试总数
	Daily           []DayCount   `json:"daily"`           // 近 N 天每天全站消息数
	TopUsers        []UserMetric `json:"topUsers"`        // 消息数 Top N 用户
}

// UserMetric 是 TopN 用户指标（仪表盘 Top 活跃用户）。
type UserMetric struct {
	UserID       string `json:"userId"`
	Username     string `json:"username"`
	DisplayName  string `json:"displayName"`
	MessageCount int64  `json:"messageCount"`
	DeviceCount  int64  `json:"deviceCount"`
}

// SystemStats 计算全站总览统计。sinceSec 为热力图起始（unixepoch 秒）。
func (d *DB) SystemStats(ctx context.Context, sinceSec int64) (*SystemStats, error) {
	s := &SystemStats{Daily: []DayCount{}, TopUsers: []UserMetric{}}

	queries := []struct {
		dst  *int64
		sql  string
		args []any
		name string
	}{
		{&s.UserCount, `SELECT COUNT(*) FROM users`, nil, "user count"},
		{&s.AdminCount, `SELECT COUNT(*) FROM users WHERE role = ?`, []any{RoleAdmin}, "admin count"},
		{&s.ActiveUserCount,
			`SELECT COUNT(DISTINCT user_id) FROM messages WHERE created_at >= strftime('%s','now','-7 days')`,
			nil, "active user count"},
		{&s.MessageCount, `SELECT COUNT(*) FROM messages`, nil, "message count"},
		{&s.TodayMessages,
			`SELECT COUNT(*) FROM messages WHERE created_at >= strftime('%s','now','start of day')`,
			nil, "today messages"},
		{&s.DeviceCount, `SELECT COUNT(*) FROM devices`, nil, "device count"},
		{&s.EnabledDevices, `SELECT COUNT(*) FROM devices WHERE enabled = 1`, nil, "enabled devices"},
		{&s.KeyCount, `SELECT COUNT(*) FROM api_keys`, nil, "key count"},
		{&s.SessionCount, `SELECT COUNT(*) FROM sessions WHERE expires_at > strftime('%s','now')`, nil, "session count"},
	}
	for _, q := range queries {
		if err := d.QueryRowContext(ctx, q.sql, q.args...).Scan(q.dst); err != nil {
			return nil, fmt.Errorf("system stats %s: %w", q.name, err)
		}
	}

	// 投递统计（sent/delivered/acked 视为成功送达）
	if err := d.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN status IN ('sent','delivered','acked') THEN 1 ELSE 0 END),0),
		   COUNT(*)
		 FROM deliveries`).Scan(&s.DeliveredCount, &s.DeliveryTotal); err != nil {
		return nil, fmt.Errorf("system stats delivered: %w", err)
	}

	// 近 N 天每天全站消息数
	rows, err := d.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', created_at, 'unixepoch') AS day, COUNT(*)
		 FROM messages
		 WHERE created_at >= ?
		 GROUP BY day ORDER BY day`, sinceSec)
	if err != nil {
		return nil, fmt.Errorf("system stats daily: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dc DayCount
		if err := rows.Scan(&dc.Day, &dc.Count); err != nil {
			return nil, fmt.Errorf("system stats daily scan: %w", err)
		}
		s.Daily = append(s.Daily, dc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("system stats daily rows: %w", err)
	}

	// Top 10 活跃用户（按消息数降序）
	trows, err := d.QueryContext(ctx,
		`SELECT u.id, u.username, u.display_name,
		        (SELECT COUNT(*) FROM messages m WHERE m.user_id = u.id),
		        (SELECT COUNT(*) FROM devices  d WHERE d.user_id = u.id)
		 FROM users u
		 ORDER BY (SELECT COUNT(*) FROM messages m WHERE m.user_id = u.id) DESC
		 LIMIT 10`)
	if err != nil {
		return nil, fmt.Errorf("system stats top users: %w", err)
	}
	defer trows.Close()
	for trows.Next() {
		var u UserMetric
		if err := trows.Scan(&u.UserID, &u.Username, &u.DisplayName, &u.MessageCount, &u.DeviceCount); err != nil {
			return nil, fmt.Errorf("system stats top users scan: %w", err)
		}
		s.TopUsers = append(s.TopUsers, u)
	}
	if err := trows.Err(); err != nil {
		return nil, fmt.Errorf("system stats top users rows: %w", err)
	}

	return s, nil
}

// UpdateUserRole 更新某用户的角色（admin/member）。返回受影响行数（0=不存在）。
func (d *DB) UpdateUserRole(ctx context.Context, id, role string) (int64, error) {
	res, err := d.ExecContext(ctx,
		`UPDATE users SET role = ? WHERE id = ?`, role, id)
	if err != nil {
		return 0, fmt.Errorf("update user role: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UpdateUserDisabled 设置某用户的禁用状态。返回受影响行数（0=不存在）。
func (d *DB) UpdateUserDisabled(ctx context.Context, id string, disabled bool) (int64, error) {
	v := 0
	if disabled {
		v = 1
	}
	res, err := d.ExecContext(ctx,
		`UPDATE users SET disabled = ? WHERE id = ?`, v, id)
	if err != nil {
		return 0, fmt.Errorf("update user disabled: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AdminGlobalMessages 是管理后台全局消息流的一行（跨用户）。
type AdminGlobalMessage struct {
	ID        string `json:"id"`
	UserID    string `json:"userId"`
	Username  string `json:"username"`
	Seq       int64  `json:"seq"`
	Title     string `json:"title"`
	AgentState string `json:"agentState"`
	CreatedAt int64  `json:"createdAt"`
}

// ListGlobalMessages 列出全站最近消息（跨用户，按 created_at DESC）。limit 上限 500。
func (d *DB) ListGlobalMessages(ctx context.Context, limit int) ([]*AdminGlobalMessage, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := d.QueryContext(ctx,
		`SELECT m.id, m.user_id, u.username, m.seq, m.title, m.agent_state, m.created_at
		 FROM messages m
		 LEFT JOIN users u ON u.id = m.user_id
		 ORDER BY m.created_at DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list global messages: %w", err)
	}
	defer rows.Close()
	var out []*AdminGlobalMessage
	for rows.Next() {
		var m AdminGlobalMessage
		if err := rows.Scan(&m.ID, &m.UserID, &m.Username, &m.Seq, &m.Title, &m.AgentState, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan global message: %w", err)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// AdminSessionRow 是管理后台活跃会话总览的一行。
type AdminSessionRow struct {
	ID         string `json:"id"`
	UserID     string `json:"userId"`
	Username   string `json:"username"`
	DeviceName string `json:"deviceName"`
	CreatedAt  int64  `json:"createdAt"`
	ExpiresAt  int64  `json:"expiresAt"`
	LastSeen   int64  `json:"lastSeen"`
}

// ListAllSessions 列出全站活跃会话（expires_at > now），按 last_seen DESC。
func (d *DB) ListAllSessions(ctx context.Context) ([]*AdminSessionRow, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT s.id, s.user_id, u.username, s.device_name, s.created_at, s.expires_at, s.last_seen
		 FROM sessions s
		 LEFT JOIN users u ON u.id = s.user_id
		 WHERE s.expires_at > strftime('%s','now')
		 ORDER BY s.last_seen DESC
		 LIMIT 500`)
	if err != nil {
		return nil, fmt.Errorf("list all sessions: %w", err)
	}
	defer rows.Close()
	var out []*AdminSessionRow
	for rows.Next() {
		var s AdminSessionRow
		if err := rows.Scan(&s.ID, &s.UserID, &s.Username, &s.DeviceName, &s.CreatedAt, &s.ExpiresAt, &s.LastSeen); err != nil {
			return nil, fmt.Errorf("scan admin session: %w", err)
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}
