package server

// Task / Event / Trace / logs / emissions / console events 的 HTTP 验收（W-task）。
//
// 覆盖的**契约点**（不是实现细节）：
//   - Task 是只读系统对象：信封四键 + metadata 无 resourceVersion（无 CAS 事实）+ status.phase 小写；
//   - 未解析派发失败（downstream_missing）：attempt=0、没有 nodeName/lease/startedAt、
//     result.error.reason 是封闭码、**不公开 version 0 / 不造 digest**（S1/E1）；
//   - 空列表恒 `[]`（items / lines / artifacts），不是 null；
//   - cancel / abandon 走 store.RequestCancel：未知 task 404、已知 pending 任务落终态且相位可读回；
//   - logs 参数域封闭（tailLines 0..5000、sinceSeconds >= 0），无档案 = 200 + attempt 0 + lines []；
//   - emissions 形状 `{metadata, items}`、默认最新在前、limit+continue 可翻页、游标绑 task；
//   - console events：缺 namespace 400、`*` 跨 ns 汇总、坏游标 400、游标不能跨 ns 复用。
//
// 本文件的夹具全部用**真库 + 真 ServeMux**（不起 WebSocket）：日志的「无档案」分支
// 用空 Hub 就能验（帧内容属于 ws 包自己的测试）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// taskResUserID 是夹具里那个已登录用户（AuthorizeNamespace 会核它存在）。
const taskResUserID = "usr_taskres"

// taskResEnv 是一套跑起来的执行事实读取域。
type taskResEnv struct {
	t   *testing.T
	db  *store.DB
	mux *http.ServeMux
	ns  string // 主命名空间 id（store 事实表引用）
	ns2 string // 第二个命名空间 id（隔离 / 跨 ns 汇总用例）
	// nsSlug 是主命名空间的对外名字（URL 与 metadata.namespace）。
	nsSlug string
	cap    string // 主命名空间里的能力行 id（tasks.capability_id 有外键）
	// deps 是装配指针：logs 用例要把 taskAPI 的 Hub 换成「背后有真 WebSocket 会话」
	// 的那一个（handler 在请求期读 a.d.Hub，注册后再改也生效）。
	deps *serverDeps
}

// newTaskResEnv 建库 / 用户 / 两个命名空间 + 一条能力行 + 挂好路由。
func newTaskResEnv(t *testing.T) *taskResEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "anotify.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.CreateUser(&store.User{ID: taskResUserID, Username: "task-res-tester", CreatedAt: store.Now()}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	primary, err := db.CreateNamespace(ctx, "taskres", "Task 资源")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	second, err := db.CreateNamespace(ctx, "taskres-b", "Task 资源 B")
	if err != nil {
		t.Fatalf("create second namespace: %v", err)
	}
	env := &taskResEnv{t: t, db: db, ns: primary.ID, ns2: second.ID, nsSlug: primary.Slug}
	env.cap = env.seedCapability(primary.ID, "review", 1, "sha256:taskres")

	deps := &serverDeps{
		DB:  db,
		Hub: ws.NewHub(db, nil),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// serverDeps.Now 是毫秒（D1）。会话中间件直接注入已登录身份：本文件的验收点是
		// 资源语义，「路由确实被 session 包裹」由 TestTaskResourceRoutesAreSessionWrapped 单独证明。
		Now: func() int64 { return store.Now() * 1000 },
		Session: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), taskResUserID)))
			})
		},
	}
	mux := http.NewServeMux()
	newTaskAPI(deps).register(mux)
	env.mux = mux
	env.deps = deps
	return env
}

// do 发一个请求（body 为 string 时按原样发送，便于构造坏 JSON）。
func (e *taskResEnv) do(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, taskResRequestBody(e.t, body))
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// tasksPath 是主命名空间的任务集合路径。
func (e *taskResEnv) tasksPath() string { return "/v1/namespaces/" + e.ns + "/tasks" }

// taskPath 是某条任务在**指定**命名空间下的路径。
func (e *taskResEnv) taskPath(ns, id, suffix string) string {
	return "/v1/namespaces/" + ns + "/tasks/" + id + suffix
}

// seedCapability 直接插一条能力行（tasks.capability_id 的外键目标）。
//
// 不走 CreateCapability：本文件的用例不关心能力正文，只关心任务行钉住的身份。
func (e *taskResEnv) seedCapability(ns, name string, version int, digest string) string {
	e.t.Helper()
	id := "capv_" + strings.ReplaceAll(digest, ":", "_") + "_" + name
	if _, err := e.db.Exec(
		`INSERT INTO capability_versions (id, namespace_id, name, version, spec, digest, fold_key, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, ns, name, version, `{}`, digest, "fold_"+id, store.Now()); err != nil {
		e.t.Fatalf("seed capability: %v", err)
	}
	return id
}

// seededTask 是插一条任务行时可改的字段（零值 = 该列的默认值）。
type seededTask struct {
	ns         string
	trace      string
	capID      string // 空串 + unresolved = 未解析派发失败（落 NULL）
	unresolved bool
	capName    string
	version    int
	digest     string
	phase      string
	attempt    int
	parent     string
	nodeName   string
	summary    string
	errText    string
	artifacts  string // 空 = []
	result     string // 空 = 没有结果事实
	waiting    string
	nextAt     *int64
	startedAt  *int64
	finished   *int64
	leaseAt    *int64
	configRev  int64
	nsVarsRev  int64
	createdAt  int64
}

// insertTask 插一条真实任务行并返回它的 id。
func (e *taskResEnv) insertTask(in seededTask) string {
	e.t.Helper()
	if in.ns == "" {
		in.ns = e.ns
	}
	if in.trace == "" {
		in.trace = "trace_taskres"
	}
	if in.capName == "" {
		in.capName = "review"
	}
	if in.capID == "" && !in.unresolved {
		in.capID = e.cap
	}
	if in.version == 0 && in.capID != "" {
		in.version = 1
	}
	if in.digest == "" && in.capID != "" {
		in.digest = "sha256:taskres"
	}
	if in.phase == "" {
		in.phase = store.TaskPending
	}
	if in.artifacts == "" {
		in.artifacts = "[]"
	}
	if in.createdAt == 0 {
		in.createdAt = store.Now()
	}
	id := store.NewTaskID()
	var capID any
	if in.capID != "" {
		capID = in.capID
	}
	var parent any
	if in.parent != "" {
		parent = in.parent
	}
	if _, err := e.db.Exec(
		`INSERT INTO tasks
		   (id, namespace_id, trace_id, parent_task_id, capability_id, capability, version, digest, params,
		    phase, attempt, node_id, node_name, config_revision, max_attempts, lease_until,
		    waiting_reason, next_attempt_at, summary, artifacts, error, created_at, started_at, finished_at,
		    namespace_vars_revision, result_json)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, in.ns, in.trace, parent, capID, in.capName, in.version, in.digest, `{"file":"a.go"}`,
		in.phase, in.attempt, in.nodeName, in.configRev, 3, in.leaseAt,
		in.waiting, in.nextAt, in.summary, in.artifacts, in.errText, in.createdAt, in.startedAt, in.finished,
		in.nsVarsRev, in.result); err != nil {
		e.t.Fatalf("insert task: %v", err)
	}
	return id
}

// seedEmit 插一条 emit 落库事实（回执原样进 result 列）。
func (e *taskResEnv) seedEmit(taskID string, emitID *string, toRef, result string, createdAt int64) string {
	e.t.Helper()
	id := "emt_" + toRef + "_" + store.NewTaskID()
	if _, err := e.db.Exec(
		`INSERT INTO emits (id, task_id, emit_id, to_ref, data, result, created_at) VALUES (?,?,?,?,?,?,?)`,
		id, taskID, emitID, toRef, `{}`, result, createdAt); err != nil {
		e.t.Fatalf("seed emit: %v", err)
	}
	return id
}

// seedEvent 插一条事件行（source='route' 即剪枝留痕）。
func (e *taskResEnv) seedEvent(ns, traceID, source, eventType, payload, reason string, createdAt int64) string {
	e.t.Helper()
	if ns == "" {
		ns = e.ns
	}
	if payload == "" {
		payload = "{}"
	}
	if reason == "" {
		reason = store.EventReasonIngested
	}
	id := store.NewEventID()
	if _, err := e.db.Exec(
		`INSERT INTO events (id, namespace_id, trace_id, source, type, payload, reason, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, ns, traceID, source, eventType, payload, reason, createdAt); err != nil {
		e.t.Fatalf("seed event: %v", err)
	}
	return id
}

// ---------- 断言工具 ----------

// trBody 解码响应体。
func trBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (code=%d body=%s)", err, rr.Code, rr.Body.String())
	}
	return out
}

// trWant 断言状态码并解码。
func trWant(t *testing.T, rr *httptest.ResponseRecorder, code int) map[string]any {
	t.Helper()
	if rr.Code != code {
		t.Fatalf("status = %d, want %d (body=%s)", rr.Code, code, rr.Body.String())
	}
	return trBody(t, rr)
}

// trObject 按路径取嵌套对象。
func trObject(t *testing.T, doc map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := trValue(t, doc, path...)
	obj, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("path %v 不是对象: %T", path, cur)
	}
	return obj
}

// trValue 按路径取任意值（缺失即失败）。
func trValue(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v 在 %q 处不是对象: %T", path, key, cur)
		}
		v, ok := obj[key]
		if !ok {
			t.Fatalf("path %v 缺键 %q（doc=%v）", path, key, doc)
		}
		cur = v
	}
	return cur
}

// trArray 按路径取数组。
func trArray(t *testing.T, doc map[string]any, path ...string) []any {
	t.Helper()
	cur := trValue(t, doc, path...)
	arr, ok := cur.([]any)
	if !ok {
		t.Fatalf("path %v 不是数组: %T (%v)", path, cur, cur)
	}
	return arr
}

// trString 按路径取字符串。
func trString(t *testing.T, doc map[string]any, path ...string) string {
	t.Helper()
	cur := trValue(t, doc, path...)
	s, ok := cur.(string)
	if !ok {
		t.Fatalf("path %v 不是字符串: %T (%v)", path, cur, cur)
	}
	return s
}

// trNumber 按路径取数字。
func trNumber(t *testing.T, doc map[string]any, path ...string) float64 {
	t.Helper()
	cur := trValue(t, doc, path...)
	n, ok := cur.(float64)
	if !ok {
		t.Fatalf("path %v 不是数字: %T (%v)", path, cur, cur)
	}
	return n
}

// trReason 断言 Status 响应体的 code/reason。
func trReason(t *testing.T, rr *httptest.ResponseRecorder, code int, reason string) map[string]any {
	t.Helper()
	doc := trWant(t, rr, code)
	if got := trString(t, doc, "kind"); got != kindStatus {
		t.Fatalf("kind = %q, want %q (body=%s)", got, kindStatus, rr.Body.String())
	}
	if got := trString(t, doc, "reason"); got != reason {
		t.Fatalf("reason = %q, want %q (body=%s)", got, reason, rr.Body.String())
	}
	return doc
}

// taskResRequestBody 把 body 编成 io.Reader（string 原样发）。
func taskResRequestBody(t *testing.T, body any) io.Reader {
	t.Helper()
	switch v := body.(type) {
	case nil:
		return nil
	case string:
		return strings.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		return strings.NewReader(string(raw))
	}
}

// ---------- 用例 ----------

// TestTaskResourceListEmptyIsArray 空列表是 `[]` 而不是 null（前端把它当"未连接"）。
func TestTaskResourceListEmptyIsArray(t *testing.T) {
	env := newTaskResEnv(t)
	rr := env.do(http.MethodGet, env.tasksPath(), nil)
	if !strings.Contains(rr.Body.String(), `"items":[]`) {
		t.Fatalf("空列表必须是 items:[]（不是 null）: %s", rr.Body.String())
	}
	doc := trWant(t, rr, http.StatusOK)
	if got := trString(t, doc, "kind"); got != "TaskList" {
		t.Fatalf("kind = %q, want TaskList", got)
	}
	if got := trString(t, doc, "apiVersion"); got != apiVersionV1 {
		t.Fatalf("apiVersion = %q, want %q", got, apiVersionV1)
	}
	if items := trArray(t, doc, "items"); len(items) != 0 {
		t.Fatalf("items 应空，得到 %d 条", len(items))
	}
}

// TestTaskResourceGetEnvelopeFacts 单 Task 信封：四键 + 事实投影（无 resourceVersion）。
func TestTaskResourceGetEnvelopeFacts(t *testing.T) {
	env := newTaskResEnv(t)
	now := store.Now()
	nextAt := now + 30
	parent := env.insertTask(seededTask{phase: store.TaskSucceeded, attempt: 1, errText: ""})
	taskID := env.insertTask(seededTask{
		phase:     store.TaskPending,
		parent:    parent,
		waiting:   store.WaitingForSlot,
		nextAt:    &nextAt,
		configRev: 7,
		nsVarsRev: 12,
		createdAt: now,
	})

	doc := trWant(t, env.do(http.MethodGet, env.taskPath(env.ns, taskID, ""), nil), http.StatusOK)
	for _, key := range []string{"apiVersion", "kind", "metadata", "spec", "status"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("信封缺 %q 键: %v", key, doc)
		}
	}
	if got := trString(t, doc, "kind"); got != "Task" {
		t.Fatalf("kind = %q, want Task", got)
	}
	meta := trObject(t, doc, "metadata")
	if got := trString(t, meta, "name"); got != taskID {
		t.Fatalf("metadata.name = %q, want %q", got, taskID)
	}
	if got := trString(t, meta, "namespace"); got != env.nsSlug {
		t.Fatalf("metadata.namespace = %q, want 路径身份（slug）%q", got, env.nsSlug)
	}
	if got := trString(t, meta, "uid"); got != taskID {
		t.Fatalf("metadata.uid = %q, want %q", got, taskID)
	}
	if got := trString(t, meta, "creationTimestamp"); got == "" {
		t.Fatal("metadata.creationTimestamp 不能为空（有真实 created_at）")
	}
	// Task 没有 resource_version 列（无 CAS）：不得编造令牌。
	if rv, ok := meta["resourceVersion"]; ok {
		t.Fatalf("Task 不该有 metadata.resourceVersion（无 CAS 事实），得到 %v", rv)
	}

	spec := trObject(t, doc, "spec")
	if got := trString(t, spec, "traceUID"); got != "trace_taskres" {
		t.Fatalf("spec.traceUID = %q", got)
	}
	if got := trString(t, spec, "parentTaskId"); got != parent {
		t.Fatalf("spec.parentTaskId = %q, want %q", got, parent)
	}
	capRef := trObject(t, spec, "capability")
	if got := trString(t, capRef, "name"); got != "review" {
		t.Fatalf("spec.capability.name = %q", got)
	}
	if got := trNumber(t, capRef, "version"); got != 1 {
		t.Fatalf("spec.capability.version = %v, want 1", got)
	}
	if got := trString(t, capRef, "digest"); got != "sha256:taskres" {
		t.Fatalf("spec.capability.digest = %q", got)
	}
	params := trObject(t, spec, "params")
	if got := trString(t, params, "file"); got != "a.go" {
		t.Fatalf("spec.params.file = %q", got)
	}

	status := trObject(t, doc, "status")
	if got := trString(t, status, "phase"); got != store.TaskPending {
		t.Fatalf("status.phase = %q, want %q（小写）", got, store.TaskPending)
	}
	if got := trString(t, status, "waitingReason"); got != store.WaitingForSlot {
		t.Fatalf("status.waitingReason = %q", got)
	}
	if got := trString(t, status, "nextAttemptAt"); got == "" {
		t.Fatal("status.nextAttemptAt 有真实事实时必须给时间")
	}
	if got := trNumber(t, status, "attempt"); got != 0 {
		t.Fatalf("status.attempt = %v, want 0", got)
	}
	if got := trNumber(t, status, "maxAttempts"); got != 3 {
		t.Fatalf("status.maxAttempts = %v, want 3", got)
	}
	if got := trString(t, status, "configRevision"); got != "7" {
		t.Fatalf("status.configRevision = %q, want \"7\"（配置生效版本是字符串）", got)
	}
	if got := trString(t, status, "namespaceVarsRevision"); got != "12" {
		t.Fatalf("status.namespaceVarsRevision = %q, want \"12\"", got)
	}
	if got := status["nodeName"]; got != nil {
		t.Fatalf("没有执行节点时 status.nodeName 必须是 null，得到 %v", got)
	}
	if got := status["startedAt"]; got != nil {
		t.Fatalf("没有执行时间时 status.startedAt 必须是 null，得到 %v", got)
	}
	result := trObject(t, status, "result")
	if got := trArray(t, result, "artifacts"); len(got) != 0 {
		t.Fatalf("result.artifacts 应为空数组，得到 %v", got)
	}
	if got := result["error"]; got != nil {
		t.Fatalf("没有失败事实时 result.error 必须是 null，得到 %v", got)
	}
}

// TestTaskResourceDownstreamMissingShape 未解析派发失败：attempt 0 + 无执行事实 + 不公开 version 0。
func TestTaskResourceDownstreamMissingShape(t *testing.T) {
	env := newTaskResEnv(t)
	finished := store.Now()
	taskID := env.insertTask(seededTask{
		unresolved: true, // 未解析：capability_id 落 NULL
		capName:    "fixer",
		version:    0,
		digest:     "",
		phase:      store.TaskFailed,
		attempt:    0,
		errText:    "downstream_missing",
		finished:   &finished,
	})

	doc := trWant(t, env.do(http.MethodGet, env.taskPath(env.ns, taskID, ""), nil), http.StatusOK)
	spec := trObject(t, doc, "spec")
	// S1/E1：失败 Task 必须保留**请求 basename**（否则「想调什么」这个事实就丢了），
	// 但没有解析出能力行 → version / digest 必须省略，不能公开哨兵 0 或编一个 hash。
	capi := trObject(t, spec, "capability")
	if got := trString(t, capi, "name"); got != "fixer" {
		t.Fatalf("spec.capability.name = %q, want 请求 basename fixer", got)
	}
	if got, ok := capi["version"]; ok && got != nil {
		t.Fatalf("未声明版本必须省略 version，得到 %v", got)
	}
	if got, ok := capi["digest"]; ok && got != nil && got != "" {
		t.Fatalf("未解析能力必须省略 digest，得到 %v", got)
	}
	status := trObject(t, doc, "status")
	if got := trString(t, status, "phase"); got != store.TaskFailed {
		t.Fatalf("status.phase = %q, want failed", got)
	}
	if got := trNumber(t, status, "attempt"); got != 0 {
		t.Fatalf("status.attempt = %v, want 0", got)
	}
	for _, key := range []string{"nodeName", "leaseExpiresAt", "startedAt"} {
		if got := status[key]; got != nil {
			t.Fatalf("未解析失败没有 %s 事实，必须是 null，得到 %v", key, got)
		}
	}
	if got := trArray(t, status, "result", "artifacts"); len(got) != 0 {
		t.Fatalf("result.artifacts 应为空数组，得到 %v", got)
	}
	errDoc := trObject(t, status, "result", "error")
	if got := trString(t, errDoc, "reason"); got != "downstream_missing" {
		t.Fatalf("result.error.reason = %q, want downstream_missing", got)
	}
}

// TestTaskResourceCancelUnknownIs404 未知 / 跨命名空间的任务 id 一律 404（不可探测）。
func TestTaskResourceCancelUnknownIs404(t *testing.T) {
	env := newTaskResEnv(t)
	trReason(t, env.do(http.MethodPost, env.taskPath(env.ns, "tsk_doesnotexist", "/cancel"), nil),
		http.StatusNotFound, reasonNotFound)

	// 任务真实存在，但不在这个命名空间：同样 404（不是 403）。
	taskID := env.insertTask(seededTask{})
	trReason(t, env.do(http.MethodPost, env.taskPath(env.ns2, taskID, "/cancel"), nil),
		http.StatusNotFound, reasonNotFound)
	if task, err := env.db.GetTask(context.Background(), env.ns, taskID); err != nil || task.Phase != store.TaskPending {
		t.Fatalf("跨 ns 的取消请求不得改动任务: err=%v task=%+v", err, task)
	}
}

// TestTaskResourceCancelPendingBecomesCancelled pending 任务：取消直接终态（没有执行者要等 ack）。
func TestTaskResourceCancelPendingBecomesCancelled(t *testing.T) {
	env := newTaskResEnv(t)
	taskID := env.insertTask(seededTask{})

	doc := trWant(t, env.do(http.MethodPost, env.taskPath(env.ns, taskID, "/cancel"), nil), http.StatusOK)
	if got := trString(t, doc, "kind"); got != "Task" {
		t.Fatalf("kind = %q, want Task", got)
	}
	if got := trString(t, doc, "status", "phase"); got != store.TaskCancelled {
		t.Fatalf("响应里的 status.phase = %q, want cancelled", got)
	}
	stored, err := env.db.GetTask(context.Background(), env.ns, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if stored.Phase != store.TaskCancelled {
		t.Fatalf("库里的 phase = %q, want cancelled", stored.Phase)
	}
	if stored.CancelReason != "cancelled_by_user" {
		t.Fatalf("cancel_reason = %q, want cancelled_by_user", stored.CancelReason)
	}
	// 终态任务不能再取消：这是调用方可理解的冲突，不是 500。
	trReason(t, env.do(http.MethodPost, env.taskPath(env.ns, taskID, "/cancel"), nil),
		http.StatusConflict, reasonConflict)
}

// TestTaskResourceAbandonPendingBecomesAbandoned abandon 用 abandoned_by_user，不走 cancel 的原因码。
func TestTaskResourceAbandonPendingBecomesAbandoned(t *testing.T) {
	env := newTaskResEnv(t)
	taskID := env.insertTask(seededTask{})

	doc := trWant(t, env.do(http.MethodPost, env.taskPath(env.ns, taskID, "/abandon"), `{"reason":"give up"}`), http.StatusOK)
	if got := trString(t, doc, "status", "phase"); got != store.TaskAbandoned {
		t.Fatalf("status.phase = %q, want abandoned", got)
	}
	stored, err := env.db.GetTask(context.Background(), env.ns, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if stored.Phase != store.TaskAbandoned {
		t.Fatalf("库里的 phase = %q, want abandoned", stored.Phase)
	}
	if stored.CancelReason != "give up" {
		t.Fatalf("cancel_reason = %q, want 调用方给的 reason（不被覆盖）", stored.CancelReason)
	}
}

// TestTaskResourceLogsParamBounds 参数域封闭：越界一律 400（不归一成「0 = 不限」）。
func TestTaskResourceLogsParamBounds(t *testing.T) {
	env := newTaskResEnv(t)
	taskID := env.insertTask(seededTask{})
	base := env.taskPath(env.ns, taskID, "/logs")
	for _, q := range []string{
		"?tailLines=-1",
		"?tailLines=5001",
		"?tailLines=abc",
		"?sinceSeconds=-1",
	} {
		trReason(t, env.do(http.MethodGet, base+q, nil), http.StatusBadRequest, reasonBadRequest)
	}
	// 边界值是合法的（0 = 不限，5000 = 上限），不该 400。
	for _, q := range []string{"?tailLines=0", "?tailLines=5000", "?sinceSeconds=0"} {
		if rr := env.do(http.MethodGet, base+q, nil); rr.Code == http.StatusBadRequest {
			t.Fatalf("%s 是合法参数，不该 400: %s", q, rr.Body.String())
		}
	}
	trReason(t, env.do(http.MethodGet, env.taskPath(env.ns, "tsk_missing", "/logs"), nil),
		http.StatusNotFound, reasonNotFound)
}

// TestTaskResourceLogsNoArchive 没有档案 = 200 + attempt 0 + lines []（不是 5xx，也不是假成功）。
func TestTaskResourceLogsNoArchive(t *testing.T) {
	env := newTaskResEnv(t)
	taskID := env.insertTask(seededTask{}) // 还没 grant：没有 attempt 行

	for _, q := range []string{"", "?previous=true", "?tailLines=10&sinceSeconds=60"} {
		rr := env.do(http.MethodGet, env.taskPath(env.ns, taskID, "/logs")+q, nil)
		if !strings.Contains(rr.Body.String(), `"lines":[]`) {
			t.Fatalf("无档案时 lines 必须是 []（不是 null）: %s", rr.Body.String())
		}
		doc := trWant(t, rr, http.StatusOK)
		if got := trNumber(t, doc, "attempt"); got != 0 {
			t.Fatalf("无档案时 attempt = %v, want 0", got)
		}
		if got := doc["truncated"]; got != false {
			t.Fatalf("无档案时 truncated 必须是 false，得到 %v", got)
		}
		if got := trString(t, doc, "taskId"); got != taskID {
			t.Fatalf("taskId = %q, want %q", got, taskID)
		}
	}
}

// TestTaskResourceEmissionsEmptyShape 空是 `{metadata:{},items:[]}`。
func TestTaskResourceEmissionsEmptyShape(t *testing.T) {
	env := newTaskResEnv(t)
	taskID := env.insertTask(seededTask{})

	rr := env.do(http.MethodGet, env.taskPath(env.ns, taskID, "/emissions"), nil)
	if !strings.Contains(rr.Body.String(), `"items":[]`) {
		t.Fatalf("空 emissions 必须是 items:[]（不是 null）: %s", rr.Body.String())
	}
	doc := trWant(t, rr, http.StatusOK)
	if meta := trObject(t, doc, "metadata"); len(meta) != 0 {
		t.Fatalf("首页没有更旧的行时 metadata 应为空对象，得到 %v", meta)
	}
	if items := trArray(t, doc, "items"); len(items) != 0 {
		t.Fatalf("items 应空，得到 %d 条", len(items))
	}
	// 未知任务 / 跨 ns 的任务 id → 404（子资源同样按命名空间作用域）。
	trReason(t, env.do(http.MethodGet, env.taskPath(env.ns, "tsk_missing", "/emissions"), nil),
		http.StatusNotFound, reasonNotFound)
	trReason(t, env.do(http.MethodGet, env.taskPath(env.ns2, taskID, "/emissions"), nil),
		http.StatusNotFound, reasonNotFound)
}

// TestTaskResourceEmissionsShapeAndCursor 默认最新在前 + 回执字段原样投影 + limit/continue 翻页。
func TestTaskResourceEmissionsShapeAndCursor(t *testing.T) {
	env := newTaskResEnv(t)
	taskID := env.insertTask(seededTask{})
	otherTask := env.insertTask(seededTask{})
	base := store.Now() - 100

	// 三条真实回执：正常派发 / 剪枝（taskId=null）/ 回执坏掉（只能回退到行上确定的事实）。
	env.seedEmit(taskID, taskResPtr("emit-1"), "fixer",
		`{"taskId":"tsk_child","eventId":"evt_1","to":"fixer@2","version":2}`, base)
	env.seedEmit(taskID, nil, "notify",
		`{"taskId":null,"eventId":"evt_2","to":"notify","pruned":{"reason":"param_missing","detail":"缺少 x"}}`, base+1)
	env.seedEmit(taskID, taskResPtr("emit-3"), "broken", `not-json`, base+2)

	doc := trWant(t, env.do(http.MethodGet, env.taskPath(env.ns, taskID, "/emissions"), nil), http.StatusOK)
	items := trArray(t, doc, "items")
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (body=%s)", len(items), "")
	}
	// 最新在前：坏回执那条（created_at 最大）排第一。
	first := trObject(t, items[0].(map[string]any))
	if got := trString(t, first, "to"); got != "broken" {
		t.Fatalf("items[0].to = %q, want broken（最新在前）", got)
	}
	if got := trString(t, first, "id"); got != "emit-3" {
		t.Fatalf("items[0].id = %q, want emit-3", got)
	}
	if got := first["taskId"]; got != nil {
		t.Fatalf("回执解析失败时 taskId 必须是 null，得到 %v", got)
	}
	if got := first["pruned"]; got != nil {
		t.Fatalf("回执解析失败时 pruned 必须是 null，得到 %v", got)
	}
	if _, ok := first["eventId"]; !ok {
		t.Fatal("每条 item 必须有 eventId 键（可以为空串）")
	}

	pruned := items[1].(map[string]any)
	if got := pruned["taskId"]; got != nil {
		t.Fatalf("剪枝回执的 taskId 必须是 null（已接收、没派发），得到 %v", got)
	}
	if got := trString(t, pruned, "eventId"); got != "evt_2" {
		t.Fatalf("items[1].eventId = %q, want evt_2", got)
	}
	if got := trString(t, pruned, "pruned", "reason"); got != "param_missing" {
		t.Fatalf("items[1].pruned.reason = %q", got)
	}

	dispatched := items[2].(map[string]any)
	if got := trString(t, dispatched, "eventId"); got != "evt_1" {
		t.Fatalf("items[2].eventId = %q, want evt_1", got)
	}
	if got := trString(t, dispatched, "to"); got != "fixer@2" {
		t.Fatalf("items[2].to = %q, want 回执里的 fixer@2（不是请求里的 toRef）", got)
	}
	if got := trNumber(t, dispatched, "version"); got != 2 {
		t.Fatalf("items[2].version = %v, want 2", got)
	}
	if got := trString(t, dispatched, "taskId"); got != "tsk_child" {
		t.Fatalf("items[2].taskId = %q, want tsk_child", got)
	}

	// limit=1：只给一条 + 一个不透明 continue。
	page1 := trWant(t, env.do(http.MethodGet, env.taskPath(env.ns, taskID, "/emissions")+"?limit=1", nil), http.StatusOK)
	if items := trArray(t, page1, "items"); len(items) != 1 {
		t.Fatalf("limit=1 应只回一条，得到 %d", len(items))
	}
	cursor := trString(t, page1, "metadata", "continue")
	if cursor == "" {
		t.Fatal("还有更旧的行时 metadata.continue 必须非空")
	}
	page2 := trWant(t, env.do(http.MethodGet,
		env.taskPath(env.ns, taskID, "/emissions")+"?limit=1&continue="+cursor, nil), http.StatusOK)
	items2 := trArray(t, page2, "items")
	if len(items2) != 1 {
		t.Fatalf("第二页应回 1 条，得到 %d", len(items2))
	}
	if got := trString(t, items2[0].(map[string]any), "eventId"); got != "evt_2" {
		t.Fatalf("第二页第一条 eventId = %q, want evt_2（紧接游标之后）", got)
	}

	// 坏游标 / 别的任务的游标 → 400（不静默回退成第一页）。
	trReason(t, env.do(http.MethodGet, env.taskPath(env.ns, taskID, "/emissions")+"?continue=%21%21", nil),
		http.StatusBadRequest, reasonBadRequest)
	otherCursor := encodeCursor(emissionCursorBinding(env.ns, otherTask), "emt_x", store.Now())
	trReason(t, env.do(http.MethodGet, env.taskPath(env.ns, taskID, "/emissions")+"?continue="+otherCursor, nil),
		http.StatusBadRequest, reasonBadRequest)
}

// TestTaskResourceConsoleEventsNamespaceScopes 跨 ns 汇总 / 缺 ns 400 / 游标绑 ns。
func TestTaskResourceConsoleEventsNamespaceScopes(t *testing.T) {
	env := newTaskResEnv(t)
	base := store.Now() - 100
	// ns A：3 条外部事件 + 1 条剪枝留痕（事件流默认不含 route）。
	aIDs := []string{
		env.seedEvent(env.ns, "trace_a", "github", "push", `{"n":1}`, "", base),
		env.seedEvent(env.ns, "trace_a", "github", "push", `{"n":2}`, "", base+1),
		env.seedEvent(env.ns, "trace_a", "gitlab", "tag", `{"n":3}`, "", base+2),
	}
	env.seedEvent(env.ns, "trace_a", store.EventPruneSource, "no_target", `{"reason":"no_target"}`, store.EventReasonPruned, base+3)
	bIDs := []string{
		env.seedEvent(env.ns2, "trace_b", "github", "push", `{"n":4}`, "", base+4),
		env.seedEvent(env.ns2, "trace_b", "github", "push", `{"n":5}`, "", base+5),
	}

	// 缺 namespace → 400（不回退默认命名空间）。
	trReason(t, env.do(http.MethodGet, consoleEventsPath, nil), http.StatusBadRequest, reasonBadRequest)

	// 单 ns：只看得到本 ns 的 3 条（剪枝行被 store 的事件流规则过滤掉）。
	doc := trWant(t, env.do(http.MethodGet, consoleEventsPath+"?namespace="+env.ns, nil), http.StatusOK)
	if got := trString(t, doc, "kind"); got != "EventList" {
		t.Fatalf("kind = %q, want EventList", got)
	}
	aItems := trArray(t, doc, "items")
	if len(aItems) != 3 {
		t.Fatalf("ns A 事件流 = %d 条, want 3（剪枝行不进事件流）", len(aItems))
	}
	if got := trString(t, aItems[0].(map[string]any), "metadata", "uid"); got != aIDs[2] {
		t.Fatalf("事件流最新在前: items[0] = %q, want %q", got, aIDs[2])
	}

	// namespace=*：跨 ns 汇总（5 条），顺序仍是 created_at desc。
	all := trWant(t, env.do(http.MethodGet, consoleEventsPath+"?namespace=*", nil), http.StatusOK)
	allItems := trArray(t, all, "items")
	if len(allItems) != 5 {
		t.Fatalf("跨 ns 汇总 = %d 条, want 5", len(allItems))
	}
	if got := trString(t, allItems[0].(map[string]any), "metadata", "uid"); got != bIDs[1] {
		t.Fatalf("跨 ns 汇总最新在前: items[0] = %q, want %q", got, bIDs[1])
	}

	// limit=2 翻页：三页分别是 2/2/1，且没有重复行。
	seen := map[string]bool{}
	path := consoleEventsPath + "?namespace=*&limit=2"
	pages := 0
	for path != "" && pages < 5 {
		page := trWant(t, env.do(http.MethodGet, path, nil), http.StatusOK)
		items := trArray(t, page, "items")
		for _, item := range items {
			uid := trString(t, item.(map[string]any), "metadata", "uid")
			if seen[uid] {
				t.Fatalf("游标翻页出现重复行 %q", uid)
			}
			seen[uid] = true
		}
		pages++
		cursor := ""
		if meta, ok := page["metadata"].(map[string]any); ok {
			cursor, _ = meta["continue"].(string)
		}
		if cursor == "" {
			break
		}
		path = consoleEventsPath + "?namespace=*&limit=2&continue=" + cursor
	}
	if len(seen) != 5 {
		t.Fatalf("翻页共取到 %d 条, want 5（判据见下）", len(seen))
	}

	// 坏游标 → 400。
	trReason(t, env.do(http.MethodGet, consoleEventsPath+"?namespace=*&continue=%21%21", nil),
		http.StatusBadRequest, reasonBadRequest)

	// 游标绑 ns：拿 A 的游标去读 B 必须 400（不能借它读越权记录）。
	pageA := trWant(t, env.do(http.MethodGet, consoleEventsPath+"?namespace="+env.ns+"&limit=1", nil), http.StatusOK)
	crossCursor := trString(t, pageA, "metadata", "continue")
	if crossCursor == "" {
		t.Fatal("ns A 有 3 条 + limit=1 时必须给 continue")
	}
	trReason(t, env.do(http.MethodGet,
		consoleEventsPath+"?namespace="+env.ns2+"&limit=1&continue="+crossCursor, nil),
		http.StatusBadRequest, reasonBadRequest)
}

// TestTaskResourceEventsAndTrace 命名空间事件列表 + trace 全景（events / tasks / pruned）。
func TestTaskResourceEventsAndTrace(t *testing.T) {
	env := newTaskResEnv(t)
	traceID := "trace_full"
	base := store.Now() - 50
	rootID := env.seedEvent(env.ns, traceID, "github", "push", `{"n":1}`, "", base)
	pruneID := env.seedEvent(env.ns, traceID, store.EventPruneSource, "param_missing",
		`{"reason":"param_missing","detail":"缺少 file","capability":"fixer"}`, store.EventReasonPruned, base+1)
	taskID := env.insertTask(seededTask{trace: traceID, phase: store.TaskRunning, attempt: 1})

	// 事件流：不带 trace → 剪枝行被过滤。
	doc := trWant(t, env.do(http.MethodGet, "/v1/namespaces/"+env.ns+"/events", nil), http.StatusOK)
	items := trArray(t, doc, "items")
	if len(items) != 1 {
		t.Fatalf("事件流 = %d 条, want 1（剪枝行不进事件流）", len(items))
	}
	ev := items[0].(map[string]any)
	for _, key := range []string{"apiVersion", "kind", "metadata", "spec", "status"} {
		if _, ok := ev[key]; !ok {
			t.Fatalf("Event 信封缺 %q 键: %v", key, ev)
		}
	}
	if got := trString(t, ev, "kind"); got != "Event" {
		t.Fatalf("kind = %q, want Event", got)
	}
	if got := trString(t, ev, "spec", "traceUID"); got != traceID {
		t.Fatalf("spec.traceUID = %q, want %q", got, traceID)
	}
	if got := trString(t, ev, "spec", "eventType"); got != "push" {
		t.Fatalf("spec.eventType = %q, want push", got)
	}
	if got := trString(t, ev, "spec", "involvedObject", "name"); got != "github" {
		t.Fatalf("spec.involvedObject.name = %q, want github", got)
	}
	if got := trString(t, ev, "metadata", "uid"); got != rootID {
		t.Fatalf("metadata.uid = %q, want %q", got, rootID)
	}

	// trace 视图：events 含剪枝行 + tasks + pruned（= source='route' 的行）。
	trace := trWant(t, env.do(http.MethodGet, "/v1/namespaces/"+env.ns+"/traces/"+traceID, nil), http.StatusOK)
	if got := trArray(t, trace, "events"); len(got) != 2 {
		t.Fatalf("trace.events = %d 条, want 2（按 trace 查含剪枝留痕）", len(got))
	}
	tasks := trArray(t, trace, "tasks")
	if len(tasks) != 1 {
		t.Fatalf("trace.tasks = %d 条, want 1", len(tasks))
	}
	if got := trString(t, tasks[0].(map[string]any), "metadata", "name"); got != taskID {
		t.Fatalf("trace.tasks[0].metadata.name = %q, want %q", got, taskID)
	}
	pruned := trArray(t, trace, "pruned")
	if len(pruned) != 1 {
		t.Fatalf("trace.pruned = %d 条, want 1", len(pruned))
	}
	pruneEnv := pruned[0].(map[string]any)
	if got := trString(t, pruneEnv, "metadata", "uid"); got != pruneID {
		t.Fatalf("trace.pruned[0].metadata.uid = %q, want %q", got, pruneID)
	}
	if got := trString(t, pruneEnv, "status", "pruned", "reason"); got != "param_missing" {
		t.Fatalf("trace.pruned[0].status.pruned.reason = %q", got)
	}
	if got := trString(t, pruneEnv, "status", "pruned", "detail"); got != "缺少 file" {
		t.Fatalf("trace.pruned[0].status.pruned.detail = %q", got)
	}

	// 未知 trace → 404（不区分「没事件」与「不存在」）。
	trReason(t, env.do(http.MethodGet, "/v1/namespaces/"+env.ns+"/traces/trace_missing", nil),
		http.StatusNotFound, reasonNotFound)
}

// TestTaskResourceListFilters 列表过滤走 store.TaskFilter（显式白名单，不引 selector）。
func TestTaskResourceListFilters(t *testing.T) {
	env := newTaskResEnv(t)
	// created_at 显式错开：同一秒的三行靠 id 破平局，顺序不可断言（store 的序是 created_at desc, id desc）。
	base := store.Now() - 10
	pendingA := env.insertTask(seededTask{trace: "trace_a", capName: "review", createdAt: base})
	runningB := env.insertTask(seededTask{trace: "trace_b", capName: "notify", phase: store.TaskRunning, attempt: 1, createdAt: base + 1})
	newest := env.insertTask(seededTask{trace: "trace_a", capName: "notify", createdAt: base + 2})

	ids := func(doc map[string]any) []string {
		out := []string{}
		for _, item := range trArray(t, doc, "items") {
			out = append(out, trString(t, item.(map[string]any), "metadata", "name"))
		}
		return out
	}

	if got := ids(trWant(t, env.do(http.MethodGet, env.tasksPath()+"?phase="+store.TaskRunning, nil), http.StatusOK)); len(got) != 1 || got[0] != runningB {
		t.Fatalf("phase=running 过滤得到 %v, want [%s]", got, runningB)
	}
	if got := ids(trWant(t, env.do(http.MethodGet, env.tasksPath()+"?trace=trace_a", nil), http.StatusOK)); len(got) != 2 {
		t.Fatalf("trace=trace_a 过滤得到 %v, want 2 条", got)
	}
	if got := ids(trWant(t, env.do(http.MethodGet, env.tasksPath()+"?capability=notify", nil), http.StatusOK)); len(got) != 2 {
		t.Fatalf("capability=notify 过滤得到 %v, want 2 条", got)
	}
	got := ids(trWant(t, env.do(http.MethodGet, env.tasksPath()+"?limit=1", nil), http.StatusOK))
	if len(got) != 1 || got[0] != newest {
		t.Fatalf("limit=1 应回最新一条 %s，得到 %v", newest, got)
	}
	if pendingA == runningB || pendingA == newest || runningB == newest {
		t.Fatal("夹具 id 撞了")
	}
	// 非整数 limit 是调用方错误（不静默落回默认值）。
	trReason(t, env.do(http.MethodGet, env.tasksPath()+"?limit=abc", nil), http.StatusBadRequest, reasonBadRequest)
}

// TestTaskResourceCancelRunningRecordsIntent running 任务只记取消意图（终态等 cancel.ack），
// 且「下发失败」不把请求判成失败（意图已经落库）。
func TestTaskResourceCancelRunningRecordsIntent(t *testing.T) {
	env := newTaskResEnv(t)
	// attempt=1 但故意不建 task_attempts 行：中枢找不到执行绑定，只能落意图（帧发不出去）。
	taskID := env.insertTask(seededTask{phase: store.TaskRunning, attempt: 1, nodeName: "anode-1"})

	doc := trWant(t, env.do(http.MethodPost, env.taskPath(env.ns, taskID, "/cancel"), nil), http.StatusOK)
	if got := trString(t, doc, "status", "phase"); got != store.TaskRunning {
		t.Fatalf("running 任务的取消不该直接终态（进程可能还在跑），status.phase = %q", got)
	}
	stored, err := env.db.GetTask(context.Background(), env.ns, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if stored.Phase != store.TaskRunning {
		t.Fatalf("库里的 phase = %q, want running（等 cancel.ack 才终态）", stored.Phase)
	}
	if stored.CancelRequestedAt == nil {
		t.Fatal("running 任务取消必须留下 cancel_requested_at 事实")
	}
	if stored.CancelReason != "cancelled_by_user" {
		t.Fatalf("cancel_reason = %q, want cancelled_by_user", stored.CancelReason)
	}
}

// TestTaskResourceRoutesAreSessionWrapped 全部路由都过会话中间件（未登录 401，而不是裸跑）。
func TestTaskResourceRoutesAreSessionWrapped(t *testing.T) {
	env := newTaskResEnv(t)
	deps := &serverDeps{
		DB: env.db,
		Session: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeStatus(w, http.StatusUnauthorized, reasonUnauthorized, "login required", nil)
			})
		},
	}
	mux := http.NewServeMux()
	newTaskAPI(deps).register(mux)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, env.tasksPath()},
		{http.MethodGet, env.taskPath(env.ns, "tsk_x", "")},
		{http.MethodPost, env.taskPath(env.ns, "tsk_x", "/cancel")},
		{http.MethodPost, env.taskPath(env.ns, "tsk_x", "/abandon")},
		{http.MethodGet, env.taskPath(env.ns, "tsk_x", "/logs")},
		{http.MethodGet, env.taskPath(env.ns, "tsk_x", "/emissions")},
		{http.MethodGet, "/v1/namespaces/" + env.ns + "/events"},
		{http.MethodGet, "/v1/namespaces/" + env.ns + "/traces/trace_x"},
		{http.MethodGet, consoleEventsPath + "?namespace=" + env.ns},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401（必须过 session 中间件）", tc.method, tc.path, rr.Code)
		}
	}
}

// taskResPtr 是取地址的小工具（构造可选幂等键用）。
func taskResPtr(s string) *string { return &s }

// ---------- fresh review 补测：logs 降级矩阵（G3-D3） ----------

// taskResIntPtr 是槽位指针小工具（hello.instances[].slot 是可选指针）。
func taskResIntPtr(v int) *int { return &v }

// insertAttempt 插一条真实的执行事实（task_attempts）：logs 的「这次跑在哪台机器」
// 只能来自这里，不能拿 tasks.node_name 冒充历史。
func (e *taskResEnv) insertAttempt(taskID string, attempt int, nodeID, nodeName string) {
	e.t.Helper()
	if _, err := e.db.Exec(
		`INSERT INTO task_attempts (task_id, attempt, node_id, node_name, phase, started_at)
		 VALUES (?,?,?,?,?,?)`,
		taskID, attempt, nodeID, nodeName, store.TaskRunning, store.Now()); err != nil {
		e.t.Fatalf("seed task attempt %s/%d: %v", taskID, attempt, err)
	}
}

// logsNodeEnv 是「节点在线」的 logs 夹具：真 store + 真 Hub + 真 WebSocket 会话。
type logsNodeEnv struct {
	*taskResEnv
	hub  *ws.Hub
	node *store.Node
	conn *websocket.Conn
}

// newNodeLogsEnv 起一条真节点会话（hello → welcome），并把 env 的 taskAPI 指向同一个 Hub。
func newNodeLogsEnv(t *testing.T) *logsNodeEnv {
	t.Helper()
	env := newTaskResEnv(t)
	ctx := context.Background()

	node := &store.Node{
		Name: "node-logs", MaxInstances: 4, MaxConcurrency: 2,
		DeclaredNamespaces: []string{env.nsSlug},
	}
	token, err := env.db.CreateNode(ctx, node)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	stored, err := env.db.GetNodeByName(ctx, node.Name)
	if err != nil {
		t.Fatalf("read node: %v", err)
	}

	hub := ws.NewHub(env.db, route.New(env.db),
		ws.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		ws.WithHelloTimeout(2*time.Second),
		ws.WithWriteTimeout(2*time.Second),
	)
	// handler 在请求期读 a.d.Hub：换成「这条真会话所在」的 Hub。
	env.deps.Hub = hub

	srv := httptest.NewServer(http.HandlerFunc(hub.ServeHTTP))
	t.Cleanup(srv.Close)

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/node/stream", nil)
	if err != nil {
		t.Fatalf("dial node stream: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	taskResWriteFrame(t, conn, &protocol.HelloFrame{
		Type: protocol.FrameHello, Name: node.Name, Token: token,
		Namespaces: []string{env.nsSlug},
		Instances:  []protocol.HelloInstance{{Slot: taskResIntPtr(0)}},
	})
	if frame := taskResReadFrame(t, conn); frame == nil {
		t.Fatal("hello 之后没有收到任何帧")
	} else if _, ok := frame.(*protocol.WelcomeFrame); !ok {
		t.Fatalf("hello 之后应收到 welcome，得到 %T", frame)
	}
	// welcome 先于 attach 写出：等注册表可见，避免与 ServeHTTP 的注册步骤竞争。
	deadline := time.Now().Add(2 * time.Second)
	for !hub.Online(stored.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("节点 %s 未注册到 hub", node.Name)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &logsNodeEnv{taskResEnv: env, hub: hub, node: stored, conn: conn}
}

// taskResWriteFrame 编码一帧写给节点连接。
func taskResWriteFrame(t *testing.T, conn *websocket.Conn, frame any) {
	t.Helper()
	data, err := protocol.Encode(frame)
	if err != nil {
		t.Fatalf("encode %T: %v", frame, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write %T: %v", frame, err)
	}
}

// taskResReadFrame 读一帧节点连接上的帧（超时即失败）。
func taskResReadFrame(t *testing.T, conn *websocket.Conn) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读节点帧: %v", err)
	}
	frame, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("解码节点帧: %v（%s）", err, data)
	}
	return frame
}

// taskResReplyLogs 在节点侧读一帧 logs.request，再用 build 造出的 logs.response 回给中枢。
//
// 为什么放在 goroutine 里：HTTP 请求本身在等这一帧回包（真同步链路），所以读/回必须并发。
// 辅助 goroutine 里不能 t.Fatalf：错误一律经 channel 交回主 goroutine 断言。
func taskResReplyLogs(conn *websocket.Conn, build func(req *protocol.LogsRequestFrame) *protocol.LogsResponseFrame) <-chan error {
	ch := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, data, err := conn.Read(ctx)
		if err != nil {
			ch <- fmt.Errorf("读节点帧: %w", err)
			return
		}
		frame, err := protocol.Decode(data)
		if err != nil {
			ch <- fmt.Errorf("解码节点帧: %w", err)
			return
		}
		req, ok := frame.(*protocol.LogsRequestFrame)
		if !ok {
			ch <- fmt.Errorf("节点应收到 logs.request，得到 %T", frame)
			return
		}
		out, err := protocol.Encode(build(req))
		if err != nil {
			ch <- fmt.Errorf("编码 logs.response: %w", err)
			return
		}
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer wcancel()
		ch <- conn.Write(wctx, websocket.MessageText, out)
	}()
	return ch
}

// TestTaskResourceLogsNodeOfflineIs503 钉降级矩阵的第一格：
// 执行过这次 attempt 的节点不在线 → 503 + reason node_offline（不是 200 空日志）。
func TestTaskResourceLogsNodeOfflineIs503(t *testing.T) {
	env := newTaskResEnv(t)
	taskID := env.insertTask(seededTask{phase: store.TaskRunning, attempt: 1, nodeName: "node-gone"})
	env.insertAttempt(taskID, 1, "nd_offline_fresh_review", "node-gone")

	rr := env.do(http.MethodGet, env.taskPath(env.ns, taskID, "/logs"), nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("节点离线时期望 503，得到 %d：%s", rr.Code, rr.Body.String())
	}
	body := trBody(t, rr)
	if kind := trString(t, body, "kind"); kind != kindStatus {
		t.Fatalf("离线降级必须是 Status 文档，kind = %q", kind)
	}
	if code := trNumber(t, body, "code"); code != http.StatusServiceUnavailable {
		t.Fatalf("Status.code = %v, want 503", code)
	}
	if reason := trString(t, body, "reason"); reason != "node_offline" {
		t.Fatalf("离线降级的 reason 期望 %q，得到 %q（body=%s）", "node_offline", reason, rr.Body.String())
	}
	// 降级不是「真空日志」：不许出现 lines 键（否则客户端会把「节点离线」读成「这次没输出」）。
	if _, ok := body["lines"]; ok {
		t.Fatalf("503 响应不该带 lines：%s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"lines"`) {
		t.Fatalf("503 响应原文不该出现 lines：%s", rr.Body.String())
	}
}

// TestTaskResourceLogsNodeErrorIs502 钉降级矩阵的第二格：
// 节点在线但读不了档案（logs_unreadable）→ 502 + 节点给的封闭码（不伪造空成功）。
func TestTaskResourceLogsNodeErrorIs502(t *testing.T) {
	env := newNodeLogsEnv(t)
	taskID := env.insertTask(seededTask{phase: store.TaskRunning, attempt: 1, nodeName: env.node.Name})
	env.insertAttempt(taskID, 1, env.node.ID, env.node.Name)

	done := taskResReplyLogs(env.conn, func(req *protocol.LogsRequestFrame) *protocol.LogsResponseFrame {
		// 错误响应不伪造 attempt、不带 lines（封闭码原样回传）。
		return &protocol.LogsResponseFrame{
			Type: protocol.FrameLogsResponse, TaskID: req.TaskID, RequestID: req.RequestID,
			Error: protocol.LogsErrUnreadable,
		}
	})
	rr := env.do(http.MethodGet, env.taskPath(env.ns, taskID, "/logs"), nil)
	if err := <-done; err != nil {
		t.Fatalf("节点侧回包失败: %v", err)
	}

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("节点读不了档案时期望 502，得到 %d：%s", rr.Code, rr.Body.String())
	}
	body := trBody(t, rr)
	if code := trNumber(t, body, "code"); code != http.StatusBadGateway {
		t.Fatalf("Status.code = %v, want 502", code)
	}
	if reason := trString(t, body, "reason"); reason != protocol.LogsErrUnreadable {
		t.Fatalf("502 的 reason 应原样回传节点的封闭码 %q，得到 %q", protocol.LogsErrUnreadable, reason)
	}
	if _, ok := body["lines"]; ok {
		t.Fatalf("502 不该伪造 lines：%s", rr.Body.String())
	}
}

// TestTaskResourceLogsWaitTimeoutIs504 钉降级矩阵的第三格「超时 → 504」。
//
// 触发条件是真的：节点在线、真的收到了 logs.request，但**不回包**。中枢等回包的分支
// （taskLogsTimeout / 请求期限到 / 客户端放弃）统一映射 504 + reason timeout；这里用
// 可取消的请求上下文把「等不到回包」确定性地造出来（不必真等 10s 的 taskLogsTimeout，
// Hub 自己的 ErrLogsTimeout 由 internal/ws 的用例覆盖）。
func TestTaskResourceLogsWaitTimeoutIs504(t *testing.T) {
	env := newNodeLogsEnv(t)
	taskID := env.insertTask(seededTask{phase: store.TaskRunning, attempt: 1, nodeName: env.node.Name})
	env.insertAttempt(taskID, 1, env.node.ID, env.node.Name)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	req := httptest.NewRequest(http.MethodGet, env.taskPath(env.ns, taskID, "/logs"), nil).WithContext(reqCtx)
	rr := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		env.mux.ServeHTTP(rr, req)
		close(served)
	}()

	// 节点确实收到了 logs.request —— 证明中枢真的向在线节点发了请求（不是提前短路）。
	frame := taskResReadFrame(t, env.conn)
	reqFrame, ok := frame.(*protocol.LogsRequestFrame)
	if !ok {
		t.Fatalf("节点应收到 logs.request，得到 %T", frame)
	}
	if reqFrame.TaskID != taskID || reqFrame.Attempt != 1 {
		t.Fatalf("logs.request 关联字段不对: %+v", reqFrame)
	}
	// 节点故意不回答：等不到回包必须 504，不能伪造 200 空日志。
	cancelReq()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("节点不回包时 logs 请求没有结束（缺少有界等待）")
	}

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("等不到回包时期望 504，得到 %d：%s", rr.Code, rr.Body.String())
	}
	body := trBody(t, rr)
	if code := trNumber(t, body, "code"); code != http.StatusGatewayTimeout {
		t.Fatalf("Status.code = %v, want 504", code)
	}
	if reason := trString(t, body, "reason"); reason != protocol.ReasonTimeout {
		t.Fatalf("504 的 reason 期望封闭码 %q，得到 %q", protocol.ReasonTimeout, reason)
	}
	if strings.Contains(rr.Body.String(), `"lines"`) {
		t.Fatalf("504 不该伪造 lines：%s", rr.Body.String())
	}
}

// TestTaskResourceLogsNodeReturnsStructuredLines 钉「成功路径是逐条记录」：
// tailLines/sinceSeconds 原样转发给节点，节点给的真实记录（sequence/stream/text/at）
// 逐字段投影，attempt 是真实 attempt 的整数，lines 恒为数组。
func TestTaskResourceLogsNodeReturnsStructuredLines(t *testing.T) {
	env := newNodeLogsEnv(t)
	taskID := env.insertTask(seededTask{phase: store.TaskRunning, attempt: 1, nodeName: env.node.Name})
	env.insertAttempt(taskID, 1, env.node.ID, env.node.Name)

	var gotReq *protocol.LogsRequestFrame
	done := taskResReplyLogs(env.conn, func(req *protocol.LogsRequestFrame) *protocol.LogsResponseFrame {
		gotReq = req
		frame := &protocol.LogsResponseFrame{
			Type: protocol.FrameLogsResponse, TaskID: req.TaskID, RequestID: req.RequestID,
			Attempt: 1, Truncated: true,
		}
		frame.SetLogLines([]protocol.LogLine{
			{Sequence: 1, Stream: "stdout", Text: "hello", At: "2024-01-02T03:04:05Z"},
			{Sequence: 2, Stream: "stderr", Text: "warn", At: "2024-01-02T03:04:06Z"},
		})
		return frame
	})
	rr := env.do(http.MethodGet, env.taskPath(env.ns, taskID, "/logs")+"?tailLines=10&sinceSeconds=60", nil)
	if err := <-done; err != nil {
		t.Fatalf("节点侧回包失败: %v", err)
	}

	// 真实查询参数被原样转发（中枢不自己截断/重排序）。
	if gotReq.TailLines != 10 || gotReq.SinceSeconds != 60 {
		t.Fatalf("logs.request 应原样带 tailLines/sinceSeconds，得到 %+v", gotReq)
	}

	if rr.Code != http.StatusOK {
		t.Fatalf("节点正常回包期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"lines":[`) {
		t.Fatalf("lines 必须是 JSON 数组，得到 %s", rr.Body.String())
	}
	body := trBody(t, rr)
	if got := trNumber(t, body, "attempt"); got != 1 {
		t.Fatalf("attempt 应是真实读到的 attempt=1，得到 %v", got)
	}
	if got := trString(t, body, "taskId"); got != taskID {
		t.Fatalf("taskId = %q, want %q", got, taskID)
	}
	if got := body["truncated"]; got != true {
		t.Fatalf("truncated 应原样回传 true，得到 %v", got)
	}
	lines := trArray(t, body, "lines")
	if len(lines) != 2 {
		t.Fatalf("lines 应有 2 条，得到 %d：%v", len(lines), lines)
	}
	first, ok := lines[0].(map[string]any)
	if !ok {
		t.Fatalf("lines[0] 不是对象：%T", lines[0])
	}
	if seq := trNumber(t, first, "sequence"); seq != 1 {
		t.Fatalf("lines[0].sequence = %v, want 1", seq)
	}
	if stream := trString(t, first, "stream"); stream != "stdout" {
		t.Fatalf("lines[0].stream = %q, want stdout", stream)
	}
	if text := trString(t, first, "text"); text != "hello" {
		t.Fatalf("lines[0].text = %q, want hello", text)
	}
	if at := trString(t, first, "at"); at != "2024-01-02T03:04:05Z" {
		t.Fatalf("lines[0].at = %q, want 节点给的原始时间", at)
	}
	second, ok := lines[1].(map[string]any)
	if !ok {
		t.Fatalf("lines[1] 不是对象：%T", lines[1])
	}
	if stream := trString(t, second, "stream"); stream != "stderr" {
		t.Fatalf("lines[1].stream = %q, want stderr", stream)
	}
}
