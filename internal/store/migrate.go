package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersionTable 记录已应用的迁移版本。
//
// 为什么要有它：旧实现用一串 `_, _ = db.Exec("ALTER TABLE ...")` 补列，
// 所有错误被静默吞掉——真实迁移失败会被当成「已经迁移过」，库就坏在无声处。
// 现在每个迁移是编号版本：已记账则跳过，未记账则在一个事务里执行 + 记账，
// 任何一步失败都返回错误（不吞错）。
const schemaVersionTable = `CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`

// migrationHeader 匹配分块注释，例如：`-- +migration 4 new_model`
var migrationHeader = regexp.MustCompile(`^--\s*\+migration\s+(\d+)\s+(\w+)\s*$`)

// migration 是 schema.sql 里的一块（一个编号版本）。
type migration struct {
	version int
	name    string
	body    string // 去掉分块头之后的 SQL；不含任何 statement
}

// migrations 是解析 schema.sql 得到的迁移列表（按 version 升序）。
type migrationSet []migration

// parseMigrations 把 schema.sql 按 `-- +migration N name` 切块。
//
// 约束（parseMigrations 会强制检查）：
//   - 版本号从 1 开始连续递增，不跳号、不重复（漏一个编号会导致「以为迁移过」）
//   - 每块必须有非空的名字
func parseMigrations(src string) (migrationSet, error) {
	var out migrationSet
	var cur *migration
	var buf strings.Builder
	flush := func() {
		if cur == nil {
			return
		}
		cur.body = strings.TrimSpace(buf.String())
		out = append(out, *cur)
		buf.Reset()
	}
	for _, line := range strings.Split(src, "\n") {
		if m := migrationHeader.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			flush()
			v, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, fmt.Errorf("parse migration version %q: %w", m[1], err)
			}
			cur = &migration{version: v, name: m[2]}
			continue
		}
		if cur == nil {
			continue // 文件头的说明文字
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()

	for i, mg := range out {
		if want := i + 1; mg.version != want {
			return nil, fmt.Errorf("migration sequence broken: at position %d got version %d (want %d)", i, mg.version, want)
		}
	}
	return out, nil
}

// migrationHooks 是某个版本里需要用 Go 逻辑实现的部分
// （例如条件 ALTER TABLE：SQLite 没有 ADD COLUMN IF NOT EXISTS）。
//
// 约定：hook 返回 error 时整个迁移回滚；hook 必须**幂等**——
// 同一版本在同一个事务内可能因重试被执行多次。
var migrationHooks = map[int]func(*sql.Tx) error{
	2:  migrateLegacyColumns,
	3:  fixLegacyMessageColumns,
	5:  migrateNewModelColumns,
	8:  migrateSourceTargetParams,
	9:  migrateEdgesFromSource,
	10: migrateNodeCapabilityRunSpec,
	12: migrateNamespaces,
	14: migrateDropOrgLayer,
	15: migrateEdgeLookupIndex,
}

// migrateNewModelColumns 迁移 5：新模型（events/tasks/...）的补列。
//
// 为什么单独一个迁移而不是塞进迁移 2：迁移 2 叫 legacy_columns，跑在
// **迁移 4 之前**，那时 tasks 表还不存在，ALTER 必然失败。
// 新模型的列只能放在建表之后。
func migrateNewModelColumns(tx *sql.Tx) error {
	alters := []struct{ table, column, ddl string }{
		// 退避时间：NULL = 立刻可派（契约 §3.3）。老任务补 NULL 即可，行为不变。
		{"tasks", "not_before", `ALTER TABLE tasks ADD COLUMN not_before INTEGER`},
		// 能力的显示名（用户自己取的中文名）。默认空串 = 没取，前端回落到 name@version。
		{"capability_versions", "display_name", `ALTER TABLE capability_versions ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`},
	}
	for _, a := range alters {
		has, err := hasColumn(tx, a.table, a.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := tx.Exec(a.ddl); err != nil {
			return fmt.Errorf("add column %s.%s: %w", a.table, a.column, err)
		}
	}
	return nil
}

// migrateSourceTargetParams 迁移 8：给 source_targets 加 params 列。
//
// 每个出口能力各自的参数模板（见 schema.sql 迁移 8 的说明）。
// 默认 '{}'：老数据没有参数，行为不变（空 params 对空 schema 的能力正好够用）。
func migrateSourceTargetParams(tx *sql.Tx) error {
	has, err := hasColumn(tx, "source_targets", "params")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := tx.Exec(
		`ALTER TABLE source_targets ADD COLUMN params TEXT NOT NULL DEFAULT '{}'`); err != nil {
		return fmt.Errorf("add column source_targets.params: %w", err)
	}
	return nil
}

// migrateNodeCapabilityRunSpec 迁移 10：给 node_capabilities 加 run_spec 列（#168）。
//
// run_spec 是节点上报的「我打算怎么跑这个能力」的 JSON 原文，
// '{}' = 声明了能力但没声明执行方式（老 agent / manifest 里没写这一项）。
// 默认 '{}'：老数据统统落到「没声明」，行为不变——中枢只存不执行（I1 边界）。
func migrateNodeCapabilityRunSpec(tx *sql.Tx) error {
	has, err := hasColumn(tx, "node_capabilities", "run_spec")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := tx.Exec(
		`ALTER TABLE node_capabilities ADD COLUMN run_spec TEXT NOT NULL DEFAULT '{}'`); err != nil {
		return fmt.Errorf("add column node_capabilities.run_spec: %w", err)
	}
	return nil
}

// migrateEdgesFromSource 迁移 9：边表左端从「能力外键」改成「能力 id 或来源名」。
//
// 旧表 edges(from_capability → capability_versions.id)。起点边靠一个
// 叫 github@1 的内置能力当锚点，于是那个空壳必须存在、会出现在能力列表上。
//
// 新表 edges(from_is_source, from_key)：入口边的左端直接是来源名。
//
// 重建而不是 ALTER 的原因：from_capability 在**主键**里，SQLite 不能改列。
// 步骤：建新表 → 搬数据（起点边转成 from_is_source=1 + 来源名）→ 删旧表 → 改名。
//
// 幂等：用 hasColumn 判断是否已经迁移过（新表有 from_is_source，旧表没有）。
func migrateEdgesFromSource(tx *sql.Tx) error {
	has, err := hasColumn(tx, "edges", "from_is_source")
	if err != nil {
		return err
	}
	if has {
		return nil // 已经迁移过
	}

	// 旧表存在吗？全新库（先跑 CREATE TABLE 的新定义）不会被走到这里，
	// 但防御一下：表不存在说明这个迁移无事可做。
	exists, err := hasTable(tx, "edges")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	stmts := []string{
		`CREATE TABLE edges_new (
			org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			from_is_source  INTEGER NOT NULL DEFAULT 0,
			from_key        TEXT NOT NULL,
			from_port       TEXT NOT NULL,
			to_capability   TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
			created_at      INTEGER NOT NULL,
			PRIMARY KEY (org_id, from_is_source, from_key, from_port, to_capability)
		)`,

		// 能力之间（from 不是内置起点）的边原样搬过来
		`INSERT INTO edges_new (org_id, from_is_source, from_key, from_port, to_capability, created_at)
		 SELECT e.org_id, 0, e.from_capability, e.from_port, e.to_capability, e.created_at
		   FROM edges e
		  WHERE NOT EXISTS (
		        SELECT 1 FROM capability_versions cv
		         WHERE cv.id = e.from_capability AND cv.source = 'builtin'
		           AND cv.name IN ('github','cron','http','manual')
		  )`,

		// 起点边：左端从「能力 id」换成「来源名」（就是该能力的 name）
		`INSERT INTO edges_new (org_id, from_is_source, from_key, from_port, to_capability, created_at)
		 SELECT e.org_id, 1, cv.name, e.from_port, e.to_capability, e.created_at
		   FROM edges e
		   JOIN capability_versions cv ON cv.id = e.from_capability
		  WHERE cv.source = 'builtin'
		    AND cv.name IN ('github','cron','http','manual')`,

		`DROP TABLE edges`,
		`ALTER TABLE edges_new RENAME TO edges`,
		`CREATE INDEX IF NOT EXISTS idx_edges_lookup
		   ON edges(org_id, from_is_source, from_key, from_port)`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrate edges from_source: %w", err)
		}
	}
	return nil
}

// migrateNamespaces 迁移 12：命名空间隔离（#162）。
//
// 【历史快照】本函数（以及它引用的 nsRebuilds / ensureDefaultNamespacesTx /
// seedBuiltinsForAllNamespacesTx）描述的是**迁移 12 那个时点的 schema**：
// orgs 还在、业务表还带 org_id。迁移 14（migrateDropOrgLayer）之后这些列与表都不存在了，
// 所以运行时代码（建命名空间 / 播种 / 回填）**不得**再调用这里的辅助，
// 只能由迁移链在老库上按顺序重放。
//
// 三件事，缺一条就是线上事故：
//  1. 给每个已存在的 org 建 `default` 命名空间（代码里不存在「没有命名空间」的分支）
//  2. 重建 11 张资源表：加 namespace_id（带 FK 级联）+ 唯一键/主键前缀换成 namespace_id
//  3. 给每个命名空间播种内置能力（否则新命名空间里 notify.phone 缺席 → end 节点声明不出来）
//
// 为什么必须重建而不是 ALTER：
//   - SQLite 不能改主键/唯一约束（`UNIQUE(org_id, name, version)` → `(namespace_id, ...)`）
//   - SQLite 不能给已有表 ADD COLUMN 时带 REFERENCES（级联删命名空间靠它）
//
// 重建过程有两个坑，都会**静默丢行**，所以两个 PRAGMA 都是必须的：
//
//  1. `foreign_keys=ON` 时 `DROP TABLE` 会先隐式 DELETE 全部行，而那条 DELETE
//     会触发子表的 `ON DELETE CASCADE` —— 于是"删父表"顺手清空了还没搬走的子表行。
//     `PRAGMA foreign_keys=OFF` 在事务里是 no-op，所以改用**先改名再删**：
//     `ALTER TABLE t RENAME TO t_old` → 建新 t → 搬数据 → `DROP TABLE t_old`。
//     子表的外键子句仍然指向 `t`（见第 2 点），而 `t_old` 没有子表 → 删它不级联。
//
//  2. SQLite ≥3.25 的 RENAME 会**重写其它表里的外键引用**（把 references t 改成 t_old）。
//     `PRAGMA legacy_alter_table=ON` 关掉这个行为，子表继续指向 `t`。
//
//     外键检查本身再用 `PRAGMA defer_foreign_keys=TRUE` 推迟到提交时刻：
//     重建中途必然出现"父表已换、子表还没换"的瞬间，提交时终态一致即可。
//
// 幂等：以 capability_versions.namespace_id 是否存在作为「已迁移」判据。
func migrateNamespaces(tx *sql.Tx) error {
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = TRUE`); err != nil {
		return fmt.Errorf("defer foreign keys: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS namespaces (
		id         TEXT PRIMARY KEY,
		org_id     TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
		slug       TEXT NOT NULL,
		name       TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE (org_id, slug)
	)`); err != nil {
		return fmt.Errorf("create namespaces: %w", err)
	}
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_namespaces_org ON namespaces(org_id, created_at)`); err != nil {
		return fmt.Errorf("create idx_namespaces_org: %w", err)
	}

	// 已迁移过？新表有 namespace_id 列，旧表没有。
	if has, err := hasColumn(tx, "capability_versions", "namespace_id"); err != nil {
		return err
	} else if has {
		return nil
	}

	// ① 每个 org 一个 default 命名空间（老数据全部落到它下面）。
	if err := ensureDefaultNamespacesTx(tx); err != nil {
		return err
	}

	// ② 三段式重建：搬进 stage（回填 namespace_id）→ 删旧表 → 建新表回灌。
	//    namespace_id 必须在**旧表还在**的时候回填（子表要沿 node_id/source_id 找父行）。
	for _, t := range nsRebuilds {
		if err := stageForNamespaces(tx, t); err != nil {
			return err
		}
	}
	for _, name := range nsDropOrder {
		if err := dropOldForNamespaces(tx, name); err != nil {
			return err
		}
	}
	for _, t := range nsRebuilds {
		if err := restoreForNamespaces(tx, t); err != nil {
			return err
		}
	}

	// ③ 每个命名空间播种内置能力（含迁移前就存在的 default）。
	return seedBuiltinsForAllNamespacesTx(tx)
}

// nsRebuild 是一张表的重建方案。
type nsRebuild struct {
	table string
	// create 是建 `<table>_ns` 的完整 DDL（列顺序必须与 copy 的列列表配合）。
	create string
	// cols 是搬数据时显式列出的列（新旧表同名）。
	cols []string
	// nsExpr 是 namespace_id 的取值表达式（在旧表别名 t 上求值）。
	nsExpr string
	// indexes 是重建后要恢复的索引（DROP TABLE 会连带删掉旧索引）。
	indexes []string
}

// defaultNSExpr 取「该行 org 的默认命名空间」——老数据的 namespace_id 全部回填到它。
const defaultNSExpr = `(SELECT ns.id FROM namespaces ns WHERE ns.org_id = t.org_id ORDER BY ns.created_at, ns.id LIMIT 1)`

// nsOfNodeExpr / nsOfSourceExpr 是**子表**的回填写法：子表自己没有 org_id，
// 沿父行（nodes / sources）找它的 org，再取该 org 的默认命名空间。
//
// 注意这里必须用父表的 org_id 而不是 `nd.namespace_id`：stage 阶段读的是**旧表**，
// 那时 nodes/sources 还没有 namespace_id 列（迁移正是要加它）。
const (
	nsOfNodeExpr   = `(SELECT ns.id FROM nodes nd, namespaces ns WHERE nd.id = t.node_id AND ns.org_id = nd.org_id ORDER BY ns.created_at, ns.id LIMIT 1)`
	nsOfSourceExpr = `(SELECT ns.id FROM sources s, namespaces ns WHERE s.id = t.source_id AND ns.org_id = s.org_id ORDER BY ns.created_at, ns.id LIMIT 1)`
)

var nsRebuilds = []nsRebuild{
	{
		table: "capability_versions",
		create: `CREATE TABLE capability_versions (
			id            TEXT PRIMARY KEY,
			namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			org_id        TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
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
		)`,
		cols:   []string{"id", "org_id", "name", "display_name", "version", "params_schema", "outputs", "lifecycle", "sticky", "source", "content_hash", "created_at"},
		nsExpr: defaultNSExpr,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_cap_name_ver ON capability_versions(namespace_id, name, version)`,
		},
	},
	{
		table: "nodes",
		create: `CREATE TABLE nodes (
			id              TEXT PRIMARY KEY,
			namespace_id    TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			name            TEXT NOT NULL,
			tags            TEXT NOT NULL DEFAULT '[]',
			status          TEXT NOT NULL DEFAULT 'offline',
			token_hash      TEXT NOT NULL,
			max_instances   INTEGER NOT NULL DEFAULT 20,
			max_concurrency INTEGER NOT NULL DEFAULT 8,
			last_seen_at    INTEGER,
			created_at      INTEGER NOT NULL,
			UNIQUE (namespace_id, name)
		)`,
		cols:   []string{"id", "org_id", "name", "tags", "status", "token_hash", "max_instances", "max_concurrency", "last_seen_at", "created_at"},
		nsExpr: defaultNSExpr,
	},
	{
		table: "node_capabilities",
		create: `CREATE TABLE node_capabilities (
			node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
			namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			run_spec      TEXT NOT NULL DEFAULT '{}',
			PRIMARY KEY (node_id, capability_id)
		)`,
		cols:   []string{"node_id", "capability_id", "run_spec"},
		nsExpr: nsOfNodeExpr,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_nodecap_cap ON node_capabilities(capability_id)`,
		},
	},
	{
		table: "node_instances",
		create: `CREATE TABLE node_instances (
			id           TEXT PRIMARY KEY,
			node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			slot         INTEGER NOT NULL,
			concurrency  INTEGER NOT NULL DEFAULT 1,
			busy_task    TEXT,
			alive        INTEGER NOT NULL DEFAULT 1,
			updated_at   INTEGER NOT NULL,
			UNIQUE (node_id, slot)
		)`,
		cols:   []string{"id", "node_id", "slot", "concurrency", "busy_task", "alive", "updated_at"},
		nsExpr: nsOfNodeExpr,
	},
	{
		table: "node_pins",
		create: `CREATE TABLE node_pins (
			sticky_key   TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			selector     TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			last_used_at INTEGER NOT NULL
		)`,
		cols:   []string{"sticky_key", "org_id", "node_id", "selector", "created_at", "last_used_at"},
		nsExpr: nsOfNodeExpr,
	},
	{
		table: "edges",
		create: `CREATE TABLE edges (
			namespace_id    TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			from_is_source  INTEGER NOT NULL DEFAULT 0,
			from_key        TEXT NOT NULL,
			from_port       TEXT NOT NULL,
			to_capability   TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
			created_at      INTEGER NOT NULL,
			PRIMARY KEY (namespace_id, from_is_source, from_key, from_port, to_capability)
		)`,
		cols:   []string{"org_id", "from_is_source", "from_key", "from_port", "to_capability", "created_at"},
		nsExpr: defaultNSExpr,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_edges_lookup
			   ON edges(namespace_id, from_is_source, from_key, from_port)`,
		},
	},
	{
		table: "sources",
		create: `CREATE TABLE sources (
			id            TEXT PRIMARY KEY,
			namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			org_id        TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			name          TEXT NOT NULL,
			kind          TEXT NOT NULL,
			config        TEXT NOT NULL DEFAULT '{}',
			enabled       INTEGER NOT NULL DEFAULT 1,
			last_fired_at INTEGER,
			next_fire_at  INTEGER,
			created_at    INTEGER NOT NULL,
			UNIQUE (namespace_id, name)
		)`,
		cols:   []string{"id", "org_id", "name", "kind", "config", "enabled", "last_fired_at", "next_fire_at", "created_at"},
		nsExpr: defaultNSExpr,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_sources_org ON sources(org_id, created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_sources_due ON sources(enabled, next_fire_at)`,
		},
	},
	{
		table: "source_targets",
		create: `CREATE TABLE source_targets (
			source_id     TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
			capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
			namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			params        TEXT NOT NULL DEFAULT '{}',
			created_at    INTEGER NOT NULL,
			PRIMARY KEY (source_id, capability_id)
		)`,
		cols:   []string{"source_id", "capability_id", "params", "created_at"},
		nsExpr: nsOfSourceExpr,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_src_targets_cap ON source_targets(capability_id)`,
		},
	},
	{
		table: "events",
		create: `CREATE TABLE events (
			id           TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			trace_id     TEXT NOT NULL,
			source       TEXT NOT NULL,
			type         TEXT NOT NULL,
			payload      TEXT NOT NULL DEFAULT '{}',
			idem_key     TEXT,
			created_at   INTEGER NOT NULL
		)`,
		cols:   []string{"id", "org_id", "trace_id", "source", "type", "payload", "idem_key", "created_at"},
		nsExpr: defaultNSExpr,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_events_trace ON events(namespace_id, trace_id)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_events_idem ON events(namespace_id, source, idem_key)
			   WHERE idem_key IS NOT NULL`,
			`CREATE INDEX IF NOT EXISTS idx_events_retention ON events(namespace_id, created_at)`,
		},
	},
	{
		table: "tasks",
		create: `CREATE TABLE tasks (
			id             TEXT PRIMARY KEY,
			namespace_id   TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			org_id         TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
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
			out            TEXT,
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
		)`,
		cols:   []string{"id", "org_id", "trace_id", "parent_task_id", "path", "capability_id", "capability", "version", "params", "status", "node_id", "instance_id", "lease_until", "attempts", "out", "reported", "summary", "artifacts", "error", "cap_hash", "graph_id", "created_at", "started_at", "finished_at", "not_before"},
		nsExpr: defaultNSExpr,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_tasks_trace ON tasks(namespace_id, trace_id)`,
			`CREATE INDEX IF NOT EXISTS idx_tasks_parent ON tasks(namespace_id, parent_task_id)`,
			`CREATE INDEX IF NOT EXISTS idx_tasks_lease ON tasks(status, lease_until)`,
			`CREATE INDEX IF NOT EXISTS idx_tasks_node ON tasks(node_id, status)`,
			`CREATE INDEX IF NOT EXISTS idx_tasks_retention
			   ON tasks(namespace_id, capability_id, status, finished_at)`,
		},
	},
	{
		table: "graph_versions",
		create: `CREATE TABLE graph_versions (
			id           TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			org_id       TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
			content_hash TEXT NOT NULL,
			snapshot     TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			UNIQUE (namespace_id, content_hash)
		)`,
		cols:   []string{"id", "org_id", "content_hash", "snapshot", "created_at"},
		nsExpr: defaultNSExpr,
	},
}

// stageForNamespaces 把一张表的数据 + 回填后的 namespace_id 搬进无约束的 `<t>_stage`。
//
// 为什么用 `CREATE TABLE ... AS SELECT` 而不是带类型的建表：stage 只是过一手数据，
// 不需要约束/类型（SQLite 本就是动态类型）。少了约束，删旧表时的级联删除就碰不到它。
func stageForNamespaces(tx *sql.Tx, t nsRebuild) error {
	exists, err := hasTable(tx, t.table)
	if err != nil {
		return err
	}
	if !exists {
		return nil // 防御：表不存在说明无事可做（正常路径不会发生）
	}
	stmts := []string{
		fmt.Sprintf(`DROP TABLE IF EXISTS %s_stage`, t.table),
		fmt.Sprintf(`CREATE TABLE %s_stage AS
			SELECT %s, %s AS namespace_id FROM %s t`,
			t.table, prefixedCols(t.cols), t.nsExpr, t.table),
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("stage %s for namespaces: %w", t.table, err)
		}
	}
	return nil
}

// dropOldForNamespaces 删掉旧表。
//
// 按 nsRebuilds 的**逆序**删（子表在前）：父表的隐式 DELETE 会级联删子表行，
// 而那些子表本来也要删；先删子表能让级联无事可做，也避免任何顺序假设。
func dropOldForNamespaces(tx *sql.Tx, table string) error {
	exists, err := hasTable(tx, table)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE %s`, table)); err != nil {
		return fmt.Errorf("drop old %s for namespaces: %w", table, err)
	}
	return nil
}

// restoreForNamespaces 按新定义建表，从 stage 回灌数据，重建索引，最后删 stage。
func restoreForNamespaces(tx *sql.Tx, t nsRebuild) error {
	exists, err := hasTable(tx, t.table+"_stage")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	colList := strings.Join(t.cols, ", ")
	stmts := []string{
		t.create,
		fmt.Sprintf(`INSERT INTO %s (%s, namespace_id)
			SELECT %s, namespace_id FROM %s_stage`,
			t.table, colList, colList, t.table),
		fmt.Sprintf(`DROP TABLE %s_stage`, t.table),
	}
	stmts = append(stmts, t.indexes...)
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("restore %s for namespaces: %w", t.table, err)
		}
	}
	return nil
}

// nsDropOrder 是**删旧表的顺序**：必须先删所有引用者，再删被引用者。
//
// 为什么不能随便删：SQLite 每次 prepare 都会解析整个 schema，只要还剩一张表
// 的外键子句指向已经删掉的表，连 `DROP TABLE` 自己都会报
// 「no such table: main.<parent>」。所以顺序 = 依赖图的逆拓扑序（能力/节点最后删）。
var nsDropOrder = []string{
	"node_capabilities",   // → nodes, capability_versions
	"node_instances",      // → nodes
	"node_pins",           // → nodes
	"source_targets",      // → sources, capability_versions
	"edges",               // → capability_versions
	"tasks",               // → capability_versions
	"sources",             // → orgs
	"nodes",               // → orgs
	"capability_versions", // → orgs
	"events",              // → orgs
	"graph_versions",      // → orgs
}

// prefixedCols 给列名加旧表别名前缀（`id` → `t.id`），避免 SELECT 里子查询撞名。
func prefixedCols(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = "t." + c
	}
	return strings.Join(out, ", ")
}

// migrate 应用所有未记账的迁移，并返回本次实际应用的版本数。
//
// modify（测试注入用）在校验完版本序列后、执行前被调用，可修改待执行的迁移
// ——用于断言「迁移失败必须回滚且不记账」。
func migrate(db *sql.DB, modify func(*migrationSet)) (int, error) {
	set, err := parseMigrations(schemaSQL)
	if err != nil {
		return 0, fmt.Errorf("parse schema.sql: %w", err)
	}
	if modify != nil {
		modify(&set)
	}
	return migrateWith(db, set, migrationHooks)
}

// migrateWith 执行迁移：每个版本一个事务，成功才记账。
func migrateWith(db *sql.DB, set migrationSet, hooks map[int]func(*sql.Tx) error) (applied int, err error) {
	if _, err := db.Exec(schemaVersionTable); err != nil {
		return 0, fmt.Errorf("create schema_version: %w", err)
	}
	done, err := appliedVersions(db)
	if err != nil {
		return 0, err
	}
	for _, m := range set {
		if done[m.version] {
			continue
		}
		if err := applyMigration(db, m, hooks[m.version]); err != nil {
			return applied, fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		applied++
	}
	return applied, nil
}

// applyMigration 在单个事务内执行一个迁移并记账。
func applyMigration(db *sql.DB, m migration, hook func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if m.body != "" {
		if _, err := tx.Exec(m.body); err != nil {
			return fmt.Errorf("exec sql: %w", err)
		}
	}
	if hook != nil {
		if err := hook(tx); err != nil {
			return fmt.Errorf("hook: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, m.version, Now(),
	); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// appliedVersions 读出已记账的版本。
func appliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_version`)
	if err != nil {
		return nil, fmt.Errorf("query schema_version: %w", err)
	}
	defer rows.Close()
	done := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_version: %w", err)
		}
		done[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_version: %w", err)
	}
	return done, nil
}

// SchemaVersion 返回当前库的最高已应用版本（0 = 空库）。供健康检查/测试使用。
func (d *DB) SchemaVersion() (int, error) {
	var v sql.NullInt64
	if err := d.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// migrateLegacyColumns 迁移 2：老库补列（幂等）。
//
// 这些列在老库里缺失，而 SQLite 不支持 `ADD COLUMN IF NOT EXISTS`，
// 只能先查 pragma_table_info。错误一律向上抛，不再静默吞掉。
func migrateLegacyColumns(tx *sql.Tx) error {
	alters := []struct{ table, column, ddl string }{
		{"passkeys", "backup_eligible", `ALTER TABLE passkeys ADD COLUMN backup_eligible INTEGER NOT NULL DEFAULT 0`},
		{"cli_auth_sessions", "kind", `ALTER TABLE cli_auth_sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'apikey'`},
		{"cli_auth_sessions", "device_hint", `ALTER TABLE cli_auth_sessions ADD COLUMN device_hint TEXT NOT NULL DEFAULT ''`},
		{"sessions", "device_name", `ALTER TABLE sessions ADD COLUMN device_name TEXT NOT NULL DEFAULT ''`},
		{"users", "role", `ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member'`},
		{"users", "disabled", `ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`},
		{"devices", "event_scope", `ALTER TABLE devices ADD COLUMN event_scope TEXT NOT NULL DEFAULT 'final'`},
	}
	for _, a := range alters {
		has, err := hasColumn(tx, a.table, a.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := tx.Exec(a.ddl); err != nil {
			return fmt.Errorf("add column %s.%s: %w", a.table, a.column, err)
		}
	}
	// users.role 索引必须在 role 列存在后创建
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_users_role ON users(role)`); err != nil {
		return fmt.Errorf("create idx_users_role: %w", err)
	}
	return nil
}

// fixLegacyMessageColumns 迁移 3：老库 messages 表去掉早已废弃的 kind / reply_to 列。
//
// 删除回复能力后这两列不再需要；SQLite 不支持 DROP COLUMN 的低版本需重建表。
// 新库（迁移 3 直接建表）没有这两列，因此本 hook 是 no-op。
func fixLegacyMessageColumns(tx *sql.Tx) error {
	hasReply, err := hasColumn(tx, "messages", "reply_to")
	if err != nil {
		return err
	}
	if !hasReply {
		return nil
	}
	// 重建表：复制公共列，丢弃 kind / reply_to。
	// 注意 deliveries 有 message_id 外键 ON DELETE CASCADE，
	// 但这里全程在同一事务内且不删行，不需要关闭 foreign_keys。
	if _, err := tx.Exec(`CREATE TABLE messages_rebuild (
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
	)`); err != nil {
		return fmt.Errorf("create messages_rebuild: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO messages_rebuild
		(id, user_id, seq, title, agent_state, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at)
		SELECT id, user_id, seq, title, agent_state, body, link, device_tags, priority, ttl_seconds, payload, created_at, expires_at
		FROM messages`); err != nil {
		return fmt.Errorf("copy messages: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE messages`); err != nil {
		return fmt.Errorf("drop messages: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE messages_rebuild RENAME TO messages`); err != nil {
		return fmt.Errorf("rename messages_rebuild: %w", err)
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_user_seq ON messages(user_id, seq)`); err != nil {
		return fmt.Errorf("recreate idx_messages_user_seq: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_user_created ON messages(user_id, created_at)`); err != nil {
		return fmt.Errorf("recreate idx_messages_user_created: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_expires ON messages(expires_at)`); err != nil {
		return fmt.Errorf("recreate idx_messages_expires: %w", err)
	}
	return nil
}

// hasColumn 判断表是否有某列。表不存在时返回 false, nil。
func hasColumn(tx *sql.Tx, table, column string) (bool, error) {
	var n int
	if err := tx.QueryRow(
		`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("probe column %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

// hasTable 判断表是否存在（SQLite 没有 information_schema）。
func hasTable(tx *sql.Tx, table string) (bool, error) {
	var n int
	if err := tx.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("probe table %s: %w", table, err)
	}
	return n > 0, nil
}

// idHashSuffix 取 content_hash 的前 8 位（能力/图版本 ID 用）。
func idHashSuffix(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}
