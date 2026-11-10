package server

// 保留域读模型（api-conventions §8.4 / §8.5）的 HTTP 验收：
//
//	GET /v1/stats?namespace=…
//	GET /v1/orphans?namespace=…[&olderThan=&limit=]
//	GET /v1/namespaces/{ns}/deployments[?nodeId=&capabilityId=]
//
// 这三条端点是**冻结保留**的现有读模型（不是 v3 扁平资源面）：一次 review 曾把它们
// 随 v3 housekeeping 一起删掉/改名，等于删掉背压唯一可观测出口和设计明确「不顺手下线」
// 的读模型。本文件的用例钉住三件事：
//  1. 路由真的在（200，不是 404），且形状沿用 v3（只统一错误体）；
//  2. cluster 路径**必须**显式带 `?namespace=`：缺参 400、不存在 404，绝不跨 ns 默认全量；
//  3. 逐 ns 授权 + 跨 ns 隔离：A 的数据不会出现在 B 的结果里。
//
// 夹具复用 taskResEnv（真库 + 真用户 + 真会话注入 + 两个命名空间）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// newRetainedReadMux 在 taskResEnv 的依赖上挂保留域路由。
//
// 不复用 newITEnv：这里的验收点是「读模型语义 + 作用域收口」，不需要六个 domain 同时装配
// （跨 domain 的 pattern 冲突由 api_integration_test.go 的整机路由表负责）。
func newRetainedReadMux(t *testing.T) (*taskResEnv, *http.ServeMux) {
	t.Helper()
	base := newTaskResEnv(t)
	mux := http.NewServeMux()
	newRetainedReadAPI(base.deps).register(mux)
	return base, mux
}

// retainedDo 走保留域 mux 发一次 GET（会话身份已由夹具的 Session 注入）。
func retainedDo(t *testing.T, mux http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

// nsSlugByID 从真库读 slug（路径与 ?namespace= 都接受 slug，这里用实测值）。
func nsSlugByID(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	list, err := db.ListNamespaces(context.Background())
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	for _, ns := range list {
		if ns.ID == id {
			return ns.Slug
		}
	}
	t.Fatalf("namespace %s 不在库里", id)
	return ""
}

// TestRetainedStatsRequiresNamespaceAndScopes 钉住 /v1/stats 的作用域口径：
// 缺 namespace 400（不是默认 ns、更不是全量）、`*` 400、逐 ns 计数、slug/id 都能解析。
func TestRetainedStatsRequiresNamespaceAndScopes(t *testing.T) {
	env, mux := newRetainedReadMux(t)
	slugA := nsSlugByID(t, env.db, env.ns)
	slugB := nsSlugByID(t, env.db, env.ns2)
	base := store.Now() - 100

	// 两个命名空间各落真实事件：A 2 条、B 1 条。
	env.seedEvent(env.ns, "trace_stats", "github", "push", `{"n":1}`, "", base)
	env.seedEvent(env.ns, "trace_stats", "github", "push", `{"n":2}`, "", base+1)
	env.seedEvent(env.ns2, "trace_stats_b", "github", "push", `{"n":3}`, "", base+2)

	// 缺 namespace → 400（统一 Status 体；不回退默认命名空间）。
	rr := retainedDo(t, mux, "/v1/stats")
	trReason(t, rr, http.StatusBadRequest, reasonBadRequest)

	// `*` 在这条端点是 400：store.MessageStats 没有「跨 ns 相加」的口径（会造出不存在的数字）。
	rr = retainedDo(t, mux, "/v1/stats?namespace=*")
	trReason(t, rr, http.StatusBadRequest, reasonBadRequest)

	// 不存在的 ns → 404（存在性判断在同一个收口里）。
	rr = retainedDo(t, mux, "/v1/stats?namespace=no-such-ns")
	trReason(t, rr, http.StatusNotFound, reasonNotFound)

	// slug：A 的真实计数。
	doc := trWant(t, retainedDo(t, mux, "/v1/stats?namespace="+slugA), http.StatusOK)
	if got := trNumber(t, doc, "total"); got != 2 {
		t.Fatalf("stats.total = %v, want 2（只算 ns A 的事件）", got)
	}
	if got := trNumber(t, doc, "today"); got != 2 {
		t.Fatalf("stats.today = %v, want 2", got)
	}
	// 空集合必须是 []：daily 是热力图数据，null 会让前端把它当成「没数据/未连接」。
	trArray(t, doc, "daily")

	// id 写法等价（cluster 读模型的 ns 解析与资源路径同一份 slug-or-id 判据）。
	doc = trWant(t, retainedDo(t, mux, "/v1/stats?namespace="+env.ns2), http.StatusOK)
	if got := trNumber(t, doc, "total"); got != 1 {
		t.Fatalf("按 id 取 B 的 stats.total = %v, want 1", got)
	}

	// B 的 slug 同样可解析，且与上面按 id 的结果一致。
	doc = trWant(t, retainedDo(t, mux, "/v1/stats?namespace="+slugB), http.StatusOK)
	if got := trNumber(t, doc, "total"); got != 1 {
		t.Fatalf("按 slug 取 B 的 stats.total = %v, want 1", got)
	}
}

// TestRetainedOrphansRequiresNamespaceAndPerNamespaceAuth 钉住 /v1/orphans：
// 缺 namespace 400、`*` 逐 ns 汇总、跨 ns 隔离、olderThan/limit 的封闭参数域。
func TestRetainedOrphansRequiresNamespaceAndPerNamespaceAuth(t *testing.T) {
	env, mux := newRetainedReadMux(t)
	slugA := nsSlugByID(t, env.db, env.ns)
	slugB := nsSlugByID(t, env.db, env.ns2)
	// 两条「派不出去」的 pending（两个命名空间各一条，能力行都在、但没有任何在线节点
	// → no_online_node）。created_at 都在 2 小时前，过 olderThan=1h 门槛。
	old := store.Now() - 2*3600
	capB := env.seedCapability(env.ns2, "ghost-cap", 1, "sha256:ghostb")
	orphanA := env.insertTask(seededTask{createdAt: old})
	orphanB := env.insertTask(seededTask{
		ns: env.ns2, capID: capB, capName: "ghost-cap", version: 1, digest: "sha256:ghostb", createdAt: old,
	})

	// 缺 namespace → 400（不得跨 ns 默认全量）。
	rr := retainedDo(t, mux, "/v1/orphans")
	trReason(t, rr, http.StatusBadRequest, reasonBadRequest)

	// 单 ns：只看得到 A 的孤儿，形状沿用 v3（orphans/count/olderThan/namespace/allScopes/hint）。
	doc := trWant(t, retainedDo(t, mux, "/v1/orphans?namespace="+slugA), http.StatusOK)
	items := trArray(t, doc, "orphans")
	if len(items) != 1 {
		t.Fatalf("ns A orphans = %d, want 1（body=%v）", len(items), doc)
	}
	first := items[0].(map[string]any)
	if got := trString(t, first, "id"); got != orphanA {
		t.Fatalf("orphans[0].id = %q, want %q", got, orphanA)
	}
	if got := trString(t, first, "reason"); got != store.OrphanNoOnlineNode {
		t.Fatalf("orphans[0].reason = %q, want %q", got, store.OrphanNoOnlineNode)
	}
	if got := trNumber(t, doc, "count"); got != 1 {
		t.Fatalf("count = %v, want 1", got)
	}
	if got := trNumber(t, doc, "olderThan"); got != 3600 {
		t.Fatalf("olderThan 默认应为 3600，得到 %v", got)
	}
	if got := doc["allScopes"]; got != false {
		t.Fatalf("allScopes = %v, want false（单 ns）", got)
	}
	if got := trString(t, doc, "namespace"); got != env.ns {
		t.Fatalf("namespace = %q, want store id %q", got, env.ns)
	}
	if got := trString(t, doc, "hint"); !strings.Contains(got, "abandon") {
		t.Fatalf("hint 应指向 abandon 处置方式，得到 %q", got)
	}

	// 跨 ns 汇总：`*` 逐 ns 授权后合并（A + B 各一条）。
	all := trWant(t, retainedDo(t, mux, "/v1/orphans?namespace=*"), http.StatusOK)
	if got := trArray(t, all, "orphans"); len(got) != 2 {
		t.Fatalf("namespace=* orphans = %d, want 2", len(got))
	}
	if got := all["allScopes"]; got != true {
		t.Fatalf("allScopes = %v, want true", got)
	}
	ids := map[string]bool{}
	for _, item := range trArray(t, all, "orphans") {
		ids[trString(t, item.(map[string]any), "id")] = true
	}
	if !ids[orphanA] || !ids[orphanB] {
		t.Fatalf("namespace=* 应同时包含 A/B 的孤儿，得到 %v", ids)
	}

	// B 的单 ns 视图里不能出现 A 的孤儿（跨 ns 隔离）。
	docB := trWant(t, retainedDo(t, mux, "/v1/orphans?namespace="+slugB), http.StatusOK)
	itemsB := trArray(t, docB, "orphans")
	if len(itemsB) != 1 || trString(t, itemsB[0].(map[string]any), "id") != orphanB {
		t.Fatalf("ns B orphans = %v, want 仅 %s", itemsB, orphanB)
	}
	if got := trString(t, itemsB[0].(map[string]any), "reason"); got != store.OrphanNoOnlineNode {
		t.Fatalf("ns B orphan reason = %q, want %q", got, store.OrphanNoOnlineNode)
	}

	// 参数域封闭：olderThan 下限/坏值、limit 非正数都是调用方错误（不静默归一）。
	trReason(t, retainedDo(t, mux, "/v1/orphans?namespace="+slugA+"&olderThan=30s"),
		http.StatusBadRequest, reasonBadRequest)
	trReason(t, retainedDo(t, mux, "/v1/orphans?namespace="+slugA+"&olderThan=nope"),
		http.StatusBadRequest, reasonBadRequest)
	trReason(t, retainedDo(t, mux, "/v1/orphans?namespace="+slugA+"&limit=0"),
		http.StatusBadRequest, reasonBadRequest)
	trReason(t, retainedDo(t, mux, "/v1/orphans?namespace="+slugA+"&limit=abc"),
		http.StatusBadRequest, reasonBadRequest)

	// 不存在 ns → 404。
	trReason(t, retainedDo(t, mux, "/v1/orphans?namespace=no-such-ns"),
		http.StatusNotFound, reasonNotFound)
}

// TestRetainedDeploymentsListAndFilters 钉住 ns 路径的部署事实读模型：
// 200 / 空列表 `[]` / nodeId+capabilityId 过滤 / 跨 ns 隔离 / 未知 ns 404。
func TestRetainedDeploymentsListAndFilters(t *testing.T) {
	env, mux := newRetainedReadMux(t)
	slugA := nsSlugByID(t, env.db, env.ns)
	slugB := nsSlugByID(t, env.db, env.ns2)

	ctx := context.Background()
	node1 := &store.Node{Name: "deploy-node-1", MaxInstances: 2, MaxConcurrency: 1}
	if _, err := env.db.CreateNode(ctx, node1); err != nil {
		t.Fatalf("create node 1: %v", err)
	}
	node2 := &store.Node{Name: "deploy-node-2", MaxInstances: 2, MaxConcurrency: 1}
	if _, err := env.db.CreateNode(ctx, node2); err != nil {
		t.Fatalf("create node 2: %v", err)
	}
	n1, err := env.db.GetNodeByName(ctx, node1.Name)
	if err != nil {
		t.Fatalf("read node 1: %v", err)
	}
	n2, err := env.db.GetNodeByName(ctx, node2.Name)
	if err != nil {
		t.Fatalf("read node 2: %v", err)
	}
	capB := env.seedCapability(env.ns2, "review-b", 1, "sha256:deployb")

	// 直接落 deployments 事实（真行，走 schema 的外键）：A 两条、B 一条。
	seedDeployment := func(id, ns, nodeID, capID, digest string) {
		t.Helper()
		now := store.Now()
		if _, err := env.db.Exec(
			`INSERT INTO deployments (id, namespace_id, node_id, capability_id, digest, state, reason, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			id, ns, nodeID, capID, digest, "pending", "", now, now); err != nil {
			t.Fatalf("seed deployment %s: %v", id, err)
		}
	}
	seedDeployment("dep_a_n1", env.ns, n1.ID, env.cap, "sha256:taskres")
	seedDeployment("dep_a_n2", env.ns, n2.ID, env.cap, "sha256:taskres")
	seedDeployment("dep_b_n1", env.ns2, n1.ID, capB, "sha256:deployb")

	// ns A：两条部署都在，字段是 v3 的 camelCase 形状。
	doc := trWant(t, retainedDo(t, mux, "/v1/namespaces/"+slugA+"/deployments"), http.StatusOK)
	items := trArray(t, doc, "deployments")
	if len(items) != 2 {
		t.Fatalf("ns A deployments = %d, want 2（body=%v）", len(items), doc)
	}
	first := items[0].(map[string]any)
	for _, key := range []string{"id", "namespaceId", "nodeId", "capabilityId", "capability", "version", "digest", "state", "createdAt", "updatedAt"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("deployment 缺字段 %q: %v", key, first)
		}
	}

	// nodeId 过滤：只留 node2 的那条。
	doc = trWant(t, retainedDo(t, mux, "/v1/namespaces/"+slugA+"/deployments?nodeId="+n2.ID), http.StatusOK)
	if got := trArray(t, doc, "deployments"); len(got) != 1 || trString(t, got[0].(map[string]any), "id") != "dep_a_n2" {
		t.Fatalf("nodeId 过滤得到 %v, want [dep_a_n2]", got)
	}

	// capabilityId 过滤：同一能力的两条。
	doc = trWant(t, retainedDo(t, mux, "/v1/namespaces/"+slugA+"/deployments?capabilityId="+env.cap), http.StatusOK)
	if got := trArray(t, doc, "deployments"); len(got) != 2 {
		t.Fatalf("capabilityId 过滤得到 %d 条, want 2", len(got))
	}

	// 无匹配 → 空数组（不是 null）。
	rr := retainedDo(t, mux, "/v1/namespaces/"+slugA+"/deployments?nodeId=nd_missing")
	if !strings.Contains(rr.Body.String(), `"deployments":[]`) {
		t.Fatalf("无匹配时 deployments 必须是 []，得到 %s", rr.Body.String())
	}

	// 跨 ns 隔离：B 只看到自己的那条（namespaceId 也是 B）。
	docB := trWant(t, retainedDo(t, mux, "/v1/namespaces/"+slugB+"/deployments"), http.StatusOK)
	itemsB := trArray(t, docB, "deployments")
	if len(itemsB) != 1 || trString(t, itemsB[0].(map[string]any), "id") != "dep_b_n1" {
		t.Fatalf("ns B deployments = %v, want [dep_b_n1]", itemsB)
	}
	if got := trString(t, itemsB[0].(map[string]any), "namespaceId"); got != env.ns2 {
		t.Fatalf("deployment.namespaceId = %q, want %q", got, env.ns2)
	}

	// 未知 ns → 404（不泄露存在性差异）。
	rr = retainedDo(t, mux, "/v1/namespaces/no-such-ns/deployments")
	trReason(t, rr, http.StatusNotFound, reasonNotFound)

	// 写/删仍退役（§9：deployments 只保留 GET）：同一路径的 POST/DELETE 必须是 405，
	// 不能因为「读模型恢复」把 v3 的部署写入口一起带回来。
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/v1/namespaces/"+slugA+"/deployments", strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s deployments = %d, want 405（写/删已退役）", method, rec.Code)
		}
	}

	// 三条端点都过会话：用一个「永远 401」的中间件再挂一次，确认不是裸露的。
	noSession := &serverDeps{DB: env.db, Session: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, http.StatusUnauthorized, reasonUnauthorized, "login required", nil)
		})
	}}
	bare := http.NewServeMux()
	newRetainedReadAPI(noSession).register(bare)
	for _, path := range []string{
		"/v1/stats?namespace=" + slugA,
		"/v1/orphans?namespace=" + slugA,
		"/v1/namespaces/" + slugA + "/deployments",
	} {
		if rr := retainedDo(t, bare, path); rr.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s = %d, want 401（必须过 session 中间件）", path, rr.Code)
		}
	}
}

// TestRetainedEdgesProjectionExists 确认 `GET /v1/namespaces/{ns}/edges`（只读派生投影）
// 仍在：§9 明确 edges 只保留这条读路径（写/删已退役）。
func TestRetainedEdgesProjectionExists(t *testing.T) {
	env := newTaskResEnv(t)
	slugA := nsSlugByID(t, env.db, env.ns)

	// taskResEnv 只挂了 taskAPI；edges 属于 capability 域，单独挂它即可（真 store）。
	mux := http.NewServeMux()
	newCapabilityAPI(env.deps).register(mux)
	rr := retainedDo(t, mux, "/v1/namespaces/"+slugA+"/edges")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET edges = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	doc := trBody(t, rr)
	items := trArray(t, doc, "items")
	if len(items) != 0 {
		t.Fatalf("空图 edges 应为 items:[]，得到 %v", items)
	}

	// 写端点仍然是退役的（§9 明确移除）：POST/DELETE 不得偷偷回来。
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/namespaces/"+slugA+"/edges", strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("%s edges = %d, want 405/404（写端点已退役）", method, rec.Code)
		}
	}
}
