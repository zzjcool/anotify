# 冻结契约 · 环放开 / 候选选择 / 批量创建 / 契约下发

> 本文件是**唯一事实源**。三个工作组的实现必须与此逐字一致；发现不一致先停下来问协调者，
> 不要自行改口径。日期：2026-11-04。

## 0. 背景（为什么改）

用户拍板四件事：

1. **允许成环**：`review → fix → review` 是合法编排，靠 **hop 预算**兜底，不做静态环检测。
2. **端口的下游是候选集合**，运行期默认 **1:1 选一个**（按优先级，同级轮盘）；`balance: all`（全派/扇出）是以后的功能，本期只留字段与分支，不实现。
3. **一个表单建多个能力**（a 和 b 一起定义），单事务，要么全成要么全不成。
4. **把出口契约下发给能力进程**（现在进程看不到，只能被 exit 4 打回来）。

## 1. 数据模型

| 变更 | 定义 |
|---|---|
| `edges.priority` | `INTEGER NOT NULL DEFAULT 0`。**小的优先**。加进 `idx_edges_lookup` 之后不需要额外索引 |
| `capability_versions.balance` | `TEXT NOT NULL DEFAULT '{}'`，JSON 对象 `{端口: {"mode": "priority"}}`；缺省 = `priority`。预留 `all` / `random` / `least_loaded` / `hash`，**本期只实现 `priority`**，其它值保存时 400（"尚未实现"），不要静默降级 |
| 迁移 | baseline 之后追加 **`-- +migration 2`**（**不要动 baseline 1**）。仓库是单块 baseline + runner 追加块的形状，见 `internal/store/migrate.go` |

`outputs` 保持 `[]string`（不改形状）。端口契约仍然是**从下游能力的 `params_schema` 减 literals 求并集**（现有 `BuildExpects` 口径不变，本期不动）。

## 2. 拓扑语义（环）

- **删除**配置期环检测：`store.checkEdgeWouldCycle` / `checkEdgeWouldCycleOn` 及其在 `createEdgeTx` 的调用；`edge_cycle_test.go` 反转为「成环保存成功」。
- **删除**运行期 `pathContains(in.Path, name)` 剪枝（`route/dispatch.go`）与 `route.ReasonPath` 常量语义。
- **新增 hop 预算**：`depth = len(入参 path)`（子任务 path = 父 path + 本能力名，现有形状不变）。`depth >= maxHops` → 新增剪枝 `ReasonMaxHops = "max_hops"`，`Detail` 带上完整 path。
- **新增 trace 任务预算**：同一 `trace_id` 已建任务数 `>= budget` → 新增剪枝 `ReasonBudget = "budget"`。
- 配置：服务端环境变量 `ANOTIFY_MAX_HOPS`（默认 **16**）、`ANOTIFY_TRACE_TASK_BUDGET`（默认 **512**）。非法值（非正整数）启动即失败，别静默用默认。

## 3. 候选选择（派发期）

对一个 `(能力, 端口)` 的候选集合（该端口的全部边）：

1. **过滤不可达**（沿用现有剪枝判定：目标能力不存在 `ref_missing`／无在线节点 `no_node`／参数不满足 `param_*`／跨用户 `foreign_user`）。
2. 剩余按 `to_ref` **字典序**排序（保证确定），再按 `priority` 升序。
3. 取**最小 priority 组**（同 priority 的整组）。
4. 组内**轮盘**：游标 `map[(namespaceID, capName, port, priority)] -> int`，**进程内**，首次使用随机初始化（`rand.IntN(len(group))`），选中后 `+1`。
   - 不持久化（用户明确要求）；不加表、不加列。
   - 组内顺序必须是排序后的确定顺序，否则「游标 +1」在不同进程里指向不同候选。
5. `balance.mode == "all"` 的分支**保留但不实现**（见 §1：保存时已 400），派发侧见 `all` 就按 `priority` 处理并 `slog.Warn` 一次。

每次 `emit` 独立走一遍上述流程（两次 emit = 两个独立任务，可以落到不同候选）。

## 4. 批量创建

新端点：`POST /v1/capabilities/batch`（会话鉴权，命名空间规则与 `POST /v1/capabilities` 相同）。

请求体：

```json
{
  "namespaceId": "ns_...",
  "capabilities": [
    { "name": "b", "displayName": "…", "outputs": ["done"], "paramsSchema": {…}, "run": {…}, "lifecycle": {…}, "sticky": "" },
    { "name": "a", "outputs": ["fix"], "run": {…}, "balance": { "fix": { "mode": "priority" } } }
  ],
  "links": [
    { "fromRef": "a", "fromPort": "fix", "toRef": "b", "literals": {}, "priority": 10 }
  ]
}
```

语义：

- **单事务**：全部能力行 + 全部产物行 + 全部边行。任一步失败 → 整批回滚（三条断言：能力 0 条、产物 0 条、边 0 条）。
- `capabilities[].name` 在批内**不可重复**（重复 → 400）。
- `capabilities[]` 的元素形状与 `POST /v1/capabilities` 的请求体**完全一致**（复用同一个 decode 结构，不要另造一份）。
- `links[].fromRef` / `toRef` 可以引用**本批新建**的名字（先插全部能力行，再解析边）。
- `links` 允许成环（§2）。
- 升级闸门：批内任一能力触发 `upgrade_breaking` → 整批 409（body 带 `upgradeImpact`），`force: true` 可越过（与单建同语义）。
- 响应：`{"capabilities": [...capabilityView...], "edges": [...edgeView...], "upgradeImpact": {...}|null}`，空列表给 `[]`。
- 单建接口 `POST /v1/capabilities` 的行为**一个字都不变**（含 `links` 字段）。

## 5. 契约下发到进程

能力进程新增两个契约变量（`cmd/agent` 的 `buildEnv`）：

```bash
ANOTIFY_EMIT_PORTS='[{"port":"fix","pick":"one","schema":{"type":"object","required":["issue"]},"targets":[{"capability":"b","version":2,"priority":10,"needs":{"type":"object","required":["issue"]}}]}]'
```

- `schema` 为 `null` 表示该端口没有下游（无约束）。
- `targets` 是候选清单（含 `priority`），按 `priority,capability` 升序。
- **追加（2026-11-06，见 `docs/design/emit-protocol.md` §2.1）**：
  - `pick` —— 一次 emit 建几个下游任务，目前恒为 `"one"`（同出口的多个下游是**候选**，不是扇出）。
  - `targets[].needs` —— 这个候选**自己**要的字段（中枢下发的 `expects[].targets[].needs` 原样透传），
    为 `null` 表示投给它不需要任何字段。没有它，作者只能看到端口并集（含**别人**的必填字段），
    点名发往其中一个时必然被误拒（事故：`trace_1793986152223364273_3611c934350f8618`）。
  - 两个字段都**总是输出**（不省按键）；这是向后兼容的**增量**（旧读者忽略未知键）。
- 同时把同一份 JSON 写进 `$ANOTIFY_TASKDIR/emit.json`（权限 0600；任务目录已清空重建）。
- 两个变量/文件**始终存在**（没端口时给 `[]`）；写入失败要让任务失败并报清楚，不要静默跳过。
- `ws.PortExpect` 已经有 `Schema`/`Targets`，`Targets[].Capability/Version` 已有，**加 `Priority`**（camelCase，与其它字段一致）。

## 6. 命名与文案

- 剪枝 reason：`max_hops`、`budget`（新增）；`path` 删除（前端/文档里不得再出现）。
- UI 术语：能力定义里的「出口（ports）」称 **「产出端口」**；端口下的目标是 **「候选（按优先级选择）」**；左侧导航「出口」页**本期不改名**（改名会牵动 e2e 与文档，另开任务）。

## 7. 分组（三组并行，互不改对方的文件）

| 组 | 范围 | 独占文件 |
|---|---|---|
| **A 拓扑与选择** | §2 + §3 | `internal/store/graph.go`、`internal/store/schema.sql` + 迁移块、`internal/route/dispatch.go`、`internal/route/prune.go`、对应 `_test.go` |
| **B 批量与契约下发** | §4 + §5 | `internal/store/capability_links.go`、`internal/server/*`、`api/openapi.yaml`、`cmd/agent/*`、对应 `_test.go` |
| **C 前端与文档** | §6 + UI | `web-src/**`、`web/**`、`docs/design/typed-pipeline.md`、`scripts/e2e/suites/frontend.mjs` |

A 与 B 都会碰 `internal/store/schema.sql`：**只有 A 动 schema 与迁移**；B 若需要新列，先停下来说。

## 8. 门禁（三组各自跑，合并后协调者再跑一次全量）

```
gofmt -l $(git ls-files '*.go')   空
go vet ./... && go build ./...
go test ./... -count=1            全绿
make contract                     A2/A4/A5/A6/A7 + openapi 一致
node scripts/check-classes.mjs    通过
make fe                           sitegen + 指纹
./scripts/e2e/run_all.sh <相关套件>
```
