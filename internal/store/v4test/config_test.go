package v4test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
)

// 三层配置 / 双 revision / CAS 的外部验收（config.md §2/§3/§4/§5）。
//
// 只用导出 API：Get*/Put*/Effective*/Resolve*/Record*，不读私有字段、不碰 SQL。

// newClusterNode 建一台集群级节点（名唯一），返回节点行。
func newClusterNode(t *testing.T, db *store.DB, name string) *store.Node {
	t.Helper()
	n := &store.Node{Name: name}
	if _, err := db.CreateNode(context.Background(), n); err != nil {
		t.Fatalf("create node %q: %v", name, err)
	}
	return n
}

// nodeOnline 把节点置为在线（可被派发的前提之一）。
func nodeOnline(t *testing.T, db *store.DB, nodeID string) {
	t.Helper()
	if err := db.UpdateNodeStatus(context.Background(), nodeID, store.NodeStatusOnline); err != nil {
		t.Fatalf("online node %s: %v", nodeID, err)
	}
}

// putNodeCfg 是 PutNodeConfig 的测试便捷封装（先取默认 resourceVersion）。
func putNodeCfg(t *testing.T, db *store.DB, nodeID string, namespaces []string, vars map[string]string, limits map[string]any) *store.NodeConfig {
	t.Helper()
	cfg, err := db.GetNodeConfig(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	cfg.Namespaces = namespaces
	cfg.Vars = vars
	cfg.Limits = limits
	if err := db.PutNodeConfig(context.Background(), cfg); err != nil {
		t.Fatalf("put node config: %v", err)
	}
	return cfg
}

// limitInt 从 limits map 里取一个整数叶子（JSON 数字形态随实现可选）。
func limitInt(t *testing.T, m map[string]any, path ...string) int {
	t.Helper()
	var cur any = m
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("limits 路径 %v: %q 不是对象（实际 %T）", path, key, cur)
		}
		cur, ok = obj[key]
		if !ok {
			t.Fatalf("limits 路径 %v: 缺 %q（现有 %v）", path, key, obj)
		}
	}
	n, err := strconv.Atoi(fmt.Sprint(cur))
	if err != nil {
		t.Fatalf("limits 路径 %v 不是整数：%v", path, cur)
	}
	return n
}

// TestConfigThreeLayerVars 逐键 local > node > namespace；缺失不注入；来源可见；ns 隔离。
func TestConfigThreeLayerVars(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsA := newNS(t, db, "team-a")
	nsB := newNS(t, db, "team-b")
	node := newClusterNode(t, db, "node-a")

	if err := db.RecordHello(ctx, node.ID, protocol.NodeFacts{Hostname: "h1"},
		nil, []string{"*"},
		map[string]string{
			"SOURCE_ROOT": "/srv/local",
			"SHARED":      "local",
			"LOCAL_ONLY":  "l",
		},
		nil, "1.4.9", "abc123"); err != nil {
		t.Fatalf("record hello: %v", err)
	}
	putNodeCfg(t, db, node.ID, []string{"*"},
		map[string]string{
			"SOURCE_ROOT":   "/srv/node",
			"SHARED":        "node",
			"NODE_ONLY":     "n",
			"NODE_BEATS_NS": "node",
		},
		nil)
	if _, err := db.PutNamespaceConfig(ctx, nsA, map[string]string{
		"SOURCE_ROOT":   "/srv/ns-a",
		"SHARED":        "ns",
		"NS_ONLY":       "a",
		"NODE_BEATS_NS": "ns",
	}, 1); err != nil {
		t.Fatalf("put ns-a config: %v", err)
	}
	if _, err := db.PutNamespaceConfig(ctx, nsB, map[string]string{
		"SOURCE_ROOT": "/srv/ns-b",
		"NS_ONLY":     "b",
	}, 1); err != nil {
		t.Fatalf("put ns-b config: %v", err)
	}

	got, err := db.EffectiveVars(ctx, node.ID, nsA, []string{
		"SOURCE_ROOT", "SHARED", "LOCAL_ONLY", "NODE_ONLY", "NS_ONLY", "NODE_BEATS_NS", "MISSING",
	})
	if err != nil {
		t.Fatalf("effective vars: %v", err)
	}
	want := map[string]store.EffectiveVar{
		"SOURCE_ROOT":   {Value: "/srv/local", Source: store.VarSourceLocal},
		"SHARED":        {Value: "local", Source: store.VarSourceLocal},
		"LOCAL_ONLY":    {Value: "l", Source: store.VarSourceLocal},
		"NODE_ONLY":     {Value: "n", Source: store.VarSourceNode},
		"NS_ONLY":       {Value: "a", Source: store.VarSourceNamespace},
		"NODE_BEATS_NS": {Value: "node", Source: store.VarSourceNode},
	}
	for name, w := range want {
		if g, ok := got[name]; !ok || g != w {
			t.Errorf("%s = %+v（存在=%v），want %+v", name, g, ok, w)
		}
	}
	if _, injected := got["MISSING"]; injected {
		t.Errorf("缺失的名字不应出现（不注入空串）：MISSING=%+v", got["MISSING"])
	}

	// ns 上下文隔离：B 的 NS_ONLY 是 b，A 的 a 绝不出现在 B 的任务里。
	gotB, err := db.EffectiveVars(ctx, node.ID, nsB, []string{"NS_ONLY", "SOURCE_ROOT"})
	if err != nil {
		t.Fatalf("effective vars for ns-b: %v", err)
	}
	if gotB["NS_ONLY"].Value != "b" || gotB["NS_ONLY"].Source != store.VarSourceNamespace {
		t.Fatalf("ns-b NS_ONLY = %+v, want b/namespace", gotB["NS_ONLY"])
	}
	if gotB["SOURCE_ROOT"].Value != "/srv/local" {
		t.Fatalf("ns-b SOURCE_ROOT = %+v（本机值应压住 ns 默认）", gotB["SOURCE_ROOT"])
	}
}

// TestConfigResolveRequiredValues 只解析 vars.*；status.*/metadata.* 不参与；缺失用原字符串上报。
func TestConfigResolveRequiredValues(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")
	node := newClusterNode(t, db, "node-a")
	if err := db.RecordHello(ctx, node.ID, protocol.NodeFacts{}, nil, []string{"*"},
		map[string]string{"SOURCE_ROOT": "/srv"}, nil, "", ""); err != nil {
		t.Fatalf("record hello: %v", err)
	}

	ok, missing, err := db.ResolveRequiredValues(ctx, node.ID, nsID,
		[]string{"vars.SOURCE_ROOT", "vars.TOOLCHAIN", "status.hostIP", "metadata.name"})
	if err != nil {
		t.Fatalf("resolve required values: %v", err)
	}
	if ok {
		t.Fatal("缺 vars.TOOLCHAIN 时 ok 必须为 false")
	}
	if len(missing) != 1 || missing[0] != "vars.TOOLCHAIN" {
		t.Fatalf("missing = %v, want [vars.TOOLCHAIN]", missing)
	}

	ok, missing, err = db.ResolveRequiredValues(ctx, node.ID, nsID,
		[]string{"vars.SOURCE_ROOT", "status.hostIP"})
	if err != nil {
		t.Fatalf("resolve required values: %v", err)
	}
	if !ok || len(missing) != 0 {
		t.Fatalf("ok=%v missing=%v，want true/[]（内置事实不参与）", ok, missing)
	}
}

// TestConfigEffectiveLimits 两层 limits 递归逐叶合并（local 覆盖 node、未写继承）。
func TestConfigEffectiveLimits(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	node := newClusterNode(t, db, "node-a")
	if err := db.RecordHello(ctx, node.ID, protocol.NodeFacts{}, nil, []string{"*"}, nil,
		map[string]any{
			"total": 8,
			"perCapability": map[string]any{
				"fixer":    2,
				"reviewer": 5,
			},
		}, "", ""); err != nil {
		t.Fatalf("record hello: %v", err)
	}
	putNodeCfg(t, db, node.ID, []string{"*"}, nil, map[string]any{
		"total": 4,
		"perCapability": map[string]any{
			"fixer":  1,
			"review": 3,
			"*":      2,
		},
	})

	limits, err := db.EffectiveLimits(ctx, node.ID)
	if err != nil {
		t.Fatalf("effective limits: %v", err)
	}
	if got := limitInt(t, limits, "total"); got != 8 {
		t.Errorf("total = %d, want 8（本机覆盖节点默认）", got)
	}
	if got := limitInt(t, limits, "perCapability", "fixer"); got != 2 {
		t.Errorf("perCapability.fixer = %d, want 2", got)
	}
	if got := limitInt(t, limits, "perCapability", "reviewer"); got != 5 {
		t.Errorf("perCapability.reviewer = %d, want 5（本机独有键保留）", got)
	}
	if got := limitInt(t, limits, "perCapability", "review"); got != 3 {
		t.Errorf("perCapability.review = %d, want 3（未写继承节点默认）", got)
	}
	if got := limitInt(t, limits, "perCapability", "*"); got != 2 {
		t.Errorf("perCapability.* = %d, want 2（未写继承节点默认）", got)
	}
}

// TestConfigNamespaceCAS 命名空间配置：缺 rv 400、旧 rv 409、成功 bump 两个版本、值校验。
func TestConfigNamespaceCAS(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")

	cfg, err := db.GetNamespaceConfig(ctx, nsID)
	if err != nil {
		t.Fatalf("get default namespace config: %v", err)
	}
	if cfg.Revision != 0 || cfg.ResourceVersion != 1 || len(cfg.Vars) != 0 {
		t.Fatalf("空默认 = %+v, want revision 0 / rv 1 / vars {}", cfg)
	}

	if _, err := db.PutNamespaceConfig(ctx, nsID, map[string]string{"A": "1"}, 0); !errors.Is(err, store.ErrResourceVersionRequired) {
		t.Fatalf("缺 rv 应返回 ErrResourceVersionRequired，实际 %v", err)
	}
	cfg, err = db.PutNamespaceConfig(ctx, nsID, map[string]string{"A": "1"}, 1)
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	if cfg.Revision != 1 || cfg.ResourceVersion != 2 {
		t.Fatalf("first put = revision %d rv %d, want 1/2", cfg.Revision, cfg.ResourceVersion)
	}
	if _, err := db.PutNamespaceConfig(ctx, nsID, map[string]string{"A": "2"}, 1); !errors.Is(err, store.ErrResourceVersionConflict) {
		t.Fatalf("旧 rv 应返回 ErrResourceVersionConflict，实际 %v", err)
	}
	cfg, err = db.PutNamespaceConfig(ctx, nsID, map[string]string{"A": "2", "B": "3"}, 2)
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if cfg.Revision != 2 || cfg.ResourceVersion != 3 {
		t.Fatalf("second put = revision %d rv %d, want 2/3", cfg.Revision, cfg.ResourceVersion)
	}
	got, err := db.GetNamespaceConfig(ctx, nsID)
	if err != nil {
		t.Fatalf("get after put: %v", err)
	}
	if got.Revision != 2 || got.ResourceVersion != 3 || got.Vars["A"] != "2" || got.Vars["B"] != "3" {
		t.Fatalf("读回 = %+v", got)
	}

	// 值校验（config.md §3.1）。
	tooMany := map[string]string{}
	for i := 0; i < 65; i++ {
		tooMany[fmt.Sprintf("K%d", i)] = "1"
	}
	badVars := []map[string]string{
		{"lower": "x"},                   // 键形状
		{"A": ""},                        // 空值
		{"A": "x\x00y"},                  // NUL
		{"A": strings.Repeat("x", 4097)}, // 超 4096 字节
		tooMany,                          // 65 项（上限 64）
	}
	for i, vars := range badVars {
		if _, err := db.PutNamespaceConfig(ctx, nsID, vars, 3); !errors.Is(err, store.ErrConfigInvalid) {
			t.Errorf("坏值 #%d 应返回 ErrConfigInvalid，实际 %v", i, err)
		}
	}
	if _, err := db.PutNamespaceConfig(ctx, "ns_missing", map[string]string{}, 1); !errors.Is(err, store.ErrNamespaceNotFound) {
		t.Fatalf("未知命名空间应返回 ErrNamespaceNotFound，实际 %v", err)
	}
}

// TestConfigNodeCASAndApplied 节点配置：CAS + 双 revision 不混用 + applied 回报。
func TestConfigNodeCASAndApplied(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	nsID := newNS(t, db, "main")
	node := newClusterNode(t, db, "node-a")

	cfg, err := db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get default node config: %v", err)
	}
	if len(cfg.Namespaces) != 1 || cfg.Namespaces[0] != "*" ||
		cfg.Revision != 0 || cfg.ResourceVersion != 1 || len(cfg.Vars) != 0 {
		t.Fatalf("空默认 = %+v, want namespaces [*] / revision 0 / rv 1", cfg)
	}
	if cfg.AppliedRevision != 0 || cfg.AppliedHash != "" || cfg.AppliedAt != nil {
		t.Fatalf("applied 默认 = %+v", cfg)
	}

	if err := db.PutNodeConfig(ctx, &store.NodeConfig{NodeID: node.ID, Vars: map[string]string{"A": "1"}}); !errors.Is(err, store.ErrResourceVersionRequired) {
		t.Fatalf("缺 rv 应返回 ErrResourceVersionRequired，实际 %v", err)
	}
	cfg.Namespaces = []string{"*"}
	cfg.Vars = map[string]string{"A": "1"}
	cfg.Limits = map[string]any{"total": 4}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if cfg.Revision != 1 || cfg.ResourceVersion != 2 {
		t.Fatalf("first put = revision %d rv %d, want 1/2", cfg.Revision, cfg.ResourceVersion)
	}
	stale := *cfg
	stale.ResourceVersion = 1
	if err := db.PutNodeConfig(ctx, &stale); !errors.Is(err, store.ErrResourceVersionConflict) {
		t.Fatalf("旧 rv 应返回 ErrResourceVersionConflict，实际 %v", err)
	}

	if err := db.RecordApplied(ctx, node.ID, 1, "sha256:applied"); err != nil {
		t.Fatalf("record applied: %v", err)
	}
	got, err := db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get after applied: %v", err)
	}
	if got.AppliedRevision != 1 || got.AppliedHash != "sha256:applied" || got.AppliedAt == nil {
		t.Fatalf("applied 回报 = revision %d hash %q at %v", got.AppliedRevision, got.AppliedHash, got.AppliedAt)
	}
	if got.Revision != 1 {
		t.Fatalf("applied 回报改写了中枢 revision：%d", got.Revision)
	}

	// 中枢再改一次默认值：revision 前进、applied 停在旧值（= ConfigPending 的判定输入）。
	cfg.Vars = map[string]string{"A": "2"}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err = db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get after second put: %v", err)
	}
	if got.Revision != 2 || got.AppliedRevision != 1 {
		t.Fatalf("configRevision/appliedRevision = %d/%d, want 2/1（不能压成一个版本）", got.Revision, got.AppliedRevision)
	}

	// 双 revision 不混用：命名空间配置的 revision 是另一个计数器。
	nsCfg, err := db.PutNamespaceConfig(ctx, nsID, map[string]string{"NS": "1"}, 1)
	if err != nil {
		t.Fatalf("put namespace config: %v", err)
	}
	if nsCfg.Revision != 1 {
		t.Fatalf("namespaceVarsRevision = %d, want 1（不受节点 revision 影响）", nsCfg.Revision)
	}
	nodeCfg, err := db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	if nodeCfg.Revision != 2 {
		t.Fatalf("node configRevision 被 ns revision 污染：%d", nodeCfg.Revision)
	}

	if err := db.RecordApplied(ctx, node.ID, 2, ""); err != nil {
		t.Fatalf("record applied 2: %v", err)
	}
	nodeCfg, err = db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	if nodeCfg.AppliedRevision != 2 || nodeCfg.Revision != 2 {
		t.Fatalf("applied 追平后 = %d/%d, want 2/2", nodeCfg.AppliedRevision, nodeCfg.Revision)
	}

	if err := db.RecordApplied(ctx, "nd_missing", 1, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知节点应返回 ErrNotFound，实际 %v", err)
	}
}

// TestConfigNodeBatch 批量写：逐台 CAS、逐台 revision+1、失败清单不连坐。
func TestConfigNodeBatch(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	n1 := newClusterNode(t, db, "node-1")
	n2 := newClusterNode(t, db, "node-2")
	n3 := newClusterNode(t, db, "node-3")

	// n2 带一个旧 rv：它必须失败，n1/n3 必须成功。
	cfgs := []*store.NodeConfig{
		{NodeID: n1.ID, Namespaces: []string{"*"}, Vars: map[string]string{"A": "1"}, ResourceVersion: 1},
		{NodeID: n2.ID, Namespaces: []string{"*"}, Vars: map[string]string{"A": "1"}, ResourceVersion: 99},
		{NodeID: n3.ID, Namespaces: []string{"*"}, Vars: map[string]string{"A": "1"}, ResourceVersion: 1},
	}
	failed := db.PutNodeConfigBatch(ctx, cfgs)
	if len(failed) != 1 || failed[0].NodeID != n2.ID || !errors.Is(failed[0].Err, store.ErrResourceVersionConflict) {
		t.Fatalf("失败清单 = %+v, want 只有 n2 + conflict", failed)
	}
	for _, n := range []*store.Node{n1, n3} {
		cfg, err := db.GetNodeConfig(ctx, n.ID)
		if err != nil {
			t.Fatalf("get config %s: %v", n.ID, err)
		}
		if cfg.Revision != 1 || cfg.ResourceVersion != 2 || cfg.Vars["A"] != "1" {
			t.Fatalf("%s 批量写后 = %+v", n.ID, cfg)
		}
	}
	cfg2, err := db.GetNodeConfig(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get config n2: %v", err)
	}
	if cfg2.Revision != 0 {
		t.Fatalf("失败的那台不该被写：%+v", cfg2)
	}
}

// TestConfigSystemCAS 系统配置：默认值、缺 rv、旧 rv、成功 bump。
func TestConfigSystemCAS(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cfg, err := db.GetSystemConfig(ctx)
	if err != nil {
		t.Fatalf("get system config: %v", err)
	}
	if cfg.RetentionEventTTLSec != 2592000 || cfg.RetentionTaskTTLSec != 2592000 ||
		cfg.RetentionResultTTLSec != 604800 || cfg.ResourceVersion != 1 {
		t.Fatalf("默认系统配置 = %+v", cfg)
	}
	if err := db.PutSystemConfig(ctx, &store.SystemConfig{RetentionEventTTLSec: 1}, 0); !errors.Is(err, store.ErrResourceVersionRequired) {
		t.Fatalf("缺 rv 应返回 ErrResourceVersionRequired，实际 %v", err)
	}
	next := &store.SystemConfig{
		RetentionEventTTLSec:  100,
		RetentionTaskTTLSec:   200,
		RetentionResultTTLSec: 300,
	}
	if err := db.PutSystemConfig(ctx, next, 1); err != nil {
		t.Fatalf("put system config: %v", err)
	}
	if next.ResourceVersion != 2 {
		t.Fatalf("成功后 rv = %d, want 2", next.ResourceVersion)
	}
	if err := db.PutSystemConfig(ctx, next, 1); !errors.Is(err, store.ErrResourceVersionConflict) {
		t.Fatalf("旧 rv 应返回 ErrResourceVersionConflict，实际 %v", err)
	}
	got, err := db.GetSystemConfig(ctx)
	if err != nil {
		t.Fatalf("get system config: %v", err)
	}
	if got.RetentionEventTTLSec != 100 || got.RetentionTaskTTLSec != 200 ||
		got.RetentionResultTTLSec != 300 || got.ResourceVersion != 2 {
		t.Fatalf("读回 = %+v", got)
	}
	if err := db.PutSystemConfig(ctx, &store.SystemConfig{RetentionEventTTLSec: -1}, 2); !errors.Is(err, store.ErrConfigInvalid) {
		t.Fatalf("负 TTL 应返回 ErrConfigInvalid，实际 %v", err)
	}
	// 0 = 永久，合法。
	if err := db.PutSystemConfig(ctx, &store.SystemConfig{}, 2); err != nil {
		t.Fatalf("0（永久）应合法：%v", err)
	}
}

// TestConfigJSONRoundTrip facts / declared capabilities / 配置 JSON 字段往返。
func TestConfigJSONRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	node := newClusterNode(t, db, "node-a")

	facts := protocol.NodeFacts{
		HostIP:   "10.0.0.7",
		Hostname: "builder-1",
		OS:       "linux",
		Arch:     "amd64",
		CPU:      8,
		WorkRoot: "/var/lib/anode/work",
		HomeDir:  "/home/builder",
	}
	caps := []store.DeclaredCapability{
		{
			Name:    "build",
			Version: 2,
			Digest:  "sha256:deadbeef",
			Commands: []protocol.HelloCommand{
				{Name: "git", Found: true, Path: "/usr/bin/git"},
				{Name: "jq", Found: false},
			},
		},
	}
	if err := db.RecordHello(ctx, node.ID, facts, caps, []string{"default", "team-a"},
		map[string]string{"SOURCE_ROOT": "/srv/src"},
		map[string]any{"total": 4, "perCapability": map[string]any{"fixer": 2}},
		"1.4.9", "abcdef"); err != nil {
		t.Fatalf("record hello: %v", err)
	}

	got, err := db.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Facts != facts {
		t.Fatalf("facts 往返丢了：%+v → %+v", facts, got.Facts)
	}
	if len(got.DeclaredCapabilities) != 1 {
		t.Fatalf("declared capabilities = %+v", got.DeclaredCapabilities)
	}
	dc := got.DeclaredCapabilities[0]
	if dc.Name != "build" || dc.Version != 2 || dc.Digest != "sha256:deadbeef" || len(dc.Commands) != 2 ||
		dc.Commands[0].Name != "git" || !dc.Commands[0].Found || dc.Commands[0].Path != "/usr/bin/git" ||
		dc.Commands[1].Name != "jq" || dc.Commands[1].Found {
		t.Fatalf("declared capability 往返丢了：%+v", dc)
	}
	if len(got.DeclaredNamespaces) != 2 || got.DeclaredNamespaces[0] != "default" ||
		got.DeclaredNamespaces[1] != "team-a" {
		t.Fatalf("declared namespaces = %v", got.DeclaredNamespaces)
	}
	if got.AgentVersion != "1.4.9" || got.AgentCommit != "abcdef" {
		t.Fatalf("agent 版本事实 = %q/%q", got.AgentVersion, got.AgentCommit)
	}

	cfg, err := db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	if cfg.LocalVars["SOURCE_ROOT"] != "/srv/src" {
		t.Fatalf("local vars 往返丢了：%v", cfg.LocalVars)
	}
	if limitInt(t, cfg.LocalLimits, "total") != 4 ||
		limitInt(t, cfg.LocalLimits, "perCapability", "fixer") != 2 {
		t.Fatalf("local limits 往返丢了：%v", cfg.LocalLimits)
	}
}
