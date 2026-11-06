package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// fakeStore 是最小 fake：只实现 sweeper 用到的两个方法。
type fakeStore struct {
	swept     []string
	sweepErr  error
	pending   []*store.Task
	listErr   error
	sweepCall int
	lastNow   int64
}

func (f *fakeStore) SweepExpiredLeases(ctx context.Context, now int64) ([]string, error) {
	f.sweepCall++
	f.lastNow = now
	if f.sweepErr != nil {
		return nil, f.sweepErr
	}
	return f.swept, nil
}

func (f *fakeStore) ListRedispatchable(ctx context.Context, now int64, limit int) ([]*store.Task, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.pending) > limit {
		return f.pending[:limit], nil
	}
	return f.pending, nil
}

// TestSweeperTickSweepsAndRedispatches 验证一轮 tick 的两阶段都执行。
func TestSweeperTickSweepsAndRedispatches(t *testing.T) {
	fs := &fakeStore{
		swept: []string{"tsk_expired"},
		pending: []*store.Task{
			{ID: "tsk_a", Status: store.TaskPending},
			{ID: "tsk_b", Status: store.TaskPending},
		},
	}
	var dispatched []string
	sw := New(fs)
	sw.Redispatch = func(ctx context.Context, task *store.Task) (bool, error) {
		dispatched = append(dispatched, task.ID)
		return true, nil
	}

	sw.Tick(context.Background())

	if fs.sweepCall != 1 {
		t.Fatalf("sweep 应调用 1 次，实际 %d", fs.sweepCall)
	}
	if len(dispatched) != 2 || dispatched[0] != "tsk_a" || dispatched[1] != "tsk_b" {
		t.Fatalf("应重派 2 条，实际 %v", dispatched)
	}
}

// TestSweeperContinuesWhenSweepFails 验证 sweeper 单轮失败不会停摆。
//
// 这是自愈机制最关键的性质：一次 DB 抖动不能让所有租约永远不被回收。
func TestSweeperContinuesWhenSweepFails(t *testing.T) {
	fs := &fakeStore{
		sweepErr: errors.New("db is busy"),
		pending:  []*store.Task{{ID: "tsk_ok"}},
	}
	var dispatched int
	sw := New(fs)
	sw.Redispatch = func(ctx context.Context, task *store.Task) (bool, error) {
		dispatched++
		return true, nil
	}

	sw.Tick(context.Background())

	if dispatched != 1 {
		t.Fatalf("sweep 失败不应阻塞重派，实际 dispatched=%d", dispatched)
	}
}

// TestSweeperSkipsWhenNoNode 验证「派不出去」不算错误，也不 fanout 报错。
func TestSweeperSkipsWhenNoNode(t *testing.T) {
	fs := &fakeStore{pending: []*store.Task{{ID: "tsk_x"}}}
	sw := New(fs)
	sw.Redispatch = func(ctx context.Context, task *store.Task) (bool, error) {
		return false, nil // 没有可用节点
	}

	sw.Tick(context.Background()) // 不应 panic / 死循环
}

// TestSweeperListErrorDoesNotPanic 验证列 pending 失败时干净返回。
func TestSweeperListErrorDoesNotPanic(t *testing.T) {
	fs := &fakeStore{listErr: errors.New("boom")}
	sw := New(fs)
	sw.Redispatch = func(ctx context.Context, task *store.Task) (bool, error) {
		t.Fatal("不该被调用")
		return false, nil
	}
	sw.Tick(context.Background())
}

// TestSweeperRespectsBatchLimit 验证每轮重派有条数上限（防积压压垮节点）。
func TestSweeperRespectsBatchLimit(t *testing.T) {
	var many []*store.Task
	for i := 0; i < 10; i++ {
		many = append(many, &store.Task{ID: "t" + string(rune('0'+i)), Status: store.TaskPending})
	}
	fs := &fakeStore{pending: many}
	sw := New(fs)
	sw.BatchLimit = 3
	var n int
	sw.Redispatch = func(ctx context.Context, task *store.Task) (bool, error) {
		n++
		return true, nil
	}

	sw.Tick(context.Background())

	if n != 3 {
		t.Fatalf("BatchLimit=3 时应只派 3 条，实际 %d", n)
	}
}

// TestSweeperRedispatchNilIsNoop 验证没配 Redispatch 时只做回收。
func TestSweeperRedispatchNilIsNoop(t *testing.T) {
	fs := &fakeStore{pending: []*store.Task{{ID: "t"}}}
	sw := New(fs)
	// 故意不设 Redispatch
	sw.Tick(context.Background())
	if fs.sweepCall != 1 {
		t.Fatalf("仍应执行回收阶段")
	}
}

// TestSweeperRunStopsOnContextCancel 验证 ctx 取消能干净退出（不泄漏 goroutine）。
func TestSweeperRunStopsOnContextCancel(t *testing.T) {
	fs := &fakeStore{}
	sw := New(fs)
	sw.Interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sw.Run(ctx)
		close(done)
	}()

	time.Sleep(35 * time.Millisecond) // 让它跑几轮
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}

	if fs.sweepCall == 0 {
		t.Fatal("Run 期间应至少扫过一次")
	}
}

// TestRetryBackoffSequence 验证退避序列（契约 §3.3：10/60/300）。
func TestRetryBackoffSequence(t *testing.T) {
	cases := []struct {
		attempts int
		want     int64
	}{
		{0, 0},
		{1, 10},
		{2, 60},
		{3, 300},
		{4, 300},  // 超出序列用最后一个
		{99, 300}, // 不无限增长
	}
	for _, c := range cases {
		if got := store.RetryBackoff(c.attempts); got != c.want {
			t.Errorf("RetryBackoff(%d) = %d, want %d", c.attempts, got, c.want)
		}
	}
}

// ---------- 即时重派（#219） ----------
//
// 病根：pending 任务原本只能靠 5s 巡检排空。槽位空出来的那一刻没有任何东西去拉下一条，
// 单槽节点实测有效吞吐 1.2 task/s（单槽理想值 ≈64 task/s）。

// countingRedispatch 记录被重派的条数（线程安全：Run 在自己的 goroutine 里跑）。
type countingRedispatch struct {
	mu    sync.Mutex
	count int
	ch    chan struct{}
}

func (c *countingRedispatch) fn(ctx context.Context, t *store.Task) (bool, error) {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	select {
	case c.ch <- struct{}{}:
	default:
	}
	return true, nil
}

func (c *countingRedispatch) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// TestSweeperNudgeRedispatchesImmediately 是这条 issue 的核心断言：
// 巡检周期设成 1 小时，**只有** Nudge 能让它干活。
func TestSweeperNudgeRedispatchesImmediately(t *testing.T) {
	fs := &fakeStore{pending: []*store.Task{{ID: "tsk_a", Status: store.TaskPending}}}
	rd := &countingRedispatch{ch: make(chan struct{}, 1)}
	s := &Sweeper{
		Store:      fs,
		Redispatch: rd.fn,
		Interval:   time.Hour, // 关键：不 Nudge 就要等一小时
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// 不 Nudge：给 100ms，必须一条都没派（证明"只有巡检"确实不动）
	select {
	case <-rd.ch:
		t.Fatal("没有任何触发就派发了：巡检周期是 1h，这里不该有动作")
	case <-time.After(100 * time.Millisecond):
	}

	s.Nudge()
	select {
	case <-rd.ch:
		// 派发了 ✓
	case <-time.After(2 * time.Second):
		t.Fatal("Nudge 之后没有重派：槽位释放没有触发即时拉取（#219 回归）")
	}
}

// TestSweeperNudgeCoalesces 验证 Nudge 是**可合并**的：
// 高吞吐下每个任务完成都会 Nudge 一次，不能变成每个任务都跑一次查询。
func TestSweeperNudgeCoalesces(t *testing.T) {
	fs := &fakeStore{pending: []*store.Task{{ID: "tsk_a", Status: store.TaskPending}}}
	rd := &countingRedispatch{ch: make(chan struct{}, 1)}
	s := &Sweeper{
		Store:      fs,
		Redispatch: rd.fn,
		Interval:   time.Hour,
		spurMinGap: 200 * time.Millisecond,
	}
	// 先放一个令牌（模拟"已经有一次待处理"），后续 1000 次 Nudge 必须全部被合并掉
	s.Nudge()
	for i := 0; i < 1000; i++ {
		s.Nudge()
	}
	if got := len(s.spurCh()); got > 1 {
		t.Fatalf("信号通道最多只该有 1 个待处理信号，实际 %d", got)
	}
}

// TestSweeperNudgeRateLimited 验证两次即时重派之间有最小间隔：
// 没有限频的话，高吞吐下每次任务完成都会触发一次查询，把 SQLite 的写队列打满。
func TestSweeperNudgeRateLimited(t *testing.T) {
	fs := &fakeStore{pending: []*store.Task{{ID: "tsk_a", Status: store.TaskPending}}}
	rd := &countingRedispatch{ch: make(chan struct{}, 1)}
	s := &Sweeper{
		Store:      fs,
		Redispatch: rd.fn,
		Interval:   time.Hour,
		spurMinGap: 300 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	start := time.Now()
	s.Nudge()
	<-rd.ch // 第一次

	// 立刻再 Nudge 若干次：都该被限频压在同一轮里
	for i := 0; i < 5; i++ {
		s.Nudge()
	}
	select {
	case <-rd.ch:
		if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
			t.Fatalf("第二次重派距第一次只有 %v，限频没生效", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("限频之后仍然应该在间隔到期后补跑一次（不能把信号吃掉）")
	}
}

// TestSweeperNudgeWithoutRedispatch：没配 Redispatch 时 Nudge 必须安全（不 panic、不卡）。
func TestSweeperNudgeWithoutRedispatch(t *testing.T) {
	s := &Sweeper{Store: &fakeStore{}, Interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	s.Nudge()
	time.Sleep(50 * time.Millisecond) // 不 panic 即通过
}
