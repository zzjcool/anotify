package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/clock"
	"github.com/zzjcool/anotify/cmd/anode/internal/execenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/cmd/anode/internal/upload"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// The clock's wall time can pass without delivering a timer: expiry must be
// checked by the sender, not inferred from waitResultAck having run already.
type rework2WallClock struct {
	*clock.Fake
	mu     sync.Mutex
	offset time.Duration
}

func (c *rework2WallClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Fake.Now().Add(c.offset)
}

func (c *rework2WallClock) elapse(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// Observe actual registration/Stop events, rather than assuming a pending
// result (or an accept frame) means the lease goroutine has installed its timer.
type rework2ObservedTimer struct {
	clock.Timer
	due      time.Time
	duration time.Duration
	stopped  bool
	owner    *rework2TimerClock
}

type rework2TimerClock struct {
	*clock.Fake
	mu      sync.Mutex
	timers  []*rework2ObservedTimer
	stops   map[time.Duration]int
	changed chan struct{}
	// Protected by mu; installed before the triggering Advance.
	beforeRegister func(time.Time)
}

func newRework2TimerClock(f *clock.Fake) *rework2TimerClock {
	return &rework2TimerClock{Fake: f, stops: make(map[time.Duration]int), changed: make(chan struct{}, 1)}
}

func (c *rework2TimerClock) notify() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *rework2TimerClock) NewTimer(d time.Duration) clock.Timer {
	return c.observe(c.Fake.NewTimer(d), c.Fake.Now().Add(d), d)
}

func (c *rework2TimerClock) NewTimerAt(target time.Time) clock.Timer {
	d := target.Sub(c.Fake.Now())
	c.mu.Lock()
	before := c.beforeRegister
	c.mu.Unlock()
	if before != nil {
		before(target)
	}
	return c.observe(c.Fake.NewTimerAt(target), target, d)
}

func (c *rework2TimerClock) observe(base clock.Timer, due time.Time, d time.Duration) clock.Timer {
	timer := &rework2ObservedTimer{Timer: base, due: due, duration: d, owner: c}
	c.mu.Lock()
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	c.notify()
	return timer
}

func (t *rework2ObservedTimer) Stop() bool {
	stopped := t.Timer.Stop()
	t.owner.mu.Lock()
	t.stopped = true
	t.owner.stops[t.duration]++
	t.owner.mu.Unlock()
	t.owner.notify()
	return stopped
}

func (c *rework2TimerClock) futureAt(due time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, timer := range c.timers {
		if !timer.stopped && timer.due.Equal(due) && due.After(c.Fake.Now()) {
			n++
		}
	}
	return n
}

func (c *rework2TimerClock) registeredAt(due time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, timer := range c.timers {
		if timer.due.Equal(due) {
			n++
		}
	}
	return n
}

// Capture completion, not just a frame added to the underlying sender snapshot.
// Initial pending results must have finished their guarded sends before replay.
type rework2CompletedSender struct {
	Sender
	mu     sync.Mutex
	leases map[string]string
}

func (s *rework2CompletedSender) SendGuarded(ctx context.Context, frame any, guard func() bool) (bool, error) {
	sent, err := s.Sender.(GuardedSender).SendGuarded(ctx, frame, guard)
	if f, ok := frame.(*protocol.TaskResultFrame); ok && sent && err == nil {
		s.mu.Lock()
		s.leases[resultKey(f.TaskID, f.Attempt)] = f.LeaseID
		s.mu.Unlock()
	}
	return sent, err
}

func (s *rework2CompletedSender) completed(att *Attempt) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leases[resultKey(att.taskID, att.attempt)] == att.leaseID
}

func (c *rework2TimerClock) stopCount(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stops[d]
}

func (c *rework2TimerClock) wait(t *testing.T, desc string, ready func() bool) {
	t.Helper()
	limit := time.NewTimer(time.Second)
	defer limit.Stop()
	for !ready() {
		select {
		case <-c.changed:
		case <-limit.C:
			t.Fatalf("timer registration synchronization timed out: %s", desc)
		}
	}
}

func rework2Pending(t *testing.T, count int) (*harness, []*Attempt) {
	t.Helper()
	h := newHarness(t)
	observed := newRework2TimerClock(h.clk)
	h.r.clk = observed // Before any execution/lease goroutine is started.
	h.sender.onResult = nil
	completed := &rework2CompletedSender{Sender: h.sender, leases: make(map[string]string)}
	h.r.SetSender(completed)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Concurrency.MaxPerNode = intPtr(2)
	})
	var attempts []*Attempt
	for _, task := range []string{"a-first", "b-second"}[:count] {
		h.dispatch(task, 1, a, digest)
		h.grant(task, 1, "lease-"+task, "token-"+task)
		h.r.mu.Lock()
		attempts = append(attempts, h.r.running[task])
		h.r.mu.Unlock()
		observed.wait(t, "initial lease timer", func() bool {
			return observed.futureAt(h.clk.Now().Add(20*time.Second)) == len(attempts)
		})
	}
	h.waitUntil(t, "real executions awaiting result ACK", func() bool {
		frames := h.sender.snapshot()
		if h.r.pendingResultCount() != count {
			return false
		}
		for _, att := range attempts {
			result := findResult(frames, att.taskID, att.attempt)
			if result == nil || result.Phase != protocol.TaskPhaseSucceeded || result.LeaseID != att.leaseID || !completed.completed(att) {
				return false
			}
		}
		return true
	}, 2*time.Second)
	return h, attempts
}

type rework2BlockedReplay struct {
	base    Sender
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *rework2BlockedReplay) Send(frame any) error {
	if f, ok := frame.(*protocol.TaskResultFrame); ok && f.TaskID == "a-first" {
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	}
	return s.base.Send(frame)
}

func (s *rework2BlockedReplay) SendGuarded(ctx context.Context, frame any, guard func() bool) (bool, error) {
	if f, ok := frame.(*protocol.TaskResultFrame); ok && f.TaskID == "a-first" {
		s.once.Do(func() { close(s.entered); <-s.release })
	}
	return s.base.(GuardedSender).SendGuarded(ctx, frame, guard)
}

// Hold absolute registration sampled at t=31 until t=50 or t=61. The former
// must expire at t=60, and the latter must wake immediately without Advance(0).
// This checks cleanup drift, not a previously proven result authorization leak.
func TestParentPositiveLeaseTimerRegistrationCrossingAdvance(t *testing.T) {
	for _, registeredAt := range []time.Duration{50 * time.Second, 61 * time.Second} {
		t.Run(fmt.Sprintf("register_at_%ds", registeredAt/time.Second), func(t *testing.T) {
			h, attempts := rework2Pending(t, 2)
			observed := h.r.clk.(*rework2TimerClock)
			start := h.clk.Now()
			target := start.Add(51 * time.Second)
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			observed.mu.Lock()
			observed.beforeRegister = func(at time.Time) {
				if at.Equal(target) && h.clk.Now().Equal(start.Add(31*time.Second)) {
					entered <- struct{}{}
					<-release
				}
			}
			observed.mu.Unlock()
			h.clk.Advance(31 * time.Second)
			for range 2 {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("lease loops did not reach absolute registration barrier")
				}
			}
			h.r.HandleLeaseAck(&protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: "a-first", Attempt: 1, LeaseID: "lease-a-first"})
			h.clk.Advance(registeredAt - 31*time.Second)
			once.Do(func() { close(release) })
			observed.wait(t, "both absolute targets registered", func() bool {
				return observed.registeredAt(target) >= 2
			})
			if registeredAt < 60*time.Second {
				if !attempts[1].authorized() || h.r.pendingResultCount() != 2 {
					t.Fatal("future absolute registration prematurely revoked the second lease")
				}
				h.clk.Advance(60*time.Second - registeredAt)
			}
			// No additional Advance/Advance(0): timer registration itself wakes
			// the past target, or the single Advance reaches the true deadline.
			h.waitUntil(t, "second expired pending reclaimed at real deadline", func() bool {
				h.r.mu.Lock()
				defer h.r.mu.Unlock()
				return h.r.pending[resultKey("b-second", 1)] == nil
			}, time.Second)
			attempts[0].mu.Lock()
			firstDeadline := attempts[0].deadline
			attempts[0].mu.Unlock()
			attempts[1].mu.Lock()
			secondDeadline := attempts[1].deadline
			attempts[1].mu.Unlock()
			if !firstDeadline.Equal(start.Add(91*time.Second)) || !secondDeadline.Equal(start.Add(60*time.Second)) || attempts[1].authorized() || !attempts[0].authorized() {
				t.Fatal("registration lost original ACK/deadline authorization binding")
			}
			before := len(h.sender.snapshot())
			h.r.SessionUp()
			frames := h.sender.snapshot()[before:]
			if findResult(frames, "b-second", 1) != nil || findResult(frames, "a-first", 1) == nil {
				t.Fatal("expired second replayed old success or valid ACK-backed first did not replay")
			}
			h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "a-first", Attempt: 1, Accepted: true})
			waitClosed(t, attempts[0].done, "real matching result ACK did not settle first")
			if h.r.pendingResultCount() != 0 || attempts[0].authorized() {
				t.Fatal("matching result ACK did not clean up valid first")
			}
		})
	}
}

func TestRework2ReplayRechecksSecondPendingAfterBlockedFirst(t *testing.T) {
	for _, revoke := range []string{"deadline", "cancel"} {
		t.Run(revoke, func(t *testing.T) {
			h, attempts := rework2Pending(t, 2)
			if revoke == "deadline" {
				observed := h.r.clk.(*rework2TimerClock)
				target := h.clk.Now().Add(51 * time.Second)
				h.clk.Advance(31 * time.Second)
				observed.wait(t, "both t31 renewal timers registered", func() bool {
					return observed.futureAt(target) == 2
				})
				h.r.HandleLeaseAck(&protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: "a-first", Attempt: 1, LeaseID: "lease-a-first"})
				observed.wait(t, "first ACK stopped and rebuilt its timer", func() bool {
					return observed.stopCount(20*time.Second) >= 1 && observed.futureAt(target) == 2
				})
			}
			s := &rework2BlockedReplay{base: h.sender, entered: make(chan struct{}), release: make(chan struct{})}
			h.r.SetSender(s)
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(s.release) }) })
			before := len(h.sender.snapshot())
			done := make(chan struct{})
			go func() { defer close(done); h.r.SessionUp() }()
			waitClosed(t, s.entered, "first replay never entered sender")
			if revoke == "deadline" {
				h.clk.Advance(30 * time.Second)
			} else {
				h.cancel("b-second", 1, "lease-b-second")
			}
			h.waitUntil(t, "second pending revoked during first send", func() bool {
				h.r.mu.Lock()
				defer h.r.mu.Unlock()
				return h.r.pending[resultKey("b-second", 1)] == nil
			}, time.Second)
			if attempts[1].authorized() {
				t.Fatal("second lease still authorized")
			}
			// Exercise a synchronous receiver ACK: no runner/attempt mutex may
			// be held across Send, including the first blocked replay.
			h.sender.mu.Lock()
			h.sender.onResult = func(f *protocol.TaskResultFrame) {
				h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: f.TaskID, Attempt: f.Attempt, Accepted: true})
			}
			h.sender.mu.Unlock()
			release.Do(func() { close(s.release) })
			waitClosed(t, done, "replay/synchronous ACK deadlocked")
			frames := h.sender.snapshot()[before:]
			if findResult(frames, "b-second", 1) != nil {
				t.Error("replay sent deleted second pending from stale snapshot")
			}
			if findResult(frames, "a-first", 1) == nil || h.r.pendingResultCount() != 0 {
				t.Error("valid first result did not replay and clean up with matching ACK")
			}
		})
	}
}

func TestRework2ReplayDeadlineWithoutTimerCleanup(t *testing.T) {
	h := newHarness(t)
	wall := &rework2WallClock{Fake: h.clk}
	h.r.clk = wall // Set before any execution goroutine is created.
	h.sender.onResult = nil
	a, digest := h.artifact(nil)
	h.dispatch("expired", 1, a, digest)
	h.grant("expired", 1, "lease-expired", "token-expired")
	h.waitUntil(t, "pending result", func() bool { return h.r.pendingResultCount() == 1 }, time.Second)
	wall.elapse(61 * time.Second)
	if h.r.pendingResultCount() != 1 {
		t.Fatal("test needs an expired pending not yet removed by timer cleanup")
	}
	before := len(h.sender.snapshot())
	h.r.SessionUp()
	if findResult(h.sender.snapshot()[before:], "expired", 1) != nil {
		t.Error("replay sent result after deadline before timer cleanup")
	}
	if h.r.pendingResultCount() != 1 || !h.r.ActiveTasks()["expired"] {
		t.Fatal("stale snapshot rejection performed lifecycle/physical cleanup before timer")
	}
}

// Model hub.Client.Send: caller enters Send first, then waits for an unrelated
// writer's mutex, and finally writes bytes. Runner-only validation cannot see
// this private queue. This is deliberately different from blocking the *first*
// replay, which a per-frame runner recheck can fix.
type rework2WriteQueue struct {
	base    Sender
	writeMu sync.Mutex
	queued  chan struct{}
	once    sync.Once
}

func (s *rework2WriteQueue) Send(frame any) error {
	if _, ok := frame.(*protocol.TaskResultFrame); ok {
		s.once.Do(func() { close(s.queued) })
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.base.Send(frame)
}

func (s *rework2WriteQueue) SendGuarded(ctx context.Context, frame any, guard func() bool) (bool, error) {
	if _, ok := frame.(*protocol.TaskResultFrame); ok {
		s.once.Do(func() { close(s.queued) })
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.base.(GuardedSender).SendGuarded(ctx, frame, guard)
}

func TestRework2ReplayDeadlineWhileWaitingInsideSenderWriteQueue(t *testing.T) {
	h := newHarness(t)
	wall := &rework2WallClock{Fake: h.clk}
	h.r.clk = wall
	h.sender.onResult = nil
	a, digest := h.artifact(nil)
	h.dispatch("queued", 1, a, digest)
	h.grant("queued", 1, "lease-queued", "token-queued")
	h.waitUntil(t, "pending result", func() bool { return h.r.pendingResultCount() == 1 }, time.Second)
	s := &rework2WriteQueue{base: h.sender, queued: make(chan struct{})}
	s.writeMu.Lock() // An unrelated config/hello writer owns the real send queue.
	h.r.SetSender(s)
	var release sync.Once
	t.Cleanup(func() { release.Do(s.writeMu.Unlock) })
	before := len(h.sender.snapshot())
	done := make(chan struct{})
	go func() { defer close(done); h.r.SessionUp() }()
	waitClosed(t, s.queued, "replay never reached sender write queue")
	wall.elapse(61 * time.Second)
	release.Do(s.writeMu.Unlock)
	waitClosed(t, done, "queued replay did not finish")
	if findResult(h.sender.snapshot()[before:], "queued", 1) != nil {
		t.Error("result crossed actual send boundary after expiring inside sender write queue")
	}
}

func TestRework2LateOldGrantCannotCreateResultACKAlias(t *testing.T) {
	h := newHarness(t)
	h.sender.onResult = nil // Capture any old failure; deliberately delay its ACK.
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nprintf once >> executions\n"
	})
	dispatchFor(h, "same", "old-assignment", a, digest)
	h.clk.Advance(31 * time.Second)
	dispatchFor(h, "same", "new-assignment", a, digest)
	h.r.HandleLeaseGrant(grantFor(h, "same", 1, "old-assignment", "old-lease", 0))
	old := findResult(h.sender.snapshot(), "same", 1)
	if old != nil || h.r.pendingResultCount() != 0 {
		t.Error("unmatched old grant created a result/ACK sending domain aliasing new execution")
	}
	h.r.mu.Lock()
	res := h.r.reservations["same"]
	h.r.mu.Unlock()
	if res == nil || res.assignmentID != "new-assignment" {
		t.Fatal("late old grant removed new reservation")
	}
	if old == nil {
		// Without a sent old result there can be no associated old ACK. An
		// unsolicited ACK before the unique legal result must do nothing.
		h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "same", Attempt: 1, Accepted: false})
		if h.r.Instances()[0].AssignmentID != "new-assignment" {
			t.Fatal("unassociated ACK released the new reservation")
		}
	}
	h.r.HandleLeaseGrant(grantFor(h, "same", 1, "new-assignment", "new-lease", 0))
	h.waitUntil(t, "new legal success pending ACK", func() bool {
		f := findResult(h.sender.snapshot(), "same", 1)
		for _, frame := range h.sender.snapshot() {
			if r, ok := frame.(*protocol.TaskResultFrame); ok && r.TaskID == "same" && r.LeaseID == "new-lease" {
				f = r
			}
		}
		return f != nil && f.LeaseID == "new-lease" && f.Phase == protocol.TaskPhaseSucceeded
	}, 2*time.Second)
	h.r.mu.Lock()
	pending := h.r.pending[resultKey("same", 1)]
	current := h.r.running["same"]
	h.r.mu.Unlock()
	if pending == nil || pending.frame.LeaseID != "new-lease" || current == nil || !current.authorized() {
		t.Fatal("new legal result lost its unique current execution binding before ACK")
	}
	// A duplicate grant is idempotent; a conflicting lease of the same
	// task/attempt cannot replace the verified execution or its pending result.
	h.r.HandleLeaseGrant(grantFor(h, "same", 1, "new-assignment", "new-lease", 0))
	h.r.HandleLeaseGrant(grantFor(h, "same", 1, "new-assignment", "conflicting-lease", 0))
	h.r.mu.Lock()
	unchanged := h.r.pending[resultKey("same", 1)] == pending && h.r.running["same"] == current
	h.r.mu.Unlock()
	if !unchanged {
		t.Fatal("duplicate/conflicting grant replaced current execution result binding")
	}
	if old != nil {
		// Real RED counterexample: an ACK obtainable from the earlier send
		// is delayed until the new legal result has replaced the same key.
		h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: old.TaskID, Attempt: old.Attempt, Accepted: false})
		if !h.r.ActiveTasks()["same"] || h.r.pendingResultCount() != 1 {
			t.Error("late old failure ACK revoked new authorization/released new slot")
		}
	}
	// With the old send removed, ACKs of this key *after* a legal send are
	// indistinguishable on frozen wire. Do not claim to distinguish them;
	// the only valid ACK domain is the unique current execution's result.
	h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "same", Attempt: 1, Accepted: true})
	if h.r.ActiveTasks()["same"] || h.r.pendingResultCount() != 0 {
		t.Fatal("matching legal result ACK did not release the reaped new slot")
	}
}

// A Sender without the result-only contract must fail closed. It must not even
// call Send, and it must retain the immutable pending binding for a legal hub.
type rework2LegacySender struct {
	base    Sender
	results int
}

func (s *rework2LegacySender) Send(frame any) error {
	if _, ok := frame.(*protocol.TaskResultFrame); ok {
		s.results++
	}
	return s.base.Send(frame)
}
func TestRework2ResultRequiresGuardedSender(t *testing.T) {
	h, _ := rework2Pending(t, 1)
	legacy := &rework2LegacySender{base: h.sender}
	h.r.SetSender(legacy)
	h.r.mu.Lock()
	p := h.r.pending[resultKey("a-first", 1)]
	h.r.mu.Unlock()
	before := len(h.sender.snapshot())
	h.r.SessionUp()
	if legacy.results != 0 || len(h.sender.snapshot()) != before {
		t.Fatal("result used unsafe legacy Send fallback")
	}
	h.r.mu.Lock()
	same := h.r.pending[resultKey("a-first", 1)] == p
	h.r.mu.Unlock()
	if !same {
		t.Fatal("missing GuardedSender destroyed pending binding")
	}
	h.r.SetSender(h.sender)
	h.r.SessionUp()
	if findResult(h.sender.snapshot()[before:], "a-first", 1) == nil {
		t.Fatal("legal sender could not replay retained result")
	}
	h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "a-first", Attempt: 1, Accepted: true})
}
func TestRework2DuplicateSnapshotKeepsImmutablePending(t *testing.T) {
	h, atts := rework2Pending(t, 1)
	h.r.mu.Lock()
	p := h.r.pending[resultKey("a-first", 1)]
	h.r.mu.Unlock()
	for range 2 {
		h.r.SessionUp()
		h.r.mu.Lock()
		same := h.r.pending[resultKey("a-first", 1)] == p
		h.r.mu.Unlock()
		if !same {
			t.Fatal("duplicate replay replaced pending binding")
		}
	}
	if !atts[0].authorized() {
		t.Fatal("duplicate snapshot revoked lease")
	}
	h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "a-first", Attempt: 1, Accepted: true})
	if h.r.pendingResultCount() != 0 {
		t.Fatal("legal ACK did not finish immutable binding")
	}
}

// LookupEmit must reject the state after stop(timeout) unlocks a.mu but before
// dropToken acquires r.mu. Model those exact fields without scheduling a process
// or relying on asynchronous token deletion; the finalization lease stays live.
func TestRework2LookupEmitChecksProcessWindowBeforeTokenRemoval(t *testing.T) {
	for _, state := range []struct {
		name        string
		emitOpen    bool
		terminating bool
		leaseOK     bool
		expired     bool
		wantEmit    bool
		wantLease   bool
	}{
		{name: "active", emitOpen: true, leaseOK: true, wantEmit: true, wantLease: true},
		{name: "timeout", terminating: true, leaseOK: true, wantLease: true},
		{name: "process_finished", leaseOK: true, wantLease: true},
		{name: "terminating_with_open_flag", emitOpen: true, terminating: true, leaseOK: true, wantLease: true},
		{name: "cancel", terminating: true},
		{name: "expiry_before_timer_cleanup", emitOpen: true, leaseOK: true, expired: true},
	} {
		t.Run(state.name, func(t *testing.T) {
			h := newHarness(t)
			att := newAttempt(h.r, &reservation{
				taskID: "emit-window", assignmentID: "assignment-emit", namespace: "default", slot: 0,
				art: &artifact.Artifact{Spec: artifact.Spec{Next: []string{"fixer"}}},
			}, &protocol.LeaseGrantFrame{Attempt: 1, LeaseID: "lease-emit", AttemptToken: "token-emit"})
			t.Cleanup(att.cancelUpload)
			att.mu.Lock()
			att.emitOpen, att.terminating, att.leaseOK = state.emitOpen, state.terminating, state.leaseOK
			if state.name == "timeout" {
				att.termReason = protocol.ReasonTimeout
				att.grace = h.r.opts.TermGrace
			}
			if state.expired {
				att.deadline = h.clk.Now() // Equality is already expired.
			}
			att.mu.Unlock()
			h.r.mu.Lock()
			h.r.tokens[att.token] = att // Deliberately retain the original token binding.
			h.r.mu.Unlock()

			if got := att.authorized(); got != state.wantLease {
				t.Fatalf("finalization lease authorization = %v, want %v", got, state.wantLease)
			}
			binding, ok := h.r.LookupEmit(att.token)
			if ok != state.wantEmit {
				t.Errorf("LookupEmit with token still mapped: allowed = %v, want %v", ok, state.wantEmit)
			}
			if state.wantEmit && (binding.TaskID != att.taskID || binding.Attempt != att.attempt || binding.LeaseID != att.leaseID || binding.Token != att.token || len(binding.Next) != 1 || binding.Next[0] != "fixer" || binding.EmitEndpoint != h.r.opts.EmitEndpoint) {
				t.Fatalf("active emit lost original identity/target binding: %+v", binding)
			}
			if state.name == "timeout" {
				// A legal renewal ACK must still extend the lease after closing
				// emit. Real upload/failed-timeout/result ACK is covered below.
				deadline := h.clk.Now().Add(90 * time.Second)
				att.noteAck(deadline)
				att.mu.Lock()
				renewed := att.deadline.Equal(deadline) && att.leaseOK && !att.emitOpen && att.terminating
				att.mu.Unlock()
				if !renewed || !att.authorized() || att.uploadCtx.Err() != nil {
					t.Fatal("closing emit revoked or cancelled legal timeout finalization/renewal")
				}
				if _, ok := h.r.LookupEmit(att.token); ok {
					t.Error("legal timeout renewal reopened script emit")
				}
			}
			h.r.mu.Lock()
			stillMapped := h.r.tokens[att.token] == att
			h.r.mu.Unlock()
			if !stillMapped {
				t.Fatal("test relied on token deletion instead of checking process authorization")
			}
		})
	}
}

// Local timeout must end a real process, not the grant's upload/result lease.
func rework2TimeoutUpload(t *testing.T, files int) (*harness, *heldTransport, *Attempt) {
	t.Helper()
	h := newHarness(t)
	h.sender.onResult = nil
	tr := &heldTransport{requests: make(chan *heldUpload, 8)}
	srv := httptest.NewServer(tr)
	t.Cleanup(srv.Close)
	h.r.opts.Upload = &upload.Client{BaseURL: srv.URL, NodeToken: "node-token", HTTP: srv.Client(), Retries: 0}
	a, digest := h.artifact(func(a *artifact.Artifact) {
		script := "#!/bin/sh\n"
		for i := 0; i < files; i++ {
			script += "printf payload > \"$ANOTIFY_ARTIFACT_DIR/" + string(rune('0'+i)) + ".txt\"\n"
		}
		a.Spec.Run.Files["run.sh"] = script + "printf once >> executions\nsleep 30\n"
		a.Spec.Lifecycle.TimeoutSec = 2
	})
	h.dispatch("slow", 1, a, digest)
	h.grant("slow", 1, "lease-slow", "token-slow")
	h.r.mu.Lock()
	att := h.r.running["slow"]
	h.r.mu.Unlock()
	if att == nil {
		t.Fatal("legal grant did not create execution")
	}
	t.Cleanup(att.cancelUpload)
	h.waitUntil(t, "real script and timeout timer", func() bool {
		_, err := os.Stat(filepath.Join(execenv.SystemWorkDir(h.workRoot, "slow", 1), "executions"))
		return err == nil && h.clk.Pending() >= 2
	}, time.Second)
	h.clk.Advance(2 * time.Second)
	waitClosed(t, att.procDone, "timeout did not stop actual process")
	if !att.authorized() {
		t.Error("local timeout incorrectly revoked the lease before finalization")
	}
	if _, ok := h.r.LookupEmit("token-slow"); ok {
		t.Error("timed-out process retained emit authorization")
	}
	return h, tr, att
}

func TestRework2TimeoutUploadsRenewAcross20And60UntilResultACK(t *testing.T) {
	h, tr, att := rework2TimeoutUpload(t, 3)
	att.mu.Lock()
	finished := att.finishedAt
	att.mu.Unlock()
	for i := 0; i < 3; i++ {
		u := takeUpload(t, tr)
		n := renews(h)
		h.clk.Advance(20 * time.Second)
		h.waitUntil(t, "timeout finalization still renews", func() bool { h.clk.Advance(0); return renews(h) > n }, time.Second)
		h.r.HandleLeaseAck(&protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: "slow", Attempt: 1, LeaseID: "lease-slow"})
		h.clk.Advance(5 * time.Second)
		if u.req.Context().Err() != nil || !att.authorized() {
			t.Fatal("ACK-backed timeout upload lost authorization")
		}
		close(u.release)
	}
	h.waitUntil(t, "legal failed timeout result", func() bool { return findResult(h.sender.snapshot(), "slow", 1) != nil }, time.Second)
	result := findResult(h.sender.snapshot(), "slow", 1)
	if result.Phase != protocol.TaskPhaseFailed || result.Result.Error == nil || result.Result.Error.Reason != protocol.ReasonTimeout || len(result.Result.Artifacts) != 3 {
		t.Fatalf("must preserve legal failed/timeout and confirmed uploads: %+v", result)
	}
	if !result.FinishedAt.Equal(finished) || h.clk.Now().Sub(finished) != 75*time.Second {
		t.Fatal("timeout finishedAt includes transfer time")
	}
	if data, err := os.ReadFile(filepath.Join(att.workDir, "executions")); err != nil || string(data) != "once" {
		t.Fatalf("timeout must not reexecute: %q %v", data, err)
	}
	n := renews(h)
	h.clk.Advance(20 * time.Second)
	h.waitUntil(t, "timeout pending result still renews", func() bool { h.clk.Advance(0); return renews(h) > n }, time.Second)
	h.r.HandleLeaseAck(&protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: "slow", Attempt: 1, LeaseID: "lease-slow"})
	if !h.r.ActiveTasks()["slow"] || !att.authorized() {
		t.Fatal("timeout result released lease/slot before ACK")
	}
	h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "slow", Attempt: 1, Accepted: true})
	waitClosed(t, att.done, "failed timeout ACK did not settle")
	if att.authorized() || h.r.ActiveTasks()["slow"] {
		t.Fatal("timeout ACK did not revoke lease/release reaped slot")
	}
}

func TestRework2TimeoutFinalizationCancelOrExpiryFencesLateResult(t *testing.T) {
	for _, revoke := range []string{"cancel", "expiry"} {
		t.Run(revoke, func(t *testing.T) {
			h, tr, att := rework2TimeoutUpload(t, 2)
			u := takeUpload(t, tr)
			n := renews(h)
			h.clk.Advance(20 * time.Second)
			h.waitUntil(t, "timeout upload renew", func() bool { h.clk.Advance(0); return renews(h) > n }, time.Second)
			h.r.HandleLeaseAck(&protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: "slow", Attempt: 1, LeaseID: "lease-slow"})
			if revoke == "cancel" {
				h.cancel("slow", 1, "lease-slow")
				if att.authorized() {
					t.Error("cancel after process timeout did not synchronously revoke lease")
				}
			} else {
				h.clk.Advance(61 * time.Second)
			}
			h.waitUntil(t, "timeout finalization cancels real HTTP upload", func() bool {
				h.clk.Advance(0)
				select {
				case <-u.cancelled:
					return true
				default:
					return false
				}
			}, 2*time.Second)
			waitClosed(t, att.done, "revoked timeout finalization did not settle")
			if att.authorized() || findResult(h.sender.snapshot(), "slow", 1) != nil || h.r.pendingResultCount() != 0 {
				t.Fatal("revoked timeout exported a late result/pending or revived lease")
			}
			if len(tr.requests) != 0 {
				t.Fatal("revoked timeout started another artifact upload")
			}
			ack := h.waitFrame(t, func(f any) bool {
				a, ok := f.(*protocol.CancelAckFrame)
				return ok && a.TaskID == "slow" && a.LeaseID == "lease-slow" && a.ProcessGroupReaped
			}, time.Second).(*protocol.CancelAckFrame)
			if ack.Slot == nil || *ack.Slot != 0 || h.r.ActiveTasks()["slow"] {
				t.Fatal("timeout cancellation did not preserve physical binding/reap")
			}
		})
	}
}

func rework2AssertExpiredArchive(t *testing.T, h *harness, att *Attempt) {
	t.Helper()
	waitClosed(t, att.done, "expired process did not settle")
	if findResult(h.sender.snapshot(), att.taskID, att.attempt) != nil || att.authorized() {
		t.Fatal("expired lease produced an illegal result or retained authorization")
	}
	att.mu.Lock()
	phase, reason := att.phase, att.errCode
	att.mu.Unlock()
	if phase != protocol.TaskPhaseFailed || reason != protocol.ReasonLeaseExpired {
		t.Fatalf("local expiry must remain failed/lease_expired: %s/%s", phase, reason)
	}
	data, err := os.ReadFile(filepath.Join(h.results.Root, hexNameForTest(att.taskID), fmt.Sprint(att.attempt), resultstore.MetaFileName))
	if err != nil {
		t.Fatal(err)
	}
	var rec resultstore.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Phase != "failed" || !rec.Reaped || rec.FinishedAt.IsZero() || rec.Node != h.r.opts.NodeName || rec.TaskID != att.taskID || rec.Attempt != att.attempt || rec.LeaseID != att.leaseID || rec.Slot == nil || *rec.Slot != att.slot || groupAlive(rec.PGID) {
		t.Fatalf("expiry archive lost failed/real reap/full binding: %+v", rec)
	}
	h.r.mu.Lock()
	hist := h.r.history[resultKey(att.taskID, att.attempt)]
	h.r.mu.Unlock()
	if hist == nil || !hist.reaped || hist.leaseID != att.leaseID || hist.slot != att.slot {
		t.Fatal("expired lease lost historical physical reap binding")
	}
}

func TestRework2ExpiredHistoricalReapDoesNotTouchNewLeasePending(t *testing.T) {
	h := newHarness(t)
	h.sender.onResult = nil
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nprintf once > executions\nsleep 30\n"
	})
	h.dispatch("history", 1, a, digest)
	h.grant("history", 1, "old-lease", "old-token")
	h.r.mu.Lock()
	old := h.r.running["history"]
	h.r.mu.Unlock()
	h.waitUntil(t, "old real process started", func() bool {
		_, err := os.Stat(filepath.Join(execenv.SystemWorkDir(h.workRoot, "history", 1), "executions"))
		return err == nil
	}, time.Second)
	h.clk.Advance(61 * time.Second)
	h.waitUntil(t, "old expiry physically reaped", func() bool {
		h.clk.Advance(0)
		select {
		case <-old.done:
			return true
		default:
			return false
		}
	}, time.Second)
	rework2AssertExpiredArchive(t, h, old)
	fast, fastDigest := h.artifact(nil)
	h.dispatch("history", 2, fast, fastDigest)
	h.grant("history", 2, "new-lease", "new-token")
	h.waitUntil(t, "new real execution result pending", func() bool {
		return findResult(h.sender.snapshot(), "history", 2) != nil && h.r.pendingResultCount() == 1
	}, time.Second)
	h.r.mu.Lock()
	current := h.r.running["history"]
	p := h.r.pending[resultKey("history", 2)]
	h.r.mu.Unlock()
	before := len(h.sender.snapshot())
	// Node identity is the current hub/session identity, not a selectable body
	// field. The historical receipt keeps the full task/attempt/lease/slot binding.
	h.cancel("history", 1, "old-lease")
	h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "history", Attempt: 1, Accepted: false})
	h.r.HandleLeaseAck(&protocol.LeaseAckFrame{Type: protocol.FrameLeaseAck, TaskID: "history", Attempt: 1, LeaseID: "old-lease"})
	h.r.SessionUp()
	h.r.mu.Lock()
	same := h.r.running["history"] == current && h.r.pending[resultKey("history", 2)] == p
	h.r.mu.Unlock()
	if !same || !current.authorized() || !h.r.ActiveTasks()["history"] {
		t.Fatal("historical reap/ACK revoked new lease/pending")
	}
	receipt := false
	for _, f := range h.sender.snapshot()[before:] {
		if ack, ok := f.(*protocol.CancelAckFrame); ok && ack.TaskID == "history" && ack.Attempt == 1 && ack.LeaseID == "old-lease" && ack.Slot != nil && *ack.Slot == old.slot && ack.ProcessGroupReaped {
			receipt = true
		}
		if result, ok := f.(*protocol.TaskResultFrame); ok && result.Attempt == 1 {
			t.Fatal("history restored old result sending authority")
		}
	}
	if !receipt {
		t.Fatal("historical full-binding reap ACK was not delivered")
	}
	h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: "history", Attempt: 2, Accepted: true})
	waitClosed(t, current.done, "new legal ACK did not finish its result")
	if h.r.ActiveTasks()["history"] || h.r.pendingResultCount() != 0 {
		t.Fatal("new result ACK did not release reaped slot")
	}
}

// A write that was already admitted may finish after normal ACK cleanup has
// installed another attempt in the same physical slot. Its network failure is
// not authority to clear that newer slot or pending binding.
type lifecycleFailedResultSender struct {
	Sender
	entered, release chan struct{}
	err              error
}

func (s *lifecycleFailedResultSender) SendGuarded(ctx context.Context, frame any, guard func() bool) (bool, error) {
	if ctx.Err() != nil || !guard() {
		return false, nil
	}
	close(s.entered)
	select {
	case <-s.release:
		return false, s.err
	case <-time.After(2 * time.Second):
		return false, errors.New("failed result write barrier timed out")
	}
}

func TestResultWriteFailureAfterACKKeepsReplacementSlot(t *testing.T) {
	h, attempts := rework2Pending(t, 1)
	old := attempts[0]
	h.r.mu.Lock()
	p, generation := h.r.pending[resultKey(old.taskID, old.attempt)], h.r.sendGeneration
	h.r.mu.Unlock()
	failure := errors.New("old admitted connection write failed")
	s := &lifecycleFailedResultSender{Sender: h.sender, entered: make(chan struct{}), release: make(chan struct{}), err: failure}
	var release sync.Once
	defer release.Do(func() { close(s.release) })
	h.r.SetSender(s)
	// SetSender changes the session generation; capture the actual new one.
	h.r.mu.Lock()
	generation = h.r.sendGeneration
	h.r.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- h.r.sendResultFrame(p, generation) }()
	waitClosed(t, s.entered, "old result did not pass admission")
	h.r.HandleResultAck(&protocol.ResultAckFrame{Type: protocol.FrameResultAck, TaskID: old.taskID, Attempt: old.attempt, Accepted: true})
	waitClosed(t, old.done, "trusted old ACK did not settle normal lifecycle")
	if old.uploadCtx.Err() != context.Canceled {
		t.Fatal("ACK did not immediately cancel upload")
	}
	h.r.SetSender(h.sender)
	a, digest := h.artifact(func(*artifact.Artifact) {})
	h.dispatch(old.taskID, 2, a, digest)
	h.grant(old.taskID, 2, "replacement-lease", "replacement-token")
	h.waitUntil(t, "replacement result pending ACK", func() bool {
		return findResult(h.sender.snapshot(), old.taskID, 2) != nil && h.r.pendingResultCount() == 1
	}, 2*time.Second)
	h.r.mu.Lock()
	current := h.r.running[old.taskID]
	pending := h.r.pending[resultKey(old.taskID, 2)]
	if current == nil || pending == nil {
		h.r.mu.Unlock()
		t.Fatal("replacement lost its binding")
	}
	slot := h.r.slots[current.slot]
	h.r.mu.Unlock()
	release.Do(func() { close(s.release) })
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("old write error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old failed write did not return")
	}
	h.r.mu.Lock()
	unchanged := h.r.running[old.taskID] == current && h.r.pending[resultKey(old.taskID, 2)] == pending && h.r.slots[current.slot] == slot
	h.r.mu.Unlock()
	if !unchanged || !current.authorized() {
		t.Fatal("old failed write released replacement slot/pending authorization")
	}
}
