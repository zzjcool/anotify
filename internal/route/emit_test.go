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

// TestEmitPinnedDeclaredRefPinsChildAndTracksMutableNext 钉住 R1-C6 的“声明即任务钉版”：
// 裸 to 命中父 next 的唯一钉版声明 → 子任务 version/digest 钉在该版本；父 next 同版本
// 可变配置之后，后续 emit 按真实当前声明走，已建子任务不变。
func TestEmitPinnedDeclaredRefPinsChildAndTracksMutableNext(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	producer := testCapability(t, db, ns.ID, "producer", 1, capOpts{Next: []string{"consumer@2"}})
	_ = testCapability(t, db, ns.ID, "consumer", 1, capOpts{})
	consumerV2 := testCapability(t, db, ns.ID, "consumer", 2, capOpts{})
	node, token := testNode(t, db)
	tokenHash := store.HashNodeToken(token)
	parent, leaseID, attemptToken := testRunningTask(t, db, ns.ID, "trace-pin", producer, node, tokenHash)
	router := New(db)
	auth := store.EmitAuth{
		TaskID: parent.ID, NodeID: node.ID, NodeTokenHash: tokenHash,
		Attempt: 1, LeaseID: leaseID, AttemptToken: attemptToken, Now: store.Now(),
	}

	first, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "consumer"})
	if err != nil {
		t.Fatalf("裸 to 命中钉版声明应可派发: %v", err)
	}
	if first.Response.TaskID == nil {
		t.Fatalf("应建子任务: %+v", first.Response)
	}
	child, err := db.GetTask(testCtx(), ns.ID, *first.Response.TaskID)
	if err != nil {
		t.Fatalf("读子任务: %v", err)
	}
	if child.Version != 2 || child.Digest != consumerV2.Digest {
		t.Fatalf("子任务必须钉住声明版本/digest（v2）: version=%d digest=%q want %q",
			child.Version, child.Digest, consumerV2.Digest)
	}

	// 父 next 同 version 改配置：后续 emit 按真实当前声明（v3）走。
	consumerV3 := testCapability(t, db, ns.ID, "consumer", 3, capOpts{})
	if _, err := db.UpdateNext(testCtx(), ns.ID, "producer", 1,
		[]string{"consumer@3"}, store.ResourceVersion(producer.ID, producer.ResourceVersion)); err != nil {
		t.Fatalf("改父 next: %v", err)
	}
	second, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "consumer"})
	if err != nil {
		t.Fatalf("改 next 后 emit: %v", err)
	}
	child2, err := db.GetTask(testCtx(), ns.ID, *second.Response.TaskID)
	if err != nil {
		t.Fatalf("读第二子任务: %v", err)
	}
	if child2.Version != 3 || child2.Digest != consumerV3.Digest {
		t.Fatalf("后续 emit 必须按当前声明 v3: version=%d digest=%q want %q",
			child2.Version, child2.Digest, consumerV3.Digest)
	}
	// 已建子任务不被改 next 漂移。
	again, err := db.GetTask(testCtx(), ns.ID, child.ID)
	if err != nil {
		t.Fatalf("回读第一子任务: %v", err)
	}
	if again.Version != 2 || again.Digest != consumerV2.Digest {
		t.Fatalf("已建子任务被改 next 漂移: %+v", again)
	}
}

// TestEmitMissingDeclaredTargetCreatesFailedTaskNotPrune 钉住 R1-C3：已声明但解析不到
// 的目标落 failed Task（attempt 0）+ 错误事件，回执不是剪枝（Pruned 为空）。
func TestEmitMissingDeclaredTargetCreatesFailedTaskNotPrune(t *testing.T) {
	db := testDB(t)
	ns := testNamespace(t, db)
	producer := testCapability(t, db, ns.ID, "producer", 1, capOpts{Next: []string{"ghost@2"}})
	node, token := testNode(t, db)
	tokenHash := store.HashNodeToken(token)
	parent, leaseID, attemptToken := testRunningTask(t, db, ns.ID, "trace-missing", producer, node, tokenHash)
	router := New(db)
	auth := store.EmitAuth{
		TaskID: parent.ID, NodeID: node.ID, NodeTokenHash: tokenHash,
		Attempt: 1, LeaseID: leaseID, AttemptToken: attemptToken, Now: store.Now(),
	}
	result, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "ghost@2", EmitID: "missing-1"})
	if err != nil {
		t.Fatalf("已声明但解析不到应落失败事实，不是 emit 错误: %v", err)
	}
	// 冻结 emit wire（protocol.EmitResponse）没有 failed-task 字段，store 的
	// EmitFailureFact 也不返回创建的 failed task id：这里只能诚实回“没有正常子任务”，
	// 不伪造 taskId、不把失败伪装成剪枝（Pruned 必为空）；真实 failed Task 在 trace 可查。
	if result.Response.Pruned != nil {
		t.Fatalf("missing 回执不得伪装剪枝: %+v", result.Response)
	}
	if result.Response.TaskID != nil {
		t.Fatalf("missing 回执不得把 failed task 假冒成成功 taskId: %+v", result.Response)
	}
	if result.Response.To != "ghost@2" || result.Response.Version != 2 {
		t.Fatalf("回执必须保真实请求 ref 与明确声明的 version: %+v", result.Response)
	}
	if result.Response.EventID == "" {
		t.Fatal("回执的 eventId 必须是真实持久事实 id")
	}
	if got := fchildCount(t, db, parent.ID); got != 0 {
		t.Fatalf("missing 不得建正常子任务，child=%d", got)
	}
	tasks, err := db.ListTasksByTrace(testCtx(), ns.ID, parent.TraceID)
	if err != nil {
		t.Fatalf("列 trace 任务: %v", err)
	}
	var failure *store.Task
	for _, task := range tasks {
		if task.CapabilityID == "" {
			failure = task
		}
	}
	if failure == nil {
		t.Fatalf("没有 failed Task 落库: %+v", tasks)
	}
	if failure.Phase != store.TaskFailed || failure.Attempt != 0 || failure.Capability != "ghost@2" ||
		failure.Version != 2 || failure.Digest != "" || failure.Error != protocol.ReasonDownstreamMissing ||
		failure.ParentTaskID == nil || *failure.ParentTaskID != parent.ID {
		t.Fatalf("failed Task 事实不对: %+v", failure)
	}
	// 错误事件（真实请求 ref + taskId）必须存在；不是剪枝回执。
	events, err := db.ListEvents(testCtx(), ns.ID, parent.TraceID, "", 50)
	if err != nil {
		t.Fatalf("列事件: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == protocol.ReasonDownstreamMissing {
			found = true
		}
	}
	if !found {
		t.Fatalf("downstream_missing 错误事件缺失: %+v", events)
	}
}

// TestEmitPruneReceiptEventIDIsPersistedFact 钉住剪枝回执的 eventId 来自真实持久事实
// （emits 接受行），不是编造的 id；同 id 回放返回同一份回执。
func TestEmitPruneReceiptEventIDIsPersistedFact(t *testing.T) {
	f := newEmitFixture(t)
	first, err := f.router.Emit(testCtx(), EmitInput{
		Auth: f.auth, To: "consumer", EmitID: "prune-once", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("缺参应按剪枝回执: %v", err)
	}
	if first.Response.EventID == "" || first.Response.Pruned == nil {
		t.Fatalf("剪枝回执缺 eventId/pruned: %+v", first.Response)
	}
	record, err := f.db.LookupEmit(testCtx(), f.parent.ID, "prune-once")
	if err != nil {
		t.Fatalf("读 emit 接受事实: %v", err)
	}
	if record.ID != first.Response.EventID {
		t.Fatalf("回执 eventId=%q 不等于真实接受事实 id=%q", first.Response.EventID, record.ID)
	}
	replay, err := f.router.Emit(testCtx(), EmitInput{
		Auth: f.auth, To: "consumer", EmitID: "prune-once", Data: json.RawMessage(`{}`),
	})
	if err != nil || !replay.Duplicate || replay.Response.EventID != first.Response.EventID {
		t.Fatalf("同 id 剪枝回放必须返回同一份回执: %v/%+v", err, replay)
	}
}

// TestEmitRevokedCredentialWritesNoPruneFact 是 G2-C4 的回归：旧代码在
// EmitAuthorized 之前裸写剪枝事件；现在授权先于任何计划事实写。
func TestEmitRevokedCredentialWritesNoPruneFact(t *testing.T) {
	f := newEmitFixture(t)
	beforeEvents := countTraceEvents(t, f.db, f.parent.TraceID)
	if err := f.db.SetNodeTokenHash(testCtx(), f.node.ID, store.HashNodeToken("rotated-token")); err != nil {
		t.Fatalf("轮换令牌: %v", err)
	}
	_, err := f.router.Emit(testCtx(), EmitInput{Auth: f.auth, To: "consumer", Data: json.RawMessage(`{}`)})
	var emitErr *EmitError
	if !errors.As(err, &emitErr) || emitErr.Reason != protocol.ReasonTokenInvalid {
		t.Fatalf("旧凭证必须 token_invalid，得到 %v", err)
	}
	if got := countTraceEvents(t, f.db, f.parent.TraceID); got != beforeEvents {
		t.Fatalf("失效授权不得写剪枝事件：%d → %d", beforeEvents, got)
	}
	if got := f.childCount(t); got != 0 {
		t.Fatalf("失效授权不得建任务：%d", got)
	}
}

// TestEmitPruneFailureIsNotSwallowed 钉住「剪枝留痕失败必须报错、不同事务假成功」。
func TestEmitPruneFailureIsNotSwallowed(t *testing.T) {
	f := newEmitFixture(t)
	if _, err := f.db.Exec(`CREATE TRIGGER g2r1_emit_prune_fail BEFORE INSERT ON events
		WHEN NEW.source = 'route' AND NEW.type = 'param_missing'
		BEGIN SELECT RAISE(ABORT, 'injected prune write failure'); END`); err != nil {
		t.Fatalf("注入剪枝写失败: %v", err)
	}
	_, err := f.router.Emit(testCtx(), EmitInput{
		Auth: f.auth, To: "consumer", EmitID: "prune-fail", Data: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("剪枝留痕写失败被吞成了成功回执")
	}
	if got := f.childCount(t); got != 0 {
		t.Fatalf("失败事务不得留下任务：%d", got)
	}
	if _, lerr := f.db.LookupEmit(testCtx(), f.parent.ID, "prune-fail"); !errors.Is(lerr, store.ErrNotFound) {
		t.Fatalf("失败事务不得留下 emit 行: %v", lerr)
	}
}

// fchildCount 数某父任务下的**正常**子任务数（capability_id 非 NULL）。
// 未解析失败子任务（capability_id IS NULL）不是正常派发，单独用 trace 任务断言。
func fchildCount(t *testing.T, db *store.DB, parentID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent_task_id = ? AND capability_id IS NOT NULL`, parentID).Scan(&n); err != nil {
		t.Fatalf("统计子任务: %v", err)
	}
	return n
}

// countTraceEvents 数某 trace 的事件行（授权前不得有新增观察）。
func countTraceEvents(t *testing.T, db *store.DB, traceID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE trace_id = ?`, traceID).Scan(&n); err != nil {
		t.Fatalf("统计事件: %v", err)
	}
	return n
}
