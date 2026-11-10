package store

// retention_test.go 覆盖 v4 保留期（config.md §3.6）：
//
//	三档 TTL 各自独立，0 = 永久（「跳过」与「扫了没东西删」必须可区分）；
//	活动任务永不删；终态任务按**可信观察时间**（result_accepted_at，或未上报时的
//	finished_at）过期；只要还有未 reaped 的 attempt，任务行就不许先于执行事实消失；
//	事件按引用组安全清理（同 trace 还有任务行就不删）；回执不在自动 sweeper 里。
//
// v3 的 SweepRetention/RetentionOptions/terminalStatusList 已删除：v4 只有
// RunRetention + 四个显式 primitive，配置从系统配置读。

import (
	"context"
	"testing"
)

// seedTerminalTask 造一条「服务器裁定的终态、未上报」任务（reported=0 + finished_at）。
//
// 直插是刻意的：CreateTasks 只能落 pending，这里要的是历史终态行这种种子；
// 正常路径（grant → complete）会留下未 reaped 的 attempt，按 v4 规则**不许删**，
// 那条语义由 TestRetentionKeepsUnreapedAttempts 单独验。
func seedTerminalTask(t *testing.T, db *DB, nsID string, c *CapabilityVersion, traceID, phase string, finishedAt int64) *Task {
	t.Helper()
	task := mkTask(nsID, traceID, c)
	if err := db.CreateTasks(context.Background(), []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	pushToPhase(t, db, task.ID, phase)
	mustExec(t, db, `UPDATE tasks SET finished_at=?, reported=0, result_accepted_at=NULL WHERE id=?`, finishedAt, task.ID)
	return task
}

// TestRetentionTTLZeroIsPermanent TTL=0 = 永久：两阶段都跳过且计数为 0。
func TestRetentionTTLZeroIsPermanent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "ret-perm", 1)
	v4SetRetention(t, db, 0, 0, 0)
	old := v4InsertEvent(t, db, v4Event(nsID, "github", "push"), 1000, 0)
	seedTerminalTask(t, db, nsID, c, "tr_perm", TaskSucceeded, 1000)

	stats, err := db.RunRetention(ctx, 10_000_000)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if !stats.EventsSkipped || !stats.TasksSkipped {
		t.Fatalf("TTL=0 应跳过两阶段: %+v", stats)
	}
	if stats.EventsDeleted != 0 || stats.TasksDeleted != 0 {
		t.Fatalf("TTL=0 删了行: %+v", stats)
	}
	if _, err := db.GetEvent(ctx, nsID, old.ID); err != nil {
		t.Fatalf("永久保留的事件被删: %v", err)
	}
}

// TestRetentionDeletesExpiredEvents 过期事件删、未过期留；事件与任务 TTL 独立。
func TestRetentionDeletesExpiredEvents(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	v4SetRetention(t, db, 100, 0, 0)
	old := v4InsertEvent(t, db, v4Event(nsID, "github", "push"), 1_000, 0)
	fresh := v4InsertEvent(t, db, v4Event(nsID, "github", "push"), 1_950, 0)

	stats, err := db.RunRetention(ctx, 2_000)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.EventsDeleted != 1 {
		t.Fatalf("EventsDeleted = %d, want 1", stats.EventsDeleted)
	}
	if !stats.TasksSkipped {
		t.Fatalf("任务 TTL=0 应跳过: %+v", stats)
	}
	if _, err := db.GetEvent(ctx, nsID, old.ID); err == nil {
		t.Fatal("过期事件没删")
	}
	if _, err := db.GetEvent(ctx, nsID, fresh.ID); err != nil {
		t.Fatalf("未过期事件被删: %v", err)
	}
}

// TestRetentionKeepsEventsReferencedByTasks 同 trace 还有任务行 → 事件不删（引用组安全）。
func TestRetentionKeepsEventsReferencedByTasks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "ret-ref", 1)
	v4SetRetention(t, db, 100, 0, 0)
	e := v4InsertEvent(t, db, v4Event(nsID, "github", "push"), 1_000, 0)
	task := mkTask(nsID, e.TraceID, c)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := db.RunRetention(ctx, 2_000); err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if _, err := db.GetEvent(ctx, nsID, e.ID); err != nil {
		t.Fatalf("被任务引用的事件被删: %v", err)
	}
	mustExec(t, db, `DELETE FROM tasks WHERE id=?`, task.ID)
	if _, err := db.RunRetention(ctx, 2_000); err != nil {
		t.Fatalf("second retention: %v", err)
	}
	if _, err := db.GetEvent(ctx, nsID, e.ID); err == nil {
		t.Fatal("引用消失后事件仍被保护")
	}
}

// TestRetentionKeepsActiveTasks pending/running 永不删（没有「过期」可言）。
func TestRetentionKeepsActiveTasks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "ret-active", 1)
	v4SetRetention(t, db, 0, 100, 0)

	pending := mkTask(nsID, "tr_pending", c)
	if err := db.CreateTasks(ctx, []*Task{pending}); err != nil {
		t.Fatalf("create pending: %v", err)
	}
	running := mkTask(nsID, "tr_running", c)
	if err := db.CreateTasks(ctx, []*Task{running}); err != nil {
		t.Fatalf("create running: %v", err)
	}
	pushToPhase(t, db, running.ID, TaskRunning)
	mustExec(t, db, `UPDATE tasks SET finished_at=1 WHERE id IN (?,?)`, pending.ID, running.ID)

	stats, err := db.RunRetention(ctx, 10_000_000)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.TasksDeleted != 0 {
		t.Fatalf("活动任务被删了 %d 行", stats.TasksDeleted)
	}
	for _, id := range []string{pending.ID, running.ID} {
		if _, err := db.GetTask(ctx, nsID, id); err != nil {
			t.Fatalf("活动任务 %s 被删: %v", id, err)
		}
	}
}

// TestRetentionDeletesExpiredTerminalTasks 过期终态（可信 finished_at）删、未过期留。
func TestRetentionDeletesExpiredTerminalTasks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "ret-term", 1)
	v4SetRetention(t, db, 0, 100, 0)
	old := seedTerminalTask(t, db, nsID, c, "tr_old", TaskSucceeded, 1_000)
	fresh := seedTerminalTask(t, db, nsID, c, "tr_fresh", TaskFailed, 1_950)

	stats, err := db.RunRetention(ctx, 2_000)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.TasksDeleted != 1 {
		t.Fatalf("TasksDeleted = %d, want 1", stats.TasksDeleted)
	}
	if _, err := db.GetTask(ctx, nsID, old.ID); err == nil {
		t.Fatal("过期终态任务没删")
	}
	if _, err := db.GetTask(ctx, nsID, fresh.ID); err != nil {
		t.Fatalf("未过期终态任务被删: %v", err)
	}
}

// TestRetentionKeepsUnreapedAttempts 还有未 reaped 的 attempt → 任务行不许先消失；
// reap 之后下一轮才允许删（物理事实保留）。
func TestRetentionKeepsUnreapedAttempts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "ret-attempt", 1)
	node := v4Node(t, db, "ret-node", 1)
	v4SetRetention(t, db, 0, 1, 0)

	task := v4Task(t, db, nsID, c, "tr_attempt_ret", "")
	leaseID, _, attempt := v4Grant(t, db, task.ID, node.ID, "asg-ret")
	exit := 0
	if disp := v4Complete(t, db, node.ID, task.ID, leaseID, attempt, TaskResult{
		Phase: TaskSucceeded, Artifacts: nil, ExitCode: &exit,
	}); disp != TaskResultRecorded {
		t.Fatalf("complete: %q", disp)
	}
	stats, err := db.RunRetention(ctx, 10_000_000)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.TasksDeleted != 0 {
		t.Fatalf("未 reaped 的任务被删了 %d 行", stats.TasksDeleted)
	}
	if _, err := db.GetTask(ctx, nsID, task.ID); err != nil {
		t.Fatalf("任务行被删: %v", err)
	}
	in := v4ReapInput(t, db, node.ID, task.ID, attempt)
	in.Now = Now()
	if err := db.AckCancelAuthorized(ctx, v4Auth(t, db, node.ID), in, true); err != nil {
		t.Fatalf("ack reap: %v", err)
	}
	stats, err = db.RunRetention(ctx, 10_000_000)
	if err != nil {
		t.Fatalf("run retention after reap: %v", err)
	}
	if stats.TasksDeleted != 1 {
		t.Fatalf("reap 之后应删除 1 行，得到 %+v", stats)
	}
}

// TestRetentionSweepsDedupRecordsNotEntities 去重窗口过期只清窗口记录，不动事件实体。
func TestRetentionSweepsDedupRecordsNotEntities(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	v4SetRetention(t, db, 1000, 0, 0)

	delivery := "dlv-sweep"
	e := v4Event(nsID, "webhook", "in")
	e.DeliveryID = &delivery
	if ok, err := db.InsertEvent(ctx, e, 1_000, 10); err != nil || !ok {
		t.Fatalf("insert event: %v %v", ok, err)
	}
	stats, err := db.RunRetention(ctx, 2_000)
	if err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if stats.DeliveriesDeleted != 1 {
		t.Fatalf("DeliveriesDeleted = %d, want 1（过期窗口该清）", stats.DeliveriesDeleted)
	}
	if stats.EventsDeleted != 0 {
		t.Fatalf("事件实体不该被删: %+v", stats)
	}
	if _, err := db.GetEvent(ctx, nsID, e.ID); err != nil {
		t.Fatalf("事件实体被删: %v", err)
	}
}

// TestRetentionDoesNotTouchPlaybookInstalls 自动 sweeper 一行回执都不删。
func TestRetentionDoesNotTouchPlaybookInstalls(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	v4SetRetention(t, db, 1, 1, 0)
	before := countRows(t, db, "playbook_installs")
	if _, err := db.RunRetention(ctx, 10_000_000); err != nil {
		t.Fatalf("run retention: %v", err)
	}
	if after := countRows(t, db, "playbook_installs"); after != before {
		t.Fatalf("回执被自动清理: %d → %d 行", before, after)
	}
}

// countRows 数一张表的行数（保留期夹具用）。
func countRows(t *testing.T, db *DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestRetentionPrimitiveLimits limit<=0 返回 0 行而不删（「少删」优于「删全表」）。
func TestRetentionPrimitiveLimits(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if n, err := db.DeleteExpiredEvents(ctx, 10_000_000, 0); err != nil || n != 0 {
		t.Fatalf("DeleteExpiredEvents(limit=0) = %d, %v", n, err)
	}
	if n, err := db.DeleteExpiredTasks(ctx, 10_000_000, 0); err != nil || n != 0 {
		t.Fatalf("DeleteExpiredTasks(limit=0) = %d, %v", n, err)
	}
	if n, err := db.DeleteExpiredEventDeliveries(ctx, 10_000_000, -1); err != nil || n != 0 {
		t.Fatalf("DeleteExpiredEventDeliveries(limit=-1) = %d, %v", n, err)
	}
}
