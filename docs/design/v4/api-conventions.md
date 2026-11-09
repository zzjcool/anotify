# v4 · API 约定（轻量资源面，派生契约）

> **状态**：已定（语义冻结；实现未开始）（2026-11-09，按 `decisions.md` V4-D1～D6 重写）。
> **上游**：`capability.md` / `source.md` / `anode.md` / `playbook.md`（语义基线）+ `decisions.md`（本轮已批准决策）+ `config.md`（三层配置）。
> **覆盖旧契约**：`api/openapi.yaml`（109 operation，62 schema）。
>
> ## 本文的边界（重要）
>
> 本文**只定义**：① 信封 ② `/v1` 路由与端点面 ③ 写语义与必要并发保护 ④ `status` 的只读派生面 ⑤ 统一错误体 ⑥ 旧→新差异。
>
> 本文**不重新定义** `spec` 字段：以 `capability.md §2`、`source.md §2`、`playbook.md §1`、`anode.md §1` 为唯一事实源。
> 本文也**不新增产品功能**：没有 `labels` / `annotations` / selectors、没有 `generation` / `CapabilityRevision` /
> `finalizers` / `deletionTimestamp`、没有 reconciler、没有 `/apis` group 路由、没有 `?propagationPolicy=`、没有 migration 12。
> 与主文档冲突时以主文档 + `decisions.md` 为准；本文必须回改，不能留两套活规则。

---

## 0. 一句话与决策索引

把现有扁平 REST 收成**一个轻量资源面**：`apiVersion/kind/metadata/spec/status` 信封、`/v1` 路由、
`resourceVersion` 必要 CAS、统一 `Status` 错误体；`status` 只读且**从事实派生**，不落库。

| 编号 | 结论 | 影响 |
|---|---|---|
| **A1** | 路由统一用 `/v1/...`，不引 `/apis/{group}/{version}`，不引 discovery | 路径简单、无第二套 group 推导 |
| **A2** | 所有资源同一 group `anotify/v1`（events/traces 不再单独分组） | 少一个 group 概念 |
| **A3** | 信封四键恒存在；`spec` 唯一可写，`status` 只读（写 → 400） | schema 收敛 |
| **A4** | 命名沿主文档：`Source.spec.kind`（不是 `spec.type`）、`metadata.version`（不是 `generation`） | 与四主文档一致 |
| **A5** | 边（`edges`）是两类派生投影：能力边来自 `spec.next`，源入口边来自 `source.spec.targets`；只保留 `GET /v1/namespaces/{namespace}/edges`；删除 `POST/DELETE /edges`、`PUT …/downstreams` | 不维护第二份路由事实；用户不能直接写 Edge |
| **A6** | `deployments` 只保留只读事实端点；写/删端点删除；部署由人工 `POST …/deploy` 写事实，节点按事实拉取 | 保留人工部署，不引控制器 |
| **A7** | 版本 = `capability_versions` 的行；不引独立 `CapabilityRevision` kind；`next` 原地更新不出版本 | 见 `decisions.md` V4-D1 |
| **A8** | `status` 读时派生：不落库、无 reconciler、无 `lastTransitionTime` / `observedGeneration`；`resolved` / `observed` 明细保留 | 见 V4-D1 |
| **A9** | 并发控制统一成 `metadata.resourceVersion` 必要条件（PUT/PATCH 走请求体，DELETE 走 `?resourceVersion=`）；不冒充 `version` | 只收已有 `if_revision` 语义 |
| **A10** | 不新增 labels/selectors；列表沿用现有显式过滤参数 | 功能克制 |
| **A11** | `namespace` 进 path；body 里显式 namespace 与 path 不同 → 400；无上下文也未给 → 400 | 不静默投递到另一个 ns |
| **A12** | 错误体统一 `Status`；`reason` 封闭集合 | 消掉两套 Error / 空 body |
| **A13** | 安装包 `POST` 创建同 slug → 409；改包用 `PUT` 整体替换；install 每个条目补齐 | 见 V4-D3 |
| **A14** | 安装回执是**响应文档**（`InstallReceipt`），不是资源、没有独立 kind 端点 | 与 `playbook.md §4.5` 一致 |
| **A15** | 任务只读；取消/放弃沿用动作端点；`status` 带 attempts/lease/诊断，**不记 env 实际值** | 见 V4-D2/D5 |
| **A16** | DELETE 只删清单/记录，不做级联卸载；没有 finalizers / `propagationPolicy` | 见 V4-D1/D3 |
| **A17** | 日志/产物默认 7 天、事件/去重记录默认 30 天；显式 0 永久；用户 `workDir` 永不删 | 见 V4-D6 |
| **A18** | 开发库直接建 v4 baseline，API 文档不再写 migration 12 | 见 V4-D6 |
| **A19** | 命名空间私有资源统一 `/v1/namespaces/{namespace}/…`；节点/命名空间管理走 cluster 路径；hooks 仍 `/v1/hooks/{token}` | 见 R2 |
| **A20** | 写 CAS 不可选：配置写必须带 `resourceVersion`，缺失 400、旧值 409；`resourceVersion` 不是配置生效版本 | 见 R2 |
| **A21** | 保留人工部署：`POST …/capabilities/{name}/deploy` 写 `deployments` 事实；install 不部署 | 见 R2 |
| **A22** | `GET …/playbooks/{slug}/installs` 是按 `valuesDigest` 折叠的实例清单；单次回执走 `…/installs/{installId}` | 见 R2/R3 |
| **A23** | `warnings[]` 是响应顶层可选字段，与信封四键并列 | 见 R3 |

---

## 1. 与 k8s 的取舍边界

### 1.1 采纳（形状与纪律）

| 采纳 | 为什么 |
|---|---|
| `apiVersion/kind/metadata/spec/status` 信封 | 一个 kind 一份形状；不再靠 `*Input` / `*View` 分叉 |
| `spec` 唯一可写面 | 变体的根因是这条纪律缺失 |
| `{Kind}List{items[]}` | 消灭「裸数组 / 命名键 / 信封回显」三种并存 |
| `metadata.resourceVersion` 必要条件 | 已有 `if_revision` 的语义，换成标准字段 + 统一 409 |
| `metadata.uid` | 收掉散落的业务 id 前缀（`cap_…` / `tsk_…`） |
| `conditions[]` + `Status` 错误体 | v4 已有 `conditions`，只是没上 wire |
| `namespace` 进 path | 现有 `namespaceId` 参数的一半不变量结构性消失 |
| `?dryRun=` / `?force=` 只保留**已有语义** | 不新造第二套 dry-run / 删除协议 |

### 1.2 不采纳（并说明为什么不必）

| 不采纳 | 理由 |
|---|---|
| **watch / `?watch=true`** | 节点侧已有双向协议；控制台不需要秒级一致性，引入 watch 会新增一整套过期重连状态机 |
| **SSA / `managedFields` / `fieldManager`** | 每个字段只有一个写者；用「读→改→写→409 重试」 |
| **`/apis` discovery（APIGroup/APIResourceList）** | kind 集合是编译期固定的；扩展点是 `Source.spec.kind` 注册表，不是 CRD |
| **CRD / 多版本共存** | `apiVersion` 只做版本标记，不做 schema 迁移；破坏性变更走 `/v2` |
| **`/status` 子资源对外** | 路径预留但本期不暴露；节点回报走 WS 帧，不是 REST |
| **finalizers / `ownerReferences` 级联 / `propagationPolicy`** | 删包只删清单、保留运行资源；所有权协议是另一层设计 |
| **labels / annotations / selectors** | 不新增产品功能；过滤用显式参数 |
| **`generation` / `CapabilityRevision`** | 版本就是 `capability_versions` 的行；`next` 原地更新不出版本 |
| **`observedGeneration` / `lastTransitionTime`** | `status` 不落库，没有“上一次收敛”的时间线 |

---

## 2. 分域：5 个 spec 文件，同一 `/v1` 前缀

| 文件 | kind | 鉴权 |
|---|---|---|
| `api/resource.yaml` | `Capability` / `Source` / `Playbook` / `Node` / `Namespace` / `Task` | Cookie / Bearer key / Node token |
| `api/events.yaml` | `Event` / `Trace`（只读） | Cookie / Bearer key |
| `api/agent.yaml` | 协议（binary / WS stream） | Node token |
| `api/ingress.yaml` | 外部投递入口（events / hooks / source dry-run / fire） | Bearer key / HMAC 签名 / Cookie |
| `api/auth.yaml` | 身份域（形状不动） | 组合 |

**为什么没有 `/apis` group**：`apiVersion` 已经能表达版本；再在路径里写一遍 group 就有两个位置能推导版本号，必然漂移。
事件与资源同 group，靠 `kind` 区分；“能不能 apply”由端点决定，不由 group 决定。

**auth 域形状不动**：认证器不是资源。Passkey / CLI 授权 / API Key / 用户管理沿用现有端点与请求体，
只做两件事：路径保持 `/v1/auth/...`、错误体换 `Status`。

---

## 3. 通用信封

### 3.1 对象

```json
{
  "apiVersion": "anotify/v1",
  "kind": "Capability",
  "metadata": { "...": "§3.2" },
  "spec":     { "...": "唯一可写面，字段见主文档" },
  "status":   { "...": "只读派生面，写请求里出现即 400" }
}
```

| # | 规则 | 违反 |
|---|---|---|
| E1 | 四个顶层键恒存在。`status` 在无事实时给 `{}` 而不是 `null`（空集合 `[]`、空对象 `{}`）。**响应顶层可有可选的 `warnings[]`**，它与四键并列，不是资源字段（A23） | 契约测试 |
| E2 | `apiVersion` 与 `kind` 必填，且必须与路径指向的 kind 一致 | 400 `kind_mismatch` |
| E3 | `metadata.name` 创建后不可变 | 400 `name_immutable` |
| E4 | 未知顶层字段 → 400 `unknown_field`（不忽略） | 400 |
| E5 | `spec` 里未知字段 → 400（各 kind 的 spec 是封闭集合） | 400 |
| E6 | 写请求里的 `status` 必须缺省或 `{}` | 400 `status_not_writable` |
| E6a | CLI 导入 / apply 在提交前**剥离 `status`**（既定 apply 忽略语义），API 对直接写 `status` 一律 400 | 两者不冲突 |
| E7 | 所有 JSON 字段 `camelCase`；空列表 `[]`，空对象 `{}` | — |

**信封的强制范围**：四键 + List `items` 只对**资源文档与资源列表**生效。下面这些**具体响应文档**是明确例外——不得伪装成另一个资源 kind，也不得为它们新建可写资源或持久化对象：

| 例外文档 | 形状事实源 |
|---|---|
| `InstallReceipt`（安装回执） | §7.3 / `playbook.md §4.5`；顶层 `created` / `skipped` 保持对象 |
| 配置子资源 wrapper（node / namespace / system） | §7.4 / §7.5；有 `metadata.resourceVersion`，没有 `spec` / `status` |
| `NodeToken`（令牌动作响应） | §7.4；动作结果，无 GET / list / DELETE |
| 受 Node 身份鉴权的内容通道（`GET /v1/node/capabilities/{digest}`） | §8.2；直接返回完整 `artifact.ArtifactJSON`（含完整 `run.files`），不带资源 wrapper / `status` |
| 动作回执与预览（source `fire` / `dry-run`、playbook `render` / `DELETE kept`） | §8.1 / §7.3 |
| 任务 `logs` / `emissions` 子资源 | §7.6 |
| 读模型（`/v1/admin/messages`、`/v1/stats` 等） | §8.3 / §8.4；其中 `/v1/console/events` 返回的 `EventList` 仍是资源列表 |

### 3.2 `ObjectMeta`

| 字段 | 类型 | 谁写 | 说明 |
|---|---|---|---|
| `name` | string | 客户端 | 名字正则**沿四主文档**（能力 `^[a-z0-9][a-z0-9._-]{0,62}$`、包 `^[a-z0-9]+(-[a-z0-9]+)*$`、源沿用其命名约束）。不为了 URL 强制 DNS-1123；URL 能含下划线，名字规则不跟着改 |
| `namespace` | string? | 客户端 | 可省略：由 path 或 CLI 明确上下文补。显式值与 path 不同 → 400 `namespace_mismatch`；两者都没有 → 400 |
| `uid` | string | 服务端 | 不透明、永不复用 |
| `version` | int? | 客户端 | **仅 `Capability` / `Playbook` 使用**，语义见主文档；不是所有 kind 都有 |
| `resourceVersion` | string | 服务端 | 不透明并发令牌。**不等于 `version`**；只用于写前置条件，不表达内容修订 |
| `creationTimestamp` | string(RFC3339) | 服务端 | 原 `createdAt` |
| `displayName` | string? | 客户端 | 纯展示；不进 digest、不产生新版本 |

**没有** `labels` / `annotations` / `generation` / `finalizers` / `deletionTimestamp`。
列表过滤使用显式查询参数（`status` / `node` / `source` / `type` / `limit` 等），不引 `labelSelector` / `fieldSelector`。

### 3.3 列表

```json
{
  "apiVersion": "anotify/v1",
  "kind": "CapabilityList",
  "metadata": {},
  "items": []
}
```

| 项 | 说明 |
|---|---|
| `items` | 空列表 `[]`，不是 `null` |
| 排序 | 每种 kind 有唯一确定的排序（默认 `creationTimestamp desc, name asc`），不提供 `?sort=` |
| 分页 | 资源列表沿用现有 `limit`（默认/上限不变），不引 `remainingItemCount`；**只有两个明细集合**（`/v1/console/events`、`…/tasks/{name}/emissions`）用 `limit` + 不透明 `continue`，游标必须绑定 ns + 筛选条件（§8.3 / §7.6） |
| 过滤 | 现有显式参数：`status` / `node` / `source` / `type` / `trace`；不新增 selector 语法 |
| 不回显 | 信封不回显 `namespaceId` / `limit` / `targetAgentVersion` 这类请求参数 |

### 3.4 写语义

| 方法 | 语义 | 细节 |
|---|---|---|
| `POST …/{plural}` | 创建 | `metadata.name` 必填；已存在 → **409 `AlreadyExists`**（包另见 A13） |
| `GET …/{plural}/{name}` | 读单个 | 不做 ETag/304；一致性靠 `resourceVersion` |
| `GET …/{plural}` | 读列表 | §3.3 |
| `PATCH …/{plural}/{name}` | merge-patch | 只用于已有端点支持的更新；`spec.next` 原地更新不产生新版本 |
| `PUT …/{plural}/{name}` | 全量替换 | **仅安装包**（A13）与**已定的配置子资源**（`/nodes/{name}/config`、`/namespaces/{name}/config`、`/system/config`，A19/A20）；其它资源不新造 PUT |
| `DELETE …/{plural}/{name}` | 删除 | 只删该对象/清单；**不级联卸载**；没有 finalizer 阻塞 |

**并发控制（A9/A20）**：CAS 令牌只有一种承载方式，PUT/PATCH 与 DELETE 的位置不同。

| 方法 | 令牌位置 | 说明 |
|---|---|---|
| `PUT` / `PATCH` 已有资源（含三个配置子资源） | 请求体 `metadata.resourceVersion` | 不是 header、不是 query |
| `DELETE` 已有资源 | **不带请求体**；唯一承载是 `?resourceVersion=<URL 编码后的字符串>` | UI 从资源当前 `metadata.resourceVersion` 构造 |

```
R 缺失            → 400 { reason: "resource_version_required" }（不静默 last-write-wins）
R != 当前值       → 409 { reason: "resource_version_conflict", details.current: {resourceVersion: "…"} }
R == 当前值       → 写，并给该行分配新的 resourceVersion
资源不存在        → 404（与 CAS 失败分开；不是 409）
```

- **不并存两条协议**：没有 `If-Match` 头，DELETE 也没有请求体；`resourceVersion` 是**字符串**，不做 Number 转换（前端不要 `parseInt`）。
- **`POST` 创建与 action POST**（`deploy` / `install` / `render` / `fire` / `tokens` / `cancel` / `abandon`）不套用这条 CAS；它们的前置条件各自按主文档。

**配置写 CAS 不可选**：`config.md` 不再有 `if_revision` 字段，统一用 `metadata.resourceVersion`。
`resourceVersion` 是**写并发令牌**；`configRevision` / `namespaceVarsRevision` 是**配置生效版本**，两者不能混用。
只做**行级 CAS**，不引全局单调水位、不把 `generation` 混同 `version`。

### 3.5 `status` 纪律

| # | 规则 |
|---|---|
| S1 | `status` 只由服务端写；客户端写 → 400 `status_not_writable` |
| S2 | `status` **从事实读时派生**（`events` / `tasks` / `deployments` / 节点上报 / `capability_versions`），不落库、不需要 reconciler |
| S3 | **没有** `observedGeneration`、**没有** `lastTransitionTime`（它们要求状态历史，与 S2 冲突） |
| S4 | `conditions[].type` / `reason` 是封闭集合；`status` ∈ `"True" \| "False"`（字符串） |
| S5 | `resolved` / `observed` 的**明细数组必须保留**（不能只给计数）；它们是诊断依据 |
| S6 | 要知道“何时变的”，查事实表（`deployments.updated_at`、事件时间、任务时间），不靠状态历史 |

---

## 4. 错误体：`Status`

### 4.1 形状

```json
{
  "apiVersion": "anotify/v1",
  "kind": "Status",
  "status": "Failure",
  "code": 400,
  "reason": "unknown_field_path",
  "message": "fieldPath \"status.foo\" is not a builtin fact",
  "details": {
    "causes": [
      { "reason": "NotInClosedSet", "message": "closed set: metadata.*, status.*, vars.*",
        "field": "spec.run.env.HOST.valueFrom.fieldRef.fieldPath" }
    ],
    "retryAfterSeconds": 0
  }
}
```

| 规则 | 说明 |
|---|---|
| `status` | `"Success"` / `"Failure"`；所有错误响应都带 body |
| `code` | = HTTP 状态码（冗余一份，客户端不必解析 HTTP） |
| `reason` | 机器可读、封闭集合（§4.2），不新增缩写 |
| `details.causes[]` | 多点校验失败时逐条给 |
| `details.retryAfterSeconds` | 429 专用 |
| HTTP 码 | `400` 校验 / `401` 未认证 / `403` 禁止 / `404` 不存在 / `405` 方法不允许 / `409` 冲突 / `413` 载荷过大 / `429` 限流 / `500` |

### 4.2 `reason` 封闭集合

**通用**：`BadRequest` `Unauthorized` `Forbidden` `NotFound` `AlreadyExists` `Conflict`
`resource_version_required` `resource_version_conflict` `kind_mismatch` `namespace_mismatch` `name_immutable`
`unknown_field` `status_not_writable` `immutable_field` `TooManyRequests`

**能力写入期**（`capability.md §6.1`，全保留）：
`impl_missing` `entrypoint_no_shebang` `files_invalid` `env_invalid` `workdir_invalid`
`undeclared_need` `unknown_field_path` `non_path_field` `undefined_env_ref` `undefined_param_ref`
`concurrency_invalid` `param_env_collision` `requires_invalid` `unknown_target`

**运行 / 派发期**（`capability.md §6.2`）：
`target_not_declared` `missing_target` `token_invalid` `task_not_running` `emit_id_conflict`
`downstream_missing` `param_missing` `param_schema` `param_type` `workdir_unavailable` `home_unavailable` `missing_requirement`
`files_escape` `artifact_export_invalid` `script_failed` `stale_config` `slot_unavailable` `cancelled_by_user` `abandoned_by_user`
`node_lost` `lease_expired` `infra_transient` `max_attempts_exceeded`

此处是跨域码索引，不意味着每个码都用于 HTTP `Status`：`stale_config` / `slot_unavailable` 是 Node 的启动前调度拒绝，公开 Task 读接口只沿等待态诊断，不把它们记成执行失败。

**源**（`source.md §6`，`kind` 命名）：
`invalid_source_kind` `invalid_source_config` `source_name_used` `source_kind_immutable`

**包**（`playbook.md §9.2`）：
`invalid_playbook` `slug_taken` `slug_invalid` `namespace_in_package` `too_many_items`
`version_content_immutable`
`entry_missing` `file_path_invalid` `file_missing` `file_unreferenced` `file_duplicate`
`values_missing` `values_invalid` `render_name_invalid` `render_type_mismatch`
`duplicate_capability` `duplicate_source` `cycle` `playbook_conflict`
`oci_unsupported` `oci_pull_failed` `oci_digest_mismatch` `oci_too_large`

**配置**（`config.md §5.2`）：`value_invalid` `value_in_use` `stale_config` `resource_version_required` `resource_version_conflict`
> `override_stale` 是展示标记，不是 HTTP 错误，不在 `Status.reason` 里。

**节点**（`anode.md §5`）：`listen_invalid` `limits_invalid` `vars_invalid` `env_passthrough_invalid`

**改名对照（只改字面，不改语义；按主文档口径）**：

| 旧 | 新 | 依据 |
|---|---|---|
| `invalid source kind` | `invalid_source_kind` | 保留 `spec.kind` 命名，只规范机器码 |
| `source name already used` | `source_name_used` | 同上 |
| `undeclared_node_var` | `undeclared_need` | 统一到 `capability.md` |
| `config_conflict` | `resource_version_conflict` | A9（节点配置仍可保留 `config_conflict` 作为具体错误子类） |

> 剪枝原因（`no_target` / `event_schema` / `param_schema` / `param_type` / `param_missing`）是
> `status.observed.pruned[].reason` 的观察原因，不是任务失败码，不进上表。`no_target` 只表示**源**的 `targets` 为空
> （事件 200 接收、不建 task）；emit 没给 `to` 是 `missing_target`（400 / CLI 2，不派发），不是剪枝。

**错误域不要混用响应体**：

| 域 | 载体 | 例子 |
|---|---|---|
| 公共 API | `Status.reason` | `unknown_field_path` / `emit_id_conflict` |
| 本地 emit 网关 | `{error, message}` + CLI 退出码 | `target_not_declared` / `emit_unreachable` |
| 任务结果 | `Task.status.result.error.reason` | `script_failed` / `files_escape` / `artifact_export_invalid` / `workdir_unavailable` / `home_unavailable` |
| Node 启动前调度拒绝 | Node 协议 task.reject.reason；资源 API 投影 Task 等待态 | 封闭为 stale_config / slot_unavailable，分别 waiting_for_config / waiting_for_slot，不是 result.error / script_failed，也不自动转成 HTTP Status |

码字面可以复用，但响应形状、HTTP 语义与判定时机不同；剪枝不是 4xx。

### 4.3 condition 词表（只读）

| type | 允许的 reason |
|---|---|
| `Implemented` | `Valid` / `Invalid` |
| `NodesAvailable` | `Valid` / `NoMatchingNode` / `CommandMissing` / `ValueMissing` / `ConfigPending` / `NodeOffline` / `NamespaceUnauthorized` |
| `DownstreamResolved` | `Valid` / `NotFound` |
| `Deployed` | `Valid` / `NotDeployed` / `Stale` |

---

## 5. 路径与作用域

### 5.1 前缀

```
/v1/...          全部资源与端点（无 /apis，无 discovery）
```

`servers: [{ url: / }]`，路径写全。`apiVersion` 只出现在文档里。

### 5.2 作用域

| 作用域 | 模板 | kind |
|---|---|---|
| cluster | `/v1/{plural}` | `namespaces` / `nodes` / `capability-templates`（公共模板不能承载 ns 私有数据） |
| namespaced | `/v1/namespaces/{namespace}/{plural}` | `capabilities` / `sources` / `playbooks` / `tasks` / `events` / `traces` / `edges` / `deployments` |

**没有无条件跨 ns 的事实返回**：`edges` / `deployments` / `events` / `traces` 都挂在 namespace 下；
管理视图若要跨 ns，必须显式给 `namespace` 过滤并逐 ns 授权，不存在“默认返回全部 ns”的端点。
`POST /v1/hooks/{token}` 是例外：token 自身定位 namespace，路径里不再写 ns。

**`Node` 是 cluster-scoped**：一台机器被多个 ns 共用是常态。谁能用哪台机器：

| 侧 | 字段 | 语义 | 缺省 |
|---|---|---|---|
| 本机 | `anode.yaml.namespaces` | 机器所有者的**申报** | `[default]` |
| 中枢 | `Node.spec.namespaces` | 管理员的**授权** | `["*"]` |

生效 ns = 交集；任一侧 `"*"` 表示全部。派发时只有 `ns ∈ 生效 ns` 的节点是候选。
**节点 token 不绑 ns**（`POST /v1/nodes/{name}/tokens` 只签“这台节点”，且该 `Node` 必须已先由 `POST /v1/nodes` 登记）。

### 5.3 子资源

| 子资源 | 路径 | 方法 | 说明 |
|---|---|---|---|
| `versions` | `/namespaces/{namespace}/capabilities/{name}/versions` | GET | `CapabilityList`：`items[]` 每项是**完整 `Capability` 信封**（同名、不同 `metadata.version`），按 `metadata.version` 整数**降序**（新→旧）；不引 `CapabilityVersion(List)` / `CapabilityRevision`，也不自造 `item.current` |
| — | `/namespaces/{namespace}/capabilities/{name}/versions/{version}` | GET | 单版本，同一个 `Capability` 信封；`?includeBody=true` 才带 `spec.run.files` 全文（`map[相对路径]string`，不是 `files[].content` 数组） |
| `deploy` | `/namespaces/{namespace}/capabilities/{name}/deploy` | POST | **人工部署**：body `{version?, nodes?}`，写 `deployments` 事实（§7.1） |
| `logs` | `/namespaces/{namespace}/tasks/{name}/logs` | GET | `{taskId, attempt, lines[], truncated}`（§7.6）；查询参数 `tailLines` / `sinceSeconds` / `previous` |
| `emissions` | `/namespaces/{namespace}/tasks/{name}/emissions` | GET | `{metadata.continue, items[]}`（§7.6）；`limit` + 不透明 `continue` |
| `traces` | `/namespaces/{namespace}/traces/{traceId}` | GET | 只读 trace 视图 |
| `body` | `/namespaces/{namespace}/playbooks/{slug}?includeBody=true` | GET | 返回 `spec.files` **原始整份 map（未渲染）**，供编辑/导出；默认详情是摘要/体检，见 §7.3 |
| `installs` | `/namespaces/{namespace}/playbooks/{slug}/installs` | GET | **按 `valuesDigest` 折叠**的实例清单（§7.3） |
| — | `/namespaces/{namespace}/playbooks/{slug}/installs/{installId}` | GET | 单次 install 的完整回执 |
| `render` | `/namespaces/{namespace}/playbooks/{slug}/render` | POST | 纯渲染：只读写包体，不读安装冲突库、不写库 |
| `install` | `/namespaces/{namespace}/playbooks/{slug}/install` | POST | 真安装；body `{values, dryRun}`；**不部署** |
| `config` | `/nodes/{name}/config` | GET / PUT | 节点默认值 wrapper（§7.4）；GET 取 CAS 令牌，PUT 只接 `metadata.resourceVersion` + `namespaces` / `vars` / `limits` |
| `config` | `/namespaces/{name}/config` | GET / PUT | 命名空间默认 `vars` wrapper（§7.5）；PUT 只接 `metadata.resourceVersion` + `vars` |
| `system config` | `/system/config` | GET / PUT | `{metadata, retention}`（§7.5）；PUT 只接 `metadata.resourceVersion` + `retention` |
| `tokens` | `/nodes/{name}/tokens` | POST | 签发节点身份凭证（`NodeToken` 动作响应，§7.4）。**先 `POST /v1/nodes` 登记**；Node 不存在 → 404；无 GET / list / DELETE；再次 POST = 轮换 |
| `stats` | `/namespaces/{name}/stats` | GET | 命名空间聚合（虚拟子资源） |

`status` 子资源**预留但本期不暴露**；`downstreams` 写入口不存在（边由 `spec.next` 派生）。

---

## 6. 资源总表

| kind | 作用域 | `spec` 事实源 | 可写字段（用户） | status |
|---|---|---|---|---|
| `Capability` | ns | `capability.md §2` | `spec` + `metadata.displayName` | 四条 condition + `nodes` / `resolved` / `observed`（派生） |
| `Source` | ns | `source.md §2`（`spec.kind`） | `spec` + `metadata.displayName` | hook URL / 触发时间 / 目标解析（派生） |
| `Playbook` | ns | `playbook.md §1`（目录包）+ §10（`spec.files` / `spec.source` 包体传输） | `spec` + `metadata.displayName` | 聚合成员 condition + `items` / `unapplied` / `sources` / `lastInstallId` / `source`（OCI 来源投影） |
| `Node` | cluster | `anode.md`（本机文件是 truth） | `spec.namespaces` / `spec.vars` / `spec.limits` / `spec.tags` | 机器事实 + 机器级 vars + 配置 revision（派生） |
| `Namespace` | cluster | 本文 §7.5 | `metadata.displayName`（配置走 `/config` 子资源） | 计数 / stats（派生） |
| `Task` | ns | 本文 §7.6（系统产生） | **无**（取消/放弃走动作端点） | phase / attempts / lease / 诊断（派生） |
| `Event` | ns | 本文 §7.7（ingress / 派发产生） | **无** | 派发结果 / 剪枝（派生） |
| `Trace` | ns | 只读虚拟 kind | **不能 apply** | 事件 / 任务 / 剪枝汇总 |

`InstallReceipt` **不在资源表**：它是 install 的响应文档（`playbook.md §4.5`），不落成独立 kind 端点。

---

## 7. 逐 kind 契约

### 7.1 `Capability`

```yaml
apiVersion: anotify/v1
kind: Capability
metadata:
  name: fixer
  namespace: default              # 可省略：由 path / CLI 上下文补
  displayName: 修 bug
  version: 3                      # = capability.md 的 metadata.version；不是 generation
  resourceVersion: "1841"
  uid: cap_1730000000_a1b2c3d4
  creationTimestamp: "..."
spec:
  # 全部字段见 capability.md §2（params / next / run / requires / lifecycle / concurrency）
status:                           # 只读；读时派生
  version: 3
  digest: "sha256:9f2c…"
  ready: false
  conditions:
    - { type: Implemented,        status: "True",  reason: Valid }
    - { type: NodesAvailable,     status: "False", reason: NoMatchingNode, message: "没有节点同时声明 git 与 vars.SOURCE_ROOT" }
    - { type: DownstreamResolved, status: "True",  reason: Valid }
    - { type: Deployed,           status: "False", reason: NotDeployed, message: "1 台节点持有旧 digest" }
  nodes:
    - { name: anode-1, state: deployed }   # deployed | stale | missing
  resolved:
    next:
      - { name: fixer, version: 2, digest: "sha256:9c1…", ready: true,
          params: { required: [file] } }
      - { name: notify, missing: true, reason: NotFound }
  observed:                       # 窗口 = 最近一个逻辑 task；清理后为空，不杜撰“最近运行时间”
    emits:  [ { to: fixer, fields: [file], ok: true } ]
    pruned: [ { to: notify, reason: param_missing, detail: "需要 text" } ]
    tasks:  { total: 12, failed: 1 }
  installedFrom: ins_7f3a        # 只记初次来源；复用不覆盖
```

| 操作 | 行为 |
|---|---|
| `POST /v1/namespaces/{namespace}/capabilities` | 创建；同名 → 409；body = 信封 |
| `PATCH /v1/namespaces/{namespace}/capabilities/{name}` | 更新 `spec` / `metadata.displayName`；内容部分变化按 `capability.md §3` 走既有版本规则，`spec.next` 原地更新**不产生新版本、不重推节点**；**必须带 `metadata.resourceVersion`**（缺失 400、旧值 409） |
| `POST /v1/namespaces/{namespace}/capabilities/{name}/deploy` | **人工部署**：body `{version?, nodes?}`；把目标版本的 digest 写成 `deployments` 事实（可选目标节点；不传则按现有部署策略选候选）；**install 不会自动调用它** |
| `DELETE …/capabilities/{name}?resourceVersion=<R>` | **允许直接删**：不带请求体，CAS 令牌只在 query（缺失 400 / 旧值 409 / 不存在 404）；只删逻辑资源，不级联改引用方；`next` / 源 `targets` 解析不到由 `DownstreamResolved` / 源体检报告缺失。已被任务钉住的版本/事实按引用完整性保留到安全清理 |
| `GET …/capabilities/{name}/versions[/{v}]` | `CapabilityList`（`items[]` 是完整 `Capability` 信封，`metadata.version` 降序）；单版本 GET 同一个信封；`?includeBody=true` 才带 `spec.run.files` 全 map（`map[路径]string`） |
| `GET /v1/namespaces/{namespace}/edges` | 只读派生投影：能力边来自 `spec.next`，源入口边来自 `source.spec.targets`（旧 `POST/DELETE /edges` 删除） |

`status` 不落库；没有 `lastTransitionTime`；`resolved` / `observed` 明细数组保留（S5）。
`allow` 的写法（`name` / `name@version`）沿 `capability.md §2.1`；**API 不改版本语法**。
裸名引用选**最大存在 version**；UI 的 current 标识由 `max(metadata.version)` 算，**不**用自造字段 `item.current`。

### 7.2 `Source`

```yaml
apiVersion: anotify/v1
kind: Source
metadata: { name: deploy-hook, namespace: default, displayName: 部署成功 }
# 写请求（POST / PATCH）可以带 token / secret
spec:
  kind: webhook                # 命名沿主文档：不是 spec.type
  enabled: true
  config: { token: src_xxx, secret: "...", payloadSchema: {...}, sample: {...} }
  targets: [ { capability: notify, params: {...} } ]

# GET 响应：同一份 spec，但敏感字段换成只读投影
spec:
  kind: webhook
  enabled: true
  config: { hookUrl: "https://hub.example/v1/hooks/src_xxx", secretSet: true,
            payloadSchema: {...}, sample: {...} }
  targets: [ { capability: notify, params: {...} } ]
status:
  hookUrl: "https://hub.example/v1/hooks/src_xxx"
  lastFiredAt: "..."
  nextFireAt: "..."            # 仅 cron
  enabled: true
  health: ok                   # ok | blocked | disabled | unapplied（与 playbook §6.2 同词）
  targetsResolved: true
  lastError: null
  installedFrom: ins_7f3a      # 只记初次来源；复用不覆盖
```

| 规则 | |
|---|---|
| `spec.kind` 不可变 | 改 → 400 `source_kind_immutable`（原“换 kind 要删重建”保留） |
| `spec.kind` 开放集合 | 合法值 = `store.SourceKinds` 注册表里的项 |
| 敏感字段读回 | `GET` **不回显** `config.token` / `config.secret`：只给 `config.hookUrl`（授权读）与 `config.secretSet`；这两个字段（含 GET 返回的对应 `status` 字段）均为只读投影。写请求出现 `spec.config.hookUrl` / `spec.config.secretSet` → 400 `unknown_field`，编辑前必须移除；写请求与 GET 响应示例分开，不拿写入示例当读响应 |
| 凭据更新 | `PATCH` 省略 `config.token` / `config.secret` 保持已有值；轮换必须显式写已定义的 `config.token` / `config.secret`，不能把 `secretSet: false` 当作清空 `secret` |
| `config.token` 轮换 | 走 `PATCH /v1/namespaces/{namespace}/sources/{name}`；旧 URL 立即 404 |
| `targets` 变更 | 同一事务重建该源入口边（`edges.from_is_source = 1`）；没有直接写 Edge 的 API |
| webhook 幂等 | 投递体 `id` 在**保留窗口（默认 30 天）**内去重；窗口外同 `id` 可再次接收；改 payload 用新 `id`；v4 不加 replay API |
| 事件入口 | `POST /v1/namespaces/{namespace}/events`（Bearer key）与 `POST /v1/hooks/{token}`（HMAC）保留；不合并成 `/ingress/events` |
| 手动触发 / 试运行 | `POST /v1/namespaces/{namespace}/sources/{name}/fire`、`POST /v1/namespaces/{namespace}/sources/dry-run`、`POST /v1/namespaces/{namespace}/sources/{name}/dry-run` 保留；`fire` 返回接收回执、`dry-run` 返回预览文档（形状统一在 §8.1），两者都按 `namespace` 做授权 |

### 7.3 `Playbook`

安装冲突使用 HTTP 400 `Status`（`reason: playbook_conflict`）：`details.valuesDigest` 标识本次值，`details.conflicts[]` 保留完整冲突矩阵条目，`details.hint` 给出修复提示。不能以通用 `details.causes[]` 替换并丢失上述结构化字段。

```yaml
apiVersion: anotify/v1
kind: Playbook
metadata: { name: release-flow, namespace: default, displayName: 发布流程, version: 3 }
spec:                    # 传输面：只搬包体，不复制根清单（playbook.md §1）；这是写请求 / includeBody=true 的读形状，默认 GET 是空投影
  files:                 # 二选一：map[相对路径]string，含 playbook.yaml 与各能力/源文件
    playbook.yaml: |
      apiVersion: anotify/v1
      kind: Playbook
      metadata: { name: release-flow }
      spec:
        values: { ... }
        capabilities: [ capabilities/review.yaml ]   # 根清单只在包内这份文件里
        sources: [ sources/on-push.yaml ]
    capabilities/review.yaml: |
      metadata: { name: '{{values.project}}-review' }
      spec: { ... }
# 或（OCI 入口，同样二选一）
# spec:
#   source: "oci://ghcr.io/acme/playbooks/release-flow:1.4.2"
status:                  # 只读；仅读响应包含，写请求（POST / PUT）不带 status
  ready: false
  unapplied:             # 非空 → ready=false（路由 / 事件去向没有全部生效）
    - { kind: capability, ref: a-review@3, field: next,
        declared: [a-fixer], actual: [other], reason: routing_unapplied }
  total: 2
  conditions:            # 四条聚合自成员能力（capability.md §2.4）
    - { type: Implemented, status: "True" }
    - { type: NodesAvailable, status: "False", reason: NoMatchingNode, members: [a-fixer] }
    - { type: DownstreamResolved, status: "True" }
    - { type: Deployed, status: "True" }
  blocker: { type: NodesAvailable, capability: a-fixer, action: open_node_page }
  items:
    - { kind: capability, name: a-fixer, version: 1, state: no_node }
    - { kind: source, name: a-on-push, state: ok, targets: [a-review] }
  sources: { total: 1, enabled: 1, blocked: 0, disabled: 0, unapplied: 0 }
  lastInstallId: ins_7f3a
  source: "oci://ghcr.io/acme/playbooks/release-flow@sha256:4c11…"   # OCI 来源的只读投影；手传包为 null
```

| 操作 | 行为 |
|---|---|
| `POST /v1/namespaces/{namespace}/playbooks` | 创建包：body 是 `Playbook` **信封**（`metadata.name` = slug）；`spec.files` 与 `spec.source` **二选一**，body 不带 `status`；`spec` 里**不复制** `values` / `capabilities` / `sources`（根清单只在包内 `playbook.yaml`）。同 slug 已存在 → **409 `slug_taken`** |
| `PUT /v1/namespaces/{namespace}/playbooks/{slug}` | **整体替换包体**：同一信封 + `metadata.resourceVersion`（缺失 400 / 旧值 409）；不影响已装的行 |
| `DELETE …/playbooks/{slug}?resourceVersion=<R>` | 不带请求体，CAS 令牌只在 query；只删清单，响应带 `kept`（**只有 capabilities / sources**）与独立 `values`；不级联卸载 |
| `POST …/playbooks/{slug}/render` | 纯渲染：`{values}` → `{values, valuesDigest, documents, files}`（渲染后的 `files` 形状沿 `playbook.md §2.4`）；**只读写包体，不读安装冲突库**、不写库 |
| `POST …/playbooks/{slug}/install` | 真安装：body `{values, dryRun}` → **`InstallReceipt`**（§4.5）；每次产生新回执（同 values 也重跑比较 + 补齐，不 409）；**不部署** |
| `GET …/playbooks/{slug}` | 默认详情：摘要 + `lastInstallId` + 体检；**不含包体全文**——`spec` 可因投影为空 `{}`，计数/体检/`lastInstallId` 在 `status`，OCI provenance 在 `status.source` |
| `GET …/playbooks/{slug}?includeBody=true` | `spec.files` 返回**原始整份 map（未渲染）**，供编辑/导出；默认摘要**不能**当完整 `PUT` 模板；`render` 返回渲染结果，不替代原始模板读取 |
| `GET …/playbooks/{slug}/installs` | **按 `valuesDigest` 折叠**的实例清单，不是原始回执列表；带 `lastInstallId` |
| `GET …/playbooks/{slug}/installs/{installId}` | 单次 install 的完整回执（`playbook.md §4.5`） |

`InstallReceipt` 形状以 `playbook.md §4.5` 为准（顶层字段是回执文档，不是资源信封；`created` / `skipped` 保持对象，不搬进 `status`，也不为它新建 kind 端点）。
`unapplied[]` / `notes[].code` 是回执与体检的稳定字段。`dryRun: true` 与 `render` 是两个动作。
`unapplied[]` 非空时 `status.ready` 必须为 `false`，`notes` 至少包含 `routing_unapplied` 或 `targets_unapplied`。

**`playbook_conflict` 走统一 `Status`，HTTP 一律 400**（不是 409）：

```json
{
  "apiVersion": "anotify/v1", "kind": "Status", "status": "Failure",
  "code": 400, "reason": "playbook_conflict",
  "message": "预案冲突：2 条需要覆盖已有定义，安装被拒绝",
  "details": {
    "valuesDigest": "sha256:9c1f…",
    "conflicts": [
      { "kind": "capability", "ref": "a-review@3", "reason": "version_conflict",
        "declaredDigest": "sha256:ab12…", "actualDigest": "sha256:cd34…" },
      { "kind": "source", "ref": "a-on-push", "reason": "config_differs", "fields": ["config.token"] }
    ],
    "hint": "给该能力改一个 metadata.version 再装；或删掉库里那一行重建"
  }
}
```

`code` 是**数字**（= HTTP 码），`reason` 是**字符串**；`playbook.md §4.2` 的 12 格判定与每条 `kind` / `ref` / `reason` / `declaredDigest` / `actualDigest` / `fields` 全保留在 `details`，不得裁成自由文本。可以另给通用 `details.causes[]`，但它**不能**替代结构化冲突矩阵。

### 7.4 `Node`（首次定义）

```yaml
apiVersion: anotify/v1
kind: Node
metadata:
  name: anode-1                # = anode.yaml 的 name（cluster-scoped：无 namespace）
  resourceVersion: "1841"
  uid: "node_..."
  creationTimestamp: "..."
spec:                          # 中枢说的期望 / 授权
  namespaces: [default, team-a] # 授权：与本机申报取交集；缺省 ["*"]
  vars:                        # 节点默认值（机器级，跨 ns 共用）
    TOOLCHAIN: "/opt/go1.25"
  limits:                      # 原 anode.yaml limits 的节点默认值
    total: 4
    perCapability: { "*": 2, "fixer": 1 }
  tags: [build, prod]          # 沿用现有 tags（不是 labels）
status:
  hostIP: "10.0.0.7"
  hostname: "anode-1"
  os: linux
  arch: amd64
  capacity: { cpu: "8" }
  workRoot: /var/lib/anode/work
  homeDir: /home/anode
  # ── 机器级配置：local + node 合并；不是“全部 ns 的 effectiveVars” ──
  localVars: { SOURCE_ROOT: "/srv/src" }
  nodeVars:  { TOOLCHAIN: "/opt/go1.25" }
  machineVars:                       # local 压住 node 后的机器级结果
    SOURCE_ROOT: "/srv/src"
    TOOLCHAIN: "/opt/go1.25"
  configRevision: "7"                # 节点默认值 revision
  appliedRevision: "6"               # 节点回报（机器级 applied）
  appliedHash: "sha256:…"
  effectiveNamespaces: [default, team-a]
  agentVersion: "1.4.9"
  instances: [ { id: 1, task: "cron-9f2-013", busy: true } ] # 沿用只读诊断投影；id 不是 wire slot
  lastSeenAt: "..."
```

| 规则 | |
|---|---|
| 登记与令牌 | 顺序固定：先 `POST /v1/nodes`（离线 `Node`，`metadata.name` 必填）→ 再 `POST /v1/nodes/{name}/tokens`。Node 不存在 → **404**；签 token **不隐式创建**节点、也不隐式选择 ns；令牌**不绑 ns** |
| 令牌响应 | `POST /v1/nodes/{name}/tokens`（body 可为空 `{}`）→ `NodeToken` 动作文档（下方示例）；**明文只返回这一次**（不是一次性 bootstrap：它长期做节点身份）；v4 不定义 `expiresAt` / 自动续签；再次 POST = 显式轮换，旧 token 立即失效。Node 读接口 / config **不回显** token |
| `spec.namespaces` | 与本机申报取交集；权限只能收紧 |
| `spec.vars` / `spec.limits` | 机器级默认值；本机同键优先；`limits` 只有本机 + 节点两层 |
| `anode.yaml` 私有字段 | `server` / `token` / `listen` / `envPassthrough` **不进 Node spec、不上报快照**；任务用到的名字可在任务 `envDiagnostics` 标 `passthrough`，不回报完整名单/值 |
| `status.machineVars` | 机器级合并结果；**没有一份扁平 `effectiveVars`**。要看某个 ns 的有效值：`GET /v1/nodes/{name}?namespace=team-a`，响应额外给 `status.namespaceVars` + `status.effectiveVars` 并标 `context.namespace` |
| `status.instances` | 沿公开只读诊断字段，不作为用户分配 / 清槽接口。slot presence、reservation、历史 lease 与 reaped 事实属于 Node 协议（capability.md §8.5 / anode.md §6），不新增资源 API 字段 / 动作；缺失快照不是 idle，phase / 重连不证明回收，旧回收只清匹配旧占用 |
| drain / upgrade | 沿用现有动作端点（`POST /nodes/{name}/drain`、`/upgrade`）；不改成 spec 字段 |
| 节点配置 | `GET/PUT /v1/nodes/{name}/config`（节点默认值 wrapper，见下方）；批量 `POST /v1/nodes/config/batch`（`config.md §4.4`） |

**令牌动作响应**（不是资源，无 GET / list / DELETE 端点）：

```json
{ "apiVersion": "anotify/v1", "kind": "NodeToken",
  "node": { "name": "node-a" }, "token": "<new-node-token>" }
```

**节点配置 wrapper**（`config.md §4`，**不是独立 kind**）：

```json
{ "metadata": { "resourceVersion": "R" },
  "namespaces": ["*"], "vars": {}, "limits": { "total": 1, "perCapability": {} },
  "configRevision": "C", "appliedRevision": "A", "appliedHash": "sha256:…" }
```

`PUT` 只接 `metadata.resourceVersion` / `namespaces` / `vars` / `limits`，返回同一 wrapper；`configRevision` / `appliedRevision` / `appliedHash` 只读、UI **不提交**，也不拿它们当 CAS 令牌（只从 `GET …/config` 取）。Node 配置与 `Node.spec` 的底层默认值是**同一份事实**：同一底层版本 / 同一事务，不能各用一个版本绕过 CAS。

批量 `POST /v1/nodes/config/batch`：body `{updates:[{name, metadata:{resourceVersion}, vars?, limits?, namespaces?}]}` → `{items:[{name, ok:true, config:<同 GET wrapper>} | {name, ok:false, error:<Status>}]}`；**逐节点原子、可部分成功**（不是 Playbook 的整包原子规则），错误里不带原始凭证值。

### 7.5 `Namespace`

```yaml
apiVersion: anotify/v1
kind: Namespace
metadata:
  name: default                # = 原 slug
  displayName: 默认命名空间     # = 原名
  uid: ns_...
  creationTimestamp: "..."
spec: {}                       # 没有 seedBuiltinCapabilities 这类可写开关（未批准功能）
status:
  counts: { capabilities: 12, nodes: 3, sources: 4, tasks: 91 }
  stats: { ... }               # 也可走 GET /v1/namespaces/{name}/stats
```

命名空间默认 `vars` 走配置子资源：`GET /v1/namespaces/{name}/config` → `{metadata:{resourceVersion}, vars:{}, namespaceVarsRevision:"N"}`；`PUT` 只接 `metadata.resourceVersion` + `vars`（`namespaceVarsRevision` 只读、UI 不提交），成功后 bump 它并返回**同 GET wrapper**。写必须 CAS；`namespaceVarsRevision` 是配置生效版本，不是 `resourceVersion`。
系统配置同样是 wrapper：`GET /v1/system/config` → `{metadata:{resourceVersion}, retention:{…}}`；`PUT` 只接 `metadata.resourceVersion` + `retention`。

没有 finalizers / 删除阻塞协议；`DELETE` 行为沿用现有实现（不做新的级联语义）。

### 7.6 `Task`（首次定义）

```yaml
apiVersion: anotify/v1
kind: Task
metadata:
  name: cron-9f2-001           # 系统生成
  namespace: default
  resourceVersion: "1852"
  uid: "tsk_..."
  creationTimestamp: "..."
spec:                          # 系统写；用户不可创建
  traceUID: "9f2a1b3c..."
  sequence: 1
  sourceRef: { name: cron-hourly }
  parentRef: null              # 业务 emit 父 task（不是 attempt 父子）
  parentTaskId: null           # 同上，链式追踪用；不表示重试
  capability:
    name: fixer
    version: 3                 # 钉死；不是 revision 名
    digest: "sha256:9f2c…"     # 双保险
  params: { file: a.go }
status:
  phase: running               # pending | running | succeeded | failed | cancelled | abandoned
  waitingReason: null          # waiting_for_slot | retry_backoff | waiting_for_config | null
  nextAttemptAt: null          # waitingReason 非空时的下一次尝试时间（退避）
  nodeName: anode-1            # 当前 attempt 的执行节点
  instanceId: 2                # 内部实例键的只读诊断，不是 Node wire slot
  attempt: 1                   # 配置校验在 accept 前；lease.grant 生效、进入启动时递增
  maxAttempts: 3               # 总尝试次数（含第一次）
  leaseExpiresAt: "..."
  startedAt: "..."
  finishedAt: null
  configRevision: "7"          # 当前 attempt 节点上的机器级版本
  namespaceVarsRevision: "12"  # 本次 ns 默认值版本（任务输入，不随节点变）
  envDiagnostics:              # 只记名字 + 来源，不记实际值（C11）
    - { name: SOURCE_ROOT, source: local }
    - { name: NOTIFY_URL,  source: namespace }
  result:
    summary: "..."
    artifacts: [ { path: "artifacts/report.md", sha256: "..." } ]  # 系统托管副本路径，不是用户 workDir 原文件
    error: null                # { reason: <封闭码>, message: "..." }
  attempts:                    # 可选诊断事实；逻辑 task 仍只有一行
    - { attempt: 1, nodeName: anode-1, configRevision: "7", phase: failed,
        startedAt: "...", finishedAt: "...", reason: script_failed }
```

| 规则 | |
|---|---|
| 不可 `POST` / `DELETE` | Task 只由派发器 / ingress 产生；要停用 `POST …/tasks/{name}/cancel` / `/abandon`（沿用现有动作） |
| `spec` 不可变 | 改 `capability` / `params` → 400 `immutable_field`（否则“重试的是另一次执行”） |
| 重试 | **同一逻辑 task 一行**，钉住 version / digest / params 与本次 ns 输入；配置校验在 `task.accept` 前，`attempt` 只在中枢 `lease.grant` 生效、进入启动时递增；排队不计。换节点时 `local` / `node` 值按新节点重新解析，`configRevision` 跟当前 attempt；`parentRef` 是业务 emit 父子，不是 attempt 父子 |
| stale_config / slot_unavailable | Node 在 accept / grant 前分别因配置过期 / 无真实空槽拒绝；不计 attempt、不授 lease、不消耗非幂等预算。公开 phase=pending，waitingReason 分别 waiting_for_config / waiting_for_slot，带有界退避或换候选；不记为 script_failed，也不扩更多 reject 码 |
| 预留与 grant | dispatch 不授执行权；accept 先预留，grant 事务生效才计 attempt。未 grant 且确知无进程的预留可到期释放；晚到 / 不匹配 grant 不执行，但已计 attempt 不倒退。slot / assignmentExpiresAt / 回收绑定是 Node 协议，不新增 Task 可写字段 |
| 取消/放弃 | 沿用 POST …/tasks/{name}/cancel / abandon；原因区分 cancelled_by_user / abandoned_by_user。phase 终态不证明物理回收；Node 按历史 attempt / lease / slot TERM/KILL 并真实 ACK 后，中枢验证连接身份与历史绑定，仅清匹配旧占用，不清新 attempt / 不恢复旧 result 或 emit 授权 |
| 诊断 | `status.envDiagnostics` 只有名字 + 来源（`local` / `node` / `namespace` / `passthrough` / `builtin`），**没有 `resolvedEnv` 实际值**；执行输入快照是内部载体，不公开成 env 值 |
| 保留 | 终态后 **30 天**（`retention.taskTtlSec`）；活动任务不清理；被任务钉住的版本/事实按引用完整性保留 |
| 保留 | 日志 / 产物 / 系统创建目录默认 7 天（`resultTtlSec` 默认 `604800`）；显式 `0` 永久；用户 `workDir` 永不删 |

**子资源响应（响应文档，不是资源信封）**：

`GET …/tasks/{name}/logs?tailLines=&sinceSeconds=&previous=`

```json
{ "taskId": "cron-9f2-001", "attempt": 1,
  "lines": [ { "sequence": 1, "stream": "stdout", "text": "…", "at": "…" } ],
  "truncated": false }
```

`tailLines` / `sinceSeconds` / `previous` 是既有查询规则；`previous` 指定**上一次 attempt**（没有则 `lines: []`），这是逻辑 task 内的 attempt 顺序，不混淆业务 emit 的父子关系。`lines[].stream` 只有 `stdout` / `stderr`；日志来自本机档案与 WS 读取；系统**不**把宿主环境快照自动写进日志（脚本自己打印仍是作者责任）。

`GET …/tasks/{name}/emissions?limit=&continue=`

```json
{ "metadata": { "continue": "" },
  "items": [ { "eventId": "…", "to": "fixer", "version": 2, "taskId": "…",
               "id": "emit-1", "pruned": null } ] }
```

每条是**一次 emit 调用的事实**（不是端口 / 自动业务序号）：字段 `eventId` / `to` / `version` / `taskId`（`null` 或 string）/ 可选 `id` / `pruned`（`null` 或 `{reason, detail}`）；`eventId` 区分独立调用，显式 `id` 可重复请求但只留一条事实；默认最新在前；分页 `limit` + 不透明 `continue`。页面要载荷正文必须显式请求已有的 body 扩展，不能靠这个摘要泄露原始宿主环境。

### 7.7 `Event` / `Trace`（只读）

```yaml
apiVersion: anotify/v1
kind: Event
metadata:
  name: deploy-hook-9f2a-001
  namespace: default
  creationTimestamp: "..."
spec:                          # 不可变
  reason: Ingested             # Ingested | Emitted | Pruned | RoutingChanged | ConfigApplied
  type: Normal                 # Normal | Warning
  involvedObject: { kind: Source, name: deploy-hook, uid: "..." }
  traceUID: "9f2a1b3c..."
  eventType: deploy.succeeded  # 业务类型，不是信封 kind
  deliveryId: evt-9f2          # 外部投递 id；保留窗口内去重，窗口外可再次接收（内部 eventId 另存，不复用）
  payload: { environment: prod }
  signature: { algorithm: hmac-sha256, verified: true }
status:
  dispatched: [ { task: cron-9f2-001, capability: notify } ]
  pruned: null                 # { reason: param_missing, detail: "..." }
  deliveredAt: "..."
```

| 规则 | |
|---|---|
| 不可写 | 客户端 `POST /v1/namespaces/{namespace}/events` 是**投递入口**，不是资源创建；资源端点对 Event 无写方法 |
| 保留 | 事件 / trace 元数据默认 **30 天**（`retention.eventTtlSec=2592000`，可配置）；去重记录同窗口；过期物理删除。任务行终态后默认 **30 天**（`retention.taskTtlSec`），活动任务不清理 |
| 同 `deliveryId` | 窗口内只表示“已接收”，不保证执行成功；过期后可再次接收；改 payload 用新 `deliveryId`；无 replay API。外部 `deliveryId` 与内部 `eventId` 是两个字段，事件行不要求 deliveryId 全局唯一 |
| `Trace` | `GET /v1/namespaces/{namespace}/traces/{traceUID}` → `{events[], tasks[], pruned[]}`；只读、无 spec、不能 list |

---

## 8. 其它端点（保留现有形状）

### 8.1 投递入口

```
POST /v1/namespaces/{namespace}/events          外部系统投递（Bearer key）；body {source, type?, id?, payload?}，source 必填
POST /v1/hooks/{token}                   外部系统投递（HMAC 可选）；body {type?, id?, payload?}；token 定位 ns；source.md §3.3
POST /v1/namespaces/{namespace}/sources/dry-run 内联源试运行；body {source: <Source 信封>, payload?}
POST /v1/namespaces/{namespace}/sources/{name}/dry-run   body {payload?}
POST /v1/namespaces/{namespace}/sources/{name}/fire      body {payload?}
```

**接收回执**（`/events`、`/hooks/{token}`、`source fire` 共用一份形状）：

```json
{ "eventId": "…", "traceId": "…", "duplicate": false,
  "tasks":  [ { "taskId": "cron-9f2-001", "capability": "notify", "version": 3 } ],
  "pruned": [ { "capability": "fixer", "reason": "param_missing", "detail": "需要 text" } ] }
```

- `duplicate: true` 时返回**原** event / trace，不重复派发；入库剪枝也返回合法 `eventId` / `traceId`（`tasks` 为空）。
- `fire` 省略 `payload` 时用该源已有 `config.payload`（没有则 `{}`）；源不存在 / 停用沿 `source.md`，必须做 ns 授权；**不新增** manual 专属幂等键或额外路由语法。

**试运行响应是预览文档**，没有 `eventId` / `taskId`，不入事件 / 任务 / 去重，不需要保存 token、不返回 secret：

```json
{ "targets": [ { "capability": "notify", "version": 3, "params": {}, "state": "ready" } ],
  "pruned":  [ { "capability": "fixer", "reason": "param_missing", "detail": "需要 text" } ] }
```

`state` ∈ `ready` / `pruned` / `missing`；`version` 可省（解析不到具体版本时）；`pruned` / `missing` 带 `reason` / `detail`，原因取 `source.md §6` 的封闭表（不再新造词）。

除上面补齐的传输细节外，这些端点保留现有请求/响应语义；错误体换 `Status`（§4）。不合并成 `/ingress/events`。

### 8.2 节点协议

```
GET /v1/node/binary             二进制下发（application/octet-stream）
GET /v1/node/stream             WS（配置帧见 config.md §4.2，执行帧见 capability.md §8.5）
GET /v1/node/capabilities/{digest}  内容下发（完整 artifact.ArtifactJSON，含完整 run.files）
```

内容通道使用 Node token 鉴权：Capability 必须属于该 Node 允许的 namespace，且已明确 deploy 到该 Node。响应直接是完整 `artifact.ArtifactJSON`，不套资源 wrapper、不带 `status`，不改变 artifact schema / body；Node 校验 digest / refs 后缓存。流通道沿用标准 `/v1/node/stream`，不另造 `/node/ws` 或 `/agent/ws`；frames 清单由 T3 HANDOFF 定义，这里不扩展。

`anode emit` 的本地端点（`POST $ANOTIFY_EMIT_ENDPOINT/emit`）只在节点本机监听，不在 openapi 里；
节点做 token/任务/目标预检后**转发中枢**，下游 task 创建、emit 幂等、跨节点派发由中枢权威完成（`capability.md §4.0/§8.5`）。

### 8.3 auth / admin / keys / passkey / cli-auth

保持现有路径与请求体（`/v1/auth/...`、`/v1/keys...`、`/v1/admin/...`、`/v1/cli-auth/...`、`/v1/passkey-enroll/...`），
只把错误体换成 `Status`。`GET /v1/admin/messages`、`GET /v1/console/events` 保留读模型，但**必须显式给 `namespace` 过滤并逐 ns 授权**；没有跨 ns 的默认全量返回。
**不做** `POST /keys/{id}/revoke` → `DELETE` 的改名、不删 `/watch`、不合并 `/poll`（这些不是本轮批准项）。

`GET /v1/console/events?namespace=NAME`：分页只有 `limit` + 不透明 `continue`，返回 `EventList`（`metadata.continue` + `items[]` 是 `Event` 信封）。刷新第一页（最新）按 `metadata.uid` 去重，**不暗加**旧的 `sinceId` / `nextCursor` 协议；`continue` 必须绑定 ns + 筛选条件，不能拿另一个 ns 的 cursor 读越权记录。已有 `source` / `traceId` 过滤可保留，但同样要明确 ns 绑定。

### 8.4 其它现有读模型

`/v1/devices`、`/v1/vapid-public-key`、`/v1/capability-templates` 沿用现有 cluster/读模型端点；
`/v1/stats`、`/v1/orphans` 必须显式带 `namespace` 过滤并逐 ns 授权；
`edges` / `deployments` 是 ns 私有事实，读路径统一为 `/v1/namespaces/{namespace}/edges`、`/v1/namespaces/{namespace}/deployments`。
本轮不重新设计；是否下线另开决策，不在 v4 API 派生里顺手删。`capability-templates` 是公共模板，不能承载 ns 私有数据。

### 8.5 保留域端点清单（形状不动）

| 域 | 端点（前缀 `/v1`） | 本轮 |
|---|---|---|
| auth | `/auth/me`、`/auth/passkeys[/{id}]`、`/auth/passkeys/register[/options]`、`/auth/password[/login, /register]`、`/auth/register[/options]`、`/auth/login[/options]`、`/auth/logout`、`/auth/sessions`、`/auth/recovery/{login,regenerate,status}` | 保 |
| keys | `/keys`、`/keys/{id}/revoke`、`/keys/self` | 保（不改成 DELETE） |
| cli-auth | `/cli-auth/sessions`、`/sessions/{id}`、`/by-code`、`/{id}/{approve,deny,poll,qr.txt}` | 保 |
| passkey-enroll | `/passkey-enroll/sessions`、`/{id}`、`/by-code`、`/{id}/{request,watch,poll,approve,deny,complete,qr.txt}` | 保（`/watch` 不删、不与 `/poll` 合并） |
| admin | `/admin`、`/admin/{stats,users,sessions,messages}`、`/admin/users/{id}/{role,disable,enable}` | 保 |
| 其它读模型 | `/devices[/{id}]`、`/vapid-public-key`、`/stats`、`/orphans`、`/capability-templates`（cluster） | 保（不顺手下线） |
| ns 私有事实读 | `/namespaces/{namespace}/edges`、`/namespaces/{namespace}/deployments`、`/namespaces/{namespace}/events`、`/namespaces/{namespace}/traces/{id}` | 保能力，路径进 ns |

> 这些端点的请求/响应形状本轮不改，只统一错误体为 `Status`（§4）。路径改名/删端点不是已批准项。

---

## 9. 旧 → 新差异摘要

> 本节只列**语义变化**；未列出的端点表示沿用现有形状与路径（现有路径本来就是 `/v1/...`）。
> 逐 operation 的对账在实现阶段从 `api/openapi.yaml` 重新生成，不在文档里维护第二份 109 行清单。

| 域 | 变化 | 依据 |
|---|---|---|
| 全局 | 路径保留 `/v1`；不再讨论 `/apis`；错误体统一 `Status`；资源端点套信封 | A1–A3 / A12 |
| capabilities | `metadata.generation` → 保留 `metadata.version`；删 `CapabilityRevision` kind / `revisions` 语义；`versions` 只读；`downstreams` 写入口删除；**保留人工 `POST …/deploy`**；删除不因 `next`/`targets` 阻塞 | A4 / A7 / A21 |
| sources | `spec.type` → **`spec.kind`**；fire / dry-run / dry-run-by-id 保留（ns 路径）；hooks 不变；读响应只给 `hookUrl` / `secretSet`；同名源 `targets` 差异 → `unapplied[]` / `targets_unapplied`，体检不全绿 | A4 / D3 |
| playbooks | `POST` 创建同 slug 409；改包 `PUT`；install 每次新回执且**不部署**；`installs` 按 valuesDigest 折叠 + 单回执详情；`render` 与 `dryRun` 分开；回执不是资源 | A13 / A14 / A22 |
| tasks | 只读 + cancel/abandon；`resolvedEnv`（实际值）→ `envDiagnostics`（名字 + 来源）；phase 小写 + `waitingReason`/`nextAttemptAt`；attempt/lease 上 wire | A15 |
| events | 只读；`id` 去重窗口 30 天；过期可再次接收；无 replay | A17 |
| nodes | cluster-scoped；`envPassthrough` / 本机私有字段不上报；`machineVars` 不做扁平 `effectiveVars`；`?namespace=` 看 ns 上下文 | A11 / A15 |
| edges | 只保留 `GET /v1/namespaces/{namespace}/edges`（派生投影）；`POST/DELETE`、`PUT downstreams` 删除 | A5 / A19 |
| deployments | 只保留 `GET /v1/namespaces/{namespace}/deployments`（事实）；写/删删除；部署由人工 `POST …/deploy` 写事实 | A6 / A21 |
| namespaces | `namespace` 进 path；body 显式冲突 400；无 finalizers | A11 / A16 |
| config | 节点默认值 + ns 默认值；`if_revision` 删除，配置写**必须** `resourceVersion` CAS；新增 `/nodes/{name}/config`、`/namespaces/{name}/config`、`/system/config`；ns 默认值不写 applied | D2 / A9 / A20 |
| warnings | 响应顶层可选 `warnings[]`，与信封四键并列 | A23 |
| 保留 | auth / admin / keys / passkey / cli-auth / devices / vapid / stats 端点形状不动（仅错误体） | 最小变更 |

**明确移除（不是“旧 109 op 兼容”的一部分）**：`/apis` group 路由、`CapabilityRevision`、`edges` / `deployments` 写端点、`PUT …/downstreams`、`spec.seedBuiltinCapabilities`、`if_revision`、`allNamespaces`、按 `next`/`targets` 的删除阻塞。
**明确保留**：CLI 登录 / CLI 升级 / 设备 / passkey / keys / admin / stats 等本轮未决定下线的能力。

---

## 10. 实现分线与门禁

### 10.1 四条互不依赖的实现线

| 线 | 文件所有权（禁止越界） | 依赖 | 验收命令 |
|---|---|---|---|
| **L1 store** | `internal/store/{schema.sql,migrate.go}` + 各 `*_test.go` 往返用例 | 无 | `go test ./internal/store/...` |
| **L2 契约文件** | `api/*.yaml`、`scripts/check_openapi.py`、`internal/server/openapi_parity_test.go` | L1 字段名 | `make contract` |
| **L3 投射层 + handler** | `internal/server/*_handlers.go`、`internal/server/mux.go`（如需 DTO 层再单开） | L1 / L2 | `go test ./internal/server/...` |
| **L4 前端 + mock** | `web-src/**`、`web/{partials,mock,graphs}.js` | L2 | `make fe && make e2e SUITE=frontend` |

**没有 L4 reconciler 线**：`status` 读时派生，不需要控制循环。这是与上一版草案最大的结构差异。

### 10.2 schema

- **开发库直接建 v4 baseline**，不追加 `-- +migration 12`，不写 `rv_counter` / `status_json` / `observed_generation`；
  旧库由 `store.Open` 明确报错（`decisions.md` V4-D6）。
- 任务表需要 attempt / lease / dedupe 字段；`capability_versions` 不新增 `generation` 列（版本仍是 `version`）。
- 加列规则仍按 `DEVELOPMENT.md`「schema 变更」；本版例外仅限“没有存量库要升级”。

### 10.3 契约门禁

| 门禁 | 要补 |
|---|---|
| `scripts/check_openapi.py` | `$ref` 可解析；每个 response 有 `content`；kind 的 `apiVersion`/`kind` 与路径一致 |
| `internal/server/openapi_parity_test.go` | 覆盖全部 kind；`Status.reason` 封闭集合快照；**不得出现** `generation` / `labels` / `fieldSelector` / `finalizers` / `/apis` / `if_revision` / `seedBuiltinCapabilities` / `allNamespaces`；资源删除不得用 `in_use` 阻塞 |
| 新增 `scripts/check_envelope.py` | 资源响应同时有 `apiVersion/kind/metadata/spec/status`；List 有 `items`；`status` 不出现在写请求 schema；`warnings[]` 允许在顶层；§3.1 的例外文档（`InstallReceipt` / 配置 wrapper / `NodeToken` / 受 Node 身份鉴权的内容通道 `GET /v1/node/capabilities/{digest}`（完整 `artifact.ArtifactJSON`） / 动作回执与预览 / `logs` / `emissions` / 读模型）从强制检查里显式排除，不靠缺键蒙过 |

### 10.4 落地顺序

```
P9.0  文档冻结（本文件 + decisions.md）
P9.1  L1 store：v4 baseline（无历史 migration）
P9.2  L2 契约文件：5 个 yaml + 三条门禁
P9.3  L3 handler 切换（信封 / namespace path / status 派生）
P9.4  L4 前端 + mock
P9.5  删旧写入口：edges 写、deployments 写、downstreams PUT
```

---

## 11. 文档同步记录

本轮已按 `decisions.md` 回改：

| 文档 | 回改 |
|---|---|
| `README.md` | 三层配置源、状态保留说明、`envPassthrough`、决策索引 |
| `config.md` | 三层 `vars`、`limits` 两层、`applied` 只机器级、双 revision |
| `capability.md` | idempotent 默认 false、7 天/30 天保留、租约/attempt、emit `id`、PATH/HOME、files 规范化 |
| `source.md` | D3 文件引用、事件 `id` 保留窗口、过期可再接收 |
| `playbook.md` | 源 targets 差异 `unapplied`、config 归一化、POST/PUT、回执引用完整性、体检不全绿 |
| `anode.md` | `envPassthrough`、PATH/HOME、token 不绑 ns、三层 vars |
| `api-conventions.md` | 本文：去掉 `/apis`、CapabilityRevision、labels、finalizers、reconciler、migration 12 |

---

## 12. 未决（不影响已批准语义）

| # | 问题 | 现状 |
|---|---|---|
| Q1 | `GET /v1/namespaces/{namespace}/edges` 是否够前端拓扑页用（单次快照） | 保留只读端点，优先不改前端算法 |
| Q2 | 列表游标 | 资源列表沿用现有 `limit`；只有 `/v1/console/events` 与 `…/tasks/{name}/emissions` 两个明细集合用 `limit` + 不透明 `continue`（§8.3 / §7.6）；其余不预造 `continue` |
| Q3 | 审计 / 状态历史 | 明确不做（`decisions.md` 刻意不做清单） |
