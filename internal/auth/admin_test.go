package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

// TestAdminMiddleware 验证超管中间件的三条路径：
// - admin 用户 → 200 放行
// - member 用户 → 403
// - disabled 用户 → 401
func TestAdminMiddleware(t *testing.T) {
	svc := newTestService(t)

	// 三个用户
	admin := &store.User{ID: store.NewUserID(), Username: "admin", Role: store.RoleAdmin, CreatedAt: store.Now()}
	member := &store.User{ID: store.NewUserID(), Username: "member", CreatedAt: store.Now()}
	disabled := &store.User{ID: store.NewUserID(), Username: "ghost", Role: store.RoleAdmin, Disabled: true, CreatedAt: store.Now()}
	for _, u := range []*store.User{admin, member, disabled} {
		if err := svc.db.CreateUser(u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	mw := svc.AdminMiddleware(next)

	cases := []struct {
		name     string
		uid      string
		wantCode int
		wantCall bool
	}{
		{"admin 放行", admin.ID, 200, true},
		{"member 拒绝", member.ID, 403, false},
		{"disabled 拒绝", disabled.ID, 401, false},
		{"未登录拒绝", "", 401, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest("GET", "/v1/admin/stats", nil)
			if c.uid != "" {
				req = req.WithContext(withUserID(req.Context(), c.uid))
			}
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			if rec.Code != c.wantCode {
				t.Errorf("status: got %d want %d", rec.Code, c.wantCode)
			}
			if called != c.wantCall {
				t.Errorf("next 调用: got %v want %v", called, c.wantCall)
			}
		})
	}
}

// TestBeginLogin_Disabled 验证已禁用用户无法发起登录。
func TestBeginLogin_Disabled(t *testing.T) {
	svc := newTestService(t)
	u := &store.User{ID: store.NewUserID(), Username: "ghost", Disabled: true, CreatedAt: store.Now()}
	if err := svc.db.CreateUser(u); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := svc.BeginLogin("ghost")
	if err == nil {
		t.Errorf("禁用用户应无法 BeginLogin")
	}
}
