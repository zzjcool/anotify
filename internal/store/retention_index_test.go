package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

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

// 候选查询必须走 idx_tasks_retain_from（无 `SCAN t`、无 `USE TEMP B-TREE FOR ORDER BY`），
// 且索引表达式与 taskRetainFromSQL 逐字一致——表达式索引按语法树匹配，差一个字符就退回全表扫。
func TestDeleteExpiredTasksUsesIndex(t *testing.T) {
	db, _ := newTestDBFile(t)
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "ret-index-630", 1)

	now := Now()
	seedRetentionTasks(t, db, nsID, c, 600, now-3600)
	mustExec(t, db, `UPDATE tasks SET reported=1, result_accepted_at=finished_at WHERE rowid % 2 = 0`)
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

	src, err := os.ReadFile("retention.go")
	if err != nil {
		t.Fatalf("read retention.go: %v", err)
	}
	if !strings.Contains(string(src), "DELETE FROM tasks WHERE id IN (`+deleteExpiredTasksCandidateSQL()+`)") {
		t.Fatalf("DeleteExpiredTasks 没有用 deleteExpiredTasksCandidateSQL() 拼语句——这里在测一条没人跑的 SQL")
	}
}

// 稳态 10 万条无过期行：全表扫 + 临时排序约 50ms（100 万 0.43s，见 #630）且全程持 DELETE 写锁；走索引是一次 seek，阈值 30ms。
func TestDeleteExpiredTasksSteadyStateCost(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "ret-cost-630", 1)

	now := Now()
	const total = 100_000
	seedRetentionTasks(t, db, nsID, c, total, now-2*86400) // 最近 2 天内完成：一条都不过期
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

func mustTaskID(t *testing.T, db *DB) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT id FROM tasks LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("pick task id: %v", err)
	}
	return id
}

func compactSQL(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
