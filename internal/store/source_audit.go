package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	AuditDefaultLimit = 50
	AuditMaxLimit     = 200
)

// SourceAuditPolledManually 是「立即拉取一次」（#555）的审计动作。
//
// 为什么单开一张表：轮询的终局已经落在 sources 行的派生列上（游标 / 失败计数 / 错误），
// 但「谁按下了按钮」只有会话知道，那一个事实没有别的归宿；capability_audit 是按能力名
// 组织的，塞进去会让「某个源的操历史」变成一次跨表拼装。
const SourceAuditPolledManually = "source.polled_manually"

// SourceAuditInput 是一次源审计追加。
//
// Detail 只存摘要（本动作记本轮计数），不存响应正文：正文里可能有业务数据。
type SourceAuditInput struct {
	NamespaceID string
	SourceID    string
	Action      string
	ActorID     string
	Detail      json.RawMessage
	Now         int64
}

// SourceAuditEntry 是 source_audit 的一行（只增不删：没有 update / delete 写入口，
// 也不接保留期——写量 = 人工动作次数）。
type SourceAuditEntry struct {
	ID          string
	NamespaceID string
	SourceID    string
	Action      string
	ActorID     string
	Detail      json.RawMessage
	CreatedAt   int64
}

// NewSourceAuditID 生成源审计 ID：sau_<base62>。
func NewSourceAuditID() string { return newID("sau") }

// AppendSourceAudit 追加一条源审计（独立事务）。
//
// 失败**不**回滚已发生的拉取：审计是「记账」，不是「执行的一部分」——调用方记日志即可。
func (d *DB) AppendSourceAudit(ctx context.Context, in SourceAuditInput) (*SourceAuditEntry, error) {
	if in.NamespaceID == "" || in.NamespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if in.Action != SourceAuditPolledManually {
		return nil, fmt.Errorf("source audit: action %q 不在封闭集合内（source.polled_manually）", in.Action)
	}
	if strings.TrimSpace(in.SourceID) == "" {
		return nil, fmt.Errorf("source audit: empty source id")
	}
	detail := in.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	if !json.Valid(detail) {
		return nil, fmt.Errorf("source audit: invalid detail")
	}
	if in.Now == 0 {
		in.Now = Now()
	}
	entry := &SourceAuditEntry{
		ID:          NewSourceAuditID(),
		NamespaceID: in.NamespaceID,
		SourceID:    in.SourceID,
		Action:      in.Action,
		ActorID:     in.ActorID,
		Detail:      detail,
		CreatedAt:   in.Now,
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO source_audit (id, namespace_id, source_id, action, actor_id, detail, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		entry.ID, entry.NamespaceID, entry.SourceID, entry.Action, entry.ActorID,
		string(entry.Detail), entry.CreatedAt); err != nil {
		return nil, fmt.Errorf("source audit: %w", err)
	}
	return entry, nil
}

func (d *DB) ListSourceAudit(ctx context.Context, namespaceID, sourceID string, beforeAt int64, limit int) ([]*SourceAuditEntry, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if limit <= 0 {
		limit = AuditDefaultLimit
	}
	if limit > AuditMaxLimit {
		limit = AuditMaxLimit
	}
	q := `SELECT id, namespace_id, source_id, action, actor_id, detail, created_at
		   FROM source_audit
		  WHERE namespace_id = ? AND source_id = ?`
	args := []any{namespaceID, sourceID}
	if beforeAt != 0 {
		q += ` AND created_at < ?`
		args = append(args, beforeAt)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.queryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list source audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*SourceAuditEntry{}
	for rows.Next() {
		var (
			e      SourceAuditEntry
			detail string
		)
		if err := rows.Scan(&e.ID, &e.NamespaceID, &e.SourceID, &e.Action, &e.ActorID,
			&detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan source audit: %w", err)
		}
		e.Detail = json.RawMessage(detail)
		if len(e.Detail) == 0 {
			e.Detail = json.RawMessage(`{}`)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list source audit: %w", err)
	}
	return out, nil
}
