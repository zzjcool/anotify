package ws

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

func TestFailedResultLeavesTraceFactAndRunsFallback(t *testing.T) {
	e := newEnv(t)
	e.capability(t, "issue-record", `{"type":"object","required":["repo","issue"],
		"properties":{"repo":{"type":"string"},"issue":{"type":"integer"},"outcome":{"type":"string"}}}`,
		true, nil)
	impl := e.capability(t, "issue-implement", `{"type":"object","required":["repo","issue"],
		"properties":{"repo":{"type":"string"},"issue":{"type":"integer"}}}`, false, []string{"issue-record"},
		func(spec *artifact.Spec) {
			spec.Lifecycle.OnFailure = &artifact.Fallback{
				To: "issue-record", Params: map[string]any{"outcome": "timeout"},
			}
		})
	task := &store.Task{
		NamespaceID: e.ns.ID, TraceID: "trace-700",
		CapabilityID: impl.ID, Capability: impl.Name,
		Version: impl.Version, Digest: impl.Digest,
		Params: json.RawMessage(`{"repo":"zzjcool/anotify","issue":700}`),
	}
	if err := e.db.CreateTasks(context.Background(), []*store.Task{task}); err != nil {
		t.Fatalf("建任务: %v", err)
	}
	conn := e.dial(t)
	leaseID, _ := e.directGrant(t, task)
	e.send(t, conn, &protocol.TaskResultFrame{
		Type: protocol.FrameTaskResult, TaskID: task.ID, Attempt: 1, Node: "node-1",
		LeaseID: leaseID, Phase: protocol.TaskPhaseFailed,
		Result: protocol.TaskResult{
			Summary:   "被超时杀掉",
			Artifacts: []protocol.TaskArtifact{},
			Error:     &protocol.TaskResultError{Reason: protocol.ReasonTimeout, Message: "lifecycle.timeoutSec 到点"},
		},
		StartedAt: time.Unix(e.now-60, 0).UTC(), FinishedAt: time.Unix(e.now, 0).UTC(),
	})
	if ack, ok := e.read(t, conn).(*protocol.ResultAckFrame); !ok || !ack.Accepted {
		t.Fatalf("失败结果也必须被 ACK（否则节点会一直重发），得到 %+v", ack)
	}

	var payload struct {
		FailureReason string `json:"failureReason"`
	}
	row := e.db.QueryRow(`SELECT payload FROM events WHERE namespace_id = ? AND trace_id = ? AND type = ?`,
		e.ns.ID, task.TraceID, protocol.ReasonTaskFailed)
	var raw string
	if err := row.Scan(&raw); err != nil {
		t.Fatalf("trace 里必须有一条 %s：%v", protocol.ReasonTaskFailed, err)
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("观察行不是 JSON: %v", err)
	}
	if payload.FailureReason != protocol.ReasonTimeout {
		t.Fatalf("观察行要带失败码 timeout（键名 failureReason），实际 %q", payload.FailureReason)
	}

	ctx := context.Background()
	var tasks []*store.Task
	if !waitFor(t, 2*time.Second, func() bool {
		var err error
		tasks, err = e.db.ListTasksByTrace(ctx, e.ns.ID, task.TraceID)
		return err == nil && len(tasks) == 2
	}) {
		t.Fatalf("兜底没有派发：trace 任务数 %d", len(tasks))
	}
	var child *store.Task
	for _, it := range tasks {
		if it.ID != task.ID {
			child = it
		}
	}
	if child == nil || child.Capability != "issue-record" || child.ParentTaskID == nil || *child.ParentTaskID != task.ID {
		t.Fatalf("兜底任务形状不对：%+v", child)
	}
	var params map[string]any
	if err := json.Unmarshal(child.Params, &params); err != nil {
		t.Fatalf("兜底参数不是对象: %v", err)
	}
	if params["issue"] != float64(700) || params["outcome"] != "timeout" {
		t.Fatalf("兜底要继承父任务上下文并带上声明参数，实际 %v", params)
	}
}
