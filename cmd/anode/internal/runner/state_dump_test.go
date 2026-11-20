package runner

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

// 失败必须自解释：等待异步事实超时时，把「到底在等谁」写进失败输出，
// 而不是只留一句「等待 X 超时」让人再去猜哪个 goroutine 卡住了。
//
// 这里只做观察，不改任何状态：所有快照都在锁内拷贝，channel 用非阻塞
// select 探测（读走元素会改变被测行为，所以只看 closed 与否）。
const dumpFrameLimit = 16

// describeStateSafe 带看门狗：如果连快照都拿不到（有 goroutine 在持锁做
// 阻塞调用），那本身就是结论 —— 直接报「快照被锁住」并只留 goroutine dump。
func (h *harness) describeStateSafe() string {
	done := make(chan string, 1)
	go func() { done <- h.describeState() }()
	select {
	case s := <-done:
		return s
	case <-time.After(2 * time.Second):
		return "\n  <快照被阻塞：有 goroutine 持锁做阻塞调用，锁住的正是 tryDescribeState 需要的锁>\n  goroutines(包内):\n" +
			runnerGoroutines()
	}
}

func (h *harness) describeState() string {
	var b strings.Builder
	h.r.mu.Lock()
	pending := make([]string, 0, len(h.r.pending))
	attempts := make([]*Attempt, 0, len(h.r.running)+len(h.r.pending))
	seen := map[*Attempt]bool{}
	for key, p := range h.r.pending {
		lease := "<nil>"
		if p.frame != nil {
			lease = p.frame.LeaseID
		}
		pending = append(pending, fmt.Sprintf("%s(lease=%s att=%p)", key, lease, p.att))
		if p.att != nil && !seen[p.att] {
			seen[p.att] = true
			attempts = append(attempts, p.att)
		}
	}
	sort.Strings(pending)
	running := make([]string, 0, len(h.r.running))
	for task, att := range h.r.running {
		running = append(running, fmt.Sprintf("%s→%p", task, att))
		if !seen[att] {
			seen[att] = true
			attempts = append(attempts, att)
		}
	}
	sort.Strings(running)
	reservations := make([]string, 0, len(h.r.reservations))
	for task, res := range h.r.reservations {
		reservations = append(reservations, fmt.Sprintf("%s(slot=%d proposed=%d)", task, res.slot, res.proposed))
	}
	sort.Strings(reservations)
	slots := make([]string, 0, len(h.r.slots))
	for i, s := range h.r.slots {
		if s == nil {
			slots = append(slots, fmt.Sprintf("[%d]=nil", i))
			continue
		}
		slots = append(slots, fmt.Sprintf("[%d] task=%q attempt=%d running=%v reaped=%v lease=%q",
			i, s.taskID, s.attempt, s.running, s.reaped, s.leaseID))
	}
	h.r.mu.Unlock()

	fmt.Fprintf(&b, "\n  runner: pending=%v running=%v reservations=%v slots=%v",
		pending, running, reservations, slots)
	frames := h.sender.snapshot()
	b.WriteString(fmt.Sprintf("\n  frames(%d):", len(frames)))
	if len(frames) > dumpFrameLimit {
		b.WriteString(fmt.Sprintf(" …只列最后 %d 帧", dumpFrameLimit))
		frames = frames[len(frames)-dumpFrameLimit:]
	}
	for _, f := range frames {
		switch fr := f.(type) {
		case *protocol.TaskResultFrame:
			fmt.Fprintf(&b, "\n    task.result task=%s attempt=%d phase=%s lease=%s node=%s summary=%q",
				fr.TaskID, fr.Attempt, fr.Phase, fr.LeaseID, fr.Node, fr.Result.Summary)
			if fr.Result.Error != nil {
				fmt.Fprintf(&b, "\n      error: reason=%q message=%q", fr.Result.Error.Reason, fr.Result.Error.Message)
			}
		case *protocol.LeaseRenewFrame:
			fmt.Fprintf(&b, "\n    lease.renew task=%s attempt=%d lease=%s", fr.TaskID, fr.Attempt, fr.LeaseID)
		default:
			fmt.Fprintf(&b, "\n    %T", f)
		}
	}
	b.WriteString("\n  attempts:")
	if len(attempts) == 0 {
		b.WriteString(" <none>")
	}
	for _, att := range attempts {
		b.WriteString("\n    ")
		b.WriteString(att.describe())
	}
	b.WriteString("\n  goroutines(包内):\n")
	b.WriteString(runnerGoroutines())
	return b.String()
}

// describe 是一条 attempt 的生命周期快照：卡在 spawn / 收管道 / 等 ACK 各有不同签名。
func (a *Attempt) describe() string {
	a.mu.Lock()
	state := fmt.Sprintf("%s/%d att=%p lease=%q phase=%s termReason=%q errCode=%q errMsg=%q terminating=%v leaseOK=%v emitOpen=%v copyErr=%v pipesClosed=%v spawned=%v record=%v workDir=%q deadline=%s",
		a.taskID, a.attempt, a, a.leaseID, a.phase, a.termReason, a.errCode, a.errMsg,
		a.terminating, a.leaseOK, a.emitOpen, a.copyErr, a.pipesClosed, a.proc != nil,
		a.record != nil, a.workDir, a.deadline)
	a.mu.Unlock()
	// 非阻塞探测：只判断通道是否已关闭，绝不从被测通道里读走事实。
	closed := func(name string, ch chan struct{}) string {
		select {
		case <-ch:
			return name + "=closed"
		default:
			return name + "=open"
		}
	}
	return state + " " + closed("procDone", a.procDone) + " " + closed("copiesDone", a.copiesDone) +
		" " + closed("resultAck", a.resultAck) + " " + closed("leaseDone", a.leaseDone) + " " + closed("done", a.done)
}

// runnerGoroutines 只挑与本包相关的 goroutine 栈：全量 dump 会被 net/http 压垮。
func runnerGoroutines() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	var b strings.Builder
	for _, block := range strings.Split(string(buf[:n]), "\n\n") {
		if !strings.Contains(block, "anotify/cmd/anode/internal/runner") {
			continue
		}
		b.WriteString(block)
		b.WriteString("\n\n")
	}
	if b.Len() == 0 {
		return "    <none>\n"
	}
	return b.String()
}
