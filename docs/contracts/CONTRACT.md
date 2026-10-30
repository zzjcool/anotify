# Anotify 实现契约 v1 · 冻结

> **状态：冻结。** 这是重构的唯一事实源。任何与它冲突的代码都是待改的。
> 设计依据：`design/wiki/`（10 页）。本文只写**实现层**：表、字段、状态机、协议帧、不变量。
>
> **无历史包袱**：库清空重建，不迁移、不双写、不兼容。
> 现有 `internal/store` 的 10 张表全部作废，按本文重建。

---

## 0. 不变量（最高优先级，任何实现不得违反）

| # | 不变量 | 违反的后果 |
|---|---|---|
| **I1** | **中枢 DB 无 `argv`、无 shell 字符串、无脚本文本** | 安全边界失效 |
| **I2** | **能力必须先定义**；节点只能声明已定义能力，不能发明名字、不能加出口 | 路由失去封闭集合 |
| **I3** | **`kind` 不落库、不写配置**，由声明推导 | 可表达出自相矛盾的配置 |
| **I4** | **`out` 与 `status` 正交**；失败也能继续，成功也能到头 | 无法表达「失败触发修复」 |
| **I5** | **`out: null` 也是已回报**；缺失 `out` ≠ `null`，缺失表示「没回报」 | 伪造「到头」 |
| **I6** | **中枢不解释任何用户提供的逻辑**：无表达式求值、无条件匹配、无优先级 | 引入求值器攻击面 |
| **I7** | **图里只有能力，没有机器**；`edges` 两端都是 `capability_id` | 换节点要改图 |
| **I8** | **一个能力定义 = 一个不可变版本**（append-only，钉 hash） | 旧 trace 无法解释 |
| **I9** | **空列表返回 `[]` 不返回 `null`** | 前端误判「未连接」 |
| **I10** | **一切租户数据先过 `org_id`**；命名空间资源还必须过 `namespace_id`，且两者必须一致（`namespace.org_id == org_id`） | 跨组织 / 跨命名空间泄漏 |
| **I11** | **跨命名空间不引用**：`edges` 两端、`source_targets` 目标、`node_capabilities` 声明、子任务继承，全部锁在同一个 `namespace_id` 内；违反一律 `400` | 预发的图能把任务派到生产的拓扑上 |

---

## 1. 表清单

**12 张新建 + 4 张沿用**，全部 `CREATE TABLE IF NOT EXISTS`。

| 表 | 归属 | 说明 |
|---|---|---|
| `orgs` | — | 组织（Tailscale tailnet 模型） |
| `org_members` | — | 用户 ↔ 组织（多对多） |
| `namespaces` | 组织 | 组织内的隔离单位（配置 + 运行面），见 §2.1.1 |
| `users` | 人 | 跨组织共享身份 |
| `passkeys` / `sessions` / `api_keys` | 人 | 认证（沿用现有实现，字段不变） |
| `capability_versions` | 命名空间 | 能力定义，append-only，钉 hash |
| `nodes` | 命名空间 | 一台机器 |
| `node_instances` | 命名空间 | 节点上的并发槽位 |
| `node_capabilities` | 命名空间 | 节点声明「我有 review@1」 |
| `node_pins` | 命名空间 | 粘性路由钉住 |
| `edges` | 命名空间 | 出口表，路由唯一依据 |
| `events` | 命名空间 | 事件日志（链的起点） |
| `tasks` | 命名空间 | 任务行 = OTel span |
| `graph_versions` | 命名空间 | 图快照，钉 hash |

### 1.1 本文定义的表（12 张）

`orgs` / `org_members` / `namespaces` / `capability_versions` / `nodes` /
`node_capabilities` / `node_instances` / `node_pins` / `edges` / `events` / `tasks` /
`graph_versions`

### 1.2 沿用不动的表（4 张）

`users` / `passkeys` / `sessions` / `api_keys` —— 字段和现有 `schema.sql` 一致，
本文不重写 DDL。改动只有一处：**所有属于组织的数据都要带 `org_id`**，这四张表属于人，不带。

另 `cli_auth_sessions` / `recovery_codes` 是 CLI 授权与恢复码，与本次重构无关，**原样保留**。

### 1.3 删除的旧表（3 张）

| 旧表 | 被什么取代 |
|---|---|
| `messages` | `events`（外部事件）+ `tasks`（执行单元） |
| `consumer_offsets` | 不再需要：`tasks` 的 `ack` 是任务级，不是流偏移 |
| `deliveries` | 推送结果记在 `tasks.result`（`notify.phone` 是个 `end` 能力） |

---

## 2. 表结构

### 2.1 组织与身份

```sql
CREATE TABLE IF NOT EXISTS orgs (
    id         TEXT PRIMARY KEY,              -- org_<base62>
    name       TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL               -- unixepoch 秒
);

CREATE TABLE IF NOT EXISTS org_members (
    org_id   TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role     TEXT NOT NULL DEFAULT 'member',  -- owner | admin | member
    joined_at INTEGER NOT NULL,
    PRIMARY KEY (org_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_org_members_user ON org_members(user_id);
```

**首次注册自动建组织**：注册事务内 `INSERT orgs` + `INSERT org_members(role='owner')`。
**代码里不存在「没有组织」的分支。**

#### 2.1.1 命名空间

**组织之下还有一层：命名空间。** 完整设计见 `docs/design/namespaces.md`，
这里只放实现必需的部分。

```sql
CREATE TABLE IF NOT EXISTS namespaces (
    id         TEXT PRIMARY KEY,              -- ns_<base62>
    org_id     TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug       TEXT NOT NULL,                 -- prod / staging / team-a（稳定，出现在 URL 里）
    name       TEXT NOT NULL,                 -- 展示名（可改）
    created_at INTEGER NOT NULL,
    UNIQUE (org_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_namespaces_org ON namespaces(org_id, created_at);
```

**隔离什么**：**全部资源** —— `capability_versions` / `nodes` / `node_capabilities` /
`node_instances` / `node_pins` / `edges` / `sources` / `events` / `tasks` / `graph_versions`。
**不隔离什么**：只有身份与密钥 —— `users` / `org_members`，以及 `api_keys`
（密钥可额外绑定一个命名空间来限制作用域，见设计文档 §2.3）。

**能力隔离后，节点协议不需要改**：命名空间放在**节点令牌**里，不在 `hello` 帧里。
节点令牌 → 节点行 → `namespace_id`，所以 `hello` 里的 `capabilities: ["review@1"]`
只在**该节点所在命名空间**里解析。完整理由见设计文档 §2.1。

**内置能力必须按命名空间播种**：新建命名空间时同事务插内置能力（否则新命名空间里
连 `notify.phone` 都没有），内置能力升版时遍历所有命名空间各插一行。
**建组织时同事务建 `default` 命名空间**（slug `default`，名「默认」）。
**代码里不存在「没有命名空间」的分支。**

**`namespace_id` 与 `org_id` 同时存在**（I10 的字面要求），但两者是包含关系：
`namespace.org_id` 就是这一行的 `org_id`。写入时由 store 层校验一致，**读路径一律用 `namespace_id`**。

### 2.2 能力定义（中枢侧，append-only）

```sql
CREATE TABLE IF NOT EXISTS capability_versions (
    id            TEXT PRIMARY KEY,           -- cap_<UTC>_<hash8>
    namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    org_id        TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,              -- review
    version       INTEGER NOT NULL,           -- 1
    params_schema TEXT NOT NULL,              -- JSON Schema
    outputs       TEXT NOT NULL,              -- JSON 数组，如 ["fix","clean"]
    lifecycle     TEXT NOT NULL DEFAULT '{}', -- JSON：expiresInSec/retryable/maxAttempts/resultTtlSec
    sticky        TEXT NOT NULL DEFAULT '',   -- '' | 'worktree' | ...
    source        TEXT NOT NULL,              -- builtin | custom
    content_hash  TEXT NOT NULL,              -- 规范化内容 sha256
    created_at    INTEGER NOT NULL,
    UNIQUE (namespace_id, name, version),
    UNIQUE (namespace_id, content_hash)       -- 同内容不产生第二行
);

CREATE INDEX IF NOT EXISTS idx_cap_name_ver
    ON capability_versions(namespace_id, name, version);
```

**名字的唯一性从「组织」缩小到「命名空间」（I11）**：`review@1` 在 prod 和 staging
是两个不同的定义、两个不同的 `id`。`edges.from_port` 的封闭集合、`kind` 推导，
全部在命名空间内闭合。

**`outputs` 是出口名的唯一来源。** 它同时是 `kind` 推导和 `edges.from_port` 的封闭集合。
**没有 `kind` 字段**——推导量不落库（I3）。

### 2.3 节点

```sql
CREATE TABLE IF NOT EXISTS nodes (
    id           TEXT PRIMARY KEY,            -- nd_<base62>
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,               -- mbp-16（可改，id 不变）
    tags         TEXT NOT NULL DEFAULT '[]',  -- JSON 数组，节点池标签
    status       TEXT NOT NULL DEFAULT 'offline', -- online | offline | draining
    token_hash   TEXT NOT NULL,               -- sha256，明文只在创建时返回一次
    max_instances   INTEGER NOT NULL DEFAULT 20,  -- 硬顶，防一份 token 复制出一片
    max_concurrency INTEGER NOT NULL DEFAULT 8,   -- 节点级并发上限
    last_seen_at INTEGER,
    created_at   INTEGER NOT NULL,
    UNIQUE (namespace_id, name)
);

/** 一台机器属于一个命名空间（I11）。令牌绑在这个命名空间上（见设计文档 §2.1），
  * 所以 `hello` 帧里的 `capabilities: ["review@1"]` 不会有歧义。
  * 要服务两个命名空间就注册两次 → 两个节点行、两个令牌。
  * 一个 WS 连接 = 一个节点 = 一个命名空间。 */

-- 节点声明「我有 review@1」——名字 + 版本，外加节点**自报的执行方式**（run_spec）。
-- 没有 argv、没有出口、没有 kind：run_spec 是节点自己的数据（JSON 原文），
-- 中枢**只存不执行**（I1），也不拿它参与派发选点。
-- 只能声明**本命名空间**定义的能力（I11，handler 校验）
CREATE TABLE IF NOT EXISTS node_capabilities (
    node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    run_spec      TEXT NOT NULL DEFAULT '{}',   -- 节点上报的执行方式原文；'{}' = 未声明
    PRIMARY KEY (node_id, capability_id)
);

CREATE INDEX IF NOT EXISTS idx_nodecap_cap ON node_capabilities(capability_id);

-- 并发槽位（node_instances），一个节点可以有 N 个
CREATE TABLE IF NOT EXISTS node_instances (
    id          TEXT PRIMARY KEY,             -- nins_<base62>
    node_id     TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    slot        INTEGER NOT NULL,             -- 0..N-1，节点申报
    concurrency INTEGER NOT NULL DEFAULT 1,   -- 这个实例能同时跑几份（节点申报）
    busy_task   TEXT,                         -- 当前任务 id，空闲为 NULL
    alive       INTEGER NOT NULL DEFAULT 1,   -- 0 = 已掉线（不等同于整个节点下线）
    updated_at  INTEGER NOT NULL,
    UNIQUE (node_id, slot)
);

-- 实际并发 = min(实例 concurrency, 实现级 capabilities[].concurrency, nodes.max_concurrency)

-- 粘性路由
CREATE TABLE IF NOT EXISTS node_pins (
    sticky_key   TEXT PRIMARY KEY,            -- sha256(trace_id ∥ capability_id ∥ selector)
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    selector     TEXT NOT NULL,               -- 当时命中的标签
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL
);
```

### 2.4 出口表（路由唯一依据）

```sql
CREATE TABLE IF NOT EXISTS edges (
    namespace_id    TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    from_is_source  INTEGER NOT NULL DEFAULT 0,   -- 0=能力边，1=入口边
    from_key        TEXT NOT NULL,                -- 能力 id 或 来源名（看上面）
    from_port       TEXT NOT NULL,                -- 出口名，能力边时必须 ∈ from 的 outputs
    to_capability   TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    created_at      INTEGER NOT NULL,
    PRIMARY KEY (namespace_id, from_is_source, from_key, from_port, to_capability)
);

CREATE INDEX IF NOT EXISTS idx_edges_lookup
    ON edges(namespace_id, from_is_source, from_key, from_port);
```

**边属于命名空间（I11）**：主键前缀从 `org_id` 换成 `namespace_id`。
同一对能力可以在**不同命名空间**各有一条边，互不影响。

**一行 = 一条边。同一个 `(from_key, from_port)` 可以有多行 → 扇出。**
**右端一定是能力定义，不引用任何机器（I7）。**

左端有两种（`from_is_source` 区分，见 #110）：
`0` = 能力之间的边（`from_key` 是能力 id，`from_port` 必须 ∈ 该能力的 `outputs`）；
`1` = **入口边**（`from_key` 是来源名，如 `github`；`from_port` 是事件类型，任意值 —— 外部系统决定）。

**I11 第 ① 条**：`from_is_source=0` 时两端能力的 `namespace_id` 都必须等于边的
`namespace_id`；把别处的能力 id 填进来在 SQL 层是合法的（外键只保证行存在），
所以 store 层**必须显式校验**，违反一律 `400`。

### 2.5 事件与任务

```sql
CREATE TABLE IF NOT EXISTS events (
    id           TEXT PRIMARY KEY,            -- evt_<base62>
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    trace_id     TEXT NOT NULL,               -- 一个根事件引起的全部工作
    source       TEXT NOT NULL,               -- github | cron | watch | http | manual（route = 剪枝保留名）
    type         TEXT NOT NULL,               -- pull_request.opened
    payload      TEXT NOT NULL DEFAULT '{}',  -- 原始 JSON，只存不解释
    idem_key     TEXT,                        -- 幂等键（触发器给；NULL 允许重复）
    created_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_trace ON events(namespace_id, trace_id);
-- 幂等键的作用域也是命名空间：同一个 idemKey 在 prod / staging 各收一次是正常的
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_idem
    ON events(namespace_id, source, idem_key) WHERE idem_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS tasks (
    id             TEXT PRIMARY KEY,          -- tsk_<base62>，同时是 OTel span_id
    namespace_id   TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    org_id         TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    trace_id       TEXT NOT NULL,
    parent_task_id TEXT,                      -- 谁派生；根事件直接派生的为 NULL
    path           TEXT NOT NULL DEFAULT '[]',-- JSON 数组：从根到本任务的能力序列（去重剪枝用）

    capability_id  TEXT NOT NULL REFERENCES capability_versions(id),
    capability     TEXT NOT NULL,             -- 冗余存 name，展示用
    version        INTEGER NOT NULL,
    params         TEXT NOT NULL DEFAULT '{}',-- 派发参数，已过 params_schema

    -- 状态机（九态，见 §3）
    status         TEXT NOT NULL DEFAULT 'pending',
    -- 执行归属
    node_id        TEXT,
    instance_id    TEXT,
    lease_until    INTEGER,                   -- 租约截止
    attempts       INTEGER NOT NULL DEFAULT 0,

    -- 结果（I4/I5）
    out            TEXT,                      -- 出口名；NULL 表示「没回报」，'' 不是合法值
    reported       INTEGER NOT NULL DEFAULT 0,-- 1 = 节点已回报（哪怕 out 是 NULL）
    summary        TEXT NOT NULL DEFAULT '',
    artifacts      TEXT NOT NULL DEFAULT '[]',
    error          TEXT NOT NULL DEFAULT '',

    -- 版本钉住（I8）
    cap_hash       TEXT NOT NULL,             -- 派发时的能力定义 content_hash
    graph_id       TEXT NOT NULL,             -- 派发时的 graph_versions.id

    created_at     INTEGER NOT NULL,
    started_at     INTEGER,
    finished_at    INTEGER
);

CREATE INDEX IF NOT EXISTS idx_tasks_trace   ON tasks(namespace_id, trace_id);
CREATE INDEX IF NOT EXISTS idx_tasks_parent  ON tasks(namespace_id, parent_task_id);
CREATE INDEX IF NOT EXISTS idx_tasks_lease   ON tasks(status, lease_until);
CREATE INDEX IF NOT EXISTS idx_tasks_node    ON tasks(node_id, status);
```

**`out` 三态必须靠两列表达**：

| `reported` | `out` | 含义 |
|---|---|---|
| 0 | `NULL` | **没回报**（漏报/超时）——不派下一跳，也不伪造「到头」 |
| 1 | `NULL` | **已回报，到头了** |
| 1 | `"fix"` | **已回报，走 fix 出口** |

### 2.6 图快照

```sql
CREATE TABLE IF NOT EXISTS graph_versions (
    id           TEXT PRIMARY KEY,            -- grf_<UTC>_<hash8>
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL,
    snapshot     TEXT NOT NULL,               -- JSON：全部 edges 的规范化快照
    created_at   INTEGER NOT NULL,
    UNIQUE (namespace_id, content_hash)
);
```

**快照的 hash 不含 `namespace_id`**：快照内容就是 edges 列表，命名空间已经由列表达。
把命名空间混进 hash 会让「同一张图在两个命名空间」变成两条内容不同、实际相同的快照。

**为什么钉图**：trace 要能重建「当时图长什么样」。`edges` 是可变的，`graph_versions` 不可变。

### 2.7 源（事件入口）

```sql
CREATE TABLE IF NOT EXISTS sources (
    id           TEXT PRIMARY KEY,            -- src_<base62>
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL,               -- cron | webhook | github | manual
    config       TEXT NOT NULL DEFAULT '{}',  -- 按 kind 解释的 JSON（I6：只读配置，不求值）
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_fired_at INTEGER,
    next_fire_at  INTEGER,
    created_at   INTEGER NOT NULL,
    UNIQUE (namespace_id, name)
);

CREATE TABLE IF NOT EXISTS source_targets (
    source_id     TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    params        TEXT NOT NULL DEFAULT '{}',
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (source_id, capability_id)
);
```

**源属于命名空间**：名字的唯一性从「组织」缩小到「命名空间」（`UNIQUE(namespace_id, name)`），
`source_targets` 的每一行都带 `namespace_id`（从源行取）。

**I11 第 ② 条**：`source_targets.capability_id` 指向的能力定义必须与源同一命名空间
（创建与更新两条路径都要校验，违反 `400`）。源不建边，它自己就是起点（见 #110）。

---

## 3. 状态机

### 3.1 九态（`tasks.status`）

```text
                    ┌──────────────────────────────────────┐
                    │                                      │
  pending ──assign──▶ assigned ──ack──▶ acked ──▶ running ──┤
     ▲                   │                │                │
     │                   │ lease 超时      │                │
     │                   ▼                ▼                ▼
     │             lease_expired ◀────────┘        succeeded / failed
     │                   │                                / canceled
     │      attempts 未用尽│                                / abandoned
     └───────────────────┘                        （终态，不可再变）
```

| 状态 | 含义 | 谁推进 |
|---|---|---|
| `pending` | 已建行，未派发 | 中枢（建任务时） |
| `assigned` | 已选中节点，等 `ack` | 中枢（派发时） |
| `acked` | 节点接单，租约开始计时 | 节点（`ack` 帧） |
| `running` | 节点报告开始执行 | 节点（`progress` 帧或隐式） |
| `succeeded` | 执行成功 | 节点（`result` 帧） |
| `failed` | 执行失败 | 节点（`result` 帧） |
| `canceled` | 被取消 | 中枢（用户/系统取消） |
| `abandoned` | 放弃，不重试 | 中枢（attempts 用尽/节点下线且不可重派） |
| `lease_expired` | **中间态**，不是终态 | 中枢（租约巡检） |

**终态集合** = `{succeeded, failed, canceled, abandoned}`。

### 3.2 状态迁移表（唯一合法迁移）

| From | To | 触发 | 条件 |
|---|---|---|---|
| `pending` | `assigned` | 中枢派发 | 选中节点 + 写租约 |
| `assigned` | `acked` | `ack` 帧 | 租约未过期 |
| `assigned` | `running` | `progress`/`result` 帧 | 节点跳过 ack（§3.1「或隐式」） |
| `assigned` | `lease_expired` | 租约巡检 | `now > lease_until` |
| `acked` | `running` | `progress` 帧 | — |
| `acked` | `lease_expired` | 租约巡检 | 心跳停 |
| `running` | 终态 | `result` 帧 | — |
| `running` | `lease_expired` | 租约巡检 | 心跳停 |
| `lease_expired` | `pending` | 中枢 | `attempts < maxAttempts` |
| `lease_expired` | `abandoned` | 中枢 | `attempts >= maxAttempts` 或实现 `idempotent:false` |
| `pending`/`assigned`/`acked`/`running` | `canceled` | 中枢 | 显式取消 |

**其余迁移一律拒绝**（返回 `ErrInvalidTransition`）。

### 3.3 租约与时序

| 参数 | 值 | 说明 |
|---|---|---|
| 租约 TTL | 30s | `lease_until = now + 30` |
| 心跳间隔 | 10s | 节点发 `heartbeat`，中枢把 `lease_until` 推到 `now + 30` |
| `ack` 等待 | 20s | 派发后 20s 未 `ack` → `lease_expired` |
| 退避 | 10s / 60s / 300s | 重派延迟 |
| 巡检周期 | 5s | 扫 `status IN (assigned,acked,running) AND lease_until < now` |

---

## 4. 派发四步（唯一路由算法）

```text
输入：org_id, namespace_id, 上游 (capability_id, out), params, trace_id, path, parent_task_id

① 查表
   SELECT to_capability FROM edges
    WHERE namespace_id=? AND from_capability=? AND from_port=?
   → N 行（N=0 则记 pruned/no_edge，结束）

② 校参数（对每个 to_capability）
   读 to 的 params_schema，校验 params
   失败 → 记 pruned/param_schema，不建任务（continue）

③ 找节点
   SELECT node_id FROM node_capabilities WHERE capability_id=?
   （只在这个命名空间的在线节点里挑，I11）
   → M 行（M=0 则记 pruned/no_node，结束）

④ 挑一台
   若能力开了 sticky：查 node_pins(sticky_key) → 命中就用，未命中按负载挑后写 pin
   否则按负载挑（busy 槽位最少，同分按 last_seen_at 最新）
   → 选 (node_id, instance_id)

⑤ 建任务（①~④ 全过之后，单条 INSERT）
   事务内：INSERT tasks + 写 pin / 占 instance
```

**① ② ③ 都发生在建任务之前**，所以剪枝不产生任务行。
**`pruned` 记录写在事件 span 上**（`tasks` 里没有对应的行），三态原因见 06 页。

**命名空间改 ①③⑤（I11），②④ 一个字不改**：能力定义和节点都按命名空间隔离了，
但参数校验（`params_schema` 属于能力自己）和挑机器（粘性/负载）跟命名空间无关。
子任务的 `namespace_id` **继承父任务**，不接受调用方指定。

---

## 5. `kind` 推导（不落库）

```go
func DeriveKind(outputs []string, declaredByAnyNode bool) Kind {
    if !declaredByAnyNode { return KindOrigin }
    for _, o := range outputs { if o != "" { return KindPipe } }
    return KindEnd
}
```

| 情况 | `kind` |
|---|---|
| 没有任何节点声明这个能力 | `origin` |
| 有节点声明，且能力 `outputs` 全为空 | `end` |
| 有节点声明，且能力 `outputs` 有非空项 | `pipe` |

**多能力混挂**：节点声明了多个能力时，只要**有任一**能力的 `outputs` 非空 → 该节点 `pipe`。

### 5.1 内置能力的 kind 特例

`DeriveKind` 只看 `outputs` + 节点声明，会把两类内置能力判错：

| 内置能力 | `DeriveKind` 会算出 | 实际含义 | 覆盖为 |
|---|---|---|---|
| `notify.phone@1`（中枢执行） | `origin`（无节点、outputs 空） | 链的**终点**，中枢自己执行 | `end` |
| `github`/`cron`/`http`/`manual` | `origin` | 链的起点 | `origin`（正确） |

所以全仓推导 kind 走 **`route.KindForCapability`**，不直接调 `DeriveKind`。
特例散落多处必然漂移——`notify.phone` 曾在能力页显示成 `origin`，误导用户把它当事件来源。

### 5.2 入口边的出口是「任意事件类型」

**（#110 后已无「内置起点能力」这个概念。** 本节保留编号，内容描述的是现在
的入口边；原「内置起点能力」的那 4 行已被 sources 取代，见 §5.4。）

事件从机器入口进来时（`POST /v1/events`），首轮派发查的是**入口边**：

```sql
edges.from_is_source = 1     -- 入口边
edges.from_key       = 'github'   -- 来源名（不是能力 id）
edges.from_port      = 'pull_request.opened'  -- 事件类型，任意值
```

入口边的出口**不校验封闭集合**（不变量 I2 的唯一例外）：

- 事件类型由外部系统决定（GitHub 发什么 type 就是什么），不可能预先枚举；
- 来源没有 `outputs` 这个概念，无从封闭。

**I2 没有被放松**：`from_is_source = 0` 的能力边，`from_port` 依然必须 ∈ 上游
能力的 `outputs`。

**为什么不再用「能力」当入口锚点**：那个"起点能力"不执行代码、不产生任务、
也没有地方存配置（cron 表达式之类），唯一作用是给边当左端。代价是能力列表
上多出 4 个用户看得见却用不上的条目。现在左端直接是来源名。

### 5.3 未知来源

**不再返回 422。** 来源不需要预先登记（它是自由字符串），所以"未知来源"
不是错误：事件照收（200），首轮派发查不到入口边 → `pruned reason=no_edge`。

理由：先让 webhook 打着、后回来建边是正常流程，事件不该因为还没配边而丢。

### 5.4 源（source）：事件的入口实例

**源 ≠ 能力。** 这个区分是 §5.1~5.3 那套「内置起点能力」的替代方案：

| | 能力（capability） | 源（source） |
|---|---|---|
| 回答 | 「能干什么」 | 「什么时候开始干」 |
| 谁声明 | 节点（`node_capabilities`） | 用户在「源」页面创建 |
| 有没有节点 | 有（`notify.phone` 除外，中枢执行） | **没有**，源自己不干活 |
| 有配置吗 | 只有 `params_schema` | 有（cron 表达式 / webhook token） |
| 怎么派发 | 查 `edges`（`from_port` 精确匹配） | 直接给 `source_targets` 里的能力 |

**源不建边。** 理由：

- 边是「能力 A 的某个出口 → 能力 B」，而源不是能力，没有 `outputs` 可枚举；
- 让用户为了配一个定时器去理解 `kind=origin`、再手建一条边，是多余的心智负担。

实现上走 `route.DispatchInput.Targets`：非空时**跳过 `EdgesFrom` 查表**，
其余四步（参数校验 / 找节点 / 占槽位 / 建任务）完全复用。这样源和普通派发
共享同一套校验，不会出现两条各自演化的路径。

三种源：

| kind | 触发方式 | 配置 |
|---|---|---|
| `cron` | 调度器到点（`next_fire_at`） | `{"expr":"0 3 * * *","tz":"Local"}` |
| `webhook` | 外部 POST `/v1/hooks/{token}` | `{"token":"src_..."}`（服务端生成） |
| `manual` | 页面点「立即运行」 | `{}` |

**`sources.kind` 与不变量 I3 的关系**：I3 规定「`kind` 不落库」，本意是
**推导量**不落库（能力的 kind 由 `outputs` + 节点声明推导，存下来会与推导
结果矛盾）。源的 `kind` 是用户显式选择的**配置**，无从推导、不存就不知道该
跑哪套逻辑，因此不受 I3 约束。守卫测试按表名限定而非按列名一刀切。

**源的参数（每个出口一份）**

源的每个出口能力**带自己的参数**，不是源上一份公共 params：

```
sources ──< source_targets(source_id, capability_id, params) >── capability_versions
```

为什么必须按出口分别存：每个能力的 `params_schema` 字段名与必填项都不同。
一个源同时喂 `analyze-pr`（要 `changedLines`）和 `deploy-preview`（要 `env`）时，
一份公共 params 必然对不上至少一边，触发时被 `param_schema` 剪枝，而用户配完
以为好了——那正是要消灭的静默失败。

派发时通过 `route.DispatchInput.TargetParams`（capabilityID → params）传递：
`Dispatch` 在步骤 ② 的循环里按当前目标取参数，没有条目时回退到公共 `Params`
（保持对 `/v1/events` 与普通边的兼容）。

**两处校验，同一个函数**（`route.ValidateParams`）：

| 时机 | 目的 |
|---|---|
| 建源 / 改源 | 用户填完表单立刻知道填错了，不用等到触发 |
| 触发派发 | 兜底（数据可能被手改过） |

两边口径必须一致——否则会出现「建源说没问题、触发被剪枝」这种最让人困惑的情况。

**cron 语义**：

- 五段表达式（分 时 日 月 周），支持 `*` `/` `,` `-`；不支持 `@daily` 等宏
- `dom` 与 `dow` 都受限时是 **OR**（标准 crontab 语义）
- 幂等键 `cron:<sourceID>:<计划时刻>`：进程重启不会重复触发同一时刻
- 表达式永不成立（如 2 月 30 日）时返回零值，调度器记日志并跳过

## 6. 协议

### 6.1 WebSocket 帧（节点 ↔ 中枢）

**传输**：`GET /v1/node/stream`，`Authorization: Bearer <node_token>`，事前鉴权再 Upgrade。

```jsonc
// ── 节点 → 中枢 ──────────────────────────────
{ "type": "hello",     "node_id": "nd_x", "protocol": 1,
  "capabilities": [ {"capability":"review","version":1},
                    {"capability":"test","version":1} ],
  // manifest 可选：声明「这个能力我怎么跑」。只存不执行（I1），中枢永不执行 command。
  // 与 capabilities 同时出现时以 manifest 为准；manifest 没写的项 run_spec 落 '{}'。
  "manifest": [ {"capability":"review","version":1,
                  "run":{"runner":"bash","command":"./runners/review.sh",
                         "timeoutSec":900,"concurrency":2,"env":{"LOG_LEVEL":"info"}}} ] }

{ "type": "heartbeat", "ts": 1730000000 }

{ "type": "ack",       "task_id": "tsk_x" }

{ "type": "progress",  "task_id": "tsk_x", "note": "step 2/5" }

{ "type": "result",    "task_id": "tsk_x",
  "status": "succeeded",           // succeeded | failed
  "out": "fix",                    // 出口名 或 null
  "summary": "发现 2 处类型错误",
  "artifacts": ["tsk_8f21a3c0"],
  "error": "" }

// ── 中枢 → 节点 ──────────────────────────────
{ "type": "welcome",   "node_id": "nd_x", "protocol": 1,
  "heartbeat_sec": 10, "lease_ttl_sec": 30 }

{ "type": "assign",    "task_id": "tsk_x",
  "capability": "review", "version": 1,
  "params": { "diff": "..." },
  "lease_until": 1730000030,
  "max_duration_sec": 3600 }

{ "type": "cancel",    "task_id": "tsk_x", "reason": "superseded" }

{ "type": "error",     "code": "unknown_capability", "message": "..." }
```

**`assign` 载荷是中枢能表达的全部**：`{capability, version, params}` + 租约。**没有 argv**（I1）。

**错误码**（`error.code`）：

| code | 何时 |
|---|---|
| `unknown_capability` | 节点声明了中枢没有的 `name@version` |
| `invalid_manifest` | 节点上报的能力 manifest 不合法（run 缺失/runner 不支持/command 为空/concurrency 越界） |
| `bad_frame` | JSON 解析失败 |
| `unknown_type` | 未知帧类型 |
| `invalid_transition` | 状态机拒绝 |
| `lease_expired` | 收到 `ack`/`result` 时租约已失效 |

### 6.2 HTTP API

沿用现有 `/v1/*` 风格。本期新增：

| Method | Path | 说明 |
|---|---|---|
| `POST` | `/v1/events` | 外部事件入口（webhook 通用） |
| `GET` | `/v1/events` | 事件列表（按 trace） |
| `GET` | `/v1/tasks` | 任务列表（按 trace/node/status） |
| `POST` | `/v1/tasks/{id}/cancel` | 取消任务 |
| `GET` | `/v1/capabilities` | 能力定义列表 |
| `POST` | `/v1/capabilities` | 定义新能力（自动算 hash、开版本） |
| `GET/POST/DELETE` | `/v1/edges` | 出口表 CRUD |
| `GET` | `/v1/nodes` | 节点列表与在线状态 |
| `POST` | `/v1/nodes/{id}/drain` | 排空节点 |
| `GET`/`POST` | `/v1/namespaces` | 列出 / 新建命名空间（新建时**同事务播种内置能力**） |
| `PATCH`/`DELETE` | `/v1/namespaces/{id}` | 改名（只改 `name`，`slug` 不可改）/ 删除（阻塞 `409`、最后一个 `400`、`?force=true` 级联） |

**JSON 字段一律 camelCase**（I9 同源约束的延伸）。

#### 命名空间参数怎么带

**显式，或者显式全部 —— 永远没有隐式默认。**

| 端点 | `namespaceId` |
|---|---|
| `/v1/capabilities`、`/v1/nodes`、`/v1/nodes/{id}`、`/v1/edges`、`/v1/sources`、`/v1/events`、`/v1/tasks`、`/v1/traces/*`、`/v1/stats` | **必填** |
| 同上但要跨命名空间汇总（管理员视图） | `namespaceId=*` |
| `/v1/orgs`、`/v1/admin/*`、`/v1/auth/*` | **不需要**（组织级） |

- 参数位置：`GET` 用 query `namespaceId`；`POST`/`PATCH`/`DELETE` 可放 query 或 body 的
  `namespaceId`（**query 优先**；例外：机器入口 `POST /v1/events` 是 **body 优先**，
  只读 query 的写端点是 `POST /v1/tasks/{id}/cancel`、`POST /v1/nodes/{id}/drain`、
  `DELETE /v1/sources/{id}`）
- 缺 `namespaceId` → `400`（**不回退到默认命名空间**：回退等于把人写错的地方藏起来）
- `namespaceId=*` → 跨命名空间汇总，**只允许读端点**；写端点 / 单对象端点带 `*` → `400`；
  `/v1/stats` 的汇总口径未定义，它的 `*` 同样 `400`
- 不存在 → `404`；属于别的 org → `403`；跨命名空间引用 → `400`（I11）
- 所有资源在响应里带 `namespaceId`；`/v1/namespaces` 自身不带
- **节点归属**：机器属于哪个命名空间由**节点令牌**决定（令牌 → 节点行 →
  `nodes.namespace_id`，握手时按 token 解析，见 §10.3），创建后不可改
  （要换就删了重建）。节点协议帧里不传 `namespaceId`，也不存在「注册节点」的 HTTP 端点。

---

## 7. 错误码与剪枝原因

| 码 | 层 | 含义 |
|---|---|---|
| `unknown_capability` | 注册/派发 | 能力名+版本在中枢无定义 |
| `no_edge` | 路由 | `edges` 查表 0 行 |
| `param_schema` | 路由 | 参数不过目标能力 `params_schema` |
| `path` | 路由 | 目标能力已在 `path` 里（环剪枝） |
| `no_node` | 路由 | 没有节点声明该能力 |
| `foreign_user` | 路由 | `params.to` 指向的用户不属于本组织 |
| `invalid_transition` | 状态机 | 非法状态迁移 |

**「没触发」的全部原因**，分两类：

| # | 原因 | 是剪枝吗 | 说明 |
|---|---|---|---|
| ① | 上游没回报这个出口 | **不是** | `reported=0` 或 `out` 指向别的出口——正常结束的分支，压根没查表 |
| ② | `no_edge` | 是 | 查表 0 行 |
| ③ | `param_schema` | 是 | 参数不过目标能力 schema |
| ④ | `no_node` | 是 | 有边、参数也对，但没有节点声明该能力 |
| ⑤ | `foreign_user` | 是 | `params.to` 指向的用户不属于本组织 |
| ⑥ | `path` | 是 | 目标能力已在路径里（环剪枝） |

**②~⑥ 五种记 `pruned` + `reason`；① 不是剪枝。**
剪枝是静默的（不打断业务），但**一律留痕**——trace 上能看到 `reason` 和随附证据。

---

## 8. 与现有代码的对照（重构清单）

| 现有 | 处置 |
|---|---|
| `internal/store/schema.sql` 10 张表 | **删**，按本文重建 13 张 |
| `internal/broker`（Publish/Subscribe/Ack/Replay） | **改**：`Message` → `Event`/`Task`；`Ack` → 任务 `ack` |
| `internal/ws`（notification/ack 帧） | **改**：换成 §6.1 的节点协议 |
| `internal/route`（`ScopeMatch`/`TagMatch`） | **删**：路由改为查 `edges` |
| `internal/push` | **保留**：`notify.phone` 变成一个 `end` 能力 |
| `internal/auth` | **改**：加 `org_id` 维度 |
| `internal/api/notify.go` | **改**：`POST /v1/notify` → 产 `events` 行 |
| `internal/server/mux.go` | **改**：按 §6.2 重新注册 |
| `api/openapi.yaml` | **重写**：现有 50 个 operation 收缩到本模型 |

---

## 9. 验收标准（契约是否被实现）

| # | 断言 | 怎么测 |
|---|---|---|
| A1 | 注册即得组织 | 新用户注册后 `org_members` 有 1 行 role=owner |
| A2 | 无 argv 可存 | `grep -ri 'argv\|shell\|script' internal/store/schema.sql` = 0 |
| A3 | 能力必须先定义 | 节点声明未定义能力 → 注册被拒，错误码 `unknown_capability` |
| A4 | `kind` 不落库 | `grep -E '^\s*kind\s' internal/store/schema.sql` = 0（注意别被 `capability` 误命中） |
| A5 | 出口封闭 | `edges.from_port` 不在 `outputs` 里 → 配置期拒绝 |
| A6 | 扇出 | 一个 `(cap, port)` 有 3 行边 → 建 3 个任务 |
| A7 | 成环剪枝 | `review→fix→review` 跑两圈后 `pruned/path` |
| A8 | `out` 三态 | 未回报 / 回报 NULL / 回报值 三者行为不同 |
| A9 | 租约接管 | `kill -9` 节点后 30s 内任务回 `pending` |
| A10 | 组织隔离 | A 组织的 token 查不到 B 组织的事件 |
| A11 | 版本并存 | `review@1` 和 `review@2` 同时存在，互不影响 |
| A12 | 空列表 `[]` | 所有 list 接口返回 `[]` 不是 `null` |

---

## 10. 已决项

| # | 决定 | 落地 |
|---|---|---|
| **Q1** | **推送目标是用户，不是组织** | `devices` 表**不加** `org_id`——手机是你的，组织删了手机还在。`notify.phone@1` 的 `params_schema` 要求 `{to: <user_id>, title, body}` |
| **Q2** | **`trace_id` 由触发器生成**，事件诞生时就带上 | 触发器（webhook/cron/watch）产事件时生成 `trace_id` 并写进 `events`。中枢不代生成、不猜 |
| **Q5** | **实例由节点申报**，不是中枢预创建 | 节点 `hello` 帧里带 `instances: [{slot, concurrency}]`；中枢 upsert `node_instances`。超 `nodes.max_instances` 拒绝注册 |
| **Q6** | **要幂等**：同一 webhook 重投不能重复建链 | 见 §10.1 |

### 10.1 事件幂等（Q6）

幂等键由**触发器**给，中枢只做唯一约束：

```sql
ALTER TABLE events ADD COLUMN idem_key TEXT;
CREATE UNIQUE INDEX idx_events_idem ON events(org_id, source, idem_key)
    WHERE idem_key IS NOT NULL;
```

- 触发器产事件时带 `idem_key`（GitHub 用 `X-GitHub-Delivery`，自己写的 cron 用 `cron:<name>:<fire_time>`）。
- **不带 `idem_key` 就允许重复**——不是所有来源都有稳定的投递 ID，硬造一个反而制造假幂等。
- 重投时 `INSERT ... ON CONFLICT DO NOTHING`；命中冲突返回**已有的 `event_id` 和 `trace_id`**，不是 409。
  理由：对 webhook 发送方而言「我已经收过了」和「我这次收下了」的正确行为都是 200，重试不该被当成错误。

### 10.1.1 推送的跨组织校验（Q1 的必然后果）

`devices` 属于用户、不属于组织，所以**必须在派发时补一道校验**，否则 A 组织能推给 B 组织的用户：

```sql
-- notify.phone 派发前
SELECT 1 FROM org_members WHERE org_id = :current_org AND user_id = :params_to;
-- 0 行 → pruned / reason=foreign_user，不建任务
```

这是 `params_schema` 管不到的部分：schema 只能校验「`to` 是个非空字符串」，
**「这个人属于本组织」是业务规则，要单独查**。同类规则以后每加一个跨组织能力都要补一次。

### 10.2 Q3：`graph_versions` 的写入时机

**两种做法，代价不同：**

| | 每次改边就落一版 | 派发时按需落版 |
|:--|:--|:--|
| 触发 | `POST/DELETE /v1/edges` 成功后立即写 | 事务内发现当前图 hash 不存在才写 |
| 写放大 | 改 10 次边 = 10 行（哪怕中途改错又改回来） | 只有「被派发用到过」的图才会留版 |
| 未派发的图 | 也留版 | 不留（省空间，但事后查不到「当时配过什么」） |
| 实现复杂度 | 低 | 中（事务内查 hash 再条件插入） |

**决定：按需落版（后者）。** 理由：`graph_versions` 的唯一用途是**解释已经发生的 trace**，
从没被任何一次派发用过的图，事后没人会去查它。按需落版既不丢可解释性，又不会因为反复试错配置而堆垃圾。
写的时候用 `INSERT ... ON CONFLICT(org_id, content_hash) DO NOTHING` 后回查，保证同一份图只有一行。

### 10.3 Q4：节点 token 形态

**两种做法，代价不同：**

| | JWT（自包含） | Opaque token + 查表 |
|:--|:--|:--|
| 验签 | 本地算签名，不碰 DB | 每次查 `nodes.token_hash` |
| 吊销 | **做不到立即失效**，只能靠短 TTL + 刷新 | 改一行立刻生效 |
| `org_id` 携带 | 写进 claim | 查表得到 |
| 泄露后的处置 | 等 TTL 过期 | 立即吊销 |
| 实现 | 需要签名/验证 + 刷新逻辑 | 一个索引查询 |

**决定：opaque token + 查表。** 理由：
1. **节点要有「立即踢掉」的能力**——一台机器丢了，改一行就该断，不该等 TTL。
2. WS 连接是长连接，JWT 的短 TTL 在这里反而是负担（要么频繁刷新，要么长 TTL 失去意义）。
3. 验签成本可以忽略：`hello` 时查一次，之后靠连接对象持有 `node_id`/`org_id`，不是每帧都查。

token 形态与现有 `api_keys` 一致：`ant_<label>_<base64url(24B)>`，DB 只存 `sha256`，明文只在创建时返回一次。

### 10.4 Q7：命名空间隔离到什么粒度

**决定：全部隔离。能力、节点、源、边、事件、任务、图快照都属于命名空间；
只有身份（`users` / `org_members`）和密钥（`api_keys`，可选绑定命名空间）属于组织。**

| 候选 | 结论 |
|---|---|
| 只有 org 一层 | ❌ 多套流程无处安放；在预发配一条边，生产也会派发 |
| 只隔离配置与运行面（能力、节点保持组织级） | ❌ 两个团队不能各有 `lint@1`；无法把某套流程的活固定到某批机器 |
| **全部隔离** | ✅ 环境之间真正互不可见；能力名、机器池都按环境切 |

**代价与解法**：能力按命名空间划分后，节点 `hello` 里的 `review@1` 会有歧义。
**解法是把命名空间放进节点令牌，而不是改协议帧** —— 令牌 → 节点行 → `namespace_id`，
`hello` 一个字不用改，已部署的 agent 只需换令牌。
一台机器要服务两个命名空间就注册两次（两个节点行、两个令牌）。
完整推导见 `docs/design/namespaces.md` §2.1。

**两条容易漏的配套**：

1. **内置能力按命名空间播种**：新建命名空间时同事务插入（否则新命名空间里连
   `notify.phone` 都没有），内置能力升版时遍历所有命名空间各插一行。
2. **删命名空间 = 拆掉一个环境**：级联删掉节点 → 里面的机器全部掉线（令牌失效），
   UI 必须把「N 台机器将失去连接」写进确认框。

---

**本文冻结日期**：2026-10-24
**冻结后变更流程**：改本文 → 更新版本号 → 所有实现按新版本调整。不改本文就改代码视为违规。
