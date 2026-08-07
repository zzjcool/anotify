package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/authn"
	"github.com/anotify/anotify/internal/store"
)

// cliAuthHandler 处理 CLI 设备授权端点。
type cliAuthHandler struct {
	mgr      *auth.CliAuthManager
	keys     *auth.KeyManager
	keysV    authn.KeyValidator
	db       *store.DB
	staticFS http.FileSystem // 用于 /agent-login.sh 分发
}

// ---------- DTO ----------

type createSessionReq struct {
	DeviceName string   `json:"deviceName"`
	Scopes     []string `json:"scopes"`
}

type createSessionResp struct {
	SessionID    string   `json:"sessionId"`
	Secret       string   `json:"secret"`
	UserCode     string   `json:"userCode"`
	AuthURL      string   `json:"authUrl"`
	PollInterval int      `json:"pollInterval"`
	ExpiresAt    int64    `json:"expiresAt"`
	Scopes       []string `json:"scopes"`
}

type sessionView struct {
	SessionID       string   `json:"sessionId"`
	DeviceName      string   `json:"deviceName"`
	RequestedScopes []string `json:"requestedScopes"`
	Status          string   `json:"status"`
	CreatedAt       int64    `json:"createdAt"`
	ExpiresAt       int64    `json:"expiresAt"`
}

type approveReq struct {
	Scopes []string `json:"scopes"`
}

type pollResp struct {
	Status string   `json:"status"`
	APIKey string   `json:"apiKey,omitempty"`
	KeyID  string   `json:"keyId,omitempty"`
	Name   string   `json:"name,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

// ---------- 入口路由 ----------

// ServeHTTP 路由 /v1/cli-auth/sessions*（由 mux 注册时统一 dispatch）。
// 注意：匿名端点（create/poll/qr）与鉴权端点（get/by-code/approve/deny）
// 在 mux 中分别用不同中间件注册，此方法仅处理 create/approve/deny/get/by-code。
func (h *cliAuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// mux 已按 method+path 注册到具体方法，此处兜底
	writeErr(w, 405, "method not allowed")
}

// create 建会话（匿名，IP 限速）。
func (h *cliAuthHandler) create(w http.ResponseWriter, r *http.Request) {
	if rateLimited(w, rlCreate, clientIP(r)) {
		return
	}
	var req createSessionReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	if len(req.Scopes) == 0 {
		writeErr(w, 400, "scopes 不能为空")
		return
	}
	created, err := h.mgr.CreateSession(req.DeviceName, req.Scopes)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidParam) {
			writeErr(w, 400, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	scopes := created.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	writeJSON(w, 200, createSessionResp{
		SessionID:    created.SessionID,
		Secret:       created.Secret,
		UserCode:     auth.FormatUserCode(created.UserCode),
		AuthURL:      authURL(r, created.SessionID),
		PollInterval: created.PollInterval,
		ExpiresAt:    created.ExpiresAt,
		Scopes:       scopes,
	})
	slog.Info("cli auth session created",
		"event", "cliauth.session.created",
		"session_id", created.SessionID,
		"ip", clientIP(r),
		"device_name", req.DeviceName,
	)
}

// get 按 ID 查会话（需登录）。
func (h *cliAuthHandler) get(w http.ResponseWriter, r *http.Request, id string) {
	s, err := h.mgr.GetByID(id)
	if err != nil {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	writeJSON(w, 200, toSessionView(s))
}

// byCode 按短码查会话（需登录，按用户限速）。
func (h *cliAuthHandler) byCode(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	if rateLimited(w, rlByCode, uid) {
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeErr(w, 400, "缺少 code 参数")
		return
	}
	s, err := h.mgr.GetByCode(code)
	if err != nil {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	writeJSON(w, 200, toSessionView(s))
}

// approve 批准（需登录）。
func (h *cliAuthHandler) approve(w http.ResponseWriter, r *http.Request, id string) {
	uid := auth.UserIDFromContext(r.Context())
	var req approveReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	if len(req.Scopes) == 0 {
		writeErr(w, 400, "scopes 不能为空")
		return
	}
	err := h.mgr.Approve(id, uid, req.Scopes)
	if err != nil {
		slog.Warn("cli auth approve conflict",
			"event", "cliauth.approve.conflict",
			"session_id", id,
			"user_id", uid,
			"error", err.Error(),
		)
		h.writeApproveErr(w, r, id, err)
		return
	}
	slog.Info("cli auth session approved",
		"event", "cliauth.session.approved",
		"session_id", id,
		"user_id", uid,
		"scopes", req.Scopes,
	)
	writeJSON(w, 200, map[string]string{"status": store.CliAuthApproved})
}

// deny 拒绝（需登录）。
func (h *cliAuthHandler) deny(w http.ResponseWriter, r *http.Request, id string) {
	uid := auth.UserIDFromContext(r.Context())
	err := h.mgr.Deny(id)
	if err != nil {
		slog.Warn("cli auth deny conflict",
			"event", "cliauth.approve.conflict",
			"session_id", id,
			"user_id", uid,
			"error", err.Error(),
		)
		h.writeApproveErr(w, r, id, err)
		return
	}
	slog.Info("cli auth session denied",
		"event", "cliauth.session.denied",
		"session_id", id,
		"user_id", uid,
	)
	writeJSON(w, 200, map[string]string{"status": store.CliAuthDenied})
}

// poll 轮询（匿名，secret 门控，按会话最小间隔限速）。
func (h *cliAuthHandler) poll(w http.ResponseWriter, r *http.Request, id string) {
	if !pollG.allow(id) {
		w.Header().Set("Retry-After", "2")
		writeErr(w, 429, "轮询过于频繁，请稍后再试")
		return
	}
	secret := r.URL.Query().Get("secret")
	res, err := h.mgr.Poll(id, secret)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			slog.Warn("cli auth poll secret invalid",
				"event", "cliauth.poll.unauthorized",
				"session_id", id,
				"ip", clientIP(r),
			)
			writeErr(w, 401, "secret 无效")
			return
		}
		if errors.Is(err, auth.ErrInvalidParam) {
			// kind guard：passkey-kind 会话不得走 apikey poll
			writeErr(w, 403, "会话类型不匹配")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, "授权会话不存在或已过期")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	resp := pollResp{Status: res.Status}
	if res.Status == store.CliAuthApproved && res.APIKey != "" {
		resp.APIKey = res.APIKey
		resp.KeyID = res.KeyID
		resp.Name = res.KeyName
		scopes := res.Scopes
		if scopes == nil {
			scopes = []string{}
		}
		resp.Scopes = scopes
		slog.Info("cli auth session consumed",
			"event", "cliauth.session.consumed",
			"session_id", id,
			"key_id", res.KeyID,
		)
	}
	writeJSON(w, 200, resp)
}

// qr 渲染 ASCII 二维码（匿名，IP 限速）。
func (h *cliAuthHandler) qr(w http.ResponseWriter, r *http.Request, id string) {
	if rateLimited(w, rlQR, clientIP(r)) {
		return
	}
	// 先校验会话存在（不泄露 secret，仅 404）
	s, err := h.mgr.GetByID(id)
	if err != nil {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	url := authURL(r, s.ID)
	ascii, err := renderQRASCII(url)
	if err != nil {
		writeErr(w, 500, "二维码生成失败")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(ascii))
}

// keysSelf 自检 Key（Bearer）。
func (h *cliAuthHandler) keysSelf(w http.ResponseWriter, r *http.Request) {
	tok := authn.BearerToken(r)
	if tok == "" {
		writeErr(w, 401, "缺少 API Key")
		return
	}
	uid, scopes, err := h.keysV.ValidateKey(r.Context(), tok)
	if err != nil {
		writeErr(w, 401, "API Key 无效")
		return
	}
	// 取记录拿 name/prefix
	prefix := tok
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	rec, err := h.db.GetAPIKeyByPrefix(prefix)
	_ = uid
	if err != nil {
		writeErr(w, 401, "API Key 无效")
		return
	}
	sc := scopes
	if sc == nil {
		sc = []string{}
	}
	writeJSON(w, 200, map[string]any{
		"name":   rec.Name,
		"prefix": rec.Prefix,
		"scopes": sc,
	})
}

// agentLoginScript 分发登录脚本（匿名，no-store）。
func (h *cliAuthHandler) agentLoginScript(w http.ResponseWriter, r *http.Request) {
	f, err := h.staticFS.Open("/agent-login.sh")
	if err != nil {
		writeErr(w, 404, "登录脚本暂不可用")
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		writeErr(w, 500, "读取登录脚本失败")
		return
	}
	w.Header().Set("Content-Type", "text/x-sh; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(int(stat.Size())))
	w.WriteHeader(200)
	_, _ = io.Copy(w, f)
}

// ---------- 辅助 ----------

func toSessionView(s *store.CliAuthSession) sessionView {
	rs := s.ScopesRequested
	if rs == nil {
		rs = []string{}
	}
	return sessionView{
		SessionID:       s.ID,
		DeviceName:      s.DeviceName,
		RequestedScopes: rs,
		Status:          s.Status,
		CreatedAt:       s.CreatedAt,
		ExpiresAt:       s.ExpiresAt,
	}
}

func (h *cliAuthHandler) writeApproveErr(w http.ResponseWriter, r *http.Request, id string, err error) {
	if errors.Is(err, auth.ErrInvalidParam) {
		writeErr(w, 400, err.Error())
		return
	}
	if errors.Is(err, auth.ErrAlreadyTerminal) {
		// 终态冲突：重新查询当前状态并返回真实 status（而非写死 "terminal"）。
		s, fetchErr := h.mgr.GetByID(id)
		if fetchErr != nil {
			writeErr(w, 404, "授权会话不存在或已过期")
			return
		}
		writeJSON(w, 409, map[string]string{"status": s.Status})
		return
	}
	writeErr(w, 500, err.Error())
}

// authURL 构造授权链接：<scheme>://<host>/cli-auth.html?s=<sessionId>
func authURL(r *http.Request, sessionID string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		// 取第一段
		if i := strings.Index(xfp, ","); i >= 0 {
			xfp = strings.TrimSpace(xfp[:i])
		}
		if xfp == "https" || xfp == "http" {
			scheme = xfp
		}
	}
	return scheme + "://" + r.Host + "/cli-auth.html?s=" + sessionID
}
