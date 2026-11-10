package runner

import (
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
)

func TestReworkPendingResultDeadlineFencesReplayAndReleasesOnlyReaped(t *testing.T) {
	h, tr, att := slowUploadHarness(t, 1)
	close(takeUpload(t, tr).release)
	h.waitUntil(t, "result awaiting ACK", func() bool { return h.r.pendingResultCount() == 1 }, time.Second)
	if !h.r.ActiveTasks()["slow"] {
		t.Fatal("unacknowledged result lost slot")
	}
	h.clk.Advance(61 * time.Second)
	waitClosed(t, att.done, "missing result/lease ACK never ended authorization")
	if h.r.pendingResultCount() != 0 || att.authorized() || h.r.ActiveTasks()["slow"] {
		t.Fatal("deadline left replay authorization/slot")
	}
	before := len(h.sender.snapshot())
	h.r.SessionUp()
	if len(h.sender.snapshot()) != before {
		t.Fatal("reconnect replayed fenced old success result")
	}
}
func TestReworkPendingResultExplicitCancelFencesReplay(t *testing.T) {
	h, tr, att := slowUploadHarness(t, 1)
	close(takeUpload(t, tr).release)
	h.waitUntil(t, "result awaiting ACK", func() bool { return h.r.pendingResultCount() == 1 }, time.Second)
	h.cancel("slow", 1, "lease-slow")
	waitClosed(t, att.done, "explicit cancel never ended pending result")
	if h.r.pendingResultCount() != 0 || att.authorized() || h.r.ActiveTasks()["slow"] {
		t.Fatal("cancelled pending result remains authorized/occupied")
	}
}
func TestReworkSmallerGrantAndExpiredLeaseCannotExecute(t *testing.T) {
	for _, fence := range []string{"smallerAttempt", "expiredLease", "wrongNode"} {
		t.Run(fence, func(t *testing.T) {
			h := newHarness(t)
			a, digest := h.artifact(func(a *artifact.Artifact) { a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nprintf once > marker\n" })
			h.dispatch("strict", 2, a, digest)
			bad := grantFor(h, "strict", 2, "assign-strict", "bad-lease", 0)
			switch fence {
			case "smallerAttempt":
				bad.Attempt = 1
			case "expiredLease":
				bad.ExpiresAt = h.clk.Now().Add(-time.Second)
			case "wrongNode":
				bad.Node = "another-node"
			}
			h.r.HandleLeaseGrant(bad)
			h.r.mu.Lock()
			att := h.r.running["strict"]
			h.r.mu.Unlock()
			if att != nil {
				t.Fatalf("%s grant spawned execution", fence)
			}
		})
	}
}
