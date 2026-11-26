package route

import (
	"context"
	"fmt"

	"github.com/zzjcool/anotify/internal/protocol"
)

// 派发预算（CONTRACT.md A7）：环是**合法编排**，兜底就是这两个上限，不是「同名就剪」。
// 顺序固定：先 hop（祖先链，一次查询）再 budget（一次 COUNT），命中只留前者。
const (
	DefaultMaxHops         = 16
	DefaultTraceTaskBudget = 512
)

// Budget 是一次派发的两个预算（<= 0 回落默认值：被关掉的预算等于没有防线）。
type Budget struct {
	MaxHops         int
	TraceTaskBudget int
}

func (b Budget) maxHops() int {
	if b.MaxHops > 0 {
		return b.MaxHops
	}
	return DefaultMaxHops
}

func (b Budget) traceTaskBudget() int {
	if b.TraceTaskBudget > 0 {
		return b.TraceTaskBudget
	}
	return DefaultTraceTaskBudget
}

// WithBudget 覆盖预算（装配层用；返回自身便于链式）。
func (r *Router) WithBudget(b Budget) *Router {
	r.budget = b
	return r
}

// checkBudget 是**唯一**的预算判定点：源入口 / 能力 emit / 判决结局三条路径共用。
// parentTaskID 为空 = 入口派发（要建的根任务 depth 从 1 起算），否则 = 父任务深度 + 1。
// budget 只数**已存在**的行（本次要建的不算）；并发窗口照旧（plan 与建行不同事务）。
func (r *Router) checkBudget(ctx context.Context, namespaceID, traceID, parentTaskID string) (*Prune, error) {
	depth := 1
	if parentTaskID != "" {
		parentDepth, err := r.db.AncestorDepth(ctx, namespaceID, parentTaskID)
		if err != nil {
			return nil, fmt.Errorf("route: 父任务 %s 的深度: %w", parentTaskID, err)
		}
		depth = parentDepth + 1
	}
	if maxHops := r.budget.maxHops(); depth >= maxHops {
		return &Prune{
			Reason: protocol.ReasonMaxHops,
			Detail: fmt.Sprintf("hop depth %d >= max_hops %d", depth, maxHops),
		}, nil
	}
	budget := r.budget.traceTaskBudget()
	built, err := r.db.CountTasksByTrace(ctx, namespaceID, traceID)
	if err != nil {
		return nil, fmt.Errorf("route: 统计 trace %s 的任务数: %w", traceID, err)
	}
	if built >= budget {
		return &Prune{
			Reason: protocol.ReasonBudget,
			Detail: fmt.Sprintf("trace 已有 %d 个任务 >= budget %d", built, budget),
		}, nil
	}
	return nil, nil
}
