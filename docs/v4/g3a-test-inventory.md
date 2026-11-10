# G3a · #331 测试 inventory（server 345 + api 18）

> 口径：G3-D6 要求「先逐名 inventory，再按当前安全事实迁移」；禁止整文件清空
> 认证/权限/CAS/delete/secret/digest/FK/backpressure 断言。本文逐 Test 名给出
> kept / migrated / retired，退役项必须写**替代用例**与理由。
>
> 基线：`b30e998`（origin/v4/integration）。统计：server `345`（含 1 个 `TestMain`，
> 真实用例 344）、api `18`。

## 0. 汇总

| 状态 | server | api | 说明 |
|---|---|---|---|
| kept（原样保留） | 见 §1 | 0 | 认证/设备/静态/背压/日志安全等保留域 |
| migrated（迁移到新用例） | 见 §2 | 18 | v3 扁平资源面 → v4 namespaced 资源面 |
| retired（具名退役） | 见 §3 | 0 | v3 flat/edgeCRUD/central exec/batch 专属语义 |

