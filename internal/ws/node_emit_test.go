package ws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 P1「emit 帧」的帧层用例。
//
// 帧层的职责边界：把一帧 emit 变成一次路由调用，再把结果写成 emit_ack。
// 所以这里守的是三件事：
//   - 成功路径：ack 带 routed/pruned，下游任务真的建出来并被派发；
//   - 拒收路径：**一律走 emit_ack.error**，且连接仍然活着（不产生重连风暴）；
//   - assign 帧带上 ports / expects / token，且 token 重发不变。

// emitFixtureWithLink 在 nodeFixture 上补一条 review --fix--> fixer 的 link，
// 并接上真路由（Publisher = hub，于是下游任务会真的被推到这条连接上）。
func emitFixtureWithLink(t *testing.T) (*nodeFixture, *store.CapabilityVersion) {
	t.Helper()
	f := newNodeFixture(t)
	ctx := context.Background()

	review, err := f.db.GetCapability(ctx, f.nsID, "review", 1)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	fixer := &store.CapabilityVersion{
		NamespaceID: f.nsID, Name: "fixer", Version: 1,
		ParamsSchema: json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`),
		Outputs:      []string{"done"},
		Source:       store.CapSourceCustom,
	}
	if err := f.db.CreateCapability(ctx, fixer); err != nil {
		t.Fatalf("create fixer: %v", err)
	}
	if err := f.db.CreateEdge(ctx, &store.Edge{
		NamespaceID:    f.nsID,
		FromCapability: review.ID,
		FromPort:       "fix",
		ToCapability:   fixer.ID,
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}

	disp := route.New(f.db)
	disp.Publisher = f.handler.Hub
	f.handler.Emit = disp
	f.handler.Expects = disp
	return f, fixer
}

// TestEmitFrameRoutesAndAcks 一次 emit：ack 正确 + 下游任务建行并派发 + assign 带齐 P1 字段。
func TestEmitFrameRoutesAndAcks(t *testing.T) {
	f, fixer := emitFixtureWithLink(t)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID,
		[]AppliedDecl{{Capability: "review", Version: 1}, {Capability: "fixer", Version: 1}},
		// 两个槽位：review 那条任务已经占住 slot 0（makeAssignedTask），
		// 下游 fixer 必须还有空槽位才能被派出去（容量不足不是剪枝，任务会留 pending）。
		[]InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}},
	)
	task := f.makeAssignedTask(t, "trc_emit_1")

	send(t, conn, &EmitFrame{
		Type: NodeFrameEmit, TaskID: task.ID, Seq: 1, Port: "fix",
		Data: json.RawMessage(`{"issue":"#42"}`), Note: "第一次产出",
	})

	var ack map[string]any
	var assigns []map[string]any
	for i := 0; i < 12 && (ack == nil || len(assigns) == 0); i++ {
		m, ok := tryReadFrame(conn, 3*time.Second)
		if !ok {
			break
		}
		switch m["type"] {
		case NodeFrameEmitAck:
			if ack == nil {
				ack = m
			}
		case NodeFrameAssign:
			assigns = append(assigns, m)
		}
	}
	if ack == nil {
		t.Fatal("没收到 emit_ack")
	}
	// 拒收必须走 error 字段；成功时 routed 应当是 1（一条 link）。
	if e, _ := ack["error"].(string); e != "" {
		t.Fatalf("emit 被拒：%q", e)
	}
	if got, _ := ack["routed"].(float64); got != 1 {
		t.Fatalf("emit_ack.routed = %v, want 1", ack["routed"])
	}
	if got, _ := ack["seq"].(float64); got != 1 {
		t.Fatalf("emit_ack.seq = %v, want 1", ack["seq"])
	}

	// 下游任务真的建出来了（parent = 本任务）。
	kids, err := f.db.ListTasks(context.Background(), f.nsID, task.TraceID, "", "", 50)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	var downstream *store.Task
	for _, k := range kids {
		if k.ParentTaskID != nil && *k.ParentTaskID == task.ID {
			downstream = k
		}
	}
	if downstream == nil {
		t.Fatalf("emit 没有建出下游任务（trace 里有 %d 条）", len(kids))
	}
	if downstream.Capability != "fixer" || downstream.CapabilityID != fixer.ID {
		t.Fatalf("下游任务 = %s(%s), want fixer", downstream.Capability, downstream.CapabilityID)
	}

	// assign 帧：ports 取能力定义的 outputs、expects 按 port 名索引、token 是 tok_ 前缀。
	if len(assigns) == 0 {
		t.Fatal("下游任务没有收到 assign 帧（publish 没接上 hub）")
	}
	a := assigns[0]
	if a["capability"] != "fixer" {
		t.Fatalf("assign.capability = %v, want fixer", a["capability"])
	}
	ports, _ := a["ports"].([]any)
	if len(ports) != 1 || ports[0] != "done" {
		t.Fatalf("assign.ports = %v, want [done]（能力定义的 outputs）", a["ports"])
	}
	token, _ := a["token"].(string)
	if !strings.HasPrefix(token, "tok_") {
		t.Fatalf("assign.token = %q, want tok_ 前缀的随机令牌", token)
	}
	// fixer 没有出边 → 没有 expects（omitempty ⇒ 字段整个不出现）。
	if _, ok := a["expects"]; ok {
		t.Fatalf("fixer 没有下游，assign 里却带了 expects：%v", a["expects"])
	}

	// 重发 assign（断线重连 / 巡检重派的形状）：token 必须**一样**，
	// 否则节点上 outbox 的重发会被判成 token 无效。
	if err := f.handler.Hub.PublishAssign(context.Background(), f.nsID, f.node.ID, downstream); err != nil {
		t.Fatalf("re-publish assign: %v", err)
	}
	var again map[string]any
	for i := 0; i < 12; i++ {
		m, ok := tryReadFrame(conn, 3*time.Second)
		if !ok {
			break
		}
		if m["type"] == NodeFrameAssign {
			again = m
			break
		}
	}
	if again == nil {
		t.Fatal("重发 assign 没收到")
	}
	if got, _ := again["token"].(string); got != token {
		t.Fatalf("重发 assign 的 token = %q, want %q（同一任务必须不变）", got, token)
	}
	// 库里也应当就是那一份（不是每次随机生成一个新的）。
	persisted, err := f.db.EnsureTaskToken(context.Background(), f.nsID, downstream.ID, store.NewTaskToken())
	if err != nil {
		t.Fatalf("read persisted token: %v", err)
	}
	if persisted != token {
		t.Fatalf("库里的 token = %q, 帧里 = %q（必须同一个）", persisted, token)
	}
}

// TestEmitFrameRejectedKeepsConnectionOpen 拒收走 emit_ack.error，连接仍然可用。
//
// 这条用例守的是「拒收不能导致重连风暴」：seq 缺口这类**业务**拒收如果走 error 帧
// 或断连，节点会重连、从 outbox 重发、又被拒——一台机器上就是无限循环。
func TestEmitFrameRejectedKeepsConnectionOpen(t *testing.T) {
	f, _ := emitFixtureWithLink(t)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID,
		[]AppliedDecl{{Capability: "review", Version: 1}, {Capability: "fixer", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 2}},
	)
	task := f.makeAssignedTask(t, "trc_emit_gap")

	// seq=2 先到：缺口 → 拒收。
	send(t, conn, &EmitFrame{
		Type: NodeFrameEmit, TaskID: task.ID, Seq: 2, Port: "fix",
		Data: json.RawMessage(`{"issue":"#42"}`),
	})
	m, ok := tryReadFrame(conn, 3*time.Second)
	if !ok {
		t.Fatal("没收到 emit_ack（拒收也必须回帧）")
	}
	if m["type"] != NodeFrameEmitAck {
		t.Fatalf("拒收回的帧是 %v, want emit_ack（不是 error 帧）", m["type"])
	}
	msg, _ := m["error"].(string)
	if !strings.Contains(msg, "seq gap") {
		t.Fatalf("emit_ack.error = %q, want 含 seq gap", msg)
	}
	if got, _ := m["routed"].(float64); got != 0 {
		t.Fatalf("拒收的 emit_ack.routed = %v, want 0", m["routed"])
	}
	if n, err := f.db.ListEmissions(context.Background(), task.ID); err != nil || len(n) != 0 {
		t.Fatalf("缺口被拒后库里 emissions = %d（err=%v），want 0", len(n), err)
	}

	// 连接仍然可用：补发 seq=1 应当成功（同一连接、不重连）。
	send(t, conn, &EmitFrame{
		Type: NodeFrameEmit, TaskID: task.ID, Seq: 1, Port: "fix",
		Data: json.RawMessage(`{"issue":"#42"}`),
	})
	ok2 := false
	for i := 0; i < 12; i++ {
		m, ok := tryReadFrame(conn, 3*time.Second)
		if !ok {
			break
		}
		if m["type"] == NodeFrameEmitAck {
			if e, _ := m["error"].(string); e == "" {
				ok2 = true
			}
			break
		}
	}
	if !ok2 {
		t.Fatal("拒收之后同一连接上的正常 emit 没能成功（连接被弄死了）")
	}
}

// TestEmitFrameNotWired 没装配 Emit 时明确拒收（而不是静默丢弃 / 断连）。
func TestEmitFrameNotWired(t *testing.T) {
	f := newNodeFixture(t) // 不设 handler.Emit
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})
	task := f.makeAssignedTask(t, "trc_emit_nowire")

	send(t, conn, &EmitFrame{Type: NodeFrameEmit, TaskID: task.ID, Seq: 1, Port: "fix"})
	m, ok := tryReadFrame(conn, 3*time.Second)
	if !ok {
		t.Fatal("没收到 emit_ack")
	}
	if m["type"] != NodeFrameEmitAck {
		t.Fatalf("帧类型 = %v, want emit_ack", m["type"])
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, "not enabled") {
		t.Fatalf("emit_ack.error = %q, want 含 not enabled", msg)
	}
}

// TestExpectsFrameShapeMatches 钉死「route 推导的 expects」与「线上帧的 PortExpect」同形。
//
// 两个类型必须分开（route 不能 import ws），所以它们的 JSON 形状只能靠这条用例守住：
// 任何一边改了字段名/tag，序列化结果就不再相等。
func TestExpectsFrameShapeMatches(t *testing.T) {
	src := map[string]route.PortExpect{
		"fix": {
			Schema: json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`),
			Targets: []route.ExpectTarget{{
				Capability: "fixer",
				Version:    3,
				// 非 0 的 priority：0 会被 omitempty 省掉，那样「帧里漏了 priority」
				// 和「priority 就是 0」会看不出区别（契约 §5 要求透传）。
				Priority: 10,
				Needs:    json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}`),
			}},
		},
	}
	got := expectsForFrame(src)
	if len(got) != 1 {
		t.Fatalf("expectsForFrame 丢了端口：%v", got)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal frame expects: %v", err)
	}
	wantJSON, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal route expects: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("expects 形状漂移：\n frame = %s\n route = %s", gotJSON, wantJSON)
	}

	// 空 map → nil（omitempty 让字段整个消失，而不是出现一个 {}）。
	if expectsForFrame(map[string]route.PortExpect{}) != nil {
		t.Fatal("空 expects 应当被折成 nil（帧里不出现该字段）")
	}

	// 上线形状抽查：这些键名是节点侧按 JSON 读的，不能改。
	frameJSON, err := json.Marshal(&AssignFrame{
		Type:    NodeFrameAssign,
		TaskID:  "tsk_x",
		Ports:   []string{"fix"},
		Expects: got,
		Token:   "tok_x",
	})
	if err != nil {
		t.Fatalf("marshal assign: %v", err)
	}
	for _, want := range []string{`"ports":["fix"]`, `"expects":{`, `"schema":`, `"targets":[`, `"capability":"fixer"`, `"needs":`, `"token":"tok_x"`} {
		if !strings.Contains(string(frameJSON), want) {
			t.Errorf("assign 帧缺少 %s：%s", want, frameJSON)
		}
	}
}
