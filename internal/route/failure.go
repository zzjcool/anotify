package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func TaskFailureReason(task *store.Task) string {
	if task == nil {
		return ""
	}
	if len(task.ResultJSON) > 0 {
		var result store.TaskResult
		if err := json.Unmarshal(task.ResultJSON, &result); err == nil && result.Reason != "" {
			return result.Reason
		}
	}
	if protocol.IsTaskFailureReason(task.Error) {
		return task.Error
	}
	return ""
}

// DispatchFailure 落一条终态失败事实并（声明允许时）派发兜底；幂等判据在 store 的同一事务里
// （四个调用点都会因帧重放重复进来）。
func (r *Router) DispatchFailure(ctx context.Context, task *store.Task) error {
	if task == nil || (task.Phase != store.TaskFailed && task.Phase != store.TaskAbandoned) {
		return nil
	}
	fact, planErr := r.failureFact(ctx, task)
	if _, err := r.db.RecordFailureFact(ctx, fact); err != nil {
		return err
	}
	return planErr
}

// failureFact 组落账输入；兜底读不出来时仍返回事实（观察行先落），读错误交回调用方。
func (r *Router) failureFact(ctx context.Context, task *store.Task) (store.FailureFact, error) {
	reason := TaskFailureReason(task)
	payload, err := json.Marshal(map[string]any{
		"taskId": task.ID, "capability": task.Capability, // failureReason 不叫 reason（后者是剪枝码）
		"phase": task.Phase, "failureReason": reason, "error": task.Error,
	})
	if err != nil {
		return store.FailureFact{}, fmt.Errorf("失败事实: marshal: %w", err)
	}
	fact := store.FailureFact{
		TaskID: task.ID,
		Observation: store.Event{
			NamespaceID: task.NamespaceID, TraceID: task.TraceID,
			Type: failureObservationReason(task), Payload: payload, CreatedAt: r.Now(),
		},
	}
	fallback, err := r.failureFallback(ctx, task)
	if err != nil {
		return fact, err
	}
	if fallback == nil || !fallbackApplies(fallback, reason) {
		return fact, nil
	}
	data, err := fallbackData(task.Params, fallback.Params)
	if err != nil {
		return fact, err
	}
	plan, err := r.PlanFromEmit(ctx, task, fallback.To, data)
	if err != nil {
		return fact, err
	}
	fact.Dispatch = store.OutcomeDispatchInputs{
		NamespaceID:  task.NamespaceID,
		TraceID:      task.TraceID,
		ParentTaskID: task.ID,
		Now:          r.Now(),
		Tasks:        plan.Tasks(task.ID),
		Failures:     plan.FailureInputs(),
		Prunes:       plan.PruneInputs(),
	}
	return fact, nil
}

func fallbackApplies(fallback *artifact.Fallback, reason string) bool {
	if fallback == nil || len(fallback.Reasons) == 0 {
		return true
	}
	for _, allowed := range fallback.Reasons {
		if allowed == reason {
			return true
		}
	}
	return false
}

func failureObservationReason(task *store.Task) string {
	if len(task.Outcomes) > 0 {
		return protocol.ReasonRouterFailed
	}
	return protocol.ReasonTaskFailed
}

func (r *Router) failureFallback(ctx context.Context, task *store.Task) (*artifact.Fallback, error) {
	if task.CapabilityID == "" {
		return nil, nil
	}
	capability, err := r.db.GetCapabilityByID(ctx, task.NamespaceID, task.CapabilityID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取失败任务的能力 %s: %w", task.Capability, err)
	}
	return capability.Spec.Lifecycle.OnFailure, nil
}

func fallbackData(parent json.RawMessage, declared map[string]any) (json.RawMessage, error) {
	merged := map[string]any{}
	if len(parent) > 0 {
		if err := json.Unmarshal(parent, &merged); err != nil {
			merged = map[string]any{}
		}
	}
	for name, value := range declared {
		merged[name] = value
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("合成兜底参数: %w", err)
	}
	return data, nil
}
