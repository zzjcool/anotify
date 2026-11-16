# 侦察报告 · issue 003（能力 tombstone 无生产回收点）

**前置动作（已完成）**：第一条工具调用读了 `internal/store/AGENTS.md`（area=store 模块约定）。与本卡相关的红线：
- `§1` store **不得** import `server`/`ws`/`lease`/`auth`/`api` → 「回收由谁跑」这件事只能由 store 之外的包决定（`internal/store/AGENTS.md:14-18`）。
- `§3.2` 迁移引擎**只支持单块**；v4 未发布期加列 = 原地改 baseline（`headVersion` 保持 1），老库会被 `ErrSchemaInvalid` 具名拒绝（`internal/store/AGENTS.md:40-52`）。
- `§3.4` 事务内读必须走 `tx`；`§3.6` 写路径 `BEGIN IMMEDIATE`，连接池 8（`internal/store/AGENTS.md:56-63`）。
- `§5` 「单连接使 A 与 B 串行化」这类历史断言**已失效**（`internal/store/AGENTS.md:59-61`）——判断 purge 与并发写的关系必须按事务重新论证。

---

## Q1 · `PurgeDeletedCapabilities` 的完整契约

**签名与位置**：`internal/store/retention.go:317`；SQL 在 `retention.go:326-333`。

| 契约项 | 事实 | 证据 |
|---|---|---|
| 删哪些行 | 只删 `capability_versions` 中 `deleted_at IS NOT NULL` 的行（活行永不删；不看 name/digest） | `retention.go:327` |
| 判据 1 | 无 `tasks.capability_id` 引用 | `retention.go:328` |
| 判据 2 | 无 `task_attempts.capability_id` 引用 | `retention.go:329` |
| 判据 3 | 无 `task_artifact_manifests.capability_id` 引用 | `retention.go:330` |
| 判据 4 | 无 `deployments.capability_id` 引用 | `retention.go:331` |
| 判据 5 | 无 `playbook_installs` 引用：`p.id = c.installed_from` **或** 回执 JSON 里有任何 text 值等于该 capability id | `retention.go:332` |
| 时间窗 | **没有**。无 `cutoff` 参数：只要「无引用 + 已软删」就立刻可删 | 签名 `retention.go:317` |
| `limit` 语义 | `limit<=0` → 直接返回 `(0,nil)`（少删好于删全表，同族约定）；`LIMIT ?` 单条语句最多删 N 行 | `retention.go:318-320`、`:333` |
| 顺序 | `ORDER BY c.deleted_at, c.id`（按删除时刻 FIFO，积压时下一批从更早删的开始） | `retention.go:333` |
| 幂等 | 是。重跑只会处理剩余 tombstone；无引用状态不会因上一批删除而改变 | 由判据本身推出（推断，无专门测试） |
| 事务 | `d.BeginTx`（DSN `_txlock=immediate` → `BEGIN IMMEDIATE`，写锁在事务起点拿）+ 检查与删除同一语句同一事务 | `store.go:36-38`、`store.go:77-80`、`retention.go:323-336` |
| 错误路径 | begin 错→裸 `err`；exec 错→`fmt.Errorf("purge capability references: %w", err)`；RowsAffected 错→裸 `err`；commit 错→裸 `err` | `retention.go:323`、`:334-343` |
| 与同文件风格差异 | 同文件其它 primitive 用 `defer func(){ _ = tx.Rollback() }()` 且错误都包装；本函数用 `defer tx.Rollback()` 且两个错误裸返回 | `retention.go:251` vs `:325`；`:339-341` |

**这些「被保护引用表」的外键事实（决定 purge 失效的后果形态）**：
- `tasks.capability_id … ON DELETE RESTRICT` → purge 漏判会**报硬错**（`schema.sql:377`）。
- `deployments.capability_id … ON DELETE CASCADE` → purge 漏判会**静默级联删掉部署行**（`schema.sql:539`）。
- `task_attempts.capability_id TEXT NOT NULL DEFAULT ''` **无外键**（`schema.sql:501`）。
- `task_artifact_manifests.capability_id TEXT NOT NULL` **无外键**（`schema.sql:644`）。
- 即：5 个判据里只有 1 个有外键兜底（tasks），其余 4 个全靠这条 SQL 自己（推断，来自 schema）。

**判据 5 的有效性（重要）**：`playbook_installs.receipt` 里的能力清单是 `name@version` 字符串（`internal/playbook/playbook.go:308-324` 的 `Created.Capabilities []string`，由 `store.FormatRef` 拼），**不含 capability UID** → `json_tree(...) j.value = c.id` 分支在正常数据上恒不命中，真正的保护是 `p.id = c.installed_from`（`retention.go:332`）。测试里该分支只被「直接构造」的夹具覆盖（`v4test/reference_force_test.go:234` 的注释与断言）。

---

## Q2 · 软删源头：`DeleteCapability`

`internal/store/resource_cas.go:112-151`，一个 `BEGIN IMMEDIATE` 事务内四步：

1. `getCapabilityLatestOn` 取该 name 的**最高 live 版本**并校验 CAS 令牌（`resource_cas.go:125-131`）。
2. `UPDATE … SET deleted_at=?, resource_version=resource_version+1 WHERE … id=? AND resource_version=? AND deleted_at IS NULL`（`resource_cas.go:132-133`），`RowsAffected != 1` → 冲突（`resource_cas.go:135-137`）。
3. `UPDATE … SET deleted_at=?, resource_version=resource_version+1 WHERE namespace_id=? AND name=? AND deleted_at IS NULL`——把该 name **所有 live 版本**打成 tombstone（`resource_cas.go:140-141`）。
4. `DELETE FROM edges WHERE namespace_id=? AND from_is_source=0 AND from_ref=?`——只删**自己的出边**；入边（别人 `spec.next` 指向它、源 `targets` 指向它）**保留为 missing**（`resource_cas.go:143-145`）。
5. 不物理删正文/ID/digest，不取消已授权任务（`resource_cas.go:111` 注释 + `.pi-orchestrator/v4/RESOURCE-DELETION.md:7-9`）。

**`deleted_at` 参与的索引/唯一约束**：
- `idx_cap_live_name_ver`：`UNIQUE(namespace_id,name,version) WHERE deleted_at IS NULL`（`schema.sql:270-271`）
- `idx_cap_live_digest`：`UNIQUE(namespace_id,digest) WHERE deleted_at IS NULL`（`schema.sql:272-273`）
- 非 partial、**不含** `deleted_at`：`idx_cap_name_ver(namespace_id,name,version)`（`schema.sql:275-276`）、`idx_cap_fold_key`（`schema.sql:279-280`）
- **没有任何索引以 `deleted_at` 起头** → purge 的 `WHERE deleted_at IS NOT NULL ORDER BY deleted_at,id` 没有可用索引（见 Q9 实测执行计划）。
- 布局 validator 会精确校验上述 partial 谓词文本（`migrate.go:447-459`、`migrate.go:664`；断言见 `v4test/schema_baseline_test.go:238-239,395`），并**明确拒绝**「给 tombstone 建唯一索引」的写法（`v4test/format_test.go:1404`，用例名 `active-plus-tombstone-key`）→ 语义上允许同名同版多条 tombstone 并存。

**软删后被过滤的读路径（逐个函数名）**：

| 函数 | 位置 |
|---|---|
| `getCapabilityOn`（GetCapability 按 name@ver） | `capability.go:493` |
| `getCapabilityLatestOn`（ResolveRef 的 latest 分支 / UpdateCapability / DeleteCapability 定位） | `ref.go:100` |
| `getCapabilityByIDOn`（GetCapabilityByID / SetCapabilityDisplayName / CurrentArtifact） | `capability.go:508`、`deployment.go:98-101` |
| `getCapabilityByDigestOn`（GetArtifactByDigest → 节点内容通道） | `capability.go:428` |
| `getCapabilityByFoldKeyOn`（省略 version 的创建/复用） | `capability.go:404-406` |
| `getCapabilityByFoldKeyLatestOn`（安装快照复用） | `playbook_tx.go:145-147` |
| `nextCapabilityVersionOn`（下一个版本号） | `capability.go:415` |
| `ListCapabilities` | `capability.go:556-557` |
| `ListCapabilitiesPage`（`/versions` 分页，对外顺序 name ASC / version DESC） | `capability.go:583-584` |
| `CapabilityNamespace`（按 ID 查归属；`checkCapabilityInNamespace` 的唯一调用点，且该 helper 目前**无调用者**） | `capability.go:626`、`:639` |
| `keptPlaybookObjectsOn`（删包响应里的 kept 能力清单） | `playbook_tx.go:453-455` |
| `EligibleNodes`（digest → name@version 反查） | `node.go:1237` |
| `GetDeploymentByNodeCapability` | `deployment.go:299` |
| `GetDeployment` | `deployment.go:305` |
| `ListDeployments`（`/v1/namespaces/{ns}/deployments`） | `deployment.go:370` |
| `ListDeploymentsByCapabilityName`（ability status 的 nodes[] 派生） | `deployment.go:424` |
| `ListNamespaces` 的能力计数（Namespace 列表/status） | `namespace.go:151` |

---

## Q3 · 谁还在读 tombstone

### 3.1 「过滤掉 tombstone」（删了也不会变）
上表 17 个函数全部属此类。对它们的接口语义：删除后就已经 404/不可见，purge 不改变输出。

### 3.2 「依赖 tombstone 存在」（读不到就会变形）
| 读点 | 位置 | 缺行时的行为 | 是否有 purge 判据保护 |
|---|---|---|---|
| `getPinnedCapabilityByIDOn`（`EligibleNodesForTask` 的钉住正文读） | `capability.go:543-548`、`node.go:1276` | 返回 `ErrNotFound` → 派发判定失败 → 任务退化成孤儿 | ✅ 判据 1 |
| `lifecycleOn`（grant 时钉幂等/maxAttempts） | `task.go:1356-1359` | **静默返回零值 lifecycle**（幂等 false、maxAttempts 默认） | ✅ 判据 1 |
| `maxPerNodeOn`（grant 预算） | `execution_auth.go:115-121` | **静默回落 `artifact.DefaultMaxPerNode`** | ✅ 判据 1 |
| `taskLifecycle`（assign 时读 lifecycle） | `task.go:748-755` | 静默零值 | ✅ 判据 1 |
| `ListOrphanPendingTasks` ① `NOT EXISTS(capability_versions)` | `orphan.go:83` | 任务被判 `no_capability` | ✅ 判据 1 |
| `ListOrphanPendingTasks` ② `EXISTS(capability_versions)` | `orphan.go:113` | 任务进入「能力还在但没节点」分支 | ✅ 判据 1 |
| `DeploymentsForNode`（握手补推正文） | `deployment.go:751-770` | join 掉行 → 该节点少一条待推部署 | ✅ 判据 4 |
| `ReconcileNode` 等 deployment 读（`JOIN capability_versions`） | `deployment.go:560`、`:673`、`:758` | join 掉行 | ✅ 判据 4 |
| `ListPlaybookInstalls` 的 capabilities 清单（`installed_from IN (…)`，**无** deleted 过滤） | `playbook.go:606-607` | 安装历史里少一条能力 | ✅ 判据 5 |
| `namespaceBlockingObjectsOn` 的 `capabilities` 计数（**无** deleted 过滤） | `namespace.go:420` | `DELETE /v1/namespaces/{ns}` 不再因 tombstone 返回 409 | ⚠️ 无判据（purge 会改变它，见 3.3） |

### 3.3 tombstone 被物理删除后**会变**的对外语义
1. **`DELETE /v1/namespaces/{ns}`（非 force）**：阻塞清单里 `capabilities=N` 统计的是 `capability_versions` **全部行**（`namespace.go:420`），`namespace_test.go:95` 断言格式 `capabilities=1`。所以「已删能力（tombstone）仍会挡住命名空间删除」是现状；purge 之后同一请求会从 409 变 200。线上可见差异只有状态码（`api_status.go:130-136` 只回通用 `namespace is not empty`，不回 blocking 明细）。
2. **`GET /v1/orphans`**：`no_capability` 的定义是「`tasks.capability_id` 在 `capability_versions` 里**物理查不到**」（`orphan.go:50`、`:83`）。只要有 tombstone，钉住它的 pending 任务就**不会**被报成 `no_capability`（能被派发时甚至不报孤儿）。判据 1 保护了这一点。
3. **`/v1/namespaces/{ns}/deployments`、`/versions`、`/capabilities/{name}`、节点内容通道 `/v1/node/capabilities/{digest}`、`/v1/stats`、Namespace status 能力计数**：均已在软删时就过滤 → **不变**（证据：`deployment.go:370`、`capability.go:583`、`capability.go:508`、`deployment.go:156`（→`capability.go:428`）、`store.MessageStats` 只数 events/tasks/devices）。
4. **任务详情 / trace / console events 投影**：能力身份来自 `tasks.capability` / `version` / `digest` 列（`schema.sql:379-381`），不 join 能力表 → **不变**。
5. **edges**：`to_ref`/`from_ref` 存的是名字引用（`capability.go:359-361`、`graph.go:182-184`），不是 UID → **不变**。

---

## Q4 · 生产回收通路现状

**`RunRetention`（`retention.go:66-130`）一轮做什么**：
1. `now==0 → Now()`（秒）；`GetSystemConfig` 读配置（`retention.go:69-71`）。
2. **① 终态任务**：`taskTtlSec<=0` → `TasksSkipped=true`；否则 `cutoff = now - taskTtlSec`，`drainRetention(DeleteExpiredTasks)`（`retention.go:79-94`、`:143`）。
3. **② 事件**：`eventTtlSec<=0` → `EventsSkipped=true`；否则 `drainRetention(DeleteExpiredEvents)`（引用组安全：同 `(ns,trace)` 还有任务行则不删，`retention.go:184-205`）。
4. **②' 过期投递去重**：`drainRetention(DeleteExpiredEventDeliveries)`，只清窗口不删事件（`retention.go:213-231`）。
5. **③ 回执一行都不删**（父裁决），只留显式 primitive `DeleteExpiredPlaybookInstalls`（`retention.go:118-126`、`:246`）。
6. 顺序固定「先 tasks → 后 events」，为了中断时残留方向恒定（`retention.go:58-64`）。

**批量与预算**：`DefaultRetentionBatch = 1000`（单条 DELETE 上限）、`DefaultRetentionBatches = 20`（每轮最多 20 批 → 最多 20k 行），批间检查 `ctx.Err()`，`Truncated` 表示预算用尽（`retention.go:32-35`、`:283-300`）。

**TTL 从哪来 / 默认值 / env**：
- **来源**：`system_config` 单行表，经 `GetSystemConfig`（`store/config.go:349-376`），列 `retention_event_ttl_sec / retention_task_ttl_sec / retention_result_ttl_sec`（`schema.sql:160-167`）。
- **默认值**：事件 2592000（30d）、任务 2592000（30d）、结果 604800（7d）（`store/config.go:45-47`、`schema.sql:162-164`）。
- **`0` 的语义**：永久保留、跳过清理（`retention.go:20-22`、`lease_test.go:224-256`）。
- **运维改法**：`PUT /v1/system/config`（`resource_namespace.go:505-558`，三字段**全必填**；`api/resource.yaml:2449-2469` + `SystemConfigWrite.additionalProperties: false`）。
- **env**：`ANOTIFY_EVENT_RETENTION_DAYS` 存在（`server/config.go:501`，`retentionDaysFromEnv` 语义：空/非法→30d，0/负数→永久，`config.go:254-285`），但**只用于启动日志**（唯一消费点 `mux.go:120-140`）——它**不会**写进 store 的 system_config，因此**对实际 TTL 无影响**（推断，grep 全仓 `EventRetentionSec` 只有 mux 日志与测试）。`ANOTIFY_ARTIFACT_GC_INTERVAL_SEC` 才是真正驱动后台循环的 env（`server/config.go:379-390`）。
- 任务 TTL / 结果 TTL **没有** env 覆盖。

**`lease.Sweeper`**：`DefaultInterval = 30s`（`lease.go:23`）；`RunOnce` 三阶段「租约回收 → `RunRetention` → `MaintainStats`」，互不连坐、`errors.Join` 汇总（`lease.go:55-92`，`RunRetention` 在 `lease.go:73`）；`Run` 用 ticker，单轮失败只 `Warn` 不退出（`lease.go:93-110`）。
**单次 limit**：`RunRetention` 内每批 1000、最多 20 批；`RecoverLostLeases` / `MaintainStats` 各有自己的口径（不在本卡范围）。

**巡检中途失败怎么处理**：`RunRetention` 出错时**返回部分有效的 stats + error**（`retention.go:56-60`），`Sweeper.RunOnce` 保留已完成的计数并 join 错误（`lease.go:73-78`），`Sweep.Run` 记 `Warn("lease sweep failed")` 后继续下一轮（`lease.go:104-108`）。→ 新增的 purge 若加在这里，必须保持同一约定（不得把已删行数丢掉）。

---

## Q5 · 接入点候选（三处 + 各自的收尾约束与先例）

### 候选 A：挂进 `RunRetention` 内部
- 位置：`retention.go:66-130`，最自然的位置是②之后（因为「释放 tombstone」依赖同一轮已删任务）。
- 签名天然匹配：`drainRetention` 要的就是 `func(limit int)(int64,error)`（`retention.go:283`），`PurgeDeletedCapabilities(ctx, limit)` 正好是这个形状（只是需要 ctx 闭包）。
- 收尾约束：**无需额外 WaitBackground**——它已经在 `lease` 循环里（`mux.go:366-368` 用 `app.runBackgroundLoop(ctx,"lease",…)`，`mux.go:586-599` 的 `trackBackground`/`WaitBackground` 覆盖它，`mux.go:41-63`）。
- 先例：`DeleteExpiredTasks/Events/EventDeliveries` 三个 primitive 都是这么接的（`retention.go:84-116`）。
- 需要同步：`RetentionStats` 加字段（`retention.go:38-55`；该结构只被 `lease.Report.Retention` 引用并进日志，无 HTTP 面：`lease.go:32`、`api_deps.go:26` 的 `Sweeper` 目前**无 handler 使用**）。

### 候选 B：`lease.Sweeper.RunOnce` 单独一步
- 位置：`lease.go:55-92`，在 `MaintainStats` 前后插一段，`errors.Join` 汇总。
- 收尾约束：同上（同一循环，已有 WaitBackground 保护）。
- 代价：`Report` 加字段（`lease.go:24-36`）；`lease_test.go:212,254` 的断言按字段读、不会因加字段红；但「三阶段互不连坐」的注释与结构要改（`lease.go:57-59`）。
- 先例：三阶段结构本身。

### 候选 C：`server` 的后台循环（独立节奏）
- 先例（完整可抄的一套）：产物字节 GC
  - 配置：`Config.ArtifactGCInterval()`（`server/config.go:367-377`）+ env `ANOTIFY_ARTIFACT_GC_INTERVAL_SEC`（`config.go:379-390`、`:507`），`<=0 = 不启 goroutine`。
  - 装配：`mux.go:296-341`（`if spool != nil && cfg.ArtifactGCInterval() > 0 { … app.runBackgroundLoop(ctx,"artifact-gc", gc.Run) }`），注释明确写了「不登记进 background 就等于绕过 #340 r3 的关停屏障」。
  - 循环体：`resultspool.GC.Run/RunOnce`（`internal/resultspool/gc.go:103-160`），单轮失败只 Warn、ctx 取消干净退出。
  - 收尾约束：`App.background` / `trackBackground` / `WaitBackground`（`mux.go:41-63`），关停顺序 `cancel → WaitBackground → db.Close` 的硬要求写在 `production_mux_test.go:55-67`，护栏测试 `production_mux_test.go:78`、`postclose_guard_test.go:10,222`（后台写者在关库后取库会确定性红）。
  - 测试先例：`artifact_gc_test.go:63,101,133`（直接调 RunOnce 做真实回收，不等 ticker）、`artifact_gc_test.go:201,229`（`WaitBackground` 收尾）。
- 代价：store 侧要暴露一个「一轮回收」入口供 server 调（`PurgeDeletedCapabilities` 已是公开入口，直接用即可）；多一条 goroutine 与一个 env。

---

## Q6 · 配置位

**现有保留期配置读取链**：`system_config`（单行表，`schema.sql:160-167`）→ `store.GetSystemConfig`（`store/config.go:349`）→ `RunRetention` 每轮读（`retention.go:71`，**改配置下一轮即生效，不需重启**）→ HTTP `GET/PUT /v1/system/config`（`resource_namespace.go:494-558`）→ OpenAPI `Retention` / `SystemConfig` / `SystemConfigWrite`（`api/resource.yaml:2449-2469`）。

**`0` 的语义**：= 永久保留（`retention.go:20-22`；`PutSystemConfig` 拒绝负数但接受 0：`store/config.go:389-395`；API 侧 `minimum: 0` + 三字段必填，`resource_namespace.go:529-534`）。

**有没有能力 tombstone 的现成位置**：**没有**。
- 无列：`system_config` 只有三个 retention 列（`schema.sql:160-167`；结构验收清单 `v4test/schema_baseline_test.go:149-152`；迁移引擎必需列 `migrate.go:372` 附近的 capability 列表与 system_config 项）。
- 无 env：全量 env 名单在 `server/config.go:470-514`，无能力/tombstone 相关项。
- 无 API 字段：`Retention` schema 只有三项（`api/resource.yaml:2449-2455`）。

**若加配置，两个落点**：
1. **`store.system_config` + `SystemConfig` + `GET/PUT /v1/system/config` + `api/resource.yaml`**（与 V4-D6「中枢保留配置放系统配置」一致，`docs/design/v4/decisions.md:228`）。代价：改 baseline（老库 `ErrSchemaInvalid`，`internal/store/AGENTS.md:40-52`）+ 4 处同步（schema / migrate required / schema_baseline_test / api yaml + parity `openapi_parity_test.go`）+ `SystemConfigWrite.additionalProperties:false` 会硬拒新字段，必须改 yaml。
2. **`internal/server/config.go` env + 独立循环**（抄 `ANOTIFY_ARTIFACT_GC_INTERVAL_SEC`）。代价：与 V4-D6 的措辞冲突（保留期该在系统配置里），但不动 schema、不动 API。

**另一个硬约束**：现有 purge **没有 cutoff 参数**（`retention.go:317`）。要「保留 N 天」必须改签名（加 `cutoff int64`），并同步 7 个测试调用点（`v4test/reference_force_test.go:182,191,200,234`、`v4test/reference_isolation_test.go:55,69,195`）。

---

## Q7 · 数据量

**表结构**：`capability_versions` 12 列，其中 7 个 TEXT（`schema.sql:254-267`）。

**结构性固定开销**（逐列字符长度可算）：
- `id` = `cap_` + 10 位 Unix 秒 + `_` + hash 前 8 位 + `_` + 6 位 base62 ≈ **30 字符**（`store/ids.go:120-122`、`ids.go:105-110`、`ids.go:124-130`）
- `namespace_id` = `ns_` + base62(16B) ≈ **25 字符**（`ids.go:20-28`、`ids.go:73`）
- `digest` = `sha256:` + 64 hex = **71 字符**（`artifact/artifact.go:1128`）
- `fold_key` = 同一 `hashJSON` 形态 = **71 字符**（`artifact/artifact.go:1113-1126`、`:1154-1161`）
- `installed_from` = 空 或 `pb_…`（22 字符）（`ids.go:80`）
- `spec` = 整份 canonical JSON（**含 `run.files` 的脚本文本**），tombstone 保留原文（软删只写 `deleted_at`，`resource_cas.go:132-141`）
- 其余：`version / resource_version / created_at / deleted_at` 为整数

**实测（只读 SQL，样本 = 本机遗留 v4 开发库 `/tmp/dev.db`，`PRAGMA user_version=4`）**：
```
capability_versions: 10 行
  length(spec): min 258 / avg 891.2 / max 2042 字节
  固定文本列合计（id+ns+name+display+digest+fold+installed_from）: avg 229.0 字节
  → 平均一行 ≈ 1.1 KB，主要项就是 spec
  tombstone 行数：1（说明实际运行中确实会积累）
  该 tombstone（ack-probe@1）被 deployments 保护（tasks/attempts/manifests/receipts 均为 0）
  按 purge 判据跑一次 SELECT count(*)：0（当前不可回收）
```
⚠️ 这是本机遗留库，不是仓库内基准数据；不要当权威容量数字用。

**「删一次能力新增多少 tombstone」**：**不新增行**——是把 `(ns,name)` 的所有 **live 版本行**原地打上 `deleted_at`（`resource_cas.go:140-141`）。所以 tombstone 行数 = 删除时刻该 name 的 live 版本数（通常 1，多版本 name 可 >1）。又因为 partial 唯一索引只约束 live 行、且 **tombstone 唯一索引被布局校验判为非法**（`v4test/format_test.go:1404`），`删除→同名重建→再删除` 会留下**同名同版多条 tombstone**（每轮各占一份完整 spec）。

**有没有现成 perf 脚本/文档提到这个增长**：**没有**。
- `scripts/perf/c/s2-retention.sh` 只测 events（`s2-retention.sh:1-20`）；`s6-capacity.sh` 的 679.9 B/event、1908.7 B/event 也全是 events（`mux.go:100-116`、`docs/perf/capacity-and-backpressure.md`）。
- 全仓 `docs/` 无 `tombstone`/`墓碑` 命中（只有 `internal/store/*` 与其测试）。

---

## Q8 · 现有测试锁住了什么

### `v4test/reference_force_test.go`
| 测试 | 证明什么 | 证据 |
|---|---|---|
| `TestC3bCapabilityPhysicalGCRealReferencesAndRecreation` | 真实引用（task / attempt / manifest / deployment）任一存在 → purge 返回 0，且 tombstone 整行字节不变；任务 TTL 删掉任务后仍被 **deployment** 保护；手工删掉部署行后才删掉 1 行；同名重建仍解析到新 UID、共享 digest 的活行不受影响 | `reference_force_test.go:167-208` |
| `TestC3bCapabilityGCInstallReceiptAnchor` | 有安装回执（`installed_from` 非空）→ purge 返回 0，且 `capability_versions` + `playbook_installs` 两行都还在 | `reference_force_test.go:210-238` |

### `v4test/reference_isolation_test.go`
| 测试 | 证明什么 | 证据 |
|---|---|---|
| `TestC3bGCIndependentlyProtectsTaskAttemptManifest` | 三类引用**各自独立**构成保护（用「直接改 capability_id」的损坏夹具隔离其余引用）；引用释放后 purge 恰好删 1 行；同 digest 的新 UID 不受害 | `reference_isolation_test.go:16-75` |
| `TestC3bPurgedCapabilityCannotGainLateTaskReference` | 已 purge 的 UID 不能再被迟到生产者钉住（`tasks` FK RESTRICT 关闭反方向竞态） | `reference_isolation_test.go:181-201` |

### 其它锁住「tombstone 必须在」的断言
- `resource_cas_test.go:720`：并发 delete→recreate 后 `SELECT count(*) … deleted_at IS NOT NULL` 必须 = 1 → **「删除 = 软删」本身被钉住**。
- `resource_cas_test.go:236-271`：删除后 `ResolveRef`（worker / worker@1 / worker@2）、`GetCapabilityByID`、`ListCapabilities`、`CurrentArtifact`、`CreateDeployments` 全部不得看见 tombstone；但 `DeploymentsForNode` **仍必须**返回指向 tombstone 的私有投递体（`resource_cas_test.go:268-272`）。
- `resource_cas_test.go:640-673`（钉住正文）：删除后用旧 UID 的 `EligibleNodesForTask` 仍须成功 → **tombstone 正文是执行事实**。
- `resource_cas_test.go:290-310`：`setRetention(1,1,1)` + `RunRetention(now+100000)` 后 tombstone 的 `spec` 必须逐字节不变（此时仍有 pending 任务钉住它，所以 purge 不会碰它）。

**如果判据被改坏（谁会被打红）**：`reference_force_test.go:182/191`（deployment / receipt 保护）、`reference_isolation_test.go:55`（三类引用各自保护）、`resource_cas_test.go:298-300`（RunRetention 后 pin 正文不丢）。**如果只是把 purge 接进 `RunRetention`**，上述断言**不会红**（因为它们的夹具都留着引用或没有 tombstone），只有 `resource_cas_test.go:720` 那类「软删保留」断言在有人把 purge 改成硬删时才会红。

---

## Q9 · 风险清单（照 tasks/events 模式接进 `RunRetention` 会踩的坑）

1. **purge 的查询计划是全表扫（实测）**。在 `/tmp/dev.db` 上 `EXPLAIN QUERY PLAN` 结果：
   ```
   |--SCAN c                       ← capability_versions 全扫（deleted_at 无索引）
   |--CORRELATED SCALAR SUBQUERY 1 → SCAN t   (tasks.capability_id 无索引)
   |--CORRELATED SCALAR SUBQUERY 2 → SCAN a   (task_attempts.capability_id 无索引)
   |--CORRELATED SCALAR SUBQUERY 3 → SCAN m   (task_artifact_manifests.capability_id 无索引)
   |--CORRELATED SCALAR SUBQUERY 4 → SEARCH d USING COVERING INDEX idx_deploy_capability
   |--CORRELATED SCALAR SUBQUERY 6 → SCAN p + SCAN j (json_tree 虚拟表)
   `--USE TEMP B-TREE FOR ORDER BY
   ```
   证据：`internal/store/schema.sql:443-458`（tasks 索引无 `capability_id`）、`:509-516`（task_attempts 同）、`:656-657`（manifest 只有 `object_id`/`expires_at` 索引）。→ 每 30s 一轮 = 每 30s 在**写事务**里做 4 次全表扫 + 临时 B 树排序；`BEGIN IMMEDIATE` 全程持写锁（`store.go:36-38`），会与事件入口抢写锁。这正是 `DefaultRetentionBatches` 存在的原因（`retention.go:35` 注释「避免长时间持写锁」）。
2. **即使没有 tombstone，它也会拿写锁**：`PurgeDeletedCapabilities` 进门就 `BeginTx`（`retention.go:323`）→ 每轮固定一次写事务开销。
3. **部署行是「死引用」，purge 判据 4 在真实数据上最常命中**：`DeleteDeployment`（`deployment.go:444`）**没有生产调用者**（grep 全仓仅定义；v3 部署写/删端点已退役，`retained_reads.go:20-24`、`:66-68`），`CreateDeployments` 只 supersede 同一 `capability_id` 的旧行（`deployment.go:187-192`）。实测 `/tmp/dev.db` 唯一的 tombstone 正是被 deployments 挡住的（见 Q7）。→ 只要能力**部署过一次**，它的 tombstone 就**永远不会**被回收，purge 只能回收「从没部署、从没跑过、从没被安装」的 tombstone。
4. **回执也是死引用**：`DeleteExpiredPlaybookInstalls` **无生产调用者**（只有 `v4test/*_test.go`），而 purge 判据 5 以 `installed_from` 保护 → Playbook 装出来的能力，其 tombstone **永久保留**。加上 `json_tree` 分支实际惰性（Q1），这条判据的「引用完整性」成本远大于收益。
5. **`deployments` 是 `ON DELETE CASCADE`**（`schema.sql:539`）：判据 4 一旦漏判（改 SQL、加并发路径），后果不是报错而是**静默删掉部署期望行**，节点侧表现为「内容突然不推了」。相对地 `tasks` 是 RESTRICT（`schema.sql:377`）会硬报错——两者失效模式不对称，测试得分开设计。
6. **静默降级而非报错的三处**：`lifecycleOn`（`task.go:1356`）、`maxPerNodeOn`（`execution_auth.go:115`）、`taskLifecycle`（`task.go:748`）在能力行缺失时**返回默认值**。所以「purge 与建任务并发」不会报错，只会把幂等/预算悄悄换成默认值——这是最难查的一类回归（判据 1 + FK 目前挡住了，但任何放宽都是静默坏）。
7. **stats/口径**：`RetentionStats` 加字段是安全的（只进日志，`lease.go:32`；`Sweeper` 在 `serverDeps` 里无 handler 使用，`api_deps.go:26`），但 `lease_test.go:212,254` 与 `v4test/retention_test.go` 会按字段读断言——新增字段要保证「跳过」与「扫了没删」可区分（沿用 `EventsSkipped`/`Truncated` 的既有语义，`retention.go:44-54`）。
8. **与 namespace 删除的耦合**：tombstone 现在会挡住非 force 的 `DELETE /v1/namespaces/{ns}`（`namespace.go:420`），purge 会把 409 变成 200。若 e2e 有「命名空间非空必须 409」的夹具依赖（`v4test/namespace_test.go:80-96`、`reference_force_test.go:66/149`），需要确认它们的夹具里是否只有 tombstone 挡住。
9. **e2e 夹具**：`scripts/e2e/fixtures/f3/f3-capability.mjs:461-482` 删除 `lint-fix`（UI 断言「列表里不再有」）；`scripts/e2e/lib/v4.mjs:118-…` 的 `capabilityBody` 造真能力。这两处都只看 live 过滤，purge 不影响；但 e2e 默认配置下若新 TTL 默认非 0，purge 会在 e2e 期间真删行——**任何「删除后还能查到历史」的断言都会变成偶发红**（目前未发现此类断言，属待验证）。
10. **store 分层红线**：不能让 store 自己起循环（`internal/store/AGENTS.md:14-18`），所以「谁跑」必须在 lease 或 server；`make contract`（A2/A5）与 purge 无关，但若为了加配置动了 `schema.sql` 注释，注意 A2 禁用词纪律（`internal/store/AGENTS.md:33-35`）。
11. **实现细节不一致（worker 会直接改到）**：`PurgeDeletedCapabilities` 的错误处理与 `defer tx.Rollback()` 与同文件其它 4 个 primitive 不同（`retention.go:325,339-341` vs `:251,271-274`），改动时顺手统一会扩大 diff。

---

## 待验证（我没确认的）
1. 真实生产库里 tombstone 的增长速率与平均 `spec` 体积（只有本机遗留 `/tmp/dev.db` 的 10 行样本）。**未做**性能压测。
2. `PurgeDeletedCapabilities` 在「同轮先删 tasks」之后是否一定能回收对应 tombstone（顺序推断自 `RunRetention` 的 tasks→events 固定序，但**没有任何测试**把两件事连起来验证）。
3. `/v1/orphans` 是否有 e2e 夹具依赖「能力被软删后仍算 `no_online_node`」（`scripts/e2e/suites/orphans.mjs` 只覆盖 `no_online_node` 的三类 NodeReason，未覆盖 `no_capability`）。
4. `ANOTIFY_EVENT_RETENTION_DAYS` 是否曾经（v3 时代）真的写进 system_config——现在是纯日志，历史意图未查。
5. `test_artifact_manifests.capability_id` 之外，是否还有别处把 capability UID 写进 JSON 列（我抽查了 `playbook_installs.receipt` 与 `events.receipt`，未穷举所有 JSON 列）。
6. 候选 C（server 独立循环）若采用，是否需要新的 e2e 套件（AGENTS §7「新功能必须加对应 E2E 套件或单测覆盖」）。

---

## 必须由 owner 裁决的分歧点（最多 4 条）

**① 回收「时间窗」语义**（要不要、多长、谁定义）
- A. 无时间窗（现状语义：无引用即可删，`retention.go:317`）：代码零改动，但「删了立刻收」意味着运维在事故现场（刚删错能力、想从库里捞回正文）没有恢复窗口。
- B. 引入 `capabilityTtlSec` 类配置：必须改 `PurgeDeletedCapabilities` 签名（加 cutoff）+ `system_config` baseline 加列（老库 `ErrSchemaInvalid`，`internal/store/AGENTS.md:40-52`）+ `api/resource.yaml`（`additionalProperties:false` 会硬拒）+ 4 处结构验收同步。
- 代价对比：A 省事但把「tombstone 保留多久」变成隐含常量；B 与 `docs/design/v4/decisions.md:228`「保留配置放系统配置」一致，代价是一次 schema + API 契约改动。

**② 回收挂在哪个循环**（同一轮 vs 独立节奏）
- A. 挂 `RunRetention`（`retention.go:66`）：零新增收尾负担（`WaitBackground` 已覆盖 `mux.go:366-368`），但把「每 30s 一次 4 表全扫 + 写锁」绑到保留期节奏上，与事件入口抢写锁（Q9-1）。
- B. `lease.Sweeper` 独立一步：可独立限频，但仍共用 30s ticker 与 `Report` 结构（`lease.go:24-36`）。
- C. server 独立循环（抄 `artifact-gc`，`mux.go:296-341`）：可给独立间隔/env、可 `<=0` 关闭、有现成 WiL 收尾护栏，代价是多一个 env、多一条 goroutine、且「保留期」这个语义被拆到两处配置体系。

**③ 回收目标是否够用**（判据 4/5 让大部分 tombstone 永远回收不了）
- 事实：`deployments` 行与 `playbook_installs` 行在生产里**都不会被删**（`DeleteDeployment`、`DeleteExpiredPlaybookInstalls` 都无生产调用者），实测唯一的 tombstone 正是被 deployments 挡住的。
- A. 只接现成 primitive（不动判据）：issue 描述的「表线性增长」实际只在「从未部署/从未跑过/从未安装」的能力上成立，收益小但绝对安全。
- B. 同时把「删除能力时顺带清部署期望/允许部署行随能力回收」引入：收益大，但改的是部署/节点同步语义（`DeploymentsForNode` 会少推、`capability.deployed` 回执可能对不上），属于 C2 边界外的产品语义变更（`.pi-orchestrator/v4/RESOURCE-DELETION.md:7-9`）。
- C. 保持判据、但在文档/issue 里把「可回收集合」写清（不写代码）：成本最低，但 issue 的「没有回收路径」仍半成立。

**④ 是否需要「含退役行的历史投影」**（决定回收窗口是不是对外承诺）
- A. 不做 `/history`（现状 `capability.md §8.4`「逻辑删除 ≠ 事实删除，钉住事实留到任务保留期结束」，`docs/design/v4/capability.md:844-845`）：回收窗口是内部实现细节，可自由调。
- B. 做 `/history`（或让 `/versions` 显示已删版本）：回收窗口变成对外承诺，必须先写进 `docs/design/v4/api-conventions.md`（issue 第 4 条要求）并固定 TTL；否则会出现「昨天还能查到的退役版本今天消失」。

---

```json
{"ok": true, "reason": "recon complete, 78 facts with file:line evidence across store/lease/server/schema/docs + 1 measured dev-DB sample and purge query plan"}
```