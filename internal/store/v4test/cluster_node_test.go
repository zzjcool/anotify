package v4test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 集群节点 / 授权交集 / 派发资格 / 槽位租约的外部验收（anode.md §0/§2/§6）。

// deployApplied 造一条「节点持有这个 digest」的部署事实（state=applied）。
func deployApplied(t *testing.T, db *store.DB, nsID, nodeID string, c *store.CapabilityVersion) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO deployments
		   (id, namespace_id, node_id, capability_id, digest, state, reason, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,'',?,?)`,
		store.NewDeploymentID(), nsID, nodeID, c.ID, c.Digest,
		store.DeploymentApplied, store.Now(), store.Now()); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
}

// declareHello 发一次 hello（声明命令探测结果 + 本机值/申报 ns）。
func declareHello(t *testing.T, db *store.DB, nodeID string, declaredNamespaces []string,
	caps []store.DeclaredCapability, localVars map[string]string) {
	t.Helper()
	if err := db.RecordHello(context.Background(), nodeID, protocol.NodeFacts{}, caps,
		declaredNamespaces, localVars, nil, "1.4.9", "abc"); err != nil {
		t.Fatalf("record hello for %s: %v", nodeID, err)
	}
}

// TestNodeLifecycle 创建/按 token 定位/状态/版本事实/删除。
func TestNodeLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	n := newClusterNode(t, db, "node-a")
	token, err := db.CreateNode(ctx, &store.Node{Name: "node-b"})
	if err != nil {
		t.Fatalf("create node-b: %v", err)
	}
	if token == "" {
		t.Fatal("CreateNode 必须返回一次明文 token")
	}
	if _, err := db.CreateNode(ctx, &store.Node{Name: "node-b"}); !errors.Is(err, store.ErrNodeExists) {
		t.Fatalf("重名应返回 ErrNodeExists，实际 %v", err)
	}

	byToken, err := db.GetNodeByTokenHash(ctx, store.HashNodeToken(token))
	if err != nil {
		t.Fatalf("get node by token: %v", err)
	}
	if byToken.Name != "node-b" {
		t.Fatalf("token 定位到 %q", byToken.Name)
	}
	if _, err := db.GetNodeByTokenHash(ctx, "deadbeef"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知 token 应返回 ErrNotFound，实际 %v", err)
	}
	if got, err := db.GetNodeByName(ctx, "node-a"); err != nil || got.ID != n.ID {
		t.Fatalf("GetNodeByName = %+v, %v", got, err)
	}

	if err := db.UpdateNodeStatus(ctx, n.ID, "bogus"); err == nil {
		t.Fatal("非法状态必须拒绝")
	}
	nodeOnline(t, db, n.ID)
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status != store.NodeStatusOnline || got.LastSeenAt == nil {
		t.Fatalf("status/last_seen = %q/%v", got.Status, got.LastSeenAt)
	}
	at := int64(123456)
	if err := db.TouchNode(ctx, n.ID, at); err != nil {
		t.Fatalf("touch node: %v", err)
	}
	got, err = db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.LastSeenAt == nil || *got.LastSeenAt != at {
		t.Fatalf("touch 后 lastSeenAt = %v", got.LastSeenAt)
	}

	list, err := db.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(list) != 2 || list[0].Name != "node-a" || list[1].Name != "node-b" {
		t.Fatalf("ListNodes = %v", []string{list[0].Name, list[1].Name})
	}

	if err := db.DeleteNode(ctx, n.ID); err != nil {
		t.Fatalf("delete node: %v", err)
	}
	if _, err := db.GetNode(ctx, n.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删除后 GetNode 应 ErrNotFound，实际 %v", err)
	}
	if err := db.DeleteNode(ctx, n.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound，实际 %v", err)
	}
}

// TestNodeAgentUpgrade 升级尝试的状态机（sent → failed / ok）。
func TestNodeAgentUpgrade(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	n := newClusterNode(t, db, "node-a")

	if err := db.MarkAgentUpgradeSent(ctx, n.ID, "1.5.0", 100); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	got, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentUpgrade.Status != store.AgentUpgradeSent || got.AgentUpgrade.TargetVersion != "1.5.0" {
		t.Fatalf("sent 状态 = %+v", got.AgentUpgrade)
	}
	if got.AgentVersion != "" {
		t.Fatalf("下发帧还没换二进制，版本列必须是空串：%q", got.AgentVersion)
	}

	if err := db.FailAgentUpgrade(ctx, n.ID, "need_root", 200); err != nil {
		t.Fatalf("fail upgrade: %v", err)
	}
	got, err = db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentUpgrade.Status != store.AgentUpgradeFailed || got.AgentUpgrade.Error != "need_root" ||
		got.AgentUpgrade.TargetVersion != "1.5.0" || got.AgentUpgrade.At != 200 {
		t.Fatalf("failed 状态 = %+v", got.AgentUpgrade)
	}
	if got.AgentVersion != "" {
		t.Fatalf("升级失败不该改版本列：%q", got.AgentVersion)
	}

	if err := db.MarkAgentUpgradeSent(ctx, n.ID, "1.5.0", 300); err != nil {
		t.Fatalf("mark sent again: %v", err)
	}
	if err := db.RecordAgentUpgrade(ctx, n.ID, "1.5.0", "c0ffee", 400); err != nil {
		t.Fatalf("record upgrade: %v", err)
	}
	got, err = db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.AgentVersion != "1.5.0" || got.AgentCommit != "c0ffee" ||
		got.AgentUpgrade.Status != store.AgentUpgradeOK || got.AgentUpgrade.At != 400 {
		t.Fatalf("ok 后 = version %q commit %q upgrade %+v", got.AgentVersion, got.AgentCommit, got.AgentUpgrade)
	}

	if err := db.FailAgentUpgrade(ctx, "nd_missing", "x", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知节点应 ErrNotFound，实际 %v", err)
	}
}

// TestNodeAuthorizedNamespaces 生效 ns = 申报 ∩ 授权；两边 "*" 语义；只收紧不扩张。
func TestNodeAuthorizedNamespaces(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	n := newClusterNode(t, db, "node-a")

	got, err := db.AuthorizedNamespaces(ctx, n.ID)
	if err != nil {
		t.Fatalf("authorized namespaces: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("还没 hello（申报为空）时生效列表 = %v, want []", got)
	}

	declareHello(t, db, n.ID, []string{"team-a", "team-b"}, nil, nil)
	got, err = db.AuthorizedNamespaces(ctx, n.ID)
	if err != nil {
		t.Fatalf("authorized namespaces: %v", err)
	}
	if !sameStrings(got, []string{"team-a", "team-b"}) {
		t.Fatalf("授权缺省 [*] 时生效 = %v, want 申报原样", got)
	}

	putNodeCfg(t, db, n.ID, []string{"team-b", "team-c"}, nil, nil)
	got, err = db.AuthorizedNamespaces(ctx, n.ID)
	if err != nil {
		t.Fatalf("authorized namespaces: %v", err)
	}
	if !sameStrings(got, []string{"team-b"}) {
		t.Fatalf("交集 = %v, want [team-b]", got)
	}

	// 授权只能收紧：申报缩小立即生效，不需要节点做任何事。
	declareHello(t, db, n.ID, []string{"team-a"}, nil, nil)
	got, err = db.AuthorizedNamespaces(ctx, n.ID)
	if err != nil {
		t.Fatalf("authorized namespaces: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("申报 team-a ∩ 授权 [team-b,team-c] = %v, want []", got)
	}

	declareHello(t, db, n.ID, []string{"*"}, nil, nil)
	putNodeCfg(t, db, n.ID, []string{"team-a", "team-c"}, nil, nil)
	got, err = db.AuthorizedNamespaces(ctx, n.ID)
	if err != nil {
		t.Fatalf("authorized namespaces: %v", err)
	}
	if !sameStrings(got, []string{"team-a", "team-c"}) {
		t.Fatalf("申报 * 时生效 = %v, want 授权", got)
	}

	declareHello(t, db, n.ID, []string{"*"}, nil, nil)
	putNodeCfg(t, db, n.ID, []string{"*"}, nil, nil)
	got, err = db.AuthorizedNamespaces(ctx, n.ID)
	if err != nil {
		t.Fatalf("authorized namespaces: %v", err)
	}
	if !sameStrings(got, []string{"*"}) {
		t.Fatalf("两边 * = %v, want [*]", got)
	}

	// 显式空授权 = 谁都不授权（不能悄悄变成 ["*"] 的权限扩张）。
	putNodeCfg(t, db, n.ID, []string{}, nil, nil)
	got, err = db.AuthorizedNamespaces(ctx, n.ID)
	if err != nil {
		t.Fatalf("authorized namespaces: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("显式空授权 = %v, want []", got)
	}
	cfg, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	if len(cfg.Namespaces) != 0 {
		t.Fatalf("显式空授权被改写成 %v", cfg.Namespaces)
	}

	if _, err := db.AuthorizedNamespaces(ctx, "nd_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知节点应 ErrNotFound，实际 %v", err)
	}
}

// TestEligibleNodesReasons 六类不可用原因各一例 + 命中节点 + 无色不是 error。
func TestEligibleNodesReasons(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	cap := testCap(nsID, "build", 0, nil)
	cap.Spec.Requires = artifact.Requires{
		Commands: []string{"git"},
		Values:   []string{"vars.SOURCE_ROOT", "vars.TOOLCHAIN"},
	}
	createCap(t, db, cap)

	// ① 离线：还没上线就参与匹配的资格都没有。
	offline := newClusterNode(t, db, "node-offline")

	// ② 授权不足：申报 other，任务 ns 是 main。
	unauthorized := newClusterNode(t, db, "node-unauthorized")
	nodeOnline(t, db, unauthorized.ID)
	declareHello(t, db, unauthorized.ID, []string{"other"}, nil, nil)

	// ③ 配置未 apply：中枢 revision 前进，节点回报还停在 0。
	pending := newClusterNode(t, db, "node-pending")
	nodeOnline(t, db, pending.ID)
	declareHello(t, db, pending.ID, []string{"*"}, nil, nil)
	putNodeCfg(t, db, pending.ID, []string{"*"}, map[string]string{}, nil)

	// ④ 未部署：配置已 apply，但没有 deployments 行。
	notDeployed := newClusterNode(t, db, "node-notdeployed")
	nodeOnline(t, db, notDeployed.ID)
	declareHello(t, db, notDeployed.ID, []string{"*"}, nil, nil)

	// ⑤ 缺命令：部署了，但 git 探测 found=false。
	cmdMissing := newClusterNode(t, db, "node-cmdmissing")
	nodeOnline(t, db, cmdMissing.ID)
	declareHello(t, db, cmdMissing.ID, []string{"*"}, []store.DeclaredCapability{{
		Name: "build", Version: cap.Version, Digest: cap.Digest,
		Commands: []protocol.HelloCommand{{Name: "git", Found: false}},
	}}, nil)
	deployApplied(t, db, nsID, cmdMissing.ID, cap)

	// ⑥ 缺值：命令齐了，但三层都没有 SOURCE_ROOT。
	valueMissing := newClusterNode(t, db, "node-valuemissing")
	nodeOnline(t, db, valueMissing.ID)
	declareHello(t, db, valueMissing.ID, []string{"*"}, []store.DeclaredCapability{{
		Name: "build", Version: cap.Version, Digest: cap.Digest,
		Commands: []protocol.HelloCommand{{Name: "git", Found: true, Path: "/usr/bin/git"}},
	}}, nil)
	deployApplied(t, db, nsID, valueMissing.ID, cap)

	// ⑦ 命中：本机值解析出 SOURCE_ROOT。
	eligibleLocal := newClusterNode(t, db, "node-eligible-local")
	nodeOnline(t, db, eligibleLocal.ID)
	declareHello(t, db, eligibleLocal.ID, []string{"*"}, []store.DeclaredCapability{{
		Name: "build", Version: cap.Version, Digest: cap.Digest,
		Commands: []protocol.HelloCommand{{Name: "git", Found: true, Path: "/usr/bin/git"}},
	}}, map[string]string{"SOURCE_ROOT": "/srv/local", "TOOLCHAIN": "/opt/go"})
	deployApplied(t, db, nsID, eligibleLocal.ID, cap)

	// ⑧ 命中：值来自任务 ns 的默认层（证明 ns 层参与匹配）。
	eligibleNS := newClusterNode(t, db, "node-eligible-ns")
	nodeOnline(t, db, eligibleNS.ID)
	declareHello(t, db, eligibleNS.ID, []string{"*"}, []store.DeclaredCapability{{
		Name: "build", Version: cap.Version, Digest: cap.Digest,
		Commands: []protocol.HelloCommand{{Name: "git", Found: true, Path: "/usr/bin/git"}},
	}}, nil)
	// 值跨层：SOURCE_ROOT 来自任务 ns 默认，TOOLCHAIN 来自节点默认（两层都参与匹配）。
	putNodeCfg(t, db, eligibleNS.ID, []string{"*"}, map[string]string{"TOOLCHAIN": "/opt/go"}, nil)
	if err := db.RecordApplied(ctx, eligibleNS.ID, 1, "sha256:ns-applied"); err != nil {
		t.Fatalf("record applied: %v", err)
	}
	deployApplied(t, db, nsID, eligibleNS.ID, cap)
	if _, err := db.PutNamespaceConfig(ctx, nsID, map[string]string{"SOURCE_ROOT": "/srv/ns"}, 1); err != nil {
		t.Fatalf("put namespace config: %v", err)
	}

	eligible, reasons, err := db.EligibleNodes(ctx, nsID, cap.Spec, cap.Digest)
	if err != nil {
		t.Fatalf("eligible nodes: %v", err)
	}
	wantReasons := map[string]string{
		offline.ID:       protocol.ConditionReasonNodeOffline,
		unauthorized.ID:  protocol.ConditionReasonNamespaceUnauthorized,
		pending.ID:       protocol.ConditionReasonConfigPending,
		notDeployed.ID:   protocol.ConditionReasonNotDeployed,
		cmdMissing.ID:    protocol.ConditionReasonCommandMissing,
		valueMissing.ID:  protocol.ConditionReasonValueMissing,
		eligibleLocal.ID: "",
		eligibleNS.ID:    "",
	}
	if len(reasons) != len(wantReasons) {
		t.Fatalf("reasons 覆盖 %d 台，want %d：%v", len(reasons), len(wantReasons), reasons)
	}
	for nodeID, want := range wantReasons {
		if got := reasons[nodeID]; got != want {
			t.Errorf("node %s reason = %q, want %q", nodeID, got, want)
		}
	}
	if len(eligible) != 2 || eligible[0].Name != "node-eligible-local" ||
		eligible[1].Name != "node-eligible-ns" {
		got := []string{}
		for _, n := range eligible {
			got = append(got, n.Name)
		}
		t.Fatalf("eligible = %v, want [node-eligible-local node-eligible-ns]", got)
	}

	// 无候选不是 error：空命名空间 + 没有任何节点。
	emptyNS := newNS(t, db, "empty")
	emptyCap := createCap(t, db, testCap(emptyNS, "lonely", 0, nil))
	eligible, reasons, err = db.EligibleNodes(ctx, emptyNS, emptyCap.Spec, emptyCap.Digest)
	if err != nil {
		t.Fatalf("无候选不应是 error：%v", err)
	}
	if len(eligible) != 0 {
		t.Fatalf("没有可用节点时 eligible 必须为空：%v", eligible)
	}
	// 逐节点原因仍可查（它们都不可用于这个新 ns），但没有任何一台是 ""（可用）。
	for nodeID, reason := range reasons {
		if reason == "" {
			t.Fatalf("节点 %s 在这个 ns 里不该被判定为可用", nodeID)
		}
	}
	// 集群里一个节点都没有时，汇总 reason 是 NoMatchingNode（不是 error）。
	// （这里 reasons 非空——别的 ns 有节点——所以先单独验空输入。）
	if got := store.NodeAvailabilityReason(nil); got != protocol.ConditionReasonNoMatchingNode {
		t.Fatalf("无节点时汇总 reason = %q, want NoMatchingNode", got)
	}
	if got := store.NodeAvailabilityReason(reasons); got == "" {
		t.Fatalf("这个新 ns 没有可用节点，汇总 reason 不能是空（Valid）")
	}
	for _, tc := range []struct{ reason, want string }{
		{protocol.ConditionReasonNodeOffline, protocol.ConditionReasonNodeOffline},
		{protocol.ConditionReasonNamespaceUnauthorized, protocol.ConditionReasonNamespaceUnauthorized},
		{protocol.ConditionReasonConfigPending, protocol.ConditionReasonConfigPending},
		{protocol.ConditionReasonNotDeployed, protocol.ConditionReasonNotDeployed},
		{protocol.ConditionReasonCommandMissing, protocol.ConditionReasonCommandMissing},
		{protocol.ConditionReasonValueMissing, protocol.ConditionReasonValueMissing},
	} {
		got := store.NodeAvailabilityReason(map[string]string{"n1": tc.reason})
		if got != tc.want {
			t.Errorf("单节点 reason %q 汇总 = %q, want %q", tc.reason, got, tc.want)
		}
	}
	// 不是 NodesAvailable 封闭集合里的值：不伪造具体原因，回落到 NoMatchingNode。
	if got := store.NodeAvailabilityReason(map[string]string{"n1": "NotAReason"}); got != protocol.ConditionReasonNoMatchingNode {
		t.Errorf("未知 reason 汇总 = %q, want NoMatchingNode", got)
	}
	// 混合优先级：离线压住缺值。
	if got := store.NodeAvailabilityReason(map[string]string{
		"n1": protocol.ConditionReasonValueMissing,
		"n2": protocol.ConditionReasonNodeOffline,
	}); got != protocol.ConditionReasonNodeOffline {
		t.Fatalf("混合原因汇总 = %q, want NodeOffline", got)
	}
	// 有可用节点时汇总为 ""（Valid）。
	if got := store.NodeAvailabilityReason(map[string]string{
		"n1": protocol.ConditionReasonValueMissing,
		"n2": "",
	}); got != "" {
		t.Fatalf("有可用节点时汇总 = %q, want \"\"", got)
	}

	// digest 不属于本 ns → ErrNotFound（调用方传错）。
	if _, _, err := db.EligibleNodes(ctx, emptyNS, cap.Spec, cap.Digest); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("跨 ns digest 应 ErrNotFound，实际 %v", err)
	}
}

// TestNodeInstancesAndLeases 槽位申报 / 空闲统计 / CAS 占槽 / 释放 / 巡检兜底。
func TestNodeInstancesAndLeases(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")
	cap := createCap(t, db, testCap(nsID, "build", 0, nil))
	n := newClusterNode(t, db, "node-a")

	insts, err := db.UpsertInstances(ctx, n.ID, []store.InstanceSpec{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 2}})
	if err != nil {
		t.Fatalf("upsert instances: %v", err)
	}
	if len(insts) != 2 || insts[0].Slot != 0 || insts[1].Concurrency != 2 {
		t.Fatalf("instances = %+v", insts)
	}
	free, err := db.FreeSlotsByNode(ctx, []string{n.ID})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	if free[n.ID] != 2 {
		t.Fatalf("free = %d, want 2", free[n.ID])
	}

	instanceID, err := db.LeaseSlot(ctx, n.ID, "tsk_1")
	if err != nil {
		t.Fatalf("lease slot: %v", err)
	}
	if instanceID == "" {
		t.Fatal("LeaseSlot 必须返回 instance id")
	}
	free, err = db.FreeSlotsByNode(ctx, []string{n.ID})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	if free[n.ID] != 1 {
		t.Fatalf("占槽后 free = %d, want 1", free[n.ID])
	}
	if err := db.ReleaseSlotByTask(ctx, n.ID, "tsk_1"); err != nil {
		t.Fatalf("release slot: %v", err)
	}
	free, err = db.FreeSlotsByNode(ctx, []string{n.ID})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	if free[n.ID] != 2 {
		t.Fatalf("释放后 free = %d, want 2", free[n.ID])
	}

	// 缩容：只申报 slot 0 时多余槽位删除。
	if _, err := db.UpsertInstances(ctx, n.ID, []store.InstanceSpec{{Slot: 0, Concurrency: 1}}); err != nil {
		t.Fatalf("upsert narrower: %v", err)
	}
	free, err = db.FreeSlotsByNode(ctx, []string{n.ID})
	if err != nil {
		t.Fatalf("free slots: %v", err)
	}
	if free[n.ID] != 1 {
		t.Fatalf("缩容后 free = %d, want 1", free[n.ID])
	}

	// 槽位申报上限。
	small := &store.Node{Name: "node-small", MaxInstances: 1}
	if _, err := db.CreateNode(ctx, small); err != nil {
		t.Fatalf("create small node: %v", err)
	}
	if _, err := db.UpsertInstances(ctx, small.ID, []store.InstanceSpec{{Slot: 0}, {Slot: 1}}); !errors.Is(err, store.ErrTooManyInstances) {
		t.Fatalf("超硬顶应 ErrTooManyInstances，实际 %v", err)
	}

	// 掉线后没有可租槽位。
	if err := db.MarkInstancesDead(ctx, n.ID); err != nil {
		t.Fatalf("mark dead: %v", err)
	}
	if _, err := db.LeaseSlot(ctx, n.ID, "tsk_2"); !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("槽位全掉线应 ErrNoCapacity，实际 %v", err)
	}

	// 巡检兜底：running 任务占着的槽位不动；终态后清掉。
	other := newClusterNode(t, db, "node-b")
	if _, err := db.UpsertInstances(ctx, other.ID, []store.InstanceSpec{{Slot: 0}}); err != nil {
		t.Fatalf("upsert other: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (id, namespace_id, trace_id, capability_id, capability, version, phase, node_id, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		"tsk_running", nsID, "trace-1", cap.ID, cap.Name, cap.Version,
		string(protocol.TaskPhaseRunning), other.ID, store.Now()); err != nil {
		t.Fatalf("insert running task: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE node_instances SET busy_task = ? WHERE node_id = ?`, "tsk_running", other.ID); err != nil {
		t.Fatalf("pin slot: %v", err)
	}
	n1, err := db.ReclaimStaleSlots(ctx, other.ID, 0)
	if err != nil {
		t.Fatalf("reclaim stale slots: %v", err)
	}
	if n1 != 0 {
		t.Fatalf("running 任务的槽位不能清（清了会双跑）：清了 %d 个", n1)
	}
	if _, err := db.Exec(
		`UPDATE tasks SET phase = ? WHERE id = ?`, string(protocol.TaskPhaseSucceeded), "tsk_running"); err != nil {
		t.Fatalf("finish task: %v", err)
	}
	n1, err = db.ReclaimStaleSlots(ctx, other.ID, 0)
	if err != nil {
		t.Fatalf("reclaim stale slots: %v", err)
	}
	if n1 != 1 {
		t.Fatalf("终态任务的槽位应被清掉：清了 %d 个", n1)
	}
}

// sameStrings 比较两个字符串列表（顺序敏感）。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNodeRehelloReplacesDeclaredCapabilities 验二次 hello **覆写**（不是合并）声明事实：
// 能力探测、申报 ns、本机 vars 快照都以后一次为准，旧条目不能残留。
func TestNodeRehelloReplacesDeclaredCapabilities(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	n := newClusterNode(t, db, "node-a")
	nodeOnline(t, db, n.ID)

	declareHello(t, db, n.ID, []string{"team-a"}, []store.DeclaredCapability{{
		Name: "build", Version: 1, Digest: "sha256:one",
		Commands: []protocol.HelloCommand{{Name: "git", Found: true, Path: "/usr/bin/git"}},
	}}, map[string]string{"OLD_VAR": "1"})
	first, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(first.DeclaredCapabilities) != 1 || first.DeclaredCapabilities[0].Name != "build" {
		t.Fatalf("首次 hello 的能力声明 = %+v", first.DeclaredCapabilities)
	}
	cfg, err := db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	if cfg.LocalVars["OLD_VAR"] != "1" {
		t.Fatalf("首次 hello 的本机值 = %+v", cfg.LocalVars)
	}

	// 二次 hello：完全换一套（旧能力/旧 ns/旧本机值都不该残留）。
	declareHello(t, db, n.ID, []string{"team-b"}, []store.DeclaredCapability{{
		Name: "deploy", Version: 2, Digest: "sha256:two",
		Commands: []protocol.HelloCommand{{Name: "kubectl", Found: false}},
	}}, map[string]string{"NEW_VAR": "2"})
	second, err := db.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node after rehello: %v", err)
	}
	if len(second.DeclaredCapabilities) != 1 || second.DeclaredCapabilities[0].Name != "deploy" ||
		second.DeclaredCapabilities[0].Version != 2 {
		t.Fatalf("二次 hello 必须整体替换能力声明，得到 %+v", second.DeclaredCapabilities)
	}
	if len(second.DeclaredNamespaces) != 1 || second.DeclaredNamespaces[0] != "team-b" {
		t.Fatalf("二次 hello 必须替换申报 ns，得到 %v", second.DeclaredNamespaces)
	}
	cfg, err = db.GetNodeConfig(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node config after rehello: %v", err)
	}
	if _, stale := cfg.LocalVars["OLD_VAR"]; stale {
		t.Fatalf("旧本机值不该残留：%+v", cfg.LocalVars)
	}
	if cfg.LocalVars["NEW_VAR"] != "2" {
		t.Fatalf("新本机值没写进去：%+v", cfg.LocalVars)
	}
}

// TestOrphanPendingTasksUseEligibilityReasons 验孤儿视图用**与派发同一份**资格判定：
// 值缺 / 配置未 apply / 授权不足 / 节点离线都能被看见，且原因取已批准词表。
func TestOrphanPendingTasksUseEligibilityReasons(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "orphan-ns")
	cap := testCap(nsID, "needs-value", 1, nil)
	cap.Spec.Requires = artifact.Requires{Values: []string{"vars.SOURCE_ROOT"}}
	createCap(t, db, cap)

	node := newClusterNode(t, db, "node-a")
	nodeOnline(t, db, node.ID)
	declareHello(t, db, node.ID, []string{"*"}, nil, nil)
	deployApplied(t, db, nsID, node.ID, cap)

	// 任务 1：接受时 ns 还没有 SOURCE_ROOT（钉住的是空快照）。
	old := newTask(t, db, nsID, cap, "trace-old", "")
	// 之后给 ns 默认值：只影响**下一个**任务（任务 1 的输入已钉住）。
	setNamespaceVars(t, db, nsID, map[string]string{"SOURCE_ROOT": "/srv/ns"})
	fresh := newTask(t, db, nsID, cap, "trace-fresh", "")

	now := store.Now()
	if _, err := db.Exec(`UPDATE tasks SET created_at = ? WHERE id IN (?,?)`, now-7200, old.ID, fresh.ID); err != nil {
		t.Fatalf("age tasks: %v", err)
	}

	findOrphan := func(tasks []store.OrphanPendingTask, id string) *store.OrphanPendingTask {
		for i := range tasks {
			if tasks[i].ID == id {
				return &tasks[i]
			}
		}
		return nil
	}

	// ① 值缺：节点其它都合格，但任务钉住的 ns 快照里没有 SOURCE_ROOT。
	orphans, err := db.ListOrphanPendingTasks(ctx, nsID, 3600, 200)
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	got := findOrphan(orphans, old.ID)
	if got == nil {
		t.Fatalf("缺值的 pending 任务应被列为孤儿：%+v", orphans)
	}
	if got.Reason != store.OrphanNoOnlineNode || got.NodeReason != protocol.ConditionReasonValueMissing {
		t.Fatalf("孤儿原因 = %q/%q，want no_online_node/ValueMissing", got.Reason, got.NodeReason)
	}
	if findOrphan(orphans, fresh.ID) != nil {
		t.Fatalf("钉住快照里有值的任务不该是孤儿（新任务用的是 B 版输入）")
	}

	// ② 授权收紧（优先级高于配置未 apply）。
	putNodeCfg(t, db, node.ID, []string{"other"}, nil, nil)
	orphans, err = db.ListOrphanPendingTasks(ctx, nsID, 3600, 200)
	if err != nil {
		t.Fatalf("list orphans after unauthorize: %v", err)
	}
	for _, id := range []string{old.ID, fresh.ID} {
		item := findOrphan(orphans, id)
		if item == nil || item.NodeReason != protocol.ConditionReasonNamespaceUnauthorized {
			t.Fatalf("授权不足应报 NamespaceUnauthorized，得到 %+v", item)
		}
	}

	// ③ 恢复授权但配置 revision 前进、节点还没 apply。
	putNodeCfg(t, db, node.ID, []string{"*"}, nil, nil)
	orphans, err = db.ListOrphanPendingTasks(ctx, nsID, 3600, 200)
	if err != nil {
		t.Fatalf("list orphans after config bump: %v", err)
	}
	if item := findOrphan(orphans, old.ID); item == nil || item.NodeReason != protocol.ConditionReasonConfigPending {
		t.Fatalf("配置未 apply 应报 ConfigPending，得到 %+v", item)
	}

	// ④ 节点回报 applied → 回到「只有任务 1 缺值」。
	cfg, err := db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	if err := db.RecordApplied(ctx, node.ID, cfg.Revision, "sha256:orphan-test"); err != nil {
		t.Fatalf("record applied: %v", err)
	}
	orphans, err = db.ListOrphanPendingTasks(ctx, nsID, 3600, 200)
	if err != nil {
		t.Fatalf("list orphans after applied: %v", err)
	}
	if item := findOrphan(orphans, old.ID); item == nil || item.NodeReason != protocol.ConditionReasonValueMissing {
		t.Fatalf("apply 之后应回到 ValueMissing，得到 %+v", item)
	}
	if findOrphan(orphans, fresh.ID) != nil {
		t.Fatalf("任务 2 应可派发，不该是孤儿")
	}

	// ⑤ 节点离线（最高优先级）。
	if err := db.UpdateNodeStatus(ctx, node.ID, store.NodeStatusOffline); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	orphans, err = db.ListOrphanPendingTasks(ctx, nsID, 3600, 200)
	if err != nil {
		t.Fatalf("list orphans offline: %v", err)
	}
	if item := findOrphan(orphans, old.ID); item == nil || item.NodeReason != protocol.ConditionReasonNodeOffline {
		t.Fatalf("节点离线应报 NodeOffline，得到 %+v", item)
	}

	// ⑥ 安全阀：没到 olderThanSec 的 pending 不当孤儿（刚建出来可能只是还没轮到）。
	if err := db.UpdateNodeStatus(ctx, node.ID, store.NodeStatusOnline); err != nil {
		t.Fatalf("mark online: %v", err)
	}
	tooNew := newTask(t, db, nsID, cap, "trace-new", "")
	orphans, err = db.ListOrphanPendingTasks(ctx, nsID, 3600, 200)
	if err != nil {
		t.Fatalf("list orphans (safety valve): %v", err)
	}
	if findOrphan(orphans, tooNew.ID) != nil {
		t.Fatalf("安全阀内的新任务不该被列为孤儿")
	}
}
