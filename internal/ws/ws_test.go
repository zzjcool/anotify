package ws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func TestDispatchFrameCarriesNoExecutionAuthority(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task, dispatch := e.dispatchOne(t, conn, capability)

	if dispatch.TaskID != task.ID || dispatch.AssignmentID == "" || dispatch.ProposedAttempt != 1 {
		t.Fatalf("dispatch 关联字段不对: %+v", dispatch)
	}
	if dispatch.Capability.Digest != capability.Digest || dispatch.Namespace != "default" {
		t.Fatalf("dispatch 未钉住目标 refs/ns: %+v", dispatch)
	}
	if dispatch.ConfigRevision == "" || dispatch.NamespaceVarsRevision == "" {
		t.Fatalf("dispatch 必须带两个 revision 字段: %+v", dispatch)
	}
	if !dispatch.AssignmentExpiresAt.After(time.Unix(e.now, 0).Add(25 * time.Second)) {
		t.Fatalf("派发窗口应为统一 30s 量级: %v", dispatch.AssignmentExpiresAt)
	}
	// 帧里不能出现任何执行授权字段（lease/token 只由 grant 生成）。
	data, err := protocol.Encode(dispatch)
	if err != nil {
		t.Fatalf("编码 dispatch: %v", err)
	}
	for _, forbidden := range []string{"leaseId", "attemptToken", `"slot"`} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("dispatch 帧不得包含 %s: %s", forbidden, data)
		}
	}
	stored := e.task(t, task.ID)
	if stored.Phase != store.TaskPending || stored.Attempt != 0 || stored.AttemptTokenHash != "" {
		t.Fatalf("派发不得推进 attempt 或写执行授权: %+v", stored)
	}
	if stored.AssignmentID != dispatch.AssignmentID {
		t.Fatalf("派发候选未落库: %+v", stored.AssignmentID)
	}
}

func TestAcceptGrantsSameSlotAndResultAcks(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task, dispatch := e.dispatchOne(t, conn, capability)

	e.send(t, conn, &protocol.TaskAcceptFrame{
		Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
		ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Slot: intPtr(0),
	})
	frame := e.read(t, conn)
	grant, ok := frame.(*protocol.LeaseGrantFrame)
	if !ok {
		t.Fatalf("accept 后应收到 lease.grant，得到 %T", frame)
	}
	if grant.Slot == nil || *grant.Slot != 0 || grant.AssignmentID != dispatch.AssignmentID {
		t.Fatalf("grant 必须回带 accepted 的同一 slot/assignment: %+v", grant)
	}
	if grant.Attempt != 1 || grant.LeaseID == "" || grant.AttemptToken == "" || grant.Node != "node-1" {
		t.Fatalf("grant 授权字段不对: %+v", grant)
	}
	stored := e.task(t, task.ID)
	if stored.Phase != store.TaskRunning || stored.Attempt != 1 || stored.LeaseID != grant.LeaseID {
		t.Fatalf("grant 后任务应 running attempt=1: %+v", stored)
	}
	instances := e.instances(t)
	if len(instances) != 1 || instances[0].BusyTask == nil || *instances[0].BusyTask != task.ID {
		t.Fatalf("grant 必须把 accepted 的 slot 绑到该 attempt: %+v", instances)
	}

	// 续租围绕同一个 leaseId，ACK 带新的到期时刻（TTL 60 > renew 20 的窗口）。
	e.send(t, conn, &protocol.LeaseRenewFrame{
		Type: protocol.FrameLeaseRenew, TaskID: task.ID, Attempt: 1, LeaseID: grant.LeaseID,
	})
	renewAck, ok := e.read(t, conn).(*protocol.LeaseAckFrame)
	if !ok {
		t.Fatal("合法续租必须 ACK")
	}
	if renewAck.LeaseID != grant.LeaseID || renewAck.Attempt != 1 || renewAck.ExpiresAt.Before(grant.ExpiresAt) {
		t.Fatalf("lease.ack 必须围绕同一 lease 并延长到期: %+v", renewAck)
	}

	phase := protocol.TaskPhaseSucceeded
	result := &protocol.TaskResultFrame{
		Type: protocol.FrameTaskResult, TaskID: task.ID, Attempt: 1, Node: "node-1",
		LeaseID: grant.LeaseID, Phase: phase, ExitCode: intPtr(0),
		Result:    protocol.TaskResult{Summary: "ok", Artifacts: []protocol.TaskArtifact{}},
		StartedAt: time.Unix(e.now-1, 0).UTC(), FinishedAt: time.Unix(e.now, 0).UTC(),
	}
	e.send(t, conn, result)
	ack, ok := e.read(t, conn).(*protocol.ResultAckFrame)
	if !ok || !ack.Accepted || ack.TaskID != task.ID || ack.Attempt != 1 {
		t.Fatalf("成功结果必须及时 ACK: %+v", ack)
	}
	done := e.task(t, task.ID)
	if done.Phase != store.TaskSucceeded || done.Summary != "ok" {
		t.Fatalf("任务未落成功终态: %+v", done)
	}
	// 同一条结果重复上报：幂等（Duplicate 也 ACK，且不重复执行任何副作用）。
	e.send(t, conn, result)
	if again, ok := e.read(t, conn).(*protocol.ResultAckFrame); !ok || !again.Accepted {
		t.Fatalf("重复结果应幂等 ACK，得到 %+v", again)
	}
}

func TestAcceptRejectsInvalidSlotAndIdentity(t *testing.T) {
	cases := []struct {
		name string
		slot *int
		node string
	}{
		{name: "missing slot", slot: nil, node: "node-1"},
		{name: "negative slot", slot: intPtr(-1), node: "node-1"},
		{name: "wrong node name", slot: intPtr(0), node: "other-node"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			capability := e.capability(t, "analyze", "", false, nil)
			conn := e.dial(t)
			task, dispatch := e.dispatchOne(t, conn, capability)
			e.send(t, conn, &protocol.TaskAcceptFrame{
				Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
				ProposedAttempt: dispatch.ProposedAttempt, Node: tc.node, Slot: tc.slot,
			})
			// 协议违例 → 断开（没有 T3 定义过的 error 帧可回）。
			if _, _, err := e.readRaw(t, conn, time.Second); err == nil {
				t.Fatal("非法 accept 必须断开连接")
			}
			stored := e.task(t, task.ID)
			if stored.Phase != store.TaskPending || stored.Attempt != 0 {
				t.Fatalf("非法 accept 不得授执行权: %+v", stored)
			}
		})
	}
}

func TestRotatedCredentialGrantsNothing(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task, dispatch := e.dispatchOne(t, conn, capability)
	if err := e.db.SetNodeTokenHash(context.Background(), e.node.ID, store.HashNodeToken("rotated")); err != nil {
		t.Fatalf("轮换节点令牌: %v", err)
	}
	e.send(t, conn, &protocol.TaskAcceptFrame{
		Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
		ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Slot: intPtr(0),
	})
	if _, _, err := e.readRaw(t, conn, time.Second); err == nil {
		t.Fatal("凭证已轮换的会话必须被断开")
	}
	stored := e.task(t, task.ID)
	if stored.Phase != store.TaskPending || stored.Attempt != 0 {
		t.Fatalf("旧凭证不得授执行权: %+v", stored)
	}
}

func TestNamespaceRevocationGrantsNothing(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task, dispatch := e.dispatchOne(t, conn, capability)
	ctx := context.Background()
	cfg, err := e.db.GetNodeConfig(ctx, e.node.ID)
	if err != nil {
		t.Fatalf("读节点配置: %v", err)
	}
	cfg.Namespaces = []string{"other"}
	if err := e.db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("撤销 ns 授权: %v", err)
	}
	e.send(t, conn, &protocol.TaskAcceptFrame{
		Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
		ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Slot: intPtr(0),
	})
	if !waitFor(t, 500*time.Millisecond, func() bool {
		return e.task(t, task.ID).Attempt == 0
	}) {
		t.Fatal("ns 已撤销但 attempt 被推进")
	}
	if stored := e.task(t, task.ID); stored.Phase != store.TaskPending {
		t.Fatalf("ns 已撤销不得授执行权: %+v", stored)
	}
}

func TestRenewAtExpiryIsRejected(t *testing.T) {
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
	// 把租约到期时刻压到 renew 的 now（expiry == now 必须拒绝，不许被续租复活）。
	if _, err := e.db.Exec(`UPDATE tasks SET lease_until = ? WHERE id = ?`, e.now, task.ID); err != nil {
		t.Fatalf("设置到期时刻: %v", err)
	}
	e.send(t, conn, &protocol.LeaseRenewFrame{
		Type: protocol.FrameLeaseRenew, TaskID: task.ID, Attempt: 1, LeaseID: grant.LeaseID,
	})
	// 不该有 lease.ack：后面的读会超时（连接随之关闭，本用例到此为止）。
	if _, _, err := e.readRaw(t, conn, 500*time.Millisecond); err == nil {
		t.Fatal("expiry == now 的续租不得 ACK")
	}
	stored := e.task(t, task.ID)
	if stored.LeaseUntil == nil || *stored.LeaseUntil != e.now {
		t.Fatalf("续租不得延长过期授权: %+v", stored.LeaseUntil)
	}
}

func TestCancelAckMatrix(t *testing.T) {
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
	ctx := context.Background()
	if err := e.hub.CancelTask(ctx, e.ns.ID, task.ID, "cancelled_by_user", 5); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	cancel, ok := e.read(t, conn).(*protocol.TaskCancelFrame)
	if !ok {
		t.Fatal("运行中的取消应下发 task.cancel")
	}
	if cancel.Attempt != 1 || cancel.LeaseID != grant.LeaseID || cancel.Slot == nil || *cancel.Slot != 0 {
		t.Fatalf("task.cancel 必须带完整历史绑定: %+v", cancel)
	}
	ack := func(leaseID string, slot int, reaped bool) *protocol.CancelAckFrame {
		return &protocol.CancelAckFrame{
			Type: protocol.FrameCancelAck, TaskID: task.ID, Attempt: 1,
			LeaseID: leaseID, Slot: intPtr(slot), ProcessGroupReaped: reaped,
		}
	}
	// 错 lease：fail closed，不动任何槽/相位。
	e.send(t, conn, ack("lease-wrong", 0, true))
	// 这里原来靠 sleep(100ms)，慢机 handler 还没跑就会假绿；现改成帧处理完成的有序屏障。
	e.handled(t, conn)
	if stored := e.task(t, task.ID); stored.Phase != store.TaskRunning {
		t.Fatalf("错 lease 的 ACK 不得改相位: %+v", stored)
	}
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask == nil {
		t.Fatalf("错 lease 的 ACK 不得清槽: %+v", inst)
	}
	// 错 slot：同样 fail closed。
	e.send(t, conn, ack(grant.LeaseID, 3, true))
	// 同上：sleep → 有序屏障。
	e.handled(t, conn)
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask == nil {
		t.Fatalf("错 slot 的 ACK 不得清槽: %+v", inst)
	}
	// reaped=false：进程组没真回收，不得释放。
	e.send(t, conn, ack(grant.LeaseID, 0, false))
	// 同上：sleep → 有序屏障。
	e.handled(t, conn)
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask == nil {
		t.Fatalf("reaped=false 不得清槽: %+v", inst)
	}
	// 精确绑定 + reaped=true：终态 + 清槽。
	e.send(t, conn, ack(grant.LeaseID, 0, true))
	if !waitFor(t, time.Second, func() bool {
		return e.task(t, task.ID).Phase == store.TaskCancelled
	}) {
		t.Fatalf("精确 ACK 后应为 cancelled: %+v", e.task(t, task.ID))
	}
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask != nil {
		t.Fatalf("精确 ACK 必须清匹配槽: %+v", inst)
	}
	// 重复精确 ACK：幂等，且不把槽重新弄脏。
	e.send(t, conn, ack(grant.LeaseID, 0, true))
	// 同上：sleep → 有序屏障。
	e.handled(t, conn)
	if stored := e.task(t, task.ID); stored.Phase != store.TaskCancelled {
		t.Fatalf("重复 ACK 不应改变终态: %+v", stored)
	}
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask != nil {
		t.Fatalf("重复 ACK 不应恢复槽占用: %+v", inst)
	}
}

func TestRehelloUnknownBusyDoesNotRelease(t *testing.T) {
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
	rehello := func(instances []protocol.HelloInstance) {
		e.send(t, conn, &protocol.HelloFrame{
			Type: protocol.FrameHello, Name: "node-1", Token: e.token,
			Namespaces: []string{"default"}, Instances: instances,
		})
		if _, ok := e.read(t, conn).(*protocol.WelcomeFrame); !ok {
			t.Fatal("rehello 应回 welcome")
		}
	}
	// 缺 instances / 缺 busy 都是「没有新断言」，不是 idle。
	rehello(nil)
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask == nil {
		t.Fatalf("缺 instances 不得释放旧 busy: %+v", inst)
	}
	rehello([]protocol.HelloInstance{{Slot: intPtr(0)}})
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask == nil {
		t.Fatalf("缺 busy 不得释放旧 busy: %+v", inst)
	}
	// busy=false 但无匹配历史引用：不清旧 busy（需真实 cancel.ack）。
	rehello([]protocol.HelloInstance{{Slot: intPtr(0), Busy: boolPtr(false)}})
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask == nil {
		t.Fatalf("无匹配引用的 idle 不得清旧 busy: %+v", inst)
	}
	// 匹配的历史绑定 + busy=false：真实回收。
	rehello([]protocol.HelloInstance{{
		Slot: intPtr(0), Busy: boolPtr(false),
		TaskID: task.ID, Attempt: 1, LeaseID: grant.LeaseID,
	}})
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask != nil {
		t.Fatalf("匹配引用的 idle 应回收旧槽: %+v", inst)
	}
}

func TestDisconnectKeepsBusyUntilExplicitFact(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task, dispatch := e.dispatchOne(t, conn, capability)
	e.send(t, conn, &protocol.TaskAcceptFrame{
		Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
		ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Slot: intPtr(0),
	})
	if _, ok := e.read(t, conn).(*protocol.LeaseGrantFrame); !ok {
		t.Fatal("应收到 lease.grant")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	if !waitFor(t, 2*time.Second, func() bool {
		node, err := e.db.GetNodeByName(context.Background(), "node-1")
		return err == nil && node.Status == store.NodeStatusOffline
	}) {
		t.Fatal("断连后节点应转 offline")
	}
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask == nil {
		t.Fatalf("断连不得假释放 busy（进程可能还在跑）: %+v", inst)
	}
	// 重连（hello 只申报 slot、不给 busy）也不得清旧占用。
	e.dial(t)
	if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask == nil {
		t.Fatalf("重连不得假释放 busy: %+v", inst)
	}
}

func TestEmitRoundTripAndErrors(t *testing.T) {
	e := newEnv(t)
	producer := e.capability(t, "producer", "", false, []string{"consumer"})
	e.capability(t, "consumer", `{"type":"object","properties":{"file":{"type":"string"}},"required":["file"]}`, false, nil)
	conn := e.dial(t)
	task := e.pendingTask(t, producer)
	leaseID, attemptToken := e.directGrant(t, task)

	emit := func(requestID, to, id, data string) *protocol.TaskEmitAckFrame {
		e.send(t, conn, &protocol.TaskEmitFrame{
			Type: protocol.FrameTaskEmit, RequestID: requestID,
			TaskID: task.ID, Attempt: 1, LeaseID: leaseID, AttemptToken: attemptToken,
			EmitRequest: protocol.EmitRequest{To: to, ID: id, Data: json.RawMessage(data)},
		})
		frame := e.read(t, conn)
		ack, ok := frame.(*protocol.TaskEmitAckFrame)
		if !ok {
			t.Fatalf("应收到 task.emit.ack，得到 %T", frame)
		}
		return ack
	}

	first := emit("r1", "consumer", "", `{"file":"a.go"}`)
	if first.HTTPStatus != 200 || first.Response == nil || first.Response.TaskID == nil {
		t.Fatalf("成功 emit 应回 200 + taskId: %+v", first)
	}
	again := emit("r2", "consumer", "", `{"file":"a.go"}`)
	if again.Response == nil || *again.Response.TaskID == *first.Response.TaskID {
		t.Fatal("无 id 的两次 emit 必须是两次新派发")
	}
	once := emit("r3", "consumer", "same-id", `{"file":"a.go"}`)
	replay := emit("r4", "consumer", "same-id", `{"file":"a.go"}`)
	if replay.HTTPStatus != 200 || replay.Response == nil || *replay.Response.TaskID != *once.Response.TaskID {
		t.Fatalf("同 id 同请求必须回放旧结果: %+v", replay)
	}
	conflict := emit("r5", "consumer", "same-id", `{"file":"b.go"}`)
	if conflict.HTTPStatus != 409 || conflict.Error == nil || conflict.Error.Error != protocol.ReasonEmitIDConflict {
		t.Fatalf("同 id 不同请求必须 409 emit_id_conflict: %+v", conflict)
	}
	pruned := emit("r6", "consumer", "", `{}`)
	if pruned.HTTPStatus != 200 || pruned.Response == nil || pruned.Response.TaskID != nil ||
		pruned.Response.Pruned == nil || pruned.Response.Pruned.Reason != protocol.ReasonParamMissing {
		t.Fatalf("缺参必须是 200 剪枝回执: %+v", pruned)
	}
	undeclared := emit("r7", "ghost", "", `{}`)
	if undeclared.HTTPStatus != 400 || undeclared.Error == nil || undeclared.Error.Error != protocol.ReasonTargetNotDeclared {
		t.Fatalf("未声明目标必须 400 target_not_declared: %+v", undeclared)
	}
	missing := emit("r8", "", "", `{}`)
	if missing.HTTPStatus != 400 || missing.Error == nil || missing.Error.Error != protocol.ReasonMissingTarget {
		t.Fatalf("缺目标必须 400 missing_target: %+v", missing)
	}
	// 凭证轮换后：过期授权不得靠已存在 id 回放。
	if err := e.db.SetNodeTokenHash(context.Background(), e.node.ID, store.HashNodeToken("rotated")); err != nil {
		t.Fatalf("轮换令牌: %v", err)
	}
	revoked := emit("r9", "consumer", "same-id", `{"file":"a.go"}`)
	if revoked.HTTPStatus != 401 || revoked.Error == nil || revoked.Error.Error != protocol.ReasonTokenInvalid {
		t.Fatalf("旧凭证必须 401 token_invalid: %+v", revoked)
	}
	// 发出去的 ack 必须自身合法（互斥、状态码对应）。
	for _, ack := range []*protocol.TaskEmitAckFrame{first, conflict, pruned, undeclared, missing, revoked} {
		if err := ack.Validate(); err != nil {
			t.Fatalf("ack 不满足协议校验: %v（%+v）", err, ack)
		}
	}
}

func TestLogsRequestCorrelatesHistoryAndEmptyPrevious(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task := e.pendingTask(t, capability)
	_, _ = e.directGrant(t, task)

	// previous 且没有更早 attempt：真空结果，且**不**给节点发请求。
	result, err := e.hub.RequestLogs(context.Background(), e.ns.ID, task.ID, 0, true, 0, 0, 2*time.Second)
	if err != nil {
		t.Fatalf("previous 无历史应返回空结果而不是错误: %v", err)
	}
	if !result.Empty || result.Frame.TaskID != task.ID {
		t.Fatalf("previous 无历史应为 Empty: %+v", result)
	}
	// 证明没发过 logs_request：下一条帧应是 config.get 的 welcome。
	e.send(t, conn, &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet})
	if _, ok := e.read(t, conn).(*protocol.WelcomeFrame); !ok {
		t.Fatal("previous 无历史不得向节点发 logs_request")
	}

	// 指定 attempt：节点按请求回包。
	type outcome struct {
		result *LogsResult
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := e.hub.RequestLogs(context.Background(), e.ns.ID, task.ID, 1, false, 100, 30, 3*time.Second)
		ch <- outcome{result: res, err: err}
	}()
	req, ok := e.read(t, conn).(*protocol.LogsRequestFrame)
	if !ok || req.TaskID != task.ID || req.Attempt != 1 || req.RequestID == "" {
		t.Fatalf("logs_request 关联字段不对: %+v", req)
	}
	if req.TailLines != 100 || req.SinceSeconds != 30 {
		t.Fatalf("tailLines/sinceSeconds 必须原样转发（节点侧过滤）: %+v", req)
	}
	// 错 taskId 的回包不得被当成结果。
	e.send(t, conn, &protocol.LogsResponseFrame{
		Type: protocol.FrameLogsResponse, TaskID: "tsk_other", RequestID: req.RequestID,
		Attempt: 1, Stdout: "wrong",
	})
	if got := <-ch; !errors.Is(got.err, ErrLogsCorrelation) {
		t.Fatalf("错 taskId 回包应 ErrLogsCorrelation，得到 %v/%+v", got.err, got.result)
	}

	// 正确回包。
	ch2 := make(chan outcome, 1)
	go func() {
		res, err := e.hub.RequestLogs(context.Background(), e.ns.ID, task.ID, 1, false, 100, 30, 3*time.Second)
		ch2 <- outcome{result: res, err: err}
	}()
	req2, ok := e.read(t, conn).(*protocol.LogsRequestFrame)
	if !ok || req2.RequestID == req.RequestID {
		t.Fatalf("每次请求必须有新的 requestId: %+v", req2)
	}
	response := &protocol.LogsResponseFrame{
		Type: protocol.FrameLogsResponse, TaskID: task.ID, RequestID: req2.RequestID,
		Attempt: 1, Stdout: "hello", Stderr: "", Bytes: 5,
	}
	response.SetLogLines([]protocol.LogLine{{
		Sequence: 1, Stream: protocol.LogStreamStdout, Text: "hello", At: "2024-01-02T03:04:05.123456789Z",
	}})
	e.send(t, conn, response)
	got := <-ch2
	if got.err != nil || got.result.Frame == nil || got.result.Frame.Stdout != "hello" {
		t.Fatalf("正确回包未交付: %v/%+v", got.err, got.result)
	}
	if !got.result.Structured {
		t.Fatal("成功 logs_response 带真实 lines 时必须标记 Structured")
	}
	if lines := got.result.Frame.LogLines(); len(lines) != 1 || lines[0].Text != "hello" {
		t.Fatalf("真实 lines 未原样转发: %+v", lines)
	}

	// 节点离线：真实错误，不是空成功。
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	if !waitFor(t, 2*time.Second, func() bool { return !e.hub.Online(e.node.ID) }) {
		t.Fatal("节点应已离线")
	}
	if _, err := e.hub.RequestLogs(context.Background(), e.ns.ID, task.ID, 1, false, 0, 0, time.Second); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("离线节点必须 ErrNodeOffline，得到 %v", err)
	}
	// 没有 attempt 的任务：真实错误，不是空成功。
	pending := e.pendingTask(t, capability)
	if _, err := e.hub.RequestLogs(context.Background(), e.ns.ID, pending.ID, 0, false, 0, 0, time.Second); !errors.Is(err, ErrNoAttempt) {
		t.Fatalf("无 attempt 必须 ErrNoAttempt，得到 %v", err)
	}
	// 跨 namespace：任务不可见（404 语义），不是空日志。
	otherNS, err := e.db.CreateNamespace(context.Background(), "other", "Other")
	if err != nil {
		t.Fatalf("建第二个 ns: %v", err)
	}
	if _, err := e.hub.RequestLogs(context.Background(), otherNS.ID, task.ID, 1, false, 0, 0, time.Second); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("跨 ns 日志请求必须 NotFound，得到 %v", err)
	}
}

func TestDeployNotifiesOnlineAndReplaysOnReconnect(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)

	outcomes, err := e.hub.Deploy(context.Background(), e.ns.ID, "analyze", 0, []string{e.node.ID})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(outcomes) != 1 || !outcomes[0].Sent {
		t.Fatalf("在线节点应收到部署通知: %+v", outcomes)
	}
	frame, ok := e.read(t, conn).(*protocol.CapabilityDeployFrame)
	if !ok {
		t.Fatal("应收到 capability.deploy")
	}
	if frame.Namespace != "default" || frame.Name != "analyze" || frame.Version != capability.Version ||
		frame.Digest != capability.Digest || frame.RequestID != outcomes[0].RequestID {
		t.Fatalf("deploy refs 不对: %+v", frame)
	}
	e.send(t, conn, &protocol.CapabilityDeployedFrame{
		Type: protocol.FrameCapabilityDeployed, RequestID: frame.RequestID,
		DeployRefs: frame.DeployRefs, Status: protocol.DeployStatusApplied,
	})
	if !waitFor(t, time.Second, func() bool {
		rows, err := e.db.ListDeployments(context.Background(), e.ns.ID, store.DeploymentFilter{NodeID: e.node.ID})
		return err == nil && len(rows) == 1 && rows[0].State == store.DeploymentApplied
	}) {
		t.Fatal("部署回执未落 applied")
	}

	// 第二台节点离线：只记 pending 意图，重连后重放。
	node2 := &store.Node{Name: "node-2", MaxInstances: 2, MaxConcurrency: 1, DeclaredNamespaces: []string{"default"}}
	token2, err := e.db.CreateNode(context.Background(), node2)
	if err != nil {
		t.Fatalf("建第二台节点: %v", err)
	}
	stored2, err := e.db.GetNodeByName(context.Background(), "node-2")
	if err != nil {
		t.Fatalf("回读第二台节点: %v", err)
	}
	outcomes2, err := e.hub.Deploy(context.Background(), e.ns.ID, "analyze", 1, []string{stored2.ID})
	if err != nil {
		t.Fatalf("离线部署: %v", err)
	}
	if len(outcomes2) != 1 || outcomes2[0].Sent {
		t.Fatalf("离线节点不应收到通知（保留 pending 意图）: %+v", outcomes2)
	}
	conn2 := e.dialNode(t, "node-2", token2)
	replayed, ok := e.read(t, conn2).(*protocol.CapabilityDeployFrame)
	if !ok || replayed.Digest != capability.Digest {
		t.Fatalf("重连应重放 pending 部署: %+v", replayed)
	}
	if len(replayed.RequestID) == 0 {
		t.Fatal("重放的部署请求必须有稳定 requestId")
	}
}

func TestConfigAppliedAndAgentFactsPersist(t *testing.T) {
	e := newEnv(t)
	conn := e.dial(t)
	// rehello 带上 agent 版本事实（D-I13）。
	e.send(t, conn, &protocol.HelloFrame{
		Type: protocol.FrameHello, Name: "node-1", Token: e.token,
		Namespaces: []string{"default"}, AgentVersion: "v1.2.3", AgentCommit: "abc123",
		Instances: []protocol.HelloInstance{{Slot: intPtr(0)}},
	})
	if _, ok := e.read(t, conn).(*protocol.WelcomeFrame); !ok {
		t.Fatal("rehello 应回 welcome")
	}
	if !waitFor(t, time.Second, func() bool {
		node, err := e.db.GetNode(context.Background(), e.node.ID)
		return err == nil && node.AgentVersion == "v1.2.3" && node.AgentCommit == "abc123"
	}) {
		t.Fatal("hello 的 agent 版本事实未落库")
	}
	// config.applied：机器级 revision/hash 与他携带的实例快照。
	e.send(t, conn, &protocol.ConfigAppliedFrame{
		Type: protocol.FrameConfigApplied, Revision: "2", Hash: "hash-2",
		Instances: []protocol.HelloInstance{{Slot: intPtr(0), Busy: boolPtr(false)}},
	})
	if !waitFor(t, time.Second, func() bool {
		cfg, err := e.db.GetNodeConfig(context.Background(), e.node.ID)
		return err == nil && cfg.AppliedRevision == 2 && cfg.AppliedHash == "hash-2"
	}) {
		t.Fatal("config.applied 未落库")
	}
}

func TestUpgradeFrameAndAck(t *testing.T) {
	e := newEnv(t)
	conn := e.dial(t)
	sha := strings.Repeat("a", 64)
	if err := e.hub.RequestUpgrade(context.Background(), e.node.ID, "v9.0.0", sha, protocol.NodeBinaryPath); err != nil {
		t.Fatalf("RequestUpgrade: %v", err)
	}
	frame, ok := e.read(t, conn).(*protocol.UpgradeFrame)
	if !ok || frame.TargetVersion != "v9.0.0" || frame.SHA256 != sha || frame.URLPath != protocol.NodeBinaryPath {
		t.Fatalf("upgrade 帧不对: %+v", frame)
	}
	e.send(t, conn, &protocol.UpgradeAckFrame{Type: protocol.FrameUpgradeAck, OK: true, Version: "v9.0.0", Commit: "c1"})
	if !waitFor(t, time.Second, func() bool {
		node, err := e.db.GetNode(context.Background(), e.node.ID)
		return err == nil && node.AgentVersion == "v9.0.0" && node.AgentCommit == "c1"
	}) {
		t.Fatal("upgrade_ack ok 应写版本事实")
	}
	// 失败回报：版本列保持旧值（二进制没换，不谎报成功）。
	e.send(t, conn, &protocol.UpgradeAckFrame{Type: protocol.FrameUpgradeAck, OK: false, Error: "mystery"})
	// 这里原来靠 sleep(100ms)，慢机 handler 还没跑就会假绿；现改成帧处理完成的有序屏障。
	e.handled(t, conn)
	node, err := e.db.GetNode(context.Background(), e.node.ID)
	if err != nil {
		t.Fatalf("读节点: %v", err)
	}
	if node.AgentVersion != "v9.0.0" {
		t.Fatalf("失败回报不得改版本列: %q", node.AgentVersion)
	}
	if err := e.hub.RequestUpgrade(context.Background(), "nd_missing", "v1", sha, protocol.NodeBinaryPath); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("离线节点升级必须 ErrNodeOffline，得到 %v", err)
	}
}

func TestUnknownFrameKeepsConnectionAndBadFrameCloses(t *testing.T) {
	e := newEnv(t)
	conn := e.dial(t)
	// 未知帧类型：记录并继续（共享 Decode 的既定语义）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"future.frame","x":1}`)); err != nil {
		t.Fatalf("写未知帧: %v", err)
	}
	e.send(t, conn, &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet})
	if _, ok := e.read(t, conn).(*protocol.WelcomeFrame); !ok {
		t.Fatal("未知帧不得打断连接")
	}
	// 坏 JSON：无法解码 → 断开（不发明 error 帧）。
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{not-json`)); err != nil {
		t.Fatalf("写坏帧: %v", err)
	}
	if _, _, err := e.readRaw(t, conn, time.Second); err == nil {
		t.Fatal("坏帧必须断开连接")
	}
}

func TestBudgetShrinkKeepsOldBusyAndRejectsNewGrant(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	hello := func(total int) {
		e.send(t, conn, &protocol.HelloFrame{
			Type: protocol.FrameHello, Name: "node-1", Token: e.token,
			Namespaces:  []string{"default"},
			LocalLimits: protocol.Limits{Total: total},
			Instances: []protocol.HelloInstance{
				{Slot: intPtr(0)}, {Slot: intPtr(1)},
			},
		})
		if _, ok := e.read(t, conn).(*protocol.WelcomeFrame); !ok {
			t.Fatal("hello 应回 welcome")
		}
	}
	// 初始预算 2：第一条任务正常 grant，占住 slot 0。
	hello(2)
	taskA, dispatchA := e.dispatchOne(t, conn, capability)
	e.send(t, conn, &protocol.TaskAcceptFrame{
		Type: protocol.FrameTaskAccept, TaskID: taskA.ID, AssignmentID: dispatchA.AssignmentID,
		ProposedAttempt: dispatchA.ProposedAttempt, Node: "node-1", Slot: intPtr(0),
	})
	if _, ok := e.read(t, conn).(*protocol.LeaseGrantFrame); !ok {
		t.Fatal("预算内应授予 lease.grant")
	}
	// 预算收缩到 1：旧 busy 必须保留，新 accept 因预算/slot 越界被拒。
	hello(1)
	taskB := e.pendingTask(t, capability)
	if n, err := e.hub.DispatchOnce(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("DispatchOnce = %d, %v", n, err)
	}
	dispatchB, ok := e.read(t, conn).(*protocol.TaskDispatchFrame)
	if !ok || dispatchB.TaskID != taskB.ID {
		t.Fatalf("应收到第二条 dispatch: %+v", dispatchB)
	}
	e.send(t, conn, &protocol.TaskAcceptFrame{
		Type: protocol.FrameTaskAccept, TaskID: taskB.ID, AssignmentID: dispatchB.AssignmentID,
		ProposedAttempt: dispatchB.ProposedAttempt, Node: "node-1", Slot: intPtr(1),
	})
	if !waitFor(t, time.Second, func() bool {
		stored := e.task(t, taskB.ID)
		return stored.Phase == store.TaskPending && stored.WaitingReason == store.WaitingForSlot
	}) {
		t.Fatalf("预算收缩后的 accept 必须回 pending/waiting_for_slot: %+v", e.task(t, taskB.ID))
	}
	if stored := e.task(t, taskB.ID); stored.Attempt != 0 {
		t.Fatalf("被预算拒绝的 accept 不得计 attempt: %+v", stored)
	}
	if stored := e.task(t, taskA.ID); stored.Phase != store.TaskRunning || stored.Attempt != 1 {
		t.Fatalf("预算收缩不得影响已授权执行: %+v", stored)
	}
	instances := e.instances(t)
	busy := 0
	for _, inst := range instances {
		if inst.BusyTask != nil {
			busy++
		}
	}
	if busy != 1 {
		t.Fatalf("旧 busy 必须保留（收缩预算不假释放）: %+v", instances)
	}
	// 没有 grant 帧可收（最后的读会超时并关闭连接）。
	if _, _, err := e.readRaw(t, conn, 300*time.Millisecond); err == nil {
		t.Fatal("预算拒绝不得发 lease.grant")
	}
}
