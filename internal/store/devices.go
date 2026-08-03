package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Device 是一台接收设备（Web Push 订阅）。
type Device struct {
	ID            string   `json:"id"`
	UserID        string   `json:"userId"`
	Name          string   `json:"name"`
	Platform      string   `json:"platform"`
	Enabled       bool     `json:"enabled"`
	StatusFilter  string   `json:"statusFilter"` // all|error|success
	Tags          []string `json:"tags"`         // 设备标签（路由用）
	Endpoint      string   `json:"endpoint"`
	P256dh        string   `json:"p256dh"`
	Auth          string   `json:"auth"`
	UserAgent     string   `json:"userAgent"`
	CreatedAt     int64    `json:"createdAt"`
	LastActive    *int64   `json:"lastActive,omitempty"`
	LastDelivered *int64   `json:"lastDelivered,omitempty"`
}

// ListDevices 返回某用户的全部设备（不限 enabled）。
func (d *DB) ListDevices(ctx context.Context, userID string) ([]*Device, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, user_id, name, platform, enabled, status_filter, tags,
		        endpoint, p256dh, auth, user_agent, created_at, last_active, last_delivered
		   FROM devices WHERE user_id = ? ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()

	var out []*Device
	for rows.Next() {
		dev, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dev)
	}
	return out, rows.Err()
}

// ListEnabledDevices 返回某用户所有 enabled 的设备（投递候选集）。
func (d *DB) ListEnabledDevices(ctx context.Context, userID string) ([]*Device, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, user_id, name, platform, enabled, status_filter, tags,
		        endpoint, p256dh, auth, user_agent, created_at, last_active, last_delivered
		   FROM devices WHERE user_id = ? AND enabled = 1 ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list enabled devices: %w", err)
	}
	defer rows.Close()

	var out []*Device
	for rows.Next() {
		dev, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dev)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanDevice(row scanner) (*Device, error) {
	var dev Device
	var enabled int
	var tagsJSON string
	var lastActive, lastDelivered sql.NullInt64
	if err := row.Scan(
		&dev.ID, &dev.UserID, &dev.Name, &dev.Platform, &enabled, &dev.StatusFilter,
		&tagsJSON, &dev.Endpoint, &dev.P256dh, &dev.Auth, &dev.UserAgent,
		&dev.CreatedAt, &lastActive, &lastDelivered,
	); err != nil {
		return nil, fmt.Errorf("scan device: %w", err)
	}
	dev.Enabled = enabled != 0
	if tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &dev.Tags); err != nil {
			return nil, fmt.Errorf("parse device tags: %w", err)
		}
	}
	if dev.Tags == nil {
		dev.Tags = []string{}
	}
	if lastActive.Valid {
		dev.LastActive = &lastActive.Int64
	}
	if lastDelivered.Valid {
		dev.LastDelivered = &lastDelivered.Int64
	}
	return &dev, nil
}

// UpsertDevice 按 endpoint 插入或更新一台设备的订阅信息。
// 已存在（同 endpoint）则更新密钥/标签/UA 并保持 enabled；不存在则新建。
func (d *DB) UpsertDevice(ctx context.Context, dev *Device) error {
	if dev.ID == "" {
		dev.ID = NewDeviceID()
	}
	tagsJSON, err := json.Marshal(dev.Tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	now := Now()
	enabled := 0
	if dev.Enabled {
		enabled = 1
	}
	_, err = d.ExecContext(ctx,
		`INSERT INTO devices
		   (id, user_id, name, platform, enabled, status_filter, tags, endpoint, p256dh, auth, user_agent, created_at, last_active)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(endpoint) DO UPDATE SET
		   p256dh=excluded.p256dh, auth=excluded.auth,
		   user_agent=excluded.user_agent, last_active=excluded.last_active`,
		dev.ID, dev.UserID, dev.Name, dev.Platform, enabled, dev.StatusFilter,
		string(tagsJSON), dev.Endpoint, dev.P256dh, dev.Auth, dev.UserAgent, now, now)
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	return nil
}

// DisableDevice 将一台设备标记为失效（如 Web Push 返回 410 Gone）。
// UpdateDevice 按 id 全字段更新设备的可变配置（name/platform/enabled/status_filter/tags）。
// 与 UpsertDevice 区分：Upsert 是「订阅刷新」（同 endpoint 只更新密钥），
// Update 是「用户改配置」（重命名/开关/过滤/标签）。p256dh/auth/endpoint 不可变。
func (d *DB) UpdateDevice(ctx context.Context, dev *Device) error {
	tagsJSON, err := json.Marshal(dev.Tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	enabled := 0
	if dev.Enabled {
		enabled = 1
	}
	res, err := d.ExecContext(ctx,
		`UPDATE devices SET name=?, platform=?, enabled=?, status_filter=?, tags=?, last_active=?
		 WHERE id=?`,
		dev.Name, dev.Platform, enabled, dev.StatusFilter, string(tagsJSON), Now(), dev.ID)
	if err != nil {
		return fmt.Errorf("update device: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) DisableDevice(ctx context.Context, deviceID string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE devices SET enabled = 0 WHERE id = ?`, deviceID)
	if err != nil {
		return fmt.Errorf("disable device: %w", err)
	}
	_ = res
	return nil
}

// TouchDelivered 更新设备的 last_delivered 时间戳。
func (d *DB) TouchDelivered(ctx context.Context, deviceID string, at int64) error {
	if _, err := d.ExecContext(ctx,
		`UPDATE devices SET last_delivered = ? WHERE id = ?`, at, deviceID); err != nil {
		return fmt.Errorf("touch delivered: %w", err)
	}
	return nil
}
