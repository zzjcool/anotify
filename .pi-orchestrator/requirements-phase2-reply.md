# 阶段二「双向回复」requirements（产品经理产出）

> 作者：pm 角色（定义层）。本文只做需求定义与方案边界，不含实现代码。
> 状态：**草案，待协调者对 §11 开放问题拍板后生效**。
> 依据代码（已读码核实）：`internal/broker/broker.go`、`internal/ws/{protocol.go,handler.go}`、
> `internal/api/notify.go`、`internal/server/mux.go`、`internal/authn/authn.go`、
> `anotify-plugins/pi/anotify.ts`、`anotify-plugins/common/notify.sh`、阶段一 `receiver-capability-design.md`。

---

## 1. 背景与目标

阶段一已完成「接收端能力模型」：消息带 `agentState`(5态)+`severity`+`kind`/`replyTo`(占位)，
设备按 `eventScope`(final|all) 订阅，WS subscribe 支持 eventScope 过滤。**但信息流是单向的**——
agent 上报 → 接收端收通知，接收端无法回话。

阶段二目标：**用户在接收端（手机/工作台/CLI）看到通知后，能回复一句话，注入回正在跑的 pi agent，
触发新一轮对话**——即"远程对话 AI"。

核心用户痛点：开发者跑着多个 agent，看到"任务完成"或"等待输入"的通知后，想直接补一句指令
（"继续改下样式"/"跑下测试"），而不是回到终端窗口重新输入。

**边界**：本阶段只做"回复注入 pi agent"这一条链路，不做 pet 呈现、不做编排原语（herdr 桥接）、
不做多 agent 群发编排。

---

## 2. 核心链路图

```
【接收端】                          【Anotify 中枢】                    【pi 进程】
手机/工作台/CLI                                                        ┌──────────────────┐
  │ 看到消息（agentState=done/blocked）                                │ pi agent 正在跑   │
  │ 用户输入回复"继续改样式"                                            │                  │
  ▼                                                                   │  ┌────────────┐  │
POST /v1/reply                                                      │  │ anotify 扩展 │  │
  { replyTo: "<原消息id>", body: "继续改样式" }                       │  │            │  │
  ──────────────▶ Go 后端                                           │  │  上报(notify)│──┼──▶ POST /v1/notify
                    │ 鉴权(Cookie 会话 或 notify:reply Key)           │  │            │  │
                    │ 反查 replyTo 消息 → 拿到 agentId/sessionId      │  │  订阅(stream)│◀─┼── WS kind=reply
                    │ 反查原消息 cwd → 定位 pi 工作目录               │  │            │  │   (含 body)
                    ▼                                                │  └─────┬──────┘  │
              构造 kind=reply 消息                                    │        │         │
              (replyTo=原id, body=指令,                              │        ▼         │
               deviceTags=[reply路由键])                             │  pi.sendUserMessage│
                    │                                                │     (body)       │
                    ▼                                                │        │         │
              Broker.Publish ──▶ WS 派发器 ──────────────────────────┼───────▶▼         │
                                   (按 reply 路由键精确投递)           │   agent 开新一轮   │
                                                                   └──────────────────┘
```

**关键新增**：pi 扩展从"只上报"变为"上报 + 订阅"——新增一条到 `/v1/stream` 的 WS 长连接，
专门收 kind=reply 的消息。

---

## 3. 回复路由模型（最关键设计）

### 3.1 问题：回复怎么精确路由到那个 pi 进程？

一个用户可能同时跑多个 pi 进程（多窗口/多项目）。回复必须送到**唯一**那个跑该任务的进程。

### 3.2 现状核实（读码结论）

- pi 扩展上报 `POST /v1/notify` 时带 `agentId`（`pi@<hostname>`）+ `cwd`，**未带 sessionId**（notify.sh 有 `--session-id` 参数但 anotify.ts 未用）。
- `agentId`/`sessionId` 目前只进 `Message.Payload`（原始 JSON blob），**不是规范化字段**，broker 不按它们路由。
- broker 的 `Subscribe(userID, tags)` 只按 **tags** 过滤；WS 的 matchTags/matchScope 也只按 tags + eventScope。
- WS notification 帧（`notificationFrame`）**不带 agentId/sessionId**——订阅端无法知道"这条是哪个 agent 发的"。

### 3.3 设计方案：reply 路由键 = deviceTags 承载

**核心思路：不新增专用路由字段，复用现有的 tags 路由机制。** 让"回复"消息带一个专属路由键，
pi 扩展订阅时只收这个键，从而实现精确投递。

路由键约定（落在 deviceTags 上）：

```
agent 上报时（pi 扩展）：deviceTags 附加一个自身标识键
  例：deviceTags = ["agent:pi@host:projname"]   （agentId + cwd 末段，稳定且短）
reply 消息（后端构造）：deviceTags = 原消息里提取的该 agent 标识键
pi 扩展订阅 WS：subscribe(tags=["agent:pi@host:projname"], event_scope="all")
```

这样：

- **精确性**：reply 的 tags 取自原消息（反查 replyTo 消息的 payload.agentId + cwd 拼出来），
  只会推给订阅了同一路由键的那个 pi 扩展连接。其他 pi 进程（路由键不同）收不到。
- **零协议侵入**：不新增 broker 路由维度，复用现有 tagMatch 过滤。
- **push 端不打扰**：reply 消息的 deviceTags 带 agent 键，普通 push 设备没订阅该键 → 收不到 reply
  （符合预期：reply 是内部回环，不该上手机锁屏）。

### 3.4 pi 进程标识（路由键怎么稳定）

`agent:pi@<hostname>:<cwd-basename>` 仍可能撞（同一项目开两个窗口）。**更稳的方案**：
pi 扩展启动时生成一个**会话级随机短 id**（如 `agent:pi@host:a1b2`），本进程生命周期内不变，
作为该进程的唯一路由键，上报和订阅都用它。

- 优点：彻底避免同项目多窗口撞键。
- 代价：用户看通知时看到的是可读性差的 id——但 deviceTags 不直接展示给用户，无妨。
- **决策点 D1**（见 §11）：路由键用「agentId+cwd 可读键」还是「进程随机 id 唯一键」？
  pm 倾向**进程随机 id**（精确性优先），可读性交给通知正文的 cwd 字段承担。

### 3.5 后端反查逻辑

`POST /v1/reply { replyTo }` 的处理：

1. 按 `replyTo`（消息 id）从 store 反查原消息（含 payload）。
2. 从 payload 提取 `agentId` + `cwd`（或阶段一后规范化出的字段），重建该 agent 的路由键。
   - 若采用「进程随机 id」，则该 id 必须已随上报写入 payload（pi 扩展把它放进 deviceTags 或 agentId）。
   - 建议：pi 扩展上报时把进程 id 同时写进 `sessionId` 字段（复用现有 payload 字段，后端直接读 sessionId 拼路由键），避免引入新字段。
3. 构造 kind=reply 消息，deviceTags=[该路由键]，Publish。
4. 若反查不到原消息 / 原消息无 agent 标识 → 404/400（见 §5）。

---

## 4. 安全模型（第二关键）

`pi.sendUserMessage` 等于让外部远程操控 agent 跑命令。这是高危能力，必须可控。

### 4.1 总开关 `allowRemoteSteer`（默认关）

- pi 扩展新增配置 `anotify.allowRemoteSteer`，**默认 `false`**。
- 默认关闭时：pi 扩展**不建立** reply 订阅连接（完全不收 reply），用户必须在终端本地操作。
- 用户显式开启后才订阅。开启动作本身是一次"我信任这个通道"的授权。

### 4.2 二次确认（可配）

- 新增 `anotify.confirmRemoteSteer`，默认 `true`。
- 开启时：pi 扩展收到 reply 后，先 `ctx.ui.confirm("收到远程指令：xxx，是否执行？")`，
  用户在终端确认后才 `sendUserMessage`。
- 关闭时（用户明确信任）：直接注入，不打扰。
- **默认 confirm=true** 与 allowRemoteSteer=false 形成双保险：即使误开了订阅，也还要人在终端点头。

### 4.3 权限（谁能回话）

- **消息属主本人**：`/v1/reply` 用 **Cookie 会话**鉴权（工作台天然有权），userID 从会话取，
  反查的原消息必须属于该 userID（越权校验）——这是**默认且推荐**路径。
- **API Key `notify:reply` scope**：为 CLI/脚本/将来的 pet 预留。Key 带该 scope 才能调 /v1/reply。
  属主校验同样生效（Key 属于某 user，只能回复该 user 的消息）。
- **决策点 D2**（见 §11）：阶段二是否同时实现 Cookie + Key 两种鉴权，还是先做 Cookie（工作台/手机 Web）？
  pm 倾向**两种都实现**（Cookie 给 Web，Key 给 CLI/pet），成本相近，一次到位。

### 4.4 审计与限流

- **审计**：kind=reply 消息本身入库（复用 messages 表，kind=reply），天然留痕。
  通知历史里能看到"谁在何时回复了什么"。
- **限流**：/v1/reply 按 user 限速（如 30/min），防脚本狂注入。复用现有 rate-limit 机制。

---

## 5. 端点设计

### 5.1 `POST /v1/reply`（独立端点，而非复用 /v1/notify）

**决策：独立 `/v1/reply`**。理由：

- /v1/notify 是"agent 上报"语义（Bearer + notify:send），/v1/reply 是"用户回话"语义（Cookie/notify:reply），
  鉴权方式、属主、校验逻辑都不同，混在一个端点会互相污染。
- 独立端点让"回复"这个动作在契约上显式可见，便于审计与限流。
- 但 reply 消息入库后**复用** broker 的 Message/Publish/Subscribe 机制（kind=reply），不另起存储。

**请求**：

```yaml
POST /v1/reply
鉴权: Cookie 会话（工作台） 或 Bearer Key（scope=notify:reply）
Content-Type: application/json
{
  "replyTo": "ntf_01J8XA...",   // 必填，目标消息 id
  "body": "继续改下样式"         // 必填，回复指令，≤ 2000 字符
}
```

**响应**（200）：

```json
{
  "id": "ntf_01J9...",          // 新 reply 消息 id
  "routed": true,               // 是否成功反查并路由到 agent
  "agentRoute": "agent:pi@host:a1b2"  // 实际投递的路由键（便于调试）
}
```

**错误**：
- `400` 缺 replyTo/body、body 超长
- `401` 无会话 / Key 无效 / 无 notify:reply scope
- `404` replyTo 消息不存在
- `403` replyTo 消息不属于当前 user（越权）
- `422` 原消息无 agent 标识（无法路由，如系统测试消息）
- `429` 超限速

### 5.2 与 /v1/notify 的关系

reply 入库后是**一条普通 broker.Message**（kind=reply），走同一套 Publish/Subscribe/推送管线。
区别只在：tags 带 agent 路由键（普通设备收不到）、kind=reply（前端可区分渲染）。

---

## 6. WS 透传与 pi 扩展过滤逻辑

### 6.1 WS 侧（基本就绪，可能需小补）

- `notificationFrame` 已带 `kind`（阶段一已加）。reply 消息 kind=reply 会原样透传。
- **缺口核实**：notificationFrame **不带 agentId/sessionId/deviceTags 之外的路由信息**。
  但 pi 扩展靠 subscribe 帧的 tags 过滤即可——只要订阅时声明了自己的路由键，
  服务端 matchTags 就只把带该键的 reply 推给它。**无需改 WS 帧结构**。
- pi 扩展订阅帧：`{ type:"subscribe", tags:["agent:pi@host:a1b2"], event_scope:"all" }`。
  （event_scope 用 all——reply 消息的 agentState 怎么定见 D3，用 all 最保险不被 final 过滤掉。）

### 6.2 pi 扩展过滤逻辑

```
收到 notification 帧：
  if (frame.kind === "reply" && frame.tags 含本进程路由键) {
    if (!config.allowRemoteSteer) return;            // 总开关（其实没开就不会订阅）
    if (config.confirmRemoteSteer) {
      const ok = await ctx.ui.confirm("远程指令", frame.body);
      if (!ok) return;
    }
    pi.sendUserMessage(frame.body, { deliverAs: agent忙?"followUp":undefined });
  }
```

- **忙/闲判断**：pi 扩展本地知道 agent 是否在跑（监听 agent_start/agent_settled）。
  忙时 `deliverAs:"followUp"`（等当前轮结束再注入），空闲时直接触发（文档：sendUserMessage 总是 trigger turn）。

---

## 7. pi 扩展改动（跨仓库 anotify-plugins）

1. **进程标识**：启动时生成/读取稳定进程 id（如 `pi@host:<rand4>`），本进程不变。
2. **上报增强**：`POST /v1/notify` 时把进程 id 写进 `sessionId`（复用现有字段，后端据此反查路由键），
   deviceTags 附加 `agent:<进程id>`。
3. **新增 WS 订阅**：连 `/v1/stream`（用 notify:receive scope 的 Key），subscribe 带本进程路由键。
   - 需处理断线重连（resume/seq），复用或新写轻量 WS 客户端（Node 内置无 WebSocket 客户端，
     **决策点 D4**：用 `node:http` 手写 upgrade 还是引一个零依赖 ws 实现？pm 倾向手写最小 upgrade，
     保持"零运行时依赖"原则）。
4. **回复注入**：按 §6.2 过滤 + confirm + sendUserMessage。
5. **配置项**：`allowRemoteSteer`(默认 false)、`confirmRemoteSteer`(默认 true)、
   `remoteSteerTags`(可选，自定义路由键覆盖)。
6. **凭证复用**：reply 订阅用的 Key 与上报用的 Key 同源（`~/.config/anotify/credentials.json`），
   但该 Key 需同时具备 notify:send + notify:receive（登录时申请，见 §11 D5）。

---

## 8. 前端改动

1. **工作台消息详情页（message.html）**：加回复输入框 + 发送按钮（调 /v1/reply，Cookie 鉴权）。
   仅当消息含 agent 标识（可路由）时显示；回复后展示 reply 消息（kind=reply 气泡）。
2. **通知列表（index.html）**：reply 消息按 kind=reply 区分渲染（如"你回复了：xxx"）。
3. **手机 Web**：同一工作台页面，移动端可用（WS 在线时）。**iOS Web Push 不支持 inline reply**——
   推送只读，用户点推送 → 打开工作台 → 在详情页回复。这是可接受的降级。
4. i18n：回复相关词条（reply.*）4 语言补齐。

---

## 9. 接收端能力矩阵（reply 维度）

| 接收端 | 能收 reply 通知？ | 能发起回复？ | 说明 |
|---|---|---|---|
| **iOS/外部 Web Push** | 否（reply 带 agent 路由键，push 设备收不到） | 间接（点推送→开工作台→回复） | push 单向、无 inline reply |
| **Web 工作台（WS）** | 是（订阅 all） | **是**（Cookie 鉴权调 /v1/reply） | 阶段二主战场 |
| **桌面 Pet（将来）** | 是（订阅 all） | 可选（持 notify:reply Key） | 阶段三呈现，回复能力本阶段已备 |
| **CLI stream** | 是 | 是（Key 带 notify:reply） | 脚本化回话 |
| **pi 扩展（被回话方）** | 是（订阅自身路由键） | N/A（它是执行方） | 收 reply → 注入 agent |

---

## 10. 协议改动清单（字段级）

### openapi.yaml
- 新增 `/reply` 路径 + `ReplyRequest`（replyTo, body）+ `ReplyResponse`（id, routed, agentRoute）。
- Key scope enum 加 `notify:reply`。
- `NotifyRequest` 无改动（kind/replyTo 占位已在）。

### broker / store
- 无 schema 改动（kind/replyTo/deviceTags 阶段一已入库）。
- 可能需 store 加"按 id 反查单条消息（含 payload）"的查询（若现有 GetMessage 不含 payload 则需补）。

### api
- 新增 `internal/api/reply.go`（或并入 notify.go）：鉴权 + 越权校验 + 反查 + 构造 kind=reply + Publish。
- mux.go 注册 `POST /v1/reply`。

### authn
- scope 常量加 `ScopeNotifyReply = "notify:reply"`。

### ws
- 无改动（kind 已透传，tags 过滤已支持）。

### 插件（跨仓库）
- `common/notify.sh`：可加 `--session-id` 已有；确认上报带进程 id。
- `pi/anotify.ts`：进程 id 生成、WS 订阅、回复注入、新配置项。

---

## 11. 开放问题（需协调者拍板）

- **D1 · pi 进程路由键**：用「agentId+cwd 可读键」还是「进程随机 id 唯一键」？
  pm 倾向**进程随机 id**（精确性优先，避免同项目多窗口撞键）。→ 待确认。
- **D2 · /v1/reply 鉴权**：只做 Cookie（Web）还是 Cookie + API Key(notify:reply) 都做？
  pm 倾向**两种都做**（Key 给 CLI/pet，成本相近）。→ 待确认。
- **D3 · reply 消息的 agentState 取值**：reply 是"用户回话"事件，不属于 agent 生命周期 5 态。
  需不需要给 reply 一个特殊 agentState（如固定 `working`，表示"用户给了新指令，agent 要干活了"）？
  还是 agentState 留空/idle？这影响 eventScope=final 的设备是否收到（pm 建议 reply 固定 agentState=working，
  这样 final 设备收不到——reply 本就不该上 push）。→ 待确认。
- **D4 · pi 扩展 WS 客户端实现**：Node 无内置 WS 客户端。手写最小 upgrade 解析（保零依赖）
  还是允许引一个轻量 ws 库？pm 倾向**手写最小实现**（守住零运行时依赖原则），
  但需评估帧解析复杂度。→ 待确认。
- **D5 · 进程 id 放哪个字段**：复用 `sessionId`（后端直接读）还是放 deviceTags（约定 agent: 前缀）？
  pm 倾向 **sessionId**（语义贴合"这个上报来自哪个会话/进程"，后端反查最直接）。→ 待确认。
- **D6 · allowRemoteSteer 默认值**：pm 已定默认 false + confirm 默认 true（双保险）。
  是否同意"默认全关、显式开启"？→ 轻量确认。

## 12. 阶段二边界

### 做
1. `POST /v1/reply`（鉴权 + 越权 + 反查 + kind=reply 入库 + 限流）。
2. scope 加 `notify:reply`。
3. pi 扩展：进程 id + 上报带 id + WS 订阅 + confirm + sendUserMessage 注入 + 安全开关。
4. 前端：消息详情回复框 + reply 气泡渲染 + i18n。
5. E2E：reply 端到端（上报→回复→WS 投递）+ 越权 + 限流 + 路由精确性。

### 不做
- ❌ Pet 呈现（阶段三）。
- ❌ 编排原语 / herdr 桥接 / 多 agent 群发（阶段四）。
- ❌ reply 的富文本/附件（仅纯文本指令）。
- ❌ 回复历史独立页（复用通知历史）。
- ❌ Claude Code / CodeX 插件的双向（它们无常驻进程，pi 先行）。

---

## 附：一句话总结

**用户在接收端对一条 agent 通知回话（POST /v1/reply，鉴权+越权校验+限流），后端反查原消息的
agent 路由键，构造 kind=reply 消息经既有 broker/WS 管线精确推给那个 pi 进程；pi 扩展（显式开启
allowRemoteSteer + 可选 confirm 双保险）收到后 sendUserMessage 注入，触发 agent 新一轮。
路由复用 tags 机制零协议侵入，安全默认全关可审计。**
