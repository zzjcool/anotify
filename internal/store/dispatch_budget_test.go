package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

func capPerNode(t *testing.T, db *DB, nsID, name string, maxPerNode int) *CapabilityVersion {
	t.Helper()
	perNode := maxPerNode
	return v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: name, Version: 1,
		Spec: artifact.Spec{Concurrency: artifact.Concurrency{MaxPerNode: &perNode}},
	})
}

func TestDispatchBudgetsSeeRunningExecutionPerNode(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := capPerNode(t, db, nsID, "triage-681", 1)
	busy := v4Node(t, db, "host-ws", 3)
	idle := v4Node(t, db, "hw", 3)

	running := v4Task(t, db, nsID, c, "tr_681_running", "")
	v4Grant(t, db, running.ID, busy.ID, "asg_681_running")

	next := v4Task(t, db, nsID, c, "tr_681_next", "")
	budgets, err := db.DispatchBudgets(ctx, next, []string{busy.ID, idle.ID}, 0)
	if err != nil {
		t.Fatalf("dispatch budgets: %v", err)
	}
	if len(budgets) != 2 {
		t.Fatalf("应给出两台节点的余量，得到 %d 条", len(budgets))
	}
	if got := budgets[busy.ID]; got.PinnedUsed != 1 || got.PinnedLimit != 1 || got.AdmitsOne() {
		t.Fatalf("跑着一条 triage（maxPerNode=1）的节点必须判满：%+v", got)
	}
	if got := budgets[idle.ID]; got.PinnedUsed != 0 || !got.AdmitsOne() {
		t.Fatalf("空闲节点必须还有余量：%+v", got)
	}
	if over := budgets[busy.ID].OverBy(); over != 1 {
		t.Fatalf("满 1 个槽位的超出量应是 1，得到 %d（%+v）", over, budgets[busy.ID])
	}
	if over := budgets[idle.ID].OverBy(); over != 0 {
		t.Fatalf("有余量的节点不该有超出量，得到 %d（%+v）", over, budgets[idle.ID])
	}
}

func TestDispatchBudgetsCountInflightProposals(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := capPerNode(t, db, nsID, "triage-inflight", 1)
	first := v4Node(t, db, "host-ws", 3)
	second := v4Node(t, db, "hw", 3)
	now := Now()

	inflight := v4Task(t, db, nsID, c, "tr_681_inflight", "")
	if _, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: inflight.ID, NodeID: first.ID, ProposedAttempt: 1, Now: now,
	}); err != nil {
		t.Fatalf("提议候选: %v", err)
	}

	next := v4Task(t, db, nsID, c, "tr_681_after_inflight", "")
	budgets, err := db.DispatchBudgets(ctx, next, []string{first.ID, second.ID}, now)
	if err != nil {
		t.Fatalf("dispatch budgets: %v", err)
	}
	if budgets[first.ID].AdmitsOne() {
		t.Fatalf("在飞候选必须算进选点余量（maxPerNode=1）：%+v", budgets[first.ID])
	}
	if !budgets[second.ID].AdmitsOne() {
		t.Fatalf("另一台仍应有余量：%+v", budgets[second.ID])
	}

	self, err := db.DispatchBudgets(ctx, inflight, []string{first.ID}, now)
	if err != nil {
		t.Fatalf("dispatch budgets (self): %v", err)
	}
	if got := self[first.ID]; got.PinnedUsed != 0 || !got.AdmitsOne() {
		t.Fatalf("任务自己的候选不得计入自己的余量：%+v", got)
	}

	expired, err := db.DispatchBudgets(ctx, next, []string{first.ID}, now+AssignmentWindowSec+1)
	if err != nil {
		t.Fatalf("dispatch budgets (expired): %v", err)
	}
	if !expired[first.ID].AdmitsOne() {
		t.Fatalf("窗口外的候选不得继续占余量：%+v", expired[first.ID])
	}
}

func TestDispatchBudgetsAfterProposalExpires(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := capPerNode(t, db, nsID, "triage-window", 1)
	node := v4Node(t, db, "host-ws", 3)

	stale := v4Task(t, db, nsID, c, "tr_681_stale", "")
	if _, err := db.ProposeTaskAssignment(ctx, AssignmentProposalInputs{
		TaskID: stale.ID, NodeID: node.ID, ProposedAttempt: 1, Now: Now() - AssignmentWindowSec - 1,
	}); err != nil {
		t.Fatalf("提议候选: %v", err)
	}
	next := v4Task(t, db, nsID, c, "tr_681_after_stale", "")
	budgets, err := db.DispatchBudgets(ctx, next, []string{node.ID}, 0)
	if err != nil {
		t.Fatalf("dispatch budgets: %v", err)
	}
	if !budgets[node.ID].AdmitsOne() {
		t.Fatalf("过期候选不得继续占余量：%+v", budgets[node.ID])
	}
}

func TestDispatchBudgetsAgreeWithGrantRejection(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := capPerNode(t, db, nsID, "triage-cap", 2)
	node := v4Node(t, db, "host-ws", 3)
	putV4NodeCfg(t, db, node.ID, []string{"*"}, nil, map[string]any{
		"total": 3, "perCapability": map[string]any{"triage-cap": 1},
	})
	v4RecordApplied(t, db, node.ID)

	running := v4Task(t, db, nsID, c, "tr_681_cap_running", "")
	v4Grant(t, db, running.ID, node.ID, "asg_681_cap_running")

	next := v4Task(t, db, nsID, c, "tr_681_cap_next", "")
	budgets, err := db.DispatchBudgets(ctx, next, []string{node.ID}, 0)
	if err != nil {
		t.Fatalf("dispatch budgets: %v", err)
	}
	got := budgets[node.ID]
	if !got.CapabilityLimited || got.CapabilityUsed != 1 || got.CapabilityLimit != 1 {
		t.Fatalf("按能力层应算出 1/1（perCapability triage-cap=1）：%+v", got)
	}
	if got.AdmitsOne() {
		t.Fatalf("按能力预算已满的节点不该 admits：%+v", got)
	}

	if err := db.AssignTask(ctx, next.ID, node.ID, "asg_681_cap_next", 1); err != nil {
		t.Fatalf("assign next: %v", err)
	}
	_, _, _, _, _, gerr := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID),
		next.ID, "asg_681_cap_next", 1, v4FreeSlot(t, db, node.ID), Now())
	if !errors.Is(gerr, ErrSlotUnavailable) || !strings.Contains(gerr.Error(), "能力预算") {
		t.Fatalf("预算表说满，grant 必须按同口径拒在按能力层，得到 %v", gerr)
	}
}

func TestDispatchBudgetsIgnoreTerminalTasks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := capPerNode(t, db, nsID, "triage-terminal", 1)
	node := v4Node(t, db, "host-ws", 3)

	done := v4Task(t, db, nsID, c, "tr_681_done", "")
	leaseID, _, attempt := v4Grant(t, db, done.ID, node.ID, "asg_681_done")
	if _, err := db.CompleteTaskClassifiedAuthorized(ctx, v4Auth(t, db, node.ID), done.ID, attempt,
		leaseID, Now(), TaskResult{Phase: TaskSucceeded}); err != nil {
		t.Fatalf("结束任务: %v", err)
	}

	next := v4Task(t, db, nsID, c, "tr_681_after_done", "")
	budgets, err := db.DispatchBudgets(ctx, next, []string{node.ID}, 0)
	if err != nil {
		t.Fatalf("dispatch budgets: %v", err)
	}
	if !budgets[node.ID].AdmitsOne() {
		t.Fatalf("终态任务不得继续占选点余量（#660）：%+v", budgets[node.ID])
	}
}
