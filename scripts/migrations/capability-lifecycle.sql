-- v4 开发库：把已有 dev 库原地升到 #432（能力启停 + 手动测试）的布局。
--
-- 为什么需要这个文件：当前迁移引擎只支持**单块** baseline（`baselineLayout()` 硬校验
-- `恰好一块且 version==baselineVersion`），所以 #432 的三处 schema 变更是在
-- `internal/store/schema.sql` 的 baseline 块内**原地**改的，`headVersion` 仍是 1。
-- 后果：老库被 `Open` 具名拒绝（`ErrSchemaInvalid`：缺表 capability_switches /
-- capability_audit、缺列 events.origin），程序不迁移、也不删用户数据。
--
-- 本文件是**运维动作**（不是迁移引擎的一部分）：三处变更全是「加表 / 加列 + 默认值」，
-- 因此可以用这几条语句原地补上，老库的结构随即与新 baseline 逐字一致，`Open` 通过。
-- 跑之前先备份库文件；跑完重启服务即可。
--
--   sqlite3 /path/to/anotify.db < scripts/migrations/capability-lifecycle.sql
--
-- 已知前提：库里 `PRAGMA user_version = 4`（v4 布局标记），且 `schema_version` 已有
-- version=1 的记账行。v3 老库不在本文件范围内（那需要 v3→v4 转换，本仓库不提供）。

BEGIN;

-- ① 能力开关：**无行 = enabled**，所以老库升级不需要任何回填。
CREATE TABLE IF NOT EXISTS capability_switches (
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    reason       TEXT NOT NULL DEFAULT '',
    updated_at   INTEGER NOT NULL,
    updated_by   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (namespace_id, name)
);

-- ② 能力操作审计（append-only）。
CREATE TABLE IF NOT EXISTS capability_audit (
    id           TEXT PRIMARY KEY,
    namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    action       TEXT NOT NULL CHECK (action IN ('enable', 'disable', 'test')),
    actor_id     TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_cap_audit_ns_name
    ON capability_audit(namespace_id, name, created_at DESC, id DESC);

-- ③ 事件来源类别：DEFAULT 'source' 覆盖全部存量行，不需要回填。
--    ALTER TABLE 加的列落在表尾；布局校验按列名比对，不看物理顺序。
ALTER TABLE events ADD COLUMN origin TEXT NOT NULL DEFAULT 'source'
    CHECK (origin IN ('source', 'test'));

COMMIT;
