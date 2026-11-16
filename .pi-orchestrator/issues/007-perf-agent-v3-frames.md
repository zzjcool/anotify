---
id: 007
kind: bug
area: [e2e]
size: M
severity: P1
state: open
needs_recon: false
contract: []
depends_on: [006]
route: [worker]
gates: ["bash -n scripts/perf/c/agent.mjs", "bash scripts/perf/c/s1-soak.sh"]
opened: 2026-11-16
---

# 007 · perf soak 的假节点还是 v3 帧 —— v4 下任务永不被消费，后 8 分钟 100% 503

## 现象（2026-11-16，s1-soak 迁到 v4 之后的第一次 600s 实跑）

```
[+122s] 主压段 000-120  200×126900  503×43817
[+483s] 主压段 120-480  503×7579859        ← 100% 被拒
[+603s] 主压段 480-600  503×2499946        ← 100% 被拒
[+605s] 压测结束：events=128174 tasks=128174 rss=101548KB
```

即：**只有前 120 秒有真实写入**（≈1068 events/s），之后 8 分钟一个都没落库。
跑出来的 RSS/goroutine/DB 曲线只有前 2 分钟有效。

## 根因链（逐步实测）

1. v4 的派发要求 `deployments.state = applied`。soak 没有调 `POST …/capabilities/{name}/deploy`，
   所以 `NodesAvailable=False / NotDeployed`，任务全部停在 `phase=pending`。
2. 节点的消费端 `scripts/perf/c/agent.mjs` 仍是 **v3 帧**：
   监听 `assign`、回 `ack` / `result`（`agent.mjs:51,55,59`）。
   v4 的帧是 `task.dispatch` / `task.result` / `capability.deploy`（权威参考：`scripts/e2e/lib/nodes.mjs`
   的 `MockNode`，它处理 `capability.deploy`、`task.dispatch`、`lease.grant`、`task.emit.ack`）。
3. 即便**手工插一行** `deployments.state='applied'`：`NodesAvailable` 转 True、任务拿到 `node_id`
   与派发提案，但 `acked` 仍为 **0**（帧名不匹配，节点根本没收到/没回）。
4. pending 只增不减 → 最老 pending 的等待时长越过 `defaultPendingMaxAgeSec`（120s）
   → `server.backpressure.engaged` → **两个投递入口都回 503**（这是设计行为，不是 bug）。

所以这不是背压的 bug，而是**假节点没跟上 v4 协议**，把 soak 变成了「积压无界上涨」的实验。

## 修法（推荐 A）

| 选项 | 做法 | 代价 |
|---|---|---|
| **A. 迁移假节点** | 按 `scripts/e2e/lib/nodes.mjs` 的 `MockNode` 重写 `agent.mjs`：`hello`/`welcome` → 收 `capability.deploy` 落内容 → `task.dispatch` → 回 `task.result`；soak 起压前先 `POST …/capabilities/{name}/deploy` 建 deployed 行 | M；但这是「让整个 perf 台子能测稳态」的唯一正解，且 `MockNode` 是现成参考 |
| B. 关背压门 | soak 里设 `ANOTIFY_PENDING_MAX_AGE_SEC=0` | 一行；但曲线变成「无界积压下的 ingest 吞吐」，与 v3 的任何数字都不可比，只能当独立实验 |

**不做 B 当正解**：它会把「派发链路是否健康」这个前提从实验里删掉，而 soak 本来要测的正是
「全链路稳态」。

## 附带发现的运维风险（顺手记，别忘）

- 503 洪峰把 `$PC_RUN/server.log` 写到 **3.6 GB**。soak 跑满 600s 前必须确认磁盘余量；
  迁移假节点时可顺手给 soak 的服务加日志采样/轮转（或把 503 的 request 日志降到 debug）。

## 验收

- `bash scripts/perf/c/s1-soak.sh` 跑满 10 分钟，**后 8 分钟仍在写**（events/tasks 单调增长、
  `503` 比例 < 1%），并且 `acked`/`resulted` 随 `tasks` 同步增长。
- 稳态段 RSS/goroutine/fd 有可读的平线（这才是 soak 的产出）。
- 更新 `docs/perf` 里依赖 soak 的数字（若有），或明确标注不可比。

## 不做

- 不改产品侧的背压逻辑（它是按设计工作的）。
- 不改 `common.sh` / s2 / s5 / s6。
