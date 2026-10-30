package lease

// 保留策略（#143）在租约巡检上的**调度**：把「过期结果清理」挂到现有的 5s 巡检
// ticker 上，但只按低频（默认 1 小时）**异步**真正执行。
//
// 为什么复用巡检 ticker 而不另起一个 ticker：巡检本来就是进程里唯一那个
// 「周期性做后台维护」的循环，再起一个只会多一份生命周期管理（启动/退出/抖动）
// 却没有额外收益。低频门（lastRetention）把「检查频率」与「执行频率」解耦：
// 每 5s 便宜地看一眼是否到点，到点才在独立 goroutine 里做真正昂贵的 DELETE。
//
// **调度默认接线（写事实，不是「默认关闭」）**：`lease.New(*store.DB)` 里用类型断言
// 自动接线（编译期 `var _ RetentionStore = (*store.DB)(nil)` 钉死签名不许漂移），
// 而 `internal/server/mux.go` 传进来的正是 `*store.DB`，所以**生产上这个调度一直在跑**。
// **本轮没有任何运维 kill switch**：没有 env 开关，也没有 orgs 列开关；想关只能改代码
// （不调用 New，或把 Sweeper.Retention 置 nil）。这是已登记的缺口，是否加开关待上级拍板。
//
// **但「调度在跑」≠「会删 events」——events 保留期默认禁用**：
// `store.DefaultEventRetentionSec == store.EventRetentionDisabled == 0`
// （0 表示**禁用**，不是「保留 0 秒」）。本版**没有开关、也没有 per-org 配置**：lease 层
// 从不给 store.RetentionOptions 填 EventRetentionSec（见 runRetentionSweep），store 见到
// `EventRetentionSec <= 0` 就跳过整个 events 阶段并把 report.EventsDisabled 置 true。
// 所以**本版升级后不会删任何 events**；将来 `orgs.retention_sec` 落地后，才由组织配置开启。
// 理由：events 删除不可逆，而本轮既无开关、也无 per-org 配置，还会静默改变
// store/stats.go 的「总数 / 今日 / 热力图」口径。
//
// **首轮立即执行（lastRetention 初值 0 = 还没跑过）**：升级后的第一个 5s tick 就会真的
// 跑一轮，但这一轮实际做的事是「tasks 侧按能力声明清理（可能为零）+ events 阶段被跳过」：
//   - tasks 侧按**能力定义**生效，不能笼统说成「不生效」：
//     **默认安装（库里只有内置能力）下不删 tasks**——内置能力
//     （store.BuiltinCapabilities）的 lifecycle 没声明 resultTtlSec，未声明就是 0 = 永不清理；
//     但**一旦有自定义能力声明了 `resultTtlSec > 0`**（经 POST /v1/capabilities 落库的
//     lifecycle.resultTtlSec），**该能力下的终态 tasks 就会被按 TTL 删**（不可逆）；
//   - events 阶段**整段跳过**（report.EventsDisabled = true），一条都不会删。
//
// 所以首轮**可能**删 tasks（只要库里存在带 TTL 的自定义能力 + 其过期终态任务），
// 但**不会**删 events——这与「升级即不可逆清空旧事件」是完全不同的后果，别混读。

import (
	"context"
	"errors"
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
// 为什么是 1 小时而不是更长/更短：清理不追求实时（过期数据多留一会儿只是占空间），
// 但拖太久会让积压轮次不足（每轮每范围有批数预算）；1 小时是「锁竞争可忽略、
// 积压当天能排空」的折中。
const DefaultRetentionInterval = time.Hour

// eventRetentionLogState 是日志里 events 保留期的**固定说法**：禁用（等 orgs.retention_sec）。
//
// 为什么固定成一个常量：本版 lease 层**从不**给 store.RetentionOptions.EventRetentionSec
// 赋值（保持零值 0），而 store 把 0 解释为 EventRetentionDisabled = 禁用。各处日志若各写各的
// （"0"、"off"、"n/a"），运维会把「events 一条没删」误读成「扫了但没东西可删」。
// 也别改写成 event_retention_sec=0——那个 0 会被读成「保留 0 秒」。
const eventRetentionLogState = "disabled（等 orgs.retention_sec）"

// maybeSweepRetention 是保留策略的**门禁 + 启动**（Tick 内部调用）：只判断「是否到点」
// 并把清理 goroutine 起来，本函数（连同 Tick）立刻返回。
//
// 语义要点：
//
//  1. Retention == nil → no-op。注意这只发生在「传入的 Store 不实现 RetentionStore」时；
//     生产路径 lease.New(*store.DB) 会自动接线，**线上这个调度一直在跑**，且无 kill switch
//     （见文件头）。但「调度在跑」≠「会删 events」：本版 lease 层从不注入 events 保留期，
//     store 收到 0（= 禁用）就跳过整个 events 阶段，所以**升级后不删任何 events**。
//  2. lastRetention == 0（还没跑过）→ **首轮立即执行**：升级/重启后的第一个 tick 就真的
//     跑一轮（不是「上线后先观察」），但这一轮做的是：tasks 侧按能力声明清理（默认安装下
//     为零——内置能力没声明 resultTtlSec；有自定义能力声明了 resultTtlSec>0 时，该能力下
//     的过期终态任务会被按 TTL 删，不可逆），**events 阶段整段跳过**（一条都不删）。
//     首轮还会打一条 Info（lease.sweeper.retention_enabled），里面写清 events 的真实状态
//     （disabled，等 orgs.retention_sec）：否则只靠「真删了东西才打日志」，运维无法区分
//     「清理跑过但没东西可删」和「清理根本没开」，更分不清「events 被禁用」和「events 零命中」。
//  3. 先写回 lastRetention 再启动：失败也记时刻。否则失败后每条 tick（5s）
//     都会重试，库抖动时反而变成高频打库，把「自愈」变成「雪崩」。
//  4. 重叠守卫：上一轮还没跑完（retentionBusy）→ 跳过本轮，不排队也不叠加。
//     低频门是 1 小时，正常不会撞上；守卫防的是「上一轮跑了超过一个周期」——
//     无索引时单轮是整 org 扫描 + TEMP B-TREE 排序（契约 §6），确实可能很慢。
//     跳过时不推进 lastRetention：这一轮什么都没做，下一 tick 还会再试。
//  5. 启动方式：retentionWG.Add → go runRetentionSweep；ctx 取消后 store 层自己检查
//     ctx.Err() 提前返回，这里不额外加逻辑。失败只记日志，绝不影响下一轮巡检
//     （巡检是自愈机制，不能停摆）。
func (s *Sweeper) maybeSweepRetention(ctx context.Context, now int64) {
	if s.Retention == nil {
		return
	}

	// 频率门：0 表示还没跑过 → 首轮立即执行。
	if s.lastRetention != 0 && now-s.lastRetention < int64(s.retentionInterval().Seconds()) {
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
	// events 的真实状态写出来**——本版 events 保留期是禁用的（lease 不注入 → store 收到 0
	// → 整段跳过）。少了这一句，运维看到「没有 events_deleted」只能猜「扫了但没东西可删」，
	// 而事实是「压根没扫 events」。
	// 只打一次——写完 lastRetention 后这个分支不再进入。位置在启动 goroutine 之前，
	// 这样「调度已启用」一定先于「本轮删了多少」（后者可能要很久）。
	if s.lastRetention == 0 {
		slog.Info("retention sweeper enabled",
			"event", "lease.sweeper.retention_enabled",
			"interval", s.retentionInterval().String(),
			"batch", s.retentionBatch(),
			"batches", s.retentionBatches(),
			// events 状态固定用 eventRetentionLogState 这一种写法（见其注释）。
			"event_retention", eventRetentionLogState,
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
// 租约回收/重派一起堵住、ticker 还会丢 tick；而无索引时单轮清理是「整 org 扫描 +
// TEMP B-TREE 排序」（契约 §6），一轮可能很久。
//
// Now 在 goroutine 里重新取：以「真正开始跑的时刻」算 cutoff，比 Tick 里那个可能已经
// 过去若干秒的 now 更准（Tick 与执行之间隔着调度延迟，甚至上一次卡住的轮次）。
//
// **这里永远不给 EventRetentionSec 赋值**（保持零值 0 = 禁用）：本版没有 events 保留期的
// 开关，也没有 per-org 配置，只有 `orgs.retention_sec` 落地后才由组织配置开启。0 让 store
// 跳过整个 events 阶段（report.EventsDisabled = true），因此升级后不会删任何 events。
// 回归断言：retention_test.go 的 TestRetentionNeverEnablesEventSweep 会盯住这一点。
func (s *Sweeper) runRetentionSweep(ctx context.Context) {
	defer s.retentionWG.Done()
	// LIFO：先解除守卫再 Done —— 同包测试 Wait 返回时守卫已经放开，可以再 Tick。
	defer s.retentionBusy.Store(false)

	// RetentionOptions 里**故意不填 EventRetentionSec**（保持零值 0 = 禁用，0 不是「保留 0 秒」）：
	// 本版没有 events 保留期的开关，也没有 per-org 配置；store 见到 <=0 就跳过整个 events
	// 阶段并置 EventsDisabled=true。给它填值就等于「升级后不可逆地删 events」，要开启必须等
	// orgs.retention_sec 落地后由组织配置决定——别在 lease 层加字段改默认。
	report, err := s.Retention.SweepRetention(ctx, store.RetentionOptions{
		Now:     s.now(),
		Batch:   s.retentionBatch(),
		Batches: s.retentionBatches(),
	})
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
				"orgs", report.Orgs,
				"truncated", report.Truncated,
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
			"orgs", report.Orgs,
			"truncated", report.Truncated,
		)
		return // 不给巡检停摆：下一条 tick 照常回收租约/重派
	}

	// 只在「真删了东西」或「被预算截断（还有积压）」时打日志：
	// 否则每小时一条空转日志，纯噪音。
	if report.TasksDeleted+report.EventsDeleted > 0 || report.Truncated {
		slog.Info("retention round finished",
			"event", "lease.sweeper.retention",
			"tasks_deleted", report.TasksDeleted,
			"events_deleted", report.EventsDeleted,
			"events_disabled", report.EventsDisabled,
			"task_scopes", report.TaskScopes,
			"orgs", report.Orgs,
			"truncated", report.Truncated,
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
