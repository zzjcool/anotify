package ws

// #264：任务令牌的**写**已经搬到派发事务里（store.assignTaskTx），发布路径只读。
//
// 为什么这条单独钉住「帧里的令牌 == 派发事务写进去的那份」：
// 令牌是节点本地 emit socket 的鉴权凭据，两端必须一致；而它现在有两个可能出错的环节：
//
//	① 派发事务有没有真的把它落库（store 侧：TestLeaseAndAssignWritesTaskToken）；
//	② 发布路径读到的是不是**库里那份**，而不是自己又生成一个新串。
//
// ②坏了后果最重：同一个任务的每一帧带不同令牌 → 节点上 outbox 的重发会被判成
// token 无效（断线重连 / 巡检重派都会踩到）。所以这里既比帧与库，也比"重发不变"。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestPublishAssignUsesTokenWrittenAtAssign 帧里的令牌来自派发事务，且重发不变。
func TestPublishAssignUsesTokenWrittenAtAssign(t *testing.T) {
	f := newNodeFixture(t)
	ctx := context.Background()

	conn := f.mustDial(t)
	hello(t, conn, f.node.ID,
		[]AppliedDecl{{Capability: "review", Version: 1}},
		[]InstanceDecl{{Slot: 0, Concurrency: 1}},
	)

	// 走生产的指派路径（AssignTask 与 LeaseAndAssign 共用 assignTaskTx）。
	task := f.makeAssignedTask(t, "trc_token_pub")

	// ① 库里已经有权令牌——**在任何 publish 之前**。
	direct, err := f.db.GetTask(ctx, f.nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if !strings.HasPrefix(direct.Token, "tok_") {
		t.Fatalf("派发后库里的 token = %q, want tok_ 前缀（发布路径不该是它的出生地）", direct.Token)
	}

	// ② publish → 帧里的令牌就是库里那份。
	if err := f.handler.Hub.PublishAssign(ctx, f.nsID, f.node.ID, task); err != nil {
		t.Fatalf("publish assign: %v", err)
	}
	first := readAssignFrame(t, conn)
	if got := frameToken(first); got != direct.Token {
		t.Fatalf("assign.token = %q, 库里的 = %q（发布路径必须读库里那份，不能另生成）",
			got, direct.Token)
	}

	// ③ 重发（断线重连 / 巡检重派的形状）→ 还是同一个令牌。
	if err := f.handler.Hub.PublishAssign(ctx, f.nsID, f.node.ID, task); err != nil {
		t.Fatalf("re-publish assign: %v", err)
	}
	second := readAssignFrame(t, conn)
	if got := frameToken(second); got != direct.Token {
		t.Fatalf("重发后 assign.token = %q, want %q（同一任务令牌必须不变）", got, direct.Token)
	}

	// ④ 发布过程没有改写令牌（只读）。
	after, err := f.db.GetTask(ctx, f.nsID, task.ID)
	if err != nil {
		t.Fatalf("get task after publish: %v", err)
	}
	if after.Token != direct.Token {
		t.Fatalf("publish 之后库里的 token 变了：%q → %q", direct.Token, after.Token)
	}
	// 令牌对客户端不可见：它只走 assign 帧，不进任务 JSON。
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	if strings.Contains(string(raw), "tok_") {
		t.Fatal("任务 JSON 里出现了令牌（凭据不该出现在任何客户端可见的载荷里）")
	}
}

// readAssignFrame 读到下一条 assign 帧（跳过之前的其它帧）。
func readAssignFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	for i := 0; i < 12; i++ {
		m, ok := tryReadFrame(conn, 3*time.Second)
		if !ok {
			break
		}
		if m["type"] == NodeFrameAssign {
			return m
		}
	}
	t.Fatal("没收到 assign 帧")
	return nil
}

// frameToken 取帧里的 token 字段。
func frameToken(m map[string]any) string {
	if m == nil {
		return ""
	}
	s, _ := m["token"].(string)
	return s
}
