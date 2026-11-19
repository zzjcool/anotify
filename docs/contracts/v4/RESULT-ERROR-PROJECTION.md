# B 中结果 Error 的旧断言精确映射（不改 C3 生产）

> 迁移自 .pi-orchestrator/v4/RESULT-ERROR-PROJECTION.md（2026-11-19，协调者工作区不再入库）

父限域核现生产（658d05/C3同字节）：Complete仍调用现normalizeTaskError，逻辑Task.error按Error→Reason→失败/放弃Summary→Phase选择；但完整ResultJSON保留提交字段，幂等判等使用完整JSON。成功时非空incoming Error不会被偷偷抹掉。旧入口退役不代表此现有投影函数已经删除。

## B 测试校准

- 允许按**当前既有逻辑投影**验证失败空Error的fallback，同时必须验证原始ResultJSON中Error仍准确为空、Reason/Summary/Phase完整保留。不能让投影反写原始提交事实或做同类结果判等归一。
- 成功Error与Reason为空时，现Task.error应为空，不能残留先前错误；非空incoming Error按现实现保留，不为了旧『成功清error』措辞强制清除真正提交值。若将来共享wire强验证矛盾组合，是G2专门规范/校验，不在B私改生产。
- before-spawn无ExitCode/time等已有允许规则不变；reported时间与服务器授权时钟仍分离，其他完整事实字段不因projection改变。
- 新增有效授权、正确lease/attempt基础上的非法result.Phase零写反例：应触及phase guard，而不是故意坏token/slot导致前置拒绝掩掉phase验证；整Task/attempt/ledger/result/registry等相关事实不变。
- TEST-MAPPING原FinishTaskWritesOutcome仍保完整正/负结果原意，区分历史中央入口与当前授权Entry，不退休整个outcome测试；旧失败/成功投影断言分别指到真实B新case。引用现auth注册rollback490–533等已存在的安全用例，不重写或删原认证安全测试。

这是两个minor缺测在B前置补齐的明确规则，不新增生产兼容入口、不重开C3/新payload/ResultVersion字段。完整raw Result事实不可被Error投影normalize改变。限域审查若仅minor可父接受明确B跟踪后推进，不因scout JSON false且无major就无限暂停编译已闭合的A；完整运行门禁仍必须真实全绿。
