package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

func TestRealPendingResultRetainsSlotUntilAuthoritativeACK(t *testing.T) {
	a, digest := makeCapability(t, "#!/bin/sh\necho real-result\n")
	f := realFixture(t, a, nil)
	f.ackResults = false
	conn := f.conn()
	f.welcome(conn)
	f.deploy(conn, digest)
	f.grant(conn, digest, 1)
	res := f.until(conn, func(v any) bool { _, ok := v.(*protocol.TaskResultFrame); return ok }, nil).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("real process did not succeed: %+v", res)
	}
	// The process has really ended, but its final result is not authoritative yet.
	f.write(conn, &protocol.TaskDispatchFrame{Type: protocol.FrameTaskDispatch, TaskID: "blocked-task", AssignmentID: "blocked-assignment", ProposedAttempt: 1, Namespace: "default", Capability: protocol.CapabilityRef{Name: "emitter", Version: 1, Digest: digest}, Params: json.RawMessage(`{}`), ConfigRevision: "r1", AssignmentExpiresAt: time.Now().Add(30 * time.Second)})
	v := f.until(conn, func(v any) bool {
		switch x := v.(type) {
		case *protocol.TaskRejectFrame:
			return x.TaskID == "blocked-task"
		case *protocol.TaskAcceptFrame:
			return x.TaskID == "blocked-task"
		}
		return false
	}, nil)
	if rej, ok := v.(*protocol.TaskRejectFrame); !ok || rej.Reason != protocol.ReasonSlotUnavailable {
		t.Fatalf("real receiver observed slot reuse before result ACK: %+v", v)
	}
	f.write(conn, &protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: res.TaskID, Attempt: 1, Accepted: true})
	f.grant(conn, digest, 2)
	res2 := f.until(conn, func(v any) bool { r, ok := v.(*protocol.TaskResultFrame); return ok && r.Attempt == 2 }, nil).(*protocol.TaskResultFrame)
	if res2.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("authoritative ACK did not permit exactly next grant: %+v", res2)
	}
	f.write(conn, &protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: res2.TaskID, Attempt: 2, Accepted: true})
}
