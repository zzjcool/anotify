package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// 命名空间作用域与授权（v4 资源路径口径）。
//
// v4 把命名空间放进路径（`/v1/namespaces/{namespace}/…`），所以「作用域从哪来」不再
// 需要 query/body 两种来源的兼容：路径段就是唯一来源，query `?namespace=` 只用于
// cluster 级读模型（`/v1/stats`、`/v1/console/events`、`/v1/orphan`）。
//
// 判据只有一份（越权与不存在的边界必须一致，否则同一个 id 在 A 端点 404、B 端点 403）：
//
//	未登录                        → 401 Unauthorized
//	路径/参数缺 ns                → 400 BadRequest
//	ns == "*"（路径)              → 400（单资源路径上跨 ns 没有定义）
//	ns 不存在                     → 404 NotFound
//
// 唯一收口是 store.AuthorizeNamespace：将来按 ns 分权限只改 store 一处，本文件形状不变。

// authorizePathNamespace 解析并授权 `{namespace}` 路径段。
//
// 返回的 namespaceID 就是 store 里用的命名空间 id（v4 里 slug 与 id 同值）。
func (d *serverDeps) authorizePathNamespace(r *http.Request) (string, error) {
	return authorizeNamespaceID(r.Context(), d.DB, r.PathValue("ns"), false)
}

// authorizeQueryNamespace 解析并授权 `?namespace=`（cluster 级读模型专用）。
//
// allowAll=true 时接受 `*`（跨 ns 汇总读）：汇总视图必须先过登录这一关，否则
// `*` 就是绕过收口的后门。`*` 只允许出现在读端点。
func (d *serverDeps) authorizeQueryNamespace(r *http.Request, allowAll bool) (string, error) {
	return authorizeNamespaceID(r.Context(), d.DB, r.URL.Query().Get("namespace"), allowAll)
}

// authorizeNamespaceID 是全文件唯一的判据实现。
func authorizeNamespaceID(ctx context.Context, db *store.DB, raw string, allowAll bool) (string, error) {
	ns := strings.TrimSpace(raw)
	if auth.UserIDFromContext(ctx) == "" {
		return "", store.ErrAuthRequired
	}
	if ns == "" {
		return "", errStatus(http.StatusBadRequest, reasonBadRequest,
			"namespace is required（不回退默认命名空间）")
	}
	if ns == store.AllNamespaces {
		if !allowAll {
			return "", errStatus(http.StatusBadRequest, reasonBadRequest, "namespace * is only allowed for cross-namespace reads")
		}
		return store.AllNamespaces, nil
	}
	if err := db.AuthorizeNamespace(ctx, auth.UserIDFromContext(ctx), ns); err != nil {
		return "", err
	}
	return ns, nil
}

// requireUser 返回当前登录用户 id；未登录给 401。
func requireUser(ctx context.Context) (string, error) {
	userID := auth.UserIDFromContext(ctx)
	if userID == "" {
		return "", store.ErrAuthRequired
	}
	return userID, nil
}
