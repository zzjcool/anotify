package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// Parent's latest decision: retained history is evidence ONLY. An old completed
// result may not ACK after a different current attempt/lease/node binding exists.
func TestC3bDuplicateResultRequiresCurrentBindingZeroFacts(t *testing.T) {
	db, ns, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	refs := registerTestArtifact(t, db, auth, "out", 1)
	result := store.TaskResult{Phase: store.TaskFailed, Reason: protocol.ReasonScriptFailed, Summary: "failed", Error: "original error", Artifacts: refs}
	nodeAuth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
	if err := db.CompleteTaskAuthorized(ctx, nodeAuth, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"payload", "manifest", "lease", "node", "rotated"} {
		t.Run(fault, func(t *testing.T) {
			supplied := result
			caller := nodeAuth
			lease := auth.LeaseID
			switch fault {
			case "payload":
				supplied.Error = "wrong"
			case "manifest":
				supplied.Artifacts = []protocol.TaskArtifact{{Path: refs[0].Path, SHA256: strings.Repeat("b", 64)}}
			case "lease":
				lease = "old-wrong-lease"
			case "node":
				caller.NodeID = "wrong-node"
			case "rotated":
				caller.NodeTokenHash = store.HashNodeToken("not-current")
			}
			before := registryFacts(t, db)
			if _, err := db.CompleteTaskClassifiedAuthorized(ctx, caller, task.ID, auth.Attempt, lease, auth.Now+100, supplied); err == nil {
				t.Fatal("wrong duplicate ACKed")
			}
			if !reflect.DeepEqual(before, registryFacts(t, db)) {
				t.Fatal("wrong duplicate mutated facts")
			}
		})
	}
	// A deterministic new-current-binding fault fixture (not a production retry of
	// terminal tasks) exercises the retained-history branch while a REAL grant
	// creates the new attempt+lease+slot with its usual authority checks.
	if _, err := db.Exec(`UPDATE tasks SET phase='pending',reported=0,lease_id='',lease_until=NULL,node_id=NULL,finished_at=NULL WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "new-attempt", 2, auth.Now+1); err != nil {
		t.Fatal(err)
	}
	lease, _, attempt, _, err := db.GrantLeaseAuthorized(ctx, nodeAuth, task.ID, "new-attempt", 2, 0, auth.Now+1)
	if err != nil {
		t.Fatal(err)
	}
	if attempt != 2 || lease == auth.LeaseID {
		t.Fatal("fixture needs new execution binding")
	}
	before := registryFacts(t, db)
	if _, err := db.CompleteTaskClassifiedAuthorized(ctx, nodeAuth, task.ID, auth.Attempt, auth.LeaseID, auth.Now+2, result); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("old history ACKed across new current: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("old result touched new current or history")
	}
	got, err := db.GetTask(ctx, ns, task.ID)
	if err != nil || got.Attempt != 2 || got.LeaseID != lease || got.Phase != store.TaskRunning {
		t.Fatalf("new attempt changed: %+v %v", got, err)
	}
	// Removing actual persisted result evidence must never turn terminal phase
	// into an inferred ACK. Keep another terminal task for that independent case.
	if err := db.CompleteTaskAuthorized(ctx, nodeAuth, task.ID, attempt, lease, auth.Now+2, store.TaskResult{Phase: store.TaskSucceeded}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE task_attempts SET result_json='' WHERE task_id=? AND attempt=?`, task.ID, attempt); err != nil {
		t.Fatal(err)
	}
	before = registryFacts(t, db)
	if _, err := db.CompleteTaskClassifiedAuthorized(ctx, nodeAuth, task.ID, attempt, lease, auth.Now+100, store.TaskResult{Phase: store.TaskSucceeded}); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("terminal phase inferred ACK without result evidence: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("missing evidence replay wrote")
	}
}

func TestC3bConcurrentSamePathRegistrationActualOwner(t *testing.T) {
	db, _, _, _, _, auth := uploadFixture(t)
	ctx := context.Background()
	in := artifactInput(auth, "same", 16<<20)
	tickets := make([]*store.ArtifactUpload, 2)
	for i := range tickets {
		var err error
		tickets[i], err = db.BeginArtifactUpload(ctx, auth, in)
		if err != nil {
			t.Fatal(err)
		}
	}
	if tickets[0].ObjectID().String() == tickets[1].ObjectID().String() {
		t.Fatal("system identity reused")
	}
	outs := make([]store.ArtifactUploadRegistration, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range tickets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outs[i], errs[i] = db.RegisterArtifactUploadClassified(ctx, auth, tickets[i], in.SHA256, in.Size)
		}(i)
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("same-path concurrency: %v", errs)
	}
	if outs[0].Status+outs[1].Status != 401 || outs[0].PublishedObjectID.String() != outs[1].PublishedObjectID.String() {
		t.Fatalf("need one201 one200 same actual owner: %+v", outs)
	}
	var count int
	var bytes int64
	if err := db.QueryRow(`SELECT COUNT(*),SUM(size) FROM task_artifact_manifests WHERE task_id=? AND attempt=?`, in.TaskID, in.Attempt).Scan(&count, &bytes); err != nil {
		t.Fatal(err)
	}
	if count != 1 || bytes != in.Size {
		t.Fatalf("doublecharge: %d/%d", count, bytes)
	}
	ids, err := db.ExpireArtifactObjects(ctx, auth.Now+1, 20)
	if err != nil {
		t.Fatal(err)
	}
	orphan := 0
	for _, id := range ids {
		if id.String() == outs[0].PublishedObjectID.String() {
			t.Fatal("winner object GCed")
		}
		orphan++
	}
	if orphan != 1 {
		t.Fatalf("duplicate orphan GC work=%d", orphan)
	}
	// Authorization still comes before already-registered duplicate detection.
	bad := auth
	bad.AttemptToken = "bad"
	before := registryFacts(t, db)
	if _, _, err := db.RegisterArtifactUpload(ctx, bad, tickets[0], in.SHA256, in.Size); !errors.Is(err, store.ErrAttemptTokenInvalid) {
		t.Fatalf("idempotence bypass token: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("unauthorized repeat wrote")
	}
}

func TestC3bTypedEmptyArraysAndExpiryPartialAttempt(t *testing.T) {
	db, ns, _, _, task, auth := uploadFixture(t)
	ctx := context.Background()
	a, err := db.GetTaskAttempt(ctx, task.ID, auth.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{task.Artifacts, a.Artifacts, store.TaskResult{Phase: store.TaskSucceeded}} {
		raw, err := json.Marshal(v)
		if err != nil || (string(raw) != "[]" && !strings.Contains(string(raw), `"artifacts":[]`)) {
			t.Fatalf("typed empty arrays: %s %v", raw, err)
		}
	}
	refs := registerTestArtifact(t, db, auth, "partial", 1)
	if _, _, err := db.RecoverLostLeases(ctx, auth.Now+60); err != nil {
		t.Fatal(err)
	}
	a, err = db.GetTaskAttempt(ctx, task.ID, auth.Attempt)
	if err != nil || !reflect.DeepEqual(a.Artifacts, refs) {
		t.Fatalf("expiry lost uploaded attempt metadata: %+v %v", a, err)
	}
	// No fabricated result fact on a lease-expired execution.
	if len(a.ResultJSON) != 0 {
		t.Fatal("lease expiry fabricated received result")
	}
	entries, err := db.ListArtifactManifest(ctx, ns, task.ID, auth.Attempt)
	if err != nil || len(entries) != 1 {
		t.Fatalf("partial upload registry lost: %+v %v", entries, err)
	}
}
