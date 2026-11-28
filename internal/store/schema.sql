-- Anotify · SQLite schema（v4 baseline，单一事实源）
-- 连接初始化 PRAGMA（在代码里设置）：
--   journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000; foreign_keys=ON;
--
-- 本文件同时是**迁移脚本**：按 `-- +migration N name` 切块，
-- store.migrateWith 逐块应用，schema_version 记账（见 migrate.go）。
--
-- 块序列：`1 baseline`（v4 的全新结构）+ 之后按编号追加的增量块（`2 …`）。
-- 每追加一块都要把 migrate.go 的 headVersion 一起 +1，并在 schema_version 里记账。
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

-- 能力开关（capability.docx §5.1 / capability lifecycle L-D1）。
--
-- **没有行 = enabled**：所以老库升级不需要任何回填，新建能力天然启用。
-- 主键是 (namespace_id, name) 而不是 capability_versions.id —— 开关是**名字**的属性：
-- 换版本不重置它，历史版本也不各带一个开关。
-- reason / updated_by 只记「最近一次操作是谁、为什么」（全 history 在 capability_audit）。
-- 删除能力时本行随 namespaces 级联消失；同名重建 = 新身份，从 enabled 开始。
CREATE TABLE IF NOT EXISTS capability_switches (
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    reason       TEXT NOT NULL DEFAULT '',
    updated_at   INTEGER NOT NULL,
    updated_by   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (namespace_id, name)
);

-- 能力操作审计（append-only）。
--
-- 为什么不复用 events：events 是「只存不解释的原始输入」，它的消费者是事件时间线、
-- 统计计数与 trace 全景，全都**没有 actor 概念**；把审批类记录塞进去会同时污染
-- 统计口径和「事件内容」的语义，而 events 也没有 actor 列可加。
-- detail 只存**摘要**（'{}' 默认）：试跑记 traceId + version，不存参数全文（参数里可能有业务数据）。
-- 本表只增不删：没有修改/删除端点，也不接保留期（写量 = 人工动作次数）。
CREATE TABLE IF NOT EXISTS capability_audit (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    action       TEXT NOT NULL CHECK (action IN ('enable', 'disable', 'test')),
    -- users.id；空串 = 非会话主体（命令行/系统）。
    actor_id     TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL
);

-- 倒序读最近操作（limit 生效）；与 ORDER BY created_at DESC, id DESC 同序。
CREATE INDEX IF NOT EXISTS idx_cap_audit_ns_name
    ON capability_audit(namespace_id, name, created_at DESC, id DESC);

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
-- origin 区分「外部投递」与「人工试跑」：'source' 覆盖全部存量行，不需要回填。
-- 不复用 source 列：source 存人类可读的源名（列表/详情要给人看），试跑没有源可写，
-- 写个固定名字又会跟「用户真的建了同名源」混成一类，且不能当稳定过滤判据。
CREATE TABLE IF NOT EXISTS events (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    trace_id     TEXT NOT NULL,
    source       TEXT NOT NULL,
    origin       TEXT NOT NULL DEFAULT 'source' CHECK (origin IN ('source', 'test')),
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
-- lease_instance_id 是**这一次 attempt 绑定的物理槽位**（node_instances.id）：换 attempt
-- 重试时可能同时占着旧 attempt 的槽，释放只能按「node + 这个 attempt 绑的槽」精确释放，
-- 不能按 task_id 一把清掉（否则旧进程还在跑，槽位就被新任务用了）。
-- assignment_id / proposed_attempt 是「这次派给谁」的候选事实（assign 时不授执行权）；
-- assignment_expires_at 是候选的**统一有界窗口**（30s，与节点 reservation 同一截止时刻）。
-- idempotent 在派发时从 spec.lifecycle 钉住（重试判定只看这一列，不回读能力）。
-- namespace_vars / namespace_vars_revision 是**任务被接受时**钉住的命名空间默认输入快照与
-- 它的 revision（D2）：排队期间的 ns 更新不影响已接受任务，换节点重试沿用同一份快照；
-- 只在内部执行输入里用，不进公共 DTO（json:"-"）。
-- cancel_reason / cancel_requested_at / stop_intent 记录停止事实：stop_intent 是**意图**
-- （cancel | abandon，空 = 无）；终态相位由 stop_intent 决定（不从 reason 字面反推），
-- running 要等 cancel.ack 的 processGroupReaped，pending 直接终态。
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

-- 能力 status.observed.tasks{total,failed} 的 24h 窗口计数：
-- (ns, 能力, 时间倒序) 三段 seek，不做全表扫（EXPLAIN 门禁见 capability_observed_test.go）。
-- 注意「最近一个逻辑任务」**不**走这条索引：既有 idx_tasks_ns_created 反向扫一段就
-- 满足 created_at DESC, id DESC（零排序），实测比走这条更快（会多一个 temp B-tree）。
CREATE INDEX IF NOT EXISTS idx_tasks_cap_created
    ON tasks(namespace_id, capability, created_at DESC, id);

-- 背压信号：取最早一条 pending（部分索引只收 pending 行）。
CREATE INDEX IF NOT EXISTS idx_tasks_pending_created_at
    ON tasks(created_at) WHERE phase = 'pending';

-- 每次执行（attempt）一行：逻辑任务仍只有 tasks 一行，attempt 是它的**执行事实**。
-- 它不是 child task（parent_task_id 只表示业务 emit 的父子），也不参与路由。
-- 机器快照必须持久化：机器级输入（local > node 合并后的 vars/limits）在 grant 那一瞬
-- 确定，本次 attempt 期间后续 config 更新 / hello 上报不得改动它（D2）；TaskInputs 只读
-- 这张表、不回读当前 node_configs。namespace 默认值仍由 tasks.namespace_vars 钉住，
-- 本表只记它当时的 revision 作诊断。
-- machine_vars / machine_limits / machine_var_sources 是**非敏感**机器值：凭证、
-- envPassthrough 名单、namespace 值都不进来（D5）；config_hash 只由共享
-- protocol.MachineConfigHash 计算（canonical JSON 只有 vars + limits）。
-- instance_id / slot 是这次 attempt 占用的物理槽；lease_id / attempt_token_hash 是这一代
-- 的授权摘要（token 只存 sha256）。reaped_at 是「进程组已被可信事实证明回收」的落点，
-- IS NULL 的 attempt 是任务 GC 的硬保留条件（物理事实不能随任务行级联消失）。
-- reclaim_reason 记录是哪一种事实（result / cancel_ack / node_instance_idle …）；
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
-- playbook_id 是**历史 id，不是外键**：删清单不该连带删它装过的回执（回执是审计，也是
-- capability_versions/sources.installed_from 指向的锚点）。CASCADE 会抹掉回执并留悬挂引用，
-- RESTRICT 会让删包永远失败，所以不放 live-parent 外键，而是把「当时是谁」**快照**进回执
-- 自己：原 id + 原 slug + 原命名空间 slug。快照好之后 playbook 行消失也不影响读回执
-- （也不会把同名重建的新包认成来源）。
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
-- （本期 cron / webhook / manual / poll，判定在应用层）。
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

-- 安装级身份：一个预案可以释放出多份互不影响的实例（一个 release 一套名字与 values）。
--
-- release 是**隔离边界**，不是标签：一条能力行 / 源行是被某次安装（installed_from）
-- 带出来的，那次安装的 release 就是这行的 owner。规划期若复用命中一行、而它是**另一个
-- release** 装出来的，安装必须整包拒绝（application 层 409 release_conflict），
-- 绝不静默认领别人的行。
--
-- 存量回填成 playbook_slug：历史回执都是「用默认 release 装的」，而默认 release 就是
-- 包名；回填成空串会让它们既归不了组，也与默认 release 对不上。
-- +migration 2 playbook_release
ALTER TABLE playbook_installs ADD COLUMN release TEXT NOT NULL DEFAULT '';
UPDATE playbook_installs SET release = playbook_slug WHERE release = '';
CREATE INDEX IF NOT EXISTS idx_playbook_installs_release
    ON playbook_installs(namespace_id, playbook_id, release, created_at);

-- 卸载按 release 撤对象，但不删回执：回执是审计快照，原文一个字都不改，
-- 只在列上记下卸载时刻（幂等 0 写与「已卸载的实例不进实例清单」都靠它）。
-- 归属判据仍是 installed_from；查询走上面的 idx_playbook_installs_release，不加索引。
-- +migration 3 playbook_uninstall
ALTER TABLE playbook_installs ADD COLUMN uninstalled_at INTEGER NOT NULL DEFAULT 0;

-- 包版本历史：PUT 把包体换掉时，旧的那份文件留在这里，于是「按 v1 再装一份」有东西可读。
--
-- 写入纪律（写侧唯一入口是 ReplacePlaybook）：内容没变就不写（同一个包体反复 PUT 不长历史）；
-- 内容变了但版本号没动，**覆盖**这个版本号那一行——一个版本号只对应一份包体，否则
-- 「按 v1 装」有两种答案。digest 与回执的 playbook.filesDigest 同一个算法（playbook.FilesDigest）。
--
-- 读路径不经过这张表：当前版本仍然以 playbooks.files 为准（每次 GET 都 join 历史是白付的钱）。
-- playbook_id 是**历史 id，不是外键**，与回执同一条纪律：删包不连带删历史（也不因此失败）。
-- +migration 4 playbook_versions
CREATE TABLE IF NOT EXISTS playbook_versions (
    playbook_id TEXT NOT NULL,
    version     INTEGER NOT NULL,
    files       TEXT NOT NULL DEFAULT '{}',
    digest      TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    created_by  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (playbook_id, version)
);

-- router 的结局表与决策凭据（#522）。outcomes 是**声明快照**（建任务那一刻钉住，不回读源）；
-- decision 是**判决事实**（一次且仅一次，`decision = ''` 是 CAS 闸）；*_token_hash /
-- *_expires_at 是一次性凭据（与 attempt token 同一条「只存摘要」纪律）。
-- +migration 5 router_outcomes
ALTER TABLE tasks ADD COLUMN outcomes TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN decision TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN decision_at INTEGER;
ALTER TABLE tasks ADD COLUMN decision_token_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN decision_expires_at INTEGER;
-- 拉取型源（#532）：轮询状态是服务端派生的事实，所以不进 config。游标只在整轮成功后前进
-- （失败重抓同一批、靠 delivery_id 幂等去重）。
-- +migration 6 poll_source
ALTER TABLE sources ADD COLUMN poll_cursor TEXT NOT NULL DEFAULT '';
ALTER TABLE sources ADD COLUMN last_poll_at INTEGER;
ALTER TABLE sources ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sources ADD COLUMN skipped_overlaps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sources ADD COLUMN last_error TEXT NOT NULL DEFAULT '';

-- 命名空间凭据（#532 的 secretRef 指向它）：值只写不读；写入口是 poll 的 config.secret 与 PUT …/secrets/{name}。
CREATE TABLE IF NOT EXISTS namespace_secrets (
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    value        TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (namespace_id, name)
);

-- 依赖索引（#534）：回执的 dependencies 不可查，而「这能力还有谁在用」必须在 SQL 侧看得见（锁判定只认 uninstalled_at = 0 的回执）。
-- +migration 7 playbook_requires
CREATE TABLE IF NOT EXISTS playbook_install_requires (
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    install_id   TEXT NOT NULL REFERENCES playbook_installs(id) ON DELETE CASCADE,
    capability   TEXT NOT NULL,
    version      INTEGER NOT NULL,
    PRIMARY KEY (install_id, capability)
);

CREATE INDEX IF NOT EXISTS idx_playbook_requires_capability
    ON playbook_install_requires(namespace_id, capability);

-- 安装时快照（#551）：回执里的 pinned 在 SQL 侧也要能查，与回执 JSON 同一事务双写；ref_kind ∈ capability / source，
-- obj_kind 只对源有意义、version / digest 只对能力有意义，seq 保住回执顺序；列名不用 kind（I3 只放行 sources / cli_auth_sessions）。
-- +migration 8 install_pinned
CREATE TABLE IF NOT EXISTS playbook_install_pinned (
    install_id   TEXT NOT NULL REFERENCES playbook_installs(id) ON DELETE CASCADE,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    ref_kind     TEXT NOT NULL,
    name         TEXT NOT NULL,
    obj_kind     TEXT NOT NULL DEFAULT '',
    version      INTEGER NOT NULL DEFAULT 0,
    digest       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (install_id, ref_kind, name)
);
CREATE INDEX IF NOT EXISTS idx_playbook_pinned_lookup
    ON playbook_install_pinned(namespace_id, ref_kind, name, version);

-- +migration 9 secret_audit
CREATE TABLE IF NOT EXISTS namespace_secret_audit (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    action       TEXT NOT NULL CHECK (action IN ('created', 'rotated', 'deleted')),
    -- users.id；空串 = 非会话主体（命令行/系统），不伪造 system 用户。
    actor_id     TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_namespace_secret_audit_ns
    ON namespace_secret_audit(namespace_id, name, created_at);
-- +migration 10 source_audit
CREATE TABLE IF NOT EXISTS source_audit (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    source_id    TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    action       TEXT NOT NULL CHECK (action IN ('source.polled_manually')),
    actor_id     TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_source_audit_source
    ON source_audit(namespace_id, source_id, created_at DESC, id DESC);

-- +migration 11 playbook_restore_audit
CREATE TABLE IF NOT EXISTS playbook_install_audit (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    playbook_id   TEXT NOT NULL DEFAULT '',
    playbook_slug TEXT NOT NULL DEFAULT '',
    release       TEXT NOT NULL DEFAULT '',
    install_id    TEXT NOT NULL DEFAULT '',
    action        TEXT NOT NULL CHECK (action IN ('install.restored')),
    actor_id      TEXT NOT NULL DEFAULT '',
    detail        TEXT NOT NULL DEFAULT '{}',
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_playbook_install_audit_release
    ON playbook_install_audit(namespace_id, playbook_slug, release, created_at DESC, id DESC);

-- +migration 12 task_retention_index
CREATE INDEX IF NOT EXISTS idx_tasks_retain_from
    ON tasks(COALESCE(result_accepted_at, CASE WHEN reported=0 THEN finished_at END));

-- +migration 13 capability_ref_indexes
-- #628：tombstone 回收判据与外键 RESTRICT 检查的三个引用列；计划断言见 retention_purge_index_test.go。
-- 不做部分索引：外键检查那条语句没有谓词，部分索引用不上。
CREATE INDEX IF NOT EXISTS idx_tasks_capability_id ON tasks(capability_id);
CREATE INDEX IF NOT EXISTS idx_task_attempts_capability ON task_attempts(capability_id);
CREATE INDEX IF NOT EXISTS idx_task_artifact_manifests_capability
    ON task_artifact_manifests(capability_id);
-- 产物对象行两段扫描各配一个部分索引：到期扫描（purged_at IS NULL，按 created_at,id 取前 N）
-- 与回收扫描（purged_at 非空）都以索引前缀为工作集；uploads 的 NOT EXISTS 复核靠 object_id 索引。
-- +migration 14 artifact_object_reclaim
-- 对象行两段扫描（到期 / 回收）各配一个部分索引：代价跟工作集走，不跟历史上传总数走（到期扫描的过滤与排序都落在索引上）。
CREATE INDEX IF NOT EXISTS idx_task_artifact_objects_live
    ON task_artifact_objects(created_at, id) WHERE purged_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_task_artifact_objects_purged
    ON task_artifact_objects(purged_at, id) WHERE purged_at IS NOT NULL;
-- 两个扫描的 NOT EXISTS 都按 object_id 复核 uploads；没有它每行一次全表扫。
CREATE INDEX IF NOT EXISTS idx_task_artifact_uploads_object
    ON task_artifact_uploads(object_id);

-- +migration 15 artifact_upload_task_index
-- 到期扫描里还有一个按 task 的 EXISTS（u.task_id=t.id）：没有它同样是每行一次 uploads 全表扫（顺带覆盖 tasks 级联删除的子表查找）。
CREATE INDEX IF NOT EXISTS idx_task_artifact_uploads_task
    ON task_artifact_uploads(task_id);
