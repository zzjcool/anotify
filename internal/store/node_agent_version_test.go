package store

// node_agent_version_test.go 覆盖节点上跑的 anode 版本与升级状态（anode.md §升级）：
//
//	hello 自报版本 → UpdateNodeAgentVersion；升级三段事实 sent / failed / ok 各一次
//	UPDATE；ok 才改版本列（failed 不许让控制台谎报成功）。
//
// v3 的 migration 3（从 v2 老库加三列）随 v4 baseline 重建退役：三列现在就在
// baseline 里，用例改成断言 **baseline 形状**（列存在 + 默认零值）。
// 节点在 v4 是**全局对象**（没有 namespace_id）：写入口按节点 id 收口，
// 未知 id 必须 ErrNotFound 且不误改其它节点。

import (
	"context"
	"errors"
	"testing"
)

// agentColumns 是节点升级事实的三列（少一列就意味着某个消费方拿到空值）。
var agentColumns = []string{"agent_version", "agent_commit", "agent_upgrade"}

// TestAgentColumnsInBaseline v4 baseline 自带三列（注册即存在，默认零值）。
func TestAgentColumnsInBaseline(t *testing.T) {
	db := newTestDB(t)
	cols := columnNames(t, db, "nodes")
	for _, col := range agentColumns {
		if !cols[col] {
			t.Fatalf("v4 baseline 缺列 %q", col)
		}
	}
	n := newTestNode(t, db, "baseline-agent")
	got, err := db.GetNode(context.Background(), n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != "" || got.AgentCommit != "" {
		t.Fatalf("新节点 agent 版本默认值 = (%q, %q), want 空串", got.AgentVersion, got.AgentCommit)
	}
	if got.AgentUpgrade != (AgentUpgrade{}) {
		t.Fatalf("新节点 agent_upgrade 默认值 = %+v, want 零值（从未尝试）", got.AgentUpgrade)
	}
}

// TestNodeAgentVersionRoundTrip hello 自报的版本存什么读什么（GetNode + ListNodes）。
func TestNodeAgentVersionRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n := newTestNode(t, db, "roundtrip")

	const version = "1.5.0+3fd5a59"
	if err := db.UpdateNodeAgentVersion(ctx, n.ID, version); err != nil {
		t.Fatalf("update agent version: %v", err)
	}
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != version {
		t.Fatalf("GetNode.AgentVersion = %q, want %q", got.AgentVersion, version)
	}
	if got.AgentCommit != "" {
		t.Fatalf("hello 只该写版本列，agent_commit 被改成 %q", got.AgentCommit)
	}
	nodes, err := db.ListNodes(ctx)
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
	if err := db.UpdateNodeAgentVersion(ctx, n.ID, ""); err != nil {
		t.Fatalf("update agent version to empty: %v", err)
	}
	got, err = db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != "" {
		t.Fatalf("AgentVersion = %q, want 空串（未知）", got.AgentVersion)
	}
}

// TestNodeAgentUpgradeStatusRoundTrip 升级状态机在库里的三种落点（sent / failed / ok）。
func TestNodeAgentUpgradeStatusRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n := newTestNode(t, db, "upgrade-status")

	if err := db.MarkAgentUpgradeSent(ctx, n.ID, "1.5.0+aaa", 111); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	want := AgentUpgrade{Status: AgentUpgradeSent, TargetVersion: "1.5.0+aaa", At: 111}
	if got.AgentUpgrade != want {
		t.Fatalf("sent 状态 = %+v, want %+v", got.AgentUpgrade, want)
	}
	if got.AgentVersion != "" {
		t.Fatalf("sent 之后 AgentVersion = %q, want 空串（节点还没换）", got.AgentVersion)
	}

	if err := db.FailAgentUpgrade(ctx, n.ID, "need_root", 222); err != nil {
		t.Fatalf("fail upgrade: %v", err)
	}
	got, err = db.GetNode(ctx, n.ID)
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

	if err := db.RecordAgentUpgrade(ctx, n.ID, "1.5.0+bbb", "bbb", 333); err != nil {
		t.Fatalf("record upgrade: %v", err)
	}
	got, err = db.GetNode(ctx, n.ID)
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

// TestNodeAgentWritesUnknownNodeRefused 四个写入口对未知节点 id 必须 ErrNotFound，
// 且不能误改任何真实节点（v4 节点是全局对象，收口靠 id 而不是 namespace）。
func TestNodeAgentWritesUnknownNodeRefused(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n := newTestNode(t, db, "scoped")

	calls := map[string]func() error{
		"update version": func() error {
			return db.UpdateNodeAgentVersion(ctx, "nod_missing", "1.5.0+xxx")
		},
		"mark sent": func() error {
			return db.MarkAgentUpgradeSent(ctx, "nod_missing", "1.5.0+xxx", 1)
		},
		"fail": func() error {
			return db.FailAgentUpgrade(ctx, "nod_missing", "need_root", 1)
		},
		"record ok": func() error {
			return db.RecordAgentUpgrade(ctx, "nod_missing", "1.5.0+xxx", "xxx", 1)
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound（未知节点必须响）", name, err)
		}
	}
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != "" || got.AgentCommit != "" || got.AgentUpgrade != (AgentUpgrade{}) {
		t.Fatalf("未知节点的写泄漏到了真实节点: %+v", got)
	}
}

// newTestNode 建一台节点（名字唯一）并返回那一行。
func newTestNode(t *testing.T, db *DB, name string) *Node {
	t.Helper()
	n := &Node{Name: name + "-" + NewUserID(), Tags: []string{}}
	if _, err := db.CreateNode(context.Background(), n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	return n
}
