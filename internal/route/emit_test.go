package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 P1.1「中枢侧 emit」的用例。
//
// 为什么这里用**真 SQLite** 而不是 fakeStore：
//
//	emit 的核心不变量全是事务性的——「emission 与子任务行同生共死」
//	（emit-protocol §7）、「幂等靠 UNIQUE(task_id, seq)」、「缺口靠已落库的
//	seq 集合」。用内存 mock 验这几条，验的是我自己写的 mock 的语义，
//	而真实语义在 store 的 SQL 里。派发判定（哪些 link 被剪）仍然复用 plan，
//	那部分由 dispatch_test.go 的 fake 覆盖，两者不冲突。

// ---------- 夹具 ----------

// emitFixture 真库上的 emit 环境。
type emitFixture struct {
	db *store.DB
	ns string
	d  *Dispatcher
}

func newEmitFixture(t *testing.T) *emitFixture {
	t.Helper()
	ctx := context.Background()
	// store.Open 自己加 file: 前缀与 PRAGMA，这里只给纯路径（与 ws 夹具一致）。
	db, err := store.Open(t.TempDir() + "/emit.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 用户 + default 命名空间（注册同事务建命名空间并播种内置能力）。
	u := &store.User{ID: store.NewUserID(), Username: "u-" + store.NewUserID(), CreatedAt: store.Now()}
	nsRow, err := db.RegisterUser(ctx, u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	return &emitFixture{db: db, ns: nsRow.ID, d: New(db)}
}

// addCap 建一个能力定义（outputs + params_schema）。
func (f *emitFixture) addCap(t *testing.T, name string, version int, outputs []string, schema string) *store.CapabilityVersion {
	t.Helper()
	if schema == "" {
		schema = `{"type":"object"}`
	}
	c := &store.CapabilityVersion{
		NamespaceID:  f.ns,
		Name:         name,
		Version:      version,
		ParamsSchema: json.RawMessage(schema),
		Outputs:      outputs,
		Source:       store.CapSourceCustom,
	}
	if err := f.db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability %s@%d: %v", name, version, err)
	}
	return c
}

// addEdge 加一条 link（from --port--> to，可带字面量覆盖）。
func (f *emitFixture) addEdge(t *testing.T, from *store.CapabilityVersion, port string, to *store.CapabilityVersion, literals string) {
	t.Helper()
	e := &store.Edge{
		NamespaceID:    f.ns,
		FromCapability: from.ID,
		FromPort:       port,
		ToCapability:   to.ID,
	}
	if literals != "" {
		e.Literals = json.RawMessage(literals)
	}
	if err := f.db.CreateEdge(context.Background(), e); err != nil {
		t.Fatalf("create edge %s--%s-->%s: %v", from.Name, port, to.Name, err)
	}
}

// addNode 建一台节点，让它声明这些能力，并申报空闲槽位。
func (f *emitFixture) addNode(t *testing.T, slots int, caps ...*store.CapabilityVersion) *store.Node {
	t.Helper()
	ctx := context.Background()
	n := &store.Node{NamespaceID: f.ns, Name: "nd-" + store.NewUserID(), Tags: []string{"pool"}}
	if _, err := f.db.CreateNode(ctx, n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	applied := make([]store.AppliedRef, 0, len(caps))
	for _, c := range caps {
		applied = append(applied, store.AppliedRef{Name: c.Name, Version: c.Version})
	}
	if _, err := f.db.ReconcileApplied(ctx, f.ns, n.ID, applied); err != nil {
		t.Fatalf("reconcile applied: %v", err)
	}
	specs := make([]store.InstanceSpec, 0, slots)
	for i := 0; i < slots; i++ {
		specs = append(specs, store.InstanceSpec{Slot: i, Concurrency: 1})
	}
	if _, err := f.db.UpsertInstances(ctx, f.ns, n.ID, specs); err != nil {
		t.Fatalf("upsert instances: %v", err)
	}
	return n
}

// newTask 建一条待执行的上游任务行（path 含它自己，与派发建行时一致）。
func (f *emitFixture) newTask(t *testing.T, capDef *store.CapabilityVersion) *store.Task {
	t.Helper()
	ctx := context.Background()
	task := &store.Task{
		NamespaceID:  f.ns,
		TraceID:      store.NewTaskID(),
		Path:         []string{capDef.ID},
		CapabilityID: capDef.ID,
		Capability:   capDef.Name,
		Version:      capDef.Version,
		Params:       json.RawMessage(`{}`),
		CapHash:      capDef.ContentHash,
		GraphID:      "grf_test",
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// emitted 用一次真实 emit 跑一遍（所有用例都从它开始，避免抄一遍输入）。
func (f *emitFixture) emit(t *testing.T, taskID string, seq int64, port, data string) (*EmitOutput, error) {
	t.Helper()
	in := EmitInput{NamespaceID: f.ns, TaskID: taskID, Seq: seq, Port: port}
	if data != "" {
		in.Data = json.RawMessage(data)
	}
	return f.d.Emit(context.Background(), in)
}

// mustEmit 要求这次 emit 成功。
func (f *emitFixture) mustEmit(t *testing.T, taskID string, seq int64, port, data string) *EmitOutput {
	t.Helper()
	out, err := f.emit(t, taskID, seq, port, data)
	if err != nil {
		t.Fatalf("emit %s#%d port %q: %v", taskID, seq, port, err)
	}
	return out
}

// emissions 读该任务已落库的 emission 列表（缺口/幂等的唯一判据）。
func (f *emitFixture) emissions(t *testing.T, taskID string) []*store.Emission {
	t.Helper()
	list, err := f.db.ListEmissions(context.Background(), taskID)
	if err != nil {
		t.Fatalf("list emissions: %v", err)
	}
	return list
}

// childTasks 读该任务的直接子任务（= 这次 emit 派发出来的行）。
func (f *emitFixture) childTasks(t *testing.T, parentID string) []*store.Task {
	t.Helper()
	list, err := f.db.ListTasks(context.Background(), f.ns, "", "", "", 500)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	out := make([]*store.Task, 0, len(list))
	for _, tk := range list {
		if tk.ParentTaskID != nil && *tk.ParentTaskID == parentID {
			out = append(out, tk)
		}
	}
	return out
}

// ---------- ① 幂等 ----------

// TestEmitIdempotent 重复 seq 不重复派发，计数取首次结果（emit-protocol §4）。
func TestEmitIdempotent(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`)
	f.addEdge(t, root, "fix", fixer, "")
	f.addNode(t, 2, fixer)
	task := f.newTask(t, root)

	first := f.mustEmit(t, task.ID, 1, "fix", `{"issue":"#42"}`)
	if first.Routed != 1 || first.Pruned != 0 || first.Duplicate {
		t.Fatalf("first emit = %+v, want routed=1 pruned=0 duplicate=false", first)
	}
	if n := len(f.childTasks(t, task.ID)); n != 1 {
		t.Fatalf("首次 emit 建了 %d 条子任务，want 1", n)
	}

	// 同一份内容再发一次：必须回首次结果，且**一条任务都不再建**。
	again := f.mustEmit(t, task.ID, 1, "fix", `{"issue":"#42"}`)
	if !again.Duplicate {
		t.Fatalf("重复 seq 没被识别成重发：%+v", again)
	}
	if again.Routed != first.Routed || again.Pruned != first.Pruned {
		t.Fatalf("重复 seq 的计数 = (%d,%d), want 首次结果 (%d,%d)",
			again.Routed, again.Pruned, first.Routed, first.Pruned)
	}
	if n := len(f.childTasks(t, task.ID)); n != 1 {
		t.Fatalf("重复 seq 又派发了一次：子任务 %d 条，want 1", n)
	}
	if n := len(f.emissions(t, task.ID)); n != 1 {
		t.Fatalf("emissions 行数 = %d, want 1（幂等键是 (task_id,seq)）", n)
	}

	// 语义相同的 JSON（键序/空白不同）不算「内容冲突」。
	same := f.mustEmit(t, task.ID, 1, "fix", `{ "issue" : "#42" }`)
	if !same.Duplicate || same.Routed != first.Routed {
		t.Fatalf("语义相同的重发被判成冲突：%+v", same)
	}
}

// ---------- ② 缺口 ----------

// TestEmitSeqGapRejected 缺口被拒，且**不落库**（emit-protocol §4 缺口策略）。
func TestEmitSeqGapRejected(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`)
	f.addEdge(t, root, "fix", fixer, "")
	f.addNode(t, 4, fixer)
	task := f.newTask(t, root)

	// 直接跳到 seq=2：1 没落库 → 拒收，且库里一条 emission 都不该有。
	_, err := f.emit(t, task.ID, 2, "fix", `{"issue":"#42"}`)
	if !errors.Is(err, ErrEmitSeqGap) {
		t.Fatalf("errors.Is(err, ErrEmitSeqGap) = false, err = %v", err)
	}
	if got := f.emissions(t, task.ID); len(got) != 0 {
		t.Fatalf("缺口被拒后仍落了 %d 条 emission：%+v", len(got), got)
	}
	if n := len(f.childTasks(t, task.ID)); n != 0 {
		t.Fatalf("缺口被拒后仍建了 %d 条子任务", n)
	}

	// seq=1 落库后，seq=2 才能过（连续性从 1 开始）。
	f.mustEmit(t, task.ID, 1, "fix", `{"issue":"#1"}`)
	f.mustEmit(t, task.ID, 2, "fix", `{"issue":"#2"}`)

	// 再跳一格（3 没来就要 4）→ 仍然拒。
	if _, err := f.emit(t, task.ID, 4, "fix", `{"issue":"#4"}`); !errors.Is(err, ErrEmitSeqGap) {
		t.Fatalf("跳号 seq=4 没被拒：%v", err)
	}
}

// ---------- ③ 同 seq 不同内容 ----------

// TestEmitSeqReusedRejected 同一 seq 换了 port 或 data → 拒（节点 bug，不能静默覆盖）。
func TestEmitSeqReusedRejected(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix", "push"}, `{"type":"object"}`)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`)
	pusher := f.addCap(t, "pusher", 1, nil, `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
	f.addEdge(t, root, "fix", fixer, "")
	f.addEdge(t, root, "push", pusher, "")
	f.addNode(t, 4, fixer, pusher)
	task := f.newTask(t, root)

	f.mustEmit(t, task.ID, 1, "fix", `{"issue":"#42"}`)

	t.Run("data 变了", func(t *testing.T) {
		_, err := f.emit(t, task.ID, 1, "fix", `{"issue":"#43"}`)
		if !errors.Is(err, ErrEmitSeqReused) {
			t.Fatalf("errors.Is(err, ErrEmitSeqReused) = false, err = %v", err)
		}
	})
	t.Run("port 变了", func(t *testing.T) {
		_, err := f.emit(t, task.ID, 1, "push", `{"issue":"#42"}`)
		if !errors.Is(err, ErrEmitSeqReused) {
			t.Fatalf("errors.Is(err, ErrEmitSeqReused) = false, err = %v", err)
		}
	})

	// 被拒的重发不能改写首次那份，也不能多派发。
	got := f.emissions(t, task.ID)
	if len(got) != 1 || got[0].Port != "fix" || got[0].Routed != 1 {
		t.Fatalf("首次 emission 被改写：%+v", got)
	}
	if n := len(f.childTasks(t, task.ID)); n != 1 {
		t.Fatalf("冲突重发导致子任务数 = %d, want 1", n)
	}
}

// ---------- ④ 终态任务 ----------

// TestEmitRejectedOnClosedTask 已终结的任务不能收 emit（emit-protocol §8）：
// 允许晚到的 emit 改动已终结的任务，等于允许它凭空产生下游。
func TestEmitRejectedOnClosedTask(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object"}`)
	f.addEdge(t, root, "fix", fixer, "")
	f.addNode(t, 2, fixer)

	ctx := context.Background()
	task := f.newTask(t, root)
	// 走真实迁移把行推到终态（pending → assigned → canceled）。
	if err := f.db.Transition(ctx, f.ns, task.ID, store.TaskPending, store.TaskAssigned); err != nil {
		t.Fatalf("→assigned: %v", err)
	}
	if err := f.db.Transition(ctx, f.ns, task.ID, store.TaskAssigned, store.TaskCanceled); err != nil {
		t.Fatalf("→canceled: %v", err)
	}

	_, err := f.emit(t, task.ID, 1, "fix", `{"issue":"#1"}`)
	if !errors.Is(err, ErrEmitTaskClosed) {
		t.Fatalf("终态任务收下了 emit：err = %v", err)
	}
	if got := f.emissions(t, task.ID); len(got) != 0 {
		t.Fatalf("终态任务被拒后仍落了 emission：%+v", got)
	}
	if n := len(f.childTasks(t, task.ID)); n != 0 {
		t.Fatalf("终态任务被拒后仍建了 %d 条子任务", n)
	}
}

// TestEmitOnRunningTasks 运行态的任务可以 emit（assigned/acked/running 都算）。
func TestEmitOnRunningTasks(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object"}`)
	f.addEdge(t, root, "fix", fixer, "")
	f.addNode(t, 2, fixer)

	ctx := context.Background()
	task := f.newTask(t, root)
	// pending → assigned → acked → running，每一步都发一次 emit（seq 依次递增）。
	steps := []func() error{
		func() error { return nil },
		func() error { return f.db.Transition(ctx, f.ns, task.ID, store.TaskPending, store.TaskAssigned) },
		func() error { return f.db.Transition(ctx, f.ns, task.ID, store.TaskAssigned, store.TaskAcked) },
		func() error { return f.db.Transition(ctx, f.ns, task.ID, store.TaskAcked, store.TaskRunning) },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("第 %d 步进状态: %v", i, err)
		}
		// 第 0 步不带 data（帧里缺省 = {}，emit-protocol §3.1），其余带空对象。
		data := "{}"
		if i == 0 {
			data = ""
		}
		if _, err := f.emit(t, task.ID, int64(i+1), "fix", data); err != nil {
			t.Fatalf("第 %d 步（状态推进后）emit 被拒: %v", i, err)
		}
	}
}

// TestEmitStatusWhitelist 白名单是**闭**的：只有四个运行态收 emit（新增状态默认拒）。
func TestEmitStatusWhitelist(t *testing.T) {
	running := []string{store.TaskPending, store.TaskAssigned, store.TaskAcked, store.TaskRunning}
	for _, s := range running {
		if !emitAcceptableStatus(s) {
			t.Errorf("%q 应当可以 emit", s)
		}
	}
	closed := []string{store.TaskSucceeded, store.TaskFailed, store.TaskCanceled, store.TaskAbandoned, store.TaskLeaseExpired, ""}
	for _, s := range closed {
		if emitAcceptableStatus(s) {
			t.Errorf("%q 不该可以 emit", s)
		}
	}
}

// TestEmitUnknownTask 不存在的任务 → ErrEmitTaskNotFound（不是 500，也不是静默成功）。
func TestEmitUnknownTask(t *testing.T) {
	f := newEmitFixture(t)
	_, err := f.emit(t, "tsk_does_not_exist", 1, "fix", `{}`)
	if !errors.Is(err, ErrEmitTaskNotFound) {
		t.Fatalf("errors.Is(err, ErrEmitTaskNotFound) = false, err = %v", err)
	}
}

// ---------- ⑤ 0 条 link ----------

// TestEmitNoLinksIsNotAnError 没有下游的 port：routed=0、不报错，但 emission 照样落库。
//
// emission 必须落库：seq 的连续性靠它（漏一条会让后续 seq 全被判成缺口）。
func TestEmitNoLinksIsNotAnError(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix", "nothing"}, `{"type":"object"}`)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object"}`)
	f.addEdge(t, root, "fix", fixer, "")
	f.addNode(t, 2, fixer)
	task := f.newTask(t, root)

	out := f.mustEmit(t, task.ID, 1, "nothing", `{"whatever":1}`)
	if out.Routed != 0 || out.Pruned != 0 {
		t.Fatalf("0 link 的 port = routed %d / pruned %d, want 0/0", out.Routed, out.Pruned)
	}
	if n := len(f.childTasks(t, task.ID)); n != 0 {
		t.Fatalf("0 link 的 port 建了 %d 条子任务", n)
	}
	got := f.emissions(t, task.ID)
	if len(got) != 1 || got[0].Seq != 1 || got[0].Routed != 0 || got[0].Pruned != 0 {
		t.Fatalf("emission = %+v, want 一条 seq=1 routed=0 pruned=0", got)
	}
	// 落库之后 seq=2 才连续——这正是「0 link 也必须记录」的理由。
	f.mustEmit(t, task.ID, 2, "fix", `{}`)
}

// ---------- ⑥ 扇出部分成功 ----------

// TestEmitFanoutPartialPrune 3 条 link 里 1 条没节点 → 其余照常派发，pruned=1。
func TestEmitFanoutPartialPrune(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	fix1 := f.addCap(t, "fix1", 1, nil, `{"type":"object"}`)
	fix2 := f.addCap(t, "fix2", 1, nil, `{"type":"object"}`)
	orphan := f.addCap(t, "orphan", 1, nil, `{"type":"object"}`)
	f.addEdge(t, root, "fix", fix1, "")
	f.addEdge(t, root, "fix", fix2, "")
	f.addEdge(t, root, "fix", orphan, "") // 没有任何节点声明它
	f.addNode(t, 2, fix1, fix2)
	task := f.newTask(t, root)

	out := f.mustEmit(t, task.ID, 1, "fix", `{}`)
	if out.Routed != 2 || out.Pruned != 1 {
		t.Fatalf("routed/pruned = %d/%d, want 2/1", out.Routed, out.Pruned)
	}
	if len(out.Reasons) != 1 || out.Reasons[0].Reason != ReasonNoNode {
		t.Fatalf("剪枝原因 = %+v, want 单条 no_node", out.Reasons)
	}

	kids := f.childTasks(t, task.ID)
	if len(kids) != 2 {
		t.Fatalf("子任务 %d 条, want 2（部分成功）", len(kids))
	}
	names := map[string]bool{}
	for _, k := range kids {
		names[k.Capability] = true
	}
	if !names["fix1"] || !names["fix2"] || names["orphan"] {
		t.Fatalf("派发的子任务 = %v, want fix1+fix2（orphan 被剪）", names)
	}
}

// ---------- ⑦ 同事务 ----------

// TestEmitEmissionAndTasksSameTransaction 建子任务失败 → emission 也不存在。
//
// 人为制造失败：让下游 params 超过 store.MaxTaskParamsBytes（建行闸门在
// createTasksTx 里），于是同一个事务整体回滚。这条用例守的是 emit-protocol §7
// 的核心：「emission 在、下游不在」这种永久空洞在库里不可能出现。
func TestEmitEmissionAndTasksSameTransaction(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object","properties":{"blob":{"type":"string"}},"required":["blob"]}`)
	f.addEdge(t, root, "fix", fixer, "")
	f.addNode(t, 2, fixer)
	task := f.newTask(t, root)

	// > 120 KiB 的字符串：建子任务行时被 CheckTaskParamsSize 拒。
	blob := make([]byte, store.MaxTaskParamsBytes+1024)
	for i := range blob {
		blob[i] = 'x'
	}
	data, err := json.Marshal(map[string]any{"blob": string(blob)})
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}

	if _, err := f.emit(t, task.ID, 1, "fix", string(data)); err == nil {
		t.Fatal("超限 params 竟然派发成功了")
	} else if !errors.Is(err, store.ErrParamsTooLarge) {
		// 确认失败真的是「建子任务行时被体积闸门拦住」——而不是别的什么原因，
		// 否则这条事务用例会因为错误的原因而绿（假绿）。
		t.Fatalf("失败原因不是 params 超限，这条事务用例就失去意义了：%v", err)
	}
	if got := f.emissions(t, task.ID); len(got) != 0 {
		t.Fatalf("子任务建行失败后仍留下 %d 条 emission（事务没回滚）：%+v", len(got), got)
	}
	if n := len(f.childTasks(t, task.ID)); n != 0 {
		t.Fatalf("失败后仍建了 %d 条子任务", n)
	}

	// 事务回滚之后，同一 seq 仍然是「没被用过」：换成合法数据能正常派发。
	if out := f.mustEmit(t, task.ID, 1, "fix", `{"blob":"ok"}`); out.Routed != 1 {
		t.Fatalf("回滚后重试 routed = %d, want 1", out.Routed)
	}
}

// ---------- 其他入口约束 ----------

// TestEmitInputValidation 非法输入在碰库之前就被拒（不写任何东西）。
func TestEmitInputValidation(t *testing.T) {
	f := newEmitFixture(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   EmitInput
	}{
		{"空 namespace", EmitInput{TaskID: "tsk_x", Seq: 1, Port: "fix"}},
		{"空 task_id", EmitInput{NamespaceID: f.ns, Seq: 1, Port: "fix"}},
		{"seq=0", EmitInput{NamespaceID: f.ns, TaskID: "tsk_x", Port: "fix"}},
		{"空 port", EmitInput{NamespaceID: f.ns, TaskID: "tsk_x", Seq: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := f.d.Emit(ctx, c.in); err == nil {
				t.Fatal("非法输入被接受了")
			}
		})
	}
}

// TestEmitDataMustBeObject data 不是对象 → 拒（节点已拦一道，中枢是第二道）。
func TestEmitDataMustBeObject(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	task := f.newTask(t, root)

	for _, data := range []string{`[1,2]`, `"text"`, `42`, `{bad`} {
		if _, err := f.emit(t, task.ID, 1, "fix", data); !errors.Is(err, ErrEmitBadData) {
			t.Fatalf("data=%s: errors.Is(err, ErrEmitBadData) = false, err = %v", data, err)
		}
	}
	if got := f.emissions(t, task.ID); len(got) != 0 {
		t.Fatalf("坏 data 也落了库：%+v", got)
	}
}

// TestEmitChildTasksInheritParent 子任务继承父任务的 trace / 命名空间 / path（+本能力）。
func TestEmitChildTasksInheritParent(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`)
	f.addEdge(t, root, "fix", fixer, "")
	f.addNode(t, 2, fixer)
	task := f.newTask(t, root)

	f.mustEmit(t, task.ID, 1, "fix", `{"issue":"#7"}`)
	kids := f.childTasks(t, task.ID)
	if len(kids) != 1 {
		t.Fatalf("子任务 %d 条, want 1", len(kids))
	}
	kid := kids[0]
	if kid.TraceID != task.TraceID {
		t.Errorf("子任务 trace = %q, want %q", kid.TraceID, task.TraceID)
	}
	if kid.NamespaceID != f.ns {
		t.Errorf("子任务 namespace = %q, want %q", kid.NamespaceID, f.ns)
	}
	if want := []string{root.ID, fixer.ID}; !reflect.DeepEqual(kid.Path, want) {
		t.Errorf("子任务 path = %v, want %v（环保护靠它）", kid.Path, want)
	}
	if string(kid.Params) != `{"issue":"#7"}` {
		t.Errorf("子任务 params = %s, want emit.data 原样", kid.Params)
	}
	if kid.CapabilityID != fixer.ID || kid.Version != fixer.Version {
		t.Errorf("子任务能力 = %s@%d, want %s@%d", kid.CapabilityID, kid.Version, fixer.ID, fixer.Version)
	}
}

// TestEmitLiteralsWin literals 覆盖 emit.data（字面量优先，设计 §3.3）。
func TestEmitLiteralsWin(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	notifier := f.addCap(t, "notifier", 1, nil,
		`{"type":"object","properties":{"issue":{"type":"string"},"to":{"type":"string"}},"required":["issue","to"]}`)
	// link 上钉死收件人：data 里不必带 to，带了也会被覆盖。
	f.addEdge(t, root, "fix", notifier, `{"to":"u_pinned"}`)
	f.addNode(t, 2, notifier)
	task := f.newTask(t, root)

	if out := f.mustEmit(t, task.ID, 1, "fix", `{"issue":"#9","to":"u_evil"}`); out.Routed != 1 {
		t.Fatalf("routed = %d, want 1", out.Routed)
	}
	kids := f.childTasks(t, task.ID)
	if len(kids) != 1 {
		t.Fatalf("子任务 %d 条, want 1", len(kids))
	}
	var params map[string]any
	if err := json.Unmarshal(kids[0].Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params["to"] != "u_pinned" {
		t.Fatalf("下游 params.to = %v, want u_pinned（字面量优先）", params["to"])
	}
}

// TestEmitParamPrunePerLink 参数不过 schema 只剪那一条 link（扇出部分成功）。
func TestEmitParamPrunePerLink(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	strict := f.addCap(t, "strict", 1, nil,
		`{"type":"object","properties":{"lines":{"type":"integer"}},"required":["lines"]}`)
	loose := f.addCap(t, "loose", 1, nil, `{"type":"object"}`)
	f.addEdge(t, root, "fix", strict, "")
	f.addEdge(t, root, "fix", loose, "")
	f.addNode(t, 4, strict, loose)
	task := f.newTask(t, root)

	// data 里没有 lines：strict 被剪（param_schema），loose 照常派发。
	out := f.mustEmit(t, task.ID, 1, "fix", `{"issue":"#1"}`)
	if out.Routed != 1 || out.Pruned != 1 {
		t.Fatalf("routed/pruned = %d/%d, want 1/1", out.Routed, out.Pruned)
	}
	if len(out.Reasons) != 1 || out.Reasons[0].Reason != ReasonParamSchema {
		t.Fatalf("剪枝原因 = %+v, want param_schema", out.Reasons)
	}
	if kids := f.childTasks(t, task.ID); len(kids) != 1 || kids[0].Capability != "loose" {
		t.Fatalf("派发结果 = %v, want 仅 loose", kids)
	}
}

// ---------- fakeStore 的 emit 面 ----------
//
// route.Store 在 P1 扩了四个 emit 方法（见 dispatch.go 的接口注释）。
// fakeStore 必须满足接口才能继续编译；实现尽量与真库同口径（ON CONFLICT DO NOTHING、
// 条数/最大 seq），这样 dispatch_test.go 里的既有用例不会因为语义漂移而假绿。

func (f *fakeStore) GetTask(ctx context.Context, namespaceID, id string) (*store.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.createdTasks {
		if t.ID == id && t.NamespaceID == namespaceID {
			return t, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) InsertEmissionAndCreateTasks(ctx context.Context, e *store.Emission, ts []*store.Task) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.emissions == nil {
		f.emissions = map[string]*store.Emission{}
	}
	key := fmt.Sprintf("%s#%d", e.TaskID, e.Seq)
	if _, ok := f.emissions[key]; ok {
		return false, nil // 幂等命中：不建行
	}
	cp := *e
	f.emissions[key] = &cp
	f.createdTasks = append(f.createdTasks, ts...)
	return true, nil
}

func (f *fakeStore) GetEmission(ctx context.Context, taskID string, seq int64) (*store.Emission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.emissions[fmt.Sprintf("%s#%d", taskID, seq)]; ok {
		return e, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) EmissionSeqBefore(ctx context.Context, taskID string, before int64) (int, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	var maxSeq int64
	for _, e := range f.emissions {
		if e.TaskID != taskID || e.Seq >= before {
			continue // 与真实实现同口径：只看 seq < before 的那一段
		}
		count++
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	return count, maxSeq, nil
}

// TestEmitConcurrentSameSeq 同一个 seq 并发到达：唯一键折成一个赢家，只派发一次。
//
// 这是「不靠顺序假设」的一半（另一半是乱序 → 缺口）。真实形状：节点在两次重连之间
// 重发了同一个 seq，而两帧分别落在两条连接上、各自被处理。
func TestEmitConcurrentSameSeq(t *testing.T) {
	f := newEmitFixture(t)
	root := f.addCap(t, "review", 1, []string{"fix"}, `{"type":"object"}`)
	fixer := f.addCap(t, "fixer", 1, nil, `{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`)
	f.addEdge(t, root, "fix", fixer, "")
	f.addNode(t, 4, fixer)
	task := f.newTask(t, root)

	const workers = 6
	type result struct {
		out *EmitOutput
		err error
	}
	results := make([]result, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out, err := f.d.Emit(context.Background(), EmitInput{
				NamespaceID: f.ns, TaskID: task.ID, Seq: 1, Port: "fix",
				Data: json.RawMessage(`{"issue":"#42"}`),
			})
			results[i] = result{out: out, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	firsts := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("worker %d 失败: %v", i, r.err)
		}
		if r.out.Routed != 1 {
			t.Fatalf("worker %d 的 routed = %d, want 1（首次结果对所有人一致）", i, r.out.Routed)
		}
		if !r.out.Duplicate {
			firsts++
		}
	}
	if firsts != 1 {
		t.Fatalf("非重发的调用 = %d 次, want 1（唯一键只该有一个赢家）", firsts)
	}
	if n := len(f.childTasks(t, task.ID)); n != 1 {
		t.Fatalf("子任务 %d 条, want 1（并发重发不能重复派发）", n)
	}
	if n := len(f.emissions(t, task.ID)); n != 1 {
		t.Fatalf("emissions %d 条, want 1", n)
	}
}
