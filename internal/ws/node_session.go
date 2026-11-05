package ws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/zzjcool/anotify/internal/store"
)

// handleTaskFrame 处理 ack / progress / result / emit 四种任务帧（契约 §6.1）。
//
// 部署帧（apply）不在这里：它不是任务帧，走 handleConfigFrame。
//
// 四种帧都必须先 hello（身份确定后才有命名空间维度可用），
// 且都会续命（由 readLoop 的 touch 统一处理）。
//
// 作用域规则（#162）：
//   - 取任务用**节点的**命名空间（s.node.NamespaceID）——节点只能看见自己隔离域里的任务；
//   - 拿到任务之后一律用**任务自己的** task.NamespaceID（任务行是作用域的事实源）。
//
// 两者正常情况下相等；分成两步写的原因：若哪天两者不等（数据异常），
// 用 task.NamespaceID 写状态迁移至少还能对上行，不会因作用域错而恒 0 行。
func (s *nodeSession) handleTaskFrame(ctx context.Context, frame any) error {
	switch f := frame.(type) {
	case *AckFrame:
		return s.handleAck(ctx, f)
	case *ProgressFrame:
		return s.handleProgress(ctx, f)
	case *ResultFrame:
		return s.handleResult(ctx, f)
	case *EmitFrame:
		return s.handleEmit(ctx, f)
	default:
		s.sendError(ctx, ErrCodeUnknownType, fmt.Sprintf("unexpected frame %T", frame))
		return nil
	}
}

// handleConfigFrame 处理与任务无关的配置帧（apply / upgrade_ack）。
//
// 为什么与任务帧分开：它们不属于任何任务、不需要租约、也不推进九态机。
// 把配置帧塞进 handleTaskFrame 会让「任务帧都要先 hello」这条纪律变得含糊。
//
// 这条分支曾经缺失（端到端 e2e 抓到的 P0）：节点回的 apply 被当成未知帧，
// 中枢回 error{unknown_type} 并把结果丢弃 —— 于是**部署失败永远停在 pending**，
// 「这台机器没装 claude」这种最有价值的诊断信息根本到不了控制台。
func (s *nodeSession) handleConfigFrame(ctx context.Context, frame any) error {
	switch f := frame.(type) {
	case *ApplyFrame:
		return s.handleApply(ctx, f)
	case *UpgradeAckFrame:
		return s.handleUpgradeAck(ctx, f)
	default:
		s.sendError(ctx, ErrCodeUnknownType, fmt.Sprintf("unexpected frame %T", frame))
		return nil
	}
}

// handleUpgradeAck 落一次 agent 升级的结果（CONTRACT-agent-upgrade §5 / §5.1）。
//
// 成功：用 ack 里的 version + commit 更新 nodes，并把 agent_upgrade 置成 ok
// （**唯一**写 agent_commit 的路径）。
// 失败：把 agent_upgrade 置成 failed + 错误短码，**版本列保持旧值**——节点上的
// 二进制没换，把它改成「目标版本」会让控制台谎报升级成功。
//
// 两种情况都写一条日志（成功 INFO / 失败 WARN 带 node_id 与短码）：
// ack 是「控制台下发」这条链唯一的闭环证据，日志是排障时的第二份事实。
//
// ack 本身不是协议错误：节点按约定回报，中枢无论如何都保持连接。
func (s *nodeSession) handleUpgradeAck(ctx context.Context, f *UpgradeAckFrame) error {
	if !f.OK {
		code := NormalizeUpgradeError(f.Error)
		if err := s.h.Store.FailAgentUpgrade(ctx, s.node.NamespaceID, s.node.ID, code, s.h.now()); err != nil {
			// 落库失败也要让日志留下这次失败（WARN 已经说明了节点侧的原因）。
			slog.Warn("record agent upgrade failure failed",
				"event", "node.ws.upgrade.record_failed",
				"node_id", s.node.ID,
				"error", err.Error(),
			)
		}
		slog.Warn("agent upgrade failed on node",
			"event", "node.ws.upgrade.failed",
			"node_id", s.node.ID,
			"namespace_id", s.node.NamespaceID,
			"error", f.Error,
		)
		return nil
	}
	if f.Version == "" {
		// 成功却不报版本 = 节点实现 bug。回 bad_frame 而不是写一列空串：
		// 空串会被控制台读成「未知（老版本）」，与「刚升级成功」矛盾。
		s.sendError(ctx, ErrCodeBadFrame, "upgrade_ack with ok=true requires version")
		return nil
	}
	if err := s.h.Store.RecordAgentUpgrade(ctx, s.node.NamespaceID, s.node.ID, f.Version, f.Commit, s.h.now()); err != nil {
		return fmt.Errorf("record agent upgrade: %w", err)
	}
	slog.Info("agent upgraded",
		"event", "node.ws.upgrade.ok",
		"node_id", s.node.ID,
		"namespace_id", s.node.NamespaceID,
		"version", f.Version,
		"commit", f.Commit,
	)
	return nil
}

// handleApply 落一条部署结果（契约 CONTRACT-protocol §7）。
//
// 作用域一律用**节点的**命名空间：部署行只可能是这个命名空间里的，
// 帧里也没有命名空间字段（令牌绑命名空间，与 hello 同一条不变量）。
//
// 状态白名单由 store.MarkDeployResult 把关：节点不能把一条部署标成任意状态。
// 行不存在（部署已被撤销）不是协议错误 —— 撤销与回报天然会撞车，
// 此时节点只是"晚了一步"，丢掉结果即可。
func (s *nodeSession) handleApply(ctx context.Context, f *ApplyFrame) error {
	if f.DeployID == "" {
		s.sendError(ctx, ErrCodeBadFrame, "apply frame requires deploy_id")
		return nil
	}
	if f.State != ApplyStateApplied && f.State != ApplyStateFailed {
		s.sendError(ctx, ErrCodeBadFrame,
			fmt.Sprintf("apply frame has invalid state %q (want %s or %s)",
				f.State, ApplyStateApplied, ApplyStateFailed))
		return nil
	}

	err := s.h.Store.MarkDeployResult(ctx, s.node.NamespaceID, f.DeployID, f.State, f.Reason)
	if errors.Is(err, store.ErrNotFound) {
		slog.Warn("apply for unknown deployment",
			"event", "node.ws.apply.unknown_deployment",
			"node_id", s.node.ID, "deploy_id", f.DeployID, "state", f.State)
		return nil
	}
	if err != nil {
		return fmt.Errorf("mark deploy result: %w", err)
	}

	slog.Info("deployment applied",
		"event", "node.ws.apply",
		"node_id", s.node.ID,
		"deploy_id", f.DeployID,
		"capability", f.Capability,
		"version", f.Version,
		"digest", f.Digest,
		"state", f.State,
	)
	return nil
}

// rejectForeignExecution 是 #222 的 fencing 面：同一任务只许一条**活着的连接**推进状态。
//
// 为什么放在「任务是不是派给本节点」之前：跨连接的重复执行是更根本的错误，
// 而且要让执行者看到 lease_expired（协议语义：这条任务已经不归你了，丢掉本地副本），
// 而不是 invalid_transition（那会让人以为是帧写错了）。
// 命中时回 error 帧并返回 true：调用方直接结束，**不推进任何状态**。
//
// 归 nil（没人在飞）或就是本连接 → 放行。
func (s *nodeSession) rejectForeignExecution(ctx context.Context, taskID string) bool {
	cur := s.h.hub().owner(taskID)
	if cur == nil || cur == s {
		return false
	}
	s.sendError(ctx, ErrCodeLeaseExpired,
		fmt.Sprintf("task %s is being executed by another connection", taskID))
	return true
}

// handleAck 处理接单：assigned → acked。
func (s *nodeSession) handleAck(ctx context.Context, f *AckFrame) error {
	if f.TaskID == "" {
		s.sendError(ctx, ErrCodeBadFrame, "ack requires task_id")
		return nil
	}
	task, err := s.h.Store.GetTask(ctx, s.node.NamespaceID, f.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		s.sendError(ctx, ErrCodeBadFrame, fmt.Sprintf("unknown task %q", f.TaskID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("load task for ack: %w", err)
	}
	if s.rejectForeignExecution(ctx, f.TaskID) {
		return nil
	}
	// 只接受派给本节点的任务（防节点 ack 别人的任务）
	if task.NodeID == nil || *task.NodeID != s.node.ID {
		s.sendError(ctx, ErrCodeInvalidTransition,
			fmt.Sprintf("task %s is not assigned to this node", f.TaskID))
		return nil
	}
	if err := s.h.Store.Transition(ctx, task.NamespaceID, f.TaskID, store.TaskAssigned, store.TaskAcked); err != nil {
		s.reportTransitionError(ctx, f.TaskID, err)
		return nil
	}
	// ack 成功即开始计时：把租约推到 now + TTL（不再受 20s ack 窗口约束）
	now := s.h.now()
	if err := s.h.Store.ExtendLease(ctx, task.NamespaceID, f.TaskID, now+int64(s.h.leaseTTLSec())); err != nil {
		slog.Warn("extend lease on ack failed",
			"event", "node.ws.ack.extend_failed",
			"task_id", f.TaskID,
			"error", err.Error(),
		)
	}
	// 归属用**库里的** attempts 校正一次：publish 时记的世代可能已经过期（#222）。
	s.own(f.TaskID, task.Attempts)
	return nil
}

// handleProgress 处理进度：acked → running（重复 progress 幂等）。
func (s *nodeSession) handleProgress(ctx context.Context, f *ProgressFrame) error {
	if f.TaskID == "" {
		s.sendError(ctx, ErrCodeBadFrame, "progress requires task_id")
		return nil
	}
	task, err := s.h.Store.GetTask(ctx, s.node.NamespaceID, f.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		s.sendError(ctx, ErrCodeBadFrame, fmt.Sprintf("unknown task %q", f.TaskID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("load task for progress: %w", err)
	}
	if s.rejectForeignExecution(ctx, f.TaskID) {
		return nil
	}
	if task.NodeID == nil || *task.NodeID != s.node.ID {
		s.sendError(ctx, ErrCodeInvalidTransition,
			fmt.Sprintf("task %s is not assigned to this node", f.TaskID))
		return nil
	}
	// 一个事务内完成：→running + started_at + 续租（契约 §2.2，内部幂等：已在 running 时照做不报错）。
	// 行为变更（有意）：旧实现里「续租失败」只记 warn，现在合并成一个事务后
	// 失败即整体回滚并回错误帧——不会出现「已 running、租约却没续上」被巡检
	// 当过期任务回收、节点还在跑而中枢已重派的中间态。
	now := s.h.now()
	if err := s.h.Store.ProgressTask(ctx, task.NamespaceID, f.TaskID, now+int64(s.h.leaseTTLSec())); err != nil {
		s.reportTransitionError(ctx, f.TaskID, err)
		return nil
	}
	// 用库里的 attempts 校正归属世代（publish 时记的可能过期，#222）。
	s.own(f.TaskID, task.Attempts)
	return nil
}

// handleResult 处理结果帧，推进到终态。
//
// # 结果不再参与路由（P1.5 硬切之后的唯一语义）
//
// 这里**曾经**是两条腿：result.out 选出口 → 同步算下一跳计划 → 跟终态同一个事务
// 建子任务行（#140）。P1.5 把 out 整条删掉（迁移 28 又删了 `tasks.out` 列），
// 于是本函数只剩一件事：**把进程事实写进终态**（status / summary / artifacts），
// 不建任何子任务行。
//
// 那「下一跳」去哪了？全部在 emit（docs/design/typed-pipeline.md §2）：
//   - 运行中 emit → route.Emit 在**自己的事务**里写 emission + 建子任务行 + 派发；
//   - 没有 emit → 到此为止（0 次 emit 合法，正常终止）；
//   - 失败前 emit 过 → 那些下游照样在（I4 的形式保留：路由与 status 无关）。
//
// 为什么不能“两者共存”（保留 result.out 兼容旧节点）：两套产出通道必然分叉——
// 同一个任务可能既 emit 过 fix、又在退出时写了一个 clean，谁算数、重复怎么去重
// 都没有定义。而且 #140 的「终态 + 子任务行同一个事务」这一套补偿逻辑会多出
// 第二条入口（一条不写 emission 的入口），去重表就成了摆设。
//
// 于是顺序简化成（序号沿用历史编号，便于对照改动前的注释）：
//
//	① 参数校验 / GetTask / 归属校验
//	④ 一个事务提交父任务终态（ReportResultAndCreateTasks，next 恒 nil）
//	⑤ 记 "task finished"，然后通知槽位空出（#219）
func (s *nodeSession) handleResult(ctx context.Context, f *ResultFrame) error {
	if f.TaskID == "" {
		s.sendError(ctx, ErrCodeBadFrame, "result requires task_id")
		return nil
	}
	if f.Status != store.TaskSucceeded && f.Status != store.TaskFailed {
		s.sendError(ctx, ErrCodeBadFrame,
			fmt.Sprintf("result status must be succeeded|failed, got %q", f.Status))
		return nil
	}
	task, err := s.h.Store.GetTask(ctx, s.node.NamespaceID, f.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		s.sendError(ctx, ErrCodeBadFrame, fmt.Sprintf("unknown task %q", f.TaskID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("load task for result: %w", err)
	}
	if s.rejectForeignExecution(ctx, f.TaskID) {
		return nil
	}
	if task.NodeID == nil || *task.NodeID != s.node.ID {
		s.sendError(ctx, ErrCodeInvalidTransition,
			fmt.Sprintf("task %s is not assigned to this node", f.TaskID))
		return nil
	}

	// ④ 一个事务提交父任务终态。
	//
	// next 恒 nil：路由不在这条路径上（见函数头）。这个参数保留是为了不动
	// store 的事务语义（「终态 + 子任务行同生共死」，见 report_next_test），
	// 而 ws 这一侧从此只写终态。
	if err := s.h.Store.ReportResultAndCreateTasks(ctx, task.NamespaceID, f.TaskID, f.Status,
		f.Summary, f.Artifacts, f.Error, nil); err != nil {
		// 终态没落库。若拒绝是「这条任务已经不归我们了」（被巡检回收/已重派/已过期），
		// 就不该再替它续租，也不该继续占着 hub 的在飞位（#218/#222）。
		if errors.Is(err, store.ErrInvalidTransition) || errors.Is(err, store.ErrNotFound) {
			s.forget(f.TaskID)
		}
		s.reportTransitionError(ctx, f.TaskID, err)
		return nil
	}
	// 终态已落库：本连接与这条任务再无关系（不再续租、不再占在飞位）。
	s.forget(f.TaskID)

	// ⑤ 记 "task finished"。
	//
	// 槽位释放已并入同一个事务（契约 §2.3 第 8 条），此处不再单独调用。
	slog.Info("task finished",
		"event", "node.ws.result",
		"task_id", f.TaskID,
		"node_id", s.node.ID,
		"status", f.Status,
	)

	// ⑤.5 槽位刚刚空出来 → 立刻拉一次 pending（#219）。
	//
	// 放在事务提交之后：槽位释放已随终态一起 COMMIT，此刻队列里若有排队的
	// pending，正是最该被派出去的时刻。
	// 钩子本身必须**非阻塞**（Nudge 是可合并的通道投递），所以这里同步调用也不会拖慢
	// result 帧的处理。
	if s.h.OnCapacityFreed != nil {
		s.h.OnCapacityFreed()
	}
	return nil
}

// reportTransitionError 把状态机拒绝翻译成协议错误帧。
//
// 三条关键映射：
//   - 租约已失效 → lease_expired（节点该丢弃本地任务）
//   - params 超限 → params_too_large（审查 m5）
//   - 其他非法迁移 → invalid_transition
//
// 注（P1.5）：result 路径已经不再建子任务行，所以 params_too_large 的最常见
// 触发点（下一跳 params 超过 store.MaxTaskParamsBytes）已经搬到 **emit**：
// 那条错误走 emit_ack.error，不走 error 帧（emit 的拒收是业务动作失败，
// 不是「帧写坏了」，见 node_emit.go 的两条硬纪律）。这里的分支保留为
// 兼底映射：任何从终态写入泄露出来的同族错误仍然不会被伪装成状态机拒绝。
func (s *nodeSession) reportTransitionError(ctx context.Context, taskID string, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidTransition):
		s.sendError(ctx, ErrCodeInvalidTransition, err.Error())
	case errors.Is(err, store.ErrNotFound):
		s.sendError(ctx, ErrCodeInvalidTransition,
			fmt.Sprintf("task %s not found (may have been reclaimed)", taskID))
	case errors.Is(err, store.ErrParamsTooLarge):
		// 审查 m5：params 超限不该伪装成状态机拒绝。文案带原来的 err.Error()：
		// taskID/体积/上限/修法全在里头。
		s.sendError(ctx, ErrCodeParamsTooLarge, err.Error())
	default:
		s.sendError(ctx, ErrCodeInvalidTransition, err.Error())
	}
}
