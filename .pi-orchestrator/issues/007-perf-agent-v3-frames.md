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
gates: ["node --check scripts/perf/c/agent.mjs", "bash -n scripts/perf/c/s1-soak.sh", "bash scripts/perf/c/s1-soak.sh"]
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

## 进展（2026-11-16，`6c94d93`）

**帧链已修好并验证**：`agent.mjs` 86 → 375 行，走 v4 `hello`(两段) → `capability.deploy/deployed`
→ `task.dispatch` → `task.accept` → `lease.grant` → `task.result` → `result.ack`；
`s1` 起压前真的 `POST …/deploy` 并断言 `NodesAvailable=True`，每段断言 `acked/resulted` 必须涨。

实测：`acked=10647 / resulted=10644 / confirmed=10644 / errors=0`，每段都在涨 ⇒ 卡要防的「静默不消费」回归已被钉住。

**但仍拿不到 10 分钟稳态**，原因换了一个（见下）。

## 新的阻塞点：派发吞吐 ≈40-90 任务/s，摄取 330-670/s

- 对照实验（20 空槽、`inflight` 常为 0、`dropped=0`、`queued=0`）下派发仍只有 **87/s**；
  `RunDispatchLoop` 配置是 250ms/轮 ×100，实测一轮 ~1.1s。
- 每任务 **5 次读 + 1 次 assignment 写事务**，且与 ingest 抢同一把写锁。
- 摄取持续高于消耗 ⇒ 最老 pending 在 **t≈141s** 越过 120s ⇒ 背压介入 ⇒ 之后 100% 503。

这不是 soak 脚本的错，也不是背压的 bug；是**派发侧吞吐上限**。所以在「测稳态」这个目标上，
要么把压力降到天花板之下（给 loadgen 加 `-rate`，soak 默认跑 40/s），
要么单开产品卡去提派发吞吐（批量派发 / 去掉每任务 5 次读 / assignment 事务合并）。

## 收口（2026-11-16，`05a178b` + 后续）

**稳态已可测**：给 `loadgen` 加 `-rate`（全局令牌桶，0 = 不限），soak 默认 `S1_RATE=40`
—— 跑在派发天花板（≈87/s）之下，背压不再介入。

| 时长 | 请求 | 状态码 | events/tasks | 节点 |
|---|---|---|---|---|
| 240s @40/s | 4794/段 | **全 200** | 9659 / 9659（1:1） | acked 9582 / resulted 9575 / errors 0 |
| 120s @40/s | 3199 + 1595 | **全 200** | 4859 / 4859 | acked 4810 = resulted 4810 |

- 之前的 503 洪峰消失（背压阈值 120s 不再被触发）。
- 顺带修掉一个「静默读旧数据」的坑：`raw/ingest_<tag>.log` 用 `>>` 追加、`s1_seg_status`
  用 `grep -m1` 取第一行 → 重跑同一时长会读到**上一轮**的数字（实测：段 1 报 `200×40311`，
  本轮实际只有 4799）。现在每段开跑前清目录 + 清同 tag 文件。

**仍需跟进**：完整 600s 一轮（约 10 分钟）尚未跑；`S1_RATE=0` 的「极限/背压」实验单独一轮。

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
