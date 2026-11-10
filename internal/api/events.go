package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/authn"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 v4 的**事件投递入口**（api-conventions §8.1 / source.md §5.1）。
//
//	POST /v1/namespaces/{namespace}/events
//
// 只有这一条路由。v3 的 `POST /v1/events`（body 里带 namespaceId）与
// `GET /v1/events`（按 trace 列事件）都已退役：
//
//   - 命名空间进路径（v4 的资源路径口径），不再有「body 里塞 namespaceId」的第二种写法；
//   - 事件列表是**资源**：`GET /v1/namespaces/{namespace}/events`（EventList 信封 +
//     ns 绑定的不透明 continue），由任务/事件资源域注册，不在本文件里实现。
//
// 鉴权是 **Bearer API Key**（scope notify:send），与控制台的会话 Cookie 是两套：
// 调用方是外部系统 / CLI，它们不带浏览器会话。key 的 owner 再走过
// store.AuthorizeNamespace 那一关（唯一收口：路径里的 ns 必须存在，将来按 ns 分权
// 只改 store 一处）。
//
// 投递语义全部在 `source.Service.Ingest` 里（同一份归一化配置 / 载荷装配 / 幂等窗口 /
// 剪枝留痕），本文件只做三件事：解析路径 + 鉴权、把 body 翻成 source.Input、把回执
// 原样写出去。**不要在 handler 里重算派发**——那会让「hooks / fire / events 三个入口
// 共用一份回执」这条纪律在第四个地方分叉。
type EventHandler struct {
	Sources *source.Service
	Keys    authn.KeyValidator

	// ResolveNamespace 把路径里的命名空间标识（对外是 slug，兼容 store id）解析成
	// store 事实表用的 namespace_id，并顺带完成「该 user 能否访问这个 ns」的收口。
	//
	// 为什么不在 api 包里自己查：slug→id 的解析判据必须只有一份（在 server 包的
	// namespace_scope.go 里）。若这里再抄一遍，两个入口对「default」与「ns_xxx」
	// 的接受度迟早分叉——症状是「同一个 ns，事件能投递但资源列表 404」。
	ResolveNamespace func(ctx context.Context, userID, raw string) (string, error)
}

// NewEventHandler 装配投递入口。
func NewEventHandler(sources *source.Service, keys authn.KeyValidator) *EventHandler {
	return &EventHandler{Sources: sources, Keys: keys}
}

// eventIngestRequest 是投递体（api-conventions §8.1 / source.md §5）。
//
//	source  必填：命名空间内的源名（不是 id、不是 token）
//	type    可选：事件类型；空 → 回落源名（source.md §5，免得调用方必须重复一遍）
//	id      可选：外部投递幂等键；保留窗口内同 id 只算一条事件
//	payload 可选：原始 JSON；空 → 只用源配置的打底载荷
type eventIngestRequest struct {
	Source  string          `json:"source"`
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

// ServeHTTP 处理 POST /v1/namespaces/{namespace}/events。
func (h *EventHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// 事件列表（GET）在资源域，本 handler 只承担 ingress：给 405 + Allow
		// 比让请求落到「路径不存在」更容易排障。
		w.Header().Set("Allow", "POST")
		writeAPIStatus(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "method not allowed")
		return
	}

	userID, err := authn.Authenticate(r, h.Keys, authn.ScopeNotifySend)
	if err != nil {
		// 401 与 403 必须分开：401 = 没带/令牌无效（去换一个），403 = 令牌有效但
		// scope 不够（去加 scope）。合并成一个码会让调用方在错误的方向上重试。
		if errors.Is(err, authn.ErrForbidden) {
			writeAPIStatus(w, http.StatusForbidden, reasonForbidden, err.Error())
			return
		}
		writeAPIStatus(w, http.StatusUnauthorized, reasonUnauthorized, err.Error())
		return
	}

	namespace := strings.TrimSpace(r.PathValue("namespace"))
	if namespace == "" {
		namespace = strings.TrimSpace(r.PathValue("ns"))
	}
	if namespace == "" {
		writeAPIStatus(w, http.StatusBadRequest, reasonBadRequest, "namespace is required")
		return
	}
	// 路径里是 slug，store 事实表引用 ns_ id：这一步是**必须**的换算，不是可选优化
	// （直接把 slug 当 namespace_id 会查不到命名空间/查不到源，或用错命名空间）。
	if h.ResolveNamespace != nil {
		resolved, rerr := h.ResolveNamespace(r.Context(), userID, namespace)
		if rerr != nil {
			writeAPIError(w, rerr)
			return
		}
		namespace = resolved
	}

	db := h.db()
	if db == nil {
		// 装配错误（没注入 Sources）：给 500 而不是 panic —— 一个没接线的端点不该
		// 让整个进程崩在一个外部请求上。
		slog.Error("event ingress is not wired: no store", "event", "events.ingress.unwired")
		writeAPIStatus(w, http.StatusInternalServerError, reasonInternal, "event ingress is not configured")
		return
	}
	// 唯一收口：已登录（key 的 owner）+ 命名空间存在。注入了解析器时它已做过这一步
	// （同一个 store.AuthorizeNamespace），没注入时（单测直接构造 handler）在这里补。
	if h.ResolveNamespace == nil {
		if err := db.AuthorizeNamespace(r.Context(), userID, namespace); err != nil {
			writeAPIError(w, err)
			return
		}
	}

	var req eventIngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		// 4 MiB 是 source.md §5 的投递上限（hooks 入口同一条），超出时 body 读取
		// 会失败：这里统一 400 —— 413 的细分留给 hooks 的限流/大小测试，ingress
		// 不额外造一套上限语义。
		writeAPIStatus(w, http.StatusBadRequest, reasonBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	sourceName := strings.TrimSpace(req.Source)
	if sourceName == "" {
		// source 必填，且**不回退**任何默认值：源名决定「谁的路由模板生效」，
		// 猜一个等于把调用方的拼写错误变成另一条流水线上的真实事件。
		writeAPIStatus(w, http.StatusBadRequest, reasonBadRequest, "source is required")
		return
	}

	src, err := db.GetSourceByName(r.Context(), namespace, sourceName)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !src.Enabled {
		// 停用的源不接收投递（source.md §5 / §5.2）。与「源不存在」同码不同文案：
		// 调用方已经通过 ns 授权，知道这个 ns 里有哪些源，区分文案不泄露任何东西。
		writeAPIStatus(w, http.StatusNotFound, reasonNotFound, "source is disabled")
		return
	}

	receipt, err := h.Sources.Ingest(r.Context(), source.Input{
		Source:     src,
		Type:       req.Type,
		DeliveryID: req.ID,
		Payload:    req.Payload,
	})
	if err != nil {
		if errors.Is(err, source.ErrBadPayload) {
			writeAPIStatus(w, http.StatusBadRequest, reasonBadRequest, err.Error())
			return
		}
		writeAPIError(w, err)
		return
	}
	if receipt == nil {
		slog.Error("event ingest returned no receipt", "event", "events.ingress.no_receipt",
			"namespace", namespace, "source", sourceName)
		writeAPIStatus(w, http.StatusInternalServerError, reasonInternal, "ingest produced no receipt")
		return
	}

	// 空列表恒 `[]`（I12）：receiptFromStore 已归一，这里再保一次底
	// （装配层是唯一出口，不能依赖「上游永远会给非 nil」）。
	if receipt.Tasks == nil {
		receipt.Tasks = []source.TaskRef{}
	}
	if receipt.Pruned == nil {
		receipt.Pruned = []route.Prune{}
	}
	writeJSON(w, http.StatusOK, receipt)
}

// db 取出 Ingest 用的同一份 store。
//
// 为什么绕一层：v4 冻结的装配形状是 `EventHandler{Sources, Keys}`（不含 *store.DB），
// 而「按名字取源行」与「ns 授权」都需要库。`source.Service` 只暴露 `Router()`，而
// Router 持有的正是编排层那一个 *store.DB（`route.Router.DB()`）——用同一条连接保证
// 「handler 里看到源行」与「Ingest 里写事件」是同一份事实，不是两份缓存。
func (h *EventHandler) db() *store.DB {
	if h == nil || h.Sources == nil {
		return nil
	}
	router := h.Sources.Router()
	if router == nil {
		return nil
	}
	return router.DB()
}

// ---------- Status（api 包不能 import server：这里是最小的一份）

// reason 字面量与 internal/server/api_status.go 的封闭集合逐字一致。
//
// 为什么复制而不 import：api 包是**被 server 依赖**的一侧（server → api），反向
// import 会成一个环。Status 的形状与 reason 词表是 api-conventions §4 的公开契约，
// 两处各写一份字符串常量不会漂移（测试会盯住响应体），但少一个环。
const (
	apiVersionV1 = "anotify/v1"
	kindStatus   = "Status"

	reasonBadRequest        = "BadRequest"
	reasonUnauthorized      = "Unauthorized"
	reasonForbidden         = "Forbidden"
	reasonNotFound          = "NotFound"
	reasonMethodNotAllowed  = "MethodNotAllowed"
	reasonInternal          = "InternalError"
	reasonNamespaceMismatch = "namespace_mismatch"
)

// statusDoc 是统一错误体（api-conventions §4.1）。
//
// 只带 ingress 会用到的字段：details 属于 CAS / 冲突类端点，投递入口不产生它。
type statusDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Code       int    `json:"code"`
	Reason     string `json:"reason"`
	Message    string `json:"message,omitempty"`
}

// writeAPIStatus 写一个 Status 响应（失败一律带 body：§4.1 不允许空 body 的错误）。
func writeAPIStatus(w http.ResponseWriter, code int, reason, message string) {
	writeJSON(w, code, statusDoc{
		APIVersion: apiVersionV1,
		Kind:       kindStatus,
		Status:     "Failure",
		Code:       code,
		Reason:     reason,
		Message:    message,
	})
}

// writeAPIError 把 store / 领域 sentinel 翻成 Status；未知错误一律 500。
//
// 与 internal/server.writeAPIError 的映射保持同口径（同一类错误在两个入口不能给
// 两个码）：只覆盖 ingress 真的会遇到的那几个。
func writeAPIError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	var cross *store.CrossNamespaceError
	if errors.As(err, &cross) {
		writeAPIStatus(w, http.StatusBadRequest, reasonNamespaceMismatch, cross.Error())
		return
	}
	switch {
	case errors.Is(err, store.ErrSourceNotFound):
		writeAPIStatus(w, http.StatusNotFound, reasonNotFound, "source not found")
	case errors.Is(err, store.ErrAuthRequired):
		writeAPIStatus(w, http.StatusUnauthorized, reasonUnauthorized, "authentication required")
	case errors.Is(err, store.ErrNamespaceRequired):
		writeAPIStatus(w, http.StatusBadRequest, reasonBadRequest, "namespace is required")
	case errors.Is(err, store.ErrNamespaceNotFound):
		writeAPIStatus(w, http.StatusNotFound, reasonNotFound, "namespace not found")
	case errors.Is(err, store.ErrNotFound):
		writeAPIStatus(w, http.StatusNotFound, reasonNotFound, "not found")
	case errors.Is(err, store.ErrSourceEventReceiptMissing):
		// 回执缺行是**服务端**的不变量破了（写事件与写回执同事务），必须 5xx。
		slog.Error("source event receipt missing", "event", "events.ingress.receipt_missing", "err", err.Error())
		writeAPIStatus(w, http.StatusInternalServerError, reasonInternal, err.Error())
	default:
		slog.Error("event ingress failed", "event", "events.ingress.failed", "err", err.Error())
		writeAPIStatus(w, http.StatusInternalServerError, reasonInternal, err.Error())
	}
}

// writeJSON 写一个 JSON 响应。
//
// 带 Cache-Control: no-store：事件与任务数据是用户私有且实时变化的，
// 中间缓存会把一个组织的响应喂给另一个组织。
//
// 故意**不缩进**：客户端是 JSON parser 不是人眼，缩进对可解析性零收益，
// 只是让投递回执这类响应白白多出近三成体积（#243 实测 54,163B → 41,855B）。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
