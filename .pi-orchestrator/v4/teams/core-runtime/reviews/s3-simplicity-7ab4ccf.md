# G1-6S 固定提交接口/简洁性 interim 复审

**固定提交**：`7ab4ccf35dc335caedd33d2182c8c5e4f6dddc19`（`chore(v4): 安全恢复核心生产检查点与完整验收契约`）
**范围**：`internal/store/{task_attempt.go,task.go,node.go,config.go}`；只读 `.pi-orchestrator/v4/teams/core-runtime/EXPORTED-INTERFACES.md`、`.pi-orchestrator/v4/SLOT-HANDOFF.md`。
**方法**：全部用 `git show 7ab4ccf:<path>` + `nl -ba` 取证；未运行 build/test、未读其它 worktree、未派生子 agent、未写文件。
**取证一致性**：`git diff --stat 21253ed..7ab4ccf -- <四文件>` 为空，EXPORTED-INTERFACES 的 “As of 21253ed”（第 3 行）与本次固定提交的实际 store surface 一致。

## 1. 事实清单

- F1 `GrantLease` 是唯一授权入口，一个 `BEGIN IMMEDIATE` 事务内完成 assignment/phase/cancel/node、applied 配置、slot 预算、slot CAS 绑定、attempt 快照与 tasks 授权字段（`internal/store/task.go:742,777-802,874-890,893-931,934-960,963-990`）。
- F2 slot 绑定唯一 SQL 点是 `bindSlotOn`，CAS 条件 `busy_task IS NULL`，写入 `busy_task/busy_attempt/busy_lease_id`（`internal/store/node.go:901-930`）；`NodeInstance` 暴露这三列（`node.go:111-121`）。
- F3 每次 attempt 的机器快照/槽绑定/授权摘要落在 `task_attempts`，读取 API 为 `GetTaskAttempt/ListTaskAttempts/PreviousTaskAttempt`（`internal/store/task_attempt.go:168-230`）；grant 时同事务插入（`task.go:963-985`）。
- F4 历史回收入口 `ReapTaskAttempt` 校验 node token hash + task/attempt/node/lease/slot/instance，只写 attempt 的 `reaped_at` 并清对应槽，明确“不碰任务 phase/当前 attempt/token”（`task_attempt.go:469-540`，语义注释 `458-461`）。
- F5 当前 attempt 取消回收入口 `AckCancel(taskID, attempt, reaped, now)` 签名不含 node/lease/slot；它负责置任务终态并按 attempt 匹配释槽（`internal/store/task.go:1320-1430`，注释 `1303-1319`）。
- F6 正常结果入口 `CompleteTaskAt` 同事务校验 attempt/lease/phase/未取消/未过期，然后 `finishAttemptOn(reaped=true)` + `releaseSlotBindingOn`（`task.go:1111-1190`）；巡检 `RecoverLostLeases/recoverOne` 任何分支都不释放物理槽（`task.go:1438-1585`）。
- F7 离线/实例事实：`MarkNodeOffline` 只置 offline+alive=0 保留 busy（`node.go:751-786`）；`ReclaimStaleSlots` 只接受 `Busy != nil && !*Busy` 且 refs 相符、ObservedAt 不早于绑定的记录（`node.go:799-879`）。
- F8 裸槽位原语仍导出：`LeaseSlot` 只做 `bindSlotOn`，无 phase/assignment/applied/预算校验（`node.go:881-901`）；`ReleaseSlot` 只做 `releaseSlotBindingOn`，无 reaped 事实（`node.go:930-938`）。
- F9 输入解析：`TaskInputs` 只读当前 attempt 快照 + tasks 首次 pin 的 namespace 快照，无快照 `ErrNoAttemptSnapshot`（`task.go:373-437`）；`EffectiveVars` 仍现读 local/node/ns 当前配置（`internal/store/config.go:919-953`）。
- F10 共享 hash/limits：`TypedMachineLimits` 是唯一 map→`protocol.Limits` 适配点，`MachineConfigHash` 只转调 protocol（`config.go:1106-1149`）；grant applied 校验用 revision+hash（`task_attempt.go:249-251`）。
- F11 attempt JSON 隐藏 `InstanceID/AttemptTokenHash/MachineVars/MachineLimits`（`task_attempt.go:63-110`）；Task 隐藏 `NamespaceVars/LeaseInstanceID/AttemptTokenHash`（`task.go:148-171`）。
- F12 冻结契约要求 cancel.ack 按 node+task+attempt+lease+slot 核验、错误 lease/slot 必须拒（`SLOT-HANDOFF.md:19,49`）；grant 不再次 `LeaseSlot` 挑槽（`:41`）；缺/空 instances 不表示无进程（`:32`）；缩预算保留存量占用（`:8`）。

## 2. 关键数据流

1. **派发**：`ListDispatchableTasks` → `AssignTaskAt` 写 `assignment_id/node_id/proposed_attempt/assignment_expires_at(now+30s)`，不授执行权（`task.go:590-696`）。
2. **授权**：`GrantLease` 同事务复核 assignment 未过期、node online、applied revision+hash、slot 在 `limits.total` 内、busy 数未满 → `bindSlotOn` → 插 `task_attempts` → 更新 tasks（running/lease/token hash/lease_instance_id/config_revision）并返回明文 token（`task.go:742-990`）。
3. **取输入**：`TaskInputs(taskID,names)` → task 行 `NamespaceVars` + attempt 行 `MachineVars/MachineVarSources`；不回退当前配置（`task.go:392-437`）。
4. **正常结束**：`CompleteTaskAt` → 校验 attempt/lease/未取消/未过期 → `finishTxGuarded` 终态 → `finishAttemptOn(reaped=true)` + 精确释槽（`task.go:1111-1190`）。
5. **取消**：`RequestCancel` 记意图（pending 直接终态，running 等 ack）；`AckCancel(reaped=true)` 置终态并按 attempt 释槽；历史绑定走 `ReapTaskAttempt`（不动任务行）（`task.go:1222-1430`，`task_attempt.go:469-540`）。
6. **离线/实例事实**：`MarkNodeOffline` 保 busy；`ReclaimStaleSlots` 只在明确 idle+refs 相符时 `reapAttemptOn`+`releaseSlotBindingOn`（`node.go:757-879`）。
7. **容量读**：`FreeSlotsByNode`（alive 且 busy_task IS NULL）、`ListInstances`、`InstanceStatsByNode`（`node.go:677-1039`）。

## 3. 发现

| # | 严重度 | 位置 | 一句话证明 |
|---|---|---|---|
| M1 | major | `internal/store/task.go:1320` / `internal/store/task_attempt.go:469` | `cancel.ack` 被拆成两个半权威入口：`AckCancel` 能置终态/释当前槽但只校验 taskId+attempt，签名本身无法接收/拒绝 node 身份、leaseId、slot；`ReapTaskAttempt` 有四元组校验（`task_attempt.go:523`）却明确“不碰任务 phase”（`460-461`）。A3 要求按 node+task+attempt+lease+slot 核验且错误 lease/slot 必须拒（SLOT-HANDOFF:19,49）。G2 按帧名直映 `AckCancel` 则 A3 验收项无法通过；只调 `ReapTaskAttempt` 则任务永不进 cancelled；安全组合是未文档化且顺序敏感的两调用。 |
| M2 | major | `internal/store/node.go:889` / `internal/store/node.go:935` | `LeaseSlot` 无 phase/assignment/applied/预算/attempt-token 校验即可把任意 (task,attempt,lease) 绑到任意 alive slot；`ReleaseSlot` 无任何 reaped 事实即可清占用。`EXPORTED-INTERFACES.md:34-35` 却把两者放进“G2必须使用的S新接口”代码块，与 SLOT-HANDOFF:41（grant 不再 LeaseSlot 挑槽）及 `task.go:1431-1435`（只有可信事实能回收）冲突；G2 直接使用即可伪造/释放容量。 |
| M3 | major | `internal/store/node.go:639-655` | `UpsertInstances` 把 `len(specs)==0`（nil 或空数组）当作清空该节点全部槽位，并把未申报 slot（含 `busy_task` 非空的在跑占用）直接 `DELETE`；A3 规定缺/空 instances 是未知、不能当进程已退出（SLOT-HANDOFF:32），缩预算要保留超范围存量占用直至回收（:8）。丢失 busy 行使 `GrantLease` 的 `busyCount`（`task.go:905-910`）低估占用，节点重新申报同 slot 后可把仍在跑的物理槽再授出。v4test 只覆盖“先 ReleaseSlot 再缩容”（`cluster_node_test.go:458-478`），未见带 busy 缩容/空实例用例。 |
| M4 | major | `internal/store/config.go:923` | `EffectiveVars` 现读 local/node/ns 当前配置并返回与 `TaskInputs` 同型的 `EffectiveVar`，注释未标“仅诊断”；D2 规定的执行输入入口是快照型 `TaskInputs`（`task.go:373-392`，EXPORTED:47 只约束后者）。G2 在 attempt 运行中误用 `EffectiveVars` 会原样重开 G1-6S 第一条 major 的配置漂移（同 attempt 变值）。 |
| M5 | major | `internal/store/task.go:1009-1013` | `RenewLease` 的 UPDATE 不含 `lease_until > now`，过期但尚未被巡检的租约可被续租复活；而 `CompleteTaskAt` 明确拒过期 result（`task.go:1161-1163`）、`VerifyAttemptToken` 拒过期 token（`1623`）、`RecoverLostLeases` 把过期当失联（`1444-1447`）。“过期=授权失效”因此非单调，需裁决是 liveness grace 还是授权漏洞。 |
| m1 | minor | `internal/store/task.go:1587,1594-1596,1623` | `VerifyAttemptToken` 注释首句称其为 “emit/task.result 的权威校验”，四行后自认“只是快速失败与诊断”；且内部用 wall clock `Now()` 而非显式时钟。命名与首句会诱导 G2 把它当授权点，TOCTOU 只能靠 `CompleteTaskAt/EmitAuthorized` 兜底。 |
| m2 | minor | `internal/store/task_attempt.go:476` | `ReapedInputs.Slot` 是 `int`，“必须带 slot”只能写成 `in.Slot < 0`；slot=0 合法，字段省略/零值同为 0，无法区分 presence（A3 要求 `*int`，SLOT-HANDOFF:15）。slot-0 attempt 的缺失绑定可通过。 |
| m3 | minor | `internal/store/task_attempt.go:390-391` | A3 四元组的 slot 只在外层 `ReapTaskAttempt` 比较（`task_attempt.go:523`）；核心 `reapAttemptOn` 签名没有 slot，另一调用点 `ReclaimStaleSlots` 也不传 slot（`node.go:866`）。所谓“唯一核心”不能自身保证 node+task+attempt+lease+slot 绑定，安全性依赖调用点。 |
| m4 | minor | `internal/store/node.go:957` | `releaseSlotBindingOn` 的 `busy_attempt = ? OR busy_attempt = 0` 允许任意 attempt 清掉“无 attempt 身份”的旧行；注释自认 v4 grant 永不产生这种行（`node.go:942-944`），本轮又明确不做旧库兼容，属不可达且削弱“精确 attempt”承诺的机器。另 `finishAttemptOn(reaped=true)` 已释槽，`CompleteTaskAt/reapAttemptForTaskTx/ReclaimStaleSlots` 又各再释一次（`task.go:1180-1184,1649-1653`；`node.go:866-870`），三套 finish+release 变体（幂等、当前无错）增加不变量分叉面。 |
| m5 | minor | `internal/store/node.go:165-167` | `SlotObservation.AssignmentID/ProposedAttempt` 在全 store 无读取；`ReclaimStaleSlots` 对 `Busy=true` 直接 continue（`node.go:820-828`），store 也没有 reservation 账本。字段暗示 store 能记录 grant 前预留，实为死接口，并让 `task_attempt.go:435`“不镜像 wire”的说法名不副实。 |
| m6 | minor | `internal/store/node.go:1307` | `EligibleNodes` 的 applied 判定只比 revision（`nodeConfigSnapshot` 无 applied_hash，`node.go:1041-1047`），`GrantLease` 却要求 revision+内容 hash（`task_attempt.go:249-251`，`task.go:874-890`）。同一问题两种口径：节点可先被判 available，再在 grant 被 `ErrConfigNotApplied` 打回，且 reason 封闭集无法表达 hash 不一致。 |
| m7 | minor | `internal/store/node.go:738,751` | `MarkInstancesDead` 注释写“连接断开时调用”，`MarkNodeOffline` 又称自己是会话死亡“唯一入口”；只调前者留下 `status=online + alive=0`（可被 EligibleNodes 选中但无槽可授），只调 `UpdateNodeStatus(offline)` 又不改 alive。一个生命周期三个入口、两种终态。 |
| m8 | minor | `internal/store/task_attempt.go:200-213` | `PreviousTaskAttempt` 以 `tasks.attempt` 为 current；`recoverOne` 重排后 `tasks.attempt` 仍是 N 而任务回 pending（`task.go:1538-1558`），此时最近一次执行是 N，函数返回 N-1，G2 的 previous 日志在重试等待窗会指错代。 |
| m9 | minor | `internal/store/task.go:1027,1761` | store 导出 `TaskRejectSlotUnavailable="slot_unavailable"` 与 `AssignmentWindowSec=30`，`EXPORTED-INTERFACES.md:20-21` 将其列为 G2 必用；SLOT-HANDOFF:47 规定 T1 不私改/镜像 T3 的 protocol。T3 delta 未到前这是封闭 reject 集与 30s 窗口的第二来源，落地后需退场，否则双源漂移。 |
| m10 | minor | `internal/store/task.go:1062` | `RejectAssignment` 的 assignment 守卫是 `assignmentID != "" && rowAssign != "" && rowAssign != assignmentID`：`assignmentID` 为空或中枢候选已被清空时仍接受 reject 并改写 waiting/backoff；`GrantLease` 对同一 assignment 是严格 fail-closed（`task.go:852-865`）。陈旧 reject 可无证据推迟派发。 |
| m11 | minor | `internal/store/node.go:41,60,287-288` | `Node.MaxConcurrency`/`DefaultMaxConcurrency` 仍导出、入库、JSON 输出并默认 8，但全 store 无任何读取/限额执行；A3 已把容量改为 `limits.total` + 真实 slot（SLOT-HANDOFF:35，`node.go:143-147`）。对外 DTO 仍暴露一个不存在的预算维度。 |
| m12 | minor | `internal/store/task.go:1744,1752` | `SlotUnavailableBackoff` 与 `StaleConfigBackoff` 实现完全相同（同一 base/max/factor），两个导出名一个值；未来只改一个会静默分叉，属多余导出面。 |

## 4. 已有防护（避免误报）

- 所有状态转换都在 `BEGIN IMMEDIATE` 事务内，且用 CAS/条件 UPDATE（phase/attempt/lease、`busy_task IS NULL`、`reaped_at IS NULL`）：`task.go:777-990,1111-1190,1320-1430`；`node.go:901-930`。未发现“先读后写”的授权竞态。
- 历史回收 `ReapTaskAttempt` 是 fail-closed（节点 hash、attempt 行、node/lease/slot/instance 逐项核对）；重复回收幂等（`task_attempt.go:469-540`）。
- `ReclaimStaleSlots` 已删除“phase != running 就清槽”的假 reaped：只认显式 `busy=false` + 相符历史 refs + ObservedAt 保护（`node.go:799-879`）；`MarkNodeOffline`/`RecoverLostLeases` 都保留 busy 账本（`node.go:751-786`，`task.go:1438-1585`）。
- D2 修复在 store 域成立：`TaskInputs` 无回退读当前配置；grant 快照同事务；`TaskAttempt` JSON 不回显机器值/token hash；hash 只经共享 `protocol.MachineConfigHash`（`config.go:1106-1149`）。
- 未把以下视为 finding（按指令）：v4 不保留旧 API/迁移兼容；`Task.Artifacts/TaskResult` 仍是旧字符串表面（EXPORTED 已列为未完成）；G2/T3 传输字段映射未落地（`internal/ws` 仍是 v3 namespace-scoped 签名，属执行域接线的预期 WIP，未计分）。
- 未运行 build/test；测试名与 161 PASS 来自已提交的 G1-6S DONE 文档，本次未复核。

## 5. 待验证

1. G2 最终把 `cancel.ack` 映射到 `AckCancel` 还是 `ReapTaskAttempt` + `AckCancel` 组合；若前者，A3 的“错误 lease/slot 拒”验收项必失败。
2. G2 hello/config.applied 是否复用 `UpsertInstances`；T3 是否会给 `instances` 提供 presence 类型、store 是否会为“空/缺实例”加保护。
3. `EffectiveVars` 是否会被 G2 用于执行输入（当前 v4test 只做配置诊断断言）。
4. 父裁决：`RenewLease` 允许过期后 grace 续租，还是必须加 `lease_until > now` 守卫。
5. T3 delta 落地后 `TaskRejectSlotUnavailable/AssignmentWindowSec` 的退场/别名化计划。
6. `AckCancel/LeaseSlot/ReleaseSlot` 在 G2 完成后是否降为 internal/test-only 或补“禁止业务调用”的 handoff 标注。
7. `UpsertInstances` 带 busy 缩容/空实例 clears 是否有意；若否，需补测试与保护（当前无覆盖）。

```json
{"ok": true, "reason": "G1-6S fixed-commit 7ab4ccf interim interface/simplicity review complete: 4 store files + committed EXPORTED-INTERFACES/SLOT-HANDOFF read-only via git show, 17 findings (5 major: cancel.ack dual half-authoritative entries task.go:1320/task_attempt.go:469, raw LeaseSlot/ReleaseSlot published as G2 must-use node.go:889/935, UpsertInstances destructive empty/shrink clear node.go:639-655, EffectiveVars live-read twin of TaskInputs config.go:923, RenewLease revives expired lease task.go:1009; 12 minor), no build/test run, interim evidence only"}
```
