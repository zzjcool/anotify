package server

// 生产 mux 整机冒烟（m10）。
//
// 为什么需要这一条：资源域的每个测试文件都只注册自己那几个路由；真实部署里
// NewApp 会一次装好 6 个资源域 + 保留域 + 节点协议 + admin/devices/keys + 静态兜底。
// 「某个 register 漏挂 / 被另一个 pattern 遮住 / 启动期依赖缺失」这类事故，单域测试
// 永远发现不了——它们全部只在**真实装配**上才会红。
//
// 本用例不测业务语义（那在各域自己的文件里），只钉两件事：
//  1. 真实装配能起来（store/auth/hub/source/route/trigger/lease 一路接线成功）；
//  2. 关键保留域端点在真实 handler 链上返回 200（不是 404/500），且未登录仍是 401。
//
// 这是 m10 的「生产 mux 无整机测试」缺口：独立探针曾这样验过，但没有仓库内守护。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// newProductionAppForTest 用真实 NewApp 起一套装配（临时 DB / 无 agent 二进制 / 无 VAPID）。
//
// 三个「可选能力」都刻意不提供：
//   - AgentBin 指向不存在的文件 → loadAgentBinary 只 WARN（合法部署形态）；
//   - VAPID env 未设置 → push.LoadVAPID 只 WARN；
//   - Pprof 关闭。
//
// 这三条正是「不带可选配置也要能起来」的验收；把它们配齐再测反而掩盖启动期分支。
func newProductionAppForTest(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := FromEnv()
	cfg.DBPath = filepath.Join(dir, "app.db")
	cfg.AgentBin = filepath.Join(dir, "no-agent-binary")
	cfg.RPDisplay = "Anotify Test"
	cfg.RPID = "localhost"
	cfg.RPOrigin = "http://localhost"
	cfg.Pprof = false

	ctx, cancel := context.WithCancel(context.Background())
	app := NewApp(ctx, cfg)
	if app == nil || app.Handler == nil || app.DB == nil || app.AuthSvc == nil {
		cancel()
		t.Fatal("NewApp 返回的装配不完整")
	}
	t.Cleanup(func() {
		cancel()
		_ = app.DB.Close()
	})
	return app
}

// appDo 发一次请求（可选会话 cookie）。
func appDo(t *testing.T, app *App, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	app.Handler.ServeHTTP(rr, req)
	return rr
}

// TestProductionMuxBootsAndServesRetainedDomains 是整机门禁。
func TestProductionMuxBootsAndServesRetainedDomains(t *testing.T) {
	app := newProductionAppForTest(t)

	// ① 健康检查：真实装配的第一条机器判据（含 boot 标识，#215）。
	rr := appDo(t, app, http.MethodGet, "/health", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	var health map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &health); err != nil || health["ok"] != true {
		t.Fatalf("/health body = %s, want {\"ok\":true,...}", rr.Body.String())
	}

	// ② 匿名端点：VAPID 公钥可读（前端订阅用），但没有任何「默认放开」的会话域。
	if rr := appDo(t, app, http.MethodGet, "/v1/vapid-public-key", nil); rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/vapid-public-key = %d, want 200（body=%s）", rr.Code, rr.Body.String())
	}
	for _, path := range []string{"/v1/auth/me", "/v1/namespaces", "/v1/nodes", "/v1/stats?namespace=default"} {
		if rr := appDo(t, app, http.MethodGet, path, nil); rr.Code != http.StatusUnauthorized {
			t.Fatalf("匿名 GET %s = %d, want 401", path, rr.Code)
		}
	}

	// ③ 建一个真实用户（注册即建默认命名空间）+ 会话：后面的 200 断言都走生产中间件。
	ctx := context.Background()
	user := &store.User{
		ID: store.NewUserID(), Username: "mux-boot", DisplayName: "整机冒烟",
		Role: store.RoleAdmin, CreatedAt: store.Now(),
	}
	if _, err := app.DB.RegisterUser(ctx, user); err != nil {
		t.Fatalf("register user: %v", err)
	}
	sess, err := app.AuthSvc.Sessions().Create(user.ID, "production mux smoke")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID}

	// ④ 关键保留域（§8.3/§8.4/§8.5）在真实 mux 上都必须是 200——
	// 这一串是 review 抓过的「被顺手删/改名」清单，整机层再钉一次。
	for _, tc := range []struct {
		path string
		kind string // 非空时断言响应 kind（资源列表/信封）
	}{
		{"/v1/auth/me", ""},
		{"/v1/namespaces", "NamespaceList"},
		{"/v1/namespaces/default/capabilities", "CapabilityList"},
		{"/v1/namespaces/default/sources", "SourceList"},
		{"/v1/namespaces/default/playbooks", "PlaybookList"},
		{"/v1/namespaces/default/tasks", "TaskList"},
		{"/v1/namespaces/default/events", "EventList"},
		{"/v1/namespaces/default/edges", ""},
		{"/v1/namespaces/default/deployments", ""},
		{"/v1/console/events?namespace=default", "EventList"},
		{"/v1/nodes", "NodeList"},
		{"/v1/system/config", ""},
		{"/v1/stats?namespace=default", ""},
		{"/v1/orphans?namespace=default", ""},
		{"/v1/devices", ""},
		{"/v1/keys", ""},
		{"/v1/admin/users", ""},
		{"/v1/capability-templates", ""},
	} {
		rr := appDo(t, app, http.MethodGet, tc.path, cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200（body=%s）", tc.path, rr.Code, rr.Body.String())
		}
		if tc.kind != "" {
			var doc map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
				t.Fatalf("GET %s 响应不是 JSON: %v（%s）", tc.path, err, rr.Body.String())
			}
			if got := doc["kind"]; got != tc.kind {
				t.Fatalf("GET %s kind = %v, want %s", tc.path, got, tc.kind)
			}
		}
	}

	// ⑤ 后台循环是真实启动的：给它们一个极短的窗口，确认没有 panic / 立即退出。
	// （循环只在 ctx 结束时退出；这里不做时序断言，只让"启动即崩"有机会暴露。）
	time.Sleep(50 * time.Millisecond)
}

// TestProductionMuxRetiredWritesAre405 钉住「退役的写/删在**真实装配**上返回 405」。
//
// 为什么必须整机测：deployments / edges 只注册了 `GET …`。生产 mux 末尾还有
// `mux.Handle("/", resolveStatic(cfg))` 兜底，未注册方法会落到静态处理器——测试构建 503
// （static.go 的「未构建」分支）、真实构建 404（静态文件找不到）。两种都不是 405，客户端
// 无法区分「这个资源没有这个方法」与「这个端点/文件不存在」，错误提示也因此误导。
// domain 层的裸 mux（只有该域的 register）根本看不到这个差异：那里 ServeMux 会自动回 405。
// 只有真实装配能把「显式方法兜底」钉住。
//
// 同一用例还钉三件事：
//  1. 405 之后**一条写也没落库**（deployments 行数前后一致）；
//  2. 读模型没被兜底遮住（GET 仍 200 且形状对，HEAD 命中 GET pattern 也是 200）；
//  3. 兜底仍在**会话内侧**（匿名写请求先吃 401，不是 405）。
func TestProductionMuxRetiredWritesAre405(t *testing.T) {
	app := newProductionAppForTest(t)
	ctx := context.Background()

	// 真会话：注册即建默认命名空间（路径用 default，与上面的整机冒烟用例同口径）。
	user := &store.User{
		ID: store.NewUserID(), Username: "mux-retired-writes", DisplayName: "退役写兜底",
		Role: store.RoleAdmin, CreatedAt: store.Now(),
	}
	if _, err := app.DB.RegisterUser(ctx, user); err != nil {
		t.Fatalf("register user: %v", err)
	}
	sess, err := app.AuthSvc.Sessions().Create(user.ID, "production mux retired writes")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	cookie := &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID}

	countDeployments := func() int {
		t.Helper()
		var n int
		if err := app.DB.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&n); err != nil {
			t.Fatalf("count deployments: %v", err)
		}
		return n
	}

	// edges 没有表（它是现算的派生投影，A5），deployments 有表——两者是同一条「只保留
	// GET」的判据，但「0 写」只能落在 deployments 上。
	paths := []string{
		"/v1/namespaces/default/deployments",
		"/v1/namespaces/default/edges",
	}
	retiredMethods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

	before := countDeployments()
	for _, path := range paths {
		for _, method := range retiredMethods {
			rr := appDo(t, app, method, path, cookie)
			// 405 必须是 Status 错误体（§4.1），reason 取封闭集合里的 MethodNotAllowed。
			// 503/404 在这里会立刻红——那正是本轮要修的整机行为。
			trReason(t, rr, http.StatusMethodNotAllowed, reasonMethodNotAllowed)
			if got := rr.Header().Get("Allow"); !strings.Contains(got, http.MethodGet) {
				t.Fatalf("%s %s 的 Allow = %q, want 含 GET（客户端据此知道剩下的合法方法）", method, path, got)
			}
		}
	}
	if after := countDeployments(); after != before {
		t.Fatalf("退役的写/删落了库：deployments %d → %d（必须 0 写）", before, after)
	} else {
		// 显式打出来：这条断言的价值就在于「跑了 8 个写/删请求，表里一行没多」。
		t.Logf("deployments 行数：%d → %d（POST/PUT/PATCH/DELETE × 2 路径后仍 0 写）", before, after)
	}

	// 读模型没被方法兜底遮住：`GET …` 比不带方法的 pattern 更具体，GET/HEAD 仍命中读端点。
	for _, tc := range []struct{ path, key string }{
		{"/v1/namespaces/default/deployments", "deployments"},
		{"/v1/namespaces/default/edges", "items"},
	} {
		rr := appDo(t, app, http.MethodGet, tc.path, cookie)
		doc := trWant(t, rr, http.StatusOK)
		if _, ok := doc[tc.key]; !ok {
			t.Fatalf("GET %s 缺 %q 键（body=%s）", tc.path, tc.key, rr.Body.String())
		}
		if rr := appDo(t, app, http.MethodHead, tc.path, cookie); rr.Code != http.StatusOK {
			t.Fatalf("HEAD %s = %d, want 200（ServeMux 的 GET pattern 也匹配 HEAD）", tc.path, rr.Code)
		}
	}

	// 兜底在会话内侧：405 不能抢在鉴权之前——否则匿名客户端能从「405 vs 401」探出
	// 哪些路径存在。
	for _, path := range paths {
		for _, method := range retiredMethods {
			if rr := appDo(t, app, method, path, nil); rr.Code != http.StatusUnauthorized {
				t.Fatalf("匿名 %s %s = %d, want 401（方法兜底必须仍在 session 内侧）", method, path, rr.Code)
			}
		}
	}
}
