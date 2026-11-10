package ws

// g2r1_test.go 是 G2-R1（C1–C7）的回归：全部走真实 httptest WebSocket + 共享
// protocol.Decode + 真实 store，不 mock 会话、不手写 wire 字节。

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// dialCaptureWelcome 做一次真实 hello 并返回 welcome 帧（用于对比 dispatch 的字段）。
func (e *env) dialCaptureWelcome(t *testing.T) (*websocket.Conn, *protocol.WelcomeFrame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.server.URL, "http") + "/v1/node/stream"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("拨号节点: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	e.send(t, conn, &protocol.HelloFrame{
		Type: protocol.FrameHello, Name: e.node.Name, Token: e.token,
		Namespaces: []string{"default"},
		Instances:  []protocol.HelloInstance{{Slot: intPtr(0)}},
	})
	frame := e.read(t, conn)
	welcome, ok := frame.(*protocol.WelcomeFrame)
	if !ok {
		t.Fatalf("hello 后应收到 welcome，得到 %T", frame)
	}
	if !waitFor(t, time.Second, func() bool { return e.hub.Online(e.node.ID) }) {
		t.Fatal("节点未注册到 hub")
	}
	// 真实节点收到 welcome 后按共享 hash 规则上报 config.applied；dispatch 候选要求
	// applied_revision == revision（配置没 apply 的节点是 ConfigPending，不该被派发）。
	hash, err := protocol.MachineConfigHash(welcome.NodeVars, welcome.NodeLimits)
	if err != nil {
		t.Fatalf("按共享规则算机器配置 hash: %v", err)
	}
	e.send(t, conn, &protocol.ConfigAppliedFrame{
		Type: protocol.FrameConfigApplied, Revision: welcome.ConfigRevision, Hash: hash,
	})
	revision, err := strconv.ParseInt(welcome.ConfigRevision, 10, 64)
	if err != nil {
		t.Fatalf("welcome.configRevision 不是整数: %q", welcome.ConfigRevision)
	}
	if !waitFor(t, time.Second, func() bool {
		cfg, err := e.db.GetNodeConfig(context.Background(), e.node.ID)
		return err == nil && cfg.AppliedRevision == revision
	}) {
		t.Fatalf("节点配置未收敛到 applied_revision=%d", revision)
	}
	return conn, welcome
}

// dialRaw 只建立连接、不发 hello（握手负例用）。
func (e *env) dialRaw(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.server.URL, "http") + "/v1/node/stream"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("拨号节点: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// TestDispatchConfigRevisionMatchesWelcome 是 R1-C1 的回归（原 critical）：
// dispatch.configRevision 必须与 welcome.configRevision 同源（真实 node_configs.revision），
// 不能用 nodes.resource_version 充数。fixture 让两个值不同，按真实 anode 的
// stale_config 判等规则（欢迎值 == 派发值）验收，并继续 accept 拿到 grant。
func TestDispatchConfigRevisionMatchesWelcome(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// 写两次配置：node_configs.revision=2；nodes.resource_version 仍是创建时的小值。
	for i := 0; i < 2; i++ {
		cfg, err := e.db.GetNodeConfig(ctx, e.node.ID)
		if err != nil {
			t.Fatalf("读节点配置: %v", err)
		}
		cfg.Vars = map[string]string{"G2_R1_REV": strconv.Itoa(i + 1)}
		if err := e.db.PutNodeConfig(ctx, cfg); err != nil {
			t.Fatalf("写节点配置: %v", err)
		}
	}
	cfg, err := e.db.GetNodeConfig(ctx, e.node.ID)
	if err != nil {
		t.Fatalf("回读节点配置: %v", err)
	}
	node, err := e.db.GetNode(ctx, e.node.ID)
	if err != nil {
		t.Fatalf("读节点: %v", err)
	}
	if cfg.Revision == node.ResourceVersion {
		t.Fatalf("fixture 必须让 config revision(%d) 与 node resourceVersion(%d) 可区分",
			cfg.Revision, node.ResourceVersion)
	}

	capability := e.capability(t, "analyze", "", false, nil)
	conn, welcome := e.dialCaptureWelcome(t)
	wantRevision := strconv.FormatInt(cfg.Revision, 10)
	if welcome.ConfigRevision != wantRevision {
		t.Fatalf("welcome.configRevision = %q，want 真实配置 revision %q", welcome.ConfigRevision, wantRevision)
	}
	task, dispatch := e.dispatchOne(t, conn, capability)
	if dispatch.ConfigRevision != welcome.ConfigRevision {
		t.Fatalf("dispatch.configRevision = %q，welcome = %q：真实节点会判 stale_config",
			dispatch.ConfigRevision, welcome.ConfigRevision)
	}
	if dispatch.ConfigRevision == strconv.FormatInt(node.ResourceVersion, 10) {
		t.Fatalf("dispatch.configRevision 仍取自 nodes.resource_version(%d)", node.ResourceVersion)
	}

	// 真实执行链：按共享 protocol 的 accept 帧回带同一个 assignment/slot 0 → 拿到 grant。
	e.send(t, conn, &protocol.TaskAcceptFrame{
		Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
		ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Slot: intPtr(0),
	})
	grant, ok := e.read(t, conn).(*protocol.LeaseGrantFrame)
	if !ok || grant.TaskID != task.ID || grant.Attempt != 1 {
		t.Fatalf("config revision 对齐后必须授租约（不能空转 stale）: %+v", grant)
	}
	if stored := e.task(t, task.ID); stored.Phase != store.TaskRunning || stored.Attempt != 1 {
		t.Fatalf("任务未进入 running: %+v", stored)
	}
}

// TestDispatchConfigRevisionZeroWithoutConfigRow：没有 node_configs 行时 welcome 与
// dispatch 都必须是真实的 "0"（emptyNodeConfig.revision），不是 resource_version。
func TestDispatchConfigRevisionZeroWithoutConfigRow(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn, welcome := e.dialCaptureWelcome(t)
	if welcome.ConfigRevision != "0" {
		t.Fatalf("无配置行 welcome.configRevision = %q，want 0", welcome.ConfigRevision)
	}
	_, dispatch := e.dispatchOne(t, conn, capability)
	if dispatch.ConfigRevision != "0" {
		t.Fatalf("无配置行 dispatch.configRevision = %q，want 与 welcome 相同的 0", dispatch.ConfigRevision)
	}
}

// TestDispatchProposalStableUntilExpiry 是 R1-C2 的回归：未到期提案跨 sweep 保持
// 同一 assignment/proposedAttempt/窗口，不写 lease/attempt/token；到期才换新绑定。
func TestDispatchProposalStableUntilExpiry(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	e.deployApplied(t, capability)
	task := e.pendingTask(t, capability)

	if n, err := e.hub.DispatchOnce(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("第一次 DispatchOnce = %d, %v", n, err)
	}
	first, ok := e.read(t, conn).(*protocol.TaskDispatchFrame)
	if !ok {
		t.Fatal("应收到第一次 task.dispatch")
	}
	if n, err := e.hub.DispatchOnce(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("第二次 DispatchOnce = %d, %v", n, err)
	}
	second, ok := e.read(t, conn).(*protocol.TaskDispatchFrame)
	if !ok {
		t.Fatal("应收到第二次 task.dispatch")
	}
	if second.AssignmentID != first.AssignmentID || second.ProposedAttempt != first.ProposedAttempt ||
		!second.AssignmentExpiresAt.Equal(first.AssignmentExpiresAt) {
		t.Fatalf("未到期提案不得 churn: first=%+v second=%+v", first, second)
	}
	stored := e.task(t, task.ID)
	if stored.AssignmentID != first.AssignmentID || stored.ProposedAttempt != first.ProposedAttempt {
		t.Fatalf("派发候选未稳定落库: %+v", stored)
	}
	if stored.Phase != store.TaskPending || stored.Attempt != 0 || stored.LeaseID != "" ||
		stored.AttemptTokenHash != "" || stored.LeaseUntil != nil {
		t.Fatalf("派发不得授 lease/attempt/token: %+v", stored)
	}

	// 到期（expiry <= now 视到期）后允许新绑定。
	e.now += store.AssignmentWindowSec + 1
	if n, err := e.hub.DispatchOnce(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("到期后 DispatchOnce = %d, %v", n, err)
	}
	third, ok := e.read(t, conn).(*protocol.TaskDispatchFrame)
	if !ok {
		t.Fatal("应收到到期后的 task.dispatch")
	}
	if third.AssignmentID == first.AssignmentID {
		t.Fatalf("到期后必须允许新绑定，仍为 %q", third.AssignmentID)
	}
}

// TestRejectAssignmentRejectsForeignAndStaleRequests：错 assignment / 错 attempt /
// 另一节点 / 过期旧 id 都不能改当前提案或退避（0 写）。
func TestRejectAssignmentRejectsForeignAndStaleRequests(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task, dispatch := e.dispatchOne(t, conn, capability)

	// 第二台节点（不参与该任务候选）：用别台 assignment 拒绝必须 0 写。
	node2 := &store.Node{Name: "node-2", MaxInstances: 2, MaxConcurrency: 1, DeclaredNamespaces: []string{"default"}}
	token2, err := e.db.CreateNode(context.Background(), node2)
	if err != nil {
		t.Fatalf("建第二台节点: %v", err)
	}
	conn2 := e.dialNode(t, "node-2", token2)

	reject := func(conn *websocket.Conn, nodeName, assignment string, attempt int) {
		e.send(t, conn, &protocol.TaskRejectFrame{
			Type: protocol.FrameTaskReject, TaskID: task.ID, AssignmentID: assignment,
			ProposedAttempt: attempt, Node: nodeName, Reason: protocol.ReasonStaleConfig,
		})
	}
	unchanged := func(tag string) {
		t.Helper()
		stored := e.task(t, task.ID)
		if stored.AssignmentID != dispatch.AssignmentID || stored.Attempt != 0 ||
			stored.Phase != store.TaskPending || stored.WaitingReason != "" || stored.NextAttemptAt != nil {
			t.Fatalf("%s 不得改当前提案/退避: %+v", tag, stored)
		}
	}
	reject(conn, "node-1", "asg_wrong", dispatch.ProposedAttempt)
	// 这里原来靠 sleep(100ms)，慢机 handler 还没跑就会假绿；现改成帧处理完成的有序屏障。
	e.handled(t, conn)
	unchanged("错 assignment")
	reject(conn, "node-1", dispatch.AssignmentID, dispatch.ProposedAttempt+1)
	// 同上：sleep → 有序屏障（避免 handler 未跑就断言 0 写）。
	e.handled(t, conn)
	unchanged("错 proposedAttempt")
	reject(conn2, "node-2", dispatch.AssignmentID, dispatch.ProposedAttempt)
	// 同上：屏障必须发在发出该帧的连接（conn2）上。
	e.handled(t, conn2)
	unchanged("另一节点")
	// 过期旧 id：同一 assignment 在窗口过期后也不能再改（expiry <= now 视过期）。
	e.now += store.AssignmentWindowSec + 1
	reject(conn, "node-1", dispatch.AssignmentID, dispatch.ProposedAttempt)
	// 同上：sleep → 有序屏障。
	e.handled(t, conn)
	unchanged("过期旧 id")
}

// TestRejectAssignmentAuthorizedSuccessAndCredentialMatrix：当前凭证/ns 被撤销时
// 0 写；恢复后精确 reject 才回 pending + waiting_for_config + bounded backoff，
// 不计 attempt；slot 不被占用。
func TestRejectAssignmentAuthorizedSuccessAndCredentialMatrix(t *testing.T) {
	t.Run("revoked namespace", func(t *testing.T) {
		e := newEnv(t)
		capability := e.capability(t, "analyze", "", false, nil)
		conn := e.dial(t)
		task, dispatch := e.dispatchOne(t, conn, capability)
		cfg, err := e.db.GetNodeConfig(context.Background(), e.node.ID)
		if err != nil {
			t.Fatalf("读节点配置: %v", err)
		}
		cfg.Namespaces = []string{"other"}
		if err := e.db.PutNodeConfig(context.Background(), cfg); err != nil {
			t.Fatalf("撤销 ns: %v", err)
		}
		e.send(t, conn, &protocol.TaskRejectFrame{
			Type: protocol.FrameTaskReject, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
			ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Reason: protocol.ReasonSlotUnavailable,
		})
		// 这里原来靠 sleep(100ms)，慢机 handler 还没跑就会假绿；现改成帧处理完成的有序屏障。
		e.handled(t, conn)
		if stored := e.task(t, task.ID); stored.AssignmentID != dispatch.AssignmentID ||
			stored.WaitingReason != "" || stored.NextAttemptAt != nil {
			t.Fatalf("ns 撤销后 reject 必须 0 写: %+v", stored)
		}
		// 恢复授权后精确 reject 才生效。
		cfg.Namespaces = []string{"default"}
		if err := e.db.PutNodeConfig(context.Background(), cfg); err != nil {
			t.Fatalf("恢复 ns: %v", err)
		}
		e.send(t, conn, &protocol.TaskRejectFrame{
			Type: protocol.FrameTaskReject, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
			ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Reason: protocol.ReasonSlotUnavailable,
		})
		if !waitFor(t, time.Second, func() bool {
			stored := e.task(t, task.ID)
			return stored.WaitingReason == store.WaitingForSlot && stored.AssignmentID == "" &&
				stored.NextAttemptAt != nil && *stored.NextAttemptAt > e.now
		}) {
			t.Fatalf("精确 reject 应回 pending/waiting_for_slot + 退避: %+v", e.task(t, task.ID))
		}
		if stored := e.task(t, task.ID); stored.Attempt != 0 || stored.Phase != store.TaskPending {
			t.Fatalf("reject 不得计 attempt: %+v", stored)
		}
		if inst := e.instances(t); len(inst) != 1 || inst[0].BusyTask != nil {
			t.Fatalf("reject 不得授/占 slot: %+v", inst)
		}
	})

	t.Run("rotated token", func(t *testing.T) {
		e := newEnv(t)
		capability := e.capability(t, "analyze", "", false, nil)
		conn := e.dial(t)
		task, dispatch := e.dispatchOne(t, conn, capability)
		if err := e.db.SetNodeTokenHash(context.Background(), e.node.ID, store.HashNodeToken("rotated")); err != nil {
			t.Fatalf("轮换令牌: %v", err)
		}
		e.send(t, conn, &protocol.TaskRejectFrame{
			Type: protocol.FrameTaskReject, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
			ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Reason: protocol.ReasonStaleConfig,
		})
		if _, _, err := e.readRaw(t, conn, time.Second); err == nil {
			t.Fatal("凭证已轮换的会话在 reject 时必须被断开")
		}
		if stored := e.task(t, task.ID); stored.AssignmentID != dispatch.AssignmentID ||
			stored.WaitingReason != "" || stored.Attempt != 0 {
			t.Fatalf("旧凭证 reject 必须 0 写: %+v", stored)
		}
	})
}

// TestAcceptSlotPresence：slot 0 合法；缺失/负数 slot 拒且不授执行权。
func TestAcceptSlotPresence(t *testing.T) {
	t.Run("slot 0 valid", func(t *testing.T) {
		e := newEnv(t)
		capability := e.capability(t, "analyze", "", false, nil)
		conn := e.dial(t)
		task, dispatch := e.dispatchOne(t, conn, capability)
		e.send(t, conn, &protocol.TaskAcceptFrame{
			Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
			ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Slot: intPtr(0),
		})
		grant, ok := e.read(t, conn).(*protocol.LeaseGrantFrame)
		if !ok || grant.Slot == nil || *grant.Slot != 0 {
			t.Fatalf("slot 0 必须合法: %+v", grant)
		}
	})
	for _, tc := range []struct {
		name string
		slot *int
	}{
		{name: "missing", slot: nil},
		{name: "negative", slot: intPtr(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			capability := e.capability(t, "analyze", "", false, nil)
			conn := e.dial(t)
			task, dispatch := e.dispatchOne(t, conn, capability)
			e.send(t, conn, &protocol.TaskAcceptFrame{
				Type: protocol.FrameTaskAccept, TaskID: task.ID, AssignmentID: dispatch.AssignmentID,
				ProposedAttempt: dispatch.ProposedAttempt, Node: "node-1", Slot: tc.slot,
			})
			if _, _, err := e.readRaw(t, conn, time.Second); err == nil {
				t.Fatal("非法 slot presence 必须断开连接")
			}
			if stored := e.task(t, task.ID); stored.Attempt != 0 || stored.Phase != store.TaskPending {
				t.Fatalf("非法 slot 不得授执行权: %+v", stored)
			}
		})
	}
}

// TestDeployRejectsUnauthorizedExplicitTargetAtomically 是 R1-C4 的回归：显式目标里
// 任一节点未获 ns 授权 → store 整包 0 写，producer 不发任何 frame。
func TestDeployRejectsUnauthorizedExplicitTargetAtomically(t *testing.T) {
	e := newEnv(t)
	e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	node2 := &store.Node{Name: "node-2", MaxInstances: 2, MaxConcurrency: 1, DeclaredNamespaces: []string{"default"}}
	if _, err := e.db.CreateNode(context.Background(), node2); err != nil {
		t.Fatalf("建第二台节点: %v", err)
	}
	stored2, err := e.db.GetNodeByName(context.Background(), "node-2")
	if err != nil {
		t.Fatalf("回读第二台节点: %v", err)
	}
	cfg, err := e.db.GetNodeConfig(context.Background(), stored2.ID)
	if err != nil {
		t.Fatalf("读第二台配置: %v", err)
	}
	cfg.Namespaces = []string{"other"}
	if err := e.db.PutNodeConfig(context.Background(), cfg); err != nil {
		t.Fatalf("撤第二台 ns 授权: %v", err)
	}

	if _, err := e.hub.Deploy(context.Background(), e.ns.ID, "analyze", 0, []string{e.node.ID, stored2.ID}); err == nil {
		t.Fatal("显式目标含未授权节点必须报错（整包 0 写）")
	}
	for _, nodeID := range []string{e.node.ID, stored2.ID} {
		rows, err := e.db.ListDeployments(context.Background(), e.ns.ID, store.DeploymentFilter{NodeID: nodeID})
		if err != nil {
			t.Fatalf("列部署: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("store 拒绝后不得留下部署意图（node %s）: %+v", nodeID, rows)
		}
	}
	// 合法节点也不得收到任何 capability.deploy 帧。
	if frame, _, err := e.readRaw(t, conn, 300*time.Millisecond); err == nil {
		t.Fatalf("store 拒绝后不得发帧，收到 %T", frame)
	}
}

// TestDeployedReceiptAuthorizedNegatives：未知 request / 错 digest / 撤销 ns /
// 轮换 token 都必须 0 写（共享 DTO 校验后交 Authorized 事务，不 precheck+裸 Mark）。
func TestDeployedReceiptAuthorizedNegatives(t *testing.T) {
	e := newEnv(t)
	e.capability(t, "analyze", "", false, nil)
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
	deploymentID := strings.TrimPrefix(frame.RequestID, "dep_")
	if deploymentID == "" || deploymentID == frame.RequestID {
		t.Fatalf("requestId 必须稳定派生自部署 id: %q", frame.RequestID)
	}

	// 未知 requestId：关联不到任何 intent。
	e.send(t, conn, &protocol.CapabilityDeployedFrame{
		Type: protocol.FrameCapabilityDeployed, RequestID: "dep_unknown",
		DeployRefs: frame.DeployRefs, Status: protocol.DeployStatusApplied,
	})
	// 这里原来靠 sleep(100ms)，慢机 handler 还没跑就会假绿；现改成帧处理完成的有序屏障。
	e.handled(t, conn)
	pending := func(tag string) {
		t.Helper()
		row, err := e.db.GetDeployment(context.Background(), e.ns.ID, deploymentID)
		if err != nil {
			t.Fatalf("读部署: %v", err)
		}
		if row.State != store.DeploymentPending || row.Reason != "" {
			t.Fatalf("%s 不得改部署状态: %+v", tag, row)
		}
	}
	pending("未知 request")

	// 错 digest：refs 四元组不匹配当前 intent。
	bad := frame.DeployRefs
	bad.Digest = strings.Repeat("0", 64)
	e.send(t, conn, &protocol.CapabilityDeployedFrame{
		Type: protocol.FrameCapabilityDeployed, RequestID: frame.RequestID,
		DeployRefs: bad, Status: protocol.DeployStatusApplied,
	})
	// 同上：sleep → 有序屏障。
	e.handled(t, conn)
	pending("错 digest")

	// 撤销 ns 授权（当前事实）：即使 refs 完全正确也必须 0 写；恢复后 exact 才生效。
	cfg, err := e.db.GetNodeConfig(context.Background(), e.node.ID)
	if err != nil {
		t.Fatalf("读节点配置: %v", err)
	}
	cfg.Namespaces = []string{"other"}
	if err := e.db.PutNodeConfig(context.Background(), cfg); err != nil {
		t.Fatalf("撤销 ns: %v", err)
	}
	e.send(t, conn, &protocol.CapabilityDeployedFrame{
		Type: protocol.FrameCapabilityDeployed, RequestID: frame.RequestID,
		DeployRefs: frame.DeployRefs, Status: protocol.DeployStatusApplied,
	})
	// 同上：sleep → 有序屏障。
	e.handled(t, conn)
	pending("撤销 ns")

	// 轮换 token：会话身份不再是当前事实 → 断开且 0 写。
	if err := e.db.SetNodeTokenHash(context.Background(), e.node.ID, store.HashNodeToken("rotated")); err != nil {
		t.Fatalf("轮换令牌: %v", err)
	}
	e.send(t, conn, &protocol.CapabilityDeployedFrame{
		Type: protocol.FrameCapabilityDeployed, RequestID: frame.RequestID,
		DeployRefs: frame.DeployRefs, Status: protocol.DeployStatusApplied,
	})
	if _, _, err := e.readRaw(t, conn, time.Second); err == nil {
		t.Fatal("轮换后部署回执必须断开会话")
	}
	pending("轮换 token")
}

// TestDeployedReceiptRoundTripIdempotentAndSuperseded：正确部署 roundtrip、精确重复
// 幂等、changed body / superseded 旧 intent 0 写。
func TestDeployedReceiptRoundTripIdempotentAndSuperseded(t *testing.T) {
	e := newEnv(t)
	e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	outcomes, err := e.hub.Deploy(context.Background(), e.ns.ID, "analyze", 0, []string{e.node.ID})
	if err != nil || len(outcomes) != 1 {
		t.Fatalf("Deploy: %v/%+v", err, outcomes)
	}
	frame, ok := e.read(t, conn).(*protocol.CapabilityDeployFrame)
	if !ok {
		t.Fatal("应收到 capability.deploy")
	}
	deploymentID := strings.TrimPrefix(frame.RequestID, "dep_")

	// 正确 roundtrip：exact ack → applied。
	e.send(t, conn, &protocol.CapabilityDeployedFrame{
		Type: protocol.FrameCapabilityDeployed, RequestID: frame.RequestID,
		DeployRefs: frame.DeployRefs, Status: protocol.DeployStatusApplied,
	})
	if !waitFor(t, time.Second, func() bool {
		row, err := e.db.GetDeployment(context.Background(), e.ns.ID, deploymentID)
		return err == nil && row.State == store.DeploymentApplied
	}) {
		t.Fatal("正确部署回执未落 applied")
	}
	// 精确重复：幂等（不改任何事实）。
	e.send(t, conn, &protocol.CapabilityDeployedFrame{
		Type: protocol.FrameCapabilityDeployed, RequestID: frame.RequestID,
		DeployRefs: frame.DeployRefs, Status: protocol.DeployStatusApplied,
	})
	// 这里原来靠 sleep(100ms)，慢机 handler 还没跑就会假绿；现改成帧处理完成的有序屏障。
	e.handled(t, conn)
	row, err := e.db.GetDeployment(context.Background(), e.ns.ID, deploymentID)
	if err != nil || row.State != store.DeploymentApplied || row.Reason != "" {
		t.Fatalf("精确重复回执必须幂等: %v/%+v", err, row)
	}
	// changed body：applied 后再报 failed → 冲突 0 写。
	e.send(t, conn, &protocol.CapabilityDeployedFrame{
		Type: protocol.FrameCapabilityDeployed, RequestID: frame.RequestID,
		DeployRefs: frame.DeployRefs, Status: protocol.DeployStatusFailed, Error: "boom",
	})
	// 同上：sleep → 有序屏障。
	e.handled(t, conn)
	row, err = e.db.GetDeployment(context.Background(), e.ns.ID, deploymentID)
	if err != nil || row.State != store.DeploymentApplied || row.Reason != "" {
		t.Fatalf("changed body 必须 0 写: %v/%+v", err, row)
	}

	// superseded 旧 intent：行被移除后，旧 requestId 回执不得复活/新建事实。
	if _, err := e.db.DeleteDeployment(context.Background(), e.ns.ID, deploymentID); err != nil {
		t.Fatalf("删除旧 intent: %v", err)
	}
	e.send(t, conn, &protocol.CapabilityDeployedFrame{
		Type: protocol.FrameCapabilityDeployed, RequestID: frame.RequestID,
		DeployRefs: frame.DeployRefs, Status: protocol.DeployStatusApplied,
	})
	// 同上：sleep → 有序屏障。
	e.handled(t, conn)
	if _, err := e.db.GetDeployment(context.Background(), e.ns.ID, deploymentID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("superseded 旧 intent 回执不得复活事实: %v", err)
	}
	if rows, err := e.db.ListDeployments(context.Background(), e.ns.ID, store.DeploymentFilter{}); err != nil || len(rows) != 0 {
		t.Fatalf("superseded 后回执不得建新部署行: %v/%+v", err, rows)
	}
}

// startLogsRequest 起一次并发日志请求，返回结果通道。
func startLogsRequest(e *env, taskID string, attempt int, tailLines, sinceSeconds int, timeout time.Duration) <-chan struct {
	result *LogsResult
	err    error
} {
	ch := make(chan struct {
		result *LogsResult
		err    error
	}, 1)
	go func() {
		result, err := e.hub.RequestLogs(context.Background(), e.ns.ID, taskID, attempt, false,
			tailLines, sinceSeconds, timeout)
		ch <- struct {
			result *LogsResult
			err    error
		}{result: result, err: err}
	}()
	return ch
}

// TestLogsWaiterBoundedCorrelationAndWake 是 R1-C5 的回归：tail/since 转发、真实
// lines 严格校验、错 attempt/错 task/未登记/重复帧不阻塞读循环、超时/断连唤醒。
func TestLogsWaiterBoundedCorrelationAndWake(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	task := e.pendingTask(t, capability)
	_, _ = e.directGrant(t, task)

	// ① tailLines/sinceSeconds/attempt 原样转发；错 attempt 的**错误帧**不得被放行。
	ch := startLogsRequest(e, task.ID, 1, 5, 60, 3*time.Second)
	req, ok := e.read(t, conn).(*protocol.LogsRequestFrame)
	if !ok || req.Attempt != 1 || req.TailLines != 5 || req.SinceSeconds != 60 {
		t.Fatalf("logs_request 必须带真实 attempt/tail/since: %+v", req)
	}
	e.send(t, conn, &protocol.LogsResponseFrame{
		Type: protocol.FrameLogsResponse, TaskID: task.ID, RequestID: req.RequestID,
		Attempt: 2, Error: protocol.LogsErrNoLocalArchive,
	})
	if got := <-ch; !errors.Is(got.err, ErrLogsCorrelation) {
		t.Fatalf("错误帧声称别的 attempt 必须 ErrLogsCorrelation，得到 %v", got.err)
	}

	// ② 成功帧必须带真实 lines（sequence 严格递增/stream 合法/at 可解析）。
	ch = startLogsRequest(e, task.ID, 1, 0, 0, 3*time.Second)
	req, ok = e.read(t, conn).(*protocol.LogsRequestFrame)
	if !ok {
		t.Fatal("应收到第二次 logs_request")
	}
	badLines := &protocol.LogsResponseFrame{
		Type: protocol.FrameLogsResponse, TaskID: task.ID, RequestID: req.RequestID, Attempt: 1,
	}
	badLines.SetLogLines([]protocol.LogLine{{Sequence: 0, Stream: protocol.LogStreamStdout, Text: "x", At: "2024-01-02T03:04:05Z"}})
	e.send(t, conn, badLines)
	if got := <-ch; !errors.Is(got.err, ErrLogsCorrelation) {
		t.Fatalf("坏 lines 必须 ErrLogsCorrelation，得到 %v", got.err)
	}

	// ③ 重复帧：第一帧交付一次；第二帧被丢弃且读循环仍处理后续帧。
	ch = startLogsRequest(e, task.ID, 1, 0, 0, 3*time.Second)
	req, ok = e.read(t, conn).(*protocol.LogsRequestFrame)
	if !ok {
		t.Fatal("应收到第三次 logs_request")
	}
	good := &protocol.LogsResponseFrame{
		Type: protocol.FrameLogsResponse, TaskID: task.ID, RequestID: req.RequestID, Attempt: 1,
	}
	good.SetLogLines([]protocol.LogLine{
		{Sequence: 1, Stream: protocol.LogStreamStdout, Text: "hello", At: "2024-01-02T03:04:05.1Z"},
		{Sequence: 2, Stream: protocol.LogStreamStderr, Text: "warn", At: "2024-01-02T03:04:06Z"},
	})
	e.send(t, conn, good)
	got := <-ch
	if got.err != nil || got.result == nil || !got.result.Structured {
		t.Fatalf("真实 lines 应交付为结构化结果: %v/%+v", got.err, got.result)
	}
	if lines := got.result.Frame.LogLines(); len(lines) != 2 || lines[1].Text != "warn" {
		t.Fatalf("lines 未原样转发: %+v", lines)
	}
	e.send(t, conn, good) // 重复：无 waiter，丢弃
	e.send(t, conn, &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet})
	if _, ok := e.read(t, conn).(*protocol.WelcomeFrame); !ok {
		t.Fatal("重复 logs_response 不得阻塞读循环")
	}

	// ④ 未登记 requestId：丢弃且不阻塞读循环。
	e.send(t, conn, &protocol.LogsResponseFrame{
		Type: protocol.FrameLogsResponse, TaskID: task.ID, RequestID: "lreq_unknown",
		Attempt: 1, Error: protocol.LogsErrNoLocalArchive,
	})
	e.send(t, conn, &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet})
	if _, ok := e.read(t, conn).(*protocol.WelcomeFrame); !ok {
		t.Fatal("未登记 logs_response 不得阻塞读循环")
	}

	// ⑤ no_local_archive 原样转发（G3 映射 410），不冒充空成功。
	ch = startLogsRequest(e, task.ID, 1, 0, 0, 3*time.Second)
	req, ok = e.read(t, conn).(*protocol.LogsRequestFrame)
	if !ok {
		t.Fatal("应收到第四次 logs_request")
	}
	e.send(t, conn, &protocol.LogsResponseFrame{
		Type: protocol.FrameLogsResponse, TaskID: task.ID, RequestID: req.RequestID,
		Error: protocol.LogsErrNoLocalArchive,
	})
	got = <-ch
	if got.err != nil || got.result == nil || got.result.Frame.Error != protocol.LogsErrNoLocalArchive ||
		got.result.Structured || got.result.Empty {
		t.Fatalf("no_local_archive 必须原样转发: %v/%+v", got.err, got.result)
	}

	// ⑥ 超时唤醒。
	ch = startLogsRequest(e, task.ID, 1, 0, 0, 150*time.Millisecond)
	if _, ok := e.read(t, conn).(*protocol.LogsRequestFrame); !ok {
		t.Fatal("应收到第五次 logs_request")
	}
	if got := <-ch; !errors.Is(got.err, ErrLogsTimeout) {
		t.Fatalf("超时必须 ErrLogsTimeout，得到 %v", got.err)
	}

	// ⑦ 断连唤醒（不等超时）。
	ch = startLogsRequest(e, task.ID, 1, 0, 0, 5*time.Second)
	if _, ok := e.read(t, conn).(*protocol.LogsRequestFrame); !ok {
		t.Fatal("应收到第六次 logs_request")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	select {
	case got := <-ch:
		if !errors.Is(got.err, ErrNodeOffline) {
			t.Fatalf("断连唤醒必须 ErrNodeOffline，得到 %v", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("断连未唤醒 in-flight 日志请求（等超时不允许）")
	}
}

// TestHandshakeRejectsBadTokenAndName：坏 token / 名字不符 / 缺 token 必须在握手期
// fail closed（连接关闭、不注册 online），不是「先连上再说」。
//
// 退役映射（v3 → v4）。本测试是**形态映射**，不声明「v4 仍在 HTTP 层拒绝」：
//   - v3 的旧断言是「HTTP upgrade 不带合法凭证 → 不返回 101（连接根本不建立）」：
//     旧客户端把节点凭证放在 upgrade 请求上，认证失败是 HTTP 层拒绝，拿不到 101。
//   - v4 的等价防线在**首帧 hello** 上：upgrade 已建立（101）后，由 Hub.handshake
//     （hub.go:237-263，ServeHTTP 在 hub.go:175 调用）读首帧、要求它必须是 hello，
//     再用 Hub.authenticate 校验 token/name；失败时 ServeHTTP 直接
//     close(websocket.StatusPolicyViolation, "handshake failed") 并 return ——
//     会话从未 attach、不注册 online。serveSession 只跑握手之后的读循环，它分派到的
//     HelloFrame 是 re-hello（同样再走一次 Hub.authenticate，失败按
//     errProtocolViolation 断连）。凭证不再走 HTTP 头，所以 v4 不存在「HTTP 层拒绝」
//     这一形态，唯一可验收的等价物是「非法 hello 后连接断开 + 中枢 0 写」。
//   - 所以下面断言的是 v4 的真实形态（能建立连接、但非法 hello 必然断开且不留事实），
//     比 v3 的「连不上」更严：v3 无法区分「被拒」与「根本没通」。
func TestHandshakeRejectsBadTokenAndName(t *testing.T) {
	cases := []struct {
		name  string
		token string
		who   string
	}{
		{name: "bad token", token: "not-a-real-token", who: "node-1"},
		{name: "name mismatch", token: "", who: "other-node"}, // token 在用例里替换
		{name: "empty token", token: "", who: "node-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			token := tc.token
			if tc.name == "name mismatch" {
				token = e.token
			}
			conn := e.dialRaw(t)
			// hello 故意带上"会话事实"（能力申报 + 物理槽位 + agent 版本）：若服务端
			// 先注册后校验，下面的 0 写断言就能真的抓住那些写入（否则断言无从判别）。
			e.send(t, conn, &protocol.HelloFrame{
				Type: protocol.FrameHello, Name: tc.who, Token: token,
				Namespaces:   []string{"default"},
				Capabilities: []protocol.HelloCapability{{Name: "analyze", Version: 1}},
				Instances:    []protocol.HelloInstance{{Slot: intPtr(0)}},
				AgentVersion: "v9.9.9-forged", AgentCommit: "deadbeef",
			})
			if _, _, err := e.readRaw(t, conn, time.Second); err == nil {
				t.Fatal("非法 hello 必须断开连接")
			}
			if e.hub.Online(e.node.ID) {
				t.Fatal("非法 hello 不得注册 online 会话")
			}
			// 判别性 0 写：非法 hello 不得留下任何会话事实。
			//
			// 关于「capability 行」：v4 baseline 没有 node_capabilities 表（注册表基线测试
			// 明确要求该表在 v4 不存在：internal/store/node_test.go 的
			// TestNodeCapabilitiesNoHarnessColumns）；hello 申报的能力落在
			// nodes.declared_capabilities JSON。所以这里断言 v4 的等价事实：
			// 节点的能力申报快照必须仍是空的（加上槽位表为空）。
			ctx := context.Background()
			if rows, err := e.db.ListInstances(ctx, e.node.ID); err != nil || len(rows) != 0 {
				t.Fatalf("非法 hello 不得申报物理槽位: %v/%+v", err, rows)
			}
			node, err := e.db.GetNode(ctx, e.node.ID)
			if err != nil {
				t.Fatalf("回读节点: %v", err)
			}
			if len(node.DeclaredCapabilities) != 0 {
				t.Fatalf("非法 hello 不得写能力申报: %+v", node.DeclaredCapabilities)
			}
			if node.Status != store.NodeStatusOffline {
				t.Fatalf("非法 hello 不得把节点置为 %q（应保持 offline）", node.Status)
			}
			// agent 版本列同样必须仍为空：上面的 hello 明确声明了
			// v9.9.9-forged/deadbeef，而 applyHello → store.RecordHello 真的会写
			// nodes.agent_version/agent_commit（正常路径见 TestConfigAppliedAndAgentFactsPersist
			// 的 rehello 断言），所以这两条断言有判别力。
			if node.AgentVersion != "" || node.AgentCommit != "" {
				t.Fatalf("非法 hello 不得写 agent 版本事实: %q/%q", node.AgentVersion, node.AgentCommit)
			}
		})
	}
}

// TestHandshakeRejectsPreHelloFrames：hello 之前的任何帧（裸上行/下行/坏 JSON）都
// 必须 fail closed；原 tokenrotate/accept 测试不能冒替这三类握手拒绝。
func TestHandshakeRejectsPreHelloFrames(t *testing.T) {
	cases := []struct {
		name  string
		frame func(e *env) any
	}{
		{name: "config.get before hello", frame: func(e *env) any {
			return &protocol.ConfigGetFrame{Type: protocol.FrameConfigGet}
		}},
		{name: "upstream frame before hello", frame: func(e *env) any {
			return &protocol.LeaseRenewFrame{
				Type: protocol.FrameLeaseRenew, TaskID: "tsk_x", Attempt: 1, LeaseID: "lease_x",
			}
		}},
		{name: "downstream direction before hello", frame: func(e *env) any {
			return &protocol.TaskDispatchFrame{Type: protocol.FrameTaskDispatch, TaskID: "tsk_x"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			conn := e.dialRaw(t)
			e.send(t, conn, tc.frame(e))
			if _, _, err := e.readRaw(t, conn, time.Second); err == nil {
				t.Fatal("hello 之前的帧必须断开连接")
			}
			if e.hub.Online(e.node.ID) {
				t.Fatal("hello 之前的帧不得注册 online 会话")
			}
		})
	}
	t.Run("bad json before hello", func(t *testing.T) {
		e := newEnv(t)
		conn := e.dialRaw(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{not-json`)); err != nil {
			t.Fatalf("写坏帧: %v", err)
		}
		if _, _, err := e.readRaw(t, conn, time.Second); err == nil {
			t.Fatal("坏 JSON 首帧必须断开连接")
		}
	})
}

// TestDirectDispatchFrameHasNoNodeResourceVersion 是一个结构断言：dispatch 的
// configRevision 不能是可被误读的节点写令牌（C1 的值域在 TestDispatchConfigRevisionMatchesWelcome
// 用真实配置 revision 对照；这里保证 JSON 编码里没有 resourceVersion 泄漏）。
func TestDirectDispatchFrameHasNoNodeResourceVersion(t *testing.T) {
	e := newEnv(t)
	capability := e.capability(t, "analyze", "", false, nil)
	conn := e.dial(t)
	_, dispatch := e.dispatchOne(t, conn, capability)
	data, err := protocol.Encode(dispatch)
	if err != nil {
		t.Fatalf("编码 dispatch: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("解码 dispatch: %v", err)
	}
	if _, ok := raw["resourceVersion"]; ok {
		t.Fatalf("dispatch 帧不得出现 resourceVersion: %s", data)
	}
	if _, ok := raw["configRevision"]; !ok {
		t.Fatalf("dispatch 帧必须带 configRevision: %s", data)
	}
}
