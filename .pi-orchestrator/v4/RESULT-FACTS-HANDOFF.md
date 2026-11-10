# C3b 已有执行结果事实字段：精确模型补齐

共享protocol33e的TaskResultFrame已经含ExitCode、StartedAt、FinishedAt。store TaskResult缺这些字段是内部模型缺口，不修改wire、不要求新权限或明文token。

## 精确授权

- `internal/store/task.go` 的TaskResult增加 `ExitCode *int`、`StartedAt/FinishedAt time.Time`，JSON字段camelCase，与现共享帧逐项映射。新增仅内部真实结果字段，不复制新frame或让store依赖broker。
- CompleteTaskAuthorized及完整result_json持久化/读取/重复比较、Task/attempt对应事实投影与必要schema/retention/往返测试纳入当前C3b卡。结果比较不能只取原Phase/Reason/Error/Artifacts部分忽略exit/时间。
- ExitCode的nil与0必须区别；before-spawn失败可无实际StartedAt/ExitCode，不为了填字段捏造进程开始。已存在正常脚本timeout/非零/成功和prerequisite失败生产形状保持。
- 完整JSON保留时间精度。等时刻可UTC规范化并剥monotonic做语义比较，不用Unix秒截断比较后宣称不同结果相等；Task/attempt索引便利列若秒精度，不作为完整事实判等源。

## serverNow 与 Node 事实绝不混用

- 所有当前凭证、lease expiry/cancel、artifact registration、quota/refGC授权仍用事务服务器now。Node reported FinishedAt不得充当鉴权now或延租依据。
- Task/attempt的进程finish/exit展示存实际提交事实，不用上传结束/接收时刻覆盖。可单独保留既有服务器接收/完成观察时间；若无合适可信时间列，可增加内部result_accepted_at用于首次接收/保留计算，不投影新公共字段。
- TTL/回收用既定可信服务器时钟和活动/未reaped保护，不让Node报告很旧/很未来时间造成提前删除、续租或跳过安全检查。保存实际报告时间与可信观察时间并存，而非把一个伪装成另一个。
- 结果时间基本一致性（两者存在时start<=finish）可验证；不要引未经冻结的硬coded机器时钟误差窗或要求before-spawn失败有伪造时间。

## 幂等确认

相同当前Node身份及task/attempt/lease、完整Phase/Reason/Summary/Error/typedmanifest/nullableExitCode/精确StartedAt/FinishedAt事实相等，才无写返回幂等ACK。任意exit/time差异必须冲突且0写，不覆盖旧row/首result_accepted_at，也不影响较新attempt。

必测：新增字段完整往返/纳秒与nil0判别；Node FinishedAt早于上传完成但存原值；用很早reportedFinishedAt仍不能以此绕服务器已过期lease；重复相同结果整行不变/不同exit或任一时间精度冲突；活动结果与30天事实、7天bytes回收规则不混。

当前C3b原writer已safe停止并保5e2331b源码/红绿。扩任务卡后同writer若可实际continue就单writer续修；若已退役不可恢复则确认后fresh接成果，不并发/不重做C3a。原4真实extensionverify及3fresh门槛后继续白盒/G2G3G4，selfPASS或okfalse不伪verified。
