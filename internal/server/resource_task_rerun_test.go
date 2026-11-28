package server

// 重跑（#605）的 HTTP 验收：新任务 + 原任务不变、版本选择、参数域封闭、status.idempotent。

import (
	"encoding/json"
	"reflect"
	"testing"

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
	// 重跑不是业务 emit 的父子关系：没有 parentTaskId，trace 是新的。
	if v, ok := trObject(t, doc, "spec")["parentTaskId"]; !ok || v != nil {
		t.Fatalf("重跑任务的 parentTaskId = %v, want null", v)
	}
	if got := trString(t, doc, "spec", "traceUID"); got == "" || got == "trace_"+orig {
		t.Fatalf("traceUID = %q（必须是一条新 trace）", got)
	}

	// 原任务一个字都没变。
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

	// 默认 = live（v2 是最高版本）。
	doc := trWant(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), ""), 200)
	if got := trNumber(t, doc, "spec", "capability", "version"); got != 2 {
		t.Fatalf("默认重跑版本 = %v, want 2（live）", got)
	}
	if got := trString(t, doc, "spec", "capability", "digest"); got != "sha256:taskres-v2" {
		t.Fatalf("默认重跑 digest = %q, want sha256:taskres-v2", got)
	}

	// 显式原版本 v1。
	doc = trWant(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"version":1}`), 200)
	if got := trNumber(t, doc, "spec", "capability", "version"); got != 1 {
		t.Fatalf("显式重跑版本 = %v, want 1", got)
	}
	if got := trString(t, doc, "spec", "capability", "digest"); got != "sha256:taskres" {
		t.Fatalf("显式重跑 digest = %q, want sha256:taskres", got)
	}
	_ = capV2

	// 不存在的版本 → 404（不静默换成 live）。
	trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"version":7}`), 404, reasonNotFound)
}

func TestTaskRerunRejectsBadInput(t *testing.T) {
	env := newTaskResEnv(t)
	orig := env.seedRerunTask(env.ns, env.cap, "review", 1, "sha256:taskres", `{}`, store.TaskFailed)

	// 未知字段：静默忽略会把拼写错误变成「按 live 跑了」。
	trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"verison":2}`), 400, "unknown_field")
	trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{"version":-1}`), 400, reasonBadRequest)
	// 坏 JSON（共享严格解码器的既有口径：解码失败一律 unknown_field）。
	trReason(t, env.do("POST", env.taskPath(env.ns, orig, "/rerun"), `{`), 400, reasonUnknownField)
	// 跨命名空间与不存在同一个回答：404。
	trReason(t, env.do("POST", env.taskPath(env.ns2, orig, "/rerun"), "{}"), 404, reasonNotFound)
	trReason(t, env.do("POST", env.taskPath(env.ns, "tsk_missing", "/rerun"), "{}"), 404, reasonNotFound)
}

// TestTaskStatusIdempotentIsExposed：前端「不会自动重试」的判断依据是真字段。
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
