package store

// namespace_test.go 覆盖 v4 命名空间（config.md §3）：
//
//	命名空间是唯一隔离维度；注册用户原子地建 default 命名空间（v4 **不**播种内置能力）；
//	改名/删除走 CAS（expectedVersion = 单一 UID+rev 令牌）；非空删除要 force，
//	且有真实执行占用（未回收 attempt）时 force 也必须拒绝。
//
// v3 的「建命名空间同时播种 builtin 能力」「CreateEdge/EnsureGraphVersion 跨 ns 图」
// 随中央播种与图对象退役：隔离断言改用能力/部署/任务/事件这些真实资源。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestNamespaceCRUD 建/读/列/改名（CAS）/删（CAS）。
func TestNamespaceCRUD(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	ns, err := db.CreateNamespace(ctx, "alpha", "Alpha")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if ns.ID == "" || ns.Slug != "alpha" || ns.Name != "Alpha" || ns.ResourceVersion == 0 {
		t.Fatalf("create 返回 = %+v", ns)
	}
	got, err := db.GetNamespace(ctx, ns.ID)
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if got.Slug != "alpha" {
		t.Fatalf("roundtrip slug = %q", got.Slug)
	}
	list, err := db.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(list) != 1 || list[0].ID != ns.ID {
		t.Fatalf("列表 = %+v, want 只有 %s", list, ns.ID)
	}
	rv := ResourceVersion(ns.ID, ns.ResourceVersion)
	renamed, err := db.UpdateNamespace(ctx, "alpha", "Alpha 2", rv)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Name != "Alpha 2" {
		t.Fatalf("改名后 name = %q", renamed.Name)
	}
	if _, err := db.UpdateNamespace(ctx, "alpha", "Again", rv); !errors.Is(err, ErrResourceVersionConflict) {
		t.Fatalf("旧 resourceVersion 应冲突，得到 %v", err)
	}
	if err := db.DeleteNamespaceByName(ctx, "alpha", false, ResourceVersion(ns.ID, renamed.ResourceVersion+1)); !errors.Is(err, ErrResourceVersionConflict) {
		t.Fatalf("错 resourceVersion 删除应冲突，得到 %v", err)
	}
	// 同部署再建一个命名空间：最后一个命名空间不允许删（ErrLastNamespace）。
	newTestExtraNS(t, db, "beta")
	if err := db.DeleteNamespaceByName(ctx, "alpha", false, ResourceVersion(ns.ID, renamed.ResourceVersion)); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	if _, err := db.GetNamespace(ctx, ns.ID); !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("删后读应 ErrNamespaceNotFound，得到 %v", err)
	}
}

// TestNamespaceRequirementEnforced 需要命名空间的入口必须拒空/通配。
func TestNamespaceRequirementEnforced(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateNamespace(ctx, "", ""); err == nil {
		t.Fatal("空 slug 建命名空间被接受")
	}
	if _, err := db.ListCapabilities(ctx, ""); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("空命名空间列表应 ErrNamespaceRequired，得到 %v", err)
	}
	if _, err := db.ListCapabilities(ctx, AllNamespaces); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("通配命名空间列表应 ErrNamespaceRequired，得到 %v", err)
	}
	if _, err := db.ListTasks(ctx, "", TaskFilter{}); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("空命名空间任务列表应 ErrNamespaceRequired，得到 %v", err)
	}
	if _, err := db.ListEvents(ctx, "", "", "", 10); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("空命名空间事件列表应 ErrNamespaceRequired，得到 %v", err)
	}
}

// TestCrossNamespaceReferencesRejected 跨命名空间引用被拒（能力 id / 名字都是局部身份）。
func TestCrossNamespaceReferencesRejected(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB := newTestExtraNS(t, db, "ns-b")

	cA := v4Cap(t, db, nsA, "review", 1)
	nodeB := v4Node(t, db, "ns-b-node", 0)
	if _, err := db.CreateDeployments(ctx, nsB, cA.ID, cA.Digest, []string{nodeB.ID}); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("跨 ns 部署应 ErrArtifactNotFound，得到 %v", err)
	}
	if _, err := db.GetCapabilityByID(ctx, nsB, cA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨 ns 读能力应 ErrNotFound，得到 %v", err)
	}
	if _, err := db.GetCapability(ctx, nsB, "review", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B 里不应有 A 的 review，得到 %v", err)
	}
}

// TestNamespaceRoundTrips ns 默认 vars 与 revision 一起往返；旧令牌 CAS 必须冲突。
func TestNamespaceRoundTrips(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns, err := db.CreateNamespace(ctx, "cfg", "配置")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	cfg, err := db.PutNamespaceConfigByName(ctx, "cfg", map[string]string{"REGION": "cn"}, ResourceVersion(ns.ID, ns.ResourceVersion))
	if err != nil {
		t.Fatalf("put namespace config: %v", err)
	}
	if cfg.Vars["REGION"] != "cn" || cfg.Revision == 0 {
		t.Fatalf("配置往返 = %+v", cfg)
	}
	got, err := db.GetNamespaceConfig(ctx, ns.ID)
	if err != nil {
		t.Fatalf("get namespace config: %v", err)
	}
	if got.Vars["REGION"] != "cn" || got.Revision != cfg.Revision {
		t.Fatalf("读回配置 = %+v, want %+v", got, cfg)
	}
	// 配置写入会把命名空间 resourceVersion +1，旧令牌（建 ns 时那枚）必须冲突。
	if _, err := db.PutNamespaceConfigByName(ctx, "cfg", map[string]string{"REGION": "us"}, ResourceVersion(ns.ID, ns.ResourceVersion)); !errors.Is(err, ErrResourceVersionConflict) {
		t.Fatalf("旧令牌应冲突，得到 %v", err)
	}
}

// TestRegisterUserCreatesDefaultNamespace 注册 = 用户 + default 命名空间原子创建，v4 不播种能力。
func TestRegisterUserCreatesDefaultNamespace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	u := &User{ID: NewUserID(), Username: "ns-user", CreatedAt: Now()}
	ns, err := db.RegisterUser(ctx, u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	if ns.ID == "" || ns.Slug != "default" {
		t.Fatalf("注册返回命名空间 = %+v, want slug=default", ns)
	}
	caps, err := db.ListCapabilities(ctx, ns.ID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if len(caps) != 0 {
		t.Fatalf("v4 注册不该播种内置能力，得到 %d 个", len(caps))
	}
	if err := db.AuthorizeNamespace(ctx, u.ID, ns.ID); err != nil {
		t.Fatalf("authorize own namespace: %v", err)
	}
}

// TestNamespaceCreateSeedsBuiltinsNot 反证：v4 新命名空间里 0 个能力（v3 的播种断言退役）。
func TestNamespaceCreateSeedsBuiltinsNot(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	ns, err := db.CreateNamespace(ctx, "empty-seed", "空")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	caps, err := db.ListCapabilities(ctx, ns.ID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if len(caps) != 0 {
		t.Fatalf("新命名空间有 %d 个能力，v4 没有中央播种", len(caps))
	}
}

// TestNamespaceDeleteRequiresForceWhenNotEmpty 非空必须 force；force 也拒绝真实执行占用。
func TestNamespaceDeleteRequiresForceWhenNotEmpty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := v4Cap(t, db, nsID, "review", 1)
	node := v4Node(t, db, "ns-force-node", 1)

	// 最后一个命名空间不允许删（ErrLastNamespace）；先补一个，让断言走「非空阻塞」分支。
	newTestExtraNS(t, db, "force-spare")
	ns, err := db.GetNamespace(ctx, nsID)
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	err = db.DeleteNamespace(ctx, ns.ID, false, ns.ResourceVersion)
	if err == nil {
		t.Fatal("非空命名空间无 force 删除被接受")
	}
	var blocked *NamespaceBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("错误类型 = %T, want *NamespaceBlockedError", err)
	}
	if !strings.Contains(strings.Join(blocked.Blocking, ","), "capabilities") {
		t.Fatalf("Blocking 说明 = %v, want 提到 capabilities", blocked.Blocking)
	}

	task := v4Task(t, db, nsID, c, "tr_ns_force", "")
	v4Grant(t, db, task.ID, node.ID, "asg-ns-force")
	after, err := db.GetNamespace(ctx, nsID)
	if err != nil {
		t.Fatalf("get namespace again: %v", err)
	}
	if err := db.DeleteNamespace(ctx, nsID, true, after.ResourceVersion); err == nil {
		t.Fatal("有未回收 attempt 的命名空间被 force 删除")
	}
	if _, err := db.GetNamespace(ctx, nsID); err != nil {
		t.Fatalf("被拒后命名空间应还在: %v", err)
	}
}
