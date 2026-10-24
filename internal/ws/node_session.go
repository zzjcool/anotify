package ws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/zzjcool/anotify/internal/store"
)

// handleTaskFrame 处理 ack / progress / result 三种任务帧（契约 §6.1）。
//
// 三种帧都必须先 hello（身份确定后才有 org 维度可用），
// 且都会续命（由 readLoop 的 touch 统一处理）。
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

// handleAck 处理接单：assigned → acked。
func (s *nodeSession) handleAck(ctx context.Context, f *AckFrame) error {
	if f.TaskID == "" {
		s.sendError(ctx, ErrCodeBadFrame, "ack requires task_id")
		return nil
	}
	task, err := s.h.Store.GetTask(ctx, s.node.OrgID, f.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		s.sendError(ctx, ErrCodeBadFrame, fmt.Sprintf("unknown task %q", f.TaskID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("load task for ack: %w", err)
	}
	// 只接受派给本节点的任务（防节点 ack 别人的任务）
	if task.NodeID == nil || *task.NodeID != s.node.ID {
		s.sendError(ctx, ErrCodeInvalidTransition,
			fmt.Sprintf("task %s is not assigned to this node", f.TaskID))
		return nil
	}
	if err := s.h.Store.Transition(ctx, s.node.OrgID, f.TaskID, store.TaskAssigned, store.TaskAcked); err != nil {
		s.reportTransitionError(ctx, f.TaskID, err)
		return nil
	}
	// ack 成功即开始计时：把租约推到 now + TTL（不再受 20s ack 窗口约束）
	now := s.h.now()
	if err := s.h.Store.ExtendLease(ctx, s.node.OrgID, f.TaskID, now+int64(s.h.leaseTTLSec())); err != nil {
		slog.Warn("extend lease on ack failed",
			"event", "node.ws.ack.extend_failed",
			"task_id", f.TaskID,
			"error", err.Error(),
		)
	}
	return nil
}

// handleProgress 处理进度：acked → running（重复 progress 幂等）。
func (s *nodeSession) handleProgress(ctx context.Context, f *ProgressFrame) error {
	if f.TaskID == "" {
		s.sendError(ctx, ErrCodeBadFrame, "progress requires task_id")
		return nil
	}
	task, err := s.h.Store.GetTask(ctx, s.node.OrgID, f.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		s.sendError(ctx, ErrCodeBadFrame, fmt.Sprintf("unknown task %q", f.TaskID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("load task for progress: %w", err)
	}
	if task.NodeID == nil || *task.NodeID != s.node.ID {
		s.sendError(ctx, ErrCodeInvalidTransition,
			fmt.Sprintf("task %s is not assigned to this node", f.TaskID))
		return nil
	}
	// StartTask 内部幂等：已在 running 时直接返回
	if err := s.h.Store.StartTask(ctx, s.node.OrgID, f.TaskID); err != nil {
		s.reportTransitionError(ctx, f.TaskID, err)
		return nil
	}
	// progress 也续命（契约 §3.3：任何上行帧续命）
	now := s.h.now()
	if err := s.h.Store.ExtendLease(ctx, s.node.OrgID, f.TaskID, now+int64(s.h.leaseTTLSec())); err != nil {
		slog.Warn("extend lease on progress failed",
			"event", "node.ws.progress.extend_failed",
			"task_id", f.TaskID,
			"error", err.Error(),
		)
	}
	return nil
}

// handleResult 处理结果帧，推进到终态（不变量 I4/I5）。
//
// 三态由 ResultFrame.Reported() 判定（自定义 UnmarshalJSON 区分「字段缺失」与「显式 null」）：
//
//	out 缺失    → reported=false（节点没回报出口）→ 不派下一跳
//	"out":null  → reported=true,  out=nil        → 到头了
//	"out":"fix" → reported=true,  out=&"fix"     → 走 fix
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
	task, err := s.h.Store.GetTask(ctx, s.node.OrgID, f.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		s.sendError(ctx, ErrCodeBadFrame, fmt.Sprintf("unknown task %q", f.TaskID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("load task for result: %w", err)
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
	if err := s.h.Store.ReportResult(ctx, s.node.OrgID, f.TaskID, f.Status, out, reported,
		f.Summary, f.Artifacts, f.Error); err != nil {
		s.reportTransitionError(ctx, f.TaskID, err)
		return nil
	}

	// 释放槽位（任务结束，槽位该空出来）
	if err := s.h.Store.ReleaseInstanceByTask(ctx, s.node.ID, f.TaskID); err != nil {
		slog.Warn("release instance after result failed",
			"event", "node.ws.result.release_failed",
			"task_id", f.TaskID,
			"node_id", s.node.ID,
			"error", err.Error(),
		)
	}

	slog.Info("task finished",
		"event", "node.ws.result",
		"task_id", f.TaskID,
		"node_id", s.node.ID,
		"status", f.Status,
		"reported", reported,
		"out", outString(out),
	)
	return nil
}

// reportTransitionError 把状态机拒绝翻译成协议错误帧。
//
// 两条关键映射：
//   - 租约已失效 → lease_expired（节点该丢弃本地任务）
//   - 其他非法迁移 → invalid_transition
func (s *nodeSession) reportTransitionError(ctx context.Context, taskID string, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidTransition):
		s.sendError(ctx, ErrCodeInvalidTransition, err.Error())
	case errors.Is(err, store.ErrNotFound):
		s.sendError(ctx, ErrCodeInvalidTransition,
			fmt.Sprintf("task %s not found (may have been reclaimed)", taskID))
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
