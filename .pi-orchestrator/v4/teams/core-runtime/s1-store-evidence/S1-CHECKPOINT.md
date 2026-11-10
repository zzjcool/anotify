# S1-STORE-G2-SEAMS · checkpoint（store 唯一 writer）

- 时间边界：父提示「临近上限前必须落真实 checkpoint」，本文件为**事实快照**，不是完成声明。
- 分支：`pi-subagent/core-g2-resume-52fc54d4`（canonical G2 worktree）。
 - **code commit：`5dc3f27`**（S1/D7 store 实现 + tests + 接口卡）；docs：`e815e06`（checkpoint）/ `d6fa733`（verdict）。
- **当前 tree HEAD：`d6fa733`**，工作区 clean；均本地 owner-only commit，未 push/MR/merge/rebase/squash。
- 基线 docs HEAD `323c969` / source `cba97d4`；分支 `pi-subagent/core-g2-resume-52fc54d4`。
- D5 是文件所有权/顺序（只 store、唯一 writer），D6 是测试门禁清单——二者**不是**新 API 章；
  INTERFACES.md 只列实际新增 Go 面（D1 无新 public API）。
- 父提示已收到：只收口 S1+D7，不扩 runtime/协议/Node/650 映射；源码 freeze 后由父接有限 fresh review。
- 接口卡已落：`.pi-orchestrator/v4/teams/core-runtime/s1-store-evidence/INTERFACES.md`（D1–D7 typed 字段/错误/schema 决策）。

## 已完成（代码 + 白盒测试，均已在本机单测跑绿）

### D1 未解析失败 Task
- `schema.sql`：`tasks.capability_id` nullable；两条 CHECK（NULL ref 只允许 failed/attempt=0/downstream_missing/空 digest/无 Node/lease/token/候选/过程事实；resolved ref 必须 `digest<>'' AND version>0`）；FK 保留。
- `task.go`：scanTask NULL→"" 受控映射；`CreateTasks` 拒绝空 capability_id / 空 digest / version<=0（但排在 params 超限检查之后，保留原 `ErrParamsTooLarge` 优先序）。
- `execution_auth.go`：GrantLeaseAuthorized 的 capability_id 用 NullString，NULL ref 明确 `ErrInvalidTransition`。
- 新 `dispatch_failure.go`：`DispatchFailureInputs`、失败 Task 构造/原子写 + downstream_missing 观察事件。
- 新 `dispatch_failure_test.go`（读路径可见 / 9 种非法 NULL 形状被 CHECK+FK 拒 / 合法形状往返 / CreateTasks pin 守卫 / 不是派发/授权候选）。

### D2 原子 ingress
- 新 `source_event.go`：`SourceEventInputs`、`SourceEventReceipt(+Task/Failure/Prune)`、`AcceptSourceEvent`（单 BEGIN IMMEDIATE：source id+resource_version CAS、namespace 存在+inputs revision pin、投递窗、event+receipt 列、正常任务、失败 Task/错误事件、剪枝事件；重复回持久 receipt 原字节；`ErrSourceEventReceiptMissing`/`ErrNamespaceInputsChanged`）。
- `event.go`：`insertIngestEventOn`（receipt 列）、`readEventReceiptOn`、`InsertPruneEvent` 重构到共用 `insertPruneFactEventOn`（语义不变）。
- 新 `source_event_test.go`：正常四类事实+receipt 往返；重复同字节/0 新写；并发重复恰好 1 first；无 deliveryId 原子；窗口过期新事件；源 CAS/ABA/namespace revision 0 写；**6 个真实 trigger 故障点**（event/receipt/window/normal task/failure task/prune）全回滚+window 未占+重试成功；0-fanout 空数组；旧窗口无 receipt 明确报错；strict layout 重开 + 删 receipt 列旧布局拒绝且不删数据。

### D3 Emit 计划事实
- `emission.go`：`EmitPruneFact`/`EmitFailureFact` + `EmitRecord.PlannedPrunes/PlannedFailures`（`json:"-"`）；`writeEmitPlannedFactsOn` 在授权/replay 之后同事务写失败 Task+错误事件+剪枝事件；prune 错误不吞。
- 新 `emission_facts_test.go`：同事务事实；replay 0 附带写；错凭证/已取消 0 写；prune 写失败整事务回滚（emit 行/任务不留）。

### D4 proposal/reject authority
- 新 `assignment_authority.go`：`ProposeTaskAssignment`（未到期原样返回不 churn；expiry==now 到期；offline/撤销 ns 0 写；不授 lease/attempt/busy）、`RejectAssignmentAuthorized`（当前 credential/ns/Node/assignment/proposed/未过期；闭集 reason；bounded backoff 不计 attempt）。
- 新 `assignment_authority_test.go`：多 sweep 不 churn；到期新绑定；offline/ns 撤销；reject 成功/重复幂等错误/0 写矩阵（旧 token、错 Node、错 assignment/attempt、坏 reason、过期、撤销 ns）/旧请求不损新提案。

### D7 部署授权
- `deployment.go`：`CreateDeployments` 同 tx 先对全部显式目标验当前 ns 授权（`namespaceAllowedOn`），任一失败全包 0 写、不 supersede 先前合法 intent；offline 已授权仍 pending。新增 `MarkDeployResultAuthorized`（`DeploymentResultInputs`：deploymentID/requestID/ns/cap UID/name/version/digest/state/reason/now；同 tx current credential/ns/Node/intent 身份/requestId/digest；重复精确同结果幂等，changed body `ErrDeploymentReceiptConflict`；`DeploymentRequestID` 稳定格式 `dep_<id>`）。
- 新 `deployment_auth_test.go`：未授权/混合 0 写且保留旧 intent；offline 已授权 pending；applied 往返+幂等不重写 updated_at+changed body 冲突；错 token/跨 Node/unknown intent/错 request/错 digest/错 capability/坏 state/撤销 ns 全 0 写；旧 intent 移除后 receipt 不改新 intent。
- 新 `v4test/dispatch_failure_visibility_test.go`（公开面读路径）：**当前未跑**（见剩余）。

### 既有 fixture 补正（有具名原因，不改强弱断言）
- `v4test/format_test.go` 种子任务补 digest（S1-D1 resolved pin CHECK）。
- `v4test/schema_baseline_test.go` C3b 用例由 DROP COLUMN 改 RENAME COLUMN（tasks CHECK 引用 result_json，SQLite 不许 DROP；「缺列严格拒绝」断言不变）。
- `v4test/resource_cas_test.go`、`internal/store/node_test.go` 两处：显式部署改为「先在授权内下发、再撤销授权」/补 declared namespace（S1-D7 真实时序）。
- `v4test/cluster_node_test.go` 直插 running task 补 digest（真实 pin）。

## 交付面汇总（21 files，+2866/−23，见 `git show --stat 5dc3f27`）

- 新增源码：`source_event.go`、`dispatch_failure.go`、`assignment_authority.go`（另有 6 个新 test 文件）。
- 修改源码：`schema.sql`（tasks nullable + 2 CHECK；events.receipt）、`task.go`、`event.go`、`emission.go`、
  `execution_auth.go`、`deployment.go`；fixture 补正 4 处（具名原因，见 verdict）。
- 实际新增 public API：
  - `AcceptSourceEvent(ctx, SourceEventInputs) (json.RawMessage, bool, error)` + `SourceEventReceipt` 家族；
  - `ProposeTaskAssignment(ctx, AssignmentProposalInputs) (AssignmentProposal, error)`；
  - `RejectAssignmentAuthorized(ctx, NodeAuthorization, AssignmentRejectionInputs) error`；
  - `MarkDeployResultAuthorized(ctx, NodeAuthorization, DeploymentResultInputs) error` + `DeploymentRequestID`；
  - `EmitRecord.PlannedPrunes []EmitPruneFact` / `PlannedFailures []EmitFailureFact`（`json:"-"`，非 wire）。
- 新错误哨兵：`ErrSourceEventReceiptMissing`、`ErrNamespaceInputsChanged`、`ErrDeploymentReceiptConflict`。
- D7 已收：`CreateDeployments` 同 tx 全目标 ns 授权（未授权/混合 0 写、不 supersede、offline 已授权 pending）；
  `MarkDeployResultAuthorized` 同 tx credential/ns/Node/intent/requestId/digest，重复精确幂等、changed body 冲突。

## 风险 / 待父序列化（不含新 prod 问题上报）
- G2 runtime 尚未消费新入口；旧 `AssignTask/RejectAssignment/MarkDeployResult/InsertEvent` 仍服务旧调用方。
- G3 负责失败 Task wire 省略 version/digest；旧 v4 布局库被严格 Open 明确拒绝（不删数据，本轮 baseline 例外）。
- 未发现需要上报父的新 prod 问题；未新增 scope、未改 runtime/protocol/Node/650 映射。

## 5 门禁真实状态（最终版，全绿）

| gate | 命令 | 真实输出 / exit |
|---|---|---|
| 1 | `gofmt -l internal/store`（只读检查）+ `git diff --check` | 无输出；`gofmt_list_exit=0` / `diffcheck_exit=0` |
| 2 | `GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/store/...` | 无输出；`vet_exit=0` |
| 3 | `… go test ./internal/store/... -count=1` | `ok github.com/zzjcool/anotify/internal/store 5.497s` + `ok …/internal/store/v4test 30.942s`；`gate3_exit=0` |
| 4 | `… go test -race ./internal/store/... -count=1` | `ok …/internal/store 81.747s` + `ok …/internal/store/v4test 378.542s`；`gate4_exit=0` |
| 5 | `… go test ./internal/route/... ./internal/ws/... ./internal/lease/... ./internal/source/... ./internal/trigger/... ./internal/logging/... ./internal/artifact/... ./internal/protocol/... -count=1` | route 0.371s / ws 1.993s / lease 0.169s / source 0.274s / trigger 0.102s / logging 0.004s / artifact 2.664s / protocol 0.064s 全部 `ok`；`gate5_exit=0` |

## 明确剩余
1. ~~v4test visibility helper~~ 已改 `encoding/json.Unmarshal`，单跑 `ok`。
2. ~~5 门禁~~ 已全部真实跑绿（上表）。
3. ~~本地 commit~~ 已落 `5dc3f27`（本 checkpoint 随 docs commit 落库；未 push/MR）。
4. 父侧后续序列化（不在本 worker scope）：G2 runtime 消费 `AcceptSourceEvent`/`ProposeTaskAssignment`/
   `RejectAssignmentAuthorized`/`EmitRecord.Planned*`/`MarkDeployResultAuthorized`；G3 投影失败 Task
   省略 version/digest；旧 `MarkDeployResult`/`RejectAssignment`/`AssignTask` 由后续 owner 切流。

## 在途受管命令
- 无。5 门禁均已在前台跑完并记录 exit（无裸后台/无挂起命令）。

## 遗留风险
- `MarkDeployResult`（旧内部入口）保留给现有 ws 调用方，尚未被新 Authorized 入口替代（G2 后续消费，属父序列化范围）。
- 失败 Task 的 wire 投影（version/digest 省略）明确留给 G3。
- 旧 v4 布局库（无 receipt 列 / capability_id NOT NULL）会被严格 Open 明确拒绝，不删数据；这是本轮 baseline 重建的既定例外。
