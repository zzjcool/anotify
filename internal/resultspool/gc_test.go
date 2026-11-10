package resultspool

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// 本文件覆盖契约 §5 的 10..13 条（GC 驱动，fake PurgeStore + 真实 spool）。

// fakePurgeStore 是 PurgeStore 的可控替身：记录每次调用，方便断言「谁被 Ack、谁没被 Ack」。
type fakePurgeStore struct {
	mu        sync.Mutex
	ids       []string
	expireErr error
	ackErrs   map[string]error
	expires   int
	attempts  []string
	acks      []string
	lastLimit int
	lastNow   int64
}

func (f *fakePurgeStore) ExpireObjects(ctx context.Context, now int64, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expires++
	f.lastLimit = limit
	f.lastNow = now
	if f.expireErr != nil {
		return nil, f.expireErr
	}
	return append([]string(nil), f.ids...), nil
}

func (f *fakePurgeStore) AckPurged(ctx context.Context, id string, now int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, id)
	if err := f.ackErrs[id]; err != nil {
		return err
	}
	f.acks = append(f.acks, id)
	return nil
}

func (f *fakePurgeStore) expireCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.expires
}

func (f *fakePurgeStore) ackAttempts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.attempts...)
}

func (f *fakePurgeStore) ackSuccesses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acks...)
}

func (f *fakePurgeStore) limitSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastLimit
}

func (f *fakePurgeStore) nowSeen() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastNow
}

// TestGCRunOncePartialFailure 覆盖 §5.10：3 个过期 id（1 个正常、1 个 Remove 失败=非法 id、
// 1 个 Ack 失败）→ GCReport{Expired:3, Purged:1, Failed:2}、返回 error 非 nil、
// 成功那个文件被删、Remove 失败的那个没有被 Ack。
func TestGCRunOncePartialFailure(t *testing.T) {
	s := newSpool(t)
	writeObject(t, s, "obj_ok", []byte("删除我"))
	writeObject(t, s, "obj_ackfail", []byte("字节删掉但回执失败"))

	const now = int64(1_700_000_000)
	st := &fakePurgeStore{
		ids:     []string{"obj_ok", "../bad-object-id", "obj_ackfail"},
		ackErrs: map[string]error{"obj_ackfail": errors.New("回执写库失败")},
	}
	g, err := NewGC(st, s, GCOptions{
		Interval: time.Minute,
		Limit:    4,
		Now:      func() int64 { return now },
	})
	if err != nil {
		t.Fatalf("NewGC 失败: %v", err)
	}

	rep, runErr := g.RunOnce(context.Background())
	if runErr == nil {
		t.Fatal("存在失败用例却返回 nil error（不许假空成功）")
	}
	want := GCReport{Expired: 3, Purged: 1, Failed: 2}
	if rep != want {
		t.Errorf("GCReport = %+v, want %+v", rep, want)
	}

	okPath, err := s.ObjectPath("obj_ok")
	if err != nil {
		t.Fatalf("ObjectPath(obj_ok) 失败: %v", err)
	}
	if _, err := os.Stat(okPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("成功清理的对象字节还在: %v", err)
	}
	ackPath, err := s.ObjectPath("obj_ackfail")
	if err != nil {
		t.Fatalf("ObjectPath(obj_ackfail) 失败: %v", err)
	}
	if _, err := os.Stat(ackPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Ack 失败的那个字节应已删除（下一轮只会重试回执）: %v", err)
	}

	if got := st.ackAttempts(); len(got) != 2 || got[0] != "obj_ok" || got[1] != "obj_ackfail" {
		t.Errorf("Ack 尝试 = %v, want [obj_ok obj_ackfail]（非法 id 那个绝不能被 Ack）", got)
	}
	if got := st.ackSuccesses(); len(got) != 1 || got[0] != "obj_ok" {
		t.Errorf("Ack 成功 = %v, want [obj_ok]", got)
	}
	if got := st.limitSeen(); got != 4 {
		t.Errorf("传给 ExpireObjects 的 limit = %d, want 4", got)
	}
	if got := st.nowSeen(); got != now {
		t.Errorf("传给 ExpireObjects 的 now = %d, want %d", got, now)
	}
}

// TestGCRunOnceEmpty 覆盖 §5.11：空列表 → GCReport{}、error nil、不调用 Ack；
// 顺带钉住 Limit 缺省值 256 与「ctx 已取消不列举」。
func TestGCRunOnceEmpty(t *testing.T) {
	s := newSpool(t)
	st := &fakePurgeStore{}
	g, err := NewGC(st, s, GCOptions{Interval: time.Minute, Now: func() int64 { return 42 }})
	if err != nil {
		t.Fatalf("NewGC 失败: %v", err)
	}

	rep, err := g.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("空列表 RunOnce 返回错误: %v", err)
	}
	if rep != (GCReport{}) {
		t.Errorf("GCReport = %+v, want 零值", rep)
	}
	if got := st.ackAttempts(); len(got) != 0 {
		t.Errorf("空列表不应调用 Ack，得到 %v", got)
	}
	if got := st.limitSeen(); got != 256 {
		t.Errorf("Limit 缺省值 = %d, want 256", got)
	}
	if got := st.nowSeen(); got != 42 {
		t.Errorf("传给 ExpireObjects 的 now = %d, want 42", got)
	}
	before := st.expireCalls()

	// ctx 已取消：不列举、不动文件，直接返回 ctx 错误。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 ctx 的 RunOnce 错误 = %v, want context.Canceled", err)
	}
	if got := st.expireCalls(); got != before {
		t.Errorf("已取消 ctx 仍调用了 ExpireObjects（%d → %d）", before, got)
	}
}

// TestGCRunOnceExpireErrorTouchesNothing 覆盖 §5.12：ExpireObjects 报错 → 返回 error
// 且一个文件都没动（不删、不 Ack）。
func TestGCRunOnceExpireErrorTouchesNothing(t *testing.T) {
	s := newSpool(t)
	const content = "must survive an unreadable metadata store"
	writeObject(t, s, "obj_keep", []byte(content))
	objPath, err := s.ObjectPath("obj_keep")
	if err != nil {
		t.Fatalf("ObjectPath 失败: %v", err)
	}

	st := &fakePurgeStore{expireErr: errors.New("元数据不可用")}
	g, err := NewGC(st, s, GCOptions{Interval: time.Minute})
	if err != nil {
		t.Fatalf("NewGC 失败: %v", err)
	}
	rep, runErr := g.RunOnce(context.Background())
	if runErr == nil {
		t.Fatal("ExpireObjects 报错时 RunOnce 应返回非 nil error")
	}
	if rep != (GCReport{}) {
		t.Errorf("GCReport = %+v, want 零值（一个都没列举到）", rep)
	}
	b, err := os.ReadFile(objPath)
	if err != nil {
		t.Fatalf("ExpireObjects 失败却动了字节: %v", err)
	}
	if string(b) != content {
		t.Errorf("对象内容被改: %q", b)
	}
	if got := st.ackAttempts(); len(got) != 0 {
		t.Errorf("ExpireObjects 失败时不应 Ack，得到 %v", got)
	}
}

// TestGCRunStopsOnCancelAndValidatesInterval 覆盖 §5.13：Run 在 ctx 取消后
// 一个 interval 内返回 nil；NewGC(Interval<=0) → error（并钉住 nil 依赖的拒绝）。
func TestGCRunStopsOnCancelAndValidatesInterval(t *testing.T) {
	s := newSpool(t)
	st := &fakePurgeStore{}

	if _, err := NewGC(st, s, GCOptions{}); err == nil {
		t.Error("NewGC(Interval=0) 应报错（有界驱动必须有正周期）")
	}
	if _, err := NewGC(st, s, GCOptions{Interval: -time.Millisecond}); err == nil {
		t.Error("NewGC(Interval<0) 应报错")
	}
	if _, err := NewGC(nil, s, GCOptions{Interval: time.Millisecond}); err == nil {
		t.Error("NewGC(nil PurgeStore) 应报错")
	}
	if _, err := NewGC(st, nil, GCOptions{Interval: time.Millisecond}); err == nil {
		t.Error("NewGC(nil spool) 应报错")
	}

	const interval = 20 * time.Millisecond
	g, err := NewGC(st, s, GCOptions{Interval: interval})
	if err != nil {
		t.Fatalf("NewGC 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()

	// 先让它真的跑过至少一轮，再取消。
	time.Sleep(3 * interval)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ctx 取消后 Run 应返回 nil，得到 %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("ctx 取消后 150ms 内 Run 没有返回")
	}
	if got := st.expireCalls(); got == 0 {
		t.Error("Run 在 ticker 上一轮都没跑（驱动没真正工作）")
	}
}
