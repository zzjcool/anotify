package store

// v2（console-declares）的 node_capabilities 对账语义：hello.applied[] 全量重建归属。
//
// # 为什么要重写这一组用例
//
// 旧模型里 hello 自报 capabilities[] + manifest[]（含 run），对应的测试钉的是
// 「run_spec 存什么读什么」「manifest 与 capabilities 合并算法」「runner/concurrency
// 校验」。v2 把那三样整个删掉（契约 D4）：
//   - run_spec 列已消失（执行方式属于产物 artifacts.spec）
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
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// rcDigestLocal 是「节点上那份，与中枢期望不同的」digest（形状合法即可，不需要真算）。
//
// 中枢的**期望值**不再是常量：P2 起它就是能力行的 content_hash（产物是能力行正文
// 算出来的，没有 artifacts 表），所以用例用 rcCapDigest 现取。
const rcDigestLocal = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

// rcCapDigest 返回某能力**当前**的产物 digest —— 也就是中枢对账时用的期望值。
func rcCapDigest(t *testing.T, db *DB, nsID, name string, version int) string {
	t.Helper()
	c, err := db.GetCapability(context.Background(), nsID, name, version)
	if err != nil {
		t.Fatalf("get capability %s@%d: %v", name, version, err)
	}
	return c.Digest()
}

// rcApplied 拼一条 applied[]（少打点字）。
func rcApplied(name string, version int, digest string) AppliedRef {
	return AppliedRef{Name: name, Version: version, Digest: digest}
}

// rcSeedCaps 定义一批能力，返回 name@version → id 的映射。
//
// 用**脚本形式**（不是 mkCap + withSpec 的 handler noop）：对账用例里
// rcDeploymentFixture 要真的部署它们，而 handler 形式刻意不可部署（契约 §5）。
func rcSeedCaps(t *testing.T, db *DB, nsID string, names ...string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	for _, name := range names {
		c := mkCap(nsID, name, 1, []string{"fix"})
		c.Spec = artifact.Spec{
			Params:  c.ParamsSchema,
			Outputs: c.Outputs,
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/bash\nexit 0\n"},
			},
		}
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
// 归属数据的往返一致性在 v2 里只剩这一份（run_spec 已删除）。
func TestReconcileAppliedRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")
	ids := rcSeedCaps(t, db, nsID, "review", "test")

	got, _, err := db.ReconcileApplied(ctx, nsID, n.ID, []AppliedRef{
		rcApplied("review", 1, rcCapDigest(t, db, nsID, "review", 1)),
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

	if _, _, err := db.ReconcileApplied(ctx, nsID, n.ID, []AppliedRef{
		rcApplied("review", 1, rcCapDigest(t, db, nsID, "review", 1)),
		rcApplied("test", 1, ""),
	}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if names := rcNodeCapNames(t, db, n.ID); len(names) != 2 {
		t.Fatalf("want 2 rows after first reconcile, got %v", names)
	}

	// 节点上删掉了 test 的缓存（或升级后不再需要它）→ 只剩 review
	if _, _, err := db.ReconcileApplied(ctx, nsID, n.ID, []AppliedRef{
		rcApplied("review", 1, rcCapDigest(t, db, nsID, "review", 1)),
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
	if _, _, err := db.ReconcileApplied(ctx, nsID, n.ID, nil); err != nil {
		t.Fatalf("empty reconcile: %v", err)
	}
	if names := rcNodeCapNames(t, db, n.ID); len(names) != 0 {
		t.Fatalf("空 applied 没清空归属: %v", names)
	}
}

// TestReconcileAppliedToleratesStaleCache 是**反转**后的用例：节点缓存里报的
// name@version 中枢已经没有（能力被删掉了）时，不再拒绝整帧，而是：
//
//   - 认下认识的（node_capabilities 只写中枢确实有的），
//   - 把不认识的作为 stale 回给调用方（ws 层放进 welcome.prune，让节点删缓存）。
//
// 为什么反转：旧行为是整帧拒绝 + `unknown_capability`，而 agent 把 hello 的
// unknown_capability 当致命错误 → 退出 → systemd 拉起 → 再拒。也就是说
// **在控制台删一个能力，会打死所有在线节点**（实测崩溃循环，每 3 秒一轮）。
// 缓存是派生物，中枢才是真相；派生数据过期不该是协议违规。
func TestReconcileAppliedToleratesStaleCache(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	rcSeedCaps(t, db, nsID, "review")
	desired := rcCapDigest(t, db, nsID, "review", 1)

	fresh, _ := mkNode(t, db, nsID, "fresh")
	withOld, _ := mkNode(t, db, nsID, "with-old")

	// withOld 先成功对账一份，用来断言「含陈旧项的对账仍会重建归属」
	if _, _, err := db.ReconcileApplied(ctx, nsID, withOld.ID,
		[]AppliedRef{rcApplied("review", 1, desired)}); err != nil {
		t.Fatalf("baseline reconcile: %v", err)
	}

	for _, node := range []*Node{fresh, withOld} {
		ids, stale, err := db.ReconcileApplied(ctx, nsID, node.ID, []AppliedRef{
			rcApplied("review", 1, desired),
			rcApplied("does-not-exist", 9, ""),
		})
		if err != nil {
			t.Fatalf("node %s: 陈旧缓存不该拒绝整帧: %v", node.Name, err)
		}
		// ids 与入参逐个对应：认识的那条给 id，不认识的给 ""。
		if ids[0] == "" || ids[1] != "" {
			t.Fatalf("node %s: ids = %v, want [<review id> \"\"]", node.Name, ids)
		}
		// stale 精确列出要剪的那条（节点据此删缓存）。
		if len(stale) != 1 || stale[0].Name != "does-not-exist" || stale[0].Version != 9 {
			t.Fatalf("node %s: stale = %+v, want [{does-not-exist 9}]", node.Name, stale)
		}
	}

	// 认识的那条照常落库，不认识的**不**进 node_capabilities（中枢不从缓存推断归属）。
	names := rcNodeCapNames(t, db, withOld.ID)
	if len(names) != 1 || names[0] != "review@1" {
		t.Fatalf("withOld 的归属 = %v, want [review@1]", names)
	}
	if n := countRows(t, db, "node_capabilities", "node_id = ?", fresh.ID); n != 1 {
		t.Fatalf("fresh 的归属行数 = %d, want 1", n)
	}

	// 空能力名仍然**拒绝**：那不是陈旧缓存，是节点 bug（形状坏掉必须响）。
	_, _, err := db.ReconcileApplied(ctx, nsID, fresh.ID, []AppliedRef{{Name: "", Version: 3}})
	if err == nil {
		t.Fatal("空能力名被接受了")
	}
	var declErr *CapabilityDeclError
	if !errors.As(err, &declErr) {
		t.Fatalf("err 不是 *CapabilityDeclError: %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "capability name is empty") {
		t.Fatalf("空能力名的错误信息 = %q, want 含 'capability name is empty'", err.Error())
	}
}

// TestReconcileAppliedListsAllStale 多个陈旧项一次列全（按声明顺序），
// 同时验证「只有形状坏掉的那类才拒绝」。
func TestReconcileAppliedListsAllStale(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	n, _ := mkNode(t, db, nsID, "mbp")

	_, stale, err := db.ReconcileApplied(ctx, nsID, n.ID, []AppliedRef{
		rcApplied("a", 1, ""),
		rcApplied("b", 2, "sha256:deadbeef"),
		rcApplied("never-defined-anywhere", 1, "sha256:beef"),
	})
	if err != nil {
		t.Fatalf("全陈旧的对账不该报错: %v", err)
	}
	got := make([]string, 0, len(stale))
	for _, s := range stale {
		got = append(got, fmt.Sprintf("%s@%d", s.Name, s.Version))
	}
	want := []string{"a@1", "b@2", "never-defined-anywhere@1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stale = %v, want %v（按声明顺序列全）", got, want)
	}
	if cnt := countRows(t, db, "node_capabilities", "node_id = ?", n.ID); cnt != 0 {
		t.Fatalf("全陈旧的对账留下了 %d 行归属", cnt)
	}
}

// rcDeploymentFixture 造一台节点 + 一个能力 + 一条 pending 部署。
//
// 第三个返回值是**中枢期望的 digest**（= 能力行的 digest）：对账用例要拿它去喂
// hello.applied[]，所以由 fixture 现算，别在用例里写死一个常量。
func rcDeploymentFixture(t *testing.T, db *DB, nsID string) (node *Node, dp *Deployment, capID, desired string) {
	t.Helper()
	ctx := context.Background()
	ids := rcSeedCaps(t, db, nsID, "review")
	capID = ids["review"]
	node, _ = mkNode(t, db, nsID, "mbp")

	var err error
	dp, err = db.CreateDeployment(ctx, nsID, node.ID, capID, "")
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if dp.State != DeploymentPending {
		t.Fatalf("新建部署的 state = %q, want %q", dp.State, DeploymentPending)
	}
	return node, dp, capID, dp.Digest
}

// TestReconcileAppliedDigestMismatchMarksPending applied 的 digest 与中枢期望
// desired 不一致 → deployments.state = pending，reason 里两端 digest 都在（排障时不猜）。
//
// 为什么不再是 local：部署本来就是中枢下发的，节点上那份不一致 = “它还没收到”，
// 不是“用户手改过”。标成“本地变体、永不覆盖”会让一条正常部署永远停在黄标上，
// 还得人去点一次。现在的语义是：中枢覆盖 —— 对账把行标回 pending，
// 握手结束时把期望的那份重新下发（ws.resendDeployments）。
func TestReconcileAppliedDigestMismatchMarksPending(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, dp, _, desired := rcDeploymentFixture(t, db, nsID)

	// 节点上缓存的是另一份产物（旧 digest，或本地被改过）
	if _, _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, rcDigestLocal)}); err != nil {
		t.Fatalf("reconcile applied: %v", err)
	}

	got, err := db.GetDeployment(ctx, nsID, dp.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got.State != DeploymentPending {
		t.Fatalf("digest 不一致时 state = %q, want %q（要重新下发）", got.State, DeploymentPending)
	}
	for _, want := range []string{rcDigestLocal, desired} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q 里没有 %q（两端 digest 都要给，否则排障只能靠猜）", got.Reason, want)
		}
	}
	// 归属仍然是「这台机器有这个能力」：产物不一致只影响“跑哪份”，不影响能不能派
	if names := rcNodeCapNames(t, db, node.ID); len(names) != 1 {
		t.Fatalf("产物不一致时归属丢了: %v", names)
	}
}

// TestReconcileAppliedDigestMatchMarksApplied digest 一致 → applied（绿勾）。
func TestReconcileAppliedDigestMatchMarksApplied(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, dp, _, desired := rcDeploymentFixture(t, db, nsID)

	if _, _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, desired)}); err != nil {
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

	// 反过来：产物不一致（pending）在节点追上中枢期望的 digest 后必须能变回 applied
	// （pending 不是终态 —— 中枢会重推，重推成功就该自愈）。
	if _, _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, rcDigestLocal)}); err != nil {
		t.Fatalf("reconcile to local: %v", err)
	}
	if _, _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, desired)}); err != nil {
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
	node, _, capID, desired := rcDeploymentFixture(t, db, nsID)

	// 部署行已存在但节点还没 hello → 归属还是空的
	if got, err := db.NodesForCapability(ctx, nsID, capID); err != nil {
		t.Fatalf("NodesForCapability: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("对账之前就选到了 %v（归属不该由部署行单方面产生）", got)
	}

	if _, _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{rcApplied("review", 1, desired)}); err != nil {
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
