## 现象

`docs/perf/` 里一批结论停留在 #240（连接池 1→8）、#242（语句缓存）、#249（latency 单位）、#270（abandon）之前，
读起来像现状，实际已经推翻。`git log 5ba1c81..HEAD -- docs/perf/` 为空 —— #277 那批改了代码没回写文档。

## 具体条目

| 文件:行 | 现文 | 事实 |
|---|---|---|
| `docs/perf/capacity-and-backpressure.md:257-258` | 「全仓**没有任何 abandon 路径**，所以这些行永远停在 pending」 | 已有 `POST /v1/tasks/{id}/abandon`（`internal/server/mux.go:411` → `AbandonPendingTask`，`internal/store/orphan.go:161`，CAS 只动 pending）；巡检侧另有自动 abandon（attempts 用尽 / 能力非幂等，`store/task.go:1371,1382`）。**刻意不做自动 abandon**（`orphan.go:26-27`）≠ 没有路径 |
| `:380-382` | 「没有 abandon 路径 … 安全阀不会替你清掉那条任务」 | 同上；`AbandonPendingTask` 就是「替你清」 |
| `:185-186` | 「迁移 17 建了部分索引 `idx_tasks_pending_created_at`」 | 迁移链已 squash 成单 baseline（`000a633`），该索引在 `schema.sql:415`，「迁移 17」不存在 |
| `docs/perf/path-a-http-api-store.md:13-15` | 「`store.Open` 把连接池**钉成 1 条**」 | 默认 8（`store.go:38` `defaultMaxOpenConns`）+ `ANOTIFY_DB_MAX_CONNS`（`store.go:41`） |
| `:21` | 「同一条 SQL 每请求重新 parse（26% 读 CPU）」 | #242 已加 per-DB 语句缓存（`store.go:46-49`） |
| `:94-102`、`:171-185` | 整段「串行瓶颈的取证（`SetMaxOpenConns(1)` 成立）」 | 前提已随 #240 消失；数字可留作历史，结论句必须标「#240 已修复」 |
| `:212` | 「连接池放开后 … 属后续调优」 | 已放开并默认 8 |
| `:215-216` | latency 单位 bug「未开 issue，建议另开一条」 | 已修（`middleware.go:99`，护栏 `middleware_latency_test.go:23`），issue #249 已关 |

## 还有一处代码注释自相矛盾（同批）

`internal/store/store.go:136-141` 写「真要做运行期刷新，得周期性在运行期跑 optimize——**那是另一个 issue，不在本批范围**」，
而同一文件 `:197-271` 就是该实现（`MaintainStats`），`lease/stats.go:40` 每 6h 调它。注释应指向 #265。

## 处置

- 只改**结论句**，不删历史数字（报告是当时的记录）；需要时在段首加「> 已被 #xxx 修复」。
- `scripts/perf/*/REPORT.md` 与 `docs/perf/c-report.md` 的测量数字不动。
- 不要在本次顺手复测（成本高、且需要干净机器）。

## 验收

- 全目录不再出现「没有任何 abandon 路径」「连接池钉成 1 条」「未开 issue」这类断言
- `make contract` 通过（文档本身不被机器校验，但 CONTRACT.md 同批改动要过）
