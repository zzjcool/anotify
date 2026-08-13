# 接收端能力/权限订阅模型 · 代码侦察上下文（context.md）

> 基于 `.pi-orchestrator/receiver-capability-design.md`（pm 设计文档）逐文件读码核实。
> 所有行号基于当前 HEAD 代码，改前状态精确到行。

---

## 1. 改动点总览表

| # | 文件 | 改什么 | 优先级 |
|---|------|--------|--------|
| 1 | `api/openapi.yaml` | NotifyRequest 删 status 加 agentState/severity/kind/replyTo；TestNotifyRequest 同理；Device PATCH 删 statusFilter 加 eventScope；AdminGlobalMessage status→agentState | P0 |
| 2 | `internal/broker/broker.go` | 删 Status 字段+5 常量；加 AgentState/Severity/Kind/ReplyTo 字段+常量+IsTerminal | P0 |
| 3 | `internal/broker/sqlite.go` | Publish 默认值改、INSERT/Replay SELECT SQL 改列名 | P0 |
| 4 | `internal/store/schema.sql` | messages.status→agent_state+加列；devices.status_filter→event_scope | P0 |
| 5 | `internal/store/store.go` | migrateColumns 加幂等迁移（新列+改列名回退） | P0 |
| 6 | `internal/store/messages.go` | MessageRow struct 改字段；InsertMessage/GetMessage/InsertTestMessage SQL+签名改 | P0 |
| 7 | `internal/store/devices.go` | Device struct 改 StatusFilter→EventScope；所有 SELECT/INSERT/UPDATE SQL 改列名 | P0 |
| 8 | `internal/store/admin.go` | AdminGlobalMessage.Status→AgentState；ListGlobalMessages SELECT 改列 | P0 |
| 9 | `internal/route/filter.go` | 删 StatusMatch；加 ScopeMatch+IsTerminal；ShouldDeliver 改调用 | P0 |
| 10 | `internal/api/notify.go` | NotifyRequest/TestNotifyRequest struct 改字段；validStatuses→validAgentStates；Message 构造改映射+severity 派生 | P0 |
| 11 | `internal/ws/protocol.go` | Frame 加 agentState/severity/kind；notificationFrame 改字段映射 | P0 |
| 12 | `internal/ws/handler.go` | session 加 eventScope；subscribe 帧解析+matchScope 过滤；subscribed 回显 | P0 |
| 13 | `internal/server/handlers.go` | devicePatchReq 改 statusFilter→eventScope；deviceUpsert 默认改 final；messageView 改 Status→AgentState | P0 |
| 14 | `internal/push/dispatcher.go` | pushPayload 可选加 agentState/severity（可选，非必须） | P1 |
| 15 | 前端 `web-src/pages/receivers.html` | statusFilter→eventScope；FILTER_LABEL 改 final/all | P0 |
| 16 | 前端 `web-src/pages/index.html` | STATUS_META 改 agentState+severity；tn-status select 改；filter 逻辑改 | P0 |
| 17 | 前端 `web-src/pages/message.html` | STATUS_META 改 agentState+severity；渲染逻辑改 | P0 |
| 18 | 前端 `web-src/pages/admin.html` | statusBadgeEl 改 agentState | P0 |
| 19 | 前端 `web-src/pages/docs.html` | curl 示例改 agentState；投递规则说明改 ScopeMatch | P1 |
| 20 | `web-src/locales/*.yaml` (4 文件) | 加 agentState.*/eventScope.*/severity.* 词条；旧 status.* 改/留 | P0 |
| 21 | 测试 `internal/*_test.go` (7 文件) | 改 Status→AgentState 断言、StatusMatch→ScopeMatch、validStatuses 等 | P0 |
| 22 | E2E `scripts/e2e/suites/*.mjs` (7 套件) | 改 status→agentState、statusFilter→eventScope 断言 | P0 |
| 23 | `scripts/ws_test.mjs` | status→agentState | P1 |
| 24 | 跨仓库 `anotify-plugins` | notify.sh --status→--agent-state；pi/anotify.ts 事件映射 | 跨仓库 |

---

## 2. 逐文件清单

### 2.1 `api/openapi.yaml`

**NotifyRequest schema**（约第 1053-1068 行，components.schemas.NotifyRequest）：
```yaml
# 当前（改前）:
NotifyRequest:
  type: object
  required: [status, title]
  properties:
    ...
    status:
      { type: string, enum: [success, error, interrupted, info, warning] }
    ...
```
- **删**：`required: [status, title]` → `required: [agentState, title]`
- **删**：`status` 属性行
- **加**：`agentState` `{ type: string, enum: [working, blocked, done, interrupted, error] }`
- **加**：`severity` `{ type: string, enum: [info, warning, error] }`（optional）
- **加**：`kind` `{ type: string, enum: [task], default: task }`
- **加**：`replyTo` `{ type: string }`（optional）

**TestNotifyRequest schema**（约第 1075-1083 行）：
- **删**：`status` 属性（含 enum + default: info）
- **加**：`agentState` `{ type: string, enum: [working, blocked, done, interrupted, error], default: done }`
- **加**：`severity` `{ type: string, enum: [info, warning, error] }`（optional）

**Device PATCH** `/devices/{id}`（约第 55-60 行）：
```yaml
# 当前:
statusFilter: { type: string, enum: [all, error, success] }
```
- **改为**：`eventScope: { type: string, enum: [final, all] }`

**DeviceUpsert schema**（约第 1037-1051 行）：当前无 statusFilter 字段（注册时不带）。
- 无需改 schema，但 `handlers.go` 中 upsert 默认值要改（见 2.13）。

**AdminGlobalMessage schema**（约第 1185-1193 行）：
- `status: { type: string }` → `agentState: { type: string }`

### 2.2 `internal/broker/broker.go`

**常量块**（第 14-19 行）：
```go
// 当前:
const (
    StatusSuccess     = "success"
    StatusError       = "error"
    StatusInterrupted = "interrupted"
    StatusInfo        = "info"
    StatusWarning     = "warning"
)
```
- **删全部 5 个常量**
- **加**：
```go
const (
    AgentStateWorking     = "working"
    AgentStateBlocked     = "blocked"
    AgentStateDone        = "done"
    AgentStateInterrupted = "interrupted"
    AgentStateError       = "error"
)

func IsTerminal(state string) bool {
    return state == AgentStateDone || state == AgentStateInterrupted || state == AgentStateError
}
```

**Message struct**（第 27-41 行）：
```go
// 当前:
type Message struct {
    ...
    Status     string    `json:"status"`     // success|error|interrupted|info|warning
    ...
}
```
- **删** `Status string \`json:"status"\``  （第 34 行）
- **加** `AgentState string \`json:"agentState"\``  （替换同一位置）
- **加** `Severity string \`json:"severity,omitempty"\``  （在 AgentState 后）
- **加** `Kind string \`json:"kind,omitempty"\``  （在 Severity 后，或靠近 Priority）
- **加** `ReplyTo string \`json:"replyTo,omitempty"\``  （在 Kind 后）

### 2.3 `internal/broker/sqlite.go`

**Publish 方法默认值**（第 53-54 行）：
```go
// 当前:
if msg.Status == "" {
    msg.Status = StatusInfo
}
```
- **改为**：
```go
if msg.AgentState == "" {
    msg.AgentState = AgentStateWorking
}
```

**Publish INSERT SQL**（第 96 行）：
```sql
-- 当前:
INSERT INTO messages (id, user_id, seq, title, status, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
```
- **改为**（加 severity/kind/reply_to 列，status→agent_state）：
```sql
INSERT INTO messages (id, user_id, seq, title, agent_state, severity, kind, reply_to, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
```
- 参数列表也要同步加 `msg.Severity, msg.Kind, msg.ReplyTo`

**Replay SELECT SQL**（第 97 行）：
```sql
-- 当前:
SELECT id, user_id, seq, title, status, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at
```
- **改为**：
```sql
SELECT id, user_id, seq, title, agent_state, severity, kind, reply_to, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at
```

**scanMessage 函数**（第 312-325 行）：
- Scan 目标从 `&m.Status` 改为 `&m.AgentState, &m.Severity, &m.Kind, &m.ReplyTo`（加 3 个新变量）

### 2.4 `internal/store/schema.sql`

**messages 表**（第 70-82 行）：
```sql
-- 当前:
status      TEXT NOT NULL,               -- success|error|interrupted|info|warning
```
- **改为**：
```sql
agent_state TEXT NOT NULL DEFAULT 'working',
severity    TEXT NOT NULL DEFAULT '',
kind        TEXT NOT NULL DEFAULT 'task',
reply_to    TEXT NOT NULL DEFAULT '',
```

**devices 表**（第 58 行）：
```sql
-- 当前:
status_filter TEXT NOT NULL DEFAULT 'all', -- all|error|success
```
- **改为**：
```sql
event_scope TEXT NOT NULL DEFAULT 'final', -- final|all（push 设备默认 final）
```

### 2.5 `internal/store/store.go`

**migrateColumns 函数**（第 63-75 行）：
- 需 **加** 幂等迁移：
  - `ALTER TABLE messages ADD COLUMN agent_state TEXT NOT NULL DEFAULT 'working'`（或用 rename 策略）
  - `ALTER TABLE messages ADD COLUMN severity TEXT NOT NULL DEFAULT ''`
  - `ALTER TABLE messages ADD COLUMN kind TEXT NOT NULL DEFAULT 'task'`
  - `ALTER TABLE messages ADD COLUMN reply_to TEXT NOT NULL DEFAULT ''`
  - `ALTER TABLE devices ADD COLUMN event_scope TEXT NOT NULL DEFAULT 'final'`
- **注意**：SQLite 不支持 `RENAME COLUMN`（3.25.0+ 支持 `ALTER TABLE ... RENAME COLUMN`）。由于 schema.sql 已直接改，新库不需要迁移。但 `migrateColumns` 的 try/ignore 模式只对 ADD COLUMN 幂等——如果 schema.sql 已经有 `agent_state` 列，再 `ADD COLUMN` 会报错被 `_` 忽略。**推荐策略**：schema.sql 直接写新列名，migrateColumns 不做 rename，只做 ADD COLUMN（新库已有列会忽略错误；老库 schema.sql 重跑会 `CREATE TABLE IF NOT EXISTS` 不改表，靠 migrateColumns 补列）。但老库的 `status`/`status_filter` 列不会被删，只是不被引用——对开发阶段可接受。

### 2.6 `internal/store/messages.go`

**MessageRow struct**（第 17-29 行）：
```go
// 当前:
type MessageRow struct {
    ...
    Status     string
    ...
}
```
- **删** `Status string`（第 23 行）
- **加** `AgentState string`、`Severity string`、`Kind string`、`ReplyTo string`

**InsertMessage SQL**（第 66 行）：
```sql
-- 当前:
INSERT INTO messages (id, user_id, seq, title, status, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
```
- 改法同 broker/sqlite.go 的 Publish INSERT

**InsertMessage 参数**（第 68 行）：
- `msg.Status` → `msg.AgentState`，加 `msg.Severity, msg.Kind, msg.ReplyTo`

**GetMessage SELECT + Scan**（第 83-86 行）：
```sql
-- 当前:
SELECT id, user_id, seq, title, status, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at
```
- 改法同 Replay SELECT；Scan 加新字段

**InsertTestMessage 签名**（第 105 行）：
```go
// 当前:
func (d *DB) InsertTestMessage(ctx context.Context, id, userID string, seq int64, status string) error {
    ...
    Status:     status,
    ...
}
```
- **改为**：`func (d *DB) InsertTestMessage(ctx context.Context, id, userID string, seq int64, agentState string) error {`
- `Status: status` → `AgentState: agentState`

### 2.7 `internal/store/devices.go`

**Device struct**（第 14-26 行）：
```go
// 当前:
StatusFilter  string   `json:"statusFilter"` // all|error|success
```
- **改为**：`EventScope string \`json:"eventScope"\` // final|all`

**ListDevices SELECT**（第 31 行）：
```sql
-- 当前:
SELECT id, user_id, name, platform, enabled, status_filter, tags, ...
```
- **改为**：`... event_scope, tags, ...`

**ListEnabledDevices SELECT**（第 53 行）：同上改 `status_filter`→`event_scope`

**scanDevice**（第 82 行）：
- `&dev.StatusFilter` → `&dev.EventScope`

**UpsertDevice INSERT**（第 123 行）：
```sql
-- 当前:
INSERT INTO devices (id, user_id, name, platform, enabled, status_filter, tags, endpoint, p256dh, auth, user_agent, created_at, last_active)
```
- **改为**：`... event_scope, tags, ...`

**UpsertDevice 参数**（第 128 行）：
- `dev.StatusFilter` → `dev.EventScope`

**UpdateDevice UPDATE**（第 150 行）：
```sql
-- 当前:
UPDATE devices SET name=?, platform=?, enabled=?, status_filter=?, tags=?, last_active=?
```
- **改为**：`... event_scope=?, tags=?, last_active=?`

**UpdateDevice 参数**（第 152 行）：
- `dev.StatusFilter` → `dev.EventScope`

### 2.8 `internal/store/admin.go`

**AdminGlobalMessage struct**（第 220-229 行）：
```go
// 当前:
Status    string `json:"status"`
```
- **改为**：`AgentState string \`json:"agentState"\``

**ListGlobalMessages SELECT + Scan**（第 234 行）：
```sql
-- 当前:
SELECT m.id, m.user_id, u.username, m.seq, m.title, m.status, m.created_at
```
- **改为**：`... m.agent_state, m.created_at`
- Scan：`&m.Status` → `&m.AgentState`

### 2.9 `internal/route/filter.go`

**整个 StatusMatch 函数**（第 18-31 行）：
- **删** `StatusMatch(filter, msgStatus string) bool` 全函数

**加 ScopeMatch**：
```go
// ScopeMatch 判定设备的订阅范围是否放行该消息。
// "all"   → 全部通过
// "final" → 仅终态（done/interrupted/error）通过
func ScopeMatch(filter, agentState string) bool {
    switch filter {
    case "", "all":
        return true
    case "final":
        return broker.IsTerminal(agentState)
    default:
        return false
    }
}
```

**ShouldDeliver**（第 64-69 行）：
```go
// 当前:
if !StatusMatch(dev.StatusFilter, msg.Status) {
    return false
}
```
- **改为**：
```go
if !ScopeMatch(dev.EventScope, msg.AgentState) {
    return false
}
```

**包注释**（第 5 行）：更新 `statusMatch` → `scopeMatch` 描述

### 2.10 `internal/api/notify.go`

**NotifyRequest struct**（第 53-64 行）：
```go
// 当前:
type NotifyRequest struct {
    ...
    Status     string   `json:"status"`
    ...
}
```
- **删** `Status string \`json:"status"\``（第 57 行）
- **加** `AgentState string \`json:"agentState"\``
- **加** `Severity string \`json:"severity"\``
- **加** `Kind string \`json:"kind"\``
- **加** `ReplyTo string \`json:"replyTo"\``

**validStatuses**（第 60-66 行）：
```go
// 当前:
var validStatuses = map[string]bool{
    broker.StatusSuccess:     true,
    broker.StatusError:       true,
    broker.StatusInterrupted: true,
    broker.StatusInfo:        true,
    broker.StatusWarning:     true,
}
```
- **改为**：
```go
var validAgentStates = map[string]bool{
    broker.AgentStateWorking:     true,
    broker.AgentStateBlocked:     true,
    broker.AgentStateDone:        true,
    broker.AgentStateInterrupted: true,
    broker.AgentStateError:       true,
}
```

**ServeHTTP 校验**（第 154-157 行）：
```go
// 当前:
if !validStatuses[req.Status] {
    writeError(w, http.StatusBadRequest,
        fmt.Sprintf("status must be one of success|error|interrupted|info|warning, got %q", req.Status))
    return
}
```
- **改为**：校验 `req.AgentState`，枚举改为 `working|blocked|done|interrupted|error`

**ServeHTTP Message 构造**（第 179-183 行）：
```go
// 当前:
msg := &broker.Message{
    ...
    Status:     req.Status,
    ...
}
```
- **改为**：`AgentState: req.AgentState`，加 `Severity: deriveSeverity(req)`、`Kind: req.Kind`（default "task"）、`ReplyTo: req.ReplyTo`

**severity 派生逻辑**（新增 helper）：
```go
func deriveSeverity(req *NotifyRequest) string {
    if req.Severity != "" {
        return req.Severity
    }
    switch req.AgentState {
    case broker.AgentStateError:
        return "error"
    case broker.AgentStateBlocked, broker.AgentStateInterrupted:
        return "warning"
    default:
        return "info"
    }
}
```

**slog.Info 日志**（第 223 行）：
- `"status", req.Status` → `"agentState", req.AgentState`

**TestNotifyRequest struct**（第 248-254 行）：
- `Status string \`json:"status"\`` → `AgentState string \`json:"agentState"\``
- 加 `Severity string \`json:"severity"\``

**ServeTestNotify 校验+构造**（第 266-295 行）：
- 默认值 `status = broker.StatusInfo` → `agentState = broker.AgentStateDone`（测试通知默认终态 done）
- `validStatuses` → `validAgentStates`
- Message 构造 `Status: status` → `AgentState: agentState`，加 severity 派生
- payload map 里 `"status": status` → `"agentState": agentState`
- 日志 `"status", status` → `"agentState", agentState`

### 2.11 `internal/ws/protocol.go`

**Frame struct notification 字段**（第 42-48 行）：
```go
// 当前:
// notification
Status  string   `json:"status,omitempty"`
```
- **删** `Status string \`json:"status,omitempty"\``
- **加**：
```go
AgentState string `json:"agentState,omitempty"`
Severity  string `json:"severity,omitempty"`
Kind      string `json:"kind,omitempty"`
```

**notificationFrame 函数**（第 64-76 行）：
```go
// 当前:
return &Frame{
    ...
    Status:  msg.Status,
    ...
}
```
- **改为**：`AgentState: msg.AgentState, Severity: msg.Severity, Kind: msg.Kind`

**subscribe 帧**：Frame 已有 `Tags []string` 字段（第 53 行）。需加 `EventScope string \`json:"event_scope,omitempty"\``（或 `eventScope`——注意 JSON tag 命名：当前帧用 snake_case 如 `subscribed_tags`/`event_id`/`sent_at`，所以新字段也用 snake_case `event_scope`）。

**subscribed 帧回显**：subscribed 帧已有 `SubscribedTags`，需加 `EventScope string \`json:"event_scope,omitempty"\`` 字段到 Frame struct。

### 2.12 `internal/ws/handler.go`

**session struct**（第 107-118 行）：
- **加** `eventScope string` 字段（与 tags 并列）

**session 初始化**（第 95 行）：
- `sess` 构造时 `eventScope: "all"`（WS 默认 all）

**handleFrame FrameSubscribe**（第 196-203 行）：
```go
// 当前:
case FrameSubscribe:
    s.setTags(normalizeTags(f.Tags))
    _ = s.writeJSON(ctx, &Frame{
        Type:           FrameSubscribed,
        SubscribedTags: s.getTags(),
        ResumeToken:    resumeTokenPrefix + strconv.FormatInt(s.lastSeq(), 10),
    })
```
- **加**：`s.setEventScope(f.EventScope)` + subscribed 帧回显 `EventScope`

**主循环过滤**（第 172 行）：
```go
// 当前:
if !matchTags(s.getTags(), msg.DeviceTags) {
    continue
}
```
- **改为**：
```go
if !matchTags(s.getTags(), msg.DeviceTags) {
    continue
}
if !matchScope(s.getEventScope(), msg.AgentState) {
    continue
}
```

**新增 matchScope helper**：
```go
func matchScope(filter, agentState string) bool {
    if filter == "" || filter == "all" {
        return true
    }
    return broker.IsTerminal(agentState) // "final"
}
```

**需加** `setEventScope`/`getEventScope` 方法（仿 setTags/getTags 用 mutex 保护）

### 2.13 `internal/server/handlers.go`

**deviceUpsert 默认值**（第 86 行）：
```go
// 当前:
StatusFilter: "all",
```
- **改为**：`EventScope: "final"`（push 设备默认 final——产品核心诉求）

**devicePatchReq struct**（第 111-116 行）：
```go
// 当前:
StatusFilter *string  `json:"statusFilter"`
```
- **改为**：`EventScope *string \`json:"eventScope"\``

**patch 方法校验**（第 146-152 行）：
```go
// 当前:
if req.StatusFilter != nil {
    switch *req.StatusFilter {
    case "all", "error", "success":
        dev.StatusFilter = *req.StatusFilter
    default:
        writeErr(w, 400, "statusFilter 仅支持 all|error|success")
        return
    }
}
```
- **改为**：
```go
if req.EventScope != nil {
    switch *req.EventScope {
    case "final", "all":
        dev.EventScope = *req.EventScope
    default:
        writeErr(w, 400, "eventScope 仅支持 final|all")
        return
    }
}
```

**messageView struct**（第 189-201 行）：
```go
// 当前:
Status     string          `json:"status"`
```
- **改为**：`AgentState string \`json:"agentState"\`` + 加 `Severity string \`json:"severity,omitempty"\``

**toMessageView 函数**（第 204-216 行）：
- `Status: m.Status` → `AgentState: m.AgentState, Severity: m.Severity`

**getOne 函数 Message 构造**（第 296-310 行）：
```go
// 当前:
msg := toMessageView(&broker.Message{
    ...
    Status:     row.Status,
    ...
})
```
- **改为**：`AgentState: row.AgentState, Severity: row.Severity`（MessageRow 也要有新字段）

### 2.14 `internal/push/dispatcher.go`

**pushPayload 函数**（第 155-167 行）：
- 当前推送载荷只发 `id/title/body/tag/url/link`。
- **可选改动**：加 `agentState` 和 `severity` 到推送载荷，让 Service Worker 能按状态着色。设计文档未明确要求，但前端 sw.js 渲染需要。建议加：
```go
p := map[string]any{
    "id":        msg.ID,
    "title":     msg.Title,
    "body":      msg.Body,
    "tag":       msg.ID,
    "url":       "message.html?id=" + url.QueryEscape(msg.ID),
    "link":      msg.Link,
    "agentState": msg.AgentState,
    "severity":  msg.Severity,
}
```

---

## 3. 旧符号全量引用清单

### 3.1 `StatusSuccess` / `StatusError` / `StatusInterrupted` / `StatusInfo` / `StatusWarning` 常量引用

| 文件 | 行号 | 引用 |
|------|------|------|
| `internal/broker/broker.go` | 16-19 | 定义处 |
| `internal/broker/sqlite.go` | 54 | `msg.Status = StatusInfo` |
| `internal/api/notify.go` | 61-65 | `validStatuses` map |
| `internal/api/notify.go` | 154-156 | `validStatuses[req.Status]` |
| `internal/api/notify.go` | 266 | `status = broker.StatusInfo` |
| `internal/route/filter.go` | 22, 23, 24, 28 | `broker.StatusError`, `broker.StatusSuccess` |
| `internal/push/filter_test.go` | 33-51 | 全部测试用例引用 |
| `internal/push/filter_test.go` | 55 | `route.StatusMatch` |
| `internal/push/dispatcher_test.go` | 101, 106 | `broker.StatusSuccess` |
| `internal/push/dispatcher_test.go` | 173, 222, 225 | `broker.StatusInfo`, `broker.StatusSuccess` |
| `internal/push/dispatcher_bench_test.go` | 40, 46 | `broker.StatusSuccess` |
| `internal/ws/handler_test.go` | 177, 201 | `broker.StatusSuccess`, `broker.StatusInfo` |
| `internal/ws/handler_test.go` | 293, 295 | `Status: "info"` (字面量) |
| `internal/route/filter_bench_test.go` | 46, 55, 81, 82, 97, 98 | `broker.StatusSuccess`, `broker.StatusError` |
| `internal/store/admin_test.go` | 134-135, 171-174, 210 | `Status: "success"` / `Status: "error"` (字面量) |
| `internal/store/messages_test.go` | 24, 47 | `Status: "success"` |
| `internal/api/notify_test.go` | 126, 338, 339 | `broker.StatusSuccess`, `broker.StatusInfo` |
| `internal/api/notify_bench_test.go` | 45, 60 | `StatusFilter: "all"`, `"status":"success"` (JSON body) |
| `internal/store/store_bench_test.go` | 34 | `StatusFilter: "all"` |

### 3.2 `.Status` 字段引用（broker.Message / MessageRow / Frame / messageView / AdminGlobalMessage）

| 文件 | 行号 | 上下文 |
|------|------|--------|
| `internal/broker/broker.go` | 34 | Message.Status 字段定义 |
| `internal/broker/sqlite.go` | 53, 96, 325 | `msg.Status`, INSERT 参数, scanMessage |
| `internal/store/messages.go` | 23, 68, 85, 105 | MessageRow.Status, InsertMessage, GetMessage, InsertTestMessage |
| `internal/route/filter.go` | 66 | `msg.Status` in ShouldDeliver |
| `internal/api/notify.go` | 57, 154, 181, 223 | NotifyRequest.Status, 校验, Message 构造, 日志 |
| `internal/ws/protocol.go` | 46, 71 | Frame.Status, notificationFrame |
| `internal/server/handlers.go` | 195, 210, 303 | messageView.Status, toMessageView, getOne |
| `internal/store/admin.go` | 225, 234 | AdminGlobalMessage.Status, ListGlobalMessages SELECT+Scan |
| `internal/push/dispatcher.go` | 无直接引用 .Status（通过 route.FilterDevices 间接） | — |

### 3.3 `StatusFilter` / `status_filter` / `statusFilter` 引用

| 文件 | 行号 | 上下文 |
|------|------|--------|
| `internal/store/schema.sql` | 58 | `status_filter TEXT NOT NULL DEFAULT 'all'` |
| `internal/store/devices.go` | 17, 31, 53, 82, 123, 128, 137, 150, 152 | struct 字段 + 4 处 SQL + scanDevice |
| `internal/route/filter.go` | 66 | `dev.StatusFilter` in ShouldDeliver |
| `internal/server/handlers.go` | 86, 113, 146-151 | upsert 默认值, patchReq struct, patch 校验 |
| `internal/push/filter_test.go` | 12, 16, 118-121 | dev() helper, FilterDevices 测试 |
| `internal/push/dispatcher_test.go` | 62 | insertDevice helper |
| `internal/route/filter_bench_test.go` | 18, 27 | benchDev helper |
| `internal/store/devices_update_test.go` | 21, 31, 45-46 | TestUpdateDevice |
| `internal/store/store_bench_test.go` | 34 | bench |
| `internal/api/notify_bench_test.go` | 45 | bench |
| `web-src/pages/receivers.html` | 336, 346, 356, 372, 648, 655, 938 | JS 中 statusFilter 属性 |
| `scripts/e2e/suites/routing.mjs` | 80, 85, 89, 96, 103, 116, 122, 125, 128, 134 | statusFilter 断言 |
| `scripts/e2e/suites/api_contract.mjs` | 232, 237, 243, 248 | PATCH statusFilter 断言 |

### 3.4 `StatusMatch` 函数引用

| 文件 | 行号 |
|------|------|
| `internal/route/filter.go` | 18-31 (定义), 66 (调用) |
| `internal/push/filter_test.go` | 26, 55 |
| `internal/route/filter_bench_test.go` | 97-99 (BenchmarkStatusMatch) |

### 3.5 E2E/脚本中 `"status"` JSON 字段引用

| 文件 | 行号 | 内容 |
|------|------|------|
| `scripts/e2e/suites/routing.mjs` | 145, 152, 159, 166, 175, 184, 191, 199, 207, 212 | `status: "success"/"error"/"interrupted"/"info"/"warning"` |
| `scripts/e2e/suites/api_contract.mjs` | 35, 44, 54, 66, 76, 82-88, 93-99, 116, 136, 151, 169, 176, 190-192, 312, 326, 330 | `status: "..."` |
| `scripts/e2e/suites/ws_protocol.mjs` | 155, 173, 206, 217, 228, 247, 251 | `status: "..."`, `notif.status === "success"` |
| `scripts/e2e/suites/security.mjs` | 30, 94, 105, 116, 126, 171 | `status: "success"` |
| `scripts/e2e/suites/edge_cases.mjs` | 35, 63, 82, 92, 109, 112, 124 | `status: "success"` |
| `scripts/e2e/suites/persistence.mjs` | 37, 97 | `status: "success"` |
| `scripts/e2e/suites/push_e2e.mjs` | 120 | `status: "success"` |
| `scripts/e2e/suites/frontend.mjs` | 206 | `status: "success"` |
| `scripts/ws_test.mjs` | 89 | `status: "success"` |
| `README.md` | 164 | `--data '{"status":"success"...}'` |
| `IOS_TESTING.md` | 42 | `--data '{"status":"success"...}'` |
| `web-src/pages/docs.html` | 181 | `"status":"success"` in curl example |
| `web-src/pages/docs.html` | 601, 603 | `statusMatch` text in pre block |

### 3.6 前端 `STATUS_META` 对象引用

| 文件 | 行号 | 内容 |
|------|------|------|
| `web-src/pages/index.html` | 567-592 | STATUS_META 定义（5 个旧 status 值→样式映射） |
| `web-src/pages/index.html` | 364 | `status: n.status \|\| n.Status \|\| "info"` |
| `web-src/pages/index.html` | 700, 715, 727, 908, 932-933 | filter/status 渲染逻辑 |
| `web-src/pages/message.html` | 77-81 | STATUS_META 定义 |
| `web-src/pages/message.html` | 156, 190, 207 | status 渲染 |
| `web-src/pages/admin.html` | 261-272 | statusBadgeEl 函数（map success→status-success 等） |
| `web-src/pages/admin.html` | 629 | `statusBadgeEl(m.status)` |

### 3.7 前端 `status-filter` 相关 CSS 类

| 文件 | 行号 | 类 |
|------|------|----|
| `web/ui.css` | 186-195 | `.status-success`, `.status-error`, `.status-warn` |
| `web/ui.css` | 176-185 | `.status-badge` |
| `web/ui.css` | 198-205 | `.status-on`, `.status-off`（这些是设备开关状态，不涉及消息 status，可保留） |

### 3.8 i18n status 相关 key

| 文件 | 行号 | key |
|------|------|-----|
| `web-src/locales/zh-CN.yaml` | 58-63 | `common.status.success/error/interrupted/info/warning` |
| `web-src/locales/zh-CN.yaml` | 154-157 | `index.recent.filter_all/filter_success/filter_error/filter_interrupted` |
| `web-src/locales/zh-CN.yaml` | 168 | `index.test_notify.status_label` |
| `web-src/locales/zh-CN.yaml` | 336-339 | `receivers.filter.all/error/success` |
| `web-src/locales/zh-CN.yaml` | 421 | `common.field.status` |
| `web-src/locales/zh-CN.yaml` | 645 | `docs.notify.params.status_desc` |
| 同目录 `en.yaml`, `es.yaml`, `ja.yaml` | 对应位置 | 同上（4 个 locale 文件全改） |

---

## 4. 测试断言改动清单

### 4.1 Go 单元测试

| 文件 | 行号 | 改前断言 | 要改成 |
|------|------|----------|--------|
| `internal/api/notify_test.go` | 126 | `msg.Status != broker.StatusSuccess` | `msg.AgentState != broker.AgentStateDone` |
| `internal/api/notify_test.go` | 338-339 | `m.Status != broker.StatusInfo` / `broker.StatusInfo` | `m.AgentState != broker.AgentStateDone`（测试通知默认改 done） |
| `internal/api/notify_test.go` | 各处 body | `"status":"success"` | `"agentState":"done"` |
| `internal/store/messages_test.go` | 24, 47 | `Status: "success"` / `got.Status != "success"` | `AgentState: "done"` / `got.AgentState != "done"` |
| `internal/store/admin_test.go` | 134-135, 171-174, 210 | `Status: "success"/"error"` | `AgentState: "done"/"error"` |
| `internal/store/devices_update_test.go` | 21, 31, 45-46 | `StatusFilter: "all"/"error"` | `EventScope: "final"/"all"`（注意：旧 "error" filter 无直接映射到新 "final"，语义不同，需改测试逻辑） |
| `internal/push/filter_test.go` | 全文 | `TestStatusMatch` 测 `StatusMatch` | **重写为 `TestScopeMatch`**：测 final/all 两档 + terminal 三值。旧 error/success filter 语义不存在了 |
| `internal/push/filter_test.go` | 88-124 | `TestShouldDeliver` 用 `StatusFilter`/`Status` | 改为 `EventScope`/`AgentState`，测试逻辑重写（final 放行终态、all 全放） |
| `internal/push/filter_test.go` | 118-121 | `FilterDevices` 测试数据 `StatusFilter: "all"/"error"` | 改为 `EventScope: "all"/"final"` |
| `internal/push/dispatcher_test.go` | 62 | `StatusFilter: filter` | `EventScope: filter` |
| `internal/push/dispatcher_test.go` | 101, 106 | `Status: broker.StatusSuccess` / `InsertTestMessage(..., broker.StatusSuccess)` | `AgentState: broker.AgentStateDone` / `InsertTestMessage(..., broker.AgentStateDone)` |
| `internal/push/dispatcher_test.go` | 173, 222, 225 | `Status: broker.StatusInfo/Success` | `AgentState: broker.AgentStateWorking/Done` |
| `internal/broker/sqlite_test.go` | 269 | `m.Status = StatusError` | `m.AgentState = AgentStateError` |
| `internal/broker/sqlite_test.go` | 284 | `g.Status != StatusError` | `g.AgentState != AgentStateError` |
| `internal/ws/handler_test.go` | 177, 201 | `Status: broker.StatusSuccess/Info` | `AgentState: broker.AgentStateDone/Working` |
| `internal/ws/handler_test.go` | 293, 295 | `Status: "info"` | `AgentState: "working"` |
| `internal/route/filter_bench_test.go` | 18, 27, 46, 55, 81, 82, 97-99 | `statusFilter`/`Status`/`StatusMatch` | `eventScope`/`AgentState`/`ScopeMatch` |
| `internal/api/notify_bench_test.go` | 45, 60 | `StatusFilter: "all"` / `"status":"success"` | `EventScope: "all"` / `"agentState":"done"` |
| `internal/store/store_bench_test.go` | 34 | `StatusFilter: "all"` | `EventScope: "all"` |
| `internal/push/dispatcher_bench_test.go` | 40, 46 | `Status: broker.StatusSuccess` / `InsertTestMessage(..., broker.StatusSuccess)` | `AgentState: broker.AgentStateDone` / `InsertTestMessage(..., broker.AgentStateDone)` |

### 4.2 E2E 套件

| 套件 | 行号 | 改前 | 要改成 |
|------|------|------|--------|
| `routing.mjs` | 2, 5, 9, 13-18 | 注释中 statusMatch/statusFilter | scopeMatch/eventScope |
| `routing.mjs` | 80, 85, 89, 96, 103 | `statusFilter: "all"/"error"` | `eventScope: "all"/"final"` |
| `routing.mjs` | 116, 122, 125, 128, 134 | 断言 `dev.statusFilter` | `dev.eventScope` |
| `routing.mjs` | 145, 152, 159, 166, 175, 184, 191, 199, 207, 212 | `status: "success"/"error"/"interrupted"/"info"/"warning"` | `agentState: "done"/"error"/"interrupted"/"working"/"blocked"` |
| `routing.mjs` | 142-152 | case1: success 广播，B filter=error 过滤 | case1: done 广播，B scope=final 放行（done 是终态！语义变了，需重写测试逻辑） |
| `routing.mjs` | 204-212 | case8b/8c: info/warning → 仅 all 通过 | working/blocked → final 不过（非终态），all 通过 |
| `api_contract.mjs` | 35, 44, 54, 66, 76, 93-99 | `status: "success"/bogus` | `agentState: "done"/bogus` |
| `api_contract.mjs` | 82-88 | 坏 status → 400 | 坏 agentState → 400 |
| `api_contract.mjs` | 116, 136, 151, 169, 176 | `status: "success"/"info"` | `agentState: "done"/"working"` |
| `api_contract.mjs` | 190-192 | test-notify 坏 status → 400 | 坏 agentState → 400 |
| `api_contract.mjs` | 232, 237, 243, 248 | PATCH `statusFilter: "error"/"bogus"` | `eventScope: "final"/"bogus"` |
| `api_contract.mjs` | 312, 326, 330 | `status: "success"/"error"` | `agentState: "done"/"error"` |
| `ws_protocol.mjs` | 155, 173 | `status: "success"` / `notif.status === "success"` | `agentState: "done"` / `notif.agentState === "done"` |
| `ws_protocol.mjs` | 206, 217, 228, 247, 251 | `status: "info"/"warning"/"success"/"error"` | `agentState: "working"/"blocked"/"done"/"error"` |
| `security.mjs` | 30, 94, 105, 116, 126, 171 | `status: "success"` | `agentState: "done"` |
| `edge_cases.mjs` | 35, 63, 82, 92, 109, 112, 124 | `status: "success"` | `agentState: "done"` |
| `persistence.mjs` | 37, 97 | `status: "success"` | `agentState: "done"` |
| `push_e2e.mjs` | 120 | `status: "success"` | `agentState: "done"` |
| `frontend.mjs` | 206 | `status: "success"` | `agentState: "done"` |
| `ws_test.mjs` | 89 | `status: "success"` | `agentState: "done"` |

**注意**：`routing.mjs` 的测试矩阵需要重大重写。当前用例依赖旧 status filter 的三值（all/error/success）语义，改后只有两值（final/all），且 `final` 放行 done+interrupted+error 三种终态——旧 `error` filter 只放行 error，旧 `success` filter 只放行 success。测试逻辑不能简单替换字段名，需要重新设计用例。

---

## 5. 前端改动清单

### 5.1 `web-src/pages/receivers.html`

| 行号 | 改前 | 要改成 |
|------|------|--------|
| 313-321 | `FILTER_LABEL = { all: 全部通知, error: 仅失败, success: 仅成功 }` | `SCOPE_LABEL = { final: 只收最终结果, all: 接收全流程 }` |
| 336, 346 | demo device `statusFilter: "all"` | `eventScope: "all"` |
| 356 | demo device `statusFilter: "error"` | `eventScope: "final"` |
| 372 | `d.statusFilter \|\| d.status_filter \|\| "all"` | `d.eventScope \|\| "final"` |
| 648 | `updateDevice(d, { statusFilter: e.target.value })` | `updateDevice(d, { eventScope: e.target.value })` |
| 655 | `d.statusFilter === val` | `d.eventScope === val` |
| 938 | `statusFilter: "all"` (demo push) | `eventScope: "final"`（新设备默认 final） |

### 5.2 `web-src/pages/index.html`

| 行号 | 改前 | 要改成 |
|------|------|--------|
| 189-195 | `<select id="tn-status">` 5 个 option (info/success/error/warning/interrupted) | 改为 `id="tn-agent-state"`，5 个 option: working/blocked/done/interrupted/error |
| 364 | `status: n.status \|\| n.Status \|\| "info"` | `agentState: n.agentState \|\| n.AgentState \|\| "working"` |
| 567-592 | `STATUS_META` 5 个 key (success/error/interrupted/info/warning) | 改为 `AGENT_STATE_META` 5 个 key (working/blocked/done/interrupted/error)，加 severity 着色逻辑 |
| 700 | `n.status === "success"` | `n.agentState === "done"` |
| 715 | `n.status === filter` | `n.agentState === filter`（或改用终态/非终态过滤） |
| 727, 908 | `STATUS_META[n.status]` | `AGENT_STATE_META[n.agentState]` |
| 842 | `status: document.getElementById("tn-status").value` | `agentState: document.getElementById("tn-agent-state").value` |
| 932-933 | `t("common.field.status")` + `n.status` | `t("common.field.agent_state")` + `n.agentState` |

### 5.3 `web-src/pages/message.html`

| 行号 | 改前 | 要改成 |
|------|------|--------|
| 77-81 | `STATUS_META` (success/error/interrupted/info/warning) | 改为 agentState 5 值 + severity 着色 |
| 156 | `STATUS_META[m.status]` | `AGENT_STATE_META[m.agentState]` |
| 190 | `t("common.field.status")` + `m.status` | `t("common.field.agent_state")` + `m.agentState` |

### 5.4 `web-src/pages/admin.html`

| 行号 | 改前 | 要改成 |
|------|------|--------|
| 147 | `{{t "admin.messages.status"}}` | `{{t "admin.messages.agent_state"}}` |
| 261-272 | `statusBadgeEl(status)` map (success/error/interrupted/info/warning) | 改为 agentState map (working/blocked/done/interrupted/error) |
| 629 | `statusBadgeEl(m.status)` | `statusBadgeEl(m.agentState)` |

### 5.5 `web-src/pages/docs.html`

| 行号 | 改前 | 要改成 |
|------|------|--------|
| 181 | `"status":"success"` in curl example | `"agentState":"done"` |
| 601 | `deliver ⟺ device.enabled AND statusMatch AND tagMatch` | `deliver ⟺ device.enabled AND scopeMatch AND tagMatch` |
| 603 | `statusMatch: device filter all=... error=... success=...` | `scopeMatch: device eventScope all=catch-all · final=terminal-only` |

### 5.6 `web-src/locales/*.yaml`（4 文件：zh-CN / en / es / ja）

需要修改/新增的 key（每个 locale 文件都要改）：

| 操作 | key | 内容（以 zh-CN 为例） |
|------|-----|----------------------|
| **改** | `common.status.success` → `common.agentState.done` | 完成 |
| **改** | `common.status.error` → `common.agentState.error` | 失败 |
| **改** | `common.status.interrupted` → `common.agentState.interrupted` | 中断 |
| **改** | `common.status.info` → `common.agentState.working` | 运行中 |
| **改** | `common.status.warning` → `common.agentState.blocked` | 等待中 |
| **加** | `common.severity.info` | 信息 |
| **加** | `common.severity.warning` | 警告 |
| **加** | `common.severity.error` | 错误 |
| **改** | `receivers.filter.all/error/success` → `receivers.scope.all/final` | 全流程 / 只收最终结果 |
| **改** | `common.field.status` → `common.field.agent_state` | 运行状态 |
| **加** | `common.field.severity` | 语气 |
| **改** | `index.recent.filter_all/success/error/interrupted` | 改为 agentState 对应值 |
| **改** | `docs.notify.params.status_desc` → `agent_state_desc` | 新枚举描述 |
| **改** | `admin.messages.status` → `admin.messages.agent_state` | 运行状态 |

### 5.7 `web/ui.css`

| 行号 | 类 | 改动 |
|------|----|------|
| 186 | `.status-success` | 可保留（改为 `.state-done` 或保留但改语义引用） |
| 190 | `.status-error` | 可保留（改为 `.state-error`） |
| 194 | `.status-warn` | 可保留（改为 `.state-warn`） |
| 176-185 | `.status-badge` | 保留（通用 badge 样式） |
| 198-205 | `.status-on` / `.status-off` | 保留（设备开关状态，不涉及消息 agentState） |

**建议**：CSS 类名不强制改（`.status-success` 等是通用样式名，不含语义），前端 JS 中改映射表即可。若要彻底，可加 `.state-working`（蓝）、`.state-blocked`（黄）、`.state-done`（绿）、`.state-error`（红）、`.state-interrupted`（灰）。

---

## 6. 跨仓库（插件）提示

> **以下改动在 `anotify-plugins` 仓库，需协调者走 cross-repo skill 单独同步。**
> 本仓库的 `make e2e` 不覆盖插件代码。

### 6.1 `anotify-plugins/common/notify.sh`
- `--status` 命令行参数 → `--agent-state`
- 加 `--severity` 可选参数
- 内部发送逻辑：将 `status` JSON 字段改为 `agentState`

### 6.2 `anotify-plugins/pi/anotify.ts`
事件映射改动（设计文档 §7.8）：

| pi 事件 | 旧映射 | 新映射 |
|---------|--------|--------|
| `message_end(role=user)` | （无对应 / info） | `agentState=working` |
| `tool_execution_end(isError=true)` | `status=error` | `agentState=working, severity=error` |
| `tool_execution_end(isError=false)` | `status=info` | `agentState=working` |
| `agent_settled` | `status=success` | `agentState=done` |
| 中断/取消 | `status=interrupted` | `agentState=interrupted` |
| 任务失败 | `status=error` | `agentState=error`（终态失败） |

**关键区分**：旧模型 `status=error` 混用，新模型拆为：
- 工具瞬时失败（agent 还在跑）→ `agentState=working, severity=error`
- 任务级终态失败 → `agentState=error`

---

## 7. 陷阱提示

### 7.1 迁移幂等性
- `schema.sql` 直接改列名（`status`→`agent_state`，`status_filter`→`event_scope`），但 `CREATE TABLE IF NOT EXISTS` 不改已存在的表。
- `store.Open` → `migrateColumns` 只做 `ADD COLUMN`（try/ignore 幂等），不做 `RENAME COLUMN`。
- **新库**（`:memory:` 或首次创建）：schema.sql 生效，有新列名，无问题。
- **老库**（已有 `status`/`status_filter` 列）：schema.sql 的 `CREATE TABLE IF NOT EXISTS` 不改表，`migrateColumns` 会 ADD 新列但旧列仍存在（不引用，无害）。
- **风险**：如果 `migrateColumns` 尝试 `ADD COLUMN agent_state` 而表已有 `status` 列但没有 `agent_state`，ADD 成功；但如果表已有 `agent_state`（二次迁移），ADD 报错被 `_` 忽略——OK。
- **结论**：当前方案安全，但 `INSERT`/`SELECT` SQL 必须用新列名，否则老库里新列有默认值但代码读旧列会失败。

### 7.2 Import 循环风险
- `internal/store` 不 import `internal/broker`（设计文档 §6 提到）。`MessageRow` 是 store 本地类型。
- 如果 `route.ScopeMatch` 需要 `broker.IsTerminal`，`route` 已经 import `broker`（filter.go 第 8 行），无循环风险。
- `store` 包的 `InsertTestMessage` 签名从 `status string` 改为 `agentState string`——调用方（broker/sqlite_test.go、push/dispatcher_test.go）传 broker 常量，无 import 循环。

### 7.3 旧 status filter "error" 语义无映射
- 旧 `statusFilter=error` 表示"只收 error 消息"。
- 新 `eventScope=final` 表示"只收终态"（done+interrupted+error）。
- **语义不同**：旧 error filter 只收 error，新 final 收三种终态。无法 1:1 映射。
- 测试需重写逻辑（不能简单替换字段名）。

### 7.4 默认值变更
- 旧 push 设备默认 `status_filter='all'`（全收）。
- 新 push 设备默认 `event_scope='final'`（只收终态）——这是产品核心诉求。
- `handlers.go` 的 `deviceUpsert` 必须把默认值从 `"all"` 改为 `"final"`。
- `schema.sql` 的 `devices` 表 default 也要改为 `'final'`。
- **风险**：如果遗漏某处默认值，新注册的 push 设备会收不到 working/blocked 中间态通知。

### 7.5 WS subscribe 帧字段命名
- 当前 WS 帧用 snake_case JSON tag（`subscribed_tags`, `event_id`, `sent_at`, `ttl_sec`）。
- 新 `event_scope` 字段应遵循 snake_case（不是 camelCase），与现有帧风格一致。
- 但 Device struct 的 JSON tag 用 camelCase（`eventScope`）。两个通道命名风格不同，不要混淆。

### 7.6 severity 派生位置
- 设计文档说"severity 默认值后端填充"。派生逻辑应放在 `notify.go` 的 `ServeHTTP` 中构造 `broker.Message` 之前，而非 broker 层。
- 理由：broker 是通用消息层，不应知道业务派生规则；API 层是协议边界，适合做这种归一化。
- `TestNotifyRequest` 的 `ServeTestNotify` 也要调同一个 `deriveSeverity` helper。

### 7.7 pushPayload 是否要加 agentState
- 当前 `pushPayload` 只发 `id/title/body/tag/url/link`。
- Service Worker（`sw.js`）渲染通知时如果需要按状态着色/选图标，需要在 payload 里加 `agentState` + `severity`。
- 设计文档未明确要求，但前端改造时可能需要。**建议**：worker 改 `dispatcher.go` 时一并加，避免 sw.js 拿不到状态。

### 7.8 AdminGlobalMessage 的 Status 字段
- `AdminGlobalMessage.Status` 用于 admin 后台全局消息列表展示（admin.html:629 `statusBadgeEl(m.status)`）。
- 改为 `AgentState` 后，admin.html 的 `statusBadgeEl` map 也要同步改。
- `ListGlobalMessages` 的 SQL `SELECT m.status` → `SELECT m.agent_state`。

### 7.9 前端 INDEX.HTML 的 filter 逻辑
- 当前 index.html 的通知列表 filter（line 715）用 `n.status === filter`，filter 值为 `all/success/error/interrupted`。
- 改后 agentState 有 5 值，filter UI 需重新设计。建议改为：`all`(全部) / `terminal`(终态) / `working`(进行中) 三档，或保留全 5 值过滤。

### 7.10 `store/admin_test.go` 的 Status 字面量
- 多处直接用 `Status: "success"` / `Status: "error"` 字面量（非 broker 常量）。
- 改为 `AgentState: "done"` / `AgentState: "error"`。需注意 `"success"` → `"done"` 不是简单的字段名替换，值也要改。

---

## 附：文件改动影响图

```
api/openapi.yaml ─┐
                  ├─→ 协议契约（事实源）
                  │
broker/broker.go ─┼─→ Message struct + 常量（被所有人引用）
broker/sqlite.go ─┘    INSERT/SELECT/scanMessage
                  │
store/schema.sql ─┐
store/store.go ──┤─→ DB schema + 迁移
store/messages.go ┤    MessageRow + SQL
store/devices.go ─┤    Device struct + SQL
store/admin.go ───┘    AdminGlobalMessage + SQL
                  │
route/filter.go ─────→ ScopeMatch (替 StatusMatch) + ShouldDeliver
                  │
api/notify.go ────────→ NotifyRequest/TestNotifyRequest + validAgentStates + deriveSeverity
server/handlers.go ───→ devicePatchReq + deviceUpsert 默认 + messageView
                  │
ws/protocol.go ───────→ Frame + notificationFrame
ws/handler.go ────────→ session.eventScope + matchScope
                  │
push/dispatcher.go ───→ pushPayload (可选加 agentState)
                  │
前端 (6 页面 + 4 locale + 1 CSS) ─→ STATUS_META→AGENT_STATE_META + filter UI
                  │
测试 (Go unit + E2E 7 套件) ─────→ 断言全改
                  │
跨仓库 (anotify-plugins) ───────→ notify.sh + anotify.ts
```
