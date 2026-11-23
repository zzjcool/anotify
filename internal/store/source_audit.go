package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SourceAuditPolledManually 是「立即拉取一次」（#555）的审计动作。
//
// 轮询本身的终局已经落在 sources 行的派生列上（游标/失败计数/错误），但**谁**按下了按钮
// 只有会话知道——那一个事实没有别的归宿，所以单独记一行 append-only 审计。
const SourceAuditPolledManually = "source.polled_manually"

// SourceAuditInput 是一次源审计追加。
//
// Detail 只存摘要（本动作记 result 的计数），不存响应正文——正文里可能有业务数据，
// 也可能有凭据头部的回显。
type SourceAuditInput struct {
	NamespaceID string
	SourceID    string
	Action      string
	ActorID     string
	Detail      json.RawMessage
	Now         int64
}

// SourceAuditEntry 是 source_audit 的一行（只增不删：没有 update / delete 写入口）。
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
