package ws

// 本文件是 P1.5（硬切 `.anotify-out` / `result.out`）在 ws 帧层的验收。
//
// 改动前它叫 node_next_test.go，守的是 #140 那套「result.out → 算下一跳计划 →
// 与父任务终态同一个事务建子任务行 → 后台 publish」的语义。P1.5 把 out 整条删掉
// 之后，那些用例的**主语消失了**——但它们守的性质没有消失，只是换了入口：
//
//	旧：result 带 out=fix  → 建下一跳（本文件原来的五条用例）
//	新：运行中 emit port=fix → 建下一跳（route.Emit；见 node_emit_test.go）
//
// 所以本文件的职责反转成「证明那条老入口真的没了」，并且**不是放宽**：
//
//	1. 老帧带 out（甚至带一个真的有边、真有下游的 port）→ 一个下游都不建；
//	2. 结果帧该做的事照旧（终态 + summary 落库），只是不再碰路由；
//	3. 「不回报出口 = 到此为止」的语义保留，只是载体从 `out IS NULL` 变成
//	   「这个任务没有任何 emission」；
//	4. I4 的形式保留：失败**之前** emit 过的下游照样在（失败不特殊）。
//
// 原来这里还有一个测试双，用来断言「接力派发的两个方法（算计划 / 补推帧）一次都
// 没被调用」。那两个方法已随 P1.5 收尾（迁移 28 + 整条退役）从仓库消失，
// 「没被调用」不再有主语；同一件事现在由**更强的形状**表达：
// 老帧带 out 时下游 0 条、下游能力的任务行 0 条、节点一个帧都收不到，再加上
// `tasks` 表连 `out` 列都没有（assertNoOutColumn）。
//
// 三条事务性质（终态与子任务行同生共死 / 巡检能捞回 / plan 失败不留终态）**没有
// 删掉**，而是搬到了它现在真正所在的入口：route.Emit 自己的事务
// （internal/route 的 emit 单测）、store.ReportResultAndCreateTasks 的事务语义
// （internal/store/report_next_test.go）。ws 这一层不再有「两段提交」这种东西。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// sentResult 发一帧结果（**没有** out 参数：帧里已经没有这个字段了）。
func sentResult(t *testing.T, conn *websocket.Conn, taskID, status, summary string) {
	t.Helper()
	send(t, conn, &ResultFrame{
		Type: NodeFrameResult, TaskID: taskID, Status: status, Summary: summary,
	})
}

// childrenOf 读出某条任务建出的直接下游（库里的父任务关系）。
func childrenOf(t *testing.T, f *nodeFixture, parentTaskID string) []*store.Task {
	t.Helper()
	rows, err := f.db.ListTasks(context.Background(), f.nsID, "", "", "", 200)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	var kids []*store.Task
	for _, row := range rows {
		if row.ParentTaskID != nil && *row.ParentTaskID == parentTaskID {
			kids = append(kids, row)
		}
	}
	return kids
}

// assertNoOutColumn 断言 `tasks` 表上**没有** out 列（迁移 28 的落地证据）。
//
// 为什么在 ws 用例里也查一次：这条不变量的表现形式曾经是「列在、但恒 NULL」，
// 现在要求它连列都不在——只看 Task 结构体是不够的，老库升级路径漏了迁移就仍然
// 有一列在那儿（`SELECT out` 照样能跑）。
func assertNoOutColumn(t *testing.T, f *nodeFixture) {
	t.Helper()
	var n int
	if err := f.db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('tasks') WHERE name = 'out'`).Scan(&n); err != nil {
		t.Fatalf("probe tasks.out: %v", err)
	}
	if n != 0 {
		t.Fatal("tasks 表仍然有 out 列：迁移 28 没生效（出口的唯一载体是 emission）")
	}
}

// ---------- 验收 ①：老帧带 out 不再产生任何下游（「这条腿真断了」） ----------

// TestResultLegacyOutCreatesNoDownstream 是 P1.5 的核心证据。
//
// 用**原始 JSON** 发一帧带 `"out":"fix"` 的 result（模拟老 agent / 手写客户端）。
// 这一帧在改动前会走出一条真有边的出口（review --fix--> fixer），建出下游任务并
// 推 assign；改动后它只能是「一帧普通的结果帧」：
//
//   - 父任务照常 succeeded（结果帧该做的事没退化）；
//   - `tasks` 表上根本没有 out 列（迁移 28；比原来的「out 恒 NULL」更强）；
//   - 下游 0 条（emit 是唯一入口，而这条任务没有 emit 过）；
//   - 节点收不到任何帧（没有 assign、没有 error）。
func TestResultLegacyOutCreatesNoDownstream(t *testing.T) {
	ctx := context.Background()
	f, fixer := emitFixtureWithLink(t) // 真有 link：review --fix--> fixer

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID,
		[]AppliedDecl{{Capability: "review", Version: 1}, {Capability: "fixer", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}},
	)
	parent := f.makeAssignedTask(t, "trace_out_retired")

	// 原始 JSON：老实现的线格式（out 键还在）。服务端必须照收、不报错、不路由。
	sendRaw(t, conn, `{"type":"result","task_id":"`+parent.ID+
		`","status":"succeeded","out":"fix","summary":"老 agent 的帧"}`)

	waitStatus(t, f.db, f.nsID, parent.ID, store.TaskSucceeded)

	// ① 终态该有的都落了：reported=1（帧到了）、summary 就是那句说明。
	got, err := f.db.GetTask(ctx, f.nsID, parent.ID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if !got.Reported {
		t.Error("reported = false，want true（结果帧到过）")
	}
	if got.Summary != "老 agent 的帧" {
		t.Errorf("summary = %q，want 老 agent 的帧", got.Summary)
	}
	// 这一列不该再存在（升级路径漏了迁移会在这里红）。
	assertNoOutColumn(t, f)

	// ② 下游 0 条：这条 trace 上只有父任务这一行。
	if kids := childrenOf(t, f, parent.ID); len(kids) != 0 {
		t.Fatalf("result.out 竟然建出了 %d 条下游任务（%v）—— 那条腿必须已经断了",
			len(kids), kids)
	}
	// fixer 的 pending 行一条都不该有（下游根本没被建出来）。
	for _, row := range allTasks(t, f) {
		if row.Capability == fixer.Name {
			t.Fatalf("下游能力 %s 出现了任务行（%s）—— result 不该再建任务", fixer.Name, row.ID)
		}
	}
	// 也没有任何 emission（老帧根本不走 emit）。
	if ems, err := f.db.ListEmissions(ctx, parent.ID); err != nil {
		t.Fatalf("list emissions: %v", err)
	} else if len(ems) != 0 {
		t.Fatalf("result 帧竟然写了 %d 条 emission（emit 是唯一产出通道）", len(ems))
	}

	// ③ 节点一个帧都没收到（没有 assign、没有 error）。放最后：读超时会关连接。
	assertNoFrame(t, conn, 500*time.Millisecond, "老帧带 out 也不许建下游、不许回 error")
}

// ---------- 验收 ②：结果帧仍然只做「写终态」这一件事 ----------

// TestResultRecordsTerminalStateWithoutRouting 结果帧的三个字段照旧落库：
// status / summary / artifacts，且不建任何任务行、不写任何 emission。
//
// 这条取代了原来那条「不回报出口也照旧走 fix 出口」的用例（已随 out 退役删除）；
// 现在「没走任何出口」的证据是 0 下游 + 0 emission + 表上没有 out 列。
func TestResultRecordsTerminalStateWithoutRouting(t *testing.T) {
	ctx := context.Background()
	f := newNodeFixture(t)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	parent := f.makeAssignedTask(t, "trace_out_retired_plain")
	sentResult(t, conn, parent.ID, store.TaskSucceeded, "做完了")
	waitStatus(t, f.db, f.nsID, parent.ID, store.TaskSucceeded)

	got, err := f.db.GetTask(ctx, f.nsID, parent.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !got.Reported {
		t.Fatalf("reported=%v，want true（结果帧到过）", got.Reported)
	}
	if got.Summary != "做完了" {
		t.Fatalf("summary = %q，want 做完了", got.Summary)
	}
	assertNoOutColumn(t, f)
	if kids := childrenOf(t, f, parent.ID); len(kids) != 0 {
		t.Fatalf("没 emit 却有 %d 条下游任务", len(kids))
	}
	// emission 一行都没有 —— 这就是新语义下「到此为止」的定义。
	ems, err := f.db.ListEmissions(ctx, parent.ID)
	if err != nil {
		t.Fatalf("list emissions: %v", err)
	}
	if len(ems) != 0 {
		t.Fatalf("没 emit 过却有 %d 条 emission", len(ems))
	}
	assertNoFrame(t, conn, 300*time.Millisecond, "普通结果帧不该产生任何下行帧")
}

// ---------- 验收 ③：I4 的形式保留 —— 失败前 emit 过，下游照样在 ----------

// TestEmitBeforeFailedResultStillRoutes 「失败也能继续」在新载体下的落地验证。
//
// 改动前它是 TestFailedResultWithOutRoutes：`status=failed` + `out=failed` 照样
// 查表派发（I4：路由只看出口，不看 status）。现在出口只来自 emit，所以正确的写法是
// **先 emit、后回失败**：
//
//	emit(port=fix) → 下游 fixer 建出来并派发
//	result(failed) → 父任务落 failed（展示维度），已经建出的下游不受影响
//
// 这条守住的东西和原来一模一样：失败**不会**把已经发生的接力撤回，也不会少派。
func TestEmitBeforeFailedResultStillRoutes(t *testing.T) {
	ctx := context.Background()
	f, fixer := emitFixtureWithLink(t)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID,
		[]AppliedDecl{{Capability: "review", Version: 1}, {Capability: "fixer", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}},
	)
	parent := f.makeAssignedTask(t, "trace_failed_after_emit")

	// ① 先 emit：这次接力是真的发生了。
	send(t, conn, &EmitFrame{
		Type: NodeFrameEmit, TaskID: parent.ID, Seq: 1, Port: "fix",
		Data: json.RawMessage(`{"issue":"#42"}`),
	})
	var ack map[string]any
	for i := 0; i < 12; i++ {
		if m, ok := tryReadFrame(conn, 3*time.Second); ok && m["type"] == NodeFrameEmitAck {
			ack = m
			break
		}
	}
	if ack == nil {
		t.Fatal("没收到 emit_ack")
	}
	if e, _ := ack["error"].(string); e != "" {
		t.Fatalf("emit 被拒：%q", e)
	}

	// ② 再回失败：父任务落 failed（summary 是那句原因），下游已经建好。
	sentResult(t, conn, parent.ID, store.TaskFailed, "exit status 7: 编译失败")
	waitStatus(t, f.db, f.nsID, parent.ID, store.TaskFailed)

	got, err := f.db.GetTask(ctx, f.nsID, parent.ID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if got.Summary == "" {
		t.Error("失败任务没有原因（summary 为空）")
	}
	assertNoOutColumn(t, f)

	kids := childrenOf(t, f, parent.ID)
	if len(kids) != 1 {
		t.Fatalf("下游任务 = %d 条，want 1（失败不撤回已经发生的 emit）", len(kids))
	}
	if kids[0].Capability != fixer.Name {
		t.Fatalf("下游任务 = %s, want %s", kids[0].Capability, fixer.Name)
	}
	// 下游的 params 就是 emit 的 data（不做字段搬运）。
	if string(kids[0].Params) != `{"issue":"#42"}` {
		t.Fatalf("下游 params = %s, want emit.data 原样", kids[0].Params)
	}
}

// allTasks 读出这个命名空间下的全部任务行（用于「某个能力一行都不该有」这种断言）。
func allTasks(t *testing.T, f *nodeFixture) []*store.Task {
	t.Helper()
	rows, err := f.db.ListTasks(context.Background(), f.nsID, "", "", "", 200)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	return rows
}
