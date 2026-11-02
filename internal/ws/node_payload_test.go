package ws

// #221 的 ws 侧回归测试：两侧的尺寸闸门。
//
//   - 握手：服务端必须显式设读上限（不设时 coder/websocket 默认 32768B → 40KB 的 hello 上不了线）
//   - 入帧：逐帧软上限 → 先回可诊断的 error 帧 `frame_too_large`，再用 1009 带文案关连接
//   - assign：params 超限的帧**绝不写进连接**（写进去 = 打死 agent → 槽位钉死 = #218 的另一条路径）

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
)

// ---------- W5（#221）：>32KiB 的 hello 必须能上线 ----------

// TestBigHelloFrameAccepted hello 带 >32KiB 载荷 → 握手成功。
//
// 旧代码：服务端从没调过 SetReadLimit，库的默认单帧上限是 32768B，
// 于是在服务端读阶段就被 1009 关掉 → 节点永远上不了线，日志里只有一句「连接关了」。
//
// 为什么载荷换了：旧用例用 40KB 的 manifest.command 造大帧。v2 的 hello 没有
// manifest（执行方式属于产物），但**这条回归与帧内容无关** —— 它钉的是服务端必须
// 显式设读上限，所以载荷换成 v2 里真正会变大的东西：一台装了几百份产物的节点，
// applied[] 天然是几十 KB。这比原来的形状更接近真实故障面（节点越多部署越容易撞上限）。
func TestBigHelloFrameAccepted(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)

	// 400 条 applied：帧体积必然超过库默认的 32768B 读上限。
	const n = 400
	applied := make([]AppliedDecl, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("probe-%03d", i)
		f.defineCapability(t, name, 1)
		applied = append(applied, AppliedDecl{
			Capability: name, Version: 1,
			Digest: fmt.Sprintf("sha256:%064d", i),
		})
	}
	frame := &HelloFrame{
		Type: NodeFrameHello, NodeID: f.node.ID, Protocol: ProtocolVersion,
		Applied: applied, Instances: []InstanceDecl{{Slot: 0, Concurrency: 1}},
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	if len(raw) <= 32768 {
		t.Fatalf("precondition broken: hello payload is %d bytes, want > 32768 (库默认读上限)", len(raw))
	}
	helloFrame(t, conn, frame)

	// 握手真的完成了：节点上线，且整份载荷都落库了（没有被截断/丢弃）。
	got, err := f.db.GetNode(context.Background(), f.nsID, f.node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status != store.NodeStatusOnline {
		t.Fatalf("node status = %q after a %d-byte hello, want online", got.Status, len(raw))
	}
	caps := listNodeCaps(t, f.db, f.node.ID)
	if len(caps) != n {
		t.Fatalf("超大 hello 落库后的归属 = %d 条, want %d（载荷被截断/丢弃了）", len(caps), n)
	}
	if c := nodeCapOf(t, caps, "probe-000", 1); c == nil {
		t.Fatal("probe-000 没落库")
	}
	if c := nodeCapOf(t, caps, fmt.Sprintf("probe-%03d", n-1), 1); c == nil {
		t.Fatalf("probe-%03d 没落库（尾部被截断）", n-1)
	}
}

// ---------- W6（#221）：超软上限的上行帧要可诊断 ----------

// TestFrameTooLargeIsDiagnosable 上行帧超过逐帧软上限 → 先收到 error{code:"frame_too_large"}
// （带体积与上限），随后以 1009 关连接，而不是让库在硬上限处静默 1009。
func TestFrameTooLargeIsDiagnosable(t *testing.T) {
	f := newNodeFixture(t)
	f.handler.MaxFrameBytes = 1024 // 软上限调到 1KB，好用小帧复现
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, nil, []InstanceDecl{{Slot: 0, Concurrency: 1}})

	raw := `{"type":"heartbeat","ts":1,"padding":"` + strings.Repeat("x", 2048) + `"}`
	sendRaw(t, conn, raw)

	frame := readUntil(t, conn, NodeFrameError)
	if frame["code"] != ErrCodeFrameTooLarge {
		t.Fatalf("code = %v, want %q (没有它，节点只看到「连接断了」，不知道是自己的帧太大)",
			frame["code"], ErrCodeFrameTooLarge)
	}
	msg, _ := frame["message"].(string)
	for _, want := range []string{"frame too large", fmt.Sprint(len(raw)), "1024"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q (体积与上限都要给，否则节点无法截断重发)", msg, want)
		}
	}

	// 随后连接被 1009（message too big）关掉。
	if code := websocket.CloseStatus(readUntilClosed(t, conn, 3*time.Second)); code != websocket.StatusMessageTooBig {
		t.Fatalf("connection closed with status %v, want 1009 (message too big)", code)
	}
}

// ---------- W7（#221）：params 超限的 assign 帧不许进连接 ----------

// TestPublishRejectsOversizedParams params 超限的任务 → PublishAssign 返回
// *store.ParamsTooLargeError，**不发帧、不关连接**。
//
// 旧代码：照发 → agent 的 1MiB 读上限（或 env E2BIG）炸掉 → 连接死 → 任务停在 assigned、
// 槽位钉死（#221 + #218 的复现路径）。
func TestPublishRejectsOversizedParams(t *testing.T) {
	f := newNodeFixture(t)
	conn := f.mustDial(t)
	hello(t, conn, f.node.ID, []AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}})

	ctx := context.Background()
	task := f.makeAssignedTask(t, "trace_big_params")

	// 直接改库造一条超限 params 的任务：建行入口的闸门（CheckTaskParamsSize）会拒绝它，
	// 所以这里模拟「闸门上线之前就落库的历史行」——那正是线上要治理的东西。
	params := bigParams(t, store.MaxTaskParamsBytes+8192)
	if _, err := f.db.Exec(`UPDATE tasks SET params = ? WHERE id = ?`, string(params), task.ID); err != nil {
		t.Fatalf("stuff oversized params: %v", err)
	}
	big := mustGetTask(t, f, task.ID)
	if len(big.Params) <= store.MaxTaskParamsBytes {
		t.Fatalf("precondition broken: params = %d bytes, want > %d", len(big.Params), store.MaxTaskParamsBytes)
	}

	err := f.handler.hub().PublishAssign(ctx, f.nsID, f.node.ID, big)
	if !errors.Is(err, store.ErrParamsTooLarge) {
		t.Fatalf("PublishAssign with %d-byte params = %v, want store.ErrParamsTooLarge", len(big.Params), err)
	}
	var sizeErr *store.ParamsTooLargeError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("error type = %T, want *store.ParamsTooLargeError", err)
	}
	if sizeErr.TaskID != task.ID || sizeErr.Size <= sizeErr.Limit {
		t.Fatalf("ParamsTooLargeError = {task:%q size:%d limit:%d}, want task=%q and size > limit",
			sizeErr.TaskID, sizeErr.Size, sizeErr.Limit, task.ID)
	}

	// 连接仍然可用：心跳 + 一个未知帧还会得到回执（若超限帧被写进连接，这里会读到断连）。
	send(t, conn, &HeartbeatFrame{Type: NodeFrameHeartbeat, TS: time.Now().Unix()})
	sendRaw(t, conn, `{"type":"bogus"}`)
	next, ok := tryReadFrame(conn, 2*time.Second)
	if !ok {
		t.Fatal("no reply after the oversized publish: the connection looks dead (#221 的连接被打死)")
	}
	if next["type"] != NodeFrameError || next["code"] != ErrCodeUnknownType {
		t.Fatalf("frame after oversized publish = %v, want the unknown_type error frame: "+
			"一条超限的 assign 帧绝不许写进连接", next)
	}
	// 任务没被推进（route 会把这种失败退回 pending 重试，见 route/dispatch.go）。
	if got := mustGetTask(t, f, task.ID); got.Status != store.TaskAssigned {
		t.Fatalf("task status = %q after a rejected publish, want %q", got.Status, store.TaskAssigned)
	}
}

// bigParams 造一份**恰好** n 字节的合法 JSON params（{"blob":"aaa…"}）。
func bigParams(t *testing.T, n int) json.RawMessage {
	t.Helper()
	const wrap = `{"blob":""}` // 11 字节
	if n < len(wrap) {
		t.Fatalf("bigParams(%d): too small for the %d-byte wrapper", n, len(wrap))
	}
	return json.RawMessage(`{"blob":"` + strings.Repeat("a", n-len(wrap)) + `"}`)
}
