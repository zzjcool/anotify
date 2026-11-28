package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是 #629 的 store 侧验收：对象行的生命周期在 purged_at 之后真的结束，两段扫描都落在索引上。

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

// 覆盖：无引用→删；仍被引用→留（ticket 过期后同轮回收）；只 expired 没 ack→不删且返回；重复跑幂等。
func TestExpireArtifactObjects_DeletesPurgedUnreferencedRows(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "artifact-reclaim", 1)
	now := Now()

	// manifest/upload 都有 (task_id,attempt) → task_attempts 的外键，引用分支必须建真任务。
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

	ids, err := db.ExpireArtifactObjects(ctx, now, 100)
	if err != nil {
		t.Fatalf("expire artifacts: %v", err)
	}
	if got, want := artifactObjectIDs(ids), []string{"obj_expired_unacked"}; !equalStrings(got, want) {
		t.Fatalf("第一轮待清理 id = %v, want %v", got, want)
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

	ids, err = db.ExpireArtifactObjects(ctx, now, 100)
	if err != nil {
		t.Fatalf("expire artifacts (2): %v", err)
	}
	if got := artifactObjectIDs(ids); len(got) != 1 || got[0] != "obj_expired_unacked" {
		t.Fatalf("第二轮待清理 id = %v, want 只有仍然没 ack 的 obj_expired_unacked", got)
	}
	if n := countArtifactObjectRows(t, db); n != 3 {
		t.Fatalf("第二轮后对象行 = %d, want 3（回收必须幂等）", n)
	}

	if _, err := db.ExpireArtifactObjects(ctx, now+400, 100); err != nil {
		t.Fatalf("expire artifacts (3): %v", err)
	}
	if artifactObjectRowExists(t, db, "obj_purged_upload") {
		t.Fatal("ticket 过期后，仅被它引用的已 purged 对象行仍然留在库里")
	}
	if !artifactObjectRowExists(t, db, "obj_purged_manifest") {
		t.Fatal("仍被 manifest 引用的对象行被删了（410 元数据会失去它的身份）")
	}

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

// 单轮删除量受 limit 约束：历史积压再大也不会把一轮写事务拖长，余量下一轮继续（幂等 + 收敛）。
func TestExpireArtifactObjects_ReclaimRespectsLimit(t *testing.T) {
	ctx := context.Background()
	db, _ := newTestDBFile(t)
	now := Now()
	for i := 0; i < 3; i++ {
		seedArtifactObjectRow(t, db, fmt.Sprintf("obj_backlog_%d", i), now-1000, now-500, now-400+int64(i))
	}
	for i, want := range []int{1, 0} { // 每轮 limit=2：3 → 1 → 0
		if _, err := db.ExpireArtifactObjects(ctx, now, 2); err != nil {
			t.Fatalf("第 %d 轮 expire artifacts: %v", i+1, err)
		}
		if n := countArtifactObjectRows(t, db); n != want {
			t.Fatalf("第 %d 轮后对象行 = %d, want %d（单轮最多删 limit 行）", i+1, n, want)
		}
	}
}

// assertNoFullScan 断言 alias 上**没有整表扫**：`SCAN <alias>` 后面必须带 USING。
func assertNoFullScan(t *testing.T, tag, alias string, plan []string) {
	t.Helper()
	for _, line := range plan {
		if strings.Contains(line, "SCAN "+alias) && !strings.Contains(line, "USING") {
			t.Errorf("%s: 出现整表扫 `SCAN %s`（扫描集不该包含历史行）:\n%s", tag, alias, strings.Join(plan, "\n"))
		}
	}
}

// 两段扫描都跑生产常量：到期候选走部分索引且无临时排序，uploads 两个 NOT EXISTS 都走索引。
func TestExpireArtifactObjects_UsesIndex(t *testing.T) {
	db, _ := newTestDBFile(t)
	now := Now()

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
	for _, idx := range []string{"idx_task_artifact_uploads_object", "idx_task_artifact_uploads_task"} {
		if !strings.Contains(expire, idx) {
			t.Errorf("uploads 的 NOT EXISTS 没有走 %s（每行一次 uploads 全表扫）:\n%s", idx, expire)
		}
	}
	assertNoFullScan(t, "到期候选集", "o", expirePlan)
	assertNoFullScan(t, "到期候选集", "u", expirePlan)
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
	assertNoFullScan(t, "回收扫描", "u", reclaimPlan)
	if strings.Contains(reclaim, "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("回收扫描要先排序才能取前 256 行:\n%s", reclaim)
	}

	// 部分索引必须真的存在且谓词与查询逐字对应（谓词漂移 = 索引失效）。
	for _, want := range []struct{ name, predicate string }{
		{"idx_task_artifact_objects_live", "where purged_at is null"},
		{"idx_task_artifact_objects_purged", "where purged_at is not null"},
	} {
		var ddl string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, want.name).Scan(&ddl); err != nil {
			t.Fatalf("read index ddl %s: %v", want.name, err)
		}
		if !strings.Contains(compactSQL(ddl), want.predicate) {
			t.Fatalf("%s 的谓词漂移了（部分索引按谓词匹配）: %s", want.name, ddl)
		}
	}
}

// 块 13/14 是纯索引块：记账只到 12 的老 v4 库在 Open 时必须就地补上索引并记到 head（不删库、不丢行）。
func TestMigrationBlocks13And14MigrateOldDatabaseInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v12-objects.db")
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	raw.SetMaxOpenConns(1)
	if err := applyBaselineMigration(raw, set[0], nil); err != nil {
		t.Fatalf("apply baseline only: %v", err)
	}
	for _, m := range set {
		if m.version == baselineVersion || m.version > 12 {
			continue
		}
		if err := applyMigration(raw, m, nil); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}
	mustExec(t, raw, `INSERT INTO task_artifact_objects(id,created_at,expired_at,purged_at) VALUES ('obj_v12',1,1,1)`)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open v12 db（块 13/14 应被应用）: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if v, err := db.SchemaVersion(); err != nil || v != headVersion {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, headVersion)
	}
	for _, idx := range [][2]string{
		{"task_artifact_objects", "idx_task_artifact_objects_live"},
		{"task_artifact_objects", "idx_task_artifact_objects_purged"},
		{"task_artifact_uploads", "idx_task_artifact_uploads_object"},
		{"task_artifact_uploads", "idx_task_artifact_uploads_task"},
	} {
		if n := countIndexRows(t, db, idx[0], idx[1]); n != 1 {
			t.Fatalf("就地迁移后索引 %s 有 %d 条, want 1", idx[1], n)
		}
	}
	if !artifactObjectRowExists(t, db, "obj_v12") {
		t.Fatal("就地迁移丢了老库里的对象行")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	var n int
	if err := db2.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&n); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if n != headVersion {
		t.Fatalf("重开后 schema_version 行数 = %d, want %d（每块只记一次账）", n, headVersion)
	}
}
