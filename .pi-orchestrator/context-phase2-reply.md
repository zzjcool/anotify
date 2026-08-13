# 阶段二「双向回复」侦察上下文 · context-phase2-reply.md

> 产出者：scout  
> 依据：`receiver-capability-design.md`（阶段一设计，阶段二地基）+ 阶段一代码现状（已合并，逐文件读码核实）  
> 注意：`requirements-phase2-reply.md` 在仓库中不存在（`.pi-orchestrator/` 下无此文件），本侦察基于设计文档 §4.3/§7.7 明确预留的 `notify:reply` scope + `kind=reply`/`replyTo` 字段，以及任务描述中列出的改动点。

---

## 1. 改动点总览表

| # | 文件 | 改什么 | 优先级 |
|---|------|--------|--------|
| B1 | `internal/authn/authn.go` | 新增 `ScopeNotifyReply = "notify:reply"` 常量 | P0 |
| B2 | `internal/auth/apikey.go` | `validScope()` 加 `notify:reply`；`scopeLabel()` 加 reply 分支 | P0 |
| B3 | `internal/api/notify.go` | 新增 `ReplyHandler` + `ReplyRequest`，参照 `ServeTestNotify`（Cookie 鉴权）但反查原消息 | P0 |
| B4 | `internal/server/mux.go` | 注册 `POST /v1/reply` 路由（`sessMW` 包裹） | P0 |
| B5 | `internal/store/messages.go` | `GetMessage` **已含 payload**（含 agentId/sessionId），无需改 | — 确认 |
| B6 | `internal/broker/broker.go` + `sqlite.go` | Message 已有 `Kind`/`ReplyTo` 字段；Publish 已支持，无需改 | — 确认 |
| B7 | `internal/ws/protocol.go` + `handler.go` | notification 帧已带 `kind`；WS 透传 reply 消息无需改 | — 确认 |
| B8 | `internal/server/ratelimit.go` | 新增 reply 限速器（按 userID，复用 `fixedWindow`） | P1 |
| F1 | `web-src/pages/message.html` | 加回复输入框 + 提交按钮 + 调 `POST /v1/reply` | P0 |
| F2 | `web-src/pages/index.html` | 通知列表中 `kind=reply` 消息区分渲染（标记/缩进） | P1 |
| F3 | `web-src/locales/*.yaml`（4 文件） | 新增 `reply.*` i18n key | P0 |
| P1 | `anotify-plugins/pi/anotify.ts` | 上报带 `--session-id`；新增 WS 客户端订阅 reply 消息；调 `sendUserMessage` 转发回复 | P0 |
| P2 | `anotify-plugins/common/notify.sh` | `--session-id` 参数已有但 anotify.ts 未用；加 `--kind`/`--reply-to` 参数 | P1 |
| P3 | `anotify-plugins/common/login.sh` | 登录申请 scope 加 `notify:receive`（pi 扩展订阅 WS 要用） | P0 |
| T1 | `scripts/e2e/suites/api_contract.mjs` | 加 reply 端点契约 case | P0 |
| T2 | 新增 `scripts/e2e/suites/reply_e2e.mjs` | reply 端到端套件 | P0 |

---

## 2. 逐文件清单

### B1. `internal/authn/authn.go`（行 14-17）

**现状**：scope 常量只有三个：
```go
const (
    ScopeNotifySend    = "notify:send"      // 行 15
    ScopeNotifyReceive = "notify:receive"   // 行 16
    ScopeDevicesRead   = "devices:read"     // 行 17
)
```

**要改成**：加 `ScopeNotifyReply = "notify:reply"`。这是协议层常量，`Authenticate()` 函数（行 83-98）接收 `wantScope` 参数，reply 端点调用时传 `authn.ScopeNotifyReply` 即可。

注意：`HasScope()` 函数（行 61-68）是通用检查器，已支持任意 scope 字符串，无需改。

### B2. `internal/auth/apikey.go`（行 19-24, 44-52）

**现状**：
- scope 常量定义在行 19-24（与 authn 重复定义，两套常量值相同）
- `validScope()` 行 44-52 只认三个 scope：
```go
func validScope(s string) bool {
    switch s {
    case ScopeNotifySend, ScopeNotifyReceive, ScopeDevicesRead:
        return true
    default:
        return false
    }
}
```
- `scopeLabel()` 行 30-42 根据 send/recv 组合生成 Key 前缀（`ant_full_`/`ant_send_`/`ant_recv_`/`ant_key_`）

**要改成**：
1. `validScope()` 加 `ScopeNotifyReply` case
2. `scopeLabel()` 可选加 reply 分支（如 `ant_reply_` 或并入 `full`）
3. 此文件常量（行 19-24）与 `authn` 包常量重复——注意两处都要加

### B3. `internal/api/notify.go`（行 1-252）

**现状**：
- `NotifyHandler`（行 21-29）处理 `POST /v1/notify`，Bearer Key 鉴权（`authn.ScopeNotifySend`）
- `ServeTestNotify`（行 163-252）处理 `POST /v1/test-notify`，Cookie 会话鉴权（`auth.UserIDFromContext`）
- `NotifyRequest`（行 31-47）已含 `Kind`/`ReplyTo` 字段
- 消息构造流程：`json.Marshal(req)` → payload → `broker.Message{...}` → `Broker.Publish()`

**要改成**：新增 `ReplyHandler`（或 `ReplyRequest`），参照 `ServeTestNotify` 的 Cookie 鉴权模式：
```go
// 伪代码
type ReplyRequest struct {
    ReplyTo string `json:"replyTo"`  // 目标消息 ID（required）
    Body    string `json:"body"`     // 回复正文（required）
}

func (h *NotifyHandler) ServeReply(w http.ResponseWriter, r *http.Request) {
    // 1. Cookie 鉴权 → userID
    user := auth.UserIDFromContext(r.Context())
    // 2. 解析 ReplyRequest
    // 3. GetMessage(userID, req.ReplyTo) → 反查原消息
    //    原消息不存在 → 404
    //    原消息 payload 含 agentId/sessionId（已确认 GetMessage 返回 payload）
    // 4. 构造 reply 消息: Kind="reply", ReplyTo=req.ReplyTo, AgentState="working"
    //    标题/正文从用户输入取
    // 5. Broker.Publish() → 消息进入流，WS/push 消费者收到
}
```

**关键设计点**：
- 回复消息 `agentState` 建议设为 `working`（用户发了回复 = agent 要开始处理）或 `done`（取决于语义）
- 回复消息 `deviceTags` 应继承原消息的 deviceTags（让原任务的路由标签一致）
- 回复消息的 `payload` 应包含原消息的 `agentId`/`sessionId`（从 GetMessage 返回的 payload 中解析）

**鉴权方式选择**：reply 端点用 Cookie 会话鉴权（参照 `ServeTestNotify`），因为回复是工作台用户操作，不走 Bearer Key。`sessMW` 中间件已把 userID 注入 context。

### B4. `internal/server/mux.go`（行 99-108）

**现状**：路由注册方式（Go 1.22+ ServeMux 模式匹配）：
```go
mux.Handle("/v1/notify", noStore(notifyH))                              // 行 99
mux.Handle("/v1/test-notify", noStore(sessMW(http.HandlerFunc(notifyH.ServeTestNotify))))  // 行 100
mux.Handle("/v1/stream", noStore(streamH))                              // 行 101
```

**要改成**：在行 100 后加：
```go
mux.Handle("/v1/reply", noStore(sessMW(http.HandlerFunc(notifyH.ServeReply))))
```

注意 `notifyH` 已持有 `Broker`/`Store` 字段（行 95），reply 端点需要的依赖都有了。

### B5. `internal/store/messages.go`（行 78-106）— 已确认，无需改

**现状核实**：`GetMessage` 返回 `*MessageRow`，其中 `Payload []byte` 字段（行 91: `m.Payload = []byte(payload)`）包含完整 JSON（即原始 `NotifyRequest` 的 `json.Marshal(req)`），其中含 `agentId`/`sessionId`/`model`/`cwd` 等字段。

**结论**：reply 反查原消息的 agentId/sessionId **直接可用**——从 `GetMessage` 返回的 `Payload` 中 `json.Unmarshal` 即可取出。

`MessageRow` 结构体（行 14-30）已含 `Kind`/`ReplyTo` 字段，reply 消息存取无障碍。

### B6. `internal/broker/broker.go`（行 44-65）+ `sqlite.go`（行 49-105）— 已确认，无需改

**现状核实**：
- `broker.Message` 结构体（行 46-65）已含 `Kind`/`ReplyTo` 字段（json tag `kind,omitempty` / `replyTo,omitempty`）
- `SQLiteBroker.Publish()`（行 49-105）已把 `Kind`/`ReplyTo` 写入 DB（INSERT 语句行 83-84）
- `scanMessage()`（行 237-258）已扫描 `Kind`/`ReplyTo`
- `Replay()` 已返回完整 Message（含 Kind/ReplyTo）

**结论**：reply 消息的构造与存储完全复用现有 Publish 流程，只需在调用方设置 `Kind="reply"`、`ReplyTo=<目标消息ID>`。

### B7. `internal/ws/protocol.go`（行 46-76）+ `handler.go`（行 1-310）— 已确认，无需改

**现状核实**：
- WS `notification` 帧（`protocol.go` 行 67-76 `notificationFrame()`）已包含 `Kind` 字段
- WS 消息分发（`handler.go` 行 167-185）按 `matchTags` + `matchScope` 过滤，reply 消息的 `agentState` 决定是否被 final/all 设备收到
- WS 订阅无需任何改动——reply 消息走 Broker.Publish → broadcast → WS 客户端自动收到

**结论**：reply 透传到 WS 客户端**零改动**。

### B8. `internal/server/ratelimit.go`（行 1-155）

**现状**：
- `fixedWindow` 限速器（行 33-82）：内存固定窗口，key→count，1 分钟窗口
- 已有限速器实例：`rlCreate`（10/min/IP）、`rlByCode`（20/min/user）、`rlQR`（30/min/IP）、`rlEnrollLookup`/`rlEnrollKnock`/`rlEnrollComplete`（各 10/min/IP）
- `rateLimited(w, rl, key)` 函数（行 146-155）：超限写 429 + Retry-After
- `clientIP(r)` 提取 IP（行 124-137，支持 X-Forwarded-For）

**要改成**：新增 reply 限速器（按 userID 限速，防止刷回复）：
```go
const rlReplyPerMin = 20  // 回复：20/min/user
var rlReply = newFixedWindow(rlReplyPerMin, time.Minute)
```
在 `ServeReply` 中调用 `rateLimited(w, rlReply, userID)`。

### F1. `web-src/pages/message.html`（行 1-232）

**现状**：
- 消息详情页结构：头部（状态点+标题）→ 正文卡片 → 全部字段 → 投递记录
- JS 在 `<script>` 块中（行 52-232），通过 `api()` 函数调 `/v1/notifications/{id}` 获取消息
- `render(m)` 函数（行 117-160）渲染消息详情
- `AGENT_STATE_META` 对象（行 60-66）映射 agentState → 样式

**要改成**：在「正文」section 和「全部字段」section 之间加回复输入区域：
```html
<!-- ===== 回复 ===== -->
<section class="card reveal visible p-5" id="reply-section">
  <div class="text-sm font-medium text-white">{{t "reply.title"}}</div>
  <textarea id="reply-input" rows="2" class="input mt-3" placeholder="{{t "reply.placeholder"}}"></textarea>
  <button id="reply-send" class="btn-primary mt-3 rounded-xl px-4 py-2 text-sm">{{t "reply.send"}}</button>
</section>
```
JS 中加 `sendReply()` 函数调 `POST /v1/reply`，body 为 `{replyTo: msgId, body: text}`。

**注意**：只有 `kind=task` 的消息才显示回复框；`kind=reply` 的消息不显示（避免对回复再回复）。需在 `render(m)` 中检查 `m.kind`。

### F2. `web-src/pages/index.html`（行 1-400+）

**现状**：
- 通知列表 `renderNotifs()` 函数（行 248-290）按 agentState 渲染状态点和 badge
- `normalize()` 函数（行 152-190）未提取 `kind` 字段

**要改成**：
1. `normalize()` 中加 `kind: n.kind || n.Kind || "task"` 提取
2. `renderNotifs()` 中对 `kind=reply` 的消息加视觉区分（如缩进、reply 图标、或 badge 标记"回复"）

### F3. `web-src/locales/*.yaml`（zh-CN / en / ja / es）

**现状**：无任何 reply 相关键（grep 确认）。

**要改成**：在 `common` 或顶层加：
```yaml
reply:
  title: "回复"
  placeholder: "输入回复内容…"
  send: "发送回复"
  sending: "发送中…"
  success: "回复已发送"
  failed: "回复失败：{msg}"
  badge: "回复"
```
四个 locale 文件都要加（zh-CN/en/ja/es）。

---

## 3. 关键现状核实

### 3.1 store GetMessage 是否含 payload — ✅ 已含

`GetMessage`（`internal/store/messages.go:78-106`）返回 `*MessageRow`，其中 `Payload []byte`（行 91）包含完整 JSON——即 `NotifyRequest` 的 `json.Marshal(req)` 结果，含 `agentId`/`sessionId`/`model`/`cwd` 等全部字段。

reply 端点反查原消息后，`json.Unmarshal(row.Payload, &originalReq)` 即可取出 agentId/sessionId，用于构造 reply 消息的 payload。

### 3.2 authn scope 机制

两层 scope 定义：
1. **`internal/authn/authn.go`（行 14-17）**：协议层常量 + `Authenticate(r, v, wantScope)` 函数（行 83-98），用于 Bearer Key 鉴权
2. **`internal/auth/apikey.go`（行 19-24）**：持久层常量 + `validScope()` 校验（行 44-52）+ `CreateKey()` 签发

两处常量值相同但独立定义。加 `notify:reply` 要两处都加。

**reply 端点鉴权方式**：不用 Bearer Key，用 Cookie 会话鉴权（`sessMW` 中间件 → `auth.UserIDFromContext`）。工作台用户天然有全部权限，不需要 scope 检查。`notify:reply` scope 是为将来 CLI/API Key 调用 reply 端点预留的。

### 3.3 限流机制

`internal/server/ratelimit.go`：
- `fixedWindow` 结构（行 33-82）：内存固定窗口限速器
- 已有 6 个限速器实例，按 IP 或 userID 限速
- `rateLimited(w, rl, key)` 函数（行 146-155）：超限写 429

reply 限速建议按 userID（`auth.UserIDFromContext` 取），20/min/user，复用 `fixedWindow` + `rateLimited`。

### 3.4 WS 帧字段

`internal/ws/protocol.go` 的 `Frame` 结构（行 46-76）：
- notification 帧已含 `Kind` 字段（行 59: `Kind string json:"kind,omitempty"`）
- `notificationFrame()` 函数（行 67-76）已把 `msg.Kind` 传入帧

**结论**：WS 客户端收到的 notification 帧已带 `kind` 字段，前端可据此区分 reply/task 消息。reply 透传到 WS 零改动。

### 3.5 pi 扩展 WS 客户端缺口 — 完全没有

**现状**：`anotify-plugins/pi/anotify.ts` 只做**上报**（调 `notify.sh` → `POST /v1/notify`），完全没有 WS 客户端代码。没有 import 任何 WebSocket 库，没有连接 `/v1/stream` 的逻辑。

**要新增**：
1. WS 客户端连接 `/v1/stream`（Bearer Key，scope=`notify:receive`）
2. 收到 `kind=reply` 的 notification 帧后，调用 `ctx.sendUserMessage(body)` 转发给 agent
3. WS 连接管理（重连、replay 续传）

**pi 扩展 API 能力**（从 `@earendil-works/pi-coding-agent` 类型定义确认）：
- `ExtensionContext.sessionManager.getSessionId()` — 可获取当前 session ID（`ReadonlySessionManager` 类型包含 `getSessionId`）
- `ExtensionCommandContext.sendUserMessage(content, options?)` — 可向 agent 发用户消息（`deliverAs: "steer" | "followUp"`）
- 但事件处理器收到的是 `ExtensionContext`（非 `ExtensionCommandContext`），`sendUserMessage` 在 `ExtensionActions` 上，需确认事件 handler 能否调到

**关键问题**：`ExtensionHandler<E>` 签名是 `(event: E, ctx: ExtensionContext) => ...`（types.d.ts），而 `sendUserMessage` 在 `ExtensionActions` / `ExtensionCommandContext` 上。事件 handler 的 `ctx` 是 `ExtensionContext`，不含 `sendUserMessage`。需要确认 pi 扩展是否提供了从事件 handler 调用 `sendUserMessage` 的途径（可能需要通过 `pi` API 注册 command 而非在事件中直接调）。

**凭证问题**：`credentials.json` 结构（`login.sh:170`）为：
```json
{"server":"https://...","apiKey":"ant_send_...","deviceName":"...","createdAt":...}
```
只含一个 `apiKey`（scope=`notify:send`）。pi 扩展订阅 WS 需要 `notify:receive` scope 的 Key——当前 `login.sh` 只申请 `notify:send`（行 133: `{"deviceName":"...","scopes":["notify:send"]}`）。

**解决方案**：
- 方案 A：`login.sh` 改为申请 `["notify:send","notify:receive"]`（一个 Key 同时有 send+receive scope）
- 方案 B：pi 扩展用 Cookie 会话鉴权连 WS（但 pi 没有 Cookie，不现实）
- **推荐方案 A**：`login.sh` 改 scopes 为 `["notify:send","notify:receive"]`，一个 Key 全能

---

## 4. 跨仓库（anotify-plugins）改动清单

### P1. `anotify-plugins/pi/anotify.ts`（全文件，335 行）

**现状结构**：
- `loadConfig(pi)` — 读 `anotify.*` 设置
- `notify(config, args)` — 调 `notify.sh` 上报
- `deriveSummary(ctx)` — 从 session entries 推导标题/正文
- `agentId()` — `pi@${HOSTNAME}`
- 导出函数注册三个事件：`message_end`（开始）、`tool_execution_end`（失败）、`agent_settled`（完成）

**改动点**：

1. **上报带 sessionId**（行 240-258 等所有 `notify()` 调用）：
   - `ctx.sessionManager.getSessionId()` 可用（类型确认）
   - 所有 `notify()` 调用加 `"--session-id", ctx.sessionManager.getSessionId()`
   - 当前所有上报都**没有**传 `--session-id`（grep 确认）

2. **新增 WS 客户端**（全新代码块）：
   ```typescript
   // 在导出函数中启动 WS 连接
   function startWSClient(config: AnotifyConfig, pi: ExtensionAPI): void {
       // 1. 读 credentials.json 拿 server + apiKey
       // 2. 连 ws://server/v1/stream (Bearer apiKey)
       // 3. 收 notification 帧 → 如果 kind=reply → 调 sendUserMessage
       // 4. 重连/replay 逻辑
   }
   ```
   - Node.js 22 有原生 `WebSocket`（全局），不需要额外依赖
   - 但 pi 扩展可能运行在非 Node 环境——需确认（从代码看用了 `node:child_process`/`node:fs`，确定是 Node 环境）

3. **WS → agent 消息转发**：
   - 收到 `kind=reply` 的 notification 帧
   - 需要调 `sendUserMessage(frame.body)` 转发给 agent
   - **阻塞问题**：事件 handler 的 `ctx` 是 `ExtensionContext`，不含 `sendUserMessage`。需要找到从 WS 回调调 `sendUserMessage` 的途径。
   - 可能方案：在 `session_start` 事件中保存可用的 command context，或通过 `pi` API 注册一个内部 command

4. **配置项扩展**：
   ```typescript
   interface AnotifyConfig {
       // ... 现有字段
       wsEnabled: boolean;      // 是否启用 WS 订阅（默认 true）
       wsReconnectMs: number;   // 重连间隔（默认 5000）
   }
   ```

### P2. `anotify-plugins/common/notify.sh`（行 1-180）

**现状**：
- `--session-id` 参数已解析（行 83: `--session-id) SESSION_ID="$2"; shift 2 ;;`）
- 但如果 `SESSION_ID` 非空，会加入 JSON body（行 113: `[ -n "$SESSION_ID" ] && OPTS="$OPTS,\"sessionId\":$(json_escape "$SESSION_ID")"`)
- **anotify.ts 没有传 `--session-id`**，所以实际上报的 payload 不含 sessionId

**要改成**：
- 加 `--kind` 参数（当前没有）：`--kind) KIND="$2"; shift 2 ;;`，加入 JSON body `"kind":...`
- 加 `--reply-to` 参数（当前没有）：`--reply-to) REPLY_TO="$2"; shift 2 ;;`，加入 JSON body `"replyTo":...`
- 这些参数主要供 reply 上报场景使用（如果 reply 也走 notify.sh）

### P3. `anotify-plugins/common/login.sh`（行 133）

**现状**：
```sh
REQ_BODY="{\"deviceName\":\"$DEVICE_NAME\",\"scopes\":[\"notify:send\"]}"
```
只申请 `notify:send` scope。

**要改成**：
```sh
REQ_BODY="{\"deviceName\":\"$DEVICE_NAME\",\"scopes\":[\"notify:send\",\"notify:receive\"]}"
```

这样 pi 扩展用同一个 Key 既能上报（send）又能订阅 WS（receive）。

**注意**：`credentials.json` 落盘时只存 `apiKey`（行 170），不存 scope。pi 扩展读 `apiKey` 后直接用于 WS Bearer 鉴权即可，不需要知道 scope。

---

## 5. 测试改动清单

### T1. `scripts/e2e/suites/api_contract.mjs`

**现状**：覆盖 notify 鉴权矩阵（401/403/400/200）、devices CRUD、keys CRUD、notifications 列表。

**要加**：
- `POST /v1/reply` 无 session → 401
- `POST /v1/reply` 缺 replyTo → 400
- `POST /v1/reply` replyTo 指向不存在的消息 → 404
- `POST /v1/reply` 合法 → 200，返回消息 ID
- reply 后 `GET /v1/notifications` 列表含 reply 消息（kind=reply）

### T2. 新增 `scripts/e2e/suites/reply_e2e.mjs`

**端到端验证**：
1. 上报一条 task 消息 → 拿到 msgId
2. 用 session Cookie 调 `POST /v1/reply`（replyTo=msgId, body="回复内容"）
3. WS 客户端收到 reply 消息帧（kind=reply, replyTo=msgId）
4. `GET /v1/notifications/{replyMsgId}` 验证 payload 含原消息的 agentId/sessionId

**复用 harness**：
- `H.startServer()` — 起服务
- `H.seed(dbPath, username)` — 建用户 + sendKey + recvKey + session
- `H.req()` — HTTP 请求（支持 session/key/body）
- `H.makeDevice()` — 建设备

### T3. `scripts/e2e/suites/security.mjs`

加 scope 越权 case：
- reply 端点目前用 Cookie 鉴权，不涉及 scope 检查（Cookie 会话天然有全权限）
- 如果将来支持 Bearer Key 调 reply，需加 `send scope Key → reply → 403` case

### T4. `scripts/e2e/suites/ws_protocol.mjs`

加 case：
- 上报 task 消息 + reply 消息 → WS 客户端收到两条 notification 帧
- reply 帧的 `kind` 字段 = "reply"，`replyTo` 字段 = 原消息 ID

---

## 6. 陷阱提示

### 6.1 两处 scope 常量不同步
`internal/authn/authn.go` 和 `internal/auth/apikey.go` 各自定义了一套 scope 常量（值相同）。加 `notify:reply` 时**两处都要加**，否则 `CreateKey` 会拒绝带 `notify:reply` scope 的 Key（`validScope()` 返回 false → `validateScopes()` 报错）。

### 6.2 pi 扩展 sendUserMessage 调用途径未明确
`ExtensionHandler` 的事件 handler 收到的 `ctx` 是 `ExtensionContext`，不含 `sendUserMessage`。`sendUserMessage` 在 `ExtensionActions` / `ExtensionCommandContext` 上。WS 回调（非 pi 事件触发的）如何调到 `sendUserMessage` 是**最大未解风险**——需要研究 pi 扩展是否提供了从任意上下文调 action 的 API，或者需要通过注册 command 间接调用。

### 6.3 credentials.json 不含 scope
`login.sh` 落盘的 `credentials.json` 只有 `{server, apiKey, deviceName, createdAt}`，不存 scope。pi 扩展无法从凭证文件判断 Key 是否有 `notify:receive` scope。如果用户用旧版 `login.sh` 登录（只申请了 `notify:send`），WS 连接会被 403 拒绝。需要：
- `login.sh` 改为申请 `["notify:send","notify:receive"]`
- pi 扩展 WS 连接失败时给出明确提示（"Key 缺少 notify:receive scope，请重新登录"）

### 6.4 reply 消息的 agentState 选择
reply 消息应该用什么 `agentState`？
- 如果设为 `working`：final 设备不收（只收终态），但 reply 是用户发的新指令，agent 要开始干活了，working 语义正确
- 如果设为 `done`：所有设备都收，但语义不对（回复不是"完成"）
- **建议**：reply 消息 `agentState=working`，让 final 设备不收中间回复，all 设备收全流程

### 6.5 reply 消息的 deviceTags 继承
reply 消息应继承原消息的 `deviceTags`，确保路由标签一致。如果 reply 不带 deviceTags（空=广播），会投递到所有设备，可能不是期望行为。需要在 `ServeReply` 中从原消息 payload 取出 deviceTags 并设置到 reply 消息上。

### 6.6 replyTo 字段未做外键约束
`messages` 表的 `reply_to` 列（schema.sql:78）是 `TEXT NOT NULL DEFAULT ''`，没有外键约束。reply 消息的 `reply_to` 指向的原消息可能已被 `DeleteExpired` 清理。`ServeReply` 中 `GetMessage` 查不到原消息时应返回 404，不要允许对不存在的消息回复。

### 6.7 anotify.ts 上报全部缺少 sessionId
当前 `anotify.ts` 的三个事件 handler（`message_end`/`tool_execution_end`/`agent_settled`）调 `notify()` 时都没有传 `--session-id`。这意味着所有已上报的消息 payload 中 `sessionId` 字段为空。阶段二要让 reply 消息能关联到原 session，必须先修复这个遗漏——在上报时带上 `ctx.sessionManager.getSessionId()`。

### 6.8 OpenAPI 契约文档未含 reply 端点
`api/openapi.yaml` 目前只有 `/v1/notify` 和 `/v1/test-notify` 的定义，没有 `/v1/reply`。需要新增 reply 端点的 OpenAPI 定义（path + request/response schema）。

### 6.9 pi 扩展 WS 连接生命周期管理
pi 扩展的 WS 连接需要在 session 开始时建立、session 结束时关闭。`session_start` 事件（reason: startup/new/resume/fork）是建立连接的时机；`session_shutdown` 事件是关闭连接的时机。fork/resume 场景需要特别处理（可能需要重连并 replay 补漏）。

### 6.10 class 纪律（前端）
`message.html` 中新增的回复输入框，class 必须用已定义的组件类（`card`/`btn-primary`/`input`）或 Tailwind 工具类。不能自造无定义的类。`make fe` 的 `check-classes.mjs` 守卫会拦住死类。

---

## 附：关键文件行号速查

| 文件 | 关键位置 | 说明 |
|------|---------|------|
| `internal/authn/authn.go:14-17` | scope 常量 | 加 `ScopeNotifyReply` |
| `internal/authn/authn.go:83-98` | `Authenticate()` | reply 端点鉴权入口 |
| `internal/auth/apikey.go:19-24` | scope 常量（第二处） | 同步加 |
| `internal/auth/apikey.go:44-52` | `validScope()` | 加 reply case |
| `internal/api/notify.go:21-29` | `NotifyHandler` 结构 | reply handler 复用其 Broker/Store |
| `internal/api/notify.go:31-47` | `NotifyRequest` | 已含 Kind/ReplyTo |
| `internal/api/notify.go:163-252` | `ServeTestNotify` | reply handler 参照此模式 |
| `internal/server/mux.go:95-100` | 路由注册 | 加 `/v1/reply` |
| `internal/server/mux.go:68` | `keyValidator` 适配 | reply 不需要（用 Cookie） |
| `internal/store/messages.go:78-106` | `GetMessage` | 已含 payload，无需改 |
| `internal/broker/broker.go:46-65` | `Message` 结构 | 已含 Kind/ReplyTo |
| `internal/broker/sqlite.go:49-105` | `Publish()` | 已支持 Kind/ReplyTo |
| `internal/ws/protocol.go:59` | `Frame.Kind` | 已透传 |
| `internal/ws/protocol.go:67-76` | `notificationFrame()` | 已传 Kind |
| `internal/ws/handler.go:167-185` | WS 消息分发 | reply 自动透传 |
| `internal/server/ratelimit.go:33-82` | `fixedWindow` | reply 限速复用 |
| `internal/server/ratelimit.go:146-155` | `rateLimited()` | reply 限速调用 |
| `internal/server/handlers.go:343-440` | `notificationsHandler` | reply 消息列表/详情已支持 |
| `internal/route/filter.go:46-60` | `ShouldDeliver` | reply 消息投递过滤已支持 |
| `internal/store/schema.sql:77-78` | `kind`/`reply_to` 列 | 已存在 |
| `internal/store/store.go:65-71` | `migrateColumns` | kind/reply_to 已迁移 |
| `web-src/pages/message.html:52-232` | JS 块 | 加 reply 逻辑 |
| `web-src/pages/index.html:152-190` | `normalize()` | 加 kind 提取 |
| `anotify-plugins/pi/anotify.ts:233-335` | 事件注册 | 加 sessionId + WS |
| `anotify-plugins/common/notify.sh:83` | `--session-id` | 已有参数，anotify.ts 未用 |
| `anotify-plugins/common/login.sh:133` | scopes | 改为 send+receive |
| `cmd/devseed/main.go:44-47` | Key 创建 | 测试用，可能加 reply scope Key |
