package store

import (
	"context"
	"encoding/json"
)

// Event 是 events 表的一行：事件日志的起点（链的根）。
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

// InsertEvent 幂等写入事件（契约 §10.1）。
//
// 带 IdemKey 且冲突时返回 (false, nil)，并把 e 填成**已有行的 event_id + trace_id**
// ——不是 409：对发送方而言「已收过」和「本次收下」都该是 200。
// 不带 IdemKey 时允许重复。
func (d *DB) InsertEvent(ctx context.Context, e *Event) (inserted bool, err error) {
	return false, ErrNotImplemented
}

// GetEvent 按 (orgID, id) 取事件；未命中返回 ErrNotFound。
func (d *DB) GetEvent(ctx context.Context, orgID, id string) (*Event, error) {
	return nil, ErrNotImplemented
}

// ListEvents 列出组织事件，可按 traceID 过滤（"" = 不过滤）；空列表返回 []。
func (d *DB) ListEvents(ctx context.Context, orgID, traceID string, limit int) ([]*Event, error) {
	return nil, ErrNotImplemented
}

// GetEventByTrace 取某 trace 的根事件；未命中返回 ErrNotFound。
func (d *DB) GetEventByTrace(ctx context.Context, orgID, traceID string) (*Event, error) {
	return nil, ErrNotImplemented
}
