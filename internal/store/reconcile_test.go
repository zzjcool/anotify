package store

// reconcile_test.go 覆盖 v4 对账（capability.md §2.4：「中枢覆盖」语义）：
//
//	节点 hello 自报 applied[]（能力 id + 版本 + digest），中枢按**能力行当前 digest**
//	对账它的部署行：一致 → applied；不一致 → pending(stale)；没报 → pending(missing)；
//	节点有、中枢没要求 → 只作为 Extra 漂移事实返回，**不**替它建部署行。
//
// v3 的 AppliedRef / ReconcileApplied（按 name+version 引用、会建 local 部署行）已删除：
// v4 用 AppliedCapability（capabilityId 是唯一身份），本地变体不再变成部署意图。

import (
	"context"
	"strings"
	"testing"
)

// reconcileFixture 建一个能力 + 一台节点 + 一条部署行，返回 (cap, node, deployID)。
func reconcileFixture(t *testing.T, db *DB, nsID, name, nodeName string) (*CapabilityVersion, *Node, string) {
	t.Helper()
	c := v4Cap(t, db, nsID, name, 1)
	node := v4Node(t, db, nodeName, 0)
	rows, err := db.CreateDeployments(context.Background(), nsID, c.ID, c.Digest, []string{node.ID})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	return c, node, rows[0].ID
}

// TestReconcileAppliedRoundTrip 节点报同一 digest → applied，reason 清空。
func TestReconcileAppliedRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, node, deployID := reconcileFixture(t, db, nsID, "review", "rc-node")

	stats, err := db.ReconcileNode(ctx, node.ID, []AppliedCapability{{
		CapabilityID: c.ID, Capability: c.Name, Version: c.Version, Digest: c.Digest,
	}}, Now())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Applied != 1 || stats.Stale != 0 || stats.Missing != 0 || len(stats.Extra) != 0 {
		t.Fatalf("对账统计 = %+v, want applied=1", stats)
	}
	got, err := db.GetDeployment(ctx, nsID, deployID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.State != DeploymentApplied || got.Reason != "" {
		t.Fatalf("对账后部署 = %+v, want applied + 空 reason", got)
	}
}

// TestReconcileAppliedFullReplace digest 不一致 → pending + stale 说明（写明两端 digest）。
func TestReconcileAppliedFullReplace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, node, deployID := reconcileFixture(t, db, nsID, "review", "rc-stale")

	stats, err := db.ReconcileNode(ctx, node.ID, []AppliedCapability{{
		CapabilityID: c.ID, Capability: c.Name, Version: c.Version, Digest: "sha256:other",
	}}, Now())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Stale != 1 || stats.Applied != 0 {
		t.Fatalf("对账统计 = %+v, want stale=1", stats)
	}
	got, err := db.GetDeployment(ctx, nsID, deployID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.State != DeploymentPending {
		t.Fatalf("stale 部署应回 pending，得到 %s", got.State)
	}
	if !strings.Contains(got.Reason, "stale") || !strings.Contains(got.Reason, c.Digest) {
		t.Fatalf("stale reason 必须写明两端 digest，得到 %q", got.Reason)
	}
}

// TestReconcileAppliedListsAllStale 没上报的部署行 → pending + missing 说明。
func TestReconcileAppliedListsAllStale(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	_, node, deployID := reconcileFixture(t, db, nsID, "review", "rc-missing")

	stats, err := db.ReconcileNode(ctx, node.ID, nil, Now())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Missing != 1 || stats.Applied != 0 || stats.Stale != 0 {
		t.Fatalf("对账统计 = %+v, want missing=1", stats)
	}
	got, err := db.GetDeployment(ctx, nsID, deployID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.State != DeploymentPending || !strings.Contains(got.Reason, "missing") {
		t.Fatalf("缺失部署 = %+v, want pending + missing", got)
	}
}

// TestReconcileAppliedExtraIsNotADeployment 节点有、中枢没要求 → Extra 事实，0 新部署行。
func TestReconcileAppliedExtraIsNotADeployment(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, node, _ := reconcileFixture(t, db, nsID, "review", "rc-extra")
	local := v4Cap(t, db, nsID, "local-variant", 1)

	before, err := db.ListDeployments(ctx, nsID, DeploymentFilter{})
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	stats, err := db.ReconcileNode(ctx, node.ID, []AppliedCapability{
		{CapabilityID: c.ID, Capability: c.Name, Version: c.Version, Digest: c.Digest},
		{CapabilityID: local.ID, Capability: local.Name, Version: local.Version, Digest: local.Digest},
	}, Now())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(stats.Extra) != 1 || stats.Extra[0].CapabilityID != local.ID {
		t.Fatalf("Extra = %+v, want 只有本地变体 %s", stats.Extra, local.ID)
	}
	after, err := db.ListDeployments(ctx, nsID, DeploymentFilter{})
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("本地变体被写成了部署意图: %d → %d 行", len(before), len(after))
	}
}

// TestReconcileAppliedDigestMismatchMarksPending 先 stale 后一致 → 回到 applied（可收敛）。
func TestReconcileAppliedDigestMismatchMarksPending(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, node, deployID := reconcileFixture(t, db, nsID, "review", "rc-converge")

	if _, err := db.ReconcileNode(ctx, node.ID, []AppliedCapability{{
		CapabilityID: c.ID, Digest: "sha256:other",
	}}, Now()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	stats, err := db.ReconcileNode(ctx, node.ID, []AppliedCapability{{
		CapabilityID: c.ID, Capability: c.Name, Version: c.Version, Digest: c.Digest,
	}}, Now())
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if stats.Applied != 1 {
		t.Fatalf("第二次对账应 applied，得到 %+v", stats)
	}
	got, err := db.GetDeployment(ctx, nsID, deployID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.State != DeploymentApplied {
		t.Fatalf("收敛后应 applied，得到 %s（reason=%q）", got.State, got.Reason)
	}
}

// TestReconcileAppliedDigestMatchMarksApplied 期望值取**能力行当前 digest**：
// 同能力换版本不影响该部署行的期望（它指向的能力行没变）。
func TestReconcileAppliedDigestMatchMarksApplied(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, node, _ := reconcileFixture(t, db, nsID, "review", "rc-current")

	next := mkCap(nsID, "review", 2, nil)
	next.Spec.Run.Files["run.sh"] = "#!/bin/sh\nexit 4\n"
	if err := db.CreateCapability(ctx, next); err != nil {
		t.Fatalf("create v2: %v", err)
	}
	stats, err := db.ReconcileNode(ctx, node.ID, []AppliedCapability{{
		CapabilityID: c.ID, Capability: c.Name, Version: c.Version, Digest: c.Digest,
	}}, Now())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Applied != 1 {
		t.Fatalf("部署行指向的能力未变，应 applied，得到 %+v", stats)
	}
}

// TestReconcileAppliedFeedsDispatch 对账为 applied 的部署仍在派发面可见（部署上下文不丢）。
func TestReconcileAppliedFeedsDispatch(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c, node, _ := reconcileFixture(t, db, nsID, "review", "rc-dispatch")

	if _, err := db.ReconcileNode(ctx, node.ID, []AppliedCapability{{
		CapabilityID: c.ID, Capability: c.Name, Version: c.Version, Digest: c.Digest,
	}}, Now()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	deliveries, err := db.DeploymentsForNode(ctx, nsID, node.ID)
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].CapabilityID != c.ID {
		t.Fatalf("对账后的交付面 = %+v, want 1 条到 %s", deliveries, c.ID)
	}
}
