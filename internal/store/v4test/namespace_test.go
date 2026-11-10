package v4test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 命名空间列表计数与删除语义的外部验收（api-conventions §7.5）。

// TestNamespaceNodeCountDerived NodeCount = 在线 × (申报 ∩ 授权)，不是配置出来的一列。
func TestNamespaceNodeCountDerived(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsA := newNS(t, db, "team-a")
	nsB := newNS(t, db, "team-b")

	// 机器 1：申报 team-a，授权缺省 [*]（不额外限制）。
	n1 := newClusterNode(t, db, "node-1")
	nodeOnline(t, db, n1.ID)
	declareHello(t, db, n1.ID, []string{"team-a"}, nil, nil)

	// 机器 2：申报 [*]，授权 team-b（只能服务 team-b）。
	n2 := newClusterNode(t, db, "node-2")
	nodeOnline(t, db, n2.ID)
	declareHello(t, db, n2.ID, []string{"*"}, nil, nil)
	putNodeCfg(t, db, n2.ID, []string{"team-b"}, nil, nil)

	// 机器 3：离线，不计数。
	n3 := newClusterNode(t, db, "node-3")
	declareHello(t, db, n3.ID, []string{"*"}, nil, nil)

	namespaces, err := db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	counts := map[string]int{}
	for _, ns := range namespaces {
		counts[ns.Slug] = ns.NodeCount
	}
	if counts["team-a"] != 1 {
		t.Errorf("team-a NodeCount = %d, want 1（node-1 在线且被授权）", counts["team-a"])
	}
	if counts["team-b"] != 1 {
		t.Errorf("team-b NodeCount = %d, want 1（node-2 在线且授权 team-b）", counts["team-b"])
	}

	// 授权收紧（team-b 不再授权）→ 下一次读立即反映。
	putNodeCfg(t, db, n2.ID, []string{"team-a"}, nil, nil)
	namespaces, err = db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	counts = map[string]int{}
	for _, ns := range namespaces {
		counts[ns.Slug] = ns.NodeCount
	}
	if counts["team-b"] != 0 {
		t.Errorf("撤销授权后 team-b NodeCount = %d, want 0", counts["team-b"])
	}
	if counts["team-a"] != 2 {
		t.Errorf("team-a NodeCount = %d, want 2", counts["team-a"])
	}

	// 两个 ns 都在（隔离维度没有被推导打乱）。
	if _, err := db.GetNamespace(ctx, nsA); err != nil {
		t.Fatalf("get ns-a: %v", err)
	}
	if _, err := db.GetNamespace(ctx, nsB); err != nil {
		t.Fatalf("get ns-b: %v", err)
	}
}

// TestNamespaceDeleteSemantics 阻塞清单（能力/源/预案/任务/事件/部署）+ force 级联；节点不删。
func TestNamespaceDeleteSemantics(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsDel := newNS(t, db, "doomed")
	_ = newNS(t, db, "keep") // 不允许删最后一个命名空间
	node := newClusterNode(t, db, "node-1")

	createCap(t, db, testCap(nsDel, "build", 0, nil))

	err := db.DeleteNamespace(ctx, nsDel, false, 1)
	var blocked *store.NamespaceBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("非空命名空间应返回 *NamespaceBlockedError，实际 %v", err)
	}
	if len(blocked.Blocking) != 1 || blocked.Blocking[0] != "capabilities=1" {
		t.Fatalf("阻塞清单 = %v, want [capabilities=1]", blocked.Blocking)
	}

	// force：级联清理该 ns 的业务行；集群级节点保留。
	if err := db.DeleteNamespace(ctx, nsDel, true, 1); err != nil {
		t.Fatalf("force delete: %v", err)
	}
	if _, err := db.GetNamespace(ctx, nsDel); !errors.Is(err, store.ErrNamespaceNotFound) {
		t.Fatalf("删除后 GetNamespace 应 ErrNamespaceNotFound，实际 %v", err)
	}
	if _, err := db.GetNode(ctx, node.ID); err != nil {
		t.Fatalf("删 ns 不应删掉集群级节点：%v", err)
	}
	var caps int
	if err := db.QueryRow(`SELECT count(*) FROM capability_versions WHERE namespace_id = ?`, nsDel).Scan(&caps); err != nil {
		t.Fatalf("count capabilities: %v", err)
	}
	if caps != 0 {
		t.Fatalf("级联后仍有 %d 条能力", caps)
	}

	// 最后一个命名空间不允许删（否则「这套流程属于哪里」没有答案）。
	keepID, err := db.DefaultNamespaceID(ctx)
	if err != nil {
		t.Fatalf("default namespace: %v", err)
	}
	if err := db.DeleteNamespace(ctx, keepID, true, 1); !errors.Is(err, store.ErrLastNamespace) {
		t.Fatalf("删最后一个 ns 应 ErrLastNamespace，实际 %v", err)
	}
}
