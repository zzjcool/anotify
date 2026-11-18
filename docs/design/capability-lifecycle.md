# 能力启停与手动测试（capability lifecycle） · 设计草案

> 状态：**草案（未实现；实现前先冻结本文件）**
> 上游：`docs/design/v4/capability.md`（`kind: Capability` 唯一定义）、`docs/design/v4/source.md`（触发入口）、`docs/design/v4/decisions.md`（`V4-D1` 轻量资源 API）、`docs/design/v4/api-conventions.md`（错误体与 `reason` 封闭集合）
> 定位：给「能力」补两件运维动作——**启停**与**手动跑一次**。不改执行语义、不改 digest 规则、不引控制循环。

## 0. 一句话

**「停用」是路由期的闸门（不建新任务），不是任务的暂停；「试跑」是一次带标记的真实运行（建真任务、走真派发），标记落在事件上而不是新开一条建任务路径。**

## 1. 术语

| 词 | 含义 | 不是什么 |
| --- | --- | --- |
| **enabled** | 该能力**接受新的触发**：源事件路由到它、上游 emit 指向它，都会正常建任务。默认值。 | 不是「已部署」（部署是事实，见 §4.3）；不是 `ready`（那个还看命令/节点/下游） |
| **disabled** | **拒绝新触发**：路由期不建任务，留在剪枝事实里。**进行中的任务不受影响**——不取消、不暂停、不等它跑完才生效。 | 不是删除（`deleted_at` 是删除，删除后连读都 404）；不是 `sources.enabled` 的替代 |
| **test-run** | 人工指定一个能力 + 一份参数，**真的跑一次**：建真任务、真派发、真执行、真出结果。 | 不是 dry-run（`sources` 的 dry-run 是 0 写入预览）；不是「重新触发历史事件」 |
| **开关行** | `capability_switches` 里按 `(namespace_id, name)` 存的一行。**没有行 = enabled**。 | 不是 `spec` 字段（`spec` 一变就换版本，见 §5.1） |

术语冲突提醒：`sources.enabled` 已存在且语义更窄（是事件的**入口**开关）。本设计**不**动它；两层的闸门互不替代——源停用 = 事件根本进不来，能力停用 = 事件进来了但这条边不建任务。

## 2. 现状：能力没有任何启停语义

| 事实 | 证据 |
| --- | --- |
| `capability_versions` 没有 `enabled` / `status` / `paused` 列；唯一的生命周期标记是 `deleted_at`（软删 tombstone） | `internal/store/schema.sql` 的 `capability_versions` 段 |
| `status` 全部读时派生、不落库；写请求带非空 `status` → `400 status_not_writable` | `docs/design/v4/capability.md` §5、§2.4 |
| 路由决策链（`PlanFromSource` / `PlanFromEmit`）只做「引用解析 → 参数渲染 → 校验」，**没有**任何能力级开关判断 | `internal/route/route.go` 的 `PlanFromSource` / `PlanFromEmit` |
| 唯一现存的 `enabled` 列属于**源**（`sources.enabled`），判定点在 HTTP 入口 + hook 查表 + cron 查询 | `internal/source/source.go` 的 `Fire`；`internal/store/source.go` 的 `LookupHook` / `ListDueSources` |
| 建任务的生产路径只有两条：命令侧接收事务（`AcceptSourceEvent`）与 emit 授权事务（`EmitAuthorized`）。`store.CreateTasks` 生产无调用方 | `internal/store/source_event.go`、`internal/store/emission.go`、`internal/store/task.go` |
| 没有审计表，也没有任何表带 actor/操作者列（只有归属用的 `user_id`） | `internal/store/schema.sql`；操作者只进请求日志 |

所以两件动作都要**从零加**，而且要加在既有的单一路径上，不新开旁路。

## 3. 状态机

```text
                    POST …/enable（幂等）
      ┌──────────────────────────────────────────────┐
      │                                              ▼
  ┌───────────┐                                ┌───────────┐
  │ disabled  │                                │  enabled  │   ← 默认（无开关行 = enabled）
  └───────────┘                                └───────────┘
      ▲                                              │
      └──────────────────────────────────────────────┘
                    POST …/disable（幂等）
```

**迁移只有两条边，都是显式人动作，都是幂等的**（对已经是目标状态的资源再调一次 = 200 + 无副作用，但**仍记一条审计**，因为「我又确认了一次」也是操作事实）。

| 问题 | 决定 | 理由 |
| --- | --- | --- |
| 谁能操作 | 对本命名空间有写权限的会话（与 `deploy` / `PATCH` 同一条 `authorizePathNamespace` 判据）。v1 **不引新权限位** | 停用是可逆的低危动作；为它发明「operator」角色会立刻与现有 member/admin 二元打架 |
| 已排队 / 进行中任务 | **完全不受影响**：不禁用、不取消、不暂停、不改 `next_attempt_at` | 「停用」是「别再起新的」，把在途任务砍掉会让链路半途断在中间，比不停更危险 |
| 停用时要不要清空 `pending` 任务 | **不做**（见 §9） | 清空队列是另一个动作（批量取消），需要它的用户可以用既有的任务取消面 |
| 停用后 `deploy` / 建新版本 / 删除 | **照旧可用** | 部署是「把字节放到节点」，不是触发；把两者绑在一起会让「先停用再升级」这种正常运维做不了 |
| 停用与 tombstone 的关系 | 删掉能力后开关行随 `namespace_id` 级联消失；重建同名能力 = **新身份**，从 enabled 开始 | 不留跨身份的开关残留（与 `event_deliveries` 按稳定 id 去重同一条口径：名字不是身份） |
| 停用的生效时机 | **下一次触发判定**（读时判定，不推帧、不通知节点） | 见 §6.2 的竞态说明：这是「最终停」不是「立刻停」 |

**状态机之外的一件事实**：`status.enabled` 是**派生读投影**（从开关行读），不是把 status 落库。`V4-D1` 禁的是「把派生 status 整块落库」，不是「不许有持久化的期望状态」。这里持久化的是**开关这个事实**，`status.enabled` 只是它的读时投影——与 `status.ready` 从 `deployments` 派生同构。

## 4. API 草案

四个新路径，全部挂在 `/v1/namespaces/{namespace}/capabilities/{name}` 下，tag `capabilities`，`security: [{session: []}]`。

### 4.1 `POST …/capabilities/{name}/enable` / `…/disable`

```yaml
/v1/namespaces/{namespace}/capabilities/{name}/disable:
  post:
    operationId: disableCapability
    tags: [capabilities]
    security: [{session: []}]
    summary: 停用能力（拒绝新触发；进行中的任务不受影响）
    requestBody:
      required: false
      content:
        application/json:
          schema:
            type: object
            additionalProperties: false
            properties:
              reason: {type: string, maxLength: 200}   # 写进审计；不进 status
    responses:
      '200': {description: 停用后的能力（含 status.enabled=false）, schema: Capability}
      '400': Status400
      '401': AuthError401
      '403': Status403
      '404': Status404
      '500': Status500
```

- `enable` 同形，无 body（或忽略 body），返回 `status.enabled: true`。
- 返回**整个 `Capability`** 而不是 `204` 或 `OkResult`：页面拿到新 `resourceVersion` 才能继续做 PATCH/删除，省一次往返。
- **不套 CAS**（与 `deploy` 一致：动作 POST 不带 `resourceVersion`）。两个管理员同时按 = 最后写赢，两条审计都留。想严格串行化的用户没有这个需求的证据；引入 CAS 反而让「点了没反应」变成常见失败。
- 幂等：重复调 = 200 + 不动 `updated_at`？**动**。`updated_at/updated_by/reason` 每次覆盖（= 最近一次操作是谁、为什么），审计留全history。

### 4.2 `POST …/capabilities/{name}/test`

```yaml
/v1/namespaces/{namespace}/capabilities/{name}/test:
  post:
    operationId: testCapability
    tags: [capabilities]
    security: [{session: []}]
    summary: 手动跑一次该能力（建真任务；事件带 origin=test 标记）
    requestBody:
      required: false
      content:
        application/json:
          schema:
            type: object
            additionalProperties: false
            properties:
              version:
                type: integer
                minimum: 1
                description: 省略 = 当前 live 版本（与 PATCH/deploy 同一条解析）
              params:
                type: object
                additionalProperties: true
                description: 该能力 spec.params 的实参；服务端按 JSON Schema 校验
              nodes:
                type: array
                items: {type: string}
                description: 限定目标节点；省略 = 全部持有所需 digest 的在线节点
    responses:
      '200':
        description: 接收回执（同 fireSource 的形状口径）
        content:
          application/json:
            schema: CapabilityTestReceipt
      '400': Status400            # 参数不合 schema / 版本号非法
      '401': AuthError401
      '403': Status403
      '404': Status404            # 能力或指定版本不存在
      '500': Status500
```

```yaml
CapabilityTestReceipt:
  type: object
  required: [traceId, origin, tasks, pruned, warnings]
  properties:
    traceId: {type: string}
    origin: {type: string, enum: [test]}
    tasks:
      type: array
      items: {$ref: '#/components/schemas/SourceReceiptTask'}   # taskId/capability/version
    pruned:
      type: array
      items: {$ref: '#/components/schemas/PruneFact'}
    warnings:
      type: array
      items: {type: object, required: [reason], properties: {reason: {type: string}, detail: {type: string}}}
```

**为什么是 `200 + 回执`，不是 `202`**：既有 `fireSource` 就是 200 + 接收回执（`api/ingress.yaml` 的 `fireSource`、`SourceReceipt`），它的语义同样是「已建任务、还没跑完」。同一个语义不该有两种状态码。回执里的 `tasks[].taskId` 是真任务 id，用户点进去看 trace —— 与源触发的回执完全同构。

**为什么 `test` 允许在 disabled 上调用**：disable 拦的是**自动**触发；人工显式动作不是自动触发。若反过来要求 enable → test → disable，用户几乎一定忘记最后一步，停用状态反而会被当成一次性开关丢掉。

**没有「未部署」的 4xx**：一个都没有节点持有该 digest 时，任务会停在 `pending`（不报错）。这既符合既有事实（`EligibleNodesForTask` 无 digest 直接返回空候选），也不该把「现在没在线机器」伪装成请求错误——回执里给一条 `warnings[{reason: capability_not_deployed}]`，页面据此提示「已排队，等节点上线」。

### 4.3 与 deploy 的关系

| 动作 | 做什么 | 与 enabled 的关系 |
| --- | --- | --- |
| `POST …/deploy` | 写「某节点持有某 digest」事实 + 尽力推 `capability.deploy` 帧 | **无关**：disabled 的能力照样可以部署（先部署、等验证通过再启用，是最常见的顺序） |
| `POST …/test` | 建真任务 | 需要节点**已经持有**该 digest 才会真的跑起来（否则 pending + warning） |
| PATCH（换版本） | 新版本行 | 开关跟着 **name** 走，换版本不重置；不需要「重放开关」 |
| DELETE | tombstone | 开关行级联消失；重建同名能力从 enabled 开始 |

**明确不把 deploy 变成「启动」**：部署是二进制分发，启动是流量准入。合并会让「暂停业务但升级依赖」这类操作无解。

### 4.4 新增的 `reason` / 剪枝原因

| 码 | 用在哪 | 语义 |
| --- | --- | --- |
| `capability_disabled` | **剪枝原因**（进 `events` + `status.observed.pruned[].reason`）；emit 回执的 `pruned` | 「这条边指向的能力被停用了」——事件照收 200，不建任务 |
| `capability_not_deployed` | test 回执的 `warnings[]`（**不是** 4xx、不是任务失败码） | 「已排队，但当前没有节点持有需要的 digest」 |

**为什么停用不返回 4xx**：事件入口的 200/4xx 语义是「这次投递**收下了没有**」（与幂等键、背压同层）。能力停用是**下游不接**，与 `downstream_missing` / `param_missing` 同一类——它们是观察原因，不是投递失败。给源调用方回 409 会让它以为该重试。

同步要改的封闭集合：`internal/protocol/enums.go` 的 `pruneReasons`、`api/events.yaml` 里写枚举的描述、`docs/design/v4/capability.md` §8.3 的剪枝原因清单。

## 5. 数据模型

### 5.1 `capability_switches`（新增表，一行 = 一个能力的开关）

```sql
CREATE TABLE IF NOT EXISTS capability_switches (
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    reason       TEXT NOT NULL DEFAULT '',
    updated_at   INTEGER NOT NULL,
    updated_by   TEXT NOT NULL DEFAULT '',   -- users.id；空 = 系统/未归属
    PRIMARY KEY (namespace_id, name)
);
```

- **无行 = enabled**。所以没有任何回填：老库升级后所有能力天然是「启用」。
- 主键是 `(namespace_id, name)`，**不是** `capability_versions.id`：开关是**名字**的属性，换版本不该重置它，历史版本也不该各带一个开关。
- **为什么不加列到 `capability_versions`**：那是 append-only 的版本表（一行 = 一个 `name@version`）。把开关放进去要回答「旧版本行的开关值算什么」，还得在每次 PATCH 建新版时复制一份——等于把一个布尔值复制到每个历史版本上，并且让「这个能力现在是启用吗」变成一次带 `WHERE deleted_at IS NULL ORDER BY version DESC` 的查询。
- **为什么不放进 `spec`**：`spec` 参与 digest 与版本寻址（`capability.md` §3）。把开关放进 `spec` 意味着停用 = 一次内容变更 = 新版本 + 新 digest + 要求重新部署；这不是运维动作该有的代价。
- 与 `sources.enabled` 的**不对称是有意的**：源的 `spec` 可以就地改（PATCH 同一个源行），能力的 `spec` 不行。

### 5.2 `capability_audit`（新增表，append-only）

```sql
CREATE TABLE IF NOT EXISTS capability_audit (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    action       TEXT NOT NULL CHECK (action IN ('enable', 'disable', 'test')),
    actor_id     TEXT NOT NULL DEFAULT '',   -- users.id；空 = 未归属（CLI/系统）
    detail       TEXT NOT NULL DEFAULT '{}', -- reason / traceId / version / 参数摘要（脱敏）
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cap_audit_ns_name
    ON capability_audit(namespace_id, name, created_at DESC, id DESC);
```

- **为什么不复用 `events`**：`events` 是「只存不解释的原始输入」，它的消费者是事件时间线、`/stats` 计数、trace 全景（都不带 actor）。把审批类记录塞进去，会同时污染统计口径和「事件内容」的语义，而 `events` 又没有 actor 列可加。
- `detail` 只存**摘要**：test 记 `traceId` + `version`，不存完整 params（参数里可能有业务数据）。
- 读取：`GET …/capabilities/{name}/audit?limit=`（默认 50，`1..200`），返回 `{items: [{action, actor, at, detail}]}` 按时间倒序。**审计若没有读接口就等于没写**，所以这条读端点和写表一起进 v1。

### 5.3 `events.origin`（新增列）

```sql
-- events 增加一列：这条事件是「外部投递」还是「人工试跑」
origin TEXT NOT NULL DEFAULT 'source' CHECK (origin IN ('source', 'test'))
```

- 默认 `'source'` 覆盖全部存量行；不需要回填。
- **为什么不复用 `source` 列**：`events.source` 存的是**人类可读的源名**（列表/详情要给人看）。试跑没有源可写；写个 `test` 字面量会和「用户真的建了一个叫 test 的源」混成一类，且无法作为稳定的过滤判据。`source='route'` 已被内部剪枝事实占用并有拒写守卫（`store.ErrReservedEventSource`），不能再借。
- 页面上 `source` 栏对 test 行显示固定文案（「手动试跑」），由 `origin=test` 驱动，不由 `source` 字面量驱动。
- **任务不加 `origin` 列**：任务是不是试跑，由它所属 trace 的根事件决定（一条事件建出的任务同属一个 trace）。v1 接受「试跑任务会出现在 `status.observed`」——那是**真实执行**的诊断事实，隐藏它等于对用户撒谎（详见 §9 的取舍）。

### 5.4 迁移方式

当前迁移引擎只支持**单块 baseline**（`headVersion` 保持 1，`baselineLayout()` 硬校验），所以上面三处 schema 变更都是**在 baseline 块内原地改**：

1. `internal/store/schema.sql` 原地改（三处），**不写** `-- +migration 2`。
2. 同步 `internal/store/schema_baseline_test.go` 的必需列/表清单。
3. 每个新列/新表都加往返单测（存什么读什么），以及「无开关行 = enabled」的默认值断言。
4. 老库：`Open` 会遇到 `ErrSchemaInvalid`（缺列/缺表）并**具名拒绝**，不迁移、不删数据；开发机需要删库重建。CI 全用 fresh 库，抓不到这条，故在第 3 步里显式覆盖默认值行为。

## 6. 触发链路：改哪里

### 6.1 闸门位置（读时判定）

```text
源事件 / hooks / fire ─┐
                       ├─► Router.PlanFromSource ──► 每个 target：
cron ──────────────────┘        │                    resolveCapability
                                │                    RenderParams → PrepareParams
                                │                    ★ 读开关（新）
                                │                      enabled  → 建任务（照旧）
                                │                      disabled → 剪枝事实 capability_disabled
                                ▼
                        AcceptSourceEvent（单事务：事件 + 剪枝 + 任务 + 回执）

上游 emit ──► Router.PlanFromEmit ──► 同一个 ★ 判断：
                                       disabled → 回执 taskId:null + capability_disabled
```

- 判定函数只加在一处，`PlanFromSource` 与 `PlanFromEmit` 共用（避免两条路径各写一份「这个能力能不能接」）。
- **判定在计划期，不在事务内**：与既有 `sources.enabled` 的判定位置一致（HTTP 入口 / hook 查表 / cron 查询都是计划期判定）。代价见 §9 的竞态条目。
- 关闭的能力**不参与** `status.observed` 的剪枝计数空转：剪枝事实照写（这是「为什么没跑」的唯一证据），但不算失败。

### 6.2 试跑链路（复用同一事务）

```text
POST …/capabilities/{name}/test
  ├─ 解析能力 + 版本（与 deploy 同一条 ResolveRef）
  ├─ 渲染/校验 params（与源路由同一条 PrepareParams；失败即 400 + details.causes[]）
  ├─ 候选节点（省略 nodes = 持有该 digest 的在线节点；只用于 warning，不阻断）
  ├─ AcceptTestEvent（事务内：写 events(origin='test') + 建 tasks + 写回执）
  └─ appendAudit(action='test', detail={traceId, version})
```

新增的 store 函数 `AcceptTestEvent` 与 `AcceptSourceEvent` **共享同一个事务内核**（计划 → 任务行 → 回执），只在两处不同：事件行的 `origin` 与 `source`，以及不写源去重记录（`event_deliveries` 属于源投递）。

**绝不新开第三条建任务路径**：`createTasksTx` 的两条调用方（接收事务、emit 事务）已经把「任务行必须钉住版本与 digest」「未解析失败行的形状」这两条 CHECK 约束吃透了；旁路建行会立刻踩到 `tasks_resolved_pin_shape`。

## 7. 前端（`web-src/pages/capability.html`）

| 元素 | 现状 | 改成 |
| --- | --- | --- |
| 状态徽标 | 页面只有版本徽标（`badge badge-accent`）与节点状态点 | 顶部加 `已启用`（`badge badge-success`）/ `已停用`（`badge badge-neutral`），数据来自 `status.enabled` |
| 动作条 `#cap-actions` | 编辑 / 部署 / 删除 | 追加 `启用` / `停用`（按状态二选一）与 `试跑`；停用走二次确认（`Anotify.confirmDanger` 已存在） |
| 试跑弹窗 | 无 | 新增 `#test-modal`，**照抄** `sources.html` 的 `#fire-modal` 范式（`openFire()` → textarea → `/fire` → 回执面板） |
| 弹窗字段 | 无 | `version`（默认当前 live 版本，可改）、`params`（见下）、`nodes`（可选，来自 `status.deployCandidates`） |
| 回执面板 | 无 | `traceId` 链接到 trace 页；`tasks[]` 逐条显示 phase；`pruned[]` / `warnings[]` 用既有 `badge-warn` |
| 审计 | 无 | 详情页底部「最近操作」折叠区，读 `GET …/audit?limit=10`（谁、何时、什么动作） |

**参数表单 = 沿用 `spec.params` 的 JSON Schema（注意：不是 `spec.values`）**：

- 能力的 JSON Schema 在 `spec.params`；`spec.requires.values` 只是**变量名字符串数组**（不是表单描述）。所以「复用 spec.values 渲染」这个说法在契约里没有对应物。
- 现成的最接近实现是 `web-src/pages/playbook.html` 里的 `valuesForm()`（按 schema 渲染控件 + 收集值），但它是**页面内闭包**，没有挂到 `window.Anotify`。
- 做法：把它抽成共享的 `Anotify.schemaForm(schema, {values, onChange})`（`web/partials.js`），试跑弹窗与 playbook 的 values 表单共用。抽不出来的极端 schema（`oneOf` / 深层 `$ref`）降级为一个 JSON textarea + 客户端 `JSON.parse` 校验，服务端仍是最终判据（`details.causes[]` 直接映射到字段旁）。
- 新 class 必须先落 `web/ui.css`（否则 `make fe` 的 class 守卫直接红）；新文案要四份 locale 齐（`web-src/locales/`）。

## 8. 审计

| 动作 | 谁 | 记在哪 |
| --- | --- | --- |
| enable / disable | 会话用户（`auth.UserIDFromContext`） | `capability_audit`：`action` + `actor_id` + `detail.reason` + `created_at`；同时覆盖 `capability_switches.updated_by/updated_at/reason`（= 最近一次） |
| test | 同上 | `capability_audit`：`action='test'`，`detail={traceId, version}`；**不**把 params 全文写进去 |
| 自动触发的剪枝 | 无 actor | 只进 `events`（剪枝事实）；不进审计表——审计面只记人动作 |

约定：

- 审计行**不可修改、不可删除**（v1 没有删除端点）。
- 保留期：v1 **不接**现有的 retention（那是事件/任务/日志的），审计行只增不删；写量 = 人工动作次数。若产品要自动过期，需要单独决定保留期（见 §9）。
- `actor_id` 为空串是合法值：用 CLI / 系统 token 操作时没有会话用户。**不**为了凑值伪造 `system` 用户。

## 9. 风险与不做的事

**取舍一：试跑写成真事件（选定）vs 不写事件**

| 方案 | 好处 | 代价（实测事实） |
| --- | --- | --- |
| **A. 写事件 + `origin='test'`（选定）** | 复用唯一接收事务（事件 / 剪枝 / 任务 / 回执同事务）；trace 页能看全过程；retention 的「trace 上还有任务就不删事件」不变量自然成立 | 所有读 `events` 的地方都要决定怎么看待 test 行：`/stats` 与 `/v1/admin/stats` 是 `COUNT(*)` 无过滤，管理后台全站事件流无过滤，ns 事件流默认只排除 `source='route'`。**必须同步加过滤**，否则测试污染统计 |
| B. 不写事件、直接建任务 | 不污染事件面 | 需要第三条建任务路径（`CreateTasks` 生产无调用方，绕 `createTasksTx` 会撞 `tasks_resolved_pin_shape`）；trace 页出现「有任务、没有根事件」的孤儿视图；没有「为什么这次跑了」的事件证据 |
| C. 做成 dry-run（0 写入预览） | 零污染 | 那不是「跑一次」，用户要的是**真结果**；dry-run 可作 v1.1 的附加动作，但不能替代 |

**取舍二：试跑任务出现在 `status.observed` 里（接受）**

`status.observed` 取「该能力最近一个任务」，试跑会成为最近任务、并进 24h 窗口计数。接受，因为：试跑是**真实执行**，把它从诊断里摘掉会让「刚刚明明失败了，页面显示上次成功」这种更难查的现象出现。要区分的是**统计口径**（`/stats` 的事件数）而不是执行诊断。

**风险清单**

| 风险 | 说明 | 缓解 |
| --- | --- | --- |
| 停用与在途触发竞态 | 开关在计划期判定：一条已通过判定、正在建事务的触发仍会建出任务 | 语义上就是「最终停」；文档（本文件 + API description）写明一次触发内不重判；不引分布式锁 |
| 停用被当成删除 | 用户以为停用后不再占节点、不再算配额 | UI 徽标明确「已停用：不再接受新触发」；API description 写明与 `DELETE` 的差别；部署/版本仍存在 |
| 试跑被当成生产后门 | 停用的能力仍可被 test 触发 | 审计 + 事件标记 + UI 明确提示；若将来要收紧，可加「测试白名单节点」而不是改 v1 语义 |
| 审计表无界增长 | v1 不清理 | 写量与人动作同阶；读取有 `limit`；保留期作为独立决策（不进 v1） |
| test 参数泄漏 | params 可能含业务数据 | 审计只存 `traceId` + `version`；事件 payload 本来就是原样存储（与源投递同一条口径，不新增泄漏面） |
| `origin` 列影响事件读路径 | 五处读 SQL 要决定过滤口径 | 实施清单里逐条列出（`store/event.go`、`store/event_page.go`、`store/stats.go`、`store/admin.go`、`store/capability_observed.go`），每处都要有测试钉住 |

**v1 明确不做**

- 灰度 / 按百分比 / 按源 / 按节点启用（开关是全局的一个布尔）。
- 定时启停（cron 只属于源）。
- 批量启停（`?all=true` 之类的集合动作）。
- 停用即清空排队任务 / 取消在途任务。
- 试跑的 dry-run 预览、试跑的同步等待结果（要看结果就点 trace）。
- 「重新触发历史事件」（v4 明确不引 replay）。
- 按版本启用（只按 `name`；版本是一个内容快照，不是可独立启停的单元）。
- 审计保留期策略、审计导出、跨命名空间审计视图。
- 能力级限流 / 配额（与本设计正交）。

## 10. 验收清单（供后续实现拆票）

**store**

- [ ] `capability_switches` 往返：写 enabled=0 → 读回 0；无行时 `GetCapabilitySwitch` 返回 enabled（默认）且不写行。
- [ ] disable → enable 幂等：重复调用不报错，`updated_at` 刷新，审计每次一行。
- [ ] `capability_audit` 往返 + 读取按 `created_at DESC, id DESC`；`limit` 边界（1 / 200 / 越界）有断言。
- [ ] `events.origin` 往返；存量插入默认 `'source'`；非法值被 CHECK 拒绝。
- [ ] 命名空间删除后开关行与审计行级联消失。

**route**

- [ ] 目标能力 `disabled` → 不建任务，产出剪枝事实 `capability_disabled`，事件行 `type='prune'`。
- [ ] 目标能力 `enabled`（或无开关行）→ 行为与本设计之前逐字一致（回归）。
- [ ] emit 指向 `disabled` 下游 → 回执 `taskId:null` + `capability_disabled`，父任务不失败。
- [ ] 停用只作用于被停的那个名字：`next`/`targets` 里的其它目标照常建任务。

**API / 契约**

- [ ] `enable`/`disable` 200 返回 `Capability` 且 `status.enabled` 正确；404（能力不存在）/403（跨 ns）/401（无会话）。
- [ ] `status.enabled` 出现在 `CapabilityStatus`；写请求体带 `status.enabled` 仍 400 `status_not_writable`。
- [ ] `test` 200 回执含 `traceId/tasks/pruned/warnings`；参数不合 schema → 400 且 `details.causes[]` 指向字段路径。
- [ ] `test` 在 disabled 上可用（显式设计），且审计留 `action='test'`。
- [ ] `test` 指定不存在版本 → 404；指定 `nodes` 越权 → 403。
- [ ] 一个节点都没持有 digest → 200 + `warnings[reason=capability_not_deployed]`，任务 `phase=pending`。
- [ ] `GET …/audit` 返回倒序、`limit` 生效；空审计返回 `items: []`（不是 `null`）。
- [ ] `internal/server/openapi_parity_test.go` 覆盖四个新 op ↔ 路由；`reason` 封闭集合（`api-conventions.md` §4.2 + `enums.go`）同步。
- [ ] 信封与 status 只读规则仍由 `scripts/check_envelope.py` 与 `make ci` 的契约检查兜住。

**事件读面（避免污染）**

- [ ] `/stats` 与 `/v1/admin/stats` 不计 `origin='test'`（含今日与热力图 SQL）。
- [ ] ns 事件列表默认不含 test；显式 `?origin=test` 只看 test（不改变 `source`/`traceId` 既有参数语义）。
- [ ] 管理后台全站事件流对 test 行有可见标记（不静默隐藏，避免「我明明跑过」）。
- [ ] trace 全景页对 test 行有「试跑」标记。

**前端**

- [ ] 徽标随 `status.enabled` 切换；停用/启用按钮只在有写权限时出现且幂等。
- [ ] 试跑弹窗：schema 表单渲染（或 textarea 降级）→ 提交 → 回执面板 → traceId 可点。
- [ ] `make fe` 全绿（class 守卫 / i18n 四份 locale / 对比度 / 分布指纹）。
- [ ] 390 视口下弹窗为全屏 sheet，无横向溢出（沿用既有 modal 行为）。

**e2e**

- [ ] `api_contract` 套件覆盖四个新端点（成功 / 404 / 403 / 参数错误）。
- [ ] `frontend` 套件覆盖：停用 → 能力页徽标变化 → 源触发被剪枝（`status.observed.pruned` 出现 `capability_disabled`）→ 启用 → 再触发建出任务。
- [ ] 真节点套件覆盖一次试跑：`test` → 真任务 → 真结果（复用既有真节点链路）。

## 11. 决策记录

| 编号 | 决策 | 理由 |
| --- | --- | --- |
| L-D1 | 开关按 `(namespace_id, name)` 存独立表，无行 = enabled | 开关是名字的属性；放版本表要复制到每个历史行，放 `spec` 会触发版本+digest 变更 |
| L-D2 | 停用只拦**自动**触发，不拦人工 `test` | 停用的目的是停止不想要的流量，不是剥夺验证手段；强制 enable→test→disable 会被忘记 |
| L-D3 | 停用是读时闸门（计划期），不取消在途任务、不推帧给节点 | 与既有 `sources.enabled` 的判定位置一致；取消在途任务会让链路半途断掉 |
| L-D4 | 停用不返回 4xx，走剪枝原因 `capability_disabled` | 事件入口的 200/4xx 只表达「收下了没有」；下游不接与 `downstream_missing` 同类 |
| L-D5 | 试跑写成**真事件**（`origin='test'`）+ 真任务，复用接收事务 | 不新开建任务路径；trace 可看全过程；代价是五处事件读路径要显式决定过滤口径 |
| L-D6 | 试跑任务**计入** `status.observed`，但不计入 `/stats` 事件数 | 执行诊断必须真实；统计口径不该被人工动作放大 |
| L-D7 | 审计用独立 `capability_audit` 表 + 读端点 | `events` 是原始输入、无 actor 列、且被统计消费；只写不读的审计等于没写 |
| L-D8 | `status.enabled` 作为派生投影暴露 | 页面需要一次请求拿到状态；持久化的是开关事实，不是 status 整块（不违反 `V4-D1`） |

## 12. 未决

1. **停用是否该出现在源的 dry-run / 预览里**：预览当前只报 `missing` / `pruned`，若目标能力被停用，是否要提前告知？倾向「要」，但要与 `sources` 的预览语义一起改（v1.1）。
2. **试跑要不要支持「只校验参数、不建任务」**（真正的 dry-run）：倾向 v1.1，且必须与 `test` 分成两个端点，不能靠一个布尔开关混在一起。
3. **审计保留期**：需要产品决定（90 天？永久？），并与 `retention` 配置面一起设计。
4. **停用的可观测性**：是否要在 `/stats` 或命名空间状态里显示「已停用能力数」？倾向不显示（它不是健康指标），留给页面。
5. **`origin` 的第三个取值**：把内部剪枝事实（`source='route'`）与 `hub` 改线事件统一进来？这会让「排除内部行」从按 source 变成按 origin，属于跨域的读路径重构，不在本设计内。
