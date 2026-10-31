package server

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// 本文件实现派发背压中间件（#228 / 契约 §1.2）。
//
// 问题：节点派发跟不上事件写入时，tasks 表里 status='pending' 的行会持续堆积，
// 越堆越久，最终表现为「所有事件都要等好几分钟才送达」——但服务看着一切正常，
// 因为每个请求都成功了，只是延迟在无声地增长（#228 的「积压不可观测」）。
//
// 决策：判定信号 = **最老 pending 任务的等待时长**（`now - MIN(created_at)`），
// 超阈时**拒绝新事件**（503 + Retry-After），不排队、不丢弃。用等待时长而不是
// pending 行数，是因为行数随命名空间数 / 能力数 / 流量口径漂移，跨部署不可比；
// 「最老的等了多久」与部署规模无关。
//
// 三条硬约束（都是本文件里最容易被人"顺手优化"掉的地方）：
//
//	① **请求路径上不查 DB**：每请求只有一次 atomic.Load；采样在后台 goroutine。
//	② **未超阈时每请求 0 次 DB 往返**（B5）；采样靠自适应降采样，稳态 0.2 次/秒。
//	③ **不因为一次 DB 抖动改变判定**：采样出错保持上一次状态（既不解保也不加保）。

// pendingAgeSource 是背压采样需要的最小数据层能力（*store.DB 满足）。
//
// 用窄接口而不是 *store.DB：本文件的单元测试不依赖真实 SQL，
// 接线正确性由 mux.go 的编译期断言（`var _ pendingAgeSource = (*store.DB)(nil)`）钉住。
type pendingAgeSource interface {
	// OldestPendingCreatedAt 返回 status='pending' 的任务里最早的 created_at
	// （unixepoch 秒）；一条 pending 都没有 → 0。
	OldestPendingCreatedAt(ctx context.Context) (int64, error)
}

const (
	// defaultBackpressureFastEvery 是「接近/超过阈值」时的采样间隔。
	defaultBackpressureFastEvery = time.Second
	// defaultBackpressureIdleEvery 是「远低于阈值」时的采样间隔。
	//
	// 5s 而不是 1s 是刻意的：稳态下后台 DB 负载从 1 次/秒降到 0.2 次/秒，
	// 而判定精度对「已经积压了两分钟」这个量级完全没有影响（阈值本身是 120s 级）。
	defaultBackpressureIdleEvery = 5 * time.Second
	// defaultBackpressureRetryAfter 是 503 的 Retry-After：15s 让调用方在一次
	// 退避后就能重试，又不至于把重试打成脉冲（积压的消化是按秒级推进的）。
	defaultBackpressureRetryAfter = 15 * time.Second

	// defaultBackpressureStuckAfter 是「超阈持续多久之后**停止拒绝**」的安全阀（默认 30 分钟）。
	//
	// 为什么必须有它（#228 复审发现的 MAJOR）：判定信号的取数口径是
	// `MIN(created_at) WHERE status='pending'`，而库里一旦存在**永远派不出去**的 pending，
	// 这个值只增不减 —— 能力升级后没有节点再声明那个版本、节点下线后没人再声明该能力、
	// 任务建出来后集群里再没有能跑它的节点：`RedispatchTask` 对这些只会一直返回 false
	// （保持 pending），全仓没有任何 abandon 路径。于是一条孤儿任务就能把**整个部署**的
	// 新事件入口钉在 503 上，而且永不恢复。
	//
	// 所以保护必须有出口：连续超阈超过该时长就**停止拒绝**（fail-open）并打一条 ERROR。
	// 这是刻意的取舍，不是 bug：「整站写不进来」比「队列继续涨」更糟。
	//   - 真正的短时过载（分钟级）在这个窗口之前早自愈了；
	//   - 只有「结构性坏掉」才会走到这里，那时人必须去看日志里那条 server.backpressure.stuck。
	defaultBackpressureStuckAfter = 30 * time.Minute

	// sampleFailedEvery 是采样失败日志的节流窗口。
	//
	// 为什么要节流：`over=true` 时采样档是 FastEvery=1s，DB 一直报错就是 1 条 WARN/秒
	// （同文件把 still_engaged 特意节流到 60s，还引了 #245 的教训 —— 这里原本漏了一处）。
	sampleFailedEvery = 60 * time.Second

	// stillEngagedEvery 是「持续超阈」日志的节流窗口。
	//
	// 超阈可能持续几分钟到几十分钟，而采样本 FastEvery 就是 1s 一次：
	// 不节流会把日志盘刷爆（同 #245 的教训）。60s 一条既能让运维看见
	// 「还在拒、拒了多久」，又不至于污染日志。
	stillEngagedEvery = 60 * time.Second
)

// backpressureOptions 是背压中间件的配置（零值字段由构造函数补默认值）。
type backpressureOptions struct {
	// MaxAge 是阈值；<= 0 = 不启用（永远放行）。0 是运维的显式选择：
	// 小部署（单机、无节点自动扩容）宁可慢也不要被自己的保护挡住。
	MaxAge time.Duration
	// FastEvery 是「接近/超过阈值」时的采样间隔，默认 1s。
	FastEvery time.Duration
	// IdleEvery 是「远低于阈值」时的采样间隔，默认 5s。
	IdleEvery time.Duration
	// RetryAfter 是 503 的 Retry-After 头，默认 15s。
	RetryAfter time.Duration
	// StuckAfter 是安全阀：连续超阈超过它就不再拒绝（fail-open），默认 30 分钟。
	// 见 defaultBackpressureStuckAfter 的注释（孤儿 pending 会让信号永不恢复）。
	StuckAfter time.Duration
}

// backpressureBody 是 503 的机器可读 body。
//
// 字段名由契约 §1.2 冻结（`error` / `pending_age_sec` / `max_pending_age_sec` /
// `retry_after_sec`）：调用方（CLI / 节点侧重试器）要据此判断"等多久再来"，
// 不能只给一句人话。字段顺序即结构体顺序，与契约示例逐字一致。
type backpressureBody struct {
	// Error 是给机器认的稳定标识（不是给用户看的文案）。
	Error string `json:"error"`
	// PendingAgeSec 是最近一次成功采样到的积压年龄（秒，向下截断 = "已经等了多少整秒"）：
	// 可能比"此刻"旧最多一个采样周期（FastEvery=1s），因为它就是判定依据本身。
	PendingAgeSec int64 `json:"pending_age_sec"`
	// MaxPendingAgeSec 是当前生效的阈值（秒）。
	MaxPendingAgeSec int64 `json:"max_pending_age_sec"`
	// RetryAfterSec 与 Retry-After 头同值（有人只读 body、有人只读头）。
	RetryAfterSec int64 `json:"retry_after_sec"`
}

// backpressureGuard 把「派发积压」翻译成「对新事件入口的 503」。
//
// 状态全部是 atomic：Wrap 在请求热路径上只读不写（除 rejectedTotal 计数），
// sample 在后台 goroutine 上写。没有任何 mutex —— 请求路径上一次锁都不该拿。
type backpressureGuard struct {
	src  pendingAgeSource
	opts backpressureOptions

	// ---- 请求路径只碰这几个（atomic 读，无锁无 DB）----
	//
	// over 是"最近一次**成功**采样是否超阈"。零值 false = 放行，
	// 于是"进程刚起、还没采过样"天然是放行的（不因为「还不知道」就拒绝新事件）。
	over          atomic.Bool
	lastAgeSec    atomic.Int64 // 最近一次成功采样的积压年龄（秒，向下截断）
	rejectedTotal atomic.Int64 // 累计 503 次数
	engagedSecs   atomic.Int64 // 本轮持续超阈的时长（秒），解除时定格

	// rejecting 是 Wrap 真正读的那个开关 = over && !stuck。
	//
	// 为什么不直接读 over：安全阀要能在「信号一直超阈」时把拒绝关掉
	// （见 defaultBackpressureStuckAfter）。拆成两个量，日志里才能区分
	// 「正在拒」与「超阈但已 fail-open」。
	rejecting atomic.Bool
	// stuck = 本轮超阈已超过安全阀窗口，拒绝已被关掉（fail-open 生效中）。
	stuck atomic.Bool

	// lastSampleFailLogUnix 是上一条 sample_failed 的时刻（unix 秒），节流用。
	lastSampleFailLogUnix atomic.Int64

	// ---- 采样侧（Run 单 goroutine 与测试读写）----
	nextEveryNanos   atomic.Int64 // 下一轮采样间隔（纳秒）；采样出错时保持不变
	engagedSinceUnix atomic.Int64 // 本轮超阈起始时刻（unix 秒）
	lastStillLogUnix atomic.Int64 // 上一条 still_engaged 的时刻（unix 秒）

	// now 是时钟，默认 time.Now。
	//
	// 做成字段只为一件事：单测要精确摆布 `age > MaxAge` 这条**严格大于**的边界
	// （真时钟下 age 永远比整数秒多一点，边界用例会变成碰运气）。生产路径不替换它。
	now func() time.Time
}

// newBackpressureGuard 构造 guard；零值/负数的间隔与 RetryAfter 补默认值。
//
// MaxAge <= 0 表示不启用（由 Run / Wrap 各自短路），构造函数**不**因此报错：
// 「关闭背压」是合法的运维选择，不是配置错误。
func newBackpressureGuard(src pendingAgeSource, opts backpressureOptions) *backpressureGuard {
	if opts.FastEvery <= 0 {
		opts.FastEvery = defaultBackpressureFastEvery
	}
	if opts.IdleEvery <= 0 {
		opts.IdleEvery = defaultBackpressureIdleEvery
	}
	if opts.RetryAfter <= 0 {
		opts.RetryAfter = defaultBackpressureRetryAfter
	}
	// 下限 1s：`int64(StuckAfter/time.Second)` 在 <1s 时会截断成 0，
	// 于是「第二次超阈采样」就立刻满足 `engagedSecs >= 0` → 保护刚生效就 fail-open。
	// 生产上这个旋钮只能由代码设置（env 不暴露它），但别留一个「填错就静默失效」的口子。
	if opts.StuckAfter < time.Second {
		opts.StuckAfter = defaultBackpressureStuckAfter
	}
	g := &backpressureGuard{src: src, opts: opts, now: time.Now}
	// 首次采样之后 sample 会按自适应规则改写它；默认给 FastEvery，
	// 让"刚起就积压"的进程尽快切到 1s 节奏。
	g.nextEveryNanos.Store(int64(opts.FastEvery))
	return g
}

// Run 是后台采样循环，直到 ctx 取消。由 NewApp 起一个 goroutine。
//
// 进入循环后**立刻同步采一次**（首个 timer 延迟 0）：进程刚起就有一个很老的 pending
// 时不该白等一个采样周期才开始保护，e2e 也靠这一点把等待从「秒级」压到「几十毫秒」。
//
// 等待一律走 `select { ctx.Done() / timer.C }`：裸 time.Sleep 会让取消退化成
// 「睡完这一轮再说」，goroutine 泄漏检测（-race 下尤其明显）会立刻抓到。
func (g *backpressureGuard) Run(ctx context.Context) {
	if g == nil || g.opts.MaxAge <= 0 || g.src == nil {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// sample 自己负责记日志（含 sample_failed）；Run 只负责节奏。
		_ = g.sample(ctx)
		timer.Reset(g.nextInterval())
	}
}

// Wrap 包住**新事件入口**的 handler。
//
// 未启用（g == nil 或 MaxAge <= 0）时**原样返回 next**：不包那一层，
// 连一次 atomic.Load 都不多付。这是 B5「未超阈时不得有额外开销」的第一道保证。
//
// 启用后每请求只做一次 atomic 判定；超阈才写 503。**不调用 next**：
// 拒绝的是"新事件"这个动作本身，写库/写 outbox 都由 next 负责，调用它等于没拒。
func (g *backpressureGuard) Wrap(next http.Handler) http.Handler {
	if g == nil || g.opts.MaxAge <= 0 {
		return next
	}
	maxAgeSec := ceilSec(g.opts.MaxAge)
	retryAfterSec := ceilSec(g.opts.RetryAfter)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 一次 atomic 读：over && !stuck 的合成结果已经提前算好放在 rejecting 里。
		if !g.rejecting.Load() {
			next.ServeHTTP(w, r)
			return
		}
		// 只有真拒了才计数（用来在 released 日志里给出"这轮挡了多少"）。
		g.rejectedTotal.Add(1)
		body := backpressureBody{
			Error:            "backpressure",
			PendingAgeSec:    g.lastAgeSec.Load(),
			MaxPendingAgeSec: maxAgeSec,
			RetryAfterSec:    retryAfterSec,
		}
		h := w.Header()
		// 这是"现在不该重试"的即时结论，任何层都不许缓存它（含 CDN / 浏览器）。
		h.Set("Cache-Control", "no-store")
		h.Set("Retry-After", strconv.FormatInt(retryAfterSec, 10))
		writeJSON(w, http.StatusServiceUnavailable, body)
	})
}

// sample 采一次并更新状态；Run 循环调用，测试也直接调（不用等真实 ticker）。
//
// 返回 error 只给调用方（测试/日志）用：**判定状态在出错时保持上一次的值**。
// 不因为一次 DB 抖动就把全站新事件挡住，也不因为抖动就解除保护 —— 抖动既不是
// 「积压好了」的证据，也不是「积压更糟了」的证据。
func (g *backpressureGuard) sample(ctx context.Context) error {
	if g == nil || g.opts.MaxAge <= 0 {
		return nil
	}
	oldest, err := g.src.OldestPendingCreatedAt(ctx)
	if err != nil {
		// 关停时的 ctx.Canceled 不是"采样失败"，别在每次优雅退出时刷一条 WARN。
		if !errors.Is(err, context.Canceled) {
			// 节流：超阈时采样档是 1s，DB 一直坏就是 1 条 WARN/秒。
			nowUnix := g.now().Unix()
			if nowUnix-g.lastSampleFailLogUnix.Load() >= int64(sampleFailedEvery/time.Second) {
				g.lastSampleFailLogUnix.Store(nowUnix)
				slog.Warn("backpressure sample failed",
					"event", "server.backpressure.sample_failed",
					"err", err,
					"engaged", g.over.Load(),
					"hint", "保持上一次判定结果：一次 DB 抖动不改变保护状态；本条日志每 60s 至多一条",
				)
			}
		}
		return err
	}

	now := g.now()
	// oldest == 0 表示"一条 pending 都没有"（见 pendingAgeSource 的约定），
	// 它不是"1970 年就在等"—— 用零值直接代表 age 0。
	// **这条依赖**：库里的 created_at 绝不可能是 0。今天由写路径保证
	// （store.createTasksTx 把 0 补成 Now()），所以成立；哪天允许 created_at=0，
	// 这里会把「有一条等了 56 年」读成「没有积压」——信号静默失效，不是误拒。
	var age time.Duration
	if oldest > 0 {
		age = now.Sub(time.Unix(oldest, 0))
		if age < 0 {
			// 写入方时间戳超前 / 时钟回拨：当成"刚入队"，不要产生负年龄。
			age = 0
		}
	}
	ageSec := int64(age / time.Second)
	g.lastAgeSec.Store(ageSec)

	// 自适应降采样（契约 §1.2 第 8 条）：本次年龄不到阈值一半 → 下一轮 5s，
	// 否则 1s。目的：稳态下后台 DB 负载 0.2 次/秒，而不是 1 次/秒。
	// 出错时不走到这里，间隔自然保持上一轮的值。
	if age*2 < g.opts.MaxAge {
		g.nextEveryNanos.Store(int64(g.opts.IdleEvery))
	} else {
		g.nextEveryNanos.Store(int64(g.opts.FastEvery))
	}

	// 判定是**严格大于**：age == MaxAge 仍然放行。阈值是"最老等了两分钟"这个语义，
	// 恰好两分钟不入罪；否则 120s 配置在实测里会表现为"119.x 秒就被拒"，不可解释。
	isOver := age > g.opts.MaxAge
	wasOver := g.over.Load()
	g.over.Store(isOver)

	// 安全阀：连续超阈超过 StuckAfter → 停止拒绝（fail-open）。见 defaultBackpressureStuckAfter。
	//
	// **只在「本轮超阈已持续」时累积**：stuck 在 case isOver（即上一轮也超阈）里算，
	// 新的一轮超阈（isOver && !wasOver）必定从 stuck=false 开始。放在 switch 之前
	// 无条件算会出这个 bug：上一轮超阈结束后 engagedSinceUnix 没被清，下一轮刚开始
	// 就把「距上次开始」当成「本轮已持续」，于是刚超阈就被误判成 stuck 而 fail-open。
	wasStuck := g.stuck.Load()
	stuck := false

	switch {
	case isOver && !wasOver:
		g.engagedSinceUnix.Store(now.Unix())
		g.engagedSecs.Store(0)
		g.lastStillLogUnix.Store(now.Unix())
		slog.Warn("backpressure engaged",
			"event", "server.backpressure.engaged",
			"pending_age_sec", ageSec,
			"max_pending_age_sec", ceilSec(g.opts.MaxAge),
			"stuck_after", g.opts.StuckAfter.String(),
			"hint", "新事件入口开始返回 503；已在库里的 pending 任务照常派发，读端点不受影响",
		)
	case isOver:
		// 状态跃迁才打日志，但 engaged_secs 每轮都要推进（它会被 still/released 读）。
		g.engagedSecs.Store(now.Unix() - g.engagedSinceUnix.Load())
		stuck = g.engagedSecs.Load() >= int64(g.opts.StuckAfter/time.Second)
		if stuck && !wasStuck {
			// 走到这里 = 结构性坏掉，不是过载：孤儿 pending 任务让信号永不恢复。
			// fail-open 是刻意的（「整站写不进来」比「队列继续涨」更糟），必须留痕。
			slog.Error("backpressure stuck: fail-open, stop rejecting new events",
				"event", "server.backpressure.stuck",
				"pending_age_sec", ageSec,
				"max_pending_age_sec", ceilSec(g.opts.MaxAge),
				"stuck_after", g.opts.StuckAfter.String(),
				"engaged_secs", g.engagedSecs.Load(),
				"rejected_total", g.rejectedTotal.Load(),
				"hint", "连续超阈超过安全阀窗口 → 已停止拒绝新事件。最可能的原因：库里存在永远派不出去的 pending 任务（能力升级后无人声明 / 节点下线后无人声明该能力 / 积压来源已消失但任务未收敛）。请先清理积压来源；继续拒绝会把整站写入钉死。",
			)
		}
		if now.Unix()-g.lastStillLogUnix.Load() >= int64(stillEngagedEvery/time.Second) {
			g.lastStillLogUnix.Store(now.Unix())
			slog.Warn("backpressure still engaged",
				"event", "server.backpressure.still_engaged",
				"pending_age_sec", ageSec,
				"max_pending_age_sec", ceilSec(g.opts.MaxAge),
				"engaged_secs", g.engagedSecs.Load(),
				"rejected_total", g.rejectedTotal.Load(),
				"rejecting", g.rejecting.Load(),
			)
		}
	case wasOver:
		engaged := now.Unix() - g.engagedSinceUnix.Load()
		if engaged < 0 {
			engaged = 0
		}
		g.engagedSecs.Store(engaged)
		slog.Info("backpressure released",
			"event", "server.backpressure.released",
			"pending_age_sec", ageSec,
			// rejected_total 是**进程级累计**（不是「这一轮挡了多少」）；engaged_secs 是本轮时长。
			// 两者口径不同，别相加也别当成「本轮拒绝数」。
			"rejected_total", g.rejectedTotal.Load(),
			"engaged_secs", engaged,
			"was_stuck", wasStuck,
		)
	}

	// 统一写两个开关（在 switch 之后：新超阈轮已把 engagedSinceUnix 重置为 now，
	// 所以这里的 stuck 一定是「本轮」的口径，不会继承上一轮）。
	g.stuck.Store(stuck)
	g.rejecting.Store(isOver && !stuck)
	return nil
}

// nextInterval 返回下一轮采样间隔（Run 在每轮之后读它）。
func (g *backpressureGuard) nextInterval() time.Duration {
	if d := time.Duration(g.nextEveryNanos.Load()); d > 0 {
		return d
	}
	return g.opts.FastEvery
}

// ceilSec 把时长换成整数秒，向上取整。
//
// 只用于**配置类**数值（阈值 / Retry-After）：配成 1500ms 时报 2 而不是 1，
// 免得 body 里的 max_pending_age_sec 比真实阈值还小。
// 观测类数值（pending_age_sec）走截断，语义是"已经等了多少整秒"。
func ceilSec(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(math.Ceil(d.Seconds()))
}
