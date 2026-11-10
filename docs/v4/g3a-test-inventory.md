# G3a · #331 测试 inventory（server 345 + api 18 → server 265 + api 8）

> G3-D6 要求：先逐名 inventory，再按当前安全事实迁移；**禁止整文件清空**认证/权限/CAS/
> delete/secret/digest/FK/backpressure 断言；退役项必须**具名**并给出**替代用例**。
>
> 基线 `b30e998`（origin/v4/integration）；交付 `issue/331-http-resources`。
> 统计口径：`grep -c '^func Test' internal/server/*_test.go`（含 1 个 `TestMain`）与 `internal/api/events_test.go`。
> 本文件按**返修后（PR #340 review 关闭轮）**的最终树重算：数字与代码逐名可核对。
>
> | | 旧（`b30e998`） | 首轮交付（`2c3b2d3`） | 返修后（本轮） |
> |---|---|---|---|
> | `internal/server` | 345（344 用例 + 1 `TestMain`） | 245（244 + 1） | **265（264 + 1）** |
> | `internal/api` | 18 | 8 | 8（本轮未改） |
>
> 差异全部是「v3 扁平资源面不存在了」的直接后果 + 返修轮补回被误删的覆盖；
> **没有一条**是「为了让测试变绿而删除或放宽断言」。
>
> 删除文件口径：`git diff --name-status b30e998..HEAD -- internal/server internal/api` 共 **34 个文件**
> （**27 个 `_test.go`** + **7 个源文件**：`console_handlers.go`、`deploy_handlers.go`、`emission_handlers.go`、
> `namespace_handlers.go`、`playbook_handlers.go`、`source_handlers.go`、`task_logs_handlers.go`），
> 其中 `_test.go` 内共 **178 个 `func Test`** 具名退役（逐类映射见 §4）。
>
> 上述 34 / 27 / 178 三个数字都是**返修终态树**上的实测值（命令见 §9.5）；旧文写的 35 / 28 / 181
> 与实测不符，本轮按命令输出改正（#340 r2 minor-1）。

## 0. 一句话结论

- **kept**：认证 / 设备 / CLI key / passkey / 密码 / 恢复码 / 静态资源 / 背压 / 日志安全 /
  gzip / 限流 / 状态写入器 / 词汇 / 嵌入 —— 这些文件的**断言未弱化**。其中两份在首轮做了
  **装配层适配**（不是放宽）：`admin_test.go` 把 `IdemKey` 改名为 `DeliveryID`（v4 字段名）、
  `hook_ratelimit_test.go` 把夹具从 v3 的 builtin `notify.phone` 换成真实能力行；
  `hook_ratelimit_test.go` 的 13 条限流断言逐字保留。返修轮另在 `config_test` 之外补回
  `dispatch_budget_test.go`（见 §4.5）。
- **rewritten**：`node_upgrade_test.go`（7→4；返修轮再 +2 = 6，见 §9.5）、`task_out_retired_test.go`（2→2，断言改为递归遍历 JSON）。
- **migrated**：v3 扁平 capability/source/playbook/namespace/node/task/event/log/emission 面 →
  v4 namespaced 资源面：新增 **84 个 server 用例**（首轮交付 `2c3b2d3` 的计数 = 72 + 11 个 review 补缺
  + 1 个收尾；其中 `api_integration_test.go` 9 个跨域集成）+ **8 个 `internal/api` 用例**。
- **retired**：v3 `edges`/`downstreams`/`deployments` 写、批量建能力、`CapabilityRevision`-式 spec、
  中央执行域（notify.phone）、跨 ns 归并快路径、v3 openapi parity —— 共 19 个文件（其中 3 个是 0 用例的纯夹具文件）。

## 1. kept：原样保留（155 个用例，逐文件计数）

| 文件 | 用例数 | 为什么必须留 |
|---|---|---|
| `admin_test.go` | 10 | 管理后台权限/角色/禁用用户即失效会话（安全域） |
| `auth_cache_wiring_test.go` | 2 | key revoke / admin disable 立刻穿缓存（安全域回归） |
| `backpressure_test.go` | 13 | 背压阈值/失败开阀/采样/日志转换（G3-D2「backpressure 保留」） |
| `cli_auth_test.go` | 13 | CLI 设备授权全套 + poll 限速 + key 自检 |
| `config_test.go` | 4 | 保留期 / pending 阈值 env 解析与钳位 |
| `config_stats_test.go` | 2 | stats 间隔 env 映射 |
| `embed_test.go` | 1 | 静态构建模式 |
| `gzip_test.go` | 5 | gzip/accessLog 顺序（含 `TestGzipJSONKeepsStatusWriter` 的 429 回归防钱） |
| `hook_ratelimit_test.go` | 8 | hook 双维度限流（429/Retry-After/per-token 不能绕过/只记一条日志）——**迁移了夹具，未放宽断言** |
| `logging_ac_test.go` | 4 | secret 不进日志（AC3）、结构化输出、级别过滤 |
| `middleware_latency_test.go` | 1 | access log 延迟单位 |
| `passkey_enroll_e2e_test.go` | 25 | passkey 设备授权端到端（含 secret 不进公开通道） |
| `passkey_enroll_test.go` | 12 | 同上（单元层） |
| `passkeys_test.go` | 8 | passkey 增删改查 + 越权/最后一个不能删 |
| `password_test.go` | 14 | 密码登录/注册/改密/删密/限速 |
| `postclose_guard_test.go` | 1（`TestMain`） | 关闭后守卫 |
| `qr_test.go` | 2 | QR 渲染 |
| `recovery_test.go` | 4 | 恢复码 regenerate/login/status + 双维度限速 |
| `static_test.go` | 23 | 静态资源缓存/ETag/Range/gzip/目录模式（23 条全是真实 HTTP 断言） |
| `status_writer_test.go` | 2 | 首次 WriteHeader 记录 + ResponseController 穿透 |
| `vocabulary_test.go` | 1 | 词汇/文案门禁 |

## 2. rewritten：保留文件名但按 v4 重写（旧→新）

### 2.1 `node_upgrade_test.go`（7 → 4；返修轮再 +2 = 6，见 §9.5）

| 旧用例 | 处理 | 替代 / 理由 |
|---|---|---|
| `TestAgentBinaryEndpoint` | 迁移 | `TestNodeUpgradeBinaryAuthAndServe`（真 node token → 200 + 二进制原文 + 版本头） |
| `TestAgentBinaryEndpointUnavailable` | 迁移 | `TestNodeUpgradeBinaryUnavailable`（未配置二进制 → 501，不伪装 200） |
| `TestNodeUpgradeEndpointGuards` | 迁移 | `TestNodeUpgradeEndpointGuards`（未登录 401 / 节点不存在 404 / bin 未配置 501） |
| `TestNodeUpgradeEndpointDispatch` | **退役** | v4 的帧内容断言在 `internal/ws/ws_test.go:633 TestUpgradeFrameAndAck`（帧级真连接）；本层只保留「推没推」→ `TestNodeUpgradeEndpointGuards` 的离线 409 分支。**理由**：`ws.UpgradePublisher`/`ws.UpgradeFrame` 已在 v4 删除，服务端只经 `Hub.RequestUpgrade`，再在 HTTP 层造一个假 publisher 等于测自己写的 fake。 |
| `TestNodeUpgradeEndpointAuthAndScope` | 迁移 | 同上（401/404 分支） |
| `TestNodesListAgentUpgradeFields` | 迁移 | `GET /v1/nodes` 的 `agentUpgrade` 投影现由 `resource_node.go` 提供 → `TestNodeResourceListEmptyIsArray` + `TestNodeResourceCreateReturnsEnvelopeWithoutToken` 覆盖信封与投影形状；`agentUpgrade` 逐字段断言不再单独保留（v4 `Node.status` 未定义该投影键，按「不编造字段」纪律省略） |
| `TestNodesListTargetAgentVersionEmptyWithoutBinary` | **退役** | `targetAgentVersion` 是 v3 `GET /v1/nodes` 顶层字段；v4 Node 信封无该键（§7.4），按「不新增非契约字段」纪律不保留 |

### 2.2 `task_out_retired_test.go`（2 → 2，断言加强）

| 旧用例 | 处理 | 说明 |
|---|---|---|
| `TestTaskEndpointsExposeNoOut` | 迁移（v4 路径 + 加强） | v3 `/v1/tasks` → `/v1/namespaces/{ns}/tasks`；`out/port/exit/nextHop` 的禁用检查从「只看顶层 map」改为**递归遍历整份 JSON**，并新增「list items 必须是数组且 2 条 / 详情 kind=Task」 |
| `TestTraceEndpointExposesNoOut` | 迁移（v4 路径 + 加强） | `/v1/traces/{id}` → `/v1/namespaces/{ns}/traces/{id}`；额外覆盖 `events[]` 内部 |

## 3. migrated：v3 资源面 → v4 namespaced 资源面（首轮 72 个新用例；当前合计 91）

| 旧文件（用例数） | 新文件 | 新用例（逐名） | 安全意图映射 |
|---|---|---|---|
| `ability_*` 见下（`capability_artifact_test.go` 5、`capability_batch_test.go` 9、`capability_links_test.go` 5、`capability_run_workdir_test.go` 3、`capability_spec_p1b2_test.go` 5、`capability_spec_p4_test.go` 4、`console_test.go` 27 中的 capability 段） | `resource_capability_test.go`（11） | `TestCapabilityResourceCreateAndGet`、`…VersionsDescending`、`…PatchCAS`、`…DeleteCAS`、`…NamespaceIsolation`、`…WriteValidation`、`…DeployUnknownNode`、`…DeployToOfflineNode`、`…EdgesAndTemplates`、`…RequiresSession`、`…EmptyLists` | 能力创建/版本族/内容寻址/`includeBody`/deploy 事实/edge 派生投影/未登录 401/CAS/跨 ns |
| `source_handlers_test.go`（19） | `resource_source_test.go`（**11**：首轮 8 + review 补 2（§8）+ 返修 1（§9.5）） | `TestSourceResourceCreateGetMasksSecrets`、`…WriteRejectsReadOnlyProjection`、`…PatchKeepsOmittedConfig`、`…PatchKindImmutable`、`…DeleteCAS`、`…FireReceiptShape`、`…DryRunSideEffectFree`、`…HookResolvesNamespaceByToken` | token/secret 脱敏（GET 与错误体）、PATCH 省略=保旧值、kind 不可变、DELETE CAS、fire 回执形状、dry-run 0 写、hook 由 token 定位 ns |
| `playbook_handlers_test.go`（10） | `resource_playbook_test.go`（**11**：首轮 10 + review 跨 ns 隔离补测 1（§8）） | `TestPlaybookResourceCreateListGet`、`…CreateBodyRules`、`…SlugTaken`、`…PutCAS`、`…RenderIsReadOnly`、`…ActionValuesErrors`、`…InstallConflictWritesNothing`、`…InstallDryRunWritesNothing`、`…InstallReceiptsAndDeleteKeeps`、`…AuthRequired` | files/source 互斥、409 slug_taken、PUT CAS、render 0 写、**安装冲突 0 写**、dryRun 0 写、installs 折叠/单回执、DELETE `kept` |
| `node_create_test.go`（11）、`nodes_batch_test.go`（5）、`console_test.go` 的 node 段 | `resource_node_test.go`（**15**：首轮 12 + review 补 2（§8）+ 返修 1（§9.5）） | `TestNodeResourceCreateReturnsEnvelopeWithoutToken`、`…TokenRotatesAndPlaintextAppearsOnce`、`…TokenUnknownNodeIs404`、`…ConfigDefaults`、`…ConfigPutCAS`、`…PatchRequiresCAS`、`…DeleteRequiresCAS`、`…ConfigBatchPartialSuccess`、`…GetNamespaceContext`、`…Upgrade`、`…ListEmptyIsArray`、`…RoutesAreSessionWrapped` | 登记不回 token、**明文只回一次**+轮换后旧 hash 失效、404、config wrapper 默认/CAS、PATCH/DELETE CAS、批量部分成功、`?namespace=` 上下文、升级分支、空列表 `[]` |
| `namespace_handlers_test.go`（12） | `resource_namespace_test.go`（**11**：首轮 9 + review 补 1（§8）+ 返修 1（§9.5）） | `TestNamespaceResourceListCreateGet`、`…CreateDuplicateSlug`、`…PatchCAS`、`…DeleteCAS`、`…ConfigRoundTrip`、`…SystemConfig`、`…Stats`、`…SpecSwitchRejected`、`…RequiresSession` | 命名空间 CRUD、重复 409、CAS、ns 配置 revision 往返（不影响节点 configRevision）、system retention、stats 不跨 ns、`seedBuiltinCapabilities` 开关被拒 |
| `console_test.go` 的 task/trace/event 段、`console_events_page_test.go`（9）、`merge_fastpath_test.go`（5） | `resource_task_test.go`（**23**：首轮 15 + review 补 4（§8）+ 返修 4（§9.5）；另 `LogsNoArchive` 于返修轮改名 `LogsNoLocalArchiveIs410`，计数不变） | `TestTaskResourceListEmptyIsArray`、`TestTaskResourceGetEnvelopeFacts`、`TestTaskResourceDownstreamMissingShape`、`TestTaskResourceCancelUnknownIs404`、`TestTaskResourceCancelPendingBecomesCancelled`、`TestTaskResourceCancelRunningRecordsIntent`、`TestTaskResourceAbandonPendingBecomesAbandoned`、`TestTaskResourceLogsParamBounds`、`TestTaskResourceLogsNoArchive`、`TestTaskResourceEmissionsEmptyShape`、`TestTaskResourceEmissionsShapeAndCursor`、`TestTaskResourceConsoleEventsNamespaceScopes`、`TestTaskResourceEventsAndTrace`、`TestTaskResourceListFilters`、`TestTaskResourceRoutesAreSessionWrapped` | Task 只读信封 / failed-attempt0(S1/E1) / cancel+abandon 终态 / logs 参数界与无档案语义 / emissions 游标 / console events 跨 ns 与 cursor 绑定 / trace 视图 / 列表过滤 |
| — （新增集成层） | `api_integration_test.go`（**9**：首轮 7 + review 补 1（§8 的 405）+ 收尾 1（`2f840ff` 的 `TestNamespaceResolveForUser`）；返修轮未增减） | `TestAPIIntegrationRouteTableIsV4Only`、`…EnvelopeDiscipline`、`…CASRejectsABA`、`…NamespaceIsolation`、`…FailedAttemptZeroTask`、`…NodeTokenOneShot`、`…NodeConfigRevision` | 六域 register 共存无 pattern 冲突、v3 扁平路由**必须 404**、四键/`status` 非 null/`items:[]` 横向一致、CAS ABA、slug→id 与跨 ns 隔离、cursor 绑 ns、E1 回执、token 一次性、configRevision 前进 |
| `emission_handlers_test.go`（4）、`task_logs_test.go`（9）、`task_logs_wire_test.go`（1） | 同上 `resource_task_test.go` | 见上 | emissions 形状/跨 ns 不可见、logs 零字节可用/降级不 5xx/每次请求打节点 |
| `internal/api/events_test.go`（18） | `internal/api/events_test.go`（8） | `TestEventIngressReceipt`、`…TypeFallsBackToSourceName`、`…Duplicate`、`…SourceRequired`、`…Unauthorized`、`…NamespaceScope`、`…DisabledSource` | 回执五字段/`tasks`/`pruned` 恒数组、同 `id` 去重回原 event/trace、source 必填 400、Bearer 鉴权、跨 ns 边界、停用源拒绝 |

> 说明：v3 的「同一个表单建多个能力 + 它们之间的边」（`POST /v1/capabilities/batch`）与
> `PUT /v1/capabilities/{name}/downstreams` 是 v3 的边写入口；v4 的边是 `spec.next` 的
> 派生投影（只读 `GET /v1/namespaces/{ns}/edges`）。它们的回滚/原子性断言（原 14 个用例）
> 由 `TestCapabilityResourceEdgesAndTemplates`（派生投影正确性）与 store 层
> `go test ./internal/store/...`（能力/版本事务原子性）共同承担——**store 侧原子性断言一条没删**。

## 4. retired：具名退役（v3 语义已不存在，替代方案见右列）

### 4.1 v3 `edges` / `downstreams` 写入口（19 个用例）

| 文件 | 退役 Test 名 | 理由 + 替代 |
|---|---|---|
| `console_downstreams_test.go`（6） | `TestCapabilityDownstreamsReplace`、`…AtomicOnInvalidPayload`、`…OnlyTouchesOwnEdges`、`TestPostEdgesCapabilityWriteRemoved`、`TestCapabilityDownstreamsEndpointScope`、`TestCapabilityDownstreamsEdgeCases` | `PUT …/downstreams` 与 `POST/DELETE /v1/edges` 是 v4 明确移除的写端点（api-conventions §9「明确移除」）。边改为 `spec.next` 的派生只读投影 → `TestCapabilityResourceEdgesAndTemplates`；边的事务原子性由 store 层能力写事务测试覆盖 |
| `console_edge_literals_test.go`（3） | `TestEdgeCreateWithLiterals`、`TestEdgeCreateWithoutLiterals`、`TestEdgeLiteralsRejected` | 同上（写端点不存在）。「出口封闭」不变量（A5）现由 `artifact.Validate`（`unknown_target`）在能力写入期拦 → `TestCapabilityResourceWriteValidation` |
| `console_edge_refs_test.go`（9） | `TestEdgeCreateWithRefs`、`TestDownstreamsLegacyIDInputCompat`、`TestEdgeCreateRejectsUnknownRef`、`TestDeleteEdgeByRef`、`TestCreateCapabilityUpgradeGate`、`TestCreateCapabilityAdditiveNoGate`、`TestListEdgesReturnsResolvedIDs`、`TestListEdgesResolvedIDFollowsLatest`、`TestDeleteEntryEdgeByRef` | 同上；`TestListEdges*` 的「派生边解析到真实 id」意图 → `TestCapabilityResourceEdgesAndTemplates`（返回 from/to/kind 的派生投影） |
| `source_entry_edge_test.go`（1） | `TestFireAndEventsShareEntryEdgePath` | 入口边不再落 `edges` 行（v4 源 fanout 由 `source.Service` 直接规划）。等价意图（fire 与 events 入口走同一条派发路径）→ `TestSourceResourceFireReceiptShape` + `TestSourceResourceHookResolvesNamespaceByToken`（两条入口都真实建 Task） |

### 4.2 v3 批量创建能力（9 个用例）

| 文件 | 退役 Test 名 | 理由 + 替代 |
|---|---|---|
| `capability_batch_test.go`（9） | `TestPostCapabilityBatchCreatesAllFaces`、`…EmptyLinksIsArray`、`…IdempotentReplay`、`…RollsBackEverything`、`…DuplicateName400`、`…CycleMatchesSinglePath`、`…UpgradeGate409AndForce`、`…EqualsSinglePath`、`…Validation400` | `POST /v1/capabilities/batch`（v3 批量表单 + 边 + force/upgrade gate）在 v4 无对应端点：能力的创建单位是**单个版本**（`POST …/capabilities`），边是派生投影。**「整批一个事务、失败 0 写」**这条安全意图保留在 store 层（`go test ./internal/store/...` 的 CreateCapability/version 事务用例）与 `TestCapabilityResourceWriteValidation`（拒绝路径 0 写） |

### 4.3 v3 capability spec/artifact 专属语义（17 个用例）

| 文件 | 退役 Test 名 | 理由 + 替代 |
|---|---|---|
| `capability_artifact_test.go`（5） | `TestCapabilityArtifactReturnsParsedFiles`、`…NoArtifact404`、`…CrossNamespace404Identical`、`…Unauthenticated401`、`…EmptyLists` | v3 的 `GET /v1/capabilities/{id}/artifact`（按 id 取解析后 spec）在 v4 是 `GET …/capabilities/{name}/versions/{v}?includeBody=true`。意图映射：跨 ns 404 → `TestCapabilityResourceNamespaceIsolation`；未登录 401 → `TestCapabilityResourceRequiresSession`；空列表 → `TestCapabilityResourceEmptyLists`；files 投影 → `TestCapabilityResourceCreateAndGet`（默认空 map / includeBody 全文） |
| `capability_run_workdir_test.go`（3） | `TestCapabilityRunInputCoversArtifactRun`、`TestCreateCapabilityStoresRunWorkDir`、`TestListCapabilitiesExposesSpecRun` | `run.workDir` 往返现由上一行的 `TestCapabilityResourceCreateAndGet` + `TestCapabilityResourceVersionsDescending` 覆盖（字节级 spec 往返） |
| `capability_spec_p1b2_test.go`（5） | `TestPostCapabilityImplMissing400`、`TestPostCapabilityBatchImplMissing400`、`TestCapabilityListSpecStripsFiles`、`TestCapabilityListSpecHandlerForm`、`TestCapabilityArtifactReturnsFullSpec` | 写入期校验（impl 缺失/入口无 shebang/handler 形状）→ `TestCapabilityResourceWriteValidation`（`entrypoint_no_shebang` 等）；列表剥离 files → `TestCapabilityResourceCreateAndGet`（默认 `spec.run.files` 是空 map） |
| `capability_spec_p4_test.go`（4） | `TestPostCapabilityRejectsHandler`、`TestPostCapabilityBatchRejectsHandler`、`TestPostCapabilityLegacyFlatBodyRejected`、`TestPostCapabilitySpecRoundTrip` | v3 的「裸 body / legacy flat body / handler 字段」入口在 v4 只有资源信封一种，未知字段一律 400 → `TestCapabilityResourceWriteValidation`（unknown_field / status_not_writable / spec 未知字段）；spec 往返 → `TestCapabilityResourceCreateAndGet` |

### 4.4 v3 部署写入口（8 个用例，`deploy_test.go` 9 个中的 8 个；第 9 个 `TestListDeploymentsFilters` 是恢复不是退役）

| 文件 | 退役 Test 名 | 理由 + 替代 |
|---|---|---|
| `deploy_test.go`（9） | `TestCreateCapabilityWithRunCompilesArtifact`、`TestCreateCapabilityWithInvalidRunRejected`、`TestDeployHandlerCapability400BuiltinNotDeployable`、`TestDeployUnknownDigest404ArtifactNotFound`、`TestDeployToOfflineNodeCreatesRow`、`TestDeleteDeploymentRemovesRow`、`TestDeployRequestValidation`、`TestCapabilityTemplates` | v4：部署只经 `POST …/capabilities/{name}/deploy`（写 `deployments` 事实），**没有** `POST/DELETE /v1/deployments`、`GET /v1/artifacts`。映射：部署到离线节点仍建行 → `TestCapabilityResourceDeployToOfflineNode`；未知节点 404 → `TestCapabilityResourceDeployUnknownNode`；模板 → `TestCapabilityResourceEdgesAndTemplates`（含 `/v1/capability-templates`）；「按 run 编译 artifact / 非法 run 拒绝」→ `TestCapabilityResourceWriteValidation` + store 层 artifact 编译测试 |
| （同一文件）`TestListDeploymentsFilters` | **恢复，不是退役** | §9 明确「deployments 只保留 `GET /v1/namespaces/{namespace}/deployments`（事实）」。返修轮补回 ns 路径的读端点真用例 `TestRetainedDeploymentsListAndFilters`（列表/`nodeId`/`capabilityId` 过滤/跨 ns 隔离/空 `[]`/未知 ns 404），首轮把它随写入口一起删掉是误删 |

### 4.5 v3 中央执行域与派发预算（退役 4 + 恢复 3）

| 文件 | 退役 Test 名 | 理由 + 替代 |
|---|---|---|
| `console_handlers_test.go`（4） | `TestCancelTaskNudgesSweeper`、`TestCancelRunningTaskFreesSlotAndNudges`、`TestRedispatchHookNoBackoffOnCapacityShortage`、`TestRedispatchHookBacksOffWhenNoNodeDeclaresCapability` | `lease.Sweeper.Redispatch` / `.Nudge` / `db.SetNotBefore` 在 v4 **已删除**（重派由 `ws.Hub.RunDispatchLoop` + `store.ProposeTaskAssignment`）。cancel 的「终态 + 停止授权」→ `TestTaskResourceCancel*`；「槽位释放后立即重派」的语义归 `store.ReclaimStaleSlots`/`ws` 包测试（本 issue 只读） |
| `dispatch_budget_test.go`（3） | `TestDispatchBudgetDefaults`、`TestDispatchBudgetEnvParsing`、`TestValidateRejectsBadDispatchBudgets` | **返修轮恢复**（不是退役）：首轮理由「env 解析的对象不存在了」与事实不符——`Config.MaxHops/TraceTaskBudget` 仍在 `internal/server/config.go:433-435` 解析、`:478-495` 启动 `Validate`、`mux.go` 启动日志打印；退役后 `grep MaxHops *_test.go` = 0，配置面裸奔。v4 只是**不再消费**这两个预算（`route.Router` 无 hop 预算），不等于「配错也不该报错」。恢复的三条只改默认值来源（从 `route.DefaultMaxHops` 改成本包 `defaultMaxHops`），断言逐字保留 |

### 4.6 v3 跨 ns 归并快路径（5 个用例）

| 文件 | 退役 Test 名 | 理由 + 替代 |
|---|---|---|
| `merge_fastpath_test.go`（5） | `TestMergeEventsSingleGroupKeepsStoreOrder`、`TestMergeEventsSingleGroupLimit`、`TestMergeEventsSingleGroupEmptyIsArray`、`TestMergeEventsMultiGroupStillSorts`、`TestMergeTasksSingleGroupKeepsStoreOrder` | 被测函数 `mergeEvents` / `mergeTasks` 随 `console_handlers.go` 一起退役：v4 的 `GET /v1/console/events?namespace=*` 与任务列表各自走 store 的 `ORDER BY created_at DESC, id DESC` + keyset。「单 ns 快路径 / 跨 ns 归并仍要重排 / 空列表 `[]`」三条意图 → `TestTaskResource*`（console events cursor 与列表）与 `TestAPIIntegrationEnvelopeDiscipline` |

### 4.7 v3 openapi 字段级 parity（3 个用例）+ 纯夹具文件（0 用例）

| 文件 | 退役对象 | 理由 + 替代 |
|---|---|---|
| `openapi_parity_test.go`（3） | `TestOpenAPIParityStructFields`、`TestOpenAPIParityArraysNeverNull`、`TestOpenAPIParityZeroCollectionsMarshal` | 它核的是 **v3** `api/openapi.yaml` ↔ v3 DTO；v4 的 exact-five spec + `check_envelope.py` + parity 门禁是 **#333（G3b）owner**，本 issue 文件所有权禁止改 `api/**`。安全意图：`ArraysNeverNull` → `writeList`/`nonNilSlice` 单点收口 + `TestCapabilityResourceEmptyLists`/`TestAPIIntegrationEnvelopeDiscipline`；`StructFields` 双向对照 → #333 |
| `internal/server/zz_spec_fixture_test.go`（0 用例） | 仅 `withSpec`/`bodyToSpec` 等 v3 夹具 | v3 扁平 capability 模型的夹具（`ParamsSchema/Outputs/Sticky/Lifecycle/Handler`），这些字段在 v4 都不存在；**0 个 `func Test`，无断言损失** |
| `internal/api/zz_spec_fixture_test.go`（0 用例） | 仅 `withSpec` | 同上 |

### 4.8 v3 节点创建与批量读模型（16 个用例）

| 文件 | 退役 Test 名 | 理由 + 替代 |
|---|---|---|
| `node_create_test.go`（11） | `TestCreateNodeRequiresNamespace`、`TestCreateNodeRejectsAllNamespaces`、`TestCreateNodeNamespaceScope`、`TestCreateNodeDuplicateNameConflict`、`TestCreateNodeTokenWorksForWSHandshake`、`TestCreateNodeNameValidation`、`TestCreateNodeDoesNotCreateInstances`、`TestCreateNodeRequiresSession`、`TestCreateNodeNamespaceQueryWins`、`TestCreateNodeNamespaceInBody`（+`TestCreateNodeReturnsTokenOnce` 迁移） | v4 Node 是 **cluster-scoped**（W4）：登记不再带 namespace（`spec.namespaces` 是授权，不是创建作用域），登记端点也**不发 token**。映射：登记不回 token / 重复名 409 / 需登录 → `TestNodeResourceCreateReturnsEnvelopeWithoutToken`、`…RoutesAreSessionWrapped`；token 只在 `POST /v1/nodes/{name}/tokens` 且 one-shot → `…TokenRotatesAndPlaintextAppearsOnce`；「不建 instances」→ `…ConfigDefaults`（新节点无实例占用断言在 `…ListEmptyIsArray` 的真实快照里） |
| `nodes_batch_test.go`（5） | `TestViewsForBatchEquivalence`、`TestViewsForBatchEmptyInput`、`TestBatchStoreQueriesEdgeCases`、`TestBatchListCapabilitiesByNodeOrdering`、`TestViewsForBadJSONDegradesOnlyThatNode` | 被测的 `db.NodesDeclaringCapability` / `db.ListCapabilitiesByNode` / `consoleHandler.viewsForBatch` 在 v4 不存在（能力归属不再由节点自报）。v4 的批量语义是**节点配置批量写**（`POST /v1/nodes/config/batch`，逐节点原子可部分成功）→ `TestNodeResourceConfigBatchPartialSuccess` |

### 4.9 v3 命名空间 CRUD 的遗留口径（旧文件 12 条 = 9 迁移 + 2 恢复 + 1 退役）

| 旧用例（`namespace_handlers_test.go`，12→9；返修后 11，见 §9.5） | 处理 |
|---|---|
| `TestNamespaceListIncludesResourceCounts`、`TestNamespaceListRequiresSession`、`TestNamespaceCreateRejectsInvalidSlug`、`TestNamespaceCreateDuplicateSlugConflicts`、`TestNamespacePatchRenamesNameOnly`、`TestNamespacePatchUnknownIs404`、`TestNamespaceDeleteLastIs400`、`TestNamespaceDeleteBlockedListsBlocking`、`TestNamespaceDeleteUnknownIs404` | 迁移到 `TestNamespaceResource*`（见 §3） |
| `TestStatsRequiresNamespaceId`、`TestStatsWithNamespaceCountsOnlyThatNamespace` | **返修轮恢复原路径**：`/v1/stats` 是 §8.4 冻结保留的 cluster 读模型（首轮误改成 ns 子资源路径且未登记）。替代/恢复：`TestRetainedStatsRequiresNamespaceAndScopes`（缺 `?namespace=` → 400、`*` → 400、slug/id 等价、逐 ns 计数）+ 保留 `TestNamespaceResourceStats`（ns 子资源是 `Namespace.status` 的镜像） |
| `TestNamespaceCreateSeedsBuiltinCapabilities` | **退役**：`spec.seedBuiltinCapabilities` 是 v4 明确移除的开关（api-conventions §9「明确移除」），内置能力播种随中央执行域一起退役（D0.2）。替代：`TestNamespaceResourceSpecSwitchRejected`（该开关在写请求里必须是 400 `unknown_field`，而不是被静默忽略） |
| `TestNamespaceAllWriteRejected`（在 `console_test.go`） | 迁移：`namespace=*` 只允许读 → `TestNamespaceResourceRequiresSession`/`TestAPIIntegration*` + `namespace_scope.go` 的 `allowAll` 判据 |
| `TestListAcrossNamespaces`、`TestNamespaceUnknownIsNotFound`、`TestNamespaceNotFound`、`TestNamespaceRequired` | 迁移到 `TestAPIIntegrationRouteTableIsV4Only`（v3 路径 404）+ `TestNamespaceResource*`（v4 路径语义） |

## 5. 新增用例清单

首轮交付：**server 新增 84（实测 `2c3b2d3`；旧文写 83 已修正）+ api 8**。
返修轮（§9）：server **+20**（其中首轮回修 `f0016b8` +17、M-1 +2、minor-2 +1、M-2 ±0，见 §9.5）；
当前 `internal/server` = **265** 个 `func Test`（**264 个用例 + 1 个 `TestMain`**），`internal/api` = 8。

```
# ---- 首轮交付（84 + 8；文件后数字为 `3243ef8` 的首轮初版计数）----
internal/server/resource_capability_test.go  11  TestCapabilityResource*
internal/server/resource_source_test.go       8  TestSourceResource*          # 首轮初版 8；当前 11（§8 +2、§9.5 +1）
internal/server/resource_playbook_test.go    10  TestPlaybookResource*        # 首轮初版 10；当前 11（§8 +1）
internal/server/resource_node_test.go        12  TestNodeResource*            # 首轮初版 12；当前 15（§8 +2、§9.5 +1）
internal/server/resource_namespace_test.go    9  TestNamespaceResource*       # 首轮初版 9；当前 11（§8 +1、§9.5 +1）
internal/server/resource_task_test.go        15  TestTaskResource*            # 首轮初版 15；当前 23（§8 +4、§9.5 +4）
internal/server/api_integration_test.go       7  TestAPIIntegration*          # 首轮初版 7；当前 9（§8 +1、`2f840ff` +1）
internal/server/node_upgrade_test.go          4  TestNodeUpgrade*
internal/server/task_out_retired_test.go      2  TestTaskEndpointsExposeNoOut / TestTraceEndpointExposesNoOut
internal/api/events_test.go                   8  TestEventIngress*（含 TestEventIngressSlugPath）
# ---- 返修轮 +17（=`f0016b8`，§9.1–§9.2）----
internal/server/retained_reads_test.go        4  TestRetained*                # 冻结保留读模型恢复
internal/server/production_mux_test.go        1  TestProductionMux*          # 生产 mux 整机冒烟（§9.5 再 +1 → 2）
internal/server/dispatch_budget_test.go       3  TestDispatchBudget*/TestValidateRejectsBadDispatchBudgets
internal/server/resource_task_test.go        +4  previous-none / no-attempt410 / events-limit 钳位 / downstream_missing 投影
internal/server/node_upgrade_test.go         +2  up_to_date+force+dirty / isUniqueVersion
internal/server/resource_namespace_test.go   +1  TestNamespaceResourceResolveByIDSlugSymmetry
internal/server/resource_node_test.go        +1  TestNodeResourceConfigWriteStatusIsNotWritable
internal/server/resource_source_test.go      +1  TestSourceResourceReservedNameIs400
# ---- r2 再 +3（§9.5）----
internal/server/production_clock_test.go      2  TestProductionMuxStatsWindowUsesMillisecondClock / …CancelRequestedAtIsRealTime
internal/server/production_mux_test.go       +1  TestProductionMuxRetiredWritesAre405
internal/server/ws_settle_test.go             0  serveHubWithSettle（真 WS 收尾屏障夹具，无 func Test）
```

## 6. 断言强度自查（未放宽）

| 纪律 | 现状 |
|---|---|
| 认证 | 每个资源域都有 `…RequiresSession` 或 401 断言；`TestAPIIntegrationRouteTableIsV4Only` 额外钉住 v3 路由 404 |
| 权限/跨 ns | `TestCapabilityResourceNamespaceIsolation`、`TestSourceResource*`（跨 ns）、`TestNodeResource*`、`TestAPIIntegrationNamespaceIsolation`（含 cursor 绑定）、`TestTaskResource` 的跨 ns 不可见 |
| CAS/delete | 6 个域各有缺令牌 400 / 旧令牌 409 + `details.current.resourceVersion` / 删除后 404 的断言（`TestAPIIntegrationCASRejectsABA` 做横向 ABA 复核） |
| secret/digest | `TestSourceResourceCreateGetMasksSecrets`（token/secret 全文不出现）、`TestNodeResourceToken*`（明文只一次 + 旧 hash 失效）、能力 digest 往返（版本族） |
| 0 写 | `TestPlaybookResourceInstallConflictWritesNothing`（安装冲突前后能力/源计数不变）、`TestPlaybookResourceInstallDryRunWritesNothing`、`TestSourceResourceDryRunSideEffectFree` |
| 背压 | `backpressure_test.go` 13 条一行未改 |
| FK/原子性 | 依赖 store 侧既有测试（`go test ./internal/store/...` 全绿），本 issue 未改 store |

## 7. 未迁移项（明确登记，不留悬空）

| 项 | owner | 原因 |
|---|---|---|
| exact-five `api/*.yaml` + `check_openapi.py` + `check_envelope.py` + parity | #333（G3b） | 331 卡文件所有权禁止改 `api/**`、`scripts/**` |
| Artifact 上传/下载（A1/A2 通道）与 `internal/resultspool/**` | #332 | 331 卡明确划归 #332 |
| `cmd/devseed` / `cmd/` 旧接口静态断裂 | #334 | `go build ./...` 仅 devseed 失败，已如实列错误 |
| Playbook `spec.source`（OCI）**不实现服务端拉取** | 后续交付（W3 的 OCI 部分，owner 待 root 指派） | W3 要求「服务端拉取一次后以解析的完整 files 包存储」，涉及网络、凭据、媒体类型与 provenance（playbook.md §11）。本 issue 只实现**互斥校验**：`files` 与 `source` 同时给 → 400 `invalid_playbook`；只给 `source` → 400 `oci_unsupported`（明确拒绝，不静默接受、不 500）。已有表驱动用例 `resource_playbook_test.go` 的 `CreateBodyRules` 覆盖三条分支 |
| `TestOpenAPIParity` 已删但 Makefile `make contract` 仍点名 + `scripts/check_openapi.py` 在 head EXIT 1 | **#333（G3b）** | v3 parity 已具名退役（§4.7），但 `Makefile:92` 仍执行 `go test -run TestOpenAPIParity` → `[no tests to run]` EXIT 0（静默通过）；`check_openapi.py` 只扫 `mux.go` 字面量，而 v4 路由在 `resource_*.go` 动态拼接 → 34 条「openapi 多写」+1 条「mux 有 openapi 缺」。本 PR 文件所有权禁止改 `Makefile`/`scripts/**`/`api/**`；合并后 `make ci` 的 contract 步骤需要 #333 处置 |
| Event 信封缺 §7.7 声明的 `spec.type` / `spec.signature` / `status.dispatched` / `status.deliveredAt` | **#333（spec）/ 后续 store 交付** | `events` 表没有 severity / signature / dispatch / deliveredAt 列，投影**不编造**（`eventSpecDoc` 注释）。精确缺口：`internal/store/schema.sql` 的 `events` 只存 `id/namespace_id/trace_id/source/type/payload/delivery_id/reason/receipt/created_at`；要回 §7.7 四字段必须补列并写事实，属 store 变更（本 issue 只读）。`spec.reason` 对 `downstream_missing` 失败观察事件当前**省略**（store 把观察行统一写 reason='Pruned'，照抄会把失败报告成剪枝，见 §9-m12） |
| `GET /v1/admin/messages` 缺 `?namespace=` 逐 ns 过滤/授权（§8.3） | **#338** | `internal/server/admin_handlers.go` 的 `listMessages` 只有 `limit`，走 `ListGlobalEvents` 跨全部 ns 并在 item 里带内部 `namespaceId`。本 PR/base 均未改该文件（保留域既有行为），§8.3 要求显式 ns + 逐 ns 授权；登记 owner #338，不在本 issue 顺手改 |
| drain 动作响应 / Node 读模型不含 draining 事实 | 契约缺口（记录；owner 待 root 指派） | `nodeAPI.drain` 先 `UpdateNodeStatus(draining)`，但响应与 `GET /v1/nodes` 的 `status` 按 §7.4 不含生命周期字段 → 客户端无法确认动作生效。已在 `resource_node.go` 文件头声明，本轮**保持记录**（m7），不在本 PR 发明 Node.status 字段 |
| `downstream_missing` 观察事件在 store 里 `reason` 仍是 `'Pruned'` | 后续 store 交付（记录） | `internal/store/event.go:253-280` 与失败 Task 共用 `insertPruneFactEventOn`，`reason` 固定 `Pruned`，只有 `type` 能区分。投影层已按 type 把它摘出剪枝语义（§9-m12），但 **store 侧事实仍不干净**（trace 查询 SQL 仍按 `source='route'` 收录）。要彻底修需 store 增失败观察标记/独立 reason（本 issue 只读） |
| `internal/push.CentralNotifier` 已成死代码 | 后续清理（无 owner） | 它原由中央执行器调用；v4 保留 VAPID 装载与公钥下发（`GET /v1/vapid-public-key`）、devices 域不动，但 Sender 构造路径不再被 server 引用。**未删除**：删除它属于 `internal/push`（只读）且不在本 issue 功能范围 |

## 8. 补充用例（fresh review 覆盖缺口，追加至 §5 清单）

review 指出 8 处覆盖缺口（跨 ns 隔离、失败 0 写、logs 降级矩阵、405、非空 ns 删除、
令牌是真凭证、批量逐项错误）。逐条补真实 httptest + 真 store 断言，**未改产品代码**。

| # | 缺口 | 新用例（文件） | 真实断言内容 |
|---|---|---|---|
| 1 | Source 跨命名空间隔离（零覆盖） | `TestSourceResourceNamespaceIsolation`（`resource_source_test.go`） | 两个真命名空间 `src-a`/`src-b` 各建同名出口能力；在 A 建的源：B 的 list `items` 长度 0、B 单读 404 且响应体不含 A 的 uid（不泄露存在性）、拿 A 的令牌在 B 做 PATCH/DELETE 均 404 且 A 的 `resourceVersion` 未前进、`spec.enabled`/`spec.targets` 原样、A 列表仍 1 条 |
| 2 | fire 失败 0 写（原来只断 404） | `TestSourceResourceFireFailureWritesNothing`（`resource_source_test.go`） | 停用源 + 不存在源各 fire 一次（两次都 404 `NotFound`）前后：`tasks`/`events` 表行数完全不变；`ListTasks`/`CountOpenTasks`/`ListEvents`（含剪枝）读模型均为 0；源本身仍 200 且 `enabled=false` |
| 3 | Playbook 跨命名空间隔离 | `TestPlaybookResourceNamespaceIsolation`（`resource_playbook_test.go`） | A 建包后：B 的 list `items` 数组长度 0、B 单包 GET 404（Status/`NotFound`）、拿 A 的 CAS 令牌在 B DELETE 404；A 里 GET 仍 200、`resourceVersion` 未前进、A 清单仍 1 条 |
| 4a | logs 降级：节点离线 503 | `TestTaskResourceLogsNodeOfflineIs503`（`resource_task_test.go`） | 真实 `task_attempts` 行指向无会话的 node → **503** + `Status.code=503` + `reason="node_offline"`，且响应**不含** `lines`（不把离线伪装成"这次没输出"） |
| 4b | logs 降级：节点读不了档案 502 | `TestTaskResourceLogsNodeErrorIs502`（`resource_task_test.go`） | 真 WebSocket 节点会话（hello→welcome，真 Hub）收到 `logs.request` 后回 `error=logs_unreadable` → **502** + `reason` 原样回传封闭码，且不伪造 `lines` |
| 4c | logs 降级：等不到回包 504 | `TestTaskResourceLogsWaitTimeoutIs504`（`resource_task_test.go`） | 同一真会话下节点收到 `logs.request` 但**不回包** → **504** + `reason="timeout"`（`protocol.ReasonTimeout`），不伪造 `lines`。用可取消的请求上下文确定性造出"等不到回包"（`taskLogsTimeout` 10s 是常量，不真等；Hub 自己的 `ErrLogsTimeout` 由 `internal/ws` 用例覆盖） |
| 4d | logs 成功路径逐条记录（支撑"lines 恒数组/attempt 整数"） | `TestTaskResourceLogsNodeReturnsStructuredLines`（`resource_task_test.go`） | `tailLines=10&sinceSeconds=60` 原样进 `logs.request`；节点回 2 条真实记录 → 200、原文 `"lines":[...]`（数组）、`attempt=1` 整数、`truncated` 原样、逐条 `sequence/stream/text/at` 与节点给的一致 |
| 5 | 405（Method Not Allowed） | `TestAPIIntegrationMethodNotAllowed`（`api_integration_test.go`） | `DELETE /v1/namespaces/{slug}/capabilities` 与 `POST /v1/nodes/{name}` 均 **405**（不是 404），并断言 `Allow` 头含该路径已注册的方法 |
| 6 | 非空命名空间删除（409 + blocking + force） | `TestNamespaceResourceDeleteBlockedAndForce`（`resource_namespace_test.go`） | 真能力行阻塞：默认 DELETE → **409** `Conflict`、`details.causes[0].field="blocking"`、`message` 说明有依赖对象、`Status.message` 含 "not empty"；被拒后 ns 与能力行都还在（零写）；`?force=true` → **204** 级联、随后 GET 404、能力行被清 |
| 7 | Node token 是真身份凭证 | `TestNodeResourceTokenIsRealCredential`（`resource_node_test.go`） | `HashNodeToken(明文)` → `GetNodeByTokenHash`（Hub 握手用的同一查询）必须返回同一 `metadata.uid` 且 `TokenHash == hash`（≠明文）；轮换前那枚哈希立刻失效；再轮换后旧明文哈希查不到、新明文哈希查到同一台 |
| 8 | Node config batch 缺令牌逐项 400 | `TestNodeResourceConfigBatchMissingTokenIsItemError`（`resource_node_test.go`） | 一批两台：缺 `metadata.resourceVersion` 的那项 `ok:false` + `error.kind=Status` + `code=400` + `reason="resource_version_required"` 且无 `config`；同批带令牌的那项 `ok:true` 并真的写入；整请求仍是 **200**（不是 400），缺令牌项的 `vars` 没有旁路落库 |

计数：新增 11 个用例（server 10 + 跨域集成 1，全部在 `internal/server` 包内）。

> 时点说明：本节写于首轮 review 补缺提交（`d8d8c42`）当时；其后 `3243ef8` / `2f840ff` 及返修轮
> 确实改了 `internal/api/events.go`、`namespace_scope.go`、`resource_node.go`、`mux.go` 等产品文件。
> 「`internal/api` 与产品代码未改」只描述本节那一刻，**不是最终状态**（review m4-4 的指正）。

## 9. 返修轮（PR #340 review → 本分支追加修复）

> 审核全文见 PR #340 review（`ok:false`）。协调者逐条裁决全部成立；本节记录每条 finding 的
> 落实位置与证据用例，以及「不改」项的 owner。

### 9.1 major（必须修）

| # | finding | 处理 | 证据（文件:行号 / 用例） |
|---|---|---|---|
| FIX-1 (M1) | 冻结保留的只读端点被删/改名：`/v1/stats` 被改成 ns 子资源、`/v1/orphans` 删除、`/v1/namespaces/{ns}/deployments` 缺失 | **恢复**三条读端点（`newRetainedReadAPI`）：「显式 `?namespace=` + 逐 ns 授权，缺参 400，不跨 ns 默认全量」；deployments 走 ns 路径只读事实。`edges` 派生投影确认仍在。集成路由表从「断言 404」改为断言 200 | `internal/server/retained_reads.go:1-235`、`mux.go`（`newRetainedReadAPI(deps).register(mux)`）、测试 `TestRetainedStatsRequiresNamespaceAndScopes`、`TestRetainedOrphansRequiresNamespaceAndPerNamespaceAuth`、`TestRetainedDeploymentsListAndFilters`、`TestRetainedEdgesProjectionExists`、`TestAPIIntegrationRouteTableIsV4Only`、`TestProductionMuxBootsAndServesRetainedDomains` |
| FIX-2 (M2) | upgrade 丢 `up_to_date`/`force`/dirty 判定 → 已最新节点被多重启一次 | 恢复判定链：离线 409 → 无二进制 501 → `agent_version == 目标 && isUniqueVersion && !force` → 200 `up_to_date`（**不写 sent、不推帧**）；`force` 支持 body `{"force":true}` 与 `?force=1|true`；dirty（`dev` / 含 `dirty`）永不判最新 | `internal/server/resource_node.go` 的 `upgradeNode`/`nodeForceFromQuery`、`node_upgrade_handlers.go` 的 `isUniqueVersion`；测试 `TestNodeUpgradeEndpointUpToDateAndForce`（4 子用例，真 WS 在线会话 + 读超时证明不推帧）、`TestIsUniqueVersionRule`、`TestNodeUpgradeEndpointGuards`（离线 409 / 在线 501） |
| FIX-3 (M3) | logs `no_local_archive` 回 200 空（契约 G3-D3 `noarchive410`），且该分支零 HTTP 覆盖 | `no_local_archive` → **410** + `reason=no_local_archive`（不伪造 lines）；「任务还没任何 attempt」同口径 410；唯一 200 空只保留 `previous=true` 且无上一次 | `internal/server/resource_task.go`（`writeLogsGone` / logs 分支）；测试 `TestTaskResourceLogsNoLocalArchiveIs410`（真 WS 节点回 `error=no_local_archive`）、`TestTaskResourceLogsNoAttemptIs410`、`TestTaskResourceLogsPreviousNoneIs200Empty` |
| FIX-4 (M4) | `GET /v1/console/events?namespace=*` 每条 item `metadata.namespace` 恒为 `"*"` | 逐 item 用 `namespaceDisplay(ctx, e.NamespaceID)` 回显事件自身 slug；单 ns 视图保持同一判据 | `internal/server/resource_task.go` 的 `consoleEvents`；测试 `TestTaskResourceConsoleEventsNamespaceScopes`（nsA 3 条 + nsB 2 条，逐 item 断言 `{slugA:3, slugB:2}`） |

### 9.2 minor

| # | finding | 处理 / 记录 | 证据 |
|---|---|---|---|
| m1 | 保留源名 `route` 从 400 退化成 500 | `writeAPIError` 增加 `store.ErrReservedEventSource → 400 invalid_source_config`；恢复 v3 守卫用例（含 body 提 `route`、0 写） | `api_status.go`；`TestSourceResourceReservedNameIs400` |
| m2 | events 列表 `limit>500` 静默折回 50（#236 回归） | handler 侧钳到 500（与 emissions/console events 同档）；缺省仍 50 | `resource_task.go` 的 `taskClampListLimit`；`TestTaskResourceEventsListLimitClamps` |
| m3 | E2 `apiVersion`/`kind` 必填未强制 | **未改**：属信封写语义（`api_envelope.go`），与 #333 的 envelope checker 口径绑定；本轮不改以免与 G3b 契约冲突。记录在交付报告风险项（当前服务端对缺失 apiVersion/kind 宽松，客户端仍必须给） | — |
| m4 | inventory 与代码 6 处不符 | 本文件整体重算：删除文件 34（27 test + 7 源）/ 178 个退役 `func Test`；`internal/api` 18→8 的 10 个旧名逐名登记（见下）；`§4.5` MaxHops 理由改正并恢复 3 条用例；`§5` 计数按实测；`§8` 时点说明；`§4.9` stats 恢复原路径 | 本文件 |
| m5 | namespace 解析读写不对称（只读兼容 id、写只认 slug） | `namespaceAPI.resolve` 统一走 `namespaceBySlugOrIDOn`；PATCH/DELETE 用解析行的 `ns.Slug` 调 store 写入口 | `resource_namespace.go`；`TestNamespaceResourceResolveByIDSlugSymmetry` |
| m6 | node config 写 `status` → `unknown_field` | 配置写解码前识别 `status`（顶层与 `updates[]`）→ 400 `status_not_writable` | `resource_node.go` 的 `nodeDecodeConfigStrictJSON`；`TestNodeResourceConfigWriteStatusIsNotWritable` |
| m7 | drain 响应/Node 读模型不含 draining 事实 | **保持记录**（§7「未迁移项」），不在本 PR 发明 Node.status 字段 | §7 |
| m8 | `TestOpenAPIParity` 0 测试 + `check_openapi.py` EXIT 1 | **owner #333**（§7） | §7 |
| m9 | `includeBody` 取值不一致（capability 只认 true / playbook 认 true+1） | 抽共享 `includeBodyQuery`（`true`/`1`，忽略大小写与空白），两个域同一份语义；补 false/缺省/`1` 三档断言 | `api_envelope.go`；`TestCapabilityResourceCreateAndGet`、`TestPlaybookResourceCreateListGet` |
| m10 | 无生产 mux 整机测试；`serverDeps` 两个未读字段 | 补真实 `NewApp` 整机冒烟（可起 + 关键保留域 200 + 匿名 401）；`Router`/`Sweeper` 两个注入字段本轮**保留**（它们是装配契约的一部分，删除需另开清理，未在 review 要求内） | `production_mux_test.go` |
| m11 | Event 信封缺 §7.7 字段 | **不伪造**：登记精确缺口（events 表缺列）+ owner #333/后续 store 交付（§7） | §7 |
| m12 | `downstream_missing` 观察事件被投影成「剪枝」 | 投影层按 `source='route' && type=downstream_missing` 摘出剪枝语义：`spec.reason` 省略、`status.pruned=null`、`trace.pruned[]` 不收录；事件本身仍在 `events[]`（事实不丢）。store 侧 reason 事实仍不干净 → §7 记录 | `resource_task.go` 的 `eventIsDispatchFailure` / `eventEnvelope` / `eventPruneFactOf`；`TestTaskResourceDownstreamMissingEventIsNotPruned` |
| m13 | Task 列表过滤参数改名且未知参数静默忽略 | 参数名改回 `status`/`traceId`/`nodeId`（+`limit`）；**契约未定义的参数一律 400 `unknown_field`**（`phase`/`trace`/`node`/`capability` 均 400；`capability` 过滤因此退役，记录在此） | `resource_task.go` 的 `taskListUnknownParams`；`TestTaskResourceListFilters` |
| m14 | `/v1/admin/messages` 缺逐 ns 过滤（§8.3） | **owner #338**（§7，本 PR/base 均未改该文件） | §7 |

### 9.3 `internal/api` 18 → 8 逐名（D6 补齐）

保留的 8 个（全部真实 httptest）：`TestEventIngressReceipt`、`…TypeFallsBackToSourceName`、
`…Duplicate`、`…SourceRequired`、`…Unauthorized`、`…NamespaceScope`、`…DisabledSource`、`…SlugPath`。

未保留的 10 个旧用例与去向：

| 旧用例 | 去向 |
|---|---|
| `TestEventToDispatch` | → `TestEventIngressReceipt`（回执五字段 + tasks/pruned 恒数组） |
| `TestEventIdempotentHTTP` | → `TestEventIngressDuplicate`（同 deliveryId 回原 event/trace、不重复派发） |
| `TestEventNoEdgePruned` | → `TestEventIngressReceipt`/`…SourceRequired`（无 target 的 200 + pruned[] 在 server 侧 `TestSourceResourceFire*` 覆盖） |
| `TestEventUnknownSourceAccepted` | → `TestEventIngressSourceRequired`（source 必填/未知 source 的边界由 store 校验） |
| `TestEventParamMissingPruned` | → `TestEventIngressReceipt`（pruned[] 形状）+ store `PruneInputs` 测试 |
| `TestEventAuthRequired` | → `TestEventIngressUnauthorized`（无/错 key 401、缺 scope 403 三分支，比 v3 并集更强） |
| `TestEventNamespaceAuthorization` | → `TestEventIngressNamespaceScope` + `TestAPIIntegrationNamespaceIsolation` |
| `TestEventList` / `TestEventPruneNotInEventStream` / `TestEventListNamespaceRequired` / `TestEventListAllNamespaces`（含 `TestEventMethodNotAllowed`、`TestEventNamespace*`、`TestEventListLimitCapHandlerClamps`） | 事件**列表**已迁到 server 资源域（`/v1/namespaces/{ns}/events`、`/v1/console/events`）：`TestTaskResourceEventsAndTrace`、`TestTaskResourceConsoleEventsNamespaceScopes`、`TestTaskResourceEventsListLimitClamps`（#236 钳位恢复） |

### 9.4 本轮机禁（真实 exit 见交付报告）

`gofmt -l` 空 / `git diff --check` 0 / `go vet` 0 / `go test ./internal/server/... ./internal/api/... ./internal/auth/... ./internal/authn/...` 0 /
`go test ./internal/store/...` 0 / `go build ./internal/...` 0 / `go build ./cmd/server` 0 / 生产代码无 `internal/executor` 引用。

### 9.5 r2 返修（#340 第二轮 review 后的提交，计数全部实测）

`f0016b8` 之后本轮 r2 返修做了 4 个批次的工作，交付时 squash 成**一个**提交
（`git rev-list --count f0016b8..HEAD` = 1；PR #340 的 head 就是这个 squash 提交）。
下表每一行的增量都用逐文件 `grep -c '^func Test'` 在工作树上实测相减得到（复算命令见本节末）。

| 批次 | 内容 | `internal/server` 用例增量 | 累计后总数 |
|---|---|---|---|
| `f0016b8` | §9.1/§9.2 的 major/minor 关闭（保留读模型、upgrade 判等、logs 410、events 投影、reserved source 400、limit 钳位等） | **+17** | 262（261 + 1 `TestMain`） |
| 返修 · **M-1** | 新增 `production_clock_test.go` 2 个用例 `TestProductionMuxStatsWindowUsesMillisecondClock`、`TestProductionMuxCancelRequestedAtIsRealTime`，用真实 `NewApp` 装配钉住 `serverDeps.Now` 的毫秒契约 | **+2** | 264（263 + 1） |
| 返修 · **minor-2** | deployments/edges 退役写在生产 mux 显式 405，`production_mux_test.go` 加 `TestProductionMuxRetiredWritesAre405` | **+1** | 265（264 + 1） |
| 返修 · **M-2** | 新增 `ws_settle_test.go`（**0 个 `func Test`**，提供本包真 WS 收尾屏障 `serveHubWithSettle`，等价于被删 `wsfixture_test.go` 的 `nodeStreamFixture.settle`）；`node_upgrade_test.go`/`resource_task_test.go`/`postclose_guard_test.go` 改用该屏障 | **±0** | 265（264 + 1） |

返修轮合计 = **+20**（`2c3b2d3` 的 245 → 返修终态的 265）；`internal/api` 返修轮未改，仍 8。

同轮「改断言但不改用例数」的两处（因此不在上表增量里）：

- `retained_reads_test.go`：方法断言从 POST/DELETE 扩到 POST/PUT/PATCH/DELETE，并改断 `Status` 体（仍 4 个用例）。
- `node_upgrade_test.go` / `resource_task_test.go` / `postclose_guard_test.go`：收尾屏障改造（分别仍 6 / 23 / 1 个）。

复算命令与实测结果（在返修终态树上执行）：

```bash
# 当前树逐文件
for f in internal/server/*_test.go internal/api/*_test.go; do
  printf "%-46s %s\n" "$f" "$(grep -c '^func Test' "$f")"
done
grep -rh '^func Test' internal/server/*_test.go | wc -l   # 265
grep -rh '^func Test' internal/api/*_test.go   | wc -l   # 8
grep -rc '^func TestMain' internal/server/*_test.go | grep -v ':0'   # postclose_guard_test.go:1
# 删除口径（base=b30e998）
git diff --name-status b30e998..HEAD -- internal/server internal/api | awk '$1=="D"' | wc -l               # 34
git diff --name-status b30e998..HEAD -- internal/server internal/api | awk '$1=="D"&&$2~/_test\.go$/' | wc -l  # 27
```

结果：`internal/server` = **265** 个 `func Test` = 264 个用例 + 1 个 `TestMain`（`postclose_guard_test.go`）；
`internal/api` = 8 个（`events_test.go`，无 `TestMain`）。
