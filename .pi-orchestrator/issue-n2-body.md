> #219（pending 只靠 5s 巡检排空）的核心已由 `5ba1c81`（PR #277）修掉：`Sweeper.spur` + `Nudge()`，
> ws 的 result 终态与 hello 握手都会立即触发一次重派，巡检降级为兜底（CONTRACT §3.3，实测 p99 89.3s → 0.33s）。
> 这里记录**同一批修复没覆盖到的两个缺口**，不重开 #219。

## 缺口 A：`CancelTask` 释放槽位却不 Nudge —— 最坏 5s 空槽

`internal/store/task.go:1441-1445`：cancel 先把任务推终态，再 `ReleaseInstanceByTask` 释放槽位；**释放后没有任何人知道槽位空了**。

全仓唯一的 nudge 接线是 `internal/server/mux.go:364`（`nodeH.OnCapacityFreed = sweeper.Nudge`），只覆盖 ws 的两条路径。`consoleHandler` 结构体（`console_handlers.go:28-43`）既没有 sweeper 也没有 nudge 入口。

⇒ cancel 之后被释放的槽位要等下一次巡检 Tick（`LeaseSweepIntervalSec = 5`，`task.go:39`），**0~5s 的空槽窗口**。
这与契约 `docs/contracts/CONTRACT.md:654` 的措辞直接冲突：*「任务终态落库（槽位释放）或节点握手成功时，中枢会**立即**触发一次重派」*——cancel 正是「任务终态落库（槽位释放）」。

附带：`CancelTask` 的终态提交与槽位释放是两条独立 Exec，无事务无补偿；释放失败时接口 500 但任务已终态、槽位仍占着（与 ws 路径「槽位释放并入终态同一事务」口径不同）。

## 缺口 B：`ErrNoCapacity` 被写成「坏节点」10s 退避 —— 即时重派对重试过的任务失效

- `internal/route/dispatch.go:1622-1626`：`LeaseAndAssign` 返回 `ErrNoCapacity` → `return false, nil`，与「没有节点声明该能力」（`:1589-1593`）**同一个返回值**。
- `internal/server/mux.go:334-356`：对 `!ok` 一律 `backoff := store.RetryBackoff(task.Attempts)` → `SetNotBefore(now+backoff)`。
- `RetryBackoff`（`internal/store/task.go:1507-1517`）= `{10,60,300}`；`attempts` 在**派发时就 +1**（`task.go:679-686`）⇒ 任何被重派过一次的任务，只要撞上一次容量不足就被写 `now+10s`。
- `ListRedispatchable` 按 `not_before` 过滤（`task.go:1538-1543`）⇒ 这 10 秒内**每一次 Nudge、每一轮巡检都看不见它**。

⇒ #219 的即时重派对 `attempts==0` 的新任务有效，对 `attempts>=1` 的任务退化成 10s 定时。而容量不足根本不是节点故障，写退避没有道理（现有测试抓不到：`sweeper_test.go:224` 用的任务 `Attempts` 零值）。

同类问题（同一函数家族）：`requeueAfterPublishFailure`（`dispatch.go:1298-1339`）经 `SweepExpiredLeases → sweepOne` 写 `not_before = now + RetryBackoff(attempts)`（`task.go:1402`），而它的注释 `dispatch.go:1127-1130` 写的是「退回 pending 让它能被下一次派发**立刻**捡起」。

## 建议修法

1. `RedispatchTask` 在容量不足时返回可识别的结果（例如 `store.ErrNoCapacity`），调用方据此**不写退避**；「无节点声明 / 节点故障」继续退避。
2. `consoleHandler` 注入 nudge（照 `taskLogs` 的窄依赖写法），cancel 成功后调用。
3. 契约 §3.3 同步两句：cancel 也是「槽位释放」；退避只用于节点侧失败。

## 验收

- `attempts=1` 的 pending 任务撞容量不足后 `not_before` 仍为 NULL，下一次 Nudge 能把它捞出来（修复前为 10s）
- cancel 成功后 fake nudge 被调用一次；cancel 失败（409）不调用
- 现有 `TestSweeperNudgeRedispatchesImmediately` 等 #219 护栏保持绿
