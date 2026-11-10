DONE G1-REVIEW-REWORK — **ROOT 裁定已落实，限定范围闭合；不是 G1/T1 批准声明。**

本次只追加改动：
- `internal/store/capability_test.go`
- `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md` 的两条原始映射

此前四个测试文件的改动全部保留。未改产品、恢复旧 API/ghost guard，未新增 status 字段；无子 agent、commit、push 或 MR。

### 新增测试与映射
新增 `capability_test.go:536 TestCapabilityGhostNextIsPreservedAndResolvedMissing`：
- 带 `ghost-target@7` 的 CREATE 成功；
- GetCapability 声明及 EdgesFrom 原始引用精确往返；
- 实际 ResolveRef 返回 `ErrNotFound`，不是 `ErrRefUnresolved`；
- 已存在能力仍可正常解析。

两条旧映射已无待裁定措辞：
- **M209**：仅窄退役旧批量 API/CREATE ghost 前置拒绝；当前多资源失败零写引用 `playbook_test.go:892 TestInstallConflictRollsBackWholePackage`。
- **M219**：仅窄退役旧 CREATE ghost 前置拒绝；能力正文＋边原子性引用 `capability_test.go:485 TestCapabilityCreateEdgeWriteFailureRollsBack`。
- 两行均补入 ghost-next 正向证明。此前并发创建、Source Update 映射及其他测试加强保留。

### 最终三个门禁原始输出

`/tmp/anotify-v4-g1-review-rework-root-ruling-gate1-format-diff.log`
```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery
+ gofmt -l internal/store internal/auth/password_test.go internal/sourcespec internal/playbook
+ git diff --check HEAD -- internal/store/task_test.go internal/store/capability_test.go internal/store/v4test/schema_baseline_test.go internal/store/schema_baseline_test.go internal/store/v4_whitebox_test.go internal/store/v4test/resource_cas_test.go internal/store/v4test/source_test.go .pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md
EXIT 0
```

`/tmp/anotify-v4-g1-review-rework-root-ruling-gate2-vet.log`
```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery
+ GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/... ./internal/auth/... ./internal/push/... ./internal/sourcespec/... ./internal/playbook/...
EXIT 0
```

`/tmp/anotify-v4-g1-review-rework-root-ruling-gate3-test.log`
```text
/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery
+ GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/store/... ./internal/auth/... ./internal/push/... ./internal/sourcespec/... ./internal/playbook/... -count=1
ok  	github.com/zzjcool/anotify/internal/store	8.940s
ok  	github.com/zzjcool/anotify/internal/store/v4test	36.663s
ok  	github.com/zzjcool/anotify/internal/auth	22.190s
ok  	github.com/zzjcool/anotify/internal/push	0.008s
ok  	github.com/zzjcool/anotify/internal/sourcespec	0.004s
ok  	github.com/zzjcool/anotify/internal/playbook	0.123s
EXIT 0
```

独立 SUBSHELL、600/600/1200 秒期限、无退出码掩盖。已重读本次完整 diff 并核对 `:485`、`:536` 锚点。**限定实施无阻塞**；后续 supervisor 验证及 G2/G3/G4/最终门禁保留。

{"ok":true,"reason":"ROOT ruling implemented; bounded scope closed; format/diff, vet and exact five-tree tests all exit 0; ready for supervisor verification, not G1/T1 approval","changedFiles":["internal/store/task_test.go","internal/store/capability_test.go","internal/store/v4test/schema_baseline_test.go","internal/store/v4_whitebox_test.go",".pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md"],"touchedThisRuling":["internal/store/capability_test.go",".pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md"],"newTests":["TestCapabilityCreateEdgeWriteFailureRollsBack","TestCompleteEmptyFailureProjectsPhaseAndKeepsRawResultFacts","TestCapabilityGhostNextIsPreservedAndResolvedMissing"],"oldMaps":{"TestCreateCapabilitiesBatchRollsBackOnBadLink":"ROOT narrow-retired old batch API/ghost CREATE guard; current atomicity maps to TestInstallConflictRollsBackWholePackage:892; positive ghost case:536","TestCreateCapabilityWithLinksRollsBackOnBadLink":"ROOT narrow-retired ghost CREATE guard only; current atomicity maps to TestCapabilityCreateEdgeWriteFailureRollsBack:485; positive ghost case:536","TestCreateCapabilityWithLinksConcurrent":"retained actual concurrent proof at resource_cas_test.go:673-690","TestUpdateSourceRebuildsEntryEdges":"retained actual update/projection proof at source_test.go:235-253"},"actual3Exits":[0,0,0],"logs":["/tmp/anotify-v4-g1-review-rework-root-ruling-gate1-format-diff.log","/tmp/anotify-v4-g1-review-rework-root-ruling-gate2-vet.log","/tmp/anotify-v4-g1-review-rework-root-ruling-gate3-test.log"],"blockers":[]}
