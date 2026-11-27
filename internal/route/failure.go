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

func (r *Router) DispatchFailure(ctx context.Context, task *store.Task) error {
	if task == nil || (task.Phase != store.TaskFailed && task.Phase != store.TaskAbandoned) {
		return nil
	}
	reason := TaskFailureReason(task)
	if err := r.db.InsertRouteObservation(ctx, task.NamespaceID, task.TraceID, failureObservationReason(task),
		map[string]any{
			"taskId": task.ID, "capability": task.Capability, // failureReason 不叫 reason（后者是剪枝码）
			"phase": task.Phase, "failureReason": reason, "error": task.Error,
		}); err != nil {
		return err
	}
	fallback, err := r.failureFallback(ctx, task)
	if err != nil {
		return err
	}
	if fallback == nil || !fallbackApplies(fallback, reason) {
		return nil
	}
	data, err := fallbackData(task.Params, fallback.Params)
	if err != nil {
		return err
	}
	plan, err := r.PlanFromEmit(ctx, task, fallback.To, data)
	if err != nil {
		return err
	}
	return r.db.CreateOutcomeDispatch(ctx, store.OutcomeDispatchInputs{
		NamespaceID:  task.NamespaceID,
		TraceID:      task.TraceID,
		ParentTaskID: task.ID,
		Now:          r.Now(),
		Tasks:        plan.Tasks(task.ID),
		Failures:     plan.FailureInputs(),
		Prunes:       plan.PruneInputs(),
	})
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
