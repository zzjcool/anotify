# E1 · 已创建失败Task的 emit 回执身份补齐（父冻结）

> 迁移自 .pi-orchestrator/v4/EMIT-FAILED-TASK-RECEIPT-HANDOFF.md（2026-11-19，协调者工作区不再入库）

## 已确认事实与裁决

G2-R1当前 code9a2b95c / docs6ecb938 的 missing分支已同EmitAuthorized事务真正创建failedTask+错误事件，但`DispatchFailureInputs`无callerID、私有newDispatchFailureTask现场生成ID、writeEmitPlannedFactsOn只回error。因此route200写`taskId:null,eventId,to,version`且无pruned；现anodeCLI严格视为无效成功回执→exit5/resultunknown，重复emitid只重放坏body。没有已批准此例外，不能拿“诚实unknown”当本scope闭环。

**不需要新增wire字段或Task数组**：现`EmitResponse.taskId`本义是“已建Task id”，失败Task也是已建Task。保持现5字段和A1共享DTO，只把真实刚创建的失败Task id放进已有taskId，不能添加failureTaskId/tasks/failed等新字段、不能tasknull+pruned误判剪枝、不把失败Task当Node已执行成功。

## E1-D1 · store 内部输入可预分配TaskID

允许`internal/store/dispatch_failure.go`的store本地typed `DispatchFailureInputs`增加**非wire**可选`TaskID string`（无用户protocol JSON字段）；构造有caller真实ID则用它，省略仍store.NewTaskID（保持Source正常多fanout内部行为）。ID必须合法TaskID且碰撞/不匹配整个emit事务回滚，不overwrite既有Task；不能靠选现有TaskID关联别的失败。

只为解决调用方需要稳定回执身份，不改变`EmitAuthorized`公共签名/鉴权顺序/历史重放/result原字节规则。业务emitid相同重复仍授权先于重放，回第一次真实taskId/EventID，新的候选ID不能写入；sameid不同body仍冲突，无id每次新Task事实。错误事件与该TaskID绑定一致、不能store另换ID或后查询猜。

## E1-D2 · route 已知真实创建身份

在`internal/route/emit.go`单个目标missing分支：预生成store.NewTaskID，放入对应PlannedFailure的typedInput.TaskID，同时现`EmitResponse.TaskID`置此ID；To/声明Version/ErrorEventID仍真实，`Pruned`保持nil，不造digest/Node结果。失败Task的Capability存真实请求basename（不是把name@v整体塞进name再重复Version），声明version单独保；其输入必须用严格引用解析，不能fmt.Sscanf前缀接受`@3junk/+3/空格`当精确版。普通目标和missing两边严格`@`正整数字符引用规则一致，歧义/坏ref沿现not_declared拒绝，不新reason。

`rec.Result`/实际store emitResult当前权威构造必须返回同一真实TaskID并同事务持久（若store覆盖调用方Result，按actual接口补齐该单点，不假定原字节原样存）；失败任何写不返回成功body；重复回读storedresult准确原task/event ID而非新候选。不得先外insertTask/recordPrune/裸SQL或把API改为“以返回HTTPerror但行已commit”不明确状态。未声明且未解析的version不存在事实，允许仅给共享`EmitResponse.Version`加omitempty以不公开哨兵0；已解析成功与显式声明正版本的version仍恒返回。此为同wire现字段presence的明确例外，不新增字段/数组或另一DTO，Node CLI/protocol执行帧不变。

若actual单emit plan出现>1 failure，不靠选第一个假完整，应给精确gap父裁决；不因此扩新数组wire。当前Source receipts有tasks数组与此无关。

## E1-D3 · 新覆盖与不弱化

- Store suppliedTaskID正常失败往返/事件指向一致、invalidID/碰撞全rollback（task/event/emit无新facts）、omitted仍生成真实TaskID。
- route `to=missing@v`收到非null真实taskId/无pruned，GetTask同namespace可见phasefailed0/声明version/无fakeNode/digest/startedAt；与错误事件task绑定。
- 显式emitid第一次与重放body字节相同、task/errorEvent各一份；每次noid都新真实failedtask；撤currentcredential/ns/lease/cancel授权0附带写；不重做所有C3cases，复用现强覆盖并新对应missing case。
- 按现anode CLI成功验收predicate（taskId非null）不再resultunknown；不改NodeCLI/protocolwire/退出码定义。旧unknown断言改成正确新契约的可区分断言，原失败行为作为修复前RED保日志，不弱为“任一字段非空”。

## 文件唯一所有权与顺序

原G2R1writer/父独立gate/review须先结束源码冻结，再一个E1worker串行做`internal/store/dispatch_failure.go`+directtests（必要currentemissionplannedinput/emitResult传递保持同tx、只对应tests）→`internal/route/emit.go`+emit_test.go、`route.go`仅strictref parsing及direct tests→共享`internal/protocol/emit.go`仅Version的omitempty/TaskID包含已创建派发失败Task的准确注释+直接DTOroundtriptests。同WT只有这一writer；这是已冻结的小型dependentstoreinput/routeconsumer接缝，不并发两个writer/新WT，不把原store/protocol大重构带回来。

除上节唯一EmitResponse字段presence/注释授权外，protocol/Node/Source/ws/server/API/Go依赖/FE/其他任务字面owner只读。旧S1/Node/G2R1收据属于旧epoch，不能认证E1新代码；其他G2R1fresh findings父收齐后再有限裁决，不把所有review照单转给E1扩大scope。

固定verify：readonlyfmt internal/store/internal/route/internal/protocol+gitdiffcheck；govet store/route/protocol；gotest ./internal/store/... ./internal/route/... ./internal/protocol/... -count1；其余ws/lease/source/trigger/logging/artifact/protocol count1。各900托管/完整log/真实exit，GOdirect/sum/auto。新store局部race可跑相关包，但不重做未变Node/r9/replay样本。相同根因2失败SAFE_BLOCKED，不重复旧baseline或把新case只绿当全scope。

owneronly本地commit允许；不push/MR/merge/rebase/reset/squash/env/settings/.git/node_modules/用户data/旧WT/session/log清理。worker/scout不指定model/preset。完成E1不等G2/G3/v4Done。
