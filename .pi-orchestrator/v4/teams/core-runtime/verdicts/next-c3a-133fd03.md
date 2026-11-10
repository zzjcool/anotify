## V4-CORE-RUNTIME-NEXT / C3a 交付报告（implementationgate，非 T1 DONE）

### 0. 工作区 / 交付形态
- cwd（已显式 pwd + 绝对 cd）：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next`
- 分支：`pi-subagent/v4-core-runtime-next-59fc6b04`
- commit：`133fd03` · HEAD：`133fd03c575d14671dbd0366bef3d1d30c9945a6`
- `feat(store): 实现认证租约与三预算`
- 依 local contract（父批准 source `df54061`、contract `e318a5e`）：**NoPush / NoMerge / NoMR，本地分支交付**，无远端链接（不伪造）。
- 未写 `NEXT-PROGRESS.md` 等 manager 文件。注意：该文件在我收工时显示一处**外部未提交改动**（"First sole writer c3a-auth-budget admitted…"），不是我写的、也**不在** commit `133fd03` 内（centurion 所有权，未触碰）。

### 1. 改动文件（12，全部在授权清单内）
```
internal/store/execution_auth.go        (新)
internal/store/node.go                  (Busy=true reservation 落真实槽账本；bindSlotOn 支持消费自己的 reservation)
internal/store/task.go                  (退役旧公开 GrantLease/RenewLease/CompleteTask/CompleteTaskAt/AckCancel)
internal/store/v4test/execution_auth_test.go (新，C3a 用例)
internal/store/v4test/task_test.go      (调用迁移 + 旧 ACK 用例精确迁移)
internal/store/v4test/task_attempt_test.go / cluster_node_test.go / emit_test.go / retention_test.go / stats_test.go / task_inputs_test.go / g14_helpers_test.go (调用/夹具迁移)
```
未改：`schema.sql`/`migrate.go`（C3a 不需要新列：reservation 复用 A3 现有 `node_instances.busy_task/busy_attempt/busy_lease_id`，不加 bootId/新 wire）、`emission.go`（`EmitAuthorized` 同事务 hash 授权原样保留，未退化）、retention/artifact/manifest/上层 runtime/API/auth/protocol/T2/T3/父文档。

### 2. 实现要点
- `NodeAuthorization{NodeID,NodeTokenHash}` 来自认证会话；所有执行入口在同一 `BEGIN IMMEDIATE` 事务内重读 `nodes.token_hash`、node 归属、node∩ns 交集，再做任务/执行事实判定。
- `GrantLeaseAuthorized`：identity → assignment+proposedAttempt+node+窗口精确匹配（stale 只拒、不破坏新候选）→ ns → maxAttempts → applied → **三预算** → 真实 slot 绑定（可消费自己已接受的 reservation）→ attempt 计数 + 机器 vars/limits/source/revision/hash 快照，同事务提交。
- 三预算：① local⊕node merged `limits.total`；② `limits.perCapability[capName] else "*"`，口径跨 namespace 同名能力；③ pinned `concurrency.maxPerNode`（任务钉住版本），口径同 ns+同名跨版本。
- 占用口径：`task_attempts.reaped_at IS NULL`（不看 phase，未回收就占）+ 节点 `Busy=true` reservation 落槽账本（`busy_lease_id=''` 判别，不伪造 lease）；自己的 reservation 按 task id 排除并 +1，恰好一次。预算缩小只阻止新授，不清存量 busy；空 observation/rehello 空申报/fake phase 不释放。
- `RenewLeaseAuthorized` / `CompleteTaskAuthorized`：当前身份 + node + ns + current attempt/lease(/attemptToken) + 未取消 + `lease_until > now`；`expiry==now` 与取消后不得复活。Result DTO 沿用现有 `TaskResult`（无新 protocol 字段，C3b 再 typed 升级）。
- `AckCancelAuthorized`：当前身份 + 历史 attempt/lease/slot/instance 精确绑定；`processGroupReaped=true` 才收；当前 attempt+cancel 同事务原子 finalize；历史/requeued ACK 只回收旧槽，不改新 phase/token，不恢复旧 result/emit；false 不释放；重复幂等。G2 仍须先 `protocol.ValidateCancelAck`。
- `ReapTaskAttempt` 保留原签名与「当前身份 + 历史绑定、不依赖 active result」语义（新增轮换反例覆盖）。
- 旧公开无认证 `GrantLease/RenewLease/CompleteTask/CompleteTaskAt/AckCancel` **已删除**（无 compat bypass）。

### 3. 实际新增 Go 签名
```go
type NodeAuthorization struct {
	NodeID        string
	NodeTokenHash string
}

func (d *DB) GrantLeaseAuthorized(ctx context.Context, auth NodeAuthorization,
	taskID, assignmentID string, proposedAttempt, slot int, now int64) (
	leaseID, attemptToken string, attempt int, expiresAt int64, err error)

func (d *DB) RenewLeaseAuthorized(ctx context.Context, auth NodeAuthorization,
	taskID string, attempt int, leaseID string, now int64) (int64, error)

func (d *DB) CompleteTaskAuthorized(ctx context.Context, auth NodeAuthorization,
	taskID string, attempt int, leaseID, attemptToken string, now int64, result TaskResult) error

func (d *DB) AckCancelAuthorized(ctx context.Context, auth NodeAuthorization,
	in ReapedInputs, processGroupReaped bool) error
```
内部新增：`authorizeNodeOn`、`namespaceAllowedOn`、`maxPerNodeOn`、`grantBudgetOn`、`recordReservationOn`；`bindSlotOn` 内部签名增加 `proposedAttempt`（未导出）。退役导出方法：`GrantLease`、`RenewLease`、`CompleteTask`、`CompleteTaskAt`、`AckCancel`。

### 4. 新增测试（12 个函数，均在 `internal/store/v4test/execution_auth_test.go`）
```
TestAuthorizedEntriesRequireCurrentNodeCredential
TestAuthorizedRenewAndResultRequireCurrentNamespace      (subtests: authorized / declared)
TestGrantStaleProposedSlotAndExpiredBindingPreserveNewAssignment
TestGrantPerCapabilityAcrossNamespaces
TestGrantWildcardPerCapabilityAcrossNames
TestGrantPinnedMaxPerNodeAcrossVersions
TestGrantCountsUnreapedExecutionsAndRealReservations
TestConcurrentNarrowBudgetGrantsExactlyOne
TestRenewAndResultCannotReviveExpiredOrCancelled
TestAckCancelAuthorizedHistoricalBinding
TestFirstGrantAfterNamespaceChangeKeepsPinnedRevision
TestMachineLimitsSnapshotPinnedPerAttempt
```
v4test 测试函数总数：218 → **230**；无 skip/noop/stub/legacyDTO，原断言含义保留。

### 5. 逐项红→绿（变异注入 → 目标测试失败 → 还原 → 通过；每条命令均 `go test ./internal/store/v4test/ -count=1 -run '^<Test>$'`，exit 0/1 与完整日志在 /tmp）
| 需求 | red（exit=1） | green（exit=0） |
|---|---|---|
| token 轮换后 grant/renew/result/reap 同 UID 仍失效、当前身份成功 | `...-red-token-rotation.log` | `...-green-token-rotation.log` |
| ns 撤销（authorized/declared）在 renew/result 生效 | `...-red2-ns-revoke.log` | `...-green2-ns-revoke.log` |
| 旧 assignment/错 proposed/过期窗口不破坏新候选 | `...-red-proposed.log` | `...-green-proposed.log` |
| perCapability 跨 ns 同名 | `...-red-percap.log` | `...-green-percap.log` |
| perCapability["*"] 兜底 | `...-red2-wildcard.log` | `...-green2-wildcard.log` |
| pinned maxPerNode 同 ns/名跨版本 | `...-red-maxpernode.log` | `...-green-maxpernode.log` |
| 未回收执行占预算（不看 phase） | `...-red2-unreaped.log` | `...-green2-unreaped.log` |
| 真实 busy reservation 占预算 | `...-red2-reservation.log` | `...-green2-reservation.log` |
| 自身 reservation 不双计 | `...-red2-own-double.log` | `...-green2-own-double.log` |
| 并发窄预算恰一成功 | `...-red-concurrent.log` | `...-green-concurrent.log` |
| expiry==now / 取消后续租/结果不复活 | `...-red-no-revive.log` | `...-green-no-revive.log` |
| 旧 ACK 错绑定拒/正确仅旧回收/幂等/false 不释 | `...-red-ack-binding.log` | `...-green-ack-binding.log` |
| task create→改 ns→首次 grant 仍原 pin/revision | `...-red-ns-pin.log` | `...-green-ns-pin.log` |
| 同 attempt 旧 limits 不变、新 attempt 新 limits | `...-red2-limits-snapshot.log` | `...-green2-limits-snapshot.log` |

（日志前缀 `/tmp/anotify-core-next-c3a-`；早期 first-round 的 `red-unreaped/reservation/wildcard` 因当时用例设计被 slot/maxPerNode 混淆已废弃，以 `red2-*` 为准。每份 log 末尾含真实 `exit=`。）

### 6. 固定四条自验（HEAD `133fd03`，每条先 pwd + 绝对 cd；Go env：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`；timeout 60 + 全日志）
| # | 命令 | 结果 | 日志 |
|---|---|---|---|
| 1 | `gofmt -l internal/store/ internal/artifact/` | exit=0（无输出） | `/tmp/anotify-core-next-c3a-final2-gofmt.log` |
| 2 | `go build ./internal/store/ ./internal/artifact/` | exit=0 | `/tmp/anotify-core-next-c3a-final2-build.log` |
| 3 | `go vet ./internal/store/v4test/ ./internal/artifact/` | exit=0 | `/tmp/anotify-core-next-c3a-final2-vet.log` |
| 4 | `go test ./internal/store/v4test/ ./internal/artifact/ ./internal/protocol/ -count=1` | exit=0 | `/tmp/anotify-core-next-c3a-final2-test.log` |

第 4 条原文：
```
ok  github.com/zzjcool/anotify/internal/store/v4test  25.991s
ok  github.com/zzjcool/anotify/internal/artifact      2.580s
ok  github.com/zzjcool/anotify/internal/protocol      0.060s
```
（HEAD 提交前后各跑一轮，`-head-*` 与 `-final2-*` 均 exit 0。）

### 7. 迁移说明（保留原断言含义）
- 旧公开无认证入口删除，v4test 调用迁移到 `*Authorized` + 测试夹具 `nodeAuthByID`/`ackCancel`/`ackReapInput`（从 nodes 行/attempt 行取真实 token hash 与历史绑定，不伪造）。
- `lifeCycleCap` 等夹具显式声明 `concurrency.maxPerNode=2`（这些用例验的是槽位/重排/回收；默认 1 的拒绝由新 C3a 用例覆盖）；stats/emit 夹具同样显式声明 2。
- `TestTaskOldAckCannotReleaseNewAttemptSlot` 的旧 unauth ACK 断言迁移为「认证 ACK + 错 lease 绑定 → ErrLeaseMismatch 且一个槽都不放」，保持原不变量；正确历史绑定只回收旧槽由新用例覆盖。
- `AckCancel` 迁到 helper 后仍按 attempt 行精确构造 lease/slot/instance；旧 `attempt+1` 的 fail-closed 断言保留。

### 8. 未决风险 / 后续（本轮不伪称完成）
- **C3b 未做**：typed artifacts `[{path,sha256}]`、upload registry（16MiB/64MiB/32、7d/30d）、manifest/result 同事务引用、artifact FK/GC/tombstone、G3 流式 spool；当前 `TaskResult.Artifacts` 仍是旧字符串表面。
- **白盒闭包未做**：`internal/store` 旧 in-package tests 仍编译失败（历史 G1-6TEST 范围），因此 `go vet ./internal/store/` 无法跑；本轮只做 v4test 外部包 + 指定四条。
- **G2 接线要求**：wire NodeSession→`NodeAuthorization`；result 帧 attemptToken；`cancel.ack` 先 `protocol.ValidateCancelAck`（slot presence/leaseId）再 `AckCancelAuthorized`；hello/accept 的 `Busy=true` reservation 事实必须经 `ReclaimStaleSlots`（现有入口）落账本，否则预算看不见「节点已接住但未 grant」的占用。
- **G2-G4**：route/ws/source/lease 运行闭环、五份 API spec、E2E/tooling 均未做；Node 最终产品支持仍待 root。旧 `internal/route`、`internal/lease` 等包仍因 v3 遗留类型无法构建（**预先存在**，非本次回归，未用兼容层掩盖）。
- budget 语义注意：`maxPerNode` 默认 1 现已强制执行；纯中枢 assignment（节点未接受）不占预算。
- 本报告仅为 C3a implementationgate ok=true；**不是 T1 DONE**，最终 `go test ./... / make contract / make e2e-quick` 仍未运行/未通过（按 recovery 记录保持未声称）。

```json
{"ok": true, "reason": "C3a done at HEAD 133fd03 (local branch pi-subagent/v4-core-runtime-next-59fc6b04, no push/merge/MR): authenticated grant/renew/complete/ack + three budgets + A3 reservation ledger; 12 new tests red->green; fixed 4 checks all exit 0; C3b/whitebox/G2-G4 unresolved, not T1 DONE"}
```
