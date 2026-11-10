package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// capTestEnv 是 Capability 资源域的真环境：真 store（临时目录里的真 sqlite 文件）+
// 真 route/source/ws 装配 + 真会话中间件。
//
// 为什么不用 stub：这里要验的就是「信封四键 / CAS 令牌 / status 派生 / 版本族排序」这些
// 与 store 事实强耦合的行为；把 store 换成 stub 等于把被测对象替换掉，测出来的只是
// handler 的分支形状。deploy 端点也走真 ws.Hub（没有节点会话 → sent=false，这正是
// 「离线排队不是失败」的真实语义）。
type capTestEnv struct {
	db     *store.DB
	mux    http.Handler
	cookie *http.Cookie
	// nsA / nsB 是 store 里的真实命名空间 id（事实表引用它）；
	// slugA / slugB 是 URL 与 metadata.namespace 上的对外名字（v4 路径用 slug）。
	nsA   string
	nsB   string
	slugA string
	slugB string
}

func newCapTestEnv(t *testing.T) *capTestEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	user := &store.User{
		ID:          store.NewUserID(),
		Username:    "cap-tester",
		DisplayName: "能力测试用户",
		Role:        store.RoleAdmin,
		CreatedAt:   store.Now(),
	}
	defaultNS, err := db.RegisterUser(ctx, user)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	secondNS, err := db.CreateNamespace(ctx, "second-ns", "第二个命名空间")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	sm := auth.NewSessionManager(db, 0, false)
	sess, err := sm.Create(user.ID, "cap · test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := route.New(db)
	deps := &serverDeps{
		DB:      db,
		Router:  router,
		Sources: source.New(db, router),
		Hub:     ws.NewHub(db, router),
		Log:     slog.Default(),
		Now:     store.Now,
		Session: sm.Middleware,
	}
	mux := http.NewServeMux()
	newCapabilityAPI(deps).register(mux)

	return &capTestEnv{
		db:     db,
		mux:    mux,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
		nsA:    defaultNS.ID,
		nsB:    secondNS.ID,
		slugA:  defaultNS.Slug,
		slugB:  secondNS.Slug,
	}
}

// do 发一个带会话 cookie 的请求（body 非 nil 时按 JSON 编码）。
func (e *capTestEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	switch v := body.(type) {
	case nil:
	case string:
		buf.WriteString(v)
	default:
		if err := json.NewEncoder(&buf).Encode(v); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// capPath 拼能力路径（name / suffix 为空时省略对应段）。
func (e *capTestEnv) capPath(ns, name, suffix string) string {
	path := "/v1/namespaces/" + ns + "/capabilities"
	if name != "" {
		path += "/" + name
	}
	if suffix != "" {
		path += "/" + suffix
	}
	return path
}

// capabilityBody 造一份合法的 Capability 写请求体（脚本正文由 script 指定）。
func capabilityBody(name string, version int, script string) map[string]any {
	meta := map[string]any{"name": name}
	if version > 0 {
		meta["version"] = version
	}
	return map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Capability",
		"metadata":   meta,
		"spec": map[string]any{
			"params": map[string]any{},
			"next":   []string{},
			"run": map[string]any{
				"entrypoint": "run.sh",
				"files":      map[string]string{"run.sh": script},
			},
			"requires":    map[string]any{},
			"lifecycle":   map[string]any{},
			"concurrency": map[string]any{},
		},
	}
}

// ---------- 通用断言工具 ----------

func decodeCapJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

func capObject(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s 不是对象: %#v", what, v)
	}
	return obj
}

func capArray(t *testing.T, v any, what string) []any {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s 不是数组: %#v", what, v)
	}
	return arr
}

func capString(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("%s 不是字符串: %#v", key, m[key])
	}
	return s
}

func capNumber(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	f, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s 不是数字: %#v", key, m[key])
	}
	return int(f)
}

// assertCapabilityEnvelope 校验四键齐全 + status 非 null + 元数据形状。
func assertCapabilityEnvelope(t *testing.T, doc map[string]any, wantName string) map[string]any {
	t.Helper()
	for _, key := range []string{"apiVersion", "kind", "metadata", "spec", "status"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("信封缺键 %q: %#v", key, doc)
		}
	}
	if got := capString(t, doc, "apiVersion"); got != "anotify/v1" {
		t.Fatalf("apiVersion = %q", got)
	}
	if got := capString(t, doc, "kind"); got != "Capability" {
		t.Fatalf("kind = %q", got)
	}
	if doc["status"] == nil {
		t.Fatalf("status 不能是 null（无事实给 {}）")
	}
	status := capObject(t, doc["status"], "status")
	if doc["spec"] == nil {
		t.Fatalf("spec 不能是 null")
	}
	meta := capObject(t, doc["metadata"], "metadata")
	if wantName != "" && capString(t, meta, "name") != wantName {
		t.Fatalf("metadata.name = %q, want %q", meta["name"], wantName)
	}
	if _, ok := meta["resourceVersion"].(string); !ok {
		t.Fatalf("metadata.resourceVersion 缺失或不是字符串: %#v", meta)
	}
	if !regexp.MustCompile(`^rv1:`).MatchString(meta["resourceVersion"].(string)) {
		t.Fatalf("metadata.resourceVersion 不是 rv1: 令牌: %q", meta["resourceVersion"])
	}
	if capString(t, meta, "uid") == "" {
		t.Fatalf("metadata.uid 为空")
	}
	// 四类 condition 恒存在，且 reason 落在封闭集合里（capability.md §2.4）。
	conditions := capArray(t, status["conditions"], "status.conditions")
	if len(conditions) != 4 {
		t.Fatalf("conditions 应有 4 条，实际 %d: %#v", len(conditions), conditions)
	}
	wantTypes := []string{"Implemented", "NodesAvailable", "DownstreamResolved", "Deployed"}
	for i, want := range wantTypes {
		condition := capObject(t, conditions[i], "condition")
		if got := capString(t, condition, "type"); got != want {
			t.Fatalf("conditions[%d].type = %q, want %q", i, got, want)
		}
		if got := capString(t, condition, "status"); got != "True" && got != "False" {
			t.Fatalf("conditions[%d].status = %q（只允许 True/False 字符串）", i, got)
		}
		if got := capString(t, condition, "reason"); got == "" {
			t.Fatalf("conditions[%d].reason 为空", i)
		}
	}
	if _, ok := status["ready"].(bool); !ok {
		t.Fatalf("status.ready 不是 bool: %#v", status["ready"])
	}
	capArray(t, status["nodes"], "status.nodes")
	if _, ok := status["resolved"].(map[string]any); !ok {
		t.Fatalf("status.resolved 不是对象: %#v", status["resolved"])
	}
	if _, ok := status["observed"].(map[string]any); !ok {
		t.Fatalf("status.observed 不是对象: %#v", status["observed"])
	}
	return meta
}

// bumpResourceVersion 把 `rv1:<uid>:<rev>` 的修订号改大（构造「旧令牌」）。
func bumpResourceVersion(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ":")
	if len(parts) != 3 {
		t.Fatalf("resourceVersion 形状不对: %q", token)
	}
	rev, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		t.Fatalf("resourceVersion 修订号不是整数: %q", token)
	}
	return fmt.Sprintf("rv1:%s:%d", parts[1], rev+7)
}

// createCapability 建一个能力并返回它的信封。
func (e *capTestEnv) createCapability(t *testing.T, ns, name, script string) map[string]any {
	t.Helper()
	rec := e.do(t, http.MethodPost, e.capPath(ns, "", ""), capabilityBody(name, 0, script))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: status=%d body=%s", name, rec.Code, rec.Body.String())
	}
	return decodeCapJSON(t, rec)
}

// ---------- 1) 创建 + GET 单对象信封 ----------

func TestCapabilityResourceCreateAndGet(t *testing.T) {
	env := newCapTestEnv(t)

	created := env.createCapability(t, env.nsA, "review", "#!/bin/sh\necho review\n")
	meta := assertCapabilityEnvelope(t, created, "review")
	if got := capNumber(t, meta, "version"); got != 1 {
		t.Fatalf("首个版本应是 1，实际 %d", got)
	}
	if got := capString(t, meta, "namespace"); got != env.slugA {
		t.Fatalf("metadata.namespace = %q, want 路径身份（slug）%q", got, env.slugA)
	}

	// 默认可读面不含脚本正文（spec.run.files 是空 map，不是 null）。
	spec := capObject(t, created["spec"], "spec")
	run := capObject(t, spec["run"], "spec.run")
	files := capObject(t, run["files"], "spec.run.files")
	if len(files) != 0 {
		t.Fatalf("默认响应不应回脚本正文，实际 files=%#v", files)
	}

	rec := env.do(t, http.MethodGet, env.capPath(env.nsA, "review", ""), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 单对象: status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeCapJSON(t, rec)
	gotMeta := assertCapabilityEnvelope(t, got, "review")
	if capString(t, gotMeta, "uid") != capString(t, meta, "uid") {
		t.Fatalf("GET 与 POST 的 uid 不一致")
	}

	// includeBody=true 才回整份正文。
	rec = env.do(t, http.MethodGet, env.capPath(env.nsA, "review", "")+"?includeBody=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET includeBody: status=%d body=%s", rec.Code, rec.Body.String())
	}
	bodySpec := capObject(t, decodeCapJSON(t, rec)["spec"], "spec")
	bodyFiles := capObject(t, capObject(t, bodySpec["run"], "run")["files"], "files")
	if len(bodyFiles) != 1 || !strings.Contains(bodyFiles["run.sh"].(string), "echo review") {
		t.Fatalf("includeBody=true 应回脚本正文，实际 %#v", bodyFiles)
	}

	// includeBody 的取值语义与 Playbook 域共用同一份判据（m9）：
	// false / 0 / 缺省 → 摘要；1（v3 前端写法）与 true 等价 → 全文。
	for _, q := range []string{"?includeBody=false", "?includeBody=0", ""} {
		rec = env.do(t, http.MethodGet, env.capPath(env.nsA, "review", "")+q, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET%s: status=%d body=%s", q, rec.Code, rec.Body.String())
		}
		summarySpec := capObject(t, decodeCapJSON(t, rec)["spec"], "spec")
		summaryFiles := capObject(t, capObject(t, summarySpec["run"], "run")["files"], "files")
		if len(summaryFiles) != 0 {
			t.Fatalf("includeBody%s 必须回摘要（files={}），实际 %#v", q, summaryFiles)
		}
	}
	rec = env.do(t, http.MethodGet, env.capPath(env.nsA, "review", "")+"?includeBody=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET includeBody=1: status=%d body=%s", rec.Code, rec.Body.String())
	}
	aliasSpec := capObject(t, decodeCapJSON(t, rec)["spec"], "spec")
	aliasFiles := capObject(t, capObject(t, aliasSpec["run"], "run")["files"], "files")
	if len(aliasFiles) != 1 {
		t.Fatalf("includeBody=1 应与 true 等价（回正文），实际 %#v", aliasFiles)
	}

	// 不存在的 name → 404。
	rec = env.do(t, http.MethodGet, env.capPath(env.nsA, "missing-one", ""), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET 不存在: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- 2) versions 列表：降序 + 完整信封 + kind ----------

func TestCapabilityResourceVersionsDescending(t *testing.T) {
	env := newCapTestEnv(t)

	// 省略 version = 内容寻址：同 name 不同正文 → 长出新版本（v2）。
	env.createCapability(t, env.nsA, "fixer", "#!/bin/sh\necho v1\n")
	second := env.createCapability(t, env.nsA, "fixer", "#!/bin/sh\necho v2\n")
	if got := capNumber(t, capObject(t, second["metadata"], "metadata"), "version"); got != 2 {
		t.Fatalf("内容寻址应分配 v2，实际 %d", got)
	}

	// 再提交一次 v1 的正文：内容寻址 = 复用已有行（foldKey 命中），不产生 v3。
	reused := env.createCapability(t, env.nsA, "fixer", "#!/bin/sh\necho v1\n")
	if got := capNumber(t, capObject(t, reused["metadata"], "metadata"), "version"); got != 1 {
		t.Fatalf("同内容重复创建应复用 v1（内容寻址），实际 %d", got)
	}

	rec := env.do(t, http.MethodGet, env.capPath(env.nsA, "fixer", "versions"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET versions: status=%d body=%s", rec.Code, rec.Body.String())
	}
	list := decodeCapJSON(t, rec)
	if got := capString(t, list, "kind"); got != "CapabilityList" {
		t.Fatalf("versions 列表 kind = %q, want CapabilityList", got)
	}
	items := capArray(t, list["items"], "items")
	if len(items) != 2 {
		t.Fatalf("versions 应有 2 项，实际 %d", len(items))
	}
	prev := 0
	for i, item := range items {
		doc := capObject(t, item, "item")
		meta := assertCapabilityEnvelope(t, doc, "fixer")
		version := capNumber(t, meta, "version")
		if i == 0 {
			if version != 2 {
				t.Fatalf("versions 应降序（新→旧），首项 version=%d", version)
			}
			prev = version
			continue
		}
		if version >= prev {
			t.Fatalf("versions 未降序: %d 之后是 %d", prev, version)
		}
		prev = version
	}

	// 单版本端点回同一个信封。
	rec = env.do(t, http.MethodGet, env.capPath(env.nsA, "fixer", "versions/1"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET versions/1: status=%d body=%s", rec.Code, rec.Body.String())
	}
	single := decodeCapJSON(t, rec)
	if got := capNumber(t, capObject(t, single["metadata"], "metadata"), "version"); got != 1 {
		t.Fatalf("versions/1 的 metadata.version = %d", got)
	}
	if got := capString(t, single, "kind"); got != "Capability" {
		t.Fatalf("单版本 kind = %q", got)
	}

	// 不存在的版本号 → 404。
	rec = env.do(t, http.MethodGet, env.capPath(env.nsA, "fixer", "versions/9"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET versions/9: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 列表端点：name 升序 + 同名内 version 降序（版本族）。
	rec = env.do(t, http.MethodGet, env.capPath(env.nsA, "", ""), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	list = decodeCapJSON(t, rec)
	if got := capString(t, list, "kind"); got != "CapabilityList" {
		t.Fatalf("列表 kind = %q", got)
	}
	listItems := capArray(t, list["items"], "items")
	if len(listItems) != 2 {
		t.Fatalf("List 应包含两个版本行（版本对象各自独立），实际 %d 项", len(listItems))
	}
	versions := make([]int, 0, len(listItems))
	for _, item := range listItems {
		doc := capObject(t, item, "item")
		meta := assertCapabilityEnvelope(t, doc, "fixer")
		versions = append(versions, capNumber(t, meta, "version"))
	}
	if versions[0] != 2 || versions[1] != 1 {
		t.Fatalf("List 同名内应 version 降序，实际 %v", versions)
	}
}

// ---------- 3) PATCH 的 CAS 与不可变面 ----------

func TestCapabilityResourcePatchCAS(t *testing.T) {
	env := newCapTestEnv(t)
	created := env.createCapability(t, env.nsA, "router", "#!/bin/sh\necho router\n")
	token := capString(t, capObject(t, created["metadata"], "metadata"), "resourceVersion")

	// 缺 metadata.resourceVersion → 400 resource_version_required。
	rec := env.do(t, http.MethodPatch, env.capPath(env.nsA, "router", ""), map[string]any{
		"metadata": map[string]any{"displayName": "路由"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH 缺令牌: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != "resource_version_required" {
		t.Fatalf("reason = %q, want resource_version_required", got)
	}

	// 旧令牌 → 409，且 details.current.resourceVersion 非空。
	rec = env.do(t, http.MethodPatch, env.capPath(env.nsA, "router", ""), map[string]any{
		"metadata": map[string]any{"displayName": "路由", "resourceVersion": bumpResourceVersion(t, token)},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH 旧令牌: status=%d body=%s", rec.Code, rec.Body.String())
	}
	conflict := decodeCapJSON(t, rec)
	if got := capString(t, conflict, "reason"); got != "resource_version_conflict" {
		t.Fatalf("reason = %q, want resource_version_conflict", got)
	}
	current := capObject(t, capObject(t, conflict["details"], "details")["current"], "details.current")
	if capString(t, current, "resourceVersion") == "" {
		t.Fatalf("409 必须带 details.current.resourceVersion")
	}

	// 正确令牌 + spec.next → 200，next 落地且令牌推进。
	rec = env.do(t, http.MethodPatch, env.capPath(env.nsA, "router", ""), map[string]any{
		"metadata": map[string]any{"displayName": "路由", "resourceVersion": token},
		"spec":     map[string]any{"next": []string{"review", "notify@2"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH 正确令牌: status=%d body=%s", rec.Code, rec.Body.String())
	}
	patched := decodeCapJSON(t, rec)
	patchedMeta := assertCapabilityEnvelope(t, patched, "router")
	if got := capString(t, patchedMeta, "displayName"); got != "路由" {
		t.Fatalf("displayName = %q", got)
	}
	next := capArray(t, capObject(t, patched["spec"], "spec")["next"], "spec.next")
	if len(next) != 2 || next[0] != "review" || next[1] != "notify@2" {
		t.Fatalf("spec.next = %#v", next)
	}
	if capString(t, patchedMeta, "resourceVersion") == token {
		t.Fatalf("写成功后 resourceVersion 必须推进")
	}
	// 下游解析不到 → DownstreamResolved 必须是 False + NotFound（派生是真的，不是写死的）。
	for _, item := range capArray(t, capObject(t, patched["status"], "status")["conditions"], "conditions") {
		condition := capObject(t, item, "condition")
		if condition["type"] == "DownstreamResolved" {
			if condition["status"] != "False" || condition["reason"] != "NotFound" {
				t.Fatalf("DownstreamResolved 应为 False/NotFound: %#v", condition)
			}
		}
	}

	// 显式把 displayName 置空（"省略" 与 "置空" 必须能区分）。
	rec = env.do(t, http.MethodPatch, env.capPath(env.nsA, "router", ""), map[string]any{
		"metadata": map[string]any{"displayName": "", "resourceVersion": capString(t, patchedMeta, "resourceVersion")},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH 清空 displayName: status=%d body=%s", rec.Code, rec.Body.String())
	}
	cleared := assertCapabilityEnvelope(t, decodeCapJSON(t, rec), "router")
	if cleared["displayName"] != nil && cleared["displayName"] != "" {
		t.Fatalf("displayName 应被清空，实际 %#v", cleared["displayName"])
	}

	// 改内容（run）不走 PATCH：内容不可变，必须开新版本。
	rec = env.do(t, http.MethodPatch, env.capPath(env.nsA, "router", ""), map[string]any{
		"metadata": map[string]any{"resourceVersion": capString(t, patchedMeta, "resourceVersion")},
		"spec":     map[string]any{"run": map[string]any{"entrypoint": "run.sh", "files": map[string]string{"run.sh": "#!/bin/sh\necho changed\n"}}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH 改内容: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != "immutable_field" {
		t.Fatalf("reason = %q, want immutable_field", got)
	}
}

// ---------- 4) DELETE 的 CAS ----------

func TestCapabilityResourceDeleteCAS(t *testing.T) {
	env := newCapTestEnv(t)
	created := env.createCapability(t, env.nsA, "doomed", "#!/bin/sh\necho doomed\n")
	token := capString(t, capObject(t, created["metadata"], "metadata"), "resourceVersion")

	// 缺 ?resourceVersion= → 400。
	rec := env.do(t, http.MethodDelete, env.capPath(env.nsA, "doomed", ""), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE 缺令牌: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != "resource_version_required" {
		t.Fatalf("reason = %q, want resource_version_required", got)
	}

	// 旧令牌 → 409 + details.current。
	rec = env.do(t, http.MethodDelete,
		env.capPath(env.nsA, "doomed", "")+"?resourceVersion="+bumpResourceVersion(t, token), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE 旧令牌: status=%d body=%s", rec.Code, rec.Body.String())
	}
	conflict := decodeCapJSON(t, rec)
	current := capObject(t, capObject(t, conflict["details"], "details")["current"], "details.current")
	if capString(t, current, "resourceVersion") == "" {
		t.Fatalf("DELETE 409 必须带 details.current.resourceVersion")
	}

	// 正确令牌 → 204，随后 GET 404。
	rec = env.do(t, http.MethodDelete,
		env.capPath(env.nsA, "doomed", "")+"?resourceVersion="+token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE 正确令牌: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, env.capPath(env.nsA, "doomed", ""), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删除后 GET: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 删完之后再删一次 → 404（不存在与 CAS 失败分开）。
	rec = env.do(t, http.MethodDelete,
		env.capPath(env.nsA, "doomed", "")+"?resourceVersion="+token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复 DELETE: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- 5) 跨命名空间隔离 ----------

func TestCapabilityResourceNamespaceIsolation(t *testing.T) {
	env := newCapTestEnv(t)

	env.createCapability(t, env.nsA, "only-in-a", "#!/bin/sh\necho a\n")
	env.createCapability(t, env.nsB, "only-in-b", "#!/bin/sh\necho b\n")

	rec := env.do(t, http.MethodGet, env.capPath(env.nsA, "", ""), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("nsA 列表: status=%d body=%s", rec.Code, rec.Body.String())
	}
	items := capArray(t, decodeCapJSON(t, rec)["items"], "items")
	if len(items) != 1 {
		t.Fatalf("nsA 应只看到自己的 1 个能力，实际 %d", len(items))
	}
	meta := capObject(t, capObject(t, items[0], "item")["metadata"], "metadata")
	if got := capString(t, meta, "name"); got != "only-in-a" {
		t.Fatalf("nsA 列表里出现了别 ns 的能力: %q", got)
	}
	if got := capString(t, meta, "namespace"); got != env.slugA {
		t.Fatalf("metadata.namespace = %q, want 路径身份（slug）%q", got, env.slugA)
	}

	// 从 nsB 看不到 nsA 的能力（404，不泄露存在性）。
	rec = env.do(t, http.MethodGet, env.capPath(env.nsB, "only-in-a", ""), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨 ns GET: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// versions 子资源同样隔离。
	rec = env.do(t, http.MethodGet, env.capPath(env.nsB, "only-in-a", "versions"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨 ns versions: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// body 里显式写别的 namespace → 400 namespace_mismatch。
	body := capabilityBody("cross", 0, "#!/bin/sh\necho cross\n")
	body["metadata"].(map[string]any)["namespace"] = env.nsB
	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("跨 ns 写入: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != "namespace_mismatch" {
		t.Fatalf("reason = %q, want namespace_mismatch", got)
	}
}

// ---------- 6) 写请求的形状校验 ----------

func TestCapabilityResourceWriteValidation(t *testing.T) {
	env := newCapTestEnv(t)

	// 带 status → 400 status_not_writable。
	body := capabilityBody("with-status", 0, "#!/bin/sh\necho s\n")
	body["status"] = map[string]any{"ready": true}
	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("带 status: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != "status_not_writable" {
		t.Fatalf("reason = %q, want status_not_writable", got)
	}

	// 未知顶层字段 → 400 unknown_field。
	body = capabilityBody("with-unknown", 0, "#!/bin/sh\necho u\n")
	body["labels"] = map[string]any{"a": "b"}
	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知顶层字段: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != "unknown_field" {
		t.Fatalf("reason = %q, want unknown_field", got)
	}

	// spec 里未知字段 → 400（spec 是封闭集合）。
	body = capabilityBody("with-unknown-spec", 0, "#!/bin/sh\necho u\n")
	body["spec"].(map[string]any)["nope"] = 1
	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("spec 未知字段: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != "unknown_field" {
		t.Fatalf("reason = %q, want unknown_field", got)
	}

	// 入口没有 shebang → 400 entrypoint_no_shebang（capability.md §6.1 的机器码）。
	body = capabilityBody("no-shebang", 0, "echo hi\n")
	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("无 shebang: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != "entrypoint_no_shebang" {
		t.Fatalf("reason = %q, want entrypoint_no_shebang", got)
	}

	// 缺 metadata.name → 400。
	body = capabilityBody("ignored", 0, "#!/bin/sh\necho n\n")
	delete(body["metadata"].(map[string]any), "name")
	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "", ""), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 name: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- 7) deploy：不存在的目标节点不回 5xx ----------

func TestCapabilityResourceDeployUnknownNode(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "deployable", "#!/bin/sh\necho deploy\n")

	// 不传 nodes：按现有候选策略（这里没有任何节点 → 诚实的空结果）。
	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "deployable", "deploy"), map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy 无候选: status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeCapJSON(t, rec)
	if got := capNumber(t, resp, "version"); got != 1 {
		t.Fatalf("deploy 应回解析到的版本 1，实际 %d", got)
	}
	if deployed := capArray(t, resp["deployed"], "deployed"); len(deployed) != 0 {
		t.Fatalf("没有节点时 deployed 应是 []，实际 %#v", resp["deployed"])
	}

	// 显式给一个不存在的节点：store 拒绝（不存在 ≠ 未授权），但**不能**是 5xx。
	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "deployable", "deploy"),
		map[string]any{"nodes": []string{"nd_does_not_exist"}})
	if rec.Code >= 500 {
		t.Fatalf("deploy 到不存在节点不能是 5xx: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deploy 到不存在节点应 404，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 未知字段 → 400（动作 body 也严格）。
	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "deployable", "deploy"),
		map[string]any{"nodeIds": []string{"nd_x"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("deploy 未知字段: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 不存在的能力 → 404。
	rec = env.do(t, http.MethodPost, env.capPath(env.nsA, "nope", "deploy"), map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deploy 不存在能力: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- 7b) deploy 到真实（离线）节点：写事实、不回推、status 如实投影 ----------

func TestCapabilityResourceDeployToOfflineNode(t *testing.T) {
	env := newCapTestEnv(t)
	ctx := context.Background()
	env.createCapability(t, env.nsA, "deploy-target", "#!/bin/sh\necho deploy\n")

	// 节点离线（零值 = offline），申报 "*" 命名空间 → 授权通过、但不可执行。
	if _, err := env.db.CreateNode(ctx, &store.Node{Name: "anode-1", DeclaredNamespaces: []string{"*"}}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	node, err := env.db.GetNodeByName(ctx, "anode-1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}

	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "deploy-target", "deploy"),
		map[string]any{"nodes": []string{node.ID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy 到离线节点: status=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeCapJSON(t, rec)
	deployed := capArray(t, resp["deployed"], "deployed")
	if len(deployed) != 1 {
		t.Fatalf("deployed 应有 1 项，实际 %d: %s", len(deployed), rec.Body.String())
	}
	outcome := capObject(t, deployed[0], "deployed[0]")
	if got := capString(t, outcome, "node"); got != node.ID {
		t.Fatalf("deployed[0].node = %q, want %q", got, node.ID)
	}
	if got := capString(t, outcome, "state"); got != store.DeploymentPending {
		t.Fatalf("deployed[0].state = %q, want pending", got)
	}
	if sent, _ := outcome["sent"].(bool); sent {
		t.Fatalf("离线节点不应 sent=true")
	}
	if capString(t, outcome, "requestId") == "" {
		t.Fatalf("deployed[0].requestId 为空")
	}

	// status 如实投影：这台节点还没持有该 digest（pending → missing），
	// 且 NodesAvailable 的 reason 是封闭集合里的 NodeOffline。
	GET := env.do(t, http.MethodGet, env.capPath(env.nsA, "deploy-target", ""), nil)
	if GET.Code != http.StatusOK {
		t.Fatalf("GET after deploy: status=%d body=%s", GET.Code, GET.Body.String())
	}
	status := capObject(t, decodeCapJSON(t, GET)["status"], "status")
	nodes := capArray(t, status["nodes"], "status.nodes")
	if len(nodes) != 1 {
		t.Fatalf("status.nodes 应有 1 项，实际 %d: %#v", len(nodes), nodes)
	}
	nodeState := capObject(t, nodes[0], "status.nodes[0]")
	if got := capString(t, nodeState, "name"); got != "anode-1" {
		t.Fatalf("status.nodes[0].name = %q, want anode-1（节点名，不是 id）", got)
	}
	if got := capString(t, nodeState, "state"); got != "missing" {
		t.Fatalf("status.nodes[0].state = %q, want missing", got)
	}
	nodesAvailable := capObject(t, capArray(t, status["conditions"], "conditions")[1], "conditions[1]")
	if nodesAvailable["type"] != "NodesAvailable" || nodesAvailable["status"] != "False" {
		t.Fatalf("NodesAvailable 应为 False: %#v", nodesAvailable)
	}
	if got := capString(t, nodesAvailable, "reason"); got != "NodeOffline" {
		t.Fatalf("NodesAvailable.reason = %q, want NodeOffline", got)
	}
}

// ---------- 8) edges 投影 与 capability-templates ----------

func TestCapabilityResourceEdgesAndTemplates(t *testing.T) {
	env := newCapTestEnv(t)
	env.createCapability(t, env.nsA, "downstream", "#!/bin/sh\necho d\n")
	upstream := env.createCapability(t, env.nsA, "upstream", "#!/bin/sh\necho u\n")

	// next 只能通过 PATCH 写（内容不可变面之外的可变字段）。
	token := capString(t, capObject(t, upstream["metadata"], "metadata"), "resourceVersion")
	rec := env.do(t, http.MethodPatch, env.capPath(env.nsA, "upstream", ""), map[string]any{
		"metadata": map[string]any{"resourceVersion": token},
		"spec":     map[string]any{"next": []string{"downstream"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH next: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = env.do(t, http.MethodGet, "/v1/namespaces/"+env.nsA+"/edges", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET edges: status=%d body=%s", rec.Code, rec.Body.String())
	}
	items := capArray(t, decodeCapJSON(t, rec)["items"], "items")
	found := false
	for _, item := range items {
		edge := capObject(t, item, "edge")
		if capString(t, edge, "from") == "upstream" && capString(t, edge, "to") == "downstream" {
			if got := capString(t, edge, "kind"); got != "capability" {
				t.Fatalf("能力边 kind = %q, want capability", got)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("edges 里没有 upstream→downstream 的能力边: %#v", items)
	}

	rec = env.do(t, http.MethodGet, "/v1/capability-templates", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET capability-templates: status=%d body=%s", rec.Code, rec.Body.String())
	}
	templates := capArray(t, decodeCapJSON(t, rec)["templates"], "templates")
	if len(templates) == 0 {
		t.Fatalf("内置模板不应为空")
	}
}

// ---------- 9) 未登录一律 401 ----------

func TestCapabilityResourceRequiresSession(t *testing.T) {
	env := newCapTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, env.capPath(env.nsA, "", ""), nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req) // 不带 cookie
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		// noStore 在 session 外层：即使 401 也必须声明不可缓存。
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// ---------- 10) 空列表纪律 ----------

func TestCapabilityResourceEmptyLists(t *testing.T) {
	env := newCapTestEnv(t)
	rec := env.do(t, http.MethodGet, env.capPath(env.nsA, "", ""), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("空列表: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("空列表必须是 []，实际 body=%s", rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, "/v1/namespaces/"+env.nsA+"/edges", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("空 edges: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("空 edges 必须是 []，实际 body=%s", rec.Body.String())
	}
}
