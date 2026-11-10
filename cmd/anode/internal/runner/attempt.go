package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/execenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/cmd/anode/internal/upload"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// Attempt 是一次真实执行（一个 lease.grant 一份）。
type Attempt struct {
	r *Runner

	taskID       string
	attempt      int
	leaseID      string
	token        string
	assignmentID string
	namespace    string
	ref          protocol.CapabilityRef
	art          *artifact.Artifact
	params       []byte
	nsVars       map[string]string
	slot         int

	startedAt  time.Time
	finishedAt time.Time

	mu          sync.Mutex
	leaseOK     bool // 租约授权（上传/结果仍可用）
	emitOpen    bool // emit 窗口（进程在跑）
	terminating bool
	termReason  string
	grace       time.Duration
	deadline    time.Time
	pgid        int
	proc        *exec.Cmd

	uploadCtx    context.Context
	cancelUpload context.CancelFunc

	ackWake       chan struct{}
	done          chan struct{}
	procDone      chan struct{}
	leaseDone     chan struct{}
	resultAck     chan struct{}
	resultAckOnce sync.Once

	stdoutR, stderrR *os.File
	copiesDone       chan struct{}
	groupReaped      bool
	groupMu          sync.Mutex

	resolved  *execenv.Resolved
	workDir   string
	exportDir string
	record    *resultstore.Attempt
	exitCode  *int
	copyErr   error
	phase     protocol.TaskPhase
	errCode   string
	errMsg    string
	manifest  []resultstore.Manifest
	uploaded  []protocol.TaskArtifact
	uploadErr error
}

func newAttempt(r *Runner, res *reservation, grant *protocol.LeaseGrantFrame) *Attempt {
	uploadCtx, cancelUpload := context.WithCancel(context.Background())
	return &Attempt{
		uploadCtx:    uploadCtx,
		cancelUpload: cancelUpload,
		r:            r,
		taskID:       res.taskID,
		attempt:      grant.Attempt,
		leaseID:      grant.LeaseID,
		token:        grant.AttemptToken,
		assignmentID: res.assignmentID,
		namespace:    res.namespace,
		ref:          res.ref,
		art:          res.art,
		params:       res.params,
		nsVars:       res.nsVars,
		slot:         res.slot,
		// No process group exists before a successful spawn. Deterministic preparation
		// failures must release the slot, unlike an actually spawned unreaped group.
		groupReaped: true,
		leaseOK:     true,
		emitOpen:    true,
		deadline:    r.clk.Now().Add(r.opts.LeaseTTL),
		ackWake:     make(chan struct{}, 1),
		done:        make(chan struct{}),
		procDone:    make(chan struct{}),
		leaseDone:   make(chan struct{}),
		resultAck:   make(chan struct{}),
		copiesDone:  make(chan struct{}),
	}
}

// noteAck 用「最后一次 ACK + 本地 TTL」刷新 deadline，并叫醒续租循环重算。
func (a *Attempt) noteAck(deadline time.Time) {
	a.mu.Lock()
	if !a.leaseOK {
		a.mu.Unlock()
		return
	}
	// A late ACK cannot revive authorization after the last-ACK deadline.
	if !a.deadline.IsZero() && !a.r.clk.Now().Before(a.deadline) {
		a.mu.Unlock()
		a.stop(protocol.ReasonLeaseExpired, a.r.opts.TermGrace)
		return
	}
	a.deadline = deadline
	a.mu.Unlock()
	select {
	case a.ackWake <- struct{}{}:
	default:
	}
}

func (a *Attempt) authorized() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.leaseOK && (a.deadline.IsZero() || a.r.clk.Now().Before(a.deadline))
}

// emitAllowed is only the authorization to accept a new script emit. Process
// termination closes it without revoking the lease used by timeout finalization.
func (a *Attempt) emitAllowed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.emitOpen && !a.terminating && a.leaseOK && (a.deadline.IsZero() || a.r.clk.Now().Before(a.deadline))
}

// run 是一次 attempt 的全部生命周期。
func (a *Attempt) run() {
	defer close(a.done)
	defer a.r.finishAttempt(a)
	// Lease authorization spans preparation, execution and result submission,
	// not merely the process lifetime: only trusted result ACK/cancel/expiry
	// ends the authorization and permits the physically reaped slot to be freed.
	go func() { defer close(a.leaseDone); a.leaseLoop() }()
	defer func() {
		a.mu.Lock()
		a.leaseOK = false
		a.emitOpen = false
		a.mu.Unlock()
		a.cancelUpload()
		<-a.leaseDone
	}()

	if err := a.prepare(); err != nil {
		a.fail(execenv.ReasonOf(err), err.Error())
		a.finalizeResult()
		a.waitResultAck()
		return
	}
	if err := a.spawn(); err != nil {
		a.fail(protocol.ReasonScriptFailed, err.Error())
		a.finalizeResult()
		a.waitResultAck()
		return
	}
	a.waitProcess()
	a.finalizeResult()
	a.waitResultAck()
}

// waitResultAck holds the same slot and authorization through authoritative
// result.ack (accepted or rejected). Cancel/deadline fences the pending fact.
func (a *Attempt) waitResultAck() {
	a.r.mu.Lock()
	p := a.r.pending[resultKey(a.taskID, a.attempt)]
	waiting := p != nil && p.att == a && p.frame.LeaseID == a.leaseID
	a.r.mu.Unlock()
	if !waiting {
		return
	}
	select {
	case <-a.resultAck:
	case <-a.uploadCtx.Done():
		select {
		case <-a.resultAck:
			return
		default:
		}
		a.r.mu.Lock()
		key := resultKey(a.taskID, a.attempt)
		if p := a.r.pending[key]; p != nil && p.att == a && p.frame.LeaseID == a.leaseID {
			delete(a.r.pending, key)
		}
		a.r.mu.Unlock()
		a.r.sendCancelAck(&protocol.CancelAckFrame{Type: protocol.FrameCancelAck, TaskID: a.taskID, Attempt: a.attempt, LeaseID: a.leaseID, Slot: intPtr(a.slot), ProcessGroupReaped: a.groupReapedNow()})
	}
}

// prepare 做 grant 之后、启动之前的全部校验（错误都已计 attempt）。
func (a *Attempt) prepare() error {
	resolved, err := execenv.Resolve(a.r.envInput(a))
	if err != nil {
		return err
	}
	a.resolved = resolved

	// 启动前命令探测：缺命令是确定性失败（missing_requirement），不自动重试。
	for _, name := range a.art.Spec.Requires.Commands {
		if _, err := a.r.opts.Baseline.LookPath(name); err != nil {
			return &execenv.Error{Reason: protocol.ReasonMissingRequirement,
				Message: fmt.Sprintf("缺少命令 %q（PATH=%s）: %v", name, a.r.opts.Baseline.PATH(), err)}
		}
	}

	if err := a.resolveWorkDir(); err != nil {
		return err
	}

	rec, err := a.r.opts.Results.Begin(resultstore.Record{
		TaskID:        a.taskID,
		Node:          a.r.opts.NodeName,
		Attempt:       a.attempt,
		Capability:    a.art.Metadata.Name,
		Digest:        a.ref.Digest,
		TTL:           resultstore.TTLFor(a.art),
		Phase:         "running",
		SystemWorkDir: systemDirOf(a),
		UserWorkDir:   userDirOf(a),
		Slot:          &[]int{a.slot}[0],
		LeaseID:       a.leaseID,
		CreatedAt:     a.r.clk.Now(),
		Manifest:      []resultstore.Manifest{},
	})
	if err != nil {
		return &execenv.Error{Reason: protocol.ReasonWorkDirUnavailable, Message: err.Error()}
	}
	a.record = rec

	// A2：每 attempt 一个系统自有导出目录（0700、唯一随机后缀），在 spawn 前创建
	// 并注入 ANOTIFY_ARTIFACT_DIR。两个任务共享同一 workDir / 同 task 重试 / 旧
	// attempt 短时重叠都拿到不同目录，不会互相收集文件。
	exportDir, err := rec.NewExportDir()
	if err != nil {
		return &execenv.Error{Reason: protocol.ReasonArtifactExportInvalid, Message: err.Error()}
	}
	a.exportDir = exportDir
	a.resolved.SetBuiltin(protocol.EnvArtifactDir, exportDir)

	if err := execenv.Materialize(a.workDir, a.art.Spec.Run.Files, a.art.Spec.Run.Entrypoint); err != nil {
		return err
	}
	return nil
}

func systemDirOf(a *Attempt) string {
	if a.art.Spec.Run.WorkDir == "" {
		return a.workDir
	}
	return ""
}

func userDirOf(a *Attempt) string {
	if a.art.Spec.Run.WorkDir == "" {
		return ""
	}
	return a.workDir
}

// resolveWorkDir 决定本次执行目录：默认每 attempt 独立系统目录；用户 workDir 必须
// 模板展开后绝对/无 ../已存在/是目录/解软链，且不与系统 GC 根重叠。
func (a *Attempt) resolveWorkDir() error {
	root := a.r.opts.WorkRoot
	if strings.TrimSpace(root) == "" {
		return &execenv.Error{Reason: protocol.ReasonWorkDirUnavailable,
			Message: "节点缺少 workRoot（无法创建系统任务目录）"}
	}
	resultRoot := a.r.opts.Results.Root
	if a.art.Spec.Run.WorkDir == "" {
		dir := execenv.SystemWorkDir(root, a.taskID, a.attempt)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return &execenv.Error{Reason: protocol.ReasonWorkDirUnavailable,
				Message: fmt.Sprintf("创建系统任务目录 %s: %v", dir, err)}
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return &execenv.Error{Reason: protocol.ReasonWorkDirUnavailable,
				Message: fmt.Sprintf("系统任务目录 %s 不是普通目录", dir)}
		}
		a.workDir = dir
		return nil
	}
	tmpl, err := execenv.ExpandWorkDir(a.art.Spec.Run.WorkDir, a.resolved)
	if err != nil {
		return err
	}
	if tmpl == "" {
		return &execenv.Error{Reason: protocol.ReasonWorkDirUnavailable,
			Message: "run.workDir 模板展开为空"}
	}
	dir, err := execenv.ValidateDir(tmpl, root, resultRoot)
	if err != nil {
		return err
	}
	a.workDir = dir
	return nil
}

// spawn 真 shebang 直接 exec（无 shell 包裹），进程放进独立进程组。
func (a *Attempt) spawn() error {
	// os.Pipe 返回 (读端, 写端)：cmd 拿写端，父进程 Start 后关写端，collector 持
	// 读端读到 EOF。反接的话子进程往读端写会 EBADF 直接退出（exit 1、零输出）。
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("创建 stdout 管道: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return fmt.Errorf("创建 stderr 管道: %w", err)
	}
	a.stdoutR, a.stderrR = stdoutR, stderrR

	stdoutLog, stderrLog, err := a.record.OpenLogs()
	if err != nil {
		_ = stdoutW.Close()
		_ = stderrW.Close()
		_ = stdoutR.Close()
		_ = stderrR.Close()
		return err
	}

	entry := execenv.EntrypointPath(a.workDir, a.art.Spec.Run.Entrypoint)
	cmd := exec.Command(entry)
	cmd.Dir = a.workDir
	cmd.Env = a.resolved.Env()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = stdoutW.Close()
		_ = stderrW.Close()
		_ = stdoutR.Close()
		_ = stderrR.Close()
		_ = stdoutLog.Close()
		_ = stderrLog.Close()
		return fmt.Errorf("启动 %s: %w", entry, err)
	}
	a.groupMu.Lock()
	a.groupReaped = false
	a.groupMu.Unlock()
	// 父进程关掉**写端**，否则读端永远等不到 EOF。
	_ = stdoutW.Close()
	_ = stderrW.Close()
	_ = stdoutLog.Close()
	_ = stderrLog.Close()

	a.mu.Lock()
	a.proc = cmd
	a.pgid = cmd.Process.Pid
	a.startedAt = a.r.clk.Now()
	terminating := a.terminating
	termGrace := a.grace
	if termGrace <= 0 {
		termGrace = a.r.opts.TermGrace
	}
	a.mu.Unlock()
	if terminating {
		// grant 与 spawn 之间已经到了 cancel/timeout/lease 失效：立刻终止刚起来的进程组。
		_ = syscall.Kill(-a.pgid, syscall.SIGTERM)
		go func() {
			if !a.waitGone(a.pgid, termGrace) {
				_ = syscall.Kill(-a.pgid, syscall.SIGKILL)
				a.waitGone(a.pgid, termGrace)
			}
		}()
	}
	// 持久化 pgid：重启后靠它核查进程组是否真的回收（A3）。
	if err := a.record.SetPGID(cmd.Process.Pid); err != nil {
		a.r.logf("runner: 持久化 pgid 失败（重启只能按 unknown 处理）：%v", err)
	}

	var copies sync.WaitGroup
	copies.Add(2)
	go a.copyLog(a.stdoutR, a.record, resultstore.StdoutFileName, &copies)
	go a.copyLog(a.stderrR, a.record, resultstore.StderrFileName, &copies)
	go func() {
		copies.Wait()
		close(a.copiesDone)
	}()

	if timeout := a.art.Spec.Lifecycle.EffectiveTimeoutSec(); timeout > 0 {
		go a.timeoutLoop(time.Duration(timeout) * time.Second)
	}
	return nil
}

// copyLog 把一条流写进本机档案；滚动写入，不把全文留在内存。
func (a *Attempt) copyLog(src *os.File, rec *resultstore.Attempt, name string, wg *sync.WaitGroup) {
	defer wg.Done()
	defer src.Close()
	dst, err := rec.OpenLogFile(name)
	if err != nil {
		a.r.logf("runner: 打开日志 %s 失败：%v", name, err)
		_, _ = io.Copy(io.Discard, src)
		a.noteCopyErr(fmt.Errorf("打开 %s: %w", name, err))
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		a.r.logf("runner: 复制 %s 日志失败：%v", name, err)
		a.noteCopyErr(fmt.Errorf("复制 %s: %w", name, err))
	}
}

// noteCopyErr 记录日志流复制的非预期错误（终态要反映，不静默吞）。
func (a *Attempt) noteCopyErr(err error) {
	a.mu.Lock()
	if a.copyErr == nil {
		a.copyErr = err
	}
	a.mu.Unlock()
}

// leaseLoop 每 RenewInterval 续租；deadline（最后一次 ACK + TTL）到点自行停止。
//
// 断线不会让它「立刻成功/无限运行」：renew 发不出去，但 deadline 仍在走，到点
// TERM → grace → KILL 整个进程组。
func (a *Attempt) leaseLoop() {
	renewAt := a.r.clk.Now().Add(a.r.opts.RenewInterval)
	for {
		a.mu.Lock()
		if !a.leaseOK {
			a.mu.Unlock()
			return
		}
		deadline := a.deadline
		a.mu.Unlock()

		next := renewAt
		if !deadline.IsZero() && deadline.Before(next) {
			next = deadline
		}
		// Keep the absolute wake target across registration/scheduling delays.
		timer := a.r.clk.NewTimerAt(next)
		select {
		case <-a.ackWake:
			timer.Stop()
		case <-a.uploadCtx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}

		a.mu.Lock()
		now := a.r.clk.Now()
		if !a.leaseOK {
			a.mu.Unlock()
			return
		}
		expired := !a.deadline.IsZero() && !now.Before(a.deadline)
		a.mu.Unlock()
		if expired {
			a.r.logf("runner: task=%s attempt=%d 租约失联（最后一次 ACK + TTL 到点），TERM/KILL 进程组",
				a.taskID, a.attempt)
			a.stop(protocol.ReasonLeaseExpired, a.r.opts.TermGrace)
			return
		}
		if !now.Before(renewAt) {
			if err := a.r.send(&protocol.LeaseRenewFrame{
				Type: protocol.FrameLeaseRenew, TaskID: a.taskID,
				Attempt: a.attempt, LeaseID: a.leaseID,
			}); err != nil {
				a.r.logf("runner: lease.renew 发送失败（等 deadline 判失联）：%v", err)
			}
			renewAt = now.Add(a.r.opts.RenewInterval)
		}
	}
}

// timeoutLoop 是 lifecycle.timeoutSec：从真 spawn 起算，到点终止并报 timeout。
func (a *Attempt) timeoutLoop(d time.Duration) {
	timer := a.r.clk.NewTimer(d)
	select {
	case <-a.procDone:
		timer.Stop()
	case <-timer.C():
		a.r.logf("runner: task=%s attempt=%d 执行超时（%s），TERM/KILL 进程组",
			a.taskID, a.attempt, d)
		a.stop(protocol.ReasonTimeout, a.r.opts.TermGrace)
	}
}

// stop ends the process/emit window. A local lifecycle timeout is not a
// revocation of the hub grant: export/upload/failed-timeout result keep renewing
// until a trusted result ACK. Actual cancel/expiry always revoke, even if a
// previous timeout has already stopped the process.
func (a *Attempt) stop(reason string, grace time.Duration) {
	revoke := reason != protocol.ReasonTimeout
	a.mu.Lock()
	alreadyStopping := a.terminating
	a.terminating = true
	a.emitOpen = false
	if revoke {
		a.leaseOK = false
	}
	if !alreadyStopping || revoke {
		a.termReason = reason
		a.grace = grace
	}
	pgid := a.pgid
	a.mu.Unlock()
	if revoke {
		a.cancelUpload()
	}
	a.r.dropToken(a)
	if alreadyStopping {
		return
	}
	if pgid > 0 && !a.groupReapedNow() {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		go func() {
			if !a.waitGone(pgid, grace) {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				a.waitGone(pgid, grace)
			}
		}()
	}
}

// stopForCancel must also revoke a grant during timeout finalization.
func (a *Attempt) stopForCancel(reason protocol.CancelReason, grace time.Duration) {
	a.stop(string(reason), grace)
}

// waitProcess 等 leader 退出，收取日志，并回收整个进程组。
func (a *Attempt) waitProcess() {
	err := a.proc.Wait()
	a.r.logf("runner: task=%s attempt=%d 进程退出: err=%v", a.taskID, a.attempt, err)
	a.mu.Lock()
	a.finishedAt = a.r.clk.Now()
	a.emitOpen = false // 任务结束（I8）：迟到 emit 一律拒绝
	a.exitCode = exitCodeOf(err)
	a.mu.Unlock()
	a.r.dropToken(a)

	// leader 退出后，孙进程可能还持有管道：给一个 grace 收尾（注入时钟驱动），
	// 之后 KILL 整个组，再关读端，保证日志 copy 一定结束（不弱断言、不无限等待）。
	copyTimer := a.r.clk.NewTimer(a.r.opts.TermGrace)
	select {
	case <-a.copiesDone:
		copyTimer.Stop()
	case <-copyTimer.C():
		a.mu.Lock()
		pgid := a.pgid
		a.mu.Unlock()
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		_ = a.stdoutR.Close()
		_ = a.stderrR.Close()
		<-a.copiesDone
	}
	close(a.procDone)

	// 槽位只在进程组真正回收后释放：残留子孙也一并收掉。
	a.mu.Lock()
	pgid := a.pgid
	a.mu.Unlock()
	if pgid > 0 && groupAlive(pgid) {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		if !a.waitGone(pgid, a.r.opts.TermGrace) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			a.waitGone(pgid, a.r.opts.TermGrace)
		}
	}
	a.groupMu.Lock()
	a.groupReaped = pgid <= 0 || !groupAlive(pgid)
	a.groupMu.Unlock()
}

// finalizeResult 计算终态、导出/上传托管副本并上报 task.result。
func (a *Attempt) finalizeResult() {
	a.mu.Lock()
	terminating := a.terminating
	termReason := a.termReason
	leaseOK := a.leaseOK
	exitCode := a.exitCode
	startedAt := a.startedAt
	finishedAt := a.finishedAt
	preCode := a.errCode
	preMsg := a.errMsg
	a.mu.Unlock()

	if terminating && isCancelReason(termReason) {
		// 取消走 cancel.ack，不再发 task.result（中枢已按取消推进）。
		a.setPhase(protocol.TaskPhaseCancelled, termReason, "")
		a.finishRecord(protocol.TaskPhaseCancelled)
		return
	}

	phase := protocol.TaskPhaseSucceeded
	reasonCode := ""
	message := ""
	switch {
	case preCode != "":
		// 启动前失败（HOME/workDir/requirement/files_escape）：不上传任何东西。
		phase = protocol.TaskPhaseFailed
		reasonCode = preCode
		message = preMsg
	case terminating && termReason != "":
		phase = protocol.TaskPhaseFailed
		reasonCode = termReason
		message = fmt.Sprintf("%s（已终止）", termReason)
	case exitCode == nil:
		phase = protocol.TaskPhaseFailed
		reasonCode = protocol.ReasonScriptFailed
		message = "子进程被信号终止（没有退出码）"
	case *exitCode != 0:
		phase = protocol.TaskPhaseFailed
		reasonCode = protocol.ReasonScriptFailed
		message = fmt.Sprintf("脚本退出码 %d", *exitCode)
	}
	a.mu.Lock()
	copyErr := a.copyErr
	a.mu.Unlock()
	if copyErr != nil && phase == protocol.TaskPhaseSucceeded {
		// 日志流复制失败 = 档案不完整，不能诚实地报成功。
		phase = protocol.TaskPhaseFailed
		reasonCode = protocol.ReasonScriptFailed
		message = "日志流复制失败: " + copyErr.Error()
	}

	// Export/upload after process completion (including local timeout) uses the
	// still-live lease; cancel/expiry/preparation failure cannot authorize it.
	if preCode == "" && a.record != nil && leaseOK && a.authorized() {
		manifest, err := a.record.ExportDir(a.exportDir)
		if err != nil {
			// 枚举/复制失败（symlink、非普通文件、越界、超限）是可判定的确定性
			// 失败：明确 failed/artifact_export_invalid，不伪造成功空列表。
			phase = protocol.TaskPhaseFailed
			reasonCode = protocol.ReasonArtifactExportInvalid
			message = err.Error()
			a.uploadErr = err
		} else {
			a.manifest = manifest
			if !leaseOK || !a.authorized() {
				phase = protocol.TaskPhaseFailed
				reasonCode = protocol.ReasonLeaseExpired
				message = "租约在产物上传前失效"
			} else if uploadErr := a.uploadManifests(manifest); uploadErr != nil {
				phase = protocol.TaskPhaseFailed
				reasonCode = protocol.ReasonArtifactUploadFailed
				message = uploadErr.Error()
				a.uploadErr = uploadErr
			}
		}
	}

	a.setPhase(phase, reasonCode, message)
	a.finishRecord(phase)
	frame := a.resultFrame(phase, reasonCode, message, startedAt, finishedAt, exitCode)
	// Cancellation/deadline can arrive during export or a blocked upload. Do not
	// publish a result assembled under the old snapshot of leaseOK/terminating.
	if !a.authorized() {
		a.mu.Lock()
		reason := a.termReason
		a.mu.Unlock()
		phase = protocol.TaskPhaseFailed
		if isCancelReason(reason) {
			phase = protocol.TaskPhaseCancelled
		}
		if reason == "" {
			reason = protocol.ReasonLeaseExpired
		}
		a.setPhase(phase, reason, "产物/结果阶段授权已停止")
		a.finishRecord(phase)
		a.r.sendCancelAck(&protocol.CancelAckFrame{
			Type: protocol.FrameCancelAck, TaskID: a.taskID, Attempt: a.attempt,
			LeaseID: a.leaseID, Slot: intPtr(a.slot), ProcessGroupReaped: a.groupReapedNow(),
		})
		return
	}
	if frame != nil {
		a.r.sendResult(frame)
	}
}

// uploadManifests 逐个上传系统托管副本；只在仍有租约授权时进行。
func (a *Attempt) uploadManifests(manifest []resultstore.Manifest) error {
	if len(manifest) == 0 {
		return nil
	}
	if a.r.opts.Upload == nil {
		return errors.New("节点没有配置产物上传客户端")
	}
	for _, m := range manifest {
		if !a.authorized() {
			return context.Canceled
		}
		resp, err := a.r.opts.Upload.Upload(a.uploadCtx, upload.Request{
			TaskID:       a.taskID,
			Attempt:      a.attempt,
			LeaseID:      a.leaseID,
			AttemptToken: a.token,
			Path:         m.Path,
			SHA256:       m.SHA256,
			Size:         m.Size,
			LocalPath:    filepath.Join(a.record.Dir(), filepath.FromSlash(m.Path)),
		})
		if err != nil {
			return err
		}
		a.uploaded = append(a.uploaded, protocol.TaskArtifact{Path: resp.Path, SHA256: resp.SHA256})
	}
	return nil
}

func (a *Attempt) setPhase(phase protocol.TaskPhase, reason, message string) {
	a.mu.Lock()
	a.phase = phase
	a.errCode = reason
	a.errMsg = message
	a.mu.Unlock()
}

// finishRecord 把终态与（本机）托管清单写进 meta（重启后 GC 的唯一依据）。
func (a *Attempt) finishRecord(phase protocol.TaskPhase) {
	if a.record == nil {
		return
	}
	a.mu.Lock()
	finishedAt := a.finishedAt
	a.mu.Unlock()
	a.record.SetReaped(a.groupReapedNow())
	if err := a.record.Finish(string(phase), a.manifest, finishedAt); err != nil {
		a.r.logf("runner: 写 attempt meta 失败：%v", err)
	}
}

// resultFrame 组装 task.result（artifacts 永远是非 nil 数组）。
func (a *Attempt) resultFrame(phase protocol.TaskPhase, reason, message string,
	startedAt, finishedAt time.Time, exitCode *int) *protocol.TaskResultFrame {
	result := protocol.TaskResult{
		Artifacts: make([]protocol.TaskArtifact, 0, len(a.uploaded)),
	}
	result.Artifacts = append(result.Artifacts, a.uploaded...)
	switch phase {
	case protocol.TaskPhaseSucceeded:
		result.Summary = fmt.Sprintf("%s v%d 执行成功（退出码 0）",
			a.art.Metadata.Name, a.art.Metadata.Version)
	default:
		result.Summary = fmt.Sprintf("%s 执行失败：%s", a.art.Metadata.Name, reason)
		if message != "" {
			result.Summary += "（" + firstLine(message) + "）"
		}
		result.Error = &protocol.TaskResultError{Reason: reason, Message: message}
	}
	return &protocol.TaskResultFrame{
		Type:       protocol.FrameTaskResult,
		TaskID:     a.taskID,
		Attempt:    a.attempt,
		Node:       a.r.opts.NodeName,
		LeaseID:    a.leaseID,
		Phase:      phase,
		ExitCode:   exitCode,
		Result:     result,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
	}
}

// fail 记录启动前错误（HOME/workDir/requirement/files_escape）。
//
// 只记录状态：终态上报统一走 finalizeResult，避免同一次 attempt 发两条 result。
func (a *Attempt) fail(reason, message string) {
	if reason == "" {
		reason = protocol.ReasonScriptFailed
	}
	now := a.r.clk.Now()
	a.mu.Lock()
	a.exitCode = nil
	a.emitOpen = false
	if a.finishedAt.IsZero() {
		a.finishedAt = now
	}
	if a.startedAt.IsZero() {
		a.startedAt = now
	}
	a.errCode = reason
	a.errMsg = message
	a.phase = protocol.TaskPhaseFailed
	a.mu.Unlock()
	a.r.dropToken(a)
}

func (a *Attempt) groupReapedNow() bool {
	a.groupMu.Lock()
	defer a.groupMu.Unlock()
	return a.groupReaped
}

func isCancelReason(reason string) bool {
	switch protocol.CancelReason(reason) {
	case protocol.CancelReasonUser, protocol.CancelReasonAbandonedByUser, protocol.CancelReasonInfra:
		return true
	}
	return false
}

// exitCodeOf 把 Wait 错误翻成退出码；被信号杀死返回 nil（wire 上省略）。
func exitCodeOf(err error) *int {
	if err == nil {
		zero := 0
		return &zero
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return nil
		}
		code := ee.ExitCode()
		if code < 0 {
			return nil
		}
		return &code
	}
	return nil
}

// groupAlive 报告进程组里是否还有进程（kill(-pgid, 0)）。
func groupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

// waitGone 用**注入时钟**等进程组消失：进程组的存活只能真实轮询观察，但
// 「TERM 到 KILL 的 5s 宽限」由 clock 决定，测试推进假时钟即可（生产仍是 5s）。
func (a *Attempt) waitGone(pgid int, grace time.Duration) bool {
	if pgid <= 0 || !groupAlive(pgid) {
		return true
	}
	if grace <= 0 {
		return false
	}
	timer := a.r.clk.NewTimer(grace)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C():
			return !groupAlive(pgid)
		case <-ticker.C:
			if !groupAlive(pgid) {
				timer.Stop()
				return true
			}
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
