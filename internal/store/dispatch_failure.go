package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
)

// dispatch_failure.go 是**未解析派发失败**的唯一落点（S1-D1）。
//
// 为什么需要一个独立形状：下游引用解析不到（downstream_missing）不是剪枝——
// capability.md §6.2 要求它成为一条**真实的失败 Task** + 一条观察事件，而不是
// `param_* / no_target / event_schema` 那种「事件收到了但没派发」的留痕。
//
// 但这条 Task 没有任何执行事实可钉：没有能力行 UID、没有 digest、没有 Node/slot/
// lease/token、没有进程时间。所以它只能以 `capability_id IS NULL` 的形状存在
// （表上两条 CHECK 把非法形状全部拒掉），并且**只能**由这里的 helper 写；
// 普通建行/派发/授权入口一律拒绝 NULL ref。

// DispatchFailureInputs 是一条未解析派发失败的事实（store 本地 typed，不是 wire DTO）。
type DispatchFailureInputs struct {
	// Capability 是请求里写的引用/名字（原样保留请求事实，必填）。
	Capability string
	// Version 是请求**明确声明**的版本；0 = 请求未声明版本（内部无解析事实）。
	Version int
	// Reason 只允许 downstream_missing（空 = 默认它）。其它错误码不是这条路径的事实。
	Reason string
	// Detail 是给错误投影的 message（人可读说明）。
	Detail string
}

// dispatchFailurePayload 是失败观察事件的 payload（沿用现 reason/message 对象）。
type dispatchFailurePayload struct {
	Capability   string `json:"capability"`
	Version      int    `json:"version,omitempty"`
	Reason       string `json:"reason"`
	Message      string `json:"message,omitempty"`
	TaskID       string `json:"taskId"`
	ParentTaskID string `json:"parentTaskId,omitempty"`
}

// normalizeDispatchFailure 校验并归一一条失败事实。
func normalizeDispatchFailure(in DispatchFailureInputs) (DispatchFailureInputs, error) {
	if strings.TrimSpace(in.Capability) == "" {
		return in, fmt.Errorf("dispatch failure: capability required")
	}
	if in.Reason == "" {
		in.Reason = protocol.ReasonDownstreamMissing
	}
	if in.Reason != protocol.ReasonDownstreamMissing {
		return in, fmt.Errorf("dispatch failure: reason %q 不是未解析派发失败（只允许 %q）",
			in.Reason, protocol.ReasonDownstreamMissing)
	}
	if in.Version < 0 {
		return in, fmt.Errorf("dispatch failure: negative declared version %d", in.Version)
	}
	return in, nil
}

// newDispatchFailureTask 构造一条**未落库**的失败 Task 行（ID 已生成）。
//
// 时间只用**真实 server 事务时刻**（created/finished），不编造 Node 过程时间：
// startedAt/result/exitCode 一律没有事实，因此全部留空。
func newDispatchFailureTask(namespaceID, traceID string, parentTaskID *string,
	in DispatchFailureInputs, now int64) (*Task, error) {
	norm, err := normalizeDispatchFailure(in)
	if err != nil {
		return nil, err
	}
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if traceID == "" {
		return nil, fmt.Errorf("dispatch failure: empty trace_id")
	}
	finished := now
	return &Task{
		ID:           NewTaskID(),
		NamespaceID:  namespaceID,
		TraceID:      traceID,
		ParentTaskID: parentTaskID,
		// CapabilityID 留空 = 没有解析出的能力行 UID（落库写 NULL）。
		CapabilityID: "",
		Capability:   norm.Capability,
		Version:      norm.Version,
		Digest:       "",
		Params:       json.RawMessage("{}"),
		Phase:        TaskFailed,
		Attempt:      0,
		Error:        norm.Reason,
		CreatedAt:    now,
		FinishedAt:   &finished,
	}, nil
}

// insertDispatchFailureTaskOn 在调用方事务里写失败 Task 行 + 它的错误观察事件。
//
// 两个写必须同事务：Task 是「这次派发失败了」的实体，事件是「为什么没派出去」的
// 留痕；缺一个都会让 trace 视图半真半假。ex 必须是**同一个 tx**（不能走池）。
func insertDispatchFailureTaskOn(ctx context.Context, ex queryExec, t *Task, in DispatchFailureInputs, now int64) error {
	norm, err := normalizeDispatchFailure(in)
	if err != nil {
		return err
	}
	if t == nil || t.ID == "" || t.Phase != TaskFailed {
		return fmt.Errorf("dispatch failure: bad task row")
	}
	var parent any
	if t.ParentTaskID != nil && *t.ParentTaskID != "" {
		parent = *t.ParentTaskID
	}
	finished := now
	if t.FinishedAt != nil {
		finished = *t.FinishedAt
	}
	if _, err := ex.execContext(ctx,
		`INSERT INTO tasks
		   (id, namespace_id, trace_id, parent_task_id, capability_id, capability, version, digest, params,
		    phase, attempt, error, created_at, finished_at)
		 VALUES (?,?,?,?,NULL,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.NamespaceID, t.TraceID, parent, t.Capability, t.Version, "", "{}",
		TaskFailed, 0, norm.Reason, t.CreatedAt, finished); err != nil {
		return fmt.Errorf("insert dispatch failure task %s: %w", t.ID, err)
	}
	payload, err := json.Marshal(dispatchFailurePayload{
		Capability:   norm.Capability,
		Version:      norm.Version,
		Reason:       norm.Reason,
		Message:      norm.Detail,
		TaskID:       t.ID,
		ParentTaskID: valueOrEmpty(t.ParentTaskID),
	})
	if err != nil {
		return fmt.Errorf("dispatch failure: marshal event payload: %w", err)
	}
	ev := &Event{
		NamespaceID: t.NamespaceID,
		TraceID:     t.TraceID,
		Type:        norm.Reason,
		Payload:     payload,
		CreatedAt:   t.CreatedAt,
	}
	return insertPruneFactEventOn(ctx, ex, ev)
}

// valueOrEmpty 取可选字符串的值（nil → 空串）。
func valueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
