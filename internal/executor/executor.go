// Package executor 执行**中枢自己**的能力（内置能力的 Central 那部分）。
//
// 为什么需要它：`notify.phone` 是图里的一条边，但它没有节点执行者——
// 没人会写一个"发推送节点"，推送是中枢自己的能力。派发器把这这类候选
// 建任务时不挑机器（node_id 为 NULL），任务停在 pending，等这里领取。
//
// 与节点执行的区别：
//
//	节点执行：assign 帧 → 节点 ack/progress/result → 状态机推进
//	中枢执行：本包直接推进状态机（pending → assigned → running → 终态）
//
// 状态机规则完全一样（九态、终态不可变、唯一写入口）。中枢执行不是
// 一条旁路，只是"执行者"从中枢外面的节点换成了中枢自己。
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/zzjcool/anotify/internal/push"
	"github.com/zzjcool/anotify/internal/store"
)

// Store 是执行器需要的数据层能力（*store.DB 满足）。
type Store interface {
	// ClaimCentralTasks 领取「中枢执行的 pending 任务」。
	//
	// 判定条件：status=pending 且 capability 是内置 Central 能力。
	// 领到后推进到 running（跳过 assigned/acked：没有节点参与，那两个
	// 状态是"等节点接单/开始"的意思，对中枢执行没有意义）。
	//
	// **全局扫描**：中枢任务没有节点，而命名空间不参与「谁能执行」的判定，
	// 所以不按命名空间过滤；领到后每条任务自己带 NamespaceID，后续写入都按它。
	ClaimCentralTasks(ctx context.Context, limit int) ([]*store.Task, error)
	// FinishTask 把 running 任务推进到终态并写回报。
	//
	// namespaceID 必须来自 task.NamespaceID：中枢执行是全局的，但每条任务的
	// 终态写入仍只在它自己的命名空间内生效（I10）。
	FinishTask(ctx context.Context, namespaceID, taskID, status string, out *string, reported bool, summary, errMsg string) error
	// CentralCapabilityNames 返回中枢执行的能力名（用于挑选待办）。
	CentralCapabilityNames() []string
}

// Notifier 是 `notify.phone` 的执行依赖。
//
// 依赖方向：executor（编排）→ push（基础设施）。反过来会让 push
// 这个纯发送层依赖编排逻辑，层次颠倒。
//
// 接口留在这里而不是直接用 *push.CentralNotifier：executor 的测试
// 需要注入假实现，不该被迫构造真实的 VAPID sender。
type Notifier interface {
	// NotifyUser 给某用户的所有匹配设备发推送，返回实际投递成功的设备数。
	NotifyUser(ctx context.Context, userID string, n push.NotifyMessage) (sent int, err error)
}

// notifyPhoneParams 是 `notify.phone` 的 params 解码结果（含执行所需的目标）。
type notifyPhoneParams struct {
	push.NotifyMessage
	To string `json:"to"`
}

// DefaultInterval 是中枢执行器的轮询间隔。
const DefaultInterval = time.Second

// Executor 执行中枢能力。
type Executor struct {
	Store    Store
	Notifier Notifier

	// BatchLimit 是每轮最多领多少条（防止一次积压把推送服务打爆）。
	BatchLimit int
}

// New 构造默认配置的执行器。
func New(st Store, n Notifier) *Executor {
	return &Executor{Store: st, Notifier: n, BatchLimit: 50}
}

func (e *Executor) batchLimit() int {
	if e.BatchLimit > 0 {
		return e.BatchLimit
	}
	return 50
}

// Tick 执行一轮：领取中枢任务并逐条执行。
//
// 单条失败不影响其他条：一条推送发不出去不该阻塞整批。
func (e *Executor) Tick(ctx context.Context) {
	tasks, err := e.Store.ClaimCentralTasks(ctx, e.batchLimit())
	if err != nil {
		slog.Error("claim central tasks failed",
			"event", "executor.claim_failed",
			"error", err.Error(),
		)
		return
	}
	if len(tasks) == 0 {
		return
	}

	var ok, failed int
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		if err := e.runOne(ctx, task); err != nil {
			failed++
			slog.Warn("central task failed",
				"event", "executor.task_failed",
				"task_id", task.ID,
				"capability", task.Capability,
				"error", err.Error(),
			)
			// 执行失败要落成 failed 终态，不能静默留在 running——
			// 否则任务会卡到租约巡检，而中枢任务没有租约。
			if ferr := e.Store.FinishTask(ctx, task.NamespaceID, task.ID,
				store.TaskFailed, nil, false, "", err.Error()); ferr != nil {
				slog.Error("mark central task failed",
					"event", "executor.mark_failed",
					"task_id", task.ID,
					"error", ferr.Error(),
				)
			}
			continue
		}
		ok++
	}

	slog.Info("central task round finished",
		"event", "executor.round",
		"ok", ok,
		"failed", failed,
	)
}

// runOne 执行单条中枢任务。
func (e *Executor) runOne(ctx context.Context, task *store.Task) error {
	switch task.Capability {
	case "notify.phone":
		return e.runNotifyPhone(ctx, task)
	default:
		return fmt.Errorf("unknown central capability %q (registered as Central but has no executor)", task.Capability)
	}
}

// runNotifyPhone 执行 `notify.phone`：按 params.to 给该用户发推送。
//
// 参数就是通知内容本身（不变量 I6：不做模板替换、不做表达式求值）。
// `to` 的组织归属已由派发器的 foreign_user 校验保证过（契约 §10.1.1），
// 这里不再重复校验——重复校验会产生两个事实源。
func (e *Executor) runNotifyPhone(ctx context.Context, task *store.Task) error {
	var p notifyPhoneParams
	if err := json.Unmarshal(task.Params, &p); err != nil {
		return fmt.Errorf("decode notify.phone params: %w", err)
	}
	if p.To == "" {
		return fmt.Errorf("notify.phone: params.to is required")
	}
	if e.Notifier == nil {
		return fmt.Errorf("notify.phone: no notifier configured (VAPID missing?)")
	}

	sent, err := e.Notifier.NotifyUser(ctx, p.To, p.NotifyMessage)
	if err != nil {
		return fmt.Errorf("notify.phone send: %w", err)
	}

	// out 三态（不变量 I5）：notify.phone 是终端能力，没有出口，
	// 所以回报 reported=1 且 out=nil，表示"已回报，到头了"。
	summary := fmt.Sprintf("pushed to %d device(s)", sent)
	if err := e.Store.FinishTask(ctx, task.NamespaceID, task.ID,
		store.TaskSucceeded, nil, true, summary, ""); err != nil {
		return fmt.Errorf("mark notify.phone succeeded: %w", err)
	}

	if sent == 0 {
		// 没有匹配设备不算失败：用户可能全关了推送。任务成功，
		// 但 summary 说清楚"发给了 0 台"，避免用户以为推了。
		slog.Info("notify.phone delivered to no device",
			"event", "executor.notify.no_device",
			"task_id", task.ID,
			"user_id", p.To,
		)
	}
	return nil
}

// Run 常驻执行，直到 ctx 取消。
//
// interval <= 0 时用 DefaultInterval（1s）：推送是用户可感知的动作，
// 秒级延迟可以接受；再短只是空转查库。
func (e *Executor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	slog.Info("central executor started",
		"event", "executor.start",
		"interval", interval.String(),
	)

	for {
		select {
		case <-ctx.Done():
			slog.Info("central executor stopped", "event", "executor.stop")
			return
		case <-t.C:
			e.Tick(ctx)
		}
	}
}
