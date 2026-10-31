package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件覆盖预案（playbook）5 个端点的 HTTP 契约（#193 / 契约 §6，测试清单 §9 W3）。
//
// 环境与 consoleEnv / nsTestEnv **故意分开**（同 nsTestEnv 的理由）：这里要同时挂
// 预案路由 + 源的 webhook 入口（真 sourceHandler + 假派发器）+ 能力 / 源列表。
// 复用别人的 env，会让「任一侧新增一条路由」变成另一侧测试的 red。
//
// 测试全部走**真 SQLite + 真会话中间件**：假 store 对赌不出事务、唯一约束、
// 幂等这些真正容易错的地方。

// playbookEnv 是预案端点的测试环境。
type playbookEnv struct {
	mux        http.Handler
	db         *store.DB
	userID     string
	nsID       string // 本环境的默认命名空间（组织层已删除，命名空间是唯一隔离边界）
	cookie     *http.Cookie
	dispatcher *nsTestDispatcher // webhook 入口的假派发器（不发帧、不落库）
}

// newPlaybookEnv 建库 + 用户（含默认命名空间）+ 会话，并挂上与生产一致的路由。
//
// 组织层已在迁移 14 删除（#204）：db.RegisterUser 一步建出「用户 + 默认命名空间
// + 内置能力」，返回值就是那个命名空间。
func newPlaybookEnv(t *testing.T) *playbookEnv {
	t.Helper()
	ctx := context.Background()

	// 收尾走 closeDBForTempDir（#259）：关库钉在 t.TempDir 的 RemoveAll 之前，
	// 并断言没有 goroutine 在关库窗口里用库（失败即报「收尾竞态」）。
	dir := t.TempDir()
	db, err := store.Open(dir + "/playbook.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { closeDBForTempDir(t, db, dir) })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify Playbook Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	u := &store.User{
		ID: store.NewUserID(), Username: "pbuser", DisplayName: "PB User", CreatedAt: store.Now(),
	}
	defNS, err := db.RegisterUser(ctx, u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	uid, nsID := u.ID, defNS.ID
	sess, err := svc.Sessions().Create(uid, "Test · Playbook")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	consoleH := &consoleHandler{db: db, auth: svc}
	pbH := &playbookHandler{db: db, console: consoleH}
	disp := &nsTestDispatcher{}
	srcH := &sourceHandler{
		db:               db,
		commander:        &source.Trigger{Store: db, Dispatcher: disp},
		resolveNamespace: consoleH.resolveNamespace,
	}

	sessMW := svc.Sessions().Middleware
	mux := http.NewServeMux()
	// 与 mux.go 的注册顺序 / 包装保持一致（noStore + sessMW）
	mux.Handle("GET /v1/playbooks", noStore(sessMW(http.HandlerFunc(pbH.list))))
	mux.Handle("POST /v1/playbooks", noStore(sessMW(http.HandlerFunc(pbH.create))))
	mux.Handle("GET /v1/playbooks/{idOrSlug}", noStore(sessMW(http.HandlerFunc(pbH.byKey))))
	mux.Handle("DELETE /v1/playbooks/{idOrSlug}", noStore(sessMW(http.HandlerFunc(pbH.byKey))))
	mux.Handle("POST /v1/playbooks/{idOrSlug}/install", noStore(sessMW(http.HandlerFunc(pbH.install))))
	// webhook 入口**不要会话**（鉴权是 URL 里的 token），要真 sourceHandler + 假派发器
	mux.Handle("POST /v1/hooks/{token}", noStore(http.HandlerFunc(srcH.hook)))
	// 安装结果要在 HTTP 视角被看见：能力 / 源列表
	mux.Handle("GET /v1/capabilities", noStore(sessMW(http.HandlerFunc(consoleH.capabilities))))
	mux.Handle("GET /v1/sources", noStore(sessMW(http.HandlerFunc(srcH.sources))))

	return &playbookEnv{
		mux: mux, db: db, userID: uid, nsID: nsID,
		cookie:     &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
		dispatcher: disp,
	}
}

// do 发一个带会话 Cookie 的请求。
func (e *playbookEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf strings.Builder
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		buf.Write(b)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(buf.String()))
	req.AddCookie(e.cookie)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// doAnon 发一个不带会话的请求（webhook 入口用：外部系统没有我们的 Cookie）。
func (e *playbookEnv) doAnon(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// newNamespace 直接走 store 建一个命名空间（隔离用例的前置）。
func (e *playbookEnv) newNamespace(t *testing.T, slug, name string) *store.Namespace {
	t.Helper()
	ns, err := e.db.CreateNamespace(context.Background(), slug, name)
	if err != nil {
		t.Fatalf("create namespace %q: %v", slug, err)
	}
	return ns
}

// create 建一个预案并断言 201，返回响应里的 playbook 对象。
func (e *playbookEnv) create(t *testing.T, nsID string, body map[string]any) map[string]any {
	t.Helper()
	rr := e.do(t, http.MethodPost, withNSID("/v1/playbooks", nsID), body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /v1/playbooks status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	return pbOf(t, decode(t, rr))
}

// install 安装一份预案并断言 200，返回整个响应体。
func (e *playbookEnv) install(t *testing.T, nsID, idOrSlug string) map[string]any {
	t.Helper()
	rr := e.do(t, http.MethodPost, withNSID("/v1/playbooks/"+idOrSlug+"/install", nsID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST install(%s) status = %d, want 200; body=%s", idOrSlug, rr.Code, rr.Body.String())
	}
	return decode(t, rr)
}

// rowCounts 取一个命名空间里三类零件的行数（钉住「幂等安装不新增行」）。
func (e *playbookEnv) rowCounts(t *testing.T, nsID string) [3]int {
	t.Helper()
	tables := [3]string{"capability_versions", "edges", "sources"}
	var out [3]int
	for i, tbl := range tables {
		var n int
		if err := e.db.QueryRow(`SELECT count(*) FROM `+tbl+` WHERE namespace_id = ?`, nsID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[i] = n
	}
	return out
}

// ---------- 测试夹具 / 断言小工具 ----------

// pbFixtureManifest 是一份「3 能力 / 2 边 / 1 webhook 触发」的 manifest。
//
// 入口能力 = analyze-pr（没有作为任何边的 to）→ 安装出的 webhook 源指向它。
func pbFixtureManifest() map[string]any {
	return map[string]any{
		"capabilities": []map[string]any{
			{"name": "analyze-pr", "version": 1, "outputs": []string{"analyzed"}},
			{
				"name": "review-pr", "version": 1, "outputs": []string{"approved"},
				// runner 是节点侧配置：中枢只存不解释，必须原样往返（D1）
				"runner": map[string]any{"env": []string{"MODE=strict"}},
			},
			{"name": "notify-pr", "version": 1},
		},
		"edges": []map[string]any{
			{"from": "analyze-pr", "fromPort": "analyzed", "to": "review-pr"},
			{"from": "review-pr", "fromPort": "approved", "to": "notify-pr"},
		},
		"triggers": []map[string]any{
			{"kind": "webhook", "types": []string{"pull_request.opened"}},
		},
	}
}

// pbOf 从响应体里取 playbook 对象。
func pbOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	raw, ok := body["playbook"]
	if !ok {
		t.Fatalf("响应里没有 playbook 字段: %v", body)
	}
	pb, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("playbook 不是对象（%T）: %v", raw, raw)
	}
	return pb
}

// pbItems 取 playbook.items（必须是数组，每一项是对象）。
func pbItems(t *testing.T, pb map[string]any) []map[string]any {
	t.Helper()
	raw, ok := pb["items"]
	if !ok {
		t.Fatalf("playbook 里没有 items 字段（详情/创建必须带）: %v", pb)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("items 不是数组（%T）: %v", raw, raw)
	}
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		item, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("items 里有非对象项: %v", v)
		}
		out = append(out, item)
	}
	return out
}

// strList 从 map 里取字符串数组（缺字段 / 类型错即失败）。
func strList(t *testing.T, m map[string]any, key string) []string {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("响应里没有 %q 字段: %v", key, m)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("%q 不是数组（%T）: %v", key, raw, raw)
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%q 里有非字符串项: %v", key, v)
		}
		out = append(out, s)
	}
	return out
}

// contains 判断字符串切片里是否有 s。
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------- 1. 命名空间作用域 ----------

// TestPlaybookRequiresNamespace 契约 §6 / D2：5 个端点全都必填 namespaceId。
//
// 缺参 400（且点名 namespaceId）、`*` 400、不存在的命名空间 404（不回显 id）。
// 读端点也不例外：预案没有「默认命名空间」也没有跨命名空间汇总形态。
func TestPlaybookRequiresNamespace(t *testing.T) {
	env := newPlaybookEnv(t)

	t.Run("POST 缺 namespaceId", func(t *testing.T) {
		rr := env.do(t, http.MethodPost, "/v1/playbooks",
			map[string]any{"slug": "pr-review", "name": "PR 审查"})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		if msg := mustStr(t, decode(t, rr), "error"); !strings.Contains(msg, "namespaceId") {
			t.Fatalf("错误文案没有点名 namespaceId: %q", msg)
		}
	})

	t.Run("GET 列表缺 namespaceId", func(t *testing.T) {
		rr := env.do(t, http.MethodGet, "/v1/playbooks", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		if msg := mustStr(t, decode(t, rr), "error"); !strings.Contains(msg, "namespaceId") {
			t.Fatalf("错误文案没有点名 namespaceId: %q", msg)
		}
	})

	t.Run("query 带 *", func(t *testing.T) {
		rr := env.do(t, http.MethodGet, withNSID("/v1/playbooks", store.AllNamespaces), nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		msg := mustStr(t, decode(t, rr), "error")
		if !strings.Contains(msg, "namespaceId") || !strings.Contains(msg, "*") {
			t.Fatalf("错误文案没有点名 namespaceId=*: %q", msg)
		}
	})

	t.Run("body 带 *", func(t *testing.T) {
		// query 优先、body 兜底：换个位置带 `*` 不能绕过
		rr := env.do(t, http.MethodPost, "/v1/playbooks",
			map[string]any{"namespaceId": store.AllNamespaces, "slug": "pr-review", "name": "PR"})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
	})

	// 迁移 14 删除组织层后（#204）**不存在「别人的命名空间」**：本部署内任何已登录用户
	// 都能读写任何存在的命名空间。所以原来的「别的 org 的 ns → 404」用例改写成下面两条
	// 命名空间口径断言（覆盖点不减、力度更强）：
	//   ① 不存在的 id → 404 且不回显 id（防探测）；
	//   ② 另一个**真实存在**的命名空间 → 读写都正常（200 / 201），不再是 403/404 的禁区，
	//      而且预案真的落在那个空间里（作用域按请求生效，而不是写死默认空间）。
	t.Run("不存在的 ns 是 404 且不回显 id", func(t *testing.T) {
		const missingNS = "ns_not_a_real_id"
		missing := env.do(t, http.MethodGet, withNSID("/v1/playbooks", missingNS), nil)
		if missing.Code != http.StatusNotFound {
			t.Fatalf("不存在的 namespaceId status = %d, want 404; body=%s", missing.Code, missing.Body.String())
		}
		if strings.Contains(missing.Body.String(), missingNS) {
			t.Fatalf("404 响应里回显了传入的命名空间 id: %s", missing.Body.String())
		}
		if msg := mustStr(t, decode(t, missing), "error"); msg != "namespace not found" {
			t.Fatalf("404 的 error = %q, want %q（不存在的命名空间只有一种回答）", msg, "namespace not found")
		}
	})

	t.Run("另一个已存在的命名空间是可读写的", func(t *testing.T) {
		other := env.newNamespace(t, "another", "另一个命名空间")

		// 读：200 且是本空间的空列表 []（既不是 403/404，也不会把本空间的预案串进去）。
		rr := env.do(t, http.MethodGet, withNSID("/v1/playbooks", other.ID), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("已存在的 namespaceId status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		if raw := rr.Body.String(); !strings.Contains(raw, `"playbooks":[]`) {
			t.Fatalf("另一个命名空间的列表不是空数组: %s", raw)
		}

		// 写：201，而且 playbook.namespaceId 指向那个命名空间（证明作用域真的生效）。
		pb := env.create(t, other.ID, map[string]any{"slug": "other-plan", "name": "另一个空间的预案"})
		if pb["namespaceId"] != other.ID {
			t.Fatalf("playbook.namespaceId = %v, want %s（作用域必须按请求的 namespaceId 生效）",
				pb["namespaceId"], other.ID)
		}
		if pb["namespaceId"] == env.nsID {
			t.Fatalf("playbook.namespaceId 落回了默认命名空间 %s（请求里的 namespaceId 被忽略）", env.nsID)
		}
	})
}

// ---------- 2. 创建 ----------

// TestPlaybookCreate 契约 §6 / D3 / D4 / D13：
// 201 + playbook.items 存在（空 manifest 时是 []，不是 null）；
// slug 重名 409（code=slug_taken）、slug 非法 400、name 缺 400。
func TestPlaybookCreate(t *testing.T) {
	env := newPlaybookEnv(t)

	// 没写 manifest → 归一化成三个空数组，items 是 []
	rr := env.do(t, http.MethodPost, withNSID("/v1/playbooks", env.nsID),
		map[string]any{"slug": "empty-plan", "name": "空预案", "description": "一句话"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	pb := pbOf(t, decode(t, rr))
	if items := pbItems(t, pb); len(items) != 0 {
		t.Fatalf("空 manifest 的 items = %v, want []", items)
	}
	// 原始 JSON 断言：空数组而不是 null（不变量 I9）
	raw := rr.Body.String()
	if !strings.Contains(raw, `"items":[]`) {
		t.Fatalf("items 不是空数组（可能是 null）: %s", raw)
	}
	if !strings.Contains(raw, `"manifest":{"capabilities":[],"edges":[],"triggers":[]}`) {
		t.Fatalf("缺省 manifest 没被归一化成三个空数组: %s", raw)
	}
	// 响应要带 namespaceId（I10 的可见性）
	if pb["namespaceId"] != env.nsID {
		t.Fatalf("playbook.namespaceId = %v, want %s", pb["namespaceId"], env.nsID)
	}
	// 组织层已随迁移 14 删除（#204）：响应里**不能**再有 orgId。原来的「orgId == 本 org」
	// 断言换成更严的一条——字段彻底不在（隔离维度只剩 namespaceId，残留 orgId 会让调用方误以为组织还在）。
	if v, ok := pb["orgId"]; ok {
		t.Fatalf("playbook.orgId = %v，组织层已删除，视图里不该再有这个字段", v)
	}
	if pb["slug"] != "empty-plan" {
		t.Fatalf("playbook.slug = %v, want empty-plan", pb["slug"])
	}

	// manifest 原文往返：runner 这类节点侧字段原样回来（D1）
	full := env.create(t, env.nsID, map[string]any{
		"slug": "pr-review", "name": "PR 审查", "manifest": pbFixtureManifest(),
	})
	b, err := json.Marshal(full["manifest"])
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if !strings.Contains(string(b), "MODE=strict") {
		t.Fatalf("manifest 里的 runner 没有原样回传: %s", b)
	}

	// slug 重名 → 409（不是 400：撞名是「与已有资源冲突」，不是参数格式错）
	rr = env.do(t, http.MethodPost, withNSID("/v1/playbooks", env.nsID),
		map[string]any{"slug": "pr-review", "name": "另一个 PR 审查"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("slug 重名 status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if code := mustStr(t, decode(t, rr), "code"); code != "slug_taken" {
		t.Fatalf("code = %q, want slug_taken", code)
	}

	// slug 非法（大写）→ 400，且**不**被静默改成小写
	rr = env.do(t, http.MethodPost, withNSID("/v1/playbooks", env.nsID),
		map[string]any{"slug": "PR-Review", "name": "大小写混写"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 slug status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if code := mustStr(t, decode(t, rr), "code"); code != "invalid_playbook" {
		t.Fatalf("code = %q, want invalid_playbook", code)
	}

	// name 缺 → 400
	rr = env.do(t, http.MethodPost, withNSID("/v1/playbooks", env.nsID),
		map[string]any{"slug": "no-name"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 name status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if code := mustStr(t, decode(t, rr), "code"); code != "invalid_playbook" {
		t.Fatalf("code = %q, want invalid_playbook", code)
	}
}

// ---------- 3. 列表 / 详情 ----------

// TestPlaybookListAndDetail 契约 §6 / D5 / I11：
// 空列表是 []（原始 JSON）、命名空间隔离、按 id / slug 都能取、不存在 404。
func TestPlaybookListAndDetail(t *testing.T) {
	env := newPlaybookEnv(t)
	nsB := env.newNamespace(t, "staging", "Staging")

	// 空列表原始 JSON 是 []（不是 null）
	rr := env.do(t, http.MethodGet, withNSID("/v1/playbooks", env.nsID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if raw := rr.Body.String(); !strings.Contains(raw, `"playbooks":[]`) {
		t.Fatalf("空列表不是 []: %s", raw)
	}

	pb := env.create(t, env.nsID, map[string]any{
		"slug": "pr-review", "name": "PR 审查", "manifest": pbFixtureManifest(),
	})
	pbID, _ := pb["id"].(string)
	if pbID == "" {
		t.Fatalf("创建响应没有 id: %v", pb)
	}

	// A 空间的列表能看到它（视图形态：带 status/next/ready/total/counts，不带 items）
	rr = env.do(t, http.MethodGet, withNSID("/v1/playbooks", env.nsID), nil)
	listBody := decode(t, rr)
	views, ok := listBody["playbooks"].([]any)
	if !ok || len(views) != 1 {
		t.Fatalf("playbooks = %v, want 1 项; body=%s", listBody["playbooks"], rr.Body.String())
	}
	view := views[0].(map[string]any)
	if view["id"] != pbID {
		t.Fatalf("列表里的 id = %v, want %s", view["id"], pbID)
	}
	if _, hasItems := view["items"]; hasItems {
		t.Fatalf("列表项不该带 items（详情才带）: %v", view)
	}
	if listBody["namespaceId"] != env.nsID {
		t.Fatalf("namespaceId = %v, want %s", listBody["namespaceId"], env.nsID)
	}

	// B 空间看不到（I11）
	rr = env.do(t, http.MethodGet, withNSID("/v1/playbooks", nsB.ID), nil)
	if raw := rr.Body.String(); !strings.Contains(raw, `"playbooks":[]`) {
		t.Fatalf("别的命名空间看到了本空间的预案: %s", raw)
	}

	// 按 id 与按 slug 都能取到
	for _, key := range []string{pbID, "pr-review"} {
		rr = env.do(t, http.MethodGet, withNSID("/v1/playbooks/"+key, env.nsID), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /v1/playbooks/%s status = %d; body=%s", key, rr.Code, rr.Body.String())
		}
		if got := pbOf(t, decode(t, rr)); got["id"] != pbID {
			t.Fatalf("按 %s 取到的预案 id = %v, want %s", key, got["id"], pbID)
		}
	}

	// 别的命名空间按 id 取 → 404（跨命名空间等同于不存在）
	rr = env.do(t, http.MethodGet, withNSID("/v1/playbooks/"+pbID, nsB.ID), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("跨命名空间取详情 status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if code := mustStr(t, decode(t, rr), "code"); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}

	// 不存在的 id / slug → 404
	rr = env.do(t, http.MethodGet, withNSID("/v1/playbooks/pb_does_not_exist", env.nsID), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在的预案 status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}

	// 跨命名空间 install → 404（I11 / M7）：拿着别空间的 namespaceId 装本空间的预案，
	// 与「预案不存在」逐字节同形，且目标命名空间里一个零件都不多。
	ctx := context.Background()
	t.Run("跨命名空间 install 是 404 且不写入", func(t *testing.T) {
		beforeB := env.rowCounts(t, nsB.ID)
		rr := env.do(t, http.MethodPost, withNSID("/v1/playbooks/pr-review/install", nsB.ID), nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("跨命名空间 install status = %d, want 404; body=%s", rr.Code, rr.Body.String())
		}
		crossBody := decode(t, rr)
		if msg := mustStr(t, crossBody, "error"); msg != "预案不存在" {
			t.Fatalf("跨命名空间 install 的 error = %q, want 预案不存在", msg)
		}
		if code := mustStr(t, crossBody, "code"); code != "not_found" {
			t.Fatalf("跨命名空间 install 的 code = %q, want not_found", code)
		}
		if afterB := env.rowCounts(t, nsB.ID); afterB != beforeB {
			t.Fatalf("跨命名空间 install 在目标命名空间里写入了零件: before=%v after=%v", beforeB, afterB)
		}
	})

	// 列表顺序（D5 / M6）：created_at, id 升序（先建的在前）。
	// 故意让**晚的**那条先插入：按插入顺序排会得出别的答案，只有真按 created_at
	// 升序才对（改成 DESC 也会红）。
	stored, err := env.db.GetPlaybook(ctx, env.nsID, pbID)
	if err != nil {
		t.Fatalf("get playbook by id: %v", err)
	}
	base := stored.CreatedAt + 1000
	if err := env.db.CreatePlaybook(ctx, &store.Playbook{
		NamespaceID: env.nsID, Slug: "later-plan", Name: "晚的", CreatedAt: base + 100,
	}); err != nil {
		t.Fatalf("create later plan: %v", err)
	}
	if err := env.db.CreatePlaybook(ctx, &store.Playbook{
		NamespaceID: env.nsID, Slug: "earlier-plan", Name: "早的", CreatedAt: base,
	}); err != nil {
		t.Fatalf("create earlier plan: %v", err)
	}
	rr = env.do(t, http.MethodGet, withNSID("/v1/playbooks", env.nsID), nil)
	listViews, ok := decode(t, rr)["playbooks"].([]any)
	if !ok {
		t.Fatalf("playbooks 不是数组: %s", rr.Body.String())
	}
	slugs := make([]string, 0, len(listViews))
	for _, raw := range listViews {
		slug, _ := raw.(map[string]any)["slug"].(string)
		slugs = append(slugs, slug)
	}
	if got, want := strings.Join(slugs, ","), "pr-review,earlier-plan,later-plan"; got != want {
		t.Fatalf("列表顺序 want %q got %q（created_at 升序）", want, got)
	}
}

// ---------- 4. install 幂等 ----------

// TestPlaybookInstallIsIdempotent 契约 §6 / D8：连跑两次，第二次 created 三数组全空、
// skipped 全满，且三类零件的行数一行都不多。
func TestPlaybookInstallIsIdempotent(t *testing.T) {
	env := newPlaybookEnv(t)
	env.create(t, env.nsID, map[string]any{
		"slug": "pr-review", "name": "PR 审查", "manifest": pbFixtureManifest(),
	})

	first := env.install(t, env.nsID, "pr-review")
	created := first["created"].(map[string]any)
	if got := strList(t, created, "capabilities"); len(got) != 3 {
		t.Fatalf("第一次 created.capabilities = %v, want 3 项", got)
	}
	if got := strList(t, created, "edges"); len(got) != 2 {
		t.Fatalf("第一次 created.edges = %v, want 2 项", got)
	}
	if got := strList(t, created, "sources"); len(got) != 1 || got[0] != "pr-review-webhook-0" {
		t.Fatalf("第一次 created.sources = %v, want [pr-review-webhook-0]", got)
	}
	before := env.rowCounts(t, env.nsID)

	rr := env.do(t, http.MethodPost, withNSID("/v1/playbooks/pr-review/install", env.nsID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("第二次 install status = %d; body=%s", rr.Code, rr.Body.String())
	}
	// 原始 JSON：三数组是 []（不是 null）
	if raw := rr.Body.String(); !strings.Contains(raw, `"created":{"capabilities":[],"edges":[],"sources":[]}`) {
		t.Fatalf("第二次 install 的 created 不是三个空数组: %s", raw)
	}
	second := decode(t, rr)
	skipped := second["skipped"].(map[string]any)
	if got := strList(t, skipped, "capabilities"); len(got) != 3 {
		t.Fatalf("第二次 skipped.capabilities = %v, want 3 项（全满）", got)
	}
	if got := strList(t, skipped, "edges"); len(got) != 2 {
		t.Fatalf("第二次 skipped.edges = %v, want 2 项（全满）", got)
	}
	if got := strList(t, skipped, "sources"); len(got) != 1 || got[0] != "pr-review-webhook-0" {
		t.Fatalf("第二次 skipped.sources = %v, want [pr-review-webhook-0]", got)
	}
	// 纯幂等不该产出一屏告警：note 只在「跳过有原因」时才有
	if notes, ok := second["notes"].([]any); !ok || len(notes) != 0 {
		t.Fatalf("第二次 install 的 notes = %v, want []", second["notes"])
	}

	if after := env.rowCounts(t, env.nsID); after != before {
		t.Fatalf("幂等安装新增了行: before=%v after=%v", before, after)
	}
}

// ---------- 5. install：单条边不成立只 skip 它自己 ----------

// TestPlaybookInstallSkipsEdgeWithMissingEndpoint 契约 §6 / D10：
// 边的一端没定义 → 该边进 skipped.edges（+note），**其余能力 / 边 / 源照装**。
func TestPlaybookInstallSkipsEdgeWithMissingEndpoint(t *testing.T) {
	env := newPlaybookEnv(t)
	env.create(t, env.nsID, map[string]any{
		"slug": "half-plan", "name": "半份预案",
		"manifest": map[string]any{
			"capabilities": []map[string]any{
				{"name": "only-cap", "version": 1, "outputs": []string{"done"}},
			},
			// to 端 only-cap 之外的能力没在本 manifest 里声明、库里也没有
			"edges": []map[string]any{
				{"from": "only-cap", "fromPort": "done", "to": "ghost-cap"},
			},
			"triggers": []map[string]any{{"kind": "webhook"}},
		},
	})

	res := env.install(t, env.nsID, "half-plan")
	created := res["created"].(map[string]any)
	skipped := res["skipped"].(map[string]any)

	if got := strList(t, skipped, "edges"); len(got) != 1 || got[0] != "only-cap --done--> ghost-cap" {
		t.Fatalf("skipped.edges = %v, want [only-cap --done--> ghost-cap]", got)
	}
	// 其余照装（不整体回滚）
	if got := strList(t, created, "capabilities"); !contains(got, "only-cap@1") {
		t.Fatalf("created.capabilities = %v, 应含 only-cap@1", got)
	}
	if got := strList(t, created, "sources"); !contains(got, "half-plan-webhook-0") {
		t.Fatalf("created.sources = %v, 应含 half-plan-webhook-0", got)
	}
	// notes 要说清缺的是哪个能力（配置期没拦住时，安装期只能靠这句话）
	notes, _ := res["notes"].([]any)
	joined := ""
	for _, n := range notes {
		joined += n.(string) + "\n"
	}
	if !strings.Contains(joined, "ghost-cap") {
		t.Fatalf("notes 没说清缺哪个能力: %q", joined)
	}
}

// ---------- 6. 安装出的 webhook 源真的能收事件 ----------

// TestPlaybookInstalledWebhookAcceptsEvents 契约 §6 / §9 W3.5：
// install 建出的 webhook 源在 GET /v1/sources 里带 hookUrl，
// 直接 POST 那个 URL（无会话）→ 200 且响应里 event 非空（真触发一轮派发）。
func TestPlaybookInstalledWebhookAcceptsEvents(t *testing.T) {
	env := newPlaybookEnv(t)
	env.create(t, env.nsID, map[string]any{
		"slug": "pr-review", "name": "PR 审查", "manifest": pbFixtureManifest(),
	})
	env.install(t, env.nsID, "pr-review")

	rr := env.do(t, http.MethodGet, withNSID("/v1/sources", env.nsID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/sources status = %d; body=%s", rr.Code, rr.Body.String())
	}
	sources, ok := decode(t, rr)["sources"].([]any)
	if !ok {
		t.Fatalf("sources 不是数组: %s", rr.Body.String())
	}
	hookURL := ""
	for _, raw := range sources {
		src := raw.(map[string]any)
		if src["name"] == "pr-review-webhook-0" {
			hookURL, _ = src["hookUrl"].(string)
		}
	}
	if hookURL == "" {
		t.Fatalf("找不到安装出来的 webhook 源的 hookUrl: %s", rr.Body.String())
	}

	// 外部系统视角：没有会话 Cookie，只有 URL 里的 token
	rr = env.doAnon(t, http.MethodPost, hookURL, map[string]any{"type": "pull_request.opened"})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200; body=%s", hookURL, rr.Code, rr.Body.String())
	}
	body := decode(t, rr)
	if body["event"] == nil {
		t.Fatalf("hook 响应里 event 为空: %s", rr.Body.String())
	}
	if ev, ok := body["event"].(map[string]any); !ok || ev["type"] != "pull_request.opened" {
		t.Fatalf("hook 响应里的 event 不是本次投递的事件: %v", body["event"])
	}
	if len(env.dispatcher.got) != 1 {
		t.Fatalf("派发次数 = %d, want 1（事件应真的派到入口能力）", len(env.dispatcher.got))
	}
}

// ---------- 7. 就绪体检走 HTTP ----------

// TestPlaybookChecksOverHTTP 契约 §6 / D6：install 后 3 个能力项是 no-node、总体 needs-node；
// 节点上线并声明这三个能力后，总体 ready 且 ready == total。
func TestPlaybookChecksOverHTTP(t *testing.T) {
	env := newPlaybookEnv(t)
	ctx := context.Background()
	env.create(t, env.nsID, map[string]any{
		"slug": "pr-review", "name": "PR 审查", "manifest": pbFixtureManifest(),
	})
	env.install(t, env.nsID, "pr-review")

	rr := env.do(t, http.MethodGet, withNSID("/v1/playbooks/pr-review", env.nsID), nil)
	pb := pbOf(t, decode(t, rr))
	if pb["status"] != store.PlaybookStatusNeedsNode {
		t.Fatalf("安装后 status = %v, want %s", pb["status"], store.PlaybookStatusNeedsNode)
	}
	items := pbItems(t, pb)
	if len(items) != 6 {
		t.Fatalf("items = %d 项, want 6（3 能力 + 2 边 + 1 触发）", len(items))
	}
	capItems := 0
	for _, item := range items {
		if item["kind"] != store.PlaybookItemCapability {
			continue
		}
		capItems++
		if item["status"] != store.PlaybookCheckNoNode {
			t.Fatalf("能力项 %v 的 status = %v, want %s（还没节点声明）",
				item["ref"], item["status"], store.PlaybookCheckNoNode)
		}
	}
	if capItems != 3 {
		t.Fatalf("能力项 = %d, want 3", capItems)
	}
	// next 是**第一个非 ok 项的对象**（不是一句话）
	next, ok := pb["next"].(map[string]any)
	if !ok {
		t.Fatalf("next 不是对象: %v", pb["next"])
	}
	if next["kind"] != store.PlaybookItemCapability {
		t.Fatalf("next.kind = %v, want capability（能力定义先于节点）", next["kind"])
	}

	// 造一台真节点，声明这三个能力并上线（直接调 store：走 HTTP 注册要令牌握手，
	// 这里验的是体检口径，不是节点协议）
	node := &store.Node{NamespaceID: env.nsID, Name: "runner-1"}
	if _, err := env.db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := env.db.ConfirmCapabilities(ctx, env.nsID, node.ID, []store.CapabilityRef{
		{Name: "analyze-pr", Version: 1},
		{Name: "review-pr", Version: 1},
		{Name: "notify-pr", Version: 1},
	}); err != nil {
		t.Fatalf("confirm capabilities: %v", err)
	}
	if err := env.db.UpdateNodeStatus(ctx, env.nsID, node.ID, store.NodeStatusOnline); err != nil {
		t.Fatalf("update node status: %v", err)
	}

	rr = env.do(t, http.MethodGet, withNSID("/v1/playbooks/pr-review", env.nsID), nil)
	pb = pbOf(t, decode(t, rr))
	if pb["status"] != store.PlaybookStatusReady {
		t.Fatalf("节点上线后 status = %v, want %s；body=%s",
			pb["status"], store.PlaybookStatusReady, rr.Body.String())
	}
	if pb["ready"] != pb["total"] {
		t.Fatalf("ready=%v total=%v，全 ok 时应当相等", pb["ready"], pb["total"])
	}
	if pb["next"] != nil {
		t.Fatalf("全 ok 时 next 应是 null: %v", pb["next"])
	}
	// 列表视图的体检结论同步变化（同一份推导量）
	rr = env.do(t, http.MethodGet, withNSID("/v1/playbooks", env.nsID), nil)
	views := decode(t, rr)["playbooks"].([]any)
	if got := views[0].(map[string]any)["status"]; got != store.PlaybookStatusReady {
		t.Fatalf("列表里的 status = %v, want %s", got, store.PlaybookStatusReady)
	}
}

// ---------- 8. 删除 ----------

// TestPlaybookDelete 契约 §6 / D14：DELETE → 200 {"deleted": true}，再 GET → 404，
// 已装的能力 / 边 / 源**原样保留**（卸载清单不回滚数据）。
func TestPlaybookDelete(t *testing.T) {
	env := newPlaybookEnv(t)
	env.create(t, env.nsID, map[string]any{
		"slug": "pr-review", "name": "PR 审查", "manifest": pbFixtureManifest(),
	})
	env.install(t, env.nsID, "pr-review")

	rr := env.do(t, http.MethodDelete, withNSID("/v1/playbooks/pr-review", env.nsID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if deleted, _ := decode(t, rr)["deleted"].(bool); !deleted {
		t.Fatalf("DELETE 响应 deleted != true: %s", rr.Body.String())
	}

	// 再取 → 404
	rr = env.do(t, http.MethodGet, withNSID("/v1/playbooks/pr-review", env.nsID), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("删除后 GET status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	// 重复删 → 404（未命中就是不存在）
	rr = env.do(t, http.MethodDelete, withNSID("/v1/playbooks/pr-review", env.nsID), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("重复 DELETE status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}

	// 已装能力仍在（GET /v1/capabilities 能看到 analyze-pr@1）
	rr = env.do(t, http.MethodGet, withNSID("/v1/capabilities", env.nsID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/capabilities status = %d; body=%s", rr.Code, rr.Body.String())
	}
	caps, _ := decode(t, rr)["capabilities"].([]any)
	found := false
	for _, raw := range caps {
		c := raw.(map[string]any)
		if c["name"] == "analyze-pr" && c["version"] == float64(1) {
			found = true
		}
	}
	if !found {
		t.Fatalf("删掉预案后能力 analyze-pr@1 不见了（卸载不该回滚数据）: %s", rr.Body.String())
	}

	// 源也还在（同一个「不回滚」结论的另一个面）
	rr = env.do(t, http.MethodGet, withNSID("/v1/sources", env.nsID), nil)
	if !strings.Contains(rr.Body.String(), "pr-review-webhook-0") {
		t.Fatalf("删掉预案后 webhook 源不见了: %s", rr.Body.String())
	}
}
