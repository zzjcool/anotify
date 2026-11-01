package store

// artifacts / deployments 两张新表（迁移 18）的往返与幂等语义。
//
// 契约见 .pi-orchestrator/console-declares/CONTRACT-protocol.md §9 / §7：
//   - artifacts 按 (namespace_id, digest) 唯一：digest 是内容寻址标识，
//     同一份内容重复登记必须是 no-op（返回同一行），不能变成两行；
//   - deployments 按 (node_id, artifact_id) 唯一：控制台重复点部署是幂等 no-op，
//     且**不得**把已经 applied 的状态重置回 pending（否则一次误点就把绿标打回灰）；
//   - 两个维度都受命名空间隔离（I11）：跨命名空间取不到。

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// dpDigest 造一个形状合法的 digest（不需要真算 sha256：store 不校验内容）。
func dpDigest(seed string) string {
	return "sha256:" + seed + "0000000000000000000000000000000000000000000000000000000000000000"[:64-len(seed)]
}

// dpCap 建一个能力定义（artifacts.capability_id 的外键目标）。
func dpCap(t *testing.T, db *DB, nsID, name string) *CapabilityVersion {
	t.Helper()
	c := mkCap(nsID, name, 1, []string{"fix"})
	if err := db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("create capability %s: %v", name, err)
	}
	return c
}

// dpArtifact 登记一份产物。
func dpArtifact(t *testing.T, db *DB, nsID, capID, digest, spec string) *Artifact {
	t.Helper()
	a, err := db.CreateArtifact(context.Background(), &Artifact{
		NamespaceID:  nsID,
		CapabilityID: capID,
		Digest:       digest,
		Spec:         []byte(spec),
	})
	if err != nil {
		t.Fatalf("create artifact %s: %v", digest, err)
	}
	return a
}

// TestCreateArtifactIdempotentByDigest 同一 digest 两次登记 → **同一行**（同一个 id）。
func TestCreateArtifactIdempotentByDigest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := dpCap(t, db, nsID, "review")
	digest := dpDigest("a1")
	spec := `{"apiVersion":"anotify/v1","kind":"Capability"}`

	first := dpArtifact(t, db, nsID, cap.ID, digest, spec)
	if first.ID == "" {
		t.Fatal("CreateArtifact 没有生成 id")
	}
	// 第二次（控制台重复保存同一个 run 块 / 重放一次部署请求）
	second := dpArtifact(t, db, nsID, cap.ID, digest, spec)
	if second.ID != first.ID {
		t.Fatalf("同 digest 两次 CreateArtifact 返回了不同的行: %s vs %s", second.ID, first.ID)
	}
	if second.CreatedAt != first.CreatedAt {
		t.Fatalf("幂等登记改了 created_at: %d → %d", first.CreatedAt, second.CreatedAt)
	}
	// 物理上只有一行
	if n := countRows(t, db, "artifacts", "namespace_id = ? AND digest = ?", nsID, digest); n != 1 {
		t.Fatalf("artifacts 里同 digest 有 %d 行, want 1", n)
	}
	// spec 与 digest 读回逐字一致（内容寻址的前提：存什么读什么）
	got, err := db.GetArtifactByDigest(ctx, nsID, digest)
	if err != nil {
		t.Fatalf("GetArtifactByDigest: %v", err)
	}
	if !bytes.Equal(got.Spec, []byte(spec)) || got.Digest != digest || got.CapabilityID != cap.ID {
		t.Fatalf("artifact round-trip mismatch: %+v", got)
	}
}

// TestArtifactsUniquePerNamespaceDigest UNIQUE(namespace_id, digest) 在 SQL 层就生效：
// 绕过 CreateArtifact 直插同 (namespace, digest) 的不同 id 必须被拒。
func TestArtifactsUniquePerNamespaceDigest(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	cap := dpCap(t, db, nsID, "review")
	digest := dpDigest("b2")
	dpArtifact(t, db, nsID, cap.ID, digest, `{}`)

	_, err := db.Exec(`INSERT INTO artifacts (id, namespace_id, capability_id, digest, spec, created_at)
		VALUES (?,?,?,?,?,?)`, "art_dup", nsID, cap.ID, digest, `{}`, Now())
	if err == nil {
		t.Fatal("同 (namespace_id, digest) 直插第二行成功了：唯一键没生效")
	}

	// 另一个命名空间里同 digest 是**另一份产物**（隔离维度是 namespace_id）
	nsB, _ := newTestNS(t, db)
	capB := dpCap(t, db, nsB, "review")
	other := dpArtifact(t, db, nsB, capB.ID, digest, `{}`)
	if other.ID == "art_dup" || other.NamespaceID != nsB {
		t.Fatalf("跨命名空间同 digest 的产物不对: %+v", other)
	}
}

// TestArtifactNamespaceIsolation 跨命名空间取不到别人的产物（I11）。
func TestArtifactNamespaceIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsA, _ := newTestNS(t, db)
	nsB, _ := newTestNS(t, db)
	capA := dpCap(t, db, nsA, "review")
	digest := dpDigest("c3")
	a := dpArtifact(t, db, nsA, capA.ID, digest, `{}`)

	if _, err := db.GetArtifactByDigest(ctx, nsB, digest); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("跨命名空间 GetArtifactByDigest err = %v, want ErrArtifactNotFound", err)
	}
	if _, err := db.GetArtifact(ctx, nsB, a.ID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("跨命名空间 GetArtifact err = %v, want ErrArtifactNotFound", err)
	}
	if got, err := db.ListArtifacts(ctx, nsB, capA.ID); err != nil {
		t.Fatalf("ListArtifacts cross: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("跨命名空间列出了 %d 份产物", len(got))
	}
	if _, err := db.LatestArtifact(ctx, nsB, capA.ID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("跨命名空间 LatestArtifact err = %v, want ErrArtifactNotFound", err)
	}
}

// TestListArtifactsAndLatest 列表（新的在前）与「最新一份」；没有产物时是
// ErrArtifactNotFound 而不是空行（handler 靠它翻 404 artifact_not_found）。
func TestListArtifactsAndLatest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := dpCap(t, db, nsID, "review")

	if _, err := db.LatestArtifact(ctx, nsID, cap.ID); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("没有产物时 LatestArtifact err = %v, want ErrArtifactNotFound", err)
	}
	first := dpArtifact(t, db, nsID, cap.ID, dpDigest("d4"), `{"v":1}`)
	second := dpArtifact(t, db, nsID, cap.ID, dpDigest("e5"), `{"v":2}`)
	// created_at 只到秒：手工钉开，确保「新的在前」这条断言不是靠运气
	if _, err := db.Exec(`UPDATE artifacts SET created_at = 1000 WHERE id = ?`, first.ID); err != nil {
		t.Fatalf("pin first created_at: %v", err)
	}
	if _, err := db.Exec(`UPDATE artifacts SET created_at = 2000 WHERE id = ?`, second.ID); err != nil {
		t.Fatalf("pin second created_at: %v", err)
	}

	list, err := db.ListArtifacts(ctx, nsID, cap.ID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("ListArtifacts 顺序 = %v, want [新的在前]", list)
	}
	latest, err := db.LatestArtifact(ctx, nsID, cap.ID)
	if err != nil {
		t.Fatalf("LatestArtifact: %v", err)
	}
	if latest.ID != second.ID {
		t.Fatalf("LatestArtifact = %s, want 最新登记的 %s", latest.ID, second.ID)
	}
}

// dpDeployFixture 造「能力 + 产物 + 一台节点」。
func dpDeployFixture(t *testing.T, db *DB, nsID string) (node *Node, art *Artifact, cap *CapabilityVersion) {
	t.Helper()
	cap = dpCap(t, db, nsID, "review")
	art = dpArtifact(t, db, nsID, cap.ID, dpDigest("f6"), `{"v":1}`)
	node, _ = mkNode(t, db, nsID, "mbp")
	return node, art, cap
}

// TestCreateDeploymentIdempotent 同 (node, artifact) 两次建行 → 同一行，
// 且**不把已经 applied 的状态重置回 pending**（契约 §7 的写者纪律）。
func TestCreateDeploymentIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, art, _ := dpDeployFixture(t, db, nsID)

	first, err := db.CreateDeployment(ctx, nsID, node.ID, art.ID)
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if first.State != DeploymentPending || first.Digest != art.Digest || first.CapabilityID != art.CapabilityID {
		t.Fatalf("新建部署字段不对: %+v", first)
	}

	// 节点确认应用成功
	if err := db.MarkDeployResult(ctx, nsID, first.ID, DeploymentApplied, ""); err != nil {
		t.Fatalf("mark applied: %v", err)
	}

	// 控制台又点了一次部署（幂等）
	again, err := db.CreateDeployment(ctx, nsID, node.ID, art.ID)
	if err != nil {
		t.Fatalf("重复部署应幂等，got err: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("重复部署建出了第二行: %s vs %s", again.ID, first.ID)
	}
	if again.State != DeploymentApplied {
		t.Fatalf("重复部署把状态重置成了 %q, want %q（绿标不该被误点打回灰）", again.State, DeploymentApplied)
	}
	if n := countRows(t, db, "deployments", "node_id = ? AND artifact_id = ?", node.ID, art.ID); n != 1 {
		t.Fatalf("deployments 行数 = %d, want 1", n)
	}

	// 同节点换一份产物（新 digest）是**另一行**：旧行是历史，不该被覆盖
	art2 := dpArtifact(t, db, nsID, art.CapabilityID, dpDigest("a7"), `{"v":2}`)
	other, err := db.CreateDeployment(ctx, nsID, node.ID, art2.ID)
	if err != nil {
		t.Fatalf("deploy new artifact: %v", err)
	}
	if other.ID == first.ID {
		t.Fatal("换 digest 的部署复用了旧行")
	}
	if n := countRows(t, db, "deployments", "node_id = ?", node.ID); n != 2 {
		t.Fatalf("deployments 行数 = %d, want 2（旧 digest 的行保留为历史）", n)
	}
}

// TestDeleteDeploymentRemovesRow 撤销部署：行直接消失（契约 §7 明确不留 removed 态），
// 且返回被删的行（调用方要靠它推 deploy{action:remove}）。
func TestDeleteDeploymentRemovesRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	node, art, cap := dpDeployFixture(t, db, nsID)

	dp, err := db.CreateDeployment(ctx, nsID, node.ID, art.ID)
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

	capA := dpCap(t, db, nsID, "review")
	capB := dpCap(t, db, nsID, "test")
	artA := dpArtifact(t, db, nsID, capA.ID, dpDigest("b8"), `{}`)
	artB := dpArtifact(t, db, nsID, capB.ID, dpDigest("c9"), `{}`)
	n1, _ := mkNode(t, db, nsID, "n1")
	n2, _ := mkNode(t, db, nsID, "n2")

	type pair struct{ node, art string }
	var pairs []pair
	for _, n := range []*Node{n1, n2} {
		for _, a := range []*Artifact{artA, artB} {
			if _, err := db.CreateDeployment(ctx, nsID, n.ID, a.ID); err != nil {
				t.Fatalf("create deployment: %v", err)
			}
			pairs = append(pairs, pair{node: n.ID, art: a.ID})
		}
	}
	if len(pairs) != 4 {
		t.Fatalf("夹具错了：%d 条部署", len(pairs))
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
