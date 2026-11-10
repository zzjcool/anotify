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

// source_event.go 是**源事件接收的原子入口**（S1-D2）。
//
// 为什么必须新增一个 store 内部入口：G2 旧路径把「事件 + 投递窗 + 剪枝留痕」放在
// 一个事务、把「任务扇出」放在另一个事务。两次提交之间任何失败都会留下
// 「事件已经消费了窗口、任务却没建」的半成品——重试被窗口吞掉，trace 里永远
// 少一支 fanout。这里把整条接收链放进**一个** BEGIN IMMEDIATE 事务：
//
//	当前源身份/版本 CAS + namespace 输入 pin
//	→ 投递窗判定（event_deliveries）
//	→ 事件行（含 receipt 快照）+ 正常任务批 + 失败 Task/错误事件 + 剪枝事件
//	→ commit
//
// 重复投递在有效窗口内返回**第一次**持久化的 receipt 字节（first=false），
// 不建新任务、不占新窗、也绝不第二次查询当前可变资源重算回执。

// ErrSourceEventReceiptMissing 表示窗口内命中的原事件没有持久 receipt。
//
// 这种行只可能来自旧的非原子写入路径（或手工数据）：它没有「第一次回执」可回放。
// 明确报错，而不是用当前资源拼一份看起来像真的回执。
var ErrSourceEventReceiptMissing = errors.New("store: source event receipt missing")

// ErrNamespaceInputsChanged 表示接收事务里 namespace 输入 revision 与调用方
// 计划时钉住的不一致（namespaceVarsRevision 已变）。
//
// 这不是「资源 CAS」：namespaceVarsRevision 是任务输入快照的版本，失败时调用方
// 必须重新计划（重新解析目标/参数），而不是原地重试同一批预编译任务。
var ErrNamespaceInputsChanged = errors.New("store: namespace inputs revision changed")

// PruneInputs 是一条**纯剪枝**观察事实（no_target / param_* / event_schema …）。
//
// downstream_missing 不在这个集合里：它是失败 Task（见 DispatchFailureInputs）。
type PruneInputs struct {
	Capability string
	Reason     string
	Detail     string
}

// SourceEventInputs 是一次源事件接收的全部 typed 事实（store 本地，不依赖 route/broker）。
type SourceEventInputs struct {
	NamespaceID string
	// SourceID 是**当前源行的稳定 id**（sources.id），不是源名。
	SourceID string
	// ExpectedSourceResourceVersion 是计划时读到的 sources.resource_version（CAS，必填 > 0）。
	ExpectedSourceResourceVersion int64
	// ExpectedNamespaceVarsRevision 是计划时钉住的 namespaceVarsRevision（无配置行 = 0）。
	ExpectedNamespaceVarsRevision int64

	// TraceID 为空 → store 生成；事件 ID 总是由 store 生成（receipt 要含它）。
	TraceID string
	// Type 为空 → 用源名（与 source 运行期同一口径）。
	Type string
	// Payload 原样保存（非法 JSON 归一为 {}，与 InsertEvent 同口径）。
	Payload json.RawMessage

	// DeliveryID 外部投递幂等键；空 = 每次新事件（不去重）。
	DeliveryID string
	// WindowSec 投递去重窗（秒；0 = 永久）。
	WindowSec int64
	// Now 可信 server 时刻；0 → Now()。
	Now int64

	// Tasks 是已解析的正常任务（pending；capability_id/version/digest 已钉住）。
	Tasks []*Task
	// Failures 是未解析派发失败事实（downstream_missing），与事件同事务。
	Failures []DispatchFailureInputs
	// Prunes 是纯剪枝观察事实。
	Prunes []PruneInputs
}

// 接收回执的内部持久事实形状（存 events.receipt 列；不是资源、不是 wire DTO）。
//
// 空列表一律 `[]`：G2/G3 之后投影时「确实没有」与「字段缺失」不能混。
type SourceEventReceipt struct {
	EventID     string                      `json:"eventId"`
	TraceID     string                      `json:"traceId"`
	Tasks       []SourceEventReceiptTask    `json:"tasks"`
	FailedTasks []SourceEventReceiptFailure `json:"failedTasks"`
	Pruned      []SourceEventReceiptPrune   `json:"pruned"`
}

// SourceEventReceiptTask 是回执里一条正常（已解析）任务。
type SourceEventReceiptTask struct {
	TaskID     string `json:"taskId"`
	Capability string `json:"capability"`
	Version    int    `json:"version"`
}

// SourceEventReceiptFailure 是回执里一条未解析失败任务。
type SourceEventReceiptFailure struct {
	TaskID     string `json:"taskId"`
	Capability string `json:"capability"`
	// Version 只在请求明确声明过时出现；0 是「没有声明/解析事实」，wire 省略。
	Version int    `json:"version,omitempty"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// SourceEventReceiptPrune 是回执里一条纯剪枝观察。
type SourceEventReceiptPrune struct {
	Capability string `json:"capability,omitempty"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
}

// AcceptSourceEvent 原子接收一次源事件（事件/窗口/任务/失败/剪枝/receipt 一个事务）。
//
// 返回 (receipt, first, err)：
//   - first=true  → 这次真的接收了新事件，receipt 是本次写入并持久化的完整回执；
//   - first=false → 窗口内重复投递，receipt 是第一次持久化的**同一份字节**；
//   - err != nil  → 整个事务回滚（0 写，**包括投递窗占用**）。
//
// 事务内校验全部走 tx（不逃出同一 snapshot）：
//  1. 源行存在、namespace 命中、resource_version CAS（源不存在/越 ns/标识 ABA 都 0 写）；
//  2. namespace 输入 revision 与计划时一致（预编译任务不能用陈旧参数被接受）；
//  3. 投递窗判定；
//  4. 事件 + 正常任务 + 失败 Task/错误事件 + 剪枝事件 + receipt。
func (d *DB) AcceptSourceEvent(ctx context.Context, in SourceEventInputs) (json.RawMessage, bool, error) {
	if in.NamespaceID == "" || in.NamespaceID == AllNamespaces {
		return nil, false, ErrNamespaceRequired
	}
	if strings.TrimSpace(in.SourceID) == "" {
		return nil, false, fmt.Errorf("accept source event: empty source_id")
	}
	if in.ExpectedSourceResourceVersion <= 0 {
		return nil, false, ErrResourceVersionRequired
	}
	if in.Now == 0 {
		in.Now = Now()
	}
	payload := string(in.Payload)
	if len(in.Payload) == 0 || !json.Valid(in.Payload) {
		payload = "{}"
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("accept source event: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	// ① 当前源身份 + namespace 命中 + spec 版本 CAS。
	var (
		sourceName string
		sourceRV   int64
	)
	err = ex.queryRowContext(ctx,
		`SELECT name, resource_version FROM sources WHERE namespace_id = ? AND id = ?`,
		in.NamespaceID, in.SourceID).Scan(&sourceName, &sourceRV)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: source %s@%s", ErrSourceNotFound, in.NamespaceID, in.SourceID)
	}
	if err != nil {
		return nil, false, fmt.Errorf("accept source event: read source: %w", err)
	}
	if err := checkResourceRevision(in.ExpectedSourceResourceVersion, in.SourceID, sourceRV); err != nil {
		return nil, false, err
	}
	if sourceName == EventPruneSource {
		return nil, false, ErrReservedEventSource
	}

	// ② namespace 存在 + 输入 revision pin（同一 tx 读，不能从池上读）。
	var namespaceSlug string
	err = ex.queryRowContext(ctx, `SELECT slug FROM namespaces WHERE id = ?`, in.NamespaceID).Scan(&namespaceSlug)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNamespaceNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("accept source event: read namespace: %w", err)
	}
	pin, err := loadNamespaceInputsOn(ctx, ex, in.NamespaceID)
	if err != nil {
		return nil, false, err
	}
	if pin.revision != in.ExpectedNamespaceVarsRevision {
		return nil, false, fmt.Errorf("%w: namespace %s inputs revision %d != planned %d",
			ErrNamespaceInputsChanged, namespaceSlug, pin.revision, in.ExpectedNamespaceVarsRevision)
	}

	// ③ 事件行事实（ID 由 store 生成，receipt 必须含它）。
	traceID := strings.TrimSpace(in.TraceID)
	if traceID == "" {
		traceID = NewEventID()
	}
	eventType := strings.TrimSpace(in.Type)
	if eventType == "" {
		eventType = sourceName
	}
	event := &Event{
		ID:          NewEventID(),
		NamespaceID: in.NamespaceID,
		TraceID:     traceID,
		Source:      sourceName,
		Type:        eventType,
		Payload:     json.RawMessage(payload),
		Reason:      EventReasonIngested,
		CreatedAt:   in.Now,
	}
	if in.DeliveryID != "" {
		deliveryID := in.DeliveryID
		event.DeliveryID = &deliveryID
	}

	// ④ 投递窗：窗口内重复 → 回第一次的持久 receipt（只读命中，回滚空事务）。
	if event.DeliveryID != nil {
		hit, cerr := claimDeliveryOn(ctx, ex, event, in.SourceID, in.Now, in.WindowSec)
		if cerr != nil {
			return nil, false, cerr
		}
		if hit != nil {
			receipt, rerr := readEventReceiptOn(ctx, ex, in.NamespaceID, hit.ID)
			if rerr != nil {
				return nil, false, rerr
			}
			return receipt, false, nil
		}
	}

	// ⑤ 归一本次计划里的任务 / 失败 / 剪枝（全部先校验再写，失败也是 0 写）。
	for i, t := range in.Tasks {
		if t == nil {
			return nil, false, fmt.Errorf("accept source event: nil task at index %d", i)
		}
		if t.ID == "" {
			t.ID = NewTaskID()
		}
		if t.TraceID == "" {
			t.TraceID = traceID
		} else if t.TraceID != traceID {
			return nil, false, fmt.Errorf("accept source event: task %d trace %q != event trace %q", i, t.TraceID, traceID)
		}
		if t.NamespaceID == "" {
			t.NamespaceID = in.NamespaceID
		} else if t.NamespaceID != in.NamespaceID {
			return nil, false, fmt.Errorf("accept source event: task %d namespace %q != event namespace %q", i, t.NamespaceID, in.NamespaceID)
		}
	}
	failureRows := make([]*Task, 0, len(in.Failures))
	for _, f := range in.Failures {
		row, ferr := newDispatchFailureTask(in.NamespaceID, traceID, nil, f, in.Now)
		if ferr != nil {
			return nil, false, ferr
		}
		failureRows = append(failureRows, row)
	}
	for i, p := range in.Prunes {
		if !protocol.IsPruneReason(p.Reason) {
			return nil, false, fmt.Errorf("accept source event: prune %d reason %q 不是剪枝码（downstream_missing 走失败 Task）", i, p.Reason)
		}
	}

	receipt := SourceEventReceipt{
		EventID:     event.ID,
		TraceID:     traceID,
		Tasks:       make([]SourceEventReceiptTask, 0, len(in.Tasks)),
		FailedTasks: make([]SourceEventReceiptFailure, 0, len(in.Failures)),
		Pruned:      make([]SourceEventReceiptPrune, 0, len(in.Prunes)),
	}
	for _, t := range in.Tasks {
		receipt.Tasks = append(receipt.Tasks, SourceEventReceiptTask{
			TaskID: t.ID, Capability: t.Capability, Version: t.Version,
		})
	}
	for i, f := range in.Failures {
		norm, _ := normalizeDispatchFailure(f)
		receipt.FailedTasks = append(receipt.FailedTasks, SourceEventReceiptFailure{
			TaskID: failureRows[i].ID, Capability: norm.Capability, Version: norm.Version,
			Reason: norm.Reason, Message: norm.Detail,
		})
	}
	for _, p := range in.Prunes {
		receipt.Pruned = append(receipt.Pruned, SourceEventReceiptPrune{
			Capability: p.Capability, Reason: p.Reason, Detail: p.Detail,
		})
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return nil, false, fmt.Errorf("accept source event: marshal receipt: %w", err)
	}

	// ⑥ 一个事务写全部事实：任一点真实失败（ctx/约束/触发）→ 全回滚（含 window）。
	if err := insertIngestEventOn(ctx, ex, event, payload, receiptJSON); err != nil {
		return nil, false, err
	}
	if err := d.createTasksTx(ctx, tx, in.Tasks); err != nil {
		return nil, false, err
	}
	for i := range in.Failures {
		if err := insertDispatchFailureTaskOn(ctx, ex, failureRows[i], in.Failures[i], in.Now); err != nil {
			return nil, false, err
		}
	}
	for _, p := range in.Prunes {
		evPayload, merr := json.Marshal(map[string]any{
			"capability": p.Capability,
			"reason":     p.Reason,
			"detail":     p.Detail,
		})
		if merr != nil {
			return nil, false, fmt.Errorf("accept source event: marshal prune: %w", merr)
		}
		ev := &Event{
			NamespaceID: in.NamespaceID,
			TraceID:     traceID,
			Type:        p.Reason,
			Payload:     evPayload,
			CreatedAt:   in.Now,
		}
		if err := insertPruneFactEventOn(ctx, ex, ev); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("accept source event: commit: %w", err)
	}
	return receiptJSON, true, nil
}
