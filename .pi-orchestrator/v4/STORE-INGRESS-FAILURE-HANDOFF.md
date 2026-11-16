# S1 · G2 受保护 store 接缝补口（父冻结）

## 依据与不做项

两路只读侦察与第二意见已确认：G2 Source 当前两个独立 commit 会消费 delivery window 后丢 fanout；store没有公开同事务 ingress 入口。用户已批准的 `capability.md §6.2` 要求 `downstream_missing` 为失败 Task + 事件，不是 `param_* / no_target / event_schema` 的剪枝。§8.3 及派生 Task 示例缺“未解析派发失败”的表示例外，不能凭示例把失败降级或造幽灵 Capability。

本轮未发布 v4 baseline 允许直接修正，仍保严格Open/layout/reopen/拒旧库不删除数据。D1-D6/C3结果授权/身份域不变，不新增兼容API/第二套wire/事务框架、store不得依赖route/ws/broker。只在当前G2已冻结树的store范围由一个专属worker实施；runtime/协议/Node/FE写入之后序列化。

## S1-D1 · 未解析失败 Task 表示

- 正常已解析任务仍钉住真实 Capability version UID/name/version/digest/params；不可变/版本FK保留，不弱化CreateTasks的正常pending校验。
- `downstream_missing` 独立产生**中枢派发失败 Task**，phase=failed、attempt=0，无Node/slot/lease/token/namespace机器grant/过程startedAt/exitCode/伪Node结果事实；创建/派发完成时间可用真实server事务时刻，不能伪造Node过程时间。
- 保留请求capability name；若请求明确声明正版本，保其版本；若未声明则内部Version=0仅表示“没有声明/解析事实”，wire省略该version而非公开0。没有resolved digest时wire省略digest；绝不造hash/version UID/幽灵版本行。
- `capability_id` 仅此严格未解析失败分支允许SQL NULL；`digest`保持现内部空串表示没有解析事实，Version保现整数（0或明确声明版本）。不要把所有Task字段改指针引发无关跨模块改造。scan用受控NULL→内部空字符串映射，反向只在明确未解析失败fact路径写NULL；不可把普通pending漏capUID写成NULL。
- 加明确可判定的内部解析状态（由capability_id是否有真实值派生，不必新增持久布尔列）；所有普通pending/attempt/grant/lifecycle入口拒未解析失败，不能把nullable当普通任务放行。schema/入口约束至少拒NULL ref的非failed、attempt>0、非空lease/token/Node/grant事实、非downstream_missing错误或假digest；已解析ref继续FK/版本一致性规则。
- 新失败Task与其错误事件同事务，Task/Trace读列表不能因NULL ref隐藏/scan失败。错误投影沿现reason/message对象，不能回填C3原始Node resultJSON或nullable ExitCode。
- 这是§8.3及Task示例的明确例外：只有成功解析的执行任务承诺完整version/digest pin；未解析派发失败保真实请求，省略不存在事实。不改变§6.2码含义。

## S1-D2 · 原子 ingress 和 receipt

冻结新增public入口：

```go
func (*DB) AcceptSourceEvent(ctx context.Context, in SourceEventInputs) (receipt json.RawMessage, first bool, err error)
```

`SourceEventInputs`须为store本地typed事实（无route/broker依赖），包含命名空间、当前Source UID/期望spec版本（或等价身份CAS）、期望namespace输入revision、Event、deliveryId/窗口/可信serverNow、正常planned Tasks、未解析DispatchFailure facts、纯Prune event facts。具体字段名称在实施前写接口卡，含义本节定死，不擅省略。

- 一次内部事务：当前Source身份/namespace/期望config与namespace输入pin校验 → delivery重复判定 → Event + delivery window +全部正常/失败Task+错误/剪枝event facts+完整receipt → commit。
- 事务内校验必须用tx，不从`d.sql.DB`读取逃出同一snapshot。预编译任务遇配置/namespace修订失配明确拒绝/0写，由runtime重新计划而非接受陈旧参数。源不存在/越namespace/标识ABA同样0写。
- 请求无deliveryId也必须原子。重复delivery在有效窗口内返回第一次的真实完整receipt（event/trace/task IDs/pruned），first=false，不创建新任务/消费新窗口。允许正常0fanout事件，如no_target。
- receipt可作为事件内部持久事实储存（如events中的专属JSON列），不新增Resource种类/独立receipt API；必须参与严格layout与往返测试。不得由第二次查询当前可变资源重算第一次receipt。
- 任一事件/任务/错误事件/窗口/receipt写失败，全部回滚，重试不被已消费窗口吞掉。禁止外BeginTx套旧InsertEvent/分两步假原子。旧独立Event API仍可服务独立日志/事件域，不新增兼容shim。
- dryRun只内存计划，不调用本写入口，不能因读取新增receipt而产生副作用。

## S1-D3 · Emit 剪枝/失败事实同授权事务

- 现 `EmitAuthorized(ctx, EmitAuth, *EmitRecord, []*Task)` 公共签名保持不变，C3credential/ns/lease/token/cancel授权先于历史重放不变。
- 如需扩展typed信息，允许给现store本地EmitRecord增加非wire的计划事实字段（Prune/DispatchFailure事件与请求目标），不用新增另一授权入口或runtime调用裸SQL。新字段不给JSON旧协议别名，也不能由Node直接控制授权/历史事实。
- 现单事务内复用private正常task/失败task/事件helper；独立id相同body replay只返回原完整receipt，新业务id重复0附带写，changed body仍冲突。无id每次新事实。
- `recordPrune`必须从runtime提前写改为传入这些事实，当前credential/namespace已撤销时，连剪枝event也0写。任何prune写错误不能吞掉/成功回执。

## S1-D4 · proposal/reject 原子 authority

G2 review已证旧提案被每sweep换assignment导致churn，task.reject缺当前凭证/namespace/目标Node绑定。允许store专属补口：

```go
func (*DB) ProposeTaskAssignment(ctx context.Context, in AssignmentProposalInputs) (AssignmentProposal, error)
func (*DB) RejectAssignmentAuthorized(ctx context.Context, auth NodeAuthorization, in AssignmentRejectionInputs) error
```

- Proposal只在pending且可派时写30s提案，不授lease/attempt/busy。未到期已存在的同Task提案返回**原完整assignment/node/proposedAttempt/expiry**，不覆写；过期才允许新提案，真实server时间相等视到期，Node候选及namespace事实同tx检查。
- Reject同tx核当前credential hash、namespace仍可用、task提案确属于该Node且assignment/proposedAttempt等匹配；旧token/另一Node/撤销ns/旧id/过期提案不能改当前提案或退避，无错误请求损伤新reservation。
- reject reason仍闭集stale_config/slot_unavailable，不计执行attempt，真实boundedbackoff；重复相同reject可保持明确幂等但不能替换新提案。不会使用Get/precheck后裸RejectAssignment代authority。
- Grant/renew/result/emit/cancel现C3入口不重构或弱化；新producer/consumer之后由G2 owner消费。

## S1-D5 · 所有权和顺序

唯一store writer允许：新 `internal/store/source_event.go` / `dispatch_failure.go` /必要assignment-authority文件及对应tests；现schema.sql与严格layout校验、task字段scan/创建/相关query与tests、event私有helper/receipt列与往返tests、emission现事务附加计划事实与tests、namespace/Source实际tx读取必要helper。只写store，所有runtime/protocol/artifact/Node/server/API/Go依赖/FE/Makefile只读。既有G1/C3不变的断言不能删/弱化；fixture若需要明确resolved正常Task字段只能按真实新校验补正，有具体原因与具名映射。

协议分类只在后续独立owner序列化更正既有reason注释/集合（不新增reason/不改执行帧）；G2 runtime之后消费真实store卡，G3/FE最后接未解析Task省略version/digest。当前Node日志L1独立只读依赖，尚未验收不集成。

如果无法在上述文件范围内完成精确签名/严格layout，报契约矛盾/具体gap，禁止擅自指针化全部Task造成跨模块破坏。全部命令核唯一canonicalG2执行树，manager已retired无其他writer。

## S1-D6 · 固定机械门禁与新覆盖

1. canonical cwd/root/GOMOD、只读gofmt-l internal/store +git diff --check。
2. `go vet ./internal/store/...`。
3. `go test ./internal/store/... -count=1`（含v4test、原全部白盒，不仅新test）。
4. `go test -race ./internal/store/... -count=1`。
5. `go test ./internal/route/... ./internal/ws/... ./internal/lease/... ./internal/source/... ./internal/trigger/... ./internal/logging/... ./internal/artifact/... ./internal/protocol/... -count=1`（只读跨消费者回归，未接新入口不冒已解决runtime）。

GOdirect/sum.golang.org/auto；有界长任务受管fullLog/真实exit，不裸后台或timeout弱断言。相同根因两失败后SAFE_BLOCKED，不重做旧baseline求绿。

新增必须：event+window+normalTask+failureTask+prune/receipt任一点故障all0writes（真实trigger/constraint/取消，不只COUNT断言）；有/无deliveryId、并发重复完整相同receipt/不newtask、namespace/Source revision变动/ABA0写；未解析显式/省略version失败往返可见、非法nullablepending/attempt/假结果拒绝、resolvedFK仍拒破坏、严格Open新schema/重开/旧库保数据；Emit撤凭证/namespace/lease/id-replay前0附带写、prune失败全rollback；unexpired proposal多sweep不churn/到期新binding、wrongNode/currenthash/ns reject0write及旧请求不损新提案。

本地owner-only提交允许供父审；NO push/MR/merge/rebase/reset/squash/settings/env/.git/node_modules/旧WT/log/session/用户数据删除。完成是S1 store门槛，不是G2/G3/v4整体完成。
