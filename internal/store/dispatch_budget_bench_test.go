// 选点预算表的库访问成本（#681 review ③）：一次 1Hz sweep 对每条任务 × 每台候选读一次。这里度量一次 sweep
// （100 条 pending × 2 台在线节点）：memo 共用一份（快照/real 占用按节点+能力只读一次），fresh 逐任务全量查。
package store_test

import (
	"context"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

const budgetSweepTasks = 100

type budgetSweepEnv struct {
	db      *store.DB
	tasks   []*store.Task
	nodeIDs []string
	now     int64
}

func newBudgetSweepEnv(tb testing.TB) *budgetSweepEnv {
	tb.Helper()
	ctx := context.Background()
	env := newBenchEnv(tb)

	second := &store.Node{Name: "n2-" + store.NewUserID(), Status: store.NodeStatusOnline,
		DeclaredNamespaces: []string{"*"}}
	if _, err := env.db.CreateNode(ctx, second); err != nil {
		tb.Fatalf("create second node: %v", err)
	}
	if _, err := env.db.UpsertInstances(ctx, second.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		tb.Fatalf("upsert second node instances: %v", err)
	}

	tasks := make([]*store.Task, 0, budgetSweepTasks)
	for i := 0; i < budgetSweepTasks; i++ {
		tasks = append(tasks, env.newTask("bench-budget-"+store.NewUserID()))
	}
	if err := env.db.CreateTasks(ctx, tasks); err != nil {
		tb.Fatalf("create tasks: %v", err)
	}
	return &budgetSweepEnv{db: env.db, tasks: tasks, nodeIDs: []string{env.nodeID, second.ID}, now: store.Now()}
}

// TestDispatchBudgetSweepFixtureIsReal 是上面那个基准的护栏：夹具必须真的给出两台节点的余量。
func TestDispatchBudgetSweepFixtureIsReal(t *testing.T) {
	ctx := context.Background()
	env := newBudgetSweepEnv(t)

	for _, memo := range []*store.DispatchBudgetMemo{nil, {}, {}} {
		budgets, err := env.db.DispatchBudgetsMemoized(ctx, env.tasks[0], env.nodeIDs, env.now, memo)
		if err != nil {
			t.Fatalf("dispatch budgets: %v", err)
		}
		if len(budgets) != len(env.nodeIDs) {
			t.Fatalf("应给出 %d 台节点的余量，得到 %d 条", len(env.nodeIDs), len(budgets))
		}
		for _, nodeID := range env.nodeIDs {
			got, ok := budgets[nodeID]
			if !ok || got.TotalLimit <= 0 {
				t.Fatalf("节点 %s 的余量表没读出来（%+v）", nodeID, got)
			}
			if got.PinnedUsed != 0 {
				t.Fatalf("首条任务不该把别人的 pending 当占用：%+v", got)
			}
		}
	}
}

func BenchmarkDispatchBudgetSweep(b *testing.B) {
	env := newBudgetSweepEnv(b)
	ctx := context.Background()

	for _, mode := range []struct {
		name string
		memo bool
	}{{"memo", true}, {"fresh", false}} {
		b.Run(mode.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var memo *store.DispatchBudgetMemo
				if mode.memo {
					memo = &store.DispatchBudgetMemo{}
				}
				for _, task := range env.tasks {
					if _, err := env.db.DispatchBudgetsMemoized(ctx, task, env.nodeIDs, env.now, memo); err != nil {
						b.Fatalf("dispatch budgets: %v", err)
					}
				}
			}
		})
	}
}
