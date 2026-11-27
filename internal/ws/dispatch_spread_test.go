package ws

// dispatch_spread_test.go 钉住 #681：提议不能把任务钉在单节点上（满预算跳过 / 被拒后换台 / 四单分散）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func (e *env) addNode(t *testing.T, name string) (*store.Node, string) {
	t.Helper()
	node := &store.Node{Name: name, MaxInstances: 4, MaxConcurrency: 2, DeclaredNamespaces: []string{"default"}}
	token, err := e.db.CreateNode(context.Background(), node)
	if err != nil {
		t.Fatalf("建节点 %s: %v", name, err)
	}
	stored, err := e.db.GetNodeByName(context.Background(), name)
	if err != nil {
		t.Fatalf("回读节点 %s: %v", name, err)
	}
	return stored, token
}

func (e *env) dialWithSlots(t *testing.T, name, token string, slots ...int) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.server.URL, "http") + "/v1/node/stream"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("拨号节点 %s: %v", name, err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	instances := make([]protocol.HelloInstance, 0, len(slots))
	for _, slot := range slots {
		instances = append(instances, protocol.HelloInstance{Slot: intPtr(slot)})
	}
	e.send(t, conn, &protocol.HelloFrame{
		Type: protocol.FrameHello, Name: name, Token: token,
		Namespaces: []string{"default"},
		Instances:  instances,
	})
	frame := e.read(t, conn)
	if _, ok := frame.(*protocol.WelcomeFrame); !ok {
		t.Fatalf("hello 后应收到 welcome，得到 %T", frame)
	}
	if !waitFor(t, time.Second, func() bool {
		node, err := e.db.GetNodeByName(context.Background(), name)
		return err == nil && e.hub.Online(node.ID)
	}) {
		t.Fatalf("节点 %s 未注册到 hub", name)
	}
	return conn
}

func (e *env) deployAppliedTo(t *testing.T, capability *store.CapabilityVersion, nodeIDs ...string) {
	t.Helper()
	ctx := context.Background()
	rows, err := e.db.CreateDeployments(ctx, e.ns.ID, capability.ID, capability.Digest, nodeIDs)
	if err != nil {
		t.Fatalf("建部署: %v", err)
	}
	for _, row := range rows {
		if err := e.db.MarkDeployResult(ctx, e.ns.ID, row.ID, store.DeploymentApplied, ""); err != nil {
			t.Fatalf("置部署 applied: %v", err)
		}
	}
}

func (e *env) pinnedCapability(t *testing.T, name string, maxPerNode int) *store.CapabilityVersion {
	t.Helper()
	perNode := maxPerNode
	spec := artifact.Spec{
		Params:      json.RawMessage(`{"type":"object"}`),
		Concurrency: artifact.Concurrency{MaxPerNode: &perNode},
		Run: artifact.Run{
			Entrypoint: "run.sh",
			Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
		},
	}
	capability := &store.CapabilityVersion{NamespaceID: e.ns.ID, Name: name, Version: 1, Spec: spec}
	if err := e.db.CreateCapability(context.Background(), capability); err != nil {
		t.Fatalf("建能力 %s: %v", name, err)
	}
	got, err := e.db.GetCapability(context.Background(), e.ns.ID, name, 1)
	if err != nil {
		t.Fatalf("回读能力: %v", err)
	}
	return got
}

func (e *env) grantOn(t *testing.T, node *store.Node, token string, task *store.Task, assignmentID string) {
	t.Helper()
	ctx := context.Background()
	if err := e.db.AssignTaskAt(ctx, task.ID, node.ID, assignmentID, 1, e.now); err != nil {
		t.Fatalf("派发候选 %s: %v", task.ID, err)
	}
	if _, _, _, attempt, _, err := e.db.GrantLeaseAuthorized(ctx,
		store.NodeAuthorization{NodeID: node.ID, NodeTokenHash: store.HashNodeToken(token)},
		task.ID, assignmentID, 1, 0, e.now); err != nil || attempt != 1 {
		t.Fatalf("在 %s 上授权 %s: attempt=%d err=%v", node.Name, task.ID, attempt, err)
	}
}

func (e *env) expectDispatch(t *testing.T, conn *websocket.Conn, taskID string) *protocol.TaskDispatchFrame {
	t.Helper()
	frame, _, err := e.readRaw(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("等 %s 的 task.dispatch 超时: %v", taskID, err)
	}
	dispatch, ok := frame.(*protocol.TaskDispatchFrame)
	if !ok {
		t.Fatalf("应收到 task.dispatch，得到 %T", frame)
	}
	if dispatch.TaskID != taskID {
		t.Fatalf("dispatch.taskId = %q，want %q", dispatch.TaskID, taskID)
	}
	return dispatch
}

func (e *env) expectNoFrame(t *testing.T, conn *websocket.Conn, d time.Duration) {
	t.Helper()
	frame, _, err := e.readRaw(t, conn, d)
	if err == nil {
		t.Fatalf("不该收到下行帧，却收到 %T: %+v", frame, frame)
	}
}

func TestDispatchSkipsNodeWhosePinnedBudgetIsFull(t *testing.T) {
	e := newEnv(t)
	capability := e.pinnedCapability(t, "triage", 1)
	other, otherToken := e.addNode(t, "node-2")
	busyConn := e.dialWithSlots(t, e.node.Name, e.token, 0, 1)
	idleConn := e.dialWithSlots(t, "node-2", otherToken, 0, 1)
	e.deployAppliedTo(t, capability, e.node.ID, other.ID)

	running := e.pendingTask(t, capability)
	e.grantOn(t, e.node, e.token, running, "asg_681_running")

	next := e.pendingTask(t, capability)
	if n, err := e.hub.DispatchOnce(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("DispatchOnce = %d, %v（第二单必须被派出去）", n, err)
	}
	dispatch := e.expectDispatch(t, idleConn, next.ID)
	if dispatch.AssignmentID == "" {
		t.Fatalf("dispatch 必须带候选 assignment: %+v", dispatch)
	}
	e.expectNoFrame(t, busyConn, 300*time.Millisecond)

	stored := e.task(t, next.ID)
	if stored.NodeID == nil || *stored.NodeID != other.ID {
		t.Fatalf("第二单的候选应落在空闲节点 %s，实际 %+v", other.Name, stored.NodeID)
	}
}

func TestDispatchMovesToAnotherNodeAfterSlotUnavailable(t *testing.T) {
	e := newEnv(t)
	capability := e.pinnedCapability(t, "triage-move", 1)
	other, otherToken := e.addNode(t, "node-2")
	firstConn := e.dialWithSlots(t, e.node.Name, e.token, 0, 1)
	secondConn := e.dialWithSlots(t, "node-2", otherToken, 0, 1)
	e.deployAppliedTo(t, capability, e.node.ID, other.ID)

	task := e.pendingTask(t, capability)
	if n, err := e.hub.DispatchOnce(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("第一轮 DispatchOnce = %d, %v", n, err)
	}
	first := e.expectDispatch(t, firstConn, task.ID)

	e.send(t, firstConn, &protocol.TaskRejectFrame{
		Type: protocol.FrameTaskReject, TaskID: task.ID,
		AssignmentID: first.AssignmentID, ProposedAttempt: first.ProposedAttempt,
		Node: e.node.Name, Reason: store.TaskRejectSlotUnavailable,
	})
	e.handled(t, firstConn)

	rejected := e.task(t, task.ID)
	if rejected.AssignmentID != "" || rejected.NodeID != nil {
		t.Fatalf("slot_unavailable 必须立即释放候选绑定：%+v", rejected)
	}
	if rejected.WaitingReason != store.WaitingForSlot {
		t.Fatalf("slot_unavailable 应回 waiting_for_slot：%+v", rejected)
	}

	e.now += store.SlotUnavailableBackoff(0) + 1
	if n, err := e.hub.DispatchOnce(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("第二轮 DispatchOnce = %d, %v", n, err)
	}
	second := e.expectDispatch(t, secondConn, task.ID)
	if second.AssignmentID == first.AssignmentID {
		t.Fatalf("换节点必须换候选身份，仍是 %q", first.AssignmentID)
	}
	e.expectNoFrame(t, firstConn, 300*time.Millisecond)

	stored := e.task(t, task.ID)
	if stored.NodeID == nil || *stored.NodeID != other.ID {
		t.Fatalf("被拒后的候选应漂到 %s，实际 %+v", other.Name, stored.NodeID)
	}
}

func TestDispatchBurstSpreadsAcrossNodes(t *testing.T) {
	e := newEnv(t)
	capability := e.pinnedCapability(t, "triage-burst", 1)
	other, otherToken := e.addNode(t, "node-2")
	firstConn := e.dialWithSlots(t, e.node.Name, e.token, 0, 1, 2)
	secondConn := e.dialWithSlots(t, "node-2", otherToken, 0, 1, 2)
	e.deployAppliedTo(t, capability, e.node.ID, other.ID)

	for i := 0; i < 4; i++ {
		e.pendingTask(t, capability)
	}
	n, err := e.hub.DispatchOnce(context.Background(), 10)
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if n != 4 {
		t.Fatalf("四条 pending 任务都应被派发（预算满也有兜底档），实际 %d", n)
	}

	perNode := map[string]int{}
	pairs := []struct {
		conn *websocket.Conn
		id   string
	}{{firstConn, e.node.ID}, {secondConn, other.ID}}
	for i := 0; i < 4; i++ {

		got := false
		for _, pair := range pairs {
			frame, _, rerr := e.readRaw(t, pair.conn, 300*time.Millisecond)
			if rerr != nil {
				continue
			}
			dispatch, ok := frame.(*protocol.TaskDispatchFrame)
			if !ok {
				t.Fatalf("第 %d 帧应是 task.dispatch，得到 %T", i+1, frame)
			}
			perNode[pair.id]++
			if stored := e.task(t, dispatch.TaskID); stored.NodeID == nil || *stored.NodeID != pair.id {
				t.Fatalf("任务 %s 的候选应落在 %s（收到帧的那台）", dispatch.TaskID, pair.id)
			}
			got = true
			break
		}
		if !got {
			t.Fatalf("第 %d 条 dispatch 既没到 node-1 也没到 node-2", i+1)
		}
	}
	if perNode[e.node.ID] != 2 || perNode[other.ID] != 2 {
		t.Fatalf("四单应两台各分到 2 条候选（超得少的先派，不许全堆一台）：%v", perNode)
	}
}

func TestDispatchFallsBackWhenEveryCandidateIsFull(t *testing.T) {
	e := newEnv(t)
	capability := e.pinnedCapability(t, "triage-fallback", 1)
	conn := e.dialWithSlots(t, e.node.Name, e.token, 0, 1)
	e.deployAppliedTo(t, capability, e.node.ID)

	running := e.pendingTask(t, capability)
	e.grantOn(t, e.node, e.token, running, "asg_681_fallback_running")

	next := e.pendingTask(t, capability)
	if n, err := e.hub.DispatchOnce(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("唯一候选预算已满也必须派发（grant 是权威）：DispatchOnce = %d, %v", n, err)
	}
	dispatch := e.expectDispatch(t, conn, next.ID)
	if dispatch.TaskID != next.ID {
		t.Fatalf("应收到 %s 的 dispatch，得到 %s", next.ID, dispatch.TaskID)
	}
}
