package store

// assignment_authority_test.go 是 S1-D4 的白盒验收：
//
//   - 未到期提案多 sweep 不 churn（原 assignment/node/proposed/expiry 原样返回）；
//   - 到期（含 expiry == now）才允许新绑定；
//   - 提案不授 lease/attempt/busy；
//   - reject 必须同事务有当前凭证/ns/目标 Node/精确 assignment/proposed/未过期；
//     旧请求/错身份/撤销 ns/旧 assignment 0 写且不损伤新提案；reason 闭集、不计 attempt。

import (
	"context"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// proposalEnv 建一条 pending 任务 + 一台 online 节点（declared "*"）。
func proposalEnv(t *testing.T) (*DB, string, *Task, *Node) {
	t.Helper()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	cap := v4Cap(t, db, nsID, "assign-cap", 1)
	task := v4Task(t, db, nsID, cap, "trace-assign", "{}")
	node := v4Node(t, db, "assign-node", 1)
	return db, nsID, task, node
}

func TestProposeTaskAssignmentNoChurn(t *testing.T) {
	db, _, task, node := proposalEnv(t)
	ctx := context.Background()

	first, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 1, Now: 1000,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if first.Reused || first.AssignmentID == "" || first.ExpiresAt != 1000+AssignmentWindowSec ||
		first.NodeID != node.ID || first.ProposedAttempt != 1 {
		t.Fatalf("first proposal = %+v", first)
	}
	// 提案不授执行权。
	got, err := db.GetTask(ctx, task.NamespaceID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 0 || got.LeaseID != "" || got.LeaseUntil != nil || got.Phase != TaskPending {
		t.Fatalf("提案改动了执行授权事实: %+v", got)
	}
	if busy := v4BusySlots(t, db, node.ID); busy != 0 {
		t.Fatalf("提案不能占物理槽: %d", busy)
	}

	// 多 sweep（甚至换一个请求节点）都必须原样返回未到期提案。
	for i := 0; i < 3; i++ {
		again, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
			TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 2, Now: int64(1001 + i),
		})
		if err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
		if !again.Reused || again.AssignmentID != first.AssignmentID ||
			again.ExpiresAt != first.ExpiresAt || again.ProposedAttempt != 1 || again.NodeID != node.ID {
			t.Fatalf("sweep %d churn 了提案: %+v (want %+v)", i, again, first)
		}
	}
}

func TestProposeTaskAssignmentExpiryAllowsNewBinding(t *testing.T) {
	db, _, task, node := proposalEnv(t)
	ctx := context.Background()
	first, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 1, Now: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// expiry == now 视为到期 → 允许新绑定。
	second, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 2, Now: first.ExpiresAt,
	})
	if err != nil {
		t.Fatalf("expired propose: %v", err)
	}
	if second.Reused || second.AssignmentID == first.AssignmentID ||
		second.ExpiresAt != first.ExpiresAt+AssignmentWindowSec || second.ProposedAttempt != 2 {
		t.Fatalf("到期后应写新绑定: first=%+v second=%+v", first, second)
	}
}

func TestProposeTaskAssignmentRejectsUnauthorizedNode(t *testing.T) {
	db, _, task, node := proposalEnv(t)
	ctx := context.Background()

	// 节点离线：不是候选。
	if err := db.UpdateNodeStatus(ctx, node.ID, NodeStatusOffline); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 1, Now: 1000,
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("offline 节点应被拒，得到 %v", err)
	}
	if err := db.UpdateNodeStatus(ctx, node.ID, NodeStatusOnline); err != nil {
		t.Fatal(err)
	}
	// 撤销 namespace 授权：0 写。
	putV4NodeCfg(t, db, node.ID, []string{"other"}, nil, nil)
	if _, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 1, Now: 1000,
	}); !errors.Is(err, ErrEmitNamespaceDenied) {
		t.Fatalf("撤销 ns 后应被拒，得到 %v", err)
	}
	got, err := db.GetTask(ctx, task.NamespaceID, task.ID)
	if err != nil || got.AssignmentID != "" || got.NodeID != nil {
		t.Fatalf("拒绝路径写入了候选: %+v %v", got, err)
	}
}

func TestRejectAssignmentAuthorized(t *testing.T) {
	db, _, task, node := proposalEnv(t)
	ctx := context.Background()
	auth := v4Auth(t, db, node.ID)

	proposal, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 1, Now: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	reject := AssignmentRejectionInputs{
		TaskID: task.ID, NodeID: node.ID, AssignmentID: proposal.AssignmentID,
		ProposedAttempt: 1, Reason: protocol.ReasonStaleConfig, Now: 1001,
	}
	if err := db.RejectAssignmentAuthorized(ctx, auth, reject); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, err := db.GetTask(ctx, task.NamespaceID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != TaskPending || got.AssignmentID != "" || got.NodeID != nil ||
		got.WaitingReason != WaitingForConfig || got.NextAttemptAt == nil ||
		*got.NextAttemptAt != 1001+StaleConfigBackoff(0) {
		t.Fatalf("reject 结果 = %+v", got)
	}
	if got.Attempt != 0 {
		t.Fatalf("reject 不能计 attempt: %d", got.Attempt)
	}

	// 重复同一 reject：明确错误、0 写（不换新提案）。
	before := *got
	if err := db.RejectAssignmentAuthorized(ctx, auth, reject); !errors.Is(err, ErrNoAssignment) {
		t.Fatalf("重复 reject 应 ErrNoAssignment，得到 %v", err)
	}
	after, _ := db.GetTask(ctx, task.NamespaceID, task.ID)
	if after.AssignmentID != before.AssignmentID || after.NextAttemptAt == nil ||
		*after.NextAttemptAt != *before.NextAttemptAt || after.WaitingReason != before.WaitingReason {
		t.Fatalf("重复 reject 改动了任务: %+v", after)
	}
	// 退避过后仍可重新提案。
	if p, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 1, Now: 2000,
	}); err != nil || p.Reused || p.AssignmentID == proposal.AssignmentID {
		t.Fatalf("重复 reject 损伤了后续提案: %+v %v", p, err)
	}
}

func TestRejectAssignmentAuthorizedWrongRequestsZeroWrites(t *testing.T) {
	db, _, task, node := proposalEnv(t)
	ctx := context.Background()
	auth := v4Auth(t, db, node.ID)
	proposal, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 1, Now: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := AssignmentRejectionInputs{
		TaskID: task.ID, NodeID: node.ID, AssignmentID: proposal.AssignmentID,
		ProposedAttempt: 1, Reason: protocol.ReasonStaleConfig, Now: 1001,
	}
	cases := map[string]struct {
		auth NodeAuthorization
		mut  func(*AssignmentRejectionInputs)
	}{
		"old_token":         {NodeAuthorization{NodeID: node.ID, NodeTokenHash: "deadbeef"}, nil},
		"wrong_target_node": {auth, func(in *AssignmentRejectionInputs) { in.NodeID = "nd_other" }},
		"wrong_assignment":  {auth, func(in *AssignmentRejectionInputs) { in.AssignmentID = "asg_old" }},
		"wrong_attempt":     {auth, func(in *AssignmentRejectionInputs) { in.ProposedAttempt = 2 }},
		"bad_reason":        {auth, func(in *AssignmentRejectionInputs) { in.Reason = "not_a_reason" }},
		"expired":           {auth, func(in *AssignmentRejectionInputs) { in.Now = proposal.ExpiresAt }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := base
			if tc.mut != nil {
				tc.mut(&in)
			}
			if err := db.RejectAssignmentAuthorized(ctx, tc.auth, in); err == nil {
				t.Fatalf("%s 被接受了", name)
			}
			got, err := db.GetTask(ctx, task.NamespaceID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.AssignmentID != proposal.AssignmentID || got.NodeID == nil || *got.NodeID != node.ID ||
				got.NextAttemptAt != nil || got.WaitingReason != "" {
				t.Fatalf("%s 损伤了当前提案: %+v", name, got)
			}
		})
	}

	// 撤销 namespace：当前凭证有效也必须 0 写。
	putV4NodeCfg(t, db, node.ID, []string{"other"}, nil, nil)
	if err := db.RejectAssignmentAuthorized(ctx, auth, base); !errors.Is(err, ErrEmitNamespaceDenied) {
		t.Fatalf("撤销 ns 后 reject 应被拒，得到 %v", err)
	}
	got, _ := db.GetTask(ctx, task.NamespaceID, task.ID)
	if got.AssignmentID != proposal.AssignmentID || got.NextAttemptAt != nil {
		t.Fatalf("撤销 ns 的 reject 写入了退避: %+v", got)
	}
}

// TestRejectAssignmentStaleRequestKeepsNewProposal 旧 assignment 的 reject 不能改掉新提案。
func TestRejectAssignmentStaleRequestKeepsNewProposal(t *testing.T) {
	db, _, task, node := proposalEnv(t)
	ctx := context.Background()
	auth := v4Auth(t, db, node.ID)
	old, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 1, Now: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: task.ID, NodeID: node.ID, ProposedAttempt: 2, Now: 1100,
	})
	if err != nil || fresh.Reused {
		t.Fatalf("到期后新提案: %+v %v", fresh, err)
	}
	stale := AssignmentRejectionInputs{
		TaskID: task.ID, NodeID: node.ID, AssignmentID: old.AssignmentID,
		ProposedAttempt: 1, Reason: protocol.ReasonStaleConfig, Now: 1101,
	}
	if err := db.RejectAssignmentAuthorized(ctx, auth, stale); !errors.Is(err, ErrNoAssignment) {
		t.Fatalf("旧 assignment reject 应 ErrNoAssignment，得到 %v", err)
	}
	got, err := db.GetTask(ctx, task.NamespaceID, task.ID)
	if err != nil || got.AssignmentID != fresh.AssignmentID || got.NextAttemptAt != nil {
		t.Fatalf("旧 reject 损伤了新提案: %+v %v", got, err)
	}
	// 新提案自己的 reject 仍然有效。
	if err := db.RejectAssignmentAuthorized(ctx, auth, AssignmentRejectionInputs{
		TaskID: task.ID, NodeID: node.ID, AssignmentID: fresh.AssignmentID,
		ProposedAttempt: 2, Reason: TaskRejectSlotUnavailable, Now: 1102,
	}); err != nil {
		t.Fatalf("fresh reject: %v", err)
	}
	got, _ = db.GetTask(ctx, task.NamespaceID, task.ID)
	if got.WaitingReason != WaitingForSlot || got.AssignmentID != "" {
		t.Fatalf("slot_unavailable reject 结果 = %+v", got)
	}
}
