---
id: 003
kind: bug
area: [task]
size: L
severity: P2
state: needs-decision
needs_recon: false
contract: []
depends_on: []
route: [worker]
gates: ["go test ./internal/store/... ./internal/lease/... -count=1", "make contract"]
opened: 2026-11-16
recon: 2026-11-16
---

# 003 · 三条「删除/回收」原语都没有生产调用点 —— deployments 与能力 tombstone 只增不减

> 侦察已完成，全部事实带 `file:line`：见 [`003-recon-tombstone.md`](003-recon-tombstone.md)（301 行）。
> 本卡只写结论、不变量、裁决点；原文的「只修 `PurgeDeletedCapabilities`」在侦察后被修正为「三个同族缺口」。

## 结论（比原卡更大）

| 原语 | 位置 | 生产调用点 |
|---|---|---|
| `PurgeDeletedCapabilities` | `internal/store/retention.go:317` | **无**（只有 `v4test/*`） |
| `DeleteExpiredPlaybookInstalls` | `internal/store/retention.go:246` | **无**（只有测试） |
| `DeleteDeployment` | `internal/store/deployment.go:444` | **无**（全仓只有定义） |

三层后果：

1. `capability_versions` 的 tombstone 永不被物理回收；
2. `playbook_installs` 回执永不回收 → purge 判据 5（`installed_from`）永久保护这些能力的 tombstone；
3. `deployments` 行永不删（只被 supersede）→ purge 判据 4 永久保护**部署过**的能力。

**实测（本机遗留 v4 开发库 10 行样本）**：唯一的 tombstone 正是被 `deployments` 挡住的。
⇒ 只把 purge 接上线，收益仅限「从未部署、从未跑过、从未被安装」的能力。这条必须先讲清，
否则会以为「接上就解决了」。

## 量化

- `capability_versions` 12 列 / 7 个 TEXT；软删**保留整份 spec**（含 `run.files` 脚本）。
  实测平均 **1.1 KB/行**（min 258 / avg 891 / max 2042 字节；样本只有 10 行，非权威容量数字）。
- 删除一次能力 = 把该 name 的**所有 live 版本原地打 `deleted_at`**，不新增行。
- 但 partial 唯一索引只约束 live 行，且布局校验**明确拒绝**「tombstone 唯一索引」（`v4test/format_test.go:1404`）
  ⇒ `删除 → 同名重建 → 再删除` 会留下**同名同版多条 tombstone**，各占一份完整 spec。

## 一个对外语义会被 purge 改掉（必须在卡里钉住）

`DELETE /v1/namespaces/{ns}`（非 force）的阻塞计数 `capabilities` 数的是
`capability_versions` **全部行**（`internal/store/namespace.go:420`，**没有** `deleted_at` 过滤）：

```sql
SELECT count(*) FROM capability_versions WHERE namespace_id = ?
```

所以「已删能力仍会挡住命名空间删除」是现状；purge 之后同一请求会从 **409 变 200**。
这一条必须在测试里显式覆盖，不能当成附带效果。

## 不能被破坏的不变量

- **判据 1（tasks）有 FK `ON DELETE RESTRICT` 兜底** → 漏判会硬报错（`schema.sql:377`）；
  **判据 4（deployments）是 `ON DELETE CASCADE`** → 漏判会**静默删掉部署行**（`schema.sql:539`），
  节点侧表现为「内容突然不推了」。两者失效模式不对称，测试要分开设计。
- 判据 2/3（`task_attempts`、`task_artifact_manifests`）**没有外键**，全靠那条 SQL 自己（`schema.sql:501`、`:644`）。
- 三处「能力行缺失时**静默降级**」：`lifecycleOn`（`task.go:1356`）、`maxPerNodeOn`（`execution_auth.go:115`）、
  `taskLifecycle`（`task.go:748`）。放宽判据不会报错，只会把幂等/预算悄悄换成默认值 —— 最难查的一类回归。
- 判据 5 的 `json_tree` 分支在真实数据上**恒不命中**（`playbook_installs.receipt` 里是 `name@version`，不含 UID）；
  真正的保护是 `installed_from`。

## 性能陷阱（挂错地方就会抢写锁）

实测 `EXPLAIN QUERY PLAN`：`capability_versions` **全扫**（没有以 `deleted_at` 起头的索引）+
`tasks` / `task_attempts` / `task_artifact_manifests` 三次**相关全扫**（`capability_id` 无索引）+
`json_tree` 虚拟表 + `USE TEMP B-TREE FOR ORDER BY`，整段在 `BEGIN IMMEDIATE` 写事务里。
挂进 `RunRetention`（30s 一轮）= 每 30s 在写锁内做 4 次全扫，与事件入口抢锁。

## 需要 owner 裁决（4 条，卡在这一步）

| # | 分歧 | 选项 A | 选项 B |
|---|---|---|---|
| 1 | **时间窗** | 无时间窗（现状语义：无引用即删）—— 零改动，但事故现场没有恢复窗口 | 引入 `capabilityTtlSec`：改 `PurgeDeletedCapabilities` 签名 + `system_config` 加列（老库 `ErrSchemaInvalid`）+ `api/resource.yaml` |
| 2 | **挂在哪个循环** | `RunRetention` 内部（零收尾负担，但 30s 全扫写锁） | server 独立循环，抄 `artifact-gc`（`mux.go:296-341`，可独立间隔/env、可 `<=0` 关闭） |
| 3 | **判据是否放宽** | 只接现成 primitive（绝对安全，收益小） | 同时让 deployments/回执随能力回收（改部署同步语义，`DeploymentsForNode` 会少推） |
| 4 | **要不要含退役行的历史投影** | 不做（`capability.md:844`「逻辑删除 ≠ 事实删除」） | 做（回收窗口变对外承诺，必须先写进 `api-conventions`） |

## 验收草案（裁决后定稿）

- 接上之后**真的会删**：过一段时间（或 `RunOnce` 一轮）后行数下降，且被引用的不删。
- 判据 4 漏判的失效模式单独测：`deployments` 不出现静默级联删除。
- `DELETE /v1/namespaces/{ns}` 的语义变化有测试（tombstone 曾在 → 409；purge 后 → 200）。
- 统计要能区分「跳过」与「扫了没删」（沿用 `EventsSkipped` / `Truncated` 的既有语义）。
- `make contract` 绿；若动了 schema，按 `internal/store/AGENTS.md` §3.2（当前只支持单块）。

## 不做

- 不改成硬删：`deleted_at` 是对外语义的一部分（删除后 `/versions` 恒 404 靠它）。
- 不在本卡里动 `api/*.yaml`（除非裁决选了 1-B 或 4-B）。
