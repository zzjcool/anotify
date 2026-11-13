package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/route"
	"github.com/zzjcool/anotify/internal/source"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// resource_source.go 是 v4 的 **Source 资源域 + 事件入口**（#331 / W-src）。
//
//	GET    /v1/namespaces/{ns}/sources                       List 信封
//	POST   /v1/namespaces/{ns}/sources                       创建
//	GET    /v1/namespaces/{ns}/sources/{name}                单对象（读投影脱敏）
//	PATCH  /v1/namespaces/{ns}/sources/{name}                改 displayName/config/targets/enabled
//	DELETE /v1/namespaces/{ns}/sources/{name}?resourceVersion= 删（CAS 只在 query）
//	POST   /v1/namespaces/{ns}/sources/{name}/fire           手动触发 → 接收回执
//	POST   /v1/namespaces/{ns}/sources/{name}/dry-run        已保存源试运行 → 预览文档
//	POST   /v1/namespaces/{ns}/sources/dry-run               未保存草稿试运行 → 预览文档
//	POST   /v1/hooks/{token}                                 外部投递（无会话；token 即凭据）
//
// namespace 作用域只有一份判据（`namespace_scope.go` 的 authorizePathNamespace）：
// 未登录 401 / 缺 ns 400 / ns 不存在 404，**绝不回退默认命名空间**。
//
// 敏感字段纪律（source.md §3.1 / api-conventions §7.2，硬约束）：
//
//	读：GET **不回显** `spec.config.token` / `spec.config.secret`，只给只读投影
//	    `spec.config.hookUrl`（真实 token 拼出的授权读 URL）与 `spec.config.secretSet`；
//	    同两个投影也进 `status.hookUrl`（非 webhook 的源为空串）。
//	写：请求体出现 `spec.config.hookUrl` / `spec.config.secretSet` → 400 unknown_field
//	    （客户端编辑前必须先剥掉读投影，否则会「把投影当配置写回去」）。
//	存：`PATCH` 省略 `config.token` / `config.secret` = 保持原值（store 的
//	    sourcespec.MergeConfig 负责合并；本文件传 nil 表示「本次不改 config」）。
//
// 凭据原文不进入日志、不进入错误文本：本文件的错误只走 writeAPIError 的映射文本
// （store/sourcespec 的错误消息只点字段名，不含值），也不额外 slog 请求体。

// kindSourceDoc 是 Source 资源在信封里的 kind 字面（List 是 kindSourceDoc+"List"）。
const kindSourceDoc = "Source"

// srcHookPathPrefix 是授权读 hookUrl 的路径前缀（相对路径，不带 host：hub 的对外
// 域名属于部署事实，不该被 handler 编进资源文档）。
const srcHookPathPrefix = "/v1/hooks/"

// 请求体上限：资源写 1 MiB（表单体量级），外部 hook 投递 4 MiB（source.md §6）。
const (
	srcWriteBodyLimit = 1 << 20
	srcHookBodyLimit  = 4 << 20
)

// status.health 的三个可取词（playbook.md §6.2 的封闭词表的行内可派生项）。
//
// `unapplied` 故意不在这里：它是「安装包声明与库里不一致」的**比对结论**，需要
// playbook 的声明做输入；单看一行 sources 无从判定（W-src 契约：status 全部从行事实
// 派生）。等 playbook 的体检聚合接进来时由那边给 unapplied，不由这里猜。
const (
	srcHealthOK       = "ok"
	srcHealthBlocked  = "blocked"
	srcHealthDisabled = "disabled"
)

// sourceAPI 是一个资源域的 handler 集合（依赖来自冻结的 serverDeps）。
type sourceAPI struct {
	d *serverDeps
}

// newSourceAPI 构造 Source 资源域。
func newSourceAPI(d *serverDeps) *sourceAPI { return &sourceAPI{d: d} }

// register 把 Source 资源域挂到 mux 上。
//
// 两条与中间件有关的纪律：
//   - `fire` 是写入口 → 背压在 **session 内侧**：未登录先 401，不让外部从
//     503/200 的差别里读出「中枢现在积压了吗」。
//   - `/v1/hooks/{token}` **没有 session**：外部系统（告警/GitHub）不可能带我们的
//     Cookie，鉴权就是 URL 里的 token，所以只有 noStore + 背压。
func (a *sourceAPI) register(mux *http.ServeMux) {
	const base = "/v1/namespaces/{ns}/sources"
	mux.Handle("GET "+base, noStore(a.d.session(http.HandlerFunc(a.list))))
	mux.Handle("POST "+base, noStore(a.d.session(http.HandlerFunc(a.create))))
	// Go 1.22+ 的最长匹配：字面段 `/dry-run` 比 `{name}` 更具体，不会歧义。
	mux.Handle("POST "+base+"/dry-run", noStore(a.d.session(http.HandlerFunc(a.previewInline))))
	mux.Handle("GET "+base+"/{name}", noStore(a.d.session(http.HandlerFunc(a.get))))
	mux.Handle("PATCH "+base+"/{name}", noStore(a.d.session(http.HandlerFunc(a.patch))))
	mux.Handle("DELETE "+base+"/{name}", noStore(a.d.session(http.HandlerFunc(a.remove))))
	mux.Handle("POST "+base+"/{name}/fire", noStore(a.d.session(a.d.backpressure(http.HandlerFunc(a.fire)))))
	mux.Handle("POST "+base+"/{name}/dry-run", noStore(a.d.session(http.HandlerFunc(a.previewSaved))))
	mux.Handle("POST /v1/hooks/{token}", noStore(a.d.backpressure(http.HandlerFunc(a.hook))))
}

// ---------- 资源 CRUD ----------

// list 处理 GET …/sources（List 信封，空列表 `[]`；`limit`/`continue` keyset 分页）。
func (a *sourceAPI) list(w http.ResponseWriter, r *http.Request) {
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
	binding := "sources|" + ns
	afterAt, afterID, err := pageAfterTimeID(r, binding)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// 多取一行只用于判定「还有没有下一页」，不进响应。
	rows, err := a.d.DB.ListSourcesPage(r.Context(), ns, afterAt, afterID, limit+1)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	rows, cont := pageCut(rows, limit, binding, func(row *store.Source) []string {
		return []string{pageKeyInt64(row.CreatedAt), row.ID}
	})
	nsDisplay := a.d.namespaceDisplay(r.Context(), ns)
	items := make([]apiEnvelope, 0, len(rows))
	for _, row := range rows {
		items = append(items, srcEnvelopeOf(row, nsDisplay))
	}
	writeList(w, http.StatusOK, kindSourceDoc+"List", listMeta{Continue: cont}, items)
}

// get 处理 GET …/sources/{name}：单对象读，走读投影（脱敏）。
func (a *sourceAPI) get(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	src, err := a.d.DB.GetSourceByName(r.Context(), ns, srcPathName(r))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, srcEnvelopeOf(src, a.d.namespaceDisplay(r.Context(), src.NamespaceID)))
}

// create 处理 POST …/sources。
//
// enabled 的默认值在这里给（true），不在 store 里：`CreateSource` 不设默认值，
// 「省略 = 启用、显式 false 才停用」是 HTTP 契约的一层语义，写进 store 会让
// playbook 安装等内部调用也意外继承这个默认。
func (a *sourceAPI) create(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	var spec srcSpecDoc
	meta, err := decodeWriteEnvelope(r, kindSourceDoc, &spec)
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
	if err := srcRejectReadOnlyConfig(spec.Config); err != nil {
		writeAPIError(w, err)
		return
	}
	enabled := true
	if spec.Enabled != nil {
		enabled = *spec.Enabled
	}
	src := &store.Source{
		NamespaceID: ns,
		Name:        strings.TrimSpace(name),
		DisplayName: strings.TrimSpace(meta.DisplayName),
		Kind:        srcSpecKind(spec.Kind),
		Config:      srcConfigBytes(spec.Config),
		Targets:     srcTargets(spec.Targets),
		Enabled:     enabled,
	}
	// kind/config/targets 的校验只有 store/sourcespec 一份实现：这里不预判，
	// 否则「handler 说行、store 说不行」多一条漂移面。
	if err := a.d.DB.CreateSource(r.Context(), src); err != nil {
		writeAPIError(w, err)
		return
	}
	// 重新读一次：webhook token（创建期一次生成）与 nextFireAt 是落库后的事实，
	// 入参对象上没有（token 由 store 的创建路径生成）。
	fresh, err := a.d.DB.GetSourceByName(r.Context(), ns, src.Name)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusCreated, srcEnvelopeOf(fresh, a.d.namespaceDisplay(r.Context(), fresh.NamespaceID)))
}

// patch 处理 PATCH …/sources/{name}（merge-patch 语义）。
//
// 只有四个可写面：metadata.displayName / spec.config / spec.targets / spec.enabled。
// name 与 kind 不可变（换名断入口边、换 kind 换语义）；CAS 令牌在 body
// metadata.resourceVersion（缺失 400、旧值 409 + details.current）。
func (a *sourceAPI) patch(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	name := srcPathName(r)
	var spec srcSpecDoc
	// resourceVersion 是 PATCH 允许的 metadata 键（api_conventions §3.4）。
	meta, err := decodeWriteEnvelope(r, kindSourceDoc, &spec, "resourceVersion")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := checkMetaNamespace(meta, r.PathValue("ns")); err != nil {
		writeAPIError(w, err)
		return
	}
	if meta.Name != "" && meta.Name != name {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonNameImmutable, "metadata.name is immutable"))
		return
	}
	token, err := casTokenFromMeta(meta)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	cur, err := a.d.DB.GetSourceByName(r.Context(), ns, name)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// spec.kind 不可变：显式给一个**不同**的 kind 是调用方在换语义，必须拒绝并说清
	// 原因；原样回传同一个 kind（UI 整份提交时会带）不算改动。
	if spec.Kind != nil && strings.TrimSpace(*spec.Kind) != cur.Kind {
		writeAPIError(w, errStatus(http.StatusBadRequest, "source_kind_immutable",
			"spec.kind is immutable; delete the source and create a new one to change kind"))
		return
	}
	if err := srcRejectReadOnlyConfig(spec.Config); err != nil {
		writeAPIError(w, err)
		return
	}

	patch := store.SourcePatch{}
	// displayName 的「省略」与「显式空串」在解码后不可区分，按省略处理（保持原值）：
	// 清空展示名不是一个有语义的操作，而「PATCH 只改 targets 顺手把展示名清掉」
	// 是实打实的数据丢失。
	if display := strings.TrimSpace(meta.DisplayName); display != "" {
		patch.DisplayName = &display
	}
	if spec.Enabled != nil {
		enabled := *spec.Enabled
		patch.Enabled = &enabled
	}
	if spec.Targets != nil {
		targets := srcTargets(spec.Targets)
		patch.Targets = &targets
	}
	// 显式 `config: null` 按「本次不改 config」处理：merge-patch 的 null 语义在这里
	// 会被读成「清空 payload/sample/payloadSchema」，而客户端只是想跳过这一节。
	if cfg := srcConfigBytes(spec.Config); cfg != nil {
		patch.Config = cfg
	}

	updated, err := a.d.DB.UpdateSourceByName(r.Context(), ns, name, patch, token)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, srcEnvelopeOf(updated, a.d.namespaceDisplay(r.Context(), updated.NamespaceID)))
}

// remove 处理 DELETE …/sources/{name}?resourceVersion=（无请求体）。
//
// 成功给 204：源删除没有需要回给客户端的对象状态或回执（v4 只有 playbook 的
// DELETE 有 `kept` 回执），回一个空的成功状态比编造一个响应文档干净。
func (a *sourceAPI) remove(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	token, err := casTokenFromQuery(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.d.DB.DeleteSourceByName(r.Context(), ns, srcPathName(r), token); err != nil {
		writeAPIError(w, err)
		return
	}
	// 204 不带 body（也不带 Content-Type：没有内容就没有内容类型）。
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 动作入口：fire / dry-run ----------

// fire 处理 POST …/sources/{name}/fire：手动触发一次，返回与 /events、/hooks 同一份回执。
//
// 省略 payload → 用源 config.payload 打底（source.md §5.2）；不新增 manual 专属幂等键。
func (a *sourceAPI) fire(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !a.sourcesReady(w) {
		return
	}
	var body srcPayloadBody
	if err := srcDecodeAction(r, &body, srcWriteBodyLimit, true); err != nil {
		writeAPIError(w, err)
		return
	}
	receipt, err := a.d.Sources.Fire(r.Context(), ns, srcPathName(r), srcPayloadBytes(body.Payload))
	if err != nil {
		a.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, srcReceiptDocOf(receipt))
}

// previewSaved 处理 POST …/sources/{name}/dry-run：拿**已保存**的源试运行。
func (a *sourceAPI) previewSaved(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !a.sourcesReady(w) {
		return
	}
	var body srcPayloadBody
	if err := srcDecodeAction(r, &body, srcWriteBodyLimit, true); err != nil {
		writeAPIError(w, err)
		return
	}
	preview, err := a.d.Sources.Preview(r.Context(), ns, srcPathName(r), srcPayloadBytes(body.Payload))
	if err != nil {
		a.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, srcPreviewDocOf(preview))
}

// previewInline 处理 POST …/sources/dry-run：拿请求体里的**草稿**试运行。
//
// 为什么这是主路径：用户改完参数、还没保存时最想试。只支持已保存的源，就等于
// 「先保存一份坏的配置再试」——那正是要消灭的往返。
//
// 草稿的 namespace 以路径与授权为准，不信请求体里手写的值（否则能用一条 body
// 把预览推到别的命名空间去）。草稿可带 GET 回来的读投影字段（hookUrl/secretSet）：
// 这是预览不是写请求，容忍并剥离它们，免得「GET → 改 targets → 试运行」这条最自然
// 的流程被 400 unknown_field 挡住（写端点仍然严格拒绝，见 srcRejectReadOnlyConfig）。
func (a *sourceAPI) previewInline(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !a.sourcesReady(w) {
		return
	}
	var body srcInlinePreviewBody
	if err := srcDecodeAction(r, &body, srcWriteBodyLimit, true); err != nil {
		writeAPIError(w, err)
		return
	}
	if len(bytes.TrimSpace(body.Source)) == 0 || srcIsNullJSON(body.Source) {
		writeAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "spec.source is required"))
		return
	}
	// 嵌套信封交给同一套严格解码：unknown_field / status_not_writable / kind_mismatch
	// 的规则只写一次（decodeWriteEnvelope），不在这里再造一份。
	nested := &http.Request{Body: io.NopCloser(bytes.NewReader(body.Source))}
	var spec srcSpecDoc
	meta, err := decodeWriteEnvelope(nested, kindSourceDoc, &spec)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := checkMetaNamespace(meta, r.PathValue("ns")); err != nil {
		writeAPIError(w, err)
		return
	}
	enabled := true
	if spec.Enabled != nil {
		enabled = *spec.Enabled
	}
	draft := &store.Source{
		NamespaceID: ns,
		Name:        strings.TrimSpace(meta.Name),
		DisplayName: strings.TrimSpace(meta.DisplayName),
		Kind:        srcSpecKind(spec.Kind),
		Config:      srcConfigBytes(srcStripReadOnlyConfig(spec.Config)),
		Targets:     srcTargets(spec.Targets),
		Enabled:     enabled,
	}
	preview, err := a.d.Sources.PreviewSource(r.Context(), draft, srcPayloadBytes(body.Payload))
	if err != nil {
		a.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, srcPreviewDocOf(preview))
}

// ---------- 外部投递入口 ----------

// hook 处理 POST /v1/hooks/{token}。
//
// 这个端点**没有会话**：鉴权就是 URL 里的 token（谁拿到 URL 谁就能往里发事件，
// 所以 token 不可枚举、可轮换）。token 定位源 → 源行自带 namespace_id，后续写事件 /
// 建任务全部用它的命名空间，不看任何请求参数（要求外部系统先知道 namespaceId 是没道理的）。
//
// 不区分「token 不存在」与「源已停用」：对外同码同文案（source.md §5）。
//
// 顺序是**刻意的**（每一格都有契约依据）：
//
//	限流（429） → token 定源（404） → 读原始字节（413） → 验签（401） → 解 JSON（400） → 接收（200）
//
// 验签必须在解 JSON **之前**、且在落事件之前：契约要的是「缺失 / 不合法 → 401 且不产生
// 事件」（§3.3 / §5.2），所以先判定、后解析。反过来的顺序会给未签名方留下「用 400 还是
// 200 探测 body 形状」的旁路，也容易让人把校验写进已落库的路径里。
// 签名对着**原始字节**算（不是解析后再序列化），见 source.VerifyHookSignature。
func (a *sourceAPI) hook(w http.ResponseWriter, r *http.Request) {
	// 限流放在查 token **之前**：这是唯一免登录写入口，先限流能顺带保护 DB 查询，
	// 也不给「用响应时间探测 token 是否存在」留窗口（见 ratelimit.go 的 rateLimitedHook）。
	if rateLimitedHook(w, r) {
		return
	}
	if !a.sourcesReady(w) {
		return
	}
	token := strings.TrimSpace(r.PathValue("token"))
	if token == "" {
		writeStatus(w, http.StatusNotFound, reasonNotFound, "source not found", nil)
		return
	}
	// 原始字节（未解析）：签名口径就是它。上限仍在同一个地方（超限 413，不读进内存再犹豫）。
	raw, err := srcReadBody(r, srcHookBodyLimit)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// token 定位源：不存在 / 已停用仍是 404（验签不能把这条口径改成 401）。
	src, err := a.d.Sources.LookupHook(r.Context(), token)
	if err != nil {
		a.writeSourceError(w, err)
		return
	}
	// 配了 secret 就必须带合法签名；没有 secret 的源直接放行（契约：空 = 不校验）。
	// 错误体只有一句固定文案：不带 secret、不区分「缺失 / 格式错 / 不匹配」。
	if err := source.VerifyHookSignature(source.HookSecret(src), raw, r.Header.Get(source.HookSignatureHeader)); err != nil {
		writeStatus(w, http.StatusUnauthorized, reasonUnauthorized, "webhook signature missing or invalid", nil)
		return
	}
	// 投递体不做严格白名单：调用方是外部系统，它们会在包装体旁边带自己的字段
	// （签名头、平台自己的 metadata）。资源写请求的 unknown_field 纪律针对的是
	// **我们自己发的写请求**，不是外部投递。
	var body source.HookBody
	if err := srcDecodeJSON(raw, &body, false); err != nil {
		writeAPIError(w, err)
		return
	}
	// 用**同一次**查表的源接收：token 在验签与落库之间不再查第二次，
	// 避免「验的是 A 的 secret、写的是 B 的源」这类缝。
	receipt, err := a.d.Sources.HookSource(r.Context(), src, body)
	if err != nil {
		a.writeSourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, srcReceiptDocOf(receipt))
}

// sourcesReady 在 source service 未注入时给明确失败，而不是对 nil 解引用 panic。
func (a *sourceAPI) sourcesReady(w http.ResponseWriter) bool {
	if a.d != nil && a.d.Sources != nil {
		return true
	}
	writeStatus(w, http.StatusServiceUnavailable, reasonInternal, "source service is not configured", nil)
	return false
}

// writeSourceError 把 source 域的哨兵映射成 Status，其余交给统一映射。
//
// 两条 source 专属映射：
//   - ErrSourceDisabled → 404：停用与不存在对外同码同文案（source.md §5，防 token 探测）。
//   - ErrBadPayload     → 400 value_invalid：载荷不是 JSON 对象，不产生事件。
//
// 不在这里写日志：accessLog 已经记了状态码与路径；把 err.Error() 再写一遍只会让
// 「某个错误文本里恰好带了请求内容」变成一条难以追回的凭据泄漏路径。
func (a *sourceAPI) writeSourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, source.ErrSourceDisabled):
		writeStatus(w, http.StatusNotFound, reasonNotFound, "source not found", nil)
	case errors.Is(err, source.ErrBadPayload):
		writeStatus(w, http.StatusBadRequest, reasonValueInvalid, "payload must be a JSON object", nil)
	default:
		writeAPIError(w, err)
	}
}

// ---------- 信封投影 ----------

// srcSpecDoc 是 Source 的写请求 spec（封闭集合：未知字段由 decodeWriteEnvelope 拒）。
//
// 指针字段承担 merge-patch 的「省略 ≠ 显式空值」：nil = 本次不改，
// 非 nil（含指向空切片 / false）才是客户端的显式意图。
type srcSpecDoc struct {
	Kind    *string              `json:"kind"`
	Enabled *bool                `json:"enabled"`
	Config  json.RawMessage      `json:"config"`
	Targets *[]sourcespec.Target `json:"targets"`
}

// srcSpecView 是 Source 的**读** spec：config 已换成脱敏投影。
type srcSpecView struct {
	Kind    string              `json:"kind"`
	Enabled bool                `json:"enabled"`
	Config  json.RawMessage     `json:"config"`
	Targets []sourcespec.Target `json:"targets"`
}

// srcStatusView 是 Source 的只读派生面（api-conventions §7.2）。
//
// lastError 恒 null：v4 没有「源的最后一次错误」这个落库事实（失败留在事件 /
// route 剪枝 / 任务里，用 trace 查），编一个字段值会让前端以为它可信。
type srcStatusView struct {
	HookURL         string  `json:"hookUrl"`
	LastFiredAt     string  `json:"lastFiredAt,omitempty"`
	NextFireAt      string  `json:"nextFireAt,omitempty"`
	Enabled         bool    `json:"enabled"`
	Health          string  `json:"health"`
	TargetsResolved bool    `json:"targetsResolved"`
	LastError       *string `json:"lastError"`
}

// srcEnvelopeOf 把一行源投影成 v4 信封（四键 + 脱敏 spec + 派生 status）。
//
// nsDisplay 是命名空间的对外名字（slug）；传空串时回落到 store id（只可能出现在
// 测试直接构造 deps 的场景，正常请求路径由 authorizePathNamespace 填好缓存）。
func srcEnvelopeOf(s *store.Source, nsDisplay string) apiEnvelope {
	cfg, hookURL := srcReadConfig(s.Kind, s.Config)
	spec := srcSpecView{
		Kind:    s.Kind,
		Enabled: s.Enabled,
		Config:  cfg,
		Targets: append([]sourcespec.Target{}, s.Targets...),
	}
	status := srcStatusView{
		HookURL: hookURL,
		Enabled: s.Enabled,
		Health:  srcHealth(s),
		// 「声明了至少一个出口」= 这条源真的能把事件送到某个能力。空 targets 是
		// 合法配置（可先建入口），但它的事件只会留一条 no_target 剪枝痕，所以
		// targetsResolved=false 与 health=blocked 说的是同一件事，与 status 自洽。
		TargetsResolved: len(s.Targets) > 0,
	}
	if s.LastFiredAt != nil {
		status.LastFiredAt = srcTimestamp(*s.LastFiredAt)
	}
	if s.NextFireAt != nil {
		status.NextFireAt = srcTimestamp(*s.NextFireAt)
	}
	if nsDisplay == "" {
		nsDisplay = s.NamespaceID
	}
	meta := objectMeta{
		Name:              s.Name,
		Namespace:         nsDisplay,
		UID:               s.ID,
		ResourceVersion:   store.ResourceVersion(s.ID, s.ResourceVersion),
		CreationTimestamp: srcTimestamp(s.CreatedAt),
		DisplayName:       s.DisplayName,
	}
	return newEnvelope(kindSourceDoc, meta, spec, status)
}

// srcHealth 从行事实派生体检词（ok / blocked / disabled；unapplied 不在行内可判，见常量注释）。
func srcHealth(s *store.Source) string {
	switch {
	case !s.Enabled:
		return srcHealthDisabled
	case len(s.Targets) == 0:
		return srcHealthBlocked
	}
	return srcHealthOK
}

// srcTimestamp 把 store 的 Unix 秒转成信封里的 RFC3339（UTC）；0 = 没有事实 → 省略。
func srcTimestamp(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// srcReadConfig 把库里的 config 投影成读形状，并顺带返回授权读的 hookUrl。
//
// 只做两件事：删掉 token / secret 原文、给 webhook 补只读投影（hookUrl / secretSet）。
// 其余字段原样保留（payload / sample / payloadSchema / expr / tz），不做二次归一化：
// 归一化是 store 写路径的唯一职责，读路径再解释一遍迟早与写路径分叉。
func srcReadConfig(kind string, raw json.RawMessage) (json.RawMessage, string) {
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			// 库里一条手改坏的行也不该把原文字节漏出去：整份打码（与
			// sourcespec.MaskConfig 同一条兜底纪律），响应里至少是合法 JSON。
			fields = map[string]json.RawMessage{}
		}
	}
	delete(fields, "token")
	delete(fields, "secret")
	hookURL := ""
	if kind == sourcespec.KindWebhook {
		// 只用**真实** token 拼 URL：token 空（异常行）= 空 hookUrl，不编一个假地址。
		cfg := sourcespec.ParseConfig(kind, raw)
		if cfg.Token != "" {
			hookURL = srcHookPathPrefix + cfg.Token
		}
		fields["hookUrl"] = srcJSONString(hookURL)
		fields["secretSet"] = json.RawMessage(strconv.FormatBool(cfg.Secret != ""))
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage(`{}`), hookURL
	}
	return out, hookURL
}

// srcRejectReadOnlyConfig 拒绝写请求里的只读投影字段（config.hookUrl / config.secretSet）。
//
// 为什么不静默忽略：忽略会让客户端以为「我写的 hookUrl 生效了」，而真正生效的是
// 服务端存着的 token；下一次它就会基于一个错误的心智模型去轮换凭据。
func srcRejectReadOnlyConfig(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil // 不是 JSON 对象：交给 store 报 invalid_source_config（校验只有一处）
	}
	for _, key := range []string{"hookUrl", "secretSet"} {
		if _, ok := fields[key]; ok {
			return errStatus(http.StatusBadRequest, reasonUnknownField,
				"spec.config."+key+" is a read-only projection; remove it before writing")
		}
	}
	return nil
}

// srcStripReadOnlyConfig 是 dry-run 的宽容版：预览是读投影、不落库，直接剥掉
// 读投影字段（GET → 改 targets → 试运行 不必先手洗一遍）。
func srcStripReadOnlyConfig(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	changed := false
	for _, key := range []string{"hookUrl", "secretSet"} {
		if _, ok := fields[key]; ok {
			delete(fields, key)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

// ---------- 回执 / 预览文档（W6 冻结形状，本地 DTO） ----------

// srcReceiptDoc 是接收回执（/events、/hooks/{token}、source fire 共用一份形状）。
//
// 刻意不在 source.Receipt 上直接序列化：那是运行期领域类型（字段名是 Go 命名），
// 而 HTTP 形状是冻结契约。中间这一层让「领域字段演进」与「wire 形状」解耦。
type srcReceiptDoc struct {
	EventID   string           `json:"eventId"`
	TraceID   string           `json:"traceId"`
	Duplicate bool             `json:"duplicate"`
	Tasks     []srcReceiptTask `json:"tasks"`
	Pruned    []route.Prune    `json:"pruned"`
}

// srcReceiptTask 是回执里的一条任务（真实请求名 + 已解析版本；version 0 = 没有事实，省略）。
type srcReceiptTask struct {
	TaskID     string `json:"taskId"`
	Capability string `json:"capability"`
	Version    int    `json:"version,omitempty"`
}

// srcReceiptDocOf 把运行期回执投影成 wire 文档（空列表恒 `[]`）。
func srcReceiptDocOf(receipt *source.Receipt) srcReceiptDoc {
	doc := srcReceiptDoc{Tasks: []srcReceiptTask{}, Pruned: []route.Prune{}}
	if receipt == nil {
		return doc
	}
	doc.EventID, doc.TraceID, doc.Duplicate = receipt.EventID, receipt.TraceID, receipt.Duplicate
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
	return doc
}

// srcPreviewDoc 是试运行预览文档：**没有** eventId / taskId（不入事件、任务、去重表）。
type srcPreviewDoc struct {
	Targets []srcPreviewTarget `json:"targets"`
	Pruned  []route.Prune      `json:"pruned"`
}

// srcPreviewTarget 是预览里的一条目标。
//
// state ∈ ready / pruned / missing（空串按 ready，领域层的零值就是 ready）；
// version 用 *int：解析不到具体版本时**省略字段**，而不是回一个会被当成真实版本的 0。
type srcPreviewTarget struct {
	Capability string          `json:"capability"`
	Version    *int            `json:"version,omitempty"`
	Params     json.RawMessage `json:"params"`
	State      string          `json:"state"`
	Reason     string          `json:"reason,omitempty"`
	Detail     string          `json:"detail,omitempty"`
}

// srcPreviewDocOf 把运行期预览投影成 wire 文档（空列表恒 `[]`）。
func srcPreviewDocOf(preview *source.Preview) srcPreviewDoc {
	doc := srcPreviewDoc{Targets: []srcPreviewTarget{}, Pruned: []route.Prune{}}
	if preview == nil {
		return doc
	}
	for _, target := range preview.Targets {
		state := target.State
		if state == "" {
			state = route.StateReady
		}
		params := target.Params
		if len(bytes.TrimSpace(params)) == 0 {
			params = json.RawMessage(`{}`)
		}
		var version *int
		if target.Version > 0 {
			v := target.Version
			version = &v
		}
		doc.Targets = append(doc.Targets, srcPreviewTarget{
			Capability: target.Capability,
			Version:    version,
			Params:     params,
			State:      state,
			Reason:     target.Reason,
			Detail:     target.Detail,
		})
	}
	for _, prune := range preview.Pruned {
		doc.Pruned = append(doc.Pruned, route.Prune{
			Capability: prune.Capability, Reason: prune.Reason, Detail: prune.Detail,
		})
	}
	return doc
}

// ---------- 请求体解码小工具 ----------

// srcPayloadBody 是 fire / 已保存源 dry-run 的请求体（`payload` 可省）。
type srcPayloadBody struct {
	Payload json.RawMessage `json:"payload"`
}

// srcInlinePreviewBody 是未保存源 dry-run 的请求体：`source` 必填。
type srcInlinePreviewBody struct {
	Source  json.RawMessage `json:"source"`
	Payload json.RawMessage `json:"payload"`
}

// srcDecodeAction 解动作端点的请求体。
//
// 空 body 合法（payload 可省 → 用源配置的打底值）。strict=true 时未知字段 →
// 400 unknown_field（我们自己的写请求）；strict=false 只要求是合法 JSON（外部投递）。
func srcDecodeAction(r *http.Request, dst any, limit int64, strict bool) error {
	raw, err := srcReadBody(r, limit)
	if err != nil {
		return err
	}
	return srcDecodeJSON(raw, dst, strict)
}

// srcDecodeJSON 从**已读出的原始字节**解动作体。
//
// 单独成函数是因为 hook 入口要先对同一份字节验签（source.VerifyHookSignature），再解析：
// 验签读一遍 body、解析再读一遍是做不到的（流已耗尽），也容易让两边看到不同的字节。
func srcDecodeJSON(raw []byte, dst any, strict bool) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		return srcDecodeError(err)
	}
	if dec.More() {
		return errStatus(http.StatusBadRequest, reasonBadRequest, "unexpected trailing JSON value")
	}
	return nil
}

// srcReadBody 读请求体并夹上限（超限 413，不把大 body 读进内存再犹豫）。
//
// 用 io.LimitReader 而不是 http.MaxBytesReader：这里要的是一个**确定的** 413，
// 而不是让读错误在解码层变成一句不分尺寸的 400。
func srcReadBody(r *http.Request, limit int64) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	_ = r.Body.Close()
	if err != nil {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	if int64(len(raw)) > limit {
		return nil, errStatus(http.StatusRequestEntityTooLarge, reasonBadRequest, "request body is too large")
	}
	return raw, nil
}

// srcDecodeError 只区分三件客户端能据此行动的事：未知字段 / 类型错 / 不是合法 JSON。
//
// 不透传解析器的原始文本：它随 Go 版本漂移，且可能把请求片段带进响应。
func srcDecodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return unknownFieldError(err)
	}
	if strings.Contains(err.Error(), "unknown field") {
		return errStatus(http.StatusBadRequest, reasonUnknownField, err.Error())
	}
	return errStatus(http.StatusBadRequest, reasonBadRequest, "request body is not valid JSON")
}

// srcPathName 取路径里的 {name}（去空白；空值在 store 侧一律 404）。
func srcPathName(r *http.Request) string {
	return strings.TrimSpace(r.PathValue("name"))
}

// srcSpecKind 取 spec.kind（去空白）。
func srcSpecKind(kind *string) string {
	if kind == nil {
		return ""
	}
	return strings.TrimSpace(*kind)
}

// srcTargets 把写请求的 targets 转成 store 类型（nil → 空切片：空 targets 合法）。
func srcTargets(in *[]sourcespec.Target) []sourcespec.Target {
	if in == nil {
		return []sourcespec.Target{}
	}
	return append([]sourcespec.Target{}, (*in)...)
}

// srcConfigBytes 取写请求的 config 字节；缺省 / 显式 null → nil（= 本次不改 / 用缺省）。
func srcConfigBytes(raw json.RawMessage) []byte {
	if len(bytes.TrimSpace(raw)) == 0 || srcIsNullJSON(raw) {
		return nil
	}
	return append([]byte(nil), raw...)
}

// srcPayloadBytes 取请求体里的 payload 原文；缺省 / 显式 null → nil（= 用源配置的打底值）。
//
// 不在这里解释载荷：`payload` 必须是 JSON 对象这条判据在 route/source 层（
// route.DecodePayload → source.ErrBadPayload），handler 只负责搬运字节。
func srcPayloadBytes(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || srcIsNullJSON(raw) {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// srcIsNullJSON 判断 raw 是否就是 JSON 的 null。
func srcIsNullJSON(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// srcJSONString 把字符串编成 JSON 字面（只用于 config 里的投影字段）。
func srcJSONString(v string) json.RawMessage {
	out, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return out
}
