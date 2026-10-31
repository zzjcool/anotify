# Anotify 实现契约 v1 · 冻结

> **状态：冻结。** 这是重构的唯一事实源。任何与它冲突的代码都是待改的。
> 设计依据：`design/wiki/`（10 页）。本文只写**实现层**：表、字段、状态机、协议帧、不变量。
>
> **无历史包袱**：库清空重建，不迁移、不双写、不兼容。
> 现有 `internal/store` 的 10 张表全部作废，按本文重建。
>
> **⚠️ 修订（#204 / 迁移 14）：组织层已删除。** 命名空间成为**唯一隔离边界**，
> `orgs` / `org_members` 两张表与所有 `org_id` 列都已移除，`orgId` / `X-Org-Id`
> 不再是任何端点的参数。本文中带 `org_id` 的 DDL 段落保留为**历史记录**
> （它们是迁移前的形状，迁移 14 的等价物见 `internal/store/migrate14.go`）；
> **规范性内容以 §0 不变量、§2 端点表、§6 命名空间章节为准**，这三处已按新语义改写。
> 数据隔离维度：组织 → 命名空间 两级 ⇒ **命名空间一级**；
> 「谁能看某个命名空间」：该组织成员 ⇒ **本部署任何已登录用户**；
> API key 作用范围：个人 key 全组织 ⇒ **个人 key 全部署**。

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
| **I10** | **命名空间是唯一隔离边界**：命名空间资源端点必须显式给 `namespaceId`，并在处理器内过 `store.AuthorizeNamespace` 收口——**今天它等价于「命名空间存在且调用方已登录」**；缺参 `400`、不存在 `404`、未登录 `401`；**跨命名空间引用一律 `400`**（见 I11）。本部署内不再有「别人的命名空间」这一态 | 跨命名空间泄漏 / 越权访问 |
| **I11** | **跨命名空间不引用**：`edges` 两端、`source_targets` 目标、`node_capabilities` 声明、子任务继承，全部锁在同一个 `namespace_id` 内；违反一律 `400` | 预发的图能把任务派到生产的拓扑上 |

---

## 1. 表清单

**13 张新建 + 4 张沿用**，全部 `CREATE TABLE IF NOT EXISTS`。

| 表 | 归属 | 说明 |
|---|---|---|
| ~~`orgs`~~ | — | ~~组织（Tailscale tailnet 模型）~~ **迁移 14 已删除** |
| ~~`org_members`~~ | — | ~~用户 ↔ 组织（多对多）~~ **迁移 14 已删除** |
| `namespaces` | **隔离边界（唯一一层）** | 隔离单位（配置 + 运行面），见 §2.1.1 |
| `users` | 人 | 全部署共享身份 |
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
| `playbooks` | 命名空间 | 预案清单（安装时批量写能力/边/源），见 §2.8 |

### 1.1 本文定义的表（13 张）

`namespaces` / `capability_versions` / `nodes` /
`node_capabilities` / `node_instances` / `node_pins` / `edges` / `events` / `tasks` /
`graph_versions` / `playbooks`

### 1.2 沿用不动的表（4 张）

`users` / `passkeys` / `sessions` / `api_keys` —— 字段和现有 `schema.sql` 一致，
本文不重写 DDL。**迁移 14 之后**：组织层已删除，这四张表本来就不带 `org_id`
（用户直接享有「本部署内访问全部命名空间」的权限，见 I10 新表述）。

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

**首次注册自动建 `default` 命名空间**（迁移 14 之后）：注册事务内建用户 +
建 `default` 命名空间 + 播种内置能力（`store.RegisterUser`，同一事务）。
原文的 `INSERT orgs` + `INSERT org_members(role='owner')` 随组织层一起删除。
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
**不隔离什么**：只有身份与密钥 —— `users`，以及 `api_keys`
（密钥可额外绑定一个命名空间来限制作用域，见设计文档 §2.3；`org_members` 已随组织层删除）。

**能力隔离后，节点协议不需要改**：命名空间放在**节点令牌**里，不在 `hello` 帧里。
节点令牌 → 节点行 → `namespace_id`，所以 `hello` 里的 `capabilities: ["review@1"]`
只在**该节点所在命名空间**里解析。完整理由见设计文档 §2.1。

**内置能力必须按命名空间播种**：新建命名空间时同事务插内置能力（否则新命名空间里
连 `notify.phone` 都没有），内置能力升版时遍历所有命名空间各插一行。
**首次注册时同事务建 `default` 命名空间**（slug `default`，名「默认」）——
迁移 14 之后注册走 `store.RegisterUser`，建用户 + 建 `default` 命名空间 + 播种
在**同一个事务**里完成，不再留「用户建好了但命名空间没建」的半成品。
**代码里不存在「没有命名空间」的分支。**

**上面的 DDL 是迁移 14 之前的形状**（`org_id` + `UNIQUE(org_id, slug)`）。
迁移 14 之后：`namespaces` 去掉 `org_id`，唯一键改为 **`UNIQUE(slug)`**
（slug 在全部署唯一；老库里跨组织重复的 slug 按 `default` / `default-2` … 去重），
`orgs` / `org_members` 两表删除。等价 DDL 见 `internal/store/migrate14.go`。

**隔离维度只有一层**：`namespace_id`（I10 原文要求「`namespace.org_id == org_id`」
的那条一致性校验随组织层一起消失——已经没有第二层可比对了）。

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

### 2.8 预案（playbooks）

```sql
-- +migration 13 playbooks
CREATE TABLE IF NOT EXISTS playbooks (
    id           TEXT PRIMARY KEY,              -- pb_<base62>
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug         TEXT NOT NULL,                 -- pr-review（URL 用，命名空间内唯一）
    name         TEXT NOT NULL,                 -- PR 审查
    descr        TEXT NOT NULL DEFAULT '',      -- 说明文字（缩写列名，原因见下）
    manifest     TEXT NOT NULL,                 -- manifest JSON 原文
    created_at   INTEGER NOT NULL,
    UNIQUE (namespace_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_playbooks_ns ON playbooks(namespace_id, created_at);
```

**列名是 `descr` 而不是 `description`**：A2 的禁词正则是**子串**匹配
（`argv|shell|script`），而 `description` 这个单词里恰好含有被禁子串 —— 写进
`schema.sql` 会让 `make contract` 失败。列名只活在 store 层，**API/JSON 字段仍是
`description`**（camelCase，见 `api/openapi.yaml`）。

五条口径：

1. **`manifest` 存用户提交的 JSON 原文**：只做校验（能力名/版本、边的 `fromPort`
   属于上游 `outputs`、触发 `kind` 合法），**不重新编码、不解释**。`runner` /
   `paramsSchema` / 自定义键原样往返（I1：中枢不知道「怎么执行」，runner 属于节点侧
   配置；重新编码等于偷偷改用户的配置）。缺省归一化成
   `{"capabilities":[],"edges":[],"triggers":[]}`（I9：空是 `[]`）。
2. **就绪体检是查询时算的推导量，不落库**（I3 同源）：能力定义在不在 / 有没有节点
   声明 / `edges` 里有没有这条边 / 有没有命中**入口能力**的源。单项状态是四态
   `ok|missing|no-node|offline`（`no-node` 与 `offline` **必须分开**：前者要去装机器，
   后者只需把机器连上），总体状态 `ready|blocked|needs-node|partial`。
3. **安装是单事务，顺序能力 → 边 → 源**：边的两端要有定义才建得了，源要有人可派；
   任何一步出错整体回滚，不留半套配置。单条边两端缺定义 / `fromPort` 不在上游
   `outputs` 里 / 同内容能力冲突，都**只 skip 那一条**（原因写进 `notes`），不整体失败。
4. **删预案不删已装物**：预案是清单，不是所有权。`DELETE /v1/playbooks/{idOrSlug}`
   只删这一行，已安装的能力定义 / 边 / 源原样保留（要拆就明确去删那些东西——
   两种语义混进一个删除动作里，迟早误删生产配置）。
5. **I8 append-only / I11 只碰本命名空间**：安装时已存在的 `(name, version)` 一律
   **跳过、绝不覆盖**（同内容冲突也只 skip，不产生第二行同内容定义）；写入的能力定义、
   边、源全部锁在**本次请求命中的那一个命名空间**内（跨命名空间引用 → `400`）。
   预案也**不提供跨命名空间汇总**：体检结论只能按单个命名空间算（见 §6.2）。

端点、状态码与字段形状见 §6.2 与 `api/openapi.yaml`（后者与 `web/mock.js` 的
`pbChecks` / `pbView` 逐字段一致，是验收基准）。

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
输入：namespace_id, 上游 (capability_id, out), params, trace_id, path, parent_task_id

① 查表
   SELECT to_capability FROM edges
    WHERE namespace_id=? AND from_capability=? AND from_port=?
   → N 行（N=0 则记 pruned/no_edge，结束）

② 校参数（对每个 to_capability）
   读 to 的 params_schema
   源首次派发：先把参数模板里的 {{路径}} 单趟取值（§5.5）并按 schema 转类型
                取不到 → pruned/param_missing；转不了 → pruned/param_type（都不建任务）
   再用解析后的值校验 params_schema
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

**两处校验，同一份 schema**（触发期 `route.ValidateParams` / 配置期 `route.ValidateLiteralParams`）：

| 时机 | 用哪个函数 | 目的 |
|---|---|---|
| 建源 / 改源 | `route.ValidateLiteralParams` | 用户填完表单立刻知道填错了，不用等到触发 |
| 触发派发 | `route.ValidateParams` | 兜底（数据可能被手改过），此时所有值都已求值 |

两边口径必须一致——否则会出现「建源说没问题、触发被剪枝」这种最让人困惑的情况。

**配置期只跑它能判定的部分**：自 #114 起，配置期**不再**「剔掉占位符后拿子集跑 `ValidateParams`」
（那会因为丢了键信息，把 `{"to":"{{a}}","title":"hi"}` 误拒成 `missing required field "to"`），
改用 `route.ValidateLiteralParams(schema, template)`——**同一份 `params_schema`**，四条口径：

1. **顶层 `required` 按模板键集合判定**：键**写了**就算满足，**不看它的值**是否已经求得出
   （值是占位符 / 映射声明都算满足）。只判**顶层**，嵌套对象的 `required` 跟着第 2 条走。
2. **逐字段**校验：**完全解析得出**的字面量用**派发期同一个**校验器（`validateValue`）判，
   路径是 `params.<key>`——`{"n":"abc"}` 对 `integer` 这种当场能判的错，配置期就报。
3. 含**未解析**内容（完整占位符 / 映射声明 `{from}`·`{var}`，含**嵌套**）的子树**整体跳过**，
   留派发期的 `ValidateParams` 兜底（那时才知道类型）。
4. 对象级约束（`minProperties` / `maxProperties`）配置期**不判**——模板的键集合不等于求值后的
   键集合，判它只会误伤；`additionalProperties: false` 仍判（键名本来就不依赖求值）。

例：schema `required: [to, title]` + `{"to": "{{pull_request.user.login}}", "title": "hi"}`
**必须能保存**：`to` 的键在 → `required` 满足；`to` 的值含占位符 → 跳过类型校验；
`title` 是字面量 → 照常逐字段校验。它到派发期才按真实取值判类型（或按 `param_missing` 剪枝）。

**cron 语义**：

- 五段表达式（分 时 日 月 周），支持 `*` `/` `,` `-`；不支持 `@daily` 等宏
- `dom` 与 `dow` 都受限时是 **OR**（标准 crontab 语义）
- 幂等键 `cron:<sourceID>:<计划时刻>`：进程重启不会重复触发同一时刻
- 表达式永不成立（如 2 月 30 日）时返回零值，调度器记日志并跳过

### 5.5 源变量与参数引用（单趟替换，不做模板引擎）

源的每个出口能力带自己的参数（§5.4）。参数的**每个值**可以是字面量，也可以是**引用**：

| 形态 | 写法 | 语义 |
|---|---|---|
| 字面量 | `{"env": "prod"}`、`{"lines": 42}`、`{"flag": true}` | 原样传，**不做任何转换** |
| 整值引用 | `{"lines": "{{pull_request.additions}}"}` | 单趟取到一个值，**保留原类型**（数字还是数字） |
| 混写 | `{"title": "PR #{{pull_request.number}}: {{pull_request.title}}"}` | 逐处替换后拼成一段文本，结果**一律字符串** |
| 映射声明（旧形态） | `{"title": {"from": "pull_request.title", "default": "(无标题)", "as": "string"}}` | 恰好含 `from` 或 `var` 之一（`default` / `as` 可选） |
| 别名引用（旧形态） | `{"title": {"var": "author"}}` | 只查别名表，**不**回退 payload 路径 |

**映射声明的判定**：对象里**恰好**含 `from`（payload 点号路径）或 `var`（别名）**之一** +
可选的 `default` / `as`；`from` 与 `var` **互斥**（同时出现 = 配置期 `400`）。
**多一个键就按字面量对象处理**——用户真想传 `{"from": 1}` 这种数据时不该被误判成映射，
行为可预期比「聪明」重要。

**配置期的判定顺序**（A15，顺序即口径，配置期与派发期必须用**同一个** `IsMapping`）：

1. 先看 `from` 与 `var` 是否**并存** → 并存就是互斥错 `400`（与它整体算不算映射无关）；
2. 再用 `IsMapping` 判它是不是映射声明；
3. **不是映射声明就按字面量对象放过**——配置期也不拦。派发期同样当字面量原样传，
   不会出现「配置期拒了、派发期明明能传」的口径分裂（A15）。

所以 `{"from": 1}`（`from` 的值不是字符串）、`{"from":"a","x":2}`（多一个键）**都能存**，
它们只是字面量对象。配置期对**对象形态**只有三种会 `400`：

| 输入 | 结果 |
|---|---|
| `{"from":"a","var":"b"}`（`from` + `var` 并存） | `400`：互斥（只能选一个） |
| `{"var":"titel"}`（`{var}` 引用的别名**没声明**） | `400`：消息含变量名（拼错必须当场发现） |
| `{"default": 1, "as": "string"}`（写全了可选键，却没有 `from`/`var`） | `400`：提示缺 `from` 或 `var` |

> 第三种为什么要报：一个对象只由**映射声明的可选键**组成、却没有主体，几乎一定是漏写了；
> 按字面量放过会得到一个永远传不进 `from` 的对象，用户还以为映射生效了。
> 反过来，只要对象里出现**任何**非 `default`/`as` 的键，它就是用户数据的字面量对象，一个都不拦。

#### 解析顺序（别名优先，`{var}` 不回落）

1. **先查别名表** `config.vars`（`[{name, from}]`）：`{{name}}` 等价于它指向的那个路径；
   名字冲突时**别名优先**——别名本来就是为了改写 / 缩短而存在的。
2. **未命中再当 payload 点号路径**（`pull_request.user.login`）。`{var:"name"}` 是**显式**
   别名引用，**只查别名表**，不回退 payload 路径。
3. **路径语法**只有两条：点号分段 + 数组十进制下标（`items.0.name`）。只查 map 自有键；
   下标必须在界内且是纯十进制正整数——`"0.0"` / `"-1"` / `"1e2"` 一律 miss。**取不到不抛错**
   （payload 来自外部，形状不受控），交给「未命中」分支处理。
4. **`{{$name}}` 已不再识别**：占位符正则以 `[A-Za-z_]` 起始、**不含 `$`**，所以 `{{$x}}`
   匹配不到任何完整占位符，而会落进闭合检查的「数量相等但残留」一档 → **配置期 `400`**
   （「占位符不合法（只支持 {{点号路径}}：不允许运算符、括号、函数或 $）」，见下节）。
   老配置请改成 `{{name}}`（同名键的别名语义不变），新的参数引用一律不写 `$`（设计文档 D.4）。
   存量配置里已经存下的 `{{$name}}` 在**派发期**按字面量原样透传（不改历史行为）。
5. **取值来源 = 事件 payload**；cron / manual 用 `config.payload` **打底**，**用户字段覆盖同名键**
   （包括 cron 的系统字段 `source` / `kind` / `firedAt`：用户显式写的配置不该被静默盖掉，设计 §3.3）。
   **webhook / github 不合并 `config.payload`**：投递体本身就是 payload（历史行里残留的 `payload` 键
   不被复活，A13）。

> `{{$x}}` 被拒与旧的 `$` 剥除兼容**不矛盾**：`{"from": "$name"}` 这种对象形态的 `from` 路径
> 仍会把前导 `$` 剥掉再查（老文档把变量写成 `$name`），那是**映射声明的路径写法**；
> `{{$x}}` 是**占位符语法**，语法层就不认 `$`。

#### I6 边界（判定权威）

**占位符正则（必须带捕获组）**：

```text
\{\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}\}
```

→ 语法上**就没有运算符 / 括号 / 函数 / 条件的位置**。这是**结构性**保证，不靠运行时检查。

| 允许 | 禁止 |
|---|---|
| `{{path.to.value}}` 单趟取值（`strings.Replace` 语义） | 对**替换结果**再扫描（递归展开 = 模板引擎） |
| 整值引用保留原类型（number / boolean / object 原样） | 条件 `{{#if x}}`、循环 `{{#each xs}}` |
| 混写 → 结果一律字符串（不再解析） | 函数 `{{f(x)}}`、运算 `{{a + b}}` |
| 对象形态 `{from}` / `{var}`（向后兼容） | 过滤器、正则 / JSONPath、默认值表达式 |
| 字面量原样传 | 任何形式的用户逻辑求值 |

**判定「是不是递归展开」的唯一问题：替换结果还会被再扫描一次吗？**

> payload 是 `{"a": "{{b}}", "b": "B"}` 时，解 `"{{a}}"` 得到的是 **`"{{b}}"`**，
> **不是** `"B"`；混写 `"x={{a}}"` 得到 `"x={{b}}"`。中枢只替换**一趟**，
> `{{b}}` 是被搬运的**数据**，不是待执行的模板。

**半截占位符必须在配置期挡住**：`"PR {{title"`、`"{{#if x}}"`、`"{{x + 1}}"`、`"{{f(x)}}"`
都不能「因为正则匹配不到就当字面量存下来」——那样用户会以为条件 / 运算生效了。
所以校验顺序是**先做闭合检查**，**闭合检查先于「是否含占位符」判断**，再做后面的形式校验。
闭合检查分三档，**两类错误消息必须分开**（原因不同，混成一句话就查不下去了）：

| 输入形态 | 判定 | 结果 |
|---|---|---|
| 只含 `}}`、**不含** `{{`（如 `{"a":{"b":1}}` 当普通字符串参数） | 就是字面量 | **放行**（能保存） |
| `{{` 与 `}}` 数量**不等**且含 `{{`（`"PR {{title"`、`"{{a}}{{b}} + 1{{"`） | 占位符没有闭合 | `400`：`参数 %s：占位符没有闭合（{{ 与 }} 数量不一致）` |
| 数量**相等**但仍有残留（`{{x + 1}}`、`{{f(x)}}`、`{{#if x}}`、`{{#each xs}}`、`{{.a}}`、`{{$x}}`） | 占位符不合法 | `400`：`参数 %s：占位符不合法（只支持 {{点号路径}}：不允许运算符、括号、函数或 $）` |

第三档的判定方式：把所有**完整占位符**（能匹配上面那条正则的）从原文里抹掉，剩下的文本里
若还含 `{{` 或 `}}`，说明那段「看起来像占位符的东西」其实不是合法占位符。
第二档只看数量、不看内容，所以 `"PR {{title"` 这种半截写法也逃不掉。

#### 类型

| 情况 | 依据 |
|---|---|
| 整值引用 `"{{x}}"` | **保留原类型**，再按目标 `params_schema` 的类型转换（`"42"` + `integer` → `42`） |
| 混写 `"PR #{{n}}"` | 结果必字符串，按 `params_schema` 转换；转不了 → `param_type` |
| 对象形态 `{from}` / `{var}` | `as` 优先，否则 `params_schema` |
| 纯字面量 | **不转换**，尊重用户写的（类型不对由 `ValidateParams` 报 `param_schema`） |

- 目标类型取值集：`string` / `number` / `integer` / `boolean` / `array` / `object`；
- 旧形态的 `as` 只允许 `string` / `number` / `boolean` / `json`（其余配置期 `400`）；
- 转换口径与 `web/mock.js` 的 `coerceValue` **结果一致、但拦截阶段与 reason 不同**：
  两边都接受 `boolean` 的 `true` / `false` / `"true"` / `"false"` / `1` / `0`，也都不接受 NaN / Inf；
  差别在 `integer`：mock 的 `coerceValue` 对 `"4.5"` **不拒**（它只做数字化，给出 `4.5`），
  靠它自己的 `validateParams` 以 `expected integer`（`param_schema` 口径）挡下；
  后端在 `CoerceValue` 里**直接拒**（返回 `(nil,false)`）→ `param_type`。
  **两边都不会把非法值派出去**，差别只在 reason / 拦截阶段（`param_schema` vs `param_type`）；
  拿 mock 当参考实现时注意这一点；
- **转不了就是失败**，绝不静默退回原值——退回会让用户看到「expected integer, got string」，
  却不知道是 payload 里那个字段本身不是数字。

#### 未命中与类型错误（一律可观测）

| 情况 | 行为 |
|---|---|
| 命中 | 用取到的值 |
| 未命中 + 有 `default`（只有对象形态有） | 用 `default` |
| 未命中 + 无 `default` | 该目标**整条剪枝**，`reason=param_missing` |
| 类型转不了 | 该目标**整条剪枝**，`reason=param_type` |

**`null` 一律算未命中**（A16）：不论 `nil` 来自别名表（`config.vars` 里 `{name,value}` 的常量
声明成 `null`，或 `from` 路径指向的字段值是 `null`），还是 payload 里的**显式 `null`**，
都按「取不到」处理——对象形态有 `default` 就用 `default`，没有就 `param_missing`。
**混写里不得把未命中静默拼成空串**：`"PR #{{n}}"` 在 `n` 为 `null` 时不会产出 `"PR #"`，
而是整条目标按 `param_missing` 剪枝（拼成空串等于伪造了一份数据，且用户永远查不到原因）。
`{var}` 与 `{{x}}` 在这一条上**完全一致**（§8.1 A7：两边都不把 `nil` 当「取到了 null」）。

**为什么整条剪枝而不是跳过这个字段**：跳过会让 `param_schema` 报「x is required」，
用户看着配置里明明写了 x 却被说必填——真实原因（payload 里没这个字段）完全没体现。

`detail` 必须能看见**用户写的原文**，并按引用来源区分（三条 miss 用 `；` 连接）。

形态是「**能力前缀 `能力名@版本 ` + 下面这段正文**」：正文**逐字**冻结，测试**按子串与等值各钉一条**
（子串钉正文，等值钉前缀——扇出时只有前缀能回答「是哪个目标的参数出了问题」，
如 `review@1 参数 title 取不到 {{pull_request.title}}（payload 里没有这个字段）`）：

| 来源 | 正文（逐字） |
|---|---|
| `{{x}}` | `参数 title 取不到 {{pull_request.title}}（payload 里没有这个字段）` |
| `{from}` | `参数 title 取不到 payload.pull_request.title（payload 里没有这个字段）` |
| `{var}` | `参数 title 取不到变量 $title（源未声明该变量，或它指向的字段在 payload 里没有）` |
| 类型 | `参数 lines 期望 integer，但求值得到 "abc"，转不了` |

#### 配置期 vs 触发期

| 时机 | 做什么 | 不做什么 |
|---|---|---|
| 建源 / 改源（`400`） | ① `config.vars` 形式校验（名字合法 / 不重名 / `from` 与 `value` 恰有其一 / `from` 路径形态）；② 参数模板**形式**校验（`route.ValidateParamTemplates`）：**占位符闭合检查（两档消息分开，见 I6 边界）→ 是否含占位符**、路径形态（非空、不含 `..`、不以 `.` 开头/结尾）、`as` 枚举、`from` 与 `var` 互斥、`{var}` 引用的别名**必须已声明**、不是映射声明的对象按字面量放过（A15）；③ `route.ValidateLiteralParams` 对**能完全解析的字面量逐字段**校验（规则见 §5.4：`required` 按模板键集合、未解析子树跳过、对象级约束不判） | 不对含占位符 / 映射声明的子树跑 schema 校验（类型要触发时才知道）；不判对象级 `min/maxProperties` |
| 触发派发（剪枝） | 取值 → 类型转换 → **同一个** `route.ValidateParams`（全量），失败按 `param_missing` / `param_type` / `param_schema` 剪枝 | 任何静默跳过 |

- `{{name}}` **不要求声明**——它可能就是 payload 路径；拼错在**触发时**以 `param_missing` 报出，并原样回显用户的引用文本。
- `{var:"name"}` 是**显式**引用，拼错必须在**配置期就 400**，错误消息含变量名。
- `DryRun` 与 `Fire` 走**同一套**解析（dry-run 也要能看到 `param_missing` / `param_type`）。
- `config.payload` **只在 cron / manual** 作为事件 payload 打底；webhook / github 用投递体，不合并
  （避免历史行里遗留的 `payload` 键被复活，A13）。
- **`config.payload` 的「必须是 JSON 对象」只对会保留它的 kind 校验**（A17）：cron / manual 上
  非对象（数组 / 标量 / `null`）→ `400`；**webhook / github 直接丢弃这个键**，残留的坏值不会
  因为这次保存而 `400`（那个键对它们没有意义，`config.payload` 不是它们的 payload）。
- **`PATCH` 的 config 合并语义**（A6 / A18）：`"config": null` 视为「**本次不改 config**」（no-op）；
  `config` 里**未带** `vars` / `payload` 键时从**当前配置继承**（这两个键由别的入口写入，控制台
  改 `expr` / `token` 时不会带上它们），显式传 `"vars": []` / `"payload": {}` **才是清空**。
  没有这条，「改个 cron 表达式」会把变量表静默抹掉，紧接着既有的 `{var:"x"}` 全部变成未声明 → 400。
- 解析只作用于**源的首次派发**（`DispatchInput.TargetParams` 有值的出口）；
  能力 → 能力的递归派发原样传递，不解析。

#### 为什么不做模板引擎

求值器一开，立刻多出三类攻击面：**模板注入**（`{{` 嵌套 / 递归展开）、
**表达式 DoS**（超长 / 深嵌套）、**语义漂移**（用户以为支持条件 / 运算 / 函数）；
而收益只是「省掉一次节点侧拼装」。节点本来就知道数据形状，拼接是它的活。
所以中枢的能力上限就是**声明式取值**（I6）。

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
| `POST` | `/v1/nodes` | 注册节点（`namespaceId` 必填；返回**只出现一次**的明文令牌，库里只存 sha256） |
| `POST` | `/v1/nodes/{id}/drain` | 排空节点 |
| `GET`/`POST` | `/v1/namespaces` | 列出 / 新建命名空间（新建时**同事务播种内置能力**） |
| `PATCH`/`DELETE` | `/v1/namespaces/{id}` | 改名（只改 `name`，`slug` 不可改）/ 删除（阻塞 `409`、最后一个 `400`、`?force=true` 级联） |
| `GET` | `/v1/playbooks` | 预案列表（每项带就绪体检结论 `status` / `ready` / `total` / `counts`） |
| `POST` | `/v1/playbooks` | 从 manifest 创建预案（**不安装**；`201`，重名 slug `409`） |
| `GET` | `/v1/playbooks/{idOrSlug}` | 预案详情（额外带逐项体检 `items`） |
| `POST` | `/v1/playbooks/{idOrSlug}/install` | 幂等安装（能力 → 边 → 源，单事务，`200`） |
| `DELETE` | `/v1/playbooks/{idOrSlug}` | 删预案（**不删已装物**；`200` `{"deleted": true}`） |

**JSON 字段一律 camelCase**（I9 同源约束的延伸）。

#### 事件列表的 `limit` 与游标分页

- 「看多少」只有一个数：**硬顶 `500`**。`GET /v1/events` 与 `GET /v1/console/events` 的
  `limit` 上限都是 `500`（store 的列表查询也以 `500` 为界）。
- 两个端点的响应**都回显生效值** `limit`（整数），但**缺省 / 非法 / 非正数**怎么算**不同**
  （这是**既有**差异，不是本批引入，也不打算在 `#236` 里拍平）：
  - `GET /v1/events`（机器入口）：缺省 / 非整数 / `≤0` → `100`（`n > 0` 才采纳）；`> 500` → 钳到 `500`。
  - `GET /v1/console/events`（控制台）：缺省 / 非整数 → `100`；`≤0` → **夹到下限 `1`**（`intParam` 的 min 夹取）；
    `> 500` → 钳到 `500`。也就是 `limit=0` / `limit=-5` 在控制台拿 1 条、在机器入口拿 100 条。
  「要到了」与「被截断」必须可分辨——**不静默降级**（`limit=501..1000` 曾静默退回旧默认值 `50`，`#236` 的病根）。
  两个端点语义不同：改一个的 `limit` 解析前先确认另一个没被顺手带坏（`console_handlers.go` 的
  `intParam` vs `internal/api` 的 `n > 0`）。
- `GET /v1/console/events` 支持游标分页：请求带 `before=<不透明游标>`（取自上一页响应的 `nextCursor`），
  响应带 `nextCursor`。**`nextCursor` 为空 ⇔ 没有更旧的行**：服务端每页**多看一行**确认
  （`perNS = limit+1`，多出来的那行只用于判定、不返回），所以「库里正好整页」不会再给出一个
  多余的空页。已知上限：`limit=500`（= 硬顶）时取不到那多出来的一行，退化为「行数 ≥ limit ⇒ 给游标」
  的近似判据，此时正好 500 条会多给一个空页。游标格式非法 → `400`，不静默忽略。
  机器入口 `GET /v1/events` **不**提供游标（保持现状）。

#### 命名空间参数怎么带

**显式，或者显式全部 —— 永远没有隐式默认。**

| 端点 | `namespaceId` |
|---|---|
| `/v1/capabilities`、`/v1/nodes`、`/v1/nodes/{id}`、`/v1/edges`、`/v1/sources`、`/v1/playbooks*`、`/v1/events`、`/v1/tasks`、`/v1/traces/*`、`/v1/stats` | **必填** |
| 同上但要跨命名空间汇总（管理员视图） | `namespaceId=*` |
| `/v1/playbooks*`（5 个端点全部） | **不接受 `*`**（一律 `400`）——预案不提供跨命名空间汇总口径 |
| `/v1/admin/*`、`/v1/auth/*` | **不需要**（部署级；组织层已删除，`/v1/orgs` 从来不存在，将来也不会加） |

- 参数位置：`GET` 用 query `namespaceId`；`POST`/`PATCH`/`DELETE` 可放 query 或 body 的
  `namespaceId`（**query 优先**；例外：机器入口 `POST /v1/events` 是 **body 优先**，
  只读 query 的写端点是 `POST /v1/tasks/{id}/cancel`、`POST /v1/nodes/{id}/drain`、
  `DELETE /v1/sources/{id}`）
- 缺 `namespaceId` → `400`（**不回退到默认命名空间**：回退等于把人写错的地方藏起来）
- `namespaceId=*` → 跨命名空间汇总，**只允许读端点**；写端点 / 单对象端点带 `*` → `400`；
  `/v1/stats` 的汇总口径未定义，它的 `*` 同样 `400`
- 不存在 → `404`；未登录 / 用户不存在 → `401`；跨命名空间引用 → `400`（I11）
  - **没有 `403`。** 组织层删除（迁移 14）后「有，但属于别人」这个中间态不存在了：
    存在就 `200`，不存在就 `404`。原先为写端点单独准备的「跨组织也报 `404`、
    响应逐字节相同、以免当命名空间 id 探测器」那套映射（`#185` / 契约 D2）
    随之失效——不存在一律 `404`，探测者拿到的回答和正常调用本来就没有差别。
  - **`/v1/playbooks*` 的 5 个端点对 `namespaceId=*` 一律 `400`**：预案不提供
    跨命名空间汇总——体检结论（能力定义 / 边 / 触发器的存在性）只能按**单个**
    命名空间算，「汇总的 ready 率」是个没有定义的量。这 5 个端点**每一个**都必填
    `namespaceId`（见 §2.8）
- 所有资源在响应里带 `namespaceId`；`/v1/namespaces` 自身不带
- **节点归属**：机器属于哪个命名空间由**节点令牌**决定（令牌 → 节点行 →
  `nodes.namespace_id`，握手时按 token 解析，见 §10.3），创建后不可改
  （要换就删了重建）。节点协议帧里不传 `namespaceId`；注册入口是 `POST /v1/nodes`
  （`namespaceId` 必填，query 或 body、**query 优先**；跨组织 → `404`；缺参 → `400`、
  `*` → `400`）。令牌即归属（token → `nodes.namespace_id`），创建后不可改。

---

## 7. 错误码与剪枝原因

| 码 | 层 | 含义 |
|---|---|---|
| `unknown_capability` | 注册/派发 | 能力名+版本在中枢无定义 |
| `no_edge` | 路由 | `edges` 查表 0 行 |
| `param_missing` | 路由 | 参数引用的路径在 payload / 别名表里取不到（含 payload 里的显式 `null`），且没有 `default`（§5.5） |
| `param_type` | 路由 | 取值结果转不成目标 `params_schema` 要求的类型（§5.5） |
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
| ③ | `param_missing` | 是 | 参数引用的路径取不到且无 `default`（§5.5） |
| ④ | `param_type` | 是 | 取值结果转不成目标 schema 要求的类型（§5.5） |
| ⑤ | `param_schema` | 是 | 参数不过目标能力 schema |
| ⑥ | `no_node` | 是 | 有边、参数也对，但没有节点声明该能力 |
| ⑦ | `foreign_user` | 是 | `params.to` 指向的用户不属于本组织 |
| ⑧ | `path` | 是 | 目标能力已在路径里（环剪枝） |

**②~⑧ 七种记 `pruned` + `reason`；① 不是剪枝。**
剪枝是静默的（不打断业务），但**一律留痕**——trace 上能看到 `reason` 和随附证据。
剪枝落库为 `events.type = pruned.<reason>`（`source=route`），如 `pruned.param_missing`。

---

## 8. 与现有代码的对照（重构清单）

| 现有 | 处置 |
|---|---|
| `internal/store/schema.sql` 10 张表 | **删**，按本文重建 13 张 |
| `internal/broker`（Publish/Subscribe/Ack/Replay） | **改**：`Message` → `Event`/`Task`；`Ack` → 任务 `ack` |
| `internal/ws`（notification/ack 帧） | **改**：换成 §6.1 的节点协议 |
| `internal/route`（`ScopeMatch`/`TagMatch`） | **删**：路由改为查 `edges` |
| `internal/push` | **保留**：`notify.phone` 变成一个 `end` 能力 |
| `internal/auth` | **改**：注册走 `store.RegisterUser`（建用户 + 默认命名空间，同事务） |
| `internal/api/notify.go` | **改**：`POST /v1/notify` → 产 `events` 行 |
| `internal/server/mux.go` | **改**：按 §6.2 重新注册 |
| `api/openapi.yaml` | **重写**：现有 50 个 operation 收缩到本模型 |

---

## 9. 验收标准（契约是否被实现）

| # | 断言 | 怎么测 |
|---|---|---|
| A1 | 注册即得命名空间 | 新用户注册后 `namespaces` 里有他的 `default` 一行且内置能力已播种（`store.RegisterUser` 同事务；组织层已在迁移 14 删除） |
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
-- notify.phone 派发前（迁移 14 之后）
SELECT 1 FROM users WHERE id = :params_to;
-- 0 行 → pruned / reason=foreign_user，不建任务
```

这是 `params_schema` 管不到的部分：schema 只能校验「`to` 是个非空字符串」，
**「这个名字在本部署里是不是个真人」是业务规则，要单独查**。

**判据变了（#204）**：原来是「`params_to` 必须是 `org_members` 里本组织的成员」，
现在组织层删除、本部署任何已登录用户都能访问任何命名空间，所以「把消息推给外组织
用户」这个威胁不存在了；规则收窄成「收件人必须存在」。剪枝原因常量与 wire 值仍是
`foreign_user`（历史拼写保留，事件流可读），语义已写在
`internal/route/dispatch.go` 的 `checkUserScoped` 注释里。
同类规则以后每加一个按人投递的能力都要补一次。

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
| 命名空间携带 | 写进 claim | 查表得到（`nodes.namespace_id`），并在握手时过 `AuthorizeNamespaceForNode` 收口 |
| 泄露后的处置 | 等 TTL 过期 | 立即吊销 |
| 实现 | 需要签名/验证 + 刷新逻辑 | 一个索引查询 |

**决定：opaque token + 查表。** 理由：
1. **节点要有「立即踢掉」的能力**——一台机器丢了，改一行就该断，不该等 TTL。
2. WS 连接是长连接，JWT 的短 TTL 在这里反而是负担（要么频繁刷新，要么长 TTL 失去意义）。
3. 验签成本可以忽略：`hello` 时查一次，之后靠连接对象持有 `node_id`/`namespace_id`，不是每帧都查。
   注意「连接持有」不等于「永不复查」：命名空间可能在连接存续期间被删除，所以握手时会过一次
   `store.AuthorizeNamespaceForNode`（#207）；令牌一旦签发就不再追溯成员关系变化，这一点写在
   该函数的注释里。

token 形态与现有 `api_keys` 一致：`ant_<label>_<base64url(24B)>`，DB 只存 `sha256`，明文只在创建时返回一次。
明文令牌的**唯一发放点**是 `POST /v1/nodes`：响应里出现一次，之后任何读接口（`GET /v1/nodes`、
`GET /v1/nodes/{id}`）都不回 `token` / `token_hash`。

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
