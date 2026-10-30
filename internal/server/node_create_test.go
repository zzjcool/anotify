package server

// POST /v1/nodes 的验收测试（issue #185 / 契约 §3、§4、§6）。
//
// 全部是 **HTTP 级 + 真 SQLite** 的用例：走 newConsoleEnv 装出来的真会话中间件、
// 真 store、真表约束，不打桩。这样测的是「调用方实际会看到的状态码 + 响应体」，
// 而不是「我们以为 handler 会怎么走」。
//
// 覆盖契约 §6 的 7 条验收：
//  1. 缺 namespaceId → 400 且点名 namespaceId（不回退默认命名空间）…… TestCreateNodeRequiresNamespace
//  2. namespaceId=* → 400 ………………………………………………………………… TestCreateNodeRejectsAllNamespaces
//  3. 不存在的命名空间 → 404（且不回显 id）；另一个**已存在**的命名空间 → 201 且落在那里
//     …………………………………………………………………………………… TestCreateNodeNamespaceScope
//  4. 成功 201 + 令牌只出现一次 ………………………………………………………… TestCreateNodeReturnsTokenOnce
//  5. 同命名空间重名 → 409 ………………………………………………………………… TestCreateNodeDuplicateNameConflict
//  6. 令牌真能做 WS 握手（真 httptest 服务器，不用 fake）……………… TestCreateNodeTokenWorksForWSHandshake
//  7. 令牌落库是 sha256（合并进 4）…………………………………………………… TestCreateNodeReturnsTokenOnce
//
// 附加边界（契约 §6「附加边界」）：名字校验、不建 node_instances、无会话 401、query 优先。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// createdNode 是一次成功注册（POST /v1/nodes → 201）解出来的两样东西。
type createdNode struct {
	node  map[string]any // node 视图（nodeView：nodes 行 + capabilities/instances/busy）
	token string         // 明文令牌：只在这一次响应里出现（契约 §10.3 / D3）
}

// mustCreateNode 断言 POST /v1/nodes 返回 201 并解出 {node, token}。
func mustCreateNode(t *testing.T, rr *httptest.ResponseRecorder) createdNode {
	t.Helper()
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /v1/nodes status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	top := decode(t, rr)
	nodeMap, ok := top["node"].(map[string]any)
	if !ok {
		t.Fatalf("201 响应缺 node 对象: %s", rr.Body.String())
	}
	token, ok := top["token"].(string)
	if !ok || token == "" {
		t.Fatalf("201 响应里 token 缺失或为空: %s", rr.Body.String())
	}
	return createdNode{node: nodeMap, token: token}
}

// mustStr 取一个字符串字段；缺失就带着整个 map Fatal（便于一眼看出响应长什么样）。
func mustStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("字段 %q 缺失或不是字符串: %v", key, m)
	}
	return v
}

// assertNoTokenLeak 断言响应体里没有明文令牌、没有落库的 sha256、也没有哈希字段名。
//
// 理由（契约 §10.3 / D3）：令牌只在 201 那一次出现；库里只有 sha256；
// 任何读端点回显任何一份都等于把凭据/它的可爆破材料送到浏览器与日志里。
func assertNoTokenLeak(t *testing.T, where, body, token, hash string) {
	t.Helper()
	if strings.Contains(body, token) {
		t.Fatalf("%s 里出现了明文令牌: %s", where, body)
	}
	if strings.Contains(body, hash) {
		t.Fatalf("%s 里出现了 token_hash: %s", where, body)
	}
	if strings.Contains(body, "tokenHash") || strings.Contains(body, "token_hash") {
		t.Fatalf("%s 里出现了令牌哈希字段名: %s", where, body)
	}
}

// assertNoTokenKey 断言响应对象（及其 node 子对象）里没有 "token" 键。
func assertNoTokenKey(t *testing.T, where string, m map[string]any) {
	t.Helper()
	if _, ok := m["token"]; ok {
		t.Fatalf("%s 顶层出现了 token 字段: %v", where, m)
	}
	if n, ok := m["node"].(map[string]any); ok {
		if _, ok := n["token"]; ok {
			t.Fatalf("%s 的 node 里出现了 token 字段: %v", where, n)
		}
		if _, ok := n["tokenHash"]; ok {
			t.Fatalf("%s 的 node 里出现了 tokenHash 字段: %v", where, n)
		}
	}
}

// TestCreateNodeRequiresNamespace 契约 §6.1 / D1：缺 namespaceId → 400 且点名 namespaceId。
//
// 关键不只是状态码，而是「绝不回退默认命名空间」：请求失败之后库里（以及列表端点）
// 都不能多出这台机器——否则「参数写错」会被悄悄落成「写到了默认命名空间」，
// 症状会在很久以后才以「任务跑在了不该跑的机器上」出现。
func TestCreateNodeRequiresNamespace(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	cases := []struct {
		name string
		body any
	}{
		// query 与 body 都没有 namespaceId
		{"有 body 但无 namespaceId", map[string]any{"name": "n1"}},
		// 空 body：handler 必须把它当「全字段为空」→ 报 namespaceId 缺失，
		// 而不是被空 body 的 EOF 抢先报成 "invalid JSON body"（契约 §3 / §4 第 3 步）。
		{"空 body", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.do(t, http.MethodPost, "/v1/nodes", tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			msg := mustStr(t, decode(t, rr), "error")
			if !strings.Contains(msg, "namespaceId") {
				t.Fatalf("错误文案没有点名 namespaceId: %q", msg)
			}

			// 没有回退默认命名空间：库里看不到这台机器。
			nodes, err := env.db.ListNodes(ctx, env.nsID)
			if err != nil {
				t.Fatalf("list nodes: %v", err)
			}
			if len(nodes) != 0 {
				t.Fatalf("默认命名空间里多出了 %d 台节点（缺参时不该回退默认 ns）", len(nodes))
			}

			// 列表端点同样看不到它（HTTP 视角的一致性）。
			list := env.do(t, http.MethodGet, env.withNS("/v1/nodes"), nil)
			if list.Code != http.StatusOK {
				t.Fatalf("GET /v1/nodes status = %d; body=%s", list.Code, list.Body.String())
			}
			var listBody struct {
				Nodes []map[string]any `json:"nodes"`
			}
			if err := json.Unmarshal(list.Body.Bytes(), &listBody); err != nil {
				t.Fatalf("decode list: %v; body=%s", err, list.Body.String())
			}
			if len(listBody.Nodes) != 0 {
				t.Fatalf("GET /v1/nodes 里多出了 %d 台节点: %s", len(listBody.Nodes), list.Body.String())
			}
		})
	}
}

// TestCreateNodeRejectsAllNamespaces 契约 §6.2 / D1：`namespaceId=*` 在写端点没有定义 → 400。
//
// `*` 是「跨命名空间汇总读」的标记；写一台机器必须落到唯一一个命名空间里，
// 所以 query 与 body 两条入口都要拒（否则客户端换个位置带 `*` 就能绕过）。
func TestCreateNodeRejectsAllNamespaces(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	cases := []struct {
		name string
		path string
		body any
	}{
		// query 优先路径
		{"query 带 *", withNSID("/v1/nodes", store.AllNamespaces), map[string]any{"name": "n1"}},
		// body 兜底路径（query 空 → 看 body）
		{"body 带 *", "/v1/nodes", map[string]any{"name": "n1", "namespaceId": store.AllNamespaces}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.do(t, http.MethodPost, tc.path, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			msg := mustStr(t, decode(t, rr), "error")
			// 文案必须说清是哪一条参数错了（避免调用方以为是自己漏了 namespaceId）。
			if !strings.Contains(msg, "namespaceId") || !strings.Contains(msg, "*") {
				t.Fatalf("错误文案没有点名 namespaceId=*: %q", msg)
			}

			// `*` 写不进任何命名空间：本组织的默认 ns 里不该多出节点。
			nodes, err := env.db.ListNodes(ctx, env.nsID)
			if err != nil {
				t.Fatalf("list nodes: %v", err)
			}
			if len(nodes) != 0 {
				t.Fatalf("namespaceId=* 竟然建出了 %d 台节点", len(nodes))
			}
		})
	}
}

// TestCreateNodeNamespaceScope 契约 §6.3 / D1：创建节点的命名空间作用域。
//
// 迁移 14 删除了组织层（#204）之后，**不存在「别人的命名空间」**：本部署内任何已登录
// 用户都能往任何**存在**的命名空间里建设备。所以原来那条「别的 org 的命名空间 → 404」
// 的断言改写为两条等价且更强的命名空间口径断言（原用例名字是
// TestCreateNodeForeignNamespaceIs404，断言数与覆盖点一个都没少）：
//  1. 不存在的命名空间 id → 404，且响应体不回显传入的 id（防探测：404 除了「不存在」
//     本身之外不能再泄漏任何东西）；
//  2. 另一个**真实存在**的命名空间 → 201 且正常建出节点，返回的 node.namespaceId
//     是那个命名空间、**不是** env.nsID——这才是「作用域真的按请求生效」的证据
//     （一个写死默认命名空间的实现在旧用例下也能全绿）。
func TestCreateNodeNamespaceScope(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	// ① 不存在的命名空间 → 404，且不回显传入的 id。
	const missingNS = "ns_not_a_real_id"
	missing := env.do(t, http.MethodPost, withNSID("/v1/nodes", missingNS),
		map[string]any{"name": "intruder"})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在的 namespaceId status = %d, want 404; body=%s",
			missing.Code, missing.Body.String())
	}
	if strings.Contains(missing.Body.String(), missingNS) {
		t.Fatalf("404 响应里回显了传入的命名空间 id: %s", missing.Body.String())
	}
	if msg := mustStr(t, decode(t, missing), "error"); msg != "namespace not found" {
		t.Fatalf("404 的 error = %q, want %q（不存在的命名空间只有一种回答）", msg, "namespace not found")
	}

	// 解析失败时绝不能落库：默认命名空间里看不到这台机器。
	nodes, err := env.db.ListNodes(ctx, env.nsID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("默认命名空间里多出了 %d 台节点（解析失败却落库了）", len(nodes))
	}

	// ② 另一个真实存在的命名空间 → 201（本部署内共享），并且真的落在那个空间里。
	other := env.seedNamespace(t, "other", "另一个命名空间")
	created := mustCreateNode(t, env.do(t, http.MethodPost, withNSID("/v1/nodes", other.ID),
		map[string]any{"name": "shared-node"}))
	if got := mustStr(t, created.node, "namespaceId"); got != other.ID {
		t.Fatalf("node.namespaceId = %q, want %q（作用域必须按请求的 namespaceId 生效）", got, other.ID)
	}
	if got := mustStr(t, created.node, "namespaceId"); got == env.nsID {
		t.Fatalf("node.namespaceId 落回了默认命名空间 %q（请求里的 namespaceId 被忽略）", env.nsID)
	}

	// 库里也真的按命名空间分开：新空间恰好 1 台，默认空间仍然是空的。
	inOther, err := env.db.ListNodes(ctx, other.ID)
	if err != nil {
		t.Fatalf("list nodes(%s): %v", other.ID, err)
	}
	if len(inOther) != 1 || inOther[0].Name != "shared-node" {
		t.Fatalf("命名空间 %s 里应该恰好有 1 台 shared-node，实际 %d 台: %v", other.ID, len(inOther), inOther)
	}
	inDefault, err := env.db.ListNodes(ctx, env.nsID)
	if err != nil {
		t.Fatalf("list nodes(%s): %v", env.nsID, err)
	}
	if len(inDefault) != 0 {
		t.Fatalf("默认命名空间里多出了 %d 台节点（应该全在 %s 里）", len(inDefault), other.ID)
	}
}

// TestCreateNodeReturnsTokenOnce 契约 §6.4 + §6.7 / D3、D4：
// 201 返回 {node, token}；令牌前缀是 ant_node_；库里只存 sha256；后续读端点一个字都不吐。
func TestCreateNodeReturnsTokenOnce(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	rr := env.do(t, http.MethodPost, env.withNS("/v1/nodes"), map[string]any{
		"name":      "mbp-16",
		"tags":      []string{"pool-a"},
		"instances": 2, // 只接受不落库（D5）：这里是「建议值」，见 TestCreateNodeDoesNotCreateInstances
	})
	created := mustCreateNode(t, rr)

	// 令牌形态：ant_node_<base64url(24B)>（D4：issue 里的 n_xxxxx 只是示意，不能改前缀——
	// ws 握手与 e2e 都按 ant_node_ 断言）。
	if !strings.HasPrefix(created.token, "ant_node_") {
		t.Fatalf("token = %q, want 前缀 ant_node_", created.token)
	}

	// node 视图与列表/详情同形（viewFor），字段必须已经落好。
	if got := mustStr(t, created.node, "name"); got != "mbp-16" {
		t.Fatalf("node.name = %q, want mbp-16", got)
	}
	if got := mustStr(t, created.node, "namespaceId"); got != env.nsID {
		t.Fatalf("node.namespaceId = %q, want %q", got, env.nsID)
	}
	// 组织层随迁移 14 删除（#204）：节点视图里**不能**再有 orgId。
	// 原来的「orgId == 本 org」断言换成了更严的一条——字段必须彻底不在响应里
	// （隔离维度只剩 namespaceId；残留的 orgId 只会让调用方以为组织还在）。
	if v, ok := created.node["orgId"]; ok {
		t.Fatalf("node.orgId = %v，组织层已删除，视图里不该再有这个字段", v)
	}
	if got := mustStr(t, created.node, "status"); got != store.NodeStatusOffline {
		t.Fatalf("node.status = %q, want %q（刚注册、还没 hello）", got, store.NodeStatusOffline)
	}
	tags, ok := created.node["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "pool-a" {
		t.Fatalf("node.tags = %v, want [pool-a]", created.node["tags"])
	}
	// instances 是 viewFor 的派生数量（实体槽位），刚建完一台都没有 → 0（不是建议值 2）。
	if got, ok := created.node["instances"].(float64); !ok || got != 0 {
		t.Fatalf("node.instances = %v, want 0（实例只在 hello 时由节点申报）", created.node["instances"])
	}
	nodeID := mustStr(t, created.node, "id")
	if nodeID == "" {
		t.Fatal("node.id 为空")
	}

	// 库里的 token_hash：既不是明文，也必须等于 sha256(明文)（§6.7）。
	var storedHash string
	if err := env.db.QueryRowContext(ctx,
		`SELECT token_hash FROM nodes WHERE id = ?`, nodeID).Scan(&storedHash); err != nil {
		t.Fatalf("查 token_hash: %v", err)
	}
	if storedHash == created.token {
		t.Fatal("库里存的是明文令牌（必须只存 sha256）")
	}
	if want := store.HashNodeToken(created.token); storedHash != want {
		t.Fatalf("token_hash = %q, want sha256(token) = %q", storedHash, want)
	}

	// 201 响应是令牌**唯一**一次出现的地方，所以这里只能断言「不带哈希」：
	// 明文 token 是本次响应的一部分（契约 §3 响应表：成功时就是 {"node":..., "token":...}）。
	createdBody := rr.Body.String()
	if strings.Contains(createdBody, storedHash) {
		t.Fatalf("201 响应里出现了 token_hash: %s", createdBody)
	}
	if strings.Contains(createdBody, "token_hash") || strings.Contains(createdBody, "tokenHash") {
		t.Fatalf("201 响应里出现了令牌哈希字段名: %s", createdBody)
	}

	// 详情读端点：顶层无 token 键、body 里没有明文/哈希。
	detail := env.do(t, http.MethodGet, env.withNS("/v1/nodes/"+nodeID), nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("GET /v1/nodes/{id} status = %d; body=%s", detail.Code, detail.Body.String())
	}
	detailBody := decode(t, detail)
	assertNoTokenKey(t, "节点详情", detailBody)
	assertNoTokenLeak(t, "节点详情", detail.Body.String(), created.token, storedHash)
	if got := mustStr(t, detailBody["node"].(map[string]any), "id"); got != nodeID {
		t.Fatalf("详情返回的 node.id = %q, want %q", got, nodeID)
	}

	// 列表读端点：同样一个字都不吐。
	list := env.do(t, http.MethodGet, env.withNS("/v1/nodes"), nil)
	if list.Code != http.StatusOK {
		t.Fatalf("GET /v1/nodes status = %d; body=%s", list.Code, list.Body.String())
	}
	listBody := decode(t, list)
	assertNoTokenKey(t, "节点列表", listBody)
	assertNoTokenLeak(t, "节点列表", list.Body.String(), created.token, storedHash)
	rows, ok := listBody["nodes"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("GET /v1/nodes 里应当恰好有 1 台节点: %s", list.Body.String())
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("列表项不是对象: %v", rows[0])
	}
	if _, ok := row["token"]; ok {
		t.Fatalf("列表项出现了 token 字段: %v", row)
	}
	if _, ok := row["tokenHash"]; ok {
		t.Fatalf("列表项出现了 tokenHash 字段: %v", row)
	}
}

// TestCreateNodeDuplicateNameConflict 契约 §6.5 / D7：唯一性是**命名空间内**的
// UNIQUE(namespace_id, name)：同 ns 同名 → 409 name_taken；另一个 ns 同名 → 201。
func TestCreateNodeDuplicateNameConflict(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	body := map[string]any{"name": "mbp-16"}

	first := env.do(t, http.MethodPost, env.withNS("/v1/nodes"), body)
	created := mustCreateNode(t, first)

	// 同命名空间第二次：409 + code=name_taken（不是 500「UNIQUE constraint failed」）。
	dup := env.do(t, http.MethodPost, env.withNS("/v1/nodes"), body)
	if dup.Code != http.StatusConflict {
		t.Fatalf("同名注册 status = %d, want 409; body=%s", dup.Code, dup.Body.String())
	}
	if got := mustStr(t, decode(t, dup), "code"); got != "name_taken" {
		t.Fatalf("同名注册 code = %q, want name_taken; body=%s", got, dup.Body.String())
	}

	// 冲突没有写出第二台机器。
	nodes, err := env.db.ListNodes(ctx, env.nsID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != mustStr(t, created.node, "id") {
		t.Fatalf("同命名空间里应当只有最初那台节点，实际 %d 台: %v", len(nodes), nodes)
	}

	// 另一个命名空间同名 → 201（隔离维度是 namespace，不是 org）。
	staging := env.seedNamespace(t, "staging", "staging")
	rr := env.do(t, http.MethodPost, withNSID("/v1/nodes", staging.ID), body)
	other := mustCreateNode(t, rr)
	if got := mustStr(t, other.node, "namespaceId"); got != staging.ID {
		t.Fatalf("staging 节点 namespaceId = %q, want %q", got, staging.ID)
	}
	if other.token == created.token {
		t.Fatal("两个节点拿到了同一个令牌（令牌必须逐节点随机）")
	}
}

// TestCreateNodeTokenWorksForWSHandshake 契约 §6.6 / D8：令牌真能用。
//
// **不用 fake**：起一个真 httptest 服务器（控制台路由 + 节点 WS 端点，接线与生产 mux 同形），
// 用 coder/websocket 真连一次，再发 hello 等 welcome。只有 welcome 回来才证明
// 「HTTP 创建的令牌 → nodes.token_hash → 连接身份 → hello 校验」这条链路真的通。
//
// 鉴权失败的口径按既有冻结断言（internal/ws/node_test.go:215 TestUnauthorizedNoUpgrade）：
// 无 token → 401，假 token → 403。issue 里把「错 token」写成 401，但既有断言更强也更细，
// 这里不削弱它（契约 D8 已记录这处偏差）。
func TestCreateNodeTokenWorksForWSHandshake(t *testing.T) {
	env := newConsoleEnv(t)

	// 1) 走真实 HTTP 建节点拿令牌（不直调 store）。
	created := mustCreateNode(t, env.do(t, http.MethodPost, env.withNS("/v1/nodes"),
		map[string]any{"name": "ws-node"}))
	nodeID := mustStr(t, created.node, "id")

	// 2) 真服务器：`/` 挂控制台 mux，再叠上节点 WS 端点（"GET /v1/node/stream" 更具体，优先生效）。
	srvMux := http.NewServeMux()
	srvMux.Handle("/", env.mux)
	srvMux.Handle("GET /v1/node/stream", noStore(ws.NewNodeHandler(env.db)))
	srv := httptest.NewServer(srvMux)
	t.Cleanup(srv.Close)

	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/node/stream"
	dial := func(token string) (*websocket.Conn, *http.Response, error) {
		opts := &websocket.DialOptions{}
		if token != "" {
			opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + token}}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return websocket.Dial(ctx, streamURL, opts)
	}

	// 3) 正确令牌：必须真的 Upgrade（101）。
	conn, resp, err := dial(created.token)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("用刚创建的令牌连 WS 失败: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	if resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("WS 握手 status = %v, want 101", resp)
	}

	// 4) hello → welcome：welcome.node_id 必须等于 HTTP 创建出来的那台节点。
	//    node_id 留空 = 「我就是令牌对应的那台机器」（协议要求非空时必须一致，见 applyHello）。
	hello, err := json.Marshal(map[string]any{
		"type":     ws.NodeFrameHello,
		"protocol": ws.ProtocolVersion,
	})
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = conn.Write(wctx, websocket.MessageText, hello)
	wcancel()
	if err != nil {
		t.Fatalf("发 hello 失败: %v", err)
	}

	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, msg, err := conn.Read(rctx)
	rcancel()
	if err != nil {
		t.Fatalf("读 welcome 帧失败: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(msg, &frame); err != nil {
		t.Fatalf("welcome 帧不是 JSON: %v; raw=%s", err, msg)
	}
	if frame["type"] != ws.NodeFrameWelcome {
		t.Fatalf("hello 之后第一帧 type = %v, want %q（握手失败就不该发 welcome）: %s",
			frame["type"], ws.NodeFrameWelcome, msg)
	}
	if frame["node_id"] != nodeID {
		t.Fatalf("welcome.node_id = %v, want %q（令牌必须绑到这台上）", frame["node_id"], nodeID)
	}

	// 5) 假令牌 → 403（既有冻结口径，见函数注释）。
	t.Run("假 token → 403", func(t *testing.T) {
		conn, resp, err := dial("ant_node_wrong")
		if err == nil {
			_ = conn.CloseNow()
			t.Fatal("假令牌竟然连上了")
		}
		if resp == nil {
			t.Fatal("假令牌没有 HTTP 响应")
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("假令牌 status = %d, want 403", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusSwitchingProtocols {
			t.Fatal("假令牌下发了 101（鉴权必须在 Upgrade 之前）")
		}
	})

	// 6) 无令牌 → 401。
	t.Run("无 token → 401", func(t *testing.T) {
		conn, resp, err := dial("")
		if err == nil {
			_ = conn.CloseNow()
			t.Fatal("不带令牌竟然连上了")
		}
		if resp == nil {
			t.Fatal("无令牌没有 HTTP 响应")
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("无令牌 status = %d, want 401", resp.StatusCode)
		}
	})
}

// TestCreateNodeNameValidation 契约 §6 附加边界 / D6：节点名规则
// `^[a-z0-9][a-z0-9._-]{0,62}$`，**不 trim**（带空白即非法）。
//
// 名字是运维用来认机器的主键：把写错的输入静默改对（trim/小写化）比直接拒绝危险得多。
func TestCreateNodeNameValidation(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	valid := []string{"a", "mbp-16", "demo_worker.v2"}
	for _, name := range valid {
		t.Run("合法/"+name, func(t *testing.T) {
			rr := env.do(t, http.MethodPost, env.withNS("/v1/nodes"), map[string]any{"name": name})
			created := mustCreateNode(t, rr)
			if got := mustStr(t, created.node, "name"); got != name {
				t.Fatalf("node.name = %q, want %q（名字必须原样落库）", got, name)
			}
		})
	}

	invalid := []struct {
		reason string
		name   string
	}{
		{"空名", ""}, // 空名走的是「name 必填」，仍应是 400 + invalid_name
		{"全空白", "   "},
		{"大写", "UPPER"},
		{"含空格", "has space"},
		{"短横线开头", "-lead"},
		{"点开头", ".dot"},
		{"64 字符", strings.Repeat("a", store.NodeNameMaxLen+1)},
		{"中文", "节点一"},
	}
	for _, tc := range invalid {
		t.Run("非法/"+tc.reason, func(t *testing.T) {
			rr := env.do(t, http.MethodPost, env.withNS("/v1/nodes"), map[string]any{"name": tc.name})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("name=%q status = %d, want 400; body=%s", tc.name, rr.Code, rr.Body.String())
			}
			m := decode(t, rr)
			if got := mustStr(t, m, "code"); got != "invalid_name" {
				t.Fatalf("name=%q code = %q, want invalid_name; body=%s", tc.name, got, rr.Body.String())
			}
			// 文案要能指导修复：必须带上 "name"。
			if msg := mustStr(t, m, "error"); !strings.Contains(msg, "name") {
				t.Fatalf("name=%q 的错误文案没提 name: %q", tc.name, msg)
			}

			// 非法名字绝不能悄悄落库。
			nodes, err := env.db.ListNodes(ctx, env.nsID)
			if err != nil {
				t.Fatalf("list nodes: %v", err)
			}
			for _, n := range nodes {
				if n.Name == tc.name {
					t.Fatalf("非法名字 %q 被落库了", tc.name)
				}
			}
		})
	}

	// 只有 3 个合法名落了库。
	nodes, err := env.db.ListNodes(ctx, env.nsID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != len(valid) {
		t.Fatalf("落库节点数 = %d, want %d（其余都是非法名字）", len(nodes), len(valid))
	}
}

// TestCreateNodeDoesNotCreateInstances 契约 §6 附加边界 / D5：
// body 里的 `instances` 是客户端的**建议值**，不落库——
// 不建 node_instances 行，也不改 nodes.max_instances（那是安全硬顶）。
//
// 实例是节点在 hello 帧里申报的（契约 Q5）；中枢预创建槽位等于凭空宣布产能，
// 派发器会把任务推给一台并不存在的槽。
func TestCreateNodeDoesNotCreateInstances(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	created := mustCreateNode(t, env.do(t, http.MethodPost, env.withNS("/v1/nodes"), map[string]any{
		"name":      "mbp-16",
		"instances": 2,
	}))
	nodeID := mustStr(t, created.node, "id")

	// ① 没有建出任何槽位行。
	var count int
	if err := env.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_instances WHERE node_id = ?`, nodeID).Scan(&count); err != nil {
		t.Fatalf("count node_instances: %v", err)
	}
	if count != 0 {
		t.Fatalf("node_instances 行数 = %d, want 0（instances 只接受不落库）", count)
	}

	// ② max_instances 仍是默认硬顶，没有被「建议值」静默改写（2 < 20，很容易看不出来，
	//    所以这里断言的是「等于默认值」而不是「大于等于建议值」）。
	if got, ok := created.node["maxInstances"].(float64); !ok || int(got) != store.DefaultMaxInstances {
		t.Fatalf("node.maxInstances = %v, want %d（硬顶不被建议值改写）",
			created.node["maxInstances"], store.DefaultMaxInstances)
	}
	var storedMax int
	if err := env.db.QueryRowContext(ctx,
		`SELECT max_instances FROM nodes WHERE id = ?`, nodeID).Scan(&storedMax); err != nil {
		t.Fatalf("查 max_instances: %v", err)
	}
	if storedMax != store.DefaultMaxInstances {
		t.Fatalf("库里 max_instances = %d, want %d", storedMax, store.DefaultMaxInstances)
	}

	// ③ 负数实例是非法的（契约 §3 响应表：instances must be >= 0）。
	neg := env.do(t, http.MethodPost, env.withNS("/v1/nodes"), map[string]any{
		"name":      "mbp-neg",
		"instances": -1,
	})
	if neg.Code != http.StatusBadRequest {
		t.Fatalf("instances=-1 status = %d, want 400; body=%s", neg.Code, neg.Body.String())
	}
	if msg := mustStr(t, decode(t, neg), "error"); !strings.Contains(msg, "instances") {
		t.Fatalf("instances=-1 的错误文案没提 instances: %q", msg)
	}
}

// TestCreateNodeRequiresSession 契约 §3 响应表：无会话 → 401（写端点绝不能匿名可写）。
func TestCreateNodeRequiresSession(t *testing.T) {
	env := newConsoleEnv(t)

	rr := env.doNoCookie(t, http.MethodPost, env.withNS("/v1/nodes"))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("无 Cookie status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	// 没会话就是没会话：不能因为「少了 cookie」而漏出任何节点。
	nodes, err := env.db.ListNodes(context.Background(), env.nsID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("401 之后库里多出了 %d 台节点", len(nodes))
	}
}

// TestCreateNodeNamespaceQueryWins 契约 §3：query 与 body 都带 namespaceId 时，**query 优先**
// （namespaceParam 的口径；写端点与读端点共用同一份规则，避免「同一参数两个位置谁赢」分叉）。
func TestCreateNodeNamespaceQueryWins(t *testing.T) {
	env := newConsoleEnv(t)

	created := mustCreateNode(t, env.do(t, http.MethodPost, env.withNS("/v1/nodes"), map[string]any{
		"name":        "mbp-16",
		"namespaceId": "ns_bogus",
	}))

	if got := mustStr(t, created.node, "namespaceId"); got != env.nsID {
		t.Fatalf("node.namespaceId = %q, want %q（query 必须优先于 body）", got, env.nsID)
	}

	// 落库的也是 query 指的那个命名空间。
	var storedNS string
	if err := env.db.QueryRowContext(context.Background(),
		`SELECT namespace_id FROM nodes WHERE id = ?`, mustStr(t, created.node, "id")).Scan(&storedNS); err != nil {
		t.Fatalf("查 namespace_id: %v", err)
	}
	if storedNS != env.nsID {
		t.Fatalf("库里 namespace_id = %q, want %q", storedNS, env.nsID)
	}
}

// TestCreateNodeNamespaceInBody 契约 §3 / D1：namespaceId 也可以只放在 body（query 优先、body 兜底）。
//
// 为什么需要这条正向用例：只测「body 带 `*` → 400」无法证明「query 为空时 handler 真的去读 body」
// ——一个只读 query 的实现也会让那条否定用例通过。
func TestCreateNodeNamespaceInBody(t *testing.T) {
	env := newConsoleEnv(t)

	created := mustCreateNode(t, env.do(t, http.MethodPost, "/v1/nodes", map[string]any{
		"namespaceId": env.nsID,
		"name":        "body-ns",
	}))
	if got := mustStr(t, created.node, "namespaceId"); got != env.nsID {
		t.Fatalf("node.namespaceId = %q, want %q（body 兜底）", got, env.nsID)
	}
	var storedNS string
	if err := env.db.QueryRowContext(context.Background(),
		`SELECT namespace_id FROM nodes WHERE id = ?`, mustStr(t, created.node, "id")).Scan(&storedNS); err != nil {
		t.Fatalf("查 namespace_id: %v", err)
	}
	if storedNS != env.nsID {
		t.Fatalf("库里 namespace_id = %q, want %q", storedNS, env.nsID)
	}

	// query 带空值（?namespaceId=）时同样回落到 body（namespaceParam 的口径）。
	created2 := mustCreateNode(t, env.do(t, http.MethodPost, "/v1/nodes?namespaceId=", map[string]any{
		"namespaceId": env.nsID,
		"name":        "body-ns-2",
	}))
	if got := mustStr(t, created2.node, "namespaceId"); got != env.nsID {
		t.Fatalf("空 query + body namespaceId 的 node.namespaceId = %q, want %q", got, env.nsID)
	}
}
