package v4test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 G1-4（task/emit/event/source/retention/deployment/stats）用的夹具。
// 与 helpers_test.go 分开是刻意的：那份文件是 G1-2/G1-3 的公共夹具，这里只放
// 本任务新增的形状，避免两个任务互相改对方的 helper。

// newNode 建一台 online 节点并申报 slots 个槽位（grant 需要真实空闲槽）。
//
// v4 容量 = 机器级 limits（预算）+ 真实 slot 记录：夹具把 limits.total 设为 slots 并
// 像真节点一样回报 applied，否则 grant 会因「配置尚未生效」被拒（A3）。
func newNode(t *testing.T, db *store.DB, name string, slots int) *store.Node {
	t.Helper()
	// 申报 "*"：节点不申报命名空间 = 谁都不授权（intersectNamespaces 的口径），
	// 而夹具里的任务需要一个真能跑的节点。
	n := &store.Node{Name: name, Status: store.NodeStatusOnline, DeclaredNamespaces: []string{"*"}}
	if _, err := db.CreateNode(context.Background(), n); err != nil {
		t.Fatalf("create node %q: %v", name, err)
	}
	specs := make([]store.InstanceSpec, 0, slots)
	for i := 0; i < slots; i++ {
		specs = append(specs, store.InstanceSpec{Slot: i})
	}
	if slots > 0 {
		if _, err := db.UpsertInstances(context.Background(), n.ID, specs); err != nil {
			t.Fatalf("upsert instances for %q: %v", name, err)
		}
		putNodeCfg(t, db, n.ID, []string{"*"}, nil, map[string]any{"total": slots})
		recordAppliedForNode(t, db, n.ID)
	}
	return n
}

// machineMergedForTest 复制 store 的两层合并口径（local 逐键压 node；limits 递归逐叶）。
// 它只用于测试侧拟真「节点自己算 appliedHash」；产品路径只调 store 的共享实现。
func machineMergedForTest(t *testing.T, db *store.DB, nodeID string) (map[string]string, map[string]any) {
	t.Helper()
	cfg, err := db.GetNodeConfig(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	vars := map[string]string{}
	for k, v := range cfg.Vars {
		vars[k] = v
	}
	for k, v := range cfg.LocalVars {
		vars[k] = v
	}
	var merge func(base, override map[string]any) map[string]any
	merge = func(base, override map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range override {
			if childOverride, ok := v.(map[string]any); ok {
				if childBase, ok := out[k].(map[string]any); ok {
					out[k] = merge(childBase, childOverride)
					continue
				}
			}
			out[k] = v
		}
		return out
	}
	return vars, merge(cfg.Limits, cfg.LocalLimits)
}

// recordAppliedForNode 让测试节点像真节点一样回报「当前机器配置已 apply」。
//
// hash 必须用与产品完全相同的共享实现（store.TypedMachineLimits →
// protocol.MachineConfigHash），否则测的根本不是同一份事实。
func recordAppliedForNode(t *testing.T, db *store.DB, nodeID string) {
	t.Helper()
	ctx := context.Background()
	cfg, err := db.GetNodeConfig(ctx, nodeID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	if cfg.Revision == 0 && cfg.AppliedRevision == 0 && cfg.AppliedHash == "" {
		return // 从未收到 config.changed：不需要 applied 事实
	}
	vars, limits := machineMergedForTest(t, db, nodeID)
	typed, terr := store.TypedMachineLimits(limits)
	if terr != nil {
		t.Fatalf("typed limits: %v", terr)
	}
	hash, herr := protocol.MachineConfigHash(vars, typed)
	if herr != nil {
		t.Fatalf("machine config hash: %v", herr)
	}
	if err := db.RecordApplied(ctx, nodeID, cfg.Revision, hash); err != nil {
		t.Fatalf("record applied: %v", err)
	}
}

// freeSlot 返回节点当前第一个真实空闲槽（模拟节点在 accept 前原子预留的那个 slot）。
func freeSlot(t *testing.T, db *store.DB, nodeID string) int {
	t.Helper()
	insts, err := db.ListInstances(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range insts {
		if in.Alive && in.BusyTask == nil {
			return in.Slot
		}
	}
	t.Fatalf("节点 %s 没有空闲 slot", nodeID)
	return -1
}

// idleObservations 把节点当前 busy 绑定变成「本机已停止该执行」的明确事实
// （模拟真节点在重连/重启后核对本机进程组：引用来自它自己记的 task/attempt/lease）。
func idleObservations(t *testing.T, db *store.DB, nodeID string, slots ...int) []store.SlotObservation {
	t.Helper()
	insts, err := db.ListInstances(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	want := map[int]bool{}
	for _, s := range slots {
		want[s] = true
	}
	out := []store.SlotObservation{}
	for _, in := range insts {
		if len(want) > 0 && !want[in.Slot] {
			continue
		}
		if in.BusyTask == nil {
			continue
		}
		noProcess := false
		out = append(out, store.SlotObservation{
			Slot: in.Slot, Busy: &noProcess,
			TaskID: *in.BusyTask, Attempt: in.BusyAttempt, LeaseID: in.BusyLeaseID,
		})
	}
	return out
}

// lifeCycleCap 建一个带 lifecycle 的能力（幂等 / 最大尝试次数可控）。
//
// C3a 起 grant 会执行能力 pinned 的 concurrency.maxPerNode（默认 1）。这一族夹具
// 验的是「一节点上两个 attempt 可以共存」的槽位/重排/回收语义，因此显式声明
// maxPerNode=2；默认 1 的拒绝语义由 execution_auth_test.go 的预算反例覆盖。
func lifeCycleCap(t *testing.T, db *store.DB, nsID, name string, idempotent bool, maxAttempts int) *store.CapabilityVersion {
	t.Helper()
	c := testCap(nsID, name, 1, nil)
	c.Spec.Lifecycle.Idempotent = idempotent
	if maxAttempts > 0 {
		c.Spec.Lifecycle.MaxAttempts = &maxAttempts
	}
	perNode := 2
	c.Spec.Concurrency.MaxPerNode = &perNode
	return createCap(t, db, c)
}

// newTask 建一条 pending 任务并回读（拿到 id/创建时间等权威值）。
func newTask(t *testing.T, db *store.DB, nsID string, c *store.CapabilityVersion, traceID, params string) *store.Task {
	t.Helper()
	if params == "" {
		params = "{}"
	}
	task := &store.Task{
		NamespaceID:  nsID,
		TraceID:      traceID,
		CapabilityID: c.ID,
		Capability:   c.Name,
		Version:      c.Version,
		Digest:       c.Digest,
		Params:       json.RawMessage(params),
	}
	if err := db.CreateTasks(context.Background(), []*store.Task{task}); err != nil {
		t.Fatalf("create task for %s: %v", c.Name, err)
	}
	got, err := db.GetTask(context.Background(), nsID, task.ID)
	if err != nil {
		t.Fatalf("read back task %s: %v", task.ID, err)
	}
	return got
}

// nodeAuthByID 把测试夹具里的节点行变成「已认证会话」形状（nodeId + 当前 token sha256）。
//
// C3a 起所有执行入口都要求 NodeAuthorization：夹具不绕过认证，只模拟认证层拿到的身份。
func nodeAuthByID(t *testing.T, db *store.DB, nodeID string) store.NodeAuthorization {
	t.Helper()
	n, err := db.GetNode(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("get node %s for auth: %v", nodeID, err)
	}
	return store.NodeAuthorization{NodeID: n.ID, NodeTokenHash: n.TokenHash}
}

// ackReapInput 从 attempt 行构造**精确历史绑定**（node/task/attempt/lease/slot/instance）。
func ackReapInput(t *testing.T, db *store.DB, nodeID, taskID string, attempt int) store.ReapedInputs {
	t.Helper()
	att, err := db.GetTaskAttempt(context.Background(), taskID, attempt)
	if err != nil {
		t.Fatalf("get attempt %s/%d: %v", taskID, attempt, err)
	}
	return store.ReapedInputs{
		TaskID: taskID, Attempt: attempt,
		NodeID: nodeID, NodeTokenHash: nodeAuthByID(t, db, nodeID).NodeTokenHash,
		LeaseID: att.LeaseID, Slot: att.Slot, InstanceID: att.InstanceID,
	}
}

// ackCancel 以认证身份 + 精确历史绑定提交 cancel.ack（reaped 是可信的 processGroupReaped 事实）。
func ackCancel(t *testing.T, db *store.DB, nodeID, taskID string, attempt int, reaped bool) error {
	t.Helper()
	in := ackReapInput(t, db, nodeID, taskID, attempt)
	in.Now = store.Now()
	return db.AckCancelAuthorized(context.Background(), nodeAuthByID(t, db, nodeID), in, reaped)
}

// grant 走完整派发：assign（记候选）→ grant（授租约，消费节点 accept 的真实 slot）。
func grant(t *testing.T, db *store.DB, taskID, nodeID, assignmentID string) (leaseID, token string, attempt int) {
	t.Helper()
	ctx := context.Background()
	slot := freeSlot(t, db, nodeID)
	if err := db.AssignTask(ctx, taskID, nodeID, assignmentID, 1); err != nil {
		t.Fatalf("assign task %s: %v", taskID, err)
	}
	leaseID, token, attempt, _, err := db.GrantLeaseAuthorized(ctx, nodeAuthByID(t, db, nodeID),
		taskID, assignmentID, 1, slot, store.Now())
	if err != nil {
		t.Fatalf("grant lease %s: %v", taskID, err)
	}
	return leaseID, token, attempt
}

// busySlots 数一台节点上被占用的槽位数（grant 占槽 / 终态释放的可观测证据）。
func busySlots(t *testing.T, db *store.DB, nodeID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM node_instances WHERE node_id = ? AND busy_task IS NOT NULL`,
		nodeID).Scan(&n); err != nil {
		t.Fatalf("count busy slots: %v", err)
	}
	return n
}

// mustPhase 断言任务相位。
func mustPhase(t *testing.T, db *store.DB, nsID, taskID, want string) *store.Task {
	t.Helper()
	got, err := db.GetTask(context.Background(), nsID, taskID)
	if err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	if got.Phase != want {
		t.Fatalf("task %s phase = %q, want %q (error=%q waiting=%q)", taskID, got.Phase, want, got.Error, got.WaitingReason)
	}
	return got
}

// eventTTL 把系统配置的事件保留期设成 sec（去重窗口就是它）。
func setEventTTL(t *testing.T, db *store.DB, sec int64) {
	t.Helper()
	ctx := context.Background()
	cfg, err := db.GetSystemConfig(ctx)
	if err != nil {
		t.Fatalf("get system config: %v", err)
	}
	cfg.RetentionEventTTLSec = sec
	cfg.RetentionTaskTTLSec = 0
	cfg.RetentionResultTTLSec = 0
	if err := db.PutSystemConfig(ctx, cfg, cfg.ResourceVersion); err != nil {
		t.Fatalf("put system config: %v", err)
	}
}

// setRetention 设置三条保留期。
func setRetention(t *testing.T, db *store.DB, eventSec, taskSec, resultSec int64) {
	t.Helper()
	ctx := context.Background()
	cfg, err := db.GetSystemConfig(ctx)
	if err != nil {
		t.Fatalf("get system config: %v", err)
	}
	cfg.RetentionEventTTLSec = eventSec
	cfg.RetentionTaskTTLSec = taskSec
	cfg.RetentionResultTTLSec = resultSec
	if err := db.PutSystemConfig(ctx, cfg, cfg.ResourceVersion); err != nil {
		t.Fatalf("put system config: %v", err)
	}
}

// mustJSON 断言一段 JSON 与期望串语义相同（键序无关）。
func mustJSON(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var got, expect any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode got json %s: %v", raw, err)
	}
	if err := json.Unmarshal([]byte(want), &expect); err != nil {
		t.Fatalf("decode want json %s: %v", want, err)
	}
	gb, _ := json.Marshal(got)
	eb, _ := json.Marshal(expect)
	if string(gb) != string(eb) {
		t.Fatalf("json mismatch:\n got: %s\nwant: %s", gb, eb)
	}
}

// pruneType 是测试里常用的剪枝码（param_missing）。
const pruneType = protocol.ReasonParamMissing

// setNamespaceVars 写一个命名空间的默认 vars（带当前 resourceVersion 做 CAS）。
// 返回写入后的配置（Revision 就是 namespaceVarsRevision）。
func setNamespaceVars(t *testing.T, db *store.DB, nsID string, vars map[string]string) *store.NamespaceConfig {
	t.Helper()
	ctx := context.Background()
	cur, err := db.GetNamespaceConfig(ctx, nsID)
	if err != nil {
		t.Fatalf("get namespace config: %v", err)
	}
	next, err := db.PutNamespaceConfig(ctx, nsID, vars, cur.ResourceVersion)
	if err != nil {
		t.Fatalf("put namespace config: %v", err)
	}
	return next
}

// helloNode 让节点上报一次 hello（本机 vars 快照 + 能力探测 + 申报 ns）。
//
// 本机 vars 只能由 hello 写（RecordHello 是唯一入口，D-I3），所以测试要用
// local 层值时必须先过一遍它。
func helloNode(t *testing.T, db *store.DB, node *store.Node, localVars map[string]string,
	caps []store.DeclaredCapability) {
	t.Helper()
	err := db.RecordHello(context.Background(), node.ID,
		store.NodeFacts{Hostname: node.Name}, caps, []string{"*"},
		localVars, nil, "test-agent", "test-commit")
	if err != nil {
		t.Fatalf("record hello for %s: %v", node.Name, err)
	}
	// hello 改了本机 vars：真节点会重算 appliedHash 并回报（apply 后才能接 grant）。
	recordAppliedForNode(t, db, node.ID)
}

// newSource 建一个源（空 targets 合法），返回带 id/反转义 config 的行。
func newSource(t *testing.T, db *store.DB, nsID, name, kind, cfg string) *store.Source {
	t.Helper()
	s := &store.Source{NamespaceID: nsID, Name: name, Kind: kind, Enabled: true}
	if cfg != "" {
		s.Config = json.RawMessage(cfg)
	}
	if err := db.CreateSource(context.Background(), s); err != nil {
		t.Fatalf("create source %q: %v", name, err)
	}
	return s
}

// taskByID 从一批任务里按 id 找一条（ListDispatchableTasks 之类的结果用）。
func taskByID(tasks []*store.Task, id string) *store.Task {
	for _, task := range tasks {
		if task.ID == id {
			return task
		}
	}
	return nil
}
