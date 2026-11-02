package store

// 迁移 18（deployment_model）的验收测试：node_capabilities 重建，run_spec 列消失，
// 数据行与 (node_id, capability_id) 组合一个不少。
//
// # 为什么这条用例必须存在
//
// 迁移 18 是本 epic 里唯一**重建业务表**的迁移（SQLite 没有 DROP COLUMN）。
// 它跑在每一次 `Open` 上：写错的话，所有已在跑的节点库要么启动失败、要么静默丢掉
// 归属数据（那会让线上机器突然「没有能力」，派发全部失败且没有任何一条错误日志）——
// 这就是迁移 10 时代那个「hook 写错 = 所有老库起不来」风险的翻版。
//
// 三条验收：
//  1. 停在 v17 的老库（有 run_spec 列且有数据）升级后：列消失、行数与
//     (node_id, capability_id) 组合不变、namespace_id 不丢；
//  2. 新表结构正确（artifacts / deployments 建出来、node_capabilities 只剩 3 列）；
//  3. 幂等：连续 Open 两次不报错、schema_version 里 18 只记一次。

import (
	"context"
	"path/filepath"
	"testing"
)

// m18NodeCapRow 是 node_capabilities 的一行（迁移前后比对用的最小形状）。
type m18NodeCapRow struct {
	NodeID       string
	CapabilityID string
	NamespaceID  string
}

// m18Rows 读回整张 node_capabilities（按主键排序，结果稳定）。
func m18Rows(t *testing.T, db *DB) []m18NodeCapRow {
	t.Helper()
	rows, err := db.Query(`SELECT node_id, capability_id, namespace_id FROM node_capabilities
		ORDER BY node_id, capability_id`)
	if err != nil {
		t.Fatalf("read node_capabilities: %v", err)
	}
	defer rows.Close()
	out := []m18NodeCapRow{}
	for rows.Next() {
		var r m18NodeCapRow
		if err := rows.Scan(&r.NodeID, &r.CapabilityID, &r.NamespaceID); err != nil {
			t.Fatalf("scan node_capabilities: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read node_capabilities: %v", err)
	}
	return out
}

// m18Fixture 在 v17 库上造「一个命名空间 + 两个能力 + 两台节点 + 三条归属（带 run_spec）」。
//
// 直接走 SQL 而不是 Open 出来的 *DB 的业务方法：v17 的 Go 代码路径已经不认 run_spec，
// 而这条用例要的正是「老库里 real 存在的 run_spec 数据」这个前提。
func m18Fixture(t *testing.T, db *DB) (nsID string) {
	t.Helper()
	nsID = "ns_m18"
	mustExec(t, db, `INSERT INTO namespaces (id, slug, name, created_at) VALUES (?, 'legacy-m18', 'legacy', 1000)`, nsID)
	const insCap = `INSERT INTO capability_versions
		(id, namespace_id, name, display_name, version, params_schema, outputs, lifecycle, sticky, source, content_hash, created_at)
		VALUES (?, ?, ?, '', 1, '{}', ?, '{}', '', 'custom', ?, 1000)`
	mustExec(t, db, insCap, "cap_m18_a", nsID, "review", `["fix"]`, "hash_m18_a")
	mustExec(t, db, insCap, "cap_m18_b", nsID, "test", `[]`, "hash_m18_b")

	const insNode = `INSERT INTO nodes
		(id, namespace_id, name, tags, status, token_hash, max_instances, max_concurrency, created_at)
		VALUES (?, ?, ?, '[]', 'offline', ?, 20, 8, 1000)`
	mustExec(t, db, insNode, "nd_m18_1", nsID, "node-1", "tok_m18_1")
	mustExec(t, db, insNode, "nd_m18_2", nsID, "node-2", "tok_m18_2")
	// 三条归属，run_spec 各带**不同内容**（证明迁移不是把它们统一刷成 '{}' 而已）
	ins := `INSERT INTO node_capabilities (node_id, capability_id, namespace_id, run_spec) VALUES (?,?,?,?)`
	mustExec(t, db, ins, "nd_m18_1", "cap_m18_a", nsID, `{"runner":"bash","command":"./review.sh","timeoutSec":900}`)
	mustExec(t, db, ins, "nd_m18_1", "cap_m18_b", nsID, `{"runner":"bash","command":"./test.sh"}`)
	mustExec(t, db, ins, "nd_m18_2", "cap_m18_a", nsID, `{}`)
	return nsID
}

// TestMigration18DropsRunSpecPreservingRows 老库（v17，有 run_spec 且有数据）升级：
// 列消失、行数与 (node_id, capability_id) 组合不变。
func TestMigration18DropsRunSpecPreservingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v17.db")
	db17 := openAtVersion(t, path, 17)
	if v := mustMaxVersion(t, db17); v != 17 {
		t.Fatalf("夹具版本 = %d, want 17", v)
	}
	// 前提证明：夹具真的停在「run_spec 还在」的那一版
	if !columnNames(t, db17, "node_capabilities")["run_spec"] {
		t.Fatal("v17 夹具没有 run_spec 列，夹具没停在该停的位置")
	}
	if columnNames(t, db17, "artifacts") != nil && len(columnNames(t, db17, "artifacts")) != 0 {
		t.Fatal("v17 夹具里已经有 artifacts 表了，夹具不对")
	}
	nsID := m18Fixture(t, db17)
	before := m18Rows(t, db17)
	if len(before) != 3 {
		t.Fatalf("夹具归属行数 = %d, want 3", len(before))
	}
	if err := db17.Close(); err != nil {
		t.Fatalf("close v17 db: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("升级 v17 库: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })

	if v, want := mustMaxVersion(t, upgraded), testTipVersion(t); v != want {
		t.Fatalf("升级后版本 = %d, want %d", v, want)
	}

	// ① 列消失：node_capabilities 只剩 (node_id, capability_id, namespace_id)
	cols := columnNames(t, upgraded, "node_capabilities")
	if cols["run_spec"] {
		t.Fatalf("迁移 18 之后 run_spec 列还在: %v", cols)
	}
	if len(cols) != 3 {
		t.Fatalf("node_capabilities 列 = %v, want 恰好 3 列", cols)
	}
	for _, want := range []string{"node_id", "capability_id", "namespace_id"} {
		if !cols[want] {
			t.Fatalf("重建后丢了列 %q: %v", want, cols)
		}
	}

	// ② 数据一个不少：(node_id, capability_id) 组合与 namespace_id 逐行一致
	after := m18Rows(t, upgraded)
	if len(after) != len(before) {
		t.Fatalf("归属行数 %d → %d（迁移丢了数据）", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("第 %d 行变了: %+v → %+v", i, before[i], after[i])
		}
	}
	// ③ 新表建出来了（artifacts / deployments 是迁移 18 的另一半，不能只重建表）
	for _, table := range []string{"artifacts", "deployments"} {
		if !tableExists(t, upgraded, table) {
			t.Fatalf("迁移 18 没有建出 %s 表", table)
		}
		if !columnNames(t, upgraded, table)["digest"] {
			t.Fatalf("%s 没有 digest 列（内容寻址不变量 A6）", table)
		}
	}
	// 重建之后索引还在（NodesForCapability 的查询计划依赖它）
	if n := countIndexRows(t, upgraded, "node_capabilities", "idx_nodecap_cap"); n != 1 {
		t.Fatalf("重建后 idx_nodecap_cap 索引数 = %d, want 1", n)
	}
	// 派生索引仍然可读（业务路径真的能用，不只是「表还在」）
	caps, err := upgraded.ListNodeCapabilities(context.Background(), "nd_m18_1")
	if err != nil {
		t.Fatalf("升级后读节点能力: %v", err)
	}
	if len(caps) != 2 || caps[0].Name != "review" || caps[1].Name != "test" {
		t.Fatalf("升级后节点归属 = %v, want [review@1 test@1]", caps)
	}
	if caps[0].NamespaceID != nsID {
		t.Fatalf("升级后 namespace_id = %q, want %q", caps[0].NamespaceID, nsID)
	}
}

// TestMigration18IdempotentReopen 幂等：连续 Open 两次不报错、18 只记一次账、数据不变。
//
// 迁移 18 的判据是「run_spec 列还在不在」，所以「第二次 Open」正是它的重入路径。
func TestMigration18IdempotentReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v17-idem.db")
	db17 := openAtVersion(t, path, 17)
	m18Fixture(t, db17)
	before := m18Rows(t, db17)
	if err := db17.Close(); err != nil {
		t.Fatalf("close v17: %v", err)
	}

	for i := 1; i <= 2; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次 Open: %v", i, err)
		}
		if v, want := mustMaxVersion(t, db), testTipVersion(t); v != want {
			t.Fatalf("第 %d 次 Open 版本 = %d, want %d", i, v, want)
		}
		if columnNames(t, db, "node_capabilities")["run_spec"] {
			t.Fatalf("第 %d 次 Open 之后 run_spec 又出现了", i)
		}
		if got := m18Rows(t, db); len(got) != len(before) {
			t.Fatalf("第 %d 次 Open 后归属行数 = %d, want %d", i, len(got), len(before))
		}
		var dup int
		if err := db.QueryRow(`SELECT count(*) - count(DISTINCT version) FROM schema_version`).Scan(&dup); err != nil {
			t.Fatalf("count duplicates: %v", err)
		}
		if dup != 0 {
			t.Fatalf("第 %d 次 Open 后 schema_version 有重复记账", i)
		}
		if n := countRows(t, db, "schema_version", "version = 18"); n != 1 {
			t.Fatalf("第 %d 次 Open 后版本 18 记了 %d 次账", i, n)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}
