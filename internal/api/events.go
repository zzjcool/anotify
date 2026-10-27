package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/trigger"
)

// EventHandler 处理事件入口（契约 §6.2）。
//
//	POST /v1/events   外部事件入口（webhook 通用）
//	GET  /v1/events   事件列表（按 trace）
type EventHandler struct {
	Store    *store.DB
	Keys     authn.KeyValidator
	Triggers *trigger.Service

	// DefaultOrgForUser 在用户没有指定组织时用主组织（单组织用户的常见情形）。
	// 为 nil 时退化为「必须显式给 orgId」。
	DefaultOrgForUser func(userID string) (string, error)
}

// EventIngestRequest 是 POST /v1/events 的请求体（camelCase，契约 §6.2）。
type EventIngestRequest struct {
	Source  string          `json:"source"`
	Type    string          `json:"type"`
	IdemKey string          `json:"idemKey,omitempty"`
	TraceID string          `json:"traceId,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// OrgID 可选：不给时用调用者的主组织。
	OrgID string `json:"orgId,omitempty"`
}

// EventIngestResponse 是 POST /v1/events 的响应体。
type EventIngestResponse struct {
	EventID  string `json:"eventId"`
	TraceID  string `json:"traceId"`
	Inserted bool   `json:"inserted"`
	// Tasks 是首轮派发建出来的任务（幂等命中时为空数组）。
	Tasks []TaskBrief `json:"tasks"`
	// Pruned 是首轮派发的剪枝记录（静默但留痕）。
	Pruned []PrunedBrief `json:"pruned"`
	// Origin 是这次事件被归到的**来源名**（"github"/"cron"/…）。
	//
	// 它曾经是"匹配到的起点能力 name@version"（#110 之前）。来源不再是一个
	// 能力，所以这里只是把请求里的 source 原样回显 —— 方便调用方确认
	// "我发的事件被归到哪个来源"，不再有"找不到起点能力"这种失败形态。
	Origin string `json:"origin,omitempty"`
}

// TaskBrief 是响应里的任务摘要。
type TaskBrief struct {
	ID         string `json:"id"`
	Capability string `json:"capability"`
	Version    int    `json:"version"`
	Status     string `json:"status"`
}

// PrunedBrief 是响应里的剪枝摘要。
type PrunedBrief struct {
	Reason       string `json:"reason"`
	CapabilityID string `json:"capabilityId,omitempty"`
	Port         string `json:"port,omitempty"`
	Detail       string `json:"detail"`
}

// ServeHTTP 处理 POST / GET /v1/events。
func (h *EventHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID, err := authn.Authenticate(r, h.Keys, authn.ScopeNotifySend)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, authn.ErrForbidden) {
			status = http.StatusForbidden
		}
		writeError(w, status, err.Error())
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.ingest(w, r, userID)
	case http.MethodGet:
		h.list(w, r, userID)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ingest 处理 POST /v1/events。
func (h *EventHandler) ingest(w http.ResponseWriter, r *http.Request, userID string) {
	var req EventIngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	// 必填校验要在落库前做：空 source/type 的事件没有任何路由意义
	// （source 就是起点能力名），存进去只会污染事件流。
	if strings.TrimSpace(req.Source) == "" {
		writeError(w, http.StatusBadRequest, "source is required")
		return
	}
	if strings.TrimSpace(req.Type) == "" {
		writeError(w, http.StatusBadRequest, "type is required")
		return
	}

	orgID, err := h.resolveOrg(r, userID, req.OrgID)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	var idemKey *string
	if req.IdemKey != "" {
		k := req.IdemKey
		idemKey = &k
	}

	res, err := h.Triggers.Ingest(r.Context(), &trigger.IngestRequest{
		OrgID:   orgID,
		Source:  req.Source,
		Type:    req.Type,
		IdemKey: idemKey,
		TraceID: req.TraceID,
		Payload: req.Payload,
	})

	// 事件已入库但派发/起源失败：仍然返回 200（对发送方而言事件收下了），
	// 但把问题写进响应的 origin 字段与日志——重投也还是同一条事件，不该让它 5xx。
	if res == nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp := EventIngestResponse{
		EventID:  res.Event.ID,
		TraceID:  res.Event.TraceID,
		Inserted: res.Inserted,
		Tasks:    []TaskBrief{},
		Pruned:   []PrunedBrief{},
	}
	// 来源名原样回显（不再需要去能力表里解析 name@version）
	if res.Event != nil {
		resp.Origin = res.Event.Source
	}
	if res.Dispatch != nil {
		for _, t := range res.Dispatch.Tasks {
			resp.Tasks = append(resp.Tasks, TaskBrief{
				ID: t.ID, Capability: t.Capability, Version: t.Version, Status: t.Status,
			})
		}
		for _, p := range res.Dispatch.Pruned {
			resp.Pruned = append(resp.Pruned, PrunedBrief{
				Reason: string(p.Reason), CapabilityID: p.CapabilityID,
				Port: p.Port, Detail: p.Detail,
			})
		}
	}

	if err != nil {
		// 派发失败（非起点问题）：事件已落库，记日志后照常返回 200
		slog.Warn("event dispatch failed after ingest",
			"event", "events.dispatch.failed",
			"event_id", resp.EventID,
			"trace_id", resp.TraceID,
			"error", err.Error(),
		)
	}

	writeJSON(w, http.StatusOK, resp)
}

// list 处理 GET /v1/events（按 trace 过滤）。
func (h *EventHandler) list(w http.ResponseWriter, r *http.Request, userID string) {
	orgID, err := h.resolveOrg(r, userID, r.URL.Query().Get("orgId"))
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	traceID := r.URL.Query().Get("traceId")
	source := r.URL.Query().Get("source")
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	events, err := h.Store.ListEvents(r.Context(), orgID, traceID, source, limit)
	if err != nil {
		slog.Error("list events failed", "event", "events.list.failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// 空列表返回 [] 而不是 null（契约 I9）
	if events == nil {
		events = []*store.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// resolveOrg 确定这次请求作用于哪个组织。
//
// 显式给了 orgId 时**必须**校验调用者是该组织成员（不变量 I10：
// 一切租户数据先过 org_id）；没给时用主组织。
func (h *EventHandler) resolveOrg(r *http.Request, userID, requested string) (string, error) {
	if requested != "" {
		ok, err := h.Store.IsOrgMember(r.Context(), requested, userID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New("not a member of the requested org")
		}
		return requested, nil
	}
	if h.DefaultOrgForUser != nil {
		return h.DefaultOrgForUser(userID)
	}
	orgID, err := h.Store.PrimaryOrgOf(r.Context(), userID)
	if err != nil {
		return "", errors.New("no org specified and user has no primary org")
	}
	return orgID, nil
}

// writeJSON 写一个 JSON 响应。
//
// 带 Cache-Control: no-store：事件与任务数据是用户私有且实时变化的，
// 中间缓存会把一个组织的响应喂给另一个组织。
//
// 原本在 api/notify.go 里，随旧通知终端模型（#80）搬到本文件：
// 事件中心只剩这一个 API 包，helper 不该再挂在已删除的文件名下。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// writeError 写一个 {"error": msg} 响应。
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
