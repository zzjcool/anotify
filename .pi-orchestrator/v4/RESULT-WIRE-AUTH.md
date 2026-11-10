# Task.result 现有 wire 的权威校验与 C3a 反例（父裁决）

## 结果无需新增 attemptToken 字段

protocol33e的task.result已经携taskId/attempt/node/leaseId和结果，但不携明文attemptToken。**保持此wire不变**；C3 CompleteTaskAuthorized接口不得要求caller制造/取回一个未传输的token。

- 使用认证NodeSession提供的 NodeAuthorization{NodeID,NodeTokenHash}，hash必须来自该连接实际认证凭证，不是处理结果时从数据库取当前hash填给自己通过校验。
- 同事务验证数据库当前Node身份仍有效/hash未轮换、task属于该node与授权ns、精确current attempt/leaseId/lease未过期/cancel状态，再写合法结果与事实。正文node只是登记name，不能作为caller自由选择数据库身份。
- leaseId是中枢授予当前attempt的不可复用绑定，结合当前认证Node身份和历史/当前attempt事实足以校验结果；不缓存/持久化grant明文secret作为结果鉴权捷径，不把库里storedTokenHash当客户端出示凭证。
- Upload与emit已传的attemptToken仍按它们的既定通道校验，不能因result无该字段而撤其限制；Renew按现共享字段和Node认证校验，不新增wire。
- 结果重发的已有幂等ACK只可针对相同已记录事实且不更新任何新attempt；旧/错结果不得改变新授权。物理reapedACK按当前Node身份+历史task/attempt/lease/slot可安全回收，非重新授权结果。

## 现 wire 事实映射

store 结果模型必须接收共享帧已经存在的 nullable ExitCode 与实际 StartedAt/FinishedAt，完整持久化/判等。鉴权 serverNow 与Node进程时间独立，不用上传完成时刻覆盖原事实，也不用报出的时间绕过expiry；精确owner和时间/TTL边界见 `RESULT-FACTS-HANDOFF.md`。

## C3a 总预算必须有可判别反例

不能用total=1却传slot1的越界请求证明total预算事务正确。

- 单预算判别：初始total2授任务A到slot1，缩total1仍保旧live A；slot0仍是当前合法空槽，B申请slot0应被**总占用**拒，而不是slot范围拒。
- 并发事务判别：初始total3有旧live slot2，缩total2；两个新任务并发申请合法slot0、slot1，其他预算放宽，最多一个成功。即使每个请求单看范围都合法，事务计数也不能超过total2。
- own reservation仅排除精确本次node+slot+assignment绑定，不能按logicaltaskId排除旧attempt或另slot真实占用。其他reservation及已授未reaped必须计入，不能通过同task复用名字绕预算。
- 三预算仍total、跨namespace同名perCapability/*、同ns+name跨versions maxPerNode；不新增quota/路径锁。

## 历史回收幂等

重复reapedACK不得重写execution error/phase/finishedAt或ReapedAt，不影响当前新slot/lease；初次回收与执行结局分开。测试保留历史整行在重复ACK前后相等的判别，不只assert槽为空。

当前C3a单writer按精准卡修这些已识别项，源码与旧结果证据保留；原阶段门禁和fresh限域复审清major，C3b/旧测试/G2G3G4和最终全Go/contract/quick不削。
