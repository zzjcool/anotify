package runner

import (
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

func TestFinishUnreapedKeepsPhysicalSlotAndGCProtection(t *testing.T) {
	h := newHarness(t)
	att := &Attempt{r: h.r, taskID: "unreaped", attempt: 1, leaseID: "lease-unreaped", token: "token-unreaped", slot: 0, art: &artifact.Artifact{Metadata: artifact.Metadata{Name: "echo"}}}
	h.r.mu.Lock()
	slot := h.r.ensureSlotLocked(0)
	slot.taskID = att.taskID
	slot.attempt = 1
	slot.leaseID = att.leaseID
	slot.running = true
	h.r.running[att.taskID] = att
	h.r.tokens[att.token] = att
	h.r.mu.Unlock()
	h.r.finishAttempt(att)
	if !h.r.ActiveTasks()[att.taskID] {
		t.Fatal("unreaped group lost GC/physical occupancy protection")
	}
	instances := h.r.Instances()
	if len(instances) == 0 || instances[0].Busy == nil || !*instances[0].Busy || instances[0].LeaseID != att.leaseID {
		t.Fatalf("unreaped slot falsely idle: %+v", instances)
	}
	att.groupMu.Lock()
	att.groupReaped = true
	att.groupMu.Unlock()
	h.r.finishAttempt(att)
	if h.r.ActiveTasks()[att.taskID] {
		t.Fatal("confirmed reaped slot not freed")
	}
}
