-- Anotify · SQLite schema（单一事实源）
-- 连接初始化 PRAGMA（在代码里设置）：
--   journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000; foreign_keys=ON;
--
-- 本文件同时是**迁移脚本**：按 `-- +migration N name` 切块，
-- store.migrateWith 逐块应用，schema_version 记账（见 migrate.go）。
--
-- baseline 之后是六块：`-- +migration 1 baseline`、`2 priority_balance`、
-- `3 agent_version`、`4 task_logs`、`5 task_seq_in_trace`、`6 drop_edges_lookup`（新变更继续往后追加）。
--
-- squash：squash 之前的 28 块增量迁移（建表 → 加列 → 删列 → 重建表 → 删表）已经合并成
-- 这一份**终态 DDL**。这里描述的是「今天库里长什么样」，不残留任何「迁移 4 建表、
-- 迁移 28 再删列」式的历史包袱——tasks.out / source_targets / orgs / org_members /
-- node_capabilities.run_spec / 旧通知模型的三张表都不在这里。
--
-- 规则（从现在起）：
--   · baseline（版本 1）视为**已发布**，不再改动；新变更从 `-- +migration 2` 往后追加块。
--   · squash 之前建的库**不支持升级**：Open 会直接报错要求删库重建（判据与文案见
--     migrate.go 的 legacyVersionError）。这里没有「增量升级」这条路径了。
--   · 需要 Go 逻辑的变更（条件 ALTER、数据搬迁）写空的 SQL 块，在 migrate.go 的
--     hooks 参数里实现——机制仍在，只是 baseline 不需要它。
--
-- 语句文本逐字节沿用 squash 前 sqlite_master 里记录的内容（引号化的表名、缩进都在内），
-- 因为「结构没有变」的证明就是：squash 前后各建一个库，sqlite_master 逐行 diff 为空。
-- 顺手重排文本只会让这道证明变模糊；要读的说明都在每张表上方的注释里。
--
-- 注释约定：只描述**现在是什么、为什么**（不变量，不是迁移史）。

-- +migration 1 baseline

-- 用户（Passkey 无密码）。首个注册用户自动成为 admin（契约 §2.1）。
-- username 唯一；role = admin | member；disabled=1 表示超管禁用了这个用户（禁止登录）。
CREATE TABLE IF NOT EXISTS users (
    id          TEXT PRIMARY KEY,            -- usr_xxx
    username    TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    role        TEXT NOT NULL DEFAULT 'member',  -- admin | member（首个注册用户自动 admin）
    disabled    INTEGER NOT NULL DEFAULT 0,      -- 超管禁用某用户（1=禁用，禁止登录）
    created_at  INTEGER NOT NULL             -- unixepoch 秒
);

-- 按角色取用户（管理员列表 / 权限检查）。
CREATE INDEX IF NOT EXISTS idx_users_role ON users(role);

-- Passkey 凭证（一个用户可有多个）。
-- backup_eligible 必须持久化认证器上报的 BackupEligible flag：登录校验时
-- 标志不一致会直接拒绝（Apple 系 passkey 一律 backup eligible）。
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

-- 登录会话（id 就是下发给浏览器的 token）。
-- device_name 是 UA 推断出来的设备名，只用于「设备列表」展示。
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,             -- sess_xxx token
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_name TEXT NOT NULL DEFAULT '',     -- 登录设备名（UA 推断，如「Chrome · macOS」）
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
);

-- 按用户取会话（设备列表 / 全端登出）。
CREATE INDEX IF NOT EXISTS idx_sessions_user         ON sessions(user_id);

-- API Key（只存 argon2id 哈希，明文只在创建时返回一次）。
-- scopes 控制 notify:send / notify:receive / devices:read；prefix 是明文前缀，便于用户识别。
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

-- 按用户取 API Key。
CREATE INDEX IF NOT EXISTS idx_apikeys_user          ON api_keys(user_id);

-- 接收设备（Web Push 订阅）——属于用户，不属于命名空间（契约 Q1）。
-- tags 是设备标签（派发时按标签选设备）；event_scope 决定推送范围（final|all）。
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

-- 按用户取设备（推送扇出）。
CREATE INDEX IF NOT EXISTS idx_devices_user          ON devices(user_id);

-- CLI 设备授权会话（device authorization flow）。
-- 一次性会话：pending → approved → consumed；旁路终态 denied / expired。
-- secret 只留 sha256（明文仅建会话时返回一次）；user_code 是给用户手抄的 8 位码。
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

-- 清理过期的一次性授权会话。
CREATE INDEX IF NOT EXISTS idx_cli_auth_expires      ON cli_auth_sessions(expires_at);

-- 恢复码（只存 argon2id PHC 哈希；明文只在签发时返回一次）。
-- 格式 XXXX-XXXX-XXXX-XXXX（去歧义字符集），一批 8 个；兑换单个后作废该码。
-- used_at IS NULL = 未使用。
CREATE TABLE IF NOT EXISTS recovery_codes (
    id          TEXT PRIMARY KEY,            -- rc_xxx
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL,               -- argon2id PHC
    prefix      TEXT NOT NULL,               -- 规范化明文前缀（便于定位候选）
    created_at  INTEGER NOT NULL,
    used_at     INTEGER                      -- NULL = 未使用
);

-- 按用户取恢复码。
CREATE INDEX IF NOT EXISTS idx_recovery_codes_user   ON recovery_codes(user_id);

-- 兑换时按前缀定位候选码（只存哈希，不能按明文查）。
CREATE INDEX IF NOT EXISTS idx_recovery_codes_prefix ON recovery_codes(prefix);

-- 命名空间：唯一的硬隔离边界（契约 §2.1.1）。
--
-- 能力 / 节点 / 边 / 源 / 事件 / 任务 / 图快照 / 产物 / 部署 / 预案**全部**属于某个
-- 命名空间；用户与密钥是部署级的（users / passkeys / sessions / api_keys）。
--
-- slug 全局唯一：它不是展示名，而是出现在 URL 里的稳定标识（/v1/namespaces/<slug>/…），
-- 两个命名空间同 slug 会让它失去意义。name 是展示名，可改。
-- created_at 决定 DefaultNamespaceID 取「最早注册的那一个」。
CREATE TABLE IF NOT EXISTS namespaces (
			id         TEXT PRIMARY KEY,
			slug       TEXT NOT NULL,
			name       TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			UNIQUE (slug)
		);

-- 命名空间列表按注册时间排序。
CREATE INDEX IF NOT EXISTS idx_namespaces_created ON namespaces(created_at);

-- 能力定义（中枢侧，append-only，契约 §2.2）。
--
-- outputs 是出口名的唯一来源：既用于 kind 推导，也定义 edges.from_port 的封闭集合。
-- 没有 kind 列——推导量不落库（不变量 I3）。
--
-- UNIQUE (namespace_id, name, version) 是能力身份；
-- UNIQUE (namespace_id, content_hash) 保证同一份内容不会产生第二行。
--
-- display_name 是**纯展示**用的标签（用户自己取的中文名）：它绝不参与路由
-- （边按引用名指向能力，出口名来自 outputs），也**不参与 content_hash**
-- ——改个显示名不该算「内容变了」。
--
-- params_schema 是 JSON Schema（派发期用它剪枝）；lifecycle 是运行期策略
-- （expiresInSec / retryable / maxAttempts / resultTtlSec / idempotent）；
-- sticky 是粘性路由的类型（'' | worktree | …）；source 是 builtin | custom。
CREATE TABLE IF NOT EXISTS capability_versions (
			id            TEXT PRIMARY KEY,
			namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			name          TEXT NOT NULL,
			display_name  TEXT NOT NULL DEFAULT '',
			version       INTEGER NOT NULL,
			params_schema TEXT NOT NULL,
			outputs       TEXT NOT NULL,
			lifecycle     TEXT NOT NULL DEFAULT '{}',
			sticky        TEXT NOT NULL DEFAULT '',
			source        TEXT NOT NULL,
			content_hash  TEXT NOT NULL,
			created_at    INTEGER NOT NULL,
			UNIQUE (namespace_id, name, version),
			UNIQUE (namespace_id, content_hash)
		);

-- 按 name@version 找能力（引用解析、升级闸门）。
CREATE INDEX IF NOT EXISTS idx_cap_name_ver ON capability_versions(namespace_id, name, version);

-- 节点（契约 §2.3）。
--
-- name 是用户可读名（可改，id 不变）；tags 是节点池标签（派发选点用）。
-- status = online | offline | draining。
-- token_hash 存 sha256，明文只在创建时返回一次。
-- max_instances 是实例数硬顶（防一份 token 复制出一片）；
-- max_concurrency 是节点级并发上限。last_seen_at 由 hello/心跳刷新。
CREATE TABLE IF NOT EXISTS nodes (
			id              TEXT PRIMARY KEY,
			namespace_id    TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			name            TEXT NOT NULL,
			tags            TEXT NOT NULL DEFAULT '[]',
			status          TEXT NOT NULL DEFAULT 'offline',
			token_hash      TEXT NOT NULL,
			max_instances   INTEGER NOT NULL DEFAULT 20,
			max_concurrency INTEGER NOT NULL DEFAULT 8,
			last_seen_at    INTEGER,
			created_at      INTEGER NOT NULL,
			UNIQUE (namespace_id, name)
		);

-- 「这台节点上有这个能力」的归属关系。
--
-- 它不是真相源，而是**部署的派生**：控制台写 deployments，节点回报 apply 结果，
-- 归属关系由对账得到（见 store 的部署/对账实现）。
-- 没有 run_spec 列：执行方式属于产物（artifacts.spec 里的 run 块），随 digest 走；
-- 中枢只存不执行（不变量 I1）。也没有出口、没有 kind：那些由能力定义与边表推导。
CREATE TABLE IF NOT EXISTS "node_capabilities" (
			node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
			namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			PRIMARY KEY (node_id, capability_id)
		);

-- 反查「哪些节点有这个能力」（派发选点）。
CREATE INDEX IF NOT EXISTS idx_nodecap_cap ON node_capabilities(capability_id);

-- 并发槽位：节点申报，中枢 upsert。
-- slot 是 0..N-1；concurrency 是这个实例能同时跑几份；
-- busy_task 是当前任务 id（空闲 NULL）；alive=0 表示这个实例掉线（不等同于整个节点下线）。
CREATE TABLE IF NOT EXISTS node_instances (
			id           TEXT PRIMARY KEY,
			node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			slot         INTEGER NOT NULL,
			concurrency  INTEGER NOT NULL DEFAULT 1,
			busy_task    TEXT,
			alive        INTEGER NOT NULL DEFAULT 1,
			updated_at   INTEGER NOT NULL,
			UNIQUE (node_id, slot)
		);

-- 粘性路由（sticky）：同一条能力尽量落到同一台机器，工作目录/缓存才不会每次重建。
-- sticky_key = sha256(trace_id ∥ capability_id ∥ selector)；selector 是当时命中的标签，
-- 留下来是为了排障时回答「它凭什么被钉在这台机器上」。
CREATE TABLE IF NOT EXISTS node_pins (
			sticky_key   TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			selector     TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			last_used_at INTEGER NOT NULL
		);

-- 出口表（路由的唯一依据，契约 §2.4）。一行 = 一条边；同一 (from_ref, from_port)
-- 可有多行 → 扇出。右端一定是能力引用，不引用任何机器（不变量 I7）。
--
-- 两端都是**引用**而不是外键（P2.2）：引用有两种写法——`fixer` 跟随 latest、
-- `fixer@1` 钉死某版。它们是两条不同的边，所以主键里放的是引用本身。
-- 正因为「跟随 latest」在 SQL 里无法用外键表达（外键只能钉住某一行），
-- from_ref / to_ref 刻意没有 REFERENCES：解析发生在派发时（按名字找当前版本），
-- 解析不到就剪枝留痕，而不是让写入失败。
--
-- from_is_source 区分左端的两种语义：
--   0  能力之间的边：from_ref 是上游能力引用，from_port 必须 ∈ 该能力的 outputs
--      （不变量 I2：封闭集合）
--   1  入口边：from_ref 是**来源名**（如 github / cron），from_port 是事件类型，
--      任意值（外部系统决定，不可能预先枚举）。机器入口 POST /v1/events 与
--      sources 触发走的都是入口边——「事件 → 第一跳」只有这一条取目标的路径。
--
-- literals 是这条边的**常量覆盖**：进入下游的 params = data ⊎ literals（字面量优先）。
--   '{}' = 没有覆盖。校验在定义期完成（每个键 ∈ 目标 params_schema.properties，
--   值类型精确相等），派发期再过一遍 params_schema。
-- mapping 是**入口边**的引用模板（{"title":"{{pull_request.title}}"}）：参数怎么拼
--   只有入口边能声明，能力边保存时硬拒——能力边的参数由 emit 提供，是实现的内部事务。
-- 两者语义相反（常量 vs 引用模板），所以不合并成一列；也都不参与能力版本
-- （content_hash）——它们是对**这条边**的配置，不是能力契约的一部分。
-- created_at 是这条边的创建时刻（审计用）。
CREATE TABLE IF NOT EXISTS "edges" (
			namespace_id    TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			from_is_source  INTEGER NOT NULL DEFAULT 0,
			from_ref        TEXT NOT NULL,
			from_port       TEXT NOT NULL,
			to_ref          TEXT NOT NULL,
			literals        TEXT NOT NULL DEFAULT '{}',
			mapping         TEXT NOT NULL DEFAULT '{}',
			created_at      INTEGER NOT NULL,
			PRIMARY KEY (namespace_id, from_is_source, from_ref, from_port, to_ref)
		);

-- 派发主路径：按 (namespace_id, from_is_source, from_ref, from_port) 取下游。
-- 形状是唯一权威（列序即查询形状）；历史库里同名的漂移索引一律以本形状为准。
CREATE INDEX IF NOT EXISTS idx_edges_lookup
		   ON edges(namespace_id, from_is_source, from_ref, from_port);

-- 事件（契约 §2.5）：只存不解释的原始输入。
-- payload 是原始 JSON（原样保存，不改写）；trace_id 串起「一个根事件引起的全部工作」。
-- idem_key 是幂等键（触发器给；NULL 允许重复）。
CREATE TABLE IF NOT EXISTS events (
			id           TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			trace_id     TEXT NOT NULL,
			source       TEXT NOT NULL,
			type         TEXT NOT NULL,
			payload      TEXT NOT NULL DEFAULT '{}',
			idem_key     TEXT,
			created_at   INTEGER NOT NULL
		);

-- 按 trace 看整条链路（trace 详情页）。
CREATE INDEX IF NOT EXISTS idx_events_trace ON events(namespace_id, trace_id);

-- 幂等索引：唯一性只对非 NULL 的 idem_key 生效（NULL 允许重复）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_idem ON events(namespace_id, source, idem_key)
			   WHERE idem_key IS NOT NULL;

-- 保留期清理 + 事件流：按命名空间过滤、按时间排序。
CREATE INDEX IF NOT EXISTS idx_events_retention ON events(namespace_id, created_at);

-- 任务（契约 §2.5 / §3）。id 同时是 OTel span_id。
--
--   path            JSON 数组：从根到本任务的能力**名**序列（运行期环剪枝按名字判重）
--   capability_id   派发时的能力定义。刻意**不带** ON DELETE CASCADE：
--                   删掉一个能力不该顺手清空它的任务历史（审计要靠它）
--   capability/version  冗余存 name@version，展示与引用解析用
--   params          派发参数，已过 params_schema
--   status          九态（契约 §3）
--   node_id/instance_id/lease_until/attempts  租约与重试
--   reported        1 = 节点已回报（终态已落账，中枢重启后不再重派）
--   summary/artifacts/error  节点回报的结果摘要 / 产物清单 / 失败原因
--   cap_hash        派发时的能力定义 content_hash
--   digest          派发时钉住的产物内容 digest（'' = 无产物 / 未知；审计字段，不参与路由）
--   graph_id        派发时的 graph_versions.id（事后能还原当时那张图）
--   token           每任务一次性令牌（节点本地 emit 的鉴权凭据）。'' = 尚未生成：
--                   中枢只在「第一次发布 assign」时生成并写回，之后重发一律用库里那个值
--                   （节点无从区分「该换令牌」与「有人在伪造」，两端必须一致）
--   not_before      退避：早于此刻的 pending 不该被重派（NULL = 立刻可派，契约 §3.3）
--   created_at/started_at/finished_at  时间戳
--
-- 没有 out 列：产出通道只有 emission（emissions.port）。任务行不记出口——
-- 一个恒为 NULL 的可读字段只会让人按它写判断（"路由结果在任务行上"）。
CREATE TABLE IF NOT EXISTS tasks (
			id             TEXT PRIMARY KEY,
			namespace_id   TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			trace_id       TEXT NOT NULL,
			parent_task_id TEXT,
			path           TEXT NOT NULL DEFAULT '[]',
			capability_id  TEXT NOT NULL REFERENCES capability_versions(id),
			capability     TEXT NOT NULL,
			version        INTEGER NOT NULL,
			params         TEXT NOT NULL DEFAULT '{}',
			status         TEXT NOT NULL DEFAULT 'pending',
			node_id        TEXT,
			instance_id    TEXT,
			lease_until    INTEGER,
			attempts       INTEGER NOT NULL DEFAULT 0,
			reported       INTEGER NOT NULL DEFAULT 0,
			summary        TEXT NOT NULL DEFAULT '',
			artifacts      TEXT NOT NULL DEFAULT '[]',
			error          TEXT NOT NULL DEFAULT '',
			cap_hash       TEXT NOT NULL,
			graph_id       TEXT NOT NULL,
			created_at     INTEGER NOT NULL,
			started_at     INTEGER,
			finished_at    INTEGER,
			not_before     INTEGER
		, digest TEXT NOT NULL DEFAULT '', token TEXT NOT NULL DEFAULT '');

-- 按 trace 看整条链路。
CREATE INDEX IF NOT EXISTS idx_tasks_trace ON tasks(namespace_id, trace_id);

-- 找派生任务（谁派生的）。
CREATE INDEX IF NOT EXISTS idx_tasks_parent ON tasks(namespace_id, parent_task_id);

-- 租约回收 + 按状态取任务（status 是等值前缀）。
CREATE INDEX IF NOT EXISTS idx_tasks_lease ON tasks(status, lease_until);

-- 按节点看它手上的任务（节点摘除 / 对账）。
CREATE INDEX IF NOT EXISTS idx_tasks_node ON tasks(node_id, status);

-- 保留期清理：正好覆盖 WHERE (namespace_id, capability_id, status) +
-- ORDER BY finished_at，避免「LIMIT 只限制删除行数、扫描/排序仍是整个命名空间」。
-- 它同时服务统计（同样按这三列过滤、按时间排序）。
CREATE INDEX IF NOT EXISTS idx_tasks_retention
			   ON tasks(namespace_id, capability_id, status, finished_at);

-- 任务列表（ListTasks：WHERE namespace_id=? ORDER BY created_at DESC, id DESC）。
-- 列序 (namespace_id, created_at, id)：前两列对齐过滤与排序，第三列 id 是同秒破平
-- （created_at 只到秒）。ASC 形状能被 SQLite **反向扫**来满足降序 ORDER BY，
-- 也仍然服务时间窗范围查询，所以不用 DESC 索引。
CREATE INDEX IF NOT EXISTS idx_tasks_ns_created ON tasks(namespace_id, created_at, id);

-- 派发背压信号（#228）：SELECT MIN(created_at) FROM tasks WHERE status='pending'
-- 退化成一次索引最左端 seek（O(log n)，成本与积压量无关）。
-- **部分**索引（只收 pending 行）：终态行不进索引，索引体积只随积压量增长。
-- 统计：SQLite 的 min/max 优化要看到本索引自己的 sqlite_stat1 条目才会选它，
-- 条目由 Open 末尾的 PRAGMA optimize 在数据灌进来之后补上（空表上扫描一行都不写）。
CREATE INDEX IF NOT EXISTS idx_tasks_pending_created_at
    ON tasks(created_at) WHERE status = 'pending';

-- 图快照（契约 §2.6）：全部 edges 的规范化快照。
-- UNIQUE (namespace_id, content_hash)：同一张图不产生第二个版本；
-- 任务行上的 graph_id 指回这里，事后能还原「当时那条链路长什么样」。
CREATE TABLE IF NOT EXISTS graph_versions (
			id           TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			content_hash TEXT NOT NULL,
			snapshot     TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			UNIQUE (namespace_id, content_hash)
		);

-- 每一次 emit 的落库行（P1.1，docs/design/emit-protocol.md §4）。
--
-- 三件事，缺一不可：
--   ① 幂等键。PRIMARY KEY(task_id, seq) 是中枢侧**唯一**的去重依据：节点重连后按 seq
--      重发未 ack 的 emit，靠它做到不重复派发下游。改成内存表就等于把「进程重启」
--      变成「重复派发 N 条任务」。
--   ② 缺口证据。seq 必须从 1 连续递增；「收到 seq=n 但 1..n-1 不齐」是丢消息的证据，
--      中枢明确拒绝而不是猜测/跳过。
--   ③ 审计。已落库的 emission 在任务被 cancel / 租约过期后仍然保留：回答「这个能力在
--      运行中到底上报过什么」，与任务终态是两件事。
--
-- routed / pruned 存的是**首次**处理的结果：重复 seq 一律回同一份计数（不重算、不重派）。
--
-- 没有 namespace_id：task_id 全局唯一，任务行自己带着命名空间；再存一列就是第二份真相。
-- ON DELETE CASCADE：任务被 retention 清掉时它的 emission 一起走——脱离任务行的
-- emission 既不能派发也不能解释。
CREATE TABLE IF NOT EXISTS emissions (
    task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,               -- 任务内单调递增，从 1 开始
    port       TEXT NOT NULL,                  -- 上游回报的出口名（路由的唯一输入）
    data       TEXT NOT NULL DEFAULT '{}',     -- 原样保存的 JSON 对象（审计用字节）
    note       TEXT NOT NULL DEFAULT '',       -- 能力进程写的备注：不参与路由，只给排障看
    routed     INTEGER NOT NULL DEFAULT 0,     -- 首次处理派发出去几条下游
    pruned     INTEGER NOT NULL DEFAULT 0,     -- 首次处理剪枝条数
    created_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, seq)
);

-- 产物：一份能力的不可变快照。
-- digest 是内容寻址标识（sha256:<64 hex>），spec 是 canonical JSON。
-- UNIQUE (namespace_id, digest) 表达「同一份产物只有一个身份」：内容一样的产物
-- 重复部署不会产生第二行，digest 也就永远不会指向两份不同的内容。
CREATE TABLE IF NOT EXISTS artifacts (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
    digest        TEXT NOT NULL,
    spec          TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    UNIQUE (namespace_id, digest)
);

-- 按能力找它的产物（部署面板 / 钉 digest）。
CREATE INDEX IF NOT EXISTS idx_artifacts_cap ON artifacts(capability_id);

-- 「控制台希望某台节点拥有某份产物」（desired，由 artifact_id 表达）
-- +「节点回报的状态」（observed，就是 state / reason / updated_at 三列）。
--
-- 写者纪律（CONTRACT-protocol §7）：
--   · 建行 / 删行只有控制台 API 会做；
--   · state / reason / updated_at 只有节点帧（apply）与 hello 对账会写。
-- 两边不得碰对方的列——这是「不产生第二个真相源」的全部机制。
--
-- state: pending | applied | failed | local
--   local = 节点上该能力的 digest 与 desired 不一致（用户手改过，即「本地变体」）。
--   中枢永不覆盖 local，UI 必须标黄而不是打绿勾。
--
-- 工作目录**不是**部署参数（#289）：它是能力定义的一部分（run.workDir），随产物 digest 走。
CREATE TABLE IF NOT EXISTS deployments (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    artifact_id  TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    digest       TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'pending',
    reason       TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (node_id, artifact_id)
);

-- 按节点看它该有哪些产物（节点 hello 对账）。
CREATE INDEX IF NOT EXISTS idx_deploy_node ON deployments(node_id);

-- 按产物看它被部署到哪些节点。
CREATE INDEX IF NOT EXISTS idx_deploy_artifact ON deployments(artifact_id);

-- 预案（可安装的流程零件，设计 docs/design/playbooks.md）。
--
-- 预案本身**不产生任务**：它只是一份清单（能力 / 边 / 触发器），安装时按
-- 「能力 → 边 → 源」的顺序补齐本命名空间里的真实资源（I8 append-only：已有定义不覆盖）。
-- 就绪体检是查询时算出来的推导量，不落库（I3 同源）。
--
-- manifest 存**提交的 JSON 原文**：run 配置 / paramsSchema 这类节点侧字段中枢只存不解释
-- （I1），重新编码会丢掉键序与用户自定义键（往返一致性）。
--
-- 本注释刻意不写任何「可执行概念」的英文关键词：make contract 的 A2 正则扫 schema.sql
-- 全文（注释也算），命中即失败。同理，说明文字这一列叫 `descr` 而不是它的完整英文单词：
-- A2 的正则是**子串**匹配，那个单词里恰好含有被禁的子串（契约 §4 的 DDL 写的是完整单词，
-- 与 A2 关卡直接冲突；列名缩写在 store 层之外不可见，Go 字段与 JSON 仍是完整单词）。
CREATE TABLE IF NOT EXISTS playbooks (
			id           TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			slug         TEXT NOT NULL,
			name         TEXT NOT NULL,
			descr        TEXT NOT NULL DEFAULT '',
			manifest     TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			UNIQUE (namespace_id, slug)
		);

-- 命名空间内的预案列表（按安装时间）。
CREATE INDEX IF NOT EXISTS idx_playbooks_ns ON playbooks(namespace_id, created_at);

-- 源（origin）：事件的**入口实例**，不是能力。
--
-- 能力回答「能干什么」（由节点声明、中枢派任务给它）；源回答「什么时候开始干」——
-- 它有配置（cron 表达式 / webhook 令牌 / 手动触发），自己产生事件，自己不干活。
--
-- config 是按 kind 解释的 JSON（不变量 I6：只读配置字段，不求值、不执行）：
--   cron    {"expr":"0 3 * * *","tz":"Local"}
--   webhook {"token":"..."}（token 用于拼出唯一 URL）
--   manual  {}
-- last_fired_at / next_fire_at 是调度元数据（只有 cron 用；其它 kind 为 NULL）。
--
-- 源的目标不是一张旁路表，而是 edges 里的**入口边**（from_is_source=1，
-- from_ref = 来源名，参数模板落在 edges.mapping 上）：这样「事件 → 第一跳」
-- 只有一条取目标的路径，不需要为每个事件类型手建一条边或一个空壳能力。
CREATE TABLE IF NOT EXISTS sources (
			id            TEXT PRIMARY KEY,
			namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			name          TEXT NOT NULL,
			kind          TEXT NOT NULL,
			config        TEXT NOT NULL DEFAULT '{}',
			enabled       INTEGER NOT NULL DEFAULT 1,
			last_fired_at INTEGER,
			next_fire_at  INTEGER,
			created_at    INTEGER NOT NULL,
			UNIQUE (namespace_id, name)
		);

-- 命名空间内的源列表（按创建时间）。
CREATE INDEX IF NOT EXISTS idx_sources_ns ON sources(namespace_id, created_at);

-- 调度器取「该触发的源」：enabled=1 且 next_fire_at 到期。
CREATE INDEX IF NOT EXISTS idx_sources_due ON sources(enabled, next_fire_at);

-- +migration 2 priority_balance

-- 候选优先级（契约 §1 / §3）：同一个 (能力, 端口) 的下游是一个**候选集合**，
-- 派发按 priority 升序取最小的一组，组内轮盘选一个（小的优先）。
-- 0 = 默认值（加这一列之前建的边全部同级，行为与「只有一个下游」时完全一样）。
-- 不额外建索引：查边只按 (namespace_id, from_is_source, from_ref, from_port) 过滤
-- （idx_edges_lookup 的形状不变），priority 只是被选出来的排序键。
ALTER TABLE edges ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;

-- 端口级平衡策略（契约 §1）：JSON 对象 {端口: {"mode": "priority"}}，缺省 {} = priority。
-- 本期只实现 priority；all / random / least_loaded / hash 保存时 400（尚未实现），
-- 不静默降级 —— 静默降级会让「我配了 all 却是 1:1」这种分歧永远查不出来。
ALTER TABLE capability_versions ADD COLUMN balance TEXT NOT NULL DEFAULT '{}';

-- +migration 3 agent_version

-- 节点上跑的 agent 版本（hello.agent_version 自报）与最近一次成功升级带来的 commit
-- （upgrade_ack 上报）。都 NOT NULL DEFAULT ''：**空串 = 未知（老 agent 不带这个字段）**，
-- 不是错误，控制台据此显示「未知（老版本）」。
--
-- 为什么拆成两列而不是一个 "1.5.0+3fd5a59" 字符串：版本号是**比较**用的（控制台据此判断
-- 「已是最新」），commit 是审计用的；拼在一起之后每次比较都要先拆串，拆错一次就是
-- 「明明同版本却一直提示可升级」。
--
-- agent_version 由 hello 落库（每次连接都刷新），agent_commit 只由 upgrade_ack 落库
-- （hello 帧没有这个字段，见 CONTRACT-agent-upgrade §3/§5）。
ALTER TABLE nodes ADD COLUMN agent_version TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN agent_commit  TEXT NOT NULL DEFAULT '';

-- 最近一次升级尝试的状态（CONTRACT-agent-upgrade §5.1）：JSON
-- {"status":"","targetVersion":"","error":"","at":0}。
--   status  ∈ ""（从未尝试）/ sent（已下发等 ack）/ ok / failed
--   error   ∈ need_root / bad_sha / download_failed / not_supported / ""
--   at      那一刻的 unix 秒（sent/ok/failed 都有）
-- 为什么要落库而不是只留在内存：控制台刷新/重启后仍要显示上次升级的结果；
-- 没有它，UI 的失败映射（need_root → 提示去节点跑 sudo）就是死代码。
-- 默认 '{}' = 从未尝试；读取端把缺列补成零值对象（恒存在的 agentUpgrade）。
ALTER TABLE nodes ADD COLUMN agent_upgrade TEXT NOT NULL DEFAULT '{}';

-- +migration 4 task_logs

-- **已停用**（CONTRACT-logs-on-node §3）：日志的真相只在**节点**上
-- （`<data-dir>/runs/<trace>/<capability>@<version>/<seq>-a<attempt>/` 下的
-- stdout.log / stderr.log，完整、8 MiB/流上限），控制台要看时中枢按需向节点取
-- （GET /v1/tasks/{taskId}/logs → 一帧 logs_request / logs_response）。
-- 本表**不再写入也不再读取**：中枢侧的读写函数已随上传端点一起删除，保留这张表只是
-- 为了不动已经发布的迁移（删表要另起一个迁移块，而这一轮明确不动 schema）。
-- 因此表里可能有**历史行**（上一版上传路径留下的），它们不再被任何代码读到，
-- 也不代表节点上现在有什么——排障请看节点上的档案。
-- 下面的说明保留下来当设计依据：它解释的是「当初为什么这么建」，不是「现在还在用」。

-- 任务的 stdout / stderr 原文（CONTRACT-task-logs §3）：节点在发 result 帧之前，
-- 用 node token 把**子进程自己的输出**POST 上来，控制台在 trace 页看得到「为什么炸了」。
--
-- 为什么单独一张表而不是 tasks 加三列：一张 20 MB 构建日志的截断版也有 64 KiB，
-- 塞进 tasks 会让每一次任务列表查询（ListTasks / stats / 巡检）都拖着大 TEXT 走；
-- 分表之后热路径一行都不碰它，读日志只有 GET /v1/tasks/{id}/logs 一条路径。
--
-- task_id 是**主键**（不是 (task_id) 索引）：一个任务只有一份日志，agent 重派/重启
-- 补传是**幂等覆盖**（ON CONFLICT DO UPDATE），不需要保留历史版本——历史版本没人读，
-- 只会让「这条日志是哪次 attempt」变成一个要额外解释的问题。
--
-- ON DELETE CASCADE：日志跟随任务生命周期。retention 清掉任务时日志一起走，
-- 否则「任务没了、日志留着」会变成一张没人回收的孤儿大表（磁盘只涨不降）。
--
-- truncated / bytes 是**采集侧**的事实（不是本表算出来的）：truncated=1 表示 agent 侧
-- 截断过（只留了最后 32 KiB/流），bytes 是截断**前**的真实字节数。前端必须据此
-- 提示「你看的不是全部」，所以它不能由 LENGTH(stdout) 反推。
CREATE TABLE IF NOT EXISTS task_logs (
    task_id      TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    namespace_id TEXT NOT NULL,
    stdout       TEXT NOT NULL DEFAULT '',
    stderr       TEXT NOT NULL DEFAULT '',
    truncated    INTEGER NOT NULL DEFAULT 0,
    bytes        INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
);

-- +migration 5 task_seq_in_trace

-- 这条任务在同一 (命名空间, trace, 能力名) 里是第几次执行（1 起），派发时算好落库。
--
-- 为什么要落库而不是读的时候现算：节点把每次执行存到本地 runs/<trace>/<cap>@<ver>/
-- 下的目录名里要它（`<seq>-a<attempt>`），而节点只知道中枢下发的那一帧——现算等于
-- 让节点自己数「同一个 trace 里我跑过几次这个能力」，跨节点/重连都会数错。
--
-- 语义口径：按**能力名**分组（不是 capability_id）——同一 trace 里 review@1 与 review@2
-- 属于同一个能力，seq 连续；按 **rowid** 定序（不是 created_at）——同一秒内扇出的多条
-- 任务也必须有确定先后。因此它只取决于「行在组内的位置」，重派同一条任务不会变。
--
-- 默认 0 = 从未派发过（也包含加这一列之前建的老行）；节点侧对 <=0 退回到 attempt。
ALTER TABLE tasks ADD COLUMN seq_in_trace INTEGER NOT NULL DEFAULT 0;

-- +migration 6 drop_edges_lookup

-- idx_edges_lookup 的四列是 edges 主键的**严格左前缀**（PRIMARY KEY 前四列逐列相同），
-- 给不了 sqlite_autoindex_edges_1 给不了的东西：派发 4 列等值实测一直选 autoindex，
-- 3 列前缀查询删掉它也仍由 autoindex 以同一 search shape 承担（无全表扫）。
-- 删掉只少一份等价的 B-tree 维护成本。IF EXISTS 保证重放旧块时不报错。
DROP INDEX IF EXISTS idx_edges_lookup;
