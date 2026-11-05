package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 本文件覆盖 task_logs（迁移 4）的存储语义：往返一致、幂等覆盖、命名空间隔离、
// 跟随任务级联删除。
//
// 与 server 侧用例的分工：这里只验「库里存什么读什么」；HTTP 形状（404 / 200 空对象 /
// 413）归 internal/server 的 handler 用例。任何一个出问题时的排查方向完全不同。

// taskLogsFixture 建库 + 命名空间 + 一条任务行（task_logs 有外键指向它）。
//
// 用文件后备的库（newTestDBFile）而不是 :memory:：级联删除的判据是
// PRAGMA foreign_keys=ON 下的真实行为，且「关掉再打开」这条幂等性也值得顺带守住。
func taskLogsFixture(t *testing.T) (*DB, string, *Task) {
	t.Helper()
	db, _ := newTestDBFile(t)
	ns, _ := newTestNS(t, db)
	ctx := context.Background()

	capDef := &CapabilityVersion{
		NamespaceID:  ns,
		Name:         "build",
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      []string{},
		Source:       CapSourceCustom,
	}
	if err := db.CreateCapability(ctx, capDef); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	task := &Task{
		NamespaceID:  ns,
		TraceID:      NewTaskID(),
		Path:         []string{capDef.ID},
		CapabilityID: capDef.ID,
		Capability:   capDef.Name,
		Version:      capDef.Version,
		Params:       json.RawMessage(`{}`),
		CapHash:      capDef.ContentHash,
		GraphID:      "grf_test",
	}
	if err := db.CreateTasks(ctx, []*Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return db, ns, task
}

// TestTaskLogsRoundTrip 存什么读什么。
//
// 逐字比对而不是"包含"：日志的用途就是**原文**，任何裁剪/转义/换行归一都会让
// 排障时看到的那一行不是节点上真实的那一行。
func TestTaskLogsRoundTrip(t *testing.T) {
	db, ns, task := taskLogsFixture(t)
	ctx := context.Background()

	// 含中文、换行、制表符、引号，以及采集侧会追加的那行截断标记（原样保存）。
	stdout := "starting build\n\tstep 1 ok\n" +
		"\n…（日志已截断：共 1048576 字节，只保留最后 32768 字节）"
	stderr := `flag provided but not defined: -to\n"quoted"\n`
	in := &TaskLogs{
		TaskID:      task.ID,
		NamespaceID: ns,
		Stdout:      stdout,
		Stderr:      stderr,
		Truncated:   true,
		Bytes:       1048576,
	}
	if err := db.UpsertTaskLogs(ctx, in); err != nil {
		t.Fatalf("upsert task logs: %v", err)
	}
	if in.CreatedAt == 0 {
		t.Error("写入后没有回填 CreatedAt")
	}

	got, err := db.GetTaskLogs(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task logs: %v", err)
	}
	if got.TaskID != task.ID || got.NamespaceID != ns {
		t.Errorf("identity 不一致：task=%q ns=%q", got.TaskID, got.NamespaceID)
	}
	if got.Stdout != stdout {
		t.Errorf("stdout 往返不一致\n  存: %q\n  读: %q", stdout, got.Stdout)
	}
	if got.Stderr != stderr {
		t.Errorf("stderr 往返不一致\n  存: %q\n  读: %q", stderr, got.Stderr)
	}
	if !got.Truncated {
		t.Error("truncated 丢失：前端据此提示「你看的不是全部」")
	}
	if got.Bytes != 1048576 {
		t.Errorf("bytes = %d, want 1048576（截断前的真实字节数）", got.Bytes)
	}
	if got.CreatedAt != in.CreatedAt {
		t.Errorf("createdAt = %d, want %d", got.CreatedAt, in.CreatedAt)
	}

	// 空串 + 全零值与「没日志」必须能区分：这一行是**有日志但为空**。
	if err := db.UpsertTaskLogs(ctx, &TaskLogs{TaskID: task.ID, NamespaceID: ns}); err != nil {
		t.Fatalf("upsert empty logs: %v", err)
	}
	empty, err := db.GetTaskLogs(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get empty task logs: %v", err)
	}
	if empty.Stdout != "" || empty.Stderr != "" || empty.Truncated || empty.Bytes != 0 {
		t.Errorf("空日志不该带回任何内容：%+v", empty)
	}
}

// TestTaskLogsUpsertIsIdempotentOverwrite 同一任务重复上传 = 覆盖，不是第二行。
//
// 重派 / agent 重启补传 / 客户端重试都会走到这里：
// 「幂等」在契约 §2 里的定义就是"直接覆盖"。
func TestTaskLogsUpsertIsIdempotentOverwrite(t *testing.T) {
	db, ns, task := taskLogsFixture(t)
	ctx := context.Background()

	first := &TaskLogs{
		TaskID: task.ID, NamespaceID: ns,
		Stdout: "第一次的 stdout", Stderr: "第一次的 stderr", Bytes: 10, CreatedAt: 111,
	}
	if err := db.UpsertTaskLogs(ctx, first); err != nil {
		t.Fatalf("upsert #1: %v", err)
	}
	second := &TaskLogs{
		TaskID: task.ID, NamespaceID: ns,
		Stdout: "第二次的 stdout", Stderr: "第二次的 stderr", Truncated: true, Bytes: 99, CreatedAt: 222,
	}
	if err := db.UpsertTaskLogs(ctx, second); err != nil {
		t.Fatalf("upsert #2: %v", err)
	}

	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM task_logs WHERE task_id = ?`, task.ID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("task_logs 行数 = %d，期望 1（幂等覆盖，不是追加）", rows)
	}
	got, err := db.GetTaskLogs(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("get task logs: %v", err)
	}
	if got.Stdout != "第二次的 stdout" || got.Stderr != "第二次的 stderr" {
		t.Errorf("覆盖后的内容 = (%q, %q)，期望第二次那份", got.Stdout, got.Stderr)
	}
	if !got.Truncated || got.Bytes != 99 || got.CreatedAt != 222 {
		t.Errorf("覆盖后的元数据 = %+v，期望第二次那份（truncated/bytes/createdAt 也要覆盖）", got)
	}
}

// TestTaskLogsNamespaceIsolation 命名空间隔离：写别人的任务 = 不写，读别人的日志 = 看不见。
//
// 写路径用 ErrNotFound 而不是 ErrForbidden：与 GetTask 同一套口径
// （「有、但属于别人」在单部署里不存在，两者对调用方是同一件事）。
func TestTaskLogsNamespaceIsolation(t *testing.T) {
	db, ns, task := taskLogsFixture(t)
	ctx := context.Background()
	other := newTestExtraNS(t, db, "other-ns")

	err := db.UpsertTaskLogs(ctx, &TaskLogs{TaskID: task.ID, NamespaceID: other, Stdout: "x"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨命名空间写入 err = %v, want ErrNotFound", err)
	}
	if err := db.UpsertTaskLogs(ctx, &TaskLogs{TaskID: task.ID, NamespaceID: ns, Stdout: "mine"}); err != nil {
		t.Fatalf("本命名空间写入: %v", err)
	}
	if _, err := db.GetTaskLogs(ctx, other, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨命名空间读 err = %v, want ErrNotFound", err)
	}
	got, err := db.GetTaskLogs(ctx, ns, task.ID)
	if err != nil {
		t.Fatalf("本命名空间读: %v", err)
	}
	if got.Stdout != "mine" {
		t.Errorf("stdout = %q, want mine", got.Stdout)
	}

	// `*`（AllNamespaces）在写路径上没有定义：必须在 SQL 之前就被挡住。
	if err := db.UpsertTaskLogs(ctx, &TaskLogs{TaskID: task.ID, NamespaceID: AllNamespaces}); !errors.Is(err, ErrNamespaceRequired) {
		t.Errorf("AllNamespaces 写入 err = %v, want ErrNamespaceRequired", err)
	}
	if _, err := db.GetTaskLogs(ctx, AllNamespaces, task.ID); !errors.Is(err, ErrNamespaceRequired) {
		t.Errorf("AllNamespaces 读取 err = %v, want ErrNamespaceRequired", err)
	}
}

// TestTaskLogsMissingTask 任务不存在 → ErrNotFound（handler 折成 404，不是 200 空对象）。
func TestTaskLogsMissingTask(t *testing.T) {
	db, ns, _ := taskLogsFixture(t)
	ctx := context.Background()

	if err := db.UpsertTaskLogs(ctx, &TaskLogs{TaskID: "tsk_nope", NamespaceID: ns}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("upsert 到不存在的任务 err = %v, want ErrNotFound", err)
	}
	if _, err := db.GetTaskLogs(ctx, ns, "tsk_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("读不存在的日志 err = %v, want ErrNotFound", err)
	}
}

// TestTaskLogsCascadeWithTask 任务被删（retention）→ 日志一起走。
//
// 没有级联的话 task_logs 会变成一张只涨不降的孤儿大表（日志是全库最大的 TEXT），
// 而它对应的任务早就没人记得了。
func TestTaskLogsCascadeWithTask(t *testing.T) {
	db, ns, task := taskLogsFixture(t)
	ctx := context.Background()

	if err := db.UpsertTaskLogs(ctx, &TaskLogs{TaskID: task.ID, NamespaceID: ns, Stdout: "boom"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM task_logs WHERE task_id = ?`, task.ID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("任务删除后 task_logs 还有 %d 行，期望级联删除", rows)
	}
}

// TestTaskLogsRejectsInvalidInput 防御性校验：调用方传坏值时不写半行。
func TestTaskLogsRejectsInvalidInput(t *testing.T) {
	db, ns, task := taskLogsFixture(t)
	ctx := context.Background()

	if err := db.UpsertTaskLogs(ctx, nil); err == nil {
		t.Error("nil 输入应当报错")
	}
	if err := db.UpsertTaskLogs(ctx, &TaskLogs{NamespaceID: ns}); err == nil ||
		!strings.Contains(err.Error(), "empty task_id") {
		t.Errorf("空 task_id 应当报错，实际 %v", err)
	}
	if err := db.DeleteTaskLogs(ctx, ns, task.ID); err != nil {
		t.Fatalf("delete（本来没有）应当幂等成功: %v", err)
	}
}

// TestTaskLogsTableShape 表结构就是契约 §3 给的 DDL（迁移 4 不许被「顺手改改」）。
//
// 为什么逐列验：task_id 的主键 / truncated 的默认值 / bytes 的类型都是**语义**——
// 主键丢了就会变成「一个任务多行日志，读哪行看运气」。
func TestTaskLogsTableShape(t *testing.T) {
	db := newTestDB(t)

	if !tableExists(t, db, "task_logs") {
		t.Fatal("迁移 4 没有建出 task_logs")
	}
	cols := columnNames(t, db, "task_logs")
	for _, want := range []string{"task_id", "namespace_id", "stdout", "stderr", "truncated", "bytes", "created_at"} {
		if !cols[want] {
			t.Errorf("task_logs 缺列 %s", want)
		}
	}
	if len(cols) != 7 {
		t.Errorf("task_logs 列数 = %d，期望 7（列集合 = 契约 §3）: %v", len(cols), cols)
	}

	// 外键必须指向 tasks(id) 且带 ON DELETE CASCADE。
	var fkSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'task_logs'`).Scan(&fkSQL); err != nil {
		t.Fatalf("read task_logs ddl: %v", err)
	}
	if !strings.Contains(fkSQL, "REFERENCES tasks(id) ON DELETE CASCADE") {
		t.Errorf("task_logs 的 task_id 必须 REFERENCES tasks(id) ON DELETE CASCADE：\n%s", fkSQL)
	}
}
