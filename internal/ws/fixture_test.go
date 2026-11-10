package ws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// 测试全部走真实的 httptest + WebSocket + 共享 protocol.Decode：不 mock 会话、
// 不手写 wire 字节。这样方向表、严格帧、presence 规则与生产是同一条路径。

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

type env struct {
	db        *store.DB
	ns        *store.Namespace
	node      *store.Node
	token     string
	tokenHash string
	hub       *Hub
	server    *httptest.Server
	now       int64
}

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/ws.db")
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db := testDB(t)
	ns, err := db.CreateNamespace(ctx, "default", "Default")
	if err != nil {
		t.Fatalf("建命名空间: %v", err)
	}
	node := &store.Node{Name: "node-1", MaxInstances: 4, MaxConcurrency: 2, DeclaredNamespaces: []string{"default"}}
	token, err := db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("建节点: %v", err)
	}
	stored, err := db.GetNodeByName(ctx, "node-1")
	if err != nil {
		t.Fatalf("回读节点: %v", err)
	}
	e := &env{
		db: db, ns: ns, node: stored, token: token,
		tokenHash: store.HashNodeToken(token),
		now:       time.Now().Unix(),
	}
	e.hub = NewHub(db, route.New(db),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithClock(func() int64 { return e.now }),
		WithHelloTimeout(2*time.Second),
		WithWriteTimeout(2*time.Second),
	)
	e.server = httptest.NewServer(http.HandlerFunc(e.hub.ServeHTTP))
	t.Cleanup(e.server.Close)
	return e
}

func (e *env) capability(t *testing.T, name, params string, idempotent bool, next []string) *store.CapabilityVersion {
	t.Helper()
	if params == "" {
		params = `{"type":"object"}`
	}
	spec := artifact.Spec{
		Params: json.RawMessage(params),
		Next:   append([]string{}, next...),
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
		},
		Lifecycle: artifact.Lifecycle{Idempotent: idempotent},
	}
	capability := &store.CapabilityVersion{NamespaceID: e.ns.ID, Name: name, Version: 1, Spec: spec}
	if err := e.db.CreateCapability(context.Background(), capability); err != nil {
		t.Fatalf("建能力 %s: %v", name, err)
	}
	got, err := e.db.GetCapability(context.Background(), e.ns.ID, name, capability.Version)
	if err != nil {
		t.Fatalf("回读能力: %v", err)
	}
	return got
}

func (e *env) pendingTask(t *testing.T, capability *store.CapabilityVersion) *store.Task {
	t.Helper()
	task := &store.Task{
		NamespaceID: e.ns.ID, TraceID: "trace-ws",
		CapabilityID: capability.ID, Capability: capability.Name,
		Version: capability.Version, Digest: capability.Digest,
		Params: json.RawMessage(`{}`),
	}
	if err := e.db.CreateTasks(context.Background(), []*store.Task{task}); err != nil {
		t.Fatalf("建任务: %v", err)
	}
	return task
}

// deployApplied 让能力在节点上处于「已部署且 applied」——这是派发候选的硬条件
// （pending 不是候选：字节还不一定在机器上）。
func (e *env) deployApplied(t *testing.T, capability *store.CapabilityVersion) {
	t.Helper()
	ctx := context.Background()
	rows, err := e.db.CreateDeployments(ctx, e.ns.ID, capability.ID, capability.Digest, []string{e.node.ID})
	if err != nil {
		t.Fatalf("建部署: %v", err)
	}
	for _, row := range rows {
		if err := e.db.MarkDeployResult(ctx, e.ns.ID, row.ID, store.DeploymentApplied, ""); err != nil {
			t.Fatalf("置部署 applied: %v", err)
		}
	}
}

// directGrant 用真实 assign → grant 造一条 running 任务（不走 dispatch 帧，便于
// 把测试焦点放在帧处理上）。slot 0 由 hello 申报的实例提供。
func (e *env) directGrant(t *testing.T, task *store.Task) (leaseID, attemptToken string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.db.UpsertInstances(ctx, e.node.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("申报槽位: %v", err)
	}
	if err := e.db.AssignTaskAt(ctx, task.ID, e.node.ID, "asg_direct", 1, e.now); err != nil {
		t.Fatalf("派发候选: %v", err)
	}
	leaseID, attemptToken, _, _, err := e.db.GrantLeaseAuthorized(ctx,
		store.NodeAuthorization{NodeID: e.node.ID, NodeTokenHash: e.tokenHash},
		task.ID, "asg_direct", 1, 0, e.now)
	if err != nil {
		t.Fatalf("授予租约: %v", err)
	}
	return leaseID, attemptToken
}

func (e *env) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	return e.dialNode(t, e.node.Name, e.token)
}

func (e *env) dialNode(t *testing.T, name, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.server.URL, "http") + "/v1/node/stream"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("拨号节点: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	e.send(t, conn, &protocol.HelloFrame{
		Type: protocol.FrameHello, Name: name, Token: token,
		Namespaces: []string{"default"},
		Instances:  []protocol.HelloInstance{{Slot: intPtr(0)}},
	})
	frame := e.read(t, conn)
	if _, ok := frame.(*protocol.WelcomeFrame); !ok {
		t.Fatalf("hello 后应收到 welcome，得到 %T", frame)
	}
	// welcome 在 handshake 里先于 attach 写出：等到注册表可见，避免用例与
	// ServeHTTP 的注册步骤竞争（生产无影响，测试需要确定性）。
	if !waitFor(t, time.Second, func() bool {
		node, err := e.db.GetNodeByName(context.Background(), name)
		return err == nil && e.hub.Online(node.ID)
	}) {
		t.Fatalf("节点 %s 未注册到 hub", name)
	}
	return conn
}

func (e *env) send(t *testing.T, conn *websocket.Conn, frame any) {
	t.Helper()
	data, err := protocol.Encode(frame)
	if err != nil {
		t.Fatalf("编码帧 %T: %v", frame, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("写帧 %T: %v", frame, err)
	}
}

func (e *env) read(t *testing.T, conn *websocket.Conn) any {
	t.Helper()
	frame, _, err := e.readRaw(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("读帧: %v", err)
	}
	return frame
}

func (e *env) readRaw(t *testing.T, conn *websocket.Conn, d time.Duration) (any, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	frame, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("解码帧: %v（%s）", err, data)
	}
	return frame, data, nil
}

func (e *env) task(t *testing.T, taskID string) *store.Task {
	t.Helper()
	task, err := e.db.GetTask(context.Background(), e.ns.ID, taskID)
	if err != nil {
		t.Fatalf("读任务 %s: %v", taskID, err)
	}
	return task
}

func (e *env) instances(t *testing.T) []*store.NodeInstance {
	t.Helper()
	rows, err := e.db.ListInstances(context.Background(), e.node.ID)
	if err != nil {
		t.Fatalf("列槽位: %v", err)
	}
	return rows
}

// waitFor 轮询一个条件（只用于「不该发生变化/最终收敛」的异步断言）。
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// dispatchOne 走完整的一轮派发并返回 dispatch 帧。
func (e *env) dispatchOne(t *testing.T, conn *websocket.Conn, capability *store.CapabilityVersion) (*store.Task, *protocol.TaskDispatchFrame) {
	t.Helper()
	e.deployApplied(t, capability)
	task := e.pendingTask(t, capability)
	if n, err := e.hub.DispatchOnce(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("DispatchOnce = %d, %v", n, err)
	}
	frame := e.read(t, conn)
	dispatch, ok := frame.(*protocol.TaskDispatchFrame)
	if !ok {
		t.Fatalf("应收到 task.dispatch，得到 %T", frame)
	}
	return task, dispatch
}
