-- Anotify · SQLite schema（v4 baseline，单一事实源）
-- 连接初始化 PRAGMA（在代码里设置）：
--   journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000; foreign_keys=ON;
--
-- 本文件同时是**迁移脚本**：按 `-- +migration N name` 切块，
-- store.migrateWith 逐块应用，schema_version 记账（见 migrate.go）。
--
-- v4 开发期直接重建：只有一块 `-- +migration 1 baseline`，没有历史增量块。
-- 旧库（schema_version 里出现过更高版本，或没有 schema_version 但已有业务表）
-- 在应用 baseline **之前**被明确拒绝；程序不会自动删数据（见 migrate.go）。
--
-- 注释约定：只描述**现在是什么、为什么**（不变量，不是迁移史）。
-- A2 纪律：全文（含注释）不得出现可执行命令类概念的那几个英文子串（见 Makefile）。

-- +migration 1 baseline

-- 用户（Passkey 无密码）。首个注册用户自动成为 admin（契约 §2.1）。
-- username 唯一；role = admin | member；disabled=1 表示超管禁用了这个用户（禁止登录）。
-- password_hash / password_updated_at 是密码登录：只存 argon2id PHC 串，空串 = 未设置。
CREATE TABLE IF NOT EXISTS users (
    id                   TEXT PRIMARY KEY,
    username             TEXT NOT NULL UNIQUE,
    display_name         TEXT NOT NULL DEFAULT '',
    role                 TEXT NOT NULL DEFAULT 'member',
    disabled             INTEGER NOT NULL DEFAULT 0,
    password_hash        TEXT NOT NULL DEFAULT '',
    password_updated_at  INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL
);

-- 按角色取用户（管理员列表 / 权限检查）。
CREATE INDEX IF NOT EXISTS idx_users_role ON users(role);

-- Passkey 凭证（一个用户可有多个）。
-- backup_eligible 必须持久化认证器上报的 BackupEligible flag：登录校验时
-- 标志不一致会直接拒绝（Apple 系 passkey 一律 backup eligible）。
CREATE TABLE IF NOT EXISTS passkeys (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    public_key      BLOB NOT NULL,
    sign_count      INTEGER NOT NULL DEFAULT 0,
    name            TEXT NOT NULL DEFAULT '',
    transports      TEXT NOT NULL DEFAULT '[]',
    backup_eligible INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER
);

-- 登录会话（id 就是下发给浏览器的 token）。
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_name TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);

-- API Key（只存 argon2id 哈希，明文只在创建时返回一次）。
CREATE TABLE IF NOT EXISTS api_keys (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    key_hash     TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT '[]',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_apikeys_user ON api_keys(user_id);

-- 接收设备（Web Push 订阅）——属于用户，不属于命名空间。
CREATE TABLE IF NOT EXISTS devices (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name           TEXT NOT NULL DEFAULT '',
    platform       TEXT NOT NULL DEFAULT '',
    enabled        INTEGER NOT NULL DEFAULT 1,
    event_scope    TEXT NOT NULL DEFAULT 'final',
    tags           TEXT NOT NULL DEFAULT '[]',
    endpoint       TEXT NOT NULL UNIQUE,
    p256dh         TEXT NOT NULL,
    auth           TEXT NOT NULL,
    user_agent     TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    last_active    INTEGER,
    last_delivered INTEGER
);

CREATE INDEX IF NOT EXISTS idx_devices_user ON devices(user_id);

-- CLI 设备授权会话（device authorization flow）。
CREATE TABLE IF NOT EXISTS cli_auth_sessions (
    id               TEXT PRIMARY KEY,
    secret_hash      TEXT NOT NULL,
    user_code        TEXT NOT NULL UNIQUE,
    device_name      TEXT NOT NULL,
    kind             TEXT NOT NULL DEFAULT 'apikey',
    device_hint      TEXT NOT NULL DEFAULT '',
    scopes_requested TEXT NOT NULL,
    scopes_granted   TEXT,
    status           TEXT NOT NULL,
    user_id          TEXT,
    key_id           TEXT,
    created_at       INTEGER NOT NULL,
    expires_at       INTEGER NOT NULL,
    consumed_at      INTEGER
);

CREATE INDEX IF NOT EXISTS idx_cli_auth_expires ON cli_auth_sessions(expires_at);

-- 恢复码（只存 argon2id PHC 哈希；明文只在签发时返回一次）。
CREATE TABLE IF NOT EXISTS recovery_codes (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL,
    prefix      TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    used_at     INTEGER
);

CREATE INDEX IF NOT EXISTS idx_recovery_codes_user   ON recovery_codes(user_id);
CREATE INDEX IF NOT EXISTS idx_recovery_codes_prefix ON recovery_codes(prefix);

-- 命名空间：唯一的硬隔离边界。
-- resource_version 是写并发令牌（CAS）：每次写入 +1，与内容版本无关。
CREATE TABLE IF NOT EXISTS namespaces (
    id               TEXT PRIMARY KEY,
    slug             TEXT NOT NULL,
    name             TEXT NOT NULL,
    resource_version INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL,
    UNIQUE (slug)
);

CREATE INDEX IF NOT EXISTS idx_namespaces_created ON namespaces(created_at);

-- 命名空间默认值（config.md §3）：vars 只读名字解析，机器事实不进这里。
-- revision 是 namespaceVarsRevision（配置生效版本），resource_version 是 CAS 令牌。
CREATE TABLE IF NOT EXISTS namespace_configs (
    namespace_id     TEXT PRIMARY KEY REFERENCES namespaces(id) ON DELETE CASCADE,
    vars             TEXT NOT NULL DEFAULT '{}',
    revision         INTEGER NOT NULL DEFAULT 0,
    resource_version INTEGER NOT NULL DEFAULT 1,
    updated_at       INTEGER NOT NULL
);

-- 中枢系统配置（保留期；config.md §3.6）。单行表：id 恒为 1。
CREATE TABLE IF NOT EXISTS system_config (
    id                      INTEGER PRIMARY KEY CHECK (id = 1),
    retention_event_ttl_sec INTEGER NOT NULL DEFAULT 2592000,
    retention_task_ttl_sec  INTEGER NOT NULL DEFAULT 2592000,
    retention_result_ttl_sec INTEGER NOT NULL DEFAULT 604800,
    resource_version        INTEGER NOT NULL DEFAULT 1,
    updated_at              INTEGER NOT NULL
);

-- 节点（集群级对象，不绑命名空间）。name 是用户可读名（集群内唯一）。
-- tags 是节点池标签；status = online | offline | draining。
-- token_hash 存 sha256，明文只在创建时返回一次。
-- max_instances 是实例数硬顶；max_concurrency 是节点级并发上限。
-- agent_version / agent_commit 是 hello 自报的版本事实（空串 = 未知）。
-- agent_upgrade 是最近一次升级尝试的状态 JSON。
CREATE TABLE IF NOT EXISTS nodes (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    tags             TEXT NOT NULL DEFAULT '[]',
    status           TEXT NOT NULL DEFAULT 'offline',
    token_hash       TEXT NOT NULL,
    max_instances    INTEGER NOT NULL DEFAULT 20,
    max_concurrency  INTEGER NOT NULL DEFAULT 8,
    last_seen_at     INTEGER,
    agent_version    TEXT NOT NULL DEFAULT '',
    agent_commit     TEXT NOT NULL DEFAULT '',
    agent_upgrade    TEXT NOT NULL DEFAULT '{}',
    resource_version INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL,
    UNIQUE (name)
);

-- 中枢节点默认值（config.md §4.1）：机器级、跨 ns 共用。
-- namespaces 是授权列表（与节点申报取交集）；vars/limits 是逐键默认值。
-- revision 是 configRevision（配置生效版本）；applied_* 是节点回报的快照。
CREATE TABLE IF NOT EXISTS node_configs (
    node_id          TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    namespaces       TEXT NOT NULL DEFAULT '["*"]',
    vars             TEXT NOT NULL DEFAULT '{}',
    limits           TEXT NOT NULL DEFAULT '{}',
    revision         INTEGER NOT NULL DEFAULT 0,
    resource_version INTEGER NOT NULL DEFAULT 1,
    applied_revision INTEGER NOT NULL DEFAULT 0,
    applied_hash     TEXT NOT NULL DEFAULT '',
    applied_at       INTEGER,
    updated_at       INTEGER NOT NULL
);

-- 并发槽位：节点申报，中枢 upsert。slot 是 0..N-1；busy_task 是当前任务 id。
CREATE TABLE IF NOT EXISTS node_instances (
    id          TEXT PRIMARY KEY,
    node_id     TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    slot        INTEGER NOT NULL,
    concurrency INTEGER NOT NULL DEFAULT 1,
    busy_task   TEXT,
    alive       INTEGER NOT NULL DEFAULT 1,
    updated_at  INTEGER NOT NULL,
    UNIQUE (node_id, slot)
);

-- 能力版本（中枢侧）：一行 = 一个 name@version。
--
-- spec 是**正文**的 canonical JSON（含 next；next 不入 digest 但必须存，
-- 它是路由的原始事实）。digest 是部署身份（含 version），fold_key 是折叠键
-- （不含 version / next / displayName，省略 version 时按它判「是不是同一行」）。
-- installed_from 是首次安装来源（安装回执 id；空 = 手建，复用不覆盖）。
-- resource_version 是写并发令牌；display_name 纯展示，不进 digest。
CREATE TABLE IF NOT EXISTS capability_versions (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    display_name     TEXT NOT NULL DEFAULT '',
    version          INTEGER NOT NULL,
    spec             TEXT NOT NULL,
    digest           TEXT NOT NULL,
    fold_key         TEXT NOT NULL,
    installed_from   TEXT NOT NULL DEFAULT '',
    resource_version INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL,
    UNIQUE (namespace_id, name, version),
    UNIQUE (namespace_id, digest)
);

CREATE INDEX IF NOT EXISTS idx_cap_name_ver
    ON capability_versions(namespace_id, name, version);

-- 折叠键查找（省略 version 的创建：同 ns + name + 同内容 → 复用已有行）。
CREATE INDEX IF NOT EXISTS idx_cap_fold_key
    ON capability_versions(namespace_id, name, fold_key);

-- 边的派生投影：能力边来自 spec.next，源入口边来自源 targets。用户不直接写边。
-- 只有四列身份 + created_at：没有端口、没有映射、没有优先级。
-- from_is_source=0 时 from_ref 是能力名；=1 时 from_ref 是源 id。
CREATE TABLE IF NOT EXISTS edges (
    namespace_id   TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    from_is_source INTEGER NOT NULL DEFAULT 0,
    from_ref       TEXT NOT NULL,
    to_ref         TEXT NOT NULL,
    created_at     INTEGER NOT NULL,
    PRIMARY KEY (namespace_id, from_is_source, from_ref, to_ref)
);

-- 前缀查下游（按上游取全部出边；读取时按声明顺序在 Go 侧恢复）。
CREATE INDEX IF NOT EXISTS idx_edges_from
    ON edges(namespace_id, from_is_source, from_ref);

-- 事件：只存不解释的原始输入。payload 原样保存；delivery_id 是调用方幂等键。
-- reason 是稳定机器码（默认 Ingested）。
CREATE TABLE IF NOT EXISTS events (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    trace_id     TEXT NOT NULL,
    source       TEXT NOT NULL,
    type         TEXT NOT NULL,
    payload      TEXT NOT NULL DEFAULT '{}',
    delivery_id  TEXT,
    reason       TEXT NOT NULL DEFAULT 'Ingested',
    created_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_trace
    ON events(namespace_id, trace_id);

-- 去重记录：唯一性只对非 NULL 的 delivery_id 生效（窗口内同 id 不重复派发）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_delivery
    ON events(namespace_id, source, delivery_id)
    WHERE delivery_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_events_retention
    ON events(namespace_id, created_at);

-- 任务：一个**逻辑 task 一行**。attempt 只是计数器（lease.grant 生效时 +1）。
-- waiting_reason ∈ waiting_for_slot | retry_backoff | waiting_for_config。
-- lease_id / lease_until / attempt_token_hash 是执行授权事实。
-- reported=1 表示节点已回报终态；summary/artifacts/error 是结果。
CREATE TABLE IF NOT EXISTS tasks (
    id                 TEXT PRIMARY KEY,
    namespace_id       TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    trace_id           TEXT NOT NULL,
    parent_task_id     TEXT,
    capability_id      TEXT NOT NULL,
    capability         TEXT NOT NULL,
    version            INTEGER NOT NULL,
    digest             TEXT NOT NULL DEFAULT '',
    params             TEXT NOT NULL DEFAULT '{}',
    phase              TEXT NOT NULL DEFAULT 'pending',
    node_id            TEXT,
    node_name          TEXT NOT NULL DEFAULT '',
    config_revision    INTEGER NOT NULL DEFAULT 0,
    attempt            INTEGER NOT NULL DEFAULT 0,
    max_attempts       INTEGER NOT NULL DEFAULT 3,
    lease_id           TEXT NOT NULL DEFAULT '',
    lease_until        INTEGER,
    attempt_token_hash TEXT NOT NULL DEFAULT '',
    waiting_reason     TEXT NOT NULL DEFAULT '',
    next_attempt_at    INTEGER,
    reported           INTEGER NOT NULL DEFAULT 0,
    summary            TEXT NOT NULL DEFAULT '',
    artifacts          TEXT NOT NULL DEFAULT '[]',
    error              TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    started_at         INTEGER,
    finished_at        INTEGER
);

CREATE INDEX IF NOT EXISTS idx_tasks_trace
    ON tasks(namespace_id, trace_id);
CREATE INDEX IF NOT EXISTS idx_tasks_parent
    ON tasks(namespace_id, parent_task_id);
CREATE INDEX IF NOT EXISTS idx_tasks_lease
    ON tasks(phase, lease_until);
CREATE INDEX IF NOT EXISTS idx_tasks_node
    ON tasks(node_id, phase);
CREATE INDEX IF NOT EXISTS idx_tasks_retention
    ON tasks(namespace_id, phase, finished_at);
CREATE INDEX IF NOT EXISTS idx_tasks_ns_created
    ON tasks(namespace_id, created_at, id);

-- 背压信号：取最早一条 pending（部分索引只收 pending 行）。
CREATE INDEX IF NOT EXISTS idx_tasks_pending_created_at
    ON tasks(created_at) WHERE phase = 'pending';

-- emit 幂等记录：同一逻辑任务内同 emit_id 只派发一次，回放同一份结果。
-- emit_id 为 NULL 时每次都新派发（部分唯一索引只对非 NULL 生效）。
CREATE TABLE IF NOT EXISTS emits (
    id         TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    emit_id    TEXT,
    to_ref     TEXT NOT NULL,
    data       TEXT NOT NULL DEFAULT '{}',
    result     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_emits_task_emit
    ON emits(task_id, emit_id) WHERE emit_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_emits_task
    ON emits(task_id);

-- 部署：「某节点持有某 digest」这条事实。人工部署写，节点回报更新 state。
-- state: pending | applied | failed | local。
CREATE TABLE IF NOT EXISTS deployments (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    digest        TEXT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'pending',
    reason        TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (node_id, capability_id, digest)
);

CREATE INDEX IF NOT EXISTS idx_deploy_node ON deployments(node_id);
CREATE INDEX IF NOT EXISTS idx_deploy_capability ON deployments(capability_id);

-- 预案：一份清单（能力 / 源），不产生任务、不是所有权。
-- files 是 canonical JSON：路径 → 文件内容（含 playbook.yaml 原文）。
-- 说明列叫 descr：A2 正则扫全文，那个英文单词里恰好含被禁子串。
CREATE TABLE IF NOT EXISTS playbooks (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    slug             TEXT NOT NULL,
    name             TEXT NOT NULL,
    descr            TEXT NOT NULL DEFAULT '',
    version          INTEGER NOT NULL DEFAULT 1,
    files            TEXT NOT NULL DEFAULT '{}',
    source           TEXT NOT NULL DEFAULT '',
    resource_version INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    UNIQUE (namespace_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_playbooks_ns ON playbooks(namespace_id, created_at);

-- 安装回执：每次 install 一行（dryRun 不写）。values 是有效 values，
-- values_digest 对未打码的值取哈希；receipt 是回执原文。
-- 回执被 installed_from 引用时延长保留（不允许悬空引用）。
CREATE TABLE IF NOT EXISTS playbook_installs (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    playbook_id   TEXT NOT NULL REFERENCES playbooks(id) ON DELETE CASCADE,
    "values"      TEXT NOT NULL DEFAULT '{}',
    values_digest TEXT NOT NULL,
    receipt       TEXT NOT NULL DEFAULT '{}',
    source        TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_playbook_installs_lookup
    ON playbook_installs(namespace_id, playbook_id, values_digest, created_at);

-- 源（触发器）：事件的入口实例，不是能力。kind 只允许注册表里的项
-- （本期 cron / webhook / manual，判定在应用层）。
-- config 是按 kind 解释的 JSON；targets 是 [{capability, params}]。
-- installed_from 是首次安装来源（复用不覆盖）。
CREATE TABLE IF NOT EXISTS sources (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    kind             TEXT NOT NULL,
    config           TEXT NOT NULL DEFAULT '{}',
    targets          TEXT NOT NULL DEFAULT '[]',
    enabled          INTEGER NOT NULL DEFAULT 1,
    last_fired_at    INTEGER,
    next_fire_at     INTEGER,
    installed_from   TEXT NOT NULL DEFAULT '',
    resource_version INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL,
    UNIQUE (namespace_id, name)
);

CREATE INDEX IF NOT EXISTS idx_sources_ns ON sources(namespace_id, created_at);
CREATE INDEX IF NOT EXISTS idx_sources_due ON sources(enabled, next_fire_at);
