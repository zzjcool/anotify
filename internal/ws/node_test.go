package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 测试夹具：真 SQLite + 真 WS ----------

// nodeFixture 是一套跑起来的节点协议环境。
type nodeFixture struct {
	db      *store.DB
	orgID   string
	node    *store.Node
	token   string
	srv     *httptest.Server
	handler *NodeHandler
}

// newNodeFixture 建组织 + 节点 + 能力，并起一个真 HTTP server 挂 NodeHandler。
func newNodeFixture(t *testing.T, opts ...func(*NodeHandler)) *nodeFixture {
	t.Helper()
	ctx := context.Background()
	// store.Open 自己会加 file: 前缀与 PRAGMA 参数，这里只传纯路径。
	db, err := store.Open(t.TempDir() + "/node.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 组织 + 用户（组织是 org_id 的来源）
	u := &store.User{ID: store.NewUserID(), Username: "u-" + store.NewUserID(), CreatedAt: store.Now()}
	if err := db.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	org, err := db.CreateOrg(ctx, "org-"+store.NewUserID())
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := db.AddOrgMember(ctx, org.ID, u.ID, store.OrgRoleOwner); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// 能力定义（节点只能声明已定义的，不变量 I2）
	cap1 := &store.CapabilityVersion{
		OrgID: org.ID, Name: "review", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"fix"}, Source: store.CapSourceBuiltin,
	}
	if err := db.CreateCapability(ctx, cap1); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	// 节点
	node := &store.Node{OrgID: org.ID, Name: "mbp-" + store.NewUserID(), Tags: []string{"pool"}}
	token, err := db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	h := NewNodeHandler(db)
	for _, o := range opts {
		o(h)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/node/stream", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &nodeFixture{db: db, orgID: org.ID, node: node, token: token, srv: srv, handler: h}
}

// dial 建立一条节点 WS 连接（不带 token 时为裸连，用于鉴权测试）。
func (f *nodeFixture) dial(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/v1/node/stream"
	opts := &websocket.DialOptions{}
	if token != "" {
		opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + token}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url, opts)
	return conn, resp, err
}

// mustDial 建立连接并返回清理函数。
func (f *nodeFixture) mustDial(t *testing.T) *websocket.Conn {
	t.Helper()
	conn, resp, err := f.dial(t, f.token)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// readUntil 读到指定 type 的帧（跳过中间的 welcome 等）。
func readUntil(t *testing.T, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	for i := 0; i < 10; i++ {
		m, ok := tryReadFrame(conn, 2*time.Second)
		if !ok {
			t.Fatalf("timeout waiting for frame %q", want)
		}
		if m["type"] == want {
			return m
		}
	}
	t.Fatalf("did not see frame %q in 10 tries", want)
	return nil
}

// tryReadFrame 带超时读一帧；超时返回 (nil, false)。
func tryReadFrame(conn *websocket.Conn, d time.Duration) (map[string]any, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	return m, true
}

// send 发一帧。
func send(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// sendRaw 发送原始 JSON 文本（用于表达「字段缺失」这种 Marshal 表达不出的语义）。
func sendRaw(t *testing.T, conn *websocket.Conn, raw string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatalf("write raw frame: %v", err)
	}
}

// hello 发 hello 并等到 welcome。
func hello(t *testing.T, conn *websocket.Conn, nodeID string, caps []CapabilityDecl, insts []InstanceDecl) {
	t.Helper()
	send(t, conn, &HelloFrame{
		Type: NodeFrameHello, NodeID: nodeID, Protocol: ProtocolVersion,
		Capabilities: caps, Instances: insts,
	})
	welcome := readUntil(t, conn, NodeFrameWelcome)
	if welcome["heartbeat_sec"] == nil || welcome["lease_ttl_sec"] == nil {
		t.Fatalf("welcome missing heartbeat/lease config: %v", welcome)
	}
}

// ---------- #68：握手与鉴权 ----------

// TestUnauthorizedNoUpgrade 无 token / 假 token → 401/403，且不出现 HTTP 101（#68 验收）。
func TestUnauthorizedNoUpgrade(t *testing.T) {
	f := newNodeFixture(t)

	t.Run("no token → 401", func(t *testing.T) {
		conn, resp, err := f.dial(t, "")
		if err == nil {
			_ = conn.CloseNow()
			t.Fatal("dial without token succeeded")
		}
		if resp == nil {
			t.Fatal("no HTTP response")
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		// 关键断言：绝不是 101
		if resp.StatusCode == http.StatusSwitchingProtocols {
			t.Fatal("server upgraded without a token (auth must happen BEFORE Upgrade)")
		}
		// 也不该带 Upgrade 头
		if resp.Header.Get("Upgrade") != "" {
			t.Fatalf("unauthorized response has Upgrade header: %q", resp.Header.Get("Upgrade"))
		}
	})

	t.Run("bad token → 403", func(t *testing.T) {
		conn, resp, err := f.dial(t, "ant_node_totally_wrong_token")
		if err == nil {
			_ = conn.CloseNow()
			t.Fatal("dial with bad token succeeded")
		}
		if resp == nil {
			t.Fatal("no HTTP response")
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("draining node → 403", func(t *testing.T) {
		if err := f.db.UpdateNodeStatus(context.Background(), f.orgID, f.node.ID, store.NodeStatusDraining); err != nil {
			t.Fatalf("drain node: %v", err)
		}
		conn, resp, err := f.dial(t, f.token)
		if err == nil {
			_ = conn.CloseNow()
			t.Fatal("draining node was allowed to connect")
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", statusOf(resp))
		}
	})
}

// TestHandshake 握手成功：welcome 帧 + 节点上线 + 能力落库（#68 验收）。
func TestHandshake(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)

	hello(t, conn, f.node.ID,
		[]CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 4}},
	)

	// 节点应被标记为 online
	got, err := f.db.GetNode(context.Background(), f.orgID, f.node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status != store.NodeStatusOnline {
		t.Fatalf("node status = %q, want online", got.Status)
	}
	// 能力已落库
	caps, err := f.db.ListNodeCapabilities(context.Background(), f.node.ID)
	if err != nil {
		t.Fatalf("list node capabilities: %v", err)
	}
	if len(caps) != 1 || caps[0].Name != "review" {
		t.Fatalf("node capabilities = %v, want [review@1]", caps)
	}
	// 实例已落库
	insts, err := f.db.ListInstances(context.Background(), f.node.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(insts) != 1 || insts[0].Slot != 0 || insts[0].Concurrency != 4 {
		t.Fatalf("instances = %v, want [{0,4}]", insts)
	}
}

// TestHelloUnknownCapability 声明未定义能力 → error 帧 code=unknown_capability（#68 / A3）。
func TestHelloUnknownCapability(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)

	send(t, conn, &HelloFrame{
		Type: NodeFrameHello, NodeID: f.node.ID, Protocol: ProtocolVersion,
		Capabilities: []CapabilityDecl{{Capability: "does-not-exist", Version: 9}},
		Instances:    []InstanceDecl{{Slot: 0, Concurrency: 1}},
	})
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeUnknownCapability {
		t.Fatalf("code = %v, want unknown_capability", frame["code"])
	}
	// 节点不该上线
	got, _ := f.db.GetNode(context.Background(), f.orgID, f.node.ID)
	if got.Status == store.NodeStatusOnline {
		t.Fatal("node went online despite declaring an undefined capability")
	}
}

// TestHelloWrongNodeID token 与 hello 自报的 node_id 不一致 → 拒绝（防冒充）。
func TestHelloWrongNodeID(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)

	send(t, conn, &HelloFrame{
		Type: NodeFrameHello, NodeID: "nd_someone_else", Protocol: ProtocolVersion,
	})
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeBadFrame {
		t.Fatalf("code = %v, want bad_frame", frame["code"])
	}
}

// TestHelloInstances 超 max_instances → 拒绝注册（#68 验收）。
func TestHelloInstances(t *testing.T) {
	f := newNodeFixture(t)
	// 把节点的 max_instances 压到 1
	if _, err := f.db.Exec(`UPDATE nodes SET max_instances = 1 WHERE id = ?`, f.node.ID); err != nil {
		t.Fatalf("tighten max_instances: %v", err)
	}
	conn := f.mustDial(t)

	send(t, conn, &HelloFrame{
		Type: NodeFrameHello, NodeID: f.node.ID, Protocol: ProtocolVersion,
		Capabilities: []CapabilityDecl{{Capability: "review", Version: 1}},
		Instances:    []InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}},
	})
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeTooManyInstances {
		t.Fatalf("code = %v, want too_many_instances", frame["code"])
	}
}

// TestFramesBeforeHelloRejected 没 hello 就发任务帧 → 拒绝。
func TestFramesBeforeHelloRejected(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	// 不发 hello，直接发任务帧。
	// （服务端在 hello 之前不会下发 welcome——welcome 是「握手成功」的信号。）
	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: "tsk_x"})
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeBadFrame {
		t.Fatalf("code = %v, want bad_frame", frame["code"])
	}
}

// TestBearerTokenParsing Authorization 头解析的大小写与格式。
func TestBearerTokenParsing(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER abc", "abc"},
		{"Bearer  abc  ", "abc"},
		{"", ""},
		{"Basic abc", ""},
		{"Bearer", ""},
		{"Bearertoken", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/v1/node/stream", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		if got := BearerToken(r); got != c.want {
			t.Errorf("BearerToken(%q) = %q, want %q", c.header, got, c.want)
		}
	}
	// nil request 不 panic
	if got := BearerToken(nil); got != "" {
		t.Errorf("BearerToken(nil) = %q", got)
	}
}

// TestAuthErrorStatusMapping 鉴权错误 → HTTP 状态码。
func TestAuthErrorStatusMapping(t *testing.T) {
	cases := map[error]int{
		ErrMissingToken:  http.StatusUnauthorized,
		ErrBadToken:      http.StatusForbidden,
		ErrNodeDraining:  http.StatusForbidden,
		context.Canceled: http.StatusInternalServerError,
	}
	for err, want := range cases {
		if got := HTTPStatusForAuthError(err); got != want {
			t.Errorf("HTTPStatusForAuthError(%v) = %d, want %d", err, got, want)
		}
	}
}

// ---------- #69：九帧编解码 + result 三态 ----------

// TestFrameCodec 9 种帧各自编解码往返（#69 验收）。
func TestFrameCodec(t *testing.T) {
	t.Run("upstream", func(t *testing.T) {
		cases := []string{
			`{"type":"hello","node_id":"nd_x","protocol":1,"capabilities":[{"capability":"review","version":1}],"instances":[{"slot":0,"concurrency":4}]}`,
			`{"type":"heartbeat","ts":1730000000}`,
			`{"type":"ack","task_id":"tsk_x"}`,
			`{"type":"progress","task_id":"tsk_x","note":"step 2/5"}`,
			`{"type":"result","task_id":"tsk_x","status":"succeeded","out":"fix","summary":"ok","artifacts":["a"],"error":""}`,
		}
		wantTypes := []string{
			"*ws.HelloFrame", "*ws.HeartbeatFrame", "*ws.AckFrame",
			"*ws.ProgressFrame", "*ws.ResultFrame",
		}
		for i, raw := range cases {
			frame, err := DecodeUpstream([]byte(raw))
			if err != nil {
				t.Fatalf("case %d decode: %v", i, err)
			}
			gotType := typeName(frame)
			if gotType != wantTypes[i] {
				t.Fatalf("case %d decoded to %s, want %s", i, gotType, wantTypes[i])
			}
			// 往返：再编码再解码得到同样的类型与关键字段
			data, err := Encode(frame)
			if err != nil {
				t.Fatalf("case %d encode: %v", i, err)
			}
			again, err := DecodeUpstream(data)
			if err != nil {
				t.Fatalf("case %d re-decode: %v", i, err)
			}
			if typeName(again) != gotType {
				t.Fatalf("case %d round-trip changed type", i)
			}
		}
	})

	t.Run("downstream", func(t *testing.T) {
		frames := []any{
			&WelcomeFrame{Type: NodeFrameWelcome, NodeID: "nd_x", Protocol: 1, HeartbeatSec: 10, LeaseTTLSec: 30},
			&AssignFrame{Type: NodeFrameAssign, TaskID: "tsk_x", Capability: "review", Version: 1,
				Params: map[string]any{"diff": "..."}, LeaseUntil: 1730000030, MaxDuration: 3600},
			&CancelFrame{Type: NodeFrameCancel, TaskID: "tsk_x", Reason: "superseded"},
			&ErrorFrame{Type: NodeFrameError, Code: "unknown_capability", Message: "..."},
		}
		wantTypes := []string{"welcome", "assign", "cancel", "error"}
		for i, f := range frames {
			data, err := Encode(f)
			if err != nil {
				t.Fatalf("frame %d encode: %v", i, err)
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("frame %d decode: %v", i, err)
			}
			if m["type"] != wantTypes[i] {
				t.Fatalf("frame %d type = %v, want %q", i, m["type"], wantTypes[i])
			}
			if !IsDownstream(wantTypes[i]) {
				t.Fatalf("%q should be classified downstream", wantTypes[i])
			}
		}
	})

	t.Run("unknown type", func(t *testing.T) {
		frame, err := DecodeUpstream([]byte(`{"type":"bogus"}`))
		if err != nil {
			t.Fatalf("unknown type should not be an error: %v", err)
		}
		uf, ok := frame.(*UnknownFrame)
		if !ok {
			t.Fatalf("got %T, want *UnknownFrame", frame)
		}
		if uf.Type != "bogus" {
			t.Fatalf("type = %q", uf.Type)
		}
	})

	t.Run("bad json", func(t *testing.T) {
		if _, err := DecodeUpstream([]byte(`{not json`)); err == nil {
			t.Fatal("bad JSON decoded without error")
		}
	})
}

// TestResultTriState result 三态（#69 验收 / 不变量 I5）。
func TestResultTriState(t *testing.T) {
	t.Run("out 缺失 → 没回报", func(t *testing.T) {
		frame, err := DecodeUpstream([]byte(`{"type":"result","task_id":"tsk_x","status":"succeeded","summary":"done"}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		rf := frame.(*ResultFrame)
		if rf.Reported() {
			t.Fatal("missing out was treated as reported")
		}
		if rf.Out != nil {
			t.Fatalf("Out = %v, want nil", *rf.Out)
		}
	})

	t.Run("out:null → 明确到头", func(t *testing.T) {
		frame, err := DecodeUpstream([]byte(`{"type":"result","task_id":"tsk_x","status":"succeeded","out":null}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		rf := frame.(*ResultFrame)
		if !rf.Reported() {
			t.Fatal("explicit null out was NOT treated as reported (I5 violated)")
		}
		if rf.Out != nil {
			t.Fatalf("Out = %q, want nil", *rf.Out)
		}
	})

	t.Run("out:\"fix\" → 走 fix", func(t *testing.T) {
		frame, err := DecodeUpstream([]byte(`{"type":"result","task_id":"tsk_x","status":"failed","out":"fix"}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		rf := frame.(*ResultFrame)
		if !rf.Reported() {
			t.Fatal("out=fix was not treated as reported")
		}
		if rf.Out == nil || *rf.Out != "fix" {
			t.Fatalf("Out = %v, want \"fix\"", rf.Out)
		}
	})

	t.Run("out 空字符串也是回报", func(t *testing.T) {
		// '' 作为出口名不合法（store 会拒绝），但协议层要能如实解析出来
		frame, err := DecodeUpstream([]byte(`{"type":"result","task_id":"tsk_x","status":"succeeded","out":""}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		rf := frame.(*ResultFrame)
		if !rf.Reported() {
			t.Fatal("out=\"\" should count as present")
		}
		if rf.Out == nil || *rf.Out != "" {
			t.Fatal("out=\"\" not preserved")
		}
	})

	t.Run("encode 不泄漏 OutPresent", func(t *testing.T) {
		rf := &ResultFrame{Type: NodeFrameResult, TaskID: "t", Status: "succeeded", OutPresent: true}
		data, err := rf.MarshalJSON()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(data), "OutPresent") || strings.Contains(string(data), "out_present") {
			t.Fatalf("internal field leaked into JSON: %s", data)
		}
	})
}

// TestAssignFrameHasNoArgv 不变量 I1：assign 帧里没有 argv / shell / 脚本。
func TestAssignFrameHasNoArgv(t *testing.T) {
	f := &AssignFrame{
		Type: NodeFrameAssign, TaskID: "tsk_x", Capability: "review", Version: 1,
		Params: map[string]any{"diff": "..."}, LeaseUntil: 1, MaxDuration: 3600,
	}
	data, err := Encode(f)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	lower := strings.ToLower(string(data))
	for _, banned := range []string{"argv", "shell", "command", "script"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("assign frame contains banned token %q: %s", banned, data)
		}
	}
	// 字段集合就是契约里那 7 个
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(m) != 7 {
		t.Fatalf("assign frame has %d fields, want 7 (type/task_id/capability/version/params/lease_until/max_duration_sec): %v", len(m), m)
	}
}

// TestNoArgvInNodeProtocolSources 静态门禁：node_*.go 里不出现 argv/shell。
func TestNoArgvInNodeProtocolSources(t *testing.T) {
	for _, name := range []string{"node_protocol.go", "node_handshake.go", "node_handler.go", "node_session.go"} {
		src := readWSFile(t, name)
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue // 注释里说明「没有 argv」是正当的
			}
			lower := strings.ToLower(line)
			for _, banned := range []string{"argv", "\"shell\"", "exec.command"} {
				if strings.Contains(lower, banned) {
					t.Errorf("%s contains banned token %q: %s", name, banned, trimmed)
				}
			}
		}
	}
}

// ---------- #70：心跳与租约 ----------

// TestHeartbeatTimeout 停发心跳 → 判死后连接被关（#70 验收）。
func TestHeartbeatTimeout(t *testing.T) {
	// HeartbeatSec 是**秒**级配置（welcome 帧下发给节点），最小有效值是 1s。
	// 判死窗口 = 2×心跳 = 2s，测试等 5s 足够。
	f := newNodeFixture(t)
	f.handler.HeartbeatSec = 1
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	// 不发任何心跳。用一个足够长的 ctx 等待服务端关闭连接：
	// 判死窗口 = 2×心跳 = 2s，给 8s 余量。
	//
	// 注意不能反复用短超时 Read：客户端读超时会自己关闭连接，
	// 那会把「服务端判死」和「客户端超时」混为一谈。
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // 忽略中间可能到达的帧
		}
		// 关闭必须有明确 status（服务端用 StatusPolicyViolation）
		if websocket.CloseStatus(err) == websocket.StatusPolicyViolation {
			return // 正是契约 §3.3 要求的「>2×心跳判死」
		}
		if ctx.Err() != nil {
			t.Fatalf("connection was not closed after heartbeat timeout (waited 8s): %v", err)
		}
		// 其他错误（如 EOF）说明连接确实断了，但 status 不理想
		t.Logf("connection closed with unexpected status: %v", err)
		return
	}
}

// TestHeartbeatKeepsAlive 持续心跳 → 连接不被判死。
func TestHeartbeatKeepsAlive(t *testing.T) {
	f := newNodeFixture(t)
	f.handler.HeartbeatSec = 1 // 1s 心跳，2s 判死
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	// 连发 5 次心跳（每次间隔 200ms），跨过 2s 判死窗口
	for i := 0; i < 12; i++ {
		send(t, conn, &HeartbeatFrame{Type: NodeFrameHeartbeat, TS: time.Now().Unix()})
		time.Sleep(200 * time.Millisecond)
	}
	// 连接还活着：再发一个 ping 式心跳，然后读——如果已关闭会读到错误
	send(t, conn, &HeartbeatFrame{Type: NodeFrameHeartbeat, TS: time.Now().Unix()})
	// 心跳不产生下行帧，所以读应超时而不是收到 close
	_, _, err := conn.Read(contextTimeout(500 * time.Millisecond))
	if err != nil && websocket.CloseStatus(err) != -1 {
		t.Fatalf("connection closed despite heartbeats: %v", err)
	}
}

// TestLeaseExtended 心跳把 lease_until 推到 now+TTL（#70 验收）。
func TestLeaseExtended(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx := context.Background()
	// 建一个 assigned 到本节点的任务
	cap1, err := f.db.GetCapability(ctx, f.orgID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	task := &store.Task{
		OrgID: f.orgID, TraceID: "trace_lease", CapabilityID: cap1.ID,
		Capability: cap1.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: cap1.ContentHash, GraphID: "grf_x",
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	instID, err := f.db.LeaseInstance(ctx, f.node.ID, task.ID)
	if err != nil {
		t.Fatalf("lease instance: %v", err)
	}
	stale := store.Now() - 1000
	if err := f.db.AssignTask(ctx, f.orgID, task.ID, f.node.ID, instID, stale); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// 发心跳，租约应被推到 now+30
	send(t, conn, &HeartbeatFrame{Type: NodeFrameHeartbeat, TS: time.Now().Unix()})
	// 给服务端一点处理时间
	deadline := time.Now().Add(3 * time.Second)
	var lease int64
	for time.Now().Before(deadline) {
		got, err := f.db.GetTask(ctx, f.orgID, task.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.LeaseUntil != nil && *got.LeaseUntil > stale {
			lease = *got.LeaseUntil
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lease == 0 {
		t.Fatal("heartbeat did not extend the lease")
	}
	want := store.Now() + int64(store.LeaseTTLSec)
	if lease < want-5 || lease > want+5 {
		t.Fatalf("lease_until = %d, want ≈ %d (now+TTL)", lease, want)
	}
	// node.last_seen_at 也被刷新
	node, err := f.db.GetNode(ctx, f.orgID, f.node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if node.LastSeenAt == nil || *node.LastSeenAt == 0 {
		t.Fatal("heartbeat did not refresh last_seen_at")
	}
}

// TestDownstreamNoTouch 只发下行不发上行 → 仍然超时（#70 验收）。
//
// 服务端下行（welcome/assign/cancel）不续命，否则一条「对端不读」的死连接
// 会因为持续推送而被误判为健康。
func TestDownstreamNoTouch(t *testing.T) {
	f := newNodeFixture(t)
	f.handler.HeartbeatSec = 1 // 2s 判死
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	// 只读（不写），服务端不会主动发任何东西——模拟「下行活跃、上行静默」
	// 等超过判死窗口后连接必须被关。
	time.Sleep(2500 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("expected the connection to be closed after no upstream activity")
	}
	if websocket.CloseStatus(err) == -1 && !isTimeout(err) {
		t.Fatalf("connection state unclear: %v", err)
	}
}

// ---------- 任务帧推进（ack / progress / result） ----------

// TestAckProgressResultFlow 完整时序：assigned → acked → running → succeeded。
func TestAckProgressResultFlow(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx := context.Background()
	task := f.makeAssignedTask(t, "trace_flow")

	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.orgID, task.ID, store.TaskAcked)

	send(t, conn, &ProgressFrame{Type: NodeFrameProgress, TaskID: task.ID, Note: "step 2/5"})
	waitStatus(t, f.db, f.orgID, task.ID, store.TaskRunning)

	send(t, conn, &ResultFrame{Type: NodeFrameResult, TaskID: task.ID, Status: store.TaskSucceeded,
		Summary: "ok", Out: strPtr("fix")})
	waitStatus(t, f.db, f.orgID, task.ID, store.TaskSucceeded)

	got, err := f.db.GetTask(ctx, f.orgID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !got.Reported || got.Out == nil || *got.Out != "fix" {
		t.Fatalf("result not recorded: reported=%v out=%v", got.Reported, got.Out)
	}
	if got.Summary != "ok" {
		t.Fatalf("summary = %q", got.Summary)
	}
	// 槽位已释放
	insts, err := f.db.ListInstances(ctx, f.node.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	for _, in := range insts {
		if in.BusyTask != nil && *in.BusyTask == task.ID {
			t.Fatal("instance still busy after result")
		}
	}
}

// TestResultMissingOutLeavesUnreported 结果帧缺 out → reported=0（不伪造「到头」）。
func TestResultMissingOutLeavesUnreported(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_missing")
	send(t, conn, &ProgressFrame{Type: NodeFrameProgress, TaskID: task.ID})
	waitStatus(t, f.db, f.orgID, task.ID, store.TaskRunning)

	// 必须发**不带 out 键**的原始 JSON。
	// 不能用 ResultFrame 字面量：它的 MarshalJSON 总会写出 "out":null，
	// 那是「显式到头」而不是「没回报」——正是 I5 要区分的东西。
	sendRaw(t, conn, `{"type":"result","task_id":"`+task.ID+`","status":"succeeded","summary":"done"}`)
	waitStatus(t, f.db, f.orgID, task.ID, store.TaskSucceeded)

	got, err := f.db.GetTask(context.Background(), f.orgID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Reported {
		t.Fatal("missing out was recorded as reported (I5 violated)")
	}
	if got.Out != nil {
		t.Fatalf("out = %q, want NULL", *got.Out)
	}
}

// TestResultExplicitNullOutMeansEnd out:null → reported=1, out=NULL。
func TestResultExplicitNullOutMeansEnd(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_null")
	// 直接发 result（跳过 ack/progress，验证隐式 running）。
	// 显式 "out":null 与「缺 out 键」在线上 JSON 里不同，必须发原始文本才能表达。
	sendRaw(t, conn, `{"type":"result","task_id":"`+task.ID+`","status":"succeeded","out":null}`)
	waitStatus(t, f.db, f.orgID, task.ID, store.TaskSucceeded)

	got, err := f.db.GetTask(context.Background(), f.orgID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !got.Reported {
		t.Fatal("explicit null out was not recorded as reported")
	}
	if got.Out != nil {
		t.Fatalf("out = %q, want NULL", *got.Out)
	}
}

// TestAckOtherNodesTaskRejected 节点不能 ack 派给别人的任务。
func TestAckOtherNodesTaskRejected(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []CapabilityDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx := context.Background()
	cap1, _ := f.db.GetCapability(ctx, f.orgID, "review", 1)
	task := &store.Task{
		OrgID: f.orgID, TraceID: "trace_other_node", CapabilityID: cap1.ID,
		Capability: cap1.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: cap1.ContentHash, GraphID: "grf_x",
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 派给另一个节点
	otherNode := &store.Node{OrgID: f.orgID, Name: "other-" + store.NewUserID()}
	if _, err := f.db.CreateNode(ctx, otherNode); err != nil {
		t.Fatalf("create other node: %v", err)
	}
	if _, err := f.db.UpsertInstances(ctx, f.orgID, otherNode.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("instances: %v", err)
	}
	instID, err := f.db.LeaseInstance(ctx, otherNode.ID, task.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := f.db.AssignTask(ctx, f.orgID, task.ID, otherNode.ID, instID, store.Now()+60); err != nil {
		t.Fatalf("assign: %v", err)
	}

	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeInvalidTransition {
		t.Fatalf("code = %v, want invalid_transition", frame["code"])
	}
	// 状态没被改
	got, _ := f.db.GetTask(ctx, f.orgID, task.ID)
	if got.Status != store.TaskAssigned {
		t.Fatalf("task status = %q, want assigned", got.Status)
	}
}

// TestUnknownTypeKeepsConnection 未知帧类型 → error 帧但连接保持。
func TestUnknownTypeKeepsConnection(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"bogus"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeUnknownType {
		t.Fatalf("code = %v, want unknown_type", frame["code"])
	}
	// 连接仍然活着：再发心跳不报错
	send(t, conn, &HeartbeatFrame{Type: NodeFrameHeartbeat, TS: time.Now().Unix()})
}

// TestBadJSONKeepsConnection 坏 JSON → error 帧但连接保持。
func TestBadJSONKeepsConnection(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{not json`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeBadFrame {
		t.Fatalf("code = %v, want bad_frame", frame["code"])
	}
}

// TestDisconnectMarksNodeOffline 断开后节点离线、实例标记掉线。
func TestDisconnectMarksNodeOffline(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	// 确认在线
	got, _ := f.db.GetNode(context.Background(), f.orgID, f.node.ID)
	if got.Status != store.NodeStatusOnline {
		t.Fatalf("status = %q, want online", got.Status)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "bye")

	// 等 cleanup 生效
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := f.db.GetNode(context.Background(), f.orgID, f.node.ID)
		if err == nil && got.Status == store.NodeStatusOffline {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _ = f.db.GetNode(context.Background(), f.orgID, f.node.ID)
	if got.Status != store.NodeStatusOffline {
		t.Fatalf("status = %q after disconnect, want offline", got.Status)
	}
	insts, _ := f.db.ListInstances(context.Background(), f.node.ID)
	for _, in := range insts {
		if in.Alive {
			t.Fatal("instance still marked alive after disconnect")
		}
	}
}

// ---------- 测试辅助 ----------

// makeAssignedTask 建一个已派给本节点的任务。
func (f *nodeFixture) makeAssignedTask(t *testing.T, traceID string) *store.Task {
	t.Helper()
	ctx := context.Background()
	cap1, err := f.db.GetCapability(ctx, f.orgID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	task := &store.Task{
		OrgID: f.orgID, TraceID: traceID, CapabilityID: cap1.ID,
		Capability: cap1.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: cap1.ContentHash, GraphID: "grf_x",
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	instID, err := f.db.LeaseInstance(ctx, f.node.ID, task.ID)
	if err != nil {
		t.Fatalf("lease instance: %v", err)
	}
	if err := f.db.AssignTask(ctx, f.orgID, task.ID, f.node.ID, instID, store.Now()+store.AckWaitSec); err != nil {
		t.Fatalf("assign: %v", err)
	}
	return task
}

// waitStatus 等任务到达期望状态（异步处理，需要轮询）。
func waitStatus(t *testing.T, db *store.DB, orgID, taskID, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		got, err := db.GetTask(context.Background(), orgID, taskID)
		if err == nil {
			last = got.Status
			if last == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s status = %q, want %q (timeout)", taskID, last, want)
}

func strPtr(s string) *string { return &s }

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func contextTimeout(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	_ = cancel // 测试里不关心泄漏（进程内短暂）
	return ctx
}

func isTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "context deadline exceeded")
}

// readWSFile 读取包内某个源文件（静态门禁用）。
func readWSFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// typeName 返回具体类型名（用于断言解码结果）。
func typeName(v any) string {
	switch v.(type) {
	case *HelloFrame:
		return "*ws.HelloFrame"
	case *HeartbeatFrame:
		return "*ws.HeartbeatFrame"
	case *AckFrame:
		return "*ws.AckFrame"
	case *ProgressFrame:
		return "*ws.ProgressFrame"
	case *ResultFrame:
		return "*ws.ResultFrame"
	case *UnknownFrame:
		return "*ws.UnknownFrame"
	}
	return "unknown"
}
