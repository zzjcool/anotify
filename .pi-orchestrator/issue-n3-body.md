## 现象

agent 的 `node.yaml` 已是 **v2**：`cmd/agent/config.go:43-50` 的 `nodeConfig` 只有 `Name/Server/Token/Instances`；`:137-138` 用 `dec.KnownFields(true)`，所以 v1 的 `capabilities:`（含 `run.concurrency`）是**未知字段 → 启动即报错退进程**。

但仓里多处脚本仍在生成 v1 yaml：

| 位置 | 问题 |
|---|---|
| `scripts/perf/b/lib.sh:178-194`（`pb_node`） | yaml 里带 `capabilities:` + `run.concurrency` → agent 拒绝加载 |
| `scripts/perf/b/lib.sh:212-219`（`pb_node_create`） | 同上 |
| `scripts/perf/3b/nodes.sh:74` | 同上 |
| `scripts/demo/chain.sh:177` | 同上，而 `make demo-chain`（Makefile:149）是**活目标** |
| `cmd/devseed/demo.go:238,245` | hello 仍发 `capabilities` 键与 `concurrency: 4`（真实 agent 恒发 1）；另 `protocol: 1` 与当前 protocol 2 不兼容（`internal/ws/node_handshake.go:250` 会拒） |

⇒ #220 的四臂复现脚本（`scripts/perf/b/s2-roundtrip.sh` 的 a/b/c/d）现在连节点都起不来；`s2` 的 b（1×8）与 d（8×8）两臂在 v2 下**语义上也不可实现**（`config.go:295` `slotConcurrency()` 恒 1）。

参考配方：`scripts/e2e/lib/nodes.mjs:180-260` 已经是能跑通的 v2 形状（bootstrap yaml + `instances` 数组），照它改。

## 顺带（契约侧）

`docs/contracts/CONTRACT.md:254` 与 `internal/store/node.go:1238` 仍写着
「实际并发 = min(实例 concurrency, 实现级 capabilities[].concurrency, nodes.max_concurrency)，由调用方（路由层）算」——
该公式**从未实现**，且 `capabilities[].concurrency` 在 v2 已不存在。`node_instances.concurrency` 只被写入/读出，没有任何调度判定读它（`LeaseInstance` 只看 `alive=1 AND busy_task IS NULL`）。

**不要**把 `node_instances.concurrency` 封顶成 1：agent 已恒发 1，封顶会红 2 个用例（`internal/store/node_test.go:284`、`internal/ws/node_test.go:481`），且与「节点申报」语义冲突。改契约文字即可。

## 验收

- `bash -n` 全部相关脚本
- `make demo-chain` 能起节点并跑通一条任务
- `scripts/perf/b/s0-smoke.sh` 在 v2 下能跑（或脚本显式 `pb_die` 指向本 issue，不静默生成坏配置）
- devseed 起的节点能在当前 server 上 online（不再 protocol 拒收）
