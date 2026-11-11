package server

// resource_source_test.go 覆盖 v4 Source 资源域 + 事件入口（#331 / W-src）的冻结契约。
//
// 夹具口径：真 SQLite + 真 store + 真 route/source service，只把**会话中间件**换成
// 「直接往 context 里放登录用户」——`serverDeps.Session` 就是资源域与鉴权之间唯一的
// 接缝，替换它不会绕过任何被测逻辑（ns 授权、CAS、脱敏、回执投影全部真实运行）。
//
// 每条用例对应任务卡里点名的硬约束，不做「先跑通再说」的空壳：
//   - 脱敏：secret 不出现、token 只出现在 hookUrl 里（Create/Get/List 三处）
//   - PATCH 省略 config = 保持凭据；换 kind → 400 source_kind_immutable
//   - DELETE/PATCH 的 CAS（缺 400 / 旧 409 + details.current）
//   - fire 回执形状（tasks/pruned 恒数组，键名冻结）
//   - dry-run 是预览文档：无 eventId/taskId，且**真的**不建任务 / 不写事件
//   - hook 由 token 定位命名空间

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
)

// sourceResClock 是夹具的固定时钟（秒）：让回执/事件时间可复现。
const sourceResClock = 1_700_000_000

// sourceResEnv 是 Source 资源域的测试环境。
type sourceResEnv struct {
	t    *testing.T
	db   *store.DB
	mux  http.Handler
	nsID string
	// nsSlug 是路径与 metadata.namespace 上的对外名字（store 事实表引用 nsID）。
	nsSlug string
}

// newSourceResEnv 建库 + 用户 + 命名空间 + 一个可解析的出口能力，并挂上与生产一致的
// Source 路由表（newSourceAPI(...).register —— 测的是真实注册结果，不是手抄的 pattern）。
func newSourceResEnv(t *testing.T) *sourceResEnv {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if cerr := db.Close(); cerr != nil {
			t.Errorf("close db: %v", cerr)
		}
	})

	userID, err := db.InsertUser(ctx, "", "srcuser", "Source User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	ns, err := db.CreateNamespace(ctx, "default", "默认")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	// 出口能力：targets 要在**源的命名空间**内可解析，否则建源就被 store 拒。
	// 参数 schema 留空（无 required），让 fire/dry-run 能真的走到 ready。
	if err := db.CreateCapability(ctx, &store.CapabilityVersion{
		NamespaceID: ns.ID,
		Name:        "notify",
		Version:     1,
		Spec: artifact.Spec{
			Params: []byte(`{"type":"object"}`),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	router := route.New(db)
	deps := &serverDeps{
		DB:      db,
		Router:  router,
		Sources: source.NewWithClock(db, router, func() int64 { return sourceResClock }),
		Now:     func() int64 { return sourceResClock * 1000 }, // 契约单位是毫秒（D1）
		Session: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), userID)))
			})
		},
	}
	mux := http.NewServeMux()
	newSourceAPI(deps).register(mux)

	return &sourceResEnv{t: t, db: db, mux: mux, nsID: ns.ID, nsSlug: ns.Slug}
}

// sourcesPath 是 List/创建路径；sourcePath 是单对象路径。
func (e *sourceResEnv) sourcesPath() string { return "/v1/namespaces/" + e.nsID + "/sources" }

func (e *sourceResEnv) sourcePath(name string) string {
	return e.sourcesPath() + "/" + name
}

// request 发一次请求并返回 recorder。body 可以是 nil / string（原样）/ 任意可 JSON 值。
func (e *sourceResEnv) request(method, target string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			e.t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest(method, target, reader))
	return rr
}

// doc 解响应体（失败时把原文带进失败信息，避免只看到 "unexpected end of JSON"）。
func (e *sourceResEnv) doc(rr *httptest.ResponseRecorder) map[string]any {
	e.t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("响应不是 JSON 对象（%d）：%s", rr.Code, rr.Body.String())
	}
	return out
}

// count 数一张表的行数（fire/dry-run 的副作用判定用它，比读回执更硬）。
func (e *sourceResEnv) count(table string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		e.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// createSource 走 HTTP 建一个源并返回 201 文档；非 201 直接失败。
func (e *sourceResEnv) createSource(spec map[string]any, meta map[string]any) map[string]any {
	e.t.Helper()
	body := map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   meta,
		"spec":       spec,
	}
	rr := e.request(http.MethodPost, e.sourcesPath(), body)
	if rr.Code != http.StatusCreated {
		e.t.Fatalf("创建源期望 201，得到 %d：%s", rr.Code, rr.Body.String())
	}
	return e.doc(rr)
}

// resourceVersion 取单对象的 metadata.resourceVersion（CAS 用例的令牌来源）。
func (e *sourceResEnv) resourceVersion(name string) string {
	e.t.Helper()
	rr := e.request(http.MethodGet, e.sourcePath(name), nil)
	if rr.Code != http.StatusOK {
		e.t.Fatalf("读取 %s 期望 200，得到 %d：%s", name, rr.Code, rr.Body.String())
	}
	return sourceResString(e.t, sourceResObj(e.t, e.doc(rr)["metadata"])["resourceVersion"])
}

// ---------- 断言小工具（全部 sourceRes 前缀：本包测试文件众多，避免撞名） ----------

func sourceResObj(t *testing.T, v any) map[string]any {
	t.Helper()
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("期望 JSON 对象，得到 %T：%v", v, v)
	}
	return obj
}

func sourceResArr(t *testing.T, v any) []any {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("期望 JSON 数组（空列表必须是 []，不能是 null），得到 %T：%v", v, v)
	}
	return arr
}

func sourceResString(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("期望字符串，得到 %T：%v", v, v)
	}
	return s
}

// sourceResAssertDoc 断言对象是 v4 资源信封（四键 + 服务端回填的 identity）。
func sourceResAssertDoc(t *testing.T, doc map[string]any, wantName, wantNS string) {
	t.Helper()
	if doc["apiVersion"] != "anotify/v1" {
		t.Fatalf("apiVersion 期望 anotify/v1，得到 %v", doc["apiVersion"])
	}
	if doc["kind"] != "Source" {
		t.Fatalf("kind 期望 Source，得到 %v", doc["kind"])
	}
	for _, key := range []string{"metadata", "spec", "status"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("信封缺 %q 键：%v", key, doc)
		}
	}
	meta := sourceResObj(t, doc["metadata"])
	if meta["name"] != wantName {
		t.Fatalf("metadata.name 期望 %q，得到 %v", wantName, meta["name"])
	}
	if meta["namespace"] != wantNS {
		t.Fatalf("metadata.namespace 期望 %q，得到 %v", wantNS, meta["namespace"])
	}
	rv := sourceResString(t, meta["resourceVersion"])
	if !strings.HasPrefix(rv, "rv1:") || !strings.Contains(rv, ":") {
		t.Fatalf("metadata.resourceVersion 必须是存储层的 rv1:<uid>:<rev> 令牌，得到 %q", rv)
	}
	if sourceResString(t, meta["creationTimestamp"]) == "" {
		t.Fatalf("metadata.creationTimestamp 必须由服务端回填 RFC3339")
	}
}

// sourceResAssertNoCredentialLeak 是脱敏的硬断言：
//
//	secret —— 响应体里一个字符都不许出现；
//	token  —— 只允许出现在授权读的 `/v1/hooks/<token>` 里（这正是 hookUrl 的语义），
//	          把合法的 URL 全部剔除后，剩下的正文里不许再有它。
func sourceResAssertNoCredentialLeak(t *testing.T, body, token, secret string) {
	t.Helper()
	if secret != "" && strings.Contains(body, secret) {
		t.Fatalf("响应体里出现了 config.secret 原文：%s", body)
	}
	rest := strings.ReplaceAll(body, srcHookPathPrefix+token, "")
	if token != "" && strings.Contains(rest, token) {
		t.Fatalf("token 出现在 hookUrl 之外的位置：%s", body)
	}
}

// sourceResAssertRawArray 断言某个顶层键编码成 JSON 数组（`[]` 而不是 `null`）。
func sourceResAssertRawArray(t *testing.T, rr *httptest.ResponseRecorder, key string) []any {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON 对象（%d）：%s", rr.Code, rr.Body.String())
	}
	field, ok := raw[key]
	if !ok {
		t.Fatalf("响应缺 %q 键：%s", key, rr.Body.String())
	}
	trimmed := bytes.TrimSpace(field)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		t.Fatalf("%q 必须是 JSON 数组（空列表给 []），得到 %s", key, rr.Body.String())
	}
	var arr []any
	if err := json.Unmarshal(field, &arr); err != nil {
		t.Fatalf("解 %q 失败：%v", key, err)
	}
	return arr
}

// ---------- 用例 ----------

// TestSourceResourceCreateGetMasksSecrets 验「GET 不回显 token/secret，只给 hookUrl/secretSet」
// 在 Create / Get / List 三处都成立，且非敏感 config 字段照常回显。
func TestSourceResourceCreateGetMasksSecrets(t *testing.T) {
	env := newSourceResEnv(t)
	const token = "src_tok_LEAKME"
	const secret = "sec_LEAKME"
	const wantHook = srcHookPathPrefix + token

	created := env.createSource(
		map[string]any{
			"kind": "webhook",
			"config": map[string]any{
				"token":  token,
				"secret": secret,
				"sample": map[string]any{"env": "prod"},
			},
			"targets": []any{map[string]any{"capability": "notify", "params": map[string]any{"text": "hi"}}},
		},
		map[string]any{"name": "deploy-hook", "displayName": "部署成功"},
	)
	sourceResAssertDoc(t, created, "deploy-hook", env.nsSlug)

	spec := sourceResObj(t, created["spec"])
	if spec["kind"] != "webhook" || spec["enabled"] != true {
		t.Fatalf("spec 投影不对：%v", spec)
	}
	targets := sourceResArr(t, spec["targets"])
	if len(targets) != 1 || sourceResObj(t, targets[0])["capability"] != "notify" {
		t.Fatalf("spec.targets 投影不对：%v", spec["targets"])
	}
	cfg := sourceResObj(t, spec["config"])
	if _, ok := cfg["token"]; ok {
		t.Fatalf("响应回显了 spec.config.token（必须换成 hookUrl）：%v", cfg)
	}
	if _, ok := cfg["secret"]; ok {
		t.Fatalf("响应回显了 spec.config.secret（必须换成 secretSet）：%v", cfg)
	}
	if cfg["hookUrl"] != wantHook {
		t.Fatalf("spec.config.hookUrl 期望 %q，得到 %v", wantHook, cfg["hookUrl"])
	}
	if cfg["secretSet"] != true {
		t.Fatalf("spec.config.secretSet 期望 true，得到 %v", cfg["secretSet"])
	}
	// 脱敏不能顺手把非敏感配置整份抹掉：sample 必须原样回显。
	if sample := sourceResObj(t, cfg["sample"]); sample["env"] != "prod" {
		t.Fatalf("非敏感 config 字段必须原样回显，sample=%v", cfg["sample"])
	}

	status := sourceResObj(t, created["status"])
	if status["hookUrl"] != wantHook {
		t.Fatalf("status.hookUrl 期望 %q，得到 %v", wantHook, status["hookUrl"])
	}
	if status["enabled"] != true || status["health"] != srcHealthOK || status["targetsResolved"] != true {
		t.Fatalf("status 派生不对（enabled/health/targetsResolved）：%v", status)
	}
	if _, ok := status["lastError"]; !ok {
		t.Fatalf("status.lastError 必须存在（无事实时给 null）：%v", status)
	}
	if status["lastError"] != nil {
		t.Fatalf("status.lastError 无事实时必须 null，得到 %v", status["lastError"])
	}
	sourceResAssertNoCredentialLeak(t, mustJSONString(t, created), token, secret)

	// 单对象读。
	rr := env.request(http.MethodGet, env.sourcePath("deploy-hook"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET 单对象期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	sourceResAssertNoCredentialLeak(t, rr.Body.String(), token, secret)
	gotCfg := sourceResObj(t, sourceResObj(t, env.doc(rr)["spec"])["config"])
	if _, ok := gotCfg["token"]; ok {
		t.Fatalf("GET 单对象回显了 token：%v", gotCfg)
	}
	if gotCfg["hookUrl"] != wantHook || gotCfg["secretSet"] != true {
		t.Fatalf("GET 单对象的读投影不对：%v", gotCfg)
	}

	// 列表。
	rr = env.request(http.MethodGet, env.sourcesPath(), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET 列表期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() == "" || strings.Contains(rr.Body.String(), `"items":null`) {
		t.Fatalf("列表 items 必须是数组：%s", rr.Body.String())
	}
	listDoc := env.doc(rr)
	if listDoc["kind"] != "SourceList" {
		t.Fatalf("列表 kind 期望 SourceList，得到 %v", listDoc["kind"])
	}
	items := sourceResArr(t, listDoc["items"])
	if len(items) != 1 {
		t.Fatalf("列表应有 1 条，得到 %d", len(items))
	}
	sourceResAssertNoCredentialLeak(t, rr.Body.String(), token, secret)
}

// TestSourceResourceWriteRejectsReadOnlyProjection 验写请求不许把读投影当配置写回去。
func TestSourceResourceWriteRejectsReadOnlyProjection(t *testing.T) {
	env := newSourceResEnv(t)
	for _, key := range []string{"hookUrl", "secretSet"} {
		body := map[string]any{
			"apiVersion": "anotify/v1",
			"kind":       "Source",
			"metadata":   map[string]any{"name": "proj-" + key},
			"spec": map[string]any{
				"kind":   "webhook",
				"config": map[string]any{key: "whatever"},
			},
		}
		rr := env.request(http.MethodPost, env.sourcesPath(), body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("写 spec.config.%s 期望 400，得到 %d：%s", key, rr.Code, rr.Body.String())
		}
		if reason := sourceResString(t, env.doc(rr)["reason"]); reason != reasonUnknownField {
			t.Fatalf("写 spec.config.%s 的 reason 期望 %q，得到 %q", key, reasonUnknownField, reason)
		}
	}

	// 校验失败的错误文本同样不能带凭据原文（只许点字段名）。
	const token = "src_err_TOKEN"
	rr := env.request(http.MethodPost, env.sourcesPath(), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "bad-config"},
		"spec": map[string]any{
			"kind":   "webhook",
			"config": map[string]any{"token": token, "bogus": 1},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("未知 config 字段期望 400，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), token) {
		t.Fatalf("错误文本里出现了 config.token 原文：%s", rr.Body.String())
	}
}

// TestSourceResourcePatchKeepsOmittedConfig 验 PATCH 省略 config.token/secret 时凭据保持原值
// （读接口从不回显它们，客户端没有旧值可发），而显式给出的其它键照常生效。
func TestSourceResourcePatchKeepsOmittedConfig(t *testing.T) {
	env := newSourceResEnv(t)
	const token = "src_keep_TOKEN"
	const secret = "keep-secret"
	env.createSource(
		map[string]any{
			"kind": "webhook",
			"config": map[string]any{
				"token":         token,
				"secret":        secret,
				"payloadSchema": map[string]any{"type": "object", "required": []any{"x"}},
			},
			"targets": []any{map[string]any{"capability": "notify"}},
		},
		map[string]any{"name": "hook"},
	)

	// 只改 payloadSchema（不重发 token/secret）：凭据必须原样保留。
	rv := env.resourceVersion("hook")
	rr := env.request(http.MethodPatch, env.sourcePath("hook"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "hook", "resourceVersion": rv},
		"spec": map[string]any{
			"kind":   "webhook",
			"config": map[string]any{"payloadSchema": map[string]any{"type": "object", "required": []any{"y"}}},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	patched := env.doc(rr)
	cfg := sourceResObj(t, sourceResObj(t, patched["spec"])["config"])
	if cfg["hookUrl"] != srcHookPathPrefix+token {
		t.Fatalf("省略 config.token 后 hookUrl 变了（凭据被清掉）：%v", cfg)
	}
	if cfg["secretSet"] != true {
		t.Fatalf("省略 config.secret 后 secretSet 应为 true：%v", cfg)
	}
	schema := sourceResObj(t, cfg["payloadSchema"])
	if req := sourceResArr(t, schema["required"]); len(req) != 1 || req[0] != "y" {
		t.Fatalf("显式给出的 payloadSchema 必须生效：%v", schema)
	}
	sourceResAssertNoCredentialLeak(t, rr.Body.String(), token, secret)

	// 只改 displayName（完全不碰 spec.config）：配置必须完全不动。
	rv = env.resourceVersion("hook")
	rr = env.request(http.MethodPatch, env.sourcePath("hook"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "hook", "resourceVersion": rv, "displayName": "改名"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("只改 displayName 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	after := env.doc(rr)
	if sourceResObj(t, after["metadata"])["displayName"] != "改名" {
		t.Fatalf("displayName 未被写入：%v", after["metadata"])
	}
	cfg = sourceResObj(t, sourceResObj(t, after["spec"])["config"])
	if cfg["hookUrl"] != srcHookPathPrefix+token || cfg["secretSet"] != true {
		t.Fatalf("只改 displayName 不该动配置：%v", cfg)
	}
	if _, ok := cfg["payloadSchema"]; !ok {
		t.Fatalf("只改 displayName 不该把 payloadSchema 抹掉：%v", cfg)
	}

	// 修完的 PATCH 也不能把读投影写回去。
	rv = env.resourceVersion("hook")
	rr = env.request(http.MethodPatch, env.sourcePath("hook"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"resourceVersion": rv},
		"spec":       map[string]any{"config": map[string]any{"hookUrl": "/v1/hooks/x"}},
	})
	if rr.Code != http.StatusBadRequest || sourceResString(t, env.doc(rr)["reason"]) != reasonUnknownField {
		t.Fatalf("PATCH 带 config.hookUrl 期望 400 unknown_field，得到 %d：%s", rr.Code, rr.Body.String())
	}
}

// TestSourceResourcePatchKindImmutable 验 spec.kind 不可变（换语义只能删了重建）。
func TestSourceResourcePatchKindImmutable(t *testing.T) {
	env := newSourceResEnv(t)
	env.createSource(
		map[string]any{"kind": "manual", "config": map[string]any{"payload": map[string]any{"a": 1}}},
		map[string]any{"name": "manual-src"},
	)
	rv := env.resourceVersion("manual-src")
	rr := env.request(http.MethodPatch, env.sourcePath("manual-src"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "manual-src", "resourceVersion": rv},
		"spec":       map[string]any{"kind": "cron"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("换 kind 期望 400，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if reason := sourceResString(t, env.doc(rr)["reason"]); reason != "source_kind_immutable" {
		t.Fatalf("换 kind 的 reason 期望 source_kind_immutable，得到 %q", reason)
	}
	// 原样回传同一个 kind（UI 整份提交）不算改动。
	rv = env.resourceVersion("manual-src")
	rr = env.request(http.MethodPatch, env.sourcePath("manual-src"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "manual-src", "resourceVersion": rv},
		"spec":       map[string]any{"kind": "manual", "enabled": false},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("回传同 kind 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	status := sourceResObj(t, env.doc(rr)["status"])
	if status["enabled"] != false || status["health"] != srcHealthDisabled {
		t.Fatalf("停用后 status 应为 enabled=false / health=disabled：%v", status)
	}
}

// TestSourceResourceDeleteCAS 验删除/更新的 CAS 承载位置与错误码：
// DELETE 令牌只在 query（缺 400 / 旧 409 + details.current）；PATCH 令牌只在 body metadata。
func TestSourceResourceDeleteCAS(t *testing.T) {
	env := newSourceResEnv(t)
	env.createSource(map[string]any{"kind": "manual"}, map[string]any{"name": "doomed"})

	// ① DELETE 缺令牌 → 400 resource_version_required（不静默 last-write-wins）。
	rr := env.request(http.MethodDelete, env.sourcePath("doomed"), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("DELETE 缺 resourceVersion 期望 400，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if reason := sourceResString(t, env.doc(rr)["reason"]); reason != reasonResourceVersionRequired {
		t.Fatalf("DELETE 缺令牌的 reason 期望 %q，得到 %q", reasonResourceVersionRequired, reason)
	}

	// ② 旧令牌 → 409，并回当前令牌（客户端据此重建）。
	current := env.resourceVersion("doomed")
	uid := sourceResObj(t, env.doc(env.request(http.MethodGet, env.sourcePath("doomed"), nil))["metadata"])["uid"]
	stale := fmt.Sprintf("rv1:%s:%d", sourceResString(t, uid), 99)
	rr = env.request(http.MethodDelete, env.sourcePath("doomed")+"?resourceVersion="+url.QueryEscape(stale), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("DELETE 旧令牌期望 409，得到 %d：%s", rr.Code, rr.Body.String())
	}
	conflict := env.doc(rr)
	if reason := sourceResString(t, conflict["reason"]); reason != reasonResourceVersionConflict {
		t.Fatalf("旧令牌的 reason 期望 %q，得到 %q", reasonResourceVersionConflict, reason)
	}
	details := sourceResObj(t, conflict["details"])
	if got := sourceResObj(t, details["current"])["resourceVersion"]; got != current {
		t.Fatalf("details.current.resourceVersion 期望 %q，得到 %v", current, got)
	}

	// ③ PATCH 缺令牌 → 400；旧令牌 → 409。
	rr = env.request(http.MethodPatch, env.sourcePath("doomed"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "doomed"},
		"spec":       map[string]any{"enabled": true},
	})
	if rr.Code != http.StatusBadRequest || sourceResString(t, env.doc(rr)["reason"]) != reasonResourceVersionRequired {
		t.Fatalf("PATCH 缺 metadata.resourceVersion 期望 400 %q，得到 %d：%s",
			reasonResourceVersionRequired, rr.Code, rr.Body.String())
	}
	rr = env.request(http.MethodPatch, env.sourcePath("doomed"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "doomed", "resourceVersion": stale},
		"spec":       map[string]any{"enabled": true},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("PATCH 旧令牌期望 409，得到 %d：%s", rr.Code, rr.Body.String())
	}

	// ④ 正确令牌 → 删掉；之后 GET 404。
	rr = env.request(http.MethodDelete, env.sourcePath("doomed")+"?resourceVersion="+url.QueryEscape(current), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE 期望 204，得到 %d：%s", rr.Code, rr.Body.String())
	}
	rr = env.request(http.MethodGet, env.sourcePath("doomed"), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("删除后 GET 期望 404，得到 %d：%s", rr.Code, rr.Body.String())
	}
	// 空列表必须是 [] 而不是 null（I12 / E7：前端据此区分「没有源」与「字段缺失」）。
	rr = env.request(http.MethodGet, env.sourcesPath(), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("删除后 GET 列表期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if items := sourceResAssertRawArray(t, rr, "items"); len(items) != 0 {
		t.Fatalf("删除后列表应为空，得到 %v", items)
	}
}

// TestSourceResourceFireReceiptShape 验手动触发回执是 W6 冻结形状：键名固定、tasks/pruned 恒数组。
func TestSourceResourceFireReceiptShape(t *testing.T) {
	env := newSourceResEnv(t)
	env.createSource(
		map[string]any{
			"kind":    "manual",
			"config":  map[string]any{"payload": map[string]any{"env": "prod"}},
			"targets": []any{map[string]any{"capability": "notify"}},
		},
		map[string]any{"name": "press-button"},
	)

	tasksBefore := env.count("tasks")
	rr := env.request(http.MethodPost, env.sourcePath("press-button")+"/fire", map[string]any{
		"payload": map[string]any{"env": "staging"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("fire 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	receipt := env.doc(rr)
	for _, key := range []string{"eventId", "traceId", "duplicate", "tasks", "pruned"} {
		if _, ok := receipt[key]; !ok {
			t.Fatalf("回执缺 %q 键：%s", key, rr.Body.String())
		}
	}
	if sourceResString(t, receipt["eventId"]) == "" || sourceResString(t, receipt["traceId"]) == "" {
		t.Fatalf("回执的 eventId/traceId 必须非空：%s", rr.Body.String())
	}
	if receipt["duplicate"] != false {
		t.Fatalf("首次 fire 的 duplicate 应为 false：%s", rr.Body.String())
	}
	if _, ok := receipt["inserted"]; ok {
		t.Fatalf("回执不得携带 v3 字段 inserted：%s", rr.Body.String())
	}
	tasks := sourceResAssertRawArray(t, rr, "tasks")
	sourceResAssertRawArray(t, rr, "pruned")
	if len(tasks) != 1 {
		t.Fatalf("一个 ready target 应建 1 个任务，得到 %d：%s", len(tasks), rr.Body.String())
	}
	task := sourceResObj(t, tasks[0])
	if sourceResString(t, task["taskId"]) == "" || task["capability"] != "notify" {
		t.Fatalf("回执任务的 taskId/capability 不对：%v", task)
	}
	if task["version"] != float64(1) {
		t.Fatalf("回执任务的 version 应为已解析版本 1，得到 %v", task["version"])
	}
	// 真的建了任务（回执不是编出来的）。
	if got := env.count("tasks") - tasksBefore; got != 1 {
		t.Fatalf("fire 应落 1 条任务，实际 %d", got)
	}

	// 无 target 的源：事件照收，tasks 仍是 []，剪枝留痕（no_target）。
	env.createSource(map[string]any{"kind": "manual"}, map[string]any{"name": "no-target"})
	rr = env.request(http.MethodPost, env.sourcePath("no-target")+"/fire", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("无 target 的源 fire 期望 200（入库 + 剪枝留痕），得到 %d：%s", rr.Code, rr.Body.String())
	}
	if tasks := sourceResAssertRawArray(t, rr, "tasks"); len(tasks) != 0 {
		t.Fatalf("无 target 时不建任务，得到 %v", tasks)
	}
	if pruned := sourceResAssertRawArray(t, rr, "pruned"); len(pruned) != 1 {
		t.Fatalf("无 target 应留一条 no_target 剪枝，得到 %v", pruned)
	} else if sourceResObj(t, pruned[0])["reason"] == "" {
		t.Fatalf("剪枝必须带 reason：%v", pruned[0])
	}

	// 停用的源按 §5 的停用规则：同码同文案（不区分「不存在」与「停用」，防探测）。
	env.createSource(map[string]any{"kind": "manual", "enabled": false}, map[string]any{"name": "off"})
	rr = env.request(http.MethodPost, env.sourcePath("off")+"/fire", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("停用源 fire 期望 404，得到 %d：%s", rr.Code, rr.Body.String())
	}
	// 不存在的源同码同文案。
	rr = env.request(http.MethodPost, env.sourcePath("nope")+"/fire", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在的源 fire 期望 404，得到 %d：%s", rr.Code, rr.Body.String())
	}
}

// TestSourceResourceDryRunSideEffectFree 验试运行是**预览文档**：无 eventId/taskId，
// 且真的不写事件 / 不建任务 / 不建源（含未保存草稿）。
func TestSourceResourceDryRunSideEffectFree(t *testing.T) {
	env := newSourceResEnv(t)
	env.createSource(
		map[string]any{
			"kind":    "manual",
			"config":  map[string]any{"payload": map[string]any{"env": "prod"}},
			"targets": []any{map[string]any{"capability": "notify"}},
		},
		map[string]any{"name": "saved"},
	)

	tasksBefore, eventsBefore := env.count("tasks"), env.count("events")

	rr := env.request(http.MethodPost, env.sourcePath("saved")+"/dry-run", map[string]any{
		"payload": map[string]any{"env": "staging"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("dry-run 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, forbidden := range []string{`"eventId"`, `"taskId"`, `"duplicate"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("预览文档不得包含 %s：%s", forbidden, body)
		}
	}
	doc := env.doc(rr)
	targets := sourceResAssertRawArray(t, rr, "targets")
	sourceResAssertRawArray(t, rr, "pruned")
	if len(targets) != 1 {
		t.Fatalf("已保存源的 dry-run 应有 1 条 target，得到 %v", targets)
	}
	target := sourceResObj(t, targets[0])
	if target["capability"] != "notify" || target["state"] != route.StateReady {
		t.Fatalf("ready target 的投影不对：%v", target)
	}
	if _, ok := target["params"]; !ok {
		t.Fatalf("预览 target 必须带 params（可以是 {}）：%v", target)
	}
	if _, ok := doc["eventId"]; ok {
		t.Fatalf("预览文档不得含 eventId：%v", doc)
	}

	// 未保存草稿：inline source。params 带一个解析不到的目标，验证 missing 分支。
	rr = env.request(http.MethodPost, env.sourcesPath()+"/dry-run", map[string]any{
		"source": map[string]any{
			"apiVersion": "anotify/v1",
			"kind":       "Source",
			"metadata":   map[string]any{"name": "draft"},
			"spec": map[string]any{
				"kind": "manual",
				"config": map[string]any{
					// GET 回来的读投影字段：预览容忍并剥离（写端点仍然 400）。
					"hookUrl":   "/v1/hooks/whatever",
					"secretSet": true,
				},
				"targets": []any{
					map[string]any{"capability": "notify"},
					map[string]any{"capability": "ghost"},
				},
			},
		},
		"payload": map[string]any{"env": "prod"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("inline dry-run 期望 200（草稿里的读投影应被剥离），得到 %d：%s", rr.Code, rr.Body.String())
	}
	drafts := sourceResAssertRawArray(t, rr, "targets")
	if len(drafts) != 2 {
		t.Fatalf("草稿的两条 target 都要出现在预览里，得到 %v", drafts)
	}
	missing := sourceResObj(t, drafts[1])
	if missing["state"] != route.StateMissing {
		t.Fatalf("解析不到的目标 state 应为 missing，得到 %v", missing)
	}
	if _, ok := missing["version"]; ok {
		t.Fatalf("解析不到版本时必须省略 version 字段，得到 %v", missing)
	}
	if _, ok := missing["reason"]; !ok {
		t.Fatalf("missing 必须带 reason：%v", missing)
	}
	if strings.Contains(rr.Body.String(), `"eventId"`) || strings.Contains(rr.Body.String(), `"taskId"`) {
		t.Fatalf("预览文档不得包含 eventId/taskId：%s", rr.Body.String())
	}

	// 草稿不落库、不建任务、不写事件。
	if got := env.count("tasks"); got != tasksBefore {
		t.Fatalf("dry-run 不得建任务：before=%d after=%d", tasksBefore, got)
	}
	if got := env.count("events"); got != eventsBefore {
		t.Fatalf("dry-run 不得写事件：before=%d after=%d", eventsBefore, got)
	}
	rr = env.request(http.MethodGet, env.sourcePath("draft"), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("草稿不该被保存成源，GET 期望 404，得到 %d：%s", rr.Code, rr.Body.String())
	}

	// 草稿缺 source → 400。
	rr = env.request(http.MethodPost, env.sourcesPath()+"/dry-run", map[string]any{"payload": map[string]any{"a": 1}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 source 的 inline dry-run 期望 400，得到 %d：%s", rr.Code, rr.Body.String())
	}
}

// TestSourceResourceHookResolvesNamespaceByToken 验外部投递入口：token 定位源 → 源行自带
// namespace，事件落进那个命名空间；未知/停用 token 一律 404（不区分，防探测）。
func TestSourceResourceHookResolvesNamespaceByToken(t *testing.T) {
	env := newSourceResEnv(t)
	const token = "tok-ns-locate"
	env.createSource(
		map[string]any{
			"kind":    "webhook",
			"config":  map[string]any{"token": token, "secret": "hook-secret"},
			"targets": []any{map[string]any{"capability": "notify"}},
		},
		map[string]any{"name": "inbound"},
	)

	rr := env.request(http.MethodPost, srcHookPathPrefix+token, map[string]any{
		"type":    "ping",
		"id":      "delivery-1",
		"payload": map[string]any{"environment": "prod"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("hook 期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	receipt := env.doc(rr)
	traceID := sourceResString(t, receipt["traceId"])
	if traceID == "" {
		t.Fatalf("hook 回执必须带 traceId：%s", rr.Body.String())
	}
	if _, ok := receipt["duplicate"]; !ok {
		t.Fatalf("hook 回执必须是接收回执形状（缺 duplicate）：%s", rr.Body.String())
	}
	sourceResAssertRawArray(t, rr, "tasks")
	sourceResAssertRawArray(t, rr, "pruned")
	sourceResAssertNoCredentialLeak(t, rr.Body.String(), token, "hook-secret")

	// token 定位出的命名空间 = 源所在命名空间（请求里根本没带 namespace）。
	var gotNS, gotSource, gotType string
	if err := env.db.QueryRow(
		`SELECT namespace_id, source, type FROM events WHERE trace_id = ?`, traceID).
		Scan(&gotNS, &gotSource, &gotType); err != nil {
		t.Fatalf("按 traceId 读事件：%v", err)
	}
	if gotNS != env.nsID {
		t.Fatalf("hook 事件应落在 token 定位的命名空间 %q，实际 %q", env.nsID, gotNS)
	}
	if gotSource != "inbound" || gotType != "ping" {
		t.Fatalf("事件 source/type 不对：source=%q type=%q", gotSource, gotType)
	}

	// 同一 id 在去重窗口内重复投递 → duplicate=true 返回原 event/trace，不重复派发。
	rr = env.request(http.MethodPost, srcHookPathPrefix+token, map[string]any{
		"type": "ping", "id": "delivery-1", "payload": map[string]any{"environment": "prod"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("重复投递期望 200，得到 %d：%s", rr.Code, rr.Body.String())
	}
	dup := env.doc(rr)
	if dup["duplicate"] != true {
		t.Fatalf("窗口内同 id 应为 duplicate=true：%s", rr.Body.String())
	}
	if dup["eventId"] != receipt["eventId"] || dup["traceId"] != receipt["traceId"] {
		t.Fatalf("重复投递必须返回原 event/trace：first=%v/%v dup=%v/%v",
			receipt["eventId"], receipt["traceId"], dup["eventId"], dup["traceId"])
	}

	// 未知 token → 404（不泄露「是否存在」的差异）。
	rr = env.request(http.MethodPost, srcHookPathPrefix+"tok-unknown", map[string]any{"type": "ping"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知 token 期望 404，得到 %d：%s", rr.Code, rr.Body.String())
	}
	unknownBody := rr.Body.String()

	// 停用的源：token 还在，但投递一律 404，且与未知 token 同码同文案。
	rv := env.resourceVersion("inbound")
	patch := env.request(http.MethodPatch, env.sourcePath("inbound"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "inbound", "resourceVersion": rv},
		"spec":       map[string]any{"enabled": false},
	})
	if patch.Code != http.StatusOK {
		t.Fatalf("停用源期望 200，得到 %d：%s", patch.Code, patch.Body.String())
	}
	rr = env.request(http.MethodPost, srcHookPathPrefix+token, map[string]any{"type": "ping"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("停用源投递期望 404，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != unknownBody {
		t.Fatalf("「停用」与「token 不存在」必须同码同文案，得到 %s vs %s", rr.Body.String(), unknownBody)
	}
}

// mustJSONString 把文档再编码成字符串（脱敏断言要扫「整份响应体任何位置」）。
func mustJSONString(t *testing.T, doc map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal doc: %v", err)
	}
	return string(raw)
}

// ---------- fresh review 补测：跨命名空间隔离 + 失败触发 0 写 ----------

// sourceResCapability 是跨 ns 用例要用的出口能力（targets 在**源的命名空间**内解析，
// 所以每个参与用例的命名空间都得有一个）。
func sourceResCapability(t *testing.T, db *store.DB, nsID, name string) {
	t.Helper()
	if err := db.CreateCapability(context.Background(), &store.CapabilityVersion{
		NamespaceID: nsID,
		Name:        name,
		Version:     1,
		Spec: artifact.Spec{
			Params: []byte(`{"type":"object"}`),
			Run: artifact.Run{
				Entrypoint: "run.sh",
				Files:      map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"},
			},
		},
	}); err != nil {
		t.Fatalf("create capability %s in %s: %v", name, nsID, err)
	}
}

// TestSourceResourceNamespaceIsolation 钉「源是命名空间内对象」这条边界：
// A 的源在 B 下**完全不存在**——列表看不见、单读 404（不泄露存在性）、
// 拿 A 的 CAS 令牌去 B 下 PATCH/DELETE 也改不动 A 的那一行。
func TestSourceResourceNamespaceIsolation(t *testing.T) {
	env := newSourceResEnv(t)
	ctx := context.Background()

	nsA, err := env.db.CreateNamespace(ctx, "src-a", "源命名空间 A")
	if err != nil {
		t.Fatalf("create namespace src-a: %v", err)
	}
	nsB, err := env.db.CreateNamespace(ctx, "src-b", "源命名空间 B")
	if err != nil {
		t.Fatalf("create namespace src-b: %v", err)
	}
	// 两个 ns 各有一个同名出口能力：源只在**自己那个** ns 里可解析。
	sourceResCapability(t, env.db, nsA.ID, "notify")
	sourceResCapability(t, env.db, nsB.ID, "notify")

	pathA := "/v1/namespaces/" + nsA.Slug + "/sources"
	pathB := "/v1/namespaces/" + nsB.Slug + "/sources"

	created := env.request(http.MethodPost, pathA, map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "deploy-hook"},
		"spec": map[string]any{
			"kind":    "webhook",
			"config":  map[string]any{"token": "src_tok_crossns"},
			"targets": []any{map[string]any{"capability": "notify"}},
		},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("在 ns A 建源期望 201，得到 %d：%s", created.Code, created.Body.String())
	}
	createdDoc := env.doc(created)
	metaA := sourceResObj(t, createdDoc["metadata"])
	if got := sourceResString(t, metaA["namespace"]); got != nsA.Slug {
		t.Fatalf("metadata.namespace 必须是 A 的 slug %q，得到 %q", nsA.Slug, got)
	}
	rvA := sourceResString(t, metaA["resourceVersion"])
	uidA := sourceResString(t, metaA["uid"])
	targetsA := sourceResArr(t, sourceResObj(t, createdDoc["spec"])["targets"])
	if len(targetsA) != 1 || sourceResObj(t, targetsA[0])["capability"] != "notify" {
		t.Fatalf("A 的源的 targets 应钉住 notify，得到 %v", targetsA)
	}

	// ① B 的列表看不到 A 的源（items 必须是 []，不是 null）。
	listB := env.request(http.MethodGet, pathB, nil)
	if listB.Code != http.StatusOK {
		t.Fatalf("ns B 列表期望 200，得到 %d：%s", listB.Code, listB.Body.String())
	}
	if items := sourceResAssertRawArray(t, listB, "items"); len(items) != 0 {
		t.Fatalf("ns B 的 sources 列表不应看见 ns A 的源，实际 %d 条：%s", len(items), listB.Body.String())
	}

	// ② B 的单对象读 → 404，且响应体里不出现 A 的 uid（不泄露存在性）。
	gotB := env.request(http.MethodGet, pathB+"/deploy-hook", nil)
	if gotB.Code != http.StatusNotFound {
		t.Fatalf("ns B 读 A 的源期望 404，得到 %d：%s", gotB.Code, gotB.Body.String())
	}
	if reason := sourceResString(t, env.doc(gotB)["reason"]); reason != reasonNotFound {
		t.Fatalf("跨 ns 单读的 reason 期望 %q，得到 %q", reasonNotFound, reason)
	}
	if body := gotB.Body.String(); strings.Contains(body, uidA) {
		t.Fatalf("404 响应体泄露了 A 的对象 uid（存在性探测）：%s", body)
	}

	// ③ 拿 A 的令牌在 B 下 PATCH → 404，A 的行一个字段都不能变。
	patchB := env.request(http.MethodPatch, pathB+"/deploy-hook", map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "deploy-hook", "resourceVersion": rvA},
		"spec":       map[string]any{"enabled": false},
	})
	if patchB.Code != http.StatusNotFound {
		t.Fatalf("ns B PATCH（带 A 的令牌）期望 404，得到 %d：%s", patchB.Code, patchB.Body.String())
	}

	// ④ 拿 A 的令牌在 B 下 DELETE → 404，A 的行不能被删。
	delB := env.request(http.MethodDelete, pathB+"/deploy-hook?resourceVersion="+url.QueryEscape(rvA), nil)
	if delB.Code != http.StatusNotFound {
		t.Fatalf("ns B DELETE（带 A 的令牌）期望 404，得到 %d：%s", delB.Code, delB.Body.String())
	}

	// ⑤ A 里读回：200、令牌未前进、enabled/targets 原样（上面两次跨 ns 写一次都没落地）。
	after := env.request(http.MethodGet, pathA+"/deploy-hook", nil)
	if after.Code != http.StatusOK {
		t.Fatalf("跨 ns 写之后 A 的源必须还在（期望 200），得到 %d：%s", after.Code, after.Body.String())
	}
	afterDoc := env.doc(after)
	if got := sourceResString(t, sourceResObj(t, afterDoc["metadata"])["resourceVersion"]); got != rvA {
		t.Fatalf("跨 ns 写不该推进 A 的 resourceVersion：%q → %q", rvA, got)
	}
	specAfter := sourceResObj(t, afterDoc["spec"])
	if specAfter["enabled"] != true {
		t.Fatalf("跨 ns PATCH 不该把 A 的源停用，得到 enabled=%v", specAfter["enabled"])
	}
	targetsAfter := sourceResArr(t, specAfter["targets"])
	if len(targetsAfter) != 1 || sourceResObj(t, targetsAfter[0])["capability"] != "notify" {
		t.Fatalf("跨 ns PATCH 不该改 A 的 targets，得到 %v", targetsAfter)
	}
	listA := env.request(http.MethodGet, pathA, nil)
	if items := sourceResAssertRawArray(t, listA, "items"); len(items) != 1 {
		t.Fatalf("A 的列表应仍有 1 条，实际 %d 条：%s", len(items), listA.Body.String())
	}
}

// TestSourceResourceFireFailureWritesNothing 钉「fire 失败是**零副作用**」：
// 停用源与不存在的源各打一次，tasks / events 一行都不许多（不是「回 404 就算了」）。
func TestSourceResourceFireFailureWritesNothing(t *testing.T) {
	env := newSourceResEnv(t)
	ctx := context.Background()

	// 停用源：token 有效、配置完整，只是 enabled=false。
	env.createSource(map[string]any{
		"kind":    "webhook",
		"enabled": false,
		"config":  map[string]any{"token": "src_tok_paused"},
		"targets": []any{map[string]any{"capability": "notify"}},
	}, map[string]any{"name": "paused-hook"})

	tasksBefore := env.count("tasks")
	eventsBefore := env.count("events")

	// ① 停用源 fire → 404（ErrSourceDisabled 的传输面口径）。
	rr := env.request(http.MethodPost, env.sourcePath("paused-hook")+"/fire", map[string]any{
		"payload": map[string]any{"k": "v"},
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("停用源 fire 期望 404，得到 %d：%s", rr.Code, rr.Body.String())
	}
	if reason := sourceResString(t, env.doc(rr)["reason"]); reason != reasonNotFound {
		t.Fatalf("停用源 fire 的 reason 期望 %q，得到 %q", reasonNotFound, reason)
	}

	// ② 不存在的源 fire → 404。
	rr = env.request(http.MethodPost, env.sourcePath("never-existed")+"/fire", map[string]any{
		"payload": map[string]any{"k": "v"},
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在源 fire 期望 404，得到 %d：%s", rr.Code, rr.Body.String())
	}

	// ③ 两次失败请求前后，tasks / events 的行数必须完全不变（直接数表，最硬的判据）。
	if got := env.count("tasks"); got != tasksBefore {
		t.Fatalf("fire 失败写了任务行：%d → %d", tasksBefore, got)
	}
	if got := env.count("events"); got != eventsBefore {
		t.Fatalf("fire 失败写了事件行：%d → %d", eventsBefore, got)
	}

	// ④ 用 store 读模型复核（防「写了又删」这种把表数抹平的实现）。
	tasks, err := env.db.ListTasks(ctx, env.nsID, store.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("fire 失败不该留下任务，实际 %d 条", len(tasks))
	}
	open, err := env.db.CountOpenTasks(ctx, env.nsID)
	if err != nil {
		t.Fatalf("count open tasks: %v", err)
	}
	if open != 0 {
		t.Fatalf("fire 失败不该留下未终结任务，实际 %d 条", open)
	}
	events, err := env.db.ListEvents(ctx, env.nsID, "", "", 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("fire 失败不该留下事件（含剪枝留痕），实际 %d 条", len(events))
	}

	// ⑤ 源本身也没被改写：仍可读、仍停用。
	kept := env.request(http.MethodGet, env.sourcePath("paused-hook"), nil)
	if kept.Code != http.StatusOK {
		t.Fatalf("失败的 fire 不该动到源本身，期望 200，得到 %d：%s", kept.Code, kept.Body.String())
	}
	if spec := sourceResObj(t, env.doc(kept)["spec"]); spec["enabled"] != false {
		t.Fatalf("失败的 fire 不该改动源配置，得到 enabled=%v", spec["enabled"])
	}
}

// TestSourceResourceReservedNameIs400 恢复 v3 的保留源名守卫（review m1）。
//
// `route` 是剪枝/失败观察留痕的保留来源名（store.EventPruneSource）。把它当普通源名
// 建出来，会让自己投递的业务事件在事件流里被**静默过滤**（并且 trace 视图把它当剪枝）
// ——这是会丢数据的调用方输入错误，必须是 400，而不是 500（服务端故障）。
//
// v3 有专门用例 `TestWriteSourceErrReservedNameIs400`（三层：包装错误→400、其它错误→500、
// body 含 `route` 提示）；v4 迁移时丢了，这里恢复同一安全意图。
func TestSourceResourceReservedNameIs400(t *testing.T) {
	env := newSourceResEnv(t)

	rr := env.request(http.MethodPost, env.sourcesPath(), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "route"},
		"spec":       map[string]any{"kind": "manual"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("保留源名期望 400，得到 %d：%s", rr.Code, rr.Body.String())
	}
	doc := env.doc(rr)
	if got := doc["kind"]; got != kindStatus {
		t.Fatalf("错误体 kind = %v, want %s", got, kindStatus)
	}
	if got := doc["reason"]; got != "invalid_source_config" {
		t.Fatalf("reason = %v, want invalid_source_config", got)
	}
	// 提示里必须点出是哪个名字（v3 的 body 含 route 断言；错误信息是给人改的）。
	msg, _ := doc["message"].(string)
	if !strings.Contains(msg, "route") {
		t.Fatalf("错误信息应提到 route，得到 %q", msg)
	}
	// 拒绝是**0 写**：源表里不能出现半条 route 源。
	if got := env.count("sources"); got != 0 {
		t.Fatalf("被拒绝的创建不得落行，sources = %d", got)
	}

}

// TestSourceResourceInvalidTargetIs400 钉「targets 指向不存在的出口能力」是 **400**，不是 500。
//
// targets 是用户写的**引用**：指向一个不存在的名字（或钉死的版本）是调用方输入错误，
// 客户端改一下就好。它落 default 500 会把「参数写错了」报成「服务器坏了」——客户端/
// 控制台据此分支重试策略，报错方向反了就会无限重试一个永远不可能成功的写。
//
// 三条一起钉：
//   - POST 非法 target → 400 + source 域封闭码 + 消息点名那个能力（0 落行）；
//   - 同一路径上合法 target 仍然 201（修复不能顺手把所有 target 都拒掉）；
//   - PATCH 换成非法 target → 同样 400，且**库里的声明不变**（拒绝 = 不改）。
func TestSourceResourceInvalidTargetIs400(t *testing.T) {
	env := newSourceResEnv(t)
	const missing = "does.not.exist"

	// ① POST：非法 target。
	rr := env.request(http.MethodPost, env.sourcesPath(), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "broken.demo"},
		"spec": map[string]any{
			"kind":    "manual",
			"targets": []any{map[string]any{"capability": missing}},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 target 期望 400，得到 %d：%s", rr.Code, rr.Body.String())
	}
	doc := env.doc(rr)
	if got := doc["kind"]; got != kindStatus {
		t.Fatalf("错误体 kind = %v, want %s", got, kindStatus)
	}
	// reason 必须落在 source 域既有封闭集合里（targets 的形状错走同一个码），
	// 不新造词：客户端是按码分支的。
	if got := doc["reason"]; got != "invalid_source_config" {
		t.Fatalf("reason = %v, want invalid_source_config", got)
	}
	// 消息要能直接指出写错的是哪一个引用。
	if msg, _ := doc["message"].(string); !strings.Contains(msg, missing) {
		t.Fatalf("错误信息应点名 %q，得到 %q", missing, msg)
	}
	// 拒绝是**0 写**：不能留下半条 broken.demo 源。
	if got := env.count("sources"); got != 0 {
		t.Fatalf("被拒绝的创建不得落行，sources = %d", got)
	}

	// ② 合法 target 仍然 201（同一 handler 路径，不许被上面那条修复带偏）。
	env.createSource(
		map[string]any{
			"kind":    "manual",
			"targets": []any{map[string]any{"capability": "notify"}},
		},
		map[string]any{"name": "ok.demo"},
	)

	// ③ PATCH：把已有源的 targets 换成不存在的引用 → 400，且库里的声明保持原样。
	rv := env.resourceVersion("ok.demo")
	rr = env.request(http.MethodPatch, env.sourcePath("ok.demo"), map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Source",
		"metadata":   map[string]any{"name": "ok.demo", "resourceVersion": rv},
		"spec": map[string]any{
			"targets": []any{map[string]any{"capability": missing}},
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PATCH 非法 target 期望 400，得到 %d：%s", rr.Code, rr.Body.String())
	}
	patchDoc := env.doc(rr)
	if got := patchDoc["reason"]; got != "invalid_source_config" {
		t.Fatalf("PATCH reason = %v, want invalid_source_config", got)
	}
	if msg, _ := patchDoc["message"].(string); !strings.Contains(msg, missing) {
		t.Fatalf("PATCH 错误信息应点名 %q，得到 %q", missing, msg)
	}
	got := env.request(http.MethodGet, env.sourcePath("ok.demo"), nil)
	if got.Code != http.StatusOK {
		t.Fatalf("读取被拒改的源期望 200，得到 %d：%s", got.Code, got.Body.String())
	}
	targets := sourceResArr(t, sourceResObj(t, env.doc(got)["spec"])["targets"])
	if len(targets) != 1 {
		t.Fatalf("被拒绝的 PATCH 不得改动 targets，得到 %v", targets)
	}
	if cap := sourceResString(t, sourceResObj(t, targets[0])["capability"]); cap != "notify" {
		t.Fatalf("被拒绝的 PATCH 把 targets 改成了 %q（应保持 notify）", cap)
	}
}
