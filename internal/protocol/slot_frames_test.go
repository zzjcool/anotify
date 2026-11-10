package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A3（父 SLOT-HANDOFF）：物理槽位与历史回收接缝的具名回归。
//
// 三件事实必须逐字钉住，否则中枢会「静默丢槽」：
//  1. slot 有 presence：0 是合法槽号，缺失/负数无效；
//  2. dispatch.assignmentExpiresAt 是同一派发窗口；
//  3. reject 只允许 stale_config | slot_unavailable，未知 fail-closed。

func TestSlotPresenceZeroVsMissing(t *testing.T) {
	zero := 0
	neg := -1
	if !ValidSlot(&zero) {
		t.Fatal("slot=0 必须有 presence 且合法")
	}
	if ValidSlot(nil) {
		t.Fatal("缺失 slot 必须无效（0 不是缺失）")
	}
	if ValidSlot(&neg) {
		t.Fatal("负数 slot 必须无效")
	}

	// 0 必须出现在 wire 上（omitempty 会把 0 吞掉，那是错的）。
	acc := &TaskAcceptFrame{
		Type: FrameTaskAccept, TaskID: "t1", AssignmentID: "a1",
		ProposedAttempt: 1, Node: "n1", Slot: &zero,
	}
	if err := ValidateAccept(acc); err != nil {
		t.Fatalf("slot=0 的 accept 应合法：%v", err)
	}
	raw, err := Encode(acc)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(m["slot"]) != "0" {
		t.Fatalf("slot=0 必须写成 wire 数字 0：%s", raw)
	}

	// 缺失 = nil：wire 上是 null（presence 缺失），且校验必须拒绝。
	accMissing := *acc
	accMissing.Slot = nil
	if err := ValidateAccept(&accMissing); err == nil {
		t.Fatal("缺 slot 的 accept 必须被拒绝")
	}
	rawMissing, err := Encode(&accMissing)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(rawMissing), `"slot":null`) {
		t.Fatalf("缺失 slot 应写成 null（presence 缺失）：%s", rawMissing)
	}
	decoded, err := Decode(rawMissing)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := decoded.(*TaskAcceptFrame).Slot; got != nil {
		t.Fatalf("缺失 slot 解码后必须仍是 nil，实际 %v", *got)
	}
}

func TestLeaseGrantCarriesAssignmentAndSlot(t *testing.T) {
	slot := 3
	g := &LeaseGrantFrame{
		Type: FrameLeaseGrant, TaskID: "t1", Attempt: 2, Node: "n1",
		LeaseID: "l1", AttemptToken: "tok", ExpiresAt: time.Now().Add(time.Minute),
		AssignmentID: "a1", Slot: &slot,
	}
	if err := ValidateGrant(g); err != nil {
		t.Fatalf("合法 grant 被拒：%v", err)
	}
	raw, err := Encode(g)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(raw), `"assignmentId":"a1"`) || !strings.Contains(string(raw), `"slot":3`) {
		t.Fatalf("grant 必须带 assignmentId + slot：%s", raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := decoded.(*LeaseGrantFrame)
	if got.AssignmentID != "a1" || got.Slot == nil || *got.Slot != 3 {
		t.Fatalf("grant 往返丢了绑定：%+v", got)
	}

	for name, mutate := range map[string]func(*LeaseGrantFrame){
		"缺 assignmentId": func(g *LeaseGrantFrame) { g.AssignmentID = "" },
		"缺 slot":         func(g *LeaseGrantFrame) { g.Slot = nil },
		"负 slot":         func(g *LeaseGrantFrame) { bad := -1; g.Slot = &bad },
		"缺 leaseId":      func(g *LeaseGrantFrame) { g.LeaseID = "" },
		"缺 token":        func(g *LeaseGrantFrame) { g.AttemptToken = "" },
		"缺 expiresAt":    func(g *LeaseGrantFrame) { g.ExpiresAt = time.Time{} },
	} {
		c := *g
		mutate(&c)
		if err := ValidateGrant(&c); err == nil {
			t.Fatalf("%s 的 grant 必须被拒绝", name)
		}
	}
}

func TestDispatchAssignmentWindow(t *testing.T) {
	base := &TaskDispatchFrame{
		Type: FrameTaskDispatch, TaskID: "t1", AssignmentID: "a1", ProposedAttempt: 1,
		Namespace: "default", ConfigRevision: "r1",
		Capability: CapabilityRef{Name: "echo", Digest: "sha256:aa"},
	}
	if err := ValidateDispatch(base); err == nil {
		t.Fatal("缺 assignmentExpiresAt 的 dispatch 必须被拒绝")
	}
	base.AssignmentExpiresAt = time.Now().Add(30 * time.Second)
	if err := ValidateDispatch(base); err != nil {
		t.Fatalf("合法 dispatch 被拒：%v", err)
	}
	raw, err := Encode(base)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(raw), `"assignmentExpiresAt":`) {
		t.Fatalf("dispatch 必须带 assignmentExpiresAt：%s", raw)
	}
}

func TestRejectReasonClosedSetTwo(t *testing.T) {
	if !IsTaskRejectReason(ReasonStaleConfig) {
		t.Fatal("stale_config 必须在 reject 集合里")
	}
	if !IsTaskRejectReason(ReasonSlotUnavailable) {
		t.Fatal("slot_unavailable 必须在 reject 集合里")
	}
	for _, bad := range []string{"no_slot", "busy", "slot_conflict", "unknown", ""} {
		if IsTaskRejectReason(bad) {
			t.Fatalf("未知 reject reason 必须 fail-closed：%q", bad)
		}
	}
	if len(TaskRejectReasons()) != 2 {
		t.Fatalf("reject 集合必须恰好 2 项，实际 %v", TaskRejectReasons())
	}
	// 不容量的失败码不能混进 reject（它们计 attempt，走 task.result）。
	for _, code := range []string{ReasonMissingRequirement, ReasonWorkDirUnavailable, ReasonFilesEscape, ReasonArtifactExportInvalid} {
		if IsTaskRejectReason(code) {
			t.Fatalf("%q 不是 accept 前拒绝，不能进 reject 集合", code)
		}
	}
}

func TestCancelBindingFields(t *testing.T) {
	slot := 2
	c := &TaskCancelFrame{
		Type: FrameTaskCancel, TaskID: "t1", Attempt: 3, LeaseID: "l3",
		Slot: &slot, Reason: CancelReasonUser,
	}
	if err := ValidateCancel(c); err != nil {
		t.Fatalf("合法 cancel 被拒：%v", err)
	}
	ack := &CancelAckFrame{
		Type: FrameCancelAck, TaskID: "t1", Attempt: 3, LeaseID: "l3",
		Slot: &slot, ProcessGroupReaped: true,
	}
	if err := ValidateCancelAck(ack); err != nil {
		t.Fatalf("合法 cancel.ack 被拒：%v", err)
	}
	raw, err := Encode(ack)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(raw), `"leaseId":"l3"`) || !strings.Contains(string(raw), `"slot":2`) {
		t.Fatalf("cancel.ack 必须回带历史绑定：%s", raw)
	}
	for name, mutate := range map[string]func(*TaskCancelFrame){
		"缺 leaseId": func(f *TaskCancelFrame) { f.LeaseID = "" },
		"缺 attempt": func(f *TaskCancelFrame) { f.Attempt = 0 },
		"负 slot":    func(f *TaskCancelFrame) { bad := -1; f.Slot = &bad },
		"缺 slot":    func(f *TaskCancelFrame) { f.Slot = nil },
	} {
		x := *c
		mutate(&x)
		if err := ValidateCancel(&x); err == nil {
			t.Fatalf("%s 的 cancel 必须被拒绝", name)
		}
	}
	if err := ValidateCancelAck(&CancelAckFrame{Type: FrameCancelAck, TaskID: "t1", Attempt: 1}); err == nil {
		t.Fatal("缺 leaseId 的 cancel.ack 必须被拒绝")
	}
}

func TestHelloInstancePresenceShape(t *testing.T) {
	zero := 0
	no := false
	inst := HelloInstance{Slot: &zero, Busy: &no}
	raw, err := Encode(&HelloFrame{Type: FrameHello, Name: "n1", Instances: []HelloInstance{inst}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var insts []map[string]json.RawMessage
	if err := json.Unmarshal(top["instances"], &insts); err != nil || len(insts) != 1 {
		t.Fatalf("instances 形状不对：%s", top["instances"])
	}
	if string(insts[0]["slot"]) != "0" {
		t.Fatalf("slot=0 必须保留 presence：%s", top["instances"])
	}
	if string(insts[0]["busy"]) != "false" {
		t.Fatalf("显式 busy=false 必须保留（可区分缺失）：%s", top["instances"])
	}
	if _, ok := insts[0]["concurrency"]; ok {
		t.Fatalf("A3 已移除 concurrency wire 字段：%s", top["instances"])
	}

	// 缺 busy = 未知（不是 idle）：wire 上不出现该键。
	unknown := HelloInstance{Slot: &zero}
	raw, err = Encode(&HelloFrame{Type: FrameHello, Name: "n1", Instances: []HelloInstance{unknown}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(string(raw), `"busy"`) {
		t.Fatalf("缺 busy 不能写成 false/true：%s", raw)
	}
	// 缺 instances 是「没有新断言」：保持 null（不能伪造成 [] 空槽账本）。
	raw, err = Encode(&HelloFrame{Type: FrameHello, Name: "n1"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var top2 map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(top2["instances"]) != "null" {
		t.Fatalf("缺 instances 必须是 null（未知），实际 %s", top2["instances"])
	}
}

func TestConfigAppliedInstancesSnapshot(t *testing.T) {
	zero := 0
	yes := true
	frame := &ConfigAppliedFrame{
		Type: FrameConfigApplied, Revision: "7", Hash: "h",
		MachineVars: map[string]string{},
		Instances: []HelloInstance{{
			Slot: &zero, Busy: &yes, TaskID: "t1", AssignmentID: "a1", ProposedAttempt: 2,
		}},
	}
	raw, err := Encode(frame)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := decoded.(*ConfigAppliedFrame)
	if len(got.Instances) != 1 || got.Instances[0].Slot == nil || *got.Instances[0].Slot != 0 ||
		got.Instances[0].Busy == nil || !*got.Instances[0].Busy ||
		got.Instances[0].AssignmentID != "a1" || got.Instances[0].ProposedAttempt != 2 {
		t.Fatalf("config.applied.instances 往返丢了：%+v", got.Instances)
	}
}

func TestCancelAndCancelAckRequireSlotPresence(t *testing.T) {
	zero, negative := 0, -1
	for _, tc := range []struct {
		name  string
		slot  *int
		valid bool
	}{{"missing", nil, false}, {"negative", &negative, false}, {"zero-present", &zero, true}} {
		t.Run(tc.name, func(t *testing.T) {
			cancel := &TaskCancelFrame{Type: FrameTaskCancel, TaskID: "t", Attempt: 1, LeaseID: "lease", Slot: tc.slot}
			ack := &CancelAckFrame{Type: FrameCancelAck, TaskID: "t", Attempt: 1, LeaseID: "lease", Slot: tc.slot, ProcessGroupReaped: true}
			if err := ValidateCancel(cancel); (err == nil) != tc.valid {
				t.Errorf("cancel Validate slot %s: %v", tc.name, err)
			}
			if err := ValidateCancelAck(ack); (err == nil) != tc.valid {
				t.Errorf("cancel ACK Validate slot %s: %v", tc.name, err)
			}
		})
	}
}
