package ws

import (
	"context"
	"log/slog"

	"github.com/zzjcool/anotify/internal/route"
)

// emit 帧的处理（P1，docs/design/emit-protocol.md §3.1 / §3.2 / §7）。
//
// 帧层只做三件事：形状校验、调一次路由、把结果（或拒收原因）写成 emit_ack。
// 状态机、seq 连续性、幂等、(task_id,seq) 唯一约束、建子任务行全在 route/store。
//
// 两条硬纪律（都是「不要制造重连风暴」的推论）：
//
//  1. **拒收一律走 emit_ack.error**，不回 error 帧、更不断连接。
//     emit 是「一次可能被拒的业务动作」（seq 缺口 / 任务已终结 / 内容冲突），
//     而 error 帧在协议里表示「你这条帧写错了」。混用会让节点把一次正常的业务
//     拒收当成协议故障去重连——重连后 outbox 重发、又被拒、又重连。
//  2. 处理失败**不返回 error**（返回 error 会让 readLoop 结束 → 连接关掉）。
//     连写 emit_ack 失败也只记日志：那条连接自己会读出错，不需要这里补一刀。
//
// 并发与乱序：同一节点的两帧可能乱序到达（不同 goroutine / 不同连接），
// 判决只看 (task_id, seq) 与库里事实，不假设先后。seq=2 先到会被判成缺口
// （回 error，节点重发 1），seq 重复到达会被折成首次结果（不重复派发）。

// handleEmit 处理一帧 emit。
func (s *nodeSession) handleEmit(ctx context.Context, f *EmitFrame) error {
	// 形状校验：缺 task_id / seq / port 的帧不该进路由层（那里要读库）。
	// 这是**帧**的问题，但仍然走 emit_ack.error：节点据此把等价错误回给 CLI，
	// 不必重连。
	if f.TaskID == "" || f.Seq <= 0 || f.Port == "" {
		s.sendEmitAck(ctx, f.TaskID, f.Seq, 0, 0,
			"emit requires task_id, seq >= 1 and a non-empty port")
		return nil
	}
	if s.h.Emit == nil {
		// 未装配（测试 / 只跑派发不跑 emit 的部署）：明确拒，而不是静默丢弃。
		// 静默丢弃的后果是 CLI 干等超时，且运维看不出「服务端根本没开这个功能」。
		s.sendEmitAck(ctx, f.TaskID, f.Seq, 0, 0, "emit is not enabled on this server")
		return nil
	}

	// 归属栅栏（#222 的同族约束）：同一任务只许一条**活着的连接**推进它。
	// 与 ack/progress/result 不同的是，这里的拒收走 emit_ack.error（不回 error 帧）——
	// emit 的失败语义是「这次产出不收」，不是「你这条帧写错了」，混用会诱发重连风暴。
	if cur := s.h.hub().owner(f.TaskID); cur != nil && cur != s {
		s.sendEmitAck(ctx, f.TaskID, f.Seq, 0, 0,
			"task is being executed by another connection")
		return nil
	}

	out, err := s.h.Emit.Emit(ctx, route.EmitInput{
		// 作用域用**节点的**命名空间：一条连接 = 一个节点 = 一个命名空间（I10/I11），
		// 帧里不带命名空间（令牌即作用域）。
		NamespaceID: s.node.NamespaceID,
		TaskID:      f.TaskID,
		Seq:         f.Seq,
		Port:        f.Port,
		// To 是脚本点名的下游（`emit --to`）。空 = 让中枢按候选选；
		// 点名了但不在候选里 → route 拒收，节点看到 emit_ack.error。
		To:   f.To,
		Data: f.Data,
		Note: f.Note,
		// 归属校验的另一半：库里记着「这条任务派给了谁」，这里把「谁在报」带下去。
		// 只靠内存栅栏不够——两个节点先后连过、前者已断开时，内存里没有 owner 了。
		NodeID: s.node.ID,
	})
	if err != nil {
		slog.Warn("emit rejected",
			"event", "node.ws.emit.rejected",
			"node_id", s.node.ID,
			"namespace_id", s.node.NamespaceID,
			"task_id", f.TaskID,
			"seq", f.Seq,
			"port", f.Port,
			"error", err.Error(),
		)
		s.sendEmitAck(ctx, f.TaskID, f.Seq, 0, 0, err.Error())
		return nil
	}

	slog.Info("emit routed",
		"event", "node.ws.emit",
		"node_id", s.node.ID,
		"namespace_id", s.node.NamespaceID,
		"task_id", out.TaskID,
		"seq", out.Seq,
		"port", f.Port,
		"routed", out.Routed,
		"pruned", out.Pruned,
		"duplicate", out.Duplicate,
		// note 只进日志，**不参与路由**（emit-protocol §3.1）。
		// 它是一条日志通道，不是第二个 payload：所以库里没有它的列。
		"note", f.Note,
	)
	s.sendEmitAck(ctx, out.TaskID, out.Seq, out.Routed, out.Pruned, "")
	return nil
}

// sendEmitAck 下发一帧 emit_ack（errMsg 非空 = 拒收）。
//
// 写失败只记日志、不返回错误：写不出去说明这条连接已经坏了，读循环下一次 Read
// 就会报错并走正常收尾；这里返回 error 只会提前一刀，且与「拒收」混成一团。
func (s *nodeSession) sendEmitAck(ctx context.Context, taskID string, seq int64, routed, pruned int, errMsg string) {
	frame := &EmitAckFrame{
		Type:   NodeFrameEmitAck,
		TaskID: taskID,
		Seq:    seq,
		Routed: routed,
		Pruned: pruned,
		Error:  errMsg,
	}
	if err := s.send(ctx, frame); err != nil {
		slog.Warn("send emit_ack failed",
			"event", "node.ws.emit.ack_failed",
			"node_id", s.node.ID,
			"task_id", taskID,
			"seq", seq,
			"error", err.Error(),
		)
	}
}
