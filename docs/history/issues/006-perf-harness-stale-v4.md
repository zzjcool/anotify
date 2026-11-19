---
id: 006
kind: bug
area: [e2e]
size: L
severity: P2
state: working
needs_recon: false
contract: []
depends_on: []
route: [worker]
gates: ["bash -n scripts/perf/c/common.sh", "bash scripts/perf/c/s6-capacity.sh b"]
opened: 2026-11-16
---

> 迁移自 .pi-orchestrator/issues/006-perf-harness-stale-v4.md（2026-11-19，协调者工作区不再入库）
> snapshot-doc: 2026-11-19 迁移时的原件定格（文中数字是当时契约定格/实测，不是「现在是多少」的承诺）

# 006 · perf 套件整体对 v4 失效 —— `docs/perf` 的数字无法复现

## 现象（2026-11-16 实测）

`scripts/perf/c/` 下的脚本仍是 v3 形态（路径、信封、表列三处都变了），跑起来会在
**第一阶段就死**，所以 v4 之后没有任何一次成功的 perf 测量。

### 证据 1：路径 404

```
$ curl -s -X POST http://localhost:8199/v1/capabilities ...
404 page not found
```

v4 的能力/源都是命名空间子资源：`/v1/namespaces/{namespace}/capabilities`、
`/v1/namespaces/{namespace}/sources`（`api/resource.yaml:48`、`:287`）。

### 证据 2：信封字段换了名字

`pc_seed` 读 `(.namespaces // [])[0].id` → v4 返回 `{"items":[{...,"metadata":{"uid":...}}]}`，
于是 PC_NS 恒为空：

```
pc_seed: 取不到默认命名空间（会话是否有效？）
```

### 证据 3：表列不存在

```
Parse error near line 2: table events has no column named idem_key
```

v4 的 `events` 没有 `idem_key`（改成了 `delivery_id` / `receipt`，见 `schema.sql` 的 events 段）。

## 影响

- `docs/perf/capacity-and-backpressure.md` 里的所有数字来自 **2026-11-01** 的运行
  （`/home/*/perf-b5/runs/*/server.log` 的时间戳），是 v3 时代的结果，**不可复现**。
- 005（保留期 env 改为空库播种）之后 s6 的 b/c 段口径本就需要复测——现在连台子都起不来。

## 已修（2026-11-16）

`scripts/perf/c/common.sh` 三个助手按 v4 校准：

| 助手 | v3 → v4 |
|---|---|
| `pc_seed` | `.namespaces[0].id` → `.items[0].metadata.uid` |
| `pc_cap` | `/v1/capabilities` + `{namespaceId,name,paramsSchema}` → `/v1/namespaces/{ns}/capabilities` + 信封（`spec.run` 必填，入口要 `#!`），返回 `.metadata.uid` |
| `pc_source` | `/v1/sources` + `targets[].capabilityId` → `/v1/namespaces/{ns}/sources` + `targets[].capability`（只认 name/name@version，所以对 `cap_…` 先反查 name），hookUrl 取 `.spec.config.hookUrl` |

`scripts/perf/c/s6-capacity.sh`：

- 4 处 `INSERT INTO events (… idem_key …)` 去掉 v3 列；
- 清理日志名 `lease.sweeper.retention` → `retention sweep deleted rows`，字段 `events_deleted=N` → `"events":N`，
  失败计数 `lease.sweeper.retention_failed` → `lease sweep failed`；
- c 段建第二命名空间：v3 `{"slug","name"}` → v4 信封 `{apiVersion,kind,metadata{name,displayName},spec{}}`；
- c 段读 URL：`?namespaceId=<id>` → `?namespace=<名字>`（v4 缺参直接 400，实测 `400×298576`）；
- c 段测量时长 20s → **40s**（v4 首拍清理在 ~30s，窗口太短则「删除落在窗口内」不成立）；
- `S6_SKIP_BUILD` 语义修正：原实现 `[ -n ]` 判非空 → `S6_SKIP_BUILD=0` 也跳过（与直觉相反）。
  现在只认 `1/true/yes/on`，且「跳过了但没二进制」会当场报清楚（实测：`s6_build: … 缺二进制` → `❌ 构建失败`），
  不再拖到后面以「取不到默认命名空间」这种不相干的报错死掉。

`scripts/perf/c/s5-slowquery.sh`：两处 `events` 插入列 + `EV_COLS` 去掉 `idem_key`（**未跑验证**）。

## 复测结果（2026-11-16，v4 代码）

- **b 段已复测**：26000 行积压 → 两轮 20000 + 6000 删空，文件 100.0% 不变、freelist 1258 页、
  重灌后 101.4% ⇒ 「平台化而非变小」的结论在 v4 仍成立。已把新数字写回
  `docs/perf/capacity-and-backpressure.md` §3。
- **c 段已复测**：baseline p95 1.428ms / under-sweep p95 1.415ms（20000 行删除落在 40s 窗口内），
  结论「无可测退化」不变。已写回 §4。

## 还没修

- ~~`s1-soak.sh`：`s1_cap` 拼的是 v3 body~~ → **已迁**（`21f7ced`）：改成 `spec.next` + `source.targets`
  + Bearer 命名空间投递入口，删掉建边/outputs 逻辑；600s 实跑通过（events=tasks=128174、edges=3 自动物化）。
  但它暴露了下一个阻塞点 → **[007](007-perf-agent-v3-frames.md)**（假节点还是 v3 帧，后 8 分钟全 503）。
- `s2-retention.sh`：`pc_cap perfgen` 之后的断言可能仍按 v3 字段，未跑验证。
- `s5-slowquery.sh`：SQL 列已改，但整段未跑验证（它还用 `svc_up` 自己起停服务）。
- a/d/e/f 段未跑（a 段依赖 30 天外推口径，d/e 依赖背压，f 段是 profile）。

## 验收

- `bash scripts/perf/c/s6-capacity.sh b` 能跑完并产出口径正确的数字（清理轮次、页数平台化）。
- 修完后**重跑并更新** `docs/perf/capacity-and-backpressure.md`（或明确标注「v3 时代数字，待复测」）。
- 全量六段（a-f，约 10 分钟机器时间）单独一轮跑，不在本卡混。

## 不做

- 不改 perf 脚本的测量方法（只做 v4 形态迁移），不改阈值/断言方向。
- 不为了让脚本跑通而放宽断言。
