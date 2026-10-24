package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Event 是 events 表的一行：事件日志的起点（链的根）。
//
// TraceID 由**触发器**生成并写进来（契约 §10 Q2），中枢不代生成、不猜。
// Payload 只存不解释（不变量 I6：中枢不解释任何用户提供的逻辑）。
type Event struct {
	ID        string          `json:"id"`
	OrgID     string          `json:"orgId"`
	TraceID   string          `json:"traceId"`
	Source    string          `json:"source"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	IdemKey   *string         `json:"idemKey,omitempty"`
	CreatedAt int64           `json:"createdAt"`
}

// eventColumns 是统一的列顺序，scanEvent 依赖它。
const eventColumns = `id, org_id, trace_id, source, type, payload, idem_key, created_at`

// InsertEvent 幂等写入事件（契约 §10.1 / Q6）。
//
// 带 IdemKey 且冲突时返回 (false, nil)，并把 e 填成**已有行的 event_id + trace_id**
// ——不是 409：对发送方而言「已收过」和「本次收下」都该是 200。
// 不带 IdemKey 时允许重复（不是所有来源都有稳定投递 ID，硬造反而制造假幂等）。
func (d *DB) InsertEvent(ctx context.Context, e *Event) (inserted bool, err error) {
	if e == nil {
		return false, fmt.Errorf("insert event: nil")
	}
	if e.OrgID == "" {
		return false, fmt.Errorf("insert event: empty org_id")
	}
	if e.Source == "" || e.Type == "" {
		return false, fmt.Errorf("insert event: source and type required")
	}
	if e.TraceID == "" {
		return false, fmt.Errorf("insert event: empty trace_id (触发器必须生成 trace_id)")
	}
	payload := string(e.Payload)
	if payload == "" || !json.Valid(e.Payload) {
		payload = "{}"
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = Now()
	}
	if e.ID == "" {
		e.ID = NewEventID()
	}

	// 幂等写：不能写 `ON CONFLICT(org_id, source, idem_key)`——SQLite 的
	// upsert 目标必须是完整唯一约束或主键，而这里用的是**部分**唯一索引
	// （`WHERE idem_key IS NOT NULL`），指定列会报
	// 「ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint」。
	// 用 INSERT OR IGNORE 让任何唯一冲突都静默跳过，再由 RowsAffected 判断是否插入。
	res, err := d.ExecContext(ctx,
		`INSERT OR IGNORE INTO events (id, org_id, trace_id, source, type, payload, idem_key, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		e.ID, e.OrgID, e.TraceID, e.Source, e.Type, payload, e.IdemKey, e.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert event rows: %w", err)
	}
	if n == 1 {
		e.Payload = json.RawMessage(payload)
		return true, nil
	}

	// 冲突：回查已有行，把它填回 e（调用方拿到的是「已有的 event_id + trace_id」）
	if e.IdemKey == nil {
		// 理论上不可达（没有 idem_key 不会冲突），但保持防御
		return false, fmt.Errorf("insert event: conflict without idem_key")
	}
	existing, gerr := d.getEventByIdemKey(ctx, e.OrgID, e.Source, *e.IdemKey)
	if gerr != nil {
		return false, fmt.Errorf("read existing event after conflict: %w", gerr)
	}
	*e = *existing
	return false, nil
}

// getEventByIdemKey 按 (org_id, source, idem_key) 取已有事件。
func (d *DB) getEventByIdemKey(ctx context.Context, orgID, source, idemKey string) (*Event, error) {
	return d.scanEvent(ctx,
		`SELECT `+eventColumns+` FROM events
		 WHERE org_id = ? AND source = ? AND idem_key = ?`, orgID, source, idemKey)
}

// scanEvent 执行查询并解析一行；未命中返回 ErrNotFound。
func (d *DB) scanEvent(ctx context.Context, query string, args ...any) (*Event, error) {
	var e Event
	var payload string
	err := d.QueryRowContext(ctx, query, args...).Scan(
		&e.ID, &e.OrgID, &e.TraceID, &e.Source, &e.Type, &payload, &e.IdemKey, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan event: %w", err)
	}
	e.Payload = json.RawMessage(payload)
	return &e, nil
}

// GetEvent 按 (orgID, id) 取事件；未命中返回 ErrNotFound。
func (d *DB) GetEvent(ctx context.Context, orgID, id string) (*Event, error) {
	return d.scanEvent(ctx,
		`SELECT `+eventColumns+` FROM events WHERE org_id = ? AND id = ?`, orgID, id)
}

// ListEvents 列出组织事件，可按 traceID 过滤（"" = 不过滤）；空列表返回 []。
//
// 按 created_at DESC（最新在前）+ id DESC（同秒内稳定序），limit ≤ 0 时用默认 50。
func (d *DB) ListEvents(ctx context.Context, orgID, traceID string, limit int) ([]*Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT ` + eventColumns + ` FROM events WHERE org_id = ?`
	args := []any{orgID}
	if traceID != "" {
		q += ` AND trace_id = ?`
		args = append(args, traceID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	out := []*Event{}
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.ID, &e.OrgID, &e.TraceID, &e.Source, &e.Type, &payload, &e.IdemKey, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event row: %w", err)
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// GetEventByTrace 取某 trace 的根事件；未命中返回 ErrNotFound。
func (d *DB) GetEventByTrace(ctx context.Context, orgID, traceID string) (*Event, error) {
	return d.scanEvent(ctx,
		`SELECT `+eventColumns+` FROM events
		 WHERE org_id = ? AND trace_id = ?
		 ORDER BY created_at, id LIMIT 1`, orgID, traceID)
}
