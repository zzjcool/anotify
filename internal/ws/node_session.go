package ws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// handleTaskFrame 处理 ack / progress / result 三种任务帧（契约 §6.1）。
//
// 部署帧（apply）不在这里：它不是任务帧，走 handleConfigFrame。
//
// 三种帧都必须先 hello（身份确定后才有命名空间维度可用），
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
	default:
		s.sendError(ctx, ErrCodeUnknownType, fmt.Sprintf("unexpected frame %T", frame))
		return nil
	}
}

// handleConfigFrame 处理与任务无关的配置帧（当前只有 apply）。
//
// 为什么与任务帧分开：apply 不属于任何任务、不需要租约、也不推进九态机。
// 把它塞进 handleTaskFrame 会让「任务帧都要先 hello」这条纪律变得含糊。
//
// 这条分支曾经缺失（端到端 e2e 抓到的 P0）：节点回的 apply 被当成未知帧，
// 中枢回 error{unknown_type} 并把结果丢弃 —— 于是**部署失败永远停在 pending**，
// 「这台机器没装 claude」这种最有价值的诊断信息根本到不了控制台。
func (s *nodeSession) handleConfigFrame(ctx context.Context, frame any) error {
	switch f := frame.(type) {
	case *ApplyFrame:
		return s.handleApply(ctx, f)
	default:
		s.sendError(ctx, ErrCodeUnknownType, fmt.Sprintf("unexpected frame %T", frame))
		return nil
	}
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

// handleResult 处理结果帧，推进到终态（不变量 I4/I5）。
//
// 三态由 ResultFrame.Reported() 判定（自定义 UnmarshalJSON 区分「字段缺失」与「显式 null」）：
//
//	out 缺失    → reported=false（节点没回报出口）→ 不派下一跳
//	"out":null  → reported=true,  out=nil        → 到头了（不派）
//	"out":"fix" → reported=true,  out=&"fix"     → 走 fix
//
// #140 起的顺序（顺序本身就是不变量，不要调整）：
//
//	① 参数校验 / GetTask / 归属校验
//	② 算 reported / out
//	③ **同步**算出下一跳计划（PlanNextHop，只读）—— 失败即不提交终态
//	④ 一个事务提交「父任务终态 + 下一跳子任务行」（ReportResultAndCreateTasks）
//	⑤ 事务提交后记 "task finished"（先记日志再派发，与改动前一致）
//	⑥ 异步 publish 这些**已落库**的行（PublishTasks）—— 在事务之外，失败只记日志
//
// 为什么 ③ 在 ④ 之前：先有计划才能把子任务行与终态一起塞进同一个事务（#140）。
// 代价是 plan 从后台挪到了读循环里——它全是只读查询，可以接受（见 ③ 处注释）。
// ③ 失败 → 终态也不提交：那是可恢复的；提交了终态却没子任务行才是永久空洞。
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

	reported := f.Reported()
	// 只有 reported 时才把 out 传下去；缺失时传 nil（store 会写 reported=0, out=NULL）
	var out *string
	if reported {
		out = f.Out
	}

	// ③ 同步算下一跳计划（契约 §4 的递归应用）。
	//
	// 只在**成功**且有出口时派：失败的任务不该继续往下走
	// （它的产物不可信），out 为 nil 时是"到头了"，也不派。
	//
	// 为什么 plan 同步跑在读循环里（而不是像旧的 dispatchNext 那样整段丢进后台）：
	// plan 全是只读查询（出口表/能力/节点），真正的网络操作在 ⑥ 的 publish。
	// 而「父任务终态」与「下一跳子任务行」必须同一个事务提交——要进事务就得
	// 先有计划，所以 plan 只能同步。成本是读循环里多几次本地 SQLite 查询。
	// 剪枝留痕（InsertPruneEvent）在事务外、best-effort：若随后的事务回滚，库里
	// 可能留下一条「这条边没走成」的痕迹——诊断信息，可接受（契约 §4）。
	//
	// plan 失败 → **不提交终态**，回错误帧。
	// 理由是 #140 的核心：一旦「父任务 succeeded 而子任务行不存在」被提交，那个
	// 空洞就是**永久**的（ListRedispatchable 只捞已存在的 pending 行，捞不到一条
	// 不存在的行）——链悄悄停掉，trace 看起来还正常。
	// 而不提交是可恢复的：父任务仍 running，租约到期后巡检把它退回 pending 重派，
	// 节点重新执行一次（能力要求幂等）。
	// 这类错误只在 DB 故障时出现，与「下一跳 publish 失败」是两回事——后者父任务
	// 已经成功、子任务行已落库，绝不能回退（见 ⑥）。
	var next []*store.Task
	if s.h.NextHop != nil && f.Status == store.TaskSucceeded && out != nil {
		// 出口表/能力/节点都按命名空间隔离：作用域必须是**父任务的命名空间**
		// （即 task.NamespaceID，不是节点的 id）。传错的话 EdgesFrom 恒 0 行，
		// 链路会在父任务成功后静默停掉。
		next, err = s.h.NextHop.PlanNextHop(ctx, task.NamespaceID, task.TraceID, task.ID,
			task.CapabilityID, *out, task.Params, task.Path)
		if err != nil {
			s.reportTransitionError(ctx, f.TaskID, err)
			return nil
		}
	}

	// ④ 一个事务：父任务终态 + 下一跳子任务行（#140）。
	//
	// 拆成两次提交时，进程在中间退出就会留下「父任务 succeeded + out=fix、
	// 子任务行不存在」的永久空洞。同一个 BEGIN/COMMIT 里，子任务插入失败则整体回滚
	// ——父任务不留在终态（宁可让父任务被巡检退回重派、幂等重执行一次，也胜于断链）。
	if err := s.h.Store.ReportResultAndCreateTasks(ctx, task.NamespaceID, f.TaskID, f.Status, out, reported,
		f.Summary, f.Artifacts, f.Error, next); err != nil {
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
	// 补偿路径（#140 成立的前提，别删）：走到这里时 next 里的行已经和终态一起
	// COMMIT，形状是 status='pending' + node_id IS NULL + attempts=0 —— 正是
	// store.ListRedispatchable 的三个筛选条件。所以即使下面 ⑥ 的 goroutine 随进程
	// 消失、或 PublishTasks 返回错误，任务也不会丢：internal/lease 的 sweeper
	// （每 5s tick，Redispatch = Dispatcher.RedispatchTask）会把它捞回来重派。
	// 这也是 publish 敢于放在事务外（且失败只 warn）的全部依据。

	// ⑤ 记 "task finished"（在事务提交之后、publish 之前）。
	//
	// 槽位释放已并入同一个事务（契约 §2.3 第 8 条），此处不再单独调用。
	slog.Info("task finished",
		"event", "node.ws.result",
		"task_id", f.TaskID,
		"node_id", s.node.ID,
		"status", f.Status,
		"reported", reported,
		"out", outString(out),
	)

	// ⑥ 事务提交后异步 publish 已落库的子任务行；失败只记日志、不回给节点。
	//
	// 幂等兜底见上方的补偿路径：行已 pending + node_id NULL，sweeper 会捞。
	if len(next) > 0 {
		s.publishNext(ctx, task.NamespaceID, task.ID, next)
	}
	return nil
}

// publishNext 事务提交后异步推送下一跳（不阻塞这条连接的读循环）。
//
// 函数体里**只允许**调用 PublishTasks：任务行已经在 ReportResultAndCreateTasks
// 的事务里落库了，这里再建行会重复建，「再查一次出口表」也会和已提交的计划不一致
// ——计划是事务前算好的，publish 只负责「让这些行跑起来」。
//
// 异步的理由：publish 要挑机器、占槽位、推 assign 帧，耗时可能到几十毫秒；
// 在读循环里同步做会卡住同一节点上其他任务的 ack/result 处理。
//
// 失败只 warn、**不把错误回给节点**：这个任务本身已经成功完成，下一跳的失败
// 不该让它变成失败（否则节点会重试、结果重复落库）。
//
// 补偿路径：行已是 pending + node_id NULL + attempts=0，即使本 goroutine 随进程
// 消失、或 PublishTasks 返回错误，store.ListRedispatchable（internal/lease sweeper
// 每 5s tick，Redispatch = Dispatcher.RedispatchTask）也会把它捞回重派。
// namespaceID 是父任务的命名空间（子任务行由 store 强制继承父任务的命名空间，I11④）。
func (s *nodeSession) publishNext(ctx context.Context, namespaceID, parentTaskID string, tasks []*store.Task) {
	go func() {
		// 用 WithoutCancel：连接断开不该让已经完成的任务丢失下一跳。
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := s.h.NextHop.PublishTasks(bg, namespaceID, tasks); err != nil {
			slog.Warn("publish next hop failed",
				"event", "node.ws.result.next_hop_failed",
				"parent_task_id", parentTaskID,
				"tasks", len(tasks),
				"error", err.Error(),
			)
		}
	}()
}

// reportTransitionError 把状态机拒绝翻译成协议错误帧。
//
// 三条关键映射：
//   - 租约已失效 → lease_expired（节点该丢弃本地任务）
//   - params 超限 → params_too_large（审查 m5，见下）
//   - 其他非法迁移 → invalid_transition
func (s *nodeSession) reportTransitionError(ctx context.Context, taskID string, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidTransition):
		s.sendError(ctx, ErrCodeInvalidTransition, err.Error())
	case errors.Is(err, store.ErrNotFound):
		s.sendError(ctx, ErrCodeInvalidTransition,
			fmt.Sprintf("task %s not found (may have been reclaimed)", taskID))
	case errors.Is(err, store.ErrParamsTooLarge):
		// 审查 m5：params 超限不该伪装成状态机拒绝。典型触发是「下一跳子任务 params
		// 太大 → 父任务终态整体回滚」，而 invalid_transition 会把病因埋掉（节点只会
		// 重跑同一份昂贵工作）。文案带原来的 err.Error()：taskID/体积/上限/修法全在里头。
		s.sendError(ctx, ErrCodeParamsTooLarge, err.Error())
	default:
		s.sendError(ctx, ErrCodeInvalidTransition, err.Error())
	}
}

// outString 把 out 指针转成日志可读形式（nil → "<none>"）。
func outString(out *string) string {
	if out == nil {
		return "<none>"
	}
	return *out
}
