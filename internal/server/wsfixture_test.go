package server

// 本文件是 #259 的夹具收口：把 #258 在 internal/ws 里验证过的 settle 模式
// **局部**搬到 server 包（不跨包抽象、不改 consoleEnv 的其它行为）。
//
// 为什么需要它：httptest.Server.Close() **不等待**已劫持（WebSocket）连接——
// net/http/httptest server.go 的 `case http.StateHijacked, http.StateClosed`
// 把它从 s.wg 摘掉——而 nodeSession.cleanup（internal/ws/node_handler.go）在
// handler goroutine 里**同步**写库（MarkInstancesDead / UpdateNodeStatus /
// ResolveLostTasks）。于是「srv.Close → db.Close」与 cleanup 并发就成了
// `WARN ... error="sql: database is closed"`，残留的 SQLite 连接还会在
// t.TempDir 的 RemoveAll 期间重建 -wal/-shm（#259 的失败签名：TempDir 里只剩
// 一个 console.db-shm）。postclose_guard_test.go 是本文件的护栏（命中即整包红）。
//
// settleGroup / newNodeFixture 的出处：internal/ws/node_test.go（#255/#258，
// `git show 3433eef -- internal/ws/node_test.go`）。这里只保留 server 需要的那一半
// （没有 store 播种、没有 handler 选项），语义逐字照搬——尤其 settleGroup 的
// sealed/pending/done 三态，不能换成裸 sync.WaitGroup（原因见其注释）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/ws"
)

// nodeStreamFixture 是「控制台路由 + 真节点 WS 端点」的最小夹具：
// env.mux 挂在 `/`，`GET /v1/node/stream` 更具体所以优先生效（与生产 mux 同形）。
type nodeStreamFixture struct {
	env *consoleEnv
	srv *httptest.Server

	// settleGroup 跟踪「还有几条 ServeHTTP 没返回」。handler 返回 ⇒ 该连接的
	// nodeSession.cleanup（**同步**写库）已经跑完 ⇒ 才可以关库（#259）。
	settleGroup settleGroup

	// trackMu 保护 tracked（客户端连接登记簿）。
	// dial 成功即登记；settle 时统一 CloseNow——服务端 readLoop 拿到 EOF 才会走
	// cleanup，于是「等 handler 返回」是有界操作。
	trackMu sync.Mutex
	tracked []*websocket.Conn
}

// newNodeStreamFixture 在给定的 consoleEnv 上起一个真 HTTP server 挂 NodeHandler。
//
// cleanup 的**注册顺序**是硬要求（t.Cleanup 是 LIFO ⇒ 反向执行）：
//
//	db.Close      （由 newConsoleEnv 最先注册 → 最后跑）
//	TempDir 清理  （t.TempDir 自己注册，紧随 db.Close 之后）
//	srv.Close     （本函数注册）
//	settle        （最后注册 → 最先跑）：seal → 关 tracked 连接 → 等所有 ServeHTTP 返回
//
// 于是「handler 返回」（nodeSession.cleanup 的同步写库已在其中完成）**先于**
// db.Close()，而 db.Close() 又先于 t.TempDir 的 RemoveAll。
func newNodeStreamFixture(t *testing.T, env *consoleEnv) *nodeStreamFixture {
	t.Helper()
	f := &nodeStreamFixture{env: env, settleGroup: newSettleGroup()}

	srvMux := http.NewServeMux()
	srvMux.Handle("/", env.mux)
	// 挂 wrapper（不是裸 handler）：handler 的进入/返回都要过 settleGroup，
	// 否则「所有 handler 都返回了吗」无从得知。noStore 保持原有接线不变（只在
	// 外面加响应头，不影响进出计数）。
	srvMux.Handle("GET /v1/node/stream", noStore(f.guarded(ws.NewNodeHandler(env.db))))
	srv := httptest.NewServer(srvMux)
	f.srv = srv
	// srv.Close 只管 HTTP 层：劫持的连接它不等（见文件头）。
	t.Cleanup(srv.Close)
	// 最后注册（LIFO 最先跑）：让所有会话在 srv.Close / db.Close 之前退干净。
	t.Cleanup(func() { f.settle(t) })
	return f
}

// streamURL 是节点 WS 端点的完整 URL（ws://…）。
func (f *nodeStreamFixture) streamURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/v1/node/stream"
}

// dial 建立一条节点 WS 连接（token 为空时不带 Authorization，用于鉴权测试）。
func (f *nodeStreamFixture) dial(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	opts := &websocket.DialOptions{}
	if token != "" {
		opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + token}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, f.streamURL(), opts)
	if err == nil && conn != nil {
		// 连上了才登记：settle 用这张表把「等 handler 返回」变成有界操作。
		f.track(conn)
	}
	return conn, resp, err
}

// ---------- 夹具收尾：确定性 happens-before（#259） ----------

// settleLimit 是等所有会话退出的上限。超时即判「会话没退」——这是失败，不是 flake。
const settleLimit = 15 * time.Second

// newSettleGroup 造一个可用的 settleGroup（done 必须已初始化）。
func newSettleGroup() settleGroup {
	return settleGroup{done: make(chan struct{})}
}

// settleGroup 是「进入/返回」计数器，语义上像 sync.WaitGroup，但有一处关键区别：
// **seal 之后所有 enter 一律被拒**。（出处：internal/ws/node_test.go，#258）
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
func (f *nodeStreamFixture) track(conn *websocket.Conn) {
	f.trackMu.Lock()
	f.tracked = append(f.tracked, conn)
	f.trackMu.Unlock()
}

// guarded 把 NodeHandler 包成 settleGroup 的进/出口。
//
// defer exit() 的位置就是 happens-before 的关键：NodeHandler.ServeHTTP 内部的
// `ns.run()` → `cleanup()` 是在**同一 goroutine 同步**写库的，所以 exit() 一定在
// 那些写库之后执行 ⇒ 「pending 归零」等价于「这些连接要写的库写全做完了」。
func (f *nodeStreamFixture) guarded(next http.Handler) http.Handler {
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
// 不该把症状甩给后面的用例（db.Close 时仍在写库就是 #259 的那条 WARN）。
func (f *nodeStreamFixture) settle(t *testing.T) {
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
		t.Fatalf("ws 夹具收尾超时：%d 条会话在 %s 内没有退出（已强制关闭 %d 条客户端连接）；"+
			"handler 返回必须早于 db.Close()（#259）",
			f.settleGroup.pendingCount(), settleLimit, len(conns))
	}
}
