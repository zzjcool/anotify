# 冻结契约 · #432 能力启停 + 手动测试（wire 层）

> 迁移自 .pi-orchestrator/cap-lifecycle/WIRE.md（2026-11-19，协调者工作区不再入库）

> 本文件由协调者在实现开始前冻结，是后端 / 前端 / e2e 三个执行面的**唯一**接口约定。
> 语义真源仍是实现本身（`internal/server` 的能力启停/试跑路径 + `internal/store` 的状态派生）；L-D1..L-D8 的原表
> 随设计文档一并移除（历史入口见 `docs/README.md`）。本文件只钉
> 「wire 上到底是什么字节」，不改设计。

## 0. 已落库的基础设施（勿重复实现）

| 位置 | 内容 |
| --- | --- |
| `internal/store/schema.sql` | `capability_switches` / `capability_audit` 两张表；`events.origin`（'source'/'test'） |
| `internal/protocol/enums.go` | `ReasonCapabilityDisabled = "capability_disabled"`（已进 `pruneReasons` 闭集） |
| `internal/store/capability_switch.go` | `GetCapabilitySwitch` / `IsCapabilityEnabled` / `SetCapabilitySwitch`（含审计，同事务）/ `AppendCapabilityAudit` / `ListCapabilityAudit` |
| `internal/store/capability_test_event.go` | `AcceptTestEvent(ctx, CapabilityTestInputs) (*CapabilityTestReceipt, error)`（事件+任务+审计同事务） |
| `internal/store/event.go` | `Event.Origin`、`EventOriginSource/Test`、`EventTestSource`（试跑事件写进 `events.source` 的固定文本 `"capability"`）、`ListEventsWithOrigin` |
| `internal/store/stats.go` / `admin.go` | 事件数口径已排除 `origin='test'` |

关键签名（server 侧直接调，**不要**改 store 包）：

```go
func (d *DB) GetCapabilitySwitch(ctx, namespaceID, name string) (*CapabilitySwitch, error) // 无行 → Enabled=true, nil
func (d *DB) SetCapabilitySwitch(ctx, in CapabilitySwitchInput) (*CapabilitySwitch, error)
type CapabilitySwitchInput struct{ NamespaceID, Name string; Enabled bool; Reason, ActorID string; Now int64 }
func (d *DB) ListCapabilityAudit(ctx, namespaceID, name string, limit int) ([]*CapabilityAuditEntry, error) // 0<limit<200 已夹取
type CapabilityAuditEntry struct{ ID, NamespaceID, Name, Action, ActorID string; Detail json.RawMessage; CreatedAt int64 }
func (d *DB) AcceptTestEvent(ctx, in CapabilityTestInputs) (*CapabilityTestReceipt, error)
type CapabilityTestInputs struct {
    NamespaceID, TraceID, Capability string
    Version int
    Payload json.RawMessage   // 事件 payload（params 快照）
    Tasks []*Task             // 必须 1..n 条；CapabilityID/Digest 已钉住
    Prunes []PruneInputs
    ActorID string
    Now int64
}
type CapabilityTestReceipt struct {
    EventID string `json:"eventId"`
    TraceID string `json:"traceId"`
    Origin  string `json:"origin"`            // 恒 "test"
    Tasks   []SourceEventReceiptTask `json:"tasks"`
    Pruned  []SourceEventReceiptPrune `json:"pruned"`
}
```

## 1. 四个端点（全部 tag `capabilities`，`security: [{session: []}]`）

### 1.1 `POST /v1/namespaces/{namespace}/capabilities/{name}/enable`

- 无请求体（若带则忽略）。
- `200` → **整个 `Capability` 信封**（`apiVersion/kind/metadata/spec/status`），`status.enabled: true`，
  `metadata.resourceVersion` 是能力行当前的令牌（enable/disable **不**改能力行的 resource_version）。
- 错误：`400 Status400` / `401 AuthError401` / `403 Status403` / `404 Status404` / `500 Status500`。
- **不套 CAS**：动作 POST 不带 resourceVersion（与 `deployCapability` 一致）。

### 1.2 `POST .../{name}/disable`

- body（可选）：`{"reason": "上游在改接口"}` —— `additionalProperties: false`，`reason.maxLength: 200`。
- `200` → 同上，`status.enabled: false`。
- 语义：只拦**自动**触发；已排队/进行中的任务不取消不暂停；`deploy`/建版本/删除不受影响。

### 1.3 `POST .../{name}/test`

- body（可选，`additionalProperties: false`）：

```json
{"version": 2, "params": {"diff": "..."}, "nodes": ["node-a"]}
```

  - `version`：省略 / 0 = 当前 live 版本（走 `ResolveRef`）；显式版本不存在 → `404`。
  - `params`：按 `spec.params` 的 JSON Schema 校验（`route.PrepareParams`）；不合 schema → `400`
    `reason=BadRequest`（或既有约定码），`details.causes[]` 每条 `{reason, field, message}`，
    `field` 是**字段路径**（如 `params.diff`）。
  - `pattern`（#475）：`spec.params` 里的 `pattern` 是 RE2 正则，只约束字符串值
    （非 string 值不判；整串匹配要在 pattern 里自己写 `^...$`，RE2 默认子串匹配）。
    值不匹配 → 同一个 400，`details.causes[].field` 指到 `params.<路径>`（嵌套如
    `params.meta.dir`），message 同时给期望的 pattern 与实际值。正则本身编不过 /
    不是字符串 / 超过长度上限 → **保存能力时**就 400 `param_pattern_invalid`，
    `field` 指到 `spec.params.properties.<name>.pattern`。派发期（源事件 / emit）
    同样执行 `pattern`：不匹配记 `param_type` 剪枝、不建任务。
  - `nodes`：候选节点白名单（节点名或 id），只用于回执 `warnings` 的判定；显式给了但解析不到 → `404`。
    **它不改变派发目标**：v4 在 `lease.grant` 时按 eligible 选点，任务行不钉节点（已写入 OpenAPI 描述）。
- `200` → `CapabilityTestReceipt`（**不是** 202；与 `fireSource` 同一状态码语义）：

```json
{
  "traceId": "evt_...",
  "origin": "test",
  "tasks": [{"taskId": "tsk_...", "capability": "review", "version": 2}],
  "pruned": [],
  "warnings": []
}
```

  - 空列表一律 `[]`（不是 null）。
  - 一个节点都没持有该 digest → **仍然 200**，`warnings: [{"reason":"capability_not_deployed","detail":"..."}]`；
    任务 `phase=pending`（**不是 4xx、不是任务失败码**）。
  - `pruned[]` 元素 = `PruneFact`（`{capability?, reason, detail?}`）。
- 在 `disabled` 能力上**可用**（L-D2：停用拦的是自动触发，不是人工验证手段）。
- 审计：同一事务写 `action='test'`，`detail={traceId, version}`（不存 params 全文）。

### 1.4 `GET .../{name}/audit?limit=10`

- `limit`：`1..200`，默认 50（store 已夹取）。
- `200` →（非信封，与 `EdgeList` 同类）：

```json
{"items": [
  {"id": "aud_...", "action": "disable", "actor": "usr_...",
   "at": "2026-11-16T10:00:00Z", "detail": {"reason": "上游在改接口"}}
]}
```

  - `items` 按 `created_at DESC, id DESC`（最新在前），空 → `{"items": []}`。
  - `action ∈ enable|disable|test`；`actor` 空串 = 非会话主体（CLI/系统），不做伪造。
  - `at` 是 RFC3339（UTC）。`detail` 是对象（`{}` 兜底）。

## 2. `CapabilityStatus` 新字段

```yaml
enabled:
  type: boolean   # 派生投影：开=接受新触发；关=拒绝自动触发（留剪枝事实 capability_disabled）
```

Go：`capabilityStatus.Enabled bool \`json:"enabled"\``，由 `statusFor` 从
`DB.GetCapabilitySwitch` 填（**无行 = true**）。写请求体带非空 `status` 仍 400 `status_not_writable`。

## 3. 前端 DOM 约定（e2e 会按此定位，不许改名）

| 选择器 | 含义 |
| --- | --- |
| `#cap-enabled-badge` | 状态徽标：`已启用`（`.badge.badge-success`）/ `已停用`（`.badge.badge-neutral`） |
| `#cap-actions [data-act="enable"]` | 启用按钮（`status.enabled=false` 时出现） |
| `#cap-actions [data-act="disable"]` | 停用按钮（`status.enabled=true` 时出现） |
| `#cap-actions [data-act="test"]` | 试跑按钮（恒出现） |
| `#test-modal` / `#test-version` / `#test-params` / `#test-params-json` / `#test-nodes` / `#test-submit` / `#test-close` / `#test-result` | 试跑弹窗（照抄 `sources.html` 的 `#fire-modal` 范式） |
| `#test-result` 文本含 `traceId` 值 | 回执面板（traceId 可点，链接到 `trace.html?traceUID=`） |
| `#cap-audit`（`[data-sec="audit"]` 节内） | 最近操作列表（读 `GET …/audit?limit=10`） |

试跑参数表单：优先按 `spec.params` 的 JSON Schema 渲染（对象/原始类型/`enum`），
`oneOf`/深层 `$ref` 等抽不出来的 schema 降级为一个 JSON textarea（`#test-params-json`），
服务端仍是最终判据（`details.causes[].field` 直接标到字段旁）。
