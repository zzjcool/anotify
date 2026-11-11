-- Anotify · SQLite schema（v4 baseline，单一事实源）
-- 连接初始化 PRAGMA（在代码里设置）：
--   journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000; foreign_keys=ON;
--
-- 本文件同时是**迁移脚本**：按 `-- +migration N name` 切块，
-- store.migrateWith 逐块应用，schema_version 记账（见 migrate.go）。
--
-- v4 开发期直接重建：只有一块 `-- +migration 1 baseline`，没有历史增量块。
-- 旧库（有 schema_version 记账却没有 v4 布局标记、有未知布局标记、或没有记账但已有用户 schema 对象）
-- 在应用 baseline **之前**被明确拒绝；视图/触发器/索引也意味着非空。
-- 只排除字面 sqlite_ 内部前缀（sqliteXkeep 是用户对象），程序不会自动删数据（见 migrate.go）。
--
-- 布局格式与记账是两件事（migrate.go 的 layoutFormatV4）：
--   schema_version=1 说明「baseline 块跑过」——v3 的库同样记着 1；
--   PRAGMA user_version=4 说明「这个文件的表结构是 v4 布局」，只由一次完整的
--   baseline 应用写成：DDL、记账行、标记落在同一个事务里，要么全成、要么全空。
--   所以 baseline 块里只能放可整体回滚的 DDL，不许有事务外副作用。
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
-- facts 是 hello 上报的内置机器事实（hostIP/hostname/os/arch/cpu/workRoot/homeDir）。
-- declared_capabilities 是 hello.capabilities[]：中枢据此回答 CommandMissing。
-- declared_namespaces 是本机申报的 ns 列表（与 node_configs.namespaces 取交集）。
CREATE TABLE IF NOT EXISTS nodes (
    id                    TEXT PRIMARY KEY,
    name                  TEXT NOT NULL,
    tags                  TEXT NOT NULL DEFAULT '[]',
    status                TEXT NOT NULL DEFAULT 'offline',
    token_hash            TEXT NOT NULL,
    max_instances         INTEGER NOT NULL DEFAULT 20,
    max_concurrency       INTEGER NOT NULL DEFAULT 8,
    last_seen_at          INTEGER,
    agent_version         TEXT NOT NULL DEFAULT '',
    agent_commit          TEXT NOT NULL DEFAULT '',
    agent_upgrade         TEXT NOT NULL DEFAULT '{}',
    facts                 TEXT NOT NULL DEFAULT '{}',
    declared_capabilities TEXT NOT NULL DEFAULT '[]',
    declared_namespaces   TEXT NOT NULL DEFAULT '[]',
    resource_version      INTEGER NOT NULL DEFAULT 1,
    created_at            INTEGER NOT NULL,
    UNIQUE (name)
);

-- 中枢节点默认值（config.md §4.1）：机器级、跨 ns 共用。
-- namespaces 是授权列表（与节点申报取交集）；vars/limits 是逐键默认值。
-- local_vars/local_limits 是节点 hello 上报的本机快照（只用于匹配与展示，不参与 apply）。
-- revision 是 configRevision（配置生效版本）；applied_* 是节点回报的快照。
CREATE TABLE IF NOT EXISTS node_configs (
    node_id          TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    namespaces       TEXT NOT NULL DEFAULT '["*"]',
    vars             TEXT NOT NULL DEFAULT '{}',
    limits           TEXT NOT NULL DEFAULT '{}',
    local_vars       TEXT NOT NULL DEFAULT '{}',
    local_limits     TEXT NOT NULL DEFAULT '{}',
    revision         INTEGER NOT NULL DEFAULT 0,
    resource_version INTEGER NOT NULL DEFAULT 1,
    applied_revision INTEGER NOT NULL DEFAULT 0,
    applied_hash     TEXT NOT NULL DEFAULT '',
    applied_at       INTEGER,
    updated_at       INTEGER NOT NULL
);

-- 并发槽位：节点申报，中枢 upsert。slot 是 0..N-1（每槽最多一个能力进程）。
--
-- 物理预算只有两项：机器级 limits.total/perCapability（预算）与真实 slot 记录（容量）。
-- 不再有 per-slot concurrency 乘数：v4 一次执行占一个 slot（一个进程）。
--
-- busy_task 是占用该槽的逻辑任务；busy_attempt / busy_lease_id 是**具体某一次执行**
-- （task_attempts 的 (task_id, attempt) 与它那一代的 leaseId）的绑定。
-- 为什么必须绑到 attempt：同一逻辑任务换 attempt 重试时可能同时占着旧 attempt 的槽
-- （旧进程还没回收），释放必须按 (instance, task, attempt) 精确判定，不能按 task_id
-- 一把清——那会把旧进程还在用的槽放给下一个任务（同一槽跑两条）。
--
-- busy_lease_id 为空是 **reservation**（节点已接住、还没 grant 的候选）与执行的唯一判别。
-- reservation 没有 lease，因此它还必须带候选身份与有界窗口（busy_assignment_id /
-- busy_expires_at）：否则旧 assignment 的观测可能清掉/复用新候选的预约，预算也无法
-- 精确排除「本次 grant 自己那条同槽预约」。执行绑定把这两个字段清空（''/0）：
-- 任何到期规则都不允许清一条真实执行（unreaped 只能由精确绑定的事实回收）。
CREATE TABLE IF NOT EXISTS node_instances (
    id                 TEXT PRIMARY KEY,
    node_id            TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    slot               INTEGER NOT NULL,
    busy_task          TEXT,
    busy_attempt       INTEGER NOT NULL DEFAULT 0,
    busy_lease_id      TEXT NOT NULL DEFAULT '',
    busy_assignment_id TEXT NOT NULL DEFAULT '',
    busy_expires_at    INTEGER NOT NULL DEFAULT 0,
    alive              INTEGER NOT NULL DEFAULT 1,
    updated_at         INTEGER NOT NULL,
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
    deleted_at       INTEGER
);

-- 仅可见资源占用名字/版本与内容身份；已钉住的历史正文保留原始 UID。
CREATE UNIQUE INDEX IF NOT EXISTS idx_cap_live_name_ver
    ON capability_versions(namespace_id, name, version) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_cap_live_digest
    ON capability_versions(namespace_id, digest) WHERE deleted_at IS NULL;

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
    -- receipt 是接收事务写下的**当时**回执（event/trace/task ids + pruned facts）。
    -- 窗口内重复投递只能回这份持久事实，不许用当前可变资源重算一遍。
    receipt      TEXT NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_trace
    ON events(namespace_id, trace_id);

CREATE INDEX IF NOT EXISTS idx_events_retention
    ON events(namespace_id, created_at);

-- 投递去重记录（source.md §3.3 D7/D12）：去重窗**独立于事件实体**。
--
-- 为什么必须单独一张表：同一次投递的去重窗（accepted_at + expires_at）与
-- 事件行的保留期是两件事——一个事件可能因为被 task 引用而活得更久，也可能
-- 早于窗口被清。绑在 events 上的唯一索引会逼出一个二选一：
--   · 旧事件还在 → 同 id 的新投递永远收不进来（违反「窗口过期后可再接收」）；
--   · 旧事件删了 → 唯一索引跟着消失，窗口内的重复投递反而收进来了。
-- 拆表后：窗口判定只看 expires_at；过期后同 id 得到**新 eventId** 并刷新本行记录，
-- 旧事件实体不受影响（不因为去重而删实体）。
--
-- source 存的是**源的稳定 id**（sources.id，`src_...`），不是源名（D7/D12）：
-- 源可以删掉后用同名重建，那是一次新的源身份——拿名字当去重键会把「旧源的窗口」
-- 与「新源的投递」混成一条。接收事务在同一事务里按 (namespace_id, 源名) 解析出
-- 稳定 id 再写这里；解析不到源行时用 `name:<源名>` 前缀键（与 `src_` id 不相交）。
-- 事件行上的 source 仍是**人类可读的源名**（列表/详情要给人看），两者不能混用。
-- expires_at = 0 表示永久窗口（eventTtlSec=0 时的语义）。
CREATE TABLE IF NOT EXISTS event_deliveries (
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    source       TEXT NOT NULL,
    delivery_id  TEXT NOT NULL,
    event_id     TEXT NOT NULL,
    accepted_at  INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    PRIMARY KEY (namespace_id, source, delivery_id)
);

CREATE INDEX IF NOT EXISTS idx_event_deliveries_expiry
    ON event_deliveries(expires_at);

-- 任务：一个**逻辑 task 一行**。attempt 只是计数器（lease.grant 生效时 +1）。
-- phase ∈ pending | running | succeeded | failed | cancelled | abandoned。
-- waiting_reason ∈ waiting_for_slot | retry_backoff | waiting_for_config（只有 pending 有意义）。
-- lease_id / lease_until / attempt_token_hash 是执行授权事实（token 只存 sha256）。
-- lease_instance_id 是**这一次 attempt 绑定的物理槽位**（node_instances.id）：
-- 同一逻辑任务换 attempt 重试时可能同时占着旧 attempt 的槽（旧进程未回收），
-- 释放只能按「node + 这个 attempt 绑的槽」精确释放，不能按 task_id 一把清掉
-- （否则旧 attempt 的进程还在跑，槽位就被新任务用了）。
-- assignment_id / proposed_attempt 是「这次派给谁」的候选事实（assign 时不授执行权）。
-- assignment_expires_at 是这次候选的**统一有界窗口**（初值 30s，与节点 reservation 同一
-- 截止时刻）：窗口失效后不再接受对应 slot 的 grant；新派发会写新 assignment。
-- idempotent 在派发时从 spec.lifecycle 钉住（重试判定只看这一列，不回读能力）。
-- namespace_vars / namespace_vars_revision 是**任务被接受时**钉住的命名空间默认输入快照
-- 与它的 namespaceVarsRevision（D2）：排队期间 ns 配置更新不影响已接受的任务，
-- 换节点重试沿用同一份快照；值只在内部执行输入里用，不进公共 DTO（json:"-"）。
-- cancel_reason / cancel_requested_at / stop_intent 记录停止事实：stop_intent 是**意图**
-- （cancel | abandon，空 = 无停止意图），cancel_reason 是人话原因或默认封闭码。终态相位
-- 由 stop_intent 决定（不从 reason 字面反推）；running 时要等 cancel.ack 的
-- processGroupReaped 才落终态，pending 时直接终态。
-- reported=1 表示节点已回报终态；summary/artifacts/error 是结果。
CREATE TABLE IF NOT EXISTS tasks (
    id                 TEXT PRIMARY KEY,
    namespace_id       TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    trace_id           TEXT NOT NULL,
    parent_task_id     TEXT,
    -- capability_id 只有「未解析派发失败」分支允许 NULL（见表尾两条 CHECK）：
    -- 正常任务仍钉住真实能力行 UID；NULL 行没有 Node/lease/grant/结果事实。
    capability_id      TEXT REFERENCES capability_versions(id) ON DELETE RESTRICT,
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
    assignment_id      TEXT NOT NULL DEFAULT '',
    proposed_attempt   INTEGER NOT NULL DEFAULT 0,
    assignment_expires_at INTEGER,
    idempotent         INTEGER NOT NULL DEFAULT 0,
    namespace_vars          TEXT NOT NULL DEFAULT '{}',
    namespace_vars_revision INTEGER NOT NULL DEFAULT 0,
    lease_id           TEXT NOT NULL DEFAULT '',
    lease_until        INTEGER,
    lease_instance_id  TEXT NOT NULL DEFAULT '',
    attempt_token_hash TEXT NOT NULL DEFAULT '',
    waiting_reason     TEXT NOT NULL DEFAULT '',
    next_attempt_at    INTEGER,
    cancel_reason      TEXT NOT NULL DEFAULT '',
    cancel_requested_at INTEGER,
    -- stop_intent 是停止意图（cancel | abandon | 空；内部列，不进公共 DTO）。
    stop_intent        TEXT NOT NULL DEFAULT '',
    reported           INTEGER NOT NULL DEFAULT 0,
    summary            TEXT NOT NULL DEFAULT '',
    artifacts          TEXT NOT NULL DEFAULT '[]',
    error              TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    started_at         INTEGER,
    finished_at        INTEGER,
    result_json        TEXT NOT NULL DEFAULT '',
    result_accepted_at INTEGER,
    -- 未解析派发失败（downstream_missing）的唯一合法形状：failed、attempt=0、
    -- 没有解析出的 digest、没有 Node/租约/token/候选/结果事实。
    -- 末尾几列是**伪事实面**：它们只可能来自 Node 过程或结果接受，而未解析失败行
    -- 从没执行过——放行它们等于允许「从没跑过却有结果」的行：
    --   summary/artifacts：Node 过程产出的摘要/产物，这条路径不可能有；
    --   result_accepted_at：从来没有结果被接受过（retention 还拿它当可信保留时间）；
    --   finished_at：终态失败行必须有**真实 server 完成时刻**，否则 retention 的
    --     COALESCE(result_accepted_at, finished_at) 为 NULL，这条行永远不被回收；
    --   version：S1-D1 只允许 0（请求未声明版本）或明确声明的正版本。
    CONSTRAINT tasks_unresolved_failure_shape CHECK (
        capability_id IS NOT NULL OR (
            phase = 'failed' AND attempt = 0
            AND digest = '' AND error = 'downstream_missing'
            AND node_id IS NULL AND node_name = ''
            AND lease_id = '' AND lease_until IS NULL AND lease_instance_id = ''
            AND attempt_token_hash = ''
            AND assignment_id = '' AND proposed_attempt = 0 AND assignment_expires_at IS NULL
            AND waiting_reason = '' AND next_attempt_at IS NULL
            AND cancel_reason = '' AND cancel_requested_at IS NULL
            AND reported = 0 AND started_at IS NULL AND result_json = ''
            AND summary = '' AND artifacts = '[]' AND result_accepted_at IS NULL
            AND finished_at IS NOT NULL AND version >= 0
        )
    ),
    -- 已解析任务必须钉住真实版本 + digest（不允许「有 UID 但没有解析事实」的行）。
    CONSTRAINT tasks_resolved_pin_shape CHECK (
        capability_id IS NULL OR (digest <> '' AND version > 0)
    )
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

-- 每次执行（attempt）一行：逻辑任务仍然只有 tasks 一行，attempt 是它的**执行事实**。
--
-- 它不是 child task（parent_task_id 只表示业务 emit 的父子关系），也不参与路由。
-- 为什么必须持久化机器快照：机器级输入（local > node 合并后的 vars/limits）在 grant
-- 那一瞬确定，本次 attempt 期间后续 config 更新 / hello 上报不得改动它（D2）。
-- TaskInputs 只读这张表，不回读当前 node_configs——那会让同一 attempt 中途换值。
-- namespace 默认值仍由 tasks.namespace_vars 在任务接受时钉住（跨 attempt 不变，
-- 本表只记它当时的 revision 作诊断）。
--
-- machine_vars / machine_limits / machine_var_sources 是**非敏感**机器值：
-- 凭证、envPassthrough 名单、namespace 值都不进来（D5）。config_hash 只由共享
-- protocol.MachineConfigHash 计算（canonical JSON 只有 vars + limits 两个键，
-- 不含 revision / namespace / token / passthrough）。
--
-- instance_id / slot 是这次 attempt 实际占用的物理槽（node_instances）；
-- lease_id / attempt_token_hash 是这一代的授权摘要（token 只存 sha256）。
-- reaped_at 是「该 attempt 的进程组已被可信事实证明回收」的落点；reaped_at IS NULL
-- 的 attempt 是任务 GC 的硬保留条件（物理事实不能随任务行级联消失）。
-- reclaim_reason 记录是哪一种事实（result / cancel_ack / node_instance_idle …）。
-- phase / error 是 attempt 级诊断，不是任务状态机（任务相位仍只有 tasks.phase）。
CREATE TABLE IF NOT EXISTS task_attempts (
    task_id                 TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt                 INTEGER NOT NULL,
    node_id                 TEXT NOT NULL,
    node_name               TEXT NOT NULL DEFAULT '',
    instance_id             TEXT NOT NULL DEFAULT '',
    slot                    INTEGER NOT NULL DEFAULT -1,
    lease_id                TEXT NOT NULL DEFAULT '',
    attempt_token_hash      TEXT NOT NULL DEFAULT '',
    config_revision         INTEGER NOT NULL DEFAULT 0,
    config_hash             TEXT NOT NULL DEFAULT '',
    machine_vars            TEXT NOT NULL DEFAULT '{}',
    machine_var_sources     TEXT NOT NULL DEFAULT '{}',
    machine_limits          TEXT NOT NULL DEFAULT '{}',
    namespace_vars_revision INTEGER NOT NULL DEFAULT 0,
    phase                   TEXT NOT NULL DEFAULT '',
    error                   TEXT NOT NULL DEFAULT '',
    started_at              INTEGER,
    finished_at             INTEGER,
    reaped_at               INTEGER,
    reclaim_reason          TEXT NOT NULL DEFAULT '',
    capability_id           TEXT NOT NULL DEFAULT '',
    artifacts               TEXT NOT NULL DEFAULT '[]',
    result_json             TEXT NOT NULL DEFAULT '',
    result_accepted_at      INTEGER,
    PRIMARY KEY (task_id, attempt)
);

-- 历史 attempt 定位（G2 的 previous 日志 / 运维按节点查执行事实）。
CREATE INDEX IF NOT EXISTS idx_task_attempts_node
    ON task_attempts(node_id, attempt);

-- GC 硬保留条件：仍有未 reaped attempt 的任务行不能删（部分索引只收未回收行）。
CREATE INDEX IF NOT EXISTS idx_task_attempts_unreaped
    ON task_attempts(task_id) WHERE reaped_at IS NULL;

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
    display_name     TEXT NOT NULL DEFAULT '',
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
--
-- playbook_id 是**历史 id，不是外键**：删一个 playbook 清单不该连带删掉它装过的回执
-- （回执是「这次安装做了什么」的审计，也是 capability_versions/sources.installed_from
-- 指向的锚点）。带 ON DELETE CASCADE 的外键会让删包同时抹掉回执并留下悬挂引用；
-- 带 RESTRICT 的外键会让删包永远失败。所以这里不放 live-parent 外键，
-- 而是把「当时是谁」**快照**进回执自己：原 id + 原 slug + 原命名空间 slug。
-- 快照好之后，playbook 行消失也不影响读回执（也不会把同名重建的新包认成来源）。
CREATE TABLE IF NOT EXISTS playbook_installs (
    id                TEXT PRIMARY KEY,
    namespace_id      TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    namespace_slug    TEXT NOT NULL DEFAULT '',
    playbook_id       TEXT NOT NULL DEFAULT '',
    playbook_slug     TEXT NOT NULL DEFAULT '',
    "values"          TEXT NOT NULL DEFAULT '{}',
    values_digest     TEXT NOT NULL,
    receipt           TEXT NOT NULL DEFAULT '{}',
    source            TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL
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
    display_name     TEXT NOT NULL DEFAULT '',
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

-- System-managed object identities never derive from a logical path or shared hash.
-- Objects outlive task metadata so namespace/task deletion cannot lose GC work.
CREATE TABLE IF NOT EXISTS task_artifact_objects (
    id TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    expired_at INTEGER,
    purged_at INTEGER
);
CREATE TABLE IF NOT EXISTS task_artifact_uploads (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL,
    path TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    size INTEGER NOT NULL,
    object_id TEXT NOT NULL REFERENCES task_artifact_objects(id) ON DELETE RESTRICT,
    expires_at INTEGER NOT NULL,
    FOREIGN KEY (task_id, attempt) REFERENCES task_attempts(task_id, attempt) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS task_artifact_manifests (
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL,
    path TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    size INTEGER NOT NULL,
    object_id TEXT NOT NULL REFERENCES task_artifact_objects(id) ON DELETE RESTRICT,
    published_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    expired_at INTEGER,
    result_ref INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (task_id, attempt, path),
    FOREIGN KEY (task_id, attempt) REFERENCES task_attempts(task_id, attempt) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_task_artifact_objects ON task_artifact_manifests(object_id);
CREATE INDEX IF NOT EXISTS idx_task_artifact_upload_expiry ON task_artifact_uploads(expires_at);
