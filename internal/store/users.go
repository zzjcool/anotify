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
