package ws

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件钉住 assign.ports 的**以边为准**口径（#383）。
//
// 用户实测的缺口：一个建时声明 `outputs: []`（终端）的能力，在能力编辑里加了下游 ——
// 边建好了，但派发只下发 `capDef.Outputs`（空）→ 节点拿到的端口契约里没有那个出口 →
// 脚本 emit 当场报「终端能力」，那条新边永远不触发。
//
// 裁定：派发时 ports = 产物声明的 outputs ∪ 该能力在 edges 里出现过的 from_port
// （去重、升序），expects 按**同一份**合并后的端口构建（否则 `emit --to` 会说
// 「不是候选」）。outputs 仍属产物、digest 不变 —— 这里同时钉住「不许把边写回声明」。

// defineCapWithOutputs 定义一个指定 outputs 的能力（夹具的 review 写死 outputs=["fix"]）。
func (f *nodeFixture) defineCapWithOutputs(
	t *testing.T, name string, version int, outputs []string, schema string,
) *store.CapabilityVersion {
	t.Helper()
	if schema == "" {
		schema = `{"type":"object"}`
	}
	c := &store.CapabilityVersion{
		NamespaceID: f.nsID, Name: name, Version: version,
		ParamsSchema: json.RawMessage(schema),
		Outputs:      outputs, Source: store.CapSourceBuiltin,
	}
	if err := f.db.CreateCapability(context.Background(), withSpec(c)); err != nil {
		t.Fatalf("create capability %s@%d: %v", name, version, err)
	}
	return c
}

// wireEmit 把真路由接到 handler 上（Emit + Expects 由同一个 Dispatcher 提供）。
//
// 必须接真 Dispatcher：本文件要验证的是「expects 与 ports 用**同一份**端口集合」，
// 换假实现就等于绕过了那个耦合点。
func (f *nodeFixture) wireEmit(t *testing.T) {
	t.Helper()
	disp := route.New(f.db)
	disp.Publisher = f.handler.Hub
	f.handler.Emit = disp
	f.handler.Expects = disp
}

// portStrings 把帧里的 ports（[]any）折成 []string（缺字段 = 空集合）。
func portStrings(frame map[string]any) []string {
	raw, _ := frame["ports"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// expectTargetCaps 取 assign.expects[port].targets[].capability。
func expectTargetCaps(t *testing.T, frame map[string]any, port string) []string {
	t.Helper()
	expects, _ := frame["expects"].(map[string]any)
	if expects == nil {
		return nil
	}
	one, _ := expects[port].(map[string]any)
	if one == nil {
		return nil
	}
	targets, _ := one["targets"].([]any)
	out := make([]string, 0, len(targets))
	for _, item := range targets {
		if m, ok := item.(map[string]any); ok {
			if name, ok := m["capability"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

// TestAssignPortsFollowEdgesWhenOutputsEmpty #383 的核心验收：
//
//	outputs: [] + 1 条出边 → assign.ports 含 out、expects.out.targets 有那个下游，
//	节点 emit 真的派发出子任务。
func TestAssignPortsFollowEdgesWhenOutputsEmpty(t *testing.T) {
	f := newNodeFixture(t)
	ctx := context.Background()

	sink := f.defineCapWithOutputs(t, "sink", 1, []string{}, `{"type":"object"}`)
	f.defineCapWithOutputs(t, "down", 1, []string{},
		`{"type":"object","required":["note"],"properties":{"note":{"type":"string"}}}`)
	hashBefore := sink.ContentHash

	// 用户路径：在能力编辑里给「建时声明 outputs: []」的能力加一条下游。
	// 它是 PUT /v1/capabilities/{name}/downstreams 的 store 入口，from_port 恒为隐式出口 out
	// —— 这正是「声明里没有 out、边上却有 out」这种状态的唯一来源。
	if _, err := f.db.ReplaceCapabilityDownstreams(ctx, f.nsID, "sink",
		[]store.DownstreamSpec{{ToRef: "down"}}); err != nil {
		t.Fatalf("add downstream to a terminal capability: %v", err)
	}
	// **不许放松**：以边为准 ≠ 把边写回声明。outputs 仍是 []，内容哈希（digest 的输入）
	// 一个字节都不能变 —— 否则「加一条边」就等于换了一份产物，append-only 的版本语义被绕开。
	after, err := f.db.GetCapability(ctx, f.nsID, "sink", 1)
	if err != nil {
		t.Fatalf("re-read sink: %v", err)
	}
	if len(after.Outputs) != 0 {
		t.Fatalf("sink.outputs = %v, want []（边不是声明，不能写回产物）", after.Outputs)
	}
	if after.ContentHash != hashBefore {
		t.Fatalf("sink.contentHash = %q, want %q（digest 不变）", after.ContentHash, hashBefore)
	}

	f.wireEmit(t)
	conn := f.mustDial(t)
	// 两个槽位：父任务（sink）占 0，emit 出来的子任务（down）要拿 1。
	hello(t, conn, f.node.ID,
		[]AppliedDecl{{Capability: "sink", Version: 1}, {Capability: "down", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}},
	)

	task := f.makeAssignedTaskWithCapability(t, "trc_ports_edge", "sink", 1)
	if err := f.handler.Hub.PublishAssign(ctx, f.nsID, f.node.ID, task); err != nil {
		t.Fatalf("publish assign: %v", err)
	}

	a := readUntil(t, conn, NodeFrameAssign)
	if got, want := portStrings(a), []string{store.ImplicitPort}; !reflect.DeepEqual(got, want) {
		t.Fatalf("assign.ports = %v, want %v（有出边的能力必须自动具备那个出口）", got, want)
	}
	if got, want := expectTargetCaps(t, a, store.ImplicitPort), []string{"down"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("assign.expects[out].targets = %v, want %v（expects 必须按同一份合并后的端口构建）", got, want)
	}
	// needs 要带上下游真正要的字段：不然实现拼不出 params，只能被拒了再猜。
	expects, _ := a["expects"].(map[string]any)
	out, _ := expects[store.ImplicitPort].(map[string]any)
	targets, _ := out["targets"].([]any)
	if needs := targets[0].(map[string]any)["needs"]; !needsMentions(t, needs, "note") {
		t.Fatalf("expects[out].targets[0].needs = %v, want 含 note（下游 params_schema 的要求）", needs)
	}

	// 节点按 assign 里的端口 emit：这次必须真的派发出子任务（缺口里它会被判「终端」）。
	send(t, conn, &EmitFrame{
		Type: NodeFrameEmit, TaskID: task.ID, Seq: 1, Port: store.ImplicitPort,
		Data: json.RawMessage(`{"note":"终端的第一次产出"}`),
	})
	var ack, childAssign map[string]any
	for i := 0; i < 12 && (ack == nil || childAssign == nil); i++ {
		m, ok := tryReadFrame(conn, 3*time.Second)
		if !ok {
			break
		}
		switch m["type"] {
		case NodeFrameEmitAck:
			ack = m
		case NodeFrameAssign:
			if m["capability"] == "down" {
				childAssign = m
			}
		}
	}
	if ack == nil {
		t.Fatal("没收到 emit_ack")
	}
	if e, _ := ack["error"].(string); e != "" {
		t.Fatalf("emit 被拒：%q（这个出口必须可用）", e)
	}
	if got, _ := ack["routed"].(float64); got != 1 {
		t.Fatalf("emit_ack.routed = %v, want 1", ack["routed"])
	}
	if childAssign == nil {
		t.Fatal("新边没有真的派发出子任务（任务行建了但 assign 没发出去）")
	}

	// 库里也要有那条子任务（parent = 本任务），routed=1 不是嘴上说说。
	kids, err := f.db.ListTasks(ctx, f.nsID, task.TraceID, "", "", 50)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	var downstream *store.Task
	for _, k := range kids {
		if k.ParentTaskID != nil && *k.ParentTaskID == task.ID {
			downstream = k
		}
	}
	if downstream == nil || downstream.Capability != "down" {
		t.Fatalf("emit 没有建出 down 子任务（trace 里有 %d 条）", len(kids))
	}
}

// TestAssignPortsStayEmptyWithoutOutEdges 终端能力行为不变：没有出边 → ports 仍为空。
func TestAssignPortsStayEmptyWithoutOutEdges(t *testing.T) {
	f := newNodeFixture(t)
	f.wireEmit(t)
	f.defineCapWithOutputs(t, "terminal", 1, []string{}, `{"type":"object"}`)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "terminal", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTaskWithCapability(t, "trc_ports_terminal", "terminal", 1)
	if err := f.handler.Hub.PublishAssign(context.Background(), f.nsID, f.node.ID, task); err != nil {
		t.Fatalf("publish assign: %v", err)
	}
	a := readUntil(t, conn, NodeFrameAssign)
	if got := portStrings(a); len(got) != 0 {
		t.Fatalf("assign.ports = %v, want 空（没有出边 = 终端，行为不变）", got)
	}
	if _, ok := a["expects"]; ok {
		t.Fatalf("终端能力不该带 expects：%v", a["expects"])
	}
}

// TestAssignPortsKeepDeclaredWhenUnwired 声明了端口、但没有边：保持现状
// （ports 仍是声明的那份；emit 成功但 routed=0 —— 这不是错误，是「到此为止」）。
func TestAssignPortsKeepDeclaredWhenUnwired(t *testing.T) {
	f := newNodeFixture(t)
	f.wireEmit(t)
	f.defineCapWithOutputs(t, "declared", 1, []string{store.ImplicitPort}, `{"type":"object"}`)

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "declared", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTaskWithCapability(t, "trc_ports_declared", "declared", 1)
	if err := f.handler.Hub.PublishAssign(context.Background(), f.nsID, f.node.ID, task); err != nil {
		t.Fatalf("publish assign: %v", err)
	}
	a := readUntil(t, conn, NodeFrameAssign)
	if got, want := portStrings(a), []string{store.ImplicitPort}; !reflect.DeepEqual(got, want) {
		t.Fatalf("assign.ports = %v, want %v", got, want)
	}

	send(t, conn, &EmitFrame{
		Type: NodeFrameEmit, TaskID: task.ID, Seq: 1, Port: store.ImplicitPort,
		Data: json.RawMessage(`{}`),
	})
	m, ok := tryReadFrame(conn, 3*time.Second)
	if !ok {
		t.Fatal("没收到 emit_ack")
	}
	if m["type"] != NodeFrameEmitAck {
		t.Fatalf("帧类型 = %v, want emit_ack", m["type"])
	}
	if e, _ := m["error"].(string); e != "" {
		t.Fatalf("没接下游的 emit 不该被拒：%q（routed=0 是合法结局）", e)
	}
	if got, _ := m["routed"].(float64); got != 0 {
		t.Fatalf("emit_ack.routed = %v, want 0", m["routed"])
	}
}

// TestAssignPortsKeepLegacyMultiPorts 历史多出口数据：outputs: [a,b]，只有 a 接了边 ——
// ports 仍含两个（声明过的端口不能被边吞掉，否则已经在 emit b 的实现当场被拒）。
func TestAssignPortsKeepLegacyMultiPorts(t *testing.T) {
	f := newNodeFixture(t)
	ctx := context.Background()
	f.defineCapWithOutputs(t, "multi", 1, []string{"before", "after"}, `{"type":"object"}`)
	f.defineCapWithOutputs(t, "down", 1, []string{}, `{"type":"object"}`)

	// 只有 before 接了边（from_port ∈ outputs 的封闭集合，老路径允许）。
	if err := f.db.CreateEdge(ctx, &store.Edge{
		NamespaceID: f.nsID, FromRef: "multi", FromPort: "before", ToRef: "down",
	}); err != nil {
		t.Fatalf("create edge: %v", err)
	}

	f.wireEmit(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "multi", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	task := f.makeAssignedTaskWithCapability(t, "trc_ports_multi", "multi", 1)
	if err := f.handler.Hub.PublishAssign(ctx, f.nsID, f.node.ID, task); err != nil {
		t.Fatalf("publish assign: %v", err)
	}
	a := readUntil(t, conn, NodeFrameAssign)
	if got, want := portStrings(a), []string{"after", "before"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("assign.ports = %v, want %v（去重升序，且不吞掉声明过但暂时没接边的端口）", got, want)
	}
}

// needsMentions 断言 needs（下游要求的字段集合）里出现了某个字段名。
func needsMentions(t *testing.T, raw any, field string) bool {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), `"`+field+`"`)
}
