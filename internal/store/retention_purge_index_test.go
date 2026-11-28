package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 #628：能力 tombstone 物理回收的判据必须「每个引用列一次 seek」。
//
// 判据里三次 NOT EXISTS 落在 tasks / task_attempts / task_artifact_manifests 的 capability_id 上。
// 这三列没有索引时是三次全表扫，单轮代价 = 任务行数 × 候选 tombstone 数，与「这轮删掉几行」
// 无关：30 万任务 + 100 个仍被引用的 tombstone 时，一条 DELETE 在 BEGIN IMMEDIATE 写锁里跑
// 6.8s（deleted=0），同时到来的写撞满 busy_timeout(5000) 后全部 database is locked。
// 所以这里守两件事：计划里没有对这三张表的 SCAN（结构），稳态代价有界（症状）。

// seedPurgeTask 直插一条引用 capID 的任务行（带真实 digest，否则 tasks_resolved_pin_shape 会挡）。
func seedPurgeTask(t *testing.T, db *DB, nsID, capID, capName, trace string, now int64) string {
	t.Helper()
	id := NewTaskID()
	mustExec(t, db, `INSERT INTO tasks
	   (id, namespace_id, trace_id, capability_id, capability, version, digest,
	    phase, reported, created_at, finished_at)
	 VALUES (?,?,?,?,?,1,?,?,0,?,?)`,
		id, nsID, trace, capID, capName, "sha256:"+strings.Repeat("d", 64),
		TaskSucceeded, now-3600, now-3600)
	return id
}

// purgeSeedPinnedTombstones 造 n 个「已删除、但仍被一条最近任务钉住」的能力版本，返回它们的 UID。
// 这正是稳态：任务保留 30 天，被引用的 tombstone 每轮都会重新进候选集，而一行都删不掉——
// 报告里 deleted=0 却持锁 6.8s 的就是这种轮次。
func purgeSeedPinnedTombstones(t *testing.T, db *DB, nsID string, n int, now int64) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("purge-628-%03d", i)
		c := v4Cap(t, db, nsID, name, 1)
		if err := db.DeleteCapability(context.Background(), nsID, name,
			ResourceVersion(c.ID, c.ResourceVersion)); err != nil {
			t.Fatalf("delete tombstone %d: %v", i, err)
		}
		seedPurgeTask(t, db, nsID, c.ID, name, fmt.Sprintf("tr_purge_pin_%03d", i), now)
		ids = append(ids, c.ID)
	}
	return ids
}

// 判据必须对三张表的 capability_id 走索引（migration 13）。
func TestPurgeDeletedCapabilitiesUsesIndexes(t *testing.T) {
	db, _ := newTestDBFile(t)
	nsID, _ := newTestNS(t, db)
	now := Now()

	live := v4Cap(t, db, nsID, "purge-628-live", 1)
	taskIDs := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		taskIDs = append(taskIDs, seedPurgeTask(t, db, nsID, live.ID, live.Name,
			fmt.Sprintf("tr_purge_live_%03d", i), now))
	}
	// attempt / manifest 也灌一些行：ANALYZE 之后计划才按真实基数选，不会是「空表随便挑」。
	mustExec(t, db, `INSERT INTO task_artifact_objects (id, created_at) VALUES ('obj-purge-628', ?)`, now)
	for _, id := range taskIDs[:200] {
		mustExec(t, db, `INSERT INTO task_attempts (task_id, attempt, node_id, capability_id)
		   VALUES (?, 1, 'node-purge-628', ?)`, id, live.ID)
		mustExec(t, db, `INSERT INTO task_artifact_manifests
		   (task_id, attempt, namespace_id, capability_id, path, sha256, size, object_id,
		    published_at, expires_at)
		   VALUES (?, 1, ?, ?, 'out.txt', ?, 1, 'obj-purge-628', ?, ?)`,
			id, nsID, live.ID, "sha256:"+strings.Repeat("e", 64), now, now+86400)
	}
	// 候选集非空（tombstone 仍被任务钉住），否则这条判据在空集上怎么写都「很快」。
	purgeSeedPinnedTombstones(t, db, nsID, 3, now)

	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	plan := strings.Join(explainQueryPlan(t, db, purgeDeletedCapabilitiesCandidateSQL(), DefaultRetentionBatch), "\n")
	t.Logf("EXPLAIN QUERY PLAN（能力 tombstone 回收候选集）:\n%s", plan)

	// 剩下的 `SCAN p` 是安装回执那条分支（json_tree(p.receipt)）：它按**安装数**扫，
	// 与任务行数无关，不属于本次回归（报告里 6.8s 全部来自上面三列）。把它结构化到
	// playbook_install_pinned 的 id 列是 #551 的后续；在那之前它不能被误当成「已修」。
	for _, scan := range []string{"SCAN t", "SCAN a", "SCAN m"} {
		if strings.Contains(plan, scan) {
			t.Errorf("候选集出现 `%s`（引用列全表扫）：代价 = 任务行数 × tombstone 数，与删了几行无关\n%s", scan, plan)
		}
	}
	for _, idx := range []string{
		"idx_tasks_capability_id",
		"idx_task_attempts_capability",
		"idx_task_artifact_manifests_capability",
	} {
		if !strings.Contains(plan, idx) {
			t.Errorf("候选集没有走 %s（migration 13 的索引没被用上）:\n%s", idx, plan)
		}
	}

	// ON DELETE RESTRICT 的父行检查：SQLite 每删一行父行都在子表上查一次引用，
	// 这条语句同样必须 seek（deleting 时才有的第二份代价）。
	fkPlan := strings.Join(explainQueryPlan(t, db, `SELECT 1 FROM tasks WHERE capability_id = ?`, live.ID), "\n")
	t.Logf("EXPLAIN QUERY PLAN（tasks.capability_id 外键 RESTRICT 检查）:\n%s", fkPlan)
	if !strings.Contains(fkPlan, "idx_tasks_capability_id") {
		t.Errorf("外键 RESTRICT 检查没有走 idx_tasks_capability_id:\n%s", fkPlan)
	}

	// 这里测的必须是生产语句：DELETE 字面量只允许出现在候选集函数一处。
	src, err := os.ReadFile("retention.go")
	if err != nil {
		t.Fatalf("read retention.go: %v", err)
	}
	if got := strings.Count(compactSQL(string(src)), "delete from capability_versions where id in ("); got != 1 {
		t.Fatalf("retention.go 有 %d 处 `DELETE FROM capability_versions` 字面量，want 1（生产语句只能从 purgeDeletedCapabilitiesCandidateSQL() 拼出来）", got)
	}
	if got := strings.Count(string(src), "purgeDeletedCapabilitiesCandidateSQL()"); got < 2 {
		t.Fatalf("purgeDeletedCapabilitiesCandidateSQL() 出现 %d 次，want 定义 + PurgeDeletedCapabilities 各一处", got)
	}
}

// 稳态（没有可删的 tombstone）的持锁时长必须与任务行数脱钩：报告里 50k 任务 / 100 个被钉住的
// tombstone 是 380ms，150k 是 3.3s。走索引后同一规模是毫秒级，阈值 150ms 留了两位数余量。
func TestPurgeDeletedCapabilitiesSteadyStateCost(t *testing.T) {
	db, _ := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	now := Now()

	live := v4Cap(t, db, nsID, "purge-cost-628", 1)
	const taskRows = 60_000
	seedRetentionTasks(t, db, nsID, live, taskRows, now-2*86400) // 最近 2 天内完成：一条都不过期
	const tombs = 100
	purgeSeedPinnedTombstones(t, db, nsID, tombs, now)
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	start := time.Now()
	n, err := db.PurgeDeletedCapabilities(ctx, DefaultRetentionBatch)
	took := time.Since(start)
	t.Logf("%d 任务 + %d 个被任务钉住的 tombstone：PurgeDeletedCapabilities deleted=%d took=%v",
		taskRows, tombs, n, took)
	if err != nil {
		t.Fatalf("purge deleted capability: %v", err)
	}
	if n != 0 {
		t.Fatalf("删了 %d 行，期望 0（每个 tombstone 都还被一条任务钉住）", n)
	}
	if took > 150*time.Millisecond {
		t.Fatalf("稳态回收耗时 %v（>150ms）：判据又退回全表扫，写锁会被持住（#628）", took)
	}
	if got := countRows(t, db, "capability_versions"); got != tombs+1 {
		t.Fatalf("capability_versions 行数 = %d, want %d（tombstone 都该留着）", got, tombs+1)
	}
}
