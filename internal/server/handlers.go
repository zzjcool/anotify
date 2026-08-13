package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/broker"
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
		ID:           store.NewDeviceID(),
		UserID:       uid,
		Name:         req.Name,
		Platform:     platform,
		Enabled:      true,
		EventScope:   "final",
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

type statsHandler struct {
	db *store.DB
}

func (h *statsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFromContext(r.Context())
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	// 热力图取近 371 天（53 周）
	since := store.Now() - 371*86400
	s, err := h.db.MessageStats(r.Context(), uid, since)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, s)
}

// ---------- Notifications ----------

type notificationsHandler struct {
	bk broker.Broker
	db *store.DB
}

// messageView 是 broker.Message 的 JSON 视图。
// broker.Message.Payload 是 []byte，默认会被序列化成 base64 字符串，
// 前端无法直接读 agentId/model 等字段；这里转成 json.RawMessage 嵌入为 JSON 对象。
type messageView struct {
	ID         string          `json:"id"`
	UserID     string          `json:"userId"`
	Seq        int64           `json:"seq"`
	Title      string          `json:"title"`
	AgentState string          `json:"agentState"`
	Severity   string          `json:"severity,omitempty"`
	Body       string          `json:"body"`
	Link       string          `json:"link"`
	DeviceTags []string        `json:"deviceTags"`
	Priority   string          `json:"priority"`
	TTLSeconds int             `json:"ttlSeconds"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"createdAt"`
	ExpiresAt  time.Time       `json:"expiresAt"`
}

// toMessageView 把一条消息转成 JSON 视图；空/非法 payload 兜底为 {}。
func toMessageView(m *broker.Message) *messageView {
	payload := m.Payload
	if len(payload) == 0 || !json.Valid(payload) {
		payload = []byte("{}")
	}
	tags := m.DeviceTags
	if tags == nil {
		tags = []string{}
	}
	return &messageView{
		ID: m.ID, UserID: m.UserID, Seq: m.Seq, Title: m.Title,
		AgentState: m.AgentState, Severity: m.Severity,
		Body: m.Body, Link: m.Link, DeviceTags: tags, Priority: m.Priority,
		TTLSeconds: m.TTLSeconds, Payload: payload, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt,
	}
}

// notificationDetail 是单条消息详情视图：完整消息 + 真实投递记录。
type notificationDetail struct {
	*messageView
	Deliveries []*store.DeliveryRow `json:"deliveries"`
}

func (h *notificationsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /v1/notifications/{id} → 单条消息详情
	if rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/notifications"), "/"); rest != "" {
		h.getOne(w, r, rest)
		return
	}
	uid := auth.UserIDFromContext(r.Context())
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	// 工作台「最近通知」需要最新 N 条（seq 降序），与 broker.Replay（升序回放
	// 历史，供 WS 补帧）语义相反，用 store 层 ListRecentMessages。
	rows, err := h.db.ListRecentMessages(r.Context(), uid, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 转成 JSON 视图（payload 解码为对象），并保证空结果序列化为 [] 而非 null
	out := make([]*messageView, 0, len(rows))
	for _, row := range rows {
		out = append(out, toMessageView(&broker.Message{
			ID:         row.ID,
			UserID:     row.UserID,
			Seq:        row.Seq,
			Title:      row.Title,
			AgentState: row.AgentState,
			Severity:   row.Severity,
			Body:       row.Body,
			Link:       row.Link,
			DeviceTags: row.DeviceTags,
			Priority:   row.Priority,
			TTLSeconds: row.TTLSeconds,
			Payload:    row.Payload,
			CreatedAt:  row.CreatedAt,
			ExpiresAt:  row.ExpiresAt,
		}))
	}
	writeJSON(w, 200, map[string]any{"notifications": out, "count": len(out)})
}

// getOne 返回单条消息详情（仅限属主；含真实投递记录，供 message.html 详情页）。
func (h *notificationsHandler) getOne(w http.ResponseWriter, r *http.Request, id string) {
	uid := auth.UserIDFromContext(r.Context())
	row, err := h.db.GetMessage(r.Context(), uid, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if row == nil {
		writeErr(w, 404, "消息不存在")
		return
	}
	deliveries, err := h.db.ListDeliveriesByMessage(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	msg := toMessageView(&broker.Message{
		ID:         row.ID,
		UserID:     row.UserID,
		Seq:        row.Seq,
		Title:      row.Title,
		AgentState: row.AgentState,
		Severity:   row.Severity,
		Body:       row.Body,
		Link:       row.Link,
		DeviceTags: row.DeviceTags,
		Priority:   row.Priority,
		TTLSeconds: row.TTLSeconds,
		Payload:    row.Payload,
		CreatedAt:  row.CreatedAt,
		ExpiresAt:  row.ExpiresAt,
	})
	writeJSON(w, 200, &notificationDetail{messageView: msg, Deliveries: deliveries})
}
