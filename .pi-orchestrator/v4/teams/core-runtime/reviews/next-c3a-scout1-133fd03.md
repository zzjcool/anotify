# C3a 只读 implementation gate（固定 133fd03）

## 事实清单

- 固定提交 `133fd03c575d14671dbd0366bef3d1d30c9945a6`，12 文件：`execution_auth.go` 新增 902 行；`task.go` 删除 532 行（公开 `GrantLease/RenewLease/CompleteTask/CompleteTaskAt/AckCancel` 全部退役）；`node.go` +129（reservation 落槽账本、`bindSlotOn` CAS）；`v4test/execution_auth_test.go` 新增 809 行、12 个 `Test*`。
- 旧公开入口已无残留导出：`internal/store` 非测试代码只剩注释引用（`execution_auth.go:17,24`；`task_attempt.go:467`、`task.go:1101` 仍写旧名，属陈旧注释）。
- `GrantLeaseAuthorized` 的事务顺序：`BeginTx` → `authorizeNodeOn`（`nodes.token_hash`+`declared_namespaces`）→ 任务行（phase/cancel/assignment/proposed/expiry）→ `namespaceAllowedOn`（`node_configs.namespaces ∩ declared`）→ lifecycle/maxAttempts → `loadMachineSnapshotOn`（node_configs 的 vars/limits/local_vars/local_limits）→ `TypedMachineLimits`+`maxPerNodeOn` → `grantBudgetOn` → slot 上限 → `bindSlotOn` CAS → `insertTaskAttemptOn` → 任务行更新 → commit（`execution_auth.go:258-535`）。Renew/Complete/Ack 同样把身份、ns、当前执行事实放在同一事务（`execution_auth.go:552,648,769`）。
- D2 首次 grant 钉版：任务创建时 `createTasksTx` 同事务读取并写入 `namespace_vars`/`namespace_vars_revision`（`task.go:306-340`），grant 只读取任务行并写 attempt 快照（`execution_auth.go:512-530`）。
- 四固定 checks 我独立实测（timeout 60，官方 Go env，真实 exit）：`gofmt -l internal/store internal/artifact` 空、exit 0；`go build ./internal/store/ ./internal/artifact/` exit 0；`go vet ./internal/store/v4test/ ./internal/artifact/` exit 0；`go test ./internal/store/v4test/ ./internal/artifact/ ./internal/protocol/ -count=1` exit 0（v4test 23.994s / artifact 2.425s / protocol 0.046s）。
- v4test 当前 230 个顶级测试通过只代表该外部测试包；C3b typed manifest/uploads/FK/GC、原白盒、G2-G4 未做，不据此宣称全系统绿。

## 红绿证据（只读 logs）

- 12 个新测试都有对应失败记录；`/tmp/anotify-core-next-c3a-red-reservation.log`、`red-unreaped.log`、`red-wildcard.log` 均为 `ok ... exit=0`，即原 red 无判别力；对应 `red2-*.log` 分别以真实功能失败（预算未计/通配未生效）取代，符合“red2 取代无判别 red”。
- `red-ns-revoke.log` 是编译失败（`declared and not used`），`red2-ns-revoke.log` 才是续租/结果未查 ns 的功能失败。
- 判别力较好的 red：token rotation（`red-token-rotation.log` 旧 token grant 得到 nil）、proposed（旧 proposed 通过）、expiry==now renew（通过）、ack binding（错 lease 得到 nil）、ns pin（快照 revision=2 vs pinned 1）、limits snapshot（red2 中 attempt limits Total=0）。
- 未发现 fixture 迁移弱化原断言：C3a 前 `GrantLease` 根本无 maxPerNode 预算（`git show 133fd03~1:internal/store/task.go` 无 `EffectiveMaxPerNode`），`lifeCycleCap` 设 `maxPerNode=2` 只是保留“同任务两个 attempt 可共存”的旧断言；显式 `maxPerNode=1` 的拒绝语义由新测试覆盖（`g14_helpers_test.go:162-163`）。默认省略 maxPerNode=1 只在 artifact 层测到，store grant 未用“未设置”形态判别，属轻微缺口。

## Findings

### 1) [major] 并发窄预算测试被 slot 上限混淆，实际不验证预算事务原子性

`TestConcurrentNarrowBudgetGrantsExactlyOne` 把节点总预算设为 `total=1`（`internal/store/v4test/execution_auth_test.go:473`），却让 taskB 使用 `slot=1`（同文件 `:500`）。`GrantLeaseAuthorized` 在预算之后还有 `if slot >= effectiveTotal` 的拒绝（`internal/store/execution_auth.go:460-470`，返回 `ErrSlotUnavailable`）。因此：

- 即使 `grantBudgetOn` 被整体删除或预算检查在事务外做，taskB 仍必然被 slot 上限拒绝，测试仍得到 `success=1/rejected=1`；
- 该测试当前唯一能失败的情形是“预算与 slot 上限同时失效”；
- `red-concurrent.log` 显示中间版本曾 `success=2`，说明当时的 red 是缺少 slot 上限，而不是缺少预算原子性；当前 green 不能证明“同预算并发 grant 恰好一个成功”。

影响：契约明确要求的 `concurrent grants same budget txn exactly one` 实际无判别测试；预算检查移出事务、或 total 统计漏项，测试都不会红。

### 2) [major] “自己的 reservation 只算一次”按 task 整体排除，不是契约要求的 same-slot exact

`countReserved` 的排除条件是 `i.busy_task <> ?`（只按 taskID），没有匹配 assignment/proposedAttempt/slot（`internal/store/execution_auth.go:178-183`）。而 `recordReservationOn` 允许同一 task 在多条 slot 上各写一条 `busy_lease_id=''` 的 reservation（`internal/store/node.go:839-847`，无“同一 task 只能一条 reservation”的约束）。测试只有一条自己的 reservation（`execution_auth_test.go:433-439`），无法区分 exact-slot 与 task-wide 排除。

未测故障：节点对同一 task 报两个 Busy=true reservation（slot A/B）后，grant 绑 slot A 时两条都被排除；预算少算 1，可能越过 perCapability/maxPerNode 授权，slot B 的 reservation 继续挂着；若 grant 的 slot 与 reservation 不在同一 slot，也会留下未被消费却被排除的 reservation。契约原文是“exclude its pregrant reservation once when grant binds same slot”，实现是 task-wide 排除。

### 3) [major] 历史 ACK 正向/幂等断言只查 busy 槽，不查 attempt 的 reaped 事实；重复 ACK 实际会重写 attempt 行

- 正向旧 ACK 后只断言 `busySlots` 从 2→1、新 attempt 的 phase/lease/token 未变（`internal/store/v4test/execution_auth_test.go:665-675`），从未 `GetTaskAttempt(attempt1).ReapedAt`。若实现释放旧槽但漏写 `reaped_at`，预算的 `countUnreaped`（`execution_auth.go:168`）会永远把旧 attempt 计为占用，容量泄漏，而该测试仍绿。
- 重复 ACK（`execution_auth_test.go:676-681`，注释“不再写事实”）实际走 `reapAttemptForTaskTx`（`execution_auth.go:894` → `task.go:1152` → `task_attempt.go:351-378`）：`finishAttemptOn` 无条件 `SET phase=?, error=?`，没有 `reaped_at IS NULL` 守卫（对比 `task_attempt.go:418` 的 `reapAttemptOn` 有守卫）。重复历史 ACK 可把旧 attempt 的 `error` 清空（`schema.sql:449` 默认 ''），与“重复 ACK 幂等”不符；测试只查槽数，查不出。

### 4) [minor] 未回收执行 + 真实 reservation 的拒绝同时可由 perCapability=2 或 maxPerNode=2 满足

`TestGrantCountsUnreapedExecutionsAndRealReservations` 使用 `lifeCycleCap`（其 `maxPerNode=2`，`g14_helpers_test.go:162-163`）同时又设 `perCapability:idem=2`（`execution_auth_test.go:375-384`），stage1 的 `3>2` 由任一维度都能拒绝。若只有其中一个维度会统计 reservation/未回收执行，测试仍绿；该用例没有隔离“reservation 进入 perCapability”与“reservation 进入 maxPerNode”。影响限于该维度组合的分支未被单独钉住；两个维度各自的独立用例存在（`TestGrantPerCapabilityAcrossNamespaces`、`TestGrantPinnedMaxPerNodeAcrossVersions`）。

### 5) [minor] `total` 的 local+node 合并未通过 grant 预算路径验证

`grantBudgetOn` 使用 `loadMachineSnapshotOn` 的 `snap.limits`（`execution_auth.go:438`），但所有新预算用例只通过 `putNodeCfg` 写 node 层 limits，没有设置 `LocalLimits` 后验证 total 被 local 覆盖。若 `loadMachineSnapshotOn` 退化为只用 node limits，现有新测试全绿。合并逻辑本身在 config 往返/共享 hash 测试中覆盖，但未覆盖 grant 的实际拒绝行为。

## 已确认未弱化/有判别力的部分

- 首次 grant 的 ns pin：`TestFirstGrantAfterNamespaceChangeKeepsPinnedRevision` 的 red 明确显示 `attempt 快照 revision=2，want pinned 1`（`/tmp/anotify-core-next-c3a-red-ns-pin.log`），并同时断言 `TaskInputs` 返回 `ns-A`、任务行 revision 不漂。
- 机器 limits 快照：red2 显示 attempt1 `Total:0`，green 后同一 attempt 改配置仍为 2、新 attempt 为 3（`execution_auth_test.go:750-808`），D2 的“旧快照不可变/新 attempt 新 limits”有判别力。
- token rotation：旧 token 对 grant/renew/result/Reclaim 全部拒绝，当前 token 成功（`execution_auth_test.go:55-125`），且 red 功能失败。
- ns 撤销：renew/result 在 authorized 与 declared 两种撤销下都拒绝，且不改 lease/槽（`execution_auth_test.go:127-170`）；grant 时 ns 撤销由迁移后的 `TestGrantRechecksNodeAuthorization` 覆盖（`task_test.go:1274+`）。
- 历史 ACK 绑定：错 lease/slot/instance 全部 `ErrLeaseMismatch` 且不释放槽，旧 ACK 只释放旧槽、新 attempt 的 phase/lease/token 不变（`execution_auth_test.go:610-711`）。
- maxPerNode 判别未被 slot 混淆：`TestGrantPinnedMaxPerNodeAcrossVersions` 先同 ns 同名跨版本拒绝，再用不同能力名成功，排除“总量满/全局 maxPerNode”解释（`execution_auth_test.go:326-373`）。
- 通配 perCapability：alpha 用满后 beta（不同名）仍能各用 1，第二个 beta 才拒绝，能区分“全局合计”与“同能力名”口径（`execution_auth_test.go:279-324`）。

## 待验证（不造断言）

- 红绿 logs 不记录 pwd/命令/HEAD/exit 行，只能按失败信息判断；我无法从 log 证明每次 red 的测试文件内容与最终 133fd03 完全一致（最终四个 checks 我已独立实测）。
- `ReclaimStaleSlots` 的调用方是否已由认证 NodeSession 绑定 nodeID，超出本 commit 12 文件范围，未验证（属 G2/上层）。
- 节点上报同一 task 多条 reservation 是否为真实可达状态（需 G2/T3 wire 行为确认）；本 finding 按 store 允许写入的路径判定。

```json
{"ok": false, "findings": [{"path": "internal/store/v4test/execution_auth_test.go", "line": 500, "severity": "major", "evidence": "total=1 (line 473) 但 taskB 用 slot=1；execution_auth.go:460-470 的 slot>=effectiveTotal 必然以 ErrSlotUnavailable 拒绝，故 success=1/rejected=1 在 grantBudgetOn 被删除或非原子时仍成立；red-concurrent.log 的 success=2 来自当时缺少 slot 上限，不能证明并发预算事务。"}, {"path": "internal/store/execution_auth.go", "line": 181, "severity": "major", "evidence": "countReserved 仅用 i.busy_task <> taskID 排除自己的 reservation，未匹配 assignment/proposed/slot；node.go:839-847 允许同一 task 在多 slot 各写一条 reservation；测试 execution_auth_test.go:433-439 只有一条 reservation，无法区分 exact same-slot 与 task-wide 排除，可能少算 1 并越过 perCapability/maxPerNode。"}, {"path": "internal/store/v4test/execution_auth_test.go", "line": 669, "severity": "major", "evidence": "正向旧 ACK 只断言 busySlots 2->1，未断言 attempt1.ReapedAt；若释放槽却漏写 reaped_at，execution_auth.go:168 的 countUnreaped 会永久计占用（预算泄漏）而测试仍绿。重复 ACK 走 execution_auth.go:894 -> task.go:1152 -> task_attempt.go:351-378，finishAttemptOn 无 reaped_at IS NULL 守卫，会重写 phase/error，可清空旧 attempt.error；测试注释“不再写事实”未被断言。"}, {"path": "internal/store/v4test/execution_auth_test.go", "line": 375, "severity": "minor", "evidence": "该用例用 lifeCycleCap 的 maxPerNode=2（g14_helpers_test.go:162-163）同时 perCapability:idem=2；stage1 的 3>2 由任一维度都能拒绝，未隔离 reservation/未回收执行的计数究竟进入 perCapability 还是 maxPerNode。"}, {"path": "internal/store/execution_auth.go", "line": 157, "severity": "minor", "evidence": "grantBudgetOn 使用 loadMachineSnapshotOn 合并后的 limits，但所有新预算用例只用 putNodeCfg 写 node 层 limits，没有 LocalLimits 覆盖后验证 total 拒绝；merge 退化时新测试仍绿（合并逻辑仅在 config 往返/hash 测试覆盖）。"}], "reason": "C3a 四个固定 checks 实测全绿且 12 个新测试多数有功能红绿（red2 已取代无判别 red），但 coverage implementation gate 不通过：并发窄预算测试被 slot>=total 混淆而无法验证预算事务原子性（major），own reservation 按 task-wide 而非 same-slot exact 排除且无测试能区分（major），历史 ACK 正向/幂等仅查槽不查 reaped attempt 且重复 ACK 会重写 attempt 行（major）；另有 perCap/maxPerNode 混同与 local+node total 未经 grant 验证的 minor 缺口。"}
```
