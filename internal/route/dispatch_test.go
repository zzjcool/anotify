package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- mock store ----------

// fakeStore 是 route.Store 的内存实现，覆盖派发全部分支。
//
// 不用真 SQLite：路由层是纯逻辑（契约 §4「最纯的一块」），
// mock 能让每个剪枝原因都精确构造，且测试毫秒级。
type fakeStore struct {
	mu sync.Mutex
	// 出口表：(org, from, port) → []toCapability
	edges map[string][]string
	// 能力定义：id → 定义
	caps map[string]*store.CapabilityVersion
	// 能力 → 声明它的节点
	nodesByCap map[string][]string
	// 节点 → 实例
	instances map[string][]*store.NodeInstance

	pins map[string]*store.NodePin

	// 记录调用，供断言
	createdTasks []*store.Task
	assignments  []assignment
	leasedSlots  map[string]string // instanceID → taskID
	graphCalls   int
	// pruneEvents 是 InsertPruneEvent 收到的剪枝事件（#142）
	pruneEvents []*store.Event

	// 可注入的错误
	errEdges       error
	errNodes       error
	errCreateTasks error
	errAssignTask  error
	// errInsertPrune 让剪枝落库失败（验证「留痕失败不能打断派发」）
	errInsertPrune error
}

type assignment struct {
	taskID, nodeID, instanceID string
	leaseUntil                 int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		edges:       map[string][]string{},
		caps:        map[string]*store.CapabilityVersion{},
		nodesByCap:  map[string][]string{},
		instances:   map[string][]*store.NodeInstance{},
		pins:        map[string]*store.NodePin{},
		leasedSlots: map[string]string{},
	}
}

// addCap 注册一个能力定义。
func (f *fakeStore) addCap(orgID, name string, version int, outputs []string, schema string) *store.CapabilityVersion {
	c := &store.CapabilityVersion{
		ID:           fmt.Sprintf("cap_%s_%d", name, version),
		OrgID:        orgID,
		Name:         name,
		Version:      version,
		Outputs:      outputs,
		ParamsSchema: json.RawMessage(schema),
		ContentHash:  fmt.Sprintf("hash_%s_%d", name, version),
		Source:       store.CapSourceCustom,
	}
	if schema == "" {
		c.ParamsSchema = json.RawMessage(`{}`)
	}
	f.caps[c.ID] = c
	return c
}

// addEdge 加一条边。
func (f *fakeStore) addEdge(orgID, from, port, to string) {
	key := orgID + "|" + from + "|" + port
	f.edges[key] = append(f.edges[key], to)
}

// addNode 让某节点声明某能力，并申报 n 个空闲槽位。
func (f *fakeStore) addNode(nodeID, capabilityID string, slots int) {
	f.nodesByCap[capabilityID] = append(f.nodesByCap[capabilityID], nodeID)
	insts := make([]*store.NodeInstance, slots)
	for i := 0; i < slots; i++ {
		insts[i] = &store.NodeInstance{
			ID:          fmt.Sprintf("%s_slot%d", nodeID, i),
			NodeID:      nodeID,
			Slot:        i,
			Concurrency: 1,
			Alive:       true,
		}
	}
	f.instances[nodeID] = insts
}

func (f *fakeStore) EdgesFrom(ctx context.Context, orgID string, fromIsSource bool, fromKey, fromPort string) ([]string, error) {
	if f.errEdges != nil {
		return nil, f.errEdges
	}
	// 入口边（fromIsSource）与能力边分开存，避免互相污染
	prefix := orgID + "|"
	if fromIsSource {
		prefix += "src|"
	}
	got := f.edges[prefix+fromKey+"|"+fromPort]
	out := make([]string, len(got))
	copy(out, got)
	return out, nil
}

func (f *fakeStore) GetCapabilityByID(ctx context.Context, orgID, id string) (*store.CapabilityVersion, error) {
	c, ok := f.caps[id]
	if !ok || c.OrgID != orgID {
		return nil, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeStore) NodesForCapability(ctx context.Context, orgID, capabilityID string) ([]string, error) {
	if f.errNodes != nil {
		return nil, f.errNodes
	}
	got := f.nodesByCap[capabilityID]
	out := make([]string, len(got))
	copy(out, got)
	return out, nil
}

func (f *fakeStore) ListInstances(ctx context.Context, nodeID string) ([]*store.NodeInstance, error) {
	return f.instances[nodeID], nil
}

// FreeSlotsByNode 与真实 store 同口径（alive 且 busy_task 为空），
// 从同一份 f.instances 计算，这样两条路径不会给出不同答案。
func (f *fakeStore) FreeSlotsByNode(ctx context.Context, nodeIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(nodeIDs))
	for _, id := range nodeIDs {
		free := 0
		for _, in := range f.instances[id] {
			if in.Alive && in.BusyTask == nil {
				free++
			}
		}
		out[id] = free
	}
	return out, nil
}

// LeaseInstance 是 fake 的**内部 helper**（不再属于 route.Store）：模拟占一个空闲槽位。
func (f *fakeStore) LeaseInstance(ctx context.Context, nodeID, taskID string) (string, error) {
	for _, in := range f.instances[nodeID] {
		if in.Alive && in.BusyTask == nil {
			in.BusyTask = &taskID
			f.leasedSlots[in.ID] = taskID
			return in.ID, nil
		}
	}
	return "", store.ErrNoCapacity
}

func (f *fakeStore) ReleaseInstance(ctx context.Context, nodeID, instanceID string) error {
	delete(f.leasedSlots, instanceID)
	for _, in := range f.instances[nodeID] {
		if in.ID == instanceID {
			in.BusyTask = nil
		}
	}
	return nil
}

func (f *fakeStore) ReleaseInstanceByTask(ctx context.Context, nodeID, taskID string) error {
	for _, in := range f.instances[nodeID] {
		if in.BusyTask != nil && *in.BusyTask == taskID {
			in.BusyTask = nil
			delete(f.leasedSlots, in.ID)
		}
	}
	return nil
}

func (f *fakeStore) GetPin(ctx context.Context, stickyKey string) (*store.NodePin, error) {
	p, ok := f.pins[stickyKey]
	if !ok {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) PutPin(ctx context.Context, p *store.NodePin) error {
	cp := *p
	f.pins[p.StickyKey] = &cp
	return nil
}

func (f *fakeStore) ExtendLease(ctx context.Context, orgID, taskID string, leaseUntil int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.createdTasks {
		if t.ID == taskID {
			t.LeaseUntil = &leaseUntil
		}
	}
	return nil
}

// SweepExpiredLeases 模拟租约巡检：把过期租约的任务退回 pending 并清归属。
func (f *fakeStore) SweepExpiredLeases(ctx context.Context, now int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var reclaimed []string
	for _, t := range f.createdTasks {
		if t.Status != store.TaskAssigned {
			continue
		}
		if t.LeaseUntil == nil || *t.LeaseUntil >= now {
			continue
		}
		t.Status = store.TaskPending
		t.NodeID = nil
		t.InstanceID = nil
		t.LeaseUntil = nil
		reclaimed = append(reclaimed, t.ID)
	}
	return reclaimed, nil
}

func (f *fakeStore) CreateTasks(ctx context.Context, ts []*store.Task) error {
	if f.errCreateTasks != nil {
		return f.errCreateTasks
	}
	f.createdTasks = append(f.createdTasks, ts...)
	return nil
}

// AssignTask 也是 fake 的内部 helper：记录一次归属写入，可注入错误。
func (f *fakeStore) AssignTask(ctx context.Context, orgID, taskID, nodeID, instanceID string, leaseUntil int64) error {
	if f.errAssignTask != nil {
		return f.errAssignTask
	}
	f.assignments = append(f.assignments, assignment{taskID, nodeID, instanceID, leaseUntil})
	return nil
}

// LeaseAndAssign 是 route.Store 要求的派发入口（#141）：真实实现里
// 「占槽位 + pending→assigned + 写归属/attempts」是**一个事务**。
//
// fake 没有事务，用「AssignTask 失败就把刚占的槽位放回去」模拟同一语义：
// 任一步失败后占住的槽位必须看不见（真实实现靠 Rollback 做到）。
// errAssignTask 的注入位置不变——仍然只在 AssignTask 那一步生效。
func (f *fakeStore) LeaseAndAssign(ctx context.Context, orgID, taskID, nodeID string, leaseUntil int64) (string, error) {
	instanceID, err := f.LeaseInstance(ctx, nodeID, taskID)
	if err != nil {
		return "", err
	}
	if err := f.AssignTask(ctx, orgID, taskID, nodeID, instanceID, leaseUntil); err != nil {
		// 模拟事务回滚：槽位不泄漏
		if rbErr := f.ReleaseInstance(ctx, nodeID, instanceID); rbErr != nil {
			return "", rbErr
		}
		return "", err
	}
	return instanceID, nil
}

func (f *fakeStore) EnsureGraphVersion(ctx context.Context, orgID string) (*store.GraphVersion, error) {
	f.graphCalls++
	return &store.GraphVersion{ID: "grf_fake", OrgID: orgID, ContentHash: "graphhash"}, nil
}

func (f *fakeStore) IsOrgMember(ctx context.Context, orgID, userID string) (bool, error) {
	// 约定：userID 以 "member" 开头的是本组织成员，其余是外部用户。
	// （用 HasPrefix 而不是切片比较：切片下标写错一位就会静默判错，见 TestForeignUser。）
	return strings.HasPrefix(userID, "member"), nil
}

// InsertPruneEvent 按 (org_id, trace_id, source, type, payload) 去重，
// 与真 store 的「存在性判断」同口径（#142）。
//
// 同口径很重要：否则 mock 里能看到 2 行、真库里只有 1 行，
// 测试会为一种不存在的存储行为背书。
func (f *fakeStore) InsertPruneEvent(ctx context.Context, e *store.Event) (bool, error) {
	if f.errInsertPrune != nil {
		return false, f.errInsertPrune
	}
	for _, got := range f.pruneEvents {
		if got.OrgID == e.OrgID && got.TraceID == e.TraceID &&
			got.Source == e.Source && got.Type == e.Type &&
			string(got.Payload) == string(e.Payload) {
			return false, nil
		}
	}
	f.pruneEvents = append(f.pruneEvents, e)
	return true, nil
}

// ---------- 测试夹具 ----------

const testOrg = "org_test"

// setupFanout 构造：review--fix--> {fix1, fix2, fix3}，三台机器各声明一个。
func setupFanout(t *testing.T) (*fakeStore, *Dispatcher, *store.CapabilityVersion, []*store.CapabilityVersion) {
	t.Helper()
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{"type":"object"}`)
	targets := make([]*store.CapabilityVersion, 0, 3)
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("fix%d", i)
		c := f.addCap(testOrg, name, 1, nil, `{"type":"object"}`)
		f.addEdge(testOrg, review.ID, "fix", c.ID)
		f.addNode(fmt.Sprintf("nd_%s", name), c.ID, 2)
		targets = append(targets, c)
	}
	return f, New(f), review, targets
}

// ---------- #65：派发四步 ----------

// TestDispatchFanout 一个 (cap, port) 有 3 行边 → 3 个任务（#65 验收）。
func TestDispatchFanout(t *testing.T) {
	f, d, review, targets := setupFanout(t)

	out, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "trace_1", FromCap: review.ID, Out: "fix",
		Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 3 {
		t.Fatalf("fanout created %d tasks, want 3", len(out.Tasks))
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("unexpected prunes: %v", out.Pruned)
	}
	// 每个目标能力各一个任务
	got := map[string]bool{}
	for _, task := range out.Tasks {
		got[task.CapabilityID] = true
		if task.Status != store.TaskAssigned {
			t.Errorf("task %s status = %q, want assigned", task.Capability, task.Status)
		}
		if task.TraceID != "trace_1" {
			t.Errorf("task trace_id = %q", task.TraceID)
		}
		if task.GraphID != "grf_fake" {
			t.Errorf("task graph_id = %q, want pinned graph", task.GraphID)
		}
		if task.CapHash == "" {
			t.Error("task missing cap_hash (version pinning, I8)")
		}
	}
	for _, c := range targets {
		if !got[c.ID] {
			t.Errorf("no task for target %s", c.Name)
		}
	}
	if len(f.assignments) != 3 {
		t.Fatalf("assignments = %d, want 3", len(f.assignments))
	}
}

// TestDispatchPartialPrune 扇出里 1 条参数不过 → 2 个任务 + 1 条 param_schema（#65 验收）。
func TestDispatchPartialPrune(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{"type":"object"}`)
	// A 要求 params.diff 是非空字符串
	strict := f.addCap(testOrg, "strict", 1, nil,
		`{"type":"object","required":["diff"],"properties":{"diff":{"type":"string","minLength":1}}}`)
	loose := f.addCap(testOrg, "loose", 1, nil, `{"type":"object"}`)
	f.addEdge(testOrg, review.ID, "fix", strict.ID)
	f.addEdge(testOrg, review.ID, "fix", loose.ID)
	f.addNode("nd_strict", strict.ID, 1)
	f.addNode("nd_loose", loose.ID, 1)

	d := New(f)
	out, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
		Params: map[string]any{}, // 没有 diff
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].CapabilityID != loose.ID {
		t.Fatalf("want 1 task for loose, got %d tasks", len(out.Tasks))
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamSchema {
		t.Fatalf("want 1 param_schema prune, got %v", out.Pruned)
	}
	if out.Pruned[0].CapabilityID != strict.ID {
		t.Fatalf("prune recorded for %q, want %q", out.Pruned[0].CapabilityID, strict.ID)
	}
}

// TestDispatchNoEdge 查表 0 行 → pruned/no_edge（#65 验收）。
func TestDispatchNoEdge(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"nobody-listens"}, `{}`)
	d := New(f)

	out, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "nobody-listens",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("no_edge produced %d tasks", len(out.Tasks))
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonNoEdge {
		t.Fatalf("want no_edge prune, got %v", out.Pruned)
	}
	// 0 行边时不该去落图版本（省一次写）
	if f.graphCalls != 0 {
		t.Errorf("no_edge pinned a graph version (%d calls), should skip", f.graphCalls)
	}
}

// TestDispatchNoReportIsNotPrune 上游没回报出口 → 不查表、不剪枝（契约 §7 第 ① 行）。
func TestDispatchNoReportIsNotPrune(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	d := New(f)

	out, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "", // 没回报
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("no-report produced %d tasks", len(out.Tasks))
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("no-report must NOT produce a prune record, got %v", out.Pruned)
	}
}

// TestDispatchValidation 入参校验。
func TestDispatchValidation(t *testing.T) {
	f := newFakeStore()
	d := New(f)
	if _, err := d.Dispatch(context.Background(), DispatchInput{TraceID: "t"}); err == nil {
		t.Error("empty org accepted")
	}
	if _, err := d.Dispatch(context.Background(), DispatchInput{OrgID: testOrg}); err == nil {
		t.Error("empty trace_id accepted")
	}
	// 无 store
	empty := &Dispatcher{}
	if _, err := empty.Dispatch(context.Background(), DispatchInput{OrgID: testOrg, TraceID: "t"}); err == nil {
		t.Error("dispatcher without store accepted")
	}
}

// TestDispatchNoNode 有边、参数对，但没有节点声明 → pruned/no_node。
func TestDispatchNoNode(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	orphan := f.addCap(testOrg, "orphan", 1, nil, `{}`) // 没有任何节点声明
	f.addEdge(testOrg, review.ID, "fix", orphan.ID)
	d := New(f)

	out, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("no_node produced %d tasks", len(out.Tasks))
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonNoNode {
		t.Fatalf("want no_node, got %v", out.Pruned)
	}
}

// TestDispatchLeavesPendingWhenNoCapacity 有节点但槽位满 → 任务留 pending（不是剪枝）。
func TestDispatchLeavesPendingWhenNoCapacity(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testOrg, "busy", 1, nil, `{}`)
	f.addEdge(testOrg, review.ID, "fix", target.ID)
	// 声明了能力，但 0 个槽位
	f.nodesByCap[target.ID] = []string{"nd_full"}
	f.instances["nd_full"] = nil

	d := New(f)
	out, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("capacity shortage must not prune, got %v", out.Pruned)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("want 1 pending task, got %d", len(out.Tasks))
	}
	if out.Tasks[0].Status != store.TaskPending {
		t.Fatalf("task status = %q, want pending", out.Tasks[0].Status)
	}
	if len(f.assignments) != 0 {
		t.Fatalf("task assigned despite no capacity")
	}
}

// TestDispatchLeaseAndAssignFailureDoesNotLeakSlot LeaseAndAssign 失败时槽位不泄漏（#141）。
//
// 派发的「占槽位 + 改状态 + 写归属」现在是一个事务：AssignTask 失败时
// 占住的槽位随事务回滚消失，所以 Dispatch 里**删掉了**失败后补一次
// ReleaseInstance 的语句。这里用只有 1 个槽位的节点把这个不变量钉住：
// 若槽位泄了，第二次派发会拿到 ErrNoCapacity 静默留 pending（不报错）。
func TestDispatchLeaseAndAssignFailureDoesNotLeakSlot(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testOrg, "fix", 1, nil, `{}`)
	f.addEdge(testOrg, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1) // 只有一个槽位：泄漏就再也占不到
	d := New(f)

	in := DispatchInput{OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix"}

	f.errAssignTask = errors.New("constraint")
	if _, err := d.Dispatch(context.Background(), in); err == nil {
		t.Fatal("lease+assign error was swallowed")
	}
	if len(f.leasedSlots) != 0 {
		t.Fatalf("failed lease+assign leaked slot(s): %v", f.leasedSlots)
	}

	// 同一节点仍能再次占位：槽位没有被那次失败的事务扣掉。
	f.errAssignTask = nil
	out, err := d.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("redispatch after failed lease+assign: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(out.Tasks))
	}
	if out.Tasks[0].Status != store.TaskAssigned {
		t.Fatalf("task status = %q, want assigned (slot leaked by failed transaction?)", out.Tasks[0].Status)
	}
	if len(f.leasedSlots) != 1 {
		t.Fatalf("leased slots = %d, want 1", len(f.leasedSlots))
	}
}

// TestDispatchPicksLeastLoadedNode 挑最空闲的那台。
func TestDispatchPicksLeastLoadedNode(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testOrg, "work", 1, nil, `{}`)
	f.addEdge(testOrg, review.ID, "fix", target.ID)
	// busy 只有 1 个槽（且已被占），idle 有 5 个空槽
	f.nodesByCap[target.ID] = []string{"nd_busy", "nd_idle"}
	busyTask := "tsk_other"
	f.instances["nd_busy"] = []*store.NodeInstance{
		{ID: "busy0", NodeID: "nd_busy", Slot: 0, Alive: true, BusyTask: &busyTask},
	}
	var idleInsts []*store.NodeInstance
	for i := 0; i < 5; i++ {
		idleInsts = append(idleInsts, &store.NodeInstance{
			ID: fmt.Sprintf("idle%d", i), NodeID: "nd_idle", Slot: i, Alive: true,
		})
	}
	f.instances["nd_idle"] = idleInsts

	d := New(f)
	out, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(out.Tasks))
	}
	if out.Tasks[0].NodeID == nil || *out.Tasks[0].NodeID != "nd_idle" {
		t.Fatalf("picked %v, want nd_idle (least loaded)", out.Tasks[0].NodeID)
	}
}

// TestDispatchPathIncludedInChildTask path 会带上父级序列 + 本任务能力。
func TestDispatchPathIncludedInChildTask(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testOrg, "fix", 1, nil, `{}`)
	f.addEdge(testOrg, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1)

	d := New(f)
	out, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", ParentTaskID: "tsk_parent",
		FromCap: review.ID, Out: "fix",
		Path: []string{review.ID},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(out.Tasks))
	}
	task := out.Tasks[0]
	if len(task.Path) != 2 || task.Path[0] != review.ID || task.Path[1] != target.ID {
		t.Fatalf("path = %v, want [%s %s]", task.Path, review.ID, target.ID)
	}
	if task.ParentTaskID == nil || *task.ParentTaskID != "tsk_parent" {
		t.Fatalf("parent_task_id = %v", task.ParentTaskID)
	}
}

// TestDispatchAckLeaseWindow 派发后的初始租约是 ack 等待窗口（20s），不是 30s TTL。
func TestDispatchAckLeaseWindow(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testOrg, "fix", 1, nil, `{}`)
	f.addEdge(testOrg, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1)

	now := int64(1730000000)
	d := New(f)
	d.Now = func() int64 { return now }
	if _, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(f.assignments) != 1 {
		t.Fatalf("want 1 assignment, got %d", len(f.assignments))
	}
	want := now + store.AckWaitSec
	if f.assignments[0].leaseUntil != want {
		t.Fatalf("lease_until = %d, want %d (ack window)", f.assignments[0].leaseUntil, want)
	}
}

// TestDispatchStoreErrorsPropagate store 报错必须向上抛，不能当成剪枝静默吞掉。
func TestDispatchStoreErrorsPropagate(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testOrg, "fix", 1, nil, `{}`)
	f.addEdge(testOrg, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1)
	d := New(f)

	f.errEdges = errors.New("db down")
	if _, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
	}); err == nil {
		t.Fatal("edges error was swallowed")
	}
	f.errEdges = nil

	f.errNodes = errors.New("db down")
	if _, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
	}); err == nil {
		t.Fatal("nodes error was swallowed")
	}
	f.errNodes = nil

	f.errCreateTasks = errors.New("constraint")
	if _, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "t", FromCap: review.ID, Out: "fix",
	}); err == nil {
		t.Fatal("create tasks error was swallowed")
	}
}

// ---------- 粘性路由 ----------

// TestDispatchStickyPin sticky 能力：同一 trace 第二次派发复用同一台机器。
func TestDispatchStickyPin(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testOrg, "implement", 1, nil, `{}`)
	target.Sticky = "worktree" // sticky 字段是 params 里的键名
	f.addEdge(testOrg, review.ID, "fix", target.ID)
	f.addNode("nd_a", target.ID, 4)
	f.addNode("nd_b", target.ID, 4)

	d := New(f)
	params := map[string]any{"worktree": "wt-feature-x"}

	out1, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "trace_sticky", FromCap: review.ID, Out: "fix", Params: params,
	})
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	node1 := *out1.Tasks[0].NodeID

	// pin 已写入
	if len(f.pins) != 1 {
		t.Fatalf("want 1 pin, got %d", len(f.pins))
	}
	key := store.StickyKey("trace_sticky", target.ID, "wt-feature-x")
	if _, ok := f.pins[key]; !ok {
		t.Fatalf("pin key mismatch; have %v", keysOf(f.pins))
	}

	// 第二次派发（同 trace、同 selector）：钉到同一台
	out2, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "trace_sticky", FromCap: review.ID, Out: "fix", Params: params,
	})
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	node2 := *out2.Tasks[0].NodeID
	if node2 != node1 {
		t.Fatalf("sticky routing broken: first=%s second=%s", node1, node2)
	}

	// 不同 trace → 不同选择器值 → 不同 pin（但可能仍是同一台，负载相同）
	out3, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "trace_other", FromCap: review.ID, Out: "fix", Params: params,
	})
	if err != nil {
		t.Fatalf("third dispatch: %v", err)
	}
	if len(f.pins) != 2 {
		t.Fatalf("want 2 pins for 2 traces, got %d", len(f.pins))
	}
	_ = out3
}

// TestDispatchStickyFallsBackWhenPinnedNodeGone 钉住的机器不再声明该能力 → 重新挑。
func TestDispatchStickyFallsBackWhenPinnedNodeGone(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testOrg, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testOrg, "implement", 1, nil, `{}`)
	target.Sticky = "worktree"
	f.addEdge(testOrg, review.ID, "fix", target.ID)
	f.addNode("nd_alive", target.ID, 2)

	// 预先钉到一个已经不在候选里的节点
	key := store.StickyKey("trace_x", target.ID, "wt-1")
	f.pins[key] = &store.NodePin{StickyKey: key, OrgID: testOrg, NodeID: "nd_dead", Selector: "wt-1"}

	d := New(f)
	out, err := d.Dispatch(context.Background(), DispatchInput{
		OrgID: testOrg, TraceID: "trace_x", FromCap: review.ID, Out: "fix",
		Params: map[string]any{"worktree": "wt-1"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if out.Tasks[0].NodeID == nil || *out.Tasks[0].NodeID != "nd_alive" {
		t.Fatalf("picked %v, want nd_alive (pinned node no longer candidate)", out.Tasks[0].NodeID)
	}
}

// ---------- 校验器 ----------

// TestValidateParamsSubset 参数校验子集的关键字。
func TestValidateParamsSubset(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		params map[string]any
		wantOK bool
	}{
		{
			name:   "no schema means accept all",
			schema: `{}`,
			params: map[string]any{"anything": 1},
			wantOK: true,
		},
		{
			name:   "required present",
			schema: `{"type":"object","required":["to"]}`,
			params: map[string]any{"to": "member_1"},
			wantOK: true,
		},
		{
			name:   "required missing",
			schema: `{"type":"object","required":["to"]}`,
			params: map[string]any{},
			wantOK: false,
		},
		{
			name:   "type mismatch",
			schema: `{"type":"object","properties":{"n":{"type":"number"}}}`,
			params: map[string]any{"n": "not-a-number"},
			wantOK: false,
		},
		{
			name:   "integer rejects float",
			schema: `{"type":"object","properties":{"n":{"type":"integer"}}}`,
			params: map[string]any{"n": 1.5},
			wantOK: false,
		},
		{
			name:   "integer accepts whole float",
			schema: `{"type":"object","properties":{"n":{"type":"integer"}}}`,
			params: map[string]any{"n": float64(3)},
			wantOK: true,
		},
		{
			name:   "type union",
			schema: `{"type":"object","properties":{"n":{"type":["string","null"]}}}`,
			params: map[string]any{"n": nil},
			wantOK: true,
		},
		{
			name:   "additionalProperties false",
			schema: `{"type":"object","properties":{"a":{}},"additionalProperties":false}`,
			params: map[string]any{"a": 1, "b": 2},
			wantOK: false,
		},
		{
			name:   "minLength",
			schema: `{"type":"object","properties":{"s":{"type":"string","minLength":3}}}`,
			params: map[string]any{"s": "ab"},
			wantOK: false,
		},
		{
			name:   "maxItems",
			schema: `{"type":"object","properties":{"xs":{"type":"array","maxItems":2}}}`,
			params: map[string]any{"xs": []any{1.0, 2.0, 3.0}},
			wantOK: false,
		},
		{
			name:   "items schema",
			schema: `{"type":"object","properties":{"xs":{"type":"array","items":{"type":"string"}}}}`,
			params: map[string]any{"xs": []any{"a", 1.0}},
			wantOK: false,
		},
		{
			name:   "enum",
			schema: `{"type":"object","properties":{"k":{"enum":["a","b"]}}}`,
			params: map[string]any{"k": "c"},
			wantOK: false,
		},
		{
			name:   "minimum",
			schema: `{"type":"object","properties":{"n":{"type":"number","minimum":5}}}`,
			params: map[string]any{"n": 1.0},
			wantOK: false,
		},
		{
			name:   "pattern ok",
			schema: `{"type":"object","properties":{"sha":{"type":"string","pattern":"^[0-9a-f]{7,40}$"}}}`,
			params: map[string]any{"sha": "abc1234"},
			wantOK: true,
		},
		{
			name:   "pattern fail",
			schema: `{"type":"object","properties":{"sha":{"type":"string","pattern":"^[0-9a-f]{7,40}$"}}}`,
			params: map[string]any{"sha": "ZZZ"},
			wantOK: false,
		},
		{
			name:   "unknown keyword ignored (I6: no evaluator)",
			schema: `{"type":"object","$ref":"#/definitions/x","if":{"type":"string"}}`,
			params: map[string]any{"whatever": true},
			wantOK: true,
		},
		{
			name:   "broken schema rejects",
			schema: `{"type": "object"`,
			params: map[string]any{},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := validateParams(json.RawMessage(c.schema), c.params)
			ok := msg == ""
			if ok != c.wantOK {
				t.Fatalf("validate = %q (ok=%v), want ok=%v", msg, ok, c.wantOK)
			}
		})
	}
}

// TestValidateErrorIsDeterministic 同一个坏输入总报同一个错（对象字段按名排序）。
func TestValidateErrorIsDeterministic(t *testing.T) {
	schema := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"additionalProperties":false}`
	var first string
	for i := 0; i < 20; i++ {
		params := map[string]any{"a": 1.0, "b": 2.0}
		msg := validateParams(json.RawMessage(schema), params)
		if i == 0 {
			first = msg
			if first == "" {
				t.Fatal("expected a validation error")
			}
			continue
		}
		if msg != first {
			t.Fatalf("non-deterministic error: %q vs %q", first, msg)
		}
	}
}

// TestValidateNestedPathError 错误信息带可读位置。
func TestValidateNestedPathError(t *testing.T) {
	schema := `{"type":"object","properties":{"repo":{"type":"object","properties":{"name":{"type":"string"}}}}}`
	msg := validateParams(json.RawMessage(schema), map[string]any{
		"repo": map[string]any{"name": 42.0},
	})
	if msg == "" {
		t.Fatal("expected validation error")
	}
	if !contains(msg, "params.repo.name") {
		t.Fatalf("error %q should mention params.repo.name", msg)
	}
}

// ---------- 小工具 ----------

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
