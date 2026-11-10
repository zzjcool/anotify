package server

// Playbook 资源域（v4 HTTP 资源层，contract.md D2 / WIRE-HANDOFF.md W3）。
//
// 三条语义边界决定了这里的形状，写代码时不要越过去：
//
//  1. **包是清单 + 回执，不是所有权**（playbook.md §7）：DELETE 只删清单行，已装出去的
//     能力 / 源一行不动，响应必须告诉用户留下了什么（kept / values）。
//  2. **默认是摘要**（W3）：包体是 `{相对路径: 正文}` 的整份 map，列表 / 详情默认不回它
//     （spec 是空投影 `{}`）；只有 `?includeBody=true` 才回原始未渲染的 `spec.files`，
//     供编辑 / 导出。默认摘要**不能**当完整 PUT 模板。
//  3. **安装不部署，冲突 0 写**：install 只在单事务里建能力 / 源 + 写回执；有冲突时整包
//     一行不写并回 400 `playbook_conflict`，details 里带 valuesDigest / conflicts[] / hint
//     （12 格矩阵明细不能被裁成 causes）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/store"
)

// kindPlaybook 是资源 kind（与包内 `kind: Playbook` 同字面，便于 e2e 断言）。
const kindPlaybook = "Playbook"

// playbookAPI 是 Playbook 资源域的 handler 集合。
//
// 只持有 serverDeps：作用域 / 授权走 namespace_scope.go，错误映射走 api_status.go，
// CAS 令牌走 api_cas.go —— 三件共享判据都不在这里再抄一份。
type playbookAPI struct{ d *serverDeps }

// newPlaybookAPI 构造 Playbook 资源域。
func newPlaybookAPI(d *serverDeps) *playbookAPI { return &playbookAPI{d: d} }

// register 注册全部 Playbook 路由（全部 session 包裹，未登录直 401）。
//
// 方法不匹配（例如 `PATCH …/playbooks/{slug}`）由 Go 1.22 ServeMux 自己回 405，
// 这里不重复实现方法分派。
func (a *playbookAPI) register(mux *http.ServeMux) {
	const base = "/v1/namespaces/{ns}/playbooks"
	routes := []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"GET " + base, a.list},
		{"POST " + base, a.create},
		{"GET " + base + "/{slug}", a.get},
		{"PUT " + base + "/{slug}", a.replace},
		{"DELETE " + base + "/{slug}", a.delete},
		{"POST " + base + "/{slug}/render", a.render},
		{"POST " + base + "/{slug}/install", a.install},
		{"GET " + base + "/{slug}/installs", a.listInstalls},
		{"GET " + base + "/{slug}/installs/{installId}", a.getInstall},
	}
	for _, rt := range routes {
		mux.Handle(rt.pattern, noStore(a.d.session(rt.h)))
	}
}

// ---------- 读端点 ----------

// list 处理 GET …/playbooks：命名空间内的包清单（摘要投影）。
func (a *playbookAPI) list(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	rows, err := a.d.DB.ListPlaybooks(r.Context(), ns)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	includeBody := includePlaybookBody(r)
	items := make([]apiEnvelope, 0, len(rows))
	for _, row := range rows {
		env, err := a.envelope(r.Context(), ns, row, includeBody)
		if err != nil {
			writePlaybookAPIError(w, err)
			return
		}
		items = append(items, env)
	}
	// 空列表是 []（writeList 收口 nil → []）；列表 kind 沿 api-conventions §3.3 的
	// `<Kind>List` 约定（同 `CapabilityList` / `EventList`）。
	writeList(w, http.StatusOK, kindPlaybook+"List", listMeta{}, items)
}

// get 处理 GET …/playbooks/{slug}：默认摘要，`?includeBody=true` 回整份包体。
func (a *playbookAPI) get(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	row, err := a.row(r, ns)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	env, err := a.envelope(r.Context(), ns, row, includePlaybookBody(r))
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
}

// listInstalls 处理 GET …/playbooks/{slug}/installs：按 valuesDigest 折叠的实例清单（§4.10）。
//
// 响应是**读模型**（total / installs），不是资源列表：一个折叠组不是可寻址对象，
// 别把它包装成 `PlaybookInstallList` kind 的 items。
func (a *playbookAPI) listInstalls(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	row, err := a.row(r, ns)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	list, err := a.d.DB.ListPlaybookInstalls(r.Context(), ns, row.ID)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	if list == nil {
		list = &store.PlaybookInstallList{Installs: []store.PlaybookInstallGroup{}}
	}
	if list.Installs == nil {
		list.Installs = []store.PlaybookInstallGroup{}
	}
	writeJSON(w, http.StatusOK, list)
}

// getInstall 处理 GET …/playbooks/{slug}/installs/{installId}：单次回执（§4.5）。
//
// 不要求当前清单存在：回执里的 slug / 包 id 是快照，删包之后仍可按旧 slug 直达（D21）。
func (a *playbookAPI) getInstall(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	installID := strings.TrimSpace(r.PathValue("installId"))
	if slug == "" || installID == "" {
		writePlaybookAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "playbook slug and installId are required"))
		return
	}
	rcpt, err := a.d.DB.GetPlaybookInstall(r.Context(), ns, slug, installID)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	// InstallReceipt 是动作文档（W3 明确例外），不套四键信封。
	writeJSON(w, http.StatusOK, rcpt)
}

// ---------- 写端点 ----------

// create 处理 POST …/playbooks：只落一行清单（不安装）。
//
// 同 slug 已存在 → store.ErrSlugTaken → 409 slug_taken（api_status.go 已注册）。
func (a *playbookAPI) create(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	var spec playbookWriteSpec
	meta, err := decodeWriteEnvelope(r, kindPlaybook, &spec, "version")
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	if err := checkMetaNamespace(meta, r.PathValue("ns")); err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	slug, err := requireMetaName(meta)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	files, source, err := spec.packageBody()
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	row, err := a.d.DB.CreatePlaybook(r.Context(), ns, slug, files, source)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	env, err := a.envelope(r.Context(), ns, row, includePlaybookBody(r))
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusCreated, env)
}

// replace 处理 PUT …/playbooks/{slug}：整体替换包体（CAS 令牌只在 body metadata.resourceVersion）。
//
// **只换模板**：已装的能力 / 源一行不动，要等下一次 install 才变（§5 / §10）。
func (a *playbookAPI) replace(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		writePlaybookAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "playbook slug is required"))
		return
	}
	var spec playbookWriteSpec
	// resourceVersion 只在 PUT/PATCH 的 body 里；POST 创建不带 CAS，所以 create 不解这个键。
	meta, err := decodeWriteEnvelope(r, kindPlaybook, &spec, "version", "resourceVersion")
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	// namespace 由路径给出（写请求的 metadata 白名单不含它）；这里仍走共享校验，
	// 一旦骨架允许 metadata.namespace，冲突会就地报 namespace_mismatch 而不是被忽略。
	if err := checkMetaNamespace(meta, r.PathValue("ns")); err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	if meta.Name != "" && meta.Name != slug {
		// 路径就是寻址（slug 不可变）：body 说另一个名字时若照写，GET 会读到自称别的包。
		writePlaybookAPIError(w, errStatus(http.StatusBadRequest, reasonNameImmutable,
			"metadata.name (slug) is immutable: "+slug))
		return
	}
	files, source, err := spec.packageBody()
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	rv, err := casTokenFromMeta(meta)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	row, err := a.d.DB.ReplacePlaybook(r.Context(), ns, slug, rv, files, source)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	env, err := a.envelope(r.Context(), ns, row, includePlaybookBody(r))
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	writeEnvelope(w, http.StatusOK, env)
}

// delete 处理 DELETE …/playbooks/{slug}?resourceVersion=：只删清单。
//
// 响应是动作文档（deleted / kept / values / note），不套四键信封：它回答的是
// 「留下了什么」，不是一个资源的状态。
func (a *playbookAPI) delete(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		writePlaybookAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "playbook slug is required"))
		return
	}
	rv, err := casTokenFromQuery(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	res, err := a.d.DB.DeletePlaybook(r.Context(), ns, slug, rv)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	if res == nil {
		writePlaybookAPIError(w, errStatus(http.StatusInternalServerError, reasonInternal, "delete playbook returned no result"))
		return
	}
	if res.Values == nil {
		res.Values = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- 动作端点（不套资源信封） ----------

// render 处理 POST …/playbooks/{slug}/render：只读渲染（不读安装冲突库、不写库、不产生回执）。
func (a *playbookAPI) render(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		writePlaybookAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "playbook slug is required"))
		return
	}
	body, err := decodePlaybookAction(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	res, err := a.d.DB.RenderPlaybook(r.Context(), ns, slug, body.Values)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// install 处理 POST …/playbooks/{slug}/install：幂等安装（单事务；不部署）。
//
// dryRun=true 走同一条代码路径但 0 持久写（回执仍返回，但不落 playbook_installs）。
func (a *playbookAPI) install(w http.ResponseWriter, r *http.Request) {
	ns, err := a.d.authorizePathNamespace(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		writePlaybookAPIError(w, errStatus(http.StatusBadRequest, reasonBadRequest, "playbook slug is required"))
		return
	}
	body, err := decodePlaybookAction(r)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	rcpt, err := a.d.DB.InstallPlaybook(r.Context(), ns, slug, body.Values, body.DryRun)
	if err != nil {
		writePlaybookAPIError(w, err)
		return
	}
	// InstallReceipt 是动作文档（W3 明确例外）。
	writeJSON(w, http.StatusOK, rcpt)
}

// ---------- 写请求体 ----------

// playbookWriteSpec 是 Playbook 写请求的 spec（W3：只搬包体，不复制根清单）。
//
// `playbook.yaml` 仍然是唯一根清单：values / 能力 / 源相对引用都从它解析，请求体
// **不要求**也不接受第二份并行的 spec.values / spec.capabilities / spec.sources
// （decodeWriteEnvelope 的严格解码会把它们报成 unknown_field）。
type playbookWriteSpec struct {
	// Files 是整份包体（相对路径 → 正文），含 playbook.yaml。
	Files map[string]string `json:"files"`
	// Source 是 OCI 入口（`oci://…@sha256:…`），与 Files 二选一。
	Source string `json:"source"`
}

// packageBody 校验 files / source 二选一，并返回要交给 store 的实参。
//
// 三个拒绝分支都是 400，但 reason 不同（封闭词表 playbook.md §9.2）：
//   - 两个都给 / 都不给 → invalid_playbook；
//   - 只给 source：W3 要求服务端拉取一次并以解析后的完整 files 存储，而 v4 一期没有
//     网络拉取实现（playbook.md §11 只留了码位）→ oci_unsupported。这里明确拒绝，
//     而不是让 store 的 Parse 报一个会误导人的 entry_missing（"包缺入口文件"）。
func (s playbookWriteSpec) packageBody() (map[string]string, string, error) {
	files := s.Files
	if len(files) == 0 {
		files = nil
	}
	source := strings.TrimSpace(s.Source)
	switch {
	case files != nil && source != "":
		return nil, "", errStatus(http.StatusBadRequest, playbook.CodeInvalidPlaybook,
			"spec.files 与 spec.source 只能给一个（当前两个都给了）")
	case files == nil && source == "":
		return nil, "", errStatus(http.StatusBadRequest, playbook.CodeInvalidPlaybook,
			"spec.files 或 spec.source 必须给一个（包体是必填）")
	case files == nil:
		return nil, "", errStatus(http.StatusBadRequest, playbook.CodeOCIUnsupported,
			"spec.source（OCI 拉取）在 v4 一期未实现：请直接上传 spec.files")
	}
	return files, source, nil
}

// playbookActionBody 是 render / install 的动作请求体。
//
// 它不是资源信封（动作 POST 不套 CAS、不套 status 只读检查），所以用一个宽松的
// 小结构：`{values, dryRun}`。render 忽略 dryRun（render 永远只读）。
type playbookActionBody struct {
	Values map[string]any `json:"values"`
	DryRun bool           `json:"dryRun"`
}

// decodePlaybookAction 解码动作 POST 的请求体。
//
// 空 body 合法（render / install 都可以不带 values：包的 values 可以全有默认值）；
// 非法 JSON / 未知字段仍按写请求的严格口径回 400（静默忽略会让人以为字段生效了）。
func decodePlaybookAction(r *http.Request) (playbookActionBody, error) {
	var body playbookActionBody
	if r == nil || r.Body == nil {
		return body, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return body, errStatus(http.StatusBadRequest, reasonBadRequest, "cannot read request body")
	}
	_ = r.Body.Close()
	if len(bytes.TrimSpace(raw)) == 0 {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return body, unknownFieldError(err)
	}
	return body, nil
}

// ---------- 响应形状 ----------

// playbookStatus 是 Playbook 的 status 读模型（playbook.md §6.1）。
//
// 每一项都必须是**真实派生**，派不出来就给空（宁少不编）：
//   - items / sources / lastInstallId / unapplied 来自回执历史：`ListPlaybookInstalls` 的
//     installed_from 反查给出「这个包历史上装出来的行」，最近一组的 lastInstallId 给出
//     该组最新回执（`unapplied[]` 就从这份回执读，它才是「差异没生效」的原始记录）。
//   - conditions 是**成员能力的四条 condition** 聚合。它需要能力读模型按 installed_from
//     查询，store 没有导出这个入口（contract D0.9：331 不新增 store 底层入口，缺签名只
//     报告），所以这里恒为空数组——不编造 `True`。
//   - ready 由 conditions + unapplied 派生（四条全 True 且 unapplied 为空），因此当前
//     恒为 false：四条 condition 一条都拿不到时不能宣称就绪。
//
// 刻意**没有** `blocker`：它要求「最该先做的一件事」，来源正是成员能力的四条
// condition；条件拿不到时给一个 blocker 就是纯编造，故省略该键（派生出事实时再加）。
type playbookStatus struct {
	Ready       bool                      `json:"ready"`
	Unapplied   []playbook.UnappliedEntry `json:"unapplied"`
	Total       int                       `json:"total"`
	Conditions  []playbookCondition       `json:"conditions"`
	Items       []playbookItem            `json:"items"`
	Sources     playbookSourceStatus      `json:"sources"`
	LastInstall string                    `json:"lastInstallId"`
	// Source 是 OCI 来源的只读投影：手传包体时是 null（不是空串，§7.3）。
	Source *string `json:"source"`
}

// playbookCondition 是聚合后的一条 condition（形状对齐 capability.md §2.4）。
type playbookCondition struct {
	Type    string   `json:"type"`
	Status  string   `json:"status"`
	Reason  string   `json:"reason,omitempty"`
	Message string   `json:"message,omitempty"`
	Members []string `json:"members,omitempty"`
}

// playbookItem 是体检的一行（只有 capability / source 两类，§6.1）。
//
// `state` 只有 `ok`：`no_node`（节点池）/ `blocked`（targets 解析）/ `disabled`
// （enabled:false）/ `unapplied` 都需要能力或源的读模型，本域拿不到（D0.9），
// 因此只声明「定义在库里」这一个已知事实。
type playbookItem struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
	State   string `json:"state"`
}

// playbookItemOK 是 items[].state 的唯一已知取值（见 playbookItem 注释）。
const playbookItemOK = "ok"

// playbookSourceStatus 是 status.sources（§6.1 的四态计数）。
//
// 只报两组能派生的数：total（历史上装出来的源个数）与 unapplied（最近回执里
// targets 未生效的源数）。enabled / blocked / disabled 需要读源行本身，本域不查库，
// 所以不出现这些键——比回 0 更诚实（0 会被前端当成"全都停用了"）。
type playbookSourceStatus struct {
	Total     int `json:"total"`
	Unapplied int `json:"unapplied"`
}

// playbookSpec 是 `?includeBody=true` 时的 spec 投影（原始未渲染包体）。
type playbookSpec struct {
	Files map[string]string `json:"files"`
}

// envelope 组装一个 Playbook 信封（默认摘要 / includeBody 全文）。
func (a *playbookAPI) envelope(ctx context.Context, ns string, row *store.PlaybookRow, includeBody bool) (apiEnvelope, error) {
	if row == nil {
		return apiEnvelope{}, errStatus(http.StatusInternalServerError, reasonInternal, "playbook row is nil")
	}
	st, err := a.statusFor(ctx, ns, row)
	if err != nil {
		return apiEnvelope{}, err
	}
	meta := objectMeta{
		Name:      row.Slug,
		Namespace: a.d.namespaceDisplay(ctx, ns),
		UID:       row.ID,
		Version:   metaVersion(row.Version),
		// resourceVersion = rv1:<uid>:<rev>（store 已有实现；HTTP 只透传与比较）。
		ResourceVersion:   store.ResourceVersion(row.ID, row.ResourceVersion),
		CreationTimestamp: time.Unix(row.CreatedAt, 0).UTC().Format(time.RFC3339),
		DisplayName:       row.DisplayName,
	}
	var spec any = struct{}{} // 默认空投影：不泄露包体全文（W3）
	if includeBody {
		files := row.Files
		if files == nil {
			files = map[string]string{}
		}
		spec = playbookSpec{Files: files}
	}
	return newEnvelope(kindPlaybook, meta, spec, st), nil
}

// statusFor 从「真实行 + 回执历史」派生 status。
func (a *playbookAPI) statusFor(ctx context.Context, ns string, row *store.PlaybookRow) (playbookStatus, error) {
	st := playbookStatus{
		Unapplied:  []playbook.UnappliedEntry{},
		Conditions: []playbookCondition{},
		Items:      []playbookItem{},
	}
	if row.Source != "" {
		source := row.Source
		st.Source = &source
	}
	list, err := a.d.DB.ListPlaybookInstalls(ctx, ns, row.ID)
	if err != nil {
		return st, err
	}
	if list != nil && len(list.Installs) > 0 {
		// 组按 lastAt 降序（store 保证）：第一组的最新回执 = 全包最近一次安装。
		st.LastInstall = list.Installs[0].LastInstallID
		st.Items = playbookItemsFromInstalls(list.Installs)
	}
	if st.LastInstall != "" {
		rcpt, err := a.d.DB.GetPlaybookInstall(ctx, ns, row.Slug, st.LastInstall)
		switch {
		case err == nil && rcpt != nil:
			if len(rcpt.Unapplied) > 0 {
				st.Unapplied = append(st.Unapplied, rcpt.Unapplied...)
			}
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return st, err
		}
	}
	st.Total = len(st.Items)
	for _, it := range st.Items {
		if it.Kind == "source" {
			st.Sources.Total++
		}
	}
	for _, u := range st.Unapplied {
		if u.Kind == "source" {
			st.Sources.Unapplied++
		}
	}
	st.Ready = playbookReady(st.Conditions, st.Unapplied)
	return st, nil
}

// playbookItemsFromInstalls 把折叠组反查出来的能力 / 源名字并成 items（去重 + 稳定排序）。
//
// 同一行可能出现在多个组里（第二组 values 装的是同一批名字时），所以先按身份去重：
// 能力的身份是 name@version，源的身份只有名字。
func playbookItemsFromInstalls(groups []store.PlaybookInstallGroup) []playbookItem {
	const (
		kindCapability = "capability"
		kindSource     = "source"
	)
	seen := map[string]playbookItem{}
	for _, g := range groups {
		for _, ref := range g.Capabilities {
			name, version, _ := store.SplitRef(ref)
			if name == "" {
				continue
			}
			seen[kindCapability+"\x00"+store.FormatRef(name, version)] = playbookItem{
				Kind: kindCapability, Name: name, Version: version, State: playbookItemOK,
			}
		}
		for _, name := range g.Sources {
			if name == "" {
				continue
			}
			seen[kindSource+"\x00"+name] = playbookItem{Kind: kindSource, Name: name, State: playbookItemOK}
		}
	}
	items := make([]playbookItem, 0, len(seen))
	for _, it := range seen {
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind // capability 在 source 前
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Version < items[j].Version
	})
	return items
}

// playbookReady 是对「四条 condition 全 True 且 unapplied 为空」的唯一实现（§6.2）。
//
// 拿不到四条 condition（成员能力读模型缺失）时 ready 必须是 false：把「不知道」
// 报成「就绪」会让前端把没部署的包显示成全绿。
func playbookReady(conditions []playbookCondition, unapplied []playbook.UnappliedEntry) bool {
	if len(unapplied) > 0 || len(conditions) != 4 {
		return false
	}
	for _, c := range conditions {
		if c.Status != "True" {
			return false
		}
	}
	return true
}

// includePlaybookBody 报告读端点是否要包体全文（W3：默认摘要，`?includeBody=true` 才回）。
func includePlaybookBody(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("includeBody"))) {
	case "true", "1":
		return true
	}
	return false
}

// row 取 URL 里的 {slug} 对应清单行。
func (a *playbookAPI) row(r *http.Request, ns string) (*store.PlaybookRow, error) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest, "playbook slug is required")
	}
	return a.d.DB.GetPlaybook(r.Context(), ns, slug)
}

// ---------- 错误映射 ----------

// playbookConflictDetails 是 400 冲突响应的 details（W3 冻结形状）。
//
// 为什么不用 statusDetails：它只有 causes / current / retryAfterSeconds 三个槽，装不下
// 12 格矩阵的 conflicts[]。把 conflicts 压成 causes 人话会丢掉 declaredDigest /
// actualDigest / fields —— 而客户端判「能不能改包解决」正是靠这些结构化字段。
type playbookConflictDetails struct {
	ValuesDigest string              `json:"valuesDigest"`
	Conflicts    []playbook.Conflict `json:"conflicts"`
	Hint         string              `json:"hint,omitempty"`
}

// playbookStatusDoc 是带自定义 details 的 Status 响应体（结构与 statusDoc 相同）。
type playbookStatusDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Code       int    `json:"code"`
	Reason     string `json:"reason"`
	Message    string `json:"message,omitempty"`
	Details    any    `json:"details,omitempty"`
}

// writePlaybookAPIError 是 Playbook 端点错误映射的唯一实现。
//
// 顺序：领域冲突（要带结构化 details）→ 其它 playbook.Error（封闭码表 → 400/404）
// → store 哨兵（slug_mismatch）→ 通用 writeAPIError（含 CAS 家族与 404）。
func writePlaybookAPIError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	var pbErr *playbook.Error
	if errors.As(err, &pbErr) {
		if pbErr.Code == playbook.CodePlaybookConflict || pbErr.Code == playbook.CodeVersionContentImmutable {
			writeJSON(w, http.StatusBadRequest, playbookStatusDoc{
				APIVersion: apiVersionV1,
				Kind:       kindStatus,
				Status:     "Failure",
				Code:       http.StatusBadRequest,
				Reason:     pbErr.Code,
				Message:    pbErr.Message,
				Details:    playbookConflictDetailsOf(pbErr),
			})
			return
		}
		code := http.StatusBadRequest
		if pbErr.Code == playbook.CodeNotFound {
			code = http.StatusNotFound
		}
		writeStatus(w, code, pbErr.Code, pbErr.Message, nil)
		return
	}
	if errors.Is(err, store.ErrPlaybookSlugMismatch) {
		// 路径 slug 与包内 metadata.name 不一致：这是调用方把包和 URL 拼错了，
		// 不是服务端故障（api_status.go 未注册该哨兵，默认会落 500）。
		writeStatus(w, http.StatusBadRequest, playbook.CodeInvalidPlaybook, err.Error(), nil)
		return
	}
	writeAPIError(w, err)
}

// playbookConflictDetailsOf 从 *playbook.Error.Details 里取出 W3 要求的三个字段。
func playbookConflictDetailsOf(e *playbook.Error) playbookConflictDetails {
	d := playbookConflictDetails{Conflicts: []playbook.Conflict{}}
	if e == nil {
		return d
	}
	if v, ok := e.Details["valuesDigest"].(string); ok {
		d.ValuesDigest = v
	}
	if v, ok := e.Details["hint"].(string); ok {
		d.Hint = v
	}
	switch raw := e.Details["conflicts"].(type) {
	case []playbook.Conflict:
		d.Conflicts = raw
	case nil:
		// 没有明细就保持空数组（空列表纪律）。
	default:
		// 兜底：万一将来 Details 换成了 map/list 的泛型形态，仍然按 JSON 形状搬过来，
		// 而不是把结构化明细静默丢掉。
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &d.Conflicts)
		}
	}
	if d.Conflicts == nil {
		d.Conflicts = []playbook.Conflict{}
	}
	return d
}
