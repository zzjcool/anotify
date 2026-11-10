package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/cmd/anode/internal/upload"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// A real HTTP receiver blocks each response while the lease fakeclock advances.
// The production Upload flow must cancel both its request and the receiver context.
type heldUpload struct {
	req       *http.Request
	release   chan struct{}
	cancelled chan struct{}
}
type heldTransport struct{ requests chan *heldUpload }

func (tr *heldTransport) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	u := &heldUpload{req: req, release: make(chan struct{}), cancelled: make(chan struct{})}
	tr.requests <- u
	select {
	case <-req.Context().Done():
		close(u.cancelled)
		return
	case <-u.release:
	}
	path, _ := protocol.DecodeArtifactPathHeader(req.Header.Get(protocol.ArtifactHeaderArtifactPath))
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v1/node/tasks/"), "/artifacts/")
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(protocol.ArtifactUploadResponse{TaskID: parts[0], Attempt: 1, Path: path, SHA256: parts[1], Size: int64(len(body))})
}
func slowUploadHarness(t *testing.T, files int) (*harness, *heldTransport, *Attempt) {
	t.Helper()
	h := newHarness(t)
	h.sender.onResult = nil // explicit delayed ACK in the lease lifecycle regressions
	tr := &heldTransport{requests: make(chan *heldUpload, 8)}
	srv := httptest.NewServer(tr)
	t.Cleanup(srv.Close)
	h.r.opts.Upload = &upload.Client{BaseURL: srv.URL, NodeToken: "node-token", HTTP: srv.Client(), Retries: 0}
	a, digest := h.artifact(func(a *artifact.Artifact) {
		script := "#!/bin/sh\n"
		for i := 0; i < files; i++ {
			script += fmt.Sprintf("printf payload > \"$ANOTIFY_ARTIFACT_DIR/%d.txt\"\n", i)
		}
		a.Spec.Run.Files["run.sh"] = script
	})
	h.dispatch("slow", 1, a, digest)
	h.grant("slow", 1, "lease-slow", "token-slow")
	var att *Attempt
	h.waitUntil(t, "actual upload phase", func() bool {
		h.r.mu.Lock()
		att = h.r.running["slow"]
		h.r.mu.Unlock()
		if att == nil {
			return false
		}
		select {
		case <-att.procDone:
			return true
		default:
			return false
		}
	}, 2*time.Second)
	t.Cleanup(att.cancelUpload) // unblock the transport even on a RED fatal assertion
	return h, tr, att
}
func takeUpload(t *testing.T, tr *heldTransport) *heldUpload {
	t.Helper()
	select {
	case u := <-tr.requests:
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("upload request not started")
		return nil
	}
}
func waitClosed(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}
func renews(h *harness) int {
	n := 0
	for _, f := range h.sender.snapshot() {
		if r, ok := f.(*protocol.LeaseRenewFrame); ok && r.TaskID == "slow" {
			n++
		}
	}
	return n
}
func TestReworkSlowUploadsRenewAcross20And60(t *testing.T) {
	h, tr, att := slowUploadHarness(t, 3)
	finished := h.clk.Now()
	for i := 0; i < 3; i++ {
		u := takeUpload(t, tr)
		// Each file occupies a real in-flight request for 25 simulated seconds.
		h.clk.Advance(20 * time.Second)
		h.waitUntil(t, "renew while process is already done", func() bool { h.clk.Advance(0); return renews(h) >= i+1 }, time.Second)
		h.r.HandleLeaseAck(&protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: "slow", Attempt: 1, LeaseID: "lease-slow"})
		h.clk.Advance(5 * time.Second)
		if err := u.req.Context().Err(); err != nil {
			t.Fatalf("ACK-backed upload cancelled: %v", err)
		}
		close(u.release)
	}
	h.waitUntil(t, "result submitted but ACK delayed", func() bool { return findResult(h.sender.snapshot(), "slow", 1) != nil }, time.Second)
	res := findResult(h.sender.snapshot(), "slow", 1)
	if res == nil || res.Phase != protocol.TaskPhaseSucceeded || len(res.Result.Artifacts) != 3 {
		t.Fatalf("three confirmed uploads must succeed: %+v", res)
	}
	if !res.FinishedAt.Equal(finished) || h.clk.Now().Sub(res.FinishedAt) != 75*time.Second {
		t.Fatalf("finishedAt includes transfer time: %+v now=%s", res, h.clk.Now())
	}
	if h.r.opts.LeaseTTL != 60*time.Second || h.r.opts.RenewInterval != 20*time.Second || h.r.opts.TermGrace != 5*time.Second {
		t.Fatal("production lease defaults changed")
	}
	for i := 0; i < 4; i++ {
		n := renews(h)
		h.clk.Advance(20 * time.Second)
		h.waitUntil(t, "pending result must renew until trusted ACK", func() bool { h.clk.Advance(0); return renews(h) > n }, time.Second)
		h.r.HandleLeaseAck(&protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: "slow", Attempt: 1, LeaseID: "lease-slow"})
		if !h.r.ActiveTasks()["slow"] || !att.authorized() {
			t.Fatal("pending result ACK lost slot/authorization")
		}
	}
	h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "slow", Attempt: 99, Accepted: true})
	if !h.r.ActiveTasks()["slow"] {
		t.Fatal("wrong ACK cleared current result slot")
	}
	h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "slow", Attempt: 1, Accepted: true})
	waitClosed(t, att.done, "trusted ACK did not finish attempt")
	n := renews(h)
	h.clk.Advance(100 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if renews(h) != n || att.authorized() || h.clk.Pending() != 0 || h.r.ActiveTasks()["slow"] {
		t.Fatalf("lease loop/slot leaked beyond trusted ACK: renew=%d->%d pending=%d", n, renews(h), h.clk.Pending())
	}

}
func TestReworkUploadDeadlineCancelsAndFencesLateResult(t *testing.T) {
	h, tr, att := slowUploadHarness(t, 2)
	u := takeUpload(t, tr)
	h.clk.Advance(61 * time.Second)
	// Old ACK cannot restore the expired attempt authorization.
	h.r.HandleLeaseAck(&protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: "slow", Attempt: 1, LeaseID: "wrong-lease"})
	waitClosed(t, u.cancelled, "last-ACK + 60s deadline did not cancel the in-flight upload")
	waitClosed(t, att.done, "expired upload did not settle")
	if att.authorized() || findResult(h.sender.snapshot(), "slow", 1) != nil {
		t.Fatal("expired transfer sent a late result or retained authority")
	}
	if len(tr.requests) != 0 {
		t.Fatal("expired transfer started another file")
	}
	if h.r.ActiveTasks()["slow"] {
		t.Fatal("physically reaped expired attempt kept a slot")
	}
}
func TestReworkExplicitCancelStopsUploadAndReaps(t *testing.T) {
	h, tr, att := slowUploadHarness(t, 2)
	u := takeUpload(t, tr)
	h.cancel("slow", 1, "lease-slow")
	waitClosed(t, u.cancelled, "explicit cancel did not cancel upload context")
	waitClosed(t, att.done, "cancelled upload did not settle")
	ack := h.waitFrame(t, func(f any) bool { c, ok := f.(*protocol.CancelAckFrame); return ok && c.TaskID == "slow" }, time.Second).(*protocol.CancelAckFrame)
	if !ack.ProcessGroupReaped || ack.LeaseID != "lease-slow" || ack.Slot == nil || *ack.Slot != 0 {
		t.Fatalf("wrong physical cancel ACK: %+v", ack)
	}
	if findResult(h.sender.snapshot(), "slow", 1) != nil || att.authorized() {
		t.Fatal("cancel during upload still published a result")
	}
}
func grantFor(h *harness, task string, attempt int, assignment, lease string, slot int) *protocol.LeaseGrantFrame {
	return &protocol.LeaseGrantFrame{Type: protocol.FrameLeaseGrant, TaskID: task, Attempt: attempt, Node: "node-a", AssignmentID: assignment, Slot: intPtr(slot), LeaseID: lease, AttemptToken: "token-" + lease, ExpiresAt: h.clk.Now().Add(protocol.LeaseTTL)}
}
func dispatchFor(h *harness, task, assignment string, a *artifact.Artifact, digest string) {
	h.r.HandleDispatch(&protocol.TaskDispatchFrame{Type: protocol.FrameTaskDispatch, TaskID: task, AssignmentID: assignment, ProposedAttempt: 1, Namespace: "default", Capability: protocol.CapabilityRef{Name: a.Metadata.Name, Version: a.Metadata.Version, Digest: digest}, ConfigRevision: "rev-1", Params: json.RawMessage(`{}`), AssignmentExpiresAt: h.clk.Now().Add(30 * time.Second)})
}
func TestReworkFutureGrantMustExactlyMatchProposedAttempt(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil)
	h.dispatch("future", 1, a, digest)
	h.r.HandleLeaseGrant(grantFor(h, "future", 2, "assign-future", "future-lease", 0))
	h.r.mu.Lock()
	running := h.r.running["future"]
	res := h.r.reservations["future"]
	h.r.mu.Unlock()
	if running != nil {
		t.Fatal("proposedAttempt=1 accepted future grant attempt=2 and spawned")
	}
	if res == nil || res.proposed != 1 {
		t.Fatal("mismatched future grant consumed the valid reservation")
	}
	if failure := findResult(h.sender.snapshot(), "future", 2); failure != nil || h.r.pendingResultCount() != 0 {
		t.Fatalf("unmatched future grant must not create an execution result/ACK alias: %+v", failure)
	}
	h.r.HandleLeaseGrant(grantFor(h, "future", 1, "assign-future", "correct-lease", 0))
	h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "future" && r.Attempt == 1 && r.Phase == protocol.TaskPhaseSucceeded
	}, 2*time.Second)
	if _, err := os.Stat(filepath.Join(h.workRoot, hexNameForTest("future"), "2")); !os.IsNotExist(err) {
		t.Fatalf("future attempt created work: %v", err)
	}
}
func TestReworkLateOldGrantPreservesNewReservation(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) { a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nprintf once >> executions\n" })
	dispatchFor(h, "same", "old-assignment", a, digest)
	h.clk.Advance(31 * time.Second)
	dispatchFor(h, "same", "new-assignment", a, digest)
	h.r.HandleLeaseGrant(grantFor(h, "same", 1, "old-assignment", "old-lease", 0))
	h.r.mu.Lock()
	res := h.r.reservations["same"]
	h.r.mu.Unlock()
	if res == nil || res.assignmentID != "new-assignment" {
		t.Fatal("late old grant deleted the new reservation")
	}
	inst := h.r.Instances()[0]
	if inst.Busy == nil || !*inst.Busy || inst.AssignmentID != "new-assignment" {
		t.Fatalf("late old grant released new slot: %+v", inst)
	}
	good := grantFor(h, "same", 1, "new-assignment", "new-lease", 0)
	h.r.HandleLeaseGrant(good)
	h.r.HandleLeaseGrant(good)
	h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "same" && r.LeaseID == "new-lease" && r.Phase == protocol.TaskPhaseSucceeded
	}, 2*time.Second)
	data, err := os.ReadFile(filepath.Join(h.workRoot, hexNameForTest("same"), "1", "executions"))
	if err != nil || string(data) != "once" {
		t.Fatalf("new assignment must execute exactly once: %q %v", data, err)
	}
}
func TestReworkTerminalUnreapedRestartProtectsUntilPhysicalDeath(t *testing.T) {
	for _, phase := range []string{"running", "cancelled", "failed"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t)
			cmd, err := startSleepGroup(t, 30)
			if err != nil {
				t.Fatal(err)
			}
			reaped := false
			t.Cleanup(func() {
				if !reaped {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					_ = cmd.Wait()
				}
			})
			rec, err := h.results.Begin(resultstore.Record{TaskID: "historical", Node: "node-a", Attempt: 3, Capability: "echo", Digest: "sha256:aa", TTL: 1, Phase: "running", Slot: intPtr(1), LeaseID: "historical-lease", CreatedAt: h.clk.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if err := rec.SetPGID(cmd.Process.Pid); err != nil {
				t.Fatal(err)
			}
			rec.SetReaped(false)
			if err := rec.Finish(phase, nil, h.clk.Now()); err != nil {
				t.Fatal(err)
			}
			// A new Store and Runner: no in-process Attempt or Drain fixture.
			opts := h.r.opts
			opts.Results = resultstore.New(h.results.Root, h.workRoot)
			r, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			r.SetSender(h.sender)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				r.Close(ctx)
			})
			if err := r.Recover(); err != nil {
				t.Fatal(err)
			}
			inst := r.Instances()[1]
			if inst.Busy == nil || !*inst.Busy || inst.TaskID != "historical" || inst.LeaseID != "historical-lease" {
				t.Fatalf("%s unreaped restart lost live PGID occupancy: %+v", phase, inst)
			}
			h.clk.Advance(2 * time.Second)
			removed, errs := opts.Results.Collect(h.clk.Now(), r.ActiveTasks())
			if len(removed) != 0 || len(errs) != 0 {
				t.Fatalf("GC touched live historical attempt: removed=%v errs=%v", removed, errs)
			}
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			reaped = true
			h.waitUntil(t, "restarted runner physically observes reap", func() bool { return !r.ActiveTasks()["historical"] }, 2*time.Second)
			ack := h.waitFrame(t, func(f any) bool {
				c, ok := f.(*protocol.CancelAckFrame)
				return ok && c.TaskID == "historical" && c.ProcessGroupReaped
			}, time.Second).(*protocol.CancelAckFrame)
			if ack.Attempt != 3 || ack.LeaseID != "historical-lease" || ack.Slot == nil || *ack.Slot != 1 {
				t.Fatalf("reap ACK lost historical binding: %+v", ack)
			}
			if findResult(h.sender.snapshot(), "historical", 3) != nil {
				t.Fatal("recovery restored old result authority")
			}
			removed, errs = opts.Results.Collect(time.Now().Add(2*time.Second), r.ActiveTasks())
			if len(removed) == 0 || len(errs) != 0 {
				t.Fatalf("reaped terminal record remains GC protected: %v %v", removed, errs)
			}
		})
	}
}
func TestReworkShrinkCountsOutOfRangeRunningAndReservations(t *testing.T) {
	for _, occupied := range []string{"running", "reservation", "recovered"} {
		t.Run(occupied, func(t *testing.T) {
			h := newHarness(t)
			a, digest := h.artifact(func(a *artifact.Artifact) { a.Spec.Concurrency.MaxPerNode = intPtr(2) })
			h.dispatch("first", 1, a, digest) // slot 0 stays reserved while slot 1 is assigned
			live, liveDigest := h.artifact(func(a *artifact.Artifact) {
				a.Spec.Concurrency.MaxPerNode = intPtr(2)
				a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nexec sleep 30\n"
			})
			h.dispatch("outside", 1, live, liveDigest)
			var childPID int
			var child *exec.Cmd
			if occupied == "running" {
				h.grant("outside", 1, "outside-lease", "outside-token")
				h.waitUntil(t, "slot1 physical process", func() bool {
					h.r.mu.Lock()
					att := h.r.running["outside"]
					h.r.mu.Unlock()
					if att == nil {
						return false
					}
					att.mu.Lock()
					defer att.mu.Unlock()
					return att.pgid > 0
				}, time.Second)
			} else if occupied == "recovered" {
				cmd, err := startSleepGroup(t, 30)
				if err != nil {
					t.Fatal(err)
				}
				childPID = cmd.Process.Pid
				child = cmd
				t.Cleanup(func() { _ = syscall.Kill(-childPID, syscall.SIGKILL); _ = cmd.Wait() })
				rec, err := h.results.Begin(resultstore.Record{TaskID: "outside", Attempt: 1, Capability: "echo", Phase: "failed", Slot: intPtr(1), LeaseID: "outside-lease", TTL: 0})
				if err != nil {
					t.Fatal(err)
				}
				if err := rec.SetPGID(childPID); err != nil {
					t.Fatal(err)
				}
				// Drop the pregrant fixture; restart with only physical metadata.
				h.r.mu.Lock()
				delete(h.r.reservations, "outside")
				h.r.releaseSlotLocked(1, "echo")
				h.r.mu.Unlock()
				if err := h.r.Recover(); err != nil {
					t.Fatal(err)
				}
			}
			h.grant("first", 1, "first-lease", "first-token")
			h.waitUntil(t, "slot0 ends and is idle", func() bool { return !h.r.ActiveTasks()["first"] }, 2*time.Second)
			h.mu.Lock()
			h.limits = protocol.Limits{Total: 1, PerCapability: map[string]int{"echo": 2}}
			h.mu.Unlock()
			other, otherDigest := h.artifact(func(a *artifact.Artifact) { a.Metadata.Name = "other" })
			h.dispatch("blocked", 1, other, otherDigest)
			rej := findReject(h.sender.snapshot(), "blocked")
			if rej == nil || rej.Reason != protocol.ReasonSlotUnavailable || hasAccept(h.sender.snapshot(), "blocked") {
				t.Fatalf("shrink ignored out-of-range %s occupancy: rejection=%+v", occupied, rej)
			}
			if findResult(h.sender.snapshot(), "blocked", 1) != nil {
				t.Fatal("capacity reject counted an attempt")
			}
			if occupied == "running" {
				h.cancel("outside", 1, "outside-lease")
			} else if occupied == "reservation" {
				h.clk.Advance(31 * time.Second)
			} else {
				_ = syscall.Kill(-childPID, syscall.SIGKILL)
				_ = child.Wait()
			}
			if occupied != "reservation" {
				h.waitUntil(t, "outside physically reaped", func() bool { return !h.r.ActiveTasks()["outside"] }, 2*time.Second)
			}
			h.dispatch("after-reap", 1, other, otherDigest)
			if !hasAccept(h.sender.snapshot(), "after-reap") {
				t.Fatal("true reap/expired pregrant did not restore capacity")
			}
		})
	}
}
