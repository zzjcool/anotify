package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
)

func seedRerunCapability(t *testing.T, db *DB, ns, name string, version int, digest string) string {
	t.Helper()
	id := "capv_" + name + "_" + digest + "_" + strconv.Itoa(version)
	if _, err := db.Exec(
		`INSERT INTO capability_versions (id, namespace_id, name, version, spec, digest, fold_key, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, ns, name, version, `{"lifecycle":{"idempotent":false}}`, digest, "fold_"+id, Now()); err != nil {
		t.Fatalf("seed capability %s@%d: %v", name, version, err)
	}
	return id
}

func TestCreateRerun_DefaultsToLiveVersionAndStoresPreparedParams(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	ns, _ := newTestNS(t, db)

	v1 := seedRerunCapability(t, db, ns, "pr-check", 1, "sha256:rerun-v1")
	seedRerunCapability(t, db, ns, "pr-check", 2, "sha256:rerun-v2")

	params := json.RawMessage(`{"repo":"acme/api","number":99,"fail":"yes","nested":{"k":[1,2]}}`)
	orig := &Task{
		NamespaceID:  ns,
		TraceID:      NewEventID(),
		CapabilityID: v1,
		Capability:   "pr-check",
		Version:      1,
		Digest:       "sha256:rerun-v1",
		Params:       append(json.RawMessage(nil), params...),
		Phase:        TaskPending,
	}
	if err := db.CreateTasks(ctx, []*Task{orig}); err != nil {
		t.Fatalf("create original task: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE tasks SET phase = ?, attempt = 1, error = ?, summary = ?, finished_at = ?
		  WHERE id = ?`,
		TaskFailed, "script_failed", "pr-check 执行失败", Now(), orig.ID); err != nil {
		t.Fatalf("finish original task: %v", err)
	}
	before, err := db.GetTask(ctx, ns, orig.ID)
	if err != nil {
		t.Fatalf("read original before rerun: %v", err)
	}

	created, err := db.CreateRerunTask(ctx, RerunInputs{NamespaceID: ns, TaskID: orig.ID, PreparedParams: params})
	if err != nil {
		t.Fatalf("create rerun task: %v", err)
	}
	if created.ID == "" || created.ID == orig.ID {
		t.Fatalf("rerun task id = %q（必须是一条新任务）", created.ID)
	}
	if created.TraceID == orig.TraceID {
		t.Fatalf("rerun trace = %q（必须是新 trace，原任务的事件不可改写）", created.TraceID)
	}
	if created.Version != 2 || created.Digest != "sha256:rerun-v2" {
		t.Fatalf("rerun 钉住 %d/%s, want 2/sha256:rerun-v2（默认 live）", created.Version, created.Digest)
	}
	if created.RerunOf != orig.ID {
		t.Fatalf("rerunOf = %q, want %q", created.RerunOf, orig.ID)
	}
	if created.Phase != TaskPending {
		t.Fatalf("rerun phase = %q, want %q", created.Phase, TaskPending)
	}
	var gotP, wantP any
	if err := json.Unmarshal(created.Params, &gotP); err != nil {
		t.Fatalf("decode rerun params: %v", err)
	}
	if err := json.Unmarshal(params, &wantP); err != nil {
		t.Fatalf("decode want params: %v", err)
	}
	if !reflect.DeepEqual(gotP, wantP) {
		t.Fatalf("rerun params = %s, want %s", created.Params, params)
	}
	reread, err := db.GetTask(ctx, ns, created.ID)
	if err != nil {
		t.Fatalf("read rerun task: %v", err)
	}
	if reread.RerunOf != orig.ID {
		t.Fatalf("读回的 rerunOf = %q, want %q", reread.RerunOf, orig.ID)
	}
	if !reflect.DeepEqual(reread.Params, created.Params) {
		t.Fatalf("读回的 params = %s, want %s", reread.Params, created.Params)
	}

	after, err := db.GetTask(ctx, ns, orig.ID)
	if err != nil {
		t.Fatalf("read original after rerun: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("原任务被改动了:\n before = %+v\n after  = %+v", before, after)
	}
}

func TestCreateRerun_ExplicitVersionAndRejections(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	ns, _ := newTestNS(t, db)
	other := newTestExtraNS(t, db, "rerun-other")

	v1 := seedRerunCapability(t, db, ns, "pr-check", 1, "sha256:pin-v1")
	seedRerunCapability(t, db, ns, "pr-check", 2, "sha256:pin-v2")

	orig := &Task{
		NamespaceID:  ns,
		TraceID:      NewEventID(),
		CapabilityID: v1,
		Capability:   "pr-check",
		Version:      1,
		Digest:       "sha256:pin-v1",
		Params:       json.RawMessage(`{"number":99}`),
		Phase:        TaskPending,
	}
	if err := db.CreateTasks(ctx, []*Task{orig}); err != nil {
		t.Fatalf("create original task: %v", err)
	}

	pinned, err := db.CreateRerunTask(ctx, RerunInputs{NamespaceID: ns, TaskID: orig.ID, Version: 1, PreparedParams: json.RawMessage(`{"number":99}`)})
	if err != nil {
		t.Fatalf("create rerun @1: %v", err)
	}
	if pinned.Version != 1 || pinned.Digest != "sha256:pin-v1" {
		t.Fatalf("显式版本重跑钉住 %d/%s, want 1/sha256:pin-v1", pinned.Version, pinned.Digest)
	}

	if _, err := db.CreateRerunTask(ctx, RerunInputs{NamespaceID: ns, TaskID: orig.ID, Version: 9, PreparedParams: json.RawMessage(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("version=9 err = %v, want ErrNotFound", err)
	}
	if _, err := db.Exec(`UPDATE capability_versions SET deleted_at = ? WHERE id = ?`, Now(), v1); err != nil {
		t.Fatalf("soft-delete v1: %v", err)
	}
	if _, err := db.CreateRerunTask(ctx, RerunInputs{NamespaceID: ns, TaskID: orig.ID, Version: 1, PreparedParams: json.RawMessage(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已删除版本 err = %v, want ErrNotFound", err)
	}
	if _, err := db.CreateRerunTask(ctx, RerunInputs{NamespaceID: other, TaskID: orig.ID, PreparedParams: json.RawMessage(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨 ns err = %v, want ErrNotFound", err)
	}
	if _, err := db.CreateRerunTask(ctx, RerunInputs{NamespaceID: ns, TaskID: "tsk_missing", PreparedParams: json.RawMessage(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的任务 err = %v, want ErrNotFound", err)
	}
}

func TestCreateRerun_UnresolvedFailureUsesRequestedName(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	ns, _ := newTestNS(t, db)
	seedRerunCapability(t, db, ns, "late-capability", 1, "sha256:late-v1")

	now := Now()
	id := NewTaskID()
	if _, err := db.Exec(
		`INSERT INTO tasks (id, namespace_id, trace_id, capability_id, capability, version, digest,
		                    params, phase, attempt, error, finished_at, created_at)
		 VALUES (?,?,?,NULL,?,?,?,?,?,0,?,?,?)`,
		id, ns, NewEventID(), "late-capability", 0, "", `{"a":1}`, TaskFailed, "downstream_missing", now, now); err != nil {
		t.Fatalf("seed unresolved task: %v", err)
	}

	created, err := db.CreateRerunTask(ctx, RerunInputs{NamespaceID: ns, TaskID: id, PreparedParams: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatalf("rerun unresolved task: %v", err)
	}
	if created.CapabilityID == "" || created.Version != 1 {
		t.Fatalf("rerun 未解析任务 = %q@%d, want 真实 capability@1", created.CapabilityID, created.Version)
	}
	if created.RerunOf != id {
		t.Fatalf("rerunOf = %q, want %q", created.RerunOf, id)
	}
}

func TestCreateRerun_RequiresPreparedParams(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	ns, _ := newTestNS(t, db)
	capID := seedRerunCapability(t, db, ns, "pr-check", 1, "sha256:prep-v1")

	orig := &Task{
		NamespaceID:  ns,
		TraceID:      NewEventID(),
		CapabilityID: capID,
		Capability:   "pr-check",
		Version:      1,
		Digest:       "sha256:prep-v1",
		Params:       json.RawMessage(`{"number":99}`),
		Phase:        TaskPending,
	}
	if err := db.CreateTasks(ctx, []*Task{orig}); err != nil {
		t.Fatalf("create original task: %v", err)
	}
	if _, err := db.CreateRerunTask(ctx, RerunInputs{NamespaceID: ns, TaskID: orig.ID}); err == nil {
		t.Fatal("PreparedParams 为空却建出了任务：归一必须由调用方完成后传入")
	}
}
