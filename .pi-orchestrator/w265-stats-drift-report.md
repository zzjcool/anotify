# #265 统计维护「真刷新」报告（worker · 分支 power）

结论一句话：**证伪成立 —— stat1 没变。** 改动见 commit `8af2fcc`（在 PR #309 上）。

## 1. 证伪结果：**没变**（#265 的第一版是安慰剂）

用例 `internal/store/stats_test.go: TestMaintainStatsRefreshesDriftedStat1`（文件后备库）：

1. 建库（`newTestDBFile`）→ `newTestNS` + `mkCap` + `CreateCapability` → `mkSeqTaskBatch(100)`；
2. `ANALYZE "tasks"` 建基线，读回 `sqlite_stat1` → **100**（断言 90..110）；
3. 再灌 2400 条 → 真实 2500 行（**25×**，远超 SQLite 的 10× 阈值）；
4. 调 `db.MaintainStats(ctx)`（= 6h 维护任务做的事）；
5. 再读 stat1。

**修复前（红，原样输出）**：

```
=== RUN   TestMaintainStatsRefreshesDriftedStat1
    stats_test.go:153: MaintainStats 执行的语句 = []；真实行数 = 2500，基线估计 = 100
    stats_test.go:161: 证伪成立：MaintainStats 之后 stat1 仍是 100（真实行数 2500，25×）——它就是再跑一遍 PRAGMA optimize=0x2，够不到运行期漂移，#265 只是字面上被修
--- FAIL: TestMaintainStatsRefreshesDriftedStat1 (0.19s)
```

即：`PRAGMA optimize=0x2` 返回**空建议集**（`执行 = []`），stat1 停在 100。
6h 任务当时跑的正是同一条 `=0x2`，所以它什么都没刷 —— #265 只在字面上被修。

额外探针（临时文件，已删）补充事实：`PRAGMA optimize=0x10002`（含「10 倍漂移就复查」那一位）
在本环境**确实会静默刷新** stat1（100 → 2500，返回建议集仍为空）；但它那一档判定依赖
**连接级**的「本连接用过该表」记录，维护 goroutine 从池里拿到哪条连接不确定，
且它会对**任意**漂移表下手（与「后台只碰 tasks」约定不符）。所以没选它，选确定性判定。

## 2. 改了什么

- `internal/store/store.go`
  - `maintainStatsOn(ctx, q, trigger)`：`PRAGMA optimize=0x2`（负责「缺失」）跑完后，
    再做一次**确定性漂移检查** `refreshDriftedStats`（负责「过期」）。
  - `refreshDriftedStats`：只针对 `tasks` —— 读 `sqlite_stat1` 的行数估计，
    与 `SELECT count(*) FROM tasks` 比，差 ≥10×（`statsDrifted`）才 `ANALYZE "tasks"`。
    无基线（stat1 不存在/无条目）时不越界（那是 0x2 的活）；任何错误只 warning（不致命）；
    成功打 `slog.Info(..., "event", "lease.stats.refreshed", "trigger", ..., "prev_rows", ..., "cur_rows", ...)`。
  - 新 helper：`stat1RowEstimate` / `countTableRows` / `statsDrifted`。
  - 修正 4 处与代码/实测不一致的注释：:127（「缺失**或过期**」→ Open 只覆盖「缺失」）、
    :136-141（② 改写：指向本文件的运行期刷新，不再是「另一个 issue」）、
    :150（「保留全部判定规则」与 :132 互斥 → 改成「只剩『无 stat1 条目』这一条」）、
    :157（「走 db.Exec」→ 实际是 `QueryContext` 读建议 + 逐条 `ExecContext`）。
- `internal/lease/stats.go`：调度不变（5s ticker + 低频门；`StatsInterval < 0` 仍是关）。
  注释改成「这一层只负责频率」；刷新成功那条日志降为 **Debug**（同一件事不该有两条 Info，
  带行数的 Info 由数据层用同一个事件名 `lease.stats.refreshed` 打出）。
- `internal/store/stats_test.go`：新增 4 个用例（见下）。

不变的红线都守住了：不无条件 ANALYZE 全库（只 tasks）、可关（`StatsInterval < 0`）、
失败不致命、ANALYZE 不用采样（不用 analysis_limit，见 Open 里 `=0x2` 那段说明）。

## 3. 新增测试

| 用例 | 钉住什么 |
|---|---|
| `TestMaintainStatsRefreshesDriftedStat1` | #265 的证伪 + 验收同一条：100→2500(25×) 后 stat1 必须变，且接近真实行数 |
| `TestMaintainStatsKeepsQuietWhenStat1IsFresh` | 统计新鲜时**不得** ANALYZE tasks（否则 6h 任务变成「每轮全表 ANALYZE」） |
| `TestStatsDrifted` | 纯函数边界：恰好 10× 才漂移、反向漂移（表被清空）也刷新、一侧为 0 |
| `TestMaintainStatsSkipsDriftCheckWhenTableEmpty` | 空表没有可比基线，不白跑 ANALYZE |

## 4. 验证输出（原样）

```
### [1] go test ./internal/store/ -run 'Stats|Maintain|PendingBacklog' -count=1 -v
=== RUN   TestMaintainStatsKeepsQuietWhenStat1IsFresh
--- PASS: TestMaintainStatsKeepsQuietWhenStat1IsFresh (0.04s)
=== RUN   TestStatsDrifted
--- PASS: TestStatsDrifted (0.00s)
=== RUN   TestMaintainStatsDoesNotTouchData
--- PASS: TestMaintainStatsDoesNotTouchData (0.01s)
=== RUN   TestMaintainStatsSkipsDriftCheckWhenTableEmpty
--- PASS: TestMaintainStatsSkipsDriftCheckWhenTableEmpty (0.01s)
...
--- PASS: TestMessageStatsRejectsEmptyNamespace (0.00s)
PASS
ok  	github.com/zzjcool/anotify/internal/store	0.534s

### [2] go test ./internal/store/ ./internal/lease/ -count=1
ok  	github.com/zzjcool/anotify/internal/store	8.274s
ok  	github.com/zzjcool/anotify/internal/lease	0.757s

### [3] 稳定性：漂移用例连跑两次
=== RUN   TestMaintainStatsRefreshesDriftedStat1
    stats_test.go:153: MaintainStats 执行的语句 = [ANALYZE "tasks"]；真实行数 = 2500，基线估计 = 100
--- PASS: TestMaintainStatsRefreshesDriftedStat1 (0.22s)
=== RUN   TestMaintainStatsRefreshesDriftedStat1
    stats_test.go:153: MaintainStats 执行的语句 = [ANALYZE "tasks"]；真实行数 = 2500，基线估计 = 100
--- PASS: TestMaintainStatsRefreshesDriftedStat1 (0.20s)
PASS
ok  	github.com/zzjcool/anotify/internal/store	0.428s
```

全仓（`go vet ./...` + `go build ./...` + `go test ./... -count=1` + `make contract`）：
除下面这一条**与本次改动无关、由环境（root）导致**的既有失败外，其余全绿：

```
--- FAIL: TestHandleUpgradeNeedRootDoesNotExit (0.00s)
    upgrade_frame_test.go:257: ack = &{Type:upgrade_ack OK:true Version:1.5.0 Commit: Error:}, want ok=false error=need_root
```

原因：该用例 chmod 0555 后期待「目录不可写 → need_root」，但本环境 `id -u = 0`，
root 无视权限位。单跑同样失败，与 store/lease 无关（未改 `cmd/agent`）。
`make contract` 通过（A2/A4/A5/A6/A7 + openapi 69 路由）。

## 5. 交付

- commit `8af2fcc`，分支 `power`（已 push）
- PR（分支 power 的既有 PR）：https://github.com/zzjcool/anotify/pull/309
- 未自行 merge

## 6. 「100 万行验收」怎么做（建议放 scripts/perf，单测不做）

单测里造 100 万行会让 store 包测试从 8s 涨到分钟级，不适合当门禁。建议
`scripts/perf/265/s1-stats-drift.sh` + 一个 `go run ./scripts/perf/265` 小工具：

1. **造数**：空库（`store.Open`）→ 先 `ANALYZE "tasks"` 取基线；用批量直插
   （BEGIN IMMEDIATE + 每批 1000 行，`PRAGMA journal_mode=WAL`）灌到基线 ×10 以上，
   规模建议 **基线 10 万 → 终值 100 万**（这样「10× 阈值」和「百万行成本」一起验到）。
2. **漂移前后**：`SELECT stat FROM sqlite_stat1 WHERE tbl='tasks'`（记首位）；
   `EXPLAIN QUERY PLAN SELECT ... FROM tasks WHERE namespace_id=? AND status='pending' ORDER BY created_at`
   看它用的是 `idx_tasks_ns_created` 还是被稀有值抢到别的索引。
3. **跑一次维护**：`db.MaintainStats(ctx)` 直接计时（这才是要测的成本），
   断言 (i) stat1 从 10 万 → ≈100 万；(ii) 耗时（预期秒级，记进报告）；
   (iii) 期间并发写入是否出现 `database is locked (5)`。
   端到端版本：`ANOTIFY_STATS_INTERVAL_SEC=60` 起 server（配置已在 `internal/server/config.go`），
   从日志里抓 `event=lease.stats.refreshed`，看 `prev_rows`/`cur_rows`。
4. **用户价值**：同一列表查询在刷新前后的 p50/p95（这是"索引建好了却慢"的直接指标）。
5. 建议另测**「清理后反向漂移」**：删掉 90% 行（保留清理的常态）→ 下一次维护应重新 ANALYZE
   （`statsDrifted` 反向分支），否则规划器会按旧的高估计选错计划。

## 7. 遗留风险

- `ANALYZE "tasks"` 是**全量**分析（刻意不采样），百万行表上是一次秒级写事务，
  期间写者排在 busy_timeout(5000) 后面。触发条件是 ≥10× 漂移（缓慢、不常发生）、
  上限 6h 一次，刷新完 stat1 新鲜、下一轮自己静默 —— 但它确实是「偶发写停顿」的一个来源，
  若要更保守可以把阈值从 10× 提到 25×，或给它单独的低频门（比 6h 更长）。
- 6h 维护的**第一次**心跳固定多一条 `count(*) FROM tasks`（走最小索引，百万行 ~几十毫秒）。
- 事件名 `lease.stats.refreshed` 现在由 store 层打（漂移触发）——语义上"租约层的事件"
  出现在数据层，用 `trigger=open|maintenance` 区分来源；如果要更严格，可改成
  `store.stats.drift_refreshed` 并在 lease 侧保留 Info（会有两条日志）。
