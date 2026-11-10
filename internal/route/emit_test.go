package route

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

type emitFixture struct {
	db        *store.DB
	namespace *store.Namespace
	parent    *store.Task
	node      *store.Node
	token     string
	auth      store.EmitAuth
	router    *Router
}

func newEmitFixture(t *testing.T) *emitFixture {
	t.Helper()
	db := testDB(t)
	ns := testNamespace(t, db)
	producer := testCapability(t, db, ns.ID, "producer", 1, capOpts{Next: []string{"consumer"}})
	testCapability(t, db, ns.ID, "consumer", 1, capOpts{
		Params: `{"type":"object","properties":{"file":{"type":"string"}},"required":["file"]}`,
	})
	node, token := testNode(t, db)
	tokenHash := store.HashNodeToken(token)
	parent, leaseID, attemptToken := testRunningTask(t, db, ns.ID, "trace-emit", producer, node, tokenHash)
	return &emitFixture{
		db: db, namespace: ns, parent: parent, node: node, token: token,
		router: New(db),
		auth: store.EmitAuth{
			TaskID: parent.ID, NodeID: node.ID, NodeTokenHash: tokenHash,
			Attempt: 1, LeaseID: leaseID, AttemptToken: attemptToken, Now: store.Now(),
		},
	}
}

func (f *emitFixture) childCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent_task_id = ?`, f.parent.ID).Scan(&n); err != nil {
		t.Fatalf("统计子任务: %v", err)
	}
	return n
}

func TestEmitCreatesPinnedChildTask(t *testing.T) {
	f := newEmitFixture(t)
	result, err := f.router.Emit(testCtx(), EmitInput{
		Auth: f.auth, To: "consumer", Data: json.RawMessage(`{"file":"a.go","ignored":true}`),
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if result.Duplicate {
		t.Fatal("首次 emit 不是幂等回放")
	}
	if result.Response.TaskID == nil {
		t.Fatalf("成功 emit 必须回 taskId，得到 %+v", result.Response)
	}
	if result.Response.To != "consumer" || result.Response.Version != 1 {
		t.Fatalf("回执目标/版本不对: %+v", result.Response)
	}
	child, err := f.db.GetTask(testCtx(), f.namespace.ID, *result.Response.TaskID)
	if err != nil {
		t.Fatalf("读子任务: %v", err)
	}
	if child.ParentTaskID == nil || *child.ParentTaskID != f.parent.ID {
		t.Fatalf("子任务缺 parentTaskId: %+v", child.ParentTaskID)
	}
	if child.TraceID != f.parent.TraceID {
		t.Fatalf("子任务 trace = %q，want %q", child.TraceID, f.parent.TraceID)
	}
	if child.Capability != "consumer" || child.Version != 1 || child.Digest == "" {
		t.Fatalf("子任务未钉住目标引用: %+v", child)
	}
	var params map[string]any
	_ = json.Unmarshal(child.Params, &params)
	if params["file"] != "a.go" {
		t.Fatalf("同名直取未落到任务 params: %v", params)
	}
}

func TestEmitWithoutIDAlwaysCreatesNew(t *testing.T) {
	f := newEmitFixture(t)
	first, err := f.router.Emit(testCtx(), EmitInput{Auth: f.auth, To: "consumer", Data: json.RawMessage(`{"file":"a.go"}`)})
	if err != nil {
		t.Fatalf("第一次 emit: %v", err)
	}
	second, err := f.router.Emit(testCtx(), EmitInput{Auth: f.auth, To: "consumer", Data: json.RawMessage(`{"file":"a.go"}`)})
	if err != nil {
		t.Fatalf("第二次 emit: %v", err)
	}
	if *first.Response.TaskID == *second.Response.TaskID {
		t.Fatal("无 id 的两次 emit 必须是两条新派发")
	}
	if got := f.childCount(t); got != 2 {
		t.Fatalf("子任务数 = %d，want 2（无 id 不按序号去重）", got)
	}
}

func TestEmitSameIDReplaysAndConflicts(t *testing.T) {
	f := newEmitFixture(t)
	first, err := f.router.Emit(testCtx(), EmitInput{
		Auth: f.auth, To: "consumer", EmitID: "review-a.go", Data: json.RawMessage(`{"file":"a.go"}`),
	})
	if err != nil {
		t.Fatalf("首次 emit: %v", err)
	}
	replay, err := f.router.Emit(testCtx(), EmitInput{
		Auth: f.auth, To: "consumer", EmitID: "review-a.go", Data: json.RawMessage(`{"file":"a.go"}`),
	})
	if err != nil {
		t.Fatalf("同 id 同请求应回放: %v", err)
	}
	if !replay.Duplicate {
		t.Fatal("同 id 同请求必须标记为幂等回放")
	}
	if *replay.Response.TaskID != *first.Response.TaskID {
		t.Fatalf("回放必须返回旧 taskId：%s != %s", *replay.Response.TaskID, *first.Response.TaskID)
	}
	if got := f.childCount(t); got != 1 {
		t.Fatalf("同 id 同请求只派发一次，子任务数 = %d", got)
	}
	_, err = f.router.Emit(testCtx(), EmitInput{
		Auth: f.auth, To: "consumer", EmitID: "review-a.go", Data: json.RawMessage(`{"file":"b.go"}`),
	})
	var emitErr *EmitError
	if !errors.As(err, &emitErr) || emitErr.Reason != protocol.ReasonEmitIDConflict {
		t.Fatalf("同 id 不同请求必须 409 emit_id_conflict，得到 %v", err)
	}
	if emitErr.HTTPStatus() != 409 || emitErr.ExitCode() != protocol.EmitExitIDConflict {
		t.Fatalf("冲突的状态码/退出码不对: %d/%d", emitErr.HTTPStatus(), emitErr.ExitCode())
	}
	if got := f.childCount(t); got != 1 {
		t.Fatalf("冲突不得派发，子任务数 = %d", got)
	}
}

func TestEmitUndeclaredTargetRejected(t *testing.T) {
	f := newEmitFixture(t)
	_, err := f.router.Emit(testCtx(), EmitInput{Auth: f.auth, To: "ghost", Data: json.RawMessage(`{}`)})
	var emitErr *EmitError
	if !errors.As(err, &emitErr) || emitErr.Reason != protocol.ReasonTargetNotDeclared {
		t.Fatalf("未声明目标必须 target_not_declared，得到 %v", err)
	}
	if got := f.childCount(t); got != 0 {
		t.Fatalf("未声明目标不得建任务，子任务数 = %d", got)
	}
}

func TestEmitMissingTargetRejected(t *testing.T) {
	f := newEmitFixture(t)
	_, err := f.router.Emit(testCtx(), EmitInput{Auth: f.auth, To: "", Data: json.RawMessage(`{}`)})
	var emitErr *EmitError
	if !errors.As(err, &emitErr) || emitErr.Reason != protocol.ReasonMissingTarget {
		t.Fatalf("缺目标必须 missing_target，得到 %v", err)
	}
}

func TestEmitPruneReceiptAndTrace(t *testing.T) {
	f := newEmitFixture(t)
	result, err := f.router.Emit(testCtx(), EmitInput{Auth: f.auth, To: "consumer", Data: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("缺参应按剪枝回执，不该是错误: %v", err)
	}
	if result.Response.TaskID != nil {
		t.Fatalf("剪枝回执必须 taskId:null，得到 %+v", result.Response)
	}
	if result.Response.Pruned == nil || result.Response.Pruned.Reason != protocol.ReasonParamMissing {
		t.Fatalf("剪枝原因不对: %+v", result.Response.Pruned)
	}
	if result.Response.EventID == "" {
		t.Fatal("剪枝回执必须有真实 eventId（留痕行）")
	}
	if got := f.childCount(t); got != 0 {
		t.Fatalf("剪枝不得建任务，子任务数 = %d", got)
	}
	// 剪枝留痕：source='route' 的事件行必须真实存在。
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM events WHERE trace_id = ? AND source = 'route' AND type = ?`,
		f.parent.TraceID, protocol.ReasonParamMissing).Scan(&n); err != nil {
		t.Fatalf("统计剪枝事件: %v", err)
	}
	if n != 1 {
		t.Fatalf("剪枝留痕行数 = %d，want 1", n)
	}
}

func TestEmitAuthorizesBeforeReplay(t *testing.T) {
	f := newEmitFixture(t)
	if _, err := f.router.Emit(testCtx(), EmitInput{
		Auth: f.auth, To: "consumer", EmitID: "keep", Data: json.RawMessage(`{"file":"a.go"}`),
	}); err != nil {
		t.Fatalf("首次 emit: %v", err)
	}
	// 令牌轮换：旧会话身份不再是当前事实。
	if err := f.db.SetNodeTokenHash(testCtx(), f.node.ID, store.HashNodeToken("rotated-token")); err != nil {
		t.Fatalf("轮换节点令牌: %v", err)
	}
	_, err := f.router.Emit(testCtx(), EmitInput{
		Auth: f.auth, To: "consumer", EmitID: "keep", Data: json.RawMessage(`{"file":"a.go"}`),
	})
	var emitErr *EmitError
	if !errors.As(err, &emitErr) || emitErr.Reason != protocol.ReasonTokenInvalid {
		t.Fatalf("旧凭证必须 token_invalid，且不得靠已存在 id 回放，得到 %v", err)
	}
	if got := f.childCount(t); got != 1 {
		t.Fatalf("失效授权不得派发，子任务数 = %d", got)
	}
}

func TestEmitOnFinishedTaskIsNotRunning(t *testing.T) {
	f := newEmitFixture(t)
	// 任务已经终结（I8）：迟到的 emit 一律拒绝，不补发、不重投。
	if _, err := f.db.Exec(`UPDATE tasks SET phase = 'succeeded', finished_at = ? WHERE id = ?`,
		store.Now(), f.parent.ID); err != nil {
		t.Fatalf("置终态: %v", err)
	}
	_, err := f.router.Emit(testCtx(), EmitInput{Auth: f.auth, To: "consumer", Data: json.RawMessage(`{"file":"a.go"}`)})
	var emitErr *EmitError
	if !errors.As(err, &emitErr) || emitErr.Reason != protocol.ReasonTaskNotRunning {
		t.Fatalf("已结束任务的 emit 必须 task_not_running，得到 %v", err)
	}
	if got := f.childCount(t); got != 0 {
		t.Fatalf("已结束任务不得派发，子任务数 = %d", got)
	}
}

func TestEmitUnknownTaskIsTokenInvalid(t *testing.T) {
	f := newEmitFixture(t)
	auth := f.auth
	auth.TaskID = "tsk_missing"
	_, err := f.router.Emit(testCtx(), EmitInput{Auth: auth, To: "consumer", Data: json.RawMessage(`{"file":"a.go"}`)})
	var emitErr *EmitError
	if !errors.As(err, &emitErr) || emitErr.Reason != protocol.ReasonTokenInvalid {
		t.Fatalf("不存在的任务必须 token_invalid（不泄漏存在性），得到 %v", err)
	}
}
