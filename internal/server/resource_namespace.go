package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/store"
)

// 命名空间资源域 + 配置子资源 + 聚合计数（v4 契约 D2 / api-conventions §7.5 / W5）。
//
// 作用域分档（§5.2）：`/v1/namespaces…` 与 `/v1/system/config` 都是 **cluster** 级
// ——命名空间对象不属于任何命名空间，问「这个 ns 属于哪个 ns」是自指。所以这一域
// 的路径上没有 `{namespace}` 段，只要**登录**（cluster 写入口不能被匿名调用）。
//
// 身份口径（§7.5）：`metadata.name` = 原 `slug`，路径 `{name}` 也是 slug；
// 库里 `namespaces.id` 是另一个不透明值（`ns_…`），只出现在 `metadata.uid` 与
// `resourceVersion` 的 rv1 令牌里。store 没有公开的 by-slug 读入口（D0.9 不新增），
// 所以 slug → 行 的解析统一走 `ListNamespaces`：它一次给出「行 + 派生计数」，
// 顺便保证单对象读与列表读用的是**同一份计数口径**（否则列表说 3 个能力、
// 详情说 0 个，前端会以为刚被删光）。

const (
	kindNamespace     = "Namespace"
	kindNamespaceList = "NamespaceList"
)

// namespaceSlugRe 是 `metadata.name`（Namespace 的 slug）的合法字符集。
//
// 权威定义在 store（namespaceSlugPattern：它出现在 URL 与令牌作用域里），这里只是
// 同一规则的复制。为什么必须复制一份：store.CreateNamespace 对非法 slug 返回的是
// **普通 error（没有哨兵）**，不在这里先拦一次就会落到 writeAPIError 的兜底 500
// ——把调用方的输入错报成服务端故障。两处必须同时改。
var namespaceSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// namespaceSpec 是 Namespace 的 `spec`：**空对象**。
//
// V4-D1 明确不做「建命名空间时播种/开关内置能力」这类功能，所以 spec 没有任何字段。
// 这里仍然把非 nil 的 `*namespaceSpec` 交给 decodeWriteEnvelope：只有给了 specDst，
// 严格解码才会检查 `spec.seedBuiltinCapabilities` 这类未知字段并回 400
// unknown_field（specDst 为 nil 时整段 spec 被跳过，等于静默忽略——那比报错危险）。
type namespaceSpec struct{}

// namespaceCounts 是 `Namespace.status.counts`（§7.5）：四项真实计数。
type namespaceCounts struct {
	Capabilities int `json:"capabilities"`
	Nodes        int `json:"nodes"`
	Sources      int `json:"sources"`
	Tasks        int `json:"tasks"`
}

// namespaceAggregate 是 `Namespace.status.stats`（§7.5「计数 / stats（派生）」）。
//
// 全是派生量，不落库：tasksByPhase 来自 CountTasksByPhase，edges 是能力/源投影的
// 派生边数（store.ListNamespaces 已经算好，不在这里重算第二份口径）。
type namespaceAggregate struct {
	TasksByPhase map[string]int `json:"tasksByPhase"`
	Edges        int            `json:"edges"`
}

// namespaceStatus 是 Namespace 的只读派生面。
type namespaceStatus struct {
	Counts namespaceCounts    `json:"counts"`
	Stats  namespaceAggregate `json:"stats"`
}

// configMeta 是配置 wrapper 的 metadata（§3.1 例外：只有 resourceVersion，没有 spec/status）。
type configMeta struct {
	ResourceVersion string `json:"resourceVersion"`
}

// namespaceConfigDoc 是命名空间配置 wrapper（§7.5 / config.md §3）：
//
//	{ "metadata": { "resourceVersion": "rv1:ns_x:3" }, "vars": {}, "namespaceVarsRevision": "1" }
//
// `namespaceVarsRevision` 是**配置生效版本**，与 resourceVersion（写并发令牌）不是
// 同一个计数器（C18），所以压成字符串只读给客户端，PUT 不接。
type namespaceConfigDoc struct {
	Metadata              configMeta        `json:"metadata"`
	Vars                  map[string]string `json:"vars"`
	NamespaceVarsRevision string            `json:"namespaceVarsRevision"`
}

// retentionDoc 是系统保留期（config.md §3.6）。0 = 永久保留，所以「缺字段」必须与
// 「显式 0」区分开：写请求用指针接，缺字段 → 400（见 retentionWrite）。
type retentionDoc struct {
	EventTTLSec  int64 `json:"eventTtlSec"`
	TaskTTLSec   int64 `json:"taskTtlSec"`
	ResultTTLSec int64 `json:"resultTtlSec"`
}

// systemConfigDoc 是系统配置 wrapper（§7.5 / config.md §3.6），不是资源、不套四键。
type systemConfigDoc struct {
	Metadata  configMeta   `json:"metadata"`
	Retention retentionDoc `json:"retention"`
}

// retentionWrite 是 PUT 的 retention 入参：三个字段都必填。
//
// 为什么用指针：0 在保留期里是**有语义的常量**（永久保留）。若用 int64 接，
// 少写一个字段就会被读成 0 —— 一次漏字段的 PUT 悄悄把事件保留期改成「永久」，
// 磁盘上只会表现为「怎么不降了」。显式 0 仍然允许，但必须写出来。
type retentionWrite struct {
	EventTTLSec  *int64 `json:"eventTtlSec"`
	TaskTTLSec   *int64 `json:"taskTtlSec"`
	ResultTTLSec *int64 `json:"resultTtlSec"`
}

// namespaceAPI 是命名空间域的全部 handler。
type namespaceAPI struct{ d *serverDeps }

// newNamespaceAPI 构造命名空间域 handler。
func newNamespaceAPI(d *serverDeps) *namespaceAPI { return &namespaceAPI{d: d} }

// register 把命名空间域挂到 mux 上：全部会话保护 + no-store（动态 API 不缓存）。
func (a *namespaceAPI) register(mux *http.ServeMux) {
	mux.Handle("GET /v1/namespaces", noStore(a.d.session(http.HandlerFunc(a.list))))
	mux.Handle("POST /v1/namespaces", noStore(a.d.session(http.HandlerFunc(a.create))))
	mux.Handle("GET /v1/namespaces/{name}", noStore(a.d.session(http.HandlerFunc(a.get))))
	mux.Handle("PATCH /v1/namespaces/{name}", noStore(a.d.session(http.HandlerFunc(a.patch))))
	mux.Handle("DELETE /v1/namespaces/{name}", noStore(a.d.session(http.HandlerFunc(a.remove))))
	mux.Handle("GET /v1/namespaces/{name}/config", noStore(a.d.session(http.HandlerFunc(a.getConfig))))
	mux.Handle("PUT /v1/namespaces/{name}/config", noStore(a.d.session(http.HandlerFunc(a.putConfig))))
	mux.Handle("GET /v1/namespaces/{name}/stats", noStore(a.d.session(http.HandlerFunc(a.getStats))))
	mux.Handle("GET /v1/system/config", noStore(a.d.session(http.HandlerFunc(a.getSystemConfig))))
	mux.Handle("PUT /v1/system/config", noStore(a.d.session(http.HandlerFunc(a.putSystemConfig))))
}

// ---------- 信封与派生计数 ----------

// namespaceMeta 把 store 行折成信封 metadata（§3.2）。
//
// uid 就是行 id（不透明、永不复用）；resourceVersion 是 rv1 令牌（绑定 uid + 写修订，
// 所以「删了重建同名 ns」拿不到旧令牌）。displayName 就是旧的 name 列 —— slug 是
// 身份（metadata.name），展示名是可改的那一半。
func namespaceMeta(ns *store.Namespace) objectMeta {
	return objectMeta{
		Name:              ns.Slug,
		UID:               ns.ID,
		ResourceVersion:   store.ResourceVersion(ns.ID, ns.ResourceVersion),
		CreationTimestamp: rfc3339(ns.CreatedAt),
		DisplayName:       ns.Name,
	}
}

// rfc3339 把 store 的**秒级**时间戳转成信封的 creationTimestamp（§3.2：string(RFC3339)）。
func rfc3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// statusOf 组装 Namespace.status（§7.5）：四项计数 + 派生聚合，全部真实值。
//
// ns 必须来自 ListNamespaces（它带 CapabilityCount / NodeCount / SourceCount / EdgeCount）；
// GetNamespace 只读身份列，计数恒 0 —— 直接拿它填 status 会把「有 12 个能力」报成 0。
func (a *namespaceAPI) statusOf(ctx context.Context, ns *store.Namespace) (namespaceStatus, error) {
	phases, err := a.d.DB.CountTasksByPhase(ctx, ns.ID, "")
	if err != nil {
		return namespaceStatus{}, err
	}
	tasks := 0
	for _, n := range phases {
		tasks += n
	}
	return namespaceStatus{
		Counts: namespaceCounts{
			Capabilities: ns.CapabilityCount,
			Nodes:        ns.NodeCount,
			Sources:      ns.SourceCount,
			Tasks:        tasks,
		},
		Stats: namespaceAggregate{TasksByPhase: phases, Edges: ns.EdgeCount},
	}, nil
}

// counted 取「带派生计数」的命名空间行（传进来的行可以只带身份列）。
//
// 走列表查询而不是新增 store 入口（D0.9）：一套部署的命名空间是「几十个」量级，
// 这点开销换来单对象读、PATCH 响应与列表读**同一份计数口径**。
func (a *namespaceAPI) counted(ctx context.Context, ns *store.Namespace) (*store.Namespace, error) {
	list, err := a.d.DB.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, cand := range list {
		if cand.ID == ns.ID {
			return cand, nil
		}
	}
	// 列表里没有 = 刚被并发删掉（不存在与 CAS 失败必须分开报，这里按不存在）。
	return nil, store.ErrNamespaceNotFound
}

// namespaceSlugFromPath 取路径上的命名空间名（§7.5：{name} 就是 slug）。
func namespaceSlugFromPath(r *http.Request) (string, error) {
	slug := strings.TrimSpace(r.PathValue("name"))
	if slug == "" {
		return "", errStatus(http.StatusBadRequest, reasonBadRequest, "namespace name is required")
	}
	return slug, nil
}

// resolve 解析并授权路径指向的命名空间，返回带派生计数的行。
//
// 判据顺序（未登录 401 → 不存在 404）与 namespace_scope.go 的收口一致：
// 先要登录（cluster 资源也不能匿名读），再判存在，最后把**行 id** 交给
// store.AuthorizeNamespace —— 授权收口吃的是 id，不是 slug（把 slug 传进去会
// 让每个真实命名空间都变成 404，因为幂等键是 `ns_…` 那一列）。
//
// 解析用 namespaceBySlugOrIDOn（slug 优先、兼容 id）：资源路径（`/v1/namespaces/{ns}/…`）
// 已经接受两种写法，Namespace 自身的 CRUD 如果只认 slug，就会出现「同一个 ns：
// `/ns_xxx/capabilities` 200、`/ns_xxx` 404」的不对称。读写路径必须同一份判据。
func (a *namespaceAPI) resolve(r *http.Request) (*store.Namespace, error) {
	raw, err := namespaceSlugFromPath(r)
	if err != nil {
		return nil, err
	}
	userID, err := requireUser(r.Context())
	if err != nil {
		return nil, err
	}
	ns, err := namespaceBySlugOrIDOn(r.Context(), a.d.DB, raw)
	if err != nil {
		return nil, err
	}
	if err := a.d.DB.AuthorizeNamespace(r.Context(), userID, ns.ID); err != nil {
		return nil, err
	}
	return ns, nil
}

// ---------- Namespace CRUD ----------

// list 处理 GET /v1/namespaces：cluster 级列表（只要登录，不需要 ns 授权）。
func (a *namespaceAPI) list(w http.ResponseWriter, r *http.Request) {
	if _, err := requireUser(r.Context()); err != nil {
		writeAPIError(w, err)
		return
	}
	list, err := a.d.DB.ListNamespaces(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	items := make([]apiEnvelope, 0, len(list))
	for _, ns := range list {
		status, serr := a.statusOf(r.Context(), ns)
		if serr != nil {
			writeAPIError(w, serr)
			return
		}
		items = append(items, newEnvelope(kindNamespace, namespaceMeta(ns), namespaceSpec{}, status))
	}
	writeList(w, http.StatusOK, kindNamespaceList, listMeta{}, items)
}

// create 处理 POST /v1/namespaces：body `metadata.name`(slug) + `metadata.displayName`。
//
// 这里显式拒绝 `spec.*`（含 V4-D1 明确不做的 seedBuiltinCapabilities 开关）：
// specDst 非 nil，严格解码才会把未知 spec 字段变成 400。
func (a *namespaceAPI) create(w http.ResponseWriter, r *http.Request) {
	if _, err := requireUser(r.Context()); err != nil {
		writeAPIError(w, err)
		return
	}
	meta, err := decodeWriteEnvelope(r, kindNamespace, &namespaceSpec{})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	slug, err := requireMetaName(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !namespaceSlugRe.MatchString(slug) {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid,
			`metadata.name must match ^[a-z0-9][a-z0-9-]{0,30}$ (it is the namespace slug used in URLs)`))
		return
	}
	// displayName 空 = 由 store 回落到 slug（与 v3 一致：name 是纯展示，不是身份）。
	ns, err := a.d.DB.CreateNamespace(r.Context(), slug, strings.TrimSpace(meta.DisplayName))
	if err != nil {
		writeAPIError(w, err) // slug 已占用 → store.ErrNamespaceExists → 409 AlreadyExists
		return
	}
	status, err := a.statusOf(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusCreated, newEnvelope(kindNamespace, namespaceMeta(ns), namespaceSpec{}, status))
}

// get 处理 GET /v1/namespaces/{name}：单对象信封。
func (a *namespaceAPI) get(w http.ResponseWriter, r *http.Request) {
	ns, err := a.resolve(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	status, err := a.statusOf(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, newEnvelope(kindNamespace, namespaceMeta(ns), namespaceSpec{}, status))
}

// patch 处理 PATCH /v1/namespaces/{name}：**只改 metadata.displayName**。
//
// slug 是身份（URL + 令牌作用域），改它等于换一个对象，所以带不同 metadata.name
// 一律 400 name_immutable —— 而不是静默忽略（"我改了但没生效"比"你不能改"难查得多）。
// CAS 令牌只认 body `metadata.resourceVersion`（W1）。
func (a *namespaceAPI) patch(w http.ResponseWriter, r *http.Request) {
	if _, err := namespaceSlugFromPath(r); err != nil {
		writeAPIError(w, err)
		return
	}
	// PATCH 的 CAS 令牌只在 body `metadata.resourceVersion`（W1），所以显式把它加进
	// metadata 白名单：decodeWriteEnvelope 默认只认 name / displayName。
	// （Namespace 是 cluster 级对象，metadata.namespace 不在白名单里 → 400 unknown_field，
	// 这正是我们要的：它本来就不存在「属于哪个 ns」这个字段。）
	meta, err := decodeWriteEnvelope(r, kindNamespace, &namespaceSpec{}, "resourceVersion")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// resolve 同时完成 slug-or-id 解析 + 授权：写路径也必须对称地接受 id，
	// 并且 store 写入口只认 slug，所以后续一律用行上的 ns.Slug（不用路径原文）。
	resolved, err := a.resolve(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if meta.Name != "" && meta.Name != resolved.Slug {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonNameImmutable,
			fmt.Sprintf("metadata.name %q does not match path namespace %q (slug is immutable)", meta.Name, resolved.Slug)))
		return
	}
	token, err := casTokenFromMeta(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	displayName := strings.TrimSpace(meta.DisplayName)
	if displayName == "" {
		// displayName 是唯一可写字段：没给就不是一次「更新」。
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "metadata.displayName is required"))
		return
	}
	ns, err := a.d.DB.UpdateNamespace(r.Context(), resolved.Slug, displayName, token)
	if err != nil {
		writeAPIError(w, err) // 缺 CAS 400 / 旧值 409 + details.current / 不存在 404
		return
	}
	counted, err := a.counted(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	status, err := a.statusOf(r.Context(), counted)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, newEnvelope(kindNamespace, namespaceMeta(counted), namespaceSpec{}, status))
}

// remove 处理 DELETE /v1/namespaces/{name}?resourceVersion=…&force=true。
//
// 删除语义沿用现有实现（§7.5：不做新的级联协议）：默认档里还有对象 → 409 并列出
// 阻塞清单；force=true 才是显式级联；最后一个命名空间不能删（400）。
// CAS 令牌只在 query（DELETE 没有请求体），缺失 400、旧值 409、不存在 404。
func (a *namespaceAPI) remove(w http.ResponseWriter, r *http.Request) {
	if _, err := namespaceSlugFromPath(r); err != nil {
		writeAPIError(w, err)
		return
	}
	token, err := casTokenFromQuery(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	force := r.URL.Query().Get("force")
	ns, err := a.resolve(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// store 删入口只认 slug：路径可能给的是 id（对称解析），这里必须用行上的 slug。
	if err := a.d.DB.DeleteNamespaceByName(r.Context(), ns.Slug, force == "true" || force == "1", token); err != nil {
		writeAPIError(w, err)
		return
	}
	a.d.logger().Info("namespace deleted",
		"event", "console.namespaces.deleted",
		"namespace", ns.Slug, "uid", ns.ID, "force", force,
	)
	// 删除没有对象可回：204 无响应体（与能力/源/节点域一致，客户端只看状态码）。
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 命名空间配置子资源（W5） ----------

// getConfig 处理 GET /v1/namespaces/{name}/config。
func (a *namespaceAPI) getConfig(w http.ResponseWriter, r *http.Request) {
	ns, err := a.resolve(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	cfg, err := a.d.DB.GetNamespaceConfig(r.Context(), ns.ID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, namespaceConfigDocOf(cfg))
}

// putConfig 处理 PUT /v1/namespaces/{name}/config：只接 metadata.resourceVersion + vars。
//
// CAS 令牌与配置生效版本是**两个**计数器（C18）：令牌是 store.ResourceVersion(uid, rev)
// 的 rv1 形式（写并发令牌），namespaceVarsRevision 是 namespace_configs.revision
// （配置生效版本），每次成功写入 +1 且只读 —— 两者不能压成一个。
func (a *namespaceAPI) putConfig(w http.ResponseWriter, r *http.Request) {
	ns, err := a.resolve(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	var vars map[string]string
	meta, err := decodeConfigWrite(r, "NamespaceConfig", "vars", &vars)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if vars == nil {
		// PUT 是整份替换：`vars` 缺席会被读成「清空所有 vars」，与「忘了写」无法区分。
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid,
			"vars is required (PUT replaces the whole namespace vars map; send {} to clear)"))
		return
	}
	token, err := casTokenFromConfig(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	cfg, err := a.d.DB.PutNamespaceConfigByName(r.Context(), ns.Slug, vars, token)
	if err != nil {
		writeAPIError(w, err) // value_invalid 400 / 旧令牌 409 + details.current
		return
	}
	writeJSON(w, http.StatusOK, namespaceConfigDocOf(cfg))
}

// namespaceConfigDocOf 把 store 的 NamespaceConfig 折成 wrapper（§7.5）。
func namespaceConfigDocOf(cfg *store.NamespaceConfig) namespaceConfigDoc {
	vars := cfg.Vars
	if vars == nil {
		vars = map[string]string{}
	}
	return namespaceConfigDoc{
		Metadata: configMeta{
			ResourceVersion: store.ResourceVersion(cfg.NamespaceID, cfg.ResourceVersion),
		},
		Vars:                  vars,
		NamespaceVarsRevision: strconv.FormatInt(cfg.Revision, 10),
	}
}

// ---------- 聚合计数子资源 ----------

// getStats 处理 GET /v1/namespaces/{name}/stats：返回 `Namespace.status` 同一份派生聚合。
//
// 它是读模型（§3.1 例外），不是资源：没有 spec，也不再套四键 —— 内容与对象信封里的
// `status` 逐字段一致，前端两处可以复用同一个渲染器。
func (a *namespaceAPI) getStats(w http.ResponseWriter, r *http.Request) {
	ns, err := a.resolve(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	status, err := a.statusOf(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// ---------- 系统配置（cluster 级） ----------

// getSystemConfig 处理 GET /v1/system/config（config.md §3.6）。
func (a *namespaceAPI) getSystemConfig(w http.ResponseWriter, r *http.Request) {
	if _, err := requireUser(r.Context()); err != nil {
		writeAPIError(w, err)
		return
	}
	cfg, err := a.d.DB.GetSystemConfig(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, systemConfigDocOf(cfg))
}

// putSystemConfig 处理 PUT /v1/system/config：只接 metadata.resourceVersion + retention。
func (a *namespaceAPI) putSystemConfig(w http.ResponseWriter, r *http.Request) {
	if _, err := requireUser(r.Context()); err != nil {
		writeAPIError(w, err)
		return
	}
	var retention retentionWrite
	meta, err := decodeConfigWrite(r, "SystemConfig", "retention", &retention)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	rv, err := parseSystemResourceVersion(meta.ResourceVersion)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if retention.EventTTLSec == nil || retention.TaskTTLSec == nil || retention.ResultTTLSec == nil {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid,
			"retention.eventTtlSec / retention.taskTtlSec / retention.resultTtlSec are all required (0 means keep forever, send it explicitly)"))
		return
	}
	cfg := &store.SystemConfig{
		RetentionEventTTLSec:  *retention.EventTTLSec,
		RetentionTaskTTLSec:   *retention.TaskTTLSec,
		RetentionResultTTLSec: *retention.ResultTTLSec,
	}
	if err := a.d.DB.PutSystemConfig(r.Context(), cfg, rv); err != nil {
		// 单行表的冲突没有自带 current 令牌（PutSystemConfig 只回包装过的哨兵），
		// 按 W1 重读一次补上：客户端拿不到当前令牌就只能猜。
		if errors.Is(err, store.ErrResourceVersionConflict) {
			if cur, cerr := a.d.DB.GetSystemConfig(r.Context()); cerr == nil {
				writeStatus(w, http.StatusConflict, reasonResourceVersionConflict, "resourceVersion is stale",
					&statusDetails{Current: map[string]any{"resourceVersion": systemResourceVersion(cur.ResourceVersion)}})
				return
			}
		}
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, systemConfigDocOf(cfg))
}

// casTokenFromConfig 取配置 wrapper 的 CAS 令牌。
//
// 承载位置与资源写请求完全一致（body `metadata.resourceVersion`，W1），所以直接复用
// casTokenFromMeta 的判据与文案：两处各写一份 message 迟早会分叉出两种客户端处理。
func casTokenFromConfig(meta configMeta) (string, error) {
	return casTokenFromMeta(objectMeta{ResourceVersion: meta.ResourceVersion})
}

// systemConfigDocOf 把 store 的 SystemConfig 折成 wrapper（§7.5）。
func systemConfigDocOf(cfg *store.SystemConfig) systemConfigDoc {
	return systemConfigDoc{
		Metadata: configMeta{ResourceVersion: systemResourceVersion(cfg.ResourceVersion)},
		Retention: retentionDoc{
			EventTTLSec:  cfg.RetentionEventTTLSec,
			TaskTTLSec:   cfg.RetentionTaskTTLSec,
			ResultTTLSec: cfg.RetentionResultTTLSec,
		},
	}
}

// systemResourceVersion 是系统配置的 wire 令牌。
//
// 与资源对象不同，system_config 是**单行表**（没有不可复用 uid），而
// store.PutSystemConfig 只接 int64 写修订（D0.9 不新增 store 入口）。所以这里用它
// 唯一的整数版本，不做 rv1 包装：rv1 的 `<uid>` 段在这里没有事实可填，编一个假 uid
// 只会让客户端以为它能跨对象比较。
func systemResourceVersion(rev int64) string { return strconv.FormatInt(rev, 10) }

// parseSystemResourceVersion 是 systemResourceVersion 的反向操作：
// 缺失 400 resource_version_required、形状坏 400 resource_version_invalid。
func parseSystemResourceVersion(token string) (int64, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, errStatus(http.StatusBadRequest, reasonResourceVersionRequired,
			"metadata.resourceVersion is required")
	}
	rv, err := strconv.ParseInt(token, 10, 64)
	if err != nil || rv <= 0 || strconv.FormatInt(rv, 10) != token {
		return 0, errStatus(http.StatusBadRequest, reasonResourceVersionInvalid,
			"metadata.resourceVersion is malformed")
	}
	return rv, nil
}

// ---------- 配置 wrapper 的严格解码 ----------

// decodeConfigWrite 严格解码配置 wrapper 的写请求（§7.5）。
//
// 只接受 `metadata{resourceVersion}` 与 payloadKey 一个载荷键：
//
//	未知顶层字段   → 400 unknown_field（静默忽略会让人以为配置生效了）
//	写 status      → 400 status_not_writable
//	metadata 别的键 → 400 unknown_field（resourceVersion 是唯一可写元数据）
//	载荷里的未知字段 → 400 unknown_field
//
// 与资源信封（decodeWriteEnvelope）分两套的原因：配置 wrapper 没有 spec/status，
// 载荷是顶层具名对象（vars / retention）。共用一套就得给信封加例外分支，反而更容易漏。
func decodeConfigWrite(r *http.Request, wantKind, payloadKey string, payloadDst any) (configMeta, error) {
	var meta configMeta
	if r.Body == nil {
		return meta, errStatus(http.StatusBadRequest, reasonBadRequest, errEmptyBody.Error())
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return meta, errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	if len(bytes.TrimSpace(raw)) == 0 {
		return meta, errStatus(http.StatusBadRequest, reasonBadRequest, errEmptyBody.Error())
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return meta, errStatus(http.StatusBadRequest, reasonBadRequest, "request body must be a JSON object")
	}
	// 键排序后再判，保证同一份坏输入永远报同一个字段（map 迭代顺序是随机的）。
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "metadata", payloadKey:
		case "status":
			return meta, errStatus(http.StatusBadRequest, reasonStatusNotWritable, "status is read-only")
		default:
			return meta, errStatus(http.StatusBadRequest, reasonUnknownField,
				fmt.Sprintf("field %q is not writable on the %s config wrapper", k, wantKind))
		}
	}

	if rawMeta, ok := top["metadata"]; ok && !isEmptyJSON(rawMeta) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rawMeta, &m); err != nil {
			return meta, errStatus(http.StatusBadRequest, reasonBadRequest, "metadata must be an object")
		}
		metaKeys := make([]string, 0, len(m))
		for k := range m {
			metaKeys = append(metaKeys, k)
		}
		sort.Strings(metaKeys)
		for _, k := range metaKeys {
			if k != "resourceVersion" {
				return meta, errStatus(http.StatusBadRequest, reasonUnknownField,
					fmt.Sprintf("metadata.%s is not writable", k))
			}
		}
		if v, ok := m["resourceVersion"]; ok {
			if err := json.Unmarshal(v, &meta.ResourceVersion); err != nil {
				return meta, errStatus(http.StatusBadRequest, reasonBadRequest,
					"metadata.resourceVersion must be a string")
			}
		}
	}

	if rawPayload, ok := top[payloadKey]; ok && !isJSONNull(rawPayload) {
		d := json.NewDecoder(bytes.NewReader(rawPayload))
		d.DisallowUnknownFields()
		if err := d.Decode(payloadDst); err != nil {
			return meta, unknownFieldError(err)
		}
	}
	return meta, nil
}

// isJSONNull 判断一段 JSON 是否就是 `null`（`{}` 不是 null：空对象是有效载荷）。
//
// 这里必须只判 null 而不能用 isEmptyJSON：`vars: {}` 是「清空所有 vars」的合法请求，
// 若把 `{}` 也当成「没给」，用户就永远清不掉命名空间默认值。
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
