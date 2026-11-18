---
id: 004
kind: docs
area: [contract]
size: S
severity: P2
state: done
needs_recon: false
contract: [api-yaml]
depends_on: []
route: [scout]
gates: ["make contract"]
opened: 2026-11-16
---

# 004 · 契约缺口：没有定义「显式重跑」的谱系（V4-D4 只覆盖自动重试）

> 这张卡**不改代码**，是给 contract owner 的裁决项。来源：一次只读干跑（dry-run）里，
> worker 被要求「加一列记录本任务是哪个任务的重跑」时，主动上报与 V4-D4 冲突并拒绝自行扩展语义。

## 契约现状

`docs/design/v4/decisions.md:130`（V4-D4）：

> 重试是**同一逻辑任务（同一 task 行）**，`attempt` 只是计数器；不新建 task_id，
> 也不把 attempt 关系伪装成 `parentTaskId`（那是业务 emit 的父子关系）。

即：**自动重试**（幂等任务遇到短暂基础设施失败）不产生新行。而 `parent_task_id`
（`internal/store/schema.sql:374`，索引 `:446`）已被占用为「业务 emit 的父子关系」，
`schema.sql:462` 的注释也写明它不代表执行关系。

## 缺口

产品另一个动作没有定义：**显式重跑 / 手动重触发**（人看到失败后，让它再跑一次）。

- 它必然产生**新 task 行**（V4-D4 只覆盖自动重试）；
- 但与原始任务之间**没有任何契约化的关联** —— 既不能用 `parent_task_id`（已被 emit 语义占用），
  也没有别的字段；
- 手动触发入口 `POST /v1/namespaces/{ns}/sources/{name}/fire` 会生成新事件/新 trace，
  表面上「重跑」等于「再触发一次」，于是**执行历史在 trace 层面断开**：
  看不出「这次是上次失败的重跑」，也做不出「一个能力连续失败 N 次」的统计。

## 要裁决的三选一

| 选项 | 含义 | 代价 |
|---|---|---|
| **A. 明确不做** | 重跑 = 重新触发，trace 之间不建关联；把这条写进 `decisions.md`「刻意不做」 | 0；但前端/用户永远看不到「重跑链」 |
| **B. 复用 trace** | 手动重触发沿用原 `traceId`，用 trace 表达「同一次业务意图的多次尝试」 | 需定义 trace 生命周期与去重窗口，会影响 `source` 的 `dedupe` 语义 |
| **C. 新字段** | 加 `tasks.retry_of`（或 `rerun_of`），语义**仅限**显式重跑；自动重试仍走 attempt | 需改 schema（注意 `internal/store/AGENTS.md` §3.2：当前引擎只支持单块，加列 = 改 baseline）+ api yaml + 前端 |

## 无论选哪个，都必须做

- 在 `docs/design/v4/decisions.md` 补一条显式记录（**不要**让实现去猜，也不要靠 `parent_task_id` 暗示）。
- 若选 C：往返单测 + `schema.sql` 注释写清「自动重试恒为空」，并显式决定
  `tasks_unresolved_failure_shape` 这条 CHECK 要不要覆盖新列。

## 不做

- 本卡不产生代码改动；`route: [scout]` 只用于裁决前的现状取证。
- 不要为了让某个 UI 需求好看而先加列后补契约（这正是 dry-run 里被判为越界的做法）。

## 裁决（协调者代行，2026-11-16）：选 A —— 明确不做，并写入契约

已写入 `docs/design/v4/decisions.md` 的「刻意不做」节：

- 重跑 = 重新触发一次（新事件 / 新 trace），不新增 `retryOf` / `rerunOf` 类字段；
- `tasks.parentTaskId` 继续只表达业务 emit 的父子关系，**不**承载手动重跑关系；
- 不拿 `attempt` 冒充重跑谱系（那会让「同一逻辑任务的第 N 次尝试」与「人手动再跑一次」混成一个口径）。

**代价（已知并接受）**：前端/用户看不到「重跑链」，也做不出「同一能力连续失败 N 次」的统计
（那需要先定 trace 生命周期与去重窗口，属于另一次契约变更）。

**如果将来要做**：重新开卡，按「选项 B 复用 trace」优先评估（不改 schema），
只有 B 不可行时才考虑 C（加列，需动 baseline）。
