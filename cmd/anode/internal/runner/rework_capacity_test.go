package runner

import (
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

func TestReworkRecoveredOccupancyCountsPerCapabilityAndMax(t *testing.T) {
	for _, limit := range []string{"perCapability", "maxPerNode"} {
		t.Run(limit, func(t *testing.T) {
			h := newHarness(t)
			cmd, err := startSleepGroup(t, 30)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
			rec, err := h.results.Begin(resultstore.Record{TaskID: "old-capacity", Attempt: 1, Capability: "echo", Slot: intPtr(1), LeaseID: "old-lease", TTL: 0})
			if err != nil {
				t.Fatal(err)
			}
			if err := rec.SetPGID(cmd.Process.Pid); err != nil {
				t.Fatal(err)
			}
			if err := h.r.Recover(); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			if limit == "perCapability" {
				h.limits = protocol.Limits{Total: 2, PerCapability: map[string]int{"echo": 1}}
			}
			h.mu.Unlock()
			a, digest := h.artifact(func(a *artifact.Artifact) {
				if limit == "perCapability" {
					a.Spec.Concurrency.MaxPerNode = intPtr(2)
				}
			})
			h.dispatch("cap-blocked", 1, a, digest)
			if rej := findReject(h.sender.snapshot(), "cap-blocked"); rej == nil || rej.Reason != protocol.ReasonSlotUnavailable {
				t.Fatalf("recovered PGID was omitted from %s: %+v", limit, rej)
			}
			// One unrelated reservation may use the remaining total capacity.
			other, otherDigest := h.artifact(func(a *artifact.Artifact) { a.Metadata.Name = "unrelated" })
			h.dispatch("unrelated", 1, other, otherDigest)
			if !hasAccept(h.sender.snapshot(), "unrelated") {
				t.Fatal("per-capability restriction incorrectly blocked unrelated capability")
			}
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			h.waitUntil(t, "old physical capacity reaped", func() bool { return !h.r.ActiveTasks()["old-capacity"] }, time.Second)
			h.dispatch("cap-after-reap", 1, a, digest)
			if !hasAccept(h.sender.snapshot(), "cap-after-reap") {
				t.Fatal("physical reap failed to release capability capacity")
			}
		})
	}
}
