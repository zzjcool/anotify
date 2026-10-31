package ws

// 本文件是「连接名下的任务归属」与「连接为什么结束」的**唯一事实源**（#218 / #222）。
//
// 为什么单独成文件：这两件事都是跨路径共享的策略——
//   - 归属由 PublishAssign（派发 goroutine）写、由 ack/progress/result（读循环）校正、
//     由心跳与 cleanup 读；
//   - 结束原因由看门狗、被新连接顶掉、上行帧超限三条路径**先标**，读循环随后的读错误
//     只能补空、不能覆盖。
//
// 策略集中一处才不会互相打架（典型事故：看门狗标了 unclean，readLoop 的读错误又把它
// 判成 clean → 对端进程还活着却把任务重派了 → 同一个任务被两个进程执行，即 #222）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"syscall"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 会话归属（#218 的根修：续租/释放的粒度是「连接名下的任务」） ----------

// own 把一条任务登记到本连接名下。
//
// gen 是**库里的** attempts（派发世代，LeaseAndAssign 每次派发 +1）：
// cleanup 用它做 CAS，防止把「我不在的时候被巡检回收并重派」的任务状态改回去。
// 到处都传库里的权威值：publish 时记的可能已经过期（#218/#222 的 fencing 面）。
func (s *nodeSession) own(taskID string, gen int) {
	if taskID == "" {
		return
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if s.owned == nil {
		s.owned = make(map[string]int, 4)
	}
	s.owned[taskID] = gen
}

// disown 把一条任务从本连接名下移除（结果已落库 / 这条任务已经不归我们）。
func (s *nodeSession) disown(taskID string) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	delete(s.owned, taskID)
}

// ownedOwners 快照本连接名下的任务（按 TaskID 排序，确定性）。
//
// 返回副本：调用方（心跳续租 / cleanup 释放）会把快照交给 store，
// 不该让它看到后续 own/disown 造成的变更（那些变更属于别的帧）。
func (s *nodeSession) ownedOwners() []store.TaskOwner {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if len(s.owned) == 0 {
		return nil
	}
	out := make([]store.TaskOwner, 0, len(s.owned))
	for id, gen := range s.owned {
		out = append(out, store.TaskOwner{TaskID: id, Attempts: gen})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out
}

// forget 把一条任务从「本连接名下」与「hub 的在飞位」里一起摘掉。
//
// 两处必须同步：只 disown 会让别的连接永远拿不到这条任务（在飞位挡着，#222）；
// 只 release 会让这条连接继续替一条不归它的任务续租（#218）。
func (s *nodeSession) forget(taskID string) {
	s.disown(taskID)
	// release 必须带上自己（审查 n4）：它只删「在飞登记 == 我」的条目。
	s.h.hub().release(taskID, s)
}

// ---------- 结束原因（#222 的前提：知道对端是死是活） ----------

// markEnd 记录连接**为什么**结束（first wins：后到的标记不覆盖先到的）。
//
// **必须先标后关**：看门狗心跳超时、被新连接顶掉、上行帧超限这三条路径都要在
// `Close` 之前调用它。否则读循环随后读到的错误是「我们自己的关闭」的产物，
// 会被归类成 clean → 任务立刻重派 → 而那个进程可能还在跑手上的命令（#222 双执行）。
//
// 反过来，对端**发了** close 帧（1000/1001/1005）时没人抢在前面标，
// 读错误会补上一个 clean 标记；而对端不发 close 帧就断流（EOF / RST）时
// 补的是 unclean——那个进程可能还活着（见 classifyReadError 的裁决依据）。
func (s *nodeSession) markEnd(clean bool, reason string) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if s.endMarked {
		return
	}
	s.endMarked = true
	s.endClean = clean
	s.endReason = reason
}

// endState 返回结束标记。未标记时返回 (false, true, "")：
// 调用方（读循环收尾）据此用**读错误**决定 clean 与否。
func (s *nodeSession) endState() (ended bool, clean bool, reason string) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if !s.endMarked {
		return false, true, ""
	}
	return true, s.endClean, s.endReason
}

// endFromReadError 在读循环结束后补齐结束标记（已被更早的路径标过就什么都不做）。
func (s *nodeSession) endFromReadError(err error) {
	if ended, _, _ := s.endState(); ended {
		return
	}
	clean, reason := classifyReadError(err)
	s.markEnd(clean, reason)
}

// classifyReadError 是「读循环为什么结束」→ clean / unclean 的**唯一判据表**。
//
// # 裁决依据（本轮 M4 重裁，owner 已定；以下两条是代码事实，不是假设）
//
//   - cmd/agent/runner.go:129 `SysProcAttr{Setpgid: true}` 起子进程，cmd.Cancel =
//     killProcessGroup（:134）杀的是**整个进程组**；
//   - cmd/agent/client.go:207-212 的优雅退出是 `drain(grace)`（取消 ctx 并掐掉在跑
//     任务的进程组）**之后**才 `conn.Close(1000)`。
//
// 所以只有「对端发了 close 帧」才能证明子进程组已经被干掉：
//
//	收到 close 帧（1000/1001/1005）⇒ 对端走的是优雅退出 ⇒ 子进程组已 SIGKILL
//	                            ⇒ clean → LostRequeue 安全（对所有能力都安全，包括非幂等；
//	                              仍然受 M1 的 !idempotent → abandoned 与 attempts 预算约束）
//	没有 close 帧（io.EOF / ECONNRESET / EPIPE，CloseStatus()==-1）
//	                            ⇒ 进程是被强杀的（kill -9 / OOM）或网络断了
//	                            ⇒ 子进程**可能还在跑**（Setpgid 且没有 Pdeathsig）
//	                            ⇒ unclean → LostAbandon
//
// clean（对端确实发了 close 帧）：
//
//	1000/1001 对端正常关闭；1005 收到一帧空 body 的 close（对端**确实发了** close）
//
// clean（**不是**对端判断，是中枢自己这一侧）：
//
//	nil（上层收尾结束了读循环）、context.Canceled/DeadlineExceeded（中枢退出；
//	重启后继续 —— 节点侧一旦发现连接坏了会 cancelAll() 杀掉进程组，见 client.go:224）
//
// unclean（对端可能还活着 → 绝不重派，交给 LostAbandon）：
//
//	io.EOF / ECONNRESET / EPIPE（对端 vanished without a close frame）、
//	net.ErrClosed（本地已关闭，不证明对端消失）、
//	1009（对端活着，只是在拒绝我们的帧，见 #221）、其它任何 close code、一切无法归类的错误
//
// **故意保留的取舍（#222 at-most-once）**：这一改会让「agent 被 kill -9」时 in-flight
// 任务落到 abandoned（+ `executor_lost: <reason>`）——因为被 SIGKILL 的 agent 没有机会
// 去杀 Setpgid 起出来的进程组，那条命令**可能还在跑**并把副作用做完。
// 宁可让用户看到 abandoned，也不要让同一个任务被两个进程并发执行两遍。
//
// **为什么 1005 算 clean 而 1006 不在表里**（上一轮 M4 已核，以 coder/websocket
// v1.8.15 源码为准）：
//   - 1006（StatusAbnormalClosure）**根本不可能由 Read 返回**：它只由 wasm 分支构造，
//     且被 validWireCloseCode 拒收。表中不为它留位置（用例里单列一条防御）。
//   - 1005（StatusNoStatusRcvd）只在**收到一帧 body 为空的 close 帧**时出现——对端发了
//     close（对端主动收工）→ clean 合理。
//
// 默认偏向 unclean 是**有意的**：#222 的取舍是「宁可让用户看到 abandoned + executor_lost，
// 也不要静默把同一个任务并发执行两遍」。
func classifyReadError(err error) (clean bool, reason string) {
	if err == nil {
		return true, "connection closed"
	}
	// 中枢退出（server shutdown / 上层 context 取消）：重启后应当重排。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true, "hub shutting down: " + err.Error()
	}
	switch code := websocket.CloseStatus(err); code {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		// 1000/1001：对端**发了** close 帧 → 走的是 client.go 的优雅退出（先 drain
		// 掐掉进程组，再关连接）→ 子进程确定已死 → 重排安全。
		return true, fmt.Sprintf("peer closed (%d): %v", code, err)
	case websocket.StatusNoStatusRcvd:
		// 1005：收到了 close 帧，只是帧里没带状态码。对端发了 close = 它主动收工。
		return true, fmt.Sprintf("peer closed (%d): %v", code, err)
	case websocket.StatusMessageTooBig:
		// 1009：对端（agent）自己的读上限炸了。它**活着**，只是在拒绝我们的帧。
		// 重派等于让它再炸一次，还可能并发执行（#221 的 1MiB 路径）。
		return false, "peer exceeded its read limit (1009): " + err.Error()
	}
	// 对端**没有发 close 帧**就断了。库在这种情形下不返回 close code（-1），
	// 只会把 socket 的结局透出来：流结束（EOF）或内核重置（RST/EPIPE）。
	// 这两种都**不能**证明对端进程死了（kill -9 走的就是这里，而它没机会杀进程组），
	// 所以一律 unclean：放弃而不是重排（#222）。
	if errors.Is(err, io.EOF) {
		return false, "peer vanished without a close frame: process may still be running (EOF)"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && (errors.Is(opErr.Err, syscall.ECONNRESET) || errors.Is(opErr.Err, syscall.EPIPE)) {
		// ECONNRESET/EPIPE：对端被 kill -9 / OOM / 网络断（也可能来自中间盒/NAT/LB
		// 摘除），socket 没了但对端的进程组可能还活着 → 同样不能重排。
		return false, "peer vanished without a close frame: process may still be running (ECONNRESET/EPIPE)"
	}
	// 我们（或库）自己把 socket 关了：读线程看到的「已关闭」并不证明对端消失。
	// 注意它必须排在上面两条**之后**：主动关闭时错误链里也可能带 EOF/RST 字样。
	if errors.Is(err, net.ErrClosed) {
		return false, "connection closed locally: " + err.Error()
	}
	return false, "unclean disconnect: " + err.Error()
}

// lostPolicy 把结束标记翻译成「失去执行者时怎么处理」（clean → 重排，unclean → 放弃）。
func (s *nodeSession) lostPolicy() store.LostPolicy {
	_, clean, _ := s.endState()
	if clean {
		return store.LostRequeue
	}
	return store.LostAbandon
}
