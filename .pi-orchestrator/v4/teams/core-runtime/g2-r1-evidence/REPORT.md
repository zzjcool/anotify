# G2-R1 · runtime 返修交付报告

- 分支：`pi-subagent/core-g2-resume-52fc54d4`
- 入场 HEAD：`80eb523c9dd79819244a2320a7dff90c31ae2a30`（clean）
- **runtime code commit**：`9a2b95c218cc780a4aed74862bea161a8d634d93`
  `fix(v4-runtime): G2-R1 配置版本同源/稳定提案/原子回执/受权部署/有界日志`
  （12 files changed, +1905/−268；含 `internal/ws/g2r1_test.go` 790 行新用例）
- 本节点：G2 唯一 runtime source writer，`worktree:false`，未派嵌套 agent，未 push/MR/merge/rebase/reset/squash。
- **不声称** G2/G3/G4/T1/v4 完成；未跑全仓 `go test ./...`、`make contract`、`make e2e`、`make fe`。

## 1. 按 R1-C1..C7 做了什么

| case | 实施 | 文件 |
|---|---|---|
| **C1** dispatch `configRevision` | 改为 `GetNodeConfig(node).Revision`（与 welcome 同真实源），按节点缓存于单轮 sweep；不再读 `nodes.resource_version`。测试让两个值真实不同（config rev 2 / node RV 1），断言 welcome==dispatch 且 != RV，并继续 accept→grant（真实 shared protocol 执行链，不出 stale_config 空转） | `internal/ws/hub.go` |
| **C2** 派发候选/拒绝授权 | `DispatchOnce` 改走 `store.ProposeTaskAssignment`：未到期提案原样复用一个 assignment/proposedAttempt/expiresAt（多 sweep 不 churn），到期才换绑定；不写 lease/attempt/token/slot。`task.reject` 改走 `store.RejectAssignmentAuthorized(sess.auth)`（当前 token hash/ns/Node/assignment/proposed/未过期同事务），错/旧/过期/撤 ns/轮换 token 0 写；轮换 token 断会话 | `internal/ws/hub.go`、`handlers.go` |
| **C3** Source/Emit 原子 receipt | `source.Ingest` 改为**单次** `store.AcceptSourceEvent`：源 RV CAS + nsVarsRevision pin + 投递窗 + 事件 + 正常任务 + failed Task/错误事件 + 纯剪枝 + 完整 receipt 同一事务；重复投递返回第一次 receipt 字节。`route.Emit` 删除提前 `recordPrune`，把 `PlannedPrunes/PlannedFailures` 交给 `EmitAuthorized` 同事务（授权先于 replay/facts）；剪枝写失败不再被吞成 200。`StateMissing` 变为 failed Task（attempt 0、真实请求 ref/声明 version、无 digest）+ 错误事件，不再是剪枝；source 回执 `tasks` 同时列正常与 failed 任务，`pruned` 只含纯剪枝；preview 每 target 自带 Reason/Detail（不再 `Pruned[0]` 兜底） | `internal/source/source.go`、`internal/route/{route,emit}.go` |
| **C4** 部署授权 | `hub.Deploy` 依赖 `store.CreateDeployments` 的同事务全 targets ns 授权（任一目标未授权 → 0 写、0 帧）。`capability.deployed` 改走 `store.MarkDeployResultAuthorized`（当前 credential/ns/intentNode/requestId 派生/capability UID/name/version/digest/未 superseded 同一事务）；删除 `AuthorizedNamespaces`+`GetDeploymentByNodeCapability` precheck+裸 `MarkDeployResult` | `internal/ws/handlers.go`、`hub.go`（Deploy 未改签名） |
| **C5** 日志 waiter/回执校验 | waiter `ch` cap 1 + 交付前从 map 移除 + 非阻塞 send：每请求最多一次，重复/未登记/晚到帧不再阻塞读循环；`logs_response` 严格配对（node/requestId/taskId/attempt）+ 成功必须有非 null `lines`、sequence≥1 严格递增、stream 闭集、at RFC3339Nano；错误码闭集、错误帧不得带 lines、错误帧声称别的 attempt → `ErrLogsCorrelation`（不能因 `error != ""` 跳过 claimed attempt）；断开唤醒 `ErrNodeOffline`（不再等超时）；`tailLines/sinceSeconds/attempt` 原样转发；`detach` 的 offline/instances 写有 5s deadline，被顶掉的旧会话不改节点事实/不误唤醒 | `internal/ws/hub.go` |
| **C6** 声明 ref 语义 | 新增 `declaredRef(next,to)`：裸 to 必须同名声明**唯一**命中（裸名→latest；唯一 `name@v`→该 v）；显式 `to=name@v` 只匹配精确声明（裸声明不再放行任意版本）；同名多钉版/裸+钉混写歧义 → `ErrNotDeclared`（不新增码）；全局 `ResolveRef` 未改。子任务按当前声明钉 version/digest；父 next 同 version 改配置后新 emit 走新声明、旧子任务不变 | `internal/route/route.go` |
| **C7** 握手/模板/mapping | 新增真判别握手负例（坏 token、名字不符、缺 token、pre-hello 上行/下行/坏 JSON → 断开且不注册 online）；模板 runtime 用例走真实 `sourcespec.RenderParams`（单趟不递归、整值保类型、混写拼串）；TEST-MAPPING 有限行按实际能辨 case 修正（见 §3），`R:V3_CENTRAL` 注明 `notify.phone` 刻意退役、独立域不退役 | `internal/ws/g2r1_test.go`、`internal/route/plan_test.go`、TEST-MAPPING |

## 2. 新增/改写测试（25 个新用例 + 2 处旧断言按新语义加强）

**`internal/route`（8 新/重写 + 2 加强）**

- `TestPlanFromSourceMissingKeepsDeclaredVersion`（新）
- `TestPlanFromEmitResolvesDeclaredRefs`（新，C6 全矩阵：唯一裸 to/显式 @v/多钉歧义/裸声明→latest/改 next）
- `TestPlanFromSourceTemplatesSinglePassAndKeepTypes`（新，C7）
- `TestEmitPinnedDeclaredRefPinsChildAndTracksMutableNext`（新，真实子任务 version+digest）
- `TestEmitMissingDeclaredTargetCreatesFailedTaskNotPrune`（新，failed Task/attempt0/请求 ref/声明 version/事件）
- `TestEmitPruneReceiptEventIDIsPersistedFact`（新，eventId == 持久 emit 接受事实 id；同 id 回放同回执）
- `TestEmitRevokedCredentialWritesNoPruneFact`（新，授权先于剪枝写）
- `TestEmitPruneFailureIsNotSwallowed`（新，注入事件写失败 → 报错、0 task、0 emit 行）
- `TestDeclaredRef`（替换旧 `TestDeclaresTarget`）
- 加强：`TestPlanFromSourceMissingCapability`（missing 不进 pruned + FailureInputs）、`TestPlanFromSourceParamMissing`（target 自身 Reason）

**`internal/source`（4 新 + 1 加强）**

- `TestIngestMissingTargetCreatesFailedTaskNotPrune`（新，回执 tasks 含 failed、真实 ref/version、事件带 taskId）
- `TestIngestDuplicateReturnsFirstReceiptWithFailuresAndPrunes`（新，duplicate 同一份完整回执、0 新写）
- `TestIngestSourceRevisionMismatchWritesNothing`（新，源 RV 失配明确报错 0 写）
- `TestPreviewPerTargetReasons`（新，param_missing 与 param_type 各自 Reason）
- 加强：`TestIngestDuplicateDeliveryReturnsOriginal`（同一批 task id + 行数不变，替代“重复回执必须为空”）

**`internal/ws`（13 新 + 1 加强）**

- `TestDispatchConfigRevisionMatchesWelcome`、`TestDispatchConfigRevisionZeroWithoutConfigRow`（C1）
- `TestDispatchProposalStableUntilExpiry`、`TestRejectAssignmentRejectsForeignAndStaleRequests`、
  `TestRejectAssignmentAuthorizedSuccessAndCredentialMatrix`、`TestAcceptSlotPresence`（C2）
- `TestDeployRejectsUnauthorizedExplicitTargetAtomically`、`TestDeployedReceiptAuthorizedNegatives`、
  `TestDeployedReceiptRoundTripIdempotentAndSuperseded`（C4）
- `TestLogsWaiterBoundedCorrelationAndWake`（C5：tail/since、坏 lines、错 attempt 错误帧、重复/未登记不阻塞、noarchive、timeout、disconnect）
- `TestHandshakeRejectsBadTokenAndName`、`TestHandshakeRejectsPreHelloFrames`（C7）
- `TestDirectDispatchFrameHasNoNodeResourceVersion`（结构断言）
- 加强：`TestLogsRequestCorrelatesHistoryAndEmptyPrevious`（真实 `lines` + Structured + tail/since 转发断言）

全部用真实 httptest WebSocket + 共享 `protocol.Encode/Decode` + 真实 store/临时库；无 mock 会话、无手写 wire、无 DTO-only 断言。

## 3. 有限 TEST-MAPPING 修正（旧名 → 实际能辨用例）

| 旧测试 | R1 前（假映射） | R1 后 |
|---|---|---|
| `TestUnauthorizedNoUpgrade` | `TestRotatedCredentialGrantsNothing`（post-hello 轮换，不是握手） | `TestHandshakeRejectsBadTokenAndName` |
| `TestHelloWrongNodeID` | `TestAcceptRejectsInvalidSlotAndIdentity` | `TestHandshakeRejectsBadTokenAndName` |
| `TestFramesBeforeHelloRejected` | `TestRotatedCredentialGrantsNothing` | `TestHandshakeRejectsPreHelloFrames` |
| `TestFetchLogsNoLocalArchive` | `TestLogsRequestCorrelates...`（无判别维度） | `TestLogsWaiterBoundedCorrelationAndWake` |
| `TestFetchLogsTimeout` | 同上 | `TestLogsWaiterBoundedCorrelationAndWake` |
| `TestFetchLogsDropsUnpairedResponse` | 同上 | `TestLogsWaiterBoundedCorrelationAndWake` |
| `TestFetchLogsWakesOnDisconnect` | 同上 | `TestLogsWaiterBoundedCorrelationAndWake` |
| `TestApplyFrameUnknownDeploymentIgnored` | `TestDeployNotifiesOnlineAndReplaysOnReconnect` | `TestDeployedReceiptAuthorizedNegatives` |
| `TestPublishDeployCrossNamespaceRejected` | 同上 | `TestDeployRejectsUnauthorizedExplicitTargetAtomically` |
| `TestDispatchRecursiveDoesNotResolve` | `TestPlanFromSourceMapsTemplatesAndKeepsOrder` | `TestPlanFromSourceTemplatesSinglePassAndKeepTypes` |
| `TestResolveParamsNoRecursion` | 同上 | 同上 |
| `TestResolveParamsWholeRefKeepsType` | 同上 | 同上 |
| `TestDispatchPruneRecordFailureDoesNotBreakDispatch` | `R:V4_POLICY` | 保留 `R:V4_POLICY` 并注明 v4 同事务语义（对应 `TestEmitPruneFailureIsNotSwallowed`+store S1 回滚用例） |
| `R:V3_CENTRAL` 说明 | 只写“中枢执行器退役” | 明确 `notify.phone` 中央 Task 成功/失败/缺 to 专属断言**刻意退役**、不恢复；独立 POST notify/device/broker/push/auth 域由现有测试与后 G3/G4 守 |

未重扫 650 个旧名；G1/C3/Node 测试未改。

## 4. 验证（四条只读门禁，命令与真实 exit 原样）

环境：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`；cwd 为本 worktree。

**Gate 1（格式只读）** — log `/tmp/anotify-g2r1-gate1-format.log`

```text
CMD: test -z "$(gofmt -l internal/route internal/ws internal/lease internal/source internal/trigger internal/logging)" && git diff --check
EXIT:0
```

**Gate 2（六包 vet）** — log `/tmp/anotify-g2r1-gate2-vet.log`

```text
CMD: go vet ./internal/route/... ./internal/ws/... ./internal/lease/... ./internal/source/... ./internal/trigger/... ./internal/logging/...
EXIT:0
```

**Gate 3（六包真实测试）** — log `/tmp/anotify-g2r1-gate3-runtime-test.log`

```text
CMD: go test ./internal/route/... ./internal/ws/... ./internal/lease/... ./internal/source/... ./internal/trigger/... ./internal/logging/... -count=1
ok  	github.com/zzjcool/anotify/internal/route	0.653s
ok  	github.com/zzjcool/anotify/internal/ws	4.072s
ok  	github.com/zzjcool/anotify/internal/lease	0.193s
ok  	github.com/zzjcool/anotify/internal/source	0.436s
ok  	github.com/zzjcool/anotify/internal/trigger	0.115s
ok  	github.com/zzjcool/anotify/internal/logging	0.026s
EXIT:0
```

**Gate 4（store/artifact/protocol 只读回归）** — log `/tmp/anotify-g2r1-gate4-store-artifact-protocol-test.log`

```text
CMD: go test ./internal/store/... ./internal/artifact/... ./internal/protocol/... -count=1
ok  	github.com/zzjcool/anotify/internal/store	5.603s
ok  	github.com/zzjcool/anotify/internal/store/v4test	30.581s
ok  	github.com/zzjcool/anotify/internal/artifact	2.545s
ok  	github.com/zzjcool/anotify/internal/protocol	0.063s
EXIT:0
```

相同根因失败重试：0 次（无第三次硬扛）。

## 5. MR

无。按任务卡：owner-only 本地提交，不 push / 不开 MR / 不 merge；父统一集成。

## 6. 未完成 / 需要父裁决的接缝

1. **emit `downstream_missing` 的 failed-task 公开投影（窄接缝）**：`store.EmitFailureFact` 不返回它同事务创建的 failed task id，`protocol.EmitResponse`/`TaskEmitAckFrame` 也没有 tasks 字段。当前行为：真实 failed Task + 错误事件落库（trace/`ListTasksByTrace` 可查）、回执 `pruned=nil`、`to`/`version` 为真实请求 ref/声明 version、`taskId=null`、`eventId` 为真实持久 emit 接受事实 id；**不伪造 taskId、不当剪枝**。若父要求 emit ack 也带 failed task id，需要扩 store `EmitFailureFact` 或 emit wire 冻结契约。
2. **G3/G4 未做**：本卡只覆盖 runtime 六包；server/api/Node 接线、`make contract`、`make fe`、`make e2e`、全仓 `go test ./...` 仍按父计划由 G3/G4/最终集成门禁负责。
3. `internal/ws.RequestLogs` 签名新增 `tailLines/sinceSeconds`（G3 尚未接线；字段是 shared protocol 既有字段）。
4. `internal/logging` 未改（独立 logger，原测试原样通过）。
