package lease

// 保留策略（#143）在租约巡检上的**调度**：把「过期结果清理」挂到现有的 5s 巡检 ticker 上，
// 但只按低频（默认 1 小时，见 DefaultRetentionInterval）**异步**真正执行。
//
// 为什么复用巡检 ticker 而不另起一个 ticker：巡检本来就是进程里唯一那个
// 「周期性做后台维护」的循环，再起一个只会多一份生命周期管理（启动/退出/抖动）
// 却没有额外收益。低频门（lastRetention）把「检查频率」与「执行频率」解耦：
// 每 5s 便宜地看一眼是否到点，到点才在独立 goroutine 里做真正昂贵的 DELETE。
//
// **调度默认接线（写事实，不是「默认关闭」）**：`lease.New(*store.DB)` 里用类型断言
// 自动接线（编译期 `var _ RetentionStore = (*store.DB)(nil)` 钉死签名不许漂移），
// 而 `internal/server/mux.go` 传进来的正是 `*store.DB`，所以**生产上这个调度一直在跑**。
// **仍然没有针对「调度本身」的 kill switch**：没有 env 开关可以把调度整个关掉，
// 想彻底关只能改代码（不调用 New，或把 Sweeper.Retention 置 nil）。
//
// **events 保留期：已有部署级开关（#229），默认 30 天**。
// `Sweeper.RetentionEventSec` 由部署配置 `ANOTIFY_EVENT_RETENTION_DAYS`
// （经 mux.go 的 `cfg.EventRetentionSec()`）注入，默认 `30*86400` 秒；
// `0` = **永久保留**（= `store.EventRetentionDisabled`）。runRetentionSweep 会把它
// **原样**填进 `store.RetentionOptions.EventRetentionSec`；store 侧见到 `<= 0` 就跳过
// 整个 events 阶段并把 `report.EventsDisabled` 置 true。
// 两层的默认值**不同、且必须不同**，别混读：
//   - **部署层**（server.Config，来自 `ANOTIFY_EVENT_RETENTION_DAYS`）默认 = 30 天，
//     所以生产上默认**就是会删** events 的；
//   - **lease 层字段**零值 0 = 永久保留（store.EventRetentionDisabled）。
//     它是「调用方没表达意见」时的安全落点：谁构造 Sweeper 却不注入这个字段（测试、
//     嵌入式用法），行为都是「什么都不删」，不会因为忘了一次赋值就开始不可逆地删数据。
// 换句话说：「不配置就不删 events」由**字段零值**承载，而「生产默认删 30 天前」由
// **部署配置**表达——这是有意的分层。
//
// 开启 events 清理（默认就是开的）有两个**必须让运维知道**的后果，mux.go 的启动日志会写：
//   - 磁盘：`auto_vacuum=0`，删了不会把文件还回磁盘，只是把页放进 freelist。
//     「稳态」= 文件不再线性增长（平台化 ≈ 保留窗口内的写入量），**不是**文件变小。
//   - 语义：events 行被删 = `idem_key` 去重窗口缩成保留期那么长（超期重投会新建 trace
//     并重新派整条链）。这是行为变化，不是纯存储优化。
//
// **追赶机制（#229 / 验收 A5）**：低频门默认 1 小时，但单轮预算是固定的
// （每范围 `Batches × Batch` 行，默认 20×1000 = 20,000 行；见 store.DefaultRetentionBatches）。
// 只在「每小时一轮」下跑，单命名空间每天最多删 ~48 万行；而 30 ev/s 的写入（约 2 行/事件）
// 是 ~518 万行/天 —— **保留期开着也永远删不完**（差一个数量级，写入量还在涨）。
// 所以：上一轮**被批数预算截断**（`report.Truncated`）时，下一轮的低频门改用
// `RetentionCatchUpInterval`（默认 30s，见 DefaultRetentionCatchUpInterval）而不是 1 小时；
// 直到某一轮不再截断（追平了）再回到 1 小时。30s 一轮 = 20,000×2880 ≈ 5,760 万行/天/范围，
// 对 518 万行/天有近 10 倍余量。追赶只提高「单轮跑几批」，**不改变单批成本**
// （每批仍是独立的 1000 行短事务，写锁持有量不变）。
//
// **首轮立即执行（lastRetention 初值 0 = 还没跑过）**：启动/重启后的第一个 5s tick 就会真的
// 跑一轮：
//   - tasks 侧按**能力定义**生效，不能笼统说成「不生效」：
//     **默认安装（库里只有内置能力）下不删 tasks**——内置能力
//     （store.BuiltinCapabilities）的 lifecycle 没声明 resultTtlSec，未声明就是 0 = 永不清理；
//     但**一旦有自定义能力声明了 `resultTtlSec > 0`**（经 POST /v1/capabilities 落库的
//     lifecycle.resultTtlSec），**该能力下的终态 tasks 就会被按 TTL 删**（不可逆）；
//   - events 侧看 `RetentionEventSec`：> 0（默认 30 天）就按保留期删，
//     <= 0（显式配 0）= 整段跳过（report.EventsDisabled = true），一条都不删。
//
// 首轮还会打一条 Info（lease.sweeper.retention_enabled），把 events 的**真实状态**
// （`eventRetentionLogState`：30 天 / 永久保留）写进去——否则运维看到
// 「没有 events_deleted」只能猜「扫了但没东西可删」。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// RetentionStore 是保留策略调度需要的数据层能力（*store.DB 满足）。
//
// 注意这里是 lease 包自己声明的窄接口，不是具体类型：测试用 fake 实现它即可，
// 不依赖 store 层的真实 SQL（两者可由不同的人并行实现）。
type RetentionStore interface {
	// SweepRetention 执行一轮保留策略清理，返回本轮删了多少、是否被预算截断。
	SweepRetention(ctx context.Context, opts store.RetentionOptions) (store.RetentionReport, error)
}

// 编译期断言：*store.DB 必须实现 RetentionStore。
//
// 为什么不能只靠 New 里的运行时类型断言：store 层的 SweepRetention 签名一旦漂移
// （多一个参数、返回值变了），go build ./... 仍然全绿，只是断言失败 → Retention == nil
// → 清理被**静默关闭**——正是 #143 要消灭的那类「字段定义了但没人实现」的 bug。
// 这一行把漂移变成编译错误，而不是运行时的隐形降级。
var _ RetentionStore = (*store.DB)(nil)

// DefaultRetentionInterval 保留策略的执行周期：1 小时（巡检 tick 是 5s，两者不同频）。
//
// 为什么常态是 1 小时而不是更长/更短：清理不追求实时（过期数据多留一会儿只是占空间），
// 但拖太久会让积压轮次不足（每轮每范围有批数预算）；1 小时是「锁竞争可忽略、
// 积压当天能排空」的折中。
//
// **它不再是「永远 1 小时」**：上一轮被批数预算截断（report.Truncated）时，下一轮改用
// DefaultRetentionCatchUpInterval 追赶（见文件头的「追赶机制」）。
const DefaultRetentionInterval = time.Hour

// DefaultRetentionCatchUpInterval 是「上一轮被批数预算截断」时的追赶周期（默认 30s）。
//
// 为什么需要它（#229 / 验收 A5）：默认单轮预算是每范围 Batches×Batch = 20×1000 行，
// 1 小时一轮 → 单命名空间每天最多删 ~48 万行；而 30 ev/s 的写入（约 2 行/事件）
// 是 ~518 万行/天，差一个数量级——保留期开着也永远删不完（inflow 与删除速率必须对账）。
// 30s 一轮 = 20,000 × 2880 ≈ 5,760 万行/天/范围，对 518 万行/天有近 10 倍余量。
//
// 为什么不更短：每批仍是一次独立的 1000 行短事务（写锁持有量不变），30s 已经够用；
// 更短只会让「稳态下根本没东西可删」也每几秒打一次库，白白跟写入抢写锁。
const DefaultRetentionCatchUpInterval = 30 * time.Second

// eventRetentionLogState 把 events 保留秒数翻译成**日志里唯一的说法**（#229）。
//
// 为什么必须是一句话而不是裸数字：各处日志若各写各的（"0"、"off"、"n/a"、
// "event_retention_sec=0"），运维会把「events 一条没删」误读成「扫了但没东西可删」，
// 而那个 "0" 更会被读成「保留 0 秒」。这里把两种真实状态写成自解释的字符串：
//
//	sec <= 0 → "permanent（不清理，events 会持续增长）"  ← 0 = 永久保留 = store.EventRetentionDisabled
//	sec  > 0 → "enabled（30d）"                          ← 天数来自 ANOTIFY_EVENT_RETENTION_DAYS
//
// 取值来自哪：`Sweeper.RetentionEventSec`，由部署配置经 mux.go 注入（默认 30 天）。
// 名字保持 eventRetentionLogState（原来是常量、现在是函数）：调用点语义不变
// （「events 保留期的可读状态」），只是取值从「恒为禁用」变成了跟随配置。
func eventRetentionLogState(sec int) string {
	if sec <= 0 {
		return "permanent（不清理，events 会持续增长）"
	}
	return fmt.Sprintf("enabled（%dd）", sec/86400)
}

// maybeSweepRetention 是保留策略的**门禁 + 启动**（Tick 内部调用）：只判断「是否到点」
// 并把清理 goroutine 起来，本函数（连同 Tick）立刻返回。
//
// 语义要点：
//
//  1. Retention == nil → no-op。注意这只发生在「传入的 Store 不实现 RetentionStore」时；
//     生产路径 lease.New(*store.DB) 会自动接线，**线上这个调度一直在跑**，且没有针对调度
//     本身的 kill switch（见文件头）。events 到底删不删，取决于 **`Sweeper.RetentionEventSec`**
//     （部署配置 ANOTIFY_EVENT_RETENTION_DAYS，默认 30 天；0 = 永久保留 → store 跳过整段）。
//  2. lastRetention == 0（还没跑过）→ **首轮立即执行**：升级/重启后的第一个 tick 就真的
//     跑一轮（不是「上线后先观察」），这一轮做的是：tasks 侧按能力声明清理（默认安装下
//     为零——内置能力没声明 resultTtlSec；有自定义能力声明了 resultTtlSec>0 时，该能力下
//     的过期终态任务会被按 TTL 删，不可逆），events 侧按 RetentionEventSec 生效
//     （>0 默认 30 天 → 真的删；<=0 → 整段跳过，一条都不删）。
//     首轮还会打一条 Info（lease.sweeper.retention_enabled），里面写清 events 的真实状态
//     （eventRetentionLogState："enabled（30d）" / "permanent（不清理，events 会持续增长）"）：
//     否则只靠「真删了东西才打日志」，运维无法区分「清理跑过但没东西可删」和「清理根本没开」，
//     更分不清「events 被永久保留」和「events 零命中」。
//  3. 先写回 lastRetention 再启动：失败也记时刻。否则失败后每条 tick（5s）
//     都会重试，库抖动时反而变成高频打库，把「自愈」变成「雪崩」。
//  4. 重叠守卫：上一轮还没跑完（retentionBusy）→ 跳过本轮，不排队也不叠加。
//     常态周期是 1 小时、追赶周期 30s；守卫防的是「上一轮跑了超过一个周期」——
//     无索引时单轮是整库扫描 + TEMP B-TREE 排序（契约 §6），确实可能很慢。
//     跳过时不推进 lastRetention：这一轮什么都没做，下一 tick 还会再试。
//  5. 启动方式：retentionWG.Add → go runRetentionSweep；ctx 取消后 store 层自己检查
//     ctx.Err() 提前返回，这里不额外加逻辑。失败只记日志，绝不影响下一轮巡检
//     （巡检是自愈机制，不能停摆）。
func (s *Sweeper) maybeSweepRetention(ctx context.Context, now int64) {
	if s.Retention == nil {
		return
	}

	// 频率门：0 表示还没跑过 → 首轮立即执行。
	//
	// 周期不是固定的：上一轮被批数预算截断（report.Truncated）说明「还没删完」，
	// 这时用追赶周期（默认 30s）而不是 1 小时——否则写入量一上来就永远追不上
	// （见文件头的「追赶机制」）。
	//
	// lastRetentionTruncated 只在**本 goroutine** 读写；清理 goroutine 那一侧用
	// atomic（retentionTruncated）交接，这里取一次快照。否则 Tick 读 / 清理 goroutine 写
	// 同一个 bool 就是数据竞争（-race 会直接报）。
	s.lastRetentionTruncated = s.retentionTruncated.Load()
	interval := s.retentionInterval()
	if s.lastRetentionTruncated {
		interval = s.retentionCatchUpInterval()
	}
	if s.lastRetention != 0 && now-s.lastRetention < int64(interval.Seconds()) {
		return
	}

	// 重叠守卫：上一轮清理还在跑就跳过本轮（CAS 败者直接退出，不排队）。
	if !s.retentionBusy.CompareAndSwap(false, true) {
		slog.Debug("retention sweep still running, skip this tick",
			"event", "lease.sweeper.retention_skipped",
		)
		return
	}

	// 首轮（进程启动 / 首次启用）打一条 Info：调度已启用要让运维能确认，同时**必须把
	// events 的真实状态写出来**——按 RetentionEventSec 是「30 天」还是「永久保留」
	// （eventRetentionLogState）。少了这一句，运维看到「没有 events_deleted」只能猜
	// 「扫了但没东西可删」，而事实可能是「压根没扫 events」（配了 0 = 永久保留）。
	// 只打一次——写完 lastRetention 后这个分支不再进入。位置在启动 goroutine 之前，
	// 这样「调度已启用」一定先于「本轮删了多少」（后者可能要很久）。
	if s.lastRetention == 0 {
		slog.Info("retention sweeper enabled",
			"event", "lease.sweeper.retention_enabled",
			"interval", s.retentionInterval().String(),
			// 追赶周期也打出来：运维要能从日志里读到「截断后多久补一轮」，
			// 而不是去翻代码里的常量（对应 A5 的「能跟上写入」）。
			"catch_up_interval", s.retentionCatchUpInterval().String(),
			"batch", s.retentionBatch(),
			"batches", s.retentionBatches(),
			// events 状态固定用 eventRetentionLogState 这一种写法（见其注释）：
			// 两种取值分别是 "permanent（不清理，events 会持续增长）" 与 "enabled（30d）"。
			"event_retention", eventRetentionLogState(s.RetentionEventSec),
			"events_retention_sec", s.RetentionEventSec,
		)
	}

	s.lastRetention = now // 先记时刻（失败也记，见语义要点 3）

	// Add 必须在启动 goroutine 之前：否则测试里的 Wait 可能漏掉这一轮。
	s.retentionWG.Add(1)
	go s.runRetentionSweep(ctx)
}

// runRetentionSweep 在独立 goroutine 里跑一轮真正的清理（maybeSweepRetention 启动）。
//
// 为什么必须独立于 Tick 的 goroutine：Tick 与 Run 共用同一条 goroutine，同步跑清理会把
// 租约回收/重派一起堵住、ticker 还会丢 tick；而无索引时单轮清理是「整库扫描 +
// TEMP B-TREE 排序」（契约 §6），一轮可能很久。
//
// Now 在 goroutine 里重新取：以「真正开始跑的时刻」算 cutoff，比 Tick 里那个可能已经
// 过去若干秒的 now 更准（Tick 与执行之间隔着调度延迟，甚至上一次卡住的轮次）。
//
// **EventRetentionSec 由部署配置注入**（#229）：这里把 `Sweeper.RetentionEventSec` **原样**
// 填进 store.RetentionOptions。取值语义（务必别读错）：
//   - 未配置（零值 0）或显式 0 → `store.EventRetentionDisabled` = **永久保留**：
//     store 跳过整个 events 阶段并置 report.EventsDisabled=true；
//   - > 0（默认 30*86400）      → 真的按保留期删 events（不可逆）。
//
// 所以「不配置就不删 events」这条老保证仍然成立——它现在由「**字段零值 0 = 永久保留**」
// 承载，而不再由「lease 层永远不填这个字段」承载。反过来说「lease 层永不注入
// EventRetentionSec」这个说法**已经不成立**（本版就是由它注入的），别再按老注释读代码。
// 回归断言：retention_test.go 的 TestRetentionEventSecPlumbing 盯住「原样透传」。
func (s *Sweeper) runRetentionSweep(ctx context.Context) {
	defer s.retentionWG.Done()
	// LIFO：先解除守卫再 Done —— 同包测试 Wait 返回时守卫已经放开，可以再 Tick。
	defer s.retentionBusy.Store(false)

	// 本轮默认**不追赶**：只有「成功且被批数预算截断」才置 true（见下面成功分支）。
	// 失败轮刻意不进追赶：库抖动时每 30s 重试一轮会把「自愈」变成「雪崩」
	// （与「失败也写回 lastRetention」同一个理由）。
	// 用 atomic 交接给 Tick 的 goroutine（那边的快照字段叫 lastRetentionTruncated）。
	s.retentionTruncated.Store(false)

	retentionSec := s.RetentionEventSec
	start := time.Now()
	report, err := s.Retention.SweepRetention(ctx, store.RetentionOptions{
		Now: s.now(),
		// #229：原样透传（0 = 永久保留，>0 = 保留这么多秒）。不要再在这里判断
		// 「该不该删」——那是部署配置（ANOTIFY_EVENT_RETENTION_DAYS）与运维的选择。
		EventRetentionSec: retentionSec,
		Batch:             s.retentionBatch(),
		Batches:           s.retentionBatches(),
	})
	// 耗时是运维对账的必需量（A2「删了多少 / 耗时」）：用它算「单轮能删完多少」，
	// 再跟写入速率比，就知道追赶周期够不够（A5）。
	elapsedMs := time.Since(start).Milliseconds()
	if err != nil {
		// 出错也把 partial report 的计数打出来：store 层返回的是「已经删了多少」的
		// partial report，只打 error 等于「删了一部分但日志里看不见删了多少」。
		// 取消/超时是正常路径（进程退出 / ctx 到期），降级成 Info，避免关停时刷 Error 噪音；
		// 其它错误才是真故障，保持 Error。
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Info("retention sweep cancelled",
				"event", "lease.sweeper.retention_cancelled",
				"error", err.Error(),
				"tasks_deleted", report.TasksDeleted,
				"events_deleted", report.EventsDeleted,
				"events_disabled", report.EventsDisabled,
				"task_scopes", report.TaskScopes,
				"namespaces", report.Namespaces,
				"truncated", report.Truncated,
				// 取消路径也要能对账：用了什么保留期、跑了多久才被打断。
				"events_retention_sec", retentionSec,
				"elapsed_ms", elapsedMs,
			)
			return
		}
		slog.Error("retention sweep failed",
			"event", "lease.sweeper.retention_failed",
			"error", err.Error(),
			"tasks_deleted", report.TasksDeleted,
			"events_deleted", report.EventsDeleted,
			"events_disabled", report.EventsDisabled,
			"task_scopes", report.TaskScopes,
			"namespaces", report.Namespaces,
			"truncated", report.Truncated,
			// 失败路径同样带保留期与耗时：排障时要分清「保留期没配」和「配了但删不动」。
			"events_retention_sec", retentionSec,
			"elapsed_ms", elapsedMs,
		)
		return // 不给巡检停摆：下一条 tick 照常回收租约/重派
	}

	// 把「这一轮是否被预算截断」交给 Tick 的 goroutine：截断 = 还有积压，下一轮
	// 用追赶周期（默认 30s）而不是 1 小时（见 maybeSweepRetention 的频率门）。
	s.retentionTruncated.Store(report.Truncated)

	// 只在「真删了东西」或「被预算截断（还有积压）」时打日志：
	// 否则每小时一条空转日志，纯噪音。
	if report.TasksDeleted+report.EventsDeleted > 0 || report.Truncated {
		slog.Info("retention round finished",
			"event", "lease.sweeper.retention",
			"tasks_deleted", report.TasksDeleted,
			"events_deleted", report.EventsDeleted,
			"events_disabled", report.EventsDisabled,
			"task_scopes", report.TaskScopes,
			"namespaces", report.Namespaces,
			"truncated", report.Truncated,
			// A2 的审计字段：删了多少之外，还要说清「按什么保留期删的」（0 = 永久保留，
			// 这时 events_deleted 恒为 0）与「这一轮花了多久」（算吞吐用）。
			"events_retention_sec", retentionSec,
			"elapsed_ms", elapsedMs,
		)
	}
}

// retentionInterval 返回保留策略执行周期；0 → DefaultRetentionInterval。
func (s *Sweeper) retentionInterval() time.Duration {
	if s.RetentionInterval > 0 {
		return s.RetentionInterval
	}
	return DefaultRetentionInterval
}

// retentionCatchUpInterval 返回追赶周期；0 → DefaultRetentionCatchUpInterval（30s）。
//
// 只在「上一轮被批数预算截断」时用（见 maybeSweepRetention 的频率门）：追平（某轮不再截断）
// 之后自动回到 retentionInterval（默认 1 小时），不需要任何运维操作。
func (s *Sweeper) retentionCatchUpInterval() time.Duration {
	if s.RetentionCatchUpInterval > 0 {
		return s.RetentionCatchUpInterval
	}
	return DefaultRetentionCatchUpInterval
}

// retentionBatch 返回单条 DELETE 的行数上限；0 → store.DefaultRetentionBatch。
func (s *Sweeper) retentionBatch() int {
	if s.RetentionBatch > 0 {
		return s.RetentionBatch
	}
	return store.DefaultRetentionBatch
}

// retentionBatches 返回每个范围每轮最多跑几批；0 → store.DefaultRetentionBatches。
func (s *Sweeper) retentionBatches() int {
	if s.RetentionBatches > 0 {
		return s.RetentionBatches
	}
	return store.DefaultRetentionBatches
}
