package server

// 本文件提供 server 包唯一的「真 WebSocket 会话」收尾屏障：serveHubWithSettle。
//
// # 为什么需要它（#340 r2 review M-2 / #259 同类竞态）
//
// 真 hub 的 detach（internal/ws/hub.go）在**返回 ServeHTTP 之前**同步写库：
//
//	MarkNodeOffline(ctx, nodeID, now)   → update node status: ...
//	MarkInstancesDead(ctx, nodeID)      → mark instances dead: ...
//
// 而夹具里 db.Close() 注册得最早（t.Cleanup 是 LIFO ⇒ 它最后跑），中间只有
// `srv.Close()` 与 `conn.Close()` 两道收尾。这两道都**不能**保证 hub 的写库已完成：
//
//   - `httptest.Server.Close()` 不等待已 hijack 的连接：net/http/httptest 的
//     server.go 对 `http.StateHijacked` 会把连接从 s.wg 摘掉，Close 立刻返回。
//     WebSocket 正是 hijack 连接，所以「srv 关了」≠「handler 返回了」。
//   - 等 `hub.Online(nodeID)==false` 也不行：detach 的顺序是**先**摘注册表（Online
//     立刻变 false）**再**写库（hub.go 的 detach：delete(sessions) → MarkNodeOffline
//     → MarkInstancesDead）。所以 Online==false 恰好落在「写库还没发生」的窗口里，
//     拿它当屏障等于把竞态留在原地。
//
// 唯一等价于「写库已发生」的观测点是 **ServeHTTP 已返回**。于是这里把 hub.ServeHTTP
// 包一层，在它返回时封盘一个 channel；cleanup 里等这个 channel。
//
// 这个屏障不是新发明：它是被删掉的 internal/server/wsfixture_test.go 里
// `nodeStreamFixture.settle` 的等价物（`git show 9305bac^:internal/server/wsfixture_test.go`），
// 只是从「一套带 store 播种 + 路由的大夹具」里剥出 server 包真正需要的那一半。
// 完整版（seal + 计数 + 拒绝新连接）见 internal/ws/node_test.go 的 settleGroup——
// 那个版本解决的是「settle 与后台连接 goroutine 并发进入」的问题，本包的两个调用点
// 都只在 cleanup 之前拨一条连接，不需要 seal 语义。

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/ws"
)

// wsSettleLimit 是等 hub.ServeHTTP 返回的上限。超时即判「会话没退干净」——这是失败，
// 不是 flake（照抄 #259 夹具的 15s 太长，本包单条会话退出是毫秒级，5s 足够宽松）。
const wsSettleLimit = 5 * time.Second

// serveHubWithSettle 把 hub.ServeHTTP 挂到 httptest.Server 上，并注册「handler 返回」
// 收尾屏障。
//
// cleanup 的**注册顺序**是硬要求（t.Cleanup 是 LIFO ⇒ 反向执行）：
//
//	conn.Close        （调用方在本函数之后注册 → 最先跑：让服务端读到 EOF）
//	srv.Close         （第二个跑：只关 HTTP 层，不等 hijack 连接的 handler）
//	barrier.wait      （第三个跑：等 hub.ServeHTTP 全部返回 = detach 的库写已完成）
//	db.Close          （夹具最先注册 → 最后跑）
//
// 于是「detach 写库」严格 happens-before「db.Close()」，postclose_guard_test.go 观察的
// `mark instances dead failed ... database is closed` 不再可能发生。
func serveHubWithSettle(t *testing.T, hub *ws.Hub) *httptest.Server {
	t.Helper()
	barrier := newHubSettle()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		barrier.enter()
		defer barrier.exit()
		hub.ServeHTTP(w, r)
	}))
	// 先注册「等 handler 返回」，再注册 srv.Close（LIFO：srv.Close 先跑）。
	t.Cleanup(func() { barrier.wait(t) })
	t.Cleanup(srv.Close)
	return srv
}

// hubSettle 追踪「还有几条 hub.ServeHTTP 没返回」。
//
// 为什么记两个数：entered 用来区分「根本没有会话」与「会话还没退」——调用方拨号失败时
// 用例已经 t.Fatalf 了，没必要再空等 wsSettleLimit；inFlight 才是屏障判据。
type hubSettle struct {
	entered  atomic.Int64 // 累计进入 handler 的次数
	inFlight atomic.Int64 // 已进入、未返回的 handler 条数
	once     sync.Once    // done 只关一次（多连接时不会二次 close panic）
	done     chan struct{}
}

// newHubSettle 造一个屏障。
func newHubSettle() *hubSettle { return &hubSettle{done: make(chan struct{})} }

// enter 登记一次 handler 进入（handler 的第一条语句，早于 Accept 写 101）。
func (s *hubSettle) enter() {
	s.entered.Add(1)
	s.inFlight.Add(1)
}

// exit 登记一次 handler 返回；最后一条返回时封盘。
func (s *hubSettle) exit() {
	if s.inFlight.Add(-1) == 0 {
		s.once.Do(func() { close(s.done) })
	}
}

// wait 等所有已进入的 handler 返回（超时即 t.Errorf，不 Fatalf：让其它 cleanup 照常跑完，
// 现场更完整）。没有会话进入过则立即返回——那时用例本身已经失败，空等只会拖慢失败。
//
// 边界（复用这个屏障时注意）：done 在 inFlight 归零时封盘，所以「封盘之后再拨一条连接」
// 不会被等。当前两个调用点都是「一个 srv 一条连接、拨号成功后才进 cleanup」，不触发该边界；
// 若将来在同一个 srv 上反复拨号，需要改成 ws 包 settleGroup 那样的 seal + 计数语义。
func (s *hubSettle) wait(t *testing.T) {
	t.Helper()
	if s.entered.Load() == 0 {
		return
	}
	select {
	case <-s.done:
	case <-time.After(wsSettleLimit):
		t.Errorf("hub.ServeHTTP 在 %s 内没有全部返回（in-flight=%d）：会话没退干净，"+
			"接下来 db.Close() 会与 hub.detach 的库写并发（#259 / #340 r2 M-2）",
			wsSettleLimit, s.inFlight.Load())
	}
}
