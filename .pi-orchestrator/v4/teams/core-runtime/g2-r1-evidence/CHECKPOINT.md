# G2-R1 owner checkpoint（worker 自留，不是父验收）

- cwd: `/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery/.pi-subagents/runs/r-01be4aa7/worktrees/core-g2-resume`
- 入场 HEAD: `80eb523c9dd79819244a2320a7dff90c31ae2a30`（clean；S1 `5dc3f27` docs `ae3ace9` + Node logs `4c822a7` 已由父精确集成）
- 角色: 唯一 G2 runtime source writer；不 push/MR/merge/rebase/reset/squash；只 owner 本地提交。
- 范围: internal/{route,ws,lease,source,trigger,logging} 相关行为 + TEST-MAPPING 有限 G2 行 + 本 evidence 目录。
- 只读: store/protocol/artifact/sourcespec/Node/server/api/tools/Makefile/Go deps/FE。

## 收口状态（全部 C1-C7 已实施）

| case | 状态 | 证据 |
|---|---|---|
| C1 dispatch configRevision 与 welcome 同源（node_configs.revision） | DONE | `internal/ws/hub.go` DispatchOnce；`TestDispatchConfigRevisionMatchesWelcome`（两值可区分 + accept→grant） |
| C2 `ProposeTaskAssignment` 稳定未到期提案；`RejectAssignmentAuthorized` | DONE | `hub.go` DispatchOnce / `handlers.go` handleReject；`TestDispatchProposalStableUntilExpiry`、`TestRejectAssignment*`、`TestAcceptSlotPresence` |
| C3 source 单次 `AcceptSourceEvent`；emit 传 PlannedPrunes/Failures 同事务；missing→failed task+event；receipt 投影 | DONE | `internal/source/source.go`、`internal/route/emit.go`+`route.go`；`TestIngest*`、`TestEmitMissing*`、`TestEmitPrune*`、`TestEmitRevoked*` |
| C4 `MarkDeployResultAuthorized` + `CreateDeployments` 原子授权 | DONE | `handlers.go` handleDeployed、`hub.go` Deploy；`TestDeployRejects*`、`TestDeployedReceipt*` |
| C5 logs waiter 有界 once/remove/nonblock + disconnect wake + tail/since/lines 严格校验 + detach deadline | DONE | `hub.go` Logs 段；`TestLogsWaiterBoundedCorrelationAndWake` |
| C6 emit 声明 ref 语义（唯一裸 to 按声明；显式 @v 精确；歧义 not_declared） | DONE | `route.go` declaredRef；`TestDeclaredRef`、`TestPlanFromEmitResolvesDeclaredRefs`、`TestEmitPinnedDeclaredRefPinsChildAndTracksMutableNext` |
| C7 握手 fail closed / 模板单趟类型 / 有限 mapping 行 | DONE | `TestHandshakeRejects*`、`TestPlanFromSourceTemplatesSinglePassAndKeepTypes`；TEST-MAPPING 12 行 + R:V3_CENTRAL 注记 |

## 门禁（真实 log + exit，2026-01 本轮）

- gate1 只读 fmt + `git diff --check`：`/tmp/anotify-g2r1-gate1-format.log` → EXIT:0
- gate2 六包 `go vet`：`/tmp/anotify-g2r1-gate2-vet.log` → EXIT:0
- gate3 六包 `go test -count=1`：`/tmp/anotify-g2r1-gate3-runtime-test.log` → EXIT:0（route/ws/lease/source/trigger/logging 全 ok）
- gate4 `store/artifact/protocol` `-count=1`：`/tmp/anotify-g2r1-gate4-store-artifact-protocol-test.log` → EXIT:0

## 在途 task id / 子 agent

- 无（本 worker 不派子 agent、不开后台任务；>60s 命令均 full tee 并核对真实 exit）。
