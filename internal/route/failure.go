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

// 失败兜底（#700）：被 timeout 杀掉的进程组不会执行任何回写，收尾只能由中枢落。
// DispatchFailure 落两件事：trace 里一条终态事实（带原因码），以及能力在
// `lifecycle.onFailure` 里声明的兜底下游；没有声明时只做前者（平台不猜哪个像收尾）。

// TaskFailureReason 取任务结果里的失败原因码（没有结果 → 空串）。
func TaskFailureReason(task *store.Task) string {
	if task == nil || len(task.ResultJSON) == 0 {
		return ""
	}
	var result store.TaskResult
	if err := json.Unmarshal(task.ResultJSON, &result); err != nil {
		return ""
	}
	return result.Reason
}

func (r *Router) DispatchFailure(ctx context.Context, task *store.Task) error {
	if task == nil || task.Phase != store.TaskFailed {
		return nil
	}
	if err := r.db.InsertRouteObservation(ctx, task.NamespaceID, task.TraceID, failureObservationReason(task),
		map[string]any{
			// 键名不能叫 `reason`：那是剪枝原因码的事实，同名不同义会污染 status.pruned.reason。
			"taskId": task.ID, "capability": task.Capability,
			"phase": task.Phase, "failureReason": TaskFailureReason(task), "error": task.Error,
		}); err != nil {
		return err
	}
	fallback, err := r.failureFallback(ctx, task)
	if err != nil {
		return err
	}
	if fallback == nil {
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

func failureObservationReason(task *store.Task) string {
	if len(task.Outcomes) > 0 {
		return protocol.ReasonRouterFailed
	}
	return protocol.ReasonTaskFailed
}

// failureFallback 读父任务钉住版本上的兜底声明；能力行已被删除等于没有声明。
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

// fallbackData 合并父任务参数与声明里的字面参数（声明优先）：上下文由父任务带过来。
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
