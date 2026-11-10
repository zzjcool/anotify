package v4test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// mintWireSession captures the credential presented by a test connection. Result
// callers never fetch nodes.token_hash (or the task's stored token) to authorize themselves.
func mintWireSession(t *testing.T, db *store.DB, nodeID, credential string) store.NodeAuthorization {
	t.Helper()
	auth := store.NodeAuthorization{NodeID: nodeID, NodeTokenHash: store.HashNodeToken(credential)}
	if err := db.SetNodeTokenHash(context.Background(), nodeID, auth.NodeTokenHash); err != nil {
		t.Fatalf("mint session: %v", err)
	}
	return auth
}

// wireRows snapshots every column, including historical execution and physical
// slot facts, rather than just the phase or busy count.
func wireRows(t *testing.T, db *store.DB, query string, args ...any) [][]any {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("snapshot query: %v", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("snapshot columns: %v", err)
	}
	out := [][]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("snapshot row: %v", err)
		}
		for i, value := range values {
			if b, ok := value.([]byte); ok {
				values[i] = string(b)
			}
		}
		out = append(out, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return out
}

func wireExecutionFacts(t *testing.T, db *store.DB, taskID, nodeID string) [][][]any {
	t.Helper()
	return [][][]any{
		wireRows(t, db, `SELECT * FROM tasks WHERE id = ?`, taskID),
		wireRows(t, db, `SELECT * FROM task_attempts WHERE task_id = ? ORDER BY attempt`, taskID),
		wireRows(t, db, `SELECT * FROM node_instances WHERE node_id = ? ORDER BY slot`, nodeID),
	}
}

func assertWireFacts(t *testing.T, before, after [][][]any) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected result changed task/whole attempt/slot facts:\nbefore=%v\nafter=%v", before, after)
	}
}

func grantWireTask(t *testing.T, db *store.DB, auth store.NodeAuthorization, taskID, assignment string, proposed, slot int, now int64) (string, int, int64) {
	t.Helper()
	ctx := context.Background()
	if err := db.AssignTaskAt(ctx, taskID, auth.NodeID, assignment, proposed, now); err != nil {
		t.Fatalf("assign: %v", err)
	}
	lease, token, attempt, expiry, err := db.GrantLeaseAuthorized(ctx, auth, taskID, assignment, proposed, slot, now)
	if err != nil || lease == "" || token == "" || attempt <= 0 {
		t.Fatalf("grant: lease=%q attempt=%d expiry=%d err=%v", lease, attempt, expiry, err)
	}
	// The grant secret is deliberately not returned to the result caller: task.result
	// carries taskId/attempt/leaseId, not plaintext attemptToken.
	return lease, attempt, expiry
}

// TestCompleteTaskActualResultWireAuthority reproduces a valid task.result with no
// attemptToken and distinguishes the actual captured session from a rotated DB identity.
func TestCompleteTaskActualResultWireAuthority(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "wire-result")
	cap := capWithPinnedMaxPerNode(t, db, ns, "idem", 3, 3)
	node := newNode(t, db, "wire-node", 1)
	oldAuth := mintWireSession(t, db, node.ID, "ant_node_wire_old_minted")
	task := newTask(t, db, ns, cap, "trace-wire", "")
	now := store.Now()
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "asg-wire", 1, now); err != nil {
		t.Fatal(err)
	}
	lease, uploadToken, attempt, _, err := db.GrantLeaseAuthorized(ctx, oldAuth, task.ID, "asg-wire", 1, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	beforeAttempt, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	currentAuth := mintWireSession(t, db, node.ID, "ant_node_wire_current_minted")
	refs := registerTestArtifact(t, db, store.EmitAuth{TaskID: task.ID, NodeID: node.ID, NodeTokenHash: currentAuth.NodeTokenHash, Attempt: attempt, LeaseID: lease, AttemptToken: uploadToken, Now: now}, "out/report.json", 1)
	exitCode := 0
	result := store.TaskResult{Phase: store.TaskSucceeded, Summary: "actual wire", Artifacts: refs, ExitCode: &exitCode, StartedAt: time.Unix(now, 0), FinishedAt: time.Unix(now+1, 0)}
	before := wireExecutionFacts(t, db, task.ID, node.ID)
	if err := db.CompleteTaskAuthorized(ctx, oldAuth, task.ID, attempt, lease, now+1, result); !errors.Is(err, store.ErrNodeIdentityMismatch) {
		t.Fatalf("rotated captured session must reject: %v", err)
	}
	assertWireFacts(t, before, wireExecutionFacts(t, db, task.ID, node.ID))
	if err := db.CompleteTaskAuthorized(ctx, currentAuth, task.ID, attempt, lease, now+1, result); err != nil {
		t.Fatalf("current captured session's actual tokenless result must succeed: %v", err)
	}
	got := mustPhase(t, db, ns, task.ID, store.TaskSucceeded)
	if !got.Reported || got.Summary != result.Summary || !reflect.DeepEqual(got.Artifacts, result.Artifacts) || got.FinishedAt == nil || *got.FinishedAt != now+1 {
		t.Fatalf("result facts missing: %+v", got)
	}
	afterAttempt, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil || afterAttempt.ReapedAt == nil || *afterAttempt.ReapedAt != now+1 || afterAttempt.FinishedAt == nil || *afterAttempt.FinishedAt != now+1 {
		t.Fatalf("result must record exact execution/reap time: %+v err=%v", afterAttempt, err)
	}
	if beforeAttempt.ConfigHash != afterAttempt.ConfigHash || beforeAttempt.ConfigRevision != afterAttempt.ConfigRevision ||
		!reflect.DeepEqual(beforeAttempt.MachineVars, afterAttempt.MachineVars) || !reflect.DeepEqual(beforeAttempt.MachineLimits, afterAttempt.MachineLimits) ||
		!reflect.DeepEqual(beforeAttempt.MachineVarSources, afterAttempt.MachineVarSources) || beforeAttempt.NamespaceVarsRevision != afterAttempt.NamespaceVarsRevision {
		t.Fatalf("result changed pinned D2 snapshot: before=%+v after=%+v", beforeAttempt, afterAttempt)
	}
	if busySlots(t, db, node.ID) != 0 {
		t.Fatal("valid result must release its own physical slot")
	}
}

// TestCompleteTaskActualResultRefusalsZeroFacts migrates token-fiction security
// semantics to actual wire authority. Every rejection compares all task, attempt
// and slot columns; an old result cannot touch the new attempt or historical facts.
func TestCompleteTaskActualResultRefusalsZeroFacts(t *testing.T) {
	for _, name := range []string{"wrongCredential", "wrongNode", "wrongLease", "oldAttempt", "authorizedNamespace", "declaredNamespace", "cancel", "expiryEqual", "expiryPast"} {
		t.Run(name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			ns := newNS(t, db, "wire-refuse")
			cap := capWithPinnedMaxPerNode(t, db, ns, "idem", 3, 3)
			node := newNode(t, db, "wire-node", 2)
			auth := mintWireSession(t, db, node.ID, "ant_node_wire_refusal_minted")
			task := newTask(t, db, ns, cap, "trace-refuse", "")
			now := store.Now()
			lease, attempt, expiry := grantWireTask(t, db, auth, task.ID, "asg-old", 1, 0, now)
			caller, resultLease, resultAttempt, resultNow := auth, lease, attempt, now+1
			want := store.ErrLeaseMismatch
			switch name {
			case "wrongCredential":
				caller.NodeTokenHash = store.HashNodeToken("ant_node_wire_wrong_presented")
				want = store.ErrNodeIdentityMismatch
			case "wrongNode":
				other := newNode(t, db, "other-node", 1)
				caller = mintWireSession(t, db, other.ID, "ant_node_wire_other_minted")
			case "wrongLease":
				resultLease = "lse_not_the_granted_binding"
			case "oldAttempt":
				if _, err := db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, now-1, task.ID); err != nil {
					t.Fatalf("expire old lease: %v", err)
				}
				if requeued, abandoned, err := db.RecoverLostLeases(ctx, now); err != nil || requeued != 1 || abandoned != 0 {
					t.Fatalf("recover old attempt: requeued=%d abandoned=%d err=%v", requeued, abandoned, err)
				}
				_, newAttempt, _ := grantWireTask(t, db, auth, task.ID, "asg-new", 2, 1, now)
				if newAttempt != attempt+1 || busySlots(t, db, node.ID) != 2 {
					t.Fatalf("old live attempt and current attempt must coexist: new=%d busy=%d", newAttempt, busySlots(t, db, node.ID))
				}
			case "authorizedNamespace":
				putNodeCfg(t, db, node.ID, []string{"other"}, nil, map[string]any{"total": 2})
				want = store.ErrEmitNamespaceDenied
			case "declaredNamespace":
				if err := db.RecordHello(ctx, node.ID, store.NodeFacts{Hostname: node.Name}, nil, []string{"other"}, nil, nil, "test-agent", "test-commit"); err != nil {
					t.Fatalf("revoke declaration: %v", err)
				}
				want = store.ErrEmitNamespaceDenied
			case "cancel":
				if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, now); err != nil {
					t.Fatalf("cancel: %v", err)
				}
				want = store.ErrTaskNotRunning
			case "expiryEqual":
				resultNow = expiry
			case "expiryPast":
				resultNow = expiry + 1
			}
			before := wireExecutionFacts(t, db, task.ID, node.ID)
			if err := db.CompleteTaskAuthorized(ctx, caller, task.ID, resultAttempt, resultLease, resultNow,
				store.TaskResult{Phase: store.TaskSucceeded, Summary: "must not write", Artifacts: []protocol.TaskArtifact{{Path: "out/forbidden", SHA256: strings.Repeat("a", 64)}}}); !errors.Is(err, want) {
				t.Fatalf("actual result refusal must be %v, got %v", want, err)
			}
			assertWireFacts(t, before, wireExecutionFacts(t, db, task.ID, node.ID))
		})
	}
}

func assertTotalGrantRejected(t *testing.T, db *store.DB, ns, taskID, nodeID string, slot int, err error) {
	t.Helper()
	if !errors.Is(err, store.ErrSlotUnavailable) || !strings.Contains(err.Error(), "机器总量预算已满") {
		t.Fatalf("legal empty slot must be rejected by total occupancy alone: %v", err)
	}
	task := mustPhase(t, db, ns, taskID, store.TaskPending)
	if task.Attempt != 0 || task.LeaseID != "" || task.LeaseUntil != nil || task.WaitingReason != store.WaitingForSlot || task.NextAttemptAt == nil {
		t.Fatalf("total rejection must consume no attempt/lease and wait boundedly: %+v", task)
	}
	if rows := wireRows(t, db, `SELECT * FROM task_attempts WHERE task_id = ?`, taskID); len(rows) != 0 {
		t.Fatalf("rejected task must have no attempt records: %v", rows)
	}
	if busy, attempt, lease, assignment, expiry := instanceLedger(t, db, nodeID, slot); busy != "" || attempt != 0 || lease != "" || assignment != "" || expiry != 0 {
		t.Fatalf("rejected slot must be wholly free: %q/%d/%q/%q/%d", busy, attempt, lease, assignment, expiry)
	}
}

// TestGrantTotalShrinkCountsOldHigherSlotOnly: total2, A in slot1, then total1.
// Slot0 remains legal and empty. perCapability/maxPerNode=10 cannot confound this
// discriminator; no reap or fake release makes the old physical occupancy disappear.
func TestGrantTotalShrinkCountsOldHigherSlotOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "total-only")
	cap := capWithPinnedMaxPerNode(t, db, ns, "idem", 10, 3)
	node := newNode(t, db, "total-node", 2)
	auth := mintWireSession(t, db, node.ID, "ant_node_total_single_minted")
	putNodeCfg(t, db, node.ID, []string{"*"}, nil, map[string]any{"total": 2, "perCapability": map[string]any{"idem": 10}})
	recordAppliedForNode(t, db, node.ID)
	now := store.Now()
	old := newTask(t, db, ns, cap, "trace-old", "")
	grantWireTask(t, db, auth, old.ID, "asg-old", 1, 1, now)
	oldFacts := wireExecutionFacts(t, db, old.ID, node.ID)
	putNodeCfg(t, db, node.ID, []string{"*"}, nil, map[string]any{"total": 1, "perCapability": map[string]any{"idem": 10}})
	recordAppliedForNode(t, db, node.ID)
	assertWireFacts(t, oldFacts, wireExecutionFacts(t, db, old.ID, node.ID))
	fresh := newTask(t, db, ns, cap, "trace-fresh", "")
	if err := db.AssignTaskAt(ctx, fresh.ID, node.ID, "asg-fresh", 1, now); err != nil {
		t.Fatalf("assign fresh: %v", err)
	}
	freeBefore := wireRows(t, db, `SELECT * FROM node_instances WHERE node_id = ? AND slot = 0`, node.ID)
	_, _, _, _, err := db.GrantLeaseAuthorized(ctx, auth, fresh.ID, "asg-fresh", 1, 0, now+1)
	assertTotalGrantRejected(t, db, ns, fresh.ID, node.ID, 0, err)
	assertWireFacts(t, oldFacts, wireExecutionFacts(t, db, old.ID, node.ID))
	if !reflect.DeepEqual(freeBefore, wireRows(t, db, `SELECT * FROM node_instances WHERE node_id = ? AND slot = 0`, node.ID)) || busySlots(t, db, node.ID) != 1 {
		t.Fatal("total rejection must not bind the legal slot0 or fake-release old slot1")
	}
}

// TestConcurrentTotalShrinkGrantsExactlyOne: total3, old live slot2, then total2.
// Two new tasks race for legal empty slots0/1 with perCapability/maxPerNode=10.
// Only total enforcement can keep the old+new unreaped occupancy at two.
func TestConcurrentTotalShrinkGrantsExactlyOne(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "total-only-race")
	cap := capWithPinnedMaxPerNode(t, db, ns, "idem", 10, 3)
	node := newNode(t, db, "total-node", 3)
	auth := mintWireSession(t, db, node.ID, "ant_node_total_race_minted")
	putNodeCfg(t, db, node.ID, []string{"*"}, nil, map[string]any{"total": 3, "perCapability": map[string]any{"idem": 10}})
	recordAppliedForNode(t, db, node.ID)
	now := store.Now()
	old := newTask(t, db, ns, cap, "trace-old", "")
	grantWireTask(t, db, auth, old.ID, "asg-old", 1, 2, now)
	oldTask := wireRows(t, db, `SELECT * FROM tasks WHERE id = ?`, old.ID)
	oldAttempt := wireRows(t, db, `SELECT * FROM task_attempts WHERE task_id = ?`, old.ID)
	oldSlot := wireRows(t, db, `SELECT * FROM node_instances WHERE node_id = ? AND slot = 2`, node.ID)
	putNodeCfg(t, db, node.ID, []string{"*"}, nil, map[string]any{"total": 2, "perCapability": map[string]any{"idem": 10}})
	recordAppliedForNode(t, db, node.ID)
	tasks := []*store.Task{newTask(t, db, ns, cap, "trace-a", ""), newTask(t, db, ns, cap, "trace-b", "")}
	assignments := []string{"asg-a", "asg-b"}
	freeBefore := make([][][]any, 2)
	for i, task := range tasks {
		if err := db.AssignTaskAt(ctx, task.ID, node.ID, assignments[i], 1, now); err != nil {
			t.Fatalf("assign %d: %v", i, err)
		}
		freeBefore[i] = wireRows(t, db, `SELECT * FROM node_instances WHERE node_id = ? AND slot = ?`, node.ID, i)
		if busy, _, _, _, _ := instanceLedger(t, db, node.ID, i); busy != "" {
			t.Fatalf("slot%d must be truly empty before the race: %q", i, busy)
		}
	}
	results := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range tasks {
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, _, _, results[i] = db.GrantLeaseAuthorized(ctx, auth, tasks[i].ID, assignments[i], 1, i, now+1)
		}(i)
	}
	close(start)
	wg.Wait()
	success, rejected := 0, 0
	for _, err := range results {
		if err == nil {
			success++
		} else {
			rejected++
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatalf("total-only transaction must admit exactly one legal new grant: success=%d rejected=%d results=%v", success, rejected, results)
	}
	for i, err := range results {
		if err != nil {
			assertTotalGrantRejected(t, db, ns, tasks[i].ID, node.ID, i, err)
			if !reflect.DeepEqual(freeBefore[i], wireRows(t, db, `SELECT * FROM node_instances WHERE node_id = ? AND slot = ?`, node.ID, i)) {
				t.Fatalf("loser slot%d changed", i)
			}
			continue
		}
		got := mustPhase(t, db, ns, tasks[i].ID, store.TaskRunning)
		att, aerr := db.GetTaskAttempt(ctx, got.ID, 1)
		if got.Attempt != 1 || got.LeaseID == "" || aerr != nil || att.Slot != i || att.LeaseID != got.LeaseID || att.ReapedAt != nil {
			t.Fatalf("winner must own exactly its legal slot%d/attempt1: task=%+v attempt=%+v err=%v", i, got, att, aerr)
		}
	}
	if !reflect.DeepEqual(oldTask, wireRows(t, db, `SELECT * FROM tasks WHERE id = ?`, old.ID)) ||
		!reflect.DeepEqual(oldAttempt, wireRows(t, db, `SELECT * FROM task_attempts WHERE task_id = ?`, old.ID)) ||
		!reflect.DeepEqual(oldSlot, wireRows(t, db, `SELECT * FROM node_instances WHERE node_id = ? AND slot = 2`, node.ID)) {
		t.Fatal("shrinking total/racing new grants must preserve every old task/attempt/slot fact")
	}
	var unreaped int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_attempts WHERE node_id = ? AND reaped_at IS NULL`, node.ID).Scan(&unreaped); err != nil || unreaped != 2 || busySlots(t, db, node.ID) != 2 {
		t.Fatalf("old live slot2 must still count: unreaped=%d busy=%d err=%v", unreaped, busySlots(t, db, node.ID), err)
	}
}
