# S1 store 实施 + 三角review收口（父独立机械门禁仍在执行）

源码：`5dc3f27`；最终docs交付：`ae3ace904d4254c963cd4dd493b72bb2622a7138`（e815/d6fa/ae3只checkpoint/verdict，不改Go）。WT为既有G2隔离树，branch52fc54d4，parent已实核clean/sourceeq，原writerretired且launch固定5项actualaccepted(verified)。

## 已实现

S1-D1未解析failedTask strict NULL ref例外/普通Task真pin+FK、可见性/roundtrip/noattempt；D2 single-tx SourceEvent/window/fanout/failure/prune/持久receipt；D3现EmitAuthorized同事务附facts/授权先于replay；D4 Propose stable pending / Reject current credential/ns/target-binding事务授权；D7显式部署全Node namespace权限同txn + MarkDeployResultAuthorized intentCAS。代码21文件、28top tests；相应五门禁原自测/workerfixedlaunch实际全部0。

三fresh默认scout atomic-authority/schema-task/tests-scope 均ok:true、0critical/major，精确有限证据已collected，不以章节数/计数代行为审查。真SQL六trigger故障all0writes、并发delivery原receipt、SourceCAS/ABA/pin、无delivery/0fanout/NULLwrongshape/FK、authbeforeemit/prune失败、proposal/reject/deploywrongbinding阴性与normaloffline回执都有覆盖。fixture改动仅真实权限/正常versiondigestsetup与CHECK引用列无法DROP时RENAME损坏布局，不改拒绝断言迁就。

## 接受minor与刻意边界

1. PruneFacts闭集在runtime输入必须先验，不允许`downstream_missing`混pruned；当前store Emit计划prune vs AcceptSourceEvent防御不完全一致，已record为store防御minor，后G2-R1必须使用typed闭集与真实failedfact，不借此宣许可任意code。
2. deliveryId是幂等接收键，duplicate按冻结返回第一次receipt、不是emitId的body-conflict规则；不同payload重投不新增Task。源CAS/namespacepin变动仍先拒，不能越rev重放。此为明确设计，不额外改Source去重兼容语义。
3. 窗口活得比事件长时可能明确receipt-missing失败，不伪造空成功；后G3/G4 retention集成测试应验证生命周期，当前不新增运维自动删除用户数据。0forever不为绕过receipt失败而重占窗口。
4. NULL失败CHECK仍有未涵盖的可选结果/摘要/配置列、SQL NULL行负version仅Go入口拒等低风险；正常入口与唯一failure构造都保持真实值，无Node原result事实。后G3wire投影不可冒C3process事实。
5. strictOpen现比较cols/PK/FK/unique，不比较SQLite CHECK表达式，非本次弱化；新增CHECK断言在真实新库运行，入口再守normalTaskpin。相同column但手工去CHECK库鉴别覆盖是现strictlayout局限，保风险，不假称全SQL字面完全同构。
6. 源并发测试2goroutine没有强制interleaving、ctxcancel/noidprunefailure有有限覆盖缺口，store/race全包门禁仍守；不在此无限补全原650库存。四种写原子故障/约束行为已真触发。
7. 旧runtime还用裸AssignTask/RejectAssignment/MarkDeployResult/InsertEvent，是**后续G2-R1必须串行替换**的活路径，不算S1已打通runtime。独立G4 notify/broker/push域不退役，旧centralnotify.phone是刻意退役不恢复executor。
8. 原自测race长前台缺独立fulltee收据是操作风险；父另外托管固定once五项留真实命令/stdout/exit/HEAD sourceeq，不能拿mdPASS顶父机械结果。

## 父独立机械验收（本文件只记录已启动）

`/tmp/anotify-v4-s1-parent/run-gate.sh` 1～5，carrier `s1-parent-gate-carrier`，immutable ae3/source5dc，格式只读、storevet、storecount1、storeracecount1、六runtime/artifact/protocolcount1。每900秒，有完整tee+exactcommand+statusJSON，重复log拒。**尚未全收齐之前不宣S1父通过，不集成Node、不派runtimewriter**。

Node日志4c已经独立门禁/三fresh收口（L1-NODE-LOGS-CLOSURE.md）。父准备只读精确产品patch：`/tmp/anotify-v4-node-logs-handoff/node-log-products.patch`，SHA256 `ddeb2a73939065437cbac828bdc70581ac75f5ddf3c571666b8ce0d6ab2062d8`，仅5a..4c的11个Node/protocol产品源码与测试；`git apply --check`在G2 immutableae3成功，**尚未apply或commit**。S1独立五项全0且无livewriter/command之后才父亲自集成，不在gate期间改shared dependency/混epoch。

G2-RUNTIME-REPAIR-HANDOFF.md是后续有限consumer返修授权，必须消费actualS1接口/Node唯一sharedDTO，criticalcfgRevision错误优先。G3/G4/原goall/contract/quick/CI/build/fullE2E和实际API/前端cross仍待，不把S1+Node局部绿冒整体v4完成。
