# Namespace force 删除与未回收执行 · C3b 精确裁决

现规范保留 Namespace 默认非空拒绝、force 显式清空、不能删最后一个 Namespace；不引 finalizer/reconciler。固定9e18f59实际 `namespace.go:348–365` 删除全部相关行，但无执行回收门槛。

## force 不是撤回已运行进程的证明

- 默认删除仍按既有非空检查拒绝。
- force 在**同一删除事务**中，先验证当前 namespace UID/CAS，然后确认没有任何已授 lease/attempt 未物理回收的执行，以及绑定该 namespace 的实际 busy/未知占用账本。终态 phase、网络离线、Node 重启或 limits 缩容不等于回收。
- 若存在此类占用，返回现有 HTTP409 `Conflict` 和明确人读说明，整次0写。管理员需先显式 cancel/abandon 等既有动作并等待可靠的进程组回收/历史reapedACK，再重试 force。不能自动后台取消后即删，也不能加公共 deletionTimestamp/finalizer、异步删除状态或让 force 悄悄变默认空库检查。
- 尚未授租、仅 pending 的任务/assignment可以随成功 force 删除；随后旧 accept/grant/result 必须在实际安全事务中拒绝，不把消失的任务重新创建。已grant但未spawn同样属于授权未reaped，不能用没见running阶段跳过检查。

## 成功 force 的明确范围

- 没有上述未回收执行时，force仍可按既有规则显式清理本namespace任务/历史/事件/源/包回执/部署/版本/配置/边，再删namespace。其显式用户删除不同于日常TTL，不能将C2普通Capability删除的tombstone语义误推为Namespace必须永久保留全部历史。
- 删除按不可复用ID，不能从旧名字请求删新namespace。Node全局登记行不删除，Node配置中的授权/ns申报和实际下次任务分配需按现session授权再验，不能靠旧缓存继续授单。
- 不得级联销毁其他namespace仍保留的真实引用，也不得直接按digest/路径删除共享能力内容或用户workDir。物理capability版本清除前检查所有剩余引用；运行产物/内容文件由对应引用安全GC处理，当前事务不能跨网络/长文件扫描持DB写锁。

## 精确所有权与验收

批准C3b worker卡加入 `internal/store/namespace.go` **仅 DeleteNamespace/DeleteNamespaceByName 与 deleteNamespaceOn 引用保护闭包**及相应v4test；必要事务引用query在既有store文件。当前C3a-wire-close先safe收口，不能并发修改。

必测：force对已grant/未spawn、terminal但未reaped、真实busy/未知ledger均409且全库相关事实0变；可信回收后force可成功且Node登记保留；旧UID/CAS不能删除同名重建；待授租任务安全清除与迟到grant拒绝；外部shared digest/其他namespace数据不被删。按现实际schema记录判定，不能因没有FK而省验证或新增手写wire字段。

这是现force语义的同步安全前置，不是用户Capability DELETE新增in_use，也不是完整异步控制平面。原C3b typedmanifest/refGC和全Go/contract/quick/finalcross不削。
