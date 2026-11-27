package store

// #630 的门禁：任务保留期清理（DeleteExpiredTasks，跟租约巡检每 30s 跑一次）的候选集
// 按「可信保留时钟」COALESCE(result_accepted_at, CASE WHEN reported=0 THEN finished_at END)
// 过滤并排序，而这条表达式原先没有任何索引可用，于是每轮都是全表扫 + 临时排序，
// 整段扫描还都在 DELETE 的写锁里——稳态一行不删，100 万任务也持锁 0.43s。
//
// 修复是 migration 12 的表达式索引 idx_tasks_retain_from。这里钉住三件事：
//   - 候选查询真的走那条索引（EXPLAIN，无全表扫、无临时排序）；
//   - 索引表达式与 store.taskRetainFromSQL **逐字一致**（SQLite 表达式索引按语法树匹配，
//     差一个字符就退回全表扫，而结果一样，功能测试永远发现不了）；
//   - 稳态（没有过期行）的成本与任务总数基本无关。

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// seedRetentionTasks 用一条事务直插 n 条终态任务（reported=0 + finished_at），
// 返回插入的时刻基数。直插是刻意的：要走 CreateTasks → grant → complete 才能得到终态，
// 十万行的种子时间会盖过被测语句本身，而这里要量的只是清理的成本。
func seedRetentionTasks(t *testing.T, db *DB, nsID string, c *CapabilityVersion, n int, finishedAt int64) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(
		`INSERT INTO tasks
		   (id, namespace_id, trace_id, capability_id, capability, version, digest,
		    phase, reported, created_at, finished_at, result_accepted_at)
		 VALUES (?,?,?,?,?,?,?,?,0,?,?,NULL)`)
	if err != nil {
		t.Fatalf("prepare seed: %v", err)
	}
	defer stmt.Close()
	for i := 0; i < n; i++ {
		phase := TaskSucceeded
		if i%7 == 0 {
			phase = TaskFailed
		}
		if _, err := stmt.Exec(NewTaskID(), nsID, fmt.Sprintf("tr_ret_%d", i), c.ID, c.Name, 1,
			"sha256:"+strings.Repeat("a", 64), phase, finishedAt+int64(i), finishedAt+int64(i)); err != nil {
			t.Fatalf("seed task %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// TestDeleteExpiredTasksUsesIndex 候选查询必须走 idx_tasks_retain_from：
// 没有 `SCAN t`（全表扫），也没有 `USE TEMP B-TREE FOR ORDER BY`（先排完再取前 1000 行）。
//
// 跑的是**产品真的执行的那条**候选 SQL（deleteExpiredTasksCandidateSQL），
// 并在末尾用源码守卫确认 DeleteExpiredTasks 用的就是它——否则本门禁可能在测一条没人跑的语句。
func TestDeleteExpiredTasksUsesIndex(t *testing.T) {
	db, _ := newTestDBFile(t)
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "ret-index-630", 1)

	// 三种形状都要在同一张表里：终态已上报（result_accepted_at）、终态未上报（finished_at）、
	// 以及不过期的活动行。规划器只有在统计新鲜时才可能选对索引，所以这里 ANALYZE。
	now := Now()
	seedRetentionTasks(t, db, nsID, c, 600, now-3600)
	mustExec(t, db, `UPDATE tasks SET reported=1, result_accepted_at=finished_at WHERE rowid % 2 = 0`)
	// 活动行：没有 finished_at / result_accepted_at，表达式为 NULL，永不删。
	mustExec(t, db, `INSERT INTO tasks
	   (id, namespace_id, trace_id, capability_id, capability, version, digest, phase, created_at)
	   VALUES (?,?,?,?,?,?,?,?,?)`,
		NewTaskID(), nsID, "tr_ret_run", c.ID, c.Name, 1, c.Digest, TaskRunning, now)
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	_, args := terminalPhaseIn()
	args = append(args, now-30*86400, DefaultRetentionBatch)
	plan := strings.Join(explainQueryPlan(t, db, deleteExpiredTasksCandidateSQL(), args...), "\n")
	t.Logf("EXPLAIN QUERY PLAN（任务保留期候选集）:\n%s", plan)

	if !strings.Contains(plan, "idx_tasks_retain_from") {
		t.Errorf("候选查询没有走 idx_tasks_retain_from（会退化成全表扫 + 临时排序）:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN t") {
		t.Errorf("候选查询出现 `SCAN t`（全表扫）:\n%s", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("候选查询要先排序才能取前 %d 行（LIMIT 没有落在 seek 上）:\n%s", DefaultRetentionBatch, plan)
	}

	// 索引形状：必须有且只有一条，且表达式与 Go 常量逐字一致（空白/大小写归一后比对）。
	if n := countIndexRows(t, db, "tasks", "idx_tasks_retain_from"); n != 1 {
		t.Fatalf("idx_tasks_retain_from 有 %d 条, want 1", n)
	}
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`,
		"idx_tasks_retain_from").Scan(&ddl); err != nil {
		t.Fatalf("read index ddl: %v", err)
	}
	if !strings.Contains(compactSQL(ddl), compactSQL(taskRetainFromSQL)) {
		t.Fatalf("索引表达式与 taskRetainFromSQL 不一致（表达式索引按语法树匹配，漂移即失去索引）:\n索引: %s\n常量: %s",
			ddl, taskRetainFromSQL)
	}

	// 产品真的跑的是这条 SQL：候选集从 deleteExpiredTasksCandidateSQL() 拼出来。
	src, err := os.ReadFile("retention.go")
	if err != nil {
		t.Fatalf("read retention.go: %v", err)
	}
	if !strings.Contains(string(src), "DELETE FROM tasks WHERE id IN (`+deleteExpiredTasksCandidateSQL()+`)") {
		t.Fatalf("DeleteExpiredTasks 没有用 deleteExpiredTasksCandidateSQL() 拼语句——本门禁在测一条没人跑的 SQL")
	}
}

// TestDeleteExpiredTasksSteadyStateCost 稳态（没有任何一行过期）的成本必须与任务总数基本无关。
//
// 10 万条终态任务：修复前是「全表扫 + 临时排序」，耗时随行数线性涨（这份形状约 50ms，
// 100 万条约 0.43s，全程持 DELETE 的写锁，见 #630）；走索引后是索引上的一次范围 seek，
// 第一条就 >= cutoff，立即返回。断言留 ~40 倍余量，避免共享 CI 上的抖动造成假红。
//
// 同时验证 LIMIT 真的落在 seek 上：10 万条未过期任务里混一条过期的，limit=1 只删它。
func TestDeleteExpiredTasksSteadyStateCost(t *testing.T) {
	if testing.Short() {
		t.Skip("10 万行规模回归：-short 跳过")
	}
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "ret-cost-630", 1)

	now := Now()
	const total = 100_000
	seedRetentionTasks(t, db, nsID, c, total, now-2*86400) // 全部在最近 2 天内完成：一条都不过期
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	cutoff := now - 30*86400
	start := time.Now()
	n, err := db.DeleteExpiredTasks(ctx, cutoff, DefaultRetentionBatch)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("delete expired tasks: %v", err)
	}
	t.Logf("%d 条未过期任务：DeleteExpiredTasks deleted=%d took=%v", total, n, took)
	if n != 0 {
		t.Fatalf("删了 %d 行，期望 0（全部未过期）", n)
	}
	if took > 30*time.Millisecond {
		t.Fatalf("稳态 DeleteExpiredTasks 耗时 %v（>30ms）：保留期清理又变成全表扫了？", took)
	}

	// LIMIT 落在 seek 上：混一条「已过期」的任务，limit=1 必须只删它，而不是先把 10 万行排完。
	oldID := mustTaskID(t, db)
	mustExec(t, db, `UPDATE tasks SET finished_at=? WHERE id=?`, now-90*86400, oldID)
	if got, err := db.DeleteExpiredTasks(ctx, cutoff, 1); err != nil || got != 1 {
		t.Fatalf("DeleteExpiredTasks(limit=1) = %d, %v; want 1", got, err)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE id=?`, oldID).Scan(&left); err != nil {
		t.Fatalf("count old task: %v", err)
	}
	if left != 0 {
		t.Fatalf("过期的老任务没有被删（limit 没有落在 seek 上？）")
	}
	if got := countRows(t, db, "tasks"); got != total-1 {
		t.Fatalf("tasks 行数 = %d, want %d（只该删掉那一条过期任务）", got, total-1)
	}
}

// mustTaskID 取一条任务的 id（种子/夹具用）。
func mustTaskID(t *testing.T, db *DB) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM tasks LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("pick task id: %v", err)
	}
	return id
}

// compactSQL 把 SQL 归一成「小写 + 单空格」，用于比较两份只该在空白/大小写上相同的表达式。
func compactSQL(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
