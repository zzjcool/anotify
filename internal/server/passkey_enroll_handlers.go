package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/store"
)

// passkeyEnrollHandler 处理 /v1/passkey-enroll/sessions/* 端点族。
type passkeyEnrollHandler struct {
	mgr *auth.PasskeyEnrollManager
}

// ---------- DTO ----------

type enrollCreateReq struct {
	DeviceName string `json:"deviceName"`
}

type enrollCreateResp struct {
	SessionID    string `json:"sessionId"`
	Secret       string `json:"secret"` // 仅供旧设备持有，不进二维码/链接
	UserCode     string `json:"userCode"`
	AuthURL      string `json:"authUrl"`
	PollInterval int    `json:"pollInterval"`
	ExpiresAt    int64  `json:"expiresAt"`
	Kind         string `json:"kind"`
}

// enrollAnonView 是匿名 lookup 返回的最小视图（不含密钥/用户ID）。
type enrollAnonView struct {
	SessionID     string `json:"sessionId"`
	Status        string `json:"status"`
	InitiatorName string `json:"initiatorName,omitempty"` // 批准后才有值
	DeviceHint    string `json:"deviceHint,omitempty"`    // requested 后才有值
	ExpiresAt     int64  `json:"expiresAt"`
}

type enrollWatchResp struct {
	Status     string `json:"status"`
	DeviceHint string `json:"deviceHint,omitempty"`
	ExpiresAt  int64  `json:"expiresAt"`
}

type enrollPollResp struct {
	Status             string `json:"status"`
	AttestationOptions any    `json:"attestationOptions,omitempty"`
	RegistrationToken  string `json:"registrationToken,omitempty"`
	InitiatorName      string `json:"initiatorName,omitempty"`
}

type enrollRequestReq struct {
	DeviceHint string `json:"deviceHint"`
}

type enrollRequestResp struct {
	Secret string `json:"secret"`
}

// ---------- 路由分发 ----------

// ServeHTTP 路由 /v1/passkey-enroll/sessions*。
// mux 已按 method+path 注册到具体方法，此处兜底。
func (h *passkeyEnrollHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writeErr(w, 405, "method not allowed")
}

// create 旧设备发起授权会话（需登录 Cookie）。
func (h *passkeyEnrollHandler) create(w http.ResponseWriter, r *http.Request) {
	var req enrollCreateReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	created, err := h.mgr.CreateSession(req.DeviceName)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidParam) {
			writeErr(w, 400, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, enrollCreateResp{
		SessionID:    created.SessionID,
		Secret:       created.Secret,
		UserCode:     auth.FormatUserCode(created.UserCode),
		AuthURL:      enrollAuthURL(r, created.SessionID),
		PollInterval: created.PollInterval,
		ExpiresAt:    created.ExpiresAt,
		Kind:         store.CliAuthKindPasskey,
	})
}

// lookup 匿名按 ID 查会话（IP 限速）。
func (h *passkeyEnrollHandler) lookup(w http.ResponseWriter, r *http.Request, id string) {
	if rateLimited(w, rlEnrollLookup, clientIP(r)) {
		return
	}
	s, err := h.mgr.GetByID(id)
	if err != nil {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	// 校验 kind=passkey（防 apikey-kind 会话调 enroll 端点）
	if s.Kind != store.CliAuthKindPasskey {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	writeJSON(w, 200, toEnrollAnonView(s))
}

// byCode 匿名按短码查会话（IP 限速 10/min）。
func (h *passkeyEnrollHandler) byCode(w http.ResponseWriter, r *http.Request) {
	if rateLimited(w, rlEnrollLookup, clientIP(r)) {
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeErr(w, 400, "缺少 code 参数")
		return
	}
	s, err := h.mgr.GetByCode(code)
	if err != nil {
		// 防枚举：统一错误文案
		writeErr(w, 404, "授权码无效或已过期")
		return
	}
	if s.Kind != store.CliAuthKindPasskey {
		writeErr(w, 404, "授权码无效或已过期")
		return
	}
	writeJSON(w, 200, toEnrollAnonView(s))
}

// knock 新设备敲门（匿名，IP 限速）：pending→requested，返回 secret。
func (h *passkeyEnrollHandler) knock(w http.ResponseWriter, r *http.Request, id string) {
	if rateLimited(w, rlEnrollKnock, clientIP(r)) {
		return
	}
	var req enrollRequestReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	secret, err := h.mgr.RequestKnock(id, req.DeviceHint)
	if err != nil {
		h.writeEnrollErr(w, r, id, err)
		return
	}
	writeJSON(w, 200, enrollRequestResp{Secret: secret})
}

// watch 旧设备轮询（需登录 Cookie，属主校验）。
func (h *passkeyEnrollHandler) watch(w http.ResponseWriter, r *http.Request, id string) {
	uid := auth.UserIDFromContext(r.Context())
	s, err := h.mgr.GetByID(id)
	if err != nil {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	// 属主校验：approved 后 session.user_id = 批准者；pending/requested 时 user_id 为空，允许已登录用户查看（发起者）
	if s.UserID != "" && s.UserID != uid {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	if s.Kind != store.CliAuthKindPasskey {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	writeJSON(w, 200, enrollWatchResp{
		Status:     s.Status,
		DeviceHint: s.DeviceHint,
		ExpiresAt:  s.ExpiresAt,
	})
}

// poll 新设备轮询（匿名，secret 门控，按 session 限速）。
func (h *passkeyEnrollHandler) poll(w http.ResponseWriter, r *http.Request, id string) {
	if !pollG.allow(id) {
		w.Header().Set("Retry-After", "2")
		writeErr(w, 429, "轮询过于频繁，请稍后再试")
		return
	}
	secret := r.URL.Query().Get("secret")
	res, err := h.mgr.Poll(id, secret)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			writeErr(w, 401, "secret 无效")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, "授权会话不存在或已过期")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	resp := enrollPollResp{Status: res.Status}
	if res.Status == store.CliAuthApproved && res.RegistrationToken != "" {
		resp.AttestationOptions = res.AttestationOptions
		resp.RegistrationToken = res.RegistrationToken
		resp.InitiatorName = res.InitiatorName
	}
	writeJSON(w, 200, resp)
}

// approve 旧设备批准（需登录 Cookie，requested→approved）。
func (h *passkeyEnrollHandler) approve(w http.ResponseWriter, r *http.Request, id string) {
	uid := auth.UserIDFromContext(r.Context())
	err := h.mgr.Approve(id, uid)
	if err != nil {
		h.writeEnrollErr(w, r, id, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": store.CliAuthApproved})
}

// deny 旧设备拒绝（需登录 Cookie，requested→denied）。
func (h *passkeyEnrollHandler) deny(w http.ResponseWriter, r *http.Request, id string) {
	err := h.mgr.Deny(id)
	if err != nil {
		h.writeEnrollErr(w, r, id, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": store.CliAuthDenied})
}

// complete 新设备提交 attestation（匿名，registrationToken 门控）。
// 注意：不能预读 body，go-webauthn 的 FinishRegistration 直接从 r.Body 解析。
// 因此先从 query/单独 body 取 registrationToken 和 name，然后让 FinishRegistration 读 body。
// 实际上 complete 的 body 就是裸 attestation JSON，registrationToken 和 name 从 query 取。
func (h *passkeyEnrollHandler) complete(w http.ResponseWriter, r *http.Request, id string) {
	if rateLimited(w, rlEnrollComplete, clientIP(r)) {
		return
	}
	regToken := r.URL.Query().Get("registrationToken")
	if regToken == "" {
		writeErr(w, 400, "缺少 registrationToken")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "新设备"
	}
	result, err := h.mgr.Complete(id, regToken, name, r)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			writeErr(w, 401, "registrationToken 无效")
			return
		}
		if errors.Is(err, auth.ErrAlreadyConsumed) {
			writeErr(w, 409, "会话已完成，不可重复提交")
			return
		}
		h.writeEnrollErr(w, r, id, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "passkeyId": result.PasskeyID})
}

// cancel 旧设备取消会话（需登录 Cookie，属主校验）。
func (h *passkeyEnrollHandler) cancel(w http.ResponseWriter, r *http.Request, id string) {
	uid := auth.UserIDFromContext(r.Context())
	s, err := h.mgr.GetByID(id)
	if err != nil {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	// 属主校验
	if s.UserID != "" && s.UserID != uid {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	if s.Kind != store.CliAuthKindPasskey {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	if err := h.mgr.Delete(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// qr 渲染 ASCII 二维码（匿名，IP 限速）。
func (h *passkeyEnrollHandler) qr(w http.ResponseWriter, r *http.Request, id string) {
	if rateLimited(w, rlQR, clientIP(r)) {
		return
	}
	s, err := h.mgr.GetByID(id)
	if err != nil {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	if s.Kind != store.CliAuthKindPasskey {
		writeErr(w, 404, "授权会话不存在或已过期")
		return
	}
	url := enrollAuthURL(r, s.ID)
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

// ---------- 辅助 ----------

func toEnrollAnonView(s *store.CliAuthSession) enrollAnonView {
	v := enrollAnonView{
		SessionID: s.ID,
		Status:    s.Status,
		ExpiresAt: s.ExpiresAt,
	}
	// requested 后才有 deviceHint
	if s.Status == store.CliAuthRequested || s.Status == store.CliAuthApproved || s.Status == store.CliAuthConsumed {
		v.DeviceHint = s.DeviceHint
	}
	// approved 后才有 initiatorName（匿名视图不返回 user_id，只返回 displayName）
	if s.Status == store.CliAuthApproved || s.Status == store.CliAuthConsumed {
		v.InitiatorName = s.DeviceName // 发起设备名（旧设备名），供新设备核对
	}
	return v
}

func (h *passkeyEnrollHandler) writeEnrollErr(w http.ResponseWriter, r *http.Request, id string, err error) {
	if errors.Is(err, auth.ErrInvalidParam) {
		writeErr(w, 400, err.Error())
		return
	}
	if errors.Is(err, auth.ErrAlreadyTerminal) {
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

// enrollAuthURL 构造授权链接：<scheme>://<host>/passkey-enroll.html?s=<sessionId>
func enrollAuthURL(r *http.Request, sessionID string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		if i := strings.Index(xfp, ","); i >= 0 {
			xfp = strings.TrimSpace(xfp[:i])
		}
		if xfp == "https" || xfp == "http" {
			scheme = xfp
		}
	}
	return scheme + "://" + r.Host + "/passkey-enroll.html?s=" + sessionID
}
