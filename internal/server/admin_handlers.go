package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// adminHandler 包装管理后台的 /v1/admin/* HTTP 端点。
// 所有端点要求登录会话（sessMW）+ 超管权限（AdminMiddleware），
// 由 mux 装配时统一套上（见 mux.go）。
type adminHandler struct {
	db  *store.DB
	svc *auth.Service
}

// adminUserOut 是管理后台用户列表的安全公开视图（不含密钥材料）。
type adminUserOut struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
	CreatedAt   int64  `json:"createdAt"`
	// #204 / #205：per-user 的 messageCount 随组织层一起删除 —— 组织没了之后
	// 「这个用户有多少事件」没有对应关系（事件属于命名空间，而用户与命名空间之间
	// 不再有归属关系）。留着只能造假数，所以字段整个去掉而不是填 0。
	DeviceCount  int64 `json:"deviceCount"`
	KeyCount     int64 `json:"keyCount"`
	SessionCount int64 `json:"sessionCount"`
}

func toAdminUserOut(u *store.UserWithStats) adminUserOut {
	return adminUserOut{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName,
		Role: u.Role, Disabled: u.Disabled, CreatedAt: u.CreatedAt,
		DeviceCount: u.DeviceCount,
		KeyCount:    u.KeyCount, SessionCount: u.SessionCount,
	}
}

// ServeHTTP 路由 /v1/admin/*。
// /v1/admin/users/{id} 子路径由 usersItem 处理（PATCH/DELETE）。
func (h *adminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/admin"), "/")

	// /v1/admin/users/{id} 与 /v1/admin/users/{id}/{action}
	if strings.HasPrefix(rest, "users/") {
		sub := strings.TrimPrefix(rest, "users/")
		parts := strings.SplitN(sub, "/", 2)
		id := parts[0]
		if id == "" {
			writeErr(w, 404, "unknown admin endpoint: "+rest)
			return
		}
		// /v1/admin/users/{id} 无子动作
		if len(parts) == 1 || parts[1] == "" {
			switch r.Method {
			case http.MethodGet:
				h.getUser(w, r, id)
			case http.MethodPatch:
				h.patchUser(w, r, id)
			default:
				writeErr(w, 405, "method not allowed")
			}
			return
		}
		// /v1/admin/users/{id}/{action}
		action := parts[1]
		switch {
		case action == "role" && r.Method == http.MethodPatch:
			h.changeRole(w, r, id)
		case action == "disable" && r.Method == http.MethodPost:
			h.setDisabled(w, r, id, true)
		case action == "enable" && r.Method == http.MethodPost:
			h.setDisabled(w, r, id, false)
		default:
			writeErr(w, 404, "unknown admin endpoint: "+rest)
		}
		return
	}

	switch rest {
	case "stats":
		h.stats(w, r)
	case "users":
		if r.Method == http.MethodGet {
			h.listUsers(w, r)
		} else {
			writeErr(w, 405, "method not allowed")
		}
	case "messages":
		if r.Method == http.MethodGet {
			h.listMessages(w, r)
		} else {
			writeErr(w, 405, "method not allowed")
		}
	case "sessions":
		if r.Method == http.MethodGet {
			h.listSessions(w, r)
		} else {
			writeErr(w, 405, "method not allowed")
		}
	case "":
		writeJSON(w, 200, map[string]any{
			"endpoints": []string{"/v1/admin/stats", "/v1/admin/users", "/v1/admin/messages", "/v1/admin/sessions"},
		})
	default:
		writeErr(w, 404, "unknown admin endpoint: "+rest)
	}
}

// GET /v1/admin/stats —— 系统总览统计。
func (h *adminHandler) stats(w http.ResponseWriter, r *http.Request) {
	// 热力图取近 371 天（53 周，与用户工作台一致）
	since := store.Now() - 371*86400
	s, err := h.db.SystemStats(r.Context(), since)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, s)
}

// GET /v1/admin/users —— 用户列表（附各用户实体计数）。
func (h *adminHandler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.db.ListUsersWithStats(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]adminUserOut, 0, len(users))
	for _, u := range users {
		out = append(out, toAdminUserOut(u))
	}
	writeJSON(w, 200, map[string]any{"users": out, "count": len(out)})
}

// GET /v1/admin/users/{id} —— 单用户详情（含实体计数）。
func (h *adminHandler) getUser(w http.ResponseWriter, r *http.Request, id string) {
	all, err := h.db.ListUsersWithStats(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, u := range all {
		if u.ID == id {
			writeJSON(w, 200, toAdminUserOut(u))
			return
		}
	}
	writeErr(w, 404, "用户不存在")
}

// PATCH /v1/admin/users/{id} —— 改用户角色或禁用状态（body: {role?, disabled?}）。
func (h *adminHandler) patchUser(w http.ResponseWriter, r *http.Request, id string) {
	uid := auth.UserIDFromContext(r.Context())
	var req struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	if req.Role != nil {
		if err := h.applyRoleChange(r, id, *req.Role, uid); err != nil {
			writeErr(w, err.code, err.msg)
			return
		}
	}
	if req.Disabled != nil {
		if err := h.applyDisabledChange(r, id, *req.Disabled, uid); err != nil {
			writeErr(w, err.code, err.msg)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// changeRole PATCH /v1/admin/users/{id}/role —— 改角色（body: {role}）。
func (h *adminHandler) changeRole(w http.ResponseWriter, r *http.Request, id string) {
	uid := auth.UserIDFromContext(r.Context())
	var req struct {
		Role string `json:"role"`
	}
	if err := readJSON(r, &req); err != nil || req.Role == "" {
		writeErr(w, 400, "缺少 role")
		return
	}
	if err := h.applyRoleChange(r, id, req.Role, uid); err != nil {
		writeErr(w, err.code, err.msg)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type adminErr struct {
	code int
	msg  string
}

func (e adminErr) Error() string { return e.msg }

// applyRoleChange 执行角色变更校验与持久化。
// - 角色必须是 admin/member
// - 不能改自己的角色（防误操作把自己降权锁死）
// - 降权最后一个 admin 前先确保还有其他 admin（防误删唯一超管）
func (h *adminHandler) applyRoleChange(r *http.Request, id, role string, actorID string) *adminErr {
	switch role {
	case store.RoleAdmin, store.RoleMember:
	default:
		return &adminErr{400, "role 仅支持 admin|member"}
	}
	if id == actorID {
		return &adminErr{409, "不能修改自己的角色"}
	}
	// 降权最后一个 admin 需拦截
	if role == store.RoleMember {
		target, err := h.db.GetUserByID(id)
		if err != nil {
			return &adminErr{404, "用户不存在"}
		}
		if target.Role == store.RoleAdmin {
			cnt, err := h.db.AdminCount(r.Context())
			if err != nil {
				return &adminErr{500, err.Error()}
			}
			if cnt <= 1 {
				return &adminErr{409, "至少保留一个超级管理员，无法降权最后一个 admin"}
			}
		}
	}
	n, err := h.db.UpdateUserRole(r.Context(), id, role)
	if err != nil {
		return &adminErr{500, err.Error()}
	}
	if n == 0 {
		return &adminErr{404, "用户不存在"}
	}
	slog.Info("admin: user role changed",
		"event", "admin.user.role_changed",
		"actor_id", actorID,
		"target_id", id,
		"role", role,
	)
	return nil
}

// setDisabled POST /v1/admin/users/{id}/disable|enable —— 禁用/启用用户。
func (h *adminHandler) setDisabled(w http.ResponseWriter, r *http.Request, id string, disabled bool) {
	uid := auth.UserIDFromContext(r.Context())
	if err := h.applyDisabledChange(r, id, disabled, uid); err != nil {
		writeErr(w, err.code, err.msg)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// applyDisabledChange 执行禁用/启用校验与持久化。
// - 不能禁用自己
// - 禁用最后一个 admin 需拦截（防误锁唯一超管）
func (h *adminHandler) applyDisabledChange(r *http.Request, id string, disabled bool, actorID string) *adminErr {
	if id == actorID {
		return &adminErr{409, "不能禁用自己的账户"}
	}
	if disabled {
		target, err := h.db.GetUserByID(id)
		if err != nil {
			return &adminErr{404, "用户不存在"}
		}
		if target.Role == store.RoleAdmin {
			cnt, err := h.db.AdminCount(r.Context())
			if err != nil {
				return &adminErr{500, err.Error()}
			}
			if cnt <= 1 {
				return &adminErr{409, "至少保留一个启用的超级管理员，无法禁用最后一个 admin"}
			}
		}
	}
	n, err := h.db.UpdateUserDisabled(r.Context(), id, disabled)
	if err != nil {
		return &adminErr{500, err.Error()}
	}
	if n == 0 {
		return &adminErr{404, "用户不存在"}
	}
	// 会话缓存失效（#241）：必须在 DB 写成功之后。
	// 不失效的话「禁用」要等一个 cacheTTL（默认 10s）才真正生效——虽然仍安全
	// （是有界的），但超管的预期是“点了就下”。反向（先失效后写库）更糟：写库失败时
	// 缓存已没，下一次请求重新缓存到的仍是「未禁用」，看起来像改了没生效。
	h.svc.Sessions().InvalidateUser(id)
	slog.Info("admin: user disabled changed",
		"event", "admin.user.disabled_changed",
		"actor_id", actorID,
		"target_id", id,
		"disabled", disabled,
	)
	return nil
}

// GET /v1/admin/messages —— 全站事件流（跨组织，?limit=）。
//
// 端点形状保留（path 与响应键名都不变），但数据源换成 events：
// 事件中心里没有"消息"这个实体，硬留着只能返回空列表。
// 管理后台前端因此无需改动，只是看到的内容从"推过的通知"变成"进来的事件"。
func (h *adminHandler) listMessages(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	events, err := h.db.ListGlobalEvents(r.Context(), limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"messages": events, "count": len(events)})
}

// GET /v1/admin/sessions —— 全站活跃会话总览。
func (h *adminHandler) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.db.ListAllSessions(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if sessions == nil {
		sessions = []*store.AdminSessionRow{}
	}
	writeJSON(w, 200, map[string]any{"sessions": sessions, "count": len(sessions)})
}
