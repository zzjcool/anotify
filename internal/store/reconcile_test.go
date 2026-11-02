package store

// v2（console-declares）的 node_capabilities 对账语义：hello.applied[] 全量重建归属。
//
// # 为什么要重写这一组用例
//
// 旧模型里 hello 自报 capabilities[] + manifest[]（含 run），对应的测试钉的是
// 「run_spec 存什么读什么」「manifest 与 capabilities 合并算法」「runner/concurrency
// 校验」。v2 把那三样整个删掉（契约 D4）：
//   - run_spec 列随迁移 18 消失（执行方式属于产物 artifacts.spec）
//   - manifest / merge 纯函数删除（节点不再陈述执行方式）
//   - run 的校验搬去 internal/artifact（产物校验，中枢不解释执行方式）
//
// 所以那批断言**不是**「行为漂移后改个字段」，而是被测能力整个消失 —— 删掉的同时
// 必须有等价的替代覆盖，这就是本文件：
//   - run_spec 往返一致 → applied[] 对账的往返一致（存什么读什么）
//   - manifest 合并 → applied 与 desired 的 digest 对账（applied / local）
//   - 「全拒 + 列全」 → 保留（未知能力仍然整帧拒绝且一次列全）
//   - 「run_spec 不参与选点」→ 「归属只由对账派生，且派发链路零改动」

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// 对账用例用的 digest（形状与 artifact.Digest 一致，这里不需要真算）。
const (
	rcDigestDesired = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	rcDigestLocal   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// rcApplied 拼一条 applied[]（少打点字）。
func rcApplied(name string, version int, digest string) AppliedRef {
	return AppliedRef{Name: name, Version: version, Digest: digest}
}

// rcSeedCaps 定义一批能力，返回 name@version → id 的映射。
func rcSeedCaps(t *testing.T, db *DB, nsID string, names ...string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	for _, name := range names {
		c := mkCap(nsID, name, 1, []string{"fix"})
		if err := db.CreateCapability(ctx, c); err != nil {
			t.Fatalf("create capability %s: %v", name, err)
		}
		out[name] = c.ID
	}
	return out
}

// rcNodeCapNames 读回这台节点的能力归属（name@version 集合，排序稳定）。
func rcNodeCapNames(t *testing.T, db *DB, nodeID string) []string {
	t.Helper()
	caps, err := db.ListNodeCapabilities(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("list node capabilities: %v", err)
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, c.Name+"@"+strconv.Itoa(c.Version))
	}
	return out
}

// TestReconcileAppliedRoundTrip 存什么读什么：applied[] 里的 name@version 全量落到
// node_capabilities，且读回来的就是那一组（不多不少、顺序按 name/version 升序）。
//
// 这条是旧 TestNodeCapabilityRunSpecRoundTrip（run_spec 往返）的**替代覆盖**：
// 归属数据的往返一致性在 v2 里只剩这一份（run_spec 已随迁移 18 删除）。
func TestReconcileAppliedRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")
	ids := rcSeedCaps(t, db, nsID, "review", "test")

	got, err := db.ReconcileApplied(ctx, nsID, n.ID, []AppliedRef{
		rcApplied("review", 1, rcDigestDesired),
		rcApplied("test", 1, ""),
	})
	if err != nil {
		t.Fatalf("reconcile applied: %v", err)
	}
	// 返回值顺序与入参一一对应（调用方拿它做逐条回执）
	if len(got) != 2 || got[0] != ids["review"] || got[1] != ids["test"] {
		t.Fatalf("ReconcileApplied 返回 %v, want [%s %s]", got, ids["review"], ids["test"])
	}
	if names := rcNodeCapNames(t, db, n.ID); strings.Join(names, ",") != "review@1,test@1" {
		t.Fatalf("node_capabilities = %v, want [review@1 test@1]", names)
	}
	// 定义本身的字段不因对账变形（嵌入 *CapabilityVersion 的既有契约）
	caps, err := db.ListNodeCapabilities(ctx, n.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(caps[0].Outputs) != 1 || caps[0].Outputs[0] != "fix" {
		t.Fatalf("outputs lost: %v", caps[0].Outputs)
	}
}

// TestReconcileAppliedFullReplace 全量替换语义：第二次 applied[] 里没有的能力，
// 归属立刻消失（这是 DELETE + INSERT 的全部意义，不是追加）。
//
// 旧用例 TestNodeCapabilityDeclsReplaceDropsRunSpec 钉的是同一件事，只是当年
// 还要顺带钉「run_spec 里的旧命令也一起消失」——那一列已经不存在了。
func TestReconcileAppliedFullReplace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")
	rcSeedCaps(t, db, nsID, "review", "test")

	if _, err := db.ReconcileApplied(ctx, nsID, n.ID, []AppliedRef{
		rcApplied("review", 1, rcDigestDesired),
		rcApplied("test", 1, ""),
	}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if names := rcNodeCapNames(t, db, n.ID); len(names) != 2 {
		t.Fatalf("want 2 rows after first reconcile, got %v", names)
	}

	// 节点上删掉了 test 的缓存（或升级后不再需要它）→ 只剩 review
	if _, err := db.ReconcileApplied(ctx, nsID, n.ID, []AppliedRef{
		rcApplied("review", 1, rcDigestDesired),
	}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	names := rcNodeCapNames(t, db, n.ID)
	if len(names) != 1 || names[0] != "review@1" {
		t.Fatalf("全量替换失败，node_capabilities = %v, want [review@1]", names)
	}
	// 物理行也只有一条（不是逻辑上隐藏）
	if n := countRows(t, db, "node_capabilities", "node_id = ?", n.ID); n != 1 {
		t.Fatalf("node_capabilities 行数 = %d, want 1", n)
	}

	// 空 applied[] 也是合法的全量替换：这台机器不再缓存任何产物
	if _, err := db.ReconcileApplied(ctx, nsID, n.ID, nil); err != nil {
		t.Fatalf("empty reconcile: %v", err)
	}
	if names := rcNodeCapNames(t, db, n.ID); len(names) != 0 {
		t.Fatalf("空 applied 没清空归属: %v", names)
	}
}

// TestReconcileAppliedUnknownRejectsWholeFrame 未知能力 → 整帧拒绝、**一行不写**。
//
// 「一行不写」是这条用例的重点：旧实现在校验与写入之间做 DELETE，写坏一点
// 就会留下「删了旧的、没写新的」这种既不能派发也无法自愈的状态。
func TestReconcileAppliedUnknownRejectsWholeFrame(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	rcSeedCaps(t, db, nsID, "review")

	fresh, _ := mkNode(t, db, nsID, "fresh")
	withOld, _ := mkNode(t, db, nsID, "with-old")

	// withOld 先成功对账一份，用来断言「被拒的帧不会把旧归属删掉」
	if _, err := db.ReconcileApplied(ctx, nsID, withOld.ID,
		[]AppliedRef{rcApplied("review", 1, rcDigestDesired)}); err != nil {
		t.Fatalf("baseline reconcile: %v", err)
	}

	for _, node := range []*Node{fresh, withOld} {
		_, err := db.ReconcileApplied(ctx, nsID, node.ID, []AppliedRef{
			rcApplied("review", 1, rcDigestDesired),
			rcApplied("does-not-exist", 9, ""),
		})
		if !errors.Is(err, ErrUnknownCapability) {
			t.Fatalf("node %s: err = %v, want ErrUnknownCapability", node.Name, err)
		}
		var declErr *CapabilityDeclError
		if !errors.As(err, &declErr) || !declErr.UnknownCapability {
			t.Fatalf("node %s: err 不是 *CapabilityDeclError{UnknownCapability:true}: %T %+v", node.Name, err, declErr)
		}
	}

	// 全拒：新节点一行都不留
	if n := countRows(t, db, "node_capabilities", "node_id = ?", fresh.ID); n != 0 {
		t.Fatalf("被拒的对账给新节点留下了 %d 行", n)
	}
	// 旧归属没被半途删掉（拒绝发生在 DELETE 之前）
	names := rcNodeCapNames(t, db, withOld.ID)
	if len(names) != 1 || names[0] != "review@1" {
		t.Fatalf("被拒的对账改动了旧归属: %v", names)
	}

	// 空能力名同样是「未知」，而且不能让它混进成功路径
	_, err := db.ReconcileApplied(ctx, nsID, fresh.ID, []AppliedRef{{Name: "", Version: 3}})
	if !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("空能力名 err = %v, want ErrUnknownCapability", err)
	}
	if !strings.Contains(err.Error(), "capability name is empty") {
		t.Fatalf("空能力名的错误信息 = %q, want 含 'capability name is empty'", err.Error())
	}
}

// TestReconcileAppliedListsAllUnknown 多个问题一次列全（`;` 分隔），不玩「修一条重启一次」。
//
// 这条是旧 TestNodeCapabilityDeclsListAllProblems 的替代覆盖：当年那批问题里
// 混着 run 校验（已随 manifest 下线），保留并钉住的是**未知能力一组 + 分组拼接**。
func TestReconcileAppliedListsAllUnknown(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	_, err := db.ReconcileApplied(ctx, nsID, n.ID, []AppliedRef{
		{Name: "", Version: 7},                                // 名字为空的（未知）
		rcApplied("a", 1, ""),                                 // 中枢没定义
		rcApplied("b", 2, "sha256:deadbeef"),                  // 中枢没定义
		rcApplied("never-defined-anywhere", 1, "sha256:beef"), // 中枢没定义
	})
	if err == nil {
		t.Fatal("未知能力被接受")
	}
	if !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("errors.Is(ErrUnknownCapability) = false (%v)", err)
	}
	var declErr *CapabilityDeclError
	if !errors.As(err, &declErr) {
		t.Fatalf("err 不是 *CapabilityDeclError: %T", err)
	}
	// 两个问题组：①名字为空 ②未知能力合成一组（按声明顺序，逗号分隔）
	if len(declErr.Problems) != 2 {
		t.Fatalf("problems = %v, want 2 组", declErr.Problems)
	}
	if !strings.Contains(declErr.Problems[0], "capability name is empty") {
		t.Fatalf("第一组 = %q, want 名字为空的问题", declErr.Problems[0])
	}
	if !strings.Contains(declErr.Problems[1], "unknown capability: a@1, b@2, never-defined-anywhere@1") {
		t.Fatalf("第二组 = %q, want 按声明顺序列全三条未知能力", declErr.Problems[1])
	}
	// 整条错误是一个字符串，用 `;` 把问题组连起来（节点侧一次看全）
	if msg := err.Error(); !strings.Contains(msg, "; ") {
		t.Fatalf("错误信息 %q 没有用 `;` 分隔问题组", msg)
	}
	// 全拒：一行不写
	if n := countRows(t, db, "node_capabilities", "node_id = ?", n.ID); n != 0 {
		t.Fatalf("被拒的对账留下了 %d 行", n)
	}
}

// rcDeploymentFixture 造一台节点 + 一个能力 + 一份产物 + 一条 pending 部署。
func rcDeploymentFixture(t *testing.T, db *DB, nsID string) (node *Node, dp *Deployment, capID string) {
	t.Helper()
	ctx := context.Background()
	ids := rcSeedCaps(t, db, nsID, "review")
	capID = ids["review"]
	node, _ = mkNode(t, db, nsID, "mbp")

	art, err := db.CreateArtifact(ctx, &Artifact{
		NamespaceID:  nsID,
		CapabilityID: capID,
		Digest:       rcDigestDesired,
		Spec:         []byte(`{"apiVersion":"anotify/v1"}`),
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	dp, err = db.CreateDeployment(ctx, nsID, node.ID, art.ID)
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if dp.State != DeploymentPending {
		t.Fatalf("新建部署的 state = %q, want %q", dp.State, DeploymentPending)
	}
	return node, dp, capID
}

// TestReconcileAppliedDigestMismatchMarksLocal applied 的 digest 与 desired 不一致
// → deployments.state = local，reason 里两端 digest 都在（排障时不猜）。
//
// 这是旧「manifest 冲突合并」用例的替代覆盖：v2 里中枢与节点之间唯一可能不一致的
// 东西就是 digest，而它对应契约 §7 的「本地变体」状态。
func TestReconcileAppliedDigestMismatchMarksLocal(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, dp, _ := rcDeploymentFixture(t, db, nsID)

	// 节点上缓存的是一份被手改过的产物（digest 变了）
	if _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, rcDigestLocal)}); err != nil {
		t.Fatalf("reconcile applied: %v", err)
	}

	got, err := db.GetDeployment(ctx, nsID, dp.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.State != DeploymentLocal {
		t.Fatalf("digest 不一致时 state = %q, want %q（本地变体，中枢永不覆盖）", got.State, DeploymentLocal)
	}
	for _, want := range []string{rcDigestLocal, rcDigestDesired} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q 里没有 %q（两端 digest 都要给，否则排障只能靠猜）", got.Reason, want)
		}
	}
	// 归属仍然是「这台机器有这个能力」：本地变体只是中枢不敢覆盖，不是不可派发
	if names := rcNodeCapNames(t, db, node.ID); len(names) != 1 {
		t.Fatalf("本地变体的归属丢了: %v", names)
	}
}

// TestReconcileAppliedDigestMatchMarksApplied digest 一致 → applied（绿勾）。
func TestReconcileAppliedDigestMatchMarksApplied(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, dp, _ := rcDeploymentFixture(t, db, nsID)

	if _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, rcDigestDesired)}); err != nil {
		t.Fatalf("reconcile applied: %v", err)
	}
	got, err := db.GetDeployment(ctx, nsID, dp.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.State != DeploymentApplied {
		t.Fatalf("digest 一致时 state = %q, want %q", got.State, DeploymentApplied)
	}
	if got.Reason != "" {
		t.Fatalf("applied 的 reason 应为空，got %q", got.Reason)
	}

	// 反过来：本地变体在节点改回中枢期望的 digest 后可以回到 applied
	// （local 不是终态 —— 中枢永不主动覆盖，但用户修好后必须能自愈）。
	if _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, rcDigestLocal)}); err != nil {
		t.Fatalf("reconcile to local: %v", err)
	}
	if _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, rcDigestDesired)}); err != nil {
		t.Fatalf("reconcile back to applied: %v", err)
	}
	got, err = db.GetDeployment(ctx, nsID, dp.ID)
	if err != nil {
		t.Fatalf("get deployment after heal: %v", err)
	}
	if got.State != DeploymentApplied || got.Reason != "" {
		t.Fatalf("自愈后 state/reason = %q/%q, want applied/空", got.State, got.Reason)
	}
}

// TestReconcileAppliedFeedsDispatch 归属只由对账派生，派发链路零改动（契约 D4 的全部价值）。
//
// 旧用例在这里钉的是「run_spec 不参与选点」（I1 的老说法）；v2 的正确说法是
// 「这台机器能跑什么 == 中枢给它部署了什么」，所以断言换成：对账之前选不到它，
// 对账之后立刻选得到 —— 而 NodesForCapability 的 SQL 一行没改。
func TestReconcileAppliedFeedsDispatch(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, _, capID := rcDeploymentFixture(t, db, nsID)

	// 部署行已存在但节点还没 hello → 归属还是空的
	if got, err := db.NodesForCapability(ctx, nsID, capID); err != nil {
		t.Fatalf("NodesForCapability: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("对账之前就选到了 %v（归属不该由部署行单方面产生）", got)
	}

	if _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, rcDigestDesired)}); err != nil {
		t.Fatalf("reconcile applied: %v", err)
	}
	got, err := db.NodesForCapability(ctx, nsID, capID)
	if err != nil {
		t.Fatalf("NodesForCapability after reconcile: %v", err)
	}
	if len(got) != 1 || got[0] != node.ID {
		t.Fatalf("派发候选 = %v, want [%s]", got, node.ID)
	}
}
