package store

// #141「一次任务执行写 11 次事务，合并到 3 次」的往返一致性测试。
//
// 为什么要在 store 层做「拆开」的对照组，而不是只断言合并入口写进去的值：
// 合并事务最容易出的错不是「写不进去」，而是「七条 UPDATE 里少了一条」——
// 单看合并后的结果很难发现哪一列被漏掉了（少写 summary 和「summary 本来就是空」
// 在读回时长得一模一样）。对照组逐条重放合并前的**真实序列**，
// 两边读回逐字段比对，才能证明「合并」与「拆开」等价（issue #141 的验收条件）。
//
// 对照组只调用签名不变的旧入口（LeaseInstance / AssignTask / Transition /
// StartTask / ExtendLease / ReleaseInstanceByTask），不复制新实现，也不改断言迁就实现。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// pinnedStartAt 是「任务已经在跑」的固定 started_at。
//
// 钉一个常量而不是用 Now()：report 路径对已有 started_at 必须**不覆盖**（COALESCE），
// 只有钉死值才能证明「报告没顺手改开始时间」——用 Now() 的话，覆盖与不覆盖
// 在同一秒内读起来一样。
const pinnedStartAt = int64(1_700_000_000)

// ---------- 夹具 ----------

// mkTxMergeNode 建一台「在线 + 声明该能力 + 一个并发槽位」的节点，返回 node_id。
//
// 每台机器只申报 1 个槽位：容量 = 1，于是「槽位有没有泄漏」这类断言
// 只需要「能不能再派一条」就能观测到，不必去数槽位。
func mkTxMergeNode(t *testing.T, db *DB, nsID string, cap *CapabilityVersion, name string) string {
	t.Helper()
	ctx := context.Background()
	node, _ := mkNode(t, db, nsID, name)
	if _, err := db.ConfirmCapabilities(ctx, nsID, node.ID,
		[]CapabilityRef{{Name: cap.Name, Version: cap.Version}}); err != nil {
		t.Fatalf("confirm capabilities on %s: %v", name, err)
	}
	if err := db.UpdateNodeStatus(ctx, nsID, node.ID, NodeStatusOnline); err != nil {
		t.Fatalf("online %s: %v", name, err)
	}
	if _, err := db.UpsertInstances(ctx, nsID, node.ID,
		[]InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		t.Fatalf("instances %s: %v", name, err)
	}
	return node.ID
}

// mkTxMergeTask 建一条 pending 任务（对照组与合并组各一条，字段完全一样）。
func mkTxMergeTask(t *testing.T, db *DB, nsID, traceID string, cap *CapabilityVersion) *Task {
	t.Helper()
	task := mkTask(nsID, traceID, cap)
	if err := db.CreateTasks(context.Background(), []*Task{task}); err != nil {
		t.Fatalf("create task %s: %v", traceID, err)
	}
	return task
}

// driveToAckedWithLease 把任务推到 acked：占槽位 → assigned（写归属/attempts）→ acked。
//
// 租约显式传参而不是用 Now()+LeaseTTLSec：两条对照任务的 lease_until 必须**同一个数值**，
// 否则「合并 vs 拆开」的 lease_until 比对里混进了时钟差，那条断言等于没比。
func driveToAckedWithLease(t *testing.T, db *DB, nsID, nodeID string, task *Task, lease int64) string {
	t.Helper()
	ctx := context.Background()
	instID, err := db.LeaseInstance(ctx, nodeID, task.ID)
	if err != nil {
		t.Fatalf("lease instance for %s: %v", task.ID, err)
	}
	if err := db.AssignTask(ctx, nsID, task.ID, nodeID, instID, lease); err != nil {
		t.Fatalf("assign %s: %v", task.ID, err)
	}
	if err := db.Transition(ctx, nsID, task.ID, TaskAssigned, TaskAcked); err != nil {
		t.Fatalf("ack %s: %v", task.ID, err)
	}
	return instID
}

// driveToRunningWithLease 在 acked 之上再推一步到 running（StartTask 写 started_at），
// 也就是真实的 ack → progress 时序。
func driveToRunningWithLease(t *testing.T, db *DB, nsID, nodeID string, task *Task, lease int64) string {
	t.Helper()
	instID := driveToAckedWithLease(t, db, nsID, nodeID, task, lease)
	if err := db.StartTask(context.Background(), nsID, task.ID); err != nil {
		t.Fatalf("start %s: %v", task.ID, err)
	}
	return instID
}

// reportResultSplit 是合入 #141 之前的 ReportResult 逐条重放（对照组）。
//
// 与旧实现一一对应：
//  1. assigned/acked 先隐式进 running 并补 started_at（节点跳过 progress 帧时）
//  2. transition 到终态（finished_at 由 transitionGuarded 写）
//  3. details 那一句 UPDATE（out/reported/summary/artifacts/error/finished_at COALESCE）
//  4. 旧 ws handler 里**单独一步**释放槽位（ReleaseInstanceByTask）
//
// 故意保留「4 次独立提交」的原样，因为要证明的正是「合并成 1 次后读回一模一样」。
func reportResultSplit(t *testing.T, db *DB, nsID, taskID, nodeID, status string,
	out *string, reported bool, summary string, artifacts []string, errMsg string) {
	t.Helper()
	ctx := context.Background()

	task, err := db.GetTask(ctx, nsID, taskID)
	if err != nil {
		t.Fatalf("split: get task: %v", err)
	}
	cur := task.Status
	if cur == TaskAssigned || cur == TaskAcked {
		if err := db.Transition(ctx, nsID, taskID, cur, TaskRunning); err != nil {
			t.Fatalf("split: %s → running: %v", cur, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE tasks SET started_at = COALESCE(started_at, ?) WHERE namespace_id = ? AND id = ?`,
			Now(), nsID, taskID); err != nil {
			t.Fatalf("split: set started_at: %v", err)
		}
		cur = TaskRunning
	}
	if cur != TaskRunning {
		t.Fatalf("split: task %s is %q, cannot report", taskID, cur)
	}
	if err := db.Transition(ctx, nsID, taskID, TaskRunning, status); err != nil {
		t.Fatalf("split: running → %s: %v", status, err)
	}

	reportedInt := 0
	if reported {
		reportedInt = 1
	}
	var outVal any
	if reported && out != nil {
		outVal = *out
	}
	if artifacts == nil {
		artifacts = []string{}
	}
	artifactsJSON, err := json.Marshal(artifacts)
	if err != nil {
		t.Fatalf("split: marshal artifacts: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks
		 SET out = ?, reported = ?, summary = ?, artifacts = ?, error = ?,
		     finished_at = COALESCE(finished_at, ?)
		 WHERE namespace_id = ? AND id = ?`,
		outVal, reportedInt, summary, string(artifactsJSON), errMsg, Now(), nsID, taskID); err != nil {
		t.Fatalf("split: details: %v", err)
	}
	// 旧实现里这一步在 ws handler（调用方），不在 store 内
	if nodeID != "" {
		if err := db.ReleaseInstanceByTask(ctx, nodeID, taskID); err != nil {
			t.Fatalf("split: release instance: %v", err)
		}
	}
}

// ---------- 断言助手 ----------

// showInt64Ptr 打印 *int64，nil 显示成 "nil" 而不是地址。
func showInt64Ptr(p *int64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *p)
}

// showStrPtr 打印 *string，nil 显示成 "nil"。
func showStrPtr(p *string) string {
	if p == nil {
		return "nil"
	}
	return *p
}

// assertSameSecond 断言两个时间戳都非空、且落在同一秒（容差 1s）。
//
// 为什么留 1s 容差：Now() 是秒粒度，两次写入若跨过秒边界就天然不同秒——
// 那是时钟的事，不是「合并写错了列」。但 nil 绝不放行：
// 「两边都没写」和「两边都写对」如果都算通过，这条断言就没有存在的意义了。
func assertSameSecond(t *testing.T, what string, merged, split *int64) {
	t.Helper()
	if merged == nil || split == nil {
		t.Errorf("%s: merged=%s split=%s，两边都必须非空", what, showInt64Ptr(merged), showInt64Ptr(split))
		return
	}
	if d := *merged - *split; d < -1 || d > 1 {
		t.Errorf("%s: merged=%d split=%d 相差 %ds（Now() 秒粒度，容差 1s）", what, *merged, *split, d)
	}
}

// assertSlotOfTask 断言节点上唯一的槽位正被 taskID 占用（活着）。
func assertSlotOfTask(t *testing.T, db *DB, nodeID, taskID string) {
	t.Helper()
	sl := onlySlot(t, db, nodeID)
	if sl.BusyTask == nil || *sl.BusyTask != taskID {
		t.Fatalf("node %s slot %d busy_task = %s, want %s", nodeID, sl.Slot, showStrPtr(sl.BusyTask), taskID)
	}
	if !sl.Alive {
		t.Fatalf("node %s slot %d alive = false, want true", nodeID, sl.Slot)
	}
}

// assertSlotReleased 断言节点上再没有槽位挂着 taskID。
func assertSlotReleased(t *testing.T, db *DB, nodeID, taskID string) {
	t.Helper()
	insts, err := db.ListInstances(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list instances of %s: %v", nodeID, err)
	}
	if len(insts) == 0 {
		t.Fatalf("node %s has no instances", nodeID)
	}
	for _, in := range insts {
		if in.BusyTask != nil && *in.BusyTask == taskID {
			t.Errorf("node %s slot %d 仍然挂着 task %s（槽位没释放）", nodeID, in.Slot, taskID)
		}
	}
}

// onlySlot 返回节点上唯一的槽位（夹具每台机器只申报 1 个）。
func onlySlot(t *testing.T, db *DB, nodeID string) *NodeInstance {
	t.Helper()
	insts, err := db.ListInstances(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list instances of %s: %v", nodeID, err)
	}
	if len(insts) != 1 {
		t.Fatalf("node %s 申报了 %d 个槽位, want 1", nodeID, len(insts))
	}
	return insts[0]
}

// assertResultFieldsMatch 逐字段比对「合并 ReportResult」与「拆开四步」读回的任务行。
//
// node_id / instance_id 不在这里比：两条对照任务跑在**不同的机器**上（各自独占 1 个槽位），
// 它们的值本来就不可能相等。这两个字段由调用方按「各落在自己的机器上」断言。
func assertResultFieldsMatch(t *testing.T, merged, split *Task) {
	t.Helper()
	if merged.Status != split.Status {
		t.Errorf("status: merged=%q split=%q", merged.Status, split.Status)
	}
	if merged.Out == nil || split.Out == nil {
		t.Errorf("out: merged=%s split=%s，两边都必须非空（I5：reported=true 且有出口时不许 NULL）",
			showStrPtr(merged.Out), showStrPtr(split.Out))
	} else if *merged.Out != *split.Out {
		t.Errorf("out: merged=%q split=%q", *merged.Out, *split.Out)
	}
	if merged.Reported != split.Reported {
		t.Errorf("reported: merged=%v split=%v", merged.Reported, split.Reported)
	}
	if merged.Summary != split.Summary {
		t.Errorf("summary: merged=%q split=%q", merged.Summary, split.Summary)
	}
	if !slices.Equal(merged.Artifacts, split.Artifacts) {
		t.Errorf("artifacts: merged=%v split=%v", merged.Artifacts, split.Artifacts)
	}
	if merged.Error != split.Error {
		t.Errorf("error: merged=%q split=%q", merged.Error, split.Error)
	}
	if merged.Attempts != split.Attempts {
		t.Errorf("attempts: merged=%d split=%d", merged.Attempts, split.Attempts)
	}
	if !sameInt64Ptr(merged.LeaseUntil, split.LeaseUntil) {
		t.Errorf("lease_until: merged=%s split=%s（报告结果不该动租约，两边必须同一个值）",
			showInt64Ptr(merged.LeaseUntil), showInt64Ptr(split.LeaseUntil))
	}
	assertSameSecond(t, "started_at", merged.StartedAt, split.StartedAt)
	assertSameSecond(t, "finished_at", merged.FinishedAt, split.FinishedAt)
}

// sameInt64Ptr 比较两个 *int64（nil 与 nil 相等）。
func sameInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ---------- 1. ReportResult：合并单事务 vs 拆开四步 ----------

// TestReportResultMatchesSplitWrites 是 #141 最核心的往返一致性用例：
// 「合并成 1 个事务的 ReportResult」与「拆开的多事务序列」写进去的东西必须逐字段一致。
//
// 两种起点各跑一遍：
//   - running：节点正常发过 progress 帧，报告只该动结果字段
//   - acked：节点跳过了 progress 帧，报告要**隐式进 running + 补 started_at**
//     （这条最容易在合并事务时被漏掉：漏了 started_at 任务就永远没有开始时间）
func TestReportResultMatchesSplitWrites(t *testing.T) {
	ctx := context.Background()

	for _, start := range []string{TaskRunning, TaskAcked} {
		start := start
		t.Run("from_"+start, func(t *testing.T) {
			db := newTestDB(t)
			nsID, _ := newTestNS(t, db)
			capv := mkCap(nsID, "review", 1, []string{"fix"})
			if err := db.CreateCapability(ctx, capv); err != nil {
				t.Fatalf("create cap: %v", err)
			}
			// 两条等价任务各占一台机器：合并组与对照组的槽位互相独立，
			// 后面才能分别断言「两条路径的槽位都真的释放了」。
			nodeMerged := mkTxMergeNode(t, db, nsID, capv, "mbp-merged")
			nodeSplit := mkTxMergeNode(t, db, nsID, capv, "mbp-split")
			taskMerged := mkTxMergeTask(t, db, nsID, "trace_merged", capv)
			taskSplit := mkTxMergeTask(t, db, nsID, "trace_split", capv)

			lease := Now() + 3600
			var instMerged, instSplit string
			switch start {
			case TaskAcked:
				instMerged = driveToAckedWithLease(t, db, nsID, nodeMerged, taskMerged, lease)
				instSplit = driveToAckedWithLease(t, db, nsID, nodeSplit, taskSplit, lease)
			case TaskRunning:
				instMerged = driveToRunningWithLease(t, db, nsID, nodeMerged, taskMerged, lease)
				instSplit = driveToRunningWithLease(t, db, nsID, nodeSplit, taskSplit, lease)
			default:
				t.Fatalf("unknown start status %q", start)
			}

			if start == TaskRunning {
				// 已经在跑的任务：把 started_at 钉成同一个固定值，
				// 报告路径（走到 running 就停了）**不该**再改它。
				for _, id := range []string{taskMerged.ID, taskSplit.ID} {
					mustExec(t, db, `UPDATE tasks SET started_at = ? WHERE id = ?`, pinnedStartAt, id)
				}
			} else {
				// acked 起点：起点必须**没有** started_at，
				// 否则「隐式进 running 补 started_at」这条逻辑根本没被触发。
				for _, id := range []string{taskMerged.ID, taskSplit.ID} {
					before, err := db.GetTask(ctx, nsID, id)
					if err != nil {
						t.Fatalf("get %s: %v", id, err)
					}
					if before.StartedAt != nil {
						t.Fatalf("前置不成立：%s 在 acked 起点已有 started_at=%d", id, *before.StartedAt)
					}
				}
			}

			out := "fix"
			artifacts := []string{"run.log", "diff.patch"}
			const summary = "改好了"

			// 合并路径：一次 ReportResult（内部单事务，且内含释放槽位）
			if err := db.ReportResult(ctx, nsID, taskMerged.ID, TaskSucceeded,
				&out, true, summary, artifacts, ""); err != nil {
				t.Fatalf("merged ReportResult: %v", err)
			}
			// 对照组：拆开的多事务序列（旧实现逐条重放）
			reportResultSplit(t, db, nsID, taskSplit.ID, nodeSplit, TaskSucceeded,
				&out, true, summary, artifacts, "")

			merged, err := db.GetTask(ctx, nsID, taskMerged.ID)
			if err != nil {
				t.Fatalf("get merged task: %v", err)
			}
			split, err := db.GetTask(ctx, nsID, taskSplit.ID)
			if err != nil {
				t.Fatalf("get split task: %v", err)
			}

			// ① 两边逐字段比对（合并 = 拆开？）
			assertResultFieldsMatch(t, merged, split)

			// ② 合并后读回的具体期望值（不只两边互比：两组都错成一样也算错）
			if merged.Status != TaskSucceeded {
				t.Errorf("merged status = %q, want succeeded", merged.Status)
			}
			if merged.Out == nil || *merged.Out != "fix" {
				t.Errorf("merged out = %s, want \"fix\"", showStrPtr(merged.Out))
			}
			if !merged.Reported {
				t.Error("merged reported = false, want true（reported=true 是本用例的入参）")
			}
			if merged.Summary != summary {
				t.Errorf("merged summary = %q, want %q", merged.Summary, summary)
			}
			if !slices.Equal(merged.Artifacts, artifacts) {
				t.Errorf("merged artifacts = %v, want %v", merged.Artifacts, artifacts)
			}
			if merged.Error != "" {
				t.Errorf("merged error = %q, want 空", merged.Error)
			}
			if merged.Attempts != 1 {
				t.Errorf("merged attempts = %d, want 1（派发时 +1）", merged.Attempts)
			}
			if merged.LeaseUntil == nil || *merged.LeaseUntil != lease {
				t.Errorf("merged lease_until = %s, want %d", showInt64Ptr(merged.LeaseUntil), lease)
			}
			if merged.NodeID == nil || *merged.NodeID != nodeMerged {
				t.Errorf("merged node_id = %s, want %s", showStrPtr(merged.NodeID), nodeMerged)
			}
			if merged.InstanceID == nil || *merged.InstanceID != instMerged {
				t.Errorf("merged instance_id = %s, want %s", showStrPtr(merged.InstanceID), instMerged)
			}
			if start == TaskRunning {
				if merged.StartedAt == nil || *merged.StartedAt != pinnedStartAt {
					t.Errorf("merged started_at = %s, want %d（报告不该覆盖已有的开始时间）",
						showInt64Ptr(merged.StartedAt), pinnedStartAt)
				}
			} else if merged.StartedAt == nil {
				t.Error("merged started_at = nil，acked 起点报告结果必须补上开始时间")
			}
			if merged.FinishedAt == nil {
				t.Error("merged finished_at = nil，终态必须有结束时间")
			}
			// 对照组同样按自己的机器/槽位断言（防止对照组夹具本身建歪）
			if split.NodeID == nil || *split.NodeID != nodeSplit {
				t.Errorf("split node_id = %s, want %s", showStrPtr(split.NodeID), nodeSplit)
			}
			if split.InstanceID == nil || *split.InstanceID != instSplit {
				t.Errorf("split instance_id = %s, want %s", showStrPtr(split.InstanceID), instSplit)
			}

			// ③ 槽位：合并路径必须自己把槽位放掉（旧实现是 ws handler 单独 Release 的）
			assertSlotReleased(t, db, nodeMerged, taskMerged.ID)
			assertSlotReleased(t, db, nodeSplit, taskSplit.ID)
		})
	}
}

// ---------- 2. LeaseAndAssign：合并单事务 vs 两步 ----------

// TestLeaseAndAssignMatchesSplitWrites 派发路径的往返一致性：
// LeaseAndAssign 一次写进去的（状态 + 归属 + attempts + 槽位）必须与
// 「LeaseInstance 后跟 AssignTask」两步写进去的一致。
func TestLeaseAndAssignMatchesSplitWrites(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	capv := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, capv); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	nodeMerged := mkTxMergeNode(t, db, nsID, capv, "mbp-merged")
	nodeSplit := mkTxMergeNode(t, db, nsID, capv, "mbp-split")
	taskMerged := mkTxMergeTask(t, db, nsID, "trace_merged", capv)
	taskSplit := mkTxMergeTask(t, db, nsID, "trace_split", capv)

	lease := Now() + 3600

	// 合并路径：一次调用写完槽位 + 状态 + 归属
	instMerged, err := db.LeaseAndAssign(ctx, nsID, taskMerged.ID, nodeMerged, lease)
	if err != nil {
		t.Fatalf("LeaseAndAssign: %v", err)
	}
	// 对照组：旧的两步
	instSplit, err := db.LeaseInstance(ctx, nodeSplit, taskSplit.ID)
	if err != nil {
		t.Fatalf("lease instance: %v", err)
	}
	if err := db.AssignTask(ctx, nsID, taskSplit.ID, nodeSplit, instSplit, lease); err != nil {
		t.Fatalf("assign task: %v", err)
	}

	merged, err := db.GetTask(ctx, nsID, taskMerged.ID)
	if err != nil {
		t.Fatalf("get merged task: %v", err)
	}
	split, err := db.GetTask(ctx, nsID, taskSplit.ID)
	if err != nil {
		t.Fatalf("get split task: %v", err)
	}

	// 状态 / attempts / 租约：两边必须一致，且都是派发后的具体值
	if merged.Status != TaskAssigned || split.Status != TaskAssigned {
		t.Fatalf("status = %q / %q, want assigned / assigned", merged.Status, split.Status)
	}
	// attempts 只在派发成功时 +1（契约 §2.1 第 5 条）：两边都必须恰好是 1
	if merged.Attempts != 1 || split.Attempts != 1 {
		t.Errorf("attempts = %d / %d, want 1 / 1", merged.Attempts, split.Attempts)
	}
	if merged.LeaseUntil == nil || split.LeaseUntil == nil {
		t.Fatalf("lease_until = %s / %s, 两边都必须非空",
			showInt64Ptr(merged.LeaseUntil), showInt64Ptr(split.LeaseUntil))
	}
	if *merged.LeaseUntil != lease || *split.LeaseUntil != lease {
		t.Errorf("lease_until = %d / %d, want %d", *merged.LeaseUntil, *split.LeaseUntil, lease)
	}
	// node_id / instance_id：两条任务在不同机器上，按各自期望值断言
	if merged.NodeID == nil || *merged.NodeID != nodeMerged {
		t.Errorf("merged node_id = %s, want %s", showStrPtr(merged.NodeID), nodeMerged)
	}
	if merged.InstanceID == nil || *merged.InstanceID != instMerged {
		t.Errorf("merged instance_id = %s, want 返回的 %s", showStrPtr(merged.InstanceID), instMerged)
	}
	if split.NodeID == nil || *split.NodeID != nodeSplit {
		t.Errorf("split node_id = %s, want %s", showStrPtr(split.NodeID), nodeSplit)
	}
	if split.InstanceID == nil || *split.InstanceID != instSplit {
		t.Errorf("split instance_id = %s, want %s", showStrPtr(split.InstanceID), instSplit)
	}
	// 派发不该顺手写「执行期」字段（否则任务会看起来已经开始/结束了）
	if merged.StartedAt != nil || split.StartedAt != nil {
		t.Errorf("started_at = %s / %s, 派发不该写开始时间",
			showInt64Ptr(merged.StartedAt), showInt64Ptr(split.StartedAt))
	}
	if merged.FinishedAt != nil || split.FinishedAt != nil {
		t.Errorf("finished_at = %s / %s, 派发不该写结束时间",
			showInt64Ptr(merged.FinishedAt), showInt64Ptr(split.FinishedAt))
	}
	if merged.Out != nil || merged.Reported {
		t.Errorf("派发不该写结果字段：out=%s reported=%v", showStrPtr(merged.Out), merged.Reported)
	}

	// 槽位：两边都必须真的占住自己的 taskID，且 alive
	if instMerged == "" {
		t.Error("LeaseAndAssign 返回空 instanceID")
	}
	assertSlotOfTask(t, db, nodeMerged, taskMerged.ID)
	assertSlotOfTask(t, db, nodeSplit, taskSplit.ID)
	// 返回的 instanceID 必须是真正被占住的那个槽位（不是「随便挑了个 id」）
	if sl := onlySlot(t, db, nodeMerged); sl.ID != instMerged {
		t.Errorf("LeaseAndAssign 返回的 instance_id = %s, 但被占住的槽位是 %s", instMerged, sl.ID)
	}
}

// ---------- 3. ProgressTask：三合一 vs StartTask + ExtendLease ----------

// TestProgressTaskMatchesSplitWrites progress 帧的往返一致性：
// 合并后的 ProgressTask 与「StartTask（→running + started_at）+ ExtendLease（续租）」
// 必须写出一模一样的状态；另外钉住两条容易丢的语义：COALESCE 不覆盖开始时间、重复帧幂等。
func TestProgressTaskMatchesSplitWrites(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	capv := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, capv); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	nodeMerged := mkTxMergeNode(t, db, nsID, capv, "mbp-merged")
	nodeSplit := mkTxMergeNode(t, db, nsID, capv, "mbp-split")
	taskMerged := mkTxMergeTask(t, db, nsID, "trace_merged", capv)
	taskSplit := mkTxMergeTask(t, db, nsID, "trace_split", capv)

	lease := Now() + 3600
	driveToAckedWithLease(t, db, nsID, nodeMerged, taskMerged, lease)
	driveToAckedWithLease(t, db, nsID, nodeSplit, taskSplit, lease)

	progressedLease := Now() + 1800

	// 合并路径：一次 ProgressTask（→running + started_at + 续租，一个事务）
	if err := db.ProgressTask(ctx, nsID, taskMerged.ID, progressedLease); err != nil {
		t.Fatalf("ProgressTask: %v", err)
	}
	// 对照组：旧的两步
	if err := db.StartTask(ctx, nsID, taskSplit.ID); err != nil {
		t.Fatalf("start task: %v", err)
	}
	if err := db.ExtendLease(ctx, nsID, taskSplit.ID, progressedLease); err != nil {
		t.Fatalf("extend lease: %v", err)
	}

	merged, err := db.GetTask(ctx, nsID, taskMerged.ID)
	if err != nil {
		t.Fatalf("get merged task: %v", err)
	}
	split, err := db.GetTask(ctx, nsID, taskSplit.ID)
	if err != nil {
		t.Fatalf("get split task: %v", err)
	}

	if merged.Status != TaskRunning || split.Status != TaskRunning {
		t.Fatalf("status = %q / %q, want running / running", merged.Status, split.Status)
	}
	if merged.StartedAt == nil {
		t.Error("merged started_at = nil，progress 帧必须写开始时间")
	}
	assertSameSecond(t, "started_at", merged.StartedAt, split.StartedAt)
	// 租约：两边都必须恰好是本次 progress 带的那个值（续租丢了两边就都不该通过）
	if merged.LeaseUntil == nil || *merged.LeaseUntil != progressedLease {
		t.Errorf("merged lease_until = %s, want %d", showInt64Ptr(merged.LeaseUntil), progressedLease)
	}
	if split.LeaseUntil == nil || *split.LeaseUntil != progressedLease {
		t.Errorf("split lease_until = %s, want %d", showInt64Ptr(split.LeaseUntil), progressedLease)
	}
	// progress 不该动结果字段（提前写结果 = 前端会看到「跑着但已有结果」）
	if merged.Reported || merged.Out != nil || merged.Summary != "" || len(merged.Artifacts) != 0 {
		t.Errorf("progress 不该写结果字段: reported=%v out=%s summary=%q artifacts=%v",
			merged.Reported, showStrPtr(merged.Out), merged.Summary, merged.Artifacts)
	}
	// 槽位仍归该任务（progress 不释放槽位）
	assertSlotOfTask(t, db, nodeMerged, taskMerged.ID)
	assertSlotOfTask(t, db, nodeSplit, taskSplit.ID)

	// 3b. COALESCE 不覆盖已有 started_at + 重复 progress 帧幂等
	t.Run("keeps_started_at_and_is_idempotent", func(t *testing.T) {
		ctx := context.Background()
		db2 := newTestDB(t)
		ns2, _ := newTestNS(t, db2)
		cap2 := mkCap(ns2, "review", 1, []string{"fix"})
		if err := db2.CreateCapability(ctx, cap2); err != nil {
			t.Fatalf("create cap: %v", err)
		}
		nodeID := mkTxMergeNode(t, db2, ns2, cap2, "mbp")
		task := mkTxMergeTask(t, db2, ns2, "trace_again", cap2)

		lease1 := Now() + 3600
		driveToAckedWithLease(t, db2, ns2, nodeID, task, lease1)
		// 人为把 started_at 钉成过去的固定值：COALESCE 语义要求它**不被覆盖**
		mustExec(t, db2, `UPDATE tasks SET started_at = ? WHERE id = ?`, pinnedStartAt, task.ID)

		firstLease := Now() + 1800
		if err := db2.ProgressTask(ctx, ns2, task.ID, firstLease); err != nil {
			t.Fatalf("first ProgressTask: %v", err)
		}
		got, err := db2.GetTask(ctx, ns2, task.ID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.Status != TaskRunning {
			t.Fatalf("status = %q, want running", got.Status)
		}
		if got.StartedAt == nil || *got.StartedAt != pinnedStartAt {
			t.Errorf("started_at = %s, want %d（COALESCE 不得覆盖已有开始时间）",
				showInt64Ptr(got.StartedAt), pinnedStartAt)
		}
		if got.LeaseUntil == nil || *got.LeaseUntil != firstLease {
			t.Errorf("lease_until = %s, want %d", showInt64Ptr(got.LeaseUntil), firstLease)
		}

		// 第二条 progress 帧：状态已是 running，必须幂等（不报错）且仍续租
		secondLease := firstLease + 60
		if err := db2.ProgressTask(ctx, ns2, task.ID, secondLease); err != nil {
			t.Fatalf("第二条 progress 帧必须幂等，却报错: %v", err)
		}
		got2, err := db2.GetTask(ctx, ns2, task.ID)
		if err != nil {
			t.Fatalf("get task after second progress: %v", err)
		}
		if got2.Status != TaskRunning {
			t.Errorf("status = %q, want running", got2.Status)
		}
		if got2.StartedAt == nil || *got2.StartedAt != pinnedStartAt {
			t.Errorf("started_at = %s, want %d（重复帧也不能覆盖开始时间）",
				showInt64Ptr(got2.StartedAt), pinnedStartAt)
		}
		if got2.LeaseUntil == nil || *got2.LeaseUntil != secondLease {
			t.Errorf("lease_until = %s, want %d（重复帧仍要续租）",
				showInt64Ptr(got2.LeaseUntil), secondLease)
		}
	})
}

// ---------- 4. 守卫不丢（合并事务不得放宽任何一条前置条件） ----------

// TestLeaseAndAssignGuardFailureDoesNotLeakSlot 非 pending 任务派发失败时**不能漏槽位**。
//
// 为什么这条必须存在：合并后「占槽位」在事务里最先发生，状态守卫在它之后。
// 实现一旦少了 Rollback（或者把占槽位挪到事务外），守卫失败仍会把节点唯一的
// 槽位标成 busy——这台机器从此接不到任何任务，而且没有任何错误日志指向它。
func TestLeaseAndAssignGuardFailureDoesNotLeakSlot(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	capv := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, capv); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	nodeID := mkTxMergeNode(t, db, nsID, capv, "mbp")
	nonPending := mkTxMergeTask(t, db, nsID, "trace_nonpending", capv)
	// 先把任务挪出 pending（不占槽位，只改状态）：派发入口只接受 pending
	if err := db.Transition(ctx, nsID, nonPending.ID, TaskPending, TaskAssigned); err != nil {
		t.Fatalf("prep: pending → assigned: %v", err)
	}

	lease := Now() + 3600
	if _, err := db.LeaseAndAssign(ctx, nsID, nonPending.ID, nodeID, lease); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("非 pending 任务 LeaseAndAssign err = %v, want ErrInvalidTransition", err)
	}

	// 槽位没有泄漏：这台机器只有 1 个槽位，再派一条 pending 任务必须成功
	okTask := mkTxMergeTask(t, db, nsID, "trace_ok", capv)
	if _, err := db.LeaseAndAssign(ctx, nsID, okTask.ID, nodeID, lease); err != nil {
		t.Fatalf("守卫失败后容量不可用（槽位泄漏了）: %v", err)
	}
	assertSlotOfTask(t, db, nodeID, okTask.ID)

	// 失败那条任务必须一个字都没被写（整事务回滚）：状态、归属、attempts 都不变
	failed, err := db.GetTask(ctx, nsID, nonPending.ID)
	if err != nil {
		t.Fatalf("get non-pending task: %v", err)
	}
	if failed.Status != TaskAssigned {
		t.Errorf("失败任务 status = %q, want assigned（不该被改回 pending 或别的）", failed.Status)
	}
	if failed.Attempts != 0 {
		t.Errorf("失败任务 attempts = %d, want 0（派发失败不得 +1）", failed.Attempts)
	}
	if failed.NodeID != nil || failed.InstanceID != nil || failed.LeaseUntil != nil {
		t.Errorf("失败任务被写了归属: node=%s inst=%s lease=%s",
			showStrPtr(failed.NodeID), showStrPtr(failed.InstanceID), showInt64Ptr(failed.LeaseUntil))
	}
}

// TestReportResultRejectsTerminalTaskWithoutTouchingSlot 终态任务再报结果必须报错，且槽位不动。
//
// 为什么值得存在：合并后 ReportResult 把「释放槽位」放进了同一个事务，
// 若把终态判断挪到释放之后（或干脆去掉），一条重复的 result 帧就能
// 悄悄改掉已经结束的任务、或者释放别人的槽位。
func TestReportResultRejectsTerminalTaskWithoutTouchingSlot(t *testing.T) {
	ctx := context.Background()
	db, nsID, nodeID, _, task := setupRunningTask(t)

	// 直接推进到终态（Transition 只管状态，不碰槽位）→ 制造「终态但槽位还占着」
	if err := db.Transition(ctx, nsID, task.ID, TaskRunning, TaskSucceeded); err != nil {
		t.Fatalf("transition to succeeded: %v", err)
	}
	assertSlotOfTask(t, db, nodeID, task.ID) // 前置：槽位此时仍被该任务占着

	out := "fix"
	err := db.ReportResult(ctx, nsID, task.ID, TaskSucceeded, &out, true, "再来一次", []string{"x"}, "")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态任务 ReportResult err = %v, want ErrInvalidTransition", err)
	}

	// 槽位状态一点都不能被改动
	assertSlotOfTask(t, db, nodeID, task.ID)
	// 结果字段也不能被第二次报告覆盖
	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Summary != "" {
		t.Errorf("summary = %q, 终态任务的结果不该被二次报告改写", got.Summary)
	}
	if !slices.Equal(got.Artifacts, []string{}) {
		t.Errorf("artifacts = %v, 终态任务的结果不该被二次报告改写", got.Artifacts)
	}
}

// TestReportResultDoesNotReleaseForeignSlot 槽位被**别的** taskID 占着时不得释放。
//
// 为什么值得存在：释放语句里的 `busy_task = taskID` 条件看起来多余（"我刚跑完，
// 槽位当然是我的"），但任务被巡检回收后槽位可能已经分给了另一条任务；
// 丢掉这个条件就会把别人正在跑的任务的槽位清空 → 同一台机器超并发执行。
func TestReportResultDoesNotReleaseForeignSlot(t *testing.T) {
	ctx := context.Background()
	db, nsID, nodeID, _, task := setupRunningTask(t)

	const foreignTask = "tsk_someone_else"
	mustExec(t, db, `UPDATE node_instances SET busy_task = ? WHERE node_id = ?`, foreignTask, nodeID)

	out := "fix"
	if err := db.ReportResult(ctx, nsID, task.ID, TaskSucceeded, &out, true, "done", nil, ""); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}
	got, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != TaskSucceeded {
		t.Errorf("status = %q, want succeeded", got.Status)
	}

	sl := onlySlot(t, db, nodeID)
	if sl.BusyTask == nil {
		t.Fatal("槽位被别的任务占着，ReportResult 却把它释放了（busy_task = taskID 条件丢了）")
	}
	if *sl.BusyTask != foreignTask {
		t.Fatalf("槽位 busy_task = %q, want %q（不得被改写）", *sl.BusyTask, foreignTask)
	}
}

// TestProgressTaskRejectsTerminalTask 终态任务的 progress 帧必须报错，不能静默成功。
//
// 为什么值得存在：契约 §2.2 第 2 条允许「状态已是 running 就照常续租」，
// 实现很容易顺手写成「不是终态就放行」；一旦终态也放行，一条已经成功结束的任务
// 会被迟到的 progress 帧续上租约，前端就从「已完成」变成「跑着」。
func TestProgressTaskRejectsTerminalTask(t *testing.T) {
	ctx := context.Background()
	db, nsID, _, _, task := setupRunningTask(t)

	if err := db.Transition(ctx, nsID, task.ID, TaskRunning, TaskSucceeded); err != nil {
		t.Fatalf("transition to succeeded: %v", err)
	}
	before, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if before.LeaseUntil == nil {
		t.Fatalf("前置不成立：running → succeeded 后 lease_until 不该是 NULL")
	}

	err = db.ProgressTask(ctx, nsID, task.ID, Now()+9999)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("终态任务 ProgressTask err = %v, want ErrInvalidTransition（不能静默成功）", err)
	}

	after, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task after progress: %v", err)
	}
	if after.Status != TaskSucceeded {
		t.Errorf("status = %q, want succeeded（终态不该被 progress 改回去）", after.Status)
	}
	if after.LeaseUntil == nil || *after.LeaseUntil != *before.LeaseUntil {
		t.Errorf("lease_until = %s, want %d（失败的整体回滚，不得续租）",
			showInt64Ptr(after.LeaseUntil), *before.LeaseUntil)
	}
}
