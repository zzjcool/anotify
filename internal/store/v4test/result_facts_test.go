package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// Decode the existing wire-shaped facts so the missing-model baseline compiles:
// a red test must prove discarded facts / a false ACK, not a compile failure.
func resultFactsInput(t *testing.T, phase string, exit *int, start, finish time.Time, refs []protocol.TaskArtifact) store.TaskResult {
	t.Helper()
	if refs == nil {
		refs = []protocol.TaskArtifact{}
	}
	raw, err := json.Marshal(map[string]any{"phase": phase, "reason": "", "summary": "actual process", "error": "", "artifacts": refs, "exitCode": exit, "startedAt": start, "finishedAt": finish})
	if err != nil {
		t.Fatal(err)
	}
	var result store.TaskResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestC3bCompleteResultFullJSONNanoseconds(t *testing.T) {
	db, ns, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	refs := registerTestArtifact(t, db, auth, "full-facts", 1)
	exit := 0
	start := time.Unix(auth.Now-100, 123456789).UTC()
	finish := time.Unix(auth.Now-75, 987654321).UTC()
	result := resultFactsInput(t, store.TaskSucceeded, &exit, start, finish, refs)
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); err != nil {
		t.Fatal(err)
	}
	a, err := db.GetTaskAttempt(ctx, task.ID, auth.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	var facts map[string]any
	if err := json.Unmarshal(a.ResultJSON, &facts); err != nil {
		t.Fatal(err)
	}
	if facts["exitCode"] != float64(0) || facts["startedAt"] != start.Format(time.RFC3339Nano) || facts["finishedAt"] != finish.Format(time.RFC3339Nano) {
		t.Fatalf("complete result discarded exit/time facts: %s", a.ResultJSON)
	}
	got, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || got.StartedAt == nil || *got.StartedAt != start.Unix() || got.FinishedAt == nil || *got.FinishedAt != finish.Unix() || a.FinishedAt == nil || *a.FinishedAt != finish.Unix() {
		t.Fatalf("process facts replaced by acceptance time: task=%+v attempt=%+v err=%v", got, a, err)
	}
}

func TestC3bCompleteResultFullFactsDiscrimination(t *testing.T) {
	db, _, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	refs := registerTestArtifact(t, db, auth, "duplicate-facts", 1)
	exit := 0
	start := time.Unix(auth.Now-100, 123456789).UTC()
	finish := time.Unix(auth.Now-75, 987654321).UTC()
	nodeAuth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
	result := resultFactsInput(t, store.TaskSucceeded, &exit, start, finish, refs)
	if err := db.CompleteTaskAuthorized(ctx, nodeAuth, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); err != nil {
		t.Fatal(err)
	}
	beforeDuplicate := registryFacts(t, db)
	sameInstant := result
	sameInstant.StartedAt = start.In(time.FixedZone("node offset", 9*3600))
	sameInstant.FinishedAt = finish.In(time.FixedZone("node offset", -5*3600))
	if disposition, err := db.CompleteTaskClassifiedAuthorized(ctx, nodeAuth, task.ID, auth.Attempt, auth.LeaseID, auth.Now+100, sameInstant); err != nil || disposition != store.TaskResultDuplicate {
		t.Fatalf("equal instant full facts must ACK: %s %v", disposition, err)
	}
	if !reflect.DeepEqual(beforeDuplicate, registryFacts(t, db)) {
		t.Fatal("same full facts rewrote task/attempt/manifest/ledger/first acceptance")
	}
	for _, name := range []string{"nilExit", "differentExit", "startedNanos", "finishedNanos", "noStart", "noFinish", "reason", "summary", "error", "phase", "manifest"} {
		t.Run(name, func(t *testing.T) {
			code, s, f := &exit, start, finish
			switch name {
			case "nilExit":
				code = nil
			case "differentExit":
				other := 7
				code = &other
			case "startedNanos":
				s = s.Add(time.Nanosecond)
			case "finishedNanos":
				f = f.Add(time.Nanosecond)
			}
			changed := resultFactsInput(t, store.TaskSucceeded, code, s, f, refs)
			switch name {
			case "noStart":
				changed.StartedAt = time.Time{}
			case "noFinish":
				changed.FinishedAt = time.Time{}
			case "reason":
				changed.Reason = protocol.ReasonScriptFailed
			case "summary":
				changed.Summary += " changed"
			case "error":
				changed.Error = "different error"
			case "phase":
				changed.Phase = store.TaskFailed
			case "manifest":
				changed.Artifacts = []protocol.TaskArtifact{}
			}
			before := registryFacts(t, db)
			want := store.ErrInvalidTransition
			if name == "phase" {
				want = store.ErrLeaseMismatch
			}
			if disposition, err := db.CompleteTaskClassifiedAuthorized(ctx, nodeAuth, task.ID, auth.Attempt, auth.LeaseID, auth.Now+100, changed); !errors.Is(err, want) || disposition != "" {
				t.Fatalf("different complete %s facts ACKed: disposition=%s err=%v", name, disposition, err)
			}
			if !reflect.DeepEqual(before, registryFacts(t, db)) {
				t.Fatal("conflict changed whole task/attempt/manifest/ledger facts")
			}
		})
	}
}

// Full JSON is the precision carrier; Task/attempt second fields are convenient
// process projections. Reopening must not replace them with grant/reap/accept.
func TestC3bCompleteResultReopenNullableExitUTC(t *testing.T) {
	for _, beforeSpawn := range []bool{false, true} {
		t.Run(map[bool]string{false: "zeroExit", true: "beforeSpawnNilExit"}[beforeSpawn], func(t *testing.T) {
			db, ns, _, node, task, auth := uploadFixture(t)
			ctx := context.Background()
			var path string
			if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
				t.Fatal(err)
			}
			zero := 0
			exit := &zero
			phase := store.TaskSucceeded
			start := time.Unix(auth.Now-10, 123456789).In(time.FixedZone("Node", 8*3600))
			finish := time.Unix(auth.Now-1, 987654321).In(time.FixedZone("Node", 8*3600))
			if beforeSpawn {
				start, exit, phase = time.Time{}, nil, store.TaskFailed
			}
			result := resultFactsInput(t, phase, exit, start, finish, nil)
			if beforeSpawn {
				result.Reason = protocol.ReasonScriptFailed
				result.Error = "prerequisite unavailable before spawn"
			}
			caller := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
			if err := db.CompleteTaskAuthorized(ctx, caller, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); err != nil {
				t.Fatal(err)
			}
			before := registryFacts(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, err := reopened.GetTask(ctx, ns, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			a, err := reopened.GetTaskAttempt(ctx, task.ID, auth.Attempt)
			if err != nil {
				t.Fatal(err)
			}
			for _, raw := range []json.RawMessage{got.ResultJSON, a.ResultJSON} {
				var decoded store.TaskResult
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				want := result
				want.StartedAt, want.FinishedAt = start.UTC(), finish.UTC()
				if !reflect.DeepEqual(decoded, want) || decoded.StartedAt.Location() != time.UTC || decoded.FinishedAt.Location() != time.UTC {
					t.Fatalf("reopen lost complete normalized facts: got=%+v want=%+v JSON=%s", decoded, want, raw)
				}
			}
			if !reflect.DeepEqual(got.ExitCode, exit) || !reflect.DeepEqual(a.ExitCode, exit) || got.FinishedAt == nil || *got.FinishedAt != finish.Unix() || !reflect.DeepEqual(got.FinishedAt, a.FinishedAt) {
				t.Fatalf("nullable exit/process finish projection: task=%+v attempt=%+v", got, a)
			}
			if beforeSpawn && (got.StartedAt != nil || a.StartedAt != nil || a.ExitCode != nil) {
				t.Fatal("before-spawn failure fabricated process start/exit")
			}
			if !beforeSpawn && (got.StartedAt == nil || *got.StartedAt != start.Unix() || !reflect.DeepEqual(got.StartedAt, a.StartedAt)) {
				t.Fatal("process start projection differs from actual Node fact")
			}
			if got.ResultAcceptedAt == nil || *got.ResultAcceptedAt != auth.Now || !reflect.DeepEqual(got.ResultAcceptedAt, a.ResultAcceptedAt) || a.ReapedAt == nil || *a.ReapedAt != auth.Now {
				t.Fatal("trusted first observation/reap missing or confused with process clock")
			}
			for _, projection := range []any{got, a} {
				raw, err := json.Marshal(projection)
				if err != nil {
					t.Fatal(err)
				}
				var public map[string]any
				if err := json.Unmarshal(raw, &public); err != nil {
					t.Fatal(err)
				}
				for _, hidden := range []string{"resultAcceptedAt", "ResultAcceptedAt", "resultJSON", "resultJson", "ResultJSON"} {
					if _, exists := public[hidden]; exists {
						t.Fatalf("internal acceptance clock/raw facts leaked: %s", raw)
					}
				}
			}
			if disposition, err := reopened.CompleteTaskClassifiedAuthorized(ctx, caller, task.ID, auth.Attempt, auth.LeaseID, auth.Now+200, result); err != nil || disposition != store.TaskResultDuplicate {
				t.Fatalf("reopened full-facts duplicate: %s %v", disposition, err)
			}
			if !reflect.DeepEqual(before, registryFacts(t, reopened)) {
				t.Fatal("reopen/duplicate mutated any whole facts or first accepted time")
			}
			if beforeSpawn {
				changed := result
				changed.ExitCode = &zero
				if _, err := reopened.CompleteTaskClassifiedAuthorized(ctx, caller, task.ID, auth.Attempt, auth.LeaseID, auth.Now+201, changed); !errors.Is(err, store.ErrInvalidTransition) {
					t.Fatalf("nil exit falsely equals zero: %v", err)
				}
				if !reflect.DeepEqual(before, registryFacts(t, reopened)) {
					t.Fatal("nil-to-zero conflict wrote full facts")
				}
			}
		})
	}
}

func TestC3bCompleteResultMonotonicEquivalentDuplicate(t *testing.T) {
	db, _, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	zero := 0
	start := time.Now() // actual monotonic reading, unlike JSON-created times
	result := store.TaskResult{Phase: store.TaskSucceeded, ExitCode: &zero, StartedAt: start, FinishedAt: start.Add(time.Nanosecond)}
	caller := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
	if err := db.CompleteTaskAuthorized(ctx, caller, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); err != nil {
		t.Fatal(err)
	}
	result.StartedAt, result.FinishedAt = result.StartedAt.UTC(), result.FinishedAt.UTC()
	before := registryFacts(t, db)
	if disposition, err := db.CompleteTaskClassifiedAuthorized(ctx, caller, task.ID, auth.Attempt, auth.LeaseID, auth.Now+100, result); err != nil || disposition != store.TaskResultDuplicate {
		t.Fatalf("monotonic-less equivalent facts: %s %v", disposition, err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("monotonic normalization rewrote persisted facts")
	}
}

func TestC3bCompleteResultCurrentClockAfterSlowUpload(t *testing.T) {
	db, ns, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	caller := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
	for _, elapsed := range []int64{20, 40, 60} {
		if _, err := db.RenewLeaseAuthorized(ctx, caller, task.ID, auth.Attempt, auth.LeaseID, auth.Now+elapsed); err != nil {
			t.Fatalf("renew current valid lease at +%ds: %v", elapsed, err)
		}
	}
	accepted := auth.Now + 75
	uploadAuth := auth
	uploadAuth.Now = accepted
	refs := registerTestArtifact(t, db, uploadAuth, "slow-upload", 1)
	zero := 0
	finish := time.Unix(auth.Now, 765432109).UTC()
	result := store.TaskResult{Phase: store.TaskSucceeded, ExitCode: &zero, StartedAt: finish.Add(-time.Second), FinishedAt: finish, Artifacts: refs}
	if err := db.CompleteTaskAuthorized(ctx, caller, task.ID, auth.Attempt, auth.LeaseID, accepted, result); err != nil {
		t.Fatalf("current renewed lease should accept original process finish: %v", err)
	}
	got, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.GetTaskAttempt(ctx, task.ID, auth.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if got.FinishedAt == nil || *got.FinishedAt != finish.Unix() || !reflect.DeepEqual(got.FinishedAt, a.FinishedAt) || got.ResultAcceptedAt == nil || *got.ResultAcceptedAt != accepted || !reflect.DeepEqual(got.ResultAcceptedAt, a.ResultAcceptedAt) || a.ReapedAt == nil || *a.ReapedAt != accepted {
		t.Fatalf("process finish and current acceptance/reap clocks mixed: task=%+v attempt=%+v", got, a)
	}
	var facts store.TaskResult
	if err := json.Unmarshal(got.ResultJSON, &facts); err != nil || !facts.FinishedAt.Equal(finish) {
		t.Fatalf("original finish nanoseconds lost: %s %v", got.ResultJSON, err)
	}
}

func TestC3bCompleteResultOldFinishCannotReviveExpiredLease(t *testing.T) {
	for _, atExpiry := range []bool{true, false} {
		t.Run(map[bool]string{true: "equal", false: "past"}[atExpiry], func(t *testing.T) {
			db, _, _, node, task, auth := uploadFixture(t)
			ctx := context.Background()
			refs := registerTestArtifact(t, db, auth, "expired-lease", 1)
			var expires int64
			if err := db.QueryRow(`SELECT lease_until FROM tasks WHERE id=?`, task.ID).Scan(&expires); err != nil {
				t.Fatal(err)
			}
			serverNow := expires
			if !atExpiry {
				serverNow++
			}
			finish := time.Unix(auth.Now-100*365*86400, 987654321).UTC()
			zero := 0
			result := store.TaskResult{Phase: store.TaskSucceeded, ExitCode: &zero, StartedAt: finish.Add(-time.Second), FinishedAt: finish, Artifacts: refs}
			before := registryFacts(t, db)
			if _, err := db.CompleteTaskClassifiedAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, serverNow, result); !errors.Is(err, store.ErrLeaseMismatch) {
				t.Fatalf("old Node finish bypassed CURRENT expired server lease: %v", err)
			}
			if !reflect.DeepEqual(before, registryFacts(t, db)) {
				t.Fatal("expired result wrote any complete facts/manifest/slot/ledger")
			}
		})
	}
}

func TestC3bCompleteResultBasicTimeOrderZeroWrite(t *testing.T) {
	db, _, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	finish := time.Unix(auth.Now, 123456789).UTC()
	before := registryFacts(t, db)
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, store.TaskResult{Phase: store.TaskFailed, StartedAt: finish.Add(time.Nanosecond), FinishedAt: finish}); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("both-present reversed times must reject: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("invalid time order wrote facts")
	}
}

func TestC3bCompleteResultOldFutureClocksTrustedRetention(t *testing.T) {
	for _, years := range []int64{-100, 100} {
		t.Run(map[int64]string{-100: "veryOldNode", 100: "futureNode"}[years], func(t *testing.T) {
			db, ns, _, node, task, auth := uploadFixture(t)
			ctx := context.Background()
			const day = int64(86400)
			setRetention(t, db, 0, 30*day, 7*day)
			refs := registerTestArtifact(t, db, auth, "retained", 1)
			finish := time.Unix(auth.Now+years*365*day, 123456789).UTC()
			zero := 0
			result := store.TaskResult{Phase: store.TaskSucceeded, ExitCode: &zero, StartedAt: finish.Add(-time.Second), FinishedAt: finish, Artifacts: refs}
			if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); err != nil {
				t.Fatalf("Node time must not trigger an invented skew window: %v", err)
			}
			if ids, err := db.ExpireArtifactObjects(ctx, auth.Now+7*day-1, 20); err != nil || len(ids) != 0 {
				t.Fatalf("Node clock prematurely expired bytes: ids=%v err=%v", ids, err)
			}
			if stats, err := db.RunRetention(ctx, auth.Now+30*day-1); err != nil || stats.TasksDeleted != 0 {
				t.Fatalf("facts did not use trusted 30-day observation: %+v %v", stats, err)
			}
			ids, err := db.ExpireArtifactObjects(ctx, auth.Now+7*day, 20)
			if err != nil || len(ids) != 1 {
				t.Fatalf("Node future finish delayed trusted 7-day bytes GC: ids=%v err=%v", ids, err)
			}
			if err := db.AckArtifactObjectPurged(ctx, ids[0], auth.Now+7*day); err != nil {
				t.Fatal(err)
			}
			if _, err := db.GetTaskArtifactDownload(ctx, ns, task.ID, refs[0].SHA256, auth.Now+7*day); !errors.Is(err, store.ErrArtifactExpired) {
				t.Fatalf("expired bytes must retain 410 facts: %v", err)
			}
			got, err := db.GetTask(ctx, ns, task.ID)
			if err != nil || got.FinishedAt == nil || *got.FinishedAt != finish.Unix() {
				t.Fatalf("bytes GC rewrote actual process finish: %+v %v", got, err)
			}
			before := registryFacts(t, db)
			if disposition, err := db.CompleteTaskClassifiedAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now+7*day, result); err != nil || disposition != store.TaskResultDuplicate {
				t.Fatalf("expired bytes with retained full refs should still ACK: %s %v", disposition, err)
			}
			if !reflect.DeepEqual(before, registryFacts(t, db)) {
				t.Fatal("repeat after bytes GC rewrote accepted clock/full facts")
			}
			if stats, err := db.RunRetention(ctx, auth.Now+30*day+1); err != nil || stats.TasksDeleted != 1 {
				t.Fatalf("old/future Node time altered trusted fact GC: %+v %v", stats, err)
			}
		})
	}
}

func TestC3bCompleteResultTrustedRetentionActiveUnreapedProtected(t *testing.T) {
	for _, guard := range []string{"activeUpload", "unreaped", "running"} {
		t.Run(guard, func(t *testing.T) {
			db, ns, _, node, task, auth := uploadFixture(t)
			ctx := context.Background()
			refs := registerTestArtifact(t, db, auth, "protected", 1)
			u, err := db.BeginArtifactUpload(ctx, auth, artifactInput(auth, "active", 1))
			if err != nil {
				t.Fatal(err)
			}
			finish := time.Unix(auth.Now-100*365*86400, 123456789).UTC()
			if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, store.TaskResult{Phase: store.TaskSucceeded, FinishedAt: finish, Artifacts: refs}); err != nil {
				t.Fatal(err)
			}
			// Isolate each retained guard; these are corrupt/stuck-runtime fixtures,
			// not production ways to reopen a terminal attempt or extend a ticket.
			if guard == "activeUpload" {
				if _, err := db.Exec(`UPDATE task_artifact_uploads SET expires_at=? WHERE task_id=?`, auth.Now+40*86400, task.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.AbortArtifactUpload(ctx, u); err != nil {
					t.Fatal(err)
				}
				sql := `UPDATE task_attempts SET reaped_at=NULL WHERE task_id=? AND attempt=?`
				if guard == "running" {
					if _, err := db.Exec(`UPDATE tasks SET phase='running' WHERE id=?`, task.ID); err != nil {
						t.Fatal(err)
					}
				} else if _, err := db.Exec(sql, task.ID, auth.Attempt); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := db.DeleteExpiredTasks(ctx, auth.Now+31*86400, 20); err != nil || n != 0 {
				t.Fatalf("%s must protect expired full facts: n=%d err=%v", guard, n, err)
			}
			ids, err := db.ExpireArtifactObjects(ctx, auth.Now+31*86400, 20)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := db.ListArtifactManifest(ctx, ns, task.ID, auth.Attempt)
			if err != nil || len(entries) != 1 || entries[0].ExpiredAt != nil {
				t.Fatalf("%s must protect manifest bytes: %+v %v", guard, entries, err)
			}
			for _, id := range ids {
				if id.String() == entries[0].ObjectID.String() {
					t.Fatalf("%s collected protected object", guard)
				}
			}
		})
	}
}

func TestC3bCompleteResultMissingFactsNeverInferACK(t *testing.T) {
	for _, missing := range []string{"taskJSON", "attemptJSON", "taskAccepted", "attemptAccepted", "manifest"} {
		t.Run(missing, func(t *testing.T) {
			db, _, _, node, task, auth := uploadFixture(t)
			ctx := context.Background()
			refs := registerTestArtifact(t, db, auth, "recorded", 1)
			zero := 0
			finish := time.Unix(auth.Now-100*365*86400, 123456789).UTC()
			result := store.TaskResult{Phase: store.TaskSucceeded, ExitCode: &zero, StartedAt: finish.Add(-time.Second), FinishedAt: finish, Artifacts: refs}
			caller := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
			if err := db.CompleteTaskAuthorized(ctx, caller, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); err != nil {
				t.Fatal(err)
			}
			q := map[string]string{
				"taskJSON":        `UPDATE tasks SET result_json='' WHERE id=?`,
				"attemptJSON":     `UPDATE task_attempts SET result_json='' WHERE task_id=?`,
				"taskAccepted":    `UPDATE tasks SET result_accepted_at=NULL WHERE id=?`,
				"attemptAccepted": `UPDATE task_attempts SET result_accepted_at=NULL WHERE task_id=?`,
				"manifest":        `DELETE FROM task_artifact_manifests WHERE task_id=?`,
			}[missing]
			if _, err := db.Exec(q, task.ID); err != nil {
				t.Fatal(err)
			}
			before := registryFacts(t, db)
			want := store.ErrInvalidTransition
			if missing == "manifest" {
				want = store.ErrArtifactReference
			}
			if disposition, err := db.CompleteTaskClassifiedAuthorized(ctx, caller, task.ID, auth.Attempt, auth.LeaseID, auth.Now+100, result); !errors.Is(err, want) || disposition != "" {
				t.Fatalf("missing %s inferred ACK: %s %v", missing, disposition, err)
			}
			if !reflect.DeepEqual(before, registryFacts(t, db)) {
				t.Fatal("missing evidence replay rewrote facts")
			}
			if missing == "taskAccepted" {
				if n, err := db.DeleteExpiredTasks(ctx, auth.Now+31*86400, 20); err != nil || n != 0 {
					t.Fatalf("missing trusted observation fell back to untrusted Node finish: %d %v", n, err)
				}
			}
		})
	}
}
