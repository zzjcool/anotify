package store

// deployments 的部署模型（console-declares / CONTRACT-protocol §7）在 P2 之后的语义。
//
// 契约见 Capability Spec v3 §11：
//   - 产物不再是表里的一行：它就是**能力行的正文**算出来的值（digest = content_hash）；
//   - deployments 引用**能力行**：(node_id, capability_id, digest) 唯一，
//     一个 (机器, 能力) 只有一条期望；
//   - 控制台重复点部署是幂等 no-op，且**不得**把已经 applied 的状态重置回 pending
//     （否则一次误点就把绿标打回灰）；
//   - 两条路都受命名空间隔离（I11）：跨命名空间取不到。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// dpScriptCap 建一个**可部署**（脚本形式）的能力。
//
// 不能用 mkCap + withSpec：那个夹具给的是 handler: noop 占位，而 handler 形式
// 刻意不可部署（契约 §5 / §6.1）。部署用例要的正是「有 files 的那一种」。
func dpScriptCap(t *testing.T, db *DB, nsID, name string) *CapabilityVersion {
	t.Helper()
	c := &CapabilityVersion{
		NamespaceID: nsID, Name: name, Version: 1, Source: CapSourceCustom,
		Spec: artifact.Spec{
			Params:  json.RawMessage(`{"type":"object"}`),
			Outputs: []string{"done"},
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/bash\necho " + name + "\n"},
			},
		},
	}
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability %s: %v", name, err)
	}
	return c
}

// decodeArtifact 把产物正文解回一份完整产物（顺手断言它真的是合法产物）。
func decodeArtifact(t *testing.T, a *Artifact) artifact.Artifact {
	t.Helper()
	var doc artifact.Artifact
	if err := json.Unmarshal(a.Spec, &doc); err != nil {
		t.Fatalf("artifact spec 不是合法产物: %v\n%s", err, a.Spec)
	}
	return doc
}

// TestCurrentArtifactIsCapabilityContent 产物 = 能力行正文算出来的值。
//
// 三件事一起钉住（任何一条错了，部署都会永远收敛不了）：
//   - digest 与能力行的 digest 是同一个值；
//   - 正文是**整份** canonical JSON（metadata + spec，run.files 在内）；
//   - 从正文重算出来的 digest 与中枢说的那个一致（节点就是这么算的）。
func TestCurrentArtifactIsCapabilityContent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	c := dpScriptCap(t, db, nsID, "review")

	got, err := db.CurrentArtifact(ctx, nsID, c.ID)
	if err != nil {
		t.Fatalf("current artifact: %v", err)
	}
	if got.Digest != c.Digest() {
		t.Fatalf("digest = %q, want 能力行的 %q", got.Digest, c.Digest())
	}
	if got.CapabilityID != c.ID || got.NamespaceID != nsID {
		t.Fatalf("归属不对: %+v", got)
	}
	doc := decodeArtifact(t, got)
	if doc.Metadata.Name != "review" || doc.Metadata.Version != 1 {
		t.Fatalf("产物 metadata 不对: %+v", doc.Metadata)
	}
	if body := doc.Spec.Run.Files["run.sh"]; body == "" {
		t.Fatalf("产物没有内联实现（自包含是部署的前提）: %+v", doc.Spec.Run)
	}
	// 内容寻址：从字节重算 == 中枢说的 digest（节点收到帧之后就是这么算的）。
	sum, derr := artifact.Digest(&doc)
	if derr != nil {
		t.Fatalf("recompute digest: %v", derr)
	}
	if sum != got.Digest {
		t.Fatalf("从正文重算的 digest = %q，中枢说的是 %q（两边分叉 = 部署永远不收敛）", sum, got.Digest)
	}

	// 按 digest 取：同一个内容、同一份字节。
	byDigest, err := db.GetArtifactByDigest(ctx, nsID, c.Digest())
	if err != nil {
		t.Fatalf("get artifact by digest: %v", err)
	}
	if byDigest.CapabilityID != c.ID || !bytes.Equal(byDigest.Spec, got.Spec) {
		t.Fatalf("按 digest 取到的不是同一份: %+v", byDigest)
	}

	// 列表最多一条（P2 的语义：一个能力行 = 一份内容 = 一个 digest）。
	list, err := db.ListArtifacts(ctx, nsID, c.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(list) != 1 || list[0].Digest != got.Digest {
		t.Fatalf("ListArtifacts = %+v, want 恰好一条当前产物", list)
	}
}

// TestArtifactAbsentCases 三种「没有产物」都是 ErrArtifactNotFound（不是 nil, nil）：
// 能力不存在 / handler 形式 / digest 对不上。
//
// handler 形式（中枢内置执行器）没有 files，给它造产物就会出现在部署面板里、
// 被推给节点后节点只会拒（契约 §6.1）——这也是 P1 期间「不写穿 artifacts」的原因。
func TestArtifactAbsentCases(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	if _, err := db.CurrentArtifact(ctx, nsID, "cap_missing"); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("未知能力: err = %v, want ErrArtifactNotFound", err)
	}
	if got, err := db.ListArtifacts(ctx, nsID, "cap_missing"); err != nil {
		t.Fatalf("list for missing capability: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("未知能力的产物列表应为空，got %d", len(got))
	}

	// 注册时播种的 notify.phone 就是 handler 形式。
	phone, err := db.GetCapability(ctx, nsID, NotifyPhoneCapability.Name, NotifyPhoneCapability.Version)
	if err != nil {
		t.Fatalf("get notify.phone: %v", err)
	}
	if phone.Spec.Handler == "" {
		t.Fatalf("前提不成立：%s 的正文不是 handler 形式", phone.Name)
	}
	if _, err := db.CurrentArtifact(ctx, nsID, phone.ID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("handler 形式: err = %v, want ErrArtifactNotFound", err)
	}
	if _, err := db.GetArtifactByDigest(ctx, nsID, phone.Digest()); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("handler 形式按 digest: err = %v, want ErrArtifactNotFound", err)
	}

	// 示例能力（script.echo）是脚本形式：它的 digest 能被命中。
	echo, err := db.GetCapability(ctx, nsID, ExampleCapability.Name, ExampleCapability.Version)
	if err != nil {
		t.Fatalf("get script.echo: %v", err)
	}
	if _, err := db.GetArtifactByDigest(ctx, nsID, echo.Digest()); err != nil {
		t.Fatalf("脚本形式的 digest 应该能命中: %v", err)
	}
}

// TestArtifactNamespaceIsolation 跨命名空间取不到别人的产物（I11）。
func TestArtifactNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, _ := newTestNS(t, db)
	capA := dpScriptCap(t, db, nsA, "review")

	if _, err := db.CurrentArtifact(ctx, nsB, capA.ID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("跨命名空间 CurrentArtifact err = %v, want ErrArtifactNotFound", err)
	}
	if _, err := db.GetArtifactByDigest(ctx, nsB, capA.Digest()); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("跨命名空间 GetArtifactByDigest err = %v, want ErrArtifactNotFound", err)
	}
	if got, err := db.ListArtifacts(ctx, nsB, capA.ID); err != nil {
		t.Fatalf("ListArtifacts cross: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("跨命名空间列出了 %d 份产物", len(got))
	}
}

// dpDeployFixture 造「可部署能力 + 一台节点」。
func dpDeployFixture(t *testing.T, db *DB, nsID string) (node *Node, cap *CapabilityVersion) {
	t.Helper()
	cap = dpScriptCap(t, db, nsID, "review")
	node, _ = mkNode(t, db, nsID, "mbp")
	return node, cap
}

// TestCreateDeploymentIdempotent 同 (node, capability) 两次建行 → 同一行，
// 且**不把已经 applied 的状态重置回 pending**（契约 §7 的写者纪律）。
func TestCreateDeploymentIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, cap := dpDeployFixture(t, db, nsID)

	first, err := db.CreateDeployment(ctx, nsID, node.ID, cap.ID, "")
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if first.State != DeploymentPending || first.Digest != cap.Digest() || first.CapabilityID != cap.ID {
		t.Fatalf("新建部署字段不对: %+v（want digest=%s）", first, cap.Digest())
	}
	if first.Capability != cap.Name || first.Version != cap.Version {
		t.Fatalf("部署行没带上能力名/版本: %+v", first)
	}

	// 节点确认应用成功
	if err := db.MarkDeployResult(ctx, nsID, first.ID, DeploymentApplied, ""); err != nil {
		t.Fatalf("mark applied: %v", err)
	}

	// 控制台又点了一次部署（幂等）——显式给同一个 digest 也一样。
	again, err := db.CreateDeployment(ctx, nsID, node.ID, cap.ID, cap.Digest())
	if err != nil {
		t.Fatalf("重复部署应幂等，got err: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("重复部署建出了第二行: %s vs %s", again.ID, first.ID)
	}
	if again.State != DeploymentApplied {
		t.Fatalf("重复部署把状态重置成了 %q, want %q（绿标不该被误点打回灰）", again.State, DeploymentApplied)
	}
	if n := countRows(t, db, "deployments", "node_id = ? AND capability_id = ?", node.ID, cap.ID); n != 1 {
		t.Fatalf("deployments 行数 = %d, want 1", n)
	}

	// 旧库形状：同一 (机器, 能力) 上还留着一条**指向历史 digest** 的行。
	// 部署是「一台机器上这个能力只有一个期望」，再点一次部署要把它取代掉——
	// 不取代的话 hello 对账永远会认为旧的那条“对”，节点每次重连都被推翻一次。
	stale := "sha256:" + "c0ffee0000000000000000000000000000000000000000000000000000000000"[:64]
	if _, err := db.ExecContext(ctx,
		`INSERT INTO deployments (id, namespace_id, node_id, capability_id, digest, state, reason, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		NewDeploymentID(), nsID, node.ID, cap.ID, stale, DeploymentApplied, "", Now()-100, Now()-100); err != nil {
		t.Fatalf("insert legacy stale row: %v", err)
	}
	if n := countRows(t, db, "deployments", "node_id = ?", node.ID); n != 2 {
		t.Fatalf("前置条件：旧行应该有 2 条，实际 %d", n)
	}
	if _, err := db.CreateDeployment(ctx, nsID, node.ID, cap.ID, ""); err != nil {
		t.Fatalf("deploy with legacy row: %v", err)
	}
	if n := countRows(t, db, "deployments", "node_id = ?", node.ID); n != 1 {
		t.Fatalf("deployments 行数 = %d, want 1（历史 digest 的行必须被取代）", n)
	}
	if n := countRows(t, db, "deployments", "node_id = ? AND digest <> ?", node.ID, cap.Digest()); n != 0 {
		t.Fatalf("还有 %d 行指向历史 digest", n)
	}

	// 换能力不受影响：取代范围只能是同一个 capability
	other := dpScriptCap(t, db, nsID, "notify")
	if _, err := db.CreateDeployment(ctx, nsID, node.ID, other.ID, ""); err != nil {
		t.Fatalf("deploy other capability: %v", err)
	}
	if n := countRows(t, db, "deployments", "node_id = ?", node.ID); n != 2 {
		t.Fatalf("跨能力部署把别的能力顶掉了：行数 = %d, want 2", n)
	}
}

// TestCreateDeploymentRejectsMismatch 请求自相矛盾时**不建行**。
//
// 三种都是 ErrArtifactNotFound（handler 靠它翻 404 artifact_not_found）：
//   - 能力不存在；
//   - 能力是 handler 形式（不可部署，契约 §5）；
//   - 显式 digest 不是这个能力的内容（那是「我要部署的是别的东西」，
//     换一个能力去部署等于静默改掉用户的意图）。
func TestCreateDeploymentRejectsMismatch(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, _ := mkNode(t, db, nsID, "box")
	capA := dpScriptCap(t, db, nsID, "review")
	capB := dpScriptCap(t, db, nsID, "notify")

	cases := []struct {
		name     string
		capID    string
		digest   string
		wantZero bool
	}{
		{"未知能力", "cap_missing", "", true},
		{"另一个能力的 digest", capA.ID, capB.Digest(), true},
		{"随便一个 digest", capA.ID, "sha256:" + "d00d000000000000000000000000000000000000000000000000000000000000"[:64], true},
	}
	for _, tc := range cases {
		if _, err := db.CreateDeployment(ctx, nsID, node.ID, tc.capID, tc.digest); !errors.Is(err, ErrArtifactNotFound) {
			t.Fatalf("%s: err = %v, want ErrArtifactNotFound", tc.name, err)
		}
		if n := countRows(t, db, "deployments", "node_id = ?", node.ID); n != 0 {
			t.Fatalf("%s: 被拒却建了 %d 行部署", tc.name, n)
		}
	}

	// handler 形式：实现编译在中枢里，没有 files 可推。
	phone, err := db.GetCapability(ctx, nsID, NotifyPhoneCapability.Name, NotifyPhoneCapability.Version)
	if err != nil {
		t.Fatalf("get notify.phone: %v", err)
	}
	if _, err := db.CreateDeployment(ctx, nsID, node.ID, phone.ID, ""); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("handler 形式: err = %v, want ErrArtifactNotFound", err)
	}
	if n := countRows(t, db, "deployments", "node_id = ?", node.ID); n != 0 {
		t.Fatalf("handler 形式被拒却建了 %d 行部署", n)
	}
}

// TestDeleteDeploymentRemovesRow 撤销部署：行直接消失（契约 §7 明确不留 removed 态），
// 且返回被删的行（调用方要靠它推 deploy{action:remove}）。
func TestDeleteDeploymentRemovesRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, cap := dpDeployFixture(t, db, nsID)

	dp, err := db.CreateDeployment(ctx, nsID, node.ID, cap.ID, "")
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if list, err := db.ListDeployments(ctx, nsID, DeploymentFilter{}); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(list) != 1 {
		t.Fatalf("删除前列表 = %d 行, want 1", len(list))
	}

	deleted, err := db.DeleteDeployment(ctx, nsID, dp.ID)
	if err != nil {
		t.Fatalf("delete deployment: %v", err)
	}
	// 撤销帧要靠返回的 CapabilityID 重新取产物（DeployFrame 没有能力字段）。
	if deleted.ID != dp.ID || deleted.NodeID != node.ID || deleted.CapabilityID != cap.ID {
		t.Fatalf("DeleteDeployment 返回的行不对: %+v", deleted)
	}
	if list, err := db.ListDeployments(ctx, nsID, DeploymentFilter{}); err != nil {
		t.Fatalf("list after delete: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("删除后列表还剩 %d 行", len(list))
	}
	if _, err := db.GetDeployment(ctx, nsID, dp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 GetDeployment err = %v, want ErrNotFound", err)
	}
	// 再删一次是 404 路径（handler 靠它回 not_found）
	if _, err := db.DeleteDeployment(ctx, nsID, dp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除 err = %v, want ErrNotFound", err)
	}
}

// TestListDeploymentsFilters 过滤条件（nodeId / capabilityId）真的下推到 SQL。
//
// 为什么单列一条：GET /v1/deployments 的两个 query 参数直接把值塞进 Filter，
// 过滤写错的表现是「页面显示全部部署」，既不报错也不易察觉。
func TestListDeploymentsFilters(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	capA := dpScriptCap(t, db, nsID, "review")
	capB := dpScriptCap(t, db, nsID, "tester")
	n1, _ := mkNode(t, db, nsID, "n1")
	n2, _ := mkNode(t, db, nsID, "n2")

	for _, n := range []*Node{n1, n2} {
		for _, c := range []*CapabilityVersion{capA, capB} {
			if _, err := db.CreateDeployment(ctx, nsID, n.ID, c.ID, ""); err != nil {
				t.Fatalf("create deployment: %v", err)
			}
		}
	}

	cases := []struct {
		name  string
		f     DeploymentFilter
		want  int
		check func(*Deployment) bool
	}{
		{"无过滤", DeploymentFilter{}, 4, func(*Deployment) bool { return true }},
		{"按节点", DeploymentFilter{NodeID: n1.ID}, 2, func(d *Deployment) bool { return d.NodeID == n1.ID }},
		{"按能力", DeploymentFilter{CapabilityID: capB.ID}, 2, func(d *Deployment) bool { return d.CapabilityID == capB.ID }},
		{"两个一起", DeploymentFilter{NodeID: n2.ID, CapabilityID: capA.ID}, 1,
			func(d *Deployment) bool { return d.NodeID == n2.ID && d.CapabilityID == capA.ID }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.ListDeployments(ctx, nsID, tc.f)
			if err != nil {
				t.Fatalf("ListDeployments: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("行数 = %d, want %d", len(got), tc.want)
			}
			for _, d := range got {
				if !tc.check(d) {
					t.Fatalf("过滤没生效: %+v", d)
				}
			}
		})
	}
}

// TestDeploymentsForNodeDerivesFromCapability 补推（hello 之后 resendDeployments）拿到的
// 帧内容来自**能力行**：digest 与正文同源，且从正文重算的 digest 与它一致。
//
// 老库里可能留着 digest 列与当前内容不一致的行（P1 之前指向历史产物）。补推要按
// 「这个能力现在的产物」推，否则节点装上的东西与中枢说的期望值永远对不上——
// 症状是每次连上来都重推一遍。
func TestDeploymentsForNodeDerivesFromCapability(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, cap := dpDeployFixture(t, db, nsID)

	dp, err := db.CreateDeployment(ctx, nsID, node.ID, cap.ID, "")
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	// 把 digest 列改成一个历史值：它只是建行那一刻的快照，不该影响期望。
	if _, err := db.ExecContext(ctx,
		`UPDATE deployments SET digest = ? WHERE id = ?`, "sha256:deadbeef", dp.ID); err != nil {
		t.Fatalf("pin stale digest: %v", err)
	}

	rows, err := db.DeploymentsForNode(ctx, nsID, node.ID)
	if err != nil {
		t.Fatalf("deployments for node: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("补推行数 = %d, want 1", len(rows))
	}
	it := rows[0]
	if it.DeploymentID != dp.ID || it.CapabilityID != cap.ID || it.Capability != cap.Name {
		t.Fatalf("补推行不对: %+v", it)
	}
	if it.Digest != cap.Digest() {
		t.Fatalf("补推 digest = %q, want 能力行的 %q（不是 deployments.digest 列的副本）", it.Digest, cap.Digest())
	}
	var doc artifact.Artifact
	if err := json.Unmarshal(it.Spec, &doc); err != nil {
		t.Fatalf("补推正文不是合法产物: %v", err)
	}
	sum, derr := artifact.Digest(&doc)
	if derr != nil {
		t.Fatalf("recompute digest: %v", derr)
	}
	if sum != it.Digest {
		t.Fatalf("推下去的正文重算 digest = %q，中枢说的是 %q", sum, it.Digest)
	}
}

// TestMarkDeployedDigestUsesCapabilityDigest hello 对账的期望值取自能力行。
//
// 两条断言合起来才是「中枢覆盖」：
//   - 节点报的正是能力当前的 digest → applied（即使 deployments.digest 列写着别的值）；
//   - 节点报别的 digest → pending，reason 里两端都写上（排障要看到差在哪）。
func TestMarkDeployedDigestUsesCapabilityDigest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, cap := dpDeployFixture(t, db, nsID)

	dp, err := db.CreateDeployment(ctx, nsID, node.ID, cap.ID, "")
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE deployments SET digest = ? WHERE id = ?`, "sha256:deadbeef", dp.ID); err != nil {
		t.Fatalf("pin stale digest: %v", err)
	}

	applied := []AppliedRef{{Name: cap.Name, Version: cap.Version, Digest: cap.Digest()}}
	if _, _, err := db.ReconcileApplied(ctx, nsID, node.ID, applied); err != nil {
		t.Fatalf("reconcile applied: %v", err)
	}
	row, err := db.GetDeployment(ctx, nsID, dp.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if row.State != DeploymentApplied || row.Reason != "" {
		t.Fatalf("一致时报的 digest 应该判 applied 且 reason 为空: state=%q reason=%q", row.State, row.Reason)
	}

	// 节点上的内容被改过（或还没收到新的）→ pending + 两端 digest 都留痕。
	stale := "sha256:" + "beef000000000000000000000000000000000000000000000000000000000000"[:64]
	if _, _, err := db.ReconcileApplied(ctx, nsID, node.ID,
		[]AppliedRef{{Name: cap.Name, Version: cap.Version, Digest: stale}}); err != nil {
		t.Fatalf("reconcile stale: %v", err)
	}
	row, err = db.GetDeployment(ctx, nsID, dp.ID)
	if err != nil {
		t.Fatalf("get deployment after stale: %v", err)
	}
	if row.State != DeploymentPending {
		t.Fatalf("不一致时 state = %q, want pending（中枢马上会重推）", row.State)
	}
	if !bytes.Contains([]byte(row.Reason), []byte(stale)) || !bytes.Contains([]byte(row.Reason), []byte(cap.Digest())) {
		t.Fatalf("reason 里应该同时有节点上的 %s 与中枢期望的 %s，实际 %q", stale, cap.Digest(), row.Reason)
	}
}

// TestDeployProtocolShapeUnchanged deploy 帧的内容形状在 P2 前后必须逐字节相同。
//
// 帧里推的是**整份产物**（WsDeployFrame.Artifact 是整份 canonical JSON，不是 spec
// 子文档）。P2 把来源从 artifacts 行换成能力行，最容易出的错是「推了一份只有 spec
// 的文档」——节点侧解析失败或 digest 全变，而单测各自都能过。
func TestDeployProtocolShapeUnchanged(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, cap := dpDeployFixture(t, db, nsID)
	if _, err := db.CreateDeployment(ctx, nsID, node.ID, cap.ID, ""); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	rows, err := db.DeploymentsForNode(ctx, nsID, node.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("deployments for node: %v (rows=%d)", err, len(rows))
	}
	// 顶层必须同时有 apiVersion / kind / metadata / spec 四个键。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rows[0].Spec, &top); err != nil {
		t.Fatalf("frame artifact 不是合法 JSON: %v", err)
	}
	for _, key := range []string{"apiVersion", "kind", "metadata", "spec"} {
		if _, ok := top[key]; !ok {
			t.Fatalf("deploy 帧的产物缺顶层键 %q（整份产物是契约，不是 spec 子文档）: %s", key, rows[0].Spec)
		}
	}
	// 与「按能力取当前产物」的字节完全一致（同一份 canonical 序列化）。
	cur, err := db.CurrentArtifact(ctx, nsID, cap.ID)
	if err != nil {
		t.Fatalf("current artifact: %v", err)
	}
	if !bytes.Equal(cur.Spec, rows[0].Spec) {
		t.Fatalf("补推正文与当前产物的字节不一致:\n%s\n%s", rows[0].Spec, cur.Spec)
	}
}

// TestDeploymentsForNodeSkipsHandlerCapability 补推**不会**把 handler 形式的产物推给节点。
//
// 建行时就拒了 handler 形式（见 CreateDeployment），这条守的是另一种入口：老库里
// 残留的部署行（P1 之前 artifacts 写穿还没跳过 handler 时留下的）。补推是节点连上来
// 就会跑的路径，推一份没有 files 的产物下去节点只会拒，而日志会显示「已下发」——
// 静默的谎话比报错难查得多。
func TestDeploymentsForNodeSkipsHandlerCapability(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, _ := mkNode(t, db, nsID, "box")

	phone, err := db.GetCapability(ctx, nsID, NotifyPhoneCapability.Name, NotifyPhoneCapability.Version)
	if err != nil {
		t.Fatalf("get notify.phone: %v", err)
	}
	// 绕过 CreateDeployment 直接造一条「handler 能力的部署行」（模拟老库里残留的一行）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO deployments (id, namespace_id, node_id, capability_id, digest, state, reason, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		NewDeploymentID(), nsID, node.ID, phone.ID, phone.Digest(), DeploymentPending, "", Now(), Now()); err != nil {
		t.Fatalf("insert legacy handler deployment: %v", err)
	}

	rows, err := db.DeploymentsForNode(ctx, nsID, node.ID)
	if err != nil {
		t.Fatalf("deployments for node: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("handler 形式被推下去了 %d 条（会得到一份没有 files 的产物）", len(rows))
	}
}

// TestArtifactRequiresRun 可部署 ⟺ **有 run**（entrypoint + files）。
//
// 三种「没有实现」的形态都必须没有产物：handler 形式（实现编译在中枢）、纯定义
// （既没 handler 也没 run —— 迁移 8 之前的老行、事件起点的残壳）、以及能力名不存在。
// 放过去的后果是同一个：它出现在部署面板里，推下去节点只会拒。
func TestArtifactRequiresRun(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// 纯定义：直接落一行没有实现正文的老式能力（绕过 CreateCapability 的校验）。
	capID := "cap_definition_only"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO capability_versions
		   (id, namespace_id, name, display_name, version, params_schema, outputs,
		    lifecycle, sticky, source, content_hash, created_at, spec)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		capID, nsID, "definition.only", "", 1, `{"type":"object"}`, `["done"]`,
		`{}`, "", CapSourceCustom, strings.Repeat("ab", 32), Now(), `{"outputs":["done"]}`); err != nil {
		t.Fatalf("insert definition-only capability: %v", err)
	}
	if _, err := db.CurrentArtifact(ctx, nsID, capID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("纯定义不该有产物: err = %v, want ErrArtifactNotFound", err)
	}
	if arts, err := db.ListArtifacts(ctx, nsID, capID); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(arts) != 0 {
		t.Fatalf("纯定义列出了 %d 份产物", len(arts))
	}
	// 也不能被部署（否则会给节点推一份没有 files 的产物）。
	node, _ := mkNode(t, db, nsID, "box")
	if _, err := db.CreateDeployment(ctx, nsID, node.ID, capID, ""); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("纯定义部署: err = %v, want ErrArtifactNotFound", err)
	}
}
