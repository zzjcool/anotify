package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// mkNode 建一台节点。
func mkNode(t *testing.T, db *DB, orgID, name string) (*Node, string) {
	t.Helper()
	n := &Node{OrgID: orgID, Name: name, Tags: []string{"pool-a"}}
	token, err := db.CreateNode(context.Background(), n)
	if err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	return n, token
}

// TestNodeCreateAndGet 基本往返 + 明文 token 只出现一次。
func TestNodeCreateAndGet(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	n, token := mkNode(t, db, orgID, "mbp-16")
	if token == "" {
		t.Fatal("CreateNode returned empty token")
	}
	if !strings.HasPrefix(token, "ant_node_") {
		t.Fatalf("token %q missing ant_node_ prefix", token)
	}
	if n.TokenHash != HashNodeToken(token) {
		t.Fatal("stored token hash is not sha256(token)")
	}
	// 明文不能落库
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM nodes WHERE id = ?`, n.ID).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if stored == token {
		t.Fatal("node token stored in plaintext")
	}

	got, err := db.GetNode(ctx, orgID, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Name != "mbp-16" || got.Status != NodeStatusOffline {
		t.Fatalf("node round-trip mismatch: %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "pool-a" {
		t.Fatalf("tags mismatch: %v", got.Tags)
	}
	if got.MaxInstances != DefaultMaxInstances || got.MaxConcurrency != DefaultMaxConcurrency {
		t.Fatalf("defaults not applied: %d / %d", got.MaxInstances, got.MaxConcurrency)
	}

	// 用 token hash 反查（握手鉴权的唯一路径）
	byToken, err := db.GetNodeByTokenHash(ctx, HashNodeToken(token))
	if err != nil {
		t.Fatalf("get node by token hash: %v", err)
	}
	if byToken.ID != n.ID || byToken.OrgID != orgID {
		t.Fatalf("token lookup returned wrong node: %+v", byToken)
	}
	if _, err := db.GetNodeByTokenHash(ctx, HashNodeToken("ant_node_wrong")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong token err = %v, want ErrNotFound", err)
	}
}

// TestNodeOrgIsolation 不变量 I10：跨组织查不到节点。
func TestNodeOrgIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgA, _ := newTestOrg(t, db)
	orgB, _ := newTestOrg(t, db)

	n, token := mkNode(t, db, orgA, "shared-name")
	if _, err := db.GetNode(ctx, orgB, n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org GetNode err = %v, want ErrNotFound", err)
	}
	// 同名节点可存在于不同组织（UNIQUE 是 (org_id, name)）
	if _, err := db.CreateNode(ctx, &Node{OrgID: orgB, Name: "shared-name"}); err != nil {
		t.Fatalf("same name in another org should be allowed: %v", err)
	}
	// 同组织同名拒绝
	if _, err := db.CreateNode(ctx, &Node{OrgID: orgA, Name: "shared-name"}); err == nil {
		t.Fatal("duplicate node name in same org accepted")
	}
	// token 是全局唯一的，所以 by-token 查询返回的是持有者所在组织
	byToken, err := db.GetNodeByTokenHash(ctx, HashNodeToken(token))
	if err != nil {
		t.Fatalf("by token: %v", err)
	}
	if byToken.OrgID != orgA {
		t.Fatalf("token resolved to org %q, want %q", byToken.OrgID, orgA)
	}
}

// TestNodeCapabilityUnknown 不变量 I2 / A3：声明未定义能力必须拒绝（#62 验收）。
func TestNodeCapabilityUnknown(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, _ := mkNode(t, db, orgID, "mbp")

	_, err := db.ConfirmCapabilities(ctx, orgID, n.ID, []CapabilityRef{{Name: "review", Version: 1}})
	if !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("declare undefined capability: err = %v, want ErrUnknownCapability", err)
	}
	// 拒绝后不得留下半份声明
	caps, err := db.ListNodeCapabilities(ctx, n.ID)
	if err != nil {
		t.Fatalf("list node caps: %v", err)
	}
	if len(caps) != 0 {
		t.Fatalf("rejected declaration left %d rows behind", len(caps))
	}

	// 定义了之后就能声明
	c := mkCap(orgID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, c); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	ids, err := db.ConfirmCapabilities(ctx, orgID, n.ID, []CapabilityRef{{Name: "review", Version: 1}})
	if err != nil {
		t.Fatalf("declare defined capability: %v", err)
	}
	if len(ids) != 1 || ids[0] != c.ID {
		t.Fatalf("ConfirmCapabilities returned %v, want [%s]", ids, c.ID)
	}

	// 版本不存在也要拒绝（review@2 没定义）
	if _, err := db.ConfirmCapabilities(ctx, orgID, n.ID, []CapabilityRef{{Name: "review", Version: 2}}); !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("undefined version err = %v, want ErrUnknownCapability", err)
	}
}

// TestNodeCapabilitiesReplacedOnRehello 再次 hello 是全量替换，不是追加。
func TestNodeCapabilitiesReplacedOnRehello(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, _ := mkNode(t, db, orgID, "mbp")

	review := mkCap(orgID, "review", 1, []string{"fix"})
	testCap := mkCap(orgID, "test", 1, nil)
	for _, c := range []*CapabilityVersion{review, testCap} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create cap: %v", err)
		}
	}
	if _, err := db.ConfirmCapabilities(ctx, orgID, n.ID, []CapabilityRef{
		{Name: "review", Version: 1}, {Name: "test", Version: 1},
	}); err != nil {
		t.Fatalf("first hello: %v", err)
	}
	caps, err := db.ListNodeCapabilities(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("want 2 declared capabilities, got %d", len(caps))
	}

	// 第二次只声明 review（节点升级后不再支持 test）
	if _, err := db.ConfirmCapabilities(ctx, orgID, n.ID, []CapabilityRef{{Name: "review", Version: 1}}); err != nil {
		t.Fatalf("second hello: %v", err)
	}
	caps, err = db.ListNodeCapabilities(ctx, n.ID)
	if err != nil {
		t.Fatalf("list after rehello: %v", err)
	}
	if len(caps) != 1 || caps[0].Name != "review" {
		t.Fatalf("rehello did not replace declarations: %v", caps)
	}
}

// TestNodeCapabilitiesNoArgvColumn node_capabilities 只有 (node_id, capability_id)，
// 没有命令、没有出口、没有 kind（不变量 I1 / I3）。
func TestNodeCapabilitiesNoArgvColumn(t *testing.T) {
	db := newTestDB(t)
	cols := columnNames(t, db, "node_capabilities")
	if len(cols) != 2 {
		t.Fatalf("node_capabilities has %d columns %v, want exactly 2 (node_id, capability_id)", len(cols), cols)
	}
	if !cols["node_id"] || !cols["capability_id"] {
		t.Fatalf("node_capabilities columns %v do not match contract", cols)
	}
	for banned := range map[string]bool{"argv": true, "command": true, "shell": true, "script": true, "kind": true, "outputs": true} {
		if cols[banned] {
			t.Errorf("node_capabilities has banned column %q", banned)
		}
	}
}

// TestNodeMaxInstances 超 nodes.max_instances 拒绝（#62 验收 / 契约 Q5）。
func TestNodeMaxInstances(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n := &Node{OrgID: orgID, Name: "small", MaxInstances: 2}
	if _, err := db.CreateNode(ctx, n); err != nil {
		t.Fatalf("create node: %v", err)
	}

	// 正好 2 个：允许
	insts, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{{Slot: 0, Concurrency: 4}, {Slot: 1, Concurrency: 4}})
	if err != nil {
		t.Fatalf("2 instances (at limit): %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("want 2 instances, got %d", len(insts))
	}

	// 3 个：拒绝
	_, err = db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{
		{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}, {Slot: 2, Concurrency: 1},
	})
	if !errors.Is(err, ErrTooManyInstances) {
		t.Fatalf("3 instances over max 2: err = %v, want ErrTooManyInstances", err)
	}
	// 拒绝后原有槽位不受影响
	insts, err = db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("rejected declaration mutated instances: got %d", len(insts))
	}
}

// TestNodeInstancesUpsertAndShrink 实例申报是完整清单：缩容会删掉旧槽位。
func TestNodeInstancesUpsertAndShrink(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, _ := mkNode(t, db, orgID, "mbp")

	if _, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{
		{Slot: 0, Concurrency: 2}, {Slot: 1, Concurrency: 2}, {Slot: 2, Concurrency: 2},
	}); err != nil {
		t.Fatalf("declare 3: %v", err)
	}
	// 缩到 1 个
	insts, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{{Slot: 0, Concurrency: 8}})
	if err != nil {
		t.Fatalf("shrink to 1: %v", err)
	}
	if len(insts) != 1 || insts[0].Slot != 0 || insts[0].Concurrency != 8 {
		t.Fatalf("shrink failed: %+v", insts)
	}
	// 槽位 1/2 已删除
	var n2 int
	if err := db.QueryRow(`SELECT count(*) FROM node_instances WHERE node_id=?`, n.ID).Scan(&n2); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("stale slots not removed: %d rows", n2)
	}

	// 重复 slot 拒绝
	if _, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{{Slot: 0}, {Slot: 0}}); err == nil {
		t.Fatal("duplicate slot accepted")
	}
	// 负数 slot 拒绝
	if _, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{{Slot: -1}}); err == nil {
		t.Fatal("negative slot accepted")
	}
}

// TestUpsertInstancesKeepsBusyTask 重新 hello 不该丢掉正在跑的任务归属。
func TestUpsertInstancesKeepsBusyTask(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, _ := mkNode(t, db, orgID, "mbp")

	if _, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	instID, err := db.LeaseInstance(ctx, n.ID, "tsk_busy")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	// 重连 hello
	if _, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		t.Fatalf("re-declare: %v", err)
	}
	insts, err := db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(insts) != 1 || insts[0].ID != instID {
		t.Fatalf("instance identity changed on re-declare: %+v", insts)
	}
	if insts[0].BusyTask == nil || *insts[0].BusyTask != "tsk_busy" {
		t.Fatalf("busy_task lost on re-declare: %+v", insts[0])
	}
}

// TestLeaseInstanceAndRelease 槽位抢占：占满后 ErrNoCapacity。
func TestLeaseInstanceAndRelease(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, _ := mkNode(t, db, orgID, "mbp")

	if _, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{
		{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1},
	}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	id1, err := db.LeaseInstance(ctx, n.ID, "tsk_1")
	if err != nil {
		t.Fatalf("lease 1: %v", err)
	}
	id2, err := db.LeaseInstance(ctx, n.ID, "tsk_2")
	if err != nil {
		t.Fatalf("lease 2: %v", err)
	}
	if id1 == id2 {
		t.Fatal("two tasks leased the same instance")
	}
	// 满了
	if _, err := db.LeaseInstance(ctx, n.ID, "tsk_3"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("third lease err = %v, want ErrNoCapacity", err)
	}
	// 释放一个
	if err := db.ReleaseInstance(ctx, n.ID, id1); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := db.LeaseInstance(ctx, n.ID, "tsk_4"); err != nil {
		t.Fatalf("lease after release: %v", err)
	}
	// 按 task 释放
	if err := db.ReleaseInstanceByTask(ctx, n.ID, "tsk_2"); err != nil {
		t.Fatalf("release by task: %v", err)
	}
	insts, err := db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	busy := 0
	for _, in := range insts {
		if in.BusyTask != nil {
			busy++
		}
	}
	if busy != 1 { // 只剩 tsk_4
		t.Fatalf("want 1 busy instance, got %d", busy)
	}
}

// TestLeaseInstanceDeadSlotIgnored 掉线的槽位不能被派任务。
func TestLeaseInstanceDeadSlotIgnored(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, _ := mkNode(t, db, orgID, "mbp")

	if _, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := db.MarkInstancesDead(ctx, n.ID); err != nil {
		t.Fatalf("mark dead: %v", err)
	}
	if _, err := db.LeaseInstance(ctx, n.ID, "tsk_x"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("lease on dead instance err = %v, want ErrNoCapacity", err)
	}
	// 重新 hello 让槽位复活
	if _, err := db.UpsertInstances(ctx, orgID, n.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("re-declare: %v", err)
	}
	if _, err := db.LeaseInstance(ctx, n.ID, "tsk_y"); err != nil {
		t.Fatalf("lease after revive: %v", err)
	}
}

// TestNodesForCapability 派发第 ③ 步：只返回可接单的节点。
func TestNodesForCapability(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	review := mkCap(orgID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, review); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	n1, _ := mkNode(t, db, orgID, "n1")
	n2, _ := mkNode(t, db, orgID, "n2")
	n3, _ := mkNode(t, db, orgID, "n3")
	for _, n := range []*Node{n1, n2, n3} {
		if _, err := db.ConfirmCapabilities(ctx, orgID, n.ID, []CapabilityRef{{Name: "review", Version: 1}}); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if err := db.UpdateNodeStatus(ctx, orgID, n.ID, NodeStatusOnline); err != nil {
			t.Fatalf("online: %v", err)
		}
	}
	got, err := db.NodesForCapability(ctx, orgID, review.ID)
	if err != nil {
		t.Fatalf("NodesForCapability: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 nodes, got %d (%v)", len(got), got)
	}

	// 排空一台 → 不再被选中
	if err := db.UpdateNodeStatus(ctx, orgID, n2.ID, NodeStatusDraining); err != nil {
		t.Fatalf("drain: %v", err)
	}
	got, err = db.NodesForCapability(ctx, orgID, review.ID)
	if err != nil {
		t.Fatalf("NodesForCapability after drain: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("draining node still selectable: got %v", got)
	}
	for _, id := range got {
		if id == n2.ID {
			t.Fatal("draining node in candidate list")
		}
	}

	// 没有节点的能力 → 空列表
	other := mkCap(orgID, "nobody", 1, nil)
	if err := db.CreateCapability(ctx, other); err != nil {
		t.Fatalf("create other: %v", err)
	}
	none, err := db.NodesForCapability(ctx, orgID, other.ID)
	if err != nil {
		t.Fatalf("NodesForCapability none: %v", err)
	}
	if none == nil {
		t.Fatal("NodesForCapability returned nil (need [])")
	}
	if len(none) != 0 {
		t.Fatalf("want 0, got %d", len(none))
	}
}

// TestNodesForCapabilityOrgIsolation 别的组织的节点不能出现在候选里。
func TestNodesForCapabilityOrgIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgA, _ := newTestOrg(t, db)
	orgB, _ := newTestOrg(t, db)

	// 两个组织各定义同名能力（各自独立的版本行）
	capA := mkCap(orgA, "review", 1, []string{"fix"})
	capB := mkCap(orgB, "review", 1, []string{"fix"})
	for _, c := range []*CapabilityVersion{capA, capB} {
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create cap: %v", err)
		}
	}
	nA, _ := mkNode(t, db, orgA, "a-node")
	if _, err := db.ConfirmCapabilities(ctx, orgA, nA.ID, []CapabilityRef{{Name: "review", Version: 1}}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := db.UpdateNodeStatus(ctx, orgA, nA.ID, NodeStatusOnline); err != nil {
		t.Fatalf("online: %v", err)
	}

	// orgB 查自己的能力：看不到 orgA 的节点
	if got, err := db.NodesForCapability(ctx, orgB, capB.ID); err != nil {
		t.Fatalf("orgB lookup: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("orgB sees %d nodes from orgA", len(got))
	}
	// 用 orgB 的 org_id 查 orgA 的 capability_id：也必须是空
	if got, err := db.NodesForCapability(ctx, orgB, capA.ID); err != nil {
		t.Fatalf("cross lookup: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("cross-org lookup returned %d nodes", len(got))
	}
}

// TestNodePins 粘性路由（#62 验收）。
func TestNodePins(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, _ := mkNode(t, db, orgID, "mbp")

	key := StickyKey("trace_1", "cap_review", "worktree:wt-a")
	if len(key) != 64 {
		t.Fatalf("StickyKey length = %d, want 64 (hex sha256)", len(key))
	}
	// 同样输入同 key
	if StickyKey("trace_1", "cap_review", "worktree:wt-a") != key {
		t.Fatal("StickyKey is not deterministic")
	}
	// 不同 trace → 不同 key
	if StickyKey("trace_2", "cap_review", "worktree:wt-a") == key {
		t.Fatal("StickyKey ignores trace_id")
	}
	// 不同 selector → 不同 key
	if StickyKey("trace_1", "cap_review", "worktree:wt-b") == key {
		t.Fatal("StickyKey ignores selector")
	}
	// 拼接歧义：(ab, c) 与 (a, bc) 必须不同
	if StickyKey("ab", "c", "x") == StickyKey("a", "bc", "x") {
		t.Fatal("StickyKey concatenation is ambiguous")
	}

	// 未命中
	if _, err := db.GetPin(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPin missing err = %v, want ErrNotFound", err)
	}

	pin := &NodePin{StickyKey: key, OrgID: orgID, NodeID: n.ID, Selector: "worktree:wt-a"}
	if err := db.PutPin(ctx, pin); err != nil {
		t.Fatalf("PutPin: %v", err)
	}
	got, err := db.GetPin(ctx, key)
	if err != nil {
		t.Fatalf("GetPin: %v", err)
	}
	if got.NodeID != n.ID || got.Selector != "worktree:wt-a" || got.OrgID != orgID {
		t.Fatalf("pin round-trip mismatch: %+v", got)
	}

	// 重复 PutPin 不换机器，只刷新 last_used_at
	before := got.LastUsedAt
	if err := db.PutPin(ctx, &NodePin{
		StickyKey: key, OrgID: orgID, NodeID: n.ID, Selector: "worktree:wt-a", LastUsedAt: before + 100,
	}); err != nil {
		t.Fatalf("PutPin again: %v", err)
	}
	got, err = db.GetPin(ctx, key)
	if err != nil {
		t.Fatalf("GetPin again: %v", err)
	}
	if got.LastUsedAt != before+100 {
		t.Fatalf("last_used_at not refreshed: %d", got.LastUsedAt)
	}

	// 删除
	if err := db.DeletePin(ctx, key); err != nil {
		t.Fatalf("DeletePin: %v", err)
	}
	if _, err := db.GetPin(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pin not deleted")
	}
}

// TestPutPinValidation 入参校验。
func TestPutPinValidation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.PutPin(ctx, nil); err == nil {
		t.Error("PutPin(nil) accepted")
	}
	if err := db.PutPin(ctx, &NodePin{StickyKey: "k"}); err == nil {
		t.Error("PutPin without org/node accepted")
	}
}

// TestNodeStatusTransitions 状态更新与非法值。
func TestNodeStatusTransitions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, _ := mkNode(t, db, orgID, "mbp")

	for _, st := range []string{NodeStatusOnline, NodeStatusDraining, NodeStatusOffline} {
		if err := db.UpdateNodeStatus(ctx, orgID, n.ID, st); err != nil {
			t.Fatalf("set %s: %v", st, err)
		}
		got, err := db.GetNode(ctx, orgID, n.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != st {
			t.Fatalf("status = %q, want %q", got.Status, st)
		}
	}
	if err := db.UpdateNodeStatus(ctx, orgID, n.ID, "bogus"); err == nil {
		t.Fatal("invalid status accepted")
	}
	if err := db.UpdateNodeStatus(ctx, orgID, "nd_missing", NodeStatusOnline); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node err = %v, want ErrNotFound", err)
	}
}

// TestTouchNode last_seen_at 刷新（上行帧续命）。
func TestTouchNode(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)
	n, _ := mkNode(t, db, orgID, "mbp")

	if err := db.TouchNode(ctx, orgID, n.ID, 12345); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, err := db.GetNode(ctx, orgID, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastSeenAt == nil || *got.LastSeenAt != 12345 {
		t.Fatalf("last_seen_at = %v, want 12345", got.LastSeenAt)
	}
}

// TestNodeEmptyListIsArray 不变量 I9。
func TestNodeEmptyListIsArray(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	nodes, err := db.ListNodes(ctx, orgID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if nodes == nil {
		t.Fatal("ListNodes returned nil (need [])")
	}
	n, _ := mkNode(t, db, orgID, "mbp")
	insts, err := db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if insts == nil {
		t.Fatal("ListInstances returned nil (need [])")
	}
	caps, err := db.ListNodeCapabilities(ctx, n.ID)
	if err != nil {
		t.Fatalf("list node caps: %v", err)
	}
	if caps == nil {
		t.Fatal("ListNodeCapabilities returned nil (need [])")
	}
}

// TestNodeTagsRoundTrip tags JSON 往返。
func TestNodeTagsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	n := &Node{OrgID: orgID, Name: "tagged", Tags: []string{"mbp", "apple-silicon", "pool-a"}}
	if _, err := db.CreateNode(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetNode(ctx, orgID, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Tags) != 3 {
		t.Fatalf("tags lost: %v", got.Tags)
	}
	if encoded, _ := json.Marshal(got.Tags); string(encoded) != `["mbp","apple-silicon","pool-a"]` {
		t.Fatalf("tags order/content changed: %s", encoded)
	}
}

// TestNewNodeTokenUnique token 唯一且每次都不同。
func TestNewNodeTokenUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		token, hash, err := NewNodeToken()
		if err != nil {
			t.Fatalf("NewNodeToken: %v", err)
		}
		if seen[token] {
			t.Fatalf("duplicate token %q", token)
		}
		seen[token] = true
		if hash != HashNodeToken(token) {
			t.Fatal("returned hash does not match sha256(token)")
		}
	}
}

// TestFreeSlotsByNode 聚合查询的空闲槽位统计（#145）。
//
// 语义必须与 ListInstances 的口径一致：**alive 且 busy_task IS NULL** 才算空闲。
// 两个边界最容易写错：
//   - 没申报槽位的节点必须出现在结果里（free=0），否则调用方会在 map 里查不到
//     而把它当成"不存在"，与"优先级最低但仍可派发"的现有语义不符
//   - 死掉的槽位不算空闲，否则会把任务派给一台已经掉线的机器
func TestFreeSlotsByNode(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	orgID, _ := newTestOrg(t, db)

	mk := func(name string, slots int) *Node {
		n, _ := mkNode(t, db, orgID, name)
		if slots > 0 {
			specs := make([]InstanceSpec, slots)
			for i := range specs {
				specs[i] = InstanceSpec{Slot: i, Concurrency: 1}
			}
			if _, err := db.UpsertInstances(ctx, orgID, n.ID, specs); err != nil {
				t.Fatalf("instances for %s: %v", name, err)
			}
		}
		return n
	}

	twoFree := mk("two-free", 2)
	oneBusy := mk("one-busy", 2)
	noSlots := mk("no-slots", 0)
	dead := mk("dead", 1)

	// oneBusy 占掉一个槽位
	if _, err := db.LeaseInstance(ctx, oneBusy.ID, "tk_1"); err != nil {
		t.Fatalf("lease instance: %v", err)
	}
	// dead 的槽位标成死亡
	if err := db.MarkInstancesDead(ctx, dead.ID); err != nil {
		t.Fatalf("mark dead: %v", err)
	}

	got, err := db.FreeSlotsByNode(ctx, []string{twoFree.ID, oneBusy.ID, noSlots.ID, dead.ID})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	want := map[string]int{twoFree.ID: 2, oneBusy.ID: 1, noSlots.ID: 0, dead.ID: 0}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("node %s free = %d, want %d", id, got[id], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("result has %d entries, want %d (%v)", len(got), len(want), got)
	}

	// 空输入不能查库报错，也不能返回 nil map（调用方直接下标取值）
	empty, err := db.FreeSlotsByNode(ctx, nil)
	if err != nil {
		t.Fatalf("empty input: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty input = %v, want 空 map", empty)
	}
}
