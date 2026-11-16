---
id: 003
kind: bug
area: [task]
size: L
severity: P2
state: done
needs_recon: false
contract: []
depends_on: []
route: [worker]
gates: ["go test ./internal/server/... ./internal/store/... -count=1", "make contract", "make e2e-one S=orphans"]
opened: 2026-11-16
recon: 2026-11-16
---

# 003 · 三条「删除/回收」原语都没有生产调用点 —— deployments 与能力 tombstone 只增不减

> 侦察已完成，全部事实带 `file:line`：见 [`003-recon-tombstone.md`](003-recon-tombstone.md)（301 行）。
> 本卡只写结论、不变量、裁决点；原文的「只修 `PurgeDeletedCapabilities`」在侦察后被修正为「三个同族缺口」。

## 结论（比原卡更大）

| 原语 | 位置 | 生产调用点 |
|---|---|---|
| `PurgeDeletedCapabilities` | `internal/store/retention.go:317` | **无**（只有 `v4test/*`） |
| `DeleteExpiredPlaybookInstalls` | `internal/store/retention.go:246` | **无**（只有测试） |
| `DeleteDeployment` | `internal/store/deployment.go:444` | **无**（全仓只有定义） |

三层后果：

1. `capability_versions` 的 tombstone 永不被物理回收；
2. `playbook_installs` 回执永不回收 → purge 判据 5（`installed_from`）永久保护这些能力的 tombstone；
3. `deployments` 行永不删（只被 supersede）→ purge 判据 4 永久保护**部署过**的能力。

**实测（本机遗留 v4 开发库 10 行样本）**：唯一的 tombstone 正是被 `deployments` 挡住的。
⇒ 只把 purge 接上线，收益仅限「从未部署、从未跑过、从未被安装」的能力。这条必须先讲清，
否则会以为「接上就解决了」。

## 量化

- `capability_versions` 12 列 / 7 个 TEXT；软删**保留整份 spec**（含 `run.files` 脚本）。
  实测平均 **1.1 KB/行**（min 258 / avg 891 / max 2042 字节；样本只有 10 行，非权威容量数字）。
- 删除一次能力 = 把该 name 的**所有 live 版本原地打 `deleted_at`**，不新增行。
- 但 partial 唯一索引只约束 live 行，且布局校验**明确拒绝**「tombstone 唯一索引」（`v4test/format_test.go:1404`）
  ⇒ `删除 → 同名重建 → 再删除` 会留下**同名同版多条 tombstone**，各占一份完整 spec。

## 一个对外语义会被 purge 改掉（必须在卡里钉住）

`DELETE /v1/namespaces/{ns}`（非 force）的阻塞计数 `capabilities` 数的是
`capability_versions` **全部行**（`internal/store/namespace.go:420`，**没有** `deleted_at` 过滤）：

```sql
SELECT count(*) FROM capability_versions WHERE namespace_id = ?
```

所以「已删能力仍会挡住命名空间删除」是现状；purge 之后同一请求会从 **409 变 200**。
这一条必须在测试里显式覆盖，不能当成附带效果。

## 不能被破坏的不变量

- **判据 1（tasks）有 FK `ON DELETE RESTRICT` 兜底** → 漏判会硬报错（`schema.sql:377`）；
  **判据 4（deployments）是 `ON DELETE CASCADE`** → 漏判会**静默删掉部署行**（`schema.sql:539`），
  节点侧表现为「内容突然不推了」。两者失效模式不对称，测试要分开设计。
- 判据 2/3（`task_attempts`、`task_artifact_manifests`）**没有外键**，全靠那条 SQL 自己（`schema.sql:501`、`:644`）。
- 三处「能力行缺失时**静默降级**」：`lifecycleOn`（`task.go:1356`）、`maxPerNodeOn`（`execution_auth.go:115`）、
  `taskLifecycle`（`task.go:748`）。放宽判据不会报错，只会把幂等/预算悄悄换成默认值 —— 最难查的一类回归。
- 判据 5 的 `json_tree` 分支在真实数据上**恒不命中**（`playbook_installs.receipt` 里是 `name@version`，不含 UID）；
  真正的保护是 `installed_from`。

## 性能陷阱（挂错地方就会抢写锁）

实测 `EXPLAIN QUERY PLAN`：`capability_versions` **全扫**（没有以 `deleted_at` 起头的索引）+
`tasks` / `task_attempts` / `task_artifact_manifests` 三次**相关全扫**（`capability_id` 无索引）+
`json_tree` 虚拟表 + `USE TEMP B-TREE FOR ORDER BY`，整段在 `BEGIN IMMEDIATE` 写事务里。
挂进 `RunRetention`（30s 一轮）= 每 30s 在写锁内做 4 次全扫，与事件入口抢锁。

## 裁决（协调者代行，2026-11-16）

用户授权代行裁决。依据：仓库哲学「不改产品语义、不做比做错好」+ 侦察事实。

| # | 裁决 | 理由 |
|---|---|---|
| 1 | **不加时间窗**（无引用即删） | 判据已经保证「无引用才删」；加时间窗要改 baseline + API，代价与收益不成正比。事故恢复靠备份，不靠墓碑 |
| 2 | **server 独立循环**（抄 `artifact-gc`） | 全表扫不该跟事件清理同频（30s）；独立间隔可调、`<=0` 可关、有现成关停护栏 |
| 3 | **只接 `PurgeDeletedCapabilities`；不动判据、不接另两个原语** | `deployments` 生命周期是产品语义（另开卡）；`DeleteExpiredPlaybookInstalls` 不接是因为 `retention.go:120-124` 已有父裁决「回执不自动删」 |
| 4 | **不做含退役行的历史投影** | 回收窗口保成内部实现细节，不变成对外承诺 |

**判据 3 的收益被低估了**：`task_attempts` / `task_artifact_manifests` / `emits` 对 `tasks(id)` 都是
`ON DELETE CASCADE`（`schema.sql:481,520,631,641`），而 `task_ttl` 默认 30 天——所以「跑过一次」
的能力在任务过期后**会自动变成可回收**。真正永久卡住的只有「部署过」和「被安装回执钉住」两类。

## 施工方案（冻结）

### 1. 配置（`internal/server/config.go`，逐字模访 `ArtifactGCIntervalSec`）

- 新字段 `CapabilityGCIntervalSec int`：未设/手滑 → **3600**；`0`/负数 → 0（禁用）；正数上限 86400。
- `func (c Config) CapabilityGCInterval() time.Duration`：`<=0 → 0`（= 不启 goroutine）。
- env 名：`ANOTIFY_CAPABILITY_GC_INTERVAL_SEC`。

### 2. GC 驱动器（新文件 `internal/server/capability_gc.go`）

```go
type capabilityGC struct {
    db         *store.DB
    log        *slog.Logger
    batch      int // 1000
    maxBatches int // 20（≈ 20000 行/轮，避免长写锁）
}
func newCapabilityGC(db *store.DB, log *slog.Logger) *capabilityGC
func (g *capabilityGC) RunOnce(ctx context.Context) (int64, error) // 多批直到返回 0 或预算用尽
func (g *capabilityGC) Run(ctx context.Context, interval time.Duration) error // ticker；单轮失败只 Warn；ctx 取消干净退出
```

- 每批之间检查 `ctx.Err()`；删除总数 >0 时打一条 `Info`（`event=server.capability.gc.deleted`）。
- 单批错误 → 返回已删数 + 包装错误（丢掉已删数会让运维以为没删）。

### 3. 接线（`internal/server/mux.go`，紧接 artifact-gc 那块之后）

```go
if cfg.CapabilityGCInterval() > 0 {
    gc := newCapabilityGC(db, slog.Default())
    app.runBackgroundLoop(ctx, "capability-gc", func(loopCtx context.Context) error {
        return gc.Run(loopCtx, cfg.CapabilityGCInterval())
    })
}
```

必须走 `runBackgroundLoop`（它已挂 `trackBackground`），否则绕过 `WaitBackground` 关停屏障。

### 4. 测试

- `internal/server/capability_gc_test.go`：
  a) 无 tombstone → `RunOnce` 返回 0、无错；
  b) 软删一个能力（无引用）→ `RunOnce` 删掉（返回 >0），再跑一次返回 0；
  c) 有引用（task 或 deployment 钉住）→ 不删；
  d) `ctx` 已取消时 `RunOnce` 立即返回、不删；
  e) `WaitBackground` 收尾不撞已关库（照 `artifact_gc_test.go` 的写法）。
- `internal/server/config_test.go`（或新文件）：`CapabilityGCIntervalSec` 解析（未设 / `"0"` / 负数 / 非数字 / 超大）。
- 判据本身的覆盖已有（`internal/store/v4test/reference_*`），不重复。

### 5. 文档

- `DEVELOPMENT.md` §10 环境变量表加一行 `ANOTIFY_CAPABILITY_GC_INTERVAL_SEC | 3600 | 已删能力（tombstone）物理回收周期；0 = 禁用`。
- `docs/design/v4/capability.md` 删除那一节（`844` 附近）补一句：tombstone 的物理回收由后台 GC 驱动；
  有引用（任务/尝试/产物清单/部署/安装回执）的不删 —— **因此「已删能力会挡住命名空间删除」不是契约保证**。

### 6. 不做

- 不接 `DeleteDeployment` / `DeleteExpiredPlaybookInstalls`（前者另开卡，后者已有父裁决）。
- 不加时间窗、不动 schema、不改 `PurgeDeletedCapabilities` 的 SQL 与签名。

### 7. 门禁

```bash
gofmt -l .
go test ./internal/server/... ./internal/store/... -count=1
make contract
make e2e-one S=orphans          # 覆盖 no_capability 分支
```

## 不做

- 不改成硬删：`deleted_at` 是对外语义的一部分（删除后 `/versions` 恒 404 靠它）。
- 不在本卡里动 `api/*.yaml`（除非裁决选了 1-B 或 4-B）。

## 交付（提交见本卡同批 commit）

- 实施：`internal/server/capability_gc.go`（新）+ `mux.go` 接线 + `config.go` 周期 + 9 条驱动测试 + 2 条配置测试
- 文档：`DEVELOPMENT.md` §10 环境变量表 + `docs/design/v4/capability.md` §8.4
- 验证：`go test ./internal/server/... ./internal/store/... -count=1` 全绿；`make contract` 通过；`make e2e-one S=orphans` 98/0
- 语义变化（已测）：tombstone 被回收后 `DELETE /v1/namespaces/{ns}` 从 409 变 200（`TestCapabilityGCPurgeFlipsNamespaceDeleteFromBlockedToAllowed`）
- 未做：`DeleteDeployment` / `DeleteExpiredPlaybookInstalls` 仍未接（裁决 3 有意保留）；启动不立即补扫（首轮等一个 tick）
