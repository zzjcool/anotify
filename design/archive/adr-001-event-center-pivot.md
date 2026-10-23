# ADR-001 · 从「通知终端」转向「事件中心 + 执行节点」

- 状态：**Accepted（owner 已逐条拍定）**
- 日期：2026-10-23
- 决策者：项目 owner
- 实施细节：[hub-blueprint.md](hub-blueprint.md)（两份文档冲突时以 blueprint 为准）
- 影响面：`internal/broker`、`internal/store`、`internal/api`、`internal/push`、`internal/ws`、`internal/route`、`api/openapi.yaml`、`web-src/`、新增 `cmd/anotifyd`

---

## 1. 背景

现状是**单向终端（sink）**：

```text
Agent ─POST /v1/notify─▶ Broker(SQLite) ─┬─▶ WS 派发器 ─▶ WS 客户端
                                          └─▶ Push 派发器 ─▶ Web Push
```text

目标是一条**控制回路**：

```text
触发源(cron/webhook/watch) ─▶ 事件日志 ─▶ 规则匹配 ─▶ 任务(租约) ─▶ 节点执行 ─▶ 回报终态
                                                                  └─▶ 人类通道(推送等)
```text

关键判断：**这不是重构，是产品转向**。旧产品是管道（无闭环），新产品有闭环（谁执行、成没成、要不要重试）。

---

## 2. 已确认为可复用的地基

| 资产 | 位置 | 复用方式 |
| :-- | :-- | :-- |
| `Publish/Subscribe/Ack/Replay` + 高水位 | `internal/broker/broker.go:56-71`、`sqlite.go:152-200` | 语义就是「至少一次 + 可回放 + offset」，直接当事件日志内核 |
| 每用户单调 `seq` + `UNIQUE(user_id, seq)` | `internal/store/schema.sql:70-85` | partition key + offset |
| WS 骨架：hello / replay(`Last-Event-Id`) / ack / resume / 30s 心跳 | `internal/ws/handler.go:85-271`、`protocol.go:14-31` | 节点出站长连接的现成协议骨架 |
| `route.FilterDevices`（enabled × event_scope × tags） | `internal/route/filter.go:27-81` | tags 直接泛化为「事件 attributes 的订阅」 |
| 单二进制 + embed + 19 套 e2e + PR CI | `Makefile`、`.github/workflows/ci.yml` | 回归网 |

## 3. 现状缺口（全零）

- 触发器侧：**无** cron / 入站 webhook / watcher / 重试队列 / 去重（全仓 grep 零命中）
- 可观测性：**无** metrics / trace，`/health` 不查 DB（`internal/server/mux.go:228-233`）
- 优雅退出：**无** `signal.Notify` / `Shutdown`（全仓零命中）
- 控制台实时：前端**无** WS / 无 SSE / 无轮询，只有下拉刷新（`web/partials.js:893-1041`）
- 单实例假设写死：`broker.go:5-7` 注释「单进程」；`SetMaxOpenConns(1)`（`store/store.go:31-34`）；限速器进程内 map
- 通知可靠性：`broadcast` 满 64 静默丢弃（`sqlite.go:274-280`）；`deliveries.attempts` 恒 1，无重试（`store/deliveries.go:27-31`）；`DeleteExpired` 零调用（`broker/sqlite.go:204`）

---

## 4. 决策（冻结项）

### D1 · Sink 分两类，永不混用

| 类别 | 语义 | 投递保证 | 能否回报 | 例子 |
| :-- | :-- | :-- |---|---|
| **human sink** | 告知人，fire-and-forget | 尽力（可丢） | ❌ 不回报 | webpush、browser-sse、将来的 email |
| **machine sink** | 委托执行 | 至少一次 + 租约 + 重试 + DLQ | ✅ 必须回报终态 | node-agent、http-hook |

**铁律**：`broadcast` 式静默丢弃只允许用于 human sink。machine sink 一律走「DB 行租约认领」，绝不走内存广播。

**推论**：iOS/Safari 永远只能是 human sink（浏览器无后台执行能力），因此它**永远不与 node-agent 竞争**，两者是完全平行的两条 lane。

### D2 · 事件信封（Event Envelope）

```jsonc
{
  "id": "evt_<ksuid>",
  "tenant_id": "default",            // D7：多租户占位
  "source": "github",                // cron | watch | webhook | github | agent
  "source_ref": "trg_<ksuid>",       // 哪个触发器实例产出（外部 webhook 为 null）
  "type": "pr.opened",               // source 命名空间内的类型
  "subject": "acme/api#123",         // 给人看 + 深链
  "occurred_at": "...",              // 源侧时间
  "received_at": "...",              // 中枢时间（迟到检测靠它）
  "dedupe_key": "sha256:...",        // 中枢推导或来源提供；(tenant, source, dedupe_key) UNIQUE
  "attributes": { "repo": "acme/api", "author": "alice", "branch": "main" },
  "payload": { /* 源原始 JSON，不解析、只透传、不入索引 */ }
}
```text

**约束**（决定后续所有性能与安全问题）：

- `attributes` 扁平 `string→string`，有界（≤32 键，值 ≤256B），**只有这一层可查询/可索引/可路由**
- `payload` 是黑盒，上限 256KB（对齐现有 1MB body 限制留余量），禁止出现在任何索引、任何推送载荷里
- 幂等锚点 = `(tenant_id, source, dedupe_key)` UNIQUE，重复投递静默去重并返回 200（**不让 webhook 重发变重复执行**）

### D3 · Rule = match + target + action，禁止任意代码

```yaml
match:                                  # 顶层只允许 AND；列表值内部 OR
  source: github
  type: [pr.opened, pr.synchronize]
  attributes.repo: { eq: acme/api }
  attributes.author: { not_in: ["renovate[bot]"] }
target: { node_selector: { labels: { gpu: "true" } } }   # 或 sink: http-hook
action:
  kind: agent.prompt                    # 受限枚举
  template: "审查 PR {{subject}}：{{payload.pull_request.title}}"
```text

**明确否决「规则里嵌 JS/Lua」**。理由：① 支持成本；② 无法向用户解释「为什么没触发」。

反过来，受限语法能对每条规则给出**逐子句解释**（`repo ✅ / type ✅ / author ❌`）。这份可解释性是相对 n8n 类工具的真正差异点，且实现便宜。

### D4 · Task = 带租约的作业队列，不是声明式 reconcile

```text
pending ──▶ leased ──▶ running ──▶ succeeded | failed | canceled
              │
              └─(租约过期, attempts<3)──▶ pending (退避)
              └─(attempts≥3)───────────▶ abandoned → DLQ
```text

- `idempotency_key = (event_id, rule_id)` UNIQUE —— **重复投递永不重复花钱**
- 租约 TTL 30s，节点每 10s 续租（心跳即续租）
- `attempts` 上限 3，指数退避；`abandoned` 进 DLQ 并可人工重投
- **必须有 `cancel`**：用户能从控制台掐掉卡住的 run。这是单向通知永远表达不了的需求
- **明确否决 kubelet 式「声明式收敛 / 幂等 reconcile」**：LLM agent run 昂贵、长时、有副作用、不可幂等，模型是 Temporal/Sidekiq 语义

### D5 · 节点只执行，中枢只表达意图（安全不变量）

- 中枢发送的是 `action.kind` + 模板，**永不发送 shell 命令字符串**
- 节点自己决定 `agent.prompt` 意味着什么：跑哪个 CLI、哪个 cwd、哪个模型凭据、什么沙箱
- **模型 API key / 仓库内容 / 工作区永不上传中枢** —— 写入 ADR 作为不可协商的安全性质
- 中枢只存**终态 + 摘要**（见 D6）
- 节点凭据是独立凭据类型，可轮转、按节点吊销；**节点标签由中枢签发，不接受节点自报**（否则被入侵节点可冒充 `gpu=true` 抢 GPU 任务）

### D6 · 中枢只看终态 + 摘要

```jsonc
// TaskResult —— 上行，硬上限 8KB
{
  "task_id": "tsk_...",
  "attempt": 1,
  "status": "succeeded" | "failed" | "canceled",
  "started_at": "...", "finished_at": "...",
  "summary": "重构完成，12 文件变更，测试全绿",   // ≤2KB
  "exit": { "code": 0 },
  "artifacts": [ { "kind": "link|commit|pr|file", "label": "...", "url": "..." } ],
  "log_hint": { "node_id": "nod_...", "local_path": "~/.anotify/logs/tsk_xxx", "bytes": 48213 }
}
```text

- **不落全量日志**：体积无界 + 日志里全是 secret
- 调试靠**指针 + 按需拉取**：`POST /v1/tasks/{id}/logs-pull` 让节点临时上传尾部 N KB（默认关，显式开启，可审计）
- 每 task 的 summary/artifacts 是控制台唯一渲染源

### D7 · 单租户开源 + 多租户为「同仓库 config-gated 超集」

**否决 fork**（两个代码库 × 现有的 e2e 全绿门禁文化 = 必然漂移）。

- 同一仓库、同一 e2e，靠 `ANOTIFY_MULTI_TENANT=1` 开关（e2e 的 per-suite 独立起服已天然支持两种模式各跑一遍）
- 多租户是**加成层（additive layer）**，不是分叉
- **现在就要付的保险费**（几乎零成本）：
  - 新表一律带 `tenant_id TEXT NOT NULL DEFAULT 'default'`
  - 新增 `internal/tenancy` 包，`Current(ctx) string` 恒返回 `"default"`（一行间接层，将来换真逻辑）
  - 新代码禁止写「没有 tenant 维度就查全表」的查询；主键继续用 KSUID，不暴露全局自增 ID
- 将来要付的真账（知道就好，现在不做）：`store/admin.go:40-48`（`ListUsersWithStats` 无 user 过滤）、`store/admin.go:104`（`SystemStats`）、`server/ratelimit.go:22-90`（进程级限速器需按 tenant 分桶）、WebAuthn RP ID 单 origin（需子域一租户或 RP 内嵌 tenant claim）

### D8 · 节点侧「命名命令注册表」——中枢永不下发命令

已确认：节点上跑的是 agent CLI 的 headless 形态（`pi -p` / `claude -p` / `codex exec` …），且**也允许自定义 shell**。但必须解决一个安全死结：

> 一旦中枢能下发命令行，中枢被入侵 = 所有节点 RCE。中枢只存 `user_id` + 一个 SQLite 文件，它不该是这个爆炸半径。

**解法：命令由节点本地定义，中枢只发「名字 + 参数」。**

节点本地配置（`~/.anotifyd/config.yaml`，**永不上传、中枢不可读**）：

```yaml
commands:
  review:
    argv: [pi, -p, "{prompt}"]          # argv 数组，绝不经过 shell 解析
    cwd: ~/code
    timeout: 1800
    max_params: { prompt: 16384 }
    allow: true
  deploy:
    argv: [./scripts/deploy.sh, {target}]
    allow: false                         # 默认关，需人工置 true
```text

中枢下发的只有：

```jsonc
{ "kind": "cmd", "name": "review", "params": { "prompt": "审查 PR ..." } }
```text

**三条不可协商的约束：**

1. **中枢永不携带 argv / shell 字符串**。中枢里没有「命令行」这个字段，物理上表达不了
2. **节点用 `execve(argv[])` 启动，禁用 `/bin/sh -c`**。参数只做**占位符替换进 argv 槽位**，不做 shell 拼接 → 注入攻击按构造被消灭（不经 shell 就无注入面）
3. **未注册的 `name` → 任务失败 `unknown_action`**，控制台可见。中枢被完全攻陷，攻击者能做的仍只是「调用运维已定义的那几条命令」

**附带收益**：节点在 `hello` 帧上报的是**命令名列表（capabilities）**，不是 argv —— 中枢能展示「哪些节点能跑 review」，但不会泄漏节点路径 / 凭据 / 脚本内容。

内置便捷 kind（本质是注册表的语法糖，仍走同一套）：`notify`（本地桌面通知）、`noop`（连通性自测）。

**节点侧必须实现**：并发上限、单 run 超时、cancel 即 kill 整个进程树、cwd 沙箱（必须在允许前缀内）、环境变量白名单（默认不透传）。

### D9 · 外部触发 = 通用 webhook（已确认，不做插件 SDK）

- 端点：`POST /v1/in/wh/{trigger_id}`，body 即 `payload`
- 鉴权：trigger 级 secret（`whsec_...`，仅存哈希，对齐现有 `api_keys` 的 argon2id 做法）+ 可选 HMAC-SHA256 `X-Anotify-Signature`
- 映射：`trigger_id → source/type/attributes 抽取规则（受限 JSONPath 子集）→ dedupe_key`
- **不做**：可安装插件、签名 SDK、schema 校验框架（另一个量级的工程）
- 内置适配器只做 GitHub / GitLab 两个（其他一律走通用 webhook）

### D10 · 通知降级为 human sink —— 但**不是**降级成可选

**这是本 ADR 最容易被误读的一条，先定义清楚：**

> 「通知是一种 sink」是**内部架构陈述**，不是产品降级。对 iOS 用户而言行为零变化。

具体处置（详见 §5）：

1. 现有 Web Push 链路（VAPID / `dispatcher` / `sw.js` / `devices` 表）**一行不删**
2. 通知改由**内建 system rule** 驱动，默认开启：`event.type ∈ 精选列表 → human sink（全设备，按 tags 路由）`
3. 精选列表维护在 `event type registry` 的 `default_notify` 标志上（**不是** per-event 字段，**不是** `severity`——该字段已于 `7fd6ffe` 全链路删除，不复用）
4. **`/v1/notify` 彻底删除，无兼容层**（owner 决定，插件仓库同步改造）。纯通知场景改用 `notify` 规则模板 + `POST /v1/events`，详见蓝图 §5.4

### D11 · tags 路由免费复用，`event_scope` 直接删除

- `devices.tags` ∩ `event.attributes`（最近匹配/ANY）—— `route.FilterDevices` 的核心逻辑几乎原样可用
- `devices.event_scope`（`final|all`）：对通用事件无意义，且 **DB 整体清空重建（D12）→ 连同数据一起删除，无需「保留但不读」的妥协**

### D12 · 不做数据迁移：数据库整体清空重建

**owner 决定：清空数据库。** 于是没有绞杀者迁移、没有双写期、没有兼容分支。

| 项 | 处置 |
| :-- | :-- |
| `messages` 表 | 从 `schema.sql` **彻底删除** |
| `deliveries` 表 | 直接改为 `event_deliveries` |
| `devices.event_scope` 列 | **彻底删除** |
| `migrateColumns` 历史 ALTER | 清空（保留机制，当前只有一份干净 schema） |
| `migrateMessagesDropKindReply` | **删除** |
| `internal/api/notify.go` / `AgentState` 五枚举 / `IsTerminal()` | **删除**（事件类型改为开放字符串） |

**升级说明只需一句**：v2.0 是新产品，升级需清空数据目录（`ANOTIFY_DB`）。

> 上版本文的 strangler 三批次（B1/B2/B3）已作废，理由记录在此：当时假设有存量用户数据需保护；实际项目仍在开发期，双写期的复杂度纯属自找。

---

### D13 · Chaining：把 sink 的输出作为 source 重新投递

核心场景（用户提出）：**agent A 完成 → 交给 B 继续 → B 完成 → 通知人**。这是本产品从「单跳触发器」变成「可编排中枢」的关键一步。

**但它引入了图环路 —— 这是整个 ADR 里最容易炸掉资金和信任的一处。** 所以设计原则是：**默认禁止，显式开启，全程可归因，硬预算封顶**。

#### D13.1 因果链模型（Correlation）

每个事件增加三个字段（D2 信封扩展）：

```jsonc
{
  "trace_id": "cor_...",   // 链根事件 id；无父时 = 自身
  "caused_by": "evt_...",        // 直接父事件 id
  "hop": 0                       // 距链根的跳数
}
```text

任务完成 → 中枢**派生**一条事件而非直接调用下家：

```jsonc
{
  "source": "agent",
  "type": "task.succeeded" | "task.failed" | "task.canceled",
  "subject": "tsk_...",
  "attributes": { "rule":"rev-pr", "node":"nod_x", "agent":"pi" },
  "payload": { /* TaskResult，D6 的 8KB 摘要 */ }
}
```text

**于是 chaining 退化为「规则匹配自己产生的事件」—— 不需要任何新的执行引擎。** 这是它与工作流引擎的本质区别（见 D13.6）。

#### D13.2 指针，不是载荷

模板上下文新增 `parent`：

```yaml
match: { source: agent, type: task.succeeded, attributes.rule: { eq: rev-pr } }
action:
  kind: cmd
  name: implement-fix
  params:
    prompt: "按上一轮审查意见修复。审查摘要：{{parent.summary}}\n分支：{{parent.artifacts.branch}}"
```text

**铁律：A 传给 B 的是指针（commit / branch / PR / 文件路径），不是 A 的上下文。**

原因有三：① 中枢本就不存全量日志（D6）；② 让 agent 通过**共享世界状态**（同一个 PR / 同一 branch）协作，比通过中枢搬运上下文更可靠、更省 token、更可审计；③ 指针是可验证的，搬运的上下文不是。

> 这条决定了整个 chaining 的质量：如果使用者试图把 A 的长文本塞进 B 的 prompt，会先撞上 8KB 上限——这是**故意**的保护，不是缺陷。

#### D13.3 六道闸（缺一不可）

| 闸 | 规则 | 默认值（可配） | 防什么 |
| :-- | :-- | :-- | :-- |
| **G1 显式 opt-in** | 规则必须声明 `allow_chain: true` 才能被 `task.*` 事件触发 | `false`（**不可配**） | 意外环路（A 的完成事件又触发 A 自己的规则） |
| **G2 深度** | `path` 中 rule_id 不重复（`depth ≤ max_hops`） | `10` | **无限环** + 拓扑失控 |
| **G3 环检测** | `rule_id ∈ path` → **剪枝**（不拦截、不暂停） | 隐含在 G2（**不可配**） | A→B→A |
| **G4 单链任务数** | 链内任务总数 ≤ `max_tasks` | `100` | 扇出放大 |
| ~~G5 付费任务数~~ | **已删除**（D13.7）—— 成本由 G4 + 每规则 `daily_budget` 承担 | — | **钱** |
| **G6 链龄** | `now − created_at ≤ max_duration` | `24h` | 卡死的链 |

**超限行为分两类（这条是本 ADR 的修订点）：**

| 类型 | 行为 |
| :-- | :-- |
| 环 / 超深（G2、G3） | **静默剪枝**：不创建任务、不暂停、**不写 `trace.blocked`**，只在 trace 记「已剪枝 + 原因」 |
| 超预算（G4、G6、每规则 `daily_budget`） | **真拦截** → `trace.paused` + 通知（需要人来调预算） |

**为什么环要改成剪枝（owner 决定允许环）**：回边是**正常的** —— 「每日巡检把自己挂上去」、「测试失败→修复→再测试」直到变绿。旧设计当错误上报，只会把控制台刷满、把 trace 卡成 paused。

**附带收益**：不需要「已触发过」去重表。环检测 = 路径前缀检查（`strings.Contains(path, "/"+rule_id+"/")`）。旧设计的 `(rule_id, trace_id)` 去重会**误杀正当的多轮迭代**。

**为什么六个而不是四个：** 上一版把「深度」和「成本」混成一个数字（用 LLM 成本论证 hop 上限），这是错的——**跳数 ≠ 花钱**。中转节点完全可以是不花钱的（`shell` 跑测试、`http-hook` 回调 CI、`notify` 推提醒）。拆开之后：`max_hops` 管可理解性、`max_paid_tasks` 管钱、`max_duration` 管卡死。`cost_class` 由节点本地 config 定，中枢不可覆盖。

全部可配（全局 env → 单规则 → 代码硬顶 32/1000/7d），只有 G1/G3 不可配（正确性边界，不是调优参数）。详见蓝图 §8.3。

#### D13.3b 执行树：一个 trace 是树/DAG，不是单链

owner 提出的场景：**PR → A → {C, D} → E**（C、D 都完成后触发 E）。这不是一条链，是菱形。

**问题①：E 的幂等键是什么？** 现有的 `sha256(event_id ∥ rule_id)` 在菱形里失效（C、D 各派生不同 event_id → E 跑两次）。

| 方案 | 判断 |
| :-- | :-- |
| 真 join（等齐 N 个父任务才触发） | ❌ **需要跨任务状态 + 超时 + 迟到处理 = 工作流引擎入口**，明确越界 |
| **时间窗口批处理**（`action.batching.windowMs`） | ✅ 无跨任务状态；窗口内合并成一次执行，后到事件进 `inputs[]` |

**这是批处理，不是 join**。UI 必须明示（否则用户会以为有 join）。

**问题②：`hop` 与 `depth` 必须分开。** 并行分支是同一 hop 层，但 `max_tasks` 要按展开后的原子任务数计。3 个分支各 5 跳 → hop=5 但 15 个任务。

**问题③：部分失败时下游跑不跑？—— 中枢不判断（owner 纠正）。**

上一版我设计了 `traces.prune = all_success`：同层有失败就自动剪掉下游。**这是错的**，因为：

1. 中枢只知道 `status=failed`，不知道**语义**（lint 报错但测试通过，未必不可接受）
2. **节点的回调本身就是一种 source** —— 它决定发不发事件、发什么 `attributes`；它的图是整棵树的一个子图
3. 中枢按失败自动剪枝 = 把业务判断硬编码进调度器

| 谁 | 决定什么 |
| :-- | :-- |
| **节点** | 回不回调、回调什么类型（例如发 `task.failed{recoverable:true}`） |
| **规则** | 收到这个事件后接不接（谓词全在规则里） |
| **中枢** | 只做：分发事件 · 记事实 · 执行硬限制（死循环 / 预算） |

**`traces.prune` 与 `expect_count` 两个字段已删除。**

#### D13.3e 状态归属：ack 是接单，不是完成

owner 提出「下发消息需要节点 ack，ack 表示任务完成，没有 ack 的是执行中」。

**方向对，但「没 ack = 执行中」会在断线时撒谎**：笔记本合盖、Wi-Fi 切换后，所有任务都显示「执行中」，包括那些已经跑完的。

按同一条原则（**谁观察到的事实谁权威**）：

| 状态 | 谁能观察 | 权威源 |
| :-- | :-- | :-- |
| `acked`（收到派发） | 节点 | 第一帧 `ack` |
| `running`（进程还活着） | **只有节点** | **心跳的 `inflight`** |
| `lease_expired`（联系不上） | 只有中枢 | 中枢租约 |
| `succeeded`/`failed` | 只有节点 | `result` 帧 |

**三条硬规则：**

1. **`ack` ≠ 完成**，只表示「我收到了派发」
2. **`running` 只能由心跳 `inflight` 维护**；心跳里没列的任务立即视为不在跑
3. **`result` 是唯一终态来源**，没有 `result` 就永远不会有 `succeeded`

**新增 `lease_expired` 状态并明确展示为「失联中」**（橙色），与「执行中」（黄色）区分。这才是既保留 ack 模型的直觉、又不撒谎的做法。

#### D13.3c Trace 对齐 OpenTelemetry

owner 要求：参考 OpenTelemetry 的 trace 思路把一次流转串起来。

**直接借 OTel 的词汇，不自己发明：**

| OTel | 本设计 |
| :-- | :-- |
| Trace | `trace_id` |
| Span / span_id | `task` / `task.id` |
| `parent_span_id` | `tasks.parent_task_id` |
| **Span Links** | `tasks.inputs[]` |
| Span Attributes | `rule` / `node` / `cost_class` / `status` |
| span events | 剪枝 · 重试 · 租约过期 |

**两个关键判断：**

1. **`Span Links` 是必需的，不是可选**。菱形 A→{C,D}→E 里 E 的 `parent_span_id` 只能指向一个（如 C），**D 通过 links 指向 E**。用父子关系硬塞会丢信息
2. **必须有 producer / consumer 两种 span 形态**。上一版 trace 只有任务节点、**没有事件节点**，于是「这条事件被哪条规则消费了」看不见。加上后，一条 trace 就是「事件 → 任务 → 事件 → 任务」交替的链

**Planner 图与 Trace 图仍是两张图**（静态推导 vs 实际渲染）。合一就错了：改规则后回看历史 trace，图会跟着变形。

- **trace 不落快照表**：从 `events` + `tasks` 现算
- `pruned` 标注是 trace 里最有价值的诊断信息 —— 它回答「我配了这条规则，为什么它没跑」
- 结构已对齐 OTLP，将来 `?format=otlp-json` 接 Jaeger/Tempo 成本很低（本期不做）

#### D13.3c 本期范围：扇出做，收集不做（owner 修正）

> owner：**这个不应该都下发吗？就和一条 PR 上来，我们可能下发 agent，也可能下发给我们手机通知。
> 派生最大的难点其实是下游收集，但是我们第一期不做的是这个收集动作，我们可以扇出，扇出数量我们都不管。**

**我前两版都错了：**

| 版本 | 错误 |
| :-- | :-- |
| 第一版 | 「本期严格 1:1」—— 会让「同一事件既派 agent 又推手机」不可能 |
| 第二版 | 把「扇出」和「收集」当成同一个问题，顺手删了 `branchBudget` |

**扇出与收集是正交的：**

|  | 扇出 | 收集 |
| :-- | :-- | :-- |
| 回答 | 「这个事件要引起几件事」 | 「那几件事都回来了吗」 |
| 本期 | ✅ **无条件支持**（N 条规则 → N 个任务） | ❌ **不做** |
| 难度 | 极低（一次 INSERT 多行） | 高（批次/计数/菱形/部分失败） |

**因此本期保留：** `branchBudget` / G3b 扇出闸 / `jti` 一次性消费 / 额度用尽可见。
**本期删除：** 汇聚、批次数、`batching.windowMs`、`join` 规则字段（写了直接报错）。

**没有收集怎么实现「等一组都完成」**：放在节点内部。A 在自己进程内并行、自己等待、自己汇总，回报中枢一个 result。对中枢来说这仍是一个任务。

**扇出爆炸的唯一闸**：G4 `max_tasks`（默认 100，硬顶 1000）+ 每事件命中规则数 ≤ 50。超出即不建任务并记 `pruned: task_budget_exhausted`。

**环仍然允许**：`path` 是「从根到本任务这条具体路径」的 rule_id 序列（不是全 trace 集合），`rule_id ∈ path` → 静默剪枝但必须可见。

#### D13.3d 节点自主派生：签名额度 + 一次性消费（owner 方案，我上一版判断有误）

owner 选了模型 B（节点可自主发起子树），方案是：**JWT 里签 `hop`，中枢每下一跳签一个新 token，收到 ack 后不再接受同一 token**。

> ⚠️ **纠正**：我上一版说「签名完全防不了指数爆炸」，**这个判断错了**。我当时假设中枢是无状态签发方（节点请求就签新的）。owner 的设计是「只在 token 被合法消费时才签下一个」，于是：

| 攻击 | owner 设计 |
| :-- | :-- |
| 篡改 `hop` | ✅ 签名 |
| 跳过中间跳 | ✅ **结构上不可能**（不存在 hop=5 的 token 而 hop=2 未走） |
| 同一 token 用 10 次 | ✅ **结构上不可能**（`jti` 一次性消费） |
| 线性无限链 | ✅ `hop ≤ maxHop` 签发时判定 |

**新问题（不是签名能解决的）与解法：**

**① 扇出变得不可能** —— 一个父 token 只能换一个子 token，那 `A → {C,D}` 怎么表达？

→ token 里带**签名的额度**：

```jsonc
{ "trace": "cor_7f2", "span": "spn_a1", "hop": 2, "maxHop": 10,
  "branchBudget": 2,        // ★ 还能派几个子（扇出额度）
  "exp": 1761237600, "jti": "spn_a1:0" }   // ★ 一次性消费
```text

`A` 拿 `branchBudget=2` → 可换 2 个子 token，第 3 次被拒。**扇出与爆炸的分界线就是这一个签名字段**，且节点改大即验签失败。

**分配权归节点（owner 选 A）**：`branchBudget` 是树的总预算，A 怎么分（C=1,D=1 还是 C=2,D=0）完全由节点决定，中枢只验签和递减、**不预估图**。这与模型 B 的精神一致 —— 节点知道自己的业务，中枢不知道。

`paidBudget` **已删除**（D13.7）；`branchBudget` **保留**（扇出本期就有，见 D13.3c）。

签名唯一真正能做的事：**把不可伪造的额度随身携带**。配合一次性消费，额度既不能回滚也不能超发。

**② nonce 消费与建任务必须同一事务** —— 消费了却没建成 = 分支永久丢失。顺序：插 `branch_nonces.jti`（UNIQUE 冲突即幂等返回）→ 建任务 → 扣预算，全在一个事务里。

**③ 节点侧记录 trace/ack 的定位** —— 应做，但它是**正确性辅助 + 防御纵深**，**不是安全边界**（被入侵的节点可以直接跳过自己的本地规则）。安全边界永远在中枢：nonce + 签名 + 预算。

**④ 成本仍需独立预算** —— 线性 10 跳 × 付费依然烧钱。但**不用 `paidBudget`**（见 D13.7），改由 G4 `max_tasks` + 每规则 `daily_budget` 在中枢侧计量。

#### D13.7 删除 `paidBudget`（owner 决定）

`paidBudget` 是唯一一个**计量源在节点**的预算 —— 节点按 `costClass` 自报扣减，等于让节点自己决定「我这次算不算花钱」。删掉。

删掉后成本控制仍有两条防线，且都不依赖节点上报：

| 防线 | 计量源 |
| :-- | :-- |
| **G4 `max_tasks`**（100） | 中枢 DB `task_count` |
| **每规则 `daily_budget`** | 中枢 DB（rule_id + 日期计数） |

**「任务数」是成本的良好代理**：一次 agent 调用必然是一个 task，`max_tasks=100` 已隐含「这条链最多花 100 次」；`daily_budget` 再拦住「一天跑 200 条链」。

`costClass` 字段**保留但只用于 UI 展示**（「这条链有 3 个付费任务」），不再参与闸门。

#### D13.8 重试次数交给节点自己定（owner 决定）

中枢不该知道「重试几次合适」：跑一次 lint 重试 3 次都值得，跑一次 2 小时训练重试 1 次就够 —— **只有节点知道**。

拆成两个概念，各管一头：

| 概念 | 谁定义 | 语义 |
| :-- | :-- | :-- |
| **节点内部重试** | **节点**（manifest `commands[].retries`） | 进程崩/非零退出，节点自己重跑，**中枢完全不知道** |
| **中枢重派** | 中枢（`max_attempts`，默认 3） | 租约过期/离线/明确 failed → 换节点再派 |

**关键规则：节点内部重试不消耗中枢 `attempts` 配额**，否则「节点重试 2 次 + 中枢重派 3 次」= 6 次执行，而中枢以为只有 3 次。

`result` 帧带 `attemptInfo.internal`，中枢记进 `tasks.internal_attempts`（仅展示）。

#### D13.4 失败语义：链不是事务

- **A 的副作用是真实的**，B 失败不会回滚 A
- 重试默认**只重试失败的那一跳**，不重跑整条链
- 但每个 task 可声明 `on_failure: retry_step | restart_chain | abort`（默认 `retry_step`）
- **失败必须通知人**，无论链多深 —— 绝不能因为「中间某跳失败」而让整条链静默死掉
- trace 状态机：`active → settled | failed | paused | expired`（TTL 24h）

#### D13.5 链与通知的关系（顺带解决 §6 的噪音问题）

**关键设计：`task.*` 事件的 `default_notify = false`，只在该 trace 静默下来时才发一条终态通知。**

于是 5 跳的链 = **1 条通知**（`「rev-pr → fix → test 全部完成，用时 23 分钟」`），而不是 5 条。

实现（单实例 SQLite 很便宜）：任务终态时，若该 trace 内已无 `pending|leased` 任务 → 标记 `settled` → 投递一条聚合通知。

这同时把 §6 的「事件中心太吵」问题在链这个最密集的场景上直接解掉了。

#### D13.6 这仍然不是工作流引擎（防 review 误判）

| | 本 ADR 的 chaining | 工作流引擎（Temporal / Argo） |
| :-- | :-- | :-- |
| 编排表达 | 若干条**互相独立的事件规则** | 一张显式 DAG |
| 依赖发现 | 中枢不知道也不关心下游是谁 | 中枢预先知道全图 |
| 分支/循环/汇聚 | **不支持**（只能靠事件条件） | 原生支持 |
| 失败恢复 | 单跳重试 | 确定性重放、补偿事务 |

**我们做的是「事件驱动的多跳派发」，不是「工作流编排」。** 需要真 DAG 的人应该去用 Temporal —— 这条边界写死在 ADR 里，避免 scope 蔓延成第二个 Argo。

**顺带澄清**：这也顺带说明为什么 D4 说「不是声明式 reconcile」——chaining 仍然是**离散的、带租约的作业**，只是这些作业之间通过事件串起来。没有任何东西在做状态收敛。

### D14 · 通知随链收敛（Notification settles with the chain）

这是一条**产品语义**决策，不只是实现细节：

- 单跳事件（cron / webhook / watch）→ 匹配即通知（按 D10 白名单 + §6 的节流）
- 多跳链（`hop > 0` 或 trace 内有活跃任务）→ **只在 trace 收敛时**通知一次
- 链中任一跳失败 → **立即**通知失败（不等收敛），并标注「后续已暂停」
- trace `paused`（撞预算/环检测）→ **必须**通知，这是需要人来决断的状态

### D15 · 节点暴露 capability manifest，而不是事件类型

起因：owner 提出「边缘节点是不是应该把能上报的消息类型暴露出来，这样定义 source / 加 sink 时就能推算出图」。

**方向对，但要先拆开三个不同的「类型」：**

| # | 是什么 | 由谁定义 | 节点该上报吗 |
| :-- | :-- | :-- | :-- |
| ① 事件类型 | `pr.opened` · `task.succeeded` | **source** | ❌ 节点不产生事件类型 |
| ② 动作类型 | `review` · `deploy` | **节点** | ✅ 这才是要暴露的 |
| ③ payload schema | `pull_request` 的字段 | 约定 | ⚠️ 仅 advisory，不强制校验 |

决策：

1. **`nodes.capabilities` 从「命令名数组」升级为 capability manifest**：`{name, kind, costClass, params(advisory), produces[], timeoutSec, description}`，带 `manifestVersion`
2. **manifest 里永远没有 `argv` / `cwd` / `env`** —— 这是 D8 的延伸：中枢连「命令怎么跑」都不该知道
3. **payload schema 是 advisory**（写入永不拒绝），只在模板编辑器做补全与提示。强制 schema 校验仍在非目标里（D3）
4. **拓扑图用类型级二部图**，不画「所有 source × 所有 sink」的全连线图：那样 3 source × 3 sink × 3 节点 = 15 条线 + 9 条虚线，大部分边不表达任何意图。类型级画法能画出**真实可读的路径**，包括接力回环（用 `produces[]`）
5. **manifest 的首要价值不是画图，而是把「上线即失效」变成保存时的警告**：没有 manifest 时，selector 写 `capability: review` 而恰好无节点提供 → 任务会全部 pending → 撞 attempts 上限 → abandoned。用户会先配好一切然后发现配了个空的东西

**为什么事件类型不能由节点上报**：节点不产生业务事实，它只执行。让节点声明 `pr.opened` 会立刻把 hub 变成「每个节点都能伪造任何事件类型」。

**三条边界**：不做强制 schema 校验；不做 schema 版本迁移（事件是事实记录，不改写历史）；**绝不用 manifest 自动建边**（没有规则就没有任务，否则一次 manifest 更新就能凭空产生自动化）。

细节见蓝图 §12。

---

## 5. 处置：iOS / Safari Web Push 怎么办

**结论先行：Web Push 链路一行都不用改。但变 sink 之后有 5 个真实的坑，必须与事件中心同期封住。**

先明确边界：iOS/Safari 是 human sink，**永远不可能是 machine sink**（无后台执行）。所以它不进租约体系、不参与重试、不需要回报 —— 它只是「告知人」这条 lane 上的一个实现。

| # | 坑 | 现实严重度 | 处置 |
| :-- | :-- | :-- | :-- |
| **P1** | **载荷预算** | 🔴 高 | iOS Web Push 载荷约 **4KB**；GitHub webhook 原始 payload 常见 20–100KB。若设计者天真地把 `event.payload` 塞进推送 → iOS 端**静默失败**（本项目 push 无重试、失败只写 `failed` 记一行）。**规则：推送载荷只能是摘要投影**（title/body/link + `evt_id`），详情靠深链回到中枢。这正好印证 D6「只看终态 + 摘要」的铁律。 |
| **P2** | **深链与登录态** | 🟠 中 | 通知点击走 `clients.openWindow`，iOS 上可能打开**Safari 标签页而非 PWA**——那里没有 `anotify_session` cookie → 被踢到登录页。现有 `?next=` 回跳机制（`624cffe`、`0df9399`）可复用，但 `scripts/e2e/suites/deeplink.mjs` 必须跟着改为新目标。**建议**：深链固定为稳定短路径 `/e/{evt_id}`，服务端 302 到详情页并保留 `next`。 |
| **P3** | **订阅生命周期无人盯** | 🔴 高 | iOS 会**静默丢弃** push 订阅（换机、清站点数据、PWA 重装）。以前通知是主产品，有人天天用、丢了立刻发现；变 sink 后没人盯，**静默失效会持续数月不被发现**。处置：新增 sink 健康自检 + 控制台横幅「这台设备已 N 天未收到推送，点此重新订阅」，并在 `devices.last_delivered` 上做阈值告警。这是**转向带来的新需求**，必须做。 |
| **P4** | **投递可观测性必须留** | 🟠 中 | `deliveries`（`schema.sql:97-107`）是「3/4 设备已送达」的唯一来源。改造为 `event_deliveries(event_id, sink_kind, target_id, status, attempts, error)`；**human 与 machine 共用一张表但 status 语义不同** —— human 只到 `sent`/`failed`（无 ack），machine 到 `acked` 且带 result。现有 `attempts` 恒 1 的缺陷对 human 可接受，对 machine 不可接受（D1 铁律）。 |
| **P5** | **谁来触发通知？** | 🟠 中 | 见 D10 第 2–3 条：内建 system rule + `default_notify` 注册表标志。**绝不能**临时拍一个「全部事件都推送」——见 §6。 |

**一个额外的、需要你现在就知道的负面效应（§6）**：

### 6. 事件中心天然比通知中心吵 —— human sink 第一版就要有节流

通知中心的事件密度 ≈ agent 任务数（一天几十条）。
事件中心的事件密度 ≈ **所有 cron tick + 所有 watcher 轮询 + 所有 webhook**（一天可能几万条）。

如果把 human sink 做成「匹配就推」，上线第一周就会被推送洪水淹没，然后用户会关掉通知 —— 而那正是你要保住的能力。

**因此 human sink 的交付物必须包含**（不能延后到「以后再说」）：

1. **静音规则**（mute）：按 `source` / `type` / `attributes` 关掉某类
2. **聚合摘要**（digest）：同一 `subject` 在 N 分钟内合并成一条「5 条更新」
3. **速率上限**：每设备每 5 分钟最多 1 条（超出进摘要队列）
4. **`default_notify` 白名单**：只推「值得打断人」的事件类型；cron tick / watch 无变化 / 成功的心跳一律不推

> 注意措辞纪律：**不要用 `severity` 表达这件事**。该字段已于 `7fd6ffe` 以「零消费方 = 纯成本」为由全链路删除，重新引入会被 review 门禁打回。用 `event type registry` 的布尔标志 + 用户的 mute 规则表达。

---

## 7. 明确不做（与路线图同等重要）

沿用项目历史上两次成功收缩的尺子（`63f7d79` 砍 reply、`7fd6ffe` 砍 severity：**零消费方的功能 = 纯成本**）：

| ❌ 不做 | 理由 |
| :-- | :-- |
| 任意代码规则 / 脚本沙箱 | 支持成本 + 不可解释 |
| 工作流 DAG / 条件分支 / 汇聚节点 / 循环语法 | 那是 Temporal 的活；本 ADR 只做 D13 的「事件驱动多跳派发」 |
| 多租户 org/团队（本期） | D7：留占位，不实现 |
| 中枢存 agent 全量日志 | 体积无界 + secret 泄漏面 |
| 插件 SDK / 可安装外部触发器 | D9：本期只要通用 webhook |
| 现在换 Postgres / 拆微服务 / 上 K8s | 单二进制自托管是核心卖点 |
| 声明式 reconcile / 期望状态收敛 | D4 |
| 双向对话（人回复 → 注入 agent） | `63f7d79` 已否决，本 ADR 不重开 |

### ⚠️ 一条必须预先写明的边界（防 reviewer 误判）

D4/D6 引入了「节点 → 中枢」的结果回报通道，**外观上像把刚删掉的 `reply` 加回来**。它不是同一件事：

| | 已删除的 `reply`（`63f7d79`） | 本 ADR 的 `TaskResult` |
| :-- | :-- | :-- |
| 主体 | **人** → agent | **机器**（调度器）→ 中枢 |
| 目的 | 对话 / 干预 / steer | 调度闭环（成败才能决定重试） |
| 是否必需 | 可选功能，零消费方 | 无它则调度器无法工作 |

**这条边界请在 review 时对照，不要当成「重新引入 reply」打回。**

---

## 8. 硬风险（按致命度）

| # | 风险 | 为什么致命 | 缓解 |
| :-- | :-- | :-- | :-- |
| R1 | **成本失控** | 一条配错的规则 = 一晚上烧掉几百刀 | 每节点最大并发、每规则日预算上限、熔断（同 `idempotency_key` 连续 N 次失败即暂停规则）。**这是准入条件，不是后期优化** |
| R2 | **单实例假设 × cron** | 加了 cron 后第二个副本 = 定时任务双发 | v1 明写单实例，但调度器必须用 **DB 行租约认领**实现，保证将来加副本是改配置不是重写 |
| R3 | **零优雅退出** | 有租约后这是正确性问题：进程被杀必须释放租约，否则节点白等 30s | `signal.Notify` + `http.Server.Shutdown` + 释放租约 + 关 eventlog |
| R4 | **控制台无实时** | 事件中心的控制台不实时就是废的 | SSE（比 WS 简单；`server/middleware.go:44-49` 的 `statusWriter` 已实现 `Flusher`） |
| R5 | **节点凭据是新的爆炸半径** | 能跑任意 agent 的长期令牌 ≠ `notify:send` key | 独立凭据类型、可轮转、按节点吊销、标签由中枢签发（D5） |
| R6 | **文档已在腐化** | `IOS_TESTING.md:56` 与 `design/tech-scheme.html:1555` 仍写着 8 月就删掉的 `status` 字段；本次转向会让几乎所有 `.md` 失效 | 立门禁：契约/行为变更必须同 PR 改文档 |
| **R7** | **链风暴 / 成本放大** | D13 是唯一能让单条 webhook 放大成 N 次 LLM 调用的通道。一次误配 = 通宵烧钱 | G1–G6 六道闸（D13.3）全部是准入条件，**不接受「先上线后补」**；`trace.blocked` / `traces.blocked_reason` 必须在控制台可见 |
| **R8** | **中枢被攻陷 ≠ 节点 RCE** | D8 的命名注册表是本项目最重要的一条安全不变量，任何「让中枢下发命令」的提案都是回归 | 把「中枢从不携带 argv」写成 e2e 断言（下发非法 action 应得 `unknown_action` 而非执行） |

---

## 9. 决策台账（全部已由 owner 拍定）

> 本 ADR 的所有条目均已确认。实施维度的细节（DDL / 协议帧 / API 面 / 验收标准）见 [hub-blueprint.md](hub-blueprint.md)。

| # | 问题 | 结论 |
| :-- | :-- | :-- |
| U1 | `default_notify` 白名单首批 | `agent.task.failed` · `trace.settled` · `trace.paused`（蓝图 §10.3） |
| U2 | 深链形态 | `/e/{id}` 短链 + 302 分派，保留 `?next=`（蓝图 §10.5） |
| U3 | 节点标签 | 中枢赋值、**绑定 `node_id`**；`os`/`arch` 只进 `reported`（D8 / 蓝图 §9.5） |
| U4 | metrics | 本期不上；`/health` 必须查 DB |
| U5 | 链预算默认值与可配性 | 默认 10 / 100 / 20 / 24h，硬顶 32 / 1000 / 100 / 7d，**全部可配**（D13.3） |
| U6 | 链默认 `on_failure` | `retry_step`（D13.4） |
| U7 | human-in-the-loop gate | 本期不做，但预留 `tasks.requires_approval` |
| U8 | `/v1/notify` | **彻底删除，无兼容层**；纯通知场景改走 `notify` 规则模板 + `POST /v1/events`（蓝图 §5.4） |
| U9 | 数据迁移 | **不做**。DB 整体清空重建（D12） |
| U10 | 多租户 | 同仓库 config-gated 超集，**不 fork**（D7） |
| U11 | 节点是否提前暴露类型 | ✅ 暴露 **capability manifest**（能执行什么），**不**暴露事件类型；拓扑图为类型级二部图（D15） |
| U12 | 是否允许环 | ✅ **允许**。环靠 `path` 去重终止，回边是剪枝不是拦截（D13.3b） |
| U13 | 扇出汇聚（A→{C,D}→E） | ✅ 时间窗口批处理，**不做真 join**（D13.3b） |
| U14 | Trace 与 Planner 图 | ✅ **两个独立视图**，trace 不落快照（D13.3c） |
| U15 | 部分失败时下游跑不跑 | ✅ **中枢不判断**：节点决定回不回调、规则决定接不接；中枢只分发/记事实/执行硬限制。`prune` 字段已删 |
| U16 | Trace 数据模型 | ✅ **对齐 OpenTelemetry**（Span Links + producer/consumer span 形态） |
| U17 | 节点自主派生（A/B 模型） | ✅ **模型 B**：JWT 携带签名额度（`hop`/`branchBudget`）+ `jti` 一次性消费（D13.3d） |
| U18 | 签名能否防跳数篡改 | ✅ **够**（我上一版判断有误）。签名额度 + 一次性消费在结构上封死跳数篡改与指数爆炸。剩余线性成本 → G4 + `daily_budget`（D13.7） |
| U19 | ack 语义 | ✅ **ack = 接单确认**，不是完成；`running` 由心跳 `inflight` 维护；新增 `lease_expired`（D13.3e） |
| U40 | 本期范围 | ✅ **扇出做，收集不做**（owner 修正）。一个事件命中 N 条规则 → 派 N 个任务，N 不管。删：汇聚/批次数/`batching`/`join` 字段；**保留**：`branchBudget`/G3b/`jti`（D13.3c） |
| U41 | 多规则同时命中 | ✅ **全部命中都下发**（owner 修正）。同一事件的多条规则是**并行派发**，不是互斥选择（D13.3c） |
| U24 | 版本标识 | ✅ **`<prefix>_<UTC时间>_<hash8>`**（owner 提出）。**但 trace 钉住 hash 版本 id，不钉住时间** —— 否则补录历史版本会让旧 trace 变形（D15） |
| U25 | trace/span 是否进元数据 | ✅ 进 `events` 普通字段 + 索引（owner 提出） |
| U26 | `paidBudget` | ✅ **删除**（owner 决定）：唯一「计量源在节点」的预算。改由 G4 + `daily_budget`（D13.7） |
| U27 | 单任务重试次数 | ✅ **节点自定义**（owner 决定）：`manifest.commands[].retries`，不消耗中枢 `attempts`（D13.8） |
| U21 | `branchBudget` 分配权 | ✅ **节点自主**（中枢只验签递减、不预估图）；但用尽必须可见（409 + span event），不静默失败（D13.3d） |
| U20 | 任务 id 怎么生成 | ✅ **`trace_id + span_id`**（owner 方案）：`task.id = tsk_{trace8}_{span8}`，`span_id` 中枢每跳新生成 → `branchLocalId` 删除（D13.3d） |
