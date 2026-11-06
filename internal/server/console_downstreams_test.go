package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖 `PUT /v1/capabilities/{name}/downstreams`（CONTRACT-downstream-edit §2）
// 的 HTTP 契约，以及 `POST /v1/edges` 的能力边分支被封之后的行为（§3）。

// dbEdgeSnapshot 把命名空间里的全部边拍成可逐条比对的快照（含 created_at / mapping）。
//
// 失败路径要证明的是「库里一个字节都没变」，所以快照必须覆盖到不参与业务断言的那些列。
func dbEdgeSnapshot(t *testing.T, env *consoleEnv) []string {
	t.Helper()
	rows, err := env.db.Query(
		`SELECT from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at
		   FROM edges WHERE namespace_id = ?
		  ORDER BY from_is_source, from_ref, from_port, to_ref`, env.nsID)
	if err != nil {
		t.Fatalf("snapshot edges: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var (
			isSrc               int
			fromRef, fromPort   string
			toRef, literals, mp string
			priority            int
			createdAt           int64
		)
		if err := rows.Scan(&isSrc, &fromRef, &fromPort, &toRef, &literals, &mp, &priority, &createdAt); err != nil {
			t.Fatalf("scan snapshot: %v", err)
		}
		out = append(out, strings.Join([]string{
			fromRef, fromPort, toRef, literals, mp,
			strconv.Itoa(isSrc), strconv.Itoa(priority), strconv.FormatInt(createdAt, 10),
		}, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return out
}

// TestCapabilityDownstreamsReplace 整体替换：增 / 删 / 改优先级 / 重排 / literals 更新 / 清空。
func TestCapabilityDownstreamsReplace(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "flow", 1, []string{"out"})
	a := env.seedCapability(t, "a", 1, nil)
	env.seedCapability(t, "b", 1, nil)
	strict := seedTargetWithSchema(t, env, "strict", edgeLiteralTargetSchema)

	// ① 建两条（一条带优先级 + literals）
	rr := env.replaceDownstreams(t, "flow", []map[string]any{
		{"toRef": "a"},
		{"toRef": "strict", "priority": 10, "literals": json.RawMessage(`{"to":"u_1"}`)},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := decode(t, rr)
	if body["name"] != "flow" {
		t.Fatalf("name = %v, want flow", body["name"])
	}
	links, _ := body["links"].([]any)
	if len(links) != 2 {
		t.Fatalf("links = %v, want 2（与请求同序、落库后的完整集合）", body["links"])
	}
	first, _ := links[0].(map[string]any)
	second, _ := links[1].(map[string]any)
	// 回执带派生字段：toRef 此刻解析到的 id 与版本（前端回显不用自己算 name→latest）
	if first["toRef"] != "a" || first["toCapabilityId"] != a.ID || first["resolvedVersion"] != float64(1) {
		t.Fatalf("links[0] = %v, want toRef=a toCapabilityId=%s resolvedVersion=1", first, a.ID)
	}
	if first["priority"] != float64(0) {
		t.Fatalf("links[0].priority = %v, want 0（缺省）", first["priority"])
	}
	if second["toCapabilityId"] != strict.ID {
		t.Fatalf("links[1].toCapabilityId = %v, want %s", second["toCapabilityId"], strict.ID)
	}
	if lit, _ := second["literals"].(map[string]any); lit["to"] != "u_1" {
		t.Fatalf("links[1].literals = %v, want {to:u_1}", second["literals"])
	}

	// 落库形状：左端是能力名、端口恒为隐式 out
	rows := dbEdgeSnapshot(t, env)
	if len(rows) != 2 {
		t.Fatalf("库里边数 = %d, want 2: %v", len(rows), rows)
	}
	for _, r := range rows {
		parts := strings.Split(r, "|")
		if parts[0] != "flow" || parts[1] != "out" {
			t.Fatalf("边的左端 = %q/%q, want flow/out（%s）", parts[0], parts[1], r)
		}
	}

	// ② 删掉 a、改 strict 的优先级与 literals
	rr = env.replaceDownstreams(t, "flow", []map[string]any{
		{"toRef": "strict", "priority": 3, "literals": json.RawMessage(`{"to":"u_2"}`)},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("replace(delete) status = %d, body=%s", rr.Code, rr.Body.String())
	}
	rows = dbEdgeSnapshot(t, env)
	if len(rows) != 1 || !strings.HasPrefix(rows[0], "flow|out|strict|") {
		t.Fatalf("删掉 a 之后库里 = %v, want 只剩 strict", rows)
	}
	if !strings.Contains(rows[0], `{"to":"u_2"}`) || !strings.Contains(rows[0], "|3|") {
		t.Fatalf("strict 的 priority/literals 没被原地更新: %s", rows[0])
	}

	// ③ 重排（提交顺序就是回执顺序）+ 加回 a
	rr = env.replaceDownstreams(t, "flow", []map[string]any{
		{"toRef": "a", "priority": 1},
		{"toRef": "strict", "priority": 2, "literals": json.RawMessage(`{"to":"u_3"}`)},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("replace(reorder) status = %d, body=%s", rr.Code, rr.Body.String())
	}
	links, _ = decode(t, rr)["links"].([]any)
	if len(links) != 2 || links[0].(map[string]any)["toRef"] != "a" ||
		links[1].(map[string]any)["toRef"] != "strict" {
		t.Fatalf("重排回执 = %v, want [a strict]", links)
	}

	// ④ 空数组 = 清空（终端能力）
	rr = env.replaceDownstreams(t, "flow", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, `"links":[]`) {
		t.Fatalf("清空的回执必须是 [] 而不是 null（I9）: %s", body)
	}
	if rows := dbEdgeSnapshot(t, env); len(rows) != 0 {
		t.Fatalf("清空之后库里还有边: %v", rows)
	}
}

// TestCapabilityDownstreamsAtomicOnInvalidPayload 任一校验失败 → 400，且**一个字节都不写**。
//
// 手法：先落 3 条合法下游，再逐条提交非法 payload，每次都用全列快照逐条比对
// （包括 created_at / literals / mapping —— 「只改了又改回去」这种回归也要能拦住）。
func TestCapabilityDownstreamsAtomicOnInvalidPayload(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "flow", 1, []string{"out"})
	env.seedCapability(t, "a", 1, nil)
	env.seedCapability(t, "b", 1, nil)
	env.seedCapability(t, "c", 1, nil)
	seedTargetWithSchema(t, env, "strict", edgeLiteralTargetSchema)

	if rr := env.replaceDownstreams(t, "flow", []map[string]any{
		{"toRef": "a", "priority": 1},
		{"toRef": "b", "priority": 2},
		{"toRef": "c", "priority": 3},
	}); rr.Code != http.StatusOK {
		t.Fatalf("baseline status = %d, body=%s", rr.Code, rr.Body.String())
	}
	before := dbEdgeSnapshot(t, env)
	if len(before) != 3 {
		t.Fatalf("baseline edges = %d, want 3", len(before))
	}

	// 101 条（超过 DownstreamsMaxLinks）
	tooMany := make([]map[string]any, 101)
	for i := range tooMany {
		tooMany[i] = map[string]any{"toRef": "a"}
	}

	cases := []struct {
		name     string
		links    []map[string]any
		wantCode string
	}{
		{"未知目标", []map[string]any{{"toRef": "a"}, {"toRef": "ghost"}}, "unknown_target"},
		{"自连", []map[string]any{{"toRef": "flow"}}, "self_link"},
		{"重复目标", []map[string]any{{"toRef": "a"}, {"toRef": "a", "priority": 9}}, "duplicate_target"},
		{"优先级过低", []map[string]any{{"toRef": "a", "priority": -1}}, "priority_out_of_range"},
		{"优先级过高", []map[string]any{{"toRef": "a", "priority": 1001}}, "priority_out_of_range"},
		{"超出条数上限", tooMany, "too_many_links"},
		{"literals 键不在目标 schema", []map[string]any{
			{"toRef": "strict", "literals": json.RawMessage(`{"nope":"x"}`)},
		}, "edge_rejected"},
		{"literals 类型不符", []map[string]any{
			{"toRef": "strict", "literals": json.RawMessage(`{"to":1}`)},
		}, "edge_rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.replaceDownstreams(t, "flow", tc.links)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			body := decode(t, rr)
			if body["code"] != tc.wantCode {
				t.Fatalf("code = %v, want %s（body=%s）", body["code"], tc.wantCode, rr.Body.String())
			}
			if msg, _ := body["error"].(string); strings.TrimSpace(msg) == "" {
				t.Fatalf("400 响应没有说明原因: %s", rr.Body.String())
			}
			after := dbEdgeSnapshot(t, env)
			if strings.Join(before, "\n") != strings.Join(after, "\n") {
				t.Fatalf("被拒的 payload 改动了库（必须一个字节都不写）：\n之前:\n%s\n之后:\n%s",
					strings.Join(before, "\n"), strings.Join(after, "\n"))
			}
		})
	}
}

// TestCapabilityDownstreamsOnlyTouchesOwnEdges 只动这个能力在隐式出口 `out` 上的边：
// 别的能力的边、入口边、以及历史多出口能力别的端口上的边都不碰。
func TestCapabilityDownstreamsOnlyTouchesOwnEdges(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "flow", 1, []string{"out"})
	env.seedCapability(t, "other", 1, []string{"out"})
	env.seedCapability(t, "a", 1, nil)
	env.seedCapability(t, "b", 1, nil)

	// flow 的存量下游 + 一条老端口 `fix` 的边（历史多出口能力的形状，界面上只读保留）
	if rr := env.replaceDownstreams(t, "flow", []map[string]any{
		{"toRef": "a"}, {"toRef": "b"},
	}); rr.Code != http.StatusOK {
		t.Fatalf("seed flow status = %d, body=%s", rr.Code, rr.Body.String())
	}
	// 直插：`fix` 不在 flow 的 outputs 里（CreateEdge 会拒），而历史多出口数据就是那个形状。
	// 它不归下游整体替换管（那条路径只写隐式出口 `out`）。
	if _, err := env.db.Exec(
		`INSERT INTO edges (namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at)
		 VALUES (?, 0, 'flow', 'fix', 'b', '{}', '{}', 0, ?)`,
		env.nsID, time.Now().Unix()); err != nil {
		t.Fatalf("seed legacy-port edge: %v", err)
	}
	// 别的能力的 out 边 + 入口边
	if rr := env.replaceDownstreams(t, "other", []map[string]any{{"toRef": "a"}}); rr.Code != http.StatusOK {
		t.Fatalf("seed other status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		NamespaceID: env.nsID, FromIsSource: true, FromRef: "github", FromPort: "push", ToRef: "a",
	}); rr.Code != http.StatusOK {
		t.Fatalf("seed entry edge status = %d, body=%s", rr.Code, rr.Body.String())
	}

	// 整体替换 flow → 只剩 a
	if rr := env.replaceDownstreams(t, "flow", []map[string]any{{"toRef": "a"}}); rr.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body=%s", rr.Code, rr.Body.String())
	}

	rows := dbEdgeSnapshot(t, env)
	var flowOut, legacyPort, otherOut, entry int
	for _, r := range rows {
		parts := strings.Split(r, "|")
		switch {
		case parts[0] == "flow" && parts[1] == "out":
			flowOut++
			if parts[2] != "a" {
				t.Fatalf("flow/out 还有未提交的下游: %s", r)
			}
		case parts[0] == "flow" && parts[1] == "fix":
			legacyPort++
		case parts[0] == "other":
			otherOut++
		case parts[0] == "github":
			entry++
		}
	}
	if flowOut != 1 || legacyPort != 1 || otherOut != 1 || entry != 1 {
		t.Fatalf("删多了/删少了：flow/out=%d legacyPort=%d other=%d entry=%d（want 1/1/1/1）\n%v",
			flowOut, legacyPort, otherOut, entry, rows)
	}
}

// TestPostEdgesCapabilityWriteRemoved `POST /v1/edges` 只接入口边；
// 能力边 → 405 `edge_write_removed`（并指向新的写入口），入口边照旧可用。
func TestPostEdgesCapabilityWriteRemoved(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "review", 1, []string{"out"})
	env.seedCapability(t, "notify", 1, nil)

	// 能力边（默认 fromIsSource=false）→ 405 + 机器码 + 明确的迁移指引
	rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		NamespaceID: env.nsID, FromRef: "review", FromPort: "out", ToRef: "notify",
	})
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405（能力边的旧写入口已封）; body=%s", rr.Code, rr.Body.String())
	}
	body := decode(t, rr)
	if body["code"] != "edge_write_removed" {
		t.Fatalf("code = %v, want edge_write_removed", body["code"])
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "PUT /v1/capabilities/{name}/downstreams") {
		t.Fatalf("错误文案没告诉用户该走哪个端点: %q", msg)
	}
	// 显式 fromIsSource=false 也一样（不是「缺字段才拒」）
	if rr := env.do(t, http.MethodPost, "/v1/edges", map[string]any{
		"namespaceId": env.nsID, "fromIsSource": false,
		"fromRef": "review", "fromPort": "out", "toRef": "notify",
	}); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("显式 fromIsSource=false status = %d, want 405", rr.Code)
	}
	if rows := dbEdgeSnapshot(t, env); len(rows) != 0 {
		t.Fatalf("被 405 拒绝的能力边落库了: %v", rows)
	}

	// 入口边保持可用（e2e / 高级用法仍需给「没有 sources 行的来源名」接入边）
	rr = env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		NamespaceID: env.nsID, FromIsSource: true,
		FromRef: "github", FromPort: "push", ToRef: "notify",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("入口边 status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if edgeObj, _ := decode(t, rr)["edge"].(map[string]any); edgeObj["fromIsSource"] != true {
		t.Fatalf("入口边响应 = %v, want fromIsSource=true", edgeObj)
	}
	if rows := dbEdgeSnapshot(t, env); len(rows) != 1 || !strings.HasPrefix(rows[0], "github|") {
		t.Fatalf("入口边没落库（或落错了）: %v", rows)
	}
}

// TestCapabilityDownstreamsEndpointScope 作用域与参数形状：
// 缺命名空间 400 / `*` 400 / 不存在的命名空间 404 / 不存在的能力 404 /
// 缺 `links` 字段 400（空数组才是清空）。
func TestCapabilityDownstreamsEndpointScope(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "flow", 1, []string{"out"})

	const path = "/v1/capabilities/flow/downstreams"
	put := func(t *testing.T, p string, body any) (int, map[string]any) {
		t.Helper()
		rr := env.do(t, http.MethodPut, p, body)
		var m map[string]any
		if rr.Body.Len() > 0 {
			m = decode(t, rr)
		}
		return rr.Code, m
	}

	// 缺 namespaceId → 400（不回退默认命名空间）
	code, m := put(t, path, map[string]any{"links": []any{}})
	if code != http.StatusBadRequest {
		t.Fatalf("缺 namespaceId status = %d, want 400", code)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, "namespaceId") {
		t.Fatalf("error = %q, want 提到 namespaceId", msg)
	}

	// `*` 是跨命名空间汇总的读操作专用 → 400
	code, _ = put(t, withNSID(path, "*"), map[string]any{"links": []any{}})
	if code != http.StatusBadRequest {
		t.Fatalf("namespaceId=* status = %d, want 400", code)
	}

	// 不存在的命名空间 → 404
	code, _ = put(t, withNSID(path, "ns_doesnotexist"), map[string]any{"links": []any{}})
	if code != http.StatusNotFound {
		t.Fatalf("未知命名空间 status = %d, want 404", code)
	}

	// 不存在的能力 → 404 not_found（不会「悄悄建一组没人能走的边」）
	code, m = put(t, env.withNS("/v1/capabilities/ghost/downstreams"), map[string]any{"links": []any{}})
	if code != http.StatusNotFound || m["code"] != "not_found" {
		t.Fatalf("未知能力 = %d %v, want 404 not_found", code, m)
	}

	// 缺 links 字段 → 400（把「漏传」与「清空」分开：前者是 bug，后者是意图）
	code, m = put(t, env.withNS(path), map[string]any{})
	if code != http.StatusBadRequest || m["code"] != "links_required" {
		t.Fatalf("缺 links = %d %v, want 400 links_required", code, m)
	}

	// 但 `{"links":null}` 与 `{"links":[]}` 不一样：null 视为缺字段（同 links_required）
	code, m = put(t, env.withNS(path), map[string]any{"links": nil})
	if code != http.StatusBadRequest || m["code"] != "links_required" {
		t.Fatalf("links=null = %d %v, want 400 links_required", code, m)
	}
}

// TestCapabilityDownstreamsEdgeCases 几个容易漏的输入形状：
//
//   - 路径里写 `name@version`：版本部分没有意义（全部版本共用同一组出边），按名字处理；
//   - `literals: null`：等于「没有覆盖」（`{}`），不是错误；
//   - 自连不管用裸名字还是钉死引用/能力 id，都必须是 `self_link`（否则会写出一条自己接自己的边）。
func TestCapabilityDownstreamsEdgeCases(t *testing.T) {
	env := newConsoleEnv(t)
	flow := env.seedCapability(t, "flow", 1, []string{"out"})
	env.seedCapability(t, "a", 1, nil)

	// ① 路径里的 `flow@1` 等价于 `flow`
	rr := env.replaceDownstreams(t, "flow@1", []map[string]any{{"toRef": "a"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("path name@version status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if name, _ := decode(t, rr)["name"].(string); name != "flow" {
		t.Fatalf("回执 name = %q, want flow（版本部分不参与）", name)
	}

	// ② literals: null → {}（可选项的通用约定：null = 没有值，不是错误）
	rr = env.replaceDownstreams(t, "flow", []map[string]any{{"toRef": "a", "literals": nil}})
	if rr.Code != http.StatusOK {
		t.Fatalf("literals=null status = %d, body=%s", rr.Code, rr.Body.String())
	}
	links, _ := decode(t, rr)["links"].([]any)
	if lit, ok := links[0].(map[string]any)["literals"].(map[string]any); !ok || len(lit) != 0 {
		t.Fatalf("literals=null 的落库回执 = %v, want {}", links[0])
	}

	// ③ 自连的三种写法都拦
	for _, toRef := range []string{"flow", "flow@1", flow.ID} {
		rr := env.replaceDownstreams(t, "flow", []map[string]any{{"toRef": toRef}})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("self link via %q status = %d, want 400; body=%s", toRef, rr.Code, rr.Body.String())
		}
		if code := decode(t, rr)["code"]; code != "self_link" {
			t.Fatalf("self link via %q code = %v, want self_link", toRef, code)
		}
	}
}
