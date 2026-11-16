# G2-R1 · runtime 范围内有限返修与接缝消费（父冻结，尚未派writer）

## 入场条件与范围

原G2 code epoch `cba97d4`，docs delivery `323c969`；三fresh审均ok:false，四局部门禁0不能盖critical/major。旧manager已retired，不resume/重扫650名。S1 store补口及Node-log R9正在各自唯一writer实施。**本runtimewriter只在S1已源码冻结/独立review清major/父真实门禁收齐后串行派发**，不得与storewriter同WT并发写。Node/protocol日志补口必须先父验收、在安全依赖边界集成再消费；未集成时不能造private wire或称logs完成。

所有权：`internal/route/**`、`ws/**`（保独立broker/device通知WS）、`lease/**`、`source/**`、`trigger/**`、`logging/**`仅相关真实适配；`teams/core-runtime/TEST-MAPPING.md`只G2相关有限行、新G2-R1证据/接口卡。不改store/protocol/artifact/sourcespec/Node/server/API/工具/Go依赖/FE/Makefile，不重做旧executor/cmdagent退役。**旧notify.phone中央Capability及其Task状态断言是刻意退役，不恢复也不要求“替代中央任务通知能力”**；独立POST notify/auth/broker/device/push域仍保，后G3/G4仅核其真实独立域回归，不能把两域混淆而复活executor。

## R1-C1 · config revision真实一致

`Hub.Dispatch`不能用Node resourceVersion充作applied config revision。welcome/config/assignment配置事实统一取真实Node Config.Revision（含当前hash），dispatch/accept与共享真实runner严格相同表示。新增真sharedprotocol+Node runner校验或等价真实consumer判等测试，不能只断言非空或令mock与错误生产值一起改。

## R1-C2 · proposal与reject authority

消费S1的真实 `ProposeTaskAssignment` / `RejectAssignmentAuthorized`（接口卡字段以验收后的actual版本为准）；未到期提案稳定不每sweep换assignment，不计attempt/授slot，过期才新提案。Reject使用会话实际捕获的nodeID/tokenHash，reason闭集、declaredproposed/slot等presence严格，当前ns/credential与Node绑定同事务；旧请求不能伤新提案。不得Get/precheck+裸Assign/Reject替authority。

新增至少：未到期连续sweep一份assignment、真实runner接/执行而非staleconfig空转、expired旧id、wrongNode/rotatedtoken/revokedns reject0write、slot0与缺失/负数presence。

## R1-C3 · Source/Emit原子receipt

- Source Fire消费S1 `AcceptSourceEvent`真实typed事实，Event/window/正常与未解析failedTask/pruneevent/receipt一次tx，source/ns修订失配明确0写不吞重试。无deliveryId也atomic，duplicate返回真实最初完整receipt，dryrun0写。
- Emit不提前`recordPrune`，将prune/dispatchfailure计划传入store现EmitRecord扩展非wire字段，`EmitAuthorized`同tx currentcred/ns/lease/token/cancel授权先于全部写与历史replay。错误绝不吞或假200；业务id与requestId不混，noid每次新facts。
- `downstream_missing`是failedTask+errorEvent，保真实请求name/明确version，无解析事实省略version/digest，不放进pruned/taskIdnull；param_*/no_target/event_schema才纯剪枝。失败Task无Node执行attempt/exit/伪过程时间。不可改S1nullable正常pending约束。
- dryrun每目标自身Reason/Detail，不能把 `Pruned[0]` 理由兜给全部targets；mixedparam/schema/missing准确区分。逐目标reason返回和aggregate两边必须一致。

## R1-C4 · 部署intent与回执

消费S1-D7原CreateDeployments同tx全Node namespace权限，以及 `MarkDeployResultAuthorized`当前credential/ns/intent/node/request/digest绑定CAS。producer不在store拒绝后发送任何frame，混合法/非法target0writes/0frames，offline已授权可pending并仅explicitdeployreplay，不reconcile/install自动deploy。

receiver共享DTO validation后交Authorized tx，不precheck+裸Mark。wrongrequest/digest/unknownns/rotatedtoken/supersededintent不写，精确重复幂等；新增actualWS负例及正确部署roundtrip。无旧wire aliases。

## R1-C5 · 日志/回执有界关联

- waiter只接受真实requestId/taskId/Node身份和实际attempt；成功回实际requestattempt，error依共享protocol错误语义不伪attempt（不得以“error非空”接受声称错误attempt的帧）；阴性：未登记request、wrongtask/attempt/node、noarchive、timeout、disconnect唤醒与重复帧。
- 读循环不向cap1 waiter重复阻塞send。用准确一次交付/removal或nonblocking有界机制，清理超时/断连/重复reply不阻塞其他frame；不能loop/poll无限等。detach/rehello真实生命周期保busy、不假slotrelease。
- Node-log R9 shared snapshot验收集成后传tail/since/attempt、严格validate真实lines/error/完整JSON。中枢只转发观察sequence/采集at，不拼raw/重编号/改server时钟过滤；global current/previous历史Node、不问当前Node latest冒全局。previous无历史200attempt0由G3HTTP投影，离线/过期/不可读真实error不伪空。
- 保既有upgrade共享协议，不改升级产品或输出帧。

## R1-C6 · next引用明确裁决（补语义，不复旧v3）

父依用户已批准 `next name@version钉死` 定义裁决：不能通过裸emit `to=name` 绕过父next仅允许的 `name@version`，否则“钉死”无效。

- 父能力真实同version的当前next声明中，裸to若匹配唯一声明引用，则按该引用解析：声明name跟随latest，声明name@v用v；显式to=name@v只能匹配已声明的精确引用，不能仅因next存在裸name就允许任意指定旧/新版。非法选择沿现not_declared拒绝，不新增码或私有协议。
- 同basename多条不同钉版而裸to无法唯一选择时明确ambiguous/badrequest，不偷偷latest；不扩大原码表，新reason若现封闭码无法表示报父。
- 子Task接受时真实version/digest/params钉住；后续子Task重试不解析新版本。父同version的next是可变图配置，后续emit必须按真实当前声明授权，先前已建子Task不变；不虚称“父运行中再改next也完全不影响后续emit”。
- 补真实dispatch结果版本/digest断言，不仅declaresTarget(bool)。若既有测试只证明裸name可匹配钉版，不删除该授权断言，只补其有效目标解析用例，禁止把bool当latest版本证据。
- 这是未覆盖组合的父明确补齐，需后G3同步主文档§2.1/§4.1与Task例外；此卡已提前冻结，不私修store.ResolveRef全局latest规则，effective declaredRef在route本地选择后交现Resolver。

## R1-C7 · 有限测试/映射缺口

只修三fresh G2 findings对应有限行，不重扫整650名：
- handshake坏token、hello registeredname不符、prehello裸/错direction帧实际连接fail-closed；原tokenrotate/accept不能冒替。仅保持v4授权安全意图，不恢复旧v3“必须HTTP no101”的特定wire断言；按当前WS handshake阶段真实拒绝/关闭测试。
- deployedunknownrequest/digest/ns负例，logs timeout/noarchive/disconnect/unregistered/wrongattempt（上C4/C5）。
- params模板单趟不递归、整值类型保真，消费现sourcespec实际行为；若需改变sourcespec是protected先报父，此处只新runtime覆盖。
- 原映射指向实际存在case及本case确实断言的安全事实，原family/pending不能冒kept/PASS；report包括有限改动行/oldname→newname。G1行不改。
- F6中旧executor notify.phone中央Task成功/失败/缺to等专属断言按冻结CONTRACT刻意退役，映射须诚实标retired而非假迁移；不构成“缺替代中央能力”major。独立POST notify/设备投递/认证/broker/push真实行为不退役，由现有测试及后G3/G4独立域回归守，不恢复中央Task通知、不写push/服务器跨owner。

## 固定门禁和独立复审

canonical真实G2 WT/root/GOMOD；gofmt-l六runtime目录只读+gitdiffcheck；六包govet；六包gotestcount1；store/artifact/protocol全部gotestcount1。GOdirect/sum/auto，launchverify不减，每长命令900秒真实fullLog/exit，不用空phase/自报顶。原总goall/contract/quick与G3/G4/cross仍待，局部绿不冒整体。

新sourcefreeze后三fresh默认scout分别正确性/安全回归、有限覆盖+具名映射、简洁性，附D1-D6/A1/A3/W6/S1-D1-D7/R1-C1-C7与刻意不做；criticalmajor精确修复复审，minor可裁保。Node/protocol未验收依赖不合入活writer；parent独立机械认证使用真实路径，原错误path/变更型fmtreceipt保历史不改绿。

无push/MR/merge/rebase/reset/squash/settings/env/.git/node_modules/用户data/旧WT/session/日志清理。只本地owner提交供父，末DONE G2-R1或SAFE_BLOCKED，报告true接口/codeepoch/门禁/有限finding/未完成的G3G4。
