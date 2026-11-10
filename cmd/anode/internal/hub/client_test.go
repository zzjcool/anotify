package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/cmd/anode/internal/applied"
	"github.com/zzjcool/anotify/cmd/anode/internal/capability"
	"github.com/zzjcool/anotify/cmd/anode/internal/clock"
	"github.com/zzjcool/anotify/cmd/anode/internal/config"
	"github.com/zzjcool/anotify/cmd/anode/internal/execenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/hostenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/node"
	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/cmd/anode/internal/runner"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

const testTimeout = 10 * time.Second

func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

func testNode(t *testing.T, serverURL, extraYAML string) *node.Node {
	t.Helper()
	yaml := "name: node-a\nserver: " + serverURL + "\ntoken: node-secret-token\nnamespaces: [default, team-a]\n" + extraYAML
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	dir := t.TempDir()
	binDir := t.TempDir()
	baseline := hostenv.CaptureWith(cfg.EnvPassthrough, func(key string) (string, bool) {
		switch key {
		case "PATH":
			return binDir, true
		case "HOME":
			return dir, true
		}
		return "", false
	})
	n, err := node.New(node.Options{
		Config:       cfg,
		Store:        applied.NewStore(dir),
		Cache:        capability.NewCache(filepath.Join(dir, "capabilities")),
		Baseline:     baseline,
		Facts:        protocol.NodeFacts{Hostname: "host-a", OS: "linux", Arch: "amd64", CPU: 2, HomeDir: dir},
		AgentVersion: "test",
		AgentCommit:  "c0ffee",
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return n
}

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

func acceptWS(t *testing.T, w http.ResponseWriter, r *http.Request) *websocket.Conn {
	t.Helper()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil
	}
	return conn
}

func welcome(revision string, nodeVars map[string]string, limits protocol.Limits, namespaces []string) protocol.WelcomeFrame {
	return protocol.WelcomeFrame{
		Type:           protocol.FrameWelcome,
		ConfigRevision: revision,
		NodeVars:       nodeVars,
		NodeLimits:     limits,
		Namespaces:     namespaces,
	}
}

func TestStreamURLDefaultAndExplicitPath(t *testing.T) {
	cases := map[string]string{
		"wss://hub.example":        "wss://hub.example/v1/node/stream",
		"wss://hub.example/":       "wss://hub.example/v1/node/stream",
		"wss://hub.example/custom": "wss://hub.example/custom",
		"ws://127.0.0.1:1/prefix":  "ws://127.0.0.1:1/prefix",
	}
	for in, want := range cases {
		got, err := StreamURL(in)
		if err != nil || got != want {
			t.Fatalf("StreamURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://hub", "wss:///path", ""} {
		if _, err := StreamURL(bad); err == nil {
			t.Fatalf("%q 应失败", bad)
		}
	}
}

func TestRunSendsHelloAndAppliesWelcome(t *testing.T) {
	helloCh := make(chan protocol.HelloFrame, 1)
	appliedCh := make(chan protocol.ConfigAppliedFrame, 1)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != StreamPath {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get(protocol.EmitAuthHeader)
		conn := acceptWS(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		frame, err := readFrame(ctx, conn)
		if err != nil {
			t.Logf("read hello: %v", err)
			return
		}
		hello, ok := frame.(*protocol.HelloFrame)
		if !ok {
			t.Logf("首帧不是 hello: %T", frame)
			return
		}
		helloCh <- *hello
		if err := writeFrame(ctx, conn, welcome("rev-1", map[string]string{"NODE_ONLY": "n"}, protocol.Limits{Total: 3}, []string{"*"})); err != nil {
			t.Logf("write welcome: %v", err)
			return
		}
		frame, err = readFrame(ctx, conn)
		if err != nil {
			t.Logf("read applied: %v", err)
			return
		}
		applied, ok := frame.(*protocol.ConfigAppliedFrame)
		if !ok {
			t.Logf("第二帧不是 config.applied: %T", frame)
			return
		}
		appliedCh <- *applied
		// 阻塞到测试取消（客户端断开）。
		_, _ = readFrame(ctx, conn)
	}))
	defer srv.Close()

	n := testNode(t, wsURL(srv.URL), "vars:\n  SOURCE_ROOT: /local\n")
	client, err := New(wsURL(srv.URL), "node-secret-token", n)
	if err != nil {
		t.Fatal(err)
	}
	client.Sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runCh := make(chan error, 1)
	go func() { runCh <- client.Run(ctx) }()

	var hello protocol.HelloFrame
	select {
	case hello = <-helloCh:
	case <-time.After(testTimeout):
		t.Fatal("未收到 hello")
	}
	if hello.Name != "node-a" || hello.Token != "node-secret-token" {
		t.Fatalf("hello = %+v", hello)
	}
	if hello.AppliedRevision != "" || hello.AppliedHash != "" {
		t.Fatalf("首连不应有 applied: %+v", hello)
	}
	if hello.LocalVars["SOURCE_ROOT"] != "/local" {
		t.Fatalf("LocalVars = %v", hello.LocalVars)
	}
	if hello.LocalLimits.Total != 0 {
		t.Fatalf("未写 local total 应发 0（protocol 里 0 = 未设置），实际 %d", hello.LocalLimits.Total)
	}
	if gotAuth != "Bearer node-secret-token" {
		t.Fatalf("WS 握手 Authorization = %q", gotAuth)
	}

	var applied protocol.ConfigAppliedFrame
	select {
	case applied = <-appliedCh:
	case <-time.After(testTimeout):
		t.Fatal("未收到 config.applied")
	}
	if applied.Revision != "rev-1" || applied.Hash == "" {
		t.Fatalf("config.applied = %+v", applied)
	}
	if applied.MachineVars["SOURCE_ROOT"] != "/local" || applied.MachineVars["NODE_ONLY"] != "n" {
		t.Fatalf("machineVars = %v", applied.MachineVars)
	}
	if rev, hash := n.AppliedState(); rev != "rev-1" || hash != applied.Hash {
		t.Fatalf("节点状态 = %q/%q", rev, hash)
	}

	cancel()
	select {
	case err := <-runCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run 返回 %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Run 未退出")
	}
}

func TestDialAuthFailureIsTerminal(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	n := testNode(t, wsURL(srv.URL), "")
	client, err := New(wsURL(srv.URL), "bad-token", n)
	if err != nil {
		t.Fatal(err)
	}
	var sleeps int32
	client.Sleep = func(ctx context.Context, d time.Duration) error {
		atomic.AddInt32(&sleeps, 1)
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	err = client.Run(ctx)
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("Run = %v, want ErrAuthRejected", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("认证失败不应重连，握手请求 %d 次", got)
	}
	if got := atomic.LoadInt32(&sleeps); got != 0 {
		t.Fatalf("认证失败不应退避重试，sleep %d 次", got)
	}
}

func TestConfigChangedTriggersConfigGet(t *testing.T) {
	appliedCh := make(chan protocol.ConfigAppliedFrame, 2)
	getCh := make(chan protocol.ConfigGetFrame, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != StreamPath {
			http.NotFound(w, r)
			return
		}
		conn := acceptWS(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		step := func() error {
			_, err := readFrame(ctx, conn)
			return err
		}
		if err := step(); err != nil { // hello
			t.Logf("hello: %v", err)
			return
		}
		if err := writeFrame(ctx, conn, welcome("rev-1", nil, protocol.Limits{Total: 1}, []string{"*"})); err != nil {
			t.Logf("welcome1: %v", err)
			return
		}
		frame, err := readFrame(ctx, conn)
		if err != nil {
			t.Logf("applied1: %v", err)
			return
		}
		if first, ok := frame.(*protocol.ConfigAppliedFrame); ok {
			appliedCh <- *first
		} else {
			t.Logf("第一帧不是 applied: %T", frame)
			return
		}
		if err := writeFrame(ctx, conn, protocol.ConfigChangedFrame{Type: protocol.FrameConfigChanged, Revision: "rev-2"}); err != nil {
			t.Logf("changed: %v", err)
			return
		}
		frame, err = readFrame(ctx, conn)
		if err != nil {
			t.Logf("config.get: %v", err)
			return
		}
		get, ok := frame.(*protocol.ConfigGetFrame)
		if !ok {
			t.Logf("不是 config.get: %T", frame)
			return
		}
		getCh <- *get
		if err := writeFrame(ctx, conn, welcome("rev-2", map[string]string{"NODE_ONLY": "two"}, protocol.Limits{Total: 1}, []string{"*"})); err != nil {
			t.Logf("welcome2: %v", err)
			return
		}
		frame, err = readFrame(ctx, conn)
		if err != nil {
			t.Logf("applied2: %v", err)
			return
		}
		if second, ok := frame.(*protocol.ConfigAppliedFrame); ok {
			appliedCh <- *second
		} else {
			t.Logf("第二帧不是 applied: %T", frame)
			return
		}
		_, _ = readFrame(ctx, conn)
	}))
	defer srv.Close()

	n := testNode(t, wsURL(srv.URL), "")
	client, _ := New(wsURL(srv.URL), "node-secret-token", n)
	client.Sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	first := <-appliedCh
	second := <-appliedCh
	if first.Revision != "rev-1" || second.Revision != "rev-2" {
		t.Fatalf("applied 顺序 = %q, %q", first.Revision, second.Revision)
	}
	if second.MachineVars["NODE_ONLY"] != "two" {
		t.Fatalf("第二次 apply machineVars = %v", second.MachineVars)
	}
	select {
	case get := <-getCh:
		if get.Type != protocol.FrameConfigGet || get.AppliedRevision != "rev-1" || get.AppliedHash != first.Hash {
			t.Fatalf("config.get = %+v, want rev-1/%s", get, first.Hash)
		}
	case <-time.After(testTimeout):
		t.Fatal("未收到 config.get")
	}
}

func TestReconnectResendsHelloWithAppliedState(t *testing.T) {
	hellos := make(chan protocol.HelloFrame, 4)
	var connCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != StreamPath {
			http.NotFound(w, r)
			return
		}
		idx := atomic.AddInt32(&connCount, 1)
		conn := acceptWS(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		frame, err := readFrame(ctx, conn)
		if err != nil {
			return
		}
		hello, ok := frame.(*protocol.HelloFrame)
		if !ok {
			return
		}
		hellos <- *hello
		if idx == 1 {
			if err := writeFrame(ctx, conn, welcome("rev-1", nil, protocol.Limits{Total: 1}, []string{"*"})); err != nil {
				return
			}
			if _, err := readFrame(ctx, conn); err != nil { // config.applied
				return
			}
			return // 关闭连接 → 客户端按退避重连
		}
		_, _ = readFrame(ctx, conn) // 第二次连接挂住到测试取消
	}))
	defer srv.Close()

	n := testNode(t, wsURL(srv.URL), "")
	client, _ := New(wsURL(srv.URL), "node-secret-token", n)
	client.Backoff = Backoff{Base: time.Millisecond, Max: 4 * time.Millisecond}
	var sleeps int32
	client.Sleep = func(ctx context.Context, d time.Duration) error {
		atomic.AddInt32(&sleeps, 1)
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runCh := make(chan error, 1)
	go func() { runCh <- client.Run(ctx) }()

	first := <-hellos
	if first.AppliedRevision != "" {
		t.Fatalf("首连 appliedRevision = %q", first.AppliedRevision)
	}
	select {
	case second := <-hellos:
		if second.AppliedRevision != "rev-1" || second.AppliedHash == "" {
			t.Fatalf("重连 hello 应带 applied: %+v", second)
		}
	case <-time.After(testTimeout):
		t.Fatal("未发生重连")
	}
	if atomic.LoadInt32(&connCount) < 2 {
		t.Fatalf("连接次数 = %d", connCount)
	}
	cancel()
	select {
	case <-runCh:
	case <-time.After(testTimeout):
		t.Fatal("Run 未退出")
	}
}

func TestReconnectBackoffBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	n := testNode(t, "ws://"+addr, "")
	client, err := New("ws://"+addr, "node-secret-token", n)
	if err != nil {
		t.Fatal(err)
	}
	client.Backoff = Backoff{Base: time.Millisecond, Max: 4 * time.Millisecond, Factor: 2}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	var delays []time.Duration
	client.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		count := len(delays)
		mu.Unlock()
		if count >= 6 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if err := client.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v", delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays[%d] = %s, want %s (all: %v)", i, delays[i], want[i], delays)
		}
	}
}

func testArtifact(t *testing.T, name string, version int) (*artifact.Artifact, string, []byte) {
	t.Helper()
	a := &artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata:   artifact.Metadata{Name: name, Version: version},
		Spec: artifact.Spec{
			Params: json.RawMessage(`{}`),
			Next:   []string{},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\ntrue\n"},
			},
			Requires: artifact.Requires{Commands: []string{}, Values: []string{}},
		},
	}
	data, err := artifact.MarshalYAML(a)
	if err != nil {
		t.Fatal(err)
	}
	parsed, digest, err := artifact.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return parsed, digest, data
}

func TestDeployFrameFetchesCachesAndAcks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   protocol.DeployStatus
	}{
		{"success", http.StatusOK, protocol.DeployStatusApplied},
		{"forbidden", http.StatusForbidden, protocol.DeployStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, digest, artifactData := testArtifact(t, "greeter", 3)
			ackCh := make(chan protocol.CapabilityDeployedFrame, 1)
			var getCount int32
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == StreamPath:
					conn := acceptWS(t, w, r)
					if conn == nil {
						return
					}
					defer conn.Close(websocket.StatusNormalClosure, "done")
					ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
					defer cancel()
					if _, err := readFrame(ctx, conn); err != nil { // hello
						t.Logf("hello: %v", err)
						return
					}
					if err := writeFrame(ctx, conn, welcome("rev-1", nil, protocol.Limits{Total: 1}, []string{"*"})); err != nil {
						t.Logf("welcome: %v", err)
						return
					}
					if _, err := readFrame(ctx, conn); err != nil { // config.applied
						t.Logf("applied: %v", err)
						return
					}
					deploy := protocol.CapabilityDeployFrame{
						Type:      protocol.FrameCapabilityDeploy,
						RequestID: "req-1",
						DeployRefs: protocol.DeployRefs{
							Namespace: "default", Name: "greeter", Version: 3, Digest: digest,
						},
					}
					if err := writeFrame(ctx, conn, deploy); err != nil {
						t.Logf("deploy: %v", err)
						return
					}
					frame, err := readFrame(ctx, conn)
					if err != nil {
						t.Logf("ack: %v", err)
						return
					}
					ack, ok := frame.(*protocol.CapabilityDeployedFrame)
					if !ok {
						t.Logf("不是 capability.deployed: %T", frame)
						return
					}
					ackCh <- *ack
					_, _ = readFrame(ctx, conn)
				case strings.HasPrefix(r.URL.Path, capability.ArtifactPathPrefix):
					atomic.AddInt32(&getCount, 1)
					if r.Header.Get(protocol.EmitAuthHeader) != "Bearer node-secret-token" {
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					if tc.status != http.StatusOK {
						http.Error(w, "denied", tc.status)
						return
					}
					_, _ = w.Write(artifactData)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			n := testNode(t, wsURL(srv.URL), "")
			client, _ := New(wsURL(srv.URL), "node-secret-token", n)
			client.Sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() { _ = client.Run(ctx) }()

			var ack protocol.CapabilityDeployedFrame
			select {
			case ack = <-ackCh:
			case <-time.After(testTimeout):
				t.Fatal("未收到 capability.deployed")
			}
			if ack.Status != tc.want {
				t.Fatalf("status = %s, want %s (error=%q)", ack.Status, tc.want, ack.Error)
			}
			if ack.RequestID != "req-1" || ack.Namespace != "default" || ack.Name != "greeter" ||
				ack.Version != 3 || ack.Digest != digest {
				t.Fatalf("回执 refs = %+v", ack)
			}
			if atomic.LoadInt32(&getCount) != 1 {
				t.Fatalf("GET 次数 = %d", getCount)
			}
			if tc.want == protocol.DeployStatusApplied {
				if !n.HasCapability(digest) {
					t.Fatal("部署成功后应已缓存")
				}
			} else if n.HasCapability(digest) {
				t.Fatal("部署失败不应缓存")
			}
			if tc.want == protocol.DeployStatusApplied && ack.Error != "" {
				t.Fatalf("成功回执不应有 error: %q", ack.Error)
			}
			if tc.want == protocol.DeployStatusFailed && ack.Error == "" {
				t.Fatal("失败回执必须有人话原因")
			}
		})
	}
}

// The wrapper signals entry into the real hub queue, not a substitute fake
// boundary. Production results must use the independent guarded interface.
type rework2HubQueue struct {
	client *Client
	queued chan struct{}
	arm    atomic.Bool
}

func (s *rework2HubQueue) signal(frame any) {
	if _, ok := frame.(*protocol.TaskResultFrame); ok && s.arm.CompareAndSwap(true, false) {
		close(s.queued)
	}
}
func (s *rework2HubQueue) Send(frame any) error { s.signal(frame); return s.client.Send(frame) }
func (s *rework2HubQueue) SendGuarded(ctx context.Context, frame any, guard func() bool) (bool, error) {
	s.signal(frame)
	return s.client.SendGuarded(ctx, frame, guard)
}

type rework2HubWallClock struct {
	*clock.Fake
	offset atomic.Int64
}

func (w *rework2HubWallClock) Now() time.Time {
	return w.Fake.Now().Add(time.Duration(w.offset.Load()))
}

type rework2HubHandler struct {
	Handler
	ExecHandler
}

func rework2Await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout: %s", what)
		var zero T
		return zero
	}
}

// Readiness must distinguish a shell's open(O_APPEND|O_CREATE) from its
// completed printf; the fake timeout may only start after the latter.
func rework2TimeoutScriptReady(marker string, pending int) (bool, []byte, error) {
	data, err := os.ReadFile(marker)
	return err == nil && string(data) == "once" && pending >= 2, data, err
}

func TestReliabilityTimeoutScriptReadyRequiresCompleteMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executions")
	for _, tc := range []struct {
		name    string
		data    string
		pending int
		ready   bool
	}{
		{"created_before_printf", "", 2, false},
		{"printf_done_before_timers", "once", 1, false},
		{"printf_and_timers_done", "once", 2, true},
		{"duplicate_execution", "onceonce", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(marker, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			ready, data, err := rework2TimeoutScriptReady(marker, tc.pending)
			if err != nil || ready != tc.ready {
				t.Fatalf("script readiness=%v want=%v marker=%q pending=%d err=%v", ready, tc.ready, data, tc.pending, err)
			}
		})
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if ready, _, err := rework2TimeoutScriptReady(marker, 2); ready || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing marker readiness=%v err=%v", ready, err)
	}
}

func TestRework2HubWriteQueueFencesRealRunnerResult(t *testing.T) {
	for _, revoke := range []string{"cancel", "expiryWithoutTimer", "pendingReplacement", "sessionGeneration", "timeoutValid", "timeoutCancel", "timeoutExpiry"} {
		t.Run(revoke, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			wall := &rework2HubWallClock{Fake: clock.NewFake(time.Now())}
			leaseAcks := make(chan struct{}, 8)
			timeoutCase := strings.HasPrefix(revoke, "timeout")
			peers := make(chan *websocket.Conn, 1)
			serverDone := make(chan struct{})
			results := make(chan *protocol.TaskResultFrame, 8)
			barriers := make(chan struct{}, 8)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				conn := acceptWS(t, w, req)
				if conn == nil {
					return
				}
				defer close(serverDone)
				defer conn.CloseNow()
				peers <- conn
				for {
					frame, err := readFrame(ctx, conn)
					if err != nil {
						return
					}
					if f, ok := frame.(*protocol.TaskResultFrame); ok {
						results <- f
					}
					if f, ok := frame.(*protocol.LeaseRenewFrame); ok {
						if err := writeFrame(ctx, conn, &protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: f.TaskID, Attempt: f.Attempt, LeaseID: f.LeaseID, ExpiresAt: wall.Now().Add(protocol.LeaseTTL)}); err != nil {
							return
						}
					}
					if f, ok := frame.(*protocol.ConfigGetFrame); ok && f.AppliedRevision == "barrier" {
						barriers <- struct{}{}
					}
				}
			}))
			defer srv.Close()
			conn, _, err := websocket.Dial(ctx, wsURL(srv.URL), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			peer := rework2Await(t, peers, "WS accept")
			n := testNode(t, wsURL(srv.URL), "")
			c, err := New(wsURL(srv.URL), "node-secret", n)
			if err != nil {
				t.Fatal(err)
			}
			c.setConn(conn)
			dir := t.TempDir()
			work := filepath.Join(dir, "work")
			a := &artifact.Artifact{APIVersion: "anotify/v1", Kind: "Capability", Metadata: artifact.Metadata{Name: "echo", Version: 1},
				Spec: artifact.Spec{Params: json.RawMessage(`{}`), Run: artifact.Run{Entrypoint: "run.sh", Files: map[string]string{"run.sh": "#!/bin/sh\necho real-result\n"}}}}
			if timeoutCase {
				a.Spec.Lifecycle.TimeoutSec = 2
				a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nprintf once >> executions\nsleep 30\n"
			}
			digest, err := artifact.Digest(a)
			if err != nil {
				t.Fatal(err)
			}
			r, err := runner.New(runner.Options{NodeName: "node-a", Clock: wall, EmitEndpoint: "http://127.0.0.1:1", WorkRoot: work, Results: resultstore.New(filepath.Join(dir, "results"), work),
				Baseline: hostenv.CaptureWith(nil, func(k string) (string, bool) {
					switch k {
					case "PATH":
						return "/usr/bin:/bin", true
					case "HOME":
						return dir, true
					}
					return "", false
				}),
				Applied: func() (string, map[string]string, protocol.Limits, bool) {
					return "rev-1", nil, protocol.Limits{Total: 2}, true
				},
				AllowsNamespace: func(string) bool { return true }, LoadArtifact: func(string) (*artifact.Artifact, error) { return a, nil }, Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			s := &rework2HubQueue{client: c, queued: make(chan struct{})}
			r.SetSender(s)
			c.Handler = rework2HubHandler{Handler: n, ExecHandler: r}
			readerDone := make(chan struct{})
			go func() {
				defer close(readerDone)
				for {
					frame, err := readFrame(ctx, conn)
					if err != nil {
						return
					}
					c.dispatch(ctx, conn, frame)
					if _, ok := frame.(*protocol.LeaseAckFrame); ok {
						leaseAcks <- struct{}{}
					}
				}
			}()
			// Cleanup always releases a held writer before Close/join; no bare read waits.
			held := false
			defer func() {
				if held {
					c.writeMu.Unlock()
				}
				cancel()
				conn.CloseNow()
				peer.CloseNow()
				rework2Await(t, readerDone, "WS ACK reader exit")
				rework2Await(t, serverDone, "WS receiver exit")
				closeCtx, cc := context.WithTimeout(context.Background(), 2*time.Second)
				defer cc()
				c.clearConn(conn)
				r.Drain(closeCtx)
				r.Close(closeCtx)
			}()
			start := func(assignment, lease string) {
				slot := 0
				dispatchDone := make(chan struct{})
				go func() {
					defer close(dispatchDone)
					r.HandleDispatch(&protocol.TaskDispatchFrame{Type: protocol.FrameTaskDispatch, TaskID: "wire", AssignmentID: assignment, ProposedAttempt: 1, Namespace: "default", Capability: protocol.CapabilityRef{Name: "echo", Version: 1, Digest: digest}, Params: json.RawMessage(`{}`), ConfigRevision: "rev-1", AssignmentExpiresAt: wall.Now().Add(30 * time.Second)})
				}()
				reserveDeadline := time.After(time.Second)
				for {
					instances := r.Instances()
					if len(instances) > 0 && instances[0].AssignmentID == assignment {
						break
					}
					select {
					case <-reserveDeadline:
						t.Fatal("dispatch did not reserve")
					case <-time.After(time.Millisecond):
					}
				}
				t.Cleanup(func() { rework2Await(t, dispatchDone, "dispatch writer exit") })
				r.HandleLeaseGrant(&protocol.LeaseGrantFrame{Type: protocol.FrameLeaseGrant, Node: "node-a", TaskID: "wire", Attempt: 1, AssignmentID: assignment, Slot: &slot, LeaseID: lease, AttemptToken: "token-" + lease, ExpiresAt: wall.Now().Add(protocol.LeaseTTL)})
			}
			start("old-assignment", "old-lease")
			if timeoutCase {
				until := time.After(time.Second)
				for {
					ready, data, err := rework2TimeoutScriptReady(filepath.Join(execenv.SystemWorkDir(work, "wire", 1), "executions"), wall.Fake.Pending())
					if ready {
						break
					}
					select {
					case <-until:
						t.Fatalf("real timeout script/timers not ready: marker=%q err=%v pending=%d", data, err, wall.Fake.Pending())
					case <-time.After(time.Millisecond):
					}
				}
				wall.Fake.Advance(2 * time.Second)
			}
			first := rework2Await(t, results, "real initial result")
			want := protocol.TaskPhaseSucceeded
			if timeoutCase {
				want = protocol.TaskPhaseFailed
			}
			if first.Phase != want || first.LeaseID != "old-lease" || (timeoutCase && (first.Result.Error == nil || first.Result.Error.Reason != protocol.ReasonTimeout)) {
				t.Fatalf("not a real success: %+v", first)
			}
			if timeoutCase {
				for range 3 {
					wall.Fake.Advance(25 * time.Second)
					timer := time.NewTimer(3 * time.Second)
					ticker := time.NewTicker(time.Millisecond)
				awaitAck:
					for {
						select {
						case <-leaseAcks:
							break awaitAck
						case <-ticker.C:
							wall.Fake.Advance(0)
						case <-timer.C:
							t.Fatal("real WS timeout renew/ACK did not continue")
						}
					}
					timer.Stop()
					ticker.Stop()
				}
				if wall.Now().Sub(first.FinishedAt) != 75*time.Second {
					t.Fatal("timeout finishedAt did not preserve real process completion")
				}
				if data, err := os.ReadFile(filepath.Join(execenv.SystemWorkDir(work, "wire", 1), "executions")); err != nil || string(data) != "once" {
					t.Fatalf("timeout script must execute exactly once: marker=%q err=%v task=%s attempt=%d lease=%s", data, err, first.TaskID, first.Attempt, first.LeaseID)
				}
				switch revoke {
				case "timeoutValid":
					revoke = "validTimeout"
				case "timeoutCancel":
					revoke = "cancel"
				case "timeoutExpiry":
					revoke = "expiryWithoutTimer"
				}
			}
			c.writeMu.Lock()
			held = true
			s.arm.Store(true)
			replayDone := make(chan struct{})
			go func() { defer close(replayDone); r.SessionUp() }()
			rework2Await(t, s.queued, "replay queued inside actual hub.writeMu")
			switch revoke {
			case "cancel":
				slot := 0
				r.HandleCancel(&protocol.TaskCancelFrame{Type: protocol.FrameTaskCancel, TaskID: "wire", Attempt: 1, LeaseID: "old-lease", Slot: &slot, Reason: protocol.CancelReasonUser})
				// Cancellation must be visible immediately, not rely on a cleanup timer.
			case "expiryWithoutTimer":
				wall.offset.Store(int64(61 * time.Second))
			case "pendingReplacement":
				// ACK of the observed first send, not a forged ACK of an unsent result.
				r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "wire", Attempt: 1, Accepted: true})
				start("new-assignment", "new-lease")
			case "sessionGeneration":
				r.SetSender(s)
			}
			c.writeMu.Unlock()
			held = false
			rework2Await(t, replayDone, "guarded replay exit")
			// A valid later frame acts as a WS barrier; ACK travels back through the real
			// socket/Client.dispatch. It must not deadlock on runner or attempt locks.
			barrier := &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet, AppliedRevision: "barrier"}
			if err := c.Send(barrier); err != nil {
				t.Fatal(err)
			}
			rework2Await(t, barriers, "WS barrier")
			if revoke == "validTimeout" {
				legal := rework2Await(t, results, "legal timeout replay after real hub queue")
				if legal.Phase != protocol.TaskPhaseFailed || legal.Result.Error == nil || legal.Result.Error.Reason != protocol.ReasonTimeout || !legal.FinishedAt.Equal(first.FinishedAt) {
					t.Fatalf("queued timeout lost legal failure/finishedAt: %+v", legal)
				}
				if err := writeFrame(ctx, peer, &protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "wire", Attempt: 1, Accepted: true}); err != nil {
					t.Fatal(err)
				}
			} else if revoke == "pendingReplacement" {
				legal := rework2Await(t, results, "replacement real result")
				if legal.LeaseID == "old-lease" {
					t.Error("queued old success crossed actual WS write boundary after pendingReplacement")
					legal = rework2Await(t, results, "new result after stale replay")
				}
				if legal.LeaseID != "new-lease" {
					t.Fatalf("replacement lost new binding: %+v", legal)
				}
				if err := writeFrame(ctx, peer, &protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "wire", Attempt: 1, Accepted: true}); err != nil {
					t.Fatal(err)
				}
			} else {
				select {
				case old := <-results:
					t.Errorf("queued old success crossed actual WS write boundary after %s: %+v", revoke, old)
				default:
				}
			}

			if revoke == "sessionGeneration" {
				r.SessionUp()
				legal := rework2Await(t, results, "new session legitimate replay")
				if legal.LeaseID != "old-lease" {
					t.Fatal("new session lost immutable binding")
				}
				if err := writeFrame(ctx, peer, &protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "wire", Attempt: 1, Accepted: true}); err != nil {
					t.Fatal(err)
				}
			}
			if revoke == "pendingReplacement" || revoke == "sessionGeneration" || revoke == "validTimeout" {
				deadline := time.After(time.Second)
				for r.ActiveTasks()["wire"] {
					select {
					case <-deadline:
						t.Fatal("real socket ACK did not finish legal result")
					case <-time.After(time.Millisecond):
					}
				}
			}
		})
	}
}

// Encoding is a deterministic checkpoint before SendGuarded waits on writeMu.
// It is test-only; the payload still goes through real protocol.Encode/WS Write.
type rework2EncodingCheckpoint struct {
	frame   any
	encoded chan struct{}
}

func (f rework2EncodingCheckpoint) MarshalJSON() ([]byte, error) {
	data, err := protocol.Encode(f.frame)
	close(f.encoded)
	return data, err
}

func TestRework2HubGuardRunsUnderWriteMuAndRejectsOldConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	peers := make(chan *websocket.Conn, 1)
	received := make(chan any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		peer := acceptWS(t, w, req)
		if peer == nil {
			return
		}
		defer peer.CloseNow()
		peers <- peer
		for {
			f, err := readFrame(ctx, peer)
			if err != nil {
				return
			}
			received <- f
		}
	}))
	defer srv.Close()
	conn, _, err := websocket.Dial(ctx, wsURL(srv.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	peer := rework2Await(t, peers, "real WS accept")
	defer peer.CloseNow()
	c, err := New(wsURL(srv.URL), "node-token", testNode(t, wsURL(srv.URL), ""))
	if err != nil {
		t.Fatal(err)
	}
	c.setConn(conn)
	frame := &protocol.TaskResultFrame{Type: protocol.FrameTaskResult, TaskID: "guard-boundary", Attempt: 1, Node: "node-a", LeaseID: "old-lease", Phase: protocol.TaskPhaseSucceeded}
	type outcome struct {
		sent bool
		err  error
	}
	for _, mode := range []string{"guardFalse", "oldConnection", "queuedContextExpiry"} {
		t.Run(mode, func(t *testing.T) {
			encoded := make(chan struct{})
			calls := atomic.Int32{}
			writeCtx, cc := context.WithCancel(ctx)
			defer cc()
			c.writeMu.Lock()
			held := true
			defer func() {
				if held {
					c.writeMu.Unlock()
				}
			}()
			done := make(chan outcome, 1)
			go func() {
				sent, err := c.SendGuarded(writeCtx, rework2EncodingCheckpoint{frame: frame, encoded: encoded}, func() bool { calls.Add(1); return false })
				done <- outcome{sent, err}
			}()
			rework2Await(t, encoded, "queued encode checkpoint")
			switch mode {
			case "oldConnection":
				// Execute exactly setConn's state update while the test owns writeMu.
				// A queued sender must not write against this later connection generation.
				c.generation.Add(1)
			case "queuedContextExpiry":
				cc()
			}
			if calls.Load() != 0 {
				t.Error("guard ran before owning actual hub.writeMu")
			}
			c.writeMu.Unlock()
			held = false
			got := rework2Await(t, done, "rejected actual write")
			if got.sent || got.err != nil {
				t.Fatalf("local rejection became sent/network failure: %+v", got)
			}
			want := int32(0)
			if mode == "guardFalse" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("guard calls=%d want=%d", calls.Load(), want)
			}
		})
	}
	// A legal guard must observe the mutex owned by SendGuarded. No other writer
	// exists here, so TryLock succeeding would prove a too-early guard invocation.
	sent, err := c.SendGuarded(ctx, frame, func() bool {
		if c.writeMu.TryLock() {
			c.writeMu.Unlock()
			t.Error("guard is outside hub write boundary")
			return false
		}
		return true
	})
	if !sent || err != nil {
		t.Fatalf("legal guarded send failed: sent=%v err=%v", sent, err)
	}
	got := rework2Await(t, received, "sole legal result on WS")
	if f, ok := got.(*protocol.TaskResultFrame); !ok || f.TaskID != frame.TaskID {
		t.Fatalf("unexpected WS frame: %#v", got)
	}
	if err := c.Send(&protocol.ConfigGetFrame{Type: protocol.FrameConfigGet, AppliedRevision: "barrier"}); err != nil {
		t.Fatal(err)
	}
	barrier := rework2Await(t, received, "WS barrier after legal result")
	if _, ok := barrier.(*protocol.ConfigGetFrame); !ok {
		t.Fatalf("rejected result wrote WS bytes: %#v", barrier)
	}
	c.clearConn(conn)
	sent, err = c.SendGuarded(ctx, frame, func() bool { t.Error("offline guard must not run"); return true })
	if sent || !errors.Is(err, ErrOffline) {
		t.Fatalf("offline network failure confused with guard rejection: sent=%v err=%v", sent, err)
	}
}

// Hold the real transport return while coder's write timeout is installed.
// Actual connection Close releases the gate, so deadline/Close tests exercise
// the library's cancellation path rather than a fake sender returning errors.
type lifecycleWriteConn struct {
	net.Conn
	entered, closed      chan struct{}
	armed                atomic.Bool
	enterOnce, closeOnce sync.Once
}

func (c *lifecycleWriteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && c.armed.Load() {
		c.enterOnce.Do(func() { close(c.entered) })
		select {
		case <-c.closed:
			return n, net.ErrClosed
		case <-time.After(15 * time.Second):
			return n, errors.New("bounded transport gate timed out")
		}
	}
	return n, err
}

func (c *lifecycleWriteConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestGuardedAdmittedWriteRetainsDeadlineCapAndConnectionClose(t *testing.T) {
	for _, mode := range []string{"callerDeadline", "original10sCap", "connectionClose"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			peers := make(chan *websocket.Conn, 1)
			serverDone := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				peer := acceptWS(t, w, req)
				if peer == nil {
					return
				}
				defer peer.CloseNow()
				peers <- peer
				<-serverDone
			}))
			defer srv.Close()
			defer close(serverDone)
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			var wire *lifecycleWriteConn
			dial := transport.DialContext
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := dial(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				wire = &lifecycleWriteConn{Conn: conn, entered: make(chan struct{}), closed: make(chan struct{})}
				return wire, nil
			}
			conn, _, err := websocket.Dial(ctx, wsURL(srv.URL), &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			peer := rework2Await(t, peers, "bounded write peer")
			defer peer.CloseNow()
			c := &Client{}
			c.setConn(conn)
			defer func() { conn.CloseNow(); c.clearConn(conn) }()
			caller, stopCaller := context.WithCancel(ctx)
			defer stopCaller()
			deadline := time.Now().Add(writeTimeout)
			if mode == "callerDeadline" {
				deadline = time.Now().Add(100 * time.Millisecond)
				var stopDeadline context.CancelFunc
				caller, stopDeadline = context.WithDeadline(caller, deadline)
				defer stopDeadline()
			}
			wire.armed.Store(true)
			type outcome struct {
				sent bool
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				sent, err := c.SendGuarded(caller, &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet}, func() bool { return true })
				done <- outcome{sent, err}
			}()
			rework2Await(t, wire.entered, "actual admitted transport write")
			// After admission, caller cleanup must not close the connection early.
			stopCaller()
			if mode == "connectionClose" {
				go conn.CloseNow()
			}
			select {
			case <-wire.closed:
				if mode != "connectionClose" && time.Now().Before(deadline) {
					t.Errorf("caller cancel closed admitted write before original deadline (%s)", mode)
				}
			case <-ctx.Done():
				t.Fatal("admitted write lost its original deadline/connection Close bound")
			}
			if got := rework2Await(t, done, "bounded write exit"); got.sent || got.err == nil {
				t.Fatalf("deadline/closed transport did not fail the write: %+v", got)
			}
		})
	}
}
