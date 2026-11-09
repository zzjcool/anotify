package v4test

import (
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// v4 baseline 的结构门禁：表/列/索引/唯一约束的**终态形状**。
//
// 这些断言是「库长什么样」的唯一验收：schema.sql 是唯一事实源，任何一处
// 漏建 / 多建 / 列名漂移都会让后续 store 代码在运行期才炸（而且是业务路径上）。
// 所以这里逐条写死，不靠「跑起来没报错」当证据。

// baselineTables 是 v4 baseline 的完整表集合（一个不多一个不少）。
var baselineTables = []string{
	"api_keys",
	"capability_versions",
	"cli_auth_sessions",
	"deployments",
	"devices",
	"event_deliveries",
	"edges",
	"emits",
	"events",
	"namespace_configs",
	"namespaces",
	"node_configs",
	"node_instances",
	"nodes",
	"passkeys",
	"playbook_installs",
	"playbooks",
	"recovery_codes",
	"sessions",
	"sources",
	"system_config",
	"tasks",
	"users",
}

// removedTables 是 v3 里存在、v4 明确删掉的表。
var removedTables = []string{
	"node_capabilities",
	"node_pins",
	"graph_versions",
	"artifacts",
	"task_logs",
	"emissions",
}

// TestSchemaBaselineTableSet 表集合与 baselineTables 完全一致。
func TestSchemaBaselineTableSet(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	sort.Strings(got)
	want := append([]string{}, baselineTables...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("表集合不对:\n got %v\nwant %v", got, want)
	}
}

// TestSchemaBaselineNoHistoricalBaggage v3 的退役结构与列都不在了。
func TestSchemaBaselineNoHistoricalBaggage(t *testing.T) {
	db := openTestDB(t)
	for _, name := range removedTables {
		if tableExists(t, db, name) {
			t.Errorf("退役表 %q 仍在 baseline 里", name)
		}
	}
	// capability_versions：旧语义列全删。
	caps := columnNames(t, db, "capability_versions")
	for _, banned := range []string{
		"outputs", "params_schema", "lifecycle", "sticky", "source",
		"content_hash", "balance", "handler",
	} {
		if caps[banned] {
			t.Errorf("capability_versions 仍带着退役列 %q", banned)
		}
	}
	// nodes：集群级对象，不绑 ns。
	if columnNames(t, db, "nodes")["namespace_id"] {
		t.Error("nodes 仍带着 namespace_id（v4 节点是集群级）")
	}
	// tasks：v3 的 status / attempts / 旧列都退役。
	tasks := columnNames(t, db, "tasks")
	for _, banned := range []string{"status", "attempts", "instance_id", "cap_hash", "graph_id", "path", "token", "not_before"} {
		if tasks[banned] {
			t.Errorf("tasks 仍带着退役列 %q", banned)
		}
	}
	// edges：端口 / 映射 / 字面量 / 优先级都不在。
	edges := columnNames(t, db, "edges")
	for _, banned := range []string{"from_port", "literals", "mapping", "priority", "position"} {
		if edges[banned] {
			t.Errorf("edges 仍带着退役列 %q", banned)
		}
	}
	// node_instances：去 namespace_id。
	if columnNames(t, db, "node_instances")["namespace_id"] {
		t.Error("node_instances 仍带着 namespace_id")
	}
}

// TestSchemaBaselineCapabilityColumns 关键表的列集合（逐表精确比对）。
func TestSchemaBaselineCapabilityColumns(t *testing.T) {
	db := openTestDB(t)

	wantCols := map[string][]string{
		"capability_versions": {
			"id", "namespace_id", "name", "display_name", "version", "spec", "digest",
			"fold_key", "installed_from", "resource_version", "created_at",
		},
		"edges": {"namespace_id", "from_is_source", "from_ref", "to_ref", "created_at"},
		"node_configs": {
			"node_id", "namespaces", "vars", "limits", "local_vars", "local_limits",
			"revision", "resource_version", "applied_revision", "applied_hash",
			"applied_at", "updated_at",
		},
		"namespace_configs": {
			"namespace_id", "vars", "revision", "resource_version", "updated_at",
		},
		"system_config": {
			"id", "retention_event_ttl_sec", "retention_task_ttl_sec",
			"retention_result_ttl_sec", "resource_version", "updated_at",
		},
		"node_instances": {"id", "node_id", "slot", "concurrency", "busy_task", "alive", "updated_at"},
		"playbooks": {
			"id", "namespace_id", "slug", "name", "descr", "version", "files", "source",
			"resource_version", "created_at", "updated_at",
		},
		"playbook_installs": {
			// playbook_id 是**历史 id**（无 live-parent 外键），slug/ns 快照让删包后
			// 回执仍可读、且不会被同名重建的新包认领（父裁决 D3）。
			"id", "namespace_id", "namespace_slug", "playbook_id", "playbook_slug",
			"values", "values_digest", "receipt", "source", "created_at",
		},
		"sources": {
			"id", "namespace_id", "name", "kind", "config", "targets", "enabled",
			"last_fired_at", "next_fire_at", "installed_from", "resource_version", "created_at",
		},
	}
	for table, want := range wantCols {
		got := columnNames(t, db, table)
		for _, col := range want {
			if !got[col] {
				t.Errorf("表 %s 缺列 %q（现有 %v）", table, col, sortedKeys(got))
			}
		}
		if len(got) != len(want) {
			t.Errorf("表 %s 列数 = %d, want %d（现有 %v）", table, len(got), len(want), sortedKeys(got))
		}
	}

	// tasks 的关键 v4 列（一逻辑任务一行 + attempt 计数器 + 租约 + 等待态）。
	tasks := columnNames(t, db, "tasks")
	for _, col := range []string{
		"attempt", "max_attempts", "node_id", "node_name", "config_revision",
		"lease_id", "lease_until", "attempt_token_hash", "waiting_reason",
		"next_attempt_at", "reported", "digest", "phase",
	} {
		if !tasks[col] {
			t.Errorf("tasks 缺列 %q", col)
		}
	}
	// emits：emit_id 幂等记录。
	emits := columnNames(t, db, "emits")
	for _, col := range []string{"id", "task_id", "emit_id", "to_ref", "data", "result", "created_at"} {
		if !emits[col] {
			t.Errorf("emits 缺列 %q", col)
		}
	}
}

// TestSchemaBaselineIndexes 关键索引（查询形状是契约的一部分）。
func TestSchemaBaselineIndexes(t *testing.T) {
	db := openTestDB(t)

	if cols := indexColumnsOn(t, db, "capability_versions", "idx_cap_name_ver"); len(cols) != 3 ||
		cols[0] != "namespace_id" || cols[1] != "name" || cols[2] != "version" {
		t.Fatalf("idx_cap_name_ver = %v", cols)
	}
	if cols := indexColumnsOn(t, db, "capability_versions", "idx_cap_fold_key"); len(cols) != 3 ||
		cols[2] != "fold_key" {
		t.Fatalf("idx_cap_fold_key = %v", cols)
	}
	// 边：前缀查下游。
	if cols := indexColumnsOn(t, db, "edges", "idx_edges_from"); len(cols) != 3 ||
		cols[0] != "namespace_id" || cols[1] != "from_is_source" || cols[2] != "from_ref" {
		t.Fatalf("idx_edges_from = %v", cols)
	}
	for _, idx := range []string{"idx_events_trace", "idx_events_retention"} {
		if len(indexColumnsOn(t, db, "events", idx)) != 2 {
			t.Fatalf("%s 列数不对", idx)
		}
	}
	for _, idx := range []string{
		"idx_tasks_retention", "idx_tasks_ns_created", "idx_tasks_lease",
		"idx_tasks_parent", "idx_tasks_trace",
	} {
		if len(indexColumnsOn(t, db, "tasks", idx)) < 2 {
			t.Fatalf("%s 列数不对", idx)
		}
	}
}

// TestSchemaBaselineUniqueConstraints 唯一约束（身份与幂等的落点）。
func TestSchemaBaselineUniqueConstraints(t *testing.T) {
	db := openTestDB(t)

	if !hasUniqueIndex(t, db, "capability_versions", []string{"namespace_id", "name", "version"}) {
		t.Error("capability_versions 缺 UNIQUE(namespace_id,name,version)")
	}
	if !hasUniqueIndex(t, db, "capability_versions", []string{"namespace_id", "digest"}) {
		t.Error("capability_versions 缺 UNIQUE(namespace_id,digest)")
	}
	for _, tc := range []struct {
		table string
		cols  []string
	}{
		// 去重窗与事件实体解耦（父裁决 D6）：唯一性落在 event_deliveries 上，
		// 而不是 events 本体——否则「旧事件因引用保留」与「同 id 过期后可再接收」
		// 会互相排斥。
		{"event_deliveries", []string{"namespace_id", "source", "delivery_id"}},
		{"emits", []string{"task_id", "emit_id"}},
		{"deployments", []string{"node_id", "capability_id", "digest"}},
	} {
		if !hasUniqueIndex(t, db, tc.table, tc.cols) {
			t.Errorf("%s 缺唯一索引 %v", tc.table, tc.cols)
		}
	}
	// 反向断言：events 上**不能**再有 delivery 级唯一索引（那会把去重绑死在实体上）。
	if hasUniqueIndex(t, db, "events", []string{"namespace_id", "source", "delivery_id"}) {
		t.Error("events 不该有 (namespace_id,source,delivery_id) 唯一索引：去重窗必须独立于事件实体")
	}
}

// hasUniqueIndex 判定表上是否存在以 cols 为前导列的 UNIQUE 索引 / 约束。
func hasUniqueIndex(t *testing.T, db *store.DB, table string, cols []string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_index_list(?) WHERE "unique" = 1`, table)
	if err != nil {
		t.Fatalf("index_list %s: %v", table, err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatalf("scan index name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close index_list: %v", err)
	}
	for _, name := range names {
		got := indexColumnsOn(t, db, table, name)
		if len(got) < len(cols) {
			continue
		}
		match := true
		for i, col := range cols {
			if got[i] != col {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestSchemaBaselineReopenIsIdempotent 同一个库重开两次：结构不变。
func TestSchemaBaselineReopenIsIdempotent(t *testing.T) {
	db, path := openTestDBWithPath(t)
	first := masterDDL(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if second := masterDDL(t, db2); second != first {
		t.Fatalf("重开后结构变了:\n first %s\nsecond %s", first, second)
	}
}

// masterDDL 把 sqlite_master 的 DDL 拍成一段稳定文本（结构幂等断言用）。
func masterDDL(t *testing.T, db *store.DB) string {
	t.Helper()
	rows, err := db.Query(
		`SELECT type, name, tbl_name, sql FROM sqlite_master
		  WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("master: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, tbl string
		var ddl sql.NullString
		if err := rows.Scan(&typ, &name, &tbl, &ddl); err != nil {
			t.Fatalf("scan master: %v", err)
		}
		b.WriteString(typ + "|" + name + "|" + tbl + "|" + ddl.String + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate master: %v", err)
	}
	return b.String()
}

// sortedKeys 返回 map 的键排序结果（错误消息用）。
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSchemaBaselineNodeFactsColumns G1-3 的节点事实列（hello 快照的落点）。
//
// 这些列不是「可选缓存」：没有它们，NodesAvailable 的 CommandMissing /
// NamespaceUnauthorized 与节点页的 status.* 都答不出来（anode.md §0/§3.3/§6）。
func TestSchemaBaselineNodeFactsColumns(t *testing.T) {
	db := openTestDB(t)
	nodes := columnNames(t, db, "nodes")
	for _, col := range []string{"facts", "declared_capabilities", "declared_namespaces"} {
		if !nodes[col] {
			t.Errorf("nodes 缺列 %q（现有 %v）", col, sortedKeys(nodes))
		}
	}
	cfg := columnNames(t, db, "node_configs")
	for _, col := range []string{"local_vars", "local_limits"} {
		if !cfg[col] {
			t.Errorf("node_configs 缺列 %q（现有 %v）", col, sortedKeys(cfg))
		}
	}
}
