-- Anotify · SQLite schema（单一事实源）
-- 连接初始化 PRAGMA（在代码里设置）：
--   journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000; foreign_keys=ON;

-- 用户（Passkey 无密码）
CREATE TABLE IF NOT EXISTS users (
    id          TEXT PRIMARY KEY,            -- usr_xxx (KSUID)
    username    TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
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
    id         TEXT PRIMARY KEY,             -- sess_xxx token
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL
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

-- 接收设备（Web Push 订阅）
CREATE TABLE IF NOT EXISTS devices (
    id           TEXT PRIMARY KEY,           -- dev_xxx
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    platform     TEXT NOT NULL DEFAULT '',   -- ios|mac|win|android|other
    enabled      INTEGER NOT NULL DEFAULT 1,
    status_filter TEXT NOT NULL DEFAULT 'all', -- all|error|success
    tags         TEXT NOT NULL DEFAULT '[]', -- JSON 数组（设备标签，路由用）
    endpoint     TEXT NOT NULL UNIQUE,       -- push endpoint
    p256dh       TEXT NOT NULL,
    auth         TEXT NOT NULL,
    user_agent   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_active  INTEGER,
    last_delivered INTEGER
);

-- 消息（队列 + 历史，单一事实源）
CREATE TABLE IF NOT EXISTS messages (
    id          TEXT PRIMARY KEY,            -- ntf_xxx (KSUID，时间有序)
    user_id     TEXT NOT NULL,
    seq         INTEGER NOT NULL,            -- 每用户单调递增（replay offset）
    title       TEXT NOT NULL,
    status      TEXT NOT NULL,               -- success|error|interrupted|info|warning
    body        TEXT NOT NULL DEFAULT '',
    link        TEXT NOT NULL DEFAULT '',
    device_tags TEXT NOT NULL DEFAULT '[]',  -- JSON 数组（路由键，broker 侧过滤）
    priority    TEXT NOT NULL DEFAULT 'normal',
    ttl_seconds INTEGER NOT NULL DEFAULT 86400,
    payload     TEXT NOT NULL,               -- 完整 JSON（含 agentId/sessionId/model 等）
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,            -- created_at + ttl
    UNIQUE (user_id, seq)
);

-- 持久消费者位移（durable consumer）
CREATE TABLE IF NOT EXISTS consumer_offsets (
    consumer_id TEXT NOT NULL,               -- 如 "home-bridge" / "dev_xxx"
    user_id     TEXT NOT NULL,
    last_seq    INTEGER NOT NULL DEFAULT 0,  -- high-water mark
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (consumer_id, user_id)
);

-- 投递记录（观测性：仪表盘 "3/4 设备已送达"）
CREATE TABLE IF NOT EXISTS deliveries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    consumer_id TEXT NOT NULL,               -- 设备 / 连接
    channel     TEXT NOT NULL,               -- webpush | websocket
    status      TEXT NOT NULL,               -- pending|sent|delivered|acked|failed
    error       TEXT NOT NULL DEFAULT '',
    attempts    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

-- CLI 设备授权会话（device authorization flow）
-- 一次性会话：pending → approved → consumed；旁路终态 denied / expired
CREATE TABLE IF NOT EXISTS cli_auth_sessions (
    id               TEXT PRIMARY KEY,            -- cas_xxx
    secret_hash      TEXT NOT NULL,               -- sha256 hex（secret 仅建会话时返回一次，不落明文）
    user_code        TEXT NOT NULL UNIQUE,        -- 8 字符大写无连字符（去歧义字符集）
    device_name      TEXT NOT NULL,               -- ≤64 字符（脚本上报 hostname）
    scopes_requested TEXT NOT NULL,               -- JSON 数组
    scopes_granted   TEXT,                        -- JSON 数组，批准前 NULL
    status           TEXT NOT NULL,               -- pending/approved/consumed/denied/expired
    user_id          TEXT,                        -- 批准前 NULL
    key_id           TEXT,                        -- 领证前 NULL
    created_at       INTEGER NOT NULL,
    expires_at       INTEGER NOT NULL,            -- created_at + 600（TTL 10 分钟）
    consumed_at      INTEGER
);

-- 索引
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_user_seq     ON messages(user_id, seq);
CREATE        INDEX IF NOT EXISTS idx_messages_user_created ON messages(user_id, created_at);
CREATE        INDEX IF NOT EXISTS idx_messages_expires      ON messages(expires_at);
CREATE        INDEX IF NOT EXISTS idx_deliveries_msg        ON deliveries(message_id);
CREATE        INDEX IF NOT EXISTS idx_deliveries_consumer   ON deliveries(consumer_id, updated_at);
CREATE        INDEX IF NOT EXISTS idx_devices_user          ON devices(user_id);
CREATE        INDEX IF NOT EXISTS idx_sessions_user         ON sessions(user_id);
CREATE        INDEX IF NOT EXISTS idx_apikeys_user          ON api_keys(user_id);
CREATE        INDEX IF NOT EXISTS idx_cli_auth_expires      ON cli_auth_sessions(expires_at);
