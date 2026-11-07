package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// 本文件是 `-- +migration 3`（nodes.agent_version / agent_commit / agent_upgrade）
// 的升级与往返门禁（CONTRACT-agent-upgrade §3 / §5.1）。
//
// 两条性质各一组用例：
//  1. **老库（v2）升上来**：三列必须存在、默认空串/空对象，已有节点行不丢；
//  2. **往返一致性**：三种写路径（hello 版本 / sent / ok / failed）存什么读什么。

// agentColumns 是 migration 3 加的三列（少一列就意味着某个消费方拿到空值）。
var agentColumns = []string{"agent_version", "agent_commit", "agent_upgrade"}

// TestMigration3AgentColumnsFromV2 模拟「上一版（migration 2）建的库」升级到本版本：
// 三列出现、默认值正确、升级前就存在的节点行带默认值而不是 NULL/报错。
func TestMigration3AgentColumnsFromV2(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v2.db")

	// 只应用 baseline + migration 2 —— 等价于「本批代码发布之前」的库。
	v2, err := openWith(path, func(set *migrationSet) {
		*set = (*set)[:2]
	})
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
	}
	if got, err := v2.SchemaVersion(); err != nil || got != 2 {
		t.Fatalf("v2 schema version = %d (err %v), want 2", got, err)
	}
	// 升级前：三列必须不存在（否则这个用例证明不了「迁移加列」）。
	before := columnNames(t, v2, "nodes")
	for _, col := range agentColumns {
		if before[col] {
			t.Fatalf("v2 库已经有列 %q：migration 3 没被隔离在 baseline/2 之后", col)
		}
	}

	// 老库里先落一台节点：升级后它必须还在，且三列取默认值。
	//
	// 命名空间用裸 INSERT 造（newRawNamespace）：注册路径会播种内置能力，而播种
	// 写的是 v3 才有的 spec 列，v2 库上根本没有——那是"用新代码写老库"，不是要测的东西。
	nsID := newRawNamespace(t, v2)
	oldNode := &Node{NamespaceID: nsID, Name: "old-node", Tags: []string{}}
	if _, err := v2.CreateNode(ctx, oldNode); err != nil {
		t.Fatalf("create node in v2 db: %v", err)
	}
	if err := v2.Close(); err != nil {
		t.Fatalf("close v2 db: %v", err)
	}

	// 用本版本代码重开 → migration 3 应用。
	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (migrate) db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got, err := db.SchemaVersion(); err != nil || got != headVersion {
		t.Fatalf("升级后 schema version = %d (err %v), want %d", got, err, headVersion)
	}
	after := columnNames(t, db, "nodes")
	for _, col := range agentColumns {
		if !after[col] {
			t.Fatalf("升级后缺少列 %q", col)
		}
	}

	// 升级前建的节点：行还在，三列是默认值（'' / '' / 零值对象）。
	got, err := db.GetNode(ctx, nsID, oldNode.ID)
	if err != nil {
		t.Fatalf("get old node after migration: %v", err)
	}
	if got.AgentVersion != "" || got.AgentCommit != "" {
		t.Fatalf("老行 agent 版本默认值 = (%q, %q), want 空串", got.AgentVersion, got.AgentCommit)
	}
	if got.AgentUpgrade != (AgentUpgrade{}) {
		t.Fatalf("老行 agent_upgrade 默认值 = %+v, want 零值（从未尝试）", got.AgentUpgrade)
	}
}

// TestNodeAgentVersionRoundTrip hello 自报的版本存什么读什么（GetNode + ListNodes 两条路径）。
func TestNodeAgentVersionRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n := newTestNode(t, db, nsID, "roundtrip")

	const version = "1.5.0+3fd5a59"
	if err := db.UpdateNodeAgentVersion(ctx, nsID, n.ID, version); err != nil {
		t.Fatalf("update agent version: %v", err)
	}

	// 读路径 ①：单台。
	got, err := db.GetNode(ctx, nsID, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != version {
		t.Fatalf("GetNode.AgentVersion = %q, want %q", got.AgentVersion, version)
	}
	if got.AgentCommit != "" {
		t.Fatalf("hello 只该写版本列，agent_commit 被改成 %q", got.AgentCommit)
	}
	// 读路径 ②：列表（控制台入口）。
	nodes, err := db.ListNodes(ctx, nsID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	var listed *Node
	for _, x := range nodes {
		if x.ID == n.ID {
			listed = x
		}
	}
	if listed == nil || listed.AgentVersion != version {
		t.Fatalf("ListNodes AgentVersion = %v, want %q", listed, version)
	}

	// 老 agent 再连上来（不带字段）→ 空串，不是错误。
	if err := db.UpdateNodeAgentVersion(ctx, nsID, n.ID, ""); err != nil {
		t.Fatalf("update agent version to empty: %v", err)
	}
	got, err = db.GetNode(ctx, nsID, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != "" {
		t.Fatalf("AgentVersion = %q, want 空串（未知）", got.AgentVersion)
	}
}

// TestNodeAgentUpgradeStatusRoundTrip 升级状态机在库里的三种落点（sent / ok / failed）:
// 每种都断言「存什么读什么」，以及 failed **不**改版本列（否则控制台会谎报成功）。
func TestNodeAgentUpgradeStatusRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n := newTestNode(t, db, nsID, "upgrade-status")

	// ① sent：端点推帧成功之后写。
	if err := db.MarkAgentUpgradeSent(ctx, nsID, n.ID, "1.5.0+aaa", 111); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	got, err := db.GetNode(ctx, nsID, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	want := AgentUpgrade{Status: AgentUpgradeSent, TargetVersion: "1.5.0+aaa", At: 111}
	if got.AgentUpgrade != want {
		t.Fatalf("sent 状态 = %+v, want %+v", got.AgentUpgrade, want)
	}
	// sent 只是「已下发」，版本列不能提前变成目标版本。
	if got.AgentVersion != "" {
		t.Fatalf("sent 之后 AgentVersion = %q, want 空串（节点还没换）", got.AgentVersion)
	}

	// ② failed：只写状态 + 错误码；targetVersion 沿用 sent 写入的值。
	if err := db.FailAgentUpgrade(ctx, nsID, n.ID, "need_root", 222); err != nil {
		t.Fatalf("fail upgrade: %v", err)
	}
	got, err = db.GetNode(ctx, nsID, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	want = AgentUpgrade{Status: AgentUpgradeFailed, TargetVersion: "1.5.0+aaa", Error: "need_root", At: 222}
	if got.AgentUpgrade != want {
		t.Fatalf("failed 状态 = %+v, want %+v", got.AgentUpgrade, want)
	}
	if got.AgentVersion != "" || got.AgentCommit != "" {
		t.Fatalf("失败的升级改了版本列: version=%q commit=%q（会让控制台谎报成功）",
			got.AgentVersion, got.AgentCommit)
	}

	// ③ ok：版本 + commit + 状态在**一次** UPDATE 里一起变。
	if err := db.RecordAgentUpgrade(ctx, nsID, n.ID, "1.5.0+bbb", "bbb", 333); err != nil {
		t.Fatalf("record upgrade: %v", err)
	}
	got, err = db.GetNode(ctx, nsID, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != "1.5.0+bbb" || got.AgentCommit != "bbb" {
		t.Fatalf("ok 之后版本列 = (%q, %q), want (1.5.0+bbb, bbb)", got.AgentVersion, got.AgentCommit)
	}
	want = AgentUpgrade{Status: AgentUpgradeOK, TargetVersion: "1.5.0+bbb", At: 333}
	if got.AgentUpgrade != want {
		t.Fatalf("ok 状态 = %+v, want %+v", got.AgentUpgrade, want)
	}
}

// TestNodeAgentWritesRespectNamespace 三个写方法都必须按 (namespace, id) 收口：
// 作用域写错时返回 ErrNotFound，且**不改**别的命名空间里的那台节点。
func TestNodeAgentWritesRespectNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	otherNS := newTestExtraNS(t, db, "agent-other")
	n := newTestNode(t, db, nsID, "scoped")

	calls := map[string]func() error{
		"update version": func() error {
			return db.UpdateNodeAgentVersion(ctx, otherNS, n.ID, "1.5.0+xxx")
		},
		"mark sent": func() error {
			return db.MarkAgentUpgradeSent(ctx, otherNS, n.ID, "1.5.0+xxx", 1)
		},
		"fail": func() error {
			return db.FailAgentUpgrade(ctx, otherNS, n.ID, "need_root", 1)
		},
		"record ok": func() error {
			return db.RecordAgentUpgrade(ctx, otherNS, n.ID, "1.5.0+xxx", "xxx", 1)
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound（作用域写错必须响）", name, err)
		}
	}
	got, err := db.GetNode(ctx, nsID, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != "" || got.AgentCommit != "" || got.AgentUpgrade != (AgentUpgrade{}) {
		t.Fatalf("跨命名空间写泄漏到了真正的节点: %+v", got)
	}
}

// newTestNode 建一台节点（名字唯一）并返回那一行。
func newTestNode(t *testing.T, db *DB, nsID, name string) *Node {
	t.Helper()
	n := &Node{NamespaceID: nsID, Name: name + "-" + NewUserID(), Tags: []string{}}
	if _, err := db.CreateNode(context.Background(), n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	return n
}
