package server

// Capability 资源域（v4 HTTP 资源层，contract.md D2 / api-conventions §7.1 / capability.md §2）。
// 四条语义边界决定了这里的形状，写代码时不要越过去：
// 1. **版本就是 capability_versions 的行**（A7）：没有 CapabilityRevision；`metadata.version`
// 是行身份的一部分，`versions` 子资源只读、按版本整数降序。
// 2. **status 读时派生、不落库**（A8/S2）：四类 condition 的 reason 取封闭集合，
// 派生不出来的字段给空数组 / 空对象，绝不编造（伪造状态比缺字段更难排查）。
// 3. **写 CAS 只有一种承载方式**（A9）：PATCH 走 body `metadata.resourceVersion`，
// DELETE 走 `?resourceVersion=`；缺失 400、旧值 409（`details.current` 由 store 的
// ResourceVersionConflictError 经 writeAPIError 翻译，这里不重写一遍）。
// 4. **默认可读面不含脚本正文**：`spec.run.files` 只在 `?includeBody=true` 时回全文
// （单文件 256 KiB、合计 1 MiB，列表一次几十条会把页面拖成几百 KB）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
	"github.com/zzjcool/anotify/internal/yamldoc"
)

// kindCapability / kindCapabilityList 是资源 kind 字面（与 artifact.KindCapability 同值，
// 列表 kind 按 api-conventions §3.3 加 `List` 后缀）。
const (
	kindCapability     = artifact.KindCapability
	kindCapabilityList = kindCapability + "List"
)

// 节点在能力 status 里的三态（capability.md §2.4：nodes[].state）。
const (
	capabilityNodeDeployed = "deployed"
	capabilityNodeStale    = "stale"
	capabilityNodeMissing  = "missing"
)

// capabilityAPI 是 Capability 资源域的 handler 集合。
//
// 只持有 serverDeps：作用域 / 授权走 namespace_scope.go，错误映射走 api_status.go，
// CAS 令牌走 api_cas.go —— 三件共享判据都不在这里再抄一份。
type capabilityAPI struct{ d *serverDeps }

// newCapabilityAPI 构造 Capability 资源域。
func newCapabilityAPI(d *serverDeps) *capabilityAPI { return &capabilityAPI{d: d} }

// register 注册全部 Capability 路由（session + noStore 包裹；未登录直 401，
// 动态 API 一律不许缓存）。
//
// 方法不匹配（例如 `PUT …/capabilities/{name}`）本函数**不**兜底：这张表只挂带方法的 pattern，
// 「路径存在、方法不允许」的答案取决于本 register 被挂到哪里——
//
// - 裸 mux（测试夹具 `http.NewServeMux()` 后逐个 register，如 api_integration_test 的
// itEnv）：Go 1.22 ServeMux 自动回 405 + Allow，因为没有任何别的 pattern 能接住这条请求；
// - 生产 mux（mux.go 末尾的 `mux.Handle("/", resolveStatic(cfg))`，见那里的「静态资源
// （CDN 缓存分级）」段）：那条兜底**接受任意方法**，所以未注册的 method+path 组合全被它
// 接住 —— 真实二进制回 404、未内嵌前端的测试构建回 503。自动 405 在这里根本不会发生，
// 客户端看到的是「文件不存在」而不是「方法不允许」。
//
// 能力域接受后者、不为逐条写端点补方法兜底：v4 里能力没有 PUT（内容变化 = 新版本，走 POST），
// 方法分派只由下面这张表表达。唯一的例外是**完全只读**的 edges（没有任何写端点，写请求的
// 正确语义是 405 而不是 404）：它另挂一条不带方法的 pattern 显式回 405（见下表 edges 两行，
// 与 deployments 共用同一个 methodNotAllowedAllowGet）。
func (a *capabilityAPI) register(mux *http.ServeMux) {
	const base = "/v1/namespaces/{ns}/capabilities"
	routes := []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"GET " + base, a.list},
		{"POST " + base, a.create},
		// 「从 YAML 创建」是集合级的**字面段**：Go 1.22+ 的最长匹配让
		// `/capabilities/yaml` 与 `/capabilities/{name}` 不歧义（方法也不同），
		// 与源域的 `POST …/sources/dry-run` 同一条先例。
		{"POST " + base + "/yaml", a.createYAML},
		{"GET " + base + "/{name}", a.get},
		{"PATCH " + base + "/{name}", a.patch},
		{"DELETE " + base + "/{name}", a.remove},
		{"GET " + base + "/{name}/yaml", a.yaml},
		{"GET " + base + "/{name}/versions", a.listVersions},
		{"GET " + base + "/{name}/versions/{version}", a.getVersion},
		{"POST " + base + "/{name}/deploy", a.deploy},
		// 启停与试跑（capability-lifecycle L-D1..L-D8）：三个动作 POST + 一个只读审计面。
		// 动作 POST 一律**不套 CAS**（与 deploy 一致：`enable`/`disable`/`test` 都不带
		// resourceVersion，两个管理员同时按 = 最后写赢，两条审计都留）。
		{"POST " + base + "/{name}/enable", a.enable},
		{"POST " + base + "/{name}/disable", a.disable},
		{"POST " + base + "/{name}/test", a.testRun},
		{"GET " + base + "/{name}/audit", a.audit},
		// edges 是两类派生投影的只读视图（能力 spec.next + 源 spec.targets）。
		{"GET /v1/namespaces/{ns}/edges", a.edges},
		// 「没有写端点」必须是一条**明确的** 405：不带方法的 pattern 只接非 GET/HEAD
		// （GET 仍命中上面那条更具体的），否则 POST/PUT/PATCH/DELETE …/edges 会落到生产 mux
		// 末尾的静态兜底（503/404），把「这个资源没有这个方法」伪装成「这个资源不存在」。
		{"/v1/namespaces/{ns}/edges", methodNotAllowedAllowGet},
		// capability-templates 是 cluster 公共模板（不能承载 ns 私有数据）。
		{"GET /v1/capability-templates", a.templates},
	}
	for _, rt := range routes {
		mux.Handle(rt.pattern, noStore(a.d.session(rt.h)))
	}
}

// ---------- 读端点 ----------

// list 处理 GET …/capabilities：命名空间内全部能力行，按 name 升序、同名内版本降序。
// `limit`/`continue` 是 keyset 分页（排序键 (name, version)，与对外顺序一致）。
//
// 为什么保留「同名多行」而不是只回最新版：每行都是有独立 `metadata.uid` 的版本对象
// （A7：版本 = capability_versions 的行），按 version 降序排列只有在存在多个版本时
// 才有意义；「最新版」的语义已经由 `GET …/{name}`（裸 name）与
// `…/{name}/versions`（版本族清单）两个更精确的路径表达。
func (a *capabilityAPI) list(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	binding := "capabilities|" + ns
	afterName, afterVersion := "", 0
	if keys, cerr := decodePageCursor(r.URL.Query().Get("continue"), binding, 2); cerr != nil {
		writeAPIError(w, cerr)
		return
	} else if len(keys) == 2 {
		v, verr := pageCursorInt64(keys[1])
		if verr != nil {
			writeAPIError(w, verr)
			return
		}
		afterName, afterVersion = keys[0], int(v)
	}
	rows, err := a.d.DB.ListCapabilitiesPage(r.Context(), ns, afterName, afterVersion, limit+1)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	rows, cont := pageCut(rows, limit, binding, func(row *store.CapabilityVersion) []string {
		return []string{row.Name, pageKeyInt64(int64(row.Version))}
	})
	includeBody := includeBodyQuery(r)
	ordered := orderedCapabilityVersions(rows)
	items := make([]apiEnvelope, 0, len(ordered))
	for _, row := range ordered {
		env, err := a.envelope(r.Context(), row, includeBody)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		items = append(items, env)
	}
	// 空列表是 []（writeList 收口 nil → []）。
	writeList(w, http.StatusOK, kindCapabilityList, listMeta{Continue: cont}, items)
}

// get 处理 GET …/capabilities/{name}：裸 name = 该 name 的最大版本。
func (a *capabilityAPI) get(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name := capabilityNameFromPath(r)
	rows, err := a.d.DB.ListCapabilities(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	row := newestVersionOf(rows, name)
	if row == nil {
		// 不存在与「不属于本命名空间」是同一个回答（隔离不泄露存在性）。
		writeAPIError(w, store.ErrNotFound)
		return
	}
	env, err := a.envelope(r.Context(), row, includeBodyQuery(r))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
}

// yaml 处理 GET …/capabilities/{name}/yaml：把该 name **最高版本**的存储定义序列化成
// 一份可整份贴回 playbook 包的 YAML 文本（`{"yaml": "<全文>"}`）。
//
// 三条边界（CONTRACT-capability-yaml §1.1）：取最高版本与 `get` 同一判据（不存在回 404）；
// 序列化**完整** spec（`artifact.MarshalYAML` 先 Normalize），默认读面「剥正文」不适用于这里；
// 这是存储定义的规范化重建，不是用户原文（键序/默认值由 Normalize 固定）。
func (a *capabilityAPI) yaml(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	rows, err := a.d.DB.ListCapabilities(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	row := newestVersionOf(rows, capabilityNameFromPath(r))
	if row == nil {
		// 不存在与「不属于本命名空间」是同一个回答（隔离不泄露存在性），与 get 同一条边。
		writeAPIError(w, store.ErrNotFound)
		return
	}
	text, err := artifact.MarshalYAML(capabilityArtifact(row))
	if err != nil {
		// 行里的 spec 反序列化不出来 = 服务端事实损坏，按统一映射落 500。
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, yamlDoc{YAML: string(text)})
}

// yamlDoc 是两条 `/yaml` 端点的响应体（CONTRACT-capability-yaml §1.1）。
//
// 刻意**不**带 apiVersion/kind/metadata：信封规则的判据是「有 apiVersion + metadata」，
// 给这个响应套信封会把它伪装成一种资源 kind；YAML 全文放在 yaml 字段里，客户端原样展示/复制。
type yamlDoc struct {
	YAML string `json:"yaml"`
}

// listVersions 处理 GET …/capabilities/{name}/versions：CapabilityList，
// `metadata.version` 整数降序（新 → 旧），每项是完整 Capability 信封。
func (a *capabilityAPI) listVersions(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name := capabilityNameFromPath(r)
	rows, err := a.d.DB.ListCapabilities(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	versions := versionsOf(rows, name) // 降序
	if len(versions) == 0 {
		writeAPIError(w, store.ErrNotFound)
		return
	}
	includeBody := includeBodyQuery(r)
	items := make([]apiEnvelope, 0, len(versions))
	for _, row := range versions {
		env, err := a.envelope(r.Context(), row, includeBody)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		items = append(items, env)
	}
	writeList(w, http.StatusOK, kindCapabilityList, listMeta{}, items)
}

// getVersion 处理 GET …/capabilities/{name}/versions/{version}。
func (a *capabilityAPI) getVersion(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name := capabilityNameFromPath(r)
	version, err := parseCapabilityVersionPath(r.PathValue("version"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	row, err := a.d.DB.GetCapability(r.Context(), ns, name, version)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	env, err := a.envelope(r.Context(), row, includeBodyQuery(r))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
}

// edges 处理 GET …/edges：只读派生投影，**没有** Edge 资源、没有写端点（A5）。
//
// 两个来源：能力边来自能力行（live latest）的 `spec.next`，入口边来自源的
// `spec.targets`。这里按投影的**事实源**现算，而不是读 edges 表：表是同一份事实的
// 副本，读副本意味着「表重建失败时 API 会说谎」。顺序固定（kind, from, to），
// 前端图渲染才稳定。
func (a *capabilityAPI) edges(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	caps, err := a.d.DB.ListCapabilities(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	sources, err := a.d.DB.ListSources(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	items := make([]capabilityEdgeView, 0, len(caps)+len(sources))
	for _, row := range latestCapabilityVersions(caps) {
		for _, to := range row.Spec.Next {
			to = strings.TrimSpace(to)
			if to == "" {
				continue
			}
			items = append(items, capabilityEdgeView{From: row.Name, To: to, Kind: capabilityEdgeFromCapability})
		}
	}
	for _, src := range sources {
		if src == nil {
			continue
		}
		for _, target := range src.Targets {
			to := strings.TrimSpace(target.Capability)
			if to == "" {
				continue
			}
			items = append(items, capabilityEdgeView{From: src.Name, To: to, Kind: capabilityEdgeFromSource})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		if items[i].From != items[j].From {
			return items[i].From < items[j].From
		}
		return items[i].To < items[j].To
	})
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// templates 处理 GET /v1/capability-templates：内置模板（cluster 读模型）。
//
// 沿用 v3 的响应形状 `{templates: [...]}` 而不是资源信封：它不是资源、没有
// metadata/status，包成 Capability 信封只会造出第二个假 kind（api-conventions §3.1
// 的「例外文档」纪律）。
func (a *capabilityAPI) templates(w http.ResponseWriter, r *http.Request) {
	templates := artifact.Templates()
	if templates == nil {
		templates = []artifact.Template{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": templates})
}

// ---------- 写端点 ----------

// create 处理 POST …/capabilities。
//
// 版本分配交给 store（capability.md §3）：省略 `metadata.version` = **内容寻址**
// ——先按 foldKey 在 (ns, name) 内找同内容行（有则复用），没有才分配 max+1。
// 这里刻意不预先填 `NextCapabilityVersion`：那会绕过 foldKey 复用，让「同一份内容
// POST 两次」长出两个版本，与 §3 的内容寻址语义直接冲突。
func (a *capabilityAPI) create(w http.ResponseWriter, r *http.Request) {
	if _, err := a.d.authorizePathNamespace(r); err != nil {
		writeAPIError(w, err)
		return
	}
	var specDst artifact.Spec
	meta, err := decodeWriteEnvelope(r, kindCapability, &specDst, "version", "namespace")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	a.createCore(w, r, meta, specDst)
}

// createYAML 处理 POST …/capabilities/yaml：请求体是一份 Capability YAML 文档
// （`{"yaml": "…"}`），而不是信封 JSON。
//
// 链路是「转换 → 复用」，不是「另写一份创建逻辑」：
//
//	yamldoc.Decode（语法/未知字段/类型/必填，带行号）
//	→ canonical JSON
//	→ decodeWriteEnvelopeBytes（与 JSON 创建同一套严格解码）
//	→ createCore（与 JSON 创建同一个后半段：归一、store、信封）
//
// 所以 `spec` 的语义校验、版本处置、信封投影一行都没有第二份实现；YAML 路径多的
// 只是「文档层」的诊断能力。
func (a *capabilityAPI) createYAML(w http.ResponseWriter, r *http.Request) {
	if _, err := a.d.authorizePathNamespace(r); err != nil {
		writeAPIError(w, err)
		return
	}
	text, err := decodeYAMLRequestBody(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	res, err := yamldoc.Decode(text, writeDocShape(r, reflect.TypeOf(artifact.Spec{}), "version", "namespace"))
	if err != nil {
		writeYAMLError(w, err)
		return
	}
	var specDst artifact.Spec
	meta, err := decodeWriteEnvelopeBytes(r, res.JSON, kindCapability, &specDst, "version", "namespace")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	a.createCore(w, r, meta, specDst)
}

// createCore 是 create 与 createYAML 共用的后半段（从 checkMetaNamespace 到写 201）。
//
// 两条入口只在前半段不同（信封 JSON vs YAML 文档 → 同一个 objectMeta + spec），
// 之后的行为必须逐字一致：版本分配、foldKey 复用（同内容 201）、显式同版本异内容
// 400 version_content_immutable、信封投影——这些全部由 store 与 a.envelope 决定。
//
// 这里再调一次 authorizePathNamespace 是签名（w, r, meta, spec）的直接代价：ns 是
// store 事实表的引用键（id），不能从 meta/path 推，也不能把「已授权」这件事藏在 context
// 里。顺序不变（未登录仍在读 body 之前就 401，两条入口都各自先授权一遍），代价是
// 每个 create 多两次轻量查询（ListNamespaces + AuthorizeNamespace）。
func (a *capabilityAPI) createCore(w http.ResponseWriter, r *http.Request, meta objectMeta, specDst artifact.Spec) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := checkMetaNamespace(meta, r.PathValue("ns")); err != nil {
		writeAPIError(w, err)
		return
	}
	name, err := requireMetaName(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	requestedVersion := 0
	if meta.Version != nil {
		requestedVersion = *meta.Version
	}
	norm, err := normalizeCapabilitySpec(name, requestedVersion, meta.DisplayName, specDst)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	cv := &store.CapabilityVersion{
		NamespaceID: ns,
		Name:        name,
		DisplayName: meta.DisplayName,
		Version:     requestedVersion, // 0 = 内容寻址，由 store 定版本号
		Spec:        norm.Spec,
	}
	if err := a.d.DB.CreateCapability(r.Context(), cv); err != nil {
		// 同 (ns,name,version) 异内容 → *store.VersionConflictError → 400
		// version_content_immutable（writeAPIError 已翻译）。
		writeAPIError(w, err)
		return
	}
	env, err := a.envelope(r.Context(), cv, includeBodyQuery(r))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusCreated, env)
}

// patch 处理 PATCH …/capabilities/{name}：只改 `metadata.displayName` 与 `spec.next`。
//
// 为什么只改这两项：内容（params/requires/lifecycle/concurrency/run）是版本身份的一部分，
// 同版改内容必须开新版本（capability.md §3「显式 version 的内容不可变」）。所以这里
// 对「客户端写了别的 spec 键」直接 400 `immutable_field`——静默忽略会让人以为改生效了。
func (a *capabilityAPI) patch(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name := capabilityNameFromPath(r)
	// 先摸一遍请求体（读一份拷贝后放回原位）：objectMeta 不区分「省略」与「零值」，
	// 而 merge-patch 必须区分（省略 ≠ 置空）。
	peek, err := peekCapabilityWriteBody(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	var specDst artifact.Spec
	// PATCH 的 CAS 令牌在 body `metadata.resourceVersion`（A9），所以它必须是允许的
	// metadata 键；POST 不套 CAS，不放行它（创建时带令牌只会被当成写错）。
	meta, err := decodeWriteEnvelope(r, kindCapability, &specDst, "version", "namespace", "resourceVersion")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := checkMetaNamespace(meta, r.PathValue("ns")); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := rejectMetaNameChange(meta, name); err != nil {
		writeAPIError(w, err)
		return
	}
	rv, err := casTokenFromMeta(meta)
	if err != nil {
		writeCASError(w, err)
		return
	}
	version := 0 // 0 = latest（裸 name 路径）
	if meta.Version != nil {
		version = *meta.Version
	}
	patch := store.CapabilityPatch{}
	if _, ok := peek.Meta["displayName"]; ok {
		displayName := meta.DisplayName
		patch.DisplayName = &displayName
	}
	if err := immutableCapabilitySpecPatch(peek.Spec); err != nil {
		writeAPIError(w, err)
		return
	}
	if _, ok := peek.Spec["next"]; ok {
		next := append([]string{}, specDst.Next...)
		patch.Next = &next
	}
	updated, err := a.d.DB.UpdateCapability(r.Context(), ns, name, version, patch, rv)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	env, err := a.envelope(r.Context(), updated, includeBodyQuery(r))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
}

// remove 处理 DELETE …/capabilities/{name}?resourceVersion=<R>。
//
// 逻辑删除**全部可见版本**，不级联：引用方（`next` / 源 `targets`）缺目标由派生状态
// 报出来（DownstreamResolved / 源体检），不在删除时改别人的文档（A16）。
func (a *capabilityAPI) remove(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name := capabilityNameFromPath(r)
	rv, err := casTokenFromQuery(r)
	if err != nil {
		writeCASError(w, err)
		return
	}
	if err := a.d.DB.DeleteCapability(r.Context(), ns, name, rv); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deploy 处理 POST …/capabilities/{name}/deploy：**人工部署**（不是控制器）。
//
// 写入 deployments 事实（可选目标节点；不传则按现有候选策略选），节点重连时按事实
// 补齐。动作 POST 不套 CAS（api-conventions §3.4）。`sent=false` 不代表失败：行已经
// 建好（state=pending），只是通知帧没写进在线会话。
func (a *capabilityAPI) deploy(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name := capabilityNameFromPath(r)
	body, err := decodeCapabilityDeployBody(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if a.d.Hub == nil {
		writeAPIError(w, errStatus(http.StatusInternalServerError, reasonInternal,
			"node transport is not wired: cannot record deployments"))
		return
	}
	// 目标节点：body 里给的是**名字**（契约 CapabilityDeployRequest.nodes），这里解析成
	// 节点 id 再交给 hub（deployments 事实钉的是 id）。
	targets, err := a.resolveDeployTargets(r.Context(), body.Nodes)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	version := body.Version
	if version <= 0 {
		resolved, rerr := a.d.DB.ResolveRef(r.Context(), ns, name)
		if rerr != nil {
			writeAPIError(w, rerr)
			return
		}
		version = resolved.Version
	}
	var outcomes []ws.DeployOutcome
	outcomes, err = a.d.Hub.Deploy(r.Context(), ns, name, version, targets)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	resp := capabilityDeployResponse{Deployed: make([]capabilityDeployOutcome, 0, len(outcomes)), Version: version}
	for _, outcome := range outcomes {
		view := capabilityDeployOutcome{Sent: outcome.Sent, RequestID: outcome.RequestID}
		if row := outcome.Deployment; row != nil {
			view.Node, view.State = row.NodeID, row.State
		}
		resp.Deployed = append(resp.Deployed, view)
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveDeployTargets 把动作 body 里的目标节点解析成节点 id。
//
// 契约面是**节点名**（CapabilityDeployRequest.nodes「目标节点名；省略 = 全部已授权且在线
// 节点」）；id 也接受——它是集群内唯一且无歧义的身份，历史调用方/调试都直接拿 id。
// 先按名查、未命中再按 id 查，每个条目至多两次点查（不引入降解析外的行为变化）。
//
// 语义边界三条：
// - 空 body / 空数组 = 省略 = 不指定目标 → 返回 nil，交给 hub 的候选策略；
// - **显式给了条目却解析不出来 → 404**：静默跳过会把「名字打错」变成「部署成功但一台
// 都没动」，而把解析失败的条目当成空列表更会把一笔笔误变成「部署到所有节点」；
// - 授权（跳 ns / 未授权）**不**在这里判：CreateDeployments 已有唯一一份判据
// （declared ∩ authorized 覆盖本 ns），它把未授权翻成 403（ErrEmitNamespaceDenied）。
// 在这里再拄一份只会在两份凭据刷新时漂移。
func (a *capabilityAPI) resolveDeployTargets(ctx context.Context, entries []string) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		node, err := a.d.DB.GetNodeByName(ctx, entry)
		if errors.Is(err, store.ErrNotFound) {
			node, err = a.d.DB.GetNode(ctx, entry)
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, errStatus(http.StatusNotFound, reasonNotFound,
				fmt.Sprintf("node %q not found", entry))
		}
		if err != nil {
			return nil, err
		}
		if node == nil || seen[node.ID] {
			continue
		}
		seen[node.ID] = true
		ids = append(ids, node.ID)
	}
	return ids, nil
}

// ---------- 启停与试跑（capability-lifecycle §4）----------
//
// 三条语义边界（设计 §1 / §3 / L-D2）：
//
// 1. **停用是路由期的闸门**：不建新任务，进行中的任务不取消/不暂停；部署、建版本、
// 删除都不受影响。所以这里只写开关行（`capability_switches`），不碰任务表。
// 2. **开关不是 status 落库**：`status.enabled` 仍是读时派生（V4-D1），本文件只写「开关
// 这个事实」，投影在 `statusFor`。
// 3. **动作 POST 不套 CAS**（与 deploy 一致）：enable/disable 都不带 resourceVersion，
// 也不改能力行的 resource_version —— 返回整个信封是为了让页面省一次往返。

// 停用原因的长度上限（契约 `CapabilityDisableRequest.reason.maxLength`）。
const capabilitySwitchReasonMax = 200

// capabilityWarningNotDeployed 是试跑回执里的 warning 原因码（设计 §4.4：**不是**剪枝码、
// 也不是任务失败码）：「已排队，但当前没有节点（或指定的那些节点没）持有需要的 digest」。
const capabilityWarningNotDeployed = "capability_not_deployed"

// capabilitySwitchRequest 是 enable/disable 的动作 body。
//
// enable 的发布形状是**无请求体**（传了也忽略：它没有可写字段）；disable 允许一条
// `reason`（进审计的 `detail.reason`，不进 status）。两边共用一个类型，判据只有一份。
type capabilitySwitchRequest struct {
	Reason string `json:"reason,omitempty"`
}

// capabilityTestRequest 是 POST …/test 的动作 body（全部可选）。
//
// - `version` 省略 / 0 = 当前 live 版本（与 PATCH/deploy 同一条解析）；
// - `params` 按 `spec.params` 的 JSON Schema 校验（`route.PrepareParams`）；
// - `nodes` 限定目标节点名；省略 = 全部持有所需 digest 的在线节点。
type capabilityTestRequest struct {
	Version int            `json:"version,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	Nodes   []string       `json:"nodes,omitempty"`
}

// capabilityTestWarning 是回执里的一条「跑得起来但会等」的观察（reason 封闭）。
type capabilityTestWarning struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// capabilityTestReceiptDoc 是试跑回执的 wire 形状（与 `CapabilityTestReceipt` 逐字对应）。
//
// 与 `fireSource` 同一条状态码语义：200 + 回执（已建任务、还没跑完），不是 202。
// 空列表恒 `[]`；`origin` 恒 `test`（让前端能把人工试跑和外部投递分开）。
type capabilityTestReceiptDoc struct {
	TraceID  string                  `json:"traceId"`
	Origin   string                  `json:"origin"`
	Tasks    []srcReceiptTask        `json:"tasks"`
	Pruned   []route.Prune           `json:"pruned"`
	Warnings []capabilityTestWarning `json:"warnings"`
}

// capabilityAuditEntryDoc 是审计列表的一行（`at` 是 RFC3339 UTC，`detail` 恒为对象）。
type capabilityAuditEntryDoc struct {
	ID     string          `json:"id"`
	Action string          `json:"action"`
	Actor  string          `json:"actor"`
	At     string          `json:"at"`
	Detail json.RawMessage `json:"detail"`
}

// capabilityAuditListDoc 是审计列表（非信封，与 EdgeList 同类：没有 apiVersion/kind）。
type capabilityAuditListDoc struct {
	Items []capabilityAuditEntryDoc `json:"items"`
}

// enable 处理 POST …/capabilities/{name}/enable（幂等，无请求体）。
func (a *capabilityAPI) enable(w http.ResponseWriter, r *http.Request) {
	a.setEnabled(w, r, true)
}

// disable 处理 POST …/capabilities/{name}/disable（幂等，可选 `{reason}`）。
func (a *capabilityAPI) disable(w http.ResponseWriter, r *http.Request) {
	a.setEnabled(w, r, false)
}

// setEnabled 是 enable/disable 的唯一实现（两个动作只差 enabled 与 body 形状）。
//
// 幂等：对已是目标状态的能力再调一次 = 200 + 不变的能力行，但**仍记一条审计**
// （「我又确认了一次」也是操作事实），`reason/updated_at/updated_by` 每次覆盖。
func (a *capabilityAPI) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name, err := capabilityNameOrBadRequest(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 能力必须存在（不存在/已删除 → 404，不区分「不属于本命名空间」：隔离不泄露存在性）。
	row, err := a.d.DB.ResolveRef(r.Context(), ns, name)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	body := capabilitySwitchRequest{}
	if !enabled {
		// enable 没有可写字段（body 忽略）；disable 的 reason 走严格解码。
		if err := decodeCapabilityActionBody(r, &body); err != nil {
			writeAPIError(w, err)
			return
		}
		if ran := []rune(body.Reason); len(ran) > capabilitySwitchReasonMax {
			writeAPIError(w, errStatus(http.StatusBadRequest, reasonValueInvalid,
				fmt.Sprintf("reason must be at most %d characters", capabilitySwitchReasonMax)))
			return
		}
	}
	// 开关按**能力行的名字**写（不是路径原文）：路由闸门读的是 capability.Name，
	// 两边必须同一个键，否则「按别名停用」会静默不生效。
	if _, err := a.d.DB.SetCapabilitySwitch(r.Context(), store.CapabilitySwitchInput{
		NamespaceID: ns,
		Name:        row.Name,
		Enabled:     enabled,
		Reason:      body.Reason,
		ActorID:     auth.UserIDFromContext(r.Context()),
		Now:         taskNowSeconds(a.d),
	}); err != nil {
		writeAPIError(w, err)
		return
	}
	// 返回**整个** Capability 信封（status.enabled 由开关行派生）。能力行本身没被写，
	// 所以 metadata.resourceVersion 与动作前逐字一致（enable/disable 不产生新版本）。
	env, err := a.envelope(r.Context(), row, includeBodyQuery(r))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
}

// testRun 处理 POST …/capabilities/{name}/test：人工跑一次（建真任务、真派发）。
//
// 为什么**不**把 deploy/建任务/试跑合并：试跑走的是与源事件同一条接收事务
// （`store.AcceptTestEvent`：事件 + 任务 + 审计一个事务），只多了一个 origin=test 标记；
// 另开一条建任务旁路会立刻踩到 `tasks_resolved_pin_shape` 的 CHECK 约束。
//
// 为什么 disabled 上也**允许**（L-D2）：disable 拦的是**自动**触发；人工显式动作不是自动
// 触发。反过来要求「先 enable → test → disable」，用户几乎一定忘记最后一步。
func (a *capabilityAPI) testRun(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name, err := capabilityNameOrBadRequest(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	var body capabilityTestRequest
	if err := decodeCapabilityActionBody(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if body.Version < 0 {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "version must be ≥ 1"))
		return
	}
	row, err := a.testTargetVersion(r, ns, name, body.Version)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 参数对齐：schema 不合 = 调用方输入错误（400 + `details.causes[].field`），不是剪枝
	// （剪枝是「事件收到了但下游不接」，人工试跑没有「下游」这回事）。
	params, prune := route.PrepareParams(row.Spec.Params, body.Params)
	if prune != nil {
		writeAPIError(w, capabilityParamError(prune))
		return
	}
	// 显式 nodes：解析成节点 id（名字或 id 都接受）；解析不到 → 404，不静默跳过。
	targets, err := a.resolveDeployTargets(r.Context(), body.Nodes)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	task := &store.Task{
		NamespaceID:  ns,
		CapabilityID: row.ID,
		Capability:   row.Name,
		Version:      row.Version,
		Digest:       row.Digest,
		Params:       params,
		Phase:        store.TaskPending,
	}
	receipt, err := a.d.DB.AcceptTestEvent(r.Context(), store.CapabilityTestInputs{
		NamespaceID: ns,
		Capability:  row.Name,
		Version:     row.Version,
		Payload:     params,
		Tasks:       []*store.Task{task},
		ActorID:     auth.UserIDFromContext(r.Context()),
		Now:         taskNowSeconds(a.d),
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 「没有节点持有这个 digest」不是错误：任务停在 pending 等节点上线，回执里给一条
	// warning（设计 §4.2：绝不把「现在没在线机器」伪装成 4xx）。
	writeJSON(w, http.StatusOK, capabilityTestReceiptOf(receipt, a.testWarnings(r.Context(), task, targets)))
}

// testTargetVersion 解析被试跑的能力版本：省略 = live（ResolveRef），显式 = 那一行。
//
// 两个分支的 404 语义一致（不存在与「不属于本命名空间」同一个回答）。
func (a *capabilityAPI) testTargetVersion(r *http.Request, ns, name string, version int) (*store.CapabilityVersion, error) {
	if version > 0 {
		row, err := a.d.DB.GetCapability(r.Context(), ns, name, version)
		if err != nil {
			return nil, err
		}
		return row, nil
	}
	row, err := a.d.DB.ResolveRef(r.Context(), ns, name)
	if err != nil {
		return nil, err
	}
	return row, nil
}

// capabilityParamError 把一条参数剪枝翻成 400 + `details.causes[]`。
//
// 顶层 reason 直接用剪枝码（`param_missing` / `param_type` / `param_schema` 都在
// api-conventions §4.2 的封闭集合里），`field` 指到具体参数路径（如 `params.diff`）——
// 前端据此把提示标到字段旁，而不是只弹一句「参数不合法」。
func capabilityParamError(prune *route.Prune) error {
	if prune == nil {
		return errStatus(http.StatusBadRequest, reasonBadRequest, "params are invalid")
	}
	return &apiError{
		Code:    http.StatusBadRequest,
		Reason:  prune.Reason,
		Message: prune.Detail,
		Details: &statusDetails{Causes: []statusCause{{
			Reason: prune.Reason, Field: prune.Field, Message: prune.Detail,
		}}},
	}
}

// testWarnings 判断这次试跑有没有「一个节点都跑不了」的事实（只观察，不改结果）。
//
// 判据复用派发路径的同一份实现（`EligibleNodesForTask`：在线 × 授权 × 配置已 apply ×
// 持有该 digest × 命令/值齐），所以「回执说能跑」与「调度真的能派」不会分叉。
// 显式给了 nodes 就只看那几台（用户已经缩小了范围，别拿别的机器安抚他）。
func (a *capabilityAPI) testWarnings(ctx context.Context, task *store.Task, explicit []string) []capabilityTestWarning {
	if task == nil {
		return nil
	}
	eligible, _, err := a.d.DB.EligibleNodesForTask(ctx, task)
	if err != nil {
		// 降级而不是报错：任务已经建好（回执必须回 200），这里只是少一条提示。
		a.d.logger().Warn("capability test: derive eligible nodes failed",
			"event", "api.capabilities.test.eligible_failed",
			"namespace_id", task.NamespaceID, "capability", task.Capability,
			"version", task.Version, "error", err.Error())
		return nil
	}
	if len(explicit) > 0 {
		wanted := make(map[string]bool, len(explicit))
		for _, id := range explicit {
			wanted[id] = true
		}
		kept := 0
		for _, node := range eligible {
			if node != nil && wanted[node.ID] {
				kept++
			}
		}
		if kept > 0 {
			return nil
		}
	} else if len(eligible) > 0 {
		return nil
	}
	return []capabilityTestWarning{{
		Reason: capabilityWarningNotDeployed,
		Detail: fmt.Sprintf("没有在线节点持有 %s@%d 的 digest %s；任务 %s 已排队，等节点上线或完成部署后自动派发",
			task.Capability, task.Version, task.Digest, task.ID),
	}}
}

// capabilityTestReceiptOf 把 store 的回执投影成 wire 文档（空列表恒 `[]`，origin 恒 test）。
func capabilityTestReceiptOf(receipt *store.CapabilityTestReceipt, warnings []capabilityTestWarning) capabilityTestReceiptDoc {
	doc := capabilityTestReceiptDoc{
		Origin:   store.EventOriginTest,
		Tasks:    []srcReceiptTask{},
		Pruned:   []route.Prune{},
		Warnings: []capabilityTestWarning{},
	}
	if receipt != nil {
		doc.TraceID = receipt.TraceID
		if receipt.Origin != "" {
			doc.Origin = receipt.Origin
		}
		for _, task := range receipt.Tasks {
			doc.Tasks = append(doc.Tasks, srcReceiptTask{
				TaskID: task.TaskID, Capability: task.Capability, Version: task.Version,
			})
		}
		for _, prune := range receipt.Pruned {
			doc.Pruned = append(doc.Pruned, route.Prune{
				Capability: prune.Capability, Reason: prune.Reason, Detail: prune.Detail,
			})
		}
	}
	doc.Warnings = append(doc.Warnings, warnings...)
	return doc
}

// audit 处理 GET …/capabilities/{name}/audit?limit=N：最近操作（最新在前）。
//
// 非信封（与 `GET /v1/namespaces/{ns}/edges` 同类：这是一份派生观察，不是可寻址资源）。
// `limit` 只做**格式**校验（非法 = 400），夹取交给 store（1..200，缺省 50）：两个数字
// 只能有一份，否则「handler 说 20、store 说 200」这种分歧会在翻页处才暴露。
func (a *capabilityAPI) audit(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name, err := capabilityNameOrBadRequest(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	limit, err := taskQueryInt(r, "limit", 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	entries, err := a.d.DB.ListCapabilityAudit(r.Context(), ns, name, limit)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	doc := capabilityAuditListDoc{Items: make([]capabilityAuditEntryDoc, 0, len(entries))}
	for _, e := range entries {
		if e == nil {
			continue
		}
		detail := bytes.TrimSpace(e.Detail)
		if len(detail) == 0 {
			detail = []byte("{}")
		}
		doc.Items = append(doc.Items, capabilityAuditEntryDoc{
			ID:     e.ID,
			Action: e.Action,
			Actor:  e.ActorID,
			At:     time.Unix(e.CreatedAt, 0).UTC().Format(time.RFC3339),
			Detail: json.RawMessage(detail),
		})
	}
	writeJSON(w, http.StatusOK, doc)
}

// capabilityNameOrBadRequest 取路径里的 `{name}` 并拒绝空串。
//
// Go 1.22 的 `{name}` 通配不匹配空段，所以这条分支在正常路由下不可达；仍然显式拒绝：
// 空名字落到 store 会变成 500（「我们没预料到」），而它的真实语义是调用方路径写坏了。
func capabilityNameOrBadRequest(r *http.Request) (string, error) {
	name := capabilityNameFromPath(r)
	if name == "" {
		return "", errStatus(http.StatusBadRequest, reasonBadRequest, "capability name is required")
	}
	return name, nil
}

// decodeCapabilityActionBody 严格解码动作 POST 的可选 body（未知字段 → 400，空 body = 全部省略）。
//
// 与 `decodeCapabilityDeployBody` 同一套判据（空 / `null` = 省略、未知字段 = 400、上限 1 MiB），
// 只是目标类型不同：把 deploy 那一份抄成三份迟早「有的动作用空 body 合法、有的不合法」。
// `UseNumber` 让 `params` 里的数字**原样**透传（试跑参数要逐字进任务行，不能被 float64 改写）。
func decodeCapabilityActionBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return unknownFieldError(err)
	}
	return nil
}

// ---------- 信封与 status 派生 ----------

// envelope 组装一个 Capability 信封（四键齐全；status 恒非 null）。
func (a *capabilityAPI) envelope(ctx context.Context, row *store.CapabilityVersion, includeBody bool) (apiEnvelope, error) {
	if row == nil {
		return apiEnvelope{}, errStatus(http.StatusInternalServerError, reasonInternal, "capability row is nil")
	}
	status, err := a.statusFor(ctx, row)
	if err != nil {
		return apiEnvelope{}, err
	}
	meta := objectMeta{
		Name:            row.Name,
		Namespace:       a.d.namespaceDisplay(ctx, row.NamespaceID),
		UID:             row.ID,
		Version:         metaVersion(row.Version),
		ResourceVersion: store.ResourceVersion(row.ID, row.ResourceVersion),
		// created_at 是秒级时刻（store.Now），wire 上是 RFC3339。
		CreationTimestamp: time.Unix(row.CreatedAt, 0).UTC().Format(time.RFC3339),
		DisplayName:       row.DisplayName,
	}
	return newEnvelope(kindCapability, meta, capabilitySpecView(row.Spec, includeBody), status), nil
}

// capabilitySpecView 返回对外的 spec 投影：默认剥掉脚本正文（`spec.run.files`）。
//
// Spec 是值类型，Run.Files 是 map；这里换一个新 map 而不是清空原 map，
// 免得把调用方（store 行）里的正文也擦掉。
func capabilitySpecView(spec artifact.Spec, includeBody bool) artifact.Spec {
	if includeBody {
		if spec.Run.Files == nil {
			spec.Run.Files = map[string]string{}
		}
		return spec
	}
	spec.Run.Files = map[string]string{}
	return spec
}

// statusFor 从事实派生 status（capability.md §5：全部现算，不落库）。
func (a *capabilityAPI) statusFor(ctx context.Context, row *store.CapabilityVersion) (capabilityStatus, error) {
	status := capabilityStatus{
		Version:          row.Version,
		Digest:           row.Digest,
		Conditions:       make([]capabilityCondition, 0, 4),
		Nodes:            []capabilityNodeState{},
		DeployCandidates: []capabilityDeployCandidate{},
		Resolved:         capabilityResolved{Next: []capabilityResolvedNext{}},
		Observed:         capabilityObserved{}, // 无事实时就是 {}（⑥ 有事实再填）
		InstalledFrom:    row.InstalledFrom,
		// Enabled 的默认值就是「启用」（无开关行 = enabled，capability-lifecycle §1）。
		Enabled: true,
	}

	// ⓪ Enabled：开关行的**派生投影**（不是把 status 落库，V4-D1）。
	//
	// 按**能力名**读（不是版本行）：换版本（PATCH）不重置开关，开关跟着 name 走。
	// 降级而不是报错：statusFor 是列表/详情共用的，一次读失败把整页变 500 的代价
	// 远大于「这一格暂时空着」——空着就是契约默认值（启用），并留一条 warn。
	switch sw, serr := a.d.DB.GetCapabilitySwitch(ctx, row.NamespaceID, row.Name); {
	case serr != nil:
		a.d.logger().Warn("capability status: enabled degraded to default",
			"event", "api.capabilities.status.enabled_failed",
			"namespace_id", row.NamespaceID, "capability", row.Name, "error", serr.Error())
	case sw != nil:
		status.Enabled = sw.Enabled
	}

	// ① Implemented：正文能不能过静态校验（正常写入路径写不进 Invalid 的行）。
	implemented := capabilityCondition{
		Type:   protocol.ConditionImplemented,
		Status: protocol.ConditionStatusTrue,
		Reason: protocol.ConditionReasonValid,
	}
	if err := artifact.Validate(capabilityArtifact(row)); err != nil {
		implemented.Status = protocol.ConditionStatusFalse
		implemented.Reason = protocol.ConditionReasonInvalid
		implemented.Message = err.Error()
	}
	status.Conditions = append(status.Conditions, implemented)

	// ② NodesAvailable：在线 × 授权 × 配置 applied × requires 命中（store 的统一判据）。
	status.Conditions = append(status.Conditions, a.nodesAvailableCondition(ctx, row))

	// ③ DownstreamResolved：`next` 每个引用能不能解析（裸名 = 该 name 的 latest）。
	downstream, next := a.downstreamResolved(ctx, row)
	status.Conditions = append(status.Conditions, downstream)
	status.Resolved.Next = next

	// ④ Deployed 与 nodes[]：同一份 deployments 事实的两个投影。
	deployed, nodes, err := a.deployedState(ctx, row)
	if err != nil {
		return status, err
	}
	status.Conditions = append(status.Conditions, deployed)
	status.Nodes = nodes

	// ⑤ DeployCandidates：控制台「人工部署」弹窗的目标列表。与 status.nodes 是**两件事**：
	// nodes 是「现在已经持有这份能力（或持有旧 digest）的机器」，candidates 是
	// 「可以把这份能力推过去的机器」（授权覆盖本 ns、不在排空中；online 取自活连接）。
	// 第一次部署时后者非空、前者必然为空 —— 把两者混用就是「没有候选节点」那股死结。
	candidates, err := a.deployCandidateStates(ctx, row.NamespaceID)
	if err != nil {
		return status, err
	}
	status.DeployCandidates = candidates

	// ⑥ Observed：最近任务 + 它的 emit/剪枝 + 24h 计数（capability.md §2）。
	//
	// 降级而不是报错：observed 是**可选派生面**，而 statusFor 是两个读端点（列表/详情）
	// 共用的——这里返回 error 会把整个 GET 变成 500，代价远大于「这一块暂时空着」。
	// 空对象本身就是文档允许的形状（`{}`），所以降级不破坏契约；但必须留一条 warn，
	// 否则一个永久坏的库会安静地让 observed 永远为空。
	observed, oerr := a.d.DB.ObservedForCapability(ctx, row.NamespaceID, row.Name)
	if oerr != nil {
		a.d.logger().Warn("capability status: observed degraded to empty object",
			"event", "api.capabilities.status.observed_failed",
			"namespace_id", row.NamespaceID, "capability", row.Name, "version", row.Version,
			"error", oerr.Error())
	} else {
		status.Observed = capabilityObservedStatus(observed)
	}

	status.Ready = true
	for _, condition := range status.Conditions {
		if condition.Status != protocol.ConditionStatusTrue {
			status.Ready = false
			break
		}
	}
	return status, nil
}

// capabilityObservedStatus 把 store 的事实拼成 wire 形状。
//
// 没有最近任务（= 这个能力从没跑过）就返回零值 `{}`：四个键都不写。这与升级前的
// 行为逐字节一致（向后兼容），「从没跑过」也就不需要额外的哨兵值。
// 反过来，**有**最近任务但窗口内零条时仍然写 `tasks:{total:0,failed:0}`：那是一句
// 有信息量的话（最近跑过，但最近 24h 没动静），不该退化成「没数据」。
// emits/pruned 的元素恒写 `fields:[]` 而不是 null。
func capabilityObservedStatus(o store.CapabilityObserved) capabilityObserved {
	out := capabilityObserved{}
	if o.TaskID == "" {
		return out
	}
	out.TaskID = o.TaskID
	for _, emit := range o.Emits {
		fields := emit.Fields
		if fields == nil {
			fields = []string{}
		}
		out.Emits = append(out.Emits, capabilityObservedEmit{To: emit.To, Fields: fields, OK: emit.OK})
	}
	for _, prune := range o.Pruned {
		out.Pruned = append(out.Pruned, capabilityObservedPrune{To: prune.To, Reason: prune.Reason, Detail: prune.Detail})
	}
	out.Tasks = &capabilityObservedTasks{Total: o.Total, Failed: o.Failed}
	return out
}

// nodesAvailableCondition 派生 NodesAvailable（capability.md §2.4 的判据集合）。
//
// 派生失败时**不伪造**「有节点」：标 False + NoMatchingNode，并把真实错误写进 message
// （reason 是封闭集合，不能为「读不出来」新造一个码）。
func (a *capabilityAPI) nodesAvailableCondition(ctx context.Context, row *store.CapabilityVersion) capabilityCondition {
	condition := capabilityCondition{Type: protocol.ConditionNodesAvailable}
	if strings.TrimSpace(row.Digest) == "" {
		condition.Status = protocol.ConditionStatusFalse
		condition.Reason = protocol.ConditionReasonNoMatchingNode
		condition.Message = "capability has no digest yet"
		return condition
	}
	nodes, reasons, err := a.d.DB.EligibleNodes(ctx, row.NamespaceID, row.Spec, row.Digest)
	if err != nil {
		a.d.logger().Warn("capability status: derive nodes availability failed",
			"event", "api.capabilities.status.nodes_failed",
			"namespace_id", row.NamespaceID, "capability", row.Name, "version", row.Version,
			"error", err.Error())
		condition.Status = protocol.ConditionStatusFalse
		condition.Reason = protocol.ConditionReasonNoMatchingNode
		condition.Message = "cannot derive node availability: " + err.Error()
		return condition
	}
	if reason := store.NodeAvailabilityReason(reasons); reason != "" {
		condition.Status = protocol.ConditionStatusFalse
		condition.Reason = reason
		condition.Message = capabilityNodeAvailabilityMessage(reason, len(nodes))
		return condition
	}
	if len(nodes) == 0 {
		condition.Status = protocol.ConditionStatusFalse
		condition.Reason = protocol.ConditionReasonNoMatchingNode
		return condition
	}
	condition.Status = protocol.ConditionStatusTrue
	condition.Reason = protocol.ConditionReasonValid
	return condition
}

// capabilityNodeAvailabilityMessage 是人话（message 可变、不参与判断）。
func capabilityNodeAvailabilityMessage(reason string, eligible int) string {
	switch reason {
	case protocol.ConditionReasonNodeOffline:
		return "没有在线节点能执行这个能力"
	case protocol.ConditionReasonNamespaceUnauthorized:
		return "没有节点的命名空间申报/授权覆盖本命名空间"
	case protocol.ConditionReasonConfigPending:
		return "节点的机器级配置还没有 applied"
	case protocol.ConditionReasonNotDeployed:
		return "节点上还没有部署这个 digest"
	case protocol.ConditionReasonCommandMissing:
		return "没有节点声明 requires.commands 里的全部命令"
	case protocol.ConditionReasonValueMissing:
		return "requires.values 在节点与中枢都解析不出值"
	default:
		if eligible > 0 {
			return "有可用节点"
		}
		return "没有节点同时满足在线、授权、配置 applied 与 requires"
	}
}

// downstreamResolved 派生 DownstreamResolved 与 `resolved.next[]`（逐个 `next` 各算各的）。
func (a *capabilityAPI) downstreamResolved(ctx context.Context, row *store.CapabilityVersion) (capabilityCondition, []capabilityResolvedNext) {
	condition := capabilityCondition{
		Type:   protocol.ConditionDownstreamResolved,
		Status: protocol.ConditionStatusTrue,
		Reason: protocol.ConditionReasonValid,
	}
	next := make([]capabilityResolvedNext, 0, len(row.Spec.Next))
	for _, ref := range row.Spec.Next {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		item := capabilityResolvedNext{Name: store.RefName(ref)}
		resolved, err := a.d.DB.ResolveRef(ctx, row.NamespaceID, ref)
		if err != nil || resolved == nil {
			item.Missing = true
			item.Reason = protocol.ConditionReasonNotFound
			next = append(next, item)
			condition.Status = protocol.ConditionStatusFalse
			condition.Reason = protocol.ConditionReasonNotFound
			condition.Message = "next 里的引用解析不到：" + ref
			continue
		}
		item.Name = resolved.Name
		item.Version = resolved.Version
		item.Digest = resolved.Digest
		next = append(next, item)
	}
	return condition, next
}

// deployedState 派生 Deployed condition 与 `nodes[]`（capability.md §5：来自 deployments 事实）。
//
// 判据按**能力名**取部署事实（`deployments × capability_versions`），不是按当前版本行：
// 部署行钉的是「某一版」，而 status 回答的是「这个能力名在节点上处于什么状态」。
//
// 逐节点（每台只出一行）：
// - 有 applied 且 digest == 当前行 digest → deployed；
// - 没有 deployed，但有 applied 且 digest != 当前行 digest → stale
// （节点持有别的版本；升级后旧部署行的 digest 与当前行不同，正是这一格）；
// - 其余（从没部署过 / pending / failed）→ missing。
//
// 同一节点可能同时挂着指向不同版本行的事实（`CreateDeployments` 的 supersede 只清同一
// capability_id 的行），所以按 deployed > stale > missing 折叠：这三个投影里最强的一条
// 才是「节点此刻持有什么」的真实回答。
// deployCandidateStates 把「可部署节点」投影成 status.deployCandidates（名字 + 是否在线）。
//
// 判据只有 store.DeployCandidateNodes（授权 ∩ 申报覆盖本 ns、不在排空中）+ 活连接事实，
// 不看「已经部署过没有」—— 否则首次部署时弹窗永远空。
func (a *capabilityAPI) deployCandidateStates(ctx context.Context, namespaceID string) ([]capabilityDeployCandidate, error) {
	nodes, err := a.d.DB.DeployCandidateNodes(ctx, namespaceID)
	if err != nil {
		return nil, err
	}
	out := make([]capabilityDeployCandidate, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, capabilityDeployCandidate{
			Name:   n.Name,
			Online: a.d.Hub != nil && a.d.Hub.Online(n.ID),
		})
	}
	return out, nil
}

// deployedState 派生 Deployed 条件与 `status.nodes`（已持有该能力的机器）。
func (a *capabilityAPI) deployedState(ctx context.Context, row *store.CapabilityVersion) (capabilityCondition, []capabilityNodeState, error) {
	condition := capabilityCondition{Type: protocol.ConditionDeployed}
	rows, err := a.d.DB.ListDeploymentsByCapabilityName(ctx, row.NamespaceID, row.Name)
	if err != nil {
		return condition, nil, err
	}
	// 状态强度：deployed 压 stale 压 missing（同一节点多行时取最强的一条）。
	const (
		rankMissing = iota
		rankStale
		rankDeployed
	)
	nodeState := make(map[string]capabilityNodeState, len(rows))
	nodeRank := make(map[string]int, len(rows))
	for _, deployment := range rows {
		if deployment == nil || deployment.NodeID == "" {
			continue
		}
		state, rank := capabilityNodeMissing, rankMissing
		if deployment.State == store.DeploymentApplied {
			if deployment.Digest == row.Digest {
				state, rank = capabilityNodeDeployed, rankDeployed
			} else {
				state, rank = capabilityNodeStale, rankStale
			}
		}
		if current, seen := nodeRank[deployment.NodeID]; seen && current >= rank {
			continue
		}
		nodeRank[deployment.NodeID] = rank
		nodeState[deployment.NodeID] = capabilityNodeState{
			Name: a.nodeName(ctx, deployment.NodeID), State: state,
		}
	}
	nodes := make([]capabilityNodeState, 0, len(nodeState))
	deployedCount, staleCount := 0, 0
	for _, node := range nodeState {
		nodes = append(nodes, node)
		switch node.State {
		case capabilityNodeDeployed:
			deployedCount++
		case capabilityNodeStale:
			staleCount++
		}
	}
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	// Deployed = 「有节点持有**当前** digest」；旧 digest 不算（capability.md §2.4）。
	if deployedCount > 0 {
		condition.Status = protocol.ConditionStatusTrue
		condition.Reason = protocol.ConditionReasonValid
		return condition, nodes, nil
	}
	condition.Status = protocol.ConditionStatusFalse
	condition.Reason = protocol.ConditionReasonNotDeployed
	if staleCount > 0 {
		condition.Message = fmt.Sprintf("%d 台节点持有旧 digest（人工点部署才会下发）", staleCount)
	} else {
		condition.Message = "没有节点持有当前 digest（人工点部署才会下发）"
	}
	return condition, nodes, nil
}

// nodeName 把节点 id 换成展示名；查不到时回 id（id 也是真实事实，不是编造）。
func (a *capabilityAPI) nodeName(ctx context.Context, nodeID string) string {
	if nodeID == "" {
		return ""
	}
	node, err := a.d.DB.GetNode(ctx, nodeID)
	if err != nil || node == nil {
		return nodeID
	}
	return node.Name
}

// capabilityArtifact 把行拼成产物（只用于静态校验：apiVersion/kind/metadata 来自行）。
func capabilityArtifact(row *store.CapabilityVersion) *artifact.Artifact {
	doc := row.Artifact()
	return &doc
}

// ---------- 输入解析 ----------

// capabilityDeployRequest 是 POST …/deploy 的请求体（api-conventions §7.1）。
type capabilityDeployRequest struct {
	Version int      `json:"version,omitempty"`
	Nodes   []string `json:"nodes,omitempty"`
}

// capabilityDeployOutcome 是逐节点结果（`sent=false` = 离线排队，不是失败）。
type capabilityDeployOutcome struct {
	Node      string `json:"node"`
	State     string `json:"state"`
	Sent      bool   `json:"sent"`
	RequestID string `json:"requestId,omitempty"`
}

type capabilityDeployResponse struct {
	Deployed []capabilityDeployOutcome `json:"deployed"`
	Version  int                       `json:"version"`
}

// capabilityEdgeView 是派生边的一行（两端都是引用字面，不是 id）。
type capabilityEdgeView struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// 边来源的 kind 字面：上游是能力还是源（edges.from_is_source 的 wire 对应物）。
const (
	capabilityEdgeFromCapability = "capability"
	capabilityEdgeFromSource     = "source"
)

// capabilitySpecStatus 是 status 的四个条件 + 派生面（capability.md §2.4）。
type capabilityCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

type capabilityNodeState struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// capabilityDeployCandidate 是「这台能力能部署到哪台机器」的一个目标（**部署**判据，
// 不是派发前提）。online 是此刻的连接事实：离线节点也能接部署意图（行会落成 pending，
// 重连时补齐），页面只是要能把两者区分开。
//
// **不要**用 capabilityNodeState 代替：那里面的 state 是「已持有这份能力的哪一版」，
// 首次部署时必然为空。
type capabilityDeployCandidate struct {
	Name   string `json:"name"`
	Online bool   `json:"online"`
}

type capabilityResolvedNext struct {
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Missing bool   `json:"missing,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type capabilityResolved struct {
	Next []capabilityResolvedNext `json:"next"`
}

// capabilityObserved 是 status.observed（capability.md §2 / CONTRACT-observed）。
//
// **空对象是合法且默认的形状**：没有任何事实时四个键都不写，`observed` 本身仍然是
// `{}`——这是本域「派生不出来的字段给空对象，绝不编造」那条边界的延续（见文件头 §2）。
// 所以这里全部 omitempty，且 tasks 用指针（struct 的 omitempty 永不生效）。
type capabilityObserved struct {
	TaskID string                    `json:"taskId,omitempty"`
	Emits  []capabilityObservedEmit  `json:"emits,omitempty"`
	Pruned []capabilityObservedPrune `json:"pruned,omitempty"`
	Tasks  *capabilityObservedTasks  `json:"tasks,omitempty"`
}

// capabilityObservedEmit 是一次 emit 的观察行（`to`/`fields`/`ok` 恒出现）。
type capabilityObservedEmit struct {
	To     string   `json:"to"`
	Fields []string `json:"fields"`
	OK     bool     `json:"ok"`
}

// capabilityObservedPrune 是一条剪枝事实（`to`/`reason`/`detail` 恒出现，detail 可为空串）。
type capabilityObservedPrune struct {
	To     string `json:"to"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// capabilityObservedTasks 是 24h 窗口计数（不做统计报表，只够判断「最近是不是老挂」）。
type capabilityObservedTasks struct {
	Total  int `json:"total"`
	Failed int `json:"failed"`
}

type capabilityStatus struct {
	Version int    `json:"version"`
	Digest  string `json:"digest"`
	// Enabled 是开关行的**派生投影**（无行 = true）；不是把 status 整块落库（V4-D1）。
	Enabled          bool                        `json:"enabled"`
	Ready            bool                        `json:"ready"`
	Conditions       []capabilityCondition       `json:"conditions"`
	Nodes            []capabilityNodeState       `json:"nodes"`
	DeployCandidates []capabilityDeployCandidate `json:"deployCandidates"`
	Resolved         capabilityResolved          `json:"resolved"`
	Observed         capabilityObserved          `json:"observed"`
	InstalledFrom    string                      `json:"installedFrom,omitempty"`
}

// capabilityNameFromPath 取 `{name}` 路径段。
func capabilityNameFromPath(r *http.Request) string {
	return strings.TrimSpace(r.PathValue("name"))
}

// parseCapabilityVersionPath 解析 `{version}` 路径段（必须是 ≥1 的整数）。
func parseCapabilityVersionPath(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	version, err := strconv.Atoi(raw)
	if err != nil || version < 1 || strconv.Itoa(version) != raw {
		return 0, errStatus(http.StatusBadRequest, reasonBadRequest,
			"metadata.version must be a positive integer")
	}
	return version, nil
}

// rejectMetaNameChange 拦住「body 里的 metadata.name 与路径不同」（E3：name 不可变）。
func rejectMetaNameChange(meta objectMeta, pathName string) error {
	if meta.Name == "" || meta.Name == pathName {
		return nil
	}
	return errStatus(http.StatusBadRequest, reasonNameImmutable,
		"metadata.name is immutable: "+meta.Name+" != "+pathName)
}

// normalizeCapabilitySpec 归一 + 校验一份能力正文（「省略 ≡ 显式默认」的唯一实现）。
func normalizeCapabilitySpec(name string, version int, displayName string, spec artifact.Spec) (*artifact.Artifact, error) {
	doc := artifact.Artifact{
		APIVersion: artifact.APIVersion,
		Kind:       artifact.KindCapability,
		Metadata:   artifact.Metadata{Name: name, Version: version, DisplayName: displayName},
		Spec:       spec,
	}
	norm, err := artifact.Normalize(&doc)
	if err != nil {
		return nil, capabilitySpecError(err)
	}
	if err := artifact.Validate(norm); err != nil {
		return nil, capabilitySpecError(err)
	}
	return norm, nil
}

// capabilitySpecError 把 artifact 的校验错误翻成 400 + 封闭集合里的 reason
// （capability.md §6.1：impl_missing / entrypoint_no_shebang / files_invalid / …）。
//
// 不复用 store.ErrCapabilityInvalid：那一层只知道「400 BadRequest」，会把具体
// 机器码（客户端据此定位是哪个字段写坏了）丢成一句笼统的错误。
func capabilitySpecError(err error) error {
	var artifactErr *artifact.Error
	if errors.As(err, &artifactErr) && len(artifactErr.Problems) > 0 {
		causes := make([]statusCause, 0, len(artifactErr.Problems))
		located := false
		for i, problem := range artifactErr.Problems {
			field := ""
			if i < len(artifactErr.Fields) {
				field = artifactErr.Fields[i]
			}
			if field != "" {
				located = true
			}
			causes = append(causes, statusCause{
				Reason:  artifact.ProblemCode(problem),
				Field:   field,
				Message: artifact.ProblemMessage(problem),
			})
		}
		ae := &apiError{
			Code:    http.StatusBadRequest,
			Reason:  artifact.Code(err),
			Message: artifact.ProblemMessage(artifactErr.Problems[0]),
		}
		if located {
			ae.Details = &statusDetails{Causes: causes}
		}
		return ae
	}
	return errStatus(http.StatusBadRequest, reasonBadRequest, err.Error())
}

// capabilityWritePeek 是写请求体里「客户端到底写了哪些键」的事实（只用于 PATCH 的
// merge 语义与内容不可变检查）。
type capabilityWritePeek struct {
	Meta map[string]json.RawMessage
	Spec map[string]json.RawMessage
}

// peekCapabilityWriteBody 读一份请求体拷贝并放回原位，让 decodeWriteEnvelope 仍然
// 看到同样的字节。
//
// 为什么要多这一遍：objectMeta 不区分「省略」与「零值」（`displayName: ""` 与不带
// displayName 都解成 ""），而 merge-patch 必须区分。这里只做**存在性**判断，
// 严格解码与报错仍然只有 decodeWriteEnvelope 一份实现。
func peekCapabilityWriteBody(r *http.Request) (capabilityWritePeek, error) {
	peek := capabilityWritePeek{Meta: map[string]json.RawMessage{}, Spec: map[string]json.RawMessage{}}
	if r.Body == nil {
		return peek, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return peek, errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if len(bytes.TrimSpace(raw)) == 0 {
		return peek, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		// 不是 JSON 对象：交给 decodeWriteEnvelope 统一报 unknown_field / BadRequest。
		return peek, nil
	}
	if spec, ok := top["spec"]; ok && !isEmptyJSON(spec) {
		_ = json.Unmarshal(spec, &peek.Spec) // spec 不是对象时忽略；严格解码会报错
	}
	if meta, ok := top["metadata"]; ok && !isEmptyJSON(meta) {
		_ = json.Unmarshal(meta, &peek.Meta)
	}
	return peek, nil
}

// immutableCapabilitySpecPatch 拦住 PATCH 里除 `next` 之外的 spec 键。
//
// 内容不可变是版本规则（capability.md §3），不是本 handler 的偏好：静默忽略
// `run` / `params` 的改动会让调用方以为改生效了，而部署身份（digest）根本没变。
func immutableCapabilitySpecPatch(spec map[string]json.RawMessage) error {
	writable := map[string]bool{"next": true}
	rejected := make([]string, 0, len(spec))
	for key := range spec {
		if !writable[key] {
			rejected = append(rejected, key)
		}
	}
	if len(rejected) == 0 {
		return nil
	}
	sort.Strings(rejected)
	return errStatus(http.StatusBadRequest, reasonImmutableField,
		"PATCH 只接受 spec.next 与 metadata.displayName；内容不可变，改内容请提交新版本（POST）: spec."+
			strings.Join(rejected, ", spec."))
}

// decodeCapabilityDeployBody 严格解码 deploy 动作 body（未知字段 → 400，空 body = 全部省略）。
func decodeCapabilityDeployBody(r *http.Request) (capabilityDeployRequest, error) {
	var body capabilityDeployRequest
	if r.Body == nil {
		return body, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return body, errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return body, unknownFieldError(err)
	}
	if body.Version < 0 {
		return body, errStatus(http.StatusBadRequest, reasonBadRequest, "version must be ≥ 1")
	}
	return body, nil
}

// ---------- 版本族工具（ListCapabilities 之后重排） ----------

// orderedCapabilityVersions 把 store 的 (name, version) 升序结果重排成
// 「name 升序、同名内 version 降序」（契约：每个 name 的版本族，新 → 旧）。
func orderedCapabilityVersions(rows []*store.CapabilityVersion) []*store.CapabilityVersion {
	out := make([]*store.CapabilityVersion, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version > out[j].Version
	})
	return out
}

// latestCapabilityVersions 把 store 的 (name, version) 升序结果收成
// 「每个 name 一条、按 name 升序、取最大 version」（edges 投影只看 live latest）。
//
// 不假设输入顺序：显式比较 version，store 将来改排序也不会悄悄取错版本。
func latestCapabilityVersions(rows []*store.CapabilityVersion) []*store.CapabilityVersion {
	out := make([]*store.CapabilityVersion, 0, len(rows))
	index := make(map[string]int, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		if at, ok := index[row.Name]; ok {
			if row.Version > out[at].Version {
				out[at] = row
			}
			continue
		}
		index[row.Name] = len(out)
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// versionsOf 返回某 name 的全部版本，**版本整数降序**（新 → 旧，api-conventions §5.3）。
func versionsOf(rows []*store.CapabilityVersion, name string) []*store.CapabilityVersion {
	out := make([]*store.CapabilityVersion, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Name != name {
			continue
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out
}

// newestVersionOf 返回某 name 的最大版本行；没有返回 nil。
func newestVersionOf(rows []*store.CapabilityVersion, name string) *store.CapabilityVersion {
	var newest *store.CapabilityVersion
	for _, row := range rows {
		if row == nil || row.Name != name {
			continue
		}
		if newest == nil || row.Version > newest.Version {
			newest = row
		}
	}
	return newest
}
