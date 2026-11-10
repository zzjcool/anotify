package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是 #228 背压信号的数据层门禁：
//   ① 空集 → 0（不是 ErrNoRows）　② 只数 pending　③ 退避中仍算积压
//   ④ 信号查询的计划必须走部分索引 idx_tasks_pending_created_at（O(log n)）
//   ⑤ 索引在 baseline 里形状正确（部分索引 + 单列 created_at），重开库幂等

// pendingBacklogIndex 是 baseline 里的背压索引名（计划门禁与形状断言共用）。
const pendingBacklogIndex = "idx_tasks_pending_created_at"

// oldestPendingProbeSQL 是背压信号查询的**探针**：与 OldestPendingCreatedAt 里的字面 SQL
// 逐字一致（脱钩由 TestPendingBacklogSQLShape 兜住）。
const oldestPendingProbeSQL = `SELECT MIN(created_at) FROM tasks WHERE phase = ?`

// TestOldestPendingCreatedAtEmpty：一条 pending 都没有 → 0, nil。
//
// ErrNoRows 是这条用例的**反例**：MIN() 在空集上返回**一行 NULL**（不是零行），
// 所以实现必须用 sql.NullInt64 扫、NULL → 0。写成 QueryRow + int64 直扫会拿到
// "converting NULL to int64 is unsupported"——背压采样会把一次正常的「没积压」
// 当成采样失败，保护逻辑就对积压彻底瞎了。
func TestOldestPendingCreatedAtEmpty(t *testing.T) {
	db := newTestDB(t)
	got, err := db.OldestPendingCreatedAt(context.Background())
	if err != nil {
		t.Fatalf("空库 OldestPendingCreatedAt 报错（空集不是错误）: %v", err)
	}
	if got != 0 {
		t.Fatalf("空库 OldestPendingCreatedAt = %d, want 0", got)
	}
}

// TestOldestPendingCreatedAtIgnoresNonPending：只有 status='pending' 行参与 MIN()。
//
// 数据（created_at）：pending 100/200/300 + succeeded 50 + lease_expired 10。
// 终态行与非 pending 行都不构成积压（契约 §1.1）。succeeded 行特意比所有 pending 都早：
// 它一旦漏进 MIN()，返回值会从 100 变成 50，这条断言就立刻红。
func TestOldestPendingCreatedAtIgnoresNonPending(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "backlog-probe", 1, nil)
	if err := db.CreateCapability(ctx, withSpec(cap)); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	mk := func(trace string, createdAt int64) *Task {
		task := mkTask(nsID, trace, cap)
		task.CreatedAt = createdAt
		return task
	}
	pending := []*Task{
		mk("trace_p100", 100),
		mk("trace_p200", 200),
		mk("trace_p300", 300),
	}
	// 终态行：created_at=50 比所有 pending 都早，但只要不是 pending 就不算积压。
	done := mk("trace_succeeded", 50)
	// running 是非 pending 的非终态行（v3 的 lease_expired 不是 v4 相位：派发步骤
	// 不是状态，超时重派表现为 attempt/租约事实）。它同样不算积压。
	running := mk("trace_running", 10)

	if err := db.CreateTasks(ctx, append(append([]*Task{}, pending...), done, running)); err != nil {
		t.Fatalf("create tasks: %v", err)
	}
	pushToPhase(t, db, done.ID, TaskSucceeded)
	pushToPhase(t, db, running.ID, TaskRunning)

	got, err := db.OldestPendingCreatedAt(ctx)
	if err != nil {
		t.Fatalf("OldestPendingCreatedAt: %v", err)
	}
	if got != 100 {
		t.Fatalf("OldestPendingCreatedAt = %d, want 100（终态/非 pending 行不参与）", got)
	}
}

// TestOldestPendingCreatedAtIgnoresNotBefore：退避中的任务**仍然算积压**（契约 §5.3）。
//
// SetNotBefore 是「重派退避」，写它的正是「派不出去」那条路径（mux.go 的 Redispatch
// 在 ErrNoCapacity 时写 not_before）。若这条查询顺手加上 `AND not_before <= now`，
// #228 的实测场景（积压 8 行/秒）会被整片漏掉——积压里最老的那批恰好都在退避中。
func TestOldestPendingCreatedAtIgnoresNotBefore(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "backlog-notbefore", 1, nil)
	if err := db.CreateCapability(ctx, withSpec(cap)); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	backedOff := mkTask(nsID, "trace_backed_off", cap)
	backedOff.CreatedAt = 100
	if err := db.CreateTasks(ctx, []*Task{backedOff}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 走真实退避路径（assign → slot_unavailable reject）：它是「积压」的证据，
	// 不是「不在等待」的证据。退避写的是 next_attempt_at + waiting_reason。
	// assign 要求节点行真实存在且 online（v4：派发候选只来自真实节点）。
	node := v4Node(t, db, "node_backlog", 1)
	if err := db.AssignTask(ctx, backedOff.ID, node.ID, "asg_backlog", 1); err != nil {
		t.Fatalf("assign backed-off task: %v", err)
	}
	if err := db.RejectAssignment(ctx, backedOff.ID, "asg_backlog", TaskRejectSlotUnavailable, Now()); err != nil {
		t.Fatalf("reject assignment (slot unavailable): %v", err)
	}
	waiting, err := db.GetTask(ctx, nsID, backedOff.ID)
	if err != nil {
		t.Fatalf("get backed-off task: %v", err)
	}
	if waiting.NextAttemptAt == nil || waiting.WaitingReason != WaitingForSlot {
		t.Fatalf("退避事实未落库: waiting=%q next=%v", waiting.WaitingReason, waiting.NextAttemptAt)
	}
	stillWaiting := mkTask(nsID, "trace_still_waiting", cap)
	stillWaiting.CreatedAt = 200
	if err := db.CreateTasks(ctx, []*Task{stillWaiting}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := db.OldestPendingCreatedAt(ctx)
	if err != nil {
		t.Fatalf("OldestPendingCreatedAt: %v", err)
	}
	if got != 100 {
		t.Fatalf("OldestPendingCreatedAt = %d, want 100（not_before 是退避，不是「不在等待」）", got)
	}
}

// TestPendingBacklogSQLShape 是 EXPLAIN 用例的护栏：探针 SQL 是照抄的，
// 一旦 OldestPendingCreatedAt 的查询形状变了而探针没跟上，下面的计划门禁
// 就变成「在测一条没人跑的 SQL」——计划很漂亮，线上照样贵。这条把脱钩变成红灯。
func TestPendingBacklogSQLShape(t *testing.T) {
	src := strings.Join(strings.Fields(readSource(t, "task.go")), " ")
	frag := strings.Join(strings.Fields(oldestPendingProbeSQL), " ")
	if !strings.Contains(src, frag) {
		t.Errorf("task.go 里找不到背压信号 SQL %q：查询形状变了？"+
			"请同步更新 pending_backlog_test.go 的 oldestPendingProbeSQL"+
			"（否则 EXPLAIN 门禁在测一条没人跑的 SQL）", frag)
	}
}

// TestPendingBacklogIndexQueryPlan 是 #228 的**计划门禁**：信号必须是
// 「部分索引最左端 seek」（O(log n)），而不是「按 status 取出全部 pending 再逐行回表」
// （O(积压量)——正好在最需要它的时候最贵）。
//
// 统计从哪来：这条用例**自己不跑 ANALYZE**，统计由产品路径（Open 末尾的
// `PRAGMA optimize=0x2`）补出来。
// 为什么必须重开一次才能断言：空表上 ANALYZE 一行 stat1 都不写，而 SQLite 的 min/max
// 优化要看到该索引自己的 stat1 条目才会选它——在此之前
// 规划器选的是 `SEARCH tasks USING INDEX idx_tasks_lease (status=?)`（逐行回表，O(pending)）。
// 实测两种状态都在下面打出来，让「统计是解锁条件」这件事有据可查，而不是一句断言。
func TestPendingBacklogIndexQueryPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh_pending.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	seedPendingBacklog(t, first, 300)
	plan := func(db *DB, tag string) string {
		t.Helper()
		p := strings.Join(explainQueryPlan(t, db, oldestPendingProbeSQL, TaskPending), "\n")
		t.Logf("EXPLAIN QUERY PLAN（%s）:\n%s", tag, p)
		return p
	}
	// 对照证据：此刻 tasks 还没有任何 stat1 条目（空表上 ANALYZE 写不出东西）。
	t.Logf("重开前 sqlite_stat1 里 tasks 的条目: %v", pendingStat1Entries(t, first))
	plan(first, "新建库灌满数据、尚未重开（无统计）")
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()
	if entries := pendingStat1Entries(t, second); len(entries) == 0 {
		t.Errorf("重开后 sqlite_stat1 里没有 tasks 的统计：Open 末尾的 optimize 没生效，" +
			"下面的计划断言也就失去意义（规划器仍是盲的）")
	}
	got := plan(second, "重开之后（Open 的 PRAGMA optimize 补出统计）")
	if !strings.Contains(got, pendingBacklogIndex) {
		t.Errorf("背压信号的计划里没有 %s：MIN(created_at) 退化成了"+
			"「按 status 取出全部 pending 再逐行回表」，成本随积压行数线性增长——"+
			"正是 #228 要消掉的那条昂贵计划:\n%s", pendingBacklogIndex, got)
	}
}

// TestPendingBacklogIndexIdempotent：索引在重开库后形状不变（幂等）+ 形状正确。
//
// 形状断言为什么重要：一条**非部分**的 `(created_at)` 索引也能让 MIN() 走索引，
// 但它把终态行也收进索引（体积随全部历史任务增长，与「只覆盖积压」的设计意图相反）。
// 所以除「索引存在且只有一条」之外，还要钉住 partial=1 与列序 = [created_at]。
func TestPendingBacklogIndexIdempotent(t *testing.T) {
	db, path := newTestDBFile(t)
	assertPendingBacklogIndexShape(t, db, "首次 Open")
	before, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("version before: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(path) // 同一个库再开一次：不报错、不重复建索引
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	after, err := db2.SchemaVersion()
	if err != nil {
		t.Fatalf("version after: %v", err)
	}
	if before != after {
		t.Fatalf("schema version changed on reopen: %d → %d", before, after)
	}
	assertPendingBacklogIndexShape(t, db2, "重开之后")

	var dup int
	if err := db2.QueryRow(`SELECT count(*) - count(DISTINCT version) FROM schema_version`).Scan(&dup); err != nil {
		t.Fatalf("count duplicates: %v", err)
	}
	if dup != 0 {
		t.Fatalf("schema_version 里有 %d 条重复记账", dup)
	}
}

// assertPendingBacklogIndexShape 断言索引存在且**只有一条**、是被部分索引、列序 = created_at。
func assertPendingBacklogIndexShape(t *testing.T, db *DB, tag string) {
	t.Helper()
	if n := countIndexRows(t, db, "tasks", pendingBacklogIndex); n != 1 {
		t.Fatalf("[%s] 表 tasks 上名为 %s 的索引有 %d 条, want 1", tag, pendingBacklogIndex, n)
	}
	var partial int
	if err := db.QueryRow(
		`SELECT partial FROM pragma_index_list('tasks') WHERE name = ?`,
		pendingBacklogIndex).Scan(&partial); err != nil {
		t.Fatalf("[%s] read pragma_index_list(%s): %v", tag, pendingBacklogIndex, err)
	}
	if partial != 1 {
		t.Fatalf("[%s] %s 的 partial = %d, want 1（不是部分索引 → 终态行也会进索引）",
			tag, pendingBacklogIndex, partial)
	}
	cols := indexColumnsOn(t, db, "tasks", pendingBacklogIndex)
	if len(cols) != 1 || cols[0] != "created_at" {
		t.Fatalf("[%s] %s 的列序 = %v, want [created_at]（首列必须是 MIN() 的目标列）",
			tag, pendingBacklogIndex, cols)
	}
	// 谓词也要钉住：列序对但谓词写成 status <> 'succeeded' 之类的形状，
	// MIN() 的结果就不再是「pending 的最早等待时刻」。
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`,
		pendingBacklogIndex).Scan(&ddl); err != nil {
		t.Fatalf("[%s] read ddl of %s: %v", tag, pendingBacklogIndex, err)
	}
	if flat := strings.Join(strings.Fields(ddl), " "); !strings.Contains(flat, "WHERE phase = 'pending'") {
		t.Fatalf("[%s] %s 的谓词不是 `WHERE phase = 'pending'`:\n%s", tag, pendingBacklogIndex, flat)
	}
}

// seedPendingBacklog 造一段积压：rows 条 pending（created_at 递增）+ 一条更早的终态行。
//
// 终态行是刻意的：它证明「索引只覆盖 pending 行」这件事在计划层面成立
// （若索引退化成全量 (created_at) 索引，succeeded 行也会被收进去）。
//
// 为什么**直插 SQL** 而不是 CreateTasks：本文件关心的是**查询计划**，不是写入路径，
// 直插能精确控制 created_at 与 status（CreateTasks 会写过半列、还要照顾外键父行）。
// 列清单只写计划需要的列，其余（digest / token / not_before…）走列默认值。
func seedPendingBacklog(t *testing.T, db *DB, rows int) {
	t.Helper()
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "backlog-seed", 1, nil)
	if err := db.CreateCapability(ctx, withSpec(cap)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	// insertPending 直插一行 pending（只写计划需要的列）。
	insertPending := func(task *Task) {
		t.Helper()
		mustExec(t, db, `INSERT INTO tasks
			(id, namespace_id, trace_id, capability_id, capability, version, digest, params,
			 phase, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?, ?)`,
			task.ID, task.NamespaceID, task.TraceID, task.CapabilityID, task.Capability,
			task.Version, task.Digest, TaskPending, task.CreatedAt)
	}
	for i := 0; i < rows; i++ {
		task := mkTask(nsID, "trace_seed", cap)
		task.ID = NewTaskID()
		task.CreatedAt = int64(1700000000 + i)
		insertPending(task)
	}
	done := mkTask(nsID, "trace_seed_done", cap)
	done.ID = NewTaskID()
	done.CreatedAt = 1699999999 // 比所有 pending 都早
	insertPending(done)
	pushToPhase(t, db, done.ID, TaskSucceeded)
}

// pendingStat1Entries 返回 sqlite_stat1 里 tasks 的条目（"索引 = stat" 形式），仅供日志/前置断言。
//
// sqlite_stat1 在「从没跑过 ANALYZE」时**整张表都不存在**，所以这里把
// "no such table" 当成空集而不是失败。
func pendingStat1Entries(t *testing.T, db *DB) []string {
	t.Helper()
	var exists int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_stat1'`).Scan(&exists); err != nil {
		t.Fatalf("probe sqlite_stat1: %v", err)
	}
	if exists == 0 {
		return nil
	}
	rows, err := db.Query(`SELECT idx, stat FROM sqlite_stat1 WHERE tbl = 'tasks'`)
	if err != nil {
		t.Fatalf("read sqlite_stat1: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var idx, stat string
		if err := rows.Scan(&idx, &stat); err != nil {
			t.Fatalf("scan sqlite_stat1: %v", err)
		}
		out = append(out, idx+" = "+stat)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_stat1: %v", err)
	}
	return out
}
