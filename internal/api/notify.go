// Package api 实现 Anotify 的 HTTP API 处理器。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/anotify/anotify/internal/auth"
	"github.com/anotify/anotify/internal/authn"
	"github.com/anotify/anotify/internal/broker"
	"github.com/anotify/anotify/internal/route"
	"github.com/anotify/anotify/internal/store"
)

// NotifyHandler 处理 POST /v1/notify（Agent 上报）。
type NotifyHandler struct {
	Broker broker.Broker
	Keys   authn.KeyValidator
	Store  *store.DB

	// OnPublished 在消息成功入队后调用（参数为 userID），用于按需启动该用户的
	// push 派发消费者（覆盖运行期新注册用户）。可为 nil。
	OnPublished func(userID string)
}

// NotifyRequest 是 POST /v1/notify 的请求体（见 api/openapi.yaml）。
type NotifyRequest struct {
	AgentID    string   `json:"agentId"`
	SessionID  string   `json:"sessionId"`
	Cwd        string   `json:"cwd"`
	Status     string   `json:"status"`
	DurationMs int64    `json:"durationMs"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Model      string   `json:"model"`
	Link       string   `json:"link"`
	DeviceTags []string `json:"deviceTags"`
	Priority   string   `json:"priority"`
	TTL        int      `json:"ttl"`
}

// DeliveryResult 是单台设备的投递预览/结果。
type DeliveryResult struct {
	Device string `json:"device"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// NotifyResponse 是 POST /v1/notify 的响应体。
type NotifyResponse struct {
	ID      string           `json:"id"`
	Matched int              `json:"matched"`
	Results []DeliveryResult `json:"results"`
}

var validStatuses = map[string]bool{
	broker.StatusSuccess:     true,
	broker.StatusError:       true,
	broker.StatusInterrupted: true,
	broker.StatusInfo:        true,
	broker.StatusWarning:     true,
}

// maxDeviceTags / maxTagLen 是 deviceTags 归一化约束。
const (
	maxDeviceTags = 10
	maxTagLen     = 32
)

// normalizeTags 归一化 deviceTags：trim、去空、去重（大小写不敏感）、限量限长。
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len(t) > maxTagLen {
			t = t[:maxTagLen]
		}
		key := strings.ToLower(t)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
		if len(out) >= maxDeviceTags {
			break
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ServeHTTP 实现 http.Handler。
func (h *NotifyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	userID, err := authn.Authenticate(r, h.Keys, authn.ScopeNotifySend)
	if err != nil {
		switch {
		case errors.Is(err, authn.ErrForbidden):
			slog.Warn("notify auth forbidden",
				"event", "notify.auth.forbidden",
				"ip", r.RemoteAddr,
			)
			writeError(w, http.StatusForbidden, err.Error())
		default:
			slog.Warn("notify auth failed",
				"event", "notify.auth.failed",
				"ip", r.RemoteAddr,
			)
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
		}
		return
	}

	var req NotifyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	// 校验必填字段
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if !validStatuses[req.Status] {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("status must be one of success|error|interrupted|info|warning, got %q", req.Status))
		return
	}

	ttl := req.TTL
	if ttl <= 0 {
		ttl = 86400
	}
	priority := req.Priority
	if priority == "" {
		priority = "normal"
	}

	now := time.Now().UTC()
	// 保留完整请求为 payload（含 agentId/sessionId/model 等未规范化字段）
	payload, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal payload: "+err.Error())
		return
	}

	msg := &broker.Message{
		ID:         store.NewMessageID(),
		UserID:     userID,
		Title:      req.Title,
		Status:     req.Status,
		Body:       req.Body,
		Link:       req.Link,
		DeviceTags: normalizeTags(req.DeviceTags),
		Priority:   priority,
		TTLSeconds: ttl,
		Payload:    payload,
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Duration(ttl) * time.Second),
	}

	if err := h.Broker.Publish(r.Context(), msg); err != nil {
		writeError(w, http.StatusInternalServerError, "publish: "+err.Error())
		return
	}
	if h.OnPublished != nil {
		h.OnPublished(userID)
	}

	// 计算投递预览：该用户的 enabled 设备中，按路由规则命中的设备。
	// 让 Agent 能看到 "0 设备匹配"，而不是静默丢失。
	resp := NotifyResponse{ID: msg.ID, Matched: 0, Results: []DeliveryResult{}}
	if h.Store != nil {
		devices, derr := h.Store.ListEnabledDevices(r.Context(), userID)
		if derr == nil {
			matched := route.FilterDevices(devices, msg)
			resp.Matched = len(matched)
			for _, dev := range matched {
				resp.Results = append(resp.Results, DeliveryResult{
					Device: dev.ID,
					Status: store.DeliveryPending,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
	slog.Info("notify published",
		"event", "notify.published",
		"message_id", msg.ID,
		"user_id", userID,
		"matched", resp.Matched,
		"status", req.Status,
	)
}

// TestNotifyRequest 是 POST /v1/test-notify 的请求体。
// 字段比 NotifyRequest 精简：网页测试通知无需 agentId/sessionId/cwd/model/durationMs
// 等运行时上下文，服务端会填入测试占位值。
type TestNotifyRequest struct {
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Status     string   `json:"status"`
	Link       string   `json:"link"`
	DeviceTags []string `json:"deviceTags"`
	Priority   string   `json:"priority"`
}

// ServeTestNotify 处理 POST /v1/test-notify（会话 Cookie 鉴权）。
//
// 与 ServeHTTP（Agent Bearer Key 上报）不同：本方法供已登录的工作台用户
// 在网页上直接发一条测试通知到自己所有设备，无需持有 send Key。
// userID 由上层 sessMW 中间件从会话注入 context。
func (h *NotifyHandler) ServeTestNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user := auth.UserIDFromContext(r.Context())
	if user == "" {
		writeError(w, http.StatusUnauthorized, "未登录")
		return
	}

	var req TestNotifyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "测试通知"
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = broker.StatusInfo
	}
	if !validStatuses[status] {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("status must be one of success|error|interrupted|info|warning, got %q", status))
		return
	}
	priority := strings.TrimSpace(req.Priority)
	if priority == "" {
		priority = "normal"
	}
	ttl := 86400

	now := time.Now().UTC()
	// 完整 payload：测试请求体 + 来源标记，便于在通知历史里区分测试消息。
	payload, err := json.Marshal(map[string]any{
		"title":      title,
		"body":       req.Body,
		"status":     status,
		"link":       req.Link,
		"deviceTags": normalizeTags(req.DeviceTags),
		"priority":   priority,
		"source":     "test-notify",
		"agentId":    "test-notify",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal payload: "+err.Error())
		return
	}

	msg := &broker.Message{
		ID:         store.NewMessageID(),
		UserID:     user,
		Title:      title,
		Status:     status,
		Body:       req.Body,
		Link:       req.Link,
		DeviceTags: normalizeTags(req.DeviceTags),
		Priority:   priority,
		TTLSeconds: ttl,
		Payload:    payload,
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Duration(ttl) * time.Second),
	}

	if err := h.Broker.Publish(r.Context(), msg); err != nil {
		writeError(w, http.StatusInternalServerError, "publish: "+err.Error())
		return
	}
	if h.OnPublished != nil {
		h.OnPublished(user)
	}

	resp := NotifyResponse{ID: msg.ID, Matched: 0, Results: []DeliveryResult{}}
	if h.Store != nil {
		devices, derr := h.Store.ListEnabledDevices(r.Context(), user)
		if derr == nil {
			matched := route.FilterDevices(devices, msg)
			resp.Matched = len(matched)
			for _, dev := range matched {
				resp.Results = append(resp.Results, DeliveryResult{
					Device: dev.ID,
					Status: store.DeliveryPending,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
	slog.Info("test notify published",
		"event", "notify.test_published",
		"message_id", msg.ID,
		"user_id", user,
		"matched", resp.Matched,
		"status", status,
	)
}
