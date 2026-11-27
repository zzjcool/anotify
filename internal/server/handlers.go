package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
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
	if uid == "" {
		writeErr(w, 401, "未登录")
		return
	}
	limit, msg := pageLimitPlain(r)
	if msg != "" {
		writeErr(w, 400, msg)
		return
	}
	binding := "keys|" + uid
	keys, err := decodePageCursor(r.URL.Query().Get("continue"), binding, 2)
	if err != nil {
		writeErr(w, 400, pageCursorMessage(err))
		return
	}
	afterAt, afterID := int64(0), ""
	if len(keys) == 2 {
		if afterAt, err = pageCursorInt64(keys[0]); err != nil {
			writeErr(w, 400, pageCursorMessage(err))
			return
		}
		afterID = keys[1]
	}
	recs, err := h.db.ListAPIKeysByUserPage(r.Context(), uid, afterAt, afterID, limit+1)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	recs, cont := pageCut(recs, limit, binding, func(k *store.APIKey) []string {
		return []string{pageKeyInt64(k.CreatedAt), k.ID}
	})
	out := make([]keyPublic, 0, len(recs))
	for _, k := range recs {
		out = append(out, toKeyPublic(k))
	}
	// count 是**本页**条数；`continue` 非空表示还有下一页（末页回 ""）。
	writeJSON(w, 200, map[string]any{"keys": out, "count": len(out), "continue": cont})
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
	// 走 KeyManager.RevokeKey 而不是直接 db.RevokeAPIKey：前者在 DB 写成功后
	// **同步**让该 key 的验证缓存失效（#227）。直接调 db 会绕过失效 → 被吊销的 key
	// 还能继续用最多一个缓存 TTL（60s），那是真正的安全回归。
	if err := h.keys.RevokeKey(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	slog.Info("key revoked",
		"event", "key.revoked",
		"key_id", id,
	)
	writeJSON(w, 200, map[string]any{"ok": true})
}
