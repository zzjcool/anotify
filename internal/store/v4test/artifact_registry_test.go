package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func uploadFixture(t *testing.T) (*store.DB, string, *store.CapabilityVersion, *store.Node, *store.Task, store.EmitAuth) {
	t.Helper()
	db := openTestDB(t)
	ns := newNS(t, db, "uploads")
	cap := lifeCycleCap(t, db, ns, "run", true, 4)
	node := newNode(t, db, "node", 2)
	task := newTask(t, db, ns, cap, "upload", "")
	lease, token, attempt := grant(t, db, task.ID, node.ID, "asg")
	return db, ns, cap, node, task, store.EmitAuth{TaskID: task.ID, NodeID: node.ID, NodeTokenHash: node.TokenHash, Attempt: attempt, LeaseID: lease, AttemptToken: token, Now: store.Now()}
}
func artifactInput(auth store.EmitAuth, path string, size int64) protocol.ArtifactUploadResponse {
	return protocol.ArtifactUploadResponse{TaskID: auth.TaskID, Attempt: auth.Attempt, Path: path, SHA256: strings.Repeat("a", 64), Size: size}
}
func registerInput(t *testing.T, db *store.DB, auth store.EmitAuth, in protocol.ArtifactUploadResponse) *store.ArtifactUpload {
	t.Helper()
	u, err := db.BeginArtifactUpload(context.Background(), auth, in)
	if err != nil {
		t.Fatal(err)
	}
	got, status, err := db.RegisterArtifactUpload(context.Background(), auth, u, in.SHA256, in.Size)
	if err != nil || status != 201 || got != in {
		t.Fatalf("register: %d %+v %v", status, got, err)
	}
	return u
}
func registryFacts(t *testing.T, db *store.DB) [][][]any {
	t.Helper()
	return [][][]any{
		wireRows(t, db, `SELECT * FROM tasks ORDER BY id`), wireRows(t, db, `SELECT * FROM task_attempts ORDER BY task_id,attempt`), wireRows(t, db, `SELECT * FROM node_instances ORDER BY id`), wireRows(t, db, `SELECT * FROM task_artifact_manifests ORDER BY task_id,attempt,path`), wireRows(t, db, `SELECT * FROM task_artifact_uploads ORDER BY id`), wireRows(t, db, `SELECT * FROM task_artifact_objects ORDER BY id`),
	}
}

func TestC3bRegisterReauthorizesAfterPrecheckZeroFacts(t *testing.T) {
	for _, fault := range []string{"rotate", "cancel", "expiry", "namespace", "token", "attempt", "node"} {
		t.Run(fault, func(t *testing.T) {
			db, _, _, node, task, auth := uploadFixture(t)
			ctx := context.Background()
			in := artifactInput(auth, "out/a", 10)
			u, err := db.BeginArtifactUpload(ctx, auth, in)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "rotate":
				if err := db.SetNodeTokenHash(ctx, node.ID, store.HashNodeToken("rotated")); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				if err := db.RequestCancel(ctx, task.ID, protocol.ReasonCancelledByUser, auth.Now); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				// Lease expires while the bounded upload ticket is still live;
				// rejection must come from final CURRENT execution authorization.
				if _, err := db.Exec(`UPDATE tasks SET lease_until=? WHERE id=?`, auth.Now+1, task.ID); err != nil {
					t.Fatal(err)
				}
				auth.Now += 2
			case "namespace":
				if _, err := db.Exec(`UPDATE node_configs SET namespaces='["other"]' WHERE node_id=?`, node.ID); err != nil {
					t.Fatal(err)
				}
			case "token":
				auth.AttemptToken = "wrong"
			case "attempt":
				auth.Attempt++
			case "node":
				auth.NodeID = "missing"
			}
			before := registryFacts(t, db)
			if _, _, err := db.RegisterArtifactUpload(ctx, auth, u, in.SHA256, in.Size); err == nil {
				t.Fatal("stale final registration accepted")
			}
			if after := registryFacts(t, db); !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected registration wrote facts: before=%v after=%v", before, after)
			}
		})
	}
}

func TestC3bUploadValidationConflictIdempotence(t *testing.T) {
	db, _, _, _, _, auth := uploadFixture(t)
	ctx := context.Background()
	for _, p := range []string{"", "/abs", "../bad", "a/../b", "a\\b", "a\x00b", "a//b", "a/./b", "a\r\nb"} {
		in := artifactInput(auth, p, 0)
		before := registryFacts(t, db)
		if _, err := db.BeginArtifactUpload(ctx, auth, in); !errors.Is(err, store.ErrArtifactPathInvalid) {
			t.Fatalf("path %q: %v", p, err)
		}
		if !reflect.DeepEqual(before, registryFacts(t, db)) {
			t.Fatal("invalid path wrote facts")
		}
	}
	for _, hash := range []string{"a", strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 64)} {
		in := artifactInput(auth, "a", 0)
		in.SHA256 = hash
		if _, err := db.BeginArtifactUpload(ctx, auth, in); !errors.Is(err, store.ErrArtifactHashMismatch) {
			t.Fatalf("hash %q: %v", hash, err)
		}
	}
	for _, size := range []int64{-1, protocol.MaxArtifactTransferBytes + 1} {
		if _, err := db.BeginArtifactUpload(ctx, auth, artifactInput(auth, "a", size)); err == nil {
			t.Fatalf("invalid size %d accepted", size)
		}
	}
	in := artifactInput(auth, "a", protocol.MaxArtifactTransferBytes)
	u := registerInput(t, db, auth, in)
	if u.ObjectID().String() == "" || strings.Contains(u.ObjectID().String(), "/") || strings.Contains(u.ObjectID().String(), in.SHA256) {
		t.Fatal("system identity derived from caller")
	}
	before := registryFacts(t, db)
	got, status, err := db.RegisterArtifactUpload(ctx, auth, u, in.SHA256, in.Size)
	if err != nil || status != 200 || got != in {
		t.Fatalf("repeat: %d %v", status, err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("exact repeat charged or mutated facts")
	}
	for _, fault := range []string{"hash", "size"} {
		bad := in
		if fault == "hash" {
			bad.SHA256 = strings.Repeat("b", 64)
		} else {
			bad.Size--
		}
		if _, err := db.BeginArtifactUpload(ctx, auth, bad); !errors.Is(err, store.ErrArtifactConflict) {
			t.Fatalf("same path %s conflict: %v", fault, err)
		}
	}
	fresh := artifactInput(auth, "actual", 10)
	v, err := db.BeginArtifactUpload(ctx, auth, fresh)
	if err != nil {
		t.Fatal(err)
	}
	for _, actual := range []struct {
		hash string
		size int64
	}{{strings.Repeat("b", 64), 10}, {fresh.SHA256, 9}, {fresh.SHA256, protocol.MaxArtifactTransferBytes + 1}} {
		before := registryFacts(t, db)
		if _, _, err := db.RegisterArtifactUpload(ctx, auth, v, actual.hash, actual.size); err == nil {
			t.Fatal("unverified actual accepted")
		}
		if !reflect.DeepEqual(before, registryFacts(t, db)) {
			t.Fatal("bad stream wrote")
		}
	}
}

func TestC3bAllArtifactLimitsAndConcurrentQuota(t *testing.T) {
	t.Run("items", func(t *testing.T) {
		db, _, _, _, _, auth := uploadFixture(t)
		for i := 0; i < 32; i++ {
			registerInput(t, db, auth, artifactInput(auth, fmt.Sprintf("item-%d", i), 0))
		}
		in := artifactInput(auth, "overflow", 0)
		u, err := db.BeginArtifactUpload(context.Background(), auth, in)
		if err != nil {
			t.Fatal(err)
		}
		before := registryFacts(t, db)
		if _, _, err := db.RegisterArtifactUpload(context.Background(), auth, u, in.SHA256, in.Size); !errors.Is(err, store.ErrArtifactTooLarge) {
			t.Fatalf("33 items: %v", err)
		}
		if !reflect.DeepEqual(before, registryFacts(t, db)) {
			t.Fatal("item rejection wrote")
		}
	})
	t.Run("bytes-and-idempotence", func(t *testing.T) {
		db, _, _, _, _, auth := uploadFixture(t)
		var u *store.ArtifactUpload
		var in protocol.ArtifactUploadResponse
		for i := 0; i < 4; i++ {
			in = artifactInput(auth, string(rune('a'+i)), 16<<20)
			u = registerInput(t, db, auth, in)
		}
		if _, status, err := db.RegisterArtifactUpload(context.Background(), auth, u, in.SHA256, in.Size); err != nil || status != 200 {
			t.Fatalf("repeat at exact quota: %d %v", status, err)
		}
		extra := artifactInput(auth, "overflow", 1)
		v, err := db.BeginArtifactUpload(context.Background(), auth, extra)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.RegisterArtifactUpload(context.Background(), auth, v, extra.SHA256, extra.Size); !errors.Is(err, store.ErrArtifactTooLarge) {
			t.Fatalf("64MiB+1: %v", err)
		}
	})
	t.Run("concurrent-byte-charge", func(t *testing.T) {
		db, _, _, _, _, auth := uploadFixture(t)
		ctx := context.Background()
		for i := 0; i < 3; i++ {
			registerInput(t, db, auth, artifactInput(auth, string(rune('a'+i)), 16<<20))
		}
		inputs := []protocol.ArtifactUploadResponse{artifactInput(auth, "d", 16<<20), artifactInput(auth, "e", 16<<20)}
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
			t.Fatalf("accepted=%d errs=%v", accepted, errs)
		}
		var count int
		var total int64
		if err := db.QueryRow(`SELECT COUNT(*),SUM(size) FROM task_artifact_manifests WHERE task_id=? AND attempt=?`, auth.TaskID, auth.Attempt).Scan(&count, &total); err != nil {
			t.Fatal(err)
		}
		if count != 4 || total != 64<<20 {
			t.Fatalf("real quota=%d/%d", count, total)
		}
	})
}

func TestC3bResultReferencesCurrentTaskNamespaceAttempt(t *testing.T) {
	db, ns, cap, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	in := artifactInput(auth, "a", 1)
	nodeAuth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
	result := store.TaskResult{Phase: store.TaskSucceeded, Artifacts: []protocol.TaskArtifact{{Path: in.Path, SHA256: in.SHA256}}}
	before := registryFacts(t, db)
	if err := db.CompleteTaskAuthorized(ctx, nodeAuth, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); !errors.Is(err, store.ErrArtifactReference) {
		t.Fatalf("unuploaded: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("unuploaded result wrote")
	}
	registerInput(t, db, auth, in)
	other := newTask(t, db, ns, cap, "other", "")
	if err := db.AssignTaskAt(ctx, other.ID, node.ID, "other", 1, auth.Now); err != nil {
		t.Fatal(err)
	}
	otherLease, _, otherAttempt, _, err := db.GrantLeaseAuthorized(ctx, nodeAuth, other.ID, "other", 1, 1, auth.Now)
	// Default maxPerNode=1: an unrelated task can also be created on a separate node.
	if errors.Is(err, store.ErrSlotUnavailable) {
		otherNode := newNode(t, db, "node-other", 1)
		nodeAuth = store.NodeAuthorization{NodeID: otherNode.ID, NodeTokenHash: otherNode.TokenHash}
		if err := db.AssignTaskAt(ctx, other.ID, otherNode.ID, "other2", 1, auth.Now); err != nil {
			t.Fatal(err)
		}
		otherLease, _, otherAttempt, _, err = db.GrantLeaseAuthorized(ctx, nodeAuth, other.ID, "other2", 1, 0, auth.Now)
	}
	if err != nil {
		t.Fatal(err)
	}
	before = registryFacts(t, db)
	if err := db.CompleteTaskAuthorized(ctx, nodeAuth, other.ID, otherAttempt, otherLease, auth.Now, result); !errors.Is(err, store.ErrArtifactReference) {
		t.Fatalf("cross task hash borrow: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("cross task wrote")
	}
	otherNS := newNS(t, db, "other-ns")
	if _, err := db.GetTaskArtifactDownload(ctx, otherNS, task.ID, in.SHA256, auth.Now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross NS download: %v", err)
	}
	// Make an actual new attempt; the earlier upload must not become its manifest.
	if _, _, err := db.RecoverLostLeases(ctx, auth.Now+60); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReapTaskAttempt(ctx, store.ReapedInputs{TaskID: task.ID, Attempt: auth.Attempt, NodeID: node.ID, NodeTokenHash: node.TokenHash, LeaseID: auth.LeaseID, Slot: 0, Now: auth.Now + 60}); err != nil {
		t.Fatal(err)
	}
	if err := db.AssignTaskAt(ctx, task.ID, node.ID, "again", 2, auth.Now+65); err != nil {
		t.Fatal(err)
	}
	nextLease, _, nextAttempt, _, err := db.GrantLeaseAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, "again", 2, 0, auth.Now+65)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, nextAttempt, nextLease, auth.Now+66, result); !errors.Is(err, store.ErrArtifactReference) {
		t.Fatalf("prior attempt ref borrowed: %v", err)
	}
}

func TestC3bPartialFailureAndTypedAttemptReopen(t *testing.T) {
	db, ns, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	in := artifactInput(auth, "a", 8)
	registerInput(t, db, auth, in)
	result := store.TaskResult{Phase: store.TaskFailed, Reason: protocol.ReasonArtifactUploadFailed, Error: "network error", Summary: "one uploaded, next failed", Artifacts: []protocol.TaskArtifact{{Path: in.Path, SHA256: in.SHA256}}}
	empty := result
	empty.Artifacts = nil
	before := registryFacts(t, db)
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, empty); !errors.Is(err, store.ErrArtifactReference) {
		t.Fatalf("fakeempty partial error: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("fakeempty wrote")
	}
	disposition, err := db.CompleteTaskClassifiedAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result)
	if err != nil || disposition != store.TaskResultRecorded {
		t.Fatalf("record: %s %v", disposition, err)
	}
	got, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.GetTaskAttempt(ctx, task.ID, auth.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Artifacts, result.Artifacts) || !reflect.DeepEqual(a.Artifacts, result.Artifacts) || got.Error != "network error" || len(a.ResultJSON) == 0 {
		t.Fatalf("partial manifest/result not retained: task=%+v attempt=%+v", got, a)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "attemptToken") || strings.Contains(string(raw), "object_id") {
		t.Fatal("secret leaked")
	}
	before = registryFacts(t, db)
	disposition, err = db.CompleteTaskClassifiedAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now+100, result)
	if err != nil || disposition != store.TaskResultDuplicate {
		t.Fatalf("duplicate: %s %v", disposition, err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("duplicate DML")
	}
	var seq int
	var dbname, dbpath string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbname, &dbpath); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dbpath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	round, err := reopened.GetTaskAttempt(ctx, task.ID, auth.Attempt)
	if err != nil || !reflect.DeepEqual(a, round) {
		t.Fatalf("actual typed attempt reopen: %+v %v", round, err)
	}

	wrong := result
	wrong.Error = "different"
	if _, err := db.CompleteTaskClassifiedAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, wrong); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("different historical result ACKed: %v", err)
	}
	if err := db.SetNodeTokenHash(ctx, node.ID, store.HashNodeToken("new")); err != nil {
		t.Fatal(err)
	}
	before = registryFacts(t, db)
	if _, err := db.CompleteTaskClassifiedAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); !errors.Is(err, store.ErrNodeIdentityMismatch) {
		t.Fatalf("stale replay credential: %v", err)
	}
	if !reflect.DeepEqual(before, registryFacts(t, db)) {
		t.Fatal("stale replay DML")
	}
}

func TestC3bByteExpiryMetadataAndActiveUploadGuard(t *testing.T) {
	db, ns, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	in := artifactInput(auth, "a", 1)
	u := registerInput(t, db, auth, in)
	ids, err := db.ExpireArtifactObjects(ctx, auth.Now+7*86400, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id.String() == u.ObjectID().String() {
			t.Fatal("active/unreaped task bytes expired")
		}
	}
	result := store.TaskResult{Phase: store.TaskSucceeded, Artifacts: []protocol.TaskArtifact{{Path: in.Path, SHA256: in.SHA256}}}
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, result); err != nil {
		t.Fatal(err)
	}
	ids, err = db.ExpireArtifactObjects(ctx, auth.Now+7*86400, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id.String() == u.ObjectID().String() {
			found = true
			if err := db.AckArtifactObjectPurged(ctx, id, auth.Now+7*86400); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("7 day bytes did not expire")
	}
	if _, err := db.GetTaskArtifactDownload(ctx, ns, task.ID, in.SHA256, auth.Now+7*86400); !errors.Is(err, store.ErrArtifactExpired) {
		t.Fatalf("expired download: %v", err)
	}
	entries, err := db.ListArtifactManifest(ctx, ns, task.ID, auth.Attempt)
	if err != nil || len(entries) != 1 || entries[0].ExpiredAt == nil {
		t.Fatalf("expired metadata: %+v %v", entries, err)
	}
	if stats, err := db.RunRetention(ctx, auth.Now+7*86400); err != nil || stats.TasksDeleted != 0 {
		t.Fatalf("task facts before 30d: %+v %v", stats, err)
	}
	if stats, err := db.RunRetention(ctx, auth.Now+30*86400+1); err != nil || stats.TasksDeleted != 1 {
		t.Fatalf("30d facts: %+v %v", stats, err)
	}
	entries, err = db.ListArtifactManifest(ctx, ns, task.ID, auth.Attempt)
	if err != nil || len(entries) != 0 {
		t.Fatalf("task expiry manifest cascade: %+v %v", entries, err)
	}
}

func TestC3bTaskTTLRetainsActiveTicket(t *testing.T) {
	db, _, _, node, task, auth := uploadFixture(t)
	ctx := context.Background()
	in := artifactInput(auth, "active", 1)
	u, err := db.BeginArtifactUpload(ctx, auth, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, auth.Attempt, auth.LeaseID, auth.Now, store.TaskResult{Phase: store.TaskSucceeded}); err != nil {
		t.Fatal(err)
	}
	if n, err := db.DeleteExpiredTasks(ctx, auth.Now+31*86400, 20); err != nil || n != 0 {
		t.Fatalf("active upload metadata deleted: %d %v", n, err)
	}
	ids, err := db.ExpireArtifactObjects(ctx, auth.Now+1, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id.String() == u.ObjectID().String() {
			t.Fatal("active spool expired")
		}
	}
	if err := db.AbortArtifactUpload(ctx, u); err != nil {
		t.Fatal(err)
	}
	if n, err := db.DeleteExpiredTasks(ctx, auth.Now+31*86400, 20); err != nil || n != 1 {
		t.Fatalf("inactive terminal fact not deleted: %d %v", n, err)
	}
}
