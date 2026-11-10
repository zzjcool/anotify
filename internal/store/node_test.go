package store

// 节点（node）的数据层验收 —— v4 迁移。
//
// v4 的节点是**集群级**对象（没有 namespace_id）：隔离靠「本机申报的 ns ∩ 中枢授权的 ns」，
// 可执行性靠与派发路径**同一份**判定（nodeEligibility）：
// 在线 + ns 申报∩授权 + 机器级配置已 applied + 持有该 digest + 命令齐全 + requires 值可解析。
//
// v3 的 pin（粘性路由）在 v4 整体删除：派发只经 assignment（记候选）→ grant（认证身份 +
// 真实 slot 换租约），没有「把任务钉在某台机器上」的持久事实。因此下列 v3 用例**逐条退役**
// （映射见 TEST-MAPPING.md）：
//
//	TestNodePins / TestPutPinValidation /
//	TestLeaseAndAssignWithPinWritesPinAtomically / …WithPinRollsBackPinOnFailure /
//	TestRollbackPinOnlyDeletesOwnPin / TestRollbackPinSameSecondSameNode /
//	TestRollbackPinValidation
//	  → 退役：pin 模型不存在。原子性/回滚的保护点由 execution_auth 的 grant 路径
//	    （v4test 的 result_current_binding / slot 用例）与本文件 TestNodeAuthorizationGrant
//	    接走：一次 grant 要么绑上真实 slot 并写出 lease，要么一行都不动。
//	TestMigration7PinIDWorksOnExistingDB
//	  → 退役：v4 是 baseline 重建（无 7 号 pin_id 迁移），老库升级由 migrate_test.go /
//	    schema_baseline_test.go 覆盖。
//	TestNodeCapabilityUnknownIsStale
//	  → 退役：v4 没有「节点申报了哪些能力行」这张表（node_capabilities 不再由生产写入），
//	    「未知/陈旧」由 eligibility 的 NotDeployed 表达，见 TestNodesForCapability。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// mkNode 建一台节点（tags=pool-a；状态由调用方后续改）。
func mkNode(t *testing.T, db *DB, name string) (*Node, string) {
	t.Helper()
	n := &Node{Name: name, Tags: []string{"pool-a"}}
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

	n, token := mkNode(t, db, "node-a")
	if n.ID == "" {
		t.Fatal("CreateNode 应生成 ID")
	}
	if token == "" {
		t.Fatal("CreateNode 应返回一次性的明文 token")
	}
	if n.TokenHash == "" || n.TokenHash == token {
		t.Fatalf("库里应只存 hash（hash=%q token=%q）", n.TokenHash, token)
	}
	if n.TokenHash != HashNodeToken(token) {
		t.Fatalf("token hash 与 HashNodeToken 不一致")
	}
	// 明文 token 不进任何 JSON 响应
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal node: %v", err)
	}
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), n.TokenHash) {
		t.Fatalf("节点 JSON 泄漏了 token/hash: %s", raw)
	}

	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Name != "node-a" || got.Status != NodeStatusOffline {
		t.Fatalf("新建节点应是 offline 且名字保持: %+v", got)
	}
	if got.MaxInstances != DefaultMaxInstances || got.MaxConcurrency != DefaultMaxConcurrency {
		t.Fatalf("默认预算应补齐: maxInstances=%d maxConcurrency=%d", got.MaxInstances, got.MaxConcurrency)
	}
	if got.Tags == nil || len(got.Tags) != 1 || got.Tags[0] != "pool-a" {
		t.Fatalf("tags 往返不一致: %v", got.Tags)
	}

	byName, err := db.GetNodeByName(ctx, "node-a")
	if err != nil || byName.ID != n.ID {
		t.Fatalf("GetNodeByName: %v (%+v)", err, byName)
	}
	byHash, err := db.GetNodeByTokenHash(ctx, HashNodeToken(token))
	if err != nil || byHash.ID != n.ID {
		t.Fatalf("GetNodeByTokenHash: %v (%+v)", err, byHash)
	}
	if _, err := db.GetNode(ctx, "nd_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的节点应 ErrNotFound, got %v", err)
	}
}

// TestNodeCreateDuplicateName：节点名**集群内**唯一（v4 没有 namespace 维度可躲）。
func TestNodeCreateDuplicateName(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mkNode(t, db, "dup")

	_, err := db.CreateNode(ctx, &Node{Name: "dup"})
	if !errors.Is(err, ErrNodeExists) {
		t.Fatalf("重名应 ErrNodeExists, got %v", err)
	}
	if _, err := db.CreateNode(ctx, &Node{Name: "   "}); err == nil {
		t.Fatal("空名字应被拒")
	}
}

// TestNodeNamespaceIsolation：v4 的隔离维度是「申报 ∩ 授权」——
// 只申报 beta 的节点接不到 alpha 的任务（判据与派发同一份）。
func TestNodeNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	alphaNS, _ := newTestNS(t, db)
	betaNS := newTestExtraNS(t, db, "beta")

	capA := v4Cap(t, db, alphaNS, "iso.alpha", 1)
	task := v4Task(t, db, alphaNS, capA, "tr_iso", "{}")

	// 只申报 beta 的在线节点（配置已 apply、持有 alpha 能力的正文）
	betaNode := v4Node(t, db, "beta-only", 1)
	cfg, err := db.GetNodeConfig(ctx, betaNode.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	cfg.Namespaces = []string{"beta"}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("put config: %v", err)
	}
	v4RecordApplied(t, db, betaNode.ID)
	deployApplied(t, db, alphaNS, capA.ID, capA.Digest, betaNode.ID)

	eligible, reasons, err := db.EligibleNodesForTask(ctx, task)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(eligible) != 0 {
		t.Fatalf("只申报 beta 的节点不该能跑 alpha 的任务: %+v", eligible)
	}
	if reasons[betaNode.ID] != protocol.ConditionReasonNamespaceUnauthorized {
		t.Fatalf("具体原因应为 %s, 实际 %s", protocol.ConditionReasonNamespaceUnauthorized, reasons[betaNode.ID])
	}

	// 同一个节点把 alpha 也申报上（并 apply）→ 立刻变成可派发
	cfg.Namespaces = []string{"beta", "default"}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("put config: %v", err)
	}
	v4RecordApplied(t, db, betaNode.ID)
	eligible, reasons, err = db.EligibleNodesForTask(ctx, task)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(eligible) != 1 || eligible[0].ID != betaNode.ID {
		t.Fatalf("申报+apply 后应可派发: eligible=%+v reasons=%v", eligible, reasons)
	}
	// 另一个 ns 仍然接不到 alpha 的任务（反向验证，不是只看一侧）
	capB := v4Cap(t, db, betaNS, "iso.beta", 1)
	taskB := v4Task(t, db, betaNS, capB, "tr_iso_b", "{}")
	if eligible, _, err := db.EligibleNodesForTask(ctx, taskB); err != nil {
		t.Fatalf("eligible beta: %v", err)
	} else if len(eligible) != 0 {
		t.Fatalf("申报 alpha+beta 但没部署 beta 正文的节点不该跑 beta 任务: %+v", eligible)
	}
}

// TestNodesForCapability：能力可跑性 = 在线 + 配置 applied + **持有该 digest**。
func TestNodesForCapability(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := v4Cap(t, db, nsID, "pool.task", 1)
	task := v4Task(t, db, nsID, c, "tr_pool", "{}")

	// ① 在线但没部署 → NotDeployed（不是静默候选）
	n1 := v4Node(t, db, "not-deployed", 1)
	eligible, reasons, err := db.EligibleNodesForTask(ctx, task)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(eligible) != 0 || reasons[n1.ID] != protocol.ConditionReasonNotDeployed {
		t.Fatalf("未部署应 NotDeployed: eligible=%+v reasons=%v", eligible, reasons)
	}

	// ② 部署 applied → 可派发
	deployApplied(t, db, nsID, c.ID, c.Digest, n1.ID)
	eligible, reasons, err = db.EligibleNodesForTask(ctx, task)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(eligible) != 1 || eligible[0].ID != n1.ID {
		t.Fatalf("部署 applied 后应可派发: eligible=%+v reasons=%v", eligible, reasons)
	}

	// ③ 部署了但配置没 apply → ConfigPending
	n2 := v4Node(t, db, "config-pending", 1)
	deployApplied(t, db, nsID, c.ID, c.Digest, n2.ID)
	cfg, err := db.GetNodeConfig(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	cfg.Vars = map[string]string{"POOL": "b"}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("put config: %v", err)
	}
	// 注意：PutNodeConfig 把 revision 推前，applied_revision 停在旧值
	eligible, reasons, err = db.EligibleNodesForTask(ctx, task)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(eligible) != 1 || eligible[0].ID != n1.ID {
		t.Fatalf("配置未 apply 的节点不该成为候选: eligible=%+v", eligible)
	}
	if reasons[n2.ID] != protocol.ConditionReasonConfigPending {
		t.Fatalf("具体原因应为 %s, 实际 %s", protocol.ConditionReasonConfigPending, reasons[n2.ID])
	}

	// ④ 离线节点即便部署了也不候选
	if err := db.UpdateNodeStatus(ctx, n1.ID, NodeStatusOffline); err != nil {
		t.Fatalf("update status: %v", err)
	}
	eligible, reasons, err = db.EligibleNodesForTask(ctx, task)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(eligible) != 0 {
		t.Fatalf("离线节点不该候选: %+v", eligible)
	}
	if reasons[n1.ID] != protocol.ConditionReasonNodeOffline {
		t.Fatalf("具体原因应为 %s, 实际 %s", protocol.ConditionReasonNodeOffline, reasons[n1.ID])
	}
	if got := NodeAvailabilityReason(reasons); got != protocol.ConditionReasonNodeOffline {
		t.Fatalf("汇总原因应优先报离线, 实际 %s", got)
	}
}

// TestNodesForCapabilityNamespaceIsolation：同一份正文在两台节点上，
// 只有申报该 ns 的那台进入候选（候选集不是「全集群」）。
func TestNodesForCapabilityNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	otherNS := newTestExtraNS(t, db, "other")

	c := v4Cap(t, db, nsID, "iso2.task", 1)
	task := v4Task(t, db, nsID, c, "tr_iso2", "{}")

	mine := v4Node(t, db, "mine", 1)
	deployApplied(t, db, nsID, c.ID, c.Digest, mine.ID)

	theirs := v4Node(t, db, "theirs", 1)
	cfg, err := db.GetNodeConfig(ctx, theirs.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	cfg.Namespaces = []string{"other"}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("put config: %v", err)
	}
	v4RecordApplied(t, db, theirs.ID)
	deployApplied(t, db, nsID, c.ID, c.Digest, theirs.ID)

	eligible, reasons, err := db.EligibleNodesForTask(ctx, task)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	if len(eligible) != 1 || eligible[0].ID != mine.ID {
		t.Fatalf("候选应只含申报该 ns 的节点: eligible=%+v", eligible)
	}
	if reasons[theirs.ID] != protocol.ConditionReasonNamespaceUnauthorized {
		t.Fatalf("未申报节点的原因应为 NamespaceUnauthorized, 实际 %s", reasons[theirs.ID])
	}
	_ = otherNS
}

// TestNodeCapabilitiesReplacedOnRehello：hello 上报的申报是**快照替换**（不是合并）。
func TestNodeCapabilitiesReplacedOnRehello(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n, _ := mkNode(t, db, "hello-node")

	first := NodeCapabilityReport{{Name: "review", Version: 1, Digest: "sha256:aaa"}}
	if err := db.RecordHello(ctx, n.ID, NodeFacts{Hostname: "h1"}, first, []string{"alpha"},
		map[string]string{"LOCAL_ONLY": "1"}, map[string]any{}, "0.1.0", "abc"); err != nil {
		t.Fatalf("record hello 1: %v", err)
	}
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(got.DeclaredCapabilities) != 1 || got.DeclaredCapabilities[0].Name != "review" {
		t.Fatalf("首次 hello 申报不对: %+v", got.DeclaredCapabilities)
	}
	if got.DeclaredNamespaces == nil || len(got.DeclaredNamespaces) != 1 || got.DeclaredNamespaces[0] != "alpha" {
		t.Fatalf("首次 hello 申报的 ns 不对: %v", got.DeclaredNamespaces)
	}
	if got.AgentVersion != "0.1.0" || got.AgentCommit != "abc" {
		t.Fatalf("agent 版本事实没落库: %+v", got)
	}
	if got.LastSeenAt == nil {
		t.Fatal("hello 应刷新 last_seen_at")
	}

	// 第二次 hello 只报 fixer：旧的 review 申报必须消失（快照替换）
	second := NodeCapabilityReport{{Name: "fixer", Version: 2, Digest: "sha256:bbb"}}
	if err := db.RecordHello(ctx, n.ID, NodeFacts{Hostname: "h2"}, second, []string{"beta"},
		map[string]string{"LOCAL_ONLY": "2"}, map[string]any{}, "0.2.0", "def"); err != nil {
		t.Fatalf("record hello 2: %v", err)
	}
	got, err = db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(got.DeclaredCapabilities) != 1 || got.DeclaredCapabilities[0].Name != "fixer" {
		t.Fatalf("第二次 hello 应替换申报: %+v", got.DeclaredCapabilities)
	}
	if len(got.DeclaredNamespaces) != 1 || got.DeclaredNamespaces[0] != "beta" {
		t.Fatalf("第二次 hello 应替换 ns 申报: %v", got.DeclaredNamespaces)
	}
	// 本机值快照跟着换（local_vars 是 hello 自己的事）
	cfg, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if cfg.LocalVars["LOCAL_ONLY"] != "2" {
		t.Fatalf("本机 vars 快照没更新: %+v", cfg.LocalVars)
	}
	// hello 不该改中枢侧默认值（revision 不动）
	if cfg.Revision != 0 {
		t.Fatalf("hello 不该改中枢默认配置: revision=%d", cfg.Revision)
	}

	// 不存在的节点：必须报错，不能静默丢配置
	if err := db.RecordHello(ctx, "nd_missing", NodeFacts{}, nil, nil, nil, nil, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知节点应 ErrNotFound, got %v", err)
	}
}

// TestNodeCapabilitiesNoHarnessColumns：v4 的「节点申报能力」是 hello 快照
// （nodes.declared_capabilities JSON），**不再落 node_capabilities 行**；
// 因此 v3 的「这张表只有三列、且没有 run_spec」升级成更强的一条：
// 整张表在 v4 baseline 里都不存在（中枢执行器时代的残留表不许回来）。
func TestNodeCapabilitiesNoHarnessColumns(t *testing.T) {
	db := newTestDB(t)
	if tableExists(t, db, "node_capabilities") {
		t.Fatalf("v4 baseline 不该再有 node_capabilities 表（节点申报是 hello 快照）；列=%v",
			columnNames(t, db, "node_capabilities"))
	}
	// 申报事实确实落在节点行上（不是「没地方存」）
	n, _ := mkNode(t, db, "declared-shape")
	if err := db.RecordHello(context.Background(), n.ID, NodeFacts{},
		NodeCapabilityReport{{Name: "review", Version: 1}}, []string{"default"}, nil, nil, "", ""); err != nil {
		t.Fatalf("record hello: %v", err)
	}
	got, err := db.GetNode(context.Background(), n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(got.DeclaredCapabilities) != 1 || got.DeclaredCapabilities[0].Name != "review" {
		t.Fatalf("申报应存在节点行上: %+v", got.DeclaredCapabilities)
	}
	if cols := columnNames(t, db, "nodes"); !cols["declared_capabilities"] {
		t.Fatalf("nodes 缺 declared_capabilities 列: %v", cols)
	}
}

// TestNodeMaxInstances：申报实例数超过 nodes.max_instances 硬顶 → ErrTooManyInstances。
func TestNodeMaxInstances(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	small := &Node{Name: "small", MaxInstances: 2, Status: NodeStatusOnline}
	if _, err := db.CreateNode(ctx, small); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, small.ID, []InstanceSpec{{Slot: 0}, {Slot: 1}, {Slot: 2}}); !errors.Is(err, ErrTooManyInstances) {
		t.Fatalf("超硬顶应 ErrTooManyInstances, got %v", err)
	}
	got, err := db.UpsertInstances(ctx, small.ID, []InstanceSpec{{Slot: 0}, {Slot: 1}})
	if err != nil {
		t.Fatalf("申报 2 个应通过: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 个实例, got %d", len(got))
	}
	// 重复 slot / 负数 slot 一律拒
	if _, err := db.UpsertInstances(ctx, small.ID, []InstanceSpec{{Slot: 0}, {Slot: 0}}); err == nil {
		t.Fatal("重复 slot 应被拒")
	}
	if _, err := db.UpsertInstances(ctx, small.ID, []InstanceSpec{{Slot: -1}}); err == nil {
		t.Fatal("负数 slot 应被拒")
	}
}

// TestNodeInstancesUpsertAndShrink：缩容删掉**空闲**的旧槽，保留仍在申报的槽。
func TestNodeInstancesUpsertAndShrink(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n, _ := mkNode(t, db, "scale")
	if err := db.UpdateNodeStatus(ctx, n.ID, NodeStatusOnline); err != nil {
		t.Fatalf("status: %v", err)
	}

	if _, err := db.UpsertInstances(ctx, n.ID, []InstanceSpec{{Slot: 0}, {Slot: 1}, {Slot: 2}}); err != nil {
		t.Fatalf("upsert 3: %v", err)
	}
	got, err := db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应有 3 个槽, got %d", len(got))
	}
	for _, in := range got {
		if !in.Alive || in.BusyTask != nil {
			t.Fatalf("新申报的槽应 alive 且空闲: %+v", in)
		}
	}

	// 缩到 1 个槽：slot 1/2 被删，slot 0 保留（同一行，不重建）
	if _, err := db.UpsertInstances(ctx, n.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	got, err = db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Slot != 0 || !got[0].Alive {
		t.Fatalf("缩容后应只剩 slot 0: %+v", got)
	}
}

// TestUpsertInstancesKeepsBusyTask：缩容**不许**删掉还绑着任务的槽
// （删了就等于把别人的执行现场冲掉）。
func TestUpsertInstancesKeepsBusyTask(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n, _ := mkNode(t, db, "busy-keep")
	if err := db.UpdateNodeStatus(ctx, n.ID, NodeStatusOnline); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, n.ID, []InstanceSpec{{Slot: 0}, {Slot: 1}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.LeaseSlot(ctx, n.ID, "tsk_busy", 1, "lease_busy", 1); err != nil {
		t.Fatalf("lease slot 1: %v", err)
	}

	// 只申报 slot 0：slot 1 忙 → 必须留着
	got, err := db.UpsertInstances(ctx, n.ID, []InstanceSpec{{Slot: 0}})
	if err != nil {
		t.Fatalf("upsert shrink: %v", err)
	}
	busy := 0
	for _, in := range got {
		if in.BusyTask != nil && *in.BusyTask == "tsk_busy" {
			busy++
		}
	}
	if busy != 1 {
		t.Fatalf("忙槽必须保留, got %+v", got)
	}
}

// TestLeaseInstanceAndRelease：绑定/重复绑定/精确释放（attempt 对不上不动）。
func TestLeaseInstanceAndRelease(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n, _ := mkNode(t, db, "lease-node")
	if err := db.UpdateNodeStatus(ctx, n.ID, NodeStatusOnline); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, n.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	instanceID, err := db.LeaseSlot(ctx, n.ID, "tsk_1", 1, "lease_1", 0)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if instanceID == "" {
		t.Fatal("lease 应返回 instance id")
	}
	// 已占的槽不能再绑
	if _, err := db.LeaseSlot(ctx, n.ID, "tsk_2", 1, "lease_2", 0); !errors.Is(err, ErrSlotUnavailable) {
		t.Fatalf("重复绑定应 ErrSlotUnavailable, got %v", err)
	}
	// 未申报的槽
	if _, err := db.LeaseSlot(ctx, n.ID, "tsk_3", 1, "lease_3", 9); !errors.Is(err, ErrSlotUnavailable) {
		t.Fatalf("未申报槽应 ErrSlotUnavailable, got %v", err)
	}

	// attempt 对不上的释放不动这一行（旧 attempt 的回收不能放掉新 attempt 的槽）
	if err := db.ReleaseSlot(ctx, n.ID, instanceID, "tsk_1", 2); err != nil {
		t.Fatalf("release wrong attempt: %v", err)
	}
	insts, err := db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if insts[0].BusyTask == nil || *insts[0].BusyTask != "tsk_1" {
		t.Fatalf("错 attempt 的释放不该清槽: %+v", insts[0])
	}

	// 正确释放
	if err := db.ReleaseSlot(ctx, n.ID, instanceID, "tsk_1", 1); err != nil {
		t.Fatalf("release: %v", err)
	}
	insts, err = db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if insts[0].BusyTask != nil {
		t.Fatalf("释放后槽应空闲: %+v", insts[0])
	}
	// 释放后可以再绑给别的任务
	if _, err := db.LeaseSlot(ctx, n.ID, "tsk_9", 1, "lease_9", 0); err != nil {
		t.Fatalf("释放后再绑定: %v", err)
	}
}

// TestLeaseInstanceDeadSlotIgnored：alive=0 的槽不可再绑定。
func TestLeaseInstanceDeadSlotIgnored(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n, _ := mkNode(t, db, "dead-slot")
	if err := db.UpdateNodeStatus(ctx, n.ID, NodeStatusOnline); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, n.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.MarkInstancesDead(ctx, n.ID); err != nil {
		t.Fatalf("mark dead: %v", err)
	}
	if _, err := db.LeaseSlot(ctx, n.ID, "tsk_dead", 1, "lease_dead", 0); !errors.Is(err, ErrSlotUnavailable) {
		t.Fatalf("掉线的槽应 ErrSlotUnavailable, got %v", err)
	}
	// 再申报一次会把它恢复成 alive
	if _, err := db.UpsertInstances(ctx, n.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("upsert revive: %v", err)
	}
	if _, err := db.LeaseSlot(ctx, n.ID, "tsk_alive", 1, "lease_alive", 0); err != nil {
		t.Fatalf("恢复后应可绑定: %v", err)
	}
}

// TestNodeStatusTransitions：三种状态可选，别的值一律拒；offline 会把槽位标成不可派发。
func TestNodeStatusTransitions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n, _ := mkNode(t, db, "status-node")
	if _, err := db.UpsertInstances(ctx, n.ID, []InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	for _, status := range []string{NodeStatusOnline, NodeStatusDraining, NodeStatusOffline} {
		if err := db.UpdateNodeStatus(ctx, n.ID, status); err != nil {
			t.Fatalf("status %s: %v", status, err)
		}
		got, err := db.GetNode(ctx, n.ID)
		if err != nil {
			t.Fatalf("get node: %v", err)
		}
		if got.Status != status {
			t.Fatalf("status = %s, want %s", got.Status, status)
		}
	}
	if err := db.UpdateNodeStatus(ctx, n.ID, "nope"); err == nil {
		t.Fatal("非法状态应被拒")
	}
	if err := db.UpdateNodeStatus(ctx, "nd_missing", NodeStatusOnline); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知节点应 ErrNotFound, got %v", err)
	}

	// MarkNodeOffline：连接死亡 → 槽位 alive=0（不释放 busy 账本）
	if _, err := db.LeaseSlot(ctx, n.ID, "tsk_off", 1, "lease_off", 0); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := db.MarkNodeOffline(ctx, n.ID, Now()); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status != NodeStatusOffline {
		t.Fatalf("MarkNodeOffline 应转 offline, got %s", got.Status)
	}
	insts, err := db.ListInstances(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if insts[0].Alive {
		t.Fatalf("offline 后槽位应 alive=0: %+v", insts[0])
	}
	if insts[0].BusyTask == nil {
		t.Fatalf("offline 不该释放 busy 账本: %+v", insts[0])
	}
}

// TestTouchNode：心跳只更新 last_seen_at。
func TestTouchNode(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n, _ := mkNode(t, db, "touch-node")
	if n.LastSeenAt != nil {
		t.Fatalf("新节点不该有 last_seen_at: %v", *n.LastSeenAt)
	}
	at := Now()
	if err := db.TouchNode(ctx, n.ID, at); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.LastSeenAt == nil || *got.LastSeenAt != at {
		t.Fatalf("last_seen_at = %v, want %d", got.LastSeenAt, at)
	}
	if err := db.TouchNode(ctx, "nd_missing", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知节点应 ErrNotFound, got %v", err)
	}
}

// TestNodeEmptyListIsArray：空列表返回 []（前端的「没有节点」判据靠这个）。
func TestNodeEmptyListIsArray(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	list, err := db.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if list == nil {
		t.Fatal("空节点列表必须返回 []，不能是 nil")
	}
	if len(list) != 0 {
		t.Fatalf("空库应 0 台节点, got %d", len(list))
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("空列表应序列化成 [], got %s", raw)
	}
}

// TestNodeTagsRoundTrip：tags 存什么读什么（含空列表）。
func TestNodeTagsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	n := &Node{Name: "tagged", Tags: []string{"pool-a", "gpu", "zone-1"}}
	if _, err := db.CreateNode(ctx, n); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Tags) != 3 || got.Tags[1] != "gpu" {
		t.Fatalf("tags 往返不一致: %v", got.Tags)
	}

	empty := &Node{Name: "no-tags"}
	if _, err := db.CreateNode(ctx, empty); err != nil {
		t.Fatalf("create empty tags: %v", err)
	}
	got, err = db.GetNode(ctx, empty.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Tags == nil || len(got.Tags) != 0 {
		t.Fatalf("空 tags 应是 [] 而不是 nil: %#v", got.Tags)
	}
}

// TestNewNodeTokenUnique：token 不可枚举（随机）且 hash 稳定。
func TestNewNodeTokenUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		token, hash, err := NewNodeToken()
		if err != nil {
			t.Fatalf("new token: %v", err)
		}
		if !strings.HasPrefix(token, "ant_node_") {
			t.Fatalf("token 形状应是 ant_node_<随机>，实际 %q", token)
		}
		if seen[token] {
			t.Fatalf("token 重复: %q", token)
		}
		seen[token] = true
		if hash != HashNodeToken(token) {
			t.Fatalf("hash 不可复现: %q vs %q", hash, HashNodeToken(token))
		}
		if strings.Contains(hash, token) {
			t.Fatalf("hash 里不该出现明文 token")
		}
	}
}

// TestFreeSlotsByNode：空闲槽数只数 alive 且没绑任务的槽；未知节点给 0。
func TestFreeSlotsByNode(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n, _ := mkNode(t, db, "free-slots")
	if err := db.UpdateNodeStatus(ctx, n.ID, NodeStatusOnline); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, n.ID, []InstanceSpec{{Slot: 0}, {Slot: 1}, {Slot: 2}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := db.FreeSlotsByNode(ctx, []string{n.ID, "nd_missing"})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	if got[n.ID] != 3 {
		t.Fatalf("空闲槽 = %d, want 3", got[n.ID])
	}
	if free, ok := got["nd_missing"]; !ok || free != 0 {
		t.Fatalf("未知节点应给 0, got %v (ok=%v)", free, ok)
	}

	if _, err := db.LeaseSlot(ctx, n.ID, "tsk_f", 1, "lease_f", 0); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := db.MarkInstancesDead(ctx, n.ID); err != nil {
		t.Fatalf("mark dead: %v", err)
	}
	got, err = db.FreeSlotsByNode(ctx, []string{n.ID})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	if got[n.ID] != 0 {
		t.Fatalf("掉线的槽不该算空闲, got %d", got[n.ID])
	}
}

// TestValidateNodeName：节点名校验（大小写/字符集/长度）。
func TestValidateNodeName(t *testing.T) {
	ok := []string{"a", "node-1", "node.a", "node_a", "0abc"}
	for _, name := range ok {
		if msg := ValidateNodeName(name); msg != "" {
			t.Errorf("ValidateNodeName(%q) = %q, want 合法", name, msg)
		}
	}
	bad := []string{"", "   ", "Node", "node a", "-lead", "a/b", strings.Repeat("x", NodeNameMaxLen+1)}
	for _, name := range bad {
		if msg := ValidateNodeName(name); msg == "" {
			t.Errorf("ValidateNodeName(%q) 应报错", name)
		}
	}
}

// TestNodeConfigRevisionAndAppliedFacts：节点配置是 CAS 写 + revision 只增；
// applied_* 只由节点回报写（中枢不替它说"已应用"）。
func TestNodeConfigRevisionAndAppliedFacts(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n, _ := mkNode(t, db, "cfg-node")

	cfg, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if len(cfg.Namespaces) != 1 || cfg.Namespaces[0] != NamespaceWildcard {
		t.Fatalf("缺省授权应是 [\"*\"]: %v", cfg.Namespaces)
	}
	if cfg.Revision != 0 || cfg.AppliedRevision != 0 {
		t.Fatalf("新节点配置 revision/applied 都应为 0: %+v", cfg)
	}

	// 第一次写：revision 1，resourceVersion 2
	cfg.Vars = map[string]string{"POOL": "a"}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("put config: %v", err)
	}
	if cfg.Revision != 1 {
		t.Fatalf("写入后 revision = %d, want 1", cfg.Revision)
	}
	if cfg.ResourceVersion != 2 {
		t.Fatalf("写入后 resourceVersion = %d, want 2", cfg.ResourceVersion)
	}
	if cfg.AppliedRevision != 0 {
		t.Fatalf("中枢写配置不该改 applied: %+v", cfg)
	}

	// 旧令牌再写 → 冲突
	stale := *cfg
	stale.ResourceVersion = 1
	if err := db.PutNodeConfig(ctx, &stale); !errors.Is(err, ErrResourceVersionConflict) {
		t.Fatalf("旧令牌应 ErrResourceVersionConflict, got %v", err)
	}

	// 节点回报 applied：只动 applied_*，不动 revision
	hash, err := MachineConfigHash(map[string]string{"POOL": "a"}, map[string]any{"total": 1})
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.RecordApplied(ctx, n.ID, cfg.Revision, hash); err != nil {
		t.Fatalf("record applied: %v", err)
	}
	applied, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if applied.AppliedRevision != 1 || applied.AppliedHash != hash || applied.AppliedAt == nil {
		t.Fatalf("applied 事实没落库: %+v", applied)
	}
	if applied.Revision != 1 {
		t.Fatalf("回报 applied 不该改中枢 revision: %d", applied.Revision)
	}
}

// TestNodeAuthorizationGrant：grant 只认**认证身份**（节点 id + 当前 token hash）；
// 成功后才把真实 slot 写成占用，并返回一次性的尝试令牌。
func TestNodeAuthorizationGrant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	c := v4Cap(t, db, nsID, "grant.task", 1)
	task := v4Task(t, db, nsID, c, "tr_grant", "{}")
	node := v4Node(t, db, "grant-node", 1)
	deployApplied(t, db, nsID, c.ID, c.Digest, node.ID)

	if err := db.AssignTask(ctx, task.ID, node.ID, "asg-1", 1); err != nil {
		t.Fatalf("assign: %v", err)
	}
	// 错的 token hash：身份不成立，一行业务写都不该发生
	bad := NodeAuthorization{NodeID: node.ID, NodeTokenHash: "not-the-hash"}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, bad, task.ID, "asg-1", 1, 0, Now()); err == nil {
		t.Fatal("错 token hash 的 grant 应被拒")
	}
	insts, err := db.ListInstances(ctx, node.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if insts[0].BusyTask != nil {
		t.Fatalf("被拒的 grant 不该占用 slot: %+v", insts[0])
	}

	leaseID, token, attempt, expiresAt, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID),
		task.ID, "asg-1", 1, 0, Now())
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if leaseID == "" || token == "" || attempt != 1 {
		t.Fatalf("grant 结果不完整: lease=%q token=%q attempt=%d", leaseID, token, attempt)
	}
	if expiresAt == 0 {
		t.Fatal("grant 必须给出租约到期时刻")
	}
	running := v4MustPhase(t, db, nsID, task.ID, TaskRunning)
	if running.LeaseID != leaseID || running.Attempt != 1 || running.LeaseUntil == nil {
		t.Fatalf("任务的租约事实不对: %+v", running)
	}
	insts, err = db.ListInstances(ctx, node.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if insts[0].BusyTask == nil || *insts[0].BusyTask != task.ID {
		t.Fatalf("grant 后 slot 应绑到任务: %+v", insts[0])
	}

	// 同一个 slot 再 grant 另一条任务 → slot 不可用（容量是硬事实）
	c2 := v4Cap(t, db, nsID, "grant.task2", 1)
	task2 := v4Task(t, db, nsID, c2, "tr_grant2", "{}")
	deployApplied(t, db, nsID, c2.ID, c2.Digest, node.ID)
	if err := db.AssignTask(ctx, task2.ID, node.ID, "asg-2", 1); err != nil {
		t.Fatalf("assign 2: %v", err)
	}
	if _, _, _, _, err := db.GrantLeaseAuthorized(ctx, v4Auth(t, db, node.ID),
		task2.ID, "asg-2", 1, 0, Now()); !errors.Is(err, ErrSlotUnavailable) {
		t.Fatalf("占满的 slot 应 ErrSlotUnavailable, got %v", err)
	}
}

// TestNodeHidesTokenHash 节点 JSON 绝不带 token hash（它是认证秘密，不是资源字段）。
//
// v3 的 `TestNodeHidesTokenHash` 在 v4 恢复同名语义：Node.TokenHash 是 json:"-"，
// 新建/读取两条路径都不得把它序列化出去。
func TestNodeHidesTokenHash(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	n := v4Node(t, db, "node-json-token", 0)
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.TokenHash == "" {
		t.Fatal("前置：节点行应有真实 token hash（认证事实）")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal node: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode node JSON: %v", err)
	}
	if _, ok := decoded["tokenHash"]; ok {
		t.Fatalf("节点 JSON 泄漏 tokenHash: %s", raw)
	}
	if strings.Contains(string(raw), got.TokenHash) {
		t.Fatalf("节点 JSON 里出现了 token hash 原值: %s", raw)
	}
}
