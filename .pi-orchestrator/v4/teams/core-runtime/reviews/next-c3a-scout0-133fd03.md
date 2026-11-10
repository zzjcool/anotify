## C3a implementation gate · 只读复审（commit 133fd03c575d14671dbd0366bef3d1d30c9945a6）

### 0. 身份与范围确认
- `pwd` = `/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next`；`git rev-parse HEAD` = `133fd03c575d14671dbd0366bef3d1d30c9945a6`，`git log` 顶条 `feat(store): 实现认证租约与三预算`。工作区仅 `.pi-orchestrator/v4/teams/core-runtime/NEXT-PROGRESS.md` 非源码改动（未写任何文件）。
- `git show --stat 133fd03` = 12 files：`execution_auth.go(+902 新)`、`node.go(+129/-...)`、`task.go(-532)` + 9 个 v4test 测试文件（与"本 commit 12 测试文件"口径一致；实为 3 源码 + 9 测试）。
- `task.go` 删除的公开入口精确为 `GrantLease/RenewLease/CompleteTask/CompleteTaskAt/AckCancel`（`git diff 133fd03^ 133fd03 -- internal/store/task.go | grep '^-func'`），**无兼容层保留**；全仓非测试代码对这四个名字零调用（仅注释）。父批准的"旧公开入口删除"事实成立，不判缺 compat。

### 1. 事实清单（diff 内证据）
- `internal/store/execution_auth.go:33-75` `NodeAuthorization{NodeID,NodeTokenHash}` + `authorizeNodeOn`：同事务重读 `nodes.token_hash` 并做**当前库事实**等值比较，空身份 fail closed（`:43-48`），令牌轮换即失效。
- `execution_auth.go:258-548` `GrantLeaseAuthorized` 事务内顺序：①当前 token hash（`:300`）→ phase/cancel（`:322-335`）→ 节点在线（`:337-349`）→ **assignment+node+proposedAttempt 三项精确匹配**（`:351-365`）→ 过期候选仅清自己且不改新候选（`:366-380`）→ ns 交集（`:382-385`）→ lifecycle/maxAttempts（`:387-421`）→ 机器 applied（`:423-449`）→ 三层预算（`:451-491`）→ slot 编号/`bindSlotOn` CAS（`:494-544`）→ attempt 快照 + tasks 授权事实**同一事务**（`:546-587`）。
- `execution_auth.go:157-231` `grantBudgetOn`：①`limits.Total`（local>node 合并快照）②`PerCapability[capName]` 否则 `"*"`，口径 `t.capability=?` 跨 ns（`:197-218`）③pinned `maxPerNode`，口径 `t.namespace_id=? AND t.capability=?` 跨版本（`:220-231`）。占用 = `task_attempts.reaped_at IS NULL`（不看 phase）+ `node_instances.busy_lease_id=''` 的真实 reservation；自己的 reservation 按 `i.busy_task <> ?` 排除（`:178-187`），grant 成功时原地转执行（`node.go:1008-1023`），**无 phase 假 reap、无假 lease**。
- `execution_auth.go:552-641` `RenewLeaseAuthorized`：token（`:583`）+ 归属/attempt/lease/cancel（`:607-620`）+ `lease_until <= now` 拒（`:614`）+ ns（`:627`）+ SQL 再带 `cancel_requested_at IS NULL AND lease_until > ?`（`:625`）→ expiry==now 不可复活。
- `execution_auth.go:648-746` `CompleteTaskAuthorized`：token hash（`:714`）、expiry（`:721`）、cancel→`ErrTaskNotRunning`（`:717-720`）、ns（`:723`）、同事务 finish+attempt reaped+仅本 attempt 槽释放（`:727-744`）。
- `execution_auth.go:769-902` `AckCancelAuthorized`：历史 node/lease/slot/instance 全部精确核对（`:800-822`）；当前 attempt+running+cancel+reaped 同事务终态+回收（`:838-862`）；reaped=false 只"收到"不释放不清租约（`:830-836`、`:864-871`）；历史/已重排只写该 attempt reaped 并释放它自己的槽（`:873-898`）。
- `internal/store/node.go:815-851` `recordReservationOn`：只接受匹配中枢当前候选（pending/同 node/同 assignment/同 proposed/窗口未过/未取消）的 Busy=true 事实，落 `busy_task/busy_attempt=proposed/busy_lease_id=''`，不抢别人绑定、不伪造 lease。
- `node.go:864-964` `ReclaimStaleSlots`：空 observations=0（`:871-874`）、Busy=nil/slot<0 跳过（`:892`）、执行类 idle 需 task+attempt+lease 全等（`:928-935`）、reservation 类 idle 需 assignment/proposed 相符且**只清 reservation 不伪造 reaped**（`:946-964`）。rehello 空申报经 `UpsertInstances` 只改 alive/updated_at（`node.go:681-690`），不清 busy。
- `node.go:996-1030` `bindSlotOn`：空槽或**本候选自己的 reservation**（`busy_lease_id=''`）CAS，别人的 reservation/执行不可抢；`LeaseSlot` 传 proposed=0 保持旧语义（`:984-990`）。
- 测试面（`execution_auth_test.go`，本人实跑）：12 个新用例覆盖轮换 token/ns 撤销/过期与取消不可复活/精确历史 ACK/三层预算/未回收+真实 reservation+自身只计一次/并发窄预算恰一/跨 ns 同名/`*` 回退/pinned 跨版本/D2 ns pin/attempt 机器快照。断言是判别式（去掉对应实现会红），非弱化。
- 夹具迁移保留原断言：`task_test.go`/`task_attempt_test.go`/`emit_test.go`/`retention_test.go`/`stats_test.go`/`cluster_node_test.go`/`task_inputs_test.go` 仅把调用迁到 `*Authorized` 并补认证夹具；`g14_helpers_test.go:189-231` 显式说明不绕过认证。

### 2. 关键数据流
`AssignTaskAt`（候选 assignment/proposed/窗口）→ `GrantLeaseAuthorized`（认证→候选 CAS→ns→预算→slot CAS→attempt 快照+lease/token/instance 落 tasks，同一 BEGIN IMMEDIATE）→ `RenewLeaseAuthorized`/`CompleteTaskAuthorized`（当前 token+attempt+lease+token hash+expiry+cancel+ns 再校验）→ `AckCancelAuthorized`（历史精确绑定只回收旧槽；当前取消原子终态）。预算读取面与写面全在同一事务，无外部预检。

### 3. 风险与坑（均已核，不构成 major）
- 旧公开入口删除后 `internal/store` **in-package** 测试仍编译失败，但缺的是 `ArtifactBuilder/Playbook/Lifecycle/AppliedRef` 等类型——在父 commit `133fd03^` 同样不存在（`git grep` 双证），且 in-package 测试无人调用被删方法；**不是本 commit 回归**。
- `internal/ws/node_handshake.go:116` 仍是旧 4 参 `ReclaimStaleSlots` 签名、`internal/route` 有更早缺失类型 → 上层未接线/未编译是契约已知"no upper callers yet"，reservation 账本目前只由测试驱动，G2 才接真 session；不作 C3a finding。
- 测试语义迁移：`TestTaskOldAckCannotReleaseNewAttemptSlot` 原先靠"历史 attempt 一律 ErrLeaseMismatch"断言，现改为"错 lease 绑定 fail closed"+新用例验"正确历史绑定只释放旧槽"；属父批准的语义更替，不是放松。

### 4. Findings（无 major/critical）
| # | 位置 | 级别 | 证据（一句因果） |
|---|---|---|---|
| 1 | internal/store/execution_auth.go:178-187 | minor（unverified） | 自身 reservation 排除按 `i.busy_task <> taskID` 对**所有槽**生效，而非"本 grant 绑定的那个槽"；若同一 task 被节点申报了两条真实 reservation，预算会少算 1（不越 slot CAS，最多预算偏差）。 |
| 2 | internal/store/node.go:946-961 | minor（unverified） | reservation idle 清理只比对 `(taskId, proposedAttempt)`（台账无 assignment 列），号段重复时旧 idle 事实可清掉更新的 reservation；正常 proposed=attempt+1 单调下发时不可达。 |
| 3 | internal/store/node.go:1048-1062 | minor（unverified） | `releaseSlotBindingOn` CAS 为 `busy_task=? AND (busy_attempt=? OR busy_attempt=0)`，不含 lease；当前所有写入点都先核 attempt 唯一性，故仅在号段复用/异常台账下才可能误释。 |
| 4 | internal/store/node.go:984 | minor（unverified） | `LeaseSlot` 仍是公开、无认证的槽绑定原语（仅测试调用），可绕过 grant 的认证/预算直接占槽，且此类绑定既非未回收 attempt 也非 reservation（预算看不见）；当前无产品调用、不能授权执行。 |
| 5 | internal/store/execution_auth.go:826-836（配合 internal/store/task_attempt.go:363-366） | minor | 当前取消路径重复 ACK 走 `finishAttemptOn(reaped=true)` 会覆写 `reaped_at/reclaim_reason` 时间戳（历史重复走 `reapAttemptOn` 才是幂等）；无状态/安全影响。 |
| 6 | internal/store/execution_auth.go:654,714（依赖 internal/protocol/exec_frames.go:191-209） | minor（unverified） | `CompleteTaskAuthorized` 强制 `attemptToken`，但冻结的 `TaskResultFrame` 无该字段且契约禁新增 protocol 字段；G2 必须在 session 侧缓存 grant 明文 token 才能调用，否则 result 接线受阻——属接口/G2 风险，非本 commit 自测失败。 |

### 5. 我自己跑过的短验证（真 exit）
- `gofmt -l` 三个源码+新测试 → 空，exit 0；`go build ./internal/store/ ./internal/protocol/` → exit 0；`go vet ./internal/store/v4test/`、`./internal/artifact/` → exit 0。
- 新用例 `go test ./internal/store/v4test -count=1 -v -run '<12 个新用例>'` → 12 PASS（含 2 subtest），0.35s，exit 0。
- 固定数组 `go test ./internal/store/v4test/ -count=1` → `ok 24.119s`，exit 0；`go test ./internal/artifact/ ./internal/protocol/ -count=1` → ok，exit 0。
- 未复跑 `go test ./...`/`make contract`/`make e2e-quick`（父声明它们尚未被执行、且已知全 repo 非绿；遵守不跑已知失败/不长裸 bash）。缺 web/DB 外部工具，未做运行期端到端。

### 6. 结论
C3a 的三条硬需求（D2 首 grant ns pin/每 attempt 机器快照、D4 current token+ns 同事务的四类授权进入口与历史回收、A3 真实 reservation 与 unreaped 占用/自身只计一次、不按 phase 假 reap）在 diff 内均有对应实现 + 判别式测试，且无公共无认证 grant/renew/result/cancel bypass；仅剩 6 条 minor/unverified（无 major/critical），故 implementation gate 通过。

```json
{"ok": true, "findings": [{"path": "internal/store/execution_auth.go", "line": 178, "severity": "minor", "evidence": "own-reservation exclusion is by task id across all slots (i.busy_task <> taskID), not the bound slot; a double-reported reservation for the same task can undercount budget by one (unverified, slot CAS still bounds physical occupancy)"}, {"path": "internal/store/node.go", "line": 946, "severity": "minor", "evidence": "reservation idle clear matches only (taskId, proposedAttempt) because the ledger has no assignment column; a stale idle fact with a reused proposed number could clear a newer reservation (unverified, unreachable if proposed = attempt+1 is monotonic)"}, {"path": "internal/store/node.go", "line": 1048, "severity": "minor", "evidence": "releaseSlotBindingOn CAS is busy_task=? AND (busy_attempt=? OR busy_attempt=0) with no lease/assignment term; safe only because attempt numbers are unique per task (unverified under abnormal ledgers)"}, {"path": "internal/store/node.go", "line": 984, "severity": "minor", "evidence": "LeaseSlot remains a public unauthenticated slot-binding primitive (test-only callers) that can occupy a slot outside grant auth/budget and is counted as neither unreaped attempt nor reservation; cannot authorize execution"}, {"path": "internal/store/execution_auth.go", "line": 826, "severity": "minor", "evidence": "duplicate cancel ACK on the current attempt goes through finishAttemptOn(reaped=true), overwriting reaped_at/reclaim_reason (task_attempt.go:363-366), unlike the idempotent historical reapAttemptOn path; no state/security impact"}, {"path": "internal/store/execution_auth.go", "line": 654, "severity": "minor", "evidence": "CompleteTaskAuthorized requires attemptToken, but frozen protocol.TaskResultFrame (internal/protocol/exec_frames.go:191-209) carries no attemptToken and contract forbids new protocol fields; G2 must cache the grant plaintext token in session or result wiring is blocked (unverified G2 risk)"}], "reason": "implementationgate OK: commit 133fd03, 3 source + 9 test files; authenticated grant/renew/result/cancel in one BEGIN IMMEDIATE with current token+ns+attempt/lease/token/expiry/cancel, 3 independent budgets (total/perCapability incl '*' cross-ns/pinned maxPerNode cross-version) over unreaped+real reservations with own-reservation once, history ACK exact-binding-only; old public unauth entries deleted with no non-test callers; 12 new discriminating tests PASS, full v4test 24.1s PASS, artifact/protocol PASS, gofmt/vet/build clean; 6 minor/unverified only, no major/critical"}
```
