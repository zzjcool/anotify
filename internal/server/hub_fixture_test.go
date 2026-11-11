package server

// 本文件是 server 包**唯一**的「真 WebSocket hub server」夹具：newHubServer。
//
// # 为什么需要它（#340 r3 / #259 同类收尾竞态）
//
// 真 hub 的 detach（internal/ws/hub.go）在**返回 ServeHTTP 之前**同步写库：
//
//	MarkNodeOffline(ctx, nodeID, now)   → update node status: ...
//	MarkInstancesDead(ctx, nodeID)      → mark instances dead: ...
//
// 而夹具里 db.Close() 注册得最早（t.Cleanup 是 LIFO ⇒ 它最后跑），中间只有
// `srv.Close()` 与 `conn.Close()` 两道收尾 —— 这两道都**不能**保证 hub 的写库已完成：
//
//   - `httptest.Server.Close()` 不等待已 hijack 的连接：net/http/httptest 的
//     server.go 对 `http.StateHijacked` 会把连接从 s.wg 摘掉，Close 立刻返回。
//     WebSocket 正是 hijack 连接，所以「srv 关了」≠「handler 返回了」。
//   - 等 `hub.Online(nodeID)==false` 也不行：detach 的顺序是**先**摘注册表（Online
//     立刻变 false）**再**写库。所以 Online==false 恰好落在「写库还没发生」的窗口里。
//
// 唯一等价于「写库已发生」的观测点是 **ServeHTTP 已返回**。于是这里把 hub.ServeHTTP
// 包一层，用 settleGroup 记录进入/返回，cleanup 里 seal → 关掉已登记的连接 → 等计数归零。
//
// # 为什么是 seal + 计数，而不是裸 sync.WaitGroup
//
// 「+1」发生在 http.Server 的连接 goroutine 里，与测试 goroutine 的等待之间**没有**
// 同步点：裸 WaitGroup 的 Add 与 Wait 并发是非法用法（计数归零后再 Add 与 Wait 重叠
// 会永久挂住/panic）。settleGroup 用 mu + sealed 把「检查 sealed」与「pending++」做成
// 一步原子操作 —— seal 返回后计数只可能单调降到 0，wait 因此有确定上界。
// 出处：internal/ws 的 node_test.go（#255/#258，`git show 3433eef -- internal/ws/node_test.go`），
// 以及被本 PR 删除的 internal/server/wsfixture_test.go
// （`git show 9305bac^:internal/server/wsfixture_test.go`）。
//
// # 使用纪律（调用点只有两个，但边界必须写清楚）
//
//  1. **一个 fixture 一个 hubServer**：在 fixture 构造函数里建好（不是每次拨号建一个），
//     这样「屏障的 cleanup 晚于 db.Close 注册」是结构性的，不依赖调用顺序的记忆。
//  2. `newHubServer` 必须在 **db.Close 的 t.Cleanup 注册之后**调用 —— LIFO 才成立：
//
//	db.Close        （夹具最先注册 → 最后跑）
//	srv.Close       （第二个跑：只关 HTTP 层，不等 hijack 连接的 handler）
//	hubServer.settle（最先跑：seal → CloseNow 已登记连接 → 等所有 ServeHTTP 返回）
//	dial 的 conn.Close（最先跑里的最前面：优雅关闭交给服务端读到 EOF）
//
// 于是「detach 写库」严格 happens-before「db.Close()」，postclose_guard_test.go 观察的
// `mark instances dead failed ... database is closed` 不再可能发生。
//  3. 需要新的节点会话就 `f.dial(t)`：连接登记在 fixture 里，settle 时有界退出。
//
// 覆盖边界（实测，别把这里读多）：本包建真会话的 6 处（`node_upgrade_test.go` 的 5 次
// `connectNode` + `resource_task_test.go` 的 logs 夹具）都是「一次 `newHubServer` + 一次
// `dial`」。**同一 server 的第二次拨号**（`hub.attach` 用新连接顶掉旧连接）目前没有用例
// 走到——`dial` 只是可以反复调用、多次 dial 不会破坏收尾屏障，不代表 supersede 已被覆盖。

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

// hubSettleLimit 是等 hub.ServeHTTP 返回的上限。超时即判「会话没退干净」——这是失败，
// 不是 flake（本包单条会话退出是毫秒级，5s 足够宽松）。
const hubSettleLimit = 5 * time.Second

// hubServer 是一套「真 hub HTTP server + 收尾屏障」。
type hubServer struct {
	srv *httptest.Server

	// settleGroup 跟踪「还有几条 ServeHTTP 没返回」。handler 返回 ⇒ detach 的同步写库
	// 已经完成 ⇒ 才可以关库（#259）。
	settleGroup settleGroup

	// trackMu 保护 tracked（客户端连接登记簿）。dial 成功即登记；settle 时统一 CloseNow。
	trackMu sync.Mutex
	tracked []*websocket.Conn
}

// newHubServer 起一个真 HTTP server 挂 hub.ServeHTTP，并注册收尾屏障。
//
// 调用约束见文件头第 2 条：必须在本 fixture 的 db.Close cleanup **之后**调用。
func newHubServer(t *testing.T, hub *ws.Hub) *hubServer {
	t.Helper()
	if hub == nil {
		t.Fatal("newHubServer: hub 不能为 nil")
	}
	f := &hubServer{settleGroup: newSettleGroup()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.settleGroup.enter() {
			// 收尾已经开始：不放新会话进来，否则「等计数归零」永远等不清。
			http.Error(w, "hub fixture already settled", http.StatusServiceUnavailable)
			return
		}
		defer f.settleGroup.exit()
		hub.ServeHTTP(w, r)
	}))
	f.srv = srv
	// srv.Close 只管 HTTP 层：劫持的连接它不等（见文件头）。
	t.Cleanup(srv.Close)
	// 最后注册（LIFO 最先跑）：让所有会话在 srv.Close / db.Close 之前退干净。
	t.Cleanup(func() { f.settle(t) })
	return f
}

// streamURL 是节点 WS 端点的完整 URL（ws://…）。
func (f *hubServer) streamURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/v1/node/stream"
}

// dial 建立一条节点 WS 连接并登记到 fixture（settle 时有界退出）。
//
// 优雅关闭的 cleanup 在这里注册 ⇒ 它晚于 settle 的 cleanup 注册 ⇒ 先跑：
// 服务端先读到一个正常的 close 帧，再在 settle 的 CloseNow 里兜底。
func (f *hubServer) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, f.streamURL(), nil)
	if err != nil {
		t.Fatalf("dial node stream: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	f.track(conn)
	return conn
}

// track 登记一条客户端连接（dial 成功后调用；settle 时统一关闭）。
func (f *hubServer) track(conn *websocket.Conn) {
	f.trackMu.Lock()
	f.tracked = append(f.tracked, conn)
	f.trackMu.Unlock()
}

// settle 是夹具的确定性收尾（t.Cleanup 注册，LIFO 下先于 srv.Close / db.Close 跑）。
//
// 三步：① seal（不再接新会话）② CloseNow 所有已登记连接（服务端读循环拿到 EOF/错误
// 才会走 detach）③ 等 pending 归零（channel 截止，**零 time.Sleep / 零轮询**）。
// 超时就直接报错并打出「还剩几条会话没退」：这是夹具没等干净的**确证失败**，
// 不该把症状甩给后面的用例（db.Close 时仍在写库就是 #259 的那条 WARN）。
func (f *hubServer) settle(t *testing.T) {
	t.Helper()
	done := f.settleGroup.seal()

	f.trackMu.Lock()
	conns := append([]*websocket.Conn(nil), f.tracked...)
	f.trackMu.Unlock()
	for _, c := range conns {
		_ = c.CloseNow()
	}

	timer := time.NewTimer(hubSettleLimit)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatalf("hub 夹具收尾超时：%d 条会话在 %s 内没有退出（已强制关闭 %d 条客户端连接）；"+
			"handler 返回必须早于 db.Close()（#259 / #340 r3）",
			f.settleGroup.pendingCount(), hubSettleLimit, len(conns))
	}
}

// ---------- settleGroup：seal 语义的进入/返回计数器 ----------

// settleGroup 是「进入/返回」计数器，语义像 sync.WaitGroup，但有一处关键区别：
// **seal 之后所有 enter 一律被拒**，且 done 只在「已 seal + 计数归零」时关闭。
//
// 为什么不能用裸 sync.WaitGroup：它的 Add 与 Wait 并发是非法用法（计数归零后再 Add
// 并与 Wait 重叠会 panic / 永久挂住），而这里的「+1」来自 http.Server 的连接 goroutine，
// 与测试 goroutine 的 Wait 之间**没有任何同步点**——正是那一面误用。
type settleGroup struct {
	mu      sync.Mutex
	sealed  bool
	pending int
	done    chan struct{}
	closed  bool // done 已关闭（防重复 close panic）
}

// newSettleGroup 造一个可用的 settleGroup（done 必须已初始化）。
func newSettleGroup() settleGroup {
	return settleGroup{done: make(chan struct{})}
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

// exit 登记一次 handler 返回；「已 seal + 计数归零」时关闭 done。
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

// ---------- settleGroup 自身的回归用例 ----------

// TestSettleGroupSealRejectsLateEnterAndWaitsPending 钉住 seal 语义的两条不变量：
// 封盘后的 enter 被拒；done 只在「已封盘 + 计数归零」时关闭。
//
// 为什么值得单测：hubServer 的行为在集成测试里「碰巧对一个连接也对」，但 seal 的真正
// 价值是「收尾开始后不再有新会话」——那一条只能在这里确定性地钉住。
func TestSettleGroupSealRejectsLateEnterAndWaitsPending(t *testing.T) {
	g := newSettleGroup()
	if !g.enter() {
		t.Fatal("封盘前 enter 应成功")
	}
	done := g.seal()
	if g.enter() {
		t.Fatal("封盘后 enter 必须被拒（否则收尾永远等不清）")
	}
	if g.pendingCount() != 1 {
		t.Fatalf("pendingCount = %d, want 1（被拒的 enter 不得计数）", g.pendingCount())
	}
	select {
	case <-done:
		t.Fatal("还有 handler 没返回，done 不该关闭")
	default:
	}
	g.exit()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("最后一条 handler 返回后 done 应立即关闭")
	}
	if g.pendingCount() != 0 {
		t.Fatalf("pendingCount = %d, want 0", g.pendingCount())
	}
	// 幂等：重复 exit 不得把计数负溢出，也不得二次 close。
	g.exit()
	if g.pendingCount() != 0 {
		t.Fatalf("多余 exit 后 pendingCount = %d, want 0", g.pendingCount())
	}
}
