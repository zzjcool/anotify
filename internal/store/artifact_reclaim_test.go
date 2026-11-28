package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// 本文件是 #629 的 store 侧验收：
//
//  1. 对象行（task_artifact_objects）的生命周期必须在 purged_at 之后**真的结束**：
//     字节删掉、且再没有任何 manifest/upload 引用的行，由下一轮 ExpireArtifactObjects
//     物理删除（否则行只增不减，每轮 GC 的写锁时长随历史上传总数单调增长）；
//  2. 两段扫描（到期 / 回收）都必须落在部分索引上，不能每轮全表扫历史行。

// seedArtifactObjectRow 直接写一行对象身份。task_artifact_objects 没有别的 NOT NULL 列，
// 也没有外键，所以这里能独立造出「已过期 / 已 purged」的历史形状。
func seedArtifactObjectRow(t *testing.T, db *DB, id string, createdAt int64, expiredAt, purgedAt any) {
	t.Helper()
	mustExec(t, db, `INSERT INTO task_artifact_objects(id,created_at,expired_at,purged_at) VALUES (?,?,?,?)`,
		id, createdAt, expiredAt, purgedAt)
}

func countArtifactObjectRows(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM task_artifact_objects`).Scan(&n); err != nil {
		t.Fatalf("count task_artifact_objects: %v", err)
	}
	return n
}

func artifactObjectRowExists(t *testing.T, db *DB, id string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM task_artifact_objects WHERE id=?`, id).Scan(&n); err != nil {
		t.Fatalf("probe object row %s: %v", id, err)
	}
	return n == 1
}

func artifactObjectIDs(ids []ArtifactObjectID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// TestExpireArtifactObjects_DeletesPurgedUnreferencedRows 覆盖 #629 第 1 项的验收：
//
//   - 已 purged 且无引用的对象行 → 一轮后被物理删除；
//   - 仍被 manifest 引用（410 的元数据还在）→ 保留，直到 manifest 随任务保留期级联删除；
//   - 仍被活 ticket 引用（外键 ON DELETE RESTRICT）→ 不许删；ticket 到期后同一轮即回收；
//   - 只 expired、还没 ack（字节可能还在）→ 不删，且仍然出现在返回值里（可重试清理）。
func TestExpireArtifactObjects_DeletesPurgedUnreferencedRows(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "artifact-reclaim", 1)
	now := Now()

	// 真实图结构：manifest/upload 都有 (task_id,attempt) → task_attempts 的外键，
	// 所以引用分支必须建出真任务，不能用一行假引用。
	taskID := NewTaskID()
	mustExec(t, db, `INSERT INTO tasks
	   (id, namespace_id, trace_id, capability_id, capability, version, digest, phase, created_at)
	   VALUES (?,?,?,?,?,?,?,?,?)`,
		taskID, nsID, "tr-obj-reclaim", c.ID, c.Name, 1, c.Digest, TaskSucceeded, now-1000)
	mustExec(t, db, `INSERT INTO task_attempts(task_id,attempt,node_id) VALUES (?,?,?)`,
		taskID, 1, "node-reclaim")

	old := now - 600
	seedArtifactObjectRow(t, db, "obj_purged_free", old, old, now-300)
	seedArtifactObjectRow(t, db, "obj_purged_manifest", old, old, now-300)
	seedArtifactObjectRow(t, db, "obj_purged_upload", old, old, now-300)
	seedArtifactObjectRow(t, db, "obj_expired_unacked", old, now-300, nil)

	mustExec(t, db, `INSERT INTO task_artifact_manifests
	   (task_id,attempt,namespace_id,capability_id,path,sha256,size,object_id,published_at,expires_at,expired_at)
	   VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		taskID, 1, nsID, c.ID, "reports/keep.md", strings.Repeat("a", 64), 12,
		"obj_purged_manifest", old, now-300, now-300)
	mustExec(t, db, `INSERT INTO task_artifact_uploads
	   (id,task_id,attempt,path,sha256,size,object_id,expires_at)
	   VALUES (?,?,?,?,?,?,?,?)`,
		"upl_reclaim", taskID, 1, "reports/ticket.md", strings.Repeat("b", 64), 13,
		"obj_purged_upload", now+300)

	// 第一轮：只应列出「已过期、没引用、还没 ack」那个；已 purged 的不再回来（字节已删）。
	ids, err := db.ExpireArtifactObjects(ctx, now, 100)
	if err != nil {
		t.Fatalf("expire artifacts: %v", err)
	}
	if got, want := artifactObjectIDs(ids), []string{"obj_expired_unacked"}; !equalStrings(got, want) {
		t.Fatalf("第一轮待清理 id = %v, want %v（已 purged 的行不该回到 GC，那会让它去删已删的字节）", got, want)
	}
	if artifactObjectRowExists(t, db, "obj_purged_free") {
		t.Fatal("已 purged 且无引用的对象行还在（#629：行只增不减）")
	}
	for _, id := range []string{"obj_purged_manifest", "obj_purged_upload", "obj_expired_unacked"} {
		if !artifactObjectRowExists(t, db, id) {
			t.Fatalf("对象行 %s 被误删（仍被引用或还没 ack）", id)
		}
	}
	if n := countArtifactObjectRows(t, db); n != 3 {
		t.Fatalf("第一轮后对象行 = %d, want 3", n)
	}

	// 第二轮（同一时刻）：回收幂等，已被删的行不会被再删一次，也没新增待清理项。
	ids, err = db.ExpireArtifactObjects(ctx, now, 100)
	if err != nil {
		t.Fatalf("expire artifacts (2): %v", err)
	}
	if got := artifactObjectIDs(ids); len(got) != 1 || got[0] != "obj_expired_unacked" {
		t.Fatalf("第二轮待清理 id = %v, want 只有仍然没 ack 的 obj_expired_unacked", got)
	}
	if n := countArtifactObjectRows(t, db); n != 3 {
		t.Fatalf("第二轮后对象行 = %d, want 3（回收必须是幂等的）", n)
	}

	// 等到 ticket 过期：过期 ticket 先被删掉，同一轮里它的对象行就能回收（不留一轮延迟）。
	if _, err := db.ExpireArtifactObjects(ctx, now+400, 100); err != nil {
		t.Fatalf("expire artifacts (3): %v", err)
	}
	if artifactObjectRowExists(t, db, "obj_purged_upload") {
		t.Fatal("ticket 过期后，仅被它引用的已 purged 对象行仍然留在库里")
	}
	if !artifactObjectRowExists(t, db, "obj_purged_manifest") {
		t.Fatal("仍被 manifest 引用的对象行被删了（410 元数据会失去它的身份）")
	}

	// manifest 随任务保留期级联删除之后，最后一轮把它也收掉。
	mustExec(t, db, `DELETE FROM task_artifact_manifests WHERE object_id=?`, "obj_purged_manifest")
	if _, err := db.ExpireArtifactObjects(ctx, now+800, 100); err != nil {
		t.Fatalf("expire artifacts (4): %v", err)
	}
	if artifactObjectRowExists(t, db, "obj_purged_manifest") {
		t.Fatal("manifest 引用消失后，已 purged 的对象行仍然留着")
	}
	if n := countArtifactObjectRows(t, db); n != 1 {
		t.Fatalf("最终对象行 = %d, want 1（只剩没 ack、字节可能还在的 obj_expired_unacked）", n)
	}
	if !artifactObjectRowExists(t, db, "obj_expired_unacked") {
		t.Fatal("还没 ack 的对象行被删了（字节可能还在盘上，GC 需要它继续重试）")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// assertNoFullScan 断言计划里没有 alias 的**整表扫**：`SCAN <alias>` 后面没有 USING。
// `SCAN o USING INDEX …` 是索引扫（只碰工作集），不在此列。
func assertNoFullScan(t *testing.T, tag, alias string, plan []string) {
	t.Helper()
	for _, line := range plan {
		if strings.Contains(line, "SCAN "+alias) && !strings.Contains(line, "USING") {
			t.Errorf("%s: 出现整表扫 `SCAN %s`（扫描集不该包含历史行）:\n%s", tag, alias, strings.Join(plan, "\n"))
		}
	}
}

// TestExpireArtifactObjects_UsesIndex 覆盖 #629 第 1 项的代价判据：
// 到期候选集与物理回收都走部分索引，不是「每轮扫一遍全部历史行 + 临时排序」。
//
// 断言直接跑生产常量（artifactObjectsExpireSQL / artifactObjectsReclaimSQL），
// 不抄副本：抄一份就等于在测一条没人跑的 SQL。
func TestExpireArtifactObjects_UsesIndex(t *testing.T) {
	db, _ := newTestDBFile(t)
	now := Now()

	// 历史包袱：大量已 purged 的行；工作集：少量仍活着的行。
	// ANALYZE 之后计划仍应只碰工作集。
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO task_artifact_objects(id,created_at,expired_at,purged_at) VALUES (?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare seed: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("obj_hist_%03d", i), now-10000+int64(i), now-100, now-50); err != nil {
			t.Fatalf("seed historical object %d: %v", i, err)
		}
	}
	for i := 0; i < 50; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("obj_live_%03d", i), now-1000+int64(i), nil, nil); err != nil {
			t.Fatalf("seed live object %d: %v", i, err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	expirePlan := explainQueryPlan(t, db, artifactObjectsExpireSQL, now, 256)
	t.Logf("EXPLAIN QUERY PLAN（到期候选集）:\n%s", strings.Join(expirePlan, "\n"))
	expire := strings.Join(expirePlan, "\n")
	if !strings.Contains(expire, "idx_task_artifact_objects_live") {
		t.Errorf("到期候选集没有走 idx_task_artifact_objects_live（会退化成全表扫历史行）:\n%s", expire)
	}
	if !strings.Contains(expire, "idx_task_artifact_uploads_object") {
		t.Errorf("uploads 的 NOT EXISTS 没有走 object_id 索引（每行一次 uploads 全表扫）:\n%s", expire)
	}
	assertNoFullScan(t, "到期候选集", "o", expirePlan)
	if strings.Contains(expire, "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("到期候选集要先排序才能取前 256 行（LIMIT 没落在索引 seek 上）:\n%s", expire)
	}

	reclaimPlan := explainQueryPlan(t, db, artifactObjectsReclaimSQL, 256)
	t.Logf("EXPLAIN QUERY PLAN（已 purged 行回收）:\n%s", strings.Join(reclaimPlan, "\n"))
	reclaim := strings.Join(reclaimPlan, "\n")
	if !strings.Contains(reclaim, "idx_task_artifact_objects_purged") {
		t.Errorf("回收扫描没有走 idx_task_artifact_objects_purged（会每轮全表扫）:\n%s", reclaim)
	}
	assertNoFullScan(t, "回收扫描", "o", reclaimPlan)
	if strings.Contains(reclaim, "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("回收扫描要先排序才能取前 256 行:\n%s", reclaim)
	}

	// 两个部分索引必须真的存在且谓词与查询逐字对应（谓词漂移 = 索引失效）。
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`,
		"idx_task_artifact_objects_live").Scan(&ddl); err != nil {
		t.Fatalf("read index ddl: %v", err)
	}
	if !strings.Contains(compactSQL(ddl), "where purged_at is null") {
		t.Fatalf("idx_task_artifact_objects_live 的谓词漂移了（部分索引按谓词匹配）: %s", ddl)
	}
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`,
		"idx_task_artifact_objects_purged").Scan(&ddl); err != nil {
		t.Fatalf("read index ddl: %v", err)
	}
	if !strings.Contains(compactSQL(ddl), "where purged_at is not null") {
		t.Fatalf("idx_task_artifact_objects_purged 的谓词漂移了（部分索引按谓词匹配）: %s", ddl)
	}
}
