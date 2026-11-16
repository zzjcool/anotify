# G1 最后有限审查返修（父批准）

固定5debf01已父独立格式/vet/五树运行测试全部通过，但审查门槛仍有缺口；机械通过不能替代原场景语义覆盖。名称一致性无major，460唯一原案例/78补漏均有具体去向，映射4行仍引用错误证据。

## 唯一 tests-only writer 范围

1. 四行映射引用真实原子零写、并发引用、Source实际UPDATE投影案例，不以读取Ref/非法脚本/顺序执行代替。已有正确测试可精确引用，不存在则补最小真实反例。
2. 有效认证/currentlease下非法result.Phase零写，比较完整Task/attempt、node_instances精确行集和实际产物登记/manifest，不只busyCOUNT。相同计数的行内容/绑定变化也必须检测；不靠坏token提前拒绝。
3. 基线索引独立验证当前v4查询、有序列/必要谓词和有效前缀，不count-only或旧v3索引；EXPLAIN不绑定无关可变完整计划文本。

源码、schema、C3、Node和已修Source名称只读，不增加兼容入口/skip/弱断言。定点功能变异若使用，必须保存前后SHA并精确恢复，最终无生产diff。

## Minor处理

包锚点指真实rawfixture/action；无Reason fallback按RESULT-ERROR-PROJECTION验证展示投影与rawJSON分离；取消BareBeginTx病例名不得声称midtxnrollback，真正DDL/ledger/hookrollback已有测试必须保留。

## 机械与复审

仍原完整三命令：格式、store/auth/push/sourcespec/playbook全树vet、同范围count1测试。新红绿、原用例保留、固定SOURCE及真实exit落盘；然后fresh仅六个有限finding复审，不能因Name通过或已零pending映射称G1通过。

清零后立即G2真实中枢runtime→G3HTTP及五spec→G4工具链/全Go-contract-e2equick。当前manager实际156K尚正常，不因历史248K强制重建；若后续安全交接只接剩G2-G4，已收口G1不重做。全产品真实Core/Node/前端CI-build-fullE2E门禁不削。
