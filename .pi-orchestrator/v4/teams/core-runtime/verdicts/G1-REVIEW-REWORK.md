## G1 bounded rework 报告

**5 个文件改动，三个固定门禁均实际 exit 0。** 未改产品、schema、认证、Node 或协议；未启动子 agent，未 commit/push/MR。**不声明 G1/T1 DONE**：两条旧坏引用映射仍需父裁定，并须 fresh targeted re-review。

### 改动文件
- `internal/store/task_test.go`
- `internal/store/capability_test.go`
- `internal/store/v4test/schema_baseline_test.go`
- `internal/store/v4_whitebox_test.go`
- `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md`

### 测试与映射
- 加强非法相位测试：有效 currentAuth/attempt/lease；前后比较 tasks、全部 attempts、node_instances **所有列**及 manifests/uploads/objects，明确是前置零写，不冒充事务回滚。
- 加强已有索引测试：独立硬编码 35 个当前 v4 显式索引，检查列名/顺序、unique、partial 及部分索引谓词。
- 新增 `TestCapabilityCreateEdgeWriteFailureRollsBack`：真实 edge INSERT 失败；触发器确认新能力与第一条边已写入，随后能力正文/digest及全边均回滚。
- 新增 `TestCompleteEmptyFailureProjectsPhaseAndKeepsRawResultFacts`：无 Error/Reason/Summary 时投影为字面 `failed`，raw Error 仍为空。
- 取消测试改名为 `TestWhiteboxCancelledBeginTxPreservesBaselineCheckpoint`，比较完整 ledger，收窄为驱动 BeginTx 取消。
- 修正 24 个 playbook 原始引用及本轮改动引起的声明锚点；未重做 460-name inventory。

| 旧映射 | 本轮处理 |
|---|---|
| `TestCreateCapabilitiesBatchRollsBackOnBadLink` | 移除 ResolveRef 伪回滚映射；注明旧批量 API 删除，引用真实单建/整装回滚，但不冒称坏引用 guard 等价 |
| `TestCreateCapabilityWithLinksRollsBackOnBadLink` | 改引新增真实 edge-write 回滚测试；旧 ghost-target 拒绝边界明确待裁定 |
| `TestCreateCapabilityWithLinksConcurrent` | 改引已有 `resource_cas_test.go:673`，实际并发断言 `681–689` |
| `TestUpdateSourceRebuildsEntryEdges` | 改引已有 `source_test.go:209`，实际 UpdateSource＋投影往返 `235–253` |

### 最终验证原始输出

日志：`/tmp/anotify-v4-g1-review-rework-gate1-format-diff.log`
```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery
+ gofmt -l internal/store internal/auth/password_test.go internal/sourcespec internal/playbook
+ git diff --check -- internal/store/task_test.go internal/store/capability_test.go internal/store/v4test/schema_baseline_test.go internal/store/schema_baseline_test.go internal/store/v4_whitebox_test.go internal/store/v4test/resource_cas_test.go internal/store/v4test/source_test.go .pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md
EXIT 0
```

日志：`/tmp/anotify-v4-g1-review-rework-gate2-vet.log`
```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery
+ GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/... ./internal/auth/... ./internal/push/... ./internal/sourcespec/... ./internal/playbook/...
EXIT 0
```

日志：`/tmp/anotify-v4-g1-review-rework-gate3-test.log`
```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery
+ GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/... ./internal/auth/... ./internal/push/... ./internal/sourcespec/... ./internal/playbook/... -count=1
ok  	github.com/zzjcool/anotify/internal/store	9.034s
ok  	github.com/zzjcool/anotify/internal/store/v4test	41.311s
ok  	github.com/zzjcool/anotify/internal/auth	22.309s
ok  	github.com/zzjcool/anotify/internal/push	0.007s
ok  	github.com/zzjcool/anotify/internal/sourcespec	0.004s
ok  	github.com/zzjcool/anotify/internal/playbook	0.102s
EXIT 0
```

各门禁独立 SUBSHELL，期限 600/600/1200 秒，无管道掩盖退出码。已逐文件重读完整 diff。

### 未决边界
当前 store 单写入口在 `capability.go:150–164`；`next` 原样投影在 `capability.go:346–372`，读取方解析说明在 `graph.go:27–29`。而 `capability.md:725–728` 定义的是 HTTP 单建严格边界。**不能据此把旧 ghost-target/ErrEdgeRejected/links[1] 断言宣布等价迁移，也未据此认定产品缺陷。**

建议父会话裁定 M209/M219 的窄迁移/退役边界，再对这五个文件做 fresh targeted re-review。G2/G3/G4及最终全 Go/contract/quick 门禁保留；其他会话的进度、审查和归档改动未触碰。

{"ok":false,"reason":"5 files changed; fixed gates all exit 0; M209/M219 legacy bad-reference equivalence requires parent adjudication and fresh targeted re-review; no G1/T1 approval claimed","changedFiles":["internal/store/task_test.go","internal/store/capability_test.go","internal/store/v4test/schema_baseline_test.go","internal/store/v4_whitebox_test.go",".pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md"],"newTests":["TestCapabilityCreateEdgeWriteFailureRollsBack","TestCompleteEmptyFailureProjectsPhaseAndKeepsRawResultFacts"],"oldMaps":{"TestCreateCapabilitiesBatchRollsBackOnBadLink":"removed batch API; real rollback citations, legacy guard equivalence withheld","TestCreateCapabilityWithLinksRollsBackOnBadLink":"actual edge-write rollback covered; ghost-target guard equivalence withheld","TestCreateCapabilityWithLinksConcurrent":"existing concurrent assertions at resource_cas_test.go:673-690","TestUpdateSourceRebuildsEntryEdges":"existing actual update and projection assertions at source_test.go:235-253"},"actual3Exits":[0,0,0],"logs":["/tmp/anotify-v4-g1-review-rework-gate1-format-diff.log","/tmp/anotify-v4-g1-review-rework-gate2-vet.log","/tmp/anotify-v4-g1-review-rework-gate3-test.log"],"blockers":["Parent adjudication of M209/M219 legacy bad-reference boundaries","Fresh targeted re-review before safe G1 checkpoint"]}
