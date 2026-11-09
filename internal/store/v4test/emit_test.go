package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// emitSetup 造一条「在跑」的父任务 + 它的权威授权事实（grant 拿到 attempt/lease/token）。
//
// 每条用例用不同的 name 前缀：节点名在集群内唯一，同一个测试库里建多台要有区分。
func emitSetup(t *testing.T, db *store.DB, nsID, name string) (*store.Task, store.EmitAuth) {
	t.Helper()
	c := createCap(t, db, testCap(nsID, "parent", 1, []string{"child"}))
	node := newNode(t, db, name+"-node", 2)
	task := newTask(t, db, nsID, c, "trace-"+name, "")
	leaseID, token, attempt := grant(t, db, task.ID, node.ID, "asg-"+name)
	return task, store.EmitAuth{
		TaskID:        task.ID,
		NodeID:        node.ID,
		NodeTokenHash: node.TokenHash,
		Attempt:       attempt,
		LeaseID:       leaseID,
		AttemptToken:  token,
		Now:           store.Now(),
	}
}

// childTasks 造 n 条待建的下游任务（同一次 emit 扇出的结果）。
func childTasks(nsID, traceID string, c *store.CapabilityVersion, n int) []*store.Task {
	out := make([]*store.Task, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &store.Task{
			NamespaceID:  nsID,
			TraceID:      traceID,
			CapabilityID: c.ID,
			Capability:   c.Name,
			Version:      c.Version,
			Digest:       c.Digest,
		})
	}
	return out
}

// TestEmitIdempotentReplay 验同 emit id 同请求 → 回旧结果、不重复建行。
func TestEmitIdempotentReplay(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-replay")
	parent, auth := emitSetup(t, db, ns, "replay")
	child := createCap(t, db, testCap(ns, "child", 1, nil))
	emitID := "emit-1"

	rec := &store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{"n":1}`)}
	result, first, err := db.EmitAuthorized(ctx, auth, rec, childTasks(ns, parent.TraceID, child, 2))
	if err != nil {
		t.Fatalf("first emit: %v", err)
	}
	if !first {
		t.Fatalf("首次 emit 应 first=true")
	}
	var payload struct {
		TaskIDs []string `json:"taskIds"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(payload.TaskIDs) != 2 {
		t.Fatalf("result 应带 2 个 taskIds，得到 %v", payload.TaskIDs)
	}

	// 重发（同一个 id、同 to、同 data，键序不同）→ 回旧结果、不建任何行。
	rec2 := &store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{ "n" : 1 }`)}
	result2, first2, err := db.EmitAuthorized(ctx, auth, rec2, childTasks(ns, parent.TraceID, child, 2))
	if err != nil {
		t.Fatalf("replay emit: %v", err)
	}
	if first2 {
		t.Fatalf("重发应 first=false")
	}
	if string(result2) != string(result) {
		t.Fatalf("重发应回同一份 result：%s vs %s", result2, result)
	}
	tasks, err := db.ListTasksByTrace(ctx, ns, parent.TraceID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 3 { // 父 + 两个孩子，重发没有多建
		t.Fatalf("重发不该建新任务，trace 里共有 %d 行", len(tasks))
	}
}

// TestEmitIDConflict 验同 id 异 to / 异 data → 409 emit_id_conflict。
func TestEmitIDConflict(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-conflict")
	parent, auth := emitSetup(t, db, ns, "conflict")
	child := createCap(t, db, testCap(ns, "child", 1, nil))
	emitID := "emit-x"

	if _, _, err := db.EmitAuthorized(ctx, auth,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{"n":1}`)},
		childTasks(ns, parent.TraceID, child, 1)); err != nil {
		t.Fatalf("first emit: %v", err)
	}
	if _, _, err := db.EmitAuthorized(ctx, auth,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "other", Data: json.RawMessage(`{"n":1}`)},
		childTasks(ns, parent.TraceID, child, 1)); !errors.Is(err, store.ErrEmitIDConflict) {
		t.Fatalf("异 to 应 ErrEmitIDConflict，得到 %v", err)
	}
	if _, _, err := db.EmitAuthorized(ctx, auth,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{"n":2}`)},
		childTasks(ns, parent.TraceID, child, 1)); !errors.Is(err, store.ErrEmitIDConflict) {
		t.Fatalf("异 data 应 ErrEmitIDConflict，得到 %v", err)
	}
	rows, err := db.ListEmits(ctx, parent.ID)
	if err != nil {
		t.Fatalf("list emits: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("冲突不该写行，emit 行数 = %d", len(rows))
	}
}

// TestEmitWithoutIDAlwaysNew 验无 emit id 每次都是新行（不按调用序号/requestId 去重）。
func TestEmitWithoutIDAlwaysNew(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-noid")
	parent, auth := emitSetup(t, db, ns, "noid")
	child := createCap(t, db, testCap(ns, "child", 1, nil))

	for i := 0; i < 2; i++ {
		_, first, err := db.EmitAuthorized(ctx, auth,
			&store.EmitRecord{TaskID: parent.ID, ToRef: "child", Data: json.RawMessage(`{"n":1}`)},
			childTasks(ns, parent.TraceID, child, 1))
		if err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
		if !first {
			t.Fatalf("无 id 的 emit 每次都该 first=true（第 %d 次）", i)
		}
	}
	rows, err := db.ListEmits(ctx, parent.ID)
	if err != nil {
		t.Fatalf("list emits: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("无 id 应每次新行，得到 %d 行", len(rows))
	}
	if rows[0].EmitID != nil || rows[1].EmitID != nil {
		t.Fatalf("无 id 的行 emit_id 必须是 NULL")
	}
}

// TestEmitNotMergedAcrossTasks 验 emit 幂等域是**单个逻辑任务**（跨 task 不合并）。
func TestEmitNotMergedAcrossTasks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-cross")
	p1, auth1 := emitSetup(t, db, ns, "cross-a")
	p2, auth2 := emitSetup(t, db, ns, "cross-b")
	child := createCap(t, db, testCap(ns, "child", 1, nil))
	emitID := "same-id"

	for _, tc := range []struct {
		taskID string
		auth   store.EmitAuth
		trace  string
	}{{p1.ID, auth1, p1.TraceID}, {p2.ID, auth2, p2.TraceID}} {
		_, first, err := db.EmitAuthorized(ctx, tc.auth,
			&store.EmitRecord{TaskID: tc.taskID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)},
			childTasks(ns, tc.trace, child, 1))
		if err != nil {
			t.Fatalf("emit for %s: %v", tc.taskID, err)
		}
		if !first {
			t.Fatalf("跨 task 的同 id 不该合并（task %s）", tc.taskID)
		}
	}
	for _, taskID := range []string{p1.ID, p2.ID} {
		rec, err := db.LookupEmit(ctx, taskID, emitID)
		if err != nil {
			t.Fatalf("lookup emit %s: %v", taskID, err)
		}
		if rec.TaskID != taskID {
			t.Fatalf("emit 行归属错乱：%s != %s", rec.TaskID, taskID)
		}
	}
	if _, err := db.LookupEmit(ctx, p1.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("空 emit id lookup 应 ErrNotFound，得到 %v", err)
	}
}

// TestEmitCustomResultIsPinned 验调用方给的 result 被钉住（回放同一份）。
func TestEmitCustomResultIsPinned(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-custom")
	parent, auth := emitSetup(t, db, ns, "custom")
	emitID := "pruned-1"
	custom := json.RawMessage(`{"taskId":null,"pruned":{"reason":"param_missing","detail":"env"}}`)

	if _, _, err := db.EmitAuthorized(ctx, auth,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child",
			Data: json.RawMessage(`{}`), Result: custom}, nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	replay, first, err := db.EmitAuthorized(ctx, auth,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)}, nil)
	if err != nil || first {
		t.Fatalf("replay: err=%v first=%v", err, first)
	}
	mustJSON(t, replay, string(custom))
}

// TestEmitAuthorizedRejectsBadAuth 验授权参数逐项被核对（身份/节点/attempt/令牌/租约）。
func TestEmitAuthorizedRejectsBadAuth(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-auth")
	parent, auth := emitSetup(t, db, ns, "auth")
	child := createCap(t, db, testCap(ns, "child", 1, nil))
	rec := func(id string) *store.EmitRecord {
		return &store.EmitRecord{TaskID: parent.ID, EmitID: &id, ToRef: "child", Data: json.RawMessage(`{}`)}
	}
	cases := []struct {
		name string
		mut  func(a *store.EmitAuth)
		want error
	}{
		{"node token", func(a *store.EmitAuth) { a.NodeTokenHash = "deadbeef" }, store.ErrNodeIdentityMismatch},
		{"shadow node", func(a *store.EmitAuth) { a.NodeID = "nd_missing" }, store.ErrNodeIdentityMismatch},
		{"old attempt", func(a *store.EmitAuth) { a.Attempt++ }, store.ErrLeaseMismatch},
		{"wrong lease", func(a *store.EmitAuth) { a.LeaseID = "lse_bogus" }, store.ErrLeaseMismatch},
		{"wrong token", func(a *store.EmitAuth) { a.AttemptToken = "tok_bogus" }, store.ErrAttemptTokenInvalid},
		{"expired lease", func(a *store.EmitAuth) { a.Now = a.Now + 3600 }, store.ErrLeaseMismatch},
	}
	for i, tc := range cases {
		bad := auth
		tc.mut(&bad)
		if _, _, err := db.EmitAuthorized(ctx, bad, rec(fmt.Sprintf("auth-%d", i)),
			childTasks(ns, parent.TraceID, child, 1)); !errors.Is(err, tc.want) {
			t.Fatalf("%s: 期望 %v，得到 %v", tc.name, tc.want, err)
		}
	}
	// 任何一条被拒的 emit 都不该留下行。
	if rows, err := db.ListEmits(ctx, parent.ID); err != nil || len(rows) != 0 {
		t.Fatalf("被拒的 emit 不该写行：%v %v", rows, err)
	}
	// 授权字段缺失（调用方忘了带）也拒。
	for _, bad := range []store.EmitAuth{
		{TaskID: parent.ID, NodeID: auth.NodeID, NodeTokenHash: auth.NodeTokenHash,
			Attempt: auth.Attempt, LeaseID: auth.LeaseID},
		{TaskID: parent.ID, NodeID: auth.NodeID, Attempt: auth.Attempt,
			LeaseID: auth.LeaseID, AttemptToken: auth.AttemptToken},
		{TaskID: "tsk_other", NodeID: auth.NodeID, NodeTokenHash: auth.NodeTokenHash,
			Attempt: auth.Attempt, LeaseID: auth.LeaseID, AttemptToken: auth.AttemptToken},
	} {
		if _, _, err := db.EmitAuthorized(ctx, bad, rec("missing-fields"), nil); err == nil {
			t.Fatalf("授权参数不完整的 emit 应被拒：%+v", bad)
		}
	}
}

// TestEmitAuthorizedRejectsAfterCancel 验**取消在接受事务之前**发生 → emit 被拒。
func TestEmitAuthorizedRejectsAfterCancel(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-cancel")
	parent, auth := emitSetup(t, db, ns, "cancel")
	child := createCap(t, db, testCap(ns, "child", 1, nil))
	emitID := "after-cancel"

	if err := db.RequestCancel(ctx, parent.ID, "cancelled_by_user", store.Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	if _, _, err := db.EmitAuthorized(ctx, auth,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)},
		childTasks(ns, parent.TraceID, child, 1)); !errors.Is(err, store.ErrTaskNotRunning) {
		t.Fatalf("取消后 emit 必须被拒（ErrTaskNotRunning），得到 %v", err)
	}
	if rows, err := db.ListEmits(ctx, parent.ID); err != nil || len(rows) != 0 {
		t.Fatalf("被拒的 emit 不该写行：%v %v", rows, err)
	}
	if tasks, err := db.ListTasksByTrace(ctx, ns, parent.TraceID); err != nil || len(tasks) != 1 {
		t.Fatalf("被拒的 emit 不该建子任务：%v %d", err, len(tasks))
	}
}

// TestEmitAuthorizedOldTokenRejectedEvenWithExistingID 验**同 id 旧 token 也拒**：
//
// 一条已经失效的执行（被重派后的旧 attempt）不能靠「我上次 emit 过」拿到回执——
// 幂等不是绕过授权的后门。授权在幂等查找之前判。
func TestEmitAuthorizedOldTokenRejectedEvenWithExistingID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-oldtoken")
	parent, auth1 := emitSetup(t, db, ns, "oldtoken")
	child := createCap(t, db, testCap(ns, "child", 1, nil))
	emitID := "carry-over"

	// attempt 1 写下一条幂等记录。
	if _, first, err := db.EmitAuthorized(ctx, auth1,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)},
		childTasks(ns, parent.TraceID, child, 1)); err != nil || !first {
		t.Fatalf("attempt 1 emit: first=%v err=%v", first, err)
	}
	// 制造重派：租约过期 → 回 pending → 再 grant（幂等能力才能重排，父任务默认非幂等，
	// 所以直接改库模拟一条已被重派的任务）。为了让 RecoverLostLeases 重排，先把
	// 能力标成幂等。
	if _, err := db.Exec(`UPDATE tasks SET idempotent = 1, lease_until = ? WHERE id = ?`,
		store.Now()-1, parent.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, _, err := db.RecoverLostLeases(ctx, store.Now()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := mustPhase(t, db, ns, parent.ID, store.TaskPending); got.Attempt != 1 {
		t.Fatalf("重排应保留 attempt=1，得到 %d", got.Attempt)
	}
	if _, err := db.Exec(`UPDATE tasks SET next_attempt_at = NULL WHERE id = ?`, parent.ID); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	leaseID2, token2, attempt2 := grant(t, db, parent.ID, auth1.NodeID, "asg-2")
	auth2 := auth1
	auth2.Attempt, auth2.LeaseID, auth2.AttemptToken = attempt2, leaseID2, token2
	auth2.Now = store.Now()

	// 旧 attempt 的授权 + 已存在的同 id → 拒（不能靠幂等拿回执）。
	if _, _, err := db.EmitAuthorized(ctx, auth1,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)},
		nil); !errors.Is(err, store.ErrLeaseMismatch) {
		t.Fatalf("旧 attempt 的同 id emit 必须被拒（ErrLeaseMismatch），得到 %v", err)
	}
	// 当前有效 attempt + 同一个 id → 回旧回执（跨 attempt 的幂等域是任务）。
	replay, first, err := db.EmitAuthorized(ctx, auth2,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)},
		nil)
	if err != nil {
		t.Fatalf("当前 attempt 的同 id emit: %v", err)
	}
	if first {
		t.Fatalf("跨 attempt 的同 id 应回旧回执（first=false）")
	}
	mustJSON(t, replay, `{"taskIds":["`+childTaskID(t, db, ns, parent.TraceID, "child")+`"]}`)
}

// childTaskID 取某 trace 上某能力的第一个子任务 id（回执断言用）。
func childTaskID(t *testing.T, db *store.DB, nsID, traceID, capability string) string {
	t.Helper()
	tasks, err := db.ListTasksByTrace(context.Background(), nsID, traceID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	for _, task := range tasks {
		if task.Capability == capability {
			return task.ID
		}
	}
	t.Fatalf("trace %s 上找不到能力 %s 的子任务", traceID, capability)
	return ""
}
