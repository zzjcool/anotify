# T1 测试迁移映射（进行中，不代表全量验收）

基线0e0ff0f；父D1-D6与CONTRACT/WIRE-HANDOFF权威。旧in-package store tests目前保持HEAD原样（唯一例外纯executor builtin_test.go已退役）；G1-6统一闭包。未经具名映射不得全删/skip/弱断言；auth/passkey/device/push/通知回归不退役。

## 已交付新测试域

| 范围 | 新文件/用例 | 状态 |
|---|---|---|
| v4产物 | artifact/*_test.go；FoldKeyExcludesVersionNextDisplayName/DigestIgnoresNextAndDisplayName/ExplicitResultTTLZeroDiffersFromOmitted/DefaultsNormalized/files-env-workDir等 | G1-1 checkpoint，两包181 run，非全仓绿 |
| 新库 | store/v4test/{migrate,schema_baseline}_test.go；新建/重开/旧库拒绝/列集合/新DDL独立校验 | G1-2 checkpoint |
| 能力事实 | store/v4test/{capability,capability_spec}_test.go；显式同版不可变/foldKey复用/next原地更新+事件/digest稳定/往返 | G1-2 checkpoint |
| 配置/node/边 | store/v4test/{config,cluster_node,edges,namespace}_test.go；三层/limits/CAS/授权交集/资格reason/NodeCount/边投影 | G1-3 checkpoint |
| task/lease/emit | store/v4test/{task,emit}_test.go；grant/stale/取消吊销/reaped/非幂等/同txnEmitAuthorized | G1-4 checkpoint；freshreview指出竞态与覆盖须G1-4R修，不当最终通过 |
| event/GC/source | store/v4test/{event,retention,source,stats,deployment}_test.go + sourcespec/spec_test.go；delivery独立窗/引用保留/receipt不自动清/三kind/template/token | G1-4 checkpoint，共113 PASS；stableSourceID/并发等G1-4R补 |
| 纯目录包 | playbook/{parse,render,plan,receipt}_test.go；TestPlanMatrix row01..row12/TokenDiffersIsMasked/SecretOmittedIsRealDifference/RenderTypeFidelity/StatusStripped/ValuesDigest/InstallsFoldKey | G1-5a c654058，54 tests/122 run；store事务尚未实现 |
| 人工deploy协议/env/hash | protocol T3N1具名tests | 父授权原样19827b3；T1不原创改协议 |

## 唯一已退役旧文件

`internal/store/builtin_test.go`：v3内置notify-phone/origin/central claim/播种专属，production builtin.go已删；G1-6须从基线提取每个Test名称并确认非executor旁支仍有新覆盖，不能以整文件名代替逐条最终映射。独立auth/device/VAPID/push API未退役。

## 具名旧→新待G1-6核对（review事实，尚未删除旧测试）

- CapabilitySameVersionDifferentContentRejected/VersionAppendOnly/EmptyListIsArray → v4test/capability_test.go同名；Lifecycle默认 → artifact TestDefaultsNormalized + v4test TestCapabilitySpecDefaultsExplicitEquivalent。
- CapabilitySpecRoundTrip/CapabilityVersionConflict → v4test/capability_spec_test.go同名；旧Handler/迁移9专属 →退役并具名登记。
- Transition/TerminalImmutable/TransitionCASAgainstConcurrency →新TaskGrant/Complete/Cancel竞态测试（R待补）；ExtendLease→TestTaskRenewLease；SweepLeases→Recover/MaxAttempts总尝试；Cancel→PendingIsTerminal/RevokesAuthorization。
- NodeCreateAndGet→TestNodeLifecycle；LeaseInstanceAndRelease/UpsertInstancesKeepsBusyTask/FreeSlots→TestNodeInstancesAndLeases；NodesForCapability→TestEligibleNodesReasons；Rehello覆盖R待补；NodeTagsRoundTrip为**保留功能**，不能因旧NodePins退役一并删。
- SourceCronParsing/Next→sourcespec TestNormalizeCron/TestNextCronTime；DueCronSources R待补；CRUD/KindImmutable/WebhookToken→v4test source对应；targets.params R往返待补。
- EmissionRoundTrip/InsertEmissionIdempotent→TestEmitIdempotentReplay/IDConflict；旧Seq自动去重→WithoutIDAlwaysNew/NotMergedAcrossTasks；EnsureTaskTokenConcurrent→唯一grant token并发（R/G2待补）。
- EventIdempotency→DeliveryWindowIdempotent/Permanent/ConcurrentSameWindow；CreateTasksAtomic/Fanout→R emit rollback+child、TaskListRoundTrip；ReportResultWritesTerminalState→Complete与旧attempt拒绝（R补）。
- RetentionDeletesExpired/KeepsNotYetExpired/KeepsNonTerminal→TaskTTL/EventTTL；新增D6→KeepsEventsReferencedByTasks/SweepsDedupRecordsNotEntities/DoesNotTouchPlaybookInstalls/Primitive引用保护。
- CreateDeploymentIdempotent/DeploymentsForNode/MarkDeployedDigest→v4test DeployCreatePending/DeliveryForNode/Reconcile；旧artifacts镜像表→退役（artifact内容身份测试保留）。
- ParseMigrations/-Gap/-FailureRollsBack/hook rollback→G1-6包内必须恢复白盒，不能用公开面重开幂等完全替代。
- TableSet/NoHistoricalBaggage/Indexes/Unique/Reopen/LegacyDatabaseRejected→v4test schema/migrate对应；旧migration专属退役。
- Playbook旧27例→G1-5b真实事务12格/dryRun/receipt历史/secret脱敏再映射，纯PlanMatrix不替代DB0写测试。

## 保留功能回归（G1-6/G3/G4硬约束）

store/auth_test.go、authz_test.go、passkey_backup_test.go、passkey_mgmt_test.go、devices_update_test.go、password_test.go、admin_test.go；internal/auth全部tests、internal/push/filter_test.go；server passkey/enroll/password/device/CLIauth/statics等不得删。不把旧store_testutil无法编译作为删这些tests理由。

## 新覆盖缺口（需修，已指派G1-4R/后续）

- cancel-vs-grant与已取消幂等leaseexpiry不再授权；drain存量ACK/no新grant；预算槽泄漏；旧attempt与过期result拒绝。
- emit ns交集拒绝/同id并发一事实/child失败回滚/终态拒绝；相同id不能绕旧授权。
- D2 task ns snapshot/revision钉住+跨node新机器值；stableSourceID同名删重建去重不混。
- source.params/parentTaskId/rehello/cron due具名往返与行为。
- CAS资源写/G3 Node根config同事实原子版本，N1共享hash与ParamEnvName落调用单源。

最终必须 go test ./... -count=1 + make contract + make e2e-quick真实通过，父fresh review/跨队全量E2E后方能完成。

## S/R2 checkpoint新增（21253ed，外部161PASS，不等全仓）

TestAttemptSnapshotPinnedPerAttempt / TestGrantConsumesAcceptedSlot / TestAssignmentWindowBoundedAndIdempotent / TestRejectSlotUnavailableNoAttempt / TestHistoricalReapRequiresExactBinding / TestRehelloIsNotProcessFact / TestTerminalPhaseIsNotProcessFact / TestBudgetShrinkKeepsLiveOccupancy / TestMachineConfigHashSharedAndNormalized / TestTaskAttemptJSONShape / TestRetentionKeepsUnreapedAttempts / TestActiveTaskNeverGC。
旧R复审的3major由S处理（grant同txn机器snapshot、历史attempt具体槽reap、不phase推断），但尚需fresh代码复审，不能把161PASS当major已独立清零。

## 最终checkpoint真实红门禁（6040f7c；日志verify/）

- go test ./... exit1：旧store inpackage fixture编译、route/lease/WS/server/agent/executor待迁移；**internal/auth TestRegisterWithPassword_Success password_test.go:438**仍断言退役builtin播种。必须迁移仅该退役断言，保留此密码注册测试及所有真实auth/passkey/device回归，不整文件删/skip。
- make contract exit2：旧checker强制content_hash/node_capabilities；恢复按v4 digest/投影/统一OpenAPI验证，不关checker。
- make e2e-quick未跑（runtime/harness/T3未集成）；原最终门禁完整保留。
- 新G1-6C1格式反例待补：v3 schema_version1/无版本/v>1/未知format/marker4半写/新库重开/拒后数据结构marker保持。父批准marker4+结构校验；实际缺陷migrate.go:178-185、216，不能靠仅baseline1认v4，不新增旧迁移。

## Recovery C1/S3 mapping checkpoint (not full-store/all-Go green)
- C1 459069f fourstagechecks0 and177externaltests; C1-R c573e46 fourstagechecks0 and194externaltests (177+S3seven+10C1R). Originalfixture preserved but exact stronger-boundary rename:
  - TestFormatBaselineFailureRollsBack -> TestFormatCollidingBaselineViewRejectedBeforeInitialization.
  - TestFormatLedgerWriteFailureRollsBackBaseline -> TestFormatCollidingLedgerViewRejectedBeforeInitialization.
  These nowassert existingview refusalbeforeinitialization/dataDDLmarkerpreservation; NOT actualmidtxnrollback. WhiteboxDDL/ledger/marker injectedrollback remainsG1-6TESTpending.
- C1-R added namedrealSQLite TestFormatExistingUserObjectsRejected, TestFormatEmitIdentityRequiresExactUniqueKey/EquivalentUniqueIndexAccepted, TestFormatMarkerAllBaselineColumnsRequired/IdentityConstraintsRequired/ForeignKeysRequired/MalformedLedgerRejected, TestFormatUnmarkedMalformedLedgerUnsupported, TestFormatSnapshotDetectsSameCountOverwrite, TestSchemaBaselinePrimaryKeyIdentity. Literalprefix/allobjects/fullPKFK/typedrowpreservation tested; strictfreshpredicate IS(NOTNULL) residualnotgreen, C1-R2actualOpenredgreenpending.
- S3 bd69177 adds TestGrantRechecksNodeAuthorization, TestStaleGrantAndRejectPreserveNewAssignment, TestExpiredAssignmentCannotRefreshDeadline, TestRenewLeaseCannotReviveExpiredAuthorization, TestRejectAssignmentRacePreservesNewAssignment, TestUpsertInstancesPreservesOccupiedHistory, TestRetentionProtectsOldUnreapedAttemptAfterCurrentReaped. ExistingHistoricalReapRequiresExactBinding strengthenedcurrenttoken+realold/currentEmitAuthorized; TaskAttemptJSONShape decodedallowedkeys+markers. Actualredgreen and4stage0; latepromptabortedfinalreceipt honestlyreplacedbyindependentfixedmechanicalclose acceptedverified.
- S3 strictgate stillpendingcurrentNodeCredentialtransactionAuthorizedEntry, threebudgets and D2taskcreate-beforegrantnamespacechange/midattemptMachineLimitschange discriminators. Do not mapthe7newtests to these absentcounterexamples. C3 ownsremainingimplementation/tests before G2.
- C2 firstattempt correctlyblockedbeforewrites/tests becauseSourceDisplayName absentfield/read-onlyschema; parentauthorisedexactsourcefield/baseline+requiredlayout/scanreadwrite/roundtrip+CAS amendment, no testsornewAPImethodyet. Original194proofretainednotC2claimedPASS. Oldinpackage tests/retainedauth etc stillpendingnamedmigration.

## C2-R scoped closure e42fe84
Original216 retained; 218total, actualfixedfourstage0/full23.285s. AddedTestResourceCASCapabilityLowerVersionCreateKeepsLatestProjection ordinaryhigh3->low1 actualred1->green0, oldhighestSpec/RV/edges unchanged. AddedTestResourceCASNodeEqualRevisionRecreateABA threeindependentpaths withold/newrevision1/differentUID: fullcurrenttoken409+root/configunchanged; UID-onlycomparisonmutantactualFAIL1/all3paths, exactresource_version.goSHArestore6f8ae20465408882c8fb0a166c0bc2cb9844a8b12ec06c0663dfc344e36def2a. Freshscout12(M1)/13(M2)fixede42oktrueacceptedattested; closesonlyC2scopedproductionprojection/coveragefindings. Originaloldinpackage andC3/G2-G4 stillpending; notfullGo/contract/e2equickPASS.


## NEXT-G1TEST partial implementation gate — NOT CLOSED

Frozen launch HEAD `0ed7bfc1153673f6a64cd36b14c623191f9be389`.

**implementationGate: false.** 本轮只完成注册退役指向；未验证的 store 迁移/白盒草案已回滚；未恢复所有原 store in-package compile/语义，不能交付 G1，不能声称 whole T1 DONE。`v4test` PASS 仅说明外部包现有测试仍绿，绝不是 original 闭包替代。

### 改动与测试覆盖
- `internal/auth/password_test.go`：仅 `TestRegisterWithPassword_Success` 末尾的 builtin assertion 区域改为真实注册 namespace 授权和脚本 Spec 能力创建/UID/digest/正文往返；其 PHC、用户、recovery、session、adminrole、错密码断言均保留；其它 auth tests 未改。该同名测试红→绿。
- `internal/store/migrate_test.go` 草案曾迁移旧 baseline 列断言/退休纯旧 migration 2/11 链；原包 compile 尚未闭合，按照失败止损要求**回滚到 launch HEAD 原样**，patch 仅保存在 `/tmp/anotify-core-next-g1tests-migrate-draft.patch`，不是本次已交付修改。表中这些迁移是后续建议/待迁移，不是已退役。
- `internal/store/v4_whitebox_test.go` 草案有 7 个内部测试，直接调用真实 transaction helper、用局部 ABORT trigger 注入 ledger SQL 失败，无生产 testhook。未编译/执行，**已从 checkout 回滚删除**，仅保存在 `/tmp/anotify-core-next-g1tests-v4-whitebox-draft.go` 供下一 serial writer 参考；不能计新增已交付测试或 PASS。
- `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md`：唯一报告/逐名清单位置。回滚后本轮没有改变任何 store tests 或 production/schema/Node/UI/protocol/artifact/push 文件；manager 的 `NEXT-PROGRESS.md` 修改不是本 writer 产物，不纳入提交。

未交付白盒草案名（**rolled back, not compiled/executed**）：
1. `TestV4BaselineDDLFailureRollsBackMarkerAndLedger`
2. `TestV4BaselineLedgerWriteFailureRollsBackAllDDL`
3. `TestV4BaselineHookFailureRollsBackMarkerAndData`
4. `TestV4MigrationLedgerWriteFailurePreservesCheckpoint`
5. `TestV4ParseMigrationsContinuousLedger`
6. `TestV4MigrationClosedConnectionFailsWithoutSuccess`
7. `TestV4MigrationCancelledContextDoesNotChangeLedger`

### 实际固定 3 commands（store 草案回滚前；不是最终提交绿灯）

| command (all explicit absolute cd this WT, logs have pwd/HEAD/cmd/exit) | actual exit | result |
|---|---:|---|
| `test -z "$(gofmt -l internal/store internal/auth/password_test.go)"` | 0 | PASS |
| `env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/... ./internal/auth/... ./internal/push/...` | 1 | FAIL, original store compile |
| `env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/... ./internal/auth/... ./internal/push/... -count=1` | 1 | FAIL original store build; v4test 34.654s/auth 20.790s/push 0.006s PASS |

日志：`/tmp/anotify-core-next-g1tests-{vet-initial,password-red,password-green,format-final,vet-final,packages-final}.log`。
命令均 `timeout 60` 有界；未用分文件编译、删用例、fake DTO/alias、套件切分冒充门禁；未运行已知 planning full Go baseline。没有 named async 工具注册；本次 fixed scoped test 实际在 60s 内完成，不是 pendingextension 或 fake selfpassed。

### 阻塞 / 已尝试 / 下一步

- 第一次真实 fixed vet 明确报 original `capability_links_test.go:59 ArtifactBuilder`、`playbook_test.go:17/51/67/77 Playbook/CapabilityRef/PlaybookChecks/PlaybookCheckItem`、`reconcile_test.go:49 AppliedRef`、`session_lease_safety_test.go:50/56 Lifecycle` undefined。
- 准备上述 store 草案后第二次 fixed vet 仍报同一批 compile blockers（原样日志在下）；full fixed packages 亦确认同样 build failed。没有第三次重复猜测，未删除这些测试族、未编造 legacy aliases。
- 当前假设：这些是未迁移的原 fixture/API 测试族，不是产品实现错误；本 writer 没有完成任务，不能将 known API mismatch 当 productbug 停报证据。
- 建议：**继续同一 test-only 路径**，下一 serial writer 从保留的逐名库存推进 `capability_links/batch -> v4 immutable create+next CAS`、`playbook -> actual raw files/render/12 cells/receipt/dryRun`、`reconcile -> actual AppliedCapability`、`session/task -> known minted credential GrantLeaseAuthorized/attempt snapshot/lease`，再闭合剩余 Node/source/event/retention/stats/IDs/bench fixture。先完成 actual in-package compile 才能恢复/执行这些白盒草案；保持断言不弱化，真实产品红测试才另派 serial productionfix。
- C3b accepted minor mixed samehash first-expired/download/live-copy 未改产品，仍由未来 G3 精确测试处理。
- G2 runtime、G3 API/download/Node E2E、G4 上层回归，以及最终 `go test ./... -count=1` / `make contract` / `make e2e-quick` 均 pending；本次不推进上层 gate。
- 本次仅 auth+mapping 为已验证的局部 checkpoint；store 两草案未通过轻量门禁，回滚到上个检查点，日志保留。未 push/merge/MR（冻卡 NoPushMergeMR）；这是未通过验收的 partial checkpoint，不是 MR 可合入交付。

### 原用例逐名库存 / 迁移状态

从当前仓库现存 Git 对象 `git ls-tree -r 0d4b975 internal/store` / `git show 0d4b975:<file>` 提取，仅 root 原 in-package `_test.go`（包含 benchmark compile 库存），没有读取其它 WT 或历史 session。`pending` 明确表示尚未迁移，不能当 kept/PASS 或整文件 low-value 删除；builtin 已在旧 checkpoint 删除的每个名字仍逐条登记。新版其它 C3b/v4test tests 全部原样保留。


#### `internal/store/admin_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestUser_RoleRoundtrip` | kept-source, pending | 同名源码保留；执行 pending |
| `TestUser_DisabledRoundtrip` | kept-source, pending | 同名源码保留；执行 pending |
| `TestUserCount` | kept-source, pending | 同名源码保留；执行 pending |
| `TestListUsersWithStats` | kept-source, pending | 同名源码保留；执行 pending |
| `TestSystemStats` | kept-source, pending | 同名源码保留；执行 pending |
| `TestListGlobalEvents` | kept-source, pending | 同名源码保留；执行 pending |
| `TestListAllSessions` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/auth_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestUser_CreateGet` | kept-source, pending | 同名源码保留；执行 pending |
| `TestPasskey_CRUD` | kept-source, pending | 同名源码保留；执行 pending |
| `TestSession_CRUD` | kept-source, pending | 同名源码保留；执行 pending |
| `TestAPIKey_CRUD` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/authz_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestAuthorizeNamespace` | kept-source, pending | 同名源码保留；执行 pending |
| `TestAuthorizeNamespaceForNode` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/bench_dispatch_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `BenchmarkDispatchChain` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBenchmarksTargetRealPaths` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/bench_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `BenchmarkCreateTasks` | v4rewrite pending | 同名待 actual API 迁移 |
| `BenchmarkTransition` | v4rewrite pending | 同名待 actual API 迁移 |
| `BenchmarkReportResult` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/bench_writeamp_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `BenchmarkCreateTasksWriteAmplification` | v4rewrite pending | 同名待 actual API 迁移 |
| `BenchmarkCreateTasksSingleRowTx` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/builtin_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestSeedBuiltinCapabilitiesIsIdempotent` | retired | 中央 builtin 播种已删除；真实 namespace/user 注册保留，TestRegisterWithPassword_Success setup 已绿 |
| `TestBuiltinCapabilityIsCentral` | retired | 纯 v3 central capability kind/播种 |
| `TestNotifyPhoneStillCentral` | retired | 纯 v3 notify.phone 中央执行 |
| `TestCentralCapabilitiesAreSpecDriven` | retired | 纯 v3 central handler 分发；实际脚本 Spec 合法性由 TestCapabilitySpecRoundTrip/InvalidRejected 保留 |
| `TestNotifyPhoneParamsSchemaRequiresTo` | retired | 纯 builtin notify.phone schema；独立 device/push/tag/VAPID 不退役 |
| `TestClaimCentralTasksRunsStateMachine` | retired | 纯 v3 central executor claim；真正 v4 grant/attempt 语义仍待原包迁移 |
| `TestFinishTaskWritesOutcome` | v4rewrite pending original | 中央 FinishTask 已退役；结果事实对应 v4test TestC3bCompleteResultFullJSONNanoseconds/TestC3bCompleteResultFullFactsDiscrimination；不能以此宣称 original closed |
| `TestFinishTaskRejectsInvalidStatus` | v4rewrite pending original | 中央 FinishTask 已退役；错误相位/回滚对应 TestCompleteTaskActualResultRefusalsZeroFacts；原包新结果回归待闭合 |
| `TestNoOriginShellCapabilities` | retired | 纯 v3 origin/中央 shell builtin 名称清单 |
| `TestEntryEdgeAcceptsAnyPort` | retired | 纯 v3 source entry direct port API；实际 source targets 投影由 TestSourceEdgesProjection 保留 |
| `TestBuiltinDisplayNames` | v4rewrite pending original | 播种实体退役；真实能力 displayName 对应 TestCapabilityDisplayName/TestCapabilityFoldKeyIgnoresNextAndDisplayName，原包现同名尚待迁移 |
| `TestBuiltinDisplayNameBackfill` | retired | 纯 builtin displayName 历史回填 migration |
| `TestBackfillBuiltinCapabilitiesCoversEveryNamespace` | retired | 纯 v3 builtin 跨 namespace 回填 migration；用户/namespace 注册及隔离断言未删 |
| `TestBackfillBuiltinCapabilitiesEmptyDeployment` | retired | 纯 v3 builtin 回填空部署 migration |
| `TestExampleBuiltinScriptEmitsDeclaredPort` | retired | 纯 builtin 示例脚本/旧出口 port env 与 emit CLI 协议；现 Node/artifact 命令由上层 G2/G3 保留，不在此改 |
| `TestExampleBuiltinIsUsable` | v4rewrite pending original | 播种机制退役；真实 Spec/digest/幂等对应 TestCapabilitySpecRoundTrip/TestCapabilityIdempotentCreate/TestDeployDeliveryForNode；原包能力/部署族待迁移 |
| `TestExampleBuiltinArtifactUpdateRepointsDeployment` | retired | 纯 builtin 同版强制改内容+历史 migration 重播；v4 同版内容不可变。一般部署 reconcile 仍由 TestDeployReconcile/TestDeployNewDigestSupersedes 保留 |
| `TestExampleBuiltinContentDerivedNotBackfilled` | v4rewrite pending original | BackfillBuiltin 已退役；派生产物正文/digest 仍对应 TestCurrentArtifactIsCapabilityContent（原包待迁移）/TestDeployDeliveryForNode |

#### `internal/store/capability_batch_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCreateCapabilitiesBatchRejectsEmptyAndDuplicate` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilitiesBatchRejectsMisalignedBuilders` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilitiesBatchLinksAcrossBatch` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilitiesBatchRollsBackOnBadLink` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilitiesBatchArtifactBuildFailureRollsBack` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilitiesBatchMergesBreakingImpact` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestMergeUpgradeImpactDedupAndSort` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/capability_links_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCreateCapabilityWithLinksCreatesAll` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilityWithLinksRollsBackOnBadLink` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilityWithLinksArtifactBuildFailureRollsBack` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilityWithLinksArtifactInTx` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilityWithLinksIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilityWithLinksConcurrent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateCapabilityWithLinksUpgradeGateStillGate` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/capability_spec_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCapabilitySpecRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilitySpecInvalidRejected` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityArtifactDerivedNotPersisted` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityVersionConflict` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBuiltinHandlersAreKnownHandlers` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBuiltinNotifyPhoneHasNoArtifact` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestMigration9Backfill` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestMigration9BackfillRejectsInvalidLegacyRow` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSpecBackfillHelperShapes` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityCreateContentAddressed` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/capability_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCapabilityCreateAndGet` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityIdempotentCreate` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilitySameVersionDifferentContentRejected` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityOutputsOrderChangesHash` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilitySchemaKeyOrderInsensitive` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityDifferentContentDifferentHash` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityNormalizeIncludesName` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityVersionAppendOnly` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityNamespaceIsolation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityEmptyListIsArray` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityEmptyOutputs` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityValidation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLifecycleIdempotentDefault` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestGraphVersionOnDemand` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestGraphVersionChangesWithEdges` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestGraphVersionEmptyGraph` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgeRejectsPortNotInOutputs` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgeRejectsCrossNamespace` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgesFromLookup` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListEdgesEmptyIsArray` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityDisplayName` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestContentHashEqualsDigest` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/cliauth_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCliAuthSession_RoundTrip` | kept-source, pending | 同名源码保留；执行 pending |
| `TestCliAuthSession_ApproveDeny_AtomicConflict` | kept-source, pending | 同名源码保留；执行 pending |
| `TestCliAuthSession_ExpiredMigration` | kept-source, pending | 同名源码保留；执行 pending |
| `TestCliAuthSession_ConsumeAndCreateKey` | kept-source, pending | 同名源码保留；执行 pending |
| `TestCliAuthSession_DeleteExpired` | kept-source, pending | 同名源码保留；执行 pending |
| `TestCliAuthSession_UserCodeUnique` | kept-source, pending | 同名源码保留；执行 pending |
| `TestCliAuthSession_NotFound` | kept-source, pending | 同名源码保留；执行 pending |
| `TestCliAuthSession_PasskeyEnroll_RoundTrip` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/deployment_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCurrentArtifactIsCapabilityContent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestArtifactAbsentCases` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestArtifactNamespaceIsolation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateDeploymentIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateDeploymentRejectsMismatch` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestDeleteDeploymentRemovesRow` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListDeploymentsFilters` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestDeploymentsForNodeDerivesFromCapability` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestMarkDeployedDigestUsesCapabilityDigest` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestDeployProtocolShapeUnchanged` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestDeploymentsForNodeSkipsHandlerCapability` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestArtifactRequiresRun` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/devices_update_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestUpdateDevice` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/downstreams_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestReplaceCapabilityDownstreams` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReplaceCapabilityDownstreamsLeavesOtherPortsAlone` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReplaceCapabilityDownstreamsIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReplaceCapabilityDownstreamsValidation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReplaceCapabilityDownstreamsNamedPort` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/edge_cycle_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCreateEdgeAllowsSelfLoop` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeAllowsCycle` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeAllowsDiamond` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeEntryEdgeNeverCycles` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/edge_literals_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEdgeLiteralsRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgeLiteralsDefaultEmpty` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeLiteralsRejected` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeLiteralsNullIsAbsent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeLiteralsAccepted` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeLiteralsIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeLiteralsEntryEdge` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestGraphHashIncludesLiterals` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/edge_mapping_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEntryEdgeMappingRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityEdgeMappingRejected` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgeMappingUndeclaredKeyRejected` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgeMappingNullNormalizedToEmpty` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgeMappingShapeRejected` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEntryEdgeWildcardPortLookup` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestGraphHashIncludesMapping` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/edge_refs_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestLinkRefFollowsLatest` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLinkRefPinned` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLinkRefAcceptsLegacyID` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeRejectsUnresolvableRef` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgeRefCycleByNameAllowed` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgeRefValidationUsesResolvedVersion` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestGraphHashStableAcrossUpgrade` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/edge_shape_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCapabilityEdgeShapeReadsPortsAndInbound` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityEdgeShapeIsolatesNamespaces` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityEdgeShapeRejectsWildcardNamespace` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/emission_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEmissionRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInsertEmissionIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEmissionSeqStatus` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEmissionListOrderedAndScoped` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestGetEmissionNotFound` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInsertEmissionRejectsBadInput` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEnsureTaskToken` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEnsureTaskTokenConcurrent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTaskTokenColumnMigrated` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEmissionsTableMigrated` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/event_page_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestListEventsBeforeEquivalence` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListEventsBeforeCursorSameCreatedAt` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListEventsBeforeEmptyIsSlice` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListEventsBeforeUsesRetentionIndex` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/event_prune_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestInsertPruneEventIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInsertPruneEventDefaultsAndValidation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListEventsHidesPrunesFromStream` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInsertEventRejectsReservedSource` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/event_task_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEventIdempotency` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEventWithoutIdemKeyAllowsDuplicates` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEventIdemKeyScopedPerSourceAndNamespace` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEventGetAndList` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEventValidation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReportResultWritesTerminalState` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReportedColumnDistinguishesNoResultFrame` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateTasksAtomic` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateTasksFanout` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateTasksRejectsNonPending` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateTasksEmptyIsNoop` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTaskRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListTasksEmptyIsArray` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListTasksFilters` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTaskNamespaceIsolation` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/graph_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEnsureGraphVersionUnchangedTakesNoWriteLock` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEnsureGraphVersionSnapshotRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestGraphSnapshotPayloadMatchesContentHash` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestGraphVersionPathsUseSameNamespace` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/ids_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestBase62EncodeIsRealBase62` | kept-source, pending | 同名源码保留；执行 pending |
| `TestBase62EncodePreservesOrder` | kept-source, pending | 同名源码保留；执行 pending |
| `TestBase62EncodeZero` | kept-source, pending | 同名源码保留；执行 pending |
| `TestNewIDPrefixAndUniqueness` | kept-source, pending | 同名源码保留；执行 pending |
| `TestNewIDTimeOrdered` | kept-source, pending | 同名源码保留；执行 pending |
| `TestCapabilityAndGraphIDShape` | kept-source, pending | 同名源码保留；执行 pending |
| `TestVersionIDUniqueness` | kept-source, pending | 同名源码保留；执行 pending |
| `TestIDHashSuffix` | kept-source, pending | 同名源码保留；执行 pending |
| `TestItoa64` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/migrate_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestParseMigrations` | kept source / pending | 原源码保留；尚未执行 |
| `TestParseMigrationsRejectsGap` | kept source / pending | 原源码保留；尚未执行 |
| `TestSchemaVersionRecorded` | kept source / pending | 原源码保留；尚未执行 |
| `TestMigrationTwoColumns` | pending retirement | 旧迁移链可退休；本轮草案已回滚，原测试仍在 |
| `TestMigrationTenDropsBalance` | pending v4rewrite | 退休列/必需 v4 列基线守卫；本轮草案已回滚，原测试仍在 |
| `TestMigrationIdempotent` | kept source / pending | 原源码保留；尚未执行 |
| `TestMigrationFailureRollsBack` | kept source / pending | 原源码保留；尚未执行 |
| `TestMigrationHookFailureRollsBack` | kept source / pending | 原源码保留；尚未执行 |
| `TestMigrationNoSwallowedErrors` | kept source / pending | 原源码保留；尚未执行 |
| `TestNoArgvOrKindInSchema` | kept source / pending | 原源码保留；尚未执行 |
| `TestMigration11DeploymentsReferenceCapability` | pending retirement | 旧迁移链可退休；本轮草案已回滚，原测试仍在 |
| `TestMigration11RefusesToDropUnmappedDeployment` | pending retirement | 旧迁移链可退休；本轮草案已回滚，原测试仍在 |

#### `internal/store/namespace_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestNamespaceCreateSeedsBuiltins` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNamespaceCRUD` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNamespaceRequirementEnforced` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCrossNamespaceReferencesRejected` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNamespaceRoundTrips` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRegisterUserCreatesDefaultNamespace` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/node_agent_version_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestMigration3AgentColumnsFromV2` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeAgentVersionRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeAgentUpgradeStatusRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeAgentWritesRespectNamespace` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/node_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestNodeCreateAndGet` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeNamespaceIsolation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeCapabilityUnknownIsStale` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeCapabilitiesReplacedOnRehello` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeCapabilitiesNoHarnessColumns` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeMaxInstances` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeInstancesUpsertAndShrink` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestUpsertInstancesKeepsBusyTask` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLeaseInstanceAndRelease` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLeaseInstanceDeadSlotIgnored` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodesForCapability` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodesForCapabilityNamespaceIsolation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodePins` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPutPinValidation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeStatusTransitions` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTouchNode` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeEmptyListIsArray` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeTagsRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNewNodeTokenUnique` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestFreeSlotsByNode` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeCreateDuplicateName` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestValidateNodeName` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLeaseAndAssignWithPinWritesPinAtomically` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLeaseAndAssignWithPinRollsBackPinOnFailure` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRollbackPinOnlyDeletesOwnPin` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRollbackPinSameSecondSameNode` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRollbackPinValidation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestMigration7PinIDWorksOnExistingDB` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/orphan_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestOrphanPendingNoNodeDeclares` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestOrphanPendingNodeOffline` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestOrphanPendingOnlineNodeNotListed` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestOrphanPendingRespectsAgeFloor` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestAbandonPendingTaskReleasesBackpressure` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestAbandonPendingTaskOnlyTouchesPending` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestAbandonPendingTaskDefaultReason` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/passkey_backup_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestPasskey_BackupEligibleRoundtrip` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/passkey_mgmt_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestPasskey_RenameRoundtrip` | kept-source, pending | 同名源码保留；执行 pending |
| `TestPasskey_DeleteRoundtrip` | kept-source, pending | 同名源码保留；执行 pending |
| `TestPasskey_ListEmptyReturnsEmpty` | kept-source, pending | 同名源码保留；执行 pending |
| `TestPasskey_LastUsedAtNullable` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/password_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestUserPassword_DefaultEmpty` | kept-source, pending | 同名源码保留；执行 pending |
| `TestUserPassword_SetGetRoundTrip` | kept-source, pending | 同名源码保留；执行 pending |
| `TestUserPassword_ClearResetsToZero` | kept-source, pending | 同名源码保留；执行 pending |
| `TestUserPassword_MissingUser` | kept-source, pending | 同名源码保留；执行 pending |
| `TestUserPassword_ColumnsExist` | kept-source, pending | 同名源码保留；执行 pending |
| `TestRegisterUserWithPassword_RoundTrip` | kept-source, pending | 同名源码保留；执行 pending |
| `TestRegisterUser_PasskeyPathKeepsPasswordEmpty` | kept-source, pending | 同名源码保留；执行 pending |
| `TestRegisterUserWithPassword_ConcurrentFirstAdmin` | kept-source, pending | 同名源码保留；执行 pending |
| `TestRegisterUser_PreMigration8SchemaStillWorks` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/pending_backlog_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestOldestPendingCreatedAtEmpty` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestOldestPendingCreatedAtIgnoresNonPending` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestOldestPendingCreatedAtIgnoresNotBefore` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPendingBacklogSQLShape` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPendingBacklogIndexQueryPlan` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPendingBacklogIndexIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/playbook_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestPlaybookManifestRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPlaybookCreateGetListDelete` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPlaybookEmptyManifestNormalized` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPlaybookSlugValidation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookSkipsEdgeWithMissingEndpoint` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookSkipsEdgeWithUnknownPort` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookWebhookSourceIsUsable` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookCronSource` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPlaybookChecksNodePresence` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPlaybookChecksPartialAndEntryCapabilities` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookDoesNotOverwriteExistingDefinition` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPlaybookManifestReservedOriginNames` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPlaybookInstallEmptyManifest` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPlaybookChecksResolvesDeclaredVersion` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookNewVersionSameSchema` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookSkipsSourcePointingAtSameEntry` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListPlaybooksCreatedAtOrder` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookInlineSpecStoresScriptBody` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookRefDoesNotCreateCapability` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookRejectsForeignHandler` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookRefAndSpecBothGiven` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookEdgeCarriesLiteralsAndPriority` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookTriggerNameAndMapping` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookLegacyManifestStillInstalls` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestPlaybookChecksHubHandlerRefIsReady` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestInstallPlaybookRejectsUnresolvableRef` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/pool_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestPoolMaxOpenConnsConfig` | kept-source, pending | 同名源码保留；执行 pending |
| `TestPoolReadsRunInParallel` | kept-source, pending | 同名源码保留；执行 pending |
| `TestPoolWriteTxIsImmediateAndDoesNotBlockReaders` | kept-source, pending | 同名源码保留；执行 pending |
| `TestPoolConcurrentWritesNoLockErrors` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMaxConnsFromEnv` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/priority_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEdgePriorityRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestEdgePriorityInGraphSnapshot` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateEdgeRejectsOutOfRangePriority` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/reconcile_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestReconcileAppliedRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReconcileAppliedFullReplace` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReconcileAppliedToleratesStaleCache` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReconcileAppliedListsAllStale` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReconcileAppliedDigestMismatchMarksPending` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReconcileAppliedDigestMatchMarksApplied` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReconcileAppliedFeedsDispatch` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/report_next_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestReportResultAndCreateTasksSuccess` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReportResultAndCreateTasksRollsBackOnChildFailure` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReportResultAndCreateTasksRollsBackOnReportFailure` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReportResultCompatWithoutNext` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReportResultAndCreateTasksChildIsRedispatchable` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/retention_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestRetentionDeletesExpiredTerminalTask` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionKeepsNotYetExpiredTask` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionIsIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionKeepsNonTerminalTasks` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionSkipsCapabilitiesWithoutTTL` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionBatchLimitAndTruncation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionNamespaceIsolation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionEventsDisabledByDefault` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionEventsCustomRetention` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTerminalStatusListMatchesIsTerminalStatus` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionDeletesAllTerminalStatuses` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionDeleteRejectsNonPositiveLimit` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestListRetentionNamespaceIDs` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepRetentionHonoursCancelledContext` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionPartialReportOnCancel` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionEventsNamespaceIsolation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionBadLifecycleDoesNotPoisonSweep` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRetentionIndexesUsed` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/schema_baseline_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestBaselineTableSet` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBaselineNoHistoricalBaggage` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBaselineForeignKeys` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBaselineIndexes` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBaselineUniqueConstraints` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBaselinePrimaryKeys` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBaselineIntegrity` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestBaselineReopenIsIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLegacyDatabaseRejected` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/seq_in_trace_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestSeqInTraceConcurrentAssignIsUnique` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSeqInTraceConcurrentLeaseAndAssignIsUnique` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSeqInTraceGroupsByTraceAndCapabilityName` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSeqInTraceStableAcrossRedispatch` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSeqInTraceRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSeqInTraceZeroUntilDispatched` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestMigration5SeqInTraceWorksOnExistingDB` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/session_lease_safety_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestResolveLostTasksAbandonsNonIdempotentStarted` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestResolveLostTasksAbandonsWhenAttemptsExhausted` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestResolveLostTasksAssignedNotStartedBeatsNonIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestResolveLostTasksCleanEndOfIdempotentStartedStillRequeues` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/session_lease_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestExtendLeasesForTasksOnlyListedIDs` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestResolveLostTasksRequeue` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestResolveLostTasksAbandon` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestResolveLostTasksSkipsStaleGeneration` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestResolveLostTasksAssignedNotStartedRequeuesEvenWhenAbandon` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateTasksRejectsOversizedParams` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReclaimStaleSlots` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/source_entry_edges_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestSourceTargetsRoundTripFromEdges` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestUpdateSourceRebuildsEntryEdges` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestRenameSourceMovesEntryEdges` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestDeleteSourceClearsEntryEdges` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateSourceRejectsBadMapping` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/source_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCronParsing` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCronNext` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCronDomDowOrSemantics` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCronNeverFires` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSourceCRUD` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSourceValidation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSourceWebhookToken` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSourceKindImmutable` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestDueCronSources` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCreateSourceRejectsReservedName` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestUpdateSourceRejectsReservedName` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestValidateVarDefs` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSourceVarDefsParsing` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSourceConfigPayloadParsing` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSourceConfigKeepsVars` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestValidateSourceConfigPayloadOnlyForKeepingKinds` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestValidateSourceConfigRejectsBadVars` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSourceResultSchemaRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSourceResultSchemaValidation` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestUpdateSourceKeepsResultSchema` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/stats_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestMaintainStatsFreshDBIsQuiet` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMaintainStatsSuggestionFilter` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMaintainStatsRefreshesDriftedStat1` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMaintainStatsKeepsQuietWhenStat1IsFresh` | kept-source, pending | 同名源码保留；执行 pending |
| `TestStatsDrifted` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMaintainStatsDoesNotTouchData` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMaintainStatsSkipsDriftCheckWhenTableEmpty` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/stats_tz_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStartOfLocalDayUnix` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMessageStatsTodayUsesLocalMidnight` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMessageStatsDailyUsesLocalDay` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMessageStatsTotalsDeliveryAndDevices` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMessageStatsPublicUsesRealClock` | kept-source, pending | 同名源码保留；执行 pending |
| `TestMessageStatsRejectsEmptyNamespace` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/stmt_evict_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStmtCacheEvictsUnusableStmt` | kept-source, pending | 同名源码保留；执行 pending |
| `TestStmtCacheFullFallsBackToDirectQuery` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/stmt_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStmtCachePrepareGate` | kept-source, pending | 同名源码保留；执行 pending |
| `TestStmtCacheBoundedByMaxCachedStmts` | kept-source, pending | 同名源码保留；执行 pending |
| `TestStmtCacheDoesNotCachePrepareFailure` | kept-source, pending | 同名源码保留；执行 pending |
| `TestStmtCacheErrorTextMatchesDirectQuery` | kept-source, pending | 同名源码保留；执行 pending |
| `TestStmtCacheZeroValueWorks` | kept-source, pending | 同名源码保留；执行 pending |
| `TestStmtCacheUnusableStmtFallsBack` | kept-source, pending | 同名源码保留；执行 pending |
| `TestStmtCacheUnusableDoesNotMatchSqlLogicErrors` | kept-source, pending | 同名源码保留；执行 pending |
| `BenchmarkListTasks` | kept-source, pending | 同名源码保留；执行 pending |
| `BenchmarkListTasksBaseline` | kept-source, pending | 同名源码保留；执行 pending |
| `BenchmarkGetSession` | kept-source, pending | 同名源码保留；执行 pending |
| `BenchmarkGetSessionBaseline` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/stmt_wiring_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStmtWiringAllReadPathsCached` | kept-source, pending | 同名源码保留；执行 pending |
| `TestStmtWiringOptionalFiltersAreDistinctSQLs` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/store_bench_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `BenchmarkListEnabledDevices` | kept-source, pending | 同名源码保留；执行 pending |

#### `internal/store/stub_signature_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStubSignaturesFrozen` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNoStubRemains` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestIsTerminalStatus` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTaskStructJSONShape` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCapabilityHasNoKind` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestNodeHidesTokenHash` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/task_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestValidTransitionTable` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTransition` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTerminalImmutable` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTransitionFinishedAt` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTransitionCASAgainstConcurrency` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTransitionNotFound` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestAssignTask` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestExtendLease` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestExtendLeasesForNode` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepLeases` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepLeasesAttemptsExhausted` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepLeasesNonIdempotentAbandons` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepLeasesIgnoresLiveLeases` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepLeasesAssignedExpires` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepLeasesIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCancelTask` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCancelPendingTask` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReportResultFromAssignedImpliesRunning` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestMarkRunningIdempotent` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestCountTasksByStatus` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepLeasesNoExpiryIsNoop` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLeaseConstants` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLifecycleMaxAttemptsFromJSON` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestStatusWriterIsUnique` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepLeasesGuardAgainstHeartbeatRace` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTransitionGuardFailedSentinel` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepNeverStartedNonIdempotentRequeues` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepRunningNonIdempotentAbandons` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestSweepNeverStartedAttemptsExhaustedAbandons` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTaskDigestRoundTrip` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLeaseAndAssignWritesTaskToken` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestAssignTaskAlsoWritesToken` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestTaskTokenNeverSerializedToJSON` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/tx_merge_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestReportResultMatchesSplitWrites` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLeaseAndAssignMatchesSplitWrites` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestProgressTaskMatchesSplitWrites` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestLeaseAndAssignGuardFailureDoesNotLeakSlot` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReportResultRejectsTerminalTaskWithoutTouchingSlot` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestReportResultDoesNotReleaseForeignSlot` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestProgressTaskRejectsTerminalTask` | v4rewrite pending | 同名待 actual API 迁移 |

#### `internal/store/upgrade_gate_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestUpgradeGateRemovedPort` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestUpgradeGateListsAffectedLinks` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestUpgradeGateAllowsAdditiveChanges` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestUpgradeGateParamTypeChanged` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestUpgradeGateOnlyForLatest` | v4rewrite pending | 同名待 actual API 迁移 |
| `TestUpgradeImpactEmptyOnFirstVersion` | v4rewrite pending | 同名待 actual API 迁移 |

### 验证输出（原样）

`/tmp/anotify-core-next-g1tests-password-red.log`

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
0ed7bfc1153673f6a64cd36b14c623191f9be389
CMD timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/auth -run ^TestRegisterWithPassword_Success$ -count=1
time=2026-11-10T04:14:23.320Z level=INFO msg="first user promoted to admin" event=auth.register.first_admin user_id=usr_BCIH4brknOs1CYv0OU username=pwreguser
--- FAIL: TestRegisterWithPassword_Success (0.41s)
    password_test.go:438: 内置能力没播下去: store: record not found
FAIL
FAIL	github.com/zzjcool/anotify/internal/auth	0.452s
FAIL
EXIT 1
```

`/tmp/anotify-core-next-g1tests-password-green.log`

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
0ed7bfc1153673f6a64cd36b14c623191f9be389
CMD timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/auth -run ^TestRegisterWithPassword_Success$ -count=1
ok  	github.com/zzjcool/anotify/internal/auth	0.404s
EXIT 0
```

`/tmp/anotify-core-next-g1tests-format-final.log`

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
0ed7bfc1153673f6a64cd36b14c623191f9be389
CMD test -z "$(gofmt -l internal/store internal/auth/password_test.go)"
EXIT 0
```

`/tmp/anotify-core-next-g1tests-vet-initial.log`

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
0ed7bfc1153673f6a64cd36b14c623191f9be389
CMD env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/... ./internal/auth/... ./internal/push/...
# github.com/zzjcool/anotify/internal/store [github.com/zzjcool/anotify/internal/store.test]
internal/store/capability_links_test.go:59:30: undefined: ArtifactBuilder
internal/store/playbook_test.go:17:84: undefined: Playbook
internal/store/playbook_test.go:51:71: undefined: CapabilityRef
internal/store/playbook_test.go:67:43: undefined: Playbook
internal/store/playbook_test.go:67:54: undefined: PlaybookChecks
internal/store/playbook_test.go:77:35: undefined: PlaybookChecks
internal/store/playbook_test.go:77:71: undefined: PlaybookCheckItem
internal/store/reconcile_test.go:49:57: undefined: AppliedRef
internal/store/session_lease_safety_test.go:50:25: undefined: Lifecycle
internal/store/session_lease_safety_test.go:56:34: undefined: Lifecycle
internal/store/session_lease_safety_test.go:56:34: too many errors
EXIT 1
```

`/tmp/anotify-core-next-g1tests-vet-final.log`

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
0ed7bfc1153673f6a64cd36b14c623191f9be389
CMD timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/... ./internal/auth/... ./internal/push/...
# github.com/zzjcool/anotify/internal/store [github.com/zzjcool/anotify/internal/store.test]
internal/store/capability_links_test.go:59:30: undefined: ArtifactBuilder
internal/store/playbook_test.go:17:84: undefined: Playbook
internal/store/playbook_test.go:51:71: undefined: CapabilityRef
internal/store/playbook_test.go:67:43: undefined: Playbook
internal/store/playbook_test.go:67:54: undefined: PlaybookChecks
internal/store/playbook_test.go:77:35: undefined: PlaybookChecks
internal/store/playbook_test.go:77:71: undefined: PlaybookCheckItem
internal/store/reconcile_test.go:49:57: undefined: AppliedRef
internal/store/session_lease_safety_test.go:50:25: undefined: Lifecycle
internal/store/session_lease_safety_test.go:56:34: undefined: Lifecycle
internal/store/session_lease_safety_test.go:56:34: too many errors
EXIT 1
```

`/tmp/anotify-core-next-g1tests-packages-final.log`

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
0ed7bfc1153673f6a64cd36b14c623191f9be389
CMD timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/... ./internal/auth/... ./internal/push/... -count=1
# github.com/zzjcool/anotify/internal/store [github.com/zzjcool/anotify/internal/store.test]
internal/store/capability_links_test.go:59:30: undefined: ArtifactBuilder
internal/store/playbook_test.go:17:84: undefined: Playbook
internal/store/playbook_test.go:51:71: undefined: CapabilityRef
internal/store/playbook_test.go:67:43: undefined: Playbook
internal/store/playbook_test.go:67:54: undefined: PlaybookChecks
internal/store/playbook_test.go:77:35: undefined: PlaybookChecks
internal/store/playbook_test.go:77:71: undefined: PlaybookCheckItem
internal/store/reconcile_test.go:49:57: undefined: AppliedRef
internal/store/session_lease_safety_test.go:50:25: undefined: Lifecycle
internal/store/session_lease_safety_test.go:56:34: undefined: Lifecycle
internal/store/session_lease_safety_test.go:56:34: too many errors
FAIL	github.com/zzjcool/anotify/internal/store [build failed]
ok  	github.com/zzjcool/anotify/internal/store/v4test	34.654s
ok  	github.com/zzjcool/anotify/internal/auth	20.790s
ok  	github.com/zzjcool/anotify/internal/push	0.006s
FAIL
EXIT 1
```

### 回滚后 auth 局部轻量验证（不替代 fixed3）

```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-runtime-next
0ed7bfc1153673f6a64cd36b14c623191f9be389
CMD timeout 60 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/auth/... ./internal/push/... (owned-change lightweight, NOT fixed3 substitute)
EXIT 0
```

---

# A-continue（同 A 续接 leaf）：原 store 全量编译闭合 + 安全族逐名映射

> 状态口径：**仅 store 编译闭合**（`go test ./internal/store/... -run '^$'` RC=0），
> 不声明任何测试执行 PASS，也不声明 G1/T1/A 全仓 DONE。B 轮（route/lease/server/agent/executor）
> 未开始，`go build ./internal/server/` 仍红（见文末"遗留"）。

## 闭合命令（原样）

```text
cwd  /root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery
HEAD 55e3e46 (+ WIP)
CMD  timeout 180s env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/... -run '^$'
LOG  /tmp/anotify-v4-delivery-a-continue-gate2-060821.log
ok  	github.com/zzjcool/anotify/internal/store	0.005s [no tests to run]
ok  	github.com/zzjcool/anotify/internal/store/v4test	(cached) [no tests to run]
RC   0
```

基线诊断（同一 cwd，`go vet -gcflags=all=-e ./internal/store/`）：582 错误
（playbook 250 / node 169 / source 95 / upgrade_gate 40 / orphan 13 / stmt_wiring 10 /
pool 3 / migrate 1 / capability_spec 1），另有外部测试包 `bench_test.go` 32 条
（首次 `-run '^$'` 才暴露）。全部闭合；未删除任何测试文件作为完成手段。

## 安全族逐名映射（原样恢复的名字在"新 case"列标注 restored）

### `internal/store/session_lease_safety_test.go`（4）

| old case | outcome | 新 case + 具体不变量（path:line） |
|---|---|---|
| `TestResolveLostTasksAbandonsNonIdempotentStarted` | migrated | `internal/store/task_test.go:558 TestRecoverLostLeasesRequeues` 的 `non-idempotent-abandons` 子用例；断言 `v4MustPhase(...TaskAbandoned)` |
| `TestResolveLostTasksAbandonsWhenAttemptsExhausted` | migrated | `internal/store/task_test.go:608 TestMaxAttemptsExceededAbandons`（`ErrMaxAttemptsExceeded` → 相位 abandoned）+ `internal/store/v4test/task_test.go:93 TestTaskMaxAttemptsTerminal` |
| `TestResolveLostTasksAssignedNotStartedBeatsNonIdempotent` | narrow-retired | v4 没有 assigned 相位：`AssignTask` 只记候选（`internal/store/task.go:692` UPDATE 不改 phase），未 grant 的任务仍是 pending，而 `RecoverLostLeases` 只看 running（`internal/store/task.go:968` 注释与实现）。未开始不被误判为失联是结构事实，断言在 `internal/store/task_test.go:287 TestAssignRecordsCandidateWithoutAttempt`（phase 仍 pending、attempt 仍 0） |
| `TestResolveLostTasksCleanEndOfIdempotentStartedStillRequeues` | migrated | `internal/store/task_test.go:558 TestRecoverLostLeasesRequeues` 的 `idempotent-requeues` 子用例（→ TaskPending） |

### `internal/store/session_lease_test.go`（7）

| old case | outcome | 新 case + 不变量 |
|---|---|---|
| `TestExtendLeasesForTasksOnlyListedIDs` | migrated（语义收窄） | v4 续租是**per-task 精确绑定**（无批量 extend 接口）：`internal/store/task_test.go:379 TestRenewLeaseAuthorized`（错 attempt/lease → `ErrLeaseMismatch`，同文件 :399/:402）+ `internal/store/v4test/task_test.go:177 TestTaskRenewLease`；过期后不可复活 → `v4test/task_test.go:1423 TestRenewLeaseCannotReviveExpiredAuthorization` |
| `TestResolveLostTasksRequeue` | migrated | `internal/store/task_test.go:558`（idempotent → pending）+ `v4test/task_test.go:307 TestTaskRecoverIdempotentRequeues` |
| `TestResolveLostTasksAbandon` | migrated | `internal/store/task_test.go:558`（non-idempotent → abandoned）+ `task_test.go:608` |
| `TestResolveLostTasksSkipsStaleGeneration` | narrow-retired | v4 无 generation 概念：执行身份是 (attempt, lease_id) 精确绑定，旧 attempt 一律 `ErrLeaseMismatch`（`v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry`、`v4test/result_current_binding_test.go:18`） |
| `TestResolveLostTasksAssignedNotStartedRequeuesEvenWhenAbandon` | narrow-retired | 同 session_lease_safety 第 3 行：无 assigned 相位，未开始即 pending，不进 lost 判定 |
| `TestCreateTasksRejectsOversizedParams` | migrated | `internal/store/v4test/task_test.go:555`（`ErrParamsTooLarge`，位于 `TestTaskListRoundTrip` 512；同段 :563 还断言「建任务只能落 pending」） |
| `TestReclaimStaleSlots` | migrated | `internal/store/v4test/cluster_node_test.go:518-549`（`TestNodeInstancesAndLeases`）：unknown / wrong-lease 观测 → 回收 0，match → 回收 1 |

### `internal/store/tx_merge_test.go`（7）

| old case | outcome | 新 case + 不变量 |
|---|---|---|
| `TestReportResultDoesNotReleaseForeignSlot` | **restored** | `internal/store/tx_merge_test.go:66`（同名，v4 真夹具）：A 完成只释放 A 的槽，B 的 slot/lease/attempt 全部不动；补充 `v4test/task_test.go:763 TestTaskOldAckCannotReleaseNewAttemptSlot` |
| `TestReportResultRejectsTerminalTaskWithoutTouchingSlot` | **restored** | `internal/store/tx_merge_test.go:111`（同名）：终态任务旧结果重放 → `ErrLeaseMismatch`，同槽新任务绑定与旧任务终态事实都不变；补充 `v4test/result_current_binding_test.go:18`（跨 attempt 旧结果零写） |
| `TestReportResultMatchesSplitWrites` | retired（等价性被结构取代） | v4 只有一条写入路径（无"拆分版"可对照）；结果事实逐字段落库由 `v4test/result_facts_test.go:33 TestC3bCompleteResultFullJSONNanoseconds` / `:61 TestC3bCompleteResultFullFactsDiscrimination` / `:232 TestC3bCompleteResultMonotonicEquivalentDuplicate` 断言 |
| `TestLeaseAndAssignMatchesSplitWrites` | retired（同上） | 派发事实：`internal/store/task_test.go:287 TestAssignRecordsCandidateWithoutAttempt`（assign 不耗 attempt/不绑槽）+ `:314 TestGrantMovesToRunning` |
| `TestProgressTaskMatchesSplitWrites` | retired | v4 无 `ProgressTask`（中间进度上报随 handler/central 执行器删除） |
| `TestLeaseAndAssignGuardFailureDoesNotLeakSlot` | migrated | `internal/store/task_test.go:357 TestGrantRejectsStaleAssignment`：被拒 grant 后 `v4BusySlots == 0`（:374）；另 `internal/store/node_test.go:821 TestNodeAuthorizationGrant`（错 token hash 的 grant 不占槽） |
| `TestProgressTaskRejectsTerminalTask` | retired | 无 ProgressTask；终态不可变由 `internal/store/task_test.go:498 TestTerminalPhaseIsImmutable` + `:462 TestCompleteDuplicateIsIdempotent` 覆盖 |

### `internal/store/builtin_test.go`（18）——中央执行器/播种族 + auth/NS 安全点

| old case | outcome | 新 case / 退役理由 |
|---|---|---|
| `TestSeedBuiltinCapabilitiesIsIdempotent` | retired（代理点迁移） | 中央播种已删；**注册原子性**改由 `internal/store/namespace_test.go:136 TestRegisterUserCreatesDefaultNamespace` 断言（用户+default ns 同事务、v4 0 播种）；带密码路径 `internal/store/password_test.go:163 TestRegisterUserWithPassword_RoundTrip`、并发首 admin `:269` |
| `TestBuiltinCapabilityIsCentral` / `TestNotifyPhoneStillCentral` / `TestCentralCapabilitiesAreSpecDriven` / `TestNotifyPhoneParamsSchemaRequiresTo` / `TestClaimCentralTasksRunsStateMachine` | retired | 纯 v3 中央执行器（handler/claim/state machine）；v4 执行权只经 `NodeAuthorization` + assignment + 真实 slot（`internal/store/task_test.go:314`、`v4test/task_test.go:19`） |
| `TestFinishTaskWritesOutcome` | migrated | `internal/store/task_test.go:408 TestCompleteRecordsResultAndFinishedAt`（相位/exitCode/finishedAt/acceptedAt 落库） |
| `TestFinishTaskRejectsInvalidStatus` | migrated | `internal/store/task_test.go:498 TestTerminalPhaseIsImmutable`（终态不可再变）+ `v4test/task_test.go:1124` |
| `TestNoOriginShellCapabilities` | retired | 原壳能力随内置播种删除；来源身份改由源行定义（`internal/store/source_test.go:215 TestSourceValidation` 的 kind/引用校验） |
| `TestEntryEdgeAcceptsAnyPort` | retired | v4 无端口（`internal/artifact` 一次运行一个结果；边是 next 的投影 `internal/store/graph.go:103`） |
| `TestBuiltinDisplayNames` / `TestBuiltinDisplayNameBackfill` / `TestBackfillBuiltinCapabilitiesCoversEveryNamespace` / `TestBackfillBuiltinCapabilitiesEmptyDeployment` | retired | backfill 随播种删除；displayName 可变性由 `internal/store/capability.go:515 SetCapabilityDisplayName`（CAS）承载，往返见 capability 族现有测试 |
| `TestExampleBuiltinScriptEmitsDeclaredPort` / `TestExampleBuiltinIsUsable` / `TestExampleBuiltinArtifactUpdateRepointsDeployment` / `TestExampleBuiltinContentDerivedNotBackfilled` | retired | 示例内置能力（notify.phone 脚本）已从产品删除；"内容派生 vs 回填"由 `internal/store/deployment_test.go:18 TestCurrentArtifactIsCapabilityContent`（产物=能力正文规范字节）与 `capability_test.go` content-addressed 用例承接 |

### 本次 leaf 重写的文件族（逐名）

#### `internal/store/node_test.go`（原 28）

migrated 同名（19）：`TestNodeCreateAndGet` `TestNodeNamespaceIsolation`（v4 判据=申报∩授权）
`TestNodeCapabilitiesReplacedOnRehello`（RecordHello 快照替换）`TestNodeCapabilitiesNoHarnessColumns`
`TestNodeMaxInstances` `TestNodeInstancesUpsertAndShrink` `TestUpsertInstancesKeepsBusyTask`
`TestLeaseInstanceAndRelease`（LeaseSlot/ReleaseSlot；错 attempt 释放不清槽 :495-510）
`TestLeaseInstanceDeadSlotIgnored` `TestNodesForCapability` `TestNodesForCapabilityNamespaceIsolation`
`TestNodeStatusTransitions` `TestTouchNode` `TestNodeEmptyListIsArray` `TestNodeTagsRoundTrip`
`TestNewNodeTokenUnique` `TestFreeSlotsByNode` `TestNodeCreateDuplicateName` `TestValidateNodeName`

retired（9，全部 pin/迁移专属）：`TestNodePins` `TestPutPinValidation`
`TestLeaseAndAssignWithPinWritesPinAtomically` `TestLeaseAndAssignWithPinRollsBackPinOnFailure`
`TestRollbackPinOnlyDeletesOwnPin` `TestRollbackPinSameSecondSameNode` `TestRollbackPinValidation`
（v4 删 pin：派发只经 assignment+slot；原子性由 `node_test.go:821 TestNodeAuthorizationGrant`、
`tx_merge_test.go:66`、`v4test/task_test.go:71 TestTaskGrantRejectsStaleAssignment` 接走）
`TestNodeCapabilityUnknownIsStale`（无 node_capabilities 声明行；"未知/陈旧"= NotDeployed，
`node_test.go:176 TestNodesForCapability` 断言）`TestMigration7PinIDWorksOnExistingDB`
（v4 baseline 重建，老库明确拒绝；由 `migrate_test.go` + `schema_baseline_test.go` 覆盖）

new：`TestNodeConfigRevisionAndAppliedFacts`（配置 CAS/revision/applied 事实）、
`TestNodeAuthorizationGrant`（grant 只认认证身份 + 真实 slot）

#### `internal/store/playbook_test.go`（原 33）

migrated 同名（12）：`TestPlaybookManifestRoundTrip`（files 原始字节往返）
`TestPlaybookCreateGetListDelete`（+删清单不删已装对象/kept）`TestPlaybookEmptyManifestNormalized`（v4=entry_missing）
`TestPlaybookSlugValidation` `TestInstallPlaybookIdempotent` `TestInstallPlaybookWebhookSourceIsUsable`
`TestInstallPlaybookCronSource` `TestPlaybookInstallEmptyManifest` `TestListPlaybooksCreatedAtOrder`
`TestInstallPlaybookInlineSpecStoresScriptBody`（渲染后正文落库）

new：`TestRenderPlaybookReadOnly`（0 写）`TestInstallPlaybookDryRunWritesNothing`（0 持久写）
`TestListPlaybookInstallsGroups`（attempts 折叠 + 组内并集）

retired（v3 manifest/端口/checks/handler/trigger/legacy 专属，逐条理由见
`internal/store/playbook_test.go` 文件头映射块）：`TestPlaybookChecksNodePresence`
`TestPlaybookChecksPartialAndEntryCapabilities` `TestPlaybookChecksResolvesDeclaredVersion`
`TestPlaybookChecksHubHandlerRefIsReady` `TestInstallPlaybookSkipsEdgeWithMissingEndpoint`
`TestInstallPlaybookSkipsEdgeWithUnknownPort` `TestInstallPlaybookSkipsSourcePointingAtSameEntry`
`TestInstallPlaybookRejectsForeignHandler` `TestInstallPlaybookRefAndSpecBothGiven`
`TestInstallPlaybookRefDoesNotCreateCapability` `TestInstallPlaybookEdgeCarriesLiteralsAndPriority`
`TestInstallPlaybookTriggerNameAndMapping` `TestInstallPlaybookLegacyManifestStillInstalls`
`TestInstallPlaybookRejectsUnresolvableRef`（→ `internal/playbook/parse.go:203 checkReferences`
在 Parse/Render 拒绝，不是跳过；本包 `TestRenderPlaybookReadOnly` 走同一条校验）
`TestInstallPlaybookDoesNotOverwriteExistingDefinition` / `TestInstallPlaybookNewVersionSameSchema`
（→ `TestInstallPlaybookIdempotent` 复用不建行 + `v4test/playbook_test.go:1038 TestInstallReuseNeverUpdatesExistingRow`）
`TestPlaybookManifestReservedOriginNames` → **窄退役并上报**：v4 安装路径
（`internal/store/playbook_tx.go:385 createInstalledSourceOn` → `insertSourceOn`）没有保留名检查，
这条 v3 约束在 v4 不存在。

#### `internal/store/source_test.go`（原 20）

migrated 同名（11）：`TestCronParsing` `TestCronNext` `TestCronDomDowOrSemantics` `TestCronNeverFires`
`TestSourceCRUD`（targets=引用 + 入口边投影 + CAS 删除）`TestSourceValidation`（含"引用必须可解析"）
`TestSourceWebhookToken`（LookupHook + 停用失效 + token 不被 PATCH 换掉）`TestSourceKindImmutable`
`TestDueCronSources`（ListDueSources + MarkSourceFired + 停用不调度）
`TestCreateSourceRejectsReservedName` `TestUpdateSourceRejectsReservedName`（v4：名字整体不可变 → `ErrSourceNameImmutable`）

概念替换（v3 vars/resultSchema/github kind 已删）：`TestValidateVarDefs`→`TestSourceTargetsParamsValidation`
（`$(payload.…)` 模板路径校验）、`TestSourceVarDefsParsing`/`TestSourceConfigKeepsVars`→`TestSourceConfigRoundTrip`
（payload/payloadSchema/sample 往返 + 未知键创建期拒绝）、`TestSourceConfigPayloadParsing`→
`TestSourceConfigPayloadOnlyForKeepingKinds`（cron/manual 保留、webhook 丢弃）、
`TestValidateSourceConfigRejectsBadVars`→`TestSourceTargetsParamsValidation`（CreateSource 透出哨兵）、
`TestSourceResultSchemaRoundTrip`→`TestSourcePayloadSchemaRoundTrip`、`TestSourceResultSchemaValidation`→
`TestSourcePayloadSchemaValidation`、`TestUpdateSourceKeepsResultSchema`→`TestUpdateSourceKeepsPayloadSchema`。
`TestValidateSourceConfigPayloadOnlyForKeepingKinds`→`TestSourceConfigPayloadOnlyForKeepingKinds`（保名，收敛到 v4 config 归一）

#### 其余（同文件保留、API 迁移）

- `internal/store/upgrade_gate_test.go`（6 同名）：v4 契约只有 params → 删出口=原地改线
  （`UpdateNext` 不改 version/digest，见 `TestUpgradeGateRemovedPort`）；破坏性只剩
  `param_type_changed`/`param_added_required`；`CreateCapabilityForce` 已删除，故不再断言 force。
- `internal/store/stmt_wiring_test.go`（2 同名）：入口换成 v4 真实读路径
  （`getCapabilityByDigestOn` 替代 `getCapabilityByHash`、`ListTasksByTrace` 替代 `getEventByIdemKey`——
  后者在 v4 不存在，去重走 `event_deliveries` 窗口占用）；ListTasks 过滤 4 个 → 16 组合；
  `wiringMinDistinctSQL=16` 实测未变（scanEvent 3→2，新增 ListTasksByTrace 1）。
- `internal/store/orphan_test.go`（7 同名 + 1 新 `TestOrphanPendingOnlyPinnedDigestCounts`）：
  原因词表改 `no_capability`/`no_online_node` + `NodeReason`（协议封闭词表）；
  "能跑"= 部署 applied（`deployments.state`），不是 node_capabilities 申报。
- `internal/store/pool_test.go`：`countRows` 与 retention 同名冲突 → 新增 `countRowsWhere`（同名用例保留 5 条）。
- `internal/store/migrate_test.go`：删未用 `strconv` 导入（10 条原样）。
- `internal/store/capability_spec_test.go`：`artifact.CanonicalJSON(c.Artifact())` → 取址（5 条原样）。
- `internal/store/bench_test.go`（外部包 store_test，32 编译错误）：`BenchmarkCreateTasks` 保留；
  `BenchmarkTransition` 退役（v4 无 Transition）→ `BenchmarkAssignGrant`；
  `BenchmarkReportResult` → `BenchmarkCompleteTask`（v4 结果入口
  `CompleteTaskClassifiedAuthorized`）；护栏 `TestBenchmarksTargetRealPaths` 保留并改成
  v4 可观测效果（落行/相位 running+租约/slot 绑上又放开）。

## 本 leaf 上报的产品发现（未改生产）

1. **升级闸门写路径不再执行**：`checkUpgradeOn` 只被 `CheckUpgrade` 调用
   （`internal/store/upgrade.go:119-120`），`CreateCapability`（`capability.go:150`）不调用它，
   `CreateCapabilityForce` 已不存在；而 `internal/server/console_handlers.go:1406` 仍调用它。
   影响：破坏性 params 变更在 store 层不会被拒（只有 console 预计算会 409）。B 轮需裁决。
2. **playbook 安装不校验保留源名 route**：`playbook_tx.go:385` → `insertSourceOn` 无保留名检查，
   而直写路径 `CreateSource`（`source.go:150`）有。v3 的 `TestPlaybookManifestReservedOriginNames` 因此无法迁移。
3. **playbook 同内容新版本号**（`internal/store/playbook_test.go` 退役说明）：显式版本 + 相同内容会撞
   `(namespace_id,name,fold_key)` 唯一索引并返回裸约束错误，而非 `ErrVersionConflict`（v3 该用例断言可落新版）。

## 遗留（不属于 A 编译门禁）

- `go build ./internal/server/` 仍红：`internal/lease`（`store.RetentionOptions`/`SweepRetention`/
  `LeaseSweepIntervalSec`/`Task.Attempts`）、`internal/route`（`store.LinkTarget`/`NodePin`/`GraphVersion`/
  `Emission`）、`internal/server`（`CreateCapabilityForce`）待 B/T2 轮迁移。
- 本文件所有 `pending` 行对上述已重写文件已被本附录取代；未触及的文件/行为保持原样。

## A-continue 实测运行记录（除 gate 外，均为本 leaf 新增/改写文件的自证）

```text
# 安全族（restored + 相关）
CMD  timeout 180s env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto \
     go test ./internal/store/ -count=1 -run 'TestReportResultDoesNotReleaseForeignSlot|TestReportResultRejectsTerminalTaskWithoutTouchingSlot|TestLeaseInstanceAndRelease|TestNodeAuthorizationGrant|TestLeaseInstanceDeadSlotIgnored|TestOrphanPending|TestAbandonPendingTask'
LOG  /tmp/anotify-v4-delivery-a-continue-safetyrun2-061213.log
ok  	github.com/zzjcool/anotify/internal/store	0.144s
RC   0

# 本 leaf 改写的全部 fixture 族（node/playbook/source/upgrade/stmt/orphan/tx_merge）
CMD  timeout 300s env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto \
     go test ./internal/store/ -count=1 -run 'TestNode|TestLease|TestUpsert|TestTouch|TestNewNodeTokenUnique|TestFreeSlots|TestValidateNodeName|TestPlaybook|TestRenderPlaybook|TestInstallPlaybook|TestListPlaybook|TestSource|TestCron|TestDueCron|TestCreateSource|TestUpdateSource|TestOrphan|TestAbandonPending|TestUpgradeGate|TestUpgradeImpact|TestStmtWiring|TestReportResult'
LOG  /tmp/anotify-v4-delivery-a-continue-myfamilies2-061351.log
ok  	github.com/zzjcool/anotify/internal/store	0.716s
RC   0
```

## 产品发现 #4（实测，未改生产）

**零出口源被接受**：`CreateSource` 的 `checkSourceTargetsOn`（`internal/store/source.go:266`）
走 `sourcespec.ValidateTargets`（`internal/sourcespec/config.go:210`），而它对空列表返回 nil；
所以 `Targets: nil` 的源能建出来（事件永远不会被派发）。v3 有这道闸门
（旧 `TestSourceValidation` 第一段断言），v4 没有。本 leaf 不在生产侧加闸门，只上报。

## 整包测试现状（信息性，**不属于 A 编译门禁**；31 条红均在本 leaf 未改写的族）

```text
CMD  timeout 600s env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/ -count=1
LOG  /tmp/anotify-v4-delivery-a-continue-storeall-final-061412.log
RC   1   fails=31

authz/namespace: TestAuthorizeNamespace TestAuthorizeNamespaceForNode TestNamespaceCRUD
  TestNamespaceRoundTrips TestNamespaceDeleteRequiresForceWhenNotEmpty
  （典型：`authz_test.go:82 delete namespace: store: resource version required` —— 旧调用没走 CAS 令牌）
capability: TestCapabilitySpecRoundTrip（requires.values/fieldPath 封闭集合校验收紧）
event/stats: TestEventGetAndList TestEmissionListOrderedAndScoped TestListEventsBeforeEquivalence
  TestListEventsBeforeEmptyIsSlice TestListEventsBeforeUsesRetentionIndex TestMessageStatsTotalsDeliveryAndDevices
migrate/baseline: TestMigrationTwoColumns TestMigrationTenDropsBalance TestBaseline*（6） TestLegacyDatabaseRejected
retention/pending: TestRetentionKeepsUnreapedAttempts TestOldestPendingCreatedAtIgnoresNotBefore TestPendingBacklogSQLShape
auth: TestRegisterUser_PreMigration8SchemaStillWorks
task: TestCompleteRecordsResultAndFinishedAt TestCompleteRequiresGrant TestTerminalPhaseIsImmutable
  TestCancelRunningAwaitsAck TestMaxAttemptsExceededAbandons TestTaskAttemptSnapshotRoundTrip
```

这些族在 v4 重写后**从未被执行过**（原包一直编译不过），属下一 leaf 的返工面，不是本 leaf 的夹具改动引起
（本 leaf 改写的 22+ 个用例组在上述两次 scoped 运行里全绿）。
