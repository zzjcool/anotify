package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/execenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// A3（SLOT-HANDOFF）具名回归：presence/slot0/过期/reject 两项/旧 ACK 栅栏/重启恢复/缩预算。

func findReject(frames []any, taskID string) *protocol.TaskRejectFrame {
	for _, f := range frames {
		if r, ok := f.(*protocol.TaskRejectFrame); ok && r.TaskID == taskID {
			return r
		}
	}
	return nil
}

func findAcceptSlot(frames []any, taskID string) *int {
	for _, f := range frames {
		if a, ok := f.(*protocol.TaskAcceptFrame); ok && a.TaskID == taskID {
			return a.Slot
		}
	}
	return nil
}

func TestAcceptReturnsSlotZeroWithPresence(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil)
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	slot := findAcceptSlot(h.sender.snapshot(), "t1")
	if slot == nil || *slot != 0 {
		t.Fatalf("第一个空槽必须是 0 且有 presence：%v", slot)
	}
}

func TestNoFreeSlotRejectsSlotUnavailableWithoutAttempt(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	h.limits = protocol.Limits{Total: 1, PerCapability: map[string]int{"echo": 1}}
	h.mu.Unlock()
	a, digest := h.artifact(nil)
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")

	// 第二个任务没有空槽：正式 reject slot_unavailable（不计 attempt、不执行）。
	h.dispatch("t2", 1, a, digest)
	rej := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskRejectFrame)
		return ok && r.TaskID == "t2"
	}, 2*time.Second).(*protocol.TaskRejectFrame)
	if rej.Reason != protocol.ReasonSlotUnavailable {
		t.Fatalf("无槽必须 reject slot_unavailable，实际 %q", rej.Reason)
	}
	if hasAccept(h.sender.snapshot(), "t2") {
		t.Fatal("无槽不能 accept")
	}
	if r := findResult(h.sender.snapshot(), "t2", 1); r != nil {
		t.Fatalf("无槽不能计 attempt/产生 result：%+v", r)
	}
}

func TestExpiredReservationReleasesSlotAndLateGrantDoesNotExecute(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil)
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)

	// 派发窗口 30s 到期；晚到的 grant 不执行、不产生能别名新执行的结果。
	h.clk.Advance(31 * time.Second)
	h.grant("t1", 1, "lease-late", "token-late")
	h.r.mu.Lock()
	running, reserved := h.r.running["t1"], h.r.reservations["t1"]
	h.r.mu.Unlock()
	if running != nil || reserved != nil {
		t.Fatal("过期 grant 不能执行，且必须释放自己的 reservation")
	}
	if res := findResult(h.sender.snapshot(), "t1", 1); res != nil || h.r.pendingResultCount() != 0 {
		t.Fatalf("过期 grant 不能产生伪造 execution result：%+v", res)
	}
	if _, err := os.Stat(execenv.SystemWorkDir(h.workRoot, "t1", 1)); !os.IsNotExist(err) {
		t.Fatalf("过期 grant 不能创建执行目录：%v", err)
	}
	// 释放后槽位 0 可以被下一个任务使用。
	h.dispatch("t2", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t2") }, 2*time.Second)
	if slot := findAcceptSlot(h.sender.snapshot(), "t2"); slot == nil || *slot != 0 {
		t.Fatalf("过期 reservation 必须释放槽位 0：%v", slot)
	}
}

func TestPrunedReservationLateGrantIsIgnored(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(nil)
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.clk.Advance(31 * time.Second)
	// 另一个 dispatch 触发过期清理：t1 的 reservation 被释放。
	h.dispatch("t2", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t2") }, 2*time.Second)
	// 已被清理的 reservation 收到晚到 grant：不执行、不产生伪造结果。
	h.grant("t1", 1, "lease-late", "token-late")
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(execenv.SystemWorkDir(h.workRoot, "t1", 1)); !os.IsNotExist(err) {
		t.Fatalf("已清理 reservation 的 grant 不能执行：%v", err)
	}
}

func TestGrantSlotOrAssignmentMismatchDoesNotExecute(t *testing.T) {
	for name, mutate := range map[string]func(*protocol.LeaseGrantFrame){
		"slot 不符":         func(f *protocol.LeaseGrantFrame) { bad := 1; f.Slot = &bad },
		"assignmentId 不符": func(f *protocol.LeaseGrantFrame) { f.AssignmentID = "asg-other" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			a, digest := h.artifact(nil)
			h.dispatch("t1", 1, a, digest)
			h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
			slot := findAcceptSlot(h.sender.snapshot(), "t1")
			h.r.mu.Lock()
			reserved := h.r.reservations["t1"]
			h.r.mu.Unlock()
			grant := &protocol.LeaseGrantFrame{Type: protocol.FrameLeaseGrant, TaskID: "t1", Attempt: 1, Node: "node-a", LeaseID: "lease-1", AttemptToken: "token-1", ExpiresAt: h.clk.Now().Add(protocol.LeaseTTL), AssignmentID: "assign-t1", Slot: slot}
			mutate(grant)
			h.r.HandleLeaseGrant(grant)
			h.r.mu.Lock()
			preserved := h.r.reservations["t1"] == reserved && h.r.running["t1"] == nil
			h.r.mu.Unlock()
			if !preserved {
				t.Fatal("mismatched grant consumed the valid reservation or executed")
			}
			if result := findResult(h.sender.snapshot(), "t1", 1); result != nil || h.r.pendingResultCount() != 0 {
				t.Fatalf("mismatched grant must not create a result/ACK alias: %+v", result)
			}
			if _, err := os.Stat(filepath.Join(h.workRoot, hexNameForTest("t1"), "1")); !os.IsNotExist(err) {
				t.Fatalf("%s：不能执行（err=%v）", name, err)
			}
			// The original valid reservation/slot remains usable by its exact grant.
			h.grant("t1", 1, "legal-lease", "legal-token")
			result := h.waitFrame(t, func(f any) bool { r, ok := f.(*protocol.TaskResultFrame); return ok && r.TaskID == "t1" }, 3*time.Second).(*protocol.TaskResultFrame)
			if result.Phase != protocol.TaskPhaseSucceeded || result.LeaseID != "legal-lease" {
				t.Fatalf("preserved reservation could not execute its legal grant: %+v", result)
			}
		})
	}
}

func TestWrongLeaseCancelDoesNotKillRunningAttempt(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 0.5\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	h.waitUntil(t, "启动", func() bool {
		h.r.mu.Lock()
		defer h.r.mu.Unlock()
		return h.r.running["t1"] != nil
	}, 3*time.Second)

	h.cancel("t1", 1, "lease-wrong") // lease 不匹配
	ack := h.waitFrame(t, func(f any) bool {
		c, ok := f.(*protocol.CancelAckFrame)
		return ok && c.TaskID == "t1"
	}, 3*time.Second).(*protocol.CancelAckFrame)
	if ack.ProcessGroupReaped {
		t.Fatal("错误 lease 的取消不能声称已回收")
	}
	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("错误 lease 的取消不能停掉执行：%+v", res)
	}
}

func TestOldLeaseACKDoesNotExtendNewAttempt(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 30\n"
	})
	// attempt 1：跑完（正常退出）后，attempt 2 占用同一个槽。
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	h.waitUntil(t, "attempt1 运行", func() bool {
		h.r.mu.Lock()
		defer h.r.mu.Unlock()
		return h.r.running["t1"] != nil && h.r.running["t1"].attempt == 1
	}, 3*time.Second)
	h.cancel("t1", 1, "lease-1")
	h.waitFrame(t, func(f any) bool {
		c, ok := f.(*protocol.CancelAckFrame)
		return ok && c.TaskID == "t1" && c.Attempt == 1
	}, 5*time.Second)

	h.dispatch("t1", 2, a, digest)
	h.waitUntil(t, "attempt2 accept", func() bool {
		slot := findAcceptSlot(h.sender.snapshot(), "t1")
		_ = slot
		h.r.mu.Lock()
		defer h.r.mu.Unlock()
		res, ok := h.r.reservations["t1"]
		return ok && res.proposed == 2
	}, 3*time.Second)
	h.grant("t1", 2, "lease-2", "token-2")
	h.waitUntil(t, "attempt2 运行", func() bool {
		h.r.mu.Lock()
		defer h.r.mu.Unlock()
		return h.r.running["t1"] != nil && h.r.running["t1"].attempt == 2
	}, 3*time.Second)

	h.r.mu.Lock()
	att := h.r.running["t1"]
	h.r.mu.Unlock()
	// 旧 attempt 的 lease.ack 不能刷新 attempt 2 的 deadline。
	h.r.HandleLeaseAck(&protocol.LeaseAckFrame{
		Type: protocol.FrameLeaseAck, TaskID: "t1", Attempt: 1, LeaseID: "lease-1",
	})
	h.waitFrameAdvancing(t, func(f any) bool {
		ack, ok := f.(*protocol.CancelAckFrame)
		return ok && ack.TaskID == "t1" && ack.Attempt == 2 && ack.LeaseID == "lease-2" && ack.Slot != nil && *ack.Slot == att.slot && ack.ProcessGroupReaped
	}, 5*time.Second, 15*time.Second)
	rework2AssertExpiredArchive(t, h, att)
}

func TestInstancesReportReservationRunningAndIdle(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 0.3\n"
	})
	// 初始：slot0/1 都是显式空闲。
	insts := h.r.Instances()
	if len(insts) != 2 {
		t.Fatalf("应报 2 个真实槽：%+v", insts)
	}
	for i, inst := range insts {
		if inst.Slot == nil || *inst.Slot != i || inst.Busy == nil || *inst.Busy {
			t.Fatalf("空槽必须显式 busy=false：%+v", inst)
		}
	}

	// reservation（未 grant）：busy=true + assignment/proposed，没有 leaseId。
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	inst := h.r.Instances()[0]
	if inst.Busy == nil || !*inst.Busy || inst.TaskID != "t1" ||
		inst.AssignmentID != "assign-t1" || inst.ProposedAttempt != 1 || inst.LeaseID != "" {
		t.Fatalf("reservation 的 instance 事实不对：%+v", inst)
	}

	// grant 后：busy=true + attempt/leaseId。
	h.grant("t1", 1, "lease-1", "token-1")
	h.waitUntil(t, "running", func() bool {
		h.r.mu.Lock()
		defer h.r.mu.Unlock()
		return h.r.running["t1"] != nil
	}, 3*time.Second)
	inst = h.r.Instances()[0]
	if inst.Busy == nil || !*inst.Busy || inst.Attempt != 1 || inst.LeaseID != "lease-1" {
		t.Fatalf("running 的 instance 事实不对：%+v", inst)
	}
	h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second)
	inst = h.r.Instances()[0]
	if inst.Busy == nil || *inst.Busy || inst.TaskID != "" {
		t.Fatalf("终态回收后必须显式空闲：%+v", inst)
	}
}

func TestShrinkLimitsKeepsOccupiedSlotAndRejectsNew(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	h.limits = protocol.Limits{Total: 2, PerCapability: map[string]int{"echo": 2}}
	h.mu.Unlock()
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nsleep 2\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	h.waitUntil(t, "running", func() bool {
		h.r.mu.Lock()
		defer h.r.mu.Unlock()
		return h.r.running["t1"] != nil
	}, 3*time.Second)

	// 缩预算到 1：slot0 的存量占用必须继续报 busy，不能假释放。
	h.mu.Lock()
	h.limits = protocol.Limits{Total: 1, PerCapability: map[string]int{"echo": 1}}
	h.mu.Unlock()
	insts := h.r.Instances()
	if len(insts) < 1 || insts[0].Busy == nil || !*insts[0].Busy || insts[0].TaskID != "t1" {
		t.Fatalf("缩预算不能假释放存量占用：%+v", insts)
	}
	// 新任务没有预算内的空槽 → reject slot_unavailable。
	h.dispatch("t2", 1, a, digest)
	rej := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskRejectFrame)
		return ok && r.TaskID == "t2"
	}, 3*time.Second).(*protocol.TaskRejectFrame)
	if rej.Reason != protocol.ReasonSlotUnavailable {
		t.Fatalf("缩预算后无槽必须 slot_unavailable：%q", rej.Reason)
	}
}

func TestRecoverKeepsBusyWhenPGIDAlive(t *testing.T) {
	h := newHarness(t)
	// 造一条「崩溃前 running」的持久记录 + 一个真的活进程组。
	rec, err := h.results.Begin(resultstore.Record{
		TaskID: "old", Node: "node-a", Attempt: 1, Capability: "echo",
		Digest: "sha256:aa", TTL: 0, Phase: "running",
		Slot: intPtr(0), LeaseID: "lease-old", CreatedAt: h.clk.Now(),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	cmd, err := startSleepGroup(t, 30)
	if err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() }()
	if err := rec.SetPGID(cmd.Process.Pid); err != nil {
		t.Fatalf("SetPGID: %v", err)
	}

	if err := h.r.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	inst := h.r.Instances()[0]
	if inst.Busy == nil || !*inst.Busy || inst.TaskID != "old" || inst.Attempt != 1 || inst.LeaseID != "lease-old" {
		t.Fatalf("活着的旧进程组必须保持 busy/unknown：%+v", inst)
	}
}

func TestRecoverMarksDeadPGIDReaped(t *testing.T) {
	h := newHarness(t)
	rec, err := h.results.Begin(resultstore.Record{
		TaskID: "old2", Node: "node-a", Attempt: 1, Capability: "echo",
		Digest: "sha256:aa", TTL: 0, Phase: "running",
		Slot: intPtr(1), LeaseID: "lease-old2", CreatedAt: h.clk.Now(),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// 一个不存在的进程组：物理上已回收，可以安全释放。
	if err := rec.SetPGID(999999); err != nil {
		t.Fatalf("SetPGID: %v", err)
	}
	if err := h.r.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	inst := h.r.Instances()[1]
	if inst.Busy == nil || *inst.Busy {
		t.Fatalf("pgid 已消失的旧记录必须报空闲：%+v", inst)
	}
	records, err := h.results.RunningRecords()
	if err != nil || len(records) != 0 {
		t.Fatalf("已回收记录不能再算 running：%+v err=%v", records, err)
	}
}

func TestConfigChangeDoesNotAffectRunningAttemptSnapshot(t *testing.T) {
	h := newHarness(t)
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Requires.Values = []string{"vars.SOURCE_ROOT"}
		a.Spec.Run.Env = map[string]artifact.EnvValue{
			"SOURCE_ROOT": {FieldPath: "vars.SOURCE_ROOT"},
		}
		a.Spec.Run.Files["run.sh"] = "#!/bin/sh\nprintf '%s' \"$SOURCE_ROOT\" > snapshot.txt\nsleep 0.4\n"
	})
	h.dispatch("t1", 1, a, digest)
	h.waitFrame(t, func(any) bool { return hasAccept(h.sender.snapshot(), "t1") }, 2*time.Second)
	h.grant("t1", 1, "lease-1", "token-1")
	workDir := filepath.Join(h.workRoot, hexNameForTest("t1"), "1")
	h.waitUntil(t, "snapshot 写入", func() bool {
		_, err := os.Stat(filepath.Join(workDir, "snapshot.txt"))
		return err == nil
	}, 3*time.Second)
	// attempt 中途改配置：已启动 attempt 的环境快照不能变。
	h.mu.Lock()
	h.vars["SOURCE_ROOT"] = "/srv/changed"
	h.mu.Unlock()
	res := h.waitFrame(t, func(f any) bool {
		r, ok := f.(*protocol.TaskResultFrame)
		return ok && r.TaskID == "t1"
	}, 5*time.Second).(*protocol.TaskResultFrame)
	if res.Phase != protocol.TaskPhaseSucceeded {
		t.Fatalf("attempt 应成功：%+v", res)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "snapshot.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "/srv/src" {
		t.Fatalf("已启动 attempt 必须用启动时快照：%q err=%v", data, err)
	}
}

// startSleepGroup 起一个独立进程组的 sleep（模拟崩溃前遗留的旧执行进程组）。
func startSleepGroup(t *testing.T, seconds int) (*exec.Cmd, error) {
	t.Helper()
	cmd := exec.Command("sleep", fmt.Sprint(seconds))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
