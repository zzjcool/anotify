# G3a · #331 测试 inventory（server 345 + api 18 → server 233 + api 7）

> G3-D6 要求：先逐名 inventory，再按当前安全事实迁移；**禁止整文件清空**认证/权限/CAS/
> delete/secret/digest/FK/backpressure 断言；退役项必须**具名**并给出**替代用例**。
>
> 基线 `b30e998`（origin/v4/integration）；交付 `issue/331-http-resources`。
> 统计口径：`rg -c '^func Test' internal/server/*_test.go`（含 1 个 `TestMain`）与 `internal/api/events_test.go`。
>
> | | 旧 | 新 |
> |---|---|---|
> | `internal/server` | 345（344 用例 + 1 `TestMain`） | 233（232 用例 + 1 `TestMain`） |
> | `internal/api` | 18 | 7 |
>
> 差异全部是「v3 扁平资源面不存在了」的直接后果；**没有一条**是「为了让测试变绿而删除或放宽断言」。

## 0. 一句话结论

- **kept**：认证 / 设备 / CLI key / passkey / 密码 / 恢复码 / 静态资源 / 背压 / 日志安全 /
  gzip / 限流 / 状态写入器 / 词汇 / 嵌入 —— 155 个用例**一行未改**（文件级不变）。
- **rewritten**：`node_upgrade_test.go`（7→4）、`task_out_retired_test.go`（2→2，断言改为递归遍历 JSON）。
- **migrated**：v3 扁平 capability/source/playbook/namespace/node/task/event/log/emission 面 →
  v4 namespaced 资源面：新增 **72 个 server 用例**（含 7 个跨域集成）+ **7 个 `internal/api` 用例**。
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

### 2.1 `node_upgrade_test.go`（7 → 4）

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

## 3. migrated：v3 资源面 → v4 namespaced 资源面（78 个新用例）

| 旧文件（用例数） | 新文件 | 新用例（逐名） | 安全意图映射 |
|---|---|---|---|
| `ability_*` 见下（`capability_artifact_test.go` 5、`capability_batch_test.go` 9、`capability_links_test.go` 5、`capability_run_workdir_test.go` 3、`capability_spec_p1b2_test.go` 5、`capability_spec_p4_test.go` 4、`console_test.go` 27 中的 capability 段） | `resource_capability_test.go`（11） | `TestCapabilityResourceCreateAndGet`、`…VersionsDescending`、`…PatchCAS`、`…DeleteCAS`、`…NamespaceIsolation`、`…WriteValidation`、`…DeployUnknownNode`、`…DeployToOfflineNode`、`…EdgesAndTemplates`、`…RequiresSession`、`…EmptyLists` | 能力创建/版本族/内容寻址/`includeBody`/deploy 事实/edge 派生投影/未登录 401/CAS/跨 ns |
| `source_handlers_test.go`（19） | `resource_source_test.go`（8） | `TestSourceResourceCreateGetMasksSecrets`、`…WriteRejectsReadOnlyProjection`、`…PatchKeepsOmittedConfig`、`…PatchKindImmutable`、`…DeleteCAS`、`…FireReceiptShape`、`…DryRunSideEffectFree`、`…HookResolvesNamespaceByToken` | token/secret 脱敏（GET 与错误体）、PATCH 省略=保旧值、kind 不可变、DELETE CAS、fire 回执形状、dry-run 0 写、hook 由 token 定位 ns |
| `playbook_handlers_test.go`（10） | `resource_playbook_test.go`（10） | `TestPlaybookResourceCreateListGet`、`…CreateBodyRules`、`…SlugTaken`、`…PutCAS`、`…RenderIsReadOnly`、`…ActionValuesErrors`、`…InstallConflictWritesNothing`、`…InstallDryRunWritesNothing`、`…InstallReceiptsAndDeleteKeeps`、`…AuthRequired` | files/source 互斥、409 slug_taken、PUT CAS、render 0 写、**安装冲突 0 写**、dryRun 0 写、installs 折叠/单回执、DELETE `kept` |
| `node_create_test.go`（11）、`nodes_batch_test.go`（5）、`console_test.go` 的 node 段 | `resource_node_test.go`（12） | `TestNodeResourceCreateReturnsEnvelopeWithoutToken`、`…TokenRotatesAndPlaintextAppearsOnce`、`…TokenUnknownNodeIs404`、`…ConfigDefaults`、`…ConfigPutCAS`、`…PatchRequiresCAS`、`…DeleteRequiresCAS`、`…ConfigBatchPartialSuccess`、`…GetNamespaceContext`、`…Upgrade`、`…ListEmptyIsArray`、`…RoutesAreSessionWrapped` | 登记不回 token、**明文只回一次**+轮换后旧 hash 失效、404、config wrapper 默认/CAS、PATCH/DELETE CAS、批量部分成功、`?namespace=` 上下文、升级分支、空列表 `[]` |
| `namespace_handlers_test.go`（12） | `resource_namespace_test.go`（9） | `TestNamespaceResourceListCreateGet`、`…CreateDuplicateSlug`、`…PatchCAS`、`…DeleteCAS`、`…ConfigRoundTrip`、`…SystemConfig`、`…Stats`、`…SpecSwitchRejected`、`…RequiresSession` | 命名空间 CRUD、重复 409、CAS、ns 配置 revision 往返（不影响节点 configRevision）、system retention、stats 不跨 ns、`seedBuiltinCapabilities` 开关被拒 |
| `console_test.go` 的 task/trace/event 段、`console_events_page_test.go`（9）、`merge_fastpath_test.go`（5） | `resource_task_test.go`（15） | `TestTaskResourceListEmptyIsArray`、`TestTaskResourceGetEnvelopeFacts`、`TestTaskResourceDownstreamMissingShape`、`TestTaskResourceCancelUnknownIs404`、`TestTaskResourceCancelPendingBecomesCancelled`、`TestTaskResourceCancelRunningRecordsIntent`、`TestTaskResourceAbandonPendingBecomesAbandoned`、`TestTaskResourceLogsParamBounds`、`TestTaskResourceLogsNoArchive`、`TestTaskResourceEmissionsEmptyShape`、`TestTaskResourceEmissionsShapeAndCursor`、`TestTaskResourceConsoleEventsNamespaceScopes`、`TestTaskResourceEventsAndTrace`、`TestTaskResourceListFilters`、`TestTaskResourceRoutesAreSessionWrapped` | Task 只读信封 / failed-attempt0(S1/E1) / cancel+abandon 终态 / logs 参数界与无档案语义 / emissions 游标 / console events 跨 ns 与 cursor 绑定 / trace 视图 / 列表过滤 |
| — （新增集成层） | `api_integration_test.go`（7） | `TestAPIIntegrationRouteTableIsV4Only`、`…EnvelopeDiscipline`、`…CASRejectsABA`、`…NamespaceIsolation`、`…FailedAttemptZeroTask`、`…NodeTokenOneShot`、`…NodeConfigRevision` | 六域 register 共存无 pattern 冲突、v3 扁平路由**必须 404**、四键/`status` 非 null/`items:[]` 横向一致、CAS ABA、slug→id 与跨 ns 隔离、cursor 绑 ns、E1 回执、token 一次性、configRevision 前进 |
| `emission_handlers_test.go`（4）、`task_logs_test.go`（9）、`task_logs_wire_test.go`（1） | 同上 `resource_task_test.go` | 见上 | emissions 形状/跨 ns 不可见、logs 零字节可用/降级不 5xx/每次请求打节点 |
| `internal/api/events_test.go`（18） | `internal/api/events_test.go`（7） | `TestEventIngressReceipt`、`…TypeFallsBackToSourceName`、`…Duplicate`、`…SourceRequired`、`…Unauthorized`、`…NamespaceScope`、`…DisabledSource` | 回执五字段/`tasks`/`pruned` 恒数组、同 `id` 去重回原 event/trace、source 必填 400、Bearer 鉴权、跨 ns 边界、停用源拒绝 |

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

### 4.4 v3 部署写入口（7 个用例）

| 文件 | 退役 Test 名 | 理由 + 替代 |
|---|---|---|
| `deploy_test.go`（9） | `TestCreateCapabilityWithRunCompilesArtifact`、`TestCreateCapabilityWithInvalidRunRejected`、`TestDeployHandlerCapability400BuiltinNotDeployable`、`TestDeployUnknownDigest404ArtifactNotFound`、`TestDeployToOfflineNodeCreatesRow`、`TestListDeploymentsFilters`、`TestDeleteDeploymentRemovesRow`、`TestDeployRequestValidation`、`TestCapabilityTemplates` | v4：部署只经 `POST …/capabilities/{name}/deploy`（写 `deployments` 事实），**没有** `POST/DELETE /v1/deployments`、`GET /v1/artifacts`。映射：部署到离线节点仍建行 → `TestCapabilityResourceDeployToOfflineNode`；未知节点 404 → `TestCapabilityResourceDeployUnknownNode`；模板 → `TestCapabilityResourceEdgesAndTemplates`（含 `/v1/capability-templates`）；「按 run 编译 artifact / 非法 run 拒绝」→ `TestCapabilityResourceWriteValidation` + store 层 artifact 编译测试 |

### 4.5 v3 中央执行域与派发预算（3 个用例）

| 文件 | 退役 Test 名 | 理由 + 替代 |
|---|---|---|
| `console_handlers_test.go`（4） | `TestCancelTaskNudgesSweeper`、`TestCancelRunningTaskFreesSlotAndNudges`、`TestRedispatchHookNoBackoffOnCapacityShortage`、`TestRedispatchHookBacksOffWhenNoNodeDeclaresCapability` | `lease.Sweeper.Redispatch` / `.Nudge` / `db.SetNotBefore` 在 v4 **已删除**（重派由 `ws.Hub.RunDispatchLoop` + `store.ProposeTaskAssignment`）。cancel 的「终态 + 停止授权」→ `TestTaskResourceCancel*`；「槽位释放后立即重派」的语义归 `store.ReclaimStaleSlots`/`ws` 包测试（本 issue 只读） |
| `dispatch_budget_test.go`（3） | `TestDispatchBudgetDefaults`、`TestDispatchBudgetEnvParsing`、`TestValidateRejectsBadDispatchBudgets` | v4 `route.Router` 没有 `MaxHops` / `TraceTaskBudget` 字段（`internal/route` 已无 hop 预算），env 解析的对象不存在了。`Config.MaxHops/TraceTaskBudget` 字段保留为兼容配置（默认 16/512，`internal/server/config.go` 的包内常量），但不再传给 route |

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

### 4.9 v3 命名空间 CRUD 的遗留口径（3 个用例）

| 旧用例（`namespace_handlers_test.go`，12→9） | 处理 |
|---|---|
| `TestNamespaceListIncludesResourceCounts`、`TestNamespaceListRequiresSession`、`TestNamespaceCreateRejectsInvalidSlug`、`TestNamespaceCreateDuplicateSlugConflicts`、`TestNamespacePatchRenamesNameOnly`、`TestNamespacePatchUnknownIs404`、`TestNamespaceDeleteLastIs400`、`TestNamespaceDeleteBlockedListsBlocking`、`TestNamespaceDeleteUnknownIs404`、`TestStatsRequiresNamespaceId`、`TestStatsWithNamespaceCountsOnlyThatNamespace` | 迁移到 `TestNamespaceResource*`（见 §3） |
| `TestNamespaceCreateSeedsBuiltinCapabilities` | **退役**：`spec.seedBuiltinCapabilities` 是 v4 明确移除的开关（api-conventions §9「明确移除」），内置能力播种随中央执行域一起退役（D0.2）。替代：`TestNamespaceResourceSpecSwitchRejected`（该开关在写请求里必须是 400 `unknown_field`，而不是被静默忽略） |
| `TestNamespaceAllWriteRejected`（在 `console_test.go`） | 迁移：`namespace=*` 只允许读 → `TestNamespaceResourceRequiresSession`/`TestAPIIntegration*` + `namespace_scope.go` 的 `allowAll` 判据 |
| `TestListAcrossNamespaces`、`TestNamespaceUnknownIsNotFound`、`TestNamespaceNotFound`、`TestNamespaceRequired` | 迁移到 `TestAPIIntegrationRouteTableIsV4Only`（v3 路径 404）+ `TestNamespaceResource*`（v4 路径语义） |

## 5. 新增用例清单（79 个：server 72 + api 7）

```
internal/server/resource_capability_test.go  11  TestCapabilityResource*
internal/server/resource_source_test.go       8  TestSourceResource*
internal/server/resource_playbook_test.go    10  TestPlaybookResource*
internal/server/resource_node_test.go        12  TestNodeResource*
internal/server/resource_namespace_test.go    9  TestNamespaceResource*
internal/server/resource_task_test.go        15  TestTaskResource*
internal/server/api_integration_test.go       7  TestAPIIntegration*
internal/server/node_upgrade_test.go          4  TestNodeUpgrade*
internal/server/task_out_retired_test.go      2  TestTaskEndpointsExposeNoOut / TestTraceEndpointExposesNoOut
internal/api/events_test.go                   7  TestEventIngress*
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
| `internal/push.CentralNotifier` 已成死代码 | 后续清理（无 owner） | 它原由中央执行器调用；v4 保留 VAPID 装载与公钥下发（`GET /v1/vapid-public-key`）、devices 域不动，但 Sender 构造路径不再被 server 引用。**未删除**：删除它属于 `internal/push`（只读）且不在本 issue 功能范围 |
