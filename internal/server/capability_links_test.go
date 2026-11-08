package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件覆盖 POST /v1/capabilities 带 `links` 的**原子**路径（HTTP 层）。
//
// 三件事必须同时成立，否则接口就退化成「两步流程 + 前端补丁」：
//  1. 全合法 → 能力、产物、全部边一次出现（响应里能看到 links）
//  2. 有一条边不合法 → 400，body 带 `links[i]` 下标，且**能力根本没建出来**
//  3. 升级闸门的语义不变：破坏性变更仍然 409 upgrade_breaking + upgradeImpact

// runScriptLinks 是带 run 的最小合法脚本（shebang 是硬要求）。
const runScriptLinks = "#!/bin/sh\nset -eu\necho done\n"

// postCapabilityLinks 发一次带 links 的 POST /v1/capabilities。
func postCapabilityLinks(t *testing.T, env *consoleEnv, body map[string]any) *decodeResult {
	t.Helper()
	// withMinimalRun：P1-b2 起建能力**必须**带实现（缺 run → 400 impl_missing）。
	// 这一层补的是「造一个合法请求」，用例自己的意图（links / 闸门）
	// 不受影响；显式写了 run 的 body 原样使用。
	rr := env.do(t, http.MethodPost, "/v1/capabilities", withMinimalRun(body))
	return &decodeResult{code: rr.Code, body: rr.Body.String()}
}

// decodeResult 是一次响应的最小快照（status + 原始 body）。
type decodeResult struct {
	code int
	body string
}

// json 把 body 解成 map（测试断言用）。
func (r *decodeResult) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, r.body)
	}
	return m
}

// countIn 数一件事在库里发生了几次（失败即 Fatal）。
func countIn(t *testing.T, env *consoleEnv, query string, args ...any) int {
	t.Helper()
	var n int
	if err := env.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestPostCapabilityWithLinksCreatesAtomically：能力 + 产物 + 2 条边一次提交。
func TestPostCapabilityWithLinksCreatesAtomically(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "notify.send", 1, nil)
	env.seedCapability(t, "archive.logs", 1, nil)

	res := postCapabilityLinks(t, env, map[string]any{
		"namespaceId":  env.nsID,
		"name":         "chain.atomic",
		"outputs":      []string{"done", "failed"},
		"paramsSchema": map[string]any{"type": "object"},
		"run": map[string]any{
			"entrypoint": "run.sh",
			"files":      map[string]string{"run.sh": runScriptLinks},
		},
		"links": []map[string]any{
			{"fromPort": "done", "toRef": "notify.send"},
			{"fromPort": "failed", "toRef": "archive.logs"},
		},
	})
	if res.code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", res.code, res.body)
	}
	m := res.json(t)
	capObj, _ := m["capability"].(map[string]any)
	if capObj == nil || capObj["name"] != "chain.atomic" {
		t.Fatalf("响应缺 capability: %v", m)
	}
	capID, _ := capObj["id"].(string)

	// links：现有 edge 视图（两端引用 + 此刻解析到的能力 id）
	links, _ := m["links"].([]any)
	if len(links) != 2 {
		t.Fatalf("响应 links = %v, want 2 条", m["links"])
	}
	first := links[0].(map[string]any)
	if first["fromRef"] != "chain.atomic" || first["fromPort"] != "done" || first["toRef"] != "notify.send" {
		t.Fatalf("第一条边形状不对: %v", first)
	}
	if first["fromIsSource"] != false {
		t.Fatalf("fromIsSource = %v, want false（能力边）", first["fromIsSource"])
	}
	// 派生字段：两端 ref 此刻解析到的能力 id（前端靠它点进详情）
	if id, _ := first["fromCapabilityId"].(string); id != capID {
		t.Fatalf("fromCapabilityId = %q, want %q", id, capID)
	}
	if id, _ := first["toCapabilityId"].(string); id == "" {
		t.Fatalf("toCapabilityId 为空（notify.send 应该解析得到）: %v", first)
	}
	if links[1].(map[string]any)["toRef"] != "archive.logs" {
		t.Fatalf("第二条边 toRef = %v, want archive.logs", links[1])
	}

	// upgradeImpact 恒在（空影响面也是对象，不是 null）
	impact, _ := m["upgradeImpact"].(map[string]any)
	if impact == nil || impact["breaking"] != false {
		t.Fatalf("upgradeImpact = %v, want breaking=false", m["upgradeImpact"])
	}
	// 产物也进了同一个事务
	if digest, _ := m["digest"].(string); !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("响应里没有 digest: %v", m)
	}
	// 产物不再是独立的行：能力行带正文 = 有产物（Capability Spec v3 §11）
	if n := countIn(t, env,
		`SELECT count(*) FROM capability_versions WHERE namespace_id = ? AND id = ? AND spec <> ''`,
		env.nsID, capID); n != 1 {
		t.Fatalf("带正文的能力行数 = %d, want 1", n)
	}
	// 边真的落库了（两条）
	if n := countIn(t, env, `SELECT count(*) FROM edges WHERE namespace_id = ? AND from_ref = ?`,
		env.nsID, "chain.atomic"); n != 2 {
		t.Fatalf("edges 行数 = %d, want 2", n)
	}
}

// TestPostCapabilityWithLinks400PointsAtBadIndex：坏边 → 400 + `links[i]` + 能力没建出来。
func TestPostCapabilityWithLinks400PointsAtBadIndex(t *testing.T) {
	cases := []struct {
		name    string
		links   []map[string]any
		wantIdx string
	}{
		{
			name:    "第一条的目标不存在",
			links:   []map[string]any{{"fromPort": "done", "toRef": "ghost.capability"}},
			wantIdx: "links[0]",
		},
		{
			name: "第二条的目标不存在",
			links: []map[string]any{
				{"fromPort": "done", "toRef": "notify.send"},
				{"fromPort": "failed", "toRef": "ghost.capability"},
			},
			wantIdx: "links[1]",
		},
		{
			// 自连/成环现在**合法**（契约 §2：环是合法编排），所以这一条换成
			// 另一种客户端可修的硬拒：端口不在 outputs 里。
			name:    "端口不在 outputs 里",
			links:   []map[string]any{{"fromPort": "not-a-port", "toRef": "notify.send"}},
			wantIdx: "links[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConsoleEnv(t)
			env.seedCapability(t, "notify.send", 1, nil)
			// 基线：回滚断言用「与提交前完全一致」判，而不是「表是空的」——
			// 命名空间建行时播种的内置能力/产物本来就在表里。
			baseCaps := countIn(t, env, `SELECT count(*) FROM capability_versions`)
			baseEdges := countIn(t, env, `SELECT count(*) FROM edges`)
			// 「产物」= 能力行的正文（P2 删了 artifacts 表）
			baseBodies := countIn(t, env, `SELECT count(*) FROM capability_versions WHERE spec <> ''`)

			res := postCapabilityLinks(t, env, map[string]any{
				"namespaceId": env.nsID,
				"name":        "chain.bad",
				"outputs":     []string{"done", "failed"},
				"run": map[string]any{
					"entrypoint": "run.sh",
					"files":      map[string]string{"run.sh": runScriptLinks},
				},
				"links": tc.links,
			})
			if res.code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400（body=%s）", res.code, res.body)
			}
			m := res.json(t)
			if m["code"] != "edge_rejected" {
				t.Fatalf("code = %v, want edge_rejected（body=%s）", m["code"], res.body)
			}
			errText, _ := m["error"].(string)
			if !strings.Contains(errText, tc.wantIdx) {
				t.Fatalf("400 的 error 里没有下标 %q: %q", tc.wantIdx, errText)
			}

			// 目标语义：**什么都没建**（能力 / 边 / 产物 三个面）
			rr := env.do(t, http.MethodGet, env.withNS("/v1/capabilities"), nil)
			caps, _ := decode(t, rr)["capabilities"].([]any)
			for _, raw := range caps {
				if c, _ := raw.(map[string]any); c["name"] == "chain.bad" {
					t.Fatalf("坏边回滚了却没回滚能力：列表里还有 %v", c)
				}
			}
			if n := countIn(t, env, `SELECT count(*) FROM capability_versions`); n != baseCaps {
				t.Fatalf("能力行数 %d → %d，回滚失败", baseCaps, n)
			}
			if n := countIn(t, env, `SELECT count(*) FROM edges`); n != baseEdges {
				t.Fatalf("边行数 %d → %d，回滚失败", baseEdges, n)
			}
			if n := countIn(t, env, `SELECT count(*) FROM capability_versions WHERE spec <> ''`); n != baseBodies {
				t.Fatalf("带正文的能力行数 %d → %d，回滚失败", baseBodies, n)
			}
		})
	}
}

// TestPostCapabilityWithLinksCrossNamespace400：跨命名空间的目标 → 400，且**仍然带下标**。
//
// 这条错误在 store 里是 *CrossNamespaceError（不是 ErrEdgeRejected），
// handler 必须先看它、把带下标的整句写出去，否则前端就只剩一句「跨命名空间引用」，
// 不知道是几条里的哪一条。
func TestPostCapabilityWithLinksCrossNamespace400(t *testing.T) {
	env := newConsoleEnv(t)
	other := env.seedNamespace(t, "other-ns", "另一个命名空间")
	env.seedCapabilityIn(t, other.ID, "other.target", 1, nil)

	res := postCapabilityLinks(t, env, map[string]any{
		"namespaceId": env.nsID,
		"name":        "chain.cross",
		"outputs":     []string{"done"},
		"links":       []map[string]any{{"fromPort": "done", "toRef": "other.target"}},
	})
	if res.code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", res.code, res.body)
	}
	m := res.json(t)
	errText, _ := m["error"].(string)
	if !strings.Contains(errText, "links[0]") {
		t.Fatalf("400 的 error 里没有下标: %q", errText)
	}
	if !strings.Contains(strings.ToLower(errText), "namespace") {
		t.Fatalf("400 的 error 里没说清是跨命名空间: %q", errText)
	}
	// 整体回滚
	if n := countIn(t, env, `SELECT count(*) FROM capability_versions WHERE namespace_id = ? AND name = ?`,
		env.nsID, "chain.cross"); n != 0 {
		t.Fatalf("跨命名空间被拒却写入了能力行: %d", n)
	}
}

// TestPostCapabilityWithLinksUpgradeGateStill409：带 links 不改变闸门语义。
func TestPostCapabilityWithLinksUpgradeGateStill409(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	// v1：出口 done + failed；一条跟随 latest 的 link 接在 failed 上
	v1 := &store.CapabilityVersion{
		NamespaceID: env.nsID,
		Name:        "fixer",
		Version:     1,
		ParamsSchema: json.RawMessage(
			`{"type":"object","required":["issue"],"properties":{"issue":{"type":"string"}}}`),
		Outputs: []string{"done", "failed"},
		Source:  store.CapSourceCustom,
	}
	if err := env.db.CreateCapability(ctx, withSpec(v1)); err != nil {
		t.Fatalf("create fixer@1: %v", err)
	}
	env.seedCapability(t, "review", 1, []string{"fix"})
	env.seedCapability(t, "notify.send", 1, nil)
	if err := env.db.CreateEdge(ctx, &store.Edge{
		NamespaceID: env.nsID, FromRef: "review", FromPort: "fix", ToRef: "fixer",
	}); err != nil {
		t.Fatalf("create link: %v", err)
	}

	// v2 新增**必填**入参 note → 破坏性变更（接在 fixer 上的 name 型 link 会拿到新契约）
	res := postCapabilityLinks(t, env, map[string]any{
		"namespaceId": env.nsID,
		"name":        "fixer",
		"outputs":     []string{"done", "failed"},
		"paramsSchema": map[string]any{
			"type":     "object",
			"required": []string{"issue", "note"},
			"properties": map[string]any{
				"issue": map[string]any{"type": "string"},
				"note":  map[string]any{"type": "string"},
			},
		},
		"links": []map[string]any{{"fromPort": "done", "toRef": "notify.send"}},
	})
	if res.code != http.StatusConflict {
		t.Fatalf("status = %d, want 409（body=%s）", res.code, res.body)
	}
	m := res.json(t)
	if m["code"] != "upgrade_breaking" {
		t.Fatalf("code = %v, want upgrade_breaking", m["code"])
	}
	impact, _ := m["upgradeImpact"].(map[string]any)
	if impact == nil || impact["breaking"] != true {
		t.Fatalf("409 必须带 breaking 的 upgradeImpact: %v", m)
	}
	if links, _ := impact["links"].([]any); len(links) != 1 {
		t.Fatalf("受影响 link 数 = %d, want 1（review --fix--> fixer）", len(links))
	}
	// 整体没写：没有 v2、没有新边
	if _, err := env.db.GetCapability(ctx, env.nsID, "fixer", 2); err == nil {
		t.Fatal("闸门拒了却还是写进了 fixer@2")
	}
	if n := countIn(t, env, `SELECT count(*) FROM edges WHERE namespace_id = ? AND from_ref = ?`,
		env.nsID, "fixer"); n != 0 {
		t.Fatalf("闸门拒了却还是写进了 fixer 的边（%d 条）", n)
	}
}

// TestPostCapabilityWithoutLinksLegacyShape：不带 links = 今天的行为（响应里没有 links 键）。
//
// 向后兼容是硬约束：老客户端（以及控制台的「+ 下游」按钮）还在用两步流程。
func TestPostCapabilityWithoutLinksLegacyShape(t *testing.T) {
	env := newConsoleEnv(t)
	rr := env.do(t, http.MethodPost, "/v1/capabilities", withMinimalRun(map[string]any{
		"namespaceId": env.nsID,
		"name":        "legacy.shape",
		"outputs":     []string{"done"},
	}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	if _, has := m["links"]; has {
		t.Fatalf("不带 links 的请求不该多出 links 键（一个字节都不变）: %v", m)
	}
	if n := countIn(t, env, `SELECT count(*) FROM edges WHERE namespace_id = ?`, env.nsID); n != 0 {
		t.Fatalf("不带 links 却建了边: %d", n)
	}
}
