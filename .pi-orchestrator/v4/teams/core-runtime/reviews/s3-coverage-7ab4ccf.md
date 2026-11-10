## 结论：S 的三个 major 未被独立清零（ok:false）

审查对象：commit `7ab4ccf`（工作树 clean；`git diff 21253ed 7ab4ccf -- internal/store` 为空）；只读，未 build/test，不声称全 Go 可编译。

### Findings

| # | severity | path:line | evidence |
|---|---|---|---|
| F1 | **critical** | `internal/store/node.go:639-655` | `UpsertInstances` 对空列表执行 `DELETE ... WHERE node_id=?`，缩容执行 `DELETE ... slot NOT IN (...)`，**都不排除 `busy_task IS NOT NULL`**。同一文件 `node.go:757-790` 的 `MarkNodeOffline` 明确保留 busy 账本，证明 busy 是物理占用事实。A3 冻结要求 `.pi-orchestrator/v4/SLOT-HANDOFF.md:32,37`：缺 instances/空数组是“未知”，rehello 不得假物理空。测试缺口：`cluster_node_test.go:469-479` 只在 `ReleaseSlot` 之后缩容；`task_attempt_test.go:515-525` rehello 申报的是同一条 slot。无 shrink-while-busy / 空列表反例 → 旧进程未回收时 busy 行可被 rehello 静默删除，容量少算、可超预算新 grant。 |
| F2 | **major** | `internal/store/task.go:848-854` | stale assignment 分支的 `UPDATE ... WHERE assignment_id = rowAssign` 清的是**当前** assignment，而不是请求里那个 stale id。反例：`AssignTask(asg-new)` → `GrantLease(asg-old)` 会清掉 asg-new；随后合法 `GrantLease(asg-new)` 得到 `ErrNoAssignment`。`task_test.go:69-88` 只断言 pending+busy=0，**未断言 `AssignmentID/NodeID` 仍保留**，所以测试通过但安全性质已破。同类：`task.go:1062-1076` 的 `RejectAssignment` 把空 assignmentID 当通配；`task_attempt_test.go:371` 只用空 id + 非法 reason，未测“合法 reason + 空 id”会清掉新 reservation。 |
| F3 | **major** | `internal/store/v4test/task_attempt_test.go:64-135` | S major#1（grant 同事务机器 snapshot）没有可判别反例：配置/hello 更新发生在 grant **之后**（`:117-120`），没有任何测试在 grant 进行中并发改 `node_configs`。`sync.WaitGroup` 仅用于 cancel/grant（`task_test.go:933`）。生产代码看是正确的（`task.go:759` BeginTx，`:869` 事务内 load），但把 `loadMachineSnapshotOn` 移出事务的回归仍会通过该测试。 |
| F4 | **major** | `internal/store/v4test/task_attempt_test.go:778` | 禁止项用 `"machineVars"/"machineLimits"/"attemptTokenHash"` 小写串；若实现删除 `json:"-"`（`task_attempt.go:66,71,76,80`），Go 默认键是 `"InstanceID"/"AttemptTokenHash"/"MachineVars"/"MachineLimits"`，大小写不匹配，三个泄漏都测不到。secret 值检查只能兜底 MachineVars；token hash、limits、InstanceID 无 backstop。 |
| F5 | **major** | `internal/store/v4test/task_attempt_test.go:409,465-471`；`task_test.go:798,837-839` | 历史 reap 后只核对 `Attempt/LeaseID`，但两处都把 `token2` 丢了；失败消息却写“相位/token”。回归若在 `ReapTaskAttempt` 中清 `tasks.attempt_token_hash`，两个测试都过，而新 attempt 的 emit/VerifyAttemptToken 会坏。生产 doc（`task_attempt.go:458-467`）声称不碰当前 token，测试未证。 |
| F6 | **major** | `internal/store/v4test/task_attempt_test.go:480-487` | `CompleteTask(attempt1)` 必因 attempt2 是当前 attempt 而拒（`task.go:1153`）；`VerifyAttemptToken(token1)` 必因 attempt2 grant 已覆盖 hash 而拒。**即使删除 ReapTaskAttempt 这两个断言仍成立**，不证明“历史回收不让旧 result/emit 复活”。且本文件从未调用 `EmitAuthorized`，A3 的“旧 emit 仍无效”在 reap 场景零覆盖。 |
| F7 | **major** | `internal/store/v4test/task_attempt_test.go:797-858`；`internal/store/retention.go:151-153` | GC 保留测试的 A/B 都是单 attempt 任务；生产是 `NOT EXISTS (任一 reaped_at IS NULL)`。回归成“只看最新/当前 attempt”该测试仍通过。缺“attempt1 未 reap + attempt2 已 reap 的终态任务必须保留”的用例。 |
| F8 | minor | `task_attempt_test.go:642-643` | 预算缩小后第一次 `ErrSlotUnavailable` 用的是已被 attempt1 占用的 slot 0，无法区分“预算满”与“slot 被占”；预算强制真正被隔离验证在 `:662-664`。 |
| F9 | minor | `task_attempt_test.go:200-278` | accepted-slot 测试只验证 argv slot 被消费；未测 `task.go:863-866` 的 assignment→node 归属不符，也未测 `alive=0` slot 经 GrantLease 被拒。 |
| F10 | minor | `task_attempt_test.go:421-452`；`task_attempt.go:527-530` | 历史 reap 未测错 taskId/不存在的 attempt/错 InstanceID；`InstanceID` 永远是 base 值，该校验从未被反例打中。 |
| F11 | minor | `task_test.go:1141`；`task.go:1161` | result 过期只测 `expiresAt+1`，未测精确边界 `now == expiresAt`（实现是 `<=`）。 |
| F12 | minor | `cluster_node_test.go:507-560` | 正路径 reclaim 用裸 task + `LeaseSlot`，没有 `task_attempts` 行，走的是 `reapAttemptOn` 的 ErrNotFound 兜底；未断言 attempt 被写 `ReapedAt`。 |
| F13 | minor | `task_test.go:1023-1048` | `TestTaskGrantBudgetExhaustedReleasesSlot` 注释称“占槽在事务外先发生”，但生产是先预算检查（`task.go:910`）后 bind（`:925+`）；断言仍可挡“先 bind 后检查”的重排，但描述的 F3 形状与代码不符。 |

### S major 覆盖清单

1. **grant 同事务机器 snapshot**：*部分*。post-grant 不漂移、跨节点重解析、ns pin 均有判别力（`TestAttemptSnapshotPinnedPerAttempt`、`TestTaskInputsMachineLayerReparsedPerNode`、`TestTaskNamespaceInputsPinnedAcrossConfigUpdate`）；**同事务原子性无并发反例**（F3）。
2. **历史 attempt 具体槽/lease reap**：*部分*。node/lease/slot/missing 绑定、幂等、只清旧槽已验证（`TestHistoricalReapRequiresExactBinding:421-478`、`TestTaskOldAckCannotReleaseNewAttemptSlot`、`TestTaskReapOfRequeuedAttemptReleasesOnlyItsSlot`）；**新 attempt token/emit 存活、旧 emit 失效未证**（F5/F6），instance/task/attempt 反例缺失（F10）。
3. **不按 phase/rehello/offline 推断**：*基本成立但未闭环*。`TestRehelloIsNotProcessFact`、`TestTerminalPhaseIsNotProcessFact`、`TestTaskOnlineLeaseExpiryKeepsSlotUntilNodeFact`、`TestTaskRecoverIdempotentRequeues`、`cluster_node_test.go:498-560` 均能挡住 phase/offline 假回收；但 **`UpsertInstances` shrink/empty 是未覆盖的 rehello 假释放通道**（F1，critical），故 major#3 不能算独立清零。

### 回归用例映射（具名）

- `TestAttemptSnapshotPinnedPerAttempt`：部分（F3）
- `TestGrantConsumesAcceptedSlot`：通过，minor（F9）
- `TestAssignmentWindowBoundedAndIdempotent`：通过（30s 边界、幂等、过期不耗 attempt 均有判别力）
- `TestRejectSlotUnavailableNoAttempt`：通过，minor（F2 的 empty-assignment 面）
- `TestHistoricalReapRequiresExactBinding`：部分（F5/F6/F10）
- `TestRehelloIsNotProcessFact` / `TestTerminalPhaseIsNotProcessFact`：通过（同列表 rehello；F1 在其外）
- `TestBudgetShrinkKeepsLiveOccupancy`：通过，minor（F8）
- `TestMachineConfigHashSharedAndNormalized`：通过（但 expected 用同一 `protocol.MachineConfigHash`，canonical 规则本身是自证）
- `TestTaskAttemptJSONShape`：**不通过**（F4）
- `TestRetentionKeepsUnreapedAttempts`：部分（F7）；`TestActiveTaskNeverGC`：通过
- `TestTaskNamespaceInputsPinnedAcrossConfigUpdate`、`TestTaskInputsMachineLayerReparsedPerNode`、`TestTaskNamespaceVarsNotInPublicJSON`、`TestTaskInputsOnlyNamedKeys`：D2 面通过（仅缺 attempt 行 `NamespaceVarsRevision` 断言）

### 待验证

- F1 的 `UpsertInstances` 空/缩容语义是否被父 A3 允许（存储注释“完整清单”与 A3“空=未知”冲突，需裁决）。
- `perCapability` 在 grant/EligibleNodes 中是否应强制（当前只在 hash/config 出现，未见调度消费）。
- 未跑任何测试；F2/F1 的失败断言是静态代码推理，未执行复现。

```json
{"ok": false, "reason": "S majors not independently cleared: critical rehello/instance-shrink can delete busy occupancy (A3 §4); stale grant/reject can clear a newer assignment; S1 TOCTOU and S2 old/new emit+tokens remain unverified by discriminating tests"}
```
