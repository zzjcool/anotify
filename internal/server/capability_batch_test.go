package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// 本文件覆盖 POST /v1/capabilities/batch（契约 §4）的 HTTP 行为。
//
// 四件事必须同时成立，否则「一个表单建多个能力」就退化成前端补丁：
//  1. 全合法 → 能力 / 产物 / 边三个面一次出现，且边可以引用**本批新建**的名字
//  2. 任何一步失败 → **整批**回滚（能力 0 条、产物 0 条、边 0 条）
//  3. 批内 name 重复 → 400（边按名字引用，重名无法表达「指向哪一个」）
//  4. 闸门语义与单建一致：任一能力破坏性 → 整批 409 + upgradeImpact，force 越过
//
// 另有一条**等价性**用例：同样内容分别走单建与批量，落库结果必须一致
// （能力版本 / content_hash / 产物 digest / 边集合）。批量不引入第二套语义。

// batchRunScript 是带 run 的最小合法脚本（shebang 是硬要求）。
const batchRunScript = "#!/bin/sh\nset -eu\necho done\n"

// batchRun 造一份「有实现」的 run 字段（带产物的能力才走得通部署链路）。
func batchRun() map[string]any {
	return map[string]any{
		"entrypoint": "run.sh",
		"files":      map[string]string{"run.sh": batchRunScript},
	}
}

// postCapabilityBatch 发一次 POST /v1/capabilities/batch。
func postCapabilityBatch(t *testing.T, env *consoleEnv, body map[string]any) *decodeResult {
	t.Helper()
	rr := env.do(t, http.MethodPost, "/v1/capabilities/batch", body)
	return &decodeResult{code: rr.Code, body: rr.Body.String()}
}

// asInt 把解密后的 JSON 数字读成 int（encoding/json 只有 float64）。
func asInt(t *testing.T, v any) int {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("不是数字: %v (%T)", v, v)
	}
	return int(f)
}

// TestPostCapabilityBatchCreatesAllFaces：能力 + 产物 + 边一次出现，边引用本批新建的名字。
//
// 关键点：`links[0].fromRef` 指向的是**数组里第二个**能力（batch.a）——
// 先插全部能力行、再解析边，所以批内的前向引用成立。
func TestPostCapabilityBatchCreatesAllFaces(t *testing.T) {
	env := newConsoleEnv(t)

	res := postCapabilityBatch(t, env, map[string]any{
		"namespaceId": env.nsID,
		"capabilities": []map[string]any{
			{"name": "batch.b", "displayName": "B", "outputs": []string{"done"}, "paramsSchema": map[string]any{"type": "object"}, "run": batchRun()},
			{"name": "batch.a", "outputs": []string{"fix"}, "paramsSchema": map[string]any{"type": "object"}, "run": batchRun()},
		},
		"links": []map[string]any{
			{"fromRef": "batch.a", "fromPort": "fix", "toRef": "batch.b"},
		},
	})
	if res.code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", res.code, res.body)
	}
	m := res.json(t)

	// ① 能力面：与请求同序，且都是权威行（带 id / version）
	caps, _ := m["capabilities"].([]any)
	if len(caps) != 2 {
		t.Fatalf("capabilities = %v, want 2 项", m["capabilities"])
	}
	first, _ := caps[0].(map[string]any)
	second, _ := caps[1].(map[string]any)
	if first["name"] != "batch.b" || second["name"] != "batch.a" {
		t.Fatalf("capabilities 顺序/名字不对: %v / %v", first["name"], second["name"])
	}
	if asInt(t, first["version"]) != 1 || asInt(t, second["version"]) != 1 {
		t.Fatalf("版本 = %v / %v, want 1 / 1", first["version"], second["version"])
	}
	firstID, _ := first["id"].(string)
	secondID, _ := second["id"].(string)
	if firstID == "" || secondID == "" {
		t.Fatalf("能力行缺 id: %v / %v", firstID, secondID)
	}
	if first["displayName"] != "B" {
		t.Fatalf("displayName = %v, want B（元素字段与单建同一套口径）", first["displayName"])
	}

	// ② 边面：现有 edge 视图（两端引用 + 此刻解析到的能力 id）
	edges, _ := m["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("edges = %v, want 1 条", m["edges"])
	}
	edge, _ := edges[0].(map[string]any)
	if edge["fromRef"] != "batch.a" || edge["fromPort"] != "fix" || edge["toRef"] != "batch.b" {
		t.Fatalf("边形状不对: %v", edge)
	}
	if edge["fromIsSource"] != false {
		t.Fatalf("fromIsSource = %v, want false（能力边）", edge["fromIsSource"])
	}
	if id, _ := edge["fromCapabilityId"].(string); id != secondID {
		t.Fatalf("fromCapabilityId = %q, want %q（batch.a）", id, secondID)
	}
	if id, _ := edge["toCapabilityId"].(string); id != firstID {
		t.Fatalf("toCapabilityId = %q, want %q（batch.b）", id, firstID)
	}

	// ③ 影响面恒在（空影响面也是对象，不是 null）
	impact, _ := m["upgradeImpact"].(map[string]any)
	if impact == nil {
		t.Fatalf("响应缺 upgradeImpact: %v", m)
	}
	if impact["breaking"] != false {
		t.Fatalf("upgradeImpact.breaking = %v, want false", impact["breaking"])
	}

	// ④ 落库三个面都对得上（产物 2 份、边 1 条）
	if n := countIn(t, env, `SELECT count(*) FROM artifacts WHERE namespace_id = ? AND capability_id IN (?,?)`,
		env.nsID, firstID, secondID); n != 2 {
		t.Fatalf("产物行数 = %d, want 2（每个带 run 的能力一份）", n)
	}
	if n := countIn(t, env, `SELECT count(*) FROM edges WHERE namespace_id = ? AND from_ref = ?`,
		env.nsID, "batch.a"); n != 1 {
		t.Fatalf("边行数 = %d, want 1", n)
	}
}

// TestPostCapabilityBatchEmptyLinksIsArray：不带 links 时 edges 是 [] 而不是 null（I9）。
func TestPostCapabilityBatchEmptyLinksIsArray(t *testing.T) {
	env := newConsoleEnv(t)
	res := postCapabilityBatch(t, env, map[string]any{
		"namespaceId": env.nsID,
		"capabilities": []map[string]any{
			{"name": "batch.solo", "outputs": []string{"done"}},
		},
	})
	if res.code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", res.code, res.body)
	}
	m := res.json(t)
	edges, ok := m["edges"].([]any)
	if !ok {
		t.Fatalf("edges = %v (%T), want []", m["edges"], m["edges"])
	}
	if len(edges) != 0 {
		t.Fatalf("edges = %v, want []", edges)
	}
}

// TestPostCapabilityBatchIdempotentReplay：同一批请求重放两次 → 200 且**不重复写**。
//
// 幂等来自三处既有实现（批量只是把它们放在同一个事务里）：能力按 content_hash 折叠、
// 产物按 (namespace, digest) 不重复插入、边按 (namespace, 左端, 端口, 右端) 引用不重复。
// 这条性质是重试安全的前提——422 之后客户端只能重发，而重发不能变成「建了两份」。
func TestPostCapabilityBatchIdempotentReplay(t *testing.T) {
	env := newConsoleEnv(t)
	body := func() map[string]any {
		return map[string]any{
			"namespaceId": env.nsID,
			"capabilities": []map[string]any{
				{"name": "replay.b", "outputs": []string{"done"}, "run": batchRun()},
				{"name": "replay.a", "outputs": []string{"fix"}, "run": batchRun()},
			},
			"links": []map[string]any{{"fromRef": "replay.a", "fromPort": "fix", "toRef": "replay.b"}},
		}
	}

	first := postCapabilityBatch(t, env, body())
	if first.code != http.StatusOK {
		t.Fatalf("第一次 status = %d（body=%s）", first.code, first.body)
	}
	second := postCapabilityBatch(t, env, body())
	if second.code != http.StatusOK {
		t.Fatalf("重放 status = %d, want 200（幂等，body=%s）", second.code, second.body)
	}

	ids := func(res *decodeResult) []string {
		m := res.json(t)
		caps, _ := m["capabilities"].([]any)
		out := make([]string, 0, len(caps))
		for _, raw := range caps {
			c, _ := raw.(map[string]any)
			id, _ := c["id"].(string)
			out = append(out, id)
		}
		return out
	}
	firstIDs, secondIDs := ids(first), ids(second)
	if len(firstIDs) != 2 || len(secondIDs) != 2 || firstIDs[0] != secondIDs[0] || firstIDs[1] != secondIDs[1] {
		t.Fatalf("重放拿到的能力行不是同一份: %v vs %v（幂等折叠应返回已有行）", firstIDs, secondIDs)
	}

	// 三个面都没有第二次写入
	if n := countIn(t, env, `SELECT count(*) FROM capability_versions WHERE namespace_id = ? AND name IN (?,?)`,
		env.nsID, "replay.a", "replay.b"); n != 2 {
		t.Fatalf("能力行数 = %d, want 2（重放不许再建一份）", n)
	}
	if n := countIn(t, env, `SELECT count(*) FROM artifacts WHERE namespace_id = ? AND capability_id IN (?,?)`,
		env.nsID, firstIDs[0], firstIDs[1]); n != 2 {
		t.Fatalf("产物行数 = %d, want 2", n)
	}
	if n := countIn(t, env, `SELECT count(*) FROM edges WHERE namespace_id = ? AND from_ref = ?`,
		env.nsID, "replay.a"); n != 1 {
		t.Fatalf("边行数 = %d, want 1", n)
	}
}

// TestPostCapabilityBatchRollsBackEverything：任何一步失败 → 能力/产物/边**全不留下**。
//
// 两个失败点分别覆盖事务的前后两段：
//   - 第二个能力是保留名（内置起点 github）→ 事故事发在「建能力」段（第一个能力 +
//     产物已经写进事务了）
//   - 一条 link 指向不存在的名字 → 事故事发在「建边」段（两个能力 + 两份产物已写进事务）
func TestPostCapabilityBatchRollsBackEverything(t *testing.T) {
	cases := []struct {
		name string
		// seed 在**取基线之前**跑：它建的行属于基线，不属于「这次批量应该建出来的行」。
		seed     func(t *testing.T, env *consoleEnv)
		body     func(env *consoleEnv) map[string]any
		wantCode int
		wantKey  string // 响应里的 code 字段（空 = 不检查）
		wantIn   string // error 文案里必须出现的片段
	}{
		{
			name: "第二个能力撞了已有的 name@version（append-only）",
			// 库里已有 batch.taken@1（内容 A）；同 name@version 换内容必然被拒
			// （不变量 I8）。失败点在**建能力**段：第一个能力 + 它的产物已经
			// 写进事务了，所以这条用例真的在验回滚。
			seed: func(t *testing.T, env *consoleEnv) {
				env.seedCapability(t, "batch.taken", 1, []string{"done"})
			},
			body: func(env *consoleEnv) map[string]any {
				return map[string]any{
					"namespaceId": env.nsID,
					"capabilities": []map[string]any{
						{"name": "batch.ok", "outputs": []string{"done"}, "run": batchRun()},
						{
							"name":         "batch.taken",
							"version":      1,
							"outputs":      []string{"other"},
							"paramsSchema": map[string]any{"type": "object", "required": []string{"x"}},
						},
					},
				}
			},
			wantCode: http.StatusConflict,
			wantKey:  "capability_conflict",
			wantIn:   "capabilities[1]",
		},
		{
			name: "link 指向不存在的名字",
			body: func(env *consoleEnv) map[string]any {
				return map[string]any{
					"namespaceId": env.nsID,
					"capabilities": []map[string]any{
						{"name": "batch.ok", "outputs": []string{"done"}, "run": batchRun()},
						{"name": "batch.ok2", "outputs": []string{"done"}, "run": batchRun()},
					},
					"links": []map[string]any{
						{"fromRef": "batch.ok", "fromPort": "done", "toRef": "ghost.capability"},
					},
				}
			},
			wantCode: http.StatusBadRequest,
			wantKey:  "edge_rejected",
			wantIn:   "links[0]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConsoleEnv(t)
			if tc.seed != nil {
				tc.seed(t, env)
			}
			// 基线用「与提交前完全一致」判，而不是「表是空的」：命名空间建行时
			// 播种的内置能力（script.echo / notify.phone…）本来就在表里。
			baseCaps := countIn(t, env, `SELECT count(*) FROM capability_versions`)
			baseEdges := countIn(t, env, `SELECT count(*) FROM edges`)
			baseArtifacts := countIn(t, env, `SELECT count(*) FROM artifacts`)

			res := postCapabilityBatch(t, env, tc.body(env))
			if res.code != tc.wantCode {
				t.Fatalf("status = %d, want %d（body=%s）", res.code, tc.wantCode, res.body)
			}
			m := res.json(t)
			if tc.wantKey != "" && m["code"] != tc.wantKey {
				t.Fatalf("code = %v, want %s（body=%s）", m["code"], tc.wantKey, res.body)
			}
			if tc.wantIn != "" {
				errText, _ := m["error"].(string)
				if !strings.Contains(errText, tc.wantIn) {
					t.Fatalf("error 里没有 %q: %q", tc.wantIn, errText)
				}
			}

			// 目标语义：三个面**一条都没留下**
			if n := countIn(t, env, `SELECT count(*) FROM capability_versions`); n != baseCaps {
				t.Fatalf("能力行数 %d → %d，整批回滚失败", baseCaps, n)
			}
			if n := countIn(t, env, `SELECT count(*) FROM artifacts`); n != baseArtifacts {
				t.Fatalf("产物行数 %d → %d，整批回滚失败", baseArtifacts, n)
			}
			if n := countIn(t, env, `SELECT count(*) FROM edges`); n != baseEdges {
				t.Fatalf("边行数 %d → %d，整批回滚失败", baseEdges, n)
			}
		})
	}
}

// TestPostCapabilityBatchDuplicateName400：批内 name 重复 → 400（不是 409）。
//
// 为什么必须硬拒：批内的边用**名字**引用，重名让「这条边指向哪一个」无法表达。
func TestPostCapabilityBatchDuplicateName400(t *testing.T) {
	env := newConsoleEnv(t)
	res := postCapabilityBatch(t, env, map[string]any{
		"namespaceId": env.nsID,
		"capabilities": []map[string]any{
			{"name": "batch.dup", "outputs": []string{"done"}},
			{"name": "batch.dup", "outputs": []string{"failed"}},
		},
	})
	if res.code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", res.code, res.body)
	}
	m := res.json(t)
	if m["code"] != "duplicate_name" {
		t.Fatalf("code = %v, want duplicate_name（body=%s）", m["code"], res.body)
	}
	errText, _ := m["error"].(string)
	if !strings.Contains(errText, "capabilities[1]") || !strings.Contains(errText, "batch.dup") {
		t.Fatalf("error 里没有定位信息（下标 + 名字）: %q", errText)
	}
	if n := countIn(t, env, `SELECT count(*) FROM capability_versions WHERE namespace_id = ? AND name = ?`,
		env.nsID, "batch.dup"); n != 0 {
		t.Fatalf("重名被拒却写入了能力行: %d", n)
	}
}

// TestPostCapabilityBatchCycleMatchesSinglePath：成环。
//
// 契约 §2 把配置期静态环检测删掉了（A 组的竖切）：删除之后 `x → y → x` 是合法编排
// （靠 hop 预算兜底），批量与单建**都**应当成功。
//
// 在 A 合入之前 `createEdgeTx` 仍会拒，所以这里断言的是**等价性**而不是「一定成功」：
// 批量不引入自己的环检测，两条路径永远同进同退（今天都 400，A 合入后都 200）。
func TestPostCapabilityBatchCycleMatchesSinglePath(t *testing.T) {
	// ① 批量：两个新能力互相接（最后一个 link 闭合环）
	batchEnv := newConsoleEnv(t)
	batchRes := postCapabilityBatch(t, batchEnv, map[string]any{
		"namespaceId": batchEnv.nsID,
		"capabilities": []map[string]any{
			{"name": "cyc.x", "outputs": []string{"out"}},
			{"name": "cyc.y", "outputs": []string{"out"}},
		},
		"links": []map[string]any{
			{"fromRef": "cyc.x", "fromPort": "out", "toRef": "cyc.y"},
			{"fromRef": "cyc.y", "fromPort": "out", "toRef": "cyc.x"},
		},
	})

	// ② 单建：同样的内容，逐条建（第二条边走 PUT downstreams 闭合环）
	singleEnv := newConsoleEnv(t)
	singleEnv.seedCapability(t, "cyc.y", 1, []string{"out"})
	rr := singleEnv.do(t, http.MethodPost, "/v1/capabilities", map[string]any{
		"namespaceId": singleEnv.nsID,
		"name":        "cyc.x",
		"outputs":     []string{"out"},
		"links":       []map[string]any{{"fromPort": "out", "toRef": "cyc.y"}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("单建前置（cyc.x → cyc.y）失败: %d %s", rr.Code, rr.Body.String())
	}
	closeRR := singleEnv.replaceDownstreams(t, "cyc.y", []map[string]any{{"toRef": "cyc.x"}})
	singleRes := &decodeResult{code: closeRR.Code, body: closeRR.Body.String()}

	// ③ 等价性：状态码必须一致（这就是「批量没有自己的环检测」的证据）
	if batchRes.code != singleRes.code {
		t.Fatalf("成环：批量 %d vs 单建 %d（两条路径必须同进同退；body=%s / %s）",
			batchRes.code, singleRes.code, batchRes.body, singleRes.body)
	}
	if batchRes.code == http.StatusOK {
		// A 组删掉静态环检测之后走这里：批量真的把环建出来了。
		m := batchRes.json(t)
		if edges, _ := m["edges"].([]any); len(edges) != 2 {
			t.Fatalf("成环被允许时应有 2 条边，got %v", m["edges"])
		}
		return
	}
	// 今天：两边都被 createEdgeTx 的静态环检测拒掉，且批量那条错误要带下标。
	batchJSON := batchRes.json(t)
	singleJSON := singleRes.json(t)
	if batchJSON["code"] != "edge_rejected" || singleJSON["code"] != "edge_rejected" {
		t.Fatalf("成环的拒绝码不对: 批量 %v / 单建 %v", batchJSON["code"], singleJSON["code"])
	}
	if errText, _ := batchJSON["error"].(string); !strings.Contains(errText, "links[1]") {
		t.Fatalf("成环的 400 没指出是第几条: %q", errText)
	}
	// 整批回滚（成环的那条 link 失败时，两个能力都不该留下）
	if n := countIn(t, batchEnv, `SELECT count(*) FROM capability_versions WHERE namespace_id = ? AND name IN (?,?)`,
		batchEnv.nsID, "cyc.x", "cyc.y"); n != 0 {
		t.Fatalf("成环被拒却留下了能力行: %d", n)
	}
}

// TestPostCapabilityBatchUpgradeGate409AndForce：任一能力破坏性 → 整批 409；force 越过。
func TestPostCapabilityBatchUpgradeGate409AndForce(t *testing.T) {
	// seed 一个已有 fixer@1（两条出口）与一条跟随 latest 的 link，让 v2 的破坏性可见。
	seed := func(t *testing.T, env *consoleEnv) {
		t.Helper()
		ctx := context.Background()
		v1 := &store.CapabilityVersion{
			NamespaceID:  env.nsID,
			Name:         "fixer",
			Version:      1,
			ParamsSchema: json.RawMessage(`{"type":"object","required":["issue"],"properties":{"issue":{"type":"string"}}}`),
			Outputs:      []string{"done", "failed"},
			Source:       store.CapSourceCustom,
		}
		if err := env.db.CreateCapability(ctx, v1); err != nil {
			t.Fatalf("create fixer@1: %v", err)
		}
		env.seedCapability(t, "review", 1, []string{"fix"})
		if err := env.db.CreateEdge(ctx, &store.Edge{
			NamespaceID: env.nsID, FromRef: "review", FromPort: "fix", ToRef: "fixer",
		}); err != nil {
			t.Fatalf("create review --fix--> fixer: %v", err)
		}
	}
	// v2：删掉出口 failed + 新增必填入参 note → 破坏性变更（接在 fixer 上的 name 型 link 会拿到新契约）
	body := func(env *consoleEnv, force any) map[string]any {
		b := map[string]any{
			"namespaceId": env.nsID,
			"capabilities": []map[string]any{
				{
					"name":    "fixer",
					"outputs": []string{"done"},
					"paramsSchema": map[string]any{
						"type":     "object",
						"required": []string{"issue", "note"},
						"properties": map[string]any{
							"issue": map[string]any{"type": "string"},
							"note":  map[string]any{"type": "string"},
						},
					},
				},
				// 同一批里的另一个能力：证明确实是**整批**被拦/被放行
				{"name": "batch.side", "outputs": []string{"done"}},
			},
		}
		if force != nil {
			b["force"] = force
		}
		return b
	}

	t.Run("无 force → 整批 409", func(t *testing.T) {
		env := newConsoleEnv(t)
		seed(t, env)
		res := postCapabilityBatch(t, env, body(env, nil))
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
		if impact["capability"] != "fixer" {
			t.Fatalf("upgradeImpact.capability = %v, want fixer（第一个破坏性能力）", impact["capability"])
		}
		if changes, _ := impact["changes"].([]any); len(changes) == 0 {
			t.Fatalf("upgradeImpact.changes 是空的（删出口 + 加必填都该报出来）: %v", impact)
		}
		if links, _ := impact["links"].([]any); len(links) != 1 {
			t.Fatalf("受影响 link 数 = %d, want 1（review --fix--> fixer）", len(links))
		}
		// 整批零写入：v2 没有、同批的其它能力也没有
		if _, err := env.db.GetCapability(context.Background(), env.nsID, "fixer", 2); err == nil {
			t.Fatal("闸门拒了却还是写进了 fixer@2")
		}
		if n := countIn(t, env, `SELECT count(*) FROM capability_versions WHERE namespace_id = ? AND name = ?`,
			env.nsID, "batch.side"); n != 0 {
			t.Fatalf("闸门拒了却还是写进了同批的 batch.side（%d 行）", n)
		}
	})

	t.Run("批级 force → 越过", func(t *testing.T) {
		env := newConsoleEnv(t)
		seed(t, env)
		res := postCapabilityBatch(t, env, body(env, true))
		if res.code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", res.code, res.body)
		}
		if _, err := env.db.GetCapability(context.Background(), env.nsID, "fixer", 2); err != nil {
			t.Fatalf("force 越过失败，fixer@2 没写进去: %v", err)
		}
	})

	// force 写在与单建同形状的元素里也算数（元素形状与 POST /v1/capabilities 完全一致，
	// 元素里的 force 只能解释为「我知道这一批会改线」）。
	t.Run("元素级 force → 越过", func(t *testing.T) {
		env := newConsoleEnv(t)
		seed(t, env)
		b := body(env, nil)
		caps := b["capabilities"].([]map[string]any)
		caps[0]["force"] = true
		res := postCapabilityBatch(t, env, b)
		if res.code != http.StatusOK {
			t.Fatalf("status = %d, want 200（body=%s）", res.code, res.body)
		}
	})
}

// TestPostCapabilityBatchEqualsSinglePath：同样内容，单建（含 links）与批量落库结果一致。
//
// 两个命名空间里放同一份定义（同名同内容），只差「怎么提交」：
//
//	单建：先建 b，再建 a 并在同一个请求里带 links
//	批量：一次提交 a + b + link
//
// 断言三个面逐项相同：能力版本 + content_hash、产物 digest、边集合。
// 这条用例是「批量不引入第二套语义」的锚——它一旦红，说明两条写入路径开始分叉。
func TestPostCapabilityBatchEqualsSinglePath(t *testing.T) {
	env := newConsoleEnv(t)
	other := env.seedNamespace(t, "eq-ns", "等价性命名空间")

	// ① 单建（默认命名空间）
	rr := env.do(t, http.MethodPost, "/v1/capabilities", map[string]any{
		"namespaceId":  env.nsID,
		"name":         "eq.b",
		"outputs":      []string{"done"},
		"paramsSchema": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
		"run":          batchRun(),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("单建 eq.b 失败: %d %s", rr.Code, rr.Body.String())
	}
	rr = env.do(t, http.MethodPost, "/v1/capabilities", map[string]any{
		"namespaceId":  env.nsID,
		"name":         "eq.a",
		"outputs":      []string{"fix"},
		"paramsSchema": map[string]any{"type": "object"},
		"run":          batchRun(),
		"links":        []map[string]any{{"fromPort": "fix", "toRef": "eq.b"}},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("单建 eq.a 失败: %d %s", rr.Code, rr.Body.String())
	}

	// ② 批量（等价性命名空间，同一份定义）
	res := postCapabilityBatch(t, env, map[string]any{
		"namespaceId": other.ID,
		"capabilities": []map[string]any{
			{
				"name":         "eq.b",
				"outputs":      []string{"done"},
				"paramsSchema": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
				"run":          batchRun(),
			},
			{
				"name":         "eq.a",
				"outputs":      []string{"fix"},
				"paramsSchema": map[string]any{"type": "object"},
				"run":          batchRun(),
			},
		},
		"links": []map[string]any{{"fromRef": "eq.a", "fromPort": "fix", "toRef": "eq.b"}},
	})
	if res.code != http.StatusOK {
		t.Fatalf("批量失败: %d %s", res.code, res.body)
	}

	// ③ 三个面对账：能力版本 + content_hash / 产物 digest / 边集合
	capFingerprint := func(nsID string) map[string]string {
		out := map[string]string{}
		rows, err := env.db.Query(
			`SELECT name, version, content_hash FROM capability_versions
			  WHERE namespace_id = ? AND name IN (?,?)`, nsID, "eq.a", "eq.b")
		if err != nil {
			t.Fatalf("query capabilities: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name, hash string
			var version int
			if err := rows.Scan(&name, &version, &hash); err != nil {
				t.Fatalf("scan capability: %v", err)
			}
			out[name] = name + "@" + strconv.Itoa(version) + "#" + hash
		}
		return out
	}
	singleCaps := capFingerprint(env.nsID)
	batchCaps := capFingerprint(other.ID)
	if len(singleCaps) != 2 || len(batchCaps) != 2 {
		t.Fatalf("能力行数不对: 单建 %v / 批量 %v", singleCaps, batchCaps)
	}
	for _, name := range []string{"eq.a", "eq.b"} {
		if singleCaps[name] != batchCaps[name] {
			t.Fatalf("%s 的能力行不一致: 单建 %q / 批量 %q", name, singleCaps[name], batchCaps[name])
		}
	}

	// 产物 digest：同内容同 name@version → 内容寻址的 digest 必须相同
	digests := func(nsID string) map[string]string {
		out := map[string]string{}
		rows, err := env.db.Query(
			`SELECT c.name, a.digest FROM artifacts a
			   JOIN capability_versions c ON c.id = a.capability_id
			  WHERE a.namespace_id = ? AND c.name IN (?,?)`, nsID, "eq.a", "eq.b")
		if err != nil {
			t.Fatalf("query artifacts: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name, digest string
			if err := rows.Scan(&name, &digest); err != nil {
				t.Fatalf("scan artifact: %v", err)
			}
			out[name] = digest
		}
		return out
	}
	singleDigests := digests(env.nsID)
	batchDigests := digests(other.ID)
	for _, name := range []string{"eq.a", "eq.b"} {
		if singleDigests[name] == "" || singleDigests[name] != batchDigests[name] {
			t.Fatalf("%s 的产物 digest 不一致: 单建 %q / 批量 %q", name, singleDigests[name], batchDigests[name])
		}
	}

	// 边集合：三元组排序后逐字比较
	edges := func(nsID string) []string {
		rows, err := env.db.Query(
			`SELECT from_ref, from_port, to_ref FROM edges
			  WHERE namespace_id = ? AND from_ref IN (?,?)`, nsID, "eq.a", "eq.b")
		if err != nil {
			t.Fatalf("query edges: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var from, port, to string
			if err := rows.Scan(&from, &port, &to); err != nil {
				t.Fatalf("scan edge: %v", err)
			}
			out = append(out, from+" --"+port+"--> "+to)
		}
		sort.Strings(out)
		return out
	}
	singleEdges := edges(env.nsID)
	batchEdges := edges(other.ID)
	if len(singleEdges) != 1 || len(batchEdges) != 1 {
		t.Fatalf("边数不对: 单建 %v / 批量 %v", singleEdges, batchEdges)
	}
	if singleEdges[0] != batchEdges[0] {
		t.Fatalf("边集合不一致: 单建 %v / 批量 %v", singleEdges, batchEdges)
	}
}

// TestPostCapabilityBatchValidation400：请求形状/批级字段的 400 全部带定位信息。
func TestPostCapabilityBatchValidation400(t *testing.T) {
	cases := []struct {
		name   string
		body   func(env *consoleEnv) map[string]any
		wantIn string
	}{
		{
			name: "没有能力",
			body: func(env *consoleEnv) map[string]any {
				return map[string]any{"namespaceId": env.nsID, "capabilities": []map[string]any{}}
			},
			wantIn: "capabilities is required",
		},
		{
			name: "元素缺 name",
			body: func(env *consoleEnv) map[string]any {
				return map[string]any{
					"namespaceId":  env.nsID,
					"capabilities": []map[string]any{{"outputs": []string{"done"}}},
				}
			},
			wantIn: "capabilities[0]: name is required",
		},
		{
			name: "元素的 namespaceId 与批级冲突",
			body: func(env *consoleEnv) map[string]any {
				return map[string]any{
					"namespaceId":  env.nsID,
					"capabilities": []map[string]any{{"name": "batch.ns", "namespaceId": "ns_other"}},
				}
			},
			wantIn: "capabilities[0]: namespaceId",
		},
		{
			name: "元素里带 links（必须放批级）",
			body: func(env *consoleEnv) map[string]any {
				return map[string]any{
					"namespaceId": env.nsID,
					"capabilities": []map[string]any{{
						"name":  "batch.inner",
						"links": []map[string]any{{"fromPort": "done", "toRef": "notify.phone"}},
					}},
				}
			},
			wantIn: "capabilities[0]: links",
		},
		{
			name: "缺 namespaceId（与单建同一套命名空间规则）",
			body: func(env *consoleEnv) map[string]any {
				return map[string]any{"capabilities": []map[string]any{{"name": "batch.nons"}}}
			},
			wantIn: "namespaceId is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newConsoleEnv(t)
			base := countIn(t, env, `SELECT count(*) FROM capability_versions`)
			res := postCapabilityBatch(t, env, tc.body(env))
			if res.code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400（body=%s）", res.code, res.body)
			}
			m := res.json(t)
			errText, _ := m["error"].(string)
			if !strings.Contains(errText, tc.wantIn) {
				t.Fatalf("error 里没有 %q: %q", tc.wantIn, errText)
			}
			if n := countIn(t, env, `SELECT count(*) FROM capability_versions`); n != base {
				t.Fatalf("校验失败却写入了能力行: %d → %d", base, n)
			}
		})
	}
}
