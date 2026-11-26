# 派发预算恢复（max_hops / budget）+ playbook 包内成环放开

<!-- snapshot-doc: 2026-11-26 交付快照（下文测试结果、量测、门禁数字都是本次运行的定格；复跑命令见 §3–§6） -->

Fixes #655 · PR #656 · 基线 `origin/main` @ `6c3ac370`

## 1. 缺陷

`docs/contracts/CONTRACT.md` A7（「环合法 + hop 预算」）是**空头承诺**：

- v3 的 `route.Dispatcher`（`e552bd34^:internal/route/dispatch.go`）在**建任何东西之前**判两个预算：
  `len(in.Path) >= MaxHops` → `pruned/max_hops`；`CountTasksByTrace >= TraceTaskBudget` → `pruned/budget`。
- `e552bd34` 用 `route.Router` 取代 `Dispatcher`，两个预算没有搬过去：`ANOTIFY_MAX_HOPS` /
  `ANOTIFY_TRACE_TASK_BUDGET` 只在 `internal/server/config.go` 读进 config、`mux.go` 打一行
  「不再消费」的日志；`pruneReasons` 词表里也没有 `max_hops` / `budget`。
- 后果：环能建（两能力互指仍 201），但**没有刹车**——A7 承诺的兜底不存在。
- 同一轮还有一条与平台语义冲突的打包规则：`internal/playbook/render.go` 的 `checkCycle`
  拒绝「包内 next 成环」，把合法编排 (`review → fix → review`) 挡在安装之外。

## 2. 改法

### 2.1 depth：`tasks.parent_task_id` 的递归 CTE（不加迁移）

`store.AncestorDepth(ctx, ns, taskID)`（`internal/store/task.go`）返回**任务的深度**（含自身，根 = 1）：

```sql
WITH RECURSIVE ancestors(id, parent_task_id, depth) AS (
    SELECT id, parent_task_id, 1 FROM tasks WHERE namespace_id = ? AND id = ?
    UNION ALL
    SELECT t.id, t.parent_task_id, a.depth + 1
      FROM tasks t JOIN ancestors a ON t.id = a.parent_task_id
     WHERE t.namespace_id = ? AND a.depth < maxAncestorDepth)   -- 64：坏数据不许打穿
SELECT COALESCE(MAX(depth), 0) FROM ancestors
```

- **不加列、不加迁移**：v4 baseline 已有 `idx_tasks_parent(namespace_id, parent_task_id)`，
  v3 的 `tasks.path` 列不回来。
- 递归步写成 `t.id = a.parent_task_id`（用主键上行），**不是** `t.parent_task_id = a.id`：
  后者在本仓的驱动（`modernc.org/sqlite`）上递归项**一行都不产出**（实测 `MAX(depth)=1`；
  同一份数据换 `t.id = a.parent_task_id` 立刻得 3）。这条踩坑写进这里，避免后人「优化」回去。

### 2.2 budget：`CountTasksByTrace`

口径与 v3 一致：`COUNT(*) WHERE namespace_id = ? AND trace_id = ?`，只数**已存在**的行，
本次要建的不算，所以预算本身就是上限。

### 2.3 一份判定，三条派发路径

`route.Router.checkBudget`（`internal/route/budget.go`）是唯一判定点，被三处调用：

| 路径 | 调用点 | parentTaskID |
|---|---|---|
| 源入口 | `PlanFromSource` | `""` → depth 从 **1** 起算 |
| 能力 emit | `PlanFromEmit`（`ws` → `route.Emit`） | 父任务 id → depth = 父深度 + 1 |
| 判决结局 | `PlanFromOutcome`（#522 的第三条建任务路径） | 父任务 id |

- **顺序固定**：先 hop（一次祖先链查询）再 budget（一次 COUNT），与 v3 一致；两者都命中只留
  hop——一次派发只留一条原因（`TestBudgetHopWinsAndOnlyOneReason`）。
- 命中时的形状：计划里 0 个 target、1 条 `Prune`，回执是 `pruned.{reason}`（**不是 4xx**），
  emit 回执 `taskId:null` + 真实 `eventId`。
- `Budget` 的 `<= 0` 回落默认值（16 / 512）：被关掉的预算等于没有防线。装配在
  `mux.go`：`route.New(db).WithBudget(route.Budget{MaxHops: cfg.MaxHops, TraceTaskBudget: cfg.TraceTaskBudget})`，
  `config.go` 的两个默认值改为直接引 `route.DefaultMaxHops` / `route.DefaultTraceTaskBudget`（消掉第二份副本）。

### 2.4 词表

`internal/protocol/enums.go` 新增 `ReasonMaxHops = "max_hops"` / `ReasonBudget = "budget"`，并进
`pruneReasons` 闭集（`IsPruneReason` / `PruneReasons()` 自动覆盖）。

### 2.5 放开 playbook 包内成环

- 删 `internal/playbook/render.go` 的 `checkCycle` 与调用点、`playbook.go` 的 `CodeCycle`；
  `TestRenderCycle` → `TestRenderCycleAllowed`（正向：`a → b → a` 与 `a → b@2 → a` 都必须渲染成功）。
- 连带清掉两处同口径的旧实现：`web/mock.js` 的 mock 打包期环检测、`scripts/v4-frontend.test.mjs`
  里断言 `reason === "cycle"` 的那条（改成断言环渲染 200）。
- 平台口径写在代码注释与 `CONTRACT.md` A7：**环合法，由预算兜底**。

### 2.6 文档

- `CONTRACT.md` A7 改成与实现一致（谁算 depth、三条路径共用、顺序、`pruned` 不是 4xx）。
- `CONTRACT.md` §7 的两张剪枝表里 `path`（环剪枝）两行 → `max_hops` / `budget`，并写明
  **环本身不是剪枝原因**；§9.1「v4 已退役的 §9 行」删掉 A7 那行（它现在真的在门禁里）。
- `api/resource.yaml`、`api/agent.yaml` 的剪枝码描述补 `max_hops` / `budget` 与「环合法」。

## 3. 量测（先量再选，非退化选择）

一次性测量（5k / 50k 行 tasks 表，单条链 16 层；`internal/store` 临时基准，未入库）：

| 查询 | 5 000 行 | 50 000 行 |
|---|---|---|
| `AncestorDepth`（16 层链的叶子） | 38.2 µs | 41.6 µs |
| `AncestorDepth`（根） | 10.4 µs | 10.6 µs |
| `CountTasksByTrace`（100 行 trace） | 34.5 µs | 25.7 µs |

结论：递归 CTE 的代价与表规模**无关**（每层一次主键查），无需改成「事务内一次性算」；
两条查询合计 ~70 µs，相对一次派发（多次往返 + 建行 + 事件写）可以忽略。

## 4. 验收证据（5 条，全部真跑）

| # | 要求 | 测试 | 断言要点 |
|---|---|---|---|
| ① | 环能建、第二圈真的跑 | `route.TestEmitCycleSecondLapWithinBudget` | a→b 建子任务；b→a 第二圈**也建**（`taskId` 非空、无 `pruned`），第二圈任务经 `AncestorDepth` 测得 depth=3 |
| ② | `depth >= MaxHops` → `pruned/max_hops`，不建任务 | `route.TestEmitMaxHopsPrunesAtBoundary` | MaxHops=3：depth 2 **放行**、depth 3（== maxHops）剪；回执 `pruned.reason=max_hops`、`taskId=nil`、`eventId` 真实；剪枝前后 trace 任务数不变（2 → 2） |
| ③ | 同 trace 任务数 `>= budget` → `pruned/budget` | `route.TestEmitTraceBudgetPrunes` | budget=3：2 行放行、3 行剪；`pruned.reason=budget`、无 taskId，emit 不报错 |
| ④ | 反例：一圈合法链不许被剪 | `route.TestBudgetDoesNotPruneLegalChain` | 默认预算下 `review → fix → review` 第二圈建任务、无 `pruned`（防「同名就剪」复活） |
| ⑤ | 入口路径也生效，depth 从 1 起算 | `source.TestIngestEntryDepthStartsAtOne` | MaxHops=1 → `pruned.reason=max_hops`、`tasks` 为空、`eventId`/`traceId` 有（事件照收）、tasks 表 0 行；MaxHops=2 → 建任务且 `AncestorDepth=1` |

附带：`route.TestBudgetHopWinsAndOnlyOneReason`（顺序 + 只留一条原因）、
`route.TestPlanFromOutcomeUsesSameBudget`（第三条第路径同一判定）、
`store.TestAncestorDepth`（深度口径：根=1 / 逐层/ 查不到=0 / 缺 ns 报错）。

## 5. 变异（2 条，必红）

| 变异 | 结果 |
|---|---|
| `depth >= maxHops` → `depth > maxHops`（差一） | 🔴 `TestEmitMaxHopsPrunesAtBoundary`（`剪枝原因 = <nil>，want max_hops`）+ `TestIngestEntryDepthStartsAtOne`（`剪枝不许建任务`） |
| 删掉 budget 判定（只留 hop） | 🔴 `TestEmitTraceBudgetPrunes`（`剪枝原因 = <nil>，want budget`） |

两次变异后都还原并复跑绿。

## 6. 门禁

`make ci`（`timeout 1500 make ci`）→ **exit 0**：`make contract` 静态断言 + openapi/字段对账、
gofmt / staticcheck、`make fe` + `check-dist-fresh`、测试纪律与注释/注解双棘轮、
`go test ./... -count=1 -shuffle=on` **42 个包 ok**、e2e harness/锁自测、`make test-frontend` /
`test-frontend-v4`、`go build -tags embeddist`；基准冒烟
`BenchmarkDispatchChain-12  4732183 ns/op / 2322 allocs/op`（预算查询占其中 ~70 µs）。

`make e2e-affected`（`timeout 2400`）→ **exit 0，31/31 套件全绿，断言 3556 通过 / 0 失败**
（`routing` 508 / `playbook_release` 614 / `contract` 185 / `api_contract` 206 / `ws_protocol` 134 /
`persistence` 82 / `live_agent` 62 / `capacity` 84 / `frontend` 47 / `events_paging` 179 …）。
跑了两次：默认并发与 `E2E_CONCURRENCY=1` **都是 31/31 / 3556 通过 / 0 失败**。

（第一次默认并发的跑里 `routing` 有 2 条断言失败——单跑 `./scripts/e2e/run_all.sh routing` →
508 通过 / 0 失败 / exit 0，重跑全量默认并发也全绿，按负载假红处理；**没有**写任何
known-env 放行名单。）

## 7. 注释棘轮（只降不升）

`node scripts/check-comment-density.mjs` 双向棘轮：新增 `internal/route/budget.go` / 两个测试文件
与几处解释的注释行，**由删除抵账**（`internal/playbook` 删掉 `checkCycle` 的整段注释、`internal/server`
去掉「v4 不再消费」的错话、`internal/route` 收掉几条复述代码的注释）。`--update` 只把基线
**下调**（仓库总量 42929 → 42907、`internal/playbook` 1321 → 1319、`internal/server` 7753 → 7734、
`internal/store` 6761 → 6760、`docs/contracts` 2720 → 2719 行），**没有任何一项上调**。

## 8. 遗留（明确不做）

1. `CONTRACT.md` §7 仍是 v3 时代的两张表：除本次替换的 `max_hops`/`budget` 行外，
   `no_edge` / `no_node` / `foreign_user` 在 v4 实现里已经不存在（v4 的闭集是
   `no_target/event_schema/param_*/capability_disabled/no_match/max_hops/budget`）。
   整表重写超出本 issue 范围，留给文档 owner。
2. **并发窗口照旧存在**（与 v3 同口径）：plan 与建行不在同一个事务里，两个同时读到 511 的派发会各建
   一条，trace 最终到 513。预算防的是「无限扇出的量级」，不是精确到行的硬账；要精确就得把 COUNT 挪进
   建行事务（emit 的 `EmitAuthorized`），那会把剪枝留痕也拖进事务。
3. 源入口的 budget 判定**恒不触发**：`source.Ingest` 每次用新 traceId，`COUNT` 必为 0。仍保留这次
   查询是为了「一份判定」而不是两条相似代码（将来若有 trace 复用，它已经是对的）。
