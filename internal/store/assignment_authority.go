package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zzjcool/anotify/internal/protocol"
)

// assignment_authority.go 是派发候选（proposal）与候选拒绝（reject）的**事务授权**补口
// （S1-D4）。
//
// G2 review 已证旧 `AssignTask` 每个 sweep 都覆写 assignment：同一 Task 在窗口内被不同
// 候选轮换，节点侧 reservation 与中枢账本相互打架；`task.reject` 也没有当前凭证/
// namespaced 绑定与目标 Node 校验，一条旧请求能改掉新候选。
//
// 这里只补两个入口，`GrantLeaseAuthorized`（真正的执行授权）不重构：
//   - ProposeTaskAssignment：候选是**不授执行权**的提案；未到期的同 Task 提案原样返回
//     （多 sweep 不 churn），到期才允许换绑定；不写 lease/attempt/busy。
//   - RejectAssignmentAuthorized：同事务核当前 credential hash / namespace 许可 /
//     目标 Node + assignment + proposedAttempt + 未过期，才回 pending + bounded backoff；
//     不满足一律 0 写，不损伤新提案。

// AssignmentProposalInputs 是一次候选提案请求（中枢调度器构造，不是节点 wire）。
type AssignmentProposalInputs struct {
	TaskID          string
	NodeID          string
	ProposedAttempt int
	// AssignmentID 空 → store 生成 asg_…；显式给值必须与本次候选一致（调用方重试可复用它）。
	AssignmentID string
	// Now 可信 server 时刻（0 → Now()）。
	Now int64
}

// AssignmentProposal 是一次候选绑定的完整事实（Reused=true 表示命中未到期原提案）。
type AssignmentProposal struct {
	TaskID          string
	AssignmentID    string
	NodeID          string
	NodeName        string
	ProposedAttempt int
	ExpiresAt       int64
	Reused          bool
}

// ProposeTaskAssignment 记录一次派发候选（不授执行权）。
//
// 同一 Task 已有**未到期**提案时，无论请求的节点/attempt 是什么，都返回原完整绑定：
// 这是「多 sweep 不 churn」的唯一实现点（不靠调用方自律）。过期（expiry <= now，
// 相等也视为到期）才允许写新绑定；新写入用条件 UPDATE 做并发 CAS。
func (d *DB) ProposeTaskAssignment(ctx context.Context, in AssignmentProposalInputs) (AssignmentProposal, error) {
	if in.TaskID == "" || in.NodeID == "" {
		return AssignmentProposal{}, fmt.Errorf("propose assignment: taskID/nodeID required")
	}
	if in.ProposedAttempt <= 0 {
		return AssignmentProposal{}, fmt.Errorf("%w: propose 必须带 proposedAttempt", ErrNoAssignment)
	}
	if in.Now == 0 {
		in.Now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return AssignmentProposal{}, fmt.Errorf("propose assignment %s: begin: %w", in.TaskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	var (
		namespaceID  string
		phase        string
		cancelReq    sql.NullInt64
		rowAssign    string
		rowExpires   sql.NullInt64
		rowNode      sql.NullString
		rowNodeName  string
		rowProposed  int
		capabilityID sql.NullString
	)
	err = ex.queryRowContext(ctx,
		`SELECT namespace_id, phase, cancel_requested_at, assignment_id, assignment_expires_at,
		        node_id, node_name, proposed_attempt, capability_id
		   FROM tasks WHERE id = ?`, in.TaskID).
		Scan(&namespaceID, &phase, &cancelReq, &rowAssign, &rowExpires, &rowNode, &rowNodeName,
			&rowProposed, &capabilityID)
	if errors.Is(err, sql.ErrNoRows) {
		return AssignmentProposal{}, ErrNotFound
	}
	if err != nil {
		return AssignmentProposal{}, fmt.Errorf("propose assignment %s: read: %w", in.TaskID, err)
	}
	if phase != TaskPending || cancelReq.Valid {
		return AssignmentProposal{}, fmt.Errorf("%w: task %s is %s（pending 且未取消才可派）", ErrInvalidTransition, in.TaskID, phase)
	}
	// 未解析失败行永远不是派发候选（它的形状 CHECK 也不允许被改回 pending）。
	if !capabilityID.Valid || capabilityID.String == "" {
		return AssignmentProposal{}, fmt.Errorf("%w: task %s 没有解析出的能力行，不能派发", ErrInvalidTransition, in.TaskID)
	}

	// 未到期的同 Task 提案：完整返回原绑定，不覆写（多 sweep 不 churn）。
	if rowAssign != "" && rowExpires.Valid && rowExpires.Int64 > in.Now && rowNode.Valid {
		return AssignmentProposal{
			TaskID:          in.TaskID,
			AssignmentID:    rowAssign,
			NodeID:          rowNode.String,
			NodeName:        rowNodeName,
			ProposedAttempt: rowProposed,
			ExpiresAt:       rowExpires.Int64,
			Reused:          true,
		}, nil // 只读命中，回滚空事务
	}

	// 目标节点候选事实同 tx 检查：存在、online、当前 ns 授权覆盖。
	var (
		nodeName      string
		nodeStatus    string
		rawDeclaredNS string
	)
	err = ex.queryRowContext(ctx,
		`SELECT name, status, declared_namespaces FROM nodes WHERE id = ?`, in.NodeID).
		Scan(&nodeName, &nodeStatus, &rawDeclaredNS)
	if errors.Is(err, sql.ErrNoRows) {
		return AssignmentProposal{}, fmt.Errorf("%w: node %s", ErrNotFound, in.NodeID)
	}
	if err != nil {
		return AssignmentProposal{}, fmt.Errorf("propose assignment %s: read node: %w", in.TaskID, err)
	}
	if nodeStatus != NodeStatusOnline {
		return AssignmentProposal{}, fmt.Errorf("%w: node %s is %s（不再接新任务）", ErrInvalidTransition, in.NodeID, nodeStatus)
	}
	if err := namespaceAllowedOn(ctx, ex, in.NodeID, rawDeclaredNS, namespaceID); err != nil {
		return AssignmentProposal{}, err
	}

	lc, lerr := lifecycleOn(ctx, ex, capabilityID.String)
	if lerr != nil {
		return AssignmentProposal{}, fmt.Errorf("propose assignment %s: %w", in.TaskID, lerr)
	}
	assignmentID := in.AssignmentID
	if assignmentID == "" {
		assignmentID = newID("asg")
	}
	expiresAt := in.Now + AssignmentWindowSec
	res, err := ex.execContext(ctx,
		`UPDATE tasks SET assignment_id = ?, node_id = ?, node_name = ?, proposed_attempt = ?,
		        assignment_expires_at = ?, idempotent = ?, max_attempts = ?
		  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL
		    AND (assignment_expires_at IS NULL OR assignment_expires_at <= ?)`,
		assignmentID, in.NodeID, nodeName, in.ProposedAttempt, expiresAt,
		boolToInt(lc.Idempotent), lc.EffectiveMaxAttempts(),
		in.TaskID, TaskPending, in.Now)
	if err != nil {
		return AssignmentProposal{}, fmt.Errorf("propose assignment %s: write: %w", in.TaskID, err)
	}
	if n, rerr := res.RowsAffected(); rerr != nil {
		return AssignmentProposal{}, fmt.Errorf("propose assignment %s: rows: %w", in.TaskID, rerr)
	} else if n == 0 {
		// 并发下另一个 sweep 先写了新提案：回读当前绑定按「未到期原提案」返回。
		var (
			curAssign  string
			curExpires sql.NullInt64
			curNode    sql.NullString
			curName    string
			curProp    int
		)
		if qerr := ex.queryRowContext(ctx,
			`SELECT assignment_id, assignment_expires_at, node_id, node_name, proposed_attempt
			   FROM tasks WHERE id = ?`, in.TaskID).
			Scan(&curAssign, &curExpires, &curNode, &curName, &curProp); qerr != nil {
			return AssignmentProposal{}, fmt.Errorf("propose assignment %s: reread: %w", in.TaskID, qerr)
		}
		if curAssign == "" || !curExpires.Valid || curExpires.Int64 <= in.Now || !curNode.Valid {
			return AssignmentProposal{}, fmt.Errorf("%w: task %s 的候选已被并发改动", ErrNoAssignment, in.TaskID)
		}
		return AssignmentProposal{
			TaskID: in.TaskID, AssignmentID: curAssign, NodeID: curNode.String, NodeName: curName,
			ProposedAttempt: curProp, ExpiresAt: curExpires.Int64, Reused: true,
		}, nil
	}
	if err := tx.Commit(); err != nil {
		return AssignmentProposal{}, fmt.Errorf("propose assignment %s: commit: %w", in.TaskID, err)
	}
	return AssignmentProposal{
		TaskID:          in.TaskID,
		AssignmentID:    assignmentID,
		NodeID:          in.NodeID,
		NodeName:        nodeName,
		ProposedAttempt: in.ProposedAttempt,
		ExpiresAt:       expiresAt,
	}, nil
}

// AssignmentRejectionInputs 是 task.reject 的 typed 事实（节点 wire 的领域形态）。
type AssignmentRejectionInputs struct {
	TaskID string
	// NodeID 是**目标绑定**：必须等于当前认证节点，防止请求体自报别的机器。
	NodeID          string
	AssignmentID    string
	ProposedAttempt int
	// Reason 闭集：stale_config | slot_unavailable。
	Reason string
	// Now 可信 server 时刻（0 → Now()）。
	Now int64
}

// RejectAssignmentAuthorized 处理一次节点候选拒绝（**当前认证身份 + 精确候选绑定**）。
//
// 同事务判据（缺一不可）：
//   - 当前节点 credential hash（nodes.token_hash == auth.NodeTokenHash）；
//   - namespace 当前仍可用（撤销后连退避也不写）；
//   - 任务 pending、未取消，提案确属该 Node，assignment/proposedAttempt 精确匹配；
//   - 提案未过期（expiry <= now 视为过期）。
//
// 通过才清候选并回 pending + bounded backoff（不计 attempt）。任何一条不满足都是
// 错误 + 0 写：旧 token / 另一 Node / 撤销 ns / 旧 assignment / 过期提案都不能改掉
// 当前新提案，也不会替别人写退避。
func (d *DB) RejectAssignmentAuthorized(ctx context.Context, auth NodeAuthorization, in AssignmentRejectionInputs) error {
	if !protocol.IsTaskRejectReason(in.Reason) && in.Reason != TaskRejectSlotUnavailable {
		return fmt.Errorf("%w: task.reject 只允许 stale_config | slot_unavailable，得到 %q", ErrInvalidTransition, in.Reason)
	}
	if in.TaskID == "" || in.AssignmentID == "" || in.NodeID == "" {
		return fmt.Errorf("%w: reject 必须带 taskID/assignmentID/nodeID", ErrNoAssignment)
	}
	if in.ProposedAttempt <= 0 {
		return fmt.Errorf("%w: reject 必须带 proposedAttempt", ErrNoAssignment)
	}
	if err := auth.validate(); err != nil {
		return err
	}
	if in.NodeID != auth.NodeID {
		return fmt.Errorf("%w: reject 的目标节点 %s 不是当前认证节点 %s",
			ErrNodeIdentityMismatch, in.NodeID, auth.NodeID)
	}
	if in.Now == 0 {
		in.Now = Now()
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reject assignment %s: begin: %w", in.TaskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}

	rawDeclared, err := authorizeNodeOn(ctx, ex, auth)
	if err != nil {
		return err
	}

	var (
		namespaceID string
		phase       string
		rowAssign   string
		rowExpires  sql.NullInt64
		rowNode     sql.NullString
		rowProposed int
		cancelReq   sql.NullInt64
		attempt     int
	)
	err = ex.queryRowContext(ctx,
		`SELECT namespace_id, phase, assignment_id, assignment_expires_at, node_id,
		        proposed_attempt, cancel_requested_at, attempt
		   FROM tasks WHERE id = ?`, in.TaskID).
		Scan(&namespaceID, &phase, &rowAssign, &rowExpires, &rowNode, &rowProposed, &cancelReq, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("reject assignment %s: read: %w", in.TaskID, err)
	}
	// 当前 ns 许可先于任何写入（撤销授权后连退避都不落）。
	if err := namespaceAllowedOn(ctx, ex, auth.NodeID, rawDeclared, namespaceID); err != nil {
		return err
	}
	if phase != TaskPending || cancelReq.Valid {
		return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, in.TaskID, phase)
	}
	if rowAssign != in.AssignmentID || !rowNode.Valid || rowNode.String != auth.NodeID ||
		rowProposed != in.ProposedAttempt {
		return fmt.Errorf("%w: task %s 当前候选 %q@%s#%d 与拒绝请求 %q@%s#%d 不匹配",
			ErrNoAssignment, in.TaskID, rowAssign, rowNode.String, rowProposed,
			in.AssignmentID, auth.NodeID, in.ProposedAttempt)
	}
	if !rowExpires.Valid || rowExpires.Int64 <= in.Now {
		return fmt.Errorf("%w: task %s 的候选 %q 已过期", ErrNoAssignment, in.TaskID, in.AssignmentID)
	}

	waiting, delay := WaitingForConfig, StaleConfigBackoff(attempt)
	if in.Reason == TaskRejectSlotUnavailable {
		waiting, delay = WaitingForSlot, SlotUnavailableBackoff(attempt)
	}
	res, err := ex.execContext(ctx,
		`UPDATE tasks
		    SET assignment_id = '', proposed_attempt = 0, assignment_expires_at = NULL,
		        node_id = NULL, node_name = '', waiting_reason = ?, next_attempt_at = ?
		  WHERE id = ? AND phase = ? AND cancel_requested_at IS NULL
		    AND assignment_id = ? AND node_id = ? AND proposed_attempt = ?
		    AND assignment_expires_at > ?`,
		waiting, in.Now+delay, in.TaskID, TaskPending, in.AssignmentID, auth.NodeID,
		in.ProposedAttempt, in.Now)
	if err != nil {
		return fmt.Errorf("reject assignment %s: write: %w", in.TaskID, err)
	}
	if n, rerr := res.RowsAffected(); rerr != nil {
		return fmt.Errorf("reject assignment %s: rows: %w", in.TaskID, rerr)
	} else if n == 0 {
		return fmt.Errorf("%w: task %s 不再接受该候选拒绝", ErrNoAssignment, in.TaskID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reject assignment %s: commit: %w", in.TaskID, err)
	}
	return nil
}
