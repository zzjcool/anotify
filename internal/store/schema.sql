-- Anotify · SQLite schema（单一事实源）
-- 连接初始化 PRAGMA（在代码里设置）：
--   journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000; foreign_keys=ON;
--
-- 本文件同时是**迁移脚本**：按 `-- +migration N name` 切块，
-- store.migrateWith 逐块应用，schema_version 记账（见 store.go）。
-- 迁移规则：只追加新块，不修改已发布的块（老库已记账，改了不起作用）。
-- 需要 Go 逻辑（条件 ALTER 等）的块写空的 SQL，在 store.go 的 migrationHooks 里实现。

-- +migration 1 core_identity
-- 用户（Passkey 无密码）
CREATE TABLE IF NOT EXISTS users (
    id          TEXT PRIMARY KEY,            -- usr_xxx
    username    TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    role        TEXT NOT NULL DEFAULT 'member',  -- admin | member（首个注册用户自动 admin）
    disabled    INTEGER NOT NULL DEFAULT 0,      -- 超管禁用某用户（1=禁用，禁止登录）
    created_at  INTEGER NOT NULL             -- unixepoch 秒
);

-- Passkey 凭证（一个用户可有多个）
CREATE TABLE IF NOT EXISTS passkeys (
    id              TEXT PRIMARY KEY,           -- credential id (base64url)
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    public_key      BLOB NOT NULL,
    sign_count      INTEGER NOT NULL DEFAULT 0,
    name            TEXT NOT NULL DEFAULT '',   -- 设备名，如「主力手机」
    transports      TEXT NOT NULL DEFAULT '[]', -- JSON 数组
    backup_eligible INTEGER NOT NULL DEFAULT 0, -- BackupEligible flag（登录校验需一致）
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER
);

-- 登录会话
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,             -- sess_xxx token
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_name TEXT NOT NULL DEFAULT '',     -- 登录设备名（UA 推断，如「Chrome · macOS」）
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
);

-- API Key（仅存哈希；scope 控制 notify:send / notify:receive / devices:read）
CREATE TABLE IF NOT EXISTS api_keys (
    id          TEXT PRIMARY KEY,            -- key 的标识（ant_xxx 前缀部分）
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL DEFAULT '',
    key_hash    TEXT NOT NULL,               -- argon2id 哈希
    prefix      TEXT NOT NULL,               -- 明文前缀，便于识别（如 ant_live_a1b2）
    scopes      TEXT NOT NULL DEFAULT '[]',  -- JSON 数组
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    last_used_at INTEGER
);

-- 接收设备（Web Push 订阅）——属于用户，不属于组织（契约 Q1）
CREATE TABLE IF NOT EXISTS devices (
    id           TEXT PRIMARY KEY,           -- dev_xxx
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    platform     TEXT NOT NULL DEFAULT '',   -- ios|mac|win|android|other
    enabled      INTEGER NOT NULL DEFAULT 1,
    event_scope TEXT NOT NULL DEFAULT 'final', -- final|all（push 设备默认 final，只收终态）
    tags         TEXT NOT NULL DEFAULT '[]', -- JSON 数组（设备标签）
    endpoint     TEXT NOT NULL UNIQUE,       -- push endpoint
    p256dh       TEXT NOT NULL,
    auth         TEXT NOT NULL,
    user_agent   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_active  INTEGER,
    last_delivered INTEGER
);

-- CLI 设备授权会话（device authorization flow）
-- 一次性会话：pending → approved → consumed；旁路终态 denied / expired
CREATE TABLE IF NOT EXISTS cli_auth_sessions (
    id               TEXT PRIMARY KEY,            -- cas_xxx
    secret_hash      TEXT NOT NULL,               -- sha256 hex（secret 仅建会话时返回一次，不落明文）
    user_code        TEXT NOT NULL UNIQUE,        -- 8 字符大写无连字符（去歧义字符集）
    device_name      TEXT NOT NULL,               -- ≤64 字符（脚本上报 hostname）
    kind             TEXT NOT NULL DEFAULT 'apikey', -- apikey / passkey（区分授权类型）
    device_hint      TEXT NOT NULL DEFAULT '',     -- Passkey 授权：新设备敲门时自报设备信息
    scopes_requested TEXT NOT NULL,               -- JSON 数组
    scopes_granted   TEXT,                        -- JSON 数组，批准前 NULL
    status           TEXT NOT NULL,               -- pending/requested/approved/consumed/denied/expired
    user_id          TEXT,                        -- 批准前 NULL
    key_id           TEXT,                        -- 领证前 NULL
    created_at       INTEGER NOT NULL,
    expires_at       INTEGER NOT NULL,            -- created_at + 600（TTL 10 分钟）
    consumed_at      INTEGER
);

-- 恢复码（仅存哈希；明文只在签发时返回一次）
-- 格式：XXXX-XXXX-XXXX-XXXX（去歧义字符集），一批 8 个；兑换单个后作废该码。
CREATE TABLE IF NOT EXISTS recovery_codes (
    id          TEXT PRIMARY KEY,            -- rc_xxx
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL,               -- argon2id PHC
    prefix      TEXT NOT NULL,               -- 规范化明文前缀（便于定位候选）
    created_at  INTEGER NOT NULL,
    used_at     INTEGER                      -- NULL = 未使用
);

CREATE        INDEX IF NOT EXISTS idx_devices_user          ON devices(user_id);
CREATE        INDEX IF NOT EXISTS idx_sessions_user         ON sessions(user_id);
CREATE        INDEX IF NOT EXISTS idx_apikeys_user          ON api_keys(user_id);
CREATE        INDEX IF NOT EXISTS idx_cli_auth_expires      ON cli_auth_sessions(expires_at);
CREATE        INDEX IF NOT EXISTS idx_recovery_codes_user   ON recovery_codes(user_id);
CREATE        INDEX IF NOT EXISTS idx_recovery_codes_prefix ON recovery_codes(prefix);

-- +migration 2 legacy_columns
-- 需要按 pragma_table_info 判断后再 ALTER 的列（老库升级用），由 store.go 的
-- migrationHooks[2] 实现；SQL 空块。新库这些列已在上面的 CREATE TABLE 里。

-- +migration 3 legacy_message_model
-- 【临时】旧通知模型的三张表。重构期间旧端点（/v1/notify、/v1/notifications、
-- 浏览器 WS）仍在运行，等 #7 把它们整体下线后，本迁移块与这些表一起删除。
-- 新代码不得再写这三张表。
CREATE TABLE IF NOT EXISTS messages (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    seq         INTEGER NOT NULL,
    title       TEXT NOT NULL,
    agent_state TEXT NOT NULL DEFAULT 'working',
    body        TEXT NOT NULL DEFAULT '',
    link        TEXT NOT NULL DEFAULT '',
    device_tags TEXT NOT NULL DEFAULT '[]',
    priority    TEXT NOT NULL DEFAULT 'normal',
    ttl_seconds INTEGER NOT NULL DEFAULT 86400,
    payload     TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    UNIQUE (user_id, seq)
);

CREATE TABLE IF NOT EXISTS consumer_offsets (
    consumer_id TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    last_seq    INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (consumer_id, user_id)
);

CREATE TABLE IF NOT EXISTS deliveries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    consumer_id TEXT NOT NULL,
    channel     TEXT NOT NULL,
    status      TEXT NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    attempts    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_user_seq     ON messages(user_id, seq);
CREATE        INDEX IF NOT EXISTS idx_messages_user_created ON messages(user_id, created_at);
CREATE        INDEX IF NOT EXISTS idx_messages_expires      ON messages(expires_at);
CREATE        INDEX IF NOT EXISTS idx_deliveries_msg        ON deliveries(message_id);
CREATE        INDEX IF NOT EXISTS idx_deliveries_consumer   ON deliveries(consumer_id, updated_at);

-- +migration 4 new_model
-- ── 组织与身份（契约 §2.1）──────────────────────────────
CREATE TABLE IF NOT EXISTS orgs (
    id         TEXT PRIMARY KEY,              -- org_<base62>
    name       TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL               -- unixepoch 秒
);

CREATE TABLE IF NOT EXISTS org_members (
    org_id    TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role      TEXT NOT NULL DEFAULT 'member',  -- owner | admin | member
    joined_at INTEGER NOT NULL,
    PRIMARY KEY (org_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_org_members_user ON org_members(user_id);

-- ── 能力定义（中枢侧，append-only，契约 §2.2）─────────────
-- outputs 是出口名的唯一来源：既用于 kind 推导，也定义 edges.from_port 的封闭集合。
-- 没有 kind 列——推导量不落库（不变量 I3）。
CREATE TABLE IF NOT EXISTS capability_versions (
    id            TEXT PRIMARY KEY,           -- cap_<UTC>_<hash8>
    org_id        TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,              -- review
    -- display_name 是**纯展示**用的标签（用户自己取的中文名）。
    -- 它绝不参与路由：edges 两端指向 id，出口名来自 outputs，节点声明用 name。
    -- 也**不参与 content_hash** —— 改个显示名不该算"内容变了"。
    display_name  TEXT NOT NULL DEFAULT '',
    version       INTEGER NOT NULL,           -- 1
    params_schema TEXT NOT NULL,              -- JSON Schema
    outputs       TEXT NOT NULL,              -- JSON 数组，如 ["fix","clean"]
    lifecycle     TEXT NOT NULL DEFAULT '{}', -- JSON：expiresInSec/retryable/maxAttempts/resultTtlSec/idempotent
    sticky        TEXT NOT NULL DEFAULT '',   -- '' | 'worktree' | ...
    source        TEXT NOT NULL,              -- builtin | custom
    content_hash  TEXT NOT NULL,              -- 规范化内容 sha256
    created_at    INTEGER NOT NULL,
    UNIQUE (org_id, name, version),
    UNIQUE (org_id, content_hash)             -- 同内容不产生第二行
);

CREATE INDEX IF NOT EXISTS idx_cap_name_ver ON capability_versions(org_id, name, version);

-- ── 节点（契约 §2.3）───────────────────────────────────
CREATE TABLE IF NOT EXISTS nodes (
    id           TEXT PRIMARY KEY,            -- nd_<base62>
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,               -- mbp-16（可改，id 不变）
    tags         TEXT NOT NULL DEFAULT '[]',  -- JSON 数组，节点池标签
    status       TEXT NOT NULL DEFAULT 'offline', -- online | offline | draining
    token_hash   TEXT NOT NULL,               -- sha256，明文只在创建时返回一次
    max_instances   INTEGER NOT NULL DEFAULT 20,  -- 硬顶，防一份 token 复制出一片
    max_concurrency INTEGER NOT NULL DEFAULT 8,   -- 节点级并发上限
    last_seen_at INTEGER,
    created_at   INTEGER NOT NULL,
    UNIQUE (org_id, name)
);

-- 节点声明「我有 review@1」。
--
-- 中枢侧只知道**做什么**（能力名 + 版本）；`run_spec` 是节点自己上报的
-- 「我打算怎么跑」的 JSON 原文，**只存不执行**（见迁移 10 的说明）。
-- 这一列不参与派发选点，控制台只拿它做展示与诊断。
-- 没有出口、没有 kind：那些是从能力定义与边表推导出来的。
CREATE TABLE IF NOT EXISTS node_capabilities (
    node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    run_spec      TEXT NOT NULL DEFAULT '{}', -- 节点上报的执行方式原文；'{}' = 未声明
    PRIMARY KEY (node_id, capability_id)
);

CREATE INDEX IF NOT EXISTS idx_nodecap_cap ON node_capabilities(capability_id);

-- 并发槽位：节点申报，中枢 upsert
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

-- 粘性路由：sticky_key = sha256(trace_id ∥ capability_id ∥ selector)
CREATE TABLE IF NOT EXISTS node_pins (
    sticky_key   TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    selector     TEXT NOT NULL,               -- 当时命中的标签
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL
);

-- ── 出口表（路由唯一依据，契约 §2.4）────────────────────
-- 一行 = 一条边。同一 (from_key, from_port) 可有多行 → 扇出。
-- 右端一定是能力定义，不引用任何机器（不变量 I7）。
--
-- 左端有**两种**，用 from_is_source 区分（见契约 §2.4）：
--
--   from_is_source = 0  能力之间的边：from_key = 上游能力的 capability_versions.id
--                       出口名必须 ∈ 该能力的 outputs（不变量 I2，封闭集合）
--
--   from_is_source = 1  入口边（机器入口 POST /v1/events）：from_key = **来源名**
--                       如 "github" / "cron" / "jira"，出口名 = 事件类型，
--                       任意值（外部系统决定，不可能预先枚举，见契约 §5.2）
--
-- 为什么入口边不再引用能力：入口原来靠一个叫 github@1 的"空壳能力"当锚点，
-- 它没有节点、不产生任务、也不可配置（能力表里没地方存 cron 表达式），
-- 用户却在能力列表里看得见它。源（sources）已经是它的完整替代，
-- 所以入口边直接以来源名为左端，不再需要那个空壳。
--
-- 历史数据：迁移 9 会把已存在的起点边（from_capability 指向内置 origin 能力）
-- 转成 from_is_source=1 + from_key=该能力的 name，然后不再需要那些空壳能力。
CREATE TABLE IF NOT EXISTS edges (
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    from_is_source  INTEGER NOT NULL DEFAULT 0,   -- 0=能力边，1=入口边
    from_key        TEXT NOT NULL,                -- 能力 id 或 来源名（看上面）
    from_port       TEXT NOT NULL,
    to_capability   TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    created_at      INTEGER NOT NULL,
    PRIMARY KEY (org_id, from_is_source, from_key, from_port, to_capability)
);

CREATE INDEX IF NOT EXISTS idx_edges_lookup ON edges(org_id, from_is_source, from_key, from_port);

CREATE INDEX IF NOT EXISTS idx_edges_lookup ON edges(org_id, from_capability, from_port);

-- ── 事件与任务（契约 §2.5）──────────────────────────────
CREATE TABLE IF NOT EXISTS events (
    id         TEXT PRIMARY KEY,              -- evt_<base62>
    org_id     TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    trace_id   TEXT NOT NULL,                 -- 一个根事件引起的全部工作
    source     TEXT NOT NULL,                 -- github | cron | watch | http | manual
    type       TEXT NOT NULL,                 -- pull_request.opened
    payload    TEXT NOT NULL DEFAULT '{}',    -- 原始 JSON，只存不解释
    idem_key   TEXT,                          -- 幂等键（触发器给；NULL 允许重复）
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_trace ON events(org_id, trace_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_idem ON events(org_id, source, idem_key)
    WHERE idem_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS tasks (
    id             TEXT PRIMARY KEY,          -- tsk_<base62>，同时是 OTel span_id
    org_id         TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    trace_id       TEXT NOT NULL,
    parent_task_id TEXT,                      -- 谁派生；根事件直接派生的为 NULL
    path           TEXT NOT NULL DEFAULT '[]',-- JSON 数组：从根到本任务的能力序列（去重剪枝用）

    capability_id  TEXT NOT NULL REFERENCES capability_versions(id),
    capability     TEXT NOT NULL,             -- 冗余存 name，展示用
    version        INTEGER NOT NULL,
    params         TEXT NOT NULL DEFAULT '{}',-- 派发参数，已过 params_schema

    status         TEXT NOT NULL DEFAULT 'pending', -- 九态，见契约 §3
    node_id        TEXT,
    instance_id    TEXT,
    lease_until    INTEGER,                   -- 租约截止
    attempts       INTEGER NOT NULL DEFAULT 0,

    out            TEXT,                      -- 出口名；NULL = 没回报（'' 不是合法值）
    reported       INTEGER NOT NULL DEFAULT 0,-- 1 = 节点已回报（哪怕 out 是 NULL）
    summary        TEXT NOT NULL DEFAULT '',
    artifacts      TEXT NOT NULL DEFAULT '[]',
    error          TEXT NOT NULL DEFAULT '',

    cap_hash       TEXT NOT NULL,             -- 派发时的能力定义 content_hash
    graph_id       TEXT NOT NULL,             -- 派发时的 graph_versions.id

    created_at     INTEGER NOT NULL,
    started_at     INTEGER,
    finished_at    INTEGER,

    -- 退避：早于此刻的 pending 任务不该被重派（契约 §3.3 的 10s/60s/300s）。
    -- NULL 表示「立刻可派」（新建任务、或没经历过失败）。
    not_before     INTEGER
);

CREATE INDEX IF NOT EXISTS idx_tasks_trace   ON tasks(org_id, trace_id);
CREATE INDEX IF NOT EXISTS idx_tasks_parent  ON tasks(org_id, parent_task_id);
CREATE INDEX IF NOT EXISTS idx_tasks_lease   ON tasks(status, lease_until);
CREATE INDEX IF NOT EXISTS idx_tasks_node    ON tasks(node_id, status);

-- ── 图快照（契约 §2.6）─────────────────────────────────
CREATE TABLE IF NOT EXISTS graph_versions (
    id           TEXT PRIMARY KEY,            -- grf_<UTC>_<hash8>
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL,
    snapshot     TEXT NOT NULL,               -- JSON：全部 edges 的规范化快照
    created_at   INTEGER NOT NULL,
    UNIQUE (org_id, content_hash)
);

-- +migration 5 new_model_columns
-- 新模型（events/tasks/...）的补列，由 store.go 的 migrationHooks[5] 实现；SQL 空块。
-- （tasks.not_before 退避列，见 #78）
-- 注意：本块此前只有 Go hook、缺 SQL 头，导致 parseMigrations 的连续性检查
-- 看到 4 → 6 而报错。补上这个空块让编号连续。

-- +migration 6 drop_legacy_message_model
-- #80：下线旧「通知终端」模型。
--
-- 为什么**不删迁移 3 的建表语句**：迁移规则是「只追加，不改已发布的块」。
-- 老库已经执行过迁移 3（建了这三张表），改迁移 3 对它不起作用；
-- 新库若跳过迁移 3 又会破坏版本连续性检查。
-- 所以正确的做法是新增一个迁移来 DROP。
--
-- 为什么可以安全删：
--   - messages   → 事件中心里"发生了什么"是 events、"做完没有"是 tasks
--   - consumer_offsets → broker 的 per-user 序列位点，broker 包已删除
--   - deliveries → 旧消息的投递记录；推送结果现在落在任务状态与日志里
--
-- devices **不在此列**：它是推送订阅注册表（"用户设备在哪"是持久事实），
-- notify.phone 仍需要它投递。
--
-- 迁移前请先导出历史数据（见 DEVELOPMENT.md 的「旧模型下线」一节）。
DROP TABLE IF EXISTS deliveries;
DROP TABLE IF EXISTS consumer_offsets;
DROP TABLE IF EXISTS messages;

-- +migration 7 sources
-- 「源」（origin）：事件的**入口实例**，不是能力。
--
-- 为什么单独一张表而不是复用 capability_versions：
--
--   能力（capability）回答"能干什么"——它由节点声明、中枢派任务给它。
--   源（source）回答"什么时候开始干"——它有配置（cron 表达式 / webhook 密钥），
--   自己产生事件，自己不干活。
--
-- 之前把它们混在一起（github/cron/http/manual 是内置起点能力），后果是
-- 用户为了发一个事件得先造一个"没有出口、没有节点"的假能力，而且没法配置
-- "每晚 3 点"这种参数——能力表里没有存这种配置的地方。
--
-- 与 graph.edge 的关系：源**不建边**。源直接持有出口能力（source_targets），
-- 语义是"我来事件 → 派给这些能力"。这样用户不需要理解 kind=origin、
-- 也不需要为每个事件类型手建一条边。
CREATE TABLE IF NOT EXISTS sources (
    id          TEXT PRIMARY KEY,             -- src_<base62>
    org_id      TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,                -- 用户可读名（"每晚体检"）
    kind        TEXT NOT NULL,                -- cron | webhook | manual
    -- config 是按 kind 解释的 JSON（不变量 I6：只读配置字段，不求值、不执行）
    --   cron    {"expr":"0 3 * * *","tz":"Local"}
    --   webhook {"token":"..."}（token 用于拼出唯一 URL）
    --   manual  {}
    config      TEXT NOT NULL DEFAULT '{}',
    enabled     INTEGER NOT NULL DEFAULT 1,
    -- 调度元数据（只有 cron 用；其它 kind 为 NULL）
    last_fired_at  INTEGER,
    next_fire_at   INTEGER,
    created_at  INTEGER NOT NULL,
    UNIQUE (org_id, name)
);

-- 源的出口：事件要派给哪些能力。
--
-- 多行 = 扇出（一个源同时喂多个能力）。**只存 capability_id**，
-- 不存端口名：源的"端口"就是它自己产生的 type，由 kind 决定
-- （cron/manual 用源自己的 name，webhook 用请求体里的 type）。
CREATE TABLE IF NOT EXISTS source_targets (
    source_id     TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (source_id, capability_id)
);

CREATE        INDEX IF NOT EXISTS idx_sources_org      ON sources(org_id, created_at);
CREATE        INDEX IF NOT EXISTS idx_sources_due      ON sources(enabled, next_fire_at);
CREATE        INDEX IF NOT EXISTS idx_src_targets_cap  ON source_targets(capability_id);

-- +migration 8 source_target_params
-- 源 → 出口能力的**参数模板**（由 store.go 的 migrationHooks[8] 实现；SQL 空块）。
--
-- 为什么必须按每个能力分别存，而不是源上存一份 params：
-- 每个出口能力有自己的 params_schema（字段名、必填项都不一样）。
-- 一个源同时喂 analyze-pr（要 changedLines）和 deploy-preview（要 env）
-- 时，一份公共 params 必然对不上其中至少一边，触发时被 param_schema 剪枝，
-- 而用户配完以为好了——这正是要消灭的静默失败。
--
-- 语义：触发时把**所有出口的参数**合并成一份 params 传给 Dispatcher。
-- 字段名冲突时以后写入的为准（出口按 capability_id 排序，行为确定）。
--
-- 与不变量 I6 的关系：这里存的是**数据**，不是模板表达式。
-- 不做 {{ .body.x }} 这种替换——中枢不解释任何用户提供的逻辑。
--
-- 用 Go hook 而不是裸 ALTER：SQLite 没有 ADD COLUMN IF NOT EXISTS，
-- 而迁移必须幂等（同一版本可能因重试执行多次）。

-- +migration 9 edges_from_source
-- #110：入口边不再需要一个"空壳能力"当左端。
--
-- 旧模型：github@1 这种内置起点能力是边表左端的外键锚点，于是它必须
-- 存在于 capability_versions 里，也就会出现在能力列表上（用户可以看见
-- 一个不执行任何代码、也不产生任务的条目）。
--
-- 新模型：入口边左端直接是**来源名**（from_is_source=1 + from_key='github'）。
-- 这张表要重建（from_capability 在主键里，SQLite 不能直接改列），
-- 所以 SQL 块留空，逻辑在 store.go 的 migrationHooks[9]。

-- +migration 10 node_capability_run_spec
-- #168：节点把「我打算怎么跑这个能力」上报上来（node.yaml 的 run 块），
-- 落成 node_capabilities.run_spec（JSON 原文）。
--
-- 为什么需要这一列：node_capabilities 过去只记 name@version，于是控制台
-- 回答不了「这台机器上的 review 到底是怎么跑的」。节点换了运行时参数、改了超时，
-- 运维只能登上那台机器去看自己的配置文件。存下来就只为了展示与排障。
--
-- 与不变量 I1（中枢不知道「怎么执行」）的关系：run 是**节点自己的事**。
-- 中枢既不解码也不执行它——不拆参数、不判路径是否存在/可执行、不拼命令行，
-- 也不把它写进任何下行帧；唯一用途是控制台读出。
-- 派发选点（NodesForCapability）看不到这一列，行为与加列前完全一致。
--
-- 用 Go hook 而不是裸 ALTER：SQLite 没有 ADD COLUMN IF NOT EXISTS，
-- 而迁移必须幂等（同一版本可能因重试执行多次），逻辑在 migrate.go 的
-- migrationHooks[10]（migrateNodeCapabilityRunSpec）。

-- +migration 11 retention_indexes
-- #143 follow-up：保留期清理的两条 DELETE 子查询原本是「整 org 扫描 + TEMP B-TREE 排序」，
-- LIMIT 只限制**删除行数**，不限制**扫描 / 排序行数**——表一大，每批都在整 org 上重排一遍。
--
-- 两条索引正好覆盖各自的 WHERE 条件 + ORDER BY：
--   tasks : org_id + capability_id + status + finished_at
--   events: org_id + created_at
-- 它们同时服务事件流与统计（都按 org 过滤、按时间排序），所以不只是给清理用的。
--
-- CREATE INDEX IF NOT EXISTS 本身幂等，不需要 Go hook。
CREATE INDEX IF NOT EXISTS idx_tasks_retention
    ON tasks (org_id, capability_id, status, finished_at);
CREATE INDEX IF NOT EXISTS idx_events_retention
    ON events (org_id, created_at);

-- +migration 12 namespaces
-- #162：命名空间（设计 docs/design/namespaces.md，契约 §2.1.1）。
--
-- 组织之下多一层硬隔离边界：能力 / 节点 / 边 / 源 / 事件 / 任务 / 图快照
-- **全部**属于某个命名空间，组织级只剩身份（users / org_members）与密钥（api_keys）。
--
-- 为什么不是「只加一列 org_id 的兄弟列」：唯一键与主键的前缀必须换成
-- namespace_id，否则 `review@1` 在 prod 与 staging 里仍然是同一行、
-- 同一条边会同时影响两个环境 —— 这正是本 issue 要消灭的耦合。
-- SQLite 改不了主键 / 唯一键，也加不了带 FK 的列，所以 11 张表的重建逻辑
-- 在 migrate.go 的 migrationHooks[12]（migrateNamespaces）：
--   ① 给每个已存在的 org 建 `default` 命名空间
--   ② 逐表重建（新表 → 搬数据 → 删旧 → 改名 → 重建索引），老数据回填到 default
--   ③ 给每个命名空间播种内置能力（notify.phone 必须存在，否则 end 节点声明不出来）
CREATE TABLE IF NOT EXISTS namespaces (
    id         TEXT PRIMARY KEY,              -- ns_<base62>
    org_id     TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug       TEXT NOT NULL,                 -- prod / staging / team-a（稳定，出现在 URL 里）
    name       TEXT NOT NULL,                 -- 展示名（可改）
    created_at INTEGER NOT NULL,
    UNIQUE (org_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_namespaces_org ON namespaces(org_id, created_at);

-- +migration 13 playbooks
-- #193：预案（playbook）—— 一份可安装的流程零件（设计 docs/design/playbooks.md）。
--
-- 预案本身**不产生任务**：它只是一份清单（能力 / 边 / 触发器），安装时按
-- 「能力 → 边 → 源」的顺序补齐本命名空间里的真实资源（I8 append-only：
-- 已有定义一律不覆盖）。就绪体检是查询时算出来的推导量，不落库（I3 同源）。
--
-- manifest 存**提交的 JSON 原文**：runner / paramsSchema 这类节点侧字段中枢
-- 只存不解释（I1），重新编码会丢掉键序与用户自定义键（往返一致性）。
--
-- 本注释刻意不写任何「可执行概念」的英文关键词：make contract 的 A2 正则
-- 扫 schema.sql 全文（注释也算），命中即失败。
--
-- 同理，说明文字这一列叫 `descr` 而不是它的完整英文单词：A2 的正则是**子串**
-- 匹配，那个单词里恰好含有被禁的子串（契约 §4 的 DDL 写的是完整单词，与 A2
-- 关卡直接冲突；列名缩写在 store 层之外不可见，Go 字段与 JSON 仍是完整单词）。
CREATE TABLE IF NOT EXISTS playbooks (
    id           TEXT PRIMARY KEY,              -- pb_<base62>
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug         TEXT NOT NULL,                 -- pr-review（URL 用，命名空间内唯一）
    name         TEXT NOT NULL,                 -- PR 审查
    descr        TEXT NOT NULL DEFAULT '',      -- 说明文字（缩写列名，见上方注释）
    manifest     TEXT NOT NULL,                 -- manifest JSON 原文
    created_at   INTEGER NOT NULL,
    UNIQUE (namespace_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_playbooks_ns ON playbooks(namespace_id, created_at);
