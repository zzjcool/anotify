package store

// 部署候选（DeployCandidateNodes）的白盒验收。
//
// 判据只有两条：ns ∈ 申报 ∩ 授权、不在 draining。在线由调用方（ws.Hub）按活连接再筛。
// **故意**比派发判据 EligibleNodes 宽：后者还要求「这台机器已经 applied 过该 digest」，
// 拿它当部署候选就会让**首次部署**永远没有目标（真实故障：控制台「没有候选节点」、
// deploy 回 200 但 deployed: []，能力永远停在 NotDeployed）。

import (
	"context"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

func TestDeployCandidateNodesIgnoresDeploymentFact(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// mk 造一台节点：declared 是本机申报（hello），authorized 是中枢侧的授权（node_configs）。
	mk := func(name, status string, declared, authorized []string) *Node {
		t.Helper()
		n := &Node{Name: name, Status: status, DeclaredNamespaces: declared}
		if _, err := db.CreateNode(ctx, n); err != nil {
			t.Fatalf("create node %q: %v", name, err)
		}
		putV4NodeCfg(t, db, n.ID, authorized, nil, nil)
		return n
	}

	// ① 命中：申报与授权都含本 ns、在线、**从没部署过**、配置也没 applied。
	fresh := mk("cand-fresh", NodeStatusOnline, []string{"*"}, []string{"*"})
	// ② 未授权：申报与授权都不含本 ns。
	other := mk("cand-other-ns", NodeStatusOnline, []string{"other-ns"}, []string{"other-ns"})
	// ③ 排空中：授权没问题，但不再接新活。
	draining := mk("cand-draining", NodeStatusDraining, []string{"*"}, []string{"*"})
	// ④ 离线但授权：仍然是候选（离线只影响「省略目标」时的默认集合，不影响能不能接意图）。
	offline := mk("cand-offline", NodeStatusOffline, []string{"*"}, []string{"*"})

	got, err := db.DeployCandidateNodes(ctx, nsID)
	if err != nil {
		t.Fatalf("deploy candidates: %v", err)
	}
	names := make(map[string]bool, len(got))
	for _, n := range got {
		names[n.Name] = true
	}
	if !names[fresh.Name] {
		t.Fatalf("从没部署过的授权节点必须是部署候选（首次部署的唯一机会点），实际 %v", names)
	}
	if !names[offline.Name] {
		t.Fatalf("离线但授权的节点仍是候选（能不能现在推由调用方按在线筛），实际 %v", names)
	}
	if names[other.Name] {
		t.Fatalf("未授权的节点不得成为候选，实际 %v", names)
	}
	if names[draining.Name] {
		t.Fatalf("排空中的节点不接新部署，实际 %v", names)
	}
}

func TestDeployCandidateNodesRequiresNamespace(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.DeployCandidateNodes(context.Background(), ""); err == nil {
		t.Fatal("空命名空间必须报错（不能把集群级查询当成「所有 ns 的候选」）")
	}
	if _, err := db.DeployCandidateNodes(context.Background(), AllNamespaces); err == nil {
		t.Fatal("AllNamespaces 不是可部署目标（部署必须落到一个具体 ns）")
	}
}

// TestMergeDeclaredCapability 钉住「回执带回的探测结果」怎么落账：
// 同名同版本替换、其它项保留、不 bump 部署事实（那是另一笔事务）。
func TestMergeDeclaredCapability(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	node := v4Node(t, db, "merge-cap-node", 0)

	decl := func(caps []DeclaredCapability) map[string]DeclaredCapability {
		t.Helper()
		got, err := db.GetNode(ctx, node.ID)
		if err != nil {
			t.Fatalf("get node: %v", err)
		}
		out := map[string]DeclaredCapability{}
		for _, c := range got.DeclaredCapabilities {
			out[c.Name] = c
		}
		return out
	}

	// ① 第一次合并：新增一项（节点此前没有任何申报）。
	if err := db.MergeDeclaredCapability(ctx, node.ID, DeclaredCapability{
		Name: "probe", Version: 2, Digest: "sha256:aa",
		Commands: []protocol.HelloCommand{{Name: "bash", Found: true, Path: "/bin/bash"}},
	}); err != nil {
		t.Fatalf("merge new: %v", err)
	}
	first := decl(nil)
	if c, ok := first["probe"]; !ok || c.Version != 2 || len(c.Commands) != 1 || !c.Commands[0].Found {
		t.Fatalf("新增项没落账：%+v", first["probe"])
	}

	// ② 同名同版本再合并：替换（探测结果变 false 也要如实换掉）。
	if err := db.MergeDeclaredCapability(ctx, node.ID, DeclaredCapability{
		Name: "probe", Version: 2, Digest: "sha256:aa",
		Commands: []protocol.HelloCommand{{Name: "bash", Found: false}},
	}); err != nil {
		t.Fatalf("merge replace: %v", err)
	}
	second := decl(nil)
	if len(second) != 1 {
		t.Fatalf("同名同版本必须替换而不是新增，实际 %+v", second)
	}
	if second["probe"].Commands[0].Found {
		t.Fatalf("探测结果必须被如实替换：%+v", second["probe"])
	}

	// ③ 另一个版本 / 另一项：并集式保留。
	if err := db.MergeDeclaredCapability(ctx, node.ID, DeclaredCapability{Name: "probe", Version: 3, Digest: "sha256:bb"}); err != nil {
		t.Fatalf("merge other version: %v", err)
	}
	if err := db.MergeDeclaredCapability(ctx, node.ID, DeclaredCapability{Name: "other", Version: 1, Digest: "sha256:cc"}); err != nil {
		t.Fatalf("merge other name: %v", err)
	}
	before, err := db.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(before.DeclaredCapabilities) != 3 {
		t.Fatalf("三项都应保留（probe@2/3、other@1），实际 %+v", before.DeclaredCapabilities)
	}

	// ④ 节点不存在：是 ErrNotFound，不静默成功。
	if err := db.MergeDeclaredCapability(ctx, "nd_missing", DeclaredCapability{Name: "x", Version: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知节点应 ErrNotFound，实际 %v", err)
	}
	// ⑤ 身份非法：name/version 必填。
	if err := db.MergeDeclaredCapability(ctx, node.ID, DeclaredCapability{Version: 1}); err == nil {
		t.Fatal("空 name 必须报错")
	}
}
