package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// 命名空间作用域解析（契约 §6.2 / 不变量 I10、I11）。
//
// 规则（没有例外、没有隐式默认）：
//
//	缺 namespaceId        → 400（**绝不回退默认命名空间**：回退等于把人写错的地方藏起来）
//	namespaceId=<具体 id>  → 过 store.AuthorizeNamespace 收口：不存在 404，未登录 401
//	namespaceId=*         → 显式跨命名空间汇总（读端点专用），**只在读端点允许**
//
// 为什么把「缺参」和「不存在」分成两个状态码：缺参是调用方写错了（改自己的代码），
// 不存在是资源没了（改数据）；混成 404 会让人以为"参数格式没问题、只是没找到"。
//
// #204 / #207 的变化：组织层删除后**没有「别人的命名空间」**了。原先把
// 「属于别的 org」映射成 403（读端点）或 404（写端点，防 id 探测）——那套口径
// 随迁移 14 一起消失：不存在就是 404，本部署内任何已登录用户都能访问任何存在的
// 命名空间。所以 resolveNamespaceCreate 这个「写端点用 404 防探测」的变体也一并删除。

// nsScope 是一次请求解析出的命名空间作用域。
type nsScope struct {
	// ID 是具体命名空间 id；All 为 true 时它是空串。
	ID string
	// All 表示 namespaceId=*：跨命名空间汇总（读端点专用）。
	All bool
}

// namespaceParam 取请求里的 namespaceId。
//
// query 优先，其次用调用方已解码的 body 字段（POST/PATCH/DELETE 的 body 里也可以带）。
// 两者都没有 → 空串（由 namespaceScope 变成 400）。
func namespaceParam(r *http.Request, bodyNS string) string {
	if v := strings.TrimSpace(r.URL.Query().Get("namespaceId")); v != "" {
		return v
	}
	return strings.TrimSpace(bodyNS)
}

var (
	// errNamespaceParamMissing / errNamespaceAllNotAllowed 是「调用方参数写错」的两条，
	// 消息即响应文案（必须与今天逐字一致，见契约 §6.2）。
	errNamespaceParamMissing  = errors.New("namespaceId is required（命名空间资源端点必须显式指定，不回退默认命名空间）")
	errNamespaceAllNotAllowed = errors.New("namespaceId=* 只能用于跨命名空间汇总的读操作")
)

// namespaceScope 是 resolveNamespace 的**唯一**规则实现。
//
// 规则只有一份的理由：「缺参 400 / `*` 视端点 400 / 不存在 404 / 未登录 401」这套判断
// 一旦在多个 handler 里各抄一遍，副本迟早分叉，最后表现为「同一个 namespaceId 在
// A 端点 404、在 B 端点 500」——调用方无从适配。这里只返回 error，**不写响应**，
// 由 resolveNamespace 统一挑错误映射。
//
// **安全收口在这里，不在各 handler 里。**
// 今天 store.AuthorizeNamespace 等价于「已登录 + 命名空间存在」；
// 将来要按命名空间分权限，**只改 store.AuthorizeNamespace 并新增 namespace_members 表**，
// 本函数与本文件其余部分形状不变。不要在 handler 里散写归属判断。
func (h *consoleHandler) namespaceScope(ctx context.Context, r *http.Request, bodyNS string, allowAll bool) (nsScope, error) {
	// 已登录是收口的第一环：`*` 汇总路径不查具体命名空间，也必须先过这一关，
	// 否则 `*` 就成了绕过收口的后门（未登录也能汇总全部署）。
	userID := auth.UserIDFromContext(ctx)
	if userID == "" {
		return nsScope{}, store.ErrAuthRequired
	}

	raw := namespaceParam(r, bodyNS)
	if raw == "" {
		return nsScope{}, errNamespaceParamMissing
	}
	if raw == store.AllNamespaces {
		if !allowAll {
			return nsScope{}, errNamespaceAllNotAllowed
		}
		// 汇总视图对「本部署任何已登录用户」都可见（#204 安全语义），
		// 所以这里只需要上面那一关，不需要再对某个 id 收口。
		return nsScope{All: true}, nil
	}
	// 唯一收口：已登录 + 命名空间存在（将来加 namespace_members 只改这一个函数）。
	if err := h.db.AuthorizeNamespace(ctx, userID, raw); err != nil {
		return nsScope{}, err
	}
	return nsScope{ID: raw}, nil
}

// resolveNamespace 解析并校验 namespaceId（读端点与写端点共用一份口径）。
//
// 缺参 400 / `*`（allowAll=false）400 / 不存在 404 / 未登录 401。
// 签名也保持不变——statsHandler / sourceHandler 以方法值注入它。
//
// allowAll=false 时（写端点、单对象端点）`*` 直接 400：把"跨命名空间"用在
// 写入或单对象读取上没有定义（写一条边属于哪个命名空间？）。
//
// 返回 (scope, ok)；ok=false 表示已经写过错误响应，调用方直接 return。
func (h *consoleHandler) resolveNamespace(w http.ResponseWriter, r *http.Request, bodyNS string, allowAll bool) (nsScope, bool) {
	ns, err := h.namespaceScope(r.Context(), r, bodyNS, allowAll)
	if err != nil {
		writeNamespaceScopeError(w, err)
		return nsScope{}, false
	}
	return ns, true
}

// writeNamespaceScopeError 把 namespaceScope 的 error 统一映射成响应。
//
// 参数回调已经删除：「写端点把跨命名空间报成 404 以免当 id 探测器」这套口径
// 属于组织层（#185 / 契约 D2）。组织层删除后不存在「别人的命名空间」，
// 不存在一律 404，探测与正常访问的回答本来就一致，没有第二套映射可挑。
func writeNamespaceScopeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNamespaceParamMissing), errors.Is(err, errNamespaceAllNotAllowed):
		// 文案就是响应体：这两条是「调用方参数写错」，改一个字都会破坏既有契约。
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeNamespaceError(w, err)
	}
}

// writeNamespaceError 把 store 的命名空间错误翻成 HTTP 状态码。
//
// 跨命名空间引用（I11）一律 400 —— 不是静默忽略，也不是 500：
// 它是**调用方的请求错了**，静默忽略会让人以为配置生效了。
func writeNamespaceError(w http.ResponseWriter, err error) {
	var cross *store.CrossNamespaceError
	switch {
	case errors.As(err, &cross):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": cross.Error()})
	case errors.Is(err, store.ErrNamespaceNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "namespace not found"})
	case errors.Is(err, store.ErrAuthRequired):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
	case errors.Is(err, store.ErrNamespaceRequired):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "namespaceId is required"})
	case errors.Is(err, store.ErrNamespaceExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "namespace slug already exists"})
	case errors.Is(err, store.ErrLastNamespace):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不允许删除最后一个命名空间"})
	default:
		var blocked *store.NamespaceBlockedError
		if errors.As(err, &blocked) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":    "namespace is not empty",
				"blocking": blocked.Blocking,
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}
