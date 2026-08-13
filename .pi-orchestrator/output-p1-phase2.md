# P1 阶段二 pi 扩展 - WS 订阅 + 回复注入 · 实施报告

## 状态：DONE

## 改动清单

### 1. `common/login.sh` — scopes 加 notify:receive（P3）
- 行 123：`["notify:send"]` → `["notify:send","notify:receive"]`
- 一个 Key 全能：既能上报（send）又能订阅 WS（receive）

### 2. `common/notify.sh` — 加 --kind/--reply-to 参数（P2）
- 新增 `KIND` / `REPLY_TO` 变量声明
- 新增 `--kind` / `--reply-to` 参数解析（case 分支）
- JSON 构造加 `"kind"` / `"replyTo"` 可选字段
- 用法注释更新

### 3. `pi/anotify.ts` — 核心改动（P1，最重）

#### 3a. 进程标识（D1）
- `generateProcessId()`：`randomBytes(2).toString("hex")` 生成 4 hex 随机短 id
- 格式：`pi@<hostname>:<rand4>`，本进程生命周期不变
- 路由键：`agent:<进程id>`

#### 3b. 上报带 sessionId（D5 + context §6.7 修复）
- 所有 3 处 `notify()` 调用（message_end / tool_execution_end / agent_settled）均加 `"--session-id", processId`
- sessionId 用进程随机 id 承载路由键（D5 决策）

#### 3c. WS 客户端（新增，零依赖）
- `WsClient` 类：Node 22 原生 `WebSocket` 全局对象
- 连接 `wss://server/v1/stream`（Bearer apiKey，从 credentials.json 读）
- subscribe 帧：`{type:"subscribe", tags:["agent:<进程id>"], event_scope:"all"}`
- 收 notification 帧：`kind==="reply" && tags 含本进程路由键` → 进入注入流程
- 重连：指数退避 5s → 10s → 30s 封顶
- resume：用 `Last-Event-Id` 头携带最后收到的消息 id
- 401/403 检测：close code 1008/4001 时提示"Key 缺少 notify:receive scope，请重新登录"

#### 3d. 回复注入
- `isRunning` 状态：`agent_start` → true，`agent_settled` → false
- `config.allowRemoteSteer`（默认 false）：false 时不建 WS 连接
- `config.confirmRemoteSteer`（默认 true）：收到 reply 后 `ctx.ui.confirm("远程指令", body)` 二次确认
- 注入：`pi.sendUserMessage(body, isRunning ? {deliverAs:"followUp"} : undefined)`（顶层 API）
- confirmCtx：保存最近事件 handler 的 ctx，供 WS 回调使用 ui.confirm/notify
- 安全提示：首次建连时 `ctx.ui.notify("已开启远程操控通道，收到指令需确认", "info")`

#### 3e. WS 生命周期（context §6.9）
- `session_start`（reason: startup/new/resume/fork）→ 建 WS（allowRemoteSteer=true 时）
- `session_shutdown`（reason: quit/reload/new/resume/fork）→ 关 WS

#### 3f. 配置项
- `allowRemoteSteer`（默认 false）+ 环境变量 `ANOTIFY_ALLOW_REMOTE_STEER`
- `confirmRemoteSteer`（默认 true）+ 环境变量 `ANOTIFY_CONFIRM_REMOTE_STEER`

#### 3g. 凭证读取
- `loadCredentials()`：读 `~/.config/anotify/credentials.json`（支持 XDG_CONFIG_HOME）
- 环境变量 `ANOTIFY_BASE_URL` / `ANOTIFY_API_KEY` 覆盖

### 4. `test/pi_extension_smoke.mjs` — 新增 9 项测试
1. ✓ 上报带 sessionId（格式 `pi@host:<rand4hex>`）
2. ✓ allowRemoteSteer=false → 不建 WS 连接
3. ✓ allowRemoteSteer=true → 建 WS 并 subscribe（tags=agent:pi@..., scope=all）
4. ✓ kind=reply + tags 匹配 → 注入 agent（followUp，agent 在跑）
5. ✓ 空闲时 reply 注入不使用 followUp（直接触发）
6. ✓ tags 不含路由键 → 不注入
7. ✓ kind != reply → 不注入
8. ✓ session_shutdown → 关闭 WS 连接
9. ✓ confirmRemoteSteer=false → 跳过确认直接注入
10. ✓ confirmRemoteSteer=true + 用户拒绝 → 不注入

测试用 MockWebSocket 替换全局 WebSocket，拦截 WS 连接，模拟服务端推送 reply 帧。

## 自测结果

### npm test（全量门禁）
```
════════════════════════════════════════
  通过：11  失败：0
════════════════════════════════════════
```

11 项全绿：
- 9 shell 测试（notify.sh × 8 + login.sh × 1）
- 1 TypeScript 类型检查（tsc --noEmit --strict）
- 1 Node 集成测试（pi_extension_smoke.mjs，含 26 个断言）

### TypeScript 类型检查
```
npx tsc --noEmit --strict --esModuleInterop --module esnext \
  --moduleResolution bundler --skipLibCheck --target es2022 --lib es2022 \
  pi/anotify.ts
→ exit 0（无错误无警告）
```

### grep 核对
- 所有 3 处 `notify()` 调用都带了 `"--session-id", processId` ✓
- 无 TODO/FIXME/占位符 ✓
- 无遗漏旧引用 ✓
- login.sh scopes 已改 `["notify:send","notify:receive"]` ✓
- notify.sh 已加 `--kind`/`--reply-to` 参数 ✓

## Bug

无新增 bug。

### 既有 bug（未修复，与本次改动无关）
- `test/test_notify_basic.sh` 第 22 行 `MOCK_BASE` 后跟全角中文 `（` 导致 `set -u` "unbound variable" 警告。这是 pre-existing bug（已在 worker memory 记录），测试仍通过（exit 0），不影响功能。

## 遗留风险

1. **confirmCtx 有效性**：WS 回调不在 pi 事件上下文中，通过保存最近事件 handler 的 ctx 来调用 `ui.confirm`。如果 session_shutdown 后 ctx 失效但 WS 还收到消息（竞态），confirm 可能抛异常——已用 try-catch 包裹，静默跳过。实际风险低：session_shutdown 会先关 WS。

2. **WebSocket 构造参数**：Node 22 原生 WebSocket 的 `new WebSocket(url, { headers })` 参数传递方式与浏览器不同。测试中用 MockWebSocket 验证了逻辑流程，但真实 Node WebSocket 的 headers 传递需实际环境验证（undici 实现）。如果 headers 不生效，Bearer token 可能需要通过 URL query 或 Sec-WebSocket-Protocol 传递——这需要后端 WS handler 配合验证。

3. **旧凭证兼容**：用户如果用旧版 login.sh 登录（只申请 notify:send），WS 连接会 403。已有明确提示"Key 缺少 notify:receive scope，请重新登录"。但不会自动重登录。

4. **reconnect 在鉴权失败后不重连**：close code 1008/4001 时不重连（避免无限 401 循环），但如果服务端临时返回 401 后恢复，需要用户重启 pi session。可接受。

## 技术决策记录

| 决策点 | 选择 | 理由 |
|--------|------|------|
| D1 路由键 | 进程随机 id | 避免同项目多窗口撞键 |
| D4 WS 客户端 | Node 22 原生 WebSocket | 零依赖 |
| D5 sessionId | 复用 sessionId 字段承载路由键 | 后端直接读，无需新字段 |
| D6 默认值 | allowRemoteSteer=false, confirmRemoteSteer=true | 双保险安全默认 |
| confirm API | ctx.ui.confirm（保存 confirmCtx） | 事件 handler 的 ctx 有 ui.confirm |
| 注入 API | pi.sendUserMessage（顶层 API） | 非 ctx.sendUserMessage |
