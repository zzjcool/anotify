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

// newNode 建一台 online 节点并申报 slots 个槽位（grant 需要空闲槽）。
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
		specs = append(specs, store.InstanceSpec{Slot: i, Concurrency: 1})
	}
	if slots > 0 {
		if _, err := db.UpsertInstances(context.Background(), n.ID, specs); err != nil {
			t.Fatalf("upsert instances for %q: %v", name, err)
		}
	}
	return n
}

// lifeCycleCap 建一个带 lifecycle 的能力（幂等 / 最大尝试次数可控）。
func lifeCycleCap(t *testing.T, db *store.DB, nsID, name string, idempotent bool, maxAttempts int) *store.CapabilityVersion {
	t.Helper()
	c := testCap(nsID, name, 1, nil)
	c.Spec.Lifecycle.Idempotent = idempotent
	if maxAttempts > 0 {
		c.Spec.Lifecycle.MaxAttempts = &maxAttempts
	}
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

// grant 走完整派发：assign（记候选）→ grant（授租约）。
func grant(t *testing.T, db *store.DB, taskID, nodeID, assignmentID string) (leaseID, token string, attempt int) {
	t.Helper()
	ctx := context.Background()
	if err := db.AssignTask(ctx, taskID, nodeID, assignmentID, 1); err != nil {
		t.Fatalf("assign task %s: %v", taskID, err)
	}
	leaseID, token, attempt, _, err := db.GrantLease(ctx, taskID, assignmentID, nodeID, store.Now())
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
