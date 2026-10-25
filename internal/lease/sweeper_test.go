package lease

import (
	"context"
	"errors"
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
