package ws

// 本文件是 P1.5（硬切 `.anotify-out` / `result.out`）在 ws 帧层的验收。
//
// 改动前它叫 node_next_test.go，守的是 #140 那套「result.out → PlanNextHop →
// 与父任务终态同一个事务建子任务行 → 后台 publish」的语义。P1.5 把 out 整条删掉
// 之后，那些用例的**主语消失了**——但它们守的性质没有消失，只是换了入口：
//
//	旧：result 带 out=fix  → 建下一跳（本文件原来的五条用例）
//	新：运行中 emit port=fix → 建下一跳（route.Emit；见 node_emit_test.go）
//
// 所以本文件的职责反转成「证明那条老入口真的没了」，并且**不是放宽**：
//
//	1. 老帧带 out（甚至带一个真的有边、真有下游的 port）→ 一个下游都不建，
//	   PlanNextHop / PublishTasks 一次都不被调用；
//	2. 结果帧该做的事照旧（终态 + summary 落库），只是不再碰路由；
//	3. 「不回报出口 = 到此为止」的语义保留，只是载体从 `out IS NULL` 变成
//	   「这个任务没有任何 emission」；
//	4. I4 的形式保留：失败**之前** emit 过的下游照样在（失败不特殊）。
//
// 三条事务性质（终态与子任务行同生共死 / 巡检能捞回 / plan 失败不留终态）**没有
// 删掉**，而是搬到了它现在真正所在的入口：route.Emit 自己的事务
// （internal/route 的 emit 单测）、store.ReportResultAndCreateTasks 的事务语义
// （internal/store/report_next_test.go）。ws 这一层不再有「两段提交」这种东西。

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 测试双：探测「下一跳入口有没有被碰」 ----------

// nextHopProbe 记录 NextHopDispatcher 被调用过几次。
//
// 它的存在就是为了那个「0」：证明 result 帧路径**一次都没碰**路由。装配层仍然
// 会给 NodeHandler.NextHop 赋值（internal/server/mux.go），所以这里沿用同一个
// 测试形状，只是断言方向反过来。
type nextHopProbe struct {
	mu           sync.Mutex
	planCalls    int
	publishCalls int
	lastOut      string
}

func (p *nextHopProbe) PlanNextHop(_ context.Context, _, _, _, _, out string, _ json.RawMessage, _ []string) ([]*store.Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.planCalls++
	p.lastOut = out
	return nil, nil
}

func (p *nextHopProbe) PublishTasks(_ context.Context, _ string, _ []*store.Task) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishCalls++
	return nil
}

func (p *nextHopProbe) calls() (plan, publish int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.planCalls, p.publishCalls
}

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

// ---------- 验收 ①：老帧带 out 不再产生任何下游（「这条腿真断了」） ----------

// TestResultLegacyOutCreatesNoDownstream 是 P1.5 的核心证据。
//
// 用**原始 JSON** 发一帧带 `"out":"fix"` 的 result（模拟老 agent / 手写客户端）。
// 这一帧在改动前会走出一条真有边的出口（review --fix--> fixer），建出下游任务并
// 推 assign；改动后它只能是「一帧普通的结果帧」：
//
//   - 父任务照常 succeeded（结果帧该做的事没退化）；
//   - tasks.out 是 NULL（没有任何东西再往这一列写）；
//   - 下游 0 条（emit 是唯一入口，而这条任务没有 emit 过）；
//   - NextHopDispatcher 一次都没被调用（不是「调用后为空」，是根本没碰）；
//   - 节点收不到任何帧（没有 assign、没有 error）。
func TestResultLegacyOutCreatesNoDownstream(t *testing.T) {
	ctx := context.Background()
	probe := &nextHopProbe{}
	f, fixer := emitFixtureWithLink(t) // 真有 link：review --fix--> fixer
	f.handler.NextHop = probe

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

	// ① 终态该有的都落了：reported=1（帧到了）、summary 就是那句说明、out=NULL。
	got, err := f.db.GetTask(ctx, f.nsID, parent.ID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if !got.Reported {
		t.Error("reported = false，want true（结果帧到过）")
	}
	if got.Out != nil {
		t.Errorf("tasks.out = %q，want NULL（out 已退役，没有任何写入方）", *got.Out)
	}
	if got.Summary != "老 agent 的帧" {
		t.Errorf("summary = %q，want 老 agent 的帧", got.Summary)
	}

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

	// ③ 路由入口一次都没被碰。
	if plan, publish := probe.calls(); plan != 0 || publish != 0 {
		t.Fatalf("NextHop 被调用了 %d 次 plan / %d 次 publish，want 0/0（result 不再路由）",
			plan, publish)
	}

	// ④ 节点一个帧都没收到（没有 assign、没有 error）。放最后：读超时会关连接。
	assertNoFrame(t, conn, 500*time.Millisecond, "老帧带 out 也不许建下游、不许回 error")
}

// ---------- 验收 ②：结果帧仍然只做「写终态」这一件事 ----------

// TestResultRecordsTerminalStateWithoutRouting 结果帧的三个字段照旧落库：
// status / summary / artifacts，且 out 恒 NULL、不建任何任务行。
//
// 这条取代了原来的 TestResultWithoutNextHopUnchanged（那条断言 out=='fix'）；
// 现在「没走任何出口」的证据是 out=NULL + 0 下游 + 0 emission。
func TestResultRecordsTerminalStateWithoutRouting(t *testing.T) {
	ctx := context.Background()
	probe := &nextHopProbe{}
	f := newNodeFixture(t, func(h *NodeHandler) { h.NextHop = probe })

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
	if !got.Reported || got.Out != nil {
		t.Fatalf("reported=%v out=%v，want reported=true + out=NULL", got.Reported, got.Out)
	}
	if got.Summary != "做完了" {
		t.Fatalf("summary = %q，want 做完了", got.Summary)
	}
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
	if plan, publish := probe.calls(); plan != 0 || publish != 0 {
		t.Fatalf("NextHop 被调用了 %d/%d 次，want 0/0", plan, publish)
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
	if got.Out != nil {
		t.Errorf("tasks.out = %q，want NULL（出口只在 emission 里）", *got.Out)
	}

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
