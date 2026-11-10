## 事实清单

1. **工作树匹配固定提交且无改动**：HEAD 为 `49253553ff5eec5f90e03cb3393a791284b408c0`，`git status --short` 为空。
2. **执行授权在写事务内重读当前事实**：grant 校验当前 token hash、assignment/node/proposed、窗口与 namespace；renew/result 重查当前绑定、取消与到期状态。证据：`internal/store/execution_auth.go:302`、`:361`、`:578`、`:612`、`:677`、`:716`。
3. **三层预算统计口径符合冻结要求**：全部 unreaped attempts 加真实 lease-free reservations；perCapability 按名字、缺省查 `"*"`，跨 namespace；pinned maxPerNode 按同 namespace/同名、跨版本。自身仅排除精确 node/task/assignment/proposed/slot 那一条。证据：`internal/store/execution_auth.go:168`、`:185`、`:220`、`:239`；槽唯一性：`internal/store/schema.sql:244`。
4. **reservation 到期替换不释放真实执行**：替换条件限定 lease 为空且 recorded expiry 已过；grant 同事务把绑定变成真实 lease 并清空 reservation 字段。证据：`internal/store/node.go:846`、`:1038`；内部新增事实仅为 `busy_assignment_id`、`busy_expires_at`：`internal/store/schema.sql:240`。
5. **历史 ACK 保留失败事实且重复回收不重写**：精确历史绑定核对后调用 `reapAttemptOn`；已 reaped 直接返回，首次更新使用 `reaped_at IS NULL`，不改 phase/error、不覆盖既有 finished_at。当前取消重复 ACK 进入该幂等路径。证据：`internal/store/execution_auth.go:813`、`:903`；`internal/store/task_attempt.go:411`、`:415`。
6. **R1 测试不再被 slot 范围拒绝掩盖**：total=2、合法 slot 0/1、perCapability=1，断言恰一成功及拒绝槽为空；另有不同槽预约、assignment 身份、预算维度隔离及整行 ACK 幂等断言。证据：`internal/store/v4test/execution_auth_test.go:498`、`:549`、`:623`、`:671`、`:823`、`:1105`、`:1175`、`:1254`。

## 关键调用链

- `GrantLeaseAuthorized(auth, taskID, assignmentID, proposedAttempt, slot, now)` → 事务内身份/候选/ns/配置校验 → `grantBudgetOn` → `bindSlotOn` → attempt 快照和任务授权事实原子落盘。证据：`internal/store/execution_auth.go:263`、`:443`、`:482`、`:505`。
- `AckCancelAuthorized(auth, ReapedInputs, processGroupReaped)` → 精确历史绑定核对；false 不释放，当前取消首次终结并回收，历史/重复只走 guarded reap。证据：`internal/store/execution_auth.go:779`、`:860`、`:880`、`:895`、`:903`。

## 风险与待验证

- 未确认 major/critical 漏洞或修复回归；重复释放调用属于非阻断简洁性问题。
- G2 result token 映射仍待父裁决，不能把当前 store 签名视为端到端已接通，也未要求新增协议字段。
- C3b manifest/GC、白盒、G2–4 刻意待验，不计作本门禁 major。
- 原四项固定门禁按任务输入接受；本轮不重复广泛测试、不声称全仓绿灯。

## 只读交付

改动文件：无。新增测试：无。  
验证：`git diff --check 133fd03^ 49253553 -- <七个范围文件>` → exit 0；未执行构建、lint/typecheck 或测试。仅阅读短 NEXT 卡及相关范围代码，未写文件、未启动子 agent。

DONE fixed49253553

{"ok":true,"findings":[{"path":"internal/store/execution_auth.go","line":748,"severity":"minor","evidence":"CompleteTaskAuthorized 调用 finishAttemptOn(reaped=true)，该函数在 task_attempt.go:378 已精确释放绑定，随后 execution_auth.go:752 再次释放；当前取消 helper task.go:1168 亦存在重复。相同事务和绑定守卫下第二次无匹配更新，非 confirmed major。"},{"path":"internal/store/execution_auth.go","line":655,"severity":"info","evidence":"CompleteTaskAuthorized 仍要求 attemptToken；NEXT-C3aR1.md:13 明确此 wire 映射问题待父处理。保留为未解决 G2 风险，本轮没有强迫 C3a 改签名或新增协议字段。"}],"reason":"strict implementation gate PASS at fixed 49253553: no confirmed major or critical in cumulative C3a+R1 scoped review; minor duplication and parent-pending G2 risk remain; not full-system green"}
