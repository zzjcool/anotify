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
	"github.com/zzjcool/anotify/internal/store"
)

// consoleEnv 是控制台 API 的测试环境（真 SQLite + 真会话中间件）。
type consoleEnv struct {
	mux    http.Handler
	db     *store.DB
	svc    *auth.Service
	userID string
	orgID  string
	cookie *http.Cookie
}

// newConsoleEnv 建库 + 用户 + 组织 + 会话，挂上与生产一致的 console 路由。
func newConsoleEnv(t *testing.T) *consoleEnv {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(t.TempDir() + "/console.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc, err := auth.NewService(db, auth.Config{
		RPDisplayName: "Anotify Test",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	// 用户 + 组织（owner）
	uid, err := db.InsertUser(ctx, "", "consoleuser", "Console User")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	// 第二个参数是 username（用于生成默认组织名）
	org, err := db.EnsureOrgForUser(ctx, uid, "consoleuser")
	if err != nil {
		t.Fatalf("ensure org: %v", err)
	}
	sess, err := svc.Sessions().Create(uid, "Test · CLI")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := &consoleHandler{db: db, auth: svc}
	sessMW := svc.Sessions().Middleware
	mux := http.NewServeMux()
	mux.Handle("GET /v1/tasks", noStore(sessMW(http.HandlerFunc(h.tasks))))
	mux.Handle("GET /v1/traces/{id}", noStore(sessMW(http.HandlerFunc(h.trace))))
	mux.Handle("GET /v1/console/events", noStore(sessMW(http.HandlerFunc(h.events))))
	mux.Handle("POST /v1/tasks/{id}/cancel", noStore(sessMW(http.HandlerFunc(h.cancelTask))))
	mux.Handle("GET /v1/capabilities", noStore(sessMW(http.HandlerFunc(h.capabilities))))
	mux.Handle("POST /v1/capabilities", noStore(sessMW(http.HandlerFunc(h.capabilities))))
	mux.Handle("GET /v1/edges", noStore(sessMW(http.HandlerFunc(h.edges))))
	mux.Handle("POST /v1/edges", noStore(sessMW(http.HandlerFunc(h.edges))))
	mux.Handle("DELETE /v1/edges", noStore(sessMW(http.HandlerFunc(h.edges))))
	mux.Handle("GET /v1/nodes", noStore(sessMW(http.HandlerFunc(h.nodes))))
	mux.Handle("GET /v1/nodes/{id}", noStore(sessMW(http.HandlerFunc(h.nodeByID))))
	mux.Handle("POST /v1/nodes/{id}/drain", noStore(sessMW(http.HandlerFunc(h.drainNode))))

	return &consoleEnv{
		mux: mux, db: db, svc: svc, userID: uid, orgID: org.ID,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}
}

// do 发一个带会话 Cookie 的请求。
func (e *consoleEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
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

// doNoCookie 发一个不带会话的请求（验证 401）。
func (e *consoleEnv) doNoCookie(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, req)
	return rr
}

// decode 把响应体解成 map。
func decode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
	return m
}

// seedCapability 建一个能力定义，返回它。
func (e *consoleEnv) seedCapability(t *testing.T, name string, version int, outputs []string) *store.CapabilityVersion {
	t.Helper()
	c := &store.CapabilityVersion{
		OrgID: e.orgID, Name: name, Version: version,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      outputs, Source: store.CapSourceBuiltin,
	}
	if err := e.db.CreateCapability(context.Background(), c); err != nil {
		t.Fatalf("seed capability %s@%d: %v", name, version, err)
	}
	return c
}

// ---------- #59 验收：空列表是 [] ----------

// TestEmptyListIsArray 所有列表端点在空数据时返回 [] 而不是 null（I9，#59 验收）。
func TestEmptyListIsArray(t *testing.T) {
	env := newConsoleEnv(t)

	cases := []struct {
		name string
		path string
		key  string
	}{
		{"tasks", "/v1/tasks", "tasks"},
		{"capabilities", "/v1/capabilities", "capabilities"},
		{"edges", "/v1/edges", "edges"},
		{"nodes", "/v1/nodes", "nodes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := env.do(t, http.MethodGet, c.path, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
			}
			// 直接断言原始 JSON：必须是 [] 而不是 null
			body := rr.Body.String()
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &raw); err != nil {
				t.Fatalf("decode: %v", err)
			}
			val, ok := raw[c.key]
			if !ok {
				t.Fatalf("response missing key %q: %s", c.key, body)
			}
			if string(val) == "null" {
				t.Fatalf("%s is null, want [] (I9)", c.key)
			}
			if string(val)[0] != '[' {
				t.Fatalf("%s = %s, want an array", c.key, val)
			}
		})
	}
}

// ---------- #59 验收：edges 校验 ----------

// TestEdgePortNotInOutputs from_port 不在 outputs 里 → 400（#59 验收 / I2）。
func TestEdgePortNotInOutputs(t *testing.T) {
	env := newConsoleEnv(t)
	from := env.seedCapability(t, "review", 1, []string{"fix"})
	to := env.seedCapability(t, "fix", 1, nil)

	rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		FromCapability: from.ID,
		FromPort:       "clean", // 不在 outputs 里（只有 fix）
		ToCapability:   to.ID,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (port not in outputs); body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	if m["code"] != "edge_rejected" {
		t.Fatalf("code = %v, want edge_rejected", m["code"])
	}
}

// TestEdgeCreateAndDelete 合法的边能建能删。
func TestEdgeCreateAndDelete(t *testing.T) {
	env := newConsoleEnv(t)
	from := env.seedCapability(t, "review", 1, []string{"fix"})
	to := env.seedCapability(t, "fix", 1, nil)

	// 建
	rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		FromCapability: from.ID, FromPort: "fix", ToCapability: to.ID,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}

	// 列
	rr = env.do(t, http.MethodGet, "/v1/edges", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	m := decode(t, rr)
	edges, _ := m["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(edges))
	}

	// 删
	path := "/v1/edges?fromCapability=" + from.ID + "&fromPort=fix&toCapability=" + to.ID
	rr = env.do(t, http.MethodDelete, path, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", rr.Code, rr.Body.String())
	}
	// 再列应为空
	rr = env.do(t, http.MethodGet, "/v1/edges", nil)
	m = decode(t, rr)
	edges, _ = m["edges"].([]any)
	if len(edges) != 0 {
		t.Fatalf("edges = %d after delete, want 0", len(edges))
	}
}

// TestEdgeCrossOrgRejected 跨组织的边 → 400（契约 §1.2）。
func TestEdgeCrossOrgRejected(t *testing.T) {
	env := newConsoleEnv(t)
	from := env.seedCapability(t, "review", 1, []string{"fix"})

	// 另一个组织的能力
	other, err := env.db.CreateOrg(context.Background(), "other-"+store.NewOrgID())
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}
	foreign := &store.CapabilityVersion{
		OrgID: other.ID, Name: "fix", Version: 1,
		ParamsSchema: json.RawMessage(`{}`), Source: store.CapSourceBuiltin,
	}
	if err := env.db.CreateCapability(context.Background(), foreign); err != nil {
		t.Fatalf("create foreign capability: %v", err)
	}

	rr := env.do(t, http.MethodPost, "/v1/edges", edgeRequest{
		FromCapability: from.ID, FromPort: "fix", ToCapability: foreign.ID,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (cross-org edge); body=%s", rr.Code, rr.Body.String())
	}
}

// ---------- #59 验收：组织隔离 ----------

// TestOrgIsolation 非成员访问别人的组织 → 403，且数据不泄漏（#59 验收 / I10）。
func TestOrgIsolation(t *testing.T) {
	env := newConsoleEnv(t)

	// 另一个组织 + 里面的数据
	other, err := env.db.CreateOrg(context.Background(), "other-"+store.NewOrgID())
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}
	foreignCap := &store.CapabilityVersion{
		OrgID: other.ID, Name: "secret-cap", Version: 1,
		ParamsSchema: json.RawMessage(`{}`), Source: store.CapSourceBuiltin,
	}
	if err := env.db.CreateCapability(context.Background(), foreignCap); err != nil {
		t.Fatalf("create foreign capability: %v", err)
	}

	// 用 orgId 参数访问别人的组织 → 403
	rr := env.do(t, http.MethodGet, "/v1/capabilities?orgId="+other.ID, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (not a member of that org)", rr.Code)
	}
	// 响应里不该有任何能力数据
	if bytes.Contains(rr.Body.Bytes(), []byte("secret-cap")) {
		t.Fatalf("cross-org read leaked data: %s", rr.Body.String())
	}

	// 用 X-Org-Id 头同样要 403
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	req.AddCookie(env.cookie)
	req.Header.Set("X-Org-Id", other.ID)
	rr2 := httptest.NewRecorder()
	env.mux.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusForbidden {
		t.Fatalf("X-Org-Id status = %d, want 403", rr2.Code)
	}

	// 访问自己的组织 → 200，且只能看到自己的能力
	rr = env.do(t, http.MethodGet, "/v1/capabilities?orgId="+env.orgID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("own org status = %d", rr.Code)
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("secret-cap")) {
		t.Fatalf("own-org read leaked foreign capability: %s", rr.Body.String())
	}
}

// TestUnauthenticated 无会话 → 401。
func TestUnauthenticated(t *testing.T) {
	env := newConsoleEnv(t)
	for _, path := range []string{"/v1/tasks", "/v1/capabilities", "/v1/edges", "/v1/nodes"} {
		t.Run(path, func(t *testing.T) {
			rr := env.doNoCookie(t, http.MethodGet, path)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
		})
	}
}

// ---------- capabilities ----------

// TestCapabilityCreate 定义新能力：自动算 hash、自动开版本。
func TestCapabilityCreate(t *testing.T) {
	env := newConsoleEnv(t)

	// 第一次：不给 version → 自动 1
	rr := env.do(t, http.MethodPost, "/v1/capabilities", capabilityListRequest{
		Name: "review", Outputs: []string{"fix"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	capObj, _ := m["capability"].(map[string]any)
	if capObj["version"] != 1.0 {
		t.Fatalf("version = %v, want 1 (auto-opened)", capObj["version"])
	}
	if capObj["contentHash"] == "" || capObj["contentHash"] == nil {
		t.Fatal("contentHash was not computed")
	}
	// kind：没有节点声明 → origin
	if m["kind"] != "origin" {
		t.Fatalf("kind = %v, want origin (no node declares it)", m["kind"])
	}

	// 同名换内容 → 自动开版本 2
	rr = env.do(t, http.MethodPost, "/v1/capabilities", capabilityListRequest{
		Name: "review", Outputs: []string{"fix", "clean"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("second create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m = decode(t, rr)
	capObj, _ = m["capability"].(map[string]any)
	if capObj["version"] != 2.0 {
		t.Fatalf("version = %v, want 2", capObj["version"])
	}
}

// mustGetCapability 取一个已播种的能力（不存在则失败）。
func (e *consoleEnv) mustGetCapability(t *testing.T, name string, version int) *store.CapabilityVersion {
	t.Helper()
	c, err := e.db.GetCapability(context.Background(), e.orgID, name, version)
	if err != nil {
		t.Fatalf("capability %s@%d not seeded: %v", name, version, err)
	}
	return c
}

// TestCapabilityListIncludesKind 列表带 kind（推导量，不落库）。
func TestCapabilityListIncludesKind(t *testing.T) {
	env := newConsoleEnv(t)
	review := env.seedCapability(t, "review", 1, []string{"fix"}) // 有出口
	// notify.phone 现在是**内置能力**（#83），由 EnsureOrgForUser 播种。
	// 不要再自己 seed 一份——那会因为内容不同而撞 append-only 约束
	// （capability_versions 的 (org_id,name,version) 唯一 + I8）。
	end := env.mustGetCapability(t, "notify.phone", 1)
	env.seedCapability(t, "trigger", 1, []string{"go"}) // 无节点声明

	// 声明 review 与 notify.phone 给一台节点
	node := &store.Node{OrgID: env.orgID, Name: "mbp"}
	if _, err := env.db.CreateNode(context.Background(), node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := env.db.ConfirmCapabilities(context.Background(), env.orgID, node.ID,
		[]store.CapabilityRef{{Name: "review", Version: 1}, {Name: "notify.phone", Version: 1}},
	); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	rr := env.do(t, http.MethodGet, "/v1/capabilities", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	m := decode(t, rr)
	caps, _ := m["capabilities"].([]any)
	got := map[string]string{}
	for _, c := range caps {
		obj, _ := c.(map[string]any)
		name, _ := obj["name"].(string)
		kind, _ := obj["kind"].(string)
		got[name] = kind
	}
	if got["review"] != "pipe" {
		t.Errorf("review kind = %q, want pipe (declared + has outputs)", got["review"])
	}
	if got["notify.phone"] != "end" {
		t.Errorf("notify.phone kind = %q, want end (declared + no outputs)", got["notify.phone"])
	}
	if got["trigger"] != "origin" {
		t.Errorf("trigger kind = %q, want origin (no node declares it)", got["trigger"])
	}
	_ = review
	_ = end
}

// TestCapabilitySameVersionDifferentContentRejected 同 name@version 换内容 → 409。
func TestCapabilitySameVersionDifferentContentRejected(t *testing.T) {
	env := newConsoleEnv(t)
	env.seedCapability(t, "review", 1, []string{"fix"})

	rr := env.do(t, http.MethodPost, "/v1/capabilities", capabilityListRequest{
		Name: "review", Version: 1, Outputs: []string{"totally-different"},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (version cannot change content); body=%s", rr.Code, rr.Body.String())
	}
}

// ---------- tasks ----------

// TestTaskListAndCancel 列出任务 + 取消。
func TestTaskListAndCancel(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	capDef := env.seedCapability(t, "review", 1, []string{"fix"})

	task := &store.Task{
		OrgID: env.orgID, TraceID: "trace_x", CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: 1, Params: json.RawMessage(`{}`),
		CapHash: capDef.ContentHash, GraphID: "grf_x",
	}
	if err := env.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// 列
	rr := env.do(t, http.MethodGet, "/v1/tasks?traceId=trace_x", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	tasks, _ := m["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if obj, _ := tasks[0].(map[string]any); obj["id"] != task.ID {
		t.Fatalf("wrong task returned")
	}

	// 取消
	rr = env.do(t, http.MethodPost, "/v1/tasks/"+task.ID+"/cancel", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body=%s", rr.Code, rr.Body.String())
	}
	got, err := env.db.GetTask(ctx, env.orgID, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != store.TaskCanceled {
		t.Fatalf("status = %q, want canceled", got.Status)
	}

	// 再取消一次 → 409（终态不可变）
	rr = env.do(t, http.MethodPost, "/v1/tasks/"+task.ID+"/cancel", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("second cancel status = %d, want 409", rr.Code)
	}
}

// TestCancelUnknownTask 取消不存在的任务 → 404。
func TestCancelUnknownTask(t *testing.T) {
	env := newConsoleEnv(t)
	rr := env.do(t, http.MethodPost, "/v1/tasks/tsk_doesnotexist/cancel", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// ---------- nodes ----------

// TestNodeDetail 查单台节点：比列表多给**逐槽**的当前状态。
//
// 详情页要回答"这台机器现在卡在哪个槽上"，列表只给数量是答不了的。
func TestNodeDetail(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	env.seedCapability(t, "review", 1, []string{"fix"})

	node := &store.Node{OrgID: env.orgID, Name: "mbp-detail"}
	if _, err := env.db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := env.db.ConfirmCapabilities(ctx, env.orgID, node.ID,
		[]store.CapabilityRef{{Name: "review", Version: 1}}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := env.db.UpsertInstances(ctx, env.orgID, node.ID,
		[]store.InstanceSpec{{Slot: 0, Concurrency: 3}, {Slot: 1, Concurrency: 2}}); err != nil {
		t.Fatalf("instances: %v", err)
	}

	rr := env.do(t, http.MethodGet, "/v1/nodes/"+node.ID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	obj, _ := m["node"].(map[string]any)
	if obj == nil || obj["name"] != "mbp-detail" {
		t.Fatalf("node = %v", m["node"])
	}
	if got, _ := obj["maxInstances"].(float64); got == 0 {
		t.Fatalf("maxInstances 丢了（页面会显示 undefined），node=%v", obj)
	}
	if caps, _ := obj["capabilities"].([]any); len(caps) != 1 {
		t.Fatalf("capabilities = %v", obj["capabilities"])
	}
	// 老路径（无 manifest）声明：runSpec 必须是空对象，而不是 null/缺失（#168 / I9）
	detailCaps, _ := obj["capabilities"].([]any)
	if cap0, _ := detailCaps[0].(map[string]any); cap0 != nil {
		runSpec, ok := cap0["runSpec"].(map[string]any)
		if !ok || len(runSpec) != 0 {
			t.Fatalf("未声明执行方式时 runSpec 应为 {}，cap=%v", cap0)
		}
	}

	// 逐槽细节：两个槽、各自的并发额度、当前都空闲
	insts, _ := m["instances"].([]any)
	if len(insts) != 2 {
		t.Fatalf("instances = %v, want 2", m["instances"])
	}
	seenSlot := map[float64]bool{}
	for _, raw := range insts {
		slot, _ := raw.(map[string]any)
		n, _ := slot["slot"].(float64)
		seenSlot[n] = true
		if _, ok := slot["concurrency"].(float64); !ok {
			t.Fatalf("concurrency 必须是数字，slot=%v", slot)
		}
		// 空闲槽的 busyTask 必须是 null（不是空串/缺失）
		if v, exists := slot["busyTask"]; !exists || v != nil {
			t.Fatalf("空闲槽 busyTask 应为 null，slot=%v", slot)
		}
	}
	if !seenSlot[0] || !seenSlot[1] {
		t.Fatalf("槽位编号不对: %v", seenSlot)
	}

	// 不存在的节点 → 404（不是 200 空对象）
	rr = env.do(t, http.MethodGet, "/v1/nodes/nd_doesnotexist", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown node status = %d, want 404", rr.Code)
	}
}

// TestNodeDetailRunSpec 节点上报的执行方式在控制台读出（#168 必测 8）。
//
// 两件事同时钉住：
//   - 带 run 的能力：`capabilities[].runSpec.command` 原样可读（展示/排障的唯一用途）
//   - 没声明 run 的能力：`runSpec` 是 `{}`（而不是 null / 缺失）
//
// list 与 detail 共用 viewFor，所以两处都必须一致。
func TestNodeDetailRunSpec(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	env.seedCapability(t, "review", 1, []string{"fix"})
	env.seedCapability(t, "test", 1, nil)

	node := &store.Node{OrgID: env.orgID, Name: "mbp-runspec"}
	if _, err := env.db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	run := `{"runner":"bash","command":"./runners/review.sh","timeoutSec":900,"concurrency":2,"env":{"LOG_LEVEL":"info"}}`
	if _, err := env.db.ConfirmCapabilityDecls(ctx, env.orgID, node.ID, []store.CapabilityDecl{
		{Name: "review", Version: 1, Run: json.RawMessage(run), RunRequired: true},
		{Name: "test", Version: 1}, // manifest 里没写这一项
	}); err != nil {
		t.Fatalf("confirm decls: %v", err)
	}

	getCap := func(path string, extract func(map[string]any) []any) map[string]map[string]any {
		t.Helper()
		rr := env.do(t, http.MethodGet, path, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body=%s", path, rr.Code, rr.Body.String())
		}
		caps := extract(decode(t, rr))
		byName := map[string]map[string]any{}
		for _, raw := range caps {
			c, _ := raw.(map[string]any)
			if c == nil {
				t.Fatalf("GET %s: capability is not an object: %v", path, raw)
			}
			name, _ := c["name"].(string)
			byName[name] = c
		}
		if len(byName) != 2 {
			t.Fatalf("GET %s: capabilities = %v, want 2", path, caps)
		}
		return byName
	}

	checks := []struct {
		path string
		caps func(map[string]any) []any
	}{
		{"/v1/nodes/" + node.ID, func(m map[string]any) []any {
			obj, _ := m["node"].(map[string]any)
			caps, _ := obj["capabilities"].([]any)
			return caps
		}},
		{"/v1/nodes", func(m map[string]any) []any {
			nodes, _ := m["nodes"].([]any)
			for _, raw := range nodes {
				if obj, _ := raw.(map[string]any); obj != nil && obj["id"] == node.ID {
					caps, _ := obj["capabilities"].([]any)
					return caps
				}
			}
			return nil
		}},
	}
	for _, c := range checks {
		byName := getCap(c.path, c.caps)

		runSpec, _ := byName["review"]["runSpec"].(map[string]any)
		if runSpec == nil {
			t.Fatalf("GET %s: review runSpec 丢了：%v", c.path, byName["review"])
		}
		// 原文可读：节点写的是什么，控制台就看得见什么（不改写、不隐藏）
		if got, _ := runSpec["command"].(string); got != "./runners/review.sh" {
			t.Fatalf("GET %s: runSpec.command = %v, want ./runners/review.sh", c.path, runSpec["command"])
		}
		if got, _ := runSpec["timeoutSec"].(float64); got != 900 {
			t.Fatalf("GET %s: runSpec.timeoutSec = %v, want 900", c.path, runSpec["timeoutSec"])
		}
		if got, _ := runSpec["runner"].(string); got != "bash" {
			t.Fatalf("GET %s: runSpec.runner = %v, want bash", c.path, runSpec["runner"])
		}

		// 未声明 → 空对象，不是 null/缺失（前端不用写两套判空）
		testSpec, ok := byName["test"]["runSpec"].(map[string]any)
		if !ok || len(testSpec) != 0 {
			t.Fatalf("GET %s: 未声明执行方式时 runSpec 应为 {}，got %v", c.path, byName["test"]["runSpec"])
		}
	}
}

// TestNodeListAndDrain 列出节点（含能力与实例）+ 排空。
func TestNodeListAndDrain(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	capDef := env.seedCapability(t, "review", 1, []string{"fix"})

	node := &store.Node{OrgID: env.orgID, Name: "mbp-1"}
	if _, err := env.db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := env.db.ConfirmCapabilities(ctx, env.orgID, node.ID,
		[]store.CapabilityRef{{Name: "review", Version: 1}}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := env.db.UpsertInstances(ctx, env.orgID, node.ID,
		[]store.InstanceSpec{{Slot: 0, Concurrency: 1}, {Slot: 1, Concurrency: 1}}); err != nil {
		t.Fatalf("instances: %v", err)
	}

	rr := env.do(t, http.MethodGet, "/v1/nodes", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	nodes, _ := m["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	obj, _ := nodes[0].(map[string]any)
	if obj["name"] != "mbp-1" {
		t.Fatalf("name = %v", obj["name"])
	}
	// capabilities 是**对象**而不是 "name@version" 字符串：节点页要用
	// 显示名（displayName）和稳定的 id（标签点进能力详情）。
	caps, _ := obj["capabilities"].([]any)
	if len(caps) != 1 {
		t.Fatalf("capabilities = %v, want 1 entry", obj["capabilities"])
	}
	cap0, _ := caps[0].(map[string]any)
	if cap0["name"] != "review" || cap0["version"] != 1.0 {
		t.Fatalf("capability name/version = %v/%v", cap0["name"], cap0["version"])
	}
	if got, _ := cap0["id"].(string); got == "" {
		t.Fatalf("capability id 不能为空（页面要用它跳能力详情），cap=%v", cap0)
	}
	// 没取显示名时必须是空串（不是 null），前端才好判断要不要回落
	if _, ok := cap0["displayName"].(string); !ok {
		t.Fatalf("displayName 必须是字符串（空串表示没取），cap=%v", cap0)
	}
	if obj["instances"] != 2.0 {
		t.Fatalf("instances = %v, want 2", obj["instances"])
	}
	if obj["busy"] != 0.0 {
		t.Fatalf("busy = %v, want 0", obj["busy"])
	}

	// 排空
	rr = env.do(t, http.MethodPost, "/v1/nodes/"+node.ID+"/drain", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("drain status = %d, body=%s", rr.Code, rr.Body.String())
	}
	got, err := env.db.GetNode(ctx, env.orgID, node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status != store.NodeStatusDraining {
		t.Fatalf("status = %q, want draining", got.Status)
	}
	// 排空后不再被派发选中
	nodeIDs, err := env.db.NodesForCapability(ctx, env.orgID, capDef.ID)
	if err != nil {
		t.Fatalf("nodes for capability: %v", err)
	}
	for _, id := range nodeIDs {
		if id == node.ID {
			t.Fatal("draining node is still a dispatch candidate")
		}
	}
}

// TestDrainUnknownNode 排空不存在的节点 → 404。
func TestDrainUnknownNode(t *testing.T) {
	env := newConsoleEnv(t)
	rr := env.do(t, http.MethodPost, "/v1/nodes/nd_doesnotexist/drain", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// TestMethodNotAllowed 各端点对不支持的方法回 405 + Allow 头。
//
// 注意 Allow 头由 Go 的 ServeMux 自动生成（带 HEAD，顺序按字典序），
// 所以只断言「包含期望的方法」而不是比较整个字符串——
// 手动设置 Allow 反而会和 ServeMux 的输出打架。
func TestMethodNotAllowed(t *testing.T) {
	env := newConsoleEnv(t)
	cases := []struct {
		method string
		path   string
		want   []string
	}{
		{http.MethodPost, "/v1/tasks", []string{"GET"}},
		{http.MethodPost, "/v1/nodes", []string{"GET"}},
		{http.MethodPut, "/v1/capabilities", []string{"GET", "POST"}},
		{http.MethodPut, "/v1/edges", []string{"GET", "POST", "DELETE"}},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rr := env.do(t, c.method, c.path, nil)
			if rr.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", rr.Code)
			}
			allow := rr.Header().Get("Allow")
			for _, m := range c.want {
				if !containsToken(allow, m) {
					t.Fatalf("Allow = %q, missing %s", allow, m)
				}
			}
		})
	}
}

// containsToken 判定逗号分隔的 Allow 头里是否有某个方法。
func containsToken(header, want string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

// TestLegacyNotificationEndpointsRemoved 验证旧通知终端模型端点已下线（#80）。
//
// 这些端点（/v1/notifications、/v1/stream、/v1/notify）连同 broker 包
// 一起删除：事件中心里"发生了什么"是 events、"做完没有"是 tasks，
// 不再有"给某人推过的消息"这个实体。
//
// 断言用真实 mux（不是手搭的子 mux），否则测不到路由注册的真实状态。
func TestLegacyNotificationEndpointsRemoved(t *testing.T) {
	env := newConsoleEnv(t)

	legacy := []struct{ method, path string }{
		{http.MethodGet, "/v1/notifications"},
		{http.MethodGet, "/v1/notifications/ntf_whatever"},
		{http.MethodPost, "/v1/notify"},
		{http.MethodPost, "/v1/test-notify"},
		{http.MethodGet, "/v1/stream"},
	}
	for _, tc := range legacy {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.AddCookie(env.cookie)
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, req)

		// 404 = 路由不存在。401/403 也算下线（没注册就会落到兜底），
		// 但绝不能是 200——那说明旧端点还活着。
		if rr.Code == http.StatusOK {
			t.Errorf("%s %s 仍返回 200，旧端点未下线", tc.method, tc.path)
		}
	}

	// 新的事件中心端点仍在
	for _, path := range []string{"/v1/console/events", "/v1/tasks", "/v1/capabilities"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(env.cookie)
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200（新端点应正常工作）", path, rr.Code)
		}
	}
}

// ---------- #142：trace 视图 ----------

// TestTraceEndpoint GET /v1/traces/{id} 一次答完「这条链为什么这样」（#142 验收）。
//
// 这是「剪枝留痕」的**查询出口**：库里写了 source='route' 的事件，
// 但如果没人能按 trace 把它拿出来，等于还是查不到。
func TestTraceEndpoint(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()
	capDef := env.seedCapability(t, "review", 1, []string{"fix"})

	const traceID = "trace_prune_demo"

	// 根事件：外部世界发生了什么
	if _, err := env.db.InsertEvent(ctx, &store.Event{
		OrgID: env.orgID, TraceID: traceID, Source: "github",
		Type: "pull_request.opened", Payload: json.RawMessage(`{"number":1}`),
	}); err != nil {
		t.Fatalf("insert root event: %v", err)
	}
	// 剪枝事件：哪条分支没跑、为什么（source='route' 是 #142 的约定）
	if _, err := env.db.InsertPruneEvent(ctx, &store.Event{
		OrgID: env.orgID, TraceID: traceID,
		Source: route.PruneEventSource,
		Type:   route.PruneEventType(route.ReasonNoEdge),
		Payload: json.RawMessage(
			`{"fromCapability":"github","port":"pull_request.review_requested",` +
				`"detail":"no edge from github port \"pull_request.review_requested\""}`),
	}); err != nil {
		t.Fatalf("insert prune event: %v", err)
	}
	// 任务：跑起来了的那条分支
	task := &store.Task{
		OrgID: env.orgID, TraceID: traceID, CapabilityID: capDef.ID,
		Capability: capDef.Name, Version: 1, Params: json.RawMessage(`{"number":1}`),
		CapHash: capDef.ContentHash, GraphID: "grf_x",
	}
	if err := env.db.CreateTasks(ctx, []*store.Task{task}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	rr := env.do(t, http.MethodGet, "/v1/traces/"+traceID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	if m["traceId"] != traceID {
		t.Errorf("traceId = %v, want %q", m["traceId"], traceID)
	}

	events, _ := m["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events = %d 条, want 2（根事件 + 剪枝）", len(events))
	}
	var sawRoute bool
	for _, raw := range events {
		if ev, _ := raw.(map[string]any); ev["source"] == route.PruneEventSource {
			sawRoute = true
		}
	}
	if !sawRoute {
		t.Error("events 里没有 source=route 的剪枝行（trace 视图必须含剪枝）")
	}

	tasks, _ := m["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d 条, want 1", len(tasks))
	}

	pruned, _ := m["pruned"].([]any)
	if len(pruned) != 1 {
		t.Fatalf("pruned = %d 条, want 1; body=%s", len(pruned), rr.Body.String())
	}
	p, _ := pruned[0].(map[string]any)
	if p["reason"] != "no_edge" {
		t.Errorf("pruned[0].reason = %v, want no_edge（type 去掉 pruned. 前缀）", p["reason"])
	}
	if p["fromCapability"] != "github" {
		t.Errorf("pruned[0].fromCapability = %v, want github", p["fromCapability"])
	}
	if p["port"] != "pull_request.review_requested" {
		t.Errorf("pruned[0].port = %v, want pull_request.review_requested", p["port"])
	}
	if s, _ := p["detail"].(string); s == "" {
		t.Error("pruned[0].detail 为空（剪枝必须带证据）")
	}
	if createdAt, _ := p["createdAt"].(float64); createdAt == 0 {
		t.Error("pruned[0].createdAt = 0（要能回答「什么时候剪的」）")
	}

	// 不存在的 trace → 404（events 与 tasks 都空）
	rr = env.do(t, http.MethodGet, "/v1/traces/trace_does_not_exist", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知 trace status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if m := decode(t, rr); m["error"] != "trace not found" {
		t.Fatalf("404 body = %v, want trace not found", m["error"])
	}

	// 无会话 → 401（与其它控制台端点同一套鉴权）
	rr = env.doNoCookie(t, http.MethodGet, "/v1/traces/"+traceID)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("无会话 status = %d, want 401", rr.Code)
	}
}

// TestConsoleEventsTraceViewRootFirst trace 视图的第一条事件必须是根事件（#142 契约增补）。
//
// 为什么断言「第一条」：trace 页拿 `evs[0]` 当根事件，用它的 source 去
// /v1/sources 里找源配置、拿源拓扑当底图。剪枝行（source='route'）时间上
// 一定晚于根事件 → 倒序排列会顶到第一条 → 页面拿到 source="route"、
// 找不到源，底图丢失，被剪枝的分支整片消失。
// 所以这不是「顺序好看」，而是页面能不能工作的硬契约。
func TestConsoleEventsTraceViewRootFirst(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	const traceID = "trace_root_first"

	// 根事件：外部世界发生了什么（时间更早）
	if _, err := env.db.InsertEvent(ctx, &store.Event{
		OrgID: env.orgID, TraceID: traceID, Source: "github",
		Type: "pull_request.opened", Payload: json.RawMessage(`{"number":1}`),
		CreatedAt: 1700000000,
	}); err != nil {
		t.Fatalf("insert root event: %v", err)
	}
	// 剪枝事件：时间更晚，DESC 排序下本来会排第一
	if _, err := env.db.InsertPruneEvent(ctx, &store.Event{
		OrgID: env.orgID, TraceID: traceID,
		Source: route.PruneEventSource,
		Type:   route.PruneEventType(route.ReasonNoEdge),
		Payload: json.RawMessage(
			`{"fromCapability":"github","port":"x","detail":"no edge"}`),
		CreatedAt: 1700000010,
	}); err != nil {
		t.Fatalf("insert prune event: %v", err)
	}

	rr := env.do(t, http.MethodGet, "/v1/console/events?traceId="+traceID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	events, _ := m["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events = %d 条, want 2（根事件 + 剪枝）", len(events))
	}
	first, _ := events[0].(map[string]any)
	if first["source"] != "github" {
		t.Fatalf("events[0].source = %v, want github（trace 页的 evs[0] 必须是根事件，"+
			"否则它找不到源、底图丢失）", first["source"])
	}
	second, _ := events[1].(map[string]any)
	if second["source"] != route.PruneEventSource {
		t.Fatalf("events[1].source = %v, want %q（剪枝仍要返回，不能被藏起来）",
			second["source"], route.PruneEventSource)
	}

	// 事件流（不带 traceId）不混剪枝——trace 视图的排序改动不能顺手把它带坏
	rr = env.do(t, http.MethodGet, "/v1/console/events", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("事件流 status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	m = decode(t, rr)
	stream, _ := m["events"].([]any)
	if len(stream) == 0 {
		t.Fatal("事件流为空，want 至少 1 条（根事件）")
	}
	for _, raw := range stream {
		if ev, _ := raw.(map[string]any); ev["source"] == route.PruneEventSource {
			t.Fatalf("事件流里出现了剪枝行: %v", ev)
		}
	}
}

// TestTraceEndpointCrossOrg trace 视图不能泄漏别的组织的数据（I10，#142 §9.4）。
//
// 场景是真实存在的：一个用户同时属于 org1（主组织）和 org2，
// trace_other_org 只存在于 org2。用 org1 去查必须 404（不区分「不存在」与
// 「不是你的」，避免探测别人的 traceId）；用 org2 去查必须 200 且内容正确
// —— 后半段是必须的，否则一个「永远 404」的实现也能骗过前半段。
func TestTraceEndpointCrossOrg(t *testing.T) {
	env := newConsoleEnv(t)
	ctx := context.Background()

	// 第二个组织（同一个用户是它的 owner：成员身份校验能过，租户隔离才被测到）
	org2, err := env.db.CreateOrg(ctx, "org-second-"+store.NewOrgID())
	if err != nil {
		t.Fatalf("create org2: %v", err)
	}
	if err := env.db.AddOrgMember(ctx, org2.ID, env.userID, store.OrgRoleOwner); err != nil {
		t.Fatalf("add org2 member: %v", err)
	}

	const otherTrace = "trace_other_org"
	if _, err := env.db.InsertEvent(ctx, &store.Event{
		OrgID: org2.ID, TraceID: otherTrace, Source: "github",
		Type: "pull_request.opened", Payload: json.RawMessage(`{"number":2}`),
	}); err != nil {
		t.Fatalf("insert org2 event: %v", err)
	}

	// 用 org1 查 org2 的 trace → 404（不泄漏存在性）
	rr := env.do(t, http.MethodGet, "/v1/traces/"+otherTrace+"?orgId="+env.orgID, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("跨组织查询 status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}

	// 用 org2 查同一条 trace → 200 且是同一串数据
	// （证明前面那个 404 是租户隔离，不是路由没注册/取数写错）
	rr = env.do(t, http.MethodGet, "/v1/traces/"+otherTrace+"?orgId="+org2.ID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("同组织查询 status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	m := decode(t, rr)
	if m["traceId"] != otherTrace {
		t.Fatalf("traceId = %v, want %q", m["traceId"], otherTrace)
	}
	events, _ := m["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("org2 的 events = %d 条, want 1", len(events))
	}
	if ev, _ := events[0].(map[string]any); ev["orgId"] != org2.ID {
		t.Fatalf("返回的 event.orgId = %v, want %q（数据必须来自 org2）", ev["orgId"], org2.ID)
	}
}
