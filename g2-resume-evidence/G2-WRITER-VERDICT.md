# G2 · Writer 交付事实与裁决证据

- 分支：`pi-subagent/core-g2-resume-52fc54d4`
- 基线：`5441426`（父冻结卡 `G2-RESUME-FROZEN.md`）
- **source code commit**：`e552bd3`（136 files changed, +4897/−52940；含旧包/旧产品删除）
- 本文件 + `TEST-MAPPING.md` G2 行：紧随 code commit 的文档 commit（父以 `git log -1` 取本文件 SHA）
- 本节点身份：G2 唯一 source writer，`worktree:false`，未派任何嵌套 writer/subagent，未 push/MR/merge/rebase/reset。

---

## 1. 本次真实进度 / checkpoint

**已完成（本树可编译 + 真实测试绿）**：

| 包 | 内容 |
|---|---|
| `internal/route` | v4 纯路由投影：`spec.next` / `source.targets` / `$(payload.路径)` 参数对齐、default 补齐、`param_missing/param_schema/param_type/event_schema/no_target/downstream_missing` 剪枝、任务钉住 name/version/digest/params（pending，零执行授权）、授权 emit（`EmitAuthorized` 先授权后幂等）。 |
| `internal/source` | manual/webhook/cron 统一 `Ingest` + 同一份 `Receipt`；`(source, deliveryId)` 保留窗口幂等并回原 event/trace；`payloadSchema` → `event_schema` 入库+剪枝不派发；剪枝逐条留痕（`source='route'`）；`Payload` 一层覆盖；0 写 dry-run（已保存/未保存源都可预览）；停用 webhook 仍 404 语义。 |
| `internal/trigger` | cron 到点走同一条 `Ingest`；按上次计划时刻推进 `next_fire_at`；Ingest 失败也推进（防热循环）且错误逐条返回；永不成立的表达式推到远处并记 error。 |
| `internal/lease` | 真实 store 的 `RecoverLostLeases`（幂等回 pending+backoff / 非幂等 abandoned，**不释放物理槽**）、`RunRetention`（TTL=0 永久、活动任务不删）、`MaintainStats`；真实占用投影（Total/Busy/Free）。 |
| `internal/ws` | 真实 httptest WebSocket + 共享 `protocol.Decode`/方向：hello 认证（token hash→当前 nodes 行）+ welcome；rehello 只更新事实、不释放 busy；dispatch 只给 assignment/proposedAttempt/30s 窗口（无 lease/token/slot）；accept→`GrantLeaseAuthorized`（同 slot 回带）；renew/ack（expiry==now 拒）；result 分类授权 + 仅 Recorded/Duplicate ACK；cancel/cancel.ack 精确历史 CAS（错 lease/slot/false/重复）；emit 转发（no-id/同 id 回放/冲突/撤销/未声明/缺目标）；历史日志真实 attempt→历史 node + requestId/taskId/attempt 关联；人工部署（在线通知、离线 pending、重连重放、回执四元组校验）；upgrade/upgrade_ack；未知帧不断连、坏帧/方向错误断开。 |
| `internal/logging` | **未改**（独立 logger 保留），原测试原样通过。 |
| 删除 | `internal/executor/**`（v3 中枢能力执行器退役）、`cmd/agent/**`（旧 Node 产品退役，v4 唯一 Node 是 `cmd/anode`）。 |
| 父纠错 | G2 格式门禁改为**只读** `test -z "$(gofmt -l …)"`；原 `gofmt -w` 验收作废（已写进 `.pi-legion/contract.md` G2 段与 `G2-RESUME-FROZEN.md`，仅此两处文档被父临时授权更正，随后归还所有权）。编辑期 `gofmt -w` 只用于排版，不作为证据。 |

**明确尚未完成（不得声称 G2 DONE）**：结构化日志协议接缝（父 pending）、事件+任务跨事务原子入口（store 缺）、emit 已声明但下游解析不到的“失败任务”落库（store 形状不允许）；见 §4。

---

## 2. 源码 changed files（code commit `e552bd3`）

新增（owned 运行期）：

```
internal/route/params.go        internal/route/route.go         internal/route/emit.go
internal/route/params_test.go   internal/route/fixture_test.go  internal/route/plan_test.go   internal/route/emit_test.go
internal/source/source.go       internal/source/fixture_test.go internal/source/source_test.go
internal/trigger/trigger.go     internal/trigger/trigger_test.go
internal/lease/lease.go         internal/lease/lease_test.go
internal/ws/hub.go              internal/ws/session.go          internal/ws/handlers.go
internal/ws/fixture_test.go     internal/ws/ws_test.go
g2-resume-evidence/G2-WRITER-INTERFACES.md   （本文与 TEST-MAPPING G2 行见紧随的文档 commit）
```

删除：`internal/{route,ws,lease,source,trigger,executor}/**` 全部 v3 文件（含旧测试）、
`cmd/agent/**` 全部文件（含 314 个旧测试）。`internal/logging/**` 未改。

---

## 3. 新增测试（57 个，全部真实 store/httptest/共享 protocol）

- `internal/route`（23）：`TestPrepareParamsPermissiveSchema`、`TestPrepareParamsDefaultsRequiredAndExtra`、
  `TestPrepareParamsMissingRequired`、`TestPrepareParamsTypeMismatch`、`TestPrepareParamsBadSchema`、
  `TestValidatePayloadSchema`、`TestDeclaresTarget`、`TestDecodePayloadRejectsNonObject`、
  `TestPlanFromSourceMapsTemplatesAndKeepsOrder`、`TestPlanFromSourceNoTargets`、
  `TestPlanFromSourceMissingCapability`、`TestPlanFromSourceParamMissing`、
  `TestPlanTasksArePendingWithoutAuthority`、`TestPlanFromEmitSameNameAndNotDeclared`、
  `TestEmitCreatesPinnedChildTask`、`TestEmitWithoutIDAlwaysCreatesNew`、`TestEmitSameIDReplaysAndConflicts`、
  `TestEmitUndeclaredTargetRejected`、`TestEmitMissingTargetRejected`、`TestEmitPruneReceiptAndTrace`、
  `TestEmitAuthorizesBeforeReplay`、`TestEmitOnFinishedTaskIsNotRunning`、`TestEmitUnknownTaskIsTokenInvalid`。
- `internal/ws`（15）：`TestDispatchFrameCarriesNoExecutionAuthority`、`TestAcceptGrantsSameSlotAndResultAcks`、
  `TestAcceptRejectsInvalidSlotAndIdentity`、`TestRotatedCredentialGrantsNothing`、`TestNamespaceRevocationGrantsNothing`、
  `TestRenewAtExpiryIsRejected`、`TestCancelAckMatrix`、`TestRehelloUnknownBusyDoesNotRelease`、
  `TestDisconnectKeepsBusyUntilExplicitFact`、`TestEmitRoundTripAndErrors`、
  `TestLogsRequestCorrelatesHistoryAndEmptyPrevious`、`TestDeployNotifiesOnlineAndReplaysOnReconnect`、
  `TestConfigAppliedAndAgentFactsPersist`、`TestUpgradeFrameAndAck`、`TestUnknownFrameKeepsConnectionAndBadFrameCloses`。
- `internal/source`（11）：`TestIngestManualUnifiedReceiptAndTask`、`TestIngestNoTargetPruneAndEvent`、
  `TestIngestParamPruneIsPerTarget`、`TestIngestPayloadSchemaPrune`、`TestIngestDuplicateDeliveryReturnsOriginal`、
  `TestIngestManualPayloadOverridesConfigOneLevel`、`TestPreviewWritesNothing`、`TestHookDisabledIsNotFound`、
  `TestIngestRejectsNonObjectPayload`、`TestFireDisabledSourceRejected`、`TestIngestDeliveryIDScopeAndAbsence`。
- `internal/lease`（5）：`TestRunOnceRecoversExpiredNonIdempotentLease`、`TestRunOnceRequeuesExpiredIdempotentLease`、
  `TestRunOnceRetentionHonorsTTLAndKeepsActive`、`TestRetentionZeroTTLIsPermanent`、`TestOccupancyByNodeReadsRealLedger`。
- `internal/trigger`（3）：`TestRunDueFiresCronAndAdvances`、`TestRunDueNeverFireExpressionAdvancesWithoutHotLoop`、
  `TestRunDueErrorsAreNotSwallowed`。
- `internal/logging`（10）：原测试保留（文件未改）。

旧→新/退役逐名映射：`.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md` 的
「G2 runtime 逐名迁移映射」节（336 个运行期旧名逐行 + `cmd/agent` 314 名整包退役名单与理由码）。

---

## 4. 精确 pending / 契约缺口（不伪造实现）

### 4.1 结构化日志（受保护协议接缝，父 pending）
- 事实：`internal/protocol/ops_frames.go:123-137` 的 `LogsResponseFrame` 只有
  `stdout/stderr/truncated/bytes/attempt`，没有 W6/A1 的逐条 `sequence/stream/text/at`。
- G2 已做：真实 history attempt → 该 attempt 的**历史 node**（`store.GetTaskAttempt`），
  显式 `attempt` 请求、`previous` 取真实上一 attempt；`requestId/taskId/attempt` 精确配对；
  合法响应原样转发；离线/超时/错误绑定是真实错误；`previous` 无历史 → `Empty=true`（attempt 0），
  仅此场景使用 0；`Structured` 恒 `false`。
- 不做：不补 `at`、不造跨流顺序、不重排/重编号、不落 `task_logs`、不按 server 时钟二次过滤、
  不把离线/缺档案/过期伪装成空成功。消费 Node-log 补口（`LOG-RECORDS-HANDOFF.md` R1–R8）
  需父在 G2 冻结边界后重新授权。

### 4.2 事件 + 任务的跨事务原子性（store 缺口）
- 现状签名：
  - `store.(*DB).InsertEvent(ctx, *Event, now, windowSec int64) (inserted bool, err error)` — `internal/store/event.go:83`（投递窗口 + 事件同一事务）
  - `store.(*DB).CreateTasks(ctx, []*Task) error` — `internal/store/task.go:254`（整批任务同一事务）
  - 不存在 `AcceptEventTasks` / 接受 `*sql.Tx` 的变体；store 对 G2 只读。
- G2 组合：`InsertEvent` → 剪枝留痕 → `CreateTasks`。`CreateTasks` 失败时事件已落库，
  调用方拿到错误；重投由 `(source, deliveryId)` 窗口吸收。**不伪造原子性、不返回假成功回执**。
- 需要：store owner 增补「窗口+事件+扇出任务」单事务入口（或提供 tx 变体）；G2 不越界写 store。

### 4.3 emit 已声明但下游解析不到（规范 vs 存储形状）
- `capability.md §6.2`：`downstream_missing` = 「名字当时解析不到 → 任务失败 + 事件」。
- `store.CreateTasks` 要求 `capability_id/capability/trace` 且只允许 `pending` 起始，
  无法为解析不到的能力落一条 failed 任务。
- G2 现状：记真实剪枝观察（`source='route'`、type=`downstream_missing`），回 `taskId:null + pruned`；
  不捏造 failed task。需父裁决是补 store 形状还是改规范表述。

### 4.4 日志 HTTP 五态
- 由 G3 翻译；G2 提供 `ErrNotFound/ErrNoAttempt → 404`、`ErrNodeOffline → 503`、
  `ErrLogsTimeout → 504`、`ErrLogsCorrelation → 502`、帧 `no_local_archive → 410`。
  400 由 G3 参数校验产生。映射表见 `G2-WRITER-INTERFACES.md §5.3`。

---

## 5. 验证（本树最终四条只读门禁；日志为唯一完整文件）

环境：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`；
cwd/root/GOMOD 均为
`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery/.pi-subagents/runs/r-01be4aa7/worktrees/core-g2-resume`。

Gate 1（**只读**格式门禁；父纠错后的唯一形式）— log `/tmp/anotify-core-recovery-g2-final-gate1-format.log`：

```text
CMD: test -z "$(gofmt -l internal/route internal/ws internal/lease internal/source internal/trigger internal/logging)" && git diff --check
EXIT:0
```

> 说明：编辑阶段用过 `gofmt -w` 做排版；它**不是**验收。原 `gofmt -w … && git diff --check`
> 证书（历史 gate1 日志 `/tmp/anotify-core-recovery-g2-gate.log`）**无效**，本文件不引用它作为证据。

Gate 2（vet）— log `/tmp/anotify-core-recovery-g2-final-gate2-vet.log`：

```text
CMD: go vet route/ws/lease/source/trigger/logging
EXIT:0
```

Gate 3（六个运行期包测试）— log `/tmp/anotify-core-recovery-g2-final-gate3-runtime-test.log`：

```text
CMD: go test route/ws/lease/source/trigger/logging -count=1
ok  	github.com/zzjcool/anotify/internal/route	0.382s
ok  	github.com/zzjcool/anotify/internal/ws	1.658s
ok  	github.com/zzjcool/anotify/internal/lease	0.169s
ok  	github.com/zzjcool/anotify/internal/source	0.283s
ok  	github.com/zzjcool/anotify/internal/trigger	0.093s
ok  	github.com/zzjcool/anotify/internal/logging	0.003s
EXIT:0
```

Gate 4（store/artifact/protocol 只读回归）— log `/tmp/anotify-core-recovery-g2-final-gate4-store-artifact-protocol-test.log`：

```text
CMD: go test ./internal/store/... ./internal/artifact/... ./internal/protocol/... -count=1
ok  	github.com/zzjcool/anotify/internal/store	10.922s
ok  	github.com/zzjcool/anotify/internal/store/v4test	45.919s
ok  	github.com/zzjcool/anotify/internal/artifact	13.770s
ok  	github.com/zzjcool/anotify/internal/protocol	0.220s
EXIT:0
```

相同根因失败重试：0 次（无第三次硬扛）。

---

## 6. 产品 bug / 观察（未改断言迁就）

1. `store` 无事件+任务跨事务入口（§4.2，精确签名/行号如上）。
2. `store.CreateTasks` 不允许非 `pending` 起始，使 `downstream_missing` 的“失败任务”语义无法落库（§4.3）。
3. 共享 `LogsResponseFrame` 无结构化记录字段（§4.1，W6/A1 受保护 gap）。
4. 全仓编译现状（预期，超出 G2）：`internal/server`、`internal/api`、`cmd/devseed` 仍是 v3 调用方，
   依赖已删除/改签名的 v3 运行期包；G3/G4 负责。G2 **未**改动这些文件，也**未**跑全仓 `go build ./...` 冒充绿。

---

## 7. 不声称 / 下一步

- 不声称 T1/v4 DONE、不声称 G3/G4、不运行/代替最终 `go test ./...`、`make contract`、`make e2e-quick`。
- 未 push / 未开 MR / 未 merge（父后续统一集成）；未改 settings/.env/.git/node_modules/用户数据；未删旧 WT/证据。
- 父下一步：fresh 三角 review（严格 `ok:true`）+ supervisor 四条只读门禁；然后
  ① 裁决 §4.2/§4.3 是否补 store；② Node-log 补口落地后授权 G2 消费结构化日志；③ 再授权 G3。
