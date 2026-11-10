// Package clock 是节点运行期用到的时钟抽象（租约/超时/GC 都走它）。
//
// 生产默认是真实时间；测试注入假时钟推进 20s / 60s / 5s，而**不把生产默认
// 缩到测试速度**（contract：lease TTL60 / renew20 / TERM grace5 是协议常量）。
//
// 只有两个能力：读当前时间、起一个一次性定时器。没有 Sleep/Ticker：运行器里
// 的周期行为（续租）由「下一次事件时间 = min(续租间隔, 到期时间)」算出，一个
// 定时器就够，避免测试时钟里出现「ticker 自己漂移」的不可控因素。
package clock

import (
	"sync"
	"time"
)

// Timer 是一次性定时器（time.Timer 的子集）。
type Timer interface {
	// C 是到期信号（带缓冲 1，只发一次）。
	C() <-chan time.Time
	// Stop 在到期前取消；返回是否成功取消（已触发 = false）。
	Stop() bool
}

// Clock 是运行期用的时钟。
type Clock interface {
	// Now 是当前时间。
	Now() time.Time
	// NewTimer 在 d 之后触发一次；d <= 0 表示立即（仍异步发信号）。
	NewTimer(d time.Duration) Timer
	// NewTimerAt 在绝对 target 到点触发；target <= Now() 时立即触发。
	NewTimerAt(target time.Time) Timer
}

// Real 是生产时钟（time.Now + time.Timer）。
type Real struct{}

// Now 返回 time.Now()。
func (Real) Now() time.Time { return time.Now() }

// NewTimer 返回真实的 time.Timer。
func (Real) NewTimer(d time.Duration) Timer { return &realTimer{t: time.NewTimer(d)} }

// NewTimerAt 是绝对时点的 best-effort 唤醒；OS 调度/暂停仍可能延后接收。
func (Real) NewTimerAt(target time.Time) Timer {
	return &realTimer{t: time.NewTimer(time.Until(target))}
}

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time { return r.t.C }
func (r *realTimer) Stop() bool          { return r.t.Stop() }

// Fake 是可推进的假时钟（只在测试里用）。
//
// 并发安全：运行器的定时器与测试的 Advance 会在不同 goroutine 上跑。
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// NewFake 从 start 开始构造假时钟。
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now 返回当前（可能被 Advance 推进过的）时间。
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer 登记一个假定时器。
func (f *Fake) NewTimer(d time.Duration) Timer {
	if d < 0 {
		d = 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newTimerAtLocked(f.now.Add(d))
}

// NewTimerAt 比较和注册都在同一锁内，注册延迟不会移动 target。
func (f *Fake) NewTimerAt(target time.Time) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newTimerAtLocked(target)
}

func (f *Fake) newTimerAtLocked(target time.Time) Timer {
	t := &fakeTimer{
		clock: f,
		due:   target,
		ch:    make(chan time.Time, 1),
	}
	if !target.After(f.now) {
		// Buffered delivery needs no Advance or receiver scheduling. Mark it
		// fired under the same lock so Stop/Pending see a settled one-shot.
		t.fired = true
		t.ch <- f.now
	} else {
		f.timers = append(f.timers, t)
	}
	return t
}

// Advance 把时间往前走 d，并按到期顺序触发定时器。
//
// 触发时把定时器从表里摘掉（一次性）。同一个定时器只发一次信号（缓冲 1）。
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	if d > 0 {
		f.now = f.now.Add(d)
	}
	now := f.now
	var due []*fakeTimer
	remain := f.timers[:0]
	for _, t := range f.timers {
		if t.stopped {
			continue
		}
		if !t.fired && !now.Before(t.due) {
			due = append(due, t)
			continue
		}
		remain = append(remain, t)
	}
	f.timers = remain
	f.mu.Unlock()

	for _, t := range due {
		t.fire(now)
	}
}

// Pending 返回还没触发的定时器数量（测试诊断用）。
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

type fakeTimer struct {
	clock   *Fake
	due     time.Time
	ch      chan time.Time
	fired   bool
	stopped bool
	once    sync.Once
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

func (t *fakeTimer) fire(now time.Time) {
	t.clock.mu.Lock()
	if t.stopped || t.fired {
		t.clock.mu.Unlock()
		return
	}
	t.fired = true
	t.clock.mu.Unlock()
	t.once.Do(func() { t.ch <- now })
}
