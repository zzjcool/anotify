package store

import (
	"context"
	"fmt"
)

// InsertUser 插入一个用户（外键父行；设备/会话/消息都引用它）。
// 若 id 为空则生成 usr_ 前缀 ID。
func (d *DB) InsertUser(ctx context.Context, id, username, displayName string) (string, error) {
	if id == "" {
		id = NewUserID()
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO users (id, username, display_name, created_at) VALUES (?,?,?,?)`,
		id, username, displayName, Now()); err != nil {
		return "", fmt.Errorf("insert user: %w", err)
	}
	return id, nil
}

// ListAllUserIDs 返回全部用户 ID（用于为每个用户启动 push 派发消费者）。
func (d *DB) ListAllUserIDs(ctx context.Context) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT id FROM users`)
	if err != nil {
		return nil, fmt.Errorf("list user ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan user id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
