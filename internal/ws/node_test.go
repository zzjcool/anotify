package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 测试夹具：真 SQLite + 真 WS ----------

// nodeFixture 是一套跑起来的节点协议环境。
type nodeFixture struct {
	db *store.DB
	// nsID 是节点所属的**命名空间**：节点/能力/任务的 store 调用都用它。
	// 夹具里就是用户注册时同事务建的 default 命名空间（与线上注册路径一致）。
	nsID    string
	node    *store.Node
	token   string
	srv     *httptest.Server
	handler *NodeHandler

	// settleGroup 跟踪「还有几条 ServeHTTP 没返回」。handler 返回 ⇒ 该连接的
	// nodeSession.cleanup（**同步**写库）已经跑完 ⇒ 才可以关库（#255，见 newNodeFixture）。
	settleGroup settleGroup

	// trackMu 保护 tracked（客户端连接登记簿）。
	// dial 成功即登记；settle 时统一 CloseNow——服务端 readLoop 拿到 EOF 才会走 cleanup，
	// 于是「等 handler 返回」是有界操作。
	trackMu sync.Mutex
	tracked []*websocket.Conn
}

// newNodeFixture 建组织 + 节点 + 能力，并起一个真 HTTP server 挂 NodeHandler。
//
// cleanup 的**注册顺序**是硬要求（t.Cleanup 是 LIFO ⇒ 反向执行）：
//
//	db.Close   （最先注册 → 最后跑）
//	srv.Close
//	settle     （最后注册 → 最先跑）：seal → 关 tracked 连接 → 等所有 ServeHTTP 返回
//
// 于是「handler 返回」（nodeSession.cleanup 的同步写库已在其中完成）**先于** db.Close()。
// 反过来（旧夹具只有 db.Close → srv.Close）就会撞上 #255 的竞态：
// httptest.Server.Close() **不等待**已劫持（WebSocket）连接——net/http/httptest
// server.go 的 `case http.StateHijacked, http.StateClosed` 把它从 s.wg 摘掉——
// 于是 db.Close() 与 cleanup 的 MarkInstancesDead / UpdateNodeStatus / ResolveLostTasks
// 并发 → `WARN ... error="sql: database is closed"`；残留的 SQLite 连接还会在
// t.TempDir 的 RemoveAll 里重建 -wal/-shm → 随机某个用例红在
// `TempDir RemoveAll cleanup: ... directory not empty`（CI 里是 TestHelloUnknownCapability）。
func newNodeFixture(t *testing.T, opts ...func(*NodeHandler)) *nodeFixture {
	t.Helper()
	ctx := context.Background()
	f := &nodeFixture{settleGroup: newSettleGroup()}

	// store.Open 自己会加 file: 前缀与 PRAGMA 参数，这里只传纯路径。
	db, err := store.Open(t.TempDir() + "/node.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	f.db = db
	// ① 最先注册：所有会话退完之后才关库（LIFO 下最后执行）。
	t.Cleanup(func() { _ = db.Close() })

	// 用户 + default 命名空间（RegisterUser 同事务：建用户 + 建 default + 播种内置能力）
	// 组织层已在迁移 14 删除（#204）：命名空间是唯一隔离边界。
	u := &store.User{ID: store.NewUserID(), Username: "u-" + store.NewUserID(), CreatedAt: store.Now()}
	nsRow, err := db.RegisterUser(ctx, u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	// 节点令牌绑命名空间（设计文档 §2.1），所以拿到 default 命名空间就够跑全链路了。
	nsID := nsRow.ID

	// 能力定义（节点只能声明已定义的，不变量 I2；解析范围被命名空间收窄）
	cap1 := &store.CapabilityVersion{
		NamespaceID: nsID, Name: "review", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"fix"}, Source: store.CapSourceBuiltin,
	}
	if err := db.CreateCapability(ctx, cap1); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	// 节点
	node := &store.Node{NamespaceID: nsID, Name: "mbp-" + store.NewUserID(), Tags: []string{"pool"}}
	token, err := db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	h := NewNodeHandler(db)
	for _, o := range opts {
		o(h)
	}
	mux := http.NewServeMux()
	// 挂 wrapper（不是裸 h）：handler 的进入/返回都要过 settleGroup，
	// 否则「所有 handler 都返回了吗」无从得知。
	mux.Handle("GET /v1/node/stream", f.guarded(h))
	srv := httptest.NewServer(mux)
	f.srv = srv
	// ② srv.Close 只管 HTTP 层：劫持的连接它不等（见函数头）。
	t.Cleanup(srv.Close)
	// ③ 最后注册（LIFO 最先跑）：让所有会话在 srv.Close / db.Close 之前退干净。
	t.Cleanup(func() { f.settle(t) })

	f.nsID = nsID
	f.node = node
	f.token = token
	f.handler = h
	return f
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
	if err == nil && conn != nil {
		// 连上了才登记：settle 用这张表把「等 handler 返回」变成有界操作。
		f.track(conn)
	}
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

// ---------- 夹具收尾：确定性 happens-before（#255） ----------

// settleLimit 是等所有会话退出的上限。超时即判「会话没退」——这是失败，不是 flake。
const settleLimit = 15 * time.Second

// newSettleGroup 造一个可用的 settleGroup（done 必须已初始化）。
func newSettleGroup() settleGroup {
	return settleGroup{done: make(chan struct{})}
}

// settleGroup 是「进入/返回」计数器，语义上像 sync.WaitGroup，但有一处关键区别：
// **seal 之后所有 enter 一律被拒**。
//
// 为什么不能用裸 sync.WaitGroup：它的 Add 与 Wait 并发是非法用法（计数归零后再 Add
// 并与 Wait 重叠会 panic / 永久挂住），而这里的「+1」来自 http.Server 的连接 goroutine，
// 与测试 goroutine 的 Wait 之间**没有任何同步点**——正是那一面误用。
// 换成 mu + sealed 后，「检查 sealed」与「pending++」是一步原子操作：seal 返回时计数
// 只可能单调降到 0，不会反弹 ⇒ 等它有确定上界。
type settleGroup struct {
	mu      sync.Mutex
	sealed  bool
	pending int
	done    chan struct{}
	closed  bool // done 已关闭（防重复 close panic）
}

// enter 登记一次 handler 进入；已 seal（收尾已开始）→ 返回 false，调用方必须拒绝服务。
func (g *settleGroup) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return false
	}
	g.pending++
	return true
}

// exit 登记一次 handler 返回；最后一个返回且已 seal → 关闭 done。
func (g *settleGroup) exit() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending > 0 {
		g.pending--
	}
	g.closeIfSettledLocked()
}

// seal 封盘：此后 enter 一律被拒；返回的 channel 在「所有已进入的 handler 都返回」时关闭。
func (g *settleGroup) seal() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sealed = true
	g.closeIfSettledLocked()
	return g.done
}

// pendingCount 报告还有几条 handler 没返回（超时诊断用）。
func (g *settleGroup) pendingCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending
}

// closeIfSettledLocked 在「已封盘 + 计数归零」时关闭 done（调用方必须持锁）。
func (g *settleGroup) closeIfSettledLocked() {
	if !g.sealed || g.pending > 0 || g.closed {
		return
	}
	g.closed = true
	close(g.done)
}

// track 登记一条客户端连接（dial 成功后调用；settle 时统一关闭）。
func (f *nodeFixture) track(conn *websocket.Conn) {
	f.trackMu.Lock()
	f.tracked = append(f.tracked, conn)
	f.trackMu.Unlock()
}

// guarded 把 NodeHandler 包成 settleGroup 的进/出口。
//
// defer exit() 的位置就是 happens-before 的关键：NodeHandler.ServeHTTP 内部的
// `ns.run()` → `cleanup()` 是在**同一 goroutine 同步**写库的，所以 exit() 一定在
// 那些写库之后执行 ⇒ 「pending 归零」等价于「这些连接要写的库写全做完了」。
func (f *nodeFixture) guarded(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.settleGroup.enter() {
			// 收尾已经开始：不放新会话进来，否则永远等不清。
			http.Error(w, "test fixture already settled", http.StatusServiceUnavailable)
			return
		}
		defer f.settleGroup.exit()
		next.ServeHTTP(w, r)
	})
}

// settle 是夹具的确定性收尾（t.Cleanup 注册，LIFO 下先于 srv.Close / db.Close 跑）。
//
// 三步：① seal（不再接新会话）② CloseNow 所有 tracked 客户端连接（服务端 readLoop
// 拿到 EOF 才会走 cleanup）③ 等 pending 归零（channel 截止，**零 time.Sleep / 零轮询**）。
// 超时就直接报错并打出「还剩几条会话没退」：这是夹具没等干净的**确证失败**，
// 不该把症状甩给后面的用例（旧夹具的偶发 / 随机红正是这么来的）。
func (f *nodeFixture) settle(t *testing.T) {
	t.Helper()
	done := f.settleGroup.seal()

	f.trackMu.Lock()
	conns := append([]*websocket.Conn(nil), f.tracked...)
	f.trackMu.Unlock()
	for _, c := range conns {
		_ = c.CloseNow()
	}

	timer := time.NewTimer(settleLimit)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Errorf("ws 夹具收尾超时：%d 条会话在 %s 内没有退出（已强制关闭 %d 条客户端连接）；"+
			"handler 返回必须早于 db.Close()（#255）",
			f.settleGroup.pendingCount(), settleLimit, len(conns))
	}
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
//
// 陷阱（#255，已用库源码钉住）：读超时会**关掉整条连接**。
// coder/websocket@v1.8.15 conn.go:188-199 的 setupReadTimeout：
//
//	stop := context.AfterFunc(ctx, func() { c.clearReadTimeout(); c.close() })
//
// 即 Read(ctx) 的 ctx 到期 → c.close() → 客户端发 FIN → 服务端 readLoop 拿到 EOF →
// nodeSession.cleanup → hub.Unregister（hub.current(nodeID) 立刻变 nil）。
// 这条链与测试 goroutine **赛跑**，所以：
//   - 正向断言（等一帧）可以用它；
//   - 负向断言（「等一会儿看有没有帧」）只能在用例的**最后一步**用，且此后不得再
//     对这条连接/这个会话做任何断言（否则断言在跟 cleanup 抢跑）；
//   - 负向帧断言的正解是 assertNoFrame（node_frame_assert_test.go）：只在
//     「窗口用尽且连接在窗口内还活着」（context.DeadlineExceeded）时通过，连接早死
//     （net.ErrClosed / EOF）会失败——既不永真，也不把后续断言变成竞态。
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

// hello 发一个 applied[] 形状的 hello 并等到 welcome。
//
// v2 只有这一种 hello：节点陈述「本机缓存了哪些产物」（契约 §3），旧的
// capabilities[]（我能跑什么）+ manifest[]（我怎么跑）两个字段整个删除。
// 所以也没有「带 manifest 的 hello」这种 helper 了 —— 它不再是合法的协议形状。
func hello(t *testing.T, conn *websocket.Conn, nodeID string, applied []AppliedDecl, insts []InstanceDecl) {
	t.Helper()
	helloFrame(t, conn, &HelloFrame{
		Type: NodeFrameHello, NodeID: nodeID, Protocol: ProtocolVersion,
		Applied: applied, Instances: insts,
	})
}

// helloFrame 发送调用方自己拼的 hello 帧并等到 welcome。
//
// 用于需要精确控制帧内容的用例（协议版本、身份、空 applied 等）：
// 这些形状用 hello 的固定参数表达不出来。
func helloFrame(t *testing.T, conn *websocket.Conn, f *HelloFrame) {
	t.Helper()
	send(t, conn, f)
	welcome := readUntil(t, conn, NodeFrameWelcome)
	if welcome["heartbeat_sec"] == nil || welcome["lease_ttl_sec"] == nil {
		t.Fatalf("welcome missing heartbeat/lease config: %v", welcome)
	}
}

// readErrorFrame 断言下一帧就是 error（握手被拒时不该先发 welcome）。
func readErrorFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	m, ok := tryReadFrame(conn, 2*time.Second)
	if !ok {
		t.Fatal("timeout waiting for error frame")
	}
	if m["type"] != NodeFrameError {
		t.Fatalf("frame after rejected hello = %q, want %q (no welcome before an error)", m["type"], NodeFrameError)
	}
	return m
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
		if err := f.db.UpdateNodeStatus(context.Background(), f.nsID, f.node.ID, store.NodeStatusDraining); err != nil {
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
		[]AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 4}},
	)

	// 节点应被标记为 online
	got, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
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
	// 为什么这里不再断言 run_spec：v2 的 hello 没有 manifest，node_capabilities 也没有
	// run_spec 列（迁移 18）。「节点上报的执行方式原样落库」这条能力整个消失，
	// 替代覆盖见 TestHelloAppliedPersists（applied 落库 + 读回一致）。
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
		Applied:   []AppliedDecl{{Capability: "does-not-exist", Version: 9}},
		Instances: []InstanceDecl{{Slot: 0, Concurrency: 1}},
	})
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeUnknownCapability {
		t.Fatalf("code = %v, want unknown_capability", frame["code"])
	}
	// 节点不该上线
	got, _ := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
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
		Applied:   []AppliedDecl{{Capability: "review", Version: 1}},
		Instances: []InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}},
	})
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeTooManyInstances {
		t.Fatalf("code = %v, want too_many_instances", frame["code"])
	}
}

// ---------- v2：hello.applied[]（「我缓存了什么」）与部署对账 ----------
//
// 这一节整体替换了 #168 的 manifest 系列（下面每个用例头都写了「为什么断言变了」）。
// 旧的四个用例钉的是「节点自报执行方式」：run_spec 往返、manifest 覆盖 capabilities、
// manifest 部分覆盖、run 非法时整帧拒绝 + mergeDeclarations 纯函数。
// v2 把这条能力整个删掉（执行方式属于产物 artifacts.spec，中枢不解释也不转发），
// 所以它们不是「行为漂移」，而是被测对象消失 —— 替代覆盖是 applied 的对账语义：
//   · applied 落库 + 读回一致（替代 run_spec 往返）
//   · applied 里未知能力 → 整帧拒绝且一次列全（保留，形状换成 applied）
//   · applied.digest 与中枢 desired 不一致 → deployments.state = local（替代 manifest 合并冲突）

// TestHelloAppliedPersists applied[] 落成 node_capabilities，读回就是那一组（契约 D4）。
//
// 为什么断言变了：旧用例（TestHelloManifestPersistsRunSpec）断言「run_spec 存的是
// 节点上报的 JSON 原文」。run_spec 列已随迁移 18 删除，能存能读的只剩「这台机器
// 归属哪些能力」这一组，所以断言换成：applied 里的 name@version 全量落库、读回一致。
func TestHelloAppliedPersists(t *testing.T) {
	f := newNodeFixture(t)
	f.defineCapability(t, "test", 1)
	conn := f.mustDial(t)

	hello(t, conn, f.node.ID,
		[]AppliedDecl{
			{Capability: "review", Version: 1, Digest: "sha256:aaaa"},
			{Capability: "test", Version: 1, Digest: "sha256:bbbb"},
		},
		[]InstanceDecl{{Slot: 0, Concurrency: 4}},
	)

	caps := listNodeCaps(t, f.db, f.node.ID)
	if len(caps) != 2 {
		t.Fatalf("node capabilities = %v, want exactly [review@1 test@1]", capSummary(caps))
	}
	if caps[0].Name != "review" || caps[1].Name != "test" {
		t.Fatalf("归属内容/顺序不对: %v", capSummary(caps))
	}

	// 节点确实上线了（applied 合法时握手照常成功）
	got, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status != store.NodeStatusOnline {
		t.Fatalf("node status = %q, want online", got.Status)
	}
}

// TestHelloAppliedIsFullReplacement 再次 hello 是全量替换：没上报的产物归属消失。
//
// 为什么断言变了：旧用例（TestHelloManifestOverridesCapabilities / PartialCoverage）
// 断言「manifest 与 capabilities 两个来源怎么合并（manifest 优先、独有项追加）」。
// v2 只剩一个来源（applied），合并算法不存在了；但「全量替换」这条语义必须照样钉住
// —— 它是 #218 那类「节点重启后归属残留」故障的唯一防线。
func TestHelloAppliedIsFullReplacement(t *testing.T) {
	f := newNodeFixture(t)
	f.defineCapability(t, "test", 1)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID,
		[]AppliedDecl{{Capability: "review", Version: 1}, {Capability: "test", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}},
	)
	if got := len(listNodeCaps(t, f.db, f.node.ID)); got != 2 {
		t.Fatalf("首次 hello 后归属 = %d 条, want 2", got)
	}

	// 节点重启（旧连接先断干净，再连一条新的），缓存里只剩 test。
	//
	// 为什么必须先断开：不断开时服务端会把新连接当作「顶掉旧连接」处理，
	// 而踢旧连接是**阻塞**的（closeWith 要等对端回 close 帧），新连接的 welcome
	// 会被推后到 2s 读超时之后 —— 那是 #218 的顶替路径，不是这个用例要测的语义。
	_ = conn.CloseNow()
	waitNodeOfflineAndDead(t, f)

	conn2 := f.mustDial(t)
	hello(t, conn2, f.node.ID,
		[]AppliedDecl{{Capability: "test", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}},
	)
	caps := listNodeCaps(t, f.db, f.node.ID)
	if len(caps) != 1 || caps[0].Name != "test" {
		t.Fatalf("全量替换失败，归属 = %v, want [test@1]", capSummary(caps))
	}
}

// TestHelloAppliedUnknownListsAll applied 里出现中枢没定义的能力 → 整帧拒绝、一次列全。
//
// 为什么断言变了：旧用例（TestHelloManifestUnknownCapabilityFirst）在「未知能力」
// 之外还混着 run 配置错误（runner 不支持 / command 为空），断言两者都进 message。
// run 校验已随 manifest 下线，能一次列全的只剩「未定义能力」这一组；
// 但「根因优先 + 一条都不漏」这两条要求原样保留。
func TestHelloAppliedUnknownListsAll(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)

	send(t, conn, &HelloFrame{
		Type: NodeFrameHello, NodeID: f.node.ID, Protocol: ProtocolVersion,
		Applied: []AppliedDecl{
			{Capability: "review", Version: 1}, // 唯一已定义的那个
			{Capability: "a", Version: 1},
			{Capability: "b", Version: 2},
		},
		Instances: []InstanceDecl{{Slot: 0, Concurrency: 1}},
	})

	frame := readErrorFrame(t, conn)
	if frame["code"] != ErrCodeUnknownCapability {
		t.Fatalf("code = %v, want %q", frame["code"], ErrCodeUnknownCapability)
	}
	msg, _ := frame["message"].(string)
	if !strings.Contains(msg, "unknown capability: a@1, b@2") {
		t.Errorf("message %q 没有把两个未知能力列全（按上报顺序）", msg)
	}
	if strings.Contains(msg, "review@1") {
		t.Errorf("message %q 把已定义的 review@1 也报成了问题", msg)
	}

	assertNodeNotOnline(t, f)
	if caps := listNodeCaps(t, f.db, f.node.ID); len(caps) != 0 {
		t.Fatalf("被拒的 hello 留下了半份归属: %v", capSummary(caps))
	}
}

// TestHelloAppliedDigestMarksLocal applied.digest 与中枢 desired 不一致 → local。
//
// 为什么断言变了：旧用例（TestHelloManifestPartialCoverage 之后那一族）钉的是
// 「manifest 里重复项后者胜 / caps 重复项只出一条」这类合并算法。v2 的中枢与节点之间
// 唯一可能不一致的东西是 digest，而它的后果是契约 §7 的「本地变体」状态。
// 这条断言就是旧「合并冲突」用例在新模型下的对应物。
func TestHelloAppliedDigestMarksLocal(t *testing.T) {
	f := newNodeFixture(t)
	ctx := context.Background()

	// 中枢侧：给 review@1 登记一份产物并部署给这台节点（state=pending）
	cap1, err := f.db.GetCapability(ctx, f.nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	art, err := f.db.CreateArtifact(ctx, &store.Artifact{
		NamespaceID:  f.nsID,
		CapabilityID: cap1.ID,
		Digest:       "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Spec:         []byte(`{"apiVersion":"anotify/v1"}`),
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	dp, err := f.db.CreateDeployment(ctx, f.nsID, f.node.ID, art.ID)
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	// 节点上缓存的是一份被手改过的产物
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID,
		[]AppliedDecl{{Capability: "review", Version: 1,
			Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}},
	)

	got, err := f.db.GetDeployment(ctx, f.nsID, dp.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.State != store.DeploymentLocal {
		t.Fatalf("hello 对账后 state = %q, want %q（本地变体）", got.State, store.DeploymentLocal)
	}
	if !strings.Contains(got.Reason, art.Digest) {
		t.Fatalf("reason %q 里没有中枢期望的 digest %s", got.Reason, art.Digest)
	}
	// 归属还在（本地变体只是中枢不敢覆盖，不是不能派发）
	if caps := listNodeCaps(t, f.db, f.node.ID); len(caps) != 1 {
		t.Fatalf("本地变体的归属丢了: %v", capSummary(caps))
	}
}

// TestHelloProtocolVersion1Rejected 协议版本 1 的 hello 必须被拒（契约 §1：老客户端必须升级）。
//
// v2 改了 hello 的形状（删 capabilities/manifest、加 applied），一个 v1 节点上来
// 只会在 unknown_capability / bad_frame 之间反复横跳，症状远比「版本不匹配」难查。
// 所以在协议层直接拒绝，并给出可操作的错误文本（两个版本号都写出来）。
//
// 错误码用 protocol_mismatch 而不是 bad_frame（契约 §1）：这是**版本不兼容**，
// 不是「帧写坏了」。看到这个码应该去升级二进制，而不是查 JSON 结构。
func TestHelloProtocolVersion1Rejected(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)

	send(t, conn, &HelloFrame{
		Type: NodeFrameHello, NodeID: f.node.ID, Protocol: 1,
		Applied:   []AppliedDecl{{Capability: "review", Version: 1}},
		Instances: []InstanceDecl{{Slot: 0, Concurrency: 1}},
	})

	frame := readErrorFrame(t, conn)
	if frame["code"] != ErrCodeProtocolMismatch {
		t.Fatalf("code = %v, want %q", frame["code"], ErrCodeProtocolMismatch)
	}
	msg, _ := frame["message"].(string)
	if !strings.Contains(msg, "unsupported protocol 1") {
		t.Fatalf("message = %q, want 含 'unsupported protocol 1'", msg)
	}
	if !strings.Contains(msg, fmt.Sprint(ProtocolVersion)) {
		t.Fatalf("message = %q, want 含服务端版本 %d（否则节点不知道该升到哪一版）", msg, ProtocolVersion)
	}
	assertNodeNotOnline(t, f)
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

// TestLeaseExtended 心跳把**本连接名下的**任务的 lease_until 推到 now+TTL（#70 验收）。
//
// #218 起租约的粒度是「连接名下的任务」，不再按节点：所以这里必须真的把任务派给
// 这条连接（PublishAssign 登记归属）——否则心跳压根不应该续它（负向情形见
// node_lease_test.go 的 TestHeartbeatRenewsOnlyOwnedTasks）。
func TestLeaseExtended(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx := context.Background()
	// 建一个 assigned 到本节点的任务
	cap1, err := f.db.GetCapability(ctx, f.nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	task := &store.Task{
		NamespaceID: f.nsID, TraceID: "trace_lease", CapabilityID: cap1.ID,
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
	if err := f.db.AssignTask(ctx, f.nsID, task.ID, f.node.ID, instID, stale); err != nil {
		t.Fatalf("assign: %v", err)
	}
	// 派给这条连接（登记归属与派发世代）；assign 帧本身不关心，客户端读不读都行。
	assigned, err := f.db.GetTask(ctx, f.nsID, task.ID)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if err := f.handler.hub().PublishAssign(ctx, f.nsID, f.node.ID, assigned); err != nil {
		t.Fatalf("publish assign: %v", err)
	}

	// 发心跳，租约应被推到 now+30
	send(t, conn, &HeartbeatFrame{Type: NodeFrameHeartbeat, TS: time.Now().Unix()})
	// 给服务端一点处理时间
	deadline := time.Now().Add(3 * time.Second)
	var lease int64
	for time.Now().Before(deadline) {
		got, err := f.db.GetTask(ctx, f.nsID, task.ID)
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
	node, err := f.db.GetNode(ctx, f.nsID, f.node.ID)
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
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx := context.Background()
	task := f.makeAssignedTask(t, "trace_flow")

	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskAcked)

	send(t, conn, &ProgressFrame{Type: NodeFrameProgress, TaskID: task.ID, Note: "step 2/5"})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskRunning)

	send(t, conn, &ResultFrame{Type: NodeFrameResult, TaskID: task.ID, Status: store.TaskSucceeded,
		Summary: "ok", Out: strPtr("fix")})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskSucceeded)

	got, err := f.db.GetTask(ctx, f.nsID, task.ID)
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
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_missing")
	send(t, conn, &ProgressFrame{Type: NodeFrameProgress, TaskID: task.ID})
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskRunning)

	// 必须发**不带 out 键**的原始 JSON。
	// 不能用 ResultFrame 字面量：它的 MarshalJSON 总会写出 "out":null，
	// 那是「显式到头」而不是「没回报」——正是 I5 要区分的东西。
	sendRaw(t, conn, `{"type":"result","task_id":"`+task.ID+`","status":"succeeded","summary":"done"}`)
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskSucceeded)

	got, err := f.db.GetTask(context.Background(), f.nsID, task.ID)
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
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTask(t, "trace_null")
	// 直接发 result（跳过 ack/progress，验证隐式 running）。
	// 显式 "out":null 与「缺 out 键」在线上 JSON 里不同，必须发原始文本才能表达。
	sendRaw(t, conn, `{"type":"result","task_id":"`+task.ID+`","status":"succeeded","out":null}`)
	waitStatus(t, f.db, f.nsID, task.ID, store.TaskSucceeded)

	got, err := f.db.GetTask(context.Background(), f.nsID, task.ID)
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
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx := context.Background()
	cap1, _ := f.db.GetCapability(ctx, f.nsID, "review", 1)
	task := &store.Task{
		NamespaceID: f.nsID, TraceID: "trace_other_node", CapabilityID: cap1.ID,
		Capability: cap1.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: cap1.ContentHash, GraphID: "grf_x",
	}
	if err := f.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 派给另一个节点
	otherNode := &store.Node{NamespaceID: f.nsID, Name: "other-" + store.NewUserID()}
	if _, err := f.db.CreateNode(ctx, otherNode); err != nil {
		t.Fatalf("create other node: %v", err)
	}
	if _, err := f.db.UpsertInstances(ctx, f.nsID, otherNode.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("instances: %v", err)
	}
	instID, err := f.db.LeaseInstance(ctx, otherNode.ID, task.ID)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := f.db.AssignTask(ctx, f.nsID, task.ID, otherNode.ID, instID, store.Now()+60); err != nil {
		t.Fatalf("assign: %v", err)
	}

	send(t, conn, &AckFrame{Type: NodeFrameAck, TaskID: task.ID})
	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeInvalidTransition {
		t.Fatalf("code = %v, want invalid_transition", frame["code"])
	}
	// 状态没被改
	got, _ := f.db.GetTask(ctx, f.nsID, task.ID)
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
	got, _ := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
	if got.Status != store.NodeStatusOnline {
		t.Fatalf("status = %q, want online", got.Status)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "bye")

	// 等 cleanup 生效
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
		if err == nil && got.Status == store.NodeStatusOffline {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _ = f.db.GetNode(context.Background(), f.nsID, f.node.ID)
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

// defineCapability 在夹具命名空间下定义一个能力（需要多于一个能力的用例用）。
func (f *nodeFixture) defineCapability(t *testing.T, name string, version int) {
	t.Helper()
	capVer := &store.CapabilityVersion{
		NamespaceID: f.nsID, Name: name, Version: version,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"fix"}, Source: store.CapSourceBuiltin,
	}
	if err := f.db.CreateCapability(context.Background(), capVer); err != nil {
		t.Fatalf("create capability %s@%d: %v", name, version, err)
	}
}

// listNodeCaps 读回节点拥有（= 已对账）的能力。
func listNodeCaps(t *testing.T, db *store.DB, nodeID string) []*store.NodeCapability {
	t.Helper()
	caps, err := db.ListNodeCapabilities(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list node capabilities: %v", err)
	}
	return caps
}

// nodeCapOf 在读回的能力里找 name@version（找不到直接失败）。
func nodeCapOf(t *testing.T, caps []*store.NodeCapability, name string, version int) *store.NodeCapability {
	t.Helper()
	for _, c := range caps {
		if c.Name == name && c.Version == version {
			return c
		}
	}
	t.Fatalf("capability %s@%d not found in %v", name, version, capSummary(caps))
	return nil
}

// capSummary 把读回的能力渲染成 name@version 列表（失败信息用）。
func capSummary(caps []*store.NodeCapability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, fmt.Sprintf("%s@%d", c.Name, c.Version))
	}
	return out
}

// assertNodeNotOnline 断言握手被拒的节点没有上线（也没有被标脏）。
func assertNodeNotOnline(t *testing.T, f *nodeFixture) {
	t.Helper()
	got, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status == store.NodeStatusOnline {
		t.Fatalf("node went online despite a rejected hello (status = %q)", got.Status)
	}
	if f.handler.hub().Online(f.node.ID) {
		t.Fatal("rejected node was registered in the hub")
	}
}

// makeAssignedTask 建一个已派给本节点的任务。
func (f *nodeFixture) makeAssignedTask(t *testing.T, traceID string) *store.Task {
	t.Helper()
	ctx := context.Background()
	cap1, err := f.db.GetCapability(ctx, f.nsID, "review", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	task := &store.Task{
		NamespaceID: f.nsID, TraceID: traceID, CapabilityID: cap1.ID,
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
	if err := f.db.AssignTask(ctx, f.nsID, task.ID, f.node.ID, instID, store.Now()+store.AckWaitSec); err != nil {
		t.Fatalf("assign: %v", err)
	}
	return task
}

// waitStatus 等任务到达期望状态（异步处理，需要轮询）。
func waitStatus(t *testing.T, db *store.DB, namespaceID, taskID, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		got, err := db.GetTask(context.Background(), namespaceID, taskID)
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
