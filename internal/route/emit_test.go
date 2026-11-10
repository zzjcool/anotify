package route

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
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

// missingEmitFixture 建一个 next=[...] 的 running 父任务，用来打 downstream_missing 回执。
func missingEmitFixture(t *testing.T, traceID string, next ...string) (*store.DB, *store.Namespace, *store.Task, store.EmitAuth, *Router) {
	t.Helper()
	db := testDB(t)
	ns := testNamespace(t, db)
	producer := testCapability(t, db, ns.ID, "producer", 1, capOpts{Next: next})
	node, token := testNode(t, db)
	tokenHash := store.HashNodeToken(token)
	parent, leaseID, attemptToken := testRunningTask(t, db, ns.ID, traceID, producer, node, tokenHash)
	auth := store.EmitAuth{
		TaskID: parent.ID, NodeID: node.ID, NodeTokenHash: tokenHash,
		Attempt: 1, LeaseID: leaseID, AttemptToken: attemptToken, Now: store.Now(),
	}
	return db, ns, parent, auth, New(db)
}

// failedTasksInTrace 列 trace 里的未解析失败任务（capability_id IS NULL）。
func failedTasksInTrace(t *testing.T, db *store.DB, namespaceID, traceID string) []*store.Task {
	t.Helper()
	tasks, err := db.ListTasksByTrace(testCtx(), namespaceID, traceID)
	if err != nil {
		t.Fatalf("列 trace 任务: %v", err)
	}
	out := make([]*store.Task, 0, 1)
	for _, task := range tasks {
		if task.CapabilityID == "" {
			out = append(out, task)
		}
	}
	return out
}

// downstreamMissingEvents 数 trace 里的 downstream_missing 观察事件。
func downstreamMissingEvents(t *testing.T, db *store.DB, namespaceID, traceID string) []*store.Event {
	t.Helper()
	events, err := db.ListEvents(testCtx(), namespaceID, traceID, "", 50)
	if err != nil {
		t.Fatalf("列事件: %v", err)
	}
	out := make([]*store.Event, 0, 1)
	for _, ev := range events {
		if ev.Type == protocol.ReasonDownstreamMissing {
			out = append(out, ev)
		}
	}
	return out
}

// TestEmitMissingDeclaredTargetCreatesFailedTaskNotPrune 钉住 E1-D2：已声明但解析不到的
// 目标落 failed Task（attempt 0）+ 错误事件，回执带**这个真实失败 Task 的 id**
// （anode CLI 的成功判据就是 taskId 非 null），不是剪枝、也不是 null。
func TestEmitMissingDeclaredTargetCreatesFailedTaskNotPrune(t *testing.T) {
	db, ns, parent, auth, router := missingEmitFixture(t, "trace-missing", "ghost@2")
	result, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "ghost@2", EmitID: "missing-1"})
	if err != nil {
		t.Fatalf("已声明但解析不到应落失败事实，不是 emit 错误: %v", err)
	}
	if result.Response.Pruned != nil {
		t.Fatalf("missing 回执不得伪装剪枝: %+v", result.Response)
	}
	if result.Response.TaskID == nil || *result.Response.TaskID == "" {
		t.Fatalf("missing 回执必须带真实失败 Task id（E1-D2），得到 %+v", result.Response)
	}
	taskID := *result.Response.TaskID
	if !strings.HasPrefix(taskID, "tsk_") {
		t.Fatalf("回执 taskId 不是真实任务 id: %q", taskID)
	}
	// 回执保真实请求 ref 与明确声明的 version（不改其它状态的 To 语义）。
	if result.Response.To != "ghost@2" || result.Response.Version != 2 {
		t.Fatalf("回执必须保真实请求 ref 与明确声明的 version: %+v", result.Response)
	}
	if result.Response.EventID == "" {
		t.Fatal("回执的 eventId 必须是真实持久事实 id")
	}
	if got := fchildCount(t, db, parent.ID); got != 0 {
		t.Fatalf("missing 不得建正常子任务，child=%d", got)
	}

	// 回执 id 必须在同 namespace 里读得到，且是真实的 failed Task（不是假 id）。
	failure, err := db.GetTask(testCtx(), ns.ID, taskID)
	if err != nil {
		t.Fatalf("回执 taskId 在 store 里读不到（假身份）: %v", err)
	}
	if failure.Phase != store.TaskFailed || failure.Attempt != 0 ||
		failure.Capability != "ghost" || failure.Version != 2 ||
		failure.Digest != "" || failure.Error != protocol.ReasonDownstreamMissing ||
		failure.ParentTaskID == nil || *failure.ParentTaskID != parent.ID {
		t.Fatalf("failed Task 事实不对（capability 存请求 basename、version 单独保）: %+v", failure)
	}
	// 没有执行事实：不编造 Node/slot/lease/过程时间/结果。
	if failure.NodeID != nil || failure.LeaseID != "" || failure.StartedAt != nil ||
		failure.ExitCode != nil || len(failure.ResultJSON) != 0 {
		t.Fatalf("failed Task 编造了执行事实: %+v", failure)
	}

	// 错误事件与同一个 TaskID 绑定（同事务、不后查猜）。
	events := downstreamMissingEvents(t, db, ns.ID, parent.TraceID)
	if len(events) != 1 {
		t.Fatalf("downstream_missing 错误事件应恰好一条，得到 %d", len(events))
	}
	var payload struct {
		Capability string `json:"capability"`
		Version    int    `json:"version"`
		TaskID     string `json:"taskId"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("解析事件 payload: %v", err)
	}
	if payload.TaskID != taskID || payload.Capability != "ghost" || payload.Version != 2 {
		t.Fatalf("错误事件没有绑定同一个真实 TaskID: %+v (回执 %s)", payload, taskID)
	}
	if n := len(failedTasksInTrace(t, db, ns.ID, parent.TraceID)); n != 1 {
		t.Fatalf("trace 里 failed Task 数 = %d，want 1", n)
	}
}

// TestEmitMissingReplayReturnsSameRealReceipt 钉住 E1-D2：同 emit id 回放回**同一个**
// 真实失败 Task 的回执（存储字节原样），不再新建 task/事件。
func TestEmitMissingReplayReturnsSameRealReceipt(t *testing.T) {
	db, ns, parent, auth, router := missingEmitFixture(t, "trace-missing-replay", "ghost@2")
	first, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "ghost@2", EmitID: "missing-replay"})
	if err != nil || first.Response.TaskID == nil {
		t.Fatalf("首次 missing emit: %v / %+v", err, first.Response)
	}
	storedFirst, err := db.LookupEmit(testCtx(), parent.ID, "missing-replay")
	if err != nil {
		t.Fatalf("读首次 emit 事实: %v", err)
	}

	replay, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "ghost@2", EmitID: "missing-replay"})
	if err != nil {
		t.Fatalf("同 id 回放: %v", err)
	}
	if !replay.Duplicate {
		t.Fatalf("同 id 同 body 应回放，不是新派发: %+v", replay)
	}
	if replay.Response.TaskID == nil || *replay.Response.TaskID != *first.Response.TaskID {
		t.Fatalf("回放必须回同一个真实 taskId: %+v vs %+v", replay.Response, first.Response)
	}
	if replay.Response.EventID != first.Response.EventID {
		t.Fatalf("回放必须回同一个 eventId: %+v vs %+v", replay.Response, first.Response)
	}
	storedReplay, err := db.LookupEmit(testCtx(), parent.ID, "missing-replay")
	if err != nil {
		t.Fatalf("回放后读 emit 事实: %v", err)
	}
	if !bytes.Equal(storedFirst.Result, storedReplay.Result) {
		t.Fatalf("回放改写了已存回执字节: %s != %s", storedFirst.Result, storedReplay.Result)
	}
	if n := len(failedTasksInTrace(t, db, ns.ID, parent.TraceID)); n != 1 {
		t.Fatalf("回放新建了失败 Task: %d", n)
	}
	if n := len(downstreamMissingEvents(t, db, ns.ID, parent.TraceID)); n != 1 {
		t.Fatalf("回放新建了错误事件: %d", n)
	}
}

// TestEmitMissingWithoutIDCreatesNewFailedTaskEachTime 钉住「无 id 每次都是新派发」：
// 两次无 id 的 missing emit 必须各建一条真实失败 Task。
func TestEmitMissingWithoutIDCreatesNewFailedTaskEachTime(t *testing.T) {
	db, ns, parent, auth, router := missingEmitFixture(t, "trace-missing-noid", "ghost@2")
	first, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "ghost@2"})
	if err != nil || first.Response.TaskID == nil {
		t.Fatalf("首次无 id missing emit: %v / %+v", err, first.Response)
	}
	second, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "ghost@2"})
	if err != nil || second.Response.TaskID == nil {
		t.Fatalf("第二次无 id missing emit: %v / %+v", err, second.Response)
	}
	if *first.Response.TaskID == *second.Response.TaskID {
		t.Fatalf("无 id 的两次 emit 复用了同一个 TaskID: %s", *first.Response.TaskID)
	}
	ids := map[string]bool{}
	for _, task := range failedTasksInTrace(t, db, ns.ID, parent.TraceID) {
		ids[task.ID] = true
	}
	if len(ids) != 2 || !ids[*first.Response.TaskID] || !ids[*second.Response.TaskID] {
		t.Fatalf("trace failed Task = %v，回执 id = %s/%s", ids, *first.Response.TaskID, *second.Response.TaskID)
	}
}

// TestEmitStrictPinnedRefRejectsNonCanonicalVersion 钉住 E1-D2：`@version` 只接受
// **规范十进制正整数**；`@2junk` / `@+2` / `@ 2` / `@02` / `@` / `@0` 都不是精确引用
// （旧代码用 Sscanf 前缀解析，会把它们当成 @2 放行）。
func TestEmitStrictPinnedRefRejectsNonCanonicalVersion(t *testing.T) {
	db, ns, parent, auth, router := missingEmitFixture(t, "trace-missing-ref", "ghost@2")
	beforeTasks, _ := db.ListTasksByTrace(testCtx(), ns.ID, parent.TraceID)
	beforeEvents, _ := db.ListEvents(testCtx(), ns.ID, parent.TraceID, "", 50)

	for _, to := range []string{"ghost@2junk", "ghost@+2", "ghost@ 2", "ghost@02", "ghost@", "ghost@0"} {
		_, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: to, Data: json.RawMessage(`{}`)})
		var emitErr *EmitError
		if !errors.As(err, &emitErr) || emitErr.Reason != protocol.ReasonTargetNotDeclared {
			t.Fatalf("to=%q 必须 target_not_declared（坏版本不是精确引用），得到 %v", to, err)
		}
		if emitErr.HTTPStatus() != 400 {
			t.Fatalf("to=%q 的 HTTP 状态 = %d，want 400", to, emitErr.HTTPStatus())
		}
	}
	// 坏引用是纯拒绝：不建任务、不写事件。
	afterTasks, _ := db.ListTasksByTrace(testCtx(), ns.ID, parent.TraceID)
	afterEvents, _ := db.ListEvents(testCtx(), ns.ID, parent.TraceID, "", 50)
	if len(afterTasks) != len(beforeTasks) || len(afterEvents) != len(beforeEvents) {
		t.Fatalf("坏引用产生了写: tasks %d→%d events %d→%d",
			len(beforeTasks), len(afterTasks), len(beforeEvents), len(afterEvents))
	}
	// 精确写法仍放行（回到真实失败 Task 回执）。
	ok, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "ghost@2", Data: json.RawMessage(`{}`)})
	if err != nil || ok.Response.TaskID == nil {
		t.Fatalf("精确 @2 引用应放行为真实失败 Task: %v / %+v", err, ok.Response)
	}
}

// TestSplitRefStrictPinnedVersions 直接钉住 strict ref 解析（emit 与 source 共用）。
func TestSplitRefStrictPinnedVersions(t *testing.T) {
	cases := map[string]struct {
		name   string
		ver    int
		pinned bool
	}{
		"ghost":       {"ghost", 0, false},
		"ghost@2":     {"ghost", 2, true},
		"ghost@12":    {"ghost", 12, true},
		"ghost@0":     {"ghost", -1, true},
		"ghost@":      {"ghost", -1, true},
		"ghost@2junk": {"ghost", -1, true},
		"ghost@+2":    {"ghost", -1, true},
		"ghost@ 2":    {"ghost", -1, true},
		"ghost@02":    {"ghost", -1, true},
		"ghost@2@3":   {"ghost", -1, true},
	}
	for ref, want := range cases {
		name, ver, pinned := splitRef(ref)
		if name != want.name || ver != want.ver || pinned != want.pinned {
			t.Fatalf("splitRef(%q) = (%q,%d,%v)，want (%q,%d,%v)",
				ref, name, ver, pinned, want.name, want.ver, want.pinned)
		}
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

// TestEmitMissingUndeclaredVersionOmitsResponseVersion 钉住 E1-D2 的 wire presence：
// 裸 ref（未声明版本、也没解析出能力行）没有版本事实 → 回执不公开哨兵 0（字段省略），
// 但 taskId 仍是真实失败 Task（已建任务），失败 Task 的 version 就是 0。
func TestEmitMissingUndeclaredVersionOmitsResponseVersion(t *testing.T) {
	db, ns, parent, auth, router := missingEmitFixture(t, "trace-missing-bare", "ghost")
	result, err := router.Emit(testCtx(), EmitInput{Auth: auth, To: "ghost", EmitID: "missing-bare"})
	if err != nil {
		t.Fatalf("裸 ref 的 missing emit: %v", err)
	}
	if result.Response.TaskID == nil {
		t.Fatalf("裸 ref missing 回执也要带真实失败 Task id: %+v", result.Response)
	}
	if result.Response.Version != 0 || result.Response.To != "ghost" || result.Response.Pruned != nil {
		t.Fatalf("裸 ref 回执事实不对: %+v", result.Response)
	}
	stored, err := db.LookupEmit(testCtx(), parent.ID, "missing-bare")
	if err != nil {
		t.Fatalf("读 emit 事实: %v", err)
	}
	if bytes.Contains(stored.Result, []byte(`"version"`)) {
		t.Fatalf("未声明的版本不得公开哨兵 0: %s", stored.Result)
	}
	if !bytes.Contains(stored.Result, []byte(`"taskId":"`+*result.Response.TaskID+`"`)) {
		t.Fatalf("已存回执必须是真实 taskId: %s", stored.Result)
	}
	task, err := db.GetTask(testCtx(), ns.ID, *result.Response.TaskID)
	if err != nil {
		t.Fatalf("读失败 Task: %v", err)
	}
	if task.Version != 0 || task.Capability != "ghost" || task.Phase != store.TaskFailed {
		t.Fatalf("失败 Task 的未声明版本事实 = %+v", task)
	}
}
