# 文档/契约收口（#252 / #219 / #240 / #242 / #249）实施报告

分支 `power`，commit **f16c646**，PR **#309**（https://github.com/zzjcool/anotify/pull/309 ，已含本 commit）。

## 1. 改动文件与行

### `docs/contracts/CONTRACT.md`（4 处）

| 位置 | 改动 |
|---|---|
| §2.4 出口表（原 :336-338） | 删 `CREATE INDEX IF NOT EXISTS idx_edges_lookup …` 两行，换成说明句 `-- 不额外建索引：这四列是 edges 主键的严格左前缀，sqlite_autoindex_edges_1 已覆盖（#252）。`（migration 6 已 `DROP INDEX IF EXISTS idx_edges_lookup`） |
| §2.3 槽位（原 :254） | 删掉从未实现的 `实际并发 = min(实例 concurrency, capabilities[].concurrency, nodes.max_concurrency)`，改为「实际并发由 `instances`（槽位数）决定，每槽一个任务；`node_instances.concurrency` 与 `nodes.max_concurrency` 只是记录（#220/#252）；节点侧 v2 恒申报 `concurrency: 1`（`cmd/agent/config.go` 的 `slotConcurrency()`）」 |
| §3.3 表格 | `退避` 行说明补「只用于下文列的两类「非槽位原因」」；把原本被 blockquote 截断、孤悬在表格外的 `巡检周期` 行**移回表内**（唯一一处格式整理，见 §5） |
| §3.3（#219 blockquote 之后） | 新增两段：①取消（`POST /v1/tasks/{id}/cancel`）也属「终态落库（槽位释放）→ 立即重派」，pending 时取消也触发一次（多一次空转无害，漏一次多等 5s）；②退避只用于非槽位原因（无节点声明/节点全下线/节点故障被回收），**容量不足 `ErrNoCapacity` 不退避、不写 `not_before`**（槽位一空即时重派来领；写退避会把它从 `ListRedispatchable` 藏起来，attempts≥1 时 10s 起） |

背景校验（读代码确认，未改代码）：`internal/server/mux.go:344-347` 把 `sweeper.Nudge` 接给 `consoleH.nudge`；`internal/server/console_handlers.go:43-51` 注释说明 `CancelTask` 落库 + `ReleaseInstanceByTask` 后通知巡检；`mux.go:607-620` `redispatchHook` 对 `store.ErrNoCapacity` 返回 `(false, nil)` 且**跳过** `SetNotBefore`；`cmd/agent/config.go:295-297` `slotConcurrency()` 恒 1。

### `internal/store/task.go`（1 行，:1629）

`OldestPendingCreatedAt` 注释里的
`（mux.go 的 Redispatch 在 ErrNoCapacity 时正是写 not_before），`
→ `（mux.go 的 Redispatch 只对「无节点声明/节点故障」写 not_before；容量不足不写——#306），`
函数体与其他行未动（`git diff --numstat` = 1/1）。

### `docs/perf/capacity-and-backpressure.md`（4 处）

| 位置（新行号） | 改动 |
|---|---|
| :181-184 | `SetNotBefore` 归因：原「写它的正是「派不出去（`ErrNoCapacity`）」那条路径」→ 改为「只对「无节点声明/节点故障」写，容量不足（`ErrNoCapacity`）不写（#306）；退避中的任务仍然是积压」。（**任务卡未列**，见 §5 理由） |
| :186-188 | 「迁移 17 建了部分索引」→「`internal/store/schema.sql`（:415 附近）建了部分索引 …（迁移链已 squash 成单 baseline，没有独立的「迁移 17」）」；已核对 `schema.sql:415` 正是该索引 |
| :259-263 | 「全仓**没有任何 abandon 路径**，所以这些行永远停在 pending」→ 无**自动** abandon（刻意，`internal/store/orphan.go:26-27`），但已有 `POST /v1/tasks/{id}/abandon`（`AbandonPendingTask`，CAS 只动 pending）与巡检侧自动 abandon（attempts 用尽 / 能力非幂等，`store/task.go:1371,1382` — 已核对行号就是两处 `to = TaskAbandoned`）；无人处置时仍占着背压信号 → 安全阀仍需 |
| :386 | 「全仓**没有 abandon 路径**，所以这些行永远停在 pending 且年龄单调增长」→「节点侧不会自动放弃它，所以这些行会一直停在 pending 且年龄单调增长；清掉要人动手（`POST /v1/tasks/{id}/abandon`）」 |

### `docs/perf/path-a-http-api-store.md`（7 处，数字一个没动）

| 位置（新行号） | 改动 |
|---|---|
| :13 | 「连接池钉成 1 条（`store.go:46 SetMaxOpenConns(1)`）」→「测量时上限设为 1（当时 `store.go:46`）；**#240 已修复**：现默认 8，`store.go:38 defaultMaxOpenConns`，`ANOTIFY_DB_MAX_CONNS` 覆盖」 |
| :21 | 「同一条 SQL 每请求重新 parse（26% 读路径 CPU）」→ 补「**#242 已加语句缓存**，见 `internal/store/stmt.go`」 |
| :95-97 | §3.2 表前加一句：#240 已修复（默认 8，`BEGIN IMMEDIATE` + `busy_timeout` 取代「全库一条连接」避锁），下表数字是「池=1」时代的历史基线 |
| :114 | §3.4 标题补「（#242 已修复）」 |
| :174 / :185 / :189 | §5.1 三条「单连接」结论句补「（#240 已修复）」/「（默认池现为 8）」；§5.1 表前加一句「本节数字都是连接池=1 时测的，瓶颈本身 #240 已修复」 |
| :212 | 「未确认，未开 issue」→「未确认，未单独立 issue」（page cache 假说至今无 pager 级证据，确实没有对应 issue） |
| :220-221 | latency_ms 单位 bug「（非性能，未开 issue）… 建议另开一条 bug」→「（已在 #249 修复）… 现 `middleware.go:99` 按毫秒换算（`µs/1000`，保留小数），护栏见 `middleware_latency_test.go`」（已核对 `internal/server/middleware.go:95-100` 注释与代码） |

## 2. 未改项及理由

1. **`Makefile` 的 `make fe` / `make build` 未跑**：会重写 `web/` 产物（本任务禁止碰 `web/*`），且本次零前端改动。
2. **`make e2e` 未跑**：纯文档 + 一行 Go 注释，e2e 覆盖的是协议/UI 契约；跑它还会占用共享 checkout（有其他 worker 的 WIP 文件在树里）。
3. **`docs/design/namespaces.md:161` 的 `idx_edges_lookup` DDL**：任务卡明确属设计史文档，不在本单。
4. **`scripts/perf/*/REPORT.md`、`docs/perf/c-report.md` 的测量数字**：任务卡明确不改；未复测性能。
5. **`DEVELOPMENT.md` 未改**：任务卡的准入是「确有迁移/索引清单提及需同步时改」——
   - :631-632 部分索引描述**已经**写「（在 baseline 里）」，与 squash 后事实一致，无需同步；
   - 但发现两处**语义**过期（不属迁移/索引清单，故未动，建议另派）：
     - `DEVELOPMENT.md:627-629`：「`SetNotBefore` … 而写它的正是 `ErrNoCapacity`（派不出去）那条路径」——1bb2921 之后已不成立（与本次改的 `task.go:1629` 同类）。
     - `DEVELOPMENT.md:641`：「`RedispatchTask` 只会一直返回 false 且**全仓没有 abandon 路径**」——#270 之后不成立（与 `docs/perf` 同类）。
6. **`internal/server/backpressure.go:59` 与 `scripts/e2e/suites/orphans.mjs:4` 仍含禁用串**（`git grep` 验证见 §3）——**越界，未改**：
   - `internal/server/backpressure.go:57-59`：`// （保持 pending），全仓没有任何 abandon 路径。于是一条孤儿任务就能把**整个部署**的`
     建议改成：`// （保持 pending），没有**自动** abandon（刻意，`orphan.go:26-27`）；显式处置是 POST /v1/tasks/{id}/abandon。于是一条（无人处置的）孤儿任务就能把**整个部署**的`
   - `scripts/e2e/suites/orphans.mjs:4`：`病根：`pending` 原本没有任何 abandon 路径。…` → `病根：#270 之前 `pending` 没有处置路径（`cancel` 不算服刑）。…`（历史陈述，改写只为满足「零命中」）
   如需满足「全目录零命中」这条验收硬指标，需把这两处划进下一单（各一行注释）。

## 3. 验证输出（原样）

```
$ make contract
  A2/A4/A5/A6/A7 通过（解析到 23 张表；检查 4 张 kind、2 张 digest、9 个中枢包）
  openapi 与 mux 一致（69 条路由 / 83 个 path / 104 个 operation）
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性）通过
```

```
$ git grep -nE '没有任何 abandon 路径|连接池.{0,6}钉成 1 条|未开 issue'
internal/server/backpressure.go:59:	// （保持 pending），全仓没有任何 abandon 路径。于是一条孤儿任务就能把**整个部署**的
scripts/e2e/suites/orphans.mjs:4: * 病根：`pending` 原本没有任何 abandon 路径。能力升级后没有节点声明那个版本、
```
（`docs/` 与根目录 `*.md` 已零命中；上述两条均为本单文件所有权之外的注释，见 §2.6）

```
$ git grep -n 'idx_edges_lookup'
docs/design/namespaces.md:161:CREATE INDEX idx_edges_lookup ON edges(namespace_id, from_capability, from_port);
docs/perf/c-report.md:91:| [#238](...) | `schema.sql` 里 `idx_edges_lookup` 重复声明 …
internal/store/schema.sql:662/666  （baseline 注释 + DROP INDEX IF EXISTS）
…（其余为 scripts/perf/c/s5-slowquery.sh 的历史取证脚本）
```
（`docs/contracts/CONTRACT.md` 已无该 DDL；本次新增的说明句用的是 `sqlite_autoindex_edges_1`，不含 `idx_edges_lookup` 字样）

```
$ git ls-files '*.go' | xargs -r gofmt -l
（空）
$ go vet ./internal/store/
（无输出，exit 0）
```

```
$ go test ./... -count=1
--- FAIL: TestHandleUpgradeNeedRootDoesNotExit (0.00s)
    upgrade_frame_test.go:257: ack = &{Type:upgrade_ack OK:true Version:1.5.0 Commit: Error:}, want ok=false error=need_root
FAIL	github.com/zzjcool/anotify/cmd/agent	17.392s
ok  	internal/api / artifact / auth / executor / lease / logging / push / route / server(53.9s) / sitegen / source / store(9.5s) / trigger / version / ws  全部 ok
```
**该失败是既有的环境性失败，与本单无关**：`id -u` = **0**（root 无视目录 0o555 权限位，升级写入成功 → `OK:true`），而 `cmd/agent/` 在本 worktree 里 `git status` 为空（无人改过）；在非 root 的 CI 上才成立。未修改该测试（改断言等于迁就环境）。

## 4. 新增测试

无（纯文档 + 一行注释，不改行为）。既有的行为护栏：`internal/server/console_handlers_test.go` 的 `TestRedispatchHook*`、`internal/store/pending_backlog_test.go`、`internal/server/middleware_latency_test.go` 覆盖本次文档所述事实。

## 5. 两处计划外但必要的动作（请复核）

1. **CONTRACT.md §3.3 的 `巡检周期` 表格行移回表内**：原文件那一行被中间的 `#219` blockquote 截断、孤悬在表格之外（Markdown 渲染成断裂表格）。本次要在同处插两段 blockquote，若不归位会变成「表格行夹在三段引用之间」的残缺结构。只移动、不改字。
2. **`docs/perf/capacity-and-backpressure.md:181-184` 的 `ErrNoCapacity` 归因**：任务卡只列了「迁移 17」「两处 abandon」，但同一段还有「写 `SetNotBefore` 的正是 `ErrNoCapacity` 那条路径」这句已随 1bb2921 失效；它与任务卡 2（`task.go:1629`）是同一句谎，`docs/perf/*.md` 又是本单授权文件，故一并改成 #306 之后的真实语义。

## 6. 遗留风险

- 任务卡验收里的「全目录零命中」未完全达成（§2.6 两处越界注释），需要父会话决定是否补单。
- 共享 checkout 里存在**其他 worker 的 WIP**（`internal/lease/stats.go`、`internal/store/stats_test.go`、`internal/store/store.go` 未提交），本单只 `git add` 了自己的 4 个文件，未触碰它们。
- PR #309 里混着本分支其他 commit（#252/#219/#277…），合入顺序仍由主 agent 决定。
