# worker 交付报告 · 能力 `status.observed`（服务端，方案 B）

分支：`feat/capability-observed`（@ b90bf5ef）· 未 commit / 未 push / 未碰前端（裁决要求）
契约：`.pi-orchestrator/CONTRACT-observed.md`（2026-11-17 方案 B 修订版）

## 1. 改动文件

| 文件 | 改动 |
| --- | --- |
| `internal/store/schema.sql` | baseline 块内（`idx_tasks_ns_created` 旁）加 `idx_tasks_cap_created ON tasks(namespace_id, capability, created_at DESC, id)`；`headVersion` 未动、未加迁移块、未改 `migrate.go` |
| `internal/store/capability_observed.go` | **新增**：`ObservedForCapability` + `CapabilityObserved/ObservedEmit/ObservedPrune` + 常量 `ObservedEmitLimit=50` / `ObservedPruneLimit=50` / `ObservedWindow=24h` |
| `internal/server/resource_capability.go` | `statusFor` ⑥ 接入真实结果；四个具名 DTO；失败 → `logger().Warn` + 空对象（不 500） |
| `api/resource.yaml` | `CapabilityStatus.observed` → `$ref CapabilityObserved`；新增 `CapabilityObserved`/`...Emit`/`...Prune`/`...Tasks`（`additionalProperties:false`，子形状 required 全覆盖） |
| `internal/server/openapi_parity_test.go` | 4 条新 pair；棘轮 `parityPairsFloor 192→196`、`specSchemaFloor 230→234` |
| `internal/store/capability_observed_test.go` | **新增** 4 个用例（含两条 EXPLAIN 门禁） |
| `internal/server/resource_capability_observed_test.go` | **新增** 2 个用例（envelope 齐全 / 降级） |

## 2. 关键实现决策（都写进了代码注释）

1. **索引只在 baseline**（方案 B）。`readTableLayout` 只收录列/唯一索引/CHECK/外键，普通索引不进布局比对 → 不需要引擎改造。
   **交付说明：已存在的本地 dev 库（marker=4、baseline 已记账）不会补建该索引，需删库重建；CI/e2e 用 fresh 库不受影响。**
2. **「最近任务」不加索引**：既有 `idx_tasks_ns_created` 反向扫即零排序（实测）。
3. **`ok` 判定**从 `emits.result` 的实际形状推导（`emission.go:emitResult` + `route/emit.go` + `protocol.EmitResponse`）：
   空 result → true；`taskId` 非 null → true；`taskIds` 非空 → true；**`pruned` 非 null → false**（剪枝回执 = 收到但没建任务）；
   其余（含非法 JSON）→ false。**刻意不回查 tasks 判 phase**：下游执行失败仍是「派发成功」，那归 `tasks.failed`。
4. **剪枝过滤写在 SQL 里**（`CASE WHEN json_valid(payload) THEN json_extract(payload,'$.taskId') END = ?`），
   让 `LIMIT 50` 作用在**匹配行**上；同 trace 里别的任务的剪枝不会把真正的 50 条挤出去。
5. **`{}` 的语义**：没有最近任务 → 四键全不写（与升级前逐字节一致）；有任务但窗口内零条 → `tasks:{total:0,failed:0}`（那是有信息量的一句话）。
6. **降级**：observed 是可选派生面且 `statusFor` 被列表/详情共用，故查询失败只 warn + 空对象。

## 3. EXPLAIN QUERY PLAN（多 ns + ANALYZE，避免单 ns 退化成 `SCAN tasks`）

计划门禁日志（`go test ./internal/store -run TestObserved -v`）：

```
capability_observed_test.go:341: sqlite_stat1[idx_tasks_cap_created] = 2400 800 160 1 1
capability_observed_test.go:354: EXPLAIN QUERY PLAN（最近任务）:
    5|0|97|SEARCH tasks USING INDEX idx_tasks_cap_created (namespace_id=? AND capability=?)
capability_observed_test.go:358: EXPLAIN QUERY PLAN（24h 窗口计数）:
    4|0|96|SEARCH tasks USING INDEX idx_tasks_cap_created (namespace_id=? AND capability=? AND created_at>?)
capability_observed_test.go:411: EXPLAIN QUERY PLAN（最近任务的剪枝事件）:
    5|0|52|SEARCH events USING INDEX idx_events_trace (namespace_id=? AND trace_id=?)
```

数据形状：3 个命名空间 × 800 行 × 5 个能力 + 真 `ANALYZE`（`stat1 = 2400 800 160 1 1` 证明统计真的落了）。
断言：窗口计数必须含 `idx_tasks_cap_created` 且**不含** `SCAN tasks`；剪枝必须含 `idx_events_trace` 且**不含** `SCAN events`；
最近任务只要求不出现 `SCAN tasks`（规划器可在 ns_created 反向扫与新索引之间选，两条都不是全表扫）。
另：门禁自检「产品代码真的用 `observedCountsSQL`」——否则会变成测一条没人跑的 SQL。

> 已知坑（上一轮实测）：**单 ns** 小库上 ANALYZE 后规划器会把窗口计数退化成 `SCAN tasks`（`namespace_id=?` 无区分度），
> 所以门禁必须按多 ns 造数据，否则是假绿。这条写进了测试注释。

## 4. 测试清单

store（`internal/store/capability_observed_test.go`）：
- `TestObservedForCapabilityEmpty` — 表驱动：能力从没跑过 / 不存在的 ns → 零值 + `Emits`/`Pruned` 非 nil；空参数报错。
- `TestObservedForCapabilityWithFacts` — 表驱动覆盖 emit `ok` 的 6 种 result 形状 + `fields` 键名排序 + 剪枝三列 + 计数 (3,1) + `payload.taskId` 过滤。
- `TestObservedForCapabilityLimits` — emits 60→50（截断位置）、pruned 60→50、**created_at 写到 25h 前**的窗口外任务不计入（无 sleep）。
- `TestObservedCountsQueryPlan` / `TestObservedPruneQueryPlan` — 上面两段 EXPLAIN 门禁 + 索引列序断言。

server（`internal/server/resource_capability_observed_test.go`）：
- `TestCapabilityStatusObserved` — 子用例「无事实 → `observed` 是 `{}`」/「有事实 → taskId/emits[to,fields,ok]/pruned[to,reason,detail]/tasks{total,failed} 齐全」。
- `TestCapabilityStatusObservedDegrades` — 用 `ALTER TABLE events RENAME` 制造**真失败**，断言 GET 仍 200 且 observe 降级为 `{}`（warn 见日志）。

## 5. 门禁原始输出

```
$ make check-test-discipline
   扫描 scripts/e2e/**/*.mjs     = 55 个（下限 55）
   扫描 internal/**/*_test.go    = 208 个（下限 201）

   e2e/waitForTimeout    :  21 /  21   ✅
   e2e/swallow-catch     :  39 /  39   ✅
   go/t.Skip             :   3 /   3   ✅
   go/time.Sleep         :  37 /  37   ✅
   e2e/bare-tag-first-nth:   3 /   3   ✅

✅ 五项全部与基线一致（t.Skip 登记 3/3 命中）—— 有改善请同步下调基线；新增必须在此刻被拦住

$ make contract
  A2/A4/A5/A6/A7/A8/A9/A11 通过（27 张表；3 张 kind 禁令、4 张内容标识、21 个中枢包、5 份 spec）
  信封/status/cursor/CAS 字段级检查通过（5 份文件 / 234 个 schema / 3 个具名 snake_case 例外）
  OpenAPI exact-five 通过（5 份文件 / 127 个 operation / 127 个唯一 operationId / 跨文件 $ref 与 pathItems 可解析）
  路由/handler/DTO 双向对账由 internal/server/openapi_parity_test.go 负责；信封字段规则由 scripts/check_envelope.py 负责
ok  	github.com/zzjcool/anotify/internal/server	5.380s
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过

$ go test ./internal/store ./internal/server -count=1
ok  	github.com/zzjcool/anotify/internal/store	8.627s
ok  	github.com/zzjcool/anotify/internal/server	61.159s

$ go build ./...
(exit 0)
```

parity 内部统计（`TestOpenAPIParityDTOFields -v`）：`schema 覆盖：total=234 paired=196 reachable=22 unpaired=16` —— 与新棘轮数字一致。

## 6. 未决问题 / 遗留风险

1. **老 dev 库缺索引**（方案 B 的固有代价）：不删库重建就只是少一条索引——查询仍正确，只是窗口计数退化成全表/ns 级扫。
2. **N+1 读放大**：`statusFor` 被能力列表、能力详情、以及 `resource_playbook.go:643` 的**逐能力聚合循环**调用。
   从没跑过的能力（playbook 聚合里的常见情形）只多 1 条索引查询（最近任务无行即短路），有历史时最多 4 条。
   列表页 M 个能力 = 至多 4M 条索引查询。若要优化：把 observed 从 `statusFor` 拆成可选派生面（需改 `resource_playbook.go`，不在本任务所有权内）。
3. **e2e / 前端未跑**（按任务卡：不碰前端、门禁只有那 4 条）。契约里的 `f3-capability.mjs` 断言与 `capability.html` /
   `web/mock.js` 对齐属于另一个人/任务；`observed` 形状变化对本轮前端是安全的（之前恒 `{}`）。
4. `price`/统计无缓存：`ObservedWindow` 用调用时刻现算，同一页两次读可能差 1 条边界行——这是「读时派生」的应有语义，不是 bug。
