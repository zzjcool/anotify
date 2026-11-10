package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/cmd/anode/internal/capability"
	"github.com/zzjcool/anotify/cmd/anode/internal/config"
	"github.com/zzjcool/anotify/cmd/anode/internal/hub"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

const testTimeout = 10 * time.Second

func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

func writeFrame(ctx context.Context, conn *websocket.Conn, frame any) error {
	data, err := protocol.Encode(frame)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func readFrame(ctx context.Context, conn *websocket.Conn) (any, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return protocol.Decode(data)
}

func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, pred func(any) bool) any {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	for {
		frame, err := readFrame(deadline, conn)
		if err != nil {
			t.Fatalf("读帧失败: %v", err)
		}
		if pred(frame) {
			return frame
		}
	}
}

func testArtifact(t *testing.T) (*artifact.Artifact, string) {
	t.Helper()
	a := &artifact.Artifact{
		APIVersion: "anotify/v1",
		Kind:       "Capability",
		Metadata:   artifact.Metadata{Name: "echo", Version: 1},
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Next:   []string{"fixer"},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\necho hello-from-script\n"},
			},
			Lifecycle:   artifact.Lifecycle{ResultTTLSec: intPtr(0)},
			Concurrency: artifact.Concurrency{MaxPerNode: intPtr(1)},
		},
	}
	if err := artifact.Validate(a); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	digest, err := artifact.Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	return a, digest
}

func intPtr(v int) *int { return &v }

// nodeChainTrace observes the real transport without changing WS reads, writes,
// deadlines, or reconnect policy. Only metadata/call stacks, never frame bodies
// or credential headers, are recorded. It is scoped to the full-chain fixture.
type nodeChainTrace struct {
	mu          sync.Mutex
	events      []string
	runReturned bool
	runErr      error
	runCtx      context.Context
}

func (d *nodeChainTrace) record(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	for _, secret := range []string{"node-token", "tok-1"} {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, message)
}

func (d *nodeChainTrace) snapshot() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fmt.Sprintf("Run returned=%v err=%v ctx.Err=%v\n%s", d.runReturned, d.runErr, d.runCtx.Err(), strings.Join(d.events, "\n"))
}

func (d *nodeChainTrace) readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, id, request string, pred func(any) bool) any {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	d.record("conn=%s await=%s", id, request)
	for {
		frame, err := readFrame(deadline, conn)
		if err != nil {
			d.record("conn=%s await=%s read error=%v closeStatus=%d readCtx.Err=%v", id, request, err, websocket.CloseStatus(err), deadline.Err())
			t.Fatalf("读帧失败 conn=%s request=%s: %v\n%s", id, request, err, d.snapshot())
		}
		if pred(frame) {
			d.record("conn=%s received=%s frame=%T", id, request, frame)
			return frame
		}
	}
}

type nodeChainConn struct {
	net.Conn
	trace        *nodeChainTrace
	id           string // client local addr = server request RemoteAddr
	readError    sync.Once
	writeError   sync.Once
	closeStarted sync.Once
	gate         *nodeChainResultWriteGate
}

func (c *nodeChainConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.readError.Do(func() { c.trace.record("conn=%s first transport read error=%v", c.id, err) })
	}
	return n, err
}

func (c *nodeChainConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && c.gate != nil && c.gate.isResult(p) {
		close(c.gate.entered) // Real result bytes delivered; coder Write has not returned.
		select {
		case <-c.gate.release:
		case <-time.After(testTimeout):
			return n, errors.New("result transport return barrier timed out")
		}
	}
	if err != nil {
		c.writeError.Do(func() { c.trace.record("conn=%s first transport write error=%v", c.id, err) })
	}
	return n, err
}

func (c *nodeChainConn) Close() error {
	c.closeStarted.Do(func() {
		c.trace.record("conn=%s first transport Close caller:\n%s", c.id, debug.Stack())
		if c.gate != nil {
			c.gate.closedOnce.Do(func() { close(c.gate.closed) })
		}
	})
	return c.Conn.Close()
}

// TestNodeFullChainColdStartDispatchGrantResultLogsAndReconnect 是真实端到端：
// 假中枢 WS ↔ 真节点进程（真 spawn / 真 lease / 真日志 / 真上传端点）。
func TestNodeFullChainColdStartDispatchGrantResultLogsAndReconnect(t *testing.T) {
	testNodeFullChain(t, nil)
}

// This gate holds the actual transport Flush after a complete, masked result
// frame is on the wire, not an artificial delay before sending or after Write.
type nodeChainResultWriteGate struct {
	entered, release, closed chan struct{}
	resultCtx                chan context.Context
	used                     atomic.Bool
	releaseOnce, closedOnce  sync.Once
}

func (g *nodeChainResultWriteGate) unblock() { g.releaseOnce.Do(func() { close(g.release) }) }

func (g *nodeChainResultWriteGate) isResult(p []byte) bool {
	if len(p) < 6 || p[0] != 0x81 || p[1]&0x80 == 0 {
		return false
	}
	n, offset := int(p[1]&0x7f), 2
	if n == 126 {
		if len(p) < 8 {
			return false
		}
		n, offset = int(p[2])<<8|int(p[3]), 4
	}
	if n == 127 || len(p) != offset+4+n {
		return false
	}
	data := make([]byte, n)
	for i := range data {
		data[i] = p[offset+4+i] ^ p[offset+i%4]
	}
	f, err := protocol.Decode(data)
	_, result := f.(*protocol.TaskResultFrame)
	return err == nil && result && g.used.CompareAndSwap(false, true)
}

// Observe the real runner context without changing its cancellation/deadline.
type nodeChainResultSender struct {
	client *hub.Client
	gate   *nodeChainResultWriteGate
}

func (s nodeChainResultSender) Send(frame any) error { return s.client.Send(frame) }
func (s nodeChainResultSender) SendGuarded(ctx context.Context, frame any, guard func() bool) (bool, error) {
	s.gate.resultCtx <- ctx
	return s.client.SendGuarded(ctx, frame, guard)
}

func TestNodeResultACKDuringTransportWriteKeepsSharedStream(t *testing.T) {
	gate := &nodeChainResultWriteGate{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}), resultCtx: make(chan context.Context, 4)}
	testNodeFullChain(t, gate)
}

func testNodeFullChain(t *testing.T, gate *nodeChainResultWriteGate) {
	t.Helper()
	if gate != nil {
		t.Cleanup(gate.unblock)
		defer gate.unblock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	trace := &nodeChainTrace{runCtx: ctx}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("full-chain final diagnostic:\n%s", trace.snapshot())
		}
	})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialContext := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialContext(ctx, network, address)
		if err != nil {
			trace.record("transport dial error=%v", err)
			return nil, err
		}
		id := conn.LocalAddr().String()
		trace.record("conn=%s transport dial connected", id)
		return &nodeChainConn{Conn: conn, trace: trace, id: id, gate: gate}, nil
	}
	t.Cleanup(transport.CloseIdleConnections)
	t.Cleanup(cancel)
	a, digest := testArtifact(t)
	dataDir := t.TempDir()
	cache := capability.NewCache(filepath.Join(dataDir, "capabilities"))
	if _, err := cache.Store(a, digest); err != nil {
		t.Fatalf("cache.Store: %v", err)
	}

	type accepted struct {
		conn *websocket.Conn
		ctx  context.Context
		id   string
	}
	conns := make(chan accepted, 4)
	// handler 必须活到测试结束：net/http 在 ServeHTTP 返回后会结束/回收连接。
	handlerDone := make(chan struct{})
	t.Cleanup(func() { close(handlerDone) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// 读用独立 ctx：HTTP handler 返回后 r.Context() 会被取消。
		trace.record("conn=%s server accepted", r.RemoteAddr)
		conns <- accepted{conn: conn, ctx: context.Background(), id: r.RemoteAddr}
		<-handlerDone
		trace.record("conn=%s server Close reason=server done", r.RemoteAddr)
		_ = conn.Close(websocket.StatusNormalClosure, "server done")
	}))
	t.Cleanup(srv.Close)

	cfg, err := config.Parse([]byte("name: node-a\nserver: " + wsURL(srv.URL) + "\ntoken: node-token\nnamespaces: [default]\n"))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	nodeApp, err := Assemble(Options{
		Config: cfg, DataDir: dataDir, Version: "test", Commit: "c0ffee",
		Logf: trace.record,
		Dial: func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
			copyOpts := *opts
			copyOpts.HTTPClient = &http.Client{Transport: transport}
			return websocket.Dial(ctx, url, &copyOpts)
		},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if gate != nil {
		nodeApp.runner.SetSender(nodeChainResultSender{client: nodeApp.client, gate: gate})
	}
	runErr := make(chan error, 1)
	runDone := make(chan struct{})
	go func() {
		err := nodeApp.Run(ctx)
		trace.mu.Lock()
		trace.runReturned, trace.runErr = true, err
		trace.mu.Unlock()
		trace.record("App.Run returned err=%v ctx.Err=%v", err, ctx.Err())
		runErr <- err
		close(runDone)
	}()
	// Join even after Fatal: a failed read must not leak the real App into the
	// next -count iteration. This is cleanup, not a reconnect/read workaround.
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(testTimeout):
			t.Errorf("Run cleanup 未退出：%s", trace.snapshot())
		}
	})

	var conn *websocket.Conn
	var connCtx context.Context
	var connID string
	select {
	case c := <-conns:
		conn, connCtx, connID = c.conn, c.ctx, c.id
	case err := <-runErr:
		t.Fatalf("节点 Run 提前退出：%v", err)
	case <-time.After(testTimeout):
		t.Fatal("节点没有连上来")
	}
	t.Cleanup(func() {
		trace.record("conn=%s test cleanup Close reason=done", connID)
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	})

	// 1) hello（冷启动）：没有 applied、没有 instances。
	hello := trace.readUntil(t, connCtx, conn, connID, "cold hello", func(f any) bool {
		_, ok := f.(*protocol.HelloFrame)
		return ok
	}).(*protocol.HelloFrame)
	if hello.Name != "node-a" || hello.Token != "node-token" || hello.AppliedRevision != "" {
		t.Fatalf("hello 不对：%+v", hello)
	}
	if len(hello.Instances) != 0 {
		t.Fatalf("冷启动不能伪造槽位：%+v", hello.Instances)
	}

	// 2) 冷启动收到 dispatch：只能 stale_config，且不执行。
	if err := writeFrame(connCtx, conn, &protocol.TaskDispatchFrame{
		Type: protocol.FrameTaskDispatch, TaskID: "t1", AssignmentID: "asg-1",
		ProposedAttempt: 1, Namespace: "default",
		Capability:     protocol.CapabilityRef{Name: "echo", Version: 1, Digest: digest},
		Params:         json.RawMessage(`{}`),
		ConfigRevision: "r1", AssignmentExpiresAt: time.Now().Add(30 * time.Second),
	}); err != nil {
		t.Fatalf("write dispatch: %v", err)
	}
	rej := trace.readUntil(t, connCtx, conn, connID, "t1 stale_config", func(f any) bool {
		r, ok := f.(*protocol.TaskRejectFrame)
		return ok && r.TaskID == "t1"
	}).(*protocol.TaskRejectFrame)
	if rej.Reason != protocol.ReasonStaleConfig {
		t.Fatalf("冷启动必须 stale_config：%q", rej.Reason)
	}

	// 3) welcome：节点 applied 并回报 config.applied（含真实 instances 快照）。
	if err := writeFrame(connCtx, conn, &protocol.WelcomeFrame{
		Type: protocol.FrameWelcome, ConfigRevision: "r1",
		NodeVars:   map[string]string{},
		NodeLimits: protocol.Limits{Total: 2, PerCapability: map[string]int{"echo": 1}},
		Namespaces: []string{"default"},
	}); err != nil {
		t.Fatalf("write welcome: %v", err)
	}
	applied := trace.readUntil(t, connCtx, conn, connID, "r1 config.applied", func(f any) bool {
		_, ok := f.(*protocol.ConfigAppliedFrame)
		return ok
	}).(*protocol.ConfigAppliedFrame)
	if applied.Revision != "r1" || len(applied.Instances) != 2 {
		t.Fatalf("config.applied 不对：%+v", applied)
	}
	for i, inst := range applied.Instances {
		if inst.Slot == nil || *inst.Slot != i || inst.Busy == nil || *inst.Busy {
			t.Fatalf("instances 快照不对：%+v", inst)
		}
	}

	// 4) dispatch → accept（带 slot 0 presence）。
	if err := writeFrame(connCtx, conn, &protocol.TaskDispatchFrame{
		Type: protocol.FrameTaskDispatch, TaskID: "t1", AssignmentID: "asg-1",
		ProposedAttempt: 1, Namespace: "default",
		Capability:     protocol.CapabilityRef{Name: "echo", Version: 1, Digest: digest},
		Params:         json.RawMessage(`{}`),
		ConfigRevision: "r1", AssignmentExpiresAt: time.Now().Add(30 * time.Second),
	}); err != nil {
		t.Fatalf("write dispatch: %v", err)
	}
	acc := trace.readUntil(t, connCtx, conn, connID, "t1 task.accept", func(f any) bool {
		x, ok := f.(*protocol.TaskAcceptFrame)
		return ok && x.TaskID == "t1"
	}).(*protocol.TaskAcceptFrame)
	if acc.Slot == nil || *acc.Slot != 0 {
		t.Fatalf("accept 必须回 slot 0：%+v", acc)
	}

	// 5) grant（回同一 slot/assignment）→ 真执行 → task.result。
	if err := writeFrame(connCtx, conn, &protocol.LeaseGrantFrame{
		Type: protocol.FrameLeaseGrant, TaskID: "t1", Attempt: 1, Node: "node-a",
		LeaseID: "lease-1", AttemptToken: "tok-1",
		ExpiresAt:    time.Now().Add(protocol.LeaseTTL),
		AssignmentID: "asg-1", Slot: acc.Slot,
	}); err != nil {
		t.Fatalf("write grant: %v", err)
	}
	res := trace.readUntil(t, connCtx, conn, connID, "t1 attempt=1 lease-1 task.result", func(f any) bool {
		x, ok := f.(*protocol.TaskResultFrame)
		return ok && x.TaskID == "t1"
	}).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseSucceeded || res.Attempt != 1 || res.LeaseID != "lease-1" {
		t.Fatalf("result 不对：%+v", res)
	}
	if res.Result.Artifacts == nil {
		t.Fatal("artifacts 必须是数组（非 nil）")
	}

	var resultCtx context.Context
	if gate != nil {
		select {
		case <-gate.entered:
		case <-time.After(testTimeout):
			t.Fatal("complete result transport frame did not enter return barrier")
		}
		select {
		case resultCtx = <-gate.resultCtx:
		case <-time.After(testTimeout):
			t.Fatal("real runner result context was not captured")
		}
		if resultCtx.Err() != nil {
			t.Fatalf("result context ended before trusted ACK: %v", resultCtx.Err())
		}
	}

	// Result submission alone does not free a lease-dependent slot. This real
	// receiver explicitly confirms the result before asserting reconnect idle.
	if err := writeFrame(connCtx, conn, &protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: res.TaskID, Attempt: res.Attempt, Accepted: true}); err != nil {
		t.Fatalf("write authoritative result ACK: %v", err)
	}

	if gate != nil {
		select {
		case <-resultCtx.Done():
		case <-time.After(testTimeout):
			t.Fatal("trusted result ACK did not immediately cancel normal upload lifecycle")
		}
		if resultCtx.Err() != context.Canceled || ctx.Err() != nil {
			t.Fatalf("wrong cancel source: result=%v Run=%v", resultCtx.Err(), ctx.Err())
		}
		t.Log("real TaskResult delivered, trusted ACK cancelled runner upload context while transport Write return is held; Run ctx remains live")
		// A bounded negative-event observation, not a sleep to find the race:
		// the frame-return barrier remains held until cancellation closes the
		// transport (old implementation) or this assertion window ends.
		select {
		case <-gate.closed:
			t.Errorf("normal result ACK cancelled the shared WS during active Write:\n%s", trace.snapshot())
		case <-time.After(250 * time.Millisecond):
		}
		gate.unblock()
	}

	// 6) logs_request.attempt=1 → logs_response 必须回实际 attempt + stdout。
	if err := writeFrame(connCtx, conn, &protocol.LogsRequestFrame{
		Type: protocol.FrameLogsRequest, TaskID: "t1", RequestID: "lr-1", Attempt: 1,
	}); err != nil {
		t.Fatalf("write logs_request: %v", err)
	}
	logs := trace.readUntil(t, connCtx, conn, connID, "t1 attempt=1 lr-1 logs_response", func(f any) bool {
		x, ok := f.(*protocol.LogsResponseFrame)
		return ok && x.RequestID == "lr-1"
	}).(*protocol.LogsResponseFrame)
	if logs.Error != "" || logs.Attempt != 1 || !strings.Contains(logs.Stdout, "hello-from-script") {
		t.Fatalf("logs_response 不对：%+v", logs)
	}

	if gate != nil {
		// Verify another subsystem on the exact same stream, before deliberate
		// reconnect; reconnect cannot conceal an ACK-induced stream failure.
		if err := writeFrame(connCtx, conn, &protocol.ConfigChangedFrame{Type: protocol.FrameConfigChanged}); err != nil {
			t.Fatalf("write same-stream config.changed: %v", err)
		}
		trace.readUntil(t, connCtx, conn, connID, "same-stream config.get after ACK and logs", func(f any) bool {
			_, ok := f.(*protocol.ConfigGetFrame)
			return ok
		})
	}

	// 7) 断线重连：重新 hello 必须带 appliedRevision + instances（不假空）。
	trace.record("conn=%s test Close reason=reconnect (logs_response already verified)", connID)
	if err := conn.Close(websocket.StatusNormalClosure, "reconnect"); err != nil {
		trace.record("conn=%s reconnect Close error=%v", connID, err)
	}
	var conn2 *websocket.Conn
	var conn2Ctx context.Context
	var conn2ID string
	select {
	case c := <-conns:
		conn2, conn2Ctx, conn2ID = c.conn, c.ctx, c.id
	case <-time.After(testTimeout):
		t.Fatalf("节点没有重连：%s", trace.snapshot())
	}
	t.Cleanup(func() {
		trace.record("conn=%s test cleanup Close reason=done", conn2ID)
		_ = conn2.Close(websocket.StatusNormalClosure, "done")
	})
	hello2 := trace.readUntil(t, conn2Ctx, conn2, conn2ID, "reconnect hello", func(f any) bool {
		_, ok := f.(*protocol.HelloFrame)
		return ok
	}).(*protocol.HelloFrame)
	if hello2.AppliedRevision != "r1" || len(hello2.Instances) != 2 {
		t.Fatalf("重连 hello 必须带 applied + instances：%+v", hello2)
	}
	for _, inst := range hello2.Instances {
		if inst.Busy == nil || *inst.Busy {
			t.Fatalf("重连时槽位应空闲（上一次已回收）：%+v", inst)
		}
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run 退出错误：%v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Run 未退出")
	}
	// 用户数据目录里没有遗留的固定导出目录名（A2 明确拒绝固定目录约定）。
	if _, err := os.Stat(filepath.Join(dataDir, ".anotify-artifacts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("不能创建固定 .anotify-artifacts 目录：%v", err)
	}
}
