package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
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
}

// EventIngestRequest 是 POST /v1/events 的请求体（camelCase，契约 §6.2）。
type EventIngestRequest struct {
	Source  string          `json:"source"`
	Type    string          `json:"type"`
	IdemKey string          `json:"idemKey,omitempty"`
	TraceID string          `json:"traceId,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// NamespaceID 是事件落进哪个命名空间（**必填**，契约 §6.2）。
	//
	// 缺了就是 400 —— 绝不回退默认命名空间：回退等于把「调用方写错了」
	// 藏成「悄悄写进了 default」，是最难查的那类串数据。
	// query 上的 namespaceId 是等价的第二来源（body 优先，见 resolveNamespace）。
	NamespaceID string `json:"namespaceId"`
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

	// 命名空间必填（契约 §6.2）：body 优先、其次 query、缺则 400。
	// 解析里同时过 AuthorizeNamespace 收口（见 resolveNamespace）。
	namespaceID, ok := h.resolveNamespace(w, r, userID, req.NamespaceID, false)
	if !ok {
		return
	}

	var idemKey *string
	if req.IdemKey != "" {
		k := req.IdemKey
		idemKey = &k
	}

	res, err := h.Triggers.Ingest(r.Context(), &trigger.IngestRequest{
		NamespaceID: namespaceID,
		Source:      req.Source,
		Type:        req.Type,
		IdemKey:     idemKey,
		TraceID:     req.TraceID,
		Payload:     req.Payload,
	})

	// 命名空间相关失败（I11 跨命名空间引用、缺参、不存在、别的组织）
	// 一律先翻成对应状态码：它们是**调用方**的问题，不是 5xx。
	if writeNamespaceError(w, err) {
		return
	}

	// 事件已入库但派发/起源失败：仍然返回 200（对发送方而言事件收下了），
	// 但把问题写进响应的 origin 字段与日志——重投也还是同一条事件，不该让它 5xx。
	if res == nil {
		msg := "ingest failed"
		if err != nil {
			msg = err.Error()
		}
		writeError(w, http.StatusBadRequest, msg)
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
//
// namespaceId 必填；`*` 是本端点允许的**读**语义：遍历本部署的全部命名空间合并，
// 再按 created_at desc 截断（契约 §4 / 设计文档 §6.2）。
func (h *EventHandler) list(w http.ResponseWriter, r *http.Request, userID string) {
	namespaceID, ok := h.resolveNamespace(w, r, userID, "", true)
	if !ok {
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

	var err error
	var events []*store.Event
	if namespaceID == store.AllNamespaces {
		events, err = h.listAcrossNamespaces(r, traceID, source, limit)
	} else {
		events, err = h.Store.ListEvents(r.Context(), namespaceID, traceID, source, limit)
	}
	if err != nil {
		if writeNamespaceError(w, err) {
			return
		}
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

// listAcrossNamespaces 是 namespaceId=* 的实现：逐个命名空间查、合并、按
// created_at desc（同秒按 id desc，与 store.ListEvents 的稳定序一致）截断到 limit。
//
// 为什么要合并后再截断：直接对每个命名空间各取 limit 再拼起来会返回 limit×N 行，
// 而且最新的一条可能在最后一个命名空间里——那等于「汇总视图」不汇总。
//
// 不引入 SQL UNION（store 层不接受 `*`）：跨命名空间读是**管理视图**，走 N 次
// 索引查询已经够快，而把它塞进 store 会让「`*` 只在 HTTP 层解释」这条边界消失。
func (h *EventHandler) listAcrossNamespaces(r *http.Request, traceID, source string, limit int) ([]*store.Event, error) {
	nss, err := h.Store.ListNamespaces(r.Context())
	if err != nil {
		return nil, err
	}
	merged := []*store.Event{}
	for _, ns := range nss {
		part, err := h.Store.ListEvents(r.Context(), ns.ID, traceID, source, limit)
		if err != nil {
			return nil, err
		}
		merged = append(merged, part...)
	}
	sortEventsNewestFirst(merged)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// sortEventsNewestFirst 按 created_at desc、id desc 排序（最新在前）。
//
// 与 store.ListEvents 的 ORDER BY 同语义：跨命名空间合并后必须重新排一次，
// 否则结果是「按命名空间分组」而不是「按时间」——汇总视图就失去意义。
func sortEventsNewestFirst(events []*store.Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].CreatedAt != events[j].CreatedAt {
			return events[i].CreatedAt > events[j].CreatedAt
		}
		return events[i].ID > events[j].ID
	})
}

// resolveNamespace 解析并校验这次请求作用于哪个命名空间（契约 §6.2）。
//
// 取值顺序：body 已解码的 namespaceId → query 的 namespaceId → 空。
// 空 → 400（**绝不回退默认命名空间**）；`*` → 只有 allowAll（读端点）才接受；
// 其余走 store.AuthorizeNamespace 收口：不存在 404，未登录 401。
//
// **安全收口写在这里，不要散到 handler 里**：今天 AuthorizeNamespace 等价于
// 「已登录 + 命名空间存在」；将来按命名空间分权限时，**只改 store.AuthorizeNamespace
// 并新增 namespace_members 表**，本函数的形状不变。
//
// 返回 (namespaceID, ok)：ok=false 表示已写过错误响应，调用方直接 return。
func (h *EventHandler) resolveNamespace(w http.ResponseWriter, r *http.Request, userID, bodyNS string, allowAll bool) (string, bool) {
	raw := strings.TrimSpace(bodyNS)
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("namespaceId"))
	}
	if raw == "" {
		writeError(w, http.StatusBadRequest, "namespaceId is required（命名空间资源端点必须显式指定，不回退默认命名空间）")
		return "", false
	}
	if raw == store.AllNamespaces {
		if !allowAll {
			writeError(w, http.StatusBadRequest, "namespaceId=* 只能用于跨命名空间汇总的读操作")
			return "", false
		}
		return store.AllNamespaces, true
	}
	// 唯一收口：已登录 + 命名空间存在（将来加 namespace_members 只改这一个函数）。
	if err := h.Store.AuthorizeNamespace(r.Context(), userID, raw); err != nil {
		writeNamespaceError(w, err)
		return "", false
	}
	return raw, true
}

// writeNamespaceError 把 store 的命名空间错误翻成 HTTP 状态码；returns true 表示已写响应。
//
// 跨命名空间引用（I11）一律 400 —— 不是静默忽略，也不是 500：它是**调用方的请求
// 错了**，静默忽略会让人以为配置生效了。这段映射与 internal/server 的同名逻辑
// 语义一致（那份在 console handler 里，api 包不跨包依赖 server，故本地复刻）。
//
// err == nil 时返回 false（什么都没写），这样调用方可以把它当成「是否是这类错误」
// 的判定直接串在正常流程里（见 ingest）。
func writeNamespaceError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var cross *store.CrossNamespaceError
	switch {
	case errors.As(err, &cross):
		writeError(w, http.StatusBadRequest, cross.Error())
	case errors.Is(err, store.ErrNamespaceNotFound):
		writeError(w, http.StatusNotFound, "namespace not found")
	case errors.Is(err, store.ErrAuthRequired):
		writeError(w, http.StatusUnauthorized, "authentication required")
	case errors.Is(err, store.ErrNamespaceRequired):
		writeError(w, http.StatusBadRequest, "namespaceId is required")
	default:
		return false
	}
	return true
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
