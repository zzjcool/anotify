# A 编译后 B 运行红测：有限契约对账

当前A格式与完整store编译-only已阶段verified，B实际原包运行诊断31红不等白盒完成。以下父只读对账针对A四个疑似产品判断，不把旧fixture差异当产品bug。

- **CreateCapabilityForce**：删除为已批准C2语义。G3迁旧server调用，不恢复生产兼容方法。
- **旧保留名单**：原TestPlaybookManifestReservedOriginNames禁止的是Capability github/cron/http/manual；这是v3规则，v4名字约束不保留该名单。用真实rawPlaybook/Spec fixture证明允许，不能再添加旧限制。
- **Source `route`**：现directCreate明确拒、installSource没有同校验，Event入口又拒；Playbook文档要求同校验。为静态确认的差异，B先执行真实materialized Source.name==route的raw包反例并与direct比较，若成功越过现规则需父明确product修；不能通过退休原安全断言或把CAP四名禁掉来遮。
- **显式新版本同正文**：文档已明确created，不能跨版复用；同版同正文reuse、异正文immutable400。fold_key只是普通索引而非unique，所报fold唯一冲突未证。测试须新对象/ID空、相同内容version1/2，失败按真实DDL/输入/错误报告。
- **空Source targets**：source.md明确接受事件、无派发、留no_target。当前两入口targets空循环校验允许；旧必须有出口规则可具名退役，不改空目标为非空规避测试。

## 哈希精确规则（纠正摘要误推）

父亲读capability.md§3：FoldKey排除metadata.version/next/displayName；部署digest**包含content metadata.version**及规范正文，排next/displayName。row v1/v2部署digest不同是设计，不是额外私有identity框架。不能将压缩摘要的『version外digest』泛化为全部hash都相等；明确函数/用途后再判。resourceVersion是UID+内部写修订CAS，三者互不冒充。某只读scout推『Node内容digest应无v』没有找到该函数，不能覆盖主文档。

B继续tests-only完整store/auth/push+真实midtxn rollback/pool/retention等，生产更改须具体新红证据/行后父批；31旧运行红逐个v4真实fixture迁，安全原scenario保留，不能只改断言。G2G3G4及最终全Go/contract/quick不削。
