package server

// Node 资源域（cluster-scoped + 配置子资源 + 身份令牌）的 HTTP 验收。
//
// 覆盖的**契约点**（不是实现细节）：
//   - POST /v1/nodes 不回 token（明文只能从 /tokens 拿，W4 顺序固定）；
//   - POST /v1/nodes/{name}/tokens 回 NodeToken，且明文只出现一次（后续读端点不含它）；
//   - 对不存在的节点签 token → 404（不隐式创建）；
//   - config GET 默认 wrapper（namespaces=["*"]、令牌非空）；PUT 缺 CAS 400 / 旧值 409+details.current；
//   - PATCH / DELETE 缺 CAS → 400；
//   - POST /v1/nodes/config/batch 逐台结果可部分成功（真节点 ok + 不存在节点 ok:false）；
//   - ?namespace= 时 status 额外给 namespaceVars/effectiveVars 并标 context.namespace；
//   - 全部路由都过 session 中间件。
//
// 这些用例共用一套真库 + 真 ServeMux（不起 WebSocket：升级端点的 409 分支用
// 空 Hub 就能验，帧内容属于 ws 包自己的测试）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// nodeTestUserID 是夹具里那个已登录用户（AuthorizeNamespace 会核它存在）。
const nodeTestUserID = "usr_nodetest"

// nodeTestEnv 是一套跑起来的 Node 资源域。
type nodeTestEnv struct {
	t    *testing.T
	db   *store.DB
	mux  *http.ServeMux
	api  *nodeAPI
	deps *serverDeps
	ns   *store.Namespace
}

// newNodeTestEnv 建库 / 用户 / 命名空间 + 挂好路由。
func newNodeTestEnv(t *testing.T) *nodeTestEnv {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "anotify.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.CreateUser(&store.User{ID: nodeTestUserID, Username: "node-tester", CreatedAt: store.Now()}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	ns, err := db.CreateNamespace(context.Background(), "default", "默认")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	deps := &serverDeps{
		DB:  db,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() int64 { return store.Now() * 1000 },
		// 会话中间件在真实部署里是「Cookie → userID」。这里直接注入已登录身份：
		// 本文件的验收点是资源语义，「路由确实被 session 包裹」由
		// TestNodeResourceRoutesAreSessionWrapped 单独证明。
		Session: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), nodeTestUserID)))
			})
		},
	}
	api := newNodeAPI(deps)
	mux := http.NewServeMux()
	api.register(mux)
	return &nodeTestEnv{t: t, db: db, mux: mux, api: api, deps: deps, ns: ns}
}

// do 发一个请求（body 为 string 时按原样发送，便于构造坏 JSON）。
func (e *nodeTestEnv) do(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	switch v := body.(type) {
	case nil:
	case string:
		buf.WriteString(v)
	default:
		if err := json.NewEncoder(&buf).Encode(v); err != nil {
			e.t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// createNode 登记一台节点并返回它的信封（断言 201）。
func (e *nodeTestEnv) createNode(name string, spec map[string]any) map[string]any {
	e.t.Helper()
	body := map[string]any{"apiVersion": "anotify/v1", "kind": "Node", "metadata": map[string]any{"name": name}}
	if spec != nil {
		body["spec"] = spec
	}
	rr := e.do(http.MethodPost, "/v1/nodes", body)
	if rr.Code != http.StatusCreated {
		e.t.Fatalf("create node %q = %d, want 201 (body=%s)", name, rr.Code, rr.Body.String())
	}
	return decodeNodeDoc(e.t, rr)
}

// nodeRV 取某台节点当前的 CAS 令牌（读端点给的那一份）。
func (e *nodeTestEnv) nodeRV(name string) (uid, rv string) {
	e.t.Helper()
	doc := decodeNodeDoc(e.t, e.do(http.MethodGet, "/v1/nodes/"+name, nil))
	meta := nodeObject(e.t, doc, "metadata")
	uid, _ = meta["uid"].(string)
	rv, _ = meta["resourceVersion"].(string)
	if uid == "" || rv == "" {
		e.t.Fatalf("node %q metadata 缺 uid/resourceVersion: %v", name, meta)
	}
	return uid, rv
}

// configRV 取某台节点配置子资源的 CAS 令牌。
func (e *nodeTestEnv) configRV(name string) string {
	e.t.Helper()
	rr := e.do(http.MethodGet, "/v1/nodes/"+name+"/config", nil)
	if rr.Code != http.StatusOK {
		e.t.Fatalf("get config %q = %d, want 200 (body=%s)", name, rr.Code, rr.Body.String())
	}
	meta := nodeObject(e.t, decodeNodeDoc(e.t, rr), "metadata")
	rv, _ := meta["resourceVersion"].(string)
	if rv == "" {
		e.t.Fatalf("config %q metadata.resourceVersion 为空: %v", name, meta)
	}
	return rv
}

// ---------- 小工具（断言用） ----------

func decodeNodeDoc(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

// nodeObject 按路径取一个嵌套对象（缺失即失败，便于一眼看出断的是哪一层）。
func nodeObject(t *testing.T, doc map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := nodeValue(t, doc, path...)
	obj, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("path %v 不是对象: %T", path, cur)
	}
	return obj
}

func nodeValue(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: 上一级不是对象（%T）", path, cur)
		}
		cur, ok = obj[key]
		if !ok {
			t.Fatalf("path %v: 缺 key %q", path, key)
		}
	}
	return cur
}

func nodeString(t *testing.T, doc map[string]any, path ...string) string {
	t.Helper()
	s, _ := nodeValue(t, doc, path...).(string)
	return s
}

func nodeSlice(t *testing.T, doc map[string]any, path ...string) []any {
	t.Helper()
	items, ok := nodeValue(t, doc, path...).([]any)
	if !ok {
		t.Fatalf("path %v 不是数组", path)
	}
	return items
}

// wantStatus 断言一个 Status 错误体。
func wantStatus(t *testing.T, rr *httptest.ResponseRecorder, code int, reason string) map[string]any {
	t.Helper()
	if rr.Code != code {
		t.Fatalf("status = %d, want %d (body=%s)", rr.Code, code, rr.Body.String())
	}
	doc := decodeNodeDoc(t, rr)
	if kind := nodeString(t, doc, "kind"); kind != kindStatus {
		t.Fatalf("kind = %q, want %q (body=%s)", kind, kindStatus, rr.Body.String())
	}
	if got := int(nodeValue(t, doc, "code").(float64)); got != code {
		t.Fatalf("code = %d, want %d", got, code)
	}
	if got := nodeString(t, doc, "reason"); got != reason {
		t.Fatalf("reason = %q, want %q (body=%s)", got, reason, rr.Body.String())
	}
	return doc
}

// ---------- 用例 ----------

// TestNodeResourceCreateReturnsEnvelopeWithoutToken 钉住 W4 的顺序：
// 登记只建离线 Node，令牌只能从 /tokens 拿（本端点连 token 字段都不出现）。
func TestNodeResourceCreateReturnsEnvelopeWithoutToken(t *testing.T) {
	env := newNodeTestEnv(t)
	body := `{"apiVersion":"anotify/v1","kind":"Node","metadata":{"name":"node-a"}}`
	rr := env.do(http.MethodPost, "/v1/nodes", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /v1/nodes = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	// 明文 / 哈希都不许出现（"token" 是两者的公共子串）。
	if raw := rr.Body.String(); strings.Contains(raw, "token") {
		t.Fatalf("POST /v1/nodes 回显了 token 相关字段（明文只能从 /tokens 拿）: %s", raw)
	}
	doc := decodeNodeDoc(t, rr)
	if kind := nodeString(t, doc, "kind"); kind != kindNode {
		t.Fatalf("kind = %q, want %q", kind, kindNode)
	}
	meta := nodeObject(t, doc, "metadata")
	if name := meta["name"]; name != "node-a" {
		t.Fatalf("metadata.name = %v, want node-a", name)
	}
	if meta["uid"] == nil || meta["uid"] == "" {
		t.Fatalf("metadata.uid 必须回填（= store 的 id）: %v", meta)
	}
	if meta["resourceVersion"] == nil || meta["resourceVersion"] == "" {
		t.Fatalf("metadata.resourceVersion 必须非空: %v", meta)
	}
	if ns, ok := meta["namespace"]; ok {
		t.Fatalf("Node 是 cluster 对象，metadata 不该有 namespace: %v", ns)
	}
	// 创建默认授权 ["*"]（W4：与本机申报取交集，权限只能收紧）。
	nsList := nodeSlice(t, doc, "spec", "namespaces")
	if len(nsList) != 1 || nsList[0] != "*" {
		t.Fatalf("spec.namespaces = %v, want [\"*\"]", nsList)
	}
	// status 恒存在（无事实给 {}，不是 null）。
	if _, ok := doc["status"].(map[string]any); !ok {
		t.Fatalf("status 必须是对象: %v", doc["status"])
	}
	// 库里确实有 token_hash（CreateNode 一定会生成），只是没有任何 HTTP 出口。
	n, err := env.db.GetNodeByName(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if n.TokenHash == "" {
		t.Fatalf("节点必须有 token_hash（明文只丢弃，不丢弃哈希）")
	}
}

// TestNodeResourceTokenRotatesAndPlaintextAppearsOnce 钉住 NodeToken 的 W4 语义：
// 响应是动作文档、明文只出现一次、再次 POST = 轮换（旧 token 立刻失效）。
func TestNodeResourceTokenRotatesAndPlaintextAppearsOnce(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("node-a", nil)

	rr := env.do(http.MethodPost, "/v1/nodes/node-a/tokens", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST tokens = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	doc := decodeNodeDoc(t, rr)
	if kind := nodeString(t, doc, "kind"); kind != kindNodeToken {
		t.Fatalf("kind = %q, want %q", kind, kindNodeToken)
	}
	if name := nodeString(t, doc, "node", "name"); name != "node-a" {
		t.Fatalf("node.name = %q, want node-a", name)
	}
	first := nodeString(t, doc, "token")
	if !strings.HasPrefix(first, "ant_node_") {
		t.Fatalf("token = %q, want ant_node_ 前缀", first)
	}

	// 明文只返回这一次：读端点（单对象 / 配置）都不能含它。
	for _, path := range []string{"/v1/nodes/node-a", "/v1/nodes/node-a/config", "/v1/nodes"} {
		raw := env.do(http.MethodGet, path, nil).Body.String()
		if strings.Contains(raw, first) {
			t.Fatalf("GET %s 回显了令牌明文: %s", path, raw)
		}
	}

	// 再次 POST = 显式轮换。
	rr2 := env.do(http.MethodPost, "/v1/nodes/node-a/tokens", map[string]any{})
	if rr2.Code != http.StatusOK {
		t.Fatalf("rotate tokens = %d, want 200 (body=%s)", rr2.Code, rr2.Body.String())
	}
	second := nodeString(t, decodeNodeDoc(t, rr2), "token")
	if second == first {
		t.Fatalf("轮换后 token 未变（%q）", second)
	}

	ctx := context.Background()
	if _, err := env.db.GetNodeByTokenHash(ctx, store.HashNodeToken(first)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("旧 token 应立刻失效（按哈希查不到节点），实际 err=%v", err)
	}
	n, err := env.db.GetNodeByTokenHash(ctx, store.HashNodeToken(second))
	if err != nil {
		t.Fatalf("新 token 应能定位节点: %v", err)
	}
	if n.Name != "node-a" {
		t.Fatalf("新 token 定位到 %q, want node-a", n.Name)
	}
}

// TestNodeResourceTokenUnknownNodeIs404 钉住「签 token 不隐式创建节点」。
func TestNodeResourceTokenUnknownNodeIs404(t *testing.T) {
	env := newNodeTestEnv(t)
	rr := env.do(http.MethodPost, "/v1/nodes/ghost/tokens", map[string]any{})
	wantStatus(t, rr, http.StatusNotFound, reasonNotFound)

	nodes, err := env.db.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("签 token 不得隐式创建节点，实际有 %d 台", len(nodes))
	}
}

// TestNodeResourceConfigDefaults 钉住「没有配置行时给可用默认」（首次 PUT 要有令牌）。
func TestNodeResourceConfigDefaults(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("node-a", nil)

	rr := env.do(http.MethodGet, "/v1/nodes/node-a/config", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET config = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	doc := decodeNodeDoc(t, rr)
	nsList := nodeSlice(t, doc, "namespaces")
	if len(nsList) != 1 || nsList[0] != "*" {
		t.Fatalf("默认 namespaces = %v, want [\"*\"]", nsList)
	}
	if vars := nodeObject(t, doc, "vars"); len(vars) != 0 {
		t.Fatalf("默认 vars 应为空对象: %v", vars)
	}
	if limits := nodeObject(t, doc, "limits"); len(limits) != 0 {
		t.Fatalf("默认 limits 应为空对象: %v", limits)
	}
	if rv := nodeString(t, doc, "metadata", "resourceVersion"); rv == "" {
		t.Fatalf("默认 wrapper 必须给非空 resourceVersion（首次 PUT 的前置条件）")
	}
	if rev := nodeString(t, doc, "configRevision"); rev != "0" {
		t.Fatalf("默认 configRevision = %q, want \"0\"", rev)
	}
	// 配置子资源不是独立 kind（W5）：没有 apiVersion/kind/spec。
	for _, key := range []string{"apiVersion", "kind", "spec", "status"} {
		if _, ok := doc[key]; ok {
			t.Fatalf("config wrapper 不该有 %q 键: %s", key, rr.Body.String())
		}
	}
}

// TestNodeResourceConfigPutCAS 钉住配置写的 CAS：缺令牌 400、旧令牌 409 + details.current。
func TestNodeResourceConfigPutCAS(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("node-a", nil)
	nodeUID, _ := env.nodeRV("node-a")
	rv0 := env.configRV("node-a")

	// 缺 metadata.resourceVersion → 400 resource_version_required
	rr := env.do(http.MethodPut, "/v1/nodes/node-a/config", map[string]any{
		"namespaces": []string{"team-a"},
	})
	wantStatus(t, rr, http.StatusBadRequest, reasonResourceVersionRequired)

	// 旧令牌 → 409 + details.current.resourceVersion
	stale := fmt.Sprintf("rv1:%s:99", nodeUID)
	rr = env.do(http.MethodPut, "/v1/nodes/node-a/config", map[string]any{
		"metadata":   map[string]any{"resourceVersion": stale},
		"namespaces": []string{"team-a"},
	})
	conflict := wantStatus(t, rr, http.StatusConflict, reasonResourceVersionConflict)
	current := nodeString(t, conflict, "details", "current", "resourceVersion")
	if current == "" {
		t.Fatalf("409 必须给 details.current.resourceVersion: %s", rr.Body.String())
	}

	// 当前令牌 → 200，写生效、令牌前进
	rr = env.do(http.MethodPut, "/v1/nodes/node-a/config", map[string]any{
		"metadata":   map[string]any{"resourceVersion": rv0},
		"namespaces": []string{"team-a"},
		"vars":       map[string]string{"REGION": "cn"},
		"limits":     map[string]any{"total": 2},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT config = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	doc := decodeNodeDoc(t, rr)
	nsList := nodeSlice(t, doc, "namespaces")
	if len(nsList) != 1 || nsList[0] != "team-a" {
		t.Fatalf("namespaces = %v, want [\"team-a\"]", nsList)
	}
	if got := nodeString(t, doc, "vars", "REGION"); got != "cn" {
		t.Fatalf("vars.REGION = %q, want cn", got)
	}
	if rev := nodeString(t, doc, "configRevision"); rev != "1" {
		t.Fatalf("configRevision = %q, want \"1\"（每次成功写入 +1）", rev)
	}
	rv1 := nodeString(t, doc, "metadata", "resourceVersion")
	if rv1 == "" || rv1 == rv0 {
		t.Fatalf("写入后 resourceVersion 必须前进: %q → %q", rv0, rv1)
	}

	// 拿旧令牌再写 → 409（旧令牌不能复用）
	rr = env.do(http.MethodPut, "/v1/nodes/node-a/config", map[string]any{
		"metadata":   map[string]any{"resourceVersion": rv0},
		"namespaces": []string{"*"},
	})
	wantStatus(t, rr, http.StatusConflict, reasonResourceVersionConflict)

	// 只读派生字段不可提交 → 400 unknown_field（不静默忽略）
	rr = env.do(http.MethodPut, "/v1/nodes/node-a/config", map[string]any{
		"metadata":       map[string]any{"resourceVersion": rv1},
		"configRevision": "99",
	})
	wantStatus(t, rr, http.StatusBadRequest, reasonUnknownField)
}

// TestNodeResourcePatchRequiresCAS 钉住 PATCH 的 CAS 承载位置与 merge 语义。
func TestNodeResourcePatchRequiresCAS(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("node-a", nil)
	_, rv0 := env.nodeRV("node-a")

	// 缺 metadata.resourceVersion → 400
	rr := env.do(http.MethodPatch, "/v1/nodes/node-a", map[string]any{
		"spec": map[string]any{"tags": []string{"build"}},
	})
	wantStatus(t, rr, http.StatusBadRequest, reasonResourceVersionRequired)

	// body 的 metadata.name 与路径不一致 → 400 name_immutable
	rr = env.do(http.MethodPatch, "/v1/nodes/node-a", map[string]any{
		"metadata": map[string]any{"name": "node-b", "resourceVersion": rv0},
		"spec":     map[string]any{"tags": []string{"build"}},
	})
	wantStatus(t, rr, http.StatusBadRequest, reasonNameImmutable)

	// 正常 PATCH：tags + vars 生效，令牌前进
	rr = env.do(http.MethodPatch, "/v1/nodes/node-a", map[string]any{
		"metadata": map[string]any{"resourceVersion": rv0},
		"spec": map[string]any{
			"tags": []string{"build", "prod"},
			"vars": map[string]string{"TOOLCHAIN": "/opt/go"},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	doc := decodeNodeDoc(t, rr)
	tags := nodeSlice(t, doc, "spec", "tags")
	if len(tags) != 2 || tags[0] != "build" {
		t.Fatalf("spec.tags = %v, want [build prod]", tags)
	}
	if got := nodeString(t, doc, "spec", "vars", "TOOLCHAIN"); got != "/opt/go" {
		t.Fatalf("spec.vars.TOOLCHAIN = %q, want /opt/go", got)
	}
	rv1 := nodeString(t, doc, "metadata", "resourceVersion")
	if rv1 == rv0 {
		t.Fatalf("PATCH 后 resourceVersion 必须前进（Node.spec 与 config 共享同一底层版本）")
	}
}

// TestNodeResourceDeleteRequiresCAS 钉住 DELETE 的 CAS 只在 query、且 409 带当前令牌。
func TestNodeResourceDeleteRequiresCAS(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("node-a", nil)
	uid, rv0 := env.nodeRV("node-a")

	rr := env.do(http.MethodDelete, "/v1/nodes/node-a", nil)
	wantStatus(t, rr, http.StatusBadRequest, reasonResourceVersionRequired)

	rr = env.do(http.MethodDelete,
		"/v1/nodes/node-a?resourceVersion="+fmt.Sprintf("rv1:%s:99", uid), nil)
	conflict := wantStatus(t, rr, http.StatusConflict, reasonResourceVersionConflict)
	if nodeString(t, conflict, "details", "current", "resourceVersion") == "" {
		t.Fatalf("409 必须给 details.current.resourceVersion: %s", rr.Body.String())
	}

	rr = env.do(http.MethodDelete, "/v1/nodes/node-a?resourceVersion="+rv0, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	wantStatus(t, env.do(http.MethodGet, "/v1/nodes/node-a", nil), http.StatusNotFound, reasonNotFound)
}

// TestNodeResourceConfigBatchPartialSuccess 钉住「逐台原子、可部分成功」。
func TestNodeResourceConfigBatchPartialSuccess(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("node-a", nil)
	rv := env.configRV("node-a")

	body := map[string]any{"updates": []any{
		map[string]any{
			"name":     "node-a",
			"metadata": map[string]any{"resourceVersion": rv},
			"vars":     map[string]string{"REGION": "cn"},
		},
		map[string]any{
			"name":     "ghost",
			"metadata": map[string]any{"resourceVersion": fmt.Sprintf("rv1:%s:1", "nd_ghost")},
		},
	}}
	rr := env.do(http.MethodPost, "/v1/nodes/config/batch", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("batch = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if raw := rr.Body.String(); strings.Contains(raw, "ant_node_") {
		t.Fatalf("批量响应不得带凭证值: %s", raw)
	}
	doc := decodeNodeDoc(t, rr)
	items := nodeSlice(t, doc, "items")
	if len(items) != 2 {
		t.Fatalf("items 长度 = %d, want 2 (body=%s)", len(items), rr.Body.String())
	}

	okItem, _ := items[0].(map[string]any)
	if okItem["ok"] != true {
		t.Fatalf("第一台应成功: %v", okItem)
	}
	if name := okItem["name"]; name != "node-a" {
		t.Fatalf("items[0].name = %v, want node-a", name)
	}
	cfg, _ := okItem["config"].(map[string]any)
	if cfg == nil {
		t.Fatalf("成功项必须带 config wrapper: %v", okItem)
	}
	if got := nodeString(t, cfg, "vars", "REGION"); got != "cn" {
		t.Fatalf("config.vars.REGION = %q, want cn", got)
	}
	if rvNew := nodeString(t, cfg, "metadata", "resourceVersion"); rvNew == "" || rvNew == rv {
		t.Fatalf("批量写入后 config 令牌必须前进: %q → %q", rv, rvNew)
	}
	// 省略的字段 = 不动：只改 vars 绝不能把授权重置回 ["*"] 之外的东西。
	nsList := nodeSlice(t, cfg, "namespaces")
	if len(nsList) != 1 || nsList[0] != "*" {
		t.Fatalf("省略 namespaces 时应保持原值，实际 %v", nsList)
	}

	badItem, _ := items[1].(map[string]any)
	if badItem["ok"] != false {
		t.Fatalf("第二台（不存在的节点）应失败: %v", badItem)
	}
	if _, ok := badItem["config"]; ok {
		t.Fatalf("失败项不该有 config: %v", badItem)
	}
	errDoc, _ := badItem["error"].(map[string]any)
	if errDoc == nil {
		t.Fatalf("失败项必须带 Status 文档: %v", badItem)
	}
	if kind := errDoc["kind"]; kind != kindStatus {
		t.Fatalf("错误文档 kind = %v, want %v", kind, kindStatus)
	}
	if code, _ := errDoc["code"].(float64); int(code) != http.StatusNotFound {
		t.Fatalf("不存在节点的错误码 = %v, want 404", errDoc["code"])
	}
	if reason := errDoc["reason"]; reason != reasonNotFound {
		t.Fatalf("不存在节点的 reason = %v, want %v", reason, reasonNotFound)
	}
}

// TestNodeResourceGetNamespaceContext 钉住「有效值必须带 ns 上下文」（config.md §2.3）。
func TestNodeResourceGetNamespaceContext(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("node-a", map[string]any{
		"vars": map[string]string{"TOOLCHAIN": "/opt/go"},
	})
	nsCfg, err := env.db.GetNamespaceConfig(context.Background(), env.ns.ID)
	if err != nil {
		t.Fatalf("get namespace config: %v", err)
	}
	if _, err := env.db.PutNamespaceConfig(context.Background(), env.ns.ID,
		map[string]string{"REGION": "us"}, nsCfg.ResourceVersion); err != nil {
		t.Fatalf("put namespace config: %v", err)
	}

	// 不带 ns：只有机器级事实，没有 ns 上下文。
	plain := decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/node-a", nil))
	if _, ok := plain["context"]; ok {
		t.Fatalf("不带 ?namespace= 时不该有 context: %v", plain["context"])
	}
	if _, ok := nodeObject(t, plain, "status")["effectiveVars"]; ok {
		t.Fatalf("不带 ?namespace= 时不该有 effectiveVars")
	}

	rr := env.do(http.MethodGet, "/v1/nodes/node-a?namespace="+env.ns.ID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET ?namespace= = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	doc := decodeNodeDoc(t, rr)
	// context.namespace 是对外名字（slug）：客户端在 query 里写的是它，
	// 回显 store 的 ns_ id 会让同一个命名空间在 URL 与响应里叫两个名字。
	if got := nodeString(t, doc, "context", "namespace"); got != env.ns.Slug {
		t.Fatalf("context.namespace = %q, want %q", got, env.ns.Slug)
	}
	if got := nodeString(t, doc, "status", "namespaceVars", "REGION"); got != "us" {
		t.Fatalf("status.namespaceVars.REGION = %q, want us", got)
	}
	// 逐键解析 local > node > namespace，且来源必须能看见。
	if got := nodeString(t, doc, "status", "effectiveVars", "REGION", "value"); got != "us" {
		t.Fatalf("effectiveVars.REGION.value = %q, want us", got)
	}
	if got := nodeString(t, doc, "status", "effectiveVars", "REGION", "source"); got != store.VarSourceNamespace {
		t.Fatalf("effectiveVars.REGION.source = %q, want %q", got, store.VarSourceNamespace)
	}
	if got := nodeString(t, doc, "status", "effectiveVars", "TOOLCHAIN", "source"); got != store.VarSourceNode {
		t.Fatalf("effectiveVars.TOOLCHAIN.source = %q, want %q", got, store.VarSourceNode)
	}

	// ns 不存在 → 404（不是「静默给一份机器级结果」）。
	wantStatus(t, env.do(http.MethodGet, "/v1/nodes/node-a?namespace=ns_missing", nil),
		http.StatusNotFound, reasonNotFound)

	// 显式给了空的 ?namespace= → 400（不能让前端以为拿到了某个 ns 的解析结果）。
	wantStatus(t, env.do(http.MethodGet, "/v1/nodes/node-a?namespace=", nil),
		http.StatusBadRequest, reasonBadRequest)
}

// TestNodeResourceUpgrade 钉住升级动作的三个前置分支（帧内容属于 ws 包测试）。
func TestNodeResourceUpgrade(t *testing.T) {
	env := newNodeTestEnv(t)
	wantStatus(t, env.do(http.MethodPost, "/v1/nodes/ghost/upgrade", map[string]any{}),
		http.StatusNotFound, reasonNotFound)

	env.createNode("node-a", nil)
	env.deps.Hub = ws.NewHub(env.db, nil)

	// 没有升级源（部署里没带 agent 二进制）→ 501
	rr := env.do(http.MethodPost, "/v1/nodes/node-a/upgrade", map[string]any{})
	wantStatus(t, rr, http.StatusNotImplemented, "agent_binary_unavailable")

	// 有升级源、但节点没有活跃连接 → 409 node_offline
	env.api.setUpgradeSource(fakeNodeUpgradeSource{version: "9.9.9+test", sha: strings.Repeat("a", 64)})
	rr = env.do(http.MethodPost, "/v1/nodes/node-a/upgrade", map[string]any{})
	wantStatus(t, rr, http.StatusConflict, "node_offline")
}

// fakeNodeUpgradeSource 是升级源的测试替身（真实实现由装配层注入）。
type fakeNodeUpgradeSource struct {
	version string
	sha     string
}

func (f fakeNodeUpgradeSource) TargetVersion() string { return f.version }
func (f fakeNodeUpgradeSource) SHA256() string        { return f.sha }

// TestNodeResourceListEmptyIsArray 钉住不变量「空列表给 [] 不是 null」。
func TestNodeResourceListEmptyIsArray(t *testing.T) {
	env := newNodeTestEnv(t)
	rr := env.do(http.MethodGet, "/v1/nodes", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/nodes = %d, want 200", rr.Code)
	}
	doc := decodeNodeDoc(t, rr)
	if kind := nodeString(t, doc, "kind"); kind != kindNodeList {
		t.Fatalf("kind = %q, want %q", kind, kindNodeList)
	}
	if strings.Contains(rr.Body.String(), `"items":null`) {
		t.Fatalf("空列表必须是 [] 而不是 null: %s", rr.Body.String())
	}
	if items := nodeSlice(t, doc, "items"); len(items) != 0 {
		t.Fatalf("items = %v, want []", items)
	}

	// 有节点时列表也得能读（同一条 items 路径）。
	env.createNode("node-a", nil)
	items := nodeSlice(t, decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes", nil)), "items")
	if len(items) != 1 {
		t.Fatalf("items 长度 = %d, want 1", len(items))
	}
}

// TestNodeResourceRoutesAreSessionWrapped 证明 11 条路由都过了 session 中间件。
//
// 用一个「一律 401」的 Session 注入：只要有一条路由漏包，它就会返回 200/400/405
// 而不是 401，测试立刻红。
func TestNodeResourceRoutesAreSessionWrapped(t *testing.T) {
	env := newNodeTestEnv(t)
	deps := &serverDeps{
		DB:  env.db,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			})
		},
	}
	mux := http.NewServeMux()
	newNodeAPI(deps).register(mux)

	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/nodes"},
		{http.MethodPost, "/v1/nodes"},
		{http.MethodPost, "/v1/nodes/config/batch"},
		{http.MethodGet, "/v1/nodes/node-a"},
		{http.MethodPatch, "/v1/nodes/node-a"},
		{http.MethodDelete, "/v1/nodes/node-a?resourceVersion=x"},
		{http.MethodPost, "/v1/nodes/node-a/tokens"},
		{http.MethodGet, "/v1/nodes/node-a/config"},
		{http.MethodPut, "/v1/nodes/node-a/config"},
		{http.MethodPost, "/v1/nodes/node-a/drain"},
		{http.MethodPost, "/v1/nodes/node-a/upgrade"},
	}
	for _, rt := range routes {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401（该路由没有过 session 中间件）", rt.method, rt.path, rr.Code)
		}
	}
}

// TestNodeResourceTokenIsRealCredential 钉「节点令牌是真身份凭证，不是随机字符串」：
// 明文经 store.HashNodeToken 得到的哈希必须能反查到**同一台**节点（Hub 握手正是走
// 这条查询：internal/ws/hub.go 的 GetNodeByTokenHash），且存的就是这个哈希；
// 轮换后旧哈希查不到、新哈希查到同一台。
func TestNodeResourceTokenIsRealCredential(t *testing.T) {
	env := newNodeTestEnv(t)
	ctx := context.Background()
	created := env.createNode("node-cred", nil)
	uid, _ := nodeObject(t, created, "metadata")["uid"].(string)
	if uid == "" {
		t.Fatalf("创建信封缺 metadata.uid: %v", created)
	}

	// 登记时 store 已生成过一枚哈希（明文由 CreateNode 返回但 HTTP 丢弃）：轮换前记下它。
	before, err := env.db.GetNodeByName(ctx, "node-cred")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if before.TokenHash == "" {
		t.Fatal("登记后节点必须有 token_hash（否则永远无法握手）")
	}

	rr := env.do(http.MethodPost, "/v1/nodes/node-cred/tokens", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST tokens = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	plaintext := nodeString(t, decodeNodeDoc(t, rr), "token")
	hash := store.HashNodeToken(plaintext)

	// ① 抓到的明文不是随机串：它的哈希就是库里的身份凭据，且指向同一台节点。
	found, err := env.db.GetNodeByTokenHash(ctx, hash)
	if err != nil {
		t.Fatalf("HashNodeToken(明文) 必须能查到节点（握手路径）: %v", err)
	}
	if found.ID != uid {
		t.Fatalf("令牌哈希查到的是 %q，want 同一台节点 %q", found.ID, uid)
	}
	if found.TokenHash != hash {
		t.Fatalf("节点行上的 token_hash = %q, want %q（口令明文绝不能落库）", found.TokenHash, hash)
	}
	if found.TokenHash == plaintext {
		t.Fatal("token_hash 不能等于明文（必须是哈希）")
	}
	// ② 轮换之前的哈希立刻失效（旧凭据不能继续握手）。
	if _, err := env.db.GetNodeByTokenHash(ctx, before.TokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("轮换前那枚哈希应查不到节点，实际 err=%v", err)
	}

	// ③ 再轮换一次：旧哈希查不到、新哈希查到同一台（令牌是「唯一身份」，不是会话随机数）。
	rr = env.do(http.MethodPost, "/v1/nodes/node-cred/tokens", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("rotate tokens = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	next := nodeString(t, decodeNodeDoc(t, rr), "token")
	if next == plaintext {
		t.Fatalf("轮换后明文未变: %q", next)
	}
	if _, err := env.db.GetNodeByTokenHash(ctx, store.HashNodeToken(plaintext)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("旧明文的哈希应立刻失效，实际 err=%v", err)
	}
	nextHash := store.HashNodeToken(next)
	rotated, err := env.db.GetNodeByTokenHash(ctx, nextHash)
	if err != nil {
		t.Fatalf("新明文的哈希应能查到节点: %v", err)
	}
	if rotated.ID != uid || rotated.TokenHash != nextHash {
		t.Fatalf("新令牌必须钉回同一台节点: id=%q want %q, hash=%q want %q",
			rotated.ID, uid, rotated.TokenHash, nextHash)
	}
}

// TestNodeResourceConfigBatchMissingTokenIsItemError 钉批量写的逐项语义：
// 某台缺 metadata.resourceVersion 时**不是整请求 400**，而是该项 ok:false +
// error.reason=resource_version_required（一次提交里其它项照常成功）。
func TestNodeResourceConfigBatchMissingTokenIsItemError(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("node-batch", nil)
	rv := env.configRV("node-batch")

	rr := env.do(http.MethodPost, "/v1/nodes/config/batch", map[string]any{"updates": []any{
		// ① 缺令牌：一项都不许落库，但也不许把整批打回 400。
		map[string]any{"name": "node-batch", "vars": map[string]string{"NOPE": "1"}},
		// ② 同批里带令牌的那台照常成功。
		map[string]any{
			"name":     "node-batch",
			"metadata": map[string]any{"resourceVersion": rv},
			"vars":     map[string]string{"REGION": "cn"},
		},
	}})
	if rr.Code != http.StatusOK {
		t.Fatalf("批量写缺令牌不该是整请求 400/500，得到 %d：%s", rr.Code, rr.Body.String())
	}
	doc := decodeNodeDoc(t, rr)
	items := nodeSlice(t, doc, "items")
	if len(items) != 2 {
		t.Fatalf("items 长度 = %d, want 2 (body=%s)", len(items), rr.Body.String())
	}

	bad, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] 不是对象：%T", items[0])
	}
	if bad["ok"] != false {
		t.Fatalf("缺令牌的那项应 ok:false，得到 %v", bad)
	}
	if _, has := bad["config"]; has {
		t.Fatalf("失败项不该带 config：%v", bad)
	}
	errDoc, ok := bad["error"].(map[string]any)
	if !ok {
		t.Fatalf("失败项必须带 Status 文档：%v", bad)
	}
	if kind := errDoc["kind"]; kind != kindStatus {
		t.Fatalf("错误文档 kind = %v, want %v", kind, kindStatus)
	}
	if code, _ := errDoc["code"].(float64); int(code) != http.StatusBadRequest {
		t.Fatalf("缺令牌的错误码 = %v, want 400", errDoc["code"])
	}
	if reason := errDoc["reason"]; reason != reasonResourceVersionRequired {
		t.Fatalf("缺令牌的 reason = %v, want %q", reason, reasonResourceVersionRequired)
	}

	good, ok := items[1].(map[string]any)
	if !ok {
		t.Fatalf("items[1] 不是对象：%T", items[1])
	}
	if good["ok"] != true {
		t.Fatalf("同批里带令牌的那项应成功，得到 %v（整批不该被一项拖垮）", good)
	}
	cfg, ok := good["config"].(map[string]any)
	if !ok {
		t.Fatalf("成功项必须带 config wrapper：%v", good)
	}
	if got := nodeString(t, cfg, "vars", "REGION"); got != "cn" {
		t.Fatalf("成功项应写入 REGION=cn，得到 %q", got)
	}
	// 缺令牌那项的 vars 绝不能旁路落库。
	cfgNow := decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/node-batch/config", nil))
	if raw := cfgNow["vars"].(map[string]any); raw["NOPE"] != nil {
		t.Fatalf("缺令牌的更新不得写入，config.vars = %v", raw)
	}
}
