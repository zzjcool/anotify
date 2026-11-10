## Builtin/auth/namespace mapping gate（只读，基于当前证据）

### 事实

- 旧文件共有 **18 个测试**：`b5adeda:internal/store/builtin_test.go` 中有 18 个 `func Test`（行号 24/69/90/121/209/235/307/364/380/414/477/502/548/602/630/742/792/893），A-continue 表格在 `TEST-MAPPING.md:1046-1053` 逐行覆盖了全部 18 个。没有按文件名直接退役任何测试。
- 纯 v3 退役理由成立：旧文件的种子播种、`notify.phone` builtin、central handler/claim、origin shell、port、示例脚本都随冻结决策删除；它们在生产代码 `internal/store/*.go` 中已经没有任何残留，相关引用只出现在 `internal/executor`、`internal/route`、`internal/server` 和 `internal/lease` 中，这属于 B 轮待处理项。
- 代理点或等价的断言已经核对：
  - `namespace_test.go:137` 断言注册时建立 `default` 命名空间、能力为 0 个；`password_test.go:163` 断言密码列与命名空间在同一事务内创建且能力为 0 个；`:269` 断言并发首次注册时恰好只有 1 个 admin。
  - 实际原子回滚另由 `internal/auth/password_test.go:490` 覆盖（重复用户名后仍只有 1 个命名空间），但 `TEST-MAPPING.md:1046` 没有引用该测试。
  - `task_test.go:408` 覆盖成功结果的相位、summary、exitCode、finishedAt、acceptedAt；`task_test.go:498` 覆盖终态不可变；`v4test/task_test.go:1124` 覆盖旧 attempt、错 lease、过期租约被拒。
  - `capability.go:515` 的 `SetCapabilityDisplayName` CAS 由 `capability_test.go:344` 和 `v4test/capability_test.go:329` 覆盖。
  - `deployment_test.go:21` 覆盖“产物=能力正文规范字节”；`capability_test.go:88/174` 覆盖幂等与内容寻址。
- auth、passkey、recovery、devices、VAPID、push 的安全测试没有被删除：`b5adeda..HEAD` 之间只修改了 `internal/auth/password_test.go` 和 `internal/store/admin_test.go`，没有删除 `auth_test.go`、`passkey_*_test.go`、`devices_update_test.go`、`internal/auth/recovery_test.go`、`internal/server/recovery_test.go` 或 VAPID/push 相关测试。

### Findings（旧 case 缺失或弱断言）

- `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md:1048` — **minor** — `TestFinishTaskWritesOutcome`（`b5adeda:internal/store/builtin_test.go:307`）被标为 `migrated`，但指定的 `internal/store/task_test.go:408` 只断言成功结果的相位、exitCode 和时间，没有断言旧 case 的“失败且无原因→兜底 `failed`”以及“成功时 error 为空”这两点；`normalizeTaskError`（`internal/store/task.go:859-869`）在 CompleteTask 路径上没有测试，`orphan_test.go:259` 只覆盖 `AbandonPendingTask`。
- `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md:1049` — **minor** — `TestFinishTaskRejectsInvalidStatus`（`b5adeda:internal/store/builtin_test.go:364`）被标为 `migrated`，但指定的 `task_test.go:498` 和 `v4test/task_test.go:1124` 只断言终态不可变以及旧 attempt、错 lease 被拒，没有测试 `execution_auth.go:689-692` 的“result phase 只接受 succeeded|failed|abandoned”分支；仓库中没有测试把 `TaskResult.Phase` 设为 `TaskCancelled`、`TaskPending` 或 `TaskRunning` 来提交。
- `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md:1046` — **minor** — 该行把“注册原子性”归给 `namespace_test.go:137` 的 happy path，但该断言实际只证明用户和 default 命名空间在成功时都存在；真正的失败回滚断言位于 `internal/auth/password_test.go:490-533`，没有被引用（不变量本身没有丢失，只是引用偏弱）。

### 结论

除上述旧结果事实对应的两个断言缺口和一处引用偏弱外，其余旧 case 都有明确的纯 v3 退役理由，或存在真实的当前同名或等价断言；没有整文件按 `builtin` 文件名退役的情况，也没有把 auth/passkey/recovery/devices/VAPID/push 安全族一并退役。`ok` 取决于映射是否要求每个旧 case 都有真实断言：本轮发现 2 个标记为 `migrated` 的旧 case 没有等价断言，因此判定映射门禁未完全闭合。

{"ok": false, "reason": "2 old cases marked migrated lack equivalent assertions: TestFinishTaskWritesOutcome (failure-empty-reason fallback untested) and TestFinishTaskRejectsInvalidStatus (invalid result phase guard untested); other 16 are pure-v3 retirements or covered, auth/passkey/recovery/devices/VAPID/push remain."}
