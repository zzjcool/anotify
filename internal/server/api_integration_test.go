package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
)

// 跨域装配验收（#331 卡「新增真实 httptest 覆盖」的集成层）。
//
// 为什么还要有这一层：每个 domain 自己的 test 文件验的是「我这个域的 handler 对不对」，
// 但它们都是**只注册自己那几个路由**。真实部署里 6 个 domain 同时挂在一个
// http.ServeMux 上——Go 1.22 的 pattern 冲突会在注册那一刻 panic，而「某个域写了
// 通配符把另一个域遮住」这种事故，单域测试永远发现不了。本文件的 env 把 6 个 register
// 全跑一遍再发请求，正是为了钉住这一层。
//
// 另外三条也只在集成层才成立：
//   - v3 扁平路由**确实没了**（不是「新路由也能用」，而是旧路径必须 404）；
//   - 跨域一致的信封/空列表纪律（每个域的实现是各自写的，这里横向比对）；
//   - cross-namespace 隔离与 cursor 绑定（要两个真命名空间 + 两个域的真实数据）。
const itUserName = "it-331-user"

type itEnv struct {
	db     *store.DB
	mux    *http.ServeMux
	cookie *http.Cookie
	nsA    string // 真实命名空间 id
	nsB    string
	slugA  string // 路径上用的 slug（v4 URL 用 slug；store 事实表引用 ns_ id）
	slugB  string
	hub    *ws.Hub
}

func newITEnv(t *testing.T) *itEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "it.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	user := &store.User{
		ID:          store.NewUserID(),
		Username:    itUserName,
		DisplayName: "集成测试用户",
		Role:        store.RoleAdmin,
		CreatedAt:   store.Now(),
	}
	defaultNS, err := db.RegisterUser(ctx, user)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	second, err := db.CreateNamespace(ctx, "it-second", "集成第二命名空间")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	sm := auth.NewSessionManager(db, 0, false)
	sess, err := sm.Create(user.ID, "it · 331")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	router := route.New(db)
	hub := ws.NewHub(db, router)
	deps := &serverDeps{
		DB:      db,
		Router:  router,
		Sources: source.New(db, router),
		Hub:     hub,
		Log:     slog.Default(),
		Now:     store.Now,
		Session: sm.Middleware,
	}
	mux := http.NewServeMux()
	// 顺序与 mux.go 生产装配一致；任何一个 register 写出冲突 pattern 都会在这里 panic。
	newCapabilityAPI(deps).register(mux)
	newSourceAPI(deps).register(mux)
	newPlaybookAPI(deps).register(mux)
	newNodeAPI(deps).register(mux)
	newNamespaceAPI(deps).register(mux)
	newTaskAPI(deps).register(mux)

	return &itEnv{
		db:     db,
		mux:    mux,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
		nsA:    defaultNS.ID,
		nsB:    second.ID,
		slugA:  defaultNS.Slug,
		slugB:  second.Slug,
		hub:    hub,
	}
}

func (e *itEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
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

// obj 解 JSON 对象（失败即测试失败）。
func (e *itEnv) obj(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return m
}

// capObject 取自 map 的对象字段（nil/缺失时返回空 map，便于统一断言）。
// itRaw 发一次 GET 并返回原始 body（用于「键必须不存在」这类原文字符串断言）。
func itRaw(t *testing.T, e *itEnv, path string) string {
	t.Helper()
	rec := e.do(t, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d; body=%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func itObject(t *testing.T, v any, path string) map[string]any {
	t.Helper()
	if v == nil {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object", path, v)
	}
	return m
}

// capArr 取自 map 的数组字段（null 视为错误：契约要求空集合给 []）。
func itArray(t *testing.T, v any, path string) []any {
	t.Helper()
	if v == nil {
		t.Fatalf("%s is null, want array", path)
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T, want array", path, v)
	}
	return arr
}

// itCapBody 是一个能被 artifact.Normalize/Validate 接受的最小能力信封。
func itCapBody(name string) map[string]any {
	return map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Capability",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"run": map[string]any{
				"entrypoint": "run.sh",
				"files":      map[string]string{"run.sh": "#!/bin/sh\necho hi\n"},
			},
		},
	}
}

// TestAPIIntegrationRouteTableIsV4Only 钉两件事：
//  1. 六个 domain 的 register 能在同一个 ServeMux 上共存（Go 1.22 pattern 冲突会在
//     register 时 panic，构建 env 本身就等于断言）；
//  2. v3 扁平资源路由真的下线了（D0.1：不回兼容旧 flat routes）。
func TestAPIIntegrationRouteTableIsV4Only(t *testing.T) {
	e := newITEnv(t)

	// v3 扁平路径必须是 404：如果哪天有人为了「兼容」把它们加回来，这条会立刻红。
	retired := []struct{ method, path string }{
		{"GET", "/v1/capabilities"},
		{"POST", "/v1/capabilities"},
		{"GET", "/v1/sources"},
		{"POST", "/v1/sources"},
		{"GET", "/v1/playbooks"},
		{"POST", "/v1/playbooks"},
		{"GET", "/v1/tasks"},
		{"POST", "/v1/tasks/x/cancel"},
		{"GET", "/v1/events"},
		{"POST", "/v1/events"},
		{"GET", "/v1/deployments"},
		{"PUT", "/v1/capabilities/whatever/downstreams"},
		{"GET", "/v1/orphans"},
		{"GET", "/v1/stats"},
	}
	for _, r := range retired {
		rec := e.do(t, r.method, r.path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (v3 flat route must be retired); body=%s",
				r.method, r.path, rec.Code, rec.Body.String())
		}
	}

	// v4 namespaced 路由必须存在：用 GET 列表验证（不建数据，只看不是 404/405）。
	for _, path := range []string{
		"/v1/namespaces/" + e.slugA + "/capabilities",
		"/v1/namespaces/" + e.slugA + "/sources",
		"/v1/namespaces/" + e.slugA + "/playbooks",
		"/v1/namespaces/" + e.slugA + "/tasks",
		"/v1/namespaces/" + e.slugA + "/events",
		"/v1/nodes",
		"/v1/namespaces",
		"/v1/system/config",
	} {
		rec := e.do(t, http.MethodGet, path, nil)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want a registered v4 route", path, rec.Code)
		}
	}
}

// TestAPIIntegrationEnvelopeDiscipline 横向比对六域共同的信封纪律：
// 四键恒存在、status 非 null、空列表是 []（不是 null）、metadata.resourceVersion 是 rv1 令牌。
func TestAPIIntegrationEnvelopeDiscipline(t *testing.T) {
	e := newITEnv(t)

	rec := e.do(t, http.MethodPost, "/v1/namespaces/"+e.slugA+"/capabilities", itCapBody("it-alpha"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create capability = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	created := e.obj(t, rec)
	for _, k := range []string{"apiVersion", "kind", "metadata", "spec", "status"} {
		if _, ok := created[k]; !ok {
			t.Errorf("created envelope missing %q: %s", k, rec.Body.String())
		}
	}
	if created["kind"] != "Capability" {
		t.Errorf("kind = %v, want Capability", created["kind"])
	}
	meta := itObject(t, created["metadata"], "metadata")
	if got, _ := meta["namespace"].(string); got != e.slugA {
		t.Errorf("metadata.namespace = %q, want path slug %q（路径即身份）", got, e.slugA)
	}
	rv, _ := meta["resourceVersion"].(string)
	if !strings.HasPrefix(rv, "rv1:") {
		t.Errorf("metadata.resourceVersion = %q, want rv1: token", rv)
	}
	if _, ok := created["status"].(map[string]any); !ok {
		t.Errorf("status must be an object (never null): %v", created["status"])
	}

	// 列表：kind 是 <Kind>List、items 是数组。
	listRec := e.do(t, http.MethodGet, "/v1/namespaces/"+e.slugA+"/capabilities", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", listRec.Code)
	}
	list := e.obj(t, listRec)
	if list["kind"] != "CapabilityList" {
		t.Errorf("list kind = %v, want CapabilityList", list["kind"])
	}
	if items := itArray(t, list["items"], "items"); len(items) != 1 {
		t.Errorf("items len = %d, want 1", len(items))
	}

	// 空列表是 []：第二命名空间没有任何能力，原始 body 必须含 "items":[]。
	emptyRec := e.do(t, http.MethodGet, "/v1/namespaces/"+e.slugB+"/capabilities", nil)
	if emptyRec.Code != http.StatusOK {
		t.Fatalf("empty list = %d, want 200", emptyRec.Code)
	}
	if !strings.Contains(emptyRec.Body.String(), `"items":[]`) {
		t.Errorf("empty list must marshal items as [], got %s", emptyRec.Body.String())
	}

	// 每个资源域的空列表都必须给 []（不是 null）。
	emptyPaths := []string{
		"/v1/namespaces/" + e.slugB + "/sources",
		"/v1/namespaces/" + e.slugB + "/playbooks",
		"/v1/namespaces/" + e.slugB + "/tasks",
		"/v1/namespaces/" + e.slugB + "/events",
		"/v1/nodes",
	}
	for _, path := range emptyPaths {
		r := e.do(t, http.MethodGet, path, nil)
		if r.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200; body=%s", path, r.Code, r.Body.String())
			continue
		}
		if !strings.Contains(r.Body.String(), `"items":[]`) {
			t.Errorf("GET %s must return items:[], got %s", path, r.Body.String())
		}
	}

	// status 只读：写请求带 status → 400 status_not_writable（S1）。
	body := itCapBody("it-beta")
	body["status"] = map[string]any{"ready": true}
	badRec := e.do(t, http.MethodPost, "/v1/namespaces/"+e.slugA+"/capabilities", body)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("write with status = %d, want 400; body=%s", badRec.Code, badRec.Body.String())
	}
	if reason := e.obj(t, badRec)["reason"]; reason != reasonStatusNotWritable {
		t.Errorf("reason = %v, want %s", reason, reasonStatusNotWritable)
	}
}

// TestAPIIntegrationCASRejectsABA 验 W1 的 CAS 语义（绑 UID/revision，拒 ABA）：
// 缺失 400、旧令牌 409 + details.current.resourceVersion、正确令牌 204 后资源真的没了。
func TestAPIIntegrationCASRejectsABA(t *testing.T) {
	e := newITEnv(t)
	base := "/v1/namespaces/" + e.slugA + "/capabilities"

	created := e.obj(t, e.do(t, http.MethodPost, base, itCapBody("it-cas")))
	meta := itObject(t, created["metadata"], "metadata")
	oldRV, _ := meta["resourceVersion"].(string)
	if oldRV == "" {
		t.Fatalf("created without resourceVersion: %v", created)
	}

	// 缺失令牌 → 400 resource_version_required（不能静默 last-write-wins）。
	noTok := e.do(t, http.MethodPatch, base+"/it-cas", map[string]any{
		"metadata": map[string]any{"displayName": "新名字"},
		"spec":     map[string]any{},
	})
	if noTok.Code != http.StatusBadRequest {
		t.Fatalf("patch without resourceVersion = %d, want 400; body=%s", noTok.Code, noTok.Body.String())
	}
	if reason := e.obj(t, noTok)["reason"]; reason != reasonResourceVersionRequired {
		t.Errorf("reason = %v, want %s", reason, reasonResourceVersionRequired)
	}

	// 先用正确令牌改一次，让 oldRV 变旧（这就是 ABA 的 A→B 步）。
	ok := e.do(t, http.MethodPatch, base+"/it-cas", map[string]any{
		"metadata": map[string]any{"resourceVersion": oldRV, "displayName": "改过的"},
		"spec":     map[string]any{},
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("patch with current resourceVersion = %d, want 200; body=%s", ok.Code, ok.Body.String())
	}

	// 拿旧令牌再写（ABA 的回到 A 步）→ 409 且回当前令牌。
	stale := e.do(t, http.MethodPatch, base+"/it-cas", map[string]any{
		"metadata": map[string]any{"resourceVersion": oldRV, "displayName": "又改回去"},
		"spec":     map[string]any{},
	})
	if stale.Code != http.StatusConflict {
		t.Fatalf("patch with stale resourceVersion = %d, want 409; body=%s", stale.Code, stale.Body.String())
	}
	staleBody := e.obj(t, stale)
	if reason := staleBody["reason"]; reason != reasonResourceVersionConflict {
		t.Errorf("reason = %v, want %s", reason, reasonResourceVersionConflict)
	}
	details := itObject(t, staleBody["details"], "details")
	current := itObject(t, details["current"], "details.current")
	if cur, _ := current["resourceVersion"].(string); cur == "" || cur == oldRV {
		t.Errorf("details.current.resourceVersion = %q, want current (non-stale) token", cur)
	}

	// DELETE 的令牌只认 query：body 里的令牌无效。
	delNoTok := e.do(t, http.MethodDelete, base+"/it-cas", nil)
	if delNoTok.Code != http.StatusBadRequest {
		t.Fatalf("delete without ?resourceVersion = %d, want 400; body=%s", delNoTok.Code, delNoTok.Body.String())
	}
	// 旧令牌 → 409。
	delStale := e.do(t, http.MethodDelete, base+"/it-cas?resourceVersion="+oldRV, nil)
	if delStale.Code != http.StatusConflict {
		t.Fatalf("delete with stale token = %d, want 409; body=%s", delStale.Code, delStale.Body.String())
	}
	// 当前令牌 → 204，然后 GET 404。
	curMeta := itObject(t, e.obj(t, e.do(t, http.MethodGet, base+"/it-cas", nil))["metadata"], "metadata")
	curRV, _ := curMeta["resourceVersion"].(string)
	delOK := e.do(t, http.MethodDelete, base+"/it-cas?resourceVersion="+curRV, nil)
	if delOK.Code != http.StatusNoContent && delOK.Code != http.StatusOK {
		t.Fatalf("delete with current token = %d, want 204; body=%s", delOK.Code, delOK.Body.String())
	}
	if got := e.do(t, http.MethodGet, base+"/it-cas", nil).Code; got != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", got)
	}
}

// TestAPIIntegrationNamespaceIsolation 验跨命名空间隔离与 cursor 绑定（W6）：
// 路径里的 slug 必须解析到 store 的 ns_ id，否则「A 建的东西在 B 也看得到」。
func TestAPIIntegrationNamespaceIsolation(t *testing.T) {
	e := newITEnv(t)

	if rec := e.do(t, http.MethodPost, "/v1/namespaces/"+e.slugA+"/capabilities", itCapBody("it-only-a")); rec.Code != http.StatusCreated {
		t.Fatalf("create in A = %d; body=%s", rec.Code, rec.Body.String())
	}

	// B 的列表为空（slug→id 解析正确时成立；把 slug 直接当 namespace_id 会串库）。
	recB := e.do(t, http.MethodGet, "/v1/namespaces/"+e.slugB+"/capabilities", nil)
	if recB.Code != http.StatusOK {
		t.Fatalf("list B = %d", recB.Code)
	}
	if items := itArray(t, e.obj(t, recB)["items"], "items"); len(items) != 0 {
		t.Errorf("B sees %d capabilities, want 0 (cross-namespace leak)", len(items))
	}
	// B 里按名字取 A 的资源 → 404。
	if got := e.do(t, http.MethodGet, "/v1/namespaces/"+e.slugB+"/capabilities/it-only-a", nil).Code; got != http.StatusNotFound {
		t.Errorf("GET A's capability from B = %d, want 404", got)
	}

	// cursor 绑定 ns + 筛选条件：A 的 cursor 拿到 B 用必须 400。
	//
	// 必须**真的播种**事件：早先这里用 t.Skip 兜「没有事件可翻页」，那会让这条最关键的
	// 越权断言永久静默（skip 永远是绿的）。现在先写 3 条真实事件，cursor 一定存在。
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		ev := &store.Event{
			ID:          store.NewEventID(),
			NamespaceID: e.nsA,
			TraceID:     "trace-cursor-" + strconv.Itoa(i),
			Source:      "it-source",
			Type:        "it.cursor",
			Payload:     json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`),
			CreatedAt:   store.Now() + int64(i),
		}
		if _, err := e.db.InsertEvent(ctx, ev, store.Now(), store.DefaultRetentionEventTTLSec); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}
	page := e.obj(t, e.do(t, http.MethodGet, "/v1/console/events?namespace="+e.slugA+"&limit=1", nil))
	cur, _ := itObject(t, page["metadata"], "metadata")["continue"].(string)
	if cur == "" {
		t.Fatalf("A 有 3 条事件但首页 limit=1 没有 continue 令牌: %v", page)
	}
	bad := e.do(t, http.MethodGet, "/v1/console/events?namespace="+e.slugB+"&limit=1&continue="+cur, nil)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("reuse A's continue on B = %d, want 400; body=%s", bad.Code, bad.Body.String())
	}
	// 同一 ns 用同一令牌必须能正常翻页（否则「绑定」退化成「一律拒绝」）。
	okPage := e.do(t, http.MethodGet, "/v1/console/events?namespace="+e.slugA+"&limit=1&continue="+cur, nil)
	if okPage.Code != http.StatusOK {
		t.Errorf("A's own continue on A = %d, want 200; body=%s", okPage.Code, okPage.Body.String())
	}
}

// TestAPIIntegrationFailedAttemptZeroTask 验 S1/E1 在 HTTP 面的投影：
// downstream_missing 是一条**真实**的 failed/attempt0 Task（有 taskId、没有伪造
// version/digest/Node 过程事实），并且它在 tasks 列表里可见。
func TestAPIIntegrationFailedAttemptZeroTask(t *testing.T) {
	e := newITEnv(t)
	ctx := context.Background()

	// 造 downstream_missing 的真实路径：源创建时目标能力必须存在（v4 配置期就校验
	// 「出口能力存在」），所以先建能力、建源，再把能力删掉——派发期解析不到目标，
	// 才会走 S1/E1 的「已创建失败 Task」而不是剪枝。
	if rec := e.do(t, http.MethodPost, "/v1/namespaces/"+e.slugA+"/capabilities", itCapBody("it-absent-cap")); rec.Code != http.StatusCreated {
		t.Fatalf("seed capability = %d; body=%s", rec.Code, rec.Body.String())
	}
	caps, err := e.db.ListCapabilities(ctx, e.nsA)
	if err != nil {
		t.Fatalf("list seeded capabilities: %v", err)
	}
	var seed *store.CapabilityVersion
	for _, c := range caps {
		if c.Name == "it-absent-cap" {
			seed = c
		}
	}
	if seed == nil {
		t.Fatalf("seeded capability row not found")
	}
	src := &store.Source{
		ID:          store.NewSourceID(),
		NamespaceID: e.nsA,
		Name:        "it-missing",
		Kind:        store.SourceKindWebhook,
		Config:      json.RawMessage(`{"payload":{}}`),
		Targets:     []sourcespec.Target{{Capability: "it-absent-cap"}},
		Enabled:     true,
	}
	if err := e.db.CreateSource(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := e.db.DeleteCapability(ctx, e.nsA, "it-absent-cap", store.ResourceVersion(seed.ID, seed.ResourceVersion)); err != nil {
		t.Fatalf("delete seeded capability: %v", err)
	}

	fire := e.do(t, http.MethodPost, "/v1/namespaces/"+e.slugA+"/sources/it-missing/fire", map[string]any{})
	if fire.Code != http.StatusOK {
		t.Fatalf("fire = %d, want 200; body=%s", fire.Code, fire.Body.String())
	}
	receipt := e.obj(t, fire)
	tasks := itArray(t, receipt["tasks"], "tasks")
	if len(tasks) != 1 {
		t.Fatalf("fire receipt tasks = %v, want exactly 1 failed task (E1: taskId must be real)", receipt["tasks"])
	}
	ref := itObject(t, tasks[0], "tasks[0]")
	taskID, _ := ref["taskId"].(string)
	if taskID == "" {
		t.Fatalf("taskId is empty: failed task must carry a real id (E1)")
	}
	if _, ok := ref["version"]; ok {
		t.Errorf("undeclared version must be omitted, got %v", ref["version"])
	}

	// 列表里必须能看见这条失败任务（NULL capability_id 不能被扫描隐藏）。
	listRec := e.do(t, http.MethodGet, "/v1/namespaces/"+e.slugA+"/tasks", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("task list = %d; body=%s", listRec.Code, listRec.Body.String())
	}
	var found map[string]any
	for _, item := range itArray(t, e.obj(t, listRec)["items"], "items") {
		m := itObject(t, item, "items[]")
		if itObject(t, m["metadata"], "metadata")["name"] == taskID {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("failed task %s not visible in list: %s", taskID, listRec.Body.String())
	}
	status := itObject(t, found["status"], "status")
	if status["phase"] != "failed" {
		t.Errorf("phase = %v, want failed", status["phase"])
	}
	if attempt, ok := status["attempt"].(float64); !ok || attempt != 0 {
		t.Errorf("attempt = %v, want 0 (attempt0 failure)", status["attempt"])
	}
	if node, ok := status["nodeName"]; ok && node != nil {
		t.Errorf("attempt0 failure must not claim a node: %v", node)
	}
	spec := itObject(t, found["spec"], "spec")
	capi := itObject(t, spec["capability"], "spec.capability")
	if capi["name"] != "it-absent-cap" {
		t.Errorf("spec.capability.name = %v, want request basename it-absent-cap", capi["name"])
	}
	// S1/E1 要的是**省略**，不是 `"version": null`：后者会让客户端以为「字段存在但无值」，
	// 而事实是「这次请求没有声明版本」。所以直接断键不存在。
	if _, ok := capi["digest"]; ok {
		t.Errorf("unresolved capability must omit digest key entirely, got %v", capi["digest"])
	}
	if _, ok := capi["version"]; ok {
		t.Errorf("undeclared version must omit version key entirely, got %v", capi["version"])
	}
	if raw := itRaw(t, e, "/v1/namespaces/"+e.slugA+"/tasks/"+taskID); strings.Contains(raw, `"digest":null`) || strings.Contains(raw, `"version":null`) {
		t.Errorf("wire must not carry version/digest null for unresolved capability: %s", raw)
	}
}

// TestAPIIntegrationNodeTokenOneShot 验 W4：登记与令牌是两个动作，
// 明文只在 POST tokens 的响应里出现一次，之后任何读接口都不回显。
func TestAPIIntegrationNodeTokenOneShot(t *testing.T) {
	e := newITEnv(t)

	create := e.do(t, http.MethodPost, "/v1/nodes", map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": "it-node-a"},
	})
	if create.Code != http.StatusCreated && create.Code != http.StatusOK {
		t.Fatalf("create node = %d; body=%s", create.Code, create.Body.String())
	}
	created := e.obj(t, create)
	meta := itObject(t, created["metadata"], "metadata")
	if _, ok := meta["token"]; ok {
		t.Errorf("POST /v1/nodes must not return a token (W4: token is a separate action)")
	}
	if strings.Contains(create.Body.String(), "ant_node_") {
		t.Errorf("POST /v1/nodes leaked a node token: %s", create.Body.String())
	}

	tok := e.do(t, http.MethodPost, "/v1/nodes/it-node-a/tokens", map[string]any{})
	if tok.Code != http.StatusOK && tok.Code != http.StatusCreated {
		t.Fatalf("issue token = %d; body=%s", tok.Code, tok.Body.String())
	}
	tokBody := e.obj(t, tok)
	if tokBody["kind"] != "NodeToken" {
		t.Errorf("kind = %v, want NodeToken", tokBody["kind"])
	}
	plaintext, _ := tokBody["token"].(string)
	if plaintext == "" {
		t.Fatalf("token action must return plaintext once: %s", tok.Body.String())
	}

	// 读接口永远不回显：单对象、列表、config。
	for _, path := range []string{"/v1/nodes/it-node-a", "/v1/nodes", "/v1/nodes/it-node-a/config"} {
		rec := e.do(t, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if strings.Contains(rec.Body.String(), plaintext) {
			t.Errorf("GET %s leaked the node token", path)
		}
	}

	// 再次 POST = 轮换：新明文，旧 token 失效（hash 查不到 node）。
	rot := e.obj(t, e.do(t, http.MethodPost, "/v1/nodes/it-node-a/tokens", map[string]any{}))
	next, _ := rot["token"].(string)
	if next == "" || next == plaintext {
		t.Fatalf("rotation must mint a new plaintext (old=%q new=%q)", plaintext, next)
	}
	if _, err := e.db.GetNodeByTokenHash(context.Background(), store.HashNodeToken(plaintext)); err == nil {
		t.Errorf("old node token still resolves after rotation")
	}
}

// TestAPIIntegrationNodeConfigRevision 验 W5：配置子资源是 wrapper（不是 kind），
// PUT 后 configRevision 前进，而 resourceVersion 是 CAS 令牌（两者不能混用）。
func TestAPIIntegrationNodeConfigRevision(t *testing.T) {
	e := newITEnv(t)
	if rec := e.do(t, http.MethodPost, "/v1/nodes", map[string]any{
		"apiVersion": "anotify/v1", "kind": "Node", "metadata": map[string]any{"name": "it-node-cfg"},
	}); rec.Code >= 300 {
		t.Fatalf("create node = %d; body=%s", rec.Code, rec.Body.String())
	}

	get := e.do(t, http.MethodGet, "/v1/nodes/it-node-cfg/config", nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET config = %d; body=%s", get.Code, get.Body.String())
	}
	cfg := e.obj(t, get)
	if cfg["kind"] != nil {
		t.Errorf("config subresource must not pretend to be a resource kind: %v", cfg["kind"])
	}
	meta := itObject(t, cfg["metadata"], "metadata")
	rv, _ := meta["resourceVersion"].(string)
	if rv == "" {
		t.Fatalf("config wrapper must carry metadata.resourceVersion: %s", get.Body.String())
	}
	revBefore, _ := cfg["configRevision"].(string)

	put := e.do(t, http.MethodPut, "/v1/nodes/it-node-cfg/config", map[string]any{
		"metadata":   map[string]any{"resourceVersion": rv},
		"namespaces": []string{"default"},
		"vars":       map[string]string{"IT_VAR": "1"},
		"limits":     map[string]any{"total": 2},
	})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT config = %d; body=%s", put.Code, put.Body.String())
	}
	after := e.obj(t, put)
	revAfter, _ := after["configRevision"].(string)
	if revAfter == "" || revAfter == revBefore {
		t.Errorf("configRevision did not advance: before=%q after=%q", revBefore, revAfter)
	}
	// 同一份旧令牌再写 → 409（配置写必须有 CAS）。
	stale := e.do(t, http.MethodPut, "/v1/nodes/it-node-cfg/config", map[string]any{
		"metadata": map[string]any{"resourceVersion": rv},
		"vars":     map[string]string{"IT_VAR": "2"},
	})
	if stale.Code != http.StatusConflict {
		t.Errorf("PUT config with stale token = %d, want 409; body=%s", stale.Code, stale.Body.String())
	}
}
