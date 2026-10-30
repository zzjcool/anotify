package server

import (
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
// 两者都没有 → 空串（由 resolveNamespace 变成 400）。
func namespaceParam(r *http.Request, bodyNS string) string {
	if v := strings.TrimSpace(r.URL.Query().Get("namespaceId")); v != "" {
		return v
	}
	return strings.TrimSpace(bodyNS)
}

// resolveNamespace 解析并校验 namespaceId。
//
// allowAll=false 时（写端点、单对象端点）`*` 直接 400：把"跨命名空间"用在
// 写入或单对象读取上没有定义（写一条边属于哪个命名空间？）。
//
// 返回 (scope, ok)；ok=false 表示已经写过错误响应，调用方直接 return。
func (h *consoleHandler) resolveNamespace(w http.ResponseWriter, r *http.Request, orgID, bodyNS string, allowAll bool) (nsScope, bool) {
	raw := namespaceParam(r, bodyNS)
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "namespaceId is required（命名空间资源端点必须显式指定，不回退默认命名空间）",
		})
		return nsScope{}, false
	}
	if raw == store.AllNamespaces {
		if !allowAll {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "namespaceId=* 只能用于跨命名空间汇总的读操作",
			})
			return nsScope{}, false
		}
		return nsScope{All: true}, true
	}
	if _, err := h.db.GetNamespace(r.Context(), orgID, raw); err != nil {
		writeNamespaceError(w, err)
		return nsScope{}, false
	}
	return nsScope{ID: raw}, true
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
