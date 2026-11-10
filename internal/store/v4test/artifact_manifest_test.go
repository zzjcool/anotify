package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func TestC3bTypedArtifactJSONRoundTrip(t *testing.T) {
	db, path := openTestDBWithPath(t)
	ctx := context.Background()
	ns := newNS(t, db, "typed-output")
	c := createCap(t, db, testCap(ns, "run", 1, nil))
	task := newTask(t, db, ns, c, "typed", "")
	want := []protocol.TaskArtifact{{Path: "out/report.json", SHA256: strings.Repeat("a", 64)}}
	raw, _ := json.Marshal(want)
	if _, err := db.Exec(`UPDATE tasks SET artifacts=? WHERE id=?`, string(raw), task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("typed manifest must decode: %v", err)
	}
	encoded, _ := json.Marshal(got.Artifacts)
	if string(encoded) != string(raw) {
		t.Fatalf("typed roundtrip: %s != %s", encoded, raw)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err = reopened.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(got.Artifacts)
	if string(encoded) != string(raw) {
		t.Fatalf("reopen: %s", encoded)
	}
}

func TestC3bSameFactResultReplayNoWrites(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "result-replay")
	c := createCap(t, db, testCap(ns, "run", 1, nil))
	node := newNode(t, db, "node", 1)
	task := newTask(t, db, ns, c, "replay", "")
	lease, _, attempt := grant(t, db, task.ID, node.ID, "asg")
	auth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
	result := store.TaskResult{Phase: store.TaskSucceeded, Summary: "done"}
	if err := db.CompleteTaskAuthorized(ctx, auth, task.ID, attempt, lease, store.Now(), result); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteTaskAuthorized(ctx, auth, task.ID, attempt, lease, store.Now()+100, result); err != nil {
		t.Fatalf("exact authenticated replay must ACK: %v", err)
	}
	after, err := db.GetTaskAttempt(ctx, task.ID, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("duplicate changed execution facts")
	}
}

func TestC3bNamespaceForceUnreapedZeroWrites(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "force-unreaped")
	newNS(t, db, "keep")
	c := createCap(t, db, testCap(ns, "run", 1, nil))
	node := newNode(t, db, "node", 1)
	task := newTask(t, db, ns, c, "force", "")
	grant(t, db, task.ID, node.ID, "asg")
	before, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = db.DeleteNamespace(ctx, ns, true, 1)
	var blocked *store.NamespaceBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("force must conflict before physical reap: %v", err)
	}
	after, err := db.GetTask(ctx, ns, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("blocked force changed task")
	}
}

func registerTestArtifact(t *testing.T, db *store.DB, auth store.EmitAuth, path string, size int64) []protocol.TaskArtifact {
	t.Helper()
	ctx := context.Background()
	in := protocol.ArtifactUploadResponse{TaskID: auth.TaskID, Attempt: auth.Attempt, Path: path, SHA256: strings.Repeat("a", 64), Size: size}
	u, err := db.BeginArtifactUpload(ctx, auth, in)
	if err != nil {
		t.Fatal(err)
	}
	got, status, err := db.RegisterArtifactUpload(ctx, auth, u, in.SHA256, in.Size)
	if err != nil || status != 201 || got != in {
		t.Fatalf("register artifact: %v %d %+v", err, status, got)
	}
	return []protocol.TaskArtifact{{Path: in.Path, SHA256: in.SHA256}}
}
