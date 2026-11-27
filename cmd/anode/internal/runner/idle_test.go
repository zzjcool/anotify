package runner

import (
	"os"
	"path/filepath"
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

// waitExportFile 等导出目录里真的出现 rel —— 假时钟不能抢在进程写文件之前推过 idle 周期，
// 否则「停止后仍要导出」测的是竞态而不是行为。
func (h *harness) waitExportFile(t *testing.T, taskID, rel string) {
	t.Helper()
	h.waitUntil(t, "导出目录出现 "+rel, func() bool {
		h.r.mu.Lock()
		att := h.r.running[taskID]
		h.r.mu.Unlock()
		if att == nil || att.exportDir == "" {
			return false
		}
		_, err := os.Stat(filepath.Join(att.exportDir, rel))
		return err == nil
	}, 5*time.Second)
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

	// 先等 idle 定时器真的武装好（2s），再推进 —— 直接推进会在「armed 之前推完额度」的竞态里空转。
	h.waitTimerArmed(t, "无进展", 2*time.Second, 3*time.Second)
	res := h.waitFrameAdvancing(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 3*time.Second, 8*time.Second).(*protocol.TaskResultFrame)
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

func TestIdleTimeoutCountsArtifactGrowth(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nwhile true; do echo tick >> \"$ANOTIFY_ARTIFACT_DIR/agent.log\"; sleep 0.01; done\n"
		a.Spec.Lifecycle.IdleTimeoutSec = intPtr(2)
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	stopLease := keepLeaseAlive(h, "t1", "lease-1")
	defer stopLease()

	for i := 0; i < 6; i++ {
		h.clk.Advance(time.Second) // 每步都跨过 idle 窗口
		time.Sleep(60 * time.Millisecond)
	}
	if r := findResult(h.sender.snapshot(), "t1", 1); r != nil {
		t.Fatalf("导出目录还在长时不该判无进展：%+v", r)
	}
	h.cancel("t1", 1, "lease-1")
}

func TestIdleTimeoutDefaultIsDisabled(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 1200\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	stopLease := keepLeaseAlive(h, "t1", "lease-1")
	defer stopLease()

	for i := 0; i < 30; i++ {
		h.clk.Advance(20 * time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	if r := findResult(h.sender.snapshot(), "t1", 1); r != nil {
		t.Fatalf("缺省必须关闭无进展判据：%+v", r)
	}
	h.cancel("t1", 1, "lease-1")
}

func TestNoProgressKeepsGrantAndExportsArtifacts(t *testing.T) {
	h := newHarness(t)
	h.sender.onResult = func(*protocol.TaskResultFrame) {} // 不 ACK：观察停止后、确认前的状态
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\necho exported > \"$ANOTIFY_ARTIFACT_DIR/out.txt\"\nsleep 1200\n"
		a.Spec.Lifecycle.IdleTimeoutSec = intPtr(2)
		a.Spec.Lifecycle.TimeoutSec = 60
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")

	h.waitExportFile(t, "t1", "out.txt")
	h.waitTimerArmed(t, "无进展", 2*time.Second, 3*time.Second)
	// 额度 5s：产物落地会让第一轮 idle 判成「有进展」并重新武装，真正的停止在第二轮（≈4s）。
	res := h.waitFrameAdvancing(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second, 10*time.Second).(*protocol.TaskResultFrame)
	if res.Result.Error == nil || res.Result.Error.Reason != protocol.ReasonNoProgress {
		t.Fatalf("期望 no_progress，得到 %+v", res.Result.Error)
	}
	if len(res.Result.Artifacts) != 1 || res.Result.Artifacts[0].Path != "artifacts/out.txt" {
		t.Fatalf("停止后产物仍要导出，得到 %+v", res.Result.Artifacts)
	}
	h.r.mu.Lock()
	att := h.r.running["t1"]
	leaseOK := att != nil && att.leaseOK
	h.r.mu.Unlock()
	if !leaseOK {
		t.Fatal("no_progress 不能撤销中枢 grant（leaseOK 必须仍为 true）")
	}
}
