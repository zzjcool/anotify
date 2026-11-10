package server

import (
	"context"
	"net/http"
	"strings"
	"sync"

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

// resolveNamespaceForUser 是 ingress 入口用的「显式 userID」版本。
//
// 为什么要单独一个：API-key 投递入口的调用方身份是 key 的 owner（authn 解出来），
// 不经过会话中间件，所以 context 里没有 auth.UserIDFromContext 的值。判据仍然是
// 同一份（见 scopeFor），只是 userID 由调用方给。
func resolveNamespaceForUser(ctx context.Context, db *store.DB, userID, raw string) (*store.Namespace, error) {
	raw = strings.TrimSpace(raw)
	if userID == "" {
		return nil, store.ErrAuthRequired
	}
	// 这里必须用 store 的 sentinel 而不是 server 私有的 *apiError：调用方
	// （internal/api 的投递入口）只能映射 store 错误，否则一个「跨 ns 读 URL 被
	// 复用到投递」的调用会拿到 500 并污染服务端错误率（review N1 的回归）。
	if raw == "" {
		return nil, store.ErrNamespaceRequired
	}
	if raw == store.AllNamespaces {
		return nil, store.ErrNamespaceRequired
	}
	all, err := db.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	var ns *store.Namespace
	for _, cand := range all {
		if cand.Slug == raw {
			ns = cand
			break
		}
	}
	if ns == nil {
		for _, cand := range all {
			if cand.ID == raw {
				ns = cand
				break
			}
		}
	}
	if ns == nil {
		return nil, store.ErrNamespaceNotFound
	}
	if err := db.AuthorizeNamespace(ctx, userID, ns.ID); err != nil {
		return nil, err
	}
	return ns, nil
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
	if d.display != nil {
		d.display.put(ns.ID, ns.Slug)
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

// nsDisplay 是 namespace id → slug 的一次性缓存（见 serverDeps.display 的注释）。
type nsDisplay struct {
	mu sync.RWMutex
	m  map[string]string
}

func (c *nsDisplay) get(id string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[id]
	return v, ok
}

func (c *nsDisplay) put(id, slug string) {
	if id == "" || slug == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]string, 8)
	}
	c.m[id] = slug
}

// namespaceDisplay 返回命名空间的对外名字（slug）。
//
// **响应 metadata.namespace 必须是它**，不是 store 的 `ns_` id：契约 §3.2 的
// metadata.namespace 是命名空间的名字，客户端拿它跟路径/自己的配置对齐；回一个
// 内部 id 会让「同一资源在列表里和在 URL 里叫两个名字」。
func (d *serverDeps) namespaceDisplay(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if id == store.AllNamespaces {
		return store.AllNamespaces
	}
	if d != nil && d.display != nil {
		if slug, ok := d.display.get(id); ok {
			return slug
		}
	}
	ns, err := d.namespaceBySlugOrID(ctx, id)
	if err != nil || ns == nil {
		return id
	}
	if d != nil && d.display != nil {
		d.display.put(id, ns.Slug)
	}
	return ns.Slug
}

// requireUser 返回当前登录用户 id；未登录给 401。
func requireUser(ctx context.Context) (string, error) {
	userID := auth.UserIDFromContext(ctx)
	if userID == "" {
		return "", store.ErrAuthRequired
	}
	return userID, nil
}
