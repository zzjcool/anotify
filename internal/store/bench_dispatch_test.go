// 派发全链基准（v4 迁移，issue #338）。
//
// 一次「源事件」走完派的全部写路径：
//
//	source.Service.Ingest                    载荷装配 → 路由投影 → 事件/投递窗/任务/回执，一个事务
//	  └ route.Router.PlanFromSource          逐行解析 targets：能力引用 → 参数校验/补默认
//	store.AssignTask                         记派发候选（1 行 UPDATE）
//	store.GrantLeaseAuthorized               认证节点身份 → 占真实槽位 → 发租约（1 事务）
//	store.CompleteTaskClassifiedAuthorized   回报终态 → 释放槽位（1 事务）
//
// ── 这次为什么重写（v3 → v4 的差异都是硬事实，不是口味）──────────────────────
//
// v3 的旧文件引用了 route.Dispatcher / Task.InstanceID / ReleaseInstance / TaskAssigned
// 与 9 态状态机 —— 这些在 v4 全部不存在：route 只剩**纯投影** Router（不写库、不派发），
// 任务改成 phase + 租约，槽位由 complete 在同一事务里释放。旧文件靠 `dispatchbench`
// build tag 关着，`go test ./...` 看不见它，只有 make bench-dispatch 会红：
// 这就是「基 quietly 腐烂」本身（#315 ③ 想防的正是它）。所以：
//
//   - 删掉 build tag。route import store、store 不 import route，外部测试包 store_test
//     同时引两者不成环，没有必要再埋一颗 tag 后面的雷；
//   - 名字冲突一并消失：v4 的 store 写路径护栏在 bench_test.go
//     （TestBenchmarksTargetRealPaths），本文件的全链护栏叫 TestDispatchChainTouchesRealPaths。
//
// 基准只报 ns/op，跑空了也照样打印数字 —— 所以下面那条护栏用例把「真的派到节点、
// 真的占槽、真的回收」钉成会失败的断言，而不是靠人肉读数字。
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// 编译期引用：这些符号就是基准被测的对象。若将来改名/删签名，这里**编译不过**；
// 否则基准会静默跑空。
var (
	_ = (*source.Service).Ingest
	_ = (*route.Router).PlanFromSource
	_ = (*store.DB).AssignTask
	_ = (*store.DB).GrantLeaseAuthorized
	_ = (*store.DB).CompleteTaskClassifiedAuthorized
)

// dispatchEnv 是派发全链的夹具：复用 benchEnv 的用户/命名空间/能力/在线节点（1 槽），
// 再加一条真源行（入 targets 指向那个能力）。
type dispatchEnv struct {
	*benchEnv
	src    *store.Source
	router *route.Router
	svc    *source.Service
	auth   store.NodeAuthorization
}

// newDispatchEnv 铺好一条可触发的最小源：kind=manual + payload 打底 + 一个 target。
//
// 源必须**真落库**：Ingest 的 CAS 拿源行上的 resource_version、事件表要 source_id 外键，
// 内存里拼一个结构体是跑不通的（那也就测不到真路径了）。
func newDispatchEnv(tb testing.TB) *dispatchEnv {
	tb.Helper()
	ctx := context.Background()
	env := newBenchEnv(tb)

	src := &store.Source{
		NamespaceID: env.nsID,
		Name:        "bench-trigger",
		Kind:        store.SourceKindManual,
		// payload 打底走源配置（v4 的 manual 源就是这么带静态数据的）。
		Config: json.RawMessage(`{"payload":{"number":42}}`),
		// 一个 target、无参数模板：目标能力校验通过后按 spec.params 补默认值。
		Targets: []sourcespec.Target{{Capability: env.capName}},
		Enabled: true,
	}
	if err := env.db.CreateSource(ctx, src); err != nil {
		tb.Fatalf("create source: %v", err)
	}
	// 读回权威事实：CreateSource 填的是内存值，resource_version 以库里那份为准
	// （Ingest 会拿它做 CAS，值不对就是 ErrConflict，基准会在第一轮就红）。
	stored, err := env.db.GetSource(ctx, env.nsID, src.ID)
	if err != nil {
		tb.Fatalf("get source: %v", err)
	}

	router := route.New(env.db)
	return &dispatchEnv{
		benchEnv: env,
		src:      stored,
		router:   router,
		svc:      source.New(env.db, router),
		auth:     env.auth,
	}
}

// fire 触发一次事件并把它派到节点上（assign → grant），返回 (taskID, leaseID, attempt)。
//
// 这是基准计时区里的**全部**工作：Ingest（事件+任务原子写）→ assign → grant。
func (e *dispatchEnv) fire(tb testing.TB, ctx context.Context) (string, string, int) {
	tb.Helper()
	receipt, err := e.svc.Ingest(ctx, source.Input{
		Source: e.src,
		Type:   "bench.trigger",
		// webhook 形状：载荷来自投递体；manual 源则以配置打底、这里一层覆盖。
		Payload: json.RawMessage(`{"number":42}`),
	})
	if err != nil {
		tb.Fatalf("ingest: %v", err)
	}
	// 回执必须真有任务：0 条 = 路由/参数校验静默剪枝了，基准测的就是空气。
	if len(receipt.Tasks) != 1 {
		tb.Fatalf("应派出 1 条任务，实际 %d（pruned=%d）", len(receipt.Tasks), len(receipt.Pruned))
	}
	taskID := receipt.Tasks[0].TaskID
	if taskID == "" {
		tb.Fatalf("回执里的任务没有 id: %+v", receipt.Tasks[0])
	}
	assignmentID := "asg_" + taskID
	if err := e.db.AssignTask(ctx, taskID, e.nodeID, assignmentID, 1); err != nil {
		tb.Fatalf("assign %s: %v", taskID, err)
	}
	leaseID, _, attempt, _, err := e.db.GrantLeaseAuthorized(ctx, e.auth, taskID, assignmentID, 1, 0, store.Now())
	if err != nil {
		tb.Fatalf("grant %s: %v", taskID, err)
	}
	return taskID, leaseID, attempt
}

// reap 结束一次执行并释放槽位（v4：release 并进 complete 的同一个事务，没有独立 API）。
//
// 不回收的话第二轮 grant 会撞 ErrNoCapacity —— 任务留在 pending，基准就变成
// 「测派不出去的成本」（最典型的假基准）。
func (e *dispatchEnv) reap(tb testing.TB, ctx context.Context, taskID, leaseID string, attempt int) {
	tb.Helper()
	start := time.Now().UTC()
	result := store.TaskResult{
		Phase:      store.TaskSucceeded,
		Summary:    "ok",
		Artifacts:  []protocol.TaskArtifact{},
		StartedAt:  start,
		FinishedAt: start.Add(time.Second),
	}
	if _, err := e.db.CompleteTaskClassifiedAuthorized(ctx, e.auth, taskID, attempt, leaseID, store.Now(), result); err != nil {
		tb.Fatalf("complete %s: %v", taskID, err)
	}
}

// BenchmarkDispatchChain 度量「一次源事件走完派发全链」的单跳总成本。
//
// 规模写死为最小可真跑的形态：1 源 / 1 target / 1 能力 / 1 节点 / 1 槽位 / 每跳 1 任务。
// 成本大头在 SQL 与事务上，所以用真 *store.DB + 真 route.Router + 真 Ingest
// （而不是 mock）：mock 掉以后这条链上什么都不剩。
func BenchmarkDispatchChain(b *testing.B) {
	env := newDispatchEnv(b)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 计时区内**恰好一次**完整的派发：ingest（事件+任务）→ assign → grant → complete（释槽）。
		taskID, leaseID, attempt := env.fire(b, ctx)
		env.reap(b, ctx, taskID, leaseID, attempt)
	}
}

// TestDispatchChainTouchesRealPaths 是全链基准的护栏：对真库把基准依赖的路径各跑一遍，
// 断言**可观测效果**（事件落库、任务 pending → running → succeeded、槽位绑上又放开）。
//
// 为什么必须有它：基准只报 ns/op，跑空了也照样打印数字。改名、target 解析失败被剪枝、
// 槽位没释放，这些失效方式都不会让 bench 变红，只会让基线悄悄失真。
func TestDispatchChainTouchesRealPaths(t *testing.T) {
	env := newDispatchEnv(t)
	ctx := context.Background()

	// ① Ingest 真的落了事件 + 1 条 pending 任务
	receipt, err := env.svc.Ingest(ctx, source.Input{
		Source:  env.src,
		Type:    "bench.guard",
		Payload: json.RawMessage(`{"number":42}`),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if receipt.EventID == "" || receipt.TraceID == "" {
		t.Fatalf("回执缺 event/trace: %+v", receipt)
	}
	if receipt.Duplicate {
		t.Fatalf("首次投递不该是重复: %+v", receipt)
	}
	if len(receipt.Tasks) != 1 {
		t.Fatalf("应派出 1 条任务，实际 %d（pruned=%d）", len(receipt.Tasks), len(receipt.Pruned))
	}
	var events int
	if err := env.db.QueryRow(
		`SELECT count(*) FROM events WHERE namespace_id = ? AND trace_id = ?`,
		env.nsID, receipt.TraceID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("事件行数 = %d, want 1（Ingest 的原子写没落到 events）", events)
	}

	taskID := receipt.Tasks[0].TaskID
	pending, err := env.db.GetTask(ctx, env.nsID, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if pending.Phase != store.TaskPending {
		t.Fatalf("新建任务必须是 pending，实际 %q", pending.Phase)
	}
	if pending.Capability != env.capName || pending.Digest != env.digest {
		t.Fatalf("任务没钉住解析出的能力身份: %+v", pending)
	}
	if len(pending.Params) == 0 {
		t.Fatalf("任务必须带已过 schema 的 params")
	}

	// ② assign + grant 真的占住了槽位、发了租约
	leaseID, attempt := env.grantOne(t, ctx, taskID)
	if leaseID == "" || attempt != 1 {
		t.Fatalf("grant 结果不完整: lease=%q attempt=%d", leaseID, attempt)
	}
	running, err := env.db.GetTask(ctx, env.nsID, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if running.Phase != store.TaskRunning || running.LeaseID != leaseID || running.LeaseUntil == nil {
		t.Fatalf("grant 后相位/租约不对: %+v", running)
	}
	insts, err := env.db.ListInstances(ctx, env.nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(insts) != 1 || insts[0].BusyTask == nil || *insts[0].BusyTask != taskID {
		t.Fatalf("grant 后槽位应绑到任务: %+v", insts)
	}

	// ③ complete 真的写终态并**在同一事务里**释放槽位（v4 没有独立 ReleaseInstance）
	env.reap(t, ctx, taskID, leaseID, attempt)
	done, err := env.db.GetTask(ctx, env.nsID, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if done.Phase != store.TaskSucceeded || done.FinishedAt == nil {
		t.Fatalf("complete 后相位不对: %+v", done)
	}
	insts, err = env.db.ListInstances(ctx, env.nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if insts[0].BusyTask != nil {
		t.Fatalf("complete 后槽位应被释放: %+v", insts[0])
	}

	// ④ 再来一轮：槽位真回收了才可能成功（基准跑多轮的隐含前提）。
	// 少了这条，「bench 只跑得出第 1 轮」这种失效会一路静默到有人拿数字做对比时。
	taskID2, leaseID2, attempt2 := env.fire(t, ctx)
	env.reap(t, ctx, taskID2, leaseID2, attempt2)

	// ⑤ 两轮派发各留一个 trace（派发是**追加**事实，不改写历史）。
	traceCount := 0
	if err := env.db.QueryRow(`SELECT count(DISTINCT trace_id) FROM events WHERE namespace_id = ?`,
		env.nsID).Scan(&traceCount); err != nil {
		t.Fatalf("count traces: %v", err)
	}
	if traceCount != 2 {
		t.Fatalf("两轮派发应留下 2 个 trace，实际 %d", traceCount)
	}
}
