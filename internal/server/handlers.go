package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/broker"
	"github.com/anotify/anotify/internal/store"
)

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 写错误响应。
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// readJSON 解析请求体。
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---------- Devices ----------

type devicesHandler struct {
	db *store.DB
}

type deviceUpsertReq struct {
	Name     string   `json:"name"`
	Platform string   `json:"platform"`
	Tags     []string `json:"tags"`
	Endpoint string   `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	UserAgent string `json:"userAgent"`
}

func (h *devicesHandler) list(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	devs, err := h.db.ListDevices(r.Context(), uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if devs == nil {
		devs = []*store.Device{}
	}
	writeJSON(w, 200, map[string]any{"devices": devs, "count": len(devs)})
}

func (h *devicesHandler) upsert(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	var req deviceUpsertReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	if req.Endpoint == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		writeErr(w, 400, "订阅数据不完整（endpoint/keys.p256dh/keys.auth）")
		return
	}
	platform := req.Platform
	if platform == "" {
		platform = "other"
	}
	dev := &store.Device{
		ID:           store.NewDeviceID(),
		UserID:       uid,
		Name:         req.Name,
		Platform:     platform,
		Enabled:      true,
		StatusFilter: "all",
		Tags:         req.Tags,
		Endpoint:     req.Endpoint,
		P256dh:       req.Keys.P256dh,
		Auth:         req.Keys.Auth,
		UserAgent:    req.UserAgent,
		CreatedAt:    store.Now(),
	}
	if dev.Tags == nil {
		dev.Tags = []string{}
	}
	if err := h.db.UpsertDevice(r.Context(), dev); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "device": dev})
}

type devicePatchReq struct {
	Name         *string  `json:"name"`
	Enabled      *bool    `json:"enabled"`
	StatusFilter *string  `json:"statusFilter"`
	Tags         []string `json:"tags"`
}

func (h *devicesHandler) patch(w http.ResponseWriter, r *http.Request, id string) {
	uid := auth.UserIDFromContext(r.Context())
	devs, err := h.db.ListDevices(r.Context(), uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var dev *store.Device
	for _, d := range devs {
		if d.ID == id {
			dev = d
			break
		}
	}
	if dev == nil {
		writeErr(w, 404, "设备不存在")
		return
	}
	var req devicePatchReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	if req.Name != nil {
		dev.Name = *req.Name
	}
	if req.Enabled != nil {
		dev.Enabled = *req.Enabled
	}
	if req.StatusFilter != nil {
		switch *req.StatusFilter {
		case "all", "error", "success":
			dev.StatusFilter = *req.StatusFilter
		default:
			writeErr(w, 400, "statusFilter 仅支持 all|error|success")
			return
		}
	}
	if req.Tags != nil {
		dev.Tags = req.Tags
	}
	// 用 UpdateDevice（按 id 全字段更新 name/enabled/status_filter/tags），
	// 而非 UpsertDevice（那是订阅刷新，只更新密钥，会丢配置）。
	if err := h.db.UpdateDevice(r.Context(), dev); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "device": dev})
}

func (h *devicesHandler) remove(w http.ResponseWriter, r *http.Request, id string) {
	// DisableDevice 按 id 置 enabled=0（保留记录以便审计）；如需物理删除可扩展 store。
	if err := h.db.DisableDevice(r.Context(), id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ServeHTTP 路由：/v1/devices 与 /v1/devices/{id}
func (h *devicesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/devices")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			h.list(w, r)
		case http.MethodPost:
			h.upsert(w, r)
		default:
			writeErr(w, 405, "method not allowed")
		}
		return
	}
	switch r.Method {
	case http.MethodPatch:
		h.patch(w, r, rest)
	case http.MethodDelete:
		h.remove(w, r, rest)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

// ---------- Keys ----------

type keysHandler struct {
	keys *auth.KeyManager
	db   *store.DB
}

type keyCreateReq struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

func (h *keysHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/keys"), "/")
	// /v1/keys/{id}/revoke
	if rest != "" && strings.HasSuffix(rest, "/revoke") {
		id := strings.TrimSuffix(rest, "/revoke")
		id = strings.Trim(id, "/")
		if r.Method == http.MethodPost {
			h.revoke(w, r, id)
			return
		}
		writeErr(w, 405, "method not allowed")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.list(w, r)
	case http.MethodPost:
		h.create(w, r)
	default:
		writeErr(w, 405, "method not allowed")
	}
}

// keyPublic 是 API Key 的安全公开视图：绝不含 key_hash（密钥材料不外泄），字段 camelCase。
type keyPublic struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	Enabled    bool     `json:"enabled"`
	CreatedAt  int64    `json:"createdAt"`
	LastUsedAt *int64   `json:"lastUsedAt,omitempty"`
}

func toKeyPublic(k *store.APIKey) keyPublic {
	var last *int64
	if k.LastUsedAt.Valid {
		last = &k.LastUsedAt.Int64
	}
	return keyPublic{
		ID: k.ID, Name: k.Name, Prefix: k.Prefix, Scopes: k.Scopes,
		Enabled: k.Enabled, CreatedAt: k.CreatedAt, LastUsedAt: last,
	}
}

func (h *keysHandler) list(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	recs, err := h.db.ListAPIKeysByUser(uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]keyPublic, 0, len(recs))
	for _, k := range recs {
		out = append(out, toKeyPublic(k))
	}
	writeJSON(w, 200, map[string]any{"keys": out, "count": len(out)})
}

func (h *keysHandler) create(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	var req keyCreateReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	if len(req.Scopes) == 0 {
		writeErr(w, 400, "至少选择一个 scope")
		return
	}
	plaintext, rec, err := h.keys.CreateKey(uid, req.Name, req.Scopes)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// 明文仅此一次返回；record 用安全公开视图（不含哈希）
	writeJSON(w, 200, map[string]any{"ok": true, "key": plaintext, "record": toKeyPublic(rec)})
}

func (h *keysHandler) revoke(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.db.RevokeAPIKey(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- Notifications ----------

type notificationsHandler struct {
	bk broker.Broker
}

func (h *notificationsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	since := int64(0)
	if q := r.URL.Query().Get("sinceSeq"); q != "" {
		if n, err := strconv.ParseInt(q, 10, 64); err == nil {
			since = n
		}
	}
	msgs, err := h.bk.Replay(r.Context(), uid, since, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 保证空结果序列化为 [] 而非 null（前端据 Array.isArray 判断连接成功）
	if msgs == nil {
		msgs = []*broker.Message{}
	}
	writeJSON(w, 200, map[string]any{"notifications": msgs, "count": len(msgs)})
}
