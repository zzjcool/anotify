package v4test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func TestC3bGCIndependentlyProtectsTaskAttemptManifest(t *testing.T) {
	for _, ref := range []string{"task", "attempt", "manifest"} {
		t.Run(ref, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			ns := newNS(t, db, "isolate")
			cap := createCap(t, db, testCap(ns, "run", 1, nil))
			node := newNode(t, db, "node", 1)
			task := newTask(t, db, ns, cap, "refs", "")
			if ref != "task" {
				lease, token, attempt := grant(t, db, task.ID, node.ID, "asg")
				auth := store.EmitAuth{TaskID: task.ID, NodeID: node.ID, NodeTokenHash: node.TokenHash, Attempt: attempt, LeaseID: lease, AttemptToken: token}
				var refs []protocol.TaskArtifact
				if ref == "manifest" {
					refs = registerTestArtifact(t, db, auth, "out", 1)
				}
				if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, lease, store.Now(), store.TaskResult{Phase: store.TaskSucceeded, Artifacts: refs}); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.DeleteCapability(ctx, ns, cap.Name, store.ResourceVersion(cap.ID, cap.ResourceVersion)); err != nil {
				t.Fatal(err)
			}
			replacement := testCap(ns, cap.Name, cap.Version, nil)
			replacement.Spec = cap.Spec
			fresh := createCap(t, db, replacement)
			// Actual grant/registration populated each reference. Independently isolate
			// the defensive guard with a direct corruption fixture of OTHER references;
			// normal immutable task pins are never updated by production.
			if ref != "task" {
				if _, err := db.Exec(`UPDATE tasks SET capability_id=? WHERE id=?`, fresh.ID, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if ref == "manifest" {
				if _, err := db.Exec(`UPDATE task_attempts SET capability_id=? WHERE task_id=?`, fresh.ID, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := namespaceWholeFacts(t, db)
			if n, err := db.PurgeDeletedCapabilities(ctx, 20); err != nil || n != 0 {
				t.Fatalf("%s alone did not protect: %d %v", ref, n, err)
			}
			if !reflect.DeepEqual(before, namespaceWholeFacts(t, db)) {
				t.Fatal("referenced GC mutated facts")
			}
			if ref == "task" {
				if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, store.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := db.DeleteExpiredTasks(ctx, store.Now()+1, 20); err != nil || n != 1 {
				t.Fatalf("fact expiry: %d %v", n, err)
			}
			if n, err := db.PurgeDeletedCapabilities(ctx, 20); err != nil || n != 1 {
				t.Fatalf("unreferenced immutable old UID: %d %v", n, err)
			}
			if got, err := db.GetCapabilityByID(ctx, ns, fresh.ID); err != nil || got.Digest != cap.Digest {
				t.Fatalf("same digest new owner harmed: %+v %v", got, err)
			}
		})
	}
}

func TestC3bResultCrossNamespaceReferenceDenial(t *testing.T) {
	db, _, _, _, _, auth := uploadFixture(t)
	ctx := context.Background()
	registerInput(t, db, auth, artifactInput(auth, "out", 1))
	ns := newNS(t, db, "other-ns")
	cap := createCap(t, db, testCap(ns, "run", 1, nil))
	node := newNode(t, db, "other-node", 1)
	task := newTask(t, db, ns, cap, "other", "")
	lease, _, attempt := grant(t, db, task.ID, node.ID, "other")
	before := registryFacts(t, db)
	in := artifactInput(auth, "out", 1)
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt, lease, store.Now(), store.TaskResult{Phase: store.TaskSucceeded, Artifacts: []protocol.TaskArtifact{{Path: in.Path, SHA256: in.SHA256}}}); !errors.Is(err, store.ErrArtifactReference) {
		t.Fatalf("other NS hash borrow: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("cross NS result wrote")
	}
}

func TestC3bConcurrentItemQuota(t *testing.T) {
	db, _, _, _, _, auth := uploadFixture(t)
	ctx := context.Background()
	for i := 0; i < 31; i++ {
		registerInput(t, db, auth, artifactInput(auth, fmt.Sprintf("item-%d", i), 0))
	}
	inputs := []protocol.ArtifactUploadResponse{artifactInput(auth, "next-left", 0), artifactInput(auth, "next-right", 0)}
	tickets := make([]*store.ArtifactUpload, 2)
	for i := range inputs {
		var err error
		tickets[i], err = db.BeginArtifactUpload(ctx, auth, inputs[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range inputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i] = db.RegisterArtifactUpload(ctx, auth, tickets[i], inputs[i].SHA256, inputs[i].Size)
		}(i)
	}
	close(start)
	wg.Wait()
	accepted := 0
	for _, err := range errs {
		if err == nil {
			accepted++
		} else if !errors.Is(err, store.ErrArtifactTooLarge) {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("item quota race: %d %v", accepted, errs)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_artifact_manifests WHERE task_id=?`, auth.TaskID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 32 {
		t.Fatalf("item quota=%d", count)
	}
}

func TestC3bDuplicateRequiresRetainedManifestFacts(t *testing.T) {
	db, _, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	refs := registerTestArtifact(t, db, auth, "out", 1)
	result := store.TaskResult{Phase: store.TaskSucceeded, Artifacts: refs}
	nodeAuth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
	if err := db.CompleteTaskAuthorized(ctx, nodeAuth, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); err != nil {
		t.Fatal(err)
	}
	// Missing manifest facts (simulated GC/corruption) cannot be guessed from phase.
	if _, err := db.Exec(`DELETE FROM task_artifact_manifests WHERE task_id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	before := registryFacts(t, db)
	if _, err := db.CompleteTaskClassifiedAuthorized(ctx, nodeAuth, task.ID, auth.Attempt, auth.LeaseID, auth.Now+60, result); !errors.Is(err, store.ErrArtifactReference) {
		t.Fatalf("missing refs inferred success: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("missing facts replay wrote")
	}
}

func TestC3bRequiredRegistrySchemaRejectsMissingTable(t *testing.T) {
	db, path := openTestDBWithPath(t)
	if _, err := db.Exec(`DROP TABLE task_artifact_manifests`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if reopened != nil {
		reopened.Close()
	}
	if !errors.Is(err, store.ErrSchemaInvalid) {
		t.Fatalf("missing actual registry layout accepted/repaired: %v", err)
	}
}

// The task FK closes the opposite GC race: after the immutable capability ID
// was purged, a delayed producer cannot create a new dangling pin to that ID.
func TestC3bPurgedCapabilityCannotGainLateTaskReference(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "late-ref")
	cap := createCap(t, db, testCap(ns, "run", 1, nil))
	if err := db.DeleteCapability(ctx, ns, cap.Name, store.ResourceVersion(cap.ID, cap.ResourceVersion)); err != nil {
		t.Fatal(err)
	}
	if n, err := db.PurgeDeletedCapabilities(ctx, 10); err != nil || n != 1 {
		t.Fatalf("unreferenced GC: %d %v", n, err)
	}
	before := namespaceWholeFacts(t, db)
	task := &store.Task{NamespaceID: ns, TraceID: "late", CapabilityID: cap.ID, Capability: cap.Name, Version: cap.Version, Digest: cap.Digest}
	if err := db.CreateTasks(ctx, []*store.Task{task}); err == nil {
		t.Fatal("late producer created dangling capability pin")
	}
	if !reflect.DeepEqual(before, namespaceWholeFacts(t, db)) {
		t.Fatal("rejected late reference wrote")
	}
}
