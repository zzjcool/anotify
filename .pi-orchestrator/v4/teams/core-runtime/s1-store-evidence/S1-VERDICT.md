# S1-STORE-G2-SEAMS · verdict（store 唯一 writer）

- **code commit：`5dc3f27`**（源码 + tests + INTERFACES），docs checkpoint：`e815e06`。
- 基线：docs `323c969` / source `cba97d4`；分支 `pi-subagent/core-g2-resume-52fc54d4`。
- 未 push / 未 MR / 未 merge / 未 rebase / 未 squash（按父指令 owner-only 本地提交）。
- 接口卡：`.pi-orchestrator/v4/teams/core-runtime/s1-store-evidence/INTERFACES.md`
- checkpoint：同目录 `S1-CHECKPOINT.md`（五门禁实测）。

## 改动文件（21 文件，+2866/-23）

新增：`source_event.go`、`dispatch_failure.go`、`assignment_authority.go`、`source_event_test.go`、
`dispatch_failure_test.go`、`assignment_authority_test.go`、`emission_facts_test.go`、
`deployment_auth_test.go`、`v4test/dispatch_failure_visibility_test.go`。

修改：`schema.sql`（tasks nullable + 2 CHECK；events.receipt）、`task.go`（scan NULL→""、pin 守卫）、
`event.go`（receipt 列 helper、prune fact helper 复用）、`emission.go`（PlannedPrunes/Failures 同事务）、
`execution_auth.go`（grant 拒 NULL ref）、`deployment.go`（CreateDeployments ns 授权 pass +
`MarkDeployResultAuthorized`）、fixture 补正（`node_test.go`、`v4test/{format,schema_baseline,resource_cas,cluster_node}_test.go`，
每处带具名原因，未改断言强弱）。

## 接口（typed 字段 / 错误 / null 映射，详见 INTERFACES.md）

- `AcceptSourceEvent(ctx, SourceEventInputs) (json.RawMessage, bool, error)`；`SourceEventInputs{NamespaceID,SourceID,ExpectedSourceResourceVersion,ExpectedNamespaceVarsRevision,TraceID,Type,Payload,DeliveryID,WindowSec,Now,Tasks,Failures,Prunes}`。
- `SourceEventReceipt{EventID,TraceID,Tasks[],FailedTasks[],Pruned[]}` 持久化在 `events.receipt`；重复回原字节、`first=false`。
- `DispatchFailureInputs{Capability,Version(0=未声明),Reason(仅downstream_missing),Detail}`。
- `EmitRecord.PlannedPrunes []EmitPruneFact` / `PlannedFailures []EmitFailureFact`（`json:"-"`，非 wire）。
- `ProposeTaskAssignment(ctx, AssignmentProposalInputs) (AssignmentProposal, error)`；`AssignmentProposal{TaskID,AssignmentID,NodeID,NodeName,ProposedAttempt,ExpiresAt,Reused}`。
- `RejectAssignmentAuthorized(ctx, NodeAuthorization, AssignmentRejectionInputs) error`；`AssignmentRejectionInputs{TaskID,NodeID,AssignmentID,ProposedAttempt,Reason,Now}`。
- `MarkDeployResultAuthorized(ctx, NodeAuthorization, DeploymentResultInputs) error`；`DeploymentResultInputs{DeploymentID,RequestID,NamespaceID,CapabilityID,Capability,Version,Digest,State,Reason,Now}`；`DeploymentRequestID(id)="dep_"+id`。
- 新错误哨兵：`ErrSourceEventReceiptMissing`、`ErrNamespaceInputsChanged`、`ErrDeploymentReceiptConflict`。
- NULL 映射：`tasks.capability_id NULL` ↔ `Task.CapabilityID==""`（scan 受控映射；普通入口拒绝空串）；
  `Version=0` 只表示未声明/无解析事实，`digest=''` 表示无解析事实；wire 省略由 G3 投影。

## 失败 Task / receipt 事实

- `phase=failed`、`attempt=0`、`error='downstream_missing'`、NULL capability ref、空 digest、
  `created_at=finished_at=server tx now`；无 Node/slot/lease/token/assignment/startedAt/exitCode/result。
- 失败 Task 与 `source='route'/type=downstream_missing` 观察事件同事务；错误投影沿用 reason/message。
- receipt 含 event/trace/正常 taskIds/失败 taskIds（显式 version 或省略）/pruned；窗口内重复只回这份持久字节。

## 测试（新增 28 个 test func，全部真实断言）

- D2（10）：四类事实原子+receipt 往返；重复同字节/0 新写；并发重复恰好 1 first；无 deliveryId；
  窗口过期；源 CAS/ABA/namespace revision 0 写；**6 个真实 trigger 故障点全回滚+window 未占+重试成功**；
  0 fanout 空数组；旧窗口无 receipt 明确报错；strict layout/重开/旧布局拒绝不删数据。
- D1（4）：NULL ref 读路径可见；9 种非法 nullable 形状被 CHECK/FK 拒；普通建行 pin 守卫；不可派发/授权。
- D3（3）：计划事实同事务；replay 0 附带写；错凭证/取消 0 写；prune 失败整事务回滚。
- D4（6）：多 sweep 不 churn；到期新绑定；offline/撤销 ns；reject 成功/0 写矩阵/旧请求不损新提案。
- D7（4 + 公开面 1）：未授权/混合 0 写保留旧 intent；offline 已授权 pending；applied 往返+幂等+changed body 冲突；
  错 token/跨 Node/unknown request/错 digest/撤销 ns/superseded 0 写。
- 既有 G1/C3 store/v4test 断言不删不弱；fixture 仅按真实新校验补 pin/授权 setup（具名原因见 commit）。

## 5 门禁真实 exit（GOdirect/sum.golang.org/auto）

1. `gofmt -l internal/store` 无输出 + `git diff --check` 无输出 → exit 0/0
2. `go vet ./internal/store/...` 无输出 → exit 0
3. `go test ./internal/store/... -count=1` → `ok store 5.497s` + `ok v4test 30.942s` → exit 0
4. `go test -race ./internal/store/... -count=1` → `ok store 81.747s` + `ok v4test 378.542s` → exit 0
5. `go test ./internal/route/... ./internal/ws/... ./internal/lease/... ./internal/source/... ./internal/trigger/... ./internal/logging/... ./internal/artifact/... ./internal/protocol/... -count=1`
   → route/ws/lease/source/trigger/logging/artifact/protocol 全 `ok` → exit 0

## 遗留（父序列化，不在本 worker scope）

- G2 runtime 尚未消费新入口（`AcceptSourceEvent`/`ProposeTaskAssignment`/`RejectAssignmentAuthorized`/
  `EmitRecord.Planned*`/`MarkDeployResultAuthorized`）；旧 `AssignTask`/`RejectAssignment`/`MarkDeployResult`/
  `InsertEvent` 仍服务旧调用方，需后续 owner 切流。
- G3 wire 投影失败 Task 的 version/digest 省略；旧 v4 布局库（无 receipt / capability_id NOT NULL）
  严格 Open 明确拒绝且不删数据（本轮 baseline 重建既定例外）。
- 本卡完成 = S1 store 门槛，不等于 G2/G3/G4/v4 整体完成。
