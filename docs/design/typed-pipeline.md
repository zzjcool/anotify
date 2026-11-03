# 能力即函数：emit、link 与执行

> 状态：**冻结**（2026-11，第二轮重排；六轮讨论的收敛结果，见 §12 已否决方案）
> 定位：本文件冻结**数据流模型**。`docs/contracts/CONTRACT.md` 是路由/协议权威，冲突时以本文件为准的部分会显式标注（见 §5.1）。
> 读者：实现这个模型的人。**先读 §0 与 §1，其余按需**。

---

## 0. 一句话

> **能力是一个会发事件的进程：`params → emit(port, data)* → exit`。link 只决定「某个 port 的去向」，不参与拼参数。**

全模型只有四样东西：**能力**（入参 + 端口）、**link**（端口 → 下游，带可选字面量）、**emit**（唯一的产出通道）、**digest**（记账）。其余全是推导量。

---

## 1. 三层

| 层 | 是什么 | 什么时候定 | 谁校验 |
|---|---|---|---|
| **能力层** | `params_schema`（入）+ `ports[]`（出，**只有名字，没有 schema**） | 定义/部署时 | 保存时拒（格式） |
| **link 层** | `(from, port) → to` + 可选字面量覆盖 | 配置时 | 保存时硬拒 |
| **执行层** | 任务：钉 `(name@version, digest)`，运行中 emit 0..N 次 | 派发/运行时 | 投递时（结构）+ 派发时（契约） |

### 1.1 名词（唯一术语）

| 名词 | 定义 |
|---|---|
| **能力 (unit)** | `name@version` 的一份定义 = **契约 + 实现 + digest**。触发器是它的一种 |
| **契约** | `params_schema` + `ports[]`。随实现下发（在产物里），不是中枢的独立一份 |
| **端口 (port)** | 出口名，能力自己声明的字符串（`fix` / `push` / `partial`…）。**没有类型、没有 schema** |
| **emit** | 运行中上报一次：`emit(port, data)`。**唯一的产出通道** |
| **link** | `(from, port) → to` + 可选字面量覆盖。**接线引用 name（默认）或 name@version** |
| **下游目录 (catalog)** | 派发时下发给实现的「你的这个 port 能去哪些能力、它们各要什么参数」 |
| **digest** | 契约+实现的 sha256。系统自动算，用户不写 |
| **trigger** | 源。它是能力，`result_schema` 就是事件 payload 契约（外部边界，必须声明） |

### 1.2 与函数类比

| 函数世界 | 这里 |
|---|---|
| `f : A → B` | `params_schema → emit(port, data)`，`B` 由 link 决定，不由 f 声明 |
| 联合返回 | `ports`：一次运行可以走多个端口（动态扇出），每个端口可走多次 |
| 参数解构 | **不存在**——data 直接就是下游的 params（要字面量就在 link 上写） |
| 调用同步 | emit 阻塞到 ack；但整条链是异步派发 |
| 一个调用一份返回值 | **0..N 次 emit**，每次都独立路由、独立剪枝 |
| 栈帧 | `trace_id` + `parent_task_id` + `path`（深度/环保护） |
| 返回值即状态码 | **状态来自进程**（退出码/信号），不来自端口 |

### 1.3 生命周期与状态

```text
assign → ack → 进程启动 → emit* → 进程退出
                                   ├─ exit 0 且无 emit  → 正常终止（没有下游，不是错误）
                                   ├─ exit 0 且有 emit  → 正常完成（emit 已各自派发）
                                   └─ exit !=0 / 被杀   → 任务 failed（summary 来自 stderr 尾部）
```

- **状态 = 进程事实**。脚本/LLM 进程死、agent 死、静默完成，都是可确定的（退出码 / 连接断开 / 租约到期）。
- **路由 = emit 的 port**，与状态无关（I4 保留其形式：失败前 emit 过 `failed` 端口，链路照样继续）。
- 端口名**没有保留字**（`ok`/`failed` 不是系统语义，只是恰好常用的业务名）。

---

## 2. emit：唯一的产出通道

### 2.1 CLI 复用 `anotify-agent`

`cmd/agent` 已经是一个静态二进制、已经在节点上、已经会讲 WS 协议（assign/ack/result），并有 `run` 子命令的先例。新增一个子命令：

```bash
anotify-agent emit --port fix --data '{"issue":"#42 空指针","patch":"...","repo":"acme/x"}'
```

```text
能力进程（脚本 / LLM agent）
  → anotify-agent emit ...            ← 环境变量提供 socket 与任务身份
  → unix socket（父子进程，agent 持有）
  → agent：补 seq、验 port ∈ ports、验结构、转发 emit 帧
  → 中枢 ack（落库 + 派发）
  → CLI 返回 0（失败返回非 0，实现应当停止或重试）
```

**三条硬要求**：

1. **任务令牌**：`ANOTIFY_EMIT_SOCK` / `ANOTIFY_TASK_ID` / `ANOTIFY_TASK_TOKEN` 由 agent 注入。一个任务的脚本不能替别的任务 emit。
2. **端口白名单**：`port ∈ ports[]`（I2 的封闭集合，提前到 emit 时判——比等进程退出才发现早得多）。
3. **阻塞到 ack**（默认 5s 超时）。理由：emit 是有副作用的动作，fire-and-forget 会让“emit 成功但中枢没收到”变成静默丢事件。`--async` 是显式逃生口，不是默认。

### 2.2 幂等与顺序

- 每个 emit 带 `seq`（agent 为该任务单调递增）。
- 中枢按 `(task_id, seq)` 唯一约束去重：重试不会重复派发下游。
- 断线重连后 agent 重发未 ack 的 emit；中枢只接受 `seq` 连续递增的一段（缺口 → 明确报错，不猜测）。

### 2.3 两层校验，别混

| 层 | 校验什么 | 失败表现 |
|---|---|---|
| **节点（投递时）** | `port ∈ ports[]`、data 是合法 JSON 对象、体积 ≤ 上限 | **拒绝本次 emit**（CLI 非 0），实现可当场修正重试 |
| **中枢（派发时）** | 每条 link 的目标 `params_schema`（复用 `ValidateParams`） | 该 link 剪枝（`param_schema` / `param_missing`），**其他 link 不受影响**（扇出可部分成功） |

**节点不做契约级校验**，因为没有“端口 schema”这种东西可校验（§4.3）。它只做结构校验，并把「下游要什么」以**目录**形式交给实现，让实现自己拼对。

> 为什么不把下游需求做成强校验：一个 port 接 3 个下游时，需要的字段是 3 条的并集，而扇出允许“1 条过、2 条剪”。在节点上拿并集当硬门槛，会把“部分成功”变成“整体失败”——这一点在 §12 记过一次。

### 2.4 上限（写入契约，避免以后拍脑袋）

| 项 | 建议值 | 超限行为 |
|---|---|---|
| 单次 emit 的 data | 64 KiB | 拒绝（不截断：下游拿到半截 JSON 比失败更坏） |
| 每个任务 emit 次数 | 1000 | 拒绝（防跑飞的循环） |
| 单次 emit ack 超时 | 5s | CLI 返回非 0 |

---

## 3. link

### 3.1 表

```sql
link(
  ns_id, from_name, port, to_name,
  literals TEXT NOT NULL DEFAULT '{}'   -- 可选的字面量覆盖：{"repo":"acme/x"}
  -- 不存版本：link 是配置，改它不该产生新版本
)
```

- `from_name` / `to_name` 是**名字**，不是 id。
- 同一 `(from_name, port)` 可有多行 → **静态 1:N 扇出**。
- **没有引用映射**（`{{result.x}}` 这类）：参数怎么拼是实现的内部事务。link 只提供连接与少量常量。

### 3.2 引用写法（挂哪一版）

```text
analyze            跟随 latest（默认）—— 升级自动生效
analyze@1          钉死版本 —— 渐进发布 / 隔离 / 回滚
```

UI 对 `name` 型 link 显示「当前解析到 `analyze@3`，已有 `@4`」，让“跟随”是可见的。

### 3.3 字面量覆盖

```text
link: analyze --fix--> notify
      literals = {"to": "u_123"}        ← 常量在配置里，不进脚本（否则改个收件人要重新部署节点）
下游实际 params = data ⊎ literals        ← 字面量优先；所以 data 里不必带 to
```

推导规则：进入下游的 params = `emit.data` 叠加 `literals`（字面量覆盖同名键）。**不做类型转换**，因此 `data` 里同名字段的类型必须与下游 `params_schema` 一致（否则派发时 `param_type` 剪枝）。

### 3.4 下游目录（catalog）

派发时随 `assign` 下发，让实现知道“我这个 port 能去哪、对面要什么”：

```jsonc
"catalog": {
  "fix":  [ {"capability":"fixer",  "version":3, "params": {…下游 params_schema…}},
            {"capability":"notify", "version":1, "params": {…}} ],
  "push": [ {"capability":"pusher", "version":1, "params": {…}} ]
}
```

约束：只含**本能力的出边**（不含全图）；只用于让实现拼对参数，**不能改变路由**（emit 只能报 port，report 哪条 link 由中枢按库查）。

> 这一条把“节点不持拓扑”从“什么都不知道”放宽为“知道自己的下游”。这是设计的自觉让步：没有它，LLM 无法拼出正确的 params；有了它，节点知道的仍然只是“我这个出口通向谁”，改不了别人的路由。

### 3.5 归属

**link 由中枢（控制台）定义与管辖，节点不参与。**

1. **拓扑是配置，不是基础设施**：link 由节点声明，加一台机器就会改变链路。
2. **同一能力常被多台机器声明**：各机自己声明出边，两台机器的不一致**没有任何机制能发现**。
3. **校验需要全图视野**：环检测、同名不同型、升级影响面。
4. **权限与隔离**：link 是命名空间内配置（I11），节点没有用户身份。

编辑入口内联在能力详情页（「端口与 link」区），全局页只做只读总览。`manifest`（hello 帧）只放执行环境（runner/timeout/concurrency/env），**永不生长拓扑字段**。

### 3.6 升级闸门

新版本成为 latest 时，比对上一版与新版契约：

| 变化 | 处置 |
|---|---|
| 加可选入参 / 加 port | 直接生效 |
| 删 port / 改入参类型 / 加必填 | **要求确认**，列出受影响的 link（含“哪些实现会拿不到它要的字段”）；确认前 `name` 型 link 继续解析到旧版本 |

---

## 4. 触发器（边界）

### 4.1 模型

```text
trigger = unit(kind=trigger)
  params_schema  = 无
  ports          = 无（它的"端口"是事件类型）
  result_schema  = 事件 payload 契约（**必须声明**）
```

### 4.2 入口校验

```text
外部 POST → 落库 events（先保住数据）
  ↓ 按 trigger.result_schema 校验
  ├─ 合规 → 派发
  └─ 不合规 → 不派发 + prune(event_schema)，trace 可见
```

**不能直接 400 丢弃**：外部系统不重试，丢了就没了。

### 4.3 为什么能力没有端口 schema

- 一个 port 可以接 N 个下游（扇出），"这个 port 的 schema"没有唯一解。
- 扇出允许部分成功，所以“并集”不能当硬门槛（§12 记过一次）。
- 因此 schema 只存在于两处：**能力入参**（`params_schema`）与**触发器出参**（`result_schema`）。中间不声明。

### 4.4 门槛怎么降：采样生成草稿

generic webhook 建源时「抓一条真实请求」→ 采样请求体 → 生成 schema 草稿（逐字段确认类型）→ 存为 `result_schema`。内置触发器（github 等）预置。**不允许 `{}`**（否则第一跳退化成运行期类型赌博）。

---

## 5. 校验矩阵

| 时机 | 检查 | 失败表现 |
|---|---|---|
| **定义期**（存产物） | `params_schema` 合法、`ports` 名字合法且不重复 | 硬拒 |
| **定义期**（存 link） | `port ∈ from.ports`；`to` 存在；`literals` 的键 ∈ `to.params_schema` 且类型相等；**同一 port 的多个下游要求同一字段不同类型 → 拒**；静态可达环 | 硬拒，一次报全部问题 |
| **入口** | payload ⊨ `trigger.result_schema` | 不派发 + `prune(event_schema)`，事件仍落库 |
| **投递**（节点） | `port ∈ ports`、JSON 对象、体积/次数上限 | 拒绝本次 emit（CLI 非 0） |
| **派发**（中枢） | 每条 link：`data ⊎ literals` ⊨ 目标 `params_schema` | 该 link 剪枝；**其他 link 不受影响** |
| **审计** | 产物 digest 与本地内容一致 | 标「本地变体」，不静默使用 |

`literals` 之所以能在定义期校验类型：它是常量，不依赖运行时。

---

## 6. 数据模型与协议（改动清单）

| 对象 | 改动 |
|---|---|
| `capability_versions` | `outputs` 保留（端口名集合）；**新增 `params_schema` 已有，无需 `result_schema`**；`kind` 仍为推导量（I3） |
| `edges` → `link` | 加 `literals`；引用从 `capability_id` 改为 `from_name`/`to_name`；删 `from_is_source`（P2） |
| `emissions`（新） | `(task_id, seq, port, data, routed_at)`，`UNIQUE(task_id, seq)` |
| `tasks` | 加 `digest`；`out`/`reported` 语义退役（emit 取代） |
| `events` | 不变（`payload` 原样存） |
| `sources` / `source_targets` | P2 收敛为 `trigger` 能力 + `link` |
| `artifacts` | 已有 `(ns, digest, spec)`；`spec` 保留 `params`/`outputs`/`requires`/`run` |
| 帧：`assign` | 加 `digest`、`ports`、`catalog` |
| 帧：`emit` / `emit_ack`（新） | `emit{task_id, seq, port, data}`；`emit_ack{task_id, seq}` |
| 帧：`result` | 只保留 `status` / `summary` / `error` / `artifacts`；**`out` 退役** |
| `progress` 帧 | 并入 emit（`--note` 走日志通道，不参与路由） |

### 6.1 对 CONTRACT 的影响（必须同步）

| 不变量 | 现状 | 之后 |
|---|---|---|
| **I2**（出口封闭） | `from_port ∈ outputs`（建边时） | `port ∈ ports`（保存时 + **每次 emit 时**） |
| **I4**（路由只看 out） | `out` 单值 | **路由只看 emit 的 port，不看 status**（形式不变，载体变） |
| **I5**（out 两态） | 有值/缺省 | **没有 emit = 到此为止**（0 次 emit 合法） |
| **I6**（不求值） | 模板单趟替换 | 保持不变；**mapping 整体消失**，`{{}}` 只留在触发器边界（事件 → 第一跳入参） |
| **I8**（版本不可变） | 不变 | 不变 |

---

## 7. 落地分期

### P0 · 记账与边界（纯增量，互不阻塞）

| # | 改动 |
|---|---|
| P0.1 | `tasks.digest` + `assign.digest`；节点优先 `<artifacts>/<digest>`，缺失即可见失败（缓存索引已是 `name@version → digest`） |
| P0.2 | `trigger.result_schema` + 入口校验 + `prune(event_schema)` + 采样草稿 |
| P0.3 | 保存 link 时硬拒 `literals` 的类型错、`port ∉ ports`、同名不同型 |

### P1 · emit（本轮最大一块，独立成期）

| # | 改动 |
|---|---|
| P1.1 | `emit` / `emit_ack` 帧 + `emissions` 表 + `(task_id, seq)` 幂等 + 与派发同事务（**复用 #140 的接力原子性**，否则“丢一次”变成“丢 N 次”） |
| P1.2 | agent 本地 socket + `anotify-agent emit` 子命令 + 任务令牌 + 端口白名单 + 结构校验 + 阻塞 ack |
| P1.3 | 派发：`data ⊎ literals` → 每条 link `ValidateParams` → 扇出（部分成功） |
| P1.4 | `assign.catalog`（本能力出边 + 目标 params_schema）下发 |
| P1.5 | 废弃 `.anotify-out` / `.anotify-result`：旧产物在退出时**合成一次 emit**（一次性兼容层，标注 deprecated，下个大版本删） |
| P1.6 | 前端：trace 从“链”变“事件流”（每次 emit 的 port + 时间 + 下游）；能力详情页显示端口与 link |

> P1.5 是唯一保留的兼容层；其余一律直接切。

### P2 · 统一（破坏性，必须一起做）

| # | 改动 |
|---|---|
| P2.1 | 源 → `kind=trigger` 的能力；删 `from_is_source` / `source_targets` / `source_target_params` / `DispatchInput.Targets` 旁路 |
| P2.2 | `edges` → `link`（名字引用）；`capability.id` 与 `content_hash` 解耦（id 稳定，hash 只寻址） |
| P2.3 | 静态环检测（运行期 `path` 剪枝降级为兜底） |
| P2.4 | 契约进签名（`ports` / `params` 参与 `content_hash`）—— 只在 P2.2 之后 |

### P3 · 可选
字段字典报告（推导量、只报告）；嵌套字段表单；`catalog` 里附带“最近实际被消费的字段”观测。

---

## 8. 不做的事

| 不做 | 理由 |
|---|---|
| 端口 schema / 端口实体 / 外键 | 一个 port 接 N 个下游时没有唯一 schema；只当名字的实体就是名字（§4.3） |
| link 上的引用映射 `{{result.x}}` | 参数怎么拼是实现的内部事务；搬运层只制造 `param_missing` |
| `as` / 自动类型转换 | 会失败的转换 = 晚发现的错；名即类型 |
| 能力上声明 `result_schema` | 第二份契约，必然与下游漂移；schema 只在入参（能力）与事件（触发器） |
| 跨跳上下文（`steps[]` / K 跳窗口） | 窄接口：下游只依赖直接上游 |
| 保留端口名 `ok`/`failed` | 状态来自进程生命周期，端口只表达业务去向 |
| 让节点声明 link / 掌握拓扑 | 拓扑会随机器变化；多机不一致不可发现 |
| fire-and-forget 的 emit | 静默丢事件，是这套系统一直在消灭的失败 |

---

## 9. 验收

### 9.1 单测
- link 保存：`port ∉ ports` / `literals` 类型错 / 同名不同型 / 静态环 → 各一条错误码
- emit：`seq` 重复幂等、缺口报错、`port ∉ ports` 拒绝、data 非对象拒绝、超体积/超次数拒绝
- 派发：`data ⊎ literals` 合并（字面量优先）、一条 link 剪枝不影响另一条（部分成功）
- 生命周期：exit 0 无 emit / exit 0 有 emit / exit≠0 前 emit 过 / 被 kill / 租约到期 → 五种状态各一条
- 入口：payload ⊨/⊭ trigger schema → 派发 / `prune(event_schema)`
- 记账：assign 写 `(version, digest)`；节点缺 digest → 可见失败

### 9.2 e2e
- `ws_protocol`：emit/ack/seq/重连重发全链
- `routing`：一次运行 emit 3 次（同 port 两次、另一 port 一次）→ 下游任务数与剪枝正确
- `real_backend`：能力脚本调用 `anotify-agent emit` 成功；exit≠0 前 emit 仍派发
- `frontend`：trace 事件流渲染；能力详情页的端口与 link 编辑
- 全量 `make e2e` + `go test ./...` + `make contract` 绿

### 9.3 巡检
- `emissions` 缺口（同一 task 的 seq 不连续）
- 未 ack 的 emit 数、被拒的 emit 数（按原因）
- `tasks.digest` 非空率、本地变体数

---

## 10. 决策记录

| # | 决策 | 理由 |
|---|---|---|
| 1 | 产出通道唯一：`emit(port, data)` | 一次性返回只是“只 emit 一次”的特例；两套协议必然分叉 |
| 2 | 端口只有名字，没有 schema | 扇出下没有唯一 schema；并集当门槛会把部分成功变成整体失败（§12） |
| 3 | 参数怎么拼是实现的内部事务 | 所以 link 没有引用映射，只有连接 + 字面量 |
| 4 | 校验两层：节点结构、中枢契约 | 节点能当场拒的只有结构；契约的判断需要下游视野 |
| 5 | 状态来自进程生命周期 | 退出码/信号/租约都是确定事实，不需要端口承担状态语义 |
| 6 | 端口无保留字 | 业务语义（成功/失败）由实现自己用端口表达 |
| 7 | emit 阻塞到 ack | 有副作用的动作不能 fire-and-forget |
| 8 | link 归中枢，节点只拿 catalog | 拓扑不能随机器变化；但实现必须知道下游要什么 |
| 9 | 引用 `name`（默认）/ `name@version`（钉死） | 自动跟随与隔离共存 |
| 10 | 任务钉 `name@version` + `digest` | 名字解决“要最新”，摘要解决“证明当时是那份” |
| 11 | 触发器必须声明 `result_schema` | 外部边界只能声明，不能推导 |
| 12 | 废弃 `.anotify-out` / `.anotify-result` | emit 取代；保留一次性兼容层（P1.5） |
| 13 | 1:N 静态扇出保留 | 已是产品能力；与动态 emit 叠加，不互斥 |
| 14 | 契约随实现下发（产物里） | 节点要用它；契约与实现分家就漂移 |

---

## 11. 未决

1. **P2 的破坏性迁移窗口**：`capability.id` 与 `content_hash` 解耦，需要停机还是双写过渡。
2. **历史可解释性的内容保留**：digest 钉住了内容身份，但产物本身要保留多久（现 `retention` 只管事件/任务）。
3. **`catalog` 体积与泄露边界**：下游多时它会变大；是否需要“只给被 emit 过的 port 补齐”（懒加载）。
4. **emit 的速率上限**（QoS）：单任务每秒 1000 次的限额要不要做，还是只做总数上限。

---

## 12. 已否决的方案（附原因，防止回头）

> 六轮讨论里被砍掉的形状。留在这里，是因为每一个都曾被认真考虑过，且都能重新长出来。

| 方案 | 为什么否 |
|---|---|
| 出口做成**实体/表**（`option`、`capability_ports`、外键、软删） | 1:N 下实体的唯一职责是“名字”，而只当名字的实体就是名字。收益（改名/统计）用一张常量表就能拿到，成本（作用域、播种、引用计数、删除语义）是永久的 |
| 能力上**独立声明 `result_schema`**（作必需契约） | 第二份契约，必然与下游 `params_schema` 漂移；且 1:N 时“我的产出”没有唯一消费者 |
| **`next(schema)`**：能力声明“下一跳的 schema” | 1:N 下没有唯一解：一个 port 接 c 和 d，c 要 `{lines}`、d 要 `{text}` |
| **`union` 推导的 `expect` 当整体校验** | 把扇出的“部分成功”变成“整体失败”，与 `dispatch.go` 既有语义冲突 |
| **mapping 的引用映射**（`{{result.x}}`、`as`、自动推断） | 参数怎么拼是实现的内部事务；搬运层只制造晚发现的 `param_missing` / `param_type` |
| **跨跳上下文**（`steps[]`、K 跳窗口、每跳 8 KiB 预算） | 窄接口足够：下游只依赖直接上游；`event` 只读旁路就够 |
| **可变 payload**（每跳改写往下传） | 重放不确定；与“原始事件不可变”冲突 |
| **共享端口自带下游** / 共享 option | 与“每个能力自己决定接力”互斥，且是隐式生效 |
| **节点声明 link** | 拓扑随机器变化；多机不一致不可发现（§3.5 四条） |
| **节点自己投递 / 自己决定下一个能力** | 已否定两次，理由在 `docs/design/nats-transport.md` §13.1（授权粒度是任务不是能力；**「同一出口只发一次」需要共享去重表 = 唯一写者 = 中枢**）与 §13.4（图必须居中，否则 trace 底图与「有边没走」失效）。与 emit 叠加后更硬：ack 的权威只能是唯一写者，`节点 → 中枢` 这一段消不掉 |
| **fire-and-forget / 直投换延迟** | 真正能拿延迟的是**本地 outbox**（emit → 本地落盘 → 立刻 ack → 后台批量同步 → 中枢 dedupe 派发），不是把中枢移出路径 |
