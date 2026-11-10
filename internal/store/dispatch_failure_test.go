package store

// dispatch_failure_test.go 是 S1-D1 的白盒验收：
//
//   - 未解析失败 Task 在 GetTask / ListTasks / trace 读路径可见（NULL ref 不隐藏、不 scan 失败）；
//   - 显式/省略 version 的失败往返；digest 空、无 Node/lease/过程时间/结果事实；
//   - 表上 CHECK 拒绝非法 nullable 形状；resolved ref 的 FK/真实 pin 不被弱化；
//   - 普通 pending 建行/派发/授权入口拒绝未解析行。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// seedDispatchFailures 用原子接收入口写两条失败事实（一条显式 version=2、一条省略）。
func seedDispatchFailures(t *testing.T) (*ingressEnv, string, []string) {
	t.Helper()
	e := newIngressEnv(t)
	ctx := context.Background()
	raw, _, err := e.db.AcceptSourceEvent(ctx, SourceEventInputs{
		NamespaceID: e.nsID, SourceID: e.source.ID,
		ExpectedSourceResourceVersion: e.source.ResourceVersion,
		TraceID:                       "trace-failures", Type: "entry", Now: 1000,
		Failures: []DispatchFailureInputs{
			{Capability: "ghost@2", Version: 2, Detail: "declared version"},
			{Capability: "ghost", Detail: "omitted version"},
		},
	})
	if err != nil {
		t.Fatalf("seed failures: %v", err)
	}
	var receipt SourceEventReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	ids := make([]string, 0, len(receipt.FailedTasks))
	for _, f := range receipt.FailedTasks {
		ids = append(ids, f.TaskID)
	}
	return e, receipt.TraceID, ids
}

func TestDispatchFailureTaskReadPaths(t *testing.T) {
	e, traceID, ids := seedDispatchFailures(t)
	ctx := context.Background()
	if len(ids) != 2 {
		t.Fatalf("failed tasks = %v", ids)
	}

	for i, wantVersion := range []int{2, 0} {
		got, err := e.db.GetTask(ctx, e.nsID, ids[i])
		if err != nil {
			t.Fatalf("get failed task %d: %v", i, err)
		}
		if got.CapabilityID != "" || got.Phase != TaskFailed || got.Attempt != 0 ||
			got.Version != wantVersion || got.Digest != "" || got.Error != protocol.ReasonDownstreamMissing {
			t.Fatalf("failed task %d = %+v (want version %d)", i, got, wantVersion)
		}
		if got.ExitCode != nil || got.StartedAt != nil || len(got.ResultJSON) != 0 {
			t.Fatalf("failed task %d 编造了过程/结果事实: %+v", i, got)
		}
	}

	list, err := e.db.ListTasks(ctx, e.nsID, TaskFilter{Phase: TaskFailed, TraceID: traceID})
	if err != nil || len(list) != 2 {
		t.Fatalf("ListTasks(failed) = %d, %v", len(list), err)
	}
	byTrace, err := e.db.ListTasksByTrace(ctx, e.nsID, traceID)
	if err != nil || len(byTrace) != 2 {
		t.Fatalf("ListTasksByTrace = %d, %v", len(byTrace), err)
	}
	counts, err := e.db.CountTasksByPhase(ctx, e.nsID, traceID)
	if err != nil || counts[TaskFailed] != 2 {
		t.Fatalf("CountTasksByPhase = %+v, %v", counts, err)
	}
	// 失败行永远不是派发候选。
	dispatchable, err := e.db.ListDispatchableTasks(ctx, 2000, 100)
	if err != nil {
		t.Fatalf("list dispatchable: %v", err)
	}
	for _, task := range dispatchable {
		if task.ID == ids[0] || task.ID == ids[1] {
			t.Fatalf("未解析失败任务出现在派发候选里: %s", task.ID)
		}
	}
}

// insertTaskSQL 直插一行 tasks（合法性由表上的 CHECK/FK 判定）。
//
// 列顺序就是调用方的位置参数下标（0..17）：前 14 列是原有事实面，末尾 4 列
// （summary/artifacts/result_accepted_at/finished_at，下标 14..17）是 R2 新钉的
// **伪事实面**——它们没有任何 Node 过程来源，未解析失败行只能是空/真实 server 时刻。
func insertTaskSQL(t *testing.T, db *DB, args ...any) error {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO tasks
		   (id, namespace_id, trace_id, capability_id, capability, version, digest, params,
		    phase, attempt, error, node_id, lease_id, created_at,
		    summary, artifacts, result_accepted_at, finished_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
	return err
}

func TestDispatchFailureSchemaRejectsIllegalNullableShapes(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	_ = ctx
	base := func(id string) []any {
		return []any{id, e.nsID, "trace-schema", nil, "ghost", 0, "", "{}", TaskFailed, 0,
			protocol.ReasonDownstreamMissing, nil, "", 1000,
			"", "[]", nil, int64(1000)}
	}
	cases := map[string]func() error{
		"null_ref_pending": func() error {
			args := base("tsk_bad1")
			args[8] = TaskPending
			return insertTaskSQL(t, e.db, args...)
		},
		"null_ref_attempt": func() error {
			args := base("tsk_bad2")
			args[9] = 1
			return insertTaskSQL(t, e.db, args...)
		},
		"null_ref_wrong_error": func() error {
			args := base("tsk_bad3")
			args[10] = "boom"
			return insertTaskSQL(t, e.db, args...)
		},
		"null_ref_fake_digest": func() error {
			args := base("tsk_bad4")
			args[6] = "sha256:fake"
			return insertTaskSQL(t, e.db, args...)
		},
		"null_ref_node_fact": func() error {
			args := base("tsk_bad5")
			args[11] = "nd_fake"
			return insertTaskSQL(t, e.db, args...)
		},
		"null_ref_lease_fact": func() error {
			args := base("tsk_bad6")
			args[12] = "lse_fake"
			return insertTaskSQL(t, e.db, args...)
		},
		"resolved_empty_digest": func() error {
			return insertTaskSQL(t, e.db, "tsk_bad7", e.nsID, "trace-schema", e.cap.ID,
				e.cap.Name, e.cap.Version, "", "{}", TaskPending, 0, "", nil, "", 1000,
				"", "[]", nil, int64(1000))
		},
		"resolved_zero_version": func() error {
			return insertTaskSQL(t, e.db, "tsk_bad8", e.nsID, "trace-schema", e.cap.ID,
				e.cap.Name, 0, e.cap.Digest, "{}", TaskPending, 0, "", nil, "", 1000,
				"", "[]", nil, int64(1000))
		},
		"resolved_missing_capability_fk": func() error {
			return insertTaskSQL(t, e.db, "tsk_bad9", e.nsID, "trace-schema", "cap_missing",
				"ghost", 1, "sha256:fake", "{}", TaskPending, 0, "", nil, "", 1000,
				"", "[]", nil, int64(1000))
		},
		// R2：伪事实面——未解析失败行不可能有 Node 过程产物/被接受的结果/伪版本。
		"null_ref_pseudo_summary": func() error {
			args := base("tsk_bad10")
			args[14] = "中枢伪造的摘要"
			return insertTaskSQL(t, e.db, args...)
		},
		"null_ref_pseudo_artifacts": func() error {
			args := base("tsk_bad11")
			args[15] = `[{"name":"fake"}]`
			return insertTaskSQL(t, e.db, args...)
		},
		"null_ref_pseudo_result_accepted": func() error {
			args := base("tsk_bad12")
			args[16] = int64(1000)
			return insertTaskSQL(t, e.db, args...)
		},
		// finished_at 缺失 → retention 的 COALESCE(result_accepted_at, finished_at)
		// 为 NULL → 该行永远不被回收（永久垃圾行）。
		"null_ref_missing_finished": func() error {
			args := base("tsk_bad13")
			args[17] = nil
			return insertTaskSQL(t, e.db, args...)
		},
		"null_ref_negative_version": func() error {
			args := base("tsk_bad14")
			args[5] = -1
			return insertTaskSQL(t, e.db, args...)
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil {
				t.Fatalf("非法形状 %s 被 SQLite 接受了", name)
			}
			var n int
			if err := e.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE trace_id = 'trace-schema'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("非法插入留下了 %d 行", n)
			}
		})
	}

	// 合法形状：NULL ref + failed/attempt=0 + downstream_missing + 空 digest/Node/lease
	// + 空 summary/artifacts/无结果接受 + 真实 server 完成时刻 + version=0。
	valid := base("tsk_valid")
	if err := insertTaskSQL(t, e.db, valid...); err != nil {
		t.Fatalf("合法未解析失败行被拒: %v", err)
	}
	got, err := e.db.GetTask(ctx, e.nsID, "tsk_valid")
	if err != nil || got.CapabilityID != "" || got.Phase != TaskFailed {
		t.Fatalf("valid failed row = %+v, %v", got, err)
	}
}

// TestDispatchFailureTaskProjectsNoPseudoFacts 是 R2 的**读路径阴性**（同时覆盖 issue
// R5 的第 ④ 项）：真实接收入口造出的 NULL-ref 失败行，经 GetTask /
// ListTasksByTrace / ListTasks 读回来时不得投影出任何伪过程/伪结果事实。
//
// 为什么读路径也要钉：这些列会原样投影到 HTTP（task.go taskColumns/scanTask），
// 表上 CHECK 只挡「写不进去」，读侧还要证明「从真实入口写进去的就是空」。
func TestDispatchFailureTaskProjectsNoPseudoFacts(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	raw, first, err := e.db.AcceptSourceEvent(ctx, e.inputs("deliver-nullref-read", "trace-nullref-read", "entry", 1000))
	if err != nil || !first {
		t.Fatalf("accept: first=%v err=%v", first, err)
	}
	var receipt SourceEventReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if len(receipt.FailedTasks) != 1 {
		t.Fatalf("receipt failed tasks = %+v", receipt.FailedTasks)
	}
	id := receipt.FailedTasks[0].TaskID

	assertNoPseudo := func(where string, got *Task) {
		t.Helper()
		if got.Summary != "" {
			t.Fatalf("%s 伪 summary: %q", where, got.Summary)
		}
		if len(got.Artifacts) != 0 {
			t.Fatalf("%s 伪 artifacts: %+v", where, got.Artifacts)
		}
		if got.ResultAcceptedAt != nil {
			t.Fatalf("%s 伪 resultAcceptedAt: %d", where, *got.ResultAcceptedAt)
		}
		if got.ExitCode != nil {
			t.Fatalf("%s 伪 exitCode: %d", where, *got.ExitCode)
		}
		if got.StartedAt != nil {
			t.Fatalf("%s 伪 startedAt（Node 过程时间）: %d", where, *got.StartedAt)
		}
		if string(got.ResultJSON) != "" {
			t.Fatalf("%s 伪 resultJson: %s", where, got.ResultJSON)
		}
		if got.Reported || got.Attempt != 0 || got.Phase != TaskFailed || got.CapabilityID != "" {
			t.Fatalf("%s 形状不是未解析失败行: %+v", where, got)
		}
		// 反向要求：真实 server 完成时刻必须存在，否则 retention 永远回收不到它。
		if got.FinishedAt == nil {
			t.Fatalf("%s 缺真实 server 完成时刻（会变成永久垃圾行）: %+v", where, got)
		}
	}

	got, err := e.db.GetTask(ctx, e.nsID, id)
	if err != nil {
		t.Fatalf("get failed task: %v", err)
	}
	assertNoPseudo("GetTask", got)

	byTrace, err := e.db.ListTasksByTrace(ctx, e.nsID, receipt.TraceID)
	if err != nil {
		t.Fatalf("list tasks by trace: %v", err)
	}
	seen := 0
	for _, task := range byTrace {
		if task.ID != id {
			continue
		}
		assertNoPseudo("ListTasksByTrace", task)
		seen++
	}
	if seen != 1 {
		t.Fatalf("ListTasksByTrace 里失败行出现 %d 次（trace %s）", seen, receipt.TraceID)
	}

	list, err := e.db.ListTasks(ctx, e.nsID, TaskFilter{TraceID: receipt.TraceID, Limit: 50})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	seen = 0
	for _, task := range list {
		if task.ID == id {
			assertNoPseudo("ListTasks", task)
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("ListTasks 里失败行出现 %d 次", seen)
	}

	// 0 写：这类行永远不能产生 attempt 行（GrantLeaseAuthorized 的 phase/能力行守卫
	// 见 TestDispatchFailureNotDispatchable；这里补一条「表里真的没有 attempt」的计数）。
	var attempts int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM task_attempts WHERE task_id = ?`, id).Scan(&attempts); err != nil {
		t.Fatalf("count task_attempts: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("未解析失败行有 %d 条 attempt 行", attempts)
	}
}

func TestCreateTasksRejectsUnresolvedAndUnpinned(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()

	task := func(mut func(*Task)) *Task {
		t := e.normalTask()
		t.TraceID = "trace-pin"
		if mut != nil {
			mut(t)
		}
		return t
	}
	for name, mut := range map[string]func(*Task){
		"no_capability": func(t *Task) { t.CapabilityID = "" },
		"no_digest":     func(t *Task) { t.Digest = "" },
		"zero_version":  func(t *Task) { t.Version = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			if err := e.db.CreateTasks(ctx, []*Task{task(mut)}); err == nil {
				t.Fatalf("普通建行接受了 %s", name)
			}
			if n, err := e.db.CountTasksByTrace(ctx, e.nsID, "trace-pin"); err != nil || n != 0 {
				t.Fatalf("拒绝路径留下了写: %d %v", n, err)
			}
		})
	}
	if err := e.db.CreateTasks(ctx, []*Task{task(nil)}); err != nil {
		t.Fatalf("完整 pin 的正常任务应接受: %v", err)
	}
}

func TestDispatchFailureNotDispatchable(t *testing.T) {
	e, _, ids := seedDispatchFailures(t)
	ctx := context.Background()

	if err := e.db.AssignTask(ctx, ids[0], "nd_x", "asg_x", 1); err == nil {
		t.Fatal("AssignTask 接受了未解析失败任务")
	}
	node := v4Node(t, e.db, "fail-node", 1)
	_, _, _, _, err := e.db.GrantLeaseAuthorized(ctx, v4Auth(t, e.db, node.ID), ids[0], "asg_x", 1, 0, Now())
	if err == nil {
		t.Fatal("GrantLeaseAuthorized 给未解析失败任务授了执行权")
	}
	if _, err := e.db.GetTaskAttempt(ctx, ids[0], 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败任务不能产生 attempt 行: %v", err)
	}
	got, err := e.db.GetTask(ctx, e.nsID, ids[0])
	if err != nil || got.Phase != TaskFailed || got.Attempt != 0 || got.LeaseID != "" {
		t.Fatalf("拒绝路径改动了失败任务: %+v, %v", got, err)
	}
	if err := e.db.RequestCancel(ctx, ids[0], protocol.ReasonCancelledByUser, 2000); err == nil {
		t.Fatal("RequestCancel 接受了终态未解析失败任务")
	}
}
