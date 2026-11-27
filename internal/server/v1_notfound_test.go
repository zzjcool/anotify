package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestUnregisteredV1RouteIsJSONStatus 钉住 #576 的另一半：站点 404 页**不得漏进 /v1**。
//
// 为什么要有这条：`mux.Handle("/", resolveStatic(cfg))` 是最后一道兜底，/v1 下未注册的
// 路径/方法以前落到它身上 —— 先是标准库纯文本 `404 page not found`，站点 404 页上线后
// 会变成一整页 **HTML**。按 spec 生成的客户端两种都解析不了，而同一个 mux 上已注册端点
// 的 404 全是 Status 信封（#600 记的就是这条口径）。
//
// 断言的是「形状」而不是某一版文案：状态 404/405、Content-Type 是 JSON、body 是 Status
// 信封（kind=Status、reason 非空）、带 no-store。方法口径（错方法 → 405 + Allow）留给
// #600，所以这里对 405 也放行 —— 但**绝不允许** text/plain 或 text/html。
func TestUnregisteredV1RouteIsJSONStatus(t *testing.T) {
	app := newProductionAppForTest(t)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"未注册的 GET 路径", http.MethodGet, "/v1/nonexistent"},
		{"未注册的深层路径", http.MethodGet, "/v1/namespaces/default/nope"},
		{"已退役端点上的 POST", http.MethodPost, "/v1/orphans"},
		{"已有资源路径上的未注册方法", http.MethodPut, "/v1/namespaces/default/capabilities"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := appDo(t, app, c.method, c.path, nil)
			if rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s = %d want 404/405（body=%s）", c.method, c.path, rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("%s %s 的 Content-Type=%q want application/json（站点 404 页漏进了 API 空间）",
					c.method, c.path, ct)
			}
			var doc struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
				Status     string `json:"status"`
				Code       int    `json:"code"`
				Reason     string `json:"reason"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
				t.Fatalf("%s %s body 不是 JSON Status: %v（body=%s）", c.method, c.path, err, rr.Body.String())
			}
			if doc.Kind != "Status" || doc.Status != "Failure" || doc.Reason == "" {
				t.Fatalf("%s %s body = %+v，want kind=Status/status=Failure/非空 reason", c.method, c.path, doc)
			}
			if doc.Code != rr.Code {
				t.Errorf("%s %s 的 status.code=%d 与 HTTP 码 %d 不一致", c.method, c.path, doc.Code, rr.Code)
			}
			if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
				t.Errorf("%s %s 的 Cache-Control=%q want no-store", c.method, c.path, cc)
			}
		})
	}

	// 反向判据：/v1/ 前缀的兜底不能遮蔽已注册的端点。
	t.Run("已注册端点仍走自己的 handler", func(t *testing.T) {
		rr := appDo(t, app, http.MethodGet, "/health", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /health = %d want 200（/v1/ 兜底遮蔽了别的路由？body=%s）", rr.Code, rr.Body.String())
		}
		// 未登录读 /v1/namespaces 是 401（会话中间件的答案），不是 404。
		rr = appDo(t, app, http.MethodGet, "/v1/namespaces", nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("GET /v1/namespaces 未登录 = %d want 401（body=%s）", rr.Code, rr.Body.String())
		}
	})
}
