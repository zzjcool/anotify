// 跨包基准：dispatch 链路（需要 internal/route）。
//
// **build tag 是临时状态**：#205 阶段 1 删掉了 store 的组织层（org_id / orgs /
// org_members），而 internal/route 仍按旧签名构造 store.Task/.NodePin/.Event ——
// 阶段 2 才适配。在那之前 route 包编译不过，这个文件若默认参与编译，会让
// `go test ./internal/store/` 整个测试二进制构建失败。
//
// 阶段 2 合并后：删掉下面这行 build tag（其余代码不用动），
// 或者直接把本文件挪到 internal/route/bench_test.go。
//
//go:build dispatchbench

package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// 编译期引用：这个符号就是基准被测的对象。若将来改名/删签名，这里**编译不过**；
// 否则基准会静默跑空（Go 的 benchmark 对「什么都没测」毫无抵抗，
// 仍然会打印出一组看起来正常的 ns/op）。
var _ = (*route.Dispatcher).Dispatch

// BenchmarkDispatchChain 度量「一次事件走完派发全路径」的单跳总成本：
//
//	EdgesFrom → EnsureGraphVersion → GetCapabilityByID → 参数校验 →
//	NodesForCapability → FreeSlotsByNode → LeaseInstance → CreateTasks → AssignTask
//
// 用真 *store.DB + 真 route.Dispatcher（而不是 mock）：这条链的成本大头在 SQL 与
// 事务上，mock 掉就什么都没测到。
//
// 固定规模写死：1 能力 / 1 节点 / 1 槽位 / 每跳 1 任务。
func BenchmarkDispatchChain(b *testing.B) {
	env := newBenchEnv(b)
	nodeID := env.addEntryGraphWithNode(b)
	ctx := context.Background()
	// Publisher 留 nil：节点不在线场景就是只落库不推送（派发本身仍是真的）。
	disp := route.New(env.db)
	params := map[string]any{"number": 42}

	var prev []*store.Task
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 槽位只有 1 个且派发后一直被占用：先在计时外把上一轮的放掉，
		// 否则从第二轮起 LeaseInstance 返回 ErrNoCapacity，任务留在 pending，
		// 基准就变成测「没派出去」的成本了（最典型的假基准）。
		if len(prev) > 0 {
			b.StopTimer()
			for _, t := range prev {
				if t.NodeID == nil || t.InstanceID == nil {
					continue
				}
				if err := env.db.ReleaseInstance(ctx, *t.NodeID, *t.InstanceID); err != nil {
					b.Fatalf("release instance: %v", err)
				}
			}
			b.StartTimer()
		}

		// Out 必须非空：空 Out 的语义是「上游没回报出口」，Dispatch 会在查表前
		// 就正常返回（out.Tasks 为空）——基准会静默变成测空气。
		// 这三条断言就是防止这种静默失效：没真派出去就直接失败。
		out, err := disp.Dispatch(ctx, route.DispatchInput{
			NamespaceID:  env.nsID,
			TraceID:      fmt.Sprintf("trc_dsp_%d", i),
			FromIsSource: true,
			FromCap:      "github",
			Out:          "pull_request.opened",
			Params:       params,
		})
		if err != nil {
			b.Fatalf("dispatch: %v", err)
		}
		if len(out.Tasks) != 1 {
			b.Fatalf("应派出 1 条任务，实际 %d（pruned=%d）", len(out.Tasks), len(out.Pruned))
		}
		got := out.Tasks[0]
		if got.Status != store.TaskAssigned || got.NodeID == nil || *got.NodeID != nodeID {
			b.Fatalf("任务没有真正派到节点：status=%q node=%v", got.Status, got.NodeID)
		}
		prev = out.Tasks
	}
}

func TestBenchmarksTargetRealPaths(t *testing.T) {
	ctx := context.Background()
	env := newBenchEnv(t)

	// —— ① CreateTasks：建 5 条 → 读回正好 5 条 pending ——
	// 5 是基准的典型规模；断言读回而不是只看 err，才能证明真的落库了。
	const traceID = "trc_guard_create"

	// 负样本：另建 1 条**别的 trace** 的任务（同组织，FK 天然满足）。
	// 为什么必须有它：若库里只有那 5 行，ListTasks 的 trace_id 过滤整个失效
	// （比如哪天被改成不过滤）也照样读回 5 条——断言看起来在验过滤，其实在验「表里有 5 行」。
	// 有了这条别的 trace，下面的 len==5 就变成了「过滤真的把别人的行挡掉了」。
	const otherTraceID = "trc_guard_other"
	if err := env.db.CreateTasks(ctx, []*store.Task{env.newTask(otherTraceID)}); err != nil {
		t.Fatalf("create negative-sample task: %v", err)
	}

	ts := make([]*store.Task, 5)
	for i := range ts {
		ts[i] = env.newTask(traceID)
	}
	if err := env.db.CreateTasks(ctx, ts); err != nil {
		t.Fatalf("create tasks: %v", err)
	}
	back, err := env.db.ListTasks(ctx, env.nsID, traceID, "", "", 50)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(back) != 5 {
		t.Fatalf("应读回 5 条任务（跨 trace 负样本必须被过滤掉），实际 %d", len(back))
	}
	for _, tk := range back {
		// 逐条确认 trace 归属：即便将来负样本也变成了 5 条（比如两批等长），
		// 这条也能把「过滤串了 trace」钉死。
		if tk.TraceID != traceID {
			t.Fatalf("读回的任务 trace 应为 %s，实际 %q（id=%s）", traceID, tk.TraceID, tk.ID)
		}
		if tk.Status != store.TaskPending {
			t.Fatalf("新建任务必须是 pending，实际 %q（id=%s）", tk.Status, tk.ID)
		}
	}

	// —— ② Transition：基准用的两条边必须合法 ——
	// 状态机是单一真值源。若哪天契约收紧了边，基准会先在这里变红，
	// 而不是在跑基准时才报 invalid_transition。
	if !store.ValidTransition(store.TaskPending, store.TaskAssigned) {
		t.Fatalf("状态机不接受 pending→assigned（BenchmarkTransition 的第一条边不合法）")
	}
	if !store.ValidTransition(store.TaskRunning, store.TaskSucceeded) {
		t.Fatalf("状态机不接受 running→succeeded（BenchmarkTransition 的第二条边不合法）")
	}

	// 真迁一条：pending → assigned，读回状态确实变了（CAS 命中）。
	task := back[0]
	if err := env.db.Transition(ctx, env.nsID, task.ID,
		store.TaskPending, store.TaskAssigned); err != nil {
		t.Fatalf("transition pending→assigned: %v", err)
	}
	cur, err := env.db.GetTask(ctx, env.nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if cur.Status != store.TaskAssigned {
		t.Fatalf("迁移后状态应为 assigned，实际 %q", cur.Status)
	}
	if cur.FinishedAt != nil {
		t.Fatalf("非终态不该有 finished_at，实际 %v", *cur.FinishedAt)
	}

	// 再推到终态：running→succeeded 会额外写 finished_at（第二个子用例测的正是它）。
	if err := env.db.Transition(ctx, env.nsID, task.ID,
		store.TaskAssigned, store.TaskRunning); err != nil {
		t.Fatalf("transition assigned→running: %v", err)
	}
	if err := env.db.Transition(ctx, env.nsID, task.ID,
		store.TaskRunning, store.TaskSucceeded); err != nil {
		t.Fatalf("transition running→succeeded: %v", err)
	}
	cur, err = env.db.GetTask(ctx, env.nsID, task.ID)
	if err != nil {
		t.Fatalf("get task after terminal transition: %v", err)
	}
	if cur.Status != store.TaskSucceeded {
		t.Fatalf("终态迁移后状态应为 succeeded，实际 %q", cur.Status)
	}
	if cur.FinishedAt == nil {
		t.Fatalf("终态必须写 finished_at（BenchmarkTransition 第二个子用例的被测行为）")
	}

	// —— ③ DispatchChain：真 Dispatcher 走完全路径，任务必须真的 assigned 到节点 ——
	nodeID := env.addEntryGraphWithNode(t)
	disp := route.New(env.db)
	out, err := disp.Dispatch(ctx, route.DispatchInput{
		NamespaceID:  env.nsID,
		TraceID:      "trc_guard_dispatch",
		FromIsSource: true,
		FromCap:      "github",
		Out:          "pull_request.opened",
		Params:       map[string]any{"number": 42},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("应派出 1 条任务，实际 %d（pruned=%d）", len(out.Tasks), len(out.Pruned))
	}
	dispatched := out.Tasks[0]
	if dispatched.Status != store.TaskAssigned {
		t.Fatalf("派发后状态应为 assigned，实际 %q", dispatched.Status)
	}
	if dispatched.GraphID == "" {
		t.Fatalf("任务必须带 graph_id（派发时按需落版的图版本）")
	}
	if dispatched.NodeID == nil || *dispatched.NodeID != nodeID {
		t.Fatalf("任务应落在唯一节点 %s 上，实际 %v", nodeID, dispatched.NodeID)
	}
	if dispatched.Attempts != 1 {
		t.Fatalf("首次派发后 attempts 应为 1，实际 %d", dispatched.Attempts)
	}

	// 返回值是内存里那份，不能当证据——库里读回才是真的。
	stored, err := env.db.GetTask(ctx, env.nsID, dispatched.ID)
	if err != nil {
		t.Fatalf("get dispatched task: %v", err)
	}
	if stored.Status != store.TaskAssigned {
		t.Fatalf("库里读回的任务状态应为 assigned，实际 %q", stored.Status)
	}

	// 图版本必须真落了：graph_id 只有能反查回快照才有复盘价值。
	if _, err := env.db.GetGraphVersion(ctx, env.nsID, dispatched.GraphID); err != nil {
		t.Fatalf("get graph version %s: %v", dispatched.GraphID, err)
	}

	// —— ④ ReportResult：基准只报 ns/op，跑空了也照样打印。这里把
	// 「回报一次结果」的可观测效果钉死：终态 + 明细落库 + 槽位释放。
	//
	// 用 ③ 派出去的那条任务（assigned）→ StartTask（running）→ 回报成功。
	if err := env.db.StartTask(ctx, env.nsID, dispatched.ID); err != nil {
		t.Fatalf("start task: %v", err)
	}
	fixOut := "fix"
	if err := env.db.ReportResult(ctx, env.nsID, dispatched.ID, store.TaskSucceeded,
		&fixOut, true, "done", []string{"run.log"}, ""); err != nil {
		t.Fatalf("report result: %v", err)
	}
	finished, err := env.db.GetTask(ctx, env.nsID, dispatched.ID)
	if err != nil {
		t.Fatalf("get finished task: %v", err)
	}
	if finished.Status != store.TaskSucceeded {
		t.Fatalf("回报后状态应为 succeeded，实际 %q", finished.Status)
	}
	if finished.Out == nil || *finished.Out != fixOut || !finished.Reported {
		t.Fatalf("回报的出口没落库：out=%v reported=%v", finished.Out, finished.Reported)
	}
	if finished.Summary != "done" || len(finished.Artifacts) != 1 || finished.Artifacts[0] != "run.log" {
		t.Fatalf("明细没落库：summary=%q artifacts=%v", finished.Summary, finished.Artifacts)
	}
	if finished.FinishedAt == nil {
		t.Fatalf("终态必须写 finished_at")
	}
	// 槽位释放已并进 ReportResult 的同一个事务（#141）：库里读回必须已空。
	// 没释放的话，基准里那条 ReleaseInstance 就成了安慰剂，ns/op 少算一次写。
	insts, err := env.db.ListInstances(ctx, nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range insts {
		if in.BusyTask != nil && *in.BusyTask == dispatched.ID {
			t.Fatalf("回报后槽位 %s 仍被任务 %s 占着（ReleaseInstance 没并进事务？）", in.ID, dispatched.ID)
		}
	}
}
