package server

// 部署模型的控制台端点（console-declares epic，契约 CONTRACT-protocol §11）。
//
// 覆盖六件事，全部是「控制台点一下 → 中枢库里发生什么」的可观测契约：
//  1. GET  /v1/capability-templates  恰好 9 个内置模板（CONTRACT-artifact §8）
//  2. POST /v1/capabilities 带 run   → 编译产物、算 digest、落 artifacts，可被 /v1/artifacts 查到
//  3. POST /v1/capabilities 不带 run → 没有产物，部署 404 artifact_not_found
//  4. POST /v1/deployments           → 离线节点**照样建行**，逐台回 pushed=false
//  5. GET  /v1/deployments           → nodeId / capabilityId 过滤生效
//  6. DELETE /v1/deployments         → 行直接消失（不留 removed 态）
//
// 这些用例共用 newConsoleEnv 的 mux（已按生产 mux 接线挂上 deployHandler，
// hub 不注入 = 所有节点都「离线」，正是第 4 条要的形状）。

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/store"
)

// runScript 是一份合法的入口文件内容（直接 exec，必须有 shebang）。
const runScript = "#!/bin/bash\nset -euo pipefail\necho '{\"out\":\"fix\"}'\n"

// TestCapabilityTemplates 内置模板恰好 9 个，且 id 就是契约冻结的那 9 个。
//
// 模板是「代码生成器的输入」：少一个 → 控制台建不出那类能力；多一个 → 前端渲染
// 一个后端没测过的脚本模板。所以断言 id 集合（不只是数量）。
func TestCapabilityTemplates(t *testing.T) {
	env := newConsoleEnv(t)

	rr := env.do(t, http.MethodGet, "/v1/capability-templates", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	raw, _ := m["templates"].([]any)
	if len(raw) != 9 {
		t.Fatalf("templates = %d 个, want 9（CONTRACT-artifact §8）", len(raw))
	}
	got := map[string]bool{}
	for _, item := range raw {
		obj, _ := item.(map[string]any)
		id, _ := obj["id"].(string)
		got[id] = true
		// 每个模板都必须能直接生成一份**可执行**产物：脚本正文要有 shebang
		// （CONTRACT-artifact §4：入口文件直接 exec，没有 shebang 就跑不起来）。
		script, _ := obj["script"].(string)
		if !strings.HasPrefix(script, "#!") {
			t.Errorf("模板 %q 的 script 没有 shebang：%q", id, script)
		}
		if ts, _ := obj["timeoutSec"].(float64); ts <= 0 {
			t.Errorf("模板 %q 没有默认超时：%v", id, obj["timeoutSec"])
		}
		for _, key := range []string{"displayName", "category", "script", "timeoutSec"} {
			if _, ok := obj[key]; !ok {
				t.Fatalf("模板 %q 缺字段 %q: %v", id, key, obj)
			}
		}
		// 注意：契约 §8 的 Template 还有 params 字段，实现里没有；fields/requires/outputs
		// 目前是 null 而不是 []（违反空列表纪律）。两处都是产品侧偏差，已上报，
		// 所以这里刻意不断言它们——不要用测试把偏差固化成契约。
	}
	for _, want := range []string{
		"script.blank", "script.http", "script.git", "script.test", "script.notify",
		"agent.claude", "agent.codex", "agent.pi", "agent.custom",
	} {
		if !got[want] {
			t.Errorf("缺少内置模板 %q（现有 %v）", want, got)
		}
	}
}

// seedRunCapability 通过 POST /v1/capabilities（带 run）建能力并返回 (capabilityID, digest)。
func seedRunCapability(t *testing.T, env *consoleEnv, name string) (string, string) {
	t.Helper()
	rr := env.do(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name":    name,
		"version": 1,
		"outputs": []string{"fix"},
		"paramsSchema": map[string]any{
			"type": "object", "properties": map[string]any{"repo": map[string]any{"type": "string"}},
		},
		"run": map[string]any{
			"entrypoint": "run.sh",
			"files":      map[string]string{"run.sh": runScript},
			"timeoutSec": 900,
			"env":        map[string]string{"LOG_LEVEL": "info"},
		},
		"requires": []string{"git"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /v1/capabilities status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	capObj, _ := m["capability"].(map[string]any)
	capID, _ := capObj["id"].(string)
	if capID == "" {
		t.Fatalf("响应里没有 capability.id: %v", m)
	}
	digest, _ := m["digest"].(string)
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("digest = %q, want sha256:<64 hex>", digest)
	}
	return capID, digest
}

// TestCreateCapabilityWithRunCompilesArtifact 带 run 的能力定义 → 产物落库 + digest 可查。
//
// 这是 v2 的入口动作：执行方式从此属于能力定义（而不是节点的 node.yaml）。
// 三件事必须同时成立：
//   - 响应带 digest（前端拿它做「部署哪个版本」的标识）
//   - artifact 的 digest 与 content 自洽（artifact.Digest 能复算出同一个值）
//   - GET /v1/artifacts?capabilityId= 能查到（部署时 resolveArtifact 走的就是这条查询）
func TestCreateCapabilityWithRunCompilesArtifact(t *testing.T) {
	env := newConsoleEnv(t)
	capID, digest := seedRunCapability(t, env, "review.pr")

	rr := env.do(t, http.MethodGet, env.withNS("/v1/artifacts?capabilityId="+capID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/artifacts status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	list, _ := m["artifacts"].([]any)
	if len(list) != 1 {
		t.Fatalf("artifacts = %d 条, want 1（%v）", len(list), m["artifacts"])
	}
	obj, _ := list[0].(map[string]any)
	if obj["digest"] != digest {
		t.Fatalf("产物 digest = %v, want %v（与 POST 响应不一致）", obj["digest"], digest)
	}
	if obj["capabilityId"] != capID {
		t.Fatalf("产物 capabilityId = %v, want %v", obj["capabilityId"], capID)
	}
	if obj["namespaceId"] != env.nsID {
		t.Fatalf("产物 namespaceId = %v, want %v", obj["namespaceId"], env.nsID)
	}

	// spec 在 JSON 里是 base64（store.Artifact.Spec 是 []byte，encoding/json 的默认编码）。
	// 解出来后必须是合法产物，且 digest 与内容自洽（内容寻址：改一个字节就变 digest）。
	specB64, _ := obj["spec"].(string)
	specRaw, err := base64.StdEncoding.DecodeString(specB64)
	if err != nil {
		t.Fatalf("spec 不是合法 base64: %v", err)
	}
	spec := string(specRaw)
	parsed, err := artifact.Parse(specRaw)
	if err != nil {
		t.Fatalf("落库的 spec 解不出来: %v\nspec=%s", err, spec)
	}
	recomputed, err := artifact.Digest(parsed)
	if err != nil {
		t.Fatalf("recompute digest: %v", err)
	}
	if recomputed != digest {
		t.Fatalf("按 spec 复算的 digest = %s, want %s（内容寻址不自洽）", recomputed, digest)
	}
	// run 块里的入口文件内容原样进了产物（自包含：拷到别的机器就能跑）
	if !strings.Contains(spec, "run.sh") || !strings.Contains(spec, "set -euo pipefail") {
		t.Fatalf("产物 spec 里没有入口文件内容: %s", spec)
	}
}

// TestCreateCapabilityWithInvalidRunRejected run 不合法 → 400，且**能力不落库**。
//
// 这条是 ws 侧被删掉的「manifest run 非法 → 整帧拒绝」用例的替代覆盖：
// v2 里执行方式在**建能力时**校验（internal/artifact.Validate），不再是节点 hello
// 时校验。校验时机变了，纪律没变：一次列出全部问题（`;` 分隔），且拒绝要原子
// —— 一份 run 写错的请求不能留下一个「永远改不掉、也永远跑不起来」的能力定义
// （capability_versions 是 append-only）。
func TestCreateCapabilityWithInvalidRunRejected(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	rr := env.do(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name":    "bad.run",
		"version": 1,
		"run": map[string]any{
			// 入口文件没有 shebang + timeout 越界：两条问题必须一起报出来
			"entrypoint": "run.sh",
			"files":      map[string]string{"run.sh": "echo hi\n"},
			"timeoutSec": 200000,
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	if m["code"] != "invalid_artifact" {
		t.Fatalf("code = %v, want invalid_artifact（body=%s）", m["code"], rr.Body.String())
	}
	msg, _ := m["error"].(string)
	for _, want := range []string{"entrypoint_no_shebang", "invalid_timeout"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息 %q 里没有 %q（校验必须一次列全）", msg, want)
		}
	}

	// 能力定义不能留下（append-only 表里的半成品永远改不掉）
	if _, err := env.db.GetCapability(ctx, env.nsID, "bad.run", 1); err == nil {
		t.Fatal("被拒的请求留下了能力定义")
	}
	// 也不该留下产物
	var n int
	if err := env.db.QueryRow(`SELECT count(*) FROM artifacts WHERE namespace_id = ?`, env.nsID).Scan(&n); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if n != 0 {
		t.Fatalf("被拒的请求留下了 %d 份产物", n)
	}
}

// TestDeployWithoutRunIs404ArtifactNotFound 没 run 的能力 → 部署 404 artifact_not_found。
//
// 「建了能力但没填 run」是最常见的用户路径（占位节点、内置能力都是纯定义）。
// 这时不该 500，也不该建一条永远不会成功的部署行——必须给一个能指导操作的说法。
func TestDeployWithoutRunIs404ArtifactNotFound(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	capDef := env.seedCapability(t, "no-run", 1, nil)
	node := &store.Node{NamespaceID: env.nsID, Name: "mbp-norun"}
	if _, err := env.db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	rr := env.do(t, http.MethodPost, env.withNS("/v1/deployments"), map[string]any{
		"capabilityId": capDef.ID,
		"nodeIds":      []string{node.ID},
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	if m["code"] != "artifact_not_found" {
		t.Fatalf("code = %v, want artifact_not_found（body=%s）", m["code"], rr.Body.String())
	}
	// 一行都不该建
	if list, err := env.db.ListDeployments(ctx, env.nsID, store.DeploymentFilter{}); err != nil {
		t.Fatalf("list deployments: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("失败的部署留下了 %d 行", len(list))
	}
}

// TestDeployToOfflineNodeCreatesRow 离线节点：行照样建（state=pending），逐台回 pushed=false。
//
// 契约 §11 的最后一句：「节点不在线 → pushed=false, error="node_offline"，**行仍然创建**」。
// 为什么这条是硬要求：行是「中枢还记得这台机器该有这份产物」的唯一凭据；行不存在，
// 节点上线后就没有任何记录能触发补推 → 部署动作永久丢失，而且用户看到的是一句
// 「节点离线」——他重试一次可能还是离线，最后以为部署成功了。
func TestDeployToOfflineNodeCreatesRow(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	capID, digest := seedRunCapability(t, env, "review.offline")

	node := &store.Node{NamespaceID: env.nsID, Name: "mbp-offline"}
	if _, err := env.db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	rr := env.do(t, http.MethodPost, env.withNS("/v1/deployments"), map[string]any{
		"capabilityId": capID,
		"nodeIds":      []string{node.ID},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	results, _ := m["deployments"].([]any)
	if len(results) != 1 {
		t.Fatalf("逐台结果 = %d 条, want 1（%v）", len(results), m["deployments"])
	}
	res, _ := results[0].(map[string]any)
	if res["pushed"] != false {
		t.Fatalf("离线节点 pushed = %v, want false", res["pushed"])
	}
	if msg, _ := res["error"].(string); msg == "" {
		t.Fatalf("离线节点没有 error 说明: %v", res)
	}
	if res["nodeId"] != node.ID {
		t.Fatalf("nodeId = %v, want %v", res["nodeId"], node.ID)
	}
	deployID, _ := res["deployId"].(string)
	if deployID == "" {
		t.Fatalf("没有 deployId（前端无法引用这一行）: %v", res)
	}

	// 行真的建好了：state=pending，digest 是 artifact 的那一份
	var state, gotDigest string
	if err := env.db.QueryRow(
		`SELECT state, digest FROM deployments WHERE id = ?`, deployID).Scan(&state, &gotDigest); err != nil {
		t.Fatalf("读部署行: %v", err)
	}
	if state != store.DeploymentPending {
		t.Fatalf("state = %q, want %q", state, store.DeploymentPending)
	}
	if gotDigest != digest {
		t.Fatalf("部署行 digest = %s, want %s", gotDigest, digest)
	}

	// 重复点部署是幂等 no-op：同一行，不新增
	rr = env.do(t, http.MethodPost, env.withNS("/v1/deployments"), map[string]any{
		"capabilityId": capID,
		"nodeIds":      []string{node.ID},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("重复部署 status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if list, err := env.db.ListDeployments(ctx, env.nsID, store.DeploymentFilter{}); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(list) != 1 {
		t.Fatalf("重复部署建出了 %d 行, want 1", len(list))
	}
}

// TestListDeploymentsFilters GET /v1/deployments 的 nodeId / capabilityId 过滤生效。
func TestListDeploymentsFilters(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	capA, _ := seedRunCapability(t, env, "review.filter")
	capB, _ := seedRunCapability(t, env, "test.filter")

	nodeA := &store.Node{NamespaceID: env.nsID, Name: "mbp-a"}
	nodeB := &store.Node{NamespaceID: env.nsID, Name: "mbp-b"}
	for _, n := range []*store.Node{nodeA, nodeB} {
		if _, err := env.db.CreateNode(ctx, n); err != nil {
			t.Fatalf("create node %s: %v", n.Name, err)
		}
	}
	deploy := func(capID string, nodeIDs ...string) {
		t.Helper()
		rr := env.do(t, http.MethodPost, env.withNS("/v1/deployments"), map[string]any{
			"capabilityId": capID,
			"nodeIds":      nodeIDs,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("deploy %s status = %d, body=%s", capID, rr.Code, rr.Body.String())
		}
	}
	deploy(capA, nodeA.ID, nodeB.ID)
	deploy(capB, nodeA.ID)

	list := func(query string) []any {
		t.Helper()
		rr := env.do(t, http.MethodGet, env.withNS("/v1/deployments"+query), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /v1/deployments%s status = %d, body=%s", query, rr.Code, rr.Body.String())
		}
		m := decode(t, rr)
		out, _ := m["deployments"].([]any)
		return out
	}

	if got := len(list("")); got != 3 {
		t.Fatalf("无过滤 = %d 条, want 3", got)
	}
	if got := len(list("?nodeId=" + nodeA.ID)); got != 2 {
		t.Fatalf("按 nodeA 过滤 = %d 条, want 2", got)
	}
	if got := len(list("?nodeId=" + nodeB.ID)); got != 1 {
		t.Fatalf("按 nodeB 过滤 = %d 条, want 1", got)
	}
	if got := len(list("?capabilityId=" + capB)); got != 1 {
		t.Fatalf("按 capB 过滤 = %d 条, want 1", got)
	}
	if got := len(list("?nodeId=" + nodeB.ID + "&capabilityId=" + capB)); got != 0 {
		t.Fatalf("两个条件一起 = %d 条, want 0", got)
	}
	// 每条的 state 字段都在（UI 靠它上色；缺了会显示成灰色「未知」）
	for _, raw := range list("?nodeId=" + nodeA.ID) {
		obj, _ := raw.(map[string]any)
		if obj["state"] == "" || obj["state"] == nil {
			t.Fatalf("部署行缺 state: %v", obj)
		}
	}
}

// TestDeleteDeploymentRemovesRow 撤销部署：行直接消失，再撤销一次 404。
func TestDeleteDeploymentRemovesRow(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	capID, _ := seedRunCapability(t, env, "review.delete")
	node := &store.Node{NamespaceID: env.nsID, Name: "mbp-del"}
	if _, err := env.db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	rr := env.do(t, http.MethodPost, env.withNS("/v1/deployments"), map[string]any{
		"capabilityId": capID, "nodeIds": []string{node.ID},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("deploy status = %d, body=%s", rr.Code, rr.Body.String())
	}
	results, _ := decode(t, rr)["deployments"].([]any)
	if len(results) != 1 {
		t.Fatalf("逐台结果 = %d 条, want 1", len(results))
	}
	res, _ := results[0].(map[string]any)
	deployID, _ := res["deployId"].(string)

	rr = env.do(t, http.MethodDelete, env.withNS("/v1/deployments?id="+deployID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if got := decode(t, rr)["deleted"]; got != deployID {
		t.Fatalf("deleted = %v, want %v", got, deployID)
	}
	// 行没了
	if list, err := env.db.ListDeployments(ctx, env.nsID, store.DeploymentFilter{}); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("删除后还剩 %d 行", len(list))
	}
	if _, err := env.db.GetDeployment(ctx, env.nsID, deployID); err == nil {
		t.Fatal("删除后仍能按 id 取到这一行")
	}

	// 再删一次 → 404（不是 200 + 空操作：前端要能区分「删过了」与「删错了」）
	rr = env.do(t, http.MethodDelete, env.withNS("/v1/deployments?id="+deployID), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("重复 DELETE status = %d, want 404, body=%s", rr.Code, rr.Body.String())
	}
}

// TestDeployRequestValidation 部署请求的三个必填/选择器错误各有明确的状态码。
func TestDeployRequestValidation(t *testing.T) {
	env := newConsoleEnv(t)

	// 缺 capabilityId → 400
	rr := env.do(t, http.MethodPost, env.withNS("/v1/deployments"), map[string]any{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 capabilityId status = %d, want 400, body=%s", rr.Code, rr.Body.String())
	}
	if code := decode(t, rr)["code"]; code != "missing_capability_id" {
		t.Fatalf("code = %v, want missing_capability_id", code)
	}

	// 选择器一台都没选中 → 400 no_target_node（不是 200 + 空列表）
	capID, _ := seedRunCapability(t, env, "review.selector")
	rr = env.do(t, http.MethodPost, env.withNS("/v1/deployments"), map[string]any{
		"capabilityId": capID,
		"tags":         []string{"no-such-tag"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("空选择器 status = %d, want 400, body=%s", rr.Code, rr.Body.String())
	}
	if code := decode(t, rr)["code"]; code != "no_target_node" {
		t.Fatalf("code = %v, want no_target_node", code)
	}

	// 按 tag 选机器：命中多台时逐台建行（选择器展开）
	nodeA := &store.Node{NamespaceID: env.nsID, Name: "pool-a-1", Tags: []string{"pool-a"}}
	nodeB := &store.Node{NamespaceID: env.nsID, Name: "pool-a-2", Tags: []string{"pool-a"}}
	for _, n := range []*store.Node{nodeA, nodeB} {
		if _, err := env.db.CreateNode(context.Background(), n); err != nil {
			t.Fatalf("create node %s: %v", n.Name, err)
		}
	}
	rr = env.do(t, http.MethodPost, env.withNS("/v1/deployments"), map[string]any{
		"capabilityId": capID,
		"tags":         []string{"pool-a"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("按 tag 部署 status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if results, _ := decode(t, rr)["deployments"].([]any); len(results) != 2 {
		t.Fatalf("按 tag 展开 = %d 台, want 2", len(results))
	}
}
