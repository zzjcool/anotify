package main

import (
	"fmt"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

func TestRealCancelMissingSlotCannotStopOrFakeReap(t *testing.T) {
	script := fmt.Sprintf("#!/bin/sh\nset -eu\n'%s' emit next --id before --data '{}'\n'%s' emit next --id after --data '{}'\necho still-alive-after-missing-slot\n", builtOld, builtOld)
	a, digest := makeCapability(t, script)
	f := realFixture(t, a, nil)
	c := f.conn()
	f.welcome(c)
	f.deploy(c, digest)
	f.grant(c, digest, 1)
	before := f.until(c, func(v any) bool { e, ok := v.(*protocol.TaskEmitFrame); return ok && e.ID == "before" }, nil).(*protocol.TaskEmitFrame)
	// Both decode shapes omit slot. The real runner must reject them before selecting
	// the running attempt or any history binding; it must never synthesize a reap ACK.
	f.write(c, &protocol.TaskCancelFrame{Type: protocol.FrameTaskCancel, TaskID: before.TaskID, Attempt: 1, LeaseID: before.LeaseID, Reason: protocol.CancelReasonUser})
	f.write(c, &protocol.TaskCancelFrame{Type: protocol.FrameTaskCancel, TaskID: "no-such-task", Attempt: 1, LeaseID: "no-such-lease", Reason: protocol.CancelReasonUser})
	rejectFakeReap := func(v any) {
		if ack, ok := v.(*protocol.CancelAckFrame); ok {
			t.Fatalf("missing-slot cancel produced physical ACK: %+v", ack)
		}
	}
	taskID := "downstream"
	ackEmit := func(e *protocol.TaskEmitFrame) {
		f.write(c, &protocol.TaskEmitAckFrame{Type: protocol.FrameTaskEmitAck, RequestID: e.RequestID, TaskID: e.TaskID, Attempt: e.Attempt, HTTPStatus: 200, Response: &protocol.EmitResponse{TaskID: &taskID, To: e.To, Version: 1}})
	}
	ackEmit(before)
	after := f.until(c, func(v any) bool { e, ok := v.(*protocol.TaskEmitFrame); return ok && e.ID == "after" }, rejectFakeReap).(*protocol.TaskEmitFrame)
	if after.AttemptToken != before.AttemptToken || after.LeaseID != before.LeaseID {
		t.Fatal("missing-slot cancel invalidated live authority")
	}
	ackEmit(after)
	result := f.until(c, func(v any) bool { _, ok := v.(*protocol.TaskResultFrame); return ok }, rejectFakeReap).(*protocol.TaskResultFrame)
	if result.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("missing-slot cancellation stopped actual task: %+v", result)
	}
	f.write(c, &protocol.LogsRequestFrame{Type: protocol.FrameLogsRequest, TaskID: result.TaskID, Attempt: 1, RequestID: "missing-slot-proof"})
	f.until(c, func(v any) bool {
		r, ok := v.(*protocol.LogsResponseFrame)
		return ok && r.RequestID == "missing-slot-proof"
	}, rejectFakeReap)
	// A server-side physical receiver must run the shared ValidateCancelAck before
	// touching its binding. Exercise the real decode→validation seam, not an ad hoc DTO.
	raw, err := protocol.Encode(&protocol.CancelAckFrame{Type: protocol.FrameCancelAck, TaskID: "logical-task", Attempt: 1, LeaseID: "lease-1", ProcessGroupReaped: true})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateCancelAck(decoded.(*protocol.CancelAckFrame)); err == nil {
		t.Fatal("decoded missing-slot ACK could fake physical reap")
	}
	t.Log("real WS receiver: missing slot cannot cancel live process or emit fake reap ACK; next emit and successful result prove authorization survived")
}
