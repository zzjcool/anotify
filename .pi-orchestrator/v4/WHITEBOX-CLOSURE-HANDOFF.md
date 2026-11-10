# G1 原测试闭包：有证据的方法改变批准

固定ad8d21c的真实门禁为format0、完整store/auth/push vet1、test1；不是已有G1白盒PASS。两vet/一test重复暴露旧ArtifactBuilder/PlaybookDTO/AppliedRef/Lifecycle等10个编译错误并被too-many-errors截断，不能据此推定仅10处。回滚的是未编译/未执行测试草稿，生产与已过C3成果不回滚。

父批准NEXT-G1TEST-FIXTURE-CLOSURE的**真实v4 fixture-first改法**，不是改名后第三次无修改retry。已有失败日志/止损计数保留，不重复原baseline门禁直到已知fixture/API错误真实迁移。

## 文件所有权

- 同WT唯一fresh writer，原 `internal/store/*_test.go`、`internal/store/v4test/*_test.go` 及需要的该测试目录子文件。
- 允许明确新增 `internal/store/v4_fixture_test.go`（store同包共享builder）与 `internal/store/v4test/fixture_test.go`（外部测试包自己的builder）；跨包需各自实际Go可见性适配，不能假设_test.go可从别包import。
- 已有 `v4_whitebox_test.go` 与迁移测试草稿仅在fixture编译闭合后重新纳入并实际执行；不是保留一份未运行草稿叫覆盖。
- TEST-MAPPING逐案记录原460库存去向，不再复制一份全pending清单；已有认证checkpoint不重写。
- 禁止生产、schema、artifact/protocol、cmdanode、auth安全生产与其他团队路径修改；遇真实产品bug给具体case/产品行/红输出，父再明确裁决。

## 实施与验收

1. 首卡A：真实Spec/version、raw playbook/files、AppliedCapability/部署、NodeAuthorization/assignment-slot/currentlease、Source/Event/retention/stats等共享fixture及所有受其影响的原包测试调用。不能通过定义已删除类型的legacy alias或生产兼容入口让旧测试编译。
2. 首卡完整目标是**全部原store包编译闭合**，单族迁移成功或外部v4test数量绿不等首卡完成。可以有界编译诊断暴露后续错误，不反复把相同too-many-errors输出当进展；同族两次有意义新改法仍失败需精确止损。
3. 然后B：真正白盒迁移/事务/池/IDs/统计/retention/字段往返和注释中说的安全不变量恢复。初始空库拒绝测试不能冒充midtxn rollback；正常错误/rollback要从实际执行路径构造。
4. 最终仍完整三机械gate：格式检查、完整store/auth/push vet、同范围go test count1（固定命令显式正确WT、日志与真实exit）。每worker都应有mechanical verify数组；若按A/B分片，A的明确编译gate与B的完整门禁分别声明，不用阶段格式绿伪完整tests PASS。
5. 原460逐案retained/new/retired必须有具体新用例或已批准纯v3语义理由。不能按builtin文件名删除内含的认证/注册原子默认NS/恢复/安全不变量，不能skip、空体、弱断言或mock授权绕路。新v4权限/事实/引用/边界实际fixture需调用现真实Store安全entry。

源C1/C2/C3a/C3b与Node支持已通过的内容不重做，当前G1原包未绿照写；A/B闭包与fresh覆盖门槛后立刻G2runtime/G3HTTP5spec/G4全Go-contract-quick，完整范围不减。无machine不等旧phase；无push/merge/MR/清理旧WT。
