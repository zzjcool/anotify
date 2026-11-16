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

## 已修（本轮）

`scripts/perf/c/common.sh` 三个助手按 v4 校准（`pc_seed` / `pc_cap` / `pc_source`）：

| 助手 | v3 → v4 |
|---|---|
| `pc_seed` | `.namespaces[0].id` → `.items[0].metadata.uid` |
| `pc_cap` | `/v1/capabilities` + `{namespaceId,name,paramsSchema}` → `/v1/namespaces/{ns}/capabilities` + 信封（`spec.run` 必填，入口要 `#!`），返回 `.metadata.uid` |
| `pc_source` | `/v1/sources` + `targets[].capabilityId` → `/v1/namespaces/{ns}/sources` + `targets[].capability`（只认 name/name@version，所以对 `cap_…` 先反查 name），hookUrl 取 `.spec.config.hookUrl` |

`scripts/perf/c/s6-capacity.sh` 的 4 处 `INSERT INTO events (... idem_key ...)` 去掉 v3 列。

## 还没修（下一张卡）

- `s1-soak.sh`：`s1_cap` 自己拼 v3 body，且 `outputs` 字段在 v4 不存在（对应 `spec` 的其它字段）。
- `s2-retention.sh`：`pc_cap perfgen` 用法相同但后续断言可能仍按 v3 字段。
- `s5-slowquery.sh`：`EV_COLS="… idem_key …"`、两处 `INSERT` 同 s6。
- 其余段（a/d/e/f）是否需要改，取决于它们是否只用 `pc_*` 助手。

## 验收

- `bash scripts/perf/c/s6-capacity.sh b` 能跑完并产出口径正确的数字（清理轮次、页数平台化）。
- 修完后**重跑并更新** `docs/perf/capacity-and-backpressure.md`（或明确标注「v3 时代数字，待复测」）。
- 全量六段（a-f，约 10 分钟机器时间）单独一轮跑，不在本卡混。

## 不做

- 不改 perf 脚本的测量方法（只做 v4 形态迁移），不改阈值/断言方向。
- 不为了让脚本跑通而放宽断言。
