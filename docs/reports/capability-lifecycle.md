<!-- snapshot-doc: 2026-11-18 #432 交付时的定格数字（make ci / e2e 的真实输出），复算命令见 §8 -->
# 能力启停 + 手动测试（#432）交付报告

> 分支 `feat/cap-lifecycle` · 基线 `origin/main` `2c115020` · 设计真源
> `docs/design/capability-lifecycle.md`（随 #433 合入）· 冻结 wire 契约
> `docs/contracts/cap-lifecycle/WIRE.md`
> PR：[#438](https://github.com/zzjcool/anotify/pull/438)（`Fixes #432`）

一句话：**停用是路由期的闸门（不建新任务），不是任务的暂停；试跑是一次带标记的真实运行
（建真任务、走真派发），标记落在事件上（`origin='test'`）而不是新开一条建任务路径。**

---

## 1. 改了什么（按层）

### 1.1 store（`internal/store/`）

| 文件 | 改动 |
| --- | --- |
| `schema.sql` | 新增 `capability_switches`、`capability_audit` 两张表 + `idx_cap_audit_ns_name`；`events` 加列 `origin TEXT NOT NULL DEFAULT 'source' CHECK (origin IN ('source','test'))`。**baseline 块内原地改，`headVersion` 仍是 1** |
| `capability_switch.go`（新） | `GetCapabilitySwitch`（**无行 = enabled**，读路径不写库）/ `IsCapabilityEnabled` / `SetCapabilitySwitch`（upsert + 审计**同一事务**）/ `AppendCapabilityAudit` / `ListCapabilityAudit`（默认 50、夹到 200、倒序）。审计 `detail` 只收摘要 |
| `capability_test_event.go`（新） | `AcceptTestEvent`：事件（`origin='test'`）+ 任务（`createTasksTx`）+ 剪枝事实 + 审计摘要**一个事务**。事务内校验 namespace 存在、能力行**仍然活着**且与任务钉住的 `name/version/digest` 逐字一致 |
| `event.go` | `Event.Origin` + `EventOriginSource/Test` + `EventTestSource`（试跑事件写进 `events.source` 的固定文本 `capability`）+ `normalizeEventOrigin`；`eventColumns` / `scanEventRow` / 两处 INSERT 带上 origin；过滤谓词抽成唯一的 `eventFilterClause`（列表与游标分页共用），新增 `ListEventsWithOrigin` |
| `event_page.go` | `ListEventsBeforeWithOrigin`（与列表共用同一份过滤谓词） |
| `stats.go` / `admin.go` | 事件数口径**排除** `origin='test'`（`Total` / `Today` / 每日热力图 / 全站 `MessageCount` / `TodayMessages`）—— L-D6 |
| `namespace.go` | 删命名空间的显式删除清单补 `capability_switches` / `capability_audit`（与既有「显式清，不靠 FK」一致） |
| `ids.go` | `NewCapabilityAuditID`（`aud_<base62>`） |

### 1.2 protocol / route

| 文件 | 改动 |
| --- | --- |
| `enums.go` | `ReasonCapabilityDisabled = "capability_disabled"` 进 `pruneReasons` 闭集（4 道 fail-closed 守卫自动覆盖：`AcceptSourceEvent`、`validateEmitPlannedPrunes`、`insertPruneFactEventOn`、`source` 包的编译期断言） |
| `params.go` | `Prune` 加 `Field string`（`json:"-"`，不进 wire）——让 400 的 `details.causes[].field` 能指到 `params.<字段>` |
| `route.go` | 闸门**唯一实现** `capabilityAccepting`（内部只调 `db.IsCapabilityEnabled`，按**能力名**查，`name@v` 引用先取名字）；`PlanFromSource` / `PlanFromEmit` 各加一处调用，位置在 `PrepareParams` 成功之后、标 `StateReady` 之前（严格按设计 §6.1 的图）；disabled → 同时写 `Targets[StatePruned]` 与 `Pruned{capability_disabled}` |

### 1.3 server / API 契约

| 文件 | 改动 |
| --- | --- |
| `resource_capability.go` | 4 条路由 + 4 个 handler；`capabilityStatus.Enabled`（开关的**派生投影**，`statusFor` 里从 `GetCapabilitySwitch` 填，无行 = true） |
| `resource_task.go` | `GET …/events` 与 `/v1/console/events` 增加 `?origin=` 透传（含游标绑定）；`eventSpecDoc.Origin` |
| `api/resource.yaml` | 4 条 path（全部显式 `security: [{session: []}]`，4xx/5xx 全 `$ref` 具名族）+ 6 个 schema：`CapabilityDisableRequest` / `CapabilityTestRequest` / `CapabilityTestReceipt` / `CapabilityTestWarning` / `CapabilityAuditEntry` / `CapabilityAuditList`；`CapabilityStatus.enabled` |
| `api/events.yaml` | `listEvents` / `listConsoleEvents` 的 `origin` query（enum `source,test`）+ `EventSpec.origin`（enum，required） |
| `openapi_parity_test.go` | **只调 ratchet 数字**：`muxPatternCount` 118→122、`parityPairsFloor` 196→202、`specSchemaFloor` 234→240、operation 总数 129→133。判据/白名单/豁免表一个字未改 |

### 1.4 前端

| 文件 | 改动 |
| --- | --- |
| `web-src/pages/capability.html` | 状态徽标 `#cap-enabled-badge`；动作条 `[data-act=enable|disable|test]`（启停按状态二选一，停用走 `confirmDanger` 二次确认）；`#test-modal`（照抄 `sources.html` `#fire-modal` 范式：版本 / schema 表单 / 节点多选 / 回执面板 / 400 causes 标到字段旁）；`[data-sec="audit"] #cap-audit` 最近操作列表 |
| `web/partials.js` | 抽出共享 `Anotify.schemaForm(schema, {values,onChange})`（对象/原始类型/`enum` 渲染；`oneOf`/`allOf`/`$ref`/`anyOf`/顶层非 object → 降级 JSON textarea）；`model.event` 透传 `origin` |
| `web-src/pages/events.html` / `trace.html` | 对 `origin=test` 的行给「手动试跑」标记（**不隐藏**：试跑是真实执行，静默隐藏会让用户以为没跑过） |
| `web/mock.js` | 预览权威补齐四个新端点（`enable`/`disable`/`test`/`audit`）+ `status.enabled` 派生：explicit-mock 套件（i18n / i18n_coverage / ui_shift）会在能力详情页触发 `GET …/audit`，mock 不实现就会打 `mock 未实现` 让 16 条断言变红（本轮实测过） |
| `web-src/locales/{zh-CN,en,ja,es}.yaml` | 新增 key（含 `common.origin_test`），四份键集合保持一致 |

### 1.5 e2e / 迁移脚本

| 文件 | 改动 |
| --- | --- |
| `scripts/e2e/suites/api_contract.mjs` + `lib/v4.mjs` | 四个端点的 HTTP 契约覆盖（成功 / 幂等 / 参数 400+causes / 404 / 401 / 未部署 warning / origin 隔离 / 审计 limit） |
| `scripts/e2e/fixtures/f3/f3-capability-lifecycle.mjs` + `f3-cases.mjs` | 新 case `capability-lifecycle`：停用 → 源触发 0 任务 → 启用 → 恢复建任务 + 试跑弹窗 + 审计列表 |
| `scripts/migrations/capability-lifecycle.sql` | 已有 dev 库的**原地升级** DDL（见 §2） |

---

## 2. 数据模型与迁移方式

### 2.1 三处 schema 变更（全部是「加表 / 加列 + 默认值」）

```sql
capability_switches (namespace_id, name, enabled, reason, updated_at, updated_by)  -- PK (namespace_id,name)，无行 = enabled
capability_audit    (id, namespace_id, name, action, actor_id, detail, created_at) -- append-only，detail 只存摘要
events.origin TEXT NOT NULL DEFAULT 'source' CHECK (origin IN ('source','test'))
```

- **开关按 `(namespace_id, name)` 存，不是按版本**：换版本（PATCH）不重置开关，历史版本不各带一个开关（L-D1）。
- **无行 = enabled** ⇒ 老库升级**零回填**，所有存量能力天然启用。
- `events.origin` 的 `DEFAULT 'source'` 覆盖全部存量行 ⇒ 也零回填。
- 删除能力后开关行/审计行随命名空间级联消失；同名重建 = 新身份，从 enabled 开始。

### 2.2 迁移方式（两种库，分别验证过）

**① fresh 库 / CI / e2e**：`Open` 直接建 baseline（`headVersion=1`），无需任何动作。

**② 已有 dev 库**：当前迁移引擎只支持**单块** baseline（`baselineLayout()` 硬校验
`恰好一块且 version==baselineVersion`；见 `internal/store/AGENTS.md` §3.2 与设计 §5.4），
所以老库会被 `Open` **具名拒绝**、不迁移、不删数据。实测（把 `HEAD` 版本的 schema 造成一个
v4 库、塞一条「用户数据」，再用本分支二进制打开）：

```
$ ANOTIFY_DB=/tmp/mig/dev.db ./anotify
{"level":"ERROR","event":"server.fatal","error":"store: v4 布局标记与库结构不一致：
 缺 CHECK 约束 events.origin；缺列 events.origin；缺表 capability_audit；缺表 capability_switches。
 程序不会在坏库上做 CREATE IF NOT EXISTS 修补，也不会删除用户数据"}
```

因为三处变更**全是加表 / 加列 + 默认值**，可以原地补齐而不用改迁移引擎：

```
$ cp anotify.db anotify.db.bak
$ sqlite3 anotify.db < scripts/migrations/capability-lifecycle.sql
$ # 重启服务
$ curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7911/health
200
```

同一次实测里，升级前后的数据核对（**不丢数据**）：

```
旧库（HEAD 的 schema）：tables 28 / events 有 origin 列 = False
  塞入：users 1 行、namespaces 1 行、capability_versions 1 行
执行 scripts/migrations/capability-lifecycle.sql
  → events 列 = [id, namespace_id, trace_id, source, type, payload, delivery_id, reason, receipt, created_at, origin]
  → users 1 / namespaces 1 / capability_versions 1（数据都在）
  → 新二进制启动成功，/health = 200
```

要点：`ALTER TABLE … ADD COLUMN` 落在表尾，`validateV4Layout` 按**列名**比对不看物理顺序，
所以升级后的库结构与新 baseline 判定一致（上面 `health=200` 就是这条的机器证据）。

> 设计 §5.4 写的是「开发机需要删库重建」。本 PR 用一条运维 DDL 把「不丢数据」这半个要求补上了，
> 且**没有**动迁移引擎（引入第 2 块属于单独授权，不在本卡范围内）。

---

## 3. 闸门语义（改在哪、为什么这么改）

- **唯一实现**：`route.(*Router).capabilityAccepting` —— `PlanFromSource` 与 `PlanFromEmit`
  共用，避免两条路径各写一份「这个能力能不能接」。
- **位置**：`resolveCapability → RenderParams → PrepareParams → ★ 读开关 → ready`（严格照设计 §6.1 的图）。
  因此「参数不合法 + 能力停用」先报参数原因，而不是 `capability_disabled`（有测试钉住这条优先级）。
- `disabled` → 不建任务，写剪枝事实 `capability_disabled`（`Targets[StatePruned]` + `Pruned` 双写，
  与 `param_missing` 同形状）；**不是** failed Task（那是 `downstream_missing` 的形状）。
- 只作用于被停的**那个名字**：同一次扇出里其它 target 照常建任务。
- **不取消、不暂停、不改**已排队/进行中的任务；`deploy` / 建版本 / 删除照旧可用。
- 生效时机 = 下一次触发判定（读时闸门）；一次已经通过判定的触发仍会建出任务（「最终停」不是「立刻停」，见设计 §9 竞态条目）。

---

## 4. API 实测（curl 请求/响应原文）

环境：`./anotify` + `devseed`（本地库 `/tmp/apidemo/t.db`），会话 Cookie 认证，命名空间 `default`。

### 4.1 造数 + 默认状态

```
$ curl -X POST $B/v1/namespaces/default/capabilities -d '{"...spec.params={diff:string required}...}'
HTTP 201
$ curl -X POST $B/v1/namespaces/default/sources -d '{"...targets":[{"capability":"review","params":{"diff":"$(payload.diff)"}}]}'
HTTP 201
$ curl $B/v1/namespaces/default/capabilities/review
status.enabled = True | resourceVersion = rv1:cap_1795027537_26bd531f_5Ui5Hmk:1
```

### 4.2 `POST …/disable`（带 reason）+ 幂等

```
$ curl -X POST $B/.../capabilities/review/disable -d '{"reason":"上游在改接口"}'
kind = Capability | status.enabled = False | resourceVersion = rv1:cap_…:1     # RV 未变
$ # 再按一次
1st: rv1:cap_…:1 False
2nd: rv1:cap_…:1 False
→ resourceVersion 与状态都未变（幂等；但每次仍写一条审计）
```

### 4.3 停用期间：源触发不建任务（200 照收）

```
$ curl -X POST $B/v1/namespaces/default/sources/demo-src/fire -d '{"payload":{"diff":"x"}}'
{
  "eventId": "evt_BCa6wnZo9fJwPFPEyo",
  "traceId": "evt_BCa6wnZxT54AwuiMaP",
  "duplicate": false,
  "tasks": [],
  "pruned": [
    {"capability": "review", "reason": "capability_disabled", "detail": "下游 \"review\" 已停用"}
  ]
}
```

### 4.4 停用期间：人工试跑**仍然可用**（200 + 真任务 + warning）

```
$ curl -X POST $B/.../capabilities/review/test -d '{"params":{"diff":"manual"}}'
HTTP 200
{
  "traceId": "evt_BCa6woFi5NpxX343UU",
  "origin": "test",
  "tasks": [{"taskId": "tsk_BCa6woFdVslE06gIKy", "capability": "review", "version": 1}],
  "pruned": [],
  "warnings": [{"reason": "capability_not_deployed",
    "detail": "没有在线节点持有 review@1 的 digest sha256:26bd…；任务 tsk_… 已排队，等节点上线或完成部署后自动派发"}]
}
```

未部署是 `200 + warnings[]`，**没有** 4xx（设计 §4.2 明确要求）。

### 4.5 参数错误 → 400 + `details.causes[].field`

```
$ curl -X POST $B/.../capabilities/review/test -d '{"params":{}}'
HTTP 400
{
  "apiVersion": "anotify/v1", "kind": "Status", "status": "Failure",
  "code": 400, "reason": "param_missing", "message": "需要 diff",
  "details": {"causes": [{"reason": "param_missing", "message": "需要 diff", "field": "params.diff"}]}
}
```

### 4.6 不存在的版本 → 404；`enable` → 200；启用后再触发 → 真任务

```
$ curl -X POST $B/.../capabilities/review/test -d '{"version":9,"params":{"diff":"x"}}'
HTTP 404
$ curl -X POST $B/.../capabilities/review/enable -d '{}'
status.enabled = True
$ curl -X POST $B/.../sources/demo-src/fire -d '{"payload":{"diff":"y"}}'
tasks = [{'taskId': 'tsk_BCa6xgLnmXePKUC8j5', 'capability': 'review', 'version': 1}] | pruned = []
```

### 4.7 `GET …/audit?limit=10`（倒序、只存摘要）

```
{
  "items": [
    {"id":"aud_BCa6xft2qIn3oZXfOp","action":"enable","actor":"usr_BCa6v71FrbdXUwI4NJ",
     "at":"2026-11-18T18:45:53Z","detail":{}},
    {"id":"aud_BCa6woFZxSFZxMESIA","action":"test","actor":"usr_…",
     "at":"2026-11-18T18:45:43Z","detail":{"traceId":"evt_BCa6woFi5NpxX343UU","version":1}},
    {"id":"aud_BCa6wGK5XdfNtZLfld","action":"disable","actor":"usr_…",
     "at":"2026-11-18T18:45:38Z","detail":{"reason":"上游在改接口"}}
  ]
}
```

试跑的 `detail` 只有 `traceId` + `version`（`grep -c "manual\|diff"` = 0：参数全文没有进审计）。

### 4.8 `events.origin='test'` 的隔离口径

```
$ curl $B/v1/stats?namespace=default
total = 3 | today = 3 | daily = [{'day': '2026-11-18', 'count': 3}]
$ sqlite3 t.db "select count(*) from events"            → 4
$ sqlite3 t.db "select count(*) from events where origin='test'"   → 1
$ sqlite3 t.db "select count(*) from events where origin='source'" → 3
```

- `/v1/stats`（工作台口径）与 `/v1/admin/stats`（全站口径）**都不计 test 行**（L-D6）。
- ns 事件流默认不含 test；`?origin=test` 只看 test；按 `trace` 查**保留** test 行
  （否则 trace 全景会出现「有任务、没根事件」的孤儿视图）。
- 管理后台 / trace 页对 test 行给「手动试跑」标记（不静默隐藏）。

---

## 5. 测试清单与「改回去即红」证据

新增 **30 个** Go 测试函数（store 12 / route 6 / server 11 / ws 1）。

### 5.1 任务卡要求的每一条 → 对应用例

| 要求 | 用例 |
| --- | --- |
| disabled 时源事件不产生任务 | `TestPlanFromSourceDisabledTargetPruned`、e2e `capability-lifecycle` |
| **人工 test 仍然能跑** | `TestCapabilityTestAllowedWhileDisabled`、`TestPlanFromSourceDisabledTargetPruned`（反向）、e2e/api_contract |
| enable/disable 幂等（resourceVersion 不变） | `TestCapabilityLifecycleEnableDisableIdempotent`、`TestCapabilitySwitchRoundtripAndIdempotent`、api_contract |
| test 真建任务、真派发 | `TestAcceptTestEventCreatesRealTask`、`TestCapabilityTestAllowedWhileDisabled`、`TestCapabilityTestNoWarningWhenNodeHoldsDigest`、**`TestCapabilityTestEventIsDispatched`（真 hub 派发，见 §6.4）** |
| 未部署 → 200 + warnings（不是 4xx） | `TestCapabilityTestNotDeployedWarns`、api_contract + e2e |
| 参数错误 → 400 + `details.causes[]` | `TestCapabilityTestParamErrorIs`、api_contract + e2e |
| audit 能查到启停与试跑；append-only | `TestCapabilityLifecycleAuditList`、`TestCapabilityAuditLimit`、`TestCapabilityAuditActionClosedSet`、`TestCapabilitySwitchCascadeOnNamespaceDelete` |
| `origin='test'` 不影响 `/stats` 口径 | `TestStatsExcludeTestOrigin`、`TestEventOriginListFilters`、`TestEventOriginRoundtrip`、api_contract |
| 默认启用（无行 = enabled） | `TestCapabilitySwitchDefaultEnabled`、`TestPlanFromSourceEnabledRegression` |
| 只作用于被停的名字 | `TestPlanFromSourceDisabledOnlyNamedTarget`、`TestCapabilitySwitchPerNamespaceAndName` |
| emit 指向 disabled 下游 | `TestPlanFromEmitDisabledDownstream`、`TestPlanFromEmitDisabledPinnedRefLooksUpByName` |
| 参数原因优先于开关（设计图的顺序） | `TestPlanFromSourceDisabledStillReportsParamError` |
| 契约/鉴权 | `TestCapabilityLifecycleRequiresSession`、`TestCapabilityDisableBodyIsStrict`、`TestCapabilityTestUnknownVersionIs`、`TestCapabilityTestEventOriginInEventStream` |
| **试跑任务真派发** | `TestCapabilityTestEventIsDispatched`（internal/ws：真 store + 真 hub + 真 WS 帧，见 §6.4 与 §5.2 MUT F） |

### 5.2 变异实测（真跑出来的红，不是声称）

store 侧（`internal/store`）：

```
MUT A  去掉 /stats 的 `origin <> 'test'` 过滤
  → --- FAIL: TestStatsExcludeTestOrigin
    capability_test_event_test.go:163: Total 必须排除试跑：want 3, got 8

MUT B  删掉 SetCapabilitySwitch 里的审计写入
  → --- FAIL: TestCapabilitySwitchRoundtripAndIdempotent
    capability_switch_test.go:108: 三次启停应留三条审计，得到 0：[]

MUT C  无开关行的默认值写成 disabled
  → --- FAIL: TestCapabilitySwitchDefaultEnabled
    capability_switch_test.go:31: 无开关行必须等于 enabled，得到 &{… Enabled:false …}

MUT D  normalizeEventOrigin 把空 origin 归成 test
  → --- FAIL: TestEventOriginRoundtrip
    capability_test_event_test.go:35: 默认 origin 应为 source，得到 "test"

MUT E1 删掉 AcceptTestEvent 的任务身份一致性校验
  → --- FAIL: TestAcceptTestEventRejections
    capability_test_event_test.go:295: 任务身份不一致必须报错

MUT F  试跑只写事件、不建任务（去掉 createTasksTx，ws 侧派发用例）
  → --- FAIL: TestCapabilityTestEventIsDispatched
    capability_test_dispatch_test.go:81: 读回试跑任务: store: record not found

MUT E2 能力行不存在时放行（= 去掉活跃身份校验）
  → --- FAIL: TestAcceptTestEventRejections
    capability_test_event_test.go:305: 能力行不存在应报 ErrNotFound，得到
    insert task 0 (review): constraint failed: FOREIGN KEY constraint failed (787)
    （第二道防线：tasks 外键自己也会拦住悬空能力行）
```

route 侧（由实施者实测，两次变异后已从备份恢复、`diff -q` 一字节一致）：

```
MUT A  两处闸门改成 `accepting, aerr := true, nil`（= 没有闸门）
  → switch_test.go:56  停用的能力不得是 ready（那会建任务）: [... State:ready ...]
  → switch_test.go:109 review 必须被剪: {... State:ready ...}
  → emit_test.go:689   停用下游必须回 taskId:null，得到 {TaskID:0x… Pruned:<nil>}
  （4 条红；默认启用回归与参数优先级 2 条仍绿 = 符合预期）
MUT B  闸门前移到参数校验之前（开关优先的错实现）
  → switch_test.go:184 参数原因优先于开关：reason = "capability_disabled"，want param_missing
```

### 5.3 「改回去即红」的判据（每条一句话）

- `TestPlanFromSourceDisabledTargetPruned`：删闸门 → 该 target 回到 `StateReady` 并建出任务。
- `TestPlanFromSourceDisabledOnlyNamedTarget`：删闸门 → 两条 target 都 ready、tasks 变 2。
- `TestPlanFromEmitDisabledDownstream`：删闸门 → 回执给 `taskId`、建出 1 条子任务、父任务链路被拖。
- `TestPlanFromSourceEnabledRegression`：把默认值反过来（无行当 disabled）→ 无行分支被剪。
- `TestPlanFromSourceDisabledStillReportsParamError`：闸门前移 → reason 变 `capability_disabled`。
- `TestCapabilitySwitchDefaultEnabled`：默认值写反 或 读路径开始写行 → 红（两条断言）。
- `TestCapabilitySwitchRoundtripAndIdempotent`：审计写入删掉 / 覆盖语义改成「幂等就跳过」→ 红。
- `TestCapabilityAuditLimit`：夹取边界改掉 / 排序改升序 → 红。
- `TestCapabilityAuditActionClosedSet`：去掉 Go 侧闭集校验 → 红；去掉 DB CHECK → 裸 SQL 用例红。
- `TestEventOriginRoundtrip`：默认值改 test → 红；去掉 DB CHECK → 裸 SQL 用例红。
- `TestEventOriginListFilters`：事件流恢复成「包含 test」/ 按 trace 开始排除 test → 红。
- `TestStatsExcludeTestOrigin`：任一 SQL 去掉 `origin <> 'test'` → 红（Total/Today/Daily/全站）。
- `TestAcceptTestEventCreatesRealTask`：事件 origin 写错 / 审计 detail 塞 params / 任务不钉 digest → 红。
- `TestAcceptTestEventRejections`：任一校验删掉 → 红（且 0 写断言会额外报「不该有行」）。
- `TestCapabilityLifecycle{EnableDisableIdempotent,EnableDisableRoundTrip,AuditList,RequiresSession,...}`：见 §5.2/§6 实测。
- e2e `capability-lifecycle`：把闸门去掉 → 「停用期间触发 0 任务」变 1 → 红；把 enable 按钮接错 → 徽标断言红。

---

## 6. e2e

### 6.1 `api_contract`（HTTP 契约）

新增一段覆盖四个端点：匿名 401（POST 动作端点要带 method，否则落到静态兜底 404——这条本身也是用例）、
默认 `enabled=true`、disable 200 + RV 不变、重复 disable 幂等、disabled 上 test 仍 200、
未部署 `warnings[capability_not_deployed]`、试跑任务 `pending`、参数 400 + `causes[].field=params.diff`、
不存在版本 404、跨 ns 404、enable 200、审计倒序/`limit`/`actor`/RFC3339/`detail` 无参数全文、
`/v1/stats` 事件总数不被试跑抬高、事件流默认无 test + `?origin=test` 只看 test + `?origin=replay` 400。

```
./scripts/e2e/run_all.sh api_contract
 断言通过: 220   断言失败: 0   mode=real
```

### 6.2 `frontend`（真实权威 UI 端到端）

新 case `capability-lifecycle`（7 条行为断言），全部走产品路径（页面按钮 + 真实 HTTP）：

1. 默认「已启用」徽标 + 「停用」按钮（无开关行 = enabled）；
2. 点「停用」→ `confirmDanger` 真确认 → 徽标「已停用」→ 返回整个 `Capability`（`status.enabled=false`）；
3. 停用期间用真实入口 `POST …/sources/{name}/fire`：**tasks 为空 + pruned[capability_disabled]**，
   并且 trace 上留了 `capability_disabled` 事件（不是只在回执里说一句）；
4. 重新进页面仍是「已停用」（持久），点「启用」→ 徽标回「已启用」；
5. 同一源再触发 → 真任务 `pending`、能力名正确（停用只拦自动触发，不是删功能）；
6. 试跑弹窗：填参数 → 提交 → 200 回执 `origin=test` + 1 个真任务 + `traceId` 显示在面板；
7. 审计区读到 enable / disable / test 三条且**最新在前**。

```
./scripts/e2e/run_all.sh --serial frontend
 断言通过: 40   断言失败: 0（含 capability-lifecycle 7 条）
```

### 6.3 全量 e2e（CI 的 affected 判定 = 全量）

```
$ node scripts/e2e/affected.mjs --base origin/main
  ⚠️ 全量 e2e（28/28 套）：api/events.yaml 落在共享面（五份 OpenAPI 真源）

$ E2E_CONCURRENCY=2 ./scripts/e2e/run_all.sh
✅ E2E 全绿：28/28 个请求套件均达标（断言 2352 通过 / 0 失败，authority 清单覆盖 28 个）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0）

  api_contract 220/0 · frontend 40/0 · events_paging 178/0 · routing 168/0 · contract 181/0
  ws_protocol 134/0 · cli_auth 133/0 · security 100/0 · orphans 98/0 · deploy 93/0
  node_v4 88/0 · ui_shift 87/0 · lang_hint 86/0 · passkey_enroll 85/0 · capacity 84/0
  persistence 82/0 · password_login 78/0 · edge_cases 77/0 · live_agent 62/0 · admin_flow 52/0
  deeplink 50/0 · auth_flow 49/0 · i18n_coverage 44/0 · i18n 33/0 · p2_guards 30/0
  ux_regression_19_23 8/0 · passkeys 7/0 · recovery_next 5/0
```

过程中修掉的一个真问题：explicit-mock 套件（i18n / i18n_coverage / ui_shift）在能力详情页会调
`GET …/audit`，而 `web/mock.js` 当时没实现 → 页面 `console.warn("mock 未实现…")` →
三套件 28 条断言变红。已在 `web/mock.js` 补齐四个端点（`enable`/`disable`/`test`/`audit`）与
`status.enabled` 派生，重跑三套件 164 断言全绿。

### 6.4 「真派发」的验证方式（任务卡要求写清用哪条）

两层都做了，**没有**用 mock 节点：

1. **Go 侧（生产调度入口）**：`internal/ws/capability_test_dispatch_test.go` 的
   `TestCapabilityTestEventIsDispatched` 用真 store + 真 `ws.Hub` + 真 WS 帧（脚本化节点客户端）：
   `db.AcceptTestEvent`（HTTP handler 背后的同一条入口）→ 事件 `origin='test'` + 任务 `pending`
   → `hub.DispatchOnce`（生产调度入口）→ 节点侧收到 `task.dispatch`，帧里的
   `taskId` / `namespace` / `capability@version` / `params` 逐字对得上。
   改回去即红（§5.2 MUT F：不建任务 → `读回试跑任务: record not found`）。
2. **e2e 侧（真实权威，无节点）**：`frontend` 套件里没有起 anode，所以试跑回执拿到的是
   `capability_not_deployed` 警告 —— 这正好把「未部署 → 200 + warning（不是 4xx）」
   验成真实事实。「真节点跑完整条链」由既有 `node_v4` 套件守护，本 PR 没有新增真节点用例。

## 7. 与设计/任务卡的偏差与发现（重要）

1. **`status.observed.pruned` 承载不了「被完全剪掉的触发」**（设计 §10 的期望需要修订）。
   `observedPrunes` 以**最近一个任务**的 (traceId, taskId) 为键；一次被全部剪掉的触发没有任务，
   于是 `status.observed = {}`，剪枝看不见。证据改为**回执 `pruned[]` + trace 事件**
   （本 PR 的 e2e 断言走这两条，并在 `docs/design/capability-lifecycle.md` 的既有文字之外明确记录）。
   若产品要在能力页显示「停用期间的剪枝计数」，需要一个新的读模型（不在本卡范围）。
2. **任务卡的「已有 dev 库原地升级」与设计 §5.4 + store 模块约定冲突**：设计明确写「老库被具名拒绝，
   开发机删库重建」，模块约定写「引入第 2 块需单独授权」。本 PR 未改迁移引擎，改为交付
   一条运维 DDL（`scripts/migrations/capability-lifecycle.sql`）并**实测**数据不丢、新二进制可启动（§2.2）。
   若后续要变成自动升级，需要单独授权改造 `baselineLayout()` 支持多块。
3. **参数错误优先于 `capability_disabled`**：设计 §6.1 的图把开关判定放在 `PrepareParams` 之后，
   照做。因此「停用 + 参数也错」报 `param_missing`。有测试钉住（防止后人「顺手把开关前移」）。
4. **试跑事件的 `events.source` 写固定文本 `capability`**：设计只规定「不复用 source 列做判据、
   展示由 origin 驱动」，没规定写什么。选 `capability` 并常量化为 `store.EventTestSource`；
   展示与统计一律看 `origin`，不看这个字面量。
5. **`403` 在资源域实际不可达**：声明了 `Status403`（设计 §4.1 的响应表），但
   `authorizePathNamespace` 的判据是「user 存在 + ns 存在」（跨 ns 是 404），资源域今天不产生 403。
   e2e 因此断言 404（不泄露存在性），403 保留在契约里等per-ns 授权落地。
6. **`playbook.html` 的 values 表单没有切到共享 `schemaForm`**：`valuesForm()` 带 writeOnly 遮盖、
   JSON 直填覆盖、`previewMask` 等专属逻辑，切换属于跨页面重构、风险与本卡目标无关。
   共享函数 `Anotify.schemaForm` 已落在 `web/partials.js`（试跑弹窗在用），playbook 的迁移留给后续票。
7. **`POST …/test` 的 `nodes` 不改变派发目标**：设计写「限定目标节点」，但 v4 的派发在
   `lease.grant` 时按「在线 + 授权 + 已 applied + 持有所需 digest」选点，**任务行不钉节点**
   （`tasks.node_id` 是派发结果，不是约束）。要真正钉节点需要 task 级的节点约束字段 ——
   那是新的 schema + 派发语义，属于单独设计。因此本 PR 的 `nodes` 只用于回执 `warnings`
   的判定，并在 OpenAPI 描述里**写明**这一点（不让文档骗人）。
8. **管理后台「可见标记」的实现范围**：`/v1/console/events` 已带 `spec.origin`，`events.html`（全站事件流）
   与 `trace.html`（链路全景）都已给「手动试跑」标记；首页工作台的事件流**没有**加标记
   （那是同一条 API 的另一个投影，加标记会动 `index` 套件的既有断言，未在本卡范围）。

---

## 8. 门禁结果

### 8.1 `make ci`（lint + unit + 契约 + 前端产物）

```
make ci   → EXIT=0
  check-annotations   unbacked 300 / 基线 300（新增 2 条曾超标，已改成可执行断言面）
  lint-go             gofmt / 包注释 / go vet / staticcheck SA*,U1000 全绿
  fe                  check-classes / check-wire-fields / check-css / check-i18n-keys /
                      check-contrast / sitegen / hash / dist-stamp 全绿
  contract            A2 + check_contract.py + check_openapi.py + TestOpenAPIParity + build anode
  go test ./...       41 个包 ok（1929 个 Test 函数，本 PR 新增 30 个）
  bench-dispatch / harness_selftest  通过
```

### 8.2 e2e

```
make ci                                   → CI_EXIT=0
E2E_CONCURRENCY=2 ./scripts/e2e/run_all.sh → E2E_EXIT=0
  28/28 套件 · 断言 2352 通过 / 0 失败 · authority real 24 / explicit-mock 4
```

### 8.3 复算命令（本报告里的定格数字都能重跑）

```
make ci                                            # lint + unit + 契约 + 前端产物 + 注解守卫
node scripts/check-annotations.mjs                 # 注解即外键（unbacked/unmarked 棘轮）
E2E_CONCURRENCY=2 ./scripts/e2e/run_all.sh         # 全量 e2e
./scripts/e2e/run_all.sh api_contract              # 四个端点的 HTTP 契约
./scripts/e2e/run_all.sh --serial frontend         # UI 端到端（含 capability-lifecycle）
go test ./internal/store/ ./internal/route/ ./internal/server/ ./internal/ws/ -count=1
```

### 8.4 其它自查

```
gofmt -l internal/…            → 空
staticcheck -checks=SA*,U1000 ./internal/store/... ./internal/route/... ./internal/protocol/... → 无输出
go build ./...                 → 通过
```
