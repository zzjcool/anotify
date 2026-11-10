package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// emitSetup 造一条「在跑」的父任务 + 它的权威授权事实（grant 拿到 attempt/lease/token）。
//
// 每条用例用不同的 name 前缀：节点名在集群内唯一，同一个测试库里建多台要有区分。
func emitSetup(t *testing.T, db *store.DB, nsID, name string) (*store.Task, store.EmitAuth) {
	t.Helper()
	// 该夹具会在旧 attempt 未回收时重派同一能力：显式给 2（默认 1 的反例见
	// execution_auth_test.go 的 pinned maxPerNode 用例）。
	parentSpec := testCap(nsID, "parent", 1, []string{"child"})
	parentPerNode := 2
	parentSpec.Spec.Concurrency.MaxPerNode = &parentPerNode
	c := createCap(t, db, parentSpec)
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

// TestEmitAuthorizedRejectsNamespaceDenied 验命名空间授权拒绝（E1 的 ns 交集判定）。
//
// 节点身份是真的、任务在跑、令牌也对——只是这台机器没被授权碰任务所属的 ns。
// 这种拒绝必须是 ErrEmitNamespaceDenied，且什么都不写。
func TestEmitAuthorizedRejectsNamespaceDenied(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-nsdenied")
	parent, auth := emitSetup(t, db, ns, "nsdenied")
	child := createCap(t, db, testCap(ns, "child", 1, nil))

	// 授权列表收窄到别的 ns（本机申报 ["*"]，交集 = ["other"]）。
	cfg, cerr := db.GetNodeConfig(ctx, auth.NodeID)
	if cerr != nil {
		t.Fatalf("get node config: %v", cerr)
	}
	cfg.Namespaces = []string{"other"}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("put node config: %v", err)
	}
	emitID := "ns-denied"
	_, _, err := db.EmitAuthorized(ctx, auth,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)},
		childTasks(ns, parent.TraceID, child, 1))
	if !errors.Is(err, store.ErrEmitNamespaceDenied) {
		t.Fatalf("未授权的 ns 应 ErrEmitNamespaceDenied，得到 %v", err)
	}
	if rows, err := db.ListEmits(ctx, parent.ID); err != nil || len(rows) != 0 {
		t.Fatalf("被拒的 emit 不该写行：%v %v", rows, err)
	}
	if tasks, err := db.ListTasksByTrace(ctx, ns, parent.TraceID); err != nil || len(tasks) != 1 {
		t.Fatalf("被拒的 emit 不该建子任务：%v %d", err, len(tasks))
	}
}

// TestEmitConcurrentSameIDOnlyOnce 验并发同 id 只产生**一条** emission 与**一组**子任务。
//
// 授权与写入在同一个事务里，唯一索引 + 回读兜底保证并发请求合并成一次派发；
// 两个并发请求都拿到同一份回执，其中恰好一个 first=true。
func TestEmitConcurrentSameIDOnlyOnce(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-concurrent")
	parent, auth := emitSetup(t, db, ns, "concurrent")
	child := createCap(t, db, testCap(ns, "child", 1, nil))
	emitID := "same-id"

	const n = 6
	type outcome struct {
		first bool
		err   error
	}
	results := make(chan outcome, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			rec := &store.EmitRecord{
				TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{"n":1}`),
			}
			_, first, err := db.EmitAuthorized(ctx, auth, rec, childTasks(ns, parent.TraceID, child, 1))
			results <- outcome{first: first, err: err}
		}()
	}
	close(start)

	firstCount := 0
	for i := 0; i < n; i++ {
		out := <-results
		if out.err != nil {
			t.Fatalf("并发同 id emit 不该报错：%v", out.err)
		}
		if out.first {
			firstCount++
		}
	}
	if firstCount != 1 {
		t.Fatalf("并发同 id 应恰好一次 first=true，得到 %d", firstCount)
	}
	rows, err := db.ListEmits(ctx, parent.ID)
	if err != nil {
		t.Fatalf("list emits: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("并发同 id 应只有一条 emission，得到 %d", len(rows))
	}
	tasks, err := db.ListTasksByTrace(ctx, ns, parent.TraceID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("并发同 id 应只建一组子任务（1 个），trace 里共 %d 行", len(tasks))
	}
}

// TestEmitChildFailureRollsBackAll 验子任务写入失败 → emit 与整组子任务 0 写（同事务回滚）。
func TestEmitChildFailureRollsBackAll(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-rollback")
	parent, auth := emitSetup(t, db, ns, "rollback")
	child := createCap(t, db, testCap(ns, "child", 1, nil))
	emitID := "rollback-1"

	bad := &store.Task{NamespaceID: ns, TraceID: parent.TraceID} // 缺 capability → 校验失败
	batch := append(childTasks(ns, parent.TraceID, child, 2), bad)
	if _, _, err := db.EmitAuthorized(ctx, auth,
		&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)},
		batch); err == nil {
		t.Fatalf("含非法子任务的 emit 应失败")
	}
	if rows, err := db.ListEmits(ctx, parent.ID); err != nil || len(rows) != 0 {
		t.Fatalf("子任务失败必须回滚 emit 行：%v %v", rows, err)
	}
	if tasks, err := db.ListTasksByTrace(ctx, ns, parent.TraceID); err != nil || len(tasks) != 1 {
		t.Fatalf("子任务失败必须回滚整组子任务（0 写）：%v %d", err, len(tasks))
	}
}

// TestEmitRejectedForTerminalTask 验终态任务（succeeded/failed）的 emit 一律拒。
func TestEmitRejectedForTerminalTask(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-terminal")
	child := createCap(t, db, testCap(ns, "child", 1, nil))

	for i, phase := range []string{store.TaskSucceeded, store.TaskFailed} {
		parent, auth := emitSetup(t, db, ns, fmt.Sprintf("terminal-%d", i))
		// emitSetup 已经 grant 过（auth 里就是这次 attempt 的权威事实）。
		if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: auth.NodeID, NodeTokenHash: auth.NodeTokenHash}, parent.ID, auth.Attempt, auth.LeaseID, 0, store.TaskResult{
			Phase: phase, Reason: "script_failed", Error: "boom",
		}); err != nil {
			t.Fatalf("complete %s: %v", phase, err)
		}
		emitID := fmt.Sprintf("terminal-emit-%d", i)
		_, _, err := db.EmitAuthorized(ctx, auth,
			&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)},
			childTasks(ns, parent.TraceID, child, 1))
		if !errors.Is(err, store.ErrTaskNotRunning) {
			t.Fatalf("%s 任务的 emit 应 ErrTaskNotRunning，得到 %v", phase, err)
		}
		if rows, err := db.ListEmits(ctx, parent.ID); err != nil || len(rows) != 0 {
			t.Fatalf("%s 任务不该留下 emit 行：%v %v", phase, rows, err)
		}
	}
}

// TestEmitCancelRaceLinearizable 验 emit 与取消没有「校验后再写」的窗口：
//
// 每一次并发都只能有两种结局——取消先赢（emit 被拒、0 写）或 emit 先赢（一组子任务 +
// 取消意图随后落下）。绝不允许「emit 写了一半」或「取消后还有子任务冒出来」。
func TestEmitCancelRaceLinearizable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "emit-cancelrace")
	child := createCap(t, db, testCap(ns, "child", 1, nil))

	for i := 0; i < 4; i++ {
		parent, auth := emitSetup(t, db, ns, fmt.Sprintf("cancelrace-%d", i))
		emitID := fmt.Sprintf("race-emit-%d", i)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var (
			emitErr   error
			first     bool
			cancelErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, first, emitErr = db.EmitAuthorized(ctx, auth,
				&store.EmitRecord{TaskID: parent.ID, EmitID: &emitID, ToRef: "child", Data: json.RawMessage(`{}`)},
				childTasks(ns, parent.TraceID, child, 1))
		}()
		go func() {
			defer wg.Done()
			<-start
			cancelErr = db.RequestCancel(ctx, parent.ID, "cancelled_by_user", store.Now())
		}()
		close(start)
		wg.Wait()

		if cancelErr != nil {
			t.Fatalf("round %d: 取消不该失败：%v", i, cancelErr)
		}
		emits, err := db.ListEmits(ctx, parent.ID)
		if err != nil {
			t.Fatalf("round %d: list emits: %v", i, err)
		}
		tasks, err := db.ListTasksByTrace(ctx, ns, parent.TraceID)
		if err != nil {
			t.Fatalf("round %d: list tasks: %v", i, err)
		}
		if len(emits) == 0 {
			// 取消先赢：emit 必须被拒且 0 写。
			if !errors.Is(emitErr, store.ErrTaskNotRunning) {
				t.Fatalf("round %d: 取消先赢时 emit 应 ErrTaskNotRunning，得到 %v", i, emitErr)
			}
			if len(tasks) != 1 {
				t.Fatalf("round %d: 取消先赢时不该有子任务，trace 有 %d 行", i, len(tasks))
			}
			continue
		}
		// emit 先赢：回执与子任务同生共死（1 条 emit + 1 个子任务 + 取消意图）。
		if emitErr != nil || !first {
			t.Fatalf("round %d: emit 先赢时应 first=true 且无错：first=%v err=%v", i, first, emitErr)
		}
		if len(emits) != 1 || len(tasks) != 2 {
			t.Fatalf("round %d: emit/子任务必须成对，emits=%d tasks=%d", i, len(emits), len(tasks))
		}
		got, err := db.GetTask(ctx, ns, parent.ID)
		if err != nil {
			t.Fatalf("round %d: get parent: %v", i, err)
		}
		if got.CancelRequestedAt == nil {
			t.Fatalf("round %d: emit 先赢后取消必须留下意图", i)
		}
		// 收尾：确认 reap 释放槽位。
		if err := ackCancel(t, db, auth.NodeID, parent.ID, auth.Attempt, true); err != nil {
			t.Fatalf("round %d: ack reap: %v", i, err)
		}
	}
}
