package lease

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 运行期统计维护（#265）的调度验收。
//
// 关键判据是**频率**：默认 6h 一轮，不能变成每 5s 一次（那是把可选优化做成常态负载）。
// 也不能默认 0 次（那等于没接上）。

type countingStats struct {
	mu    sync.Mutex
	calls int
	ran   [][]string
	cb    func()
}

func (c *countingStats) MaintainStats(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.cb != nil {
		c.cb()
	}
	return []string{`ANALYZE "tasks"`}, nil
}

func (c *countingStats) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestSweeperStatsRunsOnFirstTick：首轮立即跑一次（lastStats 零值 = 从没跑过）。
func TestSweeperStatsRunsOnFirstTick(t *testing.T) {
	st := &countingStats{}
	done := make(chan struct{})
	st.cb = func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}
	s := &Sweeper{Store: &fakeStore{}, Stats: st, Interval: time.Hour}

	s.Tick(context.Background())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("首轮 Tick 没有触发统计维护（#265：接上了却从不跑）")
	}
}

// TestSweeperStatsRespectsInterval：周期内不重复跑。
//
// 没有这道门的话，5s 一次的 Tick 就是 5s 一次 ANALYZE——大表上直接把写锁占满。
func TestSweeperStatsRespectsInterval(t *testing.T) {
	st := &countingStats{}
	s := &Sweeper{Store: &fakeStore{}, Stats: st, Interval: time.Hour}

	s.Tick(context.Background())
	waitCalls(t, st, 1)

	// 紧接着再跑几轮：都该被低频门挡住（默认 6h）
	for i := 0; i < 5; i++ {
		s.Tick(context.Background())
	}
	time.Sleep(50 * time.Millisecond)
	if got := st.get(); got != 1 {
		t.Fatalf("周期内只该跑 1 次，实际 %d 次（低频门失效：每 %v 一次 ANALYZE）", got, s.Interval)
	}
}

// TestSweeperStatsDisabled：StatsInterval < 0 = 禁用，一次都不该跑。
//
// 用负数而不是 0 表示禁用：0 的自然含义是"没设置，用默认"，
// 把它当禁用会让"忘了赋值"变成静默关闭。
func TestSweeperStatsDisabled(t *testing.T) {
	st := &countingStats{}
	s := &Sweeper{Store: &fakeStore{}, Stats: st, Interval: time.Hour, StatsInterval: -1}
	s.Tick(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := st.get(); got != 0 {
		t.Fatalf("已禁用却跑了 %d 次", got)
	}
}

// TestSweeperStatsNoStore：没有 StatsStore（测试 fake / 嵌入式用法）时必须 no-op。
func TestSweeperStatsNoStore(t *testing.T) {
	s := &Sweeper{Store: &fakeStore{}, Interval: time.Hour}
	s.Tick(context.Background()) // 不 panic 即通过
	time.Sleep(20 * time.Millisecond)
}

func waitCalls(t *testing.T, c *countingStats, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.get() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待统计维护被执行超时（want %d, got %d）", want, c.get())
}
