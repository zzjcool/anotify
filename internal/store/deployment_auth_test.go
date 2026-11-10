package store

// deployment_auth_test.go 是 S1-D7 的白盒验收：
//
//   - CreateDeployments 在同一事务里对**全部**显式目标校验当前 namespace 授权；
//     未授权/不存在（在线或离线）全包 0 写，不 supersede 先前合法 intent；
//   - offline 但已授权的节点仍是合法 pending；
//   - MarkDeployResultAuthorized 同事务核当前凭证/ns/Node/intent 身份/requestId/digest；
//     旧 token/撤销 ns/unknown request/错 digest/跨 Node/superseded 全 0 写；
//     合法 applied/error 往返；重复精确同结果幂等，changed body 冲突。

import (
	"context"
	"errors"
	"testing"
)

func TestCreateDeploymentsNamespaceAuthorization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := v4Cap(t, db, nsID, "deploy-cap", 1)
	okNode := v4Node(t, db, "deploy-ok", 0)
	badNode := v4Node(t, db, "deploy-bad", 0)
	// 撤销 badNode 的 namespace 授权（declared 仍是 "*"，但授权列表不含目标 ns）。
	putV4NodeCfg(t, db, badNode.ID, []string{"other"}, nil, nil)

	// 单独未授权目标：0 写。
	if _, err := db.CreateDeployments(ctx, nsID, cap.ID, cap.Digest, []string{badNode.ID}); !errors.Is(err, ErrEmitNamespaceDenied) {
		t.Fatalf("未授权节点应 ErrEmitNamespaceDenied，得到 %v", err)
	}
	if rows, _ := db.ListDeployments(ctx, nsID, DeploymentFilter{}); len(rows) != 0 {
		t.Fatalf("未授权目标留下了 intent: %+v", rows)
	}

	// 合法 intent 先建出来（混合请求失败时不许被 supersede/删除）。
	legal, err := db.CreateDeployments(ctx, nsID, cap.ID, "", []string{okNode.ID})
	if err != nil || len(legal) != 1 {
		t.Fatalf("legal create: %+v %v", legal, err)
	}
	if _, err := db.CreateDeployments(ctx, nsID, cap.ID, "", []string{okNode.ID, badNode.ID}); !errors.Is(err, ErrEmitNamespaceDenied) {
		t.Fatalf("混合目标应整包拒绝，得到 %v", err)
	}
	rows, err := db.ListDeployments(ctx, nsID, DeploymentFilter{})
	if err != nil || len(rows) != 1 || rows[0].ID != legal[0].ID || rows[0].State != DeploymentPending {
		t.Fatalf("混合失败改动了先前合法 intent: %+v %v", rows, err)
	}

	// offline 但已授权：仍是合法 pending（offline 不是未授权）。
	off := v4Node(t, db, "deploy-offline", 0)
	if err := db.UpdateNodeStatus(ctx, off.ID, NodeStatusOffline); err != nil {
		t.Fatal(err)
	}
	out, err := db.CreateDeployments(ctx, nsID, cap.ID, "", []string{off.ID})
	if err != nil || len(out) != 1 || out[0].State != DeploymentPending {
		t.Fatalf("已授权离线节点应生成 pending: %+v %v", out, err)
	}
}

// deployEnv 建一条 pending 部署 intent 及其节点。
func deployEnv(t *testing.T) (*DB, string, *CapabilityVersion, *Node, *Deployment) {
	t.Helper()
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	cap := v4Cap(t, db, nsID, "deploy-auth", 1)
	node := v4Node(t, db, "deploy-auth-node", 0)
	rows, err := db.CreateDeployments(ctx, nsID, cap.ID, "", []string{node.ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("create deployment: %+v %v", rows, err)
	}
	return db, nsID, cap, node, rows[0]
}

func deployResultInputs(nsID string, cap *CapabilityVersion, dep *Deployment, state, reason string, now int64) DeploymentResultInputs {
	return DeploymentResultInputs{
		DeploymentID: dep.ID,
		RequestID:    DeploymentRequestID(dep.ID),
		NamespaceID:  nsID,
		CapabilityID: cap.ID,
		Capability:   cap.Name,
		Version:      cap.Version,
		Digest:       cap.Digest,
		State:        state,
		Reason:       reason,
		Now:          now,
	}
}

func TestMarkDeployResultAuthorizedRoundTripAndIdempotence(t *testing.T) {
	db, nsID, cap, node, dep := deployEnv(t)
	ctx := context.Background()
	auth := v4Auth(t, db, node.ID)

	if err := db.MarkDeployResultAuthorized(ctx, auth, deployResultInputs(nsID, cap, dep, DeploymentApplied, "", 1000)); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	got, err := db.GetDeployment(ctx, nsID, dep.ID)
	if err != nil || got.State != DeploymentApplied || got.Reason != "" || got.UpdatedAt != 1000 {
		t.Fatalf("applied 往返 = %+v %v", got, err)
	}
	// 重复精确同结果：幂等且不重写 updated_at。
	if err := db.MarkDeployResultAuthorized(ctx, auth, deployResultInputs(nsID, cap, dep, DeploymentApplied, "", 2000)); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	again, _ := db.GetDeployment(ctx, nsID, dep.ID)
	if again.UpdatedAt != got.UpdatedAt {
		t.Fatalf("重复同结果改写了 updated_at: %d → %d", got.UpdatedAt, again.UpdatedAt)
	}
	// 已记录 receipt 后 changed body：冲突、0 写。
	if err := db.MarkDeployResultAuthorized(ctx, auth, deployResultInputs(nsID, cap, dep, DeploymentFailed, "need_root", 3000)); !errors.Is(err, ErrDeploymentReceiptConflict) {
		t.Fatalf("changed body 应 ErrDeploymentReceiptConflict，得到 %v", err)
	}
	after, _ := db.GetDeployment(ctx, nsID, dep.ID)
	if after.State != DeploymentApplied || after.UpdatedAt != got.UpdatedAt {
		t.Fatalf("冲突路径改动了 receipt: %+v", after)
	}
}

func TestMarkDeployResultAuthorizedWrongRequestsZeroWrites(t *testing.T) {
	db, nsID, cap, node, dep := deployEnv(t)
	ctx := context.Background()
	auth := v4Auth(t, db, node.ID)
	other := v4Node(t, db, "deploy-other-node", 0)

	cases := map[string]struct {
		auth NodeAuthorization
		mut  func(*DeploymentResultInputs)
	}{
		"old_token":  {NodeAuthorization{NodeID: node.ID, NodeTokenHash: "deadbeef"}, nil},
		"cross_node": {v4Auth(t, db, other.ID), nil},
		"unknown_intent": {auth, func(in *DeploymentResultInputs) {
			in.DeploymentID = "dp_missing"
			in.RequestID = DeploymentRequestID("dp_missing")
		}},
		"wrong_request":    {auth, func(in *DeploymentResultInputs) { in.RequestID = "dep_other" }},
		"wrong_digest":     {auth, func(in *DeploymentResultInputs) { in.Digest = "sha256:other" }},
		"wrong_capability": {auth, func(in *DeploymentResultInputs) { in.Capability = "other" }},
		"bad_state":        {auth, func(in *DeploymentResultInputs) { in.State = DeploymentPending }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := deployResultInputs(nsID, cap, dep, DeploymentApplied, "", 1000)
			if tc.mut != nil {
				tc.mut(&in)
			}
			if err := db.MarkDeployResultAuthorized(ctx, tc.auth, in); err == nil {
				t.Fatalf("%s 被接受了", name)
			}
			got, err := db.GetDeployment(ctx, nsID, dep.ID)
			if err != nil || got.State != DeploymentPending || got.UpdatedAt != dep.UpdatedAt {
				t.Fatalf("%s 改动了 intent: %+v %v", name, got, err)
			}
		})
	}

	// 撤销 namespace：当前凭证有效也 0 写。
	putV4NodeCfg(t, db, node.ID, []string{"other"}, nil, nil)
	if err := db.MarkDeployResultAuthorized(ctx, auth, deployResultInputs(nsID, cap, dep, DeploymentApplied, "", 1000)); !errors.Is(err, ErrEmitNamespaceDenied) {
		t.Fatalf("撤销 ns 后应 ErrEmitNamespaceDenied，得到 %v", err)
	}
	got, _ := db.GetDeployment(ctx, nsID, dep.ID)
	if got.State != DeploymentPending {
		t.Fatalf("撤销 ns 的 receipt 落库了: %+v", got)
	}
}

// TestMarkDeployResultAuthorizedSupersededIntent 被 supersede 的旧 intent：旧 receipt 不能改当前期望。
func TestMarkDeployResultAuthorizedSupersededIntent(t *testing.T) {
	db, nsID, cap, node, old := deployEnv(t)
	ctx := context.Background()
	auth := v4Auth(t, db, node.ID)

	// 旧 intent 被撤销（supersede 的落库形态是「旧行不再存在」）+ 新 intent 建立。
	if _, err := db.DeleteDeployment(ctx, nsID, old.ID); err != nil {
		t.Fatalf("drop old intent: %v", err)
	}
	cap2 := v4CapWith(t, db, &CapabilityVersion{
		NamespaceID: nsID, Name: cap.Name, Version: 2, Spec: v4Spec("", nil, "#!/bin/sh\nexit 7\n"),
	})
	if cap2.Digest == cap.Digest {
		t.Fatal("新版本 digest 必须不同（否则测的不是 supersede）")
	}
	fresh, err := db.CreateDeployments(ctx, nsID, cap2.ID, cap2.Digest, []string{node.ID})
	if err != nil || len(fresh) != 1 || fresh[0].ID == old.ID {
		t.Fatalf("supersede 新 intent: %+v %v", fresh, err)
	}
	if _, err := db.GetDeployment(ctx, nsID, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧 intent 应已被删除，得到 %v", err)
	}
	if err := db.MarkDeployResultAuthorized(ctx, auth, deployResultInputs(nsID, cap, old, DeploymentApplied, "", 1000)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧 intent receipt 应 ErrNotFound，得到 %v", err)
	}
	cur, err := db.GetDeployment(ctx, nsID, fresh[0].ID)
	if err != nil || cur.State != DeploymentPending {
		t.Fatalf("旧 receipt 改动了当前 intent: %+v %v", cur, err)
	}
}
