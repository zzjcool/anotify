package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// migrateDropOrgLayer 是迁移 14（`drop_org_layer`，#205）的 Go 实现：
// 删掉「组织」层 —— `orgs` / `org_members` 两张表、10 张表上的 `org_id` 列，
// 以及 `namespaces` 的唯一键从 `(org_id, slug)` 变成 `(slug)`。
//
// 为什么必须用「stage → drop → restore」三段式：
//
//	SQLite 没有 DROP COLUMN，更不能改唯一键；而父表被 DROP 时会做一次**隐式
//	DELETE**，`ON DELETE CASCADE` 会把子表数据一起带走 —— 所以先把每一行搬进
//	无约束的 `<t>_stage`（`CREATE TABLE ... AS SELECT`），再删表、建新表、回灌。
//	这与迁移 12（migrateNamespaces）同一套手法，区别是这次连 org_id 一起摘掉。
//
// 幂等：
//   - 最后一步删的就是 `orgs`，所以「`orgs` 不存在」= 已迁移，直接返回；
//   - 每个 stage 表先 `DROP TABLE IF EXISTS`；
//   - 去重只在真正存在重复时才 UPDATE，第二次执行时没有重复行，天然无操作。
func migrateDropOrgLayer(tx *sql.Tx) error {
	// 重建期间外键检查延到提交。
	//
	// 注意它**不是**「回灌顺序」的需要：回灌本来就按父表在前（orgLayerRebuilds 的顺序）。
	// 它的实际收益是**失败点集中且不静默**——老库里若存在历史孤儿行
	// （例如 tasks.capability_id 指向已删的能力），开启 defer 时失败发生在 commit 的
	// `FOREIGN KEY constraint failed`，整个迁移完整回滚；不开则可能在某一句话
	// INSERT 处失败，报错位置更靠后、更难判断是哪一行数据脏。升级老库前建议先跑
	// `PRAGMA foreign_key_check`。
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = TRUE`); err != nil {
		return fmt.Errorf("defer foreign keys: %w", err)
	}

	// 已经迁移过？迁移 14 的最后一步是 DROP TABLE orgs。
	exists, err := hasTable(tx, "orgs")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	// ① slug 全局唯一化，必须在「旧 namespaces 还带着 org_id」时做（按 slug 分组无关 org）。
	if err := dedupeNamespaceSlugsTx(tx); err != nil {
		return err
	}

	// ② 搬进 stage（此时旧表还在，可以逐列 SELECT）。
	for _, t := range orgLayerRebuilds {
		if err := stageForOrgLayer(tx, t); err != nil {
			return err
		}
	}

	// ③ 逆拓扑序删旧表：引用者先删，被引用者后删（避免隐式 DELETE 触发级联）。
	for _, name := range orgLayerDropOrder {
		if err := dropTableForOrgLayer(tx, name); err != nil {
			return err
		}
	}

	// ④ 建 org-free 新表 + 回灌 + 重建索引。
	for _, t := range orgLayerRebuilds {
		if err := restoreForOrgLayer(tx, t); err != nil {
			return err
		}
	}
	return nil
}

// orgLayerRebuild 是一张表在迁移 14 里的重建方案。
type orgLayerRebuild struct {
	table string
	// create 是建新表的完整 DDL（列顺序必须与 cols 一致，回灌时按 cols 显式列名）。
	create string
	// cols 是搬数据时的列（新旧表同名，**已去掉 org_id**）。
	cols []string
	// indexes 是重建后要恢复的索引（DROP TABLE 会连带删掉旧索引）。
	indexes []string
}

// orgLayerRebuilds 是**建表/回灌顺序**：父表在前，子表在后。
//
// node_capabilities / node_instances / source_targets 自己没有 org_id（迁移 12 起），
// 但它们的父表 nodes / sources / capability_versions 要删了重建 —— DROP 父表会级联
// 删掉这些子行，所以它们也必须 stage → drop → restore，否则数据就丢了。
var orgLayerRebuilds = []orgLayerRebuild{
	{
		table: "namespaces",
		create: `CREATE TABLE namespaces (
			id         TEXT PRIMARY KEY,
			slug       TEXT NOT NULL,
			name       TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			UNIQUE (slug)
		)`,
		cols: []string{"id", "slug", "name", "created_at"},
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_namespaces_created ON namespaces(created_at)`,
		},
	},
	{
		table: "capability_versions",
		create: `CREATE TABLE capability_versions (
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
		)`,
		cols: []string{"id", "namespace_id", "name", "display_name", "version", "params_schema", "outputs", "lifecycle", "sticky", "source", "content_hash", "created_at"},
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_cap_name_ver ON capability_versions(namespace_id, name, version)`,
		},
	},
	{
		table: "nodes",
		create: `CREATE TABLE nodes (
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
		)`,
		cols: []string{"id", "namespace_id", "name", "tags", "status", "token_hash", "max_instances", "max_concurrency", "last_seen_at", "created_at"},
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
		cols: []string{"node_id", "capability_id", "namespace_id", "run_spec"},
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
		cols: []string{"id", "node_id", "namespace_id", "slot", "concurrency", "busy_task", "alive", "updated_at"},
	},
	{
		table: "node_pins",
		create: `CREATE TABLE node_pins (
			sticky_key   TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			selector     TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			last_used_at INTEGER NOT NULL
		)`,
		cols: []string{"sticky_key", "namespace_id", "node_id", "selector", "created_at", "last_used_at"},
	},
	{
		table: "edges",
		create: `CREATE TABLE edges (
			namespace_id    TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			from_is_source  INTEGER NOT NULL DEFAULT 0,
			from_key        TEXT NOT NULL,
			from_port       TEXT NOT NULL,
			to_capability   TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
			created_at      INTEGER NOT NULL,
			PRIMARY KEY (namespace_id, from_is_source, from_key, from_port, to_capability)
		)`,
		cols: []string{"namespace_id", "from_is_source", "from_key", "from_port", "to_capability", "created_at"},
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
			name          TEXT NOT NULL,
			kind          TEXT NOT NULL,
			config        TEXT NOT NULL DEFAULT '{}',
			enabled       INTEGER NOT NULL DEFAULT 1,
			last_fired_at INTEGER,
			next_fire_at  INTEGER,
			created_at    INTEGER NOT NULL,
			UNIQUE (namespace_id, name)
		)`,
		cols: []string{"id", "namespace_id", "name", "kind", "config", "enabled", "last_fired_at", "next_fire_at", "created_at"},
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_sources_ns ON sources(namespace_id, created_at)`,
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
		cols: []string{"source_id", "capability_id", "namespace_id", "params", "created_at"},
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_src_targets_cap ON source_targets(capability_id)`,
		},
	},
	{
		table: "events",
		create: `CREATE TABLE events (
			id           TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			trace_id     TEXT NOT NULL,
			source       TEXT NOT NULL,
			type         TEXT NOT NULL,
			payload      TEXT NOT NULL DEFAULT '{}',
			idem_key     TEXT,
			created_at   INTEGER NOT NULL
		)`,
		cols: []string{"id", "namespace_id", "trace_id", "source", "type", "payload", "idem_key", "created_at"},
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
		cols: []string{"id", "namespace_id", "trace_id", "parent_task_id", "path", "capability_id", "capability", "version", "params", "status", "node_id", "instance_id", "lease_until", "attempts", "out", "reported", "summary", "artifacts", "error", "cap_hash", "graph_id", "created_at", "started_at", "finished_at", "not_before"},
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
			content_hash TEXT NOT NULL,
			snapshot     TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			UNIQUE (namespace_id, content_hash)
		)`,
		cols: []string{"id", "namespace_id", "content_hash", "snapshot", "created_at"},
	},
	{
		table: "playbooks",
		create: `CREATE TABLE playbooks (
			id           TEXT PRIMARY KEY,
			namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			slug         TEXT NOT NULL,
			name         TEXT NOT NULL,
			descr        TEXT NOT NULL DEFAULT '',
			manifest     TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			UNIQUE (namespace_id, slug)
		)`,
		cols: []string{"id", "namespace_id", "slug", "name", "descr", "manifest", "created_at"},
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_playbooks_ns ON playbooks(namespace_id, created_at)`,
		},
	},
}

// orgLayerDropOrder 是**删表的逆拓扑序**：引用者一定排在它引用的表前面。
//
// 顺序为什么是正确性问题（实测，不是推测）：
//
//  1. **先 stage 完再删**，所以 DROP 父表的隐式 DELETE（外键开启时会触发
//     `ON DELETE CASCADE`）级联删掉的是已经搬进 `<t>_stage` 的副本，不影响结果；
//     但只要有**任何一张还没删的表**的外键子句还指向已删的表，后续的 DROP TABLE
//     会直接报 `no such table: main.<已删表>` —— SQLite 每次 prepare 都会解析整张
//     schema。所以「引用者先删」是硬要求。
//  2. 父表本身仍然要最后删：先删 `namespaces` 再去删子表，第 1 条就会命中。
//
// 最后才是 namespaces → org_members → orgs。
var orgLayerDropOrder = []string{
	"node_capabilities",   // → nodes, capability_versions
	"node_instances",      // → nodes
	"node_pins",           // → nodes
	"source_targets",      // → sources, capability_versions
	"edges",               // → capability_versions
	"tasks",               // → capability_versions
	"playbooks",           // → namespaces
	"sources",             // → namespaces
	"nodes",               // → namespaces
	"capability_versions", // → namespaces
	"events",              // → namespaces
	"graph_versions",      // → namespaces
	"namespaces",          // → orgs
	"org_members",         // → orgs, users
	"orgs",
}

// stageForOrgLayer 把一张表的数据搬进无约束的 `<t>_stage`（只搬去掉 org_id 的列）。
func stageForOrgLayer(tx *sql.Tx, t orgLayerRebuild) error {
	exists, err := hasTable(tx, t.table)
	if err != nil {
		return err
	}
	if !exists {
		return nil // 防御：表不存在说明无事可做（正常路径不会发生）
	}
	colList := strings.Join(t.cols, ", ")
	stmts := []string{
		fmt.Sprintf(`DROP TABLE IF EXISTS %s_stage`, t.table),
		// ORDER BY rowid：让 stage 的行序等于**旧表的插入顺序**，回灌后新表的 rowid
		// 顺序与旧表一致。DefaultNamespaceID 用 `ORDER BY rowid` 取「最早插入的那个」，
		// 它的语义就建立在这条顺序上（不依赖 SQLite 未承诺的扫描顺序）。
		fmt.Sprintf(`CREATE TABLE %s_stage AS SELECT %s FROM %s ORDER BY rowid`, t.table, colList, t.table),
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("stage %s without org layer: %w", t.table, err)
		}
	}
	return nil
}

// dropTableForOrgLayer 删掉一张旧表（`IF EXISTS` 让重复执行/缺表都无害）。
func dropTableForOrgLayer(tx *sql.Tx, table string) error {
	exists, err := hasTable(tx, table)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE %s`, table)); err != nil {
		return fmt.Errorf("drop old %s for org layer: %w", table, err)
	}
	return nil
}

// restoreForOrgLayer 按 org-free 定义建表，从 stage 回灌，重建索引，最后删 stage。
func restoreForOrgLayer(tx *sql.Tx, t orgLayerRebuild) error {
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
		fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s_stage`, t.table, colList, colList, t.table),
		fmt.Sprintf(`DROP TABLE %s_stage`, t.table),
	}
	stmts = append(stmts, t.indexes...)
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("restore %s without org layer: %w", t.table, err)
		}
	}
	return nil
}

// dedupeNamespaceSlugsTx 把重复的命名空间 slug 改成全局唯一。
//
// 迁移 12 的唯一键是 `(org_id, slug)`，所以老库里**每个组织**都可以有一个
// `default`；迁移 14 之后唯一域是全局的 `(slug)`，不去重就会在重建
// `namespaces` 时撞 UNIQUE。
//
// 规则（确定、可复现）：
//   - 同一个 slug 按 `(created_at, rowid)` 保留**最早**那一行，slug 不变；
//   - 其余行改名为 `<slug>-2`、`<slug>-3`…（长度上限 31，见 namespaceSlugPattern）；
//   - 候选名不能是任何**已有** slug（避免把 default 抢成另一个命名空间的原名）。
//
// 幂等：没有重复行时不执行任何 UPDATE。
func dedupeNamespaceSlugsTx(tx *sql.Tx) error {
	exists, err := hasTable(tx, "namespaces")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	type nsSlug struct{ id, slug string }
	rows, err := tx.Query(`SELECT id, slug FROM namespaces ORDER BY slug, created_at, rowid`)
	if err != nil {
		return fmt.Errorf("list namespaces for slug dedupe: %w", err)
	}
	var all []nsSlug
	reserved := map[string]bool{}
	for rows.Next() {
		var ns nsSlug
		if err := rows.Scan(&ns.id, &ns.slug); err != nil {
			rows.Close()
			return fmt.Errorf("scan namespace slug: %w", err)
		}
		all = append(all, ns)
		reserved[ns.slug] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate namespace slugs: %w", err)
	}
	rows.Close()

	used := map[string]bool{}
	for _, ns := range all {
		if !used[ns.slug] {
			used[ns.slug] = true // 最早那一行保留原名
			continue
		}
		cand, err := nextFreeNamespaceSlug(ns.slug, used, reserved)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE namespaces SET slug = ? WHERE id = ?`, cand, ns.id); err != nil {
			return fmt.Errorf("rename duplicate namespace slug %q: %w", ns.slug, err)
		}
		used[cand] = true
	}
	return nil
}

// nextFreeNamespaceSlug 给出 base 的第一个未占用变体（base-2、base-3…）。
//
// used 是已分配的最终 slug，reserved 是所有原始 slug（含还没轮到的行）——
// 两个集合都要避开，否则会把别人的名字抢走。
func nextFreeNamespaceSlug(base string, used, reserved map[string]bool) (string, error) {
	for n := 2; n < 10000; n++ {
		cand := namespaceSlugWithSuffix(base, n)
		if !used[cand] && !reserved[cand] {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no free namespace slug left for %q", base)
}

// namespaceSlugWithSuffix 拼出 `base-<n>`，并按 namespaceSlugPattern 的 31 字符上限截断 base。
func namespaceSlugWithSuffix(base string, n int) string {
	suffix := fmt.Sprintf("-%d", n)
	if room := maxNamespaceSlugLen - len(suffix); len(base) > room {
		base = base[:room]
	}
	return base + suffix
}
