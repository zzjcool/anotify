package store

// emission_test.go 覆盖 v4 emit 入口（capability.md §4.1、D-I9）：
//
//	EmitAuthorized 在**同一个事务**里做「授权校验 → 幂等记录 → 建下一跳任务」。
//	顺序不可调整：先授权后幂等，否则一条失效执行可以靠「上次已经 emit 过」拿回执、
//	继续跑完写终态——幂等就成了绕过授权的后门。
//
// v3 的 EnsureTaskToken / ensure 式补写与 emissions 迁移断言随旧 token 通道退役：
// v4 的 attempt token 在 grant 时一次下发、只存 sha256，emit 必须带它。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// emitFixture 建能力 + 节点 + 任务并授租约，返回 (cap, node, task, auth, leaseID)。
func emitFixture(t *testing.T, db *DB, nsID, capName, nodeName, traceID string) (*CapabilityVersion, *Node, *Task, EmitAuth, string) {
	t.Helper()
	c := v4Cap(t, db, nsID, capName, 1)
	node := v4Node(t, db, nodeName, 1)
	task := v4Task(t, db, nsID, c, traceID, "")
	leaseID, token, attempt := v4Grant(t, db, task.ID, node.ID, "asg-"+task.ID)
	auth := EmitAuth{
		TaskID: task.ID, NodeID: node.ID, NodeTokenHash: v4Auth(t, db, node.ID).NodeTokenHash,
		Attempt: attempt, LeaseID: leaseID, AttemptToken: token, Now: Now(),
	}
	return c, node, task, auth, leaseID
}

// TestEmissionRoundTrip emit 行存什么读什么（to_ref/data/result/emit_id）。
func TestEmissionRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, _, task, auth, _ := emitFixture(t, db, nsID, "review", "emit-node", "tr_emit_rt")

	child := mkTask(nsID, task.TraceID, c)
	emitID := "emit-1"
	rec := &EmitRecord{TaskID: task.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{"a":1}`)}
	result, first, err := db.EmitAuthorized(ctx, auth, rec, []*Task{child})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !first {
		t.Fatal("首次 emit 应 first=true")
	}
	if len(result) == 0 || !json.Valid(result) {
		t.Fatalf("emit result 非法: %s", result)
	}
	got, err := db.LookupEmit(ctx, task.ID, emitID)
	if err != nil {
		t.Fatalf("lookup emit: %v", err)
	}
	if got.ToRef != "fix" || string(got.Data) != `{"a":1}` {
		t.Fatalf("emit 行往返 = %+v", got)
	}
	if got.ID == "" || got.CreatedAt == 0 {
		t.Fatalf("emit 行未回填 id/created_at: %+v", got)
	}
	if len(got.Result) == 0 {
		t.Fatal("emit 行必须留下 result（重放回执）")
	}
}

// TestInsertEmissionIdempotent 同 (task, emitId, to, data) 重复提交 → first=false 且同一 result。
func TestInsertEmissionIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, _, task, auth, _ := emitFixture(t, db, nsID, "review", "emit-idem", "tr_emit_idem")

	emitID := "emit-dup"
	firstResult, first, err := db.EmitAuthorized(ctx, auth,
		&EmitRecord{TaskID: task.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{"k":"v"}`)}, nil)
	if err != nil || !first {
		t.Fatalf("first emit: first=%v err=%v", first, err)
	}
	secondResult, second, err := db.EmitAuthorized(ctx, auth,
		&EmitRecord{TaskID: task.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{"k":"v"}`)}, nil)
	if err != nil {
		t.Fatalf("second emit: %v", err)
	}
	if second {
		t.Fatal("同事实重复提交应 first=false")
	}
	if string(secondResult) != string(firstResult) {
		t.Fatalf("重复提交回执不一致: %s vs %s", secondResult, firstResult)
	}
	all, err := db.ListEmits(ctx, task.ID)
	if err != nil {
		t.Fatalf("list emits: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("重复提交留下 %d 行, want 1", len(all))
	}
}

// TestEmissionIDConflict 同一个 emitId 换了 to/data → ErrEmitIDConflict。
func TestEmissionIDConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, _, task, auth, _ := emitFixture(t, db, nsID, "review", "emit-conflict", "tr_emit_conflict")

	emitID := "emit-once"
	if _, _, err := db.EmitAuthorized(ctx, auth,
		&EmitRecord{TaskID: task.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{"k":"v"}`)}, nil); err != nil {
		t.Fatalf("first emit: %v", err)
	}
	if _, _, err := db.EmitAuthorized(ctx, auth,
		&EmitRecord{TaskID: task.ID, EmitID: &emitID, ToRef: "notify", Data: json.RawMessage(`{"k":"v"}`)}, nil); !errors.Is(err, ErrEmitIDConflict) {
		t.Fatalf("同 id 换 to 应报 ErrEmitIDConflict，得到 %v", err)
	}
	if _, _, err := db.EmitAuthorized(ctx, auth,
		&EmitRecord{TaskID: task.ID, EmitID: &emitID, ToRef: "fix", Data: json.RawMessage(`{"k":"other"}`)}, nil); !errors.Is(err, ErrEmitIDConflict) {
		t.Fatalf("同 id 换 data 应报 ErrEmitIDConflict，得到 %v", err)
	}
}

// TestEmissionWithoutIDAlwaysNew 不带 emitId 的 emit 永远是新行（不去重）。
func TestEmissionWithoutIDAlwaysNew(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, _, task, auth, _ := emitFixture(t, db, nsID, "review", "emit-noid", "tr_emit_noid")

	for i := 0; i < 2; i++ {
		if _, first, err := db.EmitAuthorized(ctx, auth,
			&EmitRecord{TaskID: task.ID, ToRef: "fix", Data: json.RawMessage(`{"batch":1}`)}, nil); err != nil || !first {
			t.Fatalf("emit %d: first=%v err=%v", i, first, err)
		}
	}
	all, err := db.ListEmits(ctx, task.ID)
	if err != nil {
		t.Fatalf("list emits: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("无 emitId 的两次 emit = %d 行, want 2", len(all))
	}
}

// TestEmissionListOrderedAndScoped 列表按任务隔离（别人任务的 emit 不可见）。
func TestEmissionListOrderedAndScoped(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, node, task, auth, _ := emitFixture(t, db, nsID, "review", "emit-scope", "tr_emit_scope")

	if _, _, err := db.EmitAuthorized(ctx, auth,
		&EmitRecord{TaskID: task.ID, ToRef: "fix", Data: json.RawMessage(`{}`)}, nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	other := v4Task(t, db, nsID, c, "tr_emit_other", "")
	otherLease, otherToken, otherAttempt := v4Grant(t, db, other.ID, node.ID, "asg-other")
	otherAuth := EmitAuth{
		TaskID: other.ID, NodeID: node.ID, NodeTokenHash: v4Auth(t, db, node.ID).NodeTokenHash,
		Attempt: otherAttempt, LeaseID: otherLease, AttemptToken: otherToken, Now: Now(),
	}
	if _, _, err := db.EmitAuthorized(ctx, otherAuth,
		&EmitRecord{TaskID: other.ID, ToRef: "fix", Data: json.RawMessage(`{}`)}, nil); err != nil {
		t.Fatalf("emit other: %v", err)
	}
	first, err := db.ListEmits(ctx, task.ID)
	if err != nil {
		t.Fatalf("list task emits: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("task 的 emit = %d 行, want 1（任务隔离）", len(first))
	}
}

// TestGetEmissionNotFound 查不存在的 emit → ErrNotFound。
func TestGetEmissionNotFound(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.LookupEmit(context.Background(), "tsk_missing", "emit_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的 emit 应 ErrNotFound，得到 %v", err)
	}
}

// TestInsertEmissionRejectsBadInput 形状非法与授权不完整都必须拒（fail-closed）。
func TestInsertEmissionRejectsBadInput(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, _, task, auth, _ := emitFixture(t, db, nsID, "review", "emit-bad", "tr_emit_bad")

	if _, _, err := db.EmitAuthorized(ctx, auth, nil, nil); err == nil {
		t.Fatal("nil record 被接受")
	}
	if _, _, err := db.EmitAuthorized(ctx, auth, &EmitRecord{TaskID: task.ID}, nil); err == nil {
		t.Fatal("空 to_ref 被接受")
	}
	bad := auth
	bad.AttemptToken = ""
	if _, _, err := db.EmitAuthorized(ctx, bad,
		&EmitRecord{TaskID: task.ID, ToRef: "fix", Data: json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("缺 attemptToken 的 emit 被接受")
	}
	all, err := db.ListEmits(ctx, task.ID)
	if err != nil {
		t.Fatalf("list emits: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("被拒的 emit 留下了 %d 行", len(all))
	}
}

// TestEmitAuthorizedRejectsStaleExecution 失效执行（错 token / 冒充节点 / 取消在飞 / 过期租约）一律拒。
func TestEmitAuthorizedRejectsStaleExecution(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, _, task, auth, _ := emitFixture(t, db, nsID, "review", "emit-stale", "tr_emit_stale")

	bad := auth
	bad.AttemptToken = "not-the-token"
	if _, _, err := db.EmitAuthorized(ctx, bad,
		&EmitRecord{TaskID: task.ID, ToRef: "fix", Data: json.RawMessage(`{}`)}, nil); !errors.Is(err, ErrAttemptTokenInvalid) {
		t.Fatalf("错 token 应报 ErrAttemptTokenInvalid，得到 %v", err)
	}
	other := v4Node(t, db, "emit-other-node", 0)
	bad = auth
	bad.NodeTokenHash = v4Auth(t, db, other.ID).NodeTokenHash
	if _, _, err := db.EmitAuthorized(ctx, bad,
		&EmitRecord{TaskID: task.ID, ToRef: "fix", Data: json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("冒充另一台节点的 emit 被接受")
	}
	if err := db.RequestCancel(ctx, task.ID, "user", Now()); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	if _, _, err := db.EmitAuthorized(ctx, auth,
		&EmitRecord{TaskID: task.ID, ToRef: "fix", Data: json.RawMessage(`{}`)}, nil); !errors.Is(err, ErrTaskNotRunning) {
		t.Fatalf("取消在飞的 emit 应报 ErrTaskNotRunning，得到 %v", err)
	}
	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.LeaseUntil == nil {
		t.Fatal("grant 后应有 leaseUntil")
	}
	expired := auth
	expired.Now = *got.LeaseUntil + 1
	if _, _, err := db.EmitAuthorized(ctx, expired,
		&EmitRecord{TaskID: task.ID, ToRef: "fix", Data: json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("过期租约的 emit 被接受")
	}
}

// TestEmitCreatesChildTasksAtomically 子任务与 emit 行同一个事务：子任务非法 → 整体回滚。
func TestEmitCreatesChildTasksAtomically(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, _, task, auth, _ := emitFixture(t, db, nsID, "review", "emit-child", "tr_emit_child")

	child := mkTask(nsID, task.TraceID, c)
	child.ParentTaskID = &task.ID
	if _, _, err := db.EmitAuthorized(ctx, auth,
		&EmitRecord{TaskID: task.ID, ToRef: "review", Data: json.RawMessage(`{}`)}, []*Task{child}); err != nil {
		t.Fatalf("emit with child: %v", err)
	}
	got, err := db.GetTask(ctx, nsID, child.ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if got.ParentTaskID == nil || *got.ParentTaskID != task.ID {
		t.Fatalf("子任务父绑定 = %v, want %s", got.ParentTaskID, task.ID)
	}

	bad := &Task{NamespaceID: nsID, TraceID: task.TraceID, CapabilityID: "cap_missing", Capability: "missing"}
	emitID := "emit-rollback"
	_, _, err = db.EmitAuthorized(ctx, auth,
		&EmitRecord{TaskID: task.ID, EmitID: &emitID, ToRef: "review", Data: json.RawMessage(`{}`)}, []*Task{bad})
	if err == nil {
		t.Fatal("含非法子任务的 emit 被接受")
	}
	if _, lerr := db.LookupEmit(ctx, task.ID, emitID); !errors.Is(lerr, ErrNotFound) {
		t.Fatalf("失败事务留下了 emit 行: %v", lerr)
	}
}

// TestEmitColumnsInBaseline v4 baseline 的 emits 形状（task/emit/emit_id 唯一）。
func TestEmitColumnsInBaseline(t *testing.T) {
	db := newTestDB(t)
	cols := columnNames(t, db, "emits")
	for _, col := range []string{"id", "task_id", "emit_id", "to_ref", "data", "result", "created_at"} {
		if !cols[col] {
			t.Fatalf("emits 表缺列 %q", col)
		}
	}
}
