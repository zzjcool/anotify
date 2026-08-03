package server

import (
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
		writeErr(w, 400, err.Error())
		return
	}
	h.svc.Sessions().SetCookie(w, sessID)
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
		writeErr(w, 401, err.Error())
		return
	}
	h.svc.Sessions().SetCookie(w, sessID)
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
	writeJSON(w, 200, map[string]any{"ok": true})
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
