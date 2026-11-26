package route

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// 判决执行（#522 / A3）：能力只留结局名，平台按 outcomes 表派发（只在成功终态之后）。

// ErrOutcomeNotDeclaredExec：任务行的判决选了表里没有的结局（fail closed，不猜）。
var ErrOutcomeNotDeclaredExec = errors.New("route: 判决的结局不在任务行的 outcomes 表里")

// PlanFromOutcome 把已选结局的动作解析成下游计划。
func (r *Router) PlanFromOutcome(ctx context.Context, parent *store.Task, decision *store.Decision) (*Plan, error) {
	if parent == nil {
		return nil, fmt.Errorf("route: nil parent task")
	}
	if decision == nil {
		return nil, fmt.Errorf("route: nil decision")
	}
	outcomes := store.ParseOutcomes(parent.Outcomes)
	actions, declared := outcomes[decision.Outcome]
	if !declared {
		return nil, fmt.Errorf("%w: %q（task %s，已声明：%v）",
			ErrOutcomeNotDeclaredExec, decision.Outcome, parent.ID, sortedNames(outcomes))
	}
	plan := &Plan{NamespaceID: parent.NamespaceID, TraceID: parent.TraceID}
	if prune, err := r.checkBudget(ctx, parent.NamespaceID, parent.TraceID, parent.ID); err != nil {
		return nil, err
	} else if prune != nil {
		plan.Pruned = append(plan.Pruned, *prune)
		return plan, nil
	}
	for _, action := range actions {
		ref := action.Capability
		rendered, missing := sourcespec.RenderDecisionParams(action.Params, decision.Params)
		if len(missing) > 0 {
			detail := "需要 " + strings.Join(missing, ", ")
			name := sourcespec.RefName(ref)
			plan.Targets = append(plan.Targets, Target{
				Ref: ref, Capability: name, State: StatePruned,
				Reason: protocol.ReasonParamMissing, Detail: detail,
			})
			plan.Pruned = append(plan.Pruned, Prune{
				Capability: name, Reason: protocol.ReasonParamMissing, Detail: detail,
			})
			continue
		}
		capability, err := r.resolveCapability(ctx, parent.NamespaceID, ref)
		if err != nil {
			return nil, err
		}
		if capability == nil {
			detail := fmt.Sprintf("下游 %q 解析不到", ref)
			plan.Targets = append(plan.Targets, Target{
				Ref: ref, Capability: sourcespec.RefName(ref), Version: sourcespec.DeclaredVersion(ref),
				State: StateMissing, Reason: protocol.ReasonDownstreamMissing, Detail: detail,
			})
			continue
		}
		params, prune := PrepareParams(capability.Spec.Params, rendered)
		if prune != nil {
			prune.Capability = capability.Name
			plan.Targets = append(plan.Targets, prunedTarget(capability, ref, prune.Reason, prune.Detail))
			plan.Pruned = append(plan.Pruned, *prune)
			continue
		}
		// 与源事件同一闸门：停用是剪枝不是失败。
		accepting, aerr := r.capabilityAccepting(ctx, parent.NamespaceID, capability.Name)
		if aerr != nil {
			return nil, aerr
		}
		if !accepting {
			plan.pruneDisabled(capability, ref, capability.Name)
			continue
		}
		plan.Targets = append(plan.Targets, Target{
			Ref: ref, Capability: capability.Name, Version: capability.Version,
			Digest: capability.Digest, CapabilityID: capability.ID,
			Params: params, State: StateReady,
		})
	}
	return plan, nil
}

// DispatchOutcome 在终态之后执行结局（#522）：三种「没有下游」的情形都留观察行、0 任务。
func (r *Router) DispatchOutcome(ctx context.Context, task *store.Task) error {
	if task == nil || len(task.Outcomes) == 0 {
		return nil // 不是 router 任务：没有结局表就没有后置动作
	}
	if task.Phase != store.TaskSucceeded {
		return r.db.InsertRouteObservation(ctx, task.NamespaceID, task.TraceID, protocol.ReasonRouterFailed,
			map[string]any{
				"taskId": task.ID, "capability": task.Capability,
				"phase": task.Phase, "error": task.Error,
			})
	}
	decision := store.ParseDecision(task.Decision)
	if decision == nil {
		return r.db.InsertRouteObservation(ctx, task.NamespaceID, task.TraceID, protocol.ReasonNoDecision,
			map[string]any{"taskId": task.ID, "capability": task.Capability})
	}
	outcomes := store.ParseOutcomes(task.Outcomes)
	actions, declared := outcomes[decision.Outcome]
	if !declared {
		return fmt.Errorf("%w: %q（task %s）", ErrOutcomeNotDeclaredExec, decision.Outcome, task.ID)
	}
	if len(actions) == 0 {
		return r.db.InsertRouteObservation(ctx, task.NamespaceID, task.TraceID, protocol.ReasonOutcomeDropped,
			map[string]any{
				"taskId": task.ID, "capability": task.Capability,
				"outcome": decision.Outcome, "params": decision.Params,
			})
	}
	plan, err := r.PlanFromOutcome(ctx, task, decision)
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

func sortedNames(outcomes map[string][]sourcespec.OutcomeAction) []string {
	names := make([]string, 0, len(outcomes))
	for name := range outcomes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
