package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"
)

// #143 保留策略（store 层）的验收测试，逐条对应契约 §5 的第 1~10 条，
// 以及返工（review-1）追加的 15~17 条。
//
// 原则：
//   - 造数据走真实路径（CreateCapability / CreateTasks / Transition），
//     只有 finished_at / created_at 这种**时间戳**允许直接 UPDATE 夹具值
//     （测试必须让它们落在过去，而真实时钟只会往前走）。
//   - 每个断言都双向校验：SweepRetention 返回的 report **和** 表里实际行数。
//     report 是「代码以为自己删了多少」，count 是「库里的事实」，两者不一致就是 bug。

// ---------- 夹具 ----------

// retentionTestEventSec 是「**显式开启** events 清理」的用例用的保留期（30 天）。
//
// 为什么不直接用 DefaultEventRetentionSec：store 的默认值已经是**禁用**
// （DefaultEventRetentionSec == EventRetentionDisabled == 0）。「默认会删 events」
// 这条语义不存在了——凡是要验证 events 真被清掉的用例，都必须自己把保留期传进来，
// 否则它就是一条 forever 不会执行的路径（默认禁用的用例见
// TestRetentionEventsDisabledByDefault）。
const retentionTestEventSec = 30 * 24 * 60 * 60 // 2592000 秒 = 30 天

// retentionTestDB 建库 + 一个组织。
func retentionTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	db := newTestDB(t)
	orgID, _ := newTestOrg(t, db)
	return db, orgID
}

// mkRetentionCap 建一个声明了 lifecycle.resultTtlSec 的能力版本。
//
// 每个测试用不同的 name：同 name 同 version 换内容会被 append-only 规则拒绝（I8）。
func mkRetentionCap(t *testing.T, db *DB, orgID, name string, ttlSec int) *CapabilityVersion {
	t.Helper()
	c := mkCap(orgID, name, 1, nil)
	c.Lifecycle = Lifecycle{ResultTTLSec: ttlSec}
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability %s (ttl=%d): %v", name, ttlSec, err)
	}
	return c
}

// mkTerminalTask 用**真实状态机**把任务推到终态，再把 finished_at 改到指定时刻。
//
// 状态只能经 Transition 推进（契约 §3 唯一写入点）；finished_at 是时间戳夹具，
// 测试需要它落在过去才能验证「过期」。
func mkTerminalTask(t *testing.T, db *DB, orgID string, cap *CapabilityVersion, status string, finishedAt int64) *Task {
	t.Helper()
	if !IsTerminalStatus(status) {
		t.Fatalf("mkTerminalTask: %q 不是终态", status)
	}
	ctx := context.Background()
	task := mkTask(orgID, "trace_"+NewTaskID(), cap)
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	pushToStatus(t, db, orgID, task, status)
	mustExec(t, db, `UPDATE tasks SET finished_at = ? WHERE org_id = ? AND id = ?`,
		finishedAt, orgID, task.ID)
	return task
}

// mkEventAt 在指定时刻写一条事件。
func mkEventAt(t *testing.T, db *DB, orgID, source, typ string, createdAt int64) *Event {
	t.Helper()
	e := &Event{
		OrgID:     orgID,
		TraceID:   "trc_" + NewEventID(),
		Source:    source,
		Type:      typ,
		CreatedAt: createdAt,
	}
	inserted, err := db.InsertEvent(context.Background(), e)
	if err != nil {
		t.Fatalf("insert event (%s/%s): %v", source, typ, err)
	}
	if !inserted {
		t.Fatalf("insert event (%s/%s) reported inserted=false", source, typ)
	}
	return e
}

// countRows 直接数表里的行（与 report 双向校验）。
func countRows(t *testing.T, db *DB, table, where string, args ...any) int {
	t.Helper()
	q := "SELECT count(*) FROM " + table
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count rows in %s (%s): %v", table, where, err)
	}
	return n
}

// mustTaskPresent 断言任务行还在。
func mustTaskPresent(t *testing.T, db *DB, orgID, taskID string) {
	t.Helper()
	if _, err := db.GetTask(context.Background(), orgID, taskID); err != nil {
		t.Fatalf("task %s 应该保留，但 GetTask 失败: %v", taskID, err)
	}
}

// mustTaskGone 断言任务行已消失（ErrNotFound，而不是别的错误）。
func mustTaskGone(t *testing.T, db *DB, orgID, taskID string) {
	t.Helper()
	_, err := db.GetTask(context.Background(), orgID, taskID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("task %s 应该被删，GetTask err = %v, want ErrNotFound", taskID, err)
	}
}

// mustEventPresent 断言事件行还在。
func mustEventPresent(t *testing.T, db *DB, orgID, eventID string) {
	t.Helper()
	if _, err := db.GetEvent(context.Background(), orgID, eventID); err != nil {
		t.Fatalf("event %s 应该保留，但 GetEvent 失败: %v", eventID, err)
	}
}

// mustEventGone 断言事件行已消失。
func mustEventGone(t *testing.T, db *DB, orgID, eventID string) {
	t.Helper()
	_, err := db.GetEvent(context.Background(), orgID, eventID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("event %s 应该被删，GetEvent err = %v, want ErrNotFound", eventID, err)
	}
}

// ---------- §5-1：过期被删 ----------

func TestRetentionDeletesExpiredTerminalTask(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	cap := mkRetentionCap(t, db, orgID, "review", 60)

	expired := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-120)
	// finished_at 恰好等于 cutoff（now-60）：用**严格小于**，边界行必须保留——
	// 否则「刚好到点」的任务会在两轮之间反复消失，语义也没法解释。
	boundary := mkTerminalTask(t, db, orgID, cap, TaskFailed, now-60)

	scopes, err := db.ListResultRetentionScopes(ctx)
	if err != nil {
		t.Fatalf("list scopes: %v", err)
	}
	if len(scopes) != 1 {
		t.Fatalf("scopes = %+v, want 1 个 scope", scopes)
	}
	got := scopes[0]
	if got.OrgID != orgID || got.CapabilityID != cap.ID || got.ResultTTLSec != 60 {
		t.Fatalf("scope = %+v, want {org:%s cap:%s ttl:60}", got, orgID, cap.ID)
	}

	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.TasksDeleted != 1 {
		t.Fatalf("TasksDeleted = %d, want 1", rep.TasksDeleted)
	}
	if rep.TaskScopes != 1 {
		t.Fatalf("TaskScopes = %d, want 1", rep.TaskScopes)
	}
	if rep.Truncated {
		t.Fatal("Truncated = true, want false（1 条远没到批上限）")
	}

	mustTaskGone(t, db, orgID, expired.ID)
	mustTaskPresent(t, db, orgID, boundary.ID)
	if n := countRows(t, db, "tasks", "org_id = ?", orgID); n != 1 {
		t.Fatalf("tasks 剩 %d 行, want 1", n)
	}
}

// ---------- §5-2：未过期保留 ----------

func TestRetentionKeepsNotYetExpiredTask(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	cap := mkRetentionCap(t, db, orgID, "review", 600)

	// 300s 前完成，TTL 600s → cutoff = now-600，不该删
	fresh := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-300)

	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.TasksDeleted != 0 {
		t.Fatalf("TasksDeleted = %d, want 0（还在保留期内）", rep.TasksDeleted)
	}
	if rep.TaskScopes != 1 {
		t.Fatalf("TaskScopes = %d, want 1（有终态任务就该有 scope）", rep.TaskScopes)
	}
	mustTaskPresent(t, db, orgID, fresh.ID)
	if n := countRows(t, db, "tasks", "org_id = ?", orgID); n != 1 {
		t.Fatalf("tasks 剩 %d 行, want 1", n)
	}
}

// ---------- §5-3：幂等 / 可重入 ----------

func TestRetentionIsIdempotent(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	cap := mkRetentionCap(t, db, orgID, "review", 60)

	for i := 0; i < 3; i++ {
		mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-120-int64(i))
	}

	first, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep #1: %v", err)
	}
	if first.TasksDeleted != 3 {
		t.Fatalf("第 1 轮 TasksDeleted = %d, want 3", first.TasksDeleted)
	}
	if n := countRows(t, db, "tasks", "org_id = ?", orgID); n != 0 {
		t.Fatalf("第 1 轮后 tasks 剩 %d 行, want 0", n)
	}

	// 第 2、3 轮：没有可删的了 → 0，且已删行数不变（不会「越跑越多」也不会复活）
	for i := 2; i <= 3; i++ {
		rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
		if err != nil {
			t.Fatalf("sweep #%d: %v", i, err)
		}
		if rep.TasksDeleted != 0 {
			t.Fatalf("第 %d 轮 TasksDeleted = %d, want 0（幂等）", i, rep.TasksDeleted)
		}
		if rep.Truncated {
			t.Fatalf("第 %d 轮 Truncated = true, want false", i)
		}
		if n := countRows(t, db, "tasks", "org_id = ?", orgID); n != 0 {
			t.Fatalf("第 %d 轮后 tasks 剩 %d 行, want 0", i, n)
		}
	}
}

// ---------- §5-4：非终态保留 ----------

func TestRetentionKeepsNonTerminalTasks(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	cap := mkRetentionCap(t, db, orgID, "review", 60)

	pending := mkTask(orgID, "trace_pending", cap)
	if err := db.CreateTasks(ctx, []*Task{pending}); err != nil {
		t.Fatalf("create pending: %v", err)
	}
	running := mkTask(orgID, "trace_running", cap)
	if err := db.CreateTasks(ctx, []*Task{running}); err != nil {
		t.Fatalf("create running: %v", err)
	}
	pushToStatus(t, db, orgID, running, TaskRunning)

	// 故意制造脏数据：非终态行也带一个很老的 finished_at。
	// 清理只认「终态」这一维，脏 finished_at 不能成为删除依据——
	// 删掉在跑的任务等于把工作静默丢掉。
	old := now - 999999
	for _, id := range []string{pending.ID, running.ID} {
		mustExec(t, db, `UPDATE tasks SET finished_at = ? WHERE org_id = ? AND id = ?`, old, orgID, id)
	}

	scopes, err := db.ListResultRetentionScopes(ctx)
	if err != nil {
		t.Fatalf("list scopes: %v", err)
	}
	if len(scopes) != 0 {
		t.Fatalf("scopes = %+v, want 0（没有终态任务）", scopes)
	}

	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.TasksDeleted != 0 {
		t.Fatalf("TasksDeleted = %d, want 0（非终态绝不删）", rep.TasksDeleted)
	}
	if rep.TaskScopes != 0 {
		t.Fatalf("TaskScopes = %d, want 0", rep.TaskScopes)
	}
	mustTaskPresent(t, db, orgID, pending.ID)
	mustTaskPresent(t, db, orgID, running.ID)
	if n := countRows(t, db, "tasks", "org_id = ?", orgID); n != 2 {
		t.Fatalf("tasks 剩 %d 行, want 2", n)
	}
}

// ---------- §5-5：未声明 TTL 的能力不清理 ----------

func TestRetentionSkipsCapabilitiesWithoutTTL(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	// resultTtlSec 未声明（0）= 永不清理
	cap := mkRetentionCap(t, db, orgID, "review", 0)

	task := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-999999)

	scopes, err := db.ListResultRetentionScopes(ctx)
	if err != nil {
		t.Fatalf("list scopes: %v", err)
	}
	if scopes == nil {
		t.Fatal("ListResultRetentionScopes 返回 nil，需要 []")
	}
	if len(scopes) != 0 {
		t.Fatalf("scopes = %+v, want 0（未声明 TTL 不进 scope）", scopes)
	}

	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.TasksDeleted != 0 {
		t.Fatalf("TasksDeleted = %d, want 0", rep.TasksDeleted)
	}
	if rep.TaskScopes != 0 {
		t.Fatalf("TaskScopes = %d, want 0", rep.TaskScopes)
	}
	mustTaskPresent(t, db, orgID, task.ID)

	// 反证：TTL 门禁在 **scope 层**，不在 DELETE 层——
	// 直接按能力调用删除语句会把这条任务删掉。这保证了「不清理」的原因
	// 唯一地归因于「该能力没声明 TTL」，而不是删除语句写错了。
	n, err := db.DeleteExpiredTerminalTasks(ctx, orgID, cap.ID, now-int64(60), 100)
	if err != nil {
		t.Fatalf("direct delete: %v", err)
	}
	if n != 1 {
		t.Fatalf("直接调用删除语句删了 %d 行, want 1（TTL 判断应只发生在 scope 层）", n)
	}
	mustTaskGone(t, db, orgID, task.ID)
}

// ---------- §5-6：批上限生效 ----------

func TestRetentionBatchLimitAndTruncation(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	cap := mkRetentionCap(t, db, orgID, "review", 60)

	const (
		batch = 2
		total = batch*2 + 1 // 5 条过期任务
	)
	// finished_at 递增：最老的在前，用来验证「按 finished_at 排序」真的生效
	var tasks []*Task
	for i := 0; i < total; i++ {
		tasks = append(tasks, mkTerminalTask(t, db, orgID, cap, TaskSucceeded,
			now-1000+int64(i)*100))
	}

	// 第一轮：预算只够 1 批 → 只删 batch 行，且报告被截断
	first, err := db.SweepRetention(ctx, RetentionOptions{Now: now, Batch: batch, Batches: 1})
	if err != nil {
		t.Fatalf("sweep #1: %v", err)
	}
	if first.TasksDeleted != batch {
		t.Fatalf("第 1 轮 TasksDeleted = %d, want %d（单批上限）", first.TasksDeleted, batch)
	}
	if !first.Truncated {
		t.Fatal("第 1 轮 Truncated = false, want true（预算用尽且仍有积压）")
	}
	if n := countRows(t, db, "tasks", "org_id = ?", orgID); n != total-batch {
		t.Fatalf("第 1 轮后 tasks 剩 %d 行, want %d", n, total-batch)
	}
	// 删掉的是最老的两条
	for i := 0; i < batch; i++ {
		mustTaskGone(t, db, orgID, tasks[i].ID)
	}
	for i := batch; i < total; i++ {
		mustTaskPresent(t, db, orgID, tasks[i].ID)
	}

	// 第二轮：预算放大 → 把剩余的清空，且不再截断
	second, err := db.SweepRetention(ctx, RetentionOptions{Now: now, Batch: batch, Batches: 4})
	if err != nil {
		t.Fatalf("sweep #2: %v", err)
	}
	if second.TasksDeleted != total-batch {
		t.Fatalf("第 2 轮 TasksDeleted = %d, want %d", second.TasksDeleted, total-batch)
	}
	if second.Truncated {
		t.Fatalf("第 2 轮 Truncated = true, want false（该范围已清空）")
	}
	if n := countRows(t, db, "tasks", "org_id = ?", orgID); n != 0 {
		t.Fatalf("第 2 轮后 tasks 剩 %d 行, want 0", n)
	}
}

// ---------- §5-7：组织隔离 ----------

func TestRetentionOrgIsolation(t *testing.T) {
	db, orgA := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	orgB, _ := newTestOrg(t, db)

	capA := mkRetentionCap(t, db, orgA, "review", 60)
	capB := mkRetentionCap(t, db, orgB, "review", 60)

	aExpired := mkTerminalTask(t, db, orgA, capA, TaskSucceeded, now-120)
	aFresh := mkTerminalTask(t, db, orgA, capA, TaskSucceeded, now-10)
	bExpired := mkTerminalTask(t, db, orgB, capB, TaskSucceeded, now-120)

	// 交叉过滤：org 与 capability_id 各自都要生效，否则 A 的清理会带走 B 的数据
	cutoff := now - int64(60)
	if n, err := db.DeleteExpiredTerminalTasks(ctx, orgB, capA.ID, cutoff, 100); err != nil || n != 0 {
		t.Fatalf("跨 org 删除 n=%d err=%v, want 0/nil", n, err)
	}
	if n, err := db.DeleteExpiredTerminalTasks(ctx, orgA, capB.ID, cutoff, 100); err != nil || n != 0 {
		t.Fatalf("跨能力删除 n=%d err=%v, want 0/nil", n, err)
	}
	if n := countRows(t, db, "tasks", "org_id = ?", orgA); n != 2 {
		t.Fatalf("交叉删除后 orgA 有 %d 行, want 2（一行都不该动）", n)
	}

	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.TaskScopes != 2 {
		t.Fatalf("TaskScopes = %d, want 2（两个 org 各一个 scope）", rep.TaskScopes)
	}
	if rep.TasksDeleted != 2 {
		t.Fatalf("TasksDeleted = %d, want 2", rep.TasksDeleted)
	}
	mustTaskGone(t, db, orgA, aExpired.ID)
	mustTaskPresent(t, db, orgA, aFresh.ID)
	mustTaskGone(t, db, orgB, bExpired.ID)
	if n := countRows(t, db, "tasks", "org_id = ?", orgA); n != 1 {
		t.Fatalf("orgA 剩 %d 行, want 1", n)
	}
	if n := countRows(t, db, "tasks", "org_id = ?", orgB); n != 0 {
		t.Fatalf("orgB 剩 %d 行, want 0", n)
	}
}

// ---------- §5-8：events 默认禁用（上级 2026-10-30 决定，取代原「默认 30 天」） ----------

// TestRetentionEventsDisabledByDefault 钉住「默认一条 events 都不删」这条语义。
//
// 为什么默认禁用：删除不可逆，而本版没有开关、没有 per-org 配置，且会静默改变
// store/stats.go 的「总数 / 今日 / 热力图」口径。orgs.retention_sec 落地后由组织配置开启。
//
// 这条用例同时证明两件事：
//  1. events 一行都不删（含 source='route' 的剪枝行、含 400 天前的极老行），
//     且 report 能区分「没扫」（EventsDisabled=true）与「扫了但没东西删」；
//  2. tasks 侧**不受** events 开关影响——能力显式声明 resultTtlSec 就照常清理。
func TestRetentionEventsDisabledByDefault(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()

	const day = int64(24 * 60 * 60)
	// ① 90 天前（旧「30 天」口径下必然被删）、② 400 天前（更老）、
	// ③ source='route' 的剪枝行（#142）：旧口径下同样会被删，都不能有特例。
	e90 := mkEventAt(t, db, orgID, "github", "pull_request.opened", now-90*day)
	e400 := mkEventAt(t, db, orgID, "github", "push", now-400*day)
	eRoute := mkEventAt(t, db, orgID, "route", "pruned", now-1000*day)

	// 同时造一条过期终态任务（能力 resultTtlSec=60）：tasks 侧必须照常生效
	cap := mkRetentionCap(t, db, orgID, "review", 60)
	expired := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-120)

	// 默认 options（EventRetentionSec 留 0）= 禁用
	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !rep.EventsDisabled {
		t.Fatal("EventsDisabled = false, want true（默认保留期 <= 0 = 禁用）")
	}
	if rep.EventsDeleted != 0 {
		t.Fatalf("EventsDeleted = %d, want 0（禁用时一条都不许删）", rep.EventsDeleted)
	}
	if rep.Orgs != 0 {
		t.Fatalf("Orgs = %d, want 0（禁用时压根不该去扫 org）", rep.Orgs)
	}
	// 双向校验：report 说没删 + 库里 3 条都在
	if n := countRows(t, db, "events", "org_id = ?", orgID); n != 3 {
		t.Fatalf("events 剩 %d 行, want 3（默认禁用时一行都不该少）", n)
	}
	for _, e := range []*Event{e90, e400, eRoute} {
		mustEventPresent(t, db, orgID, e.ID)
	}

	// tasks 侧独立于 events 的开关：能力显式声明 resultTtlSec 就默认生效
	if rep.TasksDeleted != 1 {
		t.Fatalf("TasksDeleted = %d, want 1（tasks 侧不看 events 开关）", rep.TasksDeleted)
	}
	mustTaskGone(t, db, orgID, expired.ID)

	// 负值同样视为禁用（不是「保留 -5 秒」，更不是「删得更狠」）
	neg, err := db.SweepRetention(ctx, RetentionOptions{Now: now, EventRetentionSec: -5})
	if err != nil {
		t.Fatalf("sweep(EventRetentionSec=-5): %v", err)
	}
	if !neg.EventsDisabled {
		t.Fatal("EventRetentionSec=-5: EventsDisabled = false, want true（负数归一成禁用）")
	}
	if neg.EventsDeleted != 0 || neg.Orgs != 0 {
		t.Fatalf("EventRetentionSec=-5: Orgs=%d EventsDeleted=%d, want 0/0",
			neg.Orgs, neg.EventsDeleted)
	}
	if n := countRows(t, db, "events", "org_id = ?", orgID); n != 3 {
		t.Fatalf("EventRetentionSec=-5 跑完后 events 剩 %d 行, want 3（一行不少）", n)
	}
}

// ---------- §5-9：events 保留期可配置 ----------

func TestRetentionEventsCustomRetention(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()

	older := mkEventAt(t, db, orgID, "github", "push", now-20)
	newer := mkEventAt(t, db, orgID, "github", "push", now-5)

	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now, EventRetentionSec: 10})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// 显式给出保留期 ⇒ events 阶段真的跑了（与「默认禁用」那条用例互为正反两面：
	// 两条都得在，才能同时钉住「默认不删」与「给了就删」）
	if rep.EventsDisabled {
		t.Fatal("EventsDisabled = true, want false（显式给了 10s 保留期）")
	}
	if rep.EventsDeleted != 1 {
		t.Fatalf("EventsDeleted = %d, want 1（10s 保留期只该删 20s 前那条）", rep.EventsDeleted)
	}
	mustEventGone(t, db, orgID, older.ID)
	mustEventPresent(t, db, orgID, newer.ID)
	if n := countRows(t, db, "events", "org_id = ?", orgID); n != 1 {
		t.Fatalf("events 剩 %d 行, want 1", n)
	}
}

// ---------- §5-10：terminalStatusList 与 IsTerminalStatus 不漂移 ----------

func TestTerminalStatusListMatchesIsTerminalStatus(t *testing.T) {
	inList := map[string]bool{}
	for _, s := range terminalStatusList {
		if inList[s] {
			t.Fatalf("terminalStatusList 有重复项 %q", s)
		}
		inList[s] = true
	}
	if len(terminalStatusList) != 4 {
		t.Fatalf("terminalStatusList 有 %d 项, want 4（succeeded/failed/canceled/abandoned）",
			len(terminalStatusList))
	}

	known := map[string]bool{}
	for _, s := range allStatuses {
		known[s] = true
		// 双向：九态逐一对照 IsTerminalStatus 与列表成员关系
		if got, want := IsTerminalStatus(s), inList[s]; got != want {
			t.Errorf("status %q: IsTerminalStatus = %v, terminalStatusList 成员 = %v（两者漂移了）",
				s, got, want)
		}
	}
	// 列表里不允许出现九态之外的值（否则清理 SQL 永远匹配不到任何行）
	for _, s := range terminalStatusList {
		if !known[s] {
			t.Errorf("terminalStatusList 含未知状态 %q（不在九态里）", s)
		}
	}
}

// ---------- 额外守卫 ----------

// TestRetentionDeletesAllTerminalStatuses 终态四态都能被清（防止 SQL 里的 IN 漏项）。
func TestRetentionDeletesAllTerminalStatuses(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	cap := mkRetentionCap(t, db, orgID, "review", 60)

	ids := make([]string, 0, len(terminalStatusList))
	for _, status := range terminalStatusList {
		task := mkTerminalTask(t, db, orgID, cap, status, now-120)
		ids = append(ids, task.ID)
	}

	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.TasksDeleted != int64(len(ids)) {
		t.Fatalf("TasksDeleted = %d, want %d", rep.TasksDeleted, len(ids))
	}
	for _, id := range ids {
		mustTaskGone(t, db, orgID, id)
	}
	if n := countRows(t, db, "tasks", "org_id = ?", orgID); n != 0 {
		t.Fatalf("tasks 剩 %d 行, want 0", n)
	}
}

// TestRetentionDeleteRejectsNonPositiveLimit limit<=0 必须一行都不删。
//
// 删全表是最坏的失效方式（不可恢复），宁可不删：调用方传错预算时，
// 下一轮还会再来，但删掉的数据回不来。
func TestRetentionDeleteRejectsNonPositiveLimit(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	cap := mkRetentionCap(t, db, orgID, "review", 60)

	task := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-120)
	event := mkEventAt(t, db, orgID, "github", "push", now-120)

	for _, limit := range []int{0, -1} {
		n, err := db.DeleteExpiredTerminalTasks(ctx, orgID, cap.ID, now, limit)
		if err != nil {
			t.Fatalf("DeleteExpiredTerminalTasks(limit=%d) err = %v, want nil", limit, err)
		}
		if n != 0 {
			t.Fatalf("DeleteExpiredTerminalTasks(limit=%d) 删了 %d 行, want 0", limit, n)
		}
		n, err = db.DeleteExpiredEvents(ctx, orgID, now, limit)
		if err != nil {
			t.Fatalf("DeleteExpiredEvents(limit=%d) err = %v, want nil", limit, err)
		}
		if n != 0 {
			t.Fatalf("DeleteExpiredEvents(limit=%d) 删了 %d 行, want 0", limit, n)
		}
	}
	mustTaskPresent(t, db, orgID, task.ID)
	mustEventPresent(t, db, orgID, event.ID)
}

// TestListOrgIDs 空库返回 []（不是 nil），有组织时按 id 升序。
func TestListOrgIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ids, err := db.ListOrgIDs(ctx)
	if err != nil {
		t.Fatalf("list org ids: %v", err)
	}
	if ids == nil {
		t.Fatal("ListOrgIDs 返回 nil，需要 []")
	}
	if len(ids) != 0 {
		t.Fatalf("空库有 %d 个 org, want 0", len(ids))
	}

	// newTestOrg 生成的是 org_<随机>，顺序由 id 决定 → 直接按排序后的期望值比对
	var want []string
	for i := 0; i < 3; i++ {
		orgID, _ := newTestOrg(t, db)
		want = append(want, orgID)
	}
	sort.Strings(want)

	ids, err = db.ListOrgIDs(ctx)
	if err != nil {
		t.Fatalf("list org ids: %v", err)
	}
	if len(ids) != len(want) {
		t.Fatalf("有 %d 个 org, want %d", len(ids), len(want))
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids[%d] = %q, want %q（必须按 id 升序）", i, ids[i], want[i])
		}
	}
}

// TestSweepRetentionHonoursCancelledContext 已取消的 ctx 不得动库，且要带回 partial report。
func TestSweepRetentionHonoursCancelledContext(t *testing.T) {
	db, orgID := retentionTestDB(t)
	now := Now()
	cap := mkRetentionCap(t, db, orgID, "review", 60)
	task := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-120)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now})
	if err == nil {
		t.Fatal("SweepRetention(已取消 ctx) 返回 nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rep.TasksDeleted != 0 || rep.EventsDeleted != 0 {
		t.Fatalf("取消后报告 TasksDeleted=%d EventsDeleted=%d, want 0/0",
			rep.TasksDeleted, rep.EventsDeleted)
	}
	mustTaskPresent(t, db, orgID, task.ID)
}

// ---------- §5-15：中途取消 → 返回已删除行数 > 0 的 partial report ----------

// cancelAfterErrCtx 是「第 after+1 次 Err() 检查起才算取消」的 context（仅测试用）。
//
// 为什么不用 `context.WithCancel` + 定时取消：那会让「删了多少行」依赖 goroutine 调度，
// 测试会偶发飘。这里把取消点绑到**被测代码自己的 ctx.Err() 检查点**上，因此完全可数。
//
// Done() 故意保持 nil（嵌入 context.Background）：database/sql 的 Rows.initContextClose
// 见到 `ctx.Done() == nil` 立刻返回、驱动 modernc/sqlite 的
// `if ctxDone := ctx.Done(); ctxDone != nil { ... }` 也整段跳过，
// 所以 Err() **只**会被 SweepRetention / drainRetentionBatches 调到。
// 代价是这个 ctx 不符合 context 包「Err() 非 nil 时 Done() 应已关闭」的约定——
// 只作测试替身，不进生产代码。
//
// 非并发安全：SweepRetention 单 goroutine 调 Err()，这里就不加锁。
type cancelAfterErrCtx struct {
	context.Context
	after int // 前 after 次 Err() 返回 nil，之后（含第 after+1 次）返回 context.Canceled
	calls int
	fired bool
}

func (c *cancelAfterErrCtx) Err() error {
	if c.fired {
		return context.Canceled
	}
	c.calls++
	if c.calls > c.after {
		c.fired = true
		return context.Canceled
	}
	return nil
}

// TestRetentionPartialReportOnCancel 跑到一半被取消时，已经删掉的行数必须出现在
// report 里（否则日志只会显示「这一轮失败」而看不见已经发生的删除，运维无法判断进度），
// 并且错误要能被 errors.Is(err, context.Canceled) 认出来。
func TestRetentionPartialReportOnCancel(t *testing.T) {
	t.Run("tasks 阶段中途取消", func(t *testing.T) {
		db, orgID := retentionTestDB(t)
		now := Now()
		cap := mkRetentionCap(t, db, orgID, "review", 60)

		// finished_at 递增 → 删的批次从最老开始
		oldest := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-300)
		middle := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-200)
		newest := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-120)

		// Batch=2、Batches=5 时的 Err() 调用点：
		//   1 SweepRetention 入口
		//   2 scope 循环第一个 scope
		//   3 tasks 第 1 批前（删 2 行 → 满批，继续下一批）
		//   4 tasks 第 2 批前  ← after=3：在这里取消
		ctx := &cancelAfterErrCtx{Context: context.Background(), after: 3}

		rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now, Batch: 2, Batches: 5})
		if err == nil {
			t.Fatalf("中途取消后 SweepRetention 返回 nil error，report = %+v", rep)
		}
		if !ctx.fired {
			t.Fatal("取消没有触发：SweepRetention 的 ctx.Err() 检查点变了？")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if rep.TasksDeleted != 2 {
			t.Fatalf("partial report TasksDeleted = %d, want 2（第 1 批已删的行必须记进报告）",
				rep.TasksDeleted)
		}
		if rep.EventsDeleted != 0 {
			t.Fatalf("partial report EventsDeleted = %d, want 0（events 阶段根本没开始）",
				rep.EventsDeleted)
		}
		// report 与库内事实双向一致
		mustTaskGone(t, db, orgID, oldest.ID)
		mustTaskGone(t, db, orgID, middle.ID)
		mustTaskPresent(t, db, orgID, newest.ID)
		if n := countRows(t, db, "tasks", "org_id = ?", orgID); n != 1 {
			t.Fatalf("tasks 剩 %d 行, want 1", n)
		}
	})

	t.Run("events 阶段中途取消", func(t *testing.T) {
		db, orgID := retentionTestDB(t)
		now := Now()
		cap := mkRetentionCap(t, db, orgID, "review", 60)
		task := mkTerminalTask(t, db, orgID, cap, TaskSucceeded, now-120)

		// EventRetentionSec=10 且 Batch=1 → 两条过期事件各占一批
		older := mkEventAt(t, db, orgID, "github", "push", now-30)
		newer := mkEventAt(t, db, orgID, "github", "push", now-20)

		// Err() 调用点（Batch=1、Batches=5）：
		//   1 入口
		//   2 scope 循环
		//   3 tasks 第 1 批前（删 1 行 → 满批）
		//   4 tasks 第 2 批前（删 0 行 → 该范围结束）
		//   5 org 循环
		//   6 events 第 1 批前（删 1 行 → 满批）
		//   7 events 第 2 批前  ← after=6：在这里取消
		ctx := &cancelAfterErrCtx{Context: context.Background(), after: 6}

		rep, err := db.SweepRetention(ctx, RetentionOptions{
			Now: now, EventRetentionSec: 10, Batch: 1, Batches: 5,
		})
		if err == nil {
			t.Fatalf("events 阶段中途取消后 SweepRetention 返回 nil error，report = %+v", rep)
		}
		if !ctx.fired {
			t.Fatal("取消没有触发：SweepRetention 的 ctx.Err() 检查点变了？")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if rep.TasksDeleted != 1 {
			t.Fatalf("partial report TasksDeleted = %d, want 1（tasks 阶段已跑完）",
				rep.TasksDeleted)
		}
		if rep.EventsDeleted != 1 {
			t.Fatalf("partial report EventsDeleted = %d, want 1（取消前已删掉的那条要记进报告）",
				rep.EventsDeleted)
		}
		// 报告与库内事实一致：tasks 阶段完成，events 只删了最老那条
		mustTaskGone(t, db, orgID, task.ID)
		mustEventGone(t, db, orgID, older.ID)
		mustEventPresent(t, db, orgID, newer.ID)
		if n := countRows(t, db, "events", "org_id = ?", orgID); n != 1 {
			t.Fatalf("events 剩 %d 行, want 1", n)
		}
	})
}

// ---------- §5-16：events 跨 org 隔离 ----------

// TestRetentionEventsOrgIsolation 与 TaskRetentionOrgIsolation 对称：
// 事件清理的隔离依据只有 `org_id = ?` 这一个条件，写漏了就会「清 A 顺手把 B 清空」。
func TestRetentionEventsOrgIsolation(t *testing.T) {
	db, orgA := retentionTestDB(t)
	ctx := context.Background()
	now := Now()
	orgB, _ := newTestOrg(t, db)

	tooOld := now - retentionTestEventSec - 10
	aOld := mkEventAt(t, db, orgA, "github", "push", tooOld)
	aFresh := mkEventAt(t, db, orgA, "github", "push", now-60)
	bOld := mkEventAt(t, db, orgB, "github", "push", tooOld)

	// ① 只清 A：B 的过期事件一行都不能被带走
	n, err := db.DeleteExpiredEvents(ctx, orgA, now-retentionTestEventSec, 100)
	if err != nil {
		t.Fatalf("delete expired events (orgA): %v", err)
	}
	if n != 1 {
		t.Fatalf("只清 orgA 删了 %d 行, want 1", n)
	}
	mustEventGone(t, db, orgA, aOld.ID)
	mustEventPresent(t, db, orgA, aFresh.ID)
	mustEventPresent(t, db, orgB, bOld.ID)
	if n := countRows(t, db, "events", "org_id = ?", orgB); n != 1 {
		t.Fatalf("orgB 有 %d 条事件, want 1（A 的清理碰了 B）", n)
	}

	// ② 整轮 sweep：B 的那条在自己那一轮才被删，计数与库内事实一致。
	// 注意必须**显式**传保留期：默认是禁用，不传的话 events 阶段整个跳过，
	// 这个用例就变成了空跑。
	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now, EventRetentionSec: retentionTestEventSec})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.EventsDisabled {
		t.Fatal("EventsDisabled = true, want false（本次显式给了保留期）")
	}
	if rep.Orgs != 2 {
		t.Fatalf("Orgs = %d, want 2", rep.Orgs)
	}
	if rep.EventsDeleted != 1 {
		t.Fatalf("EventsDeleted = %d, want 1（A 那条上一轮已删）", rep.EventsDeleted)
	}
	mustEventGone(t, db, orgB, bOld.ID)
	mustEventPresent(t, db, orgA, aFresh.ID)
	if n := countRows(t, db, "events", "org_id = ?", orgA); n != 1 {
		t.Fatalf("orgA 剩 %d 条事件, want 1", n)
	}
	if n := countRows(t, db, "events", "org_id = ?", orgB); n != 0 {
		t.Fatalf("orgB 剩 %d 条事件, want 0", n)
	}
}

// ---------- §5-17：坏 lifecycle JSON 不毒丸整轮清理 ----------

// TestRetentionBadLifecycleDoesNotPoisonSweep 一条坏 JSON 不许让保留策略永久失效。
//
// 老库、人工改库、e2e 直插都能造出坏值；上轮实现里 json.Unmarshal 失败就 return error，
// 会让 SweepRetention 在 events 阶段之前整轮中止，于是 tasks 与 events **全部**停止清理，
// 而且每小时重复失败一次——一条坏数据等于把保留策略关掉。
func TestRetentionBadLifecycleDoesNotPoisonSweep(t *testing.T) {
	db, orgID := retentionTestDB(t)
	ctx := context.Background()
	now := Now()

	badCap := mkRetentionCap(t, db, orgID, "broken", 60)
	goodCap := mkRetentionCap(t, db, orgID, "review", 60)

	badTask := mkTerminalTask(t, db, orgID, badCap, TaskSucceeded, now-120)
	goodTask := mkTerminalTask(t, db, orgID, goodCap, TaskSucceeded, now-120)
	oldEvent := mkEventAt(t, db, orgID, "github", "push", now-retentionTestEventSec-10)

	// 直接改库造坏 lifecycle（生产路径写不出这种值，所以只能直插）
	mustExec(t, db, `UPDATE capability_versions SET lifecycle = '{bad json' WHERE id = ?`, badCap.ID)

	// 抓日志：跳过必须是**有声音**的跳过（slog.Warn + 固定 event 名），不能静默
	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prevLogger)

	scopes, err := db.ListResultRetentionScopes(ctx)
	if err != nil {
		t.Fatalf("坏 lifecycle 让 ListResultRetentionScopes 失败了: %v", err)
	}
	if len(scopes) != 1 || scopes[0].CapabilityID != goodCap.ID {
		t.Fatalf("scopes = %+v, want 只剩好能力 %s", scopes, goodCap.ID)
	}

	// 显式传保留期：默认禁用时 events 阶段根本不会跑，这个用例就验证不了
	// 「events 阶段也必须照常清理」（它本来就是这条断言的存在理由）。
	rep, err := db.SweepRetention(ctx, RetentionOptions{Now: now, EventRetentionSec: retentionTestEventSec})
	if err != nil {
		t.Fatalf("坏 lifecycle 毒丸了整轮清理: %v", err)
	}
	if rep.EventsDisabled {
		t.Fatal("EventsDisabled = true, want false（本次显式给了保留期）")
	}
	if rep.TaskScopes != 1 {
		t.Fatalf("TaskScopes = %d, want 1（坏能力被跳过）", rep.TaskScopes)
	}
	if rep.TasksDeleted != 1 {
		t.Fatalf("TasksDeleted = %d, want 1（只删好能力那条）", rep.TasksDeleted)
	}
	if rep.EventsDeleted != 1 {
		t.Fatalf("EventsDeleted = %d, want 1（events 阶段必须照常跑）", rep.EventsDeleted)
	}
	mustTaskPresent(t, db, orgID, badTask.ID)
	mustTaskGone(t, db, orgID, goodTask.ID)
	mustEventGone(t, db, orgID, oldEvent.ID)

	logs := buf.String()
	for _, want := range []string{"store.retention.bad_lifecycle", orgID, badCap.ID} {
		if !strings.Contains(logs, want) {
			t.Fatalf("warn 日志缺少 %q；实际日志:\n%s", want, logs)
		}
	}
}
