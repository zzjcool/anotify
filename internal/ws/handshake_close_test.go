package ws

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// #590：节点侧只把 welcome 之前的 1008 当身份终态，中枢把**自己的**暂时性故障也回 1008，
// 就等于一次 5s 的写锁等待把节点进程永久踢下线（exit 2、日志写着「身份被拒绝」）。

// TestHandshakeCloseCodeMatrix 钉住分类矩阵：身份 → 1008，协议 → 1002，其余 → 1013。
func TestHandshakeCloseCodeMatrix(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want websocket.StatusCode
	}{
		// token 未知（GetNodeByTokenHash 的 ErrNotFound）与握手途中节点行被删。
		{name: "token 未知", err: fmt.Errorf("get node by token hash: %w", store.ErrNotFound), want: websocket.StatusPolicyViolation},
		{name: "节点行已删除", err: store.ErrNotFound, want: websocket.StatusPolicyViolation},
		{name: "缺 token", err: fmt.Errorf("%w: hello 缺少 token", errIdentityRejected), want: websocket.StatusPolicyViolation},
		{name: "name 不符", err: fmt.Errorf("%w: hello.name 与 token 所属节点不一致", errIdentityRejected), want: websocket.StatusPolicyViolation},
		{name: "首帧不是 hello", err: fmt.Errorf("%w: 首帧必须是 hello", errProtocolViolation), want: websocket.StatusProtocolError},
		// 中枢自己的暂时性故障：节点退避重连，不该按「身份被拒」退出。
		{name: "record hello 撞上写锁", err: errors.New("record hello: begin: database is locked (5) (SQLITE_BUSY)"), want: websocket.StatusTryAgainLater},
		{name: "读首帧超时", err: context.DeadlineExceeded, want: websocket.StatusTryAgainLater},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, reason := handshakeClose(tc.err)
			if code != tc.want {
				t.Fatalf("close code = %d（%s），want %d", code, reason, tc.want)
			}
			if reason == "" {
				t.Fatal("close reason 不能为空（运维靠它区分身份/重试）")
			}
		})
	}
}

// TestHandshakeTransientDBErrorIsNotPolicyViolation 用真实持锁的写事务复现 #590 的现场形态
// （单写者 SQLite + 外部备份/VACUUM 持锁）：RecordHello 等满 busy_timeout 拿到
// `database is locked`，连接必须以 1013 断开，且锁释放后同一节点仍能握手回 welcome。
func TestHandshakeTransientDBErrorIsNotPolicyViolation(t *testing.T) {
	e := newEnv(t)
	// 另开一个句柄持锁：不占 e.db 的连接池，也不依赖池上限。
	holder, err := store.Open(e.dbPath)
	if err != nil {
		t.Fatalf("开持锁句柄: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	tx, err := holder.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("开持锁事务: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	conn := e.dialRaw(t)
	e.send(t, conn, &protocol.HelloFrame{
		Type: protocol.FrameHello, Name: e.node.Name, Token: e.token,
		Namespaces: []string{"default"},
	})
	// 读超时必须盖过 busy_timeout(5s)：握手要等满它才拿到 database is locked。
	_, _, readErr := e.readRaw(t, conn, 20*time.Second)
	if readErr == nil {
		t.Fatal("写锁期间握手必须失败并断开，实际收到帧")
	}
	if code := websocket.CloseStatus(readErr); code != websocket.StatusTryAgainLater {
		t.Fatalf("暂时性 DB 错误必须回 1013（节点退避重连），实际 %d（%v）\n"+
			"　回 1008 就是 #590：节点会按「身份被拒」exit 2 永久离线", code, readErr)
	}
	if e.hub.Online(e.node.ID) {
		t.Fatal("握手失败不得注册 online 会话")
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("释放写锁: %v", err)
	}
	back := e.dial(t) // 内部断言收到 welcome 且注册为 online
	e.send(t, back, &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet})
	if _, ok := e.read(t, back).(*protocol.WelcomeFrame); !ok {
		t.Fatal("锁释放后同一节点应能重新握手并继续收发")
	}
}

// TestHandshakeUnknownTokenStillCloses1008 是反面：身份错误仍须 1008，否则坏 token 会被无限重试。
func TestHandshakeUnknownTokenStillCloses1008(t *testing.T) {
	e := newEnv(t)
	conn := e.dialRaw(t)
	e.send(t, conn, &protocol.HelloFrame{
		Type: protocol.FrameHello, Name: e.node.Name, Token: "not-a-real-token",
	})
	_, _, err := e.readRaw(t, conn, 5*time.Second)
	if err == nil {
		t.Fatal("坏 token 必须断开连接")
	}
	if code := websocket.CloseStatus(err); code != websocket.StatusPolicyViolation {
		t.Fatalf("身份错误必须是 1008（终态），实际 %d（%v）", code, err)
	}
	if e.hub.Online(e.node.ID) {
		t.Fatal("坏 token 不得注册 online 会话")
	}
}
