package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 能力开关的两种状态语义（capability-lifecycle §1）：
//
//	enabled  = 接受新的触发（源事件路由到它、上游 emit 指向它，都正常建任务）
//	disabled = 拒绝**自动**触发（不建任务，留剪枝事实 capability_disabled）
//
// disabled 不是删除（deleted_at 才是），也不是「暂停在途任务」：已排队/进行中的
// 任务完全不受影响（不取消、不暂停、不改 next_attempt_at）。
//
// 开关按 (namespace_id, name) 存独立表，**无行 = enabled**：所以没有回填，
// 老库升级后所有能力天然启用；换版本（PATCH）不重置开关。
const (
	CapabilityAuditEnable  = "enable"
	CapabilityAuditDisable = "disable"
	CapabilityAuditTest    = "test"
)

// 审计列表的 limit 口径（与设计 §5.2 一致：默认 50，1..200）。
const (
	CapabilityAuditDefaultLimit = 50
	CapabilityAuditMaxLimit     = 200
)

// CapabilitySwitch 是一个能力的开关事实（capability_switches 的一行）。
//
// Enabled 为 true 且库里没有行，是同一个语义（默认值）；Reason/UpdatedAt/UpdatedBy
// 只记「最近一次操作」，全 history 在 capability_audit。
type CapabilitySwitch struct {
	NamespaceID string
	Name        string
	Enabled     bool
	Reason      string
	UpdatedAt   int64
	UpdatedBy   string
}

// CapabilitySwitchInput 是一次启停请求（幂等：对已是目标状态的能力再调一次 = 覆盖最近一次操作 + 一条审计）。
type CapabilitySwitchInput struct {
	NamespaceID string
	Name        string
	Enabled     bool
	Reason      string
	ActorID     string
	Now         int64
}

// CapabilityAuditEntry 是 capability_audit 的一行（append-only）。
//
// Detail 只存摘要：enable/disable 记 {reason}，test 记 {traceId, version}——
// 试跑的 params 全文不落这里（参数里可能有业务数据）。
type CapabilityAuditEntry struct {
	ID          string
	NamespaceID string
	Name        string
	Action      string
	ActorID     string
	Detail      json.RawMessage
	CreatedAt   int64
}

// CapabilityAuditInput 是追加一条审计的入参。
type CapabilityAuditInput struct {
	NamespaceID string
	Name        string
	Action      string
	ActorID     string
	Detail      json.RawMessage
	Now         int64
}

// GetCapabilitySwitch 读一个能力的开关。
//
// **没有行 = enabled**：返回 Enabled=true 且不写行（读路径不写库）。
func (d *DB) GetCapabilitySwitch(ctx context.Context, namespaceID, name string) (*CapabilitySwitch, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	return getCapabilitySwitchOn(ctx, d, namespaceID, name)
}

// IsCapabilityEnabled 是闸门用的便捷读：无行 → (true, nil)。
func (d *DB) IsCapabilityEnabled(ctx context.Context, namespaceID, name string) (bool, error) {
	sw, err := d.GetCapabilitySwitch(ctx, namespaceID, name)
	if err != nil {
		return false, err
	}
	return sw.Enabled, nil
}

// getCapabilitySwitchOn 在任意 exec 上读开关（事务内的闸门必须走 tx，见 txexec.go）。
func getCapabilitySwitchOn(ctx context.Context, ex queryExec, namespaceID, name string) (*CapabilitySwitch, error) {
	sw := &CapabilitySwitch{NamespaceID: namespaceID, Name: name, Enabled: true}
	var enabled int
	err := ex.queryRowContext(ctx,
		`SELECT enabled, reason, updated_at, updated_by
		   FROM capability_switches WHERE namespace_id = ? AND name = ?`,
		namespaceID, name).Scan(&enabled, &sw.Reason, &sw.UpdatedAt, &sw.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return sw, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read capability switch: %w", err)
	}
	sw.Enabled = enabled != 0
	return sw, nil
}

// SetCapabilitySwitch 写开关（upsert）+ 追加一条审计，**同一个事务**。
//
// 幂等：重复调到同一状态不报错、不跳过审计——「我又确认了一次」也是操作事实；
// 而 reason/updated_at/updated_by 每次覆盖（= 最近一次操作是谁、为什么）。
// 不套 CAS（动作 POST，与 deploy 一致）：两个管理员同时按 = 最后写赢，两条审计都留。
func (d *DB) SetCapabilitySwitch(ctx context.Context, in CapabilitySwitchInput) (*CapabilitySwitch, error) {
	if in.NamespaceID == "" || in.NamespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("set capability switch: empty name")
	}
	action := CapabilityAuditDisable
	if in.Enabled {
		action = CapabilityAuditEnable
	}
	detail, err := marshalAuditDetail(map[string]any{"reason": in.Reason})
	if err != nil {
		return nil, err
	}
	if in.Now == 0 {
		in.Now = Now()
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("set capability switch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	// namespace 必须存在：否则审计行会悬空（外键只在开启 foreign_keys 时兜）。
	if err := requireNamespaceOn(ctx, ex, in.NamespaceID); err != nil {
		return nil, err
	}

	enabled := 0
	if in.Enabled {
		enabled = 1
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO capability_switches (namespace_id, name, enabled, reason, updated_at, updated_by)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(namespace_id, name) DO UPDATE SET
		     enabled = excluded.enabled,
		     reason = excluded.reason,
		     updated_at = excluded.updated_at,
		     updated_by = excluded.updated_by`,
		in.NamespaceID, in.Name, enabled, in.Reason, in.Now, in.ActorID); err != nil {
		return nil, fmt.Errorf("set capability switch: %w", err)
	}

	if _, err := appendCapabilityAuditOn(ctx, ex, CapabilityAuditInput{
		NamespaceID: in.NamespaceID, Name: in.Name, Action: action,
		ActorID: in.ActorID, Detail: detail, Now: in.Now,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("set capability switch: commit: %w", err)
	}
	return &CapabilitySwitch{
		NamespaceID: in.NamespaceID, Name: in.Name, Enabled: in.Enabled,
		Reason: in.Reason, UpdatedAt: in.Now, UpdatedBy: in.ActorID,
	}, nil
}

// AppendCapabilityAudit 追加一条审计（独立事务；试跑路径在事件事务内直接调 on 版本）。
func (d *DB) AppendCapabilityAudit(ctx context.Context, in CapabilityAuditInput) (*CapabilityAuditEntry, error) {
	if in.NamespaceID == "" || in.NamespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if in.Now == 0 {
		in.Now = Now()
	}
	return appendCapabilityAuditOn(ctx, d, in)
}

// appendCapabilityAuditOn 是审计的唯一写入实现（*DB 与 txExec 共用）。
//
// 只增不改：本表没有 update / delete 的写入口，读取端点也不提供删除。
func appendCapabilityAuditOn(ctx context.Context, ex queryExec, in CapabilityAuditInput) (*CapabilityAuditEntry, error) {
	switch in.Action {
	case CapabilityAuditEnable, CapabilityAuditDisable, CapabilityAuditTest:
	default:
		return nil, fmt.Errorf("capability audit: action %q 不在封闭集合内（enable/disable/test）", in.Action)
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("capability audit: empty name")
	}
	detail := in.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	if !json.Valid(detail) {
		return nil, fmt.Errorf("capability audit: invalid detail")
	}
	entry := &CapabilityAuditEntry{
		ID:          NewCapabilityAuditID(),
		NamespaceID: in.NamespaceID,
		Name:        in.Name,
		Action:      in.Action,
		ActorID:     in.ActorID,
		Detail:      detail,
		CreatedAt:   in.Now,
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO capability_audit (id, namespace_id, name, action, actor_id, detail, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		entry.ID, entry.NamespaceID, entry.Name, entry.Action, entry.ActorID,
		string(entry.Detail), entry.CreatedAt); err != nil {
		return nil, fmt.Errorf("capability audit: %w", err)
	}
	return entry, nil
}

// ListCapabilityAudit 读某能力的最近操作（created_at DESC, id DESC；空列表 []）。
func (d *DB) ListCapabilityAudit(ctx context.Context, namespaceID, name string, limit int) ([]*CapabilityAuditEntry, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if limit <= 0 {
		limit = CapabilityAuditDefaultLimit
	}
	if limit > CapabilityAuditMaxLimit {
		limit = CapabilityAuditMaxLimit
	}
	rows, err := d.cachedQueryContext(ctx,
		`SELECT id, namespace_id, name, action, actor_id, detail, created_at
		   FROM capability_audit
		  WHERE namespace_id = ? AND name = ?
		  ORDER BY created_at DESC, id DESC
		  LIMIT ?`, namespaceID, name, limit)
	if err != nil {
		return nil, fmt.Errorf("list capability audit: %w", err)
	}
	defer rows.Close()
	out := []*CapabilityAuditEntry{} // 不变量 I9：空列表是 [] 不是 null
	for rows.Next() {
		var (
			e      CapabilityAuditEntry
			detail string
		)
		if err := rows.Scan(&e.ID, &e.NamespaceID, &e.Name, &e.Action, &e.ActorID,
			&detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan capability audit: %w", err)
		}
		e.Detail = json.RawMessage(detail)
		if len(e.Detail) == 0 {
			e.Detail = json.RawMessage(`{}`)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// marshalAuditDetail 序列化审计摘要；空 map 归一为 `{}`（列默认值同形）。
func marshalAuditDetail(v map[string]any) (json.RawMessage, error) {
	out := map[string]any{}
	for k, val := range v {
		if s, ok := val.(string); ok && s == "" {
			continue
		}
		if val == nil {
			continue
		}
		out[k] = val
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("capability audit: marshal detail: %w", err)
	}
	return raw, nil
}

// requireNamespaceOn 在事务内确认 namespace 存在（不存在 → ErrNamespaceNotFound）。
func requireNamespaceOn(ctx context.Context, ex queryExec, namespaceID string) error {
	var slug string
	err := ex.queryRowContext(ctx, `SELECT slug FROM namespaces WHERE id = ?`, namespaceID).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNamespaceNotFound
	}
	if err != nil {
		return fmt.Errorf("read namespace: %w", err)
	}
	return nil
}
