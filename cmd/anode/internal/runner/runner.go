// Package runner 是节点上的真实执行运行器：dispatch → accept/reject → lease.grant
// → 真 shebang exec → renew/ack → result/ack / cancel。
//
// 硬不变量（capability.md §8.5 / decisions V4-D4 / SLOT-HANDOFF A3）：
//
//  1. **派发不是授权**：task.dispatch 只做校验 + 原子预留真实空闲槽（accept），
//     没有 token、不执行、不计 attempt；只有 lease.grant 才启动进程，且 grant 里的
//     attempt 是权威 attempt。
//  2. **只有两种 reject**：stale_config（机器级 revision 真的不一致）与
//     slot_unavailable（没有真实空槽/预算吃满）。cache/ref/ns 的问题不得伪装成
//     stale_config；不能 accept 的派发直接不回应（中枢按租约/重派处理），启动前的
//     确定性错误走 task.result failed（计 attempt）。
//  3. **历史绑定栅栏**：lease.ack / cancel / result.ack 都按 (taskId, attempt,
//     leaseId, slot) 精确匹配；旧 attempt 的取消/ACK 不动当前进程、不清新槽。
//  4. **物理事实优先**：槽位账本记录真实占用；重启按持久记录 + pgid 核查，未确认
//     reaped 的保持 busy/unknown，不因内存为空/新 hello 就报 idle。缩预算不假释放
//     存量占用。
package runner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/anotify/cmd/anode/internal/clock"
	"github.com/zzjcool/anotify/cmd/anode/internal/execenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/hostenv"
	"github.com/zzjcool/anotify/cmd/anode/internal/resultstore"
	"github.com/zzjcool/anotify/cmd/anode/internal/upload"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// Sender 是上行帧发送入口（hub.Client 实现；离线时返回 error，不阻塞）。
type Sender interface {
	Send(frame any) error
}

// GuardedSender is an independent result-only transport contract. It must
// evaluate the pure guard at its actual serialized write boundary, not before
// joining a private write queue. false/nil is a local stale-send rejection.
// Send remains available for hello/config/ops, never as a result fallback.
type GuardedSender interface {
	SendGuarded(ctx context.Context, frame any, guard func() bool) (sent bool, err error)
}

// Options 是运行器的装配参数（全部显式注入，便于测试）。
type Options struct {
	// NodeName 是本节点名（accept/reject/result 的 node 字段；grant.node 必须相等）。
	NodeName string
	// LocalVars 是 anode.yaml 的 vars（来源诊断 local）。
	LocalVars map[string]string
	// Passthrough 是 envPassthrough 名字白名单。
	Passthrough []string
	// Baseline 是启动环境快照（PATH / HOME / 凭证值）。
	Baseline hostenv.Baseline
	// Facts 是节点内置事实（status.*）。
	Facts protocol.NodeFacts
	// Applied 返回机器级生效值（revision / vars / limits / 是否可信）。
	Applied func() (revision string, vars map[string]string, limits protocol.Limits, present bool)
	// AllowsNamespace 是本机授权预检（申报 ∩ 授权）。
	AllowsNamespace func(namespace string) bool
	// LoadArtifact 按 digest 读缓存产物（已通过 digest/refs 校验）。
	LoadArtifact func(digest string) (*artifact.Artifact, error)
	// Results 是系统结果目录（日志 / 托管副本 / meta / GC）。
	Results *resultstore.Store
	// Upload 是产物上行客户端（nil = 不上传；测试可注入 fake）。
	Upload *upload.Client
	// EmitEndpoint 是注入任务的 ANOTIFY_EMIT_ENDPOINT。
	EmitEndpoint string
	// WorkRoot 是系统创建 attempt 目录的父目录（status.workRoot）。
	WorkRoot string
	// Clock 是时钟（默认真实时钟；测试注入假时钟推进 20/60/5）。
	Clock clock.Clock
	// Logf 是诊断日志（可选）。
	Logf func(format string, args ...any)

	// LeaseTTL / RenewInterval / TermGrace 默认取协议常量（生产不得缩小）。
	LeaseTTL      time.Duration
	RenewInterval time.Duration
	TermGrace     time.Duration
}

// EmitBinding 是 emit 网关需要的「当前运行 attempt」视图。
type EmitBinding struct {
	TaskID       string
	Attempt      int
	LeaseID      string
	Token        string
	Next         []string
	EmitEndpoint string
}

// pendingResult 是已经算出的终态事实（重连只重发事实，绝不重执行）。
type pendingResult struct {
	frame *protocol.TaskResultFrame // ownership transferred on creation; immutable thereafter
	att   *Attempt                  // unique verified execution, never inferred from current running on ACK
}

// slotEntry 是一条**物理槽位**记录（A3）。
type slotEntry struct {
	index      int
	capability string
	// 已执行：task/attempt/lease；未 grant 的 reservation：task/assignment/proposed。
	taskID       string
	assignmentID string
	proposed     int
	attempt      int
	leaseID      string
	running      bool
	reservedAt   time.Time
	expiresAt    time.Time
	reaped       bool // 历史回收事实（重启恢复的 unknown 记录 reaped=false）
}

// slotRecord 是历史回收账本的一条（late cancel/ack 用）。
type slotRecord struct {
	taskID  string
	attempt int
	leaseID string
	slot    int
	reaped  bool
}

type reservation struct {
	taskID       string
	assignmentID string
	proposed     int
	namespace    string
	ref          protocol.CapabilityRef
	params       []byte
	nsVars       map[string]string
	nsRevision   string
	cfgRevision  string
	art          *artifact.Artifact
	slot         int
	created      time.Time
	expiresAt    time.Time
}

// Runner 是执行运行器（并发安全；帧处理不阻塞 WS 读循环）。
type Runner struct {
	opts Options
	clk  clock.Clock

	mu             sync.Mutex
	sender         Sender
	sendGeneration uint64
	reservations   map[string]*reservation
	running        map[string]*Attempt
	tokens         map[string]*Attempt
	slots          []*slotEntry
	recovering     map[string]bool
	recoveryStop   chan struct{}
	history        map[string]*slotRecord
	historyOrder   []string
	pending        map[string]*pendingResult
	pendingCxl     map[string]*protocol.CancelAckFrame
	draining       bool
	closed         bool
	wg             sync.WaitGroup
}

// maxHistory 是历史回收账本上限（late cancel/ack 的窗口，不是业务状态）。
const maxHistory = 1024

// New 构造运行器。
func New(opts Options) (*Runner, error) {
	if strings.TrimSpace(opts.NodeName) == "" {
		return nil, errors.New("runner: 缺少节点名")
	}
	if opts.Results == nil {
		return nil, errors.New("runner: 缺少结果目录")
	}
	if opts.Applied == nil || opts.AllowsNamespace == nil || opts.LoadArtifact == nil {
		return nil, errors.New("runner: 缺少配置/产物访问入口")
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = protocol.LeaseTTL
	}
	if opts.RenewInterval <= 0 {
		opts.RenewInterval = protocol.LeaseRenewInterval
	}
	if opts.TermGrace <= 0 {
		opts.TermGrace = protocol.LeaseTermGrace
	}
	return &Runner{
		opts:         opts,
		clk:          opts.Clock,
		reservations: map[string]*reservation{},
		running:      map[string]*Attempt{},
		tokens:       map[string]*Attempt{},
		recovering:   map[string]bool{},
		recoveryStop: make(chan struct{}),
		history:      map[string]*slotRecord{},
		pending:      map[string]*pendingResult{},
		pendingCxl:   map[string]*protocol.CancelAckFrame{},
	}, nil
}

// SetSender 绑定上行发送器（装配顺序：先 New，再建 hub.Client，再绑定）。
func (r *Runner) SetSender(s Sender) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sender = s
	r.sendGeneration++
}

func (r *Runner) logf(format string, args ...any) {
	if r.opts.Logf != nil {
		r.opts.Logf(format, args...)
	}
}

// ActiveTasks 返回当前占用槽位/正在执行的 taskId 集合（GC 用：活动任务不删）。
func (r *Runner) ActiveTasks() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]bool, len(r.running)+len(r.reservations))
	for id := range r.running {
		out[id] = true
	}
	for id := range r.reservations {
		out[id] = true
	}
	for _, slot := range r.slots {
		if slot != nil && slot.taskID != "" {
			out[slot.taskID] = true
		}
	}
	return out
}

// Draining 报告是否已进入 drain（升级 / 退出）。
func (r *Runner) Draining() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.draining
}

// Instances 返回真实物理槽位快照（hello / config.applied 用）。
//
//   - 空槽：busy=false（本机明确无进程）；
//   - reservation（未 grant）：busy=true + taskId/assignmentId/proposedAttempt，**不伪造 leaseId**；
//   - 运行/未确认回收：busy=true + taskId/attempt/leaseId；
//   - 缩预算后超出新范围的存量占用照样报告，直到真正回收。
func (r *Runner) Instances() []protocol.HelloInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	total, _, present := r.budgetTotalLocked()
	n := 0
	if present {
		n = total
	}
	// 崩溃恢复的存量占用即使超出当前预算也要报（不能假装空闲）。
	for _, slot := range r.slots {
		if slot != nil && slot.taskID != "" && slot.index+1 > n {
			n = slot.index + 1
		}
	}
	if n == 0 {
		// 冷启动没有 applied、也没有恢复的物理占用：不伪造槽位（未知 ≠ 空闲）。
		return nil
	}
	out := make([]protocol.HelloInstance, 0, n)
	for i := 0; i < n; i++ {
		idx := i
		inst := protocol.HelloInstance{Slot: &idx}
		var slot *slotEntry
		if i < len(r.slots) {
			slot = r.slots[i]
		}
		if slot == nil || slot.taskID == "" {
			no := false
			inst.Busy = &no
			out = append(out, inst)
			continue
		}
		yes := true
		inst.Busy = &yes
		inst.TaskID = slot.taskID
		if slot.running {
			inst.Attempt = slot.attempt
			inst.LeaseID = slot.leaseID
		} else {
			inst.AssignmentID = slot.assignmentID
			inst.ProposedAttempt = slot.proposed
		}
		out = append(out, inst)
	}
	return out
}

// Recover 从持久 attempt 记录恢复物理槽位账本（重启/崩溃后调用）。
//
// 未确认 reaped 的旧执行保持 busy/unknown：不能因为新进程内存为空就报 idle。
// pgid 还活着 → 占用该槽；pgid 已消失 → 记录为已回收（安全可释放）。
func (r *Runner) Recover() error {
	records, err := r.opts.Results.UnreapedRecords()
	if err != nil {
		return err
	}
	for _, rec := range records {
		if rec.Slot == nil || *rec.Slot < 0 {
			continue
		}
		if rec.PGID <= 0 || !groupAlive(rec.PGID) {
			r.reapRecovered(rec)
			continue
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil
		}
		slot := r.ensureSlotLocked(*rec.Slot)
		slot.taskID = rec.TaskID
		slot.attempt = rec.Attempt
		slot.leaseID = rec.LeaseID
		slot.capability = rec.Capability
		slot.running = true
		slot.reaped = false
		slot.reservedAt = r.clk.Now()
		r.addHistoryLocked(&slotRecord{taskID: rec.TaskID, attempt: rec.Attempt, leaseID: rec.LeaseID, slot: *rec.Slot, reaped: false})
		key := resultKey(rec.TaskID, rec.Attempt)
		if !r.recovering[key] {
			r.recovering[key] = true
			r.wg.Add(1)
			go r.watchRecovered(rec)
		}
		r.mu.Unlock()
		r.logf("runner: 重启恢复：task=%s attempt=%d slot=%d 的进程组 %d 仍活着，保持 busy", rec.TaskID, rec.Attempt, *rec.Slot, rec.PGID)
	}
	return nil
}

// Recovery never renews an old execution lease. It only observes physical death
// and reports the historical binding, even if the task was already terminal.
func (r *Runner) watchRecovered(rec resultstore.Record) {
	defer r.wg.Done()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.recoveryStop:
			return
		case <-ticker.C:
			if !groupAlive(rec.PGID) {
				r.reapRecovered(rec)
				return
			}
		}
	}
}

func (r *Runner) reapRecovered(rec resultstore.Record) {
	if err := r.opts.Results.MarkReaped(rec.TaskID, rec.Attempt, true); err != nil {
		r.logf("runner: 持久化历史回收失败：%v", err)
	}
	r.mu.Lock()
	delete(r.recovering, resultKey(rec.TaskID, rec.Attempt))
	r.addHistoryLocked(&slotRecord{taskID: rec.TaskID, attempt: rec.Attempt, leaseID: rec.LeaseID, slot: *rec.Slot, reaped: true})
	slot := r.ensureSlotLocked(*rec.Slot)
	// An old physical ACK cannot release a slot now owned by another binding.
	if slot.taskID == rec.TaskID && slot.attempt == rec.Attempt && slot.leaseID == rec.LeaseID && slot.running {
		r.releaseSlotLocked(*rec.Slot, rec.Capability)
	}
	r.mu.Unlock()
	r.sendCancelAck(&protocol.CancelAckFrame{Type: protocol.FrameCancelAck, TaskID: rec.TaskID, Attempt: rec.Attempt, LeaseID: rec.LeaseID, Slot: intPtr(*rec.Slot), ProcessGroupReaped: true})
}

// HandleDispatch 处理 task.dispatch：只做校验与真实槽位预留，不发 token、不执行。
func (r *Runner) HandleDispatch(f *protocol.TaskDispatchFrame) {
	if err := protocol.ValidateDispatch(f); err != nil {
		r.logf("runner: 忽略不合法的 task.dispatch：%v", err)
		return
	}
	r.mu.Lock()
	if r.draining || r.closed {
		r.mu.Unlock()
		return
	}
	r.pruneReservationsLocked()
	// 同一有效 assignment 重复 dispatch/accept 幂等回同一 reservation，不占第二槽。
	if res, ok := r.reservations[f.TaskID]; ok {
		if res.assignmentID == f.AssignmentID && res.proposed == f.ProposedAttempt {
			slot := res.slot
			r.mu.Unlock()
			r.sendAccept(f, slot)
			return
		}
		r.mu.Unlock()
		r.logf("runner: task=%s 已有不同 assignment 的预留（%s/%d），忽略 dispatch %s/%d",
			f.TaskID, res.assignmentID, res.proposed, f.AssignmentID, f.ProposedAttempt)
		return
	}
	if _, ok := r.running[f.TaskID]; ok {
		r.mu.Unlock()
		r.logf("runner: task=%s 已在执行，忽略重复 dispatch（不重复 accept）", f.TaskID)
		return
	}
	appliedRev, _, _, present := r.opts.Applied()
	stale := !present || appliedRev != f.ConfigRevision
	r.mu.Unlock()

	if stale {
		// 唯一合法的配置类拒绝：机器级配置还没 applied / revision 不一致。
		r.send(&protocol.TaskRejectFrame{
			Type:            protocol.FrameTaskReject,
			TaskID:          f.TaskID,
			AssignmentID:    f.AssignmentID,
			ProposedAttempt: f.ProposedAttempt,
			Node:            r.opts.NodeName,
			Reason:          protocol.ReasonStaleConfig,
		})
		return
	}
	if !r.opts.AllowsNamespace(f.Namespace) {
		r.logf("runner: task=%s 的 ns %q 不在本机授权内，拒绝 accept（无合适的 reject 原因）",
			f.TaskID, f.Namespace)
		return
	}
	art, err := r.opts.LoadArtifact(f.Capability.Digest)
	if err != nil {
		r.logf("runner: task=%s 的产物 %s 不在缓存里，拒绝 accept：%v",
			f.TaskID, f.Capability.Digest, err)
		return
	}
	if art.Metadata.Name != f.Capability.Name ||
		(art.Metadata.Version != 0 && f.Capability.Version != 0 && art.Metadata.Version != f.Capability.Version) {
		r.logf("runner: task=%s 的 capability ref 与缓存产物不一致", f.TaskID)
		return
	}

	r.mu.Lock()
	if r.draining || r.closed {
		r.mu.Unlock()
		return
	}
	if res, ok := r.reservations[f.TaskID]; ok {
		slot := res.slot
		r.mu.Unlock()
		r.sendAccept(f, slot) // 并发下的幂等分支
		return
	}
	slot, ok := r.acquireSlotLocked(f.TaskID, f.AssignmentID, f.ProposedAttempt, f.AssignmentExpiresAt, art)
	if !ok {
		r.mu.Unlock()
		// 没有真实空槽/预算吃满：正式 reject slot_unavailable（不计 attempt、不授 lease）。
		r.send(&protocol.TaskRejectFrame{
			Type:            protocol.FrameTaskReject,
			TaskID:          f.TaskID,
			AssignmentID:    f.AssignmentID,
			ProposedAttempt: f.ProposedAttempt,
			Node:            r.opts.NodeName,
			Reason:          protocol.ReasonSlotUnavailable,
		})
		return
	}
	r.reservations[f.TaskID] = &reservation{
		taskID:       f.TaskID,
		assignmentID: f.AssignmentID,
		proposed:     f.ProposedAttempt,
		namespace:    f.Namespace,
		ref:          f.Capability,
		params:       append([]byte(nil), f.Params...),
		nsVars:       cloneMap(f.NamespaceVars),
		nsRevision:   f.NamespaceVarsRevision,
		cfgRevision:  f.ConfigRevision,
		art:          art,
		slot:         slot,
		created:      r.clk.Now(),
		expiresAt:    f.AssignmentExpiresAt,
	}
	r.mu.Unlock()
	// 不在持锁时发帧（send 自己也要拿锁；持锁发送会自锁）。
	r.sendAccept(f, slot)
}

func (r *Runner) sendAccept(f *protocol.TaskDispatchFrame, slot int) {
	r.send(&protocol.TaskAcceptFrame{
		Type:            protocol.FrameTaskAccept,
		TaskID:          f.TaskID,
		AssignmentID:    f.AssignmentID,
		ProposedAttempt: f.ProposedAttempt,
		Node:            r.opts.NodeName,
		Slot:            &slot,
	})
}

// HandleLeaseGrant 处理 lease.grant：必须匹配 pending reservation 才启动进程。
func (r *Runner) HandleLeaseGrant(f *protocol.LeaseGrantFrame) {
	if err := protocol.ValidateGrant(f); err != nil {
		r.logf("runner: 忽略不合法的 lease.grant：%v", err)
		return
	}
	if f.Node != r.opts.NodeName {
		r.logf("runner: 忽略节点不符的 lease.grant（node=%q）", f.Node)
		return
	}
	r.mu.Lock()
	if r.draining || r.closed {
		r.mu.Unlock()
		return
	}
	res, ok := r.reservations[f.TaskID]
	if !ok {
		r.mu.Unlock()
		r.logf("runner: grant 没有对应的 reserved dispatch（task=%s attempt=%d），不启动",
			f.TaskID, f.Attempt)
		return
	}
	if _, running := r.running[f.TaskID]; running {
		r.mu.Unlock()
		r.logf("runner: task=%s 已在执行，忽略重复 grant（不重复执行）", f.TaskID)
		return
	}
	slotOK := f.Slot != nil && *f.Slot == res.slot && f.AssignmentID == res.assignmentID &&
		f.Attempt == res.proposed
	expired := (!res.expiresAt.IsZero() && !r.clk.Now().Before(res.expiresAt)) || !r.clk.Now().Before(f.ExpiresAt)
	if !slotOK || expired {
		// Only an expired grant with this exact binding owns this reservation.
		// Wrong/old assignments, slots and attempts must not release a newer one.
		if slotOK && expired {
			delete(r.reservations, f.TaskID)
			r.releaseSlotLocked(res.slot, res.art.Metadata.Name)
		}
		r.mu.Unlock()
		r.logf("runner: grant 与 reservation 不匹配/已过期（task=%s attempt=%d slot=%v），不执行",
			f.TaskID, f.Attempt, f.Slot)
		// No execution was authorized locally. Reporting this grant as a result
		// would alias a later legal execution's task/attempt-only result ACK.
		return
	}
	delete(r.reservations, f.TaskID)
	slot := r.ensureSlotLocked(res.slot)
	slot.running = true
	slot.attempt = f.Attempt
	slot.leaseID = f.LeaseID
	slot.reaped = false
	att := newAttempt(r, res, f)
	r.running[f.TaskID] = att
	r.tokens[f.AttemptToken] = att
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		att.run()
	}()
}

// HandleLeaseAck 更新本地 deadline（最后一次 ACK + TTL）；旧 lease 的 ACK 不动新 attempt。
func (r *Runner) HandleLeaseAck(f *protocol.LeaseAckFrame) {
	if f == nil || f.Type != protocol.FrameLeaseAck {
		return
	}
	r.mu.Lock()
	att := r.running[f.TaskID]
	r.mu.Unlock()
	if att == nil {
		return
	}
	if att.attempt != f.Attempt || att.leaseID != f.LeaseID {
		r.logf("runner: 忽略过期 lease.ack（task=%s attempt=%d lease=%s）",
			f.TaskID, f.Attempt, f.LeaseID)
		return
	}
	att.noteAck(r.clk.Now().Add(r.opts.LeaseTTL))
}

// HandleResultAck 匹配 pending 结果（事实已结束，不再重发；accepted=false 只记诊断）。
func (r *Runner) HandleResultAck(f *protocol.ResultAckFrame) {
	if f == nil || f.Type != protocol.FrameResultAck {
		return
	}
	key := resultKey(f.TaskID, f.Attempt)
	r.mu.Lock()
	p, ok := r.pending[key]
	if ok {
		delete(r.pending, key)
		// ACK has task/attempt only on wire; bind it to the pending lease
		// before ending the current attempt, never to an unrelated old result.
		if att := r.running[f.TaskID]; att != nil && att == p.att && att.attempt == f.Attempt && att.leaseID == p.frame.LeaseID {
			att.mu.Lock()
			att.leaseOK = false
			att.emitOpen = false
			att.mu.Unlock()
			att.resultAckOnce.Do(func() { close(att.resultAck) })
			att.cancelUpload()
			// The WS reader can receive a new dispatch immediately after ACK.
			// Make cleanup visible synchronously, only with true physical reap.
			if att.groupReapedNow() {
				r.finishAttemptLocked(att)
			}
		}
	}
	r.mu.Unlock()
	if !ok {
		r.logf("runner: 收到未在等待中的 result.ack（task=%s attempt=%d）", f.TaskID, f.Attempt)
		return
	}
	if !f.Accepted {
		r.logf("runner: 中枢拒绝结果（旧 attempt/lease 已作废）：task=%s attempt=%d",
			f.TaskID, f.Attempt)
	}
}

// HandleCancel 处理 task.cancel：按完整历史绑定定位，不误停同 task 新 attempt。
func (r *Runner) HandleCancel(f *protocol.TaskCancelFrame) {
	if err := protocol.ValidateCancel(f); err != nil {
		r.logf("runner: 忽略不合法的 task.cancel：%v", err)
		return
	}
	r.mu.Lock()
	att := r.running[f.TaskID]
	res := r.reservations[f.TaskID]
	hist := r.history[resultKey(f.TaskID, f.Attempt)]
	r.mu.Unlock()

	slotMatches := func(slot int) bool {
		return *f.Slot == slot
	}

	// 1) 正在执行：必须 attempt+leaseId+slot 全部匹配。
	if att != nil {
		if att.attempt == f.Attempt && att.leaseID == f.LeaseID && slotMatches(att.slot) {
			grace := r.opts.TermGrace
			if f.GraceSec > 0 {
				grace = time.Duration(f.GraceSec) * time.Second
			}
			// Fence authorization synchronously; process reap/ACK remains async.
			att.stopForCancel(f.Reason, grace)
			go func() {
				<-att.done
				r.sendCancelAck(&protocol.CancelAckFrame{
					Type: protocol.FrameCancelAck, TaskID: att.taskID, Attempt: att.attempt,
					LeaseID: att.leaseID, Slot: intPtr(att.slot),
					ProcessGroupReaped: att.groupReapedNow(),
				})
			}()
			return
		}
		// A different current execution must not hide a known historical reap.
		// Report only that exact old binding; never stop/free the current slot.
		reaped := hist != nil && hist.attempt == f.Attempt && hist.leaseID == f.LeaseID && slotMatches(hist.slot) && hist.reaped
		r.sendCancelAck(&protocol.CancelAckFrame{
			Type: protocol.FrameCancelAck, TaskID: f.TaskID, Attempt: f.Attempt,
			LeaseID: f.LeaseID, Slot: f.Slot, ProcessGroupReaped: reaped,
		})
		return
	}

	// 2) 还没 grant 的 reservation：按 assignment 的 proposed attempt + slot 释放。
	if res != nil && res.proposed == f.Attempt && slotMatches(res.slot) {
		r.mu.Lock()
		delete(r.reservations, f.TaskID)
		r.releaseSlotLocked(res.slot, res.art.Metadata.Name)
		r.mu.Unlock()
		r.sendCancelAck(&protocol.CancelAckFrame{
			Type: protocol.FrameCancelAck, TaskID: f.TaskID, Attempt: f.Attempt,
			LeaseID: f.LeaseID, Slot: intPtr(res.slot), ProcessGroupReaped: true,
		})
		return
	}

	// 3) 历史 attempt：已经自然结束/失 lease/已回收 → 只按匹配的历史绑定补报。
	if hist != nil && hist.attempt == f.Attempt && hist.leaseID == f.LeaseID && slotMatches(hist.slot) && hist.reaped {
		r.sendCancelAck(&protocol.CancelAckFrame{
			Type: protocol.FrameCancelAck, TaskID: f.TaskID, Attempt: f.Attempt,
			LeaseID: f.LeaseID, Slot: intPtr(hist.slot), ProcessGroupReaped: true,
		})
		return
	}

	// 4) 本机没有这个绑定：回 reaped=false（不当成物理回收事实）。
	r.sendCancelAck(&protocol.CancelAckFrame{
		Type: protocol.FrameCancelAck, TaskID: f.TaskID, Attempt: f.Attempt,
		LeaseID: f.LeaseID, Slot: f.Slot, ProcessGroupReaped: false,
	})
}

// SessionUp 在每次 WS 连接建立（hello 已发）后重发未确认的事实（结果/取消回执）。
//
// 只重发**已经算出的事实**：不重新执行、不重新计算 result。
func (r *Runner) SessionUp() {
	r.mu.Lock()
	r.sendGeneration++
	generation := r.sendGeneration
	results := make([]*pendingResult, 0, len(r.pending))
	for _, p := range r.pending {
		results = append(results, p)
	}
	cancels := make([]*protocol.CancelAckFrame, 0, len(r.pendingCxl))
	for _, c := range r.pendingCxl {
		cancels = append(cancels, c)
	}
	r.mu.Unlock()
	sort.Slice(results, func(i, j int) bool {
		if results[i].frame.TaskID != results[j].frame.TaskID {
			return results[i].frame.TaskID < results[j].frame.TaskID
		}
		return results[i].frame.Attempt < results[j].frame.Attempt
	})
	for _, p := range results {
		if err := r.sendResultFrame(p, generation); err != nil {
			r.logf("runner: 重发 task.result 失败（保留待下次连接）：%v", err)
			return
		}
	}
	for _, c := range cancels {
		if err := r.send(c); err != nil {
			r.logf("runner: 重发 cancel.ack 失败（保留待下次连接）：%v", err)
			return
		}
		r.mu.Lock()
		delete(r.pendingCxl, resultKey(c.TaskID, c.Attempt))
		r.mu.Unlock()
	}
}

// LookupEmit 按令牌查「当前运行」的 attempt（emit 网关的本地预检）。
//
// token 一旦 attempt 终止/取消就移除：迟到的 emit 得 task_not_running（I8）。
func (r *Runner) LookupEmit(token string) (EmitBinding, bool) {
	if token == "" {
		return EmitBinding{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	att, ok := r.tokens[token]
	if !ok || !att.emitAllowed() {
		return EmitBinding{}, false
	}
	return EmitBinding{
		TaskID:       att.taskID,
		Attempt:      att.attempt,
		LeaseID:      att.leaseID,
		Token:        att.token,
		Next:         append([]string(nil), att.art.Spec.Next...),
		EmitEndpoint: r.opts.EmitEndpoint,
	}, true
}

// Drain 停止接受新派发，并尽力停止所有在跑 attempt（升级 / 退出用）。
func (r *Runner) Drain(ctx context.Context) {
	r.mu.Lock()
	r.draining = true
	running := make([]*Attempt, 0, len(r.running))
	for _, att := range r.running {
		running = append(running, att)
	}
	reservations := make([]*reservation, 0, len(r.reservations))
	for _, res := range r.reservations {
		reservations = append(reservations, res)
	}
	r.reservations = map[string]*reservation{}
	for _, res := range reservations {
		r.releaseSlotLocked(res.slot, res.art.Metadata.Name)
	}
	r.mu.Unlock()
	for _, att := range running {
		att.stopForCancel(protocol.CancelReasonInfra, r.opts.TermGrace)
	}
	for _, att := range running {
		select {
		case <-att.done:
			// Upgrade/exit drain is also a historical physical reap event. Send it
			// synchronously before allowing binary replacement and restart.
			r.sendCancelAck(&protocol.CancelAckFrame{
				Type: protocol.FrameCancelAck, TaskID: att.taskID, Attempt: att.attempt,
				LeaseID: att.leaseID, Slot: intPtr(att.slot),
				ProcessGroupReaped: att.groupReapedNow(),
			})
		case <-ctx.Done():
			return
		}
	}
}

// Close 等待所有 attempt 收尾（进程组回收）。
func (r *Runner) Close(ctx context.Context) {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.recoveryStop)
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// budgetLocked 返回当前有效总槽数（缩预算后仍保留存量占用记录）。
func (r *Runner) budgetLocked() (int, protocol.Limits) {
	total, limits, _ := r.budgetTotalLocked()
	return total, limits
}

// budgetTotalLocked 返回总槽数与「机器级配置是否可信」。
func (r *Runner) budgetTotalLocked() (int, protocol.Limits, bool) {
	_, _, limits, present := r.opts.Applied()
	if !present {
		return 0, limits, false
	}
	total := limits.Total
	if total <= 0 {
		total = protocol.DefaultLimitsTotal
	}
	if total < 0 {
		total = 0
	}
	return total, limits, true
}

// ensureSlotLocked 保证槽位记录存在（不缩容、不丢存量）。
func (r *Runner) ensureSlotLocked(index int) *slotEntry {
	for len(r.slots) <= index {
		r.slots = append(r.slots, &slotEntry{index: len(r.slots)})
	}
	if r.slots[index] == nil {
		r.slots[index] = &slotEntry{index: index}
	}
	return r.slots[index]
}

// acquireSlotLocked 原子预留一个真实空闲槽（只在当前预算范围内分配）。
func (r *Runner) acquireSlotLocked(taskID, assignmentID string, proposed int, expiresAt time.Time, art *artifact.Artifact) (int, bool) {
	total, limits := r.budgetLocked()
	name := art.Metadata.Name
	capLimit := -1
	if v, ok := limits.PerCapability[name]; ok && v > 0 {
		capLimit = v
	} else if v, ok := limits.PerCapability[protocol.LimitsWildcard]; ok && v > 0 {
		capLimit = v
	}
	if maxPerNode := art.Spec.Concurrency.EffectiveMaxPerNode(); maxPerNode > 0 {
		if capLimit < 0 || maxPerNode < capLimit {
			capLimit = maxPerNode
		}
	}
	// The complete physical ledger counts, including reaping/recovered and
	// pregrant slots outside a shrunken budget. Empty low slots are not credits.
	occupied, sameCapability := 0, 0
	for _, slot := range r.slots {
		if slot == nil || slot.taskID == "" {
			continue
		}
		occupied++
		if slot.capability == name {
			sameCapability++
		}
	}
	if occupied >= total || (capLimit >= 0 && sameCapability >= capLimit) {
		return -1, false
	}
	for i := 0; i < total; i++ {
		slot := r.ensureSlotLocked(i)
		if slot.taskID != "" {
			continue
		}
		slot.taskID = taskID
		slot.capability = name
		slot.assignmentID = assignmentID
		slot.proposed = proposed
		slot.reservedAt = r.clk.Now()
		slot.expiresAt = expiresAt
		slot.reaped = false
		return i, true
	}
	return -1, false
}

func (r *Runner) releaseSlotLocked(slot int, _ string) {
	if slot < 0 || slot >= len(r.slots) || r.slots[slot] == nil {
		return
	}
	entry := r.slots[slot]
	entry.taskID = ""
	entry.capability = ""
	entry.assignmentID = ""
	entry.proposed = 0
	entry.attempt = 0
	entry.leaseID = ""
	entry.running = false
	entry.expiresAt = time.Time{}
}

// addHistoryLocked 记录历史回收事实（有界 FIFO）。
func (r *Runner) addHistoryLocked(rec *slotRecord) {
	key := resultKey(rec.taskID, rec.attempt)
	if _, ok := r.history[key]; !ok {
		r.historyOrder = append(r.historyOrder, key)
		if len(r.historyOrder) > maxHistory {
			oldest := r.historyOrder[0]
			r.historyOrder = r.historyOrder[1:]
			delete(r.history, oldest)
		}
	}
	r.history[key] = rec
}

// pruneReservationsLocked 丢弃过期预留（中枢没收到 accept 就重派，不能永远占槽）。
func (r *Runner) pruneReservationsLocked() {
	now := r.clk.Now()
	for id, res := range r.reservations {
		expired := !res.expiresAt.IsZero() && !now.Before(res.expiresAt)
		if !expired {
			continue
		}
		delete(r.reservations, id)
		r.releaseSlotLocked(res.slot, res.art.Metadata.Name)
		r.logf("runner: 预留 task=%s 已过派发窗口，释放槽位 %d（从未启动）", id, res.slot)
	}
}

// finishAttempt 是 attempt 结束时的统一收尾：注销、记录历史、释放槽位。
func (r *Runner) finishAttempt(att *Attempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishAttemptLocked(att)
}

func (r *Runner) finishAttemptLocked(att *Attempt) {
	if cur, ok := r.running[att.taskID]; ok && cur == att {
		delete(r.running, att.taskID)
	}
	if cur, ok := r.tokens[att.token]; ok && cur == att {
		delete(r.tokens, att.token)
	}
	reaped := att.groupReapedNow()
	r.addHistoryLocked(&slotRecord{
		taskID: att.taskID, attempt: att.attempt, leaseID: att.leaseID,
		slot: att.slot, reaped: reaped,
	})
	if reaped {
		if att.slot >= 0 && att.slot < len(r.slots) {
			slot := r.slots[att.slot]
			if slot != nil && slot.running && slot.taskID == att.taskID && slot.attempt == att.attempt && slot.leaseID == att.leaseID {
				r.releaseSlotLocked(att.slot, att.art.Metadata.Name)
			}
		}
	} else {
		r.logf("runner: task=%s attempt=%d 未确认进程组回收，保留物理槽位", att.taskID, att.attempt)
	}
}

func (r *Runner) send(frame any) error {
	r.mu.Lock()
	s := r.sender
	r.mu.Unlock()
	if s == nil {
		return errors.New("runner: 未绑定上行发送器")
	}
	return s.Send(frame)
}

// dropToken 立刻关闭 emit 授权（任务结束 / 取消 / 租约失效）。
func (r *Runner) dropToken(att *Attempt) {
	r.mu.Lock()
	if cur, ok := r.tokens[att.token]; ok && cur == att {
		delete(r.tokens, att.token)
	}
	r.mu.Unlock()
}

// sendCancelAck 发送取消回执；离线时保留事实（重连只重发，不重新收进程）。
func (r *Runner) sendCancelAck(frame *protocol.CancelAckFrame) {
	if err := r.send(frame); err != nil {
		r.mu.Lock()
		r.pendingCxl[resultKey(frame.TaskID, frame.Attempt)] = frame
		r.mu.Unlock()
		r.logf("runner: cancel.ack 发送失败（保留待重连重发）：%v", err)
	}
}

// sendResult records only a result of the unique verified execution. Invalid
// grants cannot create this sending/ACK domain or overwrite its binding.
func (r *Runner) sendResult(frame *protocol.TaskResultFrame) {
	if frame == nil {
		return
	}
	key := resultKey(frame.TaskID, frame.Attempt)
	r.mu.Lock()
	att := r.running[frame.TaskID]
	if att == nil || att.attempt != frame.Attempt || att.leaseID != frame.LeaseID || frame.Node != r.opts.NodeName {
		r.mu.Unlock()
		r.logf("runner: 忽略无唯一执行绑定的 task.result（task=%s attempt=%d）", frame.TaskID, frame.Attempt)
		return
	}
	p := r.pending[key]
	if p == nil {
		p = &pendingResult{frame: frame, att: att}
		r.pending[key] = p
	} else if p.att != att {
		r.mu.Unlock()
		r.logf("runner: 不替换已有 task.result binding（task=%s attempt=%d）", frame.TaskID, frame.Attempt)
		return
	}
	generation := r.sendGeneration
	r.mu.Unlock()
	if err := r.sendResultFrame(p, generation); err != nil {
		r.logf("runner: task.result 发送失败（保留待重连重发）：%v", err)
	}
}

func (r *Runner) sendResultFrame(p *pendingResult, generation uint64) error {
	r.mu.Lock()
	s := r.sender
	r.mu.Unlock()
	guarded, ok := s.(GuardedSender)
	if !ok {
		return errors.New("runner: task.result 需要 GuardedSender（禁止旧 Send fallback）")
	}
	// Use a conservative deadline captured before queuing: renewals may extend
	// authorization, but this write never outlives the earlier authorized window.
	att := p.att
	att.mu.Lock()
	remaining := att.deadline.Sub(r.clk.Now())
	att.mu.Unlock()
	ctx, cancel := context.WithTimeout(att.uploadCtx, remaining)
	defer cancel()
	sent, err := guarded.SendGuarded(ctx, p.frame, func() bool {
		// Lock order: runner -> attempt, same as result ACK. Release both before IO
		// so even a synchronous fake receiver can ACK without recursive deadlock.
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed || generation != r.sendGeneration || r.pending[resultKey(p.frame.TaskID, p.frame.Attempt)] != p || r.running[p.frame.TaskID] != att {
			return false
		}
		att.mu.Lock()
		defer att.mu.Unlock()
		return att.taskID == p.frame.TaskID && att.attempt == p.frame.Attempt && att.leaseID == p.frame.LeaseID &&
			att.leaseOK && att.uploadCtx.Err() == nil && ctx.Err() == nil && r.clk.Now().Before(att.deadline)
	})
	if err == nil && !sent {
		// Do not delete a replacement pending, reap a physical slot, or turn a stale
		// snapshot into an infrastructure/whole-task retry. Lifecycle owns cleanup.
		r.logf("runner: 丢弃失效 task.result 发送快照（task=%s attempt=%d）", p.frame.TaskID, p.frame.Attempt)
	}
	return err
}

// pendingResultCount 是测试/诊断用的待确认结果数。
func (r *Runner) pendingResultCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

func resultKey(taskID string, attempt int) string {
	return fmt.Sprintf("%s/%d", taskID, attempt)
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func intPtr(v int) *int { return &v }

// envInput 组装 attempt 的环境求值输入（快照：启动时取一次）。
func (r *Runner) envInput(att *Attempt) execenv.Input {
	_, vars, _, _ := r.opts.Applied()
	return execenv.Input{
		Artifact:      att.art,
		LocalVars:     r.opts.LocalVars,
		MachineVars:   cloneMap(vars),
		NamespaceVars: cloneMap(att.nsVars),
		Passthrough:   append([]string(nil), r.opts.Passthrough...),
		Baseline:      r.opts.Baseline,
		Facts:         r.opts.Facts,
		NodeName:      r.opts.NodeName,
		Params:        att.params,
		Endpoint:      r.opts.EmitEndpoint,
		TaskToken:     att.token,
		TaskID:        att.taskID,
		// 导出目录在 results.Begin 之后才创建（A2），prepare 里再补注入。
		ArtifactDir: att.exportDir,
	}
}
