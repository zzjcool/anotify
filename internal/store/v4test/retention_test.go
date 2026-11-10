package v4test

import (
	"context"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// finishedTask 造一条**已结束**的任务（succeeded），返回它的 id。
func finishedTask(t *testing.T, db *store.DB, nsID string, c *store.CapabilityVersion, node *store.Node, traceID string, finishedAt int64) string {
	t.Helper()
	ctx := context.Background()
	task := newTask(t, db, nsID, c, traceID, "")
	auth := store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}
	leaseID, _, attempt := grant(t, db, task.ID, node.ID, "asg-"+traceID)
	if err := db.CompleteTaskAuthorized(ctx, auth, task.ID, attempt, leaseID, 0, store.TaskResult{
		Phase: store.TaskSucceeded, Summary: "ok", StartedAt: time.Unix(finishedAt-1, 0), FinishedAt: time.Unix(finishedAt, 0),
	}); err != nil {
		t.Fatalf("complete task %s: %v", task.ID, err)
	}
	if _, err := db.Exec(`UPDATE tasks SET result_accepted_at = ?, created_at = ? WHERE id = ?`,
		finishedAt, finishedAt-1, task.ID); err != nil {
		t.Fatalf("age task %s: %v", task.ID, err)
	}
	// Age the trusted observation in this historical fixture, not the Node clock.
	if _, err := db.Exec(`UPDATE task_attempts SET result_accepted_at = ? WHERE task_id = ? AND attempt = ?`, finishedAt, task.ID, attempt); err != nil {
		t.Fatal(err)
	}
	return task.ID
}

// TestRetentionTaskTTL 验终态任务按 taskTtlSec 清理、活动任务不删、emits 级联。
func TestRetentionTaskTTL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ret-tasks")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 2)
	now := store.Now()

	oldID := finishedTask(t, db, ns, c, node, "old", now-1000)
	freshID := finishedTask(t, db, ns, c, node, "fresh", now-10)
	running := newTask(t, db, ns, c, "running", "")
	grant(t, db, running.ID, node.ID, "asg-running")

	// 给老任务挂一条 emit（验证级联删除）。
	// 用原始 SQL 作夹具：emit 的写路径要求「任务正在跑 + 有效授权」，
	// 而这里要的是「终态任务上已有一条历史 emit 行」——那正是保留期要级联清掉的东西。
	if _, err := db.Exec(
		`INSERT INTO emits (id, task_id, emit_id, to_ref, data, result, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		"emt_fixture", oldID, "e-old", "x", "{}", `{"taskIds":[]}`, now-1000); err != nil {
		t.Fatalf("insert emit fixture: %v", err)
	}

	setRetention(t, db, 0, 100, 0) // taskTtlSec=100
	stats, err := db.RunRetention(ctx, now)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.TaskTTLSec != 100 || stats.TasksSkipped {
		t.Fatalf("task TTL 应生效：%+v", stats)
	}
	if stats.TasksDeleted != 1 {
		t.Fatalf("应删掉 1 条终态任务，得到 %d", stats.TasksDeleted)
	}
	if _, err := db.GetTask(ctx, ns, oldID); err == nil {
		t.Fatalf("过期终态任务应被删")
	}
	if _, err := db.GetTask(ctx, ns, freshID); err != nil {
		t.Fatalf("未过期任务不该被删：%v", err)
	}
	if _, err := db.GetTask(ctx, ns, running.ID); err != nil {
		t.Fatalf("活动任务永不删：%v", err)
	}
	if emits, err := db.ListEmits(ctx, oldID); err != nil || len(emits) != 0 {
		t.Fatalf("emits 应随任务级联删除：%v %v", emits, err)
	}
	// 0 = 永久：整段跳过。
	setRetention(t, db, 0, 0, 0)
	stats, err = db.RunRetention(ctx, now)
	if err != nil {
		t.Fatalf("run retention (permanent): %v", err)
	}
	if !stats.TasksSkipped || stats.TasksDeleted != 0 {
		t.Fatalf("TTL=0 应跳过 tasks：%+v", stats)
	}
	if _, err := db.GetTask(ctx, ns, freshID); err != nil {
		t.Fatalf("永久保留时不该删任何任务：%v", err)
	}
}

// TestRetentionEventTTL 验事件（含剪枝留痕）按 eventTtlSec 清理；0 = 永久。
func TestRetentionEventTTL(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ret-events")
	now := store.Now()

	old := evt(ns, "trace-old", "hook", "")
	old.CreatedAt = now - 1000
	if ok, err := db.InsertEvent(ctx, old, now, 0); err != nil || !ok {
		t.Fatalf("insert old: %v %v", ok, err)
	}
	fresh := evt(ns, "trace-fresh", "hook", "")
	fresh.CreatedAt = now - 10
	if ok, err := db.InsertEvent(ctx, fresh, now, 0); err != nil || !ok {
		t.Fatalf("insert fresh: %v %v", ok, err)
	}
	prune := &store.Event{NamespaceID: ns, TraceID: "trace-old", Type: pruneType}
	prune.CreatedAt = now - 1000
	if err := db.InsertPruneEvent(ctx, prune); err != nil {
		t.Fatalf("insert old prune: %v", err)
	}

	setRetention(t, db, 100, 0, 0)
	stats, err := db.RunRetention(ctx, now)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.EventsSkipped || stats.EventTTLSec != 100 {
		t.Fatalf("event TTL 应生效：%+v", stats)
	}
	if stats.EventsDeleted != 2 {
		t.Fatalf("应删 2 条（事件 + 剪枝留痕），得到 %d", stats.EventsDeleted)
	}
	if _, err := db.GetEvent(ctx, ns, old.ID); err == nil {
		t.Fatalf("过期事件应被删")
	}
	if _, err := db.GetEvent(ctx, ns, fresh.ID); err != nil {
		t.Fatalf("未过期事件不该被删：%v", err)
	}

	setRetention(t, db, 0, 0, 0)
	stats, err = db.RunRetention(ctx, now)
	if err != nil {
		t.Fatalf("run retention (permanent): %v", err)
	}
	if !stats.EventsSkipped {
		t.Fatalf("TTL=0 应跳过 events：%+v", stats)
	}
	if _, err := db.GetEvent(ctx, ns, fresh.ID); err != nil {
		t.Fatalf("永久保留时不该删事件：%v", err)
	}
}

// TestRetentionDoesNotTouchPlaybookInstalls 验**事件清理不删安装回执**（父语义）。
//
// 这是本任务卡与父契约相反的一处：回执是安装审计 + installed_from 的锚点，
// 不跟 events 的保留期一起消失。RunRetention 只清 events/tasks，回执一行都不动。
// 回执自己的清理只存在于独立 primitive（DeleteExpiredPlaybookInstalls，显式 cutoff）。
//
// 回执的写路径属于 G1-5，这里直接建行作夹具（GC 判据是本任务的范围）。
func TestRetentionDoesNotTouchPlaybookInstalls(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ret-installs")
	now := store.Now()

	pbID := "pb_fixture"
	if _, err := db.Exec(
		`INSERT INTO playbooks (id, namespace_id, slug, name, descr, version, files, source, resource_version, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		pbID, ns, "pkg", "pkg", "", 1, "{}", "", 1, now, now); err != nil {
		t.Fatalf("insert playbook fixture: %v", err)
	}
	install := func(id string, createdAt int64) {
		if _, err := db.Exec(
			`INSERT INTO playbook_installs (id, namespace_id, playbook_id, "values", values_digest, receipt, source, created_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			id, ns, pbID, "{}", "d-"+id, "{}", "", createdAt); err != nil {
			t.Fatalf("insert install %s: %v", id, err)
		}
	}
	install("pbi-referenced", now-100000)
	install("pbi-old-unreferenced", now-100000)

	// 让 pbi-referenced 被一个能力行引用（installed_from）。
	c := createCap(t, db, testCap(ns, "from-pkg", 1, nil))
	if _, err := db.Exec(`UPDATE capability_versions SET installed_from = ? WHERE id = ?`,
		"pbi-referenced", c.ID); err != nil {
		t.Fatalf("mark installed_from: %v", err)
	}

	setRetention(t, db, 100, 100, 0)
	stats, err := db.RunRetention(ctx, now)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.EventsSkipped || stats.TasksSkipped {
		t.Fatalf("两条 TTL 都应生效（夹具前提）：%+v", stats)
	}
	// 关键断言：即使过了 eventTtlSec，两条回执都还在。
	if !rowExists(t, db, "playbook_installs", "pbi-referenced") {
		t.Fatalf("被引用的回执必须保留（引用不能悬空）")
	}
	if !rowExists(t, db, "playbook_installs", "pbi-old-unreferenced") {
		t.Fatalf("事件清理不该删未引用的回执（它与事件保留期无关）")
	}

	// 删 playbook 清单不该连带删回执（历史 id，不是级联外键）。
	if _, err := db.Exec(`DELETE FROM playbooks WHERE id = ?`, pbID); err != nil {
		t.Fatalf("delete playbook: %v", err)
	}
	if !rowExists(t, db, "playbook_installs", "pbi-old-unreferenced") {
		t.Fatalf("删包清单必须保留回执（否则 installed_from 会悬空）")
	}
}

// TestDeleteExpiredPlaybookInstallsPrimitive 验独立 primitive：显式 cutoff +
// 引用安全，且**不是** no-op（真能删掉未引用且过期的行）。
func TestDeleteExpiredPlaybookInstallsPrimitive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ret-install-primitive")
	now := store.Now()

	pbID := "pb_primitive"
	if _, err := db.Exec(
		`INSERT INTO playbooks (id, namespace_id, slug, name, descr, version, files, source, resource_version, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		pbID, ns, "pkg", "pkg", "", 1, "{}", "", 1, now, now); err != nil {
		t.Fatalf("insert playbook: %v", err)
	}
	for _, id := range []string{"pbi-old-free", "pbi-old-ref", "pbi-fresh"} {
		createdAt := now - 1000
		if id == "pbi-fresh" {
			createdAt = now - 10
		}
		if _, err := db.Exec(
			`INSERT INTO playbook_installs (id, namespace_id, playbook_id, "values", values_digest, receipt, source, created_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			id, ns, pbID, "{}", "d-"+id, "{}", "", createdAt); err != nil {
			t.Fatalf("insert install %s: %v", id, err)
		}
	}
	c := createCap(t, db, testCap(ns, "referrer", 1, nil))
	if _, err := db.Exec(`UPDATE capability_versions SET installed_from = ? WHERE id = ?`,
		"pbi-old-ref", c.ID); err != nil {
		t.Fatalf("mark installed_from: %v", err)
	}

	// limit <= 0 什么都不删（少删好于删全表）。
	if n, err := db.DeleteExpiredPlaybookInstalls(ctx, now, 0); err != nil || n != 0 {
		t.Fatalf("limit<=0 应 no-op：%d %v", n, err)
	}
	n, err := db.DeleteExpiredPlaybookInstalls(ctx, now-100, 100)
	if err != nil {
		t.Fatalf("delete expired installs: %v", err)
	}
	if n != 1 {
		t.Fatalf("应只删掉唯一未被引用且过期的回执，得到 %d", n)
	}
	if rowExists(t, db, "playbook_installs", "pbi-old-free") {
		t.Fatalf("未引用且过期的回执应被删（primitive 不能是 no-op）")
	}
	if !rowExists(t, db, "playbook_installs", "pbi-old-ref") {
		t.Fatalf("被引用的回执永不删")
	}
	if !rowExists(t, db, "playbook_installs", "pbi-fresh") {
		t.Fatalf("未过期的回执不该删")
	}
}

// TestRetentionResultTTLExposedOnly 验 resultTtlSec 只作为配置暴露（store 不碰文件系统）。
func TestRetentionResultTTLExposedOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	setRetention(t, db, 0, 0, 604800)
	stats, err := db.RunRetention(ctx, store.Now())
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.ResultTTLSec != 604800 {
		t.Fatalf("resultTtlSec 应被读出：%+v", stats)
	}
	cfg, err := db.GetSystemConfig(ctx)
	if err != nil || cfg.RetentionResultTTLSec != 604800 {
		t.Fatalf("系统配置应可读：%v %+v", err, cfg)
	}
	_ = protocol.ReasonParamMissing
}

// rowExists 判断某表里是否存在该 id 行。
func rowExists(t *testing.T, db *store.DB, table, id string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("probe %s %s: %v", table, id, err)
	}
	return n > 0
}

// TestRetentionKeepsEventsReferencedByTasks 验**引用组安全**：trace 上还有任务行时，
// 即使事件已过 eventTtlSec 也不删；任务被清掉之后下一轮才清。
func TestRetentionKeepsEventsReferencedByTasks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ret-refgroup")
	now := store.Now()

	// 一条被任务引用的老事件 + 一条无人引用的老事件。
	withTask := evt(ns, "trace-live", "hook", "")
	withTask.CreatedAt = now - 1000
	if ok, err := db.InsertEvent(ctx, withTask, now, 0); err != nil || !ok {
		t.Fatalf("insert live event: %v %v", ok, err)
	}
	orphan := evt(ns, "trace-dead", "hook", "")
	orphan.CreatedAt = now - 1000
	if ok, err := db.InsertEvent(ctx, orphan, now, 0); err != nil || !ok {
		t.Fatalf("insert orphan event: %v %v", ok, err)
	}

	// 在 trace-live 上放一条 pending 任务（它自己不会过期）。
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	task := newTask(t, db, ns, c, "trace-live", "")

	setRetention(t, db, 100, 100, 0)
	if _, err := db.RunRetention(ctx, now); err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if _, err := db.GetEvent(ctx, ns, withTask.ID); err != nil {
		t.Fatalf("被 task 引用的事件必须保留：%v", err)
	}
	if _, err := db.GetEvent(ctx, ns, orphan.ID); err == nil {
		t.Fatalf("无人引用且过期的事件应被清掉")
	}

	// 任务行消失（这里直接删，模拟它自己过期）→ 下一轮事件才被清。
	if _, err := db.Exec(`DELETE FROM tasks WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("drop task: %v", err)
	}
	if _, err := db.RunRetention(ctx, now); err != nil {
		t.Fatalf("run retention (after task gone): %v", err)
	}
	if _, err := db.GetEvent(ctx, ns, withTask.ID); err == nil {
		t.Fatalf("引用消失后过期事件应被清掉")
	}
}

// TestRetentionSweepsDedupRecordsNotEntities 验去重窗与实体解耦：
// sweeper 清过期去重记录，但**不删**被 task 引用而保留的事件实体。
func TestRetentionSweepsDedupRecordsNotEntities(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ret-dedup")
	now := store.Now()

	// 窗口 100s 的投递，事件本体是「老」的（会被 TTL 判定），但 trace 上有任务。
	referenced := evt(ns, "trace-keep", "hook", "deliver-keep")
	referenced.CreatedAt = now - 1000
	if ok, err := db.InsertEvent(ctx, referenced, now-1000, 100); err != nil || !ok {
		t.Fatalf("insert referenced event: %v %v", ok, err)
	}
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	newTask(t, db, ns, c, "trace-keep", "")

	setRetention(t, db, 100, 100, 0)
	stats, err := db.RunRetention(ctx, now)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.DeliveriesDeleted != 1 {
		t.Fatalf("过期去重记录应被清（得到 %d）", stats.DeliveriesDeleted)
	}
	if stats.EventsDeleted != 0 {
		t.Fatalf("被引用的事件不该被删（得到 %d）", stats.EventsDeleted)
	}
	if _, err := db.GetEvent(ctx, ns, referenced.ID); err != nil {
		t.Fatalf("事件实体必须还在：%v", err)
	}
	// 去重记录没了 = 同 id 可以当新事件接收（与过窗语义一致），且旧实体不受影响。
	again := evt(ns, "trace-new", "hook", "deliver-keep")
	if ok, err := db.InsertEvent(ctx, again, now, 100); err != nil || !ok {
		t.Fatalf("窗口记录清掉后同 id 应可再接收：%v %v", ok, err)
	}
	if _, err := db.GetEvent(ctx, ns, referenced.ID); err != nil {
		t.Fatalf("再次接收不该删旧实体：%v", err)
	}
}

// TestDeleteExpiredPlaybookInstallsProtectsRetainedTrace 验回执的**保留 trace 引用**保护：
// 回执 id 还挂在某条 trace 上时不可删（检查与删除同一事务）。
func TestDeleteExpiredPlaybookInstallsProtectsRetainedTrace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ret-install-trace")
	now := store.Now()

	if _, err := db.Exec(
		`INSERT INTO playbooks (id, namespace_id, slug, name, descr, version, files, source, resource_version, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"pb_trace", ns, "pkg", "pkg", "", 1, "{}", "", 1, now, now); err != nil {
		t.Fatalf("insert playbook: %v", err)
	}
	for _, id := range []string{"pbi-trace-live", "pbi-trace-free"} {
		if _, err := db.Exec(
			`INSERT INTO playbook_installs
			   (id, namespace_id, namespace_slug, playbook_id, playbook_slug, "values", values_digest, receipt, source, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, ns, "ret-install-trace", "pb_trace", "pkg", "{}", "d-"+id, "{}", "", now-1000); err != nil {
			t.Fatalf("insert install %s: %v", id, err)
		}
	}
	// 让其中一条回执 id 成为一条保留 trace 的根（安装动作以回执 id 作 trace 根）。
	linked := evt(ns, "pbi-trace-live", "hook", "")
	linked.CreatedAt = now - 10
	if ok, err := db.InsertEvent(ctx, linked, now, 0); err != nil || !ok {
		t.Fatalf("insert trace event: %v %v", ok, err)
	}

	n, err := db.DeleteExpiredPlaybookInstalls(ctx, now-100, 100)
	if err != nil {
		t.Fatalf("delete expired installs: %v", err)
	}
	if n != 1 {
		t.Fatalf("应只删掉没有 trace 引用的那条，得到 %d", n)
	}
	if !rowExists(t, db, "playbook_installs", "pbi-trace-live") {
		t.Fatalf("被保留 trace 引用的回执不可删")
	}
	if rowExists(t, db, "playbook_installs", "pbi-trace-free") {
		t.Fatalf("无引用的过期回执应被删")
	}
}

// TestRetentionProtectsOldUnreapedAttemptAfterCurrentReaped discriminates
// all-history GC from latest-only GC: current attempt2 has completed and reaped,
// but attempt1 still holds its physical slot and must protect the logical task.
func TestRetentionProtectsOldUnreapedAttemptAfterCurrentReaped(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "ret-old-attempt")
	c := lifeCycleCap(t, db, ns, "idem", true, 3)
	node := newNode(t, db, "node-a", 2)
	task := newTask(t, db, ns, c, "trace-history", "")
	lease1, _, attempt1 := grant(t, db, task.ID, node.ID, "asg-old")
	old, err := db.GetTaskAttempt(ctx, task.ID, attempt1)
	if err != nil {
		t.Fatal(err)
	}
	requeueByExpiry(t, db, task.ID)
	lease2, _, attempt2 := regrantOnFreeSlot(t, db, task.ID, node.ID, "asg-current")
	if err := db.CompleteTaskAuthorized(ctx, store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: node.TokenHash}, task.ID, attempt2, lease2, 0, store.TaskResult{Phase: store.TaskSucceeded}); err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	if _, err := db.Exec(`UPDATE tasks SET result_accepted_at = ?, created_at = ? WHERE id = ?`, now-1000, now-1001, task.ID); err != nil {
		t.Fatal(err)
	}
	attempts, err := db.ListTaskAttempts(ctx, task.ID)
	if err != nil || len(attempts) != 2 || attempts[0].ReapedAt != nil || attempts[1].ReapedAt == nil {
		t.Fatalf("fixture needs old unreaped and current reaped: attempts=%+v err=%v", attempts, err)
	}
	setRetention(t, db, 0, 100, 0)
	if stats, err := db.RunRetention(ctx, now); err != nil || stats.TasksDeleted != 0 {
		t.Fatalf("old unreaped history must protect terminal/current-reaped task: stats=%+v err=%v", stats, err)
	}
	if n, err := db.DeleteExpiredTasks(ctx, now, 10); err != nil || n != 0 {
		t.Fatalf("primitive GC must check all history: deleted=%d err=%v", n, err)
	}
	if _, err := db.GetTask(ctx, ns, task.ID); err != nil {
		t.Fatalf("logical task must remain: %v", err)
	}
	attempts, err = db.ListTaskAttempts(ctx, task.ID)
	if err != nil || len(attempts) != 2 || attempts[0].ReapedAt != nil || busySlots(t, db, node.ID) != 1 {
		t.Fatalf("GC must preserve old attempt ledger/slot: attempts=%+v err=%v", attempts, err)
	}
	if reaped, err := db.ReapTaskAttempt(ctx, store.ReapedInputs{
		TaskID: task.ID, Attempt: attempt1, NodeID: node.ID, NodeTokenHash: node.TokenHash,
		LeaseID: lease1, Slot: old.Slot, InstanceID: old.InstanceID,
	}); err != nil || !reaped {
		t.Fatalf("exact old history reap: reaped=%v err=%v", reaped, err)
	}
	if stats, err := db.RunRetention(ctx, now); err != nil || stats.TasksDeleted != 1 {
		t.Fatalf("only after old reap can entire task expire: stats=%+v err=%v", stats, err)
	}
	if attempts, err := db.ListTaskAttempts(ctx, task.ID); err != nil || len(attempts) != 0 || busySlots(t, db, node.ID) != 0 {
		t.Fatalf("reaped task GC must cascade attempts: attempts=%+v err=%v", attempts, err)
	}
}
