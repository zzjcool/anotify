package v4test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// TestDeployCreatePending 验人工部署写 pending 行（多节点一次成型）。
func TestDeployCreatePending(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "deploy-create")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	nodeA := newNode(t, db, "node-a", 1)
	nodeB := newNode(t, db, "node-b", 1)

	deps, err := db.CreateDeployments(ctx, ns, c.ID, "", []string{nodeA.ID, nodeB.ID})
	if err != nil {
		t.Fatalf("create deployments: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("应建 2 条部署，得到 %d", len(deps))
	}
	for _, dp := range deps {
		if dp.State != store.DeploymentPending {
			t.Fatalf("人工部署必须 pending，得到 %q", dp.State)
		}
		if dp.Digest != c.Digest {
			t.Fatalf("digest 应取能力行的：%q != %q", dp.Digest, c.Digest)
		}
	}
	// 重复部署是幂等 no-op（不该把状态打回灰，也不该多出行）。
	if err := db.MarkDeployResult(ctx, ns, deps[0].ID, store.DeploymentApplied, ""); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	if _, err := db.CreateDeployments(ctx, ns, c.ID, c.Digest, []string{nodeA.ID}); err != nil {
		t.Fatalf("re-deploy: %v", err)
	}
	still, err := db.GetDeployment(ctx, ns, deps[0].ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if still.State != store.DeploymentApplied {
		t.Fatalf("重复部署不该重置状态，得到 %q", still.State)
	}
	// 传别人的 digest 是自相矛盾的请求。
	if _, err := db.CreateDeployments(ctx, ns, c.ID, "sha256:deadbeef", []string{nodeA.ID}); !errors.Is(err, store.ErrArtifactNotFound) {
		t.Fatalf("异 digest 应 ErrArtifactNotFound，得到 %v", err)
	}
	// 空节点集合是 no-op。
	if deps, err := db.CreateDeployments(ctx, ns, c.ID, "", nil); err != nil || len(deps) != 0 {
		t.Fatalf("空节点集合应 no-op：%v %v", deps, err)
	}
	// 空列表必须是 []。
	empty, err := db.ListDeployments(ctx, ns, store.DeploymentFilter{NodeID: "nd_missing"})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("空列表必须是 []：%v %v", empty, err)
	}
}

// TestDeployReconcile 验对账：applied / stale / missing 三分，local 变体不建意图。
func TestDeployReconcile(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "deploy-reconcile")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	other := createCap(t, db, testCap(ns, "notify", 1, nil))
	node := newNode(t, db, "node-a", 1)

	if _, err := db.CreateDeployments(ctx, ns, c.ID, "", []string{node.ID}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	now := store.Now()

	// ① 节点上报一致 → applied
	stats, err := db.ReconcileNode(ctx, node.ID, []store.AppliedCapability{
		{CapabilityID: c.ID, Capability: c.Name, Version: c.Version, Digest: c.Digest},
	}, now)
	if err != nil {
		t.Fatalf("reconcile applied: %v", err)
	}
	if stats.Applied != 1 || stats.Stale != 0 || stats.Missing != 0 {
		t.Fatalf("一致应 applied：%+v", stats)
	}
	dep, err := db.GetDeploymentByNodeCapability(ctx, ns, node.ID, c.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if dep.State != store.DeploymentApplied || dep.Reason != "" {
		t.Fatalf("一致后应 applied 且无 reason：%+v", dep)
	}

	// ② 上报了但 digest 不一致 → pending + stale reason
	stats, err = db.ReconcileNode(ctx, node.ID, []store.AppliedCapability{
		{CapabilityID: c.ID, Capability: c.Name, Version: c.Version, Digest: "sha256:other"},
	}, now)
	if err != nil {
		t.Fatalf("reconcile stale: %v", err)
	}
	if stats.Stale != 1 || stats.Applied != 0 {
		t.Fatalf("不一致应 stale：%+v", stats)
	}
	dep, err = db.GetDeploymentByNodeCapability(ctx, ns, node.ID, c.ID)
	if err != nil {
		t.Fatalf("get stale deployment: %v", err)
	}
	if dep.State != store.DeploymentPending || !strings.Contains(dep.Reason, "stale") {
		t.Fatalf("stale 应 pending + reason=stale：%+v", dep)
	}

	// ③ 什么都没上报 → pending + missing reason，且不上报的能力被记成 Extra
	stats, err = db.ReconcileNode(ctx, node.ID, []store.AppliedCapability{
		{CapabilityID: other.ID, Capability: other.Name, Version: other.Version, Digest: other.Digest},
	}, now)
	if err != nil {
		t.Fatalf("reconcile missing: %v", err)
	}
	if stats.Missing != 1 || len(stats.Extra) != 1 || stats.Extra[0].CapabilityID != other.ID {
		t.Fatalf("缺失应记 missing、未要求的内容进 Extra：%+v", stats)
	}
	dep, err = db.GetDeploymentByNodeCapability(ctx, ns, node.ID, c.ID)
	if err != nil {
		t.Fatalf("get missing deployment: %v", err)
	}
	if dep.State != store.DeploymentPending || !strings.Contains(dep.Reason, "missing") {
		t.Fatalf("missing 应 pending + reason=missing：%+v", dep)
	}
	// local 变体不建部署行（意图只能来自控制台）。
	if _, err := db.GetDeploymentByNodeCapability(ctx, ns, node.ID, other.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("节点自报不该替中枢建部署意图，得到 %v", err)
	}
}

// TestDeployNewDigestSupersedes 验同一 (node, capability) 只保留一行（新期望取代旧行）。
func TestDeployNewDigestSupersedes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "deploy-supersede")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)

	if _, err := db.CreateDeployments(ctx, ns, c.ID, "", []string{node.ID}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 能力内容变了（新版本 = 新 digest）。
	c2 := createCap(t, db, testCap(ns, "analyze", 2, nil))
	if c2.Digest == c.Digest {
		t.Fatalf("不同版本应有不同 digest（夹具前提）")
	}
	if _, err := db.CreateDeployments(ctx, ns, c2.ID, "", []string{node.ID}); err != nil {
		t.Fatalf("create v2: %v", err)
	}
	all, err := db.ListDeployments(ctx, ns, store.DeploymentFilter{NodeID: node.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// 两条能力（同能力不同版本是两行 capability_versions），各自一行部署；
	// 但 (node, capability_id) 唯一，所以这里恰好 2 行。
	if len(all) != 2 {
		t.Fatalf("每 (node, capability) 一行，得到 %d", len(all))
	}
}

// TestDeployDeliveryForNode 验补推形状：digest 与 spec 同源（都来自能力行）。
func TestDeployDeliveryForNode(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "deploy-delivery")
	c := createCap(t, db, testCap(ns, "analyze", 1, nil))
	node := newNode(t, db, "node-a", 1)
	if _, err := db.CreateDeployments(ctx, ns, c.ID, "", []string{node.ID}); err != nil {
		t.Fatalf("create: %v", err)
	}
	items, err := db.DeploymentsForNode(ctx, ns, node.ID)
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应有一条补推，得到 %d", len(items))
	}
	if items[0].Digest != c.Digest || len(items[0].Spec) == 0 {
		t.Fatalf("补推的 digest/spec 都该来自能力行：%+v", items[0])
	}
	// 删除部署后不该再有补推。
	dep, err := db.GetDeploymentByNodeCapability(ctx, ns, node.ID, c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := db.DeleteDeployment(ctx, ns, dep.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if items, err := db.DeploymentsForNode(ctx, ns, node.ID); err != nil || len(items) != 0 {
		t.Fatalf("删除后不该再补推：%v %v", items, err)
	}
}
