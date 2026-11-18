---
id: 005
kind: bug
area: [server]
size: S
severity: P2
state: done
needs_recon: false
contract: []
depends_on: []
route: [worker]
gates: ["go test ./internal/server/... -count=1", "go test ./internal/store/ -run Retention -count=1", "make e2e-one S=capacity"]
opened: 2026-11-16
closed: 2026-11-16
---

# 005 · `ANOTIFY_EVENT_RETENTION_DAYS` 不生效 —— 只进启动日志，实际 TTL 来自 `system_config`

## 现象

`ANOTIFY_EVENT_RETENTION_DAYS` 在文档里被写成「events 保留天数」：

- `DEVELOPMENT.md:761`：`ANOTIFY_EVENT_RETENTION_DAYS | 30 | events 保留天数；0 = 永久保留`
- `docs/perf/capacity-and-backpressure.md:278`：同上，还写了钳位规则
- `scripts/perf/c/s6-capacity.sh:193,304,320,419`：靠它切「retention=0（永久）/ retention=1（一天）」

但代码里它**只被读来打日志**：

- 解析：`internal/server/config.go:408`（`Config.EventRetentionSec()`）、`config.go:254-285`
- 唯一消费点：`internal/server/mux.go:120-124`（启动时 `slog` 打 `events_retention_sec`）
- 真正决定 TTL 的是 `store.system_config`：`RunRetention` 每轮 `GetSystemConfig`（`retention.go:71`）
- 而 `system_config` 的**唯一写者**是 `PutSystemConfig`（`store/config.go:381`），
  调用点只有 HTTP `PUT /v1/system/config`（`resource_namespace.go:547`）。
  全仓没有「启动期用 env 播种 system_config」的路径（`grep system_config scripts/dev.sh cmd/devseed/*.go` 为空）。

⇒ 运维把 env 改成 90 天，重启后日志说 90 天，**实际仍在用 `system_config` 的默认 30 天**。

## 为什么现在才发现

这是 v4 换掉保留期配置来源时留下的**双轨残留**：配置搬到了 `system_config` + API，
env 那套没删、也没改成「无害别名」，文档和 perf 脚本继续按旧语义用它。

## 影响

1. **运维误判**：以为改了保留期，磁盘仍按 30 天删（或反之，以为永久保留却仍在删）。
2. **perf 结论的口径**（需 owner 确认）：`s6-capacity.sh` 的 b/c 段用 env 切 retention=0/1。
   若 env 实为空转，那两段的「retention=1」其实是默认 30 天 —— 40 天前的种子数据照样被删，
   所以**断言仍然通过、数字看似成立**，但「保留期 1 天」这个前提是错的。
   这对 `docs/perf/capacity-and-backpressure.md` 的结论口径是可追溯性缺口，不是数值必然错。

## 修法（二选一，建议 B）

| 选项 | 做法 | 代价 |
|---|---|---|
| A. 让 env 生效 | 首次 `Open` 且 `system_config` 无行时，用 env 播种 | 改 store（启动期写库 + 幂等）+ 新测试；与「保留期配置在 system_config」的双源问题**永久存在** |
| **B. 承认它已退役** | 改名/删除 env；文档改写成「保留期用 `PUT /v1/system/config`」；perf 脚本改用 `PUT`（或直接 `sqlite3` 写 `system_config`，perf 脚本已有库路径） | 文档 3 处 + 脚本 1 处；不动 schema、不动 API |

建议 B：保留期是**运行时可改**的配置（`GET/PUT /v1/system/config`），再留一个只在启动时生效的 env 就是双源。
若为了兼容已部署脚本而保留 env，至少要：启动日志显式写「本 env 已不生效，实际值来自 system_config」+ 文档标注 deprecated。

## 验收

- `grep -rn 'ANOTIFY_EVENT_RETENTION_DAYS'` 的每一处都对应「已退役」或「真的生效」，没有第三种。
- 若选 B：`make ci` 不新增红；perf 脚本仍能切 retention（改用 `PUT /v1/system/config` 或直接写库）。
- 若选 A：新增测试证明「首次 Open 用 env 播种」「已有行时 env 不覆盖」两条。

## 冻结方案（协调者，2026-11-16）

选 **A′：env 只做「首次播种」，不做双源覆盖**。理由：保留运维已有的部署习惯（env）、不删 4 个已存在的解析测试，
同时让 `system_config` 继续是唯一运行期源（与 V4-D6 一致）。

### 改动清单

1. **`internal/store/config.go`** 新增：

```go
// SeedSystemConfigIfEmpty 在 system_config 无行时写入给定保留期（幂等）。
// 已有行 → 不动、返回 false。运行期改配置的唯一路径仍是 PutSystemConfig。
func (d *DB) SeedSystemConfigIfEmpty(ctx context.Context, cfg *SystemConfig) (bool, error)
```

- 实现为**单条** `INSERT OR IGNORE`（并发启动天然安全），`resource_version` 写 1，`updated_at` 按同文件惯例。
- `nil` / 负数 → 报错且不写（口径照 `PutSystemConfig`）。
- 返回 `RowsAffected()==1` 作为「确实播种了」。

2. **`internal/server/config.go`** 加一个字段（不动解析层）：

```go
// EventRetentionDaysSet 表示 ANOTIFY_EVENT_RETENTION_DAYS 被显式设置过。
// 只有显式设置才播种 system_config；未设置 = 不写库，保持「无行 = 默认值」。
EventRetentionDaysSet bool
```

3. **`internal/server/mux.go`**（`store.Open` 之后、现有 retention 日志之前）：

- 若 `cfg.EventRetentionDaysSet`：
  `db.SeedSystemConfigIfEmpty(ctx, &store.SystemConfig{RetentionEventTTLSec: int64(cfg.EventRetentionSec()), RetentionTaskTTLSec: store.DefaultRetentionTaskTTLSec, RetentionResultTTLSec: store.DefaultRetentionResultTTLSec})`
  - seeded → `server.retention.seeded_from_env`（Info）
  - 已有行 → `server.retention.env_ignored`（Warn，hint 指向 `PUT /v1/system/config`）
  - 失败 → `server.retention.seed_failed`（Warn，**不阻断启动**）
- **现有日志改成报「生效值」**：读 `db.GetSystemConfig(ctx)` 的 `RetentionEventTTLSec` 判configured/permanent 分支，
  并加 `source` 字段（`system_config` / `env_seed` / `default`）。
  **不得**再把 `cfg.EventRetentionDays` 当生效值打印。
  `event` 名（`server.retention.configured` / `server.retention.permanent`）与关键字段
  （`events_retention_sec`、`est_bytes_per_event`、`hint`）保持不变——文档引用了它们。

4. **测试**
   - `internal/store`：`TestSeedSystemConfigIfEmpty` —— ①空库→true 且 `GetSystemConfig` 读回一致、rv=1；
     ②再调一次→false 且值不变；③先 `PutSystemConfig` 改过→seed 不动它；④nil/负数→报错且零写入。
   - `internal/server`：`TestEventRetentionDaysSet` —— 未设→false；`"0"`→true；`""`→false。
   - 既有 4 个 `config_test.go` 断言**不变**。

5. **文档**（只加一句「只播种」的限定）：
   - `DEVELOPMENT.md:761`
   - `docs/perf/capacity-and-backpressure.md:278`
   写法：「仅在首次启动且 `system_config` 无行时播种；之后运行期改保留期用 `PUT /v1/system/config`」。

6. **不动 `scripts/perf/c/s6-capacity.sh`**：它每次都用全新 dbdir（`pc_start` 里 `rm -rf`），
   所以每轮都会播种 → 脚本按原意生效。但需要在交付说明里标注：**perf 数字的口径可能需复测**
   （此前实际按 30 天跑，现在真按 `retention=0/1` 跑）。

7. **门禁**：`go test ./internal/server/... ./internal/store/ -count=1`、`make contract`、`gofmt -l .`。

### 不做

- 不删 `ANOTIFY_EVENT_RETENTION_DAYS` 的解析与 4 个测试（运维习惯 + 已有契约）。
- 不让 env 覆盖已有 `system_config`（那就是双源）。
- 不动 `system_config` 的 schema（不加列）。

## 已修（`bb53ee2` 代码 / `8a4fa16` 报告）

按冻结方案 A′ 落地，**未触碰** `schema.sql` / `migrate.go` / `scripts/perf/c/s6-capacity.sh`。

| 文件 | 改动 |
|---|---|
| `internal/store/config.go` | 新增 `SeedSystemConfigIfEmpty`：单条 `INSERT OR IGNORE`（并发安全）→ `resource_version=1`；nil/负数 → `ErrConfigInvalid` 且零写入 |
| `internal/server/config.go` | 新增 `EventRetentionDaysSet`（env 是否显式设置）+ `retentionDaysSetFromEnv`；解析层与既有 4 个断言**未改** |
| `internal/server/mux.go` | `reportEventRetention`：空库播种 → 报**生效值**（读 `system_config`），新增 `source` 字段；三条日志 `seeded_from_env` / `env_ignored` / `seed_failed`，播种失败只 WARN |
| `internal/store/config_seed_test.go` | `TestSeedSystemConfigIfEmpty`：往返+rv / 幂等 / PUT 后不被覆盖 / 非法值零写入 |
| `internal/server/retention_seed_test.go` | `TestEventRetentionDaysSet` + `TestReportEventRetentionWiring`（6 子用例，抓 JSON 日志断言「报的是生效值不是 env 回显」） |
| `DEVELOPMENT.md` / `docs/perf/capacity-and-backpressure.md` | 环境变量表补「只播种」限定 |
| `scripts/e2e/suites/capacity.mjs` | **修掉一条把 bug 当成期望的断言**（原 `:474` 写「env=0 不改变 system_config」），换成新契约 + 新增 `seeded_from_env` 日志断言 |

### 验证（原样输出）

```
$ gofmt -l .                                                      → 无输出
$ go test ./internal/store/ -run 'Seed|Retention|Config' -count=1  → ok 0.230s
$ go test ./internal/server/ -count=1                             → ok 52.962s
$ make contract                                                   → 全通过（125 ops / 229 schema / 27 表）
$ make e2e-one S=capacity                                         → 84 通过 / 0 失败（147.5s）
    ✅ 启动日志有 server.retention.seeded_from_env（env 确实播种了）
    ✅ env=0 播种进 system_config：eventTtlSec=0（永久保留）
    ✅ 播种只写 events：task 保留期仍是 store 默认 30 天
    ✅ 对照：把保留期从 0 改成 1s 后，同一条 40 天前事件被删（巡检确实读系统配置）
$ go test ./... -count=1  → 仅两处基线红：cmd/anode（无 sudo）、internal/sitegen（docs 页）
```

### 行为对照

| 场景 | 修前 | 修后 |
|---|---|---|
| 空库 + env=90d | 日志说 90 天，实际 30 天 | 播种 90 天进库，日志 `source=env_seed` |
| 有行 + env=90d | 日志说 90 天，实际按库 | 不动库，WARN `env_ignored`，日志报库里的生效值 |
| 未设 env | 默认 30 天 | **不写库**（保持「无行 = 默认值」），日志 `source=default` |

### 遗留风险

- `scripts/perf/c/s6-capacity.sh` 现在**真的**按 `retention=0/1` 跑（以前实际是 30 天），
  `docs/perf/capacity-and-backpressure.md` 的 s6 b/c 段数字口径建议复测。
- 其他 e2e 套件若从外部环境继承到该 env，会播种 —— 目前只有 capacity 套件设它。

## 不做

- 不顺手改 retention 的默认值（30 天是用户拍板的，见 `internal/server/config.go:21-22`）。
