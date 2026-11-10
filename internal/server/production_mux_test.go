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
