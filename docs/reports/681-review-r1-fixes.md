# #681 review 第 1 轮：逐条处理（PR #688 追加提交）

as_of: 2026-11-27 · PR zzjcool/anotify#688 · 分支 `pi-subagent/issue-681-r1` · 本文件＝r1 的裁决与取证

snapshot-doc: 2026-11-27 的取证快照（数字是当时实测的定格，不是对未来的承诺）

## 结论

7 条意见全部处理，没有静默跳过：1/2（major）补断言与直测；3/4/5/6 改实现（含基准）；
7（note）补了一条真 e2e。每条新增的回归网都跑过「改回去即红」。

| # | 级别 | 裁决 | 落点 |
| --- | --- | --- | --- |
| 1 | major | 认，补用例 | `internal/store/dispatch_budget_test.go`（总量层） |
| 2 | major | 认，补用例 | `internal/ws/steer_test.go`（窗口 / 清除，直测） |
| 3 | minor·性能 | 认，跳过 + 单次 sweep 复用 | `internal/ws/hub.go`、`internal/store/execution_auth.go`（memo）、基准 |
| 4 | minor | 认，恢复按需统计 | `budgetShape` 一处派生 + `TestBudgetShapeLayersAreOnDemand` |
| 5 | minor | 认，内存快筛 | `internal/ws/steer.go`（`nodeSteered`） |
| 6 | nit | 认，改文案 | `执行预算: …` + `TestDispatchBudgetErrorsAreSideNeutral` |
| 7 | note | 认，补 e2e | `scripts/e2e/suites/routing.mjs` 第 17 节 |

## 逐条

### 1. [major] 机器总量（limits.total）层零覆盖

原来只有 pinned / perCapability 两层的断言，删掉 `AdmitsOne` / `OverBy` 里的总量两段仍然全绿。

新增 `TestDispatchBudgetsSeeMachineTotalLayer`：节点 `total=1`、先跑一条**别的能力**的执行（让
pinned/perCapability 都不参与），断言 `TotalUsed/TotalLimit=1/1`、`CapabilityLimited=false`、
`AdmitsOne()==false`、`OverBy()==1`，且同一夹具下 `grant` 按**同一层**拒（消息含「机器总量预算」）。

变异自证（删掉 TotalLimit 两段）：

```
--- FAIL: TestDispatchBudgetsSeeMachineTotalLayer (0.07s)
    机器总量已满（1/1）必须判不许再收：
      {TotalUsed:1 TotalLimit:1 CapabilityLimited:false PinnedUsed:0 PinnedLimit:4}
```

### 2. [major] slotSteer 两条转移零覆盖

新增两条直测（`internal/ws/steer_test.go`）：

- `TestSlotSteerWindowExpires`：mark 后按窗口边界断言，并断言过期条目被顺手回收、建议按
  node/ns/能力 精确匹配。
- `TestSlotSteerClearedByGrant`：走 `noteSlotFull` / `clearSlotFullSteer`（handleReject /
  handleAccept 用的就是这两个），mark 生效 → 一条租约授出后清除；没有建议时再调一次不读库也不报错。

口径差异（不是放宽，是纠正 review 里的猜测）：30s 窗口是**排他**边界（`now >= until` 即失效），
与 `assignment_expires_at` 的到期判据同形，所以断言取 ±1s：`+29` 有效、`+30` 失效。
review 原话「mark 后 now+30 仍 true」与实现不符；两种口径都能防「窗口永不过期」，我选了与派发窗口
一致的那一种并把它钉住。

变异自证：

```
# if now >= until → if false && now >= until（窗口永不过期）
--- FAIL: TestSlotSteerWindowExpires
    30s 到点建议必须失效（否则被拒过的机器长期排在后面）

# clearSlotFull 改成空实现
--- FAIL: TestSlotSteerClearedByGrant
    授出一次租约后建议必须清除
```

### 3. [minor·性能回归] 1Hz sweep 的库访问放大

两处都改了：

1. `len(nodes) < 2` 时不查预算表（`dispatchCandidates` 本来就原样返回，这部分是纯废算）。
   单节点集群 / 现有单节点用例回到改前的读量。
2. 新增 `store.DispatchBudgetMemo`：**一次 sweep 共用一个**，复用「一轮内不变」的读 ——
   能力 `maxPerNode`（按 capabilityID）、机器快照（按节点）、real 占用层（按
   节点+ns+能力+层集，层集进 key 所以不同能力版本不会串），只有**在飞候选**按任务重算
   （它随本轮派发变化，不能缓存）。`DispatchBudgets` 保留为「不共享 memo」的等价入口，
   review 的变异用例照旧可用；hub 走 `DispatchBudgetsMemoized`。

新增基准 `BenchmarkDispatchBudgetSweep`（100 条同能力 pending × 2 台在线节点 = 一次 sweep 的预算读量，
`-benchtime 10x -count 3`）：

```
BenchmarkDispatchBudgetSweep/memo-12   10  13524525 ns/op  494948 B/op   9938 allocs/op
BenchmarkDispatchBudgetSweep/memo-12   10  15418450 ns/op  494950 B/op   9938 allocs/op
BenchmarkDispatchBudgetSweep/memo-12   10  18479565 ns/op  494924 B/op   9937 allocs/op
BenchmarkDispatchBudgetSweep/fresh-12  10  30608458 ns/op 2094916 B/op  43302 allocs/op
BenchmarkDispatchBudgetSweep/fresh-12  10  39656742 ns/op 2094943 B/op  43302 allocs/op
BenchmarkDispatchBudgetSweep/fresh-12  10  42438187 ns/op 2094925 B/op  43302 allocs/op
```

`memo`（本轮）≈ fresh（PR head 的逐任务全量查）的 1/2.3，分配是 1/4.4；
剩下的成本是每条任务 × 每台候选的在飞候选查询（review 建议的「只重算 inflight」）。

`make bench-dispatch` 已把它接进 `make ci`（review 指出的「CI 抓不到」）。基准的护栏是
`TestDispatchBudgetSweepFixtureIsReal`：夹具必须真的读出两台节点的余量，基准不许跑空。
`TestDispatchBudgetsMemoReusesRealButNotInflight` 钉住「共用 memo 仍按任务排自己的候选」，
变异（`pendingProposalsOn` 不再排除任务自己）即红。

### 4. [minor] grantBudgetOn 无条件跑三层统计

抽出 `budgetShape`：**一处**派生「哪几层配了、每层上限多少」，grant 与选点共用同一份派生，
所以不会出现两边口径漂移，同时没配的层不查也不判 —— 回到改前的 2 条（无 perCapability 且
pinned 不设限）/4 条（有 pinned）。`TestBudgetShapeLayersAreOnDemand` 钉住这份派生
（变异：`layers()` 恒为 `{total,capability,pinned}` 即红）。

accept/grant 这条热路径的基准仍由 `BenchmarkDispatchChain`（assign → grant → complete）覆盖。
3-way 实测（`-benchtime 300x -count 3`，同一台机器）：改前 `a5c606f4` 2173–2304µs / 1822 allocs、
PR head `36dd4904` 2325–2495µs / **1881** allocs、本轮 2276–2361µs / **1826** allocs —— 多出来的
两条 SQL 与 55 次分配已回到改前水平。

### 5. [minor] handleAccept 成功路径每条租约多一次 GetTaskByID

`clearSlotFullSteer` 先查内存表（`nodeSteered`，按 nodeID 扫小 map），没有建议就直接返回，
不再为罕见记账在读路径上多读一次 task。

### 6. [nit] 共用后的错误文案仍写「授予租约」

`budgetBusyOn` / `pendingProposalsOn` / `maxPerNodeOn` 的包装改成「执行预算: …」：派发侧读失败现在
是 `dispatch budgets: 执行预算: 统计在飞候选: …`，不再把排查方向指向 grant。
`TestDispatchBudgetErrorsAreSideNeutral` 用关库造读失败，断言错误里没有「授予租约」（改回去即红）。

### 7. [note] 验收第 1 条只有调度侧用例，没有 e2e

补了一条 e2e（`scripts/e2e/suites/routing.mjs` 第 17 节）：真 server 二进制 + 1Hz 真派发循环
+ 两台 MockNode（真 WS 会话、真帧）——

- 能力 `spread.spread681`（`maxPerNode=1`）部署到两台，4 单同时进来；
- 断言两台候选**各 2 条提案**（不是 4:0）；
- 两台各 accept 1 条 → 两条 `running`（attempt=1）分别落在两台，另 2 条仍在等槽（不许越权）。

变异自证（`dispatchCandidates` 直接 `return nodes`，即修前行为）：

```
❌ 17.3 四单在两台候选上各 2 条提案（不是 4:0） — 期望 [2,2] 实际 [4,0]
❌ 17.6 两条执行分别落在两台机器上 — 期望 ["spread-node-a","spread-node-b"] 实际 ["spread-node-a"]
断言失败: 5
```

## 验证命令与输出（本机实测，`umask 0022`）

```
$ go test ./internal/store/... ./internal/ws/ -count=1
ok  github.com/zzjcool/anotify/internal/store          20.4s
ok  github.com/zzjcool/anotify/internal/store/v4test   60.1s
ok  github.com/zzjcool/anotify/internal/ws              6.4s

$ make ci                            # contract/lint-go/fe/dist-fresh/注释棘轮/…/bench-dispatch/make test
EXIT=0

$ ./scripts/e2e/run_all.sh           # 全量矩阵（固化门禁，32 套件并行）
EXIT=0（32/32 套件达标，断言 3619 通过 / 0 失败）

$ ./scripts/e2e/run_all.sh --serial routing
EXIT=0（535 通过 / 0 失败；第 17 节 26 条断言全绿）
```

并行矩阵里暴露并修掉的一处 e2e 夹具问题：`scripts/e2e/lib/nodes.mjs` 的 `seedNodeRows` 直插库时
没有 `busy_timeout`（默认 0），第 17 节两次 `MockNode.connect` 在高负载下撞上 server 的写锁，
routing 报 `database is locked`（首轮全量 e2e 的唯一红）。已按 `V.withDB` 同口径置
`PRAGMA busy_timeout = 10000`；这不是产品改动，是套件夹具的等锁口径。

## 取舍与遗留

- 无 schema / 迁移改动（`headVersion` 未动）。
- 注释棘轮（L12）是「只降不升」的：本轮新增注释按**同目录等量压缩**处理，
  `node scripts/check-comment-density.mjs` 仍与基线逐字一致（不靠抬基线过关）。
- `DispatchBudgetMemo` 是**单次 sweep** 的生命周期（hub 每轮新建）：跨 sweep 复用会让快照/real
  占用变成陈旧写入，故不做。
