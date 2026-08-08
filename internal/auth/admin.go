package auth

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/anotify/anotify/internal/store"
)

// ctxRole 是注入 context 的 role 键（私有，避免与 ctxUserID 冲突）。
type ctxRole string

const ctxRoleKey ctxRole = "anotify.role"

// AdminMiddleware 校验当前登录用户是否为超管（role=admin）。
// 必须套在 SessionMiddleware 之后（依赖 context 里的 userID）。
// 非 admin 返回 403。已禁用用户返回 401（视同未登录）。
func (s *Service) AdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := UserIDFromContext(r.Context())
		if uid == "" {
			http.Error(w, `{"error":"未登录"}`, http.StatusUnauthorized)
			return
		}
		u, err := s.db.GetUserByID(uid)
		if err != nil {
			slog.Warn("admin middleware: user not found",
				"event", "auth.admin.user_missing",
				"user_id", uid,
			)
			http.Error(w, `{"error":"用户不存在"}`, http.StatusUnauthorized)
			return
		}
		if u.Disabled {
			slog.Warn("admin middleware: user disabled",
				"event", "auth.admin.disabled",
				"user_id", uid,
			)
			http.Error(w, `{"error":"账户已被禁用"}`, http.StatusUnauthorized)
			return
		}
		if u.Role != store.RoleAdmin {
			slog.Warn("admin middleware: forbidden",
				"event", "auth.admin.forbidden",
				"user_id", uid,
				"role", u.Role,
			)
			http.Error(w, `{"error":"需要管理员权限"}`, http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), ctxRoleKey, u.Role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
