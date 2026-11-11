package runner

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// V4-10：进程组消失是内核事实，SIGKILL 只保证死亡。僵尸仍挂在组里时
// kill(2) 依旧返回成功，确认必须用**真实时间**有界轮询，不能被注入时钟
// 推进「骗过」（V4-10 的 RED 就是 waitGone 的截止时间吃假时钟）。
func TestWaitGroupGoneConfirmsRealKernelReap(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动真实进程组: %v", err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	if !groupAlive(pgid) {
		t.Fatal("刚启动的进程组必须是活的")
	}

	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	// 已 SIGKILL、尚未被 wait 回收：僵尸仍在组里，内核仍判定该组存在。
	// 即便给足真实窗口也必须诚实返回 false，而不是按注入时钟「认为」回收了。
	if waitGroupGone(pgid, 200*time.Millisecond, 5*time.Millisecond, nil) {
		t.Fatal("尚未被内核回收的进程组不能报告为已回收")
	}

	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("wait 真实进程: %v", err)
		}
	}
	start := time.Now()
	if !waitGroupGone(pgid, reapConfirmWindow, reapConfirmInterval, nil) {
		t.Fatal("内核回收后必须在真实时间内确认进程组消失")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("真实确认应是毫秒级，实际 %s", elapsed)
	}
}

// V4-10 要求 3：回收路径自己关读端是**有意收尾**，不能记成日志复制失败，
// 更不能把成功任务翻成 failed。
func TestIntentionalPipeCloseIsNotCopyFailure(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		// leader 立刻成功退出，孙进程继续持有 stdout/stderr 管道：只有回收路径
		// 主动关读端才能让 io.Copy 结束（有意关闭，不是复制错误）。
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 30 &\necho hi\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	h.r.mu.Lock()
	att := h.r.running["t1"]
	h.r.mu.Unlock()

	res := h.waitFrameAdvancing(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1" && r.Attempt == 1
	}, time.Second, 10*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("主动关闭读端不能把成功任务翻成 failed：%+v", res)
	}
	att.mu.Lock()
	copyErr := att.copyErr
	att.mu.Unlock()
	if copyErr != nil {
		t.Fatalf("主动关闭读端被记成日志复制错误：%v", copyErr)
	}
	if !att.groupReapedNow() {
		t.Fatal("SIGKILL 之后必须在真实时间内确认进程组回收")
	}
	if _, stdout, _, found, err := h.results.Logs("t1", 1); err != nil || !found || string(stdout) != "hi\n" {
		t.Fatalf("日志内容不完整：found=%v stdout=%q err=%v", found, stdout, err)
	}
}

// V4-10 要求 2：终态仍未确认回收时保留槽位，但必须有界后台复核；一旦确认
// 内核回收，补发同 attempt/lease/slot 的 reaped=true cancel.ack 并释放槽位。
func TestUnconfirmedReapIsRecheckedAndAckedLater(t *testing.T) {
	h := newHarness(t)
	// 用一个真实进程组代表「内核还没回收的残留组」：复核只能看真实内核事实。
	holder := exec.Command("sleep", "30")
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := holder.Start(); err != nil {
		t.Fatalf("启动真实进程组: %v", err)
	}
	pgid := holder.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = holder.Wait()
	})

	att := &Attempt{
		r: h.r, taskID: "later", attempt: 1, leaseID: "lease-later",
		token: "token-later", slot: 0, pgid: pgid,
		art: &artifact.Artifact{Metadata: artifact.Metadata{Name: "echo"}},
	}
	h.r.mu.Lock()
	slot := h.r.ensureSlotLocked(0)
	slot.taskID = att.taskID
	slot.attempt = att.attempt
	slot.leaseID = att.leaseID
	slot.running = true
	h.r.running[att.taskID] = att
	h.r.tokens[att.token] = att
	h.r.mu.Unlock()

	h.r.finishAttempt(att) // 未确认回收：保留物理槽位（现有语义）
	if !h.r.ActiveTasks()[att.taskID] {
		t.Fatal("未确认回收不能释放物理槽位")
	}
	if ack := findCancelAck(h.sender.snapshot(), att.taskID, att.attempt); ack != nil && ack.ProcessGroupReaped {
		t.Fatal("进程组还活着时不能声称已回收")
	}

	// 内核真正回收进程组：后台复核必须自行发现并补发 reaped=true 的回执。
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	if err := holder.Wait(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("wait 真实进程: %v", err)
		}
	}
	ack := h.waitFrame(t, func(f any) bool {
		c, ok := f.(*protocol.CancelAckFrame)
		return ok && c.TaskID == att.taskID && c.Attempt == att.attempt &&
			c.LeaseID == att.leaseID && c.Slot != nil && *c.Slot == att.slot
	}, 10*time.Second).(*protocol.CancelAckFrame)
	if !ack.ProcessGroupReaped {
		t.Fatalf("复核确认回收后必须补发 reaped=true：%+v", ack)
	}
	if !att.groupReapedNow() {
		t.Fatal("复核确认后 attempt 的回收事实必须更新")
	}
	h.waitUntil(t, "复核确认后释放槽位", func() bool { return !h.r.ActiveTasks()[att.taskID] }, 2*time.Second)
	h.r.mu.Lock()
	hist := h.r.history[resultKey(att.taskID, att.attempt)]
	h.r.mu.Unlock()
	if hist == nil || !hist.reaped || hist.leaseID != att.leaseID || hist.slot != att.slot {
		t.Fatalf("复核确认后历史绑定必须是 reaped=true：%+v", hist)
	}
}

func findCancelAck(frames []any, taskID string, attempt int) *protocol.CancelAckFrame {
	for _, f := range frames {
		if ack, ok := f.(*protocol.CancelAckFrame); ok && ack.TaskID == taskID && ack.Attempt == attempt {
			return ack
		}
	}
	return nil
}
