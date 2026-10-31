package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件覆盖 /v1/namespaces（契约 §6.1）与 /v1/stats 的命名空间解析（§6.2）。
//
// 测试环境与 console_test.go 的 consoleEnv **故意分开**：
// 那边挂的是控制台读端点（任务/事件/能力），这边挂的是命名空间自身 + 源 + 统计，
// 两套路由互不依赖。共用一个 env 会让任一侧加一条路由就把另一侧的测试改红。

// nsTestDispatcher 是记录入参的假派发器（不发帧、不落库）。
//
// 只用 source.Trigger 的真实逻辑（构造事件 / 拼 DispatchInput），
// 但把"派给谁"这部分换成可断言的记录器。
type nsTestDispatcher struct {
	got []route.DispatchInput
	err error
}

func (d *nsTestDispatcher) Dispatch(_ context.Context, in route.DispatchInput) (*route.DispatchOutput, error) {
	d.got = append(d.got, in)
	if d.err != nil {
		return nil, d.err
	}
	return &route.DispatchOutput{}, nil
}

// nsTestEnv 是命名空间 / 源 / 统计相关端点的测试环境（真 SQLite + 真会话中间件）。
//
// 组织层已随迁移 14 删除（#204）：这里的夹具也就没有“组织”这一层，
// 只有「用户 + 它注册时自带的 default 命名空间」，隔离边界就是命名空间本身。
type nsTestEnv struct {
	mux        http.Handler
	db         *store.DB
	userID     string
	nsID       string // 用户注册时自带的 default 命名空间
	otherNSID  string // 第二个命名空间（需要时由 t.Helper 建）
	cookie     *http.Cookie
	dispatcher *nsTestDispatcher
}

// newNSTestEnv 建库 + 用户（含注册自带的 default 命名空间）+ 会话，并挂上与生产一致的路由。
func newNSTestEnv(t *testing.T) *nsTestEnv {
	t.Helper()
	ctx := context.Background()

	// 收尾走 closeDBForTempDir（#259）：关库钉在 t.TempDir 的 RemoveAll 之前，
	// 并断言没有 goroutine 在关库窗口里用库（失败即报「收尾竞态」）。
	dir := t.TempDir()
	db, err := store.Open(dir + "/ns.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { closeDBForTempDir(t, db, dir) })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify NS Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	// RegisterUser 在一个事务里建好「用户 + default 命名空间 + 内置能力」：
	// 契约 §2.1 要求不存在没有命名空间的用户，所以 register 返回的就是它的命名空间。
	uid := store.NewUserID()
	defaultNS, err := db.RegisterUser(ctx, &store.User{
		ID:          uid,
		Username:    "u-" + uid,
		DisplayName: "NS User",
		CreatedAt:   store.Now(),
	})
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	nsID := defaultNS.ID
	sess, err := svc.Sessions().Create(uid, "Test · NS")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	consoleH := &consoleHandler{db: db, auth: svc}
	disp := &nsTestDispatcher{}
	srcH := &sourceHandler{
		db:               db,
		commander:        &source.Trigger{Store: db, Dispatcher: disp},
		resolveNamespace: consoleH.resolveNamespace,
	}
	nsH := &namespaceHandler{db: db}
	statsH := &statsHandler{
		db:               db,
		resolveNamespace: consoleH.resolveNamespace,
	}

	sessMW := svc.Sessions().Middleware
	mux := http.NewServeMux()
	mux.Handle("GET /v1/namespaces", noStore(sessMW(http.HandlerFunc(nsH.list))))
	mux.Handle("POST /v1/namespaces", noStore(sessMW(http.HandlerFunc(nsH.create))))
	mux.Handle("PATCH /v1/namespaces/{id}", noStore(sessMW(http.HandlerFunc(nsH.byID))))
	mux.Handle("DELETE /v1/namespaces/{id}", noStore(sessMW(http.HandlerFunc(nsH.byID))))
	mux.Handle("GET /v1/stats", noStore(sessMW(statsH)))
	mux.Handle("GET /v1/sources", noStore(sessMW(http.HandlerFunc(srcH.sources))))
	mux.Handle("POST /v1/sources", noStore(sessMW(http.HandlerFunc(srcH.sources))))
	mux.Handle("GET /v1/sources/{id}", noStore(sessMW(http.HandlerFunc(srcH.sourceByID))))
	mux.Handle("PATCH /v1/sources/{id}", noStore(sessMW(http.HandlerFunc(srcH.sourceByID))))
	mux.Handle("DELETE /v1/sources/{id}", noStore(sessMW(http.HandlerFunc(srcH.sourceByID))))
	mux.Handle("POST /v1/sources/{id}/fire", noStore(sessMW(http.HandlerFunc(srcH.fire))))
	mux.Handle("POST /v1/sources/{id}/dry-run", noStore(sessMW(http.HandlerFunc(srcH.dryRun))))
	mux.Handle("POST /v1/sources/dry-run", noStore(sessMW(http.HandlerFunc(srcH.dryRun))))
	mux.Handle("POST /v1/hooks/{token}", noStore(http.HandlerFunc(srcH.hook)))

	return &nsTestEnv{
		mux: mux, db: db, userID: uid, nsID: nsID,
		cookie:     &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
		dispatcher: disp,
	}
}

// do 发一个带会话 Cookie 的请求。
func (e *nsTestEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.AddCookie(e.cookie)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// doAnon 发一个不带会话的请求（验证 401）。
func (e *nsTestEnv) doAnon(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// newNamespace 直接走 store 建一个命名空间（测试前置）。
func (e *nsTestEnv) newNamespace(t *testing.T, slug, name string) *store.Namespace {
	t.Helper()
	ns, err := e.db.CreateNamespace(context.Background(), slug, name)
	if err != nil {
		t.Fatalf("create namespace %q: %v", slug, err)
	}
	return ns
}

// builtinCapID 取某个命名空间里指定内置能力的 id（建源时需要一个本命名空间的出口）。
func (e *nsTestEnv) builtinCapID(t *testing.T, namespaceID, name string) string {
	t.Helper()
	caps, err := e.db.ListCapabilities(context.Background(), namespaceID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	for _, c := range caps {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("builtin capability %q not found in namespace %s", name, namespaceID)
	return ""
}

// createSource 直接走 store 建一个源（测试前置，绕开 handler 的参数校验）。
func (e *nsTestEnv) createSource(t *testing.T, namespaceID, name string) *store.Source {
	t.Helper()
	src := &store.Source{
		NamespaceID: namespaceID,
		Name:        name,
		Kind:        store.SourceKindManual,
		Config:      json.RawMessage(`{}`),
		Enabled:     true,
		Targets:     []store.SourceTarget{{CapabilityID: e.builtinCapID(t, namespaceID, "notify.phone")}},
	}
	if err := e.db.CreateSource(context.Background(), src); err != nil {
		t.Fatalf("create source %q: %v", name, err)
	}
	return src
}

// nsDecode 把响应体解成 map。
func nsDecode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return m
}

// ---------- /v1/namespaces ----------

// TestNamespaceListIncludesResourceCounts 列表必须带四类资源计数（契约 §6.1：UI 直接用）。
//
// 计数不是"可有可无的装饰"：删命名空间前的确认框要写「N 台机器将失去连接」，
// 数据就来自这里。列表里不带计数的话，前端只能为每个命名空间再发 4 个请求去数。
func TestNamespaceListIncludesResourceCounts(t *testing.T) {
	env := newNSTestEnv(t)
	env.createSource(t, env.nsID, "src-a")

	rr := env.do(t, http.MethodGet, "/v1/namespaces", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	body := nsDecode(t, rr)
	list, _ := body["namespaces"].([]any)
	if len(list) != 1 {
		t.Fatalf("namespaces = %v, want 1 个（注册时自带的 default）", body["namespaces"])
	}
	ns, _ := list[0].(map[string]any)

	// 计数键必须存在且是 camelCase（前端按 camelCase 读，缺键会读成 undefined）
	for _, k := range []string{"capabilityCount", "nodeCount", "sourceCount", "edgeCount"} {
		if _, ok := ns[k]; !ok {
			t.Fatalf("namespace 缺 %q 键：%v", k, ns)
		}
	}
	if ns["slug"] != store.DefaultNamespaceSlug {
		t.Fatalf("slug = %v, want %q", ns["slug"], store.DefaultNamespaceSlug)
	}
	// 内置能力随命名空间播种（否则 end 节点都声明不出来），所以一定 > 0
	if n, _ := ns["capabilityCount"].(float64); n <= 0 {
		t.Fatalf("capabilityCount = %v, want > 0（内置能力必须播种）", ns["capabilityCount"])
	}
	if n, _ := ns["sourceCount"].(float64); n != 1 {
		t.Fatalf("sourceCount = %v, want 1", ns["sourceCount"])
	}
	// 命名空间自身是部署级资源：行里不该再出现 namespaceId（那等于自指）
	if _, ok := ns["namespaceId"]; ok {
		t.Fatalf("namespace 行不该带 namespaceId：%v", ns)
	}
}

// TestNamespaceListRequiresSession 命名空间列表是部署级数据，必须登录。
func TestNamespaceListRequiresSession(t *testing.T) {
	env := newNSTestEnv(t)
	rr := env.doAnon(t, http.MethodGet, "/v1/namespaces", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestNamespaceCreateRejectsInvalidSlug 非法 slug 一律 400（不是 500，也不是静默改写）。
//
// slug 出现在 URL 与节点令牌作用域里，允许大写/空格/中文会让一半调用方
// 拼错地址（且错误只在生产暴露）。store 对非法 slug 返回的是普通 error，
// handler 不先校验就会落到 500 —— 那会把"你的输入错了"说成"中枢坏了"。
func TestNamespaceCreateRejectsInvalidSlug(t *testing.T) {
	env := newNSTestEnv(t)
	for _, slug := range []string{"", "Prod", "pro d", "生产", "-prod", strings.Repeat("a", 32)} {
		t.Run(slug, func(t *testing.T) {
			rr := env.do(t, http.MethodPost, "/v1/namespaces", map[string]any{"slug": slug, "name": "X"})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for slug %q (body=%s)", rr.Code, slug, rr.Body.String())
			}
		})
	}
}

// TestNamespaceCreateDuplicateSlugConflicts slug 重复 → 409（迁移 14 把唯一键从 (org_id, slug) 改成全局 UNIQUE(slug)）。
func TestNamespaceCreateDuplicateSlugConflicts(t *testing.T) {
	env := newNSTestEnv(t)
	rr := env.do(t, http.MethodPost, "/v1/namespaces", map[string]any{"slug": "staging", "name": "预发"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	created := nsDecode(t, rr)["namespace"].(map[string]any)
	if created["slug"] != "staging" || created["name"] != "预发" {
		t.Fatalf("namespace = %v, want slug=staging name=预发", created)
	}
	if created["namespaceId"] != nil {
		t.Fatalf("新建的 namespace 行不该带 namespaceId：%v", created)
	}

	rr = env.do(t, http.MethodPost, "/v1/namespaces", map[string]any{"slug": "staging", "name": "另一个"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestNamespaceCreateSeedsBuiltinCapabilities 新命名空间必须在同事务里播种内置能力。
//
// 没有 notify.phone 的命名空间里连一个 end 节点都声明不出来，推送直接失效 ——
// 而且失败方式是"新建的命名空间里什么都跑不起来"，排查成本极高。
func TestNamespaceCreateSeedsBuiltinCapabilities(t *testing.T) {
	env := newNSTestEnv(t)
	rr := env.do(t, http.MethodPost, "/v1/namespaces", map[string]any{"slug": "staging", "name": "预发"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	id := nsDecode(t, rr)["namespace"].(map[string]any)["id"].(string)
	if got := env.builtinCapID(t, id, "notify.phone"); got == "" {
		t.Fatal("新命名空间里没有 notify.phone：内置能力没播种")
	}
}

// TestNamespacePatchRenamesNameOnlyOnly PATCH 只改 name；带 slug 一律 400，且 slug 不变。
//
// 为什么带 slug 要拒绝而不是忽略：slug 是稳定标识（URL + 令牌作用域）。
// "静默忽略"的结果是用户以为自己改了名，直到某天发现 URL 还是旧的。
func TestNamespacePatchRenamesNameOnly(t *testing.T) {
	env := newNSTestEnv(t)
	ns := env.newNamespace(t, "staging", "预发")

	rr := env.do(t, http.MethodPatch, "/v1/namespaces/"+ns.ID, map[string]any{"name": "预发环境"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	body := nsDecode(t, rr)["namespace"].(map[string]any)
	if body["name"] != "预发环境" {
		t.Fatalf("name = %v, want 预发环境", body["name"])
	}
	if body["slug"] != "staging" {
		t.Fatalf("slug = %v, want staging（未改）", body["slug"])
	}

	// 带 slug 的请求：400，且库里 slug/name 都不变
	rr = env.do(t, http.MethodPatch, "/v1/namespaces/"+ns.ID, map[string]any{"slug": "renamed", "name": "再改"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("带 slug 的 PATCH status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	got, err := env.db.GetNamespace(context.Background(), ns.ID)
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if got.Slug != "staging" || got.Name != "预发环境" {
		t.Fatalf("namespace = %+v, want slug=staging name=预发环境（带 slug 的 PATCH 不能生效）", got)
	}

	// name 缺失 → 400
	rr = env.do(t, http.MethodPatch, "/v1/namespaces/"+ns.ID, map[string]any{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("空 body status = %d, want 400", rr.Code)
	}
}

// TestNamespacePatchUnknownIs404 不存在的 id → 404（组织层删除后没有「别人的命名空间」，
// 所以 404 就是「不存在」：探测与正常访问的回答本来就一致，没有第二套映射）。
func TestNamespacePatchUnknownIs404(t *testing.T) {
	env := newNSTestEnv(t)
	rr := env.do(t, http.MethodPatch, "/v1/namespaces/ns_doesnotexist", map[string]any{"name": "x"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestNamespaceDeleteLastIs400 不允许删最后一个命名空间（契约 §2.1：不存在没有命名空间的部署）。
func TestNamespaceDeleteLastIs400(t *testing.T) {
	env := newNSTestEnv(t)
	rr := env.do(t, http.MethodDelete, "/v1/namespaces/"+env.nsID+"?force=true", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	// 删失败之后命名空间必须还在（不能删一半）
	if _, err := env.db.GetNamespace(context.Background(), env.nsID); err != nil {
		t.Fatalf("default namespace 应该还在：%v", err)
	}
}

// TestNamespaceDeleteBlockedListsBlocking 非空且没带 force → 409，body 列出阻塞对象。
//
// 删命名空间 = 里面的机器全部掉线（令牌失效）。默认档必须让人看清自己在拆什么，
// 所以 409 的 body 里要有 blocking 清单，而不是一句"不能删"。
func TestNamespaceDeleteBlockedListsBlocking(t *testing.T) {
	env := newNSTestEnv(t)
	ns := env.newNamespace(t, "staging", "预发")
	env.createSource(t, ns.ID, "src-in-staging")

	rr := env.do(t, http.MethodDelete, "/v1/namespaces/"+ns.ID, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rr.Code, rr.Body.String())
	}
	body := nsDecode(t, rr)
	blocking, _ := body["blocking"].([]any)
	if len(blocking) == 0 {
		t.Fatalf("409 必须列出 blocking 清单，body=%s", rr.Body.String())
	}
	joined := ""
	for _, b := range blocking {
		joined += b.(string) + ","
	}
	if !strings.Contains(joined, "sources=1") {
		t.Fatalf("blocking = %v, want 含 sources=1", blocking)
	}
	// 阻塞后命名空间还在
	if _, err := env.db.GetNamespace(context.Background(), ns.ID); err != nil {
		t.Fatalf("被阻塞的删除不该删掉任何东西：%v", err)
	}

	// force=true → 200，级联删掉（含里面的源）
	rr = env.do(t, http.MethodDelete, "/v1/namespaces/"+ns.ID+"?force=true", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("force status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if _, err := env.db.GetNamespace(context.Background(), ns.ID); err == nil {
		t.Fatal("force=true 之后命名空间应该被删掉")
	}
}

// TestNamespaceDeleteUnknownIs404 不存在的 id → 404；而一个**真实存在**的命名空间照常能删。
//
// 本用例改写自 TestNamespaceDeleteForeignOrgIs403（组织层已随迁移 14 删除，#204）：
//
//	改写前证明什么：跨组织越权被拦在 403 —— 别人的 org 里的命名空间，
//	  即使 id 猜对了也删不掉（安全边界是 org_members）。
//	改写后证明什么：一套部署 = 一个团队，本部署内任何已登录用户都能访问任何
//	  **存在**的命名空间，所以唯一的失败原因是「不存在」→ 404；而 404 不是
//	  权限拒绝的马甲 —— 紧接着对另一个真实存在的命名空间做完全相同的 DELETE
//	  会返回 200 且行真的消失，说明那 404 只源于目标不存在。
//
// 为什么不能直接删掉这条：它是「删错了目标也不会误删到别人的东西」的守卫，
// 换成命名空间口径后守卫的对象变成「不存在 vs 存在」两种回答不可混淆。
func TestNamespaceDeleteUnknownIs404(t *testing.T) {
	env := newNSTestEnv(t)

	rr := env.do(t, http.MethodDelete, "/v1/namespaces/ns_doesnotexist?force=true", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
	}

	// 同样的请求打到一个真实存在的命名空间上必须成功：否则上面的 404 可能只是
	// 「一律拒绝」的马甲，而不是「这个 id 不存在」。
	other := env.newNamespace(t, "staging", "预发")
	rr = env.do(t, http.MethodDelete, "/v1/namespaces/"+other.ID+"?force=true", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("删真实存在的命名空间 status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if _, err := env.db.GetNamespace(context.Background(), other.ID); err == nil {
		t.Fatal("staging 命名空间应该已被删掉")
	}
}

// ---------- /v1/stats ----------

// TestStatsRequiresNamespaceId 统计是命名空间资源（契约 §6.2）：缺 namespaceId → 400。
//
// 关键在"不回退默认命名空间"：回退会把"前端忘了传"这种 bug 藏起来，
// 表现为"prod 的页面显示 staging 的数字"，而且永远不会报错。
func TestStatsRequiresNamespaceId(t *testing.T) {
	env := newNSTestEnv(t)
	rr := env.do(t, http.MethodGet, "/v1/stats", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "namespaceId") {
		t.Fatalf("400 的 body 要说清缺的是 namespaceId：%s", body)
	}
}

// TestStatsWithNamespaceCountsOnlyThatNamespace 统计只数本命名空间的事件。
func TestStatsWithNamespaceCountsOnlyThatNamespace(t *testing.T) {
	env := newNSTestEnv(t)
	other := env.newNamespace(t, "staging", "预发")

	// 两个命名空间各插一条事件
	for _, nsID := range []string{env.nsID, other.ID} {
		nsID := nsID
		if _, err := env.db.InsertEvent(context.Background(), &store.Event{
			NamespaceID: nsID, Source: "src", Type: "ping",
			TraceID: "tr_" + nsID, Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("insert event in %s: %v", nsID, err)
		}
	}

	rr := env.do(t, http.MethodGet, "/v1/stats?namespaceId="+env.nsID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	body := nsDecode(t, rr)
	if total, _ := body["total"].(float64); total != 1 {
		t.Fatalf("total = %v, want 1（只数本命名空间）", body["total"])
	}

	// `*` 不支持：store.MessageStats 明确拒绝跨命名空间汇总（口径是"把各 ns 加起来"，
	// 与"这个命名空间的统计"是两个数，不能悄悄给一个）
	rr = env.do(t, http.MethodGet, "/v1/stats?namespaceId=*", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("namespaceId=* status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}
