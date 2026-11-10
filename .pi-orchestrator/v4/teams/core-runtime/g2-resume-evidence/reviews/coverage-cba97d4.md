# G2 独立只读复核 · 测试覆盖 / 旧名→新名映射对抗审查

**基线**：cwd 即 core-g2-resume worktree，HEAD `cba97d4f9ae5d4bffa3d9e3f12ef7280a25d8797`；只读比较 `5441426..cba97d4` 的 G2 运行期包（route/ws/lease/source/trigger/logging/executor/cmd-agent）与 `TEST-MAPPING.md` G2 节、`g2-resume-evidence/*`。未跑基线/build/test，未派子 agent，未改任何文件。

## 1. 计数与完整性（独立核实）

| 项 | 声明 | 独立核实 | 方法/结果 |
|---|---|---|---|
| 运行期旧测试名 | 336 | **336** | `git grep '^func Test' 5441426 -- internal/{route,ws,lease,source,trigger,executor,logging}/*_test.go`；分项 141+110+30+18+18+9+10 |
| cmd/agent 旧测试名 | 314 | **314** | 同法逐名，无缺无多 |
| 新增测试 | 58 | **58**（route23/ws16/lease5/source11/trigger3） | 当前树 `func Test` 计数；logging 10 个原样保留，文件未改 |
| 映射表覆盖 | 逐名 336 行 | **无缺/无重/无多** | `comm` 双向对比，G2 节各包行数与 141/110/30/18/18/9/10 全等 |
| 映射目标测试名 | all new | **全部存在** | 表内目标名与当前树测试名集差集为空 |

结论：**没有统计虚报，也没有整文件 deprecated 式清空**（executor/cmd-agent 是显式授权删除）。ws 新测试走真实 `httptest`+`protocol.Decode`（`internal/ws/fixture_test.go:159`、`:220`），route/source/lease/trigger 全部真实 store + 临时库；未发现 DTO/compile-only 替代。

## 2. Findings（按严重度，只列旧名安全意图 → 当前可区分证据）

**F1 [major] 握手认证三类否定用例被映射到一个无关测试，v4 没有可区分用例**
- `TEST-MAPPING.md:1460` `TestUnauthorizedNoUpgrade → TestRotatedCredentialGrantsNothing`；`:1463` `TestHelloWrongNodeID → TestAcceptRejectsInvalidSlotAndIdentity`；`:1470` `TestFramesBeforeHelloRejected → TestRotatedCredentialGrantsNothing`。
- 证据：`internal/ws/ws_test.go:151` 该测试先 `e.dial` 成功后才轮换；`internal/ws/fixture_test.go:159` 拨号只送合法 token；`internal/ws/ws_test.go:119` 只测 accept 帧的 node 名，不测 hello 名与 token 不一致。
- 旧测试实际钉的是：无 token/坏 token 不得升级（`node_test.go:393` 原文断言非 101/无 Upgrade 头）、hello 名不符拒绝（`:530`）、hello 之前发帧拒绝（`:790`）。v4 实现有对应拒绝（`internal/ws/hub.go:236` 首帧必须 hello、`:255-268` token/name 绑定），但 **G2 无任何坏 token/h 名不符/裸帧测试**，这三行映射为假。

**F2 [major] 日志四旧名统一映射到缺多个判别维度的测试；且存在错 attempt 错误帧被当结果交付**
- `TEST-MAPPING.md:1442/1445/1446/1447`（NoLocalArchive/Timeout/DropsUnpaired/WakesOnDisconnect）全部指向 `TestLogsRequestCorrelatesHistoryAndEmptyPrevious`。
- `internal/ws/ws_test.go:445` 该测试只覆盖：previous 真空、错 taskId（`Error==""`）→ `ErrLogsCorrelation`（`:485`）、正确回包、离线、无 attempt、跨 ns。**没有** timeout、`no_local_archive` 转发、断连唤醒、未登记 requestId 丢弃。
- 产品/覆盖证据：`internal/ws/hub.go:706-711` 等待只 select ctx/timer/waiter，断连中等待只能得到 `ErrLogsTimeout`，旧 `WakeOnDisconnect` 语义静默消失；`hub.go:736-741` 对 `frame.Error != ""` 跳过 attempt 校验 → **错 attempt 的 error 帧会被当作所请求 attempt 的结果交付**，正是“wrong request attempt 相关性”漏洞，而 `:485` 的断言只覆盖 taskId 维度、且只测 `Error==""`。接口文档 `G2-WRITER-INTERFACES.md` §5.3 的 410 原样转发无任何测试。

**F3 [major] emit 剪枝留痕写在授权之前，且留痕失败被静默吞掉——与映射依据 R:V4_POLICY 相反**
- `TEST-MAPPING.md:1376` `TestDispatchPruneRecordFailureDoesNotBreakDispatch → R:V4_POLICY`（理由=v4 要求留痕失败必须报错）。
- 代码证据：`internal/route/emit.go:114` 在 `EmitAuthorized`（`:140`）**之前**调用 `recordPrune` 写 events 行；且 `perr == nil` 才回填 eventId，失败继续 200 回执、无 error。失效/轮换凭证可以造成本 ns 的剪枝事件写入（先写后授权），失败留痕则被吞。
- 测试证据：`internal/route/emit_test.go:86` `TestEmitPruneReceiptAndTrace` 只断言合法授权下恰好 1 行、eventId 非空；无“失效授权 + 剪枝=0 写”“留痕失败必须报错”任何反例。冻结卡“先授权后回放/业务”与 source 侧行为（`internal/source/source.go` Ingest 对 recordPrune 失败返回错误）不一致。

**F4 [major] 部署回执的授权/匹配否定用例缺失，两行映射为假**
- `TEST-MAPPING.md:1392` `TestApplyFrameUnknownDeploymentIgnored`、`:1411` `TestPublishDeployCrossNamespaceRejected` → `TestDeployNotifiesOnlineAndReplaysOnReconnect`。
- `internal/ws/ws_test.go:534` 只覆盖在线通知+正确回执落 applied+离线 pending 重连重放；**没有**未请求部署回执、错 requestId/digest、未授权 ns 回执的用例。
- 实现确有待测守卫：`internal/ws/handlers.go:209/225/241`（ns 授权、capability、`digest/requestId` 四元组匹配，不匹配只 warn 返回）。旧名钉的正是“外来/未知部署事实不得落库”，现在 G2 无 discriminator；store 侧无法覆盖该 ws 帧校验。

**F5 [major] 参数模板“单趟不递归 / 整值引用保类型”映射到只测字符串直取的测试**
- `TEST-MAPPING.md:1291` `TestDispatchRecursiveDoesNotResolve`、`:1343` `TestResolveParamsNoRecursion`、`:1344` `TestResolveParamsWholeRefKeepsType` → `TestPlanFromSourceMapsTemplatesAndKeepsOrder`。
- `internal/route/plan_test.go:16-96` 只用 `"$(payload.env)"`（字符串）与 default；不测 payload 自带 `$(payload.x)` 文本（注入/二次展开）、数字/bool/object 整值引用、混写字符串。
- 实现为单趟求值+整值保类型（`internal/sourcespec/template.go:256-289` 读证据），但 **G2 无任何测试钉住**；旧测试的明确断言（原 `route/params_test.go:89/112`）被标为已迁移，属假映射。

**F6 [major] 通知（notify.phone）旧断言随 executor 整包退役，push 侧无可替代测试**
- `TEST-MAPPING.md:1585/1586/1588/1589/1591`（`TestTickNotifyPhoneSucceeds`、`TestTickMarksFailedOnSendError`、`ZeroDevices`、`MissingTo`、`FinishUsesTaskNamespacePerTask`）→ `R:V3_CENTRAL`。
- 但 D6 要求保留独立 notify：`internal/push/central.go:22` 仍有 `NotifyUser`，`internal/server/mux.go:365` 仍构造 `push.CentralNotifier`；`internal/push` 现有测试只有 `filter_test.go:25-113`（过滤规则），**没有**推送成功/失败终态、零设备、缺 to、namespace 取自任务行的断言。删除行为本身被 G2 卡授权，但“通知语义”没有任何替代测试/明确 pending 归属，须父裁决（G4 补测或明确退役）。

**F7 [minor] 两行 emit 映射指向未覆盖该场景的新测试（覆盖实际在 store，应改 R:V4_STORE）**
- `TEST-MAPPING.md:1305` `TestEmitRejectsForeignNode → TestEmitRoundTripAndErrors`：`internal/ws/ws_test.go:377` 全程单节点，无 foreign node；该不变量由只读证据 `internal/store/v4test/emit_test.go:229`（shadow node）覆盖。`:1318` `TestEmitEmissionAndTasksSameTransaction → TestEmitSameIDReplaysAndConflicts`：`internal/route/emit_test.go:100` 无回滚用例；回滚由 `internal/store/v4test/emit_test.go:472` 覆盖。映射结论方向存在，但“指向 G2 新测试”不成立。

**F8 [minor] renew 所有权映射假；连接关闭行为变化未标注**
- `TEST-MAPPING.md:1434` `TestHeartbeatRenewsOnlyOwnedTasks → TestRenewAtExpiryIsRejected`：`internal/ws/ws_test.go:200` 只测 expiry==now；错 lease/attempt 仅 store 证据 `internal/store/task_test.go:381`，应标 R:V4_STORE。`:1429` `TestHeartbeatLeaseFailureClosesConnection` 同理：v4 续租失败只不发 ack（`handlers.go` handleRenew），不断连，映射未体现策略变化。

**F9 [minor] HelloInstances 映射假（覆盖在 store）**
- `TEST-MAPPING.md:1464` `TestHelloInstances → TestRehelloUnknownBusyDoesNotRelease`；`internal/ws/ws_test.go:302` 不测超 max_instances；实际守卫 `internal/store/node.go:643` 与被映射包外的 `internal/store/node_test.go:377`。

**F10 [minor] lease 映射过度声明**：`TEST-MAPPING.md:1515` 按 ns 保留、`:1516/1517` 审计字段、`:1518/1524` stats 首 tick/无节点、`:1529` 退避序列 → `internal/lease/lease_test.go:169/262` 等只覆盖单 ns TTL、零 TTL、占用投影；per-ns、audit 字段、StatsRefreshed、1/2/4 退避序列均无断言。

**F11 [minor] source/trigger 映射过度声明**
- `TEST-MAPPING.md:1546/1548` cron 级 config.payload 打底/用户字段覆盖 → `internal/source/source_test.go:13/161`（都是 manual kind，cron 基线未测）。
- `:1556` `TestDryRunAppliesResultSchema → TestPreviewWritesNothing`：`source_test.go:196` 的源无 `payloadSchema`，预览阶段的 event_schema 剪枝未测。
- `:1575` `TestDispatchFailureKeepsEvent → R:V4_GAP` 诚实，但“CreateTasks 失败后事件仍在”无测试；`internal/trigger/trigger_test.go:131` 只测关库导致 sweep 错误，VERDICT §1 声称的“Ingest 失败也推进 next_fire_at”无判别用例。

**F12 [minor] dispatch 错误传播 / 超大 emit 可诊断性映射不成立**
- `TEST-MAPPING.md:1280` `TestDispatchStoreErrorsPropagate → R:V4_STORE`：`internal/ws/hub.go` DispatchOnce 对 AssignTaskAt/候选错误 log+continue、返回 nil；store 测试无法覆盖循环策略，映射依据错位（且无测试）。
- `:1433` `TestEmitOversizedParamsIsDiagnosable → R:V4_STORE`：store 只覆盖拒绝；v4 ws 对 generic 错误不回 ack（`internal/ws/handlers.go:294`），旧“可诊断 ack”行为静默变成超时/未知，未列入 pending。

**F13 [minor] 剪枝 Target.reason 丢失 + 预览用首条 prune 回填**
- `internal/route/route.go:218-225` `prunedTarget` 对已解析能力把 `Reason` 置空；`internal/source/source.go:272` 预览对任意 pruned target 用 `plan.Pruned[0].Reason` 回填——多条不同原因时会张冠李戴。`internal/route/plan_test.go:60` 等只断 State/Reason 在计划层，未断言 target.reason；`source_test.go:79` 也未覆盖多原因预览。

## 3. 待核（未读/未验证，不当作结论）

- `task.reject`（`internal/ws/handlers.go:76`）无任何 ws 测试；stale_config/slot_unavailable 的 ws 拒绝路径仅靠 store 证据，存储侧具体测试未逐行核。
- “同一 binding 重复 accept 幂等”在 G2 无测试；store 是否覆盖未核实。
- `InsertEvent` 投递窗口“窗口外同 id 算新事件”未在 G2 测试出现，store 侧窗口测试未核。
- R:V4_G3 的两处（user-scoped foreign-user 系 `TEST-MAPPING.md:1296/1380-1382`）与 v4 无用户作用域文档的矛盾未裁决；R:V4_G3 的 `TestPublishUpgradeCrossNamespaceRejected` 亦存疑。
- executor 内 notify 语义的正式归属（G4/G3）需父裁决；本报告只按 D6“保留 notify”指出无替代测试。

## 4. 结论

计数与列名完整、新测试质量整体真实（httptest+共享 Decode+真实 store），但 **6 处 major** 属于“旧名安全意图 → 当前无 discriminator 的假迁移/假覆盖”，其中 F3 是代码层写序/吞错问题（非仅测试缺口），F2 含真实日志相关性缺陷。按 G2 冻结验收口径不能给 ok:true；与 implementer SAFE_BLOCKED 一致，此证据供父裁决/rework。

```json
{"ok": false, "reason": "G2 test-mapping independent recon: counts verified (336 runtime old names / 314 cmd/agent / 58 new + 10 logging retained, mapping complete), but 6 major false migrations/uncovered security invariants: auth handshake negatives (TEST-MAPPING.md:1460,1463,1470), logs timeout/no_local_archive/disconnect/wrong-attempt-error correlation (1442-1447 + hub.go:736-741), emit prune write before EmitAuthorized and swallowed prune-write error vs R:V4_POLICY (1376, route/emit.go:114), deploy receipt auth negatives (1392,1411), template non-recursion/type preservation (1291,1343,1344), notify assertions retired without replacement (1585-1591). Not DONE."}
```
