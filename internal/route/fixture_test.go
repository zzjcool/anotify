package route

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

// 共享夹具：真实 store（临时库、真实 migration）+ 真实 artifact.Spec。
// 不造 fake store：路由的全部判定都要能对着真实 SQL 约束成立。

func testCtx() context.Context { return context.Background() }

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/route.db")
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testNamespace(t *testing.T, db *store.DB) *store.Namespace {
	t.Helper()
	ns, err := db.CreateNamespace(testCtx(), "default", "Default")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}
	return ns
}

type capOpts struct {
	Params string
	Next   []string
}

func testCapability(t *testing.T, db *store.DB, namespaceID, name string, version int, opts capOpts) *store.CapabilityVersion {
	t.Helper()
	params := opts.Params
	if params == "" {
		params = `{"type":"object"}`
	}
	spec := artifact.Spec{
		Params: json.RawMessage(params),
		Next:   append([]string{}, opts.Next...),
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
		},
	}
	capability := &store.CapabilityVersion{
		NamespaceID: namespaceID, Name: name, Version: version, Spec: spec,
	}
	if err := db.CreateCapability(testCtx(), capability); err != nil {
		t.Fatalf("建能力 %s: %v", name, err)
	}
	got, err := db.GetCapability(testCtx(), namespaceID, name, capability.Version)
	if err != nil {
		t.Fatalf("回读能力 %s: %v", name, err)
	}
	return got
}

func testNode(t *testing.T, db *store.DB) (*store.Node, string) {
	t.Helper()
	node := &store.Node{
		Name: "node-1", MaxInstances: 4, MaxConcurrency: 2,
		DeclaredNamespaces: []string{"default"},
	}
	token, err := db.CreateNode(testCtx(), node)
	if err != nil {
		t.Fatalf("建节点: %v", err)
	}
	stored, err := db.GetNodeByName(testCtx(), node.Name)
	if err != nil {
		t.Fatalf("回读节点: %v", err)
	}
	if err := db.UpdateNodeStatus(testCtx(), stored.ID, store.NodeStatusOnline); err != nil {
		t.Fatalf("节点上线: %v", err)
	}
	return stored, token
}

func testPendingTask(t *testing.T, db *store.DB, namespaceID, traceID string, capability *store.CapabilityVersion) *store.Task {
	t.Helper()
	task := &store.Task{
		NamespaceID: namespaceID, TraceID: traceID,
		CapabilityID: capability.ID, Capability: capability.Name,
		Version: capability.Version, Digest: capability.Digest,
		Params: json.RawMessage(`{}`),
	}
	if err := db.CreateTasks(testCtx(), []*store.Task{task}); err != nil {
		t.Fatalf("建任务: %v", err)
	}
	return task
}

// testRunningTask 走真实的 assign → grant（slot 0）得到一条 running 任务。
func testRunningTask(t *testing.T, db *store.DB, namespaceID, traceID string, capability *store.CapabilityVersion, node *store.Node, tokenHash string) (*store.Task, string, string) {
	t.Helper()
	task := testPendingTask(t, db, namespaceID, traceID, capability)
	if _, err := db.UpsertInstances(testCtx(), node.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("申报槽位: %v", err)
	}
	now := store.Now()
	if err := db.AssignTaskAt(testCtx(), task.ID, node.ID, "asg_route_1", 1, now); err != nil {
		t.Fatalf("记派发候选: %v", err)
	}
	leaseID, attemptToken, _, _, err := db.GrantLeaseAuthorized(testCtx(),
		store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: tokenHash},
		task.ID, "asg_route_1", 1, 0, now)
	if err != nil {
		t.Fatalf("授予租约: %v", err)
	}
	return task, leaseID, attemptToken
}
