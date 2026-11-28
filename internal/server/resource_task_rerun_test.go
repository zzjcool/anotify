package server

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func (e *taskResEnv) seedRerunTask(ns, capID, name string, version int, digest, params, phase string) string {
	e.t.Helper()
	if ns == "" {
		ns = e.ns
	}
	id := store.NewTaskID()
	now := store.Now()
	if _, err := e.db.Exec(
		`INSERT INTO tasks (id, namespace_id, trace_id, capability_id, capability, version, digest,
		                    params, phase, attempt, error, finished_at, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,1,?,?,?)`,
		id, ns, "trace_"+id, capID, name, version, digest, params, phase, "script_failed", now, now); err != nil {
		e.t.Fatalf("seed task: %v", err)
	}
	return id
}

func TestTaskRerunCreatesNewTaskAndKeepsOriginal(t *testing.T) {
	env := newTaskResEnv(t)
	params := `{"repo":"acme/api","number":99,"fail":"yes","nested":{"k":[1,2]}}`
	orig := env.seedRerunTask(env.ns, env.cap, "review", 1, "sha256:taskres", params, store.TaskFailed)

	rr := env.do("POST", env.taskPath(env.ns, orig, "/rerun"), "{}")
	doc := trWant(t, rr, 200)
	if got := trString(t, doc, "kind"); got != "Task" {
		t.Fatalf("kind = %q, want Task", got)
	}
	created := trString(t, doc, "metadata", "name")
	if created == "" || created == orig {
		t.Fatalf("新任务 name = %q（必须是一条新任务）", created)
	}
	if got := trString(t, doc, "spec", "rerunOf"); got != orig {
		t.Fatalf("spec.rerunOf = %q, want %q", got, orig)
	}
	wantParams := json.RawMessage(params)
	var gotP, wantP any
	raw, err := json.Marshal(trValue(t, doc, "spec", "params"))
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if err := json.Unmarshal(raw, &gotP); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if err := json.Unmarshal(wantParams, &wantP); err != nil {
		t.Fatalf("decode want params: %v", err)
	}
	if !reflect.DeepEqual(gotP, wantP) {
		t.Fatalf("重跑 params = %s, want %s", raw, params)
	}
	if v, ok := trObject(t, doc, "spec")["parentTaskId"]; !ok || v != nil {
		t.Fatalf("重跑任务的 parentTaskId = %v, want null", v)
	}
	if got := trString(t, doc, "spec", "traceUID"); got == "" || got == "trace_"+orig {
		t.Fatalf("traceUID = %q（必须是一条新 trace）", got)
	}

	origDoc := trWant(t, env.do("GET", env.taskPath(env.ns, orig, ""), nil), 200)
	if got := trString(t, origDoc, "status", "phase"); got != store.TaskFailed {
		t.Fatalf("原任务 phase = %q, want %q", got, store.TaskFailed)
	}
	if v, ok := trObject(t, origDoc, "spec")["rerunOf"]; !ok || v != nil {
		t.Fatalf("原任务 spec.rerunOf = %v, want null（重跑不改原任务）", v)
	}
	rawOrig, err := json.Marshal(trValue(t, origDoc, "spec", "params"))
	if err != nil {
		t.Fatalf("marshal orig params: %v", err)
	}
	var gotOrig any
	if err := json.Unmarshal(rawOrig, &gotOrig); err != nil {
		t.Fatalf("decode orig params: %v", err)
	}
	if !reflect.DeepEqual(gotOrig, wantP) {
		t.Fatalf("原任务 params 变了 = %s, want %s", rawOrig, params)
	}
}

func TestTaskRerunVersionSelection(t *testing.T) {
	env := newTaskResEnv(t)
	capV2 := env.seedCapability(env.ns, "review", 2, "sha256:taskres-v2")
	orig := env.seedRerunTask(env.ns, env.cap, "review", 1, "sha256:taskres", `{"n":1}`, store.TaskFailed)

	doc := trWant(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), ""), 200)
	if got := trNumber(t, doc, "spec", "capability", "version"); got != 2 {
		t.Fatalf("默认重跑版本 = %v, want 2（live）", got)
	}
	if got := trString(t, doc, "spec", "capability", "digest"); got != "sha256:taskres-v2" {
		t.Fatalf("默认重跑 digest = %q, want sha256:taskres-v2", got)
	}

	doc = trWant(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"version":1}`), 200)
	if got := trNumber(t, doc, "spec", "capability", "version"); got != 1 {
		t.Fatalf("显式重跑版本 = %v, want 1", got)
	}
	if got := trString(t, doc, "spec", "capability", "digest"); got != "sha256:taskres" {
		t.Fatalf("显式重跑 digest = %q, want sha256:taskres", got)
	}
	_ = capV2

	trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"version":7}`), 404, reasonNotFound)
}

func TestTaskRerunRejectsBadInput(t *testing.T) {
	env := newTaskResEnv(t)
	orig := env.seedRerunTask(env.ns, env.cap, "review", 1, "sha256:taskres", `{}`, store.TaskFailed)

	trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"verison":2}`), 400, "unknown_field")
	trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"params":{"repo":"x"}}`), 400, "unknown_field")
	trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"version":-1}`), 400, reasonBadRequest)
	trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{`), 400, reasonUnknownField)
	trReason(t, env.do("POST", env.taskPath(env.ns2, orig, "/rerun"), "{}"), 404, reasonNotFound)
	trReason(t, env.do("POST", env.taskPath(env.ns, "tsk_missing", "/rerun"), "{}"), 404, reasonNotFound)
}

func TestTaskRerunAlignsParamsToTargetVersionContract(t *testing.T) {
	env := newTaskResEnv(t)
	env.seedCapabilitySpec(env.ns, "review", 2, "sha256:taskres-v2",
		`{"params":{"type":"object","required":["repo"],"properties":{"repo":{"type":"string"},"channel":{"type":"string","default":"slack"}}}}`)
	orig := env.seedRerunTask(env.ns, env.cap, "review", 1, "sha256:taskres", `{"repo":"acme/api","number":99}`, store.TaskFailed)

	doc := trWant(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), "{}"), 200)
	if got := trNumber(t, doc, "spec", "capability", "version"); got != 2 {
		t.Fatalf("重跑版本 = %v, want 2（live）", got)
	}
	params, ok := trValue(t, doc, "spec", "params").(map[string]any)
	if !ok {
		t.Fatalf("spec.params 不是对象：%s", mustJSON(t, doc))
	}
	want := map[string]any{"repo": "acme/api", "channel": "slack"}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("重跑 params = %v, want %v（按 v2 契约归一：补 default、丢契约外字段）", params, want)
	}

	origDoc := trWant(t, env.do("GET", env.taskPath(env.ns, orig, ""), nil), 200)
	if got := trValue(t, origDoc, "spec", "params"); !reflect.DeepEqual(got, map[string]any{"repo": "acme/api", "number": float64(99)}) {
		t.Fatalf("原任务 params 被改写了：%v", got)
	}
}

func TestTaskRerunRejectsInputThatViolatesTargetContract(t *testing.T) {
	env := newTaskResEnv(t)
	env.seedCapabilitySpec(env.ns, "review", 2, "sha256:taskres-v2",
		`{"params":{"type":"object","required":["repo"],"properties":{"repo":{"type":"string"}}}}`)
	orig := env.seedRerunTask(env.ns, env.cap, "review", 1, "sha256:taskres", `{"number":99}`, store.TaskFailed)

	body := trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), "{}"), 400, protocol.ReasonParamMissing)
	causes := trArray(t, body, "details", "causes")
	if len(causes) != 1 {
		t.Fatalf("details.causes = %v, want 一条", causes)
	}
	cause, _ := causes[0].(map[string]any)
	if got, _ := cause["field"].(string); got != "params.repo" {
		t.Fatalf("details.causes[0].field = %q, want params.repo", got)
	}

	var created int
	if err := env.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM tasks WHERE namespace_id = ? AND rerun_of = ?`, env.ns, orig).Scan(&created); err != nil {
		t.Fatalf("count rerun rows: %v", err)
	}
	if created != 0 {
		t.Fatalf("被拒的重跑建出了 %d 行任务", created)
	}

	doc := trWant(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"version":1}`), 200)
	if got := trNumber(t, doc, "spec", "capability", "version"); got != 1 {
		t.Fatalf("显式重跑版本 = %v, want 1", got)
	}
}

func TestTaskStatusIdempotentIsExposed(t *testing.T) {
	env := newTaskResEnv(t)
	id := env.insertTask(seededTask{phase: store.TaskFailed, attempt: 1, errText: "script_failed", finished: taskResInt64(store.Now())})
	doc := trWant(t, env.do("GET", env.taskPath(env.ns, id, ""), nil), 200)
	if _, ok := trObject(t, doc, "status")["idempotent"]; !ok {
		t.Fatalf("status.idempotent 缺失：%s", mustJSON(t, doc))
	}
}

func taskResInt64(v int64) *int64 { return &v }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		return "<marshal error>"
	}
	return string(b)
}
