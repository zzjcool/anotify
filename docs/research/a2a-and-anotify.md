# A2A 与 Anotify：关系调研

> 调研日期：2026-10-28
> 依据：A2A 规范 1.0（仓库最新 release `v1.0.1`，2026-05-28；规范站标注 Latest Released `1.0.0`）
> 本地事实源：`design/wiki/` 十页 + `docs/contracts/CONTRACT.md` + `internal/store/schema.sql`

## 0. 结论先行

1. **A2A 不是编排协议。** 它标准化的是「独立、可能不透明的 agent 之间怎么发现、怎么发任务、怎么收结果」。规范里没有图、没有路由表、没有 fan-out/fan-in、没有租约/心跳。编排明确留给 host/client——A2A 自己的文档也是这么画的：AI assistant 充当 orchestrator。
2. **Anotify 是编排/控制面，A2A 是边界/传输面。** 两者不在同一层，不是替代关系。A2A 给「够得着别的 agent」，Anotify 给「谁在什么时候做什么、为什么、失败了怎么办」。
3. **「动态编排」不能从 A2A 白拿。** A2A 只提供 discovery、multi-turn、task 这些原语；图是否在运行期变化，仍是产品决定。Anotify 现在的「动态」是 **静态拓扑 + 运行期选出口/选节点/重派**；A2A 不改变这一点。
4. **最深的分歧是价值观。** A2A 的核心原则是 *Opaque Execution*；Anotify 的核心卖点是「查表可解释」。接 A2A 等于在边界上接受一个黑盒，必须把可解释性承诺降级为「到 agent 边界为止」。

一句话定位：

> **A2A 负责让 agent 互相够得着；Anotify 负责决定该够谁、按什么顺序、失败了怎么办，并留下可查的记录。**

---

## 1. A2A 1.0 是什么

### 1.1 定位

- 开放标准，Linux Foundation 项目，Google 贡献，Apache 2.0。
- 面向**独立、可能不透明的 agentic 应用**之间的通信与互操作。
- 不是 agent 框架，不是 runtime，不是编排引擎；和 MCP 互补：MCP 管 agent→工具/数据，A2A 管 agent→agent。

### 1.2 规范结构

三层：

1. **数据模型**（`specification/a2a.proto` 是规范源）：`Task`、`TaskStatus`、`Message`、`Part`、`Artifact`、`AgentCard`、`AgentSkill`、`AgentInterface`、`AgentExtension` 等。
2. **抽象操作**：`SendMessage`、`SendStreamingMessage`、`GetTask`、`ListTasks`、`CancelTask`、`SubscribeToTask`、push notification config 的 CRUD、`GetExtendedAgentCard`。
3. **协议绑定**：JSON-RPC、gRPC、HTTP+JSON，以及 custom binding。

### 1.3 关键对象

```text
Task        { id, contextId, status, artifacts[], history[], metadata }
TaskStatus  { state, message?, timestamp? }
Message     { messageId, contextId?, taskId?, role, parts[], metadata?, extensions[], referenceTaskIds[] }
Part        oneof { text | raw | url | data } + metadata/filename/mediaType
Artifact    { artifactId, name?, description?, parts[], metadata?, extensions[] }

AgentCard   { name, description, supportedInterfaces[], provider, version, capabilities,
              securitySchemes, securityRequirements, defaultInput/OutputModes, skills[], signatures[] }
AgentSkill  { id, name, description, tags[], examples[], inputModes[], outputModes[], securityRequirements[] }
AgentInterface { url, protocolBinding, tenant?, protocolVersion }
```

`TaskState`：`SUBMITTED` / `WORKING` / `COMPLETED` / `FAILED` / `CANCELED` / `INPUT_REQUIRED` / `REJECTED` / `AUTH_REQUIRED`。

### 1.4 三个必须记住的「没有」

- **Skill 不可直接调用。** `SendMessage` 只接受 `Message`，没有 skill id 参数；skill 只是 AgentCard 里的发现元数据，具体路由由远端 agent 自己决定。
- **Skill 没有参数 Schema。** `AgentSkill` 只有 id/name/description/tags/examples/inputModes/outputModes/securityRequirements；没有 `params_schema`。参数是否合法只能由远端 agent 自己校验，并以 `INVALID_ARGUMENT` 之类错误返回。
- **没有租约、心跳、attempt、abandoned。** A2A 的任务生命周期由服务端拥有；协议层只有状态与更新投递（polling / SSE / push webhook），没有「派给谁、多久没心跳就回收、换一台重派」这些控制面语义。

### 1.5 发现与更新投递

- 发现：`https://{domain}/.well-known/agent-card.json`、预配置 URL、curated registry。**registry 没有标准 API**；运行时 `QuerySkill()` 仍在「What's next」里。
- 更新：轮询 `GetTask`；SSE `SendStreamingMessage` / `SubscribeToTask`；服务端向客户端注册的 webhook 发 push（at-least-once，客户端要幂等）。
- 多轮：`contextId` 分组，`referenceTaskIds` 引用旧任务，`input-required` / `auth-required` 挂起等人/等授权；任务到终态不可重启，后续只能在同一 `contextId` 开新任务。

### 1.6 扩展机制

- 扩展以 URI 标识，在 AgentCard 的 `capabilities.extensions[]` 声明，客户端通过 `A2A-Extensions` 头选择性启用。
- 可做 data-only、profile（收窄消息形状）、method、state machine 扩展。
- 官方 samples 里已有两个和 Anotify 相关的方向：
  - **AGP（Agent Gateway Protocol）**：按 declared capability 路由 Intent，带 cost/policy，BGP 式分层。最接近 Anotify 的 `edges`，但只是 sample 扩展，没有封闭出口、没有租约、没有 trace。
  - **Traceability Extension**：在 Message/Artifact 的 metadata 里带 `trace_id` + steps。说明「A2A 核心没有 trace 上下文传播」是公认缺口。

---

## 2. Anotify 是什么（对照用）

三层：

```text
能力定义（capability_versions）  name@version + params_schema + outputs[]
        ↓
节点（nodes / node_capabilities） 只声明「我有 review@1」，argv/凭据不出本机
        ↓
出口表（edges）                  (能力, 出口) → 能力，查表是唯一路由依据
```

运行期：

- 派发四步：查 `edges` → 用目标能力 `params_schema` 校验 → 查声明了该能力的节点 → 按负载挑一台（或 sticky pin）。
- `edges` 返回 N 行 = 扇出 N 个任务；0 行 = `pruned/no_edge`。
- 任务有租约：TTL 30s、心跳 10s、ack 20s、`max_attempts` 3、退避 10/60/300s；状态 `pending → assigned → acked → running → succeeded|failed|canceled|abandoned`。
- 节点回报 `result { status, out, summary, artifacts }`；`out` 是出口名或 `null`，与 `status` 正交。
- 环允许，靠 `path`（从根到本任务的能力序列）去重剪枝，剪枝在 trace 上可见。
- 图是显式声明的；没有条件求值、没有优先级、没有运行期改图；收集/join、能力组合、人工审批、跨组织共享都明确不做。

**关键点：Anotify 的「动态」在运行期（选出口、选节点、重派、剪枝），不在拓扑期。** 图本身是静态的、可版本钉住的。

---

## 3. 概念对照

| A2A | Anotify | 关系与差异 |
|---|---|---|
| `AgentCard` | `nodes` + `capability_versions` + tags | 都在广播「我会什么」。A2A 是描述性的、面向人的；Anotify 是规范性的、可校验的。 |
| `AgentSkill` | `capability`（`name@version`） | 名字层面对应，但 A2A skill 没有版本、没有参数 Schema、没有出口集合。A2A 的 `AgentCard.version` 是 agent 版本，不是 skill 版本；无法表达 `review@1` 与 `review@2` 并存。 |
| `SendMessage` + `Message.parts` | 任务载荷 `{capability, version, params}` | A2A 是「意图 + 内容」，由远端 agent 解释；Anotify 是「已定义动作 + 结构化参数」，派发前校验。 |
| `Task` | `tasks` 行 | 同名不同权：A2A task 由服务端生成、服务端拥有、终态不可变；Anotify task 由中枢生成、可重派、可回收。 |
| `contextId` | `trace_id` | 都是分组键。A2A 的 context 是会话性的，服务端生成、对客户端不透明；Anotify 的 trace 是因果性的，中枢生成、可查询。不能假设 `contextId` 能稳定映射到 `trace_id`（客户端不应随意自带 contextId）。 |
| `referenceTaskIds` | `parent_task_id` / `path` | A2A 是客户端给的「相关任务」提示；Anotify 的 `path` 是路由输入，直接参与环剪枝。 |
| `Artifact` | `result.artifacts` + `summary` | A2A artifact 可大可丰富（text/file/data parts）；Anotify result 上限 8KB，偏「指针 + 摘要」。 |
| `TaskStatus.message` / status update | task 状态 + trace span | 都是进度/结果上报；A2A 的粒度是 task 级事件，Anotify 的粒度是任务状态机 + span。 |
| `TaskState` | 任务状态机 | A2A 有 `input-required` / `auth-required` / `rejected`，Anotify 没有；Anotify 有 `abandoned` / `lease_expired`，A2A 没有。映射不是一一对应。 |
| SSE / push webhook | 节点出站 WS + `notify.*` | 方向不同：A2A 服务端要可达（push 时客户端也要可达）；Anotify 节点只出站，不开入站端口。 |
| `AgentInterface.tenant` | `org_id` | 都能做多租户路由。A2A 的 tenant 是不透明字符串；Anotify 的 org 是强隔离边界。 |
| `AgentExtension` | 无对应机制 | A2A 可扩展；Anotify 靠能力定义版本化。A2A 扩展可以承载 Anotify 的契约元数据（见 §6）。 |

---

## 4. 五个硬差异（决定能不能直接用）

### 4.1 粒度：agent 不是 capability

A2A 的可寻址单位是 **agent**，不是 skill。你发一条 Message，远端 agent 自己决定用哪个 skill、内部跑几步。A2A 没有 skill selector，也没有 `params_schema`，更没有「这个 skill 跑完会从哪几个出口之一出去」。

Anotify 的可寻址单位是 **capability 原子**：`review@1` 有明确的参数结构和封闭出口集合。这比 A2A skill 细一层、严一层。

后果：

- 把 A2A agent 当 Anotify 节点，等于把一个「会自己规划的黑盒」塞进一个「要求按契约回报出口」的位置。中间必须有一个适配层，把远端结果翻译成 `out`。
- 翻译方式只有三种：远端按约定返回结构化的 `out`（需要扩展/约定）；按 A2A 终态做固定映射（能力弱，但兼容任意 agent）；或者让 LLM 判断（引入求值器，违反 Anotify 的 I6）。**推荐前两种，第三种应显式做成图里的一个分类能力，而不是藏在适配器里。**

### 4.2 方向：A2A server 要可达，Anotify 节点只出站

- A2A **server** 必须被 client HTTP 访问；push notification 还要求 **client** 也暴露 webhook。
- Anotify 节点是出站 WebSocket，不开放入站端口；凭据和 argv 不出本机。

后果：

- 让每个 Anotify 节点变成 A2A server，会直接破坏「不开入站端口」这条安全边界。
- 反过来，**Anotify 节点做 A2A client 是可行的**：`SendMessage` / `SubscribeToTask` 都是出站请求，节点用 polling/SSE 收结果即可，不需要入站（除非要用 push）。
- 中枢做 A2A server（ingress）也是可行的，但中枢会变成公网 API，需要新的认证、限流、租户映射和 SSRF/滥用防护。

### 4.3 生命周期：没有租约，任务属于远端

- A2A 没有 lease/heartbeat/attempt；task 由服务端生成、服务端推进、终态不可变。
- Anotify 的 task 是调度单元，可以重派、回收、标 `abandoned`，而且 `out` 缺失 != `out: null`。

后果：

- 一个 Anotify task 调用一个 A2A agent 时，实际有 **两个 task**：Anotify 的调度 task 和远端的 A2A task。不要复用同一张 `tasks` 表，建议单独 `remote_tasks`（`task_id`、`a2a_task_id`、`context_id`、`last_state`、`artifacts`、`last_polled_at`）。
- Anotify 重派 = 新建一个远端 A2A task（客户端不能指定 taskId），用 `referenceTaskIds` 关联上一次；不能假设远端 task 可重启。
- A2A 没有 `idempotent` 声明。默认应当把远端调用视为「有副作用」，重派前必须有人/策略确认；这与 wiki 里 `idempotent: false` 的默认值一致。
- 远端失联没有协议级检测；Anotify 需要自己的 watchdog（`max_duration` + 轮询/流断开策略），并把「远端状态未知」显式记录，不要伪造成 failed/completed。

### 4.4 可解释性：opaque 对上 table-lookup

A2A 的指导原则是 **Opaque Execution**：agent 之间只交换声明的能力和结果，不共享内部计划/工具/记忆。Anotify 的卖点是「为什么没触发」可以靠查表回答。

后果：

- 一旦调用 A2A agent，trace 只能解释到 **agent 边界**：发了什么 Message、拿到什么 Task 状态/Artifact、什么时候超时。里面为什么走那条路，拿不到。
- A2A 核心规范没有定义 trace 上下文传播；只有 samples 里的 Traceability Extension（metadata 里带 trace_id + steps）。要用就得双方都支持，否则 Anotify 的 trace 在边界处断掉。
- 产品话术必须改：不是「端到端可解释」，而是「编排层可解释；被编排的 agent 内部按其自身能力披露」。

### 4.5 动态性：A2A 没有图，也没有 planner

A2A 提供的是：

- 发现（AgentCard / registry）
- 多轮（`contextId`、`input-required`、`referenceTaskIds`）
- 并行 follow-up（同一 context 下多个 task）
- 长任务（streaming / push）

它不提供：

- 图/拓扑
- 条件路由、fan-out/fan-in、join
- 重试/租约/优先级
- 运行期改图
- 规划器（planner）

A2A 文档里的 orchestrator 是 **host/client 自己**，不是协议的一部分。所以「动态编排」这件事，A2A 只给了叶子节点的调用方式，没给调度算法。

---

## 5. 「动态编排」到底指哪一层？

先把词拆开，否则会拿 A2A 去解一个它没打算解的问题。

| 层次 | 含义 | Anotify 现在 | A2A 能帮吗 |
|---|---|---|---|
| **D1 选机器** | 同一个能力派给哪台节点/实例 | 有：负载、sticky pin、租约重派 | 不能。A2A 的服务端就是 agent 本身，没有节点池概念 |
| **D2 选出口** | 跑完后走哪个分支 | 有：`result.out` + `edges` 查表 | 只能间接：要求远端在 Artifact/DataPart 里返回 `out`，或用终态映射 |
| **D3 扇出宽度** | 一次派发几个下游 | 静态：`edges` 有几行就几个；动态扇出仅预留 | 不能。A2A 没有边表，也没有「按结果决定扇出」的语义 |
| **D4 运行期拓扑** | 跑着跑着改图 / planner 决定下一步 | **明确不做**（`edges` 变更要新版本） | 不能。A2A 明确不管编排 |
| **D5 发现新 agent** | 运行时发现「还有谁会做这个」 | 节点注册时声明能力，白名单校验 | **能**：AgentCard / registry / well-known；但 registry 无标准 API，QuerySkill 仍是未来项 |

所以：

- 如果「动态编排」指 **D1 + D2**（运行期做选择，但拓扑静态），Anotify 已经是了；A2A 只是让叶子可以是第三方 agent（D5）。
- 如果指 **D3**（运行期决定扇出几个），A2A 帮不上；需要在 Anotify 里做「结果携带派生目标 + 白名单校验 + 建任务」，这可以做到不违反 I2/I6，但会引入新的状态机。
- 如果指 **D4**（LLM/规则在运行期决定图），那是另一个产品：需要 planner、join、挂起/恢复、幂等、预算/终止条件。A2A 不提供这些，只提供叶子调用。wiki 现在明确拒绝 D4，这是有意的产品决定，不是技术债。

---

## 6. 四种接入姿势

### 姿势 A：A2A ingress —— 中枢对外做 A2A server（推荐先做）

把 Anotify 暴露成一个 A2A agent，让任何 A2A client 能触发一条 trace。

- AgentCard 的 skills = **入口能力**（没有上游边的能力），或一个受控的 `run` skill；不要把全部能力都暴露，否则外部可以从图中间开始。
- `SendMessage` → 写 `events(source=a2a)` → 走现有派发；`contextId` 由 Anotify 生成并映射 `trace_id`（Anotify 是 server，有权生成）；`messageId` 做幂等键。
- 结果：Anotify task 状态 → A2A `TaskStatus`；`result.artifacts` → A2A `Artifact`（注意 8KB 上限，大文件走 URL/下载端点）。
- 状态映射建议：
  - `pending/assigned/acked/running` → `WORKING`（或 `SUBMITTED`）
  - `succeeded` → `COMPLETED`
  - `failed` → `FAILED`
  - `canceled` → `CANCELED`
  - `abandoned` → `FAILED` + message（不要伪装成 `CANCELED`）
  - `no_edge` / `no_node` / `param_schema` → `REJECTED` + 结构化原因（这是 Anotify 的强项，别丢掉）
  - `input-required` / `auth-required`：本期没有挂起态，明确返回 `FAILED`/`REJECTED` 并说明；不要假装支持。
- 多租户：一个 org 一个 AgentCard，或一个 endpoint + `tenant=org_id`；鉴权用 Bearer/API key/OAuth，不要复用节点 token。
- push notification：中枢可达，可以做；也可以只支持 polling/SSE，先降低面积。

**收益**：立刻进入 A2A 生态，外部 agent 可以把 Anotify 当「一个会按图执行的 agent」；Anotify 内部仍然完全可控。
**代价**：中枢变成公网入口，需要新的安全面；A2A 客户端可能发来不符合 `params_schema` 的 Message，必须在 ingress 就拒绝。

### 姿势 B：A2A egress —— 把远端 agent 当成一种能力实现（推荐第二步）

新增一个能力（例如 `a2a.call@1`），由节点实现：节点作为 A2A client，把 `params` 转成 `Message.parts`，调用远端 agent，收 `Task`/`Artifact`，翻译成 `result {status, out}`。

两种建模方式：

1. **通用能力 + endpoint 在 params 里**：`a2a.call@1` 的 `params_schema` 包含 `agent_url`、`parts` 等。灵活，但图里看不出「调的是哪个 agent」，而且任意 URL 会扩大 SSRF 面。
2. **每个远端 agent 一个能力**（推荐）：`vendor.review@1`，节点本地配置把「能力名@版本 → A2A endpoint + 凭据」映射起来，`argv` 位置换成 adapter 配置。图仍然只认能力，凭据不出本机，`edges` 可读。这与 wiki 的「中枢知道接口，不知道实现」完全一致。

出口翻译（按优先级）：

1. **约定结构化出口**：远端在 Artifact/DataPart 里返回 `{"out": "fix"}`，Anotify 用目标能力的 `outputs` 校验；不合法 → `out: null` + `reason=invalid_out`。
2. **终态映射**：`COMPLETED → 成功出口`、`FAILED/CANCELED/REJECTED → 失败出口`、`INPUT_REQUIRED/AUTH_REQUIRED → 无出口 + 通知`。映射规则写在能力定义或节点本地配置里，必须显式、可审计。
3. **不要**在适配器里塞 LLM 判断。需要分类就定义一个 `classify@1` 能力，让它在图里可见。

可靠性：

- `remote_tasks` 表 + watchdog；流断/轮询失败 → `unknown`，不是 `failed`。
- 远端没有 idempotent 声明，默认不自动重派；重派 = 新 A2A task + `referenceTaskIds`。
- trace：在 HTTP header/metadata 里带 W3C `traceparent`，并记录远端 `taskId`/`contextId`；边界内不承诺。

**收益**：Anotify 可以编排任意 A2A agent，而不用把节点协议改成 A2A；节点仍然只出站。
**代价**：多一层适配与状态映射；可解释性在边界处降级；远端行为不可控。

### 姿势 C：把节点协议换成 A2A（不建议）

理由：

- A2A server 要入站，破坏「节点不开端口」。
- 没有 lease/ack/heartbeat/attempt，Anotify 的可靠性模型要全部重建。
- 没有 `params_schema`、没有封闭 `outputs`，I2/I6 的静态可分析性丢失。
- 没有节点池/并发槽位/粘性路由。

除非产品彻底转向「编排第三方 agent，而不是执行本机命令」，否则不值得。

### 姿势 D：把 Anotify 定位成 A2A 之上的编排层 / 贡献 A2A 扩展（长期）

- 叙事：A2A 给 reach，Anotify 给 control + accountability。
- 可以把「能力契约 + 出口 + 版本」做成一个 A2A extension/profile，让 AgentCard 携带 Anotify 兼容的路由元数据；这样 A2A 生态里的 agent 可以被 Anotify 直接编进图。
- 先例：AGP sample 已经在做「按 declared capability 路由 + policy/cost」；但它没有封闭出口、租约、trace。你们的差异化是确定性、可解释、可靠性，不是「又一个 gateway」。
- 这条路是生态/标准化长线，不适合作为近期交付。

---

## 7. 如果要做，最小落地（建议的 spike）

目标：验证「A2A 边界」而不改内核。

1. **Ingress**：中枢加一个 A2A server facade（先 HTTP+JSON 或 JSON-RPC 一种绑定），只暴露 1 个入口能力；AgentCard 手写/生成；`SendMessage` → event；`GetTask` 映射状态。
2. **Egress**：一个节点适配器，实现 `vendor.review@1`（或 `a2a.call@1`），调用一个本地/sample A2A agent；优先走 SSE/polling，不用 push。
3. **出口约定**：sample agent 返回 DataPart `{"out": "..."}`；同时实现终态映射兜底。
4. **`remote_tasks`**：记录远端 task/context/state/artifacts；watchdog 标 `unknown`。
5. **Trace**：`traceparent` + 远端 task id；trace 页面明确标「边界」。
6. **E2E**：A2A client → Anotify → 远端 agent → `notify.phone`，跑通一条；再跑一条远端失败的，验证 `out` 缺失/拒绝路径。

这个 spike 能回答：状态映射是否够用、`out` 约定是否可推广、trace 边界怎么展示、安全面（公网 ingress + 出站 allowlist）是否可控。
它不能回答：动态拓扑（D4）——那需要单独设计。

---

## 8. 需要产品决策的开放问题

1. 你说的「动态编排」是 D1–D2（运行期选择，拓扑静态），还是 D3（动态扇出），还是 D4（运行期改图/planner）？
2. 接受「可解释性到 agent 边界为止」吗？如果不接受，A2A egress 只能用于可信/自建 agent。
3. 要不要做公网 A2A ingress？这会把中枢变成公开 API，安全模型要重写一版。
4. 多租户用「一 org 一 AgentCard」还是「单 endpoint + tenant」？
5. 远端调用的凭据放哪？按 wiki 的边界应放节点本地；那 `edges` 里就只看到能力名，看不到远端 URL——这是可接受的抽象，还是会让「为什么走了这个 agent」变得不透明？
6. 如果要做 D3/D4，`join`/挂起/预算/终止条件怎么设计？A2A 的 `input-required` 只是提示，不是答案。

---

## 附录：引用

- A2A 规范（Latest 1.0.0）：https://a2a-protocol.org/latest/specification/
- A2A 规范源（proto，规范数据模型唯一权威）：https://github.com/a2aproject/A2A/blob/main/specification/a2a.proto
- A2A 仓库 / Release：https://github.com/a2aproject/A2A/releases
- What is A2A（含 orchestrator / agent stack / MCP 对比）：https://github.com/a2aproject/A2A/blob/main/docs/topics/what-is-a2a.md
- Life of a Task（contextId、input-required、task immutability、parallel follow-ups）：https://github.com/a2aproject/A2A/blob/main/docs/topics/life-of-a-task.md
- Agent Discovery（well-known、registry 无标准 API）：https://github.com/a2aproject/A2A/blob/main/docs/topics/agent-discovery.md
- Streaming & Async（SSE、push、无租约）：https://github.com/a2aproject/A2A/blob/main/docs/topics/streaming-and-async.md
- Multi-Tenancy（tenant 字段）：https://github.com/a2aproject/A2A/blob/main/docs/topics/multi-tenancy.md
- Extensions（URI、AgentCard 声明、profile/method/state machine）：https://github.com/a2aproject/A2A/blob/main/docs/topics/extensions.md
- AGP sample extension（capability 路由 / policy / cost）：https://github.com/a2aproject/a2a-samples/tree/main/extensions/agp
- Traceability Extension sample：https://github.com/a2aproject/a2a-samples/tree/main/extensions/traceability
- Anotify 设计事实源：`design/wiki/index.html` 起十页；实现契约：`docs/contracts/CONTRACT.md`
