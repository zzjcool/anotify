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
	// errGetUser 让用户查询失败（验证「鉴权查库失败必须 fail closed」）
	errGetUser error
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

// addCap 注册一个能力定义（属于 nsID 这个命名空间）。
//
// id 里带 nsID：能力 id 全局唯一，而两个命名空间可以有**同名同版本**的能力
// （如各自的 review@1）——用「名字@版本」当 id 会让它们撞成一个，
// 那样「按命名空间隔离」的断言就永远不可能失败（假绿）。
func (f *fakeStore) addCap(nsID, name string, version int, outputs []string, schema string) *store.CapabilityVersion {
	c := &store.CapabilityVersion{
		ID:           fmt.Sprintf("cap_%s_%s_%d", nsID, name, version),
		NamespaceID:  nsID,
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

// addEdge 加一条边（edges 属于某个命名空间，契约 §1）。
func (f *fakeStore) addEdge(nsID, from, port, to string) {
	key := nsID + "|" + from + "|" + port
	f.edges[key] = append(f.edges[key], to)
}

// capKey 是 fake 里「命名空间 + 能力」的复合键。
//
// 与真 store 的 node_capabilities.namespace_id 同口径：③ 找节点时少了
// namespace 维度就**查不到节点**（而不是照样返回别人的节点）。mock 若宽容，
// 「漏传命名空间」这类回归在测试里就永远是绿的。
func capKey(nsID, capabilityID string) string { return nsID + "|" + capabilityID }

// capNamespace 取能力定义所属的命名空间（真库里 node 与 cap 必须同 namespace，I11③）。
func (f *fakeStore) capNamespace(capabilityID string) string {
	if c := f.caps[capabilityID]; c != nil {
		return c.NamespaceID
	}
	return ""
}

// addNode 让某节点声明某能力，并申报 n 个空闲槽位。
func (f *fakeStore) addNode(nodeID, capabilityID string, slots int) {
	key := capKey(f.capNamespace(capabilityID), capabilityID)
	f.nodesByCap[key] = append(f.nodesByCap[key], nodeID)
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

// setNodes 只让这些节点「声明」某能力，不申报实例（用于构造 0 槽位/负载不均的场景）。
func (f *fakeStore) setNodes(capabilityID string, nodeIDs ...string) {
	f.nodesByCap[capKey(f.capNamespace(capabilityID), capabilityID)] = nodeIDs
}

func (f *fakeStore) EdgesFrom(ctx context.Context, namespaceID string, fromIsSource bool, fromKey, fromPort string) ([]string, error) {
	if f.errEdges != nil {
		return nil, f.errEdges
	}
	// 入口边（fromIsSource）与能力边分开存，避免互相污染。
	// 命名空间是 key 的一部分：同一条 (from, port) 在两个命名空间里各是各的边。
	prefix := namespaceID + "|"
	if fromIsSource {
		prefix += "src|"
	}
	got := f.edges[prefix+fromKey+"|"+fromPort]
	out := make([]string, len(got))
	copy(out, got)
	return out, nil
}

func (f *fakeStore) GetCapabilityByID(ctx context.Context, namespaceID, id string) (*store.CapabilityVersion, error) {
	c, ok := f.caps[id]
	if !ok || c.NamespaceID != namespaceID {
		return nil, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeStore) NodesForCapability(ctx context.Context, namespaceID, capabilityID string) ([]string, error) {
	if f.errNodes != nil {
		return nil, f.errNodes
	}
	got := f.nodesByCap[capKey(namespaceID, capabilityID)]
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

func (f *fakeStore) ExtendLease(ctx context.Context, namespaceID, taskID string, leaseUntil int64) error {
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
func (f *fakeStore) LeaseAndAssign(ctx context.Context, namespaceID, taskID, nodeID string, leaseUntil int64) (string, error) {
	instanceID, err := f.LeaseInstance(ctx, nodeID, taskID)
	if err != nil {
		return "", err
	}
	if err := f.AssignTask(ctx, namespaceID, taskID, nodeID, instanceID, leaseUntil); err != nil {
		// 模拟事务回滚：槽位不泄漏
		if rbErr := f.ReleaseInstance(ctx, nodeID, instanceID); rbErr != nil {
			return "", rbErr
		}
		return "", err
	}
	return instanceID, nil
}

func (f *fakeStore) EnsureGraphVersion(ctx context.Context, namespaceID string) (*store.GraphVersion, error) {
	f.graphCalls++
	return &store.GraphVersion{ID: "grf_fake", NamespaceID: namespaceID, ContentHash: "graphhash"}, nil
}

// GetUserByID 与真 store 同口径：查不到返回 store.ErrNotFound（#204 之后没有组织层，
// 用户维度校验收窄成「这个人在本部署里存在吗」，见 checkUserScoped）。
//
// 约定：userID 以 "member" 开头的是本部署的真实用户，其余查无此人
// （outsider_ / stranger / 空串都走 404 分支）。用 HasPrefix 而不是精确匹配，
// 是为了保住原来那批用例的写法（member_1 / member_alice）而语义不变。
func (f *fakeStore) GetUserByID(id string) (*store.User, error) {
	if f.errGetUser != nil {
		return nil, f.errGetUser
	}
	if !strings.HasPrefix(id, "member") {
		return nil, store.ErrNotFound
	}
	return &store.User{ID: id, Username: id, DisplayName: id}, nil
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
	// 真 store 缺 namespace 直接报错（events 是命名空间资源）：mock 也照此拒绝，
	// 否则「剪枝留痕漏传 namespace」在测试里永远发现不了。
	if e.NamespaceID == "" || e.NamespaceID == store.AllNamespaces {
		return false, store.ErrNamespaceRequired
	}
	for _, got := range f.pruneEvents {
		if got.NamespaceID == e.NamespaceID && got.TraceID == e.TraceID &&
			got.Source == e.Source && got.Type == e.Type &&
			string(got.Payload) == string(e.Payload) {
			return false, nil
		}
	}
	f.pruneEvents = append(f.pruneEvents, e)
	return true, nil
}

// ---------- 测试夹具 ----------

// testNS 是这些用例用的命名空间（派发四步 ①③⑤ 的唯一隔离维度）。
const testNS = "ns_test"

// setupFanout 构造：review--fix--> {fix1, fix2, fix3}，三台机器各声明一个。
func setupFanout(t *testing.T) (*fakeStore, *Dispatcher, *store.CapabilityVersion, []*store.CapabilityVersion) {
	t.Helper()
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	targets := make([]*store.CapabilityVersion, 0, 3)
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("fix%d", i)
		c := f.addCap(testNS, name, 1, nil, `{"type":"object"}`)
		f.addEdge(testNS, review.ID, "fix", c.ID)
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
		NamespaceID: testNS, TraceID: "trace_1", FromCap: review.ID, Out: "fix",
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
		if task.NamespaceID != testNS {
			t.Errorf("task namespace_id = %q, want %q（⑤ 根任务用入参的命名空间）",
				task.NamespaceID, testNS)
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
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{"type":"object"}`)
	// A 要求 params.diff 是非空字符串
	strict := f.addCap(testNS, "strict", 1, nil,
		`{"type":"object","required":["diff"],"properties":{"diff":{"type":"string","minLength":1}}}`)
	loose := f.addCap(testNS, "loose", 1, nil, `{"type":"object"}`)
	f.addEdge(testNS, review.ID, "fix", strict.ID)
	f.addEdge(testNS, review.ID, "fix", loose.ID)
	f.addNode("nd_strict", strict.ID, 1)
	f.addNode("nd_loose", loose.ID, 1)

	d := New(f)
	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
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
	review := f.addCap(testNS, "review", 1, []string{"nobody-listens"}, `{}`)
	d := New(f)

	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "nobody-listens",
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
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	d := New(f)

	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "", // 没回报
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
	// 什么也没有：最先撞到的是「命名空间必填」（隔离维度，不允许回退默认值）
	if _, err := d.Dispatch(context.Background(), DispatchInput{TraceID: "t"}); err == nil {
		t.Error("empty namespace accepted")
	}
	// 有 org + namespace，但没有 trace_id
	if _, err := d.Dispatch(context.Background(), DispatchInput{NamespaceID: testNS}); err == nil {
		t.Error("empty trace_id accepted")
	}
	// 无 store
	empty := &Dispatcher{}
	if _, err := empty.Dispatch(context.Background(), DispatchInput{NamespaceID: testNS, TraceID: "t"}); err == nil {
		t.Error("dispatcher without store accepted")
	}
}

// TestDispatchRejectsEmptyNamespace 命名空间为空 → 报错，且**什么也不建**（契约 §3）。
//
// 为什么不能「回退到默认命名空间」：那会把「调用方忘了带 namespaceId」变成
// 「悄悄写进了 default」——用户的边/任务混进别的命名空间，事后只剩
// 「为什么这条链跑了」没法解释。所以这里硬失败，并且让 handler 能靠
// errors.Is(err, store.ErrNamespaceRequired) 直接翻成 400。
func TestDispatchRejectsEmptyNamespace(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "fix", 1, nil, `{}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 2)
	d := New(f)

	_, err := d.Dispatch(context.Background(), DispatchInput{
		TraceID: "t", FromCap: review.ID, Out: "fix",
		// NamespaceID 故意留空
	})
	if err == nil {
		t.Fatal("空 NamespaceID 竟然派发成功了（会默默写进某个命名空间）")
	}
	if !errors.Is(err, store.ErrNamespaceRequired) {
		t.Fatalf("err = %v, want 可被 errors.Is(store.ErrNamespaceRequired) 识别（handler 靠它翻 400）", err)
	}

	// 硬要求：一个任务、一个槽位、一条剪枝事件、一次图版本也不许落
	if len(f.createdTasks) != 0 {
		t.Fatalf("创建了 %d 个任务, want 0", len(f.createdTasks))
	}
	if len(f.assignments) != 0 || len(f.leasedSlots) != 0 {
		t.Fatalf("占位了: assignments=%d slots=%d, want 0/0", len(f.assignments), len(f.leasedSlots))
	}
	if len(f.pruneEvents) != 0 || f.graphCalls != 0 {
		t.Fatalf("还落了剪枝/图版本: prunes=%d graphCalls=%d, want 0/0", len(f.pruneEvents), f.graphCalls)
	}
}

// TestDispatchIsolatesNamespaces 两个命名空间各有一套同名能力与各自的边，
// 派发只能命中**本命名空间**那条（契约 §3 四步的 ①③⑤）。
//
// 这是命名空间隔离在派发侧的硬断言：两个命名空间里都有 review@1 → fix@1，
// 且各自都有节点声明。若 ① 查边或 ③ 找节点漏了 namespace 维度，
// 就会把另一个命名空间的 capability_id 派出来（串数据的典型形态）。
func TestDispatchIsolatesNamespaces(t *testing.T) {
	const nsA, nsB = "ns_a", "ns_b"
	f := newFakeStore()

	// 两个命名空间：同名同版本的能力各一份（id 不同）
	reviewA := f.addCap(nsA, "review", 1, []string{"fix"}, `{}`)
	fixA := f.addCap(nsA, "fix", 1, nil, `{}`)
	f.addEdge(nsA, reviewA.ID, "fix", fixA.ID)
	f.addNode("nd_fix_a", fixA.ID, 2)

	reviewB := f.addCap(nsB, "review", 1, []string{"fix"}, `{}`)
	fixB := f.addCap(nsB, "fix", 1, nil, `{}`)
	f.addEdge(nsB, reviewB.ID, "fix", fixB.ID)
	f.addNode("nd_fix_b", fixB.ID, 2)

	if reviewA.ID == reviewB.ID || fixA.ID == fixB.ID {
		t.Fatal("用例前提不成立：两个命名空间的能力 id 必须不同（id 全局唯一）")
	}

	d := New(f)

	// ① 在 nsA 里派发：只建 nsA 的任务
	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: nsA, TraceID: "trace_a",
		FromCap: reviewA.ID, Out: "fix", Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch in ns_a: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("ns_a 派发了 %d 个任务, want 1", len(out.Tasks))
	}
	if got := out.Tasks[0]; got.CapabilityID != fixA.ID || got.NamespaceID != nsA {
		t.Fatalf("ns_a 的任务 = %s/%s, want %s/%s（串到别的命名空间了）",
			got.CapabilityID, got.NamespaceID, fixA.ID, nsA)
	}
	for _, task := range out.Tasks {
		if task.CapabilityID == fixB.ID {
			t.Fatalf("ns_b 的能力 %s 出现在了 ns_a 的派发结果里（①③ 漏了 namespace 维度）", fixB.ID)
		}
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("ns_a 不该有剪枝: %v", out.Pruned)
	}

	// ② 反向：在 nsB 里派发只建 nsB 的任务
	out, err = d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: nsB, TraceID: "trace_b",
		FromCap: reviewB.ID, Out: "fix", Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch in ns_b: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].CapabilityID != fixB.ID || out.Tasks[0].NamespaceID != nsB {
		t.Fatalf("ns_b 的任务 = %+v, want 恰好 %s/%s", out.Tasks, fixB.ID, nsB)
	}
	for _, task := range out.Tasks {
		if task.CapabilityID == fixA.ID {
			t.Fatalf("ns_a 的能力 %s 出现在了 ns_b 的派发结果里", fixA.ID)
		}
	}

	// ③ 边的隔离：nsA 里**没有** review --deploy--> 任何东西，
	// 即使 nsB 有这条边，也只会在 nsA 里得到 no_edge（必须留痕，不能借别人的边）。
	review2B := f.addCap(nsB, "review2", 1, []string{"deploy"}, `{}`)
	deployB := f.addCap(nsB, "deploy", 1, nil, `{}`)
	f.addEdge(nsB, review2B.ID, "deploy", deployB.ID)
	f.addNode("nd_deploy_b", deployB.ID, 2)

	// nsA 里同名的 review2 能力（不同 id、没有任何边）
	review2A := f.addCap(nsA, "review2", 1, []string{"deploy"}, `{}`)
	out, err = d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: nsA, TraceID: "trace_a2",
		FromCap: review2A.ID, Out: "deploy", Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("dispatch review2 in ns_a: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("ns_a 借到了 ns_b 的边，建了 %d 个任务: %+v", len(out.Tasks), out.Tasks)
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonNoEdge {
		t.Fatalf("out.Pruned = %+v, want 一条 no_edge（ns_a 本来就没这条边）", out.Pruned)
	}
	// 剪枝留痕也要落在**本命名空间**（events 是命名空间资源）
	events := pruneEventsFor(f, "trace_a2")
	if len(events) != 1 || events[0].NamespaceID != nsA {
		t.Fatalf("剪枝事件 = %+v, want 恰好 1 条且 namespace_id = %s", events, nsA)
	}
}

// TestDispatchSourceTargetsStayInNamespace 源触发（in.Targets 非空）同样按命名空间隔离。
//
// 源不走 edges 查表（它自己就是起点），所以这条路径少了 ① 那一层；
// 但 ③ 找节点、⑤ 建任务仍必须落在**源自己的**命名空间里（调用方从
// src.NamespaceID 传进来，契约 §3）。跨命名空间的目标能力是明确错误，
// 不能静默建出一条属于别的命名空间的任务行。
func TestDispatchSourceTargetsStayInNamespace(t *testing.T) {
	const nsA, nsB = "ns_src_a", "ns_src_b"
	ctx := context.Background()
	f := newFakeStore()
	fixA := f.addCap(nsA, "fix", 1, nil, `{}`)
	f.addNode("nd_a", fixA.ID, 2)
	fixB := f.addCap(nsB, "fix", 1, nil, `{}`)
	f.addNode("nd_b", fixB.ID, 2)

	d := New(f)
	out, err := d.Dispatch(ctx, DispatchInput{
		NamespaceID: nsA, TraceID: "trace_src",
		Targets: []string{fixA.ID}, Out: "push", Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("source dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("源触发建了 %d 个任务, want 1", len(out.Tasks))
	}
	task := out.Tasks[0]
	if task.NamespaceID != nsA || task.CapabilityID != fixA.ID {
		t.Fatalf("task = %s/%s, want %s/%s", task.CapabilityID, task.NamespaceID, fixA.ID, nsA)
	}
	if task.NodeID == nil || *task.NodeID != "nd_a" {
		t.Fatalf("task node_id = %v, want nd_a（③ 只能在本命名空间里找）", task.NodeID)
	}

	// 目标是别的命名空间的能力 → 报错，不建任何行（GetCapabilityByID 按 namespace 查不到）
	_, err = d.Dispatch(ctx, DispatchInput{
		NamespaceID: nsA, TraceID: "trace_src_bad",
		Targets: []string{fixB.ID}, Out: "push", Params: map[string]any{},
	})
	if err == nil {
		t.Fatal("源触发的目标指向了别的命名空间的能力，却派发成功了（会串数据）")
	}
	if len(f.createdTasks) != 1 {
		t.Fatalf("createdTasks = %d, want 1（失败那次不许建行）", len(f.createdTasks))
	}
}

// TestDispatchNoNode 有边、参数对，但没有节点声明 → pruned/no_node。
func TestDispatchNoNode(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	orphan := f.addCap(testNS, "orphan", 1, nil, `{}`) // 没有任何节点声明
	f.addEdge(testNS, review.ID, "fix", orphan.ID)
	d := New(f)

	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
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
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "busy", 1, nil, `{}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	// 声明了能力，但 0 个槽位（setNodes 只声明、不申报实例）
	f.setNodes(target.ID, "nd_full")
	f.instances["nd_full"] = nil

	d := New(f)
	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
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
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "fix", 1, nil, `{}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1) // 只有一个槽位：泄漏就再也占不到
	d := New(f)

	in := DispatchInput{NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix"}

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
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "work", 1, nil, `{}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	// busy 只有 1 个槽（且已被占），idle 有 5 个空槽
	f.setNodes(target.ID, "nd_busy", "nd_idle")
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
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
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
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "fix", 1, nil, `{}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1)

	d := New(f)
	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", ParentTaskID: "tsk_parent",
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
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "fix", 1, nil, `{}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1)

	now := int64(1730000000)
	d := New(f)
	d.Now = func() int64 { return now }
	if _, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
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
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "fix", 1, nil, `{}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1)
	d := New(f)

	f.errEdges = errors.New("db down")
	if _, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
	}); err == nil {
		t.Fatal("edges error was swallowed")
	}
	f.errEdges = nil

	f.errNodes = errors.New("db down")
	if _, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
	}); err == nil {
		t.Fatal("nodes error was swallowed")
	}
	f.errNodes = nil

	f.errCreateTasks = errors.New("constraint")
	if _, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t", FromCap: review.ID, Out: "fix",
	}); err == nil {
		t.Fatal("create tasks error was swallowed")
	}
}

// ---------- 粘性路由 ----------

// TestDispatchStickyPin sticky 能力：同一 trace 第二次派发复用同一台机器。
func TestDispatchStickyPin(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "implement", 1, nil, `{}`)
	target.Sticky = "worktree" // sticky 字段是 params 里的键名
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_a", target.ID, 4)
	f.addNode("nd_b", target.ID, 4)

	d := New(f)
	params := map[string]any{"worktree": "wt-feature-x"}

	out1, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "trace_sticky", FromCap: review.ID, Out: "fix", Params: params,
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
		NamespaceID: testNS, TraceID: "trace_sticky", FromCap: review.ID, Out: "fix", Params: params,
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
		NamespaceID: testNS, TraceID: "trace_other", FromCap: review.ID, Out: "fix", Params: params,
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
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "implement", 1, nil, `{}`)
	target.Sticky = "worktree"
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_alive", target.ID, 2)

	// 预先钉到一个已经不在候选里的节点
	key := store.StickyKey("trace_x", target.ID, "wt-1")
	f.pins[key] = &store.NodePin{StickyKey: key, NamespaceID: testNS, NodeID: "nd_dead", Selector: "wt-1"}

	d := New(f)
	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "trace_x", FromCap: review.ID, Out: "fix",
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

// ---------- #114：源参数模板求值（Payload / VarValues → 实际参数） ----------

// sourceInput 组一次「源首次派发」的输入：Targets 非空 → 不查边，
// TargetParams 是按目标的**模板**（含 {{x}} / {from} / {var}）。
func sourceInput(capID string, template map[string]any, payload map[string]any) DispatchInput {
	return DispatchInput{
		NamespaceID: testNS, TraceID: "t_source",
		FromCap: "github", FromIsSource: true, Out: "push",
		Targets:      []string{capID},
		TargetParams: map[string]map[string]any{capID: template},
		Payload:      payload,
	}
}

// decodeTaskParams 把任务的 params 解成 map（断言用）。
func decodeTaskParams(t *testing.T, task *store.Task) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(task.Params, &m); err != nil {
		t.Fatalf("task params %q 解析失败: %v", string(task.Params), err)
	}
	return m
}

// TestDispatchResolvesParamTemplate 源路径的 `{{n}}` 被解析成 payload 里的值（契约 §4 第 11 项）。
//
// 关键的不是"拿到了 42"，而是**没有产生 param_schema 剪枝**：模板里的
// `"{{n}}"` 在求值前是个字符串，若先拿它去撞 integer schema，会被误拒。
func TestDispatchResolvesParamTemplate(t *testing.T) {
	f := newFakeStore()
	loose := f.addCap(testNS, "loose", 1, nil,
		`{"type":"object","properties":{"n":{"type":"integer"}}}`)
	f.addNode("nd_loose", loose.ID, 1)
	d := New(f)

	out, err := d.Dispatch(context.Background(), sourceInput(loose.ID,
		map[string]any{"n": "{{n}}"},
		map[string]any{"n": float64(42)},
	))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Pruned) != 0 {
		t.Fatalf("含占位符的参数被误拒: %v", out.Pruned)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(out.Tasks))
	}
	got := decodeTaskParams(t, out.Tasks[0])
	if got["n"] != float64(42) {
		t.Fatalf("params.n = %#v, want 42（整值引用保留原类型）", got["n"])
	}
}

// TestDispatchParamTemplateMissing 引用取不到 → param_missing（契约 §4 第 12 项）。
func TestDispatchParamTemplateMissing(t *testing.T) {
	f := newFakeStore()
	loose := f.addCap(testNS, "loose", 1, nil, `{"type":"object"}`)
	f.addNode("nd_loose", loose.ID, 1)
	d := New(f)

	// payload 里没有 pull_request，且没写 default → 该目标整条剪枝
	out, err := d.Dispatch(context.Background(), sourceInput(loose.ID,
		map[string]any{"n": "{{pull_request.title}}"},
		map[string]any{},
	))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("取不到取值点却建了任务: %d", len(out.Tasks))
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamMissing {
		t.Fatalf("want 1 条 param_missing, got %v", out.Pruned)
	}
	detail := out.Pruned[0].Detail
	// 文案逐字冻结（契约 §2.2）；前缀是能力名，所以用子串断言
	if !strings.Contains(detail, "参数 n 取不到 {{pull_request.title}}（payload 里没有这个字段）") {
		t.Fatalf("detail = %q, want 含字段名与原始引用文本", detail)
	}
	if out.Pruned[0].CapabilityID != loose.ID {
		t.Fatalf("剪枝记录挂在 %q, want %q", out.Pruned[0].CapabilityID, loose.ID)
	}
}

// TestDispatchParamTemplateTypeError 取到了值但转不了 → param_type（契约 §4 第 13 项）。
func TestDispatchParamTemplateTypeError(t *testing.T) {
	f := newFakeStore()
	strict := f.addCap(testNS, "strict", 1, nil,
		`{"type":"object","properties":{"lines":{"type":"integer"}}}`)
	f.addNode("nd_strict", strict.ID, 1)
	d := New(f)

	out, err := d.Dispatch(context.Background(), sourceInput(strict.ID,
		map[string]any{"lines": "{{lines}}"},
		map[string]any{"lines": "abc"},
	))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("类型转不了却建了任务: %d", len(out.Tasks))
	}
	if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamType {
		t.Fatalf("want 1 条 param_type, got %v", out.Pruned)
	}
	detail := out.Pruned[0].Detail
	if !strings.Contains(detail, "参数 lines 期望 integer，但求值得到 \"abc\"，转不了") {
		t.Fatalf("detail = %q, want 期望类型 + 实际值", detail)
	}
}

// TestDispatchParamTemplateMixed 字面量与引用混在同一份模板里，两者都落地（契约 §4 第 14 项）。
func TestDispatchParamTemplateMixed(t *testing.T) {
	f := newFakeStore()
	target := f.addCap(testNS, "deploy", 1, nil,
		`{"type":"object","properties":{"env":{"type":"string"},"n":{"type":"integer"}}}`)
	f.addNode("nd_deploy", target.ID, 1)
	d := New(f)

	out, err := d.Dispatch(context.Background(), sourceInput(target.ID,
		map[string]any{"env": "prod", "n": "{{n}}"},
		map[string]any{"n": float64(3)},
	))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Pruned) != 0 || len(out.Tasks) != 1 {
		t.Fatalf("tasks=%d pruned=%v, want 1 task 0 prune", len(out.Tasks), out.Pruned)
	}
	got := decodeTaskParams(t, out.Tasks[0])
	if got["env"] != "prod" {
		t.Errorf("字面量丢失: env = %#v", got["env"])
	}
	if got["n"] != float64(3) {
		t.Errorf("引用未落地: n = %#v", got["n"])
	}
}

// TestDispatchRecursiveDoesNotResolve 能力→能力递归派发**不**解析参数（契约 §2.5）。
//
// 递归路径不带 TargetParams，参数原样传给下一跳：即使上游结果里恰好有
// `{{n}}` 这样的文本，也不会被当成模板再展开一次（*单趟* 是硬边界）。
func TestDispatchRecursiveDoesNotResolve(t *testing.T) {
	f := newFakeStore()
	review := f.addCap(testNS, "review", 1, []string{"fix"}, `{}`)
	target := f.addCap(testNS, "fix", 1, nil, `{"type":"object"}`)
	f.addEdge(testNS, review.ID, "fix", target.ID)
	f.addNode("nd_fix", target.ID, 1)
	d := New(f)

	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t_recursive", FromCap: review.ID, Out: "fix",
		Params:  map[string]any{"n": "{{n}}"},
		Payload: map[string]any{"n": float64(1)}, // 递归路径不该看它
		Path:    []string{review.ID},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d (pruned=%v)", len(out.Tasks), out.Pruned)
	}
	got := decodeTaskParams(t, out.Tasks[0])
	if got["n"] != "{{n}}" {
		t.Fatalf("递归派发把参数当模板展开了: n = %#v, want 原文 \"{{n}}\"", got["n"])
	}
}

// TestDispatchParamMissingDetailSources 未命中的三种引用形态在 detail 里可区分（契约 §4 第 16 项）。
//
// 用户看到 `$v` 就知道是变量声明那边的问题，看到 `payload.a.b` 就知道是
// payload 结构不对——两者病因不同，混成一句话就查不下去了。
func TestDispatchParamMissingDetailSources(t *testing.T) {
	cases := []struct {
		name     string
		template map[string]any
		want     string
	}{
		{
			name:     "占位符回显原文",
			template: map[string]any{"title": "{{x}}"},
			want:     "参数 title 取不到 {{x}}（payload 里没有这个字段）",
		},
		{
			name:     "from 映射报 payload 路径",
			template: map[string]any{"title": map[string]any{"from": "a.b"}},
			want:     "参数 title 取不到 payload.a.b（payload 里没有这个字段）",
		},
		{
			name:     "var 映射报变量名",
			template: map[string]any{"title": map[string]any{"var": "v"}},
			want:     "参数 title 取不到变量 $v（源未声明该变量，或它指向的字段在 payload 里没有）",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeStore()
			loose := f.addCap(testNS, "loose", 1, nil, `{"type":"object"}`)
			f.addNode("nd_loose", loose.ID, 1)
			d := New(f)

			out, err := d.Dispatch(context.Background(), sourceInput(loose.ID, c.template, nil))
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if len(out.Pruned) != 1 || out.Pruned[0].Reason != ReasonParamMissing {
				t.Fatalf("want 1 条 param_missing, got %v", out.Pruned)
			}
			if detail := out.Pruned[0].Detail; !strings.Contains(detail, c.want) {
				t.Fatalf("detail = %q, want 含 %q", detail, c.want)
			}
		})
	}

	// 多个未命中用「；」串起来：一条目标只落一条记录，但病因不能丢
	t.Run("多条未命中用分号连接", func(t *testing.T) {
		f := newFakeStore()
		loose := f.addCap(testNS, "loose", 1, nil, `{"type":"object"}`)
		f.addNode("nd_loose", loose.ID, 1)
		d := New(f)

		out, err := d.Dispatch(context.Background(), sourceInput(loose.ID,
			map[string]any{"a": "{{x}}", "b": "{{y}}"}, nil))
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if len(out.Pruned) != 1 {
			t.Fatalf("一条目标只该落一条记录, got %v", out.Pruned)
		}
		detail := out.Pruned[0].Detail
		if !strings.Contains(detail, "；") || !strings.Contains(detail, "参数 a 取不到 {{x}}") ||
			!strings.Contains(detail, "参数 b 取不到 {{y}}") {
			t.Fatalf("detail = %q, want 两条 miss 用「；」连接", detail)
		}
	})
}

// TestParamPruneDetailExact param_missing / param_type 的 detail 是**整串**冻结的（契约 §8.1 A2）。
//
// 上面几个用例按子串断言正文，只能证明「病因写清楚了」；子串断言漏得掉整个
// `能力名@版本 ` 前缀——而前缀恰恰是扇出场景里唯一能回答「哪个目标」的字段
// （一条 trace 同时喂多个目标，各目标自己求值、各自可能剪枝）。
// 所以这里用**等值**断言把「前缀 + 冻结正文」一起钉死，改一个字就红。
func TestParamPruneDetailExact(t *testing.T) {
	cases := []struct {
		name     string
		capName  string
		schema   string
		template map[string]any
		payload  map[string]any
		reason   PruneReason
		want     string
	}{
		{
			name:     "param_missing：前缀 + 取不到 {{x}} 的正文",
			capName:  "loose",
			schema:   `{"type":"object"}`,
			template: map[string]any{"n": "{{x}}"},
			payload:  nil, // payload 里没有这个字段
			reason:   ReasonParamMissing,
			want:     "loose@1 参数 n 取不到 {{x}}（payload 里没有这个字段）",
		},
		{
			name:     "param_type：前缀 + 期望类型与实际值的正文",
			capName:  "strict",
			schema:   `{"type":"object","properties":{"lines":{"type":"integer"}}}`,
			template: map[string]any{"lines": "{{lines}}"},
			payload:  map[string]any{"lines": "abc"},
			reason:   ReasonParamType,
			want:     `strict@1 参数 lines 期望 integer，但求值得到 "abc"，转不了`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeStore()
			target := f.addCap(testNS, c.capName, 1, nil, c.schema)
			f.addNode("nd_target", target.ID, 1)
			d := New(f)

			out, err := d.Dispatch(context.Background(), sourceInput(target.ID, c.template, c.payload))
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if len(out.Pruned) != 1 {
				t.Fatalf("want 1 条剪枝记录, got %v", out.Pruned)
			}
			if got := out.Pruned[0].Reason; got != c.reason {
				t.Fatalf("reason = %q, want %q", got, c.reason)
			}
			if got := out.Pruned[0].Detail; got != c.want {
				t.Fatalf("detail 不是冻结文案：\n got  %q\n want %q", got, c.want)
			}
		})
	}
}

// TestDispatchDryRunPreviewUsesResolvedParams 试运行的预览参数是**解析后**的值。
//
// 用户改完模板点"试运行"，最想知道的就是 `{{n}}` 到底取到了什么；
// 预览里给回原文就等于什么都没说。
func TestDispatchDryRunPreviewUsesResolvedParams(t *testing.T) {
	f := newFakeStore()
	loose := f.addCap(testNS, "loose", 1, nil,
		`{"type":"object","properties":{"n":{"type":"integer"}}}`)
	f.addNode("nd_loose", loose.ID, 1)
	d := New(f)

	in := sourceInput(loose.ID, map[string]any{"n": "{{n}}"}, map[string]any{"n": float64(7)})
	in.DryRun = true
	out, err := d.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("want 1 preview task, got %d", len(out.Tasks))
	}
	if out.Tasks[0].Status != store.TaskPending {
		t.Errorf("预览任务状态 = %q, want pending", out.Tasks[0].Status)
	}
	if got := decodeTaskParams(t, out.Tasks[0]); got["n"] != float64(7) {
		t.Fatalf("预览参数 = %#v, want 解析后的 7", got)
	}
}

// TestParamDetailFormattersNilCap 能力定义缺失时 detail 只省前缀，不 panic。
//
// 剪枝留痕一定不能因为格式化细节把派发打断（它是诊断，不是业务）。
func TestParamDetailFormattersNilCap(t *testing.T) {
	miss := formatParamMissing(nil, []ParamMiss{{Field: "title", Ref: "{{x}}"}})
	if !strings.Contains(miss, "参数 title 取不到 {{x}}（payload 里没有这个字段）") {
		t.Fatalf("detail = %q", miss)
	}
	typ := formatParamType(nil, ParamTypeErr{Field: "n", Want: "integer", Got: "abc"})
	if !strings.Contains(typ, "参数 n 期望 integer，但求值得到 \"abc\"，转不了") {
		t.Fatalf("detail = %q", typ)
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

// TestDispatchUserScopedLookupErrorFailsClosed 查成员关系失败时**整个派发失败**。
//
// 这条守的是一个 fail-open 漏洞：checkUserScoped 以前只返回 (bool, PrunedRecord)，
// 注释写着「查库失败…向上抛」，但签名里根本没有 error —— 于是 IsOrgMember 出错时
// 等价于"校验通过"，消息会推给不属于本组织的 params.to。
//
// 三种处理里只有一种是对的：
//
//	· 当成剪枝 → 消息静默消失（用户以为发了）
//	· 当成通过 → 可能推给外组织用户（安全边界破了）
//	· 返回 error → 这次 trace 失败并留错，人能重试    ← 这个
func TestDispatchUserScopedLookupErrorFailsClosed(t *testing.T) {
	f := newFakeStore()
	f.errGetUser = errors.New("db is on fire")
	notify := f.addCap(testNS, "notify.phone", 1, nil, `{}`)
	f.addNode("nd_notify", notify.ID, 1)
	d := New(f)
	// 写明这条能力是 user-scoped：不依赖 DefaultUserScoped 的当前取值
	d.UserScoped = map[string]bool{"notify.phone": true}

	out, err := d.Dispatch(context.Background(), DispatchInput{
		NamespaceID: testNS, TraceID: "t_member_err",
		FromCap: "github", FromIsSource: true, Out: "push",
		Targets:      []string{notify.ID},
		TargetParams: map[string]map[string]any{notify.ID: {"to": "member-1"}},
	})
	if err == nil {
		t.Fatalf("查库失败必须返回 error（否则等于放行），out=%+v", out)
	}
	// 断言强度不变：仍然要求错误里带出「是哪一步、查的是谁」。
	// 文案随 #204 从 check org member 改成 check user（判据已是用户存在性）。
	for _, want := range []string{"check user", `"member-1"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误要说清是哪一步失败（缺 %q）, got %v", want, err)
		}
	}
	if out != nil && len(out.Tasks) != 0 {
		t.Fatalf("查库失败不该建出任务, got %d", len(out.Tasks))
	}
}
