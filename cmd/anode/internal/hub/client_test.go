package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/cmd/anode/internal/applied"
	"github.com/zzjcool/anotify/cmd/anode/internal/capability"
	"github.com/zzjcool/anotify/cmd/anode/internal/config"
	"github.com/zzjcool/anotify/cmd/anode/internal/hostenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/node"
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
