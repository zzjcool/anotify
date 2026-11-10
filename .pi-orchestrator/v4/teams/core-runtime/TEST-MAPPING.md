# T1 测试迁移映射（B 轮：原 460 行已逐行落地；不代表 G1/T1 全量验收）

> 状态口径：本文件下方「原用例逐名库存」是**唯一映射事实**（B 轮逐行更新：kept / migrated /
> restored / retired，全部带真实 test+不变量）。更早的 checkpoint 段落只是历史记录；
> A-continue 的重复附录已删除（不再有第二份 pending 清单）。
> B 轮三项机械门禁的真实结果与唯一产品红见文末「B 轮结果」。

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
7. `TestV4MigrationCancelledContextDoesNotChangeLedger`（历史草案名；现 `TestWhiteboxCancelledBeginTxPreservesBaselineCheckpoint` 只验驱动取消+完整 ledger 不变，迁移入口不接收 context，不宣称迁移取消）

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
| `TestUser_RoleRoundtrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/admin_test.go:11 TestUser_RoleRoundtrip` |
| `TestUser_DisabledRoundtrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/admin_test.go:57 TestUser_DisabledRoundtrip` |
| `TestUserCount` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/admin_test.go:87 TestUserCount` |
| `TestListUsersWithStats` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/admin_test.go:127 TestListUsersWithStats` |
| `TestSystemStats` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/admin_test.go:183 TestSystemStats` |
| `TestListGlobalEvents` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/admin_test.go:258 TestListGlobalEvents` |
| `TestListAllSessions` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/admin_test.go:325 TestListAllSessions` |

#### `internal/store/auth_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestUser_CreateGet` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/auth_test.go:18 TestUser_CreateGet` |
| `TestPasskey_CRUD` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/auth_test.go:49 TestPasskey_CRUD` |
| `TestSession_CRUD` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/auth_test.go:88 TestSession_CRUD` |
| `TestAPIKey_CRUD` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/auth_test.go:126 TestAPIKey_CRUD` |

#### `internal/store/authz_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestAuthorizeNamespace` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/authz_test.go:17 TestAuthorizeNamespace` |
| `TestAuthorizeNamespaceForNode` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/authz_test.go:99 TestAuthorizeNamespaceForNode` |

#### `internal/store/bench_dispatch_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `BenchmarkDispatchChain` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/bench_dispatch_test.go:38 BenchmarkDispatchChain` |
| `TestBenchmarksTargetRealPaths` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/bench_test.go:254 TestBenchmarksTargetRealPaths` |

#### `internal/store/bench_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `BenchmarkCreateTasks` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/bench_test.go:162 BenchmarkCreateTasks` |
| `BenchmarkTransition` | migrated | `internal/store/bench_test.go:193 BenchmarkAssignGrant` |
| `BenchmarkReportResult` | migrated | `internal/store/bench_test.go:219 BenchmarkCompleteTask` |

#### `internal/store/bench_writeamp_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `BenchmarkCreateTasksWriteAmplification` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/bench_writeamp_test.go:192 BenchmarkCreateTasksWriteAmplification` |
| `BenchmarkCreateTasksSingleRowTx` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/bench_writeamp_test.go:255 BenchmarkCreateTasksSingleRowTx` |

#### `internal/store/builtin_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestSeedBuiltinCapabilitiesIsIdempotent` | retired（中央播种已删） | 注册原子性改由 `internal/store/namespace_test.go:140 TestRegisterUserCreatesDefaultNamespace` 断言（用户+default ns 同事务、v4 0 播种） |
| `TestBuiltinCapabilityIsCentral` | retired（纯 v3 中央执行器） | v4 执行权只经授权身份+assignment+真实 slot：`internal/store/task_test.go:316 TestGrantMovesToRunning` |
| `TestNotifyPhoneStillCentral` | retired（纯 v3 中央执行器） | v4 执行权：`internal/store/task_test.go:316 TestGrantMovesToRunning` |
| `TestCentralCapabilitiesAreSpecDriven` | retired（纯 v3 handler 分发） | 正文=脚本，校验 `internal/store/capability_spec_test.go:93 TestCapabilitySpecInvalidRejected` |
| `TestNotifyPhoneParamsSchemaRequiresTo` | retired（纯 builtin schema） | 独立 device/push/tag/VAPID 测试保留 |
| `TestClaimCentralTasksRunsStateMachine` | retired（纯 v3 central claim） | 真实派发 `internal/store/task_test.go:316 TestGrantMovesToRunning` |
| `TestFinishTaskWritesOutcome` | migrated | 正/负结果事实 `internal/store/task_test.go:410 TestCompleteRecordsResultAndFinishedAt`；失败空 Error 投影（原始 ResultJSON 不改写）`internal/store/task_test.go:923 TestCompleteErrorProjectionKeepsRawResultFacts`；失败 Error/Reason/Summary 全空 → 字面 failed、raw Error 仍空 `internal/store/task_test.go:1081 TestCompleteEmptyFailureProjectsPhaseAndKeepsRawResultFacts`；后继成功清旧投影/保留 submitted Error `internal/store/task_test.go:970 TestCompleteSuccessorAttemptClearsProjectionAndKeepsSubmittedError` |
| `TestFinishTaskRejectsInvalidStatus` | migrated | 非法 result.phase 有效授权零写 `internal/store/task_test.go:819 TestCompleteRejectsInvalidResultPhaseWithZeroWrites`；终态不可变 `internal/store/task_test.go:507 TestTerminalPhaseIsImmutable` |
| `TestNoOriginShellCapabilities` | retired（壳能力随播种删除） | 来源身份由源行定义 `internal/store/source_test.go:216 TestSourceValidation` |
| `TestEntryEdgeAcceptsAnyPort` | retired（v4 无端口） | 入口边投影 `internal/store/v4test/edges_test.go:72 TestSourceEdgesProjection` |
| `TestBuiltinDisplayNames` | migrated | displayName 往返/CAS `internal/store/capability_test.go:346 TestCapabilityDisplayName`；foldKey 不含 displayName `internal/store/v4test/capability_test.go:119 TestCapabilityFoldKeyIgnoresNextAndDisplayName` |
| `TestBuiltinDisplayNameBackfill` | retired（历史回填迁移删除） |  |
| `TestBackfillBuiltinCapabilitiesCoversEveryNamespace` | retired（v3 builtin 回填） |  |
| `TestBackfillBuiltinCapabilitiesEmptyDeployment` | retired（v3 builtin 回填） |  |
| `TestExampleBuiltinScriptEmitsDeclaredPort` | retired（示例内置能力删除） | 出口仅 emits，缺 to=missing_target（protocol） |
| `TestExampleBuiltinIsUsable` | migrated | 内容寻址/幂等 `internal/store/capability_test.go:90 TestCapabilityIdempotentCreate`；部署投递 `internal/store/v4test/deployment_test.go:169 TestDeployDeliveryForNode` |
| `TestExampleBuiltinArtifactUpdateRepointsDeployment` | retired（v4 同版内容不可变） | 部署 reconcile `internal/store/v4test/deployment_test.go:66 TestDeployReconcile` |
| `TestExampleBuiltinContentDerivedNotBackfilled` | migrated | 产物=正文派生 `internal/store/deployment_test.go:21 TestCurrentArtifactIsCapabilityContent` |

#### `internal/store/capability_batch_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCreateCapabilitiesBatchRejectsEmptyAndDuplicate` | retired（CreateCapabilitiesBatch 删除） | 单行幂等/同版冲突 `internal/store/capability_test.go:90 TestCapabilityIdempotentCreate` / `internal/store/capability_test.go:117 TestCapabilitySameVersionDifferentContentRejected` |
| `TestCreateCapabilitiesBatchRejectsMisalignedBuilders` | retired（builder 双输入随 artifact.Spec 删除） | 正文校验前置 `internal/store/capability_spec_test.go:93 TestCapabilitySpecInvalidRejected` |
| `TestCreateCapabilitiesBatchLinksAcrossBatch` | retired（无批量 API） | 出口=next 引用，跨行解析 `internal/store/v4test/edges_test.go:118 TestRefResolution` |
| `TestCreateCapabilitiesBatchRollsBackOnBadLink` | narrow-retired（ROOT 裁定：仅旧 CreateCapabilitiesBatch / BatchLinkSpec API 与 CREATE ghost-reference 前置拒绝退役；保留当前多对象原子性） | 当前多资源失败 0 写由 `internal/store/v4test/playbook_test.go:892 TestInstallConflictRollsBackWholePackage` 承接：能力/源/边/回执行计数不变、a-extra 无行、既有入口边保留，不恢复旧 endpoint。正向 ghost-next 声明与边落库、读取时缺失分类由 `internal/store/capability_test.go:536 TestCapabilityGhostNextIsPreservedAndResolvedMissing` 承接：CREATE 成功、GetCapability.next 与 EdgesFrom 原始引用精确往返、ResolveRef 返回 ErrNotFound 而非 ErrRefUnresolved；ResolveRef 读取不是写回滚证明 |
| `TestCreateCapabilitiesBatchArtifactBuildFailureRollsBack` | retired（无独立 artifact 构建） | 产物派生 `internal/store/capability_spec_test.go:121 TestCapabilityArtifactDerivedNotPersisted` |
| `TestCreateCapabilitiesBatchMergesBreakingImpact` | retired（批量影响合并属展示层） | 单版本影响 `internal/store/upgrade_gate_test.go:134 TestUpgradeGateListsAffectedLinks` |
| `TestMergeUpgradeImpactDedupAndSort` | retired（去重排序属 console） | 参数类型破坏 `internal/store/upgrade_gate_test.go:206 TestUpgradeGateParamTypeChanged` |

#### `internal/store/capability_links_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCreateCapabilityWithLinksCreatesAll` | migrated | next→edges 投影 `internal/store/v4test/capability_test.go:403 TestCapabilityCreateProjectsEdges` |
| `TestCreateCapabilityWithLinksRollsBackOnBadLink` | narrow-retired（ROOT 裁定：仅旧 CREATE ghost-reference 前置拒绝退役；保留当前能力正文+边写原子性） | 当前原子性由 `internal/store/capability_test.go:485 TestCapabilityCreateEdgeWriteFailureRollsBack` 承接：触发器确认新能力+第一条边已在事务内可见后，真实 edge INSERT 失败；能力全文/digest 与全部边前后逐项不变、失败能力不可读，不以非法脚本前置拒绝代替回滚。允许 ghost-next 的正向事实由 `internal/store/capability_test.go:536 TestCapabilityGhostNextIsPreservedAndResolvedMissing` 承接：声明/边原样持久化、实际 ResolveRef 缺失为 ErrNotFound；不添加 ghost ban 或新 status 字段 |
| `TestCreateCapabilityWithLinksArtifactBuildFailureRollsBack` | retired（产物派生，无构建步骤） | `internal/store/capability_spec_test.go:121 TestCapabilityArtifactDerivedNotPersisted` |
| `TestCreateCapabilityWithLinksArtifactInTx` | retired（artifacts 表删除） | 正文/digest 同事务落 capability_versions `internal/store/capability_test.go:396 TestCapabilityDigestMatchesArtifactContent` |
| `TestCreateCapabilityWithLinksIdempotent` | migrated | `internal/store/capability_test.go:90 TestCapabilityIdempotentCreate` |
| `TestCreateCapabilityWithLinksConcurrent` | migrated | `internal/store/v4test/resource_cas_test.go:673 TestResourceCASConcurrentCreateAndDeleteRecreate`：681-689 两 goroutine 同时 CreateCapability，两个成功、同 UID、仅一条 live 行；后续 create/delete 竞态同时验证 live identity 与边投影，不以顺序 Create 代替并发 |
| `TestCreateCapabilityWithLinksUpgradeGateStillGate` | retired（父裁定#1：CreateCapabilityForce 删除） | 保留闸门断言 `internal/store/upgrade_gate_test.go:76 TestUpgradeGateRemovedPort` |

#### `internal/store/capability_spec_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCapabilitySpecRoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_spec_test.go:22 TestCapabilitySpecRoundTrip` |
| `TestCapabilitySpecInvalidRejected` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_spec_test.go:93 TestCapabilitySpecInvalidRejected` |
| `TestCapabilityArtifactDerivedNotPersisted` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_spec_test.go:121 TestCapabilityArtifactDerivedNotPersisted` |
| `TestCapabilityVersionConflict` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_spec_test.go:148 TestCapabilityVersionConflict` |
| `TestBuiltinHandlersAreKnownHandlers` | retired（v4 无 handler） | `internal/store/capability_spec_test.go:22 TestCapabilitySpecRoundTrip` |
| `TestBuiltinNotifyPhoneHasNoArtifact` | retired（内置示例删除） | `internal/store/capability_spec_test.go:121 TestCapabilityArtifactDerivedNotPersisted` |
| `TestMigration9Backfill` | retired（v4 单块 baseline，无历史回填） | `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` |
| `TestMigration9BackfillRejectsInvalidLegacyRow` | retired（无历史回填） | `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` |
| `TestSpecBackfillHelperShapes` | retired（无旧 spec 回填） | `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` |
| `TestCapabilityCreateContentAddressed` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_spec_test.go:177 TestCapabilityCreateContentAddressed` |

#### `internal/store/capability_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCapabilityCreateAndGet` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:37 TestCapabilityCreateAndGet` |
| `TestCapabilityIdempotentCreate` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:90 TestCapabilityIdempotentCreate` |
| `TestCapabilitySameVersionDifferentContentRejected` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:117 TestCapabilitySameVersionDifferentContentRejected` |
| `TestCapabilityOutputsOrderChangesHash` | retired（outputs 列删除） | `internal/store/v4test/capability_test.go:119 TestCapabilityFoldKeyIgnoresNextAndDisplayName` / `internal/store/v4test/capability_test.go:369 TestCapabilityDigestIncludesVersion` |
| `TestCapabilitySchemaKeyOrderInsensitive` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:134 TestCapabilitySchemaKeyOrderInsensitive` |
| `TestCapabilityDifferentContentDifferentHash` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:155 TestCapabilityDifferentContentDifferentHash` |
| `TestCapabilityNormalizeIncludesName` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:175 TestCapabilityNormalizeIncludesName` |
| `TestCapabilityVersionAppendOnly` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:194 TestCapabilityVersionAppendOnly` |
| `TestCapabilityNamespaceIsolation` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:231 TestCapabilityNamespaceIsolation` |
| `TestCapabilityEmptyListIsArray` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:261 TestCapabilityEmptyListIsArray` |
| `TestCapabilityEmptyOutputs` | retired（outputs 列删除） | `internal/store/capability_test.go:261 TestCapabilityEmptyListIsArray` |
| `TestCapabilityValidation` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:278 TestCapabilityValidation` |
| `TestLifecycleIdempotentDefault` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:315 TestLifecycleIdempotentDefault` |
| `TestGraphVersionOnDemand` | retired（graph_versions 删除） | `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` |
| `TestGraphVersionChangesWithEdges` | retired（图=edges 投影） | `internal/store/v4test/edges_test.go:30 TestCapabilityEdgesProjection` |
| `TestGraphVersionEmptyGraph` | retired（graph_versions 删除） | `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` |
| `TestEdgeRejectsPortNotInOutputs` | retired（无端口） | `internal/store/capability_test.go:278 TestCapabilityValidation` |
| `TestEdgeRejectsCrossNamespace` | migrated | `internal/store/namespace_test.go:92 TestCrossNamespaceReferencesRejected` |
| `TestEdgesFromLookup` | migrated | `internal/store/v4test/edges_test.go:30 TestCapabilityEdgesProjection` |
| `TestListEdgesEmptyIsArray` | migrated | `internal/store/v4test/edges_test.go:72 TestSourceEdgesProjection` |
| `TestCapabilityDisplayName` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/capability_test.go:346 TestCapabilityDisplayName` |
| `TestContentHashEqualsDigest` | migrated | `internal/store/capability_test.go:396 TestCapabilityDigestMatchesArtifactContent` |

#### `internal/store/cliauth_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCliAuthSession_RoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/cliauth_test.go:9 TestCliAuthSession_RoundTrip` |
| `TestCliAuthSession_ApproveDeny_AtomicConflict` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/cliauth_test.go:58 TestCliAuthSession_ApproveDeny_AtomicConflict` |
| `TestCliAuthSession_ExpiredMigration` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/cliauth_test.go:123 TestCliAuthSession_ExpiredMigration` |
| `TestCliAuthSession_ConsumeAndCreateKey` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/cliauth_test.go:150 TestCliAuthSession_ConsumeAndCreateKey` |
| `TestCliAuthSession_DeleteExpired` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/cliauth_test.go:226 TestCliAuthSession_DeleteExpired` |
| `TestCliAuthSession_UserCodeUnique` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/cliauth_test.go:256 TestCliAuthSession_UserCodeUnique` |
| `TestCliAuthSession_NotFound` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/cliauth_test.go:283 TestCliAuthSession_NotFound` |
| `TestCliAuthSession_PasskeyEnroll_RoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/cliauth_test.go:319 TestCliAuthSession_PasskeyEnroll_RoundTrip` |

#### `internal/store/deployment_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCurrentArtifactIsCapabilityContent` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/deployment_test.go:21 TestCurrentArtifactIsCapabilityContent` |
| `TestArtifactAbsentCases` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/deployment_test.go:50 TestArtifactAbsentCases` |
| `TestArtifactNamespaceIsolation` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/deployment_test.go:69 TestArtifactNamespaceIsolation` |
| `TestCreateDeploymentIdempotent` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/deployment_test.go:88 TestCreateDeploymentIdempotent` |
| `TestCreateDeploymentRejectsMismatch` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/deployment_test.go:126 TestCreateDeploymentRejectsMismatch` |
| `TestDeleteDeploymentRemovesRow` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/deployment_test.go:149 TestDeleteDeploymentRemovesRow` |
| `TestListDeploymentsFilters` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/deployment_test.go:175 TestListDeploymentsFilters` |
| `TestDeploymentsForNodeDerivesFromCapability` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/deployment_test.go:202 TestDeploymentsForNodeDerivesFromCapability` |
| `TestMarkDeployedDigestUsesCapabilityDigest` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/deployment_test.go:236 TestMarkDeployedDigestUsesCapabilityDigest` |
| `TestDeployProtocolShapeUnchanged` | migrated | `internal/store/v4test/deployment_test.go:169 TestDeployDeliveryForNode` / `internal/store/deployment_test.go:270 TestDeploymentDeliveryCarriesOpaqueBytes` |
| `TestDeploymentsForNodeSkipsHandlerCapability` | retired（无 handler 能力） | `internal/store/deployment_test.go:202 TestDeploymentsForNodeDerivesFromCapability` |
| `TestArtifactRequiresRun` | migrated | `internal/store/capability_spec_test.go:93 TestCapabilitySpecInvalidRejected` |

#### `internal/store/devices_update_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestUpdateDevice` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/devices_update_test.go:11 TestUpdateDevice` |

#### `internal/store/downstreams_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestReplaceCapabilityDownstreams` | retired（端口/downstream 对象删除） | 出口=next `internal/store/v4test/capability_test.go:440 TestCapabilityUpdateNext` |
| `TestReplaceCapabilityDownstreamsLeavesOtherPortsAlone` | retired（v4 无端口） | `internal/store/v4test/edges_test.go:30 TestCapabilityEdgesProjection` |
| `TestReplaceCapabilityDownstreamsIdempotent` | migrated | next 原地更新（不改 version/digest）`internal/store/v4test/capability_test.go:440 TestCapabilityUpdateNext` |
| `TestReplaceCapabilityDownstreamsValidation` | migrated | 引用必须可解析 `internal/store/v4test/edges_test.go:118 TestRefResolution` |
| `TestReplaceCapabilityDownstreamsNamedPort` | retired（v4 无端口名） |  |

#### `internal/store/edge_cycle_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCreateEdgeAllowsSelfLoop` | migrated | 环合法（写入期不拒）`internal/store/v4test/edges_test.go:118 TestRefResolution` |
| `TestCreateEdgeAllowsCycle` | migrated | `internal/store/v4test/edges_test.go:118 TestRefResolution` |
| `TestCreateEdgeAllowsDiamond` | migrated | 投影按 from→to `internal/store/v4test/edges_test.go:30 TestCapabilityEdgesProjection` |
| `TestCreateEdgeEntryEdgeNeverCycles` | retired（源不在能力图上） | `internal/store/v4test/edges_test.go:72 TestSourceEdgesProjection` |

#### `internal/store/edge_literals_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEdgeLiteralsRoundTrip` | retired（edges 无 literals 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestEdgeLiteralsDefaultEmpty` | retired（edges 无 literals 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestCreateEdgeLiteralsRejected` | retired（无 literals 输入） | `internal/store/v4test/edges_test.go:30 TestCapabilityEdgesProjection` |
| `TestCreateEdgeLiteralsNullIsAbsent` | retired（无 literals 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestCreateEdgeLiteralsAccepted` | retired（无 literals 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestCreateEdgeLiteralsIdempotent` | retired（无 literals 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestCreateEdgeLiteralsEntryEdge` | retired（无 literals 列） | `internal/store/v4test/edges_test.go:72 TestSourceEdgesProjection` |
| `TestGraphHashIncludesLiterals` | retired（graph_versions 删除） | `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` |

#### `internal/store/edge_mapping_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEntryEdgeMappingRoundTrip` | retired（edges 无 mapping 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestCapabilityEdgeMappingRejected` | retired（无 mapping 列） | `internal/store/v4test/edges_test.go:30 TestCapabilityEdgesProjection` |
| `TestEdgeMappingUndeclaredKeyRejected` | retired（映射=target.params 模板） | `internal/store/source_test.go:740 TestSourceTargetsParamsValidation` |
| `TestEdgeMappingNullNormalizedToEmpty` | retired（无 mapping 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestEdgeMappingShapeRejected` | retired（无 mapping 列） | `internal/store/source_test.go:740 TestSourceTargetsParamsValidation` |
| `TestEntryEdgeWildcardPortLookup` | retired（无端口） | `internal/store/v4test/edges_test.go:72 TestSourceEdgesProjection` |
| `TestGraphHashIncludesMapping` | retired（graph_versions 删除） | `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` |

#### `internal/store/edge_refs_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestLinkRefFollowsLatest` | migrated | 裸名→latest `internal/store/v4test/edges_test.go:118 TestRefResolution` |
| `TestLinkRefPinned` | migrated | name@version `internal/store/v4test/edges_test.go:118 TestRefResolution` |
| `TestLinkRefAcceptsLegacyID` | retired（v4 只接受 name/name@version） | `internal/store/v4test/edges_test.go:118 TestRefResolution` |
| `TestCreateEdgeRejectsUnresolvableRef` | migrated | `internal/store/v4test/edges_test.go:118 TestRefResolution` / `internal/store/capability_test.go:278 TestCapabilityValidation` |
| `TestEdgeRefCycleByNameAllowed` | migrated | `internal/store/v4test/edges_test.go:118 TestRefResolution` |
| `TestEdgeRefValidationUsesResolvedVersion` | migrated | `internal/store/capability_test.go:194 TestCapabilityVersionAppendOnly` / `internal/store/v4test/edges_test.go:118 TestRefResolution` |
| `TestGraphHashStableAcrossUpgrade` | retired（无图 hash） | `internal/store/capability_test.go:194 TestCapabilityVersionAppendOnly` |

#### `internal/store/edge_shape_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCapabilityEdgeShapeReadsPortsAndInbound` | migrated | 出边投影（无端口）`internal/store/v4test/edges_test.go:30 TestCapabilityEdgesProjection` |
| `TestCapabilityEdgeShapeIsolatesNamespaces` | migrated | `internal/store/capability_test.go:231 TestCapabilityNamespaceIsolation` |
| `TestCapabilityEdgeShapeRejectsWildcardNamespace` | migrated | `internal/store/namespace_test.go:70 TestNamespaceRequirementEnforced` |

#### `internal/store/emission_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEmissionRoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/emission_test.go:34 TestEmissionRoundTrip` |
| `TestInsertEmissionIdempotent` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/emission_test.go:69 TestInsertEmissionIdempotent` |
| `TestEmissionSeqStatus` | retired（v4 emits 无 seq/status） | `internal/store/emission_test.go:34 TestEmissionRoundTrip` / `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestEmissionListOrderedAndScoped` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/emission_test.go:146 TestEmissionListOrderedAndScoped` |
| `TestGetEmissionNotFound` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/emission_test.go:179 TestGetEmissionNotFound` |
| `TestInsertEmissionRejectsBadInput` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/emission_test.go:187 TestInsertEmissionRejectsBadInput` |
| `TestEnsureTaskToken` | migrated | grant 下发唯一 attempt token `internal/store/v4test/task_test.go:203 TestTaskAttemptToken` |
| `TestEnsureTaskTokenConcurrent` | migrated | `internal/store/v4test/task_test.go:203 TestTaskAttemptToken` / `internal/store/v4test/task_test.go:1030 TestTaskGrantBudgetExhaustedReleasesSlot` |
| `TestTaskTokenColumnMigrated` | retired（历史迁移链） | `internal/store/v4test/task_test.go:203 TestTaskAttemptToken` / `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` |
| `TestEmissionsTableMigrated` | retired（历史迁移链） | `internal/store/emission_test.go:290 TestEmitColumnsInBaseline` |

#### `internal/store/event_page_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestListEventsBeforeEquivalence` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_page_test.go:99 TestListEventsBeforeEquivalence` |
| `TestListEventsBeforeCursorSameCreatedAt` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_page_test.go:225 TestListEventsBeforeCursorSameCreatedAt` |
| `TestListEventsBeforeEmptyIsSlice` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_page_test.go:337 TestListEventsBeforeEmptyIsSlice` |
| `TestListEventsBeforeUsesRetentionIndex` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_page_test.go:385 TestListEventsBeforeUsesRetentionIndex` |

#### `internal/store/event_prune_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestInsertPruneEventIdempotent` | migrated | `internal/store/event_task_test.go:228 TestInsertPruneEventNeverDeduped` |
| `TestInsertPruneEventDefaultsAndValidation` | migrated | `internal/store/event_task_test.go:197 TestEventValidation` |
| `TestListEventsHidesPrunesFromStream` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_prune_test.go:74 TestListEventsHidesPrunesFromStream` |
| `TestInsertEventRejectsReservedSource` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_prune_test.go:110 TestInsertEventRejectsReservedSource` |

#### `internal/store/event_task_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEventIdempotency` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_task_test.go:19 TestEventIdempotency` |
| `TestEventWithoutIdemKeyAllowsDuplicates` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_task_test.go:53 TestEventWithoutIdemKeyAllowsDuplicates` |
| `TestEventIdemKeyScopedPerSourceAndNamespace` | migrated | `internal/store/event_task_test.go:114 TestEventDeliveryScopedPerSourceAndNamespace` |
| `TestEventGetAndList` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_task_test.go:144 TestEventGetAndList` |
| `TestEventValidation` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/event_task_test.go:197 TestEventValidation` |
| `TestReportResultWritesTerminalState` | migrated | `internal/store/task_test.go:410 TestCompleteRecordsResultAndFinishedAt` / `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestReportedColumnDistinguishesNoResultFrame` | migrated | `internal/store/task_test.go:410 TestCompleteRecordsResultAndFinishedAt`（reported/acceptedAt 事实） |
| `TestCreateTasksAtomic` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:105 TestCreateTasksAtomic` |
| `TestCreateTasksFanout` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:126 TestCreateTasksFanout` |
| `TestCreateTasksRejectsNonPending` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:155 TestCreateTasksRejectsNonPending` |
| `TestCreateTasksEmptyIsNoop` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:169 TestCreateTasksEmptyIsNoop` |
| `TestTaskRoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:177 TestTaskRoundTrip` |
| `TestListTasksEmptyIsArray` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:208 TestListTasksEmptyIsArray` |
| `TestListTasksFilters` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:225 TestListTasksFilters` |
| `TestTaskNamespaceIsolation` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:263 TestTaskNamespaceIsolation` |

#### `internal/store/graph_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEnsureGraphVersionUnchangedTakesNoWriteLock` | retired（graph_versions 删除） | `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` |
| `TestEnsureGraphVersionSnapshotRoundTrip` | retired（无图快照） | `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` |
| `TestGraphSnapshotPayloadMatchesContentHash` | retired（无图快照/hash） | `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` |
| `TestGraphVersionPathsUseSameNamespace` | retired（无图对象） | `internal/store/v4test/edges_test.go:30 TestCapabilityEdgesProjection` |

#### `internal/store/ids_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestBase62EncodeIsRealBase62` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/ids_test.go:11 TestBase62EncodeIsRealBase62` |
| `TestBase62EncodePreservesOrder` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/ids_test.go:30 TestBase62EncodePreservesOrder` |
| `TestBase62EncodeZero` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/ids_test.go:44 TestBase62EncodeZero` |
| `TestNewIDPrefixAndUniqueness` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/ids_test.go:54 TestNewIDPrefixAndUniqueness` |
| `TestNewIDTimeOrdered` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/ids_test.go:89 TestNewIDTimeOrdered` |
| `TestCapabilityAndGraphIDShape` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/ids_test.go:107 TestCapabilityAndGraphIDShape` |
| `TestVersionIDUniqueness` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/ids_test.go:128 TestVersionIDUniqueness` |
| `TestIDHashSuffix` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/ids_test.go:142 TestIDHashSuffix` |
| `TestItoa64` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/ids_test.go:155 TestItoa64` |

#### `internal/store/migrate_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestParseMigrations` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/migrate_test.go:17 TestParseMigrations` |
| `TestParseMigrationsRejectsGap` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/migrate_test.go:48 TestParseMigrationsRejectsGap` |
| `TestSchemaVersionRecorded` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/migrate_test.go:61 TestSchemaVersionRecorded` |
| `TestMigrationTwoColumns` | retired（v4 baseline 单块，历史迁移链删除） | `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` + `internal/store/v4_whitebox_test.go TestWhiteboxParseMigrationsContinuousLedger` |
| `TestMigrationTenDropsBalance` | retired（v4 baseline 单块，历史迁移链删除） | `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` + `internal/store/v4_whitebox_test.go TestWhiteboxParseMigrationsContinuousLedger` |
| `TestMigrationIdempotent` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/migrate_test.go:87 TestMigrationIdempotent` |
| `TestMigrationFailureRollsBack` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/migrate_test.go:127 TestMigrationFailureRollsBack` |
| `TestMigrationHookFailureRollsBack` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/migrate_test.go:161 TestMigrationHookFailureRollsBack` |
| `TestMigrationNoSwallowedErrors` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/migrate_test.go:194 TestMigrationNoSwallowedErrors` |
| `TestNoArgvOrKindInSchema` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/migrate_test.go:219 TestNoArgvOrKindInSchema` |
| `TestMigration11DeploymentsReferenceCapability` | retired（v4 baseline 单块，历史迁移链删除） | `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` + `internal/store/v4_whitebox_test.go TestWhiteboxParseMigrationsContinuousLedger` |
| `TestMigration11RefusesToDropUnmappedDeployment` | retired（v4 baseline 单块，历史迁移链删除） | `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` + `internal/store/v4_whitebox_test.go TestWhiteboxParseMigrationsContinuousLedger` |

#### `internal/store/namespace_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestNamespaceCreateSeedsBuiltins` | retired（v4 不播种） | `internal/store/namespace_test.go:165 TestNamespaceCreateSeedsBuiltinsNot` |
| `TestNamespaceCRUD` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/namespace_test.go:20 TestNamespaceCRUD` |
| `TestNamespaceRequirementEnforced` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/namespace_test.go:70 TestNamespaceRequirementEnforced` |
| `TestCrossNamespaceReferencesRejected` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/namespace_test.go:92 TestCrossNamespaceReferencesRejected` |
| `TestNamespaceRoundTrips` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/namespace_test.go:112 TestNamespaceRoundTrips` |
| `TestRegisterUserCreatesDefaultNamespace` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/namespace_test.go:140 TestRegisterUserCreatesDefaultNamespace` |

#### `internal/store/node_agent_version_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestMigration3AgentColumnsFromV2` | retired（历史升级链删除） | `internal/store/node_agent_version_test.go:23 TestAgentColumnsInBaseline` |
| `TestNodeAgentVersionRoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_agent_version_test.go:45 TestNodeAgentVersionRoundTrip` |
| `TestNodeAgentUpgradeStatusRoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_agent_version_test.go:90 TestNodeAgentUpgradeStatusRoundTrip` |
| `TestNodeAgentWritesRespectNamespace` | retired（v4 节点集群级，无 namespace_id） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/node_test.go:119 TestNodeNamespaceIsolation` |

#### `internal/store/node_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestNodeCreateAndGet` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:49 TestNodeCreateAndGet` |
| `TestNodeNamespaceIsolation` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:119 TestNodeNamespaceIsolation` |
| `TestNodeCapabilityUnknownIsStale` | migrated | `internal/store/node_test.go:286 TestNodeCapabilitiesReplacedOnRehello` / `internal/store/reconcile_test.go:138 TestReconcileAppliedDigestMismatchMarksPending` |
| `TestNodeCapabilitiesReplacedOnRehello` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:286 TestNodeCapabilitiesReplacedOnRehello` |
| `TestNodeCapabilitiesNoHarnessColumns` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:352 TestNodeCapabilitiesNoHarnessColumns` |
| `TestNodeMaxInstances` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:377 TestNodeMaxInstances` |
| `TestNodeInstancesUpsertAndShrink` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:405 TestNodeInstancesUpsertAndShrink` |
| `TestUpsertInstancesKeepsBusyTask` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:444 TestUpsertInstancesKeepsBusyTask` |
| `TestLeaseInstanceAndRelease` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:475 TestLeaseInstanceAndRelease` |
| `TestLeaseInstanceDeadSlotIgnored` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:532 TestLeaseInstanceDeadSlotIgnored` |
| `TestNodesForCapability` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:176 TestNodesForCapability` |
| `TestNodesForCapabilityNamespaceIsolation` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:248 TestNodesForCapabilityNamespaceIsolation` |
| `TestNodePins` | retired（v4 无 sticky pin） | 授权/租约是唯一执行事实 `internal/store/task_test.go:316 TestGrantMovesToRunning` / `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestPutPinValidation` | retired（v4 无 sticky pin） | 授权/租约是唯一执行事实 `internal/store/task_test.go:316 TestGrantMovesToRunning` / `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestNodeStatusTransitions` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:558 TestNodeStatusTransitions` |
| `TestTouchNode` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:612 TestTouchNode` |
| `TestNodeEmptyListIsArray` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:636 TestNodeEmptyListIsArray` |
| `TestNodeTagsRoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:660 TestNodeTagsRoundTrip` |
| `TestNewNodeTokenUnique` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:690 TestNewNodeTokenUnique` |
| `TestFreeSlotsByNode` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:714 TestFreeSlotsByNode` |
| `TestNodeCreateDuplicateName` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:103 TestNodeCreateDuplicateName` |
| `TestValidateNodeName` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/node_test.go:752 TestValidateNodeName` |
| `TestLeaseAndAssignWithPinWritesPinAtomically` | retired（v4 无 sticky pin） | 授权/租约是唯一执行事实 `internal/store/task_test.go:316 TestGrantMovesToRunning` / `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestLeaseAndAssignWithPinRollsBackPinOnFailure` | retired（v4 无 sticky pin） | 授权/租约是唯一执行事实 `internal/store/task_test.go:316 TestGrantMovesToRunning` / `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestRollbackPinOnlyDeletesOwnPin` | retired（v4 无 sticky pin） | 授权/租约是唯一执行事实 `internal/store/task_test.go:316 TestGrantMovesToRunning` / `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestRollbackPinSameSecondSameNode` | retired（v4 无 sticky pin） | 授权/租约是唯一执行事实 `internal/store/task_test.go:316 TestGrantMovesToRunning` / `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestRollbackPinValidation` | retired（v4 无 sticky pin） | 授权/租约是唯一执行事实 `internal/store/task_test.go:316 TestGrantMovesToRunning` / `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestMigration7PinIDWorksOnExistingDB` | retired（v4 baseline 单块，历史迁移链删除） | `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` + `internal/store/v4_whitebox_test.go TestWhiteboxParseMigrationsContinuousLedger` |

#### `internal/store/orphan_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestOrphanPendingNoNodeDeclares` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/orphan_test.go:73 TestOrphanPendingNoNodeDeclares` |
| `TestOrphanPendingNodeOffline` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/orphan_test.go:98 TestOrphanPendingNodeOffline` |
| `TestOrphanPendingOnlineNodeNotListed` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/orphan_test.go:119 TestOrphanPendingOnlineNodeNotListed` |
| `TestOrphanPendingRespectsAgeFloor` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/orphan_test.go:161 TestOrphanPendingRespectsAgeFloor` |
| `TestAbandonPendingTaskReleasesBackpressure` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/orphan_test.go:176 TestAbandonPendingTaskReleasesBackpressure` |
| `TestAbandonPendingTaskOnlyTouchesPending` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/orphan_test.go:221 TestAbandonPendingTaskOnlyTouchesPending` |
| `TestAbandonPendingTaskDefaultReason` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/orphan_test.go:250 TestAbandonPendingTaskDefaultReason` |

#### `internal/store/passkey_backup_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestPasskey_BackupEligibleRoundtrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/passkey_backup_test.go:12 TestPasskey_BackupEligibleRoundtrip` |

#### `internal/store/passkey_mgmt_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestPasskey_RenameRoundtrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/passkey_mgmt_test.go:11 TestPasskey_RenameRoundtrip` |
| `TestPasskey_DeleteRoundtrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/passkey_mgmt_test.go:54 TestPasskey_DeleteRoundtrip` |
| `TestPasskey_ListEmptyReturnsEmpty` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/passkey_mgmt_test.go:105 TestPasskey_ListEmptyReturnsEmpty` |
| `TestPasskey_LastUsedAtNullable` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/passkey_mgmt_test.go:125 TestPasskey_LastUsedAtNullable` |

#### `internal/store/password_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestUserPassword_DefaultEmpty` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/password_test.go:16 TestUserPassword_DefaultEmpty` |
| `TestUserPassword_SetGetRoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/password_test.go:35 TestUserPassword_SetGetRoundTrip` |
| `TestUserPassword_ClearResetsToZero` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/password_test.go:74 TestUserPassword_ClearResetsToZero` |
| `TestUserPassword_MissingUser` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/password_test.go:105 TestUserPassword_MissingUser` |
| `TestUserPassword_ColumnsExist` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/password_test.go:124 TestUserPassword_ColumnsExist` |
| `TestRegisterUserWithPassword_RoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/password_test.go:162 TestRegisterUserWithPassword_RoundTrip` |
| `TestRegisterUser_PasskeyPathKeepsPasswordEmpty` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/password_test.go:232 TestRegisterUser_PasskeyPathKeepsPasswordEmpty` |
| `TestRegisterUserWithPassword_ConcurrentFirstAdmin` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/password_test.go:268 TestRegisterUserWithPassword_ConcurrentFirstAdmin` |
| `TestRegisterUser_PreMigration8SchemaStillWorks` | retired（preMigration8 链退役） | `internal/store/password_test.go:162 TestRegisterUserWithPassword_RoundTrip` / `internal/store/v4test/migrate_test.go:92 TestLegacyDatabaseRejectedVersion` |

#### `internal/store/pending_backlog_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestOldestPendingCreatedAtEmpty` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pending_backlog_test.go:28 TestOldestPendingCreatedAtEmpty` |
| `TestOldestPendingCreatedAtIgnoresNonPending` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pending_backlog_test.go:44 TestOldestPendingCreatedAtIgnoresNonPending` |
| `TestOldestPendingCreatedAtIgnoresNotBefore` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pending_backlog_test.go:89 TestOldestPendingCreatedAtIgnoresNotBefore` |
| `TestPendingBacklogSQLShape` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pending_backlog_test.go:138 TestPendingBacklogSQLShape` |
| `TestPendingBacklogIndexQueryPlan` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pending_backlog_test.go:158 TestPendingBacklogIndexQueryPlan` |
| `TestPendingBacklogIndexIdempotent` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pending_backlog_test.go:200 TestPendingBacklogIndexIdempotent` |

#### `internal/store/playbook_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestPlaybookManifestRoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:171 TestPlaybookManifestRoundTrip` |
| `TestPlaybookCreateGetListDelete` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:204 TestPlaybookCreateGetListDelete` |
| `TestPlaybookEmptyManifestNormalized` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:266 TestPlaybookEmptyManifestNormalized` |
| `TestPlaybookSlugValidation` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:283 TestPlaybookSlugValidation` |
| `TestInstallPlaybookIdempotent` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:387 TestInstallPlaybookIdempotent` |
| `TestInstallPlaybookSkipsEdgeWithMissingEndpoint` | retired（无端/边条目） | 引用必须可解析 `internal/store/playbook_test.go:322 TestRenderPlaybookReadOnly` |
| `TestInstallPlaybookSkipsEdgeWithUnknownPort` | retired（无端口） | `internal/store/playbook_test.go:322 TestRenderPlaybookReadOnly` |
| `TestInstallPlaybookWebhookSourceIsUsable` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:505 TestInstallPlaybookWebhookSourceIsUsable` |
| `TestInstallPlaybookCronSource` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:549 TestInstallPlaybookCronSource` |
| `TestPlaybookChecksNodePresence` | retired（v4 无 checks 读模型） | 安装全有或全无 `internal/store/playbook_test.go:433 TestInstallPlaybookDryRunWritesNothing` / `internal/store/v4test/playbook_test.go:892 TestInstallConflictRollsBackWholePackage` |
| `TestPlaybookChecksPartialAndEntryCapabilities` | retired（v4 无 checks 读模型） | 安装全有或全无 `internal/store/playbook_test.go:433 TestInstallPlaybookDryRunWritesNothing` / `internal/store/v4test/playbook_test.go:892 TestInstallConflictRollsBackWholePackage` |
| `TestInstallPlaybookDoesNotOverwriteExistingDefinition` | migrated | `internal/store/playbook_test.go:387 TestInstallPlaybookIdempotent` / `internal/store/v4test/playbook_test.go:1038 TestInstallReuseNeverUpdatesExistingRow` |
| `TestPlaybookManifestReservedOriginNames` | migrated（父裁定#2 拆成两条；Source-name ROOT rework 实测通过） | 能力名 github/cron/http/manual 合法 allowlist `internal/store/playbook_test.go:767 TestPlaybookInstallAllowsV4CapabilityNamesThatWereReservedInV3`；源名 route 与直写同规则拒绝并整装 0 写 `internal/store/playbook_test.go:699 TestPlaybookInstallReservedSourceNameRejectedLikeDirectCreate`；定向红→绿及固定五树完整 vet/test 证据见末尾 Source-name report（不代表 G1 DONE） |
| `TestPlaybookInstallEmptyManifest` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:585 TestPlaybookInstallEmptyManifest` |
| `TestPlaybookChecksResolvesDeclaredVersion` | retired（v4 无 checks 读模型） | 安装全有或全无 `internal/store/playbook_test.go:433 TestInstallPlaybookDryRunWritesNothing` / `internal/store/v4test/playbook_test.go:892 TestInstallConflictRollsBackWholePackage` |
| `TestInstallPlaybookNewVersionSameSchema` | migrated | `internal/store/playbook_test.go:387 TestInstallPlaybookIdempotent` / `internal/store/v4test/playbook_test.go:1038 TestInstallReuseNeverUpdatesExistingRow` |
| `TestInstallPlaybookSkipsSourcePointingAtSameEntry` | retired（v3 manifest 条目模型删除） | v4 入口边只由 source targets 投影（无“指向同一入口”的边条目）：`internal/store/v4test/edges_test.go:72 TestSourceEdgesProjection` |
| `TestListPlaybooksCreatedAtOrder` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:666 TestListPlaybooksCreatedAtOrder` |
| `TestInstallPlaybookInlineSpecStoresScriptBody` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/playbook_test.go:469 TestInstallPlaybookInlineSpecStoresScriptBody` |
| `TestInstallPlaybookRefDoesNotCreateCapability` | retired（引用只解析、不建行） | `internal/store/playbook_test.go:322 TestRenderPlaybookReadOnly` |
| `TestInstallPlaybookRejectsForeignHandler` | retired（无 handler） | `internal/store/playbook_test.go:283 TestPlaybookSlugValidation` |
| `TestInstallPlaybookRefAndSpecBothGiven` | retired（v3 manifest 双写形状） | `internal/store/playbook_test.go:322 TestRenderPlaybookReadOnly` |
| `TestInstallPlaybookEdgeCarriesLiteralsAndPriority` | retired（无 literals/priority） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestInstallPlaybookTriggerNameAndMapping` | retired（无触发器映射） | `internal/store/playbook_test.go:505 TestInstallPlaybookWebhookSourceIsUsable` |
| `TestInstallPlaybookLegacyManifestStillInstalls` | retired（v3 manifest 不兼容） | `internal/store/playbook_test.go:283 TestPlaybookSlugValidation` |
| `TestPlaybookChecksHubHandlerRefIsReady` | retired（v4 无 checks 读模型） | 安装全有或全无 `internal/store/playbook_test.go:433 TestInstallPlaybookDryRunWritesNothing` / `internal/store/v4test/playbook_test.go:892 TestInstallConflictRollsBackWholePackage` |
| `TestInstallPlaybookRejectsUnresolvableRef` | migrated | Parse/Render 统一拒绝 `internal/store/playbook_test.go:322 TestRenderPlaybookReadOnly` |

#### `internal/store/pool_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestPoolMaxOpenConnsConfig` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pool_test.go:28 TestPoolMaxOpenConnsConfig` |
| `TestPoolReadsRunInParallel` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pool_test.go:102 TestPoolReadsRunInParallel` |
| `TestPoolWriteTxIsImmediateAndDoesNotBlockReaders` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pool_test.go:170 TestPoolWriteTxIsImmediateAndDoesNotBlockReaders` |
| `TestPoolConcurrentWritesNoLockErrors` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pool_test.go:237 TestPoolConcurrentWritesNoLockErrors` |
| `TestMaxConnsFromEnv` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/pool_test.go:340 TestMaxConnsFromEnv` |

#### `internal/store/priority_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestEdgePriorityRoundTrip` | retired（edges.priority 列删除） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestEdgePriorityInGraphSnapshot` | retired（无优先级/图快照） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` |
| `TestCreateEdgeRejectsOutOfRangePriority` | retired（无 priority 输入） | `internal/store/v4test/capability_test.go:403 TestCapabilityCreateProjectsEdges` |

#### `internal/store/reconcile_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestReconcileAppliedRoundTrip` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/reconcile_test.go:31 TestReconcileAppliedRoundTrip` |
| `TestReconcileAppliedFullReplace` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/reconcile_test.go:56 TestReconcileAppliedFullReplace` |
| `TestReconcileAppliedToleratesStaleCache` | migrated | `internal/store/reconcile_test.go:31 TestReconcileAppliedRoundTrip` / `internal/store/reconcile_test.go:56 TestReconcileAppliedFullReplace` |
| `TestReconcileAppliedListsAllStale` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/reconcile_test.go:84 TestReconcileAppliedListsAllStale` |
| `TestReconcileAppliedDigestMismatchMarksPending` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/reconcile_test.go:138 TestReconcileAppliedDigestMismatchMarksPending` |
| `TestReconcileAppliedDigestMatchMarksApplied` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/reconcile_test.go:169 TestReconcileAppliedDigestMatchMarksApplied` |
| `TestReconcileAppliedFeedsDispatch` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/reconcile_test.go:192 TestReconcileAppliedFeedsDispatch` |

#### `internal/store/report_next_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestReportResultAndCreateTasksSuccess` | migrated | 结果事实 `internal/store/task_test.go:410 TestCompleteRecordsResultAndFinishedAt`；子任务原子 `internal/store/emission_test.go:257 TestEmitCreatesChildTasksAtomically` |
| `TestReportResultAndCreateTasksRollsBackOnChildFailure` | migrated | `internal/store/v4test/emit_test.go:472 TestEmitChildFailureRollsBackAll` |
| `TestReportResultAndCreateTasksRollsBackOnReportFailure` | migrated | 结果/绑定零写：`internal/store/task_test.go:819 TestCompleteRejectsInvalidResultPhaseWithZeroWrites` + `internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestReportResultCompatWithoutNext` | retired（无兼容入口） | 出口仅 emits；缺 to=missing_target |
| `TestReportResultAndCreateTasksChildIsRedispatchable` | migrated | `internal/store/emission_test.go:257 TestEmitCreatesChildTasksAtomically` / `internal/store/task_test.go:289 TestAssignRecordsCandidateWithoutAttempt` |

#### `internal/store/retention_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestRetentionDeletesExpiredTerminalTask` | migrated | `internal/store/retention_test.go:148 TestRetentionDeletesExpiredTerminalTasks` / `internal/store/v4test/retention_test.go:36 TestRetentionTaskTTL` |
| `TestRetentionKeepsNotYetExpiredTask` | migrated | `internal/store/retention_test.go:148 TestRetentionDeletesExpiredTerminalTasks` |
| `TestRetentionIsIdempotent` | migrated | `internal/store/retention_test.go:279 TestRetentionPrimitiveLimits` |
| `TestRetentionKeepsNonTerminalTasks` | migrated | `internal/store/retention_test.go:115 TestRetentionKeepsActiveTasks` |
| `TestRetentionSkipsCapabilitiesWithoutTTL` | migrated | `internal/store/retention_test.go:35 TestRetentionTTLZeroIsPermanent` |
| `TestRetentionBatchLimitAndTruncation` | migrated | `internal/store/retention_test.go:279 TestRetentionPrimitiveLimits` |
| `TestRetentionNamespaceIsolation` | migrated | `internal/store/v4test/retention_test.go:36 TestRetentionTaskTTL` |
| `TestRetentionEventsDisabledByDefault` | migrated | `internal/store/retention_test.go:35 TestRetentionTTLZeroIsPermanent` |
| `TestRetentionEventsCustomRetention` | migrated | `internal/store/retention_test.go:60 TestRetentionDeletesExpiredEvents` |
| `TestTerminalStatusListMatchesIsTerminalStatus` | migrated | `internal/store/task_test.go:73 TestTaskPhaseSet` |
| `TestRetentionDeletesAllTerminalStatuses` | migrated | `internal/store/retention_test.go:148 TestRetentionDeletesExpiredTerminalTasks` |
| `TestRetentionDeleteRejectsNonPositiveLimit` | migrated | `internal/store/retention_test.go:279 TestRetentionPrimitiveLimits` |
| `TestListRetentionNamespaceIDs` | retired（v4 保留期按库+ns 行） | `internal/store/v4test/retention_test.go:36 TestRetentionTaskTTL` |
| `TestSweepRetentionHonoursCancelledContext` | migrated | `internal/store/retention_test.go:279 TestRetentionPrimitiveLimits` |
| `TestRetentionPartialReportOnCancel` | retired（v4 一次性 stats） | `internal/store/retention_test.go:279 TestRetentionPrimitiveLimits` |
| `TestRetentionEventsNamespaceIsolation` | migrated | `internal/store/retention_test.go:60 TestRetentionDeletesExpiredEvents` |
| `TestRetentionBadLifecycleDoesNotPoisonSweep` | retired（生命周期来自钉住正文） | `internal/store/retention_test.go:115 TestRetentionKeepsActiveTasks` |
| `TestRetentionIndexesUsed` | migrated | `internal/store/v4test/schema_baseline_test.go:222 TestSchemaBaselineIndexes` |

#### `internal/store/schema_baseline_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestBaselineTableSet` | retired（v3 baseline 期望） | v4 结构门禁 `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` / `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/v4test/schema_baseline_test.go:222 TestSchemaBaselineIndexes` / `internal/store/v4test/schema_baseline_test.go:295 TestSchemaBaselineUniqueConstraints` / `internal/store/v4test/schema_baseline_test.go:414 TestSchemaBaselinePrimaryKeyIdentity` |
| `TestBaselineNoHistoricalBaggage` | retired（v3 baseline 期望） | v4 结构门禁 `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` / `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/v4test/schema_baseline_test.go:222 TestSchemaBaselineIndexes` / `internal/store/v4test/schema_baseline_test.go:295 TestSchemaBaselineUniqueConstraints` / `internal/store/v4test/schema_baseline_test.go:414 TestSchemaBaselinePrimaryKeyIdentity` |
| `TestBaselineForeignKeys` | retired（v3 baseline 期望） | v4 结构门禁 `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` / `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/v4test/schema_baseline_test.go:222 TestSchemaBaselineIndexes` / `internal/store/v4test/schema_baseline_test.go:295 TestSchemaBaselineUniqueConstraints` / `internal/store/v4test/schema_baseline_test.go:414 TestSchemaBaselinePrimaryKeyIdentity` |
| `TestBaselineIndexes` | migrated（当前 v4 查询索引不变量保留；非 v3 表/status 兼容） | `internal/store/v4test/schema_baseline_test.go:222 TestSchemaBaselineIndexes`：独立硬编码名字/表/完整列序/unique/partial 与真实 PRAGMA 比对；包括 tasks(namespace_id,created_at,id)、events trace/retention、tasks retention/lease/node/parent/trace/pending 及 partial predicate，不从同份 SQL 推导期望 |
| `TestBaselineUniqueConstraints` | retired（v3 baseline 期望） | v4 结构门禁 `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` / `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/v4test/schema_baseline_test.go:222 TestSchemaBaselineIndexes` / `internal/store/v4test/schema_baseline_test.go:295 TestSchemaBaselineUniqueConstraints` / `internal/store/v4test/schema_baseline_test.go:414 TestSchemaBaselinePrimaryKeyIdentity` |
| `TestBaselinePrimaryKeys` | retired（v3 baseline 期望） | v4 结构门禁 `internal/store/v4test/schema_baseline_test.go:62 TestSchemaBaselineTableSet` / `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/v4test/schema_baseline_test.go:222 TestSchemaBaselineIndexes` / `internal/store/v4test/schema_baseline_test.go:295 TestSchemaBaselineUniqueConstraints` / `internal/store/v4test/schema_baseline_test.go:414 TestSchemaBaselinePrimaryKeyIdentity` |
| `TestBaselineIntegrity` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/schema_baseline_test.go:100 TestBaselineIntegrity` |
| `TestBaselineReopenIsIdempotent` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/schema_baseline_test.go:122 TestBaselineReopenIsIdempotent` |
| `TestLegacyDatabaseRejected` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/schema_baseline_test.go:168 TestLegacyDatabaseRejected` |

#### `internal/store/seq_in_trace_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestSeqInTraceConcurrentAssignIsUnique` | retired（tasks 无 seq_in_trace 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/task_test.go:225 TestListTasksFilters` |
| `TestSeqInTraceConcurrentLeaseAndAssignIsUnique` | retired（tasks 无 seq_in_trace 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/task_test.go:225 TestListTasksFilters` |
| `TestSeqInTraceGroupsByTraceAndCapabilityName` | retired（tasks 无 seq_in_trace 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/task_test.go:225 TestListTasksFilters` |
| `TestSeqInTraceStableAcrossRedispatch` | retired（tasks 无 seq_in_trace 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/task_test.go:225 TestListTasksFilters` |
| `TestSeqInTraceRoundTrip` | retired（tasks 无 seq_in_trace 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/task_test.go:225 TestListTasksFilters` |
| `TestSeqInTraceZeroUntilDispatched` | retired（tasks 无 seq_in_trace 列） | `internal/store/v4test/schema_baseline_test.go:90 TestSchemaBaselineNoHistoricalBaggage` / `internal/store/task_test.go:225 TestListTasksFilters` |
| `TestMigration5SeqInTraceWorksOnExistingDB` | retired（v4 baseline 单块，历史迁移链删除） | `internal/store/v4test/migrate_test.go:20 TestMigrationSchemaVersion` + `internal/store/v4_whitebox_test.go TestWhiteboxParseMigrationsContinuousLedger` |

#### `internal/store/session_lease_safety_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestResolveLostTasksAbandonsNonIdempotentStarted` | migrated | `internal/store/task_test.go:567 TestRecoverLostLeasesRequeues` 的 non-idempotent-abandons 子用例 |
| `TestResolveLostTasksAbandonsWhenAttemptsExhausted` | migrated | 预算耗尽精确断言（ErrMaxAttemptsExceeded → abandoned + reason + busySlots==0）：`internal/store/v4test/task_test.go:1030 TestTaskGrantBudgetExhaustedReleasesSlot`；另 `internal/store/task_test.go:617 TestMaxAttemptsExceededAbandons` / `internal/store/v4test/task_test.go:93 TestTaskMaxAttemptsTerminal`（非幂等租约过期路径） |
| `TestResolveLostTasksAssignedNotStartedBeatsNonIdempotent` | narrow-retired（v4 无 assigned 相位） | AssignTask 只记候选、不改相位：`internal/store/task_test.go:289 TestAssignRecordsCandidateWithoutAttempt` |
| `TestResolveLostTasksCleanEndOfIdempotentStartedStillRequeues` | migrated | `internal/store/task_test.go:567 TestRecoverLostLeasesRequeues` 的 idempotent-requeues 子用例 |

#### `internal/store/session_lease_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestExtendLeasesForTasksOnlyListedIDs` | migrated（语义收窄：per-task 续租） | `internal/store/task_test.go:381 TestRenewLeaseAuthorized` + `internal/store/v4test/task_test.go:1423 TestRenewLeaseCannotReviveExpiredAuthorization` |
| `TestResolveLostTasksRequeue` | migrated（语义收紧） | 重排**不释放物理槽**：busy 保留到可信回收事实（`internal/store/task.go:980` 注释；`internal/store/task_test.go:567 TestRecoverLostLeasesRequeues`）；retry_backoff/next_attempt_at 见 `internal/store/v4test/task_test.go:307 TestTaskRecoverIdempotentRequeues` |
| `TestResolveLostTasksAbandon` | migrated（语义收紧） | 非幂等/预算耗尽直接终局**且槽位保持占用**（等可信回收）：`internal/store/task_test.go:567 TestRecoverLostLeasesRequeues` / `internal/store/task_test.go:617 TestMaxAttemptsExceededAbandons` |
| `TestResolveLostTasksSkipsStaleGeneration` | narrow-retired（v4 无 generation） | (attempt, lease_id) 精确绑定：`internal/store/v4test/task_test.go:1124 TestTaskCompleteRejectsOldAttemptLeaseAndExpiry` |
| `TestResolveLostTasksAssignedNotStartedRequeuesEvenWhenAbandon` | narrow-retired（无 assigned 相位） | `internal/store/task_test.go:289 TestAssignRecordsCandidateWithoutAttempt` |
| `TestCreateTasksRejectsOversizedParams` | migrated（旧“窄断言”已补齐） | 混批 good+oversized→0 行、恰好上限接受并逐字节往返、`*ParamsTooLargeError{TaskID,Size,Limit}` detail：`internal/store/task_test.go:755 TestCreateTasksMixedBatchOversizedParamsWritesNothing`；批量原子机制另见 `internal/store/task_test.go:105 TestCreateTasksAtomic` |
| `TestReclaimStaleSlots` | migrated（语义收紧，非同一不变量） | v4 不再按相位推断清槽：正向回收必须带**节点 idle 观测**且 task/attempt/lease 全匹配（`internal/store/v4test/cluster_node_test.go:421 TestNodeInstancesAndLeases`）；live/unknown/wrong-lease 一律 fail-closed（回收 0） |

#### `internal/store/source_entry_edges_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestSourceTargetsRoundTripFromEdges` | migrated | `internal/store/v4test/edges_test.go:72 TestSourceEdgesProjection` |
| `TestUpdateSourceRebuildsEntryEdges` | migrated | `internal/store/v4test/source_test.go:209 TestSourceTargetsRebuildEdges`：235-253 真实 UpdateSource 将 analyze 换成 notify，旧边消失、新边唯一、GetSource.targets 往返一致；不是直接改投影或只验 CRUD |
| `TestRenameSourceMovesEntryEdges` | retired（v4 源名不可变） | `internal/store/source_test.go:484 TestUpdateSourceRejectsReservedName` |
| `TestDeleteSourceClearsEntryEdges` | migrated | `internal/store/source_test.go:144 TestSourceCRUD` |
| `TestCreateSourceRejectsBadMapping` | retired（无 mapping） | `internal/store/source_test.go:740 TestSourceTargetsParamsValidation` |

#### `internal/store/source_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestCronParsing` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:27 TestCronParsing` |
| `TestCronNext` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:58 TestCronNext` |
| `TestCronDomDowOrSemantics` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:88 TestCronDomDowOrSemantics` |
| `TestCronNeverFires` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:105 TestCronNeverFires` |
| `TestSourceCRUD` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:144 TestSourceCRUD` |
| `TestSourceValidation` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:216 TestSourceValidation` |
| `TestSourceWebhookToken` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:288 TestSourceWebhookToken` |
| `TestSourceKindImmutable` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:348 TestSourceKindImmutable` |
| `TestDueCronSources` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:379 TestDueCronSources` |
| `TestCreateSourceRejectsReservedName` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:444 TestCreateSourceRejectsReservedName` |
| `TestUpdateSourceRejectsReservedName` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/source_test.go:484 TestUpdateSourceRejectsReservedName` |
| `TestValidateVarDefs` | migrated | `internal/store/source_test.go:740 TestSourceTargetsParamsValidation` |
| `TestSourceVarDefsParsing` | migrated | `internal/store/source_test.go:640 TestSourceConfigRoundTrip` |
| `TestSourceConfigPayloadParsing` | migrated | `internal/store/source_test.go:538 TestSourceConfigPayloadOnlyForKeepingKinds` |
| `TestSourceConfigKeepsVars` | migrated | `internal/store/source_test.go:640 TestSourceConfigRoundTrip` |
| `TestValidateSourceConfigPayloadOnlyForKeepingKinds` | migrated | `internal/store/source_test.go:538 TestSourceConfigPayloadOnlyForKeepingKinds` |
| `TestValidateSourceConfigRejectsBadVars` | migrated | `internal/store/source_test.go:740 TestSourceTargetsParamsValidation` |
| `TestSourceResultSchemaRoundTrip` | migrated | `internal/store/source_test.go:804 TestSourcePayloadSchemaRoundTrip` |
| `TestSourceResultSchemaValidation` | migrated | `internal/store/source_test.go:850 TestSourcePayloadSchemaValidation` |
| `TestUpdateSourceKeepsResultSchema` | migrated | `internal/store/source_test.go:907 TestUpdateSourceKeepsPayloadSchema` |

#### `internal/store/stats_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestMaintainStatsFreshDBIsQuiet` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_test.go:42 TestMaintainStatsFreshDBIsQuiet` |
| `TestMaintainStatsSuggestionFilter` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_test.go:57 TestMaintainStatsSuggestionFilter` |
| `TestMaintainStatsRefreshesDriftedStat1` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_test.go:119 TestMaintainStatsRefreshesDriftedStat1` |
| `TestMaintainStatsKeepsQuietWhenStat1IsFresh` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_test.go:174 TestMaintainStatsKeepsQuietWhenStat1IsFresh` |
| `TestStatsDrifted` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_test.go:203 TestStatsDrifted` |
| `TestMaintainStatsDoesNotTouchData` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_test.go:227 TestMaintainStatsDoesNotTouchData` |
| `TestMaintainStatsSkipsDriftCheckWhenTableEmpty` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_test.go:248 TestMaintainStatsSkipsDriftCheckWhenTableEmpty` |

#### `internal/store/stats_tz_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStartOfLocalDayUnix` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_tz_test.go:70 TestStartOfLocalDayUnix` |
| `TestMessageStatsTodayUsesLocalMidnight` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_tz_test.go:92 TestMessageStatsTodayUsesLocalMidnight` |
| `TestMessageStatsDailyUsesLocalDay` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_tz_test.go:122 TestMessageStatsDailyUsesLocalDay` |
| `TestMessageStatsTotalsDeliveryAndDevices` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_tz_test.go:159 TestMessageStatsTotalsDeliveryAndDevices` |
| `TestMessageStatsPublicUsesRealClock` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_tz_test.go:218 TestMessageStatsPublicUsesRealClock` |
| `TestMessageStatsRejectsEmptyNamespace` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stats_tz_test.go:241 TestMessageStatsRejectsEmptyNamespace` |

#### `internal/store/stmt_evict_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStmtCacheEvictsUnusableStmt` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_evict_test.go:57 TestStmtCacheEvictsUnusableStmt` |
| `TestStmtCacheFullFallsBackToDirectQuery` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_evict_test.go:204 TestStmtCacheFullFallsBackToDirectQuery` |

#### `internal/store/stmt_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStmtCachePrepareGate` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:220 TestStmtCachePrepareGate` |
| `TestStmtCacheBoundedByMaxCachedStmts` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:349 TestStmtCacheBoundedByMaxCachedStmts` |
| `TestStmtCacheDoesNotCachePrepareFailure` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:424 TestStmtCacheDoesNotCachePrepareFailure` |
| `TestStmtCacheErrorTextMatchesDirectQuery` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:474 TestStmtCacheErrorTextMatchesDirectQuery` |
| `TestStmtCacheZeroValueWorks` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:501 TestStmtCacheZeroValueWorks` |
| `TestStmtCacheUnusableStmtFallsBack` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:524 TestStmtCacheUnusableStmtFallsBack` |
| `TestStmtCacheUnusableDoesNotMatchSqlLogicErrors` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:578 TestStmtCacheUnusableDoesNotMatchSqlLogicErrors` |
| `BenchmarkListTasks` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:822 BenchmarkListTasks` |
| `BenchmarkListTasksBaseline` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:840 BenchmarkListTasksBaseline` |
| `BenchmarkGetSession` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:870 BenchmarkGetSession` |
| `BenchmarkGetSessionBaseline` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_test.go:886 BenchmarkGetSessionBaseline` |

#### `internal/store/stmt_wiring_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStmtWiringAllReadPathsCached` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_wiring_test.go:385 TestStmtWiringAllReadPathsCached` |
| `TestStmtWiringOptionalFiltersAreDistinctSQLs` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/stmt_wiring_test.go:456 TestStmtWiringOptionalFiltersAreDistinctSQLs` |

#### `internal/store/store_bench_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `BenchmarkListEnabledDevices` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/store_bench_test.go:25 BenchmarkListEnabledDevices` |

#### `internal/store/stub_signature_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestStubSignaturesFrozen` | retired（v3 签名清单） | 真实签名由各域新测试编译期使用钉住 |
| `TestNoStubRemains` | restored | `internal/store/v4_whitebox_test.go TestWhiteboxNoStubRemains`（静态：生产码不得 return ErrNotImplemented） |
| `TestIsTerminalStatus` | migrated | `internal/store/task_test.go:73 TestTaskPhaseSet`（IsTerminalPhase） |
| `TestTaskStructJSONShape` | migrated | `internal/store/task_test.go:725 TestTaskTokenNeverSerializedToJSON` |
| `TestCapabilityHasNoKind` | migrated | `internal/store/migrate_test.go:219 TestNoArgvOrKindInSchema` |
| `TestNodeHidesTokenHash` | restored | `internal/store/node_test.go:895 TestNodeHidesTokenHash` |

#### `internal/store/task_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestValidTransitionTable` | retired（v4 无 Transition） | `internal/store/task_test.go:73 TestTaskPhaseSet` |
| `TestTransition` | retired（v4 无 Transition） | `internal/store/task_test.go:316 TestGrantMovesToRunning` |
| `TestTerminalImmutable` | migrated | `internal/store/task_test.go:507 TestTerminalPhaseIsImmutable` |
| `TestTransitionFinishedAt` | migrated | `internal/store/task_test.go:410 TestCompleteRecordsResultAndFinishedAt` |
| `TestTransitionCASAgainstConcurrency` | migrated | `internal/store/task_test.go:359 TestGrantRejectsStaleAssignment` + `internal/store/v4test/task_test.go:71 TestTaskGrantRejectsStaleAssignment` |
| `TestTransitionNotFound` | retired（无 Transition） | `internal/store/task_test.go:316 TestGrantMovesToRunning` |
| `TestAssignTask` | migrated | `internal/store/task_test.go:289 TestAssignRecordsCandidateWithoutAttempt` |
| `TestExtendLease` | migrated | `internal/store/task_test.go:381 TestRenewLeaseAuthorized` + `internal/store/v4test/task_test.go:177 TestTaskRenewLease` |
| `TestExtendLeasesForNode` | retired（per-task 续租，无批量） | `internal/store/task_test.go:381 TestRenewLeaseAuthorized` |
| `TestSweepLeases` | migrated | `internal/store/task_test.go:567 TestRecoverLostLeasesRequeues` + `internal/store/v4test/task_test.go:307 TestTaskRecoverIdempotentRequeues` |
| `TestSweepLeasesAttemptsExhausted` | migrated | `internal/store/task_test.go:617 TestMaxAttemptsExceededAbandons` + `internal/store/v4test/task_test.go:93 TestTaskMaxAttemptsTerminal` |
| `TestSweepLeasesNonIdempotentAbandons` | migrated | `internal/store/task_test.go:567 TestRecoverLostLeasesRequeues`（non-idempotent-abandons） |
| `TestSweepLeasesIgnoresLiveLeases` | migrated | `internal/store/v4test/task_test.go:642 TestTaskOnlineLeaseExpiryKeepsSlotUntilNodeFact` |
| `TestSweepLeasesAssignedExpires` | retired（无 assigned 相位） | `internal/store/task_test.go:289 TestAssignRecordsCandidateWithoutAttempt` |
| `TestSweepLeasesIdempotent` | migrated | `internal/store/task_test.go:567 TestRecoverLostLeasesRequeues` + `internal/store/v4test/task_test.go:307 TestTaskRecoverIdempotentRequeues` |
| `TestCancelTask` | migrated | `internal/store/task_test.go:535 TestCancelRunningAwaitsAck` + `internal/store/v4test/task_test.go:234 TestTaskCancelWaitsForReap` |
| `TestCancelPendingTask` | migrated | `internal/store/task_test.go:507 TestTerminalPhaseIsImmutable` + `internal/store/v4test/task_test.go:287 TestTaskCancelPendingIsTerminal` |
| `TestReportResultFromAssignedImpliesRunning` | retired（v4 无隐含 running） | `internal/store/task_test.go:454 TestCompleteRequiresGrant` |
| `TestMarkRunningIdempotent` | retired（无独立 start 入口） | `internal/store/task_test.go:316 TestGrantMovesToRunning` |
| `TestCountTasksByStatus` | migrated | `internal/store/task_test.go:225 TestListTasksFilters` |
| `TestSweepLeasesNoExpiryIsNoop` | migrated | `internal/store/task_test.go:567 TestRecoverLostLeasesRequeues` + `internal/store/v4test/task_test.go:307 TestTaskRecoverIdempotentRequeues` |
| `TestLeaseConstants` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:706 TestLeaseConstants` |
| `TestLifecycleMaxAttemptsFromJSON` | migrated | 派发时从钉住正文取预算 `internal/store/task_test.go:617 TestMaxAttemptsExceededAbandons` |
| `TestStatusWriterIsUnique` | retired（v4 无 status writer） | `internal/store/task_test.go:73 TestTaskPhaseSet` |
| `TestSweepLeasesGuardAgainstHeartbeatRace` | migrated | `internal/store/task_test.go:381 TestRenewLeaseAuthorized` + `internal/store/v4test/task_test.go:642 TestTaskOnlineLeaseExpiryKeepsSlotUntilNodeFact` |
| `TestTransitionGuardFailedSentinel` | retired（v4 无 Transition guard 哨兵） | 被拒 grant 的错误分类：`internal/store/task_test.go:359 TestGrantRejectsStaleAssignment` |
| `TestSweepNeverStartedNonIdempotentRequeues` | migrated | `internal/store/task_test.go:567 TestRecoverLostLeasesRequeues` |
| `TestSweepRunningNonIdempotentAbandons` | migrated | `internal/store/task_test.go:567 TestRecoverLostLeasesRequeues` |
| `TestSweepNeverStartedAttemptsExhaustedAbandons` | migrated | `internal/store/task_test.go:617 TestMaxAttemptsExceededAbandons` |
| `TestTaskDigestRoundTrip` | migrated | `internal/store/task_test.go:177 TestTaskRoundTrip` |
| `TestLeaseAndAssignWritesTaskToken` | migrated | `internal/store/v4test/task_test.go:203 TestTaskAttemptToken` |
| `TestAssignTaskAlsoWritesToken` | migrated | `internal/store/v4test/task_test.go:203 TestTaskAttemptToken` |
| `TestTaskTokenNeverSerializedToJSON` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/task_test.go:725 TestTaskTokenNeverSerializedToJSON` |

#### `internal/store/tx_merge_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestReportResultMatchesSplitWrites` | retired（v4 单写路径，无拆分版对照） | 结果事实逐字段 `internal/store/v4test/result_facts_test.go:33 TestC3bCompleteResultFullJSONNanoseconds` |
| `TestLeaseAndAssignMatchesSplitWrites` | retired（同上） | 派发事实 `internal/store/task_test.go:289 TestAssignRecordsCandidateWithoutAttempt` + `internal/store/task_test.go:316 TestGrantMovesToRunning` |
| `TestProgressTaskMatchesSplitWrites` | retired（v4 无 ProgressTask） |  |
| `TestLeaseAndAssignGuardFailureDoesNotLeakSlot` | migrated | `internal/store/task_test.go:359 TestGrantRejectsStaleAssignment`（被拒 grant busy=0） |
| `TestReportResultRejectsTerminalTaskWithoutTouchingSlot` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/tx_merge_test.go:115 TestReportResultRejectsTerminalTaskWithoutTouchingSlot` |
| `TestReportResultDoesNotReleaseForeignSlot` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/tx_merge_test.go:66 TestReportResultDoesNotReleaseForeignSlot` |
| `TestProgressTaskRejectsTerminalTask` | retired（无 ProgressTask） | `internal/store/task_test.go:507 TestTerminalPhaseIsImmutable` |

#### `internal/store/upgrade_gate_test.go`

| old case | outcome | new case / reason / pending |
|---|---|---|
| `TestUpgradeGateRemovedPort` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/upgrade_gate_test.go:76 TestUpgradeGateRemovedPort` |
| `TestUpgradeGateListsAffectedLinks` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/upgrade_gate_test.go:134 TestUpgradeGateListsAffectedLinks` |
| `TestUpgradeGateAllowsAdditiveChanges` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/upgrade_gate_test.go:181 TestUpgradeGateAllowsAdditiveChanges` |
| `TestUpgradeGateParamTypeChanged` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/upgrade_gate_test.go:206 TestUpgradeGateParamTypeChanged` |
| `TestUpgradeGateOnlyForLatest` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/upgrade_gate_test.go:236 TestUpgradeGateOnlyForLatest` |
| `TestUpgradeImpactEmptyOnFirstVersion` | kept（同名源码保留；B 原包实际执行绿） | `internal/store/upgrade_gate_test.go:275 TestUpgradeImpactEmptyOnFirstVersion` |

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

---

# B 轮结果（tests-only 原包/白盒闭合；不声明 G1/T1 DONE）

cwd `/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery`
起点 checkpoint `5d44685` + WIP；未提交、未 push（sole writer，worktree:false）。

## 1. 已知 31 红 → 0（原 store 包）

按父裁定用**真夹具**修输入/调用/模式，不弱化断言；按名退役纯 v3 历史链并用真实 v4 边界替代：

- CAS/最后 ns：`authz_test.go`、`namespace_test.go`（Delete/Config 走 `ResourceVersion`，末 ns 先补一个真实 ns）；
- Spec 正文：`TestCapabilitySpecRoundTrip` 改成 `vars.<NAME>` / 封闭 `fieldPath` / 声明 `run.env`（落库断言 canonical JSON）；
- 事件/保留期：prune 码 `no_edge`→`no_target`、trace 夹具插入前写 TraceID、`status`→`phase`、`ReclaimStaleSlots` 语义注释；
- emission：跨任务 emit 用**另一台真实节点**（1 slot 不超卖）；
- pending/backlog：assign 用真实 online 节点；探针 SQL 跟随产品参数化形状（`phase = ?`）；
- retention：终态未回收 attempt 走真实 recovered-abandoned 路径，再用 `ReapTaskAttempt` 精确回收；
- task：结果必须带节点执行时间；未 grant 的绑定错误是 `ErrLeaseMismatch`；取消原因空→`ReasonCancelledByUser`；
  `maxAttempts` 在**派发时**从钉住正文取（建行默认 3）；节点授权按 ns slug；
- migrate/baseline：`TestMigrationTwoColumns`/`TestMigrationTenDropsBalance`/preMigration8 链、6 条 v3 `TestBaseline*`
  **按名退役**（v4 baseline 单块，见 §3 白盒与 `v4test/schema_baseline_test.go` 真实结构门禁）；`TestLegacyDatabaseRejected`
  文案改到现 v4 可操作错误（不删数据/不补标记语义保留）。

## 2. 限域 review 修复 + 新具名用例（全部原包实际执行）

| 新 case | 位置 | 断言 |
|---|---|---|
| `TestCreateTasksMixedBatchOversizedParamsWritesNothing` | `internal/store/task_test.go` | good+oversized 混批→0 行；恰好上限接受+逐字节往返；`*ParamsTooLargeError{TaskID,Size,Limit}` |
| `TestCompleteRejectsInvalidResultPhaseWithZeroWrites` | `internal/store/task_test.go` | 有效 auth/lease/attempt 走到**相位闸门**；task/attempt 整行、attempt 账本、槽位 0 写 |
| `TestCompleteErrorProjectionKeepsRawResultFacts` | `internal/store/task_test.go` | 失败空 Error→Task.error 投影为 Reason；原始 ResultJSON.error 仍为空，reason/phase/summary/exitCode 保留 |
| `TestCompleteSuccessorAttemptClearsProjectionAndKeepsSubmittedError` | `internal/store/task_test.go` | 后继成功清旧投影；成功带非空 submitted Error 按现实现保留（不施加旧“成功必清 error”） |
| `TestCapabilitySameSpecExplicitNewVersionCreatesNewRow` | `internal/store/capability_test.go` | 新对象空 ID、显式 v2 同正文→新 UID/两行；FoldKey 相等、Digest 因含 version 不同 |
| `TestPlaybookInstallAllowsV4CapabilityNamesThatWereReservedInV3` | `internal/store/playbook_test.go` | 真原包安装 github/cron/http/manual 全部建出（allowlist，不是新黑名单） |
| `TestPlaybookInstallReservedSourceNameRejectedLikeDirectCreate` | `internal/store/playbook_test.go:699` | §4 保留原始产品红证据；本轮共享规则修复后定向/完整测试 PASS，原哨兵 errors.Is、row==nil、cap/source/edge/receipt 0 写均保留 |
| `TestNodeHidesTokenHash` | `internal/store/node_test.go` | Node JSON 不含 tokenHash/原值 |
| `TestSourceValidation`（零出口段） | `internal/store/source_test.go` | 空 targets 合法、0 入口边、事件→0 task、`no_target` 剪枝痕可落库 |

错误投影语义以 `/root/code/anotify/.pi-orchestrator/v4/RESULT-ERROR-PROJECTION.md` 为准（不改 C3 生产）；
注册失败回滚引用既有 `internal/auth/password_test.go:490 TestRegisterWithPassword_DuplicateUsername` /
`:529 TestRegisterWithPassword_ConcurrentDuplicate`（本 leaf 未重写认证）。

## 3. TRUE 白盒（`internal/store/v4_whitebox_test.go`，13 条，实际执行）

`TestWhiteboxParseMigrationsContinuousLedger` / `...MalformedHeaderIsNotABlock` /
`TestWhiteboxBaselineDDLFailureRollsBackMarkerAndLedger` / `...BaselineLedgerWriteFailureRollsBackAllDDL` /
`...BaselineHookFailureRollsBackMarkerAndData`（事务中途 marker=4、失败后归零）/ `...LaterMigrationFailurePreservesCheckpoint` /
`...MigrationClosedConnectionFailsWithoutSuccess` / `TestWhiteboxCancelledBeginTxPreservesBaselineCheckpoint`（只验 driver BeginTx + 全 ledger；`internal/store/migrate.go:799-801` 没有 context 参数）/
`...OpenWritesMarkerAndLedgerAtomically` / `...StrictMalformedLayoutRefusedWithoutRepair` /
`...UnsupportedLayoutRefusedWithoutRepair` / `...WriteTransactionCommitRollbackClosed` / `...NoStubRemains`（静态恢复 v3 名）。

迁移/布局用例调用**真实** `applyBaselineMigration/applyMigration/migrate/probeDatabaseFormat/validateV4Layout/userSchemaObjects`；取消用例仅调用真实驱动 BeginTx，通用写事务取消由 `internal/store/v4_whitebox_test.go:403 TestWhiteboxWriteTransactionCommitRollbackClosed` 覆盖，不伪称存在 withWriteTxn 或取消迁移 API。
没有生产测试钩子、没有 skip/noop。preOpen 拒绝（坏布局）与事务中回滚是两组不同用例。

## 4. 唯一产品红历史（本轮 ROOT Source-name 修复；原始证据保留）

`TestPlaybookInstallReservedSourceNameRejectedLikeDirectCreate`（`internal/store/playbook_test.go`）：
真原包的源文档 `metadata.name: route`（字面量，非模板键/非前缀变体），安装**成功**并落库源行。

- 证据（gate3 原样）：`receipt=&{... Created:{Capabilities:[demo-audit@1] Sources:[route]} ...}`、
  `sourceRow=&{... Name:route ... InstalledFrom:ins_...}`。
- 生产点 1：`internal/store/source.go:157` 直写 `CreateSource` 有保留名闸门（`ErrReservedEventSource`）。
- 生产点 2：`internal/store/playbook_tx.go:385` `createInstalledSourceOn` → `insertSourceOn`（`source.go:203`）
  **没有**同一校验；而 `playbook.md §2.2` 要求渲染产物走与直接创建完全相同的校验路径。
- 期望（测试断言）：安装报 `ErrReservedEventSource` 且整装 0 写（源/回执/能力/入口边）。

上列为前次 B tests-only 的真实红证据，不删除。ROOT 已批准末尾 Source-name 小产品修复；同名用例现定向/完整测试 PASS。仅按父批准纠正 GetSourceByName absence 为 ErrSourceNotFound 并加 row==nil，不改变生产/API，不减 0 写断言。

## 5. 三项机械门禁（实际命令/退出码）

```text
# gate1（format + scoped diff check）
CMD test -z "$(gofmt -l internal/store internal/auth/password_test.go)"; git diff --check -- 'internal/store/*_test.go' 'internal/store/v4test/*_test.go'
EXIT 0

# gate2
CMD env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/... ./internal/auth/... ./internal/push/...
LOG /tmp/anotify-v4-delivery-b-gate2-vet.log
EXIT 0

# gate3
CMD env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/... ./internal/auth/... ./internal/push/... -count=1
LOG /tmp/anotify-v4-delivery-b-gate3-test.log
EXIT 1   fails=1（仅 §4 产品红；store 其余/v4test/auth/push 全绿：ok store 8.798s / v4test 35.154s / auth 21.731s / push 0.007s）
```

## 6. 未决 / 不声明

- 原 gate3 的 Source-name 产品红现按 ROOT 批准修复，末尾固定五树三项门禁均实际 0；仍须父 fresh G1 审查，不能把本 leaf 的机械通过当作 G1 DONE。
- `legacy central failed-empty-error / success-error-clear`：失败无 Error/Reason/Summary 的字面 `failed` 分支由 `internal/store/task_test.go:1081 TestCompleteEmptyFailureProjectsPhaseAndKeepsRawResultFacts` 验证；保留现成功 submitted Error 语义，未做 normalization。
- 旧 `TestFinishTaskWritesOutcome` 的正/负结果原意保留：正结果 → `TestCompleteRecordsResultAndFinishedAt`；
  失败空 Error + 有 Reason 投影 → `TestCompleteErrorProjectionKeepsRawResultFacts`；全空失败 → `TestCompleteEmptyFailureProjectsPhaseAndKeepsRawResultFacts`；成功清旧投影/保留 submitted →
  `TestCompleteSuccessorAttemptClearsProjectionAndKeepsSubmittedError`；非法相位零写 →
  `TestCompleteRejectsInvalidResultPhaseWithZeroWrites`。
- G2 runtime / G3 HTTP5 / G4 tools+Node quick / 最终 `go test ./...`+contract+e2e 仍不在本 leaf。

## 7. 既有白盒/安全族的承接（不重复造测试）

- 池/读写配置：`internal/store/pool_test.go`（`TestPoolMaxOpenConnsConfig` / `TestPoolReadsRunInParallel` /
  `TestPoolWriteTxIsImmediateAndDoesNotBlockReaders` / `TestPoolConcurrentWritesNoLockErrors`）；
  写事务 commit/rollback/cancel/closed 的内部契约由本 leaf 的 `TestWhiteboxWriteTransactionCommitRollbackClosed` 补齐。
- IDs：`internal/store/ids_test.go`（`TestNewIDPrefixAndUniqueness` / `TestCapabilityAndGraphIDShape`）。
- stats/retention：`internal/store/stats_test.go`、`internal/store/retention_test.go:148 TestRetentionDeletesExpiredTerminalTasks`、
  `internal/store/v4test/retention_test.go:433 TestRetentionProtectsOldUnreapedAttemptAfterCurrentReaped`。
- 持久字段往返 / CAS / 引用：`internal/store/v4test/{resource_cas,result_facts,result_current_binding,capability_spec,config}_test.go`。
- 当前授权 / slot / 预算：`internal/store/node_test.go:829 TestNodeAuthorizationGrant`、
  `internal/store/v4test/task_test.go:1030 TestTaskGrantBudgetExhaustedReleasesSlot`、
  `:642 TestTaskOnlineLeaseExpiryKeepsSlotUntilNodeFact`、`:1239 TestTaskReapOfRequeuedAttemptReleasesOnlyItsSlot`。
- 注册失败回滚（认证安全，未重写）：`internal/auth/password_test.go:490 TestRegisterWithPassword_DuplicateUsername` /
  `:529 TestRegisterWithPassword_ConcurrentDuplicate`。


# Source-name small product rework · ROOT approved · mechanical closure

Authority: root SOURCE-NAME-CLOSURE.md + DELIVERY-B-ROUTE-REWORK.md + ROOT 第五路径 event.go 精确授权。
Canonical worktree: /root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery。
Base HEAD: b99db8af256a722958bdc088232579c8e8dda347；未 commit/push/merge/MR，未开子 agent。
这是 Source-name 实现与机械门禁交付，不是 G1/B/T1 总验收完成。

## 做了什么 / 精确文件清单

Production（5）：
- internal/sourcespec/name.go：唯一共享签名 func ValidateName(name string) error；只在判定时 TrimSpace，空/纯空白、字面 route 拒绝，不改原输入。
- internal/store/source.go：CreateSource 用共享规则，namespace/config/targets/原事务不改；不隐式 rename。
- internal/playbook/render.go：Source name 在 values 物化后用共享规则；不借用能力名 regex。源名失败通过双 %w 同时保留 playbook render_name_invalid 的 errors.As 投影与共享 sentinel 的 errors.Is。
- internal/store/playbook_tx.go：最终 installed source 写入前再校验同规则；不调用完整 CreateSource/checkSourceTargets，pending 引用仍允许，token 生成/原外事务不改。
- internal/store/event.go：仅加 sourcespec import、保留导出 ErrReservedEventSource 为同一共享指针；原 sentinel 文本逐字保留，无 init/cause bridge/string identity，无 event 行为改动。

Tests/helpers（7）：
- internal/sourcespec/name_test.go
- internal/playbook/render_test.go
- internal/playbook/plan_test.go
- internal/playbook/helpers_test.go
- internal/store/source_test.go
- internal/store/playbook_test.go
- internal/store/v4test/source_name_test.go（精确 optional owner）

Report/mapping（1）：本文件，仅 source-name 精确行与本轮实证。
父 metadata 变更另列：.pi-legion/contract.md、DELIVERY-PROGRESS.md、task-card；不是本 writer 产出。

## 新增具名测试与保护点

| 具名测试 | 文件 | 实测保护 |
|---|---|---|
| TestValidateNameExistingRuleOnly | internal/sourcespec/name_test.go | 空/空白/trim-route 拒；github/cron/http/manual、Route、route-a、中文/空格、@、64 字符不被新禁 |
| TestRenderSourceNameAfterMaterialization | internal/playbook/render_test.go:737 | 真 literal route + values route/trim-route/empty/whitespace；render_name_invalid 与共享 errors.Is；无 render 产出 |
| TestRenderSourceLegalNamesNoInputRename | internal/playbook/render_test.go:766 | 合法名按原值输出到 Source 与 rendered 文件；空 targets；原 files/values 不改 |
| TestPlanReservedSourceNameEmptyExistingCreateReuse | internal/playbook/plan_test.go:574 | literal/values route；EmptyExisting/create/reuse 全拒，nil Plan；共享 sentinel 不丢 |
| TestCreateSourceNameRuleNoInputRename | internal/store/source_test.go | direct Create 同规则/errors.Is；合法名字落库往返不改，空 targets 合法 |
| TestInstalledSourceNameFinalDefenseRollsBack | internal/store/playbook_test.go:801 | 绕过 Render 的真实写防御；先真写 capability，确认事务中 count=1，再错误源拒绝，rollback 后 cap/source/edge/receipt/event/task/deployment 全 0 |
| TestStoredSourceNameRenderDryRunInstallRejectCreateReuse | internal/store/v4test/source_name_test.go:70 | 真存储包/正常能力/错误源；stored render、dryRun、实际 install；create 与历史坏行 reuse；nil 输出、共享 sentinel+domain code；8 表每一列 persistent facts 原样不变，raw files/values 不改 |
| TestInstallSourceLegalKindsNamesEmptyTargetsAndPending | internal/store/v4test/source_name_test.go:130 | webhook/cron/manual × 7 合法源名；dryRun 0 写、真实 installedFrom、empty targets reuse；未解析 targets 真落库并报 pending |

原测试保留：TestPlaybookInstallReservedSourceNameRejectedLikeDirectCreate（:699）；只按父批准将 absence expectation 纠正为实际 ErrSourceNotFound 并加 row==nil，cap/source/edge/receipt 0 写不减。原 TestSourceValidation 的零出口/0 task/no_target 痕仍实测 PASS。原 12 格配置/targets conflicts、receipt/rawinputs 与 auth/push 不被绕过：完整五树测试包含它们。

## RED → GREEN / 失败尝试

- 首先新增纯 Render/Plan create/reuse 与 direct no-rename 用例，再跑定向（不是 unchanged 全量 baseline）。actual TARGETED_RED_EXIT=1：literal/values route 的 Render 无错误、Plan EmptyExisting/create/reuse 无错误；原 install 真建 cap/source/receipt；合法源名误用能力 regex、direct 输入被 trim 改写。
- 原样红输出：/tmp/anotify-v4-delivery-source-name-red.log（命令+完整原始输出留存；含原测试 config 原始字节，不在报告另复制凭证）。
- 生产修复第一轮后 /tmp/anotify-v4-delivery-source-name-green.log actual exit1：原测试成功抵达 absence 断言，却将 ErrSourceNotFound 错认 ErrNotFound；新增存储夹具替换 schema 时遗失 env 默认。后者恢复原 pbValuesSchema + name property；前者按父精准批准纠正并增强 nil row。未改 API/规则、未弱化断言，未为绿灯绕过目标。第二次定向全绿，源码无需第二轮生产修复。
- gate1 首次 /tmp/anotify-v4-delivery-source-name-gate1-format.log actual exit1：gofmt/diffcheck 已绿，ownership probe 看到父新写 .pi-legion/contract.md 与 DELIVERY-PROGRESS.md；归属统计将明确父 metadata 与 writer owners 分列后实际 gate1=0，不把两文档冒充 writer 产出。
- pi-lens/async registry 工具未加载；没有配置/registry hack。使用权威 Go vet/test 与 bounded timeout/full tee logs，所有 bash 都有 canonical cd + pwd-P/gitroot exact guard。

最终定向输出（原样）：

```text
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto timeout 180 go test ./internal/sourcespec ./internal/playbook ./internal/store ./internal/store/v4test -run TestValidateNameExistingRuleOnly\|TestRenderSourceNameAfterMaterialization\|TestRenderSourceLegalNamesNoInputRename\|TestPlanReservedSourceNameEmptyExistingCreateReuse\|TestCreateSourceNameRuleNoInputRename\|TestPlaybookInstallReservedSourceNameRejectedLikeDirectCreate\|TestInstalledSourceNameFinalDefenseRollsBack\|TestStoredSourceNameRenderDryRunInstallRejectCreateReuse\|TestInstallSourceLegalKindsNamesEmptyTargetsAndPending\|TestSourceValidation -count=1
ok  	github.com/zzjcool/anotify/internal/sourcespec	0.005s
ok  	github.com/zzjcool/anotify/internal/playbook	0.024s
ok  	github.com/zzjcool/anotify/internal/store	0.090s
ok  	github.com/zzjcool/anotify/internal/store/v4test	0.935s
TARGETED_GREEN_EXIT=0
```

## 固定 THREE full gates · actual exits [0, 0, 0]

原 B store/auth/push 完整范围保留，并补共享 sourcespec/playbook 两树；没有改为 scoped-green substitute。
这是源码全量完成后的唯一完整 test 执行，未重跑未改动 baseline。

Gate1 最终原样输出（/tmp/anotify-v4-delivery-source-name-gate1-format-final.log）：

```text
gofmt -l internal/sourcespec internal/playbook internal/store internal/auth/password_test.go; git diff --check; exact writer ownership/parent metadata partition
FORMAT_OK
SCOPE_OK 12 writer implementation/test/report files
PARENT_METADATA_NOT_WRITER_OWNED .pi-legion/contract.md, .pi-orchestrator/v4/teams/core-runtime/DELIVERY-PROGRESS.md, .pi-orchestrator/v4/teams/core-runtime/task-cards/DELIVERY-B-ROUTE-REWORK.md
FULL_GATE1_EXIT=0
```

Gate2 原样输出（/tmp/anotify-v4-delivery-source-name-gate2-vet.log）：

```text
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto timeout 600 go vet ./internal/store/... ./internal/auth/... ./internal/push/... ./internal/sourcespec/... ./internal/playbook/...
FULL_GATE2_EXIT=0
```

Gate3 原样输出（/tmp/anotify-v4-delivery-source-name-gate3-test.log）：

```text
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto timeout 600 go test ./internal/store/... ./internal/auth/... ./internal/push/... ./internal/sourcespec/... ./internal/playbook/... -count=1
ok  	github.com/zzjcool/anotify/internal/store	9.044s
ok  	github.com/zzjcool/anotify/internal/store/v4test	36.234s
ok  	github.com/zzjcool/anotify/internal/auth	21.499s
ok  	github.com/zzjcool/anotify/internal/push	0.007s
ok  	github.com/zzjcool/anotify/internal/sourcespec	0.005s
ok  	github.com/zzjcool/anotify/internal/playbook	0.111s
FULL_GATE3_EXIT=0
```

## 未决 / 明确不做

本授权 scope 无剩余失败项。未改 Node/protocol/artifact/C3 auth-budget-result/UI/settings/env，也未迁库/清数据/新 namespace/credential/kind/target 规则。未提交/推送/MR（本卡明确禁止，非漏做）。
父 fresh G1 correctness/coverage/simplicity、原 460 mapping 与真实白盒复核仍必需；G2 runtime、G3 HTTP5、G4 tools/Node quick 及最终 whole-Go/contract/quick 不在本 leaf，未声称执行或通过。

报告工具事故与恢复：整文件 read 遭 50KB 截断，append write 一度误删 mapping 尾段；交付前完整 diff 重读当场发现。已从本 HEAD 原文精确恢复并仅重放上述 5 处 source-name 行修改与本 report，原库存/历史验证输出全部保留。恢复后逐字比较非授权内容，未改任何 Go 源码，不额外重跑完整 Go baseline。

---

## G2 runtime 逐名迁移映射（branch `pi-subagent/core-g2-resume-52fc54d4`，code commit `e552bd3`）

> 本节只追加 G2 行；以上 G1 映射原样保留，不改写、不重排。
> 逐名口径：`git show e552bd3^:<旧文件>` 里每一个 `func Test…` 都有一行结论；
> 新测试名来自当前 `internal/{route,ws,lease,source,trigger,logging}`。
> 本表不把「整文件 deprecated」当理由：每个旧名要么落到具体新测试，要么带 D 依据退役。


### 退役/映射理由码（D 依据）

| 码 | 依据 |
|---|---|
| `R:PURE_V3_SCHED` | v3 调度概念（priority / cursor / least-loaded / redispatch / 启动随机化）在 v4 没有对应物：候选由 `EligibleNodesForTask`（在线 + ns 申报∩授权 + 配置 applied + deployments applied + commands + values）与真实空槽决定（capability.md §8.5）。 |
| `R:PURE_V3_PORT` | v3 多端口 / ports 整体删除（capability.md §1「一个能力一个结果」；emit 只有一个出口，wire 无 `port`）。 |
| `R:PURE_V3_PIN` | v3 NodePin / GraphVersion / LinkTarget / 边跳数 / 用户作用域图投影删除；v4 边只是 `spec.next` / `source.targets` 的派生表（§8.1），任务只钉目标 refs。 |
| `R:PURE_V3_LITERAL` | v3 literals / mapping 表达式删除（source.md D1/D10：只有 `$(payload.路径)` 模板，可拼接；没有 literals / 表达式 / 默认值语法）。 |
| `R:PURE_V3_KIND` | v3 trigger kind 分类（user / agent / builtin central）删除；v4 kind = source 注册表（cron / webhook / manual，sourcespec 由 G1 覆盖）。 |
| `R:PURE_V3_SEQ` | v3 emit seq / note / status 白名单删除（V4-D4：显式 `id` 是唯一业务幂等键；`requestId` 只做 RPC 关联）。 |
| `R:PURE_V3_ASSIGN` | v3 assign/publish library 帧与本地 argv 生成删除（v4 派发只给 refs/params；节点在 grant 后按脚本执行）。 |
| `R:PURE_V3_WS` | v3 WS 兼容缓存 / 协议版本 / 心跳 / 测试 harness 细节删除（v4 开发期只有一种 agent，协议无版本协商，`anode.md §3.3`）。 |
| `R:PURE_V3_TRACE` | v3 显式/确定性 trace id 删除（v4 trace 由接收动作生成；外部幂等键是 `deliveryId`，capability.md §8.4）。 |
| `R:PURE_V3_INGEST` | v3 trigger 包特有的事件入库/kind/未知源语义删除；v4 统一在 `internal/source`（三入口共用一份回执）。 |
| `R:PURE_V3_SWEEPER` | v3 sweeper 的 redispatch/nudge/可注入 options 删除；v4 调度循环在 `ws.DispatchOnce`，巡检只做 store 的真实回收/保留/统计。 |
| `R:V3_CENTRAL` | v4 明确不留中枢内置执行器（capability.md §7：只有脚本 + 节点执行），`internal/executor` 整体删除。其中旧 `notify.phone` 中央 Task 成功/失败/缺 to 等专属断言**刻意退役**（CONTRACT 已裁决，不恢复、不要求替代的中央任务通知能力）；独立 POST notify / device / broker / push / auth 域不退役，由现有测试与后 G3/G4 独立域回归守。 |
| `R:V3_NODE_BINARY` | v4 唯一 Node 产品是 `cmd/anode`（Node 队实现）；旧 `cmd/agent` 二进制整体删除，任何 CLI/apply/cache/emit 语义都不进 G2。 |
| `R:V4_STORE` | 语义保留，但权威覆盖在 G1 的 store authorized 事务 / artifact / 并发测试；G2 不另造假 store 断言。 |
| `R:V4_PROTOCOL` | 语义保留，但权威覆盖在 T3 protocol 帧/方向/严格解码/限额测试（共享 wire 只能有一份）。 |
| `R:V4_G3` | 属于 G3 HTTP 传输层的参数/状态码/授权（user-scoped dispatch、bearer 解析映射等）；G2 只提供 typed error 供 G3 翻译。 |
| `R:V4_POLICY` | v4 有意改变策略：剪枝留痕失败必须报错（不静默继续派发）；webhook 不使用 `config.payload`（source.md §5 载荷来自投递体）。 |
| `R:V4_GAP` | 明确未实现并记录为 gap（见文末「G2 明确 pending」）。 |
| `R:SELF` | 文件未改、测试原样保留（`internal/logging` 独立 logger）。 |


### `internal/route`（141 个旧名）

| 旧测试 | 结论 |
|---|---|
| `TestDispatchPriorityPrefersLower` | `R:PURE_V3_SCHED` |
| `TestDispatchFallsBackWhenHigherPriorityUnreachable` | `R:PURE_V3_SCHED` |
| `TestDispatchAllCandidatesUnreachable` | `R:PURE_V3_SCHED` |
| `TestDispatchStartupRandomization` | `R:PURE_V3_SCHED` |
| `TestEmitTwiceTwoIndependentTasks` | `TestEmitWithoutIDAlwaysCreatesNew` |
| `TestDispatchCursorKeyedByPort` | `R:PURE_V3_PORT` |
| `TestPlanPinsCurrentArtifactDigest` | `TestPlanTasksArePendingWithoutAuthority` |
| `TestDispatchKeepsDigestThroughCreate` | `TestEmitCreatesPinnedChildTask` |
| `TestDispatchNoArtifactKeepsDigestEmpty` | `R:V4_STORE` |
| `TestDispatchEntryEdgeResolvesPayloadPath` | `TestPlanFromSourceMapsTemplatesAndKeepsOrder` |
| `TestDispatchEntryEdgeFanoutPartialPrune` | `TestIngestParamPruneIsPerTarget` |
| `TestDispatchEntryEdgeRequiresEntryFields` | `TestPlanFromSourceParamMissing` |
| `TestDispatchEntryEdgeBrokenMappingFailClosed` | `TestPlanFromSourceParamMissing` |
| `TestDispatchEntryEdgeEmptyMappingIsIdentity` | `TestPrepareParamsPermissiveSchema` |
| `TestDispatchEntryEdgeEmptyMappingStillAppliesLiterals` | `R:PURE_V3_LITERAL` |
| `TestDispatchLiteralsOverride` | `R:PURE_V3_LITERAL` |
| `TestDispatchLiteralsFanoutIsolation` | `R:PURE_V3_LITERAL` |
| `TestDispatchNoLiteralsUnchanged` | `TestPrepareParamsPermissiveSchema` |
| `TestDispatchLiteralTypeCheckedAtDispatch` | `R:PURE_V3_LITERAL` |
| `TestDispatchBrokenLiteralsFailClosed` | `R:PURE_V3_LITERAL` |
| `TestDispatchEntryEdgeLiteralsOverride` | `R:PURE_V3_LITERAL` |
| `TestDispatchResolvesLinkRef` | `TestPlanFromSourceMissingCapability` |
| `TestDispatchPrunesUnresolvableLinkRef` | `TestPlanFromSourceMissingCapability` |
| `TestDispatchSelectsOneCandidate` | `R:PURE_V3_SCHED` |
| `TestDispatchPartialPrune` | `TestIngestParamPruneIsPerTarget` |
| `TestDispatchNoEdge` | `TestPlanFromSourceNoTargets` |
| `TestDispatchNoReportIsNotPrune` | `R:PURE_V3_PIN` |
| `TestDispatchValidation` | `R:V4_STORE` |
| `TestDispatchRejectsEmptyNamespace` | `R:V4_STORE` |
| `TestDispatchIsolatesNamespaces` | `R:V4_STORE` |
| `TestDispatchEntryEdgeStaysInNamespace` | `R:V4_STORE` |
| `TestDispatchNoNode` | `R:PURE_V3_SCHED` |
| `TestDispatchLeavesPendingWhenNoCapacity` | `R:V4_STORE` |
| `TestDispatchLeaseAndAssignFailureDoesNotLeakSlot` | `R:V4_STORE` |
| `TestDispatchPicksLeastLoadedNode` | `R:PURE_V3_SCHED` |
| `TestDispatchPathIncludedInChildTask` | `R:PURE_V3_PORT` |
| `TestDispatchAckLeaseWindow` | `R:V4_STORE` |
| `TestDispatchStoreErrorsPropagate` | `R:V4_STORE` |
| `TestDispatchStickyPin` | `R:PURE_V3_PIN` |
| `TestDispatchStickyHitDoesNotRewritePin` | `R:PURE_V3_PIN` |
| `TestDispatchStickyFallsBackWhenPinnedNodeGone` | `R:PURE_V3_PIN` |
| `TestValidateParamsSubset` | `TestPrepareParamsDefaultsRequiredAndExtra` |
| `TestValidateErrorIsDeterministic` | `TestPrepareParamsMissingRequired` |
| `TestValidateNestedPathError` | `TestPrepareParamsTypeMismatch` |
| `TestDispatchResolvesParamTemplate` | `TestPlanFromSourceMapsTemplatesAndKeepsOrder` |
| `TestDispatchParamTemplateMissing` | `TestPlanFromSourceParamMissing` |
| `TestDispatchParamTemplateTypeError` | `TestPrepareParamsTypeMismatch` |
| `TestDispatchParamTemplateMixed` | `TestPlanFromSourceMapsTemplatesAndKeepsOrder` |
| `TestDispatchRecursiveDoesNotResolve` | `TestPlanFromSourceTemplatesSinglePassAndKeepTypes` |
| `TestDispatchParamMissingDetailSources` | `TestPlanFromSourceParamMissing` |
| `TestParamPruneDetailExact` | `TestPlanFromSourceParamMissing` |
| `TestDispatchDryRunPreviewUsesResolvedParams` | `TestPreviewWritesNothing` |
| `TestParamDetailFormattersNilCap` | `TestPrepareParamsBadSchema` |
| `TestDispatchUserScopedLookupErrorFailsClosed` | `R:V4_G3` |
| `TestDispatchEntryEdgesFanOut` | `R:PURE_V3_PORT` |
| `TestRedispatchTaskNoCapacityReturnsSentinel` | `R:PURE_V3_SCHED` |
| `TestRedispatchTaskNoNodeStillReturnsNoError` | `R:PURE_V3_SCHED` |
| `TestDispatchStickyPublishFailureRollsBackPin` | `R:PURE_V3_PIN` |
| `TestDispatchStickyPublishFailureKeepsForeignPin` | `R:PURE_V3_PIN` |
| `TestDispatchToNarrowsBeforeValidation` | `R:PURE_V3_PORT` |
| `TestDispatchToNamedTargetStillValidated` | `R:PURE_V3_PORT` |
| `TestDispatchToNotCandidate` | `R:PURE_V3_PORT` |
| `TestEmitRejectsForeignNode` | `TestEmitRoundTripAndErrors` |
| `TestEmitPersistsNote` | `R:PURE_V3_SEQ` |
| `TestEmitPortsAreIndependentAndShaped` | `R:PURE_V3_PORT` |
| `TestEmitPortShapeRejectsWrongPayload` | `R:PURE_V3_PORT` |
| `TestEmitIdempotent` | `TestEmitSameIDReplaysAndConflicts` |
| `TestEmitSeqGapRejected` | `R:PURE_V3_SEQ` |
| `TestEmitSeqReusedRejected` | `R:PURE_V3_SEQ` |
| `TestEmitRejectedOnClosedTask` | `TestEmitOnFinishedTaskIsNotRunning` |
| `TestEmitOnRunningTasks` | `TestEmitCreatesPinnedChildTask` |
| `TestEmitStatusWhitelist` | `R:PURE_V3_SEQ` |
| `TestEmitUnknownTask` | `TestEmitUnknownTaskIsTokenInvalid` |
| `TestEmitNoLinksIsNotAnError` | `TestPlanFromEmitSameNameAndNotDeclared` |
| `TestEmitCandidatePartialPrune` | `TestEmitPruneReceiptAndTrace` |
| `TestEmitEmissionAndTasksSameTransaction` | `TestEmitSameIDReplaysAndConflicts` |
| `TestEmitInputValidation` | `TestEmitMissingTargetRejected` |
| `TestEmitDataMustBeObject` | `TestPlanFromEmitSameNameAndNotDeclared` |
| `TestEmitChildTasksInheritParent` | `TestEmitCreatesPinnedChildTask` |
| `TestEmitLiteralsWin` | `R:PURE_V3_LITERAL` |
| `TestEmitParamPrunePerLink` | `TestEmitPruneReceiptAndTrace` |
| `TestEmitConcurrentSameSeq` | `R:PURE_V3_SEQ` |
| `TestBuildExpectsTargetsCarryPriority` | `R:PURE_V3_SCHED` |
| `TestBuildExpectsUnionWithLiterals` | `R:PURE_V3_LITERAL` |
| `TestBuildExpectsNoLinksAbsent` | `R:PURE_V3_PIN` |
| `TestBuildExpectsEmptyNeedsSchema` | `R:PURE_V3_PIN` |
| `TestBuildExpectsEmptyPortList` | `R:PURE_V3_PORT` |
| `TestBuildExpectsNamespaceRequired` | `R:V4_STORE` |
| `TestBuildExpectsDedupesRequired` | `R:V4_STORE` |
| `TestDeriveKind` | `R:PURE_V3_KIND` |
| `TestKindForCapabilityFollowsEdges` | `R:PURE_V3_KIND` |
| `TestKindForCapabilityBuiltinOverrideWins` | `R:PURE_V3_KIND` |
| `TestNodeKindMixedCapabilities` | `R:PURE_V3_KIND` |
| `TestKindValues` | `R:PURE_V3_KIND` |
| `TestDeriveKindIsPure` | `R:PURE_V3_KIND` |
| `TestKindForCapabilityDoesNotMutateShape` | `R:PURE_V3_KIND` |
| `TestDispatchNoopPrunedNoImpl` | `R:V3_CENTRAL` |
| `TestDispatchExecutableHandlerWithoutNodeIsCentral` | `R:V3_CENTRAL` |
| `TestRenderTemplate` | `TestPlanFromSourceMapsTemplatesAndKeepsOrder` |
| `TestRenderTemplateSinglePass` | `TestPlanFromSourceMapsTemplatesAndKeepsOrder` |
| `TestResolveParamsNoRecursion` | `TestPlanFromSourceTemplatesSinglePassAndKeepTypes` |
| `TestResolveParamsWholeRefKeepsType` | `TestPlanFromSourceTemplatesSinglePassAndKeepTypes` |
| `TestGetPath` | `TestPlanFromSourceMapsTemplatesAndKeepsOrder` |
| `TestResolveParamsMissingRefs` | `TestPlanFromSourceParamMissing` |
| `TestValidateParamTemplatesPlaceholderForms` | `R:PURE_V3_LITERAL` |
| `TestValidateParamTemplatesMappings` | `R:PURE_V3_LITERAL` |
| `TestValidateParamTemplatesMappingOrLiteral` | `R:PURE_V3_LITERAL` |
| `TestValidateLiteralParams` | `R:PURE_V3_LITERAL` |
| `TestIsMapping` | `R:PURE_V3_LITERAL` |
| `TestIsVarRef` | `R:PURE_V3_PIN` |
| `TestCoerceValue` | `R:PURE_V3_LITERAL` |
| `TestSchemaPropType` | `R:PURE_V3_LITERAL` |
| `TestResolveVars` | `R:PURE_V3_PIN` |
| `TestResolveParamsMappings` | `TestPlanFromSourceMapsTemplatesAndKeepsOrder` |
| `TestResolveParamsTypeError` | `TestPrepareParamsTypeMismatch` |
| `TestResolveParamsEmptyTemplate` | `TestPrepareParamsPermissiveSchema` |
| `TestResolveParamsLiteralClone` | `R:PURE_V3_LITERAL` |
| `TestValidateParamTemplatesLiteralBraces` | `R:PURE_V3_LITERAL` |
| `TestValidateParamTemplatesFormTiers` | `R:PURE_V3_LITERAL` |
| `TestValidateParamTemplatesDollarRefRejected` | `R:PURE_V3_LITERAL` |
| `TestResolveParamsMappingNoRecursion` | `TestPlanFromSourceMapsTemplatesAndKeepsOrder` |
| `TestResolveParamsVarNilIsMiss` | `R:PURE_V3_PIN` |
| `TestResolveParamsExplicitNullIsMiss` | `TestPrepareParamsMissingRequired` |
| `TestParamPruneReasons` | `R:V4_PROTOCOL` |
| `TestValidateEventPayload` | `TestValidatePayloadSchema` |
| `TestValidateEventPayloadMatchesParamsSemantics` | `TestValidatePayloadSchema` |
| `TestMergePortsUnion` | `R:PURE_V3_PORT` |
| `TestMergePortsDoesNotMutateInputs` | `R:PURE_V3_PORT` |
| `TestEffectivePortsScenario` | `R:PURE_V3_PORT` |
| `TestDispatchRecordsEveryPruneReason` | `TestEmitPruneReceiptAndTrace` |
| `TestDispatchPruneNotDuplicated` | `TestEmitPruneReceiptAndTrace` |
| `TestDispatchDryRunWritesNoPruneEvent` | `TestPreviewWritesNothing` |
| `TestDispatchNoOutWritesNoPruneEvent` | `R:PURE_V3_PORT` |
| `TestDispatchPruneRecordFailureDoesNotBreakDispatch` | `R:V4_POLICY`（v4：剪枝留痕与 emit 同事务，留痕失败必须报错不假成功；对应 `TestEmitPruneFailureIsNotSwallowed`/store S1 事务回滚用例） |
| `TestPruneEventTypeMapping` | `R:V4_PROTOCOL` |
| `TestPruneReasons` | `R:V4_PROTOCOL` |
| `TestPruneMaxHopsCycle` | `R:PURE_V3_PIN` |
| `TestForeignUser` | `R:V4_G3` |
| `TestForeignUserEmptyTo` | `R:V4_G3` |
| `TestNonUserScopedCapabilitySkipsMemberCheck` | `R:V4_G3` |
| `TestPruneReasonValues` | `R:V4_PROTOCOL` |

### `internal/ws`（110 个旧名）

| 旧测试 | 结论 |
|---|---|
| `TestApplyFrameLandsInStore` | `TestDeployNotifiesOnlineAndReplaysOnReconnect` |
| `TestApplyFrameFailureIsVisible` | `R:V4_STORE` |
| `TestApplyFrameRejectsBadInput` | `R:V4_PROTOCOL` |
| `TestApplyFrameUnknownDeploymentIgnored` | `TestDeployedReceiptAuthorizedNegatives` |
| `TestApplyFrameJSONShape` | `R:V4_PROTOCOL` |
| `TestAssignFrameForCarriesDigest` | `R:PURE_V3_ASSIGN` |
| `TestAssignFrameForOmitsEmptyDigest` | `R:PURE_V3_ASSIGN` |
| `TestAssignPortsFollowEdgesWhenOutputsEmpty` | `R:PURE_V3_PORT` |
| `TestAssignPortsStayEmptyWithoutOutEdges` | `R:PURE_V3_PORT` |
| `TestAssignPortsKeepDeclaredWhenUnwired` | `R:PURE_V3_PORT` |
| `TestAssignPortsKeepLegacyMultiPorts` | `R:PURE_V3_PORT` |
| `TestAssignFrameForIsPure` | `R:PURE_V3_ASSIGN` |
| `TestPublishAssignCarriesLibraryAttemptsAndSeq` | `R:PURE_V3_ASSIGN` |
| `TestPublishAssignUsesLibraryValuesNotInMemory` | `R:PURE_V3_ASSIGN` |
| `TestPublishAssignOmitsZeroSeq` | `R:PURE_V3_ASSIGN` |
| `TestPublishAssignUsesTokenWrittenAtAssign` | `R:PURE_V3_ASSIGN` |
| `TestDecodeUpstreamApplyFrame` | `R:V4_PROTOCOL` |
| `TestDecodeUpstreamApplyMinimal` | `R:V4_PROTOCOL` |
| `TestDeployFrameRoundTrip` | `R:V4_PROTOCOL` |
| `TestFrameDirectionTable` | `R:V4_PROTOCOL` |
| `TestAssignFrameCarriesDigestAndTraceID` | `R:PURE_V3_ASSIGN` |
| `TestPublishDeployOfflineNode` | `TestDeployNotifiesOnlineAndReplaysOnReconnect` |
| `TestPublishDeployCrossNamespaceRejected` | `TestDeployRejectsUnauthorizedExplicitTargetAtomically` |
| `TestPublishDeployDeliversFrame` | `TestDeployNotifiesOnlineAndReplaysOnReconnect` |
| `TestEmitFrameRoutesAndAcks` | `TestEmitRoundTripAndErrors` |
| `TestEmitFrameRejectedKeepsConnectionOpen` | `TestEmitRoundTripAndErrors` |
| `TestEmitFrameNotWired` | `TestEmitRoundTripAndErrors` |
| `TestExpectsFrameShapeMatches` | `R:V4_PROTOCOL` |
| `TestEmitFrameCarriesNoteAndSeq` | `R:PURE_V3_SEQ` |
| `TestEmitAckFrameShape` | `R:V4_PROTOCOL` |
| `TestAckedNonIdempotentCleanEndAbandons` | `TestRunOnceRecoversExpiredNonIdempotentLease` |
| `TestKilledAgentAbandonsInflightTask` | `TestRunOnceRecoversExpiredNonIdempotentLease` |
| `TestHelloReassertKeepsNodeOnline` | `TestConfigAppliedAndAgentFactsPersist` |
| `TestPublishAssignFailsWhenGenerationUnreadable` | `R:PURE_V3_ASSIGN` |
| `TestPublishAssignSkipsTerminalTask` | `R:V4_STORE` |
| `TestReleaseIgnoresForeignOwner` | `R:PURE_V3_ASSIGN` |
| `TestClassifyReadError` | `R:PURE_V3_WS` |
| `TestHelloReclaimsStaleSlots` | `TestRehelloUnknownBusyDoesNotRelease` |
| `TestInflightReleasedAfterResult` | `TestCancelAckMatrix` |
| `TestCleanEndAllowsRedispatch` | `TestRunOnceRecoversExpiredNonIdempotentLease` |
| `TestHeartbeatLeaseFailureClosesConnection` | `TestRenewAtExpiryIsRejected` |
| `TestCleanupInsideWindowKeepsNewConnectionOnline` | `R:V4_STORE` |
| `TestSupersededCleanupKeepsNewConnectionOnline` | `TestDisconnectKeepsBusyUntilExplicitFact` |
| `TestPublishAssignUsesAuthoritativeAttempts` | `R:V4_STORE` |
| `TestEmitOversizedParamsIsDiagnosable` | `R:V4_STORE` |
| `TestHeartbeatRenewsOnlyOwnedTasks` | `TestRenewAtExpiryIsRejected` |
| `TestUncleanEndAbandonsStartedTask` | `TestRunOnceRecoversExpiredNonIdempotentLease` |
| `TestCleanEndRequeuesStartedTask` | `TestRunOnceRequeuesExpiredIdempotentLease` |
| `TestAssignedNotStartedRequeuedOnUncleanEnd` | `TestRunOnceRequeuesExpiredIdempotentLease` |
| `TestSameTaskNotExecutedByTwoConnections` | `R:V4_STORE` |
| `TestLogsFrameWire` | `R:V4_PROTOCOL` |
| `TestDecodeUpstreamLogsResponse` | `R:V4_PROTOCOL` |
| `TestFetchLogsDeliversResponse` | `TestLogsRequestCorrelatesHistoryAndEmptyPrevious` |
| `TestFetchLogsNoLocalArchive` | `TestLogsWaiterBoundedCorrelationAndWake` |
| `TestFetchLogsOffline` | `TestLogsRequestCorrelatesHistoryAndEmptyPrevious` |
| `TestFetchLogsCrossNamespace` | `TestLogsRequestCorrelatesHistoryAndEmptyPrevious` |
| `TestFetchLogsTimeout` | `TestLogsWaiterBoundedCorrelationAndWake` |
| `TestFetchLogsDropsUnpairedResponse` | `TestLogsWaiterBoundedCorrelationAndWake` |
| `TestFetchLogsWakesOnDisconnect` | `TestLogsWaiterBoundedCorrelationAndWake` |
| `TestLogsRequestIDUniqueAndSafe` | `TestLogsRequestCorrelatesHistoryAndEmptyPrevious` |
| `TestCrossNamespaceTaskNotVisible` | `TestLogsRequestCorrelatesHistoryAndEmptyPrevious` |
| `TestPublishAssignRoutesByNamespace` | `R:V4_STORE` |
| `TestResultFreesSlotNudgesDispatcher` | `R:V4_STORE` |
| `TestRejectedResultDoesNotNudge` | `R:V4_STORE` |
| `TestHelloNudgesDispatcher` | `R:PURE_V3_SCHED` |
| `TestBigHelloFrameAccepted` | `R:V4_PROTOCOL` |
| `TestFrameTooLargeIsDiagnosable` | `R:V4_PROTOCOL` |
| `TestPublishRejectsOversizedParams` | `R:V4_STORE` |
| `TestResultLegacyOutCreatesNoDownstream` | `TestAcceptGrantsSameSlotAndResultAcks` |
| `TestResultRecordsTerminalStateWithoutRouting` | `TestAcceptGrantsSameSlotAndResultAcks` |
| `TestEmitBeforeFailedResultStillRoutes` | `TestEmitRoundTripAndErrors` |
| `TestUnauthorizedNoUpgrade` | `TestHandshakeRejectsBadTokenAndName` |
| `TestHandshake` | `TestDispatchFrameCarriesNoExecutionAuthority` |
| `TestHelloStaleCacheIsPruned` | `R:PURE_V3_WS` |
| `TestHelloWrongNodeID` | `TestHandshakeRejectsBadTokenAndName` |
| `TestHelloInstances` | `TestRehelloUnknownBusyDoesNotRelease` |
| `TestHelloAppliedPersists` | `TestConfigAppliedAndAgentFactsPersist` |
| `TestHelloAppliedIsFullReplacement` | `R:PURE_V3_WS` |
| `TestHelloAppliedUnknownListsAll` | `R:PURE_V3_WS` |
| `TestHelloAppliedDigestMismatchResendsDesiredArtifact` | `R:PURE_V3_WS` |
| `TestHelloProtocolVersion1Rejected` | `R:PURE_V3_WS` |
| `TestFramesBeforeHelloRejected` | `TestHandshakeRejectsPreHelloFrames` |
| `TestBearerTokenParsing` | `R:PURE_V3_WS` |
| `TestAuthErrorStatusMapping` | `R:V4_G3` |
| `TestFrameCodec` | `R:V4_PROTOCOL` |
| `TestResultFrameHasNoOut` | `R:V4_PROTOCOL` |
| `TestAssignFrameHasNoArgv` | `R:V4_PROTOCOL` |
| `TestNoArgvInNodeProtocolSources` | `R:V4_PROTOCOL` |
| `TestHeartbeatTimeout` | `R:PURE_V3_WS` |
| `TestHeartbeatKeepsAlive` | `R:PURE_V3_WS` |
| `TestLeaseExtended` | `TestAcceptGrantsSameSlotAndResultAcks` |
| `TestDownstreamNoTouch` | `R:PURE_V3_ASSIGN` |
| `TestAckProgressResultFlow` | `TestAcceptGrantsSameSlotAndResultAcks` |
| `TestResultNoOutMeansStop` | `TestAcceptGrantsSameSlotAndResultAcks` |
| `TestAckOtherNodesTaskRejected` | `R:V4_STORE` |
| `TestUnknownTypeKeepsConnection` | `TestUnknownFrameKeepsConnectionAndBadFrameCloses` |
| `TestBadJSONKeepsConnection` | `TestUnknownFrameKeepsConnectionAndBadFrameCloses` |
| `TestDisconnectMarksNodeOffline` | `TestDisconnectKeepsBusyUntilExplicitFact` |
| `TestDecodeUpstreamUpgradeAck` | `R:V4_PROTOCOL` |
| `TestUpgradeFrameWire` | `TestUpgradeFrameAndAck` |
| `TestNormalizeUpgradeError` | `R:V4_PROTOCOL` |
| `TestPublishUpgradeOfflineNode` | `TestUpgradeFrameAndAck` |
| `TestPublishUpgradeDeliversFrame` | `TestUpgradeFrameAndAck` |
| `TestPublishUpgradeCrossNamespaceRejected` | `R:V4_G3` |
| `TestHelloAgentVersionPersisted` | `TestConfigAppliedAndAgentFactsPersist` |
| `TestHelloWithoutAgentVersion` | `TestConfigAppliedAndAgentFactsPersist` |
| `TestUpgradeAckSuccessPersists` | `TestUpgradeFrameAndAck` |
| `TestUpgradeAckFailurePersistsAndLogs` | `TestUpgradeFrameAndAck` |
| `TestUpgradeAckUnknownErrorCodeNormalized` | `TestUpgradeFrameAndAck` |
| `TestMain` | `R:PURE_V3_WS` |

### `internal/lease`（30 个旧名）

| 旧测试 | 结论 |
|---|---|
| `TestRetentionNilIsNoop` | `R:PURE_V3_SWEEPER` |
| `TestRetentionFirstTickThenHourlyGate` | `TestRunOnceRetentionHonorsTTLAndKeepsActive` |
| `TestRetentionFailureDoesNotStallTick` | `R:PURE_V3_SWEEPER` |
| `TestNewAutoWiresRetention` | `R:PURE_V3_SWEEPER` |
| `TestNewWiresRealStoreRetention` | `TestRunOnceRetentionHonorsTTLAndKeepsActive` |
| `TestRetentionOverrides` | `R:PURE_V3_SWEEPER` |
| `TestRetentionDoesNotBlockTick` | `R:PURE_V3_SWEEPER` |
| `TestRetentionOverlapGuard` | `R:PURE_V3_SWEEPER` |
| `TestRetentionEventSecPlumbing` | `TestRunOnceRetentionHonorsTTLAndKeepsActive` |
| `TestRetentionCatchUpWhenTruncated` | `R:PURE_V3_SWEEPER` |
| `TestEventRetentionLogState` | `TestRunOnceRetentionHonorsTTLAndKeepsActive` |
| `TestRetentionSweepsPerNamespace` | `TestRunOnceRetentionHonorsTTLAndKeepsActive` |
| `TestRetentionAuditFieldsOnFailureAndCancel` | `TestRunOnceRecoversExpiredNonIdempotentLease` |
| `TestRetentionAuditFieldsOnSuccess` | `TestRunOnceRetentionHonorsTTLAndKeepsActive` |
| `TestSweeperStatsRunsOnFirstTick` | `TestOccupancyByNodeReadsRealLedger` |
| `TestSweeperStatsRespectsInterval` | `R:PURE_V3_SWEEPER` |
| `TestSweeperStatsDisabled` | `R:PURE_V3_SWEEPER` |
| `TestSweeperStatsNoStore` | `R:PURE_V3_SWEEPER` |
| `TestSweeperTickSweepsAndRedispatches` | `TestRunOnceRecoversExpiredNonIdempotentLease` |
| `TestSweeperContinuesWhenSweepFails` | `R:PURE_V3_SWEEPER` |
| `TestSweeperSkipsWhenNoNode` | `TestOccupancyByNodeReadsRealLedger` |
| `TestSweeperListErrorDoesNotPanic` | `R:PURE_V3_SWEEPER` |
| `TestSweeperRespectsBatchLimit` | `R:PURE_V3_SWEEPER` |
| `TestSweeperRedispatchNilIsNoop` | `R:PURE_V3_SWEEPER` |
| `TestSweeperRunStopsOnContextCancel` | `R:PURE_V3_SWEEPER` |
| `TestRetryBackoffSequence` | `TestRunOnceRequeuesExpiredIdempotentLease` |
| `TestSweeperNudgeRedispatchesImmediately` | `R:PURE_V3_SWEEPER` |
| `TestSweeperNudgeCoalesces` | `R:PURE_V3_SWEEPER` |
| `TestSweeperNudgeRateLimited` | `R:PURE_V3_SWEEPER` |
| `TestSweeperNudgeWithoutRedispatch` | `R:PURE_V3_SWEEPER` |

### `internal/source`（18 个旧名）

| 旧测试 | 结论 |
|---|---|
| `TestFireUsesEntryEdge` | `TestIngestManualUnifiedReceiptAndTask` |
| `TestFireDisabledSource` | `TestFireDisabledSourceRejected` |
| `TestFireIdempotent` | `TestIngestDuplicateDeliveryReturnsOriginal` |
| `TestSchedulerTickFiresDueCron` | `TestRunDueFiresCronAndAdvances` |
| `TestSchedulerSkipsNotDue` | `TestRunDueFiresCronAndAdvances` |
| `TestFirePassesPayloadAndVars` | `TestIngestManualPayloadOverridesConfigOneLevel` |
| `TestFireConfigPayloadIsBaseConfigPayload` | `TestIngestManualUnifiedReceiptAndTask` |
| `TestSchedulerTickCronPayloadIncludesConfig` | `TestIngestManualUnifiedReceiptAndTask` |
| `TestSchedulerTickCronPayloadSystemFields` | `R:PURE_V3_INGEST` |
| `TestSchedulerTickCronPayloadUserFieldsWin` | `TestIngestManualPayloadOverridesConfigOneLevel` |
| `TestFireWebhookIgnoresConfigPayload` | `R:V4_POLICY` |
| `TestEventPayloadKindScope` | `R:PURE_V3_KIND` |
| `TestDryRunPassesPayloadAndVars` | `TestPreviewWritesNothing` |
| `TestFireDoesNotFlattenParams` | `TestIngestManualUnifiedReceiptAndTask` |
| `TestFireDispatchesWhenPayloadSatisfiesResultSchema` | `TestIngestManualUnifiedReceiptAndTask` |
| `TestFirePrunesEventSchemaViolation` | `TestIngestPayloadSchemaPrune` |
| `TestFireWithoutResultSchemaUnchanged` | `TestPrepareParamsPermissiveSchema` |
| `TestDryRunAppliesResultSchema` | `TestPreviewWritesNothing` |

### `internal/trigger`（18 个旧名）

| 旧测试 | 结论 |
|---|---|
| `TestEventIdempotent` | `TestIngestDuplicateDeliveryReturnsOriginal` |
| `TestNoIdemKeyAllowsDuplicates` | `TestIngestDeliveryIDScopeAndAbsence` |
| `TestEmptyIdemKeyTreatedAsAbsent` | `TestIngestDeliveryIDScopeAndAbsence` |
| `TestIdemKeyScopedToSource` | `TestIngestDeliveryIDScopeAndAbsence` |
| `TestIdemKeyScopedToDefaultNamespace` | `R:V4_STORE` |
| `TestIdemKeyScopedToNamespace` | `R:V4_STORE` |
| `TestTraceIDFromTrigger` | `TestIngestManualUnifiedReceiptAndTask` |
| `TestTraceIDUniqueness` | `TestIngestManualUnifiedReceiptAndTask` |
| `TestExplicitTraceIDHonored` | `R:PURE_V3_TRACE` |
| `TestCronDeterministicID` | `R:PURE_V3_TRACE` |
| `TestCronIdempotentAcrossFire` | `R:PURE_V3_TRACE` |
| `TestEntryEdgeUsesSourceName` | `TestIngestManualUnifiedReceiptAndTask` |
| `TestUnknownSourceNoEntryEdge` | `R:PURE_V3_INGEST` |
| `TestDispatchFailureKeepsEvent` | `R:V4_GAP` |
| `TestPayloadPassThrough` | `TestIngestManualUnifiedReceiptAndTask` |
| `TestInvalidPayloadDefaultsToEmpty` | `TestIngestRejectsNonObjectPayload` |
| `TestIngestValidation` | `TestIngestRejectsNonObjectPayload` |
| `TestConcurrentIngestSameIdemKey` | `R:V4_STORE` |

### `internal/executor`（9 个旧名）

| 旧测试 | 结论 |
|---|---|
| `TestTickNotifyPhoneSucceeds` | `R:V3_CENTRAL` |
| `TestTickMarksFailedOnSendError` | `R:V3_CENTRAL` |
| `TestTickSurvivesClaimError` | `R:V3_CENTRAL` |
| `TestTickZeroDevicesIsSuccess` | `R:V3_CENTRAL` |
| `TestTickMissingToFails` | `R:V3_CENTRAL` |
| `TestTickUnknownCentralCapability` | `R:V3_CENTRAL` |
| `TestFinishUsesTaskNamespacePerTask` | `R:V3_CENTRAL` |
| `TestBatchLimitRespected` | `R:V3_CENTRAL` |
| `TestRunStopsOnContextCancel` | `R:V3_CENTRAL` |

### `internal/logging`（10 个旧名）

| 旧测试 | 结论 |
|---|---|
| `TestParseLevel` | 保留原测试（文件未改，独立 logger） |
| `TestParseFormat` | 保留原测试（文件未改，独立 logger） |
| `TestIsValidLevel` | 保留原测试（文件未改，独立 logger） |
| `TestIsValidFormat` | 保留原测试（文件未改，独立 logger） |
| `TestNewRequestID` | 保留原测试（文件未改，独立 logger） |
| `TestRequestIDContext` | 保留原测试（文件未改，独立 logger） |
| `TestUserIDContext` | 保留原测试（文件未改，独立 logger） |
| `TestFromContext` | 保留原测试（文件未改，独立 logger） |
| `TestFromContext_Empty` | 保留原测试（文件未改，独立 logger） |
| `TestInit_JSONOutput` | 保留原测试（文件未改，独立 logger） |

### `cmd/agent`（整个旧 Node 产品，314 个旧名）


结论：**整包删除**（`R:V3_NODE_BINARY`）。v4 唯一 Node 产品是 `cmd/anode`（由 Node 队交付），
旧 `cmd/agent` 的 CLI/apply/cache/emit/runner/runstore/logs_serve 语义没有任何一条进入 G2
运行期；不保留兼容二进制、不迁移旧测试、不新建同名包。以下逐名列出（同一结论）：

<details><summary>314 个旧测试名</summary>


  `TestHandleDeployApply` · `TestHandleDeployMissingRequirement` · `TestPathReasonDetail` · `TestHandleDeployRequirementPresent`
  `TestHandleDeployInvalidArtifact` · `TestHandleDeployRemove` · `TestHandleDeployRemoveWithoutArtifact` · `TestHandleDeployUnsupportedAction`
  `TestHandleDeployDrainingCtx` · `TestCachePutGetRoundTrip` · `TestCachePutIdempotent` · `TestCachePutDigestMismatch`
  `TestCachePathTraversalRejected` · `TestCacheRemove` · `TestCacheAppliedReportsContentDigest` · `TestCacheAppliedOrderAndSkipsGarbage`
  `TestCacheAppliedEmptyIsNotNil` · `TestSafeJoin` · `TestCachePrune` · `TestCheckReportsAllProblemsAndExitsOne`
  `TestCheckJSONShapeAndCodes` · `TestCheckOKExitsZeroWithoutExecutingOrWriting` · `TestCheckJSONOKShape` · `TestCheckBrokenYAMLListsInvalidYAML`
  `TestCheckMissingFileIsCommandError` · `TestCheckUsageErrors` · `TestCheckAcceptsFlagsAfterPath` · `TestCheckHelpListsBoundaries`
  `TestUsageListsEverySubcommand` · `TestAgentCommandsMatchDispatch` · `TestHelpExitsZeroAndShowsCommandTable` · `TestVersionIsOneLine`
  `TestSkillInEmptyDir` · `TestVersionAndSkillShareVersionString` · `TestPreludeCommandPosition` · `TestSubcommandAfterGraceFlag`
  `TestVersionAndSkillIgnoreBrokenConfig` · `TestCapabilitiesEmptyDataDirExitsZero` · `TestCapabilitiesListsCachedArtifacts` · `TestCapabilitiesHelpExplainsEmptyIsZero`
  `TestStatusLive` · `TestStatusStaleWhenPIDGone` · `TestStatusStaleWhenHeartbeatOld` · `TestStatusMissingStateFile`
  `TestStatusHelpSaysLocalView` · `TestStateWriterSnapshots` · `TestStateWriterFromDaemon` · `TestClientHelloWelcomeHeartbeat`
  `TestClientHelloReportsCachedArtifacts` · `TestClientDeployThenHelloReconcile` · `TestClientAssignAckThenResult` · `TestClientAssignByDigest`
  `TestClientFailedCommand` · `TestClientCancel` · `TestClientGracefulShutdown` · `TestClientNoArtifact`
  `TestClientPassesParams` · `TestClientTraceIDAndDigestReachTask` · `TestClientSemaphoreLimitsConcurrency` · `TestClientHandshakeRejected`
  `TestClientConnectionClosed` · `TestHeartbeatInterval` · `TestClientAckBeforeSemaphore` · `TestServeNodeRealWSAssignAckResult`
  `TestServeNodeRealWSGracefulShutdownKeepsResult` · `TestRunsJSONShape` · `TestRunsJSONEmpty` · `TestRunsOutputModeBothWays`
  `TestShowJSONMatchesDisk` · `TestShowWindows` · `TestShowAcceptsTracePrefix` · `TestRunsTracePrefix`
  `TestAmbiguousTracePrefix` · `TestEmitExitKinds` · `TestEmitJSONSuccessAndTextMode` · `TestUpgradeJSON`
  `TestShowRunNotFoundKind` · `TestRunArtifactKindReusesArtifactCode` · `TestSubcommandHelpShowsExitCodes` · `TestConfigLoadBootstrap`
  `TestConfigValidateErrors` · `TestConfigValidateListsAllProblems` · `TestConfigUnknownField` · `TestConfigMissingFile`
  `TestConfigStreamURL` · `TestConfigHello` · `TestConfigHelloEmptyApplied` · `TestConfigDefaults`
  `TestDefaultDataDir` · `TestConfigFromEnv` · `TestConfigFromEnvNeedsToken` · `TestConfigFromEnvBadInstances`
  `TestConfigFromEnvMissingServer` · `TestLoadConfigFromPrecedence` · `TestRunEmitLocalModeNoSocket` · `TestRunEmitSocketMissing`
  `TestRunEmitBadArguments` · `TestRunEmitHelp` · `TestRunEmitAgainstServer` · `TestRunEmitExitCodeTable`
  `TestRunEmitWaitRoutedFlag` · `TestMainWithEmitDispatch` · `TestEmitCLIRealProcess` · `TestEmitCLIRealProcessLocalMode`
  `TestResolveEmitPort` · `TestBuildEnvEmitPorts` · `TestBuildEnvEmitPortsEmpty` · `TestBuildEnvEmitPortsSchemaNull`
  `TestBuildEnvEmitPortsWriteFails` · `TestBuildEnvEmitPortsSameSourceAsExpects` · `TestRunJobEmitPortsProbe` · `TestEmitPortsOfFallsBackToArtifactOutputs`
  `TestBuildEnvEmitPortsCandidatesCarryOwnNeeds` · `TestRunEmitTaskFileHappyPath` · `TestRunEmitTaskFileSinglePortNoTo` · `TestEmitSourceRejectsBothEntrypoints`
  `TestEmitSourceRejectsBadTaskFile` · `TestEmitSourceTaskFilePermTooOpen` · `TestEmitSourceTaskFileMissing` · `TestEmitSourceTaskFileEmptySock`
  `TestEmitCLITaskFileRealProcess` · `TestEmitServerDurablePath` · `TestEmitSocketPermissions` · `TestEmitRejectToken`
  `TestEmitTokenEmptyMeansNoAuth` · `TestEmitRejectPort` · `TestEmitRejectSchemaMissingField` · `TestEmitToPinnedCandidateUsesItsNeeds`
  `TestEmitRejectDataNotObject` · `TestEmitRejectOversize` · `TestEmitRejectRateLimit` · `TestEmitSeqPersistedAcrossRestart`
  `TestEmitAckStopsResend` · `TestEmitWaitRouted` · `TestEmitWaitRoutedHubRejects` · `TestEmitGapRequeuesEarlier`
  `TestEmitRejectsAfterTaskCancel` · `TestEmitSocketRemovedOnClose` · `TestEmitSocketRemovedWithTask` · `TestClientEmitSocketAndAck`
  `TestRunnerInjectsEmitEnv` · `TestClientResendsUnackedOutboxOnRedispatch` · `TestClientHelloCarriesAgentVersion` · `TestHelloAgentVersionDevBuild`
  `TestLogsServeHitReturnsArchiveTail` · `TestLogsServeMissReturnsNoLocalArchive` · `TestLogsServeTruncatesToOneMiB` · `TestLogsServeDropsControlBytesAndInvalidUTF8`
  `TestRunStoreTaskRefFallsBackAndRepairs` · `TestRunStoreGCDeletesOnlyMatchingIndex` · `TestRunStoreIndexSkipsUnsafeTaskID` · `TestLogsServeRealWSRequestAfterTask`
  `TestRunMissingConfigFile` · `TestRunBadConfig` · `TestGraceDuration` · `TestParseFlagsDefaults`
  `TestParseFlagsExplicit` · `TestParseFlagsUnknownFlag` · `TestRunHelp` · `TestNewLoggerLevel`
  `TestRunStandaloneEndToEnd` · `TestRunStandaloneFlagsAfterPath` · `TestRunStandaloneExitCodePassThrough` · `TestRunStandaloneTimeout`
  `TestRunStandaloneOwnExit124` · `TestRunStandaloneBadInput` · `TestMainWithDispatch` · `TestSubcommandAfterGlobalFlags`
  `TestFrameGoldenUpstream` · `TestFrameTypeConstantsMatchServer` · `TestFrameEncodeError` · `TestHelloGoldenReadableByServer`
  `TestResultFrameHasNoOut` · `TestResultFrameTagsMatchServer` · `TestResultFrameReadableByServer` · `TestDecodeDownstream`
  `TestBackoffDelaySequence` · `TestBackoffDelayJitterBounds` · `TestClassifyNodeFailure` · `TestClassifyHandshakeTimeout`
  `TestDialErrorCarriesRealStatus` · `TestDialErrorNoResponse` · `TestHandshakeRejectKeepsMessage` · `TestRetryStateLogNoise`
  `TestReconnectLoopRetriesTransientUntilSuccess` · `TestReconnectLoopResetsAfterHandshake` · `TestReconnectLoopPermanentStopsImmediately` · `TestReconnectLoopNoRetry`
  `TestReconnectLoopSignalDuringBackoff` · `TestReconnectLoopSignalDuringAttempt` · `TestServeNodeCallsHandshakeHook` · `TestPostHandshakeDropIsTransient`
  `TestNodeConfigErrorExitsNonZero` · `TestParseFlagsNoRetry` · `TestRunStandaloneArchivesToRuns` · `TestRunStandaloneNoDataDirDoesNotArchive`
  `TestRunStandaloneTimeoutArchivesWithRef` · `TestRunStandaloneArchiveTruncatesHugeStream` · `TestRunJobParamEnv` · `TestRunJobEnvIdentity`
  `TestRunJobEnvEmptyTraceID` · `TestRunJobWorkDirIsolation` · `TestRunJobCleansWorkDirOnSuccess` · `TestRunJobKeepsWorkDirOnFailure`
  `TestRunJobWorkDirRejectsBadTaskID` · `TestRunJobShebangPicksInterpreter` · `TestRunJobExitCodePropagated` · `TestRunJobFailureErrorUsesStderrFirstLine`
  `TestRunJobSuccess` · `TestRunJobSuccessEmptyOutput` · `TestRunJobNoOutPlumbing` · `TestRunJobTimeout`
  `TestRunJobTimeoutFromMaxDuration` · `TestRunJobCanceled` · `TestRunJobWaitDelay` · `TestRunJobWaitDelayKeepsTailOutput`
  `TestRunJobNoArtifact` · `TestRunJobSeparatesStdoutStderr` · `TestRunJobLogsTruncatedPerStream` · `TestRunJobInvalidUTF8Replaced`
  `TestRunJobSummaryIsFirstStdoutLine` · `TestStreamLogWindow` · `TestBuildEnv` · `TestBuildEnvPathPrefix`
  `TestBuildEnvPathPrefixEndToEnd` · `TestBuildEnvInjectsWorkDir` · `TestBuildEnvEmptyParams` · `TestBuildEnvParamError`
  `TestEffectiveTimeout` · `TestParamEnvName` · `TestOneLine` · `TestOneLineSummaryThroughRunJob`
  `TestRunJobIgnoresLegacyOutFile` · `TestRunJobLegacyOutFileNoEffectOnFailure` · `TestRunJobFailureKeepsWorkDir` · `TestRunJobUndeclaredOutNoLongerFails`
  `TestRunJobExampleCapability` · `TestRunJobSummarySurvivesTailTruncation` · `TestRunPersistArchiveKeepsFullOutputBeyondUploadWindow` · `TestRunPersistSaveKeepsFullOutputAndInput`
  `TestRunPersistCaptureOffByDefault` · `TestRunPersistExitCode` · `TestRunPersistClientArchivesBeforeResult` · `TestRunPersistBrokenRunsDirKeepsTaskSucceeded`
  `TestRunPersistStoreRejectsFileAsRunsDir` · `TestRunsCLISeedRefs` · `TestRunsCLIRecentList` · `TestRunsCLIListLimit`
  `TestRunsCLITraceMode` · `TestRunsCLICapabilityMode` · `TestRunsCLIShow` · `TestRunsCLIShowTruncatedHint`
  `TestRunsCLIShowHalfWritten` · `TestRunsCLIEmptyAndNotFound` · `TestRunsCLIBadArgs` · `TestRunsCLIHelp`
  `TestRunsCLISubcommandRegistration` · `TestRunStoreSavePathAndFiles` · `TestRunStoreListFiltersAndOrder` · `TestRunStoreLoadRoundTrip`
  `TestRunStoreSaveIsIdempotent` · `TestRunStoreSaveFailsGracefully` · `TestRunStorePathSegmentsFromPeer` · `TestRunLogKeepsHead`
  `TestRunStoreGCByAgeAndCount` · `TestStartRunGCStopsOnCancel` · `TestSchemaOutputStableAndParseable` · `TestSchemaDefaultIsCapability`
  `TestSchemaUnknownNameIsUsageError` · `TestSchemaExtraArgsIsUsageError` · `TestSchemaHelpListsThreeNames` · `TestCapabilitySchemaMatchesValidate`
  `TestCapabilityCanonicalJSONSatisfiesSchema` · `TestCapabilitySchemaRequiredKeys` · `TestNodeSchemaMatchesConfigValidate` · `TestEmitSchemaMatchesWireContract`
  `TestEmitSchemaPortPatternMatchesOutputs` · `TestMain` · `TestHandleUpgradeRejectsBadSHA` · `TestHandleUpgradeRefusesForeignHost`
  `TestHandleUpgradeBadSHAOnDownload` · `TestHandleUpgradeSuccess` · `TestHandleUpgradeNeedRootDoesNotExit` · `TestUpgradeReplacesBinary`
  `TestUpgradeCheckModes` · `TestUpgradeSkipsWhenUpToDate` · `TestUpgradeForceOverwrites` · `TestUpgradeBadSHADoesNotReplace`
  `TestUpgradeDownloadInterruptedKeepsTarget` · `TestUpgradeServerNotEnabled` · `TestUpgradeNeedRoot` · `TestUpgradeRestartFailureStillZero`
  `TestUpgradeServerOverride` · `TestUpgradeHelpExitsZero` · `TestMainWithUpgradeDispatch` · `TestResolveTarget`
  `TestOpenDownloadFilePlacesTempBesideTarget` · `TestBinaryEndpoint` · `TestRestartCandidates` · `TestResolveChdirDefaultsToTaskDir`
  `TestResolveChdirUsesDeclaredDir` · `TestResolveChdirRejects` · `TestResolveChdirSymlinkResolved` · `TestRunJobWorkDirEndToEnd`
  `TestRunJobTaskDirWhenNoWorkDir` · `TestRunJobFailsWhenWorkDirMissing` · `TestRunJobFailsWhenWorkDirIsFile` · `TestRunJobFailsWhenWorkDirRelative`
  `TestResolveChdirExpandsHome` · `TestResolveChdirExpandedHomeStillChecked` · `TestResolveChdirRejectsUnsupportedHomeForms` · `TestRunJobWorkDirHomeEndToEnd`
  `TestRunJobFailsWhenHomeUnavailable` · `TestRunJobFailsWhenHomeIsRelative`

</details>


### G2 明确 pending（不是“已用别的实现替代”）

1. **结构化日志（受保护协议接缝）**：共享 `protocol.LogsResponseFrame` 目前只有
   `stdout/stderr/truncated/bytes/attempt`，没有 W6/A1 要求的逐条
   `sequence/stream/text/at`。G2 只做**真实** history node/attempt 定位、requestId/taskId/
   attempt 关联校验与合法响应转发；不补时间戳、不跨流排序、不把离线/缺档案/过期伪装成空成功。
   结构化采集与帧补口由父冻结的 Node-log 单元（`LOG-RECORDS-HANDOFF.md` R1–R8）交付，
   G2 侧 `LogsResult.Structured` 恒为 `false`，接入后再由父授权消费。
2. **事件 + 任务的跨事务原子性**：store 只有 `InsertEvent`（投递窗口 + 事件同一事务）与
   `CreateTasks`（整批任务同一事务），**没有**「事件 + 扇出任务」同一事务入口
   （`store.(*DB).InsertEvent` event.go:207；`store.(*DB).CreateTasks` task.go:254；
   无 `AcceptEventTasks` / tx 变体）。G2 的顺序是 InsertEvent → 剪枝留痕 → CreateTasks：
   任务批失败时事件已存在，调用方拿到错误，重投由 deliveryId 窗口吸收；不伪造原子性、
   不返回假成功回执。需要真正跨表原子时须由 store owner 增补事务入口（不在 G2 写权限内）。
3. **emit 已声明但下游解析不到**：capability.md §6.2 要求 `downstream_missing` 是
   「任务失败 + 事件」，但 `CreateTasks` 要求 `capability_id/capability/trace` 且只允许
   `pending` 起始，无法为解析不到的能力落一条 failed 任务。G2 记一条真实剪枝观察
   （`source='route'`、type=`downstream_missing`）并回 `taskId:null + pruned`；
   不捏造 failed task；属已知契约/存储缺口，父裁决后补。
4. **日志 HTTP 五态映射由 G3 落地**：G2 提供可判别错误——`store.ErrNotFound` /
   `ErrNoAttempt`（404 任务不存在/跨 ns/无该 attempt）、`ErrNodeOffline`（503）、
   `ErrLogsTimeout`（504）、`ErrLogsCorrelation`（502 非法/错误绑定回包）、
   帧内 `error=no_local_archive`（410 真实 attempt 但档案不在，原样转发）。
   `previous=true` 且确实无上一 attempt → `Empty=true`（对应 200 + attempt 0 + 空数组），
   仅此场景使用 attempt 0。
