package ws

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是上行帧的业务处理：每一帧先做**形状/方向/身份**校验，再交给 store 的
// *Authorized 事务。会话层自己不做「先查一次库再写」的 TOCTOU 判定。

// handleConfigApplied 落一次 config.applied：机器级 revision/hash + 可选实例快照。
func (h *Hub) handleConfigApplied(ctx context.Context, sess *Session, f *protocol.ConfigAppliedFrame) error {
	if strings.TrimSpace(f.Revision) == "" {
		return fmt.Errorf("%w: config.applied 缺 revision", errProtocolViolation)
	}
	revision, err := strconv.ParseInt(strings.TrimSpace(f.Revision), 10, 64)
	if err != nil || revision < 0 {
		return fmt.Errorf("%w: config.applied.revision 不是非负整数: %q", errProtocolViolation, f.Revision)
	}
	if err := h.db.RecordApplied(ctx, sess.nodeID, revision, f.Hash); err != nil {
		return err
	}
	return h.applyInstances(ctx, sess.nodeID, f.Instances)
}

// handleAccept 处理 task.accept：只有这里（节点接受 + 真实 slot）才可能授权执行。
//
// 顺序不可交换：形状（含 slot presence，0 合法）→ 会话身份（node 是登记名）→
// GrantLeaseAuthorized 事务（当前凭证/ns/assignment/proposed/slot/三层预算）。
// 任一步失败都**不授执行权**：不发 grant；store 已把 stale_config/slot_unavailable
// 放回 pending + 有界退避，其它错误保持原状（assignment 窗口自然过期）。
func (h *Hub) handleAccept(ctx context.Context, sess *Session, f *protocol.TaskAcceptFrame) error {
	if err := protocol.ValidateAccept(f); err != nil {
		return fmt.Errorf("%w: %v", errProtocolViolation, err)
	}
	if f.Node != sess.nodeName {
		return fmt.Errorf("%w: accept.node %q 不是本会话节点 %q", errProtocolViolation, f.Node, sess.nodeName)
	}
	slot := *f.Slot
	leaseID, attemptToken, attempt, expiresAt, err := h.db.GrantLeaseAuthorized(
		ctx, sess.auth, f.TaskID, f.AssignmentID, f.ProposedAttempt, slot, h.Now())
	if err != nil {
		if errors.Is(err, store.ErrNodeIdentityMismatch) {
			// 令牌已轮换：这条会话的身份不再是当前事实，断开而不是继续服务。
			h.log.Warn("grant rejected: node identity changed", "node_id", sess.nodeID, "task_id", f.TaskID)
			sess.close(websocket.StatusPolicyViolation, "node identity changed")
			return nil
		}
		h.log.Warn("grant rejected", "node_id", sess.nodeID, "task_id", f.TaskID,
			"slot", slot, "err", err)
		return nil
	}
	return sess.Send(ctx, &protocol.LeaseGrantFrame{
		Type:         protocol.FrameLeaseGrant,
		TaskID:       f.TaskID,
		Attempt:      attempt,
		Node:         sess.nodeName,
		LeaseID:      leaseID,
		AttemptToken: attemptToken,
		ExpiresAt:    timeUnix(expiresAt),
		AssignmentID: f.AssignmentID,
		Slot:         &slot,
	})
}

// handleReject 处理 task.reject：只接受封闭集合的两个 pre-grant 理由。
//
// 授权判定全在 store.RejectAssignmentAuthorized 同一事务：当前 credential hash /
// namespace 仍可用 / 目标 Node + assignment + proposedAttempt 精确匹配 / 提案未过期。
// 会话层只做形状与 wire node 名校验，不 Get-precheck 后裸 Reject。
func (h *Hub) handleReject(ctx context.Context, sess *Session, f *protocol.TaskRejectFrame) error {
	if !protocol.IsTaskRejectReason(f.Reason) {
		return fmt.Errorf("%w: 未知 reject reason %q", errProtocolViolation, f.Reason)
	}
	if f.TaskID == "" || f.AssignmentID == "" || f.ProposedAttempt < 1 {
		return fmt.Errorf("%w: task.reject 缺 task/assignment/proposed", errProtocolViolation)
	}
	if f.Node != sess.nodeName {
		return fmt.Errorf("%w: reject.node %q 不是本会话节点 %q", errProtocolViolation, f.Node, sess.nodeName)
	}
	err := h.db.RejectAssignmentAuthorized(ctx, sess.auth, store.AssignmentRejectionInputs{
		TaskID:          f.TaskID,
		NodeID:          sess.nodeID,
		AssignmentID:    f.AssignmentID,
		ProposedAttempt: f.ProposedAttempt,
		Reason:          f.Reason,
		Now:             h.Now(),
	})
	if err != nil {
		if errors.Is(err, store.ErrNodeIdentityMismatch) {
			// 令牌已轮换：这条会话的身份不再是当前事实，断开而不是继续服务。
			sess.close(websocket.StatusPolicyViolation, "node identity changed")
			return nil
		}
		// 错/旧/撤销/过期：0 写、不伤新提案；没有 ack 帧可回。
		h.log.Warn("reject assignment refused", "node_id", sess.nodeID, "task_id", f.TaskID,
			"assignment", f.AssignmentID, "err", err)
	}
	return nil
}

// handleRenew 处理 lease.renew：围绕同一个中枢 leaseId 续租，回 lease.ack。
//
// 授权全在 RenewLeaseAuthorized 事务里判（当前凭证/ns/归属/attempt+lease/未取消/
// 未过期）。`expiry == now` 一律拒绝：到点的授权不得被一次续租复活。
func (h *Hub) handleRenew(ctx context.Context, sess *Session, f *protocol.LeaseRenewFrame) error {
	if f.TaskID == "" || f.Attempt < 1 || f.LeaseID == "" {
		return fmt.Errorf("%w: lease.renew 缺 task/attempt/lease", errProtocolViolation)
	}
	expiresAt, err := h.db.RenewLeaseAuthorized(ctx, sess.auth, f.TaskID, f.Attempt, f.LeaseID, h.Now())
	if err != nil {
		if errors.Is(err, store.ErrNodeIdentityMismatch) {
			sess.close(websocket.StatusPolicyViolation, "node identity changed")
			return nil
		}
		// 过期/取消/绑定不符：不发 ack（节点按最后一次 ACK + 本地 deadline 自行停止）。
		h.log.Warn("renew rejected", "node_id", sess.nodeID, "task_id", f.TaskID, "err", err)
		return nil
	}
	return sess.Send(ctx, &protocol.LeaseAckFrame{
		Type:      protocol.FrameLeaseAck,
		TaskID:    f.TaskID,
		Attempt:   f.Attempt,
		LeaseID:   f.LeaseID,
		ExpiresAt: timeUnix(expiresAt),
	})
}

// handleResult 处理 task.result：分类授权写入 + 及时 ACK。
//
// ACK 纪律（C3b）：只有 store 明确返回 Recorded / Duplicate（当前认证身份 +
// 当前绑定 + 全量保留事实一致）才回 result.ack{accepted:true}。任何 generic error
// ——旧 attempt、过期租约、取消在飞、manifest 不匹配、任意内部错误——都**不 ACK**：
// 节点会把「没有 ack」当成结果未被中枢接受，而不是被一条错误的 ack 骗过去。
func (h *Hub) handleResult(ctx context.Context, sess *Session, f *protocol.TaskResultFrame) error {
	if f.TaskID == "" || f.Attempt < 1 || f.LeaseID == "" {
		return fmt.Errorf("%w: task.result 缺 task/attempt/lease", errProtocolViolation)
	}
	if f.Node != sess.nodeName {
		return fmt.Errorf("%w: result.node %q 不是本会话节点 %q", errProtocolViolation, f.Node, sess.nodeName)
	}
	phase := protocol.TaskPhase(f.Phase)
	if !phase.Valid() || !phase.Terminal() || phase == protocol.TaskPhaseCancelled {
		return fmt.Errorf("%w: result.phase %q 不是可接受终态", errProtocolViolation, f.Phase)
	}
	result := store.TaskResult{
		Phase:      string(phase),
		Summary:    f.Result.Summary,
		Artifacts:  f.Result.Artifacts,
		ExitCode:   f.ExitCode,
		StartedAt:  f.StartedAt,
		FinishedAt: f.FinishedAt,
	}
	if result.Artifacts == nil {
		result.Artifacts = []protocol.TaskArtifact{}
	}
	if f.Result.Error != nil {
		result.Reason = f.Result.Error.Reason
		result.Error = f.Result.Error.Message
		if result.Error == "" {
			result.Error = result.Reason
		}
	}
	disposition, err := h.db.CompleteTaskClassifiedAuthorized(
		ctx, sess.auth, f.TaskID, f.Attempt, f.LeaseID, h.Now(), result)
	if err != nil {
		if errors.Is(err, store.ErrNodeIdentityMismatch) {
			sess.close(websocket.StatusPolicyViolation, "node identity changed")
			return nil
		}
		h.log.Warn("result rejected", "node_id", sess.nodeID, "task_id", f.TaskID,
			"attempt", f.Attempt, "err", err)
		return nil
	}
	if disposition != store.TaskResultRecorded && disposition != store.TaskResultDuplicate {
		h.log.Warn("result disposition not ackable", "task_id", f.TaskID, "disposition", disposition)
		return nil
	}
	return sess.Send(ctx, &protocol.ResultAckFrame{
		Type:     protocol.FrameResultAck,
		TaskID:   f.TaskID,
		Attempt:  f.Attempt,
		Accepted: true,
	})
}

// handleCancelAck 处理 cancel.ack：**先** ValidateCancelAck（历史绑定 presence 是
// 使用整数 slot 的前置条件），再按当前认证身份 + 精确历史绑定 CAS 回收。
func (h *Hub) handleCancelAck(ctx context.Context, sess *Session, f *protocol.CancelAckFrame) error {
	if err := protocol.ValidateCancelAck(f); err != nil {
		return fmt.Errorf("%w: %v", errProtocolViolation, err)
	}
	input := store.ReapedInputs{
		TaskID:        f.TaskID,
		Attempt:       f.Attempt,
		NodeID:        sess.nodeID,
		NodeTokenHash: sess.tokenHash,
		LeaseID:       f.LeaseID,
		Slot:          *f.Slot,
		Now:           h.Now(),
		Reason:        store.ReclaimReasonCancelAck,
	}
	if err := h.db.AckCancelAuthorized(ctx, sess.auth, input, f.ProcessGroupReaped); err != nil {
		if errors.Is(err, store.ErrNodeIdentityMismatch) {
			sess.close(websocket.StatusPolicyViolation, "node identity changed")
			return nil
		}
		// 错 lease/slot/缺失历史绑定：fail closed（不清新槽、不改新相位）。
		h.log.Warn("cancel ack rejected", "node_id", sess.nodeID, "task_id", f.TaskID,
			"attempt", f.Attempt, "reaped", f.ProcessGroupReaped, "err", err)
	}
	return nil
}

// handleDeployed 处理 capability.deployed：按 requestId + refs 四元组配回部署意图。
//
// 共享帧的形状/状态校验后，全部权威判定交给 store.MarkDeployResultAuthorized 的
// 同一事务（当前 credential hash / ns 仍可用 / intent 归属该 Node / requestId 与
// capability UID/name/version/digest 绑定 / 未 superseded）。未知 request/错 digest/
// 未知 ns/轮换 token 一律 0 写，不 Get-precheck 后裸 Mark。
func (h *Hub) handleDeployed(ctx context.Context, sess *Session, f *protocol.CapabilityDeployedFrame) error {
	if !f.Status.Valid() {
		return fmt.Errorf("%w: capability.deployed.status %q 不在封闭集合", errProtocolViolation, f.Status)
	}
	if f.Namespace == "" || f.Name == "" || f.Version <= 0 || f.Digest == "" {
		return fmt.Errorf("%w: capability.deployed 缺 refs", errProtocolViolation)
	}
	deploymentID, ok := strings.CutPrefix(f.RequestID, "dep_")
	if !ok || deploymentID == "" {
		h.log.Warn("deploy ack with foreign request id", "node_id", sess.nodeID, "request_id", f.RequestID)
		return nil
	}
	namespaceID, err := h.namespaceIDBySlug(ctx, f.Namespace)
	if err != nil {
		h.log.Warn("deploy ack for unknown namespace", "node_id", sess.nodeID, "namespace", f.Namespace)
		return nil
	}
	capability, err := h.db.GetCapability(ctx, namespaceID, f.Name, f.Version)
	if err != nil {
		h.log.Warn("deploy ack for unknown capability", "node_id", sess.nodeID,
			"namespace", f.Namespace, "name", f.Name, "version", f.Version, "err", err)
		return nil
	}
	state := store.DeploymentApplied
	reason := ""
	if f.Status == protocol.DeployStatusFailed {
		state = store.DeploymentFailed
		reason = f.Error
	}
	// 回执里的命令探测结果是**新事实**：部署落地后节点不会重连，不落账的话中枢会一直
	// 按旧的 hello 判 CommandMissing（能力部署成功却永远不可派发）。
	//
	// 顺序：先落部署事实（权威判定都在那一笔事务里），再合并申报；合并失败只告警 ——
	// 部署已经成立，下一次 hello 会整份对齐，不该让一条附属事实回滚主结果。
	mergeDeclared := state == store.DeploymentApplied

	err = h.db.MarkDeployResultAuthorized(ctx, sess.auth, store.DeploymentResultInputs{
		DeploymentID: deploymentID,
		RequestID:    f.RequestID,
		NamespaceID:  namespaceID,
		CapabilityID: capability.ID,
		Capability:   capability.Name,
		Version:      capability.Version,
		Digest:       f.Digest,
		State:        state,
		Reason:       reason,
		Now:          h.Now(),
	})
	if err != nil {
		if errors.Is(err, store.ErrNodeIdentityMismatch) {
			sess.close(websocket.StatusPolicyViolation, "node identity changed")
			return nil
		}
		h.log.Warn("deploy ack rejected", "node_id", sess.nodeID, "request_id", f.RequestID,
			"deployment", deploymentID, "err", err)
		return nil
	}
	if mergeDeclared {
		if err := h.db.MergeDeclaredCapability(ctx, sess.nodeID, store.DeclaredCapability{
			Name:     capability.Name,
			Version:  capability.Version,
			Digest:   capability.Digest,
			Commands: f.Commands,
		}); err != nil {
			h.log.Warn("deploy ack: merge declared capability failed", "node_id", sess.nodeID,
				"capability", capability.Name, "version", capability.Version, "err", err)
		}
	}
	return nil
}

// handleEmit 处理 task.emit：把节点转发的 HTTP emit 交给路由层，再按结论回 ack。
//
// 授权顺序在 route.Emit → store.EmitAuthorized：当前凭证/ns/current lease/token/
// 未过期/未取消 → 幂等（(taskId,id)）→ 写 emit 记录 + 子任务。requestId 只用于
// ack 配对；业务幂等只看显式 id。
func (h *Hub) handleEmit(ctx context.Context, sess *Session, f *protocol.TaskEmitFrame) error {
	if err := f.Validate(); err != nil {
		return fmt.Errorf("%w: %v", errProtocolViolation, err)
	}
	result, err := h.router.Emit(ctx, route.EmitInput{
		Auth: store.EmitAuth{
			TaskID:        f.TaskID,
			NodeID:        sess.nodeID,
			NodeTokenHash: sess.tokenHash,
			Attempt:       f.Attempt,
			LeaseID:       f.LeaseID,
			AttemptToken:  f.AttemptToken,
			Now:           h.Now(),
		},
		To:     f.To,
		EmitID: f.ID,
		Data:   f.Data,
	})
	if err != nil {
		var emitErr *route.EmitError
		if errors.As(err, &emitErr) {
			body := emitErr.Body()
			return sess.Send(ctx, &protocol.TaskEmitAckFrame{
				Type:       protocol.FrameTaskEmitAck,
				RequestID:  f.RequestID,
				TaskID:     f.TaskID,
				Attempt:    f.Attempt,
				HTTPStatus: emitErr.HTTPStatus(),
				Error:      &body,
			})
		}
		// 非可判别错误：不回结论（节点网关按 timeout 走「结果未知」CLI 5），
		// 不伪造一个业务错误码。
		h.log.Warn("emit failed unexpectedly", "node_id", sess.nodeID, "task_id", f.TaskID, "err", err)
		return nil
	}
	response := result.Response
	return sess.Send(ctx, &protocol.TaskEmitAckFrame{
		Type:       protocol.FrameTaskEmitAck,
		RequestID:  f.RequestID,
		TaskID:     f.TaskID,
		Attempt:    f.Attempt,
		HTTPStatus: protocol.EmitHTTPStatus(""),
		Response:   &response,
	})
}

// namespaceIDBySlug 把 wire 上的 ns 名解析成内部 id（找不到 → ErrNotFound）。
func (h *Hub) namespaceIDBySlug(ctx context.Context, slug string) (string, error) {
	namespaces, err := h.db.ListNamespaces(ctx)
	if err != nil {
		return "", err
	}
	for _, namespace := range namespaces {
		if namespace.Slug == slug {
			return namespace.ID, nil
		}
	}
	return "", store.ErrNotFound
}

// handleUpgradeAck 处理 upgrade_ack：成功写版本列，失败只写升级状态。
//
// 保留 v3 的运维帧语义（协议已搬进共享包，行为不变）：ok=false 时**不**改
// agent_version —— 二进制没换，把它写成目标版本就是谎报成功。
func (h *Hub) handleUpgradeAck(ctx context.Context, sess *Session, f *protocol.UpgradeAckFrame) error {
	if f.OK {
		if f.Version == "" {
			return fmt.Errorf("%w: upgrade_ack ok=true 但缺 version", errProtocolViolation)
		}
		return h.db.RecordAgentUpgrade(ctx, sess.nodeID, f.Version, f.Commit, h.Now())
	}
	return h.db.FailAgentUpgrade(ctx, sess.nodeID, protocol.NormalizeUpgradeError(f.Error), h.Now())
}

// RequestUpgrade 向在线节点下发一次二进制升级（帧里不带二进制，节点自己 HTTP 下载）。
//
// 先落 sent 事实再发帧：帧发失败时状态仍是「已尝试」，不会把一次已发生的下发
// 说成没发生。
func (h *Hub) RequestUpgrade(ctx context.Context, nodeID, targetVersion, sha256Hex, urlPath string) error {
	sess := h.session(nodeID)
	if sess == nil {
		return ErrNodeOffline
	}
	if err := h.db.MarkAgentUpgradeSent(ctx, nodeID, targetVersion, h.Now()); err != nil {
		return err
	}
	return sess.Send(ctx, &protocol.UpgradeFrame{
		Type: protocol.FrameUpgrade, TargetVersion: targetVersion,
		SHA256: sha256Hex, URLPath: urlPath,
	})
}
