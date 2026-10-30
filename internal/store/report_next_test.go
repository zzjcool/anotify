package store

// issue #140「接力派发在后台 goroutine 里，进程退出即丢任务」的 store 层验收用例。
//
// 冻结契约 §1.1 / §3 的必测项：
//  1. 成功路径：一个事务里父任务落终态 + 下一跳子任务行落库（pending / 无归属 / attempts=0）
//  2. 同生共死（核心）：子任务插入失败 → 整体回滚，父任务不在终态、结果字段一个字没写、
//     该 trace 下子任务 0 行（绝不出现「父 succeeded、子任务不存在」的空洞）
//  3. 兼容路径：ReportResult(next=nil) 与旧实现语义一致（I5 的 out 三态、错误文案/哨兵不变）
//  4. 可捞回：成功路径建出的子任务能被 ListRedispatchable 捞到（崩溃点补偿路径）
//
// 本文件只新增用例，不改 task_test.go 里任何既有断言。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mkNextHopTask 构造一条「下一跳」子任务行（还没落库，ID 由 createTasksTx 生成）。
//
// 与调用方（internal/route.PlanNextHop）构造的形状一致：pending + 无归属 + attempts=0
// + parent_task_id 指向刚跑完的父任务。
func mkNextHopTask(nsID, traceID string, cap *CapabilityVersion, parentTaskID string) *Task {
	parent := parentTaskID
	child := mkTask(nsID, traceID, cap)
	child.ParentTaskID = &parent
	return child
}

// ---------- 1. 成功路径 ----------

// TestReportResultAndCreateTasksSuccess 一个事务里父任务终态 + 子任务行都落库。
func TestReportResultAndCreateTasksSuccess(t *testing.T) {
	ctx := context.Background()
	db, nsID, _, cap, parent := setupRunningTask(t)

	child := mkNextHopTask(nsID, parent.TraceID, cap, parent.ID)
	out := "fix"
	if err := db.ReportResultAndCreateTasks(ctx, nsID, parent.ID, TaskSucceeded,
		&out, true, "改好了", []string{"diff.patch"}, "", []*Task{child}); err != nil {
		t.Fatalf("ReportResultAndCreateTasks: %v", err)
	}

	// ① 父任务：终态 + 结果字段全部落库（不是「只改了 status」）
	got, err := db.GetTask(ctx, nsID, parent.ID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if got.Status != TaskSucceeded {
		t.Fatalf("parent status = %q, want succeeded", got.Status)
	}
	if got.Out == nil || *got.Out != "fix" {
		t.Fatalf("parent out = %s, want \"fix\"", showStrPtr(got.Out))
	}
	if !got.Reported {
		t.Error("parent reported = false, want true")
	}
	if got.Summary != "改好了" {
		t.Errorf("parent summary = %q, want \"改好了\"", got.Summary)
	}
	if got.FinishedAt == nil {
		t.Error("parent finished_at = nil，终态必须有结束时间")
	}

	// ② 子任务行：pending + 没有任何归属 + attempts=0 + parent 指对
	if child.ID == "" {
		t.Fatal("createTasksTx 没给子任务生成 ID")
	}
	kid, err := db.GetTask(ctx, nsID, child.ID)
	if err != nil {
		t.Fatalf("get child %s: %v", child.ID, err)
	}
	if kid.Status != TaskPending {
		t.Errorf("child status = %q, want pending", kid.Status)
	}
	if kid.NodeID != nil || kid.InstanceID != nil || kid.LeaseUntil != nil {
		t.Errorf("child 不该有归属：node=%s inst=%s lease=%s",
			showStrPtr(kid.NodeID), showStrPtr(kid.InstanceID), showInt64Ptr(kid.LeaseUntil))
	}
	if kid.Attempts != 0 {
		t.Errorf("child attempts = %d, want 0（派发时才 +1）", kid.Attempts)
	}
	if kid.ParentTaskID == nil || *kid.ParentTaskID != parent.ID {
		t.Errorf("child parent_task_id = %s, want %s",
			showStrPtr(kid.ParentTaskID), parent.ID)
	}
	if kid.TraceID != parent.TraceID {
		t.Errorf("child trace_id = %q, want %q（同一 trace 才叫接力）", kid.TraceID, parent.TraceID)
	}
	if kid.Capability != cap.Name || kid.Version != cap.Version {
		t.Errorf("child capability = %s@%d, want %s@%d",
			kid.Capability, kid.Version, cap.Name, cap.Version)
	}
}

// ---------- 2. 同生共死（核心） ----------

// TestReportResultAndCreateTasksRollsBackOnChildFailure 子任务插入失败 → 整体回滚。
//
// 两种被 createTasksTx 拒绝的方式各跑一遍：
//   - 状态不是 pending（命中「建任务只能落在 pending」校验）
//   - 主键与父任务冲突（命中 primary key 约束，验证的是 DB 层失败也能回滚）
//
// 断言的是「回滚彻底」：不只是 status 没变，父任务的 out/summary/finished_at
// 也不能留下任何痕迹——否则前端会看到「running 但已有结果」的畸形行。
func TestReportResultAndCreateTasksRollsBackOnChildFailure(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name       string
		breakChild func(child, parent *Task)
		// wantErrPart 是本次失败必须命中的错误文案片段（顺带钉住错误文案没被改）
		wantErrPart string
	}{
		{
			name:        "child_not_pending",
			breakChild:  func(child, _ *Task) { child.Status = TaskAssigned },
			wantErrPart: "must start as pending",
		},
		{
			name:        "child_duplicate_id",
			breakChild:  func(child, parent *Task) { child.ID = parent.ID },
			wantErrPart: "insert task 0",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			db, nsID, _, cap, parent := setupRunningTask(t)

			before, err := db.GetTask(ctx, nsID, parent.ID)
			if err != nil {
				t.Fatalf("get parent before: %v", err)
			}
			// 前置：父任务是非终态（否则「父仍在原状态」这条就观测不到）
			if before.Status != TaskRunning {
				t.Fatalf("前置不成立：父任务 status = %q, want running", before.Status)
			}

			child := mkNextHopTask(nsID, parent.TraceID, cap, parent.ID)
			c.breakChild(child, parent)

			out := "fix"
			err = db.ReportResultAndCreateTasks(ctx, nsID, parent.ID, TaskSucceeded,
				&out, true, "改好了", []string{"diff.patch"}, "", []*Task{child})
			if err == nil {
				t.Fatal("子任务插入失败时整体必须报错（不许吞掉）")
			}
			if !strings.Contains(err.Error(), c.wantErrPart) {
				t.Fatalf("err = %v, want 含 %q", err, c.wantErrPart)
			}

			// ① 父任务仍是原状态：不在终态（这是「不出现永久空洞」的另一半）
			after, err := db.GetTask(ctx, nsID, parent.ID)
			if err != nil {
				t.Fatalf("get parent after: %v", err)
			}
			if after.Status != before.Status {
				t.Errorf("父任务 status = %q, want %q（整体回滚，终态不得生效）",
					after.Status, before.Status)
			}
			if IsTerminalStatus(after.Status) {
				t.Errorf("父任务落进了终态 %q，说明终态写入没随子任务失败回滚", after.Status)
			}
			// ② 结果字段一个字都没写（回滚彻底，不是「status 回退了但字段留着」）
			if after.Out != nil {
				t.Errorf("父任务 out = %s, want nil（回滚后不得留结果）", showStrPtr(after.Out))
			}
			if after.Reported {
				t.Error("父任务 reported = true, want false（回滚后不得留回报标记）")
			}
			if after.Summary != "" {
				t.Errorf("父任务 summary = %q, want 空", after.Summary)
			}
			if after.FinishedAt != nil {
				t.Errorf("父任务 finished_at = %s, want nil", showInt64Ptr(after.FinishedAt))
			}
			// ③ 归属 / attempts / 租约也不该被顺手改掉（回滚要覆盖整个事务）
			if after.Attempts != before.Attempts {
				t.Errorf("父任务 attempts = %d, want %d", after.Attempts, before.Attempts)
			}
			if showStrPtr(after.NodeID) != showStrPtr(before.NodeID) {
				t.Errorf("父任务 node_id = %s, want %s",
					showStrPtr(after.NodeID), showStrPtr(before.NodeID))
			}
			if !sameInt64Ptr(after.LeaseUntil, before.LeaseUntil) {
				t.Errorf("父任务 lease_until = %s, want %s",
					showInt64Ptr(after.LeaseUntil), showInt64Ptr(before.LeaseUntil))
			}
			// ④ 该 trace 下仍然只有父任务一行 → 子任务 0 行
			rows, err := db.ListTasks(ctx, nsID, parent.TraceID, "", "", 50)
			if err != nil {
				t.Fatalf("list tasks: %v", err)
			}
			if len(rows) != 1 {
				ids := make([]string, 0, len(rows))
				for _, r := range rows {
					ids = append(ids, r.ID+"("+r.Status+")")
				}
				t.Fatalf("trace 下应有 1 行（父任务），实际 %d 行: %v", len(rows), ids)
			}
			if rows[0].ID != parent.ID {
				t.Fatalf("trace 下唯一一行是 %s, want 父任务 %s", rows[0].ID, parent.ID)
			}
		})
	}
}

// TestReportResultAndCreateTasksRollsBackOnReportFailure 父任务报告失败时也不许建子任务。
//
// 反方向的「同生共死」：终态写失败（任务已是终态 → ErrInvalidTransition）时，
// 子任务行同样不能落库——否则一条重复的 result 帧就能凭空造出下一跳任务。
func TestReportResultAndCreateTasksRollsBackOnReportFailure(t *testing.T) {
	ctx := context.Background()
	db, nsID, _, cap, parent := setupRunningTask(t)

	// 先把父任务推到终态（模拟「重复 result 帧」）
	if err := db.Transition(ctx, nsID, parent.ID, TaskRunning, TaskSucceeded); err != nil {
		t.Fatalf("prep transition: %v", err)
	}

	child := mkNextHopTask(nsID, parent.TraceID, cap, parent.ID)
	out := "fix"
	err := db.ReportResultAndCreateTasks(ctx, nsID, parent.ID, TaskSucceeded,
		&out, true, "再来一次", nil, "", []*Task{child})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态任务再报结果 err = %v, want ErrInvalidTransition", err)
	}

	if child.ID != "" {
		// createTasksTx 没被调用过 → 连 ID 都不该生成（说明失败点确实在它之前）
		t.Errorf("报告失败时不该走到建子任务那一步，却生成了 ID %s", child.ID)
	}
	rows, err := db.ListTasks(ctx, nsID, parent.TraceID, "", "", 50)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("trace 下应有 1 行（父任务），实际 %d 行", len(rows))
	}
}

// ---------- 3. 兼容路径：ReportResult(next=nil) ----------

// TestReportResultCompatWithoutNext 旧入口在新实现上语义不变（I5 三态 + 错误契约）。
func TestReportResultCompatWithoutNext(t *testing.T) {
	ctx := context.Background()

	// reported=false, out=nil → 没回报：out 必须留 NULL（不伪造「到头」）
	t.Run("unreported_leaves_out_null", func(t *testing.T) {
		db, nsID, _, _, task := setupRunningTask(t)
		if err := db.ReportResult(ctx, nsID, task.ID, TaskSucceeded, nil, false, "", nil, ""); err != nil {
			t.Fatalf("ReportResult: %v", err)
		}
		got, err := db.GetTask(ctx, nsID, task.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != TaskSucceeded {
			t.Fatalf("status = %q, want succeeded", got.Status)
		}
		if got.Out != nil {
			t.Errorf("out = %s, want nil（reported=false 不得伪造出口）", showStrPtr(got.Out))
		}
		if got.Reported {
			t.Error("reported = true, want false")
		}
		if got.FinishedAt == nil {
			t.Error("finished_at = nil，终态必须有结束时间")
		}
		// next=nil 意味着不建行：库里只有这一条任务
		rows, err := db.ListTasks(ctx, nsID, task.TraceID, "", "", 50)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("trace 下应有 1 行，实际 %d 行", len(rows))
		}
	})

	// reported=true && out=nil → 明确到头：out 仍是 NULL，但 reported=1
	t.Run("reported_without_port_keeps_null_out", func(t *testing.T) {
		db, nsID, _, _, task := setupRunningTask(t)
		if err := db.ReportResult(ctx, nsID, task.ID, TaskSucceeded, nil, true, "到头了", nil, ""); err != nil {
			t.Fatalf("ReportResult: %v", err)
		}
		got, err := db.GetTask(ctx, nsID, task.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Out != nil {
			t.Errorf("out = %s, want nil（reported=true + out=nil 就是「到头」）", showStrPtr(got.Out))
		}
		if !got.Reported {
			t.Error("reported = false, want true")
		}
		if got.Summary != "到头了" {
			t.Errorf("summary = %q, want \"到头了\"", got.Summary)
		}
	})

	// 失败终态也走同一条路（failed 是唯一的另一个合法终态）
	t.Run("failed_status", func(t *testing.T) {
		db, nsID, _, _, task := setupRunningTask(t)
		out := "fix"
		if err := db.ReportResult(ctx, nsID, task.ID, TaskFailed, &out, true, "炸了", nil, "boom"); err != nil {
			t.Fatalf("ReportResult(failed): %v", err)
		}
		got, err := db.GetTask(ctx, nsID, task.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != TaskFailed {
			t.Errorf("status = %q, want failed", got.Status)
		}
		if got.Error != "boom" {
			t.Errorf("error = %q, want \"boom\"", got.Error)
		}
	})

	// 错误文案 / 哨兵值不变：ReportResult 现在是薄壳，这些必须原样透出
	t.Run("error_contract_unchanged", func(t *testing.T) {
		db, nsID, _, _, task := setupRunningTask(t)

		err := db.ReportResult(ctx, nsID, task.ID, TaskRunning, nil, false, "", nil, "")
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("非法终态 err = %v, want ErrInvalidTransition", err)
		}
		if err != nil && !strings.Contains(err.Error(), "result status must be succeeded|failed") {
			t.Errorf("非法终态 err = %v, want 含原有文案", err)
		}

		empty := ""
		err = db.ReportResult(ctx, nsID, task.ID, TaskSucceeded, &empty, true, "", nil, "")
		if err == nil || !strings.Contains(err.Error(), "empty out string is not a valid port") {
			t.Errorf("空出口名 err = %v, want 原有文案", err)
		}

		if err := db.ReportResult(ctx, nsID, "tsk_missing", TaskSucceeded, nil, false, "", nil, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("任务不存在 err = %v, want ErrNotFound", err)
		}

		// 上面两次失败都不该产生任何副作用 → 任务状态一点没动
		got, err := db.GetTask(ctx, nsID, task.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != TaskRunning {
			t.Errorf("status = %q, want running（失败的报告不得半途写库）", got.Status)
		}

		// CreateTasks 空入参依旧是 no-op（拆事务后这条语义不能丢）
		if err := db.CreateTasks(ctx, nil); err != nil {
			t.Errorf("CreateTasks(nil) = %v, want nil", err)
		}
		if err := db.CreateTasks(ctx, []*Task{}); err != nil {
			t.Errorf("CreateTasks([]) = %v, want nil", err)
		}
	})
}

// ---------- 4. 可捞回：崩溃点补偿路径 ----------

// TestReportResultAndCreateTasksChildIsRedispatchable 事务提交后 publish 丢失也能救回来。
//
// 这是 #140 的补偿路径：子任务行落库为 pending + node_id IS NULL，谁都没派它也没关系，
// internal/lease 的巡检 sweeper 每 5s tick 用 ListRedispatchable 把它捞回来重派。
func TestReportResultAndCreateTasksChildIsRedispatchable(t *testing.T) {
	ctx := context.Background()
	db, nsID, _, cap, parent := setupRunningTask(t)

	child := mkNextHopTask(nsID, parent.TraceID, cap, parent.ID)
	out := "fix"
	if err := db.ReportResultAndCreateTasks(ctx, nsID, parent.ID, TaskSucceeded,
		&out, true, "改好了", nil, "", []*Task{child}); err != nil {
		t.Fatalf("ReportResultAndCreateTasks: %v", err)
	}

	// 模拟「publish 随进程消失」：不调用任何派发逻辑，直接问巡检能捞到什么
	list, err := db.ListRedispatchable(ctx, Now(), 100)
	if err != nil {
		t.Fatalf("list redispatchable: %v", err)
	}
	var found *Task
	for _, x := range list {
		if x.ID == child.ID {
			found = x
			break
		}
	}
	if found == nil {
		ids := make([]string, 0, len(list))
		for _, x := range list {
			ids = append(ids, x.ID+"("+x.Status+")")
		}
		t.Fatalf("子任务 %s 不在 ListRedispatchable 里（崩溃点补偿路径断了）；捞到: %v",
			child.ID, ids)
	}
	if found.Status != TaskPending {
		t.Errorf("可重派的子任务 status = %q, want pending", found.Status)
	}
	if found.NodeID != nil {
		t.Errorf("可重派的子任务 node_id = %s, want nil", showStrPtr(found.NodeID))
	}
	if found.Attempts != 0 {
		t.Errorf("可重派的子任务 attempts = %d, want 0", found.Attempts)
	}
	if found.NotBefore != nil {
		t.Errorf("新建的下一跳不该带退避（应立即重派）：not_before = %d", *found.NotBefore)
	}

	// 父任务已经是终态，不该出现在重派池里（链的头不能被重跑）
	for _, x := range list {
		if x.ID == parent.ID {
			t.Fatalf("终态父任务 %s 出现在 ListRedispatchable 里", parent.ID)
		}
	}
}
