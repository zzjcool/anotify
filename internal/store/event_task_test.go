package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// mkTask 构造一个 pending 任务。
func mkTask(orgID, traceID string, cap *CapabilityVersion, path ...string) *Task {
	if path == nil {
		path = []string{}
	}
	return &Task{
		OrgID:        orgID,
		TraceID:      traceID,
		Path:         path,
		CapabilityID: cap.ID,
		Capability:   cap.Name,
		Version:      cap.Version,
		Params:       json.RawMessage(`{"k":"v"}`),
		CapHash:      cap.ContentHash,
		GraphID:      "grf_test",
	}
}

// setupTaskFixture 建组织 + 能力 + 一个待派发的任务。
func setupTaskFixture(t *testing.T) (db *DB, orgID string, cap *CapabilityVersion, task *Task) {
	t.Helper()
	db = newTestDB(t)
	ctx := context.Background()
	orgID, _ = newTestOrg(t, db)
	cap = mkCap(orgID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	task = mkTask(orgID, "trace_1", cap)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return db, orgID, cap, task
}

// driveToRunning 把任务按状态机推到 running（模拟真实节点时序：
// pending → assigned → acked → running）。
//
// 不能直接对 pending 调 ReportResult：契约 §3.2 里 result 只能从 running 出发，
// 而 pending 的任务还没有节点持有，拿不到 result 帧。
func driveToRunning(t *testing.T, db *DB, orgID, nodeID string, task *Task) string {
	t.Helper()
	ctx := context.Background()
	instID, err := db.LeaseInstance(ctx, nodeID, task.ID)
	if err != nil {
		t.Fatalf("lease instance: %v", err)
	}
	if err := db.AssignTask(ctx, orgID, task.ID, nodeID, instID, Now()+LeaseTTLSec); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := db.Transition(ctx, orgID, task.ID, TaskAssigned, TaskAcked); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := db.StartTask(ctx, orgID, task.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	return instID
}

// expandCapacity 把节点扩到 n 个并发槽位（setupRunningTask 默认只申报 1 个，
// 后续还要推别的任务时必须先扩，否则 LeaseInstance 会 ErrNoCapacity）。
func expandCapacity(t *testing.T, db *DB, orgID, nodeID string, n int) {
	t.Helper()
	specs := make([]InstanceSpec, n)
	for i := range specs {
		specs[i] = InstanceSpec{Slot: i, Concurrency: 1}
	}
	if _, err := db.UpsertInstances(context.Background(), orgID, nodeID, specs); err != nil {
		t.Fatalf("expand capacity to %d: %v", n, err)
	}
}

// mustOnlyNode 返回组织里唯一的节点 ID（setupRunningTask 只建一个）。
func mustOnlyNode(t *testing.T, db *DB, orgID string) string {
	t.Helper()
	nodes, err := db.ListNodes(context.Background(), orgID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want exactly 1 node, got %d", len(nodes))
	}
	return nodes[0].ID
}

// setupRunningTask 建一个已推到 running 的任务（含节点与槽位）。
func setupRunningTask(t *testing.T) (db *DB, orgID, nodeID string, cap *CapabilityVersion, task *Task) {
	t.Helper()
	ctx := context.Background()
	db = newTestDB(t)
	orgID, _ = newTestOrg(t, db)
	cap = mkCap(orgID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	node, _ := mkNode(t, db, orgID, "mbp")
	nodeID = node.ID
	if _, err := db.ConfirmCapabilities(ctx, orgID, nodeID, []CapabilityRef{{Name: cap.Name, Version: cap.Version}}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := db.UpdateNodeStatus(ctx, orgID, nodeID, NodeStatusOnline); err != nil {
		t.Fatalf("online: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, orgID, nodeID, []InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		t.Fatalf("instances: %v", err)
	}
	task = mkTask(orgID, "trace_run", cap)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	driveToRunning(t, db, orgID, nodeID, task)
	return db, orgID, nodeID, cap, task
}

// ---------- #63：events 幂等 ----------

// TestEventIdempotency 同一 idem_key 插两次 → inserted=false 且返回同一 event_id（#63 验收）。
func TestEventIdempotency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	key := "X-GitHub-Delivery-abc"
	first := &Event{
		OrgID: orgID, TraceID: "trace_abc", Source: "github",
		Type: "pull_request.opened", Payload: json.RawMessage(`{"n":1}`), IdemKey: &key,
	}
	inserted, err := db.InsertEvent(ctx, first)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !inserted {
		t.Fatal("first insert returned inserted=false")
	}
	if first.ID == "" {
		t.Fatal("first insert did not assign event id")
	}

	// 重投：不同 trace_id 也想插，但必须返回已有的
	second := &Event{
		OrgID: orgID, TraceID: "trace_DIFFERENT", Source: "github",
		Type: "pull_request.opened", Payload: json.RawMessage(`{"n":1}`), IdemKey: &key,
	}
	inserted, err = db.InsertEvent(ctx, second)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if inserted {
		t.Fatal("duplicate idem_key reported inserted=true")
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate returned event_id %q, want existing %q", second.ID, first.ID)
	}
	if second.TraceID != "trace_abc" {
		t.Fatalf("duplicate returned trace_id %q, want existing trace_abc (not the new one)", second.TraceID)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE org_id=?`, orgID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("events has %d rows, want 1", n)
	}
}

// TestEventWithoutIdemKeyAllowsDuplicates 不带 idem_key 允许重复（契约 §10.1）。
func TestEventWithoutIdemKeyAllowsDuplicates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	for i := 0; i < 3; i++ {
		e := &Event{OrgID: orgID, TraceID: "trace_x", Source: "manual", Type: "click"}
		inserted, err := db.InsertEvent(ctx, e)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		if !inserted {
			t.Fatalf("insert %d (no idem_key) reported inserted=false", i)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE org_id=?`, orgID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("events has %d rows, want 3 (no idem_key ⇒ duplicates allowed)", n)
	}
}

// TestEventIdemKeyScopedPerSourceAndOrg idem_key 的唯一性是 (org, source, key)。
func TestEventIdemKeyScopedPerSourceAndOrg(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgA, _ := newTestOrg(t, db)
	orgB, _ := newTestOrg(t, db)
	key := "same-delivery-id"

	// 同 org、不同 source：允许（各自独立的投递 ID 空间）
	for _, src := range []string{"github", "gitlab"} {
		e := &Event{OrgID: orgA, TraceID: "t-" + src, Source: src, Type: "push", IdemKey: &key}
		inserted, err := db.InsertEvent(ctx, e)
		if err != nil {
			t.Fatalf("insert source %s: %v", src, err)
		}
		if !inserted {
			t.Fatalf("source %s was deduped against another source", src)
		}
	}
	// 不同 org、同 source、同 key：允许（组织隔离，不变量 I10）
	e := &Event{OrgID: orgB, TraceID: "t-b", Source: "github", Type: "push", IdemKey: &key}
	inserted, err := db.InsertEvent(ctx, e)
	if err != nil {
		t.Fatalf("insert orgB: %v", err)
	}
	if !inserted {
		t.Fatal("idem_key leaked across orgs (invariant I10 violated)")
	}
}

// TestEventGetAndList 事件读取与列表。
func TestEventGetAndList(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	if _, err := db.GetEvent(ctx, orgID, "evt_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetEvent missing err = %v, want ErrNotFound", err)
	}
	if _, err := db.GetEventByTrace(ctx, orgID, "trace_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetEventByTrace missing err = %v, want ErrNotFound", err)
	}

	e1 := &Event{OrgID: orgID, TraceID: "trace_1", Source: "github", Type: "pr.opened", Payload: json.RawMessage(`{"a":1}`)}
	if _, err := db.InsertEvent(ctx, e1); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	e2 := &Event{OrgID: orgID, TraceID: "trace_2", Source: "cron", Type: "tick"}
	if _, err := db.InsertEvent(ctx, e2); err != nil {
		t.Fatalf("insert 2: %v", err)
	}

	got, err := db.GetEvent(ctx, orgID, e1.ID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if got.TraceID != "trace_1" || got.Source != "github" || got.Type != "pr.opened" {
		t.Fatalf("event round-trip mismatch: %+v", got)
	}
	if string(got.Payload) != `{"a":1}` {
		t.Fatalf("payload not preserved: %s", got.Payload)
	}

	all, err := db.ListEvents(ctx, orgID, "", "", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 events, got %d", len(all))
	}
	// 按 trace 过滤
	byTrace, err := db.ListEvents(ctx, orgID, "trace_1", "", 10)
	if err != nil {
		t.Fatalf("list by trace: %v", err)
	}
	if len(byTrace) != 1 || byTrace[0].ID != e1.ID {
		t.Fatalf("trace filter wrong: %+v", byTrace)
	}
	// by trace helper
	root, err := db.GetEventByTrace(ctx, orgID, "trace_1")
	if err != nil {
		t.Fatalf("get by trace: %v", err)
	}
	if root.ID != e1.ID {
		t.Fatalf("GetEventByTrace returned %s, want %s", root.ID, e1.ID)
	}

	// 空列表是 []
	empty, err := db.ListEvents(ctx, orgID, "trace_none", "", 10)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if empty == nil {
		t.Fatal("ListEvents returned nil (need [])")
	}
}

// TestEventValidation 入参校验。
func TestEventValidation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	if _, err := db.InsertEvent(ctx, nil); err == nil {
		t.Error("InsertEvent(nil) accepted")
	}
	if _, err := db.InsertEvent(ctx, &Event{OrgID: orgID, Source: "s", Type: "t"}); err == nil {
		t.Error("InsertEvent without trace_id accepted (trigger must generate it)")
	}
	if _, err := db.InsertEvent(ctx, &Event{OrgID: orgID, TraceID: "t"}); err == nil {
		t.Error("InsertEvent without source/type accepted")
	}
	if _, err := db.InsertEvent(ctx, &Event{TraceID: "t", Source: "s", Type: "t"}); err == nil {
		t.Error("InsertEvent without org accepted")
	}
}

// ---------- #63：tasks out 三态 ----------

// TestOutTriState 不变量 I5：未回报 / 回报 NULL / 回报值 三者行为不同（#63 验收）。
func TestOutTriState(t *testing.T) {
	db, orgID, nodeID, cap, _ := setupRunningTask(t)
	ctx := context.Background()
	expandCapacity(t, db, orgID, nodeID, 8)

	// ① reported=0, out=NULL：没回报
	unreported := mkTask(orgID, "trace_unreported", cap)
	if err := db.CreateTasks(ctx, []*Task{unreported}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetTask(ctx, orgID, unreported.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Reported {
		t.Error("fresh task has reported=true")
	}
	if got.Out != nil {
		t.Errorf("fresh task has out=%q, want nil", *got.Out)
	}

	// ② reported=1, out=NULL：明确到头
	atEnd := mkTask(orgID, "trace_end", cap)
	if err := db.CreateTasks(ctx, []*Task{atEnd}); err != nil {
		t.Fatalf("create: %v", err)
	}
	driveToRunning(t, db, orgID, mustOnlyNode(t, db, orgID), atEnd)
	if err := db.ReportResult(ctx, orgID, atEnd.ID, TaskSucceeded, nil, true, "done", nil, ""); err != nil {
		t.Fatalf("report nil out: %v", err)
	}
	got, err = db.GetTask(ctx, orgID, atEnd.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Reported {
		t.Fatal("reported=true was not persisted")
	}
	if got.Out != nil {
		t.Fatalf("reported + nil out stored as %q, want NULL", *got.Out)
	}
	if got.Status != TaskSucceeded {
		t.Fatalf("status = %q, want succeeded", got.Status)
	}

	// ③ reported=1, out="fix"：走 fix
	goesToFix := mkTask(orgID, "trace_fix", cap)
	if err := db.CreateTasks(ctx, []*Task{goesToFix}); err != nil {
		t.Fatalf("create: %v", err)
	}
	driveToRunning(t, db, orgID, mustOnlyNode(t, db, orgID), goesToFix)
	port := "fix"
	if err := db.ReportResult(ctx, orgID, goesToFix.ID, TaskFailed, &port, true, "needs fix", nil, "type error"); err != nil {
		t.Fatalf("report out=fix: %v", err)
	}
	got, err = db.GetTask(ctx, orgID, goesToFix.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Reported {
		t.Fatal("reported lost")
	}
	if got.Out == nil || *got.Out != "fix" {
		t.Fatalf("out = %v, want \"fix\"", got.Out)
	}
	if got.Status != TaskFailed {
		t.Fatalf("status = %q, want failed (I4: out 与 status 正交)", got.Status)
	}
	if got.Error != "type error" {
		t.Fatalf("error = %q", got.Error)
	}
	if got.FinishedAt == nil {
		t.Fatal("finished_at not set on terminal status")
	}
}

// TestReportResultMissingOutIsNotNilOut 关键区分：
// 即使 out=nil，「reported」两态含义完全不同——这里断言 reported 列真的写进去了。
func TestReportResultMissingOutIsNotNilOut(t *testing.T) {
	db, orgID, nodeID, cap, _ := setupRunningTask(t)
	ctx := context.Background()

	a := mkTask(orgID, "trace_a", cap)
	b := mkTask(orgID, "trace_b", cap)
	for _, x := range []*Task{a, b} {
		if err := db.CreateTasks(ctx, []*Task{x}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	// b 推到 running 才能回报；a 停在 pending 表示「节点根本没上报」
	expandCapacity(t, db, orgID, nodeID, 8)
	driveToRunning(t, db, orgID, nodeID, b)
	// a: 没回报（不调 ReportResult）
	// b: 回报到头（reported=true, out=nil）
	if err := db.ReportResult(ctx, orgID, b.ID, TaskSucceeded, nil, true, "", nil, ""); err != nil {
		t.Fatalf("report: %v", err)
	}
	ga, _ := db.GetTask(ctx, orgID, a.ID)
	gb, _ := db.GetTask(ctx, orgID, b.ID)
	if ga.Reported == gb.Reported {
		t.Fatal("reported column cannot distinguish 没回报 from 回报到头 (I5 broken)")
	}
	if ga.Out != nil || gb.Out != nil {
		t.Fatal("both should have NULL out")
	}
}

// TestReportResultRejectsEmptyOutString ” 不是合法出口名（契约 §2.5）。
func TestReportResultRejectsEmptyOutString(t *testing.T) {
	db, orgID, _, cap, task := setupRunningTask(t)
	ctx := context.Background()
	empty := ""
	err := db.ReportResult(ctx, orgID, task.ID, TaskSucceeded, &empty, true, "", nil, "")
	if err == nil {
		t.Fatal("empty out string accepted (should use nil for 到头)")
	}
	_ = cap
}

// ---------- #63：批量建任务原子性 ----------

// TestCreateTasksAtomic 第 3 个任务故意写坏 → 前 2 个也不落库（#63 验收）。
func TestCreateTasksAtomic(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	cap := mkCap(orgID, "review", 1, nil)
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create cap: %v", err)
	}

	good1 := mkTask(orgID, "trace_1", cap)
	good2 := mkTask(orgID, "trace_1", cap)
	// 第 3 个：capability_id 指向不存在的行 → 外键失败
	bad := mkTask(orgID, "trace_1", cap)
	bad.CapabilityID = "cap_does_not_exist"

	err := db.CreateTasks(ctx, []*Task{good1, good2, bad})
	if err == nil {
		t.Fatal("CreateTasks accepted a broken task")
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE org_id=?`, orgID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("partial write: %d tasks persisted, want 0 (fanout must be atomic)", n)
	}
}

// TestCreateTasksFanout 扇出：一个事件产 N 个任务。
func TestCreateTasksFanout(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	cap := mkCap(orgID, "review", 1, nil)
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	var batch []*Task
	for i := 0; i < 3; i++ {
		batch = append(batch, mkTask(orgID, "trace_fanout", cap))
	}
	if err := db.CreateTasks(ctx, batch); err != nil {
		t.Fatalf("create fanout: %v", err)
	}
	tasks, err := db.ListTasks(ctx, orgID, "trace_fanout", "", "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(tasks))
	}
	seen := map[string]bool{}
	for _, x := range tasks {
		if x.ID == "" {
			t.Fatal("task created without id")
		}
		if seen[x.ID] {
			t.Fatal("duplicate task id in fanout")
		}
		seen[x.ID] = true
		if x.Status != TaskPending {
			t.Fatalf("new task status = %q, want pending", x.Status)
		}
	}
}

// TestCreateTasksRejectsNonPending 建任务只能落在 pending。
func TestCreateTasksRejectsNonPending(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	cap := mkCap(orgID, "review", 1, nil)
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	task := mkTask(orgID, "trace_1", cap)
	task.Status = TaskSucceeded
	if err := db.CreateTasks(ctx, []*Task{task}); err == nil {
		t.Fatal("CreateTasks accepted a task that was not pending")
	}
}

// TestCreateTasksEmptyIsNoop 空扇出是正常情况，不是错误。
func TestCreateTasksEmptyIsNoop(t *testing.T) {
	db := newTestDB(t)
	if err := db.CreateTasks(context.Background(), nil); err != nil {
		t.Fatalf("CreateTasks(nil) = %v, want nil", err)
	}
	if err := db.CreateTasks(context.Background(), []*Task{}); err != nil {
		t.Fatalf("CreateTasks([]) = %v, want nil", err)
	}
}

// TestTaskRoundTrip 每个新字段的往返一致性（AGENTS.md §3 要求）。
func TestTaskRoundTrip(t *testing.T) {
	db, orgID, cap, _ := setupTaskFixture(t)
	ctx := context.Background()

	node := "nd_round"
	inst := "nins_round"
	lease := int64(1730000000)
	started := int64(1730000001)
	finished := int64(1730000002)
	port := "fix"
	parent := "tsk_parent"
	task := &Task{
		OrgID:        orgID,
		TraceID:      "trace_rt",
		ParentTaskID: &parent,
		Path:         []string{"cap_a", "cap_b"},
		CapabilityID: cap.ID,
		Capability:   cap.Name,
		Version:      cap.Version,
		Params:       json.RawMessage(`{"diff":"x"}`),
		Status:       TaskPending,
		NodeID:       &node,
		InstanceID:   &inst,
		LeaseUntil:   &lease,
		Attempts:     3,
		Out:          &port,
		Reported:     true,
		Summary:      "发现 2 处类型错误",
		Artifacts:    []string{"tsk_8f21a3c0"},
		Error:        "",
		CapHash:      cap.ContentHash,
		GraphID:      "grf_rt",
		StartedAt:    &started,
		FinishedAt:   &finished,
	}
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetTask(ctx, orgID, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.TraceID != "trace_rt" || got.Capability != cap.Name || got.Version != cap.Version {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if got.ParentTaskID == nil || *got.ParentTaskID != parent {
		t.Fatalf("parent_task_id lost: %v", got.ParentTaskID)
	}
	if len(got.Path) != 2 || got.Path[0] != "cap_a" || got.Path[1] != "cap_b" {
		t.Fatalf("path lost: %v", got.Path)
	}
	if string(got.Params) != `{"diff":"x"}` {
		t.Fatalf("params lost: %s", got.Params)
	}
	if got.NodeID == nil || *got.NodeID != node {
		t.Fatalf("node_id lost: %v", got.NodeID)
	}
	if got.InstanceID == nil || *got.InstanceID != inst {
		t.Fatalf("instance_id lost: %v", got.InstanceID)
	}
	if got.LeaseUntil == nil || *got.LeaseUntil != lease {
		t.Fatalf("lease_until lost: %v", got.LeaseUntil)
	}
	if got.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", got.Attempts)
	}
	if got.Out == nil || *got.Out != port {
		t.Fatalf("out lost: %v", got.Out)
	}
	if !got.Reported {
		t.Fatal("reported lost")
	}
	if got.Summary != "发现 2 处类型错误" {
		t.Fatalf("summary lost: %q", got.Summary)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0] != "tsk_8f21a3c0" {
		t.Fatalf("artifacts lost: %v", got.Artifacts)
	}
	if got.CapHash != cap.ContentHash || got.GraphID != "grf_rt" {
		t.Fatalf("version pinning lost: %q / %q", got.CapHash, got.GraphID)
	}
	if got.StartedAt == nil || *got.StartedAt != started {
		t.Fatalf("started_at lost: %v", got.StartedAt)
	}
	if got.FinishedAt == nil || *got.FinishedAt != finished {
		t.Fatalf("finished_at lost: %v", got.FinishedAt)
	}
}

// TestListTasksEmptyIsArray 不变量 I9。
func TestListTasksEmptyIsArray(t *testing.T) {
	db := newTestDB(t)
	orgID, _ := newTestOrg(t, db)
	tasks, err := db.ListTasks(context.Background(), orgID, "", "", "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if tasks == nil {
		t.Fatal("ListTasks returned nil (need [])")
	}
}

// TestListTasksFilters 过滤条件。
func TestListTasksFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	cap := mkCap(orgID, "review", 1, nil)
	if err := db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	a := mkTask(orgID, "trace_a", cap)
	b := mkTask(orgID, "trace_b", cap)
	if err := db.CreateTasks(ctx, []*Task{a, b}); err != nil {
		t.Fatalf("create: %v", err)
	}
	byTrace, err := db.ListTasks(ctx, orgID, "trace_a", "", "", 10)
	if err != nil {
		t.Fatalf("by trace: %v", err)
	}
	if len(byTrace) != 1 || byTrace[0].ID != a.ID {
		t.Fatalf("trace filter wrong: %+v", byTrace)
	}
	byStatus, err := db.ListTasks(ctx, orgID, "", "", TaskPending, 10)
	if err != nil {
		t.Fatalf("by status: %v", err)
	}
	if len(byStatus) != 2 {
		t.Fatalf("pending filter got %d, want 2", len(byStatus))
	}
	none, err := db.ListTasks(ctx, orgID, "", "", TaskSucceeded, 10)
	if err != nil {
		t.Fatalf("by status none: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("want 0 succeeded, got %d", len(none))
	}
}

// TestTaskOrgIsolation 不变量 I10。
func TestTaskOrgIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgA, _ := newTestOrg(t, db)
	orgB, _ := newTestOrg(t, db)
	capA := mkCap(orgA, "review", 1, nil)
	if err := db.CreateCapability(ctx, capA); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	task := mkTask(orgA, "trace_iso", capA)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.GetTask(ctx, orgB, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org GetTask err = %v, want ErrNotFound", err)
	}
	tasksB, err := db.ListTasks(ctx, orgB, "", "", "", 10)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(tasksB) != 0 {
		t.Fatalf("org B sees %d tasks from org A", len(tasksB))
	}
}
