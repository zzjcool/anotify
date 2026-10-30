// store 包**写路径**的基准 + 防静默失效护栏（issue #146）。
//
// 为什么是外部测试包 store_test（而不是同目录其他文件那样的 package store）：
// BenchmarkDispatchChain 要 import internal/route，而 internal/route 反过来
// import internal/store——写成内部测试包会 import cycle。代价是拿不到
// store_bench_test.go 里的内部 helper（benchStore 等），所以这里自带一套
// 只依赖 store 导出 API 的夹具。
//
// 三个基准对应写路径的三个层次，作为任何优化（事务合并 #141、pickNode 聚合 #145、
// 图版本缓存）之前的对比基线：
//
//	BenchmarkCreateTasks    派发的主成本：1 tx 内插 n 行
//	BenchmarkTransition     真正的热点：每任务 5–6 次，1 tx 改 1 行
//	BenchmarkDispatchChain  一条 trace 的单跳总成本：Dispatch 全路径
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// 编译期引用：这三个符号就是基准被测的对象。若将来改名/删签名，
// 这里**编译不过**；否则基准会静默跑空（Go 的 benchmark 对「什么都没测」毫无抵抗，
// 仍然会打印出一组看起来正常的 ns/op）。
var (
	_ = (*store.DB).CreateTasks
	_ = (*store.DB).Transition
	_ = (*route.Dispatcher).Dispatch
)

// benchCapName 是基准用的能力名。取自定义名字前缀 bench.，避免撞上内置能力名
// （内置起点的名字是保留的，CreateCapability 会直接拒绝）。
const benchCapName = "bench.review"

// benchEnv 是三个基准共用的最小可用环境：一个组织 + 一个自定义能力定义。
//
// 为什么不能随手造字符串 ID：tasks.org_id / tasks.capability_id 都有外键，
// store.Open 的 DSN 显式开了 foreign_keys(ON)，伪造父行会直接 FK 失败。
type benchEnv struct {
	db      *store.DB
	orgID   string
	capID   string
	capName string
}

// newBenchEnv 打开内存库并铺好 FK 父行：用户+组织 → 能力定义。
//
// 用 :memory: 而不是临时文件：基准要的是 store 层自身的成本，
// 不该把磁盘 fsync 噪声混进来。
func newBenchEnv(tb testing.TB) *benchEnv {
	tb.Helper()
	ctx := context.Background()

	db, err := store.Open(":memory:")
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	// RegisterUserWithOrg 一步建 user + org + owner 关系（org 是 tasks.org_id 的父行）。
	user := &store.User{ID: store.NewUserID(), Username: "u-" + store.NewUserID(), CreatedAt: store.Now()}
	org, err := db.RegisterUserWithOrg(ctx, user, true)
	if err != nil {
		tb.Fatalf("register user with org: %v", err)
	}

	// 能力定义：params_schema 用空对象（怎么传参都过，不会把派发剪枝掉），
	// outputs 留一个口子让能力形态贴近真实（今后接下游时 from_port 才有封闭集合）。
	capDef := &store.CapabilityVersion{
		OrgID: org.ID, Name: benchCapName, Version: 1,
		ParamsSchema: json.RawMessage(`{}`),
		Outputs:      []string{"fix"},
		Source:       store.CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		tb.Fatalf("create capability: %v", err)
	}

	return &benchEnv{db: db, orgID: org.ID, capID: capDef.ID, capName: capDef.Name}
}

// newTask 造**未落库**的一行 pending 任务（外键父行已由 env 提供）。
//
// 字段取值对齐 Dispatch 真实构造任务时填的那几个：Path 带上目标能力、
// Params 是已过 schema 的 JSON。ID/CreatedAt 故意留空，由 CreateTasks 补
// （那是它的真实职责，也该被度量）。
func (e *benchEnv) newTask(traceID string) *store.Task {
	return &store.Task{
		OrgID:        e.orgID,
		TraceID:      traceID,
		Path:         []string{e.capID},
		CapabilityID: e.capID,
		Capability:   e.capName,
		Version:      1,
		Params:       json.RawMessage(`{"number":42}`),
		Status:       store.TaskPending,
	}
}

// createOne 立刻落一行 pending 任务并返回它（已被 CreateTasks 填好 ID）。
// 只在基准的 b.StopTimer() 段调用——装数据不该计入被测操作。
func (e *benchEnv) createOne(tb testing.TB, ctx context.Context, traceID string) *store.Task {
	tb.Helper()
	t := e.newTask(traceID)
	if err := e.db.CreateTasks(ctx, []*store.Task{t}); err != nil {
		tb.Fatalf("create task: %v", err)
	}
	return t
}

// addEntryGraphWithNode 挂上入口边 + 1 台节点 + 1 个槽位，返回节点 ID。
//
// 固定规模：1 能力 / 1 节点 / 1 槽位 / 每跳 1 任务。规模一变 ns/op 就不可比
// （多节点会摊薄 pickNode 的聚合查询成本），所以夹具里写死，不做参数化。
func (e *benchEnv) addEntryGraphWithNode(tb testing.TB) string {
	tb.Helper()
	ctx := context.Background()

	// 入口边：来源 "github" 的 pull_request.opened → bench.review@1。
	// FromIsSource=true 时左端是**来源名**，出口是外部系统决定的事件类型。
	if err := e.db.CreateEdge(ctx, &store.Edge{
		OrgID:          e.orgID,
		FromIsSource:   true,
		FromCapability: "github",
		FromPort:       "pull_request.opened",
		ToCapability:   e.capID,
	}); err != nil {
		tb.Fatalf("create entry edge: %v", err)
	}

	// 节点默认 offline——NodesForCapability 把 offline 也算「可接单」
	// （offline 是「还没连上」，不是「拒单」；draining 才是拒单）。
	node := &store.Node{OrgID: e.orgID, Name: "n-" + store.NewUserID()}
	if _, err := e.db.CreateNode(ctx, node); err != nil {
		tb.Fatalf("create node: %v", err)
	}
	if _, err := e.db.ConfirmCapabilities(ctx, e.orgID, node.ID,
		[]store.CapabilityRef{{Name: e.capName, Version: 1}}); err != nil {
		tb.Fatalf("confirm capabilities: %v", err)
	}
	if _, err := e.db.UpsertInstances(ctx, e.orgID, node.ID,
		[]store.InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		tb.Fatalf("upsert instances: %v", err)
	}
	return node.ID
}

// BenchmarkCreateTasks 度量「一个事务内批量建 n 条任务」。
//
// 规模取 1 / 5 / 20 且是常量（不随 b.N 变）：
//   - 1  = 单任务基线（不扇出的链）
//   - 5  = 一条 trace 的典型扇出（issue #146 里估算 traces/s 就用 5 任务 trace）
//   - 20 = 大扇出上界
//
// 规模跟着 b.N 变是最常见的假基准：不同规模的样本混在一起算平均，ns/op 既不可比
// 也没法用作优化前后的基线。
func BenchmarkCreateTasks(b *testing.B) {
	env := newBenchEnv(b)
	ctx := context.Background()

	for _, n := range []int{1, 5, 20} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			// 复用切片壳（省掉每轮一次切片分配），里面的 Task 每轮重建：
			// CreateTasks 会回填 ID，复用同一个 Task 会在第二轮撞主键。
			// 每轮新建这批结构体是调用方的真实成本（真派发也是这么造的），照实计入。
			ts := make([]*store.Task, n)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// TraceID 每轮唯一：区分批次，也避免所有行挤在同一个索引前缀上。
				traceID := fmt.Sprintf("trc_crt_%d_%d", n, i)
				for j := range ts {
					ts[j] = env.newTask(traceID)
				}
				if err := env.db.CreateTasks(ctx, ts); err != nil {
					b.Fatalf("create tasks (n=%d): %v", n, err)
				}
			}
			// 备注：tasks 表行数随 b.N 线性增长（每轮 n 行）。基准跑在内存库上，
			// 行数增长只影响内存占用、不影响单次插入语义，可以接受；
			// 不要在计时区内加清理，那会污染被测路径。
		})
	}
}

// BenchmarkTransition 度量「一次状态迁移」（1 tx、1 行 UPDATE）。
//
// 这是真正的热点：一条任务从 pending 走到终态要 5–6 次迁移。
// 两个子用例分别覆盖**非终态写**与**终态写**（后者会额外写 finished_at，
// 多一列、多一次 Now()，成本与前者不同，不能只测一个）。
//
// 装数据一律放在 b.StopTimer() 段：终态不可再变，所以每轮都必须是一行新鲜的
// 任务；而 testing 只在计时区间累计 allocs，StopTimer 段也不会污染 -benchmem。
func BenchmarkTransition(b *testing.B) {
	env := newBenchEnv(b)
	ctx := context.Background()

	b.Run("pending_to_assigned", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			t := env.createOne(b, ctx, fmt.Sprintf("trc_tr_a_%d", i))
			b.StartTimer()

			// 计时区内**恰好一次** Transition。
			if err := env.db.Transition(ctx, env.orgID, t.ID,
				store.TaskPending, store.TaskAssigned); err != nil {
				b.Fatalf("transition pending→assigned: %v", err)
			}
		}
	})

	b.Run("running_to_succeeded", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// 推进到 running 需要两次 Transition，**两次都在计时外**：
			// 若允许其中一次落进计时区，ns/op 就不再是「一次迁移」的成本。
			b.StopTimer()
			t := env.createOne(b, ctx, fmt.Sprintf("trc_tr_s_%d", i))
			if err := env.db.Transition(ctx, env.orgID, t.ID,
				store.TaskPending, store.TaskAssigned); err != nil {
				b.Fatalf("setup pending→assigned: %v", err)
			}
			if err := env.db.Transition(ctx, env.orgID, t.ID,
				store.TaskAssigned, store.TaskRunning); err != nil {
				b.Fatalf("setup assigned→running: %v", err)
			}
			b.StartTimer()

			// 计时区内**恰好一次**、且是终态写（额外落 finished_at）。
			if err := env.db.Transition(ctx, env.orgID, t.ID,
				store.TaskRunning, store.TaskSucceeded); err != nil {
				b.Fatalf("transition running→succeeded: %v", err)
			}
		}
	})
}

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
			OrgID:        env.orgID,
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

// BenchmarkReportResult 度量「节点回报一次结果」的写成本（result 路径）。
//
// 这是 issue #141 里被拆得最碎的一条路径：合并前 ReportResult 内部是
// transition(→running) + started_at + transition(→终态) + details 四次独立提交，
// 槽位释放还要再单开一次（ReleaseInstanceByTask）；合并后应当只剩一次提交。
//
// 装数据一律在计时外：一条 running 的任务需要 pending→assigned→acked→running
// 加占槽位，四步都不该算进「回报结果」的成本。
func BenchmarkReportResult(b *testing.B) {
	env := newBenchEnv(b)
	nodeID := env.addEntryGraphWithNode(b)
	ctx := context.Background()
	out := "fix"
	artifacts := []string{"run.log", "diff.patch"}

	b.Run("running_to_succeeded", func(b *testing.B) {
		// testing 可能对同一个子基准调用多次（校准/重跑），每次进来先把上一轮
		// 调用残留的槽位放掉：否则第一次调用跑完的那个槽位一直 busy，
		// 下一次调用第一轮就 LeaseInstance → ErrNoCapacity。
		// （合并事务后 ReportResult 自己会释放，这段就是空操作，留着不影响可比性）
		if insts, err := env.db.ListInstances(ctx, nodeID); err == nil {
			for _, in := range insts {
				if in.BusyTask != nil {
					if err := env.db.ReleaseInstance(ctx, nodeID, in.ID); err != nil {
						b.Fatalf("reset slot: %v", err)
					}
				}
			}
		}

		var prevInst string
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			// 上一轮的槽位：合并后 ReportResult 自己释放，这里是空操作；
			// 合并前必须显式释放，否则第二轮起 LeaseInstance 直接 ErrNoCapacity，
			// 基准会静默变成测「没回报成」的成本。
			if prevInst != "" {
				if err := env.db.ReleaseInstance(ctx, nodeID, prevInst); err != nil {
					b.Fatalf("release instance: %v", err)
				}
			}
			t := env.createOne(b, ctx, fmt.Sprintf("trc_rr_%d", i))
			instID, err := env.db.LeaseInstance(ctx, nodeID, t.ID)
			if err != nil {
				b.Fatalf("lease instance: %v", err)
			}
			if err := env.db.AssignTask(ctx, env.orgID, t.ID, nodeID, instID, store.Now()+store.LeaseTTLSec); err != nil {
				b.Fatalf("assign: %v", err)
			}
			if err := env.db.Transition(ctx, env.orgID, t.ID, store.TaskAssigned, store.TaskAcked); err != nil {
				b.Fatalf("setup assigned→acked: %v", err)
			}
			if err := env.db.StartTask(ctx, env.orgID, t.ID); err != nil {
				b.Fatalf("setup acked→running: %v", err)
			}
			prevInst = instID
			b.StartTimer()

			// 计时区内**恰好一次**：节点回报结果。
			if err := env.db.ReportResult(ctx, env.orgID, t.ID, store.TaskSucceeded,
				&out, true, "ok", artifacts, ""); err != nil {
				b.Fatalf("report result: %v", err)
			}
		}
	})
}

// TestBenchmarksTargetRealPaths 是三个基准的护栏：对真库把基准依赖的路径各跑一遍，
// 断言**可观测效果**（库里真的多了行、状态真的变了）。
//
// 为什么需要它：基准只报 ns/op，跑空了也照样打印数字。改名、参数变空、桶位被占满，
// 这些失效方式都不会让 go test 变红，只会让基线悄悄失真。本测试把「基准确实
// 测到了真路径」变成一条会失败的断言。
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
	back, err := env.db.ListTasks(ctx, env.orgID, traceID, "", "", 50)
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
	if err := env.db.Transition(ctx, env.orgID, task.ID,
		store.TaskPending, store.TaskAssigned); err != nil {
		t.Fatalf("transition pending→assigned: %v", err)
	}
	cur, err := env.db.GetTask(ctx, env.orgID, task.ID)
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
	if err := env.db.Transition(ctx, env.orgID, task.ID,
		store.TaskAssigned, store.TaskRunning); err != nil {
		t.Fatalf("transition assigned→running: %v", err)
	}
	if err := env.db.Transition(ctx, env.orgID, task.ID,
		store.TaskRunning, store.TaskSucceeded); err != nil {
		t.Fatalf("transition running→succeeded: %v", err)
	}
	cur, err = env.db.GetTask(ctx, env.orgID, task.ID)
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
		OrgID:        env.orgID,
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
	stored, err := env.db.GetTask(ctx, env.orgID, dispatched.ID)
	if err != nil {
		t.Fatalf("get dispatched task: %v", err)
	}
	if stored.Status != store.TaskAssigned {
		t.Fatalf("库里读回的任务状态应为 assigned，实际 %q", stored.Status)
	}

	// 图版本必须真落了：graph_id 只有能反查回快照才有复盘价值。
	if _, err := env.db.GetGraphVersion(ctx, env.orgID, dispatched.GraphID); err != nil {
		t.Fatalf("get graph version %s: %v", dispatched.GraphID, err)
	}

	// —— ④ ReportResult：基准只报 ns/op，跑空了也照样打印。这里把
	// 「回报一次结果」的可观测效果钉死：终态 + 明细落库 + 槽位释放。
	//
	// 用 ③ 派出去的那条任务（assigned）→ StartTask（running）→ 回报成功。
	if err := env.db.StartTask(ctx, env.orgID, dispatched.ID); err != nil {
		t.Fatalf("start task: %v", err)
	}
	fixOut := "fix"
	if err := env.db.ReportResult(ctx, env.orgID, dispatched.ID, store.TaskSucceeded,
		&fixOut, true, "done", []string{"run.log"}, ""); err != nil {
		t.Fatalf("report result: %v", err)
	}
	finished, err := env.db.GetTask(ctx, env.orgID, dispatched.ID)
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
