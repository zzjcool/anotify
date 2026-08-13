# 阶段二实施计划：双向回复

> 依据：requirements-phase2-reply.md（pm）+ context-phase2-reply.md（scout）
> 关键风险已解除：`pi.sendUserMessage()` 是顶层 API，事件 handler/WS 回调可直接调（非 ctx.sendUserMessage）。

## 一、任务拆分

### Task W1 · 后端（worker，worktree）
依赖 scout context B1-B8。

1. **scope 常量**（两处同步，context §6.1 陷阱）：
   - `internal/authn/authn.go`：加 `ScopeNotifyReply = "notify:reply"`
   - `internal/auth/apikey.go`：常量 + `validScope()` + `scopeLabel()`（reply 并入 full 或单独前缀）
2. **reply 端点** `internal/api/notify.go`：
   - 新增 `ReplyRequest{ReplyTo, Body}` + `ServeReply` 方法
   - Cookie 鉴权（sessMW，`auth.UserIDFromContext`）
   - 限流（ratelimit.go 加 `rlReply` 20/min/user，context B8）
   - 反查原消息 `GetMessage(userID, replyTo)`（已含 payload，B5 确认无需改 store）
   - 越权校验（原消息不属于该 user → 403）
   - 原消息不存在 → 404；无 agent 标识 → 422
   - 构造 reply 消息：`Kind="reply"`, `ReplyTo=replyTo`, `AgentState="working"`（D3）, `Body=用户输入`, `deviceTags=原消息的agent路由键`（context §6.5）
   - 从原消息 payload 反序列化取 agentId/sessionId，构造 reply 的 payload
   - `Broker.Publish`
3. **路由注册** `internal/server/mux.go`：`mux.Handle("/v1/reply", noStore(sessMW(...)))`（B4）
4. **openapi.yaml**：加 `/reply` 路径 + ReplyRequest/ReplyResponse schema + scope enum 加 notify:reply（context §6.8）
5. **Go 单测** `internal/api/reply_test.go`（新建）：合法回复/越权403/不存在404/无agent422/限流429/缺字段400
6. **devseed**：测试播种工具加 reply scope Key 选项（context T2 提到）

自测：`go build ./...` + `go test ./internal/...` 全绿。

### Task F1 · 前端（worker 壳，主工作区）
依赖 scout context F1-F3。

1. **message.html**：正文与全部字段之间加回复区（textarea + 发送按钮），只有 kind=task 才显示；`sendReply()` 调 POST /v1/reply（Cookie）；回复后刷新展示 reply 气泡
2. **index.html**：normalize() 提取 kind；renderNotifs() 对 kind=reply 加区分渲染（缩进/reply 图标/badge）
3. **locales/*.yaml**（4 语言）：加 reply.title/placeholder/send/sending/success/failed/badge
4. class 纪律（context §6.10）：只用 card/btn-primary/input + Tailwind 工具类

自测：`make fe` 无 warn + web_verify 逐页。

### Task P1 · pi 扩展（worker 壳，anotify-plugins 仓库，跨仓库）
依赖 scout context P1-P3。这是阶段二最重的部分。

1. **进程标识**：anotify.ts 启动生成进程随机 id（D1），本进程不变
2. **上报带 sessionId**：所有 notify() 调用加 `--session-id`（context §6.7 修复遗漏）。sessionId 用进程随机 id（D5：复用 sessionId 字段承载路由键）
3. **WS 客户端**（新增）：
   - Node 22 有原生 `WebSocket`（全局），零依赖（D4 确认环境是 Node）
   - 连 `wss://server/v1/stream`（Bearer apiKey）
   - subscribe 帧：`{type:"subscribe", tags:["agent:<进程id>"], event_scope:"all"}`（D1 路由键）
   - 收 kind=reply 且 tags 含本进程键 → 进入注入流程
   - 重连：断线指数退避 + resume（Last-Event-Id 或 resume token）
4. **回复注入**：
   - 维护 `isRunning` 状态（监听 agent_start/agent_settled）
   - `config.allowRemoteSteer`（默认 false，D6）false 时不建 WS 连接
   - `config.confirmRemoteSteer`（默认 true）true 时 `ctx.ui.confirm()` 二次确认（注：用 pi 顶层 confirm 或 ctx.ui——核实哪个可用）
   - 注入：`pi.sendUserMessage(body, isRunning ? {deliverAs:"followUp"} : undefined)`（★ 顶层 API，非 ctx）
   - 首次开启订阅时 `ctx.ui.notify` 提示"已开启远程操控通道"（我补的安全提示）
5. **login.sh**：scopes 改 `["notify:send","notify:receive"]`（P3，context §6.3 陷阱）
6. **notify.sh**：加 `--kind`/`--reply-to` 参数（P2，虽然 reply 主要走后端构造，但留参数备用）
7. **测试**：pi_extension_smoke.mjs 加 WS 订阅 + reply 注入的 mock 测试

自测：`npm test` 全绿。

### Task T1 · E2E（tester/worker 壳，主工作区）
依赖 W1+F1。

1. **api_contract.mjs**：reply 端点契约（401/400/404/403/422/200，context T1）
2. **新建 reply_e2e.mjs**（T2）：上报 task → reply → WS 收 reply 帧 → 验证 payload 含原 agentId
3. **ws_protocol.mjs**（T4）：reply 帧透传 + kind 字段
4. **security.mjs**（T3）：reply 越权（属主校验）

门禁：`make e2e` 全绿。

### Task R1 · 终审（reviewer）
重点审：安全模型（默认关+确认+越权+限流）、路由键精确性、sendUserMessage 顶层 API 用法、WS 生命周期。

## 二、执行顺序

```
W1(后端, worktree) ┐
                   ├─ W1 合并后 → T1(tester) → R1(reviewer)
F1(前端, 主工作区)  ┘
P1(pi 扩展, plugins 仓库) ── 独立并行（跨仓库不冲突）
```

W1/F1 后端前端可并行（worktree + 主工作区）。P1 跨仓库完全独立，可与 W1/F1 并行。T1/R1 依赖 W1+F1。

## 三、验收标准
1. POST /v1/reply：Cookie 鉴权 + 越权 + 限流 + 反查路由 + kind=reply 入库
2. notify:reply scope 加入（两处常量同步）
3. pi 扩展：进程 id + 上报带 sessionId + WS 订阅 + allowRemoteSteer 开关 + confirm + sendUserMessage 注入
4. 前端：消息详情回复框 + reply 气泡 + i18n
5. make e2e + npm test 全绿
6. 安全：默认全关、可审计（reply 入库）、可限流

## 四、开放问题（已拍板）
D1 进程随机 id / D2 Cookie+Key 都做（阶段二先实现 Cookie，Key scope 常量先加但不强制实现 Key 鉴权路径，留给 CLI/pet）/ D3 reply agentState=working / D4 手写 WS（Node22 原生 WebSocket）/ D5 sessionId / D6 默认全关。
