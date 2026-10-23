# Anotify Hub · 总设计（Blueprint v1）

> 状态：**讨论中，未冻结**。冻结后本文件是唯一事实源。
> 决策理由见 [adr-001-event-center-pivot.md](adr-001-event-center-pivot.md)（该文为决策记录，与本文件冲突时以本文件为准）。
> 定位：**从「Agent 完成通知」转向「事件中枢 + 执行节点」**。

---

## 0. 一句话定位

> 一个可自托管的**事件中枢**：把 cron / webhook / watch / agent 回报统一成事件流，用可解释的规则把它变成任务，派给**你机器上的节点**执行，结果再回流成事件——最终把该让人知道的事推到手机上。

## 1. 非目标（比功能清单更重要）

沿用本项目两次成功收缩的尺子（`63f7d79` 砍 reply、`7fd6ffe` 砍 severity：**零消费方的功能 = 纯成本**）：

| ❌ 不做 | 理由 |
| :-- | :-- |
| 任意代码规则 / 脚本沙箱 | 支持成本 + 无法解释「为什么没触发」 |
| 工作流 DAG / 条件分支 / 汇聚节点 / 循环语法 | Temporal 的活。本期只做「事件驱动的 1:1 多跳派发」 |
| **扇出（一个事件派多个任务）** | **本期不做**（owner 决定，§8.5b）。表结构预留，后期放开无需迁移 |
| 中枢下发命令行 | 见 §9，这是最重要的安全不变量 |
| 中枢存 agent 全量日志 | 体积无界 + secret 泄漏面 |
| 插件 SDK / 可安装外部触发器 | 本期只要通用 webhook |
| 现在换 Postgres / 拆微服务 / 上 K8s | 单二进制自托管是核心卖点 |
| 声明式 reconcile / 期望状态收敛 | LLM agent run 有副作用、昂贵、不可幂等 |
| 双向对话（人回复 → 注入 agent） | `63f7d79` 已否决 |
| 多租户实现（本期） | 只留占位（§13） |

---

## 2. 概念模型（8 个概念，先记这个）

```text
Source（触发源）──产生──▶ Event（事件）──匹配──▶ Rule（规则）
                            ▲                      │
                            │                      ▼
                       Task（任务）◀───────── 命中则创建（本期 1 条规则 → 1 个任务）
                            │
                            ▼
              Node（身份）+ Instance（执行器）────▶ TaskResult
                            │                      │
                            │                      └──回流成 Event（chaining）
                            ▼
                      Sink（出口）──▶ 人或外部系统
```text

| # | 概念 | 一句话 | 关键约束 |
| :-- | :-- | :-- | :-- |
| 1 | **Source** | 事件的生产者（cron / webhook / watch / agent） | 有独立 secret 与启用开关 |
| 2 | **Event** | 不可变的、已去重的事实记录 | `payload` 不可查询；只有 `attributes` 可路由 |
| 3 | **Rule** | 谓词 + 目标 + 动作 | 受限语法，禁止代码；每子句可解释 |
| 4 | **Task** | 一次待执行的工作 | 带租约、幂等键、可取消 |
| 5 | **Node** | 执行任务的机器 | 出站长连接；标签由中枢赋值 |
| 5b | **Node Instance** | 一次运行中的节点进程 | 同一 node 可多实例共享 token；各自申报并发（§12.3） |
| 6 | **Sink** | 事件出口 | 分 human / machine 两类，语义不同 |
| 7 | **Correlation** | 一条因果链 | 环检测、深度、预算的作用域 |
| 8 | **Tenant** | 隔离边界 | 本期恒为 `default` |

---

## 3. 数据模型

### 3.1 表清单

| 表 | 作用 | 状态 |
| :-- | :-- | :-- |
| `tenants` | 租户 | 新增（开源版一行 `default`） |
| `sources` | 触发源实例 + 调度租约 | 新增 |
| `events` | 事件日志（唯一事实源） | 新增，取代 `messages` |
| `rules` | 规则 | 新增 |
| `nodes` | 节点注册 + 标签 + 租约 | 新增 |
| `tasks` | 任务 + 租约 + 结果 | 新增 |
| `traces` | 因果链状态 | 新增 |
| `deliveries` | 投递/执行观测（human+machine 共用） | 改造 |
| `mutes` | 静音与节流规则 | 新增 |
| `users` / `passkeys` / `sessions` / `api_keys` | 账号与凭据 | **原样保留** |
| `devices` | 推送设备 | **保留，删除 `event_scope` 列** |
| `cli_auth_sessions` / `recovery_codes` | 设备授权 / 恢复码 | **原样保留** |
| `messages` | 旧通知表 | **彻底删除**（DB 清空重建，见 §14.1） |

### 3.2 核心表 DDL 草案

```sql
-- 事件日志：唯一事实源
CREATE TABLE events (
  id             TEXT PRIMARY KEY,          -- evt_<ksuid>
  tenant_id      TEXT NOT NULL DEFAULT 'default',
  seq            INTEGER NOT NULL,          -- 全局单调，事务内生成，= SSE/WS 的 offset
  source         TEXT NOT NULL,             -- github | cron | watch | webhook | agent
  source_ref     TEXT,                      -- src_xxx；外部 webhook 为 NULL
  type           TEXT NOT NULL,             -- 源命名空间内的类型，如 pr.opened
  subject        TEXT,                      -- 给人看 + 深链的目标，如 acme/api#123
  summary        TEXT,                      -- 一句话人类可读摘要，≤512B（供通知投影/列表）
  occurred_at    INTEGER NOT NULL,          -- 源侧时间（秒）
  received_at    INTEGER NOT NULL,          -- 中枢时间（迟到检测靠它）
  dedupe_key     TEXT NOT NULL,
  attributes     TEXT NOT NULL,             -- JSON，扁平 string→string，≤32 键
  payload        TEXT,                      -- 原始 JSON，截断至 64KB
  payload_bytes  INTEGER NOT NULL DEFAULT 0,
  payload_sha256 TEXT,                      -- 完整原文的哈希（证明未篡改）
  payload_truncated INTEGER NOT NULL DEFAULT 0,
  trace_id TEXT NOT NULL,             -- 链根事件 id
  caused_by      TEXT,                      -- 父事件 id
  -- ★ trace 元数据进普通字段（§12.2.4）
  trace_id       TEXT NOT NULL,
  span_id        TEXT NOT NULL,
  parent_span_id TEXT,
  hop            INTEGER NOT NULL DEFAULT 0,
  rule_version_id TEXT,                     -- 产生此事件时的规则版本
  expires_at     INTEGER,
  UNIQUE(tenant_id, source, dedupe_key)
);
CREATE INDEX idx_events_trace ON events(trace_id, seq);
CREATE INDEX idx_events_span  ON events(span_id);

CREATE TABLE tasks (
  id              TEXT PRIMARY KEY,         -- tsk_<ksuid>
  tenant_id       TEXT NOT NULL DEFAULT 'default',
  event_id        TEXT NOT NULL REFERENCES events(id),
  rule_id         TEXT NOT NULL REFERENCES rules(id),
  trace_id  TEXT NOT NULL,
  -- ★ 执行树结构（§8.6）：一个 trace 是树/DAG，不是单链
  parent_task_id  TEXT REFERENCES tasks(id), -- OTel parent_span_id（按 span 而非 task 记）
  inputs          TEXT NOT NULL DEFAULT '[]', -- OTel Span Links：多上游合批时指向所有上游
  span_id         TEXT NOT NULL,            -- OTel span（= task.id 的后半段）
  hop             INTEGER NOT NULL,         -- 签名字段，签发时确定
  path            TEXT NOT NULL,            -- 祖先 rule_id 路径，如 /rev-pr/impl-fix/ ；环检测就靠它
  depth           INTEGER NOT NULL,         -- 参与执行的任务数（扇出不计），= G2 计量单位
  node_id         TEXT REFERENCES nodes(id),   -- 身份（标签来自这里）
  instance_id     TEXT REFERENCES node_instances(id),  -- 实际执行者
  manifest_version_id TEXT REFERENCES node_manifest_versions(id),  -- 派发时的节点能力版本（nmf_*）
  internal_attempts   INTEGER NOT NULL DEFAULT 0,  -- 节点内部重试次数（仅展示，§12.2.6）
  action          TEXT NOT NULL,            -- 渲染后的 action JSON（不含 argv）
  status          TEXT NOT NULL,            -- pending|assigned|acked|running|awaiting_children|lease_expired|succeeded|failed|canceled|abandoned
  attempts        INTEGER NOT NULL DEFAULT 0,
  lease_owner     TEXT,                     -- ★ node_instance.id（nin_*），精确到进程
  lease_until     INTEGER,
  not_before      INTEGER,                  -- 退避
  result          TEXT,                     -- TaskResult JSON，≤8KB
  error           TEXT,
  created_at      INTEGER NOT NULL,
  started_at      INTEGER,
  finished_at     INTEGER,
  idempotency_key TEXT NOT NULL UNIQUE,     -- 规则派生: sha256(event_id ∥ rule_id)
  sticky_key      TEXT,                     -- sha256(trace_id ∥ rule_id ∥ selector)（§12.4）
  pinned_node_id  TEXT REFERENCES nodes(id),-- 粘性路由落点（§12.4.2）
  -- ★ 任务级生命周期属性（§12.4.8）
  expires_at      INTEGER,                  -- 硬过期：到点直接 abandoned，绝不重派
  retryable       INTEGER NOT NULL DEFAULT 1,  -- 0 = 一次性，失败不重派
  max_attempts    INTEGER,                  -- NULL → 中枢默认 3
  result_ttl_sec  INTEGER
                                            -- 节点派生: token.jti（一次性消费，见 branch_nonces）
);

-- 类型注册表版本（§12.2.3）
CREATE TABLE registry_versions (
  id            TEXT PRIMARY KEY,           -- rty_20261023T101500Z_3c4d5e6f
  registry      TEXT NOT NULL,              -- 整份注册表 JSON
  content_hash  TEXT NOT NULL UNIQUE,
  effective_at  INTEGER NOT NULL
);

-- 版本化快照（§12.2）：append-only，永不修改
CREATE TABLE rule_versions (
  id            TEXT PRIMARY KEY,           -- reg_20261023T101500Z_a1b2c3d4（§12.2.1）
  tenant_id     TEXT NOT NULL DEFAULT 'default',
  rules         TEXT NOT NULL,              -- 全部规则的完整快照 JSON
  content_hash  TEXT NOT NULL UNIQUE,       -- 内容相同则不建新版本
  effective_at  INTEGER NOT NULL,           -- UTC 秒
  created_by    TEXT,
  change_note   TEXT
);

-- 汇聚（§8.6.1）本期不需要新表：用 tasks.parent_task_id + 父终态推导。
-- 后期 fanout 机制预留位置见 §8.6.1-1（本期不建表）。

-- 粘性路由（§12.4.2）：首次 hash 选节点后记住，后续查表
CREATE TABLE node_pins (
  sticky_key   TEXT PRIMARY KEY,            -- sha256(trace_id ∥ rule_id ∥ selector)
  node_id      TEXT NOT NULL REFERENCES nodes(id),
  trace_id     TEXT NOT NULL REFERENCES traces(id),
  rule_id      TEXT NOT NULL,
  pinned_at    INTEGER NOT NULL,
  stale        INTEGER NOT NULL DEFAULT 0   -- 连续 N 次超时后置 1（§12.4.4）
);
CREATE INDEX idx_node_pins_node ON node_pins(node_id);
CREATE INDEX idx_node_pins_trace ON node_pins(trace_id);

-- 汇聚不需要新表：用 tasks.parent_task_id 计数即可（§8.6.1）

-- 执行单元（§12.3.2）：一个 node 可有多个实例共享同一 token
CREATE TABLE node_instances (
  id           TEXT PRIMARY KEY,            -- inst_<ksuid>
  node_id      TEXT NOT NULL REFERENCES nodes(id),
  instance_key TEXT NOT NULL,               -- hash(hostname + pid/containerId)
  reported     TEXT NOT NULL DEFAULT '{}',  -- {os, arch, daemonVersion}
  inflight     INTEGER NOT NULL DEFAULT 0,
  concurrency  INTEGER NOT NULL DEFAULT 1,  -- ★ 实例自报的并行能力
  status       TEXT NOT NULL DEFAULT 'offline',  -- online|offline|draining
  last_seen_at INTEGER,
  UNIQUE(node_id, instance_key)
);

CREATE TABLE node_manifest_versions (
  id            TEXT PRIMARY KEY,           -- nmf_20261023T101500Z_9f2c1b7e
  node_id       TEXT NOT NULL REFERENCES nodes(id),
  manifest      TEXT NOT NULL,
  content_hash  TEXT NOT NULL,
  effective_at  INTEGER NOT NULL,
  UNIQUE(node_id, content_hash)
);

-- 一次性消费的派生 nonce（§8.3.4）
CREATE TABLE branch_nonces (
  jti         TEXT PRIMARY KEY,             -- token 的 jti，UNIQUE 保证一次性
  trace_id    TEXT NOT NULL,
  span_id     TEXT NOT NULL,
  consumed_at INTEGER NOT NULL
);

CREATE TABLE nodes (
  id           TEXT PRIMARY KEY,            -- nod_<ksuid>
  tenant_id    TEXT NOT NULL DEFAULT 'default',
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL,               -- argon2id
  token_prefix TEXT NOT NULL,
  -- ★ 路由用标签：中枢赋值，节点不可自报（§9.3）
  labels       TEXT NOT NULL DEFAULT '{}',
  -- 仅展示用：节点上报的客观信息
  reported     TEXT NOT NULL DEFAULT '{}',  -- {os,arch,version,daemonVersion}，仅展示
  capabilities TEXT NOT NULL DEFAULT '[]',  -- capability manifest（§12.1），含 name/costClass/params/produces
  manifest_version INTEGER NOT NULL DEFAULT 1,
  status       TEXT NOT NULL DEFAULT 'offline', -- online|offline|draining
  max_concurrency INTEGER NOT NULL DEFAULT 1,   -- ★ 池级并发上限（中枢赋值，见 §12.3.3）
  max_instances   INTEGER NOT NULL DEFAULT 20,  -- ★ 防 token 扩散：单 node 最大实例数
  last_seen_at INTEGER,
  enrolled_at  INTEGER NOT NULL,
  revoked_at   INTEGER
);

CREATE TABLE traces (
  id             TEXT PRIMARY KEY,         -- cor_xxx，= 链根事件 id
  tenant_id      TEXT NOT NULL DEFAULT 'default',
  root_event_id  TEXT NOT NULL REFERENCES events(id),
  status         TEXT NOT NULL,            -- active|settled|failed|paused|expired
  hop_max        INTEGER NOT NULL DEFAULT 0,   -- 观测到的最大深度
  task_count     INTEGER NOT NULL DEFAULT 0,   -- 已创建任务数（展开后的原子任务，正确定额）
  paid_count     INTEGER NOT NULL DEFAULT 0,   -- 保留：仅统计展示，不参与闸门（§12.2.5）
  -- 预算快照：创建时从规则/全局解析后固化，避免改配置影响在飞的链
  max_hops       INTEGER NOT NULL,         -- G2：最深 path 长度
  max_tasks      INTEGER NOT NULL,         -- G4：task_count 上限
  max_paid_tasks INTEGER,                  -- 已废弃（G5 删除，§12.2.5）；保留列以便回滚
  max_duration   INTEGER NOT NULL,         -- G6：墙钟龄上限
  blocked_reason TEXT,                     -- 撞哪道闸：depth|tasks|paid|duration|cycle
  -- ★ 版本钉住（§12.2.2）：trace 诞生时固化，之后永不改变
  -- ★ 版本钉住（§12.2.2）：钉住不可变 id，而不是时间
  rule_version_id     TEXT NOT NULL REFERENCES rule_versions(id),
  registry_version_id TEXT NOT NULL REFERENCES registry_versions(id),
  settled_at     INTEGER,
  created_at     INTEGER NOT NULL
);
```text

### 3.3 关键设计：`seq` 而非时间戳做 offset

沿用现有 broker 的事务内 `MAX(seq)+1` 技巧（`internal/broker/sqlite.go:84-90`），依赖 `SetMaxOpenConns(1)` 保证串行化。

- 好处：SSE / 节点 WS 的续传只需一个整数 `Last-Event-Id`
- 风险：放宽连接池即冲突。**在 schema 注释里写明这个耦合**，若将来换 Postgres 改用 `BIGSERIAL`

---

## 4. 事件信封

```jsonc
{
  "id": "evt_2fQ...",
  "seq": 91824,
  "source": "github",
  "sourceRef": "src_8aK...",
  "type": "pr.opened",
  "subject": "acme/api#123",
  "summary": "alice 打开了 PR #123：重构事件层",
  "occurredAt": "2026-10-23T10:00:00Z",
  "receivedAt": "2026-10-23T10:00:01Z",
  "attributes": {
    "repo": "acme/api",
    "author": "alice",
    "branch": "feat/x",
    "action": "opened"
  },
  "payload": { "...": "源原始 JSON，截断至 64KB" },
  "payloadBytes": 24188,
  "payloadSha256": "9f2c...",
  "traceId": "cor_...",
  "causedBy": null,
  "hop": 0
}
```text

### 4.1 三条硬约束

| 约束 | 值 | 为什么 |
| :-- | :-- | :-- |
| `attributes` 扁平、有界 | ≤32 键，键 ≤64B，值 ≤256B，只能 string | 唯一可索引/可路由/可筛选的层。有界才能建索引 |
| `summary` 短 | ≤512B，单行，纯文本 | 通知投影与控制台列表只需要一句话；不做索引 |
| `payload` 黑盒 | ≤64KB，超出截断 + 存 `sha256` + `payload_truncated=true` | 防止 GitHub 大 payload 把 DB 撑爆 |
| 去重 | `(tenant, source, dedupe_key)` UNIQUE，重复返回 200 | 不让 webhook 重发变成重复执行 |

### 4.2 `attributes` 从哪来

每种 source 有固定的**抽取配置**：

```jsonc
// github source 的抽取映射
{ "repo": "repository.full_name",
  "author": "pull_request.user.login",
  "branch": "pull_request.head.ref",
  "action": "action",
  "number": "pull_request.number" }
```text

通用 webhook 由用户在创建 source 时填这张映射表（受限 JSONPath 子集：`a.b.c` 与 `a.0.b`，不支持表达式/函数）。

---

## 5. 规则语言

### 5.1 语法：谓词树，最大深度 2

```jsonc
{
  "match": {
    "all_of": [
      { "field": "source",              "op": "eq",     "value": "github" },
      { "field": "type",                "op": "in",     "value": ["pr.opened", "pr.synchronize"] },
      { "field": "attributes.repo",     "op": "eq",     "value": "acme/api" },
      { "field": "attributes.author",   "op": "not_in", "value": ["renovate[bot]"] },
      { "any_of": [                                     // 允许一层 OR
        { "field": "attributes.branch", "op": "prefix", "value": "feat/" },
        { "field": "attributes.labels", "op": "contains", "value": "urgent" }
      ]}
    ]
  },
  "target":  { "node_selector": { "labels": { "gpu": "true" } } },
  "action": {
    "kind": "cmd",
    "name": "review",
    "params": { "prompt": "审查 {{subject}}：{{payload.pull_request.title}}" }
  },
  "allow_chain": false,
  "on_failure": "retry_step",
  "max_attempts": 3,
  "daily_budget": 50
}
```text

**运算符全集**（就这 11 个，不再增加）：
`eq` · `ne` · `in` · `not_in` · `prefix` · `suffix` · `contains` · `exists` · `gt` · `lt` · `glob`

**模板上下文**（只读，白名单字段）：

| 变量 | 内容 |
| :-- | :-- |
| `{{id}}` `{{type}}` `{{source}}` `{{subject}}` | 事件基本字段 |
| `{{attributes.X}}` | 单个属性 |
| `{{payload.a.b}}` | 受限路径取值（结果 > 2KB 截断） |
| `{{parent.summary}}` `{{parent.artifacts.X}}` | **上一跳任务的结果**（仅 chaining 场景） |

### 5.2 杀手特性：逐子句解释

`POST /v1/rules/{id}/explain?event=evt_x` 返回：

```text
rule "rev-pr" against evt_2fQ (github/pr.opened):

  source == "github"                         ✅
  type in [pr.opened, pr.synchronize]        ✅  (pr.opened)
  attributes.repo == "acme/api"              ✅
  attributes.author not_in [renovate[bot]]   ❌  (author = "renovate[bot]")

  → 未命中。仅因 1 个条件。
```text

**这份可解释性就是本产品相对 n8n 类工具的真正差异点**，而且实现便宜（谓词树求值时收集 trace）。

### 5.3 `target` 的三种形态

| 类型 | 语义 |
| :-- | :-- |
| `{ "node_selector": { "labels": {...}, "capability": "review" } }` | 派给匹配的节点 |
| `{ "sink": "webpush" }` | 直接投递给人类设备 |
| `{ "sink": "http-hook", "hook_id": "hk_..." }` | 回调外部系统 |

> ⚠️ **`target: {sink: webpush}` 是陷阱，不推荐使用**。直接推原文会绕开 §10.2 的通知收敛，5 跳链变成 5 条推送。正确做法：不写 `target`，让事件走内建 system rule（§10.3）。本节保留它只为说明「能直接投递」这件事是存在但不鼓励的。

---

## 5.4 纯通知场景：`notify` 规则模板（取代 `/v1/notify`）

> 背景：本设计**彻底删除 `POST /v1/notify`**（Owner 决定，无兼容适配器，插件仓库同步改造）。
> 但旧产品最宝贵的用法——**一条 curl 就通知我**——不能因此消失，否则新用户上手门槛会变高。

**解法：把通知从「端点」降级为「规则模板」。** 控制台新建规则时选「通知我」，自动生成：

```jsonc
{
  "name": "通知我（默认）",
  "match": { "all_of": [ { "field": "source", "op": "eq", "value": "api" } ] },
  "action": { "kind": "notify", "title": "{{subject}}", "body": "{{summary}}" },
  "allow_chain": false
}
```text

用户只需：

```bash
curl -X POST https://hub/v1/events \
  -H "Authorization: Bearer evt_..." \
  -H "Content-Type: application/json" \
  -d '{"source":"api","type":"deploy.done","subject":"部署完成","summary":"构建成功，用时 3 分钟"}'
```text

就得到通知。两行、零前置配置（`source: api` 是内建 source，不需要先创建）。

**这比原来的 `/v1/notify` 更强，因为同一个端点还能触发 agent**：

```bash
data='{"source":"api","type":"deploy.done","subject":"部署完成","summary":"...",
       "attributes":{"env":"prod"},"payload":{"commit":"a1b2c3d"}}'
curl -X POST https://hub/v1/events -H "Authorization: Bearer evt_..." -d "$data"
```text

配第二条规则（`attributes.env == prod` → `cmd:deploy-verify`），同一条 curl 既通知人又唤起 agent。

**与旧 `/v1/notify` 的差异（迁移说明唯一需要写的部分）**：

| 旧 | 新 |
| :-- | :-- |
| `POST /v1/notify` | `POST /v1/events` |
| `{title}` | `{subject}`（+ `{summary}` 可选，默认回退到 `subject`） |
| `{agentState: "done"}` | `{type: "task.done"}`（自由字符串，不再枚举） |
| `{deviceTags: [...]}` | `{attributes: {tags: "..."}}`（逗号分隔字符串，因为 attributes 值只能 string） |
| `{link}` | `{attributes: {link}}` |
| 自动推送 | 需配一条 `kind: notify` 规则（控制台一键建） |

> `agentState` 的五个枚举值（`working|blocked|done|interrupted|error`）与 `IsTerminal()` 随 `messages` 一起删除：事件类型是**开放字符串**，不再由 hub 枚举。这是转向带来的真实好处：hub 不再替用户定义「什么算一个状态」。

---

## 6. 任务与租约

### 6.1 状态机

```text
                    ┌──────────────────────────────────────┐
                    ▼                                      │
  [created] ──▶ pending ──lease──▶ leased ──ack──▶ running ─┤
                    ▲                                      │
                    │                                      ├──▶ succeeded
                    │                                      ├──▶ failed ──┐
                    │                                      └──▶ canceled │
                    │                                                     │
                    └──(租约过期 → lease_expired, attempts<max)───────────┤
                                                                          │
                    (attempts ≥ max) ──▶ abandoned（进 DLQ，可人工重投） ◀┘
```text

| 规则 | 值 |
| :-- | :-- |
| 租约 TTL | 30s（节点心跳即续租，10s 间隔） |
| 默认 `max_attempts`（**中枢重派**） | 3 |
| 节点内部重试 | 由 manifest `commands[].retries` 定义，**不消耗中枢 attempts**（§12.2.6） |
| 退避 | 10s → 60s → 300s |
| 幂等 | `UNIQUE(event_id, rule_id)`——重复投递永不重复花钱 |
| 取消 | 节点收到 `cancel` 必须 kill 整个进程树，回报 `canceled` |
| 租约过期处理 | 后台 sweep（**不是**内存 ticker，走 DB 行扫描），回 `pending` 并 `attempts++` |

### 6.2 `TaskResult`（上行，硬上限 8KB）

```jsonc
{
  "taskId": "tsk_...",
  "attempt": 1,
  "status": "succeeded",
  "startedAt": "...", "finishedAt": "...",
  "summary": "重构完成，12 文件变更，测试全绿",   // ≤2KB
  "exit": { "code": 0 },
  "artifacts": [
    { "kind": "commit", "label": "a1b2c3d", "url": "https://..." },
    { "kind": "pr",     "label": "#124",   "url": "https://..." }
  ],
  "logHint": { "localPath": "~/.anotifyd/logs/tsk_xxx.log", "bytes": 48213 }
}
```text

- **中枢不存全量日志**。调试靠 `POST /v1/tasks/{id}/logs-request`，节点按需上传尾部 N KB（默认关，显式开启，可审计）
- `summary` + `artifacts` 是控制台唯一渲染源

### 6.3 状态归属：谁观察到的事实，就由谁权威（owner 修正）

> owner 提出：**下发消息需要节点 ack，ack 表示任务完成，没有 ack 的就是执行中。**

这个想法的方向对，但直接落地会与本设计的一条安全不变量冲突，必须说清：

#### 6.3.1 为什么不能「没 ack = 执行中」

**WS 连接会断**（笔记本合盖、Wi-Fi 切换、代理重启）。如果「没 ack = 执行中」，那断线期间所有任务都显示「执行中」——**包括那些其实已经跑完、甚至节点机器已经关机的**。

更关键的问题是：**「任务还在跑」这件事，中枢无法观察，节点能观察。**

于是按 §9 的同一条原则（谁有权威），状态必须这样切：

| 状态 | 谁能观察到 | 权威方 | 中枢能否推断 |
| :-- | :-- | :-- | :-- |
| `running`（进程还活着） | **只有节点** | **节点心跳里的 `inflight`** | ❌ 推断不了 |
| `acked`（收到派发了） | 只有节点 | 节点的第一帧 `ack` | ✅ 有 ack 即知 |
| `lease_expired`（联系不上了） | 只有中枢 | 中枢的租约 | ✅ 纯本地判断 |
| `succeeded` / `failed` | 只有节点 | 节点的 `result` | ❌ |
| `canceled` | 双方 | 中枢发起、节点确认 | ✅ |

**所以 `lease_expired` 必须是一个明确可展示的状态**，而不是含糊的「执行中」。这是本设计相对「ack 模型」的关键修正：

```text
pending ──assign──▶ assigned ──ack──▶ acked ──start──▶ running ──result──▶ succeeded|failed
                        │                                  │
                        │ (无 ack, 租约过期)                │ (心跳消失, 租约过期)
                        ▼                                  ▼
                    lease_expired  ──(attempts<max)──▶ pending
                                   ──(attempts≥max)──▶ abandoned
```text

**用户在控制台看到的文案必须区分：**

| 显示 | 含义 | 用户该做什么 |
| :-- | :-- | :-- |
| 🟡 执行中 | 有活跃心跳，节点报告 `inflight` 含此任务 | 等 |
| 🟠 **失联中**（`lease_expired`） | 该任务最后一次心跳后超过 2×hb | 可能节点挂了；可手动取消或等自动重派 |
| ⚪ 已派发待确认 | 已 assign，未收到 ack（**短窗口，≤hb**） | 等（这个才真的是「没 ack 就是执行中」） |

#### 6.3.2 帧定义补全

```text
节点 → 中枢
  ack         { taskIds: [...] }        # 收到 assign 即发；语义是「我接下了」不是「我完成了」
  start       { taskId, startedAt }     # 进程真正拉起（可选：ack 后立刻拉起时省略，由 ack 隐含）
  heartbeat   { inflight: [{taskId, progress?}] }   # ★ 这是 running 的唯一权威来源
  result      { taskId, attempt, status, summary, exit, artifacts, logHint }
```text

**三条硬规则：**

1. **`ack` ≠ 完成**。`ack` 只表示「我收到了派发」，中枢据此把 `assigned` → `acked`，**不代表任务在跑**
2. **`running` 只能由心跳的 `inflight` 维护**。心跳里没列的任务，中枢立即视为不在跑（不是等租约过期）
3. **`result` 是唯一的终态来源**。没有 `result` 就永远不会有 `succeeded`

这样「ack 表示任务完成」被拆成两件不同的事：`ack` = 接单确认，`inflight` = 正在跑，`result` = 完成。三者都有才有完整信息，缺 `result` 就是没完成。

#### 6.3.3 超时语义

| 超时 | 阈值 | 触发动作 |
| :-- | :-- | :-- |
| assign → ack | `2 × heartbeatSec`（默认 20s） | 视为节点拒绝/失联 → 回 `pending` |
| 最后一次心跳 | `2 × heartbeatSec` | `running` → `lease_expired` |
| 租约 TTL | 30s | `leased/acked/running` 全部可被重派（`attempts++`） |
| `deadline`（action 自带） | `action.timeoutSec` | 节点自身必须 kill 进程树并回报 `failed{reason:timeout}` |

**注意最后一个**：任务超时应该由**节点**报（它才知道进程超时），中枢的 `deadline` 只是兜底租约。

---

## 7. 节点协议（出站 WebSocket）

### 7.1 为什么复用现有 WS 骨架

现有 `GET /v1/stream` 的帧设计与节点需求几乎一一对应（`internal/ws/handler.go:85-271`）：

| 现有能力 | 节点需要 |
| :-- | :-- |
| `hello` 下发心跳间隔 | ✅ 直接复用 |
| `Last-Event-Id` / `resume` 续传 | ✅ 任务补派用得上 |
| 上行 `ping` 续命、2×hb 无活动断开 | ✅ 死连接检测 |
| `ack` 帧 + `consumer_offsets` | 🔧 改成任务 ack |

**结论：保留骨架，重写帧与状态机。**

### 7.2 帧定义

```text
节点 → 中枢
  hello       { nodeToken, instanceKey, manifest:{…}, reported:{os,arch,version}, concurrency }
  heartbeat   { inflight: [taskId...] }        # 同时续租所有 inflight
  ack         { taskIds: [...] }               # 收到 assign 后确认
  result      { taskId, attempt, status, summary, exit, artifacts, logHint }
  logChunk    { taskId, offset, data }         # 仅在 logs-request 后
  manifest    { commands: […] }                # 配置变更时可重发，无需重连
  bye         { reason }

中枢 → 节点
  welcome     { nodeId, instanceId, poolMaxConcurrency, leaseTtl, heartbeatSec, serverTime }
  assign      { taskId, action, deadline, attempt }
  cancel      { taskId, reason }
  logsRequest { taskId, tailBytes }
  drain       { }                              # 排空：不再接新任务
  bye         { reason }
```text

### 7.3 鉴权

- 节点用**独立凭据类型**（不是 `notify:send` key）。`POST /v1/nodes/enroll` 生成一次性 enroll token → 首次连接换取长期 token（仅存 argon2id 哈希）
- 可轮转、可按节点吊销（`nodes.revoked_at`）
- WS 鉴权走 `Authorization: Bearer` 或 `?node_token=`（受限客户端）

### 7.4 断线与补派

1. 中枢发现节点离线（无心跳 > 2×hb）→ `nodes.status='offline'`
2. 该节点所有 `leased/running` 任务：**不立即释放**（可能仍在跑），等租约 TTL 过期
3. 节点重连 → `hello` 带 `inflight` → 中枢对账：
   - 节点还在跑 → 续租，保留 `running`
   - 节点已丢 → 立即置 `pending`，不等 TTL
4. `pending` 任务重新选节点（selector 排除离线节点）

**这一步是必须的**：只靠 TTL 会让离线节点的任务白等 30s；只靠 `inflight` 会因节点崩溃而永久泄漏。

---

## 8. Chaining（sink 输出作为 source）

### 8.1 因果链

事件新增 `traceId` / `causedBy` / `hop`。任务终态时**派生**一条新事件：

```jsonc
{ "source": "agent", "type": "task.succeeded", "subject": "tsk_...",
  "attributes": { "rule": "rev-pr", "node": "nod_x", "agent": "pi", "outcome": "succeeded" },
  "payload": { /* TaskResult 摘要 */ },
  "traceId": "cor_...", "causedBy": "evt_父", "hop": 1 }
```text

于是 **chaining = 规则匹配自己产生的事件**，零新增执行引擎。

### 8.2 传指针，不传载荷

```yaml
params:
  prompt: "按上一轮审查意见修复。摘要：{{parent.summary}}\n分支：{{parent.artifacts.branch}}"
```text

**铁律**：A 传给 B 的是**指针**（commit / branch / PR / 文件路径），不是 A 的上下文。

三个理由：① 中枢本就不存全量日志；② 让 agent 通过**共享世界状态**协作比搬运上下文更可靠、更省 token、更可审计；③ 指针可验证，搬运的上下文不可验证。

> 想把 A 的长文本塞进 B 的 prompt 会先撞 8KB 上限——**这是故意的保护**。

### 8.3 六道闸（缺一不可）

| 闸 | 规则 | 默认（可配） | 防什么 |
| :-- | :-- | :-- | :-- |
| **G1 显式 opt-in** | 规则须声明 `allow_chain: true` 才能被 `task.*` 触发 | `false`（**不可配**） | 意外环路（A 的完成事件又触发 A） |
| **G2 深度** | `path` 中同一 `rule_id` **不重复**（等价于 `depth ≤ max_hops`） | `10` | **无限环** + 拓扑失控 |
| **G3 环检测** | **不用去重表** —— 环检测就是 G2 的路径检查：`rule_id ∈ path` → 剪枝 | 隐含在 G2（**不可配**） | A→B→A |
| **G4 单链任务数** | 链内原子任务总数 ≤ `max_tasks` | `100` | 扇出放大 |
| ~~G5 付费任务数~~ | **已删除**（§12.2.5）—— 成本改由 G4 `max_tasks` + 每规则 `daily_budget` 承担 | — | **钱** |
| **G6 链龄** | `now − created_at ≤ max_duration` | `24h` | 卡死的链 |

#### 可配性：三层覆盖 + 代码硬顶

```text
全局默认    ANOTIFY_MAX_HOPS=10                ANOTIFY_MAX_TASKS_PER_TRACE=100
           ANOTIFY_MAX_TRACE_DURATION=24h
              ↓ 被覆盖
单规则       rules.max_hops / max_tasks / max_duration   （NULL = 用全局）
              ↓ 被封顶
硬顶（代码）  32                                  1000
            7d
```text

硬顶存在的理由是**放大系数的物理边界**：10 跳 × 3 扇出已是 5 万条潜在任务，没有 `max_tasks` 当场剪枝，一次误配就能把 SQLite 写满。

**硬顶本身不可配**（改代码才能动），其余全部可配。只有 G1（`allow_chain`）和 G3（环检测）是正确性边界、不可配。

> G5 `max_paid_tasks` **已删除** —— 见 §12.2.5。成本改由 `max_tasks` + 每规则 `daily_budget` 承担。

#### ⚠️ G3 的性质变了：从「拦截」改为「剪枝」（owner 决定允许环）

旧设计是「`(rule_id, trace_id)` 已触发过 → 拒绝 + 暂停整条链」。**这是错的**，因为回边是**正常的**：

- 「每日巡检」把自己作为一跳再挂上去
- 「测试失败 → 修复 → 再测试」，直到变绿为止

把它当错误上报，只会把控制台刷满、把 trace 卡成 `paused`。

**新语义：**

| 情况 | 行为 |
| :-- | :-- |
| `rule_id ∈ path`（回边） | **静默剪枝**：不创建任务、不暂停 trace、**不写 `trace.blocked`**；只在 trace 里记一条「已剪枝：路径上已有 `rev-pr`」 |
| `depth > max_hops`（不同规则但太深） | 同上，剪枝并标注原因 |
| `task_count` / `duration` / 每规则 `daily_budget` 超限 | 这些才是**真拦截** → `trace.paused` + 通知（需要人来调预算） |

**关键收益**：不需要「已触发过」去重表。环检测 = 路径前缀检查，一条 `strings.Contains(path, "/"+rule_id+"/")` 就够。旧设计的 `(rule_id, trace_id)` 去重会**误杀正当的多轮迭代**。

**这也是为什么 G2 是唯一防死循环的闸**：只要路径上 rule_id 不重复，循环必然终止。`max_hops` 就是这条规则的上界外显。

#### 8.3.1 修正：签名 + 一次性消费 = 链保证（owner 方案成立）

> ⚠️ 我上一版说「签名完全防不了」，**这个判断是错的**，需要纠正。

**我当时的错误假设**：中枢是无状态签发方 —— 节点只要请求，中枢就签一个新的。

**owner 的实际设计**：中枢**只在一个 token 被合法消费时**才签下一个。于是：

| 环节 | 效果 |
| :-- | :-- |
| JWT 里签 `hop` | 节点改不了跳数（改动即验签失败） |
| **中枢收到 `ack` 后不再接受同一 token** | **同一个父任务只有一个有效子 token** |
| 中枢签发时 `hop = 父.hop + 1` | 无法跳过中间跳（不存在 `hop=5` 的 token 而 `hop=2` 未走） |

**这三条加起来，比我上一版提的「path 字符串 + 预算簿记」更强**：

| 攻击 | 上一版 path 方案 | **owner 的 JWT + 一次性消费** |
| :-- | :-- | :-- |
| 篡改 hop | ✅ 签名 | ✅ 签名（更强：hop 是数值，不是字符串） |
| 跳过几跳 | ✅ path 前缀检查 | ✅ **结构上不可能**（没有中间 token） |
| 同一 token 用 10 次做指数爆炸 | ⚠️ 靠 `max_tasks` 兜 | ✅ **结构上不可能**（token 一次性） |
| 线性无限链 | ⚠️ 靠 `max_hops` | ✅ `hop ≤ max_hop` 在签发时判定 |

**所以我上一版的「10×10×10 指数爆炸」攻击在这个设计下不成立。** 有限状态机 + 一次性凭据，闭环了。

**但还剩三个真问题（都不是签名能解决的）：**

1. **扇出变得不可能**（见 §8.3.2）—— 一个父 token 只能换一个子 token，那 `A → {C, D}` 怎么表达？
2. **nonce 消费的原子性**（见 §8.3.4）—— 消费了却建任务失败 = 这条分支永久丢失
3. **成本仍需独立预算**（见 §12.2.5）—— 线性 10 跳 × 付费 = 依然烧钱，签名管不了钱。解法是 G4 `max_tasks` + 每规则 `daily_budget`（中枢侧计量）

#### 8.3.2 扇出保留，`branchBudget` 保留（owner 修正）

> owner：**扇出数量我们不管，只是本期不做「收集」。**
> 我上一版把扇出和收集当成同一个问题，删掉了 `branchBudget` —— 那是错的。

扇出本期就在（一个事件命中 N 条规则 → N 个任务），所以节点派生时同样可能一次派多个子。**`branchBudget` 因此保留**：

```jsonc
// 本期 token claim（与扇出前一致，全部保留）
{ "trace": "trc_7f2", "span": "spn_a1",
  "hop": 3, "maxHop": 10,
  "branchBudget": 2,          // ★ 保留：还能派几个子；0 = 叶子
  "exp": …, "jti": "…" }
```

| 机制 | 本期是否需要 | 说明 |
| :-- | :-- | :-- |
| `branchBudget` claim | ✅ **需要** | 派生可能一次派多个（扇出），需要额度 |
| G3b 扇出闸（§8.3.6） | ✅ **需要** | 防止单次派生爆炸 |
| `jti` 一次性消费 | ✅ **需要** | 防重放 |
| 「额度用尽可见」（409 + span event + 控制台 🚫） | ✅ **需要** | 绝不静默失败 |

**`branchBudget` 与 `jti` 是互补的，不是二选一：**

| 机制 | 防什么 |
| :-- | :-- |
| `jti` 一次性 | 同一个 token 被**重放** |
| `branchBudget` 递减 | 单个 token 派生**太多** |
| 两者合起来 | 「派生次数既不可重放、也不可超量」 |

**本期不做的是「收集」，不是「扇出」**（§8.5b）。所以额度机制**全部保留**；只是没人去数「派出去的都回来了吗」。

#### 8.3.2b 额度分配：由节点自主决定（owner 选 A，本期有效）

`branchBudget` 是**树的剩余总预算**，不是每层的。所以 A 拿到 2 之后怎么分，完全由节点决定：

| A 的分配 | 结果 |
| :-- | :-- |
| C=1, D=1 | C 和 D 各自还能再派 1 个 |
| C=2, D=0 | D 变成叶子 |
| C=0, D=0 | 都是叶子 → 这条线在这里结束 |

**中枢只做两件事：验签、递减。不参与分配。**

这与模型 B 的精神一致：节点知道自己的业务，中枢不知道。中枢不猜「这条链还需要几次扇出」。

**⚠️ 但必须让「额度用光」可见，不能静默失败。**

```text
节点请求派生 → 中枢返回 409 { error: "budget_exhausted", remaining: 0 }
```

同时写 span event：`{ "span": "spn_a1", "event": "budget_exhausted", "detail": { "requested": 1, "remaining": 0 } }`

**为什么这条必须做**：否则用户看到「某条线断了」，但 trace 上一片绿、没有任何错误 —— 最坏的一种可观测性缺口。

#### 8.3.3 身份 = trace + span（owner 方案，比序号更好）

> owner 纠正：**「这条消息不是在流转吗，id 由两部分组成，一部分 trace 一部分 span，每次生成一个新的 span」**

对，这比「节点侧单调序号」更简洁，而且和 §8.7 的 OTel 模型天然一致：

```text
task.id = "tsk_" + trace8 + "_" + span8      // 例：tsk_cor7f2a_spn8c21

trace_id  = trace_id     ← 整条链不变
span_id   = 每跳由中枢新生成    ← 天然唯一
```text

**关键收益：幂等键不需要额外字段了。** 中枢建任务时自己生成 `span_id`，所以：

| 派生来源 | 去重依据 | 幂等键 |
| :-- | :-- | :-- |
| 规则派生 | `(event_id, rule_id)` 不能建两次 | `sha256(event_id ∥ rule_id)` |
| **节点派生** | **token 的 `jti` 只能消费一次** | **`jti` 本身** |
| 任务主键 | 中枢生成 span | **`task.id` 天然唯一** |

`branchLocalId` 字段**删除** —— 不需要了。节点的 `jti` 就是它的「我请求过这一次」的证明。

**为什么这比节点侧单调序号好：**

- 单调序号要求节点持久化计数器，重装就冲突（我上一版担心的正是这个）
- `jti` 由中枢在签发 token 时生成并签名，**重装节点无从伪造，也无从重放**（已消费）

#### 8.3.4 真正需要小心的地方：nonce 消费与建任务的原子性

一次性消费有个经典陷阱：**消费了 nonce，但建任务失败** → 这条分支永久丢失，且节点无法重试（`jti` 已作废）。

**所以两步必须在同一个事务里：**

```sql
BEGIN;
  -- 1. 消费 nonce（唯一索引，冲突即说明已被用过）
  INSERT INTO branch_nonces(jti, trace_id, span_id, consumed_at)
    VALUES(?, ?, ?, ?);            -- UNIQUE(jti) 冲突 → 回滚，返回已存在的任务

  -- 2. 建任务
  INSERT INTO tasks(id, ..., span_id, hop, ...) VALUES(...);

  -- 3. 扣减 trace 预算
  UPDATE traces SET task_count = task_count + 1 WHERE id = ?;
COMMIT;
```text

**顺序很重要**：先插 nonce（可能冲突）→ 再建任务 → 再扣预算，全部在一个事务里。这样：

- 建任务失败 → nonce 也回滚 → 节点可以重试（同一个 `jti` 还能再来）
- nonce 冲突 → 直接返回已存在的任务（幂等响应）

**注意：`task_count` 必须是中枢 DB 的计数，不能只靠 token。** 因为 token 只约束「派生」，不约束「规则派生的扇出」—— 那条路径没有 token（§8.5b-6）。

#### 8.3.5 节点侧记录 trace/ack —— 正确性辅助，不是安全边界

> owner 说：「node 应该也记录这个 trace，有过记录、或者 ack 过的就不再接收了。」

**这个应该做，但要说清它的定位：**

| 定位 | 说明 |
| :-- | :-- |
| ✅ **正确性辅助** | 节点重启后能恢复「我接过哪些任务」，避免重复执行（对本地有副作用的命令很重要） |
| ✅ **防御纵深** | 万一中枢的 nonce 表损坏/回滚，节点侧还能挡一层 |
| ❌ ****不是**安全边界** | 被入侵的节点**不必遵守自己的本地规则** —— 它可以直接跳过检查 |

**所以安全边界永远在中枢**（nonce + 签名 + 预算）。节点侧的记录是「让好节点更可靠」，不是「让坏节点变好」。

节点本地记录形态（`~/.anotifyd/state/trace.jsonl`，只追加）：

```jsonc
{ "trace": "cor_7f2", "span": "spn_a1", "jti": "spn_a1:0", "state": "acked",  "at": "..." }
{ "trace": "cor_7f2", "span": "spn_a1", "jti": "spn_a1:0", "state": "result", "at": "..." }
```text

#### 8.3.6 六道闸的计量源（修订）

| 闸 | 计量来源 | 节点能否作弊 |
| :-- | :-- | :-- |
| G1 显式 opt-in | 规则字段 / manifest `allowBranch` | ❌ |
| G2 深度 | token 的签名字段 `hop ≤ maxHop` | ❌ **签名** |
| G3 环检测 | 中枢 DB 的 `path`（rule_id 去重） | ❌ |
| **G3b 扇出** | **token 的签名 `branchBudget`** | ❌ **签名** |
| G4 任务数 | 中枢 DB `task_count`（含规则派生） | ❌ |
| ~~G5 付费数~~ | **已删除**（§12.2.5）—— 成本由 G4 + 每规则 `daily_budget` 承担 | — |
| G6 链龄 | 中枢时钟 + token `exp` | ❌ |

**一句话修订**：签名确实能携带**不可伪造、不可回滚的额度**（`hop`），配合一次性消费就构成强约束。本期 1:1 下连「额度分配」问题都不存在（§8.3.2）。

### 8.4 链不是事务

- A 的副作用是真实的，B 失败不回滚 A
- 默认只重试失败的那一跳（`on_failure: retry_step`），不重跑整条链
- 可选值：`retry_step`（默认）· `restart_chain`（要求 A 幂等）· `abort`
- **失败必须通知人**，无论链多深
- trace 状态机：`active → settled | failed | paused | expired`（TTL 24h）

### 8.5 这仍然不是工作流引擎

允许环 + 支持扇出之后，这条边界必须重新划一次，否则上面两节写的东西与它自相矛盾：

| | 本设计 | 工作流引擎（Temporal / Argo） |
| :-- | :-- | :-- |
| 编排表达 | 若干条**互相独立**的事件规则 | 一张显式 DAG |
| 依赖发现 | 中枢**不知道**下游是谁；图是**事后涌现**的 | 中枢**预先知道**全图 |
| 分支（扇出） | ✅ **本期就有**：命中 N 条规则就派 N 个任务，N 不管（§8.5b-1） | ✅ |
| **汇聚（join）** | ❌ **本期不做收集**（收集放在节点内部，§8.5b-4） | ✅ 原生 |
| 循环（回边） | ✅ **本期允许**：`path` 里 rule_id 重复即**静默剪枝**，不报错（§8.5b-5） | ✅ 原生 |
| 条件分支 | ❌ 只能靠事件属性表达 | ✅ |
| 失败恢复 | 单跳重试 | 确定性重放、补偿事务 |
| 状态 | 全部是**离散任务行**，无状态机解释器 | 确定性状态机 |

**最关键的一行是「依赖发现」**：本设计的图是**从事件流里涌现出来的**，中枢从来没有一张「这个 PR 应该经过 A→C/D→E」的图。它只知道「现在有这些规则」，然后让事件自己去撞。

这一条同时解释了为什么：

- 不需要 **配置期 join**（没人预先知道图长什么样），但需要**父终态 + 子全落定**这个判断（见 §8.6.1）
- 不需要 DAG 校验（图是涌现的，没有「合法性」可言）
- trace **特别重要**（因为你无法从配置推断出实际会发生什么，只能事后看）

需要**真 DAG**（配置期声明依赖、条件分支、补偿事务、确定性重放）的人应该去用 Temporal。这条边界写死。

---

### 8.5b 本期范围：无条件扇出，不做收集

> owner 两次修正了这里，我第二次错得更隐蔽：
> 1. 第一次我说「本期严格 1:1」—— 错，1:1 会禁止最基础的用法
> 2. 我接着把「扇出」和「收集」当成同一个问题 —— **这是真正的错误**

**一个 PR 上来，既要派 agent 去审查，也要给我手机推一条通知。** 这两件事毫无关系，必须都发生。

**扇出和收集是两个正交的问题：**

| | 扇出 | 收集 |
| :-- | :-- | :-- |
| 回答 | 「这个事件要引起几件事」 | 「那几件事都回来了吗，回来后干什么」 |
| 本期 | ✅ **无条件支持** | ❌ **不做** |
| 难度 | 极低（一次 `INSERT` 多行） | 高（批次、计数、菱形汇聚、部分失败） |

**所以本期：一个事件命中 N 条规则 → 派 N 个任务，一个不少。N 是多少我们完全不管。**

#### 8.5b-1 本期的形状：任意扇出、任意成环，但没有收集

```text
                    ┌──▶ 任务 t1a（agent 审查）
e1 ──▶ 命中 3 条规则 ├──▶ 任务 t1b（推送手机）
                    └──▶ 任务 t1c（写日志）
                              │
                              ▼
                    各自派生出 e2a / e2b / e2c（互不相干，各走各的）
                              │
                              ▼
                    它们可以继续扇出、也可以绕回来（环）
                    ❌ 但没有任何机制把它们「收」到一起
```

**本期没有「汇聚」这个概念**：每条派生线独立延伸，形成一个**森林**而不是一棵可汇聚的树。

#### 8.5b-2 为什么扇出这么便宜

因为扇出**不需要中枢知道任何额外信息**：

```sql
-- 一个事件命中 N 条规则 = 一个事务里 INSERT N 行
BEGIN;
  INSERT INTO events (...) VALUES (...);           -- 1 行
  INSERT INTO tasks (...) VALUES (...), (...), (...);  -- N 行
  -- 每行的 parent_task_id 都是触发它的任务（或 NULL）
COMMIT;
```

| 扇出需要的 | 有吗 |
| :-- | :-- |
| 知道命中哪几条规则 | ✅ 规则求值本来就要做 |
| 每行任务独立 | ✅ 天然 |
| 并发处理 | ✅ 各自独立租约、独立节点 |
| 额外的表 | ❌ **不需要** |
| 额外的 API | ❌ **不需要** |
| 计数 / 批次 / seal | ❌ **不需要** —— 因为不收集 |

**这就是为什么扇出可以在本期有，而收集不能**：扇出只是「多写几行」，收集才需要「记账 + 判断 + 定时」。

#### 8.5b-3 本期明确不做的：收集

| 不做 | 意味着 |
| :-- | :-- |
| **汇聚（join）** | 没有任何规则能声明「等 t1a、t1b 都完成后再跑」 |
| **批次计数** | 不建 `fanouts` 表、不维护 `pending` / `resolved` |
| **`batching.windowMs`** | 多个事件不合批 |
| **`join` 规则字段** | **写了直接报错**（验收 A23）—— 不静默忽略 |
| **菱形图** | trace 是森林，不是有汇聚点的 DAG |

**用户会怎么踩到**：想配「两个探索都完成后汇总」，本期做不到。答案：**让上游节点自己等**（§8.5b-4）。

#### 8.5b-4 没有收集，怎么实现「等一组都完成」

**把等待放到节点里，而不是中枢里。** 这正是「中枢不持有策略」的延续：

```text
A 是一个任务，它在自己进程内：
  1. 并行启动 3 个子任务（通过自己的逻辑，不经过中枢扇出）
  2. 自己等它们都返回
  3. 把结果汇总
  4. 回报中枢一个 result
```

对中枢来说，这**仍然是一个任务**。中枢完全不知道 A 内部开了 3 个并行分支。

| 谁 | 负责 |
| :-- | :-- |
| 中枢 | 派 1 个任务给 A、收 1 个 result |
| **节点 A** | 内部并行、内部等待、内部汇总、内部重试 |
| 用户 | 在 A 的 manifest 里用 shell/CLI 表达「并行」，中枢不关心 |

**这个方案的好处**：中枢保持极简（无批次、无计数、无状态机扩展），而「等一组」这个需求用一个通用能力（节点可以跑任意命令，包括并行编排）满足。

**代价**：A 内部的并行不可在中枢侧观测（用户在 trace 上只看到 A 一个 span）。这是本期明确接受的取舍。

#### 8.5b-5 本期仍允许成环

扇出与环是正交维度，两者本期都在：

```text
e1 ──▶ r1(rev-pr) ──▶ t1 ──▶ e2
                                │
                    ┌───────────┘
                    ▼
              r2(impl-fix) ──▶ t2 ──▶ e3
                                       │
                              ┌────────┘
                              ▼
                        r1(rev-pr) ← 回边！
                        path 已含 /rev-pr/ → 静默剪枝
```

**保证终止的不是 `max_hops`，而是 `path` 去重**：

```text
同一个 rule_id 在一条 path 里最多出现一次
→ 路径最长 = 规则总数 R
→ 必然终止
```

⚠️ **扇出后 `path` 的含义要写清**：`path` 是**从根到本任务这条具体路径上的 rule_id 序列**，不是「整个 trace 见过的 rule_id 集合」。否则一条分支剪枝会影响另一条分支。

```text
e1 → r1 → t1 ─┬─ r2 → t2   path = /r1/r2/     ✅ 可以再到 r3
              └─ r3 → t3   path = /r1/r3/     ✅ 不受 t2 影响
```

**剪枝必须可见**：静默不报错，但 trace 上必须显示「这里有一条被剪掉的回边 + 原因」。

#### 8.5b-6 防止扇出爆炸（唯一的成本闸）

既然扇出无条件允许，就必须有一道**总量**闸，否则一个事件命中 500 条规则就写满 DB。

| 闸 | 值 | 作用 |
| :-- | :-- | :-- |
| **G4 `max_tasks`** | 默认 `100`，硬顶 `1000` | **一个 trace 内的任务总数上限**（本期 G4 重新变得必要） |
| **规则数上限** | 每个事件命中规则数 ≤ `50` | 防止一次扇出过大 |

**为什么 G4 本期反而更重要**：上一版我以为 1:1 能让 G4 冗余（任务数 ≤ 跳数+1），**那是错的** —— 有扇出后，`10` 跳 × `3` 扇出 = 59049 个潜在任务，必须靠 `max_tasks` 当场剪枝。

**多余分支怎么处理**：超出 `max_tasks` 时**不创建任务**，在 trace 上记 `pruned: task_budget_exhausted`。**绝不静默丢弃**（同 §8.5b-5 的可观测原则）。

#### 8.5b-7 后期加收集时，需要动哪里

| 要改的 | 改动量 |
| :-- | :-- |
| 加 `fanouts` 表 | 新表，不动 `tasks` |
| 加 `join` 规则字段 | 规则 schema 扩展 |
| 恢复批次数 / seal 逻辑 | 新逻辑，不动现有派发路径 |
| trace 图渲染 | 森林 → 树的布局 |

**关键：本期虽然不做收集，但表结构完全兼容。** `parent_task_id` / `inputs` / `hop` / `depth` / `path` 的语义在扇出下都已正确（尤其是 `path` 是「本路径」而非「全 trace」），所以后期只是**加新表和新规则字段**，不迁移已有数据。

### 8.6 执行树：本期是「森林」，没有汇聚点

> 扇出本期就有，**收集**没有。所以 trace 不是一棵可汇聚的树，而是一片各自延伸的森林。

```text
e1 → 命中 3 条规则
   ├──▶ t1a ──▶ e2a ──▶ t2a ──▶ …   （独立延伸）
   ├──▶ t1b ──▶ e2b ──▶ …            （独立延伸）
   └──▶ t1c（结束）
```

| 字段 | 本期语义 |
| :-- | :-- |
| `parent_task_id` | 触发它的那个任务（扇出的 N 个任务共享同一个父） |
| `inputs` | **恒为 1 个元素**（触发它的父任务）；汇聚才需要 N 个 |
| `hop` / `depth` | 本期相等；分叉后 `depth` 才需要单独计展开数 |
| `path` | **从根到本任务这条具体路径**的 rule_id 序列（不是全 trace 集合，见 §8.5b-5） |

#### 8.6.1 汇聚 —— 本期不做

扇出有了，但**没人去数「派出去的都回来了吗」**，所以：

- ❌ **不需要父任务终态推导查询**（不是判断不了，是没人要这个判断）
- ❌ **不需要 `fanouts` 表**
- ❌ **不需要 `batching.windowMs`**
- ❌ **不需要 `join` 规则字段**（写了直接报错，见验收 A23）

**这些随「收集」一起延期，与扇出无关。** 后期方案记在这里备查：

| 后期方案 | 触发条件 | 已预留的缝 |
| :-- | :-- | :-- |
| 父任务终态推导 | `parent_task_id` 计数 + 父已终态 | 字段已在（§8.5b-3） |
| `fanout` 显式声明 | agent 边算边派 | `POST /branches` 单一接口，加帧即可 |
| `batching` 降噪 | 多个独立事件合并 | 规则字段位保留 |

#### 8.6.2 收敛通知在单链上的含义

`trace.settled` 的触发条件（**本期简化版**）：

```text
trace 内没有任何 pending|assigned|acked|running 任务
  → settled（不再需要判断「批处理窗口是否关闭」）
```text

所以一条 3 跳的链只发 **1 条**通知：「PR#123：审查 → 修复 → 汇总，全部完成，用时 18 分钟」。

#### 8.6.3 执行树的结构字段（本期）

```sql
tasks.parent_task_id   -- 触发它的父任务（扇出的 N 个任务共享同一父）
tasks.path             -- 祖先 rule_id 路径 "/rev-pr/impl-fix/"（环检测靠它）
tasks.hop              -- 事件派生次数 = 本期也等于 depth
tasks.depth            -- 参与执行的任务数（分叉后才与 hop 分离）
tasks.inputs           -- OTel Span Links：本期恒为 [父 task]（1 个元素）；汇聚才需 N 个
```text

**为什么两个字段都留**：本期恒相等，但收集功能上线后 `hop`（层级）与 `depth`（展开数）会不同。**现在分开存，后期不用改表。**

#### 8.6.4 下游跑不跑 —— 不是中枢决定的（owner 修正）

上一版我设计过 `traces.prune = all_success`：同层有失败就自动剪掉下游。**这个字段已删除**，因为它让中枢替用户做业务判断。

**1:1 下这条规则更清晰**：

```text
任务 t1 的 status = failed
  → 节点是否回报？节点决定
  → 节点回报了 → 派生事件产生 → 规则决定接不接
  → 中枢只做「分发事件 / 记事实 / 执行硬限制」
```text

| 谁 | 决定什么 |
| :-- | :-- |
| **节点** | 失败时是否回调（有些失败值得让下游知道，有些不必） |
| **规则** | 接不接这个事件（可以对 `payload.recoverable` 做条件） |
| **中枢** | 只记账 + 执行 G1–G4 硬闸，**绝不判断业务上该不该继续** |

**同一个原则贯穿本期**：中枢是**事实容器**，不是策略持有者。

### 8.7 Trace：对齐 OpenTelemetry 的模型

> ⚠️ 本节的「Planner 图与 Trace 图分开」已被 **§12.2 版本化** 修正为「数据分开、渲染可叠加」。以 §12.3.1 为准。
>
> owner 要求：参考 OpenTelemetry 的 trace 思路，把一次流转串起来。

#### 8.7.1 概念映射（直接借 OTel 的词汇，不自己发明）

| OTel | 本设计 | 说明 |
| :-- | :-- | :-- |
| **Trace** | `trace_id` | 一整条因果链，= 链根事件 id |
| **Span** | `task` | 一次执行单元；有 `span_id`、`parent_span_id`、`start`、`end`、`status` |
| **span_id** | `task.id`（`tsk_*`） | 全局唯一即可作为 span id |
| **parent_span_id** | `tasks.parent_task_id` | 形成树 |
| **Span Kind** | `root` / `internal` / `producer` / `consumer` | 见下 |
| **Span Attributes** | `rule` / `node` / `cost_class` / `status` | 筛选用 |
| **Span Links** | `tasks.inputs[]` | **一个 span 指向多个上游（非父子关系）** —— 菱形合批就靠它 |
| **Events（span 内）** | 剪枝、重试、租约过期 | 时间点标注 |
| **Trace Sampled** | 全部保留（本产品量级不需要采样） | |
| **Resource / 元数据** | `trace_id` / `span_id` / `parent_span_id` / `hop` 全部进 `events` 普通字段（§12.2.4），不是独立查询参数 | 与 `source`/`type` 平级 |

**为什么 `Span Links` 这个字段是必需的（不是可选）**：OTel 里 links 就是为「多个上游 → 一个下游」设计的。菱形 A→{C,D}→E 里：

- E 的 `parent_span_id` 只能指向一个（比如 C）
- **D 通过 `links` 指向 E** ← 这才是准确表达

用父子关系硬塞会丢失信息，用 links 才和 OTel 语义一致。

#### 8.7.2 三个 Span 形态

```text
root        evt_1 (pr.opened)  ← trace 起点，根 span 是事件本身
producer    rule:rev-pr → tsk_A   ← 由事件派生任务，这是「生产」跨度
internal    tsk_A 执行（node, 起止时间, artifacts）
consumer    tsk_A → evt_2 (task.succeeded)  ← 任务终态派生出事件
```text

**这样串起来之后，一条 trace 就是一路「事件 → 任务 → 事件 → 任务」交替的链**，而不是只有任务。

这是与上一版最大的不同：上一版的 trace 只有任务节点，**没有事件节点**，于是「这条事件到底被哪条规则消费了」看不见。加上 producer/consumer span 才对。

#### 8.7.3 边与剪枝

每条边 = 一次「规则命中 → 创建任务」，或一次「任务终态 → 派发事件」，或一次剪枝：

```jsonc
{ "parentSpan": "tsk_A", "span": "tsk_C", "kind": "internal", "rule": "fix-lint" }
{ "parentSpan": "tsk_D", "span": "tsk_E", "kind": "link",
  "inputs": ["tsk_C", "tsk_D"] }                          // ← 菱形合批：links 不是 parent
{ "parentSpan": "tsk_E", "span": null, "rule": "rev-pr",
  "event": "pruned", "reason": "cycle: path 中已有 rev-pr" }  // ← 中枢唯一的介入点
```text

**`pruned` 标注是 trace 里最有价值的诊断信息** —— 它回答「我配了这条规则，为什么它没跑」。

#### 8.7.4 实时与回放

| 能力 | 实现 |
| :-- | :-- |
| 实时追踪 | 复用控制台 SSE（`/v1/events/stream`），按 `traceId` 过滤 |
| 事后回放 | `GET /v1/traces/{traceId}/graph` 返回 OTel 形状的 JSON；前端渲染 |
| 时间轴 scrub | 前端按 span 起止时间做滑块，后端无需额外支持 |
| 与日志关联 | span 详情里的 `logHint.localPath` → `logs-request` 现拉 |
| **导出** | **可选**：`GET /v1/traces/{traceId}/graph?format=otlp-json` 供接 Jaeger/Tempo（本期不做，结构已兼容） |

**不落 trace 快照表**：从 `events` + `tasks` 现算。加一张快照表 = 同一份事实存两遍，必然不一致。

## 9. 节点安全模型：命名命令注册表

### 9.1 死结

> 一旦中枢能下发命令行，中枢被入侵 = **所有节点 RCE**。

而中枢只是一个 Go 二进制 + 一个 SQLite 文件，它是所有资产里**最不该**承担这个爆炸半径的地方。

### 9.2 解法：命令由节点定义，中枢只发「名字 + 参数」

节点本地配置 `~/.anotifyd/config.yaml`（**永不上传，中枢不可读**）：

```yaml
commands:
  review:                                  # ← 中枢只知道这个名字
    argv: [pi, -p, "{prompt}"]             # argv 数组，绝不经过 shell
    cwd: ~/code
    timeout: 1800
    maxParams: { prompt: 16384 }
    allow: true

  deploy:
    argv: [./scripts/deploy.sh, "{target}"]
    cwd: ~/code/infra
    allow: false                           # 默认关，需人工置 true
```text

中枢下发的**全部内容**：

```jsonc
{ "kind": "cmd", "name": "review", "params": { "prompt": "审查 PR ..." } }
```text

### 9.3 三条不可协商的约束

| # | 约束 | 效果 |
| :-- | :-- | :-- |
| 1 | **中枢永不携带 argv / shell 字符串**（DB 里没有这个字段，物理上表达不了） | 中枢被攻陷 ≠ RCE |
| 2 | **节点用 `execve(argv[])` 启动，禁用 `/bin/sh -c`**；参数只做占位符替换进 argv 槽位 | **注入攻击按构造被消灭**（不经 shell 就无注入面） |
| 3 | **未注册的 `name` → `unknown_action` 失败**，控制台可见 | 攻击者只能调运维已定义的那几条命令 |

### 9.4 标签：中枢赋值，不接受自报

**问题场景**（回答「拒绝节点自报」到底防什么）：

| 场景 | 若相信自报 | 实际 |
| :-- | :-- | :-- |
| 你在笔记本上跑 `anotifyd`，运营配置了 `env: dev` | — | — |
| 有人偷了这台机的 node token，或伪造一个 `anotifyd` 连上来，声称 `labels: {env: prod, can_deploy: true}` | **它立刻拿到生产部署任务的派发权** | 只有中枢 `nodes.labels` 里的 `env: dev` 生效，它拿不到 prod 任务 |

所以字段分成两套，**路由只用中枢那套**：

| 字段 | 来源 | 用途 |
| :-- | :-- | :-- |
| `nodes.labels` | **中枢**（控制台/配置文件手工赋） | **唯一参与 `node_selector` 匹配** |
| `nodes.reported` | 节点自报（`os` / `arch` / 版本） | 仅 UI 展示 |
| `nodes.capabilities` | 节点自报（命令名列表） | 可参与 selector，但自报错只会失败，不会提权 |

`capabilities` 自报的安全性：节点声称有 `deploy` 命令但其实没有 → 任务失败 `unknown_action`，仅此而已。它**不能**借此提权（能跑什么由本地 config 的 `allow` 决定）。中枢会在首次失败后把「声明 vs 实际」不一致展示出来。

### 9.5 标签绑定的身份：`node_id`（不是连接、不是主机）

> owner 补充：**标签实际是打在 node 对应的那个 key 上**。已采纳，后果如下。

- 同一个 node token 从**任何机器**连上来，都继承该 `node_id` 的标签。换机、重装、重连**都不改变**标签 —— 这正是我们想要的（标签描述的是「这台机器的职责」，不是「这次连接」）
- 因此：**token 泄漏 = 继承该节点的全部标签**。在没有硬件证明（attestation）的前提下这是**必然代价**，不是缺陷。只能靠三件事控制：
  1. token 仅存 argon2id 哈希（DB 泄漏也无法直接登录）
  2. 一键轮换（旧 token 立即失效）
  3. 吊销（`nodes.revoked_at`），并**取消该节点全部 inflight 任务**
- 想让一台机器「换个身份」→ 走**重新 enroll** → 生成新 `node_id` → 标签重新赋
- 改标签**不需要**节点重连：中枢侧 PATCH 即生效（下一次 `node_selector` 匹配就能看到）
- 推论：**不要把 node token 与环境标签一起放进镜像/CI secret**。那把「一个 token 泄漏」放大成「所有 env:prod 节点失守」

---

## 10. Sink 抽象

### 10.1 两类 sink，永不混用

| 类别 | 语义 | 投递保证 | 回报 | 实现 |
| :-- | :-- | :-- | :-- | :-- |
| **human** | 告知人，fire-and-forget | 尽力（可丢） | ❌ | webpush · sse · (未来 email) |
| **machine** | 委托执行 | 至少一次 + 租约 + 重试 + DLQ | ✅ 必须 | node · http-hook |

**铁律**：静默丢弃只允许用于 human sink。machine sink 一律走 DB 行租约。

**推论**：iOS / Safari 永远只能是 human sink（浏览器无后台执行），因此**永远不与 node 竞争**。

### 10.2 通知收敛（这是本设计最优雅的一处）

| 场景 | 通知行为 |
| :-- | :-- |
| 单跳事件（cron / webhook / watch） | 匹配即通知（按白名单 + §10.3 节流） |
| 多跳链 | **只在 trace 收敛时**通知一次 |
| 链中任一跳失败 | **立即**通知失败，不等收敛；标注「后续已暂停」 |
| trace `paused`（撞闸） | **必须**通知（需人决断） |

于是 5 跳的链 = **1 条通知**（「rev-pr → fix → test 全部完成，用时 23 分钟」），而不是 5 条。

**但深链（G2 放宽到 10 跳后会出现）需要一条补充规则**：

| 场景 | 行为 |
| :-- | :-- |
| trace 活跃超过 30 分钟且仍有任务在跑 | 发一次「仍在进行中」通知（**每链只一次**，不是周期性） |
| 上述通知后仍未收敛，超过 `max_duration` | 标 `expired` 并发一条「链已超时」通知 |

否则一个跑了 3 小时的 10 跳链，用户会以为它静默死了 —— 而这正是 §10.2 要避免的状态。

**实现**：任务终态时，若该 trace 内已无 `pending|leased|running` 任务 → 置 `settled` → 投递一条聚合通知。单实例 SQLite 上这一步很便宜（一条带索引的 count）。

### 10.3 `default_notify` 白名单（**已修正**）

> ⚠️ 上一轮我列的清单有个内在矛盾：既让 `agent.task.succeeded` 默认通知，又说「链收敛才通知」——那 5 跳链会先发 5 条。**已修正为三个事件类型：**

| 事件类型 | 何时发 | 为什么 |
| :-- | :-- | :-- |
| `agent.task.failed` | **立即**，无论链多深 | 失败需要人决断；绝不能因中间跳失败而让整条链静默死掉 |
| `trace.settled` | trace 收敛时（**单任务链也算**，此时文案是「任务完成」） | 唯一「一切顺利」的通知入口；天然去重，5 跳只有 1 条 |
| `trace.paused` | 撞 G1–G6 任一闸 | 需要人来改规则/调预算，不通知就等于卡死 |

- `agent.task.succeeded` **仍是真实事件**（供规则 chaining 用），但 **`default_notify = false`** —— 它由 `trace.settled` 覆盖
- `cron` tick / `watch` 无变化 / 心跳类事件：`default_notify = false`
- **`source: api` 的事件（即旧的 `/v1/notify` 用法）：`default_notify = false`** —— 靠用户建的 `kind: notify` 规则推送（§5.4），而非内建白名单。否则会与规则重复推送两次

**措辞纪律**：不要用 `severity` 表达这件事。该字段已于 `7fd6ffe` 以「零消费方」为由全链路删除，重新引入会被 review 门禁打回。用事件类型注册表的布尔标志 + 用户 `mutes` 规则。

### 10.4 节流（**必须与事件中心同时上线**）

事件中心天然比通知中心吵：

- 通知中心的事件密度 ≈ agent 任务数（一天几十条）
- 事件中心的密度 ≈ **所有 cron tick + 所有 watcher 轮询 + 所有 webhook**（一天可能几万条）

若 human sink 做成「匹配就推」，上线第一周就会被淹没，然后你会关掉通知——而那正是要保住的能力。

| 机制 | 规则 |
| :-- | :-- |
| **mute** | 用同一套谓词语法关掉某类（`source=cron` / `attributes.repo=X`） |
| **digest** | 同一 `subject` 在 N 分钟内合并成「5 条更新」 |
| **速率上限** | 每设备每 5 分钟最多 1 条，超出进摘要队列 |
| **白名单** | 见 §10.3 |

### 10.5 深链形态（U4 展开）

> 上一版我写了「`/e/{id}` 302」，但没解释为什么。这里说清。

**「深链」= 用户点手机通知栏之后打开的那个 URL。** 现状是 `message.html?id=<id>`（`internal/push/dispatcher.go:190-206`）。

换成什么，取决于一个容易被忽略的事实：

> **iOS 上 `clients.openWindow` 往往打开 Safari 标签页，而不是已安装的 PWA。**
> Safari 与 PWA 是两个**独立的存储域** —— 那个标签页里没有 `anotify_session` cookie，所以会被踢到登录页。

三种写法，选 B：

| 写法 | URL | 问题 |
| :-- | :-- | :-- |
| A · 直接页面 | `https://hub/event.html?id=evt_x` | 被踢到登录页后**丢失「我本来想看哪条」**；页面改名即失效 |
| **B · 短路径 + 302** | `https://hub/e/evt_x` | ✅ **推荐** |
| C · 带 token 的直链 | `https://hub/e/evt_x?t=xxx` | ❌ 一次性 token 会留在通知栏与通知中心历史里 |

B 的服务端逻辑：

```text
GET /e/{id}
  ├─ 有有效 session → 302 到 /event.html?id={id}   （id 前缀 evt_/tsk_/cor_ 分派）
  └─ 无 session      → 302 到 /login.html?next=%2Fe%2F{id}
                                        ↑ 登录后回到 /e/{id}，再走一次 302
```text

好处：

1. **通知栏里永远是短链**（截图、转发都干净）
2. 登录后**必定**回到那条事件（复用已有的 `?next=` 白名单机制，`web/partials.js:722-763`）
3. 页面改名/重构**不影响**已发出的历史通知
4. 同一个短链**同时服务**事件详情 / 任务详情 / 链视图（按前缀分派）

⚠️ 现有 e2e 套件 `scripts/e2e/suites/deeplink.mjs` 断言的是 `message.html?id=`，必须同步改。

---

## 11. API 面

```text
── 事件 ─────────────────────────────────────────────
POST   /v1/events                        通用上报（Bearer, scope events:write）
GET    /v1/events                        列表/筛选（source/type/subject/时间/属性）
GET    /v1/events/{id}
GET    /v1/events/stream                 SSE（控制台实时流，支持 Last-Event-Id）
── 入站 webhook ─────────────────────────────────────
POST   /v1/in/{sourceId}                 通用 webhook（secret / 可选 HMAC-SHA256）
POST   /v1/in/github/{sourceId}          GitHub 适配器（校验 X-Hub-Signature-256）
POST   /v1/in/gitlab/{sourceId}          GitLab 适配器（校验 X-Gitlab-Token）

── 触发源 ───────────────────────────────────────────
GET|POST  /v1/sources
PATCH|DELETE /v1/sources/{id}
POST   /v1/sources/{id}/trigger          立即触发一次（可 dryRun）
GET    /v1/sources/{id}/preview          下一次触发时间（cron）

── 规则 ─────────────────────────────────────────────
GET|POST  /v1/rules
PATCH|DELETE /v1/rules/{id}
POST   /v1/rules/{id}/explain?event=     逐子句解释（§5.2）
POST   /v1/rules/{id}/dry-run?event=     返回「会派给哪个节点」

── 任务 ─────────────────────────────────────────────
GET    /v1/tasks                         列表（status/node/trace 筛选）
GET    /v1/tasks/{id}
POST   /v1/tasks/{id}/cancel
POST   /v1/tasks/{id}/retry
POST   /v1/tasks/{id}/logs-request       请求节点上传尾部日志

── 节点 ─────────────────────────────────────────────
GET    /v1/nodes
PATCH  /v1/nodes/{id}                    改名 / 改标签 / 改并发
POST   /v1/nodes/{id}/revoke
POST   /v1/nodes/enroll                  生成一次性 enroll token
GET    /v1/nodes/{id}/instances          该节点的所有实例（并发/负载/状态）
DELETE /v1/nodes/{id}/instances/{ninId}  踢掉单个实例（不影响同池其他实例）
GET    /v1/nodes/stream                  WS（节点用，§7）

── 节点自主派生（§8.3.4）───────────────────────────
POST   /v1/chains/{traceId}/branches   节点派生新子树（幂等：token.jti）
                                        409 budget_exhausted / paid_budget_exhausted

── Trace 图（§8.6 / §8.7）─────────────────────────
GET    /v1/traces/{traceId}              trace 概览：状态、深度、任务数、预算消耗
GET    /v1/traces/{traceId}/graph?view=tree      树视图（默认）
GET    /v1/traces/{traceId}/graph?view=timeline  时间轴视图（可 scrub）
GET    /v1/traces/{traceId}/graph?format=otlp-json   OTLP 兼容导出（本期不做）
POST   /v1/tasks/{id}/cancel            取消一个任务（节点 kill 进程树）\nPOST   /v1/tasks/{id}/cancel?subtree=true    取消整棵子树

── 索引 / 类型 / 拓扑（§12）────────────────────────
GET    /v1/registry                      类型注册表（builtin + observed）
GET    /v1/rulesets                      规则集版本列表（append-only）
GET    /v1/rulesets/{version}            某一版完整快照（审计）
GET    /v1/rulesets/{v1}/diff/{v2}       两版差异
GET    /v1/nodes/{id}/manifests         该节点 manifest 版本历史
GET    /v1/registry?source=github        只取某 source 的类型
GET    /v1/graph?view=rules              类型级二部图（当前版本）
GET    /v1/graph?view=overlay&cor=cor_x  叠加图：当时 planner + 实际 trace（默认）
GET    /v1/graph?view=nodes              实例级图（节点 / 在飞任务）
POST   /v1/rules/validate                **保存前预检**：capability 是否存在 / params 是否齐全 / 标签能否匹配

── 沿用不动 ─────────────────────────────────────────
/v1/devices*  /v1/keys*  /v1/auth/*  /v1/cli-auth/*  /v1/passkey-enroll/*  /v1/admin/*
/v1/vapid-public-key  /health

── 已删除 ──────────────────────────────────────────
（无）POST /v1/notify                     彻底删除，无兼容适配器（§5.4 替代写法）
```text

---

## 12. 类型注册表 · Capability Manifest · 拓扑图

> 起因（owner 提出）：节点是不是应该提前把「能上报什么消息类型」暴露出来，这样定义 source / 添加 sink 时就能直接推算出图，用户也知道自己该干嘛。

**结论：方向对，但要先把这个想法里混在一起的两个类型系统拆开。** 否则 registry 会变成一锅粥。

### 12.1 三种「类型」，只有一种该由节点上报

| # | 是什么 | 例子 | 由谁定义 | 谁有权威 |
| :-- | :-- | :-- | :-- | :-- |
| ① **事件类型** | 事实的种类 | `pr.opened` · `task.succeeded` · `watch.changed` | **source** | 中枢 registry |
| ② **动作类型（capability）** | 节点能干什么 | `review` · `deploy` · `build` | **节点** | 节点本地 config |
| ③ **数据类型（schema）** | 一个 payload / params 的形状 | `pull_request` 的字段、`prompt` 的类型 | 约定 | 中枢 registry（只描述不校验） |

**关键判断：**

- **① 不该由节点上报。** 节点不产生事件类型 —— source 才产生。`anotifyd` 上报的是「我本机能跑 `review`」，那是 ②，不是 ①。
- **③ 不要做成强制 schema 校验框架。** 这是明确的非目标（`§1`）。但「用户在填模板时能看到字段清单」这个需求是真实的 → 做成**建议性 schema**（advisory，写入时**不拒绝**，只在模板编辑器里做补全与提示）。
- **② 才是你真正要的那个。** 而且它已经隐含在 `nodes.capabilities` 里了（`§3`），只是需要说清语义。

**做法：把 `capabilities` 从「命令名数组」升级为 capability manifest**（`$schema` 版本化）：

```jsonc
{
  "manifestVersion": 1,
  "nodeId": "nod_x",
  "reported": { "os": "darwin", "arch": "arm64", "daemonVersion": "0.1.0" },
  "concurrency": 2,
  "commands": [
    {
      "name": "review",
      "kind": "cmd",
      "costClass": "paid",            // ← 仅用于 UI 展示「这条链有 N 个付费任务」
      "params": {                      // 建议性 schema，不做强制校验
        "prompt": { "type": "string", "maxBytes": 16384, "required": true }
      },
      "produces": ["task.succeeded", "task.failed"],
      "timeoutSec": 1800,
      "retries": { "max": 2, "backoffMs": 5000 },   // ★ 节点自定义内部重试（§12.2.6）
      "idempotent": false,      // ★ 节点下线时是否可安全重派（§12.4.3，默认 false）
      "resumable": false,       // ★ 是否支持断点续传（§12.4.3）
      "description": "用本地 pi 审查一个 PR"
    },
    {
      "name": "notify",
      "kind": "notify",
      "costClass": "free",
      "params": { "title": { "type": "string" }, "body": { "type": "string" } },
      "produces": []
    }
  ]
}
```text

**安全约束（不可协商，沿用 `§9` 的同一套逻辑）：**

| 字段 | 传输方向 | 中枢能拿它做什么 |
| :-- | :-- | :-- |
| `commands[].name` | 节点 → 中枢 | 参与 `node_selector.capability` 匹配 |
| `commands[].costClass` | 节点 → 中枢 | **仅 UI 展示**（不再参与闸门，§12.2.5） |
| `commands[].params` | 节点 → 中枢 | 仅展示 + 编辑器补全，**不做强制校验** |
| ~~`argv`~~ | **永不出现** | **manifest 里没有这个字段** |
| ~~`cwd` / `env`~~ | **永不出现** | 同上 |

> manifest 是 `§9.3` 第一条约定的延伸：**中枢连「命令怎么跑」都不该知道**，只该知道「能不能跑、要花多少钱、需要什么参数」。

### 12.2 版本标识与钉住：用「时间 + hash」，但钉住的是 hash

> owner 提出：**版本号就是「时间 + hash」；用时间去找这个时间点之前最新的版本。**
> 后半句有一个必须修正的地方 —— 否则历史 trace 会当场变形。

#### 12.2.1 标识：`evt_20261023T101500Z_a1b2c3d4`

```text
<prefix>_<UTC 时间，秒级>_<content_hash 前 8 位>

reg_20261023T101500Z_a1b2c3d4     规则集
nmf_20261023T101500Z_9f2c1b7e     节点 manifest
rty_20261023T101500Z_3c4d5e6f     类型注册表
```text

三个好处，都用得上：

| 特性 | 用途 |
| :-- | :-- |
| **时间可读** | 日志与 UI 里直接看出「这是 10 月 23 日 10:15 那版」，不用查表 |
| **时间可排序** | `ORDER BY id` 即时间序（UTC + 定长格式，字典序 = 时间序） |
| **hash 去重** | 内容没变则 hash 相同 → **不建新版本**；同时可直接做完整性校验 |

**hash 是内容哈希，不是随机串** —— 这带来一个免费的好处：两个不同时间的版本如果内容完全一样（改了又改回来），hash 相同，可以判定「无实质变化」。

#### 12.2.2 ⚠️ 修正：钉住 hash，不钉住时间

owner 的原话是「用时间去找这个时间前面最新的版本」。**作为「按时间查询」的接口，这是对的；但作为 trace 的钉住方式，它是错的。**

原因很具体：

```text
10:15  规则 v_A  （含 review → impl-fix 这条边）
10:20  跑了一次 trace，钉住「时间 10:20」
10:30  规则改成 v_B  ← 「10:20 之前最新的版本」现在指向 v_B？
       还是 v_A？取决于你怎么查
```text

如果 trace 只存时间，然后回看时执行「取该时间之前最新版本」的查询，那这个查询的**结果会随数据增长而改变** —— 一旦补录了历史版本、或修正了某个 `effective_at`，历史 trace 的图就变了。

**正确做法：钉住不可变的版本 id。**

```sql
-- traces 表
traces.registry_version_id   TEXT NOT NULL REFERENCES registry_versions(id)
traces.rule_version_id       TEXT NOT NULL REFERENCES rule_versions(id)

-- tasks 表
tasks.manifest_version_id    TEXT NOT NULL REFERENCES node_manifest_versions(id)
tasks.hop_ctx                -- 见 §12.2.4
```sql

回看时是 `SELECT * FROM rule_versions WHERE id = 'reg_20261023T101500Z_a1b2c3d4'` —— **单点主键查询，结果永远一样**。

「按时间找最新版本」的查询能力**保留**，但只用于**创建时**（trace 诞生那一刻做一次）：

```sql
创建 trace 时：
  1. SELECT id FROM rule_versions
     WHERE effective_at <= now() ORDER BY effective_at DESC LIMIT 1;
  2. 把查到的 id 写进 traces.rule_version_id
  3. 之后永远只用这个 id
```text

**一句话：时间用于「当时选哪一个」，hash 用于「之后它永远是哪一个」。**

#### 12.2.3 三样东西统一用这个标识

| 对象 | 前缀 | 谁产生 | 变化时机 |
| :-- | :-- | :-- | :-- |
| **规则集** | `reg_` | 中枢（任何规则变更） | 规则增删改 |
| **节点 manifest** | `nmf_` | 中枢（收到 `hello`/`manifest` 帧且内容变化） | 节点命令/标签变更 |
| **类型注册表** | `rty_` | 中枢（builtin 升级或 observed 累积） | 发版 |

> **注意：节点不上报自己的版本号**，节点只上报 manifest **内容**；中枢根据内容算 hash 并生成 id。
>
> 理由与 §8.3 一致：让节点自报版本 id，它就能声称自己是任意版本（包括不存在的）。**hash 必须由中枢对实际上报的内容计算。**

#### 12.2.4 hop 上下文也进元数据（owner 提出）

> owner 指出：**trace_id 和 span 是不是要写入元数据呢，和其他字段一样。**

是。这两类标识**不该是独立查询参数，而应该是事件的普通字段**，和 `source` / `type` / `attributes` 平级：

```sql
events (
  ...
  trace_id      TEXT NOT NULL,     -- 普通字段，有索引
  span_id       TEXT NOT NULL,
  parent_span_id TEXT,
  hop           INTEGER NOT NULL,
  rule_version_id TEXT,            -- 产生这条事件时的规则版本
)
```text

好处：

| 好处 | 说明 |
| :-- | :-- |
| 统一查询 | `GET /v1/events?trace_id=trc_x` 与其他筛选条件组合，不需要特殊端点 |
| 事件与任务同构 | 两者都带 trace/span，渲染图时逻辑一致（§8.7 的 producer/consumer span） |
| 审计天然 | 每条事件自带「我是哪一版规则产生的」，不依赖 join |

**索引**（必须建，否则按 trace 查事件是全表扫描）：

```sql
CREATE INDEX idx_events_trace ON events(trace_id, seq);
CREATE INDEX idx_events_span  ON events(span_id);
```text

#### 12.2.5 删除 `paidBudget`（owner 决定）

> owner：**paid 那个预算我觉得没必要，其他的够用了。**

同意，且理由比「够用」更硬：**`paidBudget` 是唯一一个「计量源在节点」的预算**。

- `hop` / `jti` 都是签名字段，中枢说了算
- `paidBudget` 若由节点按 `costClass` 自报扣减，就等于让节点自己决定「我这次算不算花钱」

删掉它之后，成本控制仍有两条防线，而且都不依赖节点上报：

| 防线 | 计量源 | 说明 |
| :-- | :-- | :-- |
| **G4 `max_tasks`** | 中枢 DB `task_count` | 任务数硬上限（100），与成本强相关 —— 每次 agent 调用必然是一个 task |
| **每规则 `daily_budget`** | 中枢 DB（按 rule_id + 日期计数） | 规则级日预算，持续累计 |

**「任务数」在这里是成本的良好代理**：一个付费任务就是一个 task，`max_tasks=100` 已经隐含了「这条链最多花 100 次」。再加 `daily_budget` 拦住「一天跑 200 条链」。

`costClass` 字段**保留**（用于 UI 显示「这条链有 3 个付费任务」），但**不再参与闸门计算**。

#### 12.2.6 重试次数交给节点自己定（owner 决定）

> owner：**单个节点最大重试多少次，可以直接让 node 来自己定义。**

对。而且这比中枢统一定义更正确：

**中枢不该知道「重试几次合适」**。跑一次 lint 可能重试 3 次都值得，跑一次 2 小时的训练任务重试 1 次就够 —— 只有节点知道。

所以拆成两个概念，各管一头：

| 概念 | 谁定义 | 语义 |
| :-- | :-- | :-- |
| **节点内部重试** | **节点**（manifest 里 `retries`） | 进程崩了/非零退出，节点自己重跑，**中枢完全不知道** |
| **中枢重派** | 中枢（`max_attempts`，默认 3） | 租约过期/节点离线/节点明确回报 failed → 中枢换个节点再派 |

```jsonc
// manifest 里
{ "name": "review",
  "retries": { "max": 2, "backoffMs": 5000 } }   // ← 节点自己定
```text

**关键规则：节点内部重试不消耗中枢的 `attempts` 配额。** 否则「节点重试 2 次 + 中枢重派 3 次」会变成 6 次执行，而中枢以为只有 3 次。

对应地，节点的 `result` 帧里要带 `attemptInfo`：

```jsonc
{ "t": "result", "taskId": "tsk_...", "status": "succeeded",
  "attemptInfo": { "internal": 2 } }   // 节点内部试了 3 次(0,1,2)才成功
```text

中枢把它记进 `tasks.internal_attempts`（只展示，不参与闸门）—— 用户能看到「这个任务实际试了 3 次」，这对排障很重要。

### 12.3 节点池：共享 token + 每实例并发申报 + 按负载派发

> owner 提出两点：① 一个 token 是否允许多个 node 启动，做负载均衡；② 节点申报「能并行执行几个」。

**第 ② 点直接采纳。第 ① 点需要纠正一个前提：负载均衡根本不需要共享 token —— 但它确实带来一个真实的运维便利，所以设计成可选模式。**

#### 12.3.1 先纠正：负载均衡不等于共享 token

现在 `nodes.id` 已经允许 N 个实例共存：同一台机器起 2 个 `anotifyd`，各自 enroll 得到 2 个 `nod_xxx`，两个都打 `env:prod` 标签，`node_selector` 会同时匹配到它们。

**资源已经被池化了。** 所以「做负载均衡」这个目标，不需要共用一个 token 就能达到。

**那共享 token 真正解决的是什么？** 是**运维成本**，不是调度：

| 场景 | 独立 token（现状） | 共享 token |
| :-- | :-- | :-- |
| 在 10 台机器上部署 20 个实例 | 要 enroll 20 次、管 20 个 token | enroll 1 次，同一份配置下发 |
| 某个实例挂了 | 单独 rotate 那一个 | **吊销 pod 身份，不影响其他实例** |
| 一台机器要「换个身份」 | 重新 enroll | 换一张 token |

所以正确的表述是：**共享 token 是「降低大规模部署的运维成本」的可选模式，不是「实现负载均衡」的前提。**

#### 12.3.2 设计：`node` 与 `node_instance` 两级

```sql
CREATE TABLE nodes (              -- 身份单元 = 脑（逻辑的）
  id            TEXT PRIMARY KEY,   -- nod_<ksuid>
  token_hash    TEXT NOT NULL,      -- argon2id（多实例共享同一 token）
  labels        TEXT NOT NULL,      -- ★ 中枢赋值的路由标签
  capabilities  TEXT NOT NULL,      -- 声明的命令清单
  max_concurrency INTEGER,          -- ★ 池内总并发上限（中枢赋值）
  max_instances   INTEGER,          -- ★ 单 node 最大实例数
  ...                               -- 其余字段见 §3.2 正式 DDL
);

-- node_instances 的正式 DDL 见 §3.2；此处只看字段意图
-- id / node_id / instance_key / concurrency / inflight / status
```text

**关键划分：**

| 概念 | 含义 | 谁决定 |
| :-- | :-- | :-- |
| `nodes` | **身份**：这是「哪一台机器 / 哪个职责」 | 中枢（enroll 时创建，标签手工赋） |
| `node_instances` | **执行器**：当前有几个进程连着、各自能吃多少并发 | 节点（`hello` 时自报） |

**这个划分解掉了 owner 的顾虑**：共享 token 时，10 个实例共用 1 个 `node_id`，但**仍是 10 个 `node_instance`**。所以：

- 派发仍然精确到实例（`leased_by = inst_xxx`）
- 一个实例挂了只影响它自己
- 标签仍然绑在 `node_id`（身份）上，**不会因为共享 token 而互相污染**

#### 12.3.3 并发申报：三层上限，取最小值

节点在 `hello` 里自报 `concurrency`（本实例能同时跑几个任务）：

```jsonc
{ "t": "hello", "nodeToken": "…", "instanceKey": "…",
  "concurrency": 4,                       // ★ 本实例能并行几个
  "commands": [ { "name": "review", "concurrency": 1 },   // ★ 可选：单命令再限一层
                { "name": "notify", "concurrency": 8 } ] }
```text

**三层上限，实际派发数 = min(三者)：**

| 层 | 值 | 谁定 | 作用 |
| :-- | :-- | :-- | :-- |
| 实例级 | `node_instances.concurrency` | **节点自报** | 这台机器能吃多少（CPU/内存/模型并发额度） |
| 命令级 | `commands[].concurrency`（可选） | **节点自报** | 同一命令不能并行太多（比如 `pi` 一次只能跑一个） |
| 池级 | `nodes.max_concurrency` | **中枢赋值** | 防止一个 token 扩散到 100 个实例后把后端打爆 |

**为什么池级要由中枢定**：这是 owner 提共享 token 时引入的新风险 ——

> 一个 token 泄漏，攻击者可以起 1000 个实例，全都带 `env:prod` 标签，把生产任务的派发权稀释掉。

所以 `nodes.max_concurrency` 是**中枢侧的安全阀**，不来自节点自报。默认 1（保守），需要时手工调高。

#### 12.3.4 派发算法：最少负载优先

```text
候选集 = { inst | inst.status = online
             and inst.node.labels 匹配 node_selector
             and inst.node.capabilities 含所需命令
             and inst.inflight + pending_for_nin < inst.concurrency
             and inst.node 总 inflight < nodes.max_concurrency }

选中 = 候选集按 inflight 升序、last_seen_at 降序 取第一个
```text

**为什么用 `inflight` 而不是轮询（round-robin）**：

- 轮询假设所有任务耗时相近 —— 但我们的任务是 LLM agent，一个跑 30 秒、一个跑 30 分钟
- `inflight` 是**真实负载**的直接观测，不需要任何估算
- 平手时用 `last_seen_at` 降序（优先选最近心跳的，避免选到即将掉线的实例）

**任务耗时不均场景的说明（诚实）**：最少负载优先在「长任务 + 短任务混合」时会让短任务集中在少数实例上。真正的解法是「预估耗时」或「work stealing」，但两者都超出本期范围。**先做 inflight 最少，样本积累后再谈。**

#### 12.3.5 并发申报的安全边界：自报可以「少」，不能「多」

`concurrency` 是节点自报的，这带来一个必须说清的边界：

| 攻击 | 后果 | 是否有害 |
| :-- | :-- | :-- |
| 节点**谎报更低的** `concurrency` | 少接任务 | 无害（它自己吃亏） |
| 节点**谎报更高的** `concurrency` | 接到超过自己能力的任务，自己 OOM / 超时 | **只伤害自己** |
| 节点谎报高 `concurrency` **并真的并行跑** | 消耗自己的模型额度 | 同上 |

**关键：`concurrency` 谎报不构成对别人的攻击**，因为它只能让自己多接活。真正需要防的「把生产任务稀释掉」是 `nodes.max_concurrency` 管的（中枢赋值）。

**但有一个真实风险**：恶意节点谎报 `concurrency: 1000` 并**抢走所有任务后不执行**（或执行后不回报）→ 任务占着租约直到过期，形成**拒绝服务**。

缓解：

1. `nodes.max_concurrency`（中枢侧上限）
2. 派发后 `assigned → acked` 有 20 秒超时（蓝图 §6.3），不 ack 就重派
3. 连续 N 次「抢到但不 ack」的实例自动降权（`inflight` 视为满），并在控制台告警

#### 12.3.6 池内实例的三种故障，分别怎么处理

| 故障 | 判定 | 处理 |
| :-- | :-- | :-- |
| **单实例掉线**（pod 重启） | 该 `inst` 心跳消失 > 2×hb | `inst.status=offline`；它的 `running` 任务等租约过期后重派；**同 `node_id` 的其他实例不受影响** |
| **整池掉线**（机器关机） | 所有 `inst` 都 offline | `nodes.status=offline`；任务全部等租约过期 |
| **实例数爆炸**（token 泄漏或被攻击） | 某 `node_id` 下 `inst` 数量激增 | 超过阈值（默认 20）拒绝新连接并告警；`nodes.max_concurrency` 仍然限制总量 |

#### 12.3.7 控制台展示

节点页变成两级树：

```text
desktop            env:prod, gpu    [2/4 忙]   ← nodes 层（身份 + 标签 + 池上限 4）
├─ inst_a1  darwin/arm64   inflight 1/2   ● 在线 42s
└─ inst_a2  darwin/arm64   inflight 1/2   ● 在线 12s
```text

派发时显示「选了 `inst_a1`，因为它的 inflight 是 1/2 而 `inst_a2` 是 2/2」——**排障可见**（呼应「可解释性」这个定位）。

### 12.4 粘性路由：同一 trace 的同一规则必须落在同一节点

> owner 提出的问题：**如果节点是负载均衡的，汇聚怎么处理？如果节点下线了怎么办？rebalance 怎么办？**
> owner 的方案：**第一次 hash 算出节点后就查表，后面的事件用粘性算法路由回原节点；节点下线就当死消息。**

这个方案是对的，而且比"纯 hash"更强。但它有一个必须分级的地方 —— **"死了不管"对无副作用任务是节俭，对有副作用任务是灾难**。

#### 12.4.1 为什么需要粘性（本期仍需要）

> 本期 1:1，没有「B、C 分散在不同节点」的问题，但**同一条链的连续任务仍应落在同一台机器上** ——
> 因为它们共享工作目录、git worktree、缓存。粘性逻辑因此保留，只是少了「一组分支要粘在一起」这一层。

#### 12.4.1b 原来的分析（扇出场景，留档）

`parent_task_id` 计数汇聚（§8.6.1）引入了一个新要求：**B、C、D 必须在"各自的执行上下文"里能读到彼此**。

如果负载均衡把 B 派给 node1、C 派给 node2，那么"汇聚"发生在中枢的 DB 里（`COUNT(*)`），这没问题。**但 E 需要的不是"个数"，是 B 和 C 的实际产出**：

```text
E 收到 [B_succeeded, C_succeeded] 两个事件
→ E 的执行需要 B 和 C 的 result 内容
→ 如果 result 只在 node1/node2 的本地磁盘上，E（在 node3）读不到
```text

所以有两个层次的问题，必须分开看：

| 层次 | 问题 | 解法 |
| :-- | :-- | :-- |
| **计数层** | 「B、C、D 都落定了吗」 | 中枢 DB 的 `COUNT(*)`，与节点无关 ✅ |
| **数据层** | 「E 怎么拿到 B、C 的产出」 | **靠 `tasks.result`（中枢已存 ≤8KB）**，或者靠节点间共享目录 |

**关键结论：只要 `result` 在中枢（当前设计就是 ≤8KB 摘要），E 在哪个节点都能跑，粘性不是汇聚的必要条件。**

那粘性解决什么？解决**大产出**的场景：

| 场景 | result ≤ 8KB 够吗 | 需要粘性吗 |
| :-- | :-- | :-- |
| 审查结论（几百字 + 几个文件路径） | ✅ 够 | ❌ 不需要 |
| 探索结果（大段代码片段、完整 diff） | ❌ 不够 | ✅ **需要** |
| 构建产物（二进制、镜像） | ❌ 不够 | ✅ **必须** |

**所以粘性是为了「同一批工作共享本地状态」**（工作目录、临时文件、git worktree、缓存），不是为了汇聚计数。

#### 12.4.2 owner 的方案：hash → 查表 → 粘住

```text
① 第一次：sticky_key = hash(trace_id ∥ rule_id ∥ node_selector)
           查 node_pins 表：没有 → 按负载均衡选一个 → 写入 pin
② 后续：   同样的 sticky_key → 查到 pin → 路由回那个节点
③ 节点下线：pin 仍然存在，但节点不可用
```text

**为什么 key 里要有 `trace_id`**：不同 trace 各自独立，不应该互相粘在同一个节点上（否则第一个 trace 就把节点占满了）。

**为什么 key 里要有 `rule_id`**：同一 trace 里不同规则不该被迫落在同一节点（`review` 和 `notify` 完全可以在不同机器）。

**为什么是 `hash` 而不是直接随机**：hash 让"同一个 key 在重启后仍算到同一个节点"，是**可复现的**。这是 hash 相对随机的唯一优势，但对运维排障很重要（"为什么这个 trace 总在 node2 上"）。

```sql
CREATE TABLE node_pins (
  sticky_key   TEXT PRIMARY KEY,      -- sha256(trace_id ∥ rule_id ∥ selector)
  node_id      TEXT NOT NULL REFERENCES nodes(id),
  trace_id     TEXT NOT NULL,
  rule_id      TEXT NOT NULL,
  pinned_at    INTEGER NOT NULL,
  -- 不设 TTL：pin 的生命周期 = trace 的生命周期
  UNIQUE(trace_id, rule_id, selector_hash)
);
CREATE INDEX idx_node_pins_node ON node_pins(node_id);
```text

#### 12.4.3 ⚠️ 必须分级：「节点下线不管」只对小副作用任务成立

owner 说「下线了不管，当死消息」。**这个原则在无副作用任务上完全正确，在有副作用任务上是灾难。**

反例：

```text
B 是「修复代码并 git push 到分支」
B 在 node1 上跑到 90%，node1 被拔网线
→ 按"死了不管"：B 记为 abandoned，trace 就此结束
→ 用户看到"任务失败"，但 node1 的磁盘上有个半完成的 worktree
  以及**可能已经 push 了一半的分支**
→ 如果 operator 手动重跑，可能产生重复的 commit / PR
```text

所以按**副作用级别**分三档，这是 `manifest.commands[].idempotent` 的另一个用途：

| 命令类型 | 节点下线时 | 理由 |
| :-- | :-- | :-- |
| **`idempotent: true`**（只读：lint、审查、探索） | **abandoned，不重派**（owner 的方案） | 重跑无副作用，但也没必要 —— 让用户决定 |
| **`idempotent: false`**（有副作用：push、deploy） | **abandoned + 显式告警 + 需要人工确认** | 半完成状态是危险的，不能自动重跑 |
| **`resumable: true`**（可断点续传） | 重派到**新节点**（知道自己的进度） | 节点侧有 checkpoint |

#### 12.4.8 任务级生命周期属性（owner 提出）

> owner：**「任务可以配置属性，比如过期多久、能不能重投」**

对，而且这几条应该**与 `idempotent` 分开**：`idempotent` 描述「重跑安全吗」，这里描述「允许重跑多久、重跑几次」。

```jsonc
{
  "action": { "kind": "cmd", "name": "review" },
  "lifecycle": {
    "expiresInSec": 3600,     // 硬过期：到点直接 abandoned，绝不重派
    "retryable": true,        // false = 一次性执行
    "maxAttempts": 3,         // 覆盖中枢默认
    "resultTtlSec": 604800    // result 保留 7 天（默认永不删）
  }
}
```text

| 属性 | 默认 | 谁可覆盖 | 作用 |
| :-- | :-- | :-- | :-- |
| `expiresInSec` | 继承 trace 的 `max_duration` 余额 | 规则 / manifest | **硬过期**。到点即使还在排队也变 `abandoned`，**不重派** |
| `retryable` | `1` | 规则 / manifest（`idempotent:false` → 强制 `0`） | `0` = 一次性，失败直接终态 |
| `maxAttempts` | 3（中枢） | 规则 | **中枢重派**上限，与节点内部重试无关（§12.2.6） |
| `resultTtlSec` | NULL = 永不删 | 规则 | `tasks.result` 保留期；到期**清空 result 但保留行** |

**为什么 `expiresAt` 必须是硬过期**：

```text
B 是「部署到 staging」，排队 2 小时后终于轮到
→ 但 2 小时前的代码已经过期，跑它毫无意义
→ 硬过期让它直接 abandoned，而不是消费一次部署窗口
```text

**为什么 `resultTtlSec` 只清 result 不清行**：trace 图需要「这里有个任务、它成功了」，但 8KB result 不必永久保留。清空后行仍在、状态仍在，图仍完整。

**`retryable` vs `idempotent`（容易混淆）**：

| | `idempotent` | `retryable` |
| :-- | :-- | :-- |
| 回答 | 「重跑**会不会**产生副作用」 | 「**允许**重跑吗」 |
| 属于 | 节点能力（manifest） | 任务策略（规则） |
| 关系 | `idempotent:false` → **强制** `retryable=0` | `retryable=0` 也可加在 `idempotent:true` 上 |

**节点说「重跑危险」是硬约束；规则只能更保守，不能更激进。**

**默认是 `idempotent: false`**（保守）：未声明的命令在节点下线时**不自动重派**，而是标 `abandoned` 并在控制台告警。这与「宁可少做，绝不多做」一致。

#### 12.4.4 rebalance：pin 失效的三种情况

owner 提到「如果发生了 rebalance 的话怎么处理」。分清三种：

| 情况 | pin 还在吗 | 处理 |
| :-- | :-- | :-- |
| **节点暂时离线**（pod 重启、网络抖动） | ✅ 在 | 任务等租约超时 → 重派时**仍用这个 pin**（节点可能回来了）。如果连续 N 次超时，pin 标记 `stale` |
| **节点被移除/吊销**（`revoked_at` 非空） | ✅ 在但失效 | pin 删掉，重新 hash 选新节点。**这条才是真正的 rebalance** |
| **节点能力变了**（manifest 不再提供该命令） | ✅ 在但不可用 | pin 保留，但 `capabilities` 不匹配 → 立即重选并更新 pin，写审计 |

**关键：rebalance 不是「重新 hash 全部」，而是「只处理失效的那几个 pin」。** 因为 hash 只用于**首次**选择，之后一律查表 —— 这就避免了经典的「一致性 hash 环大幅重排」问题。

owner 说的「第一次 hash 出来了节点，就后面的事件过来查表」正是这个意思，**这是对的，而且比一致性 hash 简单得多**。

#### 12.4.5 「死消息」怎么处理才不丢

owner 说「这个就当做死了的消息」。这里要注意：**"当死消息"不等于"丢弃"**。

| 处理方式 | 后果 |
| :-- | :-- |
| 直接删掉 | ❌ trace 永久缺一块，用户看不到发生了什么 |
| 标 `abandoned` 留在表里 | ✅ 事实留在 DB，trace 图上看得到「这里死了」 |
| 标 `abandoned` + 通知 | ✅✅ 用户知道要人工介入（**默认行为**） |

所以 `abandoned` 是**终态但不等于成功**。它和 `failed` 的区别：

| 状态 | 含义 | 会重试吗 | 会通知吗 |
| :-- | :-- | :-- | :-- |
| `failed` | 节点明确报告失败 | 按 `max_attempts` | 按规则 |
| `abandoned` | 节点消失，永远不知道结果 | ❌ **不重试**（除非 `idempotent: true`） | ✅ **默认通知**（因为结果未知） |
| `canceled` | 人/规则主动取消 | ❌ | 按规则 |

#### 12.4.6 pin 的清理

pin 不能无限增长。生命周期 = trace 的生命周期：

```sql
-- trace 落定（settled/failed/expired）时级联删除
DELETE FROM node_pins WHERE trace_id = ?;
```text

**为什么不在 trace 还在跑的时候清理**：还在跑的 trace 随时可能派生新任务，pin 必须一直在。这也是为什么 pin 不设 TTL —— **TTL 与 trace 生命周期不一致会导致「跑到一半 pin 消失、被迫 rebalance」**，那正是我们想避免的。

#### 12.4.7 控制台展示

```text
trace trc_7f2a
├─ review      → node:desktop    📌 pinned (sticky 命中)
├─ explore-fix → node:desktop    📌 pinned
├─ explore-test→ node:desktop    📌 pinned (同一 sticky_key)
└─ summarize   → node:laptop     ← 首个任务在本 trace 内，无 pin
```text

「📌 pinned」这个标记很重要 —— 它让用户理解「为什么这个 trace 一直在这台机器上」，而不是怀疑调度坏了。

### 12.5 拓扑图：从「两两连线」改成「类型级二部图」

**先看坑。** 如果按字面画「所有 source × 所有 sink → 连线」，一张图会变成这样：

```text
        webpush  http-hook  node:laptop  node:desktop  node:old
cron      ·         ·            ·            ·           ·      ← 15 条线 + 9 条虚线
github    ·         ·            ·            ·           ·         全部来自 3 条规则
watch     ·         ·            ·            ·           ·
```text

**大部分边不表达任何意图**，用户看不出来该干嘛。正确的图粒度是**类型级**：

| 图的节点 | 从哪来 |
| :-- | :-- |
| 事件类型节点 | registry（只画**有规则引用**的，或 `count > 0` 的） |
| 动作/命令节点 | 节点 manifest 的 `commands[].name`（聚合，不按节点拆） |
| 规则边 | 规则本身：`触发类型 → 动作名` |
| 回环边 | `produces[]` 里的类型：`命令 → 事件类型`（虚线，标注「接力」） |

于是「A 完成交给 B，B 完成推给人」会画成一条**真实可读的路径**，而不是 15 条散线：

```text
pr.opened ──[rev-pr]──▶ review ──produces──▶ task.succeeded
                                                  │
                                          [allow_chain: true]
                                                  ▼
                                          implement-fix ──produces──▶ task.succeeded
                                                                          │
                                                                     [notify]
                                                                          ▼
                                                                       📱 webpush
```text

#### 12.5.1 与 Trace 图重叠（版本化之后就成立了）

§8.7.1 原来写「Planner 图与 Trace 图必须是两张图」，理由是「改规则会让历史图变形」。**有了 §12.2 的版本钉住，这条限制消失了**：

| 图层 | 来源 | 渲染 |
| :-- | :-- | :-- |
| **Planner（当时）** | `traces.rule_version_id` 快照 + 各 task 的 `manifest_version_id` | 灰色细线 = 当时存在但未走到 |
| **Trace（实际）** | 实际 `events` + `tasks` 行 | 亮色实线 = 实际走过 |
| **剪枝** | 中枢的 `pruned` span event | 红色虚框 + 原因 |
| **合批** | `tasks.inputs[]`（Span Links） | 汇合处加粗连线 |

**同一张画布三层叠加**，于是「我配了什么」与「实际跑了什么」的差集一眼可见 —— 这就是要的重叠。

> 仍保留**纯 Planner 视图**（看改动前的图）与**纯 Trace 视图**（干净排障）。三个模式：`?view=planner` / `?view=trace` / `?view=overlay`（默认）。

三个视图各有用途：

| 视图 | 回答什么问题 | 给谁看 |
| :-- | :-- | :-- |
| **规则图**（类型级，当前版本） | 「我的自动化长什么样，哪里断了」 | 日常运维 |
| **叠加图**（当时 planner + 实际 trace） | 「这次为什么没走到那里」 | 排障 |
| **节点图**（实例级） | 「哪台机器闲着、谁挂着任务」 | 容量与故障排查 |

### 12.6 这个「提前声明」真正解决的是一个**上线即失效**问题

现有设计里 `capabilities` 是可选的，selector 里写 `capability: review` 而**恰好没有任何节点提供它** → 任务全部落到 `pending` → 到 `attempts` 上限 → `abandoned` → 进 DLQ。

也就是说：**用户会先配好一切、然后发现自己配了个空的东西。** 这不是小毛病，它会直接吃掉信任。

所以 manifest 的第一个交付价值不是「画图好看」，而是三个前置校验（保存规则时立即给反馈，不是等运行时）：

| 检查 | 时机 | 反馈 |
| :-- | :-- | :-- |
| `node_selector.capability` 在**至少一个在线节点**的 manifest 中存在 | 保存规则时 | ⚠️「当前没有节点提供 `review`，这条规则会一直 pending」 |
| `action.params` 的 `required` 字段与 manifest 声明的 `params` 对得上 | 保存规则时 | ⚠️「`review` 需要 `prompt`，你没给」 |
| 该 `node_selector` 在当前在线节点里**能匹配到人** | 保存规则时 | ⚠️「标签 `gpu=true` 无匹配节点」 |

### 12.7 提前声明还有两个便宜的好处

1. **控制台能自动生成表单**。用户建规则时选 `cmd:review`，编辑器按 manifest 的 `params` 渲染输入框 —— 不用手写 JSON，也不会拼错 key。
2. **`costClass` 用于展示**：建规则时能显示「这条链有 3 个付费任务」（不再参与闸门计算，见 §12.2.5）。

### 12.8 三条边界（防止这个需求长成 workflow 引擎）

| ❌ 越界 | 为什么 |
| :-- | :-- |
| 强制 payload schema 校验 | `§1` 已否决。做成 advisory（`advisory: true`），写入永不拒绝 |
| 类型级「schema 版本迁移」 | 事件是事实记录，不改写历史。新类型就是新字符串 |
| 用 manifest 推导**自动建边** | planner 由人的**规则**驱动，绝不自动连。没有规则就没有任务 —— 否则一次 manifest 更新就能凭空产生自动化 |

> 一句话：manifest 让**不可见的失败变成保存时的警告**，runner 图让**规则的全貌可读**。它不是编排引擎，也不该长出编排能力。

---

## 13. 多租户占位

**否决 fork**（两代码库 × 「e2e 全绿才算完成」的文化 = 必然漂移）。

| 项 | 现在 | 将来 |
| :-- | :-- | :-- |
| 开关 | `ANOTIFY_MULTI_TENANT=0`（默认） | `=1` |
| 隔离 | `internal/tenancy.Current(ctx)` 恒返回 `"default"` | 真实解析 |
| 表 | 所有新表带 `tenant_id NOT NULL DEFAULT 'default'` | 同 |
| 查询 | 新代码禁止「无 tenant 维度查全表」 | 同 |
| e2e | 每套件独立起服 → 两种模式各跑一遍 | 同 |

**将来要付的真账**（知道就好，不做）：`store/admin.go:40-48` 的 `ListUsersWithStats` 无 user 过滤、`SystemStats` 是全局的、`server/ratelimit.go:22-90` 限速器是进程内 map、WebAuthn RP ID 单 origin（需子域一租户）。

---

## 14. 现有代码：复用 / 重写判定

> 前提：**本项目仍在开发阶段，无历史兼容包袱**。能复用就复用，不能就重写。

| 子系统 | 判定 | 理由 |
| :-- | :-- | :-- |
| `internal/store`（SQLite + 幂等迁移模式） | ✅ **保留** | 能跑，有往返一致性单测，schema 驱动 |
| `internal/auth` 全家桶（webauthn / session / apikey / cliauth / enroll / recovery / admin） | ✅ **完整保留** | 与转向正交，是真实工作量 |
| `internal/push` + `web/sw.js` + VAPID | ✅ **完整保留** | human sink 的实现，一行不用改 |
| `internal/ws` 协议骨架（hello/replay/ack/resume/心跳） | 🔧 **保留骨架，重写帧** | 与节点需求几乎一一对应 |
| `internal/route/filter`（三条件匹配） | 🔧 **保留思路，泛化为 attributes 谓词** | 已在做正确的事，只是维度太少 |
| `internal/api/notify.go` | ❌ **删除**（无适配器） | `/v1/notify` 彻底下线，改用 `/v1/events`（§5.4） |
| `web-src` + `sitegen` + `hash.mjs` + `check-classes` | ✅ **保留** | 构建系统成熟，别动 |
| 前端 `index` / `keys` / `security` / `admin` / `login` 页 | ✅ **保留** | |
| `internal/broker` | ❌ **删除，新写 `internal/eventlog`** | 接口是「按用户订阅通知」；事件日志要 append / read / lease。保留事务内 seq 技巧 |
| `messages` 表 | ❌ **删除，`events` 取代** | 语义不同；无双写期成本更低（§14.1） |
| `dispatchManager`（per-user push goroutine） | ❌ **删除** | 被 sink 抽象取代 |
| `broker.IsTerminal` / `AgentState` 五枚举 | ❌ **删除** | 事件类型是开放字符串，hub 不再枚举状态 |
| `web-src/pages/docs.html` | 🔧 **重写 API 章节** | 现在是产品文档的入口，必须与新契约一致 |
| `cmd/devseed` | ✅ **保留并扩展** | e2e 播种必备 |

### 13.1 数据迁移：不存在这一步

**整个数据库清空重建**（owner 已决定）。于是没有迁移代码、没有双写期、没有灰度、没有兼容分支。

具体后果：

| 项 | 处置 |
| :-- | :-- |
| `messages` 表 | 从 `schema.sql` **彻底删除**（不是保留为只读历史） |
| `deliveries` 表 | 直接改为 `event_deliveries`，无过渡期 |
| `devices.event_scope` 列 | **彻底删除**（不再需要「保留但不读」的妥协，见 U7） |
| `migrateColumns` 里的历史 ALTER | 清空（只留未来要用的机制） |
| `migrateMessagesDropKindReply` | **删除**（不再需要重建表迁移） |
| `eventlog` 的 `seq` 生成 | 仍是「事务内 `MAX(seq)+1`」，但**不再有 legacy 数据**参与 |

**保留的机制**：`schema.sql` 作为单一事实源 + `store.Open` 里的幂等迁移框架。未来仍需要，只是当前只有一份干净的 schema。

**升级说明只需一句**：v2.0 是新产品，升级需清空数据目录（`ANOTIFY_DB`）。

---

## 15. UI 面

| 页面 | 状态 | 内容 |
| :-- | :-- | :-- |
| `events.html` | 🆕 | 事件流（SSE 实时），左筛选右详情，属性表 + payload 折叠 |
| `rules.html` | 🆕 | 规则列表 + 编辑器 + **explain 面板**（逐子句 ✅/❌）+ **保存时预检**（§12.4） |
| `topology.html` | 🆕 | **三模式图**：`planner`（当前规则）/ `trace`（实际路径）/ `overlay`（默认，当时 planner + 实际 trace 叠加）。+ 实例级节点图（§12.3.1） |
| `trace.html` | 🆕 | **Trace 回放**：OTel 形状的 span 树 + 「车流」动画（实际路过的节点依次点亮，剪枝边静态标注）+ 时间轴 scrub（§8.7） |
| `sources.html` | 🆕 | 触发源列表（cron 下次触发时间 / webhook URL + secret / watch 状态） |
| `nodes.html` | 🆕 | 节点列表（在线、标签、并发、能力）、enroll 流程 |
| `tasks.html` | 🆕 | 任务列表 + 详情 + 取消/重试 + **trace 链视图** |
| `index.html` | 🔧 | 总览改为「事件量 / 任务成功率 / 节点在线数 / 最近链」 |
| `receivers.html` | 🔧 | 设备管理保留，加「sink 健康自检」横幅 |
| `keys.html` `security.html` `admin.html` `login.html` `docs.html` | ✅ | 保留 |

**控制台必须实时**：事件中心的控制台不实时就是废的。用 SSE（比 WS 简单；`server/middleware.go:44-49` 的 `statusWriter` 已实现 `Flusher`）。

---

## 16. 安全模型（威胁表）

| 威胁 | 后果 | 缓解 |
| :-- | :-- | :-- |
| **中枢被完全攻陷** | 最坏 | 命名命令注册表（§9.3）→ 只能调已定义命令；无 argv 可下发 |
| **node token 泄漏** | 拿到该节点的派发权 | 标签由中枢赋值（不能提权）；可吊销；`capabilities` 自报只导致失败 |
| **webhook secret 泄漏** | 伪造事件 | 只存哈希；可轮转；`dedupe_key` UNIQUE 防重放；HMAC 可选 |
| **恶意 event payload** | XSS 进控制台 | 永不 `innerHTML`（现有 `el()` 已用 `textContent`）；payload 只做 JSON 树渲染 |
| **链风暴** | 通宵烧钱 | G1–G4 + 每规则 `daily_budget` + 熔断（同 `idempotency_key` 连续 N 次失败即暂停规则） |
| **推送洪水** | 用户关掉通知 | 白名单 + mute + digest + 速率上限（§10.4） |
| **日志泄漏 secret** | 不可逆 | 中枢只存摘要；`logs-request` 显式开启且可审计 |
| **中枢 API key 泄漏** | 写入垃圾事件 | scope 分离（`events:write` / `tasks:read` / `nodes:admin`）；限速 |

---

## 17. 成本与容量

| 项 | 估算 | 结论 |
| :-- | :-- | :-- |
| 事件量 | cron 每分钟 × 10 源 = 14k/天 + webhook 数百 | SQLite 单写完全够 |
| 存储 | 100k 事件/天 × 平均 2KB = 200MB/天 | 需 **90 天 TTL 清理**（`DeleteExpired` 现在零调用，必须接上）+ payload 截断至 64KB |
| 任务 | 每天几十~几百 | 无压力 |
| 节点 | < 20 | 无压力 |
| **LLM 成本** | **不是 hub 的问题，但 hub 必须能兜住** | 每规则 `daily_budget` + 熔断 |

---

## 18. 验收标准（怎么知道做对了）

不是「测试通过」，而是这 30 个演示能跑通：

| # | 演示 | 验证了什么 |
| :-- | :-- | :-- |
| A1 | cron 每 2 分钟 → 节点跑 `echo` → 控制台 SSE 实时看到 → 推送到手机 | 全链路 + 实时性 + human sink |
| **A1'** | **全新部署**：只建一条 `kind: notify` 规则 → 一条 curl `POST /v1/events` → 手机收到推送 | **旧产品最简用法的等价性**（§5.4） |
| A2 | 真 GitHub PR → 审查 agent → 修复 agent → **收敛一条**通知 | chaining + 通知收敛 + 指针传递 |
| A3 | 拔网线 → 节点 offline → 任务不派给离线节点 → 恢复后补派 | 节点对账 |
| A4 | 在 hub 的 SQLite 里 `grep argv` → **零命中**；下发未注册命令 → `unknown_action` | 安全不变量 |
| A5 | `kill -9` hub → 重启 → 租约过期 → 任务重回 pending，**无重复执行** | 租约与幂等 |
| **A6** | **本期就验**：配 `rev-pr → impl-fix → rev-pr` 的回边 → 第二次 `rev-pr` 被**静默剪枝**（不报错、不暂停 trace），trace 上显示「已剪枝：path 中已有 rev-pr」 | 1:1 仍允许成环，剪枝而非拦截（§8.5b-5） |
| **A8** | PR→A→{C,D}→E：A 派完 C、D 后结束（自动 seal）→ C、D 都落定 → **E 只跑一次**，`inputs=[tsk_C, tsk_D]` | 运行时屏障 + OTel Span Links（§8.6.1） |
| **A18** | C 成功、D 失败 → barrier **照样 fire**，E 被触发；payload 含两个 branch 的真实状态。**中枢未判断该不该跑** | `expect` 是事实不是策略（§8.6.1-1 / §8.6.1-3） |
| **A19** | 两个探索任务 B、C 都成功后 → **没有任何 E 被自动触发**（本期不收集）；要汇总就由 A 在自己进程内等 B、C | 收集放在节点里（§8.5b-4） |
| **A20** | 同一 trace 的 `review` 规则连跑 5 次（5 个不同事件）→ **全部落在同一节点**（`node_pins` 命中）；换一个 trace → 可能落另一节点 | 粘性路由（§12.4.2） |
| **A21** | 节点被吊销 → 该节点的 pin 全部失效并重选；节点只是临时离线 → pin **保留**，恢复后仍回原节点 | rebalance 只处理失效 pin（§12.4.4） |
| **A22** | 规则配 `expiresInSec:60`，任务排队超过 60s → 变 `abandoned` 且**不重派**；配 `retryable:false` → 首次失败即终态 | 任务级生命周期属性（§12.4.8） |
| **A23** | 规则里写 `join` 字段 → 保存时报错并提示「当前版本不做收集」 | 未支持的字段必须显式报错，不能静默忽略（§8.5b-3） |
| **A10** | 试一个「D 失败但上游仍回调」的节点配置（节点发 `task.failed{recoverable:true}`）→ E **照常跑**；换成节点不发事件 → E 不跑。全程中枢未做任何剪枝判断 | 「下游跑不跑由节点/规则决定」（§8.6.4） |
| **A11** | 用同一份 token 连续发 200 次派生请求 → **第 2 次起全部被拒**（`jti` 已消费），trace 只多 1 个任务 | 一次性消费封死重放（§8.3.1 / §8.3.4） |
| **A11'** | 改 token 里的 `hop`（3→1）→ 验签失败 401 | 跳数不可篡改（§8.3.2） |
| **A12** | 一个 `branchBudget=2` 的 token → A 成功派 C 和 D（各分 1）；A 再派第 3 次被拒；C 用掉自己那 1 个派 E 成功 | 扇出额度（§8.3.2） |
| **A12'** | 一个事件命中 3 条规则 → **建 3 个任务**（agent 审查 + 推手机 + 写日志），三条线各自独立延伸、互不等待；trace 上能看到 3 个并列 span | 扇出全部下发，不做收集（§8.5b-1） |
| **A12''** | 让 A 把 `branchBudget` 全分给 C（C=2, D=0）→ C 派生成功、D 派生返回 409；**trace 上 D 显示 🚫「派生额度用尽」**，而不是一片绿 | 额度用尽必须可见（§8.3.2b） |
| **A13** | 下发任务后拔掉节点网线 → 控制台 10s 内从「执行中」变「失联中」（`lease_expired`），而不是一直显示执行中 | ack 语义与状态归属（§6.3） |
| **A14** | 跑一条 trace → 删掉一条规则 → 重开旧 trace 的**叠加图**：那条边仍在（灰色未走到），图不变形；页面提示「此 trace 使用规则集 v12，当前 v15」 | 版本钉住使两图可重叠（§12.2.2） |
| **A16** | 用同一 token 起 3 个实例 → 控制台显示 1 个 node、3 个 instance；派发时选 inflight 最少的那个 | 共享 token + 两级模型（§12.3.2） |
| **A17** | 实例 A 报 `concurrency:2`、实例 B 报 `1` → 同时来 4 个任务：A 接 2、B 接 1，第 4 个排队；kill 掉 A → 它的任务等租约过期后重派给 B | 并发申报 + 单实例故障隔离（§12.3.3 / §12.3.6） |
| **A15** | 链跑到一半改规则 → 在飞任务**继续用旧快照**完成；新派生任务用新版本；全程 `rule_version_id` 可查；显式 rebase 有审计行 | 版本钉住正在跑的链（§12.2.3） |
| **A9** | 打开 `/v1/traces/{id}/graph?view=overlay` → 完整执行树（含剪枝边与原因）；**改规则后重新打开旧 trace → 图不变**（读的是钉住的 `rule_version_id` 快照） | Planner 数据来自版本快照，渲染时与 trace 叠加（§12.2 / §12.3.1） |
| **A7** | 建一条 `capability: review` 规则，但无节点提供该命令 → **保存时就告警**（不是跑完才 abandoned）；接上一个节点后告警消失，拓扑图上出现 `review` 节点与连线 | **capability manifest 的前置校验 + 拓扑图**（§12.4） |

---

## 19. 硬风险

| # | 风险 | 缓解 |
| :-- | :-- | :-- |
| R1 | **成本失控**（一条错规则 = 通宵烧钱） | `daily_budget` + 熔断 + 每节点并发上限，**是上线准入而非后期优化** |
| R2 | **单实例假设 × cron** | 调度器必须用 **DB 行租约认领**，保证将来加副本是改配置不是重写 |
| R3 | **零优雅退出**（全仓无 `signal.Notify`） | 有租约后这是正确性问题：进程被杀必须释放租约，否则节点白等 30s |
| R4 | **控制台无实时** | SSE |
| R5 | **节点凭据是新的爆炸半径** | §9 |
| R6 | **文档已在腐化**（`IOS_TESTING.md:56` 仍写 8 月删掉的 `status`） | 契约/行为变更必须同 PR 改文档 |
| R7 | **链风暴 / 成本放大** | G1–G6 + `traces` 预算快照，`blocked_reason` 控制台可见 |
| R8 | **「让中枢下发命令」的提案会不断冒出来** | 把「中枢从不携带 argv」写成 e2e 断言（A4） |

---

## 20. 未决项（需 owner 拍板）

| # | 问题 | 结论 |
| :-- | :-- | :-- |
| U1 | `max_hops` / `max_tasks` / `max_duration` | ✅ **全部可配**（三层：全局 env → 单规则 → 代码硬顶）。默认 **10 / 100 / 24h**，硬顶 **32 / 1000 / 7d**。**深度与成本是两个正交预算**：`max_hops` 管可理解性，`max_tasks` + `daily_budget` 管钱，`max_duration` 管卡死（§8.3 / §12.2.5） |
| U2 | 链默认 `on_failure` | ✅ `retry_step` |
| U3 | 节点标签来源 | ✅ 中枢赋值，**绑定 `node_id`（凭据身份）**，不接受自报；`os`/`arch` 只进 `reported` 供展示（§9.4 / §9.5） |
| U4 | 深链形态 | ✅ `/e/{id}` 短链 + 302 分派，保留 `?next=`（§10.5） |
| U5 | human-in-the-loop gate | ✅ 本期不做，但**预留**：`tasks.requires_approval` + 复用 `cli_auth_sessions` 的「敲门 → 待批准 → 批准/拒绝」状态机 |
| U6 | metrics | ✅ 本期不上；但 `/health` 必须查 DB 连通性 |
| U7 | `event_scope` 列 | ✅ **连同数据一起删除**（DB 清空，无需妥协） |
| — | 数据迁移 | ✅ **不做**。DB 整体清空重建（§14.1） |
| U8 | `/v1/notify` 去留 | ✅ **彻底删除，无兼容层**；插件仓库同步改造；纯通知场景改走 `notify` 规则模板（§5.4） |
| U9 | 节点是否提前暴露类型 | ✅ **是，但它暴露的是 capability manifest（能执行什么），不是事件类型**（事件类型由 source 产生）。payload schema 为 advisory，不强制校验。拓扑图用**类型级二部图**而非全连线。见 §12 |
| U10 | 拓扑图粒度 | ✅ 类型级（规则图）+ 实例级（节点图）两个视图，不画「所有 source × 所有 sink」的全连线图（§12.3） |
| **U11** | **是否允许环** | ✅ **允许**。靠 `path` 里 rule_id 去重保证终止（`max_hops` 是其上界）。回边是**剪枝**不是拦截，不暂停 trace、不写 `trace.blocked`（§8.3） |
| **U12** | **扇出后多下游汇聚（A→{C,D}→E）** | ✅ **父任务计数**（owner 方案）：B/C/D 携带 `parent_task_id`；父终态 ∧ 子全终态 → 触发下游一次。**不需要 barrier 表、不需要额外 API、不需要计数参数**（§8.6.1）。`batching.windowMs` 保留但用途不同 |
| **U13** | **Trace 存不存快照** | ✅ **trace 本身不存**（从 `events` + `tasks` 现算）；但**规则集与 manifest 必须版本化存快照**，否则「当时 planner 是什么样」无从复原（§12.2） |
| **U15** | **C 成功、D 失败时 E 跑不跑** | ✅ **中枢不判断**。节点决定回不回调、回调什么；规则决定接不接。中枢只做「分发 / 记事实 / 执行硬限制」。`traces.prune` 字段**已删除**（§8.6.3） |
| **U16** | **Trace 的数据模型** | ✅ **对齐 OpenTelemetry**：Trace / span_id / parent_span_id / Span Links / span events；新增 producer/consumer span 形态，让「事件 → 任务 → 事件」串成一条 trace（§8.7） |
| **U17** | **节点能否自主派生（模型 A/B）** | ✅ **模型 B**：节点可自主发起**下一跳**（本期严格 1:1，一次派生）。**JWT 携带签名 `hop`** + **`jti` 一次性消费**（§8.3.1 / §8.5b） |
| **U18** | **靠签名防跳数篡改够不够** | ✅ **够**（我上一版判断有误）。**签名额度 + 一次性消费**在结构上封死了跳数篡改与指数爆炸。剩余线性成本 → G4 `max_tasks` + `daily_budget`（§8.3.1 / §12.2.5） |
| **U24** | **版本标识用什么** | ✅ **`<prefix>_<UTC时间>_<hash前8>`**（owner 提出），如 `reg_20261023T101500Z_a1b2c3d4`。时间可读可排序、hash 去重。**但 trace 钉住的是 hash 版本 id，不是时间** —— 否则补录历史版本会让旧 trace 变形（§12.2.2） |
| **U25** | **trace_id / span 是否进元数据** | ✅ **进普通字段**（owner 提出），与 `source`/`type` 平级，带索引。统一查询、事件与任务同构、自带审计（§12.2.4） |
| **U26** | **`paidBudget` 保留吗** | ✅ **删除**（owner 决定）。它是唯一「计量源在节点」的预算，节点可自报 `costClass` 绕过。成本改由 G4 `max_tasks` + 每规则 `daily_budget`（中枢侧计量）。`costClass` 仅留 UI 展示（§12.2.5） |
| **U29** | **一个 token 能否启多个 node（负载均衡）** | ✅ **可以**（owner 提出）。但纠正前提：负载均衡本来就不需要共享 token（多 enroll 即可）。共享 token 的真实价值是**降低大规模部署运维成本**。设计成 `nodes`（身份）+ `node_instances`（执行器）两级（§12.3.2） |
| **U30** | **节点申报并发能力** | ✅ **采纳**（owner 提出）：实例自报 `concurrency` + 可选命令级 `concurrency`；中枢再加池级 `nodes.max_concurrency` 兜底。派发取三者最小值，按 `inflight` 最少优先（§12.3.3 / §12.3.4） |
| **U37** | **汇聚按什么分组** | ✅ **父任务声明的 `fanout`**（owner 三次修正后的最终形态）。既不是 `parent_task_id`（混批），**也不是 `event_id`**（错在假设扇出在事件入库时就完整 —— 动态派发的 agent 边算边派，`total` 只有**父终态/seal 时**才可信）。静态扇出自动建隐式 fanout，动态扇出由节点 `fanout.open/emit/seal`（§8.6.1） |
| **U38** | **汇聚用聚合查询还是计数器** | ✅ **计数器**（owner：「查询数据库这个方法不好」）。`fanouts.emitted/resolved` 在 emit / 终态时 O(1) 递增，同一事务内完成，无聚合扫描（§8.6.1-5） |
| **U40** | **本期范围：扇出做不做** | ✅ **扇出做，收集不做**（owner 修正，我前两版都错）。一个事件命中 N 条规则 → **派 N 个任务，N 多少不管**（一个 PR 既要派 agent 也要推手机）。不做的只是「下游收集」：无汇聚、无批次计数、无 `batching`、`join` 字段写了报错。`branchBudget` / G3b / `jti` 全部**保留**（§8.5b） |
| **U41** | **多条规则同时命中怎么办** | ✅ **全部命中都下发**（owner 修正）。我上一版说「按 priority 取第一条」是错的 —— 那会让「同一事件既派 agent 又推手机」不可能。同一事件的多条规则是**并行派发**，不是互斥选择（§8.5b-1） |
| **U39** | **任务能否配置过期与重投** | ✅ **能**（owner 提出）：`lifecycle.expiresInSec / retryable / maxAttempts / resultTtlSec`。`expiresAt` 是**硬过期**（到点 abandoned 绝不重派）；`retryable` 受 `idempotent:false` 强制为 0（§12.4.8） |
| **U34** | **负载均衡下的汇聚怎么做** | ✅ **粘性路由**（owner 方案）：首次 `hash(trace∥rule∥selector)` 选节点 → 写 `node_pins` → 后续查表。**rebalance 只处理失效 pin，不重排全局**（§12.4.2 / §12.4.4） |
| **U35** | **节点下线怎么办** | ✅ 分级（owner「死了不管」**只对小副作用成立**）：`idempotent:true` → abandoned 不重派；`idempotent:false`（**默认**）→ abandoned + 告警 + 人工确认；`resumable:true` → 重派新节点。**abandoned ≠ 成功，默认通知**（§12.4.3 / §12.4.5） |
| **U36** | **中枢内存存不存调度状态** | ✅ **不存**（owner 原则）。所有状态在 SQLite：`node_pins` / `tasks.parent_task_id` / 租约。进程重启后从表恢复，无内存漂移（§12.4 / §12.3） |
| **U32** | **汇聚要不要 `expect_count` 字段** | ❌ **不需要**（owner 方案 + 二次修正）：`parent_task_id` 已存在，`COUNT(*)` 就是「该等几个」。连 barrier 表都不需要（§8.6.1）。U15 删的 `prune` 是策略，与此无关 |
| **U33** | **汇聚要不要专门的超时机制** | ❌ **不需要**。每个任务已有租约超时，最终都会变成终态（含 `abandoned`）；父任务超时成 `abandoned` 后 `total` 也被定格。复用租约，不引入新参数（§8.6.1-6） |
| **U31** | **共享 token 的扩散风险** | ✅ `nodes.max_instances`（默认 20，超限拒绝新连接并告警）+ `nodes.max_concurrency` 中枢侧上限。谎报高 `concurrency` 只伤害自己，不构成对他人的攻击（§12.3.5） |
| **U28** | **node token 要不要短期化** | ⚠️ **待定**。调研显示长期 token 是运维事故高发点（GitHub 注册 token 1h、GitLab 410 Gone、Windmill 已转 workload identity）。当前设计是长期 token（argon2id 哈希）+ 手动 rotate。**建议增加 `expiresAt` + 自动续期**，见 `positioning.md` §5.4 |
| **U27** | **单任务重试次数谁定** | ✅ **节点自己定**（owner 决定）：`manifest.commands[].retries`。**节点内部重试不消耗中枢 `attempts` 配额**，分开计数（`tasks.internal_attempts` 仅展示）（§12.2.6） |
| **U22** | **节点能力与规则是否版本化** | ✅ **必须版本化**（owner 提出）：`node_manifest_versions` + `rule_versions` + `registry_versions`，**append-only**。trace 诞生时钉住版本 id → **Planner 与 Trace 可重叠渲染**（`?view=overlay`）+ 完整审计（§12.2） |
| **U23** | **正在跑的链遇到改规则** | ✅ **不重载**：trace 钉住旧快照；升级只能显式 `POST /v1/traces/{id}/rebase` 且有审计（§12.2.3） |
| **U21** | **`branchBudget` 怎么分配** | ✅ **由节点自主分配**（owner 选 A）：中枢只验签和递减，不参与分配、不预估图。但**额度用尽必须可见**（409 + `budget_exhausted` span event + 控制台 🚫），绝不静默失败（§8.3.2b） |
| **U18b** | **任务身份怎么生成** | ✅ **`trace_id + span_id`**（owner 方案）。`span_id` 由中枢每跳新生成，`task.id = tsk_{trace8}_{span8}`；`branchLocalId` 字段删除（§8.3.3） |
| **U19** | **ack 的语义** | ✅ **`ack` = 接单确认，不是完成**。`running` 只由心跳 `inflight` 维护；无 result 即未完成；新增 `lease_expired`（失联中）状态，不把断线显示成执行中（§6.3） |

### 20.1 删除 `/v1/notify` 的连带清单

| 连带项 | 处置 |
| :-- | :-- |
| `internal/api/notify.go` | 删除 |
| `AgentState` 五枚举 + `IsTerminal()` | 删除（事件类型为开放字符串） |
| `devices.event_scope` 列 | 删除 |
| `messages` 表 / `broker` 包 | 删除 |
| 外部 `anotify-plugins` 仓库 | **本仓不管**，但交付时需告知：`POST /v1/notify` → `POST /v1/events` |
| `web-src/pages/docs.html` | 重写 API 章节 |
| `web-src/pages/connect.html` | 接入引导改为「建一条 notify 规则 + 一条 curl」 |
| `scripts/e2e/suites/*` | `notify` 相关断言全改 `/v1/events` |
