package store

// v4_fixture_test.go 是 **in-package 测试的 v4 公共夹具**（G1-6 A 轮交付）。
//
// 为什么要一份夹具：v3 的旧测试用 `CapabilityVersion.ParamsSchema/Outputs/Lifecycle/
// Sticky` 这些派生字段 + `HandlerSpec(...)` 造能力、用 `Transition/StartTask/
// AckTask/LeaseInstance` 推任务。v4 里这些字段与入口整体删除（capability.md §2/§8、
// task.md A3）：
//
//   - 能力正文只有 `artifact.Spec`（params/next/run/requires/lifecycle/concurrency），
//     落库时由 store 归一化并算 foldKey/digest；
//   - 执行权只能经 `NodeAuthorization`（认证身份）+ assignment + 真实 slot 换租约，
//     attempt/slot/lease 是任务执行的事实。
//
// 本文件只做**测试夹具**：给迁移后的用例一份「真 Spec / 真节点 / 真 grant」的最小
// 构造器。它不碰生产路径、不加 alias、不提供任何绕过认证的钩子——
// `v4Auth` 拿的是节点行上真实的 token_hash（模拟认证层交出的身份），
// 与 v4test 外部夹具有完全相同的纪律。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// timeAtUnixNano 把纳秒时间戳还原成 UTC time（结果事实按纳秒保真，不借时钟）。
func timeAtUnixNano(ns int64) time.Time {
	return time.Unix(0, ns).UTC()
}

// v4Spec 造一份最小的合法产物正文（真实脚本正文，不是 handler 镜像）。
func v4Spec(params string, next []string, script string) artifact.Spec {
	if params == "" {
		params = `{"type":"object"}`
	}
	if script == "" {
		script = "#!/bin/sh\nexit 0\n"
	}
	return artifact.Spec{
		Params: json.RawMessage(params),
		Next:   append([]string{}, next...),
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": script},
		},
	}
}

// withSpec 给只有 name/version（或者带 params/next）的旧式夹具补一份**真实正文**。
//
// 它是 v3 `HandlerSpec(noop, …)` 的 v4 对应物：调用方显式写了 run 正文就原样保留
// （那条用例在验正文本身），否则补一份最小脚本——digest/foldKey 由 store 真实计算。
func withSpec(c *CapabilityVersion) *CapabilityVersion {
	if c == nil {
		return nil
	}
	if c.Spec.Run.Entrypoint != "" || len(c.Spec.Run.Files) > 0 {
		return c
	}
	spec := v4Spec(string(c.Spec.Params), c.Spec.Next, "")
	spec.Requires = c.Spec.Requires
	spec.Lifecycle = c.Spec.Lifecycle
	spec.Concurrency = c.Spec.Concurrency
	c.Spec = spec
	return c
}

// v4Cap 造一个带真实正文的能力行并落库（失败即 Fatal）。
//
// version>0 = 显式版本（同版内容不可变，CAS 走 ResourceVersion）；0 = 内容寻址
// （store 按 foldKey 复用或分配下一个版本）。
func v4Cap(t *testing.T, db *DB, nsID, name string, version int, next ...string) *CapabilityVersion {
	t.Helper()
	c := withSpec(&CapabilityVersion{NamespaceID: nsID, Name: name, Version: version, Spec: v4Spec("", next, "")})
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability %s@%d: %v", name, version, err)
	}
	return c
}

// v4CapWith 落库一份调用方拼好的能力行（保留 Spec 里的 lifecycle/concurrency 等）。
func v4CapWith(t *testing.T, db *DB, c *CapabilityVersion) *CapabilityVersion {
	t.Helper()
	c = withSpec(c)
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability %s@%d: %v", c.Name, c.Version, err)
	}
	return c
}

// putV4NodeCfg 写节点中枢侧默认值（namespaces/vars/limits），保留 local 覆盖。
func putV4NodeCfg(t *testing.T, db *DB, nodeID string, namespaces []string, vars map[string]string, limits map[string]any) *NodeConfig {
	t.Helper()
	cfg, err := db.GetNodeConfig(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	cfg.Namespaces = namespaces
	cfg.Vars = vars
	cfg.Limits = limits
	if err := db.PutNodeConfig(context.Background(), cfg); err != nil {
		t.Fatalf("put node config: %v", err)
	}
	return cfg
}

// v4MachineMerged 复刻 store 的两层合并（local 压 node；limits 逐叶），
// 只用于让夹具像真节点一样算 appliedHash——产品路径只走 store 的共享实现。
func v4MachineMerged(t *testing.T, db *DB, nodeID string) (map[string]string, map[string]any) {
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

// v4RecordApplied 让夹具节点回报「这份机器配置已 apply」（grant 的前置事实）。
func v4RecordApplied(t *testing.T, db *DB, nodeID string) {
	t.Helper()
	cfg, err := db.GetNodeConfig(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	if cfg.Revision == 0 && cfg.AppliedRevision == 0 && cfg.AppliedHash == "" {
		return
	}
	vars, limits := v4MachineMerged(t, db, nodeID)
	typed, terr := TypedMachineLimits(limits)
	if terr != nil {
		t.Fatalf("typed machine limits: %v", terr)
	}
	hash, herr := protocol.MachineConfigHash(vars, typed)
	if herr != nil {
		t.Fatalf("machine config hash: %v", herr)
	}
	if err := db.RecordApplied(context.Background(), nodeID, cfg.Revision, hash); err != nil {
		t.Fatalf("record applied: %v", err)
	}
}

// v4Node 建一台 online 节点并申报 slots 个真实槽位（grant 必须有空闲 slot）。
func v4Node(t *testing.T, db *DB, name string, slots int) *Node {
	t.Helper()
	n := &Node{Name: name, Status: NodeStatusOnline, DeclaredNamespaces: []string{"*"}}
	if _, err := db.CreateNode(context.Background(), n); err != nil {
		t.Fatalf("create node %q: %v", name, err)
	}
	if slots > 0 {
		specs := make([]InstanceSpec, 0, slots)
		for i := 0; i < slots; i++ {
			specs = append(specs, InstanceSpec{Slot: i})
		}
		if _, err := db.UpsertInstances(context.Background(), n.ID, specs); err != nil {
			t.Fatalf("upsert instances for %q: %v", name, err)
		}
		putV4NodeCfg(t, db, n.ID, []string{"*"}, nil, map[string]any{"total": slots})
		v4RecordApplied(t, db, n.ID)
	}
	return n
}

// v4Auth 把节点行变成「已认证会话」形状（nodeId + 当前 token sha256）。
func v4Auth(t *testing.T, db *DB, nodeID string) NodeAuthorization {
	t.Helper()
	n, err := db.GetNode(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("get node %s for auth: %v", nodeID, err)
	}
	return NodeAuthorization{NodeID: n.ID, NodeTokenHash: n.TokenHash}
}

// v4Task 建一条 pending 任务并回读权威行。
func v4Task(t *testing.T, db *DB, nsID string, c *CapabilityVersion, traceID, params string) *Task {
	t.Helper()
	if params == "" {
		params = "{}"
	}
	task := &Task{
		NamespaceID:  nsID,
		TraceID:      traceID,
		CapabilityID: c.ID,
		Capability:   c.Name,
		Version:      c.Version,
		Digest:       c.Digest,
		Params:       json.RawMessage(params),
	}
	if err := db.CreateTasks(context.Background(), []*Task{task}); err != nil {
		t.Fatalf("create task for %s: %v", c.Name, err)
	}
	got, err := db.GetTask(context.Background(), nsID, task.ID)
	if err != nil {
		t.Fatalf("read back task %s: %v", task.ID, err)
	}
	return got
}

// v4FreeSlot 返回节点当前第一个真实空闲槽。
func v4FreeSlot(t *testing.T, db *DB, nodeID string) int {
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

// v4BusySlots 数节点当前被占用的槽位。
func v4BusySlots(t *testing.T, db *DB, nodeID string) int {
	t.Helper()
	insts, err := db.ListInstances(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	n := 0
	for _, in := range insts {
		if in.BusyTask != nil {
			n++
		}
	}
	return n
}

// v4Grant 走完整派发：assign（记候选）→ grant（认证身份 + 真实 slot 换租约）。
func v4Grant(t *testing.T, db *DB, taskID, nodeID, assignmentID string) (leaseID, attemptToken string, attempt int) {
	t.Helper()
	ctx := context.Background()
	slot := v4FreeSlot(t, db, nodeID)
	if err := db.AssignTask(ctx, taskID, nodeID, assignmentID, 1); err != nil {
		t.Fatalf("assign task %s: %v", taskID, err)
	}
	leaseID, attemptToken, attempt, _, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, nodeID),
		taskID, assignmentID, 1, slot, Now())
	if err != nil {
		t.Fatalf("grant lease %s: %v", taskID, err)
	}
	return leaseID, attemptToken, attempt
}

// v4MustPhase 回读任务并要求相位（失败即 Fatal）。
func v4MustPhase(t *testing.T, db *DB, nsID, taskID, want string) *Task {
	t.Helper()
	got, err := db.GetTask(context.Background(), nsID, taskID)
	if err != nil {
		t.Fatalf("get task %s: %v", taskID, err)
	}
	if got.Phase != want {
		t.Fatalf("task %s phase = %q, want %q", taskID, got.Phase, want)
	}
	return got
}

// v4Event 造一条可插入的事件（trace_id 必填，由夹具生成）。
func v4Event(nsID, source, typ string) *Event {
	return &Event{NamespaceID: nsID, TraceID: "tr_" + NewEventID(), Source: source, Type: typ}
}

// v4InsertEvent 插事件并要求「真的插入了」（windowSec 是这个源的去重窗口）。
func v4InsertEvent(t *testing.T, db *DB, e *Event, now, windowSec int64) *Event {
	t.Helper()
	inserted, err := db.InsertEvent(context.Background(), e, now, windowSec)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if !inserted {
		t.Fatalf("insert event %s: 期望新插入，实际被判为重复投递", e.Type)
	}
	return e
}

// v4SetRetention 写系统配置里的三档保留期（0 = 永久）。
func v4SetRetention(t *testing.T, db *DB, eventSec, taskSec, resultSec int64) {
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

// mkSeqTaskBatch 批量建 n 条任务（统计/保留期夹具：关心行数与 trace 分组，
// 不关心单条任务的派发事实）。每行独立建，保持各自 id/created_at 的顺序。
func mkSeqTaskBatch(t *testing.T, db *DB, nsID, traceID string, c *CapabilityVersion, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		task := mkTask(nsID, traceID, c)
		if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
			t.Fatalf("create task %d/%d: %v", i, n, err)
		}
	}
}
