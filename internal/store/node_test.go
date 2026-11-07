package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// mkNode 建一台节点。
func mkNode(t *testing.T, db *DB, nsID, name string) (*Node, string) {
	t.Helper()
	n := &Node{NamespaceID: nsID, Name: name, Tags: []string{"pool-a"}}
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
	nsID, _ := newTestNS(t, db)

	n, token := mkNode(t, db, nsID, "mbp-16")
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

	got, err := db.GetNode(ctx, nsID, n.ID)
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
	// 令牌全局唯一 → by-token 查询不看命名空间；但它**必须**把行的 namespace_id
	// 扫出来（握手后整条连接靠 NamespaceID 定位隔离域，I11）。
	if byToken.ID != n.ID || byToken.NamespaceID != nsID {
		t.Fatalf("token lookup returned wrong node (namespace): %+v", byToken)
	}
	if _, err := db.GetNodeByTokenHash(ctx, HashNodeToken("ant_node_wrong")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong token err = %v, want ErrNotFound", err)
	}
}

// TestNodeNamespaceIsolation 不变量 I11：跨命名空间查不到节点。
func TestNodeNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, _ := newTestNS(t, db)

	n, token := mkNode(t, db, nsA, "shared-name")
	if _, err := db.GetNode(ctx, nsB, n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-namespace GetNode err = %v, want ErrNotFound", err)
	}
	// 同名节点可存在于不同命名空间（UNIQUE 是 (namespace_id, name)）：
	// 这正是 #162 把唯一键前缀从「组织」换成 namespace_id 的直接后果——
	// prod 与 staging 可以各有一台 mbp-16。
	if _, err := db.CreateNode(ctx, &Node{NamespaceID: nsB, Name: "shared-name"}); err != nil {
		t.Fatalf("same name in another namespace should be allowed: %v", err)
	}
	// 同命名空间同名拒绝
	if _, err := db.CreateNode(ctx, &Node{NamespaceID: nsA, Name: "shared-name"}); err == nil {
		t.Fatal("duplicate node name in same namespace accepted")
	}
	// 同一个库里另建的命名空间也要允许：唯一键前缀只有 namespace_id（否则新建的
	// 命名空间里连一台与默认命名空间同名的机器都登记不了）。
	nsA2 := newTestExtraNS(t, db, "staging")
	if _, err := db.CreateNode(ctx, &Node{NamespaceID: nsA2, Name: "shared-name"}); err != nil {
		t.Fatalf("same name in another namespace of the same deployment should be allowed: %v", err)
	}
	// token 是全局唯一的，所以 by-token 查询返回的是持有者所在命名空间
	byToken, err := db.GetNodeByTokenHash(ctx, HashNodeToken(token))
	if err != nil {
		t.Fatalf("by token: %v", err)
	}
	if byToken.NamespaceID != nsA {
		t.Fatalf("token resolved to namespace %q, want %q", byToken.NamespaceID, nsA)
	}
}

// TestNodeCapabilityUnknownIsStale 不变量 I2 / A3（**语义已改**）：节点报一条
// 中枢没有的能力，是**陈旧缓存**，不是致命错误。
//
// 旧行为（报错 → ws 整帧 unknown_capability → agent 退出 → systemd 重启 → 再拒）
// 的后果是「在控制台删一个能力 = 打死所有在线节点」，实测崩溃循环。
// 现在：不报错、不写归属、由调用方拿着 stale 让节点删缓存。
func TestNodeCapabilityUnknownIsStale(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	if _, err := db.ConfirmCapabilities(ctx, nsID, n.ID, []CapabilityRef{{Name: "review", Version: 1}}); err != nil {
		t.Fatalf("未定义能力应当按陈旧缓存处理（不报错），err = %v", err)
	}
	// 不写归属：中枢不从节点缓存推断「这台机器能跑什么」
	caps, err := db.ListNodeCapabilities(ctx, n.ID)
	if err != nil {
		t.Fatalf("list node caps: %v", err)
	}
	if len(caps) != 0 {
		t.Fatalf("陈旧缓存不该写成归属，留下的 %d 行", len(caps))
	}

	// 定义了之后就能声明
	c := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	ids, err := db.ConfirmCapabilities(ctx, nsID, n.ID, []CapabilityRef{{Name: "review", Version: 1}})
	if err != nil {
		t.Fatalf("declare defined capability: %v", err)
	}
	if len(ids) != 1 || ids[0] != c.ID {
		t.Fatalf("ConfirmCapabilities returned %v, want [%s]", ids, c.ID)
	}

	// 版本不存在同样是陈旧缓存（review@2 没定义）：不是错误，也不写归属。
	if _, err := db.ConfirmCapabilities(ctx, nsID, n.ID, []CapabilityRef{{Name: "review", Version: 2}}); err != nil {
		t.Fatalf("未定义版本应当按陈旧缓存处理（不报错），err = %v", err)
	}
	if caps, err := db.ListNodeCapabilities(ctx, n.ID); err != nil {
		t.Fatalf("list node caps: %v", err)
	} else if len(caps) != 0 {
		t.Fatalf("陈旧缓存写进了归属: %+v", caps)
	}
}

// TestNodeCapabilitiesReplacedOnRehello 再次 hello 是全量替换，不是追加。
func TestNodeCapabilitiesReplacedOnRehello(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	review := mkCap(nsID, "review", 1, []string{"fix"})
	testCap := mkCap(nsID, "test", 1, nil)
	for _, c := range []*CapabilityVersion{review, testCap} {
		if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
			t.Fatalf("create cap: %v", err)
		}
	}
	if _, err := db.ConfirmCapabilities(ctx, nsID, n.ID, []CapabilityRef{
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
	if _, err := db.ConfirmCapabilities(ctx, nsID, n.ID, []CapabilityRef{{Name: "review", Version: 1}}); err != nil {
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

// TestNodeCapabilitiesNoHarnessColumns node_capabilities 只有 (node_id, capability_id, namespace_id)。
//
// v2（console-declares）把 run_spec 这一列整列删掉（重建表）：这张表从
// 「节点自报的能力事实」降级成「已部署产物在节点上的派生索引」（契约 D4 / §8）。
// 执行方式属于产物的 spec（artifacts.spec），中枢不存、不读、不执行任何可执行内容。
//
// 断言三条：没有 argv / command / shell / script（I1 新版：中枢手上没有可执行内容）、
// 没有 run_spec（旧模型的残留语义载体，已随 v2 消失）、
// 没有 outputs / kind（I3：kind 由 DeriveKind 推导，不落库）。
func TestNodeCapabilitiesNoHarnessColumns(t *testing.T) {
	db := newTestDB(t)
	cols := columnNames(t, db, "node_capabilities")
	if len(cols) != 3 {
		t.Fatalf("node_capabilities has %d columns %v, want exactly 3 (node_id, capability_id, namespace_id)", len(cols), cols)
	}
	for _, want := range []string{"node_id", "capability_id", "namespace_id"} {
		if !cols[want] {
			t.Fatalf("node_capabilities columns %v missing %q", cols, want)
		}
	}
	for banned := range map[string]bool{
		"run_spec": true, "argv": true, "command": true, "shell": true, "script": true,
		"kind": true, "outputs": true,
	} {
		if cols[banned] {
			t.Errorf("node_capabilities has banned column %q", banned)
		}
	}
}

// TestNodeMaxInstances 超 nodes.max_instances 拒绝（#62 验收 / 契约 Q5）。
func TestNodeMaxInstances(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n := &Node{NamespaceID: nsID, Name: "small", MaxInstances: 2}
	if _, err := db.CreateNode(ctx, n); err != nil {
		t.Fatalf("create node: %v", err)
	}

	// 正好 2 个：允许
	insts, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{{Slot: 0, Concurrency: 4}, {Slot: 1, Concurrency: 4}})
	if err != nil {
		t.Fatalf("2 instances (at limit): %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("want 2 instances, got %d", len(insts))
	}

	// 3 个：拒绝
	_, err = db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{
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
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	if _, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{
		{Slot: 0, Concurrency: 2}, {Slot: 1, Concurrency: 2}, {Slot: 2, Concurrency: 2},
	}); err != nil {
		t.Fatalf("declare 3: %v", err)
	}
	// 缩到 1 个
	insts, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{{Slot: 0, Concurrency: 8}})
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
	if _, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{{Slot: 0}, {Slot: 0}}); err == nil {
		t.Fatal("duplicate slot accepted")
	}
	// 负数 slot 拒绝
	if _, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{{Slot: -1}}); err == nil {
		t.Fatal("negative slot accepted")
	}
}

// TestUpsertInstancesKeepsBusyTask 重新 hello 不该丢掉正在跑的任务归属。
func TestUpsertInstancesKeepsBusyTask(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	if _, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	instID, err := db.LeaseInstance(ctx, n.ID, "tsk_busy")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	// 重连 hello
	if _, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
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
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	if _, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{
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
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	if _, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := db.MarkInstancesDead(ctx, n.ID); err != nil {
		t.Fatalf("mark dead: %v", err)
	}
	if _, err := db.LeaseInstance(ctx, n.ID, "tsk_x"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("lease on dead instance err = %v, want ErrNoCapacity", err)
	}
	// 重新 hello 让槽位复活
	if _, err := db.UpsertInstances(ctx, nsID, n.ID, []InstanceSpec{{Slot: 0}}); err != nil {
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
	nsID, _ := newTestNS(t, db)

	review := mkCap(nsID, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(review)); err != nil {
		t.Fatalf("create cap: %v", err)
	}
	n1, _ := mkNode(t, db, nsID, "n1")
	n2, _ := mkNode(t, db, nsID, "n2")
	n3, _ := mkNode(t, db, nsID, "n3")
	for _, n := range []*Node{n1, n2, n3} {
		if _, err := db.ConfirmCapabilities(ctx, nsID, n.ID, []CapabilityRef{{Name: "review", Version: 1}}); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if err := db.UpdateNodeStatus(ctx, nsID, n.ID, NodeStatusOnline); err != nil {
			t.Fatalf("online: %v", err)
		}
	}
	got, err := db.NodesForCapability(ctx, nsID, review.ID)
	if err != nil {
		t.Fatalf("NodesForCapability: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 nodes, got %d (%v)", len(got), got)
	}

	// 排空一台 → 不再被选中
	if err := db.UpdateNodeStatus(ctx, nsID, n2.ID, NodeStatusDraining); err != nil {
		t.Fatalf("drain: %v", err)
	}
	got, err = db.NodesForCapability(ctx, nsID, review.ID)
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
	other := mkCap(nsID, "nobody", 1, nil)
	if err := db.CreateCapability(ctx, withSpec(other)); err != nil {
		t.Fatalf("create other: %v", err)
	}
	none, err := db.NodesForCapability(ctx, nsID, other.ID)
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

// TestNodesForCapabilityNamespaceIsolation 别的命名空间的节点不能出现在候选里（I11）。
//
// 两个维度都要验：两套独立的「用户 + 默认命名空间」（两个 default 命名空间）与
// 同一部署里另建的命名空间——后者是唯一隔离维度落在 namespace_id 上的核心面
// （prod 的节点不能接 staging 的任务，哪怕它们住在同一个库里）。
func TestNodesForCapabilityNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, _ := newTestNS(t, db)

	// 两个命名空间各定义同名能力（各自独立的版本行，UNIQUE 前缀是 namespace_id）
	capA := mkCap(nsA, "review", 1, []string{"fix"})
	capB := mkCap(nsB, "review", 1, []string{"fix"})
	for _, c := range []*CapabilityVersion{capA, capB} {
		if err := db.CreateCapability(ctx, withSpec(c)); err != nil {
			t.Fatalf("create cap: %v", err)
		}
	}
	nA, _ := mkNode(t, db, nsA, "a-node")
	if _, err := db.ConfirmCapabilities(ctx, nsA, nA.ID, []CapabilityRef{{Name: "review", Version: 1}}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := db.UpdateNodeStatus(ctx, nsA, nA.ID, NodeStatusOnline); err != nil {
		t.Fatalf("online: %v", err)
	}

	// nsB 查自己的能力：看不到 nsA 的节点
	if got, err := db.NodesForCapability(ctx, nsB, capB.ID); err != nil {
		t.Fatalf("nsB lookup: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("nsB sees %d nodes from nsA", len(got))
	}
	// 用 nsB 的命名空间查 nsA 的 capability_id：必须是空（能力 id 全局唯一，隔离靠 namespace_id）
	if got, err := db.NodesForCapability(ctx, nsB, capA.ID); err != nil {
		t.Fatalf("cross lookup: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("cross-namespace lookup returned %d nodes", len(got))
	}

	// 同一个库里另建的命名空间：同样不行。这是「唯一隔离维度是 namespace_id」的
	// 核心回归——若实现按别的维度过滤（例如已经删掉的组织层），这里会返回 nsA 的那台机器。
	sibling := newTestExtraNS(t, db, "staging")
	sibCap := mkCap(sibling, "review", 1, []string{"fix"})
	if err := db.CreateCapability(ctx, withSpec(sibCap)); err != nil {
		t.Fatalf("create sibling cap: %v", err)
	}
	if got, err := db.NodesForCapability(ctx, sibling, sibCap.ID); err != nil {
		t.Fatalf("sibling lookup: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("同库的另一个命名空间看到了 %d 台别处的机器（I11 破了）", len(got))
	}
}

// TestNodePins 粘性路由（#62 验收）。
func TestNodePins(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

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

	pin := &NodePin{StickyKey: key, NamespaceID: nsID, NodeID: n.ID, Selector: "worktree:wt-a"}
	if err := db.PutPin(ctx, pin); err != nil {
		t.Fatalf("PutPin: %v", err)
	}
	// 行身份（pin_id，迁移 7）由 putPinExec 就地填好并写回调用方——
	// 回滚靠它认自己那一行，不再是秒级的 created_at（#315）。
	if pin.PinID == "" {
		t.Fatal("PutPin 没把 PinID 写回调用方（回滚条件会不命中）")
	}
	got, err := db.GetPin(ctx, key)
	if err != nil {
		t.Fatalf("GetPin: %v", err)
	}
	// 钉住记的是 namespace_id（唯一隔离维度，由调用方显式传，不存在派生自组织层的字段）
	if got.NodeID != n.ID || got.Selector != "worktree:wt-a" || got.NamespaceID != nsID {
		t.Fatalf("pin round-trip mismatch: %+v", got)
	}
	// 存什么读什么：pin_id 也得往返一致（它现在是回滚的判据，丢了就退化成 0 行命中）。
	if got.PinID != pin.PinID {
		t.Fatalf("pin_id round-trip = %q，调用方拿到 %q", got.PinID, pin.PinID)
	}

	// 重复 PutPin 不换机器，只刷新 last_used_at；行身份也不被后来的调用冒领
	before := got.LastUsedAt
	if err := db.PutPin(ctx, &NodePin{
		StickyKey: key, NamespaceID: nsID, NodeID: n.ID, Selector: "worktree:wt-a", LastUsedAt: before + 100,
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
	if got.PinID != pin.PinID {
		t.Fatalf("已存在的 pin 被后来的 PutPin 换了身份：%q → %q", pin.PinID, got.PinID)
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
		t.Error("PutPin without namespace/node accepted")
	}
	// 有 node 但没命名空间：必须是 ErrNamespaceRequired（钉住必须落在某个命名空间里）
	if err := db.PutPin(ctx, &NodePin{StickyKey: "k", NodeID: "nd_x"}); !errors.Is(err, ErrNamespaceRequired) {
		t.Errorf("PutPin without namespace err = %v, want ErrNamespaceRequired", err)
	}
	// 通配符不是写入作用域
	if err := db.PutPin(ctx, &NodePin{StickyKey: "k", NamespaceID: AllNamespaces, NodeID: "nd_x"}); !errors.Is(err, ErrNamespaceRequired) {
		t.Errorf("PutPin on %q err = %v, want ErrNamespaceRequired", AllNamespaces, err)
	}
}

// TestNodeStatusTransitions 状态更新与非法值。
func TestNodeStatusTransitions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	for _, st := range []string{NodeStatusOnline, NodeStatusDraining, NodeStatusOffline} {
		if err := db.UpdateNodeStatus(ctx, nsID, n.ID, st); err != nil {
			t.Fatalf("set %s: %v", st, err)
		}
		got, err := db.GetNode(ctx, nsID, n.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != st {
			t.Fatalf("status = %q, want %q", got.Status, st)
		}
	}
	if err := db.UpdateNodeStatus(ctx, nsID, n.ID, "bogus"); err == nil {
		t.Fatal("invalid status accepted")
	}
	if err := db.UpdateNodeStatus(ctx, nsID, "nd_missing", NodeStatusOnline); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node err = %v, want ErrNotFound", err)
	}
}

// TestTouchNode last_seen_at 刷新（上行帧续命）。
func TestTouchNode(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	if err := db.TouchNode(ctx, nsID, n.ID, 12345); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, err := db.GetNode(ctx, nsID, n.ID)
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
	nsID, _ := newTestNS(t, db)

	nodes, err := db.ListNodes(ctx, nsID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if nodes == nil {
		t.Fatal("ListNodes returned nil (need [])")
	}
	n, _ := mkNode(t, db, nsID, "mbp")
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
	nsID, _ := newTestNS(t, db)

	n := &Node{NamespaceID: nsID, Name: "tagged", Tags: []string{"mbp", "apple-silicon", "pool-a"}}
	if _, err := db.CreateNode(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetNode(ctx, nsID, n.ID)
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
	nsID, _ := newTestNS(t, db)

	mk := func(name string, slots int) *Node {
		n, _ := mkNode(t, db, nsID, name)
		if slots > 0 {
			specs := make([]InstanceSpec, slots)
			for i := range specs {
				specs[i] = InstanceSpec{Slot: i, Concurrency: 1}
			}
			if _, err := db.UpsertInstances(ctx, nsID, n.ID, specs); err != nil {
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

// TestNodeCreateDuplicateName 重名映射成哨兵错误（#185 / 契约 D7）。
//
// 判定必须来自表约束 UNIQUE(namespace_id, name) 而不是预查：
// handler 拿到 ErrNodeExists 就回 409，TOCTOU 窗口由唯一索引兜住。
// 同时钉住唯一性的**范围**——命名空间内，跨命名空间可以同名。
func TestNodeCreateDuplicateName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, _ := newTestNS(t, db)

	first, _ := mkNode(t, db, nsA, "mbp-16")

	// 同命名空间重名 → ErrNodeExists（handler 据此回 409）
	_, err := db.CreateNode(ctx, &Node{NamespaceID: nsA, Name: "mbp-16"})
	if !errors.Is(err, ErrNodeExists) {
		t.Fatalf("duplicate name err = %v, want ErrNodeExists", err)
	}
	// 错误消息要能指出是哪台机器重名（排障用），但不能泄漏令牌/哈希
	if !strings.Contains(err.Error(), "mbp-16") {
		t.Fatalf("duplicate name err = %q, want 含冲突名字", err)
	}
	if strings.Contains(err.Error(), "ant_node_") {
		t.Fatalf("duplicate name err 泄漏了 token: %q", err)
	}

	// 失败的插入不能留下残行，也不能覆盖原来那台机器
	nodes, err := db.ListNodes(ctx, nsA)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("namespace A has %d nodes, want 1: %+v", len(nodes), nodes)
	}
	got, err := db.GetNode(ctx, nsA, first.ID)
	if err != nil {
		t.Fatalf("original node gone after rejected insert: %v", err)
	}
	if got.Name != "mbp-16" || got.TokenHash != first.TokenHash {
		t.Fatalf("original node mutated by rejected insert: %+v", got)
	}

	// 唯一性是命名空间内的：另一个命名空间同名应当成功（prod 与 staging 各有 mbp-16）
	tokenB, err := db.CreateNode(ctx, &Node{NamespaceID: nsB, Name: "mbp-16"})
	if err != nil {
		t.Fatalf("same name in another namespace rejected: %v", err)
	}
	if !strings.HasPrefix(tokenB, "ant_node_") {
		t.Fatalf("token in ns B = %q, want ant_node_ 前缀", tokenB)
	}
	byName, err := db.GetNodeByName(ctx, nsB, "mbp-16")
	if err != nil {
		t.Fatalf("get node by name in ns B: %v", err)
	}
	if byName.ID == first.ID {
		t.Fatalf("cross-namespace create returned the original node id %q", first.ID)
	}
}

// TestValidateNodeName 钉住节点名规则（契约 D6）：1..63 字符、[a-z0-9._-]、
// 必须字母或数字开头、**不 trim**（带空白即非法）。
func TestValidateNodeName(t *testing.T) {
	if NodeNameMaxLen != 63 {
		t.Fatalf("NodeNameMaxLen = %d, want 63（契约 D6）", NodeNameMaxLen)
	}
	name63 := strings.Repeat("a", NodeNameMaxLen)
	name64 := strings.Repeat("a", NodeNameMaxLen+1)

	valid := []struct {
		desc  string
		input string
	}{
		{"单个字母", "a"},
		{"单个数字", "0"},
		{"主机名", "mbp-16"},
		{"下划线与点", "demo_worker.v2"},
		{"63 字符（上限）", name63},
		{"a..b 属于合法字符集（不判点号语义）", "a..b"},
	}
	for _, c := range valid {
		if msg := ValidateNodeName(c.input); msg != "" {
			t.Errorf("%s: ValidateNodeName(%q) = %q, want 合法", c.desc, c.input, msg)
		}
	}

	invalid := []struct {
		desc  string
		input string
	}{
		{"空串", ""},
		{"全空白", "   "},
		{"大写", "UPPER"},
		{"含空格", "has space"},
		{"短横线开头", "-lead"},
		{"点开头", ".dot"},
		{"64 字符（超上限）", name64},
		{"中文", "中文节点"},
		{"前导空白（不 trim）", " mbp-16"},
		{"下划线开头", "_lead"},
	}
	for _, c := range invalid {
		msg := ValidateNodeName(c.input)
		if msg == "" {
			t.Errorf("%s: ValidateNodeName(%q) = \"\", want 非空错误（应拒绝）", c.desc, c.input)
			continue
		}
		if !strings.Contains(msg, "name") {
			t.Errorf("%s: 错误消息 %q 未点名字段 name", c.desc, msg)
		}
	}
}

// ---------- #264：粘性 pin 并进派发事务 ----------

// TestLeaseAndAssignWithPinWritesPinAtomically pin 与 assigned 同一个事务落库。
//
// 验收口径：**派发事务返回时 pin 已经在库里**（原实现要等推帧成功后再写一行），
// 并且 pin.CreatedAt 被就地填好——回滚路径正是靠它区分"本次新建"与"早先就有"。
func TestLeaseAndAssignWithPinWritesPinAtomically(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "review", 1, []string{"fix"})
	mustCreateCap(t, db, cap)
	nodeID := mkTxMergeNode(t, db, nsID, cap, "n-pin")
	task := mkTxMergeTask(t, db, nsID, "trc_pin", cap)

	pin := &NodePin{
		StickyKey:   StickyKey("trc_pin", cap.ID, "wt-a"),
		NamespaceID: nsID,
		NodeID:      nodeID,
		Selector:    "wt-a",
	}
	if _, err := db.LeaseAndAssignWithPin(ctx, nsID, task.ID, nodeID, Now()+LeaseTTLSec, pin); err != nil {
		t.Fatalf("lease and assign with pin: %v", err)
	}
	if pin.CreatedAt == 0 {
		t.Fatal("pin.CreatedAt 没有被就地填上")
	}
	if pin.PinID == "" {
		t.Fatal("pin.PinID 没有被就地填上（回滚路径要靠这个行身份做条件删除）")
	}
	got, err := db.GetPin(ctx, pin.StickyKey)
	if err != nil {
		t.Fatalf("pin 没落库（派发事务返回时它就该在库里）: %v", err)
	}
	if got.NodeID != nodeID || got.Selector != "wt-a" || got.NamespaceID != nsID {
		t.Fatalf("pin 内容不对：%+v", got)
	}
	if got.CreatedAt != pin.CreatedAt {
		t.Fatalf("库里的 created_at = %d，调用方拿到 %d", got.CreatedAt, pin.CreatedAt)
	}
	if got.PinID != pin.PinID {
		t.Fatalf("库里的 pin_id = %q，调用方拿到 %q（回滚条件会不命中）", got.PinID, pin.PinID)
	}
	// 任务本身也照常派发出去了（合并不能吃掉原语义）。
	back, err := db.GetTask(ctx, nsID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if back.Status != TaskAssigned || back.NodeID == nil || *back.NodeID != nodeID {
		t.Fatalf("任务状态/归属不对：status=%s node=%v", back.Status, back.NodeID)
	}
}

// TestLeaseAndAssignWithPinRollsBackPinOnFailure 派发失败时 pin 必须一起回滚（同一事务）。
//
// 这是「折进事务」的全部价值所在：不能出现「库里多了一条 pin，任务却没派出去」。
// 顺带钉住槽位也不泄漏（整笔回滚，不是单步补偿）。
func TestLeaseAndAssignWithPinRollsBackPinOnFailure(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "review", 1, []string{"fix"})
	mustCreateCap(t, db, cap)
	nodeID := mkTxMergeNode(t, db, nsID, cap, "n-pin2")

	// 非 pending 的任务：assignTaskTx 的守卫会拒绝（pending→assigned 是唯一合法的派发边），
	// 而它排在 pin 写入**之前**——所以这一笔必须整体回滚。
	bad := mkTxMergeTask(t, db, nsID, "trc_pin_bad", cap)
	mustExec(t, db, `UPDATE tasks SET status = 'running' WHERE id = ?`, bad.ID)

	pin := &NodePin{
		StickyKey:   StickyKey("trc_pin_bad", cap.ID, "wt-b"),
		NamespaceID: nsID,
		NodeID:      nodeID,
		Selector:    "wt-b",
	}
	if _, err := db.LeaseAndAssignWithPin(ctx, nsID, bad.ID, nodeID, Now()+LeaseTTLSec, pin); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("非 pending 任务 LeaseAndAssignWithPin err = %v, want ErrInvalidTransition", err)
	}
	if _, err := db.GetPin(ctx, pin.StickyKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("派发失败却留下了 pin（事务没回滚干净）：err=%v", err)
	}
	// 槽位也没被占住：一个全新的 pending 任务还能占到那唯一的槽。
	ok := mkTxMergeTask(t, db, nsID, "trc_pin_ok", cap)
	if _, err := db.LeaseAndAssign(ctx, nsID, ok.ID, nodeID, Now()+LeaseTTLSec); err != nil {
		t.Fatalf("失败那笔泄漏了槽位：%v", err)
	}
}

// TestRollbackPinOnlyDeletesOwnPin 回滚只删**本次新建**的那条 pin。
//
// 无脑 DeletePin 的后果是「一次推帧失败顺手清掉同一 trace 上一跳留下的钉住」，
// 于是整条 trace 的粘性静默失效。三个分支分别钉住：自己的删掉、别人的留着、
// 同 key 但不同机器的留着。判据是 pin_id（行身份）——不是秒级的 created_at。
func TestRollbackPinOnlyDeletesOwnPin(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "review", 1, []string{"fix"})
	mustCreateCap(t, db, cap)
	nodeA := mkTxMergeNode(t, db, nsID, cap, "n-a")
	nodeB := mkTxMergeNode(t, db, nsID, cap, "n-b")
	key := StickyKey("trc_rb", cap.ID, "wt-rb")

	// ① 本次新建的 pin → 删掉。
	mine := &NodePin{StickyKey: key, NamespaceID: nsID, NodeID: nodeA, Selector: "wt-rb"}
	if err := db.PutPin(ctx, mine); err != nil {
		t.Fatalf("put pin: %v", err)
	}
	deleted, err := db.RollbackPin(ctx, key, nodeA, mine.PinID)
	if err != nil {
		t.Fatalf("rollback pin: %v", err)
	}
	if !deleted {
		t.Fatal("本次新建的 pin 没被删掉")
	}
	if _, err := db.GetPin(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("回滚后 pin 仍在：%v", err)
	}

	// ② 早先留下的 pin（行身份是别人的）→ 一行都不动。
	old := &NodePin{StickyKey: key, NamespaceID: nsID, NodeID: nodeA, Selector: "wt-rb",
		CreatedAt: Now() - 1000}
	if err := db.PutPin(ctx, old); err != nil {
		t.Fatalf("put old pin: %v", err)
	}
	deleted, err = db.RollbackPin(ctx, key, nodeA, NewPinID())
	if err != nil {
		t.Fatalf("rollback old pin: %v", err)
	}
	if deleted {
		t.Fatal("回滚删掉了早先留下的 pin（整条 trace 的粘性会被误清）")
	}
	if got, err := db.GetPin(ctx, key); err != nil || got.NodeID != nodeA {
		t.Fatalf("早先的 pin 被动过：%+v err=%v", got, err)
	}

	// ③ 同 key 但机器不同（钉住不换机器：库里还是 nodeA）→ 也不动。
	deleted, err = db.RollbackPin(ctx, key, nodeB, old.PinID)
	if err != nil {
		t.Fatalf("rollback other node: %v", err)
	}
	if deleted {
		t.Fatal("回滚删掉了另一台机器的 pin")
	}
	if _, err := db.GetPin(ctx, key); err != nil {
		t.Fatalf("pin 被误删：%v", err)
	}
}

// TestRollbackPinSameSecondSameNode 同秒同 key 同节点的两次派发，回滚只删**自己**那行。
//
// 这是 #315 的验收用例。旧判据 (sticky_key, node_id, created_at) 里的 created_at
// 只有**秒级**精度，所以两次派发拿到同一个值：一旦第一次的行已经被它自己删掉、
// 而第二次在同一秒里重建了一条，第一次的身份（created_at 仍是 T）迟到再回滚时
// 就会把第二次那行删掉 —— 这个 trace 的粘性被静默清掉。
//
// 断言方式：两次派发都把 CreatedAt 钉成同一个值（真的同秒），并且断言回滚判据
// 用的是行身份而不是那个时间戳。
func TestRollbackPinSameSecondSameNode(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "review", 1, []string{"fix"})
	mustCreateCap(t, db, cap)
	nodeA := mkTxMergeNode(t, db, nsID, cap, "n-same")
	key := StickyKey("trc_same_sec", cap.ID, "wt-sec")
	const sameSecond int64 = 1700000000

	// ① 第一次派发建 pin（秒 T），随后它自己推帧失败 → 按自己的行身份删掉。
	d1 := &NodePin{StickyKey: key, NamespaceID: nsID, NodeID: nodeA, Selector: "wt-sec",
		CreatedAt: sameSecond, LastUsedAt: sameSecond}
	if err := db.PutPin(ctx, d1); err != nil {
		t.Fatalf("put pin (d1): %v", err)
	}
	if deleted, err := db.RollbackPin(ctx, key, nodeA, d1.PinID); err != nil || !deleted {
		t.Fatalf("d1 回滚自己的 pin 应命中：deleted=%v err=%v", deleted, err)
	}

	// ② 第二次派发在**同一秒 T**、同 key、同节点上重建了一条 pin。
	d2 := &NodePin{StickyKey: key, NamespaceID: nsID, NodeID: nodeA, Selector: "wt-sec",
		CreatedAt: sameSecond, LastUsedAt: sameSecond}
	if err := db.PutPin(ctx, d2); err != nil {
		t.Fatalf("put pin (d2): %v", err)
	}
	if d2.PinID == d1.PinID {
		t.Fatalf("两次新建拿到了同一个行身份 %q", d2.PinID)
	}
	stored, err := db.GetPin(ctx, key)
	if err != nil {
		t.Fatalf("get pin: %v", err)
	}
	if stored.CreatedAt != sameSecond {
		t.Fatalf("库里 created_at = %d，期望 %d（这条用例的意义就是两行同秒）", stored.CreatedAt, sameSecond)
	}

	// ③ d1 的身份（created_at 与 d2 完全一样）迟到再回滚：绝不能删 d2 那一行。
	deleted, err := db.RollbackPin(ctx, key, nodeA, d1.PinID)
	if err != nil {
		t.Fatalf("rollback (d1 late): %v", err)
	}
	if deleted {
		t.Fatal("回滚按秒级 created_at 删掉了别人那一行（#315 的 bug 又回来了）")
	}
	stored, err = db.GetPin(ctx, key)
	if err != nil {
		t.Fatalf("d2 的 pin 被误删：%v", err)
	}
	if stored.PinID != d2.PinID || stored.NodeID != nodeA {
		t.Fatalf("d2 的 pin 被改动：%+v", stored)
	}
	// 同一个三元组 (key, node, created_at) 现在确实能匹到一行—
	// 而这行是 d2 的（d1 那行已经删了）。旧判据就是靠这个值删的，所以它会误删。
	var colliding int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM node_pins WHERE sticky_key = ? AND node_id = ? AND created_at = ?`,
		key, nodeA, sameSecond).Scan(&colliding); err != nil {
		t.Fatalf("count by old predicate: %v", err)
	}
	if colliding != 1 {
		t.Fatalf("旧判据 (key, node, created_at) 命中 %d 行，期望 1——这条用例没成功构造出同秒撞车", colliding)
	}

	// ④ 对照：d2 自己的回滚仍然删得掉（判据不是“永远不删”）。
	deleted, err = db.RollbackPin(ctx, key, nodeA, d2.PinID)
	if err != nil {
		t.Fatalf("rollback (d2): %v", err)
	}
	if !deleted {
		t.Fatal("d2 回滚自己的 pin 没命中")
	}
	if _, err := db.GetPin(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("回滚后 pin 仍在：%v", err)
	}
}

// TestRollbackPinValidation 回滚必须带行身份（不能退化成“按 key 删”）。
func TestRollbackPinValidation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.RollbackPin(ctx, "k", "nd_x", ""); err == nil {
		t.Error("RollbackPin 接受了空 pin_id（会把“没有身份”变成无条件删除）")
	}
}

// TestMigration7PinIDWorksOnExistingDB 迁移 7 的升级路径：v6 的库（node_pins 没有 pin_id）
// 重开时必须把这一列补回来，且**老行拿到 ""**——它不是任何一次「本次派发」建的，
// 所以回滚永远不会误删它（正是想要的语义）。
//
// 为什么值得单独测：迁移是按版本记账、只跑一次的，全新库覆盖不到「ALTER 打在有数据的
// 老表上」这条路径（NOT NULL DEFAULT ” 在老行上到底填什么、会不会失败）。
func TestMigration7PinIDWorksOnExistingDB(t *testing.T) {
	db, path := newTestDBFile(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := mkCap(nsID, "review", 1, []string{"fix"})
	mustCreateCap(t, db, cap)
	nodeA := mkTxMergeNode(t, db, nsID, cap, "n-m7")
	key := StickyKey("trc_m7", cap.ID, "wt-m7")

	// 迁移 7 之后建的 pin（模拟「老库里的历史数据」）。
	old := &NodePin{StickyKey: key, NamespaceID: nsID, NodeID: nodeA, Selector: "wt-m7"}
	if err := db.PutPin(ctx, old); err != nil {
		t.Fatalf("put pin: %v", err)
	}

	// 退回 v6 的形状：列没了、版本记账也删掉。
	mustExec(t, db, `ALTER TABLE node_pins DROP COLUMN pin_id`)
	mustExec(t, db, `DELETE FROM schema_version WHERE version = ?`, 7) // 迁移 7 = pin_id；别用 headVersion（它随新增块漂移）
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen a v6-shaped db: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	if v, err := db2.SchemaVersion(); err != nil || v != headVersion {
		t.Fatalf("重开后 schema 版本 = %d (err=%v)，want %d", v, err, headVersion)
	}

	// 老行还在，pin_id 补成 ""（不是 NULL、也不是新生成的值）。
	got, err := db2.GetPin(ctx, key)
	if err != nil {
		t.Fatalf("get pin after migration: %v", err)
	}
	if got.PinID != "" {
		t.Fatalf("老行的 pin_id = %q，want 空（加列前建的行不属于任何一次派发）", got.PinID)
	}
	if got.NodeID != nodeA || got.Selector != "wt-m7" || got.NamespaceID != nsID {
		t.Fatalf("迁移后老 pin 内容不对：%+v", got)
	}

	// 新的派发在同一个 key 上写 pin：ON CONFLICT 不冒领老行的身份，
	// 所以它的回滚不会删掉老行（老行不是本次派发建的）。
	fresh := &NodePin{StickyKey: key, NamespaceID: nsID, NodeID: nodeA, Selector: "wt-m7"}
	if err := db2.PutPin(ctx, fresh); err != nil {
		t.Fatalf("put pin on migrated db: %v", err)
	}
	if fresh.PinID == "" {
		t.Fatal("迁移后的库上新 pin 没拿到行身份")
	}
	if deleted, err := db2.RollbackPin(ctx, key, nodeA, fresh.PinID); err != nil || deleted {
		t.Fatalf("回滚误删了升级前的老行：deleted=%v err=%v", deleted, err)
	}
	if got, err := db2.GetPin(ctx, key); err != nil || got.NodeID != nodeA {
		t.Fatalf("升级前的老行被动过：%+v err=%v", got, err)
	}

	// 对照：老行清掉之后新建的 pin，回滚仍然能删掉自己。
	if err := db2.DeletePin(ctx, key); err != nil {
		t.Fatalf("delete pin: %v", err)
	}
	own := &NodePin{StickyKey: key, NamespaceID: nsID, NodeID: nodeA, Selector: "wt-m7"}
	if err := db2.PutPin(ctx, own); err != nil {
		t.Fatalf("put own pin: %v", err)
	}
	if deleted, err := db2.RollbackPin(ctx, key, nodeA, own.PinID); err != nil || !deleted {
		t.Fatalf("迁移后的库上回滚自己的 pin 应命中：deleted=%v err=%v", deleted, err)
	}
}
