package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/store"
)

// 命名空间作用域解析（契约 §6.2 / 不变量 I10、I11）。
//
// 规则（没有例外、没有隐式默认）：
//
//	缺 namespaceId            → 400（**绝不回退默认命名空间**：回退等于把人写错的地方藏起来）
//	namespaceId=<具体 id>     → 校验它属于当前 org：不存在 404，别的 org 403
//	namespaceId=*             → 显式跨命名空间汇总（管理员视图），**只在读端点允许**
//
// 为什么把「缺参」和「不存在」分成两个状态码：缺参是调用方写错了（改自己的代码），
// 不存在是资源没了（改数据）；混成 404 会让人以为"参数格式没问题、只是没找到"。

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

// namespaceScope 是 resolveNamespace / resolveNamespaceCreate 的**唯一**规则实现。
//
// 规则只有一份的理由：「缺参 400 / `*` 视端点 400 / 不存在 404 / 别的 org 403」这套判断
// 一旦在多个 handler 里各抄一遍，副本迟早分叉，最后表现为「同一个 namespaceId 在
// A 端点 404、在 B 端点 403」——调用方无从适配。这里只返回 error，**不写响应**，
// 由调用方挑自己的错误映射（读端点 403、写端点 404，见 resolveNamespaceCreate）。
func (h *consoleHandler) namespaceScope(ctx context.Context, r *http.Request, orgID, bodyNS string, allowAll bool) (nsScope, error) {
	raw := namespaceParam(r, bodyNS)
	if raw == "" {
		return nsScope{}, errNamespaceParamMissing
	}
	if raw == store.AllNamespaces {
		if !allowAll {
			return nsScope{}, errNamespaceAllNotAllowed
		}
		return nsScope{All: true}, nil
	}
	if _, err := h.db.GetNamespace(ctx, orgID, raw); err != nil {
		return nsScope{}, err
	}
	return nsScope{ID: raw}, nil
}

// resolveNamespace 解析并校验 namespaceId（读端点与既有写端点用）。
//
// 行为与抽 namespaceScope 之前**完全一致**：缺参 400 / `*`（allowAll=false）400 /
// 不存在 404 / 别的 org 403。签名也保持不变——statsHandler 以方法值注入它。
//
// allowAll=false 时（写端点、单对象端点）`*` 直接 400：把"跨命名空间"用在
// 写入或单对象读取上没有定义（写一条边属于哪个命名空间？）。
//
// 返回 (scope, ok)；ok=false 表示已经写过错误响应，调用方直接 return。
func (h *consoleHandler) resolveNamespace(w http.ResponseWriter, r *http.Request, orgID, bodyNS string, allowAll bool) (nsScope, bool) {
	ns, err := h.namespaceScope(r.Context(), r, orgID, bodyNS, allowAll)
	if err != nil {
		writeNamespaceScopeError(w, err, false)
		return nsScope{}, false
	}
	return ns, true
}

// resolveNamespaceCreate 解析并校验 namespaceId，**只给 POST /v1/nodes 用**。
//
// 规则与 resolveNamespace 同一份（namespaceScope），只有一处口径不同：
// 「别的 org」按 **404**，响应体与「不存在」逐字节相同（`{"error":"namespace not found"}`）。
// 理由：创建节点是写端点，若跨组织报 403 就等于给了一个**命名空间 id 存在性探测器**
// ——任何人拿候选 id 打一遍这个端点，就能分辨「这个 id 是别人的」还是「这个 id 不存在」
// （issue #185 / 契约 D2）。读端点保持 403（现有断言不削弱），两者都保留。
func (h *consoleHandler) resolveNamespaceCreate(w http.ResponseWriter, r *http.Request, orgID, bodyNS string) (nsScope, bool) {
	ns, err := h.namespaceScope(r.Context(), r, orgID, bodyNS, false)
	if err != nil {
		writeNamespaceScopeError(w, err, true)
		return nsScope{}, false
	}
	return ns, true
}

// writeNamespaceScopeError 把 namespaceScope 的 error 统一映射成响应。
//
// foreignAsNotFound=true 时 `store.ErrNamespaceForbidden` → 404 `namespace not found`
// （与「不存在」逐字节相同，见 resolveNamespaceCreate 的注释）。其余错误——包括
// 不存在、跨命名空间引用、数据库故障——原样交给 writeNamespaceError（该函数一字未改，
// 其它端点的 403 语义因此保持原样）。
func writeNamespaceScopeError(w http.ResponseWriter, err error, foreignAsNotFound bool) {
	switch {
	case errors.Is(err, errNamespaceParamMissing), errors.Is(err, errNamespaceAllNotAllowed):
		// 文案就是响应体：这两条是「调用方参数写错」，改一个字都会破坏既有契约。
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case foreignAsNotFound && errors.Is(err, store.ErrNamespaceForbidden):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "namespace not found"})
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
	case errors.Is(err, store.ErrNamespaceForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "namespace belongs to another org"})
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
