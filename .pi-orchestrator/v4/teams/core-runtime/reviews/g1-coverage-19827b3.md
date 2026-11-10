# G1-4 checkpoint 只读审查（52b17a6）

范围：internal/artifact、internal/sourcespec、internal/store/v4test、旧 internal/store/*_test.go；未跑 build/test/baseline。状态口径：PROGRESS.md:13 记“独立审查在途…不当全量DONE”，小包 PASS 不等全仓 PASS；旧 in-package 编译失败属 G1-6，未计完成。G1-5/G2 未实现按指示不 finding。

## Findings（严重度 · 证据）

1. **[medium] Emit 命名空间授权零覆盖** — emission.go:301-304 实现 申报∩授权 判定（错误 ErrEmitNamespaceDenied, emission.go:23）；emit_test.go:223-266 只测 node token/shadow/attempt/lease/token/expiry，v4test 全包无该错误引用。E1“namespace 由权威 task 与 Node 申报∩授权推导”改坏仍绿。
2. **[medium] Emit 同事务原子性/并发未证** — emission.go:150-200 单事务+唯一冲突兜底（emission.go:181）；emit_test.go:50/98/300 全串行。无“子任务写入失败→emit 0 行”回滚用例、无并发同 id 用例；终态任务 emit 拒绝（phase!=running）只测了取消分支（emit_test.go:272）。
3. **[medium] 旧 attempt result 拒绝未测** — task.go:831 校验 attempt/lease；v4test 仅当前 attempt 或终态后调用 CompleteTask（task_test.go:222,369,390,424），无 attempt+1/错 lease 用例。E1“旧 attempt 令牌/结果/emit 拒绝”只覆盖令牌+emit。
4. **[medium] D2 任务输入可复现载体缺/未测** — schema.sql:335 有 config_revision、task.go:667 写入，但 v4test 无断言（task_test.go:16 只查 attempt/lease）；schema 中无 namespaceVarsRevision 或等价 ns 输入快照。D2 要求首次派发留下 ns 默认值可复现载体、换节点重试沿用。
5. **[medium] cron 调度查询无 v4 用例** — source.go:520 ListDueSources；v4test/source_test.go:28-77 只验建源时 NextFireAt，无 due/limit/跨 ns；旧 source_test.go:273 DueCronSources 未接住。
6. **[low] 节点重 hello 覆盖声明能力未测** — node.go:296 覆盖写 declared_capabilities；无二次 hello 用例（旧 node_test.go:162 NodeCapabilitiesReplacedOnRehello）。
7. **[low] parentTaskId 往返/跨 ns 拒绝未测** — task.go:306-318 有跨 ns 检查；v4test 无 ParentTaskID 引用，emit_test.go 的 childTasks 不设 parent。
8. **[low] source targets.params 往返未断言** — source_test.go:219,233 写入 params，247 只回读 Capability；params 解码坏掉仍绿。
9. **[low] artifact/protocol ParamEnvName 双实现** — artifact.go:680 vs protocol/env.go:34；artifact_test.go:797 只测私有副本。N1 快照 19827b3 晚于本 commit，G1-6/G2 须切换共享函数并删副本。
10. **[info] next-only“不推节点”仅间接** — capability_test.go:436 断言 version/digest 不变；无“UpdateNext 不产生 deployment/通知”用例（G2 补）。
11. **[info] applied hash 未接 MachineConfigHash** — config.go:782 存任意串，config_test.go:294 只往返 "sha256:applied"；G2 生产端需加同函数用例。
12. **[status，不计完成] 旧 store tests 不可编译** — store_testutil_test.go:28/32/39/40 引用已删 Spec.Handler/Lifecycle/HandlerSpec/HandlerNoop/ParamsSchema/Outputs/Sticky，拖住全部 50 个旧测试文件（含保留域）。PROGRESS.md:13 未宣称全仓 PASS。

## 保留域清单（现存、未删、当前被编译阻塞；G1-6 必须恢复，不得退役）

- auth_test.go:18/49/88/126；authz_test.go:17/95（authz.go:56-63 注释仍写 v3 nodes.namespace_id，v4 授权已改交集，G2/G3 须改语义并迁移 95 行用例，不可删）
- passkey_backup_test.go:12；passkey_mgmt_test.go:11/54/105/125
- devices_update_test.go:11；password_test.go:17/36/75/106/125/163/232/268/344；admin_test.go:11/57/87/127/183/258/325
- internal/push/filter_test.go:25/60/86/113（独立包，未受影响）

## TEST-MAPPING（具名旧→新；不宣称可删）

- **capability_test**: CapabilitySameVersionDifferentContentRejected/VersionAppendOnly/EmptyListIsArray→v4test/capability_test.go 同名；LifecycleIdempotentDefault→capability_spec_test.go:TestCapabilitySpecDefaultsExplicitEquivalent + artifact TestDefaultsNormalized；EdgeRejectsPortNotInOutputs→设计退役（无 port），由 TestSchemaBaselineNoHistoricalBaggage 禁列监听。
- **capability_spec**: CapabilitySpecRoundTrip→v4test/capability_spec_test.go:TestCapabilitySpecRoundTrip；CapabilityVersionConflict→TestCapabilityVersionConflict；BuiltinHandlers/Migration9Backfill→随 builtin/migration9 退役，G1-6 删除前须登记。
- **task_test**: Transition/TerminalImmutable/TransitionCASAgainstConcurrency→v4test/task_test.go:TestTaskGrantAttemptOnlyOnGrant/TestTaskCompleteReleasesSlot/TestTaskCancelWaitsForReap；ExtendLease→TestTaskRenewLease；SweepLeases*→TestTaskRecoverIdempotentRequeues/TestTaskMaxAttemptsThreeTotal/TestTaskMaxAttemptsTerminal；CancelTask→TestTaskCancelPendingIsTerminal/TestTaskCancelRevokesAuthorization；CountTasksByStatus→v4test/stats_test.go。
- **node_test**: NodeCreateAndGet→v4test/cluster_node_test.go:TestNodeLifecycle；LeaseInstanceAndRelease/UpsertInstancesKeepsBusyTask/FreeSlotsByNode→TestNodeInstancesAndLeases；NodesForCapability→TestEligibleNodesReasons；NodeCapabilitiesReplacedOnRehello→缺失(F6)；NodePins/PutPinValidation/NodeTagsRoundTrip→设计退役。
- **source_test**: CronParsing/CronNext→sourcespec/spec_test.go:TestNormalizeCron/TestNextCronTime；DueCronSources→缺失(F5)；SourceCRUD/Validation/SourceKindImmutable→v4test/source_test.go:TestSourceCronConfig/TestSourceKindAndNameImmutable；SourceWebhookToken→同名；RejectsReservedName→TestSourceKindAndNameImmutable 保留名分支。
- **emission_test**: EmissionRoundTrip/InsertEmissionIdempotent→v4test/emit_test.go:TestEmitIdempotentReplay/TestEmitIDConflict；EmissionSeqStatus→设计退役，新语义 TestEmitWithoutIDAlwaysNew+TestEmitNotMergedAcrossTasks；EnsureTaskTokenConcurrent→TestTaskAttemptToken（无并发等价，G2 补）。
- **event_task_test**: EventIdempotency→v4test/event_test.go:TestEventDeliveryWindowIdempotent/Permanent；CreateTasksAtomic/Fanout→TestTaskListRoundTrip 超限整拒 + emit 子任务（同事务回滚缺 F2）；TaskRoundTrip→TestTaskListRoundTrip；ReportResultWritesTerminalState→TestTaskCompleteReleasesSlot。
- **retention_test**: DeletesExpired/KeepsNotYetExpired/KeepsNonTerminal→v4test/retention_test.go:TestRetentionTaskTTL；EventsCustomRetention→TestRetentionEventTTL；BatchLimit→TestDeleteExpiredPlaybookInstallsPrimitive；新增引用组/去重解耦→TestRetentionKeepsEventsReferencedByTasks/TestRetentionSweepsDedupRecordsNotEntities。
- **deployment_test**: CreateDeploymentIdempotent→v4test/deployment_test.go:TestDeployCreatePending/TestDeployNewDigestSupersedes；DeploymentsForNode→TestDeployDeliveryForNode；MarkDeployedDigest→TestDeployReconcile；Artifact*（artifacts 表）→设计退役，内容身份由 artifact tests 承接。
- **migrate_test**: ParseMigrations/-Gap/-FailureRollsBack→v4test/migrate_test.go 仅公开面；包内 parse/回滚用例由 G1-6 补（migrate_test.go:11-14 自述）。
- **schema_baseline_test**: TableSet/NoHistoricalBaggage/Indexes/Unique/Reopen→v4test/schema_baseline_test.go 同名；LegacyDatabaseRejected→TestLegacyDatabaseRejectedVersion/Tables。
- **session_lease/report_next/source_entry_edges/reconcile/pool/pending_backlog/orphan/graph/edge_***→v4test/task_test.go、edges_test.go、source_test.go、deployment_test.go 对应投影/回收用例；priority/literals/mapping 由 schema 门禁监听退役。
- **playbook_test（27 例）**→G1-5 后续（按指示不 finding）；**builtin_test（18 例）**已随中枢 executor 退役，但 lifecycle/clean-env/样例脚本语义须由 artifact 默认值测试+G2 executor 用例接住，不作为“可删”。

{"ok": true, "reason": "recon complete: 12 findings (5 medium, 3 low, 2 info, 1 status, 1 info), 4 named coverage gaps + reserved-domain migration list; G1-4 not full-repo DONE, G1-6 must restore auth/passkey/device/push tests"}
