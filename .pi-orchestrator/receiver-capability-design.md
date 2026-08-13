# 接收端能力/权限订阅模型 · 设计建议

> 作者：anotify-pm（定义层）。本文只做**调研结论 + 协议设计建议**，不含实现代码。
> 状态：**草案，待协调者/产品负责人对 §8 开放问题拍板后生效**。
> 依据代码：api/openapi.yaml、internal/route/filter.go、internal/broker/broker.go、
> internal/store/{schema.sql,devices.go,messages.go}、internal/api/notify.go、
> internal/ws/{protocol.go,handler.go}、internal/push/dispatcher.go、internal/server/handlers.go、
> web-src/pages/receivers.html（均已逐一读码核实，非凭空设计）。

---

## 1. 问题分析：当前模型把三件事搅在一起

### 1.1 `status` 字段：语气与运行态混为一谈

`NotifyRequest.status`（api/openapi.yaml:1053 起）取值 `success|error|interrupted|info|warning`。
这五个值里混着两种东西：

- **运行态**：success（做完了）、interrupted（被打断）、info（在跑）
- **语气**：warning（注意一下）、error（出错了——但出错时 agent 可能还在跑，也可能已死）

后果：发送方（插件）上报时要猜"这算状态还是语气"；接收方（设备）按它过滤时，
"只收 error" 既可能指"只收失败的任务"，也可能指"工具报错的瞬间"——语义含糊。
pi 插件现在就拿 `status=error` 报"工具失败（节流）"，又拿 `status=success` 报"任务完成"，
一个是瞬时事件语气，一个是终态，却共用同一字段。

### 1.2 `Device.status_filter`：过滤偏好与接收端能力混在一起

`internal/route/filter.go` 的投递判定：
`enabled AND StatusMatch(device.status_filter, msg.status) AND TagMatch`。
`status_filter`（all|error|success）表达的是"用户想按语气过滤通知"，
但它被当成了**唯一的接收端维度**——协议里根本没有地方说"这个接收端只能收最终结果 /
能收全流程 / 能回话"。

### 1.3 协议不认识"接收端能力"这个维度

产品负责人原话诉求：

> "iOS 的外部 Web Push 不能 stream、也不能回复，那就只接收最终结果；
> 但宠物（pet）可以接收全生命流程。"

这句话里有**三个正交维度**，当前协议一个都表达不了：

1. **订阅范围**：只要最终结果 vs 要全生命周期。
2. **交互能力**：能不能回复（双向）。
3. **传输形态**：push（APNs/FCM 存储转发，不丢但贵、单向）vs WS（在线实时，断线靠 replay 补）。

### 1.4 协调者被否的草稿错在哪

草稿把 `receive_modes: ["push","pet","terminal"]` 放进 Device 字段。
这是**形态（form）**，不是**能力（capability）**：协议一旦写了 "pet"，
将来出现第四种形态（手表、车载）就得改协议；且同一形态可以有不同能力
（两个 pet 实例，一个可回复一个只读）。**协议层必须只描述能力与权限，形态是实现细节。**

---

## 2. 能力维度设计：三根正交的轴

核心结论：**把"接收端差异"拆成三根互相独立的轴，每根轴有自己的归属位置。**

### 轴 A · 订阅范围（Subscription Scope）——"我想收哪段生命周期"

- 取值：`final`（只收终态事件）| `all`（收全部生命周期事件）
- 语义：对消息 `agentState` 的过滤器。`final` = 只放行终态（done/interrupted/error）。
- 本质：是**终态性（terminality）**这一个布尔派生量的两级预设。协议上不暴露"任意状态子集"，
  只有两个预设档——刻意收窄，避免阶段一过度设计（见 §8 Q4 留了扩展口）。
- **这是偏好，不是权限**：用户对自己的设备随时可改，不需要授权。

### 轴 B · 交互权限（Interaction Permission）——"我被允许做什么"

- 取值：沿用并扩展 API Key scope 体系。
  现有 `notify:send / notify:receive / devices:read`；阶段二新增 **`notify:reply`**（占位）。
- 语义：**授权（authorization）**维度——"这个凭证被允许提交回复吗"。
  它天然长在凭证上（API Key / 会话 Cookie），不长在设备上。
- 关键区分：**有权限 ≠ 有通道**。工作台会话（Cookie）天然有全部权限；
  push 设备即使被授权也没有回话通道（push 是单向的）——那是传输决定的，不是权限决定的。

### 轴 C · 传输与可靠性（Transport & Reliability）——"消息怎么到我、会不会丢"

- **不进协议字段**。它是**接收端接入方式的内在属性**：
  - 走 Web Push（注册 endpoint）→ APNs/FCM 存储转发，设备离线不丢，单向，每条有成本。
  - 走 WebSocket（连 /stream）→ 在线实时，断线靠 seq 位移 replay 补齐（broker 已有 Replay），可承载双向。
- 服务端从**接入路径**就知道传输是什么（push 设备有 endpoint；WS 是长连接），
  不需要任何字段声明。**"会不会丢"不是订阅维度，是传输属性**——
  push 的可靠由 APNs/FCM 保证，WS 的可靠由 replay 保证，协议两者都已具备。

### 为什么这样切（设计原则）

1. **正交**：改任何一根轴不影响另外两根。push 设备可以从 final 改成 all（用户偏好变了），
   不需要动权限也不需要动传输。
2. **形态永不入协议**：pet/手表/车载都只是"某个走了 WS 或 push 的接收端"，
   协议只看到 scope/permission/传输三轴。
3. **权限与偏好分离**：权限（轴 B）防越权，归凭证；偏好（轴 A）管体验，归订阅。
   两套体系各管各的，不打架。

---

## 3. agentState 设计：生命周期运行态

### 3.1 枚举（5 值）

```
working      在跑（任务开始 / 进行中）
blocked      卡住（等待输入 / 等待审批）
done         完成（正常结束）
interrupted  被打断（用户中止 / 外部中断）
error        失败（任务级失败，终态）
```

依据：对齐 herdr 5 态（working/blocked/done/idle/unknown）与 CodeX pet 4 阶段
（Running/Waiting/Review/Failed）的交集，去掉两个"非事件"值：

- **herdr 的 idle/unknown 不进枚举**：idle 是"完成且已被看过"的 UI 确认态、
  unknown 是"检测不到"——它们是 herdr 持续跟踪活终端的产物。
  Anotify 是事件流，"没事件"本身就表达了 idle/unknown，不需要上报一个"我空闲了"的事件。
- **pet 的 Failed = error、Review = done、Waiting = blocked、Running = working**，一一对上。

### 3.2 终态性划分（轴 A 的过滤依据）

| agentState | 终态？ | 说明 |
|---|---|---|
| working | 否 | 中间态 |
| blocked | 否 | 中间态（等输入，任务未终结） |
| done | **是** | 终态 |
| interrupted | **是** | 终态 |
| error | **是** | 终态 |

`final` 订阅 = 只放行终态三值。

### 3.3 旧 status → agentState 映射（迁移用）

| 旧 status | 新 agentState | 备注 |
|---|---|---|
| info | working | "开始/进行中"通知 |
| warning | blocked | "需要注意"≈等待处理 |
| success | done | |
| interrupted | interrupted | |
| error | error（任务失败）**或** working+severity=error（工具瞬时失败） | 见 3.4 |

### 3.4 error 是状态还是语气？——拆成 state + severity

这是本设计的关键裁决点。结论：**两者都要，但分开**。

- `agentState=error`：**任务级失败**（终态）。agent 这次彻底没跑成。
- 新增可选字段 **`severity`**（`info|warning|error`，默认从 agentState 派生）：
  **呈现语气**，纯展示用（通知颜色/图标），不参与生命周期、不参与轴 A 过滤。
- 用例：pi 插件现在报"工具失败（节流，agent 还在跑）"——
  新模型下是 `agentState=working, severity=error`（在跑，但这次工具调用红了）。
  既保留了"工具失败要红色提醒"的现有体验，又不污染状态机。

派生默认：working→info，blocked→warning，done→info（或 success 绿），interrupted→warning，error→error。
（severity 默认值后端填充，前端可直接用。）

---

## 4. 接收端能力声明模型：字段放哪

### 4.1 三轴的归属（核心答案）

| 轴 | 归属 | 理由 |
|---|---|---|
| A 订阅范围 `eventScope` | **订阅（Subscription）**——push 设备存 Device 行；WS 存 subscribe 帧 | 偏好随订阅走，持久订阅（push）落库，临时订阅（WS 连接）随连接 |
| B 交互权限 | **凭证（Credential）**——API Key scope / 会话权限 | 授权必须绑定身份，不能绑定设备（设备会丢、会共享） |
| C 传输可靠性 | **不声明**——由接入路径推断 | push 有 endpoint、WS 是长连接，服务端天然知道 |

### 4.2 为什么不打架

- 轴 A 和轴 B 是正交的：一个管"我想看什么"，一个管"我被允许做什么"。
  一个 WS 客户端可以 scope 订阅 final（偏好），同时持有 notify:reply（权限）——互不干扰。
- 轴 C 不需要字段：传输差异已经体现在**两个既有消费者**上
  （broker 的消费者1=WS 派发器、消费者2=Push 派发器），这是架构里已有的分流点。

### 4.3 scope 扩展（阶段一只占位）

现有 enum：`notify:send, notify:receive, devices:read`。
阶段一**不动**；协议文档预留 `notify:reply`（阶段二：用户→agent 方向的提交权）。
注意方向：agent 上报用 notify:send；接收端收流用 notify:receive；
将来**接收端回话**用 notify:reply——三个 scope 对应三个动作，语义干净。

---

## 5. 投递过滤新规则

替代 `route.StatusMatch`：

```
ShouldDeliver(dev, msg) ⟺
    dev.enabled
    AND ScopeMatch(dev.eventScope, msg.agentState)   -- 替代 StatusMatch
    AND TagMatch(dev.Tags, msg.DeviceTags)           -- 不变
```

```
ScopeMatch(filter, agentState):
    "all"   → true
    "final" → agentState ∈ {done, interrupted, error}
```

要点：

- **severity 不参与过滤**——它只是呈现语气。一个 `working+severity=error`（工具瞬时失败）
  对 `final` 设备**不投递**（agent 还没到终态），对 `all` 设备投递并显示红色。
- **TagMatch 保持不变**——轴 A（scope）管"生命周期哪段"，tags 管"哪个任务/项目"，两轴正交。
- **WS 侧**同理：subscribe 帧可加 `eventScope`，ws/handler.go 里 matchTags 旁边加 matchScope；
  不加则默认 `all`（WS 是实时通道，默认全量合理）。
- **旧的"只收 error"过滤语义被 final 取代**（见 §8 Q3 的取舍说明）。

---

## 6. 接收端能力矩阵（验证模型够用）

| 接收端 | 传输（轴C，推断） | 订阅范围（轴A） | 可回复（轴B） | 离线不丢 |
|---|---|---|---|---|
| **iOS/外部 Web Push** | push（存储转发） | `final`（默认；用户可升 all） | 否（无通道） | ✅ APNs/FCM 持久化 |
| **Web 工作台（WS）** | WS + replay | `all`（默认） | 阶段二：✅（Cookie 会话天然有权） | 在线实时；断线 replay 补齐 |
| **桌面 Pet（将来）** | WS + replay | `all`（需全流程驱动动画） | 可选（持有的 Key 若有 notify:reply） | 同 WS |
| **CLI stream** | WS + replay | `all`（可用 --final 收窄） | 阶段二：Key 有 notify:reply 则可 | 同 WS |

验证产品负责人两个案例：

1. **"iOS push 只收最终结果"** → push 设备 `eventScope=final`，
   只有 done/interrupted/error 上锁屏。✅ 模型直接表达。
2. **"pet 收全生命流程"** → pet 走 WS 长连接，`eventScope=all`，
   working→跑动画、blocked→等待动画、done→完成动画、error→失败动画
   （正好对上 CodeX pet 的 Running/Waiting/Review/Failed 四态）。✅ 模型直接表达。

两轴正交性验证：把 iOS 设备从 final 改成 all（用户就想看全程）→ 只改轴 A 一个字段，
权限与传输不动。✅ 证明切轴干净。

---

## 7. 协议改动清单（字段级）

### 7.1 `NotifyRequest`（api/openapi.yaml）

| 字段 | 改动 |
|---|---|
| `status` | **删除** |
| `agentState` | **新增**，string，enum `[working, blocked, done, interrupted, error]`，**required**（与 title 同级） |
| `severity` | **新增**，string，enum `[info, warning, error]`，optional，缺省由 agentState 派生 |
| `kind` | **新增占位**，string，enum `[task]`，default `task`（阶段二扩 `reply/steer`） |
| `replyTo` | **新增占位**，string，optional（阶段二 kind=reply 时指目标消息 id） |
| 其余（agentId/sessionId/cwd/durationMs/title/body/model/link/deviceTags/priority/ttl） | 保留不动 |

### 7.2 `TestNotifyRequest`（工作台测试通知）

`status` → `agentState`（enum 同上，default `done`）；可选 `severity`。

### 7.3 `broker.Message`（internal/broker/broker.go）

- 删 `Status string`；加 `AgentState string`（json `agentState`）。
- 加 `Severity string`（json `severity,omitempty`）、`Kind string`（json `kind`）、`ReplyTo string`（json `replyTo,omitempty"`）。
- 删常量 StatusSuccess/.../StatusWarning；加 AgentStateWorking/.../AgentStateError 常量 +
  `IsTerminal(state) bool` helper。

### 7.4 store schema（internal/store/schema.sql + 幂等列迁移）

- `messages` 表：`status` 列 → `agent_state`（NOT NULL DEFAULT 'working'）；加 `severity TEXT NOT NULL DEFAULT ''`、`kind TEXT NOT NULL DEFAULT 'task'`、`reply_to TEXT NOT NULL DEFAULT ''`。
- `devices` 表：`status_filter` 列 → `event_scope`（TEXT NOT NULL DEFAULT 'final'）。
  （push 设备默认 final——这是产品负责人的核心诉求落地。）
- 未上线，直接改 schema.sql + 调整 migrateColumns 幂等迁移即可，不留双写兼容。

### 7.5 `Device`（internal/store/devices.go + server/handlers.go PATCH）

- `StatusFilter` → `EventScope string`（json `eventScope`，enum `[final, all]`）。
- `platform`（ios|mac|win|android|other）**保留**——它是 push 通道的投递元数据
  （决定 APNs/FCM 路由与展示），**不参与过滤**，不算"形态入协议"。
- PATCH /v1/devices/{id}：`statusFilter` 字段 → `eventScope`。

### 7.6 WS 协议（internal/ws/protocol.go + handler.go）

- notification 帧：`status` → `agentState`，加 `severity`、`kind`。
- subscribe 帧：加可选 `event_scope` 字段（`final|all`，缺省 `all`）；
  session 增加 scope 过滤（matchTags 旁加 matchScope）。
- subscribed 帧：回显 `event_scope`。

### 7.7 scope（internal/authn）

- 阶段一不变。文档预留 `notify:reply`（阶段二）。

### 7.8 插件仓库（anotify-plugins，跨仓库同步）

- `common/notify.sh`：`--status` → `--agent-state`；加 `--severity`。
- `pi/anotify.ts` 事件映射：
  - `message_end(role=user)` → `agentState=working`
  - `tool_execution_end(isError)` → `agentState=working, severity=error`（在跑但工具红了）
  - `agent_settled` → `agentState=done`

### 7.9 前端（web-src/，一起改，不留旧）

- receivers.html：`statusFilter` 下拉 → `eventScope`（final=只收最终结果 / all=接收全流程）；
  新设备注册默认 `final`。
- 通知列表/详情：按 `agentState` + `severity` 渲染（working 蓝、blocked 黄、done 绿、error 红、interrupted 灰）。
- i18n locales 补 `agentState.*`、`eventScope.*` 词条。

---

## 8. 开放问题（需协调者/产品负责人拍板）

- **Q1 · 彻底删 status？** 我的建议：**删**，由 agentState+severity 完全取代，不留兼容别名
  （未上线，留双字段只会造成"谁权威"的糊涂账）。→ 待确认。
- **Q2 · agentState 定 5 值？** working/blocked/done/interrupted/error。
  备选：砍 interrupted 并入 done（用 severity=warning 表达被打断）。
  我建议保留 interrupted（与旧语义 1:1，迁移无损）。→ 待确认。
- **Q3 · 旧"只收 error"过滤语义废弃？** final 档会同时放行 done+interrupted+error，
  无法表达"只收失败"。我认为这是可接受的语义升级（用户关心的是"终态结果"而非单一语气），
  若将来确需，可在 WS subscribe 加 states 白名单（push 不加）。→ 待确认。
- **Q4 · 轴 A 只给 final/all 两档？** 刻意收窄防过度设计。若将来要"只收 done 不收 interrupted"，
  扩第三档 `minimal` 或开放 states 数组。→ 我建议阶段一只做两档，待确认。
- **Q5 · WS subscribe 帧阶段一就加 eventScope 吗？** 我建议加（协议一次到位，前端立即可用）；
  若想再收窄阶段一范围，也可只做 push 侧 Device.eventScope，WS 默认 all 不可调。→ 待确认。
- **Q6 · severity 命名？** 也可叫 `tone`/`level`。我建议 severity（业界通用，语义准）。→ 轻量确认。

---

## 9. 阶段一边界

### 做

1. NotifyRequest/Message/schema：`status` → `agentState` + `severity`（+kind/replyTo 占位）。
2. Device：`status_filter` → `eventScope`（final|all），push 默认 final。
3. route：ScopeMatch 替代 StatusMatch；WS subscribe 加 eventScope。
4. 前端：receivers 页 + 通知列表按新模型渲染（一起改，不留旧）。
5. 插件仓库 notify.sh / anotify.ts 同步（跨仓库协议变更）。
6. `make e2e` 全绿 + 改 routing/security/api_contract 套件断言。

### 不做（后续阶段）

- ❌ 双向回复 `/v1/reply`、notify:reply scope 实际生效（阶段二；本阶段仅占位 kind/replyTo）。
- ❌ Pet 呈现（阶段三；本阶段 `eventScope=all` + WS 已为它备好数据通道）。
- ❌ herdr 桥接、编排原语（阶段四）。
- ❌ 差异化投递实现（pet 专属协议等）——阶段一所有接收端仍走既有 push/WS 两管道。
- ❌ 任意 states 白名单过滤（过度设计，见 Q4）。

---

## 附：一句话总结

**协议只认三根轴——订阅范围（eventScope：final/all，归订阅）、交互权限（scope：归凭证）、
传输可靠性（归接入路径，不声明）；agent 生命周期用 agentState 五态建模，
error 拆成"终态 error"与"瞬时语气 severity"两层。形态（push/pet/web）永不进协议字段。**
