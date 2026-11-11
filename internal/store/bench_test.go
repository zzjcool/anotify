// store 包**写路径**的基准 + 防静默失效护栏（issue #146）—— v4 迁移。
//
// 为什么是外部测试包 store_test（而不是同目录其他文件那样的 package store）：
// BenchmarkDispatchChain 要 import internal/route，而 internal/route 反过来
// import internal/store——写成内部测试包会 import cycle。代价是拿不到
// store_bench_test.go 里的内部 helper（benchStore 等），所以这里自带一套
// 只依赖 store 导出 API 的夹具。
//
// v4 的写路径与 v3 不同，基准也换成了 v4 真实的三个层次：
//
//	BenchmarkCreateTasks   派发的主成本：1 tx 内插 n 行（建 pending 任务）
//	BenchmarkAssignGrant   一次派发的两个写：assign（记候选）+ grant（认证身份换租约）
//	BenchmarkCompleteTask  一次结果回报的写：一次事务里落结果事实 + 释放真实槽位
//
// v3 的 `Transition`（9 态状态机的单次迁移）整体删除，所以那条基准退役；
// 它度量的「一次热路径单行写」由上面两个 v4 入口（grant / complete）继续覆盖。
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 编译期引用：这些符号就是基准被测的对象。若将来改名/删签名，
// 这里**编译不过**；否则基准会静默跑空（Go 的 benchmark 对「什么都没测」毫无抵抗，
// 仍然会打印出一组看起来正常的 ns/op）。
//
// dispatch 全链（要 import internal/route + internal/source）在 bench_dispatch_test.go，
// v4 已去掉 v3 那个 `dispatchbench` build tag（#338）：它和本文件一样在 `go test ./internal/store` 里编译。
var (
	_ = (*store.DB).CreateTasks
	_ = (*store.DB).AssignTask
	_ = (*store.DB).GrantLeaseAuthorized
	_ = (*store.DB).CompleteTaskClassifiedAuthorized
)

// benchCapName 是基准用的能力名。取自定义名字前缀 bench.，避免撞上内置能力名
// （内置起点的名字是保留的，CreateCapability 会直接拒绝）。
const benchCapName = "bench.review"

// benchEnv 是三个基准共用的最小可用环境：一个用户（+ 它的默认命名空间）
// + 一个自定义能力定义 + 一台在线节点（1 个真实槽位）。
type benchEnv struct {
	db      *store.DB
	nsID    string
	capID   string
	capName string
	digest  string
	nodeID  string
	auth    store.NodeAuthorization
}

// newBenchEnv 打开内存库并铺好 FK 父行：用户 + 默认命名空间 → 能力 → 在线节点。
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

	// RegisterUser 一步建 user + 它的 default 命名空间 + 内置能力（同一事务）。
	user := &store.User{ID: store.NewUserID(), Username: "u-" + store.NewUserID(), CreatedAt: store.Now()}
	ns, err := db.RegisterUser(ctx, user)
	if err != nil {
		tb.Fatalf("register user: %v", err)
	}

	// 能力正文（spec）是派发的输入。这里**必须是真脚本**：v4 只有脚本一种实现形式，
	// 用占位正文会让 grant 后的执行路径失去意义。
	capDef := &store.CapabilityVersion{
		NamespaceID: ns.ID, Name: benchCapName, Version: 1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{"type":"object"}`),
			Next:   []string{},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		tb.Fatalf("create capability: %v", err)
	}

	// 节点：在线 + 1 个真实槽位（grant 必须有空闲 slot）。
	node := &store.Node{Name: "n-" + store.NewUserID(), Status: store.NodeStatusOnline,
		DeclaredNamespaces: []string{"*"}}
	if _, err := db.CreateNode(ctx, node); err != nil {
		tb.Fatalf("create node: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, node.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		tb.Fatalf("upsert instances: %v", err)
	}

	return &benchEnv{
		db: db, nsID: ns.ID, capID: capDef.ID, capName: capDef.Name, digest: capDef.Digest,
		nodeID: node.ID,
		auth:   store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash},
	}
}

// newTask 造**未落库**的一行 pending 任务（外键父行已由 env 提供）。
//
// 字段取值对齐真实派发填的那几个：能力身份 + 钉住的 digest + 已过 schema 的 params。
// ID/CreatedAt 故意留空，由 CreateTasks 补（那是它的真实职责，也该被度量）。
func (e *benchEnv) newTask(traceID string) *store.Task {
	return &store.Task{
		NamespaceID:  e.nsID,
		TraceID:      traceID,
		CapabilityID: e.capID,
		Capability:   e.capName,
		Version:      1,
		Digest:       e.digest,
		Params:       json.RawMessage(`{"number":42}`),
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

// grantOne 走完整的 assign → grant（真实身份 + 真实 slot），返回租约事实。
func (e *benchEnv) grantOne(tb testing.TB, ctx context.Context, taskID string) (leaseID string, attempt int) {
	tb.Helper()
	assignmentID := "asg-" + taskID
	if err := e.db.AssignTask(ctx, taskID, e.nodeID, assignmentID, 1); err != nil {
		tb.Fatalf("assign %s: %v", taskID, err)
	}
	leaseID, _, attempt, _, err := e.db.GrantLeaseAuthorized(ctx, e.auth, taskID, assignmentID, 1, 0, store.Now())
	if err != nil {
		tb.Fatalf("grant %s: %v", taskID, err)
	}
	return leaseID, attempt
}

// BenchmarkCreateTasks 度量「一个事务内批量建 n 条任务」。
//
// 规模取 1 / 5 / 20 且是常量（不随 b.N 变）：
//   - 1  = 单任务基线（不扇出的链）
//   - 5  = 一条 trace 的典型扇出（issue #146 里估算 traces/s 就用 5 任务 trace）
//   - 20 = 大扇出上界
func BenchmarkCreateTasks(b *testing.B) {
	env := newBenchEnv(b)
	ctx := context.Background()

	for _, n := range []int{1, 5, 20} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			// 复用切片壳（省掉每轮一次切片分配），里面的 Task 每轮重建：
			// CreateTasks 会回填 ID，复用同一个 Task 会在第二轮撞主键。
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
		})
	}
}

// BenchmarkAssignGrant 度量「一次派发的两个写」：assign（记候选，1 行 UPDATE）
// + grant（认证身份 + 真实 slot → 租约，1 tx）。
//
// 这是 v4 的热路径：一条任务每换一次 attempt 就要走一遍（v3 的 Transition 已删除）。
// 终态那一次写由 BenchmarkCompleteTask 覆盖。
func BenchmarkAssignGrant(b *testing.B) {
	env := newBenchEnv(b)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 装数据在计时外：建 pending 任务属于 CreateTasks 的成本，不重复计入。
		b.StopTimer()
		t := env.createOne(b, ctx, fmt.Sprintf("trc_grant_%d", i))
		b.StartTimer()

		assignmentID := "asg-" + t.ID
		if err := env.db.AssignTask(ctx, t.ID, env.nodeID, assignmentID, 1); err != nil {
			b.Fatalf("assign: %v", err)
		}
		if _, _, _, _, err := env.db.GrantLeaseAuthorized(ctx, env.auth, t.ID, assignmentID, 1, 0, store.Now()); err != nil {
			b.Fatalf("grant: %v", err)
		}
	}
}

// BenchmarkCompleteTask 度量「节点回报一次结果」的写成本。
//
// v4 的完整事实写：result_json（含纳秒）+ 投影列 + 释放真实槽位，一个事务。
// grant（装数据）在计时外——那是 BenchmarkAssignGrant 的成本。
func BenchmarkCompleteTask(b *testing.B) {
	env := newBenchEnv(b)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		t := env.createOne(b, ctx, fmt.Sprintf("trc_done_%d", i))
		leaseID, attempt := env.grantOne(b, ctx, t.ID)
		start := time.Now().UTC()
		finished := start.Add(time.Second)
		result := store.TaskResult{
			Phase:      store.TaskSucceeded,
			Summary:    "ok",
			Artifacts:  []protocol.TaskArtifact{},
			StartedAt:  start,
			FinishedAt: finished,
		}
		b.StartTimer()

		// 计时区内**恰好一次**：节点回报结果（含释槽）。
		if _, err := env.db.CompleteTaskClassifiedAuthorized(ctx, env.auth, t.ID, attempt, leaseID,
			store.Now(), result); err != nil {
			b.Fatalf("complete: %v", err)
		}
	}
}

// TestBenchmarksTargetRealPaths 是三个基准的护栏：对真库把基准依赖的路径各跑一遍，
// 断言**可观测效果**（库里真的多了行、相位真的变了、槽位真的绑上又放开）。
//
// 为什么需要它：基准只报 ns/op，跑空了也照样打印数字。改名、参数变空、桶位被占满，
// 这些失效方式都不会让 go test 变红，只会让基线悄悄失真。本测试把「基准确实
// 测到了真路径」变成一条会失败的断言。
func TestBenchmarksTargetRealPaths(t *testing.T) {
	env := newBenchEnv(t)
	ctx := context.Background()

	// ① CreateTasks 真的落行
	task := env.createOne(t, ctx, "trc_guard_create")
	var rows int
	if err := env.db.QueryRow(`SELECT count(*) FROM tasks WHERE trace_id = ?`, "trc_guard_create").Scan(&rows); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if rows != 1 {
		t.Fatalf("CreateTasks 落库行数 = %d, want 1", rows)
	}

	// ② assign + grant 真的换了租约、真的占住了槽位
	leaseID, attempt := env.grantOne(t, ctx, task.ID)
	if leaseID == "" || attempt != 1 {
		t.Fatalf("grant 结果不完整: lease=%q attempt=%d", leaseID, attempt)
	}
	running, err := env.db.GetTask(ctx, env.nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if running.Phase != store.TaskRunning || running.LeaseID != leaseID || running.LeaseUntil == nil {
		t.Fatalf("grant 后任务相位/租约不对: %+v", running)
	}
	insts, err := env.db.ListInstances(ctx, env.nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(insts) != 1 || insts[0].BusyTask == nil || *insts[0].BusyTask != task.ID {
		t.Fatalf("grant 后槽位应绑到任务: %+v", insts)
	}

	// ③ complete 真的写终态 + 释放槽位
	start := time.Now().UTC()
	result := store.TaskResult{
		Phase:      store.TaskSucceeded,
		Summary:    "ok",
		Artifacts:  []protocol.TaskArtifact{},
		StartedAt:  start,
		FinishedAt: start.Add(time.Second),
	}
	disp, err := env.db.CompleteTaskClassifiedAuthorized(ctx, env.auth, task.ID, attempt, leaseID, store.Now(), result)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if disp != store.TaskResultRecorded {
		t.Fatalf("回报分类 = %q, want recorded", disp)
	}
	done, err := env.db.GetTask(ctx, env.nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if done.Phase != store.TaskSucceeded || done.FinishedAt == nil {
		t.Fatalf("complete 后任务相位不对: %+v", done)
	}
	insts, err = env.db.ListInstances(ctx, env.nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if insts[0].BusyTask != nil {
		t.Fatalf("complete 后槽位应被释放: %+v", insts[0])
	}
}
