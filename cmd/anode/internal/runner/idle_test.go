package runner

import (
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

func keepLeaseAlive(h *harness, taskID, leaseID string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.r.HandleLeaseAck(&protocol.LeaseAckFrame{
				Type: protocol.FrameLeaseAck, TaskID: taskID, Attempt: 1, LeaseID: leaseID,
				ExpiresAt: h.clk.Now().Add(protocol.LeaseTTL),
			})
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func TestIdleTimeoutEndsSilentTaskEarly(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 1200\n"
		a.Spec.Lifecycle.IdleTimeoutSec = intPtr(2)
		a.Spec.Lifecycle.TimeoutSec = 60 // 整任务上限远大于 idle：早退必须由 idle 触发
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")

	res := h.waitFrameAdvancing(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, time.Second, 8*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseFailed || res.Result.Error == nil ||
		res.Result.Error.Reason != protocol.ReasonNoProgress {
		t.Fatalf("无进展必须是 failed/no_progress：%+v", res)
	}
}

func TestIdleTimeoutResetsOnLogBytes(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nwhile true; do echo tick; sleep 0.01; done\n"
		a.Spec.Lifecycle.IdleTimeoutSec = intPtr(100)
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	stopLease := keepLeaseAlive(h, "t1", "lease-1")
	defer stopLease()

	for i := 0; i < 12; i++ {
		h.clk.Advance(10 * time.Second)
		time.Sleep(40 * time.Millisecond)
	}
	if r := findResult(h.sender.snapshot(), "t1", 1); r != nil {
		t.Fatalf("日志一直在出字节时不该判无进展：%+v", r)
	}
	if !h.r.ActiveTasks()["t1"] {
		t.Fatalf("日志一直在出字节时任务必须还在跑；状态：%s", h.describeStateSafe())
	}
	h.cancel("t1", 1, "lease-1")
}

func TestIdleTimeoutDisabledByZero(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 1200\n"
		a.Spec.Lifecycle.IdleTimeoutSec = intPtr(0) // 显式关闭：合法地长时间一声不响
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	stopLease := keepLeaseAlive(h, "t1", "lease-1")
	defer stopLease()

	for i := 0; i < 30; i++ {
		h.clk.Advance(20 * time.Second) // 每步都走过一次续租，避免租约判失联
		time.Sleep(2 * time.Millisecond)
	}
	if r := findResult(h.sender.snapshot(), "t1", 1); r != nil {
		t.Fatalf("显式 0 关闭时不该判无进展：%+v", r)
	}
	if !h.r.ActiveTasks()["t1"] {
		t.Fatalf("显式 0 关闭时任务必须还在跑；状态：%s", h.describeStateSafe())
	}
	h.cancel("t1", 1, "lease-1")
}
