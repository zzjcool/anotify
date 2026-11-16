---
id: 010
kind: feat
area: [contract]
size: M
severity: P2
state: needs-decision
needs_recon: false
contract: [api-yaml]
depends_on: [008]
route: [worker]
gates: ["make contract", "go test ./internal/api/... ./internal/server/... -count=1"]
opened: 2026-11-16
---

# 010 · 写事务撞 BUSY 时仍是 500 —— 要变成 503 需先改契约定

> 008 的 Phase 2。008 只消灭了「读也抢写锁」那一半（ingest 写锁次数 3 → 1）；
> 剩下那一半是**写事务本身**在极端竞争下等满 `busy_timeout(5000ms)`，
> 现在仍被 ingress 翻成 **500**（服务端错误）→ 调用方按语义不会重试 → 事件丢。

## 为什么不能「加个 case」了事

现状的事实（008 侦察第 5 节，均有 `file:line`）：

1. `api/ingress.yaml:353-354` 为 `POST …/events` 声明的 503 **只有一种**：`Backpressure503`；
2. 它的 body 被 `api/resource.yaml:1508-1523` 冻结成 snake_case `BackpressureError`
   （`error` / `pending_age_sec` / `max_pending_age_sec` / `retry_after_sec` 四个 required 键）；
3. `scripts/e2e/suites/capacity.mjs:197` **硬断言** `body.error === "backpressure"`；
4. `docs/design/v4/api-conventions.md:247-260` 的 `status.reason` 是**封闭集合**，
   没有「暂时不可用」类词，HTTP 码表也只列到 500。

所以「busy → 503」不是代码问题，是**契约问题**：要么复用 `Backpressure503`（语义不符：
背压是主动拒绝，busy 是被动超时），要么新增一个 reason + schema 并同步三处门禁。

## 三条候选（需 owner 裁决）

| 选项 | 做法 | 代价 |
|---|---|---|
| **A. 复用 Backpressure503** | busy 也回 `503` + `Retry-After`，body 仍用 `BackpressureError`（`pending_age_sec` 填 0 或省略 → 需放宽 required） | 最小；但把「主动拒绝」和「被动超时」压成同一个 reason，运维无法区分 |
| **B. 新增 `ServiceUnavailable`** | 新 reason + 新 schema（`UnavailableError`，至少 `retry_after_sec`）+ `api-conventions.md` 的封闭集合 + parity 门禁 | 契约改动完整、语义清晰；要动 4 个文件 + e2e 快照 |
| **C. 不改契约，改成 500 + `Retry-After` 头** | 仅加头 | 违反 HTTP 语义（5xx 里只有 503 承诺可重试），且调用方 SDK 通常只对 503 重试 |

建议 **B**（语义正确、对调用方契约明确），但它是契约变更，需要 owner 拍板。

## 顺带（同一形状，建议同批）

`AcceptSourceEvent` 的**幂等命中**路径现在会 `BEGIN IMMEDIATE` 拿一次写锁再 `Rollback`
（008 报告 §5.3）。它也是一次「为了读而拿写锁」，与本卡同源，建议一起看。

## 验收

- busy 时回 503（或裁决选定的码）+ `Retry-After`，且**有测试**（并发写压住 + 该写路径）。
- 契约三处同步：`api/*.yaml`、`api-conventions.md` 的 reason 集合、`scripts/e2e/suites/capacity.mjs` 的断言。
- `make contract` 绿。

## 不做

- 不调大 `busy_timeout`（那是把 5s 变 10s）。
- 不动 `_txlock=immediate`（008 已证明三处测试 + 两条业务语义依赖它）。
