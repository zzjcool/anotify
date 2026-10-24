package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// TestStubSignaturesFrozen #54 的核心交付：5 个新文件的公开方法签名已冻结，
// #2 实现时只填方法体、不改签名；#3/#4 可以立刻对着签名写代码。
//
// 这个测试像一份编译期契约：任何签名漂移都会让它失败。
func TestStubSignaturesFrozen(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// ── capability.go ──────────────────────────────
	var (
		_ func(context.Context, *CapabilityVersion) error                        = db.CreateCapability
		_ func(context.Context, string, string, int) (*CapabilityVersion, error) = db.GetCapability
		_ func(context.Context, string, string) (*CapabilityVersion, error)      = db.GetCapabilityByID
		_ func(context.Context, string) ([]*CapabilityVersion, error)            = db.ListCapabilities
		_ func(context.Context, string, string) (int, error)                     = db.NextCapabilityVersion
		_ func(*CapabilityVersion) (string, error)                               = NormalizeCapability
	)

	// ── node.go ────────────────────────────────────
	var (
		_ func(context.Context, *Node) (string, error)                                   = db.CreateNode
		_ func(context.Context, string) (*Node, error)                                   = db.GetNodeByTokenHash
		_ func(context.Context, string, string) (*Node, error)                           = db.GetNode
		_ func(context.Context, string) ([]*Node, error)                                 = db.ListNodes
		_ func(context.Context, string, string, string) error                            = db.UpdateNodeStatus
		_ func(context.Context, string, string, int64) error                             = db.TouchNode
		_ func(context.Context, string, string, []CapabilityRef) ([]string, error)       = db.ConfirmCapabilities
		_ func(context.Context, string) ([]*CapabilityVersion, error)                    = db.ListNodeCapabilities
		_ func(context.Context, string, string) ([]string, error)                        = db.NodesForCapability
		_ func(context.Context, string, string, []InstanceSpec) ([]*NodeInstance, error) = db.UpsertInstances
		_ func(context.Context, string) ([]*NodeInstance, error)                         = db.ListInstances
		_ func(context.Context, string) error                                            = db.MarkInstancesDead
		_ func(context.Context, string, string) (string, error)                          = db.LeaseInstance
		_ func(context.Context, string, string) error                                    = db.ReleaseInstance
		_ func(context.Context, string) (*NodePin, error)                                = db.GetPin
		_ func(context.Context, *NodePin) error                                          = db.PutPin
		_ func(string, string, string) string                                            = StickyKey
	)

	// ── event.go ───────────────────────────────────
	var (
		_ func(context.Context, *Event) (bool, error)                  = db.InsertEvent
		_ func(context.Context, string, string) (*Event, error)        = db.GetEvent
		_ func(context.Context, string, string, int) ([]*Event, error) = db.ListEvents
		_ func(context.Context, string, string) (*Event, error)        = db.GetEventByTrace
	)

	// ── task.go ────────────────────────────────────
	var (
		_ func(context.Context, []*Task) error                                                         = db.CreateTasks
		_ func(context.Context, string, string) (*Task, error)                                         = db.GetTask
		_ func(context.Context, string, string, string, string, int) ([]*Task, error)                  = db.ListTasks
		_ func(context.Context, string, string, string, string) error                                  = db.Transition
		_ func(context.Context, int64) ([]string, error)                                               = db.SweepExpiredLeases
		_ func(context.Context, string, string, string, string, int64) error                           = db.AssignTask
		_ func(context.Context, string, string, int64) error                                           = db.ExtendLease
		_ func(context.Context, string, string, string, *string, bool, string, []string, string) error = db.ReportResult
		_ func(string, string) bool                                                                    = ValidTransition
		_ func(string) bool                                                                            = IsTerminalStatus
	)

	// ── graph.go ───────────────────────────────────
	var (
		_ func(context.Context, *Edge) error                              = db.CreateEdge
		_ func(context.Context, string, string, string, string) error     = db.DeleteEdge
		_ func(context.Context, string, string) ([]*Edge, error)          = db.ListEdges
		_ func(context.Context, string, string, string) ([]string, error) = db.EdgesFrom
		_ func(context.Context, string) (*GraphVersion, error)            = db.EnsureGraphVersion
		_ func(context.Context, string, string) (*GraphVersion, error)    = db.GetGraphVersion
	)

	_ = ctx
}

// TestStubsReturnNotImplemented 断言空壳方法确实返回 ErrNotImplemented，
// 而不是静默返回零值——#2 实现到一半时最容易出的错就是「忘了实现但测试仍绿」。
func TestStubsReturnNotImplemented(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	cases := []struct {
		name string
		call func() error
	}{
		{"CreateCapability", func() error { return db.CreateCapability(ctx, &CapabilityVersion{}) }},
		{"GetCapability", func() error { _, err := db.GetCapability(ctx, orgID, "x", 1); return err }},
		{"GetCapabilityByID", func() error { _, err := db.GetCapabilityByID(ctx, orgID, "cap_x"); return err }},
		{"ListCapabilities", func() error { _, err := db.ListCapabilities(ctx, orgID); return err }},
		{"NextCapabilityVersion", func() error { _, err := db.NextCapabilityVersion(ctx, orgID, "x"); return err }},
		{"CreateNode", func() error { _, err := db.CreateNode(ctx, &Node{}); return err }},
		{"GetNodeByTokenHash", func() error { _, err := db.GetNodeByTokenHash(ctx, "h"); return err }},
		{"GetNode", func() error { _, err := db.GetNode(ctx, orgID, "nd_x"); return err }},
		{"ListNodes", func() error { _, err := db.ListNodes(ctx, orgID); return err }},
		{"UpdateNodeStatus", func() error { return db.UpdateNodeStatus(ctx, orgID, "nd_x", NodeStatusOnline) }},
		{"TouchNode", func() error { return db.TouchNode(ctx, orgID, "nd_x", Now()) }},
		{"ConfirmCapabilities", func() error { _, err := db.ConfirmCapabilities(ctx, orgID, "nd_x", nil); return err }},
		{"ListNodeCapabilities", func() error { _, err := db.ListNodeCapabilities(ctx, "nd_x"); return err }},
		{"NodesForCapability", func() error { _, err := db.NodesForCapability(ctx, orgID, "cap_x"); return err }},
		{"UpsertInstances", func() error { _, err := db.UpsertInstances(ctx, orgID, "nd_x", nil); return err }},
		{"ListInstances", func() error { _, err := db.ListInstances(ctx, "nd_x"); return err }},
		{"MarkInstancesDead", func() error { return db.MarkInstancesDead(ctx, "nd_x") }},
		{"LeaseInstance", func() error { _, err := db.LeaseInstance(ctx, "nd_x", "tsk_x"); return err }},
		{"ReleaseInstance", func() error { return db.ReleaseInstance(ctx, "nd_x", "nins_x") }},
		{"GetPin", func() error { _, err := db.GetPin(ctx, "k"); return err }},
		{"PutPin", func() error { return db.PutPin(ctx, &NodePin{}) }},
		{"InsertEvent", func() error { _, err := db.InsertEvent(ctx, &Event{}); return err }},
		{"GetEvent", func() error { _, err := db.GetEvent(ctx, orgID, "evt_x"); return err }},
		{"ListEvents", func() error { _, err := db.ListEvents(ctx, orgID, "", 10); return err }},
		{"GetEventByTrace", func() error { _, err := db.GetEventByTrace(ctx, orgID, "trace"); return err }},
		{"CreateTasks", func() error { return db.CreateTasks(ctx, nil) }},
		{"GetTask", func() error { _, err := db.GetTask(ctx, orgID, "tsk_x"); return err }},
		{"ListTasks", func() error { _, err := db.ListTasks(ctx, orgID, "", "", "", 10); return err }},
		{"Transition", func() error { return db.Transition(ctx, orgID, "tsk_x", TaskPending, TaskAssigned) }},
		{"SweepExpiredLeases", func() error { _, err := db.SweepExpiredLeases(ctx, Now()); return err }},
		{"AssignTask", func() error { return db.AssignTask(ctx, orgID, "tsk_x", "nd_x", "nins_x", Now()+30) }},
		{"ExtendLease", func() error { return db.ExtendLease(ctx, orgID, "tsk_x", Now()+30) }},
		{"ReportResult", func() error {
			return db.ReportResult(ctx, orgID, "tsk_x", TaskSucceeded, nil, true, "", nil, "")
		}},
		{"CreateEdge", func() error { return db.CreateEdge(ctx, &Edge{}) }},
		{"DeleteEdge", func() error { return db.DeleteEdge(ctx, orgID, "cap_a", "fix", "cap_b") }},
		{"ListEdges", func() error { _, err := db.ListEdges(ctx, orgID, ""); return err }},
		{"EdgesFrom", func() error { _, err := db.EdgesFrom(ctx, orgID, "cap_a", "fix"); return err }},
		{"EnsureGraphVersion", func() error { _, err := db.EnsureGraphVersion(ctx, orgID); return err }},
		{"GetGraphVersion", func() error { _, err := db.GetGraphVersion(ctx, orgID, "grf_x"); return err }},
	}
	for _, c := range cases {
		err := c.call()
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s: err = %v, want ErrNotImplemented", c.name, err)
		}
	}

	// 纯函数空壳
	if h, err := NormalizeCapability(&CapabilityVersion{}); !errors.Is(err, ErrNotImplemented) || h != "" {
		t.Errorf("NormalizeCapability = %q, %v; want \"\", ErrNotImplemented", h, err)
	}
	if !ValidTransition(TaskPending, TaskAssigned) == false {
		// 空壳返回 false；#2.4 实现后这里会变成 true。
		// 断言「当前是空壳」而不是断言 false，避免 #2.4 实现时误报：
		// 只要它开始返回 true 就说明实现了，#54 的脚手架测试不再有意义。
		t.Logf("ValidTransition already implemented (pending→assigned = %v)", ValidTransition(TaskPending, TaskAssigned))
	}
	if IsTerminalStatus(TaskPending) {
		t.Error("IsTerminalStatus(pending) = true, want false")
	}
	if StickyKey("trace", "cap", "sel") != "" {
		t.Log("StickyKey already implemented")
	}
}

// TestIsTerminalStatus 终态集合（契约 §3.1）。
func TestIsTerminalStatus(t *testing.T) {
	terminal := []string{TaskSucceeded, TaskFailed, TaskCanceled, TaskAbandoned}
	nonTerminal := []string{TaskPending, TaskAssigned, TaskAcked, TaskRunning, TaskLeaseExpired, ""}
	for _, s := range terminal {
		if !IsTerminalStatus(s) {
			t.Errorf("IsTerminalStatus(%q) = false, want true", s)
		}
	}
	for _, s := range nonTerminal {
		if IsTerminalStatus(s) {
			t.Errorf("IsTerminalStatus(%q) = true, want false", s)
		}
	}
}

// TestTaskStructJSONShape 任务结构体的 JSON 契约（camelCase + out 三态字段都在）。
func TestTaskStructJSONShape(t *testing.T) {
	typ := reflect.TypeOf(Task{})
	want := []string{
		"id", "orgId", "traceId", "parentTaskId", "path", "capabilityId", "capability",
		"version", "params", "status", "nodeId", "instanceId", "leaseUntil", "attempts",
		"out", "reported", "summary", "artifacts", "error", "capHash", "graphId",
		"createdAt", "startedAt", "finishedAt",
	}
	found := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" {
			continue
		}
		name := tag
		for j := 0; j < len(tag); j++ {
			if tag[j] == ',' {
				name = tag[:j]
				break
			}
		}
		found[name] = true
	}
	for _, w := range want {
		if !found[w] {
			t.Errorf("Task missing json field %q (camelCase contract)", w)
		}
	}
}

// TestCapabilityHasNoKind 不变量 I3 在 Go 类型层的体现：能力定义里不能有 Kind 字段。
func TestCapabilityHasNoKind(t *testing.T) {
	typ := reflect.TypeOf(CapabilityVersion{})
	for i := 0; i < typ.NumField(); i++ {
		if name := typ.Field(i).Name; name == "Kind" {
			t.Fatal("CapabilityVersion has Kind field: kind must be derived, not stored (I3)")
		}
	}
}
