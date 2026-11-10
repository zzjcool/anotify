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
func insertTaskSQL(t *testing.T, db *DB, args ...any) error {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO tasks
		   (id, namespace_id, trace_id, capability_id, capability, version, digest, params,
		    phase, attempt, error, node_id, lease_id, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
	return err
}

func TestDispatchFailureSchemaRejectsIllegalNullableShapes(t *testing.T) {
	e := newIngressEnv(t)
	ctx := context.Background()
	_ = ctx
	base := func(id string) []any {
		return []any{id, e.nsID, "trace-schema", nil, "ghost", 0, "", "{}", TaskFailed, 0,
			protocol.ReasonDownstreamMissing, nil, "", 1000}
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
				e.cap.Name, e.cap.Version, "", "{}", TaskPending, 0, "", nil, "", 1000)
		},
		"resolved_zero_version": func() error {
			return insertTaskSQL(t, e.db, "tsk_bad8", e.nsID, "trace-schema", e.cap.ID,
				e.cap.Name, 0, e.cap.Digest, "{}", TaskPending, 0, "", nil, "", 1000)
		},
		"resolved_missing_capability_fk": func() error {
			return insertTaskSQL(t, e.db, "tsk_bad9", e.nsID, "trace-schema", "cap_missing",
				"ghost", 1, "sha256:fake", "{}", TaskPending, 0, "", nil, "", 1000)
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

	// 合法形状：NULL ref + failed/attempt=0 + downstream_missing + 空 digest/Node/lease。
	valid := base("tsk_valid")
	if err := insertTaskSQL(t, e.db, valid...); err != nil {
		t.Fatalf("合法未解析失败行被拒: %v", err)
	}
	got, err := e.db.GetTask(ctx, e.nsID, "tsk_valid")
	if err != nil || got.CapabilityID != "" || got.Phase != TaskFailed {
		t.Fatalf("valid failed row = %+v, %v", got, err)
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
