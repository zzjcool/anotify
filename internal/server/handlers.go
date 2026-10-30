package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
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
		ID:         store.NewDeviceID(),
		UserID:     uid,
		Name:       req.Name,
		Platform:   platform,
		Enabled:    true,
		EventScope: "final",
		Tags:       req.Tags,
		Endpoint:   req.Endpoint,
		P256dh:     req.Keys.P256dh,
		Auth:       req.Keys.Auth,
		UserAgent:  req.UserAgent,
		CreatedAt:  store.Now(),
	}
	if dev.Tags == nil {
		dev.Tags = []string{}
	}
	if err := h.db.UpsertDevice(r.Context(), dev); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	slog.Info("device upserted",
		"event", "device.upserted",
		"user_id", uid,
		"device_id", dev.ID,
		"platform", platform,
	)
	writeJSON(w, 200, map[string]any{"ok": true, "device": dev})
}

type devicePatchReq struct {
	Name       *string  `json:"name"`
	Enabled    *bool    `json:"enabled"`
	EventScope *string  `json:"eventScope"`
	Tags       []string `json:"tags"`
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
	if req.EventScope != nil {
		switch *req.EventScope {
		case "final", "all":
			dev.EventScope = *req.EventScope
		default:
			writeErr(w, 400, "eventScope 仅支持 final|all")
			return
		}
	}
	if req.Tags != nil {
		dev.Tags = req.Tags
	}
	// 用 UpdateDevice（按 id 全字段更新 name/enabled/event_scope/tags），
	// 而非 UpsertDevice（那是订阅刷新，只更新密钥，会丢配置）。
	if err := h.db.UpdateDevice(r.Context(), dev); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	slog.Info("device updated",
		"event", "device.updated",
		"user_id", uid,
		"device_id", dev.ID,
	)
	writeJSON(w, 200, map[string]any{"ok": true, "device": dev})
}

func (h *devicesHandler) remove(w http.ResponseWriter, r *http.Request, id string) {
	// DisableDevice 按 id 置 enabled=0（保留记录以便审计）；如需物理删除可扩展 store。
	if err := h.db.DisableDevice(r.Context(), id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	slog.Info("device disabled",
		"event", "device.disabled",
		"device_id", id,
	)
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
	slog.Info("key created",
		"event", "key.created",
		"user_id", uid,
		"key_id", rec.ID,
		"scopes", req.Scopes,
	)
}

func (h *keysHandler) revoke(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.db.RevokeAPIKey(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	slog.Info("key revoked",
		"event", "key.revoked",
		"key_id", id,
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- Stats ----------

// statsHandler 提供 /v1/stats（事件总数 / 今日 / 完成率 / 热力图）。
//
// 统计是**命名空间级**资源（契约 §6.2）：事件与任务都按 namespace 隔离，
// "本组织有多少事件"这个数字不存在了——同一套流程在 prod / staging
// 各有各的统计。所以它和 /v1/sources 等资源端点走**同一套**解析：
// 先 org（成员身份），再 namespaceId（**必填**，缺 → 400）。
//
// 为什么复用 consoleHandler 的两个解析函数而不是自己写一遍：
// 「缺参 400 / 不存在 404 / 别的 org 403 / `*` 只在读端点」这套规则
// 只能有一处真相，抄一份必然漂移（漂移的表现是同一个错在两页返回不同状态码）。
type statsHandler struct {
	db *store.DB
	// resolveOrg / resolveNamespace 由 mux.go 注入（consoleHandler 的同名方法）。
	resolveOrg       func(w http.ResponseWriter, r *http.Request, requested string) (string, bool)
	resolveNamespace func(w http.ResponseWriter, r *http.Request, orgID, bodyNS string, allowAll bool) (nsScope, bool)
}

func (h *statsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	orgID, ok := h.resolveOrg(w, r, r.URL.Query().Get("orgId"))
	if !ok {
		return
	}
	// allowAll=false：`*`（跨命名空间汇总）对统计同样有意义，但它的口径是
	// "把各命名空间的计数加起来"——store.MessageStats 明确拒绝 `*`
	// （返回 ErrNamespaceRequired），所以这里直接 400，而不是悄悄汇总一个
	// 用户没要求的数字。管理员汇总视图留待单独实现（见交付报告的风险项）。
	scope, ok := h.resolveNamespace(w, r, orgID, "", false)
	if !ok {
		return
	}
	// 热力图取近 371 天（53 周）
	since := store.Now() - 371*86400
	s, err := h.db.MessageStats(r.Context(), scope.ID, uid, since)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, s)
}
