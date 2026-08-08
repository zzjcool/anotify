package server

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/store"
)

// authHandler 包装 auth.Service，提供 /v1/auth/* HTTP 端点。
type authHandler struct {
	svc *auth.Service
}

type usernameReq struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Token       string `json:"token"` // discoverable login 的会话 token
}

// ServeHTTP 路由 /v1/auth/*。
func (h *authHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/auth"), "/")
	switch sub {
	case "register/options":
		h.registerOptions(w, r)
	case "register":
		h.register(w, r)
	case "login/options":
		h.loginOptions(w, r)
	case "login":
		h.login(w, r)
	case "logout":
		h.logout(w, r)
	case "sessions":
		h.sessions(w, r)
	case "me":
		h.me(w, r)
	default:
		writeErr(w, 404, "unknown auth endpoint: "+sub)
	}
}

func (h *authHandler) registerOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req usernameReq
	if err := readJSON(r, &req); err != nil || req.Username == "" {
		writeErr(w, 400, "缺少 username")
		return
	}
	if err := auth.ValidateUsername(req.Username); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	opts, err := h.svc.BeginRegister(req.Username, req.DisplayName)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, opts)
}

func (h *authHandler) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	// username 从 query 传（WebAuthn 断言在 body）
	username := r.URL.Query().Get("username")
	if username == "" {
		writeErr(w, 400, "缺少 username（query）")
		return
	}
	sessID, err := h.svc.FinishRegister(username, r)
	if err != nil {
		slog.Warn("register failed",
			"event", "auth.register.fail",
			"ip", clientIP(r),
			"error", err.Error(),
		)
		writeErr(w, 400, err.Error())
		return
	}
	h.svc.Sessions().SetCookie(w, sessID)
	slog.Info("register success",
		"event", "auth.register.success",
		"ip", clientIP(r),
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *authHandler) loginOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	var req usernameReq
	_ = readJSON(r, &req) // username 可空（discoverable）
	var (
		opts any
		err  error
	)
	if req.Username != "" {
		if verr := auth.ValidateUsername(req.Username); verr != nil {
			writeErr(w, 400, verr.Error())
			return
		}
		opts, err = h.svc.BeginLogin(req.Username)
	} else {
		opts, err = h.svc.BeginDiscoverableLogin(req.Token)
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, opts)
}

func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	username := r.URL.Query().Get("username")
	token := r.URL.Query().Get("token")
	var (
		sessID string
		err    error
	)
	if username != "" {
		sessID, err = h.svc.FinishLogin(username, r)
	} else {
		sessID, err = h.svc.FinishDiscoverableLogin(token, r)
	}
	if err != nil {
		slog.Warn("login failed",
			"event", "auth.login.fail",
			"ip", clientIP(r),
			"error", err.Error(),
		)
		writeErr(w, 401, err.Error())
		return
	}
	h.svc.Sessions().SetCookie(w, sessID)
	slog.Info("login success",
		"event", "auth.login.success",
		"ip", clientIP(r),
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *authHandler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	if c, err := r.Cookie(auth.SessionCookieName); err == nil {
		_ = h.svc.Sessions().Revoke(c.Value)
	}
	h.svc.Sessions().ClearCookie(w)
	slog.Info("logout",
		"event", "auth.logout",
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// me 返回当前登录用户（前端侧栏/顶栏显示真实用户名用）。
func (h *authHandler) me(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	u, err := h.svc.GetUser(uid)
	if err != nil {
		writeErr(w, 404, "用户不存在")
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": u.ID, "username": u.Username, "displayName": u.DisplayName,
		"role": u.Role, "disabled": u.Disabled, "createdAt": u.CreatedAt,
	})
}

func (h *authHandler) sessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	list, err := h.svc.Sessions().ListByUser(uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []*store.Session{}
	}
	writeJSON(w, 200, map[string]any{"sessions": list, "count": len(list)})
}

// passkeysRoot 路由 /v1/auth/passkeys（无尾斜杠）：GET 列表 / POST 补建凭证完成。
// 补建凭证的 options 与 finish 走 /v1/auth/passkeys/register* 子路径，由 passkeysItem 处理。
func (h *authHandler) passkeysRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listPasskeys(w, r)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

// passkeysItem 路由 /v1/auth/passkeys/*：补建凭证 options/finish、重命名、删除。
func (h *authHandler) passkeysItem(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, "/v1/auth/passkeys/")
	switch {
	case sub == "register/options" && r.Method == http.MethodPost:
		h.addPasskeyOptions(w, r)
	case sub == "register" && r.Method == http.MethodPost:
		h.addPasskeyFinish(w, r)
	case strings.Contains(sub, "/"):
		// /v1/auth/passkeys/register/options 已在上面处理；其它多段路径不接受。
		writeErr(w, 404, "unknown passkey endpoint: "+sub)
	case r.Method == http.MethodPatch:
		h.renamePasskey(w, r, sub)
	case r.Method == http.MethodDelete:
		h.deletePasskey(w, r, sub)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

// ---------- Passkey 管理（已登录用户） ----------

// passkeyOut 是返回给前端的 Passkey 表示（不泄露 publicKey 原始字节）。
type passkeyOut struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Transports     []string `json:"transports"`
	BackupEligible bool     `json:"backupEligible"`
	CreatedAt      int64    `json:"createdAt"`
	LastUsedAt     int64    `json:"lastUsedAt"`
	Current        bool     `json:"current"`
}

func toPasskeyOut(p *store.Passkey) passkeyOut {
	tr := p.Transports
	if tr == nil {
		tr = []string{}
	}
	var lastUsed int64
	if p.LastUsedAt.Valid {
		lastUsed = p.LastUsedAt.Int64
	}
	return passkeyOut{
		ID:             p.ID,
		Name:           p.Name,
		Type:           "passkey",
		Transports:     tr,
		BackupEligible: p.BackupEligible,
		CreatedAt:      p.CreatedAt,
		LastUsedAt:     lastUsed,
	}
}

// listPasskeys GET /v1/auth/passkeys —— 列当前用户的凭证。
// 返回空数组 [] 而非 null，否则前端误判 demo 模式（AGENTS.md 第 3 节）。
func (h *authHandler) listPasskeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	list, err := h.svc.ListPasskeys(uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]passkeyOut, 0, len(list))
	for _, p := range list {
		out = append(out, toPasskeyOut(p))
	}
	writeJSON(w, 200, map[string]any{"passkeys": out, "count": len(out)})
}

// addPasskeyOptions POST /v1/auth/passkeys/register/options —— 已登录用户补建凭证。
// 与注册新用户的 /register/options 区分：不传 username，从会话取 userID。
func (h *authHandler) addPasskeyOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	creation, err := h.svc.BeginAddCredential(uid)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, creation)
}

// addPasskeyFinish POST /v1/auth/passkeys/register —— 完成补建凭证。
// body: 裸 WebAuthn attestation JSON（{id,rawId,type,response:{clientDataJSON,attestationObject}}）。
// name 从 query ?name= 取（可选，默认「新设备」），与注册新用户的 ?username= 风格一致。
// 注意：不能预读 body，go-webauthn 的 FinishRegistration 直接从 r.Body 解析。
func (h *authHandler) addPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "新设备"
	}
	if err := h.svc.FinishAddCredential(uid, name, r); err != nil {
		slog.Warn("passkey add failed",
			"event", "auth.passkey.add_fail",
			"user_id", uid,
			"error", err.Error(),
		)
		writeErr(w, 400, err.Error())
		return
	}
	slog.Info("passkey created",
		"event", "auth.passkey.created",
		"user_id", uid,
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// renamePasskey PATCH /v1/auth/passkeys/{id} —— 重命名。
// 越权（改别人的凭证）应返回 404，不泄露存在性。
func (h *authHandler) renamePasskey(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPatch {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	// 先校验凭证属于当前用户（越权转 404）。
	p, err := h.svc.GetPasskey(id)
	if err != nil || p.UserID != uid {
		writeErr(w, 404, "凭证不存在")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil || req.Name == "" {
		writeErr(w, 400, "缺少 name")
		return
	}
	if err := h.svc.RenamePasskey(id, req.Name); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	slog.Info("passkey renamed",
		"event", "auth.passkey.renamed",
		"user_id", uid,
		"passkey_id", id,
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// deletePasskey DELETE /v1/auth/passkeys/{id} —— 删除。
// 越权（删别人的凭证）转 404。至少保留一个 Passkey，删最后一个返回 409。
func (h *authHandler) deletePasskey(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		writeErr(w, 405, "method not allowed")
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	p, err := h.svc.GetPasskey(id)
	if err != nil || p.UserID != uid {
		writeErr(w, 404, "凭证不存在")
		return
	}
	// 至少保留一个 Passkey：删掉最后一个会导致用户无法再用 Passkey 登录
	// （只能靠恢复码/其它凭证兜底），产品上应禁止。
	list, err := h.svc.ListPasskeys(uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(list) <= 1 {
		slog.Warn("passkey delete blocked: last credential",
			"event", "auth.passkey.delete_last_blocked",
			"user_id", uid,
			"passkey_id", id,
		)
		writeErr(w, 409, "至少保留一个 Passkey，删除后你将无法登录。请先添加新的 Passkey 再删除此凭证。")
		return
	}
	if err := h.svc.DeletePasskey(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	slog.Info("passkey deleted",
		"event", "auth.passkey.deleted",
		"user_id", uid,
		"passkey_id", id,
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}
