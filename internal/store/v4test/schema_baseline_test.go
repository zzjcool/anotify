package v4test

import (
	"database/sql"
	"errors"
	"path/filepath"
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
	"task_artifact_manifests",
	"task_artifact_objects",
	"task_artifact_uploads",
	"task_attempts",
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
		`SELECT name FROM sqlite_master WHERE type='table' AND substr(name, 1, 7) <> 'sqlite_' AND name <> 'schema_version'`)
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
	for _, banned := range []string{"status", "attempts", "instance_id", "cap_hash", "graph_id", "path", "token", "not_before", "seq_in_trace"} {
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
			"fold_key", "installed_from", "resource_version", "created_at", "deleted_at",
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
		"node_instances": {
			"id", "node_id", "slot", "busy_task",
			// 每个 busy 槽绑到**具体一次执行**（task + attempt + lease）：只按 task_id
			// 没法区分同一逻辑任务重叠的旧/新 attempt，旧 ACK 会烧掉槽位（G1-6S）。
			// reservation（busy_lease_id 为空）还必须带候选 assignment 与有界窗口：
			// 预算只排除同槽同 assignment 的那一条，旧 assignment 不能清新预约。
			"busy_attempt", "busy_lease_id", "busy_assignment_id", "busy_expires_at",
			"alive", "updated_at",
		},
		"task_attempts": {
			"task_id", "attempt", "node_id", "node_name", "instance_id", "slot",
			"lease_id", "attempt_token_hash", "config_revision", "config_hash",
			"machine_vars", "machine_var_sources", "machine_limits",
			"namespace_vars_revision", "phase", "error",
			"started_at", "finished_at", "reaped_at", "reclaim_reason",
			"capability_id", "artifacts", "result_json", "result_accepted_at",
		},
		"task_artifact_objects":   {"id", "created_at", "expired_at", "purged_at"},
		"task_artifact_uploads":   {"id", "task_id", "attempt", "path", "sha256", "size", "object_id", "expires_at"},
		"task_artifact_manifests": {"task_id", "attempt", "namespace_id", "capability_id", "path", "sha256", "size", "object_id", "published_at", "expires_at", "expired_at", "result_ref"},
		"playbooks": {
			"id", "namespace_id", "slug", "name", "display_name", "descr", "version",
			"files", "source", "resource_version", "created_at", "updated_at",
		},
		"playbook_installs": {
			// playbook_id 是**历史 id**（无 live-parent 外键），slug/ns 快照让删包后
			// 回执仍可读、且不会被同名重建的新包认领（父裁决 D3）。
			"id", "namespace_id", "namespace_slug", "playbook_id", "playbook_slug",
			"values", "values_digest", "receipt", "source", "created_at",
		},
		"sources": {
			"id", "namespace_id", "name", "display_name", "kind", "config", "targets", "enabled",
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
		"next_attempt_at", "reported", "digest", "phase", "result_json", "result_accepted_at",
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

// TestSchemaBaselineIndexes 固定当前 v4 查询索引的名字、表、有序列和 unique/partial。
// 期望独立写死，不从 schemaSQL 或 validateV4Layout 的同源期望中推导。
func TestSchemaBaselineIndexes(t *testing.T) {
	db := openTestDB(t)
	indexes := []struct {
		name, table string
		cols        []string
		unique      bool
		predicate   string
	}{
		{name: "idx_users_role", table: "users", cols: []string{"role"}},
		{name: "idx_sessions_user", table: "sessions", cols: []string{"user_id"}},
		{name: "idx_apikeys_user", table: "api_keys", cols: []string{"user_id"}},
		{name: "idx_devices_user", table: "devices", cols: []string{"user_id"}},
		{name: "idx_cli_auth_expires", table: "cli_auth_sessions", cols: []string{"expires_at"}},
		{name: "idx_recovery_codes_user", table: "recovery_codes", cols: []string{"user_id"}},
		{name: "idx_recovery_codes_prefix", table: "recovery_codes", cols: []string{"prefix"}},
		{name: "idx_namespaces_created", table: "namespaces", cols: []string{"created_at"}},
		{name: "idx_cap_live_name_ver", table: "capability_versions", cols: []string{"namespace_id", "name", "version"}, unique: true, predicate: "deleted_at is null"},
		{name: "idx_cap_live_digest", table: "capability_versions", cols: []string{"namespace_id", "digest"}, unique: true, predicate: "deleted_at is null"},
		{name: "idx_cap_name_ver", table: "capability_versions", cols: []string{"namespace_id", "name", "version"}},
		{name: "idx_cap_fold_key", table: "capability_versions", cols: []string{"namespace_id", "name", "fold_key"}},
		{name: "idx_edges_from", table: "edges", cols: []string{"namespace_id", "from_is_source", "from_ref"}},
		{name: "idx_events_trace", table: "events", cols: []string{"namespace_id", "trace_id"}},
		{name: "idx_events_retention", table: "events", cols: []string{"namespace_id", "created_at"}},
		{name: "idx_event_deliveries_expiry", table: "event_deliveries", cols: []string{"expires_at"}},
		{name: "idx_tasks_trace", table: "tasks", cols: []string{"namespace_id", "trace_id"}},
		{name: "idx_tasks_parent", table: "tasks", cols: []string{"namespace_id", "parent_task_id"}},
		{name: "idx_tasks_lease", table: "tasks", cols: []string{"phase", "lease_until"}},
		{name: "idx_tasks_node", table: "tasks", cols: []string{"node_id", "phase"}},
		{name: "idx_tasks_retention", table: "tasks", cols: []string{"namespace_id", "phase", "finished_at"}},
		{name: "idx_tasks_ns_created", table: "tasks", cols: []string{"namespace_id", "created_at", "id"}},
		{name: "idx_tasks_pending_created_at", table: "tasks", cols: []string{"created_at"}, predicate: "phase = 'pending'"},
		{name: "idx_task_attempts_node", table: "task_attempts", cols: []string{"node_id", "attempt"}},
		{name: "idx_task_attempts_unreaped", table: "task_attempts", cols: []string{"task_id"}, predicate: "reaped_at is null"},
		{name: "idx_emits_task_emit", table: "emits", cols: []string{"task_id", "emit_id"}, unique: true, predicate: "emit_id is not null"},
		{name: "idx_emits_task", table: "emits", cols: []string{"task_id"}},
		{name: "idx_deploy_node", table: "deployments", cols: []string{"node_id"}},
		{name: "idx_deploy_capability", table: "deployments", cols: []string{"capability_id"}},
		{name: "idx_playbooks_ns", table: "playbooks", cols: []string{"namespace_id", "created_at"}},
		{name: "idx_playbook_installs_lookup", table: "playbook_installs", cols: []string{"namespace_id", "playbook_id", "values_digest", "created_at"}},
		{name: "idx_sources_ns", table: "sources", cols: []string{"namespace_id", "created_at"}},
		{name: "idx_sources_due", table: "sources", cols: []string{"enabled", "next_fire_at"}},
		{name: "idx_task_artifact_objects", table: "task_artifact_manifests", cols: []string{"object_id"}},
		{name: "idx_task_artifact_upload_expiry", table: "task_artifact_uploads", cols: []string{"expires_at"}},
	}
	for _, want := range indexes {
		t.Run(want.name, func(t *testing.T) {
			var unique, partial bool
			if err := db.QueryRow(
				`SELECT "unique", partial FROM pragma_index_list(?) WHERE name=?`,
				want.table, want.name).Scan(&unique, &partial); err != nil {
				t.Fatalf("index %s on %s: %v", want.name, want.table, err)
			}
			if unique != want.unique || partial != (want.predicate != "") {
				t.Fatalf("%s unique/partial = %v/%v, want %v/%v", want.name, unique, partial, want.unique, want.predicate != "")
			}
			if got := indexColumnsOn(t, db, want.table, want.name); strings.Join(got, "\x00") != strings.Join(want.cols, "\x00") {
				t.Fatalf("%s ordered columns = %v, want %v", want.name, got, want.cols)
			}
			if want.predicate != "" {
				var ddl string
				if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, want.name).Scan(&ddl); err != nil {
					t.Fatal(err)
				}
				compact := strings.ToLower(strings.Join(strings.Fields(ddl), " "))
				if !strings.HasSuffix(compact, " where "+want.predicate) {
					t.Fatalf("%s partial predicate: %s, want %s", want.name, ddl, want.predicate)
				}
			}
		})
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
		{"task_artifact_manifests", []string{"task_id", "attempt", "path"}},
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

// hasUniqueIndex 只匹配完整、按序、没有表达式的身份键；部分索引仅允许非 NULL emit 键。
func hasUniqueIndex(t *testing.T, db *store.DB, table string, cols []string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name, partial FROM pragma_index_list(?) WHERE "unique"=1`, table)
	if err != nil {
		t.Fatal(err)
	}
	type index struct {
		name    string
		partial bool
	}
	var indexes []index
	for rows.Next() {
		var i index
		if err := rows.Scan(&i.name, &i.partial); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		indexes = append(indexes, i)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, i := range indexes {
		rows, err := db.Query(`SELECT cid, name FROM pragma_index_xinfo(?) WHERE key=1 ORDER BY seqno`, i.name)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		ordinary := true
		for rows.Next() {
			var cid int
			var name sql.NullString
			if err := rows.Scan(&cid, &name); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			if cid < 0 || !name.Valid {
				ordinary = false
			}
			got = append(got, name.String)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !ordinary || len(got) != len(cols) || strings.Join(got, "\x00") != strings.Join(cols, "\x00") {
			continue
		}

		if table == "capability_versions" && !i.partial {
			continue
		}
		if i.partial {
			var wantPredicate string
			switch table {
			case "emits":
				if strings.Join(cols, ",") != "task_id,emit_id" {
					continue
				}
				wantPredicate = "emit_id is not null"
			case "capability_versions":
				wantPredicate = "deleted_at is null"
			default:
				continue
			}
			var ddl string
			if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name=?`, i.name).Scan(&ddl); err != nil {
				t.Fatal(err)
			}
			compact := strings.ToLower(strings.Join(strings.Fields(ddl), " "))
			wantDDL := "create unique index " + strings.ToLower(i.name) + " on " + table + "(" + strings.Join(cols, ", ") + ") where " + wantPredicate
			if compact != wantDDL {
				continue
			}
		}
		return true
	}
	return false
}

func TestSchemaBaselinePrimaryKeyIdentity(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(`SELECT name FROM pragma_table_xinfo('task_attempts') WHERE pk>0 ORDER BY pk`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "task_id,attempt" {
		t.Fatalf("attempt PK identity = %v", names)
	}
	if !hasUniqueIndex(t, db, "nodes", []string{"name"}) {
		t.Fatal("nodes.name missing exact UNIQUE identity")
	}
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
		  WHERE substr(name, 1, 7) <> 'sqlite_' ORDER BY type, name`)
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

// TestSchemaBaselinePlaybookNoLiveParentFK 是「删包仍可读回执」的结构前提（playbook.md §7.3）：
// `playbook_installs.playbook_id` 是**历史 id**，不能有 live-parent 外键；
// `capability_versions/sources.installed_from` 也只是逻辑引用（无 FK），
// 引用安全由显式 GC 的引用检查保证，而不是「CASCADE 顺手删掉」。
func TestSchemaBaselinePlaybookNoLiveParentFK(t *testing.T) {
	db := openTestDB(t)

	fks := foreignKeyColumns(t, db, "playbook_installs")
	if target, ok := fks["playbook_id"]; ok {
		t.Fatalf("playbook_installs.playbook_id 不能有外键（删包会连带删回执），实际指向 %q", target)
	}
	if _, ok := fks["namespace_id"]; !ok {
		t.Fatalf("playbook_installs.namespace_id 应保留外键（删命名空间才级联删回执）")
	}
	for _, table := range []string{"capability_versions", "sources"} {
		if target, ok := foreignKeyColumns(t, db, table)["installed_from"]; ok {
			t.Fatalf("%s.installed_from 不能有外键（回执可被显式 GC），实际指向 %q", table, target)
		}
	}
}

// foreignKeyColumns 返回某表的外键列 → 目标表（无外键时返回空 map）。
func foreignKeyColumns(t *testing.T, db *store.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT "from", "table" FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		t.Fatalf("foreign_key_list %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var from, target string
		if err := rows.Scan(&from, &target); err != nil {
			t.Fatalf("scan foreign key: %v", err)
		}
		out[from] = target
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign keys: %v", err)
	}
	return out
}

// TestSchemaBaselineLayoutMarkerIsSeparateFromLedger v4 的「布局格式」与「记账版本」
// 是两个独立事实：schema_version 记 1（baseline 跑过），PRAGMA user_version 记 4（表结构是 v4）。
//
// 为什么必须分开：v3 的库同样记着 schema_version=1，只看记账根本分不出
// 「v4 新建的库」和「v3 的库」——那正是 G1-6C1 之前 Open 放行 v3 库的根因。
func TestSchemaBaselineLayoutMarkerIsSeparateFromLedger(t *testing.T) {
	db, path := openTestDBWithPath(t)
	if got := rawUserVersion(t, path); got != 4 {
		t.Fatalf("新库 PRAGMA user_version = %d, want 4（v4 布局格式标记）", got)
	}
	if v, err := db.SchemaVersion(); err != nil || v != 1 {
		t.Fatalf("SchemaVersion = %d, %v; want 1（baseline 记账，不是布局标记）", v, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&n); err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if n != 1 {
		t.Fatalf("schema_version 行数 = %d, want 1", n)
	}
}

// TestSchemaBaselineEveryTableRequiredByFormat 独立固定表集合对运行期 schema-derived 门禁的反例：
// 少任何一张表，marker=4 的库都必须被拒绝（而不是被 CREATE IF NOT EXISTS 补齐）。
//
// 这条用例不借用运行期参照结构；独立删表输入确保每张 baseline 表都真正参与拒绝。
func TestSchemaBaselineEveryTableRequiredByFormat(t *testing.T) {
	for _, table := range baselineTables {
		t.Run(table, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "drop-one.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatalf("create valid v4 db: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, `DROP TABLE "`+table+`"`)
			if err := raw.Close(); err != nil {
				t.Fatalf("close raw: %v", err)
			}
			before := snapshotDatabase(t, path)

			_, err = store.Open(path)
			if err == nil {
				t.Fatalf("marker=4 但缺表 %q 的库应该被拒绝，实际 Open 成功", table)
			}
			if !errors.Is(err, store.ErrSchemaInvalid) {
				t.Fatalf("应返回 ErrSchemaInvalid，实际: %v", err)
			}
			if !strings.Contains(err.Error(), "缺表 "+table) {
				t.Fatalf("错误信息应点名缺表 %q: %v", table, err)
			}
			if after := snapshotDatabase(t, path); after != before {
				t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
			}
			if rawTableExists(t, path, table) {
				t.Fatalf("坏库被自愈了：%q 又被建出来了", table)
			}
		})
	}
}

// TestSchemaBaselineRetiredColumnsRejectedByFormat v3 的退役列一旦出现在 marker=4 的库里，
// 必须被拒绝：列在、v4 代码不读它 = 「能写进去、读出来丢」。
//
// 与 TestSchemaBaselineNoHistoricalBaggage（正向断言「新库里没有这些列」）成对：
// 那条管「新建的库干净」，这条管「不干净的库不许被当成 v4 打开」。
func TestSchemaBaselineRetiredColumnsRejectedByFormat(t *testing.T) {
	cases := []struct{ table, column string }{
		{"nodes", "namespace_id"},
		{"node_instances", "namespace_id"},
		{"capability_versions", "outputs"},
		{"capability_versions", "params_schema"},
		{"capability_versions", "content_hash"},
		{"tasks", "status"},
		{"tasks", "attempts"},
		{"tasks", "cap_hash"},
		{"edges", "from_port"},
		{"edges", "literals"},
	}
	for _, tc := range cases {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "retired.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatalf("create valid v4 db: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			raw := openRawFixture(t, path)
			mustExecSQL(t, raw, `ALTER TABLE "`+tc.table+`" ADD COLUMN "`+tc.column+`" TEXT`)
			if err := raw.Close(); err != nil {
				t.Fatalf("close raw: %v", err)
			}
			before := snapshotDatabase(t, path)

			_, err = store.Open(path)
			if err == nil {
				t.Fatalf("marker=4 但残留 %s.%s 的库应该被拒绝，实际 Open 成功", tc.table, tc.column)
			}
			if !errors.Is(err, store.ErrSchemaInvalid) {
				t.Fatalf("应返回 ErrSchemaInvalid，实际: %v", err)
			}
			if want := "残留 v3 列 " + tc.table + "." + tc.column; !strings.Contains(err.Error(), want) {
				t.Fatalf("错误信息应点名 %q: %v", want, err)
			}
			if after := snapshotDatabase(t, path); after != before {
				t.Fatalf("拒绝改动了数据目录:\n before %s\n after  %s", before, after)
			}
			if !rawColumnNames(t, path, tc.table)[tc.column] {
				t.Fatal("拒绝时删了列：程序不许改用户的数据结构")
			}
		})
	}
}

func TestC3bResultFactsSchemaRejectsMissingColumnsZeroWrites(t *testing.T) {
	for _, tc := range []struct{ table, column string }{
		{"tasks", "result_json"}, {"tasks", "result_accepted_at"}, {"task_attempts", "result_json"}, {"task_attempts", "result_accepted_at"},
	} {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing-result-facts.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			raw := openRawFixture(t, path)
			// tasks 的未解析失败形状 CHECK 引用了 result_json，SQLite 不允许 DROP 被 CHECK
			// 引用的列；RENAME 制造同样的「缺列」事实，严格 Open 的拒绝对不变。
			mustExecSQL(t, raw, `ALTER TABLE "`+tc.table+`" RENAME COLUMN "`+tc.column+`" TO c1r_missing`)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotDatabase(t, path)
			bad, err := store.Open(path)
			if bad != nil {
				_ = bad.Close()
			}
			if !errors.Is(err, store.ErrSchemaInvalid) || !strings.Contains(err.Error(), tc.column) {
				t.Fatalf("missing complete facts column must fail closed: %v", err)
			}
			if after := snapshotDatabase(t, path); after != before {
				t.Fatal("format refusal modified existing user DB")
			}
		})
	}
}
