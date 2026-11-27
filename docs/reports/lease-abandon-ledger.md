<!-- snapshot-doc: 2026-11-26 #660 交付时的定格（门禁与变异输出原样贴在下文，复算命令见 §6） -->
<!-- r2（#682 review）：正文符号改用现名 ConvergeTerminalLedgers / TestConvergeTerminalLedgersIsIdempotent；旧名已不存在于仓库，只在本注保留以便考古。 -->

# #660 · 租约 abandon 的两本账收敛

分支 `fix/660-lease-abandon-ledger`。交付项：写路径同事务收敛 + 预算读时兜底 + 存量收敛 + 4 条验收用例 + 2 条变异自证。

## 1. 缺陷（issue #660 实测 + 代码定位）

`lease sweep` 把任务判 `abandoned` 时只写 `tasks.phase`，两本账都还开着：

| 账 | 未收敛的后果 | 代码锚点 |
| --- | --- | --- |
| `task_attempts.reaped_at IS NULL` | `countUnreaped` 把它算成「仍可能占着物理槽」，`DefaultLimitsTotal=1` 时预算恒 1/1 → 之后每个任务 `slot_unavailable` | `internal/store/execution_auth.go` `grantBudgetOn` |
| `node_instances.busy_*` | 槽位再等不到释放它的调用点（`ReclaimStaleSlots` 要节点事实、`cancel.ack` 不会再来了） | `internal/store/node.go` `releaseSlotBindingOn` |

实测证据（issue 评论）：删掉 `node_instances` 那行后任务**仍然**被拒，
`select a.task_id, a.attempt, a.reaped_at, t.phase … where a.reaped_at is null` 显示
`attempt=1, reaped_at=NULL, phase=abandoned` —— 第二本账才是真正的闸。

## 2. 改动

1. **写路径**（`internal/store/task.go` `recoverOne` 的 default 分支）：终局与相位写入同一个事务里
   调 `finishAttemptOn(..., reaped=true)`（落 `reaped_at` + `reclaim_reason='lease_abandon'`，并按
   attempt 行绑定的 instance/task/attempt 精确释槽），再用任务行钉住的 `lease_instance_id` 兜底
   释放一次（attempt 行缺失的坏数据也不会把槽永久钉死）。
   `requeue` 分支与**取消在飞**分支原样不动：旧进程可能还在跑，槽位仍等可信事实。
2. **读时兜底**（`internal/store/execution_auth.go` `countUnreaped`）：统计未回收执行时排除
   **已终态的任务**（`t.phase NOT IN (<terminal>)`）。物理事实仍是 `reaped_at`/`busy_*`，
   这条只影响预算归因——否则一条遗留的 abandoned attempt 就能永久钉死节点，而补它的调用点早就不存在。
3. **存量收敛**（`internal/store/ledger_converge.go` `ConvergeTerminalLedgers`，当时叫
   `ConvergeAbandonedLedgers`）：
   补齐 `abandoned` 任务遗留 attempt 的 `reaped_at`（`reclaim_reason='sweep_converge'`），
   清掉指向它们的槽行；一轮一个 `BEGIN IMMEDIATE` 事务，返回两项计数。接进
   `internal/lease` 的 `Sweeper.RunOnce`（`Report.LedgersConverged` + `Info` 日志，只在该轮
   有命中时打）。幂等：第二轮两项计数均为 0。
4. **契约边界（有意为之）**：收敛面**只覆盖 `abandoned` 且 `cancel_requested_at IS NULL`** 的行。
   取消在飞的终态归 `cancel.ack`（CONTRACT §cancel 槽位等 processGroupReaped），抢先写 reaped
   会把「进程组是否真回收」写假；这一类靠第 2 条的读时兜底恢复容量，物理槽仍按 ack / 实例事实释放。

## 3. 验收对账

用例都在 `internal/store/lease_abandon_test.go`（`package store`，走真实 grant → 租约过期 → sweep）：

| 验收项 | 用例 | 判据 |
| --- | --- | --- |
| ① sweep 后 reaped 非空 + 无占用 | `TestLeaseSweepAbandonSettlesBothLedgers` | `GetTaskAttempt().ReapedAt != nil` 且 `reclaim_reason='lease_abandon'`；`v4BusySlots==0` |
| ② 连续 abandon 不累积 | `TestConsecutiveLeaseAbandonsDoNotLeakCapacity` | 两轮后第三任务 `GrantLeaseAuthorized` 成功（attempt=1、相位 running） |
| ③ 反例：正常 running 仍计入预算 | `TestRunningExecutionStillConsumesBudget` | 总量 1、两个真实槽：第二个任务拿到合法槽仍 `ErrSlotUnavailable`（`机器总量预算已满`），槽账不变 |
| ④ 收敛幂等 | `TestConvergeTerminalLedgersIsIdempotent` | 首轮 1/1、次轮 0/0；遗留行 reaped、取消在飞的行原样保留 |
| 读时兜底（issue 的第二本账） | `TestTerminalUnreapedAttemptDoesNotBlockBudget` | 槽行被人工删掉、只剩终态未回收 attempt 时，新任务仍被预算接受 |

## 4. 变异自证（两条都必红）

```
# 变异 1「只修一边」：终局仍写 reaped，但不释槽（改用直接 UPDATE reaped）
--- FAIL: TestLeaseSweepAbandonSettlesBothLedgers (0.05s)
    lease_abandon_test.go:39: abandon 必须清槽账 busy_*（#660）：仍有 1 个槽被占
--- FAIL: TestConsecutiveLeaseAbandonsDoNotLeakCapacity (0.02s)
    lease_abandon_test.go:67: 第 1 轮 abandon 之后仍占 1 个槽（槽账漏）
FAIL	github.com/zzjcool/anotify/internal/store	3.253s
```

```
# 变异 2「相位条件含 abandoned」：读时兜底把 abandoned 也算未回收
--- FAIL: TestTerminalUnreapedAttemptDoesNotBlockBudget (0.01s)
    lease_abandon_test.go:209: 终态任务的遗留 attempt 不得再占预算（#660），
      得到 slot_unavailable: node nd_… 机器总量预算已满（1/1）
FAIL	github.com/zzjcool/anotify/internal/store	0.219s
```

两条变异都只红在针对性用例上，其余用例保持绿（变异点被精确钉住）。

## 5. 断言方向变更（有意反转，不是迁就实现）

既有用例里 4 处断言把「abandon 后槽位必须保留」写成了通则，与本 issue 的裁决冲突，已改成
「**abandon 终局同事务收敛**；其余终态（取消在飞）等可信事实」：

- `internal/store/v4test/task_test.go` `TestTaskRecoverIdempotentRequeues`：requeue 那格保留（busy=1），
  abandoned 那格释放 + attempt 补 reaped；随后的实例回收事实从 2 条降为 1 条。
- `internal/store/v4test/task_test.go` `TestTaskMaxAttemptsThreeTotal`：第 1/2 次是 requeue（槽保留），
  第 3 次是终局 abandon（槽释放 + reaped 非空）。
- `internal/store/v4test/task_attempt_test.go` `TestTerminalPhaseIsNotProcessFact`：终态样本从
  「非幂等过期 → abandoned」改为「取消在飞 → cancelled」，保住「相位不是物理事实」这条通则的见证。
- `internal/lease/lease_test.go` `TestRunOnceRecoversExpiredNonIdempotentLease`：从「不得释放物理槽」
  改为「abandon 后 reaped 非空 + 槽空」。
- `internal/store/retention_test.go` `TestRetentionKeepsUnreapedAttempts`：「终态但未回收」的造法从
  abandon 改为取消在飞，保留期「执行事实先于任务行消失」的判据一字未减。

## 6. 门禁输出

```
$ go test ./internal/store/... ./internal/lease/... ./internal/server/... ./internal/ws/... -count=1
ok  	github.com/zzjcool/anotify/internal/store	8.483s
ok  	github.com/zzjcool/anotify/internal/store/v4test	23.874s
ok  	github.com/zzjcool/anotify/internal/lease	0.508s
ok  	github.com/zzjcool/anotify/internal/server	56.505s
ok  	github.com/zzjcool/anotify/internal/ws	3.760s
```

```
$ make lint-go     # gofmt + 包注释 + go vet + staticcheck SA*,U1000
（无输出，exit 0；首次运行自动装了 .bin/staticcheck）
$ make contract
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
$ make check-annotations check-test-discipline check-doc-links check-agents tmp-leak-guard check-comment-density
✅ 全绿（注释密度：注释行 43297 / 基线 43297，目录 0 个不一致）
```

全量 `make test`（`go test ./... -count=1 -shuffle=on`）在本机**不是绿的**，但与本改动无关：

- 本机共 52 条失败（`cmd/anode/**` 的 install/real-binary 用例、`internal/artifact`），在**基线提交
  996212a1 的干净归档**里跑 `go test ./... -count=1` 同样是 52 条，失败集合只差两条墙钟型 flake
  （`cmd/anode/internal/{hub,runner}` 互相轮换；这两个包 `go list -deps` 里没有 `internal/store`）。
- `internal/playbook TestExamplePlaybooksInstallable/issue-solve` 只在**我的工作区**红：本地残留一个
  未跟踪的空目录 `examples/playbooks/issue-solve/{capabilities,sources}`（干净归档里没有它，该用例通过）。
  为拿到一次干净的全量结果，我把它临时移出跑了一遍、随后原样移回。
- `make e2e` / `make e2e-affected` 未跑：本机 docker 不可用（本 issue 也不触碰 wire/前端/DOM 契约）。

## 7. 未决 / 需 owner 裁决

1. `docs/contracts/v4/CONTRACT.md:133` 写着「不按 phase/rehello 假回收」。本 issue 的裁决是
   **abandon 终局**用 phase 收敛两本账（并保留 `reclaim_reason='lease_abandon'` 这个诚实标签）。
   建议 contract owner 在该行补一句例外；`docs/contracts/` 是 shrink-only 目录，我没有自行编辑契约文件。
2. 取消在飞的终态行仍会长期保留 `busy_*`（等 `cancel.ack`）。容量已由读时兜底恢复，但如果节点
   永久不再回来，那一格会一直显示占用到节点重连给出实例事实 / 任务被保留期清掉。要不要把这类行
   也纳入收敛（代价是把「进程组是否回收」写假）需要单独立项。
3. 收敛只在 sweep 周期里跑（默认 30s）：升级后第一轮即可恢复被钉死的节点，不需要人工 SQL。
