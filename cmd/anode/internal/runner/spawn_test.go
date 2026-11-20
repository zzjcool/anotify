package runner

// ETXTBSY 回归：Linux 的 execve 拒绝「inode 上仍有写句柄」的入口脚本，而**另一个
// 并发任务**的 fork 会把这份写句柄复制进自己的子进程（CLOEXEC 只在 execve 那一刻
// 才关），于是健康任务的 exec 会偶发撞成 ETXTBSY。修法是 spawn 退避重试；这里钉住
// 两侧：重试到窗口关闭必须成功，重试用尽必须如实上报（不是重试到天荒地老，也不是
// 把 ETXTBSY 吞掉当成功），且非 ETXTBSY 的启动错误不许被重试拖成慢失败。

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/execenv"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// holdEntrypointWrite 预先建好 attempt 的工作目录与入口脚本，并持有写句柄 ——
// 同进程里对同一 inode 持有写句柄，execve 必定返回 ETXTBSY（内核 i_writecount）。
// 返回的 release 关闭句柄：重试窗口随之关闭。
func holdEntrypointWrite(t *testing.T, h *harness, taskID string) (release func()) {
	t.Helper()
	workDir := execenv.SystemWorkDir(h.workRoot, taskID, 1)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("预建 workDir: %v", err)
	}
	entry := execenv.EntrypointPath(workDir, "run.sh")
	if err := os.WriteFile(entry, []byte("#!/bin/sh\necho hi\n"), 0o700); err != nil {
		t.Fatalf("预写入口脚本: %v", err)
	}
	f, err := os.OpenFile(entry, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("持有入口脚本写句柄: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = f.Close() }) }
	t.Cleanup(release)
	return release
}

// watchSpawnRetry 把「spawn 因 ETXTBSY 退避重试」这条日志变成信号。
// 必须在本用例创建任何 attempt 之前设置 Logf（之后它就是并发读的了）。
func watchSpawnRetry(t *testing.T, h *harness) <-chan struct{} {
	t.Helper()
	retried := make(chan struct{})
	var once sync.Once
	h.r.opts.Logf = func(format string, args ...any) {
		if strings.Contains(format, "ETXTBSY") {
			once.Do(func() { close(retried) })
		}
		t.Logf(format, args...)
	}
	return retried
}

// awaitFirstRetry 等到第一次 ETXTBSY 重试被观察到。
//
// 若 exec 确实因写句柄失败、却直接以失败结果落地（= 重试被回退），这里立刻判红；
// 若该平台的 exec 不因写句柄失败（非 Linux 语义），前提不成立，skip 并说明。
func awaitFirstRetry(t *testing.T, h *harness, retried <-chan struct{}, taskID string) {
	t.Helper()
	timer := time.NewTimer(testWaitLimit)
	defer timer.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-retried:
			return
		case <-tick.C:
			r := findResult(h.sender.snapshot(), taskID, 1)
			if r == nil {
				continue
			}
			msg := ""
			if r.Result.Error != nil {
				msg = r.Result.Error.Reason + ": " + r.Result.Error.Message
			}
			if strings.Contains(strings.ToLower(msg), "text file busy") {
				t.Fatalf("exec 撞到 ETXTBSY（%s）却没有重试，直接判成 %s", msg, r.Phase)
			}
			t.Fatalf("期望 ETXTBSY 前提成立，实际拿到 %s（%s）", r.Phase, msg)
		case <-timer.C:
			t.Skipf("平台不把「写句柄未关」当 exec 失败（非 Linux ETXTBSY 语义），此用例不适用")
		}
	}
}

func TestSpawnRetriesTextFileBusy(t *testing.T) {
	h := newHarness(t)
	h.sender.onResult = nil
	retried := watchSpawnRetry(t, h)
	a, digest := h.artifact(nil)
	release := holdEntrypointWrite(t, h, "busy")
	h.dispatch("busy", 1, a, digest)
	h.grant("busy", 1, "lease-busy", "token-busy")
	awaitFirstRetry(t, h, retried, "busy")
	// 写句柄一关，窗口就关了：剩下的重试必须把这次执行救回来（不能判 failed）。
	release()
	h.waitUntil(t, "retry 窗口关闭后执行成功", func() bool {
		r := findResult(h.sender.snapshot(), "busy", 1)
		return r != nil && r.Phase == protocol.TaskPhaseSucceeded
	}, testWaitLimit)
}

func TestSpawnReportsTextFileBusyAfterRetriesExhausted(t *testing.T) {
	h := newHarness(t)
	h.sender.onResult = nil
	retried := watchSpawnRetry(t, h)
	a, digest := h.artifact(nil)
	// 整个执行期都不释放：重试必须用尽并如实上报（有界，不会无限等）。
	holdEntrypointWrite(t, h, "busy")
	h.dispatch("busy", 1, a, digest)
	h.grant("busy", 1, "lease-busy", "token-busy")
	awaitFirstRetry(t, h, retried, "busy")
	start := time.Now()
	h.waitUntil(t, "重试用尽后如实上报 script_failed", func() bool {
		r := findResult(h.sender.snapshot(), "busy", 1)
		if r == nil || r.Phase != protocol.TaskPhaseFailed {
			return false
		}
		if r.Result.Error == nil || !strings.Contains(strings.ToLower(r.Result.Error.Message), "text file busy") {
			t.Fatalf("ETXTBSY 用尽后必须如实上报原始错误，实际：%+v", r.Result)
		}
		return true
	}, testWaitLimit)
	budget := spawnRetryBudget()
	if elapsed := time.Since(start); elapsed < budget-time.Millisecond {
		t.Fatalf("重试用尽只用 %s，少于退避预算 %s：重试序列没有真正跑完", elapsed, budget)
	}
}

// 重试只针对 ETXTBSY：其它启动错误（这里是 shebang 解释器不存在 → ENOENT）
// 必须立刻如实上报，不能被退避重试拖成「慢失败」。
func TestSpawnDoesNotRetryOtherStartErrors(t *testing.T) {
	h := newHarness(t)
	h.sender.onResult = nil
	// 不信墙钟：机器负载下手一次 fork/exec 本身就可能超过退避预算，用「有没有发出
	// 重试」这条直接证据判定（ETXTBSY 重试是唯一会打印它的分支）。
	var mu sync.Mutex
	retried := 0
	h.r.opts.Logf = func(format string, args ...any) {
		if strings.Contains(format, "ETXTBSY") {
			mu.Lock()
			retried++
			mu.Unlock()
		}
		t.Logf(format, args...)
	}
	a, digest := h.artifact(func(a *artifact.Artifact) {
		a.Spec.Run.Files["run.sh"] = "#!/nonexistent/interpreter\necho hi\n"
	})
	h.dispatch("nointerp", 1, a, digest)
	h.grant("nointerp", 1, "lease-nointerp", "token-nointerp")
	h.waitUntil(t, "非 ETXTBSY 的启动错误立刻落地", func() bool {
		r := findResult(h.sender.snapshot(), "nointerp", 1)
		return r != nil && r.Phase == protocol.TaskPhaseFailed
	}, testWaitLimit)
	mu.Lock()
	defer mu.Unlock()
	if retried != 0 {
		t.Fatalf("ENOENT 这类确定性启动错误不该走 ETXTBSY 退避重试，实际重试了 %d 次", retried)
	}
	r := findResult(h.sender.snapshot(), "nointerp", 1)
	if r.Result.Error == nil || r.Result.Error.Reason != protocol.ReasonScriptFailed {
		t.Fatalf("启动错误必须以 script_failed 落地，实际：%+v", r.Result)
	}
	if !strings.Contains(r.Result.Error.Message, "no such file or directory") {
		t.Fatalf("启动错误必须带原始原因，实际：%q", r.Result.Error.Message)
	}
}

func spawnRetryBudget() time.Duration {
	var budget time.Duration
	for _, d := range spawnRetries {
		budget += d
	}
	return budget
}
