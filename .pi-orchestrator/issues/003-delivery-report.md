# 003 交付报告 · 已删能力（capability tombstone）的后台物理回收

- 卡：`.pi-orchestrator/issues/003-purge-deleted-capabilities-unwired.md`（裁决 1–4 + 「施工方案（冻结）」）
- 侦察依据：`.pi-orchestrator/issues/003-recon-tombstone.md`（未重新侦察）
- 模块约定：`internal/server/AGENTS.md`（area=server）、`internal/store/AGENTS.md`（area=store）
- 工作区：`/home/zzjcool/code/anotify`（当前分支 `main`，**未提交、未 push**——隔离与 MR 归协调者；
  `git status` 就是本次全部改动：5 改 + 2 新增）

---

## 1. 做了什么（逐条对齐冻结方案）

| 冻结方案 | 落点 | 说明 |
|---|---|---|
| §1 配置 `CapabilityGCIntervalSec` | `internal/server/config.go` | 新字段 + `Config.CapabilityGCInterval()`（`<=0 → 0`）+ `capabilityGCIntervalFromEnv`；env `ANOTIFY_CAPABILITY_GC_INTERVAL_SEC`；`FromEnv` 里接线 |
| §2 GC 驱动器 | `internal/server/capability_gc.go`（新，130 行） | `capabilityGC{db,log,batch=1000,maxBatches=20}` / `newCapabilityGC` / `RunOnce` / `Run`，行为见下 |
| §3 接线 | `internal/server/mux.go` | 紧接 artifact-gc 那块之后，走 `app.runBackgroundLoop(ctx, "capability-gc", ...)` |
| §4 测试 | `internal/server/capability_gc_test.go`（新，390 行）、`internal/server/config_test.go`（+54 行） | 9 条驱动/接线测试 + 2 条配置测试，见 §3 |
| §5 文档 | `DEVELOPMENT.md` §10 表 +1 行；`docs/design/v4/capability.md` §8.4 +4 行 | env 名/默认值/关闭方式；「已删能力挡住命名空间删除不是契约保证」 |
| §6 不做 | — | 未接 `DeleteDeployment` / `DeleteExpiredPlaybookInstalls`；未加时间窗；`PurgeDeletedCapabilities` 的 SQL/签名一字未动；`schema.sql` / `migrate.go` 一字未动；未动 `api/*.yaml` |

驱动器语义（`capability_gc.go`）：

- `RunOnce`：进门先看 `ctx.Err()`；每批之间再检查（取消立刻生效）；`PurgeDeletedCapabilities(ctx, 1000)`
  多批直到某批返回 **0** 或 20 批预算用尽。**单批失败返回「已删数量 + 包装错误」**（`deleted += n` 在错误判断之前），
  错误串里带 `第 N 批物理回收失败（已删 M 行）`。
- 删除总数 >0 打**一条** Info：`event=server.capability.gc.deleted, deleted=N`（失败轮次也打，已删数不丢）。
- `Run`：`time.NewTicker(interval)`，单轮失败只 `Warn`（`event=server.capability.gc.failed`，含 `deleted`）不停摆；
  `ctx.Done()` → 返回 `nil`（干净退出，`runBackgroundLoop` 不会记 WARN）；`interval <= 0` 直接返回（防线）。

---

## 2. 对四个问题的回答

### 2.1 GC 一轮最多删多少行、为什么这样定

**1000 × 20 = 20000 行/轮**（`batch=1000`、`maxBatches=20`，与 `store.DefaultRetentionBatch` /
`DefaultRetentionBatches` 逐字相同）。

定这个数的原因是**写锁**，不是吞吐：`PurgeDeletedCapabilities` 的判据是
`capability_versions` 全扫（没有以 `deleted_at` 起头的索引）+ `tasks` / `task_attempts` /
`task_artifact_manifests` 三次相关全扫 + `json_tree` 虚拟表 + `USE TEMP B-TREE FOR ORDER BY`，
整段在 `BEGIN IMMEDIATE` 写事务里（侦察 Q9 的 `EXPLAIN QUERY PLAN` 实测）。而 tombstone 的
增长速率是个位数/天量级（删一次能力只是把该 name 的 live 版本**原地**打 `deleted_at`，不新增行）。
按实测平均 ~1.1 KB/行，20000 行 ≈ 22 MB/轮的删除上限——积压时下一轮继续（`Run` 每 3600s 一轮），
代价是「回收延迟」，换来的是「不会长时间攥着写锁把事件入口挤成 503」。
`RunOnce` 在**第一批就返回 0** 时立刻收手（不空转 20 个写事务）。

### 2.2 新增/修改了哪些测试，各自钉住什么

`internal/server/capability_gc_test.go`（全部真 sqlite，不用 stub）：

| 测试 | 钉住什么 |
|---|---|
| `TestCapabilityGCRunOnceOnCleanDatabase` | 空库/只有活行 → `(0, nil)`，且**活能力行不被删**（判据只认 `deleted_at IS NOT NULL`） |
| `TestCapabilityGCPurgesUnreferencedTombstone` | 生产路径软删（`DeleteCapability` + CAS）→ 一轮删掉 1 行（返回 1）；**再跑一轮返回 0**（幂等，回收不是一次性批量） |
| `TestCapabilityGCKeepsReferencedTombstone` | 「**有引用不删**」：被 `deployments` 钉住的 tombstone 返回 0 且整行仍在（选它而不是 `tasks` 是因为 deployments 是 `ON DELETE CASCADE`——漏判会**静默**少推内容，比 RESTRICT 的硬报错更需要显式钉住）；随后删掉那条引用再跑 → 删 1 行（证明前面的 0 来自判据，不是驱动器没跑） |
| `TestCapabilityGCRunOnceHonorsCanceledContext` | 「**ctx 取消立即停**」：已取消的 ctx → `errors.Is(err, context.Canceled)`、`deleted==0`、行还在（关停窗口里不该有新写事务落地） |
| `TestCapabilityGCBatchesUntilNothingLeft` | 多批直到清零（`batch=1` + 2 条 tombstone → 一轮返回 2） |
| `TestCapabilityGCStopsAtBatchBudget` | **单轮预算**（`batch=1,maxBatches=1`）→ 只删 1 行并如实返回 1，剩余留到下一轮（1 → 1 → 0）。这条同时钉住「非失败的中途退出也要把已删数累加返回」 |
| `TestCapabilityGCRunOnceWrapsStoreError` | 库不可用 → 错误**不被吞**、带 `capability gc` 前缀、`deleted==0` |
| `TestCapabilityGCWiredByNewApp` | **真实装配**：先在库文件里落一条 tombstone，`NewApp` 配 1s 周期，轮询到该行真的消失（没接线会超时）；再 `cancel → WaitBackground → closeDBForTempDir`（`InUse==0` / `OpenConnections==0` / 无 `-wal|-shm`），即「`WaitBackground` 收尾不撞已关库」 |
| `TestCapabilityGCPurgeFlipsNamespaceDeleteFromBlockedToAllowed` | 本卡唯一**对外可见语义变化**：tombstone 现在计入 `DELETE /v1/namespaces/{ns}` 的 `capabilities` 阻塞计数（先断言 `NamespaceBlockedError` 且清单含 `capabilities=`），回收后同一请求不再被挡。分两段断言，避免「没有 tombstone」的假绿 |

`internal/server/config_test.go`（追加 2 条）：

| 测试 | 钉住什么 |
|---|---|
| `TestCapabilityGCIntervalEnvParsing` | 未设/空 → 3600；`"0"` → 0；负数 → 0；非法 → 3600（手滑不能替运维决定「关掉回收」）；正常值原样；`9223372036854775807` → 钳到 86400（不回绕） |
| `TestCapabilityGCIntervalConversion` | `Config.CapabilityGCInterval()`：`0/-1 → 0`（不是负 Duration——`time.NewTicker(负数)` 会 panic），正数按秒 |

未改任何既有测试/断言；store 侧判据的覆盖沿用 `internal/store/v4test/reference_*`（不重复）。

**覆盖边界（照实说）**：「单批失败保留已删数量」无法用公开 API 构造出确定性用例——要『第一批成功提交、第二批失败』
就必须在两次 `PurgeDeletedCapabilities` 之间插入外部动作，而两者之间只有几微秒、且没有可注入的接缝。
现在的覆盖是：错误路径本身（上面第 7 条）+ 累计计数在中途退出时如实返回（第 6 条），
不变量本身写在 `RunOnce` 的 `deleted += n` 早于错误判断处（注释点名原因）。

### 2.3 默认间隔是多少、怎么关掉

- 默认 **3600 秒（1 小时）**：未设置、空、非数字（手滑）都取这个值；正数上限 **86400 秒（1 天）**。
- 关掉：`ANOTIFY_CAPABILITY_GC_INTERVAL_SEC=0`（或任意负数）→ `CapabilityGCIntervalSec=0` →
  `CapabilityGCInterval()==0` → `mux.go` 里的 `if cfg.CapabilityGCInterval() > 0` 不成立 → **不起 goroutine**。
  `Run` 里还有一道 `interval <= 0 → return nil` 的防线。
- 语义提醒：非法值（如 `"1h"`）**回落 3600 而不是 0**——`0` 是「关掉不可逆回收」的显式选择，不能被拼写错误代劳。
  关掉之后 tombstone 只增不减，重启也不补扫（没有启动即扫的行为，第一轮在 interval 之后）。

### 2.4 有没有发现产品 bug

**没有发现本卡范围内的产品 bug，也没有为了变绿改任何断言或跳测试。** 两条观察（不构成 bug，供协调者决策）：

1. `PurgeDeletedCapabilities` 与同文件其它 4 个 primitive 风格不一致（`defer tx.Rollback()` 无 `_ =`、两处错误裸返回）——
   侦察 Q1 已记录；本卡明确「不改签名/SQL」，故原样保留。
2. 冻结方案选了 ticker（第一轮在启动后一个 interval 才跑），所以**部署后积压最多等 1 小时**才开始回收。
   这是方案的选择而非缺陷；若将来希望「启动即补扫一轮」，那是显式改动（本卡未做）。

---

## 3. 验证输出（原样）

### 3.1 `gofmt -l .`

```
$ gofmt -l .
```

（无输出 = 全部已格式化）

### 3.2 `go test ./internal/server/... ./internal/store/... -count=1`

```
ok  	github.com/zzjcool/anotify/internal/server	53.972s
ok  	github.com/zzjcool/anotify/internal/store	5.063s
ok  	github.com/zzjcool/anotify/internal/store/v4test	23.025s
```

### 3.3 `make contract`

```
  A2/A4/A5/A6/A7/A8/A9/A11 通过（27 张表；3 张 kind 禁令、4 张内容标识、21 个中枢包、5 份 spec）
  信封/status/cursor/CAS 字段级检查通过（5 份文件 / 229 个 schema / 3 个具名 snake_case 例外）
  OpenAPI exact-five 通过（5 份文件 / 125 个 operation / 125 个唯一 operationId / 跨文件 $ref 与 pathItems 可解析）
  路由/handler/DTO 双向对账由 internal/server/openapi_parity_test.go 负责；信封字段规则由 scripts/check_envelope.py 负责
ok  	github.com/zzjcool/anotify/internal/server	1.741s
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
```

### 3.4 `make e2e-one S=orphans`

```
./scripts/e2e/run_all.sh orphans
==========================================
 Anotify E2E 全量测试
==========================================
▶ 构建二进制…
  ✅ 二进制就绪

  ✅ 静态检查：e2e 中没有裸 mkdtempSync（只允许 lib/harness.mjs 的 helper）
模式: 串行（1 个套件）
验收 authority: real 1 个 / explicit-mock 0 个（#338 裁决）

▶ 套件: orphans
  === SUITE: orphans（孤儿 pending：诊断与处置）· v4 ===
    ✅ 拿到默认命名空间 id
  --- 1. 空列表与信封形状 ---
    ✅ GET /v1/orphans?namespace=default
    ✅ 空列表是数组（不是 null）
    ✅ 空列表长度 0
    ✅ count = 0
    ✅ 默认 olderThan = 3600 秒
    ✅ namespace 回显请求的 ns id
    ✅ 单 ns 视图 allScopes=false
    ✅ hint 说明「不会自动清理、用 abandon 显式处置」
  --- 2. 判定：没有任何节点 → NoMatchingNode ---
    ✅ 建能力 orphan.cap.plain
    ✅ GET /v1/orphans?namespace=default
    ✅ 列出那条 2 小时前的孤儿任务
    ✅ reason = no_online_node
    ✅ nodeReason = NoMatchingNode（一台节点都没有）
    ✅ 类别：任务仍是 pending
    ✅ 诊断带 namespaceId
    ✅ 诊断带能力名
    ✅ 诊断带钉住的 digest
    ✅ idleSec ≈ 7200
  --- 3. 起一台真节点：在线 + 配置 applied + 持有 digest ---
    ✅ 登记节点 orphan-node
    ✅ 登记节点 orphan-node 拿到 uid
    ✅ 签发 orphan-node 的令牌
    ✅ orphan-node 的令牌明文只在签发响应里
    ✅ 建能力 orphan.cap.needs-command
    ✅ 建能力 orphan.cap.needs-value
    ✅ 建能力 orphan.cap.not-deployed
    ✅ 部署 orphan.cap.plain → orphan-node
    ✅ 部署 orphan.cap.needs-command → orphan-node
    ✅ 部署 orphan.cap.needs-value → orphan-node
    ✅ GET /v1/orphans?namespace=default
    ✅ 在线且持有 digest → 普通能力的任务**不**出现在孤儿列表
    ✅ 要命令但节点没探到 → nodeReason CommandMissing
    ✅ 要值但三层都没有 SOURCE_ROOT → nodeReason ValueMissing
    ✅ 节点没持有该 digest → nodeReason NotDeployed
    ✅ CommandMissing 的类别都是 no_online_node
    ✅ ValueMissing 的类别都是 no_online_node
    ✅ NotDeployed 的类别都是 no_online_node
  --- 4. 跨命名空间：节点没申报 ns → NamespaceUnauthorized ---
    ✅ 建第二个命名空间
    ✅ 第二个命名空间拿到 id
    ✅ 建能力 orphan.cap.ns2
    ✅ GET /v1/orphans?namespace=orphans-two
    ✅ 第二个命名空间自己的视图里看得见
    ✅ 节点没申报 ns2 → nodeReason NamespaceUnauthorized
    ✅ GET /v1/orphans?namespace=default
    ✅ 默认 ns 的视图**不**含 ns2 的任务
    ✅ GET /v1/orphans?namespace=*
    ✅ namespace=* → allScopes=true
    ✅ namespace=* 回显 *
    ✅ namespace=* 同时含两个 ns 的孤儿
    ✅ count 与 orphans.length 一致
  --- 5. 配置 revision 前进 → ConfigPending，config.applied 之后自愈 ---
    ✅ 读节点 orphan-node
    ✅ 节点带 resourceVersion（CAS 令牌）
    ✅ PATCH 节点 orphan-node 的 vars
    ✅ 读节点 orphan-node 的配置
    ✅ 配置 revision 已前进、applied 还停在旧值
    ✅ GET /v1/orphans?namespace=default
    ✅ 配置未 apply → nodeReason ConfigPending（比 NotDeployed/缺命令更具体）
    ✅ GET /v1/orphans?namespace=default
    ✅ config.applied 之后普通能力的任务不再是孤儿（自愈）
    ✅ GET /v1/orphans?namespace=default
    ✅ 自愈后 CommandMissing 仍然成立（配置不是它唯一的问题）
  --- 6. olderThan 下限防误报 ---
    ✅ GET /v1/orphans?namespace=default
    ✅ 10 秒前的 pending 不算孤儿
    ✅ olderThan=2h → 200
    ✅ olderThan=2h：含 3 小时前的任务（2h < 3h）
    ✅ olderThan=4h → 200
    ✅ olderThan=4h：3 小时前的任务在窗口外（4h > 3h）
    ✅ olderThan 回显请求的窗口（7200）
  --- 7. abandon：处置 + 冲突 + 404 ---
    ✅ POST …/tasks/{id}/abandon → 200
    ✅ 返回的 Task 信封 kind
    ✅ 返回的任务相位 = abandoned
    ✅ 库里 phase = abandoned
    ✅ 原因写进 error 列（事后能回答「为什么没跑」）
    ✅ 终态写了 finished_at
    ✅ GET /v1/orphans?namespace=default
    ✅ 处置后不再出现在孤儿列表
    ✅ 已 abandoned 再 abandon → 409（CAS 不假装成功） → 409
    ✅ 已 abandoned 再 abandon → 409（CAS 不假装成功） → reason=Conflict
    ✅ 第二次拒绝没有改写原因
    ✅ 不存在的任务 → 404
    ✅ 跨 ns 处置别人的任务 → 404（不泄露存在性）
  --- 8. 参数校验 / 鉴权 / 只读 ---
    ✅ olderThan=0 → 400（不许把刚派发的当孤儿）
    ✅ olderThan=abc → 400
    ✅ olderThan=30s → 400（下限 1 分钟）
    ✅ limit=0 → 400
    ✅ 未登录 → 401
    ✅ 缺 namespace → 400（绝不回退默认 / 不做全量）
    ✅ 不存在的命名空间 → 404
    ✅ GET /v1/orphans?namespace=default
    ✅ POST /v1/orphans 没有写入口（404/405）
    ✅ GET /v1/orphans?namespace=default
    ✅ POST /v1/orphans 不改事实（孤儿集合与 count 都不变）
    ✅ POST /v1/orphans 后 count 不变
    ✅ hint 指到 v4 的 abandon 路径 /v1/namespaces/{ns}/tasks/{id}/abandon
  --- 9. 节点断开 → NodeOffline ---
    ✅ GET /v1/orphans?namespace=default
    ✅ GET /v1/orphans?namespace=default
    ✅ 节点断开后任务变成孤儿，且原因 = NodeOffline
    ✅ NodeOffline 的类别仍是 no_online_node
  
  [orphans] 98 通过 / 0 失败
  ✅ 套件 orphans 通过

==========================================
 汇总
==========================================
  总耗时: 3.5s
  断言通过: 98
  断言失败: 0
  验收 authority: real 1 / explicit-mock 0（明细见每行 mode=；清单 .e2e-bin/results/_authority-manifest.json）

  ✅ orphans            98通过/0失败  3.5s  mode=real
==========================================
✅ E2E 全绿：1/1 个请求套件均达标（断言 98 通过 / 0 失败，authority 清单覆盖 1 个）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 3.6s）
```

### 3.5 额外：`go vet ./...` / `go build ./...`

```
go vet ./...    → exit 0（无输出）
go build ./...  → exit 0（无输出）
```

### 3.6 额外：`go test ./... -count=1` 的基线红对比（回答「是否新增红」）

`make ci` 在本机基线就是红的（`issues/README.md`「基线红」一节）。本次全量 `go test ./...` 的红点是
**同样的三处**，且我把 HEAD 源码导到 `/tmp/anotify-head`（`git archive HEAD | tar -x`）做了对照：

| 包 | 本次树 | HEAD 干净副本 | 结论 |
|---|---|---|---|
| `internal/sitegen` `TestDocsPageRendersFiveSpecFixturesFourLanguages`（want 16 tag groups, got 0） | FAIL | **FAIL（同样 4 行）** | 基线红，非本次引入 |
| `cmd/anode` 5 个 `TestInstallScript*`（无 sudo / systemctl） | FAIL | **FAIL（同样 5 条）** | 基线红，非本次引入 |
| `internal/ws` `TestBudgetShrinkKeepsOldBusyAndRejectsNewGrant`（帧序竞态） | FAIL（1 次） | 单跑 PASS（3.5s） | 已知 flaky；本次树单跑 `-count=5` 全 PASS，且未碰 `internal/ws`/`internal/protocol` |
| 其余全部包（含本次改动的 `internal/server`、`internal/store`、`internal/store/v4test`） | ok | ok | **无新增红** |

---

## 4. 未决问题 / 遗留风险

1. **收益边界（侦察 Q7/Q9 已量化，必须在收口时讲清）**：判据 4/5 让大部分 tombstone 仍然回收不了——
   `DeleteDeployment` 与 `DeleteExpiredPlaybookInstalls` 仍无生产调用点，所以「部署过」与「被安装回执钉住」的
   tombstone **永久保留**。本卡按裁决 3 只接 purge，这个收益上限是有意接受的，不是遗漏。
2. **代码现状与卡里「判据 3 收益」的判断一致**：跑过一次的能力在 `task_ttl`（默认 30 天）过期后会自动变成可回收
   （`task_attempts` / `task_artifact_manifests` 对 `tasks(id)` 是 CASCADE）；这一条**没有**新测试直接连起来验证
   （侦察「待验证 #2」），属于 store 判据的既有行为，未在本卡扩面。
3. **被 purge 改变的那条语义**已写进 `docs/design/v4/capability.md §8.4`（「已删能力挡住命名空间删除**不是**契约保证」），
   并有测试钉住；若将来有人想让它重新变成保证，需要同时改文档 + 测试 + 关掉本 GC。
4. **没有启动日志记录 GC 周期**（artifact-gc 有 `server.artifact.gc.configured`）。冻结方案未要求，
   为控制 diff 没加；排障时目前靠 `server.capability.gc.deleted` / `.failed` 两条事件 + README env 表。
5. **未提交**：改动留在工作区（`main` 分支），由协调者收队做隔离/MR；我没有本地 merge、没有 push。
