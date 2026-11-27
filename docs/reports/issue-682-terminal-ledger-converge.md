<!-- snapshot-doc: 2026-11-27 #682 交付时的定格（门禁与变异输出贴在下文；r2 已按 PR review 重跑 §4（每条变异都重打一次）、补 §2 的第二种放宽与 §3 的三条新用例） -->

# #682 · 终局任务的两本账收敛（`ConvergeAbandonedLedgers` → `ConvergeTerminalLedgers`）

分支 `pi-subagent/issue-682-r1`。交付项：判据从「只认 abandoned」放宽到**全部终态** + 取消在飞的宽限 +
store 侧 6 条收敛用例（含现场同款重排泄露）+ 1 条相位同源断言 + 1 条巡检接线用例 + 5 条变异自证。

## 1. 缺陷（issue #682 实测 + 代码定位）

`#660`/`#663` 只收敛了 `abandoned` 终局。dev 实测（2026-11-27）`succeeded` 的任务同样占着槽：

```
node_id                        slot busy_task      task.phase
nd_BCq3yWTPcWRehDvbYz(host-ws) 0    tsk_BCr480sj…  succeeded     ← 占着不放
nd_BCq3yWTPcWRehDvbYz          1    tsk_BCr5XgH…   running
task_attempts.reaped_at IS NULL → 4 条
```

| 账 | 未收敛的后果 | 代码锚点 |
| --- | --- | --- |
| `node_instances.busy_*` | 该机 `busy=2/2` 但实际上只有 1 个在跑 → 再也接不了新任务（`#633` 等了 40 分钟 `slot_unavailable`） | `releaseSlotBindingOn`（`internal/store/node.go:1109`） |
| `task_attempts.reaped_at IS NULL` | 预算归因继续把死掉的 attempt 算成「可能占着物理槽」 | `countUnreaped`（`internal/store/execution_auth.go`） |

**根因**：写路径（`completeTaskAuthorized` / `cancel.ack` / 实例观测）只在「它自己那一代执行」上做精确
释槽；一旦那代执行被重排（`recoverOne` 的 requeue 分支故意保留旧槽与 `reaped NULL`，等节点事实），
旧槽就只能等节点上报 idle——而节点在任务终局后不再报它。巡检（`RecoverLostLeases`）只看 `running`，
终态任务进不了候选集，于是**没有任何机制会把这一格收回来**（issue：「漏了之后不会再被任何机制收回来」）。

现场同款的最小复现（`TestConvergeTerminalLedgersRecoversSlotLeakedByRetry`）：
attempt 1 租约失联 → 重排（旧槽仍压着）→ attempt 2 换槽成功 → 任务 `succeeded` → 旧槽永久丢失。

## 2. 改动

1. `internal/store/ledger_converge.go`：`ConvergeAbandonedLedgers` → **`ConvergeTerminalLedgers`**。
   判据（#682 建议 1）从 `phase = 'abandoned'` 放宽成 `phase IN (<IsTerminalPhase 的四个>)`
   ——占位符来自 `terminalPhaseIn()`，沿用「终态只有一份事实」的既有约定，不在这里另抄字面量。
   动作不变（与 #660/#663 逐字一致）：同一 `BEGIN IMMEDIATE` 事务里补 `task_attempts.reaped_at`
   （`reclaim_reason='sweep_converge'`）+ 清 `node_instances.busy_*`，两项计数进 `LedgerConvergence`。
2. `internal/lease/lease.go`：`Sweeper.RunOnce` 改调新方法（位置不变），`Report.LedgersConverged`
   字段与 JSON（`ledgersConverged` / `attemptsReaped` / `slotsReleased`）**一律不改**——没有 wire 变更；
   日志文案从 `converged abandoned ledgers` 改为 `converged terminal ledgers`。包注释同步。
3. 用例：`internal/store/ledger_terminal_converge_test.go`（新增）+ `lease_abandon_test.go` 改名对齐 +
   `internal/lease/lease_test.go` 加巡检接线用例。

### 契约边界（有意为之，别当成漏做）

`#660` 冻结的边界是：**`processGroupReaped` 才是「进程组真回收」的权威事实**，收敛抢先写 reaped
会把这条事实写假。所以 `cancel_requested_at IS NOT NULL` 的行仍不会被无条件收敛，而是给了
**宽限 `terminalSlotGrace = 600s`**：

- 宽限内：原样留给 `cancel.ack`（`TestConvergeTerminalLedgersHonoursCancelAckGrace` 前半段断言，
  `#660` 的 `TestConvergeTerminalLedgersIsIdempotent` 后半段也仍然守着这条）；
- 宽限过后仍终态且仍占槽：只能承认 ack 不会再来（节点离线后再不出现 / ack 丢失），由收敛兜底，
  否则这一格容量永久丢失——这正是 #682 的标题缺陷。

600s 的取法：巡检间隔（`lease.DefaultInterval=30s`）的 20 倍，远大于取消的 TERM→KILL
（`protocol.LeaseTermGrace=5s`）与结果上报窗口；它只影响「ack 永远不会来」的病态行，正常 ack
早在宽限内落账。

**第二种放宽（review 明确要求写出来）：`cancel.ack{processGroupReaped:false}` 也挡不住宽限后的收敛。**
`internal/protocol/exec_frames.go:235` 把 ack=false 定义为「进程不保证停得住」的权威事实，这层语义没有变：
宽限内中枢一字不动（`AckCancelAuthorized` 收到 false 直接 return，相位/槽/租约都不改），但它只是**一次性事件**——
只要任务已终态、`cancel_requested_at` 已过 600s、槽还指着它，收敛仍会补 `reaped_at` + 释槽。
所以「ack 明说没回收」与「ack 不会再来」在**宽限之后**被同样对待，这是对 #660 契约的实质放宽：

- 危害级别是契约/诊断级，不是容量级：节点侧 PGID 账本挡住同一槽重跑，`runner.acquireSlotLocked`
  在槽真被占着时 `reject slot_unavailable`（fail closed）；grant 预算本来就把终态执行排除在外。
- 代价是 `reaped_at` 的诚实性：宽限之后这一列的含义降级为「中枢不再等这条执行」，而不是
  「进程组已回收」；`reclaim_reason='sweep_converge'` 是区分它与 `cancel_ack` 的唯一标签。
- 仍然这么做的理由：另一种选择（cancel 在飞永不收敛）就是 #682 的缺陷本身——节点永久离线时那一格容量
  再也回不来；600s 宽限已把正常 ack（TERM 5s + 上报窗口）排除在外。
- 用例：`TestConvergeTerminalLedgersReclaimsAfterAckReportsNotReaped` 把 ack=false 的现场走一遍
  （ack 后原样 → 宽限内 `0/0` → 宽限后 `1/1` 且 `sweep_converge`）。

**为什么提前释槽不会双跑**：中枢提前认为某格空了只会去派新任务，而节点侧 `runner.acquireSlotLocked`
在槽真被占着时 `reject slot_unavailable`（fail closed，`cmd/anode/internal/runner/runner.go:459`），
不会在同一个物理槽上并发跑两个进程组。

## 3. 验收对账

| issue 验收项 | 用例 | 判据 |
| --- | --- | --- |
| ① succeeded / failed / cancelled 各造一条「终局但仍占槽」的行，收敛后 `busy_*` 全空、`reaped_at` 非空 | `TestConvergeTerminalLedgersReleasesTerminalSlots`（4 个 subtest，含 abandoned 回归） | 首轮 `1/1`、`GetTaskAttempt().ReapedAt != nil` 且 `reclaim_reason='sweep_converge'`、`v4BusySlots==0` |
| ② 幂等：连跑两次，第二次 `released=0` | 同上用例尾段 + `TestConvergeTerminalLedgersIsIdempotent` | 第二轮 `0/0`，且 reaped 行逐字段不变 |
| 取消在飞不得抢先写 reaped（#660 边界） | `TestConvergeTerminalLedgersHonoursCancelAckGrace` | 宽限内 `0/0` 且 `busy==1`；宽限后 `1/1` 且 `busy==0` |
| 现场同款：重排泄露的那一格要能自己回来 | `TestConvergeTerminalLedgersRecoversSlotLeakedByRetry` | 收敛前 `busy==1`、旧 attempt `reaped NULL`；收敛后 `busy==0`、`sweep_converge`，且第三个任务能拿到槽 |
| 接在 `Sweeper.RunOnce` 里（issue 建议 3） | `internal/lease`: `TestRunOnceConvergesTerminalTaskSlots` | `Report.LedgersConverged == {1,1}`、槽账清空、第二轮 `0/0` |
| 非终态（running / pending 占位）不得被放宽后的判据收敛 | `TestConvergeTerminalLedgersLeavesNonTerminalPhasesAlone` | 两个 subtest 都 `{0,0}`、`busy==1`、`attempt.ReapedAt==nil`、相位不变 |
| ack 明说 `processGroupReaped=false`（§2 的第二种放宽） | `TestConvergeTerminalLedgersReclaimsAfterAckReportsNotReaped` | ack 后原样；宽限内 `0/0`；宽限后 `1/1` + `sweep_converge` |
| 「终态只有一份事实」：SQL 占位符与 `IsTerminalPhase` 同源 | `TestTerminalPhaseInMatchesIsTerminalPhase`（`retention_test.go`） | 逐相位 `listed[p] == IsTerminalPhase(p)`，占位符数/参数数/合法相位/去重一并钉住 |

**真机验收（issue ③）**：本分支只做到「单测 + 巡检接线」；跑完一单后 `node_instances` /
`task_attempts` 无残留需要在 dev 机器上跑一轮真实任务由协调者复核（本地没有 dev 库）。
升级到本分支后，存量那 4 条 `reaped_at IS NULL` + 占槽行会在**下一轮巡检（≤30s）**被就地收敛，
不需要手工清库（`TestConvergeTerminalLedgersRecoversSlotLeakedByRetry` 覆盖的正是这个形状）。

## 4. 变异自证（五条都必红；输出是 86f91ca4 之后在本分支重跑的原样截取）

> 复算方式：对 `internal/store/ledger_converge.go`（第五条对 `retention.go`）逐个打变异，跑
> `go test ./internal/store/ -run 'TestConvergeTerminalLedgers' -count=1`（第五条 `-run 'TestTerminalPhaseInMatches'`），
> 跑完 `git checkout --` 还原。下面行号对应当前提交的测试文件（旧快照里 `:121/:139/:189` 是另一个版本的行号，已作废）。

```
# 变异 1「判据不放宽」：phases 换回只认 abandoned（phases, phaseArgs := "?", []any{TaskAbandoned}）
--- FAIL: TestConvergeTerminalLedgersReleasesTerminalSlots (0.22s)
    --- FAIL: TestConvergeTerminalLedgersReleasesTerminalSlots/succeeded (0.06s)
        ledger_terminal_converge_test.go:60: 首次收敛应各补 1 行，得到 {AttemptsReaped:0 SlotsReleased:0}
    --- FAIL: TestConvergeTerminalLedgersReleasesTerminalSlots/failed (0.08s)
        ledger_terminal_converge_test.go:60: 首次收敛应各补 1 行，得到 {AttemptsReaped:0 SlotsReleased:0}
    --- FAIL: TestConvergeTerminalLedgersReleasesTerminalSlots/cancelled_ack_lost (0.04s)
        ledger_terminal_converge_test.go:60: 首次收敛应各补 1 行，得到 {AttemptsReaped:0 SlotsReleased:0}
--- FAIL: TestConvergeTerminalLedgersHonoursCancelAckGrace (0.04s)
    ledger_terminal_converge_test.go:132: 宽限过后必须收敛（ack 不会再来），得到 {AttemptsReaped:0 SlotsReleased:0}
--- FAIL: TestConvergeTerminalLedgersRecoversSlotLeakedByRetry (0.04s)
    ledger_terminal_converge_test.go:180: 重排泄露的那一格应被收敛：期望 1/1，得到 {AttemptsReaped:0 SlotsReleased:0}
--- FAIL: TestConvergeTerminalLedgersReclaimsAfterAckReportsNotReaped (0.09s)
    ledger_terminal_converge_test.go:298: 宽限后仍占槽 = 那一格再也回不来，必须收敛，得到 {AttemptsReaped:0 SlotsReleased:0}
FAIL
FAIL	github.com/zzjcool/anotify/internal/store	0.669s
FAIL

# 变异 2「只补 reaped、不释槽」：slot UPDATE 加 AND 0
--- FAIL: TestConvergeTerminalLedgersIsIdempotent (0.12s)
    lease_abandon_test.go:153: 首次收敛应各补 1 行，得到 {AttemptsReaped:1 SlotsReleased:0}
--- FAIL: TestConvergeTerminalLedgersReleasesTerminalSlots (0.28s)
    --- FAIL: TestConvergeTerminalLedgersReleasesTerminalSlots/succeeded (0.07s)
        ledger_terminal_converge_test.go:60: 首次收敛应各补 1 行，得到 {AttemptsReaped:1 SlotsReleased:0}
    --- FAIL: TestConvergeTerminalLedgersReleasesTerminalSlots/failed (0.04s)
        ledger_terminal_converge_test.go:60: 首次收敛应各补 1 行，得到 {AttemptsReaped:1 SlotsReleased:0}
    --- FAIL: TestConvergeTerminalLedgersReleasesTerminalSlots/cancelled_ack_lost (0.10s)
        ledger_terminal_converge_test.go:60: 首次收敛应各补 1 行，得到 {AttemptsReaped:1 SlotsReleased:0}
    --- FAIL: TestConvergeTerminalLedgersReleasesTerminalSlots/abandoned (0.06s)
        ledger_terminal_converge_test.go:60: 首次收敛应各补 1 行，得到 {AttemptsReaped:1 SlotsReleased:0}
--- FAIL: TestConvergeTerminalLedgersHonoursCancelAckGrace (0.04s)
    ledger_terminal_converge_test.go:132: 宽限过后必须收敛（ack 不会再来），得到 {AttemptsReaped:1 SlotsReleased:0}
--- FAIL: TestConvergeTerminalLedgersRecoversSlotLeakedByRetry (0.04s)
    ledger_terminal_converge_test.go:180: 重排泄露的那一格应被收敛：期望 1/1，得到 {AttemptsReaped:1 SlotsReleased:0}
--- FAIL: TestConvergeTerminalLedgersReclaimsAfterAckReportsNotReaped (0.04s)
    ledger_terminal_converge_test.go:298: 宽限后仍占槽 = 那一格再也回不来，必须收敛，得到 {AttemptsReaped:1 SlotsReleased:0}
FAIL
FAIL	github.com/zzjcool/anotify/internal/store	0.658s
FAIL

# 变异 3「取消在飞不给宽限」：宽限条件恒真（OR 1=1）
--- FAIL: TestConvergeTerminalLedgersIsIdempotent (0.10s)
    lease_abandon_test.go:153: 首次收敛应各补 1 行，得到 {AttemptsReaped:2 SlotsReleased:2}
--- FAIL: TestConvergeTerminalLedgersHonoursCancelAckGrace (0.08s)
    ledger_terminal_converge_test.go:114: 宽限内的取消在飞行不得被收敛，得到 {AttemptsReaped:1 SlotsReleased:1}
--- FAIL: TestConvergeTerminalLedgersReclaimsAfterAckReportsNotReaped (0.08s)
    ledger_terminal_converge_test.go:287: 宽限内必须继续等节点回收事实，得到 {AttemptsReaped:1 SlotsReleased:1}
FAIL
FAIL	github.com/zzjcool/anotify/internal/store	0.572s
FAIL
```

**变异 4 是 review 指出的「放宽过头」方向——旧用例确实全绿，只有新守卫会红：**

```
# 变异 4a「只排除 pending」：phase IN (?,?,?,?) 换成 phase NOT IN ('pending') 并丢掉 phaseArgs
--- FAIL: TestConvergeTerminalLedgersLeavesNonTerminalPhasesAlone (0.10s)
    --- FAIL: TestConvergeTerminalLedgersLeavesNonTerminalPhasesAlone/running (0.04s)
        ledger_terminal_converge_test.go:230: 非终态（running）的执行不得被收敛，得到 {AttemptsReaped:1 SlotsReleased:1}
FAIL
FAIL	github.com/zzjcool/anotify/internal/store	0.542s
FAIL

# 变异 4b「忘了相位过滤」：phase IN (...) 换成 1=1
--- FAIL: TestConvergeTerminalLedgersLeavesNonTerminalPhasesAlone (0.09s)
    --- FAIL: TestConvergeTerminalLedgersLeavesNonTerminalPhasesAlone/pending_reservation (0.05s)
        ledger_terminal_converge_test.go:230: 非终态（pending）的执行不得被收敛，得到 {AttemptsReaped:1 SlotsReleased:1}
    --- FAIL: TestConvergeTerminalLedgersLeavesNonTerminalPhasesAlone/running (0.04s)
        ledger_terminal_converge_test.go:230: 非终态（running）的执行不得被收敛，得到 {AttemptsReaped:1 SlotsReleased:1}
FAIL
FAIL	github.com/zzjcool/anotify/internal/store	0.587s
FAIL
```

两条变异的覆盖面对应 review 的两种写法：4a 能杀掉 `NOT IN ('pending')`（running 红、pending 仍绿），
4b 才能杀掉「忘了相位过滤」（pending 也红）。旧的三条用例在 4a/4b 下**全部保持绿**，
也就是说没有 `TestConvergeTerminalLedgersLeavesNonTerminalPhasesAlone` 这两个 subtest，
这两种会去放 running 任务槽的放宽都能带着全绿 CI 合入。

```
# 变异 5「终态两份事实分叉」：terminalPhaseIn() 少列 abandoned
--- FAIL: TestTerminalPhaseInMatchesIsTerminalPhase (0.00s)
    retention_test.go:317: terminalPhaseIn() 与 IsTerminalPhase("abandoned") 不一致：SQL false / Go true
FAIL
FAIL	github.com/zzjcool/anotify/internal/store	0.012s
FAIL
```

## 5. 门禁实跑（r2 本机）

```
$ go test ./internal/store/ ./internal/lease/ -count=1
ok  	github.com/zzjcool/anotify/internal/store	32.554s
ok  	github.com/zzjcool/anotify/internal/lease	5.140s
$ go test ./internal/store/v4test/ -count=1
ok  	github.com/zzjcool/anotify/internal/store/v4test	92.103s
$ gofmt -l internal/store/ internal/lease/   # 空
$ go vet ./internal/store/ ./internal/lease/ # 空
$ node scripts/check-comment-density.mjs
✅ 与基线完全一致（注释行 42889 / 基线 42889，目录 0 个不一致）
$ node scripts/{check-agents,check-annotations,check-doc-links,check-doc-endpoints,check-test-discipline}.mjs
✅ 全绿（细节见 PR 评论 / r2 返工说明）
```

`make ci` 的全量输出见 PR 正文与提交说明；本次 r2 只动了测试与 `docs/reports`（`ledger_converge.go` 一行未改，
注释行因此仍与基线逐字节相等）。

## 6. 为什么代码注释这么短（不是漏写）

`make ci` 里的 `node scripts/check-comment-density.mjs` 是**双向棘轮**：目录注释行、仓库总量既不许超
也不许低于基线（基线只降不升，`--update` 抬高会被拒）。所以本 PR 新增的注释行必须与删掉的行数**逐行相抵**
（`internal/store` / `internal/lease` 两个目录的计数现在都与基线**完全相等**）。

代价就是代码里只留指向本文件的指针（`ledger_converge.go` 的方法 doc + `terminalSlotGrace` 那一行），
**判据、宽限理由、为什么不双跑、验收对账、变异自证全部落在本文**（`docs/reports` 属于 evidence 面，
按 PR 增长，不设上限——这是仓库对「为什么」的指定存放位置）。
