package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 迁移 28（P1.5 收尾：删掉 `tasks.out` 整列）的验收。
//
// 三条硬要求：
//  1. 幂等——同一版本重跑不炸，且不会删出第二种形状；
//  2. 升级后的 `tasks` 表**没有 out 列**，其余列与全新库逐列一致（类型/NOT NULL/默认值），
//     外键与索引也没丢（重建路径最容易在这里静默退化）；
//  3. 外键仍然生效：`PRAGMA foreign_key_check` 干净，且
//     `emissions.task_id REFERENCES tasks(id) ON DELETE CASCADE` 删任务时真的级联。
//
// 两条路径都要测：正常引擎走 `ALTER TABLE DROP COLUMN`（快路），
// 引擎拒绝时走表重建（兜底）。兜底那条在真实引擎上不会被触发，所以直接调
// rebuildTasksWithoutOut 打一遍——否则它就是一段没人跑过的代码。

// legacyV27DB 造一个「迁移 27 时代」的库（tasks 上还有 out 列）。
//
// 做法与 legacyV26DB 相同：openWith 注入截断到 27 的迁移集，于是库结构恰好是 v27。
func legacyV27DB(t *testing.T) (path, nsID string, parent, child *Task) {
	t.Helper()
	ctx := context.Background()
	path = filepath.Join(t.TempDir(), "legacy-v27.db")
	old := openAtVersion(t, path, 27)
	if v, err := old.SchemaVersion(); err != nil || v != 27 {
		t.Fatalf("老库版本 = %d, %v; want 27", v, err)
	}
	// 前置：夹具必须真的是「迁移 28 之前」的形状，否则这条用例什么都没测。
	if !columnNames(t, old, "tasks")["out"] {
		t.Fatal("v27 库里 tasks 没有 out 列，夹具不对")
	}

	ns, err := old.RegisterUser(ctx, &User{
		ID: NewUserID(), Username: "u-" + NewUserID(), DisplayName: "老库用户", CreatedAt: Now(),
	})
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	nsID = ns.ID

	capDef := &CapabilityVersion{
		NamespaceID: nsID, Name: "review", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{"fix"},
		Source:       CapSourceCustom,
	}
	if err := old.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	parent = &Task{
		ID: "tsk_v27_parent", NamespaceID: nsID, TraceID: "trc_v27",
		Path: []string{}, CapabilityID: capDef.ID, Capability: capDef.Name, Version: 1,
		Params: json.RawMessage(`{"k":"v"}`), CapHash: capDef.ContentHash, GraphID: "grf_v27",
		Status: TaskPending,
	}
	if err := old.CreateTasks(ctx, []*Task{parent}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	// 子任务（带 parent_task_id）也放一条：重建时父子关系与 JSON 列必须原样留下。
	parentID := parent.ID
	child = &Task{
		ID: "tsk_v27_child", NamespaceID: nsID, TraceID: "trc_v27",
		ParentTaskID: &parentID,
		Path:         []string{"review"}, CapabilityID: capDef.ID, Capability: capDef.Name, Version: 1,
		Params: json.RawMessage(`{"issue":"#42"}`), CapHash: capDef.ContentHash, GraphID: "grf_v27",
		Status: TaskPending,
	}
	if err := old.CreateTasks(ctx, []*Task{child}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	// 老库里这一列还**真的可能有值**（迁移前的写入方留下的历史行）。
	// 直接写 SQL：编译期代码里已经没有 Out 字段了，只能绕过 API 造这个形状。
	mustExec(t, old, `UPDATE tasks SET out = 'fix', reported = 1, summary = '老结果' WHERE id = ?`, parent.ID)

	// 一条 emission：既是要保住的数据，也是级联删除用例的被引用方。
	mustExec(t, old, `INSERT INTO emissions (task_id, seq, port, data, note, routed, pruned, created_at)
		VALUES (?, 1, 'fix', '{}', '', 1, 0, 1000)`, parent.ID)

	if err := old.Close(); err != nil {
		t.Fatalf("close v27 db: %v", err)
	}
	return path, nsID, parent, child
}

// taskColumnShape 是「列名 + 类型 + NOT NULL + 默认值 + 主键位」的逐列形状。
func taskColumnShape(t *testing.T, db *DB) [][5]string {
	t.Helper()
	rows, err := db.Query(
		`SELECT name, type, "notnull", coalesce(dflt_value, '<nil>'), pk FROM pragma_table_info('tasks') ORDER BY cid`)
	if err != nil {
		t.Fatalf("probe tasks columns: %v", err)
	}
	defer rows.Close()
	var out [][5]string
	for rows.Next() {
		var name, typ, dflt string
		var notNull, pk int
		if err := rows.Scan(&name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan tasks column: %v", err)
		}
		out = append(out, [5]string{name, typ, strconv.Itoa(notNull), dflt, strconv.Itoa(pk)})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("probe tasks columns: %v", err)
	}
	return out
}

// fkList 是某张表上声明出来的外键（订单列出来的字段写成可比较的字符串）。
func fkList(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table)
	if err != nil {
		t.Fatalf("probe %s fks: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var tbl, from, to, onDelete string
		if err := rows.Scan(&tbl, &from, &to, &onDelete); err != nil {
			t.Fatalf("scan %s fk: %v", table, err)
		}
		out = append(out, table+"."+from+" → "+tbl+"."+to+" ON DELETE "+onDelete)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("probe %s fks: %v", table, err)
	}
	return out
}

// taskIndexNames 是 tasks 上的索引名集合。
func taskIndexNames(t *testing.T, db *DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='tasks'`)
	if err != nil {
		t.Fatalf("probe task indexes: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("probe task indexes: %v", err)
	}
	return out
}

// assertForeignKeyCheckClean 断言 `PRAGMA foreign_key_check` 没有输出（整库）。
func assertForeignKeyCheckClean(t *testing.T, db *DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var table, parent string
		var rowid int64
		var fk int
		if err := rows.Scan(&table, &rowid, &parent, &fk); err != nil {
			t.Fatalf("scan foreign_key_check: %v", err)
		}
		t.Errorf("foreign_key_check 报出一行：table=%s rowid=%d parent=%s fk=%d", table, rowid, parent, fk)
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	if n != 0 {
		t.Fatalf("foreign_key_check 有 %d 行输出，want 干净", n)
	}
}

// freshTaskShape 造一个**全新库**（从头迁移到 tip）并返回 (列形状, 外键, 索引)。
//
// 为什么要有这条基准：重建路径是手写 DDL，唯一的正确判据是「和全新库长一样」。
// 少了它，DDL 漏一个 DEFAULT / 丢一条 REFERENCES 都不会有任何人发现。
func freshTaskShape(t *testing.T) (cols [][5]string, fks []string, indexes map[string]bool) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return taskColumnShape(t, db), fkList(t, db, "tasks"), taskIndexNames(t, db)
}

// TestMigration28DropsOutColumn 老库升级：out 列没了，数据/外键/索引都还在，幂等。
func TestMigration28DropsOutColumn(t *testing.T) {
	path, nsID, parent, child := legacyV27DB(t)

	up, err := Open(path)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	defer func() { _ = up.Close() }()
	ctx := context.Background()

	// ① 列没了（这是本次迁移的全部目的）
	if cols := columnNames(t, up, "tasks"); cols["out"] {
		t.Fatalf("迁移 28 之后 tasks 仍然有 out 列：%v", cols)
	}

	// ② 其余列一字不差地等于全新库（类型/NOT NULL/默认值/主键位逐列比对）
	wantCols, wantFKs, wantIndexes := freshTaskShape(t)
	gotCols := taskColumnShape(t, up)
	if len(gotCols) != len(wantCols) {
		t.Fatalf("升级后 tasks 有 %d 列, want %d\n got=%v", len(gotCols), len(wantCols), gotCols)
	}
	for i := range wantCols {
		if gotCols[i] != wantCols[i] {
			t.Errorf("第 %d 列 = %v, want %v（列名/类型/NOT NULL/默认值必须与全新库一致）",
				i, gotCols[i], wantCols[i])
		}
	}

	// ③ 外键与索引没丢（重建路径最容易在这里退化）
	gotFKs := fkList(t, up, "tasks")
	if len(gotFKs) != len(wantFKs) {
		t.Fatalf("升级后 tasks 外键 = %v, want %v", gotFKs, wantFKs)
	}
	for i := range wantFKs {
		if gotFKs[i] != wantFKs[i] {
			t.Errorf("外键 %d = %q, want %q", i, gotFKs[i], wantFKs[i])
		}
	}
	gotIndexes := taskIndexNames(t, up)
	for name := range wantIndexes {
		if !gotIndexes[name] {
			t.Errorf("升级后丢了索引 %s（现有 %v）", name, gotIndexes)
		}
	}

	// ④ 数据一行不少、值一字不改
	var (
		gotSummary, gotParams, gotPath, gotCap string
		gotReported                            int
	)
	if err := up.QueryRow(
		`SELECT summary, params, path, capability, reported FROM tasks WHERE id = ?`, parent.ID).
		Scan(&gotSummary, &gotParams, &gotPath, &gotCap, &gotReported); err != nil {
		t.Fatalf("读老任务: %v", err)
	}
	if gotSummary != "老结果" || gotReported != 1 {
		t.Errorf("老任务的 summary/reported = %q/%d, want 老结果/1", gotSummary, gotReported)
	}
	if gotParams != `{"k":"v"}` || gotCap != "review" {
		t.Errorf("老任务的 params/capability = %q/%q, want %q/review", gotParams, gotCap, `{"k":"v"}`)
	}
	if gotPath != `[]` {
		t.Errorf("老任务的 path = %q, want []", gotPath)
	}
	// 父子关系原样保留（parent_task_id 是普通列，但重建时最容易漏）
	var gotParent string
	if err := up.QueryRow(`SELECT parent_task_id FROM tasks WHERE id = ?`, child.ID).Scan(&gotParent); err != nil {
		t.Fatalf("读子任务: %v", err)
	}
	if gotParent != parent.ID {
		t.Errorf("子任务 parent_task_id = %q, want %q", gotParent, parent.ID)
	}
	var n int
	if err := up.QueryRow(`SELECT count(*) FROM tasks WHERE namespace_id = ?`, nsID).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if n != 2 {
		t.Fatalf("升级后 tasks = %d 行, want 2（不许丢行）", n)
	}

	// ⑤ 外键体检干净
	assertForeignKeyCheckClean(t, up)

	// ⑥ 幂等：同一版本 hook 再跑两遍不炸、不改出第二种形状
	tx, err := up.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if err := migrateDropTaskOut(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("第 %d 次 migrateDropTaskOut: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if cols := columnNames(t, up, "tasks"); cols["out"] {
		t.Fatal("重跑之后 out 列又出现了")
	}
	if again := taskColumnShape(t, up); len(again) != len(wantCols) {
		t.Fatalf("重跑后列数 = %d, want %d", len(again), len(wantCols))
	}

	// ⑦ emissions 的级联删除仍然生效（重建路径若把外键弄丢，这里会静默不删）
	if _, err := up.Exec(`DELETE FROM tasks WHERE id = ?`, child.ID); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	if _, err := up.Exec(`DELETE FROM tasks WHERE id = ?`, parent.ID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	var ems int
	if err := up.QueryRow(`SELECT count(*) FROM emissions`).Scan(&ems); err != nil {
		t.Fatalf("count emissions: %v", err)
	}
	if ems != 0 {
		t.Fatalf("删任务后仍有 %d 条 emission：ON DELETE CASCADE 失效了", ems)
	}
	assertForeignKeyCheckClean(t, up)

	// ⑧ tasks 只剩一张（重建路径的中间表不能留在库里）
	if tableExists(t, up, "tasks_drop_out_old") {
		t.Fatal("重建用的中间表 tasks_drop_out_old 留在库里了")
	}
	_ = ctx
}

// TestMigration28RebuildPathMatchesFresh 兜底路径（表重建）必须与快路得到同一个形状。
//
// 真实引擎支持 `DROP COLUMN`，所以 rebuildTasksWithoutOut 在升级路径上不会被触发。
// 直接调它，才能证明「引擎不支持时退化出来的表」和全新库一样：
// 列/外键/索引/数据/级联全都要对上。
func TestMigration28RebuildPathMatchesFresh(t *testing.T) {
	path, nsID, parent, child := legacyV27DB(t)

	// 用 v27 库自己（此时 out 还在）跑重建：它等价于「引擎拒绝 DROP COLUMN」那条分支。
	// openAtVersion(27) 把迁移集截断到 27，所以库停在升级前那一版（不会顺手跑迁移 28）。
	db := openAtVersion(t, path, 27)
	defer func() { _ = db.Close() }()
	if !columnNames(t, db, "tasks")["out"] {
		t.Fatal("前置不成立：重建路径的输入必须是带 out 列的 v27 表")
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := rebuildTasksWithoutOut(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("rebuildTasksWithoutOut: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	wantCols, wantFKs, wantIndexes := freshTaskShape(t)
	gotCols := taskColumnShape(t, db)
	if len(gotCols) != len(wantCols) {
		t.Fatalf("重建后 tasks 有 %d 列, want %d\n got=%v", len(gotCols), len(wantCols), gotCols)
	}
	for i := range wantCols {
		if gotCols[i] != wantCols[i] {
			t.Errorf("第 %d 列 = %v, want %v", i, gotCols[i], wantCols[i])
		}
	}
	gotFKs := fkList(t, db, "tasks")
	if len(gotFKs) != len(wantFKs) {
		t.Fatalf("重建后 tasks 外键 = %v, want %v", gotFKs, wantFKs)
	}
	for i := range wantFKs {
		if gotFKs[i] != wantFKs[i] {
			t.Errorf("外键 %d = %q, want %q", i, gotFKs[i], wantFKs[i])
		}
	}
	gotIndexes := taskIndexNames(t, db)
	for name := range wantIndexes {
		if !gotIndexes[name] {
			t.Errorf("重建后丢了索引 %s（现有 %v）", name, gotIndexes)
		}
	}

	// 数据与级联
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE namespace_id = ?`, nsID).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if n != 2 {
		t.Fatalf("重建后 tasks = %d 行, want 2", n)
	}
	var gotParent string
	if err := db.QueryRow(`SELECT parent_task_id FROM tasks WHERE id = ?`, child.ID).Scan(&gotParent); err != nil {
		t.Fatalf("读子任务: %v", err)
	}
	if gotParent != parent.ID {
		t.Errorf("子任务 parent_task_id = %q, want %q", gotParent, parent.ID)
	}
	assertForeignKeyCheckClean(t, db)
	// 重建之后 emissions 仍然指向 tasks（legacy_alter_table 关掉了 RENAME 的引用重写）
	if got := fkList(t, db, "emissions"); len(got) != 1 || got[0] != "emissions.task_id → tasks.id ON DELETE CASCADE" {
		t.Fatalf("重建后 emissions 外键 = %v, want 指向 tasks 的级联", got)
	}
	if _, err := db.Exec(`DELETE FROM tasks`); err != nil {
		t.Fatalf("delete tasks: %v", err)
	}
	var ems int
	if err := db.QueryRow(`SELECT count(*) FROM emissions`).Scan(&ems); err != nil {
		t.Fatalf("count emissions: %v", err)
	}
	if ems != 0 {
		t.Fatalf("重建后删任务没级联删除 emission（还剩 %d 条）", ems)
	}
}

// TestTasksForeignKeyReferrersAreKnown 记录「谁引用了 tasks」。
//
// 迁移 28 的表重建依赖一个前提：**除 emissions 之外没有别的表引用 tasks**。
// 将来若有人加了一张带 `REFERENCES tasks` 的表，重建路径就会在 DELETE 时触发那条
// 级联（或留下悬空引用）。这条用例把「新增引用者」变成一次显式失败，逼着改迁移。
func TestTasksForeignKeyReferrersAreKnown(t *testing.T) {
	db := newTestDB(t)
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	_ = rows.Close()

	got := map[string]bool{}
	for _, tbl := range tables {
		for _, fk := range fkList(t, db, tbl) {
			if strings.Contains(fk, "→ tasks.") {
				got[tbl] = true
			}
		}
	}
	if len(got) != 1 || !got["emissions"] {
		t.Fatalf("引用 tasks 的表 = %v, want 只有 emissions"+
			"（新增引用者必须同步处理迁移 28 的表重建）", got)
	}
}
