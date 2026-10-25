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

-- 节点声明「我有 review@1」——只有名字 + 版本，没有命令、没有出口、没有 kind
CREATE TABLE IF NOT EXISTS node_capabilities (
    node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
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
-- 一行 = 一条边。同一 (from_capability, from_port) 可有多行 → 扇出。
-- 两端都是能力定义，不引用任何机器（不变量 I7）。
CREATE TABLE IF NOT EXISTS edges (
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    from_capability TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    from_port       TEXT NOT NULL,            -- 出口名，必须 ∈ from 的 outputs
    to_capability   TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    created_at      INTEGER NOT NULL,
    PRIMARY KEY (org_id, from_capability, from_port, to_capability)
);

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
