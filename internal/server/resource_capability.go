package server

// Capability 资源域（v4 HTTP 资源层，contract.md D2 / api-conventions §7.1 / capability.md §2）。
//
// 四条语义边界决定了这里的形状，写代码时不要越过去：
//
//  1. **版本就是 capability_versions 的行**（A7）：没有 CapabilityRevision；`metadata.version`
//     是行身份的一部分，`versions` 子资源只读、按版本整数降序。
//  2. **status 读时派生、不落库**（A8/S2）：四类 condition 的 reason 取封闭集合，
//     派生不出来的字段给空数组 / 空对象，绝不编造（伪造状态比缺字段更难排查）。
//  3. **写 CAS 只有一种承载方式**（A9）：PATCH 走 body `metadata.resourceVersion`，
//     DELETE 走 `?resourceVersion=`；缺失 400、旧值 409（`details.current` 由 store 的
//     ResourceVersionConflictError 经 writeAPIError 翻译，这里不重写一遍）。
//  4. **默认可读面不含脚本正文**：`spec.run.files` 只在 `?includeBody=true` 时回全文
//     （单文件 256 KiB、合计 1 MiB，列表一次几十条会把页面拖成几百 KB）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
	"github.com/zzjcool/anotify/internal/store"
	"github.com/zzjcool/anotify/internal/ws"
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
// 方法不匹配（例如 `PUT …/capabilities/{name}`）由 Go 1.22 ServeMux 自己回 405：
// v4 里能力没有 PUT（内容变化 = 新版本，走 POST），不为它再实现一遍方法分派。
func (a *capabilityAPI) register(mux *http.ServeMux) {
	const base = "/v1/namespaces/{ns}/capabilities"
	routes := []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"GET " + base, a.list},
		{"POST " + base, a.create},
		{"GET " + base + "/{name}", a.get},
		{"PATCH " + base + "/{name}", a.patch},
		{"DELETE " + base + "/{name}", a.remove},
		{"GET " + base + "/{name}/versions", a.listVersions},
		{"GET " + base + "/{name}/versions/{version}", a.getVersion},
		{"POST " + base + "/{name}/deploy", a.deploy},
		// edges 是两类派生投影的只读视图（能力 spec.next + 源 spec.targets）。
		{"GET /v1/namespaces/{ns}/edges", a.edges},
		// capability-templates 是 cluster 公共模板（不能承载 ns 私有数据）。
		{"GET /v1/capability-templates", a.templates},
	}
	for _, rt := range routes {
		mux.Handle(rt.pattern, noStore(a.d.session(rt.h)))
	}
}

// ---------- 读端点 ----------

// list 处理 GET …/capabilities：命名空间内全部能力行，按 name 升序、同名内版本降序。
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
	rows, err := a.d.DB.ListCapabilities(r.Context(), ns)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	includeBody := includeCapabilityBody(r)
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
	writeList(w, http.StatusOK, kindCapabilityList, listMeta{}, items)
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
	env, err := a.envelope(r.Context(), row, includeCapabilityBody(r))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
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
	includeBody := includeCapabilityBody(r)
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
	env, err := a.envelope(r.Context(), row, includeCapabilityBody(r))
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
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	var specDst artifact.Spec
	meta, err := decodeWriteEnvelope(r, kindCapability, &specDst, "version", "namespace")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := checkMetaNamespace(meta, ns); err != nil {
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
	env, err := a.envelope(r.Context(), cv, includeCapabilityBody(r))
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
	if err := checkMetaNamespace(meta, ns); err != nil {
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
	env, err := a.envelope(r.Context(), updated, includeCapabilityBody(r))
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
	outcomes, err = a.d.Hub.Deploy(r.Context(), ns, name, version, body.Nodes)
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
		Version:       row.Version,
		Digest:        row.Digest,
		Conditions:    make([]capabilityCondition, 0, 4),
		Nodes:         []capabilityNodeState{},
		Resolved:      capabilityResolved{Next: []capabilityResolvedNext{}},
		Observed:      map[string]any{}, // 事件/任务的窗口统计本域不查，给空对象而不是编造 0
		InstalledFrom: row.InstalledFrom,
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

	status.Ready = true
	for _, condition := range status.Conditions {
		if condition.Status != protocol.ConditionStatusTrue {
			status.Ready = false
			break
		}
	}
	return status, nil
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
// state 映射：节点确认应用了当前 digest → deployed；其余（pending / failed）→ missing。
// 「stale」需要一个**独立于当前 digest** 的节点侧快照才能判，本域不查节点上报，
// 所以不声称 stale（宁可少报，不伪造）。
func (a *capabilityAPI) deployedState(ctx context.Context, row *store.CapabilityVersion) (capabilityCondition, []capabilityNodeState, error) {
	condition := capabilityCondition{Type: protocol.ConditionDeployed}
	rows, err := a.d.DB.ListDeployments(ctx, row.NamespaceID, store.DeploymentFilter{CapabilityID: row.ID})
	if err != nil {
		return condition, nil, err
	}
	nodes := make([]capabilityNodeState, 0, len(rows))
	deployedCount := 0
	for _, deployment := range rows {
		if deployment == nil {
			continue
		}
		state := capabilityNodeMissing
		if deployment.State == store.DeploymentApplied && deployment.Digest == row.Digest {
			state = capabilityNodeDeployed
			deployedCount++
		}
		nodes = append(nodes, capabilityNodeState{Name: a.nodeName(ctx, deployment.NodeID), State: state})
	}
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	if deployedCount > 0 {
		condition.Status = protocol.ConditionStatusTrue
		condition.Reason = protocol.ConditionReasonValid
		return condition, nodes, nil
	}
	condition.Status = protocol.ConditionStatusFalse
	condition.Reason = protocol.ConditionReasonNotDeployed
	condition.Message = "没有节点持有当前 digest（人工点部署才会下发）"
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

type capabilityStatus struct {
	Version       int                   `json:"version"`
	Digest        string                `json:"digest"`
	Ready         bool                  `json:"ready"`
	Conditions    []capabilityCondition `json:"conditions"`
	Nodes         []capabilityNodeState `json:"nodes"`
	Resolved      capabilityResolved    `json:"resolved"`
	Observed      map[string]any        `json:"observed"`
	InstalledFrom string                `json:"installedFrom,omitempty"`
}

// includeCapabilityBody 判定是否回脚本正文（只有显式 `?includeBody=true` 才回）。
func includeCapabilityBody(r *http.Request) bool {
	return r.URL.Query().Get("includeBody") == "true"
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
		return errStatus(http.StatusBadRequest, artifact.Code(err),
			artifact.ProblemMessage(artifactErr.Problems[0]))
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
