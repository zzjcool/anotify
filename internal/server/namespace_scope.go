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
// cluster 级读模型（`/v1/stats`、`/v1/console/events`）。
//
// # 一个必须显式处理的错位：路径里是 slug，store 里是 id
//
// `namespaces` 表有两列：`id`（不透明 `ns_<base62>`，所有事实表的 `namespace_id`
// 都引用它）与 `slug`（用户面稳定标识，URL 里用的就是它）。两者**永远不同值**，
// 所以 handler 拿到路径段后必须先解析成行，store 调用一律用 `ns.ID`，
// 响应 `metadata.namespace` 一律用 `ns.Slug`——混用会得到「创建成功但查不到」。
//
// 判据只有一份（越权与不存在的边界必须一致，否则同一个 id 在 A 端点 404、B 端点 403）：
//
//	未登录          → 401 Unauthorized
//	缺 ns           → 400 BadRequest
//	ns == "*"       → 400（路径）；`?namespace=*` 仅允许读端点且必须显式 allowAll
//	ns 不存在       → 404 NotFound
//
// 唯一收口是 store.AuthorizeNamespace：将来按 ns 分权限只改 store 一处，本文件形状不变。

// namespaceScope 解析路径 `{ns}` 并完成登录 + 存在性授权。
//
// 解析顺序：slug 优先，其次 id（id 兼容让「手写测试/CLI 传 ns_xxx」也能工作，
// 且不会与 slug 冲突——slug 正则不允许下划线，id 前缀是 `ns_`）。
func (d *serverDeps) namespaceScope(r *http.Request) (*store.Namespace, error) {
	return d.scopeFor(r.Context(), r.PathValue("ns"))
}

// authorizePathNamespace 是 namespaceScope 的「只要 store 用的 id」便捷版。
func (d *serverDeps) authorizePathNamespace(r *http.Request) (string, error) {
	ns, err := d.namespaceScope(r)
	if err != nil {
		return "", err
	}
	return ns.ID, nil
}

// authorizeQueryNamespace 解析 `?namespace=`（cluster 级读模型专用），返回 store id。
//
// allowAll=true 时接受 `*`（跨 ns 汇总读）：汇总视图必须先过登录这一关，否则
// `*` 就是绕过收口的后门。`*` 只允许出现在读端点。
func (d *serverDeps) authorizeQueryNamespace(r *http.Request, allowAll bool) (string, error) {
	raw := r.URL.Query().Get("namespace")
	if strings.TrimSpace(raw) == store.AllNamespaces {
		if auth.UserIDFromContext(r.Context()) == "" {
			return "", store.ErrAuthRequired
		}
		if !allowAll {
			return "", errStatus(http.StatusBadRequest, reasonBadRequest,
				"namespace * is only allowed for cross-namespace reads")
		}
		return store.AllNamespaces, nil
	}
	ns, err := d.scopeFor(r.Context(), raw)
	if err != nil {
		return "", err
	}
	return ns.ID, nil
}

// scopeFor 是全文件唯一的判据实现。
func (d *serverDeps) scopeFor(ctx context.Context, raw string) (*store.Namespace, error) {
	raw = strings.TrimSpace(raw)
	userID := auth.UserIDFromContext(ctx)
	if userID == "" {
		return nil, store.ErrAuthRequired
	}
	if raw == "" {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest,
			"namespace is required（不回退默认命名空间）")
	}
	if raw == store.AllNamespaces {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest,
			"namespace * is only allowed for cross-namespace reads")
	}
	ns, err := d.namespaceBySlugOrID(ctx, raw)
	if err != nil {
		return nil, err
	}
	if err := d.DB.AuthorizeNamespace(ctx, userID, ns.ID); err != nil {
		return nil, err
	}
	return ns, nil
}

// namespaceBySlugOrID 按 slug（优先）或 id 查命名空间行。
//
// store 没有导出的 by-slug 读入口（D0.9：本 issue 不新增 store 接口），而
// ListNamespaces 已经带了列表页需要的计数、且命名空间数量是「部署级」量级
// （个位数到几十），所以这里用一次列表查询解析，不做缓存：命名空间增删是低频操作，
// 缓存只会引入「新建的 ns 立刻 404」这类难查的时序 bug。
func (d *serverDeps) namespaceBySlugOrID(ctx context.Context, raw string) (*store.Namespace, error) {
	all, err := d.DB.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, ns := range all {
		if ns.Slug == raw {
			return ns, nil
		}
	}
	for _, ns := range all {
		if ns.ID == raw {
			return ns, nil
		}
	}
	return nil, store.ErrNamespaceNotFound
}

// requireUser 返回当前登录用户 id；未登录给 401。
func requireUser(ctx context.Context) (string, error) {
	userID := auth.UserIDFromContext(ctx)
	if userID == "" {
		return "", store.ErrAuthRequired
	}
	return userID, nil
}
