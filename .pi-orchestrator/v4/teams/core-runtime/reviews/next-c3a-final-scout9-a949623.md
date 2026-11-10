## finalC3a：simplicity/interface 门禁通过

固定 HEAD：`a9496238e34a96e1d5ad0087a34e9f02d92f821d`；工作目录与指定 WT 一致，工作树干净。**未发现本范围 Major；不代表 T1 DONE 或全仓绿灯。**

### 事实清单

1. `CompleteTaskAuthorized(ctx, auth, taskID, attempt, leaseID, now, result)` 不要求不存在于结果 wire 的明文 token；没有将数据库 storedTokenHash 冒充 caller 凭证。证据：`internal/store/execution_auth.go:657–658,681–729`。
2. 当前节点 hash、task-node 绑定、namespace、current attempt/lease、expiry、cancel 均在结果写事务内检查。底层 DSN 使用 `_txlock=immediate`。证据：`internal/store/execution_auth.go:50–70,673–729`；`internal/store/store.go:104`。
3. 三预算统计所有 unreaped attempts 与真实 reservations；own reservation 仅排除精确 node/slot/task/proposed/assignment 绑定。证据：`internal/store/execution_auth.go:159–240`。普通 SQL/helpers，没有新增 quota/路径锁框架。
4. 历史回收只补回收事实并精确释槽；保留已有 error/phase/finishedAt；重复 ACK 不重新写入。证据：`internal/store/execution_auth.go:898–906`；`internal/store/task_attempt.go:411–429`。
5. R2 保留精确首次时间与重复整 task、old/new attempt、槽账本比较。证据：`internal/store/v4test/execution_auth_test.go:1078–1115,1364–1409`。
6. 新测试使用 captured credential、实际 tokenless result；总预算测试采用旧高位 live slot，缩容后申请合法 slot0 或并发合法 slots0/1，其他预算为10。证据：`internal/store/v4test/execution_wire_test.go:17–22,88–95,140–199,222–330`。
7. Renew 的 expiry/cancel 检查保留；provided token 校验未因 result 改签而撤销，emit wrong-token 断言仍在。证据：`internal/store/execution_auth.go:616–631`；`internal/store/task.go:1119–1130`；`internal/store/v4test/emit_test.go:247`。最终生产差异仅限 result 授权签名及其 token-fiction 检查删除。

### 关键调用链

- Grant → current identity/assignment/ns → applied snapshot → 三预算 → 精确 slot CAS → attempt snapshot/task 授权 → commit。证据：`execution_auth.go:303–546`。
- Result → current identity/execution/ns → `finishTxGuarded` → attempt/result facts → 精确释槽 → commit。证据：`execution_auth.go:681–755`。
- Historical ACK → current identity/exact historical binding → `reapAttemptOn` → 精确旧槽释放。证据：`execution_auth.go:808–906`。

### 风险与待验证

- waiting/backoff SQL 有轻微重复，不构成 Major。
- **安全 terminal duplicate-result ACK 路径仍缺失**：终态直接拒绝，尚无 exact recorded-result 比较。不能将终态 `GetTask` 或 `ErrInvalidTransition` 直接转换成 ACK。证据：`execution_auth.go:708–710`。这是已报告的未来 C3b/G2 边界，不是本轮伪造完成。
- C3b typed manifests/references/GC、whitebox、G2–4 与最终全系统门禁仍待验证。

### 验证与交付

本轮改动文件：无；新增测试：无。只读检查 `git diff --check` → exit0。

读取并接受 supplied fixed4：gofmt、build、vet、v4test/artifact/protocol 均 exit0；各日志确认指定 WT 与固定 HEAD。证据：`/tmp/anotify-core-next-c3awire-check{1-gofmt,2-build,3-vet,4-test}.log:1–7`。未重复运行测试、修改文件或启动子 agent。

{"ok":true,"findings":[{"path":"internal/store/execution_auth.go","line":450,"severity":"minor","evidence":"Waiting/backoff updates repeat across budget, range and slot-CAS refusals; local duplication only, no Major interface/framework issue."},{"path":"internal/store/execution_auth.go","line":708,"severity":"info","evidence":"Terminal result replay remains fail-closed without safe exact-fact ACK classification; explicitly deferred to C3b/G2, not hidden or replaced with unsafe Get-check ACK."}],"reason":"Strict finalC3a simplicity/interface implementation gate PASS; major=false. Fixed a949623 and correct clean WT verified; supplied four scoped gates accepted as exit0. No forbidden framework or unapproved semantic relaxation found. C3b/whitebox/G2-4 pending; not T1 DONE or full-green."}
