package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
)

// CapabilityTestInputs 是一次**人工试跑**的全部 typed 事实（store 本地类型）。
//
// 试跑与源事件走同一个事务内核（事件行 + 任务行 + 回执），只有三处不同：
//   - 事件 origin='test'、source 固定 EventTestSource；
//   - 不写投递去重记录（event_deliveries 属于源投递）；
//   - 同一事务里追加一条 action='test' 的审计摘要。
type CapabilityTestInputs struct {
	NamespaceID string
	// TraceID 为空 → store 生成；事件 ID 总是由 store 生成（回执里要给出）。
	TraceID string
	// Capability / Version 是被试跑的能力身份（调用方已按引用解析好）。
	Capability string
	Version    int
	// Payload 原样保存（非法 JSON 归一为 {}）：这是「为什么这次跑了」的事件证据。
	Payload json.RawMessage
	// Tasks 是已解析的正常任务（pending；capability_id/version/digest 已钉住）。
	Tasks []*Task
	// Prunes 是纯剪枝观察事实（正常情况下为空：参数不合 schema 在计划期就 400 了）。
	Prunes []PruneInputs
	// ActorID 是发起试跑的会话用户（空 = 非会话主体）。
	ActorID string
	Now     int64
}

// CapabilityTestReceipt 是试跑回执的持久事实（存 events.receipt，也直接回给调用方）。
//
// 与 SourceEventReceipt 同形状口径：空列表一律 `[]`，不是 null。
type CapabilityTestReceipt struct {
	EventID string                    `json:"eventId"`
	TraceID string                    `json:"traceId"`
	Origin  string                    `json:"origin"`
	Tasks   []SourceEventReceiptTask  `json:"tasks"`
	Pruned  []SourceEventReceiptPrune `json:"pruned"`
}

// AcceptTestEvent 原子接收一次人工试跑（事件 + 任务 + 剪枝 + 回执 + 审计一个事务）。
//
// 为什么必须复用接收事务而不是新开建任务路径：createTasksTx 的两条调用方已经把
// 「任务行必须钉住版本与 digest」的 CHECK 约束吃透了，旁路建行会立刻踩到
// tasks_resolved_pin_shape。
//
// 校验全部走 tx（不逃出同一 snapshot）：
//  1. namespace 存在；
//  2. 能力行在同一事务里仍然是**活的**，且与任务钉住的身份逐字一致；
//  3. 任务的 capability/version/digest 必须等于入参（防止调用方拼错一行造成
//     「回执说 A、任务跑 B」）。
func (d *DB) AcceptTestEvent(ctx context.Context, in CapabilityTestInputs) (*CapabilityTestReceipt, error) {
	if in.NamespaceID == "" || in.NamespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if strings.TrimSpace(in.Capability) == "" {
		return nil, fmt.Errorf("accept test event: empty capability")
	}
	if in.Version <= 0 {
		return nil, ErrResourceVersionRequired
	}
	if len(in.Tasks) == 0 {
		return nil, fmt.Errorf("accept test event: no tasks（试跑必须建真任务）")
	}
	if in.Now == 0 {
		in.Now = Now()
	}

	payload := string(in.Payload)
	if len(in.Payload) == 0 || !json.Valid(in.Payload) {
		payload = "{}"
	}

	traceID := strings.TrimSpace(in.TraceID)
	if traceID == "" {
		traceID = NewEventID()
	}

	// 归一任务（全部先校验再写，失败也是 0 写）。
	for i, t := range in.Tasks {
		if t == nil {
			return nil, fmt.Errorf("accept test event: nil task at index %d", i)
		}
		if t.ID == "" {
			t.ID = NewTaskID()
		}
		if t.NamespaceID == "" {
			t.NamespaceID = in.NamespaceID
		} else if t.NamespaceID != in.NamespaceID {
			return nil, fmt.Errorf("accept test event: task %d namespace %q != %q", i, t.NamespaceID, in.NamespaceID)
		}
		if t.TraceID == "" {
			t.TraceID = traceID
		} else if t.TraceID != traceID {
			return nil, fmt.Errorf("accept test event: task %d trace %q != %q", i, t.TraceID, traceID)
		}
		if t.Capability != in.Capability || t.Version != in.Version {
			return nil, fmt.Errorf("accept test event: task %d 身份 %s@%d != %s@%d",
				i, t.Capability, t.Version, in.Capability, in.Version)
		}
		if t.CapabilityID == "" || t.Digest == "" {
			return nil, fmt.Errorf("accept test event: task %d 未钉住 capability_id/digest", i)
		}
	}
	for i, p := range in.Prunes {
		if !protocol.IsPruneReason(p.Reason) {
			return nil, fmt.Errorf("accept test event: prune %d reason %q 不是剪枝码", i, p.Reason)
		}
	}

	receipt := CapabilityTestReceipt{
		EventID: "",
		TraceID: traceID,
		Origin:  EventOriginTest,
		Tasks:   make([]SourceEventReceiptTask, 0, len(in.Tasks)),
		Pruned:  make([]SourceEventReceiptPrune, 0, len(in.Prunes)),
	}
	for _, t := range in.Tasks {
		receipt.Tasks = append(receipt.Tasks, SourceEventReceiptTask{
			TaskID: t.ID, Capability: t.Capability, Version: t.Version,
		})
	}
	for _, p := range in.Prunes {
		receipt.Pruned = append(receipt.Pruned, SourceEventReceiptPrune{
			Capability: p.Capability, Reason: p.Reason, Detail: p.Detail,
		})
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("accept test event: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	// ① namespace 存在。
	if err := requireNamespaceOn(ctx, ex, in.NamespaceID); err != nil {
		return nil, err
	}

	// ② 能力行仍然是活的，且与任务钉住的身份一致（删除/换版本与试跑竞态）。
	var (
		liveName    string
		liveVersion int
		liveDigest  string
	)
	err = ex.queryRowContext(ctx,
		`SELECT name, version, digest FROM capability_versions
		  WHERE namespace_id = ? AND id = ? AND deleted_at IS NULL`,
		in.NamespaceID, in.Tasks[0].CapabilityID).Scan(&liveName, &liveVersion, &liveDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("accept test event: read capability: %w", err)
	}
	if liveName != in.Capability || liveVersion != in.Version || liveDigest != in.Tasks[0].Digest {
		return nil, fmt.Errorf("accept test event: 能力行 %s@%d/%s 与计划 %s@%d/%s 不一致",
			liveName, liveVersion, liveDigest, in.Capability, in.Version, in.Tasks[0].Digest)
	}

	// ③ 事件行（ID 由 store 生成，回执必须含它）。
	event := &Event{
		ID:          NewEventID(),
		NamespaceID: in.NamespaceID,
		TraceID:     traceID,
		Source:      EventTestSource,
		Origin:      EventOriginTest,
		Type:        in.Capability,
		Payload:     json.RawMessage(payload),
		Reason:      EventReasonIngested,
		CreatedAt:   in.Now,
	}
	receipt.EventID = event.ID
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("accept test event: marshal receipt: %w", err)
	}

	// ④ 一个事务写全部事实。
	if err := insertIngestEventOn(ctx, ex, event, payload, receiptJSON); err != nil {
		return nil, err
	}
	if err := d.createTasksTx(ctx, tx, in.Tasks); err != nil {
		return nil, err
	}
	for _, p := range in.Prunes {
		evPayload, merr := json.Marshal(map[string]any{
			"capability": p.Capability, "reason": p.Reason, "detail": p.Detail,
		})
		if merr != nil {
			return nil, fmt.Errorf("accept test event: marshal prune: %w", merr)
		}
		ev := &Event{
			NamespaceID: in.NamespaceID, TraceID: traceID, Type: p.Reason,
			Payload: evPayload, CreatedAt: in.Now,
		}
		if err := insertPruneFactEventOn(ctx, ex, ev); err != nil {
			return nil, err
		}
	}
	// ⑤ 审计摘要：只记 traceId + version，**不存参数全文**。
	detail, derr := marshalAuditDetail(map[string]any{"traceId": traceID, "version": in.Version})
	if derr != nil {
		return nil, derr
	}
	if _, err := appendCapabilityAuditOn(ctx, ex, CapabilityAuditInput{
		NamespaceID: in.NamespaceID, Name: in.Capability, Action: CapabilityAuditTest,
		ActorID: in.ActorID, Detail: detail, Now: in.Now,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("accept test event: commit: %w", err)
	}
	return &receipt, nil
}
