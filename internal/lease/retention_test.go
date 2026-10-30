package lease

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// fakeRetentionStore 同时实现 Store 与 RetentionStore（契约 §5 第 11~14 条）。
//
// 为什么不复用 sweeper_test.go 的 fakeStore：那边不记录 list 调用次数，
// 而「失败不停摆」需要证明 Tick 走完了重派阶段。fakeStore 保持原样不动。
type fakeRetentionStore struct {
	// Store 部分
	swept     []string
	pending   []*store.Task
	sweepCall int
	listCall  int

	// RetentionStore 部分
	retentionCall int
	lastOpts      store.RetentionOptions
	report        store.RetentionReport
	retErr        error
}

func (f *fakeRetentionStore) SweepExpiredLeases(ctx context.Context, now int64) ([]string, error) {
	f.sweepCall++
	return f.swept, nil
}

func (f *fakeRetentionStore) ListRedispatchable(ctx context.Context, now int64, limit int) ([]*store.Task, error) {
	f.listCall++
	if len(f.pending) > limit {
		return f.pending[:limit], nil
	}
	return f.pending, nil
}

func (f *fakeRetentionStore) SweepRetention(ctx context.Context, opts store.RetentionOptions) (store.RetentionReport, error) {
	f.retentionCall++
	f.lastOpts = opts
	if f.retErr != nil {
		return store.RetentionReport{}, f.retErr
	}
	return f.report, nil
}

// 契约 §5-11：Retention == nil → no-op。
//
// 名字很重要：这测的是「Retention 被置 nil 时不做清理」这条**接口语义**，
// 不是「默认关闭」——生产路径 lease.New(*store.DB) 会自动接线，默认是**开启**的。
// 这里的 nil 是测试自己手动置的（下一行），叫 default 会误导读者以为线上不清理。
//
// 同时验证：不做清理时租约回收/重派两个阶段照常（清理不能影响巡检本身）。
func TestRetentionNilIsNoop(t *testing.T) {
	fs := &fakeRetentionStore{pending: []*store.Task{{ID: "tsk_a"}}}
	sw := New(fs)
	sw.Retention = nil // 显式关闭（New 会自动接线，这里模拟「只实现 Store」的场景）
	var dispatched int
	sw.Redispatch = func(ctx context.Context, task *store.Task) (bool, error) {
		dispatched++
		return true, nil
	}

	sw.Tick(context.Background())
	sw.retentionWG.Wait() // 异步清理：先等它收尾再断言（没启动时立即返回）

	if fs.retentionCall != 0 {
		t.Fatalf("Retention=nil 时不该清理，实际调用 %d 次", fs.retentionCall)
	}
	if fs.sweepCall != 1 {
		t.Fatalf("回收阶段应照常执行 1 次，实际 %d", fs.sweepCall)
	}
	if fs.listCall != 1 || dispatched != 1 {
		t.Fatalf("重派阶段应照常执行，实际 list=%d dispatched=%d", fs.listCall, dispatched)
	}
}

// 契约 §5-12：首轮执行 + 频率门。
//
// 首次 Tick（lastRetention==0）立即执行一次排积压；now 不推进再 Tick 仍是 1 次；
// 推进满一个周期（1 小时）后 → 2 次。
func TestRetentionFirstTickThenHourlyGate(t *testing.T) {
	var fakeNow int64 = 1_700_000_000
	fs := &fakeRetentionStore{}
	sw := New(fs)
	sw.Now = func() int64 { return fakeNow }

	sw.Tick(context.Background())
	sw.retentionWG.Wait() // 异步：等这一轮清理跑完再断言
	if fs.retentionCall != 1 {
		t.Fatalf("首轮应立即执行 1 次，实际 %d", fs.retentionCall)
	}
	if fs.lastOpts.Now != fakeNow {
		t.Fatalf("传给 SweepRetention 的 Now 应是注入值 %d，实际 %d", fakeNow, fs.lastOpts.Now)
	}
	if fs.lastOpts.Batch != store.DefaultRetentionBatch || fs.lastOpts.Batches != store.DefaultRetentionBatches {
		t.Fatalf("Batch/Batches 应回落到默认常量 %d/%d，实际 %d/%d",
			store.DefaultRetentionBatch, store.DefaultRetentionBatches,
			fs.lastOpts.Batch, fs.lastOpts.Batches)
	}

	sw.Tick(context.Background()) // now 未推进
	sw.retentionWG.Wait()
	if fs.retentionCall != 1 {
		t.Fatalf("同 now 内不该重复清理，实际 %d 次", fs.retentionCall)
	}

	fakeNow += int64(DefaultRetentionInterval.Seconds()) - 1
	sw.Tick(context.Background()) // 差 1 秒，还不到点
	sw.retentionWG.Wait()
	if fs.retentionCall != 1 {
		t.Fatalf("未满 1 小时不该清理，实际 %d 次", fs.retentionCall)
	}

	fakeNow++
	sw.Tick(context.Background()) // 刚好满 1 小时
	sw.retentionWG.Wait()
	if fs.retentionCall != 2 {
		t.Fatalf("满 1 小时后应再执行 1 次（共 2 次），实际 %d", fs.retentionCall)
	}
	if fs.lastOpts.Now != fakeNow {
		t.Fatalf("第二轮 Now 应是推进后的 %d，实际 %d", fakeNow, fs.lastOpts.Now)
	}
}

// 契约 §5-13：失败不停摆。
//
// SweepRetention 返回 error 时：Tick 仍走完回收与重派；lastRetention 已推进
// （失败也记时刻，避免每 5s 重试打爆库），推进满一个周期后才再试。
func TestRetentionFailureDoesNotStallTick(t *testing.T) {
	var fakeNow int64 = 1_700_000_000
	fs := &fakeRetentionStore{
		retErr:  errors.New("db is locked"),
		pending: []*store.Task{{ID: "tsk_ok"}},
	}
	sw := New(fs)
	sw.Now = func() int64 { return fakeNow }
	var dispatched int
	sw.Redispatch = func(ctx context.Context, task *store.Task) (bool, error) {
		dispatched++
		return true, nil
	}

	sw.Tick(context.Background()) // 不该 panic，也不该中断巡检
	sw.retentionWG.Wait()         // 异步清理：等结果落定再断言（失败也在 goroutine 里）

	if fs.retentionCall != 1 {
		t.Fatalf("应尝试清理 1 次，实际 %d", fs.retentionCall)
	}
	if fs.sweepCall != 1 || fs.listCall != 1 || dispatched != 1 {
		t.Fatalf("清理失败不应影响巡检阶段：sweep=%d list=%d dispatched=%d",
			fs.sweepCall, fs.listCall, dispatched)
	}
	if sw.lastRetention != fakeNow {
		t.Fatalf("失败后也应写回 lastRetention=%d，实际 %d", fakeNow, sw.lastRetention)
	}

	sw.Tick(context.Background()) // 未到周期，不重试
	sw.retentionWG.Wait()
	if fs.retentionCall != 1 {
		t.Fatalf("失败后未满周期不该重试，实际 %d 次", fs.retentionCall)
	}

	fakeNow += int64(DefaultRetentionInterval.Seconds())
	sw.Tick(context.Background()) // 到点才再试
	sw.retentionWG.Wait()
	if fs.retentionCall != 2 {
		t.Fatalf("满周期后应再试 1 次（共 2 次），实际 %d", fs.retentionCall)
	}
	if fs.sweepCall != 3 || fs.listCall != 3 {
		t.Fatalf("三条 tick 的巡检阶段都应执行：sweep=%d list=%d", fs.sweepCall, fs.listCall)
	}
}

// 契约 §5-14：New 自动接线。
//
// 同时实现 Store 与 RetentionStore → 启用；只实现 Store（sweeper_test.go 的
// fakeStore，不改它）→ 保持关闭（nil）。
func TestNewAutoWiresRetention(t *testing.T) {
	sw := New(&fakeRetentionStore{})
	if sw.Retention == nil {
		t.Fatal("实现 RetentionStore 的 store 应被自动接线，实际 nil")
	}

	swWithout := New(&fakeStore{})
	if swWithout.Retention != nil {
		t.Fatal("只实现 Store 的 fakeStore 不该启用保留清理，实际非 nil")
	}

	// 零值回落（契约 §4.2 的三条 0 → 默认）。
	if got := sw.retentionInterval(); got != DefaultRetentionInterval {
		t.Fatalf("RetentionInterval 零值应回落到 %v，实际 %v", DefaultRetentionInterval, got)
	}
	if got := sw.retentionBatch(); got != store.DefaultRetentionBatch {
		t.Fatalf("RetentionBatch 零值应回落到 %d，实际 %d", store.DefaultRetentionBatch, got)
	}
	if got := sw.retentionBatches(); got != store.DefaultRetentionBatches {
		t.Fatalf("RetentionBatches 零值应回落到 %d，实际 %d", store.DefaultRetentionBatches, got)
	}
}

// 额外：真实 store（非 fake）的装配路径——证明进程里 lease.New(*store.DB)
// 真的会把清理打开，而不是因为类型断言失败静默降级成「永远不清理」。
//
// 另有一条编译期断言（retention.go 里的 var _ RetentionStore = (*store.DB)(nil)）
// 负责在签名漂移时直接编译失败；这里补的是「运行时对象确实被接上」这一环。
func TestNewWiresRealStoreRetention(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer db.Close()

	sw := New(db)
	if sw.Retention == nil {
		t.Fatal("*store.DB 应实现 RetentionStore，实际 New 后 Retention == nil（清理被静默关闭）")
	}
	// 接口相等：同一 dynamic type（*store.DB）+ 同一指针 ⇒ 相等。
	if sw.Retention != RetentionStore(db) {
		t.Fatal("Retention 应就是传入的 *store.DB 本身")
	}

	// 空库上跑一拍：不该 panic（真实 SQL 路径也算被覆盖到了）。
	sw.Tick(context.Background())
	sw.retentionWG.Wait() // 异步：等真实 SQL 跑完（否则下面直接再清一次，两路并发打同一个内存库）

	report, err := sw.Retention.SweepRetention(context.Background(), store.RetentionOptions{Now: store.Now()})
	if err != nil {
		t.Fatalf("空库清理不该报错: %v", err)
	}
	if report.TasksDeleted != 0 || report.EventsDeleted != 0 || report.Truncated {
		t.Fatalf("空库应删 0 行且不截断，实际 %+v", report)
	}
}

// 额外：显式配置的周期/批量应覆盖默认值（部署时可调，不必改代码）。
func TestRetentionOverrides(t *testing.T) {
	var fakeNow int64 = 1_700_000_000
	fs := &fakeRetentionStore{}
	sw := New(fs)
	sw.Now = func() int64 { return fakeNow }
	sw.RetentionInterval = 10 * time.Second
	sw.RetentionBatch = 7
	sw.RetentionBatches = 2

	sw.Tick(context.Background())
	sw.retentionWG.Wait()
	if fs.lastOpts.Batch != 7 || fs.lastOpts.Batches != 2 {
		t.Fatalf("应使用显式批量 7/2，实际 %d/%d", fs.lastOpts.Batch, fs.lastOpts.Batches)
	}

	fakeNow += 10 // 满 10s
	sw.Tick(context.Background())
	sw.retentionWG.Wait()
	if fs.retentionCall != 2 {
		t.Fatalf("周期 10s 时应执行 2 次，实际 %d", fs.retentionCall)
	}
}

// blockingRetentionStore 的 SweepRetention 会一直阻塞到测试 close(release) 才返回。
//
// 用途（契约 §5-18 / 返工单 B1、B2）：证明「清理慢不拖 Tick」和「重叠守卫」。
// 计数一律用 atomic：这些方法在清理 goroutine 里跑，测试 goroutine 同时会读，
// 普通 int 在 -race 下就是数据竞争。
type blockingRetentionStore struct {
	release chan struct{} // close 后放行所有轮的 SweepRetention
	entered chan struct{} // 每轮进入 SweepRetention 时发一个（缓冲 4，守卫失效也不会堵死）

	calls     atomic.Int32 // SweepRetention 进入次数
	active    atomic.Int32 // 当前正在跑的轮数
	maxActive atomic.Int32 // active 的历史峰值（>1 说明清理重叠了）
	sweepCall atomic.Int32 // Store.SweepExpiredLeases 调用次数
	listCall  atomic.Int32 // Store.ListRedispatchable 调用次数
}

func newBlockingRetentionStore() *blockingRetentionStore {
	return &blockingRetentionStore{
		release: make(chan struct{}),
		entered: make(chan struct{}, 4),
	}
}

func (b *blockingRetentionStore) SweepExpiredLeases(ctx context.Context, now int64) ([]string, error) {
	b.sweepCall.Add(1)
	return nil, nil
}

func (b *blockingRetentionStore) ListRedispatchable(ctx context.Context, now int64, limit int) ([]*store.Task, error) {
	b.listCall.Add(1)
	return nil, nil
}

func (b *blockingRetentionStore) SweepRetention(ctx context.Context, opts store.RetentionOptions) (store.RetentionReport, error) {
	cur := b.active.Add(1)
	defer b.active.Add(-1)
	for { // 记录并发峰值：CAS 循环，避免两轮同时更新丢峰值
		prev := b.maxActive.Load()
		if cur <= prev || b.maxActive.CompareAndSwap(prev, cur) {
			break
		}
	}
	b.calls.Add(1)
	b.entered <- struct{}{}
	<-b.release // 卡住，直到测试放行
	return store.RetentionReport{}, nil
}

// 契约 §5-18（返工单 B1）：清理是**异步**的——清理再慢也不许拖住 5s 巡检。
//
// 判定分三步：
//  1. 先确认清理真的进了 SweepRetention 并卡在 `<-release` 上（否则「Tick 先返回」没意义）；
//  2. 清理此刻卡着时，Tick 必须已经返回（200ms 内，超时即失败）；
//  3. 放行后 Wait 到清理结束，SweepRetention 调用次数正好 1（不漏跑、不重复跑）。
func TestRetentionDoesNotBlockTick(t *testing.T) {
	bs := newBlockingRetentionStore()
	sw := New(bs)
	sw.Redispatch = func(ctx context.Context, task *store.Task) (bool, error) { return true, nil }

	tickDone := make(chan struct{})
	go func() {
		sw.Tick(context.Background())
		close(tickDone)
	}()

	// 第一步：先确认清理**真的启动了并卡在 SweepRetention 里**（release 还没关），
	// 排除「清理压根没跑」这种假通过。
	select {
	case <-bs.entered:
	case <-time.After(time.Second):
		close(bs.release)
		sw.retentionWG.Wait()
		t.Fatal("清理 goroutine 迟迟没进入 SweepRetention")
	}
	if got := bs.active.Load(); got != 1 {
		t.Fatalf("此时应有一轮清理正在跑（active=1），实际 %d", got)
	}

	// 第二步：清理此刻还是卡着的（release 未关），Tick 必须已经返回。
	// 同步实现会死在上面那句话里——它卡在清理上，永远走不到 close(tickDone)。
	select {
	case <-tickDone:
	case <-time.After(200 * time.Millisecond):
		// 放行再收尾，别把清理 goroutine 永远挂着（否则 -race 下能跑出 goroutine 泄漏）。
		close(bs.release)
		sw.retentionWG.Wait()
		t.Fatal("Tick 应在清理未完成时就返回，实际被阻塞式清理拖住了（异步化失效）")
	}

	// Tick 已返回，且巡检的回收/重派两个阶段都真跑完了（没被清理拖住）。
	if got := bs.sweepCall.Load(); got != 1 {
		t.Fatalf("回收阶段应执行 1 次，实际 %d", got)
	}
	if got := bs.listCall.Load(); got != 1 {
		t.Fatalf("重派阶段应执行 1 次，实际 %d", got)
	}

	close(bs.release)
	sw.retentionWG.Wait()
	if got := bs.calls.Load(); got != 1 {
		t.Fatalf("一轮巡检应只调用 1 次 SweepRetention，实际 %d", got)
	}
}

// 返工单 B2：重叠守卫——同一时刻只允许一轮清理在跑。
//
// 做法：第一轮卡在 fake 里（确认 active==1）→ 手动把 lastRetention 置 0 逼开低频门
// （否则第二次 Tick 会被「未到周期」挡住，压根测不到守卫）→ 再 Tick。
// 守卫生效时第二次 Tick 不启动任何 goroutine；失效时会有第二轮并发进入，峰值 = 2。
func TestRetentionOverlapGuard(t *testing.T) {
	bs := newBlockingRetentionStore()
	sw := New(bs)
	sw.Redispatch = func(ctx context.Context, task *store.Task) (bool, error) { return true, nil }

	sw.Tick(context.Background()) // 第一轮：启动后卡在 fake 里
	select {
	case <-bs.entered:
	case <-time.After(time.Second):
		close(bs.release)
		sw.retentionWG.Wait()
		t.Fatal("第一轮清理没进入 SweepRetention")
	}
	if got := bs.active.Load(); got != 1 {
		close(bs.release)
		sw.retentionWG.Wait()
		t.Fatalf("第一轮应在跑（active=1），实际 %d", got)
	}

	sw.lastRetention = 0 // 逼开低频门
	sw.Tick(context.Background())

	// 给可能的第二轮一点时间进入：守卫失效时它会在卡住之前把 active 加到 2。
	deadline := time.Now().Add(100 * time.Millisecond)
	for bs.calls.Load() == 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	close(bs.release)
	sw.retentionWG.Wait() // 守卫失效时会等到两轮，不会挂住（release 已关）

	if got := bs.maxActive.Load(); got != 1 {
		t.Fatalf("清理不应重叠：并发峰值应为 1，实际 %d（重叠守卫失效）", got)
	}
	if got := bs.calls.Load(); got != 1 {
		t.Fatalf("上一轮未完成时本轮应被跳过，实际累计进入 %d 轮", got)
	}
	// 守卫必须在轮次结束后自动解除，否则之后永远不会再清理。
	if sw.retentionBusy.Load() {
		t.Fatal("清理结束后 retentionBusy 应已复位")
	}

	// 复跑一次：证明守卫没把清理永久锁死（release 已关，这一轮立即返回）。
	sw.lastRetention = 0
	sw.Tick(context.Background())
	sw.retentionWG.Wait()
	if got := bs.calls.Load(); got != 2 {
		t.Fatalf("上一轮结束后应能再跑一轮（累计 2 轮），实际 %d", got)
	}
}

// capturingRetentionStore 是变更单 A3 要求的 capturing fake：把每次收到的
// store.RetentionOptions 原样存下来（而不是只留「最后一次」）。
//
// 为什么不复用 fakeRetentionStore：这条测的是「lease 层交给 store 的参数里
// EventRetentionSec 永远是 0（= 禁用）」——属于**默认值回归**，将来有人给 Sweeper
// 加个字段（env / orgs 列）把默认改成开启，这条必须立刻红。
// 用 mutex 而不是裸字段：SweepRetention 在清理 goroutine 里写，测试 goroutine 读。
type capturingRetentionStore struct {
	mu   sync.Mutex
	opts []store.RetentionOptions
}

func (c *capturingRetentionStore) SweepExpiredLeases(ctx context.Context, now int64) ([]string, error) {
	return nil, nil
}

func (c *capturingRetentionStore) ListRedispatchable(ctx context.Context, now int64, limit int) ([]*store.Task, error) {
	return nil, nil
}

func (c *capturingRetentionStore) SweepRetention(ctx context.Context, opts store.RetentionOptions) (store.RetentionReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts = append(c.opts, opts)
	// 模拟 store 侧契约（§4.1）：EventRetentionSec <= 0 ⇒ 跳过整个 events 阶段。
	return store.RetentionReport{EventsDisabled: opts.EventRetentionSec <= 0}, nil
}

func (c *capturingRetentionStore) capturedOpts() []store.RetentionOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]store.RetentionOptions(nil), c.opts...)
}

// 变更单 A3（本单真正的验收）：**lease 层永远不会自己开启 events 清理**。
//
// 做法：capturing fake 记下收到的 store.RetentionOptions，跑一轮 Tick + retentionWG.Wait()，
// 然后断言 EventRetentionSec == 0（0 = store.EventRetentionDisabled = 禁用，不是「保留 0 秒」），
// 同时 Now 真有值、Batch/Batches 回落到 store 默认常量（证明其余参数没被顺手写坏）。
//
// 为什么值得单独一条：events 删除不可逆，本版刻意不给 Sweeper 加 EventRetentionSec 开关，
// 那「升级不删 events」的唯一保证就是「lease 交给 store 的 EventRetentionSec 恒为 0」。
func TestRetentionNeverEnablesEventSweep(t *testing.T) {
	var fakeNow int64 = 1_700_000_000
	cs := &capturingRetentionStore{}
	sw := New(cs)
	sw.Now = func() int64 { return fakeNow }

	sw.Tick(context.Background())
	sw.retentionWG.Wait() // 异步：等这轮清理跑完，再看它收到了什么

	got := cs.capturedOpts()
	if len(got) != 1 {
		t.Fatalf("一轮 Tick 应向 store 发起 1 次清理，实际 %d 次", len(got))
	}
	opts := got[0]
	if opts.EventRetentionSec != 0 {
		t.Fatalf("lease 层不该注入 events 保留期（0 = 禁用），实际 EventRetentionSec=%d",
			opts.EventRetentionSec)
	}
	// 与冻结常量对一下：0 的含义是 EventRetentionDisabled，避免哪天常量被改成别的值。
	if opts.EventRetentionSec != store.EventRetentionDisabled {
		t.Fatalf("EventRetentionSec 应等于 store.EventRetentionDisabled（%d），实际 %d",
			store.EventRetentionDisabled, opts.EventRetentionSec)
	}
	// 「lease 不注入 ⇒ 禁用」这条链依赖 store 侧的「0 = 禁用」默认值：
	// 如果有人把 store 默认改回正数，上面的断言照样绿，但线上就会开始删 events。
	if store.DefaultEventRetentionSec != store.EventRetentionDisabled {
		t.Fatalf("store 默认 events 保留期必须是禁用（%d），实际 %d——lease 不注入就会变成「默认开启」",
			store.EventRetentionDisabled, store.DefaultEventRetentionSec)
	}
	if opts.Now != fakeNow {
		t.Fatalf("Now 应是注入的 %d，实际 %d", fakeNow, opts.Now)
	}
	if opts.Batch != store.DefaultRetentionBatch || opts.Batches != store.DefaultRetentionBatches {
		t.Fatalf("Batch/Batches 应回落到默认常量 %d/%d，实际 %d/%d",
			store.DefaultRetentionBatch, store.DefaultRetentionBatches, opts.Batch, opts.Batches)
	}
}

// ---------- #162 验收：保留期清理按命名空间生效 ----------

// mkNsCapability 在一个命名空间里建一个声明了 lifecycle.resultTtlSec 的能力版本。
func mkNsCapability(t *testing.T, db *store.DB, namespaceID, name string, ttlSec int) *store.CapabilityVersion {
	t.Helper()
	c := &store.CapabilityVersion{
		NamespaceID:  namespaceID,
		Name:         name,
		Version:      1,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Source:       store.CapSourceCustom,
		Lifecycle:    store.Lifecycle{ResultTTLSec: ttlSec},
	}
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability %s in %s: %v", name, namespaceID, err)
	}
	return c
}

// mkFinishedTask 用真实状态机把任务推到 succeeded，再把 finished_at 设到指定时刻。
//
// 只能经 Transition 推进（状态机唯一写入口）；finished_at 是时间戳夹具，
// 测试需要它落在过去才能验证「过期」。
func mkFinishedTask(t *testing.T, db *store.DB, namespaceID string, cap *store.CapabilityVersion, finishedAt int64) *store.Task {
	t.Helper()
	ctx := context.Background()
	task := &store.Task{
		NamespaceID:  namespaceID,
		TraceID:      "trace_" + store.NewTaskID(),
		Path:         []string{},
		CapabilityID: cap.ID,
		Capability:   cap.Name,
		Version:      cap.Version,
		Params:       json.RawMessage(`{}`),
		GraphID:      "grf_lease_test",
	}
	if err := db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	chain := [][2]string{
		{store.TaskPending, store.TaskAssigned},
		{store.TaskAssigned, store.TaskAcked},
		{store.TaskAcked, store.TaskRunning},
		{store.TaskRunning, store.TaskSucceeded},
	}
	for _, step := range chain {
		if err := db.Transition(ctx, namespaceID, task.ID, step[0], step[1]); err != nil {
			t.Fatalf("transition %s → %s: %v", step[0], step[1], err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET finished_at = ? WHERE namespace_id = ? AND id = ?`,
		finishedAt, namespaceID, task.ID); err != nil {
		t.Fatalf("stub finished_at: %v", err)
	}
	return task
}

// countTasks 数某个命名空间里的任务行。
func countTasks(t *testing.T, db *store.DB, namespaceID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM tasks WHERE namespace_id = ?`, namespaceID).Scan(&n); err != nil {
		t.Fatalf("count tasks in %s: %v", namespaceID, err)
	}
	return n
}

// TestRetentionSweepsPerNamespace 巡检驱动的一轮清理必须**按命名空间**各自生效（#162）。
//
// 背景：store 的 ResultRetentionScope 现在按（命名空间 × 能力）出 scope，
// DeleteExpiredTerminalTasks/DeleteExpiredEvents 的作用域参数从 orgID 换成了 namespaceID。
// lease 层不自带命名空间（它是全局巡检），但必须把这条维度**完整传下去**——
// 所以这里用真库造两个命名空间的过期终态任务，断言两边都被清掉。
//
// 用真库而不是 fake 的理由：fake 只能证明「调了 SweepRetention」，
// 证明不了「命名空间维度没在半路被 org_id 顶掉」——而后者正是这次改动最容易回归的地方
// （orgID 传进 namespace 位置的实现会让删除 0 行，且**不报错**）。
func TestRetentionSweepsPerNamespace(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir() + "/lease-ns.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	org, err := db.CreateOrg(ctx, "org-"+store.NewOrgID())
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	ns1, err := db.DefaultNamespaceID(ctx, org.ID)
	if err != nil {
		t.Fatalf("default namespace: %v", err)
	}
	ns2, err := db.CreateNamespace(ctx, org.ID, "staging", "预发")
	if err != nil {
		t.Fatalf("create second namespace: %v", err)
	}

	const ttlSec = 60
	now := int64(1_700_000_000)
	expired := now - 3600 // 早于 now - ttl
	fresh := now - 10     // 未过期

	cap1 := mkNsCapability(t, db, ns1, "retained", ttlSec)
	cap2 := mkNsCapability(t, db, ns2.ID, "retained", ttlSec)
	cap3 := mkNsCapability(t, db, ns2.ID, "retained-fresh", ttlSec)

	mkFinishedTask(t, db, ns1, cap1, expired)    // 应被删
	mkFinishedTask(t, db, ns2.ID, cap2, expired) // 应被删（另一个命名空间）
	mkFinishedTask(t, db, ns2.ID, cap3, fresh)   // 不该被删（未过期）

	sw := New(db)
	sw.Now = func() int64 { return now }
	sw.RetentionInterval = time.Second
	sw.Tick(ctx) // 首轮立即执行清理（lastRetention == 0）
	sw.retentionWG.Wait()

	if got := countTasks(t, db, ns1); got != 0 {
		t.Fatalf("默认命名空间里过期终态任务应被清掉，剩 %d 行", got)
	}
	// ns2 里两条：过期的那条被删，未过期的那条留下 → 正好 1 行
	if got := countTasks(t, db, ns2.ID); got != 1 {
		t.Fatalf("staging 命名空间应只剩 1 行（未过期的），实际 %d 行", got)
	}
}
