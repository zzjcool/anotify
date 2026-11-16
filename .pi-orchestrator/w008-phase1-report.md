# 008 Phase 1 · worker 报告（ingest 热路径去读事务 + ingress 补 409 映射）

commit: `61934c2`（main，本地）
area: store（根因侧）+ api（映射侧）· 卡片: `.pi-orchestrator/issues/008-sqlite-busy-500-ingress.md`

## 1. 做了什么（4 件事，严格按卡片 Phase 1 冻结范围）

### ① `internal/store/config.go`：两个热路径读改成**无事务读**

| 函数 | 改前 | 改后 |
|---|---|---|
| `GetSystemConfig` | `d.BeginTx` + `tx.QueryRowContext` + `Commit` | `d.queryRowContext`（`queryExec` seam，per-DB 语句缓存） |
| `GetNamespaceConfig` | `d.BeginTx` + `namespaceExistsOn(txExec{tx})` + 2 条 `tx.QueryRowContext` + `Commit` | `ex := d`，三条读**全部**走同一个 `ex` |

- 形状与注释风格照 `node.go:1409` `nodeEligibility`（「只读路径不开事务：DSN 是
  `_txlock=immediate`，读事务会先拿写锁」）。
- `GetNamespaceConfig` 的三条内部读（含 `namespaceExistsOn`）一起换成 `ex = d` ——
  只改一半会变成「存在性走池、配置行走另一条连接」的撕裂形态（已在注释里写明这条纪律）。
- 注释写明**为什么去掉事务仍安全**：
  - `GetSystemConfig`：读的是单行单语句（`WHERE id = 1`，一条语句本身原子）；调用方把错误当
    可回落 —— `eventWindow` 出错即用默认保留期（`internal/source/source.go:364-367`）；
    `RetentionConfig` / `RunRetention` 只报错不写库；改配置走 `PutSystemConfig` 的 CAS，
    它在自己的写事务内重读 `resource_version`。
  - `GetNamespaceConfig`：三条读**不是** check-then-act。`Revision` 只是调用方带进写事务的
    「计划时钉住的输入版本」，真正的校验在 `AcceptSourceEvent` 的写事务内
    （`internal/store/source_event.go:187-196` → `ErrNamespaceInputsChanged`，api 侧 409）；
    `namespaces.resource_version` 同样只是写侧 CAS 的前置条件。最坏情况 = 「拿到略旧快照 →
    写事务里被判 409」，不会被伪造成成功。
- `namespaceExistsOn` 的文档注释同步（它现在两种调用方都有：写路径传 `txExec`、只读路径传 `d`）。

### ② `internal/api/events.go`：`writeAPIError` 补两条 409 映射（与 `server/api_status.go` 对齐）

- `store.ErrNamespaceInputsChanged` → `409 Conflict`
- `store.ErrResourceVersionConflict` → `409 resource_version_conflict`
  （`*store.ResourceVersionConflictError` 的 `Unwrap()` 就是这个 sentinel，一条 `errors.Is`
  同时覆盖 typed 与哨兵两种形状；ingress 不回 `details.current` —— 投递端点的调用方拿到 409
  重新 GET 源即可，api 包的 `statusDoc` 刻意不带 `details`，这条注释已就地说明）
- 新增两个 reason 字面量常量并注明与 `server.reasonConflict` / `reasonResourceVersionConflict`
  逐字一致（`api-conventions §4.2` 封闭集合）。

### ③ 单测

- `internal/store/pool_test.go` `TestPoolWriteTxIsImmediateAndDoesNotBlockReaders`：
  「写持锁期间直读照常」的裸 `SELECT count(*) FROM users` 断言换成两条**真·ingest 热路径读**
  `GetNamespaceConfig` / `GetSystemConfig`（后者是卡片要求的同款补测），并把读超时 3s → 20s，
  让回归表现为锁错误（busy_timeout 5000ms）而不是 ctx 先取消。
- `internal/api/events_test.go` 新增 `TestWriteAPIErrorConflictMapping`：4 个 case ——
  ns 输入变更 → 409、CAS 冲突（typed）→ 409、CAS 冲突（sentinel 被包一层）→ 409、
  **未知错误仍 500**（反向对照：防止「一切皆 409」吞掉服务端故障）。reason 断言用**字面量**
  （`"Conflict"` / `"resource_version_conflict"` / `"InternalError"`），更严格地钉住线上契约。

### ④ 没做的事（边界）

未动 `_txlock=immediate`（`store.go:104`）与 `busy_timeout`（`store.go:102`）；未动另外 4 个纯读函数
（`GetNodeConfig` / `loadVarsLayers` / `EffectiveLimits` / `AuthorizedNamespaces`）；未碰
`api/*.yaml`（契约）与 Phase 2（busy → 503）；未动 `docs/design/v4/*`。

## 2. 改前红 / 改后绿（原样输出）

### 红（改前，两条测试都真的会红）

```text
$ go test ./internal/store/ -run 'TestPoolWriteTxIsImmediateAndDoesNotBlockReaders' -count=1 -v
=== RUN   TestPoolWriteTxIsImmediateAndDoesNotBlockReaders
    pool_test.go:198: 写事务持锁期间 GetNamespaceConfig 应照常成功，实际: get namespace config: begin: database is locked (5) (SQLITE_BUSY)
--- FAIL: TestPoolWriteTxIsImmediateAndDoesNotBlockReaders (4.62s)
FAIL
FAIL	github.com/zzjcool/anotify/internal/store	4.627s
FAIL
```

（与线上现象逐字同形：`get namespace config: begin: database is locked (5) (SQLITE_BUSY)`，
4.6s ≈ busy_timeout 5000ms；线上那次是 5.006s。）

```text
```text
=== RUN   TestWriteAPIErrorConflictMapping
=== RUN   TestWriteAPIErrorConflictMapping/命名空间输入_revision_变了_→_409
=== RUN   TestWriteAPIErrorConflictMapping/源_CAS_冲突（typed，带当前令牌）→_409
=== RUN   TestWriteAPIErrorConflictMapping/源_CAS_冲突（sentinel，被包了一层）→_409
=== RUN   TestWriteAPIErrorConflictMapping/未知错误仍然_500
2026/11/17 01:46:03 ERROR event ingress failed event=events.ingress.failed err=boom
--- PASS: TestWriteAPIErrorConflictMapping (0.00s)
    --- PASS: TestWriteAPIErrorConflictMapping/命名空间输入_revision_变了_→_409 (0.00s)
    --- PASS: TestWriteAPIErrorConflictMapping/源_CAS_冲突（typed，带当前令牌）→_409 (0.00s)
    --- PASS: TestWriteAPIErrorConflictMapping/源_CAS_冲突（sentinel，被包了一层）→_409 (0.00s)
    --- PASS: TestWriteAPIErrorConflictMapping/未知错误仍然_500 (0.00s)
PASS
ok  	github.com/zzjcool/anotify/internal/api	0.005s
```

### 绿（改后）

```text
$ go test ./internal/store/ -run 'TestPoolWriteTxIsImmediateAndDoesNotBlockReaders' -count=1 -v
=== RUN   TestPoolWriteTxIsImmediateAndDoesNotBlockReaders
--- PASS: TestPoolWriteTxIsImmediateAndDoesNotBlockReaders (0.44s)
PASS
ok  	github.com/zzjcool/anotify/internal/store	0.447s

$ go test ./internal/api/ -run 'TestWriteAPIErrorConflictMapping' -count=1 -v
--- PASS: TestWriteAPIErrorConflictMapping (0.00s)
    --- PASS: .../命名空间输入_revision_变了_→_409
    --- PASS: .../源_CAS_冲突（typed，带当前令牌）→_409
    --- PASS: .../源_CAS_冲突（sentinel，被包了一层）→_409
    --- PASS: .../未知错误仍然_500
ok  	github.com/zzjcool/anotify/internal/api	0.005s
```

（4.62s → 0.44s：改后不再等锁，那 4.2s 全是 busy_timeout 的等待。）

## 3. 门禁（自己跑的，原样）

```text
$ gofmt -l .
（无输出）
$ go test ./internal/store/... -count=1
ok  	github.com/zzjcool/anotify/internal/store	5.115s
ok  	github.com/zzjcool/anotify/internal/store/v4test	22.495s
$ go test ./internal/api/... ./internal/server/... -count=1
ok  	github.com/zzjcool/anotify/internal/api	0.085s
ok  	github.com/zzjcool/anotify/internal/server	53.670s
$ make contract
  A2/A4/A5/A6/A7/A8/A9/A11 通过（27 张表；3 张 kind 禁令、4 张内容标识、21 个中枢包、5 份 spec）
  信封/status/cursor/CAS 字段级检查通过（5 份文件 / 229 个 schema / 3 个具名 snake_case 例外）
  OpenAPI exact-five 通过（5 份文件 / 125 个 operation / 125 个唯一 operationId / 跨文件 $ref 与 pathItems 可解析）
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
```

额外（这两个包直接用这两个函数，属于「上下游影响面」）：

```text
$ go test ./internal/source/... ./internal/lease/... -count=1
ok  	github.com/zzjcool/anotify/internal/source	0.186s
ok  	github.com/zzjcool/anotify/internal/lease	0.078s
$ go vet ./internal/store/ ./internal/api/ ./internal/server/ ./internal/source/
（无输出）
```

`make ci` 未跑（本机基线红：sitegen docs / `cmd/anode` 无 sudo / ws·runner flaky）。

## 4. 额外问题的回答

**Q1. 改前那条测试的红是什么样** → 见 §2 第一段：`get namespace config: begin: database is locked (5)
(SQLITE_BUSY)`，4.62s 后失败，与线上 500 的错误文本逐字一致。

**Q2. 去掉两个读事务后，ingest 一次请求还剩几次写锁** → **1 次**（就是真正的写：
`AcceptSourceEvent` 的 `BEGIN IMMEDIATE`）。改前是 **3 次**（2 读 + 1 写）。核对过整条链：
`AuthorizeNamespace`（`authz.go:27`）与 `GetSourceByName`（`source.go:407`）本来就没有事务；
`internal/route` / `internal/source` 里 `BeginTx` 计数为 0；`internal/store/source.go` 的 3 处
`BeginTx` 分别在 `CreateSource` / `UpdateSource` / `DeleteSource`（写路径，不在 ingest 链上）。
所以「ingest 期间的写锁持有窗口」从 3 段降到 1 段，且剩下的那段就是必须串行化的那一段。

**Q3. 有没有发现产品 bug** → 本卡范围内另发现的 500 就是卡片侦察里已记的那条
（`writeAPIError` 漏映 `ErrNamespaceInputsChanged` / `ErrResourceVersionConflict`，
`/v1/namespaces/{ns}/events` 把「重新计划」的 409 报成 500），已按要求在同一批修掉，
并用 `TestWriteAPIErrorConflictMapping` 钉住（含「未知错误仍 500」的反向对照）。
除此之外没有发现新的产品 bug；**没有改任何断言迁就**（`make ci` 的既知红也一行没碰）。

## 5. 遗留风险（需协调者裁决 / 知情）

1. **Phase 2 仍未做**：写事务自身撞 BUSY 时依旧是 500（不是 503 + `Retry-After`）。本卡只消灭了
   「读也抢写锁」这一半；剩下那半在极端写竞争下仍会 5xx，且需要动契约（`api/ingress.yaml:353` /
   `Backpressure503` / `capacity.mjs:197` 硬断言），按卡片要求另开卡裁决。
2. **读侧一致性从「单事务快照」降级为「三条独立快照」**（仅 `GetNamespaceConfig`）：
   - 写侧无影响（`Revision` / `ResourceVersion` 都是写事务内重读再比，见 §1①）。
   - 两处**展示**投影在「并发 PUT 恰好落在两条读之间」时可能轻微不一致：
     `resource_namespace.go:425`（GET ns 配置：revision 可能比 resourceVersion 早一拍 —— CAS 仍安全，
     因为 PUT 只比 RV）和 `resource_node.go:832`（节点读信封里 `status.namespaceVars` 与
     `effectiveVars` 来自两次独立读）。都是读模型投影，不是事实来源，未改。
3. **`AcceptSourceEvent` 回滚空事务**（幂等命中路径）现在仍是 `BEGIN IMMEDIATE` 拿一次写锁再
   `Rollback`。这不在本卡冻结范围（卡片明确只做两个读），但它也是一次「为了读而拿写锁」的形状，
   建议并入 Phase 2 一起看。
4. 并发实测未做：本卡验收用的是「写事务持锁期间读该路径」的可复现单测（不是压测撞），
   压测回归（2/35245）需协调者跑 soak 复核。
