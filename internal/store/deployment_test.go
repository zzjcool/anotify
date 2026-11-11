package store

// deployment_test.go 覆盖 v4 部署（deployment.md）：
//
//	部署行 = 「某节点应当拥有某份内容」+ 节点回报的状态；内容由 capability_versions
//	派生（没有 artifacts 镜像表）。推给节点的是整份产物正文 + digest，节点从收到的
//	字节重算 digest 必须一致，否则永远收敛不了。
//
// v3 的 handler 能力（无 run 也可部署）随中枢执行器退役：v4 只有脚本，
// 没有 run 的行不可部署（ErrArtifactNotFound）。

import (
	"context"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// TestCurrentArtifactIsCapabilityContent 产物正文 = 能力正文的规范字节（digest 同源）。
func TestCurrentArtifactIsCapabilityContent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1, "fix")

	art, err := db.CurrentArtifact(ctx, nsID, c.ID)
	if err != nil {
		t.Fatalf("current artifact: %v", err)
	}
	if art.CapabilityID != c.ID || art.NamespaceID != nsID {
		t.Fatalf("产物身份 = %+v, want ns=%s cap=%s", art, nsID, c.ID)
	}
	parsed, digest, err := artifact.Decode(art.Spec)
	if err != nil {
		t.Fatalf("decode artifact bytes: %v", err)
	}
	if digest != c.Digest {
		t.Fatalf("从字节重算 digest = %q, want 行上的 %q", digest, c.Digest)
	}
	if parsed.Metadata.Name != "review" || parsed.Metadata.Version != 1 {
		t.Fatalf("产物元数据 = %+v", parsed.Metadata)
	}
	if len(parsed.Spec.Next) != 1 || parsed.Spec.Next[0] != "fix" {
		t.Fatalf("产物 next = %v", parsed.Spec.Next)
	}
}

// TestArtifactAbsentCases 没有能力行 / 没有 run 都归到 ErrArtifactNotFound。
func TestArtifactAbsentCases(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if _, err := db.CurrentArtifact(ctx, nsID, "cap_missing"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("不存在的能力应报 ErrArtifactNotFound，得到 %v", err)
	}
	badID := NewCapabilityID("deadbeef")
	mustExec(t, db, `INSERT INTO capability_versions
		(id, namespace_id, name, version, spec, digest, fold_key, resource_version, created_at)
		VALUES (?, ?, 'no-run', 1, '{"params":{}}', 'sha256:deadbeef', 'sha256:deadbeef', 1, ?)`,
		badID, nsID, Now())
	if _, err := db.CurrentArtifact(ctx, nsID, badID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("没有 run 的行应报 ErrArtifactNotFound，得到 %v", err)
	}
}

// TestArtifactNamespaceIsolation 产物只在所属命名空间可读。
func TestArtifactNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB := newTestExtraNS(t, db, "dep-other")
	c := v4Cap(t, db, nsA, "review", 1)

	if _, err := db.CurrentArtifact(ctx, nsA, c.ID); err != nil {
		t.Fatalf("A 读自己的产物: %v", err)
	}
	if _, err := db.CurrentArtifact(ctx, nsB, c.ID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("B 读 A 的产物应报 ErrArtifactNotFound，得到 %v", err)
	}
	if _, err := db.GetArtifactByDigest(ctx, nsB, c.Digest); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("B 按 digest 读 A 的产物应报 ErrArtifactNotFound，得到 %v", err)
	}
}

// TestCreateDeploymentIdempotent 同 (node, capability, digest) 重复下发不重复建行。
func TestCreateDeploymentIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "dep-node", 0)

	first, err := db.CreateDeployments(ctx, nsID, c.ID, c.Digest, []string{node.ID})
	if err != nil {
		t.Fatalf("create deployments: %v", err)
	}
	if len(first) != 1 || first[0].State != DeploymentPending {
		t.Fatalf("首批部署 = %+v, want 1 行 pending", first)
	}
	again, err := db.CreateDeployments(ctx, nsID, c.ID, c.Digest, []string{node.ID})
	if err != nil {
		t.Fatalf("create deployments again: %v", err)
	}
	if len(again) != 1 || again[0].ID != first[0].ID {
		t.Fatalf("重复下发建了新行: %+v vs %+v", again, first)
	}
	list, err := db.ListDeployments(ctx, nsID, DeploymentFilter{NodeID: node.ID})
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("部署行数 = %d, want 1", len(list))
	}
	empty, err := db.CreateDeployments(ctx, nsID, c.ID, c.Digest, nil)
	if err != nil {
		t.Fatalf("empty create deployments: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空节点列表应返回空切片，得到 %#v", empty)
	}
}

// TestCreateDeploymentRejectsMismatch digest 与能力行不符 / 能力不存在都必须拒。
func TestCreateDeploymentRejectsMismatch(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "dep-mismatch", 0)

	if _, err := db.CreateDeployments(ctx, nsID, c.ID, "sha256:not-the-digest", []string{node.ID}); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("错 digest 应报 ErrArtifactNotFound，得到 %v", err)
	}
	if _, err := db.CreateDeployments(ctx, nsID, "cap_missing", "", []string{node.ID}); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("不存在的能力应报 ErrArtifactNotFound，得到 %v", err)
	}
	list, err := db.ListDeployments(ctx, nsID, DeploymentFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("被拒的下发留下了 %d 行", len(list))
	}
}

// TestDeleteDeploymentRemovesRow 删部署只删行，不动能力/产物。
func TestDeleteDeploymentRemovesRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "dep-delete", 0)
	rows, err := db.CreateDeployments(ctx, nsID, c.ID, c.Digest, []string{node.ID})
	if err != nil {
		t.Fatalf("create deployments: %v", err)
	}
	got, err := db.DeleteDeployment(ctx, nsID, rows[0].ID)
	if err != nil {
		t.Fatalf("delete deployment: %v", err)
	}
	if got == nil || got.ID != rows[0].ID {
		t.Fatalf("删除返回行 = %+v", got)
	}
	if _, err := db.GetDeployment(ctx, nsID, rows[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删后仍能读到部署: %v", err)
	}
	if _, err := db.CurrentArtifact(ctx, nsID, c.ID); err != nil {
		t.Fatalf("删部署不该动能力/产物: %v", err)
	}
}

// TestListDeploymentsFilters 过滤条件（node / capability）都要生效。
func TestListDeploymentsFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	a := v4Cap(t, db, nsID, "first", 1)
	b := v4Cap(t, db, nsID, "second", 1)
	n1 := v4Node(t, db, "dep-n1", 0)
	n2 := v4Node(t, db, "dep-n2", 0)

	if _, err := db.CreateDeployments(ctx, nsID, a.ID, a.Digest, []string{n1.ID, n2.ID}); err != nil {
		t.Fatalf("deploy a: %v", err)
	}
	if _, err := db.CreateDeployments(ctx, nsID, b.ID, b.Digest, []string{n1.ID}); err != nil {
		t.Fatalf("deploy b: %v", err)
	}
	if rows, err := db.ListDeployments(ctx, nsID, DeploymentFilter{NodeID: n2.ID}); err != nil || len(rows) != 1 {
		t.Fatalf("按 node 过滤 = %d 行 (err %v), want 1", len(rows), err)
	}
	if rows, err := db.ListDeployments(ctx, nsID, DeploymentFilter{CapabilityID: b.ID}); err != nil || len(rows) != 1 {
		t.Fatalf("按 capability 过滤 = %d 行 (err %v), want 1", len(rows), err)
	}
	if rows, err := db.ListDeployments(ctx, nsID, DeploymentFilter{NodeID: n1.ID}); err != nil || len(rows) != 2 {
		t.Fatalf("n1 上应有 2 条部署，得到 %d (err %v)", len(rows), err)
	}
}

// TestDeploymentsForNodeDerivesFromCapability 推给节点的整份产物来自能力行（含 next）。
func TestDeploymentsForNodeDerivesFromCapability(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1, "fix")
	node := v4Node(t, db, "dep-delivery", 0)
	if _, err := db.CreateDeployments(ctx, nsID, c.ID, c.Digest, []string{node.ID}); err != nil {
		t.Fatalf("create deployments: %v", err)
	}

	deliveries, err := db.DeploymentsForNode(ctx, nsID, node.ID)
	if err != nil {
		t.Fatalf("deliveries for node: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("交付行数 = %d, want 1", len(deliveries))
	}
	d := deliveries[0]
	if d.CapabilityID != c.ID || d.Digest != c.Digest || d.Capability != "review" || d.Version != 1 {
		t.Fatalf("交付身份 = %+v", d)
	}
	parsed, digest, err := artifact.Decode(d.Spec)
	if err != nil {
		t.Fatalf("decode delivered spec: %v", err)
	}
	if digest != c.Digest {
		t.Fatalf("交付字节重算 digest = %q, want %q", digest, c.Digest)
	}
	if len(parsed.Spec.Next) != 1 || parsed.Spec.Next[0] != "fix" {
		t.Fatalf("交付 next = %v（路由必须随产物一起下发）", parsed.Spec.Next)
	}
}

// TestMarkDeployedDigestUsesCapabilityDigest 节点回报状态；digest 仍取能力行。
func TestMarkDeployedDigestUsesCapabilityDigest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "dep-mark", 0)
	rows, err := db.CreateDeployments(ctx, nsID, c.ID, c.Digest, []string{node.ID})
	if err != nil {
		t.Fatalf("create deployments: %v", err)
	}
	if err := db.MarkDeployResult(ctx, nsID, rows[0].ID, DeploymentApplied, ""); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	got, err := db.GetDeployment(ctx, nsID, rows[0].ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.State != DeploymentApplied || got.Digest != c.Digest {
		t.Fatalf("回报后 = %+v, want applied + digest %s", got, c.Digest)
	}
	if err := db.MarkDeployResult(ctx, nsID, rows[0].ID, DeploymentFailed, "need_root"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, err = db.GetDeployment(ctx, nsID, rows[0].ID)
	if err != nil {
		t.Fatalf("get deployment after fail: %v", err)
	}
	if got.State != DeploymentFailed || got.Reason != "need_root" {
		t.Fatalf("失败回报 = %+v", got)
	}
}

// TestDeploymentDeliveryCarriesOpaqueBytes 交付字节是节点可直接落盘的规范正文：
// Parse → MarshalYAML → Parse 的 digest 必须稳定。
func TestDeploymentDeliveryCarriesOpaqueBytes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "dep-bytes", 0)
	if _, err := db.CreateDeployments(ctx, nsID, c.ID, c.Digest, []string{node.ID}); err != nil {
		t.Fatalf("create deployments: %v", err)
	}
	deliveries, err := db.DeploymentsForNode(ctx, nsID, node.ID)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries: %v %d", err, len(deliveries))
	}
	parsed, err := artifact.Parse(deliveries[0].Spec)
	if err != nil {
		t.Fatalf("parse delivered spec: %v", err)
	}
	yamlBytes, err := artifact.MarshalYAML(parsed)
	if err != nil {
		t.Fatalf("marshal delivered spec: %v", err)
	}
	reparsed, err := artifact.Parse(yamlBytes)
	if err != nil {
		t.Fatalf("re-parse delivered spec: %v", err)
	}
	first, err := artifact.Digest(parsed)
	if err != nil {
		t.Fatalf("digest parsed: %v", err)
	}
	second, err := artifact.Digest(reparsed)
	if err != nil {
		t.Fatalf("digest reparsed: %v", err)
	}
	if first != second || first != c.Digest {
		t.Fatalf("落盘往返后 digest 漂移: %q / %q, want %q", first, second, c.Digest)
	}
}

// TestGetDeploymentByNodeCapability 按 (node, capability) 反查唯一部署行。
func TestGetDeploymentByNodeCapability(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "dep-lookup", 0)
	rows, err := db.CreateDeployments(ctx, nsID, c.ID, c.Digest, []string{node.ID})
	if err != nil {
		t.Fatalf("create deployments: %v", err)
	}
	got, err := db.GetDeploymentByNodeCapability(ctx, nsID, node.ID, c.ID)
	if err != nil {
		t.Fatalf("get by node+capability: %v", err)
	}
	if got.ID != rows[0].ID {
		t.Fatalf("反查行 = %s, want %s", got.ID, rows[0].ID)
	}
	if _, err := db.GetDeploymentByNodeCapability(ctx, nsID, node.ID, "cap_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("反查不存在的能力应 ErrNotFound，得到 %v", err)
	}
}

// TestListDeploymentsByCapabilityName 按**能力名**取部署事实（#362）。
//
// 部署行钉的是 capability_versions 的**某一行版本**，而 status 要回答的是「这个能力名
// 在节点上处于什么状态」。按 id 查只能看见当前那一版，升级后的旧部署行彻底不可见 —
// 那正是「升级后 UI 里旧版本部署消失、stale 永远派生不出来」的根因。
//
// 三个判据：
//   - 同名 v1/v2 各自部署到不同节点 → 两台各一行，且都带得出（version/digest）；
//   - 不含别的能力的部署行；
//   - 重复部署不产生第二行（同一 (node, capability_id, digest) 幂等）。
func TestListDeploymentsByCapabilityName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	v1 := v4Cap(t, db, nsID, "deploy-by-name", 1)
	v2 := v4Cap(t, db, nsID, "deploy-by-name", 2)
	other := v4Cap(t, db, nsID, "unrelated", 1)
	if v1.ID == v2.ID || v1.Digest == v2.Digest {
		t.Fatalf("同名两版必须是两行两个 digest：v1=%s/%s v2=%s/%s", v1.ID, v1.Digest, v2.ID, v2.Digest)
	}
	nodeA := v4Node(t, db, "dep-name-a", 0)
	nodeB := v4Node(t, db, "dep-name-b", 0)

	if _, err := db.CreateDeployments(ctx, nsID, v1.ID, v1.Digest, []string{nodeA.ID}); err != nil {
		t.Fatalf("deploy v1 to node-a: %v", err)
	}
	if _, err := db.CreateDeployments(ctx, nsID, v2.ID, v2.Digest, []string{nodeB.ID}); err != nil {
		t.Fatalf("deploy v2 to node-b: %v", err)
	}
	if _, err := db.CreateDeployments(ctx, nsID, other.ID, other.Digest, []string{nodeA.ID}); err != nil {
		t.Fatalf("deploy unrelated: %v", err)
	}

	rows, err := db.ListDeploymentsByCapabilityName(ctx, nsID, "deploy-by-name")
	if err != nil {
		t.Fatalf("list by capability name: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("同名能力的部署行 = %d, want 2（两台节点各一行）: %+v", len(rows), rows)
	}
	byNode := map[string]*Deployment{}
	for _, dp := range rows {
		if dp.Capability != "deploy-by-name" {
			t.Fatalf("带出了别的能力：%+v", dp)
		}
		if _, dup := byNode[dp.NodeID]; dup {
			t.Fatalf("节点 %s 出现两行：%+v", dp.NodeID, rows)
		}
		byNode[dp.NodeID] = dp
	}
	if got := byNode[nodeA.ID]; got == nil || got.Version != 1 || got.Digest != v1.Digest || got.CapabilityID != v1.ID {
		t.Fatalf("node-a 的行 = %+v, want 指向 v1（%s/%s）", got, v1.ID, v1.Digest)
	}
	if got := byNode[nodeB.ID]; got == nil || got.Version != 2 || got.Digest != v2.Digest || got.CapabilityID != v2.ID {
		t.Fatalf("node-b 的行 = %+v, want 指向 v2（%s/%s）", got, v2.ID, v2.Digest)
	}
	if byNode[nodeA.ID].State != DeploymentPending {
		t.Fatalf("新部署行应是 pending，实际 %q", byNode[nodeA.ID].State)
	}

	// 重复部署同一版本：不产生第二行（幂等），digest 仍是这一版的。
	firstID := byNode[nodeA.ID].ID
	if _, err := db.CreateDeployments(ctx, nsID, v1.ID, v1.Digest, []string{nodeA.ID}); err != nil {
		t.Fatalf("redeploy v1 to node-a: %v", err)
	}
	again, err := db.ListDeploymentsByCapabilityName(ctx, nsID, "deploy-by-name")
	if err != nil {
		t.Fatalf("list after redeploy: %v", err)
	}
	if len(again) != 2 {
		t.Fatalf("重复部署后行数 = %d, want 2（不新增行）: %+v", len(again), again)
	}
	for _, dp := range again {
		if dp.NodeID == nodeA.ID && (dp.ID != firstID || dp.Digest != v1.Digest) {
			t.Fatalf("重复部署改动了 node-a 的行：%+v（want id=%s digest=%s）", dp, firstID, v1.Digest)
		}
	}

	// 同一节点上的跨版本事实**不隐藏**：CreateDeployments 的 supersede 只清同一
	// capability_id 的行，所以把 v2 也部署到 node-a 会留下两行。折叠成「每节点一行」
	// 是调用方（server 的 status 派生）的事——只有它知道正确性该按哪个 digest 判。
	if _, err := db.CreateDeployments(ctx, nsID, v2.ID, v2.Digest, []string{nodeA.ID}); err != nil {
		t.Fatalf("deploy v2 to node-a: %v", err)
	}
	crossVersion, err := db.ListDeploymentsByCapabilityName(ctx, nsID, "deploy-by-name")
	if err != nil {
		t.Fatalf("list after cross-version deploy: %v", err)
	}
	if len(crossVersion) != 3 {
		t.Fatalf("跨版本事实 = %d 行, want 3（node-a 两行 + node-b 一行，store 不替调用方折叠）: %+v",
			len(crossVersion), crossVersion)
	}
}

// TestListDeploymentsByCapabilityNameEdgeCases 边界：空名 → 空列表；跨命名空间必须报错。
func TestListDeploymentsByCapabilityNameEdgeCases(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if rows, err := db.ListDeploymentsByCapabilityName(ctx, nsID, "  "); err != nil || len(rows) != 0 {
		t.Fatalf("空名 = %d 行 (err %v), want 0 行且无错", len(rows), err)
	}
	if rows, err := db.ListDeploymentsByCapabilityName(ctx, nsID, "nothing-here"); err != nil || len(rows) != 0 {
		t.Fatalf("不存在的能力名 = %d 行 (err %v), want 0 行且无错", len(rows), err)
	}
	if _, err := db.ListDeploymentsByCapabilityName(ctx, AllNamespaces, "x"); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("跨命名空间应报 ErrNamespaceRequired，得到 %v", err)
	}
}
