package ws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// 负向帧断言（#255 收口）：**只有一条** assertNoFrame。
//
// 它的主语与改前的「等一会儿看有没有帧」**完全一致**（任何帧都算失败），
// 唯一区别是：连接早死不再被当成通过。
//
// 为什么不能继续用 `tryReadFrame` 表达「对端不该收到帧」：
//
// coder/websocket@v1.8.15 的读超时会**关掉整条连接** —— conn.go:188-199：
//
//	func (c *Conn) setupReadTimeout(ctx context.Context) bool {
//		if ctx.Done() == nil { return false }
//		stop := context.AfterFunc(ctx, func() { c.clearReadTimeout(); c.close() })
//		...
//	}
//
// 即 `Read(ctx)` 的 ctx 一到期就 `c.close()`（read.go:254-257 的 finishRead 再把
// 错误归一成 ctx.Err()）→ 客户端发 FIN → 服务端 readLoop 读到 EOF → `cleanup()` →
// `hub.Unregister`。所以「等 d 看有没有东西来」这种负向断言同时有两个毛病：
//
//	(a) 永真风险：连接在窗口开始之前就已经死了（例如上一次负向读把它关了），
//	    读什么都读不到 ⇒ 断言照样通过，其实什么都没证明；
//	(b) 竞态：窗口结束时连接**一定**已经死了，窗口之后任何「会话还在不在」的
//	    状态断言都在和服务端 cleanup 赛跑。CI 上跑输的那次就是
//	    node_lease_fix3_test.go 的 `hub().current(nodeID) == nil`（run 41200397150）。
//
// assertNoFrame 用「只有窗口用尽才算通过，连接早死一律红」干掉 (a)；
// (b) 只能靠摆放位置解决 ⇒ **只能放在用例的最后一步**。

// assertNoFrame 在窗口 d 内断言对端**一个下行帧都没发来**——主语与改前那条
// `if frame, ok := tryReadFrame(conn, 500ms); ok { t.Fatal(...) }` **完全一致**
// （任何帧都算失败），区别只在「连接早死」不再被当成通过。
//
// 为什么**不**把主语收窄成「本 task 的 assign 帧」（审查意见）：产品下行帧目前有
// welcome / assign / cancel / error（node_protocol.go），负向用例里它们**都不该出现**；
// 收窄主语会白白丢掉 cancel / 重复 welcome / error 这一片检测面。所以主语仍是
// 「任何帧」，welcome 也不排除（hello() 已经消费过那唯一一条，再来一条就是 bug）。
//
// 判定（只有一条通过路径）：
//   - WebSocket 读成功 ⇒ 对端确实发了帧 ⇒ t.Fatalf（附帧原文与 why）。
//   - ctx 到期（context.DeadlineExceeded）⇒ 窗口用尽 **且连接在窗口内是活的** ⇒ 通过。
//   - 其它错误（net.ErrClosed / io.EOF / close 帧 / …）⇒ t.Fatalf：
//     负向断言不成立：读不到帧既可能是没发，也可能是连接早死了。
//
// 调用之后**不得再用这条 conn，也不得再做任何依赖「会话还活着」的断言**：
// 库在窗口结束时会 c.close()（conn.go:188-199），连接此刻已经死了，服务端的
// cleanup / hub.Unregister 随时会跑完。
func assertNoFrame(t *testing.T, conn *websocket.Conn, d time.Duration, why string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err == nil {
		t.Fatalf("peer received a frame within %v, want none（%s）: %s", d, why, data)
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		// 窗口用尽：连接在窗口内是活的，只是没有帧来（此刻库已把它关了）。
		return
	}
	t.Fatalf("负向断言不成立：读不到帧既可能是没发，也可能是连接早死了"+
		"（err = %v, 窗口 %v, %s）：连接已死时本断言什么都不证明——"+
		"改用状态断言（hub 的在飞表 / 会话归属）或先修连接", err, d, why)
}
