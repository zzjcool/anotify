package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPprof_NotOnPublicMux(t *testing.T) {
	app := newProductionAppForTest(t, func(cfg *Config) { cfg.Pprof = true })

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/profile", "/debug/pprof/heap"} {
		t.Run("业务 mux 不暴露 "+path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			app.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusServiceUnavailable {
				t.Errorf("pprof 开关打开时 %s 应落到静态兜底（404/503），得到 %d —— 说明它又被挂回业务 mux 了", path, rec.Code)
			}
			if body := rec.Body.String(); strings.Contains(body, "/debug/pprof/") || strings.Contains(body, "Types of profiles available") {
				t.Errorf("pprof 开关打开时 %s 的响应体里出现了 pprof 索引内容 —— 已暴露到 ANOTIFY_ADDR", path)
			}
		})
	}

	t.Run("独立 pprof mux 提供索引", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NewPprofMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("NewPprofMux 的 /debug/pprof/ 必须返回 200，得到 %d", rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "Types of profiles available") {
			t.Errorf("NewPprofMux 的 /debug/pprof/ 响应不是 pprof 索引页（缺标题），body: %.120q", body)
		}
	})
}
