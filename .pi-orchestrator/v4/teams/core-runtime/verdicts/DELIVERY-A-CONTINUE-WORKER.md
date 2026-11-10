# DELIVERY A continuation（fresh leaf）· 交付报告

- canonical cwd（每次 bash 都带 `cd` + `pwd -P` + `git rev-parse --show-toplevel` 双守卫）：
  `/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery`
- HEAD：`55e3e46`（未提交；按卡由 manager 统一提交）
- 夹具完整性：`internal/store/v4_fixture_test.go` =
  `16a97a33…e149e81c`、`internal/store/v4test/fixture_test.go` = `b5c16dc7…c46b893`
  —— 与 manager 实测值一致，**本 leaf 未改夹具**。

## 做了什么（按卡顺序）

1. **真实剩余 fixture 族**（全部用当前 C2/C3 真 API，无 legacy alias/compat/hook/skip）：
   - `internal/store/playbook_test.go`：重写为 v4 目录包（`files` 原始包体 / Create-Get-List-
     Replace(CAS)-Delete(kept) / Render 只读 / Install 真事务 + dryRun 0 写 / 回执 + installs 折叠组）。
   - `internal/store/node_test.go`：重写为 v4 节点（集群级节点 / 申报∩授权 / 配置 revision+CAS /
     applied 事实 / RecordHello 快照替换 / slots / LeaseSlot-ReleaseSlot / eligible 原因词表 / grant 认证）。
   - `internal/store/source_test.go`：重写为 v4 源（targets 引用 / `$(payload.…)` 模板 /
     payloadSchema / closed config fields / LookupHook / ListDueSources / 名字不可变）。
   - 小族一次到位：`orphan_test.go`（8 条，含新增 pinned-digest 判定）、
     `upgrade_gate_test.go`（params-only 闸门 + UpdateNext 原地改线）、`stmt_wiring_test.go`
     （v4 读路径 + ListTasks 16 组合）、`pool_test.go`（countRows 签名冲突 → countRowsWhere）、
     `migrate_test.go`（未用 strconv）、`capability_spec_test.go`（Artifact() 取址）、
     `bench_test.go`（外部测试包 32 条 → v4 三个写路径基准 + 护栏）。
2. **安全族精确映射**：`session_lease_safety` / `session_lease` / `tx_merge` / `builtin`
   逐条落到 `TEST-MAPPING.md` 新附录（每条 = 旧名 → 具体现名 + 文件:行 + 不变量，
   或窄退役的具体产品理由）。缺实证的两条 **已按原名恢复**：
   `internal/store/tx_merge_test.go:66 TestReportResultDoesNotReleaseForeignSlot`、
   `:111 TestReportResultRejectsTerminalTaskWithoutTouchingSlot`（v4 真夹具：auth/slot/attempt/lease）。
3. **编译闭合实测**（A 门禁）：`timeout 180s env GOPROXY=direct GOSUMDB=sum.golang.org
   GOTOOLCHAIN=auto go test ./internal/store/... -run '^$'` → **RC=0**
   （`ok internal/store`、`ok internal/store/v4test`，均为 `[no tests to run]`，**不声称测试 PASS**）。
   基线 582 + 32 条编译错误全部清零；未删除任何测试文件（`tx_merge_test.go` 为恢复新增）。

## 改动文件清单

- `internal/store/playbook_test.go`
- `internal/store/node_test.go`
- `internal/store/source_test.go`
- `internal/store/orphan_test.go`
- `internal/store/upgrade_gate_test.go`
- `internal/store/stmt_wiring_test.go`
- `internal/store/bench_test.go`
- `internal/store/pool_test.go`
- `internal/store/migrate_test.go`
- `internal/store/capability_spec_test.go`
- `internal/store/tx_merge_test.go`（新增；恢复原两条安全用例）
- `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md`（A-continue 附录：逐名映射 + 运行记录 + 上报）

（工作区里另有 `DELIVERY-PROGRESS.md` / `task-cards/DELIVERY-A-CONTINUE.md` / `CANONICAL-CWD-SOP.md`
的改动不是我做的，未触碰。）

## 新增 / 恢复的测试名

- 恢复：`TestReportResultDoesNotReleaseForeignSlot`、`TestReportResultRejectsTerminalTaskWithoutTouchingSlot`
- 新增：`TestNodeConfigRevisionAndAppliedFacts`、`TestNodeAuthorizationGrant`、
  `TestOrphanPendingOnlyPinnedDigestCounts`、`TestRenderPlaybookReadOnly`、
  `TestInstallPlaybookDryRunWritesNothing`、`TestListPlaybookInstallsGroups`、
  `BenchmarkAssignGrant`、`BenchmarkCompleteTask`、`TestBenchmarksTargetRealPaths`（改写保留）
- 概念替换（v3 概念已删 → v4 真事实）：`TestSourceTargetsParamsValidation`、
  `TestSourcePayloadSchemaRoundTrip`、`TestSourcePayloadSchemaValidation`、
  `TestUpdateSourceKeepsPayloadSchema`、`TestSourceConfigRoundTrip`、
  `TestSourceConfigPayloadOnlyForKeepingKinds`

## 验证命令与结果（原样）

```text
# A 门禁（compile only）
CMD  timeout 180s env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto \
     go test ./internal/store/... -run '^$'
LOG  /tmp/anotify-v4-delivery-a-continue-gate-final-061402.log
ok  	github.com/zzjcool/anotify/internal/store	0.009s [no tests to run]
ok  	github.com/zzjcool/anotify/internal/store/v4test	(cached) [no tests to run]
RC   0

# 安全族自证
CMD  … go test ./internal/store/ -count=1 -run 'TestReportResultDoesNotReleaseForeignSlot|…|TestAbandonPendingTask'
LOG  /tmp/anotify-v4-delivery-a-continue-safetyrun2-061213.log
ok  	github.com/zzjcool/anotify/internal/store	0.144s
RC   0

# 本 leaf 改写的全部 fixture 族
CMD  … go test ./internal/store/ -count=1 -run 'TestNode|…|TestUpgradeImpact|TestStmtWiring|TestReportResult'
LOG  /tmp/anotify-v4-delivery-a-continue-myfamilies2-061351.log
ok  	github.com/zzjcool/anotify/internal/store	0.716s
RC   0

# gofmt（scoped）
gofmt -l internal/store/   → 无输出，rc 0
```

## 发现的产品问题（未改生产，按卡上报）

1. **升级闸门写路径不再执行**：`checkUpgradeOn` 只被 `CheckUpgrade` 调用
   （`internal/store/upgrade.go:119`），`CreateCapability`（`capability.go:150`）不调用；
   `CreateCapabilityForce` 已不存在但 `internal/server/console_handlers.go:1406` 仍调用。
2. **playbook 安装不校验保留源名 route**：`playbook_tx.go:385` → `insertSourceOn`
   无保留名检查（直写路径 `source.go:150` 有），v3 该用例无法迁移。
3. **同内容不同版本号的 playbook 能力**会撞 `(ns,name,fold_key)` 唯一索引并返回裸约束错误，
   而非 `ErrVersionConflict`。
4. **零出口源被接受**：`source.go:266` → `sourcespec.ValidateTargets`（`config.go:210`）对空列表返回 nil，
   `Targets: nil` 的源能建出来（事件永不派发）；v3 有这道闸门。

## 遗留 / 阻塞（不是本 leaf 的夹具问题）

- 整包 `go test ./internal/store/ -count=1` 仍 **31 条红**，全部落在本 leaf 未改写的族
  （authz/namespace CAS、capability spec 严格校验、event list before/stats、migration/baseline、
  retention/pending、auth 迁移8、task_test 的 complete/cancel/maxattempts/snapshot）——
  这些族在 v4 重写后从未能执行（原包一直编译不过），属下一 leaf 返工面。
  明细与日志：`TEST-MAPPING.md` 附录 + `/tmp/anotify-v4-delivery-a-continue-storeall-final-061412.log`。
- `go build ./internal/server/` 仍红（lease/route/server 的 v3 API），属 B/T2 轮。
- 未 commit / 未 push / 未 merge（按卡由 manager 提交）。

## 未决问题（需父会话裁决）

- 上述产品发现 1–4 是否在本轮修生产、还是登记为 B/T2 项。
- 31 条整包红是否另派 leaf（建议按族切：namespace/authz、migration/baseline、event/stats、task）。
