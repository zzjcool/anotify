package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// 命名空间资源域 / 配置子资源 / 聚合计数的 HTTP 契约测试（v4 api-conventions §7.5 / W5）。
//
// 这些用例刻意只走真实入口：真实 store（内存库）+ 真实会话中间件 + 真实 mux 路由，
// 断言的是**wire 形状**（四键信封、rv1 令牌、配置 wrapper、Status 错误体），
// 而不是 handler 的内部字段 —— 前端与 CLI 只认 wire。

// nsTestEnv 是一套命名空间域的测试环境。
type nsTestEnv struct {
	t      *testing.T
	db     *store.DB
	mux    http.Handler
	userID string
	nsID   string // 注册用户时建出的默认命名空间（slug = default）
	cookie *http.Cookie
}

func newNamespaceTestEnv(t *testing.T) *nsTestEnv {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	user := &store.User{ID: store.NewUserID(), Username: "ns-user", DisplayName: "命名空间管理员", CreatedAt: store.Now()}
	ns, err := db.RegisterUser(context.Background(), user)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	sm := auth.NewSessionManager(db, 0, false)
	sess, err := sm.Create(user.ID, "ns test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	deps := &serverDeps{DB: db, Session: sm.Middleware}
	mux := http.NewServeMux()
	newNamespaceAPI(deps).register(mux)

	return &nsTestEnv{
		t: t, db: db, mux: mux, userID: user.ID, nsID: ns.ID,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}
}

// do 发一次请求（cookie == nil 表示未登录）。
func (e *nsTestEnv) do(method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	return e.doRaw(method, path, reader, cookie)
}

// doRaw 发一次原始请求体（用于构造「未知字段」这类精确 JSON）。
func (e *nsTestEnv) doRaw(method, path string, body io.Reader, cookie *http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, body)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// authed 发一次已登录请求。
func (e *nsTestEnv) authed(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(method, path, body, e.cookie)
}

// nsrJSON 断言状态码并解出 JSON 对象。
func nsrJSON(t *testing.T, rec *httptest.ResponseRecorder, want int) map[string]any {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("%s → %d, want %d (body=%s)", rec.Result().Status, rec.Code, want, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

// nsrReason 断言 Status 错误体的 reason。
func nsrReason(t *testing.T, rec *httptest.ResponseRecorder, wantCode int, wantReason string) map[string]any {
	t.Helper()
	obj := nsrJSON(t, rec, wantCode)
	if got := nsrStr(t, obj, "reason"); got != wantReason {
		t.Fatalf("Status.reason = %q, want %q (body=%s)", got, wantReason, rec.Body.String())
	}
	if got := nsrStr(t, obj, "kind"); got != kindStatus {
		t.Fatalf("Status.kind = %q, want %q", got, kindStatus)
	}
	return obj
}

func nsrObj(t *testing.T, obj map[string]any, key string) map[string]any {
	t.Helper()
	raw, ok := obj[key]
	if !ok || raw == nil {
		t.Fatalf("缺少字段 %q (obj=%v)", key, obj)
	}
	out, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("字段 %q 不是对象：%T", key, raw)
	}
	return out
}

func nsrArr(t *testing.T, obj map[string]any, key string) []any {
	t.Helper()
	raw, ok := obj[key]
	if !ok || raw == nil {
		t.Fatalf("缺少字段 %q (obj=%v)", key, obj)
	}
	out, ok := raw.([]any)
	if !ok {
		t.Fatalf("字段 %q 不是数组：%T", key, raw)
	}
	return out
}

func nsrStr(t *testing.T, obj map[string]any, key string) string {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("缺少字段 %q (obj=%v)", key, obj)
	}
	out, ok := raw.(string)
	if !ok {
		t.Fatalf("字段 %q 不是字符串：%T", key, raw)
	}
	return out
}

func nsrNum(t *testing.T, obj map[string]any, key string) float64 {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("缺少字段 %q (obj=%v)", key, obj)
	}
	out, ok := raw.(float64)
	if !ok {
		t.Fatalf("字段 %q 不是数字：%T", key, raw)
	}
	return out
}

// nsrResourceVersion 断言 metadata.resourceVersion 是 rv1 令牌并返回它。
func nsrResourceVersion(t *testing.T, obj map[string]any) string {
	t.Helper()
	meta := nsrObj(t, obj, "metadata")
	rv := nsrStr(t, meta, "resourceVersion")
	if !strings.HasPrefix(rv, "rv1:") {
		t.Fatalf("metadata.resourceVersion = %q, want rv1: 前缀", rv)
	}
	return rv
}

// nsrCreateNamespace 用 HTTP 入口建一个命名空间，返回 (slug, uid, resourceVersion)。
func nsrCreateNamespace(t *testing.T, env *nsTestEnv, slug, displayName string) (string, string, string) {
	t.Helper()
	rec := env.authed(http.MethodPost, "/v1/namespaces", map[string]any{
		"metadata": map[string]any{"name": slug, "displayName": displayName},
	})
	obj := nsrJSON(t, rec, http.StatusCreated)
	if got := nsrStr(t, obj, "kind"); got != kindNamespace {
		t.Fatalf("POST /v1/namespaces kind = %q, want %q", got, kindNamespace)
	}
	meta := nsrObj(t, obj, "metadata")
	if got := nsrStr(t, meta, "name"); got != slug {
		t.Fatalf("metadata.name = %q, want %q", got, slug)
	}
	if got := nsrStr(t, meta, "displayName"); got != displayName {
		t.Fatalf("metadata.displayName = %q, want %q", got, displayName)
	}
	return slug, nsrStr(t, meta, "uid"), nsrResourceVersion(t, obj)
}

// TestNamespaceResourceListCreateGet 覆盖 list / create / get 的信封形状。
func TestNamespaceResourceListCreateGet(t *testing.T) {
	env := newNamespaceTestEnv(t)

	// --- list：NamespaceList + items[]（空列表是 []，不是 null） ---
	listRec := env.authed(http.MethodGet, "/v1/namespaces", nil)
	list := nsrJSON(t, listRec, http.StatusOK)
	if got := nsrStr(t, list, "kind"); got != kindNamespaceList {
		t.Fatalf("列表 kind = %q, want %q", got, kindNamespaceList)
	}
	if got := nsrStr(t, list, "apiVersion"); got != apiVersionV1 {
		t.Fatalf("列表 apiVersion = %q, want %q", got, apiVersionV1)
	}
	items := nsrArr(t, list, "items")
	if len(items) != 1 {
		t.Fatalf("注册后应有 1 个命名空间，得到 %d（items=%v）", len(items), items)
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] 不是对象：%T", items[0])
	}
	if got := nsrStr(t, first, "kind"); got != kindNamespace {
		t.Fatalf("items[0].kind = %q, want %q", got, kindNamespace)
	}
	firstMeta := nsrObj(t, first, "metadata")
	if got := nsrStr(t, firstMeta, "name"); got != "default" {
		t.Fatalf("items[0].metadata.name = %q, want default", got)
	}
	if got := nsrStr(t, firstMeta, "uid"); !strings.HasPrefix(got, "ns_") {
		t.Fatalf("items[0].metadata.uid = %q, want ns_ 前缀", got)
	}
	if ts := nsrStr(t, firstMeta, "creationTimestamp"); ts == "" {
		t.Fatal("items[0].metadata.creationTimestamp 为空，want RFC3339")
	}
	nsrResourceVersion(t, first)
	// spec 恒为 {}（V4-D1：没有 seedBuiltinCapabilities 这类开关）
	if spec := nsrObj(t, first, "spec"); len(spec) != 0 {
		t.Fatalf("Namespace.spec = %v, want {}", spec)
	}
	// status.counts 四项都必须真实出现（新库全 0，但键必须在）
	counts := nsrObj(t, nsrObj(t, first, "status"), "counts")
	for _, key := range []string{"capabilities", "nodes", "sources", "tasks"} {
		nsrNum(t, counts, key)
	}

	// --- create ---
	slug, uid, _ := nsrCreateNamespace(t, env, "team-a", "团队 A")
	if !strings.HasPrefix(uid, "ns_") {
		t.Fatalf("metadata.uid = %q, want ns_ 前缀", uid)
	}

	// --- get：单对象信封，uid 与创建时一致 ---
	got := nsrJSON(t, env.authed(http.MethodGet, "/v1/namespaces/"+slug, nil), http.StatusOK)
	if gotKind := nsrStr(t, got, "kind"); gotKind != kindNamespace {
		t.Fatalf("GET kind = %q, want %q", gotKind, kindNamespace)
	}
	gotMeta := nsrObj(t, got, "metadata")
	if gotUID := nsrStr(t, gotMeta, "uid"); gotUID != uid {
		t.Fatalf("GET metadata.uid = %q, want %q", gotUID, uid)
	}
	if disp := nsrStr(t, gotMeta, "displayName"); disp != "团队 A" {
		t.Fatalf("GET metadata.displayName = %q, want 团队 A", disp)
	}
	nsrResourceVersion(t, got)

	// --- 未知命名空间 → 404 NotFound ---
	nsrReason(t, env.authed(http.MethodGet, "/v1/namespaces/nope", nil), http.StatusNotFound, reasonNotFound)
}

// TestNamespaceResourceCreateDuplicateSlug slug 已占用必须 409 AlreadyExists（不是 500）。
func TestNamespaceResourceCreateDuplicateSlug(t *testing.T) {
	env := newNamespaceTestEnv(t)
	nsrCreateNamespace(t, env, "dup", "第一次")

	rec := env.authed(http.MethodPost, "/v1/namespaces", map[string]any{
		"metadata": map[string]any{"name": "dup", "displayName": "第二次"},
	})
	nsrReason(t, rec, http.StatusConflict, reasonAlreadyExists)
}

// TestNamespaceResourcePatchCAS PATCH 只改 displayName，必须带 CAS 令牌。
func TestNamespaceResourcePatchCAS(t *testing.T) {
	env := newNamespaceTestEnv(t)
	slug, uid, _ := nsrCreateNamespace(t, env, "team-b", "旧名字")
	path := "/v1/namespaces/" + slug

	// 缺 metadata.resourceVersion → 400 resource_version_required
	rec := env.authed(http.MethodPatch, path, map[string]any{"metadata": map[string]any{"displayName": "新名字"}})
	nsrReason(t, rec, http.StatusBadRequest, reasonResourceVersionRequired)

	// 旧令牌 → 409 + details.current.resourceVersion（客户端据此重建令牌）
	cur := nsrJSON(t, env.authed(http.MethodGet, path, nil), http.StatusOK)
	curRV := nsrResourceVersion(t, cur)
	stale := "rv1:" + uid + ":999"
	rec = env.authed(http.MethodPatch, path, map[string]any{
		"metadata": map[string]any{"displayName": "新名字", "resourceVersion": stale},
	})
	conflict := nsrReason(t, rec, http.StatusConflict, reasonResourceVersionConflict)
	details := nsrObj(t, conflict, "details")
	current := nsrObj(t, details, "current")
	if got := nsrStr(t, current, "resourceVersion"); got != curRV {
		t.Fatalf("details.current.resourceVersion = %q, want %q", got, curRV)
	}

	// metadata.name 与路径不一致 → 400 name_immutable（slug 是身份，不可改）
	rec = env.authed(http.MethodPatch, path, map[string]any{
		"metadata": map[string]any{"name": "other", "displayName": "新名字", "resourceVersion": curRV},
	})
	nsrReason(t, rec, http.StatusBadRequest, reasonNameImmutable)

	// 正确令牌 → 200，displayName 生效、令牌前进
	rec = env.authed(http.MethodPatch, path, map[string]any{
		"metadata": map[string]any{"displayName": "新名字", "resourceVersion": curRV},
	})
	patched := nsrJSON(t, rec, http.StatusOK)
	if got := nsrStr(t, nsrObj(t, patched, "metadata"), "displayName"); got != "新名字" {
		t.Fatalf("PATCH 后 displayName = %q, want 新名字", got)
	}
	nextRV := nsrResourceVersion(t, patched)
	if nextRV == curRV {
		t.Fatalf("PATCH 后 resourceVersion 未前进（仍为 %q）", curRV)
	}
	// slug 不变
	if got := nsrStr(t, nsrObj(t, patched, "metadata"), "name"); got != slug {
		t.Fatalf("PATCH 后 metadata.name = %q, want %q", got, slug)
	}
	// 旧令牌现在应冲突（证明 CAS 真的生效，不是只看存在性）
	rec = env.authed(http.MethodPatch, path, map[string]any{
		"metadata": map[string]any{"displayName": "又改", "resourceVersion": curRV},
	})
	nsrReason(t, rec, http.StatusConflict, reasonResourceVersionConflict)
}

// TestNamespaceResourceDeleteCAS DELETE 的令牌只在 query；缺失 400、成功 200、最后一个 400。
func TestNamespaceResourceDeleteCAS(t *testing.T) {
	env := newNamespaceTestEnv(t)
	slug, _, rv := nsrCreateNamespace(t, env, "doomed", "待删")
	path := "/v1/namespaces/" + slug

	// 缺 ?resourceVersion → 400 resource_version_required（不静默 last-write-wins）
	nsrReason(t, env.authed(http.MethodDelete, path, nil), http.StatusBadRequest, reasonResourceVersionRequired)

	// 正确令牌 → 204 无响应体，随后 404
	rec := env.authed(http.MethodDelete, path+"?resourceVersion="+rv, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE 正确令牌 → %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "" {
		t.Fatalf("DELETE 204 不应带响应体，得到 %q", body)
	}
	nsrReason(t, env.authed(http.MethodGet, path, nil), http.StatusNotFound, reasonNotFound)

	// 最后一个命名空间不能删（400）：只剩注册时那个默认 ns
	list := nsrJSON(t, env.authed(http.MethodGet, "/v1/namespaces", nil), http.StatusOK)
	items := nsrArr(t, list, "items")
	if len(items) != 1 {
		t.Fatalf("删除后应只剩 1 个命名空间，得到 %d", len(items))
	}
	only, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] 不是对象：%T", items[0])
	}
	onlySlug := nsrStr(t, nsrObj(t, only, "metadata"), "name")
	onlyRV := nsrResourceVersion(t, only)
	rec = env.authed(http.MethodDelete, "/v1/namespaces/"+onlySlug+"?resourceVersion="+onlyRV, nil)
	nsrReason(t, rec, http.StatusBadRequest, reasonBadRequest)
}

// TestNamespaceResourceConfigRoundTrip 覆盖 ns 配置 wrapper 的 GET→PUT 往返与两个计数器的分工。
func TestNamespaceResourceConfigRoundTrip(t *testing.T) {
	env := newNamespaceTestEnv(t)
	ctx := context.Background()
	path := "/v1/namespaces/default/config"

	// 造一台节点的配置行：用来证明 ns 级写不碰机器级 configRevision（C18 的分层）。
	node := &store.Node{ID: store.NewNodeID(), Name: "node-a"}
	if _, err := env.db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	nodeCfg := &store.NodeConfig{NodeID: node.ID, Vars: map[string]string{}, Limits: map[string]any{}, ResourceVersion: 1}
	if err := env.db.PutNodeConfig(ctx, nodeCfg); err != nil {
		t.Fatalf("put node config: %v", err)
	}
	nodeRevision := nodeCfg.Revision

	// --- GET：空默认 ---
	got := nsrJSON(t, env.authed(http.MethodGet, path, nil), http.StatusOK)
	rv0 := nsrStr(t, nsrObj(t, got, "metadata"), "resourceVersion")
	if !strings.HasPrefix(rv0, "rv1:") {
		t.Fatalf("config metadata.resourceVersion = %q, want rv1: 前缀", rv0)
	}
	if vars := nsrObj(t, got, "vars"); len(vars) != 0 {
		t.Fatalf("初始 vars = %v, want {}", vars)
	}
	if rev := nsrStr(t, got, "namespaceVarsRevision"); rev != "0" {
		t.Fatalf("初始 namespaceVarsRevision = %q, want 0", rev)
	}

	// --- PUT：只接 metadata.resourceVersion + vars ---
	putBody := map[string]any{
		"metadata": map[string]any{"resourceVersion": rv0},
		"vars":     map[string]string{"SOURCE_ROOT": "/srv/src", "NOTIFY_URL": "https://example.test/hook"},
	}
	put := nsrJSON(t, env.authed(http.MethodPut, path, putBody), http.StatusOK)
	vars := nsrObj(t, put, "vars")
	if got := nsrStr(t, vars, "SOURCE_ROOT"); got != "/srv/src" {
		t.Fatalf("PUT 后 vars.SOURCE_ROOT = %q, want /srv/src", got)
	}
	rv1 := nsrStr(t, nsrObj(t, put, "metadata"), "resourceVersion")
	if rv1 == rv0 {
		t.Fatalf("PUT 后 resourceVersion 未前进（仍为 %q）", rv0)
	}
	if rev := nsrStr(t, put, "namespaceVarsRevision"); rev != "1" {
		t.Fatalf("PUT 后 namespaceVarsRevision = %q, want 1", rev)
	}

	// --- GET 读回：vars 与 revision 都持久化了 ---
	again := nsrJSON(t, env.authed(http.MethodGet, path, nil), http.StatusOK)
	if got := nsrStr(t, nsrObj(t, again, "vars"), "NOTIFY_URL"); got != "https://example.test/hook" {
		t.Fatalf("读回 vars.NOTIFY_URL = %q", got)
	}
	if rev := nsrStr(t, again, "namespaceVarsRevision"); rev != "1" {
		t.Fatalf("读回 namespaceVarsRevision = %q, want 1", rev)
	}

	// --- 第二次 PUT：namespaceVarsRevision 单调递增，令牌跟着前进 ---
	second := map[string]any{
		"metadata": map[string]any{"resourceVersion": rv1},
		"vars":     map[string]string{"SOURCE_ROOT": "/srv/src2"},
	}
	put2 := nsrJSON(t, env.authed(http.MethodPut, path, second), http.StatusOK)
	if rev := nsrStr(t, put2, "namespaceVarsRevision"); rev != "2" {
		t.Fatalf("第二次 PUT 后 namespaceVarsRevision = %q, want 2", rev)
	}
	if vars := nsrObj(t, put2, "vars"); len(vars) != 1 {
		t.Fatalf("PUT 是整份替换，第二条之后应有 1 个 var，得到 %v", vars)
	}

	// --- 显式空对象 = 清空（`{}` 不能被当成「没给」，否则用户永远清不掉默认值） ---
	rv2 := nsrStr(t, nsrObj(t, put2, "metadata"), "resourceVersion")
	cleared := nsrJSON(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata": map[string]any{"resourceVersion": rv2},
		"vars":     map[string]string{},
	}), http.StatusOK)
	if vars := nsrObj(t, cleared, "vars"); len(vars) != 0 {
		t.Fatalf("清空后 vars = %v, want {}", vars)
	}
	if rev := nsrStr(t, cleared, "namespaceVarsRevision"); rev != "3" {
		t.Fatalf("清空后 namespaceVarsRevision = %q, want 3", rev)
	}

	// --- 分层：ns 级写不碰机器级 configRevision ---
	after, err := env.db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	if after.Revision != nodeRevision {
		t.Fatalf("ns 配置写入改动了节点 configRevision：%d → %d", nodeRevision, after.Revision)
	}

	// --- 拒收：缺令牌 400 ---
	nsrReason(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata": map[string]any{},
		"vars":     map[string]string{"A_1": "v"},
	}), http.StatusBadRequest, reasonResourceVersionRequired)

	// --- 拒收：vars 值非法（键必须 ^[A-Z][A-Z0-9_]{0,63}$）→ 400 value_invalid ---
	cur := nsrJSON(t, env.authed(http.MethodGet, path, nil), http.StatusOK)
	curRV := nsrStr(t, nsrObj(t, cur, "metadata"), "resourceVersion")
	nsrReason(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata": map[string]any{"resourceVersion": curRV},
		"vars":     map[string]string{"lower_case": "v"},
	}), http.StatusBadRequest, reasonValueInvalid)

	// --- 拒收：namespaceVarsRevision 只读，写在 body 里 → 400 unknown_field ---
	nsrReason(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata":              map[string]any{"resourceVersion": curRV},
		"vars":                  map[string]string{},
		"namespaceVarsRevision": "9",
	}), http.StatusBadRequest, reasonUnknownField)

	// --- 拒收：写 status → 400 status_not_writable ---
	nsrReason(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata": map[string]any{"resourceVersion": curRV},
		"vars":     map[string]string{},
		"status":   map[string]any{"counts": map[string]any{}},
	}), http.StatusBadRequest, reasonStatusNotWritable)
}

// TestNamespaceResourceSystemConfig 覆盖系统配置 wrapper 的 GET→PUT 往返与 CAS。
func TestNamespaceResourceSystemConfig(t *testing.T) {
	env := newNamespaceTestEnv(t)
	path := "/v1/system/config"

	got := nsrJSON(t, env.authed(http.MethodGet, path, nil), http.StatusOK)
	rv0 := nsrStr(t, nsrObj(t, got, "metadata"), "resourceVersion")
	if rv0 != "1" {
		t.Fatalf("系统配置初始 resourceVersion = %q, want 1（单行表默认行已存在）", rv0)
	}
	ret := nsrObj(t, got, "retention")
	if nsrNum(t, ret, "eventTtlSec") != float64(store.DefaultRetentionEventTTLSec) {
		t.Fatalf("默认 eventTtlSec = %v, want %d", ret["eventTtlSec"], store.DefaultRetentionEventTTLSec)
	}
	if nsrNum(t, ret, "resultTtlSec") != float64(store.DefaultRetentionResultTTLSec) {
		t.Fatalf("默认 resultTtlSec = %v, want %d", ret["resultTtlSec"], store.DefaultRetentionResultTTLSec)
	}

	// 缺令牌 → 400
	nsrReason(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata":  map[string]any{},
		"retention": map[string]any{"eventTtlSec": 10, "taskTtlSec": 20, "resultTtlSec": 30},
	}), http.StatusBadRequest, reasonResourceVersionRequired)

	// 缺 retention 字段 → 400（0 = 永久保留，漏字段不能被读成 0）
	nsrReason(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata":  map[string]any{"resourceVersion": rv0},
		"retention": map[string]any{"eventTtlSec": 10, "taskTtlSec": 20},
	}), http.StatusBadRequest, reasonValueInvalid)

	// 正常写 → 200，retention 往返，令牌前进
	put := nsrJSON(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata":  map[string]any{"resourceVersion": rv0},
		"retention": map[string]any{"eventTtlSec": 10, "taskTtlSec": 20, "resultTtlSec": 30},
	}), http.StatusOK)
	rv1 := nsrStr(t, nsrObj(t, put, "metadata"), "resourceVersion")
	if rv1 == rv0 {
		t.Fatalf("PUT 后 resourceVersion 未前进（仍为 %q）", rv0)
	}
	putRet := nsrObj(t, put, "retention")
	if nsrNum(t, putRet, "eventTtlSec") != 10 || nsrNum(t, putRet, "taskTtlSec") != 20 || nsrNum(t, putRet, "resultTtlSec") != 30 {
		t.Fatalf("PUT 回显 retention = %v, want {10,20,30}", putRet)
	}

	// GET 读回同一份事实
	readBack := nsrJSON(t, env.authed(http.MethodGet, path, nil), http.StatusOK)
	backRet := nsrObj(t, readBack, "retention")
	if nsrNum(t, backRet, "eventTtlSec") != 10 || nsrNum(t, backRet, "taskTtlSec") != 20 || nsrNum(t, backRet, "resultTtlSec") != 30 {
		t.Fatalf("读回 retention = %v, want {10,20,30}", backRet)
	}

	// 旧令牌 → 409 + details.current.resourceVersion
	conflict := nsrReason(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata":  map[string]any{"resourceVersion": rv0},
		"retention": map[string]any{"eventTtlSec": 1, "taskTtlSec": 1, "resultTtlSec": 1},
	}), http.StatusConflict, reasonResourceVersionConflict)
	current := nsrObj(t, nsrObj(t, conflict, "details"), "current")
	if got := nsrStr(t, current, "resourceVersion"); got != rv1 {
		t.Fatalf("details.current.resourceVersion = %q, want %q", got, rv1)
	}

	// 显式 0 必须被接受（永久保留是合法选择，但要写出来）
	zero := nsrJSON(t, env.authed(http.MethodPut, path, map[string]any{
		"metadata":  map[string]any{"resourceVersion": rv1},
		"retention": map[string]any{"eventTtlSec": 0, "taskTtlSec": 0, "resultTtlSec": 0},
	}), http.StatusOK)
	if nsrNum(t, nsrObj(t, zero, "retention"), "eventTtlSec") != 0 {
		t.Fatalf("显式 0 未被接受：%v", zero)
	}
}

// TestNamespaceResourceStats 聚合计数必须**只**算本命名空间（跨 ns 泄漏会直接误导容量判断）。
func TestNamespaceResourceStats(t *testing.T) {
	env := newNamespaceTestEnv(t)
	ctx := context.Background()

	// default 里建一个真实能力；另一个 ns 什么都不建
	cap := &store.CapabilityVersion{
		NamespaceID: env.nsID,
		Name:        "fixer",
		Version:     1,
		Spec: artifact.Spec{
			Params: json.RawMessage(`{"type":"object"}`),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}
	if err := env.db.CreateCapability(ctx, cap); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	if err := env.db.CreateSource(ctx, &store.Source{NamespaceID: env.nsID, Name: "manual-1", Kind: "manual"}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	nsrCreateNamespace(t, env, "empty-one", "空的")

	// default：capabilities=1, sources=1
	defaultStats := nsrJSON(t, env.authed(http.MethodGet, "/v1/namespaces/default/stats", nil), http.StatusOK)
	defaultCounts := nsrObj(t, defaultStats, "counts")
	if got := nsrNum(t, defaultCounts, "capabilities"); got != 1 {
		t.Fatalf("default counts.capabilities = %v, want 1", got)
	}
	if got := nsrNum(t, defaultCounts, "sources"); got != 1 {
		t.Fatalf("default counts.sources = %v, want 1", got)
	}
	if got := nsrNum(t, defaultCounts, "tasks"); got != 0 {
		t.Fatalf("default counts.tasks = %v, want 0", got)
	}
	// stats 里的相位分布是同源派生量（空即 {}）
	if phases := nsrObj(t, defaultStats, "stats"); len(phases) == 0 {
		t.Fatalf("stats 对象为空，want 至少含 tasksByPhase / edges：%v", defaultStats)
	}

	// empty-one：四项全 0（不继承 default 的任何计数）
	emptyStats := nsrJSON(t, env.authed(http.MethodGet, "/v1/namespaces/empty-one/stats", nil), http.StatusOK)
	emptyCounts := nsrObj(t, emptyStats, "counts")
	for _, key := range []string{"capabilities", "nodes", "sources", "tasks"} {
		if got := nsrNum(t, emptyCounts, key); got != 0 {
			t.Fatalf("empty-one counts.%s = %v, want 0（计数不得跨 ns 泄漏）", key, got)
		}
	}

	// 对象信封里的 status 与 /stats 必须同源（同一份计数口径）
	obj := nsrJSON(t, env.authed(http.MethodGet, "/v1/namespaces/default", nil), http.StatusOK)
	objCounts := nsrObj(t, nsrObj(t, obj, "status"), "counts")
	if got := nsrNum(t, objCounts, "capabilities"); got != 1 {
		t.Fatalf("GET /v1/namespaces/default status.counts.capabilities = %v, want 1", got)
	}
}

// TestNamespaceResourceSpecSwitchRejected V4-D1 刻意不做的 spec 开关必须被拒（不静默忽略）。
func TestNamespaceResourceSpecSwitchRejected(t *testing.T) {
	env := newNamespaceTestEnv(t)
	body := `{"apiVersion":"anotify/v1","kind":"Namespace","metadata":{"name":"with-switch"},"spec":{"seedBuiltinCapabilities":true}}`
	nsrReason(t, env.doRaw(http.MethodPost, "/v1/namespaces", strings.NewReader(body), env.cookie),
		http.StatusBadRequest, reasonUnknownField)

	// 未知顶层字段同样拒（不静默忽略）
	nsrReason(t, env.doRaw(http.MethodPost, "/v1/namespaces",
		strings.NewReader(`{"metadata":{"name":"x-unknown"},"seedBuiltinCapabilities":true}`), env.cookie),
		http.StatusBadRequest, reasonUnknownField)

	// 写 status → 400 status_not_writable
	nsrReason(t, env.doRaw(http.MethodPost, "/v1/namespaces",
		strings.NewReader(`{"metadata":{"name":"x-status"},"status":{"counts":{}}}`), env.cookie),
		http.StatusBadRequest, reasonStatusNotWritable)

	// 非法 slug → 400 value_invalid（而不是 store 的普通 error 变成 500）
	nsrReason(t, env.authed(http.MethodPost, "/v1/namespaces", map[string]any{
		"metadata": map[string]any{"name": "Bad_Slug"},
	}), http.StatusBadRequest, reasonValueInvalid)

	// PATCH 带 spec 开关同样拒
	slug, _, rv := nsrCreateNamespace(t, env, "clean-ns", "干净")
	nsrReason(t, env.doRaw(http.MethodPatch, "/v1/namespaces/"+slug, strings.NewReader(
		`{"metadata":{"displayName":"改名","resourceVersion":"`+rv+`"},"spec":{"seedBuiltinCapabilities":true}}`),
		env.cookie), http.StatusBadRequest, reasonUnknownField)
}

// TestNamespaceResourceRequiresSession cluster 级资源也要登录（未登录 401，不是 404）。
func TestNamespaceResourceRequiresSession(t *testing.T) {
	env := newNamespaceTestEnv(t)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/namespaces"},
		{http.MethodPost, "/v1/namespaces"},
		{http.MethodGet, "/v1/namespaces/default"},
		{http.MethodPatch, "/v1/namespaces/default"},
		{http.MethodDelete, "/v1/namespaces/default?resourceVersion=rv1:x:1"},
		{http.MethodGet, "/v1/namespaces/default/config"},
		{http.MethodPut, "/v1/namespaces/default/config"},
		{http.MethodGet, "/v1/namespaces/default/stats"},
		{http.MethodGet, "/v1/system/config"},
		{http.MethodPut, "/v1/system/config"},
	}
	for _, tc := range cases {
		rec := env.do(tc.method, tc.path, map[string]any{"metadata": map[string]any{"name": "x"}}, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未登录 → %d, want 401 (body=%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestNamespaceResourceDeleteBlockedAndForce 钉「删非空命名空间」的两档语义：
// 默认删除被阻塞 → 409 Conflict + details.causes 里给出 blocking 判据，且**一行都不许删**；
// ?force=true 才是显式级联（204，随后 404，里面的对象随级联消失）。
func TestNamespaceResourceDeleteBlockedAndForce(t *testing.T) {
	env := newNamespaceTestEnv(t)
	ctx := context.Background()

	slug, uid, rv := nsrCreateNamespace(t, env, "has-content", "有依赖的命名空间")
	// 放一个真实能力行：命名空间里「有对象」是阻塞删除的唯一判据来源（不是 UI 状态）。
	if err := env.db.CreateCapability(ctx, &store.CapabilityVersion{
		NamespaceID: uid,
		Name:        "review",
		Version:     1,
		Spec: artifact.Spec{
			Params: []byte(`{"type":"object"}`),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}); err != nil {
		t.Fatalf("seed capability: %v", err)
	}
	path := "/v1/namespaces/" + slug

	// ① 默认删除：非空 → 409 Conflict（不是 400 也不是 500），并给出阻塞判据。
	rec := env.authed(http.MethodDelete, path+"?resourceVersion="+rv, nil)
	obj := nsrReason(t, rec, http.StatusConflict, reasonConflict)
	details := nsrObj(t, obj, "details")
	causes := nsrArr(t, details, "causes")
	if len(causes) == 0 {
		t.Fatalf("409 必须在 details.causes 里给出阻塞判据: %s", rec.Body.String())
	}
	cause, ok := causes[0].(map[string]any)
	if !ok {
		t.Fatalf("causes[0] 不是对象: %T", causes[0])
	}
	if field := nsrStr(t, cause, "field"); field != "blocking" {
		t.Fatalf("causes[0].field = %q, want blocking（客户端靠它分支）", field)
	}
	if msg := nsrStr(t, cause, "message"); !strings.Contains(msg, "dependent") {
		t.Fatalf("causes[0].message 应说明有依赖对象，得到 %q", msg)
	}
	// 人话（Status.message）也必须说清「非空」，不能只是一个 409 码。
	if msg := nsrStr(t, obj, "message"); !strings.Contains(msg, "not empty") {
		t.Fatalf("Status.message = %q, want 说明命名空间非空", msg)
	}

	// ② 被阻塞的删除是零写：ns 还在（拿得到新令牌）、能力行也还在。
	kept := nsrJSON(t, env.authed(http.MethodGet, path, nil), http.StatusOK)
	rvNow := nsrResourceVersion(t, kept)
	caps, err := env.db.ListCapabilities(ctx, uid)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if len(caps) != 1 {
		t.Fatalf("被阻塞的删除不该动命名空间里的对象，能力行数 = %d, want 1", len(caps))
	}

	// ③ ?force=true 是显式级联：204 → 之后 404，能力行随级联消失。
	rec = env.authed(http.MethodDelete, path+"?force=true&resourceVersion="+rvNow, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("force 删除期望 204，得到 %d：%s", rec.Code, rec.Body.String())
	}
	nsrReason(t, env.authed(http.MethodGet, path, nil), http.StatusNotFound, reasonNotFound)
	caps, err = env.db.ListCapabilities(ctx, uid)
	if err != nil {
		t.Fatalf("list capabilities after force: %v", err)
	}
	if len(caps) != 0 {
		t.Fatalf("force 删除应级联清掉能力行，实际还剩 %d 条", len(caps))
	}
}
