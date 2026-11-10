package ws

// result_receipt_hardening_test.go 是 issue #337 R3（ws 侧）的**判别性**回归：
// 畸形 / 伪造身份的 task.result 不得跨过「解码」与「身份核对」两道闸，因此既不会
// 产生 result.ack，也不会留下任何结果事实。
//
// 为什么需要这一组：v4 的 result 回执是「节点据此判定中枢是否接受」的唯一权威信号，
// 且结果列（phase/reported/result_json/result_accepted_at）一旦写入就不可回滚。
// 已有测试只覆盖「合法身份的成功/重复 ACK」；「伪造身份必须被拒且 0 写」此前只有
// store 层单测，ws 这一层的**帧到 handler 之间的闸**（解码失败 → 断连、node 名不符
// → 协议违例、lease/attempt 不符 → store 拒绝不 ACK）没有判别性覆盖。
//
// 判别手法（不是「等一会儿看没变」）：
//   - 会断连的路径（畸形帧、错 node 名）用 e.readRaw 等断开 + 校验真实 close code；
//   - 不断连的路径（错 leaseId / 错 attempt / 未知 taskId）用 e.handled 的
//     config.get → welcome 有序屏障：serveSession 单 goroutine 串行处理帧，welcome
//     到了就说明前面的 result handler 已返回；若它回过 ack，ack 会先于 welcome 到达
//     （e.handled 直接 Fatal），所以「拿到 welcome」本身就是「没收到 ack」的证据。
//   - 「0 写」用事实快照对比：屏障之后的那次快照就是最终事实，字段级比对不变即可
//     证明写入没发生（而不是靠时间赌 handler 还没跑）。

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

// liveAttempt 走完整一轮 dispatch → accept，返回一条真实 running 任务与它的 grant。
func liveAttempt(t *testing.T) (*env, *websocket.Conn, *store.Task, *protocol.LeaseGrantFrame) {
	t.Helper()
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task, dispatch := e.dispatchOne(t, conn, capability)
	e.send(t, conn, &protocol.TaskAcceptFrame{
		Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
		ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Slot: intPtr(0),
	})
	grant, ok := e.read(t, conn).(*protocol.LeaseGrantFrame)
	if !ok {
		t.Fatal("应收到 lease.grant")
	}
	stored := e.task(t, task.ID)
	if stored.Phase != store.TaskRunning || stored.Attempt != 1 || stored.LeaseID != grant.LeaseID {
		t.Fatalf("fixture 需要一条 running/attempt=1 且带 grant 的任务: %+v", stored)
	}
	return e, conn, stored, grant
}

// resultFrame 造一条「结构合法、但身份/绑定可能被伪造」的终态结果帧。
func resultFrame(taskID string, attempt int, node, leaseID string) *protocol.TaskResultFrame {
	now := time.Now().UTC()
	return &protocol.TaskResultFrame{
		Type: protocol.FrameTaskResult, TaskID: taskID, Attempt: attempt, Node: node,
		LeaseID: leaseID, Phase: protocol.TaskPhaseSucceeded, ExitCode: intPtr(0),
		Result:    protocol.TaskResult{Summary: "forged", Artifacts: []protocol.TaskArtifact{}},
		StartedAt: now.Add(-time.Second), FinishedAt: now,
	}
}

// ptrFacts 把可空指针渲染成可比较的字符串（NULL 与 0 必须可区分）。
func ptrFacts[T any](v *T) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%v", *v)
}

// resultFacts 是一条任务的「结果事实」快照：伪造/畸形 result 之后必须逐字段不变。
type resultFacts struct {
	Phase      string
	Reported   bool
	AcceptedAt string // tasks.result_accepted_at：NULL / 秒级时刻
	ResultJSON string
	Summary    string
	Error      string
	ExitCode   string
	Artifacts  string
	NodeID     string
	NodeName   string
	Attempt    int
	AttPhase   string
	AttAccept  string // task_attempts.result_accepted_at
	AttResult  string // task_attempts.result_json
	AttError   string
	Events     int
}

// captureResultFacts 读一次任务 + 该 attempt + ns 事件数，合成可比较快照。
func captureResultFacts(t *testing.T, e *env, taskID string, attempt int) resultFacts {
	t.Helper()
	ctx := context.Background()
	stored, err := e.db.GetTask(ctx, e.ns.ID, taskID)
	if err != nil {
		t.Fatalf("读任务 %s: %v", taskID, err)
	}
	att, err := e.db.GetTaskAttempt(ctx, taskID, attempt)
	if err != nil {
		t.Fatalf("读 attempt %s#%d: %v", taskID, attempt, err)
	}
	return resultFacts{
		Phase:      stored.Phase,
		Reported:   stored.Reported,
		AcceptedAt: ptrFacts(stored.ResultAcceptedAt),
		ResultJSON: string(stored.ResultJSON),
		Summary:    stored.Summary,
		Error:      stored.Error,
		ExitCode:   ptrFacts(stored.ExitCode),
		Artifacts:  fmt.Sprintf("%+v", stored.Artifacts),
		NodeID:     att.NodeID,
		NodeName:   att.NodeName,
		Attempt:    att.Attempt,
		AttPhase:   att.Phase,
		AttAccept:  ptrFacts(att.ResultAcceptedAt),
		AttResult:  string(att.ResultJSON),
		AttError:   att.Error,
		Events:     eventCount(t, e),
	}
}

// eventCount 读当前 ns 的事件行数（0 写断言的基线/复核）。
func eventCount(t *testing.T, e *env) int {
	t.Helper()
	events, err := e.db.ListEvents(context.Background(), e.ns.ID, "", "", 100)
	if err != nil {
		t.Fatalf("列事件: %v", err)
	}
	return len(events)
}

// taskCount 读当前 ns 的任务行数（未知 taskId 的 result 不得凭空建行）。
func taskCount(t *testing.T, e *env) int {
	t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM tasks WHERE namespace_id = ?`, e.ns.ID).Scan(&n); err != nil {
		t.Fatalf("数任务行: %v", err)
	}
	return n
}

// TestMalformedResultFrameClosesBeforeIdentityCheck 钉住 R3 的第一条事实：
// 畸形 result 帧在**解码阶段**就被拒（session.go 的 serveSession：Decode 失败 →
// sess.close(StatusUnsupportedData) + return），handler 根本不被调用，所以：
//   - 连接被关闭（不是「静默忽略继续服务」）；
//   - 帧里伪造的 node 名没有任何机会被身份核对逻辑看到，更不会有任何 DB 写。
func TestMalformedResultFrameClosesBeforeIdentityCheck(t *testing.T) {
	cases := []struct {
		name  string
		bytes func(taskID string) []byte
	}{
		{
			// 截断的帧：连帧类型都读不出来（FrameTypeOf 失败）。
			name: "truncated json",
			bytes: func(taskID string) []byte {
				return []byte(`{"type":"task.result","taskId":"` + taskID +
					`","attempt":1,"node":"node-2-forged",`)
			},
		},
		{
			// 外壳完整（帧类型可读出）、但字段类型非法：FrameTypeOf 通过、Decode 失败。
			name: "bad field type",
			bytes: func(taskID string) []byte {
				return []byte(`{"type":"task.result","taskId":"` + taskID +
					`","attempt":"one","node":"node-2-forged","leaseId":"lease_forged","phase":"succeeded"}`)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, conn, task, _ := liveAttempt(t)
			before := captureResultFacts(t, e, task.ID, 1)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := conn.Write(ctx, websocket.MessageText, tc.bytes(task.ID)); err != nil {
				t.Fatalf("写畸形 result 帧: %v", err)
			}
			_, _, err := e.readRaw(t, conn, time.Second)
			if err == nil {
				t.Fatal("畸形 result 帧必须断开连接（不许当作没收到继续服务）")
			}
			if code := websocket.CloseStatus(err); code != websocket.StatusUnsupportedData {
				t.Fatalf("畸形帧的断开码 = %v（%v），want %v（bad_frame）",
					code, err, websocket.StatusUnsupportedData)
			}
			// 0 写：解码失败早于身份核对（handlers.handleResult 的 node 名判定）与
			// store 事务，伪造 node 名不可能被落成任何事实。
			if after := captureResultFacts(t, e, task.ID, 1); before != after {
				t.Fatalf("畸形 result 帧不得留下任何结果事实（handler 之前就断开了）:\n before=%+v\n after =%+v",
					before, after)
			}
		})
	}
}

// TestForgedResultIdentityIsRejectedWithoutAckOrWrites 钉住 R3 的第二条事实：
// 帧能解码、但身份/绑定对不上时，三道闸分别生效，且**都不回 ack、都不写事实**：
//   - 错 node 名  → handlers.handleResult 的会话身份核对 → errProtocolViolation
//     → 会话被关（StatusPolicyViolation）；
//   - 错 leaseId / 错 attempt → store 的授权事务拒绝 → 不 ACK（连接保持）。
//
// 「不 ACK」不是靠超时赌：错 lease/attempt 用 e.handled 的序屏障，拿到 welcome
// 就说明 handler 已返回且中间没有插进任何 ack 帧。
func TestForgedResultIdentityIsRejectedWithoutAckOrWrites(t *testing.T) {
	t.Run("wrong node name", func(t *testing.T) {
		e, conn, task, grant := liveAttempt(t)
		before := captureResultFacts(t, e, task.ID, 1)
		e.send(t, conn, resultFrame(task.ID, grant.Attempt, "node-2-forged", grant.LeaseID))
		// 错 node 名是协议违例：连接被关（不是回一帧拒绝），所以用「等断开」而不是屏障。
		_, _, err := e.readRaw(t, conn, time.Second)
		if err == nil {
			t.Fatal("result.node 不是本会话节点时必须断开会话（fail closed）")
		}
		if code := websocket.CloseStatus(err); code != websocket.StatusPolicyViolation {
			t.Fatalf("错 node 名的断开码 = %v（%v），want %v（protocol violation）",
				code, err, websocket.StatusPolicyViolation)
		}
		// 连接已断：此时 DB 里不可能还有在飞的 handler（serveSession 已 return）。
		if after := captureResultFacts(t, e, task.ID, 1); before != after {
			t.Fatalf("伪造 node 名的 result 不得留下任何结果事实:\n before=%+v\n after =%+v", before, after)
		}
	})

	t.Run("wrong lease id", func(t *testing.T) {
		e, conn, task, grant := liveAttempt(t)
		before := captureResultFacts(t, e, task.ID, 1)
		e.send(t, conn, resultFrame(task.ID, grant.Attempt, e.node.Name, "lease_forged"))
		// 序屏障：拿到 welcome ⇒ 前面的 result handler 已返回，且没有回过 ack。
		e.handled(t, conn)
		if after := captureResultFacts(t, e, task.ID, 1); before != after {
			t.Fatalf("错 leaseId 的 result 必须 0 写:\n before=%+v\n after =%+v", before, after)
		}
	})

	t.Run("wrong attempt", func(t *testing.T) {
		e, conn, task, grant := liveAttempt(t)
		before := captureResultFacts(t, e, task.ID, 1)
		e.send(t, conn, resultFrame(task.ID, grant.Attempt+1, e.node.Name, grant.LeaseID))
		// 序屏障：同上，无 ack + handler 已返回。
		e.handled(t, conn)
		if after := captureResultFacts(t, e, task.ID, 1); before != after {
			t.Fatalf("错 attempt 的 result 必须 0 写:\n before=%+v\n after =%+v", before, after)
		}
		// 错 attempt 也不许凭空建一行 attempt 事实。
		if _, err := e.db.GetTaskAttempt(context.Background(), task.ID, grant.Attempt+1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("错 attempt 的 result 不得建 attempt 行: %v", err)
		}
	})
}

// TestResultFrameForUnknownTaskIsNotAcked 钉住 R3 的第三条事实：帧完全可解码、
// 身份也「像」真的（本会话节点名 + 任意 lease），但 taskId 不存在时 store 事务
// 拒绝 → 不 ACK，且不凭空造任务/attempt/事件行。
func TestResultFrameForUnknownTaskIsNotAcked(t *testing.T) {
	e := newEnv(t)
	conn := e.dial(t)
	ctx := context.Background()
	const unknownTaskID = "tsk_does_not_exist"
	tasksBefore := taskCount(t, e)

	e.send(t, conn, resultFrame(unknownTaskID, 1, e.node.Name, "lease_forged"))
	// 序屏障：若 handler 回了 ack，ack 会先于 welcome 到达 → e.handled Fatal。
	e.handled(t, conn)

	if _, err := e.db.GetTask(ctx, e.ns.ID, unknownTaskID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知 taskId 的 result 不得建任务行: %v", err)
	}
	if got := taskCount(t, e); got != tasksBefore {
		t.Fatalf("未知 taskId 的 result 不得改变任务行数: %d → %d", tasksBefore, got)
	}
	if attempts, err := e.db.ListTaskAttempts(ctx, unknownTaskID); err != nil || len(attempts) != 0 {
		t.Fatalf("未知 taskId 的 result 不得建 attempt 行: %v/%+v", err, attempts)
	}
	if got := eventCount(t, e); got != 0 {
		t.Fatalf("未知 taskId 的 result 不得写事件行: %d 行", got)
	}
}
