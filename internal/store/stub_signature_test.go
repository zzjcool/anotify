package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestStubSignaturesFrozen 公开方法签名已冻结：实现时只填方法体、不改签名。
//
// 这个测试像一份编译期契约：任何签名漂移都会让它失败。
// 它同时是 #2/#3/#4 并行开工的前提（各包对着签名写代码）。
func TestStubSignaturesFrozen(t *testing.T) {
	db := newTestDB(t)

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
		_ func(context.Context, []string) (map[string]int, error)                        = db.FreeSlotsByNode
		_ func(context.Context, string) error                                            = db.MarkInstancesDead
		_ func(context.Context, string, string) (string, error)                          = db.LeaseInstance
		_ func(context.Context, string, string) error                                    = db.ReleaseInstance
		_ func(context.Context, string) (*NodePin, error)                                = db.GetPin
		_ func(context.Context, *NodePin) error                                          = db.PutPin
		_ func(string, string, string) string                                            = StickyKey
	)

	// ── event.go ───────────────────────────────────
	var (
		_ func(context.Context, *Event) (bool, error)                          = db.InsertEvent
		_ func(context.Context, string, string) (*Event, error)                = db.GetEvent
		_ func(context.Context, string, string, string, int) ([]*Event, error) = db.ListEvents
		_ func(context.Context, string, string) (*Event, error)                = db.GetEventByTrace
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
		_ func(context.Context, *Edge) error                                    = db.CreateEdge
		_ func(context.Context, string, bool, string, string, string) error     = db.DeleteEdge
		_ func(context.Context, string, string) ([]*Edge, error)                = db.ListEdges
		_ func(context.Context, string, bool, string, string) ([]string, error) = db.EdgesFrom
		_ func(context.Context, string) (*GraphVersion, error)                  = db.EnsureGraphVersion
		_ func(context.Context, string, string) (*GraphVersion, error)          = db.GetGraphVersion
	)

	// ── org.go ─────────────────────────────────────
	var (
		_ func(context.Context, string) (*Org, error)         = db.CreateOrg
		_ func(context.Context, string) (*Org, error)         = db.GetOrg
		_ func(context.Context, string, string, string) error = db.AddOrgMember
		_ func(context.Context, string, string) (bool, error) = db.IsOrgMember
		_ func(context.Context, string) ([]*Org, error)       = db.ListOrgsByUser
		_ func(context.Context, string) ([]*OrgMember, error) = db.ListOrgMembers
		_ func(context.Context, string) (string, error)       = db.PrimaryOrgOf
		_ func(context.Context, *User, bool) (*Org, error)    = db.RegisterUserWithOrg
		_ func(context.Context, string, string) (*Org, error) = db.EnsureOrgForUser
	)
}

// TestNoStubRemains #55 完成后，store 包不应再有任何 ErrNotImplemented 空壳。
//
// 这条断言取代了脚手架阶段的 TestStubsReturnNotImplemented：
// 那时空壳**必须**返回 ErrNotImplemented 以暴露「忘了实现」，
// 现在实现完了就**必须**没有空壳，否则是漏实现的信号。
func TestNoStubRemains(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	// 逐个调用，断言不会再拿到 ErrNotImplemented。
	// 参数故意用「必然查不到」的值：合法实现应返回 ErrNotFound 或 nil，而不是 NotImplemented。
	calls := map[string]func() error{
		"CreateCapability": func() error {
			return db.CreateCapability(ctx, &CapabilityVersion{OrgID: orgID, Name: "x", Version: 1})
		},
		"GetCapability":         func() error { _, err := db.GetCapability(ctx, orgID, "x", 1); return err },
		"GetCapabilityByID":     func() error { _, err := db.GetCapabilityByID(ctx, orgID, "cap_x"); return err },
		"ListCapabilities":      func() error { _, err := db.ListCapabilities(ctx, orgID); return err },
		"NextCapabilityVersion": func() error { _, err := db.NextCapabilityVersion(ctx, orgID, "x"); return err },
		"CreateNode": func() error {
			_, err := db.CreateNode(ctx, &Node{OrgID: orgID, Name: "n-" + NewUserID()})
			return err
		},
		"GetNodeByTokenHash":   func() error { _, err := db.GetNodeByTokenHash(ctx, "nope"); return err },
		"GetNode":              func() error { _, err := db.GetNode(ctx, orgID, "nd_x"); return err },
		"ListNodes":            func() error { _, err := db.ListNodes(ctx, orgID); return err },
		"UpdateNodeStatus":     func() error { return db.UpdateNodeStatus(ctx, orgID, "nd_x", NodeStatusOnline) },
		"TouchNode":            func() error { return db.TouchNode(ctx, orgID, "nd_x", Now()) },
		"ConfirmCapabilities":  func() error { _, err := db.ConfirmCapabilities(ctx, orgID, "nd_x", nil); return err },
		"ListNodeCapabilities": func() error { _, err := db.ListNodeCapabilities(ctx, "nd_x"); return err },
		"NodesForCapability":   func() error { _, err := db.NodesForCapability(ctx, orgID, "cap_x"); return err },
		"UpsertInstances": func() error {
			_, err := db.UpsertInstances(ctx, orgID, "nd_x", []InstanceSpec{{Slot: 0}})
			return err
		},
		"ListInstances":     func() error { _, err := db.ListInstances(ctx, "nd_x"); return err },
		"FreeSlotsByNode":   func() error { _, err := db.FreeSlotsByNode(ctx, []string{"nd_x"}); return err },
		"MarkInstancesDead": func() error { return db.MarkInstancesDead(ctx, "nd_x") },
		"LeaseInstance":     func() error { _, err := db.LeaseInstance(ctx, "nd_x", "tsk_x"); return err },
		"ReleaseInstance":   func() error { return db.ReleaseInstance(ctx, "nd_x", "nins_x") },
		"GetPin":            func() error { _, err := db.GetPin(ctx, "k"); return err },
		"PutPin":            func() error { return db.PutPin(ctx, &NodePin{StickyKey: "k", OrgID: orgID, NodeID: "nd_x"}) },
		"InsertEvent": func() error {
			_, err := db.InsertEvent(ctx, &Event{OrgID: orgID, TraceID: "t", Source: "s", Type: "y"})
			return err
		},
		"GetEvent":           func() error { _, err := db.GetEvent(ctx, orgID, "evt_x"); return err },
		"ListEvents":         func() error { _, err := db.ListEvents(ctx, orgID, "", "", 10); return err },
		"GetEventByTrace":    func() error { _, err := db.GetEventByTrace(ctx, orgID, "trace"); return err },
		"CreateTasks":        func() error { return db.CreateTasks(ctx, nil) },
		"GetTask":            func() error { _, err := db.GetTask(ctx, orgID, "tsk_x"); return err },
		"ListTasks":          func() error { _, err := db.ListTasks(ctx, orgID, "", "", "", 10); return err },
		"SweepExpiredLeases": func() error { _, err := db.SweepExpiredLeases(ctx, Now()); return err },
		"AssignTask":         func() error { return db.AssignTask(ctx, orgID, "tsk_x", "nd_x", "nins_x", Now()+30) },
		"ExtendLease":        func() error { return db.ExtendLease(ctx, orgID, "tsk_x", Now()+30) },
		"ReportResult": func() error {
			return db.ReportResult(ctx, orgID, "tsk_x", TaskSucceeded, nil, true, "", nil, "")
		},
		"CreateEdge":         func() error { return db.CreateEdge(ctx, &Edge{}) },
		"DeleteEdge":         func() error { return db.DeleteEdge(ctx, orgID, false, "cap_a", "fix", "cap_b") },
		"ListEdges":          func() error { _, err := db.ListEdges(ctx, orgID, ""); return err },
		"EdgesFrom":          func() error { _, err := db.EdgesFrom(ctx, orgID, false, "cap_a", "fix"); return err },
		"EnsureGraphVersion": func() error { _, err := db.EnsureGraphVersion(ctx, orgID); return err },
		"GetGraphVersion":    func() error { _, err := db.GetGraphVersion(ctx, orgID, "grf_x"); return err },
	}
	for name, call := range calls {
		if err := call(); err != nil && err.Error() == ErrNotImplemented.Error() {
			t.Errorf("%s still returns ErrNotImplemented (#55 should have implemented it)", name)
		}
	}

	// 纯函数
	if _, err := NormalizeCapability(&CapabilityVersion{}); err != nil {
		t.Errorf("NormalizeCapability still a stub: %v", err)
	}
	if StickyKey("a", "b", "c") == "" {
		t.Error("StickyKey still a stub")
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

// TestNodeHidesTokenHash Node 的 token_hash 不得出现在 JSON 响应里（契约 §10.3）。
func TestNodeHidesTokenHash(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, token := mkNode(t, db, orgID, "mbp")
	got, err := db.GetNode(ctx, orgID, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal node: %v", err)
	}
	body := string(encoded)
	if len(body) == 0 {
		t.Fatal("empty marshaled node")
	}
	for _, leak := range []string{token, HashNodeToken(token), "token_hash", "tokenHash"} {
		if leak != "" && strings.Contains(body, leak) {
			t.Fatalf("node JSON leaks token material: %s", body)
		}
	}
}
